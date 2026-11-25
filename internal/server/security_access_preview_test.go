package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sorus/internal/database"
)

// accessLogs は出力先に書かれたアクセスログ（type が access の JSON の行）を順に返す。
func accessLogs(t *testing.T, out string) []accessLogEntry {
	t.Helper()
	var entries []accessLogEntry
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, `"type":"access"`) {
			continue
		}
		var e accessLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("access log line is not JSON: %v\n%s", err, line)
		}
		entries = append(entries, e)
	}
	return entries
}

// waitAccessLog は method・path の要求のアクセスログが書かれるまで待って返す。ログは応答を書き終えた後に書くため、
// クライアントが応答を読み終えた時点ではまだ無いことがある。
func waitAccessLog(t *testing.T, logs *lockedBuffer, method, path string) accessLogEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, e := range accessLogs(t, logs.String()) {
			if e.Method == method && e.Path == path {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no access log for %s %s\nlogs:\n%s", method, path, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// サーバーモードのアクセスログは、公開ページ・/admin・/oauth・/api への要求を1行の JSON で残す。パスにはクエリ文字列を
// 含めず、Authorization・Cookie も書かない。送り元は TRUSTED_PROXIES のプロキシが付けた X-Forwarded-For を使う。
func TestAccessLogRecordsRequestsWithoutSecrets(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	logs := &lockedBuffer{}
	env := newTestEnvWith(t, Config{LogOutput: logs, TrustedProxies: []string{"127.0.0.1", "::1"}}, nil)
	c := env.newClient()

	// 監視とアセットは残さない（後の要求のログを待てば、これらのログが書かれる機会は過ぎている）。
	expectStatus(t, c.get("/healthz"), http.StatusOK)
	expectStatus(t, c.get("/static/css/public.css"), http.StatusOK)

	req, err := http.NewRequest(http.MethodGet, env.srv.URL+"/?t=secret-query-token&x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-bearer-token")
	req.Header.Set("Cookie", "sorus_session=secret-cookie-value")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	top := c.do(req)
	expectStatus(t, top, http.StatusOK)

	expectStatus(t, c.get("/no-such-page?token=secret-404-token"), http.StatusNotFound)
	expectStatus(t, c.get("/admin/articles"), http.StatusSeeOther)
	// ログインのフォーム（パスワード）は本文で送るため、ログには出ない。
	writer := env.loggedIn(studentA)
	expectStatus(t, writer.get("/admin/articles"), http.StatusOK)
	tokenRes := c.postRaw(OAuthTokenPath, nil)
	if tokenRes.StatusCode < 400 {
		t.Fatalf("POST %s without parameters: status %d, want an error", OAuthTokenPath, tokenRes.StatusCode)
	}
	mcpReq, err := http.NewRequest(http.MethodPost, env.srv.URL+"/api/mcp?access_token=secret-mcp-token", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	mcpReq.Header.Set("Content-Type", "application/json")
	expectStatus(t, c.do(mcpReq), http.StatusUnauthorized)

	got := waitAccessLog(t, logs, http.MethodGet, "/")
	if got.Status != http.StatusOK || got.RemoteIP != "203.0.113.7" || got.BytesOut <= 0 || got.LatencyMS < 0 {
		t.Errorf("top page log = %+v, want status 200, remote_ip 203.0.113.7, bytes_out > 0", got)
	}
	if _, err := time.Parse(accessLogTimeFormat, got.Time); err != nil {
		t.Errorf("time %q: %v", got.Time, err)
	}
	if e := waitAccessLog(t, logs, http.MethodGet, "/no-such-page"); e.Status != http.StatusNotFound || e.BytesOut <= 0 {
		t.Errorf("404 page log = %+v, want status 404 with the not-found page", e)
	}
	waitAccessLog(t, logs, http.MethodPost, "/admin/login")
	if e := waitAccessLog(t, logs, http.MethodPost, OAuthTokenPath); e.Status != tokenRes.StatusCode {
		t.Errorf("oauth token log status = %d, want %d", e.Status, tokenRes.StatusCode)
	}
	if e := waitAccessLog(t, logs, http.MethodPost, "/api/mcp"); e.Status != http.StatusUnauthorized {
		t.Errorf("mcp log status = %d, want 401", e.Status)
	}

	var adminStatuses []int
	for _, e := range accessLogs(t, logs.String()) {
		if e.Path == "/admin/articles" {
			adminStatuses = append(adminStatuses, e.Status)
		}
		if e.Path == "/healthz" || strings.HasPrefix(e.Path, "/static/") {
			t.Errorf("noise was logged: %+v", e)
		}
		if e.Time == "" || e.Method == "" || e.RemoteIP == "" || e.Status == 0 {
			t.Errorf("log entry lacks a field: %+v", e)
		}
	}
	if len(adminStatuses) != 2 || adminStatuses[0] != http.StatusSeeOther || adminStatuses[1] != http.StatusOK {
		t.Errorf("/admin/articles statuses = %v, want [303 200]", adminStatuses)
	}
	out := logs.String()
	for _, secret := range []string{"secret-query-token", "secret-bearer-token", "secret-cookie-value", "secret-404-token",
		"secret-mcp-token", testPassword, "?t=", "Bearer", "access_token"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q:\n%s", secret, out)
		}
	}
}

// 公開専用モードもサーバーモードと同じく記録する。管理画面を隠す 404（Pre のミドルウェアが返す）も残る。
func TestAccessLogInPublicOnlyMode(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	logs := &lockedBuffer{}
	env := newTestEnvWith(t, Config{LogOutput: logs, PublicOnly: true}, nil)
	c := env.newClient()
	expectStatus(t, c.get("/admin/login?next=/admin"), http.StatusNotFound)
	if e := waitAccessLog(t, logs, http.MethodGet, "/admin/login"); e.Status != http.StatusNotFound {
		t.Errorf("hidden admin log status = %d, want 404", e.Status)
	}
	if strings.Contains(logs.String(), "next=") {
		t.Errorf("the query string was logged:\n%s", logs.String())
	}
}

// newDesktopEnvLogging は newDesktopEnv と同じデスクトップモードのサーバーを、Echo のログを logs に書かせて起動する。
func newDesktopEnvLogging(t *testing.T, logs *lockedBuffer) *testEnv {
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
	s, err := New(Config{DB: db, UploadDir: filepath.Join(dir, "uploads"), AttachmentDir: filepath.Join(dir, "attachments"),
		Desktop: true, DisableScheduler: true, LogOutput: logs})
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
	return &testEnv{t: t, db: db, srv: srv, server: s, uploadDir: filepath.Join(dir, "uploads"), attachmentDir: filepath.Join(dir, "attachments")}
}

// デスクトップモードは自分の PC の中だけで使うため、アクセスログを書かない。
func TestAccessLogIsOffInDesktopMode(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	logs := &lockedBuffer{}
	env := newDesktopEnvLogging(t, logs)
	c := env.completeSetup()
	expectStatus(t, c.get("/"), http.StatusOK)
	expectStatus(t, c.get("/admin/articles"), http.StatusOK)
	expectStatus(t, c.get("/no-such-page"), http.StatusNotFound)
	if got := accessLogs(t, logs.String()); len(got) != 0 {
		t.Errorf("desktop mode wrote access logs: %+v", got)
	}
}

// previewTokenPattern は既定のブラウザで開かせるプレビューの URL のトークン（128 ビットの 32 文字の16進数）。
var previewTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// openPreview は c で記事 id のプレビューを既定のブラウザで開かせ、開かせた URL のトークンを返す。
func openPreview(t *testing.T, env *testEnv, c *client, opened *[]string, id string) string {
	t.Helper()
	before := len(*opened)
	expectStatus(t, c.postAPI("/admin/articles/"+id+"/preview-open", "", nil), http.StatusNoContent)
	if len(*opened) != before+1 {
		t.Fatalf("opened %q, want one more URL", *opened)
	}
	u := (*opened)[len(*opened)-1]
	prefix := env.srv.URL + PreviewTokenPath + "/"
	token, ok := strings.CutPrefix(u, prefix)
	if !ok || !previewTokenPattern.MatchString(token) {
		t.Fatalf("opened %q, want %s<32 hex digits>", u, prefix)
	}
	return token
}

// 「公開デザインで確認」をデスクトップモードで押すと、使い捨てのトークンの URL を既定のブラウザで開かせる。既定のブラウザ
// （ログインの Cookie が無い）でも公開テーマのまま開け、2回目は 404。閲覧数に数えず、検索エンジン・キャッシュに残させず、
// 開いている画面の接続のスクリプト・解析タグも入れない。
func TestDesktopPreviewOpenUsesSingleUseToken(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	opened := stubDesktopOpenBrowser(t, nil)
	env, _ := newDesktopEnv(t)
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES ('theme', 'news'), ('analytics_tag', '<script id="site-analytics"></script>')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}
	c := env.completeSetup()
	id := c.createArticle("既定のブラウザで確かめる下書き", "## はじめに\n\n本文の段落です。")

	token := openPreview(t, env, c, opened, id)
	if again := openPreview(t, env, c, opened, id); again == token {
		t.Fatalf("two previews got the same token %q", token)
	}

	browser := env.newClient()
	res := browser.get(PreviewTokenPath + "/" + token)
	expectStatus(t, res, http.StatusOK)
	for header, want := range map[string]string{"Cache-Control": "no-store", "X-Robots-Tag": "noindex", "ETag": ""} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	expectContains(t, res.Body, `data-theme="news"`, `<meta name="robots" content="noindex">`,
		"<title>既定のブラウザで確かめる下書き | ", "本文の段落です。")
	expectNotContainsAny(t, "token preview", res.Body, windowScriptMarker, `id="site-analytics"`)

	// 1回だけ使える。
	expectStatus(t, browser.get(PreviewTokenPath+"/"+token), http.StatusNotFound)
	expectStatus(t, c.get(PreviewTokenPath+"/"+token), http.StatusNotFound)

	env.flushAnalytics()
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily`); n != 0 {
		t.Errorf("token previews counted %d views, want 0", n)
	}
}

// 期限（60 秒）の切れたトークン・知らないトークン・ゴミ箱に入れた記事のトークンは 404。
func TestDesktopPreviewTokenRejectsExpiredAndUnknownTokens(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	prev := clockNow
	clockNow = func() time.Time { return time.Unix(0, now.Load()) }
	t.Cleanup(func() { clockNow = prev })

	opened := stubDesktopOpenBrowser(t, nil)
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	id := c.createArticle("期限を確かめる下書き", "本文")
	browser := env.newClient()

	t.Run("just before the deadline", func(t *testing.T) {
		token := openPreview(t, env, c, opened, id)
		now.Add(int64(previewTokenTTL - time.Second))
		expectStatus(t, browser.get(PreviewTokenPath+"/"+token), http.StatusOK)
	})
	t.Run("expired", func(t *testing.T) {
		token := openPreview(t, env, c, opened, id)
		now.Add(int64(previewTokenTTL))
		expectStatus(t, browser.get(PreviewTokenPath+"/"+token), http.StatusNotFound)
	})
	t.Run("unknown", func(t *testing.T) {
		for _, token := range []string{"not-a-token", strings.Repeat("0", 32), strings.Repeat("f", 33)} {
			expectStatus(t, browser.get(PreviewTokenPath+"/"+token), http.StatusNotFound)
		}
	})
	t.Run("trashed after issuing", func(t *testing.T) {
		token := openPreview(t, env, c, opened, id)
		expectStatus(t, c.postForm("/admin/articles/"+id+"/delete", nil), http.StatusSeeOther)
		expectStatus(t, browser.get(PreviewTokenPath+"/"+token), http.StatusNotFound)
	})
}

// プレビューを開かせるには、ログイン・CSRF トークン・存在する（ゴミ箱でない）記事が要る。ブラウザを起動できなければ
// 500 にし、発行したトークンは使えなくする。
func TestDesktopPreviewOpenRequiresLoginCSRFAndArticle(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	opened := stubDesktopOpenBrowser(t, nil)
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	id := c.createArticle("権限を確かめる下書き", "本文")
	path := "/admin/articles/" + id + "/preview-open"

	expectStatus(t, c.postRaw(path, nil), http.StatusForbidden)
	if res := env.newClient().postForm(path, nil); res.StatusCode == http.StatusNoContent {
		t.Errorf("a visitor who is not logged in opened a preview (status %d)", res.StatusCode)
	}
	expectStatus(t, c.postAPI("/admin/articles/no-such-id/preview-open", "", nil), http.StatusNotFound)
	trashed := c.createArticle("ゴミ箱の下書き", "本文")
	expectStatus(t, c.postForm("/admin/articles/"+trashed+"/delete", nil), http.StatusSeeOther)
	expectStatus(t, c.postAPI("/admin/articles/"+trashed+"/preview-open", "", nil), http.StatusNotFound)
	if len(*opened) != 0 {
		t.Fatalf("opened %q without login, CSRF token or an article", *opened)
	}

	fail := stubDesktopOpenBrowser(t, errors.New("no browser"))
	expectStatus(t, c.postAPI(path, "", nil), http.StatusInternalServerError)
	if len(*fail) != 1 {
		t.Fatalf("tried to open %q, want one URL", *fail)
	}
	token := strings.TrimPrefix((*fail)[0], env.srv.URL+PreviewTokenPath+"/")
	expectStatus(t, env.newClient().get(PreviewTokenPath+"/"+token), http.StatusNotFound)
}

// サーバーモードには既定のブラウザで開かせる操作も、使い捨てのプレビューも無い（存在しないページと同じ 404）。
func TestPreviewTokenRoutesAreDesktopOnly(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	opened := stubDesktopOpenBrowser(t, nil)
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("サーバーモードの下書き", "本文")
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/preview-open", "", nil), http.StatusNotFound)
	expectStatus(t, env.newClient().get(PreviewTokenPath+"/"+strings.Repeat("a", 32)), http.StatusNotFound)
	// 構造体のメソッドを直接呼んでも、デスクトップモードでなければ描画しない。
	token, err := env.server.previewTokens.issue(id, clockNow())
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, env.newClient().get(PreviewTokenPath+"/"+token), http.StatusNotFound)
	if len(*opened) != 0 {
		t.Errorf("server mode opened %q", *opened)
	}
}

func TestPreviewTokenStore(t *testing.T) {
	var st previewTokenStore
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	a, err := st.issue("article-a", now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.issue("article-b", now)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !previewTokenPattern.MatchString(a) || !previewTokenPattern.MatchString(b) {
		t.Fatalf("tokens %q and %q must be distinct 32-digit hex strings", a, b)
	}
	if id, ok := st.consume(a, now.Add(previewTokenTTL-time.Nanosecond)); !ok || id != "article-a" {
		t.Errorf("consume(a) = %q, %v; want article-a, true", id, ok)
	}
	if _, ok := st.consume(a, now); ok {
		t.Error("a token was used twice")
	}
	if _, ok := st.consume(b, now.Add(previewTokenTTL)); ok {
		t.Error("an expired token was accepted")
	}
	if _, ok := st.consume("", now); ok {
		t.Error("an empty token was accepted")
	}

	// 期限の切れたトークンは、次の発行のときに掃除する。
	for range 3 {
		if _, err := st.issue("old", now); err != nil {
			t.Fatal(err)
		}
	}
	c, err := st.issue("article-c", now.Add(previewTokenTTL))
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n := len(st.tokens)
	st.mu.Unlock()
	if n != 1 {
		t.Errorf("%d tokens are kept after the sweep, want 1", n)
	}
	st.revoke(c)
	if _, ok := st.consume(c, now.Add(previewTokenTTL)); ok {
		t.Error("a revoked token was accepted")
	}
}

func TestDesktopPreviewOrigin(t *testing.T) {
	for _, tc := range []struct{ scheme, host, want string }{
		{"http", "127.0.0.1:8780", "http://127.0.0.1:8780"},
		{"http", "localhost:8780", "http://localhost:8780"},
		{"http", "[::1]:8780", "http://[::1]:8780"},
		{"https", "127.0.0.1:8443", "https://127.0.0.1:8443"},
		{"", "127.0.0.1:8780", "http://127.0.0.1:8780"},
	} {
		got, err := desktopPreviewOrigin(tc.scheme, tc.host)
		if err != nil || got != tc.want {
			t.Errorf("desktopPreviewOrigin(%q, %q) = %q, %v; want %q", tc.scheme, tc.host, got, err, tc.want)
		}
	}
	for _, host := range []string{"", "example.com:8780", "192.168.1.10:8780", "127.0.0.1:8780/x", "user@127.0.0.1:8780",
		"127.0.0.1:8780?x=1", "127.0.0.1:8780#x", "localhost.example.com:8780"} {
		if got, err := desktopPreviewOrigin("http", host); err == nil {
			t.Errorf("desktopPreviewOrigin(http, %q) = %q, want an error", host, got)
		}
	}
}
