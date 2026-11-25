package mcp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// testVerifier・testChallenge は RFC 7636 付録 B の PKCE の例。
const (
	testVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	testRedirect  = "https://claude.ai/api/mcp/auth_callback"
)

func TestPKCEHelpers(t *testing.T) {
	if got := PKCEChallenge(testVerifier); got != testChallenge {
		t.Errorf("PKCEChallenge = %q, want %q (RFC 7636 appendix B)", got, testChallenge)
	}
	if !VerifyPKCE(testVerifier, testChallenge) {
		t.Error("VerifyPKCE rejected the RFC 7636 example")
	}
	for _, tt := range []struct {
		name, verifier, challenge string
	}{
		{"wrong verifier", strings.Repeat("a", 43), testChallenge},
		{"short verifier", "abc", PKCEChallenge("abc")},
		{"verifier with invalid char", strings.Repeat("a", 42) + "+", PKCEChallenge(strings.Repeat("a", 42) + "+")},
		{"plain method (challenge = verifier)", testVerifier, testVerifier},
		{"empty", "", ""},
	} {
		if VerifyPKCE(tt.verifier, tt.challenge) {
			t.Errorf("%s: VerifyPKCE accepted", tt.name)
		}
	}
	if ValidCodeChallenge(strings.Repeat("a", 44)) || ValidCodeChallenge("short") || !ValidCodeChallenge(testChallenge) {
		t.Error("ValidCodeChallenge")
	}
	if ValidCodeVerifier(strings.Repeat("a", 129)) || !ValidCodeVerifier(strings.Repeat("a", 128)) {
		t.Error("ValidCodeVerifier length bounds")
	}
}

func TestValidRedirectURI(t *testing.T) {
	for _, tt := range []struct {
		uri  string
		want bool
	}{
		{"https://claude.ai/api/mcp/auth_callback", true},
		{"https://chatgpt.com/connector_platform_oauth_redirect?x=1", true},
		{"http://localhost:33418/callback", true},
		{"http://127.0.0.1:6274/oauth/callback", true},
		{"http://[::1]:8080/cb", true},
		{"http://localhost/cb", true},
		{"http://example.com/callback", false},
		{"http://192.168.1.10/cb", false},
		{"http://localhost.evil.example/cb", false},
		{"cursor://anysphere.cursor-retrieval/oauth/callback", false},
		{"javascript:alert(1)", false},
		{"https://claude.ai/cb#fragment", false},
		{"https://user:pass@claude.ai/cb", false},
		{"https:///no-host", false},
		{"/relative/path", false},
		{" https://claude.ai/cb", false},
		{"https://claude.ai/c b", false},
		{"", false},
	} {
		if got := ValidRedirectURI(tt.uri); got != tt.want {
			t.Errorf("ValidRedirectURI(%q) = %v, want %v", tt.uri, got, tt.want)
		}
	}
}

// oauthFixture は登録済みのクライアントと OAuthStore。
type oauthFixture struct {
	env    *testEnv
	store  *OAuthStore
	client *OAuthClient
	now    time.Time
}

func newOAuthFixture(t *testing.T, cfg Config) *oauthFixture {
	t.Helper()
	env := newTestEnv(t, cfg)
	f := &oauthFixture{env: env, store: NewOAuthStore(env.db), now: time.Now().UTC()}
	f.store.SetClock(func() time.Time { return f.now })
	c, err := f.store.RegisterClient(context.Background(), "Claude", []string{testRedirect, "http://127.0.0.1:6274/cb"})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	f.client = c
	return f
}

func (f *oauthFixture) issueCode(t *testing.T, userID string) string {
	t.Helper()
	code, err := f.store.IssueCode(context.Background(), OAuthCodeRequest{
		ClientID: f.client.ClientID, UserID: userID, RedirectURI: testRedirect,
		CodeChallenge: testChallenge, Scope: OAuthScope, Resource: "https://cms.example/api/mcp",
	})
	if err != nil {
		t.Fatalf("IssueCode: %v", err)
	}
	return code
}

func TestOAuthClientRegistration(t *testing.T) {
	f := newOAuthFixture(t, Config{})
	ctx := context.Background()
	got, err := f.store.GetClient(ctx, f.client.ClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if !strings.HasPrefix(got.ClientID, OAuthClientIDPrefix) || got.ClientName != "Claude" ||
		!got.HasRedirectURI(testRedirect) || got.HasRedirectURI(testRedirect+"/") || got.HasRedirectURI("https://claude.ai") {
		t.Errorf("client = %+v", got)
	}
	if _, err := f.store.GetClient(ctx, "sorusc_unknown"); !errors.Is(err, ErrOAuthInvalidClient) {
		t.Errorf("GetClient(unknown) err = %v, want ErrOAuthInvalidClient", err)
	}
	for _, uris := range [][]string{nil, {"http://evil.example/cb"}, {testRedirect, "myapp://cb"}} {
		if _, err := f.store.RegisterClient(ctx, "bad", uris); !errors.Is(err, ErrOAuthInvalidRedirectURI) {
			t.Errorf("RegisterClient(%v) err = %v, want ErrOAuthInvalidRedirectURI", uris, err)
		}
	}

	// 一度も認可されないまま1日たったクライアントは、次の登録のときに消える。
	f.now = f.now.Add(oauthUnusedClientTTL + time.Hour)
	if _, err := f.store.RegisterClient(ctx, "later", []string{testRedirect}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.GetClient(ctx, f.client.ClientID); !errors.Is(err, ErrOAuthInvalidClient) {
		t.Errorf("unused client was not pruned: %v", err)
	}
}

func TestOAuthAuthorizationCodeFlow(t *testing.T) {
	f := newOAuthFixture(t, Config{})
	ctx := context.Background()
	code := f.issueCode(t, writerA.ID)

	// コード自体は保存しない（ハッシュだけ）。
	if n := countRows(t, f.env, `SELECT COUNT(*) FROM oauth_codes WHERE code_hash = ? OR code_hash = ?`, code, HashToken(code)); n != 1 {
		t.Fatalf("oauth_codes rows = %d", n)
	}
	if n := countRows(t, f.env, `SELECT COUNT(*) FROM oauth_codes WHERE code_hash = ?`, code); n != 0 {
		t.Error("raw code is stored")
	}

	// 失敗した引き換えではコードを使用済みにしない（正しい持ち主はあとで引き換えられる）。
	failures := []struct{ name, client, redirect, verifier string }{
		{"wrong verifier", f.client.ClientID, testRedirect, strings.Repeat("x", 43)},
		{"missing verifier", f.client.ClientID, testRedirect, ""},
		{"challenge as verifier (plain)", f.client.ClientID, testRedirect, testChallenge},
		{"other registered redirect", f.client.ClientID, "http://127.0.0.1:6274/cb", testVerifier},
		{"wrong client", "sorusc_other", testRedirect, testVerifier},
	}
	for _, tt := range failures {
		if _, err := f.store.ExchangeCode(ctx, code, tt.client, tt.redirect, tt.verifier); !errors.Is(err, ErrOAuthInvalidGrant) {
			t.Errorf("%s: err = %v, want ErrOAuthInvalidGrant", tt.name, err)
		}
	}
	if _, err := f.store.ExchangeCode(ctx, "soruscode_unknown", f.client.ClientID, testRedirect, testVerifier); !errors.Is(err, ErrOAuthInvalidGrant) {
		t.Errorf("unknown code: err = %v", err)
	}

	set, err := f.store.ExchangeCode(ctx, code, f.client.ClientID, testRedirect, testVerifier)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if !strings.HasPrefix(set.AccessToken, OAuthAccessTokenPrefix) || !strings.HasPrefix(set.RefreshToken, OAuthRefreshTokenPrefix) ||
		set.ExpiresIn != 3600 || set.Scope != OAuthScope {
		t.Errorf("token set = %+v", set)
	}
	// トークンもハッシュだけを保存する。
	if n := countRows(t, f.env, `SELECT COUNT(*) FROM oauth_tokens WHERE token_hash = ? AND refresh_token_hash = ? AND user_id = ? AND resource = ?`,
		HashToken(set.AccessToken), HashToken(set.RefreshToken), writerA.ID, "https://cms.example/api/mcp"); n != 1 {
		t.Errorf("stored token rows = %d, want 1", n)
	}
	if n := countRows(t, f.env, `SELECT COUNT(*) FROM oauth_tokens WHERE token_hash = ? OR refresh_token_hash = ?`, set.AccessToken, set.RefreshToken); n != 0 {
		t.Error("raw token is stored")
	}

	// コードは使い捨て。
	if _, err := f.store.ExchangeCode(ctx, code, f.client.ClientID, testRedirect, testVerifier); !errors.Is(err, ErrOAuthInvalidGrant) {
		t.Errorf("reused code: err = %v, want ErrOAuthInvalidGrant", err)
	}

	// 期限（5分）を過ぎたコードは使えない。
	late := f.issueCode(t, writerA.ID)
	f.now = f.now.Add(OAuthCodeTTL + time.Second)
	if _, err := f.store.ExchangeCode(ctx, late, f.client.ClientID, testRedirect, testVerifier); !errors.Is(err, ErrOAuthInvalidGrant) {
		t.Errorf("expired code: err = %v, want ErrOAuthInvalidGrant", err)
	}
}

func TestOAuthRefreshRotation(t *testing.T) {
	f := newOAuthFixture(t, Config{})
	ctx := context.Background()
	first, err := f.store.ExchangeCode(ctx, f.issueCode(t, writerA.ID), f.client.ClientID, testRedirect, testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Refresh(ctx, first.RefreshToken, "sorusc_other"); !errors.Is(err, ErrOAuthInvalidGrant) {
		t.Errorf("refresh with another client: err = %v", err)
	}
	second, err := f.store.Refresh(ctx, first.RefreshToken, f.client.ClientID)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh did not rotate tokens")
	}
	tokens := NewDBTokenStore(f.env.db)
	tokens.now = func() time.Time { return f.now }
	// 古い組（アクセストークンも）は使えず、新しい組は使える。
	if _, err := tokens.Authenticate(ctx, first.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("old access token after rotation: err = %v, want ErrInvalidToken", err)
	}
	if u, err := tokens.Authenticate(ctx, second.AccessToken); err != nil || u.ID != writerA.ID {
		t.Fatalf("new access token: user %v, err %v", u, err)
	}

	// 無効にした（ローテーション済みの）リフレッシュトークンがもう一度使われたら、同じ連携のトークンをすべて無効にする。
	if _, err := f.store.Refresh(ctx, first.RefreshToken, f.client.ClientID); !errors.Is(err, ErrOAuthInvalidGrant) {
		t.Errorf("reused refresh token: err = %v, want ErrOAuthInvalidGrant", err)
	}
	if _, err := tokens.Authenticate(ctx, second.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("access token after refresh token reuse: err = %v, want ErrInvalidToken", err)
	}
	if _, err := f.store.Refresh(ctx, second.RefreshToken, f.client.ClientID); !errors.Is(err, ErrOAuthInvalidGrant) {
		t.Errorf("refresh token after reuse detection: err = %v, want ErrOAuthInvalidGrant", err)
	}

	// アクセストークンは1時間、リフレッシュトークンは30日で切れる。
	third, err := f.store.ExchangeCode(ctx, f.issueCode(t, writerA.ID), f.client.ClientID, testRedirect, testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(OAuthAccessTokenTTL + time.Minute)
	if _, err := tokens.Authenticate(ctx, third.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expired access token: err = %v, want ErrInvalidToken", err)
	}
	fourth, err := f.store.Refresh(ctx, third.RefreshToken, f.client.ClientID)
	if err != nil {
		t.Fatalf("refresh after access token expiry: %v", err)
	}
	f.now = f.now.Add(OAuthRefreshTokenTTL + time.Minute)
	if _, err := f.store.Refresh(ctx, fourth.RefreshToken, f.client.ClientID); !errors.Is(err, ErrOAuthInvalidGrant) {
		t.Errorf("expired refresh token: err = %v, want ErrOAuthInvalidGrant", err)
	}
}

// TestOAuthAccessTokenOnMCPEndpoint は OAuth のアクセストークンで /api/mcp を使え、取り消したトークンは
// 401（resource_metadata 付き）になることを確かめる。
func TestOAuthAccessTokenOnMCPEndpoint(t *testing.T) {
	f := newOAuthFixture(t, Config{})
	f.store.SetClock(time.Now)
	set, err := f.store.ExchangeCode(context.Background(), f.issueCode(t, editor.ID), f.client.ClientID, testRedirect, testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	r := f.env.post("", ping, map[string]string{"Authorization": "Bearer " + set.AccessToken})
	if r.Status != http.StatusOK {
		t.Fatalf("ping with oauth token: status %d: %s", r.Status, r.Body)
	}
	var used presence
	if err := f.env.db.Get(&used, `SELECT last_used_at FROM oauth_tokens WHERE token_hash = ?`, HashToken(set.AccessToken)); err != nil {
		t.Fatal(err)
	}
	if !used.Valid {
		t.Error("oauth token last_used_at is not recorded")
	}

	mustExec(t, f.env.db, `UPDATE oauth_tokens SET revoked_at = CURRENT_TIMESTAMP WHERE token_hash = ?`, HashToken(set.AccessToken))
	r = f.env.post("", ping, map[string]string{"Authorization": "Bearer " + set.AccessToken})
	if r.Status != http.StatusUnauthorized {
		t.Fatalf("revoked oauth token: status %d, want 401", r.Status)
	}
	want := `resource_metadata="` + f.env.http.URL + ProtectedResourceMetadataPath + `"`
	if h := r.Header.Get("WWW-Authenticate"); !strings.Contains(h, `error="invalid_token"`) || !strings.Contains(h, want) {
		t.Errorf("WWW-Authenticate = %q, want invalid_token and %s", h, want)
	}
	r = f.env.post("", ping, map[string]string{"Authorization": ""})
	if h := r.Header.Get("WWW-Authenticate"); r.Status != http.StatusUnauthorized || !strings.HasPrefix(h, "Bearer ") || !strings.Contains(h, want) {
		t.Errorf("no token: status %d, WWW-Authenticate = %q, want %s", r.Status, h, want)
	}
}

func TestHostname(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"localhost:8080", "localhost", true},
		{"LOCALHOST", "localhost", true},
		{"127.0.0.1:52080", "127.0.0.1", true},
		{"[::1]:8080", "::1", true},
		{"[::1]", "::1", true},
		{"cms.example.ac.jp.", "cms.example.ac.jp", true},
		{"192.168.1.20:8080", "192.168.1.20", true},
		{"sorus.local:8080", "sorus.local", true},
		{"", "", false},
		{"evil.example:", "", false},
		{"evil.example:80a", "", false},
		{"[::1", "", false},
		{"[nothost]:80", "", false},
		{"a b", "", false},
		{"evil.example/path", "", false},
		{"user@evil.example", "", false},
		{"::1", "", false},
	} {
		got, ok := hostname(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("hostname(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// TestDNSRebindingProtection は /api/mcp の Host・Origin の検査を確かめる。デスクトップモード（LoopbackOnly）では
// この PC を指す Host と AllowedHosts だけを受け付け、サーバーモードでは社内 LAN のアドレス・ホスト名も受け付ける。
// どちらのモードでも、許可していないオリジンのブラウザからの要求は断る。
func TestDNSRebindingProtection(t *testing.T) {
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	send := func(env *testEnv, host, origin string) httpResult {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, env.http.URL+DefaultBasePath, strings.NewReader(ping))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+env.tokens[writerA.ID])
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return httpResult{Status: res.StatusCode, Header: res.Header, Body: string(b)}
	}

	desktop := newTestEnv(t, Config{LoopbackOnly: true, AllowedHosts: []string{"cms.example.ac.jp"},
		AllowedOrigins: []string{"https://cms.example.ac.jp"}})
	for _, tt := range []struct {
		name, host, origin string
		want               int
	}{
		{"loopback ip", "127.0.0.1:8080", "", http.StatusOK},
		{"localhost", "localhost:8080", "", http.StatusOK},
		{"ipv6 loopback", "[::1]:8080", "", http.StatusOK},
		{"allowed host", "cms.example.ac.jp", "", http.StatusOK},
		{"rebinding attacker host", "evil.example:8080", "", http.StatusForbidden},
		{"rebinding attacker host with same-host origin", "evil.example:8080", "http://evil.example:8080", http.StatusForbidden},
		{"lan ip in desktop mode", "192.168.1.20:8080", "", http.StatusForbidden},
		{"subdomain of localhost name", "localhost.evil.example", "", http.StatusForbidden},
		{"foreign origin on loopback host", "127.0.0.1:8080", "https://evil.example", http.StatusForbidden},
		{"null origin", "127.0.0.1:8080", "null", http.StatusForbidden},
		{"same-host origin", "127.0.0.1:8080", "http://127.0.0.1:8080", http.StatusOK},
		{"allowed origin", "127.0.0.1:8080", "https://cms.example.ac.jp", http.StatusOK},
	} {
		if r := send(desktop, tt.host, tt.origin); r.Status != tt.want {
			t.Errorf("desktop %s: status %d, want %d: %s", tt.name, r.Status, tt.want, r.Body)
		}
	}

	server := newTestEnv(t, Config{AllowedOrigins: []string{"https://cms.example.ac.jp"}})
	for _, tt := range []struct {
		name, host, origin string
		want               int
	}{
		{"lan ip", "192.168.1.20:8080", "", http.StatusOK},
		{"lan hostname", "sorus.local:8080", "", http.StatusOK},
		{"public domain", "cms.example.ac.jp", "", http.StatusOK},
		{"loopback", "127.0.0.1:8080", "", http.StatusOK},
		{"malformed host", "evil.example:80a", "", http.StatusForbidden},
		{"foreign origin", "sorus.local:8080", "https://evil.example", http.StatusForbidden},
		{"same-host origin", "sorus.local:8080", "http://sorus.local:8080", http.StatusOK},
		{"allowed origin", "sorus.local:8080", "https://cms.example.ac.jp", http.StatusOK},
	} {
		if r := send(server, tt.host, tt.origin); r.Status != tt.want {
			t.Errorf("server %s: status %d, want %d: %s", tt.name, r.Status, tt.want, r.Body)
		}
	}

	// 宛先の検査は認証より前に行い、未認証の要求にも 403 を返す（SSE の旧仕様のパスも同じ）。
	req := httptest.NewRequest(http.MethodGet, DefaultBasePath+"/sse", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	desktop.server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("sse with attacker host: status %d, want 403", rec.Code)
	}
}

func countRows(t *testing.T, env *testEnv, query string, args ...any) int {
	t.Helper()
	var n int
	if err := env.db.Get(&n, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestParseTrustedProxies(t *testing.T) {
	list, err := ParseTrustedProxies(" 127.0.0.1, 10.0.0.0/8 ,,::1, 2001:db8::/32, ::ffff:192.168.0.0/112 ")
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	for addr, want := range map[string]bool{
		"127.0.0.1":        true,
		"127.0.0.2":        false,
		"10.20.30.40":      true,
		"11.0.0.1":         false,
		"::1":              true,
		"::ffff:127.0.0.1": true, // IPv4 射影 IPv6 は IPv4 として比べる。
		"2001:db8:1::5":    true,
		"2001:db9::1":      false,
		"192.168.3.4":      true, // ::ffff:192.168.0.0/112 は 192.168.0.0/16。
		"192.169.0.1":      false,
	} {
		if got := list.Contains(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", addr, got, want)
		}
	}
	if list.Contains(netip.Addr{}) {
		t.Error("the zero address is trusted")
	}

	empty, err := ParseTrustedProxies("")
	if err != nil || !empty.Empty() || empty.Contains(netip.MustParseAddr("127.0.0.1")) {
		t.Errorf("empty list = %+v, %v", empty, err)
	}

	// 解析できない項目はエラーにし、信頼しない（解析できた項目だけを使う）。
	partial, err := ParseTrustedProxies("127.0.0.1, not-an-ip, 10.0.0.0/33, fe80::1%eth0")
	if err == nil || !strings.Contains(err.Error(), "not-an-ip") || !strings.Contains(err.Error(), "10.0.0.0/33") {
		t.Errorf("err = %v, want the invalid entries", err)
	}
	if !partial.Contains(netip.MustParseAddr("127.0.0.1")) || partial.Contains(netip.MustParseAddr("10.0.0.1")) {
		t.Errorf("partial list = %+v", partial)
	}
}

// EnvTrustedProxies は環境変数が変わったら読み直す。
func TestEnvTrustedProxies(t *testing.T) {
	t.Setenv(TrustedProxiesEnv, "10.0.0.1")
	if !EnvTrustedProxies().Contains(netip.MustParseAddr("10.0.0.1")) {
		t.Fatal("10.0.0.1 is not trusted")
	}
	t.Setenv(TrustedProxiesEnv, "")
	if !EnvTrustedProxies().Empty() {
		t.Error("the list was not reloaded after the variable changed")
	}
}

// スキームは接続自体の TLS と、TRUSTED_PROXIES のプロキシが付けた転送ヘッダーだけで決める。既定（未設定）では、
// この PC 自身や LAN からの接続でも、クライアントが書いた X-Forwarded-Proto などを無視する。
func TestRequestOriginTrustsForwardedHeadersOnlyFromTrustedProxies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted string
		remote  string
		tls     bool
		headers map[string]string
		want    string
	}{
		{"plain", "", "203.0.113.5:1000", false, nil, "http://cms.example.jp"},
		{"tls", "", "203.0.113.5:1000", true, nil, "https://cms.example.jp"},
		{"tls ignores forwarded http", "", "203.0.113.5:1000", true, map[string]string{"X-Forwarded-Proto": "http"}, "https://cms.example.jp"},
		{"spoofed proto", "", "203.0.113.5:1000", false, map[string]string{"X-Forwarded-Proto": "https"}, "http://cms.example.jp"},
		{"spoofed proto from loopback", "", "127.0.0.1:1000", false, map[string]string{"X-Forwarded-Proto": "https"}, "http://cms.example.jp"},
		{"spoofed protocol", "", "192.168.1.5:1000", false, map[string]string{"X-Forwarded-Protocol": "https"}, "http://cms.example.jp"},
		{"spoofed ssl", "", "192.168.1.5:1000", false, map[string]string{"X-Forwarded-Ssl": "on"}, "http://cms.example.jp"},
		{"spoofed url scheme", "", "10.0.0.5:1000", false, map[string]string{"X-Url-Scheme": "https"}, "http://cms.example.jp"},
		{"untrusted lan host", "10.0.0.1", "10.0.0.5:1000", false, map[string]string{"X-Forwarded-Proto": "https"}, "http://cms.example.jp"},
		{"trusted proto", "127.0.0.1", "127.0.0.1:1000", false, map[string]string{"X-Forwarded-Proto": "https"}, "https://cms.example.jp"},
		{"trusted proto list", "127.0.0.1", "127.0.0.1:1000", false, map[string]string{"X-Forwarded-Proto": "HTTPS, http"}, "https://cms.example.jp"},
		{"trusted protocol", "10.0.0.0/8", "10.0.0.5:1000", false, map[string]string{"X-Forwarded-Protocol": "https"}, "https://cms.example.jp"},
		{"trusted ssl", "10.0.0.0/8", "10.0.0.5:1000", false, map[string]string{"X-Forwarded-Ssl": "on"}, "https://cms.example.jp"},
		{"trusted url scheme", "10.0.0.0/8", "10.0.0.5:1000", false, map[string]string{"X-Url-Scheme": "https"}, "https://cms.example.jp"},
		{"trusted unknown scheme", "127.0.0.1", "127.0.0.1:1000", false, map[string]string{"X-Forwarded-Proto": "javascript"}, "http://cms.example.jp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trusted := mustTrustedProxies(t, tc.trusted)
			r := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
			r.Host = "cms.example.jp"
			r.RemoteAddr = tc.remote
			if !tc.tls {
				r.TLS = nil
			} else if r.TLS == nil {
				r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := trusted.Origin(r); got != tc.want {
				t.Errorf("Origin = %q, want %q", got, tc.want)
			}
		})
	}

	// Host の不審な文字は取り除き、空なら localhost。
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = `evil"<host>;,`
	if got := (TrustedProxies{}).Origin(r); got != "http://evilhost" {
		t.Errorf("Origin(bad host) = %q", got)
	}
	r.Host = ""
	if got := (TrustedProxies{}).Origin(r); got != "http://localhost" {
		t.Errorf("Origin(empty host) = %q", got)
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		trusted, remote, xff, want string
	}{
		{"", "203.0.113.5:1000", "198.51.100.1", "203.0.113.5"},
		{"", "127.0.0.1:1000", "198.51.100.1", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1:1000", "198.51.100.1", "198.51.100.1"},
		{"127.0.0.1", "127.0.0.1:1000", "", "127.0.0.1"},
		{"127.0.0.1", "[fe80::1%eth0]:1000", "198.51.100.1", "fe80::1"},
		{"", "not-an-address", "198.51.100.1", "not-an-address"},
		{"", "pipe:1000", "", "pipe"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := mustTrustedProxies(t, tc.trusted).ClientIP(r); got != tc.want {
			t.Errorf("ClientIP(%s, xff %q, trusted %q) = %q, want %q", tc.remote, tc.xff, tc.trusted, got, tc.want)
		}
	}
}

// 401 の WWW-Authenticate の resource_metadata も、クライアントが書いた X-Forwarded-Proto でスキームを変えない。
func TestUnauthorizedMetadataIgnoresSpoofedProto(t *testing.T) {
	t.Setenv(TrustedProxiesEnv, "")
	env := newTestEnv(t, Config{})
	req := httptest.NewRequest(http.MethodPost, DefaultBasePath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Host = "cms.example.jp"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	env.server.ServeHTTP(rec, req)
	want := `resource_metadata="http://cms.example.jp` + ProtectedResourceMetadataPath + `"`
	if h := rec.Header().Get("WWW-Authenticate"); rec.Code != http.StatusUnauthorized || !strings.Contains(h, want) {
		t.Errorf("status %d, WWW-Authenticate = %q, want %s", rec.Code, h, want)
	}
}

// PreferredOrigin は、公開 URL のホスト名が要求の Host と同じときだけ公開 URL のオリジンを使う。
func TestPreferredOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, base, host, proto, want string
	}{
		{"matching host", "https://cms.example.jp", "cms.example.jp", "", "https://cms.example.jp"},
		{"matching host, spoofed proto", "https://cms.example.jp", "cms.example.jp", "http", "https://cms.example.jp"},
		{"matching host, other port", "https://cms.example.jp", "cms.example.jp:8443", "", "https://cms.example.jp"},
		{"base with port and path", "https://CMS.example.jp:8443/blog/", "cms.example.jp", "", "https://cms.example.jp:8443"},
		{"trailing dot", "https://cms.example.jp.", "cms.example.jp", "", "https://cms.example.jp."},
		{"http base", "http://cms.example.jp", "cms.example.jp", "https", "http://cms.example.jp"},
		{"other host", "https://media.example.jp", "cms.example.jp", "https", "http://cms.example.jp"},
		{"static host", "https://example.pages.dev", "cms.example.jp", "", "http://cms.example.jp"},
		{"empty base", "", "cms.example.jp", "", "http://cms.example.jp"},
		{"loopback base", "http://localhost:8080", "localhost:8080", "", "http://localhost:8080"},
		{"loopback ip base", "https://127.0.0.1", "127.0.0.1", "", "http://127.0.0.1"},
		{"non-http base", "ftp://cms.example.jp", "cms.example.jp", "", "http://cms.example.jp"},
		{"userinfo base", "https://user@cms.example.jp", "cms.example.jp", "", "http://cms.example.jp"},
		{"relative base", "cms.example.jp", "cms.example.jp", "", "http://cms.example.jp"},
		{"bad request host", "https://cms.example.jp", `cms.example.jp"`, "", "http://cms.example.jp"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = tc.host
		if tc.proto != "" {
			r.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		if got := (TrustedProxies{}).PreferredOrigin(r, tc.base, nil); got != tc.want {
			t.Errorf("%s: PreferredOrigin(%q, host %q) = %q, want %q", tc.name, tc.base, tc.host, got, tc.want)
		}
	}
}

// 401 の resource_metadata は、公開 URL のホスト名が同じならそのスキームで組み立てる。デスクトップモード
// （LoopbackOnly）では公開 URL を使わない。
func TestUnauthorizedMetadataPrefersMatchingBaseURL(t *testing.T) {
	t.Setenv(TrustedProxiesEnv, "")
	send := func(env *testEnv, host string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, DefaultBasePath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		env.server.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("host %s: status %d, want 401", host, rec.Code)
		}
		return rec.Header().Get("WWW-Authenticate")
	}
	server := newTestEnv(t, Config{BaseURL: "https://cms.example.jp"})
	if h := send(server, "cms.example.jp"); !strings.Contains(h, `resource_metadata="https://cms.example.jp`+ProtectedResourceMetadataPath+`"`) {
		t.Errorf("server mode: WWW-Authenticate = %q", h)
	}
	if h := send(server, "admin.example.jp"); !strings.Contains(h, `resource_metadata="http://admin.example.jp`+ProtectedResourceMetadataPath+`"`) {
		t.Errorf("server mode, other host: WWW-Authenticate = %q", h)
	}
	desktop := newTestEnv(t, Config{BaseURL: "https://cms.example.jp", LoopbackOnly: true, AllowedHosts: []string{"cms.example.jp"}})
	if h := send(desktop, "cms.example.jp"); !strings.Contains(h, `resource_metadata="http://cms.example.jp`+ProtectedResourceMetadataPath+`"`) {
		t.Errorf("desktop mode: WWW-Authenticate = %q", h)
	}
}

// mustTrustedProxies は TRUSTED_PROXIES の書式の s を解析する（試験用）。
func mustTrustedProxies(t *testing.T, s string) TrustedProxies {
	t.Helper()
	list, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", s, err)
	}
	return list
}

// NewTrustedProxies は解析済みの範囲から ParseTrustedProxies と同じ一覧を作る（server パッケージと共有するため）。
func TestNewTrustedProxies(t *testing.T) {
	list := NewTrustedProxies(netip.MustParsePrefix("10.1.2.3/8"), netip.Prefix{}, netip.MustParsePrefix("::ffff:192.0.2.0/120"))
	for addr, want := range map[string]bool{"10.9.9.9": true, "192.0.2.7": true, "::ffff:192.0.2.7": true, "11.0.0.1": false, "192.0.3.1": false} {
		if got := list.Contains(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", addr, got, want)
		}
	}
	if !NewTrustedProxies().Empty() {
		t.Error("NewTrustedProxies() is not empty")
	}
}

// HostAllowed は ALLOWED_HOSTS の照合。ポートを書いた値はポートまで、書かない値はホスト名だけで比べる。
func TestHostAllowed(t *testing.T) {
	allowed := []string{"cms.example.jp", "www.example.jp:8443", "[::1]:8080", " Admin.Example.JP. "}
	for host, want := range map[string]bool{
		"cms.example.jp":       true,
		"CMS.example.jp.":      true,
		"cms.example.jp:8080":  true,
		"www.example.jp:8443":  true,
		"www.example.jp":       false,
		"www.example.jp:443":   false,
		"[::1]:8080":           true,
		"[::1]:9090":           false,
		"admin.example.jp":     true,
		"evil.example":         false,
		"cms.example.jp.evil":  false,
		"cms.example.jp:80a":   false,
		`cms.example.jp"`:      false,
		"":                     false,
		"evil.example@cms.jp":  false,
		"sub.cms.example.jp":   false,
		"cms.example.jp:65536": true, // ポートの範囲は Host の書式（5桁まで）だけを確かめる
	} {
		if got := HostAllowed(host, allowed); got != want {
			t.Errorf("HostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
	if HostAllowed("cms.example.jp", nil) {
		t.Error("an empty list allowed a host")
	}
}

// ALLOWED_HOSTS を指定したら、一覧に無い（偽の）Host を応答のオリジンに載せない。公開 URL のホストが一覧にあれば
// 公開 URL のオリジン、無ければ一覧の先頭のホストを使う。
func TestPreferredOriginAllowedHosts(t *testing.T) {
	trusted := mustTrustedProxies(t, "192.0.2.0/24") // httptest.NewRequest の接続元は 192.0.2.1
	for _, tc := range []struct {
		name, base, host, proto string
		allowed                 []string
		want                    string
	}{
		{"allowed host, matching base", "https://cms.example.jp", "cms.example.jp", "", []string{"cms.example.jp"}, "https://cms.example.jp"},
		{"allowed host, other base", "https://example.pages.dev", "cms.example.jp", "https", []string{"cms.example.jp"}, "https://cms.example.jp"},
		{"allowed host with port", "", "cms.example.jp:8443", "", []string{"cms.example.jp"}, "http://cms.example.jp:8443"},
		{"spoofed host, base in list", "https://cms.example.jp", "evil.example", "", []string{"cms.example.jp"}, "https://cms.example.jp"},
		{"spoofed host, base port in list", "https://cms.example.jp:8443", "evil.example", "", []string{"cms.example.jp:8443"}, "https://cms.example.jp:8443"},
		{"spoofed host, static base", "https://example.pages.dev", "evil.example", "https", []string{"cms.example.jp", "www.example.jp"}, "https://cms.example.jp"},
		{"spoofed host, no base", "", "evil.example", "", []string{"CMS.example.jp:8443"}, "http://cms.example.jp:8443"},
		{"spoofed host, invalid first entry", "", "evil.example", "", []string{"bad host", "cms.example.jp"}, "http://cms.example.jp"},
		{"malformed host", "", `evil"<x>`, "", []string{"cms.example.jp"}, "http://cms.example.jp"},
		{"port not in list", "", "www.example.jp:9999", "", []string{"www.example.jp:8443"}, "http://www.example.jp:8443"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = tc.host
		if tc.proto != "" {
			r.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		if got := trusted.PreferredOrigin(r, tc.base, tc.allowed); got != tc.want {
			t.Errorf("%s: PreferredOrigin(%q, host %q, %v) = %q, want %q", tc.name, tc.base, tc.host, tc.allowed, got, tc.want)
		}
	}
}

// サーバーモードで AllowedHosts（ALLOWED_HOSTS）を指定したら、一覧に無い Host の要求を認証より前に 403 で断る。
// 指定しなければ、今までどおり書式の正しい Host はすべて受け付ける。
func TestServerModeAllowedHosts(t *testing.T) {
	send := func(env *testEnv, host string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, DefaultBasePath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		env.server.ServeHTTP(rec, req)
		return rec
	}
	none := mustTrustedProxies(t, "")
	restricted := newTestEnv(t, Config{BaseURL: "https://cms.example.jp", AllowedHosts: []string{"cms.example.jp", "sorus.local:8080"}, TrustedProxies: &none})
	for host, want := range map[string]int{
		"cms.example.jp":      http.StatusUnauthorized,
		"cms.example.jp:8443": http.StatusUnauthorized,
		"sorus.local:8080":    http.StatusUnauthorized,
		"sorus.local:9090":    http.StatusForbidden,
		"evil.example":        http.StatusForbidden,
		"127.0.0.1:8080":      http.StatusForbidden,
		"192.168.1.20:8080":   http.StatusForbidden,
	} {
		if rec := send(restricted, host); rec.Code != want {
			t.Errorf("restricted %s: status %d, want %d: %s", host, rec.Code, want, rec.Body)
		}
	}
	if h := send(restricted, "sorus.local:8080").Header().Get("WWW-Authenticate"); !strings.Contains(h, `resource_metadata="http://sorus.local:8080`+ProtectedResourceMetadataPath+`"`) {
		t.Errorf("allowed host: WWW-Authenticate = %q", h)
	}

	open := newTestEnv(t, Config{TrustedProxies: &none})
	for _, host := range []string{"evil.example", "127.0.0.1:8080", "192.168.1.20:8080"} {
		if rec := send(open, host); rec.Code != http.StatusUnauthorized {
			t.Errorf("unrestricted %s: status %d, want 401", host, rec.Code)
		}
	}
}

// Config.TrustedProxies を渡したら、環境変数 TRUSTED_PROXIES ではなくその一覧で転送ヘッダーを信じるか決める。
// nil のときだけ環境変数を使う。
func TestConfigTrustedProxiesOverridesEnv(t *testing.T) {
	metadata := func(env *testEnv) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, DefaultBasePath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Host = "cms.example.jp"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Forwarded-Proto", "https") // 接続元は httptest.NewRequest の 192.0.2.1
		rec := httptest.NewRecorder()
		env.server.ServeHTTP(rec, req)
		return rec.Header().Get("WWW-Authenticate")
	}
	const https = `resource_metadata="https://cms.example.jp` + ProtectedResourceMetadataPath + `"`
	const plain = `resource_metadata="http://cms.example.jp` + ProtectedResourceMetadataPath + `"`

	// 環境変数は 192.0.2.1 を信頼しているが、Config で空の一覧を渡したサーバーは転送ヘッダーを信じない。
	t.Setenv(TrustedProxiesEnv, "192.0.2.0/24")
	none := mustTrustedProxies(t, "")
	if h := metadata(newTestEnv(t, Config{TrustedProxies: &none})); !strings.Contains(h, plain) {
		t.Errorf("config without proxies, env trusting: WWW-Authenticate = %q, want %s", h, plain)
	}
	if h := metadata(newTestEnv(t, Config{})); !strings.Contains(h, https) {
		t.Errorf("nil config falls back to env: WWW-Authenticate = %q, want %s", h, https)
	}

	// 環境変数が空でも、Config で信頼したプロキシの転送ヘッダーは使う。
	t.Setenv(TrustedProxiesEnv, "")
	proxy := NewTrustedProxies(netip.MustParsePrefix("192.0.2.1/32"))
	if h := metadata(newTestEnv(t, Config{TrustedProxies: &proxy})); !strings.Contains(h, https) {
		t.Errorf("config trusting, env empty: WWW-Authenticate = %q, want %s", h, https)
	}
}
