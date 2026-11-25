package server

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sorus/internal/desktop"
)

var recoveryCodePattern = regexp.MustCompile(`\b(\d{4})-(\d{4})\b`)

// readRecoveryCode はデータディレクトリの再設定コードのファイルからコード（1234-5678）を読む。
func readRecoveryCode(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	m := recoveryCodePattern.FindString(string(data))
	if m == "" {
		t.Fatalf("no code in %s:\n%s", file, data)
	}
	return m
}

// wrongCode は code と違う8桁のコードを返す。
func wrongCode(code string) string {
	if strings.HasPrefix(code, "0000") {
		return "1111-1111"
	}
	return "0000-0000"
}

func TestDesktopPasswordRecovery(t *testing.T) {
	t.Setenv(desktop.NoBrowserEnv, "1")
	env, _ := newDesktopEnv(t)
	env.completeSetup()
	file := filepath.Join(filepath.Dir(env.uploadDir), RecoveryFileName)
	c := env.newClient()

	// ログイン画面は、サイト管理者への問い合わせではなく、この PC での再設定へ案内する。
	login := c.get("/admin/login").Body
	expectContains(t, login, `href="/admin/login/recovery"`, "パスワードを忘れたとき")
	if strings.Contains(login, "サイト管理者") {
		t.Error("desktop login page tells the user to ask a site administrator")
	}
	expectContains(t, c.get(recoveryPath).Body, "再設定コードを作る", `action="/admin/login/recovery/code"`)

	// コードを作ると、データディレクトリのファイルに 4-4 桁で書き出す（所有者だけが読める）。
	expectRedirect(t, c.postForm(recoveryCodePath, nil), recoveryPath)
	code := readRecoveryCode(t, file)
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("code file mode = %v, %v", info, err)
	}
	page := c.get(recoveryPath).Body
	expectContains(t, page, `name="code"`, `name="password"`, RecoveryFileName, filepath.Dir(file))
	// ユーザーが1人なら、ユーザー名は尋ねず、パスワードを変えるユーザー名を見せる。
	if strings.Contains(page, `id="username"`) {
		t.Error("asks for the username although there is only one user")
	}
	expectContains(t, page, `id="recovery-username"`, "対象のユーザー名", "hanako",
		`<input type="text" name="username" value="hanako" autocomplete="username" readonly hidden>`)

	reset := func(code, password string) *response {
		return c.postForm(recoveryPath, url.Values{"code": {code}, "password": {password}, "password_confirm": {password}})
	}
	// 短いパスワードではコードを使わない（間違えた回数にも数えない）。
	res := reset(code, "short")
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "8文字以上")
	// 間違えると、あと何回試せるかを伝える。
	res = reset(wrongCode(code), "new-password-1")
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "再設定コードが違います", "あと 4 回")

	// 全角の数字や区切りなしでも受け付ける。新しいパスワードでログインでき、古いパスワードでは入れない。
	fullWidth := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '０' + (r - '0')
		}
		return -1
	}, code)
	expectRedirect(t, reset(fullWidth, "new-password-1"), "/admin/login?flash=password_reset&username=hanako")
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("code file still exists after use: %v", err)
	}
	expectStatus(t, env.newClient().login("hanako", "password123"), http.StatusUnauthorized)
	expectRedirect(t, env.newClient().login("hanako", "new-password-1"), "/admin/articles")

	// 1回だけ使える。
	res = reset(code, "new-password-2")
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "再設定コードがありません")
	expectRedirect(t, env.newClient().login("hanako", "new-password-1"), "/admin/articles")
}

// 「パスワードを忘れたとき」の一連の流れ（admin_ux_phase5_spec.md 1.2）。再設定コードを作り、区切りに全角のハイフンや
// 空白を含めて入力してパスワードを変えると、ユーザー名を入れたログイン画面へ戻り、変えたことを画面の中に出し続ける。
// そこから新しいパスワードでログインすると、管理画面へ入れる。
func TestDesktopPasswordRecoveryThenLogin(t *testing.T) {
	t.Setenv(desktop.NoBrowserEnv, "1")
	env, _ := newDesktopEnv(t)
	env.completeSetup()
	file := filepath.Join(filepath.Dir(env.uploadDir), RecoveryFileName)
	c := env.newClient()

	expectContains(t, c.get("/admin/login").Body, `href="/admin/login/recovery"`)
	expectRedirect(t, c.postForm(recoveryCodePath, nil), recoveryPath)
	page := c.get(recoveryPath).Body
	expectContains(t, page, "対象のユーザー名", "hanako")
	code := readRecoveryCode(t, file)
	typed := " " + strings.Replace(code, "-", "－", 1) + " "
	// 再設定の画面のフォームのまま送る（見えない username の欄も一緒に送られる）。
	res := c.postForm(recoveryPath, url.Values{"code": {typed}, "username": {"hanako"},
		"password": {"new-password-1"}, "password_confirm": {"new-password-1"}})
	expectStatus(t, res, http.StatusSeeOther)
	loc := res.Header.Get("Location")
	if want := "/admin/login?flash=password_reset&username=hanako"; loc != want {
		t.Fatalf("Location = %q, want %q", loc, want)
	}

	login := c.get(loc)
	expectStatus(t, login, http.StatusOK)
	expectContains(t, login.Body,
		`<p id="login-flash"`, `role="status"`,
		"パスワードを変更しました。新しいパスワードでログインしてください。",
		`value="hanako"`,
		`autocomplete="current-password" autofocus`,
	)
	expectNotContains(t, login.Body, `autocomplete="username" autofocus`)

	// 古いパスワードでは入れず、新しいパスワードなら入れる。
	expectStatus(t, env.newClient().login("hanako", "password123"), http.StatusUnauthorized)
	expectRedirect(t, c.postForm("/admin/login", url.Values{"username": {"hanako"}, "password": {"new-password-1"}}), "/admin/articles")
	expectStatus(t, c.get("/admin/articles"), http.StatusOK)

	// 英語の画面では、案内も英語で出す。
	en := env.newClient()
	en.useEnglish()
	expectContains(t, en.get(loc).Body, "Your password has been changed. Log in with your new password.")
}

// ログイン画面の ?username= は、ユーザー名に使える形式のときだけ欄に入れる。日本語入力のまま打った全角の
// ユーザー名でもログインできる。
func TestLoginPageUsernamePrefill(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	expectContains(t, c.get("/admin/login?username=hanako").Body, `value="hanako"`)
	for _, bad := range []string{"%3Cscript%3E", "a", strings.Repeat("x", 40), "%E3%81%AF%E3%81%AA%E3%81%93"} {
		body := c.get("/admin/login?username=" + bad).Body
		expectContains(t, body, `value=""`, `autocomplete="username" autofocus`)
		expectNotContains(t, body, "&lt;script&gt;", "xxxxxxxx")
	}
	// 通知が無いときは、フォームの中の通知を出さない。
	expectNotContains(t, c.get("/admin/login").Body, `id="login-flash"`)

	expectRedirect(t, env.newClient().login("ｙａｍａｄａ", testPassword), "/admin/articles")
	expectRedirect(t, env.newClient().login("　yamada ", testPassword), "/admin/articles")
	if got := normalizeUsername(" ｔａｒｏ＿０１ "); got != "taro_01" {
		t.Errorf("normalizeUsername = %q, want taro_01", got)
	}
}

// useRecoveryClock は、この試験で作る再設定コードの状態の現在時刻を *now にする。
func useRecoveryClock(t *testing.T, now *time.Time) {
	t.Helper()
	old := recoveryClock
	recoveryClock = func() time.Time { return *now }
	t.Cleanup(func() { recoveryClock = old })
}

func TestDesktopPasswordRecoveryLocksAfterFiveWrongCodes(t *testing.T) {
	t.Setenv(desktop.NoBrowserEnv, "1")
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	useRecoveryClock(t, &now)
	env, _ := newDesktopEnv(t)
	env.completeSetup()
	file := filepath.Join(filepath.Dir(env.uploadDir), RecoveryFileName)
	c := env.newClient()
	expectRedirect(t, c.postForm(recoveryCodePath, nil), recoveryPath)
	code := readRecoveryCode(t, file)
	form := url.Values{"code": {wrongCode(code)}, "password": {"new-password-1"}, "password_confirm": {"new-password-1"}}
	for i := 1; i < recoveryMaxAttempts; i++ {
		expectContains(t, c.postForm(recoveryPath, form).Body, "再設定コードが違います")
	}
	res := c.postForm(recoveryPath, form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "5回間違えた", "しばらく待ってから、もう一度お試しください（あと 15 分）", "再設定コードを作る")
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("code file still exists after lock: %v", err)
	}
	// 正しいコードでも、もう使えない。
	form.Set("code", code)
	expectContains(t, c.postForm(recoveryPath, form).Body, "再設定コードがありません")

	// 15分たつまでは、コードを作り直して次の5回を試すことはできない。
	now = now.Add(5 * time.Minute)
	res = c.postForm(recoveryCodePath, nil)
	expectStatus(t, res, http.StatusTooManyRequests)
	expectContains(t, res.Body, "しばらく待ってから、もう一度お試しください（あと 10 分）", "再設定コードを作る")
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a new code file was written during the lockout: %v", err)
	}
	form.Set("code", wrongCode(code))
	for i := 0; i < recoveryMaxAttempts; i++ {
		res := c.postForm(recoveryPath, form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "再設定コードがありません")
	}
	now = now.Add(9*time.Minute + 30*time.Second)
	expectContains(t, c.postForm(recoveryCodePath, nil).Body, "あと 1 分")
	expectRedirect(t, env.newClient().login("hanako", "password123"), "/admin/articles")

	// 15分たてば、作り直したコードで再設定できる。
	now = now.Add(30 * time.Second)
	expectRedirect(t, c.postForm(recoveryCodePath, nil), recoveryPath)
	form.Set("code", readRecoveryCode(t, file))
	expectRedirect(t, c.postForm(recoveryPath, form), "/admin/login?flash=password_reset")
	expectRedirect(t, env.newClient().login("hanako", "new-password-1"), "/admin/articles")
}

// 間違えた回数はコードを作り直しても戻らない。作り直しながら4回ずつ試すことはできない。
func TestRecoveryAttemptBudgetSurvivesReissue(t *testing.T) {
	r := newRecoveryState(t.TempDir())
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	if err := r.issue(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < recoveryMaxAttempts; i++ {
		if _, err := r.verify(wrongCode(readRecoveryCode(t, r.file))); !errors.Is(err, errRecoveryWrong) {
			t.Fatalf("wrong code %d = %v, want errRecoveryWrong", i, err)
		}
	}
	if err := r.issue(); err != nil {
		t.Fatal(err)
	}
	code := readRecoveryCode(t, r.file)
	if _, err := r.verify(wrongCode(code)); !errors.Is(err, errRecoveryLocked) {
		t.Fatalf("5th wrong code after reissue = %v, want errRecoveryLocked", err)
	}
	var cooldown *recoveryCooldownError
	if err := r.issue(); !errors.As(err, &cooldown) || cooldown.Wait != recoveryLockout {
		t.Fatalf("issue during lockout = %v, want recoveryCooldownError of %s", err, recoveryLockout)
	}
	if r.active() {
		t.Error("a code is active during the lockout")
	}
	now = now.Add(recoveryLockout)
	if err := r.issue(); err != nil {
		t.Fatalf("issue after lockout = %v", err)
	}
	// 解けたあとは、また5回まで試せる。
	if remaining, err := r.verify(wrongCode(readRecoveryCode(t, r.file))); !errors.Is(err, errRecoveryWrong) || remaining != recoveryMaxAttempts-1 {
		t.Errorf("first wrong code after lockout = %d, %v", remaining, err)
	}

	// しばらく間違えなければ、間違えた回数は数え直す（前の日の打ち間違いで、すぐ待たされない）。
	now = now.Add(recoveryLockout)
	if err := r.issue(); err != nil {
		t.Fatal(err)
	}
	if remaining, err := r.verify(wrongCode(readRecoveryCode(t, r.file))); !errors.Is(err, errRecoveryWrong) || remaining != recoveryMaxAttempts-1 {
		t.Errorf("wrong code after a quiet period = %d, %v, want %d left", remaining, err, recoveryMaxAttempts-1)
	}
}

func TestDesktopPasswordRecoveryWithSeveralUsers(t *testing.T) {
	t.Setenv(desktop.NoBrowserEnv, "1")
	env, _ := newDesktopEnv(t)
	env.completeSetup()
	if _, err := env.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('u2', 'taro', 'Taro', 'x', 'writer')`); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(env.uploadDir), RecoveryFileName)
	c := env.newClient()
	expectRedirect(t, c.postForm(recoveryCodePath, nil), recoveryPath)
	expectContains(t, c.get(recoveryPath).Body, `name="username"`)
	code := readRecoveryCode(t, file)
	form := url.Values{"code": {code}, "username": {"nobody"}, "password": {"new-password-1"}, "password_confirm": {"new-password-1"}}
	expectContains(t, c.postForm(recoveryPath, form).Body, "そのユーザー名のアカウントがありません")
	// ユーザーが2人以上なら、再設定画面にほかの人のユーザー名を出さない。
	expectNotContains(t, c.get(recoveryPath).Body, "対象のユーザー名")
	form.Set("username", "hanako")
	expectRedirect(t, c.postForm(recoveryPath, form), "/admin/login?flash=password_reset&username=hanako")
	expectRedirect(t, env.newClient().login("hanako", "new-password-1"), "/admin/articles")
}

func TestPasswordRecoveryNotFoundInServerMode(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	login := c.get("/admin/login").Body
	if strings.Contains(login, "/admin/login/recovery") {
		t.Error("server mode login page links to the desktop recovery")
	}
	expectContains(t, login, "サイト管理者")
	expectStatus(t, c.get(recoveryPath), http.StatusNotFound)
	expectStatus(t, c.postForm(recoveryCodePath, nil), http.StatusNotFound)
	expectStatus(t, c.postForm(recoveryPath, url.Values{"code": {"1234-5678"}}), http.StatusNotFound)
}

func TestRecoveryCodeExpires(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, RecoveryFileName)
	if err := os.WriteFile(stale, []byte("1111-2222"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRecoveryState(dir)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a code file left by the previous run was not removed")
	}
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	if err := r.issue(); err != nil {
		t.Fatal(err)
	}
	code := readRecoveryCode(t, r.file)
	if !r.active() {
		t.Fatal("new code is not active")
	}
	now = now.Add(recoveryCodeTTL)
	if r.active() {
		t.Error("code is active after 15 minutes")
	}
	if _, err := r.verify(code); !errors.Is(err, errRecoveryExpired) {
		t.Fatalf("verify after expiry = %v, want errRecoveryExpired", err)
	}
	if _, err := os.Stat(r.file); !errors.Is(err, os.ErrNotExist) {
		t.Error("expired code file was not removed")
	}
	if _, err := r.verify(code); !errors.Is(err, errRecoveryNoCode) {
		t.Errorf("verify after expiry again = %v, want errRecoveryNoCode", err)
	}
	// 新しいコードを作ると、前のコードは使えない。
	now = now.Add(time.Minute)
	if err := r.issue(); err != nil {
		t.Fatal(err)
	}
	first := readRecoveryCode(t, r.file)
	if err := r.issue(); err != nil {
		t.Fatal(err)
	}
	second := readRecoveryCode(t, r.file)
	if first != second {
		if _, err := r.verify(first); !errors.Is(err, errRecoveryWrong) {
			t.Errorf("verify old code = %v, want errRecoveryWrong", err)
		}
	}
	if _, err := r.verify(second); err != nil {
		t.Errorf("verify current code = %v", err)
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	for in, want := range map[string]string{"1234-5678": "12345678", " 1234 5678 ": "12345678", "１２３４－５６７８": "12345678", "abc": ""} {
		if got := normalizeRecoveryCode(in); got != want {
			t.Errorf("normalizeRecoveryCode(%q) = %q, want %q", in, got, want)
		}
	}
}
