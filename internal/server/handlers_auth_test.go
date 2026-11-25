package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"sorus/internal/mcp"
)

// loginFrom は X-Forwarded-For を付けてログインのフォームを送る（送り元の偽装を試すため）。
func (c *client) loginFrom(xff, username, password string) *response {
	c.t.Helper()
	form := url.Values{"username": {username}, "password": {password}, CSRFFormField: {c.csrfToken()}}
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+"/admin/login", strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	return c.do(req)
}

// expectLoginLocked はログインが 429（Retry-After・待つ時間の案内付き）で断られたことを確かめる。
func expectLoginLocked(t *testing.T, res *response) {
	t.Helper()
	expectStatus(t, res, http.StatusTooManyRequests)
	secs, err := strconv.Atoi(res.Header.Get("Retry-After"))
	if err != nil || secs < 1 || secs > int(oauthRateWindow/time.Second) {
		t.Errorf("Retry-After = %q", res.Header.Get("Retry-After"))
	}
	expectContains(t, res.Body, "ログインの失敗が続いたため")
	for _, ck := range res.Cookies() {
		if strings.Contains(ck.Name, "session") && ck.Value != "" {
			t.Errorf("a session cookie was issued on a locked login: %v", ck)
		}
	}
}

// 同じユーザー名でパスワードを続けて間違えると、上限に達した後は正しいパスワードでも 429 で断る（区切りが変わるまで）。
// 他のユーザー名のログインは止めない。区切りが変わればログインできる。
func TestAdminLoginLocksUserAfterFailures(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	env.server.oauthLimits.adminLoginUser.setClock(func() time.Time { return now })
	c := env.newClient()
	for i := range adminLoginFailPerUser {
		res := c.login(studentA.Username, "wrong-password")
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, res.StatusCode)
		}
	}
	expectLoginLocked(t, c.login(studentA.Username, testPassword))

	// 別のユーザーは同じ送り元からでもログインできる（送り元ごとの上限はユーザー名ごとより多い）。
	expectRedirect(t, env.newClient().login(studentB.Username, testPassword), "/admin/articles")

	now = now.Add(oauthRateWindow)
	expectRedirect(t, env.newClient().login(studentA.Username, testPassword), "/admin/articles")
}

// 正しいパスワードでログインできたら、そのユーザー名の誤りの数を数え直す。
func TestAdminLoginSuccessResetsUserCounter(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	for range adminLoginFailPerUser - 1 {
		expectStatus(t, c.login(studentA.Username, "wrong-password"), http.StatusUnauthorized)
	}
	expectRedirect(t, env.newClient().login(studentA.Username, testPassword), "/admin/articles")
	for range adminLoginFailPerUser - 1 {
		expectStatus(t, c.login(studentA.Username, "wrong-password"), http.StatusUnauthorized)
	}
	expectRedirect(t, env.newClient().login(studentA.Username, testPassword), "/admin/articles")
}

// 登録されていないユーザー名も同じ上限で止める（止まり方からユーザー名の有無が分からない）。ユーザー名に使えない
// 形式の入力はまとめて数える。
func TestAdminLoginLocksUnknownUsernames(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	for range adminLoginFailPerUser {
		expectStatus(t, c.login("no-such-user", "x"), http.StatusUnauthorized)
	}
	expectLoginLocked(t, c.login("no-such-user", "x"))

	for i := range adminLoginFailPerUser {
		expectStatus(t, c.login(fmt.Sprintf("bad name %d !", i), "x"), http.StatusUnauthorized)
	}
	expectLoginLocked(t, c.login("another bad name!", "x"))
	if n := len(env.server.oauthLimits.adminLoginUnknown.entries); n != 2 {
		t.Errorf("unknown username entries = %d, want 2 (no-such-user and the shared invalid-name key)", n)
	}
	// 登録されているユーザーには影響しない。
	expectRedirect(t, env.newClient().login(studentA.Username, testPassword), "/admin/articles")
}

// 送り元ごとの上限に達したら、ユーザー名を変えても、正しいパスワードでも 429 で断る。Config.TrustedProxies が
// 未設定なら、X-Forwarded-For を書き換えても別の送り元として数えられない（環境変数 TRUSTED_PROXIES がテストの
// クライアントを信頼していても、サーバーは Config だけを使う）。
func TestAdminLoginLocksClientAfterFailures(t *testing.T) {
	t.Setenv(mcp.TrustedProxiesEnv, "127.0.0.1,::1")
	env := newTestEnv(t)
	env.server.oauthLimits.adminLoginClient.setLimit(3)
	c := env.newClient()
	for i, name := range []string{studentA.Username, studentB.Username, "no-such-user"} {
		res := c.loginFrom(fmt.Sprintf("198.51.100.%d", i+1), name, "wrong-password")
		expectStatus(t, res, http.StatusUnauthorized)
	}
	expectLoginLocked(t, c.loginFrom("198.51.100.99", admin.Username, testPassword))
	expectLoginLocked(t, env.newClient().login(chief.Username, testPassword))
}

// Config.TrustedProxies のリバースプロキシ（ここではテストのクライアント 127.0.0.1）の後ろでは、プロキシが付けた
// X-Forwarded-For の送り元ごとに数える（環境変数 TRUSTED_PROXIES が空でも）。
func TestAdminLoginBehindTrustedProxy(t *testing.T) {
	t.Setenv(mcp.TrustedProxiesEnv, "")
	env := newTestEnvWith(t, Config{TrustedProxies: []string{"127.0.0.1", "::1"}}, nil)
	env.server.oauthLimits.adminLoginClient.setLimit(2)
	c := env.newClient()
	for range 2 {
		expectStatus(t, c.loginFrom("198.51.100.1", "no-such-user", "x"), http.StatusUnauthorized)
	}
	expectLoginLocked(t, c.loginFrom("198.51.100.1", studentA.Username, testPassword))
	// 別の送り元（プロキシが付けた値）からは止めない。
	expectRedirect(t, env.newClient().loginFrom("198.51.100.2", studentA.Username, testPassword), "/admin/articles")
}
