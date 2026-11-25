package server

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"sorus/static"
)

func init() {
	pageNames = append(pageNames, "offline")
}

func (s *Server) handleOffline(c echo.Context) error {
	title := localizedMessage(c, "offline_title")
	return s.render(c, http.StatusOK, "offline", title, nil)
}

// handleManifest は PWA 用の Web App Manifest を返す。
func (s *Server) handleManifest(c echo.Context) error {
	data, err := static.FS.ReadFile("manifest.json")
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "manifest not found")
	}
	return c.Blob(http.StatusOK, "application/manifest+json", data)
}

// handleServiceWorker は PWA 用の Service Worker スクリプトを返す。
// Service-Worker-Allowed ヘッダーを設定してルートスコープでの制御を許可する。
func (s *Server) handleServiceWorker(c echo.Context) error {
	data, err := static.FS.ReadFile("sw.js")
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "service worker not found")
	}
	c.Response().Header().Set("Service-Worker-Allowed", "/")
	c.Response().Header().Set("Cache-Control", "no-cache")
	return c.Blob(http.StatusOK, "application/javascript", data)
}
