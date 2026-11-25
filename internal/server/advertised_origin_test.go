package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sorus/internal/auth"
	"sorus/internal/mcp"
)

// oauthMetadataAt は Host（と転送ヘッダー）を指定して認可サーバーのメタデータを取得し、issuer を返す。
func oauthMetadataAt(t *testing.T, s *Server, host string, headers map[string]string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata at %s: status %d: %s", host, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	issuer, _ := out["issuer"].(string)
	if out["token_endpoint"] != issuer+OAuthTokenPath {
		t.Errorf("token_endpoint = %v, want %s", out["token_endpoint"], issuer+OAuthTokenPath)
	}
	return issuer
}

// OAuth のメタデータは、公開 URL（BaseURL）のホスト名が要求の Host と同じなら、BaseURL のスキーム・ポートで
// 組み立てる（転送ヘッダーや Host のポートに左右されない）。ホスト名が違えば（BaseURL が静的な公開先など）、要求が
// 届いたオリジン。デスクトップモードでは BaseURL を使わない。
func TestOAuthMetadataPrefersMatchingBaseURL(t *testing.T) {
	t.Setenv(mcp.TrustedProxiesEnv, "")
	env := newTestEnvWith(t, Config{BaseURL: "https://cms.example.jp", DisableScheduler: true}, nil)
	for _, tc := range []struct {
		name, host string
		headers    map[string]string
		want       string
	}{
		{"same host over plain http", "cms.example.jp", nil, "https://cms.example.jp"},
		{"same host, spoofed http proto", "cms.example.jp", map[string]string{"X-Forwarded-Proto": "http"}, "https://cms.example.jp"},
		{"same host, other port", "CMS.example.jp:8080", nil, "https://cms.example.jp"},
		{"other host", "admin.example.jp", nil, "http://admin.example.jp"},
		{"other host, spoofed https proto", "admin.example.jp", map[string]string{"X-Forwarded-Proto": "https"}, "http://admin.example.jp"},
		{"loopback host", "127.0.0.1:8080", nil, "http://127.0.0.1:8080"},
	} {
		if got := oauthMetadataAt(t, env.server, tc.host, tc.headers); got != tc.want {
			t.Errorf("%s: issuer = %q, want %q", tc.name, got, tc.want)
		}
	}

	// 公開 URL が静的な公開先（OAuth も MCP サーバーも無い）なら、ホスト名が違うため要求のオリジンを使う。
	env.server.setBaseURL("https://example.pages.dev")
	if got := oauthMetadataAt(t, env.server, "cms.example.jp", nil); got != "http://cms.example.jp" {
		t.Errorf("static base url: issuer = %q", got)
	}

	desktop := newTestEnvWith(t, Config{Desktop: true, DisableScheduler: true, BaseURL: "https://cms.example.jp"}, nil)
	if got := oauthMetadataAt(t, desktop.server, "cms.example.jp", nil); got != "http://cms.example.jp" {
		t.Errorf("desktop: issuer = %q, want the request origin", got)
	}
}

// MCP の 401 の resource_metadata も同じ規則（BaseURL のホスト名が同じならそのスキーム）。
func TestMCPUnauthorizedPrefersMatchingBaseURL(t *testing.T) {
	t.Setenv(mcp.TrustedProxiesEnv, "")
	env := newTestEnvWith(t, Config{BaseURL: "https://cms.example.jp", DisableScheduler: true}, nil)
	req := httptest.NewRequest(http.MethodPost, env.server.cfg.MCPAPIPath, nil)
	req.Host = "cms.example.jp"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	want := `resource_metadata="https://cms.example.jp` + mcp.ProtectedResourceMetadataPathFor(env.server.cfg.MCPAPIPath) + `"`
	if h := rec.Header().Get("WWW-Authenticate"); rec.Code != http.StatusUnauthorized || !strings.Contains(h, want) {
		t.Errorf("status %d, WWW-Authenticate = %q, want %s", rec.Code, h, want)
	}
}

// 招待コードの URL も、BaseURL のホスト名が同じならそのスキーム・ポートを使う（平文で届いても https の公開 URL）。
// ホスト名が違えば、要求の（信頼するプロキシの転送ヘッダーだけで決めた）スキーム。
func TestSyncInviteURLPrefersMatchingBaseURL(t *testing.T) {
	t.Setenv(mcp.TrustedProxiesEnv, "")
	env := newInviteSyncEnv(t)
	env.server.setBaseURL("https://media.example.jp")
	adminTok, _ := env.recordedToken("admin", auth.RoleAdmin)

	rec := env.syncAt("media.example.jp", "", adminTok, issueInviteRequest(auth.RoleWriter, "鈴木", 7))
	expectSyncStatus(t, rec, http.StatusOK)
	inv, err := DecodeInviteCode(decodeSync[SyncIssueInviteResponse](t, rec).InviteCode)
	if err != nil || inv.URL != "https://media.example.jp"+env.server.cfg.SyncAPIPath {
		t.Fatalf("invite = %+v, %v", inv, err)
	}

	rec = env.syncAt("other.example.jp", "https", adminTok, issueInviteRequest(auth.RoleWriter, "佐藤", 7))
	expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
}

// mcpUnauthorizedAt は Host（と転送ヘッダー）を指定して MCP エンドポイントに認証なしで送り、応答を返す。
func mcpUnauthorizedAt(t *testing.T, s *Server, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, s.cfg.MCPAPIPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// ALLOWED_HOSTS（Config.AllowedHosts）を指定したサイトでは、一覧に無い（偽の）Host で届いた要求にも、その Host を
// OAuth のメタデータに載せない。BaseURL のホストが一覧にあれば BaseURL、無ければ一覧の先頭のホストを使う。
func TestOAuthMetadataAllowedHosts(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: "https://cms.example.jp", AllowedHosts: []string{"cms.example.jp", "admin.example.jp"},
		DisableScheduler: true}, nil)
	for _, tc := range []struct{ host, want string }{
		{"cms.example.jp", "https://cms.example.jp"},
		{"admin.example.jp", "http://admin.example.jp"},
		{"evil.example", "https://cms.example.jp"},
		{"evil.example:8443", "https://cms.example.jp"},
		{"127.0.0.1:8080", "https://cms.example.jp"},
	} {
		if got := oauthMetadataAt(t, env.server, tc.host, nil); got != tc.want {
			t.Errorf("host %s: issuer = %q, want %q", tc.host, got, tc.want)
		}
	}

	// BaseURL が静的な公開先（ホストが一覧に無い）なら、一覧の先頭のホストを使う（スキームは要求のもの）。
	env.server.setBaseURL("https://example.pages.dev")
	if got := oauthMetadataAt(t, env.server, "evil.example", nil); got != "http://cms.example.jp" {
		t.Errorf("static base url, spoofed host: issuer = %q", got)
	}

	// BaseURL が無くても、ポート付きの値はポートまで使う。
	noBase := newTestEnvWith(t, Config{AllowedHosts: []string{"CMS.example.jp:8443"}, DisableScheduler: true}, nil)
	if got := oauthMetadataAt(t, noBase.server, "evil.example", nil); got != "http://cms.example.jp:8443" {
		t.Errorf("no base url, spoofed host: issuer = %q", got)
	}
	if got := oauthMetadataAt(t, noBase.server, "cms.example.jp:8443", nil); got != "http://cms.example.jp:8443" {
		t.Errorf("no base url, allowed host: issuer = %q", got)
	}

	// ALLOWED_HOSTS が無ければ、今までどおり要求の Host を使う。
	open := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	if got := oauthMetadataAt(t, open.server, "sorus.local:8080", nil); got != "http://sorus.local:8080" {
		t.Errorf("no allowed hosts: issuer = %q", got)
	}
}

// ALLOWED_HOSTS を指定したサーバーモードでは、MCP エンドポイントも一覧に無い Host の要求を認証より前に 403 で断る。
// 指定しなければ、社内 LAN のアドレス・ホスト名でも使えるよう、書式の正しい Host はすべて受け付ける。
func TestMCPRejectsHostsOutsideAllowedHosts(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: "https://cms.example.jp", AllowedHosts: []string{"cms.example.jp"}, DisableScheduler: true}, nil)
	for host, want := range map[string]int{
		"cms.example.jp":      http.StatusUnauthorized,
		"cms.example.jp:8080": http.StatusUnauthorized,
		"evil.example":        http.StatusForbidden,
		"127.0.0.1:8080":      http.StatusForbidden,
		"192.168.1.20:8080":   http.StatusForbidden,
	} {
		if rec := mcpUnauthorizedAt(t, env.server, host, nil); rec.Code != want {
			t.Errorf("host %s: status %d, want %d: %s", host, rec.Code, want, rec.Body)
		}
	}
	open := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	for _, host := range []string{"evil.example", "192.168.1.20:8080", "sorus.local:8080"} {
		if rec := mcpUnauthorizedAt(t, open.server, host, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("no allowed hosts, host %s: status %d, want 401", host, rec.Code)
		}
	}
	// デスクトップモードは今までどおり、この PC 自身を指す Host だけを受け付ける（ALLOWED_HOSTS とは無関係）。
	desktop := newTestEnvWith(t, Config{Desktop: true, DisableScheduler: true}, nil)
	if rec := mcpUnauthorizedAt(t, desktop.server, "127.0.0.1:8080", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("desktop loopback: status %d, want 401", rec.Code)
	}
	if rec := mcpUnauthorizedAt(t, desktop.server, "evil.example", nil); rec.Code != http.StatusForbidden {
		t.Errorf("desktop foreign host: status %d, want 403", rec.Code)
	}
}

// ALLOWED_HOSTS を指定した公開サーバーでは、一覧に無い Host で届いた招待の発行や MCP の案内にも、その Host を載せない。
func TestSyncURLsIgnoreHostsOutsideAllowedHosts(t *testing.T) {
	t.Setenv(mcp.TrustedProxiesEnv, "")
	env := newInviteSyncEnvWith(t, Config{AllowedHosts: []string{"media.example.jp"}, TrustedProxies: []string{"192.0.2.1"}})
	adminTok, _ := env.recordedToken("admin", auth.RoleAdmin)

	rec := env.syncAt("evil.example", "https", adminTok, issueInviteRequest(auth.RoleWriter, "鈴木", 7))
	expectSyncStatus(t, rec, http.StatusOK)
	inv, err := DecodeInviteCode(decodeSync[SyncIssueInviteResponse](t, rec).InviteCode)
	if err != nil || inv.URL != "https://media.example.jp"+env.server.cfg.SyncAPIPath {
		t.Fatalf("invite = %+v, %v", inv, err)
	}

	rec = env.syncAt("evil.example", "https", adminTok, SyncRequest{Action: SyncActionMCPInfo})
	expectSyncStatus(t, rec, http.StatusOK)
	if info := decodeSync[SyncMCPInfoResponse](t, rec); info.URL != "https://media.example.jp"+env.server.cfg.MCPAPIPath {
		t.Errorf("mcp_info url = %q", info.URL)
	}
	rec = env.syncAt("evil.example", "https", adminTok, SyncRequest{Action: SyncActionMCPIssueLoginCode})
	expectSyncStatus(t, rec, http.StatusOK)
	if lc := decodeSync[SyncMCPLoginCodeResponse](t, rec); lc.AuthorizeURL != "https://media.example.jp"+OAuthAuthorizePath {
		t.Errorf("login code authorize url = %q", lc.AuthorizeURL)
	}
}

// 転送ヘッダーを信じるリバースプロキシは Config.TrustedProxies だけで決め、OAuth のメタデータ・MCP の 401・回数の
// 制限のどれも同じ一覧を使う。環境変数 TRUSTED_PROXIES には左右されない。
func TestServerSharesConfiguredTrustedProxies(t *testing.T) {
	https := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "198.51.100.7"} // 接続元は 192.0.2.1
	mcpMetadata := func(s *Server, origin string) string {
		return `resource_metadata="` + origin + mcp.ProtectedResourceMetadataPathFor(s.cfg.MCPAPIPath) + `"`
	}
	req := httptest.NewRequest(http.MethodPost, OAuthTokenPath, nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.7")

	// Config で信頼したプロキシの転送ヘッダーは、環境変数が空でも使う。
	t.Setenv(mcp.TrustedProxiesEnv, "")
	trusted := newTestEnvWith(t, Config{TrustedProxies: []string{"192.0.2.0/24"}, DisableScheduler: true}, nil)
	if got := oauthMetadataAt(t, trusted.server, "admin.example.jp", https); got != "https://admin.example.jp" {
		t.Errorf("configured proxy: issuer = %q", got)
	}
	if h := mcpUnauthorizedAt(t, trusted.server, "admin.example.jp", https).Header().Get("WWW-Authenticate"); !strings.Contains(h, mcpMetadata(trusted.server, "https://admin.example.jp")) {
		t.Errorf("configured proxy: WWW-Authenticate = %q", h)
	}
	if got := oauthClientKey(req, trusted.server.oauthLimits.proxies); got != "198.51.100.7" {
		t.Errorf("configured proxy: rate limit key = %q", got)
	}

	// Config で信頼していなければ、環境変数が接続元を信頼していても転送ヘッダーを使わない。
	t.Setenv(mcp.TrustedProxiesEnv, "192.0.2.0/24")
	untrusted := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	if got := oauthMetadataAt(t, untrusted.server, "admin.example.jp", https); got != "http://admin.example.jp" {
		t.Errorf("env only: issuer = %q", got)
	}
	if h := mcpUnauthorizedAt(t, untrusted.server, "admin.example.jp", https).Header().Get("WWW-Authenticate"); !strings.Contains(h, mcpMetadata(untrusted.server, "http://admin.example.jp")) {
		t.Errorf("env only: WWW-Authenticate = %q", h)
	}
	if got := oauthClientKey(req, untrusted.server.oauthLimits.proxies); got != "192.0.2.1" {
		t.Errorf("env only: rate limit key = %q", got)
	}
}
