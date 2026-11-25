package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/labstack/echo/v4"
)

// AI 機能のマスタースイッチ（AI を使えない組織向け）。
//
// OFF にすると、AI が関わる画面とボタン（記事エディタの AI ツールバー・翻訳・画像の AI 自動生成・AI 設定・MCP 連携）を
// すべて出さず、そのルートを存在しないページと同じ 404 にし（requireAIEnabled）、記事の保存などで裏で行っていた埋め込みの
// 作成（外部の Embeddings API への送信）も止める。保存済みの AI の設定（ai_config）・記事の埋め込み（article_embeddings）・
// 画像の説明とタグ（media）は消さず、ON に戻せばそのまま使える。
//
// 優先順位は「環境変数 SORUS_AI の強制 OFF > 基本設定で保存した値（ai_enabled）> 既定の ON（今までのサイトとの互換）」。
// 環境変数は会社の IT 管理者がポリシーとして固定するためのもので、管理画面からは ON に戻せない。
const (
	// settingAIEnabled は AI 機能の ON/OFF を保存する settings のキー。値は "1"（ON）/ "0"（OFF）。
	settingAIEnabled = "ai_enabled"
	// AIEnvVar は AI 機能を強制的に OFF にする環境変数。off・false・0（大文字・小文字は問わない）で OFF に固定する。
	AIEnvVar = "SORUS_AI"
)

func init() {
	// AI 機能が OFF のサイトで、パスワード変更の通知（user_password_updated）の代わりに出す、AI アプリ（MCP）に触れない
	// 通知（renderPage）。
	flashMessages[flashPasswordUpdatedNoAI] = "パスワードを変えました。その人はログアウトされました"
	flashKeys[flashPasswordUpdatedNoAI] = flashPasswordUpdatedNoAI
}

// flashPasswordUpdatedNoAI は AI 機能が OFF のサイトで出す、パスワード変更の通知のキー。
const flashPasswordUpdatedNoAI = "user_password_updated_no_ai"

// aiEnvForcedOffValue は環境変数 SORUS_AI の値 v が、AI 機能の強制 OFF を表すかを返す。
func aiEnvForcedOffValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0":
		return true
	}
	return false
}

// aiEnvForcedOff は環境変数 SORUS_AI で AI 機能を強制的に OFF にしているかを返す。
func (s *Server) aiEnvForcedOff() bool { return aiEnvForcedOffValue(os.Getenv(AIEnvVar)) }

// aiEnabledSetting は基本設定で保存した AI 機能の ON/OFF を返す。保存していなければ ON（今までのサイトとの互換）。
// 保存した値を読めないとき（DB の障害）は、外部の AI サービスへ送らないよう OFF とみなす。
func (s *Server) aiEnabledSetting() bool {
	var v string
	err := s.db.Get(&v, `SELECT value FROM settings WHERE key = ?`, settingAIEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil {
		return false
	}
	return parseSettingBool(v, true)
}

// aiEnabled は AI 機能を使えるかを返す。環境変数 SORUS_AI の強制 OFF を、基本設定で保存した値より優先する。
func (s *Server) aiEnabled() bool {
	return !s.aiEnvForcedOff() && s.aiEnabledSetting()
}

// requireAIEnabled は AI 機能が OFF のとき、AI が関わるルートを存在しないページと同じ 404 にする。
func (s *Server) requireAIEnabled(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !s.aiEnabled() {
			return echo.ErrNotFound
		}
		return next(c)
	}
}

// formatAIEnabled は AI 機能の ON/OFF を settings に保存する値にする。
func formatAIEnabled(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// contextExecer は ExecContext で書き込める DB（*sqlx.DB）またはトランザクション（*sqlx.Tx）。
type contextExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// revokeAllMCPAccess は外部の AI アプリとの連携をすべて取り消す。MCP の API トークンを消し、OAuth 2.1 で許可した連携
// （アクセストークン・リフレッシュトークン）を取り消し済みにし、まだ交換されていない認可コードも使えなくする。
// 「AI・外部連携」を OFF にするときの「許可済みの連携も解除する」と、同期 API の RevokeAllMCP で使う。
// 取り消した API トークンと連携の数を返す。
func revokeAllMCPAccess(ctx context.Context, db contextExecer) (tokens, apps int, err error) {
	now := clockNow().UTC()
	res, err := db.ExecContext(ctx, `DELETE FROM mcp_tokens`)
	if err != nil {
		return 0, 0, fmt.Errorf("revoke mcp tokens: %w", err)
	}
	n, _ := res.RowsAffected()
	tokens = int(n)
	res, err = db.ExecContext(ctx, `UPDATE oauth_tokens SET revoked_at = ? WHERE revoked_at IS NULL`, now)
	if err != nil {
		return tokens, 0, fmt.Errorf("revoke oauth tokens: %w", err)
	}
	n, _ = res.RowsAffected()
	apps = int(n)
	if _, err := db.ExecContext(ctx, `UPDATE oauth_codes SET used_at = ? WHERE used_at IS NULL`, now); err != nil {
		return tokens, apps, fmt.Errorf("revoke oauth codes: %w", err)
	}
	return tokens, apps, nil
}
