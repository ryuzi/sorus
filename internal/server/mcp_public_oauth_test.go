package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/mcp"
)

func boolPtr(b bool) *bool { return &b }

// enableMultilingual は基本設定の多言語対応を ON にする（新しいサイトの既定は OFF。isMultilingualEnabled は要求ごとに読む）。
func enableMultilingual(t *testing.T, db *sqlx.DB) {
	t.Helper()
	setSettingForTest(t, db, settingMultilingualEnabled, "1")
}

func setSettingForTest(t *testing.T, db *sqlx.DB, key, value string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		t.Fatal(err)
	}
}

// newPublicMCPEnv は同期 API と MCP サーバーを秘匿したパスに置いた公開専用モードのサーバーを起動する。
func newPublicMCPEnv(t *testing.T) *syncEnv {
	t.Helper()
	p, err := NewMCPAPIPath()
	if err != nil {
		t.Fatal(err)
	}
	return newSyncEnvWith(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t), MCPAPIPath: p}, nil)
}

// mcpCallAt は path の MCP サーバーに token で JSON-RPC の要求を送る（TLS で起動したサーバーでも使える）。
func (e *testEnv) mcpCallAt(path, token, method string, params any) (*http.Response, map[string]any) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(string(body)))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// mcpToolAt は path の MCP サーバーのツールを呼び、ツールのエラーかと本文を返す。
func (e *testEnv) mcpToolAt(path, token, name string, args map[string]any) (status int, isError bool, text string) {
	e.t.Helper()
	res, out := e.mcpCallAt(path, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, true, ""
	}
	if out["error"] != nil {
		e.t.Fatalf("%s: JSON-RPC error %v", name, out["error"])
	}
	result, _ := out["result"].(map[string]any)
	isError, _ = result["isError"].(bool)
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	return res.StatusCode, isError, text
}

// claimsOf は e の署名鍵で発行したトークンの内容を返す。
func (e *syncEnv) claimsOf(token string) *auth.Claims {
	e.t.Helper()
	claims, err := auth.VerifyToken(e.priv.Public().(ed25519.PublicKey), token)
	if err != nil {
		e.t.Fatal(err)
	}
	return claims
}

// issueOAuthAccessToken は userID に OAuth 2.1 のアクセストークンを発行する（同意画面を経ずに保存処理で発行する）。
func issueOAuthAccessToken(t *testing.T, store *mcp.OAuthStore, userID string) (access, clientID string) {
	t.Helper()
	ctx := context.Background()
	client, err := store.RegisterClient(ctx, "Claude", []string{oauthTestRedirect})
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.IssueCode(ctx, mcp.OAuthCodeRequest{ClientID: client.ClientID, UserID: userID, RedirectURI: oauthTestRedirect,
		CodeChallenge: oauthTestChallenge, Scope: mcp.OAuthScope})
	if err != nil {
		t.Fatal(err)
	}
	set, err := store.ExchangeCode(ctx, code, client.ClientID, oauthTestRedirect, oauthTestVerifier)
	if err != nil {
		t.Fatal(err)
	}
	return set.AccessToken, client.ClientID
}

func TestNormalizeMCPAPIPath(t *testing.T) {
	p, err := NewMCPAPIPath()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^/api/mcp-[0-9a-f]{32}$`).MatchString(p) {
		t.Errorf("NewMCPAPIPath = %q", p)
	}
	if q, _ := NewMCPAPIPath(); q == p {
		t.Errorf("NewMCPAPIPath returned the same path twice: %q", p)
	}
	for in, want := range map[string]string{
		"/api/mcp": "/api/mcp", " /api/mcp-abc/ ": "/api/mcp-abc", "api/agents/x": "/api/agents/x", p: p,
	} {
		if got, err := NormalizeMCPAPIPath(in); err != nil || got != want {
			t.Errorf("NormalizeMCPAPIPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/", "/api", "/mcp", "/admin/mcp", "/api/../admin", "/api//mcp", "/api/:id", "/api/mcp*",
		"/api/mcp x", "/api/" + strings.Repeat("a", 300)} {
		if got, err := NormalizeMCPAPIPath(bad); !errors.Is(err, ErrInvalidMCPAPIPath) {
			t.Errorf("NormalizeMCPAPIPath(%q) = %q, %v; want ErrInvalidMCPAPIPath", bad, got, err)
		}
	}
}

func TestResolveMCPAPIPath(t *testing.T) {
	if p, err := resolveMCPAPIPath("", false, DefaultSyncAPIPath); err != nil || p != mcp.DefaultBasePath {
		t.Errorf("server mode default = %q, %v", p, err)
	}
	// 公開専用モードで省略したら、この起動のあいだだけの秘匿パス。
	p1, err := resolveMCPAPIPath("", true, DefaultSyncAPIPath)
	if err != nil || !strings.HasPrefix(p1, "/api/mcp-") {
		t.Errorf("public-only default = %q, %v", p1, err)
	}
	if p, err := resolveMCPAPIPath("/api/mcp-custom", false, DefaultSyncAPIPath); err != nil || p != "/api/mcp-custom" {
		t.Errorf("MCP_API_PATH in server mode = %q, %v", p, err)
	}
	for _, tc := range []struct {
		raw        string
		publicOnly bool
		sync       string
	}{
		{"/api/mcp", true, DefaultSyncAPIPath},       // 推測できるパスは公開専用モードでは使えない
		{"/api/sync", false, DefaultSyncAPIPath},     // 同期 API と同じ
		{"/api/sync/mcp", false, DefaultSyncAPIPath}, // 同期 API の下
		{"/api/x", false, "/api/x/sync"},             // 同期 API を含む
		{"/static/mcp", false, DefaultSyncAPIPath},   // /api の外
	} {
		if p, err := resolveMCPAPIPath(tc.raw, tc.publicOnly, tc.sync); !errors.Is(err, ErrInvalidMCPAPIPath) {
			t.Errorf("resolveMCPAPIPath(%q, %v, %q) = %q, %v; want error", tc.raw, tc.publicOnly, tc.sync, p, err)
		}
	}
	if _, err := New(Config{DB: newTestEnv(t).db, PublicOnly: true, MCPAPIPath: "/api/mcp"}); !errors.Is(err, ErrInvalidMCPAPIPath) {
		t.Errorf("New with /api/mcp in public-only mode: %v, want ErrInvalidMCPAPIPath", err)
	}
}

func TestProtectedResourceMetadataPathFor(t *testing.T) {
	for in, want := range map[string]string{
		"": mcp.ProtectedResourceMetadataPath, "/api/mcp": mcp.ProtectedResourceMetadataPath, "/api/mcp/": mcp.ProtectedResourceMetadataPath,
		"/api/mcp-abc": mcp.ProtectedResourceMetadataPath + "/api/mcp-abc",
	} {
		if got := mcp.ProtectedResourceMetadataPathFor(in); got != want {
			t.Errorf("ProtectedResourceMetadataPathFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// 公開専用モードでも MCP サーバーと OAuth 2.1 を秘匿したパスで使える。固定の /api/mcp と、共通の保護されたリソースの
// メタデータ（秘匿パスを載せることになる）は存在しないページと同じ 404 で、401 の WWW-Authenticate だけが秘匿パス専用の
// メタデータを案内する。
func TestPublicOnlyMCPSecretPath(t *testing.T) {
	env := newPublicMCPEnv(t)
	secret := env.server.cfg.MCPAPIPath
	origin := env.srv.URL

	for _, p := range []string{mcp.DefaultBasePath, mcp.DefaultBasePath + "/sse", mcp.DefaultBasePath + "/messages"} {
		if res, _ := env.mcpCallAt(p, "", "ping", map[string]any{}); res.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s in public-only mode: status %d, want 404", p, res.StatusCode)
		}
	}
	res, _ := env.mcpCallAt(secret, "", "ping", map[string]any{})
	wantMeta := origin + mcp.ProtectedResourceMetadataPath + secret
	if h := res.Header.Get("WWW-Authenticate"); res.StatusCode != http.StatusUnauthorized || !strings.Contains(h, `resource_metadata="`+wantMeta+`"`) {
		t.Errorf("unauthenticated POST %s: status %d, WWW-Authenticate %q (want resource_metadata %s)", secret, res.StatusCode, h, wantMeta)
	}

	for _, p := range []string{mcp.ProtectedResourceMetadataPath, mcp.ProtectedResourceMetadataPath + mcp.DefaultBasePath} {
		r, _ := env.oauthJSON(http.MethodGet, p, "", "")
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with a secret MCP path: status %d, want 404", p, r.StatusCode)
		}
	}
	r, meta := env.oauthJSON(http.MethodGet, mcp.ProtectedResourceMetadataPath+secret, "", "")
	if r.StatusCode != http.StatusOK || meta["resource"] != origin+secret ||
		strings.Join(anyStrings(meta["authorization_servers"]), ",") != origin {
		t.Errorf("GET secret metadata: status %d, %v", r.StatusCode, meta)
	}
	if r, out := env.oauthJSON(http.MethodGet, "/.well-known/oauth-authorization-server", "", ""); r.StatusCode != http.StatusOK ||
		out["authorization_endpoint"] != origin+OAuthAuthorizePath || strings.Contains(strings.ToLower(toJSON(t, out)), "mcp-") {
		t.Errorf("authorization server metadata: status %d, %v", r.StatusCode, out)
	}
	// 秘匿パスはページのスラッグにも、/api の下の別の処理にも使われない。
	if got := env.newClient().get("/api/mcp-" + strings.Repeat("0", 32)); got.StatusCode != http.StatusNotFound {
		t.Errorf("GET a different secret-like path: status %d, want 404", got.StatusCode)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// サーバーモードでも MCP_API_PATH（Config.MCPAPIPath）でパスを変えられる。変えたら /api/mcp は 404。
func TestServerModeCustomMCPPath(t *testing.T) {
	env := newTestEnvWith(t, Config{MCPAPIPath: "/api/mcp-lan"}, nil)
	if res, _ := env.mcpCallAt(mcp.DefaultBasePath, "", "ping", map[string]any{}); res.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/mcp: status %d, want 404", res.StatusCode)
	}
	res, _ := env.mcpCallAt("/api/mcp-lan", "", "ping", map[string]any{})
	if h := res.Header.Get("WWW-Authenticate"); res.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(h, mcp.ProtectedResourceMetadataPath+"/api/mcp-lan") {
		t.Errorf("POST /api/mcp-lan: status %d, WWW-Authenticate %q", res.StatusCode, h)
	}
	// 管理画面の MCP 連携の画面も、変えたパスを案内する。
	page := env.loggedIn(studentA).get(MCPSettingsPath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, env.srv.URL+"/api/mcp-lan")
}

// 公開専用モードの同意画面は、管理画面のログインの代わりに、接続した PC で表示した確認コードで本人を確かめ、招待の
// ユーザーとロールで AI アプリに許可する。発行したアクセストークンは秘匿パスの MCP サーバーで、招待のロールの権限で使え、
// 招待を取り消すと使えなくなる。
func TestPublicOnlyOAuthWithConfirmationCode(t *testing.T) {
	env := newPublicMCPEnv(t)
	secret := env.server.cfg.MCPAPIPath
	writerTok := env.token("山田（外部ライター）", auth.RoleWriter)
	clientID := env.registerOAuthClient("ChatGPT", oauthTestRedirect)
	browser := env.newClient()
	authPath := authorizeQuery(clientID, map[string]string{"resource": env.srv.URL + secret})

	// ログイン画面（/admin/login は無い）へ送らず、確認コードを入れる画面を出す。
	res := browser.get(authPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="oauth-login"`, "ChatGPT", "claude.ai", `name="login_code"`, `name="step" value="login"`)
	expectNotContains(t, res.Body, `id="oauth-consent"`, `name="ticket"`)
	form := consentForm(t, res.Body)

	// CSRF トークンが無ければ断る。
	noCSRF := cloneValues(form)
	noCSRF.Del("_csrf")
	noCSRF.Set("login_code", "ABCD-EFGH")
	expectStatus(t, browser.postRaw(OAuthAuthorizePath, noCSRF), http.StatusForbidden)

	// 違うコードは断り、もう一度入れてもらう。
	wrong := cloneValues(form)
	wrong.Set("login_code", "ABCD-EFGH")
	res = browser.postRaw(OAuthAuthorizePath, wrong)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, `id="oauth-login-error"`, `id="oauth-login"`)

	// PC が同期 API で確認コードを発行する。
	rec := env.call(writerTok, SyncRequest{Action: SyncActionMCPIssueLoginCode})
	expectSyncStatus(t, rec, http.StatusOK)
	lc := decodeSync[SyncMCPLoginCodeResponse](t, rec)
	if !regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$`).MatchString(lc.Code) ||
		lc.ExpiresAt.Sub(time.Now()) > oauthLoginCodeTTL || lc.AuthorizeURL != "http://example.com"+OAuthAuthorizePath {
		t.Errorf("login code = %+v", lc)
	}

	// 小文字・空白区切りで入れても受け付け、招待のユーザーとロールで同意画面を出す。
	good := cloneValues(form)
	good.Set("login_code", strings.ToLower(strings.ReplaceAll(lc.Code, "-", " ")))
	res = browser.postRaw(OAuthAuthorizePath, good)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="oauth-consent"`, "山田（外部ライター）", `name="ticket"`, `name="decision" value="allow"`)
	consent := consentForm(t, res.Body)
	if consent.Get("ticket") == "" || consent.Has("login_code") || consent.Has("step") {
		t.Fatalf("consent form = %v", consent)
	}
	// 確認コードは1回だけ使える。
	expectStatus(t, browser.postRaw(OAuthAuthorizePath, good), http.StatusUnprocessableEntity)

	// チケットの無い「許可する」は断る（確認コードからやり直し）。
	noTicket := cloneValues(consent)
	noTicket.Del("ticket")
	noTicket.Set("decision", "allow")
	res = browser.postRaw(OAuthAuthorizePath, noTicket)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, `id="oauth-login"`)

	allow := cloneValues(consent)
	allow.Set("decision", "allow")
	res = browser.postRaw(OAuthAuthorizePath, allow)
	expectStatus(t, res, http.StatusFound)
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != oauthTestRedirect || loc.Query().Get("state") != "st-123" {
		t.Fatalf("allow Location = %q", res.Header.Get("Location"))
	}
	// チケットも1回限り。
	expectStatus(t, browser.postRaw(OAuthAuthorizePath, allow), http.StatusUnprocessableEntity)

	r, tok := env.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "client_id": {clientID},
		"redirect_uri": {oauthTestRedirect}, "code_verifier": {oauthTestVerifier}})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("token: status %d, %v", r.StatusCode, tok)
	}
	access, _ := tok["access_token"].(string)

	// 秘匿パスの MCP サーバーを、招待のユーザー（writer）の権限で使える。
	claims := env.claimsOf(writerTok)
	if status, isErr, text := env.mcpToolAt(secret, access, "cms_save_draft", map[string]any{"title": "公開サーバーで書いた下書き", "content_markdown": "本文"}); status != http.StatusOK || isErr {
		t.Fatalf("cms_save_draft: status %d, %s", status, text)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE title = '公開サーバーで書いた下書き' AND author_id = ?`, "sync-tok-"+claims.ID); n != 1 {
		t.Errorf("draft by the invite's user: rows = %d, want 1", n)
	}
	if _, isErr, _ := env.mcpToolAt(secret, access, "cms_approve_and_publish", map[string]any{"article_id": "x"}); !isErr {
		t.Error("writer approved an article through OAuth on the public server")
	}

	// 「やめる」はチケット無しでも access_denied で戻す。
	res = browser.postRaw(OAuthAuthorizePath, func() url.Values { v := cloneValues(form); v.Set("decision", "deny"); return v }())
	expectStatus(t, res, http.StatusFound)
	if l, _ := url.Parse(res.Header.Get("Location")); l.Query().Get("error") != "access_denied" {
		t.Errorf("deny Location = %q", res.Header.Get("Location"))
	}

	// 別の認可要求（code_challenge が違う）には、確かめた結果を使い回せない。
	rec = env.call(writerTok, SyncRequest{Action: SyncActionMCPIssueLoginCode})
	lc2 := decodeSync[SyncMCPLoginCodeResponse](t, rec)
	good2 := cloneValues(form)
	good2.Set("login_code", lc2.Code)
	res = browser.postRaw(OAuthAuthorizePath, good2)
	expectStatus(t, res, http.StatusOK)
	tampered := consentForm(t, res.Body)
	tampered.Set("code_challenge", mcp.PKCEChallenge(strings.Repeat("b", 43)))
	tampered.Set("decision", "allow")
	expectStatus(t, browser.postRaw(OAuthAuthorizePath, tampered), http.StatusUnprocessableEntity)

	// 招待を取り消すと、その招待で許可した連携はすぐに使えなくなる。
	owner := env.token("owner", auth.RoleAdmin)
	expectSyncStatus(t, env.call(owner, SyncRequest{Action: SyncActionRevokeToken, JTI: claims.ID}), http.StatusOK)
	if r, _ := env.mcpCallAt(secret, access, "ping", map[string]any{}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("MCP after the invite was revoked: status %d, want 401", r.StatusCode)
	}
	// 取り消した招待の確認コードも使えない。
	revokedCode := env.call(writerTok, SyncRequest{Action: SyncActionMCPIssueLoginCode})
	expectSyncStatus(t, revokedCode, http.StatusUnauthorized)
}

// 確認コードを発行した後に招待を取り消したら、そのコードでは本人を確かめられない。
func TestPublicOnlyOAuthCodeOfRevokedInvite(t *testing.T) {
	env := newPublicMCPEnv(t)
	writerTok := env.token("取り消される人", auth.RoleWriter)
	lc := decodeSync[SyncMCPLoginCodeResponse](t, env.call(writerTok, SyncRequest{Action: SyncActionMCPIssueLoginCode}))
	expectSyncStatus(t, env.call(env.token("owner", auth.RoleAdmin), SyncRequest{Action: SyncActionRevokeToken, JTI: env.claimsOf(writerTok).ID}), http.StatusOK)
	clientID := env.registerOAuthClient("Claude", oauthTestRedirect)
	browser := env.newClient()
	form := consentForm(t, browser.get(authorizeQuery(clientID, nil)).Body)
	form.Set("login_code", lc.Code)
	expectStatus(t, browser.postRaw(OAuthAuthorizePath, form), http.StatusUnprocessableEntity)
}

// 同期 API の MCP 連携の操作: パス・URL と一覧の取得、API トークンの発行・失効、許可した連携の取り消し。一覧・失効・
// 取り消しは自分の分だけ（admin の招待は全員の分）。
func TestSyncMCPTokenManagement(t *testing.T) {
	env := newPublicMCPEnv(t)
	secret := env.server.cfg.MCPAPIPath
	writerA := env.token("ライターA", auth.RoleWriter)
	writerB := env.token("ライターB", auth.RoleWriter)
	owner := env.token("管理者", auth.RoleAdmin)

	info := decodeSync[SyncMCPInfoResponse](t, env.call(writerA, SyncRequest{Action: SyncActionMCPInfo}))
	if info.Path != secret || info.URL != "http://example.com"+secret || info.Role != auth.RoleWriter || info.Subject != "ライターA" ||
		info.ShowAll || !info.OAuth || info.Tokens == nil || len(info.Tokens) != 0 || info.OAuthApps == nil {
		t.Errorf("mcp_info = %+v", info)
	}

	for _, bad := range []*SyncMCPRequest{nil, {Name: "  "}, {Name: strings.Repeat("あ", 101)}, {Name: "x", ExpiresDays: -1},
		{Name: "x", ExpiresDays: maxSyncMCPTokenDays + 1}, {Name: "a\u0007b"}} {
		if rec := env.call(writerA, SyncRequest{Action: SyncActionMCPIssueToken, MCP: bad}); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("mcp_issue_token %+v: status %d, want 422", bad, rec.Code)
		}
	}
	rec := env.call(writerA, SyncRequest{Action: SyncActionMCPIssueToken, MCP: &SyncMCPRequest{Name: " Claude   Code ", ExpiresDays: 30}})
	expectSyncStatus(t, rec, http.StatusOK)
	issued := decodeSync[SyncMCPIssueTokenResponse](t, rec)
	if !strings.HasPrefix(issued.Token, mcp.TokenPrefix) || issued.Name != "Claude Code" || issued.ExpiresAt == nil ||
		issued.ExpiresAt.Sub(time.Now()) < 29*24*time.Hour {
		t.Fatalf("mcp_issue_token = %+v", issued)
	}
	// 公開サーバーにはハッシュだけを保存する。
	if n := env.countRows(`SELECT COUNT(*) FROM mcp_tokens WHERE token_hash = ?`, mcp.HashToken(issued.Token)); n != 1 {
		t.Errorf("stored token hash rows = %d", n)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM mcp_tokens WHERE token_hash = ?`, issued.Token); n != 0 {
		t.Error("raw token is stored")
	}
	// 発行したトークンで、秘匿パスの MCP サーバーを使える。
	if r, out := env.mcpCallAt(secret, issued.Token, "ping", map[string]any{}); r.StatusCode != http.StatusOK || out["error"] != nil {
		t.Errorf("ping with the issued token: status %d, %v", r.StatusCode, out)
	}

	// ほかのライターには見えず、失効させられない。
	if info := decodeSync[SyncMCPInfoResponse](t, env.call(writerB, SyncRequest{Action: SyncActionMCPInfo})); len(info.Tokens) != 0 {
		t.Errorf("writer B sees tokens: %+v", info.Tokens)
	}
	if rec := env.call(writerB, SyncRequest{Action: SyncActionMCPRevokeToken, MCP: &SyncMCPRequest{ID: issued.ID}}); rec.Code != http.StatusNotFound {
		t.Errorf("writer B revoked writer A's token: status %d", rec.Code)
	}
	// admin の招待は全員の分を見られる。
	all := decodeSync[SyncMCPInfoResponse](t, env.call(owner, SyncRequest{Action: SyncActionMCPInfo}))
	if !all.ShowAll || len(all.Tokens) != 1 || all.Tokens[0].ID != issued.ID || all.Tokens[0].OwnerName != "ライターA" || all.Tokens[0].Mine {
		t.Errorf("admin mcp_info tokens = %+v", all.Tokens)
	}
	if mine := decodeSync[SyncMCPInfoResponse](t, env.call(writerA, SyncRequest{Action: SyncActionMCPInfo})); len(mine.Tokens) != 1 || !mine.Tokens[0].Mine {
		t.Errorf("writer A mcp_info tokens = %+v", mine.Tokens)
	}

	expectSyncStatus(t, env.call(writerA, SyncRequest{Action: SyncActionMCPRevokeToken, MCP: &SyncMCPRequest{ID: issued.ID}}), http.StatusOK)
	if r, _ := env.mcpCallAt(secret, issued.Token, "ping", map[string]any{}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("ping after revoke: status %d, want 401", r.StatusCode)
	}

	// OAuth 2.1 で許可した連携の一覧と取り消し。
	access, _ := issueOAuthAccessToken(t, env.server.oauth, "sync-tok-"+env.claimsOf(writerA).ID)
	mine := decodeSync[SyncMCPInfoResponse](t, env.call(writerA, SyncRequest{Action: SyncActionMCPInfo}))
	if len(mine.OAuthApps) != 1 || mine.OAuthApps[0].ClientName != "Claude" || !mine.OAuthApps[0].Mine || mine.OAuthApps[0].OwnerName != "ライターA" {
		t.Fatalf("oauth apps = %+v", mine.OAuthApps)
	}
	appID := mine.OAuthApps[0].ID
	if rec := env.call(writerB, SyncRequest{Action: SyncActionMCPRevokeOAuthApp, MCP: &SyncMCPRequest{ID: appID}}); rec.Code != http.StatusNotFound {
		t.Errorf("writer B revoked writer A's app: status %d", rec.Code)
	}
	expectSyncStatus(t, env.call(owner, SyncRequest{Action: SyncActionMCPRevokeOAuthApp, MCP: &SyncMCPRequest{ID: appID}}), http.StatusOK)
	if r, _ := env.mcpCallAt(secret, access, "ping", map[string]any{}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("ping after the app was revoked: status %d, want 401", r.StatusCode)
	}
	if rec := env.call(owner, SyncRequest{Action: SyncActionMCPRevokeOAuthApp, MCP: &SyncMCPRequest{ID: appID}}); rec.Code != http.StatusNotFound {
		t.Errorf("revoke twice: status %d, want 404", rec.Code)
	}
}

// PC の sorus の「MCP連携」から、接続中の公開サーバーの MCP の URL・API トークン・確認コードを扱える（ローカルの管理者だけ）。
func TestRemoteMCPFromPC(t *testing.T) {
	remote := newPublicMCPEnv(t)
	env := &remoteEnv{local: newTestEnv(t), remote: remote}
	adm := env.local.loggedIn(admin)

	// 公開サーバーが未設定なら、欄を出さず（nil）、API は 409。
	if d := env.local.server.remoteMCPData(context.Background(), i18n.JA); d != nil {
		t.Errorf("unconfigured remote data = %+v", d)
	}
	expectStatus(t, adm.getJSON(MCPRemoteSettingsPath, nil), http.StatusConflict)

	env.configure("山田のPC", auth.RoleWriter)
	wantEndpoint := remote.srv.URL + remote.server.cfg.MCPAPIPath
	var data mcpRemoteData
	expectStatus(t, adm.getJSON(MCPRemoteSettingsPath, &data), http.StatusOK)
	if !data.Connected || data.Error != "" || data.Endpoint != wantEndpoint || data.Role != auth.RoleWriter || data.Subject != "山田のPC" ||
		data.ShowOwner || !data.OAuth || data.ServerURL != remote.srv.URL {
		t.Errorf("remote data = %+v", data)
	}

	// API トークンを発行すると、トークンと公開サーバーの MCP の URL を1回だけ返す。
	var created struct {
		ID, Name, Token, Endpoint string
	}
	expectStatus(t, adm.postJSON(MCPRemoteSettingsPath+"/tokens", url.Values{"name": {"Claude Code"}, "expires": {"90"}}, &created), http.StatusCreated)
	if !strings.HasPrefix(created.Token, mcp.TokenPrefix) || created.Endpoint != wantEndpoint || created.Name != "Claude Code" {
		t.Fatalf("created = %+v", created)
	}
	if r, _ := remote.mcpCallAt(remote.server.cfg.MCPAPIPath, created.Token, "ping", map[string]any{}); r.StatusCode != http.StatusOK {
		t.Errorf("ping on the public server with the token from the PC: status %d", r.StatusCode)
	}
	expectStatus(t, adm.postJSON(MCPRemoteSettingsPath+"/tokens", url.Values{"name": {""}, "expires": {"90"}}, nil), http.StatusUnprocessableEntity)
	expectStatus(t, adm.postJSON(MCPRemoteSettingsPath+"/tokens", url.Values{"name": {"x"}, "expires": {"7"}}, nil), http.StatusUnprocessableEntity)

	// 画面の初期表示のデータ（mcpTokensData.Remote）にも、発行したトークンが載る。
	d := env.local.server.remoteMCPData(context.Background(), i18n.JA)
	if d == nil || len(d.Tokens) != 1 || d.Tokens[0].ID != created.ID || d.Tokens[0].Name != "Claude Code" {
		t.Fatalf("remote data tokens = %+v", d)
	}
	mcpPage := adm.get(MCPSettingsPath)
	expectStatus(t, mcpPage, http.StatusOK)
	expectContains(t, mcpPage.Body, `id="remote-mcp"`, wantEndpoint, `id="remote-mcp-login-code-btn"`, "Claude Code", `data-remote-token-revoke="`+created.ID+`"`)
	// 2つの接続URLを対比して出し、高度な設定の例はローカル・公開サーバーの URL を切り替えられる（§1-4）。
	expectContains(t, mcpPage.Body, "この PC 専用の接続URL（ローカル）", `id="mcp-endpoint-anywhere"`, "どこからでも使える接続URL（公開サーバー）",
		`id="mcp-endpoint-remote" type="text" readonly value="`+wantEndpoint+`"`,
		`data-local-url="`+env.local.srv.URL+`/api/mcp"`, `data-remote-url="`+wantEndpoint+`"`, `name="mcp-snippet-target" value="remote"`)
	expectNotContains(t, mcpPage.Body, `id="mcp-snippet-domain-note"`, "Skywork")

	var revoked map[string]any
	expectStatus(t, adm.postJSON(MCPRemoteSettingsPath+"/tokens/"+created.ID+"/revoke", nil, &revoked), http.StatusOK)
	if r, _ := remote.mcpCallAt(remote.server.cfg.MCPAPIPath, created.Token, "ping", map[string]any{}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("ping after revoke from the PC: status %d, want 401", r.StatusCode)
	}
	var fail struct{ Message string }
	expectStatus(t, adm.postJSON(MCPRemoteSettingsPath+"/tokens/"+created.ID+"/revoke", nil, &fail), http.StatusBadGateway)
	if !strings.Contains(fail.Message, "見つかりません") {
		t.Errorf("revoke twice message = %q", fail.Message)
	}

	// 公開サーバーの同意画面で入れる確認コード。
	var lc struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	expectStatus(t, adm.postJSON(MCPRemoteSettingsPath+"/login-code", nil, &lc), http.StatusOK)
	if !regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`).MatchString(lc.Code) || lc.ExpiresIn <= 0 || lc.ExpiresIn > int(oauthLoginCodeTTL/time.Second) {
		t.Errorf("login code = %+v", lc)
	}

	// 許可した連携の取り消し。
	access, _ := issueOAuthAccessToken(t, remote.server.oauth, "sync-tok-"+remote.claimsOf(env.local.server.loadRemoteConfig().Token).ID)
	d = env.local.server.remoteMCPData(context.Background(), i18n.JA)
	if d == nil || len(d.OAuthApps) != 1 {
		t.Fatalf("remote oauth apps = %+v", d)
	}
	expectStatus(t, adm.postJSON(MCPRemoteSettingsPath+"/oauth/"+d.OAuthApps[0].ID+"/revoke", nil, nil), http.StatusOK)
	if r, _ := remote.mcpCallAt(remote.server.cfg.MCPAPIPath, access, "ping", map[string]any{}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("ping after the app was revoked from the PC: status %d, want 401", r.StatusCode)
	}

	// 接続先とトークンはサイトで1つのため、ローカルの管理者でなければ扱えない（ライターが admin の招待の権限を得ない）。
	writer := env.local.loggedIn(studentA)
	expectStatus(t, writer.getJSON(MCPRemoteSettingsPath, nil), http.StatusForbidden)
	expectStatus(t, writer.postJSON(MCPRemoteSettingsPath+"/tokens", url.Values{"name": {"x"}, "expires": {"90"}}, nil), http.StatusForbidden)
	expectStatus(t, writer.postJSON(MCPRemoteSettingsPath+"/login-code", nil, nil), http.StatusForbidden)
	writerPage := writer.get(MCPSettingsPath)
	expectStatus(t, writerPage, http.StatusOK)
	expectNotContains(t, writerPage.Body, `id="remote-mcp"`, wantEndpoint)
}

func TestRemoteMCPFailureMessage(t *testing.T) {
	old := &remoteError{Status: http.StatusBadRequest, Message: `未対応の action です: "mcp_info"`}
	if got := remoteMCPFailureMessage(old, i18n.JA); !strings.Contains(got, "公開サーバーの sorus を更新") {
		t.Errorf("old server message = %q", got)
	}
	if got := remoteMCPFailureMessage(&remoteError{Status: 0, Message: "接続できませんでした"}, i18n.JA); got != "接続できませんでした" {
		t.Errorf("connect failure message = %q", got)
	}
	// 英語の管理画面では、この PC で作る説明を英語にし、公開サーバーが書いた理由はそのまま添える。
	if got := remoteMCPFailureMessage(old, i18n.EN); !strings.Contains(got, "update sorus on the public server") {
		t.Errorf("old server message (en) = %q", got)
	}
	if got := remoteMCPFailureMessage(&remoteError{Status: 0, Message: "接続できませんでした"}, i18n.EN); !strings.HasPrefix(got, "Could not connect to the public server") {
		t.Errorf("connect failure message (en) = %q", got)
	}
	if got := remoteMCPFailureMessage(&remoteError{Status: http.StatusUnauthorized, Message: "トークンが…"}, i18n.EN); !strings.Contains(got, "did not accept the token") {
		t.Errorf("401 message (en) = %q", got)
	}
	if got := remoteMCPFailureMessage(&remoteError{Status: http.StatusNotFound, Message: "見つかりません", ServerMessage: true}, i18n.EN); got != "The public server returned an error: 見つかりません" {
		t.Errorf("server message (en) = %q", got)
	}
	if got := remoteMCPFailureMessage(errors.New("x"), i18n.EN); !strings.Contains(got, "MCP integration information") {
		t.Errorf("non-remote error (en) = %q", got)
	}
	if got := remoteMCPEndpoint(remoteConfig{URL: "https://example.com"}, &SyncMCPInfoResponse{Path: "/api/mcp-abc", URL: "http://10.0.0.2/api/mcp-abc"}); got != "https://example.com/api/mcp-abc" {
		t.Errorf("endpoint = %q", got)
	}
	if got := remoteMCPEndpoint(remoteConfig{URL: "https://example.com"}, &SyncMCPInfoResponse{Path: "../x", URL: "https://example.com/api/mcp-x"}); got != "https://example.com/api/mcp-x" {
		t.Errorf("endpoint with a bad path = %q", got)
	}
}

// 閲覧制限を有効にした公開専用モードのサーバーでも、AI アプリのサーバーは閲覧用のパスワード無しで秘匿パスの MCP サーバーと
// OAuth 2.1 のエンドポイントを使える。同意画面は本人を確かめる前にサイト名を出さない。
func TestPublicOnlyMCPBehindSiteAccess(t *testing.T) {
	const siteName = "準備中のメディア"
	remote := newPublicOnlyTLSEnv(t, func(db *sqlx.DB) { setSettingForTest(t, db, "site_name", siteName) })
	enableSiteAccess(t, remote, testBasicAuthUser, testBasicAuthPass)
	secret := remote.server.cfg.MCPAPIPath
	anon := remote.newClient()

	res := anon.rawRequest(http.MethodPost, secret, nil)
	expectStatus(t, res, http.StatusUnauthorized)
	expectNotBasicAuthChallenge(t, res, "POST "+secret)
	expectNotBasicAuthChallenge(t, anon.rawRequest(http.MethodGet, mcp.ProtectedResourceMetadataPath+secret, nil), "secret metadata")
	expectBasicAuthChallenge(t, anon.rawRequest(http.MethodGet, mcp.ProtectedResourceMetadataPath, map[string]string{"Accept": "text/html"}))
	expectBasicAuthChallenge(t, anon.rawRequest(http.MethodPost, mcp.DefaultBasePath, map[string]string{"Accept": "text/html"}))

	req, _ := http.NewRequest(http.MethodPost, remote.srv.URL+OAuthRegisterPath,
		strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+oauthTestRedirect+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	reg := anon.do(req)
	expectStatus(t, reg, http.StatusCreated)
	var client struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(reg.Body), &client); err != nil {
		t.Fatal(err)
	}
	page := anon.get(authorizeQuery(client.ClientID, nil))
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="oauth-login"`)
	expectNotContains(t, page.Body, siteName)
}

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	l := newRateLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if _, ok := l.hit("a"); !ok {
			t.Fatalf("hit %d refused", i+1)
		}
	}
	if retry, ok := l.hit("a"); ok || retry != time.Minute {
		t.Errorf("third hit = %v, %v; want refused with 1m", retry, ok)
	}
	if _, ok := l.hit("b"); !ok {
		t.Error("another key was refused")
	}
	if _, blocked := l.blocked("a"); !blocked {
		t.Error("a is not blocked")
	}
	now = now.Add(30 * time.Second)
	if retry, blocked := l.blocked("a"); !blocked || retry != 30*time.Second {
		t.Errorf("blocked after 30s = %v, %v", retry, blocked)
	}
	now = now.Add(31 * time.Second)
	if _, ok := l.hit("a"); !ok {
		t.Error("hit after the window was refused")
	}
	if _, blocked := l.blocked("c"); blocked {
		t.Error("unused key is blocked")
	}
}

// OAuth 2.1 のクライアント登録・トークン・同意画面は回数を制限し、超えたら 429（Retry-After 付き）を返す。
func TestOAuthEndpointsRateLimited(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauthLimits.register.setLimit(2)
	env.registerOAuthClient("A", oauthTestRedirect)
	clientID := env.registerOAuthClient("B", oauthTestRedirect)
	res, out := env.oauthJSON(http.MethodPost, OAuthRegisterPath, "application/json", `{"redirect_uris":["`+oauthTestRedirect+`"]}`)
	if res.StatusCode != http.StatusTooManyRequests || out["error"] != "temporarily_unavailable" || res.Header.Get("Retry-After") == "" {
		t.Errorf("third register: status %d, %v, Retry-After %q", res.StatusCode, out, res.Header.Get("Retry-After"))
	}

	env.server.oauthLimits.token.setLimit(1)
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "client_id": {clientID}}
	if r, _ := env.tokenRequest(form); r.StatusCode != http.StatusBadRequest {
		t.Errorf("first token request: status %d, want 400", r.StatusCode)
	}
	if r, out := env.tokenRequest(form); r.StatusCode != http.StatusTooManyRequests || out["error"] != "temporarily_unavailable" {
		t.Errorf("second token request: status %d, %v", r.StatusCode, out)
	}

	env.server.oauthLimits.authorize.setLimit(1)
	browser := env.newClient()
	browser.get(authorizeQuery(clientID, nil))
	page := browser.get(authorizeQuery(clientID, nil))
	expectStatus(t, page, http.StatusTooManyRequests)
	expectContains(t, page.Body, `id="oauth-error"`)
}

// 確認コードの誤りが上限に達したら、区切りが変わるまで正しいコードでも受け付けない（総当たりの防止）。
func TestPublicOnlyOAuthLoginCodeBruteForce(t *testing.T) {
	env := newPublicMCPEnv(t)
	env.server.oauthLimits.loginFail.setLimit(2)
	clientID := env.registerOAuthClient("Claude", oauthTestRedirect)
	browser := env.newClient()
	form := consentForm(t, browser.get(authorizeQuery(clientID, nil)).Body)
	for i := 0; i < 2; i++ {
		f := cloneValues(form)
		f.Set("login_code", "ZZZZ-ZZZZ")
		expectStatus(t, browser.postRaw(OAuthAuthorizePath, f), http.StatusUnprocessableEntity)
	}
	lc := decodeSync[SyncMCPLoginCodeResponse](t, env.call(env.token("w", auth.RoleWriter), SyncRequest{Action: SyncActionMCPIssueLoginCode}))
	f := cloneValues(form)
	f.Set("login_code", lc.Code)
	res := browser.postRaw(OAuthAuthorizePath, f)
	expectStatus(t, res, http.StatusTooManyRequests)
	if res.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// 止めているあいだに試したコードは使用済みにしない（区切りが変われば使える）。
	if _, ok := env.server.oauthLogins.consumeCode(lc.Code); !ok {
		t.Error("the code was consumed while login was blocked")
	}
}

func TestOAuthLoginStore(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	l := newOAuthLoginStore()
	l.now = func() time.Time { return now }
	code1, exp, err := l.issueCode("u1")
	if err != nil || !exp.Equal(now.Add(oauthLoginCodeTTL)) {
		t.Fatalf("issueCode = %q, %v, %v", code1, exp, err)
	}
	// 同じユーザーで発行し直すと、前のコードは使えない。
	code2, _, _ := l.issueCode("u1")
	if _, ok := l.consumeCode(code1); ok && code1 != code2 {
		t.Error("the previous code is still usable")
	}
	if u, ok := l.consumeCode(" " + strings.ToLower(code2) + " "); !ok || u != "u1" {
		t.Errorf("consumeCode = %q, %v", u, ok)
	}
	code3, _, _ := l.issueCode("u2")
	now = now.Add(oauthLoginCodeTTL)
	if _, ok := l.consumeCode(code3); ok {
		t.Error("an expired code was accepted")
	}
	for _, bad := range []string{"", "ABC", strings.Repeat("A", 100)} {
		if _, ok := l.consumeCode(bad); ok {
			t.Errorf("consumeCode(%q) accepted", bad)
		}
	}

	req := oauthAuthorizeRequest{ClientID: "c1", RedirectURI: oauthTestRedirect, CodeChallenge: oauthTestChallenge}
	ticket, err := l.issueTicket("u1", req)
	if err != nil {
		t.Fatal(err)
	}
	other := req
	other.ClientID = "c2"
	if _, ok := l.consumeTicket(ticket, other); ok {
		t.Error("a ticket was used for another client")
	}
	// 一致しなかったチケットも使用済みになる。
	if _, ok := l.consumeTicket(ticket, req); ok {
		t.Error("a ticket was reused after a mismatch")
	}
	ticket, _ = l.issueTicket("u1", req)
	if u, ok := l.consumeTicket(ticket, req); !ok || u != "u1" {
		t.Errorf("consumeTicket = %q, %v", u, ok)
	}
}

// パスワードを変えたら、その人が OAuth 2.1 で許可した AI アプリの連携も取り消す（Task 1 の残件）。
func TestPasswordChangeRevokesOAuthTokens(t *testing.T) {
	env := newTestEnv(t)
	access, _ := issueOAuthAccessToken(t, env.server.oauth, studentA.ID)
	other, _ := issueOAuthAccessToken(t, env.server.oauth, studentB.ID)
	if r, _ := env.mcpCall(access, "ping", map[string]any{}); r.StatusCode != http.StatusOK {
		t.Fatalf("ping before the password change: status %d", r.StatusCode)
	}
	res := env.loggedIn(admin).postForm("/admin/users/"+studentA.ID+"/password", url.Values{"password": {"new-password-123"}})
	expectRedirect(t, res, "/admin/users?flash=user_password_updated")
	if r, _ := env.mcpCall(access, "ping", map[string]any{}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("ping after the password change: status %d, want 401", r.StatusCode)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM oauth_tokens WHERE user_id = ? AND revoked_at IS NULL`, studentA.ID); n != 0 {
		t.Errorf("live oauth tokens after the password change = %d", n)
	}
	// ほかの人の連携はそのまま。
	if r, _ := env.mcpCall(other, "ping", map[string]any{}); r.StatusCode != http.StatusOK {
		t.Errorf("another user's oauth token: status %d, want 200", r.StatusCode)
	}
}

// 多言語対応の ON/OFF も公開サーバーへ反映し、公開サーバーの英語ページ（/en 以下）を PC の設定に揃える（Task 1 の残件）。
func TestRemoteSettingsPushSendsMultilingual(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("root", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	var out struct {
		Settings SyncSettingsValues `json:"settings"`
	}
	enableMultilingual(t, env.local.db)
	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if got := env.remote.setting(settingMultilingualEnabled); got != "1" || out.Settings.MultilingualEnabled == nil || !*out.Settings.MultilingualEnabled {
		t.Errorf("after ON: remote setting %q, response %v", got, out.Settings.MultilingualEnabled)
	}
	if rec := env.remote.get("/en"); rec.Code != http.StatusOK {
		t.Errorf("remote /en after ON: status %d, want 200", rec.Code)
	}

	setSettingForTest(t, env.local.db, settingMultilingualEnabled, "0")
	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if got := env.remote.setting(settingMultilingualEnabled); got != "0" || out.Settings.MultilingualEnabled == nil || *out.Settings.MultilingualEnabled {
		t.Errorf("after OFF: remote setting %q, response %v", got, out.Settings.MultilingualEnabled)
	}
	if rec := env.remote.get("/en"); rec.Code != http.StatusNotFound {
		t.Errorf("remote /en after OFF: status %d, want 404", rec.Code)
	}
}

// sync_settings の multilingual_enabled だけを送っても反映でき、省略したら変えない。
func TestSyncSettingsMultilingual(t *testing.T) {
	env := newSyncEnv(t)
	owner := env.token("owner", auth.RoleAdmin)
	rec := env.call(owner, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{MultilingualEnabled: boolPtr(true)}})
	expectSyncStatus(t, rec, http.StatusOK)
	if got := env.setting(settingMultilingualEnabled); got != "1" {
		t.Errorf("multilingual_enabled = %q, want 1", got)
	}
	rec = env.call(owner, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{SiteName: strPtr("名前だけ")}})
	expectSyncStatus(t, rec, http.StatusOK)
	if got := env.setting(settingMultilingualEnabled); got != "1" {
		t.Errorf("multilingual_enabled after a push without it = %q, want 1", got)
	}
	if res := decodeSync[SyncSettingsResponse](t, rec); res.Settings.MultilingualEnabled == nil || !*res.Settings.MultilingualEnabled {
		t.Errorf("response multilingual = %v", res.Settings.MultilingualEnabled)
	}
	// 多言語対応の変更は管理者の招待だけ（sync_settings と同じ）。
	if rec := env.call(env.token("ed", auth.RoleEditor), SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{MultilingualEnabled: boolPtr(false)}}); rec.Code != http.StatusForbidden {
		t.Errorf("editor sync_settings: status %d, want 403", rec.Code)
	}
}

// MCP の API トークンの誤りの案内は表示言語（英語）でも出す。
func TestMCPTokenErrorsLocalized(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	writer.get("/admin/set-lang?lang=en")
	res := writer.postForm(MCPSettingsPath, url.Values{"name": {""}, "expires": {"90"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "Enter a name for the token")
	expectNotContains(t, res.Body, "トークンの名前（使うツールや端末など）を入力してください")
	res = writer.postForm(MCPSettingsPath, url.Values{"name": {"x"}, "expires": {"7"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "Choose when the token expires.")
}

// PC の「MCP連携」の公開サーバーの欄の API は、管理画面を英語で表示しているときはエラーも英語で返す
// （admin_ux_and_desktop_polish_spec.md §1-4。Phase 2 の残課題）。日本語の表示では今までどおり日本語。
func TestRemoteMCPErrorsFollowDisplayLanguage(t *testing.T) {
	remote := newPublicMCPEnv(t)
	env := &remoteEnv{local: newTestEnv(t), remote: remote}
	ja := env.local.loggedIn(admin)
	en := env.local.loggedIn(admin)
	en.useEnglish()
	message := func(res *response) string {
		t.Helper()
		var body struct{ Message string }
		if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
			t.Fatalf("decode error body: %v\n%s", err, res.Body)
		}
		return body.Message
	}

	// 公開サーバーが未設定（409）。
	res := en.getJSON(MCPRemoteSettingsPath, nil)
	expectStatus(t, res, http.StatusConflict)
	if got := message(res); !strings.HasPrefix(got, "No public server is set up") || !strings.Contains(got, "Settings → Publishing → Connect to a publish server") {
		t.Errorf("unconfigured (en) = %q", got)
	}
	res = ja.getJSON(MCPRemoteSettingsPath, nil)
	expectStatus(t, res, http.StatusConflict)
	if got := message(res); !strings.Contains(got, "公開サーバーが未設定です") {
		t.Errorf("unconfigured (ja) = %q", got)
	}

	env.configure("山田のPC", auth.RoleWriter)
	cases := []struct {
		form   url.Values
		ja, en string
	}{
		{url.Values{"name": {"  "}, "expires": {"90"}}, "トークンの名前", "Enter a name for the token"},
		{url.Values{"name": {strings.Repeat("あ", maxMCPTokenNameRunes+1)}, "expires": {"90"}}, "文字以内", "characters or fewer"},
		{url.Values{"name": {"a\x01b"}, "expires": {"90"}}, "制御文字", "control characters"},
		{url.Values{"name": {"Claude Code"}, "expires": {"7"}}, "有効期限を選択", "Choose when the token expires"},
	}
	for _, tc := range cases {
		res := en.postJSON(MCPRemoteSettingsPath+"/tokens", tc.form, nil)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		if got := message(res); !strings.Contains(got, tc.en) {
			t.Errorf("token create %v (en) = %q, want %q", tc.form, got, tc.en)
		}
		res = ja.postJSON(MCPRemoteSettingsPath+"/tokens", tc.form, nil)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		if got := message(res); !strings.Contains(got, tc.ja) {
			t.Errorf("token create %v (ja) = %q, want %q", tc.form, got, tc.ja)
		}
	}

	// 公開サーバーが断った理由（公開サーバーが書いた日本語）は、英語の説明に添えて返す。
	res = en.postJSON(MCPRemoteSettingsPath+"/tokens/no-such-token/revoke", nil, nil)
	expectStatus(t, res, http.StatusBadGateway)
	if got := message(res); !strings.HasPrefix(got, "The public server returned an error: ") {
		t.Errorf("revoke unknown (en) = %q", got)
	}

	// 公開サーバーに接続できないときの画面の欄（Remote.Error）も表示言語に従う。
	remote.srv.Close()
	if d := env.local.server.remoteMCPData(context.Background(), i18n.EN); d == nil || !strings.HasPrefix(d.Error, "Could not connect to the public server") {
		t.Errorf("unreachable remote data (en) = %+v", d)
	}
	if d := env.local.server.remoteMCPData(context.Background(), i18n.JA); d == nil || !strings.Contains(d.Error, "公開サーバーに接続できませんでした") {
		t.Errorf("unreachable remote data (ja) = %+v", d)
	}
}

// 公開サーバーが無い（静的な公開先・未接続）ときは、公開サーバーの接続URLと設定例の切り替えを出さず、PC 版の
// 127.0.0.1 の URL に「この PC 専用」の説明と、独自ドメインへの読み替えを添える（§1-4）。英語の表示も同じ構成。
func TestMCPPageWithoutPublicServer(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	res := adm.get(MCPSettingsPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "この PC 専用の接続URL（ローカル）", `id="mcp-snippet-domain-note"`, "https://あなたのドメイン/api/mcp",
		`<span data-mcp-url>`+env.srv.URL+`/api/mcp</span>`, "ChatGPT / Claude / Gemini / Perplexity")
	expectNotContains(t, res.Body, `id="remote-mcp"`, `id="mcp-endpoint-anywhere"`, "data-mcp-url-switch", "Skywork")

	en := env.loggedIn(admin)
	en.useEnglish()
	res = en.get(MCPSettingsPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "URL for this PC only (local)", "https://your-domain/api/mcp")
	expectNotContains(t, res.Body, "この PC 専用の接続URL（ローカル）", "data-mcp-url-switch")
}
