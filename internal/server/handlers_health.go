package server

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// healthCheckTimeout は /healthz の各チェックの待ち時間の上限。監視ツールのタイムアウトより短くする。
const healthCheckTimeout = 2 * time.Second

// /healthz の status の値。
const (
	healthOK    = "ok"    // 配信できる
	healthError = "error" // データベースに接続できず配信できない
)

// healthResponse は GET /healthz の JSON レスポンス。認証なしで誰でも呼べるため、状態（status）だけを返し、
// ディスクの空き容量・稼働時間・検索インデックスや予約投稿の状態など、サーバーの内部の様子は出さない
// （攻撃の下調べに使われないようにする。内部の異常はサーバーのログに残す）。
type healthResponse struct {
	Status string `json:"status"`
}

// handleHealth はロードバランサー・監視ツール向けのヘルスチェック。認証不要。
//
// データベースに接続できなければ 503 と {"status":"error"} を返し、トラフィックを外させる。それ以外は 200 と
// {"status":"ok"}。検索インデックスの異常や予約投稿ワーカーの停止は、記事の配信自体はできるため ok のまま、ログに残す。
func (s *Server) handleHealth(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), healthCheckTimeout)
	defer cancel()

	c.Response().Header().Set("Cache-Control", "no-store")
	var one int
	if err := s.db.GetContext(ctx, &one, `SELECT 1`); err != nil {
		c.Logger().Errorf("healthz: database: %v", err)
		return c.JSON(http.StatusServiceUnavailable, healthResponse{Status: healthError})
	}
	if _, err := s.search.DocCount(); err != nil {
		c.Logger().Errorf("healthz: search index: %v", err)
	}
	if s.scheduler != nil && !s.scheduler.Running() {
		c.Logger().Errorf("healthz: scheduler is stopped")
	}
	return c.JSON(http.StatusOK, healthResponse{Status: healthOK})
}
