package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/mcp"
)

// サイト閲覧制限（Basic 認証）。サイトの準備中や関係者だけに見せるプレビューのあいだ、公開サイトを開くのに
// ユーザー名とパスワードを求める。
//
// 動作モードごとの扱い（basicAuthApplies は Config.Desktop だけで決める）:
//   - サーバーモード（管理画面つき。sorus --server。同じ LAN・VPN のメンバーが1台のパソコン・サーバーの sorus を共有して
//     静的サイト公開を使う形が主で、Docker・systemd での運用、手元の make dev・go run . も含む）: 「設定」→「サイト設定」で
//     管理者が有効にする。同期 API を有効にしたサーバーでは、接続した PC の set_site_access でも変わる（同じキー・同じ版）。
//   - 公開専用モード（--public-only の公開サーバー）: 管理画面が無いため、接続した PC の管理者が「設定」→「公開サーバー」
//     から同期 API の get_site_access・set_site_access で確認・変更する（handlers_sync_site_access.go）。サイト基本設定の
//     反映（sync_settings）では変わらない。
//   - デスクトップモード（公開サイトをインターネットに直接配信しない）: 閲覧制限は無い。「サイト設定」に項目を出さず、保存も閲覧制限の
//     キーを読み書きしない。v0.2.8 の「サイト設定」で保存した設定が DB に残っていても、公開サイトにパスワードを求めない
//     （設定は消さずに残す）。公開サーバーの閲覧制限は「設定」→「公開サーバー」で扱う。
//
// 管理画面（サーバーモードの /admin 以下、ログインを含む）は管理画面のログインで、MCP サーバー（Config.MCPAPIPath。
// 公開専用モードでは秘匿したパス）・同期 API（Config.SyncAPIPath への POST）はトークンで守るため対象にしない。管理画面も読み込む共通の静的
// アセット（/static/ 以下）・稼働の監視など、サイトの内容を含まないパスも対象にしない。対象外にするのは、ルーティングで
// 実際にそれらの処理に届く要求だけ（siteGateExemptPath。メンテナンス表示と共通）。ルートが合わない要求は公開サイトの
// 404 ページ（サイト名・メニュー）になるため対象にする。管理画面にログイン中の編集部員は、ユーザー名とパスワードを
// 入れなくても公開サイトを開ける（管理画面に表示する画像で、ブラウザがパスワードを尋ねないようにするため）。
//
// この制限は sorus が直接配信する公開サイトにだけかかる。「サイトに反映」で書き出した静的サイト（Cloudflare Pages など）には
// かからない。同期 API でつなぐ公開サーバーの閲覧制限は公開サーバー自身の DB の設定で、接続した側（PC・サーバー）の DB の
// 設定とは互いに影響しない。
//
// 設定の書き込み（サイト設定の保存・set_site_access・復元後の読み直し）は Server.basicAuthMu で排他し、DB に保存してから
// 同じロックの中でメモリ（Server.basicAuth）に反映する。設定が変わるたびに版（basicAuthRevisionSetting）を作り直し、
// 複数の画面・PC からの変更が食い違わないよう、読み込んだときの版と一致する場合だけ書き込む。

const (
	// basicAuthEnabledSetting は閲覧制限を有効にしているか（"1" なら有効）を保存する settings のキー。
	basicAuthEnabledSetting = "basic_auth_enabled"
	// basicAuthUserSetting は閲覧用のユーザー名を保存する settings のキー。
	basicAuthUserSetting = "basic_auth_user"
	// basicAuthPassHashSetting は閲覧用のパスワードの bcrypt ハッシュを保存する settings のキー。パスワードそのものは
	// 保存せず、管理画面にも表示しない。
	basicAuthPassHashSetting = "basic_auth_pass_hash"
	// basicAuthRevisionSetting は閲覧制限の設定の版を保存する settings のキー。設定（有効・ユーザー名・パスワード）が
	// 変わるたびに推測できない乱数で作り直す。パスワードやハッシュからは作らない。一度も変えていなければ空。
	basicAuthRevisionSetting = "basic_auth_revision"
	// basicAuthRevisionBytes は版の乱数部分のバイト数（16進数で32桁）。
	basicAuthRevisionBytes = 16

	// BasicAuthChallenge は閲覧制限で、認証が要る応答に付ける WWW-Authenticate ヘッダーの値。
	BasicAuthChallenge = `Basic realm="Protected Site"`
	// basicAuthCacheControl は閲覧制限で守った応答の Cache-Control。CDN などの共有キャッシュに保存させず
	// （パスワードを知らない人に配らない）、ブラウザには毎回確かめさせる。
	basicAuthCacheControl = "private, no-cache"
	// basicAuthGuardedKey は閲覧制限で守った要求であることを echo.Context に記録するキー（basicAuthPrivateCache が使う）。
	// メンテナンス表示中に通した要求（管理画面にログイン中の編集部員・同期 API のトークンでの画像の取得。maintenance.go）
	// にも記録し、訪問者に見せない応答を共有のキャッシュに残させない。
	basicAuthGuardedKey = "server.basicAuthGuarded"

	// maxBasicAuthUserRunes は閲覧用のユーザー名の上限文字数。
	maxBasicAuthUserRunes = 100
	// maxBasicAuthPassBytes は閲覧用のパスワードの上限バイト数（bcrypt が扱える上限）。
	maxBasicAuthPassBytes = 72
)

// basicAuthSettings は保存済みの閲覧制限の設定。
type basicAuthSettings struct {
	Enabled  bool
	User     string
	PassHash string
}

// active は閲覧制限が働く設定か（有効で、ユーザー名とパスワードが設定済みか）を返す。
func (b basicAuthSettings) active() bool {
	return b.Enabled && b.User != "" && b.PassHash != ""
}

// basicAuthGate は閲覧制限の設定と、照合に成功した資格情報のキャッシュ。設定を保存し直すと作り直す。
type basicAuthGate struct {
	basicAuthSettings
	// verified は bcrypt で照合に成功した「ユーザー名:パスワード」の SHA-256。公開ページは画像なども含めて要求ごとに
	// 資格情報が届くため、毎回 bcrypt で照合すると重い。正しい資格情報は全員が同じため、1つだけ覚えておく。
	verified atomic.Pointer[[sha256.Size]byte]
}

// authorized は r の Authorization ヘッダーが閲覧用のユーザー名とパスワードに一致するかを返す。
func (g *basicAuthGate) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(user + ":" + pass))
	if v := g.verified.Load(); v != nil && subtle.ConstantTimeCompare(v[:], sum[:]) == 1 {
		return true
	}
	// ユーザー名が違っても bcrypt の照合は行い、応答時間からユーザー名の正否を見分けられないようにする。
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(g.User)) == 1
	if !auth.CheckPassword(pass, g.PassHash) || !userOK {
		return false
	}
	g.verified.Store(&sum)
	return true
}

// basicAuthState は保存済みの閲覧制限の設定と、その版。
type basicAuthState struct {
	basicAuthSettings
	Revision string
}

// loadBasicAuthSettings は保存済みの閲覧制限の設定を DB から読む。
func loadBasicAuthSettings(db *sqlx.DB) (basicAuthSettings, error) {
	st, err := loadBasicAuthState(context.Background(), db)
	return st.basicAuthSettings, err
}

// loadBasicAuthState は保存済みの閲覧制限の設定と版を q（DB またはトランザクション）から読む。
func loadBasicAuthState(ctx context.Context, q sqlx.QueryerContext) (basicAuthState, error) {
	var rows []struct{ Key, Value string }
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT key, value FROM settings WHERE key IN (?, ?, ?, ?)`,
		basicAuthEnabledSetting, basicAuthUserSetting, basicAuthPassHashSetting, basicAuthRevisionSetting); err != nil {
		return basicAuthState{}, err
	}
	var b basicAuthState
	for _, r := range rows {
		switch r.Key {
		case basicAuthEnabledSetting:
			b.Enabled = r.Value == "1"
		case basicAuthUserSetting:
			b.User = r.Value
		case basicAuthPassHashSetting:
			b.PassHash = r.Value
		case basicAuthRevisionSetting:
			b.Revision = r.Value
		}
	}
	return b, nil
}

// newBasicAuthRevision は閲覧制限の設定の新しい版を作る。
func newBasicAuthRevision() (string, error) {
	buf := make([]byte, basicAuthRevisionBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("server: basic auth revision: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// saveBasicAuthTx は閲覧制限の設定 next を tx に書き込む。cur は同じ tx で読んだ保存済みの設定。設定が変わる場合だけ
// 書き込み、版を作り直す（変わらなければ何も書かず、changed は false）。呼び出し側は Server.basicAuthMu を持ち、
// コミットしてから setBasicAuth でメモリに反映する。
func saveBasicAuthTx(ctx context.Context, tx *sqlx.Tx, cur basicAuthState, next basicAuthSettings) (saved basicAuthState, changed bool, err error) {
	if cur.basicAuthSettings == next {
		return cur, false, nil
	}
	rev, err := newBasicAuthRevision()
	if err != nil {
		return cur, false, err
	}
	enabled := "0"
	if next.Enabled {
		enabled = "1"
	}
	for _, kv := range [][2]string{
		{basicAuthEnabledSetting, enabled},
		{basicAuthUserSetting, next.User},
		{basicAuthPassHashSetting, next.PassHash},
		{basicAuthRevisionSetting, rev},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, kv[0], kv[1]); err != nil {
			return cur, false, fmt.Errorf("server: save basic auth settings: %w", err)
		}
	}
	return basicAuthState{basicAuthSettings: next, Revision: rev}, true, nil
}

// setBasicAuth は閲覧制限の設定をサーバーのメモリに反映する。次の要求から新しい設定で判定する。
// 保存した設定を反映するときは、保存と同じ Server.basicAuthMu の中で呼ぶ（DB とメモリの順序を揃える）。
func (s *Server) setBasicAuth(b basicAuthSettings) {
	s.basicAuth.Store(&basicAuthGate{basicAuthSettings: b})
}

// basicAuthApplies は、このサーバーで閲覧制限を扱うか（デスクトップモードでないか）を返す。サーバーモード（管理画面
// つき・同期 API の有無を問わない）と公開専用モードなら true。デスクトップモードでは false で、「サイト設定」に項目を
// 出さず、保存済みの設定（v0.2.8 の「サイト設定」で保存したものを含む）があっても働かせない。
func (s *Server) basicAuthApplies() bool {
	return !s.cfg.Desktop
}

// basicAuthActive はサーバーのメモリ上で閲覧制限が働いているか（実際に公開サイトでパスワードを求めるか）を返す。
func (s *Server) basicAuthActive() bool {
	g := s.basicAuth.Load()
	return g != nil && g.active() && s.basicAuthApplies()
}

// reloadBasicAuth は保存済みの閲覧制限の設定を DB から読み直してメモリに反映する。読めなければ今の設定のまま。
func (s *Server) reloadBasicAuth() error {
	s.basicAuthMu.Lock()
	defer s.basicAuthMu.Unlock()
	b, err := loadBasicAuthSettings(s.db)
	if err != nil {
		return err
	}
	s.setBasicAuth(b)
	return nil
}

// basicAuthMiddleware はデスクトップモード以外（basicAuthApplies）で閲覧制限が有効なとき、対象のパスへの要求に閲覧用の
// ユーザー名とパスワードを求める。一致しなければ 401 Unauthorized と WWW-Authenticate ヘッダーを返す。転送ルールの
// リダイレクトや 404 ページからもサイトの内容が分からないよう、ルーティングより前に判定する（e.Pre で登録する）。
func (s *Server) basicAuthMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			g := s.basicAuth.Load()
			if g == nil || !g.active() || !s.basicAuthApplies() || s.siteGateExempt(c) {
				return next(c)
			}
			c.Set(basicAuthGuardedKey, true)
			if g.authorized(c.Request()) || s.isStaff(c) || s.syncTokenReadsUpload(c) {
				return next(c)
			}
			return basicAuthChallenge(c)
		}
	}
}

// syncTokenReadsUpload は、校閲待ちの記事を取り込む PC が公開サーバーの画像（UploadURLPrefix 以下）を
// 同期 API のトークン（Authorization: Bearer）で取得する要求かを返す。閲覧制限を有効にした公開サーバーでも、
// 閲覧用のパスワードを知らない PC が今までどおり取り込めるようにする。同期 API と同じく署名・有効期限を確かめ、
// 校閲待ちの記事を取得できるロール（editor, admin）だけを認める。画像のルートに届く GET・HEAD だけが対象（正規化しない
// パスと正規化したパスのどちらも UploadURLPrefix の下。"//uploads/..." はルートが合わず公開サイトの 404 ページになる）。
// 取り消しも同期 API と同じ記録で確かめ（syncTokenRevoked）、記録の無いトークンはここで記録して一覧に出し、
// 取り消せるようにする。記録を読めない・書けないときは認めない（閲覧用のパスワードを求める 401 になる）。
func (s *Server) syncTokenReadsUpload(c echo.Context) bool {
	r := c.Request()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	underUploads := func(p string) bool {
		return strings.HasPrefix(p, UploadURLPrefix) && strings.HasPrefix(path.Clean(p), UploadURLPrefix)
	}
	if !underUploads(r.URL.Path) || (r.URL.RawPath != "" && !underUploads(r.URL.RawPath)) {
		return false
	}
	scheme, token, ok := strings.Cut(r.Header.Get(echo.HeaderAuthorization), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	pub := s.syncPublicKey()
	if pub == nil {
		return false
	}
	claims, err := auth.VerifyToken(pub, token)
	if err != nil || !auth.CanFetchPending(claims.Role) {
		return false
	}
	revoked, err := s.syncTokenRevoked(r.Context(), claims)
	if err != nil {
		c.Logger().Warnf("server: upload read with a sync token: %v", err)
		return false
	}
	return !revoked
}

// basicAuthChallenge は閲覧用のユーザー名とパスワードを求める 401 応答を返す。ブラウザの入力画面を閉じた人には、
// 本文の案内を表示する。
func basicAuthChallenge(c echo.Context) error {
	h := c.Response().Header()
	h.Set(echo.HeaderWWWAuthenticate, BasicAuthChallenge)
	h.Set("Cache-Control", "no-store")
	h.Set(echo.HeaderXContentTypeOptions, "nosniff")
	lang := string(language(c))
	title := html.EscapeString(localizedMessage(c, "basic_auth_required_title"))
	body := html.EscapeString(localizedMessage(c, "basic_auth_required_body"))
	return c.HTML(http.StatusUnauthorized, `<!doctype html>
<html lang="`+lang+`"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex"><title>401 Unauthorized</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 36rem; margin: 4rem auto; padding: 0 1rem; color: #333; line-height: 1.7">
<h1 style="font-size: 1.25rem">`+title+`</h1>
<p>`+body+`</p>
</body></html>
`)
}

// basicAuthPrivateCache は閲覧制限で守った応答（basicAuthMiddleware・maintenanceMiddleware が記録した要求）の Cache-Control を
// basicAuthCacheControl にする。静的アセット・公開ページのキャッシュのミドルウェアが付ける public の
// Cache-Control より後に決めるよう、それらより内側（後）に e.Use で登録する。
func basicAuthPrivateCache(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if guarded, _ := c.Get(basicAuthGuardedKey).(bool); guarded {
			res := c.Response()
			res.Before(func() { res.Header().Set("Cache-Control", basicAuthCacheControl) })
		}
		return next(c)
	}
}

// isBasicChallenge は h が Basic 認証を求める応答のヘッダー（WWW-Authenticate: Basic ...）かを返す。
func isBasicChallenge(h http.Header) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get(echo.HeaderWWWAuthenticate))), "basic")
}

// siteGateExempt は、閲覧制限（basicAuthMiddleware）とメンテナンス表示（maintenanceMiddleware）をかけない要求かを返す。
// サーバー内部の要求（静的エクスポートと、管理画面のテーマのプレビュー）と、siteGateExemptPath の要求が対象外。
// ルーティングは RawPath があればそれで行うため、Path と RawPath のどちらも対象外のときだけ対象外にする（符号化で
// 対象外のパスに見せかけさせない）。
func (s *Server) siteGateExempt(c echo.Context) bool {
	if isStaticExport(c) {
		return true
	}
	if _, ok := themeOverrideFrom(c); ok {
		return true
	}
	r := c.Request()
	exempt := func(p string) bool { return s.siteGateExemptPath(r.Method, p) }
	return exempt(r.URL.Path) && (r.URL.RawPath == "" || exempt(r.URL.RawPath))
}

// siteGateExemptPath は method で p を開く要求を、閲覧制限・メンテナンス表示の対象外にするかを返す。対象外にするのは、
// ルーティングで実際にサイトの内容を含まない処理（管理画面のログインで守る管理画面・トークンで守る同期 API と MCP
// サーバー・組み込みの共通のアセット・稼働の監視）に届く要求だけ。ルートが合わない要求は "/:slug" などに届き、公開サイトの
// 404 ページ（テーマ・サイト名・メニュー）になるため対象にする。
//   - ルートが 1 つのパスは、登録したメソッドで、正規化しないパスがそのまま一致するときだけ（"/healthz/"・"//healthz"・
//     "/healthz/."、同期 API のパスへの GET や末尾に "/" をつけた POST は対象。秘匿したパスの有無も見せない）。
//   - パスの下をまとめて受けるルートは、正規化しないパス（ルーティングが見る）と正規化したパス（処理が見る）のどちらも
//     その下のときだけ（"//admin" はルートが合わず、"/api/mcp/../../about" は処理が別のパスとして扱う）。
//   - 管理画面は、管理画面のあるサーバーモードだけに登録する。公開専用モードの /admin は PublicGuardMiddleware が公開サイトの
//     404 ページにするため対象。MCP サーバーはどちらのモードでも Config.MCPAPIPath にだけ登録する（公開専用モードの
//     /api/mcp はルートが合わないため対象）。/api の下に、同期 API と MCP サーバーのほかの処理は無い。
//   - OAuth 2.1 のエンドポイントはどちらのモードでも登録する。保護されたリソースのメタデータは、MCP サーバーを秘匿した
//     パスで動かすときはそのパス専用のもの（mcp.ProtectedResourceMetadataPathFor）だけを登録する。
//   - 開いている画面の接続（WindowPath）はデスクトップモードだけにあり、デスクトップモードではどちらも働かないため挙げない。
func (s *Server) siteGateExemptPath(method, p string) bool {
	getOrHead := method == http.MethodGet || method == http.MethodHead
	mcpPath := s.mcpAPIPath()
	switch p {
	case s.cfg.SyncAPIPath:
		return s.cfg.SyncAPIPath != "" && method == http.MethodPost
	case "/healthz":
		return method == http.MethodGet
	case "/manifest.json", "/sw.js":
		return getOrHead
	// MCP の OAuth 2.1 のエンドポイント（handlers_oauth.go）。サイトの内容を含まず、同意画面は管理画面のログイン（公開専用
	// モードでは PC で発行した確認コード）で守る。AI アプリのサーバーが閲覧用のパスワードを知らなくても、メタデータの取得・
	// 登録・トークンの発行ができるようにする。
	case oauthAuthServerMetadataPath:
		return getOrHead || method == http.MethodOptions
	case mcp.ProtectedResourceMetadataPath, mcp.ProtectedResourceMetadataPath + mcp.DefaultBasePath:
		return !s.mcpSecretPath() && (getOrHead || method == http.MethodOptions)
	case mcp.ProtectedResourceMetadataPathFor(mcpPath):
		return mcpPath != "" && (getOrHead || method == http.MethodOptions)
	case OAuthRegisterPath, OAuthTokenPath:
		return method == http.MethodPost || method == http.MethodOptions
	case OAuthAuthorizePath:
		return getOrHead || method == http.MethodPost
	}
	clean := path.Clean(p)
	under := func(prefix string) bool {
		if prefix == "" {
			return false
		}
		in := func(q string) bool { return q == prefix || strings.HasPrefix(q, prefix+"/") }
		return in(p) && in(clean)
	}
	// 管理画面も読み込む、sorus に組み込みの共通のアセット（サイトの内容を含まない）。"/static" と "/static/" は対象にする。
	if getOrHead && strings.HasPrefix(p, "/static/") && strings.HasPrefix(clean, "/static/") {
		return true
	}
	return under(mcpPath) || (!s.cfg.PublicOnly && under("/admin"))
}

// 閲覧制限の入力の誤り。管理画面には表示言語の文言（i18n のキー）で出す。
var (
	errBasicAuthUserRequired = errors.New("basic_auth_err_user_required")
	errBasicAuthUserInvalid  = errors.New("basic_auth_err_user_invalid")
	errBasicAuthUserLong     = errors.New("basic_auth_err_user_long")
	errBasicAuthPassRequired = errors.New("basic_auth_err_pass_required")
	errBasicAuthPassInvalid  = errors.New("basic_auth_err_pass_invalid")
	errBasicAuthPassLong     = errors.New("basic_auth_err_pass_long")
)

// validateBasicAuthInput はサーバーモードの「サイト設定」と公開サーバーの閲覧制限（set_site_access）で入力された設定を
// 検査する。pass が空なら保存済みのパスワードを使い続ける（hasStoredPass はそれがあるか）。有効にするときは、ユーザー名と
// パスワードのどちらも要る。
// ユーザー名に ":" は使えない（Basic 認証はユーザー名とパスワードを ":" で区切って送る）。
func validateBasicAuthInput(enabled bool, user, pass string, hasStoredPass bool) error {
	switch {
	case strings.ContainsRune(user, ':') || strings.ContainsFunc(user, unicode.IsControl):
		return errBasicAuthUserInvalid
	case utf8.RuneCountInString(user) > maxBasicAuthUserRunes:
		return errBasicAuthUserLong
	case strings.ContainsFunc(pass, unicode.IsControl):
		return errBasicAuthPassInvalid
	case len(pass) > maxBasicAuthPassBytes:
		return errBasicAuthPassLong
	case enabled && user == "":
		return errBasicAuthUserRequired
	case enabled && pass == "" && !hasStoredPass:
		return errBasicAuthPassRequired
	}
	return nil
}
