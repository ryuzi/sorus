package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/database"
	"sorus/internal/lock"
	"sorus/internal/rag"
	"sorus/internal/search"
)

// mcpTokensSchema は Phase 8 のマイグレーション v4 と同じ mcp_tokens の定義。v4 を含まない DB でもテストできるよう、
// Migrate の後に IF NOT EXISTS で作る（v4 適用済みなら何もしない）。
const mcpTokensSchema = `
CREATE TABLE IF NOT EXISTS mcp_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    last_used_at DATETIME,
    expires_at   DATETIME,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);`

type testUser struct {
	ID, Username, DisplayName string
	Role                      auth.Role
}

var (
	writerA = testUser{"writer-a", "yamada", "山田", auth.RoleWriter}
	writerB = testUser{"writer-b", "suzuki", "鈴木", auth.RoleWriter}
	editor  = testUser{"editor-1", "sato", "佐藤デスク", auth.RoleEditor}
	admin   = testUser{"admin-1", "admin", "管理者", auth.RoleAdmin}
)

// カテゴリ。categoryDefault は ID が最小で、新規作成の既定のカテゴリになる。
const (
	categoryDefault  = int64(1)
	categoryResearch = int64(2)
)

type testEnv struct {
	t      *testing.T
	db     *sqlx.DB
	server *Server
	http   *httptest.Server
	// tokens はユーザー ID ごとの API トークン。
	tokens map[string]string

	// rag は withRAG で設定した RAG サービス。
	rag *rag.Service
	// seeded は seed した記事の数。記事ごとに更新日時をずらし、一覧の順序を決定的にする。
	seeded int

	mu      sync.Mutex
	changed []string
}

// newTestEnv はファイル DB・テストユーザー・カテゴリ・API トークンを用意して MCP サーバーを起動する。
// cfg の DB と OnArticleChanged は上書きする。opts はサーバーの起動前に cfg を整える（DB を使う設定のため）。
func newTestEnv(t *testing.T, cfg Config, opts ...func(*testEnv, *Config)) *testEnv {
	t.Helper()
	db, err := database.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.Exec(mcpTokensSchema); err != nil {
		t.Fatalf("create mcp_tokens: %v", err)
	}
	env := &testEnv{t: t, db: db, tokens: map[string]string{}}
	for _, u := range []testUser{writerA, writerB, editor, admin} {
		env.addUser(u)
	}
	mustExec(t, db, `INSERT INTO categories (id, name, slug, description) VALUES (?, '未分類', 'uncategorized', ''), (?, '研究', 'research', '研究室の紹介')`,
		categoryDefault, categoryResearch)

	cfg.DB = db
	for _, opt := range opts {
		opt(env, &cfg)
	}
	cfg.OnArticleChanged = func(id string) {
		env.mu.Lock()
		defer env.mu.Unlock()
		env.changed = append(env.changed, id)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	env.server, err = New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env.http = httptest.NewServer(env.server)
	t.Cleanup(env.http.Close)
	return env
}

// withRAG は fakeEmbedder を使う RAG サービスを設定する。
func withRAG(env *testEnv, cfg *Config) {
	env.rag = rag.NewService(env.db, fakeEmbedder{})
	cfg.RAG = env.rag
}

// addUser はユーザーを登録し、API トークンを発行する。
func (e *testEnv) addUser(u testUser) {
	e.t.Helper()
	mustExec(e.t, e.db, `INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, 'unused', ?)`,
		u.ID, u.Username, u.DisplayName, u.Role)
	raw, _, err := IssueToken(context.Background(), e.db, u.ID, "test", nil)
	if err != nil {
		e.t.Fatalf("IssueToken: %v", err)
	}
	e.tokens[u.ID] = raw
}

func mustExec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// seed は記事を保存して ID を返す。Status が published なら公開日時を1時間前にする。
// 更新日時は seed した順に1秒ずつ新しくする（いずれも現在より前）。
func (e *testEnv) seed(a article.Article) string {
	e.t.Helper()
	if a.CategoryID == 0 {
		a.CategoryID = categoryDefault
	}
	if a.Status == article.StatusPublished && a.PublishedAt == nil {
		at := time.Now().Add(-time.Hour).UTC()
		a.PublishedAt = &at
	}
	if err := article.NewService(e.db).Save(&a); err != nil {
		e.t.Fatalf("seed %q: %v", a.Title, err)
	}
	e.seeded++
	updated := time.Now().Add(-time.Hour).Add(time.Duration(e.seeded) * time.Second).UTC()
	mustExec(e.t, e.db, `UPDATE articles SET updated_at = ? WHERE id = ?`, updated, a.ID)
	return a.ID
}

func (e *testEnv) article(id string) *article.Article {
	e.t.Helper()
	a, err := article.NewService(e.db).GetByID(id)
	if err != nil {
		e.t.Fatalf("GetByID(%s): %v", id, err)
	}
	return a
}

func (e *testEnv) changedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.changed)
}

// httpResult は HTTP の応答。
type httpResult struct {
	Status int
	Header http.Header
	Body   string
}

// post は MCP エンドポイントに body を POST する。headers の値が空ならそのヘッダーを付けない。
func (e *testEnv) post(userID, body string, headers map[string]string) httpResult {
	e.t.Helper()
	return e.do(http.MethodPost, DefaultBasePath, userID, body, headers)
}

func (e *testEnv) do(method, path, userID, body string, headers map[string]string) httpResult {
	e.t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if userID != "" {
		req.Header.Set("Authorization", "Bearer "+e.tokens[userID])
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return httpResult{Status: res.StatusCode, Header: res.Header, Body: string(b)}
}

// rpcResponse は JSON-RPC のレスポンスを読んだもの。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// checkEnvelope は raw が JSON-RPC 2.0 のレスポンスの形（jsonrpc が "2.0"、id があり、result と error の
// どちらか一方だけを持つ）であることを確かめて返す。
func checkEnvelope(t *testing.T, raw []byte) rpcResponse {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("response is not a JSON object: %v: %s", err, raw)
	}
	if string(fields["jsonrpc"]) != `"2.0"` {
		t.Errorf("jsonrpc = %s, want \"2.0\": %s", fields["jsonrpc"], raw)
	}
	if _, ok := fields["id"]; !ok {
		t.Errorf("response has no id: %s", raw)
	}
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	if hasResult == hasError {
		t.Errorf("response must have exactly one of result and error: %s", raw)
	}
	for k := range fields {
		if k != "jsonrpc" && k != "id" && k != "result" && k != "error" {
			t.Errorf("unexpected member %q: %s", k, raw)
		}
	}
	var res rpcResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Error != nil && res.Error.Message == "" {
		t.Errorf("error without message: %s", raw)
	}
	return res
}

// call は method を params で呼び、成功した結果を返す。JSON-RPC のエラーならテストを失敗させる。
func (e *testEnv) call(userID, method string, params any) json.RawMessage {
	e.t.Helper()
	res := e.rpc(userID, method, params)
	if res.Error != nil {
		e.t.Fatalf("%s: unexpected error %d %s", method, res.Error.Code, res.Error.Message)
	}
	return res.Result
}

// rpc は method を params（nil なら省略）で id 1 のリクエストとして送り、レスポンスを返す。
func (e *testEnv) rpc(userID, method string, params any) rpcResponse {
	e.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		e.t.Fatal(err)
	}
	r := e.post(userID, string(body), nil)
	if r.Status != http.StatusOK {
		e.t.Fatalf("%s: status %d: %s", method, r.Status, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		e.t.Errorf("Content-Type = %q", ct)
	}
	res := checkEnvelope(e.t, []byte(r.Body))
	if string(res.ID) != "1" {
		e.t.Errorf("%s: id = %s, want 1", method, res.ID)
	}
	return res
}

// toolResult は tools/call の結果を読んだもの。
type toolResult struct {
	Content           []content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func (e *testEnv) callTool(userID, name string, args any) toolResult {
	e.t.Helper()
	var res toolResult
	if err := json.Unmarshal(e.call(userID, "tools/call", map[string]any{"name": name, "arguments": args}), &res); err != nil {
		e.t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		e.t.Fatalf("%s: content = %+v, want one text block", name, res.Content)
	}
	return res
}

// toolOK はツールを呼び、成功した結果（structuredContent）を out に読み込む。text の本文は structuredContent と
// 同じ JSON であること（structuredContent を読まないクライアント向け）も確かめる。
func (e *testEnv) toolOK(userID, name string, args, out any) {
	e.t.Helper()
	res := e.callTool(userID, name, args)
	if res.IsError {
		e.t.Fatalf("%s: unexpected tool error: %s", name, res.Content[0].Text)
	}
	var fromText, fromStructured any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &fromText); err != nil {
		e.t.Fatalf("%s: text is not JSON: %v", name, err)
	}
	if err := json.Unmarshal(res.StructuredContent, &fromStructured); err != nil {
		e.t.Fatalf("%s: structuredContent: %v", name, err)
	}
	a, _ := json.Marshal(fromText)
	b, _ := json.Marshal(fromStructured)
	if string(a) != string(b) {
		e.t.Errorf("%s: text and structuredContent differ:\n%s\n%s", name, a, b)
	}
	if out != nil {
		if err := json.Unmarshal(res.StructuredContent, out); err != nil {
			e.t.Fatalf("%s: decode result: %v", name, err)
		}
	}
}

// toolErr はツールを呼び、ツールの実行時エラー（isError）のメッセージを返す。
func (e *testEnv) toolErr(userID, name string, args any) string {
	e.t.Helper()
	res := e.callTool(userID, name, args)
	if !res.IsError {
		e.t.Fatalf("%s: expected tool error, got %s", name, res.Content[0].Text)
	}
	if res.StructuredContent != nil {
		e.t.Errorf("%s: error result has structuredContent", name)
	}
	return res.Content[0].Text
}

func expectContains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%q does not contain %q", s, sub)
		}
	}
}

func TestHashAndGenerateToken(t *testing.T) {
	// SHA-256("abc") の既知の値。
	if got := HashToken("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("HashToken = %s", got)
	}
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateToken()
	if !strings.HasPrefix(a, TokenPrefix) || len(a) != len(TokenPrefix)+64 || a == b {
		t.Errorf("GenerateToken = %q, %q", a, b)
	}
}

func TestIssueTokenStoresOnlyHash(t *testing.T) {
	env := newTestEnv(t, Config{})
	raw := env.tokens[writerA.ID]
	var rows []struct {
		Hash   string `db:"token_hash"`
		UserID string `db:"user_id"`
	}
	if err := env.db.Select(&rows, `SELECT token_hash, user_id FROM mcp_tokens WHERE user_id = ?`, writerA.ID); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Hash != HashToken(raw) {
		t.Fatalf("stored rows = %+v, want the SHA-256 of the token", rows)
	}
	var n int
	if err := env.db.Get(&n, `SELECT COUNT(*) FROM mcp_tokens WHERE token_hash = ? OR name = ? OR id = ?`, raw, raw, raw); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("raw token is stored in mcp_tokens")
	}
}

func TestAuthentication(t *testing.T) {
	env := newTestEnv(t, Config{})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	unauthorized := func(name string, headers map[string]string, wantInvalidToken bool) {
		t.Helper()
		r := env.post("", ping, headers)
		if r.Status != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401: %s", name, r.Status, r.Body)
		}
		challenge := r.Header.Get("WWW-Authenticate")
		if !strings.HasPrefix(challenge, "Bearer ") || strings.Contains(challenge, "invalid_token") != wantInvalidToken {
			t.Errorf("%s: WWW-Authenticate = %q", name, challenge)
		}
		res := checkEnvelope(t, []byte(r.Body))
		if string(res.ID) != "null" || res.Error == nil {
			t.Errorf("%s: body = %s", name, r.Body)
		}
	}
	unauthorized("no header", nil, false)
	unauthorized("basic scheme", map[string]string{"Authorization": "Basic " + env.tokens[writerA.ID]}, false)
	unauthorized("empty bearer", map[string]string{"Authorization": "Bearer "}, false)
	unauthorized("unknown token", map[string]string{"Authorization": "Bearer cmsmcp_unknown"}, true)
	unauthorized("token hash instead of token", map[string]string{"Authorization": "Bearer " + HashToken(env.tokens[writerA.ID])}, true)
	unauthorized("too long token", map[string]string{"Authorization": "Bearer " + strings.Repeat("a", maxTokenBytes+1)}, true)

	past := time.Now().Add(-time.Minute)
	expired, _, err := IssueToken(context.Background(), env.db, writerA.ID, "expired", &past)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized("expired token", map[string]string{"Authorization": "Bearer " + expired}, true)

	// トークンを発行したユーザーを削除すると、トークンも CASCADE で消えて使えなくなる。
	gone := testUser{"gone-1", "gone", "退部者", auth.RoleWriter}
	env.addUser(gone)
	if r := env.post(gone.ID, ping, nil); r.Status != http.StatusOK {
		t.Fatalf("before delete: status %d", r.Status)
	}
	mustExec(t, env.db, `DELETE FROM users WHERE id = ?`, gone.ID)
	unauthorized("deleted user", map[string]string{"Authorization": "Bearer " + env.tokens[gone.ID]}, true)

	future := time.Now().Add(time.Hour)
	valid, tokenID, err := IssueToken(context.Background(), env.db, writerA.ID, "valid", &future)
	if err != nil {
		t.Fatal(err)
	}
	// スキーム名の大文字・小文字は区別しない（RFC 7235）。
	r := env.post("", ping, map[string]string{"Authorization": "bearer " + valid})
	if r.Status != http.StatusOK {
		t.Fatalf("valid token: status %d: %s", r.Status, r.Body)
	}
	var used presence
	if err := env.db.Get(&used, `SELECT last_used_at FROM mcp_tokens WHERE id = ?`, tokenID); err != nil {
		t.Fatal(err)
	}
	if !used.Valid {
		t.Error("last_used_at is not recorded")
	}
}

// presence は列が NULL でないかだけを見るための型（時刻の形式に依存しない）。
type presence struct {
	Valid bool
}

func (n *presence) Scan(v any) error {
	n.Valid = v != nil
	return nil
}

func TestUserWithoutCMSRoleIsForbidden(t *testing.T) {
	env := newTestEnv(t, Config{Tokens: fakeTokens{"guest-token": {ID: "guest", Role: "guest"}}})
	r := env.do(http.MethodPost, DefaultBasePath, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		map[string]string{"Authorization": "Bearer guest-token"})
	if r.Status != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", r.Status, r.Body)
	}
}

// fakeTokens は生のトークンからユーザーを引く TokenStore。
type fakeTokens map[string]*auth.User

func (f fakeTokens) Authenticate(_ context.Context, raw string) (*auth.User, error) {
	if u, ok := f[raw]; ok {
		return u, nil
	}
	return nil, ErrInvalidToken
}

func TestHTTPTransport(t *testing.T) {
	env := newTestEnv(t, Config{AllowedOrigins: []string{"https://cms.example.ac.jp"}})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		body    string
		want    int
	}{
		{"GET has no SSE stream", http.MethodGet, DefaultBasePath, nil, "", http.StatusMethodNotAllowed},
		{"DELETE has no session", http.MethodDelete, DefaultBasePath, nil, "", http.StatusMethodNotAllowed},
		{"unknown path", http.MethodPost, DefaultBasePath + "/unknown", nil, ping, http.StatusNotFound},
		{"trailing slash", http.MethodPost, DefaultBasePath + "/", nil, ping, http.StatusOK},
		{"text/plain body", http.MethodPost, DefaultBasePath, map[string]string{"Content-Type": "text/plain"}, ping, http.StatusUnsupportedMediaType},
		{"json with charset", http.MethodPost, DefaultBasePath, map[string]string{"Content-Type": "application/json; charset=utf-8"}, ping, http.StatusOK},
		{"html only accept", http.MethodPost, DefaultBasePath, map[string]string{"Accept": "text/html"}, ping, http.StatusNotAcceptable},
		{"wildcard accept", http.MethodPost, DefaultBasePath, map[string]string{"Accept": "*/*"}, ping, http.StatusOK},
		{"no accept", http.MethodPost, DefaultBasePath, map[string]string{"Accept": ""}, ping, http.StatusOK},
		{"supported protocol header", http.MethodPost, DefaultBasePath, map[string]string{"MCP-Protocol-Version": "2025-06-18"}, ping, http.StatusOK},
		{"unsupported protocol header", http.MethodPost, DefaultBasePath, map[string]string{"MCP-Protocol-Version": "1999-01-01"}, ping, http.StatusBadRequest},
		{"foreign origin", http.MethodPost, DefaultBasePath, map[string]string{"Origin": "https://evil.example"}, ping, http.StatusForbidden},
		{"allowed origin", http.MethodPost, DefaultBasePath, map[string]string{"Origin": "https://cms.example.ac.jp"}, ping, http.StatusOK},
		{"same host origin", http.MethodPost, DefaultBasePath, map[string]string{"Origin": env.http.URL}, ping, http.StatusOK},
		{"too large body", http.MethodPost, DefaultBasePath, nil, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", maxRequestBytes) + `"}}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := env.do(tt.method, tt.path, writerA.ID, tt.body, tt.headers)
			if r.Status != tt.want {
				t.Fatalf("status %d, want %d: %s", r.Status, tt.want, r.Body)
			}
			if r.Status == http.StatusMethodNotAllowed && r.Header.Get("Allow") != http.MethodPost {
				t.Errorf("Allow = %q", r.Header.Get("Allow"))
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
			}
			checkEnvelope(t, []byte(r.Body))
		})
	}
}

func TestJSONRPCSingleMessages(t *testing.T) {
	env := newTestEnv(t, Config{})
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantID     string
		wantCode   int // 0 なら成功の結果
	}{
		{"parse error", `{"jsonrpc": "2.0", "method": "foobar, "params": "bar", "baz]`, http.StatusBadRequest, "null", CodeParseError},
		{"empty body", ``, http.StatusBadRequest, "null", CodeParseError},
		{"not an object", `"ping"`, http.StatusBadRequest, "null", CodeInvalidRequest},
		{"null message", `null`, http.StatusBadRequest, "null", CodeInvalidRequest},
		{"method is not a string", `{"jsonrpc": "2.0", "method": 1, "params": "bar"}`, http.StatusBadRequest, "null", CodeInvalidRequest},
		{"missing jsonrpc keeps id", `{"id": 7, "method": "ping"}`, http.StatusBadRequest, "7", CodeInvalidRequest},
		{"wrong jsonrpc version", `{"jsonrpc": "1.0", "id": "a", "method": "ping"}`, http.StatusBadRequest, `"a"`, CodeInvalidRequest},
		{"missing method", `{"jsonrpc": "2.0", "id": 3}`, http.StatusBadRequest, "3", CodeInvalidRequest},
		{"object id", `{"jsonrpc": "2.0", "id": {"a": 1}, "method": "ping"}`, http.StatusBadRequest, "null", CodeInvalidRequest},
		{"boolean id", `{"jsonrpc": "2.0", "id": true, "method": "ping"}`, http.StatusBadRequest, "null", CodeInvalidRequest},
		{"string params", `{"jsonrpc": "2.0", "id": 4, "method": "ping", "params": "x"}`, http.StatusBadRequest, "4", CodeInvalidRequest},
		{"method not found", `{"jsonrpc": "2.0", "id": "abc", "method": "foobar"}`, http.StatusOK, `"abc"`, CodeMethodNotFound},
		{"reserved rpc method", `{"jsonrpc": "2.0", "id": 5, "method": "rpc.discover"}`, http.StatusOK, "5", CodeMethodNotFound},
		{"array params for object method", `{"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": [1, 2]}`, http.StatusOK, "6", CodeInvalidParams},
		{"wrong param type", `{"jsonrpc": "2.0", "id": 8, "method": "resources/read", "params": {"uri": 5}}`, http.StatusOK, "8", CodeInvalidParams},
		{"unknown tool", `{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": {"name": "cms_publish"}}`, http.StatusOK, "9", CodeInvalidParams},
		{"numeric id", `{"jsonrpc": "2.0", "id": 42, "method": "ping"}`, http.StatusOK, "42", 0},
		{"fractional id", `{"jsonrpc": "2.0", "id": 1.5, "method": "ping"}`, http.StatusOK, "1.5", 0},
		{"string id", `{"jsonrpc": "2.0", "id": "req-1", "method": "ping"}`, http.StatusOK, `"req-1"`, 0},
		{"null id", `{"jsonrpc": "2.0", "id": null, "method": "ping"}`, http.StatusOK, "null", 0},
		{"null params", `{"jsonrpc": "2.0", "id": 10, "method": "ping", "params": null}`, http.StatusOK, "10", 0},
		{"whitespace around body", " \n{\"jsonrpc\": \"2.0\", \"id\": 11, \"method\": \"ping\"}\n ", http.StatusOK, "11", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := env.post(writerA.ID, tt.body, nil)
			if r.Status != tt.wantStatus {
				t.Fatalf("status %d, want %d: %s", r.Status, tt.wantStatus, r.Body)
			}
			res := checkEnvelope(t, []byte(r.Body))
			if string(res.ID) != tt.wantID {
				t.Errorf("id = %s, want %s", res.ID, tt.wantID)
			}
			switch {
			case tt.wantCode == 0 && res.Error != nil:
				t.Errorf("unexpected error %+v", res.Error)
			case tt.wantCode != 0 && (res.Error == nil || res.Error.Code != tt.wantCode):
				t.Errorf("error = %+v, want code %d", res.Error, tt.wantCode)
			}
		})
	}
}

func TestJSONRPCNotificationsGetNoResponse(t *testing.T) {
	env := newTestEnv(t, Config{})
	for _, body := range []string{
		`{"jsonrpc": "2.0", "method": "notifications/initialized"}`,
		`{"jsonrpc": "2.0", "method": "ping"}`,
		`{"jsonrpc": "2.0", "method": "no/such/method"}`,
		`{"jsonrpc": "2.0", "method": "tools/call", "params": {"name": "cms_save_draft", "arguments": {"title": "通知で作らない"}}}`,
		// クライアントからのレスポンス（サーバーは要求を送らないため読み捨てる）。
		`{"jsonrpc": "2.0", "id": 1, "result": {}}`,
		`[{"jsonrpc": "2.0", "method": "notifications/initialized"}, {"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": 1}}]`,
	} {
		r := env.post(writerA.ID, body, nil)
		if r.Status != http.StatusAccepted || r.Body != "" {
			t.Errorf("%s: status %d body %q, want 202 without body", body, r.Status, r.Body)
		}
	}
	// 通知としてのツール呼び出しも実行はされるが、応答は返さない。
	var n int
	if err := env.db.Get(&n, `SELECT COUNT(*) FROM articles WHERE title = '通知で作らない'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("notification tool call created %d articles, want 1", n)
	}
}

func TestJSONRPCBatch(t *testing.T) {
	env := newTestEnv(t, Config{})
	r := env.post(writerA.ID, `[
		{"jsonrpc": "2.0", "method": "ping", "id": "1"},
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "method": "tools/list", "id": 2},
		{"foo": "boo"},
		{"jsonrpc": "2.0", "method": "foo.get", "params": {"name": "myself"}, "id": "5"},
		{"jsonrpc": "2.0", "method": "initialize", "params": {"protocolVersion": "2025-03-26"}, "id": "9"}
	]`, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(r.Body), &raws); err != nil {
		t.Fatalf("batch response is not an array: %s", r.Body)
	}
	want := []struct {
		id   string
		code int
	}{{`"1"`, 0}, {"2", 0}, {"null", CodeInvalidRequest}, {`"5"`, CodeMethodNotFound}, {`"9"`, 0}}
	if len(raws) != len(want) {
		t.Fatalf("got %d responses, want %d: %s", len(raws), len(want), r.Body)
	}
	for i, w := range want {
		res := checkEnvelope(t, raws[i])
		code := 0
		if res.Error != nil {
			code = res.Error.Code
		}
		if string(res.ID) != w.id || code != w.code {
			t.Errorf("response %d: id %s code %d, want id %s code %d", i, res.ID, code, w.id, w.code)
		}
	}

	// 不正な要素だけのバッチには、要素ごとに Invalid Request を返す。
	r = env.post(writerA.ID, `[1, 2, 3]`, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("[1,2,3]: status %d", r.Status)
	}
	if err := json.Unmarshal([]byte(r.Body), &raws); err != nil || len(raws) != 3 {
		t.Fatalf("[1,2,3]: %s", r.Body)
	}
	for _, raw := range raws {
		if res := checkEnvelope(t, raw); res.Error == nil || res.Error.Code != CodeInvalidRequest || string(res.ID) != "null" {
			t.Errorf("[1,2,3]: %s", raw)
		}
	}

	// 空のバッチ・JSON として壊れたバッチは単一のエラーを返す。
	for body, code := range map[string]int{
		`[]`: CodeInvalidRequest,
		`[{"jsonrpc": "2.0", "method": "ping", "id": "1"}, {"jsonrpc": "2.0", "method"]`: CodeParseError,
	} {
		r := env.post(writerA.ID, body, nil)
		if r.Status != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, r.Status)
		}
		if res := checkEnvelope(t, []byte(r.Body)); res.Error == nil || res.Error.Code != code || string(res.ID) != "null" {
			t.Errorf("%s: %s", body, r.Body)
		}
	}

	big := make([]string, maxBatchSize+1)
	for i := range big {
		big[i] = fmt.Sprintf(`{"jsonrpc": "2.0", "method": "ping", "id": %d}`, i)
	}
	if r := env.post(writerA.ID, "["+strings.Join(big, ",")+"]", nil); r.Status != http.StatusBadRequest {
		t.Errorf("oversized batch: status %d", r.Status)
	}
}

func TestPanicInToolBecomesInternalError(t *testing.T) {
	env := newTestEnv(t, Config{})
	// HTTP サーバーのゴルーチンと競合しないよう、ツールの追加と呼び出しはこのゴルーチンで行う。
	s := env.server
	s.tools = append(s.tools, &tool{
		info: toolInfo{Name: "boom", InputSchema: objectSchema(map[string]any{})},
		run:  func(context.Context, *auth.User, json.RawMessage) (any, error) { panic("boom") },
	})
	user := &auth.User{ID: writerA.ID, Role: auth.RoleWriter}
	status, out := s.handleBody(context.Background(), user, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`))
	raw, _ := json.Marshal(out)
	res := checkEnvelope(t, raw)
	if status != http.StatusOK || res.Error == nil || res.Error.Code != CodeInternalError {
		t.Fatalf("status %d response %s, want internal error", status, raw)
	}
	// パニックの後もサーバーは応答を続ける。通知のパニックには何も返さない。
	if status, out := s.handleBody(context.Background(), user, []byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"boom"}}`)); status != http.StatusAccepted || out != nil {
		t.Errorf("panicking notification: status %d out %v", status, out)
	}
	env.call(writerA.ID, "ping", nil)
}

func TestInitializeAndPing(t *testing.T) {
	env := newTestEnv(t, Config{Version: "9.9.9"})
	for requested, want := range map[string]string{
		"2024-11-05": "2024-11-05",
		"2025-03-26": "2025-03-26",
		"2025-06-18": "2025-06-18",
		"2025-11-25": "2025-11-25",
		"2099-01-01": LatestProtocolVersion,
		"":           LatestProtocolVersion,
	} {
		var res initializeResult
		raw := env.call(writerA.ID, "initialize", map[string]any{
			"protocolVersion": requested,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		})
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		if res.ProtocolVersion != want {
			t.Errorf("initialize(%q): protocolVersion = %q, want %q", requested, res.ProtocolVersion, want)
		}
		if res.Capabilities.Tools == nil || res.Capabilities.Resources == nil {
			t.Errorf("capabilities = %s", raw)
		}
		if res.ServerInfo.Name != "sorus" || res.ServerInfo.Title != "sorus — ローカルファーストの執筆・発信ツール" ||
			res.ServerInfo.Version != "9.9.9" || !strings.HasPrefix(res.Instructions, "sorus は、あなたのPCの中で動くローカルファーストの執筆・発信ツールです。") {
			t.Errorf("serverInfo = %+v instructions = %q", res.ServerInfo, res.Instructions)
		}
	}
	if raw := env.call(writerA.ID, "ping", nil); string(raw) != "{}" {
		t.Errorf("ping result = %s, want {}", raw)
	}
}

func TestToolsList(t *testing.T) {
	env := newTestEnv(t, Config{})
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema map[string]any  `json:"inputSchema"`
			Annotations toolAnnotations `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(env.call(writerA.ID, "tools/list", nil), &res); err != nil {
		t.Fatal(err)
	}
	wantReadOnly := map[string]bool{
		"cms_get_site_context": true, "cms_list_articles": true, "cms_get_article": true, "cms_save_draft": false,
		"cms_submit_for_review": false, "cms_proofread": true, "cms_search": true, "cms_diagnose_article": true,
		"cms_optimize_seo": true, "cms_suggest_taxonomy": true, "cms_approve_and_publish": false,
		"cms_publish_site": false, "cms_get_publish_status": true, "cms_check_publish_health": true,
		"cms_list_themes": true, "cms_install_theme": false, "cms_activate_theme": false, "cms_uninstall_theme": false,
		"cms_get_translation_status": true, "cms_create_translation": false, "cms_sync_translation": false,
	}
	wantDestructive := map[string]bool{"cms_uninstall_theme": true}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("%s: description %q schema %v", tl.Name, tl.Description, tl.InputSchema)
		}
		if _, ok := tl.InputSchema["properties"].(map[string]any); !ok {
			t.Errorf("%s: schema has no properties", tl.Name)
		}
		if tl.Annotations.ReadOnlyHint != wantReadOnly[tl.Name] || tl.Annotations.DestructiveHint != wantDestructive[tl.Name] {
			t.Errorf("%s: annotations = %+v", tl.Name, tl.Annotations)
		}
	}
	want := []string{
		"cms_get_site_context", "cms_list_articles", "cms_get_article", "cms_save_draft", "cms_submit_for_review",
		"cms_proofread", "cms_search", "cms_diagnose_article", "cms_optimize_seo", "cms_suggest_taxonomy",
		"cms_approve_and_publish", "cms_publish_site", "cms_get_publish_status", "cms_check_publish_health",
		"cms_list_themes", "cms_install_theme", "cms_activate_theme", "cms_uninstall_theme",
		"cms_get_translation_status", "cms_create_translation", "cms_sync_translation",
	}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

// seedLibrary は権限の確認に使う記事を登録する。
type library struct {
	draftA, deletedA, draftB, publishedB, reviewEditor string
}

func seedLibrary(env *testEnv) library {
	return library{
		draftA:       env.seed(article.Article{Title: "山田の下書き", ContentMarkdown: "# 見出し\n\n山田の本文", AuthorID: writerA.ID, Status: article.StatusDraft}),
		deletedA:     env.seed(article.Article{Title: "山田の削除済み", AuthorID: writerA.ID, Status: article.StatusDeleted}),
		draftB:       env.seed(article.Article{Title: "鈴木の下書き", ContentMarkdown: "鈴木の未公開の本文", AuthorID: writerB.ID, Status: article.StatusDraft}),
		publishedB:   env.seed(article.Article{Title: "鈴木の研究紹介", Slug: "suzuki-research", ContentMarkdown: "公開済みの本文", AuthorID: writerB.ID, CategoryID: categoryResearch, Status: article.StatusPublished}),
		reviewEditor: env.seed(article.Article{Title: "デスクの校閲待ち", AuthorID: editor.ID, Status: article.StatusReview}),
	}
}

func listIDs(res listArticlesResult) []string {
	var ids []string
	for _, a := range res.Articles {
		ids = append(ids, a.ID)
	}
	slices.Sort(ids)
	return ids
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func TestListArticles(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)

	tests := []struct {
		name string
		user string
		args map[string]any
		want []string
	}{
		// 管理画面の一覧と同じく、執筆者にも他の執筆者の下書き・校閲待ちを見せる（チームでの共有）。
		{"writer sees team articles like admin", writerA.ID, nil, sortedIDs(lib.draftA, lib.draftB, lib.publishedB, lib.reviewEditor)},
		{"editor sees all but trash", editor.ID, map[string]any{}, sortedIDs(lib.draftA, lib.draftB, lib.publishedB, lib.reviewEditor)},
		{"admin all statuses", admin.ID, map[string]any{"status": "all"}, sortedIDs(lib.draftA, lib.draftB, lib.publishedB, lib.reviewEditor)},
		{"writer trash", writerA.ID, map[string]any{"status": "deleted"}, sortedIDs(lib.deletedA)},
		{"writer drafts include teammates", writerA.ID, map[string]any{"status": "draft"}, sortedIDs(lib.draftA, lib.draftB)},
		{"editor drafts", editor.ID, map[string]any{"status": "draft"}, sortedIDs(lib.draftA, lib.draftB)},
		{"category by slug", editor.ID, map[string]any{"category": "research"}, sortedIDs(lib.publishedB)},
		{"category by name", editor.ID, map[string]any{"category": "研究"}, sortedIDs(lib.publishedB)},
		{"category by id", editor.ID, map[string]any{"category_id": categoryDefault}, sortedIDs(lib.draftA, lib.draftB, lib.reviewEditor)},
		{"mine", writerB.ID, map[string]any{"mine": true}, sortedIDs(lib.draftB, lib.publishedB)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var res listArticlesResult
			env.toolOK(tt.user, "cms_list_articles", tt.args, &res)
			if got := listIDs(res); !slices.Equal(got, tt.want) {
				t.Errorf("ids = %v, want %v", got, tt.want)
			}
			if res.Total != len(tt.want) {
				t.Errorf("total = %d, want %d", res.Total, len(tt.want))
			}
		})
	}

	var res listArticlesResult
	env.toolOK(editor.ID, "cms_list_articles", map[string]any{"category": "research"}, &res)
	if a := res.Articles[0]; a.Category != "研究" || a.Author != "鈴木" || a.Status != article.StatusPublished || a.PublishedAt == nil || a.Slug != "suzuki-research" {
		t.Errorf("row = %+v", a)
	}

	// ページング: 更新日時の新しい順に limit 件ずつ返し、続きがあれば next_offset を返す。
	env.toolOK(editor.ID, "cms_list_articles", map[string]any{"limit": 3}, &res)
	if len(res.Articles) != 3 || res.Total != 4 || res.NextOffset == nil || *res.NextOffset != 3 {
		t.Fatalf("page 1 = %d articles, total %d, next %v", len(res.Articles), res.Total, res.NextOffset)
	}
	if res.Articles[0].ID != lib.reviewEditor {
		t.Errorf("first = %s, want the most recently updated %s", res.Articles[0].ID, lib.reviewEditor)
	}
	res = listArticlesResult{}
	env.toolOK(editor.ID, "cms_list_articles", map[string]any{"limit": 3, "offset": 3}, &res)
	if len(res.Articles) != 1 || res.NextOffset != nil {
		t.Errorf("page 2 = %d articles, next %v", len(res.Articles), res.NextOffset)
	}

	expectContains(t, env.toolErr(writerA.ID, "cms_list_articles", map[string]any{"status": "secret"}), "status")
	expectContains(t, env.toolErr(writerA.ID, "cms_list_articles", map[string]any{"category": "スポーツ"}), "スポーツ", "研究（slug: research")
	expectContains(t, env.toolErr(writerA.ID, "cms_list_articles", map[string]any{"limit": maxListLimit + 1}), "limit")
	expectContains(t, env.toolErr(writerA.ID, "cms_list_articles", map[string]any{"statuses": "draft"}), "statuses")
	expectContains(t, env.toolErr(writerA.ID, "cms_list_articles", map[string]any{"limit": "10"}), "limit")
	expectContains(t, env.toolErr(writerA.ID, "cms_list_articles", []any{1}), "JSON オブジェクト")
}

// 同じミリ秒に書かれたコメントも、書いた順に返す（julianday はミリ秒単位で、ID はランダム）。
func TestGetArticleCommentsSameInstantKeepInsertOrder(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)
	for _, c := range []struct{ id, content string }{{"ffff", "先に書いた"}, {"0000", "後に書いた"}} {
		mustExec(t, env.db, `INSERT INTO article_comments (id, article_id, user_id, content, created_at) VALUES (?, ?, ?, ?, '2026-10-30 09:00:00')`,
			c.id, lib.draftA, editor.ID, c.content)
	}
	var d articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": lib.draftA}, &d)
	if len(d.Comments) != 2 || d.Comments[0].Content != "先に書いた" || d.Comments[1].Content != "後に書いた" {
		t.Errorf("comments = %+v", d.Comments)
	}
}

func TestGetArticle(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)
	mustExec(t, env.db, `INSERT INTO article_comments (id, article_id, user_id, content) VALUES ('c1', ?, ?, '【差し戻し】出典を追加してください')`,
		lib.draftA, editor.ID)

	var d articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": lib.draftA}, &d)
	if d.ID != lib.draftA || d.Title != "山田の下書き" || d.ContentMarkdown != "# 見出し\n\n山田の本文" || d.Status != article.StatusDraft {
		t.Errorf("detail = %+v", d)
	}
	if d.ResourceURI != "cms://articles/"+lib.draftA || d.EditPath != "/admin/articles/"+lib.draftA+"/edit" || d.PublicPath != "" {
		t.Errorf("paths = %q %q %q", d.ResourceURI, d.EditPath, d.PublicPath)
	}
	if len(d.Comments) != 1 || d.Comments[0].Author != "佐藤デスク" || !strings.Contains(d.Comments[0].Content, "出典") {
		t.Errorf("comments = %+v", d.Comments)
	}

	// 他の執筆者の公開記事はスラッグで取得できる。内部コメントは管理画面の編集画面と同じく執筆チームに返す。
	mustExec(t, env.db, `INSERT INTO article_comments (id, article_id, user_id, content) VALUES ('c2', ?, ?, '内部のやり取り')`,
		lib.publishedB, editor.ID)
	d = articleDetail{}
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"slug": "suzuki-research"}, &d)
	if d.ID != lib.publishedB || d.PublicPath != "/articles/suzuki-research" || len(d.Comments) != 1 {
		t.Errorf("published detail = %+v", d)
	}

	// 他の執筆者の下書きも管理画面と同じく読める。無い記事は見つからない。
	d = articleDetail{}
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": lib.draftB}, &d)
	if d.ContentMarkdown != "鈴木の未公開の本文" || d.PublicPath != "" {
		t.Errorf("teammate draft detail = %+v", d)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_get_article", map[string]any{"article_id": "no-such-id"}), "見つからない")
	d = articleDetail{}
	env.toolOK(editor.ID, "cms_get_article", map[string]any{"article_id": lib.draftB}, &d)
	if d.ContentMarkdown != "鈴木の未公開の本文" {
		t.Errorf("editor detail = %+v", d)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_get_article", map[string]any{}), "article_id か slug")
}

func TestSaveDraftCreate(t *testing.T) {
	env := newTestEnv(t, Config{})
	var res saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title": "  新歓レポート  ", "content_markdown": "本文1行目\r\n本文2行目", "excerpt": "リード", "category": "research",
	}, &res)
	a := env.article(res.Article.ID)
	if !res.Created || a.Title != "新歓レポート" || a.ContentMarkdown != "本文1行目\n本文2行目" || a.Excerpt != "リード" ||
		a.Status != article.StatusDraft || a.AuthorID != writerA.ID || a.CategoryID != categoryResearch || a.Slug != "新歓レポート" {
		t.Errorf("created = %+v (result %+v)", a, res)
	}
	if !strings.Contains(a.ContentHTML, "本文1行目") {
		t.Errorf("content_html is not rendered: %q", a.ContentHTML)
	}
	if res.EditPath != "/admin/articles/"+a.ID+"/edit" || res.EditURL != res.EditPath || res.Article.Author != "山田" {
		t.Errorf("result = %+v", res)
	}
	if got := revisionComments(t, env, a.ID); !slices.Equal(got, []string{"作成（MCP）"}) {
		t.Errorf("revisions = %v", got)
	}
	if got := env.changedIDs(); !slices.Equal(got, []string{a.ID}) {
		t.Errorf("OnArticleChanged = %v", got)
	}

	// BaseURL が設定されている場合は絶対 URL になること。
	envWithBase := newTestEnv(t, Config{BaseURL: "https://media.example.jp/"})
	var resWithBase saveDraftResult
	envWithBase.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "URL確認記事"}, &resWithBase)
	if wantURL := "https://media.example.jp/admin/articles/" + resWithBase.Article.ID + "/edit"; resWithBase.EditURL != wantURL {
		t.Errorf("EditURL with base = %q, want %q", resWithBase.EditURL, wantURL)
	}

	// 同じタイトルはスラッグに連番を付け、カテゴリ未指定は既定のカテゴリにする。
	var second saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "新歓レポート"}, &second)
	if second.Article.Slug != "新歓レポート-2" || second.Article.CategoryID != categoryDefault {
		t.Errorf("second = %+v", second.Article)
	}

	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"content_markdown": "タイトルなし"}), "title が必須")
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "   "}), "title を空")
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "x", "category_id": 99}), `"99"`)
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "x", "status": "published"}), "status")
}

func TestGetSiteContext(t *testing.T) {
	env := newTestEnv(t, Config{SiteName: "研究メディア", SiteDescription: "研究室のニュース", BaseURL: "https://example.test"})
	seedLibrary(env)
	var got struct {
		SiteName        string `json:"site_name"`
		SiteDescription string `json:"site_description"`
		Categories      []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"categories"`
		Recent []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Summary  string `json:"summary"`
			Category string `json:"category"`
		} `json:"recent_articles"`
		LLMS string `json:"llms_txt"`
	}
	env.toolOK(writerA.ID, "cms_get_site_context", map[string]any{}, &got)
	if got.SiteName != "研究メディア" || got.SiteDescription != "研究室のニュース" || got.LLMS != "https://example.test/llms.txt" {
		t.Fatalf("site context metadata = %+v", got)
	}
	if len(got.Categories) != 2 || len(got.Recent) == 0 || got.Recent[0].Title == "" {
		t.Fatalf("site context lists = %+v", got)
	}
}

func TestGetSiteContext_DynamicSiteInfo(t *testing.T) {
	currentBase := "https://initial.example.jp"
	currentName := "初期サイト名"
	currentDesc := "初期説明"

	env := newTestEnv(t, Config{
		SiteName:        "古い名前",
		SiteDescription: "古い説明",
		BaseURL:         "https://old.example.jp",
		GetBaseURL:         func() string { return currentBase },
		GetSiteName:        func() string { return currentName },
		GetSiteDescription: func() string { return currentDesc },
	})

	var got struct {
		SiteName        string `json:"site_name"`
		SiteDescription string `json:"site_description"`
		LLMS            string `json:"llms_txt"`
	}
	env.toolOK(writerA.ID, "cms_get_site_context", map[string]any{}, &got)
	if got.SiteName != "初期サイト名" || got.SiteDescription != "初期説明" || got.LLMS != "https://initial.example.jp/llms.txt" {
		t.Fatalf("site context dynamic initial = %+v", got)
	}

	// 動的に設定が変更された場合
	currentBase = "https://updated-dynamic.example.jp"
	currentName = "更新後サイト名"
	currentDesc = "更新後説明"

	env.toolOK(writerA.ID, "cms_get_site_context", map[string]any{}, &got)
	if got.SiteName != "更新後サイト名" || got.SiteDescription != "更新後説明" || got.LLMS != "https://updated-dynamic.example.jp/llms.txt" {
		t.Fatalf("site context dynamic updated = %+v", got)
	}

	// cms_save_draft の EditURL も動的 BaseURL を反映すること
	var res saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "動的URL記事"}, &res)
	if wantURL := "https://updated-dynamic.example.jp/admin/articles/" + res.Article.ID + "/edit"; res.EditURL != wantURL {
		t.Errorf("EditURL with dynamic base = %q, want %q", res.EditURL, wantURL)
	}

	// サイト名が空の場合は "sorus" にフォールバックすること
	currentName = "   "
	env.toolOK(writerA.ID, "cms_get_site_context", map[string]any{}, &got)
	if got.SiteName != "sorus" {
		t.Errorf("siteName fallback = %q, want 'sorus'", got.SiteName)
	}
}

func revisionComments(t *testing.T, env *testEnv, articleID string) []string {
	t.Helper()
	var out []string
	if err := env.db.Select(&out, `SELECT comment FROM article_revisions WHERE article_id = ? ORDER BY revision_num`, articleID); err != nil {
		t.Fatal(err)
	}
	return out
}

func lockedBy(t *testing.T, env *testEnv, articleID string) string {
	t.Helper()
	var by *string
	if err := env.db.Get(&by, `SELECT locked_by FROM articles WHERE id = ?`, articleID); err != nil {
		t.Fatal(err)
	}
	if by == nil {
		return ""
	}
	return *by
}

func TestSaveDraftUpdate(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)
	locker := lock.NewLocker(env.db, 0)
	ctx := context.Background()

	// 既存の記事の本文を変える保存は、記事を変えずに変更案として預ける（sorus の画面で確かめてから反映）。ロックは残さない。
	var res saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftA, "content_markdown": "書き直した本文"}, &res)
	a := env.article(lib.draftA)
	if res.Created || !res.NeedsConfirmation || res.Proposal == nil || a.Title != "山田の下書き" || a.ContentMarkdown != "# 見出し\n\n山田の本文" ||
		a.Status != article.StatusDraft {
		t.Errorf("content update = %+v (result %+v)", a, res)
	}
	if got := revisionComments(t, env, lib.draftA); len(got) != 0 {
		t.Errorf("revisions = %v", got)
	}
	if by := lockedBy(t, env, lib.draftA); by != "" {
		t.Errorf("lock left by %q", by)
	}

	// スラッグなどの設定だけの変更はそのまま保存する。指定した項目だけを更新し、他は保つ。
	res = saveDraftResult{}
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftA, "slug": "yamada-custom"}, &res)
	a = env.article(lib.draftA)
	if res.NeedsConfirmation || a.Slug != "yamada-custom" || a.Title != "山田の下書き" || a.ContentMarkdown != "# 見出し\n\n山田の本文" {
		t.Errorf("metadata update = %+v (result %+v)", a, res)
	}
	if got := revisionComments(t, env, lib.draftA); !slices.Equal(got, []string{"下書き保存（MCP）"}) {
		t.Errorf("revisions = %v", got)
	}
	if by := lockedBy(t, env, lib.draftA); by != "" {
		t.Errorf("lock left by %q", by)
	}
	// 空のスラッグはタイトルから作り直す。
	res = saveDraftResult{}
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftA, "slug": ""}, &res)
	if res.Article.Slug != "山田の下書き" || res.NeedsConfirmation {
		t.Errorf("slug regenerated = %+v", res.Article)
	}

	// 管理画面と同じく、執筆者も他の人の下書きを変更できる（文章の変更は案になる）。公開中の記事は変更できない。
	res = saveDraftResult{}
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftB, "title": "山田の提案"}, &res)
	if !res.NeedsConfirmation || env.article(lib.draftB).Title != "鈴木の下書き" {
		t.Errorf("teammate title change = %+v", res)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.publishedB, "title": "乗っ取り"}), "下書き（draft）の記事だけ")
	env.toolOK(editor.ID, "cms_save_draft", map[string]any{"article_id": lib.draftB, "slug": "desk-slug"}, nil)
	if b := env.article(lib.draftB); b.Slug != "desk-slug" || b.AuthorID != writerB.ID {
		t.Errorf("editor update = %+v", b)
	}

	// 下書き以外（公開中・校閲待ち）は編集者でも変更できない（案も作らない）。
	expectContains(t, env.toolErr(editor.ID, "cms_save_draft", map[string]any{"article_id": lib.publishedB, "title": "x"}), "下書き（draft）の記事だけ", "published")
	expectContains(t, env.toolErr(editor.ID, "cms_save_draft", map[string]any{"article_id": lib.reviewEditor, "title": "x"}), "review")
	if b := env.article(lib.publishedB); b.Title != "鈴木の研究紹介" || b.Status != article.StatusPublished {
		t.Errorf("published article changed: %+v", b)
	}
	if p, err := LoadProposal(ctx, env.db, lib.publishedB); err != nil || p != nil {
		t.Errorf("proposal for a published article = %+v, %v", p, err)
	}

	// 他のユーザーが管理画面で編集中なら保存せず、案も預からない。
	if _, err := locker.Acquire(ctx, lib.draftB, writerB.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := LoadProposal(ctx, env.db, lib.draftB)
	expectContains(t, env.toolErr(editor.ID, "cms_save_draft", map[string]any{"article_id": lib.draftB, "slug": "overwrite"}), "鈴木さんが管理画面で編集中")
	expectContains(t, env.toolErr(editor.ID, "cms_save_draft", map[string]any{"article_id": lib.draftB, "excerpt": "上書き"}), "鈴木さんが管理画面で編集中")
	if b := env.article(lib.draftB); b.Slug != "desk-slug" || b.Excerpt != "" {
		t.Errorf("locked article was overwritten: %+v", b)
	}
	if after, _ := LoadProposal(ctx, env.db, lib.draftB); after == nil || after.ID != before.ID {
		t.Errorf("proposal changed while locked: before %+v after %+v", before, after)
	}
	if by := lockedBy(t, env, lib.draftB); by != writerB.ID {
		t.Errorf("lock holder = %q, want %q", by, writerB.ID)
	}

	// 自分が管理画面で開いている記事は保存・提案でき、自分のロックは残す。
	if _, err := locker.Acquire(ctx, lib.draftA, writerA.ID); err != nil {
		t.Fatal(err)
	}
	res = saveDraftResult{}
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftA, "excerpt": "自分で追記"}, &res)
	if by := lockedBy(t, env, lib.draftA); by != writerA.ID || !res.NeedsConfirmation {
		t.Errorf("own lock holder = %q, want kept (result %+v)", by, res)
	}
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": lib.draftA, "slug": "own-locked"}, &res)
	if by := lockedBy(t, env, lib.draftA); by != writerA.ID || env.article(lib.draftA).Slug != "own-locked" {
		t.Errorf("own lock holder = %q, want kept", by)
	}

	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"article_id": "missing", "title": "x"}), "見つからない")
}

func TestSubmitForReview(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)

	var res submitResult
	env.toolOK(writerA.ID, "cms_submit_for_review", map[string]any{"article_id": lib.draftA, "note": "写真の掲載許可は取得済みです"}, &res)
	if res.Article.Status != article.StatusReview || env.article(lib.draftA).Status != article.StatusReview {
		t.Errorf("status = %s", res.Article.Status)
	}
	var comments []struct {
		UserID  string `db:"user_id"`
		Content string `db:"content"`
	}
	if err := env.db.Select(&comments, `SELECT user_id, content FROM article_comments WHERE article_id = ?`, lib.draftA); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].UserID != writerA.ID || comments[0].Content != "【校閲申請】写真の掲載許可は取得済みです" {
		t.Errorf("comments = %+v", comments)
	}
	if got := revisionComments(t, env, lib.draftA); !slices.Equal(got, []string{"校閲申請（MCP）"}) {
		t.Errorf("revisions = %v", got)
	}
	if by := lockedBy(t, env, lib.draftA); by != "" {
		t.Errorf("lock left by %q", by)
	}
	if got := env.changedIDs(); !slices.Equal(got, []string{lib.draftA}) {
		t.Errorf("OnArticleChanged = %v", got)
	}

	// 校閲待ち・公開中の記事は申請し直せない（他の人の下書きの申請は TestWriterSharesTeamArticlesLikeAdmin で確かめる）。
	expectContains(t, env.toolErr(writerA.ID, "cms_submit_for_review", map[string]any{"article_id": lib.draftA}), "review")
	expectContains(t, env.toolErr(writerA.ID, "cms_submit_for_review", map[string]any{"article_id": lib.publishedB}), "下書き（draft）の記事だけ")
	expectContains(t, env.toolErr(writerA.ID, "cms_submit_for_review", map[string]any{}), "article_id")
	expectContains(t, env.toolErr(writerB.ID, "cms_submit_for_review", map[string]any{"article_id": lib.draftB, "note": strings.Repeat("あ", maxNoteRunes+1)}), "note")

	// 他のユーザーが編集中なら申請しない。
	if _, err := lock.NewLocker(env.db, 0).Acquire(context.Background(), lib.draftB, editor.ID); err != nil {
		t.Fatal(err)
	}
	expectContains(t, env.toolErr(writerB.ID, "cms_submit_for_review", map[string]any{"article_id": lib.draftB}), "佐藤デスクさんが管理画面で編集中")
	if st := env.article(lib.draftB).Status; st != article.StatusDraft {
		t.Errorf("locked article status = %s", st)
	}

	// 申請メモが無ければ既定の文面を残す。編集者は他人の下書きも申請できる。
	if err := lock.NewLocker(env.db, 0).Release(context.Background(), lib.draftB, editor.ID); err != nil {
		t.Fatal(err)
	}
	env.toolOK(editor.ID, "cms_submit_for_review", map[string]any{"article_id": lib.draftB}, nil)
	var last string
	if err := env.db.Get(&last, `SELECT content FROM article_comments WHERE article_id = ?`, lib.draftB); err != nil {
		t.Fatal(err)
	}
	if last != "【校閲申請】校閲をお願いします。" {
		t.Errorf("default note = %q", last)
	}
}

// fakeAI は呼び出しを記録し、決まった加工をした結果を返す AI プロバイダー。
type fakeAI struct {
	mu   sync.Mutex
	err  error
	last struct {
		op, text, option string
		rag              []rag.SearchResult
	}
}

func (f *fakeAI) record(ctx context.Context, op, text, option string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last.op, f.last.text, f.last.option, f.last.rag = op, text, option, ai.RAGContextFrom(ctx)
	return f.err
}

func rewrite(text string) *ai.ProofreadResult {
	improved := strings.ReplaceAll(text, "誤字", "正しい字")
	return &ai.ProofreadResult{OriginalText: text, ImprovedText: improved, Diffs: ai.Diff(text, improved), Explanation: "誤字を直しました"}
}

func (f *fakeAI) Name() string { return "Fake AI" }

func (f *fakeAI) Proofread(ctx context.Context, text string) (*ai.ProofreadResult, error) {
	if err := f.record(ctx, "proofread", text, ""); err != nil {
		return nil, err
	}
	return rewrite(text), nil
}

func (f *fakeAI) SuggestTitles(ctx context.Context, text string) ([]string, error) {
	return nil, f.record(ctx, "titles", text, "")
}

func (f *fakeAI) Summarize(ctx context.Context, text string) (string, error) {
	return "リード文: " + text, f.record(ctx, "summary", text, "")
}

func (f *fakeAI) Simplify(ctx context.Context, text, audience string) (*ai.ProofreadResult, error) {
	if err := f.record(ctx, "simplify", text, audience); err != nil {
		return nil, err
	}
	return rewrite(text), nil
}

func (f *fakeAI) GenerateAbstract(ctx context.Context, text string) (string, error) {
	return "要旨: " + text, f.record(ctx, "abstract", text, "")
}

func (f *fakeAI) Translate(ctx context.Context, text, srcLang, targetLang string, opts ai.TranslateOptions) (string, error) {
	if err := f.record(ctx, "translate", text, targetLang); err != nil {
		return "", err
	}
	return "Translated", nil
}

func (f *fakeAI) StyleCheck(ctx context.Context, text, guide string) (*ai.ProofreadResult, error) {
	if err := f.record(ctx, "style-check", text, guide); err != nil {
		return nil, err
	}
	return rewrite(text), nil
}

func TestProofreadNotConfigured(t *testing.T) {
	env := newTestEnv(t, Config{})
	expectContains(t, env.toolErr(writerA.ID, "cms_proofread", map[string]any{"text": "本文"}), "設定されていません", "GEMINI_API_KEY")
}

func TestProofread(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnv(t, Config{AI: fake})
	lib := seedLibrary(env)

	var res proofreadResult
	env.toolOK(writerA.ID, "cms_proofread", map[string]any{"text": "誤字のある文章"}, &res)
	if res.Mode != "proofread" || res.Provider != "Fake AI" || res.ImprovedText != "正しい字のある文章" ||
		res.Changed == nil || !*res.Changed || res.Explanation != "誤字を直しました" {
		t.Errorf("proofread = %+v", res)
	}
	if res.Diff != "[-誤-]{+正しい+}字のある文章" {
		t.Errorf("diff = %q", res.Diff)
	}

	res = proofreadResult{}
	env.toolOK(writerA.ID, "cms_proofread", map[string]any{"text": "問題のない文章", "mode": "style-check", "style_guide": "数字は半角"}, &res)
	if res.Changed == nil || *res.Changed || fake.last.op != "style-check" || fake.last.option != "数字は半角" {
		t.Errorf("style-check = %+v, last = %+v", res, fake.last)
	}

	cases := []struct {
		args     map[string]any
		op, opt  string
		wantText string
	}{
		{map[string]any{"text": "量子", "mode": "simplify", "audience": "high-school"}, "simplify", "high-school", ""},
		{map[string]any{"text": "研究", "mode": "abstract"}, "abstract", "", "要旨: 研究"},
		{map[string]any{"text": "本文", "mode": "summary"}, "summary", "", "リード文: 本文"},
		{map[string]any{"text": "本文", "mode": "translate", "target_lang": "en"}, "translate", "en", "Translated"},
	}
	for _, c := range cases {
		res = proofreadResult{}
		env.toolOK(writerA.ID, "cms_proofread", c.args, &res)
		if fake.last.op != c.op || fake.last.option != c.opt || res.Text != c.wantText {
			t.Errorf("%v: result %+v, last %+v", c.args, res, fake.last)
		}
	}
	if res.SourceLang != "ja" || res.TargetLang != "en" {
		t.Errorf("translate langs = %s → %s", res.SourceLang, res.TargetLang)
	}

	// article_id なら記事の本文を推敲する。他の執筆者の下書きも管理画面と同じく対象にできる。無い記事は対象にできない。
	env.toolOK(writerA.ID, "cms_proofread", map[string]any{"article_id": lib.draftA}, nil)
	if fake.last.text != "# 見出し\n\n山田の本文" {
		t.Errorf("proofread text = %q", fake.last.text)
	}
	env.toolOK(writerA.ID, "cms_proofread", map[string]any{"article_id": lib.draftB}, nil)
	if fake.last.text != "鈴木の未公開の本文" {
		t.Errorf("proofread teammate text = %q", fake.last.text)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_proofread", map[string]any{"article_id": "no-such-id"}), "見つからない")

	expectContains(t, env.toolErr(writerA.ID, "cms_proofread", map[string]any{"text": "  "}), "text か")
	expectContains(t, env.toolErr(writerA.ID, "cms_proofread", map[string]any{"text": strings.Repeat("あ", maxAITextRunes+1)}), "長すぎます")
	expectContains(t, env.toolErr(writerA.ID, "cms_proofread", map[string]any{"text": "x", "mode": "publish"}), "mode")

	// AI の失敗は内部の詳細を出さずに、対処の分かる文言で返す。
	for err, want := range map[error]string{
		&ai.APIError{Provider: "fake", StatusCode: http.StatusTooManyRequests, Message: "secret detail"}: "利用上限",
		&ai.APIError{Provider: "fake", StatusCode: http.StatusUnauthorized, Message: "secret detail"}:    "API キーが無効",
		ai.ErrUnsupportedLanguage:                        "日本語と英語",
		fmt.Errorf("wrap: %w", context.DeadlineExceeded): "時間がかかりすぎ",
		ai.ErrEmptyResponse:                              "有効な応答がありません",
		errors.New("secret detail"):                      "応答を取得できませんでした",
	} {
		fake.err = err
		msg := env.toolErr(writerA.ID, "cms_proofread", map[string]any{"text": "本文"})
		expectContains(t, msg, want)
		if strings.Contains(msg, "secret") {
			t.Errorf("error message leaks details: %q", msg)
		}
	}
}

// fakeEmbedder は語の出現回数をベクトルにする決定的な rag.Embedder。
type fakeEmbedder struct{}

var vocab = []string{"量子", "計算", "サッカー", "料理"}

func (fakeEmbedder) Name() string  { return "Fake" }
func (fakeEmbedder) Model() string { return "fake-v1" }

func (fakeEmbedder) Embed(_ context.Context, texts []string, _ rag.TaskType) (*rag.Embeddings, error) {
	res := &rag.Embeddings{Model: "fake-v1"}
	for _, text := range texts {
		v := make([]float32, len(vocab)+1)
		for i, w := range vocab {
			v[i] = float32(strings.Count(text, w))
		}
		v[len(vocab)] = 0.1
		res.Vectors = append(res.Vectors, v)
	}
	return res, nil
}

func TestProofreadWithRAG(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnv(t, Config{AI: fake}, withRAG)
	id := env.seed(article.Article{Title: "量子計算の研究室", ContentMarkdown: "量子計算の研究を紹介します。量子計算は速い。", AuthorID: writerB.ID, Status: article.StatusPublished})
	if err := env.rag.IndexArticle(context.Background(), id, env.article(id).ContentMarkdown); err != nil {
		t.Fatal(err)
	}

	var res proofreadResult
	env.toolOK(writerA.ID, "cms_proofread", map[string]any{"text": "量子計算について書きます", "use_rag": true}, &res)
	if len(fake.last.rag) == 0 || fake.last.rag[0].ArticleID != id {
		t.Errorf("rag context = %+v", fake.last.rag)
	}
	if !slices.Equal(res.References, []string{"量子計算の研究室"}) {
		t.Errorf("references = %v", res.References)
	}
	// 推敲の対象の記事自身は参考資料から除く。
	env.toolOK(writerB.ID, "cms_proofread", map[string]any{"article_id": id, "use_rag": true}, &res)
	if len(fake.last.rag) != 0 {
		t.Errorf("rag context includes the article itself: %+v", fake.last.rag)
	}
}

func TestSearch(t *testing.T) {
	idx, err := search.NewMemIndex()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	env := newTestEnv(t, Config{Search: idx}, withRAG)
	lib := seedLibrary(env)
	if err := idx.SyncFromDB(context.Background(), env.db); err != nil {
		t.Fatal(err)
	}

	var res keywordSearchResult
	env.toolOK(writerA.ID, "cms_search", map[string]any{"query": "公開済み"}, &res)
	if res.Mode != "keyword" || res.Total != 1 || len(res.Hits) != 1 || res.Hits[0].ID != lib.publishedB {
		t.Fatalf("keyword = %+v", res)
	}
	if h := res.Hits[0]; h.Slug != "suzuki-research" || h.Category != "研究" || len(h.Snippets) == 0 || !strings.Contains(h.Snippets[0], "**公開済み**") {
		t.Errorf("hit = %+v", h)
	}
	// 下書きは検索に出ない（編集者でも）。
	env.toolOK(editor.ID, "cms_search", map[string]any{"query": "未公開"}, &res)
	if res.Total != 0 || res.Hits == nil {
		t.Errorf("draft found: %+v", res)
	}

	expectContains(t, env.toolErr(writerA.ID, "cms_search", map[string]any{"query": " "}), "query")
	expectContains(t, env.toolErr(writerA.ID, "cms_search", map[string]any{"query": "x", "mode": "fuzzy"}), "mode")
	expectContains(t, env.toolErr(writerA.ID, "cms_search", map[string]any{"query": "x", "limit": maxSearchLimit + 1}), "limit")
	expectContains(t, env.toolErr(writerA.ID, "cms_search", map[string]any{"query": strings.Repeat("あ", maxQueryRunes+1)}), "query")

	quantum := env.seed(article.Article{Title: "量子の話", ContentMarkdown: "量子計算の話。", AuthorID: writerB.ID, Status: article.StatusPublished})
	soccer := env.seed(article.Article{Title: "サッカー部", ContentMarkdown: "サッカーの試合。", AuthorID: writerB.ID, Status: article.StatusPublished})
	for _, id := range []string{quantum, soccer} {
		if err := env.rag.IndexArticle(context.Background(), id, env.article(id).ContentMarkdown); err != nil {
			t.Fatal(err)
		}
	}
	var sem semanticSearchResult
	env.toolOK(writerA.ID, "cms_search", map[string]any{"query": "量子計算", "mode": "semantic", "limit": 1}, &sem)
	if sem.Mode != "semantic" || len(sem.Results) != 1 || sem.Results[0].ArticleID != quantum {
		t.Errorf("semantic = %+v", sem)
	}

	// 検索インデックス・RAG を設定していなければ、その旨を返す。
	plain := newTestEnv(t, Config{})
	expectContains(t, plain.toolErr(writerA.ID, "cms_search", map[string]any{"query": "x"}), "全文検索が設定されていません")
	expectContains(t, plain.toolErr(writerA.ID, "cms_search", map[string]any{"query": "量子", "mode": "semantic"}), "意味検索（RAG）が設定されていません")
}

func TestResources(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)

	var list listResourcesResult
	if err := json.Unmarshal(env.call(writerA.ID, "resources/list", nil), &list); err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, r := range list.Resources {
		uris = append(uris, r.URI)
	}
	// 管理画面の一覧と同じく、執筆チームには他の人の下書き・校閲待ちも見せる（ゴミ箱は除く）。
	want := []string{categoriesURI, "cms://articles/" + lib.reviewEditor, "cms://articles/" + lib.publishedB,
		"cms://articles/" + lib.draftB, "cms://articles/" + lib.draftA}
	if !slices.Equal(uris, want) || list.NextCursor != "" {
		t.Errorf("resources = %v (next %q), want %v", uris, list.NextCursor, want)
	}
	if r := list.Resources[4]; r.Name != "山田の下書き" || r.Title != "山田の下書き" || r.MimeType != "text/markdown" {
		// Name はスラッグ。日本語タイトルのスラッグはタイトルと同じになる。
		t.Errorf("article resource = %+v", r)
	}

	var templates listResourceTemplatesResult
	if err := json.Unmarshal(env.call(writerA.ID, "resources/templates/list", nil), &templates); err != nil {
		t.Fatal(err)
	}
	if len(templates.ResourceTemplates) != 1 || templates.ResourceTemplates[0].URITemplate != "cms://articles/{id}" {
		t.Errorf("templates = %+v", templates)
	}

	var read readResourceResult
	if err := json.Unmarshal(env.call(writerA.ID, "resources/read", map[string]any{"uri": categoriesURI}), &read); err != nil {
		t.Fatal(err)
	}
	var cats []categoryRow
	if len(read.Contents) != 1 || read.Contents[0].MimeType != "application/json" || json.Unmarshal([]byte(read.Contents[0].Text), &cats) != nil {
		t.Fatalf("categories = %+v", read)
	}
	if len(cats) != 2 || cats[1].Slug != "research" || cats[1].PublishedArticles != 1 || cats[0].PublishedArticles != 0 {
		t.Errorf("categories = %+v", cats)
	}

	uri := "cms://articles/" + lib.draftA
	if err := json.Unmarshal(env.call(writerA.ID, "resources/read", map[string]any{"uri": uri}), &read); err != nil {
		t.Fatal(err)
	}
	c := read.Contents[0]
	if c.URI != uri || c.MimeType != "text/markdown" || !strings.HasPrefix(c.Text, "---\n") ||
		!strings.HasSuffix(c.Text, "---\n# 見出し\n\n山田の本文") {
		t.Errorf("article resource = %+v", c)
	}
	expectContains(t, c.Text, "title: 山田の下書き", "status: draft", "category: 未分類", "author: 山田")

	for _, bad := range []string{"cms://articles/missing", "cms://articles/", "cms://articles/" + lib.draftA + "/x", "cms://users", "https://example.com"} {
		res := env.rpc(writerA.ID, "resources/read", map[string]any{"uri": bad})
		if res.Error == nil || res.Error.Code != CodeResourceNotFound {
			t.Errorf("read %q: error = %+v, want resource not found", bad, res.Error)
		}
	}
	// 編集者も執筆者も他人の下書きを読める。
	env.call(editor.ID, "resources/read", map[string]any{"uri": "cms://articles/" + lib.draftB})
	env.call(writerA.ID, "resources/read", map[string]any{"uri": "cms://articles/" + lib.draftB})
}

func TestResourcesPagination(t *testing.T) {
	env := newTestEnv(t, Config{})
	for i := range resourcesPageSize + 5 {
		env.seed(article.Article{Title: fmt.Sprintf("記事%03d", i), AuthorID: writerA.ID, Status: article.StatusDraft})
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var list listResourcesResult
		if err := json.Unmarshal(env.call(writerA.ID, "resources/list", params), &list); err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range list.Resources {
			if seen[r.URI] {
				t.Errorf("duplicate resource %s", r.URI)
			}
			seen[r.URI] = true
		}
		if list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
		if pages > 3 {
			t.Fatal("pagination does not end")
		}
	}
	if pages != 2 || len(seen) != resourcesPageSize+5+1 {
		t.Errorf("pages = %d, resources = %d", pages, len(seen))
	}
	res := env.rpc(writerA.ID, "resources/list", map[string]any{"cursor": "not-a-cursor!"})
	if res.Error == nil || res.Error.Code != CodeInvalidParams {
		t.Errorf("invalid cursor: %+v", res.Error)
	}
}

// sseEvent は SSE のイベント1件。
type sseEvent struct{ name, data string }

func readEvent(t *testing.T, r *bufio.Reader) sseEvent {
	t.Helper()
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read sse: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "" && ev.name != "":
			return ev
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestLegacySSETransport(t *testing.T) {
	env := newTestEnv(t, Config{})

	if r := env.do(http.MethodGet, DefaultBasePath+"/sse", "", "", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated sse: status %d", r.Status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env.http.URL+DefaultBasePath+"/sse", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokens[writerA.ID])
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sse: status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	stream := bufio.NewReader(resp.Body)
	ev := readEvent(t, stream)
	if ev.name != "endpoint" || !strings.HasPrefix(ev.data, DefaultBasePath+"/messages?sessionId=") {
		t.Fatalf("first event = %+v", ev)
	}
	endpoint := ev.data

	r := env.do(http.MethodPost, endpoint, writerA.ID, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2024-11-05"}}`, nil)
	if r.Status != http.StatusAccepted {
		t.Fatalf("post message: status %d: %s", r.Status, r.Body)
	}
	ev = readEvent(t, stream)
	if ev.name != "message" {
		t.Fatalf("event = %+v", ev)
	}
	res := checkEnvelope(t, []byte(ev.data))
	if string(res.ID) != `"init"` || !strings.Contains(string(res.Result), `"protocolVersion":"2024-11-05"`) {
		t.Errorf("initialize over sse = %s", ev.data)
	}

	// 通知には何も送らず、バッチの応答は1件の配列として送る。
	if r := env.do(http.MethodPost, endpoint, writerA.ID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); r.Status != http.StatusAccepted {
		t.Fatalf("notification: status %d", r.Status)
	}
	if r := env.do(http.MethodPost, endpoint, writerA.ID, `[{"jsonrpc":"2.0","id":2,"method":"ping"}]`, nil); r.Status != http.StatusAccepted {
		t.Fatalf("batch: status %d", r.Status)
	}
	ev = readEvent(t, stream)
	var batch []json.RawMessage
	if err := json.Unmarshal([]byte(ev.data), &batch); err != nil || len(batch) != 1 || string(checkEnvelope(t, batch[0]).ID) != "2" {
		t.Errorf("batch over sse = %s", ev.data)
	}

	// 壊れた JSON は POST の応答で返す。
	if r := env.do(http.MethodPost, endpoint, writerA.ID, `{`, nil); r.Status != http.StatusBadRequest {
		t.Errorf("parse error: status %d", r.Status)
	}
	// 他のユーザーのトークンや存在しないセッションには送れない。
	if r := env.do(http.MethodPost, endpoint, writerB.ID, `{"jsonrpc":"2.0","id":3,"method":"ping"}`, nil); r.Status != http.StatusNotFound {
		t.Errorf("other user's session: status %d", r.Status)
	}
	if r := env.do(http.MethodPost, DefaultBasePath+"/messages?sessionId=unknown", writerA.ID, `{"jsonrpc":"2.0","id":3,"method":"ping"}`, nil); r.Status != http.StatusNotFound {
		t.Errorf("unknown session: status %d", r.Status)
	}

	// 接続を閉じるとセッションは消える。
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		env.server.sessions.mu.Lock()
		n := len(env.server.sessions.sessions)
		env.server.sessions.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions remain after disconnect", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := env.do(http.MethodPost, endpoint, writerA.ID, `{"jsonrpc":"2.0","id":4,"method":"ping"}`, nil); r.Status != http.StatusNotFound {
		t.Errorf("closed session: status %d", r.Status)
	}
}

func TestToolTextKeepsMarkdownReadable(t *testing.T) {
	env := newTestEnv(t, Config{})
	var saved saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "比較 <A> & <B>", "content_markdown": "a < b && c > d"}, &saved)
	res := env.callTool(writerA.ID, "cms_get_article", map[string]any{"article_id": saved.Article.ID})
	// HTML 用のエスケープ（\u003c など）をせず、AI がそのまま読めるテキストで返す。
	expectContains(t, res.Content[0].Text, `"content_markdown": "a < b && c > d"`, `"title": "比較 <A> & <B>"`)
	r := env.post(writerA.ID, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cms_get_article","arguments":{"article_id":"`+saved.Article.ID+`"}}}`, nil)
	if strings.Contains(r.Body, "\\u003c") || strings.Contains(r.Body, "\\u0026") || !strings.Contains(r.Body, "a < b && c > d") {
		t.Errorf("response escapes HTML characters: %s", r.Body)
	}
}

func TestLastUsedAtIsThrottled(t *testing.T) {
	env := newTestEnv(t, Config{})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	lastUsed := func() string {
		t.Helper()
		var v string
		if err := env.db.Get(&v, `SELECT COALESCE(CAST(last_used_at AS TEXT), '') FROM mcp_tokens WHERE token_hash = ?`, HashToken(env.tokens[writerA.ID])); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if lastUsed() != "" {
		t.Fatal("last_used_at is set before use")
	}
	env.post(writerA.ID, ping, nil)
	first := lastUsed()
	if first == "" {
		t.Fatal("last_used_at is not recorded")
	}
	time.Sleep(5 * time.Millisecond)
	env.post(writerA.ID, ping, nil)
	if got := lastUsed(); got != first {
		t.Errorf("last_used_at rewritten within %v: %s → %s", lastUsedInterval, first, got)
	}
	mustExec(t, env.db, `UPDATE mcp_tokens SET last_used_at = ? WHERE token_hash = ?`, time.Now().Add(-time.Hour).UTC(), HashToken(env.tokens[writerA.ID]))
	old := lastUsed()
	env.post(writerA.ID, ping, nil)
	if got := lastUsed(); got == old {
		t.Errorf("stale last_used_at was not updated: %s", got)
	}
}

func TestCloseEndsSSEStreams(t *testing.T) {
	env := newTestEnv(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, env.http.URL+DefaultBasePath+"/sse", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokens[writerA.ID])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	stream := bufio.NewReader(resp.Body)
	if ev := readEvent(t, stream); ev.name != "endpoint" {
		t.Fatalf("first event = %+v", ev)
	}

	env.server.Close()
	env.server.Close() // 2回目も安全
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, stream)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sse stream is still open after Close")
	}
	if r := env.do(http.MethodGet, DefaultBasePath+"/sse", writerA.ID, "", nil); r.Status != http.StatusServiceUnavailable {
		t.Errorf("sse after Close: status %d", r.Status)
	}
	// Streamable HTTP は Close の後も使える。
	env.call(writerA.ID, "ping", nil)
}

func TestBasePath(t *testing.T) {
	tokens := fakeTokens{"t": {ID: "u", Role: auth.RoleWriter}}
	db, err := database.Connect(filepath.Join(t.TempDir(), "base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for base, paths := range map[string][2]string{
		"/":        {"/", "/messages"},
		"mcp/v1/":  {"/mcp/v1", "/mcp/v1/messages"},
		"/api/mcp": {"/api/mcp", "/api/mcp/messages"},
	} {
		s, err := New(Config{DB: db, BasePath: base, Tokens: tokens})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, paths[0], strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("BasePath %q: POST %s = %d", base, paths[0], rec.Code)
		}
		// 存在しないセッションへの送信は 404 で、パスの割り当て自体は messages に届いている。
		req = httptest.NewRequest(http.MethodPost, paths[1]+"?sessionId=x", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "session not found") {
			t.Errorf("BasePath %q: POST %s = %d %s", base, paths[1], rec.Code, rec.Body)
		}
	}
	if _, err := New(Config{}); err == nil {
		t.Error("New without DB succeeded")
	}
}

func TestAIErrorUsesJapaneseMessagesForTruncatedAndProtectedContent(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}
	p := &fakeAI{}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ai.ErrTruncated, "AIの応答が途中で切れたため、保存していません。記事を短く分けてもう一度試してください"},
		{ai.ErrProtectedContentMissing, "翻訳でコードや画像の一部が失われたため、保存していません。もう一度試してください"},
	} {
		got := s.aiError(p, tc.err).Error()
		if got != tc.want {
			t.Errorf("aiError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
