package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"sorus/internal/mcp"
)

var newTokenPattern = regexp.MustCompile(`id="new-token-value" type="text" readonly value="(` + mcp.TokenPrefix + `[0-9a-f]+)"`)

// issueMCPToken は c で API トークンを発行し、画面に一度だけ表示された生のトークンを返す。
func issueMCPToken(c *client, name, expires string) string {
	c.t.Helper()
	res := c.postForm("/admin/settings/mcp", url.Values{"name": {name}, "expires": {expires}})
	expectStatus(c.t, res, http.StatusCreated)
	m := newTokenPattern.FindStringSubmatch(res.Body)
	if m == nil {
		c.t.Fatalf("issued token is not shown:\n%.3000s", res.Body)
	}
	return m[1]
}

// mcpCall は API トークンで MCP サーバー（/api/mcp）に JSON-RPC のリクエストを送り、レスポンスを返す。
func (e *testEnv) mcpCall(token, method string, params any) (*http.Response, map[string]any) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+mcp.DefaultBasePath, strings.NewReader(string(body)))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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

func TestMCPTokenLifecycle(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, nil)
	writer := env.loggedIn(studentA)

	res := writer.get("/admin/settings/mcp")
	expectStatus(t, res, http.StatusOK)
	// 接続先は公開サイトの URL（BaseURL）ではなく、管理画面を開いているこの sorus（mcpEndpointURL）。
	expectContains(t, res.Body, "まだAPIトークンがありません", env.srv.URL+"/api/mcp", "claude mcp add --transport http sorus",
		"~/.copilot/mcp-config.json", "&lt;APIトークン&gt;", "ライター")
	expectNotContains(t, res.Body, testBaseURL+"/api/mcp")

	res = writer.postForm("/admin/settings/mcp", url.Values{"name": {"研究室PCの Claude Code"}, "expires": {"30"}})
	expectStatus(t, res, http.StatusCreated)
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	m := newTokenPattern.FindStringSubmatch(res.Body)
	if m == nil {
		t.Fatalf("token not shown:\n%.3000s", res.Body)
	}
	raw := m[1]
	// 接続設定例には発行したトークンがそのまま入る。
	expectContains(t, res.Body, `Authorization: Bearer `+raw, "研究室PCの Claude Code")

	// DB には SHA-256 ハッシュだけを保存し、生のトークンは保存しない。
	var row struct {
		ID        string     `db:"id"`
		Hash      string     `db:"token_hash"`
		ExpiresAt *time.Time `db:"expires_at"`
	}
	if err := env.db.Get(&row, `SELECT id, token_hash, expires_at FROM mcp_tokens WHERE user_id = ?`, studentA.ID); err != nil {
		t.Fatal(err)
	}
	if row.Hash != mcp.HashToken(raw) || strings.Contains(row.Hash, raw) {
		t.Errorf("token_hash = %q", row.Hash)
	}
	if row.ExpiresAt == nil || row.ExpiresAt.Sub(time.Now().AddDate(0, 0, 30)).Abs() > time.Minute {
		t.Errorf("expires_at = %v, want about 30 days later", row.ExpiresAt)
	}
	if env.countRows(`SELECT COUNT(*) FROM mcp_tokens WHERE token_hash = ?`, raw) != 0 {
		t.Error("raw token was stored")
	}

	// 一覧を開き直しても、トークンは二度と表示されない。
	res = writer.get("/admin/settings/mcp")
	expectContains(t, res.Body, "研究室PCの Claude Code", "未使用", "/admin/settings/mcp/"+row.ID+"/revoke")
	if strings.Contains(res.Body, raw) {
		t.Error("token is shown again on the list page")
	}

	// 発行したトークンで、CMS に組み込まれた MCP サーバーを使える。
	httpRes, out := env.mcpCall(raw, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "copilot-cli", "version": "1.0"},
	})
	if httpRes.StatusCode != http.StatusOK || out["result"] == nil {
		t.Fatalf("initialize: status %d, %v", httpRes.StatusCode, out)
	}
	_, out = env.mcpCall(raw, "tools/list", map[string]any{})
	if s := fmt.Sprint(out["result"]); !strings.Contains(s, "cms_list_articles") || !strings.Contains(s, "cms_save_draft") {
		t.Errorf("tools/list = %v", out)
	}
	_, out = env.mcpCall(raw, "tools/call", map[string]any{"name": "cms_save_draft", "arguments": map[string]any{
		"title": "MCPから書いた下書き", "content_markdown": "AIエージェントが保存した本文",
	}})
	if s := fmt.Sprint(out["result"]); strings.Contains(s, "isError:true") || out["error"] != nil {
		t.Fatalf("cms_save_draft = %v", out)
	}
	if env.countRows(`SELECT COUNT(*) FROM articles WHERE title = 'MCPから書いた下書き' AND author_id = ?`, studentA.ID) != 1 {
		t.Error("draft saved via MCP was not stored as the token owner's article")
	}
	if res := writer.get("/admin/settings/mcp"); strings.Contains(res.Body, "未使用") {
		t.Error("last_used_at was not shown after using the token")
	}

	// トークンが無い・違うときは 401。
	if res, _ := env.mcpCall("", "tools/list", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d", res.StatusCode)
	}
	if res, _ := env.mcpCall(mcp.TokenPrefix+"bogus", "tools/list", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus token: status %d", res.StatusCode)
	}

	// 失効させると、そのトークンでは接続できなくなる。
	expectRedirect(t, writer.postForm("/admin/settings/mcp/"+row.ID+"/revoke", nil), "/admin/settings/mcp?flash=token_revoked")
	if res, _ := env.mcpCall(raw, "tools/list", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: status %d", res.StatusCode)
	}
	expectStatus(t, writer.postForm("/admin/settings/mcp/"+row.ID+"/revoke", nil), http.StatusNotFound)
}

func TestMCPTokenValidationAndPermissions(t *testing.T) {
	env := newTestEnv(t)
	writerA := env.loggedIn(studentA)
	writerB := env.loggedIn(studentB)
	adm := env.loggedIn(admin)

	for _, form := range []url.Values{
		{"name": {"  "}, "expires": {"90"}},
		{"name": {strings.Repeat("長", 101)}, "expires": {"90"}},
		{"name": {"CLI"}, "expires": {"7"}},
	} {
		res := writerA.postForm("/admin/settings/mcp", form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		if newTokenPattern.MatchString(res.Body) {
			t.Error("token issued for invalid input")
		}
	}
	// CSRF トークンの無いフォーム送信は拒否する。
	expectStatus(t, writerA.postRaw("/admin/settings/mcp", url.Values{"name": {"CLI"}, "expires": {"90"}}), http.StatusForbidden)

	issueMCPToken(writerA, "Aのトークン", "never")
	issueMCPToken(writerB, "Bのトークン", "90")
	var tokenA string
	if err := env.db.Get(&tokenA, `SELECT id FROM mcp_tokens WHERE user_id = ?`, studentA.ID); err != nil {
		t.Fatal(err)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM mcp_tokens WHERE user_id = ? AND expires_at IS NULL`, studentA.ID); n != 1 {
		t.Errorf("never-expiring token has expires_at set")
	}

	// 執筆者は自分のトークンだけを見て、失効できる。管理者は全員分を発行者付きで見て、失効できる。
	res := writerB.get("/admin/settings/mcp")
	if strings.Contains(res.Body, "Aのトークン") || !strings.Contains(res.Body, "Bのトークン") || strings.Contains(res.Body, "発行者") {
		t.Errorf("writer B sees other users' tokens:\n%.2000s", res.Body)
	}
	expectStatus(t, writerB.postForm("/admin/settings/mcp/"+tokenA+"/revoke", nil), http.StatusNotFound)
	res = adm.get("/admin/settings/mcp")
	expectContains(t, res.Body, "Aのトークン", "Bのトークン", "発行者", "山田", "鈴木", "全員分")
	expectRedirect(t, adm.postForm("/admin/settings/mcp/"+tokenA+"/revoke", nil), "/admin/settings/mcp")
	if env.countRows(`SELECT COUNT(*) FROM mcp_tokens WHERE id = ?`, tokenA) != 0 {
		t.Error("admin could not revoke another user's token")
	}

	// 期限切れのトークンは一覧で「期限切れ」と表示する。
	if _, err := env.db.Exec(`UPDATE mcp_tokens SET expires_at = ? WHERE user_id = ?`, time.Now().UTC().Add(-time.Hour), studentB.ID); err != nil {
		t.Fatal(err)
	}
	expectContains(t, writerB.get("/admin/settings/mcp").Body, "期限切れ")

	// 1人あたりの上限を超える発行は拒否する。
	for i := 1; i < maxMCPTokensPerUser; i++ {
		issueMCPToken(writerB, fmt.Sprintf("token-%d", i), "30")
	}
	res = writerB.postForm("/admin/settings/mcp", url.Values{"name": {"over"}, "expires": {"30"}})
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, "20個まで")

	// 未ログインではトークン管理画面を開けない。
	expectRedirect(t, env.newClient().get("/admin/settings/mcp"), "/admin/login")
}
