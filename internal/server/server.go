// Package server は Echo v4 による Web サーバーと、管理画面・公開サイトのハンドラを提供する。
// Cookie セッション認証・CSRF 保護・排他編集ロックを組み合わせ、テンプレートと静的アセットは
// embed.FS からシングルバイナリで配信する。
package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"

	"sorus/internal/ai"
	"sorus/internal/analytics"
	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/image"
	"sorus/internal/lock"
	"sorus/internal/mcp"
	"sorus/internal/rag"
	"sorus/internal/revision"
	"sorus/internal/scheduler"
	"sorus/internal/search"
	"sorus/internal/social"
	"sorus/internal/theme"
	"sorus/static"
	"sorus/templates"
)

const (
	// CSRFHeaderName は fetch リクエストが CSRF トークンを載せるヘッダー。
	CSRFHeaderName = echo.HeaderXCSRFToken
	// CSRFFormField はフォーム（sendBeacon の FormData を含む）が CSRF トークンを載せるフィールド。
	CSRFFormField = "_csrf"
	// csrfContextKey は CSRF ミドルウェアがトークンを echo.Context に格納するキー。
	csrfContextKey = "csrf"

	// DefaultSessionDuration はログインセッションの既定の有効期間。
	DefaultSessionDuration = 7 * 24 * time.Hour
	// DesktopSessionDuration はデスクトップモードのログインセッションの既定の有効期間。手元の PC で使う前提のため、
	// ログインしたままにする（ブラウザが Cookie を保てる上限の400日より短くする）。
	DesktopSessionDuration = 365 * 24 * time.Hour
	// DefaultUploadDir はアップロード画像（生成した WebP）の既定の保存先。
	DefaultUploadDir = "uploads"
	// UploadURLPrefix はアップロード画像を配信する URL パス。
	UploadURLPrefix = "/uploads/"
	// DefaultAttachmentDir は添付資料（論文 PDF・配布資料）の既定の保存先。静的配信はせず、
	// ダウンロード API だけが読み出す。
	DefaultAttachmentDir = "attachments"
	// DefaultMaxAttachmentSize は添付資料1件の既定の上限バイト数（30MB）。
	DefaultMaxAttachmentSize = 30 << 20
	// DefaultThemesDir は取り込んだカスタムテーマ（<id>/theme.json・theme.css）の既定の置き場所。
	DefaultThemesDir = "themes"

	// HeartbeatInterval は編集画面がロックのハートビートを送る間隔。TTL（既定2分）より十分短くする。
	HeartbeatInterval = 30 * time.Second

	// bodyLimit はリクエストボディの上限。画像アップロードの上限（15MB）に multipart のヘッダー分を足した値。
	bodyLimit = "20M"
	// multipartOverhead は添付資料のアップロードで、ファイル本体の上限に加えて受け付ける multipart のヘッダー分。
	multipartOverhead = 1 << 20
)

// Config は New に渡すサーバー設定。DB 以外は省略すると既定値を使う。
type Config struct {
	// DB は database.Migrate 適用済みの接続。
	DB *sqlx.DB
	// UploadDir はアップロード画像の保存先ディレクトリ。既定は DefaultUploadDir。
	UploadDir string
	// SecureCookie はセッション・CSRF Cookie に Secure 属性を付けるか。HTTPS で配信する本番では true にする。
	SecureCookie bool
	// SessionDuration はログインセッションの有効期間。既定は DefaultSessionDuration。
	SessionDuration time.Duration
	// LockTTL はハートビートが途絶えてから編集ロックが失効するまでの時間。既定は lock.DefaultTTL。
	LockTTL time.Duration
	// MaxUploadSize は画像アップロードの上限バイト数。既定は image.DefaultMaxUploadSize。
	MaxUploadSize int64

	// SiteName は公開サイトのサイト名（<title>・og:site_name・フィード）。既定は DefaultSiteName。
	SiteName string
	// SiteNameEn は英語表示時のサイト名。未設定時は SiteName にフォールバックする。
	SiteNameEn string
	// ShowAuthor は公開ページや一覧に執筆者名を表示するか。既定は false（非表示）。
	ShowAuthor bool
	// SiteDescription はトップページの meta description とフィードの説明文。
	SiteDescription string
	// BaseURL は canonical・OGP・フィード・サイトマップに使う絶対 URL の起点（例: https://media.example.ac.jp）。
	// 省略するとリクエストの Host から組み立てる。本番では Host ヘッダーの偽装に左右されないよう設定する。
	BaseURL string
	// AllowedHosts は BaseURL が無いとき、公開サイトの絶対 URL（canonical・OGP・フィード・サイトマップ）の組み立てに
	// 使ってよい Host ヘッダーの値（例: example.jp・www.example.jp:8443。main.go が ALLOWED_HOSTS から渡す）。
	// ポートを書いた値はポートまで、書かない値はホスト名だけで比べる。一覧に無い Host で届いた要求には、偽の Host を
	// 載せた URL を返さず先頭の値で組み立てる。空なら Host をそのまま使う（従来どおり）。不正な値なら New がエラーを返す。
	AllowedHosts []string
	// TrustedProxies は転送ヘッダー（X-Forwarded-For・X-Forwarded-Proto）を信頼するリバースプロキシの IP アドレスか
	// CIDR（例: 127.0.0.1・10.0.0.0/8。main.go が TRUSTED_PROXIES から渡す）。空なら転送ヘッダーを一切信頼せず、
	// 直接つながった送り元のアドレスを使う（c.RealIP・スキームの偽装を防ぐ）。不正な値なら New がエラーを返す。
	TrustedProxies []string
	// Search は全文検索インデックス。省略するとメモリ上のインデックスを作る（再起動で作り直す）。
	// 渡したインデックスは呼び出し側が Close する。
	Search *search.Index
	// AI は記事エディタの AI 執筆アシスタント（推敲・タイトル提案・リード文生成）。
	// 省略すると設定ガイドだけを返す ai.Dummy を使う（環境変数からの生成は呼び出し側で ai.NewProviderFromEnv を使う）。
	AI ai.Provider
	// RAG は記事の埋め込み索引・類似記事検索サービス。省略時は RAG 機能を無効にする。
	RAG *rag.Service

	// AttachmentDir は添付資料の保存先ディレクトリ。既定は DefaultAttachmentDir。
	AttachmentDir string
	// MaxAttachmentSize は添付資料1件の上限バイト数。既定は DefaultMaxAttachmentSize。
	MaxAttachmentSize int64
	// ThemesDir は取り込んだカスタムテーマの置き場所。既定は DefaultThemesDir。無ければ取り込むときに作る。
	ThemesDir string
	// MaxRestoreSize は管理画面から復元するバックアップファイルの上限バイト数。既定は DefaultMaxRestoreSize。
	MaxRestoreSize int64
	// SchedulerInterval は予約投稿を確認する周期。既定は scheduler.DefaultInterval。
	SchedulerInterval time.Duration
	// DisableScheduler は予約投稿の自動公開ワーカーを起動しないか。複数プロセスで同じ DB を使い、
	// ワーカーを1つに絞るときに使う。
	DisableScheduler bool

	// Analytics は公開記事の PV を集計するサービス。省略すると New が作成し、Shutdown で未保存の集計を書き出して閉じる。
	// 渡したサービスは呼び出し側が Close する。
	Analytics *analytics.Service
	// CountStaffViews はログイン中の編集部員（管理画面のセッションを持つ人）の閲覧も PV に数えるか。既定は数えない。
	CountStaffViews bool
	// Location は PV の日次集計（ダッシュボードの「本日」・推移の日付の区切り）に使うタイムゾーン。
	// 省略すると管理画面の表示と同じ日本時間（JST）。Analytics を渡すときは同じタイムゾーンで作る。
	Location *time.Location
	// MCPAllowedOrigins は MCP サーバー（MCPAPIPath）へのブラウザからの要求で許可するオリジン。
	// 同じホストからの要求と、Origin ヘッダーの無い要求（CLI・エディタ）は常に許可する。
	MCPAllowedOrigins []string
	// MCPAPIPath は MCP サーバー（Streamable HTTP・旧仕様の SSE）のパス。省略すると、デスクトップモード・サーバーモードは
	// mcp.DefaultBasePath（/api/mcp）、公開専用モードはこの起動のあいだだけ使う推測できないパス（/api/mcp-<乱数>）。
	// 公開専用モードでは main.go が MCP_API_PATH か mcp_path.txt の秘匿パスを渡す。/api の下のパスだけを受け付け
	// （NormalizeMCPAPIPath）、公開専用モードで /api/mcp を指定したとき・同期 API のパスと重なるときは New がエラーを返す。
	// 指定したパスだけを登録し、ほかのパス（秘匿したときの /api/mcp を含む）は存在しないページと同じ 404 になる。
	MCPAPIPath string

	// PublicOnly は公開専用モードで動かすか。管理画面（/admin 以下）を登録せず、/admin への要求は公開サイトの 404 ページで
	// 返す。公開ページ・静的アセット・同期 API は通常どおり配信する。MCP サーバーは秘匿したパス（MCPAPIPath）にだけ登録し、
	// OAuth 2.1 の同意画面は管理画面のログインの代わりに、接続した PC で発行する確認コードで本人を確かめる
	// （handlers_oauth_public.go）。
	PublicOnly bool
	// SyncPublicKey はケーパビリティ・トークンの署名を検証する Ed25519 公開鍵。指定すると同期 API
	// （POST SyncAPIPath）を登録する。省略すると同期 API は登録しない。
	SyncPublicKey ed25519.PublicKey
	// SyncSigningKey は SyncPublicKey と対になる Ed25519 秘密鍵。指定すると同期 API で招待（トークン）を
	// 発行できる（issue_invite）。省略すると招待の発行だけを断る。ログや画面には出さない。
	SyncSigningKey ed25519.PrivateKey
	// SyncAPIPath は同期 API のパス。既定は DefaultSyncAPIPath。公開サーバーでは NewSyncAPIPath で生成した
	// 推測できないパスを指定し、固定のパスへの無差別なスキャンや大量の要求を、存在しないページと同じ 404 で門前払いする。
	// NormalizeSyncAPIPath で検査し、正しくなければ New がエラーを返す。
	SyncAPIPath string

	// Desktop はデスクトップモード（自分の PC で使うアプリ版）で動かすか。管理者が1人もいなければ /admin への要求を
	// 「はじめに」画面（SetupPath）へ送る。管理画面に「sorus を終了」を出す。SessionDuration の既定は DesktopSessionDuration。
	Desktop bool
	// SuggestedUsername は「はじめに」画面のユーザー名の初期値（OS のユーザー名）。ユーザー名に使えない形式なら空欄にする。
	SuggestedUsername string
	// OnQuit はデスクトップモードで「sorus を終了」が押されたときに呼ぶ関数。終了を伝える画面を返す直前に呼ぶため、
	// 呼び出し側は別の goroutine で Shutdown し、処理中の要求（その画面を含む）を返し終えてからプロセスを終える。
	// finishFirst は「予約をすべて反映してから終了する」を選んだか（desktop_app_window_spec.md D4・D5）。true なら、
	// 呼び出し側はまだ反映していない予約（PendingScheduledReflects）を反映し終えてから終了する。
	OnQuit func(finishFirst bool)
	// LogOutput は Echo のログ（ハンドラーの警告・エラー）の出力先。省略すると標準出力。
	LogOutput io.Writer

	// SiteKey はデスクトップモードで同時に動くサイトを見分ける鍵。セッション・CSRF・表示言語の Cookie の名前
	// （auth.CookieNamesFor）に使う。1つ目のサイトとサーバーモードは空（今までの名前のまま）、2つ目以降のサイトは
	// そのサイトの id。英小文字と数字だけにすること（それ以外なら New がエラーを返す）。
	SiteKey string
	// Sites はデスクトップモードのサイトの一覧・追加・削除（「設定」→「サイト」）。サーバーモードでは nil。
	Sites Sites
	// LogPath はデスクトップモードの動作の記録（sorus.log）のパス。「サイト」画面で開けなかったサイトの案内に出す。
	// 省略するとデータディレクトリの sorus.log を使う。
	LogPath string
	// UpdateCheck は新しい版の確認を、デスクトップモードのアプリ全体で1つにまとめる共有の状態。nil なら、この
	// サーバーだけで確かめる（Desktop のときだけ）。
	UpdateCheck *UpdateCheck
}

// Server は管理画面の HTTP サーバー。
type Server struct {
	echo *echo.Echo
	db   *sqlx.DB
	cfg  Config
	// siteMu は s.cfg.BaseURL, SiteName, SiteDescription の動的更新と並行した読み出しを排他する。
	siteMu   sync.RWMutex
	articles *article.Service
	locker   *lock.Locker
	search   *search.Index
	// ownsSearch は search を New が作成し、Shutdown で閉じるか。
	ownsSearch bool
	redirects  *redirectCache
	// aiCur は記事エディタ・MCP が使う AI プロバイダー。「設定」→「AI」で保存すると差し替えるため、多くの
	// goroutine（要求の処理）から同時に読まれても競合しないよう atomic に読み書きする（aiProvider・setAIProvider）。
	aiCur      atomic.Pointer[providerBox]
	ragService *rag.Service
	revisions  *revision.Service
	// themes はビルトインとカスタムテーマを合わせたカタログ（themeCatalog）。テーマを取り込む・削除すると
	// Reload で中身を入れ替える。themeMu は取り込み・削除どうしを排他する（読み出しはカタログ自身が排他する）。
	themes  *theme.Catalog
	themeMu sync.Mutex
	// ragIndexer は記事の埋め込みをバックグラウンドで更新する。related は関連記事の推薦結果のキャッシュ。
	ragIndexer *ragIndexer
	related    *relatedCache

	// scheduler は予約投稿の自動公開ワーカー。Config.DisableScheduler なら nil。
	scheduler *scheduler.Scheduler
	// stopScheduler は scheduler を止める。schedulerDone は scheduler の Run が返ると閉じる。
	stopScheduler context.CancelFunc
	schedulerDone chan struct{}
	// startedAt はサーバーの起動時刻。ヘルスチェックの稼働時間に使う。
	startedAt time.Time

	// analytics は公開記事の PV 集計。ownsAnalytics は New が作成し、Shutdown で閉じるか。
	analytics     *analytics.Service
	ownsAnalytics bool
	// ogp は動的 OGP 画像の生成とキャッシュ。
	ogp *social.Generator
	// mcp は外部の AI エージェント向けの MCP サーバー（/api/mcp）。
	mcp *mcp.Server
	// oauth は MCP の OAuth 2.1（PKCE + DCR）のクライアント・認可コード・トークンの保存（handlers_oauth.go）。
	oauth *mcp.OAuthStore
	// oauthLimits は OAuth 2.1 のエンドポイント（登録・トークン・同意画面・確認コード）の、総当たりを抑える回数の制限
	// （oauth_rate_limit.go）。
	oauthLimits *oauthRateLimits
	// oauthLogins は公開専用モードの同意画面で本人を確かめる確認コードと、確かめた後の同意の手続き（メモリ上。
	// handlers_oauth_public.go）。
	oauthLogins *oauthLoginStore
	// backupMu は完全バックアップの作成（読み取りロック）と復元（書き込みロック）を排他する。
	backupMu sync.RWMutex
	// remoteDirtyMu は公開サーバーへ未反映の設定（remote_dirty_settings）の読み書きを排他する。remoteDirtyRev は
	// 未反映として記録するたびに増え、反映の途中で記録された変更を、反映の完了で消さないために使う（handlers_remote.go）。
	remoteDirtyMu  sync.Mutex
	remoteDirtyRev uint64
	// remoteBulk は公開サーバーへの一括同期（handlers_remote_bulk.go）の状態。
	remoteBulk remoteBulkState
	// imports は WordPress の取り込み（handlers_import.go）の排他と、確認したファイルの預かり。
	imports importState
	// basicAuth はサイト閲覧制限（Basic 認証）の設定（basic_auth.go）。「サイト設定」で保存すると差し替えるため、
	// 要求の処理と競合しないよう atomic に読み書きする。デスクトップモードでは読み込んでも働かない（basicAuthApplies）。
	basicAuth atomic.Pointer[basicAuthGate]
	// basicAuthMu は閲覧制限の設定の書き込み（サイト設定の保存・同期 API の set_site_access・復元後の読み直し）を排他し、
	// DB への保存とメモリ（basicAuth）への反映を同じ順序にする。
	basicAuthMu sync.Mutex
	// maintenance は公開サーバーのメンテナンス表示をしているか（maintenance.go）。同期 API の set_maintenance で切り替え、
	// 要求の処理と競合しないよう atomic に読み書きする。デスクトップモードでは働かない（maintenanceApplies）。
	maintenance atomic.Bool
	// maintenancePage は訪問者に出す画面（標準または独自 HTML）。maintenance と同じく atomic に読み、書き込みは
	// maintenanceMu の中で DB に保存してから差し替える。
	maintenancePage atomic.Pointer[maintenancePage]
	// maintenanceMu はメンテナンス表示の設定の書き込み（set_maintenance・復元後の読み直し）を排他し、DB への保存とメモリ
	// （maintenance・maintenancePage）への反映を同じ順序にする。
	maintenanceMu sync.Mutex
	// siteAccessDialer は「公開サーバーの閲覧制限」で、公開サーバーが返したサイトの URL を外から開いて確かめるときの
	// 接続（handlers_remote_site_access.go）。公開されたアドレスにだけ接続する。零値で使い、試験だけが差し替える。
	siteAccessDialer publicDialer
	// syncKey は同期 API のトークンを検証する公開鍵（Config.SyncPublicKey または RegisterSyncAPI で渡した鍵）。
	// 閲覧制限を有効にした公開サーバーで、PC がトークンで画像を取得するときにも使う（syncTokenReadsUpload）。
	syncKey atomic.Pointer[ed25519.PublicKey]

	// cookies はこのサイトの Cookie の名前（Config.SiteKey から決める）。
	cookies auth.CookieNames
	// allowedHosts は Config.AllowedHosts を小文字にそろえたもの（normalizeAllowedHosts）。proxies は
	// Config.TrustedProxies を解析した範囲（ParseTrustedProxies）。転送ヘッダーを信じるかは、公開サイトの URL・
	// OAuth・招待コードのオリジン、回数の制限（oauthLimits）、MCP サーバー（mcp.Config.TrustedProxies）のどれも
	// この1つの一覧で決める（環境変数 TRUSTED_PROXIES は直接読まない）。どちらも New の後は変えない。
	allowedHosts []string
	proxies      mcp.TrustedProxies
	// startCare はデスクトップモードの自動バックアップと新しい版の確認を始める。stopCare はそれを止め、careDone は
	// 自動バックアップの goroutine が終わると閉じる。どれもデスクトップモードでなければ nil。
	startCare func()
	stopCare  context.CancelFunc
	careDone  chan struct{}
	// bgMu は StopBackground・ResumeBackground・Shutdown の、バックグラウンドの仕事の停止と再開を排他する。
	// bgStopped は止めているか、shutDown は Shutdown を始めたか、reflectPending は止めたときに予約投稿による
	// 反映を取り消したか。
	bgMu           sync.Mutex
	bgStopped      bool
	shutDown       bool
	reflectPending bool
	// publishHealthWG is registered under bgMu so StopBackground and Shutdown can
	// prevent new checks before waiting for current checks to finish.
	publishHealthWG sync.WaitGroup
	// previewTokens はデスクトップモードで下書きを既定のブラウザで開く、使い捨てのプレビューのトークン（desktop.go）。
	previewTokens previewTokenStore
	// accessLogMu はアクセスログ（accessLogMiddleware）の1行ずつの書き込みを、同時に処理している要求のあいだで排他する。
	accessLogMu sync.Mutex
}

// New は cfg に従ってルーティングとミドルウェアを設定した Server を返す。
// カテゴリが1件も無ければ、記事を作成できるよう既定のカテゴリ「未分類」を登録する。
func New(cfg Config) (*Server, error) {
	if cfg.DB == nil {
		return nil, errors.New("server: nil DB")
	}
	if cfg.SyncPublicKey != nil {
		if len(cfg.SyncPublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("server: sync public key: %w", auth.ErrInvalidKey)
		}
		// 呼び出し側が鍵のスライスを書き換えても検証に影響しないよう複製する。
		cfg.SyncPublicKey = slices.Clone(cfg.SyncPublicKey)
	}
	if cfg.SyncSigningKey != nil {
		// 長さの異なる秘密鍵では Public が panic するため、長さを先に確かめる。
		if len(cfg.SyncSigningKey) != ed25519.PrivateKeySize ||
			!cfg.SyncSigningKey.Public().(ed25519.PublicKey).Equal(cfg.SyncPublicKey) {
			return nil, fmt.Errorf("server: sync signing key: %w (it must pair with SyncPublicKey)", auth.ErrInvalidKey)
		}
		cfg.SyncSigningKey = slices.Clone(cfg.SyncSigningKey)
	}
	if !validSiteKey(cfg.SiteKey) {
		return nil, fmt.Errorf("server: invalid site key %q: use lowercase letters and digits", cfg.SiteKey)
	}
	if cfg.SyncAPIPath == "" {
		cfg.SyncAPIPath = DefaultSyncAPIPath
	} else {
		p, err := NormalizeSyncAPIPath(cfg.SyncAPIPath)
		if err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
		cfg.SyncAPIPath = p
	}
	mcpPath, err := resolveMCPAPIPath(cfg.MCPAPIPath, cfg.PublicOnly, cfg.SyncAPIPath)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	cfg.MCPAPIPath = mcpPath
	if cfg.UploadDir == "" {
		cfg.UploadDir = DefaultUploadDir
	}
	if cfg.SessionDuration <= 0 {
		cfg.SessionDuration = DefaultSessionDuration
		if cfg.Desktop {
			cfg.SessionDuration = DesktopSessionDuration
		}
	}
	if cfg.LockTTL <= 0 {
		cfg.LockTTL = lock.DefaultTTL
	}
	if cfg.MaxUploadSize <= 0 {
		cfg.MaxUploadSize = image.DefaultMaxUploadSize
	}
	applySavedSiteSettings(&cfg)
	// 閲覧制限を有効にしたサイトを、設定を読めないまま守らずに配信しないよう、読めなければ起動しない。
	basicAuth, err := loadBasicAuthSettings(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("server: load basic auth settings: %w", err)
	}
	// メンテナンス表示中の公開サーバーが、設定を読めないまま公開サイトを配信し始めないよう、読めなければ起動しない。
	maintenance, err := loadMaintenanceState(context.Background(), cfg.DB)
	if err != nil {
		return nil, err
	}
	if cfg.SiteName == "" {
		cfg.SiteName = DefaultSiteName
	}
	if cfg.AttachmentDir == "" {
		cfg.AttachmentDir = DefaultAttachmentDir
	}
	if cfg.MaxAttachmentSize <= 0 {
		cfg.MaxAttachmentSize = DefaultMaxAttachmentSize
	}
	if cfg.MaxRestoreSize <= 0 {
		cfg.MaxRestoreSize = DefaultMaxRestoreSize
	}
	if cfg.ThemesDir == "" {
		cfg.ThemesDir = DefaultThemesDir
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	allowedHosts, err := normalizeAllowedHosts(cfg.AllowedHosts)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	trustedProxies, err := ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	proxies := mcp.NewTrustedProxies(trustedProxies...)
	if err := os.MkdirAll(cfg.UploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("server: create upload dir: %w", err)
	}
	// 添付資料は他のユーザーやプロセスから読めないよう、所有者とグループだけに権限を与える。
	if err := os.MkdirAll(cfg.AttachmentDir, 0o750); err != nil {
		return nil, fmt.Errorf("server: create attachment dir: %w", err)
	}
	if err := ensureDefaultCategory(cfg.DB); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}

	// サーバー起動時に、過去のクラッシュ等で残った古い作業ディレクトリ（30分以上経過）を安全に掃除する。
	// 置き換え前のテーマ（.old）を含むものは消さずに残す（戻しもしない。CleanupStaleStaging）。
	if cfg.ThemesDir != "" {
		theme.CleanupStaleStaging(cfg.ThemesDir, 30*time.Minute)
	}
	// 壊れたカスタムテーマは LoadComposite が飛ばすため、ここで失敗するのはビルトインの不具合か、置き場所を読めないとき。
	themes, err := theme.LoadComposite(static.FS, cfg.ThemesDir)
	if err != nil {
		return nil, fmt.Errorf("server: load themes: %w", err)
	}

	r, err := newRenderer(templates.FS)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	ogp, err := social.NewGenerator(social.DefaultCacheSize)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}

	idx, ownsSearch := cfg.Search, false
	if idx == nil {
		if idx, err = search.NewMemIndex(); err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
		ownsSearch = true
	}
	// 起動時に公開中の記事をインデックスへ揃える（前回終了後の DB の変更も反映する）。
	if err := idx.SyncFromDB(context.Background(), cfg.DB); err != nil {
		if ownsSearch {
			idx.Close()
		}
		return nil, fmt.Errorf("server: %w", err)
	}
	// AI 検索（RAG）の埋め込みも、読者に見えている記事のものだけを残す。埋め込みを公開中の記事だけに作るようにする前に
	// 作られた下書きなどの埋め込みを消す（毎回の起動で行っても、消すものが無ければ何も変わらない）。
	if _, err := cfg.DB.Exec(`DELETE FROM article_embeddings WHERE article_id NOT IN (SELECT a.id FROM articles a WHERE `+
		article.VisibleCondition+`)`, article.StatusPublished, clockNow().UTC()); err != nil {
		if ownsSearch {
			idx.Close()
		}
		return nil, fmt.Errorf("server: remove embeddings of unpublished articles: %w", err)
	}

	if cfg.AI == nil {
		cfg.AI = ai.NewDummy("")
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Renderer = r
	if cfg.LogOutput != nil {
		e.Logger.SetOutput(cfg.LogOutput)
	}
	// echo.New の既定は ERROR で、ハンドラーの Warnf（AI API の生のエラーなど）が sorus.log に残らない。
	// 利用者の画面には分かりやすい文だけを出し、原因の調べに要る詳細はログに残すため INFO にする。
	e.Logger.SetLevel(log.INFO)
	// c.RealIP()（PV の重複判定など）は、既定では直接つながった送り元のアドレスだけを使う。echo の既定（IPExtractor
	// が nil）は誰でも書ける X-Forwarded-For・X-Real-IP をそのまま信じるため、TrustedProxies を指定したときだけ
	// そのプロキシが付け足した X-Forwarded-For を読む（newIPExtractor）。
	e.IPExtractor = newIPExtractor(trustedProxies)
	SetTimeouts(e.Server)

	// 同期 API のパスの先頭部分は設定で変わるため、固定の予約語に加えてページのスラッグに使えなくする
	// （管理画面・同期・MCP の保存と公開で同じ規則を使う）。
	reservedSlugs := []string{article.PathReservedSlug(cfg.SyncAPIPath), article.PathReservedSlug(cfg.MCPAPIPath)}
	s := &Server{
		echo:       e,
		db:         cfg.DB,
		cfg:        cfg,
		articles:   article.NewService(cfg.DB, article.WithReservedSlugs(reservedSlugs...)),
		locker:     lock.NewLocker(cfg.DB, cfg.LockTTL),
		search:     idx,
		ownsSearch: ownsSearch,
		redirects:  newRedirectCache(cfg.DB, redirectCacheTTL),
		ragService: cfg.RAG,
		revisions:  revision.NewService(cfg.DB),
		themes:     themes,
		ragIndexer: newRAGIndexer(),
		related:    newRelatedCache(relatedCacheTTL),
		startedAt:  time.Now(),
		ogp:        ogp,
		cookies:    auth.CookieNamesFor(cfg.SiteKey),

		allowedHosts: allowedHosts,
		proxies:      proxies,
	}
	s.setBasicAuth(basicAuth)
	s.maintenance.Store(maintenance.Enabled)
	s.setMaintenancePage(maintenance.Page)
	// 管理画面で保存した AI の設定があれば、それを環境変数（cfg.AI）より優先する（handlers_settings_ai.go）。
	s.setAIProvider(s.providerFromSavedAI(e.Logger))
	allowedOrigins := cfg.MCPAllowedOrigins
	if cfg.BaseURL != "" {
		allowedOrigins = append([]string{cfg.BaseURL}, allowedOrigins...)
	}
	s.oauth = mcp.NewOAuthStore(cfg.DB)
	s.oauthLimits = newOAuthRateLimits(proxies)
	s.oauthLogins = newOAuthLoginStore()
	s.mcp, err = mcp.New(mcp.Config{
		DB:       cfg.DB,
		BasePath: cfg.MCPAPIPath,
		// 同期 API のトークン（招待）で作られたユーザー（公開サーバーで確認コードから許可した連携・PC から発行した
		// トークン）は、元の招待が取り消された・期限が切れたら使えなくする（syncAwareTokenStore）。
		Tokens: &syncAwareTokenStore{db: cfg.DB, inner: mcp.NewDBTokenStore(cfg.DB)},
		Search: idx,
		RAG:    cfg.RAG,
		AI:     cfg.AI,
		// 管理画面で AI の設定を保存したら、MCP の cms_proofread も新しい設定で動く。
		AISource:           s.aiProvider,
		SiteName:           cfg.SiteName,
		SiteDescription:    cfg.SiteDescription,
		BaseURL:            cfg.BaseURL,
		GetBaseURL:         s.getBaseURL,
		GetSiteName:        s.getSiteName,
		GetSiteDescription: s.getSiteDescription,
		LockTTL:            cfg.LockTTL,
		// 管理画面の無い公開サーバーでは、開けない編集画面の URL を返さず、変更案は PC の sorus で確かめるよう案内する。
		PublicOnly: cfg.PublicOnly,
		// MCP で保存・校閲申請した記事も、管理画面で保存したときと同じく検索インデックスと RAG に反映する。
		OnArticleChanged:   func(id string) { s.reindex(e.Logger, id) },
		PublishSite:        s.mcpPublishSite,
		GetPublishStatus:   s.mcpGetPublishStatus,
		CheckPublishHealth: func(ctx context.Context) mcp.PublishHealthResult { return s.mcpCheckPublishHealth(ctx) },
		OnArticlePublished: func() { s.reflectAfterPublish() },
		// MCP の記事の取り下げ・サイト構成・メディアライブラリも、管理画面と同じ検査・保存・後処理を使う（mcp_bridge.go）。
		OnArticleUnpublished: s.mcpArticleUnpublished,
		GetSiteStructure:     s.mcpSiteStructure,
		UpdateSiteStructure:  s.mcpUpdateSiteStructure,
		ListMedia:            s.mcpListMedia,
		// MCP のテーマ操作も管理画面と同じカタログ・排他を使い、変更後は記事の公開と同じく静的サイトへ反映する。
		ThemesDir:      cfg.ThemesDir,
		ThemeCatalog:   s.themes,
		ThemeMu:        &s.themeMu,
		OnThemeChanged: func() { s.reflectAfterPublish() },
		AllowedOrigins: allowedOrigins,
		// DNS リバインディング対策: デスクトップモードはこの PC の中だけで使うため、/api/mcp の Host ヘッダーを
		// この PC 自身（localhost・127.0.0.1・[::1]）と MCPAllowedOrigins のホストに限る。サーバーモードは社内 LAN の
		// IP アドレスやホスト名（http://192.168.x.x:8080・http://sorus.local:8080）でも使えるよう、ALLOWED_HOSTS が
		// 無ければ Host は書式だけを確かめ、ALLOWED_HOSTS があればその一覧に限る（応答に載せるオリジンも同じ）。
		// どちらのモードでも、Origin ヘッダー付き（ブラウザから）の要求は許可したオリジンからだけ受け付ける。
		LoopbackOnly:      cfg.Desktop,
		AllowedHosts:      mcpAllowedHosts(cfg, allowedHosts),
		ReservedPageSlugs: reservedSlugs,
		// 転送ヘッダーは管理画面・OAuth と同じ一覧（Config.TrustedProxies）で信じるか決める。
		TrustedProxies: &proxies,
	})
	if err != nil {
		if ownsSearch {
			idx.Close()
		}
		return nil, fmt.Errorf("server: %w", err)
	}
	s.analytics = cfg.Analytics
	if s.analytics == nil {
		s.analytics, s.ownsAnalytics = analytics.NewService(cfg.DB,
			analytics.WithLocation(s.timeLocation()), analytics.WithClock(func() time.Time { return clockNow() })), true
	}
	s.routes()
	if !cfg.DisableScheduler {
		s.startScheduler()
	}
	return s, nil
}

// permissionsPolicy は、管理画面・公開サイトとも使わないブラウザの機能（カメラ・マイク・位置情報）を、
// 埋め込まれた第三者のスクリプトやフレームも含めて使えなくする Permissions-Policy。
const permissionsPolicy = "camera=(), microphone=(), geolocation=()"

// permissionsPolicyMiddleware は応答に Permissions-Policy を付ける。ハンドラが独自の値を付けた場合はそれを優先する。
func permissionsPolicyMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Response().Header()
		if h.Get("Permissions-Policy") == "" {
			h.Set("Permissions-Policy", permissionsPolicy)
		}
		return next(c)
	}
}

// startScheduler は予約投稿の自動公開ワーカーをバックグラウンドで起動する。Shutdown で停止する。
func (s *Server) startScheduler() {
	s.scheduler = scheduler.New(s.db, s.search, s.cfg.SchedulerInterval)
	s.scheduler.SetOnPublished(s.onScheduledPublished)
	// ゴミ箱に入れてから30日たった記事は、スケジューラが1日ごとに完全削除する。添付資料のファイルと
	// 検索インデックスの後片付けは、管理画面からの完全削除と同じく afterPurge で行う。
	s.scheduler.SetOnTrashPurged(func(res article.PurgeResult) { s.afterPurge(s.echo.Logger, res) })
	ctx, cancel := context.WithCancel(context.Background())
	s.stopScheduler = cancel
	s.schedulerDone = make(chan struct{})
	go func() {
		defer close(s.schedulerDone)
		_ = s.scheduler.Run(ctx)
	}()
}

// Handler は http.Server などに渡す HTTP ハンドラを返す。
func (s *Server) Handler() http.Handler { return s.echo }

// Start は addr で HTTP サーバーを起動し、停止するまでブロックする。
func (s *Server) Start(addr string) error { return s.echo.Start(addr) }

// Serve は待ち受け済みの ln で HTTP サーバーを動かし、Shutdown で止まるまでブロックする（停止後は
// http.ErrServerClosed を返す）。guard が nil でなければ、すべての要求をまず guard で包んだハンドラーに通す
// （デスクトップモードの Host ヘッダーの検査）。読み書きのタイムアウトは Start と同じ。
func (s *Server) Serve(ln net.Listener, guard func(http.Handler) http.Handler) error {
	var h http.Handler = s.echo
	if guard != nil {
		h = guard(h)
	}
	s.echo.Server.Handler = h
	s.echo.Server.ErrorLog = s.echo.StdLogger
	return s.echo.Server.Serve(ln)
}

// Shutdown は処理中のリクエストを待ってサーバーを停止する。バックグラウンドの埋め込みの更新と
// 予約投稿ワーカーを止め、New が作成した検索インデックスも閉じる。New が作成した PV 集計は、
// メモリ上の未保存の集計を DB に書き出してから閉じる。
func (s *Server) Shutdown(ctx context.Context) error {
	s.bgMu.Lock()
	// 止め始めたら、処理中の要求を待っているあいだも、ResumeBackground で動かし直さない。
	s.shutDown = true
	stopScheduler, schedulerDone := s.stopScheduler, s.schedulerDone
	s.bgMu.Unlock()
	s.leaveUpdateCheck()
	// 終わらない MCP の SSE ストリームを先に閉じ、echo.Shutdown が待ち続けないようにする。
	s.mcp.Close()
	err := s.echo.Shutdown(ctx)
	// 確認したまま取り込まれなかった WordPress のファイルを消す。
	s.imports.clear()
	if s.ownsAnalytics {
		if aerr := s.analytics.Close(); aerr != nil {
			err = errors.Join(err, fmt.Errorf("server: flush analytics: %w", aerr))
		}
	}
	if rerr := s.stopRAGIndex(ctx); rerr != nil {
		err = errors.Join(err, fmt.Errorf("server: stop rag indexing: %w", rerr))
	}
	if healthErr := waitPublishHealth(ctx, &s.publishHealthWG); healthErr != nil {
		err = errors.Join(err, fmt.Errorf("server: stop publish health check: %w", healthErr))
	}
	schedulerTimedOut := false
	if stopScheduler != nil {
		stopScheduler()
		// 公開処理の途中で検索インデックスを閉じないよう、ワーカーの終了を待つ。
		select {
		case <-schedulerDone:
		case <-ctx.Done():
			schedulerTimedOut = true
			err = errors.Join(err, fmt.Errorf("server: stop scheduler: %w", ctx.Err()))
		}
	}
	if s.ownsSearch && !schedulerTimedOut {
		err = errors.Join(err, s.search.Close())
	}
	return err
}

func (s *Server) routes() {
	e := s.echo
	e.HTTPErrorHandler = s.httpErrorHandler
	// サーバーモード（公開専用モードを含む）では、閲覧制限の 401・メンテナンス画面・転送ルールのリダイレクトも残るよう、
	// アクセスログをどのミドルウェアよりも外側（Pre の先頭）に置く。デスクトップモードは自分の PC の中だけで使うため記録しない。
	if !s.cfg.Desktop {
		e.Pre(s.accessLogMiddleware())
	}
	// 基本のセキュリティヘッダー（securityHeadersConfig）は、閲覧制限の 401・メンテナンス画面・転送ルールのリダイレクト・
	// 公開専用モードの /admin の 404 にも付けるよう、アクセスログを除くほかのどのミドルウェアよりも先（Pre の先頭）に置く。
	e.Pre(middleware.SecureWithConfig(securityHeadersConfig(s.cfg.SecureCookie)))
	// echo の SecureConfig に無い Permissions-Policy も、同じくすべての応答に付ける。
	e.Pre(permissionsPolicyMiddleware)
	// 表示言語の Cookie の名前はサイトごとに違うため、要求ごとに渡す（language）。どの画面よりも先に入れる。
	e.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(langCookieContextKey, s.cookies.Lang)
			return next(c)
		}
	})
	// サイト閲覧制限（Basic 認証）は、転送ルールのリダイレクトからも公開サイトの内容が分からないよう、その前に判定する。
	e.Pre(s.basicAuthMiddleware())
	// 公開サーバーのメンテナンス表示（maintenance.go）は閲覧制限の後（パスワードを知らない人には 401 だけを返す）、転送ルールの
	// 前（メンテナンス中はリダイレクトも返さない）に判定する。公開専用モードの /admin も、メンテナンス中は公開サイトの 404 ページ
	// （テーマ・サイト名を含む）ではなくメンテナンス画面にするため、次の遮断より前に置く（閲覧制限は /admin を対象にしないため、
	// 遮断を閲覧制限の後に置いても、メンテナンス表示をしていないときの応答は変わらない）。
	e.Pre(s.maintenanceMiddleware())
	// 公開専用モードの /admin の遮断は、管理画面へのパスを転送先に持つリダイレクトよりも先に判定する。
	e.Pre(PublicGuardMiddleware(s.cfg.PublicOnly))
	e.Pre(s.redirectMiddleware())
	e.Use(middleware.Recover())
	// 添付資料のアップロード・バックアップからの復元・WordPress の取り込みだけは上限を引き上げる。CSRF ミドルウェアが
	// フォームを読む前に制限するため、ルート単位ではなく全体のミドルウェアで振り分ける。
	attachmentLimit := fmt.Sprintf("%dB", s.cfg.MaxAttachmentSize+multipartOverhead)
	restoreLimit := fmt.Sprintf("%dB", s.cfg.MaxRestoreSize+multipartOverhead)
	importLimit := fmt.Sprintf("%dB", maxImportSize+multipartOverhead)
	// 同期 API は記事の添付資料（sync_attachments）を Base64 で1件ずつ受け取るため、添付資料1件の上限の 4/3 倍に JSON の分を
	// 足した値まで受け付ける（トークンを確かめてから本文を読む。syncHandler）。
	syncLimit := fmt.Sprintf("%dB", max(int64(20<<20), s.cfg.MaxAttachmentSize/3*4+4*multipartOverhead))
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{Limit: bodyLimit,
		Skipper: func(c echo.Context) bool {
			return isAttachmentUpload(c) || isBackupRestore(c) || isImportUpload(c) || s.isSyncAPIRequest(c)
		}}))
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{Limit: syncLimit,
		Skipper: func(c echo.Context) bool { return !s.isSyncAPIRequest(c) }}))
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{Limit: attachmentLimit,
		Skipper: func(c echo.Context) bool { return !isAttachmentUpload(c) }}))
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{Limit: restoreLimit,
		Skipper: func(c echo.Context) bool { return !isBackupRestore(c) }}))
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{Limit: importLimit,
		Skipper: func(c echo.Context) bool { return !isImportUpload(c) }}))
	// 復元の本文はログインを確かめる前に読ませない（/admin グループの CSRF ミドルウェアより先に照合する）。
	e.Use(s.requireRestoreCSRFHeader)
	// 取り込みの本文も、管理者のログインを確かめてから読ませる。
	e.Use(s.requireImportAdmin)
	e.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		// WebP・PDF・ZIP などは圧縮済みのため再圧縮しない。添付資料は Range 要求にも応じる。
		Skipper: func(c echo.Context) bool {
			// OGP 画像（PNG）とバックアップの Zip も圧縮済み。MCP の SSE ストリームは逐次送るため圧縮でバッファさせない。
			p := c.Request().URL.Path
			return strings.HasPrefix(p, UploadURLPrefix) || strings.HasPrefix(p, AttachmentURLPrefix) ||
				strings.HasPrefix(p, "/admin/attachments/") || strings.HasPrefix(p, OGImageURLPrefix) ||
				s.isMCPPath(p) || isBackupTransferPath(p) || p == WindowPath
		},
	}))
	// CDN を前段に置けるよう、静的アセットには不変キャッシュ、公開ページには CDN 向けの Cache-Control と ETag を付ける。
	// 公開ページの ETag は圧縮前の本文から作るため Gzip の内側に置く。
	e.Use(StaticCacheMiddleware())
	// 開いている画面の接続（WebSocket）は応答を乗っ取って双方向に送るため、本文を溜める対象から外す。
	// BaseURL が無いサイトの公開ページは Host ヘッダーから絶対 URL を組み立てるため、Host ごとにキャッシュを分けさせる
	// （偽の Host で作らせたページを、ほかの訪問者へ返させない）。
	// 使い捨てのプレビュー（PreviewTokenPath）は1回しか開けず、キャッシュもさせないため対象外にする。
	e.Use(PublicHTMLCacheMiddlewareWithConfig(PublicHTMLCacheConfig{
		Excluded: []string{s.cfg.SyncAPIPath, WindowPath, PreviewTokenPath},
		VaryHost: func() bool { return s.getBaseURL() == "" },
	}))
	// 公開ページには多層防御の基本 CSP を付与する。
	e.Use(s.publicCSPMiddleware())
	// 管理画面・はじめに・OAuth の同意画面には、外部への送信・埋め込みを絞った CSP を付ける（adminCSP）。
	e.Use(s.adminCSPMiddleware())
	// 閲覧制限で守った応答は CDN にキャッシュさせない。上の2つが付ける Cache-Control より後に決めるため、その内側に置く。
	e.Use(basicAuthPrivateCache)

	// 公開ページ・静的アセットは HEAD にも応じる。CDN・クローラー・監視は応答ヘッダー（Cache-Control・ETag）だけを
	// HEAD で確かめることがあり、SNS のクローラーは画像の有無を HEAD で確かめることがある（HEAD は PV に数えない）。
	// 添付資料のダウンロードは HEAD でダウンロード数を増やさないよう GET だけにする。
	getHead := []string{http.MethodGet, http.MethodHead}
	e.Match(getHead, "/static*", echo.StaticDirectoryHandler(static.FS, false))
	e.Match(getHead, UploadURLPrefix+"*", echo.StaticDirectoryHandler(os.DirFS(s.cfg.UploadDir), false), uploadCSPMiddleware)
	e.Match(getHead, CustomThemeURLPrefix+":id/*", s.handleCustomThemeAsset)

	e.Match(getHead, "/", s.handlePublicIndex)
	e.Match(getHead, articlesListPath, s.handlePublicArticles)
	e.Match(getHead, "/articles/:slug", s.handlePublicArticle)
	e.Match(getHead, "/categories/:slug", s.handlePublicCategory)
	e.Match(getHead, "/search", s.handlePublicSearch)
	// :::gallery の「もっと見る」の続きの写真（handlers_gallery_api.go）。
	e.Match(getHead, galleryAPIPath, s.handlePublicGalleryAPI)
	e.Match(getHead, "/feed.xml", s.handleFeed)
	e.Match(getHead, "/sitemap.xml", s.handleSitemap)
	e.Match(getHead, "/robots.txt", s.handleRobots)
	e.Match(getHead, "/favicon.ico", s.handleFavicon)
	e.Match(getHead, "/llms.txt", s.handleLLMSTxt)
	e.Match(getHead, "/llms-full.txt", s.handleLLMSFullTxt)
	e.GET(AttachmentURLPrefix+":id/download", s.handleAttachmentDownload)
	e.GET("/healthz", s.handleHealth)
	e.Match(getHead, "/manifest.json", s.handleManifest)
	e.Match(getHead, "/sw.js", s.handleServiceWorker)
	e.Match(getHead, OGImageURLPrefix+":file", s.handleOGImage)
	// 開いている画面の接続は、デスクトップモードのときだけ登録する（それ以外は無いページと同じ 404）。
	if s.cfg.Desktop && !s.cfg.PublicOnly {
		s.registerWindowRoute()
		// 下書きを OS の既定のブラウザで公開デザインのまま確かめる使い捨てのプレビュー（handlers_public.go）も、
		// デスクトップモードのときだけ登録する。
		e.GET(PreviewTokenPath+"/:token", s.handleTokenThemePreview)
	}
	// 多言語対応（例: en）。基本設定で多言語対応を OFF にしたサイト（isMultilingualEnabled）では、英語のページは
	// 存在しないページと同じ 404 にする（requireMultilingual）。
	for _, lang := range []string{"en"} {
		prefix := "/" + lang
		ml := s.requireMultilingual
		e.Match(getHead, prefix, s.handlePublicIndex, ml)
		e.Match(getHead, prefix+"/", s.handlePublicIndex, ml)
		e.Match(getHead, prefix+articlesListPath, s.handlePublicArticles, ml)
		e.Match(getHead, prefix+"/articles/:slug", s.handlePublicArticle, ml)
		e.Match(getHead, prefix+"/categories/:slug", s.handlePublicCategory, ml)
		e.Match(getHead, prefix+"/search", s.handlePublicSearch, ml)
		e.Any(prefix+"/:slug", func(c echo.Context) error {
			if m := c.Request().Method; m != http.MethodGet && m != http.MethodHead {
				return echo.ErrNotFound
			}
			return s.handlePublicPage(c)
		}, ml)
	}

	// 固定ページ（/<slug>）は固定のパスをすべて登録した後に置く。Echo は固定のパスを優先するため、予約語
	// （article.IsReservedPageSlug）と同じ名前のパスはページより先に既存の機能が受ける。無いページは 404 ページ。
	// 末尾のパラメータは / を含む残りのパス（/api/sync など）にも一致するため、全メソッドで受けて GET・HEAD 以外は
	// ページのルートが無かったときと同じ 404 にする（405 にすると、秘匿した同期 API のパスの有無などで応答が変わる）。
	e.Any("/:slug", func(c echo.Context) error {
		if m := c.Request().Method; m != http.MethodGet && m != http.MethodHead {
			return echo.ErrNotFound
		}
		return s.handlePublicPage(c)
	})

	// 同期 API はケーパビリティ・トークンで認証・認可する API のため、管理画面の Cookie セッション・CSRF の外に置く。
	// 登録するのは Config.SyncAPIPath だけで、秘匿パスを使うときは固定の /api/sync は存在しないページと同じ 404 になる。
	if s.cfg.SyncPublicKey != nil {
		s.setSyncPublicKey(s.cfg.SyncPublicKey)
		e.POST(s.cfg.SyncAPIPath, s.syncHandler(s.cfg.SyncPublicKey))
	}
	// MCP サーバーは Bearer トークンで認証する API のため、管理画面の Cookie セッション・CSRF の外に置く。
	// 登録するのは Config.MCPAPIPath だけ。公開専用モードは秘匿したパスだけに置き、固定の /api/mcp への無差別な
	// スキャンは存在しないページと同じ 404 になる。AI 機能を OFF にしたサイトでは、MCP も OAuth も 404 にする
	// （requireAIEnabled。外部の AI アプリから sorus を操作させない）。
	mcpHandler := echo.WrapHandler(s.mcp)
	e.Any(s.cfg.MCPAPIPath, mcpHandler, s.requireAIEnabled)
	e.Any(s.cfg.MCPAPIPath+"/*", mcpHandler, s.requireAIEnabled)
	// MCP の OAuth 2.1（ディスカバリー・クライアント登録・認可・トークン）。同意画面はサーバーモード・デスクトップモードでは
	// 管理画面のログインを、公開専用モードでは接続した PC で発行する確認コードを使う。
	s.registerOAuthRoutes(e)

	// 公開専用モードでは管理画面をインターネットに出さない（/admin への要求は PublicGuardMiddleware が 404 で返す）。
	if s.cfg.PublicOnly {
		return
	}
	// オフライン画面は未認証でも表示できるよう、認証・初期設定ミドルウェアの外に置く。
	e.GET("/admin/offline", s.handleOffline)

	adminMiddleware := []echo.MiddlewareFunc{middleware.CSRFWithConfig(middleware.CSRFConfig{
		TokenLookup:    "header:" + CSRFHeaderName + ",form:" + CSRFFormField,
		ContextKey:     csrfContextKey,
		CookieName:     s.cookies.CSRF,
		CookiePath:     "/admin",
		CookieSecure:   s.cfg.SecureCookie,
		CookieHTTPOnly: true,
		CookieSameSite: http.SameSiteStrictMode,
		// トークン欠落は既定で 400 になるため、不正なトークンと同じく 403 に揃える。
		ErrorHandler: func(err error, c echo.Context) error {
			return echo.NewHTTPError(http.StatusForbidden, "invalid or missing csrf token")
		},
	})}
	// デスクトップモードでは、最初のユーザーを作るまで /admin への要求をすべて「はじめに」画面へ送る。
	if s.cfg.Desktop {
		adminMiddleware = append(adminMiddleware, s.requireSetup)
	}
	admin := e.Group("/admin", adminMiddleware...)
	admin.GET("/login", s.handleLoginPage)
	admin.POST("/login", s.handleLogin)
	admin.GET("/set-lang", s.handleSetLanguage)
	if s.cfg.Desktop {
		s.registerSetupRoutes(admin)
	}

	authed := admin.Group("", auth.AuthMiddlewareWithConfig(s.db, auth.MiddlewareConfig{
		Secure:     s.cfg.SecureCookie,
		LoginPath:  auth.DefaultLoginPath,
		CookieName: s.cookies.Session,
	}))
	authed.GET("", s.handleDashboard)
	authed.GET("/", s.handleDashboard)
	authed.POST("/logout", s.handleLogout)
	if s.cfg.Desktop {
		s.registerDesktopRoutes(authed)
	}
	s.registerDesktopCareRoutes(admin, authed)

	// 権限ミドルウェア（auth.User.HasRole で判定する。管理者は編集者・ライターのすべての役割を持つため、どれも通る）
	// 記事の執筆・編集はライター・編集者（と管理者）。
	authorOnly := auth.RequireRole(auth.RoleEditor, auth.RoleWriter)
	// 記事の承認・差し戻しは編集者（と管理者）。ライターは不可。
	editorOnly := auth.RequireRole(auth.RoleEditor)
	// サイト設定・ユーザー管理は管理者専用。
	adminOnly := auth.RequireRole(auth.RoleAdmin)

	authed.GET("/articles", s.handleArticleList)
	authed.GET("/articles/:id", s.handleArticleView)
	authed.GET("/articles/new", s.handleArticleNew, authorOnly)
	authed.POST("/articles/new", s.handleArticleCreate, authorOnly)
	// AI 機能を OFF にしたサイトでは、AI が関わるルート（翻訳・/ai/*・RAG 検索・変更案・MCP の設定）を 404 にする。
	aiOnly := s.requireAIEnabled
	authed.POST("/articles/translate", s.handleArticleTranslate, aiOnly, authorOnly)
	authed.GET("/articles/:id/edit", s.handleArticleEdit, authorOnly)
	authed.POST("/articles/:id", s.handleArticleSave, authorOnly)
	authed.POST("/articles/:id/delete", s.handleArticleDelete, authorOnly)
	// 下書き・校閲待ち・予約中の記事も、適用中の公開テーマで確かめる（handlers_public.go）。
	authed.GET("/articles/:id/preview", s.handleArticleThemePreview, authorOnly)
	if s.cfg.Desktop {
		// デスクトップモードでは、同じプレビューを使い捨てのトークンで OS の既定のブラウザ（Chrome など）に開かせる（desktop.go）。
		authed.POST("/articles/:id/preview-open", s.handleDesktopPreviewOpen, authorOnly)
	}
	// ゴミ箱: 元に戻すのは執筆者も行える。完全削除・ゴミ箱を空にするのは取り消せないため編集者・管理者に限る。
	authed.POST("/articles/:id/restore", s.handleArticleRestore, authorOnly)
	authed.POST("/articles/:id/purge", s.handleArticlePurge, editorOnly)
	authed.POST("/articles/trash/empty", s.handleTrashEmpty, editorOnly)
	authed.POST("/preview", s.handlePreview, authorOnly)

	authed.POST("/articles/:id/lock", s.handleLockHeartbeat)
	authed.POST("/articles/:id/unlock", s.handleUnlock)
	authed.POST("/articles/:id/force-unlock", s.handleForceUnlock, auth.RequireRole(auth.RoleAdmin, auth.RoleEditor))

	authed.POST("/media/upload", s.handleMediaUpload, authorOnly)

	authed.POST("/ai/proofread", s.handleAIProofread, aiOnly, authorOnly)
	authed.POST("/ai/suggest-titles", s.handleAISuggestTitles, aiOnly, authorOnly)
	authed.POST("/ai/summarize", s.handleAISummarize, aiOnly, authorOnly)
	authed.POST("/ai/draft", s.handleAIDraft, aiOnly, authorOnly)
	authed.POST("/ai/fact-check", s.handleAIFactCheck, aiOnly, authorOnly)
	authed.POST("/ai/academic", s.handleAIAcademic, aiOnly, authorOnly)
	authed.POST("/ai/social-post", s.handleSocialPost, aiOnly, authorOnly)
	authed.GET("/api/rag/search", s.handleRAGSearch, aiOnly)

	// 共同編集ワークフロー
	authed.POST("/articles/:id/submit-review", s.handleSubmitReview, authorOnly)
	authed.POST("/articles/:id/reject-review", s.handleRejectReview, editorOnly)
	authed.POST("/articles/:id/approve-publish", s.handleApprovePublish, editorOnly)
	authed.GET("/articles/:id/comments", s.handleCommentList, authorOnly)
	authed.POST("/articles/:id/comments", s.handleCommentCreate, authorOnly)

	authed.GET("/articles/:id/revisions", s.handleRevisionList, authorOnly)
	authed.GET("/articles/:id/revisions/:num/diff", s.handleRevisionDiff, authorOnly)
	authed.POST("/articles/:id/revisions/:num/restore", s.handleRevisionRestore, authorOnly)
	// AI アプリ（MCP）から届いた変更案の破棄。反映は編集欄に入れてから、いつもの保存で行う（handlers_proposal.go）。
	authed.POST("/articles/:id/proposal/discard", s.handleProposalDiscard, aiOnly, authorOnly)

	authed.POST("/articles/:id/attachments", s.handleAttachmentUpload, authorOnly)
	authed.DELETE("/articles/:id/attachments/:aid", s.handleAttachmentDelete, authorOnly)
	authed.GET("/attachments/:aid/download", s.handleAdminAttachmentDownload, authorOnly)

	// MCP 連携（外部の AI エージェント用の API トークン）。各ユーザーが自分のトークンを管理する。
	authed.GET("/settings/mcp", s.handleMCPTokens, aiOnly)
	authed.POST("/settings/mcp", s.handleMCPTokenCreate, aiOnly)
	authed.POST("/settings/mcp/:id/revoke", s.handleMCPTokenRevoke, aiOnly)
	authed.POST("/settings/mcp/oauth/:id/revoke", s.handleOAuthAppRevoke, aiOnly)
	// 公開サーバー（同期 API で接続中）の MCP 連携。接続先とトークンはサイトで1つのため、ローカルの管理者だけが扱う
	// （ライターがローカルの admin のトークンで公開サーバーの admin の権限を得ないようにする。handlers_remote_mcp.go）。
	remoteMCPAdmin := auth.RequireRole(auth.RoleAdmin)
	s.registerRemoteMCPRoutes(authed, func(next echo.HandlerFunc) echo.HandlerFunc {
		return aiOnly(remoteMCPAdmin(next))
	})

	// 公開サーバー（このローカル CMS から公開サーバーの同期 API を呼ぶ）。
	s.registerRemoteRoutes(authed)
	s.registerPublishRoutes(authed)
	s.registerManuscriptRoutes(authed)
	s.registerSettingsHubRoutes(authed)
	s.registerCategoryRoutes(authed, auth.RequireRole(auth.RoleAdmin, auth.RoleEditor))

	// 管理者専用: テーマ設定と、適用前に公開ページをそのテーマで確かめるプレビュー
	authed.GET("/settings/theme", s.handleThemeSettings, adminOnly)
	authed.POST("/settings/theme", s.handleThemeSettingsSave, adminOnly)
	authed.POST("/settings/theme/card-images", s.handleCardImageSettingsSave, adminOnly)
	authed.GET("/settings/theme/preview", s.handleThemePreview, adminOnly)
	authed.POST("/settings/theme/install-url", s.handleThemeInstallURL, adminOnly)
	authed.POST("/settings/theme/install-zip", s.handleThemeInstallZip, adminOnly)
	authed.POST("/settings/theme/uninstall", s.handleThemeUninstall, adminOnly)
	// サイト構成（メニュー・トップページ）は管理者だけが変えられる。
	authed.GET("/settings/structure", s.handleStructureSettings, adminOnly)
	authed.POST("/settings/structure", s.handleStructureSettingsSave, adminOnly)

	// 管理者専用: サイト基本設定
	authed.GET("/settings/general", s.handleGeneralSettings, adminOnly)
	authed.POST("/settings/general", s.handleGeneralSettingsSave, adminOnly)
	// 管理者専用: AI 執筆アシスタントの設定と接続テスト
	s.registerAISettingsRoutes(authed, adminOnly)
	// 管理者専用: サイト運用（公開サーバーの閲覧制限・メンテナンス表示）
	s.registerOperationsRoutes(authed, adminOnly)

	// メディアライブラリ（DAM）: 一覧・メタデータ編集・ピッカー API はログイン中の全員、削除は管理者専用。
	s.registerMediaRoutes(authed, adminOnly)

	// 管理者専用: 完全バックアップの作成・ダウンロードと復元
	s.registerBackupRoutes(authed, adminOnly)
	// 管理者専用: WordPress の書き出しファイル（WXR）の取り込み
	s.registerImportRoutes(authed, adminOnly)
	// 管理者専用: サイトの一覧・追加・削除（デスクトップモードで Config.Sites があるときだけ）
	s.registerSitesRoutes(authed, adminOnly)

	// 管理者専用: ユーザー管理
	authed.GET("/users", s.handleUsersList, adminOnly)
	authed.POST("/users", s.handleUserCreate, adminOnly)
	authed.POST("/users/:id/edit", s.handleUserEdit, adminOnly)
	authed.POST("/users/:id/password", s.handleUserPassword, adminOnly)
	authed.POST("/users/:id/delete", s.handleUserDelete, adminOnly)
}

// SetTimeouts は hs に、低速なクライアントによる接続資源の占有（Slowloris 等）を防ぐ待ち時間の上限を付ける。
// 通常の配信（New の Echo のサーバー）と自動 HTTPS（main.go の newAutocertServers）の HTTPS サーバーで同じ値を使う。
// 長くかかる要求（同期・バックアップ・MCP のストリームなど）は、それぞれ http.ResponseController で期限を延ばす。
func SetTimeouts(hs *http.Server) {
	hs.ReadHeaderTimeout = 10 * time.Second
	hs.ReadTimeout = 30 * time.Second
	hs.WriteTimeout = 60 * time.Second
	hs.IdleTimeout = 120 * time.Second
}

// accessLogTimeFormat はアクセスログの time の書式（ミリ秒まで・時差付き）。
const accessLogTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// accessLogEntry はアクセスログの1行（JSON）。path は URL のパス（c.Request().URL.Path）だけにし、クエリ文字列
// （?t= などの秘密の値）や、Authorization・Cookie などのヘッダーは書かない。remote_ip は c.RealIP()
// （TRUSTED_PROXIES のプロキシが付けた X-Forwarded-For だけを信じる。newIPExtractor）。type は、同じ出力先に出る
// ハンドラーの警告・エラーの行と見分けるための印（常に "access"）。
type accessLogEntry struct {
	Time      string  `json:"time"`
	Type      string  `json:"type"`
	Method    string  `json:"method"`
	Path      string  `json:"path"`
	Status    int     `json:"status"`
	RemoteIP  string  `json:"remote_ip"`
	LatencyMS float64 `json:"latency_ms"`
	BytesOut  int64   `json:"bytes_out"`
}

// accessLogSkipped はアクセスログに残さない要求のパスかを返す。監視が繰り返し叩くヘルスチェックと、ページを開くたびに
// 読み込まれる静的アセット・カスタムテーマの CSS などは、調べたい要求（公開ページ・/admin・/oauth・/api など）を埋もれ
// させるため残さない。
func accessLogSkipped(p string) bool {
	return p == "/healthz" || p == "/static" || strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, CustomThemeURLPrefix)
}

// accessLogMiddleware はサーバーモードの HTTP アクセスログを、Echo のログの出力先（Config.LogOutput。省略時は標準出力で、
// systemd で動かすと journalctl -u sorus で読める）へ1要求1行の JSON で書く。項目は accessLogEntry。
// エラーを返したハンドラーの応答（404 ページ・401 など）も、エラーハンドラーで書き終えてから実際のステータスで記録する。
func (s *Server) accessLogMiddleware() echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		Skipper:         func(c echo.Context) bool { return accessLogSkipped(c.Request().URL.Path) },
		HandleError:     true,
		LogMethod:       true,
		LogURIPath:      true,
		LogStatus:       true,
		LogRemoteIP:     true,
		LogLatency:      true,
		LogResponseSize: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			line, err := json.Marshal(accessLogEntry{
				Time:      v.StartTime.Format(accessLogTimeFormat),
				Type:      "access",
				Method:    v.Method,
				Path:      v.URIPath,
				Status:    v.Status,
				RemoteIP:  v.RemoteIP,
				LatencyMS: float64(v.Latency.Microseconds()) / 1000,
				BytesOut:  v.ResponseSize,
			})
			if err != nil {
				return nil
			}
			line = append(line, '\n')
			s.accessLogMu.Lock()
			defer s.accessLogMu.Unlock()
			_, _ = s.echo.Logger.Output().Write(line)
			return nil
		},
	})
}

// originHosts はオリジン（https://cms.example.ac.jp など）の一覧から、ホスト名（ポートを除く）の一覧を返す。
// 読めないオリジンは飛ばす。
// mcpAllowedHosts は MCP サーバーの Host ヘッダーの許可リスト（mcp.Config.AllowedHosts）を返す。デスクトップモードは
// この PC 自身に加えて MCPAllowedOrigins のホスト、サーバーモードは ALLOWED_HOSTS（allowedHosts。空なら制限しない）。
func mcpAllowedHosts(cfg Config, allowedHosts []string) []string {
	if cfg.Desktop {
		return originHosts(cfg.MCPAllowedOrigins)
	}
	return allowedHosts
}

func originHosts(origins []string) []string {
	var hosts []string
	for _, o := range origins {
		if u, err := url.Parse(strings.TrimSpace(o)); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

// isSyncAPIRequest は同期 API（POST Config.SyncAPIPath）への要求かを返す。同期 API を登録していないサーバーでは常に false。
func (s *Server) isSyncAPIRequest(c echo.Context) bool {
	r := c.Request()
	return r.Method == http.MethodPost && s.syncPublicKey() != nil && r.URL.Path == s.cfg.SyncAPIPath
}

// isAttachmentUpload は添付資料のアップロード（POST /admin/articles/:id/attachments）への要求かを返す。
func isAttachmentUpload(c echo.Context) bool {
	r := c.Request()
	if r.Method != http.MethodPost {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/admin/articles/")
	if !ok {
		return false
	}
	id, tail, ok := strings.Cut(rest, "/")
	return ok && id != "" && tail == "attachments"
}

func init() {
	pageNames = append(pageNames, "access_denied")
}

// accessDeniedData は管理画面の「この操作を行う権限がありません」のページ（templates/admin/access_denied.html）に渡すデータ。
type accessDeniedData struct {
	// Detail はハンドラーが付けた理由。Echo の既定の文言（"Forbidden" など）なら空。
	Detail string
}

// httpErrorHandler は公開サイトの 404 を読者向けのページで返す。管理画面の 403・401 は adminDenied で返す。
// それ以外は Echo の既定の処理に任せる。
// 公開専用モードでは /admin への GET・HEAD も、存在しないページと見分けがつかないよう同じページで返す。
func (s *Server) httpErrorHandler(err error, c echo.Context) {
	var he *echo.HTTPError
	r := c.Request()
	if !c.Response().Committed && errors.As(err, &he) && (he.Code == http.StatusForbidden || he.Code == http.StatusUnauthorized) &&
		isAdminPath(r.URL.Path) {
		s.adminDenied(he, c)
		return
	}
	hidden := s.cfg.PublicOnly && (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(isAdminPath(r.URL.Path) || isAdminPath(r.URL.RawPath))
	if c.Response().Committed || !errors.As(err, &he) || he.Code != http.StatusNotFound || !(s.isPublicPath(r) || hidden) {
		s.echo.DefaultHTTPErrorHandler(err, c)
		return
	}
	lang := currentLang(c)
	// 多言語対応が OFF なら /en 以下も日本語の 404 ページにする（英語のトップ・検索へのリンクも 404 になるため）。
	if lang == "en" && !s.isMultilingualEnabled() {
		lang = "ja"
	}
	title := "ページが見つかりません"
	if lang == "en" {
		title = "Page Not Found"
	}
	p := s.newPublicPage(title, "", "")
	p.Lang = lang
	p.NoIndex = true
	if rerr := s.renderPublic(c, http.StatusNotFound, "not_found", p); rerr != nil {
		s.echo.DefaultHTTPErrorHandler(err, c)
	}
}

// adminDenied は管理画面で権限が無い（403）・ログインしていない（401）ときに応答する。ブラウザの画面遷移（HTML を求める
// GET・HEAD。HTMX を除く）には管理画面の見た目の案内ページを返し、画面内の fetch・HTMX にはこれまでどおり JSON を返す。
// どちらも、ハンドラーが理由を付けていなければ、Echo の既定の英語（"Forbidden" など）ではなく表示言語の文言にする。
func (s *Server) adminDenied(he *echo.HTTPError, c echo.Context) {
	r := c.Request()
	detail, _ := he.Message.(string)
	if strings.EqualFold(detail, http.StatusText(he.Code)) {
		detail = ""
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("HX-Request") != "true" &&
		strings.Contains(r.Header.Get(echo.HeaderAccept), echo.MIMETextHTML) {
		data := accessDeniedData{Detail: detail}
		if err := s.render(c, he.Code, "access_denied", localizedMessage(c, "access_denied_title"), data); err == nil {
			return
		}
	}
	msg := detail
	if msg == "" {
		key := "access_denied_title"
		if he.Code == http.StatusUnauthorized {
			key = "login_required"
		}
		msg = localizedMessage(c, key)
	}
	s.echo.DefaultHTTPErrorHandler(echo.NewHTTPError(he.Code, msg), c)
}

// isPublicPath は読者向けの HTML ページへの GET/HEAD か（管理画面・静的アセット・画像・API 以外か）を返す。
func (s *Server) isPublicPath(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	return !(p == "/admin" || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/static/") ||
		strings.HasPrefix(p, UploadURLPrefix) || strings.HasPrefix(p, OGImageURLPrefix) || s.isMCPPath(p) ||
		strings.HasPrefix(p, CustomThemeURLPrefix) ||
		p == "/healthz" || p == "/manifest.json" || p == "/sw.js")
}

// reindexArticle は記事 id の現在の状態を検索インデックスに反映する。公開中でなければインデックスから外す。
// 一時的なロック競合等で失敗した場合はバックグラウンドで最大3回リトライする。
// RAG の埋め込みは外部 API を呼ぶため、保存の応答を待たせないようバックグラウンドで更新する（scheduleRAGIndex）。
// 記事の変更は他の記事の関連記事にも影響するため、検索インデックスを更新したら関連記事のキャッシュをすべて捨てる。
func (s *Server) reindexArticle(c echo.Context, id string) {
	s.reindex(c.Logger(), id)
}

// reindex は reindexArticle の本体。リクエストの外（MCP で保存した記事）からも呼ぶため、ログの出力先を受け取る。
// AI 機能が OFF なら、埋め込みは作り直さない（scheduleRAGIndex が外部の Embeddings API を呼ばずに戻る）。
func (s *Server) reindex(logger echo.Logger, id string) {
	defer s.related.invalidate()
	s.scheduleRAGIndex(id)
	// RAG だけを設定した構成では検索インデックスが無い。以降の DeleteArticle・IndexArticle を呼ばない。
	if s.search == nil {
		return
	}
	doIndex := func() error {
		a, err := s.articles.GetByID(id)
		if errors.Is(err, article.ErrNotFound) {
			return s.search.DeleteArticle(id)
		} else if err != nil {
			return err
		}
		var names struct {
			Author   string `db:"author_name"`
			Category string `db:"category_name"`
		}
		err = s.db.Get(&names, `SELECT
			COALESCE((SELECT COALESCE(NULLIF(display_name, ''), username) FROM users WHERE id = ?), '') AS author_name,
			COALESCE((SELECT name FROM categories WHERE id = ?), '') AS category_name`, a.AuthorID, a.CategoryID)
		if err != nil {
			return err
		}
		return s.search.IndexArticle(a, names.Author, names.Category)
	}

	if err := doIndex(); err != nil {
		logger.Warnf("search: reindex article %s failed (will retry): %v", id, err)
		go func() {
			for attempt := 1; attempt <= 3; attempt++ {
				time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
				if err := doIndex(); err == nil {
					s.related.invalidate()
					return
				}
			}
			logger.Errorf("search: reindex article %s failed after retries", id)
		}()
	}
}

// csrfToken は現在のリクエストの CSRF トークンを返す。
func csrfToken(c echo.Context) string {
	token, _ := c.Get(csrfContextKey).(string)
	return token
}

// currentUser は認証ミドルウェアが格納したユーザーを返す。認証済みルートでのみ呼ぶこと。
func currentUser(c echo.Context) *auth.User {
	user, _ := auth.UserFromContext(c)
	return user
}

// applySavedSiteSettings は管理画面の「サイト基本設定」で保存した値を cfg に反映する。
// 保存済みの値は環境変数由来の値より優先し、再起動後や sorus export でも同じ設定を使う。
func applySavedSiteSettings(cfg *Config) {
	var rows []struct{ Key, Value string }
	if err := cfg.DB.Select(&rows, `SELECT key, value FROM settings
		WHERE key IN ('site_name', 'site_name_en', 'site_description', 'base_url', 'show_author') AND value != ''`); err != nil {
		return
	}
	for _, r := range rows {
		switch r.Key {
		case "site_name":
			cfg.SiteName = r.Value
		case "site_name_en":
			cfg.SiteNameEn = r.Value
		case "site_description":
			cfg.SiteDescription = r.Value
		case "base_url":
			cfg.BaseURL = r.Value
		case "show_author":
			cfg.ShowAuthor = (r.Value == "1" || r.Value == "true")
		}
	}
}

// ensureDefaultCategory はカテゴリが1件も無いときに「未分類」を登録する。
func ensureDefaultCategory(db *sqlx.DB) error {
	_, err := db.Exec(`INSERT INTO categories (name, slug)
		SELECT '未分類', 'uncategorized' WHERE NOT EXISTS (SELECT 1 FROM categories)`)
	if err != nil {
		return fmt.Errorf("ensure default category: %w", err)
	}
	return nil
}

// getBaseURL はメモリ上の BaseURL を排他して読み取る。
func (s *Server) getBaseURL() string {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.cfg.BaseURL
}

// setBaseURL はメモリ上の BaseURL を排他して更新する。
func (s *Server) setBaseURL(baseURL string) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.cfg.BaseURL = baseURL
}

// getSiteInfo はメモリ上の SiteName と SiteDescription を排他して読み取る。
func (s *Server) getSiteInfo() (name, desc string) {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.cfg.SiteName, s.cfg.SiteDescription
}

// getSiteName はメモリ上の SiteName を排他して読み取る。
func (s *Server) getSiteName() string {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.cfg.SiteName
}

// getSiteDescription はメモリ上の SiteDescription を排他して読み取る。
func (s *Server) getSiteDescription() string {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.cfg.SiteDescription
}

// setSiteInfo はメモリ上の SiteName と SiteDescription を排他して更新する。
func (s *Server) setSiteInfo(name, desc string) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.cfg.SiteName = name
	s.cfg.SiteDescription = desc
}

// setSiteName はメモリ上の SiteName を排他して更新する。
func (s *Server) setSiteName(name string) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.cfg.SiteName = name
}

// setSiteDescription はメモリ上の SiteDescription を排他して更新する。
func (s *Server) setSiteDescription(desc string) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.cfg.SiteDescription = desc
}

// getSiteNameEn はメモリ上の SiteNameEn を排他して読み取る。
func (s *Server) getSiteNameEn() string {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.cfg.SiteNameEn
}

// setSiteNameEn はメモリ上の SiteNameEn を排他して更新する。
func (s *Server) setSiteNameEn(name string) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.cfg.SiteNameEn = name
}

// getShowAuthor はメモリ上の ShowAuthor を排他して読み取る。
func (s *Server) getShowAuthor() bool {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.cfg.ShowAuthor
}

// setShowAuthor はメモリ上の ShowAuthor を排他して更新する。関連記事の全文検索（keywordScores）は
// 執筆者名を表示しない間は著者名で一致させないため、切り替えたら関連記事の推薦結果のキャッシュを捨てる。
func (s *Server) setShowAuthor(show bool) {
	s.siteMu.Lock()
	changed := s.cfg.ShowAuthor != show
	s.cfg.ShowAuthor = show
	s.siteMu.Unlock()
	if changed && s.related != nil {
		s.related.invalidate()
	}
}

// siteNameFor は言語 lang に応じたサイト名を返す。lang が "en" で SiteNameEn が設定されていれば
// それを優先使用し、未設定時は getSiteName() にフォールバックする。
func (s *Server) siteNameFor(lang string) string {
	if strings.ToLower(strings.TrimSpace(lang)) == "en" {
		if en := s.getSiteNameEn(); en != "" {
			return en
		}
	}
	return s.getSiteName()
}

// publicCSPMiddleware は公開ページのレスポンスヘッダーに多層防御となる基本 CSP を付与する。
// OAuth の同意画面（/oauth）は管理画面と同じ CSP（adminCSPMiddleware）にするため対象外にする。
func (s *Server) publicCSPMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p := c.Request().URL.Path
			if isPublicHTMLCachePath(p) && !isAdminCSPPath(p) {
				c.Response().Before(func() {
					h := c.Response().Header()
					if h.Get("Content-Security-Policy") == "" {
						h.Set("Content-Security-Policy", s.publicCSP())
					}
				})
			}
			return next(c)
		}
	}
}

const (
	// hstsMaxAge は HTTPS で配信するとき（Config.SecureCookie）に付ける Strict-Transport-Security の有効期間（1年）。
	hstsMaxAge = 31536000
	// loopbackHTTPSources はこの PC の http（どのポートでも）を表す CSP の送り先。CSP の書式では [::1] を書けない。
	loopbackHTTPSources = "http://localhost:* http://127.0.0.1:*"
	// uploadCSP は /uploads/ のファイル（利用者がアップロードした画像。SVG を含む）を直接開いたときの CSP。
	// <img> で読み込む画像には効かず、ファイルそのものを開いたときだけ効く。SVG に仕込んだスクリプトや外部の読み込みを
	// 動かさないよう、すべてを禁じた上でサンドボックス（スクリプト不可・別オリジン扱い）にする。
	uploadCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; sandbox"
)

// securityHeadersConfig はすべての応答に付ける基本のセキュリティヘッダーの設定を返す。echo の既定（X-XSS-Protection・
// X-Content-Type-Options: nosniff・X-Frame-Options: SAMEORIGIN）に Referrer-Policy を加え、ほかのサイトへ移るときは
// オリジンだけを送る（下書きのプレビューや管理画面の URL・検索語を外へ漏らさない）。secure（HTTPS で配信する本番）なら
// HSTS（1年）も付ける。echo は TLS で届いた要求（と X-Forwarded-Proto: https）にだけ HSTS を付け、平文の応答には付けない。
// 同じドメインのほかのサブドメインが平文で動いていることがあるため、includeSubDomains は付けない。
func securityHeadersConfig(secure bool) middleware.SecureConfig {
	cfg := middleware.DefaultSecureConfig
	cfg.ReferrerPolicy = "strict-origin-when-cross-origin"
	if secure {
		cfg.HSTSMaxAge = hstsMaxAge
		cfg.HSTSExcludeSubdomains = true
	}
	return cfg
}

// isAdminCSPPath は管理画面の CSP（adminCSP）を付けるパス（/admin・/setup・/oauth とその下）かを返す。
// 「はじめに」（SetupPath）は /admin の下にある。
func isAdminCSPPath(p string) bool {
	if isAdminPath(p) {
		return true
	}
	p = path.Clean("/" + p)
	for _, prefix := range []string{"/setup", "/oauth"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// adminCSP は管理画面・はじめに・OAuth の同意画面の Content-Security-Policy を返す。今の画面（テンプレートに直接書いた
// スクリプト・プレビュー・デスクトップモードの接続）を壊さない範囲で、最も厳しくする。
//   - script-src・style-src の 'unsafe-inline': 管理画面はテンプレートに直接書いたスクリプト・onclick などと、Tailwind
//     （static/vendor/tailwindcss）が実行時に差し込む <style> に頼るため外せない。外部の配信元と eval は許さない。
//   - connect-src 'self': fetch・sendBeacon・デスクトップモードの WebSocket（同じホスト）は自分のサーバーにだけ送る。
//     入力した内容を外部へ送らせない。
//   - img-src 'self' data: blob: https:: 本文の仕上がりプレビューは公開ページと同じく外部の https 画像を出し、
//     ファビコンの選択（URL.createObjectURL）は blob: を使う。
//   - frame-src: テーマのプレビュー（同じオリジン）と、本文の YouTube の埋め込み（youtube-nocookie）だけ。
//   - object-src 'none'・base-uri 'self'・frame-ancestors 'self': プラグイン・<base> の差し替え・ほかのサイトへの
//     埋め込み（クリックジャッキング）を禁じる。
//   - form-action: ブラウザはフォームを送った後のリダイレクト先にも form-action を当てはめる。OAuth の同意画面は
//     「許可する」の後に AI アプリの戻り先（redirect_uri）へ 302 で送るため、mcp.ValidRedirectURI が受け付ける戻り先
//     （https と、この PC の http）を許す。デスクトップモードの「サイト」画面は、作ったサイトの管理画面
//     （http://127.0.0.1:<別のポート>/admin）へ 303 で送るため、この PC の http を許す。
func (s *Server) adminCSP(r *http.Request) string {
	formAction := "form-action 'self'"
	p := path.Clean("/" + r.URL.Path)
	oauth := p == "/oauth" || strings.HasPrefix(p, "/oauth/")
	switch {
	case oauth && oauthRedirectsToIPv6Loopback(r):
		// 戻り先が http://[::1] のときは CSP に書けないため、この画面に限り form-action を付けない（同意を断られないように）。
		formAction = ""
	case oauth:
		formAction += " https: " + loopbackHTTPSources
	case s.cfg.Desktop:
		formAction += " " + loopbackHTTPSources
	}
	directives := []string{
		"default-src 'self'",
		"base-uri 'self'",
		formAction,
		"frame-ancestors 'self'",
		"object-src 'none'",
		"img-src 'self' data: blob: https:",
		"style-src 'self' 'unsafe-inline'",
		"script-src 'self' 'unsafe-inline'",
		"connect-src 'self'",
		"frame-src 'self' https://www.youtube-nocookie.com",
		"font-src 'self'",
		"worker-src 'self'",
		"manifest-src 'self'",
	}
	return strings.Join(slices.DeleteFunc(directives, func(d string) bool { return d == "" }), "; ")
}

// oauthRedirectsToIPv6Loopback は OAuth の画面の要求（クエリか、読み込み済みのフォーム）の redirect_uri が
// http://[::1] かを返す。
func oauthRedirectsToIPv6Loopback(r *http.Request) bool {
	raw := r.URL.Query().Get("redirect_uri")
	if raw == "" && r.PostForm != nil {
		raw = r.PostForm.Get("redirect_uri")
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && u.Hostname() == "::1"
}

// adminCSPMiddleware は管理画面・はじめに・OAuth の同意画面の応答に adminCSP を付ける。添付資料のダウンロード（sandbox）や
// 公開テーマでのプレビュー（renderPublic）のように、ハンドラーが自分で CSP を決めた応答はそのままにする。
func (s *Server) adminCSPMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !isAdminCSPPath(c.Request().URL.Path) {
				return next(c)
			}
			res := c.Response()
			res.Before(func() {
				if res.Header().Get(echo.HeaderContentSecurityPolicy) == "" {
					res.Header().Set(echo.HeaderContentSecurityPolicy, s.adminCSP(c.Request()))
				}
			})
			return next(c)
		}
	}
}

// uploadCSPMiddleware は /uploads/ のファイルの応答に uploadCSP を付ける。存在しないファイルの 404 ページ（公開サイトの
// テーマで描く）は崩さないよう、成功した応答（400 未満）にだけ付ける。
func uploadCSPMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		res := c.Response()
		res.Before(func() {
			if res.Status < http.StatusBadRequest {
				res.Header().Set(echo.HeaderContentSecurityPolicy, uploadCSP)
			}
		})
		return next(c)
	}
}

// ParseTrustedProxies は TRUSTED_PROXIES の値（IP アドレスか CIDR の一覧。例: 127.0.0.1, 10.0.0.0/8）を解析する。
// 空の値は飛ばす。IPv4 を埋め込んだ IPv6（::ffff:10.0.0.1）は IPv4 として扱う。
func ParseTrustedProxies(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range list {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(v, "/") {
			pp, err := netip.ParsePrefix(v)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: use an IP address or a CIDR such as 10.0.0.0/8", v)
			}
			p = pp.Masked()
		} else {
			a, err := netip.ParseAddr(v)
			if err != nil || a.Zone() != "" {
				return nil, fmt.Errorf("invalid trusted proxy %q: use an IP address or a CIDR such as 10.0.0.0/8", v)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		out = append(out, p)
	}
	return out, nil
}

// newIPExtractor は c.RealIP() が使う送り元のアドレスの取り出し方を返す。trusted が空なら直接つながった送り元
// （RemoteAddr）だけを使う。指定があれば、送り元が trusted のプロキシのときだけ X-Forwarded-For を右（近い側）から
// 読み、trusted でない最初のアドレスを使う。ループバック・私設アドレスでも、trusted に含めない限り信頼しない。
func newIPExtractor(trusted []netip.Prefix) echo.IPExtractor {
	if len(trusted) == 0 {
		return echo.ExtractIPDirect()
	}
	opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
	for _, p := range trusted {
		opts = append(opts, echo.TrustIPRange(&net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}))
	}
	return echo.ExtractIPFromXFFHeader(opts...)
}

// allowedHostPattern は ALLOWED_HOSTS の1つの値の書式（小文字にした後のホスト名か角括弧付きの IPv6 と、任意のポート）。
var allowedHostPattern = regexp.MustCompile(`^(?:[a-z0-9-]+(?:\.[a-z0-9-]+)*|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)

// normalizeAllowedHosts は Config.AllowedHosts を小文字にそろえ、空の値と重複を除く。https://example.jp/ のように URL で
// 書いた値はホスト（とポート）だけを使う。ホストとして読めない値はエラーにする（打ち間違いのまま守らずに動かさない）。
func normalizeAllowedHosts(list []string) ([]string, error) {
	var out []string
	for _, raw := range list {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" {
			continue
		}
		if strings.Contains(v, "://") {
			u, err := url.Parse(v)
			if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
				return nil, fmt.Errorf("invalid allowed host %q: use a host name such as example.jp", raw)
			}
			v = u.Host
		}
		if name, port, err := net.SplitHostPort(v); err == nil {
			if strings.Contains(name, ":") {
				name = "[" + name + "]"
			}
			v = strings.TrimSuffix(name, ".") + ":" + port
		} else {
			v = strings.TrimSuffix(v, ".")
		}
		if !allowedHostPattern.MatchString(v) {
			return nil, fmt.Errorf("invalid allowed host %q: use a host name such as example.jp", raw)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}
