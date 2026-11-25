package auth

import (
	"errors"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// 管理者は、サーバーモードでもデスクトップモードでも、編集者・ライターのすべての役割を持つ。
// 編集者・ライターの役割は保存上の役割のまま。
func TestUserHasRole(t *testing.T) {
	admin := &User{Role: RoleAdmin}
	editor := &User{Role: RoleEditor}
	writer := &User{Role: RoleWriter}
	var none *User

	cases := []struct {
		name  string
		user  *User
		roles []Role
		want  bool
	}{
		{"admin as admin", admin, []Role{RoleAdmin}, true},
		{"admin as editor", admin, []Role{RoleEditor}, true},
		{"admin as writer", admin, []Role{RoleWriter}, true},
		{"admin as author", admin, []Role{RoleEditor, RoleWriter}, true},
		{"admin with no roles", admin, nil, false},
		{"admin with unknown role", admin, []Role{"owner"}, false},
		{"editor as editor or admin", editor, []Role{RoleAdmin, RoleEditor}, true},
		{"editor as admin", editor, []Role{RoleAdmin}, false},
		{"editor as writer", editor, []Role{RoleWriter}, false},
		{"writer as writer", writer, []Role{RoleWriter}, true},
		{"writer as editor", writer, []Role{RoleEditor}, false},
		{"writer as admin", writer, []Role{RoleAdmin}, false},
		{"nil user", none, []Role{RoleAdmin}, false},
	}
	for _, tc := range cases {
		if got := tc.user.HasRole(tc.roles...); got != tc.want {
			t.Errorf("%s: HasRole(%v) = %v, want %v", tc.name, tc.roles, got, tc.want)
		}
	}
}

// 管理者は、ミドルウェアの設定に関わらず、編集者・執筆者用のルートも通る（保存上の役割は admin のまま）。
// ライターは編集者用のルートを通れない。
func TestRequireRoleAdminHasEveryRole(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "a1", "admin", RoleAdmin)
	insertUser(t, db, "w1", "writer", RoleWriter)
	adminSession := mustCreateSession(t, db, "a1").ID
	writerSession := mustCreateSession(t, db, "w1").ID

	e := echo.New()
	g := e.Group("/admin", AuthMiddlewareWithConfig(db, MiddlewareConfig{}))
	handler := func(c echo.Context) error {
		user, _ := UserFromContext(c)
		return c.String(http.StatusOK, string(user.Role))
	}
	g.GET("/write", handler, RequireRole(RoleWriter))
	g.GET("/review", handler, RequireRole(RoleEditor))

	for _, path := range []string{"/admin/write", "/admin/review"} {
		if rec := serve(e, request{path: path, cookie: adminSession}); rec.Code != http.StatusOK || rec.Body.String() != "admin" {
			t.Errorf("admin on %s: got %d %q, want 200 admin", path, rec.Code, rec.Body.String())
		}
	}
	if rec := serve(e, request{path: "/admin/write", cookie: writerSession}); rec.Code != http.StatusOK {
		t.Errorf("writer on writer route: status %d, want 200", rec.Code)
	}
	if rec := serve(e, request{path: "/admin/review", cookie: writerSession}); rec.Code != http.StatusForbidden {
		t.Errorf("writer on editor route: status %d, want 403", rec.Code)
	}
}

func TestCreateFirstUser(t *testing.T) {
	db := openDB(t)

	if _, err := CreateFirstUser(db, "a", "名前", "password123", RoleAdmin); !errors.Is(err, ErrInvalidUsername) {
		t.Errorf("short username: err = %v, want ErrInvalidUsername", err)
	}
	if _, err := CreateFirstUser(db, "taro", "名前", "short", RoleAdmin); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("short password: err = %v, want ErrInvalidPassword", err)
	}

	u, err := CreateFirstUser(db, " taro ", " 山田太郎 ", "password123", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateFirstUser: %v", err)
	}
	if u.Username != "taro" || u.DisplayName != "山田太郎" || u.Role != RoleAdmin {
		t.Errorf("user = %+v", u)
	}
	stored, err := GetUserByID(db, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !CheckPassword("password123", stored.PasswordHash) {
		t.Error("stored password does not match")
	}

	// 2人目は作れない（「はじめに」をやり直して管理者を乗っ取れない）。
	if _, err := CreateFirstUser(db, "mallory", "", "password123", RoleAdmin); !errors.Is(err, ErrSetupDone) {
		t.Errorf("second user: err = %v, want ErrSetupDone", err)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM users`); err != nil || n != 1 {
		t.Errorf("users = %d (err %v), want 1", n, err)
	}
}

// 古い版の取り込みで著者のユーザー（admin でない）だけがあるときも、最初の管理者は作れる。そのユーザーと同じ
// ユーザー名は ErrUsernameTaken で断る。管理者ができたあとは、2人目の管理者は作れない。
func TestCreateFirstUserWithNonAdminUsers(t *testing.T) {
	db := openDB(t)
	setupDone := func(want bool) {
		t.Helper()
		if done, err := SetupDone(db); err != nil || done != want {
			t.Errorf("SetupDone = %v, %v; want %v", done, err, want)
		}
	}
	setupDone(false)
	if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w1', 'admin', 'admin', '', 'writer')`); err != nil {
		t.Fatal(err)
	}
	setupDone(false)

	if _, err := CreateFirstUser(db, "admin", "花子", "password123", RoleAdmin); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("username of an existing writer: err = %v, want ErrUsernameTaken", err)
	}
	u, err := CreateFirstUser(db, "hanako", "花子", "password123", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateFirstUser with only a writer: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Errorf("user = %+v", u)
	}
	setupDone(true)
	if _, err := CreateFirstUser(db, "mallory", "", "password123", RoleAdmin); !errors.Is(err, ErrSetupDone) {
		t.Errorf("second admin: err = %v, want ErrSetupDone", err)
	}
	// 管理者がいれば、ユーザー名が重なっていても「はじめに」が済んでいることを伝える。
	if _, err := CreateFirstUser(db, "admin", "", "password123", RoleAdmin); !errors.Is(err, ErrSetupDone) {
		t.Errorf("second admin with a taken username: err = %v, want ErrSetupDone", err)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM users WHERE role = 'admin'`); err != nil || n != 1 {
		t.Errorf("admins = %d (err %v), want 1", n, err)
	}
}
