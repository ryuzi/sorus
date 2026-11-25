package server

import (
	"path"
	"strings"

	"github.com/labstack/echo/v4"
)

// isAdminPath は管理画面（/admin 以下）へのパスかを返す。"/admin/../admin" や "//admin" のような
// 表記揺れでも判定できるよう、正規化したパスで比べる。
func isAdminPath(p string) bool {
	if p == "" {
		return false
	}
	p = path.Clean("/" + p)
	return p == "/admin" || strings.HasPrefix(p, "/admin/")
}

// PublicGuardMiddleware は公開専用モード（publicOnly が true）のとき、管理画面（/admin 以下）への
// すべての要求を 404 Not Found で遮断し、公開サーバーに管理機能の存在自体を見せない。
// 公開ページ・静的アセット・同期 API（/api/sync など）は遮断しない。publicOnly が false なら何もしない。
// ルーティングより前に判定するよう e.Pre で登録する。
func PublicGuardMiddleware(publicOnly bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if !publicOnly {
			return next
		}
		return func(c echo.Context) error {
			u := c.Request().URL
			if isAdminPath(u.Path) || isAdminPath(u.RawPath) {
				return echo.ErrNotFound
			}
			return next(c)
		}
	}
}
