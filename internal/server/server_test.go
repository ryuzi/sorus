package server

import (
	"bytes"
	"context"
	"encoding/json"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/database"
)

const testPassword = "correct-horse"

// testUser はテスト用に登録するユーザー。
type testUser struct {
	ID, Username, DisplayName string
	Role                      auth.Role
}

var (
	studentA = testUser{"user-a", "yamada", "山田", auth.RoleWriter}
	studentB = testUser{"user-b", "suzuki", "鈴木", auth.RoleWriter}
	admin    = testUser{"admin-1", "admin", "編集長", auth.RoleAdmin}
	// chief は公開・承認ができる編集者（Phase 6 の権限マトリクスの Editor）。
	chief = testUser{"editor-1", "sato", "佐藤デスク", auth.RoleEditor}
)

type testEnv struct {
	t             *testing.T
	db            *sqlx.DB
	srv           *httptest.Server
	server        *Server
	uploadDir     string
	attachmentDir string
}

// newTestEnv はファイルDB・一時アップロード先・テストユーザーを用意してサーバーを起動する。
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, Config{}, nil)
}

// newTestEnvWith は cfg（DB・UploadDir 以外）でサーバーを起動する。seed はサーバー起動前に呼ぶ
// （起動時の検索インデックス同期の対象にしたいデータを入れる）。
func newTestEnvWith(t *testing.T, cfg Config, seed func(db *sqlx.DB)) *testEnv {
	t.Helper()
	return startTestEnv(t, cfg, seed, false)
}

// startTestEnv は newTestEnvWith と同じくサーバーを起動する。useTLS なら、本番の公開サーバー（AUTOCERT_DOMAIN）と
// 同じく sorus が TLS を直接受けるサーバー（httptest の TLS サーバー。証明書は 127.0.0.1 のもの）で起動する。
// 閲覧制限の変更（set_site_access）は TLS で直接届いた接続でしか受け付けないため、それを使う試験で使う。
func startTestEnv(t *testing.T, cfg Config, seed func(db *sqlx.DB), useTLS bool) *testEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Connect(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// 実運用の DefaultCost はテストを遅くするだけなので最小コストでハッシュする。
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	for _, u := range []testUser{studentA, studentB, admin, chief} {
		if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, ?, ?)`,
			u.ID, u.Username, u.DisplayName, string(hash), u.Role); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}

	if seed != nil {
		seed(db)
	}

	uploadDir := filepath.Join(dir, "uploads")
	cfg.DB, cfg.UploadDir = db, uploadDir
	if cfg.AttachmentDir == "" {
		cfg.AttachmentDir = filepath.Join(dir, "attachments")
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(s.Handler())
	} else {
		srv = httptest.NewServer(s.Handler())
	}
	t.Cleanup(srv.Close)
	// DB を閉じる前に予約投稿ワーカーを止める（Cleanup は登録の逆順に実行される）。
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return &testEnv{t: t, db: db, srv: srv, server: s, uploadDir: uploadDir, attachmentDir: cfg.AttachmentDir}
}

// client はリダイレクトを追わず Cookie を保持する、1人のブラウザに相当する HTTP クライアント。
type client struct {
	t    *testing.T
	env  *testEnv
	http *http.Client
}

// newClient はサーバーに接続するクライアントを返す。TLS で起動したサーバー（startTestEnv）なら、その証明書を信頼する。
func (e *testEnv) newClient() *client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	c := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if e.srv.TLS != nil {
		c.Transport = e.srv.Client().Transport
	}
	return &client{t: e.t, env: e, http: c}
}

// loggedIn は u でログインしたクライアントを返す。
func (e *testEnv) loggedIn(u testUser) *client {
	c := e.newClient()
	res := c.login(u.Username, testPassword)
	if res.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login %s: status %d, want 303", u.Username, res.StatusCode)
	}
	return c
}

// response は本文を読み終えたレスポンス。
type response struct {
	*http.Response
	Body string
}

func (c *client) do(req *http.Request) *response {
	c.t.Helper()
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return &response{Response: res, Body: string(b)}
}

// get はブラウザの画面遷移として（Accept: text/html で）GET する。
func (c *client) get(path string) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return c.do(req)
}

// csrfToken は Cookie に保存された CSRF トークンを返す。無ければログイン画面を開いて発行させる。
func (c *client) csrfToken() string {
	c.t.Helper()
	if tok := c.cookie(auth.CSRFCookieName); tok != "" {
		return tok
	}
	c.get("/admin/login")
	tok := c.cookie(auth.CSRFCookieName)
	if tok == "" {
		c.t.Fatal("CSRF cookie was not issued")
	}
	return tok
}

func (c *client) cookie(name string) string {
	u, _ := url.Parse(c.env.srv.URL + "/admin/")
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// cloneValues は url.Values のディープコピーを返す。
func cloneValues(v url.Values) url.Values {
	if v == nil {
		return url.Values{}
	}
	dup := make(url.Values, len(v))
	for k, vals := range v {
		dup[k] = append([]string(nil), vals...)
	}
	return dup
}

// postForm はフォーム送信を行う。CSRF トークンは _csrf フィールドで自動的に付ける。
func (c *client) postForm(path string, form url.Values) *response {
	c.t.Helper()
	form = cloneValues(form)
	if !form.Has(CSRFFormField) {
		form.Set(CSRFFormField, c.csrfToken())
	}
	return c.postRaw(path, form)
}

// postRaw は form をそのまま送る（CSRF トークンを付けない）。
func (c *client) postRaw(path string, form url.Values) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	return c.do(req)
}

// postAPI は fetch と同様に X-CSRF-Token ヘッダー付きで POST する。
func (c *client) postAPI(path, contentType string, body io.Reader) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(CSRFHeaderName, c.csrfToken())
	return c.do(req)
}

func (c *client) login(username, password string) *response {
	c.t.Helper()
	return c.postForm("/admin/login", url.Values{"username": {username}, "password": {password}})
}

func expectStatus(t *testing.T, res *response, want int) {
	t.Helper()
	if res.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d\nbody: %.2000s", res.Request.Method, res.Request.URL.Path, res.StatusCode, want, res.Body)
	}
}

func expectRedirect(t *testing.T, res *response, wantPrefix string) string {
	t.Helper()
	expectStatus(t, res, http.StatusSeeOther)
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, wantPrefix) {
		t.Fatalf("Location = %q, want prefix %q", loc, wantPrefix)
	}
	return loc
}

func expectContains(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("body does not contain %q\nbody: %.3000s", w, body)
		}
	}
}

func (e *testEnv) mustGetArticle(id string) *article.Article {
	e.t.Helper()
	a, err := article.NewService(e.db).GetByID(id)
	if err != nil {
		e.t.Fatalf("GetByID(%q): %v", id, err)
	}
	return a
}

func (e *testEnv) countRows(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Get(&n, query, args...); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

var editPathPattern = regexp.MustCompile(`^/admin/articles/([0-9a-f]+)/edit`)

// createArticle は c で記事を新規作成し、その ID を返す。
func (c *client) createArticle(title, body string) string {
	c.t.Helper()
	res := c.postForm("/admin/articles/new", url.Values{
		"title":            {title},
		"category_id":      {"1"},
		"content_markdown": {body},
		"action":           {"draft"},
	})
	loc := expectRedirect(c.t, res, "/admin/articles/")
	m := editPathPattern.FindStringSubmatch(loc)
	if m == nil {
		c.t.Fatalf("unexpected redirect %q", loc)
	}
	return m[1]
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	for _, path := range []string{"/admin/articles", "/admin/articles/new", "/admin"} {
		res := c.get(path)
		expectRedirect(t, res, "/admin/login")
	}
	// API への未認証リクエストはリダイレクトせず 401 を返す。
	res := c.postAPI("/admin/preview", "application/x-www-form-urlencoded", strings.NewReader("content_markdown=x"))
	expectStatus(t, res, http.StatusUnauthorized)
}

func TestLoginLogoutFlow(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	res := c.get("/admin/login")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `name="csrf-token"`, `name="_csrf"`, "ログイン")

	res = c.login(studentA.Username, "wrong-password")
	expectStatus(t, res, http.StatusUnauthorized)
	expectContains(t, res.Body, "ユーザー名またはパスワードが違います")
	if c.cookie(auth.SessionCookieName) != "" {
		t.Fatal("session cookie issued for wrong password")
	}
	res = c.login("nobody", testPassword)
	expectStatus(t, res, http.StatusUnauthorized)

	res = c.login(studentA.Username, testPassword)
	expectRedirect(t, res, "/admin/articles")
	session := c.cookie(auth.SessionCookieName)
	if session == "" {
		t.Fatal("session cookie not issued")
	}
	if n := env.countRows(`SELECT COUNT(*) FROM sessions WHERE id = ? AND user_id = ?`, session, studentA.ID); n != 1 {
		t.Fatalf("sessions for user = %d, want 1", n)
	}

	res = c.get("/admin/articles")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "記事一覧", "山田 さん")

	// ログイン済みでログイン画面を開くと一覧へ送られる。
	expectRedirect(t, c.get("/admin/login"), "/admin/articles")

	res = c.postForm("/admin/logout", nil)
	expectRedirect(t, res, "/admin/login")
	if n := env.countRows(`SELECT COUNT(*) FROM sessions WHERE id = ?`, session); n != 0 {
		t.Fatalf("session not deleted on logout")
	}
	expectRedirect(t, c.get("/admin/articles"), "/admin/login")
}

func TestCSRFProtection(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	form := url.Values{"title": {"CSRF"}, "category_id": {"1"}, "content_markdown": {"x"}}

	// トークン欠落・不正なトークンはどちらも 403。
	expectStatus(t, c.postRaw("/admin/articles/new", form), http.StatusForbidden)
	bad := url.Values{CSRFFormField: {"forged-token"}}
	for k, v := range form {
		bad[k] = v
	}
	expectStatus(t, c.postRaw("/admin/articles/new", bad), http.StatusForbidden)
	if n := env.countRows(`SELECT COUNT(*) FROM articles`); n != 0 {
		t.Fatalf("articles = %d after rejected requests, want 0", n)
	}

	// ヘッダーのトークンが不正な API リクエストも 403。
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/admin/preview", strings.NewReader("content_markdown=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(CSRFHeaderName, "forged-token")
	expectStatus(t, c.do(req), http.StatusForbidden)

	// ログイン・ログアウトも CSRF 保護の対象。
	anon := env.newClient()
	expectStatus(t, anon.postRaw("/admin/login", url.Values{"username": {studentA.Username}, "password": {testPassword}}), http.StatusForbidden)
	expectStatus(t, c.postRaw("/admin/logout", nil), http.StatusForbidden)

	// 正しいトークンなら通る。
	c.createArticle("CSRF OK", "body")
}

func TestArticleCRUD(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)

	res := c.get("/admin/articles/new")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="content_markdown"`, `id="preview"`, "未分類")

	// タイトル未入力は 422 で入力を保持して返す。
	res = c.postForm("/admin/articles/new", url.Values{"title": {" "}, "category_id": {"1"}, "content_markdown": {"本文を残す"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "タイトルを入力してください", "本文を残す")

	id := c.createArticle("はじめての記事", "# 見出し\n\n本文です。<script>alert(1)</script>")
	a := env.mustGetArticle(id)
	if a.Title != "はじめての記事" || a.AuthorID != studentA.ID || a.Status != article.StatusDraft || a.CategoryID != 1 {
		t.Fatalf("created article = %+v", a)
	}
	if !strings.Contains(a.ContentHTML, "<h1") || strings.Contains(a.ContentHTML, "<script") {
		t.Fatalf("ContentHTML = %q", a.ContentHTML)
	}

	res = c.get("/admin/articles")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "はじめての記事", "山田", "下書き")

	res = c.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `value="はじめての記事"`, "本文です。")
	if strings.Contains(res.Body, "<script>alert(1)") {
		t.Fatal("edit preview contains unsanitized script")
	}

	// 執筆者は直接公開できない（権限マトリクス）。内容も変わらない。
	publishForm := url.Values{
		"title":            {"公開した記事"},
		"slug":             {"first-post"},
		"category_id":      {"1"},
		"excerpt":          {"抜粋"},
		"content_markdown": {"更新後の本文\r\n2行目"},
		"action":           {"publish"},
	}
	// postForm はフォームに自分の CSRF トークンを書き込むため、使い回すフォームは複製して渡す。
	expectStatus(t, c.postForm("/admin/articles/"+id, maps.Clone(publishForm)), http.StatusForbidden)
	if a = env.mustGetArticle(id); a.Status != article.StatusDraft || a.Title != "はじめての記事" {
		t.Fatalf("article changed by forbidden publish: %+v", a)
	}

	// 編集者が更新して公開する（執筆者が編集画面を離れてロックを解放した後）。
	expectStatus(t, c.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	ed := env.loggedIn(chief)
	res = ed.postForm("/admin/articles/"+id, publishForm)
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=published")
	a = env.mustGetArticle(id)
	if a.Title != "公開した記事" || a.Slug != "first-post" || a.Status != article.StatusPublished ||
		a.PublishedAt == nil || a.Excerpt != "抜粋" || a.ContentMarkdown != "更新後の本文\n2行目" || a.AuthorID != studentA.ID {
		t.Fatalf("updated article = %+v", a)
	}
	expectContains(t, c.get("/admin/articles?status=published").Body, "公開した記事", "公開中")
	if strings.Contains(c.get("/admin/articles").Body, "公開した記事") {
		t.Fatal("published article is listed in drafts tab")
	}

	// 公開中の記事は執筆者がゴミ箱に移せない。編集者が論理削除するとゴミ箱タブに移り、ロックも解放される。
	expectStatus(t, c.postForm("/admin/articles/"+id+"/delete", nil), http.StatusConflict)
	expectStatus(t, ed.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	expectStatus(t, c.postForm("/admin/articles/"+id+"/delete", nil), http.StatusForbidden)
	res = ed.postForm("/admin/articles/"+id+"/delete", nil)
	expectRedirect(t, res, "/admin/articles?status=deleted")
	a = env.mustGetArticle(id)
	if a.Status != article.StatusDeleted || a.LockedBy != nil {
		t.Fatalf("deleted article status=%s locked_by=%v", a.Status, a.LockedBy)
	}
	expectContains(t, c.get("/admin/articles?status=deleted").Body, "公開した記事", "ゴミ箱")

	expectStatus(t, c.get("/admin/articles/nonexistent/edit"), http.StatusNotFound)
	expectStatus(t, c.postForm("/admin/articles/nonexistent/delete", nil), http.StatusNotFound)
}

var (
	saveButtonPattern  = regexp.MustCompile(`<button[^>]*id="save-button"[^>]*>`)
	publishBtnPattern  = regexp.MustCompile(`<button[^>]*id="publish-button"[^>]*>`)
	readOnlyTextarea   = regexp.MustCompile(`<textarea[^>]*id="content_markdown"[^>]*\sreadonly`)
	lockWarningMessage = "山田さんが編集中です（閲覧専用モード）"
	// disabledAttr は disabled 属性に一致する。Tailwind の "disabled:" バリアントのクラス名には一致しない。
	disabledAttr = regexp.MustCompile(`\sdisabled[\s/>=]`)
)

// buttonDisabled は body 中の pattern のボタンが disabled かを判定する。
func buttonDisabled(t *testing.T, body string, pattern *regexp.Regexp) bool {
	t.Helper()
	tag := pattern.FindString(body)
	if tag == "" {
		t.Fatalf("button %s not found", pattern)
	}
	return disabledAttr.MatchString(tag)
}

func TestEditLockWarningForOtherUser(t *testing.T) {
	env := newTestEnv(t)
	a := env.loggedIn(studentA)
	b := env.loggedIn(studentB)
	id := a.createArticle("共同執筆", "Aの本文")

	// 学生Aが編集画面を開くとロックを獲得し、編集可能。
	res := a.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "閲覧専用モード") || buttonDisabled(t, res.Body, saveButtonPattern) {
		t.Fatal("lock holder sees read-only mode")
	}
	if got := env.mustGetArticle(id).LockedBy; got == nil || *got != studentA.ID {
		t.Fatalf("locked_by = %v, want %s", got, studentA.ID)
	}

	// 学生Bが同じ記事を開くと警告バナーが出て、保存ボタンが無効になる。
	res = b.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, lockWarningMessage, `id="lock-banner"`)
	for _, p := range []*regexp.Regexp{saveButtonPattern, publishBtnPattern} {
		if !buttonDisabled(t, res.Body, p) {
			t.Errorf("button %s is not disabled", p)
		}
	}
	if !readOnlyTextarea.MatchString(res.Body) {
		t.Error("textarea is not readonly")
	}
	if strings.Contains(res.Body, "force-unlock-button") {
		t.Error("non-admin sees force unlock button")
	}
	// 一覧にもロック中の表示が出る。
	expectContains(t, b.get("/admin/articles").Body, "山田さんが編集中")

	// 学生Bの保存・削除・ハートビート・アンロックは拒否され、内容は変わらない。
	res = b.postForm("/admin/articles/"+id, url.Values{"title": {"Bの上書き"}, "category_id": {"1"}, "content_markdown": {"Bの本文"}})
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, lockWarningMessage, "Bの本文")
	expectStatus(t, b.postForm("/admin/articles/"+id+"/delete", nil), http.StatusConflict)
	res = b.postAPI("/admin/articles/"+id+"/lock", "", nil)
	expectStatus(t, res, http.StatusConflict)
	var lr lockResponse
	if err := json.Unmarshal([]byte(res.Body), &lr); err != nil || lr.LockedBy != "山田" || lr.OwnLock {
		t.Fatalf("heartbeat conflict body = %s (%v)", res.Body, err)
	}
	expectStatus(t, b.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusConflict)
	expectStatus(t, b.postForm("/admin/articles/"+id+"/force-unlock", nil), http.StatusForbidden)
	got := env.mustGetArticle(id)
	if got.Title != "共同執筆" || got.ContentMarkdown != "Aの本文" || got.Status != article.StatusDraft {
		t.Fatalf("article modified by non-holder: %+v", got)
	}

	// 学生Aのハートビートは成功し、ロックが延長される。
	before := *env.mustGetArticle(id).LockedAt
	time.Sleep(10 * time.Millisecond)
	res = a.postAPI("/admin/articles/"+id+"/lock", "", nil)
	expectStatus(t, res, http.StatusOK)
	if err := json.Unmarshal([]byte(res.Body), &lr); err != nil || !lr.Locked || !lr.OwnLock {
		t.Fatalf("heartbeat body = %s (%v)", res.Body, err)
	}
	if after := *env.mustGetArticle(id).LockedAt; !after.After(before) {
		t.Fatalf("locked_at not refreshed: before %v after %v", before, after)
	}

	// 学生Aの保存はロックを保ったまま成功する。
	res = a.postForm("/admin/articles/"+id, url.Values{"title": {"共同執筆"}, "category_id": {"1"}, "content_markdown": {"Aの更新"}})
	expectRedirect(t, res, "/admin/articles/"+id+"/edit")
	got = env.mustGetArticle(id)
	if got.ContentMarkdown != "Aの更新" || got.LockedBy == nil || *got.LockedBy != studentA.ID {
		t.Fatalf("after save by holder: content=%q locked_by=%v", got.ContentMarkdown, got.LockedBy)
	}

	// 学生Aが離脱（sendBeacon と同じく FormData の _csrf 付き）するとロックが外れ、学生Bが編集できる。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField(CSRFFormField, a.csrfToken())
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/admin/articles/"+id+"/unlock", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	expectStatus(t, a.do(req), http.StatusNoContent)
	if got := env.mustGetArticle(id).LockedBy; got != nil {
		t.Fatalf("locked_by = %v after unlock", *got)
	}
	// 重複したアンロックも成功する。
	expectStatus(t, a.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	res = b.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "閲覧専用モード") || buttonDisabled(t, res.Body, saveButtonPattern) {
		t.Fatal("student B still read-only after unlock")
	}
}

// 管理者は編集者・ライターの役割も持つため、記事の作成・他の人の記事の編集・削除ができる。
func TestAdminCanAuthorAndEditArticles(t *testing.T) {
	env := newTestEnv(t)
	a := env.loggedIn(studentA)
	id := a.createArticle("ライターの記事", "本文")
	expectStatus(t, a.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	adm := env.loggedIn(admin)
	expectStatus(t, adm.get("/admin/articles/new"), http.StatusOK)
	adm.createArticle("管理者の記事", "本文")

	expectStatus(t, adm.get("/admin/articles/"+id+"/edit"), http.StatusOK)
	expectRedirect(t, adm.postForm("/admin/articles/"+id, url.Values{
		"title": {"管理者が直した記事"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"draft"},
	}), "/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id); got.Title != "管理者が直した記事" || got.AuthorID != studentA.ID {
		t.Fatalf("after admin edit: title=%q author=%s", got.Title, got.AuthorID)
	}
	expectStatus(t, adm.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	expectRedirect(t, adm.postForm("/admin/articles/"+id+"/delete", nil), "/admin/articles")
	if got := env.mustGetArticle(id); got.Status != article.StatusDeleted {
		t.Errorf("status after admin delete = %s, want deleted", got.Status)
	}
}

// 管理者は編集者の役割も持つため、校閲待ちの記事を開いて、差し戻し・承認ができる。
func TestAdminCanReviewArticles(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("校閲待ちの記事", "本文")
	att := writer.mustAttach(id, "資料.pdf", pdfData)
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/submit-review", url.Values{
		"title": {"校閲待ちの記事"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"review"},
	}), "/admin/articles?status=review")

	adm := env.loggedIn(admin)
	res := adm.get("/admin/articles?status=review")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "校閲待ちの記事", `href="/admin/articles/new"`, `href="/admin/articles/`+id+`/edit"`)
	expectStatus(t, adm.getHX("/admin/articles/"+id+"/comments"), http.StatusOK)
	expectStatus(t, adm.getHX("/admin/articles/"+id+"/revisions"), http.StatusOK)
	expectStatus(t, adm.getHX("/admin/articles/"+id+"/revisions/1/diff"), http.StatusOK)
	expectStatus(t, adm.get("/admin/attachments/"+att.ID+"/download"), http.StatusOK)

	expectRedirect(t, adm.postForm("/admin/articles/"+id+"/reject-review", url.Values{"reason": {"出典を追記してください"}}),
		"/admin/articles/"+id+"/edit?flash=rejected")
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft {
		t.Fatalf("status after admin reject = %s, want draft", a.Status)
	}
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/submit-review", "", nil), http.StatusOK)
	expectRedirect(t, adm.postForm("/admin/articles/"+id+"/approve-publish", nil), "/admin/articles")
	if a := env.mustGetArticle(id); a.Status != article.StatusPublished {
		t.Fatalf("status after admin approve = %s, want published", a.Status)
	}
}

func TestEditorCanManageArticlesButNotSite(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("ライターの下書き", "本文")
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	ed := env.loggedIn(chief)
	// 編集者は他人の記事を編集・公開でき、自分でも記事を作成できる。
	expectStatus(t, ed.get("/admin/articles/"+id+"/edit"), http.StatusOK)
	expectRedirect(t, ed.postForm("/admin/articles/"+id, url.Values{
		"title": {"編集者が仕上げた記事"}, "slug": {"edited"}, "category_id": {"1"},
		"content_markdown": {"本文"}, "action": {"publish"},
	}), "/admin/articles/"+id+"/edit?flash=published")
	if a := env.mustGetArticle(id); a.Status != article.StatusPublished || a.AuthorID != studentA.ID {
		t.Fatalf("after editor publish: status=%s author=%s", a.Status, a.AuthorID)
	}
	ed.createArticle("編集者の記事", "本文")

	// サイト設定・テーマ・ユーザー管理は管理者専用。
	for _, path := range []string{"/admin/settings/general", "/admin/settings/theme", "/admin/users"} {
		expectStatus(t, ed.get(path), http.StatusForbidden)
	}
	res := ed.get("/admin/articles")
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, `href="/admin/users"`) || strings.Contains(res.Body, `href="/admin/settings/general"`) {
		t.Error("editor sees admin-only navigation")
	}
}

func TestAdminForceUnlock(t *testing.T) {
	env := newTestEnv(t)
	a := env.loggedIn(studentA)
	adm := env.loggedIn(admin)
	id := a.createArticle("強制解除", "本文")
	expectStatus(t, a.get("/admin/articles/"+id+"/edit"), http.StatusOK)

	// 管理者は編集画面でロック警告と強制解除ボタンを確認できる
	res := adm.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, lockWarningMessage, `id="force-unlock-button"`)

	// 管理者は POST でロックを強制解除でき、編集画面へリダイレクトされる
	res = adm.postForm("/admin/articles/"+id+"/force-unlock", nil)
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=unlocked")

	// DB 上のロックが解除されていること
	if got := env.mustGetArticle(id).LockedBy; got != nil {
		t.Fatalf("locked_by = %v, want nil", got)
	}
}

func TestEditorForceUnlock(t *testing.T) {
	env := newTestEnv(t)
	a := env.loggedIn(studentA)
	ed := env.loggedIn(chief)
	id := a.createArticle("編集者の強制解除", "本文")
	expectStatus(t, a.get("/admin/articles/"+id+"/edit"), http.StatusOK)

	// 編集者は編集画面でロック警告と強制解除ボタンを確認できる
	res := ed.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, lockWarningMessage, `id="force-unlock-button"`, "強制解除して編集する")

	// 編集者が強制解除すると編集画面へ戻り、自身がロックを取得する
	res = ed.postForm("/admin/articles/"+id+"/force-unlock", nil)
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=unlocked")
	res = ed.get(res.Header.Get("Location"))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "ロックを強制解除しました")

	// ロックを失った学生Aのハートビートは 409 になる
	expectStatus(t, a.postAPI("/admin/articles/"+id+"/lock", "", nil), http.StatusConflict)
}

func TestPreviewSanitizesMarkdown(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	form := url.Values{"content_markdown": {"## 目次の見出し\n\n**太字**<script>alert('xss')</script><img src=x onerror=alert(1)>"}}
	res := c.postAPI("/admin/preview", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "<strong>太字</strong>", `<h2 id="目次の見出し">`, "目次")
	for _, bad := range []string{"<script", "onerror", "alert("} {
		if strings.Contains(res.Body, bad) {
			t.Errorf("preview contains %q: %s", bad, res.Body)
		}
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (c *client) upload(filename string, data []byte, alt string) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if alt != "" {
		mw.WriteField("alt", alt)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		c.t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	return c.postAPI("/admin/media/upload", mw.FormDataContentType(), &buf)
}

func TestMediaUpload(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)

	res := c.upload("campus.png", pngBytes(t, 1600, 900), "キャンパス[写真]")
	expectStatus(t, res, http.StatusOK)
	var m mediaResponse
	if err := json.Unmarshal([]byte(res.Body), &m); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	if m.ID == "" || m.Width != 1600 || m.Height != 900 || m.MimeType != "image/png" || m.Alt != "キャンパス写真" {
		t.Fatalf("media response = %+v", m)
	}
	if m.Markdown != "![キャンパス写真]("+m.LargeURL+")" || m.URL != m.LargeURL {
		t.Fatalf("markdown = %q, large = %q", m.Markdown, m.LargeURL)
	}
	if !strings.Contains(m.HTML, `srcset="`) || !strings.Contains(m.HTML, m.SmallURL+" 400w") {
		t.Fatalf("html = %q", m.HTML)
	}

	// 生成した WebP が保存され、/uploads/ から配信される。
	for _, u := range []string{m.OriginalURL, m.LargeURL, m.MediumURL, m.SmallURL} {
		if !strings.HasPrefix(u, UploadURLPrefix) || !strings.HasSuffix(u, ".webp") {
			t.Fatalf("unexpected url %q", u)
		}
		if _, err := os.Stat(filepath.Join(env.uploadDir, strings.TrimPrefix(u, UploadURLPrefix))); err != nil {
			t.Fatalf("file for %s: %v", u, err)
		}
		r := c.get(u)
		expectStatus(t, r, http.StatusOK)
		if ct := r.Header.Get("Content-Type"); ct != "image/webp" || !strings.HasPrefix(r.Body, "RIFF") {
			t.Fatalf("GET %s: content-type %q", u, ct)
		}
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND uploaded_by = ? AND width = 1600`, m.ID, studentA.ID); n != 1 {
		t.Fatalf("media rows = %d, want 1", n)
	}

	// 代替テキストを省略するとファイル名を使う。
	res = c.upload("logo.png", pngBytes(t, 32, 32), "")
	expectStatus(t, res, http.StatusOK)
	if err := json.Unmarshal([]byte(res.Body), &m); err != nil || m.Alt != "logo" {
		t.Fatalf("alt = %q (%v)", m.Alt, err)
	}

	// 画像以外・拡張子の詐称・ファイル無しは拒否し、ファイルを残さない。
	files, _ := os.ReadDir(env.uploadDir)
	expectStatus(t, c.upload("note.png", []byte("not an image"), ""), http.StatusUnsupportedMediaType)
	expectStatus(t, c.upload("photo.jpg", pngBytes(t, 8, 8), ""), http.StatusUnsupportedMediaType)
	expectStatus(t, c.postAPI("/admin/media/upload", "application/x-www-form-urlencoded", strings.NewReader("alt=x")), http.StatusBadRequest)
	if after, _ := os.ReadDir(env.uploadDir); len(after) != len(files) {
		t.Fatalf("upload dir has %d files after rejected uploads, want %d", len(after), len(files))
	}

	// 未認証のアップロードは 401。
	anon := env.newClient()
	expectStatus(t, anon.upload("campus.png", pngBytes(t, 8, 8), ""), http.StatusUnauthorized)
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	for _, p := range []string{"/static/js/editor.js", "/static/js/admin.js", "/static/css/admin.css"} {
		res := c.get(p)
		expectStatus(t, res, http.StatusOK)
		if len(res.Body) == 0 {
			t.Errorf("%s is empty", p)
		}
	}
	expectStatus(t, c.get("/static/embed.go"), http.StatusNotFound)
}

// /favicon.ico は GET・HEAD とも現在の Favicon へ 302 でリダイレクトする。
func TestFaviconICORedirect(t *testing.T) {
	env := newTestEnv(t)
	pub := env.newClient()

	head := func(path string) *response {
		t.Helper()
		req, err := http.NewRequest(http.MethodHead, env.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return pub.do(req)
	}
	expectFaviconRedirect := func(want string) {
		t.Helper()
		for _, res := range []*response{pub.get("/favicon.ico"), head("/favicon.ico")} {
			expectStatus(t, res, http.StatusFound)
			if loc := res.Header.Get("Location"); loc != want {
				t.Errorf("%s /favicon.ico: Location = %q, want %q", res.Request.Method, loc, want)
			}
			if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("%s /favicon.ico: Cache-Control = %q", res.Request.Method, cc)
			}
		}
	}

	// 未設定なら既定の Favicon へ。転送先は実際に配信される。
	expectFaviconRedirect(DefaultFaviconURL)
	expectStatus(t, pub.get(DefaultFaviconURL), http.StatusOK)

	// カスタム Favicon を設定するとその URL へ。
	expectRedirect(t, env.loggedIn(admin).postSettingsWithFavicon(generalSettingsForm(), "favicon.ico", "image/x-icon", testFaviconICO),
		"/admin/settings/general?flash=settings_saved")
	custom := env.setting("favicon_url")
	expectFaviconRedirect(custom)
	expectStatus(t, pub.get(custom), http.StatusOK)

	// 保存値が外部サイトなどを指していても、外部へは転送せず既定の Favicon へ転送する。
	for _, bad := range []string{
		"https://evil.example/x.ico", "//evil.example/x.ico", "/\\evil.example/x.ico", "/\tevil",
		"javascript:alert(1)", "evil.example/x.ico", "/uploads/x.ico\r\nSet-Cookie: a=b",
	} {
		if _, err := env.db.Exec(`UPDATE settings SET value = ? WHERE key = 'favicon_url'`, bad); err != nil {
			t.Fatal(err)
		}
		res := pub.get("/favicon.ico")
		expectStatus(t, res, http.StatusFound)
		if loc := res.Header.Get("Location"); loc != DefaultFaviconURL {
			t.Errorf("favicon_url %q: Location = %q, want %q", bad, loc, DefaultFaviconURL)
		}
	}

	// /favicon.ico は公開専用モードでも配信する。
	pubOnly := newTestEnvWith(t, Config{PublicOnly: true}, nil)
	res := pubOnly.newClient().get("/favicon.ico")
	expectStatus(t, res, http.StatusFound)
	if loc := res.Header.Get("Location"); loc != DefaultFaviconURL {
		t.Errorf("public-only Location = %q", loc)
	}
}
