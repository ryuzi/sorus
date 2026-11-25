package server

// 同期 API の MCP 連携の操作（公開サーバー側）。公開専用モードの公開サーバーには管理画面が無いため、接続した PC の sorus の
// 「設定」→「MCP連携」から、公開サーバーの MCP サーバー（秘匿したパス）の URL、API トークンの発行・一覧・失効、
// OAuth 2.1 で許可した AI アプリの一覧・取り消し、同意画面で本人を確かめる確認コードの発行を行う（handlers_remote_mcp.go）。
//
// 公開サーバーでのユーザーは、同期 API の招待（トークン）ごとに作られるユーザー（syncUser。sync-tok-<jti>）で、権限は
// 招待のロール（writer・editor・admin）。発行した API トークンと許可した連携は、このユーザーの権限で MCP サーバーを使う。
// 招待を取り消す・期限が切れると、そのユーザーのトークンと連携も使えなくなる（syncAwareTokenStore）。
// 一覧・失効・取り消しは、admin の招待なら公開サーバーの全員の分、それ以外は自分の分だけを扱う（PC の画面と同じ分担）。

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/mcp"
)

// 同期 API の MCP 連携のアクション。どれも有効な招待（writer・editor・admin）なら使える。
const (
	// SyncActionMCPInfo は MCP サーバーのパス・URL と、API トークン・許可した AI アプリの一覧を返す。
	SyncActionMCPInfo = "mcp_info"
	// SyncActionMCPIssueToken は API トークンを発行する（MCP.Name・MCP.ExpiresDays）。トークンはこの応答でだけ返す。
	SyncActionMCPIssueToken = "mcp_issue_token"
	// SyncActionMCPRevokeToken は API トークン（MCP.ID）を失効させる。
	SyncActionMCPRevokeToken = "mcp_revoke_token"
	// SyncActionMCPRevokeOAuthApp は OAuth 2.1 で許可した AI アプリの連携（MCP.ID）を取り消す。
	SyncActionMCPRevokeOAuthApp = "mcp_revoke_oauth_app"
	// SyncActionMCPIssueLoginCode は公開サーバーの OAuth 2.1 の同意画面で本人を確かめる確認コードを発行する。
	SyncActionMCPIssueLoginCode = "mcp_issue_login_code"
)

const (
	// maxSyncMCPTokenDays は mcp_issue_token で指定できる有効日数の上限。
	maxSyncMCPTokenDays = 3650
	// maxSyncMCPListed は mcp_info が返す API トークン・連携のそれぞれの上限。
	maxSyncMCPListed = 500
)

// canUseSyncMCP は MCP 連携の操作を行えるロールか（有効な招待ならどのロールでも。権限は招待のロールのまま）。
func canUseSyncMCP(role auth.Role) bool { return auth.CanSubmitDraft(role) }

// SyncMCPRequest は MCP 連携の操作の内容（SyncRequest.MCP）。
type SyncMCPRequest struct {
	// Name は mcp_issue_token で発行するトークンの名前（使うツールや端末など。100文字以内）。
	Name string `json:"name,omitempty"`
	// ExpiresDays は mcp_issue_token で発行するトークンの有効日数。0 なら無期限。
	ExpiresDays int `json:"expires_days,omitempty"`
	// ID は mcp_revoke_token で失効させるトークン、mcp_revoke_oauth_app で取り消す連携の ID。
	ID string `json:"id,omitempty"`
}

// SyncMCPToken は公開サーバーの API トークン1つ（トークン本体は返さない）。
type SyncMCPToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OwnerName  string     `json:"owner_name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	// Mine はこの要求の招待のユーザーのトークンか。
	Mine bool `json:"mine"`
}

// Expired は有効期限を過ぎたトークンか。
func (t SyncMCPToken) Expired() bool { return t.ExpiresAt != nil && !t.ExpiresAt.After(time.Now()) }

// SyncOAuthApp は公開サーバーで OAuth 2.1 で許可した AI アプリの連携1つ。
type SyncOAuthApp struct {
	ID         string     `json:"id"`
	ClientName string     `json:"client_name"`
	OwnerName  string     `json:"owner_name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// ExpiresAt は使わないままだと連携が切れる日時（リフレッシュトークンの期限）。
	ExpiresAt time.Time `json:"expires_at"`
	Mine      bool      `json:"mine"`
}

// SyncMCPInfoResponse は mcp_info の応答。
type SyncMCPInfoResponse struct {
	Action string `json:"action"`
	// Path は MCP サーバーのパス（公開専用モードでは /api/mcp-<秘匿パス>）。URL は公開サーバーが受けた要求のオリジンから
	// 組み立てた MCP サーバーの URL（PC は自分が接続している公開サーバーの URL と Path から組み立て直してよい）。
	Path string `json:"path"`
	URL  string `json:"url"`
	// Role・Subject はこの要求の招待のロールと名前（公開サーバーでの MCP の権限と表示名）。
	Role    auth.Role `json:"role"`
	Subject string    `json:"subject"`
	// ShowAll は Tokens・OAuthApps が公開サーバーの全員の分か（admin の招待）。
	ShowAll   bool           `json:"show_all"`
	Tokens    []SyncMCPToken `json:"tokens"`
	OAuthApps []SyncOAuthApp `json:"oauth_apps"`
	// OAuth は OAuth 2.1（確認コードの同意画面）に対応しているか。
	OAuth bool `json:"oauth"`
}

// SyncMCPIssueTokenResponse は mcp_issue_token の応答。Token はこの応答でだけ返し、公開サーバーはハッシュだけを保存する。
type SyncMCPIssueTokenResponse struct {
	Action    string     `json:"action"`
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// SyncMCPRevokeResponse は mcp_revoke_token・mcp_revoke_oauth_app の応答。
type SyncMCPRevokeResponse struct {
	Action string `json:"action"`
	ID     string `json:"id"`
}

// SyncMCPLoginCodeResponse は mcp_issue_login_code の応答。
type SyncMCPLoginCodeResponse struct {
	Action string `json:"action"`
	// Code は確認コード（XXXX-XXXX）。ExpiresAt まで1回だけ使える。同じ招待で発行し直すと、前のコードは使えなくなる。
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	// AuthorizeURL は同意画面の URL（参考。AI アプリが開く URL にはクエリが付く）。
	AuthorizeURL string `json:"authorize_url"`
}

// syncMCP は MCP 連携のアクションを処理する。syncHandler から呼ばれ、権限（canUseSyncMCP）は確かめ済み。
func (s *Server) syncMCP(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	// AI 機能を OFF にしたサイトでは、公開サーバーの MCP 連携（トークンの発行・一覧など）も無いものとして扱う。
	if !s.aiEnabled() {
		return echo.ErrNotFound
	}
	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	all := claims.Role == auth.RoleAdmin
	in := req.MCP
	if in == nil {
		in = &SyncMCPRequest{}
	}
	switch req.Action {
	case SyncActionMCPInfo:
		return s.syncMCPInfo(c, claims, userID, all)
	case SyncActionMCPIssueToken:
		return s.syncMCPIssueToken(c, userID, in)
	case SyncActionMCPRevokeToken:
		res, err := s.db.ExecContext(ctx, `DELETE FROM mcp_tokens WHERE id = ? AND (user_id = ? OR ?)`, in.ID, userID, all)
		return syncMCPRevoked(c, res, err, SyncActionMCPRevokeToken, in.ID, "API トークン")
	case SyncActionMCPRevokeOAuthApp:
		res, err := s.db.ExecContext(ctx, `UPDATE oauth_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL AND (user_id = ? OR ?)`,
			clockNow().UTC(), in.ID, userID, all)
		return syncMCPRevoked(c, res, err, SyncActionMCPRevokeOAuthApp, in.ID, "AI アプリの連携")
	default: // SyncActionMCPIssueLoginCode
		code, expires, err := s.oauthLogins.issueCode(userID)
		if errors.Is(err, errOAuthLoginStoreFull) {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "確認コードの発行が混み合っています。しばらく待ってからもう一度お試しください")
		}
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, SyncMCPLoginCodeResponse{Action: SyncActionMCPIssueLoginCode, Code: code, ExpiresAt: expires.UTC(),
			AuthorizeURL: s.advertisedOrigin(c) + OAuthAuthorizePath})
	}
}

// syncMCPRevoked は失効・取り消しの結果を返す。対象が無い（ID の誤り・他人のもの・済み）なら 404。
func syncMCPRevoked(c echo.Context, res sql.Result, err error, action, id, label string) error {
	if err != nil {
		return fmt.Errorf("server: sync %s: %w", action, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("server: sync %s: %w", action, err)
	} else if n == 0 {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("公開サーバーに、この%sが見つかりません（失効・取り消し済みか、ほかの人のものです）", label))
	}
	return c.JSON(http.StatusOK, SyncMCPRevokeResponse{Action: action, ID: id})
}

// syncMCPInfo は mcp_info に応える。
func (s *Server) syncMCPInfo(c echo.Context, claims *auth.Claims, userID string, all bool) error {
	ctx := c.Request().Context()
	out := SyncMCPInfoResponse{
		Action: SyncActionMCPInfo, Path: s.cfg.MCPAPIPath, URL: s.advertisedOrigin(c) + s.cfg.MCPAPIPath,
		Role: claims.Role, Subject: strings.TrimSpace(claims.Subject), ShowAll: all, OAuth: true,
		Tokens: []SyncMCPToken{}, OAuthApps: []SyncOAuthApp{},
	}
	var tokens []mcpTokenRow
	if err := s.db.SelectContext(ctx, &tokens, `SELECT t.id, t.name, t.user_id, t.created_at, t.last_used_at, t.expires_at,
			COALESCE(NULLIF(u.display_name, ''), u.username, '') AS owner_name
		FROM mcp_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.user_id = ? OR ?
		ORDER BY julianday(t.created_at) DESC, t.id LIMIT ?`, userID, all, maxSyncMCPListed); err != nil {
		return fmt.Errorf("server: sync mcp_info: %w", err)
	}
	for _, t := range tokens {
		out.Tokens = append(out.Tokens, SyncMCPToken{ID: t.ID, Name: t.Name, OwnerName: t.OwnerName, CreatedAt: t.CreatedAt,
			LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt, Mine: t.UserID == userID})
	}
	apps, err := s.oauthAppsFor(userID, all, maxSyncMCPListed)
	if err != nil {
		return fmt.Errorf("server: sync mcp_info: %w", err)
	}
	for _, a := range apps {
		out.OAuthApps = append(out.OAuthApps, SyncOAuthApp{ID: a.ID, ClientName: a.ClientName, OwnerName: a.OwnerName,
			CreatedAt: a.CreatedAt, LastUsedAt: a.LastUsedAt, ExpiresAt: a.ExpiresAt, Mine: a.UserID == userID})
	}
	return c.JSON(http.StatusOK, out)
}

// validMCPTokenName は API トークンの名前を整えて検査する。正しくなければ利用者に見せる理由を返す。
func validMCPTokenName(raw string) (string, string) {
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", "トークンの名前（使うツールや端末など）を指定してください"
	case utf8.RuneCountInString(name) > maxMCPTokenNameRunes:
		return "", fmt.Sprintf("トークンの名前は%d文字以内で指定してください", maxMCPTokenNameRunes)
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", "トークンの名前に制御文字は使えません"
	}
	return name, ""
}

// syncMCPIssueToken は mcp_issue_token に応える。
func (s *Server) syncMCPIssueToken(c echo.Context, userID string, in *SyncMCPRequest) error {
	name, msg := validMCPTokenName(in.Name)
	if msg != "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, msg)
	}
	if in.ExpiresDays < 0 || in.ExpiresDays > maxSyncMCPTokenDays {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("有効日数は0（無期限）から%d日までで指定してください", maxSyncMCPTokenDays))
	}
	ctx := c.Request().Context()
	var count int
	if err := s.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM mcp_tokens WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("server: sync mcp_issue_token: %w", err)
	}
	if count >= maxMCPTokensPerUser {
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("公開サーバーの API トークンは1人%d個までです。使っていないトークンを失効させてから発行してください", maxMCPTokensPerUser))
	}
	var expiresAt *time.Time
	if in.ExpiresDays > 0 {
		t := clockNow().UTC().AddDate(0, 0, in.ExpiresDays)
		expiresAt = &t
	}
	raw, id, err := mcp.IssueToken(ctx, s.db, userID, name, expiresAt)
	if err != nil {
		return err
	}
	c.Logger().Infof("sync api: issued an MCP token %s for %s", id, userID)
	return c.JSON(http.StatusOK, SyncMCPIssueTokenResponse{Action: SyncActionMCPIssueToken, ID: id, Name: name, Token: raw, ExpiresAt: expiresAt})
}
