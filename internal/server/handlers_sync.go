package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gen2brain/webp"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/image"
	"sorus/internal/lock"
	"sorus/internal/mcp"
	"sorus/internal/theme"
)

const (
	// maxSyncImages は submit_draft の1回の要求で受け付ける画像の数。1枚の画像は原寸と3サイズの4ファイルになる。
	maxSyncImages = 64
	// maxPendingArticles は fetch_pending が1回に返す記事数の上限。
	maxPendingArticles = 200
	// maxSettingRunes はサイト基本設定（解析タグ以外）の各項目の最大文字数。
	maxSettingRunes = 1000
	// maxAnalyticsTagRunes は解析タグの最大文字数。
	maxAnalyticsTagRunes = 10000
	// maxSyncDisplayNameRunes は同期で作成するユーザーの表示名（トークンの sub）の最大文字数。
	maxSyncDisplayNameRunes = 100

	// syncUserIDPrefix は同期で作成するユーザーの ID・ユーザー名の接頭辞。
	syncUserIDPrefix = "sync-"
	// unusablePasswordHash は同期で作成するユーザーのパスワードハッシュ。bcrypt の形式ではないため、
	// どのパスワードとも一致せず、管理画面にはログインできない。
	unusablePasswordHash = "!"
)

var (
	// syncImageNamePattern は転送を受け付ける画像のファイル名。image.Process が生成する
	// <32桁の16進数>-original.webp と <32桁の16進数>-<横幅>.webp だけを許し、パスの区切りや別の拡張子を拒否する。
	syncImageNamePattern = regexp.MustCompile(`^[0-9a-f]{32}-(?:original|[0-9]{1,5})\.webp$`)
	// syncArticleIDPattern は提出で指定できる記事 ID。
	syncArticleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// syncPermissions はアクションごとに、実行できるロールかを判定する。
var syncPermissions = map[string]func(auth.Role) bool{
	SyncActionSubmitDraft:    auth.CanSubmitDraft,
	SyncActionFetchPending:   auth.CanFetchPending,
	SyncActionPublishArticle: auth.CanPublish,
	SyncActionSyncSettings:   auth.CanSyncSettings,
	SyncActionIssueInvite:    auth.CanManageInvites,
	SyncActionListTokens:     auth.CanManageInvites,
	SyncActionRevokeToken:    auth.CanManageInvites,
	// 予約と予約の取り消しは、公開と同じ立場（editor, admin）に限る。
	SyncActionScheduleArticle:   auth.CanPublish,
	SyncActionUnscheduleArticle: auth.CanPublish,
	SyncActionUnpublishArticle:  auth.CanPublish,
	// 対応している機能の問い合わせは、有効なトークンならどのロールでもできる（記事の内容は返さない）。
	SyncActionServerInfo: func(auth.Role) bool { return true },
	// 公開サーバー自身の閲覧制限（Basic 認証）の確認と変更は、サイト設定と同じく管理者だけ。
	SyncActionGetSiteAccess: auth.CanSyncSettings,
	SyncActionSetSiteAccess: auth.CanSyncSettings,
	// 公開サーバーのメンテナンス表示の確認と切り替えも、サイト設定と同じく管理者だけ。
	SyncActionGetMaintenance: auth.CanSyncSettings,
	SyncActionSetMaintenance: auth.CanSyncSettings,
	// フォトギャラリーに使う画像の記録の追加・更新（handlers_sync_media.go）は公開サイトをすぐに変えるため、公開と同じ立場に限る。
	SyncActionSyncMedia: auth.CanPublish,
	// 記事の添付資料（PDF など）の受け取り（handlers_sync_attachments.go）も、公開サイトの記事をすぐに変えるため公開と同じ立場に限る。
	SyncActionSyncAttachments: auth.CanPublish,
	// 公開サーバーの MCP 連携（handlers_sync_mcp.go）は、有効な招待ならどのロールでも、招待のロールの権限で扱える。
	SyncActionMCPInfo:           canUseSyncMCP,
	SyncActionMCPIssueToken:     canUseSyncMCP,
	SyncActionMCPRevokeToken:    canUseSyncMCP,
	SyncActionMCPRevokeOAuthApp: canUseSyncMCP,
	SyncActionMCPIssueLoginCode: canUseSyncMCP,
	// 編集中の印と変更案の取り込みは、校閲待ちの記事を本文ごと受け取れる立場（editor, admin）に限る。
	SyncActionCheckout:        auth.CanFetchPending,
	SyncActionReleaseCheckout: auth.CanFetchPending,
	SyncActionResolveProposal: auth.CanFetchPending,
	// 下書き・校閲待ちの記事1件の取り込みは、提出できる立場（writer, editor, admin）ならできる（公開中の記事は返さない）。
	SyncActionPullArticle: auth.CanSubmitDraft,
}

// RegisterSyncAPI は pubKey で署名を検証する同期 API（POST Config.SyncAPIPath）を登録する。
// 管理画面の Cookie セッション・CSRF の外に置き、ケーパビリティ・トークン（Authorization: Bearer）で
// 認証・認可する。pubKey が Ed25519 の公開鍵の長さでなければ auth.ErrInvalidKey を返す。
func (s *Server) RegisterSyncAPI(pubKey ed25519.PublicKey) error {
	if len(pubKey) != ed25519.PublicKeySize {
		return fmt.Errorf("server: sync api: %w", auth.ErrInvalidKey)
	}
	// 呼び出し側が鍵のスライスを書き換えても検証に影響しないよう複製する。
	key := slices.Clone(pubKey)
	s.setSyncPublicKey(key)
	s.echo.POST(s.cfg.SyncAPIPath, s.syncHandler(key))
	return nil
}

// setSyncPublicKey は同期 API のトークンを検証する公開鍵を覚える。
func (s *Server) setSyncPublicKey(pub ed25519.PublicKey) {
	key := slices.Clone(pub)
	s.syncKey.Store(&key)
}

// syncPublicKey は同期 API のトークンを検証する公開鍵を返す。同期 API を登録していなければ nil。
func (s *Server) syncPublicKey() ed25519.PublicKey {
	if p := s.syncKey.Load(); p != nil {
		return *p
	}
	return nil
}

// syncHandler は同期 API のハンドラを返す。トークンが無い・無効・期限切れ・取り消し済みなら 401、
// ロールにアクションの権限が無ければ 403 を返す。トークンの検証はボディを読む前に行う。
//
// 大量の試行を抑えるため、接続元の IP アドレスごとに要求の数（syncRequestLimit）と認証の失敗の数
// （syncAuthFailureLimit）を数え、超えたら期間が明けるまで 429 と Retry-After を返す（トークンの検証より前に判定する）。
func (s *Server) syncHandler(pubKey ed25519.PublicKey) echo.HandlerFunc {
	requests := newSyncRateLimiter(syncRequestLimit, syncRequestWindow)
	failures := newSyncRateLimiter(syncAuthFailureLimit, syncAuthFailureWindow)
	return func(c echo.Context) error {
		// 認証済みの応答には記事の本文や設定が含まれるため、途中のキャッシュに残させない。
		c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
		ip := s.syncClientIP(c)
		now := time.Now()
		if wait := failures.blocked(ip, now); wait > 0 {
			return syncTooManyRequests(c, wait)
		}
		if wait := requests.hit(ip, now); wait > 0 {
			return syncTooManyRequests(c, wait)
		}
		claims, err := s.verifySyncToken(c, pubKey)
		if err != nil {
			var he *echo.HTTPError
			if errors.As(err, &he) && he.Code == http.StatusUnauthorized {
				failures.hit(ip, now)
			}
			return err
		}
		// 有効なトークンの要求に限り、本文（画像・独自テーマのファイルで十数 MB になる）を読み終えて応答するまでの
		// 期限を延ばす（サーバーの ReadTimeout・WriteTimeout は、認証の前の低速な接続の占有を防ぐためのもの）。
		extendSyncDeadlines(c)
		req, err := decodeSyncRequest(c)
		if err != nil {
			return err
		}
		allowed, ok := syncPermissions[req.Action]
		if !ok {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("未対応の action です: %q", req.Action))
		}
		if !allowed(claims.Role) {
			return echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf("このトークン（%s）には %s の権限がありません", claims.Role, req.Action))
		}
		switch req.Action {
		case SyncActionSubmitDraft:
			return s.syncSubmitDraft(c, claims, req)
		case SyncActionFetchPending:
			return s.syncFetchPending(c)
		case SyncActionPublishArticle:
			return s.syncPublishArticle(c, claims, req)
		case SyncActionScheduleArticle:
			return s.syncScheduleArticle(c, claims, req)
		case SyncActionUnscheduleArticle:
			return s.syncUnscheduleArticle(c, claims, req)
		case SyncActionUnpublishArticle:
			return s.syncUnpublishArticle(c, claims, req)
		case SyncActionServerInfo:
			return c.JSON(http.StatusOK, SyncServerInfoResponse{Action: SyncActionServerInfo, Schedule: true,
				CustomThemes: true, MaxThemePackageBytes: theme.MaxArchiveSize,
				Attachments: true, MaxAttachmentBytes: s.cfg.MaxAttachmentSize, AISwitch: true})
		case SyncActionIssueInvite:
			return s.syncIssueInvite(c, claims, req)
		case SyncActionListTokens:
			return s.syncListTokens(c, claims)
		case SyncActionRevokeToken:
			return s.syncRevokeToken(c, claims, req)
		case SyncActionGetSiteAccess:
			return s.syncGetSiteAccess(c)
		case SyncActionSetSiteAccess:
			return s.syncSetSiteAccess(c, req)
		case SyncActionGetMaintenance:
			return s.syncGetMaintenance(c)
		case SyncActionSetMaintenance:
			return s.syncSetMaintenance(c, req)
		case SyncActionSyncMedia:
			return s.syncMedia(c, claims, req)
		case SyncActionSyncAttachments:
			return s.syncAttachments(c, claims, req)
		case SyncActionMCPInfo, SyncActionMCPIssueToken, SyncActionMCPRevokeToken, SyncActionMCPRevokeOAuthApp, SyncActionMCPIssueLoginCode:
			return s.syncMCP(c, claims, req)
		case SyncActionCheckout:
			return s.syncCheckout(c, claims, req)
		case SyncActionReleaseCheckout:
			return s.syncReleaseCheckout(c, claims, req)
		case SyncActionResolveProposal:
			return s.syncResolveProposal(c, claims, req)
		case SyncActionPullArticle:
			return s.syncPullArticle(c, req)
		default: // SyncActionSyncSettings（syncPermissions に無い action は上で拒否している）
			return s.syncSettings(c, req)
		}
	}
}

// 同期 API のレート制限。PC の sorus は操作のたびに数回ずつ呼ぶだけのため、通常の利用では届かない値にする。
const (
	// syncRequestLimit・syncRequestWindow は、1つの IP アドレスから syncRequestWindow の間に受け付ける要求の数。
	syncRequestLimit  = 600
	syncRequestWindow = time.Minute
	// syncAuthFailureLimit・syncAuthFailureWindow は、1つの IP アドレスに許す認証の失敗（401）の数。超えると
	// 期間が明けるまで、正しいトークンの要求も含めて受け付けない（トークンの総当たりを抑える）。
	syncAuthFailureLimit  = 30
	syncAuthFailureWindow = 10 * time.Minute
	// syncRateMaxClients は覚えておく IP アドレスの数の上限。超えたら期間の明けたものを捨てる。
	syncRateMaxClients = 10000
)

// syncRateLimiter は IP アドレスごとに、固定の期間（window）の中の回数を数える。並行に使用してよい。
type syncRateLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	clients map[string]*syncRateWindow
}

// syncRateWindow は1つの IP アドレスの、start から window の間の回数。
type syncRateWindow struct {
	start time.Time
	count int
}

func newSyncRateLimiter(limit int, window time.Duration) *syncRateLimiter {
	return &syncRateLimiter{limit: limit, window: window, clients: map[string]*syncRateWindow{}}
}

// current は ip の now の時点の期間を返す。期間が明けていれば新しい期間にする。l.mu を持って呼ぶこと。
func (l *syncRateLimiter) current(ip string, now time.Time) *syncRateWindow {
	w, ok := l.clients[ip]
	if ok && now.Sub(w.start) < l.window {
		return w
	}
	if !ok && len(l.clients) >= syncRateMaxClients {
		for k, old := range l.clients {
			if now.Sub(old.start) >= l.window {
				delete(l.clients, k)
			}
		}
	}
	w = &syncRateWindow{start: now}
	l.clients[ip] = w
	return w
}

// hit は ip の回数を1つ増やす。上限を超えたら、期間が明けるまでの時間を返す（超えていなければ 0）。
func (l *syncRateLimiter) hit(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.current(ip, now)
	w.count++
	if w.count > l.limit {
		return w.start.Add(l.window).Sub(now)
	}
	return 0
}

// blocked は ip の回数が上限に達していれば、期間が明けるまでの時間を返す（達していなければ 0）。回数は増やさない。
func (l *syncRateLimiter) blocked(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.clients[ip]
	if !ok || now.Sub(w.start) >= l.window || w.count < l.limit {
		return 0
	}
	return w.start.Add(l.window).Sub(now)
}

// syncClientIP はレート制限に使う接続元の IP アドレスを返す。echo の IPExtractor（newIPExtractor）に従い、
// TrustedProxies を指定していなければ接続元のアドレスを使う（偽装できる X-Forwarded-For は信じない）。
func (s *Server) syncClientIP(c echo.Context) string {
	return c.RealIP()
}

// syncTooManyRequests は Retry-After（秒）を付けた 429 を返す。
func syncTooManyRequests(c echo.Context, wait time.Duration) error {
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	c.Response().Header().Set("Retry-After", strconv.Itoa(secs))
	return echo.NewHTTPError(http.StatusTooManyRequests, "要求が多すぎます。しばらく待ってから、もう一度試してください")
}

// syncTransferTimeout は有効なトークンの同期 API の要求で、本文の読み込みと応答の書き込みに許す時間。PC が独自テーマを
// 送るときの制限時間（remoteThemePushTimeout）と同じにする。
const syncTransferTimeout = remoteThemePushTimeout

// extendSyncDeadlines はこの要求の読み書きの期限を syncTransferTimeout 後まで延ばす。
func extendSyncDeadlines(c echo.Context) {
	rc := http.NewResponseController(c.Response())
	deadline := time.Now().Add(syncTransferTimeout)
	if err := rc.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.Logger().Warnf("sync api: extend read deadline: %v", err)
	}
	if err := rc.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.Logger().Warnf("sync api: extend write deadline: %v", err)
	}
}

// verifySyncToken は Authorization ヘッダーの Bearer トークンを検証し、Claims を返す。
// 署名・有効期限に加えて、取り消し済みのトークン（checkSyncTokenRecord）も拒否する。
// 失敗時は WWW-Authenticate ヘッダー（RFC 6750）を付けた 401 を返す。
func (s *Server) verifySyncToken(c echo.Context, pubKey ed25519.PublicKey) (*auth.Claims, error) {
	header := c.Request().Header.Get(echo.HeaderAuthorization)
	if header == "" {
		return nil, syncUnauthorized(c, "", "Authorization ヘッダーに Bearer トークンを指定してください")
	}
	scheme, token, ok := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, syncUnauthorized(c, "invalid_request", "Authorization ヘッダーに Bearer トークンを指定してください")
	}
	claims, err := auth.VerifyToken(pubKey, token)
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		return nil, syncUnauthorized(c, "invalid_token", "トークンの有効期限が切れています")
	case errors.Is(err, auth.ErrMalformedToken), errors.Is(err, auth.ErrInvalidSignature):
		return nil, syncUnauthorized(c, "invalid_token", "トークンが無効です")
	case err != nil:
		return nil, fmt.Errorf("server: sync api: verify token: %w", err)
	}
	if claims.ID == "" {
		return nil, syncUnauthorized(c, "invalid_token",
			"この招待コードは現在使えません。サイト管理者に新しい招待コードを発行してもらい、もう一度参加してください")
	}
	if err := s.checkSyncTokenRecord(c, claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// syncUnauthorized は 401 を返す。code が空でなければ WWW-Authenticate に error 属性として載せる。
func syncUnauthorized(c echo.Context, code, msg string) error {
	challenge := "Bearer"
	if code != "" {
		challenge += ` error="` + code + `"`
	}
	c.Response().Header().Set(echo.HeaderWWWAuthenticate, challenge)
	return echo.NewHTTPError(http.StatusUnauthorized, msg)
}

// decodeSyncRequest は application/json のリクエストボディを SyncRequest として読む。
func decodeSyncRequest(c echo.Context) (*SyncRequest, error) {
	mediaType, _, _ := mime.ParseMediaType(c.Request().Header.Get(echo.HeaderContentType))
	if mediaType != echo.MIMEApplicationJSON {
		return nil, echo.NewHTTPError(http.StatusUnsupportedMediaType, "Content-Type に application/json を指定してください")
	}
	var req SyncRequest
	dec := json.NewDecoder(c.Request().Body)
	err := dec.Decode(&req)
	if err == nil {
		// 1つの JSON 値の後に続くデータは、壊れた要求として扱う。
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("trailing data after json value")
		}
	}
	if err != nil {
		// ボディの上限超過（BodyLimit ミドルウェアの 413）はそのまま返す。
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return nil, he
		}
		return nil, echo.NewHTTPError(http.StatusBadRequest, "リクエストの JSON を読み取れません")
	}
	req.Action = strings.TrimSpace(req.Action)
	return &req, nil
}

// syncSubmitDraft は記事と添付画像を受け取り、記事を校閲待ちとして保存する。公開ページには反映しない。
//
// 記事 ID を指定すると同じ ID で保存し、既存の記事なら更新する。更新できるのは下書き・校閲待ちの記事だけで
// （公開中・予約中の記事を校閲待ちに戻すと公開の取り下げになるため）、執筆チーム内で共有するため誰が提出した記事でもよい。
// 既存の記事は編集ロックを獲得してから更新する。新しい記事は作成だけを行い、存在の確認から作成までの間に
// 同じ ID の記事が作成されていれば、その記事を上書きせずに 409 を返す。
// 画像は記事より先に検査し、記事を保存できなかったときは、この要求で新しく保存した画像を削除する
// （同時に作成された記事との競合で保存できなかった場合は、その記事が同じ画像を参照しうるため残す）。
func (s *Server) syncSubmitDraft(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	return s.syncSaveRemoteArticle(c, claims, req, false, SyncActionSubmitDraft)
}

func (s *Server) syncSaveRemoteArticle(c echo.Context, claims *auth.Claims, req *SyncRequest, publish bool, responseAction string) error {
	in := req.Article
	if in == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "article を指定してください")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "タイトルを入力してください")
	}
	id := strings.TrimSpace(in.ID)
	if id != "" && !syncArticleIDPattern.MatchString(id) {
		return echo.NewHTTPError(http.StatusBadRequest, "記事 ID の形式が正しくありません")
	}
	kind := article.Kind(strings.TrimSpace(string(in.Kind)))
	if kind != "" && !kind.Valid() {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("未対応の kind です: %q（post か page を指定してください）", in.Kind))
	}
	var featuredImage *string
	if in.FeaturedImageURL != nil {
		u := strings.TrimSpace(*in.FeaturedImageURL)
		if !validFeaturedImageURL(u) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "featured_image_url の形式が正しくありません（/ で始まるパスか http(s) の URL を指定してください）")
		}
		featuredImage = &u
	}
	if err := s.validateSyncImages(req.Images); err != nil {
		return err
	}

	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	catID, err := s.syncCategoryID(ctx, in.Category)
	if err != nil {
		return err
	}

	var existing *article.Article
	if id != "" {
		existing, err = s.articles.GetByID(id)
		if errors.Is(err, article.ErrNotFound) {
			existing = nil
		} else if err != nil {
			return err
		}
	}
	a := &article.Article{ID: id, AuthorID: userID, CategoryID: catID}
	if existing != nil {
		if err := checkSyncEditable(existing, publish); err != nil {
			return err
		}
		release, err := s.lockForSync(ctx, id, userID)
		if err != nil {
			return err
		}
		defer release()
		// ロックの獲得までに状態が変わっていないか、保存する locked_by・locked_at とともに読み直して確かめる。
		if a, err = s.articles.GetByID(id); err != nil {
			return err
		}
		if err := checkSyncEditable(a, publish); err != nil {
			return err
		}
		if err := checkSyncCheckout(a, syncActorName(claims), req.ClientID, req.Force, clockNow()); err != nil {
			return err
		}
		if req.BaseUpdatedAt != nil && syncUpdatedAtDiffers(a.UpdatedAt, *req.BaseUpdatedAt) {
			return s.syncConflict(c, a)
		}
		if catID != 0 {
			a.CategoryID = catID
		}
	} else if a.CategoryID == 0 {
		if a.CategoryID, err = s.syncDefaultCategoryID(ctx); err != nil {
			return err
		}
	}

	a.Title = title
	// 更新でスラッグを省略した場合は、タイトルから作り直さず現在のスラッグを保つ。
	if slug := strings.TrimSpace(in.Slug); slug != "" || existing == nil {
		a.Slug = slug
	}
	// 種類を省略した場合は、新しい記事は post（article.Service.Save の既定）、更新では現在の種類を保つ。
	if kind != "" {
		a.Kind = kind
	}
	if a.Kind == article.KindPage {
		slug := a.Slug
		if slug == "" {
			slug = a.Title
		}
		// 管理画面と同じく、予約語と同期 API のパスの先頭部分を拒否する（画像を保存する前に断る）。
		if slug = article.NormalizeSlug(slug); s.articles.IsReservedPageSlug(slug) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, i18n.Translate(string(i18n.JA), "reserved_page_slug", slug))
		}
	}
	a.ContentMarkdown = normalizeNewlines(in.ContentMarkdown)
	a.Excerpt = strings.TrimSpace(normalizeNewlines(in.Excerpt))
	// アイキャッチを送らない古いローカル CMS からの更新では今の値を保ち、空文字なら外す。
	if featuredImage != nil {
		a.FeaturedImage = *featuredImage
	}
	if lang := strings.TrimSpace(in.Lang); lang != "" {
		a.Lang = lang
	}
	if groupID := strings.TrimSpace(in.TranslationGroupID); groupID != "" {
		if !syncArticleIDPattern.MatchString(groupID) {
			return echo.NewHTTPError(http.StatusBadRequest, "translation_group_id の形式が正しくありません")
		}
		a.TranslationGroupID = groupID
	}
	if sourceID := strings.TrimSpace(in.SourceArticleID); sourceID != "" {
		if !syncArticleIDPattern.MatchString(sourceID) {
			return echo.NewHTTPError(http.StatusBadRequest, "source_article_id の形式が正しくありません")
		}
		a.SourceArticleID = sql.NullString{String: sourceID, Valid: true}
	}
	if err := s.applySyncHierarchy(ctx, a, in.ParentID, in.SortOrder); err != nil {
		return err
	}
	if publish {
		a.Status = article.StatusPublished
		now := time.Now().UTC()
		if a.PublishedAt == nil || a.PublishedAt.After(now) {
			a.PublishedAt = &now
		}
		// PC での公開日時が過去なら、その日時にそろえる（一括同期で送った古い記事が新着として並ばないように）。
		// 未来の日時は即時公開の要求として扱わず（予約は schedule_article を使う）、今までどおりにする。
		if in.PublishedAt != nil && !in.PublishedAt.IsZero() && !in.PublishedAt.After(now) {
			t := in.PublishedAt.UTC()
			a.PublishedAt = &t
		}
	} else {
		a.Status = article.StatusReview
	}

	save := s.articles.Save
	if existing == nil {
		save = s.articles.Create
	}
	created, urls, err := s.saveSyncImages(req.Images)
	if err != nil {
		return err
	}
	if err := save(a); err != nil {
		if errors.Is(err, article.ErrExists) {
			// 同時に作成した提出が、この要求で保存した画像を同じ内容の保存済みの画像として参照しているため削除しない。
			return echo.NewHTTPError(http.StatusConflict, "同じ記事 ID の記事が同時に作成されました。もう一度提出してください")
		}
		removeFiles(created)
		if errors.Is(err, article.ErrInvalidParent) || errors.Is(err, article.ErrHasChildPages) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "固定ページの親子関係が正しくないため保存できませんでした（自分の子ページを親にした・子ページのある固定ページを記事にした など）")
		}
		if errors.Is(err, article.ErrInvalid) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "記事を保存できませんでした。入力内容を確認してください")
		}
		return err
	}
	s.reindexArticle(c, a.ID)
	s.clearSyncCheckout(c, a)
	if publish {
		s.syncSnapshot(c, a, userID, "同期で直接公開")
		s.trackScheduled(c, a.ID)
	} else {
		s.syncSnapshot(c, a, userID, "同期で提出")
	}

	ref, err := s.syncArticleRef(ctx, a)
	if err != nil {
		return err
	}
	out := SyncSubmitResponse{
		Action:  responseAction,
		Created: existing == nil,
		Article: ref,
		Images:  urls,
	}
	if publish {
		// PC が添付資料の無い記事の sync_attachments を省けるよう、公開サーバーにあるこの記事の添付資料の数を返す。
		var n int
		if err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM attachments WHERE article_id = ?`, a.ID); err != nil {
			return fmt.Errorf("server: sync publish: count attachments: %w", err)
		}
		out.Attachments = &n
	}
	return c.JSON(http.StatusOK, out)
}

// checkSyncEditable は提出で既存の記事 a を更新できるかを確かめる。執筆チーム内では非公開の記事を共有するため、
// 誰が提出した記事でも更新できる（公開中の記事を直接公開で更新できるのは、公開の権限を持つトークンだけ）。
func checkSyncEditable(a *article.Article, publish bool) error {
	if a.Status != article.StatusDraft && a.Status != article.StatusReview && !(publish && a.Status == article.StatusPublished) {
		return echo.NewHTTPError(http.StatusConflict,
			"更新できるのは下書き・校閲待ちの記事だけです（現在: "+statusLabel(a.Status)+"）")
	}
	return nil
}

// validateSyncImages は転送された画像を保存する前にすべて検査する。ファイル名は image.Process が生成する形式、
// 中身は寸法の上限内の WebP でなければならない。
func (s *Server) validateSyncImages(images []SyncImage) error {
	if len(images) > maxSyncImages {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("画像は1回に%d件まで転送できます", maxSyncImages))
	}
	seen := make(map[string]bool, len(images))
	for _, img := range images {
		if !syncImageNamePattern.MatchString(img.Name) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("画像のファイル名が正しくありません: %q", img.Name))
		}
		if seen[img.Name] {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "同じ名前の画像が重複しています: "+img.Name)
		}
		seen[img.Name] = true
		if int64(len(img.Data)) > s.cfg.MaxUploadSize {
			return uploadError(image.ErrFileTooLarge)
		}
		if image.DetectMIME(img.Data) != image.MimeWebP {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "WebP の画像だけを転送できます: "+img.Name)
		}
		cfg, err := webp.DecodeConfig(bytes.NewReader(img.Data))
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "画像が壊れているため読み込めません: "+img.Name)
		}
		if int64(cfg.Width)*int64(cfg.Height) > image.MaxPixels {
			return uploadError(image.ErrTooManyPixels)
		}
	}
	return nil
}

// saveSyncImages は検査済みの images を UploadDir に同じファイル名で保存し、この要求で新しく作成したファイルの
// パスと、全画像の配信 URL を返す。同じ名前のファイルが既にあれば、内容が同じなら保存済みとして扱い、
// 異なれば上書きせずに 409 を返す。失敗した場合は、この要求で作成したファイルを削除する。
func (s *Server) saveSyncImages(images []SyncImage) (created, urls []string, err error) {
	urls = make([]string, 0, len(images))
	defer func() {
		if err != nil {
			removeFiles(created)
			created = nil
		}
	}()
	for _, img := range images {
		dst := filepath.Join(s.cfg.UploadDir, img.Name)
		werr := writeFileExclusive(dst, img.Data)
		switch {
		case errors.Is(werr, fs.ErrExist):
			current, rerr := os.ReadFile(dst)
			if rerr != nil {
				return created, nil, fmt.Errorf("server: sync image: %w", rerr)
			}
			if !bytes.Equal(current, img.Data) {
				return created, nil, echo.NewHTTPError(http.StatusConflict, "同じ名前の別の画像が既に保存されています: "+img.Name)
			}
		case werr != nil:
			return created, nil, werr
		default:
			created = append(created, dst)
		}
		urls = append(urls, UploadURLPrefix+img.Name)
	}
	return created, urls, nil
}

// writeFileExclusive は data を一時ファイルに書き切ってから dst にハードリンクする。dst が既にあれば
// fs.ErrExist を返し、既存のファイルを上書きしない。書きかけのファイルを dst の名前で公開しない。
func writeFileExclusive(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".sync-*.tmp")
	if err != nil {
		return fmt.Errorf("server: sync image: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // リンク後も一時ファイルの名前だけを消す。
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("server: sync image: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("server: sync image: write: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("server: sync image: chmod: %w", err)
	}
	if err := os.Link(tmpName, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fs.ErrExist
		}
		return fmt.Errorf("server: sync image: link: %w", err)
	}
	return nil
}

func removeFiles(paths []string) {
	for _, p := range paths {
		os.Remove(p)
	}
}

// syncPendingSelect は fetch_pending で返す記事の列と結合。WHERE 以降は呼び出し側が付ける。
const syncPendingSelect = `SELECT a.id, a.title, a.slug, a.content_markdown,
		a.excerpt, a.status, a.kind, a.author_id, a.created_at, a.updated_at, a.published_at,
		COALESCE(c.slug, '') AS category_slug, COALESCE(c.name, '') AS category_name,
		COALESCE(NULLIF(u.display_name, ''), u.username, '') AS author_name,
		COALESCE(NULLIF(a.lang, ''), 'ja') AS lang,
		COALESCE(a.translation_group_id, '') AS translation_group_id,
		COALESCE(a.source_article_id, '') AS source_article_id,
		COALESCE(a.parent_id, '') AS parent_id, a.sort_order,
		COALESCE(a.checked_out_by, '') AS checked_out_by, a.checked_out_at,
		COALESCE(a.checked_out_client, '') AS checked_out_client
	FROM articles a
	LEFT JOIN categories c ON c.id = a.category_id
	LEFT JOIN users u ON u.id = a.author_id
	`

// syncFetchPending は校閲待ちの記事を本文とともに、更新日時の古い順（校閲を待っている順）に返す。
// あわせて予約中の記事（公開で、公開日時が未来）を公開日時の早い順に Scheduled で返す。予約中の記事は
// Articles には入れない（予約を知らない古いローカル CMS が、校閲待ちとして公開の操作を出さないように）。
// 下書きの記事は更新日時の古い順に Drafts で返す。どの記事にも、有効な編集中の印（チェックアウト）と
// AI アプリからの確認待ちの変更案を添える。
func (s *Server) syncFetchPending(c echo.Context) error {
	ctx := c.Request().Context()
	articles := []SyncPendingArticle{}
	err := s.db.SelectContext(ctx, &articles, syncPendingSelect+`WHERE a.status = ?
		ORDER BY julianday(a.updated_at), a.id
		LIMIT ?`, article.StatusReview, maxPendingArticles)
	if err != nil {
		return fmt.Errorf("server: sync fetch pending: %w", err)
	}
	scheduled := []SyncPendingArticle{}
	err = s.db.SelectContext(ctx, &scheduled, syncPendingSelect+`WHERE `+article.ScheduledCondition+`
		ORDER BY julianday(a.published_at), a.id
		LIMIT ?`, article.StatusPublished, clockNow().UTC(), maxPendingArticles)
	if err != nil {
		return fmt.Errorf("server: sync fetch pending: scheduled: %w", err)
	}
	drafts := []SyncPendingArticle{}
	err = s.db.SelectContext(ctx, &drafts, syncPendingSelect+`WHERE a.status = ?
		ORDER BY julianday(a.updated_at), a.id
		LIMIT ?`, article.StatusDraft, maxPendingArticles)
	if err != nil {
		return fmt.Errorf("server: sync fetch pending: drafts: %w", err)
	}
	now := clockNow()
	for _, list := range [][]SyncPendingArticle{articles, scheduled, drafts} {
		for i := range list {
			if err := s.decorateSyncPending(c, &list[i], now); err != nil {
				return err
			}
		}
	}
	return c.JSON(http.StatusOK, SyncPendingResponse{Action: SyncActionFetchPending, Articles: articles, Scheduled: scheduled, Drafts: drafts})
}

// syncPullArticle は記事 article_id 1件を、本文と、公開サーバーにある参照画像のファイル名とともに返す。
// 下書き・校閲待ちの記事に限る。記事が無ければ（削除済みを含む）404、それ以外の状態なら 409 を返す。
func (s *Server) syncPullArticle(c echo.Context, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var p SyncPendingArticle
	err = s.db.GetContext(ctx, &p, syncPendingSelect+`WHERE a.id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && p.Status == article.StatusDeleted) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return fmt.Errorf("server: sync pull article: %w", err)
	}
	if p.Status != article.StatusDraft && p.Status != article.StatusReview {
		return echo.NewHTTPError(http.StatusConflict, "下書きか校閲待ちの記事だけを取り込めます")
	}
	if err := s.decorateSyncPending(c, &p, clockNow()); err != nil {
		return err
	}
	var featured string
	if err := s.db.GetContext(ctx, &featured, `SELECT featured_image_url FROM articles WHERE id = ?`, id); err != nil {
		return fmt.Errorf("server: sync pull article: featured image: %w", err)
	}
	images := []string{}
	for _, hash := range referencedImageHashes(p.ContentMarkdown + "\n" + featured) {
		for _, variant := range remoteImageVariants {
			name := hash + "-" + variant + ".webp"
			if _, err := os.Stat(filepath.Join(s.cfg.UploadDir, name)); err == nil {
				images = append(images, name)
			}
		}
	}
	return c.JSON(http.StatusOK, SyncPullArticleResponse{Action: SyncActionPullArticle, Article: p, Images: images})
}

// syncPublishArticle は校閲待ちの記事を公開する。article と images がある場合は内容も更新してから直接公開する。
// 記事が無ければ 404、校閲待ちでなければ 409、他のユーザーが管理画面で編集中なら 409 を返す。
func (s *Server) syncPublishArticle(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	if req.Article != nil {
		return s.syncSaveRemoteArticle(c, claims, req, true, SyncActionPublishArticle)
	}
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	release, err := s.lockForSync(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()

	// handleApprovePublish と同じく、使えないスラッグのページは状態を変える前に断る。
	if a, err := s.articles.GetByID(id); err == nil && s.reservedPage(a) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, i18n.Translate(string(i18n.JA), "reserved_page_slug", a.Slug))
	} else if err != nil && !errors.Is(err, article.ErrNotFound) {
		return err
	} else if err == nil {
		if err := checkSyncCheckout(a, syncActorName(claims), req.ClientID, req.Force, clockNow()); err != nil {
			return err
		}
		if req.BaseUpdatedAt != nil && syncUpdatedAtDiffers(a.UpdatedAt, *req.BaseUpdatedAt) {
			return s.syncConflict(c, a)
		}
	}

	now := time.Now().UTC()
	// 一度公開した記事を再承認した場合は、最初の公開日時を保つ（handleApprovePublish と同じ扱い）。
	res, err := s.db.ExecContext(ctx, `UPDATE articles SET status = ?, published_at = COALESCE(published_at, ?),
			scheduled_at = NULL, updated_at = ? WHERE id = ? AND status = ?`,
		article.StatusPublished, now, now, id, article.StatusReview)
	if err != nil {
		return fmt.Errorf("server: sync publish: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("server: sync publish: %w", err)
	}
	a, err := s.articles.GetByID(id)
	if err != nil {
		return err
	}
	if n == 0 {
		return echo.NewHTTPError(http.StatusConflict, "校閲待ちの記事ではありません（現在: "+statusLabel(a.Status)+"）")
	}
	s.reindexArticle(c, id)
	s.trackScheduled(c, id)
	s.clearSyncCheckout(c, a)
	s.syncSnapshot(c, a, userID, "同期で承認・公開")

	ref, err := s.syncArticleRef(ctx, a)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SyncPublishResponse{Action: SyncActionPublishArticle, Article: ref})
}

// syncArticleIDFrom は article_id を検査して返す。空なら 400、形式が正しくなければ 400 を返す。
func syncArticleIDFrom(req *SyncRequest) (string, error) {
	id := strings.TrimSpace(req.ArticleID)
	if id == "" {
		return "", echo.NewHTTPError(http.StatusBadRequest, "article_id を指定してください")
	}
	if !syncArticleIDPattern.MatchString(id) {
		return "", echo.NewHTTPError(http.StatusBadRequest, "記事 ID の形式が正しくありません")
	}
	return id, nil
}

// syncScheduleArticle は校閲待ちの記事 article_id を、公開日時 publish_at に読者に見え始める予約にし
// （状態は公開、公開日時は publish_at）、検索インデックスに反映する（検索は公開日時で絞るため、時刻まで出ない）。
// 公開サーバーは公開日時を保存するだけで、時刻が来ても何も書き換えない。読者向けのページ・一覧・RSS・
// サイトマップ・検索は、読むたびに公開日時と今の時刻で絞る（article.VisibleCondition）。
//
// publish_at が無ければ 400、今から MinScheduleLead 以上先でなければ 422 を返す。記事が無ければ 404、
// 他のユーザーが編集中なら 409 を返す。読者に見えている記事や、一度公開した記事（公開日時が過去）は、
// 予約にすると見えなくなるため 422 で断る（D9）。それ以外で校閲待ちでなければ 409 を返す。
func (s *Server) syncScheduleArticle(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	if req.PublishAt == nil || req.PublishAt.IsZero() {
		return echo.NewHTTPError(http.StatusBadRequest, "publish_at を指定してください")
	}
	publishAt := req.PublishAt.UTC()
	if now := clockNow().UTC(); publishAt.Before(now.Add(MinScheduleLead)) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "予約の日時には、今から1分以上先の日時を指定してください")
	}
	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	release, err := s.lockForSync(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()

	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return err
	}
	// ロックの獲得を待つ間に時刻が進むため、読み直した記事と同じ時刻で判定する。
	now := clockNow().UTC()
	switch {
	case article.IsVisible(a, now):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "この記事はすでに公開中のため、予約にできません")
	case a.Status == article.StatusReview && a.PublishedAt != nil && !a.PublishedAt.After(now):
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			"一度公開した記事は予約にできません。予約せずに公開してください（最初の公開日時のまま公開します）")
	case a.Status != article.StatusReview:
		return echo.NewHTTPError(http.StatusConflict, "校閲待ちの記事ではありません（現在: "+statusLabel(article.ViewStatus(a, now))+"）")
	case s.reservedPage(a):
		// publish_article と同じく、使えないスラッグのページは状態を変える前に断る。
		return echo.NewHTTPError(http.StatusUnprocessableEntity, i18n.Translate(string(i18n.JA), "reserved_page_slug", a.Slug))
	}
	if err := checkSyncCheckout(a, syncActorName(claims), req.ClientID, req.Force, now); err != nil {
		return err
	}
	if req.BaseUpdatedAt != nil && syncUpdatedAtDiffers(a.UpdatedAt, *req.BaseUpdatedAt) {
		return s.syncConflict(c, a)
	}

	// 条件を UPDATE にも書き、確かめてから書き込むまでの間に状態や公開日時が変わっていれば書き込まない。
	res, err := s.db.ExecContext(ctx, `UPDATE articles SET status = ?, published_at = ?, scheduled_at = NULL, updated_at = ?
			WHERE id = ? AND status = ? AND (published_at IS NULL OR julianday(published_at) > julianday(?))`,
		article.StatusPublished, publishAt, now, id, article.StatusReview, now)
	if err != nil {
		return fmt.Errorf("server: sync schedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("server: sync schedule: %w", err)
	}
	if a, err = s.articles.GetByID(id); err != nil {
		return err
	}
	if n == 0 {
		return echo.NewHTTPError(http.StatusConflict, "校閲待ちの記事ではありません（現在: "+statusLabel(article.ViewStatus(a, now))+"）")
	}
	s.reindexArticle(c, id)
	s.trackScheduled(c, id)
	s.clearSyncCheckout(c, a)
	s.syncSnapshot(c, a, userID, "同期で承認・予約")

	ref, err := s.syncArticleRef(ctx, a)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SyncPublishResponse{Action: SyncActionScheduleArticle, Article: ref})
}

// syncUnscheduleArticle は予約中の記事 article_id を校閲待ちに戻し、公開日時を消す。予約中の記事の公開日時は
// 未来で、一度公開した記事は予約にできない（syncScheduleArticle・article.Service.Save）ため、予約中の記事は
// 読者に見えたことが無く、消してよい（再び承認すると、そのときの日時が公開日時になる）。
// 記事が無ければ 404、他のユーザーが編集中なら 409、予約中でなければ（公開日時を過ぎて公開された記事を含む）409 を返す。
func (s *Server) syncUnscheduleArticle(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	release, err := s.lockForSync(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()

	now := clockNow().UTC()
	// 公開日時を過ぎて読者に見え始めた記事は、取り消すと見えなくなるため戻さない（条件は article.ScheduledCondition）。
	res, err := s.db.ExecContext(ctx, `UPDATE articles SET status = ?, published_at = NULL, scheduled_at = NULL, updated_at = ?
			WHERE id IN (SELECT a.id FROM articles a WHERE a.id = ? AND `+article.ScheduledCondition+`)`,
		article.StatusReview, now, id, article.StatusPublished, now)
	if err != nil {
		return fmt.Errorf("server: sync unschedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("server: sync unschedule: %w", err)
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return err
	}
	if n == 0 {
		if article.IsVisible(a, now) {
			return echo.NewHTTPError(http.StatusConflict, "予約の時刻を過ぎ、すでに公開されています。予約は取り消せません")
		}
		return echo.NewHTTPError(http.StatusConflict, "予約中の記事ではありません（現在: "+statusLabel(article.ViewStatus(a, now))+"）")
	}
	s.reindexArticle(c, id)
	s.trackScheduled(c, id)
	s.syncSnapshot(c, a, userID, "同期で予約を取り消し")

	ref, err := s.syncArticleRef(ctx, a)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SyncPublishResponse{Action: SyncActionUnscheduleArticle, Article: ref})
}

// syncUnpublishArticle は公開先の記事を下書きに戻す。公開日時が過去なら初回公開日時として保持し、
// 未来なら予約を取り消すため消す。記事が無く、または編集中なら 404/409 を返す。
func (s *Server) syncUnpublishArticle(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	release, err := s.lockForSync(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()

	now := clockNow().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE articles SET status = ?,
		published_at = CASE WHEN published_at IS NOT NULL AND julianday(published_at) > julianday(?) THEN NULL ELSE published_at END,
		scheduled_at = NULL, updated_at = ? WHERE id = ?`, article.StatusDraft, now, now, id); err != nil {
		return fmt.Errorf("server: sync unpublish: %w", err)
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return err
	}
	s.reindexArticle(c, id)
	s.trackScheduled(c, id)
	s.syncSnapshot(c, a, userID, "同期で下書きに戻す")
	ref, err := s.syncArticleRef(ctx, a)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SyncPublishResponse{Action: SyncActionUnpublishArticle, Article: ref})
}

// syncSettings はサイト基本設定のうち指定された項目を検査して保存し、サーバーのメモリ上の設定にも反映する。
// 管理画面の「サイト基本設定」と同じ settings のキーを使う。サイト構成（メニュー・トップページ・カード）は
// 指定された項目だけを保存済みの値に重ね、管理画面の「サイト構成」と同じ検査をしてから保存する。
// どれかの項目が不正なら、何も保存せずに 422 を返す。
//
// ThemePackage（独自テーマのファイル）が付いていれば、テーマを作業ディレクトリに展開・検証し（不正なら 422）、
// 設定を書き込むトランザクションの中で配置してカタログを読み直してから確定する。確定に失敗したら配置したテーマを
// 元に戻す（themeMu の中で行い、ほかの取り込み・反映と重ならない）。元に戻せなかったとき・途中でプロセスが
// 止まったときは、テーマのファイルだけが無いか新しい版になることがある。元のデータは PC にあるため、PC から
// もう一度反映して揃える（自動の復旧はしない）。
func (s *Server) syncSettings(c echo.Context, req *SyncRequest) error {
	in := req.Settings
	if in == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "settings を指定してください")
	}
	if in.ThemePackage != nil && in.Theme == nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "独自テーマのファイル（theme_package）を送るときは theme も指定してください")
	}
	ctx := c.Request().Context()
	type update struct{ key, value string }
	var updates []update
	texts := []struct {
		key, label string
		value      *string
		limit      int
	}{
		{"site_name", "サイト名", in.SiteName, maxSettingRunes},
		{"site_description", "サイトの説明", in.SiteDescription, maxSettingRunes},
		{"copyright", "著作権表記", in.Copyright, maxSettingRunes},
		{"analytics_tag", "解析タグ", in.AnalyticsTag, maxAnalyticsTagRunes},
	}
	for _, t := range texts {
		if t.value == nil {
			continue
		}
		value := strings.TrimSpace(normalizeNewlines(*t.value))
		if utf8.RuneCountInString(value) > t.limit {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("%sは%d文字以内で指定してください", t.label, t.limit))
		}
		// 管理画面の保存と同じく、空のサイト名は既定のサイト名にする。
		if t.key == "site_name" && value == "" {
			value = DefaultSiteName
		}
		updates = append(updates, update{t.key, value})
	}
	if in.BaseURL != nil {
		baseURL, err := normalizeSyncBaseURL(*in.BaseURL)
		if err != nil {
			return err
		}
		updates = append(updates, update{"base_url", baseURL})
	}
	for _, b := range []struct {
		key   string
		value *bool
	}{
		{settingShowFrontCardImages, in.ShowFrontCardImages},
		{settingShowArticleCardImages, in.ShowArticleCardImages},
		{settingShowRelatedCardImages, in.ShowRelatedCardImages},
	} {
		if b.value != nil {
			updates = append(updates, update{b.key, formatSettingBool(*b.value)})
		}
	}
	// 多言語対応の ON/OFF は、管理画面の基本設定の保存と同じ形（"1"・"0"）で保存する。
	if in.MultilingualEnabled != nil {
		flag := "0"
		if *in.MultilingualEnabled {
			flag = "1"
		}
		updates = append(updates, update{settingMultilingualEnabled, flag})
	}
	if in.ShowAuthor != nil {
		// 管理画面のサイト基本設定の保存と同じ形（"1" / "0"）で保存する。
		updates = append(updates, update{settingShowAuthor, formatShowAuthor(*in.ShowAuthor)})
	}
	// AI 機能の ON/OFF（handlers_ai_switch.go）も、基本設定の保存と同じ形で保存する。環境変数 SORUS_AI=off は保存した値より
	// 優先するため、保存しても公開サーバーでは OFF のまま（応答の AIForcedOff で PC に伝える）。
	if in.AIEnabled != nil {
		updates = append(updates, update{settingAIEnabled, formatAIEnabled(*in.AIEnabled)})
	}
	hasStructure := in.SiteMenu != nil || in.SiteFooterMenu != nil || in.FrontPage != nil || in.FrontCards != nil || in.TopGalleryTag != nil ||
		in.TopGalleryLimit != nil || in.TopGalleryTitle != nil || in.TopGalleryLayout != nil || in.TopGalleryLinkURL != nil || in.TopGalleryLinkLabel != nil
	if len(updates) == 0 && !hasStructure && in.Theme == nil && !in.RevokeAllMCP {
		return echo.NewHTTPError(http.StatusBadRequest, "更新する設定を指定してください")
	}
	var st SiteStructure
	if hasStructure {
		st = s.loadSiteStructure(ctx)
		if in.SiteMenu != nil {
			st.Menu = *in.SiteMenu
		}
		if in.SiteFooterMenu != nil {
			st.FooterMenu = *in.SiteFooterMenu
		}
		if in.FrontPage != nil {
			st.FrontPage = strings.TrimSpace(*in.FrontPage)
		}
		if in.FrontCards != nil {
			st.FrontCards = *in.FrontCards
		}
		if in.TopGalleryTag != nil {
			st.TopGalleryTag = strings.TrimSpace(*in.TopGalleryTag)
		}
		if in.TopGalleryLimit != nil {
			st.TopGalleryLimit = *in.TopGalleryLimit
		}
		if in.TopGalleryTitle != nil {
			st.TopGalleryTitle = strings.TrimSpace(*in.TopGalleryTitle)
		}
		if in.TopGalleryLayout != nil {
			st.TopGalleryLayout = strings.TrimSpace(*in.TopGalleryLayout)
		}
		if in.TopGalleryLinkURL != nil {
			st.TopGalleryLinkURL = strings.TrimSpace(*in.TopGalleryLinkURL)
		}
		if in.TopGalleryLinkLabel != nil {
			st.TopGalleryLinkLabel = strings.TrimSpace(*in.TopGalleryLinkLabel)
		}
		// 基本設定を書き込む前に検査し、サイト構成が不正なら基本設定も変えない。
		if err := validateSiteStructure(st); err != nil {
			return syncStructureError(err)
		}
	}

	// テーマは最後に検査する（ほかの項目が不正なら、独自テーマのファイルを展開しない）。
	var staged *theme.StagedTheme
	if in.Theme != nil {
		s.themeMu.Lock()
		defer s.themeMu.Unlock()
		if in.ThemePackage != nil {
			var err error
			if staged, err = s.stageSyncTheme(c, *in.Theme, in.ThemePackage); err != nil {
				return err
			}
			defer staged.Close()
			updates = append(updates, update{"theme", staged.Theme.ID})
		} else {
			t, ok := s.themeCatalog().Get(*in.Theme)
			if !ok {
				return echo.NewHTTPError(http.StatusUnprocessableEntity,
					fmt.Sprintf("公開サーバーの sorus にテーマ「%s」がありません。公開サーバーの sorus を更新してください", strings.TrimSpace(*in.Theme)))
			}
			updates = append(updates, update{"theme", t.ID})
		}
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, u.key, u.value); err != nil {
			return fmt.Errorf("server: sync settings: %w", err)
		}
	}
	if hasStructure {
		if err := s.saveSiteStructureTx(ctx, tx, st); err != nil {
			return syncStructureError(err)
		}
	}
	var revokedTokens, revokedApps int
	if in.RevokeAllMCP {
		if revokedTokens, revokedApps, err = revokeAllMCPAccess(ctx, tx); err != nil {
			return fmt.Errorf("server: sync settings: %w", err)
		}
	}
	if staged != nil {
		if err := s.commitSyncTheme(c, staged); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		if staged != nil {
			c.Logger().Errorf("sync settings: commit: %v", err)
			return s.rollbackSyncTheme(c, staged)
		}
		return fmt.Errorf("server: sync settings: %w", err)
	}

	// サーバーメモリ上の設定値も更新する（handleGeneralSettingsSave と同じく、空の BaseURL では上書きしない）。
	for _, u := range updates {
		switch u.key {
		case "site_name":
			s.setSiteName(u.value)
		case "site_description":
			s.setSiteDescription(u.value)
		case "base_url":
			if u.value != "" {
				s.setBaseURL(u.value)
			}
		case settingShowAuthor:
			s.setShowAuthor(u.value == "1")
		}
	}

	saved := s.loadSiteStructure(ctx)
	if saved.Menu == nil {
		saved.Menu = []MenuItem{}
	}
	// フッターのサイトマップは、子項目とサイトマップを知っていることを伝えるため、空でも空の配列で返す。
	footer := saved.FooterMenu
	if footer == nil {
		footer = []MenuItem{}
	}
	if saved.FrontCards == nil {
		saved.FrontCards = []string{}
	}
	multilingual := s.isMultilingualEnabled()
	showAuthor := s.getShowAuthor()
	aiOn := s.aiEnabled()
	if in.RevokeAllMCP {
		c.Logger().Infof("sync api: revoked %d MCP tokens and %d OAuth app connections", revokedTokens, revokedApps)
	}
	var themeResult *SyncThemePackageResult
	if staged != nil {
		themeResult = &SyncThemePackageResult{ID: staged.Theme.ID, SHA256: syncThemeDigest(in.ThemePackage.Archive)}
	}
	return c.JSON(http.StatusOK, SyncSettingsResponse{
		Action:           SyncActionSyncSettings,
		ThemePackage:     themeResult,
		RevokedMCPTokens: revokedTokens,
		RevokedOAuthApps: revokedApps,
		Settings: SyncSettingsValues{
			SiteName:        s.getSetting("site_name", s.getSiteName()),
			SiteDescription: s.getSetting("site_description", s.getSiteDescription()),
			BaseURL:         s.getSetting("base_url", s.getBaseURL()),
			Copyright:       s.getSetting("copyright", ""),
			AnalyticsTag:    s.getSetting("analytics_tag", ""),
			Theme:           s.themeCatalog().Resolve(s.getSetting("theme", theme.Default)).ID,
			SiteMenu:        saved.Menu,
			SiteFooterMenu:  &footer,
			FrontPage:       saved.FrontPage,
			FrontCards:      saved.FrontCards,
			TopGalleryTag:   saved.TopGalleryTag,
			TopGalleryLimit: saved.TopGalleryLimit,
			TopGalleryTitle: saved.TopGalleryTitle,
			// フォトギャラリーの並べ方と「すべて見る」リンクは、この項目を知っていることを伝えるため常に返す。
			TopGalleryLayout:    &saved.TopGalleryLayout,
			TopGalleryLinkURL:   &saved.TopGalleryLinkURL,
			TopGalleryLinkLabel: &saved.TopGalleryLinkLabel,
			// 多言語対応の ON/OFF は、保存した値（無ければ英語の記事の有無から決めた値）を返す。
			MultilingualEnabled: &multilingual,
			ShowAuthor:          &showAuthor,
			// AI 機能は、環境変数の強制 OFF を含めた公開サーバーの実際の状態を返す。
			AIEnabled:   &aiOn,
			AIForcedOff: s.aiEnvForcedOff(),
		},
	})
}

// syncStructureError はサイト構成の検証エラーを 422 にする。それ以外（DB のエラーなど）は内部エラーとして包む。
func syncStructureError(err error) error {
	var se *siteStructureError
	if errors.As(err, &se) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "サイト構成: "+se.Error())
	}
	return fmt.Errorf("server: sync settings: structure: %w", err)
}

// normalizeSyncBaseURL は BaseURL を検査し、末尾の / を除いて返す。空文字は設定の解除として受け付ける。
// http・https の絶対 URL で、ユーザー情報・クエリ・フラグメントを含まないものだけを許す。
func normalizeSyncBaseURL(raw string) (string, error) {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || utf8.RuneCountInString(v) > maxSettingRunes {
		return "", echo.NewHTTPError(http.StatusUnprocessableEntity, "BaseURL には https://example.com の形式の URL を指定してください")
	}
	return v, nil
}

// syncUser はトークンの jti（claims.ID）に基づく固有のユーザー ID を返す。
// 招待の名前文字列ではなくトークンの一意な jti を利用者ID（sync-tok-<jti>）に結びつけ、
// 名前は表示用（display_name）としてのみ保持する。
// 公開サーバーには管理画面のログインが無いため、作成するユーザーはログインできない（unusablePasswordHash）。
// 記事の執筆者（author_id）と編集ロックの保持者に使う。
func (s *Server) syncUser(ctx context.Context, claims *auth.Claims) (string, error) {
	sub := strings.TrimSpace(claims.Subject)
	displayName := sub
	if r := []rune(displayName); len(r) > maxSyncDisplayNameRunes {
		displayName = string(r[:maxSyncDisplayNameRunes])
	}

	userID := syncUserIDPrefix + "tok-" + claims.ID

	var id string
	err := s.db.GetContext(ctx, &id, `SELECT id FROM users WHERE id = ?`, userID)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("server: sync user: %w", err)
	}

	// 並行する要求が同じユーザーを先に作成した場合は、そのユーザーを使う。
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users (id, username, display_name, password_hash, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET display_name = excluded.display_name`,
		userID, userID, displayName, unusablePasswordHash, claims.Role, time.Now().UTC()); err != nil {
		return "", fmt.Errorf("server: sync user: create: %w", err)
	}
	if err := s.db.GetContext(ctx, &id, `SELECT id FROM users WHERE id = ?`, userID); err != nil {
		return "", fmt.Errorf("server: sync user: create %s: %w", userID, err)
	}
	return id, nil
}

// syncCategoryID は name（スラッグまたは名前、スラッグの一致を優先）のカテゴリの ID を返す。
// 未指定または公開サーバーに無いカテゴリなら 0 を返す。公開サーバーではカテゴリを作成できないため、
// 見つからないカテゴリでも提出は拒否しない。
func (s *Server) syncCategoryID(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	var id int64
	err := s.db.GetContext(ctx, &id, `SELECT id FROM categories WHERE slug = ? OR name = ?
		ORDER BY slug = ? DESC, id LIMIT 1`, name, name, name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("server: sync category: %w", err)
	}
	return id, nil
}

// syncDefaultCategoryID は新規の記事でカテゴリが決まらないときのカテゴリ（ID が最小のもの）を返す。
func (s *Server) syncDefaultCategoryID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.GetContext(ctx, &id, `SELECT id FROM categories ORDER BY id LIMIT 1`); err != nil {
		return 0, fmt.Errorf("server: sync default category: %w", err)
	}
	return id, nil
}

// lockForSync は記事 id の編集ロックを userID で獲得し、操作の後に呼ぶ解放関数を返す。
// 記事が無ければ 404、他のユーザーが管理画面で編集中なら 409 を返す。操作の前から userID がロックを
// 持っていた場合は、解放関数を呼んでもロックを残す。
func (s *Server) lockForSync(ctx context.Context, id, userID string) (func(), error) {
	held := false
	if pre, err := s.locker.Check(ctx, id, userID); err == nil {
		held = pre.IsLocked && pre.IsOwnLock
	}
	info, err := s.locker.Acquire(ctx, id, userID)
	switch {
	case errors.Is(err, lock.ErrArticleNotFound):
		return nil, echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	case errors.Is(err, lock.ErrAlreadyLocked):
		return nil, echo.NewHTTPError(http.StatusConflict, info.UserName+"さんが編集中のため変更できません")
	case err != nil:
		return nil, err
	}
	return func() {
		if !held {
			// 要求が中断されてもロックを残さないよう、キャンセルされない context で解放する。
			_ = s.locker.Release(context.WithoutCancel(ctx), id, userID)
		}
	}, nil
}

// syncSnapshot は記事のリビジョンを userID の操作として記録する。保存自体は成功しているため、
// 失敗してもログに残すだけにする。
func (s *Server) syncSnapshot(c echo.Context, a *article.Article, userID, comment string) {
	if _, _, err := s.revisions.SaveSnapshot(c.Request().Context(), a, userID, comment); err != nil {
		c.Logger().Warnf("revision: snapshot article %s: %v", a.ID, err)
	}
}

// syncArticleRef は記事 a の公開サーバー上の状態を応答用にまとめる。
func (s *Server) syncArticleRef(ctx context.Context, a *article.Article) (SyncArticleRef, error) {
	var category string
	err := s.db.GetContext(ctx, &category, `SELECT slug FROM categories WHERE id = ?`, a.CategoryID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SyncArticleRef{}, fmt.Errorf("server: sync article: %w", err)
	}
	return SyncArticleRef{
		ID:          a.ID,
		Slug:        a.Slug,
		Status:      a.Status,
		Kind:        a.Kind,
		Category:    category,
		URL:         s.getBaseURL() + a.URL(),
		UpdatedAt:   a.UpdatedAt,
		PublishedAt: a.PublishedAt,
	}, nil
}

// syncActorName は同期 API の要求を出した人の名前（トークンの sub。syncUser が表示名にする値と同じ）を返す。
// 編集中の印（article.Article.CheckedOutBy）にはこの名前を残す。
func syncActorName(claims *auth.Claims) string {
	name := strings.TrimSpace(claims.Subject)
	if r := []rune(name); len(r) > maxSyncDisplayNameRunes {
		name = string(r[:maxSyncDisplayNameRunes])
	}
	return name
}

// checkSyncCheckout は、記事 a にほかの PC の有効な編集中の印があれば 409 を返す。force なら確かめない。
// 印を付けた PC は識別子（clientID、SyncRequest.ClientID）で見分ける。同じ人・同じトークンでも、別の PC の印なら断る。
//   - 要求が識別子を送ったとき: 印にも識別子があり、それが同じときだけ通す。
//   - 識別子を送らない古い PC: 印にも識別子が無く、印を付けた人が actor のときだけ通す（従来どおり名前で見分ける）。
func checkSyncCheckout(a *article.Article, actor, clientID string, force bool, now time.Time) error {
	if force || !a.IsCheckedOut(now) {
		return nil
	}
	clientID = strings.TrimSpace(clientID)
	if clientID != "" && a.CheckedOutClient != "" && a.CheckedOutClient == clientID {
		return nil
	}
	if clientID == "" && a.CheckedOutClient == "" && a.CheckedOutBy == actor {
		return nil
	}
	msg := a.CheckedOutBy + "さんが PC で編集中のため変更できません"
	if a.CheckedOutBy == actor && a.CheckedOutClient != clientID {
		msg = a.CheckedOutBy + "さんが別の PC で編集中のため変更できません"
	}
	// PC が「引き継ぐ」（force）を出せるよう、理由の種類（code）と編集中の人を添える。
	return echo.NewHTTPError(http.StatusConflict, map[string]any{
		"message":        msg,
		"code":           SyncCheckedOutCode,
		"checked_out_by": a.CheckedOutBy,
	})
}

// syncUpdatedAtDiffers は公開サーバーの記事の更新日時 server が、PC が最後に取得したときの base と1秒より大きくずれているかを返す。
// DB と JSON を通ると秒未満が丸められることがあるため、1秒までのずれは同じ版とみなす。
func syncUpdatedAtDiffers(server, base time.Time) bool {
	d := server.Sub(base)
	if d < 0 {
		d = -d
	}
	return d > time.Second
}

// syncConflict は、PC が最後に取得したときから記事 a が公開サーバーで変わっていたときの 409 の応答を書く。
// PC が差分を見せられるよう、公開サーバーの今のタイトル・本文・更新日時・執筆者を返す。
func (s *Server) syncConflict(c echo.Context, a *article.Article) error {
	names, err := s.userNames(c, []string{a.AuthorID})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusConflict, SyncConflictResponse{
		Code:                  SyncConflictCode,
		Message:               "PC で取得した後に、公開サーバーでこの記事が変更されました。保存していません。公開サーバーの内容を確かめてください",
		ArticleID:             a.ID,
		ServerUpdatedAt:       a.UpdatedAt,
		ServerTitle:           a.Title,
		ServerContentMarkdown: a.ContentMarkdown,
		ServerAuthorName:      names[a.AuthorID],
	})
}

// clearSyncCheckout は提出・公開・予約を終えた記事 a の編集中の印を外す。操作は終わっているため、失敗してもログに残すだけにする。
func (s *Server) clearSyncCheckout(c echo.Context, a *article.Article) {
	if a.CheckedOutBy == "" && a.CheckedOutAt == nil {
		return
	}
	if err := s.articles.ClearCheckout(c.Request().Context(), a.ID); err != nil {
		c.Logger().Warnf("sync: clear checkout of article %s: %v", a.ID, err)
		return
	}
	a.CheckedOutBy, a.CheckedOutClient, a.CheckedOutAt = "", "", nil
}

// decorateSyncPending は fetch_pending で返す記事 p から期限の切れた編集中の印を外し、確認待ちの変更案を添える。
func (s *Server) decorateSyncPending(c echo.Context, p *SyncPendingArticle, now time.Time) error {
	if !(&article.Article{CheckedOutBy: p.CheckedOutBy, CheckedOutAt: p.CheckedOutAt}).IsCheckedOut(now) {
		p.CheckedOutBy, p.CheckedOutClient, p.CheckedOutAt = "", "", nil
	}
	cur := article.Article{ID: p.ID, Title: p.Title, Excerpt: p.Excerpt, ContentMarkdown: p.ContentMarkdown}
	summary, err := s.syncProposalSummary(c, &cur)
	if err != nil {
		return err
	}
	p.PendingProposal = summary
	return nil
}

// syncProposalSummary は記事 cur に届いている変更案を、cur に重ねた後のタイトル・抜粋・本文とともに返す。無ければ nil。
func (s *Server) syncProposalSummary(c echo.Context, cur *article.Article) (*SyncProposalSummary, error) {
	p, err := mcp.LoadProposal(c.Request().Context(), s.db, cur.ID)
	if err != nil || p == nil {
		return nil, err
	}
	names, err := s.userNames(c, []string{p.ProposedBy})
	if err != nil {
		return nil, err
	}
	by := names[p.ProposedBy]
	if by == "" {
		by = unknownSaver
	}
	next := p.Changes.Apply(*cur)
	out := &SyncProposalSummary{
		ID: p.ID, Kind: p.Kind, ProposedBy: by, CreatedAt: p.CreatedAt,
		HasTitle: p.Changes.Title != nil, HasExcerpt: p.Changes.Excerpt != nil, HasContent: p.Changes.ContentMarkdown != nil,
	}
	if out.HasTitle {
		out.Title = next.Title
	}
	if out.HasExcerpt {
		out.Excerpt = next.Excerpt
	}
	if out.HasContent {
		out.ContentMarkdown = next.ContentMarkdown
	}
	return out, nil
}

// syncCheckout は記事 article_id に、要求を出した人が PC の sorus で編集中の印を付ける（付け直すと開始日時を今にする）。
// 記事が無ければ 404、ほかの人の有効な印があれば force を付けない限り 409、ほかの人が編集ロックを持っていれば 409 を返す。
// 印を付けても記事の更新日時は変えない（PC は応答の updated_at を、次の提出の base_updated_at に使う）。
func (s *Server) syncCheckout(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	actor := syncActorName(claims)
	if actor == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "このトークンには名前（sub）が無いため、編集中の印を付けられません")
	}
	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	release, err := s.lockForSync(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return err
	}
	now := clockNow().UTC()
	if err := checkSyncCheckout(a, actor, req.ClientID, req.Force, now); err != nil {
		return err
	}
	if err := s.articles.SetCheckout(ctx, id, actor, req.ClientID, now); err != nil {
		return fmt.Errorf("server: sync checkout: %w", err)
	}
	if a, err = s.articles.GetByID(id); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SyncCheckoutResponse{Action: SyncActionCheckout, ArticleID: id,
		CheckedOutBy: a.CheckedOutBy, CheckedOutAt: a.CheckedOutAt, CheckedOutClient: a.CheckedOutClient, UpdatedAt: a.UpdatedAt})
}

// syncReleaseCheckout は記事 article_id の編集中の印を外す。印が無い・期限が切れていても成功とする。
// 記事が無ければ 404、ほかの人の有効な印は force を付けない限り外さずに 409 を返す。
func (s *Server) syncReleaseCheckout(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return err
	}
	if err := checkSyncCheckout(a, syncActorName(claims), req.ClientID, req.Force, clockNow()); err != nil {
		return err
	}
	if err := s.articles.ClearCheckout(ctx, id); errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return fmt.Errorf("server: sync release checkout: %w", err)
	}
	return c.JSON(http.StatusOK, SyncCheckoutResponse{Action: SyncActionReleaseCheckout, ArticleID: id, UpdatedAt: a.UpdatedAt})
}

// syncResolveProposal は記事 article_id に届いている AI アプリからの変更案を、proposal_action に従って記事に取り込む
// （accept）か、記事を変えずに破棄する（discard）。取り込みは管理画面で案を反映して保存したときと同じく、今の記事に
// 案を重ねて保存し、版を残してから案を消す。翻訳の案で原文が変わっていた（確かめられない）ときは要更新の印を残す。
//
// proposal_action が不正なら 400、記事や案が無ければ 404、proposal_id の案が新しい案に置き換わっていれば 409、
// ほかの人の編集中の印・編集ロックがあれば 409 を返す。応答の Article は操作した後の記事。
func (s *Server) syncResolveProposal(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	action := strings.TrimSpace(req.ProposalAction)
	if action != SyncProposalAccept && action != SyncProposalDiscard {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("proposal_action には %s か %s を指定してください（指定: %q）", SyncProposalAccept, SyncProposalDiscard, req.ProposalAction))
	}
	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	release, err := s.lockForSync(ctx, id, userID)
	if err != nil {
		return err
	}
	defer release()
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	} else if err != nil {
		return err
	}
	if err := checkSyncCheckout(a, syncActorName(claims), req.ClientID, req.Force, clockNow()); err != nil {
		return err
	}
	p, err := mcp.LoadProposal(ctx, s.db, id)
	if err != nil {
		return err
	}
	if p == nil {
		return echo.NewHTTPError(http.StatusNotFound, "この記事に確認待ちの変更案はありません")
	}
	if want := strings.TrimSpace(req.ProposalID); want != "" && want != p.ID {
		return echo.NewHTTPError(http.StatusConflict, "確かめた変更案は、その後に届いた新しい案に置き換わりました。今の案を確かめてください")
	}

	if action == SyncProposalAccept {
		keepOutdated := false
		if ps := s.checkProposalSource(c, p, a); ps != nil {
			keepOutdated = ps.Status != sourceCurrent
		}
		oldTitle, oldContent := a.Title, a.ContentMarkdown
		next := p.Changes.Apply(*a)
		if keepOutdated {
			next.IsOutdated = true
		}
		if err := s.articles.Save(&next); err != nil {
			if errors.Is(err, article.ErrInvalid) {
				return echo.NewHTTPError(http.StatusUnprocessableEntity, "変更案を取り込めませんでした。案の内容を確認してください")
			}
			return err
		}
		if next.Lang == "ja" && next.TranslationGroupID != "" && (oldTitle != next.Title || oldContent != next.ContentMarkdown) {
			_ = s.articles.MarkTranslationsOutdated(ctx, next.TranslationGroupID, next.ID)
		}
		if p.IsTranslation() && next.IsOutdated && !keepOutdated {
			_ = s.articles.ClearOutdated(ctx, next.ID)
		}
		s.reindexArticle(c, id)
		s.trackScheduled(c, id)
		if a, err = s.articles.GetByID(id); err != nil {
			return err
		}
		s.syncSnapshot(c, a, userID, proposalComment("同期で変更案を取り込み", p.ID))
	}
	if _, err := mcp.DeleteProposal(ctx, s.db, id, p.ID); err != nil {
		return fmt.Errorf("server: sync resolve proposal: %w", err)
	}

	out := []SyncPendingArticle{}
	if err := s.db.SelectContext(ctx, &out, syncPendingSelect+`WHERE a.id = ?`, id); err != nil {
		return fmt.Errorf("server: sync resolve proposal: %w", err)
	}
	if len(out) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	}
	if err := s.decorateSyncPending(c, &out[0], clockNow()); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, SyncResolveProposalResponse{Action: SyncActionResolveProposal, ProposalAction: action, Article: out[0]})
}
