package server

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"sorus/internal/desktop"
)

// failTokenDeletes は API トークン（mcp_tokens）の削除を必ず失敗させ、パスワードの変更が途中で失敗した状態を作る。
func (e *testEnv) failTokenDeletes() {
	e.t.Helper()
	if _, err := e.db.Exec(`CREATE TRIGGER fail_token_delete BEFORE DELETE ON mcp_tokens
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		e.t.Fatal(err)
	}
}

// expectMCPStatus は API トークン token で MCP サーバーの tools/list を呼び、HTTP の状態が want かを確かめる。
func (e *testEnv) expectMCPStatus(token string, want int, step string) {
	e.t.Helper()
	if res, _ := e.mcpCall(token, "tools/list", map[string]any{}); res.StatusCode != want {
		e.t.Fatalf("%s: MCP status %d, want %d", step, res.StatusCode, want)
	}
}

// 管理者がパスワードを再設定すると、その人はログアウトされ、AI アプリの接続（API トークン）も解除される。
// 結果の通知は、トークンを発行し直す必要があることを伝える。
func TestAdminPasswordResetDisconnectsAIApps(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	adm := env.loggedIn(admin)
	w := env.loggedIn(studentA)
	token := issueMCPToken(w, "研究室PCの Claude Code", "30")
	env.expectMCPStatus(token, http.StatusOK, "before the reset")

	loc := expectRedirect(t, adm.postForm("/admin/users/"+studentA.ID+"/password", url.Values{"password": {"new-password-1"}}),
		"/admin/users?flash=user_password_updated")
	expectContains(t, adm.get(loc).Body, "ログアウト", "AIアプリ", "トークンの発行し直し")

	env.expectMCPStatus(token, http.StatusUnauthorized, "after the reset")
	if n := env.countRows(`SELECT COUNT(*) FROM mcp_tokens WHERE user_id = ?`, studentA.ID); n != 0 {
		t.Errorf("API tokens left = %d, want 0", n)
	}
	expectRedirect(t, w.get("/admin/articles"), "/admin/login")
}

// パスワードの再設定が途中で失敗したら、何も変えずにエラーを出す（成功とは表示しない）。
func TestAdminPasswordResetFailureChangesNothing(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	adm := env.loggedIn(admin)
	w := env.loggedIn(studentA)
	token := issueMCPToken(w, "研究室PCの Claude Code", "30")
	env.failTokenDeletes()

	msg := userErrorRedirect(t, adm.postForm("/admin/users/"+studentA.ID+"/password", url.Values{"password": {"new-password-1"}}))
	if !strings.Contains(msg, "パスワード") {
		t.Errorf("error = %q", msg)
	}
	env.expectMCPStatus(token, http.StatusOK, "after the failed reset")
	expectStatus(t, w.get("/admin/articles"), http.StatusOK)
	expectStatus(t, env.newClient().login(studentA.Username, "new-password-1"), http.StatusUnauthorized)
	expectRedirect(t, env.newClient().login(studentA.Username, testPassword), "/admin")
}

// recoverPassword は「パスワードを忘れたとき」で再設定コードを作り、password に変える送信の応答を返す。
func (e *testEnv) recoverPassword(password string) *response {
	e.t.Helper()
	c := e.newClient()
	expectRedirect(e.t, c.postForm(recoveryCodePath, nil), recoveryPath)
	code := readRecoveryCode(e.t, filepath.Join(filepath.Dir(e.uploadDir), RecoveryFileName))
	return c.postForm(recoveryPath, url.Values{"code": {code}, "password": {password}, "password_confirm": {password}})
}

// 「パスワードを忘れたとき」で変えても、ログアウトされ、AI アプリの接続も解除される。ログインの画面の通知で、
// トークンを発行し直す必要があることを伝える。
func TestDesktopPasswordRecoveryDisconnectsAIApps(t *testing.T) {
	t.Setenv(desktop.NoBrowserEnv, "1")
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	token := issueMCPToken(c, "自宅の Cursor", "30")
	env.expectMCPStatus(token, http.StatusOK, "before the recovery")

	loc := expectRedirect(t, env.recoverPassword("new-password-1"), "/admin/login?flash=password_reset")
	expectContains(t, env.newClient().get(loc).Body, "新しいパスワードでログイン", "AIアプリ", "トークンの発行し直し")

	env.expectMCPStatus(token, http.StatusUnauthorized, "after the recovery")
	expectRedirect(t, c.get("/admin/articles"), "/admin/login")
}

// 「パスワードを忘れたとき」の再設定が途中で失敗したら、何も変えずに画面にエラーを出す。
func TestDesktopPasswordRecoveryFailureShowsError(t *testing.T) {
	t.Setenv(desktop.NoBrowserEnv, "1")
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	token := issueMCPToken(c, "自宅の Cursor", "30")
	env.failTokenDeletes()

	res := env.recoverPassword("new-password-1")
	expectStatus(t, res, http.StatusInternalServerError)
	expectContains(t, res.Body, "パスワードを変えられませんでした")
	expectNotContains(t, res.Body, flashMessages["password_reset"])

	env.expectMCPStatus(token, http.StatusOK, "after the failed recovery")
	expectStatus(t, c.get("/admin/articles"), http.StatusOK)
	expectStatus(t, env.newClient().login("hanako", "new-password-1"), http.StatusUnauthorized)
	expectRedirect(t, env.newClient().login("hanako", "password123"), "/admin/articles")
}
