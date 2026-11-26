package server

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/auth"
)

// TestAudit_H1_SiteInfoDataRace は s.cfg のアクセサメソッドの並行呼び出しで
// データレースが発生しないことを検証する。
func TestAudit_H1_SiteInfoDataRace(t *testing.T) {
	env := newTestEnv(t)
	var wg sync.WaitGroup
	const workers = 8
	const iterations = 200

	for i := range workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := range iterations {
				if (workerID+j)%2 == 0 {
					env.server.setSiteInfo("SiteName", "SiteDescription")
					env.server.setBaseURL("https://example.com")
				} else {
					_ = env.server.getSiteName()
					_ = env.server.getSiteDescription()
					_, _ = env.server.getSiteInfo()
					_ = env.server.getBaseURL()
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestAudit_H2_SyncTokenIdentityAndInviteValidation は
// 招待作成時のプレフィックス・既存ユーザー重複拒絶と、
// jti ベースの本人識別（なりすまし防止）を検証する。
func TestAudit_H2_SyncTokenIdentityAndInviteValidation(t *testing.T) {
	env := newInviteSyncEnv(t)
	adminTok := env.token("admin-user", auth.RoleAdmin)

	// 1. sync- で始まる名前の招待作成拒否
	t.Run("rejects sync- prefix subject", func(t *testing.T) {
		req := issueInviteRequest(auth.RoleWriter, "sync-hacker", 7)
		rec := env.call(adminTok, req)
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
	})

	// 2. 既存ローカルユーザー名と同じ名前の招待作成拒否
	t.Run("rejects existing local username", func(t *testing.T) {
		req := issueInviteRequest(auth.RoleWriter, "editor-1", 7)
		rec := env.call(adminTok, req)
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
	})

	// 3. 同じ sub であっても異なる jti を持つトークンは独立したユーザーIDとなる
	t.Run("tokens with distinct jti map to distinct user ids", func(t *testing.T) {
		tokA, claimsA := env.recordedToken("reporter", auth.RoleWriter)
		tokB, claimsB := env.recordedToken("reporter", auth.RoleWriter)
		if claimsA.ID == claimsB.ID {
			t.Fatal("jti must be unique")
		}

		ctx := context.Background()
		userA, err := env.server.syncUser(ctx, claimsA)
		if err != nil {
			t.Fatalf("syncUser A: %v", err)
		}
		userB, err := env.server.syncUser(ctx, claimsB)
		if err != nil {
			t.Fatalf("syncUser B: %v", err)
		}

		if userA == userB {
			t.Errorf("distinct tokens must not share user ID: got %q", userA)
		}
		if !strings.HasPrefix(userA, syncUserIDPrefix+"tok-"+claimsA.ID) {
			t.Errorf("userA ID %q must have prefix %q", userA, syncUserIDPrefix+"tok-"+claimsA.ID)
		}
		if !strings.HasPrefix(userB, syncUserIDPrefix+"tok-"+claimsB.ID) {
			t.Errorf("userB ID %q must have prefix %q", userB, syncUserIDPrefix+"tok-"+claimsB.ID)
		}

		// 執筆チーム内では非公開の記事を共有するため、A が提出した記事を B も更新できる。ただし執筆者は A のまま
		// （B の更新で A の同期ユーザーになりすましたり、執筆者が B に置き換わったりしない）。
		recA := env.call(tokA, submitRequest(SyncArticle{Title: "Reporter A Article", ContentMarkdown: "Body A"}))
		expectSyncStatus(t, recA, http.StatusOK)
		resA := decodeSync[SyncSubmitResponse](t, recA)

		recB := env.call(tokB, submitRequest(SyncArticle{ID: resA.Article.ID, Title: "Reporter B Update", ContentMarkdown: "Body B"}))
		expectSyncStatus(t, recB, http.StatusOK)
		if a := env.mustGetArticle(resA.Article.ID); a.AuthorID != userA || a.Title != "Reporter B Update" {
			t.Errorf("article after B's update: author %q, title %q; want author %q (A) and B's title", a.AuthorID, a.Title, userA)
		}
	})
}

// TestAudit_M1_RemoteTokenLeakPrevention は接続先 URL 変更時のトークン送信・保存遮断を検証する。
func TestAudit_M1_RemoteTokenLeakPrevention(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	c.login("admin", testPassword)

	// 初期設定: 保存済み URL とトークン
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	tok, err := auth.IssueToken(priv, "test-admin", "admin", time.Hour)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := env.server.saveRemoteConfig(remoteConfig{
		URL:   "https://legit-server.example.jp",
		Token: tok,
	}); err != nil {
		t.Fatalf("saveRemoteConfig: %v", err)
	}

	// 1. 設定保存時: ホストを変更してトークンを空にした場合は拒絶される
	t.Run("save with changed host and empty token is rejected", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote", url.Values{
			"remote_url":   {"https://evil-server.example.com"},
			"remote_token": {""},
		})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "接続先を変更する場合は、新しいトークンを入力してください")

		// 設定が変更されていないことを確認
		cfg := env.server.loadRemoteConfig()
		if cfg.URL != "https://legit-server.example.jp" || cfg.Token != tok {
			t.Errorf("rejected save changed remoteConfig: %+v", cfg)
		}
	})

	// 2. 接続テスト時: ホストを変更してトークンを空にした場合は送信拒絶される
	t.Run("test with changed host and empty token is blocked", func(t *testing.T) {
		res := c.postForm("/admin/remote/test", url.Values{
			"remote_url":   {"https://evil-server.example.com"},
			"remote_token": {""},
		})
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "接続先を変更する場合は、新しいトークンを入力してください")
	})

	// 3. 空 URL で保存した場合、トークンも確実に破棄される
	t.Run("save with empty URL clears token", func(t *testing.T) {
		// まず有効な設定を保存
		if err := env.server.saveRemoteConfig(remoteConfig{
			URL:   "https://legit-server.example.jp",
			Token: tok,
		}); err != nil {
			t.Fatalf("saveRemoteConfig: %v", err)
		}

		res := c.postForm("/admin/settings/remote", url.Values{
			"remote_url":   {""},
			"remote_token": {""},
		})
		expectRedirect(t, res, "/admin/settings/remote?flash=settings_saved")
		cfg := env.server.loadRemoteConfig()
		if cfg.URL != "" || cfg.Token != "" {
			t.Fatalf("remoteConfig after clearing URL = %+v, want empty URL and token", cfg)
		}
	})

	// 4. 残存トークンがある状態で新しい URL をトークンなしで保存・テストしようとした場合は拒絶される
	t.Run("save new URL after empty without token is rejected", func(t *testing.T) {
		if err := env.server.saveRemoteConfig(remoteConfig{
			URL:   "",
			Token: tok,
		}); err != nil {
			t.Fatal(err)
		}

		res := c.postForm("/admin/settings/remote", url.Values{
			"remote_url":   {"https://another-server.example.com"},
			"remote_token": {""},
		})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "接続先を変更する場合は、新しいトークンを入力してください")

		// 接続テスト時も同様に拒絶される
		testRes := c.postForm("/admin/remote/test", url.Values{
			"remote_url":   {"https://another-server.example.com"},
			"remote_token": {""},
		})
		expectStatus(t, testRes, http.StatusOK)
		expectContains(t, testRes.Body, "接続先を変更する場合は、新しいトークンを入力してください")
	})
}

// TestAudit_M2_SyncMultilingualFieldValidation は多言語関連フィールドの形式検証を検証する。
func TestAudit_M2_SyncMultilingualFieldValidation(t *testing.T) {
	env := newSyncEnv(t)
	writerTok := env.token("writer-user", auth.RoleWriter)

	// 1. 不正な translation_group_id
	t.Run("rejects invalid translation_group_id", func(t *testing.T) {
		req := submitRequest(SyncArticle{
			Title:              "Multilingual Draft",
			ContentMarkdown:    "Body",
			TranslationGroupID: "../../etc/passwd",
		})
		rec := env.call(writerTok, req)
		expectSyncStatus(t, rec, http.StatusBadRequest)
	})

	// 2. 不正な source_article_id
	t.Run("rejects invalid source_article_id", func(t *testing.T) {
		req := submitRequest(SyncArticle{
			Title:           "Multilingual Draft",
			ContentMarkdown: "Body",
			SourceArticleID: "invalid source id with spaces",
		})
		rec := env.call(writerTok, req)
		expectSyncStatus(t, rec, http.StatusBadRequest)
	})
}

// TestAudit_M3_SyncSettingsAtomicTransaction は sync_settings のトランザクションアトミック性を検証する。
func TestAudit_M3_SyncSettingsAtomicTransaction(t *testing.T) {
	env := newSyncEnv(t)
	adminTok := env.token("admin-user", auth.RoleAdmin)

	// 壊れたサイト構成（カードが6枚超過でエラーになるもの）
	// と同時に site_name の変更を要求
	initialName := env.server.getSiteName()
	badCards := []string{"card1", "card2", "card3", "card4", "card5", "card6", "card7"} // 6枚超過でエラー

	strPtr := func(s string) *string { return &s }
	req := SyncRequest{
		Action: SyncActionSyncSettings,
		Settings: &SyncSettings{
			SiteName:      strPtr("New Site Name Attempt"),
			FrontSections: &[]FrontSection{{Type: FrontSectionCards, Pages: badCards}},
		},
	}
	rec := env.call(adminTok, req)
	// サイト構成のバリデーションエラーで 422 になるはず
	expectSyncStatus(t, rec, http.StatusUnprocessableEntity)

	// トランザクションがアトミックにロールバックされ、SiteName も変わっていないことを検証
	currentName := env.server.getSiteName()
	if currentName != initialName {
		t.Errorf("atomic rollback failed: SiteName was changed to %q, want %q", currentName, initialName)
	}
}

// TestAudit_M4_AIBaseURLPlainHTTPRestriction はカスタム AI の baseURL における
// 平文 HTTP の制限を検証する。
func TestAudit_M4_AIBaseURLPlainHTTPRestriction(t *testing.T) {
	// 許可されるべき URL (localhost, 127.0.0.1, private IP)
	allowedURLs := []string{
		"http://localhost:11434/v1",
		"http://127.0.0.1:11434/v1",
		"http://192.168.1.50:8000/v1",
		"http://10.0.0.5:8080",
		"http://172.16.0.2:11434",
		"https://api.openai.com/v1",
		"https://my-llm.example.org",
	}
	for _, u := range allowedURLs {
		normalized, err := normalizeAIBaseURL(u)
		if err != nil {
			t.Errorf("normalizeAIBaseURL(%q) failed: %v", u, err)
		}
		if normalized == "" {
			t.Errorf("normalizeAIBaseURL(%q) returned empty", u)
		}
	}

	// 拒絶されるべき URL (外部ホストへの平文 http)
	blockedURLs := []string{
		"http://api.openai.com/v1",
		"http://example.com/v1",
		"http://8.8.8.8:8080/v1",
	}
	for _, u := range blockedURLs {
		_, err := normalizeAIBaseURL(u)
		if err == nil {
			t.Errorf("normalizeAIBaseURL(%q) must be rejected for external plain HTTP", u)
		} else if !strings.Contains(err.Error(), "https://") {
			t.Errorf("unexpected error message: %v", err)
		}
	}
}

// TestAudit_L1_TranslationProviderValidation は翻訳 API および設定での
// プロバイダー許可リスト検証を検証する。
func TestAudit_L1_TranslationProviderValidation(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	adm := env.loggedIn(admin)

	// 1. 翻訳 API で不正な provider を指定すると 400 Bad Request
	res := ed.postForm("/admin/articles/translate", url.Values{
		"text":        {"Hello world"},
		"target_lang": {"ja"},
		"provider":    {"malicious_provider"},
	})
	expectStatus(t, res, http.StatusBadRequest)
	expectContains(t, res.Body, "無効な翻訳プロバイダーです")

	// 2. AI 設定保存で不正な translation_provider を指定するとエラー
	saveRes := adm.postForm("/admin/settings/ai", url.Values{
		"provider":             {"gemini"},
		"api_key_gemini":       {"test-key-1234"},
		"translation_provider": {"invalid_provider"},
	})
	expectStatus(t, saveRes, http.StatusUnprocessableEntity)
	expectContains(t, saveRes.Body, "有効な翻訳プロバイダー")
}

// TestAudit_L2_AIKeyMasking は AI API キーの末尾4文字のみを残す安全なマスキングを検証する。
func TestAudit_L2_AIKeyMasking(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", "********"},
		{"1234", "********"},
		{"12345", "********2345"},
		{"AQ.Ab8RN6-test", "********test"},
		{"sk-proj-abc123xyz9876", "********9876"},
	}
	for _, tc := range cases {
		got := maskAIKey(tc.input)
		if got != tc.want {
			t.Errorf("maskAIKey(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestAudit_L3_SyncThemeMuLock は同期 API でテーマを更新する際に
// themeMu による排他制御が行われていることを検証する。
func TestAudit_L3_SyncThemeMuLock(t *testing.T) {
	env := newInviteSyncEnv(t)
	adminTok := env.token("admin-user", auth.RoleAdmin)

	// themeMu を事前に保持している間、同期 API のテーマ更新はブロックされ、
	// themeMu を解放すると完了する。
	env.server.themeMu.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := SyncRequest{
			Action: SyncActionSyncSettings,
			Settings: &SyncSettings{
				Theme: strPtr("tech"),
			},
		}
		rec := env.call(adminTok, req)
		expectSyncStatus(t, rec, http.StatusOK)
	}()

	select {
	case <-done:
		t.Fatal("syncSettings with theme proceeded while themeMu was locked")
	case <-time.After(100 * time.Millisecond):
		// 期待通りブロックされている
	}

	env.server.themeMu.Unlock()

	select {
	case <-done:
		// 正常終了
	case <-time.After(2 * time.Second):
		t.Fatal("syncSettings timed out after unlocking themeMu")
	}

	if got := env.setting("theme"); got != "tech" {
		t.Errorf("saved theme = %q, want 'tech'", got)
	}
}
