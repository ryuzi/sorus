package mcp

// OAuth 2.1（PKCE + Dynamic Client Registration）で MCP クライアント（ChatGPT・Claude などの AI アプリ）に
// アクセストークンを発行するための保存処理。HTTP のエンドポイント（ディスカバリー・登録・認可・トークン）は
// server パッケージ（handlers_oauth.go）が受け持ち、ここではクライアント・認可コード・トークンの保存と検証を行う。
//
// 認可コード・アクセストークン・リフレッシュトークンは、API トークンと同じく SHA-256 ハッシュ（HashToken）だけを
// 保存する。クライアントはすべて公開クライアント（クライアントシークレットを持たない）で、PKCE（S256）で
// 認可コードの横取りを防ぐ。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	// OAuthAccessTokenPrefix・OAuthRefreshTokenPrefix・OAuthCodePrefix・OAuthClientIDPrefix は発行する値の接頭辞。
	// 秘密情報の検出ツールが見つけやすく、ログなどで種類を見分けやすくする。
	OAuthAccessTokenPrefix  = "sorusat_"
	OAuthRefreshTokenPrefix = "sorusrt_"
	OAuthCodePrefix         = "soruscode_"
	OAuthClientIDPrefix     = "sorusc_"

	// OAuthAccessTokenTTL はアクセストークンの有効期間（token エンドポイントの expires_in）。
	OAuthAccessTokenTTL = time.Hour
	// OAuthRefreshTokenTTL はリフレッシュトークンの有効期間。使うたびに新しいトークンへ入れ替わり（ローテーション）、
	// 期間も延びるため、この期間 AI アプリを使わなかったときだけ、もう一度許可が要る。
	OAuthRefreshTokenTTL = 30 * 24 * time.Hour
	// OAuthCodeTTL は認可コードの有効期間。
	OAuthCodeTTL = 5 * time.Minute
	// OAuthScope は発行するトークンのスコープ。トークンは許可したユーザーの権限で MCP のすべての操作に使える。
	OAuthScope = "mcp"
	// OAuthCodeChallengeMethod は受け付ける PKCE の方式（plain は受け付けない）。
	OAuthCodeChallengeMethod = "S256"

	// MaxOAuthRedirectURIs は1つのクライアントに登録できるリダイレクト URI の数の上限。
	MaxOAuthRedirectURIs = 10
	// maxOAuthRedirectURIBytes はリダイレクト URI 1つの長さの上限。
	maxOAuthRedirectURIBytes = 2000
	// MaxOAuthClientNameRunes はクライアント名の文字数の上限。
	MaxOAuthClientNameRunes = 200
	// maxOAuthClients は登録しておけるクライアント数の上限。登録は認証なしで受け付けるため、大量の登録で DB を
	// 膨らませられないようにする。
	maxOAuthClients = 1000
	// oauthUnusedClientTTL は、一度も認可されなかったクライアントの登録を消すまでの時間。
	oauthUnusedClientTTL = 24 * time.Hour
)

var (
	// ErrOAuthInvalidClient はクライアント ID が登録されていないことを表す。
	ErrOAuthInvalidClient = errors.New("mcp: oauth: unknown client")
	// ErrOAuthInvalidGrant は認可コード・リフレッシュトークンが無効（存在しない・期限切れ・使用済み・
	// クライアントやリダイレクト URI の不一致・PKCE の検証失敗）であることを表す。
	ErrOAuthInvalidGrant = errors.New("mcp: oauth: invalid grant")
	// ErrOAuthInvalidRedirectURI はリダイレクト URI が https か、この PC（localhost・127.0.0.1）の http でないことを表す。
	ErrOAuthInvalidRedirectURI = errors.New("mcp: oauth: invalid redirect_uri")
	// ErrOAuthTooManyClients は登録済みのクライアントが上限に達していることを表す。
	ErrOAuthTooManyClients = errors.New("mcp: oauth: too many registered clients")
)

// OAuthClient は登録済みの OAuth クライアント。
type OAuthClient struct {
	ClientID     string
	ClientName   string
	RedirectURIs []string
	CreatedAt    time.Time
}

// HasRedirectURI は uri が登録済みのリダイレクト URI のどれかと完全に一致するかを返す。
func (c *OAuthClient) HasRedirectURI(uri string) bool {
	for _, u := range c.RedirectURIs {
		if u == uri {
			return true
		}
	}
	return false
}

// OAuthCodeRequest は認可コードを発行するときの内容。
type OAuthCodeRequest struct {
	ClientID      string
	UserID        string
	RedirectURI   string
	CodeChallenge string
	Scope         string
	Resource      string
}

// OAuthTokenSet は token エンドポイントで返すトークンの組。
type OAuthTokenSet struct {
	AccessToken  string
	RefreshToken string
	Scope        string
	// ExpiresIn はアクセストークンの有効期間（秒）。
	ExpiresIn int
}

// OAuthStore は oauth_clients・oauth_codes・oauth_tokens を読み書きする。並行に使用してよい。
type OAuthStore struct {
	db *sqlx.DB
	// now は現在時刻を返す。テストで時刻を進めるために差し替える。
	now func() time.Time
}

// NewOAuthStore は db（database.Migrate 適用済み）の OAuthStore を返す。
func NewOAuthStore(db *sqlx.DB) *OAuthStore {
	return &OAuthStore{db: db, now: time.Now}
}

// SetClock は現在時刻を返す関数を差し替える（テスト用）。
func (s *OAuthStore) SetClock(now func() time.Time) { s.now = now }

// ValidRedirectURI は OAuth クライアントのリダイレクト URI として受け付けられるかを返す。https の URL と、
// この PC で待ち受ける http://localhost・http://127.0.0.1（ポートは任意。IPv6 の [::1] も同じ扱い）だけを受け付け、
// フラグメント付きの URI、ユーザー情報付きの URI、独自スキーム（cursor:// など）は受け付けない。
func ValidRedirectURI(raw string) bool {
	if raw == "" || len(raw) > maxOAuthRedirectURIBytes || strings.TrimSpace(raw) != raw {
		return false
	}
	for _, r := range raw {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil || u.Opaque != "" || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Hostname() != ""
	case "http":
		host := strings.ToLower(u.Hostname())
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// pkceCharsetValid は s が PKCE の code_verifier・code_challenge に使える文字（RFC 7636 の unreserved）だけかを返す。
func pkceCharsetValid(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// ValidCodeVerifier は PKCE の code_verifier（43〜128文字の unreserved 文字）として正しいかを返す。
func ValidCodeVerifier(v string) bool {
	return len(v) >= 43 && len(v) <= 128 && pkceCharsetValid(v)
}

// ValidCodeChallenge は S256 の code_challenge（SHA-256 の base64url、パディングなしの43文字）として正しいかを返す。
func ValidCodeChallenge(c string) bool {
	if len(c) != 43 || !pkceCharsetValid(c) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == sha256.Size
}

// PKCEChallenge は code_verifier から S256 の code_challenge を求める。
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyPKCE は verifier が S256 の challenge に対応するかを、一定時間の比較で確かめる。
func VerifyPKCE(verifier, challenge string) bool {
	if !ValidCodeVerifier(verifier) || !ValidCodeChallenge(challenge) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(PKCEChallenge(verifier)), []byte(challenge)) == 1
}

// randomHex は n バイトの乱数を hex で返す。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mcp: oauth: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// RegisterClient は公開クライアントを登録して返す。redirectURIs はすべて ValidRedirectURI を満たすこと
// （満たさなければ ErrOAuthInvalidRedirectURI）。登録のたびに、一度も認可されないまま oauthUnusedClientTTL が過ぎた
// クライアントを消し、それでも maxOAuthClients に達していれば ErrOAuthTooManyClients を返す。
func (s *OAuthStore) RegisterClient(ctx context.Context, name string, redirectURIs []string) (*OAuthClient, error) {
	if len(redirectURIs) == 0 || len(redirectURIs) > MaxOAuthRedirectURIs {
		return nil, ErrOAuthInvalidRedirectURI
	}
	uris := make([]string, 0, len(redirectURIs))
	for _, u := range redirectURIs {
		if !ValidRedirectURI(u) {
			return nil, ErrOAuthInvalidRedirectURI
		}
		uris = append(uris, u)
	}
	encoded, err := json.Marshal(uris)
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: register: %w", err)
	}
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	client := &OAuthClient{ClientID: OAuthClientIDPrefix + id, ClientName: name, RedirectURIs: uris, CreatedAt: now}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: register: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_clients
		WHERE julianday(created_at) < julianday(?)
		  AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.client_id = oauth_clients.client_id)
		  AND NOT EXISTS (SELECT 1 FROM oauth_codes c WHERE c.client_id = oauth_clients.client_id)`,
		now.Add(-oauthUnusedClientTTL)); err != nil {
		return nil, fmt.Errorf("mcp: oauth: prune clients: %w", err)
	}
	var count int
	if err := tx.GetContext(ctx, &count, `SELECT COUNT(*) FROM oauth_clients`); err != nil {
		return nil, fmt.Errorf("mcp: oauth: register: %w", err)
	}
	if count >= maxOAuthClients {
		return nil, ErrOAuthTooManyClients
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_clients (client_id, client_name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		client.ClientID, client.ClientName, string(encoded), now); err != nil {
		return nil, fmt.Errorf("mcp: oauth: register: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mcp: oauth: register: %w", err)
	}
	return client, nil
}

// GetClient は登録済みのクライアントを返す。登録されていなければ ErrOAuthInvalidClient。
func (s *OAuthStore) GetClient(ctx context.Context, clientID string) (*OAuthClient, error) {
	if clientID == "" || len(clientID) > 200 {
		return nil, ErrOAuthInvalidClient
	}
	var row struct {
		ClientID     string    `db:"client_id"`
		ClientName   string    `db:"client_name"`
		RedirectURIs string    `db:"redirect_uris"`
		CreatedAt    time.Time `db:"created_at"`
	}
	err := s.db.GetContext(ctx, &row, `SELECT client_id, client_name, redirect_uris, created_at FROM oauth_clients WHERE client_id = ?`, clientID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOAuthInvalidClient
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: get client: %w", err)
	}
	c := &OAuthClient{ClientID: row.ClientID, ClientName: row.ClientName, CreatedAt: row.CreatedAt}
	if err := json.Unmarshal([]byte(row.RedirectURIs), &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("mcp: oauth: client %s: redirect_uris: %w", clientID, err)
	}
	return c, nil
}

// IssueCode は req の認可コードを発行し、生のコードを返す（DB にはハッシュだけを保存する）。コードは OAuthCodeTTL の
// あいだ1回だけ使える。期限切れから1日以上たったコードは、このときに消す。
func (s *OAuthStore) IssueCode(ctx context.Context, req OAuthCodeRequest) (string, error) {
	if !ValidCodeChallenge(req.CodeChallenge) {
		return "", ErrOAuthInvalidGrant
	}
	random, err := randomHex(32)
	if err != nil {
		return "", err
	}
	raw := OAuthCodePrefix + random
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_codes WHERE julianday(expires_at) < julianday(?)`, now.Add(-24*time.Hour)); err != nil {
		return "", fmt.Errorf("mcp: oauth: prune codes: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO oauth_codes
		(code_hash, client_id, user_id, redirect_uri, code_challenge, code_challenge_method, scope, resource, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		HashToken(raw), req.ClientID, req.UserID, req.RedirectURI, req.CodeChallenge, OAuthCodeChallengeMethod,
		req.Scope, req.Resource, now.Add(OAuthCodeTTL))
	if err != nil {
		return "", fmt.Errorf("mcp: oauth: issue code: %w", err)
	}
	return raw, nil
}

// execer は sqlx.Tx と sqlx.DB に共通の書き込み。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertTokenSet は新しいアクセストークン・リフレッシュトークンの組を保存して返す。createdAt は連携を許可した日時
// （ローテーションでは最初の許可の日時を引き継ぐ）、lastUsedAt は最後に使った日時（nil なら未使用）。
func (s *OAuthStore) insertTokenSet(ctx context.Context, ex execer, clientID, userID, scope, resource string,
	createdAt time.Time, lastUsedAt *time.Time) (*OAuthTokenSet, error) {
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	access, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	refresh, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	set := &OAuthTokenSet{
		AccessToken:  OAuthAccessTokenPrefix + access,
		RefreshToken: OAuthRefreshTokenPrefix + refresh,
		Scope:        scope,
		ExpiresIn:    int(OAuthAccessTokenTTL / time.Second),
	}
	now := s.now().UTC()
	var used any
	if lastUsedAt != nil {
		used = lastUsedAt.UTC()
	}
	_, err = ex.ExecContext(ctx, `INSERT INTO oauth_tokens
		(id, token_hash, refresh_token_hash, client_id, user_id, scope, resource, expires_at, refresh_expires_at, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, HashToken(set.AccessToken), HashToken(set.RefreshToken), clientID, userID, scope, resource,
		now.Add(OAuthAccessTokenTTL), now.Add(OAuthRefreshTokenTTL), createdAt.UTC(), used)
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: issue token: %w", err)
	}
	return set, nil
}

// pruneTokens はリフレッシュトークンの期限も過ぎた（もう使えない）トークンの記録を消す。
func (s *OAuthStore) pruneTokens(ctx context.Context, ex execer, now time.Time) error {
	_, err := ex.ExecContext(ctx, `DELETE FROM oauth_tokens
		WHERE julianday(COALESCE(refresh_expires_at, expires_at)) < julianday(?)`, now)
	if err != nil {
		return fmt.Errorf("mcp: oauth: prune tokens: %w", err)
	}
	return nil
}

// ExchangeCode は認可コードをトークンの組に引き換える（grant_type=authorization_code）。コードが存在し、期限内で
// 未使用で、発行したときのクライアント・リダイレクト URI と一致し、codeVerifier が PKCE（S256）の検証を通るときだけ
// コードを使用済みにしてトークンを発行する。それ以外は ErrOAuthInvalidGrant。
func (s *OAuthStore) ExchangeCode(ctx context.Context, rawCode, clientID, redirectURI, codeVerifier string) (*OAuthTokenSet, error) {
	if rawCode == "" || len(rawCode) > maxTokenBytes {
		return nil, ErrOAuthInvalidGrant
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: exchange code: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var code struct {
		ClientID      string `db:"client_id"`
		UserID        string `db:"user_id"`
		RedirectURI   string `db:"redirect_uri"`
		CodeChallenge string `db:"code_challenge"`
		Method        string `db:"code_challenge_method"`
		Scope         string `db:"scope"`
		Resource      string `db:"resource"`
		Used          bool   `db:"used"`
		Live          bool   `db:"live"`
	}
	err = tx.GetContext(ctx, &code, `SELECT client_id, user_id, redirect_uri, code_challenge, code_challenge_method, scope, resource,
			used_at IS NOT NULL AS used, julianday(expires_at) > julianday(?) AS live
		FROM oauth_codes WHERE code_hash = ?`, now, HashToken(rawCode))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOAuthInvalidGrant
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: exchange code: %w", err)
	}
	if code.Used || !code.Live || code.ClientID != clientID || code.RedirectURI != redirectURI ||
		code.Method != OAuthCodeChallengeMethod || !VerifyPKCE(codeVerifier, code.CodeChallenge) {
		return nil, ErrOAuthInvalidGrant
	}
	res, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL`, now, HashToken(rawCode))
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: exchange code: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("mcp: oauth: exchange code: %w", err)
	} else if n != 1 {
		return nil, ErrOAuthInvalidGrant
	}
	if err := s.pruneTokens(ctx, tx, now); err != nil {
		return nil, err
	}
	set, err := s.insertTokenSet(ctx, tx, code.ClientID, code.UserID, code.Scope, code.Resource, now, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mcp: oauth: exchange code: %w", err)
	}
	return set, nil
}

// Refresh はリフレッシュトークンを新しいトークンの組に引き換える（grant_type=refresh_token）。古い組（アクセス
// トークンとリフレッシュトークン）は無効にする（ローテーション）。無効にしたリフレッシュトークンがもう一度使われたら、
// トークンが漏れて別の人が使った恐れがあるため、同じクライアント・ユーザーの有効なトークンをすべて無効にする。
func (s *OAuthStore) Refresh(ctx context.Context, rawRefresh, clientID string) (*OAuthTokenSet, error) {
	if rawRefresh == "" || len(rawRefresh) > maxTokenBytes {
		return nil, ErrOAuthInvalidGrant
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: refresh: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var tok struct {
		ID        string    `db:"id"`
		ClientID  string    `db:"client_id"`
		UserID    string    `db:"user_id"`
		Scope     string    `db:"scope"`
		Resource  string    `db:"resource"`
		CreatedAt time.Time `db:"created_at"`
		Revoked   bool      `db:"revoked"`
		Live      bool      `db:"live"`
	}
	err = tx.GetContext(ctx, &tok, `SELECT id, client_id, user_id, scope, resource, created_at,
			revoked_at IS NOT NULL AS revoked,
			(refresh_expires_at IS NOT NULL AND julianday(refresh_expires_at) > julianday(?)) AS live
		FROM oauth_tokens WHERE refresh_token_hash = ?`, now, HashToken(rawRefresh))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOAuthInvalidGrant
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: refresh: %w", err)
	}
	if tok.ClientID != clientID {
		return nil, ErrOAuthInvalidGrant
	}
	if tok.Revoked {
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET revoked_at = ? WHERE client_id = ? AND user_id = ? AND revoked_at IS NULL`,
			now, tok.ClientID, tok.UserID); err != nil {
			return nil, fmt.Errorf("mcp: oauth: revoke reused family: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("mcp: oauth: revoke reused family: %w", err)
		}
		return nil, ErrOAuthInvalidGrant
	}
	if !tok.Live {
		return nil, ErrOAuthInvalidGrant
	}
	res, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, tok.ID)
	if err != nil {
		return nil, fmt.Errorf("mcp: oauth: refresh: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("mcp: oauth: refresh: %w", err)
	} else if n != 1 {
		return nil, ErrOAuthInvalidGrant
	}
	set, err := s.insertTokenSet(ctx, tx, tok.ClientID, tok.UserID, tok.Scope, tok.Resource, tok.CreatedAt, &now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mcp: oauth: refresh: %w", err)
	}
	return set, nil
}

// hostname は Host ヘッダー（host[:port]）からホスト名を取り出して小文字で返す。書式が正しくなければ ok = false。
// IPv6 は [::1]:8080 の形を受け付け、括弧を除いた形（::1）で返す。
func hostname(hostport string) (string, bool) {
	name, _, ok := splitHost(hostport)
	return name, ok
}

// splitHost は Host ヘッダー（host[:port]）をホスト名（小文字。IPv6 は括弧なし、末尾の . なし）とポート（無ければ空）に
// 分ける。書式が正しくなければ ok = false。
func splitHost(hostport string) (name, port string, ok bool) {
	if hostport == "" || len(hostport) > 255 {
		return "", "", false
	}
	for i := 0; i < len(hostport); i++ {
		c := hostport[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == ':' || c == '[' || c == ']') {
			return "", "", false
		}
	}
	validPort := func(p string) bool {
		if p == "" || len(p) > 5 {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
		return true
	}
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", "", false
		}
		ip, rest := hostport[1:end], hostport[end+1:]
		if rest != "" && (!strings.HasPrefix(rest, ":") || !validPort(rest[1:])) {
			return "", "", false
		}
		if net.ParseIP(ip) == nil {
			return "", "", false
		}
		return strings.ToLower(ip), strings.TrimPrefix(rest, ":"), true
	}
	h := hostport
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		if !validPort(h[i+1:]) {
			return "", "", false
		}
		h, port = h[:i], h[i+1:]
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" || strings.ContainsAny(h, ":[]") {
		return "", "", false
	}
	return strings.ToLower(h), port, true
}

// validHost は hostport が Host ヘッダー（host[:port]）として正しい書式かを返す。
func validHost(hostport string) bool {
	_, _, ok := splitHost(hostport)
	return ok
}

// HostAllowed は Host ヘッダーの値 host（host[:port]）が allowed のどれかに当たるかを返す。allowed の値はホスト名か
// host:port（IPv6 は [::1]:8080）。ポートを書いた値はポートまで、書かない値はホスト名だけで比べる（大文字・小文字と
// 末尾の . は区別しない）。書式の正しくない host は、どの値にも当たらない。ALLOWED_HOSTS の照合に使う。
func HostAllowed(host string, allowed []string) bool {
	name, port, ok := splitHost(host)
	if !ok {
		return false
	}
	for _, a := range allowed {
		an, ap, ok := splitHost(strings.TrimSpace(a))
		if ok && an == name && (ap == "" || ap == port) {
			return true
		}
	}
	return false
}

// isLoopbackHost はホスト名がこの PC 自身（localhost・127.0.0.0/8・::1）を指すかを返す。
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// TrustedProxiesEnv は、送り元のアドレス（X-Forwarded-For）と要求のスキーム（X-Forwarded-Proto など）を任せる
// リバースプロキシのアドレスを指定する環境変数。カンマ区切りの IP アドレスか CIDR（例: 127.0.0.1,10.0.0.0/8）。
// 未設定なら、どの接続元の転送ヘッダーも信頼しない（この PC 自身や LAN のアドレスからの接続でも、接続元のアドレスと
// 接続自体の TLS だけを使う）。転送ヘッダーはクライアントが自由に書けるため、プロキシを置く運用で明示したときだけ使う。
const TrustedProxiesEnv = "TRUSTED_PROXIES"

// TrustedProxies は信頼するリバースプロキシのアドレスの一覧。ゼロ値は何も信頼しない。
type TrustedProxies struct {
	prefixes []netip.Prefix
}

// ParseTrustedProxies は TRUSTED_PROXIES の書式（カンマ区切りの IP アドレス・CIDR。前後の空白と空の項目は無視）を
// 解析する。解析できない項目があればエラーを返す。そのときも、解析できた項目だけの一覧を返す（解析できない項目は信頼しない）。
func ParseTrustedProxies(s string) (TrustedProxies, error) {
	var t TrustedProxies
	var bad []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			p, err := netip.ParsePrefix(item)
			if err != nil || p.Addr().Zone() != "" {
				bad = append(bad, item)
				continue
			}
			if p.Addr().Is4In6() && p.Bits() >= 96 {
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
			}
			t.prefixes = append(t.prefixes, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil || a.Zone() != "" {
			bad = append(bad, item)
			continue
		}
		a = a.Unmap()
		t.prefixes = append(t.prefixes, netip.PrefixFrom(a, a.BitLen()))
	}
	if len(bad) > 0 {
		return t, fmt.Errorf("mcp: invalid %s entries (not trusted): %s", TrustedProxiesEnv, strings.Join(bad, ", "))
	}
	return t, nil
}

// Contains は addr が信頼するプロキシのアドレスかを返す。IPv4 射影 IPv6（::ffff:a.b.c.d）は IPv4 として比べる。
func (t TrustedProxies) Contains(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, p := range t.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Empty は信頼するプロキシが1つも無いかを返す。
func (t TrustedProxies) Empty() bool { return len(t.prefixes) == 0 }

// NewTrustedProxies は解析済みのアドレスの範囲から一覧を作る。server パッケージが server.Config.TrustedProxies を
// 解析した範囲を MCP サーバー（Config.TrustedProxies）と共有するのに使う。無効な範囲は飛ばし、IPv4 射影 IPv6 の範囲は
// IPv4 の範囲にそろえる。
func NewTrustedProxies(prefixes ...netip.Prefix) TrustedProxies {
	var t TrustedProxies
	for _, p := range prefixes {
		if !p.IsValid() {
			continue
		}
		if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		t.prefixes = append(t.prefixes, p.Masked())
	}
	return t
}

// trustedProxiesCache は EnvTrustedProxies が最後に解析した環境変数の値と、その結果。
type trustedProxiesCache struct {
	raw  string
	list TrustedProxies
}

var trustedProxiesCur atomic.Pointer[trustedProxiesCache]

// EnvTrustedProxies は環境変数 TRUSTED_PROXIES の今の値の一覧を返す。値が変わらない間は解析し直さない。解析できない
// 項目は信頼せず、値が変わって解析し直すたびにログに出す。Config.TrustedProxies を渡さなかった MCP サーバーだけが使う
// （server パッケージは server.Config.TrustedProxies の一覧を渡し、環境変数には頼らない）。
func EnvTrustedProxies() TrustedProxies {
	raw := os.Getenv(TrustedProxiesEnv)
	if c := trustedProxiesCur.Load(); c != nil && c.raw == raw {
		return c.list
	}
	list, err := ParseTrustedProxies(raw)
	if err != nil {
		log.Printf("%v", err)
	}
	trustedProxiesCur.Store(&trustedProxiesCache{raw: raw, list: list})
	return list
}

// remoteAddr は r の接続元（RemoteAddr）の IP アドレスを返す。解析できなければ ok = false。
func remoteAddr(r *http.Request) (netip.Addr, bool) {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

// FromProxy は r が t のリバースプロキシから直接届いた要求かを返す。true のときだけ、X-Forwarded-For・
// X-Forwarded-Proto などの転送ヘッダーを使ってよい。
func (t TrustedProxies) FromProxy(r *http.Request) bool {
	a, ok := remoteAddr(r)
	return ok && t.Contains(a)
}

// ClientIP は要求の送り元の IP アドレス（回数の制限に使う）を返す。接続元が t のプロキシなら、
// X-Forwarded-For を後ろ（接続元に近い側）から見て、信頼するプロキシでない最初のアドレスを使う（前の方の値は
// クライアントが自分で書けて偽れるため、信頼するプロキシが付け足した値だけをたどる）。途中で書式の正しくない値に
// 当たったら接続元のアドレスを使い、すべて信頼するプロキシなら最も前のアドレスを使う。接続元が信頼するプロキシで
// なければ（t が空なら常に）、この PC 自身や私設のアドレスでも接続元のアドレスを使う。
// 接続元を IP アドレスとして解析できなければ、RemoteAddr のホスト部分をそのまま返す。
func (t TrustedProxies) ClientIP(r *http.Request) string {
	direct, ok := remoteAddr(r)
	if !ok {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return host
	}
	if !t.Contains(direct) {
		return direct.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	client := direct
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return direct.String()
		}
		client = hop.Unmap().WithZone("")
		if !t.Contains(client) {
			break
		}
	}
	return client.String()
}

// Scheme は要求が届いたスキーム（"https" か "http"）を返す。接続自体が TLS なら https。t のプロキシから届いた
// 要求だけ、プロキシが付けた X-Forwarded-Proto などを Echo の Context.Scheme と同じ順で見る。それ以外の接続元の
// 転送ヘッダーは、クライアントが自由に書けるため無視する。
func (t TrustedProxies) Scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if !t.FromProxy(r) {
		return "http"
	}
	var scheme string
	switch {
	case r.Header.Get("X-Forwarded-Proto") != "":
		scheme = r.Header.Get("X-Forwarded-Proto")
	case r.Header.Get("X-Forwarded-Protocol") != "":
		scheme = r.Header.Get("X-Forwarded-Protocol")
	case r.Header.Get("X-Forwarded-Ssl") == "on":
		scheme = "https"
	case r.Header.Get("X-Url-Scheme") != "":
		scheme = r.Header.Get("X-Url-Scheme")
	}
	// 複数のプロキシを通ると "https, http" のように並ぶ。先頭がクライアントの接続したスキーム。
	scheme, _, _ = strings.Cut(scheme, ",")
	if strings.EqualFold(strings.TrimSpace(scheme), "https") {
		return "https"
	}
	return "http"
}

// Origin は要求が届いたオリジン（scheme://host[:port]）を返す。スキームは Scheme（t のプロキシの転送ヘッダー
// だけを使う）。Host ヘッダーの制御文字や引用符などは取り除く（応答ヘッダーに埋め込むため）。
func (t TrustedProxies) Origin(r *http.Request) string {
	host := strings.Map(func(c rune) rune {
		if c > 32 && c < 127 && c != '<' && c != '>' && c != '"' && c != '`' && c != '\\' && c != ',' && c != ';' {
			return c
		}
		return -1
	}, r.Host)
	if host == "" {
		host = "localhost"
	}
	return t.Scheme(r) + "://" + host
}

// PreferredOrigin は応答（OAuth のメタデータ・WWW-Authenticate・招待コードなど）に載せる、この sorus のオリジンを
// 返す。
//
// allowedHosts（ALLOWED_HOSTS。HostAllowed の書式）が空でなく、要求の Host がそのどれにも当たらなければ、偽の
// Host をそのまま載せない: baseURL（サイトの公開 URL）のホストが allowedHosts に当たれば baseURL のオリジン、
// そうでなければ allowedHosts の先頭の（書式の正しい）ホストを、Scheme のスキームで返す。
//
// それ以外は、baseURL が http(s) の絶対 URL で、この PC 自身を指さず、ホスト名が要求の Host のホスト名と同じなら、
// baseURL のオリジン（スキーム・ポートも baseURL のもの。転送ヘッダーや Host のポートに左右されない）を返し、
// 違えば Origin を返す。baseURL は静的な公開先（Cloudflare Pages など。OAuth も MCP サーバーも無い）を指すことも
// あるため、ホスト名が違えば使わない。デスクトップモードの公開 URL はいつも静的な公開先のため、呼び出し側は
// baseURL を空にして渡すこと。
func (t TrustedProxies) PreferredOrigin(r *http.Request, baseURL string, allowedHosts []string) string {
	origin, host, baseOK := baseOrigin(baseURL)
	if len(allowedHosts) > 0 && !HostAllowed(r.Host, allowedHosts) {
		if _, hostport, _ := strings.Cut(origin, "://"); baseOK && HostAllowed(hostport, allowedHosts) {
			return origin
		}
		for _, a := range allowedHosts {
			if a = strings.ToLower(strings.TrimSpace(a)); validHost(a) {
				return t.Scheme(r) + "://" + a
			}
		}
	}
	if baseOK {
		if reqHost, ok := hostname(r.Host); ok && reqHost == host {
			return origin
		}
	}
	return t.Origin(r)
}

// baseOrigin は baseURL のオリジン（scheme://host[:port]。小文字）とホスト名を返す。http(s) の絶対 URL でない、
// ユーザー情報を含む、ホストの書式が正しくない、またはこの PC 自身を指すなら ok = false。
func baseOrigin(baseURL string) (origin, host string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.User != nil || u.Host == "" {
		return "", "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", false
	}
	host, ok = hostname(u.Host)
	if !ok || isLoopbackHost(host) {
		return "", "", false
	}
	return scheme + "://" + strings.ToLower(u.Host), host, true
}

// ProtectedResourceMetadataPath は RFC 9728 の保護されたリソースのメタデータのパス。
const ProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource"

// ProtectedResourceMetadataPathFor は MCP エンドポイント basePath の、保護されたリソースのメタデータのパスを返す。
// 既定のパス（DefaultBasePath。空も同じ）なら、今までどおり共通の ProtectedResourceMetadataPath を返す。秘匿したパス
// （公開サーバーの /api/mcp-<乱数> など）なら、RFC 9728 3.1 のパスを挿入した形
// （/.well-known/oauth-protected-resource/api/mcp-<乱数>）を返す。秘匿したパスは、それを知っている人（401 の
// WWW-Authenticate を受け取った AI アプリ）にだけ案内し、共通のメタデータには載せない。
func ProtectedResourceMetadataPathFor(basePath string) string {
	p := strings.TrimRight(basePath, "/")
	if p == "" || p == DefaultBasePath {
		return ProtectedResourceMetadataPath
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return ProtectedResourceMetadataPath + p
}
