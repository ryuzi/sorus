package server

import (
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"
)

// 「設定」→「サイト運用」（管理者だけ）。公開サーバーへすぐに反映される運用の機能（公開サイトの閲覧制限
// （Basic 認証）とメンテナンス表示）を、接続・参加・招待の「公開サーバー」画面から分けて置く。静的サイトの公開先には
// 適用されない。確認・切り替えは既存の API（/admin/remote/site-access・/admin/remote/maintenance*）をそのまま使い、
// この画面はそれらを並べるだけにする。表示できるか（CanManageSiteAccess・CanManageMaintenance）は
// 「公開サーバー」画面と同じ remoteSettingsData で決める。
const operationsPagePath = "/admin/settings/operations"

func init() {
	if !slices.Contains(pageNames, "settings_operations") {
		pageNames = append(pageNames, "settings_operations")
	}
}

// registerOperationsRoutes は「サイト運用」の画面を登録する。
func (s *Server) registerOperationsRoutes(authed *echo.Group, adminOnly echo.MiddlewareFunc) {
	authed.GET("/settings/operations", s.handleOperationsPage, adminOnly)
}

// handleOperationsPage は「サイト運用」の画面を返す。公開サーバーに接続していなければ、画面は接続の案内だけを出す
// （remoteSettingsData の Connected が false）。
func (s *Server) handleOperationsPage(c echo.Context) error {
	title := translateOr(string(language(c)), "operations_title")
	return s.render(c, http.StatusOK, "settings_operations", title, s.remoteSettingsData(c, s.loadRemoteConfig()))
}
