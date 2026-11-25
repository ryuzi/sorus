package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"sorus/internal/publish"
)

// handleCloudflareWorkersPublishTest は Cloudflare Workers（Static Assets）の「接続テスト」。API トークンで Worker の
// 有無と、アカウントの workers.dev のサブドメイン（公開 URL）を読めるかを確かめる。何も作らず、何も送らない。
func (s *Server) handleCloudflareWorkersPublishTest(c echo.Context, cfg publishCloudflareWorkersConfig, ctx context.Context, fail func(error) error) error {
	workers := publish.NewCloudflareWorkers(cfg.client())
	exists, err := workers.ScriptExists(ctx)
	if err != nil {
		return fail(err)
	}
	var details []string
	if exists {
		details = append(details, "Worker「"+cfg.Script+"」はすでにあります。初めて「サイトに反映」するときは、置き換えてよいかを確かめてから送ります。")
	} else {
		details = append(details, "Worker「"+cfg.Script+"」はまだありません。最初に「サイトに反映」したときに作ります。")
	}
	u, err := workers.URL(ctx)
	switch {
	case errors.Is(err, publish.ErrWorkersSubdomainNotFound):
		details = append(details, "Cloudflare のアカウントに workers.dev のサブドメインがまだありません。Cloudflare の画面で「Workers & Pages」を一度開いてサブドメインを決めてから反映してください。")
	case err != nil:
		return fail(err)
	default:
		details = append(details, "公開サイトの URL: "+u)
	}
	return c.JSON(http.StatusOK, publishTestResult{OK: true, Message: "接続できました。", Details: details})
}
