package server

// OAuth 2.1（PKCE + Dynamic Client Registration）の認可サーバー。ChatGPT・Claude などの AI アプリが、API トークンを
// 手で貼り付けなくても、ブラウザでこの sorus にログインして「許可する」を押すだけで MCP サーバー（Config.MCPAPIPath）を
// 使えるようにする。
//
//   - GET /.well-known/oauth-protected-resource（RFC 9728）: MCP サーバーと、その認可サーバー（この sorus）を知らせる。
//     MCP サーバーを秘匿したパスで動かすときは、共通のこのパスには置かず、パスを挿入した
//     /.well-known/oauth-protected-resource/<秘匿パス> だけに置く（MCP サーバーの 401 の WWW-Authenticate で案内する）。
//   - GET /.well-known/oauth-authorization-server（RFC 8414）: 各エンドポイントと対応する方式を知らせる。
//   - POST /oauth/register（RFC 7591）: AI アプリ（公開クライアント）を登録し、client_id を発行する。
//   - GET/POST /oauth/authorize: ログインしたユーザーに同意画面を出し、許可されたら認可コードを発行してリダイレクトする。
//     公開専用モード（管理画面のログインが無い）では、接続した PC で発行した確認コードで本人を確かめる
//     （handlers_oauth_public.go）。
//   - POST /oauth/token: 認可コード（PKCE の検証付き）とリフレッシュトークン（ローテーション）をトークンに引き換える。
//
// 登録・トークン・同意画面への POST は、総当たりを抑えるため送り元ごとと全体の回数を制限する（oauth_rate_limit.go）。
// 保存と検証は mcp.OAuthStore が行う。

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"sorus/internal/auth"
	"sorus/internal/mcp"
)

const (
	// OAuthAuthorizePath・OAuthTokenPath・OAuthRegisterPath は OAuth 2.1 のエンドポイント。
	OAuthAuthorizePath = "/oauth/authorize"
	OAuthTokenPath     = "/oauth/token"
	OAuthRegisterPath  = "/oauth/register"
	// oauthAuthServerMetadataPath は RFC 8414 の認可サーバーのメタデータのパス。
	oauthAuthServerMetadataPath = "/.well-known/oauth-authorization-server"
	// maxOAuthRegisterBytes はクライアント登録の本文の上限。
	maxOAuthRegisterBytes = 64 << 10
	// maxOAuthStateBytes は state の長さの上限。
	maxOAuthStateBytes = 1024
	// maxOAuthResourceBytes は resource の長さの上限。
	maxOAuthResourceBytes = 2000
)

func init() {
	pageNames = append(pageNames, "oauth_authorize")
}

// oauthAuthorizeData は oauth_authorize.html（同意画面）に渡すデータ。
type oauthAuthorizeData struct {
	// Error は同意画面を出せないとき（未登録の AI アプリ・登録と違うリダイレクト先）の理由。空なら同意画面を出す。
	Error string
	// ClientName は AI アプリの名前（登録時の client_name。無ければ client_id）。RedirectHost は許可した後に戻る先のホスト。
	ClientName   string
	RedirectHost string
	// UserName・Role は許可するユーザーの名前と権限（AI アプリはこの権限で操作する）。
	UserName string
	Role     auth.Role
	// Request は許可・拒否のフォームで送り返す認可要求のパラメーター。
	Request oauthAuthorizeRequest

	// 以下は公開専用モード（handlers_oauth_public.go）だけで使う。
	// LoginRequired は同意画面の前に、PC の sorus で表示した確認コードを入れる画面を出すか（UserName・Role は空）。
	// LoginError は確認コードが正しくなかったときなどの案内。
	LoginRequired bool
	LoginError    string
	// Ticket は確認コードで本人を確かめた結果。同意画面のフォームで送り返す（この認可要求だけに使え、1回限り）。
	Ticket string
	// HideSiteName は画面にサイト名を出さないか（閲覧制限を有効にした公開専用モードのサーバー）。
	HideSiteName bool
}

// oauthAuthorizeRequest は認可要求のパラメーター。
type oauthAuthorizeRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	State               string
	Scope               string
	Resource            string
}

// readOAuthAuthorizeRequest は get（クエリまたはフォーム）から認可要求を読む。
func readOAuthAuthorizeRequest(get func(string) string) oauthAuthorizeRequest {
	return oauthAuthorizeRequest{
		ResponseType:        get("response_type"),
		ClientID:            get("client_id"),
		RedirectURI:         get("redirect_uri"),
		CodeChallenge:       get("code_challenge"),
		CodeChallengeMethod: get("code_challenge_method"),
		State:               get("state"),
		Scope:               get("scope"),
		Resource:            get("resource"),
	}
}

// query は認可要求を GET /oauth/authorize のクエリに戻す（ログイン後に戻ってくるため）。
func (r oauthAuthorizeRequest) query() url.Values {
	q := url.Values{}
	for k, v := range map[string]string{
		"response_type": r.ResponseType, "client_id": r.ClientID, "redirect_uri": r.RedirectURI,
		"code_challenge": r.CodeChallenge, "code_challenge_method": r.CodeChallengeMethod,
		"state": r.State, "scope": r.Scope, "resource": r.Resource,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

// registerOAuthRoutes は OAuth 2.1 のエンドポイントを登録する。認可サーバーのメタデータ・登録・トークンは、
// ブラウザで動く MCP クライアントからも呼べるよう CORS を許可する（Cookie は使わず、応答に利用者ごとの秘密は含まない）。
// 同意画面は管理画面と同じセッションでログインを確かめ、許可・拒否のフォームは専用の CSRF トークンで守る。
func (s *Server) registerOAuthRoutes(e *echo.Echo) {
	cors := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()
			h.Set("Access-Control-Allow-Origin", "*")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
			h.Set("Access-Control-Max-Age", "86400")
			if c.Request().Method == http.MethodOptions {
				return c.NoContent(http.StatusNoContent)
			}
			return next(c)
		}
	}
	// AI 機能を OFF にしたサイト（aiEnabled）では、OAuth のエンドポイントもすべて存在しないページと同じ 404 にする。
	ai := s.requireAIEnabled
	metadata := []string{http.MethodGet, http.MethodHead, http.MethodOptions}
	// 既定のパス（/api/mcp）では、共通のパスと、RFC 9728 のパスの挿入（/.well-known/oauth-protected-resource/api/mcp）で
	// 探すクライアントの両方に同じ内容を返す。秘匿したパスでは、共通のパスに秘匿パスを載せないよう、パスを挿入した形だけに
	// 置く（共通のパスと /.well-known/oauth-protected-resource/api/mcp は存在しないページと同じ 404）。
	if !s.mcpSecretPath() {
		e.Match(metadata, mcp.ProtectedResourceMetadataPath, s.handleOAuthProtectedResource, ai, cors)
	}
	e.Match(metadata, mcp.ProtectedResourceMetadataPathFor(s.cfg.MCPAPIPath), s.handleOAuthProtectedResource, ai, cors)
	if !s.mcpSecretPath() {
		e.Match(metadata, mcp.ProtectedResourceMetadataPath+mcp.DefaultBasePath, s.handleOAuthProtectedResource, ai, cors)
	}
	e.Match(metadata, oauthAuthServerMetadataPath, s.handleOAuthAuthServerMetadata, ai, cors)
	e.Match([]string{http.MethodPost, http.MethodOptions}, OAuthRegisterPath, s.handleOAuthRegister, ai, cors)
	e.Match([]string{http.MethodPost, http.MethodOptions}, OAuthTokenPath, s.handleOAuthToken, ai, cors)

	csrf := middleware.CSRFWithConfig(middleware.CSRFConfig{
		TokenLookup:    "form:" + CSRFFormField,
		ContextKey:     csrfContextKey,
		CookieName:     s.cookies.CSRF + "_oauth",
		CookiePath:     OAuthAuthorizePath,
		CookieSecure:   s.cfg.SecureCookie,
		CookieHTTPOnly: true,
		CookieSameSite: http.SameSiteStrictMode,
		ErrorHandler: func(err error, c echo.Context) error {
			return echo.NewHTTPError(http.StatusForbidden, "invalid or missing csrf token")
		},
	})
	if s.cfg.PublicOnly {
		e.GET(OAuthAuthorizePath, s.handlePublicOAuthAuthorize, ai, csrf)
		e.POST(OAuthAuthorizePath, s.handlePublicOAuthAuthorizePost, ai, csrf)
		return
	}
	e.GET(OAuthAuthorizePath, s.handleOAuthAuthorize, ai, csrf)
	e.POST(OAuthAuthorizePath, s.handleOAuthAuthorizeDecision, ai, csrf)
}

// oauthNoStore は OAuth の応答をキャッシュさせない（RFC 6749 5.1）。
func oauthNoStore(c echo.Context) {
	h := c.Response().Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
}

// oauthError は OAuth のエラー応答（JSON）を返す。
func oauthError(c echo.Context, status int, code, desc string) error {
	oauthNoStore(c)
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	return c.JSON(status, body)
}

// advertisedOrigin は OAuth のメタデータや招待コードに載せる、この sorus のオリジンを返す。公開 URL（BaseURL）の
// ホスト名が要求の Host と同じなら、そのスキーム・ポートで組み立てる（mcp.TrustedProxies.PreferredOrigin。クライアントが
// 書いた X-Forwarded-Proto などに左右されない）。違えば要求が届いたオリジン（BaseURL は静的な公開先のこともあるため）。
// スキームは TLS か Config.TrustedProxies のプロキシの転送ヘッダーだけで決める。ALLOWED_HOSTS（Config.AllowedHosts）を
// 指定したサイトで、一覧に無い Host で届いた要求には、その Host ではなく BaseURL（ホストが一覧にあるとき）か一覧の
// 先頭のホストを返す（偽の Host を OAuth のメタデータや招待コードに載せない）。デスクトップモードの BaseURL はいつも
// 静的な公開先のため使わない。
func (s *Server) advertisedOrigin(c echo.Context) string {
	base := s.getBaseURL()
	if s.cfg.Desktop {
		base = ""
	}
	return s.proxies.PreferredOrigin(c.Request(), base, s.allowedHosts)
}

// handleOAuthProtectedResource は保護されたリソース（MCP サーバー）のメタデータ（RFC 9728）を返す。
func (s *Server) handleOAuthProtectedResource(c echo.Context) error {
	origin := s.advertisedOrigin(c)
	oauthNoStore(c)
	return c.JSON(http.StatusOK, map[string]any{
		"resource":                 origin + s.cfg.MCPAPIPath,
		"authorization_servers":    []string{origin},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{mcp.OAuthScope},
		"resource_name":            s.getSiteName(),
	})
}

// handleOAuthAuthServerMetadata は認可サーバーのメタデータ（RFC 8414）を返す。issuer は advertisedOrigin。
func (s *Server) handleOAuthAuthServerMetadata(c echo.Context) error {
	origin := s.advertisedOrigin(c)
	oauthNoStore(c)
	return c.JSON(http.StatusOK, map[string]any{
		"issuer":                                origin,
		"authorization_endpoint":                origin + OAuthAuthorizePath,
		"token_endpoint":                        origin + OAuthTokenPath,
		"registration_endpoint":                 origin + OAuthRegisterPath,
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{mcp.OAuthCodeChallengeMethod},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{mcp.OAuthScope},
	})
}

// cleanOAuthClientName はクライアント名から制御文字を除き、前後の空白を詰めて上限の文字数に切り詰める。
func cleanOAuthClientName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > mcp.MaxOAuthClientNameRunes {
		name = string([]rune(name)[:mcp.MaxOAuthClientNameRunes])
	}
	return name
}

// handleOAuthRegister は AI アプリ（公開クライアント）を登録する（RFC 7591）。リダイレクト URI は https の URL と、
// この PC の http://localhost・http://127.0.0.1 だけを受け付ける。クライアントシークレットは発行せず、
// token_endpoint_auth_method は常に none（PKCE で守る）として登録する。
func (s *Server) handleOAuthRegister(c echo.Context) error {
	if retry, ok := s.oauthLimits.allowRegister(c.Request()); !ok {
		return oauthTooManyRequests(c, retry)
	}
	mt, _, err := mime.ParseMediaType(c.Request().Header.Get(echo.HeaderContentType))
	if err != nil || mt != echo.MIMEApplicationJSON {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "Content-Type must be application/json")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxOAuthRegisterBytes+1))
	if err != nil || len(body) > maxOAuthRegisterBytes {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "request body is too large or unreadable")
	}
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "request body must be a JSON object")
	}
	if len(req.RedirectURIs) == 0 {
		return oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
	}
	if len(req.RedirectURIs) > mcp.MaxOAuthRedirectURIs {
		return oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "too many redirect_uris")
	}
	for _, u := range req.RedirectURIs {
		if !mcp.ValidRedirectURI(u) {
			return oauthError(c, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect_uris must use https, or http on localhost / 127.0.0.1")
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type: "+g)
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			return oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "unsupported response_type: "+rt)
		}
	}
	client, err := s.oauth.RegisterClient(c.Request().Context(), cleanOAuthClientName(req.ClientName), req.RedirectURIs)
	switch {
	case errors.Is(err, mcp.ErrOAuthInvalidRedirectURI):
		return oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect_uris")
	case errors.Is(err, mcp.ErrOAuthTooManyClients):
		return oauthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "too many registered clients; try again later")
	case err != nil:
		return err
	}
	resp := map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if client.ClientName != "" {
		resp["client_name"] = client.ClientName
	}
	oauthNoStore(c)
	return c.JSON(http.StatusCreated, resp)
}

// sessionUser は管理画面のセッション Cookie が有効なら、そのユーザーを返す（OAuth の同意画面は /admin の外にあるため、
// 認証ミドルウェアを通さずに確かめる）。
func (s *Server) sessionUser(c echo.Context) *auth.User {
	cookie, err := c.Cookie(s.cookies.Session)
	if err != nil || cookie.Value == "" {
		return nil
	}
	u, err := auth.ValidateSession(s.db, cookie.Value)
	if err != nil {
		return nil
	}
	return u
}

// validResource は RFC 8707 の resource（このサーバーの MCP エンドポイントの絶対 URL）として受け付けるかを返す。
// 省略は受け付ける。リバースプロキシの内側でもホストが食い違わないよう、パスだけを確かめる。
func (s *Server) validResource(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > maxOAuthResourceBytes {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Fragment != "" {
		return false
	}
	return strings.TrimSuffix(u.Path, "/") == s.cfg.MCPAPIPath
}

// validState は state が送り返せる値（上限の長さ以内で、制御文字を含まない）かを返す。
func validState(state string) bool {
	if len(state) > maxOAuthStateBytes {
		return false
	}
	for _, r := range state {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// checkOAuthAuthorizeRequest は認可要求を確かめる。AI アプリが未登録か、リダイレクト先が登録と完全に一致しないときは、
// 攻撃者のサイトへコードやエラーを送らないよう、リダイレクトせずにこの sorus の画面で断る（fatal に理由を返す）。
// それ以外の不備は、RFC 6749 4.1.2.1 のとおりリダイレクト先へ error（errCode）で知らせる。
func (s *Server) checkOAuthAuthorizeRequest(c echo.Context, req oauthAuthorizeRequest) (client *mcp.OAuthClient, fatal, errCode, errDesc string, err error) {
	ja := language(c) != "en"
	client, err = s.oauth.GetClient(c.Request().Context(), req.ClientID)
	if errors.Is(err, mcp.ErrOAuthInvalidClient) {
		if ja {
			return nil, "この AI アプリは sorus に登録されていません。AI アプリの画面から、もう一度接続をやり直してください。", "", "", nil
		}
		return nil, "This AI app is not registered with sorus. Start connecting again from the AI app.", "", "", nil
	}
	if err != nil {
		return nil, "", "", "", err
	}
	if req.RedirectURI == "" || !client.HasRedirectURI(req.RedirectURI) {
		if ja {
			return nil, "AI アプリが知らせた戻り先（redirect_uri）が、登録された戻り先と一致しません。安全のため接続を中止しました。", "", "", nil
		}
		return nil, "The return address (redirect_uri) sent by the AI app does not match the registered one. The connection was stopped for safety.", "", "", nil
	}
	switch {
	case req.ResponseType != "code":
		return client, "", "unsupported_response_type", "response_type must be code", nil
	case req.CodeChallengeMethod != mcp.OAuthCodeChallengeMethod:
		return client, "", "invalid_request", "code_challenge_method must be S256", nil
	case !mcp.ValidCodeChallenge(req.CodeChallenge):
		return client, "", "invalid_request", "code_challenge is missing or malformed", nil
	case !validState(req.State):
		return client, "", "invalid_request", "state is too long or contains control characters", nil
	case !s.validResource(req.Resource):
		return client, "", "invalid_target", "resource must be this server's MCP endpoint", nil
	}
	return client, "", "", "", nil
}

// oauthRedirect は AI アプリのリダイレクト先へ params を付けて戻す。登録済みのリダイレクト URI にもとから付いている
// クエリは残す。
func oauthRedirect(c echo.Context, redirectURI string, params url.Values) error {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return err
	}
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	oauthNoStore(c)
	return c.Redirect(http.StatusFound, u.String())
}

// oauthErrorRedirect は認可要求の不備をリダイレクト先へ知らせる。state があれば送り返す。
func oauthErrorRedirect(c echo.Context, req oauthAuthorizeRequest, code, desc string) error {
	params := url.Values{"error": {code}}
	if desc != "" {
		params.Set("error_description", desc)
	}
	if req.State != "" && validState(req.State) {
		params.Set("state", req.State)
	}
	return oauthRedirect(c, req.RedirectURI, params)
}

// hideSiteNameContextKey は、画面（layout のタイトルを含む）にサイト名を出さないことを renderPage に伝えるキー。
const hideSiteNameContextKey = "server.hideSiteName"

// renderOAuthAuthorize は同意画面（または同意画面を出せない理由）を返す。
func (s *Server) renderOAuthAuthorize(c echo.Context, code int, data oauthAuthorizeData) error {
	oauthNoStore(c)
	if data.HideSiteName {
		c.Set(hideSiteNameContextKey, true)
	}
	return s.render(c, code, "oauth_authorize", localizedMessage(c, "mcp"), data)
}

// handleOAuthAuthorize は認可要求（GET /oauth/authorize）を受ける。要求を確かめ、ログインしていなければログイン画面へ
// （ログイン後にこの要求へ戻る）、ログイン済みなら AI アプリに許可するかを尋ねる同意画面を出す。
func (s *Server) handleOAuthAuthorize(c echo.Context) error {
	if retry, ok := s.oauthLimits.allowAuthorize(c.Request()); !ok {
		return s.renderOAuthTooManyRequests(c, retry)
	}
	req := readOAuthAuthorizeRequest(c.QueryParam)
	client, fatal, errCode, errDesc, err := s.checkOAuthAuthorizeRequest(c, req)
	if err != nil {
		return err
	}
	if fatal != "" {
		return s.renderOAuthAuthorize(c, http.StatusBadRequest, oauthAuthorizeData{Error: fatal})
	}
	if errCode != "" {
		return oauthErrorRedirect(c, req, errCode, errDesc)
	}
	user := s.sessionUser(c)
	if user == nil {
		return c.Redirect(http.StatusSeeOther, auth.DefaultLoginPath+"?next="+url.QueryEscape(OAuthAuthorizePath+"?"+req.query().Encode()))
	}
	return s.renderOAuthAuthorize(c, http.StatusOK, s.oauthConsentData(client, user, req))
}

// oauthConsentData は同意画面のデータを作る。
func (s *Server) oauthConsentData(client *mcp.OAuthClient, user *auth.User, req oauthAuthorizeRequest) oauthAuthorizeData {
	name := client.ClientName
	if name == "" {
		name = client.ClientID
	}
	host := ""
	if u, err := url.Parse(req.RedirectURI); err == nil {
		host = u.Host
	}
	userName := user.DisplayName
	if userName == "" {
		userName = user.Username
	}
	return oauthAuthorizeData{ClientName: name, RedirectHost: host, UserName: userName, Role: user.Role, Request: req}
}

// handleOAuthAuthorizeDecision は同意画面の「許可する」「許可しない」（POST /oauth/authorize）を受ける。CSRF トークンは
// ミドルウェアが確かめる。許可されたら OAuthCodeTTL（5分）だけ有効な認可コードを発行し、リダイレクト先へ戻す。
func (s *Server) handleOAuthAuthorizeDecision(c echo.Context) error {
	if retry, ok := s.oauthLimits.allowAuthorize(c.Request()); !ok {
		return s.renderOAuthTooManyRequests(c, retry)
	}
	req := readOAuthAuthorizeRequest(c.FormValue)
	client, fatal, errCode, errDesc, err := s.checkOAuthAuthorizeRequest(c, req)
	if err != nil {
		return err
	}
	if fatal != "" {
		return s.renderOAuthAuthorize(c, http.StatusBadRequest, oauthAuthorizeData{Error: fatal})
	}
	if errCode != "" {
		return oauthErrorRedirect(c, req, errCode, errDesc)
	}
	user := s.sessionUser(c)
	if user == nil {
		return c.Redirect(http.StatusSeeOther, auth.DefaultLoginPath+"?next="+url.QueryEscape(OAuthAuthorizePath+"?"+req.query().Encode()))
	}
	if c.FormValue("decision") != "allow" {
		return oauthErrorRedirect(c, req, "access_denied", "the user denied the request")
	}
	code, err := s.oauth.IssueCode(c.Request().Context(), mcp.OAuthCodeRequest{
		ClientID: client.ClientID, UserID: user.ID, RedirectURI: req.RedirectURI,
		CodeChallenge: req.CodeChallenge, Scope: mcp.OAuthScope, Resource: req.Resource,
	})
	if err != nil {
		return err
	}
	c.Logger().Infof("oauth: %s authorized client %s (%s)", user.Username, client.ClientID, client.ClientName)
	params := url.Values{"code": {code}}
	if req.State != "" {
		params.Set("state", req.State)
	}
	return oauthRedirect(c, req.RedirectURI, params)
}

// handleOAuthToken はトークンエンドポイント（POST /oauth/token）。grant_type=authorization_code では認可コードを
// PKCE（code_verifier）・client_id・redirect_uri の一致を確かめて引き換え、grant_type=refresh_token では
// リフレッシュトークンを新しい組に入れ替える。
func (s *Server) handleOAuthToken(c echo.Context) error {
	if retry, ok := s.oauthLimits.allowToken(c.Request()); !ok {
		return oauthTooManyRequests(c, retry)
	}
	mt, _, err := mime.ParseMediaType(c.Request().Header.Get(echo.HeaderContentType))
	if err != nil || mt != echo.MIMEApplicationForm {
		return oauthError(c, http.StatusBadRequest, "invalid_request", "Content-Type must be application/x-www-form-urlencoded")
	}
	clientID := c.FormValue("client_id")
	if clientID == "" {
		// client_secret を持たないクライアントが HTTP Basic の利用者名で client_id を送る場合にも応じる。
		if id, _, ok := c.Request().BasicAuth(); ok {
			clientID, _ = url.QueryUnescape(id)
		}
	}
	ctx := c.Request().Context()
	if _, err := s.oauth.GetClient(ctx, clientID); errors.Is(err, mcp.ErrOAuthInvalidClient) {
		return oauthError(c, http.StatusUnauthorized, "invalid_client", "unknown client_id")
	} else if err != nil {
		return err
	}
	var set *mcp.OAuthTokenSet
	switch c.FormValue("grant_type") {
	case "authorization_code":
		code, redirectURI, verifier := c.FormValue("code"), c.FormValue("redirect_uri"), c.FormValue("code_verifier")
		if code == "" || redirectURI == "" || verifier == "" {
			return oauthError(c, http.StatusBadRequest, "invalid_request", "code, redirect_uri and code_verifier are required")
		}
		set, err = s.oauth.ExchangeCode(ctx, code, clientID, redirectURI, verifier)
	case "refresh_token":
		refresh := c.FormValue("refresh_token")
		if refresh == "" {
			return oauthError(c, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		}
		set, err = s.oauth.Refresh(ctx, refresh, clientID)
	default:
		return oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
	if errors.Is(err, mcp.ErrOAuthInvalidGrant) {
		return oauthError(c, http.StatusBadRequest, "invalid_grant", "the code or refresh token is invalid, expired, or already used")
	}
	if err != nil {
		return err
	}
	oauthNoStore(c)
	return c.JSON(http.StatusOK, map[string]any{
		"access_token":  set.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    set.ExpiresIn,
		"refresh_token": set.RefreshToken,
		"scope":         set.Scope,
	})
}

// oauthAppRow は MCP 連携の画面に出す、OAuth 2.1 で許可した AI アプリ（連携）の1行。トークンの組1つにあたる
// （リフレッシュで入れ替わっても、許可した日時は引き継ぐ）。
type oauthAppRow struct {
	ID string
	// UserID は連携を許可したユーザー。
	UserID     string
	ClientName string
	OwnerName  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	// ExpiresAt は使わないままだと連携が切れる日時（リフレッシュトークンの期限）。
	ExpiresAt time.Time
}

// oauthApps は取り消していない、期限内の連携の一覧を返す。all なら全員の連携、そうでなければ userID の連携だけ。
func (s *Server) oauthApps(userID string, all bool) ([]oauthAppRow, error) {
	return s.oauthAppsFor(userID, all, -1)
}

// oauthAppsFor は oauthApps と同じ一覧を、新しい順に最大 limit 件（負なら無制限）返す。
func (s *Server) oauthAppsFor(userID string, all bool, limit int) ([]oauthAppRow, error) {
	// 日時の列は式（COALESCE など）を通すと文字列で返るため、列のまま読んでから組み立てる。
	var rows []struct {
		ID               string     `db:"id"`
		UserID           string     `db:"user_id"`
		ClientName       string     `db:"client_name"`
		OwnerName        string     `db:"owner_name"`
		CreatedAt        time.Time  `db:"created_at"`
		LastUsedAt       *time.Time `db:"last_used_at"`
		ExpiresAt        time.Time  `db:"expires_at"`
		RefreshExpiresAt *time.Time `db:"refresh_expires_at"`
	}
	err := s.db.Select(&rows, `SELECT t.id, t.user_id, t.created_at, t.last_used_at, t.expires_at, t.refresh_expires_at,
			COALESCE(NULLIF(c.client_name, ''), c.client_id) AS client_name,
			COALESCE(NULLIF(u.display_name, ''), u.username, '') AS owner_name
		FROM oauth_tokens t
		JOIN oauth_clients c ON c.client_id = t.client_id
		JOIN users u ON u.id = t.user_id
		WHERE t.revoked_at IS NULL AND julianday(COALESCE(t.refresh_expires_at, t.expires_at)) > julianday(?)
		  AND (t.user_id = ? OR ?)
		ORDER BY julianday(t.created_at) DESC, t.id LIMIT ?`, clockNow().UTC(), userID, all, limit)
	apps := make([]oauthAppRow, 0, len(rows))
	for _, r := range rows {
		app := oauthAppRow{ID: r.ID, UserID: r.UserID, ClientName: r.ClientName, OwnerName: r.OwnerName, CreatedAt: r.CreatedAt,
			LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt}
		if r.RefreshExpiresAt != nil {
			app.ExpiresAt = *r.RefreshExpiresAt
		}
		apps = append(apps, app)
	}
	return apps, err
}

// handleOAuthAppRevoke は OAuth 2.1 で許可した AI アプリの連携を取り消す（アクセストークンとリフレッシュトークンを
// 無効にする）。自分の連携のほか、管理者は全員の連携を取り消せる。
func (s *Server) handleOAuthAppRevoke(c echo.Context) error {
	user := currentUser(c)
	res, err := s.db.Exec(`UPDATE oauth_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL AND (user_id = ? OR ?)`,
		clockNow().UTC(), c.Param("id"), user.ID, user.HasRole(auth.RoleAdmin))
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
