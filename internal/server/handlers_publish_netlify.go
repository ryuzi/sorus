package server

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"sorus/internal/publish"
)

func (s *Server) handleNetlifyPublishTest(c echo.Context, cfg publishNetlifyConfig, ctx context.Context, fail func(error) error) error {
	url, err := publish.NewNetlify(cfg.client()).SiteURL(ctx)
	if err != nil {
		return fail(err)
	}
	details := []string{"Site ID と Personal Access Token で Netlify のサイトを読み取れます。"}
	if url != "" {
		details = append(details, "公開サイトの URL: "+url)
	}
	return c.JSON(http.StatusOK, publishTestResult{OK: true, Message: "接続できました。", Details: details})
}
