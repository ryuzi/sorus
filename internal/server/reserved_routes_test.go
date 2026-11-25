package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
)

// unreservedRouteSegments は routes のうち、パスの先頭部分がページのスラッグの予約語（article.IsReservedPageSlug）に
// 無いものを「パス（先頭部分）」の形で返す。/ と、ページ自身のルート（/:slug）と、skip のパスは調べない。
func unreservedRouteSegments(routes []*echo.Route, skip ...string) []string {
	var missing []string
	seen := map[string]bool{}
	for _, r := range routes {
		if r.Path == "/" || r.Path == "/:slug" || r.Path == "" {
			continue
		}
		if skipped := func() bool {
			for _, p := range skip {
				if r.Path == p {
					return true
				}
			}
			return false
		}(); skipped {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(r.Path, "/"), "/")
		// "/static*" のような末尾のワイルドカードは、先頭部分の名前に含めない。
		first = strings.TrimSuffix(first, "*")
		if first == "" || seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		if !article.IsReservedPageSlug(first) {
			missing = append(missing, fmt.Sprintf("%s (%s)", r.Path, first))
		}
	}
	return missing
}

// TestRegisteredRoutesAreReservedPageSlugs は、Echo に登録した固定のパスの先頭部分がすべてページのスラッグの予約語に
// 入っていることを確かめる。予約語は公開サイトの固定のパスを手で写した一覧のため、パスを足して予約語に足し忘れると、
// 同じスラッグのページがそのパスを隠す（またはパスがページを隠す）。
//
// 例えば routes() に e.GET("/foo", ...) を足して予約語に "foo" を足し忘れると、このテストは
//
//	route paths whose first segment is not a reserved page slug: [/foo (foo)]
//	add them to reservedPageSlugs in internal/article/article.go
//
// で失敗する（下の「足し忘れの例」はこの失敗を偽のルートで確かめる）。同期 API のパス（Config.SyncAPIPath）は
// 秘匿パスにでき、サーバーが別に検査するためここでは除く。
func TestRegisteredRoutesAreReservedPageSlugs(t *testing.T) {
	env := newPagesEnv(t, Config{})
	routes := env.server.echo.Routes()
	if len(routes) < 20 {
		t.Fatalf("only %d routes registered; the test is not looking at the real router", len(routes))
	}
	if missing := unreservedRouteSegments(routes, env.server.cfg.SyncAPIPath); len(missing) > 0 {
		t.Errorf("route paths whose first segment is not a reserved page slug: %v\nadd them to reservedPageSlugs in internal/article/article.go", missing)
	}

	// 足し忘れの例: 予約語に無いパスを登録すると検出する。
	fake := append(routes, &echo.Route{Method: "GET", Path: "/foo"}, &echo.Route{Method: "GET", Path: "/bar/:id"})
	got := unreservedRouteSegments(fake, env.server.cfg.SyncAPIPath)
	if strings.Join(got, ",") != "/foo (foo),/bar/:id (bar)" {
		t.Errorf("drift not detected: %v", got)
	}
}
