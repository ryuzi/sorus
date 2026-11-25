package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"sorus/internal/auth"
)

// userErrorRedirect はユーザー管理画面へのエラー付きリダイレクトを検証し、エラー文言を返す。
func userErrorRedirect(t *testing.T, res *response) string {
	t.Helper()
	loc := expectRedirect(t, res, "/admin/users?error=")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	return u.Query().Get("error")
}

func (e *testEnv) userRole(id string) auth.Role {
	e.t.Helper()
	var role auth.Role
	if err := e.db.Get(&role, `SELECT role FROM users WHERE id = ?`, id); err != nil {
		e.t.Fatalf("select role of %q: %v", id, err)
	}
	return role
}

func (e *testEnv) userIDByName(username string) string {
	e.t.Helper()
	var id string
	if err := e.db.Get(&id, `SELECT id FROM users WHERE username = ?`, username); err != nil {
		e.t.Fatalf("select user %q: %v", username, err)
	}
	return id
}

func TestUsersListShowsAllUsers(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	res := adm.get("/admin/users")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "ユーザー管理", "登録ユーザー一覧（4名）",
		studentA.Username, studentB.Username, admin.Username, chief.Username,
		"ライター (Writer)", "編集者 (Editor)", "管理者 (Admin)", "あなた")
	// 自分自身には削除ボタンを出さない。
	if strings.Contains(res.Body, `action="/admin/users/`+admin.ID+`/delete"`) {
		t.Error("delete form for the current user is rendered")
	}
	expectContains(t, res.Body, `action="/admin/users/`+studentA.ID+`/delete"`)
}

func TestUserCreate(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	res := adm.postForm("/admin/users", url.Values{
		"username": {"tanaka"}, "display_name": {"田中"}, "password": {"tanaka-pass"}, "role": {"writer"},
	})
	expectRedirect(t, res, "/admin/users?flash=user_created")
	id := env.userIDByName("tanaka")
	if got := env.userRole(id); got != auth.RoleWriter {
		t.Fatalf("role = %q, want writer", got)
	}

	// 作成したユーザーでログインできる。
	c := env.newClient()
	expectRedirect(t, c.login("tanaka", "tanaka-pass"), "/admin")

	res = adm.get("/admin/users?flash=user_created")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "ユーザーを作成しました", "tanaka", "田中")

	// 入力不備はエラー付きで一覧に戻り、ユーザーは増えない。
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"duplicate", url.Values{"username": {"tanaka"}, "password": {"tanaka-pass"}, "role": {"writer"}}, "既に使用されています"},
		{"invalid username", url.Values{"username": {"a!"}, "password": {"long-enough"}, "role": {"writer"}}, "ユーザー名は3〜32文字"},
		{"short password", url.Values{"username": {"kato"}, "password": {"short"}, "role": {"writer"}}, "8文字以上"},
		{"invalid role", url.Values{"username": {"kato"}, "password": {"long-enough"}, "role": {"reader"}}, "ロール"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := env.countRows(`SELECT COUNT(*) FROM users`)
			msg := userErrorRedirect(t, adm.postForm("/admin/users", tc.form))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want it to contain %q", msg, tc.want)
			}
			if after := env.countRows(`SELECT COUNT(*) FROM users`); after != before {
				t.Errorf("user count = %d, want %d", after, before)
			}
		})
	}

	// エラー文言は一覧画面に表示される。
	res = adm.get("/admin/users?error=" + url.QueryEscape("このユーザー名は既に使用されています"))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `role="alert"`, "このユーザー名は既に使用されています")
}

func TestUserRoleUpdate(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	res := adm.postForm("/admin/users/"+studentA.ID+"/edit", url.Values{
		"display_name": {"山田（昇格）"}, "role": {"editor"},
	})
	expectRedirect(t, res, "/admin/users?flash=user_updated")
	if got := env.userRole(studentA.ID); got != auth.RoleEditor {
		t.Fatalf("role = %q, want editor", got)
	}
	var name string
	if err := env.db.Get(&name, `SELECT display_name FROM users WHERE id = ?`, studentA.ID); err != nil {
		t.Fatal(err)
	}
	if name != "山田（昇格）" {
		t.Errorf("display_name = %q", name)
	}

	// 昇格したユーザーは次のリクエストから編集者として承認操作ができる（403 にならない）。
	ed := env.loggedIn(studentA)
	id := env.loggedIn(studentB).createArticle("校閲対象", "本文")
	res = ed.postForm("/admin/articles/"+id+"/reject-review", url.Values{"comment": {"確認"}})
	if res.StatusCode == http.StatusForbidden {
		t.Fatal("promoted editor got 403 on reject-review")
	}

	// 無効なロールは拒否される。
	msg := userErrorRedirect(t, adm.postForm("/admin/users/"+studentB.ID+"/edit", url.Values{"role": {"root"}}))
	if !strings.Contains(msg, "権限が無効") {
		t.Errorf("error = %q", msg)
	}
	if got := env.userRole(studentB.ID); got != auth.RoleWriter {
		t.Errorf("role = %q, want writer", got)
	}
}

func TestUserLastAdminProtection(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	// 管理者が1人だけのときは降格できない。
	msg := userErrorRedirect(t, adm.postForm("/admin/users/"+admin.ID+"/edit", url.Values{
		"display_name": {admin.DisplayName}, "role": {"editor"},
	}))
	if !strings.Contains(msg, "最後の管理者") {
		t.Errorf("error = %q", msg)
	}
	if got := env.userRole(admin.ID); got != auth.RoleAdmin {
		t.Fatalf("role = %q, want admin", got)
	}

	// 2人目の管理者を作れば、最初の管理者を削除できる。ただし残る1人は削除・降格できない。
	expectRedirect(t, adm.postForm("/admin/users/"+chief.ID+"/edit", url.Values{
		"display_name": {chief.DisplayName}, "role": {"admin"},
	}), "/admin/users?flash=user_updated")
	adm2 := env.loggedIn(chief)
	expectRedirect(t, adm2.postForm("/admin/users/"+admin.ID+"/delete", nil), "/admin/users?flash=user_deleted")
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE id = ?`, admin.ID); n != 0 {
		t.Fatalf("admin still exists (count %d)", n)
	}

	msg = userErrorRedirect(t, adm2.postForm("/admin/users/"+chief.ID+"/edit", url.Values{
		"display_name": {chief.DisplayName}, "role": {"writer"},
	}))
	if !strings.Contains(msg, "最後の管理者") {
		t.Errorf("error = %q", msg)
	}
	if got := env.userRole(chief.ID); got != auth.RoleAdmin {
		t.Fatalf("role = %q, want admin", got)
	}
	// HTTP では最後の管理者は自分自身でもあるため、削除は自己削除防止で先に拒否される。
	// 管理者以外のアカウントからの削除はモデル層の最後の管理者保護で拒否される。
	if err := auth.DeleteUser(env.db, studentA.ID, chief.ID); !errors.Is(err, auth.ErrCannotDeleteLastAdmin) {
		t.Fatalf("DeleteUser(last admin) = %v, want ErrCannotDeleteLastAdmin", err)
	}
}

func TestUserPasswordChange(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	a := env.loggedIn(studentA)

	res := adm.postForm("/admin/users/"+studentA.ID+"/password", url.Values{"password": {"new-password-1"}})
	expectRedirect(t, res, "/admin/users?flash=user_password_updated")

	// 既存セッションは破棄され、再ログインが必要になる。
	expectRedirect(t, a.get("/admin/articles"), "/admin/login")

	c := env.newClient()
	expectStatus(t, c.login(studentA.Username, testPassword), http.StatusUnauthorized)
	expectRedirect(t, c.login(studentA.Username, "new-password-1"), "/admin")

	// 短すぎるパスワードは拒否され、パスワードは変わらない。
	msg := userErrorRedirect(t, adm.postForm("/admin/users/"+studentA.ID+"/password", url.Values{"password": {"short"}}))
	if !strings.Contains(msg, "8文字以上") {
		t.Errorf("error = %q", msg)
	}
	expectRedirect(t, env.newClient().login(studentA.Username, "new-password-1"), "/admin")

	// 存在しないユーザーはエラーになる。
	userErrorRedirect(t, adm.postForm("/admin/users/no-such-user/password", url.Values{"password": {"long-enough"}}))
}

func TestUserDeleteSelfRejected(t *testing.T) {
	env := newTestEnv(t)
	// 管理者が2人いても自分自身は削除できない（最後の管理者保護とは別の理由で拒否される）。
	if _, err := env.db.Exec(`UPDATE users SET role = 'admin' WHERE id = ?`, chief.ID); err != nil {
		t.Fatal(err)
	}
	adm := env.loggedIn(admin)

	msg := userErrorRedirect(t, adm.postForm("/admin/users/"+admin.ID+"/delete", nil))
	if !strings.Contains(msg, "自分自身") {
		t.Errorf("error = %q", msg)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE id = ?`, admin.ID); n != 1 {
		t.Fatal("admin deleted itself")
	}
}

func TestUserDeleteWithArticlesRejected(t *testing.T) {
	env := newTestEnv(t)
	env.loggedIn(studentA).createArticle("山田の記事", "本文")
	adm := env.loggedIn(admin)

	msg := userErrorRedirect(t, adm.postForm("/admin/users/"+studentA.ID+"/delete", nil))
	if !strings.Contains(msg, "執筆した記事が存在する") {
		t.Errorf("error = %q", msg)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE id = ?`, studentA.ID); n != 1 {
		t.Fatal("user with articles was deleted")
	}

	// 記事を持たないユーザーは削除でき、セッションも無効になる。
	b := env.loggedIn(studentB)
	expectRedirect(t, adm.postForm("/admin/users/"+studentB.ID+"/delete", nil), "/admin/users?flash=user_deleted")
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE id = ?`, studentB.ID); n != 0 {
		t.Fatal("user without articles was not deleted")
	}
	if n := env.countRows(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, studentB.ID); n != 0 {
		t.Errorf("sessions of deleted user = %d, want 0", n)
	}
	expectRedirect(t, b.get("/admin/articles"), "/admin/login")
}

func TestUserManagementForbiddenForNonAdmins(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{studentA, chief} {
		t.Run(string(u.Role), func(t *testing.T) {
			c := env.loggedIn(u)
			expectStatus(t, c.get("/admin/users"), http.StatusForbidden)
			expectStatus(t, c.postForm("/admin/users", url.Values{
				"username": {"intruder"}, "password": {"intruder-pass"}, "role": {"admin"},
			}), http.StatusForbidden)
			expectStatus(t, c.postForm("/admin/users/"+studentB.ID+"/edit", url.Values{"role": {"admin"}}), http.StatusForbidden)
			expectStatus(t, c.postForm("/admin/users/"+studentB.ID+"/password", url.Values{"password": {"hijacked-pass"}}), http.StatusForbidden)
			expectStatus(t, c.postForm("/admin/users/"+studentB.ID+"/delete", nil), http.StatusForbidden)
		})
	}
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE username = 'intruder'`); n != 0 {
		t.Error("non-admin created a user")
	}
	if got := env.userRole(studentB.ID); got != auth.RoleWriter {
		t.Errorf("studentB role = %q, want writer", got)
	}
	expectRedirect(t, env.newClient().login(studentB.Username, testPassword), "/admin")

	// 未ログインはログイン画面へ誘導される。
	expectRedirect(t, env.newClient().get("/admin/users"), "/admin/login")
}
