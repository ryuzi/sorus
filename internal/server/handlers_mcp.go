package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/mcp"
)

const (
	// MCPSettingsPath は MCP 連携（API トークン）の管理画面のパス。
	MCPSettingsPath = "/admin/settings/mcp"
	// maxMCPTokensPerUser は1人が同時に持てる API トークンの上限。
	maxMCPTokensPerUser = 20
	// maxMCPTokenNameRunes はトークン名の最大文字数。
	maxMCPTokenNameRunes = 100
	// defaultMCPTokenExpiry は発行フォームで既定に選ぶ有効期限。
	defaultMCPTokenExpiry = "90"
)

// mcpTokenExpiry は発行フォームで選べる有効期限。
type mcpTokenExpiry struct {
	Value, Label string
	// Days は有効日数。0 なら無期限。
	Days int
}

var mcpTokenExpiries = []mcpTokenExpiry{
	{"30", "30日", 30},
	{"90", "90日", 90},
	{"365", "1年", 365},
	{"never", "無期限", 0},
}

// mcpTokenRow は API トークン一覧の1行。トークン本体は保存していない（SHA-256 ハッシュのみ）。
type mcpTokenRow struct {
	ID         string     `db:"id"`
	Name       string     `db:"name"`
	UserID     string     `db:"user_id"`
	OwnerName  string     `db:"owner_name"`
	CreatedAt  time.Time  `db:"created_at"`
	LastUsedAt *time.Time `db:"last_used_at"`
	ExpiresAt  *time.Time `db:"expires_at"`
}

// Expired は有効期限を過ぎたトークンか。
func (r mcpTokenRow) Expired() bool { return r.ExpiresAt != nil && !r.ExpiresAt.After(time.Now()) }

// mcpTokensData は mcp_tokens.html に渡すデータ。
type mcpTokensData struct {
	Tokens []mcpTokenRow
	// NewToken は今発行したトークン。この応答でだけ表示し、二度と表示できない。
	NewToken, NewTokenName string
	// Endpoint は MCP サーバーの URL（Streamable HTTP）。
	Endpoint string
	// EndpointLocal は Endpoint がこの PC の中（127.0.0.1・localhost）を指すか。PC 版の sorus では、同じ PC の AI ツール
	// からしか届かない「この PC 専用の接続URL（ローカル）」として案内する（admin_ux_and_desktop_polish_spec.md §1-4）。
	EndpointLocal bool
	// EndpointPath は MCP サーバーのパス（Config.MCPAPIPath）。独自ドメインで動かすときの読み替えの例
	// （https://あなたのドメイン<EndpointPath>）に使う。
	EndpointPath string
	// ShowOwner は全ユーザーのトークンを表示している（管理者である）か。
	ShowOwner bool
	Expiries  []mcpTokenExpiry
	// FormName・FormExpiry は発行フォームの入力値。
	FormName, FormExpiry string
	// Role はトークンで操作できる権限（発行したユーザーの役割）。
	Role auth.Role
	// OAuthApps は OAuth 2.1 で許可した AI アプリ（ChatGPT・Claude など）の連携。ShowOwner なら全員の連携、
	// そうでなければ自分の連携だけ。POST /admin/settings/mcp/oauth/:id/revoke で取り消せる。
	OAuthApps []oauthAppRow
	// Remote は接続中の公開サーバー（--public-only）の MCP 連携（公開サーバーの MCP の URL・API トークン・許可した AI アプリ・
	// 確認コードの発行。handlers_remote_mcp.go の mcpRemoteData）。公開サーバーが未設定か、ローカルの管理者でなければ nil
	// （欄を出さない）。操作は MCPRemoteSettingsPath 以下の JSON の API で行う。
	Remote *mcpRemoteData
}

// TokenOrPlaceholder は設定例に埋め込むトークン。発行直後なら実際の値、そうでなければプレースホルダー。
func (d mcpTokensData) TokenOrPlaceholder() string {
	if d.NewToken != "" {
		return d.NewToken
	}
	return "<APIトークン>"
}

// handleMCPTokens は MCP 連携の設定画面（API トークンの一覧・発行フォーム・各 AI ツールの接続設定例）を返す。
func (s *Server) handleMCPTokens(c echo.Context) error {
	data, err := s.mcpTokensData(c)
	if err != nil {
		return err
	}
	return s.render(c, http.StatusOK, "mcp_tokens", pageTitle(c, "mcp"), data)
}

// mcpTokenMessage は API トークンの発行の誤りの案内を、表示言語（日本語・英語）で返す。
func mcpTokenMessage(c echo.Context, ja, en string) string {
	if language(c) == "en" {
		return en
	}
	return ja
}

// handleMCPTokenCreate は API トークンを発行し、そのトークンを一度だけ表示する画面を返す。
// トークンを再表示できないよう、リダイレクトせずにこの応答で表示し、キャッシュも禁止する。
func (s *Server) handleMCPTokenCreate(c echo.Context) error {
	user := currentUser(c)
	name := strings.Join(strings.Fields(c.FormValue("name")), " ")
	expiry := c.FormValue("expires")

	fail := func(code int, msg string) error {
		data, err := s.mcpTokensData(c)
		if err != nil {
			return err
		}
		data.FormName, data.FormExpiry = name, expiry
		return s.renderError(c, code, "mcp_tokens", pageTitle(c, "mcp"), msg, data)
	}
	if name == "" {
		return fail(http.StatusUnprocessableEntity, mcpTokenMessage(c, "トークンの名前（使うツールや端末など）を入力してください",
			"Enter a name for the token (such as the tool or device that uses it)."))
	}
	if utf8.RuneCountInString(name) > maxMCPTokenNameRunes {
		return fail(http.StatusUnprocessableEntity, mcpTokenMessage(c,
			fmt.Sprintf("トークンの名前は%d文字以内で入力してください", maxMCPTokenNameRunes),
			fmt.Sprintf("The token name must be %d characters or fewer.", maxMCPTokenNameRunes)))
	}
	var days int
	valid := false
	for _, e := range mcpTokenExpiries {
		if e.Value == expiry {
			days, valid = e.Days, true
		}
	}
	if !valid {
		return fail(http.StatusUnprocessableEntity, mcpTokenMessage(c, "有効期限を選択してください", "Choose when the token expires."))
	}
	var count int
	if err := s.db.Get(&count, `SELECT COUNT(*) FROM mcp_tokens WHERE user_id = ?`, user.ID); err != nil {
		return err
	}
	if count >= maxMCPTokensPerUser {
		return fail(http.StatusConflict, mcpTokenMessage(c,
			fmt.Sprintf("APIトークンは1人%d個までです。使っていないトークンを失効させてから発行してください", maxMCPTokensPerUser),
			fmt.Sprintf("You can have up to %d API tokens. Revoke a token you no longer use, then issue a new one.", maxMCPTokensPerUser)))
	}

	var expiresAt *time.Time
	if days > 0 {
		t := time.Now().UTC().AddDate(0, 0, days)
		expiresAt = &t
	}
	raw, _, err := mcp.IssueToken(c.Request().Context(), s.db, user.ID, name, expiresAt)
	if err != nil {
		return err
	}
	data, err := s.mcpTokensData(c)
	if err != nil {
		return err
	}
	data.NewToken, data.NewTokenName = raw, name
	c.Response().Header().Set("Cache-Control", "no-store")
	return s.renderPage(c, http.StatusCreated, "mcp_tokens", pageTitle(c, "mcp"), data, "token_created", false)
}

// handleMCPTokenRevoke は API トークンを失効（削除）させる。自分のトークンのほか、管理者は全員のトークンを失効できる。
func (s *Server) handleMCPTokenRevoke(c echo.Context) error {
	user := currentUser(c)
	res, err := s.db.Exec(`DELETE FROM mcp_tokens WHERE id = ? AND (user_id = ? OR ?)`,
		c.Param("id"), user.ID, user.HasRole(auth.RoleAdmin))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return echo.ErrNotFound
	}
	return c.Redirect(http.StatusSeeOther, MCPSettingsPath+"?flash=token_revoked")
}

// mcpEndpointURL は AI ツールに設定する MCP サーバーの URL を返す。MCP サーバーは公開サイト（Config.BaseURL。
// 静的な公開先には MCP サーバーが無く、公開専用モードのサーバーの MCP サーバーは秘匿したパスにある）ではなく、いま
// 管理画面を開いているこの sorus で動いているため、要求が届いたオリジン（デスクトップモードの複数サイトなら、サイトごとの
// ポート）と Config.MCPAPIPath から組み立てる。公開サーバーの MCP の URL は mcpTokensData.Remote.Endpoint。
func (s *Server) mcpEndpointURL(c echo.Context) string {
	return s.requestOrigin(c) + s.cfg.MCPAPIPath
}

// mcpTokensData は画面に出すトークンの一覧を読む。管理者は全員のトークン、それ以外は自分のトークンだけ。
func (s *Server) mcpTokensData(c echo.Context) (mcpTokensData, error) {
	user := currentUser(c)
	data := mcpTokensData{
		Endpoint:     s.mcpEndpointURL(c),
		EndpointPath: s.cfg.MCPAPIPath,
		ShowOwner:    user.HasRole(auth.RoleAdmin),
		Expiries:     mcpTokenExpiries,
		FormExpiry:   defaultMCPTokenExpiry,
		Role:         user.Role,
		Tokens:       []mcpTokenRow{},
	}
	if u, err := url.Parse(data.Endpoint); err == nil {
		data.EndpointLocal = isLoopbackHost(u.Hostname())
	}
	err := s.db.Select(&data.Tokens, `SELECT t.id, t.name, t.user_id, t.created_at, t.last_used_at, t.expires_at,
			COALESCE(NULLIF(u.display_name, ''), u.username, '') AS owner_name
		FROM mcp_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.user_id = ? OR ?
		ORDER BY julianday(t.created_at) DESC, t.id`, user.ID, data.ShowOwner)
	if err != nil {
		return data, err
	}
	if data.OAuthApps, err = s.oauthApps(user.ID, data.ShowOwner); err != nil {
		return data, err
	}
	// 公開サーバーの MCP 連携は、接続先とトークンがサイトで1つのため、ローカルの管理者にだけ出す。
	if user.HasRole(auth.RoleAdmin) {
		data.Remote = s.remoteMCPData(c.Request().Context(), language(c))
	}
	return data, nil
}
