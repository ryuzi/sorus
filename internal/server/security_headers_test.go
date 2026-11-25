package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"sorus/internal/database"
)

// security_hardening_spec.md §2.4・§2.5・§2.3 の一部（待ち受けアドレス以外）の試験。

// cspDirectives は CSP をディレクティブ名から値への対応にする。
func cspDirectives(csp string) map[string]string {
	out := map[string]string{}
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		name, value, _ := strings.Cut(d, " ")
		out[name] = value
	}
	return out
}

func TestReferrerPolicyAndBasicHeadersOnEveryResponse(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	for _, path := range []string{"/", "/admin/login", "/healthz", "/robots.txt", "/no-such-page", "/static/js/admin.js"} {
		res := c.get(path)
		if got := res.Header.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
			t.Errorf("%s: Referrer-Policy = %q, want strict-origin-when-cross-origin", path, got)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
		if got := res.Header.Get("X-Frame-Options"); got != "SAMEORIGIN" {
			t.Errorf("%s: X-Frame-Options = %q, want SAMEORIGIN", path, got)
		}
		// 平文で SecureCookie も無いサーバーは HSTS を付けない。
		if got := res.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("%s: Strict-Transport-Security = %q over plain http, want none", path, got)
		}
	}

	// 公開専用モードの /admin は、ルーティングの前（Pre）に公開サイトの 404 で返す。その応答にも付ける。
	pub := newTestEnvWith(t, Config{PublicOnly: true}, nil)
	res := pub.newClient().get("/admin/login")
	expectStatus(t, res, http.StatusNotFound)
	if got := res.Header.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("public-only /admin: Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
}

func TestHSTSOnlyWhenServingHTTPS(t *testing.T) {
	const want = "max-age=31536000"
	for _, tc := range []struct {
		name   string
		secure bool
		tls    bool
		want   string
	}{
		{"https with SecureCookie", true, true, want},
		{"https without SecureCookie", false, true, ""},
		// HSTS は平文の応答には付けない（ブラウザも平文の HSTS は無視する）。
		{"plain http with SecureCookie", true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startTestEnv(t, Config{SecureCookie: tc.secure}, nil, tc.tls)
			res := env.newClient().get("/")
			expectStatus(t, res, http.StatusOK)
			if got := res.Header.Get("Strict-Transport-Security"); got != tc.want {
				t.Errorf("Strict-Transport-Security = %q, want %q", got, tc.want)
			}
			if got := res.Header.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
				t.Errorf("Referrer-Policy = %q", got)
			}
		})
	}
}

func TestAdminCSPOnAdminPages(t *testing.T) {
	env := newTestEnv(t)
	staff := env.loggedIn(admin)
	for _, path := range []string{"/admin/login", "/admin", "/admin/articles", "/admin/no-such-page"} {
		c := staff
		if path == "/admin/login" {
			c = env.newClient()
		}
		res := c.get(path)
		csp := res.Header.Get("Content-Security-Policy")
		d := cspDirectives(csp)
		for name, want := range map[string]string{
			"default-src":     "'self'",
			"base-uri":        "'self'",
			"form-action":     "'self'",
			"frame-ancestors": "'self'",
			"object-src":      "'none'",
			"connect-src":     "'self'",
			"script-src":      "'self' 'unsafe-inline'",
			"style-src":       "'self' 'unsafe-inline'",
			"img-src":         "'self' data: blob: https:",
			"frame-src":       "'self' https://www.youtube-nocookie.com",
		} {
			if d[name] != want {
				t.Errorf("%s (status %d): %s = %q, want %q\nCSP: %s", path, res.StatusCode, name, d[name], want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: admin CSP allows eval: %s", path, csp)
		}
	}

	// 公開ページは公開用の CSP のまま（管理画面の CSP にしない）。
	if got := env.newClient().get("/").Header.Get("Content-Security-Policy"); got != DefaultPublicCSP {
		t.Errorf("public page CSP = %q, want DefaultPublicCSP", got)
	}
}

func TestAdminCSPOnOAuthConsentAllowsRedirectTargets(t *testing.T) {
	env := newTestEnv(t)
	clientID := env.registerOAuthClient("Claude", oauthTestRedirect)
	res := env.loggedIn(admin).get(authorizeQuery(clientID, nil))
	expectStatus(t, res, http.StatusOK)
	if !strings.Contains(res.Body, `name="_csrf"`) {
		t.Fatalf("consent form not rendered: %.500s", res.Body)
	}
	d := cspDirectives(res.Header.Get("Content-Security-Policy"))
	// 「許可する」の後の 302 は AI アプリの戻り先へ進むため、form-action はその戻り先（https と、この PC の http）を許す。
	if got, want := d["form-action"], "'self' https: http://localhost:* http://127.0.0.1:*"; got != want {
		t.Errorf("oauth consent form-action = %q, want %q", got, want)
	}
	if d["object-src"] != "'none'" || d["frame-ancestors"] != "'self'" || d["img-src"] != "'self' data: blob: https:" {
		t.Errorf("oauth consent page does not use the admin CSP: %v", d)
	}
	// 公開ページ用の CSP（画像の許可が違う）を先に付けない。
	if strings.Contains(res.Header.Get("Content-Security-Policy"), "img-src 'self' data: https:;") {
		t.Errorf("oauth consent page got the public CSP: %s", res.Header.Get("Content-Security-Policy"))
	}
}

func TestAdminCSPFormAction(t *testing.T) {
	srv := &Server{}
	desktop := &Server{cfg: Config{Desktop: true}}
	formAction := func(s *Server, target string) (string, bool) {
		v, ok := cspDirectives(s.adminCSP(httptest.NewRequest(http.MethodGet, target, nil)))["form-action"]
		return v, ok
	}
	for _, tc := range []struct {
		name   string
		s      *Server
		target string
		want   string
	}{
		{"server admin", srv, "/admin/articles", "'self'"},
		{"server setup", srv, "/setup/x", "'self'"},
		// デスクトップモードの「サイト」画面は、ほかのサイトの管理画面（この PC の別のポート）へ 303 で送る。
		{"desktop admin", desktop, "/admin/settings/sites", "'self' http://localhost:* http://127.0.0.1:*"},
		{"oauth", srv, "/oauth/authorize?redirect_uri=http%3A%2F%2F127.0.0.1%3A3334%2Fcb", "'self' https: http://localhost:* http://127.0.0.1:*"},
		{"desktop oauth", desktop, "/oauth/authorize", "'self' https: http://localhost:* http://127.0.0.1:*"},
	} {
		got, ok := formAction(tc.s, tc.target)
		if !ok || got != tc.want {
			t.Errorf("%s: form-action = %q (present %v), want %q", tc.name, got, ok, tc.want)
		}
	}
	// 戻り先が http://[::1] なら CSP に書けないため、その画面に限り form-action を付けない。
	if got, ok := formAction(srv, "/oauth/authorize?redirect_uri=http%3A%2F%2F%5B%3A%3A1%5D%3A3334%2Fcb"); ok {
		t.Errorf("oauth with [::1] redirect: form-action = %q, want none", got)
	}
	req := httptest.NewRequest(http.MethodPost, OAuthAuthorizePath, strings.NewReader("redirect_uri=http%3A%2F%2F%5B%3A%3A1%5D%2Fcb"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if _, ok := cspDirectives(srv.adminCSP(req))["form-action"]; ok {
		t.Error("oauth POST page with [::1] redirect: form-action present, want none")
	}
}

func TestAdminCSPKeepsHandlerCSP(t *testing.T) {
	s := &Server{}
	e := echo.New()
	e.Use(s.adminCSPMiddleware())
	e.GET("/admin/attachments/:id/download", func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderContentSecurityPolicy, "default-src 'none'; sandbox")
		return c.String(http.StatusOK, "file")
	})
	e.GET("/admin/x", func(c echo.Context) error { return c.String(http.StatusOK, "x") })
	e.GET("/public", func(c echo.Context) error { return c.String(http.StatusOK, "p") })

	serve := func(path string) string {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header().Get(echo.HeaderContentSecurityPolicy)
	}
	if got := serve("/admin/attachments/a/download"); got != "default-src 'none'; sandbox" {
		t.Errorf("handler CSP was replaced: %q", got)
	}
	if got := serve("/admin/x"); !strings.Contains(got, "object-src 'none'") {
		t.Errorf("admin CSP missing: %q", got)
	}
	if got := serve("/public"); got != "" {
		t.Errorf("admin CSP added to a public path: %q", got)
	}
}

func TestDesktopAdminCSP(t *testing.T) {
	env, _ := newDesktopEnv(t)
	res := env.newClient().get(SetupPath)
	expectStatus(t, res, http.StatusOK)
	d := cspDirectives(res.Header.Get("Content-Security-Policy"))
	if got, want := d["form-action"], "'self' http://localhost:* http://127.0.0.1:*"; got != want {
		t.Errorf("desktop setup form-action = %q, want %q", got, want)
	}
	if d["connect-src"] != "'self'" {
		t.Errorf("desktop setup connect-src = %q, want 'self'", d["connect-src"])
	}
}

func TestIsAdminCSPPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/admin": true, "/admin/": true, "/admin/setup": true, "/setup": true, "/setup/a": true,
		"/oauth/authorize": true, "/oauth": true, "/oauthx": false, "/": false, "/articles/x": false,
		"/administrator": false, "/uploads/a.svg": false, "/static/../admin/x": true,
	} {
		if got := isAdminCSPPath(p); got != want {
			t.Errorf("isAdminCSPPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestUploadsCSPSandboxesFiles(t *testing.T) {
	env := newTestEnv(t)
	env.writeUpload("2026/11/evil.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.cookie)</script></svg>`)
	env.writeUpload("2026/11/photo.webp", "RIFF0000WEBP")
	c := env.newClient()
	for _, path := range []string{"/uploads/2026/11/evil.svg", "/uploads/2026/11/photo.webp"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		csp := res.Header.Get("Content-Security-Policy")
		if csp != uploadCSP {
			t.Errorf("%s: CSP = %q, want %q", path, csp, uploadCSP)
		}
		d := cspDirectives(csp)
		if _, ok := d["sandbox"]; !ok || d["default-src"] != "'none'" {
			t.Errorf("%s: CSP does not sandbox the file: %q", path, csp)
		}
	}
	// HEAD にも付ける。
	req, err := http.NewRequest(http.MethodHead, env.srv.URL+"/uploads/2026/11/evil.svg", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := c.do(req); res.Header.Get("Content-Security-Policy") != uploadCSP {
		t.Errorf("HEAD svg: CSP = %q", res.Header.Get("Content-Security-Policy"))
	}
	// 存在しないファイルは公開サイトの 404 ページ（テーマの CSS を読み込む）を崩さない。
	res := c.get("/uploads/2026/11/missing.svg")
	expectStatus(t, res, http.StatusNotFound)
	if strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("404 page got the uploads sandbox CSP: %q", res.Header.Get("Content-Security-Policy"))
	}
}

func TestPublicCSPHardened(t *testing.T) {
	tag := `<script defer src="https://static.cloudflareinsights.com/beacon.min.js"></script>`
	for name, csp := range map[string]string{"default": DefaultPublicCSP, "analytics": buildPublicCSP(tag)} {
		d := cspDirectives(csp)
		for directive, want := range map[string]string{
			"base-uri":        "'self'",
			"form-action":     "'self'",
			"object-src":      "'none'",
			"img-src":         "'self' data: https:",
			"default-src":     "'self'",
			"frame-ancestors": "'self'",
			"frame-src":       "'self' https://www.youtube-nocookie.com",
		} {
			if d[directive] != want {
				t.Errorf("%s: %s = %q, want %q\nCSP: %s", name, directive, d[directive], want, csp)
			}
		}
		if strings.Contains(csp, " http:") {
			t.Errorf("%s: CSP still allows plain http: %s", name, csp)
		}
	}
	if d := cspDirectives(buildPublicCSP(tag)); !strings.Contains(d["script-src"], "https://static.cloudflareinsights.com") {
		t.Errorf("analytics CSP lost the script origin: %v", d)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies([]string{" 127.0.0.1 ", "", "10.1.2.3/8", "::1", "::ffff:192.0.2.9", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("192.0.2.9/32"), netip.MustParsePrefix("2001:db8::/32"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("ParseTrustedProxies = %v, want %v", got, want)
	}
	for _, bad := range []string{"proxy.example", "10.0.0.0/33", "fe80::1%eth0", "300.1.1.1"} {
		if _, err := ParseTrustedProxies([]string{bad}); err == nil {
			t.Errorf("ParseTrustedProxies(%q) succeeded, want error", bad)
		}
	}
}

func TestIPExtractorIgnoresForwardedHeadersByDefault(t *testing.T) {
	req := func(remote string, header map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		for k, v := range header {
			r.Header.Set(k, v)
		}
		return r
	}
	spoof := map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Real-IP": "198.51.100.8"}

	direct := newIPExtractor(nil)
	for _, remote := range []string{"203.0.113.5:4000", "127.0.0.1:4000", "10.0.0.2:4000"} {
		want, _, _ := strings.Cut(remote, ":")
		if got := direct(req(remote, spoof)); got != want {
			t.Errorf("no trusted proxies, from %s: RealIP = %q, want %q (the forwarded headers must be ignored)", remote, got, want)
		}
	}

	trusted := newIPExtractor([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	for _, tc := range []struct {
		remote, xff, want string
	}{
		// 信頼したプロキシが付け足した値（右端に近い、信頼していない最初のアドレス）を使う。
		{"10.0.0.2:4000", "198.51.100.7", "198.51.100.7"},
		// クライアントが自分で書いた先頭の値は使わない。
		{"10.0.0.2:4000", "192.0.2.66, 198.51.100.7", "198.51.100.7"},
		{"10.0.0.2:4000", "192.0.2.66, 198.51.100.7, 10.9.9.9", "198.51.100.7"},
		// 信頼していない送り元の X-Forwarded-For は使わない（ループバック・私設アドレスも、指定しなければ信頼しない）。
		{"203.0.113.5:4000", "198.51.100.7", "203.0.113.5"},
		{"127.0.0.1:4000", "198.51.100.7", "127.0.0.1"},
		{"192.168.1.1:4000", "198.51.100.7", "192.168.1.1"},
	} {
		if got := trusted(req(tc.remote, map[string]string{"X-Forwarded-For": tc.xff})); got != tc.want {
			t.Errorf("trusted 10.0.0.0/8, from %s with XFF %q: RealIP = %q, want %q", tc.remote, tc.xff, got, tc.want)
		}
	}
}

func TestServerRealIPUsesConfiguredExtractor(t *testing.T) {
	realIP := func(env *testEnv, remote, xff string) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", xff)
		return env.server.echo.NewContext(r, httptest.NewRecorder()).RealIP()
	}
	if got := realIP(newTestEnv(t), "203.0.113.5:4000", "198.51.100.7"); got != "203.0.113.5" {
		t.Errorf("default server: RealIP = %q, want the direct peer 203.0.113.5", got)
	}
	env := newTestEnvWith(t, Config{TrustedProxies: []string{"10.0.0.0/8"}}, nil)
	if got := realIP(env, "10.0.0.2:4000", "198.51.100.7"); got != "198.51.100.7" {
		t.Errorf("TrustedProxies server: RealIP = %q, want 198.51.100.7", got)
	}
	if got := realIP(env, "203.0.113.5:4000", "198.51.100.7"); got != "203.0.113.5" {
		t.Errorf("TrustedProxies server, untrusted peer: RealIP = %q, want 203.0.113.5", got)
	}
}

func TestNewRejectsInvalidHostAndProxyConfig(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Connect(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]Config{
		"allowed host":  {AllowedHosts: []string{"exa mple.jp"}},
		"allowed url":   {AllowedHosts: []string{"https://example.jp/path"}},
		"trusted proxy": {TrustedProxies: []string{"proxy.example"}},
	} {
		cfg.DB, cfg.UploadDir, cfg.AttachmentDir, cfg.ThemesDir = db, filepath.Join(dir, "u"), filepath.Join(dir, "a"), filepath.Join(dir, "t")
		if s, err := New(cfg); err == nil {
			s.Shutdown(t.Context())
			t.Errorf("%s: New succeeded, want error", name)
		}
	}
}

func TestNormalizeAllowedHosts(t *testing.T) {
	got, err := normalizeAllowedHosts([]string{" Media.Example.JP ", "", "https://www.example.jp/", "example.jp:8443", "[::1]:8080", "[::1]", "media.example.jp.", "media.example.jp"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"media.example.jp", "www.example.jp", "example.jp:8443", "[::1]:8080", "[::1]"}
	if !slices.Equal(got, want) {
		t.Errorf("normalizeAllowedHosts = %q, want %q", got, want)
	}
	for _, bad := range []string{"exa mple.jp", "example.jp:http", "https://user@example.jp", "https://example.jp/a", "example.jp/a", "<x>"} {
		if _, err := normalizeAllowedHosts([]string{bad}); err == nil {
			t.Errorf("normalizeAllowedHosts(%q) succeeded, want error", bad)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allowed := []string{"media.example.jp", "www.example.jp:8443", "[::1]"}
	for host, want := range map[string]bool{
		"media.example.jp": true, "MEDIA.example.jp:8080": true, "media.example.jp.": true,
		"www.example.jp:8443": true, "www.example.jp": false, "www.example.jp:9999": false,
		"[::1]:8080": true, "evil.example": false, "media.example.jp.evil.example": false, "": false,
	} {
		if got := hostAllowed(host, allowed); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// robotsSitemap は Host（と追加のヘッダー）を指定して robots.txt を取り、サイトマップの URL と Vary を返す。
func robotsSitemap(t *testing.T, env *testEnv, host string, header map[string]string) (sitemap string, vary []string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, env.srv.URL+"/robots.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res := env.newClient().do(req)
	expectStatus(t, res, http.StatusOK)
	_, after, ok := strings.Cut(res.Body, "Sitemap: ")
	if !ok {
		t.Fatalf("robots.txt has no Sitemap line: %s", res.Body)
	}
	return strings.TrimSpace(after), res.Header.Values("Vary")
}

func TestBaseURLValidatesHostAgainstAllowedHosts(t *testing.T) {
	// ALLOWED_HOSTS も BASE_URL も無ければ、従来どおり Host から組み立てる（そのぶん Host ごとにキャッシュを分ける）。
	open := newTestEnv(t)
	if got, _ := robotsSitemap(t, open, "lan.example:8080", nil); got != "http://lan.example:8080/sitemap.xml" {
		t.Errorf("no allowed hosts: sitemap = %q", got)
	}

	env := newTestEnvWith(t, Config{AllowedHosts: []string{"media.example.jp", "www.example.jp:8443"}}, nil)
	for host, want := range map[string]string{
		"media.example.jp":      "http://media.example.jp/sitemap.xml",
		"media.example.jp:8080": "http://media.example.jp:8080/sitemap.xml",
		"www.example.jp:8443":   "http://www.example.jp:8443/sitemap.xml",
		// 一覧に無い Host（偽の Host）は使わず、先頭の値で組み立てる。
		"evil.example":        "http://media.example.jp/sitemap.xml",
		"www.example.jp:9999": "http://media.example.jp/sitemap.xml",
	} {
		if got, _ := robotsSitemap(t, env, host, nil); got != want {
			t.Errorf("Host %q: sitemap = %q, want %q", host, got, want)
		}
	}

	// BASE_URL があれば、Host と ALLOWED_HOSTS より優先する。
	based := newTestEnvWith(t, Config{BaseURL: "https://news.example.jp/", AllowedHosts: []string{"media.example.jp"}}, nil)
	if got, _ := robotsSitemap(t, based, "media.example.jp", nil); got != "https://news.example.jp/sitemap.xml" {
		t.Errorf("BaseURL: sitemap = %q", got)
	}
}

func TestBaseURLSchemeTrustsOnlyConfiguredProxies(t *testing.T) {
	https := map[string]string{"X-Forwarded-Proto": "https"}
	// 信頼したプロキシが無ければ、誰でも書ける X-Forwarded-Proto でスキームを変えさせない。
	env := newTestEnv(t)
	if got, _ := robotsSitemap(t, env, "media.example.jp", https); got != "http://media.example.jp/sitemap.xml" {
		t.Errorf("untrusted X-Forwarded-Proto: sitemap = %q, want http", got)
	}
	// 試験のサーバーには 127.0.0.1 からつながるため、それを信頼したプロキシにすると従う。
	proxied := newTestEnvWith(t, Config{TrustedProxies: []string{"127.0.0.1", "::1"}}, nil)
	if got, _ := robotsSitemap(t, proxied, "media.example.jp", https); got != "https://media.example.jp/sitemap.xml" {
		t.Errorf("trusted X-Forwarded-Proto: sitemap = %q, want https", got)
	}
	if got, _ := robotsSitemap(t, proxied, "media.example.jp", nil); got != "http://media.example.jp/sitemap.xml" {
		t.Errorf("trusted proxy without X-Forwarded-Proto: sitemap = %q, want http", got)
	}
	// TLS で直接届いた要求は https。
	tlsEnv := startTestEnv(t, Config{}, nil, true)
	if got, _ := robotsSitemap(t, tlsEnv, "media.example.jp", nil); got != "https://media.example.jp/sitemap.xml" {
		t.Errorf("direct TLS: sitemap = %q, want https", got)
	}
}

func TestPublicHTMLCacheVaryHostWithoutBaseURL(t *testing.T) {
	hasHost := func(vary []string) bool {
		for _, v := range vary {
			for _, f := range strings.Split(v, ",") {
				if strings.EqualFold(strings.TrimSpace(f), "Host") {
					return true
				}
			}
		}
		return false
	}
	env := newTestEnv(t)
	if _, vary := robotsSitemap(t, env, "media.example.jp", nil); !hasHost(vary) {
		t.Errorf("no BASE_URL: Vary = %q, want Host", vary)
	}
	res := env.newClient().get("/")
	expectStatus(t, res, http.StatusOK)
	if !hasHost(res.Header.Values("Vary")) {
		t.Errorf("no BASE_URL top page: Vary = %q, want Host", res.Header.Values("Vary"))
	}
	// 304 にも同じ Vary を付ける。
	req, err := http.NewRequest(http.MethodGet, env.srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", res.Header.Get("ETag"))
	notModified := env.newClient().do(req)
	expectStatus(t, notModified, http.StatusNotModified)
	if !hasHost(notModified.Header.Values("Vary")) {
		t.Errorf("no BASE_URL 304: Vary = %q, want Host", notModified.Header.Values("Vary"))
	}

	// BASE_URL があれば Host に左右されないため付けない。管理画面で後から BASE_URL を保存したときも従う。
	based := newTestEnvWith(t, Config{BaseURL: "https://news.example.jp"}, nil)
	if _, vary := robotsSitemap(t, based, "media.example.jp", nil); hasHost(vary) {
		t.Errorf("BASE_URL set: Vary = %q, want no Host", vary)
	}
	env.server.setBaseURL("https://news.example.jp")
	if _, vary := robotsSitemap(t, env, "media.example.jp", nil); hasHost(vary) {
		t.Errorf("BASE_URL saved later: Vary = %q, want no Host", vary)
	}
}

func TestPublicHTMLCacheMiddlewareVaryHostConfig(t *testing.T) {
	vary := true
	e := newCacheTestEcho(PublicHTMLCacheMiddlewareWithConfig(PublicHTMLCacheConfig{VaryHost: func() bool { return vary }}))
	e.GET("/hx", func(c echo.Context) error {
		c.Response().Header().Add(echo.HeaderVary, "HX-Request")
		return c.HTML(http.StatusOK, "hx")
	})
	serve := func(path string) http.Header {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header()
	}
	if got := serve("/hx").Values("Vary"); !slices.Equal(got, []string{"HX-Request", "Host"}) {
		t.Errorf("Vary = %q, want HX-Request and Host", got)
	}
	// Cookie を発行する応答（キャッシュさせない）・管理画面には付けない。
	if got := serve("/with-cookie").Values("Vary"); slices.Contains(got, "Host") {
		t.Errorf("with-cookie Vary = %q, want no Host", got)
	}
	vary = false
	if got := serve("/").Values("Vary"); slices.Contains(got, "Host") {
		t.Errorf("VaryHost false: Vary = %q, want no Host", got)
	}
	// 従来の PublicHTMLCacheMiddleware は Vary を付けない。
	plain := newCacheTestEcho(PublicHTMLCacheMiddleware())
	rec := httptest.NewRecorder()
	plain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Values("Vary"); len(got) != 0 {
		t.Errorf("PublicHTMLCacheMiddleware Vary = %q, want none", got)
	}

	h := http.Header{}
	h.Set("Vary", "Accept-Encoding, host")
	addVary(h, "Host")
	if got := h.Values("Vary"); len(got) != 1 {
		t.Errorf("addVary duplicated Host: %q", got)
	}
}

// すべての応答（公開サイト・管理画面・ログイン画面・公開専用モードの /admin の 404）に、カメラ・マイク・位置情報を
// 使わせない Permissions-Policy を付ける。
func TestPermissionsPolicyOnEveryResponse(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	for _, path := range []string{"/", "/admin/login", "/no-such-page", "/static/js/admin.js"} {
		if got := c.get(path).Header.Get("Permissions-Policy"); got != "camera=(), microphone=(), geolocation=()" {
			t.Errorf("%s: Permissions-Policy = %q", path, got)
		}
	}
	adm := env.loggedIn(admin)
	res := adm.get("/admin")
	expectStatus(t, res, http.StatusOK)
	if got := res.Header.Get("Permissions-Policy"); got != permissionsPolicy {
		t.Errorf("/admin: Permissions-Policy = %q", got)
	}
	pub := newTestEnvWith(t, Config{PublicOnly: true}, nil)
	if got := pub.newClient().get("/admin/login").Header.Get("Permissions-Policy"); got != permissionsPolicy {
		t.Errorf("public-only /admin: Permissions-Policy = %q", got)
	}
}
