// Package mcp は外部の AI エージェント（VS Code・Cursor・Claude Code・GitHub Copilot CLI など）から CMS を
// 操作するための Model Context Protocol（MCP）サーバーを、net/http と encoding/json だけで提供する。
//
// トランスポートは、POST で JSON-RPC 2.0 のメッセージ（バッチを含む）を受けて application/json で応答する
// ステートレスな Streamable HTTP と、旧仕様（2024-11-05）の HTTP+SSE。認証は管理画面で発行した API トークンの
// Bearer 認証で、トークンの SHA-256 ハッシュを mcp_tokens テーブル（API トークン）と oauth_tokens テーブル
// （OAuth 2.1 のアクセストークン）と照合し、発行したユーザーの権限で操作する。
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/lock"
	"sorus/internal/rag"
	"sorus/internal/revision"
	"sorus/internal/search"
	"sorus/internal/theme"
	"sorus/static"
)

const (
	// DefaultBasePath は MCP エンドポイントの既定のパス。旧仕様の SSE は DefaultBasePath+"/sse" で接続し、
	// DefaultBasePath+"/messages" にメッセージを送る。
	DefaultBasePath = "/api/mcp"
	// TokenPrefix は GenerateToken が発行する API トークンの接頭辞。秘密情報の検出ツールが見つけやすくする。
	TokenPrefix = "cmsmcp_"

	// serverName は initialize で返すサーバー名。
	serverName = "sorus"
	// defaultVersion は Config.Version を省略したときのサーバーのバージョン。
	defaultVersion = "1.0.0"

	// maxRequestBytes は1回の POST の本文の上限。長い記事本文を下書き保存できる大きさにする。
	maxRequestBytes = 4 << 20
	// maxBatchSize はバッチで受け付けるメッセージ数の上限。
	maxBatchSize = 50
	// maxTokenBytes は照合する API トークンの最大長。これより長い値はハッシュせずに拒否する。
	maxTokenBytes = 256
	// tokenBytes は GenerateToken の乱数バイト長。hex で64文字になる。
	tokenBytes = 32
	// lastUsedInterval は mcp_tokens.last_used_at を更新する最短の間隔。リクエストごとの書き込みを避ける。
	lastUsedInterval = time.Minute

	// maxSSESessions は同時に開ける旧仕様の SSE セッション数の上限。
	maxSSESessions = 100
	// sseKeepAlive は SSE の接続を保つためにコメント行を送る間隔。
	sseKeepAlive = 25 * time.Second
	// sseBuffer はセッションごとに送信待ちにできるメッセージ数。
	sseBuffer = 32

	// codeServerError は HTTP レベルの拒否（認証・ヘッダーの不備など）の本文に使う JSON-RPC のエラーコード。
	codeServerError = -32000
)

// ErrInvalidToken は API トークンが存在しない・失効した・期限切れであることを表す。
var ErrInvalidToken = errors.New("mcp: invalid or expired token")

// TokenStore は Bearer トークンを照合して、トークンを発行したユーザーを返す。
type TokenStore interface {
	// Authenticate は生のトークンが有効なら発行したユーザーを返し、無効なら ErrInvalidToken を返す。
	Authenticate(ctx context.Context, rawToken string) (*auth.User, error)
}

// Config は New に渡す設定。DB 以外は省略できる。
type Config struct {
	// DB は database.Migrate 適用済み（mcp_tokens を含む）の接続。
	DB *sqlx.DB
	// Site metadata is returned by cms_get_site_context.
	SiteName        string
	SiteDescription string
	BaseURL         string
	// GetBaseURL, GetSiteName, and GetSiteDescription dynamically resolve site information,
	// reflecting runtime settings changes and Netlify / Cloudflare Pages publication.
	GetBaseURL         func() string
	GetSiteName        func() string
	GetSiteDescription func() string
	// Search は cms_search のキーワード検索に使う全文検索インデックス。nil ならキーワード検索は使えない。
	Search *search.Index
	// RAG は cms_search の意味検索と cms_proofread の過去記事参照に使う。nil か埋め込み API 未設定なら使えない。
	RAG *rag.Service
	// AI は cms_proofread に使う AI プロバイダー。省略すると設定ガイドだけを返す ai.Dummy を使う。
	AI ai.Provider
	// AISource は呼び出しのたびに使う AI プロバイダーを返す関数。指定すると AI より優先する。管理画面で AI の設定を
	// 保存したときに、MCP も新しいプロバイダーで動くようにする（並行に呼ばれても安全な関数を渡すこと）。
	AISource func() ai.Provider
	// LockTTL は編集ロックの有効期間。管理画面（server.Config.LockTTL）と同じ値にする。既定は lock.DefaultTTL。
	LockTTL time.Duration
	// Tokens は Bearer トークンの照合方法。省略すると mcp_tokens テーブルと照合する NewDBTokenStore を使う。
	Tokens TokenStore
	// OnArticleChanged は MCP で記事を保存・校閲申請した後に呼ばれる。検索インデックスや RAG の埋め込みの
	// 更新に使う。保存の応答を待たせないよう、時間のかかる処理はバックグラウンドで行うこと。
	OnArticleChanged func(articleID string)
	// PublishSite runs the configured static site reflection for an authorized MCP request.
	PublishSite func(ctx context.Context, purgeCDN bool, wait time.Duration) (PublishResult, error)
	// GetPublishStatus returns a snapshot of the active or most recent static site reflection.
	GetPublishStatus func() PublishStatus
	// CheckPublishHealth checks the configured public site URL.
	CheckPublishHealth func(ctx context.Context) PublishHealthResult
	// OnArticlePublished is called after an article is approved for immediate publication.
	OnArticlePublished func()
	// ThemesDir は独自テーマを保存・読み込むディレクトリ。空なら ThemeCatalog の保存先を使い、それも空なら
	// cms_install_theme は使えない（一覧・適用はビルトインテーマで動く）。
	ThemesDir string
	// ThemeCatalog はテーマのカタログ。管理画面と同じものを渡す。nil なら theme.LoadComposite(static.FS, ThemesDir) で作る。
	ThemeCatalog *theme.Catalog
	// ThemeMu はテーマの取り込み・削除・カタログの読み直しの排他に使う。管理画面と同じものを渡す。nil なら内部の Mutex を使う。
	ThemeMu *sync.Mutex
	// OnThemeChanged はテーマを適用・取り込み・削除した後に呼ばれる（静的サイトの再反映など）。
	OnThemeChanged func()
	// BasePath は MCP エンドポイントのパス（末尾の / は除く）。既定は DefaultBasePath。
	BasePath string
	// AllowedOrigins は Origin ヘッダー付きの要求（ブラウザからの要求）で許可するオリジン（例: https://cms.example.ac.jp）。
	// 同じホストからの要求は常に許可する。Origin ヘッダーの無い要求（CLI・エディタ）は検査しない。
	AllowedOrigins []string
	// LoopbackOnly は Host ヘッダーを、この PC 自身（localhost・127.0.0.1・[::1]）と AllowedHosts だけに限るか。
	// この PC の中だけで動くデスクトップモードで true にし、DNS リバインディング（攻撃者のドメインを 127.0.0.1 に
	// 向け直して、ブラウザからローカルの MCP サーバーを呼ばせる攻撃）を Host ヘッダーで断る。false（サーバーモード）
	// なら、社内 LAN の IP アドレスやホスト名でも使えるよう、AllowedHosts が空なら書式の正しい Host ヘッダーはすべて
	// 受け付ける。
	LoopbackOnly bool
	// AllowedHosts は Host ヘッダーとして受け付けるホスト名か host:port（例: cms.example.ac.jp・cms.example.ac.jp:8443。
	// HostAllowed の書式）。LoopbackOnly なら、この PC 自身を指すホスト名に加えて受け付けるホスト。LoopbackOnly でない
	// （サーバーモード）なら、空でなければ Host ヘッダーをこの一覧だけに限り（ALLOWED_HOSTS）、応答に載せるオリジン
	// （PreferredOrigin）にも一覧に無い Host を使わない。
	AllowedHosts []string
	// TrustedProxies は転送ヘッダー（X-Forwarded-For・X-Forwarded-Proto など）を信頼するリバースプロキシの一覧。
	// server パッケージは server.Config.TrustedProxies を解析した一覧を渡し、管理画面・OAuth・回数の制限と同じ判定に
	// そろえる。nil なら環境変数 TRUSTED_PROXIES（EnvTrustedProxies）を使う。
	TrustedProxies *TrustedProxies
	// PublicOnly は管理画面（/admin）を持たない公開サーバーで動くか。true なら、開けない編集画面の URL
	// （edit_path・edit_url・confirm_url）を返さず、変更案は PC の sorus の「届いた原稿」で確かめるよう案内する。
	PublicOnly bool
	// Version は initialize で返すサーバーのバージョン。
	Version string
	// Logger は内部エラーの記録先。省略すると log.Default()。
	Logger *log.Logger
	// ReservedPageSlugs は固定の予約語に加えてページのスラッグに使えない語（article.WithReservedSlugs）。
	// 管理画面と同じく、同期 API のパスの先頭部分（article.PathReservedSlug）を渡す。
	ReservedPageSlugs []string
}

// PublishResult summarizes a completed static site reflection.
type PublishResult struct {
	Status          string   `json:"status"`
	DestinationType string   `json:"destination_type"`
	Uploaded        int      `json:"uploaded"`
	Deleted         int      `json:"deleted"`
	Unchanged       int      `json:"unchanged"`
	Details         []string `json:"details,omitempty"`
	DurationMS      int64    `json:"duration_ms"`
	CDNPurged       bool     `json:"cdn_purged"`
	Message         string   `json:"message"`
}

// PublishStatus is a point-in-time summary of static site reflection.
type PublishStatus struct {
	Running  bool               `json:"running"`
	Phase    string             `json:"phase"`
	Progress int                `json:"progress"`
	Last     *PublishLastResult `json:"last"`
}

// PublishLastResult is the most recently finished reflection.
type PublishLastResult struct {
	OK         bool      `json:"ok"`
	Message    string    `json:"message"`
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
	Uploaded   int       `json:"uploaded"`
	Deleted    int       `json:"deleted"`
	Unchanged  int       `json:"unchanged"`
	DurationMS int64     `json:"duration_ms"`
}

// PublishHealthResult summarizes the configured site's health check.
type PublishHealthResult struct {
	Healthy          bool      `json:"healthy"`
	StatusCode       int       `json:"status_code"`
	ResponseTimeMS   int64     `json:"response_time_ms"`
	TLSDaysRemaining int       `json:"tls_days_remaining"`
	SitemapFound     bool      `json:"sitemap_found"`
	Issues           []string  `json:"issues"`
	CheckedAt        time.Time `json:"checked_at"`
}

// Server は MCP サーバー。http.Handler として BasePath 以下に割り当てる。並行に使用してよい。
type Server struct {
	cfg       Config
	db        *sqlx.DB
	articles  *article.Service
	locker    *lock.Locker
	revisions *revision.Service
	tokens    TokenStore
	logger    *log.Logger
	tools     []*tool
	sessions  *sessionRegistry
	themes    *theme.Catalog
	themeMu   *sync.Mutex
}

// New は cfg の MCP サーバーを返す。
func New(cfg Config) (*Server, error) {
	if cfg.DB == nil {
		return nil, errors.New("mcp: nil DB")
	}
	if cfg.AI == nil {
		cfg.AI = ai.NewDummy("")
	}
	if cfg.SiteName == "" {
		cfg.SiteName = "sorus"
	}
	if cfg.BasePath == "" {
		cfg.BasePath = DefaultBasePath
	}
	// 前後の / を揃える。ルート（"/"）は空文字になり、エンドポイントは "/"・"/sse"・"/messages" になる。
	if trimmed := strings.Trim(cfg.BasePath, "/"); trimmed != "" {
		cfg.BasePath = "/" + trimmed
	} else {
		cfg.BasePath = ""
	}
	if cfg.Version == "" {
		cfg.Version = defaultVersion
	}
	if cfg.Tokens == nil {
		cfg.Tokens = NewDBTokenStore(cfg.DB)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.ThemeCatalog == nil {
		c, err := theme.LoadComposite(static.FS, cfg.ThemesDir)
		if err != nil {
			return nil, fmt.Errorf("mcp: load themes: %w", err)
		}
		cfg.ThemeCatalog = c
	}
	if cfg.ThemeMu == nil {
		cfg.ThemeMu = new(sync.Mutex)
	}
	s := &Server{
		cfg:       cfg,
		db:        cfg.DB,
		articles:  article.NewService(cfg.DB, article.WithReservedSlugs(cfg.ReservedPageSlugs...)),
		locker:    lock.NewLocker(cfg.DB, cfg.LockTTL),
		revisions: revision.NewService(cfg.DB),
		tokens:    cfg.Tokens,
		logger:    cfg.Logger,
		sessions:  &sessionRegistry{sessions: map[string]*sseSession{}, closing: make(chan struct{})},
		themes:    cfg.ThemeCatalog,
		themeMu:   cfg.ThemeMu,
	}
	s.tools = s.newTools()
	return s, nil
}

func (s *Server) baseURL() string {
	if s.cfg.GetBaseURL != nil {
		return s.cfg.GetBaseURL()
	}
	return s.cfg.BaseURL
}

// trustedProxies は転送ヘッダーを信頼するリバースプロキシの一覧（Config.TrustedProxies。nil なら環境変数）。
func (s *Server) trustedProxies() TrustedProxies {
	if s.cfg.TrustedProxies != nil {
		return *s.cfg.TrustedProxies
	}
	return EnvTrustedProxies()
}

// advertisedHosts は応答に載せるオリジンに使ってよい Host の一覧（PreferredOrigin の allowedHosts）。デスクトップ
// モード（LoopbackOnly）の AllowedHosts は、この PC 自身に加えて受け付けるホストのため、ここでは使わない（nil）。
func (s *Server) advertisedHosts() []string {
	if s.cfg.LoopbackOnly {
		return nil
	}
	return s.cfg.AllowedHosts
}

// advertisedBaseURL は応答に載せるオリジンを決めるときに使う公開 URL（PreferredOrigin）。デスクトップモード
// （LoopbackOnly）の公開 URL はいつも静的な公開先（MCP サーバーが無い）のため、使わない（空を返す）。
func (s *Server) advertisedBaseURL() string {
	if s.cfg.LoopbackOnly {
		return ""
	}
	return s.baseURL()
}

func (s *Server) siteName() string {
	if s.cfg.GetSiteName != nil {
		if name := strings.TrimSpace(s.cfg.GetSiteName()); name != "" {
			return name
		}
		return "sorus"
	}
	if name := strings.TrimSpace(s.cfg.SiteName); name != "" {
		return name
	}
	return "sorus"
}

func (s *Server) siteDescription() string {
	if s.cfg.GetSiteDescription != nil {
		return s.cfg.GetSiteDescription()
	}
	return s.cfg.SiteDescription
}

// Close は開いている SSE ストリームをすべて終了し、新しい SSE 接続を受け付けないようにする。
// http.Server.Shutdown は SSE のような終わらない応答を待ち続けるため、Shutdown の前に呼ぶ
// （または http.Server.RegisterOnShutdown に登録する）。Streamable HTTP の POST は引き続き処理する。
// 複数回呼んでもよい。
func (s *Server) Close() {
	s.sessions.close()
}

// ServeHTTP は BasePath への POST を Streamable HTTP として、BasePath+"/sse" への GET と
// BasePath+"/messages" への POST を旧仕様の HTTP+SSE として処理する。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// DNS リバインディング対策: 認証やパスの判定より前に、宛先（Host）と送り元のページ（Origin）を確かめる。
	if !s.hostAllowed(r) {
		writeHTTPError(w, http.StatusForbidden, "forbidden: host not allowed")
		return
	}
	if !s.originAllowed(r) {
		writeHTTPError(w, http.StatusForbidden, "forbidden: origin not allowed")
		return
	}
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case s.cfg.BasePath:
		s.serveStreamable(w, r)
	case s.cfg.BasePath + "/sse":
		s.serveSSE(w, r)
	case s.cfg.BasePath + "/messages":
		s.serveSSEMessage(w, r)
	default:
		writeHTTPError(w, http.StatusNotFound, "not found")
	}
}

// serveStreamable は Streamable HTTP の POST を処理する。サーバーから送る通知が無いため GET の SSE ストリームは
// 提供せず（405）、セッションも発行しない。
func (s *Server) serveStreamable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed: use POST")
		return
	}
	user, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if !accepts(r, "application/json") {
		writeHTTPError(w, http.StatusNotAcceptable, "not acceptable: client must accept application/json")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(supportedProtocolVersions, v) {
		writeHTTPError(w, http.StatusBadRequest, "unsupported MCP-Protocol-Version: "+v)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	status, out := s.handleBody(r.Context(), user, body)
	if out == nil {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, out)
}

// authorize は Origin・Bearer トークン・ユーザーの権限を検査し、通れば発行したユーザーを返す。
// 通らなければ応答を書き込んで ok = false を返す。
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	// 401 には、OAuth 2.1 に対応した AI アプリが認可サーバーを見つけられるよう、保護されたリソースのメタデータ
	// （RFC 9728）の URL を付ける。秘匿したパス（BasePath が DefaultBasePath でない）で動かすときは、公開の
	// /.well-known/oauth-protected-resource に秘匿したパスを載せないよう、このエンドポイント専用のメタデータ
	// （パスを挿入した形。ProtectedResourceMetadataPathFor）を案内する。
	// オリジンは、公開 URL（BaseURL）のホスト名が要求の Host と同じならそのスキーム・ポートで組み立てる
	// （PreferredOrigin。クライアントが書いた X-Forwarded-Proto などに左右されない）。
	metadata := `resource_metadata="` + s.trustedProxies().PreferredOrigin(r, s.advertisedBaseURL(), s.advertisedHosts()) +
		ProtectedResourceMetadataPathFor(s.cfg.BasePath) + `"`
	token, ok := bearerToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cms-mcp", `+metadata)
		writeHTTPError(w, http.StatusUnauthorized, "unauthorized: Authorization: Bearer <token> is required")
		return nil, false
	}
	user, err := s.tokens.Authenticate(r.Context(), token)
	if errors.Is(err, ErrInvalidToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cms-mcp", error="invalid_token", `+metadata)
		writeHTTPError(w, http.StatusUnauthorized, "unauthorized: invalid or expired token")
		return nil, false
	}
	if err != nil {
		s.logger.Printf("mcp: authenticate: %v", err)
		writeHTTPError(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	if roleRank(user.Role) == 0 {
		writeHTTPError(w, http.StatusForbidden, "forbidden: the token owner has no CMS role")
		return nil, false
	}
	return user, true
}

// hostAllowed は要求の Host ヘッダーを受け付けるかを返す。書式の正しくない Host は常に断る。LoopbackOnly
// （デスクトップモード）なら、この PC 自身を指すホスト名（localhost・127.0.0.0/8・::1）と AllowedHosts だけを受け付ける。
// DNS リバインディングの要求は、Host ヘッダーが攻撃者のドメイン名になるため、ここで断られる。サーバーモードでは、
// AllowedHosts（ALLOWED_HOSTS）を指定したときだけ、一覧に無い Host を断る。
func (s *Server) hostAllowed(r *http.Request) bool {
	h, ok := hostname(r.Host)
	if !ok {
		return false
	}
	if s.cfg.LoopbackOnly {
		return isLoopbackHost(h) || HostAllowed(r.Host, s.cfg.AllowedHosts)
	}
	return len(s.cfg.AllowedHosts) == 0 || HostAllowed(r.Host, s.cfg.AllowedHosts)
}

// originAllowed はブラウザからの要求（Origin ヘッダー付き）が同じホストか AllowedOrigins のオリジンからかを返す。
// DNS リバインディングで他のサイトからローカルの CMS を操作されるのを防ぐ。
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if slices.Contains(s.cfg.AllowedOrigins, origin) {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// bearerToken は Authorization ヘッダーの Bearer トークンを返す。スキーム名の大文字・小文字は区別しない。
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// accepts は Accept ヘッダーが mediaType を受け付けるかを返す。ヘッダーが無ければ受け付けるとみなす。
func accepts(r *http.Request, mediaType string) bool {
	header := r.Header.Get("Accept")
	if strings.TrimSpace(header) == "" {
		return true
	}
	major, _, _ := strings.Cut(mediaType, "/")
	for _, part := range strings.Split(header, ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if mt == mediaType || mt == "*/*" || mt == major+"/*" {
			return true
		}
	}
	return false
}

// readBody は Content-Type が application/json の本文を maxRequestBytes まで読む。
// 不備があれば応答を書き込んで ok = false を返す。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeHTTPError(w, http.StatusUnsupportedMediaType, "unsupported media type: Content-Type must be application/json")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeHTTPError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large (max %d bytes)", maxRequestBytes))
		return nil, false
	}
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, "failed to read request body")
		return nil, false
	}
	return body, true
}

// handleBody は POST の本文（単一のメッセージまたはバッチ）を処理し、HTTP ステータスと応答の本文を返す。
//
// 返すレスポンスが無い（通知とクライアントのレスポンスだけ）なら 202 と nil を返す。JSON として読めない本文、
// 空のバッチ、不正な単一のメッセージは受け付けられない入力として 400 で返す。
func (s *Server) handleBody(ctx context.Context, user *auth.User, body []byte) (int, any) {
	msgs, batch, err := splitBatch(body)
	if err != nil {
		return http.StatusBadRequest, &response{Error: newError(CodeParseError, "parse error: invalid JSON")}
	}
	if !batch {
		res := s.handleMessage(ctx, user, msgs[0])
		if res == nil {
			return http.StatusAccepted, nil
		}
		if res.Error != nil && res.Error.Code == CodeInvalidRequest {
			return http.StatusBadRequest, res
		}
		return http.StatusOK, res
	}
	if len(msgs) == 0 {
		return http.StatusBadRequest, &response{Error: newError(CodeInvalidRequest, "invalid request: empty batch")}
	}
	if len(msgs) > maxBatchSize {
		return http.StatusBadRequest, &response{Error: newError(CodeInvalidRequest,
			fmt.Sprintf("invalid request: batch too large (max %d messages)", maxBatchSize))}
	}
	out := []*response{}
	for _, m := range msgs {
		if res := s.handleMessage(ctx, user, m); res != nil {
			out = append(out, res)
		}
	}
	if len(out) == 0 {
		return http.StatusAccepted, nil
	}
	return http.StatusOK, out
}

// handleMessage は1件のメッセージを処理し、返すレスポンスを返す。通知とクライアントのレスポンスには nil を返す。
func (s *Server) handleMessage(ctx context.Context, user *auth.User, raw json.RawMessage) (res *response) {
	req, errResp := parseMessage(raw)
	if errResp != nil || req == nil {
		return errResp
	}
	defer func() {
		if p := recover(); p != nil {
			s.logger.Printf("mcp: panic in %s: %v", req.Method, p)
			res = nil
			if !req.isNotification() {
				res = &response{ID: req.ID, Error: newError(CodeInternalError, "internal error")}
			}
		}
	}()
	result, rpcErr := s.dispatch(ctx, user, req)
	if req.isNotification() {
		return nil
	}
	if rpcErr != nil {
		return &response{ID: req.ID, Error: rpcErr}
	}
	return &response{ID: req.ID, Result: result}
}

// dispatch はメソッドごとの処理を呼ぶ。
func (s *Server) dispatch(ctx context.Context, user *auth.User, req *request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p initializeParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
		return s.initialize(p), nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return s.listTools(), nil
	case "tools/call":
		var p callToolParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
		return s.callTool(ctx, user, p)
	case "resources/list":
		var p paginatedParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
		return s.listResources(ctx, user, p.Cursor)
	case "resources/templates/list":
		return listResourceTemplatesResult{ResourceTemplates: resourceTemplates}, nil
	case "resources/read":
		var p readResourceParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
		return s.readResource(ctx, user, p.URI)
	case "notifications/initialized", "notifications/cancelled":
		// ステートレスなため初期化の完了を記録する必要はなく、処理は同期的に終わっているため取り消す対象も無い。
		return nil, nil
	}
	return nil, newError(CodeMethodNotFound, "method not found: "+req.Method)
}

// instructions は initialize でクライアント（AI エージェント）に渡す、このサーバーの使い方。
const instructions = "sorus は、あなたのPCの中で動くローカルファーストの執筆・発信ツールです。" +
	"記事の作成・推敲は cms_save_draft（下書き保存）と cms_proofread（AI 推敲）で行い、仕上がったら cms_submit_for_review で校閲を申請してください。" +
	"公開・承認は編集者が管理画面で行うため、このサーバーからは公開できません。" +
	"管理画面と同じく、執筆チーム（執筆者・編集者・管理者）は他の人の下書きも閲覧・編集できます。" +
	"既存の記事のタイトル・本文・抜粋を置き換える操作（cms_save_draft での更新、既存の翻訳の置き換え、cms_sync_translation）は、" +
	"記事をすぐには変えずに変更案として預かります（needs_confirmation: true）。sorus の画面で人が差分を確かめて保存したときにだけ反映されるため、" +
	"返された confirm_url を利用者に伝えてください。"

// publicOnlyInstructions は PublicOnly のサーバーで instructions の代わりに渡す使い方。公開サーバーには
// 管理画面が無いため、変更案は PC の sorus の「届いた原稿」で確かめてもらう。
const publicOnlyInstructions = "sorus の公開サーバーです。" +
	"記事の作成・推敲は cms_save_draft（下書き保存）と cms_proofread（AI 推敲）で行い、仕上がったら cms_submit_for_review で校閲を申請してください。" +
	"このサーバーには管理画面がありません。既存の記事のタイトル・本文・抜粋を置き換える操作は、記事をすぐには変えずに変更案として預かります（needs_confirmation: true）。" +
	"利用者に、PC の sorus の「届いた原稿」で差分を確かめるよう伝えてください。"

func (s *Server) initialize(p initializeParams) initializeResult {
	return initializeResult{
		ProtocolVersion: negotiateVersion(p.ProtocolVersion),
		Capabilities: serverCapabilities{
			Tools:     &toolsCapability{},
			Resources: &resourcesCapability{},
		},
		ServerInfo:   implementation{Name: serverName, Title: "sorus — ローカルファーストの執筆・発信ツール", Version: s.cfg.Version},
		Instructions: s.instructions(),
	}
}

// instructions は initialize で渡す使い方を返す。
func (s *Server) instructions() string {
	if s.cfg.PublicOnly {
		return publicOnlyInstructions
	}
	return instructions
}

// writeJSON は v を JSON で書き出す。
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := marshalJSON(v, false)
	if err != nil {
		status = http.StatusInternalServerError
		body, _ = marshalJSON(&response{Error: newError(CodeInternalError, "internal error")}, false)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeHTTPError は HTTP レベルの拒否を、id が null の JSON-RPC エラーを本文にして返す。
func writeHTTPError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, &response{Error: newError(codeServerError, msg)})
}

// roleRank は権限の強さを返す。CMS のロールでなければ 0。
func roleRank(r auth.Role) int {
	switch r {
	case auth.RoleWriter:
		return 1
	case auth.RoleEditor:
		return 2
	case auth.RoleAdmin:
		return 3
	}
	return 0
}

// GenerateToken は新しい API トークン（TokenPrefix + 乱数の hex）を返す。DB には HashToken の値だけを保存し、
// 生のトークンは発行時に一度だけ利用者に表示する。
func GenerateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mcp: generate token: %w", err)
	}
	return TokenPrefix + hex.EncodeToString(b), nil
}

// HashToken は API トークンの SHA-256 ハッシュ（小文字 hex）を返す。mcp_tokens.token_hash に保存する値。
func HashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// IssueToken は userID のユーザーに name という名前の API トークンを発行して mcp_tokens に保存し、
// 生のトークンとトークンの ID を返す。expiresAt が nil なら無期限。
func IssueToken(ctx context.Context, db *sqlx.DB, userID, name string, expiresAt *time.Time) (rawToken, tokenID string, err error) {
	rawToken, err = GenerateToken()
	if err != nil {
		return "", "", err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", fmt.Errorf("mcp: issue token: %w", err)
	}
	tokenID = hex.EncodeToString(idBytes)
	var expires any
	if expiresAt != nil {
		expires = expiresAt.UTC()
	}
	_, err = db.ExecContext(ctx, `INSERT INTO mcp_tokens (id, user_id, name, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, tokenID, userID, name, HashToken(rawToken), expires, time.Now().UTC())
	if err != nil {
		return "", "", fmt.Errorf("mcp: issue token: %w", err)
	}
	return rawToken, tokenID, nil
}

// DBTokenStore は mcp_tokens テーブルと照合する TokenStore。
type DBTokenStore struct {
	db *sqlx.DB
	// now は現在時刻を返す。テストで時刻を固定するために差し替える。
	now func() time.Time
}

// NewDBTokenStore は db の mcp_tokens と照合する DBTokenStore を返す。
func NewDBTokenStore(db *sqlx.DB) *DBTokenStore {
	return &DBTokenStore{db: db, now: time.Now}
}

// Authenticate はトークンの SHA-256 ハッシュで mcp_tokens（管理画面で発行した API トークン）を引き、無ければ
// oauth_tokens（OAuth 2.1 で AI アプリに発行したアクセストークン）を引いて、期限内なら発行したユーザーを返す。
// 発行したユーザーが削除されたトークンは外部キーの CASCADE で消えるため、無効になる。
// 照合できたトークンの last_used_at を lastUsedInterval ごとに更新する（失敗しても認証は成功させる）。
func (s *DBTokenStore) Authenticate(ctx context.Context, rawToken string) (*auth.User, error) {
	if rawToken == "" || len(rawToken) > maxTokenBytes {
		return nil, ErrInvalidToken
	}
	now := s.now().UTC()
	var row struct {
		TokenID string `db:"token_id"`
		// Stale は last_used_at が未記録か lastUsedInterval より古いか。
		Stale bool `db:"stale"`
		auth.User
	}
	// 日時の比較は SQLite の julianday で行い、保存形式の違い（タイムゾーン表記など）に左右されないようにする。
	err := s.db.GetContext(ctx, &row, `SELECT t.id AS token_id,
			(t.last_used_at IS NULL OR julianday(t.last_used_at) <= julianday(?)) AS stale,
			u.id, u.username, u.display_name, u.password_hash, u.role, u.created_at
		FROM mcp_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND (t.expires_at IS NULL OR julianday(t.expires_at) > julianday(?))`,
		now.Add(-lastUsedInterval), HashToken(rawToken), now)
	if errors.Is(err, sql.ErrNoRows) {
		return s.authenticateOAuth(ctx, rawToken, now)
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: authenticate: %w", err)
	}
	if row.Stale {
		_, _ = s.db.ExecContext(ctx, `UPDATE mcp_tokens SET last_used_at = ? WHERE id = ?`, now, row.TokenID)
	}
	// パスワードハッシュはツールの処理に不要なため持ち回らない。
	row.User.PasswordHash = ""
	return &row.User, nil
}

// authenticateOAuth は OAuth 2.1 で発行したアクセストークンを oauth_tokens と照合し、取り消されておらず期限内なら
// 連携を許可したユーザーを返す。照合できたトークンの last_used_at を lastUsedInterval ごとに更新する。
func (s *DBTokenStore) authenticateOAuth(ctx context.Context, rawToken string, now time.Time) (*auth.User, error) {
	var row struct {
		TokenID string `db:"token_id"`
		Stale   bool   `db:"stale"`
		auth.User
	}
	err := s.db.GetContext(ctx, &row, `SELECT t.id AS token_id,
			(t.last_used_at IS NULL OR julianday(t.last_used_at) <= julianday(?)) AS stale,
			u.id, u.username, u.display_name, u.password_hash, u.role, u.created_at
		FROM oauth_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND t.revoked_at IS NULL AND julianday(t.expires_at) > julianday(?)`,
		now.Add(-lastUsedInterval), HashToken(rawToken), now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: authenticate oauth: %w", err)
	}
	if row.Stale {
		_, _ = s.db.ExecContext(ctx, `UPDATE oauth_tokens SET last_used_at = ? WHERE id = ?`, now, row.TokenID)
	}
	row.User.PasswordHash = ""
	return &row.User, nil
}

// sessionRegistry は旧仕様の HTTP+SSE のセッション。
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*sseSession
	// closing は Close で閉じ、開いているストリームを終わらせる。
	closing chan struct{}
	closed  bool
}

func (reg *sessionRegistry) close() {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if !reg.closed {
		reg.closed = true
		close(reg.closing)
	}
}

// sseSession は SSE の接続1本。クライアントが messages に POST したメッセージへのレスポンスを out から送る。
type sseSession struct {
	id     string
	userID string
	out    chan []byte
	done   chan struct{}
}

func (reg *sessionRegistry) add(userID string) (*sseSession, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("mcp: generate session id: %w", err)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.closed {
		return nil, errServerClosed
	}
	if len(reg.sessions) >= maxSSESessions {
		return nil, errTooManySessions
	}
	sess := &sseSession{id: hex.EncodeToString(b), userID: userID, out: make(chan []byte, sseBuffer), done: make(chan struct{})}
	reg.sessions[sess.id] = sess
	return sess, nil
}

func (reg *sessionRegistry) remove(sess *sseSession) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	delete(reg.sessions, sess.id)
	close(sess.done)
}

// get は id のセッションを返す。別のユーザーのセッションは無いものとして扱う。
func (reg *sessionRegistry) get(id, userID string) *sseSession {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	sess := reg.sessions[id]
	if sess == nil || sess.userID != userID {
		return nil
	}
	return sess
}

var (
	errTooManySessions = errors.New("mcp: too many sse sessions")
	errServerClosed    = errors.New("mcp: server is closed")
)

// serveSSE は旧仕様（2024-11-05）の HTTP+SSE の GET を処理する。最初に endpoint イベントでメッセージの送り先を
// 知らせ、その後は messages に POST されたメッセージへのレスポンスを message イベントで送る。
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed: use GET")
		return
	}
	user, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if !accepts(r, "text/event-stream") {
		writeHTTPError(w, http.StatusNotAcceptable, "not acceptable: client must accept text/event-stream")
		return
	}
	sess, err := s.sessions.add(user.ID)
	if errors.Is(err, errTooManySessions) || errors.Is(err, errServerClosed) {
		writeHTTPError(w, http.StatusServiceUnavailable, "sse transport unavailable: "+strings.TrimPrefix(err.Error(), "mcp: "))
		return
	}
	if err != nil {
		s.logger.Printf("mcp: sse: %v", err)
		writeHTTPError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer s.sessions.remove(sess)

	rc := http.NewResponseController(w)
	// 長時間の接続になるため、http.Server の WriteTimeout による打ち切りを外す（対応しない ResponseWriter では無視する）。
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event, data string) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("endpoint", s.cfg.BasePath+"/messages?sessionId="+sess.id) {
		return
	}
	ticker := time.NewTicker(sseKeepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.sessions.closing:
			return
		case msg := <-sess.out:
			if !send("message", string(msg)) {
				return
			}
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// serveSSEMessage は旧仕様の HTTP+SSE でクライアントが送るメッセージを処理する。レスポンスはセッションの
// SSE ストリームで送り、この POST には 202 を返す。セッションを開いたユーザーと同じトークンの持ち主だけが送れる。
func (s *Server) serveSSEMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed: use POST")
		return
	}
	user, ok := s.authorize(w, r)
	if !ok {
		return
	}
	sess := s.sessions.get(r.URL.Query().Get("sessionId"), user.ID)
	if sess == nil {
		writeHTTPError(w, http.StatusNotFound, "session not found")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	status, out := s.handleBody(r.Context(), user, body)
	if status >= http.StatusBadRequest {
		writeJSON(w, status, out)
		return
	}
	if out != nil {
		msg, err := marshalJSON(out, false)
		if err != nil {
			s.logger.Printf("mcp: sse: marshal response: %v", err)
			writeHTTPError(w, http.StatusInternalServerError, "internal error")
			return
		}
		select {
		case sess.out <- msg:
		case <-sess.done:
			writeHTTPError(w, http.StatusNotFound, "session closed")
			return
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
