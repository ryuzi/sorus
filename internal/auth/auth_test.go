package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	"sorus/internal/database"
)

func openDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(":memory:")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func insertUser(t *testing.T, db *sqlx.DB, id, username string, role Role) {
	t.Helper()
	// bcrypt はテストを遅くするだけなので、ここではダミーのハッシュを使う。
	_, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, ?, ?)`,
		id, username, strings.ToUpper(username), "hash-"+id, role)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func mustCreateSession(t *testing.T, db *sqlx.DB, userID string) *Session {
	t.Helper()
	s, err := CreateSession(db, userID, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

func expireSession(t *testing.T, db *sqlx.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second), id); err != nil {
		t.Fatalf("expire session: %v", err)
	}
}

func countSessions(t *testing.T, db *sqlx.DB, id string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sessions WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHashPassword(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == pw || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash = %q, want bcrypt hash", hash)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatal(err)
	}
	if cost != bcrypt.DefaultCost {
		t.Errorf("cost = %d, want %d", cost, bcrypt.DefaultCost)
	}

	hash2, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if hash2 == hash {
		t.Error("two hashes of the same password are identical, want distinct salts")
	}

	if _, err := HashPassword(strings.Repeat("a", 73)); err == nil {
		t.Error("HashPassword accepted a 73-byte password, want error")
	}
}

func TestCheckPassword(t *testing.T) {
	hash, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		password string
		hash     string
		want     bool
	}{
		{"correct", "s3cret!", hash, true},
		{"wrong", "s3cret?", hash, false},
		{"case differs", "S3CRET!", hash, false},
		{"empty", "", hash, false},
		{"malformed hash", "s3cret!", "not-a-hash", false},
		{"plaintext as hash", "s3cret!", "s3cret!", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckPassword(tc.password, tc.hash); got != tc.want {
				t.Errorf("CheckPassword(%q) = %v, want %v", tc.password, got, tc.want)
			}
		})
	}
}

func TestCreateSession(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "u1", "alice", RoleWriter)

	before := time.Now()
	s, err := CreateSession(db, "u1", 2*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !validSessionID(s.ID) {
		t.Errorf("session id %q is not 64 lowercase hex chars", s.ID)
	}
	if s.UserID != "u1" {
		t.Errorf("UserID = %q, want u1", s.UserID)
	}
	if d := s.ExpiresAt.Sub(before); d < 2*time.Hour || d > 2*time.Hour+time.Minute {
		t.Errorf("ExpiresAt - now = %v, want about 2h", d)
	}

	var stored Session
	if err := db.Get(&stored, `SELECT id, user_id, expires_at, created_at FROM sessions WHERE id = ?`, s.ID); err != nil {
		t.Fatalf("select stored session: %v", err)
	}
	if !stored.ExpiresAt.Equal(s.ExpiresAt) || stored.UserID != "u1" {
		t.Errorf("stored session = %+v, want %+v", stored, *s)
	}

	t.Run("ids are unique", func(t *testing.T) {
		seen := map[string]bool{s.ID: true}
		for i := 0; i < 50; i++ {
			s := mustCreateSession(t, db, "u1")
			if seen[s.ID] {
				t.Fatalf("duplicate session id %q", s.ID)
			}
			seen[s.ID] = true
		}
	})
	t.Run("non-positive duration", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Minute} {
			if _, err := CreateSession(db, "u1", d); err == nil {
				t.Errorf("CreateSession(duration=%v) succeeded, want error", d)
			}
		}
	})
	t.Run("unknown user", func(t *testing.T) {
		if _, err := CreateSession(db, "no-such-user", time.Hour); err == nil {
			t.Error("CreateSession for unknown user succeeded, want error")
		}
	})
}

func TestValidateSession(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "u1", "alice", RoleEditor)

	t.Run("valid", func(t *testing.T) {
		s := mustCreateSession(t, db, "u1")
		user, err := ValidateSession(db, s.ID)
		if err != nil {
			t.Fatalf("ValidateSession: %v", err)
		}
		if user.ID != "u1" || user.Username != "alice" || user.DisplayName != "ALICE" ||
			user.Role != RoleEditor || user.PasswordHash != "hash-u1" || user.CreatedAt.IsZero() {
			t.Errorf("user = %+v, want alice/editor", user)
		}
	})
	t.Run("unknown and malformed ids", func(t *testing.T) {
		unknown := strings.Repeat("0", 64)
		for _, id := range []string{"", unknown, "not-hex", strings.Repeat("A", 64), "' OR 1=1 --"} {
			if _, err := ValidateSession(db, id); !errors.Is(err, ErrSessionNotFound) {
				t.Errorf("ValidateSession(%q) err = %v, want ErrSessionNotFound", id, err)
			}
		}
	})
	t.Run("expired", func(t *testing.T) {
		s := mustCreateSession(t, db, "u1")
		expireSession(t, db, s.ID)
		if _, err := ValidateSession(db, s.ID); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("err = %v, want ErrSessionExpired", err)
		}
		if n := countSessions(t, db, s.ID); n != 0 {
			t.Errorf("expired session rows = %d, want 0 (deleted on detection)", n)
		}
		if _, err := ValidateSession(db, s.ID); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("second validate err = %v, want ErrSessionNotFound", err)
		}
	})
	t.Run("user deleted", func(t *testing.T) {
		insertUser(t, db, "u2", "bob", RoleWriter)
		s := mustCreateSession(t, db, "u2")
		if _, err := db.Exec(`DELETE FROM users WHERE id = ?`, "u2"); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateSession(db, s.ID); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("err = %v, want ErrSessionNotFound", err)
		}
	})
}

func TestDeleteSession(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "u1", "alice", RoleWriter)
	s1 := mustCreateSession(t, db, "u1")
	s2 := mustCreateSession(t, db, "u1")

	if err := DeleteSession(db, s1.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := ValidateSession(db, s1.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("deleted session err = %v, want ErrSessionNotFound", err)
	}
	if _, err := ValidateSession(db, s2.ID); err != nil {
		t.Errorf("other session invalidated: %v", err)
	}
	if err := DeleteSession(db, s1.ID); err != nil {
		t.Errorf("DeleteSession on missing session: %v, want nil", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "u1", "alice", RoleWriter)
	expired1 := mustCreateSession(t, db, "u1")
	expired2 := mustCreateSession(t, db, "u1")
	live := mustCreateSession(t, db, "u1")
	expireSession(t, db, expired1.ID)
	expireSession(t, db, expired2.ID)

	n, err := DeleteExpiredSessions(db)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d sessions, want 2", n)
	}
	if countSessions(t, db, live.ID) != 1 {
		t.Error("live session was deleted")
	}
}

func TestNewSessionCookie(t *testing.T) {
	s := &Session{ID: strings.Repeat("ab", 32), ExpiresAt: time.Now().Add(time.Hour)}
	for _, secure := range []bool{true, false} {
		c := NewSessionCookie(SessionCookieName, s, secure)
		if c.Name != SessionCookieName || c.Value != s.ID {
			t.Errorf("cookie = %s=%s, want %s=%s", c.Name, c.Value, SessionCookieName, s.ID)
		}
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure != secure {
			t.Errorf("cookie attrs = HttpOnly:%v SameSite:%v Path:%q Secure:%v, want true/Strict/\"/\"/%v",
				c.HttpOnly, c.SameSite, c.Path, c.Secure, secure)
		}
		if c.MaxAge <= 3500 || c.MaxAge > 3600 {
			t.Errorf("MaxAge = %d, want about 3600", c.MaxAge)
		}
	}

	expired := NewSessionCookie(SessionCookieName, &Session{ID: s.ID, ExpiresAt: time.Now().Add(-time.Minute)}, true)
	if expired.MaxAge >= 0 {
		t.Errorf("expired session MaxAge = %d, want negative", expired.MaxAge)
	}
}

// newTestServer は AuthMiddleware で保護したルートを持つ Echo を返す。
// ハンドラーは Context に注入されたユーザー名を返す。
func newTestServer(db *sqlx.DB, auth echo.MiddlewareFunc) *echo.Echo {
	e := echo.New()
	whoami := func(c echo.Context) error {
		user, ok := UserFromContext(c)
		if !ok {
			return c.String(http.StatusInternalServerError, "no user in context")
		}
		return c.String(http.StatusOK, user.Username+":"+string(user.Role))
	}
	g := e.Group("/admin", auth)
	g.GET("/me", whoami)
	g.POST("/me", whoami)
	g.GET("/users", whoami, RequireRole(RoleAdmin))
	g.GET("/articles", whoami, RequireRole(RoleAdmin, RoleEditor))
	// RequireRole を AuthMiddleware なしで使った場合の挙動確認用。
	e.GET("/norole", whoami, RequireRole(RoleAdmin))
	return e
}

type request struct {
	method  string
	path    string
	cookie  string
	headers map[string]string
}

func serve(e *echo.Echo, r request) *httptest.ResponseRecorder {
	method := r.method
	if method == "" {
		method = http.MethodGet
	}
	req := httptest.NewRequest(method, r.path, nil)
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: r.cookie})
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestAuthMiddleware(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "u1", "alice", RoleWriter)
	e := newTestServer(db, AuthMiddleware(db))
	valid := mustCreateSession(t, db, "u1")
	browser := map[string]string{"Accept": "text/html,application/xhtml+xml,*/*;q=0.8"}

	t.Run("valid cookie injects user", func(t *testing.T) {
		rec := serve(e, request{path: "/admin/me", cookie: valid.ID})
		if rec.Code != http.StatusOK || rec.Body.String() != "alice:writer" {
			t.Errorf("got %d %q, want 200 alice:writer", rec.Code, rec.Body.String())
		}
	})
	t.Run("no cookie API request", func(t *testing.T) {
		rec := serve(e, request{path: "/admin/me", headers: map[string]string{"Accept": "application/json"}})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("no cookie browser navigation redirects", func(t *testing.T) {
		rec := serve(e, request{path: "/admin/me", headers: browser})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != DefaultLoginPath {
			t.Errorf("got %d Location=%q, want 303 %s", rec.Code, rec.Header().Get("Location"), DefaultLoginPath)
		}
	})
	t.Run("no cookie browser POST", func(t *testing.T) {
		rec := serve(e, request{method: http.MethodPost, path: "/admin/me", headers: browser})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("no cookie HTMX request", func(t *testing.T) {
		rec := serve(e, request{path: "/admin/me", headers: map[string]string{"HX-Request": "true", "Accept": "text/html"}})
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != DefaultLoginPath {
			t.Errorf("got %d HX-Redirect=%q, want 401 %s", rec.Code, rec.Header().Get("HX-Redirect"), DefaultLoginPath)
		}
	})
	t.Run("unknown cookie is cleared", func(t *testing.T) {
		rec := serve(e, request{path: "/admin/me", cookie: strings.Repeat("f", 64)})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		assertCookieCleared(t, rec)
	})
	t.Run("expired cookie is rejected and revoked", func(t *testing.T) {
		s := mustCreateSession(t, db, "u1")
		expireSession(t, db, s.ID)
		rec := serve(e, request{path: "/admin/me", cookie: s.ID})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		assertCookieCleared(t, rec)
		if countSessions(t, db, s.ID) != 0 {
			t.Error("expired session row still exists")
		}
	})
	t.Run("deleted session is rejected", func(t *testing.T) {
		s := mustCreateSession(t, db, "u1")
		if err := DeleteSession(db, s.ID); err != nil {
			t.Fatal(err)
		}
		rec := serve(e, request{path: "/admin/me", cookie: s.ID})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

func TestAuthMiddlewareWithConfig(t *testing.T) {
	db := openDB(t)
	e := newTestServer(db, AuthMiddlewareWithConfig(db, MiddlewareConfig{Secure: false}))

	t.Run("empty LoginPath always returns 401", func(t *testing.T) {
		rec := serve(e, request{path: "/admin/me", headers: map[string]string{"Accept": "text/html"}})
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Location") != "" {
			t.Errorf("got %d Location=%q, want 401 without redirect", rec.Code, rec.Header().Get("Location"))
		}
	})
	t.Run("insecure clear cookie", func(t *testing.T) {
		rec := serve(e, request{path: "/admin/me", cookie: strings.Repeat("f", 64)})
		c := assertCookieCleared(t, rec)
		if c != nil && c.Secure {
			t.Error("cleared cookie has Secure, want false per config")
		}
	})

	custom := newTestServer(db, AuthMiddlewareWithConfig(db, MiddlewareConfig{LoginPath: "/login"}))
	rec := serve(custom, request{path: "/admin/me", headers: map[string]string{"Accept": "text/html"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("custom LoginPath: got %d Location=%q, want 303 /login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAuthMiddlewareDatabaseError(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "u1", "alice", RoleWriter)
	s := mustCreateSession(t, db, "u1")
	e := newTestServer(db, AuthMiddleware(db))
	db.Close()

	rec := serve(e, request{path: "/admin/me", cookie: s.ID})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on database failure", rec.Code)
	}
}

func TestRequireRole(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "a1", "admin", RoleAdmin)
	insertUser(t, db, "e1", "editor", RoleEditor)
	insertUser(t, db, "w1", "writer", RoleWriter)
	e := newTestServer(db, AuthMiddleware(db))
	sessions := map[Role]string{
		RoleAdmin:  mustCreateSession(t, db, "a1").ID,
		RoleEditor: mustCreateSession(t, db, "e1").ID,
		RoleWriter: mustCreateSession(t, db, "w1").ID,
	}

	cases := []struct {
		path string
		role Role
		want int
	}{
		{"/admin/users", RoleAdmin, http.StatusOK},
		{"/admin/users", RoleEditor, http.StatusForbidden},
		{"/admin/users", RoleWriter, http.StatusForbidden},
		{"/admin/articles", RoleAdmin, http.StatusOK},
		{"/admin/articles", RoleEditor, http.StatusOK},
		{"/admin/articles", RoleWriter, http.StatusForbidden},
		{"/admin/me", RoleWriter, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.path+" as "+string(tc.role), func(t *testing.T) {
			rec := serve(e, request{path: tc.path, cookie: sessions[tc.role]})
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}

	t.Run("without AuthMiddleware", func(t *testing.T) {
		rec := serve(e, request{path: "/norole", cookie: sessions[RoleAdmin]})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("no roles denies everyone", func(t *testing.T) {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		SetUser(c, &User{ID: "a1", Role: RoleAdmin})
		err := RequireRole()(func(echo.Context) error { return nil })(c)
		if !errors.Is(err, echo.ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})
}

func TestSetSessionCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/login", nil), rec)
	s := &Session{ID: strings.Repeat("cd", 32), ExpiresAt: time.Now().Add(24 * time.Hour)}
	SetSessionCookie(c, SessionCookieName, s, true)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	got := cookies[0]
	if got.Name != SessionCookieName || got.Value != s.ID || !got.HttpOnly || !got.Secure ||
		got.SameSite != http.SameSiteStrictMode || got.Path != "/" {
		t.Errorf("Set-Cookie = %q, want HttpOnly; Secure; SameSite=Strict; Path=/", rec.Header().Get("Set-Cookie"))
	}
}

// assertCookieCleared はレスポンスがセッション Cookie を削除させていることを確認する。
func assertCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			if c.Value != "" || c.MaxAge >= 0 {
				t.Errorf("session cookie not cleared: value=%q MaxAge=%d", c.Value, c.MaxAge)
			}
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
				t.Errorf("cleared cookie attrs = %q, want HttpOnly; SameSite=Strict; Path=/", rec.Header().Get("Set-Cookie"))
			}
			return c
		}
	}
	t.Error("response does not clear the session cookie")
	return nil
}
