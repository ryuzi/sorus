package server

// 公開サーバー（同期 API で接続中）の MCP 連携を、PC の sorus の「設定」→「MCP連携」から扱う（PC 側）。公開サーバーには
// 管理画面が無いため、同期 API の MCP 連携の操作（handlers_sync_mcp.go）を呼ぶ。
//
//   - 画面の初期表示: mcpTokensData の Remote（mcpRemoteData）に、公開サーバーの MCP の URL（https://<公開ドメイン>/api/mcp-<秘匿パス>）・
//     API トークン・許可した AI アプリの一覧を入れる（公開サーバーに聞けなければ Remote.Error に理由）。
//   - GET  /admin/settings/mcp/remote                      : 同じ内容を JSON で返す（画面の再読み込み）。
//   - POST /admin/settings/mcp/remote/tokens                : API トークンを発行する（name・expires。JSON でトークンを1回だけ返す）。
//   - POST /admin/settings/mcp/remote/tokens/:id/revoke     : API トークンを失効させる。
//   - POST /admin/settings/mcp/remote/oauth/:id/revoke      : AI アプリの連携を取り消す。
//   - POST /admin/settings/mcp/remote/login-code            : 公開サーバーの同意画面で入れる確認コードを発行する。
//
// どれも JSON で応え、公開サーバーとの通信の失敗は 502（{"message": ...}）で返す（remoteFailure）。接続先とトークンはサイトで
// 1つのため、ローカルの管理者だけが扱う（server.go の routes）。公開サーバーでの権限は、接続中の招待のロールのまま。
// 利用者に見せる文（エラー・Remote.Error）は、管理画面の表示言語（language(c)）で日本語・英語を出し分ける。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
)

const (
	// remoteMCPInfoTimeout は MCP 連携の画面を開くときに、公開サーバーへ一覧を聞く制限時間。公開サーバーが応えなくても
	// 画面（このPCの MCP 連携）は出す。
	remoteMCPInfoTimeout = 5 * time.Second
	// MCPRemoteSettingsPath は公開サーバーの MCP 連携の API のパス。
	MCPRemoteSettingsPath = MCPSettingsPath + "/remote"
)

// mcpRemoteData は「MCP連携」の画面の、公開サーバーの MCP 連携の欄に渡すデータ（mcp_tokens.html の .Data.Remote）。
// JSON（GET MCPRemoteSettingsPath）でも同じ内容を返す。
type mcpRemoteData struct {
	// Connected は公開サーバーに接続済み（URL と期限内のトークンを保存している）か。false なら Error に理由がある。
	Connected bool `json:"connected"`
	// Error は公開サーバーの情報を取得できなかった理由（空なら取得できた）。
	Error string `json:"error,omitempty"`
	// ServerURL は接続中の公開サーバーの URL（例: https://example.com）。
	ServerURL string `json:"server_url"`
	// Endpoint は AI アプリに設定する公開サーバーの MCP の URL（https://<公開ドメイン>/api/mcp-<秘匿パス>）。取得できなければ空。
	Endpoint string `json:"endpoint"`
	// Role・Subject は公開サーバーでの権限（接続中の招待のロール）と名前。発行したトークンと許可した連携はこの権限で動く。
	Role    auth.Role `json:"role"`
	Subject string    `json:"subject"`
	// ShowOwner は Tokens・OAuthApps が公開サーバーの全員の分か（admin の招待）。
	ShowOwner bool `json:"show_owner"`
	// Tokens は公開サーバーの API トークン。POST MCPRemoteSettingsPath/tokens/:id/revoke で失効させる。
	Tokens []SyncMCPToken `json:"tokens"`
	// OAuthApps は公開サーバーで OAuth 2.1 で許可した AI アプリ（ChatGPT・Claude など）。
	// POST MCPRemoteSettingsPath/oauth/:id/revoke で取り消す。
	OAuthApps []SyncOAuthApp `json:"oauth_apps"`
	// OAuth は公開サーバーが OAuth 2.1（PC で表示する確認コードの同意画面）に対応しているか。確認コードは
	// POST MCPRemoteSettingsPath/login-code で発行する。
	OAuth bool `json:"oauth"`
}

// registerRemoteMCPRoutes は公開サーバーの MCP 連携の API を登録する。guard はローカルの管理者だけを通す。
func (s *Server) registerRemoteMCPRoutes(authed *echo.Group, guard echo.MiddlewareFunc) {
	authed.GET("/settings/mcp/remote", s.handleRemoteMCPInfo, guard)
	authed.POST("/settings/mcp/remote/tokens", s.handleRemoteMCPTokenCreate, guard)
	authed.POST("/settings/mcp/remote/tokens/:id/revoke", s.handleRemoteMCPTokenRevoke, guard)
	authed.POST("/settings/mcp/remote/oauth/:id/revoke", s.handleRemoteMCPOAuthRevoke, guard)
	authed.POST("/settings/mcp/remote/login-code", s.handleRemoteMCPLoginCode, guard)
}

// remoteMCPText は lang が英語なら en、それ以外なら ja を返す。
func remoteMCPText(lang i18n.Lang, ja, en string) string {
	if i18n.Normalize(string(lang)) == i18n.EN {
		return en
	}
	return ja
}

// remoteMCPUnreadable は公開サーバーの応答が読めない（古い公開サーバーなど）ときの文。
func remoteMCPUnreadable(lang i18n.Lang) string {
	return remoteMCPText(lang, "公開サーバーの応答を読み取れませんでした。公開サーバーの sorus を更新してください",
		"Could not read the public server's response. Please update sorus on the public server.")
}

// remoteMCPConfig は公開サーバーの MCP 連携に使う接続の設定を返す（requireRemote。どのロールの招待でもよい）。
// 使えない理由（未設定・期限切れなど）は lang の言葉で返す（ステータスは requireRemote と同じ）。
func (s *Server) remoteMCPConfig(lang i18n.Lang) (remoteConfig, error) {
	cfg, err := s.requireRemote(canUseSyncMCP, "MCP 連携")
	if err == nil || i18n.Normalize(string(lang)) != i18n.EN {
		return cfg, err
	}
	var he *echo.HTTPError
	if !errors.As(err, &he) {
		return cfg, err
	}
	const joinHint = "open Settings → Publishing → Connect to a publish server in sorus and paste the invite code."
	msg := ""
	switch info, perr := parseRemoteToken(cfg.Token, time.Now()); {
	case cfg.URL == "" || cfg.Token == "":
		msg = "No public server is set up. To connect, " + joinHint
	case perr != nil:
		msg = "The saved token cannot be read. Ask the site administrator for a new invite code, then " + joinHint
	case info.Expired:
		msg = "The invite has expired. Ask the site administrator for a new invite code, then " + joinHint
	default:
		msg = fmt.Sprintf("The current token (%s) cannot use MCP integration.", info.Role)
	}
	return cfg, echo.NewHTTPError(he.Code, msg)
}

// remoteMCPFailureMessage は公開サーバーとの通信の失敗を、利用者に見せる文（lang の言葉）にする。MCP 連携を知らない
// 古い公開サーバー（未対応の action）には、更新を案内する。英語では、公開サーバーが書いた理由（ServerMessage）は
// 訳せないためそのまま添え、この PC で作った説明（接続できない・ステータスからの説明）は英語の文にする。
func remoteMCPFailureMessage(err error, lang i18n.Lang) string {
	en := i18n.Normalize(string(lang)) == i18n.EN
	var re *remoteError
	if !errors.As(err, &re) {
		return remoteMCPText(lang, "公開サーバーの MCP 連携の情報を取得できませんでした",
			"Could not get the MCP integration information from the public server.")
	}
	if re.Status == http.StatusBadRequest && strings.Contains(re.Message, "未対応の action") {
		return remoteMCPText(lang, "公開サーバーの sorus が古いため、公開サーバーの MCP 連携を使えません。公開サーバーの sorus を更新してください",
			"The sorus on the public server is too old to use MCP integration. Please update sorus on the public server.")
	}
	if !en {
		return re.Message
	}
	switch {
	case re.ServerMessage:
		return "The public server returned an error: " + re.Message
	case re.Status == 0:
		return "Could not connect to the public server. Check the URL and your internet connection."
	case re.Status >= 300 && re.Status < 400:
		return "The public server URL redirected to a different URL. Set the destination URL (for example, one starting with https://)."
	case re.Status == http.StatusOK:
		return "Could not read the public server's response. Check that the URL points to a sorus public server."
	case re.Status == http.StatusUnauthorized:
		return "The public server did not accept the token. Check that it has not expired and that it belongs to this server."
	case re.Status == http.StatusForbidden:
		return "This token does not have permission for this operation."
	case re.Status == http.StatusNotFound || re.Status == http.StatusMethodNotAllowed:
		return "The sync API was not found on the public server. Check that the URL includes the sync API path (/api/sync-...)."
	}
	return fmt.Sprintf("An error occurred on the public server (%d).", re.Status)
}

// remoteMCPFailure は err をブラウザへの JSON 応答にする（remoteFailure と同じく、公開サーバーとの通信の失敗は 502）。
// 文は管理画面の表示言語にする。
func remoteMCPFailure(c echo.Context, err error) error {
	var re *remoteError
	if !errors.As(err, &re) {
		return err
	}
	if re.cause != nil {
		c.Logger().Warnf("%v", re)
	}
	return c.JSON(http.StatusBadGateway, map[string]any{"message": remoteMCPFailureMessage(err, language(c)), "remote_status": re.Status})
}

// remoteMCPEndpoint は公開サーバーの MCP の URL を返す。PC が接続している公開サーバーの URL（cfg.URL）と、公開サーバーが
// 知らせたパスから組み立てる（公開サーバーの前のプロキシの内側のアドレスを出さないため）。パスが正しくなければ、公開サーバーが
// 知らせた URL をそのまま使う。
func remoteMCPEndpoint(cfg remoteConfig, info *SyncMCPInfoResponse) string {
	if p, err := NormalizeMCPAPIPath(info.Path); err == nil {
		if u, err := url.Parse(cfg.URL); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host + p
		}
	}
	return info.URL
}

// remoteMCPData は公開サーバーの MCP 連携の欄のデータを作る。公開サーバーが未設定なら nil（欄を出さない）。
// Error は lang（管理画面の表示言語）の言葉にする。
func (s *Server) remoteMCPData(ctx context.Context, lang i18n.Lang) *mcpRemoteData {
	cfg := s.loadRemoteConfig()
	if cfg.URL == "" || cfg.Token == "" {
		return nil
	}
	d := &mcpRemoteData{ServerURL: cfg.URL, Tokens: []SyncMCPToken{}, OAuthApps: []SyncOAuthApp{}}
	if info, err := parseRemoteToken(cfg.Token, time.Now()); err == nil {
		d.Role, d.Subject = info.Role, info.Subject
	}
	if _, err := s.remoteMCPConfig(lang); err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			d.Error, _ = he.Message.(string)
		}
		if d.Error == "" {
			d.Error = err.Error()
		}
		return d
	}
	d.Connected = true
	ctx, cancel := context.WithTimeout(ctx, remoteMCPInfoTimeout)
	defer cancel()
	var out SyncMCPInfoResponse
	if err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionMCPInfo}, &out); err != nil {
		d.Error = remoteMCPFailureMessage(err, lang)
		return d
	}
	if out.Action != SyncActionMCPInfo {
		d.Error = remoteMCPUnreadable(lang)
		return d
	}
	d.Endpoint = remoteMCPEndpoint(cfg, &out)
	d.Role, d.Subject, d.ShowOwner, d.OAuth = out.Role, out.Subject, out.ShowAll, out.OAuth
	if out.Tokens != nil {
		d.Tokens = out.Tokens
	}
	if out.OAuthApps != nil {
		d.OAuthApps = out.OAuthApps
	}
	return d
}

// handleRemoteMCPInfo は公開サーバーの MCP 連携の欄のデータを JSON で返す。公開サーバーが未設定なら 409。
func (s *Server) handleRemoteMCPInfo(c echo.Context) error {
	lang := language(c)
	d := s.remoteMCPData(c.Request().Context(), lang)
	if d == nil {
		_, err := s.remoteMCPConfig(lang)
		return err
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSON(http.StatusOK, d)
}

// handleRemoteMCPTokenCreate は公開サーバーで API トークンを発行し、トークンと公開サーバーの MCP の URL を返す。
// トークンはこの応答でしか受け取れない（公開サーバーはハッシュだけを保存する）。
func (s *Server) handleRemoteMCPTokenCreate(c echo.Context) error {
	lang := language(c)
	cfg, err := s.remoteMCPConfig(lang)
	if err != nil {
		return err
	}
	name, msg := validMCPTokenName(c.FormValue("name"))
	if msg != "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, remoteMCPTokenNameMessage(lang, c.FormValue("name"), msg))
	}
	days, ok := -1, false
	for _, e := range mcpTokenExpiries {
		if e.Value == c.FormValue("expires") {
			days, ok = e.Days, true
		}
	}
	if !ok {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, remoteMCPText(lang, "有効期限を選択してください", "Choose when the token expires."))
	}
	ctx := c.Request().Context()
	var out SyncMCPIssueTokenResponse
	req := SyncRequest{Action: SyncActionMCPIssueToken, MCP: &SyncMCPRequest{Name: name, ExpiresDays: days}}
	if err := callRemote(ctx, cfg, req, &out); err != nil {
		return remoteMCPFailure(c, err)
	}
	if out.Action != SyncActionMCPIssueToken || out.Token == "" {
		return c.JSON(http.StatusBadGateway, map[string]any{"message": remoteMCPUnreadable(lang)})
	}
	endpoint := ""
	var info SyncMCPInfoResponse
	if err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionMCPInfo}, &info); err == nil && info.Action == SyncActionMCPInfo {
		endpoint = remoteMCPEndpoint(cfg, &info)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSON(http.StatusCreated, map[string]any{
		"id": out.ID, "name": out.Name, "token": out.Token, "expires_at": out.ExpiresAt, "endpoint": endpoint,
	})
}

// handleRemoteMCPTokenRevoke は公開サーバーの API トークンを失効させる。
func (s *Server) handleRemoteMCPTokenRevoke(c echo.Context) error {
	return s.remoteMCPRevoke(c, SyncActionMCPRevokeToken)
}

// handleRemoteMCPOAuthRevoke は公開サーバーで許可した AI アプリの連携を取り消す。
func (s *Server) handleRemoteMCPOAuthRevoke(c echo.Context) error {
	return s.remoteMCPRevoke(c, SyncActionMCPRevokeOAuthApp)
}

func (s *Server) remoteMCPRevoke(c echo.Context, action string) error {
	cfg, err := s.remoteMCPConfig(language(c))
	if err != nil {
		return err
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" || len(id) > 200 {
		return echo.ErrNotFound
	}
	var out SyncMCPRevokeResponse
	if err := callRemote(c.Request().Context(), cfg, SyncRequest{Action: action, MCP: &SyncMCPRequest{ID: id}}, &out); err != nil {
		return remoteMCPFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"id": id, "revoked": true})
}

// handleRemoteMCPLoginCode は公開サーバーの同意画面で入れる確認コードを発行して返す。
func (s *Server) handleRemoteMCPLoginCode(c echo.Context) error {
	lang := language(c)
	cfg, err := s.remoteMCPConfig(lang)
	if err != nil {
		return err
	}
	var out SyncMCPLoginCodeResponse
	if err := callRemote(c.Request().Context(), cfg, SyncRequest{Action: SyncActionMCPIssueLoginCode}, &out); err != nil {
		return remoteMCPFailure(c, err)
	}
	if out.Action != SyncActionMCPIssueLoginCode || out.Code == "" {
		return c.JSON(http.StatusBadGateway, map[string]any{"message": remoteMCPUnreadable(lang)})
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSON(http.StatusOK, map[string]any{
		"code": out.Code, "expires_at": out.ExpiresAt, "expires_in": int(time.Until(out.ExpiresAt).Seconds()),
	})
}

// remoteMCPTokenNameMessage は validMCPTokenName の誤り（日本語の ja）を lang の言葉にする。英語では、入力した名前 raw を
// validMCPTokenName と同じく整えて、誤りの種類を判断する（空・長すぎる・制御文字）。
func remoteMCPTokenNameMessage(lang i18n.Lang, raw, ja string) string {
	if i18n.Normalize(string(lang)) != i18n.EN {
		return ja
	}
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "Enter a name for the token (such as the tool or device that uses it)."
	case utf8.RuneCountInString(name) > maxMCPTokenNameRunes:
		return fmt.Sprintf("The token name must be %d characters or fewer.", maxMCPTokenNameRunes)
	}
	return "The token name cannot contain control characters."
}
