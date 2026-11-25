package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCookieNamesFor(t *testing.T) {
	// 1つ目のサイトとサーバーモードは今までの名前のまま（ログイン中の人をログアウトさせない）。
	if got, want := CookieNamesFor(""), (CookieNames{Session: "cms_session", CSRF: "cms_csrf", Lang: "sorus_lang"}); got != want {
		t.Errorf(`CookieNamesFor("") = %+v, want %+v`, got, want)
	}
	if got, want := CookieNamesFor("abc"), (CookieNames{Session: "cms_session_abc", CSRF: "cms_csrf_abc", Lang: "sorus_lang_abc"}); got != want {
		t.Errorf(`CookieNamesFor("abc") = %+v, want %+v`, got, want)
	}
}

// サイトの鍵を付けたセッション Cookie だけを読み、無効なら同じ名前の Cookie を消す。別のサイトの Cookie には触れない。
func TestAuthMiddlewareCookieName(t *testing.T) {
	db := openDB(t)
	insertUser(t, db, "u1", "alice", RoleWriter)
	s := mustCreateSession(t, db, "u1")
	name := CookieNamesFor("abc").Session
	e := newTestServer(db, AuthMiddlewareWithConfig(db, MiddlewareConfig{CookieName: name}))
	get := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/me", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(&http.Cookie{Name: name, Value: s.ID}); rec.Code != http.StatusOK {
		t.Errorf("keyed session cookie: %d, want 200", rec.Code)
	}
	rec := get(&http.Cookie{Name: SessionCookieName, Value: s.ID})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("another site's session cookie: %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("another site's session cookie was touched: %q", rec.Header().Values("Set-Cookie"))
	}
	rec = get(&http.Cookie{Name: name, Value: "ff"})
	cleared := rec.Result().Cookies()
	if rec.Code != http.StatusUnauthorized || len(cleared) != 1 || cleared[0].Name != name || cleared[0].MaxAge >= 0 {
		t.Errorf("invalid keyed cookie: %d, Set-Cookie %q, want 401 clearing %s", rec.Code, rec.Header().Values("Set-Cookie"), name)
	}
}
