package server

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"sorus/internal/publish"
)

// GitHub Pages の公開先設定と接続テスト。
type publishGitHubConfig struct {
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Directory  string `json:"directory,omitempty"`
	Token      string `json:"token,omitempty"`
}

func (c publishGitHubConfig) client() publish.GitHubPagesConfig {
	return publish.GitHubPagesConfig{Owner: c.Owner, Repository: c.Repository, Branch: c.Branch, Directory: c.Directory, Token: c.Token, API: publishGitHubAPI}
}

// publishGitHubAPI is replaceable in tests while production uses github.com.
var publishGitHubAPI = "https://api.github.com"

type publishGitHubView struct {
	Owner, Repository, Branch, Directory string
	HasToken                             bool
}

func (s *Server) handleGitHubPublishTest(c echo.Context, cfg publishGitHubConfig, ctx context.Context, fail func(error) error) error {
	if err := publish.NewGitHubPages(cfg.client()).TestConnection(ctx); err != nil {
		return fail(err)
	}
	return c.JSON(http.StatusOK, publishTestResult{OK: true, Message: "接続できました。", Details: []string{"公開先ブランチを読み取れます。GitHub Pages の公開元設定も確認してください。"}})
}
