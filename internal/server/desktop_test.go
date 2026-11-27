package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/database"
)

// newDesktopEnv はユーザーが1人もいない DB でデスクトップモードのサーバーを起動する。quits は「sorus を終了」で
// Config.OnQuit が呼ばれた回数。
func newDesktopEnv(t *testing.T) (env *testEnv, quits *atomic.Int32) {
	t.Helper()
	env, quits, _ = newDesktopEnvQuits(t)
	return env, quits
}

// newDesktopEnvQuits は newDesktopEnv と同じサーバーを起動し、Config.OnQuit が finishFirst=true（予約をすべて反映して
// から終了する）で呼ばれた回数 finishes も返す。
func newDesktopEnvQuits(t *testing.T) (env *testEnv, quits, finishes *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Connect(filepath.Join(dir, "desktop.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	quits, finishes = new(atomic.Int32), new(atomic.Int32)
	s, err := New(Config{
		DB:                db,
		UploadDir:         filepath.Join(dir, "uploads"),
		AttachmentDir:     filepath.Join(dir, "attachments"),
		Desktop:           true,
		SuggestedUsername: "hanako",
		OnQuit: func(finishFirst bool) {
			quits.Add(1)
			if finishFirst {
				finishes.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return &testEnv{t: t, db: db, srv: srv, server: s, uploadDir: filepath.Join(dir, "uploads"), attachmentDir: filepath.Join(dir, "attachments")}, quits, finishes
}

// setupForm は「はじめに」画面の正しい入力。
func setupForm() url.Values {
	return url.Values{
		"display_name":     {"山田 花子"},
		"username":         {"hanako"},
		"password":         {"password123"},
		"password_confirm": {"password123"},
		"site_name":        {"学生新聞"},
	}
}

// setupCSRF は「はじめに」画面を開いて CSRF トークンを発行させる（ユーザーがいない間はログイン画面も setup へ送られる）。
func (c *client) setupCSRF() {
	c.t.Helper()
	expectStatus(c.t, c.get(SetupPath), http.StatusOK)
	if c.cookie(auth.CSRFCookieName) == "" {
		c.t.Fatal("CSRF cookie was not issued on the setup page")
	}
}

// completeSetup は「はじめに」を済ませ、ログインしたクライアントを返す。
func (e *testEnv) completeSetup() *client {
	e.t.Helper()
	c := e.newClient()
	c.setupCSRF()
	expectRedirect(e.t, c.postForm(SetupPath, setupForm()), "/admin")
	return c
}

func TestDesktopRedirectsToSetupWhenNoUsers(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	for _, path := range []string{"/admin", "/admin/", "/admin/login", "/admin/articles", "/admin/settings/general", "/admin/no-such-page"} {
		loc := expectRedirect(t, c.get(path), SetupPath)
		if loc != SetupPath {
			t.Errorf("GET %s: Location = %q, want %q", path, loc, SetupPath)
		}
	}
	// フォーム送信（ログインの試行など）も「はじめに」へ送る。
	c.setupCSRF()
	expectRedirect(t, c.postForm("/admin/login", url.Values{"username": {"admin"}, "password": {"x"}}), SetupPath)

	res := c.get(SetupPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `name="display_name"`, `name="username"`, `value="hanako"`, `name="password"`,
		`name="password_confirm"`, `name="site_name"`)
	// 公開ページとヘルスチェックはそのまま。
	expectStatus(t, c.get("/healthz"), http.StatusOK)
}

func TestDesktopSetupSuggestedUsernameMustBeValid(t *testing.T) {
	env, _ := newDesktopEnv(t)
	env.server.cfg.SuggestedUsername = "Hanako Yamada"
	res := env.newClient().get(SetupPath)
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, `value="Hanako Yamada"`) {
		t.Error("an invalid OS username was prefilled")
	}
}

func TestDesktopSetupCreatesUserAndLogsIn(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	if c.cookie(auth.SessionCookieName) == "" {
		t.Fatal("setup did not log the user in")
	}

	var u auth.User
	if err := env.db.Get(&u, `SELECT id, username, display_name, password_hash, role, created_at FROM users`); err != nil {
		t.Fatal(err)
	}
	if u.Username != "hanako" || u.DisplayName != "山田 花子" || u.Role != auth.RoleAdmin || !auth.CheckPassword("password123", u.PasswordHash) {
		t.Errorf("user = %+v", u)
	}
	if got := env.server.getSetting("site_name", ""); got != "学生新聞" {
		t.Errorf("site_name = %q", got)
	}

	res := c.get("/admin")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "学生新聞", "山田 花子", `action="/admin/desktop/quit"`, "sorus を終了",
		`href="/admin/settings"`)
	expectNotContainsAny(t, "desktop admin header", res.Body, `id="nav-new-article"`)
	// デスクトップモードでも、管理者はユーザー管理へ遷移できる。
	hub := c.get("/admin/settings").Body
	expectContains(t, hub, `href="/admin/settings/general"`, `href="/admin/users"`)
	expectStatus(t, c.get("/admin/users"), http.StatusOK)

	// 「はじめに」は済んだので、画面を開くと管理画面へ送る。
	expectRedirect(t, c.get(SetupPath), "/admin")
	// ログアウト後はログイン画面が使える。
	expectRedirect(t, c.postForm("/admin/logout", nil), "/admin/login")
	other := env.newClient()
	expectStatus(t, other.get("/admin/login"), http.StatusOK)
	expectRedirect(t, other.login("hanako", "password123"), "/admin/articles")
}

func TestDesktopSetupStarterChoices(t *testing.T) {
	for _, starter := range []string{"blog", "docs", "site", "empty"} {
		t.Run(starter, func(t *testing.T) {
			env, _ := newDesktopEnv(t)
			c := env.newClient()
			c.setupCSRF()
			form := setupForm()
			form.Set("site_starter", starter)
			expectRedirect(t, c.postForm(SetupPath, form), "/admin")
			pages := env.countRows(`SELECT COUNT(*) FROM articles WHERE kind='page'`)
			wantPages := 0
			if starter == "docs" {
				wantPages = 2
			}
			if starter == "site" {
				wantPages = 2
			}
			if pages != wantPages {
				t.Errorf("pages = %d, want %d", pages, wantPages)
			}
			var sections string
			if err := env.db.Get(&sections, `SELECT value FROM settings WHERE key='front_sections'`); err != nil {
				t.Fatal(err)
			}
			if starter == "empty" && sections != "[]" {
				t.Errorf("empty front_sections = %s", sections)
			}
			if starter == "docs" && env.server.getSetting("theme", "") != "tech" {
				t.Errorf("docs theme = %q", env.server.getSetting("theme", ""))
			}
			if starter == "site" && env.server.getSetting("theme", "") != "portal" {
				t.Errorf("site theme = %q", env.server.getSetting("theme", ""))
			}
			if starter == "docs" || starter == "site" {
				wantFront := map[string]string{"docs": "guide", "site": "about"}[starter]
				if got := env.server.getSetting(settingFrontPage, ""); got != wantFront {
					t.Errorf("front_page = %q, want %q", got, wantFront)
				}
				if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE kind='page' AND status='published' AND published_at IS NOT NULL`); n != wantPages {
					t.Errorf("published pages = %d, want %d", n, wantPages)
				}
				res := c.get("/")
				expectStatus(t, res, http.StatusOK)
				if starter == "docs" {
					expectContains(t, res.Body, "このサイトの使い方をまとめます", "ガイド", `class="front-cards"`, `href="/installation"`, "インストール", "導入手順をここに記載します")
				} else {
					expectContains(t, res.Body, "活動や作品をご紹介します", "お問い合わせ", `class="front-cards"`, `href="/contact"`)
				}
			}
		})
	}
}

// 「はじめに」では立場（自分で公開する／原稿を書いて渡す）を尋ねない。名前・パスワードとサイト名だけで始められる。
func TestDesktopSetupHasNoIntentQuestion(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	page := c.get(SetupPath).Body
	expectNotContainsAny(t, "setup page", page, `name="intent"`, "あなたの立場", "自分で公開する", "原稿を書いて誰かに渡す")
	expectContains(t, page, "2つのことを決めると")

	expectRedirect(t, c.postForm(SetupPath, setupForm()), "/admin")
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key = 'desktop_intent'`); n != 0 {
		t.Errorf("desktop_intent rows = %d, want 0", n)
	}
}

func TestDesktopSetupValidation(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	tests := []struct {
		field, value, want string
	}{
		{"display_name", " ", "表示名"},
		{"username", "山田", "ユーザー名"},
		{"password", "short", "8文字以上"},
		{"password_confirm", "password124", "一致しません"},
		{"site_name", "", "サイト名"},
	}
	for _, tt := range tests {
		form := setupForm()
		form.Set(tt.field, tt.value)
		res := c.postForm(SetupPath, form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, tt.want, `id="setup-errors"`)
		// 入力した内容は残す（パスワードは除く）。
		if tt.field != "display_name" {
			expectContains(t, res.Body, `value="山田 花子"`)
		}
		if strings.Contains(res.Body, "password123") {
			t.Errorf("%s: the password was echoed back", tt.field)
		}
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users`); n != 0 {
		t.Errorf("users = %d after invalid input, want 0", n)
	}
}

func TestDesktopSetupRequiresCSRF(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	expectStatus(t, c.postRaw(SetupPath, setupForm()), http.StatusForbidden)
	form := setupForm()
	form.Set(CSRFFormField, "forged")
	expectStatus(t, c.postRaw(SetupPath, form), http.StatusForbidden)
	if n := env.countRows(`SELECT COUNT(*) FROM users`); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

// ユーザーがいれば「はじめに」をやり直して別の管理者を作ることはできない。
func TestDesktopSetupRejectedWhenUserExists(t *testing.T) {
	env, _ := newDesktopEnv(t)
	env.completeSetup()

	attacker := env.newClient()
	expectStatus(t, attacker.get("/admin/login"), http.StatusOK)
	form := setupForm()
	form.Set("username", "mallory")
	res := attacker.postForm(SetupPath, form)
	expectStatus(t, res, http.StatusForbidden)
	expectContains(t, res.Body, "すでに済んでいます")
	if attacker.cookie(auth.SessionCookieName) != "" {
		t.Error("a rejected setup issued a session")
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users`); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE username = 'mallory'`); n != 0 {
		t.Error("the second setup created a user")
	}
	expectRedirect(t, attacker.get(SetupPath), "/admin")
}

// 古い版の取り込みで著者のユーザー（ログインできないライター）だけが作られ、管理者がいないデスクトップでも、
// 「はじめに」を出して利用者のアカウント（管理者）を作れる。作ったあとは、ほかの人が2人目の管理者を作れない。
func TestDesktopSetupWhenOnlyNonAdminUsersExist(t *testing.T) {
	env, _ := newDesktopEnv(t)
	for _, name := range []string{"taro", "admin"} {
		if _, err := env.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, '', 'writer')`,
			"w-"+name, name, name); err != nil {
			t.Fatal(err)
		}
	}

	c := env.newClient()
	for _, path := range []string{"/admin", "/admin/login", "/admin/articles"} {
		expectRedirect(t, c.get(path), SetupPath)
	}
	c.setupCSRF()

	// 取り込みで作られたユーザーと同じユーザー名は、分かる言葉で断る。
	form := setupForm()
	form.Set("username", "admin")
	res := c.postForm(SetupPath, form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, `id="setup-errors"`, "このユーザー名はすでに使われています")
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE role = 'admin'`); n != 0 {
		t.Errorf("admins = %d after a refused setup, want 0", n)
	}

	expectRedirect(t, c.postForm(SetupPath, setupForm()), "/admin")
	if c.cookie(auth.SessionCookieName) == "" {
		t.Fatal("setup did not log the user in")
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE username = 'hanako' AND role = 'admin'`); n != 1 {
		t.Errorf("admin hanako rows = %d, want 1", n)
	}
	expectRedirect(t, c.get(SetupPath), "/admin")
	expectStatus(t, c.get("/admin/articles"), http.StatusOK)

	// 管理者ができたあとは、ログインしていない人が「はじめに」から2人目の管理者を作れない。
	attacker := env.newClient()
	expectStatus(t, attacker.get("/admin/login"), http.StatusOK)
	form = setupForm()
	form.Set("username", "mallory")
	res = attacker.postForm(SetupPath, form)
	expectStatus(t, res, http.StatusForbidden)
	expectContains(t, res.Body, "すでに済んでいます")
	if attacker.cookie(auth.SessionCookieName) != "" {
		t.Error("a rejected setup issued a session")
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE role = 'admin'`); n != 1 {
		t.Errorf("admins = %d, want 1", n)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users`); n != 3 {
		t.Errorf("users = %d, want 3 (2 writers and the account)", n)
	}
}

// デスクトップモードの1人の利用者は、保存上の役割が admin のまま、記事を書いて校閲なしで公開できる。
func TestDesktopUserCanWriteAndPublish(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()

	list := c.get("/admin/articles")
	expectStatus(t, list, http.StatusOK)
	expectContains(t, list.Body, `id="new-article-button"`)
	expectStatus(t, c.get("/admin/articles/new"), http.StatusOK)

	id := c.createArticle("ひとりで書く記事", "本文です。")
	form := url.Values{"title": {"ひとりで書く記事"}, "category_id": {"1"}, "content_markdown": {"本文です。"}, "action": {"publish"}}
	expectRedirect(t, c.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=published")
	a := env.mustGetArticle(id)
	if a.Status != article.StatusPublished {
		t.Fatalf("status = %s, want published", a.Status)
	}
	var role auth.Role
	if err := env.db.Get(&role, `SELECT role FROM users`); err != nil || role != auth.RoleAdmin {
		t.Errorf("stored role = %q (err %v), want admin", role, err)
	}
	// 一覧の行は編集画面へのリンクになり、公開中の記事もゴミ箱へ移せる。
	list = c.get("/admin/articles?status=published")
	expectContains(t, list.Body, `href="/admin/articles/`+id+`/edit"`, `action="/admin/articles/`+id+`/delete"`)
	// 編集者だけの操作（差し戻し・承認）と管理者だけの操作（サイト設定）の両方ができる。
	expectStatus(t, c.get("/admin/settings/general"), http.StatusOK)
	res := c.postForm("/admin/articles/"+id+"/reject-review", url.Values{"comment": {"確認"}})
	if res.StatusCode == http.StatusForbidden {
		t.Errorf("reject-review: 403 for the desktop user")
	}
}

// サーバーモードの管理者も、編集者・ライターと同じく記事を書いて、編集し、公開できる。
// 編集者・ライターの権限は変わらない（管理者の画面は使えず、ライターは承認できない）。
func TestServerAdminCanWriteAndPublish(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	c := env.loggedIn(admin)
	expectStatus(t, c.get("/admin/articles/new"), http.StatusOK)
	expectContains(t, c.get("/admin/articles").Body, `id="new-article-button"`)

	id := c.createArticle("管理者の記事", "本文です。")
	expectStatus(t, c.get("/admin/articles/"+id+"/edit"), http.StatusOK)
	form := url.Values{"title": {"管理者の記事"}, "category_id": {"1"}, "content_markdown": {"本文です。"}, "action": {"publish"}}
	expectRedirect(t, c.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if a := env.mustGetArticle(id); a.Status != article.StatusPublished {
		t.Fatalf("status = %s, want published", a.Status)
	}
	// 公開中の記事も、管理者が直せる。
	form.Set("content_markdown", "直した本文です。")
	form.Set("action", "save")
	expectRedirect(t, c.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if a := env.mustGetArticle(id); a.ContentMarkdown != "直した本文です。" || a.Status != article.StatusPublished {
		t.Errorf("article after admin edit = %q (%s)", a.ContentMarkdown, a.Status)
	}
	list := c.get("/admin/articles?status=published")
	expectContains(t, list.Body, `href="/admin/articles/`+id+`/edit"`)
	// サーバーモードでは「sorus を終了」は出さない。
	expectNotContainsAny(t, "server-mode admin header", c.get("/admin").Body, "sorus を終了")

	// 編集者は公開できるが、管理者の画面は使えない。ライターは承認できず、管理者の画面も使えない。
	expectStatus(t, env.loggedIn(chief).get("/admin/settings/general"), http.StatusForbidden)
	writer := env.loggedIn(studentA)
	expectStatus(t, writer.get("/admin/settings/general"), http.StatusForbidden)
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/approve-publish", nil), http.StatusForbidden)
}

// デスクトップモードでも、役割は保存上の役割で判定する。ライターのユーザーはライターの権限だけを持つ。
func TestDesktopWriterHasOnlyWriterRole(t *testing.T) {
	env, _ := newDesktopEnv(t)
	owner := env.completeSetup()
	id := owner.createArticle("管理者の記事", "本文です。")

	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w-1', 'taro', '太郎', ?, 'writer')`, string(hash)); err != nil {
		t.Fatal(err)
	}
	w := env.newClient()
	expectRedirect(t, w.login("taro", testPassword), "/admin/articles")

	// 記事は書ける。
	expectStatus(t, w.get("/admin/articles/new"), http.StatusOK)
	w.createArticle("ライターの記事", "本文です。")
	// 管理者・編集者の操作はできない。
	for _, path := range []string{"/admin/settings/general", "/admin/users", "/admin/import", "/admin/manuscripts"} {
		expectStatus(t, w.get(path), http.StatusForbidden)
	}
	expectStatus(t, w.postForm("/admin/articles/"+id+"/approve-publish", nil), http.StatusForbidden)
	expectStatus(t, w.postForm(importPreviewPath, nil), http.StatusForbidden)
	home := w.get("/admin").Body
	expectNotContainsAny(t, "desktop writer header", home, `id="nav-manuscripts"`, `data-publish-panel=`)
	// 「sorus を終了」と更新のお知らせは、役割ではなくデスクトップモードかで出す。
	expectContains(t, home, "sorus を終了", `data-desktop-part="/admin/update/banner"`)
}

func TestDesktopQuit(t *testing.T) {
	env, quits := newDesktopEnv(t)
	c := env.completeSetup()

	expectStatus(t, c.postRaw(DesktopQuitPath, nil), http.StatusForbidden)
	if quits.Load() != 0 {
		t.Fatal("quit without a CSRF token stopped sorus")
	}
	// ログインしていない人は終了できない。
	expectRedirect(t, env.newClient().get("/admin"), "/admin/login")

	res := c.postForm(DesktopQuitPath, nil)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "終了しました。このタブは閉じてかまいません。")
	if quits.Load() != 1 {
		t.Errorf("OnQuit called %d times, want 1", quits.Load())
	}
}

// stubDesktopOpenBrowser は desktopOpenBrowser を、渡された URL を記録して err を返す関数に差し替える。
func stubDesktopOpenBrowser(t *testing.T, err error) *[]string {
	t.Helper()
	var opened []string
	orig := desktopOpenBrowser
	desktopOpenBrowser = func(u string) error {
		opened = append(opened, u)
		return err
	}
	t.Cleanup(func() { desktopOpenBrowser = orig })
	return &opened
}

func TestDesktopOpenExternal(t *testing.T) {
	opened := stubDesktopOpenBrowser(t, nil)
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	host := strings.TrimPrefix(env.srv.URL, "http://")

	// JSON（admin.js の送り方）。
	res := c.postAPI(DesktopOpenExternalPath, "application/json", strings.NewReader(`{"url":"https://example.com/docs?q=1&x=2"}`))
	expectStatus(t, res, http.StatusNoContent)
	// フォーム。相対パスは、このサーバーのページとして開く。
	expectStatus(t, c.postForm(DesktopOpenExternalPath, url.Values{"url": {"/articles/hello"}}), http.StatusNoContent)

	want := []string{"https://example.com/docs?q=1&x=2", "http://" + host + "/articles/hello"}
	if len(*opened) != len(want) {
		t.Fatalf("opened %q, want %q", *opened, want)
	}
	for i := range want {
		if (*opened)[i] != want[i] {
			t.Errorf("opened[%d] = %q, want %q", i, (*opened)[i], want[i])
		}
	}
}

func TestDesktopOpenExternalRejectsUnsafeURLs(t *testing.T) {
	opened := stubDesktopOpenBrowser(t, nil)
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()

	for _, raw := range []string{
		"", "   ", "javascript:alert(1)", "JavaScript:alert(1)", "file:///etc/passwd", "data:text/html,<b>x</b>",
		"//evil.example/path", "/\\evil.example/path", "mailto:a@example.com", "https://", "articles/hello",
	} {
		expectStatus(t, c.postForm(DesktopOpenExternalPath, url.Values{"url": {raw}}), http.StatusBadRequest)
	}
	expectStatus(t, c.postAPI(DesktopOpenExternalPath, "application/json", strings.NewReader(`{"url":"javascript:alert(1)"}`)), http.StatusBadRequest)
	expectStatus(t, c.postAPI(DesktopOpenExternalPath, "application/json", strings.NewReader(`{`)), http.StatusBadRequest)
	if len(*opened) != 0 {
		t.Errorf("unsafe URLs were opened: %q", *opened)
	}
}

func TestDesktopOpenExternalRequiresCSRFAndLogin(t *testing.T) {
	opened := stubDesktopOpenBrowser(t, nil)
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()

	expectStatus(t, c.postRaw(DesktopOpenExternalPath, url.Values{"url": {"https://example.com/"}}), http.StatusForbidden)
	anon := env.newClient()
	res := anon.postForm(DesktopOpenExternalPath, url.Values{"url": {"https://example.com/"}})
	if res.StatusCode == http.StatusNoContent {
		t.Errorf("a visitor who is not logged in opened a link (status %d)", res.StatusCode)
	}
	if len(*opened) != 0 {
		t.Errorf("opened %q without CSRF token or login", *opened)
	}
}

func TestDesktopOpenExternalBrowserFailure(t *testing.T) {
	stubDesktopOpenBrowser(t, errors.New("no browser"))
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	expectStatus(t, c.postForm(DesktopOpenExternalPath, url.Values{"url": {"https://example.com/"}}), http.StatusInternalServerError)
}

func TestDesktopLayoutMarksOpenExternal(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	res := c.get("/admin")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `data-open-external="/admin/desktop/open-external"`)

	server := newTestEnv(t).loggedIn(admin).get("/admin")
	expectStatus(t, server, http.StatusOK)
	expectNotContainsAny(t, "server-mode layout", server.Body, `data-open-external`)
}

// 左上のサイトの切り替え（同じ PC の別のポートで動くほかのサイトの管理画面）は、OS の既定のブラウザに逃がさず、
// アプリのウィンドウの中で開く（admin_ux_phase5_spec.md 1.1）。メニューのリンクに data-no-external-intercept を付け、
// admin.js はその印のリンクと、ホスト名が同じ /admin のリンクを open-external に送らない。
func TestDesktopSiteSwitchStaysInAppWindow(t *testing.T) {
	createdAt := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)
	_, c := newSitesDesktopClient(t, &fakeSites{sites: []SiteInfo{
		{ID: "site1", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true},
		{ID: "site2", Name: "研究室", AdminURL: "http://127.0.0.1:8781/admin", Created: createdAt},
	}}, "site1")
	body := c.get("/admin").Body
	expectContains(t, body, `data-open-external="/admin/desktop/open-external"`)
	_, rest, ok := strings.Cut(body, `<details id="site-menu"`)
	if !ok {
		t.Fatal("site menu not found with 2 sites")
	}
	menu, _, _ := strings.Cut(rest, "</details>")
	for _, href := range []string{"http://127.0.0.1:8780/admin", "http://127.0.0.1:8781/admin"} {
		expectContains(t, menu, `<a href="`+href+`" data-no-external-intercept `)
	}

	js := c.get("/static/js/admin.js")
	expectStatus(t, js, http.StatusOK)
	_, fn, ok := strings.Cut(js.Body, "function externalLink(e) {")
	if !ok {
		t.Fatal("externalLink not found in admin.js")
	}
	fn, _, _ = strings.Cut(fn, "\n  }\n")
	expectContains(t, fn,
		"a.hasAttribute('data-no-external-intercept')",
		"if (a.hostname === window.location.hostname && /^\\/admin(\\/|$)/.test(a.pathname)) return null;",
	)
}

func TestDesktopRoutesAbsentInServerMode(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	expectStatus(t, c.postForm(DesktopQuitPath, nil), http.StatusNotFound)
	opened := stubDesktopOpenBrowser(t, nil)
	expectStatus(t, c.postForm(DesktopOpenExternalPath, url.Values{"url": {"https://example.com/"}}), http.StatusNotFound)
	if len(*opened) != 0 {
		t.Errorf("server mode opened %q", *opened)
	}
	expectStatus(t, c.get(SetupPath), http.StatusNotFound)
	expectStatus(t, c.postForm(SetupPath, setupForm()), http.StatusNotFound)
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE username = 'hanako'`); n != 0 {
		t.Error("setup created a user in server mode")
	}
}

func TestDesktopSessionIsLongLived(t *testing.T) {
	env, _ := newDesktopEnv(t)
	if env.server.cfg.SessionDuration != DesktopSessionDuration {
		t.Errorf("SessionDuration = %v, want %v", env.server.cfg.SessionDuration, DesktopSessionDuration)
	}
	if server := newTestEnv(t).server; server.cfg.SessionDuration != DefaultSessionDuration {
		t.Errorf("server-mode SessionDuration = %v, want %v", server.cfg.SessionDuration, DefaultSessionDuration)
	}
}
