package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"sorus/internal/mcp"
)

// RFC 7636 付録 B の PKCE の例。
const (
	oauthTestVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	oauthTestChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	oauthTestRedirect  = "https://claude.ai/api/mcp/auth_callback"
)

// oauthJSON は c で JSON の要求を送り、応答を返す（CSRF トークンや Cookie は付けない。AI アプリのサーバーからの要求）。
func (e *testEnv) oauthJSON(method, path, contentType, body string) (*http.Response, map[string]any) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// registerOAuthClient は DCR で AI アプリを登録し、client_id を返す。
func (e *testEnv) registerOAuthClient(name string, redirects ...string) string {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": redirects,
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
	res, out := e.oauthJSON(http.MethodPost, OAuthRegisterPath, "application/json", string(body))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("register: status %d: %v", res.StatusCode, out)
	}
	id, _ := out["client_id"].(string)
	if !strings.HasPrefix(id, mcp.OAuthClientIDPrefix) || out["token_endpoint_auth_method"] != "none" {
		e.t.Fatalf("register response = %v", out)
	}
	return id
}

// tokenRequest は /oauth/token にフォームを送る。
func (e *testEnv) tokenRequest(form url.Values) (*http.Response, map[string]any) {
	e.t.Helper()
	return e.oauthJSON(http.MethodPost, OAuthTokenPath, "application/x-www-form-urlencoded", form.Encode())
}

// authorizeQuery は認可要求のクエリを返す。
func authorizeQuery(clientID string, overrides map[string]string) string {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {oauthTestRedirect},
		"code_challenge": {oauthTestChallenge}, "code_challenge_method": {"S256"}, "state": {"st-123"},
		"scope": {"mcp"},
	}
	for k, v := range overrides {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	return OAuthAuthorizePath + "?" + q.Encode()
}

var oauthCSRFPattern = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// consentForm は同意画面のフォームの値（CSRF トークンと隠し欄）を読み取る。
func consentForm(t *testing.T, body string) url.Values {
	t.Helper()
	form := url.Values{}
	m := oauthCSRFPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("consent page has no csrf token:\n%.2000s", body)
	}
	form.Set("_csrf", m[1])
	for _, f := range regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`).FindAllStringSubmatch(body, -1) {
		if f[1] != "_csrf" {
			form.Set(f[1], htmlUnescape(f[2]))
		}
	}
	return form
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#43;", "+", "&#34;", `"`, "&#39;", "'", "&lt;", "<", "&gt;", ">").Replace(s)
}

func TestOAuthDiscoveryMetadata(t *testing.T) {
	env := newTestEnv(t)
	origin := env.srv.URL

	for _, p := range []string{mcp.ProtectedResourceMetadataPath, mcp.ProtectedResourceMetadataPath + mcp.DefaultBasePath} {
		res, out := env.oauthJSON(http.MethodGet, p, "", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", p, res.StatusCode)
		}
		if out["resource"] != origin+"/api/mcp" || !strings.Contains(strings.Join(anyStrings(out["authorization_servers"]), ","), origin) ||
			strings.Join(anyStrings(out["bearer_methods_supported"]), ",") != "header" {
			t.Errorf("GET %s = %v", p, out)
		}
		if res.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("GET %s: no CORS header", p)
		}
	}

	res, out := env.oauthJSON(http.MethodGet, "/.well-known/oauth-authorization-server", "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authorization server metadata: status %d", res.StatusCode)
	}
	want := map[string]string{
		"issuer":                 origin,
		"authorization_endpoint": origin + "/oauth/authorize",
		"token_endpoint":         origin + "/oauth/token",
		"registration_endpoint":  origin + "/oauth/register",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %v, want %s", k, out[k], v)
		}
	}
	for k, v := range map[string]string{
		"response_types_supported":              "code",
		"grant_types_supported":                 "authorization_code,refresh_token",
		"code_challenge_methods_supported":      "S256",
		"token_endpoint_auth_methods_supported": "none",
	} {
		if got := strings.Join(anyStrings(out[k]), ","); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// ブラウザで動く MCP クライアントの CORS のプリフライトに応じる。
	req, _ := http.NewRequest(http.MethodOptions, env.srv.URL+OAuthTokenPath, nil)
	req.Header.Set("Origin", "https://inspector.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	pre, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pre.Body.Close()
	if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: status %d, ACAO %q", pre.StatusCode, pre.Header.Get("Access-Control-Allow-Origin"))
	}

	// /api/mcp の 401 は保護されたリソースのメタデータの場所を知らせる。
	r, _ := env.mcpCall("", "ping", map[string]any{})
	if h := r.Header.Get("WWW-Authenticate"); r.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(h, `resource_metadata="`+origin+`/.well-known/oauth-protected-resource"`) {
		t.Errorf("unauthenticated MCP: status %d, WWW-Authenticate %q", r.StatusCode, h)
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func TestOAuthDynamicClientRegistrationValidation(t *testing.T) {
	env := newTestEnv(t)
	id := env.registerOAuthClient("Claude\x00 Desktop", oauthTestRedirect, "http://127.0.0.1:33418/callback", "http://localhost:6274/cb")
	var stored struct {
		Name string `db:"client_name"`
		URIs string `db:"redirect_uris"`
	}
	if err := env.db.Get(&stored, `SELECT client_name, redirect_uris FROM oauth_clients WHERE client_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Claude Desktop" || !strings.Contains(stored.URIs, "127.0.0.1:33418") {
		t.Errorf("stored client = %+v", stored)
	}

	for _, tt := range []struct {
		name, contentType, body, wantErr string
	}{
		{"http non-loopback redirect", "application/json", `{"redirect_uris":["http://evil.example/cb"]}`, "invalid_redirect_uri"},
		{"custom scheme redirect", "application/json", `{"redirect_uris":["cursor://anysphere/cb"]}`, "invalid_redirect_uri"},
		{"fragment in redirect", "application/json", `{"redirect_uris":["https://claude.ai/cb#x"]}`, "invalid_redirect_uri"},
		{"one bad redirect among good", "application/json", `{"redirect_uris":["https://claude.ai/cb","http://192.168.1.2/cb"]}`, "invalid_redirect_uri"},
		{"no redirect uris", "application/json", `{"client_name":"x"}`, "invalid_redirect_uri"},
		{"implicit grant", "application/json", `{"redirect_uris":["https://claude.ai/cb"],"grant_types":["implicit"]}`, "invalid_client_metadata"},
		{"token response type", "application/json", `{"redirect_uris":["https://claude.ai/cb"],"response_types":["token"]}`, "invalid_client_metadata"},
		{"not json", "application/x-www-form-urlencoded", `redirect_uris=https://claude.ai/cb`, "invalid_client_metadata"},
		{"broken json", "application/json", `{"redirect_uris":`, "invalid_client_metadata"},
	} {
		res, out := env.oauthJSON(http.MethodPost, OAuthRegisterPath, tt.contentType, tt.body)
		if res.StatusCode != http.StatusBadRequest || out["error"] != tt.wantErr {
			t.Errorf("%s: status %d, body %v; want 400 %s", tt.name, res.StatusCode, out, tt.wantErr)
		}
	}
	if n := env.countRows(`SELECT COUNT(*) FROM oauth_clients`); n != 1 {
		t.Errorf("oauth_clients rows = %d, want 1 (rejected registrations must not be stored)", n)
	}
}

// TestOAuthAuthorizationCodeFlowEndToEnd は登録 → 認可（未ログインならログイン画面を経由）→ 同意 → トークン →
// MCP の利用 → リフレッシュ → 管理画面での取り消し、までの全体を確かめる。
func TestOAuthAuthorizationCodeFlowEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	clientID := env.registerOAuthClient("Claude", oauthTestRedirect)
	browser := env.newClient()

	// 未ログインならログイン画面へ。ログイン後に同じ認可要求へ戻る。
	authPath := authorizeQuery(clientID, nil)
	res := browser.get(authPath)
	loginURL := expectRedirect(t, res, "/admin/login?next=")
	if next, _ := url.Parse(loginURL); next.Query().Get("next") != authPath {
		t.Fatalf("next = %q, want %q", next.Query().Get("next"), authPath)
	}
	expectStatus(t, browser.get(loginURL), http.StatusOK)
	res = browser.login(chief.Username, testPassword)
	if loc := expectRedirect(t, res, OAuthAuthorizePath+"?"); loc != authPath {
		t.Fatalf("after login Location = %q, want %q", loc, authPath)
	}

	// ログイン済みなら同意画面を出す。
	res = browser.get(authPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="oauth-consent"`, "Claude", "claude.ai", "佐藤デスク", `name="decision" value="allow"`)
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("consent Cache-Control = %q, want no-store", cc)
	}
	form := consentForm(t, res.Body)

	// CSRF トークンの無い・違う許可は断る。
	bad := cloneValues(form)
	bad.Set("_csrf", "forged")
	bad.Set("decision", "allow")
	expectStatus(t, browser.postRaw(OAuthAuthorizePath, bad), http.StatusForbidden)
	noToken := cloneValues(form)
	noToken.Del("_csrf")
	noToken.Set("decision", "allow")
	expectStatus(t, browser.postRaw(OAuthAuthorizePath, noToken), http.StatusForbidden)

	// 「許可しない」はリダイレクト先へ access_denied を返す。
	deny := cloneValues(form)
	deny.Set("decision", "deny")
	res = browser.postRaw(OAuthAuthorizePath, deny)
	expectStatus(t, res, http.StatusFound)
	if loc, _ := url.Parse(res.Header.Get("Location")); loc.Query().Get("error") != "access_denied" ||
		loc.Query().Get("state") != "st-123" || loc.Query().Get("code") != "" {
		t.Errorf("deny Location = %q", res.Header.Get("Location"))
	}

	// 「許可する」で認可コードを発行してリダイレクトする。
	allow := cloneValues(form)
	allow.Set("decision", "allow")
	res = browser.postRaw(OAuthAuthorizePath, allow)
	expectStatus(t, res, http.StatusFound)
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != oauthTestRedirect || loc.Query().Get("state") != "st-123" {
		t.Fatalf("allow Location = %q", res.Header.Get("Location"))
	}
	code := loc.Query().Get("code")
	if !strings.HasPrefix(code, mcp.OAuthCodePrefix) {
		t.Fatalf("code = %q", code)
	}

	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {oauthTestRedirect}, "code_verifier": {oauthTestVerifier}}

	// PKCE の検証に失敗したら invalid_grant。
	wrong := cloneValues(exchange)
	wrong.Set("code_verifier", strings.Repeat("A", 43))
	if r, out := env.tokenRequest(wrong); r.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Errorf("wrong verifier: status %d, %v", r.StatusCode, out)
	}
	noVerifier := cloneValues(exchange)
	noVerifier.Del("code_verifier")
	if r, out := env.tokenRequest(noVerifier); r.StatusCode != http.StatusBadRequest || out["error"] != "invalid_request" {
		t.Errorf("missing verifier: status %d, %v", r.StatusCode, out)
	}
	otherRedirect := cloneValues(exchange)
	otherRedirect.Set("redirect_uri", "https://claude.ai/other")
	if r, out := env.tokenRequest(otherRedirect); r.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Errorf("redirect_uri mismatch: status %d, %v", r.StatusCode, out)
	}
	unknownClient := cloneValues(exchange)
	unknownClient.Set("client_id", "sorusc_unknown")
	if r, out := env.tokenRequest(unknownClient); r.StatusCode != http.StatusUnauthorized || out["error"] != "invalid_client" {
		t.Errorf("unknown client: status %d, %v", r.StatusCode, out)
	}

	r, tok := env.tokenRequest(exchange)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("token: status %d, %v", r.StatusCode, tok)
	}
	if r.Header.Get("Cache-Control") != "no-store" || tok["token_type"] != "Bearer" || tok["expires_in"] != float64(3600) || tok["scope"] != "mcp" {
		t.Errorf("token response = %v (Cache-Control %q)", tok, r.Header.Get("Cache-Control"))
	}
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)

	// コードは使い捨て。
	if r, out := env.tokenRequest(exchange); r.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Errorf("reused code: status %d, %v", r.StatusCode, out)
	}

	// アクセストークンで MCP サーバーを使える（許可したユーザーの権限）。
	httpRes, out := env.mcpCall(access, "tools/call", map[string]any{"name": "cms_save_draft", "arguments": map[string]any{
		"title": "OAuthで書いた下書き", "content_markdown": "本文",
	}})
	if httpRes.StatusCode != http.StatusOK || out["error"] != nil {
		t.Fatalf("MCP with oauth token: status %d, %v", httpRes.StatusCode, out)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE title = 'OAuthで書いた下書き' AND author_id = ?`, chief.ID); n != 1 {
		t.Errorf("draft saved by oauth token: rows = %d, want 1 (author %s)", n, chief.ID)
	}

	// リフレッシュでトークンが入れ替わり、古いアクセストークンは使えない。
	r, tok2 := env.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("refresh: status %d, %v", r.StatusCode, tok2)
	}
	access2, _ := tok2["access_token"].(string)
	if access2 == access || tok2["refresh_token"] == refresh || tok2["refresh_token"] == "" {
		t.Fatalf("refresh did not rotate: %v", tok2)
	}
	if res, _ := env.mcpCall(access, "ping", map[string]any{}); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("old access token after refresh: status %d, want 401", res.StatusCode)
	}
	if res, _ := env.mcpCall(access2, "ping", map[string]any{}); res.StatusCode != http.StatusOK {
		t.Errorf("new access token: status %d, want 200", res.StatusCode)
	}
	if r, out := env.tokenRequest(url.Values{"grant_type": {"password"}, "client_id": {clientID}}); r.StatusCode != http.StatusBadRequest ||
		out["error"] != "unsupported_grant_type" {
		t.Errorf("unsupported grant: status %d, %v", r.StatusCode, out)
	}

	// 管理画面の MCP 連携に、許可した AI アプリが出る。執筆者からは見えず、編集者本人と管理者には見える。
	owner := env.loggedIn(chief)
	pages := recordPages(env)
	d := pages.renderedPage(t, owner, MCPSettingsPath, "mcp_tokens").Data.(mcpTokensData)
	if len(d.OAuthApps) != 1 || d.OAuthApps[0].ClientName != "Claude" || d.OAuthApps[0].OwnerName != "佐藤デスク" ||
		d.OAuthApps[0].LastUsedAt == nil || d.OAuthApps[0].ExpiresAt.IsZero() {
		t.Fatalf("OAuthApps = %+v", d.OAuthApps)
	}
	appID := d.OAuthApps[0].ID
	writer := env.loggedIn(studentA)
	if wd := pages.renderedPage(t, writer, MCPSettingsPath, "mcp_tokens").Data.(mcpTokensData); len(wd.OAuthApps) != 0 {
		t.Errorf("writer sees other users' OAuth apps: %+v", wd.OAuthApps)
	}
	if ad := pages.renderedPage(t, env.loggedIn(admin), MCPSettingsPath, "mcp_tokens").Data.(mcpTokensData); len(ad.OAuthApps) != 1 {
		t.Errorf("admin OAuthApps = %+v, want the editor's app", ad.OAuthApps)
	}
	// 他の人の連携は取り消せない。
	expectStatus(t, writer.postForm(MCPSettingsPath+"/oauth/"+appID+"/revoke", nil), http.StatusNotFound)

	// 取り消すと、アクセストークンもリフレッシュトークンも使えなくなる。
	expectRedirect(t, owner.postForm(MCPSettingsPath+"/oauth/"+appID+"/revoke", nil), MCPSettingsPath+"?flash=")
	if res, _ := env.mcpCall(access2, "ping", map[string]any{}); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("access token after revoke: status %d, want 401", res.StatusCode)
	}
	if r, out := env.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok2["refresh_token"].(string)}, "client_id": {clientID}}); r.StatusCode != http.StatusBadRequest ||
		out["error"] != "invalid_grant" {
		t.Errorf("refresh after revoke: status %d, %v", r.StatusCode, out)
	}
	if d := pages.renderedPage(t, owner, MCPSettingsPath, "mcp_tokens").Data.(mcpTokensData); len(d.OAuthApps) != 0 {
		t.Errorf("revoked app is still listed: %+v", d.OAuthApps)
	}
}

// TestOAuthAuthorizeRequestValidation は認可要求の検証を確かめる。未登録の AI アプリ・登録と違うリダイレクト先は、
// リダイレクトせずにこの sorus の画面で断り、それ以外の不備はリダイレクト先へ error で返す。
func TestOAuthAuthorizeRequestValidation(t *testing.T) {
	env := newTestEnv(t)
	clientID := env.registerOAuthClient("Claude", oauthTestRedirect)
	browser := env.loggedIn(chief)

	for _, tt := range []struct {
		name      string
		overrides map[string]string
	}{
		{"unknown client", map[string]string{"client_id": "sorusc_unknown"}},
		{"missing client", map[string]string{"client_id": ""}},
		{"unregistered redirect", map[string]string{"redirect_uri": "https://evil.example/cb"}},
		{"redirect prefix only", map[string]string{"redirect_uri": oauthTestRedirect + "/extra"}},
		{"missing redirect", map[string]string{"redirect_uri": ""}},
	} {
		res := browser.get(authorizeQuery(clientID, tt.overrides))
		if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(res.Body, `id="oauth-error"`) {
			t.Errorf("%s: status %d, Location %q", tt.name, res.StatusCode, res.Header.Get("Location"))
		}
	}

	for _, tt := range []struct {
		name      string
		overrides map[string]string
		wantErr   string
	}{
		{"plain pkce", map[string]string{"code_challenge_method": "plain"}, "invalid_request"},
		{"missing pkce method", map[string]string{"code_challenge_method": ""}, "invalid_request"},
		{"missing challenge", map[string]string{"code_challenge": ""}, "invalid_request"},
		{"malformed challenge", map[string]string{"code_challenge": "short"}, "invalid_request"},
		{"implicit flow", map[string]string{"response_type": "token"}, "unsupported_response_type"},
		{"foreign resource", map[string]string{"resource": "https://other.example/api"}, "invalid_target"},
	} {
		res := browser.get(authorizeQuery(clientID, tt.overrides))
		if res.StatusCode != http.StatusFound {
			t.Errorf("%s: status %d, want 302", tt.name, res.StatusCode)
			continue
		}
		loc, _ := url.Parse(res.Header.Get("Location"))
		if loc.Host != "claude.ai" || loc.Query().Get("error") != tt.wantErr || loc.Query().Get("state") != "st-123" {
			t.Errorf("%s: Location = %q, want error=%s", tt.name, res.Header.Get("Location"), tt.wantErr)
		}
	}

	// 自分の MCP エンドポイントを指す resource は受け付け、トークンに記録する。
	res := browser.get(authorizeQuery(clientID, map[string]string{"resource": env.srv.URL + "/api/mcp"}))
	expectStatus(t, res, http.StatusOK)
}

func TestSafeLoginNext(t *testing.T) {
	for _, tt := range []struct {
		next string
		ok   bool
	}{
		{"/oauth/authorize?client_id=x&state=y", true},
		{"/admin/settings/mcp", true},
		{"/admin", true},
		{"//evil.example/oauth/authorize", false},
		{"https://evil.example/admin", false},
		{"/\\evil.example", false},
		{"/admin/login", false},
		{"/admin/login?next=/admin", false},
		{"/admin/logout", false},
		{"/admin/../oauth/token", false},
		{"/oauth/token", false},
		{"/articles/foo", false},
		{"javascript:alert(1)", false},
		{"", false},
	} {
		if _, ok := safeLoginNext(tt.next); ok != tt.ok {
			t.Errorf("safeLoginNext(%q) ok = %v, want %v", tt.next, ok, tt.ok)
		}
	}
}

// TestLoginNextIgnoresUnsafeTarget はログイン画面の ?next= に別サイトの URL を渡しても、ログイン後はいつもの画面へ
// 進むことを確かめる（開いたリダイレクトにしない）。
func TestLoginNextIgnoresUnsafeTarget(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	expectStatus(t, c.get("/admin/login?next="+url.QueryEscape("https://evil.example/")), http.StatusOK)
	expectRedirect(t, c.login(studentA.Username, testPassword), "/admin/articles")
}

// TestMCPDNSRebindingOnServer は組み込みの MCP サーバーの Host・Origin の検査を確かめる。サーバーモードでは社内 LAN の
// アドレスでも使え、デスクトップモードではこの PC を指す Host だけを受け付ける。
func TestMCPDNSRebindingOnServer(t *testing.T) {
	send := func(env *testEnv, token, host, origin string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, env.srv.URL+mcp.DefaultBasePath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	server := newTestEnvWith(t, Config{BaseURL: "https://media.example.jp", MCPAllowedOrigins: []string{"https://editor.example.jp"}}, nil)
	token := issueMCPToken(server.loggedIn(studentA), "LAN", "30")
	for _, tt := range []struct {
		name, host, origin string
		want               int
	}{
		{"lan ip", "192.168.10.5:8080", "", http.StatusOK},
		{"lan hostname", "sorus.local:8080", "", http.StatusOK},
		{"malformed host", "sorus.local:http", "", http.StatusForbidden},
		{"foreign origin", "192.168.10.5:8080", "https://evil.example", http.StatusForbidden},
		{"base url origin", "192.168.10.5:8080", "https://media.example.jp", http.StatusOK},
		{"configured origin", "192.168.10.5:8080", "https://editor.example.jp", http.StatusOK},
	} {
		if got := send(server, token, tt.host, tt.origin); got != tt.want {
			t.Errorf("server mode %s: status %d, want %d", tt.name, got, tt.want)
		}
	}

	desktop := startTestEnv(t, Config{Desktop: true, DisableScheduler: true, MCPAllowedOrigins: []string{"https://editor.example.jp"}}, nil, false)
	dtoken := issueMCPToken(desktop.loggedIn(studentA), "PC", "30")
	for _, tt := range []struct {
		name, host, origin string
		want               int
	}{
		{"loopback", strings.TrimPrefix(desktop.srv.URL, "http://"), "", http.StatusOK},
		{"localhost", "localhost:1234", "", http.StatusOK},
		{"rebinding attacker", "evil.example:1234", "", http.StatusForbidden},
		{"rebinding attacker with its own origin", "evil.example:1234", "http://evil.example:1234", http.StatusForbidden},
		{"lan ip", "192.168.10.5:8080", "", http.StatusForbidden},
		{"configured host", "editor.example.jp", "", http.StatusOK},
	} {
		if got := send(desktop, dtoken, tt.host, tt.origin); got != tt.want {
			t.Errorf("desktop mode %s: status %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestOAuthEndpointsBypassSiteAccessGate は閲覧制限（Basic 認証）を有効にしたサーバーでも、AI アプリのサーバーが
// OAuth のメタデータ・登録・トークンのエンドポイントを使えることを確かめる（サイトの内容は含まない）。
func TestOAuthEndpointsBypassSiteAccessGate(t *testing.T) {
	env := newTestEnv(t)
	enableBasicAuth(t, env.loggedIn(admin), "viewer", "viewer-pass-123")
	expectBasicAuthChallenge(t, env.newClient().get("/"))
	for _, p := range []string{mcp.ProtectedResourceMetadataPath, "/.well-known/oauth-authorization-server"} {
		if res, _ := env.oauthJSON(http.MethodGet, p, "", ""); res.StatusCode != http.StatusOK {
			t.Errorf("GET %s behind site gate: status %d, want 200", p, res.StatusCode)
		}
	}
	env.registerOAuthClient("Claude", oauthTestRedirect)
	if res, out := env.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "client_id": {"sorusc_unknown"}}); res.StatusCode != http.StatusUnauthorized ||
		out["error"] != "invalid_client" {
		t.Errorf("token endpoint behind site gate: status %d, %v", res.StatusCode, out)
	}
}

// TestOAuthInPublicOnlyModeUsesConfirmationCode は公開専用モード（管理画面のユーザーが居ない）でも OAuth のエンドポイントを
// 登録し、同意画面は管理画面のログインへ送らずに、接続した PC で表示する確認コードを求めることを確かめる
// （詳しい流れは mcp_public_oauth_test.go）。MCP サーバーのパスを渡さなければ、
// この起動のあいだだけの秘匿パスに置き、固定の /api/mcp は 404 にする。
func TestOAuthInPublicOnlyModeUsesConfirmationCode(t *testing.T) {
	env := newTestEnvWith(t, Config{PublicOnly: true}, nil)
	if p := env.server.cfg.MCPAPIPath; !strings.HasPrefix(p, "/api/mcp-") {
		t.Fatalf("MCPAPIPath in public-only mode = %q, want a secret path", p)
	}
	if res, _ := env.oauthJSON(http.MethodGet, "/.well-known/oauth-authorization-server", "", ""); res.StatusCode != http.StatusOK {
		t.Errorf("authorization server metadata in public-only mode: status %d, want 200", res.StatusCode)
	}
	if res, _ := env.oauthJSON(http.MethodGet, mcp.ProtectedResourceMetadataPath, "", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("shared protected resource metadata in public-only mode: status %d, want 404", res.StatusCode)
	}
	clientID := env.registerOAuthClient("Claude", oauthTestRedirect)
	res := env.newClient().get(authorizeQuery(clientID, nil))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="oauth-login"`, `name="login_code"`)
	if r, _ := env.mcpCall("", "ping", map[string]any{}); r.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/mcp in public-only mode: status %d, want 404", r.StatusCode)
	}
}
