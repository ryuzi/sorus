package server

// 公開専用モード（--public-only）の OAuth 2.1 の同意画面。公開サーバーには管理画面のログインが無く、ユーザーは同期 API の
// 招待（トークン）で作られたユーザー（sync-tok-<jti>。パスワードを持たない）だけのため、接続した PC の sorus で発行した
// 確認コード（同期 API の mcp_issue_login_code。handlers_sync_mcp.go）で本人を確かめる。
//
//  1. AI アプリが GET /oauth/authorize を開く。要求を確かめ、確認コードを入れる画面を出す。
//  2. 利用者は PC の sorus の「設定」→「MCP連携」→「公開サーバー」で確認コードを表示し、画面に入れる（POST step=login）。
//     コードは1回だけ・oauthLoginCodeTTL のあいだ使え、送り元ごとと全体の誤りの回数を制限する（oauth_rate_limit.go）。
//  3. コードが正しければ、そのコードを発行した招待のユーザーと権限（writer・editor・admin）を示す同意画面を出す。
//     本人を確かめた結果は、この認可要求（client_id・redirect_uri・code_challenge）に結び付けた1回限りのチケットにする。
//  4. 「許可する」（POST decision=allow とチケット）で、そのユーザーの認可コードを発行する。AI アプリはこのユーザーの
//     権限で MCP サーバーを使う（ロールによる権限の制御は PC の sorus と同じ）。
//
// 確認コードとチケットはメモリ上だけに持つ（再起動で消える。PC でもう一度表示すればよい）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/mcp"
)

const (
	// oauthLoginCodeTTL は確認コードの有効期間。
	oauthLoginCodeTTL = 10 * time.Minute
	// oauthConsentTicketTTL は確認コードで本人を確かめてから「許可する」を押すまでに許す時間。
	oauthConsentTicketTTL = 10 * time.Minute
	// oauthLoginCodeLength は確認コードの文字数（区切りの - を除く）。32種類の文字で40ビット。誤りの回数の制限と
	// 短い有効期間とあわせて、総当たりで当てられないようにする。
	oauthLoginCodeLength = 8
	// oauthLoginCodeAlphabet は確認コードに使う文字。読み違えやすい I・O・0・1 を除いた32文字。
	oauthLoginCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// maxOAuthLoginEntries は同時に持つ確認コード・チケットの数の上限（それぞれ）。
	maxOAuthLoginEntries = 1000
	// oauthLoginCodeField・oauthTicketField・oauthStepField は同意画面のフォームの欄。
	oauthLoginCodeField = "login_code"
	oauthTicketField    = "ticket"
	oauthStepField      = "step"
	oauthStepLogin      = "login"
)

// errOAuthLoginStoreFull は確認コード・チケットが上限まで溜まっていることを表す。
var errOAuthLoginStoreFull = errors.New("server: oauth: too many pending login codes")

// oauthLoginStore は確認コードとチケット。並行に使用してよい。
type oauthLoginStore struct {
	mu sync.Mutex
	// codes は確認コードの SHA-256 → 発行したユーザー。tickets はチケットの SHA-256 → 確かめた本人と認可要求。
	codes   map[string]oauthLoginCode
	tickets map[string]oauthConsentTicket
	now     func() time.Time
}

type oauthLoginCode struct {
	userID    string
	expiresAt time.Time
}

type oauthConsentTicket struct {
	userID                               string
	clientID, redirectURI, codeChallenge string
	expiresAt                            time.Time
}

func newOAuthLoginStore() *oauthLoginStore {
	return &oauthLoginStore{codes: map[string]oauthLoginCode{}, tickets: map[string]oauthConsentTicket{}, now: time.Now}
}

// normalizeOAuthLoginCode は入力された確認コードから空白・区切りを除き、大文字にする。
func normalizeOAuthLoginCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		if r == '-' || r == ' ' || r == '　' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hashOAuthSecret(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// prune は期限の過ぎた確認コードとチケットを消す。l.mu を持って呼ぶ。
func (l *oauthLoginStore) prune(now time.Time) {
	for k, v := range l.codes {
		if !now.Before(v.expiresAt) {
			delete(l.codes, k)
		}
	}
	for k, v := range l.tickets {
		if !now.Before(v.expiresAt) {
			delete(l.tickets, k)
		}
	}
}

// issueCode は userID の確認コードを発行し、表示用の形（XXXX-XXXX）と期限を返す。同じユーザーの前のコードは無効にする。
func (l *oauthLoginStore) issueCode(userID string) (string, time.Time, error) {
	buf := make([]byte, oauthLoginCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, fmt.Errorf("server: oauth: login code: %w", err)
	}
	code := make([]byte, oauthLoginCodeLength)
	for i, b := range buf {
		code[i] = oauthLoginCodeAlphabet[int(b)%len(oauthLoginCodeAlphabet)]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	for k, v := range l.codes {
		if v.userID == userID {
			delete(l.codes, k)
		}
	}
	if len(l.codes) >= maxOAuthLoginEntries {
		return "", time.Time{}, errOAuthLoginStoreFull
	}
	expires := now.Add(oauthLoginCodeTTL)
	l.codes[hashOAuthSecret(string(code))] = oauthLoginCode{userID: userID, expiresAt: expires}
	return string(code[:4]) + "-" + string(code[4:]), expires, nil
}

// consumeCode は確認コードが有効ならそれを使用済みにして、発行したユーザーを返す。
func (l *oauthLoginStore) consumeCode(raw string) (string, bool) {
	code := normalizeOAuthLoginCode(raw)
	if len(code) != oauthLoginCodeLength {
		return "", false
	}
	key := hashOAuthSecret(code)
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.codes[key]
	if !ok {
		return "", false
	}
	delete(l.codes, key)
	if !l.now().Before(v.expiresAt) {
		return "", false
	}
	return v.userID, true
}

// issueTicket は本人を確かめた userID を、認可要求 req に結び付けたチケットにする。
func (l *oauthLoginStore) issueTicket(userID string, req oauthAuthorizeRequest) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("server: oauth: ticket: %w", err)
	}
	ticket := hex.EncodeToString(buf)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	if len(l.tickets) >= maxOAuthLoginEntries {
		return "", errOAuthLoginStoreFull
	}
	l.tickets[hashOAuthSecret(ticket)] = oauthConsentTicket{userID: userID, clientID: req.ClientID, redirectURI: req.RedirectURI,
		codeChallenge: req.CodeChallenge, expiresAt: now.Add(oauthConsentTicketTTL)}
	return ticket, nil
}

// consumeTicket はチケットを使用済みにし、期限内で認可要求 req と一致すれば本人のユーザー ID を返す。
func (l *oauthLoginStore) consumeTicket(ticket string, req oauthAuthorizeRequest) (string, bool) {
	if ticket == "" || len(ticket) > 128 {
		return "", false
	}
	key := hashOAuthSecret(ticket)
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.tickets[key]
	if !ok {
		return "", false
	}
	delete(l.tickets, key)
	if !l.now().Before(v.expiresAt) || v.clientID != req.ClientID || v.redirectURI != req.RedirectURI || v.codeChallenge != req.CodeChallenge {
		return "", false
	}
	return v.userID, true
}

// oauthHidesSiteName は同意画面でサイト名を出さないかを返す。閲覧制限を有効にした公開専用モードのサーバーでは、
// 同意画面は閲覧制限の対象外のため、本人を確かめる前の画面にサイト名を出さない。
func (s *Server) oauthHidesSiteName() bool {
	return s.cfg.PublicOnly && s.basicAuthActive()
}

// oauthUser は同意画面に出すユーザーを読む。
func (s *Server) oauthUser(ctx context.Context, id string) (*auth.User, error) {
	var u auth.User
	err := s.db.GetContext(ctx, &u, `SELECT id, username, display_name, password_hash, role, created_at FROM users WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	u.PasswordHash = ""
	return &u, nil
}

// publicOAuthLoginData は確認コードを入れる画面のデータを作る。
func (s *Server) publicOAuthLoginData(client *mcp.OAuthClient, req oauthAuthorizeRequest, loginErr string) oauthAuthorizeData {
	name := client.ClientName
	if name == "" {
		name = client.ClientID
	}
	host := ""
	if u, err := url.Parse(req.RedirectURI); err == nil {
		host = u.Host
	}
	return oauthAuthorizeData{ClientName: name, RedirectHost: host, Request: req, LoginRequired: true, LoginError: loginErr,
		HideSiteName: s.oauthHidesSiteName()}
}

// publicOAuthCheck は公開専用モードの同意画面への要求を確かめる。続けてよければ client を返し、応答を書いたときは
// done = true を返す。
func (s *Server) publicOAuthCheck(c echo.Context, req oauthAuthorizeRequest) (client *mcp.OAuthClient, done bool, err error) {
	client, fatal, errCode, errDesc, err := s.checkOAuthAuthorizeRequest(c, req)
	if err != nil {
		return nil, true, err
	}
	if fatal != "" {
		return nil, true, s.renderOAuthAuthorize(c, http.StatusBadRequest, oauthAuthorizeData{Error: fatal, HideSiteName: s.oauthHidesSiteName()})
	}
	if errCode != "" {
		return nil, true, oauthErrorRedirect(c, req, errCode, errDesc)
	}
	return client, false, nil
}

// handlePublicOAuthAuthorize は公開専用モードの認可要求（GET /oauth/authorize）を受ける。要求を確かめ、PC の sorus で
// 表示した確認コードを入れる画面を出す。
func (s *Server) handlePublicOAuthAuthorize(c echo.Context) error {
	if retry, ok := s.oauthLimits.allowAuthorize(c.Request()); !ok {
		return s.renderOAuthTooManyRequests(c, retry)
	}
	req := readOAuthAuthorizeRequest(c.QueryParam)
	client, done, err := s.publicOAuthCheck(c, req)
	if done {
		return err
	}
	return s.renderOAuthAuthorize(c, http.StatusOK, s.publicOAuthLoginData(client, req, ""))
}

// handlePublicOAuthAuthorizePost は公開専用モードの同意画面のフォーム（POST /oauth/authorize）を受ける。CSRF トークンは
// ミドルウェアが確かめる。step=login なら確認コードを確かめて同意画面を出し、そうでなければ「許可する」「許可しない」を受ける。
func (s *Server) handlePublicOAuthAuthorizePost(c echo.Context) error {
	if retry, ok := s.oauthLimits.allowAuthorize(c.Request()); !ok {
		return s.renderOAuthTooManyRequests(c, retry)
	}
	req := readOAuthAuthorizeRequest(c.FormValue)
	client, done, err := s.publicOAuthCheck(c, req)
	if done {
		return err
	}
	ja := language(c) != "en"
	ctx := c.Request().Context()
	// 「やめる」「許可しない」は、確認コードの画面（step=login の隠し欄つき）からでも、本人を確かめずに access_denied で戻す
	// （確認コードの誤りとして数えない）。確かめた結果（チケット）があれば使えなくする。
	ticket := c.FormValue(oauthTicketField)
	if c.FormValue("decision") == "deny" {
		if ticket != "" {
			s.oauthLogins.consumeTicket(ticket, req)
		}
		return oauthErrorRedirect(c, req, "access_denied", "the user denied the request")
	}
	if c.FormValue(oauthStepField) == oauthStepLogin {
		if retry, blocked := s.oauthLimits.loginBlocked(c.Request()); blocked {
			return s.renderOAuthTooManyRequests(c, retry)
		}
		userID, ok := s.oauthLogins.consumeCode(c.FormValue(oauthLoginCodeField))
		var user *auth.User
		if ok {
			if user, err = s.oauthUser(ctx, userID); errors.Is(err, sql.ErrNoRows) {
				ok = false
			} else if err != nil {
				return err
			}
		}
		if ok && !s.syncUserUsable(ctx, user.ID) {
			ok = false
		}
		if !ok {
			s.oauthLimits.loginFailed(c.Request())
			msg := "確認コードが正しくないか、有効期限（10分）が切れています。PC の sorus の「設定」→「MCP連携」で、新しい確認コードを表示して入れ直してください。"
			if !ja {
				msg = "The confirmation code is wrong or has expired (10 minutes). Show a new code in sorus on your PC (Settings → MCP) and enter it again."
			}
			return s.renderOAuthAuthorize(c, http.StatusUnprocessableEntity, s.publicOAuthLoginData(client, req, msg))
		}
		ticket, err := s.oauthLogins.issueTicket(user.ID, req)
		if err != nil {
			return err
		}
		data := s.oauthConsentData(client, user, req)
		data.Ticket = ticket
		return s.renderOAuthAuthorize(c, http.StatusOK, data)
	}

	if c.FormValue("decision") != "allow" {
		if ticket != "" {
			s.oauthLogins.consumeTicket(ticket, req)
		}
		return oauthErrorRedirect(c, req, "access_denied", "the user denied the request")
	}
	userID, ok := s.oauthLogins.consumeTicket(ticket, req)
	if ok && !s.syncUserUsable(ctx, userID) {
		ok = false
	}
	if !ok {
		msg := "確認の有効期限が切れたか、確認が済んでいません。もう一度、PC の sorus で表示した確認コードを入れてください。"
		if !ja {
			msg = "The confirmation has expired or was not completed. Enter the confirmation code shown in sorus on your PC again."
		}
		return s.renderOAuthAuthorize(c, http.StatusUnprocessableEntity, s.publicOAuthLoginData(client, req, msg))
	}
	code, err := s.oauth.IssueCode(ctx, mcp.OAuthCodeRequest{
		ClientID: client.ClientID, UserID: userID, RedirectURI: req.RedirectURI,
		CodeChallenge: req.CodeChallenge, Scope: mcp.OAuthScope, Resource: req.Resource,
	})
	if err != nil {
		return err
	}
	c.Logger().Infof("oauth: %s authorized client %s (%s) with a confirmation code", userID, client.ClientID, client.ClientName)
	params := url.Values{"code": {code}}
	if req.State != "" {
		params.Set("state", req.State)
	}
	return oauthRedirect(c, req.RedirectURI, params)
}

// syncTokenUserJTI は同期 API の招待で作られたユーザーの ID（sync-tok-<jti>）から jti を返す。そうでなければ ok = false。
func syncTokenUserJTI(userID string) (string, bool) {
	return strings.CutPrefix(userID, syncUserIDPrefix+"tok-")
}

// syncUserUsableQ は userID が同期 API の招待で作られたユーザーなら、その招待が取り消されておらず期限内かを返す。
// それ以外のユーザー（管理画面のユーザー）は常に true。記録を読めないときは false（受け付けない）。
func syncUserUsableQ(ctx context.Context, q sqlx.QueryerContext, userID string, now time.Time) bool {
	jti, ok := syncTokenUserJTI(userID)
	if !ok {
		return true
	}
	var live bool
	err := sqlx.GetContext(ctx, q, &live, `SELECT revoked_at IS NULL AND julianday(expires_at) > julianday(?) FROM sync_tokens WHERE jti = ?`,
		now.UTC(), jti)
	return err == nil && live
}

// syncUserUsable は userID が使えるユーザーか（syncUserUsableQ）を返す。
func (s *Server) syncUserUsable(ctx context.Context, userID string) bool {
	return syncUserUsableQ(ctx, s.db, userID, clockNow())
}

// syncAwareTokenStore は MCP の Bearer トークン（API トークン・OAuth 2.1 のアクセストークン）を inner で照合し、
// 持ち主が同期 API の招待で作られたユーザーなら、その招待が取り消されておらず期限内のときだけ受け付ける。公開サーバーで
// 招待を取り消したら（revoke_token）、その招待で発行した MCP のトークンと AI アプリの連携も、すぐに使えなくなる。
type syncAwareTokenStore struct {
	db    *sqlx.DB
	inner mcp.TokenStore
}

// Authenticate は mcp.TokenStore を満たす。
func (t *syncAwareTokenStore) Authenticate(ctx context.Context, rawToken string) (*auth.User, error) {
	u, err := t.inner.Authenticate(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	if !syncUserUsableQ(ctx, t.db, u.ID, clockNow()) {
		return nil, mcp.ErrInvalidToken
	}
	return u, nil
}
