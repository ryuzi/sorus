package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/auth"

	"github.com/jmoiron/sqlx"
)

// 記事公開時の自動反映（reflectAfterPublish）: 記事を今すぐ公開したときに、公開先へ自動で反映するか・利用者に何を知らせるか。

// shortenReflectDebounce は自動の反映（記事公開時・予約投稿）を始めるまでの待ち時間を d にする。
func shortenReflectDebounce(s *Server, d time.Duration) {
	st := s.publishState()
	st.mu.Lock()
	st.debounce = d
	st.mu.Unlock()
}

// reflectState は始めた反映の数と、自動の反映を始めるのを待っているか（タイマーがあるか）を返す。
func reflectState(s *Server) (reflects int, pending bool) {
	st := s.publishState()
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.reflects, st.timer != nil
}

// configureFolderReflect はフォルダの公開先を保存し、自動の反映を始めるまでの待ち時間を debounce にして、
// 書き出し先のフォルダを返す。
func configureFolderReflect(t *testing.T, env *testEnv, debounce time.Duration) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "site-export")
	saveFolderDestination(t, env.loggedIn(admin), out)
	shortenReflectDebounce(env.server, debounce)
	return out
}

// articleFormFor は記事フォームの送信（スラッグを決めて、押したボタンを action にする）。
func articleFormFor(title, slug, action string) url.Values {
	return url.Values{"title": {title}, "slug": {slug}, "category_id": {"1"},
		"content_markdown": {title + "の本文"}, "action": {action}}
}

func TestArticleUpdateConflictDetection(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("競合検出", "conflict-detection", "draft")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	a, err := env.server.articles.GetByID(id)
	if err != nil {
		t.Fatal(err)
	}

	stale := articleFormFor("古い内容", "conflict-detection", "draft")
	stale.Set("base_updated_at", strconv.FormatInt(a.UpdatedAt.Unix()-100, 10))
	res := ed.postForm("/admin/articles/"+id, stale)
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, "他の端末またはユーザーによって記事が更新されています")

	current := articleFormFor("最新の内容", "conflict-detection", "draft")
	current.Set("base_updated_at", strconv.FormatInt(a.UpdatedAt.Unix(), 10))
	res = ed.postForm("/admin/articles/"+id, current)
	expectStatus(t, res, http.StatusSeeOther)
}

// expectFlash は Location の ?flash= が want であることを確かめる。
func expectFlash(t *testing.T, loc, want string) {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("flash"); got != want {
		t.Fatalf("Location = %q, want flash=%s", loc, want)
	}
}

// expectNoReflect は待ち時間を過ぎても、反映を始めず・待ってもいないことを確かめる。
func expectNoReflect(t *testing.T, env *testEnv, step string, debounce time.Duration) {
	t.Helper()
	time.Sleep(3 * debounce)
	if reflects, pending := reflectState(env.server); reflects != 0 || pending {
		t.Fatalf("%s: reflects = %d (timer pending %v), want no reflect", step, reflects, pending)
	}
}

// 新規作成・編集画面が、端末内下書きの判定に必要な更新日時と復元 UI を含めて返す。
func TestArticleEditorIncludesOfflineDraftSupport(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)

	newPage := ed.get("/admin/articles/new")
	expectStatus(t, newPage, http.StatusOK)
	expectContains(t, newPage.Body, `data-updated-at="0"`, `id="offline-draft-banner"`, `/static/js/offline_draft.js`)

	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("下書き復元テスト", "offline-draft", "draft")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	editPage := ed.get("/admin/articles/" + id + "/edit")
	expectStatus(t, editPage, http.StatusOK)
	expectContains(t, editPage.Body, `id="offline-draft-banner"`, `/static/js/offline_draft.js`)
	if !strings.Contains(editPage.Body, `data-updated-at="`) || strings.Contains(editPage.Body, `data-updated-at="0"`) {
		t.Errorf("saved article edit page should have a nonzero data-updated-at")
	}
}

// 未保存の下書きのバナーに「差分を確認」ボタンと差分の表示枠があり、差分の文言が表示言語の辞書から
// data-* 属性で渡される（offline_draft.js が textContent で組み立てる）。
func TestArticleEditorOfflineDraftDiffUI(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("差分表示テスト", "offline-draft-diff", "draft")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]

	ja := ed.get("/admin/articles/" + id + "/edit")
	expectStatus(t, ja, http.StatusOK)
	expectContains(t, ja.Body,
		`id="offline-draft-diff-toggle"`, `aria-controls="offline-draft-diff"`, `aria-expanded="false"`,
		`id="offline-draft-diff" class="hidden`,
		`data-msg-view-diff="差分を確認"`, `data-msg-hide-diff="差分を閉じる"`,
		`data-msg-current="現在（サーバー）"`, `data-msg-local="未保存の下書き"`,
		`data-msg-no-body-diff="本文に違いはありません。"`,
		`data-label-title="タイトル"`, `data-label-content-markdown="本文"`, `data-label-category-id="カテゴリ"`)
	// ギャラリーのタグ入力の例は、特定の業界・行事に寄らない汎用の言葉にする。
	expectContains(t, ja.Body, `placeholder="例: イベント"`)
	if strings.Contains(ja.Body, "例: 学園祭") {
		t.Error("gallery tag placeholder should use a generic example, not 学園祭")
	}

	expectStatus(t, ed.get("/admin/set-lang?lang=en&return=/admin"), http.StatusSeeOther)
	en := ed.get("/admin/articles/" + id + "/edit")
	expectStatus(t, en, http.StatusOK)
	expectContains(t, en.Body,
		`id="offline-draft-diff-toggle"`, `>View changes</button>`,
		`data-msg-view-diff="View changes"`, `data-msg-hide-diff="Hide changes"`,
		`data-msg-current="Current (server)"`, `data-msg-local="Unsaved draft"`,
		`data-label-title="Title"`, `data-label-content-markdown="Body"`, `placeholder="e.g. event"`)
}

// offline_draft.js は差分を DOM API と textContent で組み立て、端末の下書きを innerHTML に流し込まない。
func TestOfflineDraftScriptBuildsDiffWithTextContent(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "static", "js", "offline_draft.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{"function renderDiff(", "function lineDiff(", "offline-draft-diff-toggle", "textContent", "'content_markdown'", "'category_id'", "'excerpt'"} {
		if !strings.Contains(src, want) {
			t.Errorf("offline_draft.js should contain %q", want)
		}
	}
	for _, banned := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(src, banned) {
			t.Errorf("offline_draft.js must not use %s (draft text is user input)", banned)
		}
	}
}

func TestArticleCreateAndSaveWithDraftToken(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	form := articleFormFor("Token test", "", "draft")
	form.Set("draft_token", "token-abc")
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	id := editPathPattern.FindStringSubmatch(u.Path)
	if len(id) != 2 {
		t.Fatalf("redirect path %q does not identify the created article", u.Path)
	}
	if got := u.Query().Get("saved_draft_token"); got != "token-abc" {
		t.Fatalf("saved_draft_token = %q, want token-abc (Location %q)", got, loc)
	}
	editPage := ed.get(loc)
	expectStatus(t, editPage, http.StatusOK)
	expectContains(t, editPage.Body, `data-saved-draft-token="token-abc"`)

	form = articleFormFor("Token test updated ", "", "draft")
	form.Set("draft_token", "token-save-456")
	a, err := env.server.articles.GetByID(id[1])
	if err != nil {
		t.Fatal(err)
	}
	form.Set("base_updated_at", strconv.FormatInt(a.UpdatedAt.Unix(), 10))
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+id[1], form), "/admin/articles/")
	u, err = url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("saved_draft_token"); got != "token-save-456" {
		t.Fatalf("saved_draft_token = %q, want token-save-456 (Location %q)", got, loc)
	}
}

func TestArticleCreateReviewAndSchedulePreserveDraftToken(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)

	review := articleFormFor("Token review", "token-review", "review")
	review.Set("draft_token", "review-token")
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", review), "/admin/articles?")
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("saved_draft_token"); got != "review-token" {
		t.Fatalf("review saved_draft_token = %q, want review-token (Location %q)", got, loc)
	}

	scheduled := articleFormFor("Token scheduled", "token-scheduled", "publish")
	scheduled.Set("publish_at", futureLocal(2*time.Hour))
	scheduled.Set("draft_token", "scheduled-token")
	loc = expectRedirect(t, ed.postForm("/admin/articles/new", scheduled), "/admin/articles/")
	parsed, err = url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("saved_draft_token"); got != "scheduled-token" {
		t.Fatalf("scheduled saved_draft_token = %q, want scheduled-token (Location %q)", got, loc)
	}
}

// 公開先を決めてあれば、作成と同時の公開・下書きからの公開・公開中の記事の更新のたびに、少し待ってから自動で反映する。
// 待っているあいだも反映中として出し、予約投稿にしたときは自動の反映を予約しない。
func TestArticlePublishAutoReflects(t *testing.T) {
	env := newTestEnv(t)
	out := configureFolderReflect(t, env, 300*time.Millisecond)
	ed := env.loggedIn(chief)
	adm := env.loggedIn(admin)

	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("作成と同時に公開", "created-now", "publish")), "/admin/articles/")
	expectFlash(t, loc, flashPublishedReflecting)
	// 反映を始めるのを待っているあいだも、状態の API は反映中として返し、画面が結果まで読み続けられるようにする。
	res := adm.get("/admin/publish/status")
	expectStatus(t, res, http.StatusOK)
	var status publishStatus
	if err := json.Unmarshal([]byte(res.Body), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.Message == "" {
		t.Errorf("status while waiting for the automatic reflect = %+v, want running with a message", status)
	}
	expectContains(t, ed.get(loc).Body, "記事を公開しました。静的サイトへの反映を開始しました")
	got := waitReflectDone(t, env.server)
	if !got.OK || got.Trigger != publishTriggerArticlePublish {
		t.Fatalf("reflect after creating a published article = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(out, "articles", "created-now", "index.html")); err != nil {
		t.Errorf("article published on create not reflected: %v", err)
	}

	// 下書きから公開する。
	loc = expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("下書きから公開", "from-draft", "draft")), "/admin/articles/")
	expectFlash(t, loc, "created")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	if reflects, pending := reflectState(env.server); reflects != 1 || pending {
		t.Fatalf("after saving a draft: reflects = %d (timer pending %v), want no new reflect", reflects, pending)
	}
	expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/"+id, articleFormFor("下書きから公開", "from-draft", "publish")),
		"/admin/articles/"+id+"/edit"), flashPublishedReflecting)
	if got := waitReflectDone(t, env.server); !got.OK || got.Trigger != publishTriggerArticlePublish {
		t.Fatalf("reflect after publishing a draft = %+v", got)
	}
	page := filepath.Join(out, "articles", "from-draft", "index.html")
	if _, err := os.Stat(page); err != nil {
		t.Fatalf("article published from a draft not reflected: %v", err)
	}

	// 公開中の記事を、公開したまま直す。
	expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/"+id, articleFormFor("公開したまま直した記事", "from-draft", "save")),
		"/admin/articles/"+id+"/edit"), flashPublishedReflecting)
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("reflect after updating a published article = %+v", got)
	}
	if b, err := os.ReadFile(page); err != nil || !strings.Contains(string(b), "公開したまま直した記事") {
		t.Errorf("updated article not reflected (err %v)", err)
	}
	if reflects, _ := reflectState(env.server); reflects != 3 {
		t.Errorf("reflects = %d, want 3 (one per publish)", reflects)
	}

	// 日時を決めて予約したときは、予約時刻が来てから予約投稿の反映で送るため、自動の反映を予約しない。
	form := articleFormFor("予約した記事", "scheduled-later", "publish")
	form.Set("publish_at", futureLocal(2*time.Hour))
	expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/"), "scheduled")
	time.Sleep(3 * 300 * time.Millisecond)
	if reflects, pending := reflectState(env.server); reflects != 3 || pending {
		t.Errorf("after scheduling: reflects = %d (timer pending %v), want no automatic reflect", reflects, pending)
	}
}

// 「記事公開時に自動でサイトに反映する」を外していれば、公開しても反映せず、「サイトに反映」を押すよう知らせる。
func TestArticlePublishWithoutAutoReflect(t *testing.T) {
	env := newTestEnv(t)
	const debounce = 50 * time.Millisecond
	out := configureFolderReflect(t, env, debounce)
	if err := upsertSetting(env.db, publishSettingAutoReflect, "0"); err != nil {
		t.Fatal(err)
	}
	ed := env.loggedIn(chief)

	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("自動で送らない記事", "no-auto", "publish")), "/admin/articles/")
	expectFlash(t, loc, flashPublishedManual)
	expectContains(t, ed.get(loc).Body, "記事を公開しました。静的サイトへ反映するには『サイトに反映』を実行してください")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/"+id, articleFormFor("自動で送らない記事", "no-auto", "save")),
		"/admin/articles/"+id+"/edit"), flashPublishedManual)
	expectNoReflect(t, env, "auto reflect off", debounce)
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("export folder written with the automatic reflect off: %v", err)
	}
}

// 公開中の記事を下書きに戻すかゴミ箱へ移したら、自動反映で公開先から取り下げる。
func TestArticleUnpublishAutoReflects(t *testing.T) {
	const debounce = 40 * time.Millisecond
	env := newTestEnv(t)
	out := configureFolderReflect(t, env, debounce)
	ed := env.loggedIn(chief)

	for _, action := range []string{"draft", "trash"} {
		t.Run(action, func(t *testing.T) {
			loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("取り下げる記事", "unpublish-"+action, "publish")), "/admin/articles/")
			id := editPathPattern.FindStringSubmatch(loc)[1]
			if got := waitReflectDone(t, env.server); !got.OK {
				t.Fatalf("initial publish reflect = %+v", got)
			}
			page := filepath.Join(out, "articles", "unpublish-"+action, "index.html")
			if _, err := os.Stat(page); err != nil {
				t.Fatalf("initially published article missing: %v", err)
			}

			if action == "draft" {
				form := articleFormFor("取り下げる記事", "unpublish-"+action, action)
				expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
			} else {
				expectRedirect(t, ed.postForm("/admin/articles/"+id+"/delete", nil), "/admin/articles?status=deleted")
			}
			if got := waitReflectDone(t, env.server); !got.OK {
				t.Fatalf("reflect after %s = %+v", action, got)
			}
			if _, err := os.Stat(page); !os.IsNotExist(err) {
				t.Errorf("article still exists in static site after %s (stat error %v)", action, err)
			}
		})
	}
}

// 公開先が決まっていなければ反映しない。デスクトップモードでは公開先を決めるよう知らせ、サーバーモード（sorus が
// 公開サイトそのもの）では、いつもの通知のままにする。以前の版の「はじめに」で保存した立場（desktop_intent）は読まない。
func TestArticlePublishWithoutDestination(t *testing.T) {
	const debounce = 50 * time.Millisecond
	t.Run("server", func(t *testing.T) {
		env := newTestEnv(t)
		shortenReflectDebounce(env.server, debounce)
		ed := env.loggedIn(chief)
		loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("公開先なし", "no-target", "publish")), "/admin/articles/")
		expectFlash(t, loc, "created")
		id := editPathPattern.FindStringSubmatch(loc)[1]
		expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/"+id, articleFormFor("公開先なし", "no-target", "save")),
			"/admin/articles/"+id+"/edit"), "published")
		expectNoReflect(t, env, "server without a destination", debounce)
	})
	t.Run("desktop", func(t *testing.T) {
		env, _ := newDesktopEnv(t)
		shortenReflectDebounce(env.server, debounce)
		c := env.completeSetup()
		loc := expectRedirect(t, c.postForm("/admin/articles/new", articleFormFor("公開先なし", "no-target", "publish")), "/admin/articles/")
		expectFlash(t, loc, flashPublishedNoTarget)
		expectContains(t, c.get(loc).Body, "記事を公開しました。（公開先が未設定のため、公開先設定画面から設定してください）")

		if err := upsertSetting(env.db, "desktop_intent", "writer"); err != nil {
			t.Fatal(err)
		}
		id := editPathPattern.FindStringSubmatch(loc)[1]
		expectFlash(t, expectRedirect(t, c.postForm("/admin/articles/"+id, articleFormFor("公開先なし", "no-target", "save")),
			"/admin/articles/"+id+"/edit"), flashPublishedNoTarget)
		expectNoReflect(t, env, "desktop without a destination", debounce)
	})
}

// デスクトップモードで、起動する前に予約時刻を過ぎた記事について尋ねているあいだは、記事を公開しても自動では
// 反映しない（尋ねた記事が一緒に送られるため）。「サイトに反映」を押すよう知らせる。
func TestDesktopOverdueSkipsArticlePublishReflect(t *testing.T) {
	out := filepath.Join(t.TempDir(), "site-export")
	env := newDesktopEnvWith(t, func(env *testEnv) { seedOverdueDesktop(t, env, out) })
	const debounce = 50 * time.Millisecond
	shortenReflectDebounce(env.server, debounce)
	st := env.server.publishState()
	waitForCond(t, "overdue article counted", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.overdue == 1
	})
	c := env.loggedIn(testUser{ID: "w", Username: "writer0"})
	loc := expectRedirect(t, c.postForm("/admin/articles/new", articleFormFor("尋ねているあいだに公開", "while-asking", "publish")), "/admin/articles/")
	expectFlash(t, loc, flashPublishedManual)
	expectNoReflect(t, env, "overdue articles awaiting an answer", debounce)
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("export folder written while asking about overdue articles: %v", err)
	}
}

func TestArticleTranslatePartialAndFull(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnvWith(t, Config{
		AI:              fake,
		SiteName:        "テストラボ",
		SiteDescription: "先端研究の情報発信",
	}, nil)
	c := env.loggedIn(studentA)

	// 1. 原文記事を作成
	srcID := c.createArticle("日本語タイトル", "# 日本語本文\n\n段落です。")

	// 2. 記事全体の一括翻訳（従来互換）
	t.Run("full article translation backward compatibility", func(t *testing.T) {
		reqBody := `{"source_article_id":"` + srcID + `","target_lang":"en"}`
		res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(reqBody))
		expectStatus(t, res, http.StatusOK)

		var out struct {
			OK       bool   `json:"ok"`
			Title    string `json:"title"`
			Excerpt  string `json:"excerpt"`
			Markdown string `json:"markdown"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			t.Fatalf("unmarshal json: %v\n%s", err, res.Body)
		}
		if !out.OK || out.Title != "Translated: 日本語タイトル" || out.Markdown != "Translated: # 日本語本文\n\n段落です。" {
			t.Errorf("full translation result mismatch: %+v", out)
		}
	})

	// 3. 部分テキストの意図指定型翻訳・推敲
	t.Run("partial intent-driven translation", func(t *testing.T) {
		reqBody := `{
			"text": "この部分は親しみやすく翻訳してください。",
			"target_lang": "en",
			"source_lang": "ja",
			"style": "friendly",
			"instruction": "高校生向けにわかりやすく",
			"source_article_id": "` + srcID + `"
		}`
		res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(reqBody))
		expectStatus(t, res, http.StatusOK)

		var out struct {
			OK             bool   `json:"ok"`
			TranslatedText string `json:"translated_text"`
			Error          string `json:"error"`
		}
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			t.Fatalf("unmarshal json: %v\n%s", err, res.Body)
		}
		if !out.OK || out.TranslatedText != "Translated: この部分は親しみやすく翻訳してください。" {
			t.Errorf("partial translation result mismatch: %+v", out)
		}

		fake.mu.Lock()
		lastOpts := fake.lastTranslateOpts
		fake.mu.Unlock()

		if lastOpts.Style != "friendly" {
			t.Errorf("opts.Style = %q, want friendly", lastOpts.Style)
		}
		if lastOpts.Instruction != "高校生向けにわかりやすく" {
			t.Errorf("opts.Instruction = %q, want 高校生向けにわかりやすく", lastOpts.Instruction)
		}
		if !strings.Contains(lastOpts.SiteContext, "テストラボ") {
			t.Errorf("opts.SiteContext = %q, want to contain テストラボ", lastOpts.SiteContext)
		}
	})

	// 4. パラメータ不足時のバリデーション
	t.Run("validation on empty params", func(t *testing.T) {
		reqBody := `{"target_lang":"en"}`
		res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(reqBody))
		expectStatus(t, res, http.StatusBadRequest)
	})

	// Critical 1: 空白テキスト送信時に全体翻訳へフォールスルーせず400を返す
	t.Run("blank text validation blocks fallthrough", func(t *testing.T) {
		reqBody := `{"text":"   \t\n  ","source_article_id":"` + srcID + `","target_lang":"en"}`
		res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(reqBody))
		expectStatus(t, res, http.StatusBadRequest)
		expectContains(t, res.Body, "翻訳するテキストを入力してください")
	})

	// Major 7: 前後文脈の受け渡し確認
	t.Run("surrounding context passed to translation options", func(t *testing.T) {
		reqBody := `{
			"text": "中心テキスト",
			"target_lang": "en",
			"before_context": "これは直前の文章です。",
			"after_context": "これは直後の文章です。"
		}`
		res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(reqBody))
		expectStatus(t, res, http.StatusOK)

		fake.mu.Lock()
		lastOpts := fake.lastTranslateOpts
		fake.mu.Unlock()

		if lastOpts.BeforeContext != "これは直前の文章です。" {
			t.Errorf("opts.BeforeContext = %q, want これは直前の文章です。", lastOpts.BeforeContext)
		}
		if lastOpts.AfterContext != "これは直後の文章です。" {
			t.Errorf("opts.AfterContext = %q, want これは直後の文章です。", lastOpts.AfterContext)
		}
	})

	// Major 2 & 3: DeepL指定＋指示ありでLLM未設定の場合のスマートルーティングと警告
	t.Run("deepl with instruction warning when llm unconfigured", func(t *testing.T) {
		reqBody := `{
			"text": "テキスト",
			"target_lang": "en",
			"provider": "deepl",
			"instruction": "丁寧な口調で"
		}`
		// DeepL未設定（キーなし）の場合は APIキー未設定エラー (400) になるが、
		// 構成済みプロバイダーであれば警告付きで 200 が返る。
		// ここでは fakeAI ではなく deepl プロバイダーが初期化されるためキー未設定(400)を検証
		res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(reqBody))
		// DeepL の API キーが空なので 400 Bad Request
		expectStatus(t, res, http.StatusBadRequest)
		expectContains(t, res.Body, "翻訳AIのAPIキーが未設定です")
	})

	// 5. エディタ画面でのUI要素レンダリング確認
	t.Run("render partial translation UI elements in editor", func(t *testing.T) {
		res := c.get("/admin/articles/" + srcID + "/edit")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`data-ai-partial`,
			`id="partial-translate-modal"`,
			`name="partial-style"`,
			`id="partial-translate-instruction"`,
			`id="partial-translate-submit"`,
		)
	})
}

type failOnTranslateAI struct {
	fakeAI
	failAt int
	calls  int
}

func (f *failOnTranslateAI) Translate(_ context.Context, text string, _, _ string, _ ai.TranslateOptions) (string, error) {
	f.calls++
	if f.calls == f.failAt {
		return "", ai.ErrTruncated
	}
	return "Translated: " + text, nil
}

func TestArticleTranslateDoesNotReturnRawAIError(t *testing.T) {
	for _, tc := range []struct {
		name, request, prefix string
		failAt                int
	}{
		{"partial", `{"text":"本文","target_lang":"en"}`, "翻訳処理に失敗しました: ", 1},
		{"title", `{"source_article_id":"%s","target_lang":"en"}`, "タイトルの翻訳に失敗しました: ", 1},
		{"excerpt", `{"source_article_id":"%s","target_lang":"en"}`, "抜粋の翻訳に失敗しました: ", 2},
		{"body", `{"source_article_id":"%s","target_lang":"en"}`, "本文の翻訳に失敗しました: ", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &failOnTranslateAI{failAt: tc.failAt}
			env := newTestEnvWith(t, Config{AI: fake}, nil)
			c := env.loggedIn(studentA)
			srcID := c.createArticle("日本語タイトル", "本文")
			if tc.name != "partial" {
				if _, err := env.db.Exec(`UPDATE articles SET excerpt = '抜粋' WHERE id = ?`, srcID); err != nil {
					t.Fatal(err)
				}
			}
			body := tc.request
			if tc.name != "partial" {
				body = fmt.Sprintf(body, srcID)
			}
			res := c.postAPI("/admin/articles/translate", "application/json", strings.NewReader(body))
			expectStatus(t, res, http.StatusOK)
			var out struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
				t.Fatal(err)
			}
			want := tc.prefix + "AIの応答が途中で切れたため、保存していません。記事を短く分けてもう一度試してください"
			if out.OK || out.Error != want || strings.Contains(out.Error, ai.ErrTruncated.Error()) {
				t.Errorf("translation error = %+v, want safe Japanese error %q", out, want)
			}
		})
	}
}

// 記事編集画面の UX（desktop_quit_and_editor_ux_spec.md §1.3）: 装飾ツールバーと「? 書き方ガイド」、
// シンタックスハイライトのミラー層、メディアバー、カテゴリ管理リンクの見た目を表示言語ごとに確かめる。
func TestArticleEditorFormattingToolbarAndGuide(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)

	ja := ed.get("/admin/articles/new")
	expectStatus(t, ja, http.StatusOK)
	expectContains(t, ja.Body,
		`id="editor-format-toolbar" role="toolbar" aria-controls="content_markdown"`,
		`data-md-format="heading"`, `data-md-format="bold"`, `data-md-format="link"`, `data-md-format="list"`,
		`data-md-format="quote"`, `data-md-format="code"`, `data-md-format="math"`,
		`>見出し</button>`, `>太字</button>`, `>リンク</button>`, `>箇条書き</button>`, `>引用</button>`, `>コード</button>`, `>数式</button>`,
		`id="editor-writing-guide" class="help-tip`, `>? 書き方ガイド</button>`, `<code>$E=mc^2$</code>`, `<code>$$</code>`,
		`詳しい方向け: Markdown 記法に対応しています。`,
		// ハイライトは textarea の背面の、読み上げない <pre> のミラー層。
		`<div id="editor-wrap" class="editor-wrap">`, `<pre class="editor-highlight text-base lg:text-sm" aria-hidden="true"><code id="editor-highlight"></code></pre>`,
		`id="content_markdown" name="content_markdown"`,
		// 画像の挿入はドロップ枠の横のメディアバーにまとめる。
		`id="editor-media-bar"`, `id="dropzone" for="image-input"`, `id="gallery-modal-open"`, `id="media-picker-open" data-media-picker-open="insert"`,
		// カテゴリ管理は青い下線リンクではなく、控えめなボタン。
		`id="category-manage-link" class="btn btn-ghost btn-sm`,
		// プレビューは簡易表示と明示し、保存前は公開デザインで確かめられないことを案内する。
		`内容のプレビュー（簡易表示）`, `id="public-design-hint"`, `下書き保存すると公開デザインで確認できます`)
	// 「選択範囲をAI推敲」は「文章を整える ▾」の項目だけにし、装飾バーの重複ボタンは出さない（admin_ux_phase5_spec.md §1.4）。
	expectNotContains(t, ja.Body, `text-blue-700 underline`, `<span>Markdown <span`, `id="public-design-link"`, `partial-translate-open-btn`)
	toolbar := ja.Body[strings.Index(ja.Body, `id="editor-format-toolbar"`):]
	toolbar = toolbar[:strings.Index(toolbar, `id="editor-wrap"`)]
	expectNotContains(t, toolbar, `AI推敲`, `AI翻訳`, `data-ai-partial`)
	if strings.Index(ja.Body, `id="editor-format-toolbar"`) > strings.Index(ja.Body, `id="content_markdown"`) {
		t.Error("formatting toolbar should be placed above the body textarea")
	}
	if strings.Index(ja.Body, `id="editor-highlight"`) > strings.Index(ja.Body, `id="content_markdown"`) {
		t.Error("highlight mirror should come before (behind) the body textarea")
	}

	expectStatus(t, ed.get("/admin/set-lang?lang=en&return=/admin"), http.StatusSeeOther)
	en := ed.get("/admin/articles/new")
	expectStatus(t, en, http.StatusOK)
	expectContains(t, en.Body, `>Heading</button>`, `>Bold</button>`, `>Link</button>`, `>List</button>`, `>Quote</button>`,
		`>Code</button>`, `>Math</button>`, `>? Writing guide</button>`, `For advanced users: Markdown syntax is supported.`,
		`Content preview (quick view)`, `Save a draft to check it in the site design`)
	expectNotContains(t, en.Body, `見出し</button>`, `書き方ガイド`)
}

// 「公開デザインで確認 ↗」（admin_ux_phase5_spec.md §1.4）は、保存済みの記事ならステータスを問わず出す。下書き・校閲待ち・
// 予約中は公開テーマで保存済みの内容を表示する /admin/articles/{id}/preview を、公開中は公開 URL を別タブで開く。
// 未保存の新規記事だけは、下書き保存すると確かめられることを案内する。
func TestArticleEditorPublicDesignLink(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	var reviewID, scheduledID string
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "デザイン確認", "design-check", "")
		reviewID = seedArticle(t, db, article.Article{Title: "校閲待ちの記事", Slug: "review-design", CategoryID: catID, ContentMarkdown: "本文", Status: article.StatusReview})
		scheduledID = seedArticle(t, db, article.Article{Title: "予約中の記事", Slug: "scheduled-design", CategoryID: catID, ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: &future})
	})
	ed := env.loggedIn(chief)

	newPage := ed.get("/admin/articles/new")
	expectStatus(t, newPage, http.StatusOK)
	expectContains(t, newPage.Body, `id="public-design-hint"`, `下書き保存すると公開デザインで確認できます`)
	expectNotContains(t, newPage.Body, `id="public-design-link"`, `/preview"`)

	draftLoc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("下書きの記事", "draft-design", "draft")), "/admin/articles/")
	draftID := editPathPattern.FindStringSubmatch(draftLoc)[1]
	for name, id := range map[string]string{"draft": draftID, "review": reviewID, "scheduled": scheduledID} {
		res := ed.get("/admin/articles/" + id + "/edit")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `id="public-design-link" href="/admin/articles/`+id+`/preview" target="_blank" rel="noopener"`, `公開デザインで確認 ↗`)
		if strings.Contains(res.Body, `id="public-design-hint"`) {
			t.Errorf("%s: saved article should not show the save hint", name)
		}
	}

	pubLoc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("公開した記事", "published-design", "publish")), "/admin/articles/")
	pub := ed.get("/admin/articles/" + editPathPattern.FindStringSubmatch(pubLoc)[1] + "/edit")
	expectStatus(t, pub, http.StatusOK)
	expectContains(t, pub.Body, `id="public-design-link" href="/articles/published-design" target="_blank" rel="noopener"`, `公開デザインで確認 ↗`)
	expectNotContains(t, pub.Body, `id="public-design-hint"`)
	expectStatus(t, env.newClient().get("/articles/published-design"), http.StatusOK)

	expectStatus(t, ed.get("/admin/set-lang?lang=en&return=/admin"), http.StatusSeeOther)
	en := ed.get("/admin/articles/" + draftID + "/edit")
	expectContains(t, en.Body, `View in site design ↗`)
}

// 翻訳編集画面（+EN, admin_ux_phase5_spec.md §1.5）: 原文バーに「全文をAI翻訳」と表示の2択（原文と比較 / 仕上がりプレビュー）、
// 保存済みなら「公開デザインで確認 ↗」を置く。AI ツールバーは翻訳・推敲・仕上げに絞り、左右のペインの上に1本だけ出す。
// 原文ペインの操作行と本文の枠は、右の装飾ツールバーと編集欄に高さ・余白をそろえる。
func TestArticleEditorTranslationModeLayout(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "翻訳", "translation", "")
		seedArticle(t, db, article.Article{ID: "art-ja-src", Title: "原文の記事", Slug: "source-ja", CategoryID: catID, Excerpt: "原文の抜粋",
			ContentMarkdown: "## はじめに\n\n原文の本文。", Status: article.StatusPublished, Lang: "ja", TranslationGroupID: "grp-tr"})
	})
	enableMultilingual(t, env.db)
	ed := env.loggedIn(chief)

	check := func(t *testing.T, body string) {
		t.Helper()
		banner := body[strings.Index(body, `id="source-article-banner"`):]
		banner = banner[:strings.Index(banner, `id="title"`)]
		expectContains(t, banner, `原文の記事`, `下の「全文をAI翻訳」から始めると`)
		// 「全文をAI翻訳」と表示の2択は原文バーに埋もれさせず、AI ツールバーの先頭に置く（ux_security_docs_final_polish_spec.md §1.3）。
		expectNotContains(t, banner, `id="translate-article-btn"`, `id="translation-view-toggle"`)
		expectNotContains(t, body, `id="toggle-split-btn"`, `partial-translate-open-btn`)
		for _, id := range []string{`id="translate-article-btn"`, `id="translation-view-toggle"`} {
			if n := strings.Count(body, id); n != 1 {
				t.Errorf("%s appears %d times, want 1", id, n)
			}
		}

		// AI ツールバーは1本だけ、左右のペイン（原文・編集欄）の上に置き、「AI で下書き」「翻訳する」は出さない。
		if n := strings.Count(body, `id="ai-toolbar"`); n != 1 {
			t.Fatalf("ai-toolbar appears %d times", n)
		}
		toolbarAt := strings.Index(body, `id="ai-toolbar"`)
		if !(toolbarAt < strings.Index(body, `id="split-source-pane"`) && toolbarAt < strings.Index(body, `id="editor-pane-edit"`)) {
			t.Error("translation mode: ai-toolbar should sit above both panes")
		}
		toolbar := body[toolbarAt:]
		toolbar = toolbar[:strings.Index(toolbar, `src="/static/js/ai.js`)]
		expectContains(t, toolbar, `data-ai-mode="translation"`, `data-ai-partial`, `選択範囲を英語に翻訳・推敲`,
			`id="translate-article-btn" class="btn btn-primary btn-sm`, `全文をAI翻訳`,
			`id="translation-view-toggle" role="group"`, `data-view-mode="compare" aria-pressed="true"`, `data-view-mode="preview" aria-pressed="false"`,
			`>原文と比較</button>`, `>仕上がりプレビュー</button>`,
			`data-ai-action="proofread"`, `data-ai-action="suggest-titles"`, `data-ai-action="summarize"`, `id="social-open-button"`)
		expectNotContains(t, toolbar, `data-ai-action="draft"`, `AI で下書き`, `data-ai-action="translate"`, `指示・トーンを指定して直す`)
		// 並びは「全文をAI翻訳」（主導線・最上段の左端）→「選択範囲を英語に翻訳・推敲」→表示の2択→メニュー→右端の参照・使う AI。
		order := []string{`id="translate-article-btn"`, `data-ai-partial`, `id="translation-view-toggle"`, `<details`, `id="ai-toolbar-side"`, `id="ai-use-rag"`}
		for i := 1; i < len(order); i++ {
			if a, b := strings.Index(toolbar, order[i-1]), strings.Index(toolbar, order[i]); a < 0 || b < 0 || a > b {
				t.Errorf("translation toolbar: %s should come before %s", order[i-1], order[i])
			}
		}
		if first, tr := strings.Index(toolbar, "<button"), strings.Index(toolbar, `<button type="button" id="translate-article-btn"`); first < 0 || first != tr {
			t.Error("translation mode: 全文をAI翻訳 should be the first button of the toolbar")
		}

		// 原文ペイン: 操作行は装飾ツールバーと同じ枠、本文の枠は編集欄と同じ高さ。
		pane := body[strings.Index(body, `id="split-source-pane"`):strings.Index(body, `id="editor-pane-edit"`)]
		expectContains(t, pane, `id="source-pane-toolbar" class="mb-1.5 flex flex-wrap items-center gap-1 rounded-lg border border-neutral-200 bg-neutral-50 px-1 py-1"`,
			`id="source-partial-translate-btn"`, `選択文を翻訳して挿入`, `id="source-scroll-sync" type="checkbox" checked`,
			`id="source-body-scroll"`, `h-[60vh]`, `lg:h-[65vh]`, `id="source-title-display"`, `id="source-excerpt-display"`, `id="source-body-display"`)
		ta := tagWithID(t, body, "content_markdown")
		if !strings.Contains(ta, "h-[60vh]") || !strings.Contains(ta, "lg:h-[65vh]") {
			t.Errorf("editor height changed; keep it aligned with the source pane: %s", ta)
		}
	}

	t.Run("new translation", func(t *testing.T) {
		res := ed.get("/admin/articles/new?source_article_id=art-ja-src&lang=en")
		expectStatus(t, res, http.StatusOK)
		check(t, res.Body)
		// 未保存なので、公開デザインのリンクは出さず案内だけ。
		expectNotContains(t, res.Body, `id="translation-public-design-link"`, `id="public-design-link"`)
		expectContains(t, res.Body, `id="public-design-hint"`)
	})

	t.Run("saved translation", func(t *testing.T) {
		form := articleFormFor("Source article", "source-en", "draft")
		form.Set("lang", "en")
		form.Set("source_article_id", "art-ja-src")
		form.Set("translation_group_id", "grp-tr")
		loc := expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/")
		id := editPathPattern.FindStringSubmatch(loc)[1]
		res := ed.get("/admin/articles/" + id + "/edit")
		expectStatus(t, res, http.StatusOK)
		check(t, res.Body)
		expectContains(t, res.Body, `id="translation-public-design-link" href="/admin/articles/`+id+`/preview" target="_blank" rel="noopener"`)
	})

	t.Run("english UI", func(t *testing.T) {
		expectStatus(t, ed.get("/admin/set-lang?lang=en&return=/admin"), http.StatusSeeOther)
		res := ed.get("/admin/articles/new?source_article_id=art-ja-src&lang=en")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `Translate all with AI`, `>Compare with original</button>`, `>Result preview</button>`,
			`Translate / polish selection`, `Translate selection and insert`, `Sync scrolling`)
	})

	// 多言語対応を OFF にしたら、原文がある記事でも翻訳の画面にはせず、いつもの AI ツールバーを編集欄の上に出す。
	t.Run("multilingual off", func(t *testing.T) {
		setSettingForTest(t, env.db, settingMultilingualEnabled, "0")
		res := ed.get("/admin/articles/new?source_article_id=art-ja-src&lang=en")
		expectStatus(t, res, http.StatusOK)
		expectNotContains(t, res.Body, `id="source-article-banner"`, `id="split-source-pane"`, `data-ai-mode="translation"`)
		expectContains(t, res.Body, `data-ai-action="draft"`, `data-ai-action="translate"`, `Revise with your own instructions…`)
		// 通常の執筆でも翻訳編集と同じく、ツールバーは左右のペインの上に1本だけ置き、右端の参照・使う AI の位置をそろえる。
		if n := strings.Count(res.Body, `id="ai-toolbar"`); n != 1 {
			t.Fatalf("ai-toolbar appears %d times", n)
		}
		if strings.Index(res.Body, `id="ai-toolbar"`) > strings.Index(res.Body, `id="editor-pane-edit"`) {
			t.Error("normal mode: ai-toolbar should sit above the panes like translation mode")
		}
	})
}

// 翻訳の表示切り替え・スクロール連動（editor.js）と、選択範囲の AI 推敲を「文章を整える ▾」から直接開く導線（ai.js）。
func TestEditorScriptTranslationViewAndPartialEntry(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", "static", "js", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	editor := read("editor.js")
	for _, want := range []string{
		"getElementById('translation-view-toggle')", "function setViewMode(", "'aria-pressed'", "classList.toggle('hidden', !compare)",
		"getElementById('source-body-scroll')", "getElementById('source-scroll-sync')", "function syncSourceScroll(", "scrollEcho",
		"[data-ai-partial], #translate-article-btn, #source-partial-translate-btn",
	} {
		if !strings.Contains(editor, want) {
			t.Errorf("editor.js should contain %q", want)
		}
	}
	aiJS := read("ai.js")
	expectContains(t, aiJS, "getElementById('partial-translate-modal')", "window.sorusOpenPartialTranslate('editor')", "[data-ai-partial]")
	expectNotContains(t, aiJS, "partial-translate-open-btn", ".click()")

	tmpl, err := os.ReadFile(filepath.Join("..", "..", "templates", "admin", "article_edit.html"))
	if err != nil {
		t.Fatal(err)
	}
	expectContains(t, string(tmpl), "window.sorusOpenPartialTranslate = function (origin)")
	expectNotContains(t, string(tmpl), "toggle-split-btn", "partial-translate-open-btn")
}

// editor.js のハイライトは本文を HTML エスケープしてから描き、スクロール・大きさ・IME の確定・value の書き換えに追従する。
func TestEditorScriptHighlightAndToolbar(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "static", "js", "editor.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		"function highlightMarkdown(", "function escapeHTML(", "highlightCode.innerHTML = highlightMarkdown(textarea.value)",
		"'&lt;'", "'&amp;'", "addEventListener('scroll', syncHighlightScroll)", "highlightPre.scrollTop = textarea.scrollTop",
		"highlightPre.scrollLeft = textarea.scrollLeft", "new ResizeObserver(syncHighlightGeometry)", "'compositionend'",
		"Object.defineProperty(textarea, 'value'", "classList.add('has-highlight')",
		"getElementById('editor-format-toolbar')", "data-md-format", "execCommand('insertText'", "new Event('input', { bubbles: true })",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("editor.js should contain %q", want)
		}
	}
	// ミラーの innerHTML に入れるのは highlightMarkdown の結果だけ（その中で本文は必ず escapeHTML を通る）。
	if n := strings.Count(src, "highlightCode.innerHTML"); n != 1 {
		t.Errorf("highlightCode.innerHTML should be assigned exactly once, got %d", n)
	}

	css, err := os.ReadFile(filepath.Join("..", "..", "static", "css", "admin.css"))
	if err != nil {
		t.Fatal(err)
	}
	// textarea とミラーは1つの規則で同じ文字・余白・枠線・折り返しにそろえる（カーソルずれを防ぐ）。
	for _, want := range []string{
		".editor-wrap .editor-input, .editor-wrap .editor-highlight {", "white-space: pre-wrap; overflow-wrap: break-word;",
		".editor-wrap.has-highlight .editor-input { background: transparent; color: transparent; caret-color:",
		".editor-wrap:not(.has-highlight) .editor-highlight { display: none; }",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("admin.css should contain %q", want)
		}
	}
}

// AI ツールバーは通常の執筆でも翻訳編集（+EN）でも、左右のペインのすぐ上に1本だけ置き、右端の「自サイトの過去記事を参照する」
// 「使う AI」（#ai-toolbar-side）を同じ枠・同じ高さで出す（ux_security_docs_final_polish_spec.md §1.3）。
// 閲覧専用（他の人が編集中）の翻訳編集では、ツールバーを出さない代わりに表示の2択を原文バーに出す。
func TestAIToolbarSamePlaceInNormalAndTranslationMode(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "位置", "position", "")
		seedArticle(t, db, article.Article{ID: "art-ja-pos", Title: "位置の原文", Slug: "pos-ja", CategoryID: catID,
			ContentMarkdown: "原文", Status: article.StatusPublished, Lang: "ja", TranslationGroupID: "grp-pos"})
	})
	enableMultilingual(t, env.db)
	ed := env.loggedIn(chief)

	layout := func(t *testing.T, body string) (bar, side string) {
		t.Helper()
		if n := strings.Count(body, `id="ai-toolbar"`); n != 1 {
			t.Fatalf("ai-toolbar appears %d times", n)
		}
		at := strings.Index(body, `id="ai-toolbar"`)
		if !(strings.Index(body, `id="featured-image-field"`) < at && at < strings.Index(body, `id="editor-tabs"`)) {
			t.Error("ai-toolbar should sit between the featured image field and the panes")
		}
		bar = strings.Replace(tagWithID(t, body, "ai-toolbar"), ` data-ai-mode="translation"`, "", 1)
		return bar, tagWithID(t, body, "ai-toolbar-side")
	}
	normal := ed.get("/admin/articles/new")
	expectStatus(t, normal, http.StatusOK)
	tr := ed.get("/admin/articles/new?source_article_id=art-ja-pos&lang=en")
	expectStatus(t, tr, http.StatusOK)
	nBar, nSide := layout(t, normal.Body)
	tBar, tSide := layout(t, tr.Body)
	if nBar != tBar || nSide != tSide {
		t.Errorf("toolbar frame differs between modes:\nnormal: %s\n        %s\ntranslation: %s\n             %s", nBar, nSide, tBar, tSide)
	}

	// 閲覧専用の翻訳編集: ツールバーは出さず、表示の2択は原文バーに1つだけ。
	form := articleFormFor("Position", "pos-en", "draft")
	form.Set("lang", "en")
	form.Set("source_article_id", "art-ja-pos")
	form.Set("translation_group_id", "grp-pos")
	id := editPathPattern.FindStringSubmatch(expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/"))[1]
	ed.get("/admin/articles/" + id + "/edit") // 編集長がロックを取る。
	other := env.loggedIn(studentB)
	ro := other.get("/admin/articles/" + id + "/edit")
	expectStatus(t, ro, http.StatusOK)
	expectContains(t, ro.Body, "閲覧専用モード")
	expectNotContains(t, ro.Body, `id="ai-toolbar"`, `id="translate-article-btn"`, `下の「全文をAI翻訳」から始めると`)
	banner := ro.Body[strings.Index(ro.Body, `id="source-article-banner"`):]
	banner = banner[:strings.Index(banner, `id="title"`)]
	expectContains(t, banner, `id="translation-view-toggle" role="group"`, `>原文と比較</button>`)
	if n := strings.Count(ro.Body, `id="translation-view-toggle"`); n != 1 {
		t.Errorf("translation-view-toggle appears %d times in read-only mode", n)
	}
}

// 「公開デザインで確認 ↗」は、/admin/articles/{id}/preview を開く未公開の保存済み記事にだけ data-desktop-preview
// （POST /admin/articles/{id}/preview-open）を付け、デスクトップ版では admin.js が OS の既定のブラウザで開かせる
// （ux_security_docs_final_polish_spec.md §1.2）。公開中の記事は公開ページそのものを開くため付けない。
func TestArticleEditorDesktopPreviewLink(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	expectRedirect(t, c.postForm(SetupPath, setupForm()), "/admin")
	if err := env.db.Get(new(string), `SELECT id FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	enableMultilingual(t, env.db)

	draftID := editPathPattern.FindStringSubmatch(expectRedirect(t, c.postForm("/admin/articles/new", articleFormFor("既定ブラウザ", "default-browser", "draft")), "/admin/articles/"))[1]
	draft := c.get("/admin/articles/" + draftID + "/edit")
	expectStatus(t, draft, http.StatusOK)
	expectContains(t, draft.Body, `data-open-external="/admin/desktop/open-external"`,
		`id="public-design-link" href="/admin/articles/`+draftID+`/preview" target="_blank" rel="noopener" data-desktop-preview="/admin/articles/`+draftID+`/preview-open"`)
	expectNotContains(t, c.get("/admin/articles/new").Body, `data-desktop-preview`)

	// 翻訳編集画面の原文バーの「公開デザインで確認 ↗」にも付ける。
	form := articleFormFor("Default browser", "default-browser-en", "draft")
	form.Set("lang", "en")
	form.Set("source_article_id", draftID)
	trID := editPathPattern.FindStringSubmatch(expectRedirect(t, c.postForm("/admin/articles/new", form), "/admin/articles/"))[1]
	tr := c.get("/admin/articles/" + trID + "/edit")
	expectStatus(t, tr, http.StatusOK)
	expectContains(t, tr.Body, `id="source-article-banner"`,
		`id="translation-public-design-link" href="/admin/articles/`+trID+`/preview" target="_blank" rel="noopener" data-desktop-preview="/admin/articles/`+trID+`/preview-open"`)

	pubID := editPathPattern.FindStringSubmatch(expectRedirect(t, c.postForm("/admin/articles/new", articleFormFor("公開済み", "published-browser", "publish")), "/admin/articles/"))[1]
	pub := c.get("/admin/articles/" + pubID + "/edit")
	expectStatus(t, pub, http.StatusOK)
	expectContains(t, pub.Body, `id="public-design-link" href="/articles/published-browser"`)
	expectNotContains(t, pub.Body, `data-desktop-preview`)

	// admin.js: デスクトップモード（data-open-external）のときだけ、左クリックを横取りして CSRF トークン付きで POST し、
	// 204 で開けなかったときだけ新しいタブで開く。外部リンクの処理より先に登録する。
	js := c.get("/static/js/admin.js")
	expectStatus(t, js, http.StatusOK)
	expectContains(t, js.Body, "a[data-desktop-preview]", "getAttribute('data-desktop-preview')", "document.body.dataset.openExternal",
		"'X-CSRF-Token': csrfToken()", "r.status !== 204", "window.open(href, '_blank', 'noopener')")
	if strings.Index(js.Body, "a[data-desktop-preview]") > strings.Index(js.Body, "var link = externalLink(e);") {
		t.Error("the desktop preview handler should run before the external link handler")
	}
}

// 公開サーバーに執筆者として参加しているとき、記事画面の「公開する」はこの PC の中だけの公開なので、「この PC で公開」と
// 名前を変え、公開サーバーへは「原稿を下書きとして提出」で送ると案内する。編集者として参加しているときは、
// 「公開する」で公開サーバーにも反映すると添える。action="publish" と id はそのまま（ux_security_docs_final_polish_spec.md §1.3）。
func TestArticleEditorLocalPublishVsRemoteServer(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	expectRedirect(t, c.postForm(SetupPath, setupForm()), "/admin")
	var userID string
	if err := env.db.Get(&userID, `SELECT id FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	draftID := seedArticle(t, env.db, article.Article{Title: "私の記事", ContentMarkdown: "本文", Status: article.StatusDraft, AuthorID: userID})
	pubID := seedArticle(t, env.db, article.Article{Title: "公開中の記事", Slug: "local-published", ContentMarkdown: "本文", Status: article.StatusPublished, AuthorID: userID})

	// 公開サーバー未連携: いつもの「公開する」で、案内は出さない。
	plain := c.get("/admin/articles/" + draftID + "/edit").Body
	expectContains(t, tagWithID(t, plain, "publish-button"), `name="action" value="publish"`)
	expectContains(t, plain, ">公開する")
	expectNotContains(t, plain, `id="publish-scope-note"`, "この PC で公開")

	remote := newSyncEnv(t)
	save := func(role auth.Role) {
		cfg := remoteConfig{URL: remote.srv.URL, SyncPath: remote.server.cfg.SyncAPIPath, Token: remote.token("hanako", role)}
		if err := env.server.saveRemoteConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}

	save(auth.RoleWriter)
	body := c.get("/admin/articles/" + draftID + "/edit").Body
	btn := tagWithID(t, body, "publish-button")
	expectContains(t, btn, `name="action" value="publish"`, `aria-describedby="publish-scope-note"`, "公開サーバーのサイトには出ません")
	expectContains(t, body, "この PC で公開", `id="publish-scope-note"`, "この PC の中での公開状態（確認用）",
		"公開サーバーのサイトには出ません。", "「原稿を下書きとして提出」で送り", `data-remote-submit="`+draftID+`"`)
	expectContains(t, c.get("/admin/articles/"+pubID+"/edit").Body, "この PC で更新して公開")
	newPage := c.get("/admin/articles/new").Body
	expectContains(t, newPage, "この PC で公開", "（提出は下書き保存のあとにできます）")

	// 公開ボタンの値は変わらないので、押せば今まで通りこの PC で公開される。
	form := articleFormFor("私の記事", "my-article", "publish")
	expectRedirect(t, c.postForm("/admin/articles/"+draftID, form), "/admin/articles/"+draftID)
	var st string
	if err := env.db.Get(&st, `SELECT status FROM articles WHERE id = ?`, draftID); err != nil {
		t.Fatal(err)
	}
	if st != string(article.StatusPublished) {
		t.Errorf("status after publish = %q, want published", st)
	}

	save(auth.RoleEditor)
	body = c.get("/admin/articles/" + pubID + "/edit").Body
	expectContains(t, body, `id="publish-scope-note"`, "この PC で公開したうえで公開サーバーのサイトにも反映します", "更新して公開")
	expectNotContains(t, body, "この PC で更新して公開", `data-remote-submit=`)

	// 英語の画面。
	save(auth.RoleWriter)
	expectStatus(t, c.get("/admin/set-lang?lang=en&return=/admin"), http.StatusSeeOther)
	en := c.get("/admin/articles/" + pubID + "/edit").Body
	expectContains(t, en, "Update and publish on this PC", "It does not appear on the publish server's site.", "Submit manuscript as draft")
}

// trashedArticle は writer で記事を作って添付資料を1つ付け、ゴミ箱に移す。記事 ID と添付資料の保存先を返す。
func trashedArticle(t *testing.T, env *testEnv, writer *client, title string) (id, attachmentFile string) {
	t.Helper()
	id = writer.createArticle(title, "本文")
	writer.mustAttach(id, "資料.pdf", pdfData)
	var name string
	if err := env.db.Get(&name, `SELECT file_path FROM attachments WHERE article_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	attachmentFile = filepath.Join(env.attachmentDir, name)
	if _, err := os.Stat(attachmentFile); err != nil {
		t.Fatalf("attachment file: %v", err)
	}
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/delete", url.Values{}), "/admin/articles?status=deleted")
	return id, attachmentFile
}

func articleExists(t *testing.T, env *testEnv, id string) bool {
	t.Helper()
	var n int
	if err := env.db.Get(&n, `SELECT COUNT(*) FROM articles WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// ゴミ箱のタブ: 執筆者は「元に戻す」で下書きに戻せる。「完全に削除」「ゴミ箱を空にする」は編集者・管理者だけが行え、
// 確認モーダル（data-confirm）で確かめてから送る。完全に削除すると添付資料のファイルも消える。
func TestTrashRestoreAndPurge(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id, file := trashedArticle(t, env, writer, "ゴミ箱の記事")

	list := writer.get("/admin/articles?status=deleted")
	expectStatus(t, list, http.StatusOK)
	expectContains(t, list.Body, `id="trash-auto-purge-note"`, "ゴミ箱に入れた記事は30日後に自動で完全削除されます",
		`action="/admin/articles/`+id+`/restore"`, `id="confirm-modal"`)
	expectNotContains(t, list.Body, `action="/admin/articles/`+id+`/purge"`, `id="trash-empty-form"`)
	// ゴミ箱以外のタブには出さない。
	expectNotContains(t, writer.get("/admin/articles").Body, `id="trash-auto-purge-note"`)

	// 執筆者は完全に削除できない。
	if res := writer.postForm("/admin/articles/"+id+"/purge", url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("writer purge: status %d, want 403", res.StatusCode)
	}
	if res := writer.postForm("/admin/articles/trash/empty", url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("writer empty trash: status %d, want 403", res.StatusCode)
	}

	// 元に戻すと下書きになる。
	loc := expectRedirect(t, writer.postForm("/admin/articles/"+id+"/restore", url.Values{}), "/admin/articles?status=deleted")
	if !strings.Contains(loc, "trash=restored") {
		t.Errorf("restore redirect = %q", loc)
	}
	expectContains(t, writer.get(loc).Body, `id="trash-result-notice"`, "記事をゴミ箱から下書きに戻しました")
	if a := env.mustGetArticle(id); a.Status != "draft" {
		t.Fatalf("status after restore = %q, want draft", a.Status)
	}
	// ゴミ箱にない記事は戻せない・完全に削除できない（公開中の記事を誤って消さない）。
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/restore", url.Values{}), http.StatusConflict)
	adm := env.loggedIn(admin)
	expectStatus(t, adm.postForm("/admin/articles/"+id+"/purge", url.Values{}), http.StatusConflict)
	if !articleExists(t, env, id) {
		t.Fatal("article outside the trash was deleted")
	}

	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/delete", url.Values{}), "/admin/articles?status=deleted")
	list = adm.get("/admin/articles?status=deleted")
	expectContains(t, list.Body, `action="/admin/articles/`+id+`/purge"`, `data-confirm="「ゴミ箱の記事」を完全に削除します。`,
		`id="trash-empty-form"`, "ゴミ箱にあるすべての記事とページ（1件）")

	loc = expectRedirect(t, adm.postForm("/admin/articles/"+id+"/purge", url.Values{}), "/admin/articles?status=deleted")
	if !strings.Contains(loc, "trash=purged") {
		t.Errorf("purge redirect = %q", loc)
	}
	if articleExists(t, env, id) {
		t.Fatal("article still exists after purge")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("attachment file after purge: %v, want removed", err)
	}
	expectStatus(t, adm.postForm("/admin/articles/"+id+"/purge", url.Values{}), http.StatusNotFound)
}

func TestTrashEmpty(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	a, fileA := trashedArticle(t, env, writer, "ゴミ1")
	b, fileB := trashedArticle(t, env, writer, "ゴミ2")
	keep := writer.createArticle("残す記事", "本文")

	adm := env.loggedIn(chief)
	loc := expectRedirect(t, adm.postForm("/admin/articles/trash/empty", url.Values{}), "/admin/articles?status=deleted")
	if !strings.Contains(loc, "trash=emptied") {
		t.Errorf("empty redirect = %q", loc)
	}
	for _, id := range []string{a, b} {
		if articleExists(t, env, id) {
			t.Errorf("article %s still exists", id)
		}
	}
	for _, f := range []string{fileA, fileB} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("attachment %s: %v, want removed", f, err)
		}
	}
	if !articleExists(t, env, keep) {
		t.Error("article outside the trash was deleted")
	}
	page := adm.get(loc)
	expectContains(t, page.Body, "ゴミ箱を空にしました", "ゴミ箱は空です")
	expectNotContains(t, page.Body, `id="trash-empty-form"`)
}

// スケジューラの自動削除: ゴミ箱に入れてから30日たった記事だけを消し、添付資料のファイルも消す。
func TestTrashAutoPurgeAfter30Days(t *testing.T) {
	env := newTestEnv(t)
	if env.server.scheduler == nil {
		t.Fatal("scheduler is not running")
	}
	writer := env.loggedIn(studentA)
	old, oldFile := trashedArticle(t, env, writer, "古いゴミ")
	fresh, freshFile := trashedArticle(t, env, writer, "新しいゴミ")
	past := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := env.db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, past, old); err != nil {
		t.Fatal(err)
	}

	res, err := env.server.scheduler.PurgeExpiredTrash(context.Background())
	if err != nil {
		t.Fatalf("PurgeExpiredTrash: %v", err)
	}
	if len(res.IDs) != 1 || res.IDs[0] != old {
		t.Fatalf("purged = %v, want [%s]", res.IDs, old)
	}
	if articleExists(t, env, old) {
		t.Error("expired trash article still exists")
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("attachment of the purged article: %v, want removed", err)
	}
	if !articleExists(t, env, fresh) {
		t.Error("recent trash article was purged")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Errorf("attachment of the kept article: %v", err)
	}
}
