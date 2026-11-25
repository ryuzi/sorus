package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"sorus/internal/auth"
)

// newInviteRemoteEnv はローカル CMS と、招待を発行できる公開サーバーを起動する。
func newInviteRemoteEnv(t *testing.T) *remoteEnv {
	t.Helper()
	return &remoteEnv{local: newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil), remote: newInviteSyncEnv(t)}
}

// inviteFor は公開サーバーで sub・role のトークンを発行・記録し、公開サーバーの同期 API の URL と束ねた招待コードを返す。
func (e *remoteEnv) inviteFor(sub string, role auth.Role) (string, *auth.Claims) {
	e.local.t.Helper()
	tok, claims := e.remote.recordedToken(sub, role)
	code, err := EncodeInviteCode(e.remote.srv.URL+e.remote.server.cfg.SyncAPIPath, tok)
	if err != nil {
		e.local.t.Fatalf("EncodeInviteCode: %v", err)
	}
	return code, claims
}

func rawInvite(v any) string {
	b, _ := json.Marshal(v)
	return InviteCodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

func TestInviteCodeRoundTrip(t *testing.T) {
	_, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.IssueToken(priv, "山田", "writer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const syncURL = "https://media.example.jp/api/sync-0123456789abcdef0123456789abcdef"
	code, err := EncodeInviteCode(syncURL, tok)
	if err != nil {
		t.Fatalf("EncodeInviteCode: %v", err)
	}
	body, ok := strings.CutPrefix(code, "sorus-invite:")
	if !ok || strings.ContainsAny(body, "=+/") {
		t.Fatalf("code = %q, want sorus-invite: + unpadded base64url", code)
	}
	var raw map[string]any
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(data, &raw) != nil {
		t.Fatalf("payload is not base64url JSON: %v", err)
	}
	if raw["v"] != float64(1) || raw["url"] != syncURL || raw["token"] != tok || len(raw) != 3 {
		t.Errorf("payload = %v", raw)
	}

	// メールやチャットで付いた前後の空白・改行、途中の改行は無視する。
	for _, in := range []string{code, "  " + code + "\n", "\r\n" + code[:30] + "\n" + code[30:] + "\t"} {
		inv, err := DecodeInviteCode(in)
		if err != nil {
			t.Fatalf("DecodeInviteCode(%q): %v", in, err)
		}
		if inv.V != 1 || inv.URL != syncURL || inv.Token != tok {
			t.Errorf("decoded = %+v", inv)
		}
	}

	// この PC 内のサーバーに限り http を使える。
	if _, err := EncodeInviteCode("http://127.0.0.1:8080/api/sync-0123456789abcdef0123456789abcdef", tok); err != nil {
		t.Errorf("loopback http: %v", err)
	}
	if _, err := DecodeInviteCode(rawInvite(map[string]any{"v": 1, "url": "http://localhost:9000/api/sync", "token": tok})); err != nil {
		t.Errorf("localhost http: %v", err)
	}
}

func TestInviteCodeRejects(t *testing.T) {
	_, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.IssueToken(priv, "山田", "writer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const syncURL = "https://media.example.jp/api/sync-0123456789abcdef0123456789abcdef"
	good, err := EncodeInviteCode(syncURL, tok)
	if err != nil {
		t.Fatal(err)
	}
	padded := good + strings.Repeat("=", (4-len(strings.TrimPrefix(good, InviteCodePrefix))%4)%4)
	for name, in := range map[string]string{
		"empty":            "",
		"no prefix":        strings.TrimPrefix(good, InviteCodePrefix),
		"other prefix":     "sorus-token:" + strings.TrimPrefix(good, InviteCodePrefix),
		"bare token":       tok,
		"not base64":       InviteCodePrefix + "%%%",
		"standard base64":  InviteCodePrefix + base64.StdEncoding.EncodeToString([]byte(`{"v":1,"url":"`+syncURL+`","token":"`+tok+`"}`)),
		"not json":         InviteCodePrefix + base64.RawURLEncoding.EncodeToString([]byte("hello")),
		"version 2":        rawInvite(map[string]any{"v": 2, "url": syncURL, "token": tok}),
		"version missing":  rawInvite(map[string]any{"url": syncURL, "token": tok}),
		"plain http":       rawInvite(map[string]any{"v": 1, "url": "http://media.example.jp/api/sync-0123456789abcdef0123456789abcdef", "token": tok}),
		"ftp":              rawInvite(map[string]any{"v": 1, "url": "ftp://media.example.jp/api/sync", "token": tok}),
		"no url":           rawInvite(map[string]any{"v": 1, "token": tok}),
		"user info":        rawInvite(map[string]any{"v": 1, "url": "https://u:p@media.example.jp/api/sync", "token": tok}),
		"bad sync path":    rawInvite(map[string]any{"v": 1, "url": "https://media.example.jp/admin/sync", "token": tok}),
		"broken token":     rawInvite(map[string]any{"v": 1, "url": syncURL, "token": "abc.def"}),
		"no token":         rawInvite(map[string]any{"v": 1, "url": syncURL}),
		"padded":           padded,
		"too long":         InviteCodePrefix + strings.Repeat("A", maxInviteCodeLength),
		"truncated":        good[:len(good)-10],
		"prefix only":      InviteCodePrefix,
		"text around code": "招待です " + good,
	} {
		if name == "padded" && padded == good {
			continue
		}
		if inv, err := DecodeInviteCode(in); err == nil {
			t.Errorf("%s: DecodeInviteCode = %+v, want error", name, inv)
		}
	}
	if _, err := DecodeInviteCode(rawInvite(map[string]any{"v": 2, "url": syncURL, "token": tok})); err == nil || !strings.Contains(err.Error(), "更新") {
		t.Errorf("version 2 error = %v, want a message to update sorus", err)
	}
	for name, u := range map[string]string{
		"plain http": "http://media.example.jp/api/sync",
		"no scheme":  "media.example.jp/api/sync",
	} {
		if _, err := EncodeInviteCode(u, tok); err == nil {
			t.Errorf("EncodeInviteCode(%s) succeeded, want error", name)
		}
	}
	if _, err := EncodeInviteCode(syncURL, "not-a-token"); err == nil {
		t.Error("EncodeInviteCode(broken token) succeeded, want error")
	}
}

func TestRemoteJoinWithInviteCode(t *testing.T) {
	env := newInviteRemoteEnv(t)
	c := env.local.loggedIn(admin)

	page := c.get("/admin/settings/remote")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, ">公開先</h1>", "公開サーバーに参加する", `name="invite_code"`, `action="/admin/settings/remote/join"`,
		"接続を確かめて参加する", `id="remote-advanced"`, "詳細設定", `name="remote_url"`, `name="remote_token"`, `src="/static/js/remote_invite.js?v=`)
	expectNotContains(t, page.Body, `id="remote-connection"`, "data-remote-invites")
	expectStatus(t, c.get("/admin/remote/invite.js"), http.StatusNotFound)
	if js := c.get("/static/js/remote_invite.js"); js.StatusCode != http.StatusOK || !strings.Contains(js.Body, "/admin/remote/invites") ||
		!strings.HasPrefix(js.Header.Get("Content-Type"), "text/javascript") {
		t.Errorf("invite.js = %d %s", js.StatusCode, js.Header.Get("Content-Type"))
	}

	t.Run("broken code keeps input and saves nothing", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote/join", url.Values{"invite_code": {"sorus-invite:broken"}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "招待コードの形式が正しくありません", ">sorus-invite:broken</textarea>")
		if cfg := env.local.server.loadRemoteConfig(); cfg.URL != "" || cfg.Token != "" {
			t.Errorf("config = %+v", cfg)
		}
		res = c.postForm("/admin/settings/remote/join", url.Values{"invite_code": {"  "}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "招待コードを貼り付けてください")
	})

	t.Run("server that rejects the token saves nothing", func(t *testing.T) {
		// 別の鍵で署名したトークン（別の公開サーバーの招待）は、接続の確認で断られる。
		_, otherPriv, err := auth.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		other, err := auth.IssueToken(otherPriv, "他人", "admin", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		code, err := EncodeInviteCode(env.remote.srv.URL+env.remote.server.cfg.SyncAPIPath, other)
		if err != nil {
			t.Fatal(err)
		}
		res := c.postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "まだ参加していません", "トークンが無効です")
		if cfg := env.local.server.loadRemoteConfig(); cfg.Token != "" {
			t.Errorf("rejected invite was saved: %+v", cfg)
		}
	})

	t.Run("expired invite", func(t *testing.T) {
		expired := signedToken(env.remote.priv, `{"alg":"EdDSA","typ":"sorus-cap"}`,
			fmt.Sprintf(`{"sub":"x","role":"writer","exp":%d,"iat":1700000000,"jti":"0123456789abcdef0123456789abcdef"}`, time.Now().Add(-time.Minute).Unix()))
		code, err := EncodeInviteCode(env.remote.srv.URL+env.remote.server.cfg.SyncAPIPath, expired)
		if err != nil {
			t.Fatal(err)
		}
		res := c.postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "有効期限が切れています")
	})

	code, claims := env.inviteFor("編集長", auth.RoleAdmin)
	res := c.postForm("/admin/settings/remote/join", url.Values{"invite_code": {"\n" + code + "\n"}})
	expectRedirect(t, res, "/admin/settings/remote?flash=remote_joined")
	cfg := env.local.server.loadRemoteConfig()
	if cfg.URL != env.remote.srv.URL || cfg.SyncPath != env.remote.server.cfg.SyncAPIPath || cfg.Token == "" {
		t.Fatalf("saved config = %+v", cfg)
	}
	if info, err := parseRemoteToken(cfg.Token, time.Now()); err != nil || info.ID != claims.ID {
		t.Fatalf("saved token = %+v, %v", info, err)
	}

	page = c.get("/admin/settings/remote?flash=remote_joined")
	expectStatus(t, page, http.StatusOK)
	host := strings.TrimPrefix(env.remote.srv.URL, "http://")
	expectContains(t, page.Body, "公開サーバーに参加しました", `id="remote-connection"`, host, "編集長", "サイト管理者",
		"招待の発行と取り消しができます", expiryText(time.Unix(claims.ExpiresAt, 0), time.Now()), "data-remote-invites", `id="remote-invite-form"`, "招待コードを作る", "取り消す」",
		"別の招待コードで参加し直す", InviteJoinHint[:len("sorus の「設定」→「公開先」で「公開サーバーに接続する」を開き")])
	expectNotContains(t, page.Body, cfg.Token, "古い形式", "remote-invite-legacy", "remote-connection-legacy", "公開サーバーに参加する")
	// 参加したあとの最初の画面は、いまの接続・一括同期・招待に絞る。参加し直す・詳細設定・接続の解除は、最下部の閉じた
	// 「接続・設定の管理」の中だけに置く。
	manageAt := strings.Index(page.Body, `<details id="remote-manage" class="publish-manage">`)
	if manageAt < 0 {
		t.Fatal("the connection management section should be closed while the connection is healthy")
	}
	for _, marker := range []string{`id="remote-join-form"`, `id="remote-advanced"`, `id="publish-method-clear-remote"`} {
		if at := strings.Index(page.Body, marker); at < manageAt {
			t.Errorf("%s must be inside the connection management section at the bottom (at %d, section at %d)", marker, at, manageAt)
		}
	}
	for _, marker := range []string{`id="remote-connection"`, `data-remote-invites`} {
		if at := strings.Index(page.Body, marker); at < 0 || at > manageAt {
			t.Errorf("%s must be in the first view, before the connection management section", marker)
		}
	}
}

func TestRemoteInviteAndRevokeFromLocalApp(t *testing.T) {
	env := newInviteRemoteEnv(t)
	c := env.local.loggedIn(admin)
	code, adminClaims := env.inviteFor("編集長", auth.RoleAdmin)
	expectRedirect(t, c.postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}}), "/admin/settings/remote")

	var issued struct {
		Message    string `json:"message"`
		InviteCode string `json:"invite_code"`
		JTI        string `json:"jti"`
		RoleLabel  string `json:"role_label"`
		ExpiresAt  string `json:"expires_at"`
	}
	res := c.postJSON("/admin/remote/invites", url.Values{"name": {"山田 花子"}, "role": {"writer"}, "ttl_days": {"30"}}, &issued)
	expectStatus(t, res, http.StatusOK)
	if !strings.Contains(issued.Message, "山田 花子") || issued.RoleLabel != "ライター" || !strings.HasSuffix(issued.ExpiresAt, "日") {
		t.Errorf("issued = %+v", issued)
	}
	inv, err := DecodeInviteCode(issued.InviteCode)
	if err != nil {
		t.Fatalf("invite code: %v", err)
	}
	if inv.URL != env.remote.srv.URL+env.remote.server.cfg.SyncAPIPath {
		t.Errorf("invite url = %q", inv.URL)
	}
	// ローカル CMS は招待コードを保存しない。
	var n int
	if err := env.local.db.Get(&n, `SELECT COUNT(*) FROM settings WHERE value LIKE ?`, "%"+inv.Token+"%"); err != nil || n != 0 {
		t.Errorf("issued token stored locally: %d, %v", n, err)
	}

	// 招待された人のアプリ（デスクトップモード。利用者は管理者の役割も持つ）が参加し、提出できる。
	writerApp, _ := newDesktopEnv(t)
	wc := writerApp.completeSetup()
	expectRedirect(t, wc.postForm("/admin/settings/remote/join", url.Values{"invite_code": {issued.InviteCode}}), "/admin/settings/remote")
	writerPage := wc.get("/admin/settings/remote")
	expectContains(t, writerPage.Body, "ライター", "記事を書いて公開サーバーへ提出できます")
	expectNotContains(t, writerPage.Body, "data-remote-invites")
	id := wc.createArticle("招待されて書いた記事", "")
	expectStatus(t, wc.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusOK)

	type tokenList struct {
		Tokens []remoteTokenItem `json:"tokens"`
	}
	var list tokenList
	expectStatus(t, c.getJSON("/admin/remote/tokens", &list), http.StatusOK)
	byID := map[string]remoteTokenItem{}
	for _, it := range list.Tokens {
		byID[it.JTI] = it
	}
	if len(list.Tokens) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if w := byID[issued.JTI]; w.Subject != "山田 花子" || w.RoleLabel != "ライター" || w.Status != "active" || w.StatusLabel != "使えます" || w.IssuedBy != "編集長" || w.Current {
		t.Errorf("writer item = %+v", w)
	}
	if a := byID[adminClaims.ID]; !a.Current || a.IssuedBy != "サーバーで発行" || a.RoleLabel != "サイト管理者" {
		t.Errorf("admin item = %+v", a)
	}

	// 取り消すと、招待された人のアプリからは提出できなくなる。
	var revoked struct {
		Message string          `json:"message"`
		Token   remoteTokenItem `json:"token"`
	}
	res = c.postJSON("/admin/remote/tokens/"+issued.JTI+"/revoke", nil, &revoked)
	expectStatus(t, res, http.StatusOK)
	if revoked.Token.Status != "revoked" || !strings.Contains(revoked.Token.StatusLabel, "取り消し済み") || !strings.Contains(revoked.Message, "山田 花子") {
		t.Errorf("revoke = %+v", revoked)
	}
	res = wc.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil)
	expectStatus(t, res, http.StatusBadGateway)
	if msg := jsonMessage(t, res); !strings.Contains(msg, "取り消されています") {
		t.Errorf("submit after revoke: %q", msg)
	}
	// 取り消された招待で参加し直すこともできない。
	res = wc.postForm("/admin/settings/remote/join", url.Values{"invite_code": {issued.InviteCode}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "取り消されています")

	t.Run("self revoke needs confirmation and keeps the last admin", func(t *testing.T) {
		res := c.postJSON("/admin/remote/tokens/"+adminClaims.ID+"/revoke", nil, nil)
		expectStatus(t, res, http.StatusConflict)
		res = c.postJSON("/admin/remote/tokens/"+adminClaims.ID+"/revoke", url.Values{"confirm_self": {"1"}}, nil)
		expectStatus(t, res, http.StatusBadGateway)
		if msg := jsonMessage(t, res); !strings.Contains(msg, "最後のサイト管理者") {
			t.Errorf("last admin message = %q", msg)
		}

		// 2人目のサイト管理者を招待すれば、確認のうえで自分の招待を取り消せる。
		res = c.postJSON("/admin/remote/invites", url.Values{"name": {"副編集長"}, "role": {"admin"}, "ttl_days": {"365"}}, nil)
		expectStatus(t, res, http.StatusOK)
		var self struct {
			Message string `json:"message"`
		}
		res = c.postJSON("/admin/remote/tokens/"+adminClaims.ID+"/revoke", url.Values{"confirm_self": {"1"}}, &self)
		expectStatus(t, res, http.StatusOK)
		if !strings.Contains(self.Message, InviteJoinHint) {
			t.Errorf("self revoke message = %q", self.Message)
		}
		res = c.getJSON("/admin/remote/tokens", nil)
		expectStatus(t, res, http.StatusBadGateway)
	})

	t.Run("bad input", func(t *testing.T) {
		expectStatus(t, c.postJSON("/admin/remote/tokens/not-a-jti/revoke", nil, nil), http.StatusBadRequest)
		for _, form := range []url.Values{
			{"name": {""}, "role": {"writer"}, "ttl_days": {"30"}},
			{"name": {"x"}, "role": {"owner"}, "ttl_days": {"30"}},
			{"name": {"x"}, "role": {"writer"}, "ttl_days": {"0"}},
			{"name": {"x"}, "role": {"writer"}, "ttl_days": {"3651"}},
			{"name": {"x"}, "role": {"writer"}, "ttl_days": {"abc"}},
		} {
			if res := c.postJSON("/admin/remote/invites", form, nil); res.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("invite %v: status %d, want 422", form, res.StatusCode)
			}
		}
	})
}

// 招待の管理はローカルの管理者だけが、admin のトークンで行える。
func TestRemoteInvitePermissions(t *testing.T) {
	env := newInviteRemoteEnv(t)
	code, _ := env.inviteFor("編集長", auth.RoleAdmin)
	adminClient := env.local.loggedIn(admin)
	expectRedirect(t, adminClient.postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}}), "/admin/settings/remote")

	writer := env.local.loggedIn(studentA)
	expectStatus(t, writer.postJSON("/admin/remote/invites", url.Values{"name": {"x"}, "role": {"writer"}, "ttl_days": {"30"}}, nil), http.StatusForbidden)
	expectStatus(t, writer.getJSON("/admin/remote/tokens", nil), http.StatusForbidden)
	expectStatus(t, writer.get("/admin/settings/remote"), http.StatusForbidden)
	expectStatus(t, adminClient.postRaw("/admin/remote/invites", url.Values{"name": {"x"}, "role": {"writer"}, "ttl_days": {"30"}}), http.StatusForbidden)

	// 編集者の招待で参加していると、ローカルの管理者でも招待は扱えない。
	editorCode, _ := env.inviteFor("佐藤", auth.RoleEditor)
	expectRedirect(t, adminClient.postForm("/admin/settings/remote/join", url.Values{"invite_code": {editorCode}}), "/admin/settings/remote")
	res := adminClient.postJSON("/admin/remote/invites", url.Values{"name": {"x"}, "role": {"writer"}, "ttl_days": {"30"}}, nil)
	expectStatus(t, res, http.StatusForbidden)
	expectNotContains(t, adminClient.get("/admin/settings/remote").Body, "data-remote-invites")
}

// ID の無い招待トークンは、PC 側でも受け付けない。
func TestParseRemoteTokenRequiresID(t *testing.T) {
	_, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	token := signedToken(priv, `{"alg":"EdDSA","typ":"sorus-cap"}`,
		fmt.Sprintf(`{"sub":"昔の管理者","role":"admin","exp":%d,"iat":%d}`, time.Now().Add(time.Hour).Unix(), time.Now().Unix()))
	if _, err := parseRemoteToken(token, time.Now()); err == nil {
		t.Fatal("parseRemoteToken accepted an invite token without an ID")
	}
}

func TestExpiryText(t *testing.T) {
	now := time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		exp  time.Time
		want string
	}{
		{now.Add(365 * 24 * time.Hour), "2027年10月28日まで（あと365日）"},
		{now.Add(36 * time.Hour), "2026年10月30日まで（あと2日）"},
		{now.Add(2 * time.Hour), "今日の 14:00 まで"},
		{now.Add(-time.Hour), "期限切れ（2026年10月28日まで）"},
	} {
		if got := expiryText(tc.exp, now); got != tc.want {
			t.Errorf("expiryText(%v) = %q, want %q", tc.exp, got, tc.want)
		}
	}
}

// サイト全体の公開サーバーの接続（URL とトークン）を変えられるのは管理者だけ。サーバーモードのライター・編集者は、
// 設定の画面・保存・招待での参加・接続テストのどれも 403 になり、管理者が保存した設定は変わらない。
// 編集者は、管理者が設定した連携で「届いた原稿」から公開サーバーの校閲待ちを受け取れる。
func TestRemoteSettingsAdminOnly(t *testing.T) {
	env := newInviteRemoteEnv(t)
	code, _ := env.inviteFor("編集長", auth.RoleAdmin)
	expectRedirect(t, env.local.loggedIn(admin).postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}}), "/admin/settings/remote")
	before := env.local.server.loadRemoteConfig()
	other, _ := env.inviteFor("別の人", auth.RoleWriter)
	otherInv, err := DecodeInviteCode(other)
	if err != nil {
		t.Fatal(err)
	}

	for _, u := range []testUser{studentA, chief} {
		c := env.local.loggedIn(u)
		expectStatus(t, c.get("/admin/settings/remote"), http.StatusForbidden)
		expectStatus(t, c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://evil.example.com"}, "remote_token": {otherInv.Token}}), http.StatusForbidden)
		expectStatus(t, c.postForm("/admin/settings/remote", url.Values{"remote_url": {before.URL + before.SyncPath}, "clear_token": {"1"}}), http.StatusForbidden)
		expectStatus(t, c.postForm("/admin/settings/remote/join", url.Values{"invite_code": {other}}), http.StatusForbidden)
		expectStatus(t, c.postJSON("/admin/remote/test", url.Values{"remote_url": {otherInv.URL}, "remote_token": {otherInv.Token}}, nil), http.StatusForbidden)
	}
	if after := env.local.server.loadRemoteConfig(); after != before {
		t.Errorf("remote config changed by a writer or editor: %+v, want %+v", after, before)
	}

	id := env.remote.seedReview("サーバーに届いた原稿", "server-one")
	ed := env.local.loggedIn(chief)
	inbox := ed.get("/admin/manuscripts")
	expectStatus(t, inbox, http.StatusOK)
	expectContains(t, inbox.Body, "サーバーに届いた原稿", "data-remote-inbox-import=")
	expectStatus(t, ed.getJSON("/admin/remote/pending", nil), http.StatusOK)
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/import", nil, nil), http.StatusOK)
}
