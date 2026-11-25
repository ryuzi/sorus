package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// newCacheTestEcho は与えたミドルウェアと、公開ページ・静的アセット・管理画面・API を模したルートを持つ Echo を返す。
func newCacheTestEcho(mws ...echo.MiddlewareFunc) *echo.Echo {
	e := echo.New()
	e.Use(mws...)
	text := func(body string) echo.HandlerFunc {
		return func(c echo.Context) error { return c.HTML(http.StatusOK, body) }
	}
	// 静的アセットと公開ページは HEAD でも確かめられるようにする。
	getHead := []string{http.MethodGet, http.MethodHead}
	e.Match(getHead, "/", text("<h1>top</h1>"))
	e.Match(getHead, "/articles/:slug", func(c echo.Context) error {
		return c.HTML(http.StatusOK, "<h1>"+c.Param("slug")+"</h1>")
	})
	e.GET("/feed.xml", func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "public, max-age=600")
		return c.Blob(http.StatusOK, "application/rss+xml", []byte("<rss/>"))
	})
	e.GET("/with-cookie", func(c echo.Context) error {
		c.SetCookie(&http.Cookie{Name: "lang", Value: "ja"})
		return c.HTML(http.StatusOK, "cookie")
	})
	e.GET("/missing", func(c echo.Context) error { return echo.ErrNotFound })
	e.Match(getHead, "/static/*", func(c echo.Context) error {
		if c.Param("*") == "missing.css" {
			return echo.ErrNotFound
		}
		return c.Blob(http.StatusOK, "text/css", []byte("body{}"))
	})
	e.Match(getHead, "/uploads/*", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "image/webp", []byte("RIFF"))
	})
	e.GET("/admin", text("dashboard"))
	e.GET("/admin/login", text("login"))
	e.GET("/api/sync", text("sync"))
	e.POST("/api/sync", text("synced"))
	e.POST("/", text("posted"))
	e.GET("/healthz", text("ok"))
	return e
}

func doRequest(e *echo.Echo, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestCacheStaticAssetsImmutable(t *testing.T) {
	e := newCacheTestEcho(StaticCacheMiddleware())
	for _, target := range []string{"/static/app.css", "/uploads/photo.webp"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := doRequest(e, method, target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: status = %d, want 200", method, target, rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != StaticCacheControl {
				t.Errorf("%s %s: Cache-Control = %q, want %q", method, target, got, StaticCacheControl)
			}
		}
	}
}

func TestCacheStaticAssetsSkipsFailuresAndOtherPaths(t *testing.T) {
	e := newCacheTestEcho(StaticCacheMiddleware())
	cases := []struct {
		method, target string
		wantCode       int
	}{
		{http.MethodGet, "/static/missing.css", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/admin", http.StatusOK},
		{http.MethodGet, "/api/sync", http.StatusOK},
		{http.MethodPost, "/api/sync", http.StatusOK},
	}
	for _, tc := range cases {
		rec := doRequest(e, tc.method, tc.target, nil)
		if rec.Code != tc.wantCode {
			t.Fatalf("%s %s: status = %d, want %d", tc.method, tc.target, rec.Code, tc.wantCode)
		}
		if got := rec.Header().Get("Cache-Control"); got != "" {
			t.Errorf("%s %s: Cache-Control = %q, want empty", tc.method, tc.target, got)
		}
	}
}

func TestCachePublicHTMLHeadersAndETag(t *testing.T) {
	e := newCacheTestEcho(PublicHTMLCacheMiddleware())
	rec := doRequest(e, http.MethodGet, "/articles/hello", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != PublicHTMLCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, PublicHTMLCacheControl)
	}
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 10 {
		t.Fatalf("ETag = %q, want weak quoted tag", etag)
	}
	if got := rec.Body.String(); got != "<h1>hello</h1>" {
		t.Errorf("body = %q, want original body", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}

	// 同じ内容なら同じ ETag、異なる内容なら異なる ETag になる。
	if again := doRequest(e, http.MethodGet, "/articles/hello", nil).Header().Get("ETag"); again != etag {
		t.Errorf("ETag changed for same content: %q vs %q", again, etag)
	}
	if other := doRequest(e, http.MethodGet, "/articles/other", nil).Header().Get("ETag"); other == etag {
		t.Errorf("ETag must differ for different content, both %q", etag)
	}

	head := doRequest(e, http.MethodHead, "/articles/hello", nil)
	if head.Code != http.StatusOK || head.Header().Get("ETag") != etag ||
		head.Header().Get("Cache-Control") != PublicHTMLCacheControl {
		t.Errorf("HEAD: status=%d ETag=%q Cache-Control=%q", head.Code, head.Header().Get("ETag"),
			head.Header().Get("Cache-Control"))
	}
}

func TestCachePublicHTMLNotModified(t *testing.T) {
	e := newCacheTestEcho(PublicHTMLCacheMiddleware())
	etag := doRequest(e, http.MethodGet, "/", nil).Header().Get("ETag")
	if etag == "" {
		t.Fatal("ETag missing")
	}
	strong := strings.TrimPrefix(etag, "W/")
	for _, inm := range []string{etag, strong, `"other", ` + etag, "*"} {
		rec := doRequest(e, http.MethodGet, "/", map[string]string{"If-None-Match": inm})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match %q: status = %d, want 304", inm, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: body = %q, want empty", inm, rec.Body.String())
		}
		if got := rec.Header().Get("ETag"); got != etag {
			t.Errorf("If-None-Match %q: ETag = %q, want %q", inm, got, etag)
		}
		if got := rec.Header().Get("Cache-Control"); got != PublicHTMLCacheControl {
			t.Errorf("If-None-Match %q: Cache-Control = %q", inm, got)
		}
	}

	rec := doRequest(e, http.MethodGet, "/", map[string]string{"If-None-Match": `W/"stale"`})
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>top</h1>" {
		t.Errorf("mismatched If-None-Match: status=%d body=%q, want 200 with body", rec.Code, rec.Body.String())
	}
}

func TestCachePublicHTMLSkipsAdminAPIAndNonGET(t *testing.T) {
	e := newCacheTestEcho(PublicHTMLCacheMiddleware())
	cases := []struct{ method, target string }{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/admin/login"},
		{http.MethodGet, "/api/sync"},
		{http.MethodPost, "/api/sync"},
		{http.MethodPost, "/"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/static/app.css"},
	}
	for _, tc := range cases {
		rec := doRequest(e, tc.method, tc.target, map[string]string{"If-None-Match": "*"})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status = %d, want 200", tc.method, tc.target, rec.Code)
		}
		if rec.Header().Get("ETag") != "" || rec.Header().Get("Cache-Control") != "" {
			t.Errorf("%s %s: ETag=%q Cache-Control=%q, want both empty", tc.method, tc.target,
				rec.Header().Get("ETag"), rec.Header().Get("Cache-Control"))
		}
	}
}

func TestCachePublicHTMLRespectsHandlerDecisions(t *testing.T) {
	e := newCacheTestEcho(PublicHTMLCacheMiddleware())

	// ハンドラーが決めた Cache-Control は上書きしないが、ETag による再検証には応じる。
	feed := doRequest(e, http.MethodGet, "/feed.xml", nil)
	if got := feed.Header().Get("Cache-Control"); got != "public, max-age=600" {
		t.Errorf("feed Cache-Control = %q, want handler value", got)
	}
	if feed.Header().Get("ETag") == "" {
		t.Error("feed ETag missing")
	}

	// Cookie を発行する応答は CDN にキャッシュさせない。
	cookie := doRequest(e, http.MethodGet, "/with-cookie", nil)
	if cookie.Header().Get("Cache-Control") != "" || cookie.Header().Get("ETag") != "" {
		t.Errorf("cookie response: Cache-Control=%q ETag=%q, want both empty",
			cookie.Header().Get("Cache-Control"), cookie.Header().Get("ETag"))
	}
	if cookie.Body.String() != "cookie" || cookie.Header().Get("Set-Cookie") == "" {
		t.Errorf("cookie response altered: body=%q Set-Cookie=%q", cookie.Body.String(), cookie.Header().Get("Set-Cookie"))
	}

	// エラー応答はキャッシュせず、エラーハンドラーの応答をそのまま返す。
	missing := doRequest(e, http.MethodGet, "/missing", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing: status = %d, want 404", missing.Code)
	}
	if missing.Header().Get("Cache-Control") != "" || missing.Header().Get("ETag") != "" {
		t.Errorf("404: Cache-Control=%q ETag=%q, want both empty",
			missing.Header().Get("Cache-Control"), missing.Header().Get("ETag"))
	}
	if !strings.Contains(missing.Body.String(), "Not Found") {
		t.Errorf("404 body = %q, want error handler output", missing.Body.String())
	}

	// Range 要求は部分応答のため対象外。
	ranged := doRequest(e, http.MethodGet, "/", map[string]string{"Range": "bytes=0-1", "If-None-Match": "*"})
	if ranged.Code != http.StatusOK || ranged.Header().Get("ETag") != "" {
		t.Errorf("Range: status=%d ETag=%q, want 200 without ETag", ranged.Code, ranged.Header().Get("ETag"))
	}
}

func TestCachePublicHTMLWithGzip(t *testing.T) {
	// 実サーバーと同じく Gzip の内側に置いても、圧縮された本文と ETag・304 が正しく扱われる。
	e := newCacheTestEcho(middleware.Gzip(), StaticCacheMiddleware(), PublicHTMLCacheMiddleware())
	rec := doRequest(e, http.MethodGet, "/", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status=%d Content-Encoding=%q, want gzip 200", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if string(body) != "<h1>top</h1>" {
		t.Errorf("decompressed body = %q", body)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("ETag missing")
	}
	nm := doRequest(e, http.MethodGet, "/", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	if nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
		t.Errorf("gzip 304: status=%d body length=%d, want 304 empty", nm.Code, nm.Body.Len())
	}
	if got := doRequest(e, http.MethodGet, "/static/app.css", nil).Header().Get("Cache-Control"); got != StaticCacheControl {
		t.Errorf("static under gzip: Cache-Control = %q", got)
	}
}

func newGuardTestEcho(publicOnly bool) *echo.Echo {
	e := newCacheTestEcho()
	e.Pre(PublicGuardMiddleware(publicOnly))
	return e
}

func TestPublicGuardBlocksAdminWhenPublicOnly(t *testing.T) {
	e := newGuardTestEcho(true)
	for _, target := range []string{"/admin", "/admin/", "/admin/login", "/admin/articles/1/edit",
		"/admin/x/../login", "//admin", "/%61dmin/login"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodHead} {
			rec := doRequest(e, method, target, nil)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 404", method, target, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "dashboard") || strings.Contains(rec.Body.String(), "login") {
				t.Errorf("%s %s: admin content leaked: %q", method, target, rec.Body.String())
			}
		}
	}
}

func TestPublicGuardAllowsPublicAndAPIWhenPublicOnly(t *testing.T) {
	e := newGuardTestEcho(true)
	cases := []struct {
		method, target, want string
	}{
		{http.MethodGet, "/", "<h1>top</h1>"},
		{http.MethodGet, "/articles/hello", "<h1>hello</h1>"},
		{http.MethodGet, "/static/app.css", "body{}"},
		{http.MethodGet, "/api/sync", "sync"},
		{http.MethodPost, "/api/sync", "synced"},
	}
	for _, tc := range cases {
		rec := doRequest(e, tc.method, tc.target, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Errorf("%s %s: status=%d body=%q, want 200 %q", tc.method, tc.target, rec.Code, rec.Body.String(), tc.want)
		}
	}
	// /admin で始まるだけの別パスは管理画面ではない（ルートが無いので 404 だが、ガードの判定対象外）。
	if isAdminPath("/administrator") || isAdminPath("/admins/x") {
		t.Error("isAdminPath must not match paths that only share the prefix")
	}
}

func TestPublicGuardDisabledAllowsAdmin(t *testing.T) {
	e := newGuardTestEcho(false)
	for target, want := range map[string]string{"/admin": "dashboard", "/admin/login": "login", "/api/sync": "sync"} {
		rec := doRequest(e, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("GET %s: status=%d body=%q, want 200 %q", target, rec.Code, rec.Body.String(), want)
		}
	}
}
