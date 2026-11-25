package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"

	"sorus/internal/article"
	"sorus/internal/database"
)

// windowScriptMarker は開いている画面の接続のスクリプトが出ているかの目印。
const windowScriptMarker = "new WebSocket("

// lockedBuffer は、接続のハンドラーと同時に読み書きできる動作の記録の出力先。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// windowSite は、開いている画面の接続を確かめるためのデスクトップモードのサイト。tune で待ち受けの http.Server の
// 設定（読み書きのタイムアウト）を変えられる。
type windowSite struct {
	server *Server
	srv    *httptest.Server
	log    *lockedBuffer
}

func newWindowSite(t *testing.T, tune func(*http.Server)) *windowSite {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Connect(filepath.Join(dir, "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	s, err := New(Config{DB: db, UploadDir: filepath.Join(dir, "uploads"), AttachmentDir: filepath.Join(dir, "attachments"),
		Desktop: true, DisableScheduler: true, LogOutput: logs})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(s.Handler())
	if tune != nil {
		tune(srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return &windowSite{server: s, srv: srv, log: logs}
}

// origin はこのサイトのページの Origin（ブラウザが WebSocket の接続に付ける値）。
func (w *windowSite) origin() string { return w.srv.URL }

// openWindow は、ページと同じ Origin で WindowPath へ WebSocket で接続する。
func (w *windowSite) openWindow(t *testing.T) *websocket.Conn {
	t.Helper()
	return w.openWindowQuery(t, "")
}

// token はこのサイトの接続の合言葉（ページのスクリプトが &t= に付ける値）。
func (w *windowSite) token() string { return w.server.windowState().token }

// openWindowQuery は openWindow と同じく、合言葉を付けて接続する。query は WindowPath に付ける、合言葉のほかの問い合わせ
// （空なら付けない）。
func (w *windowSite) openWindowQuery(t *testing.T, query string) *websocket.Conn {
	t.Helper()
	if query != "" {
		query += "&"
	}
	conn, err := w.dialWindow(query + "t=" + w.token())
	if err != nil {
		t.Fatalf("dial %s?%s: %v", WindowPath, query, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// dialWindow は、ページと同じ Origin で WindowPath?query へ WebSocket で接続する（合言葉は query に含める）。
func (w *windowSite) dialWindow(query string) (*websocket.Conn, error) {
	u := "ws" + strings.TrimPrefix(w.srv.URL, "http") + WindowPath
	if query != "" {
		u += "?" + query
	}
	return websocket.Dial(u, "", w.origin())
}

// setWindowTimings は ping の間隔と、何も届かないまま待つ上限をテストの間だけ変える。
func setWindowTimings(t *testing.T, keepalive, readWait time.Duration) {
	t.Helper()
	prevKeepalive, prevReadWait := windowKeepalive, windowReadWait
	windowKeepalive, windowReadWait = keepalive, readWait
	t.Cleanup(func() { windowKeepalive, windowReadWait = prevKeepalive, prevReadWait })
}

// rawUpgrade は WebSocket の接続の要求を、Origin を指定し、合言葉を付けて送り、応答を返す。
func (w *windowSite) rawUpgrade(t *testing.T, origin string) *http.Response {
	t.Helper()
	return w.rawUpgradeQuery(t, origin, "t="+w.token())
}

// rawUpgradeQuery は rawUpgrade と同じく送る。query は WindowPath に付ける問い合わせ（空なら付けない）。
func (w *windowSite) rawUpgradeQuery(t *testing.T, origin, query string) *http.Response {
	t.Helper()
	u := w.srv.URL + WindowPath
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// waitClosed は、サーバーが接続を閉じるまで読み続ける（ブラウザと同じく、送られる ping には自動で応える）。
func waitClosed(t *testing.T, conn *websocket.Conn, what string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, conn)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the window connection stayed open", what)
	}
}

// 画面が WebSocket でつながると数が増え、切れると減る。数の変化は動作の記録に残る。
func TestWindowCountRisesAndFalls(t *testing.T) {
	w := newWindowSite(t, nil)
	if n := w.server.OpenWindows(); n != 0 {
		t.Fatalf("OpenWindows before any window = %d, want 0", n)
	}
	c1 := w.openWindow(t)
	waitForCond(t, "one window open", func() bool { return w.server.OpenWindows() == 1 })
	c2 := w.openWindow(t)
	waitForCond(t, "two windows open", func() bool { return w.server.OpenWindows() == 2 })

	c1.Close()
	waitForCond(t, "one window closed", func() bool { return w.server.OpenWindows() == 1 })
	c2.Close()
	waitForCond(t, "all windows closed", func() bool { return w.server.OpenWindows() == 0 })

	logs := w.log.String()
	for _, want := range []string{"window: connected (1 open)", "window: connected (2 open)",
		"window: disconnected (1 open): closed by the page", "window: disconnected (0 open): closed by the page"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if got := strings.Count(logs, "window: disconnected"); got != 2 {
		t.Errorf("disconnected logged %d times, want 2:\n%s", got, logs)
	}
}

// 接続は、待ち受けのサーバーの読み書きのタイムアウトを過ぎても切れず、数えたままになる（つながっている間に送る ping も
// 書き込める）。ping に pong を返している間は、何も届かないまま待つ上限（windowReadWait）を何度過ぎても切れない。
func TestWindowSurvivesServerTimeouts(t *testing.T) {
	setWindowTimings(t, 30*time.Millisecond, 150*time.Millisecond)
	w := newWindowSite(t, func(hs *http.Server) {
		hs.ReadTimeout = 100 * time.Millisecond
		hs.WriteTimeout = 100 * time.Millisecond
	})
	conn := w.openWindow(t)
	// ブラウザと同じく、届いた ping に応えるため読み続ける。
	readErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, conn)
		readErr <- err
	}()
	waitForCond(t, "window open", func() bool { return w.server.OpenWindows() == 1 })
	time.Sleep(600 * time.Millisecond)
	if n := w.server.OpenWindows(); n != 1 {
		t.Fatalf("OpenWindows after the server timeouts = %d, want 1\n%s", n, w.log.String())
	}
	select {
	case err := <-readErr:
		t.Fatalf("the window connection ended after the server timeouts: %v", err)
	default:
	}
	conn.Close()
	waitForCond(t, "window closed", func() bool { return w.server.OpenWindows() == 0 })
}

// ほかのサイト（別の Origin）のページからの接続は 403 で断り、数えない。Origin の無い要求も断る。
func TestWindowRefusesCrossOrigin(t *testing.T) {
	w := newWindowSite(t, nil)
	for _, origin := range []string{"http://evil.example", "http://127.0.0.1:1", ""} {
		res := w.rawUpgrade(t, origin)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q: status %d, want 403", origin, res.StatusCode)
		}
	}
	if n := w.server.OpenWindows(); n != 0 {
		t.Fatalf("OpenWindows after cross-origin attempts = %d, want 0", n)
	}
	if strings.Contains(w.log.String(), "window: connected") {
		t.Errorf("a cross-origin attempt was logged as connected:\n%s", w.log.String())
	}
	// 同じ Origin なら受け付ける（WebSocket に切り替える）。
	if res := w.rawUpgrade(t, w.origin()); res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("same origin: status %d, want 101", res.StatusCode)
	}
	waitForCond(t, "same-origin window counted", func() bool { return w.server.OpenWindows() == 1 })
}

// 合言葉（?t=）が無い・違う（前に起動した sorus のページ、ほかのサイトの合言葉）接続は、同じ Origin でも 403 で断り、
// 数えず、接続したと記録しない。
func TestWindowRefusesWrongToken(t *testing.T) {
	w := newWindowSite(t, nil)
	other := newWindowSite(t, nil)
	if w.token() == other.token() {
		t.Fatalf("two sites share the window token %q", w.token())
	}
	if len(w.token()) != 32 || strings.Trim(w.token(), "0123456789abcdef") != "" {
		t.Fatalf("window token %q is not 32 hex characters", w.token())
	}
	for name, query := range map[string]string{
		"no token":         "",
		"page only":        "page=" + url.QueryEscape("/admin"),
		"empty token":      "t=",
		"another site":     "t=" + other.token(),
		"truncated":        "t=" + w.token()[:31],
		"longer":           "t=" + w.token() + "0",
		"upper case":       "t=" + strings.ToUpper(w.token()),
		"repeated, second": "t=" + other.token() + "&t=" + w.token(),
	} {
		if res := w.rawUpgradeQuery(t, w.origin(), query); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, res.StatusCode)
		}
		if conn, err := w.dialWindow(query); err == nil {
			conn.Close()
			t.Errorf("%s: the WebSocket connection was accepted", name)
		}
	}
	if n := w.server.OpenWindows(); n != 0 {
		t.Fatalf("OpenWindows after wrong-token attempts = %d, want 0", n)
	}
	if strings.Contains(w.log.String(), "window: connected") {
		t.Errorf("a wrong-token attempt was logged as connected:\n%s", w.log.String())
	}
	// 合言葉が合えば受け付ける。
	w.openWindowQuery(t, "page="+url.QueryEscape("/admin"))
	waitForCond(t, "right-token window counted", func() bool { return w.server.OpenWindows() == 1 })
}

// Shutdown が始まると、開いたままの接続を終わらせる（終わらない接続を Shutdown が待ち続けない）。始まった後の接続は断る。
func TestWindowStreamEndsOnShutdown(t *testing.T) {
	w := newWindowSite(t, nil)
	c1, c2 := w.openWindow(t), w.openWindow(t)
	waitForCond(t, "two windows open", func() bool { return w.server.OpenWindows() == 2 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// httptest のサーバーでは Echo の http.Server は待ち受けていないが、Shutdown の始まりに呼ぶ関数は同じく呼ばれる。
	if err := w.server.echo.Server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	waitClosed(t, c1, "first window")
	waitClosed(t, c2, "second window")
	waitForCond(t, "windows uncounted after shutdown", func() bool { return w.server.OpenWindows() == 0 })
	if res := w.rawUpgrade(t, w.origin()); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("connect after shutdown: status %d, want 503", res.StatusCode)
	}
}

// testWindowToken はスクリプトを確かめるときに埋め込む合言葉。
const testWindowToken = "0123456789abcdef0123456789abcdef"

// 埋め込むスクリプトは WebSocket でつなぎ、閉じたら windowRetry ミリ秒後につなぎ直す。SSE（EventSource）は使わない
// （ブラウザの同じホストへの接続の上限 6 本を、開いている画面が使い切らないように）。接続の URL に合言葉を付ける。
func TestWindowScriptUsesWebSocket(t *testing.T) {
	script := string(windowScriptWith(testWindowToken))
	for _, want := range []string{"new WebSocket(", "location.host + '" + WindowPath + "'", "'wss://'", "'ws://'",
		"onclose", "setTimeout(connect, wait)", "window.top !== window", "!window.WebSocket",
		"window.sorusWindow", "'?page=' + encodeURIComponent(location.pathname) + '&t=" + testWindowToken + "'"} {
		if !strings.Contains(script, want) {
			t.Errorf("windowScript lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "EventSource") {
		t.Errorf("windowScript still uses EventSource:\n%s", script)
	}
}

// つながる前に切れる（合言葉が違って断られた・sorus がいない）ことが続くと、つなぎ直すまでの待ちを倍にしていき、
// windowRetryMax で止める。sorus が規約違反（1008）で閉じたらつなぎ直さない。一度つながれば待ちは windowRetry に戻る。
func TestWindowScriptBacksOffAfterRefusals(t *testing.T) {
	script := string(windowScriptWith(testWindowToken))
	retry := strconv.Itoa(windowRetry)
	for _, want := range []string{
		"var fails = 0;",
		"var opened = false;",
		"s.onopen = function() { opened = true; fails = 0; };",
		"if (e && e.code === 1008) return;",
		"if (!opened) fails++;",
		"var wait = fails > 1 ? Math.min(" + retry + " * Math.pow(2, fails - 1), " + strconv.Itoa(windowRetryMax) + ") : " + retry + ";",
		"setTimeout(connect, wait);",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("windowScript lacks %q:\n%s", want, script)
		}
	}
	if windowRetryMax < 10*windowRetry || windowRetryMax > 60000 {
		t.Errorf("windowRetryMax = %d ms, want between %d and 60000", windowRetryMax, 10*windowRetry)
	}
}

// ウィンドウを閉じたら 1 分以内に終わるよう、ping の間隔と応答を待つ上限を短くする（admin_ux_phase5_spec.md 1.1）。
// 応答を待つ上限は、pong の遅れ1回では切らないよう ping の間隔より長く、アプリの猶予時間（5 秒）と合わせて
// 1 分を十分に下回る。つなぎ直しの待ちは猶予時間より短い。
func TestWindowTimingQuitsWithinAMinute(t *testing.T) {
	if windowKeepalive != 10*time.Second || windowReadWait != 25*time.Second {
		t.Errorf("windowKeepalive, windowReadWait = %s, %s, want 10s, 25s", windowKeepalive, windowReadWait)
	}
	if windowReadWait < 2*windowKeepalive {
		t.Errorf("windowReadWait %s cuts a page off after one late pong (keepalive %s)", windowReadWait, windowKeepalive)
	}
	const appGrace = 5 * time.Second
	if total := windowReadWait + appGrace; total > 35*time.Second {
		t.Errorf("an unresponsive window keeps sorus running for %s, want at most 35s", total)
	}
	if retry := time.Duration(windowRetry) * time.Millisecond; retry >= appGrace {
		t.Errorf("windowRetry %s is not shorter than the app grace %s", retry, appGrace)
	}
}

// ページを離れる・閉じるとき（pagehide・beforeunload）は、closing を立てて接続を自分から閉じ、onclose でつなぎ直さない。
// 戻る・進むでページがそのまま戻ったとき（pageshow の persisted）と、beforeunload の後もページが残ったとき（ダウンロード
// のリンクなど）は、つなぎ直す。古い接続の onclose や予約したつなぎ直しで、接続が2本にならない。
func TestWindowScriptClosesOnUnload(t *testing.T) {
	script := string(windowScriptWith(testWindowToken))
	retry := strconv.Itoa(windowRetry)
	for _, want := range []string{
		"var closing = false;",
		"closing = true;",
		// 正常終了（1000）を送り、sorus が応答の途絶え（windowReadWait）を待たずに数から外せるようにする。
		"if (w.socket) { try { w.socket.close(1000); } catch (_) {} }",
		"window.addEventListener('pagehide', suspend);",
		"window.addEventListener('beforeunload', function() {\n    suspend();\n    setTimeout(resume, " + retry + ");",
		"window.addEventListener('pageshow', function(e) { if (e.persisted) resume(); });",
		// onclose は閉じている途中ならつなぎ直さず、今の接続のときだけつなぎ直す。
		"s.onclose = function(e) {\n      if (closing) return;\n      if (w.socket !== s) return;",
		// つながっている・つなぎ始めた接続があれば、新しくつながない。
		"if (closing || (w.socket && w.socket.readyState <= 1)) return;",
		// resume は閉じている途中のときだけ closing を戻してつなぎ直す。
		"if (!closing) return;\n    closing = false;\n    connect();",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("windowScript lacks %q:\n%s", want, script)
		}
	}
	// 以前の、閉じたら必ずつなぎ直す onclose は残さない。
	if strings.Contains(script, "onclose = function() { setTimeout(connect") {
		t.Errorf("windowScript still reconnects unconditionally:\n%s", script)
	}
}

// 開いている画面として数えるのは、管理画面（ログイン・はじめに・終了の確認を含む）・OAuth の同意画面だけ。公開ページは
// 数えない。パスの先頭が同じでも、区切り（/）で終わらないものは別のページ。
func TestIsAppWindowPage(t *testing.T) {
	e := echo.New()
	for path, want := range map[string]bool{
		"/admin": true, "/admin/": true, "/admin/login": true, "/admin/articles/1/edit": true, SetupPath: true,
		DesktopQuitPath: true, "/setup": true, "/setup/restore": true, OAuthAuthorizePath: true, "/oauth/consent": true,
		"/": false, "/articles/hello": false, "/en/": false, "/en/articles/hello": false, "/categories/news": false,
		"/search": false, "/feed.xml": false, "/about": false, "/administrator": false, "/oauthx": false, "/setups": false,
		"/en/admin": false,
	} {
		c := e.NewContext(httptest.NewRequest(http.MethodGet, path, nil), httptest.NewRecorder())
		if got := isAppWindowPage(c); got != want {
			t.Errorf("isAppWindowPage(%q) = %v, want %v", path, got, want)
		}
	}
	// テーマのプレビューは公開ページを内部の要求（/）で描くが、管理画面のページとして数える。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), themeOverrideKey{}, themeOverride{ID: "tech", Preview: &themePreview{}}))
	if !isAppWindowPage(e.NewContext(req, httptest.NewRecorder())) {
		t.Error("isAppWindowPage(theme preview) = false, want true")
	}
	// エクスポートの --theme（プレビューではない上書き）は公開ページのまま。
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), themeOverrideKey{}, themeOverride{ID: "tech"}))
	if isAppWindowPage(e.NewContext(req, httptest.NewRecorder())) {
		t.Error("isAppWindowPage(export theme override) = true, want false")
	}
}

// 接続の窓口とスクリプトは、デスクトップモードのアプリの画面（管理画面・ログイン・はじめに・テーマのプレビュー）にだけ
// ある。公開ページ（トップ・記事・英語の記事・カテゴリ・検索・見つからないページ）には出さない（別のブラウザで開いた
// 公開ページが、開いている画面として数えられないように）。サーバーモード・公開専用モードでは窓口が 404 で、
// スクリプトも出ない。静的な書き出しにも出さない。
func TestWindowEndpointAndScriptOnlyInDesktopMode(t *testing.T) {
	desktopEnv, _ := newDesktopEnv(t)
	guest := desktopEnv.newClient()
	setup := guest.get(SetupPath)
	expectStatus(t, setup, http.StatusOK)
	expectContains(t, setup.Body, windowScriptMarker, "location.host + '"+WindowPath+"'")
	// ページのスクリプトには、このサイトの合言葉が入る。Cookie には出さない。
	token := desktopEnv.server.windowState().token
	expectContains(t, setup.Body, "&t="+token+"'")
	if strings.Contains(strings.Join(setup.Header.Values("Set-Cookie"), "\n"), token) {
		t.Errorf("the window token is in a cookie: %v", setup.Header.Values("Set-Cookie"))
	}
	expectNotContainsAny(t, "rendered window script", setup.Body, "EventSource")
	c := desktopEnv.completeSetup()
	for _, path := range []string{"/admin", "/admin/articles", "/admin/settings/theme/preview?theme=tech",
		DesktopQuitPath} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, windowScriptMarker)
	}
	login := desktopEnv.newClient().get("/admin/login")
	expectStatus(t, login, http.StatusOK)
	expectContains(t, login.Body, windowScriptMarker)

	// 公開ページ（公開記事・カテゴリを用意する）。管理者としてログインしたままでも出さない。
	var author string
	if err := desktopEnv.db.Get(&author, `SELECT id FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := desktopEnv.db.Exec(`INSERT OR IGNORE INTO categories (id, name, slug) VALUES (1, '未分類', 'uncategorized')`); err != nil {
		t.Fatal(err)
	}
	seedArticle(t, desktopEnv.db, article.Article{AuthorID: author, Title: "公開中", Slug: "window-public",
		ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	for _, viewer := range []*client{desktopEnv.newClient(), c} {
		for path, code := range map[string]int{"/": http.StatusOK, "/articles/window-public": http.StatusOK,
			"/categories/uncategorized": http.StatusOK, "/search?q=" + url.QueryEscape("公開"): http.StatusOK,
			"/no-such-page": http.StatusNotFound} {
			res := viewer.get(path)
			expectStatus(t, res, code)
			expectNotContainsAny(t, "desktop public "+path, res.Body, WindowPath, windowScriptMarker)
		}
	}

	dir, _, err := desktopEnv.server.ExportToTemp(context.Background(), ExportOptions{})
	if err != nil {
		t.Fatalf("ExportToTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	index, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(index), WindowPath) {
		t.Error("the static export contains the window script")
	}

	serverEnv := newTestEnv(t)
	sc := serverEnv.loggedIn(admin)
	expectStatus(t, sc.get(WindowPath), http.StatusNotFound)
	for _, path := range []string{"/admin", "/admin/login", "/"} {
		res := serverEnv.newClient().get(path)
		if path == "/admin" {
			res = sc.get(path)
		}
		expectNotContainsAny(t, "server mode "+path, res.Body, WindowPath)
	}

	publicOnly := newTestEnvWith(t, Config{PublicOnly: true}, nil)
	pc := publicOnly.newClient()
	expectStatus(t, pc.get(WindowPath), http.StatusNotFound)
	expectNotContainsAny(t, "public-only top", pc.get("/").Body, WindowPath)
}

// ページが close のフレームを送ったら（接続そのものはまだ閉じていなくても）、すぐに数から外し、理由を残す。
func TestWindowCloseFrameUncountsAtOnce(t *testing.T) {
	w := newWindowSite(t, nil)
	conn := w.openWindowQuery(t, "page="+url.QueryEscape("/admin/articles"))
	waitForCond(t, "window open", func() bool { return w.server.OpenWindows() == 1 })
	if err := conn.WriteClose(1000); err != nil {
		t.Fatalf("send close frame: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for w.server.OpenWindows() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the window stayed counted after a close frame:\n%s", w.log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitForCond(t, "disconnect logged", func() bool {
		return strings.Contains(w.log.String(), "window: disconnected (0 open) /admin/articles: closed by the page")
	})
	expectContains(t, w.log.String(), "window: connected (1 open) /admin/articles")
}

// ping に pong を返さない（応えない）相手は、何も届かないまま windowReadWait を過ぎたら切って数から外す。
func TestWindowDropsUnresponsivePage(t *testing.T) {
	setWindowTimings(t, 30*time.Millisecond, 200*time.Millisecond)
	w := newWindowSite(t, nil)
	// 読まない接続は、届いた ping に pong を返さない。
	w.openWindow(t)
	waitForCond(t, "window open", func() bool { return w.server.OpenWindows() == 1 })
	waitForCond(t, "unresponsive window dropped", func() bool { return w.server.OpenWindows() == 0 })
	waitForCond(t, "drop logged", func() bool {
		return strings.Contains(w.log.String(), "window: disconnected (0 open): no response from the page for 200ms")
	})
}

// 動作の記録に残すページのパスは、/ で始まるものだけ。表示できない文字（改行など）は ? に置き換え、長すぎれば切る。
func TestWindowPageForLog(t *testing.T) {
	long := "/" + strings.Repeat("a", windowPageMax+50)
	for raw, want := range map[string]string{
		"":                          "",
		"/admin/articles":           "/admin/articles",
		"/admin/%E8%A8%98%E4%BA%8B": "/admin/%E8%A8%98%E4%BA%8B",
		"admin":                     "",
		"https://evil.example/":     "",
		"/admin\nwindow: fake":      "/admin?window:?fake",
		"/記事":                       "/??????",
		long:                        long[:windowPageMax],
	} {
		req := httptest.NewRequest(http.MethodGet, WindowPath+"?page="+url.QueryEscape(raw), nil)
		if got := windowPage(req); got != want {
			t.Errorf("windowPage(%q) = %q, want %q", raw, got, want)
		}
	}
}

// 接続のパスの先頭部分は、固定ページのスラッグに使えない。
func TestWindowPathIsReservedSlug(t *testing.T) {
	if got := article.PathReservedSlug(WindowPath); !article.IsReservedPageSlug(got) {
		t.Fatalf("%q (from %s) is not a reserved page slug", got, WindowPath)
	}
	env, _ := newDesktopEnv(t)
	err := env.server.articles.Save(&article.Article{Kind: article.KindPage, Title: "窓", Slug: article.PathReservedSlug(WindowPath),
		AuthorID: "x", CategoryID: 1})
	if !errors.Is(err, article.ErrReservedSlug) {
		t.Fatalf("Save page with slug %q: err = %v, want ErrReservedSlug", article.PathReservedSlug(WindowPath), err)
	}
}

// pendingSite は PendingScheduledReflects を確かめるための、予約投稿の自動の確認を止めたデスクトップモードのサイト。
// 同じ DB のファイルで起動し直せる。
type pendingSite struct {
	t      *testing.T
	dir    string
	db     *sqlx.DB
	server *Server
}

func newPendingSite(t *testing.T) *pendingSite {
	t.Helper()
	p := &pendingSite{t: t, dir: t.TempDir()}
	p.start()
	t.Cleanup(p.stop)
	if _, err := p.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w', 'writer0', 'w', 'x', 'admin')`); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *pendingSite) start() {
	p.t.Helper()
	db, err := database.Connect(filepath.Join(p.dir, "site.db"))
	if err != nil {
		p.t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		p.t.Fatal(err)
	}
	s, err := New(Config{DB: db, UploadDir: filepath.Join(p.dir, "uploads"), AttachmentDir: filepath.Join(p.dir, "attachments"),
		Desktop: true, DisableScheduler: true, LogOutput: io.Discard})
	if err != nil {
		p.t.Fatal(err)
	}
	p.db, p.server = db, s
}

func (p *pendingSite) stop() {
	if p.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.server.Shutdown(ctx); err != nil {
		p.t.Errorf("Shutdown: %v", err)
	}
	p.db.Close()
	p.server, p.db = nil, nil
}

// restart は sorus を閉じて、同じデータで起動し直す。
func (p *pendingSite) restart() {
	p.t.Helper()
	p.stop()
	p.start()
}

// useFolderTarget はフォルダの公開先を決める。siteURL が空なら公開サイトURLを入れない（反映は失敗する）。
func (p *pendingSite) useFolderTarget(siteURL string) string {
	p.t.Helper()
	out := filepath.Join(p.dir, "export")
	b, _ := json.Marshal(publishFolderConfig{Path: out})
	for k, v := range map[string]string{publishSettingKind: publishKindFolder, publishSettingFolder: string(b), "base_url": siteURL} {
		if err := upsertSetting(p.db, k, v); err != nil {
			p.t.Fatal(err)
		}
	}
	p.server.cfg.BaseURL = siteURL
	return out
}

func (p *pendingSite) expectPending(step string, wantN int, wantLast time.Time) {
	p.t.Helper()
	n, last, err := p.server.PendingScheduledReflects()
	if err != nil {
		p.t.Fatalf("%s: PendingScheduledReflects: %v", step, err)
	}
	if n != wantN || !last.Equal(wantLast) {
		p.t.Fatalf("%s: PendingScheduledReflects = %d, %v; want %d, %v", step, n, last, wantN, wantLast)
	}
}

// reflect は「サイトに反映」を1回行い、結果を返す。
func (p *pendingSite) reflect() publishOutcome {
	p.t.Helper()
	if !p.server.startReflect(publishTriggerManual) {
		p.t.Fatal("reflect did not start")
	}
	return waitReflectDone(p.t, p.server)
}

// 公開先が決まっていなければ、予約があっても 0 とゼロ時刻を返す。
func TestPendingScheduledReflectsWithoutTarget(t *testing.T) {
	p := newPendingSite(t)
	seedScheduled(t, p.db, article.Article{AuthorID: "w", Title: "予約", Slug: "future", ContentMarkdown: "本文"},
		time.Now().UTC().Add(time.Hour).Truncate(time.Second))
	p.expectPending("no target", 0, time.Time{})
}

// 予約中の記事を数え、最も遅い公開日時を返す。公開日時を迎えた予約だった記事は、反映に成功するまで数えたままにし、
// 成功したら数えない。起動し直しても同じ答えになる。すぐに公開した記事は数えない。
func TestPendingScheduledReflectsUntilReflectSucceeds(t *testing.T) {
	p := newPendingSite(t)
	now := time.Now().UTC().Truncate(time.Second)
	setClock(t, now)
	p.useFolderTarget("")
	seedArticle(t, p.db, article.Article{AuthorID: "w", Title: "すぐに公開", Slug: "now", ContentMarkdown: "本文",
		Status: article.StatusPublished, PublishedAt: &now})
	early, late := now.Add(time.Hour), now.Add(3*time.Hour)
	seedScheduled(t, p.db, article.Article{AuthorID: "w", Title: "先の予約", Slug: "early", ContentMarkdown: "本文"}, early)
	seedScheduled(t, p.db, article.Article{AuthorID: "w", Title: "後の予約", Slug: "late", ContentMarkdown: "本文"}, late)
	p.expectPending("two scheduled", 2, late)

	// 先の予約が公開日時を迎えた。まだ反映していないため数えたまま。
	setClock(t, now.Add(2*time.Hour))
	p.expectPending("one due, not reflected", 2, late)
	// 反映が失敗した（公開サイトURLが無い）ときも数えたまま。起動し直しても同じ。
	if out := p.reflect(); out.OK {
		t.Fatalf("reflect without a site URL succeeded: %+v", out)
	}
	p.expectPending("after a failed reflect", 2, late)
	p.restart()
	p.expectPending("failed reflect, after restart", 2, late)

	// 反映に成功したら、公開日時を迎えた記事は数えない。予約中の記事は数えたまま。
	out := p.useFolderTarget(testBaseURL)
	if res := p.reflect(); !res.OK {
		t.Fatalf("reflect failed: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(out, "articles", "early", "index.html")); err != nil {
		t.Fatalf("the due article was not exported: %v", err)
	}
	p.expectPending("after a successful reflect", 1, late)
	p.restart()
	p.expectPending("successful reflect, after restart", 1, late)

	// 後の予約も公開日時を迎え、反映したら 0。
	setClock(t, now.Add(4*time.Hour))
	p.expectPending("second due", 1, late)
	if res := p.reflect(); !res.OK {
		t.Fatalf("second reflect failed: %+v", res)
	}
	p.expectPending("all reflected", 0, time.Time{})
}

// 最後に成功した反映を始めた時刻を保存する前の版で反映していたときは、最後の反映が成功していればその終わった時刻を使う。
func TestPendingScheduledReflectsFallsBackToLastOutcome(t *testing.T) {
	p := newPendingSite(t)
	now := time.Now().UTC().Truncate(time.Second)
	setClock(t, now)
	p.useFolderTarget(testBaseURL)
	due := now.Add(-2 * time.Hour)
	seedScheduled(t, p.db, article.Article{AuthorID: "w", Title: "過ぎた予約", Slug: "due", ContentMarkdown: "本文"}, due)
	p.expectPending("never reflected", 1, due)
	if err := p.server.saveLastOutcome(publishOutcome{OK: true, FinishedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	p.expectPending("reflected by an older version", 0, time.Time{})
	if err := p.server.saveLastOutcome(publishOutcome{OK: false, FinishedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	p.expectPending("last reflect failed", 1, due)
}

// Busy は反映かバックアップが動いている間 true。
func TestBusyDuringReflectOrBackup(t *testing.T) {
	p := newPendingSite(t)
	s := p.server
	if s.Busy() {
		t.Fatal("Busy with nothing running")
	}
	st := s.publishState()
	st.mu.Lock()
	st.running = true
	st.mu.Unlock()
	if !s.Busy() {
		t.Error("Busy = false during a reflect")
	}
	st.mu.Lock()
	st.running = false
	st.mu.Unlock()

	s.backupMu.RLock()
	if !s.Busy() {
		t.Error("Busy = false while a backup is being created")
	}
	s.backupMu.RUnlock()
	s.backupMu.Lock()
	if !s.Busy() {
		t.Error("Busy = false while a backup is being restored")
	}
	s.backupMu.Unlock()
	if s.Busy() {
		t.Error("Busy after everything finished")
	}
}

// 本物の反映（フォルダへの書き出し）の間も Busy は true。
func TestBusyDuringRealReflect(t *testing.T) {
	p := newPendingSite(t)
	p.useFolderTarget(testBaseURL)
	// 書き出しはバックアップの読み取りロックの中で行うため、書き込みを持って反映を書き出しの手前で待たせる。
	p.server.backupMu.Lock()
	if !p.server.startReflect(publishTriggerManual) {
		t.Fatal("reflect did not start")
	}
	if !p.server.Busy() {
		t.Error("Busy = false while a reflect is running")
	}
	p.server.backupMu.Unlock()
	if out := waitReflectDone(t, p.server); !out.OK {
		t.Fatalf("reflect failed: %+v", out)
	}
	if p.server.Busy() {
		t.Error("Busy after the reflect finished")
	}
}

// seedPendingDesktop はデスクトップモードのサイトに、フォルダの公開先と予約中の記事を1件用意し、その公開日時を返す。
func seedPendingDesktop(t *testing.T, env *testEnv) time.Time {
	t.Helper()
	b, _ := json.Marshal(publishFolderConfig{Path: filepath.Join(t.TempDir(), "export")})
	for k, v := range map[string]string{publishSettingKind: publishKindFolder, publishSettingFolder: string(b), "base_url": testBaseURL} {
		if err := upsertSetting(env.db, k, v); err != nil {
			t.Fatal(err)
		}
	}
	var author string
	if err := env.db.Get(&author, `SELECT id FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT OR IGNORE INTO categories (id, name, slug) VALUES (1, '未分類', 'uncategorized')`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2030, 11, 3, 0, 0, 0, 0, time.UTC) // 日本時間の 11月3日 9:00
	seedScheduled(t, env.db, article.Article{AuthorID: author, Title: "予約", Slug: "future", ContentMarkdown: "本文"}, at)
	return at
}

// 「sorus を終了」は、まだ反映していない予約が無ければ今のまま（その場の確認と POST）。予約があれば、終わらせ方を選ぶ
// 確認の画面を開く。
func TestDesktopQuitMenuDependsOnPendingReflects(t *testing.T) {
	env, quits, finishes := newDesktopEnvQuits(t)
	c := env.completeSetup()
	menu := userMenu(t, c.get("/admin").Body)
	expectContains(t, menu, `action="/admin/desktop/quit" id="nav-desktop-quit" data-confirm="sorus を終了しますか？"`)
	expectNotContainsAny(t, "menu without pending", menu, `href="/admin/desktop/quit"`)

	seedPendingDesktop(t, env)
	menu = userMenu(t, c.get("/admin").Body)
	expectContains(t, menu, `<a href="/admin/desktop/quit" id="nav-desktop-quit"`, "sorus を終了</a>")
	expectNotContainsAny(t, "menu with pending", menu, `data-confirm="sorus を終了しますか？"`, `action="/admin/desktop/quit"`)

	page := c.get(DesktopQuitPath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "sorus を終了しますか？",
		"まだサイトに反映していない予約の記事が 1 件あります（最後は 11月3日 09:00）",
		`name="_csrf"`, `name="finish" value="1" id="quit-finish-first">予約をすべて反映してから終了する</button>`,
		`name="finish" value="0" id="quit-now" class="secondary">今すぐ終了する</button>`, `id="quit-cancel"`, ">やめる</a>",
		windowScriptMarker)
	expectNotContainsAny(t, "confirm page", page.Body, "ポート", "インスタンス", "サーバー", "cdn.tailwindcss.com")
	if quits.Load() != 0 {
		t.Fatal("opening the confirm page quit sorus")
	}

	// CSRF のトークンが無ければ終了しない。
	expectStatus(t, c.postRaw(DesktopQuitPath, nil), http.StatusForbidden)
	if quits.Load() != 0 {
		t.Fatal("quit without a CSRF token")
	}

	later := c.postForm(DesktopQuitPath, url.Values{"finish": {"1"}})
	expectStatus(t, later, http.StatusOK)
	expectContains(t, later.Body, "予約をすべて反映したら、sorus を終了します", "このウィンドウは閉じてかまいません。")
	expectNotContainsAny(t, "finish-first page", later.Body, WindowPath, "ポート", "インスタンス", "サーバー")
	if quits.Load() != 1 || finishes.Load() != 1 {
		t.Fatalf("finish=1: OnQuit calls %d (finishFirst %d), want 1 (1)", quits.Load(), finishes.Load())
	}

	now := c.postForm(DesktopQuitPath, url.Values{"finish": {"0"}})
	expectStatus(t, now, http.StatusOK)
	expectContains(t, now.Body, "終了しました。このタブは閉じてかまいません。")
	if quits.Load() != 2 || finishes.Load() != 1 {
		t.Fatalf("finish=0: OnQuit calls %d (finishFirst %d), want 2 (1)", quits.Load(), finishes.Load())
	}
	// 今までの「sorus を終了」（finish なし）は今すぐ終了する。
	c.postForm(DesktopQuitPath, nil)
	if quits.Load() != 3 || finishes.Load() != 1 {
		t.Fatalf("no finish: OnQuit calls %d (finishFirst %d), want 3 (1)", quits.Load(), finishes.Load())
	}
}

// 英語の表示では、確認の画面も英語で出す。
func TestDesktopQuitConfirmInEnglish(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	seedPendingDesktop(t, env)
	c.get("/admin/set-lang?lang=en&return=/admin")
	page := c.get(DesktopQuitPath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `<html lang="en">`, "Quit sorus?",
		"1 scheduled article(s) are not on your site yet (the last one at Nov 3 09:00)",
		"Send all scheduled articles, then quit", "Quit now", "Cancel")
	later := c.postForm(DesktopQuitPath, url.Values{"finish": {"1"}})
	expectContains(t, later.Body, "sorus will quit after sending all scheduled articles", "You can close this window.")
}

// 確認の画面は、予約が無くなっていれば「今すぐ終了する」と「やめる」だけを出す。サーバーモードには無い。
func TestDesktopQuitConfirmWithoutPending(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	page := c.get(DesktopQuitPath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="quit-now">今すぐ終了する</button>`, ">やめる</a>")
	expectNotContainsAny(t, "confirm without pending", page.Body, "quit-finish-first", "quit-pending")

	serverEnv := newTestEnv(t)
	expectStatus(t, serverEnv.loggedIn(admin).get(DesktopQuitPath), http.StatusNotFound)
}

// 「新しいサイトを作る」は同じウィンドウで開く（target を付けない）。
func TestSiteCreateFormOpensInSameWindow(t *testing.T) {
	sites := &fakeSites{sites: []SiteInfo{{ID: "", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Current: true}}}
	_, c := newSitesDesktopClient(t, sites, "")
	body := c.get(sitesPagePath).Body
	start := strings.Index(body, `id="site-create"`)
	if start < 0 {
		t.Fatal("no site creation section")
	}
	section := body[start:]
	section = section[:strings.Index(section, "</section>")]
	expectContains(t, section, `<form method="post" action="/admin/settings/sites"`)
	if strings.Contains(section, "target=") || strings.Contains(section, "新しいタブ") {
		t.Errorf("site creation form opens elsewhere: %s", section)
	}
}
