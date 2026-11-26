package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
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

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/image"
	"sorus/internal/markdown"
	"sorus/internal/theme"
)

// ローカル CMS の管理画面から公開サーバーの同期 API を呼ぶ「公開サーバー」の画面と API。
// 公開サーバーの URL・同期 API のパスとケーパビリティ・トークンは settings テーブルに保存し、トークンはブラウザに返さない。
// 参加は招待コード1つで行い（handlers_remote_invite.go）、URL とトークンを直接入力する欄は「詳細設定」に残す。
// ブラウザ（static/js/remote_sync.js）はローカルの /admin/remote/* を呼び、ローカル CMS が公開サーバーへ中継する。

const (
	// remoteSettingURL・remoteSettingSyncPath・remoteSettingToken は公開サーバーの URL、同期 API のパス、
	// トークンを保存する settings のキー。同期 API のパスが空なら DefaultSyncAPIPath を使う。
	remoteSettingURL      = "remote_server_url"
	remoteSettingSyncPath = "remote_sync_path"
	remoteSettingToken    = "remote_token"
	// remoteSettingClientID は、この PC の sorus を公開サーバーに見分けてもらう識別子（16バイトの乱数の16進数）を保存する
	// settings のキー。同じ人・同じ招待のトークンを複数の PC で使っても、編集中の印がどの PC のものかを見分ける。
	remoteSettingClientID = "remote_client_id"

	// remoteTimeout は公開サーバーへの1回の要求の制限時間。画像を含む提出に間に合う長さにする。
	remoteTimeout = 60 * time.Second
	// maxRemoteResponseBytes は公開サーバーの応答として読む上限。fetch_pending は本文を最大200件含む。
	maxRemoteResponseBytes = 64 << 20
	// maxRemoteImageBytes は1回の提出で送る画像の合計バイト数。Base64 で 4/3 倍になっても
	// 公開サーバーのリクエストボディの上限（bodyLimit = 20MB）に収まるようにする。
	maxRemoteImageBytes = 14 << 20
	// maxRemoteTokenLength は保存を受け付けるトークンの長さ。正規のトークンは数百バイトに収まる。
	maxRemoteTokenLength = 4096
	// remotePingAction は接続テストで送る、公開サーバーに無い action。公開サーバーはトークンを検証してから
	// action を判定するため、400（未対応の action）が返ればトークンが受け入れられたと分かる。
	remotePingAction = "ping"
)

var (
	// remoteImageRefPattern は記事の本文・アイキャッチから参照されているアップロード画像（/uploads/<ハッシュ>-<サイズ>.webp）。
	remoteImageRefPattern = regexp.MustCompile(`/uploads/([0-9a-f]{32})-(?:original|[0-9]{1,5})\.webp`)
	// remoteImageVariants は image.Process が1枚の画像から生成するファイルのサイズ名。
	remoteImageVariants = []string{"original", fmt.Sprint(image.WidthLarge), fmt.Sprint(image.WidthMedium), fmt.Sprint(image.WidthSmall)}

	// remoteHTTPClient は公開サーバーを呼ぶ HTTP クライアント。リダイレクトを追うと POST が GET に変わり、
	// 別のホストへトークンを送るおそれもあるため、追わずにエラーとして扱う。
	remoteHTTPClient = &http.Client{
		Timeout:       remoteTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
)

func init() {
	if !slices.Contains(pageNames, "settings_remote") {
		pageNames = append(pageNames, "settings_remote")
	}
}

// registerRemoteRoutes は「公開サーバー」の画面と API を、ログイン済み・CSRF 保護済みの /admin グループに登録する。
func (s *Server) registerRemoteRoutes(authed *echo.Group) {
	// 提出は記事を書く人（ライター・編集者）、校閲待ちの取得・取り込み・公開は編集者、サイト設定の反映は管理者。
	// ローカルの権限マトリクス（server.go の routes）と同じ分担にする。接続先の URL とトークンはサイトで1つのため、
	// 設定の画面・保存・招待での参加・接続テストも管理者だけにする（管理者は編集者・ライターの役割も持つ）。
	authorOnly := auth.RequireRole(auth.RoleEditor, auth.RoleWriter)
	editorOnly := auth.RequireRole(auth.RoleEditor)
	adminOnly := auth.RequireRole(auth.RoleAdmin)

	authed.GET("/settings/remote", s.handleRemoteSettings, adminOnly)
	authed.POST("/settings/remote", s.handleRemoteSettingsSave, adminOnly)
	authed.POST("/settings/remote/join", s.handleRemoteJoin, adminOnly)
	authed.POST("/remote/test", s.handleRemoteTest, adminOnly)
	// 招待の発行・一覧・取り消しは、ローカルの管理者が admin のトークンで行う。
	authed.GET("/remote/tokens", s.handleRemoteTokens, adminOnly)
	authed.POST("/remote/invites", s.handleRemoteInvite, adminOnly)
	authed.POST("/remote/tokens/:jti/revoke", s.handleRemoteRevoke, adminOnly)
	authed.POST("/remote/articles/:id/submit", s.handleRemoteSubmit, authorOnly)
	authed.GET("/remote/pending", s.handleRemotePending, editorOnly)
	// 校閲待ちの一覧・プレビューの画像。公開サーバーが閲覧制限を有効にしていても見られるよう、PC がトークンで取得して返す。
	authed.GET("/remote/uploads/:name", s.handleRemoteUpload, editorOnly)
	authed.POST("/remote/pending/:id/preview", s.handleRemotePreview, editorOnly)
	authed.POST("/remote/pending/:id/import", s.handleRemoteImport, editorOnly)
	// 提出の食い違いで「サーバー版を取り込む」は書き手も使うため、提出と同じ立場（writer, editor）に許す。
	authed.POST("/remote/pending/:id/pull-latest", s.handleRemotePullLatest, authorOnly)
	authed.POST("/remote/pending/:id/proposal", s.handleRemoteResolveProposal, editorOnly)
	authed.POST("/remote/pending/:id/publish", s.handleRemotePublish, editorOnly)
	authed.POST("/remote/pending/:id/schedule", s.handleRemoteSchedule, editorOnly)
	authed.POST("/remote/scheduled/:id/unschedule", s.handleRemoteUnschedule, editorOnly)
	authed.POST("/remote/settings/push", s.handleRemoteSettingsPush, adminOnly)
	// 公開中の記事・添付資料・フォトギャラリーの写真の一括同期（handlers_remote_bulk.go）。
	authed.GET("/remote/bulk-sync", s.handleRemoteBulkStatus, adminOnly)
	authed.POST("/remote/bulk-sync", s.handleRemoteBulkStart, adminOnly)
	// 公開サーバー自身の閲覧制限（Basic 認証）の確認・変更は、ローカルの管理者が admin のトークンで行う。
	authed.GET("/remote/site-access", s.handleRemoteSiteAccess, adminOnly)
	authed.POST("/remote/site-access", s.handleRemoteSiteAccessSave, adminOnly)
	// 公開サーバーのメンテナンス表示の確認・切り替えも、ローカルの管理者が admin のトークンで行う。
	authed.GET("/remote/maintenance", s.handleRemoteMaintenance, adminOnly)
	authed.POST("/remote/maintenance", s.handleRemoteMaintenanceSave, adminOnly)
	authed.POST("/remote/maintenance/page", s.handleRemoteMaintenancePage, adminOnly)
	authed.POST("/remote/maintenance/apply", s.handleRemoteMaintenanceApply, adminOnly)
	authed.GET("/remote/maintenance/page/download", s.handleRemoteMaintenancePageDownload, adminOnly)
}

// remoteConfig は保存済みの公開サーバーの接続の設定。
type remoteConfig struct {
	// URL は公開サーバーの URL（例: https://example.com）。画像の取得や公開ページの URL の起点にもする。
	URL string
	// SyncPath は公開サーバーの同期 API のパス（例: /api/sync-<秘匿パス>）。空なら DefaultSyncAPIPath。
	SyncPath string
	Token    string
}

// remoteClientID は、この PC の sorus の識別子（SyncRequest.ClientID）を返す。まだ無ければ 16バイトの乱数で作って
// settings に保存する（同時に呼ばれても、先に保存された値を全員が使う）。読み書きに失敗したら空を返し、
// 公開サーバーは従来どおり名前（sub）だけで見分ける。
func (s *Server) remoteClientID(ctx context.Context) string {
	var id string
	if err := s.db.GetContext(ctx, &id, `SELECT value FROM settings WHERE key = ?`, remoteSettingClientID); err == nil && id != "" {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP WHERE settings.value = ''`,
		remoteSettingClientID, hex.EncodeToString(b[:])); err != nil {
		return ""
	}
	if err := s.db.GetContext(ctx, &id, `SELECT value FROM settings WHERE key = ?`, remoteSettingClientID); err != nil {
		return ""
	}
	return id
}

func (s *Server) loadRemoteConfig() remoteConfig {
	return remoteConfig{
		URL:      s.getSetting(remoteSettingURL, ""),
		SyncPath: s.getSetting(remoteSettingSyncPath, ""),
		Token:    s.getSetting(remoteSettingToken, ""),
	}
}

// endpoint は同期 API の完全な URL を返す。
func (c remoteConfig) endpoint() string {
	if c.SyncPath == "" {
		return c.URL + DefaultSyncAPIPath
	}
	return c.URL + c.SyncPath
}

// formURL は設定画面の URL 欄に表示する値。同期 API のパスを含めた完全な URL を表示し、そのまま保存し直せるようにする。
// 同期 API のパスが既定で、URL にもパスが無ければ、入力されたとおり公開サーバーの URL だけを表示する。
func (c remoteConfig) formURL() string {
	if c.URL == "" {
		return ""
	}
	if u, err := url.Parse(c.URL); c.SyncPath == "" && err == nil && u.Path == "" {
		return c.URL
	}
	return c.endpoint()
}

// remoteTokenInfo はトークンに書かれている内容。ローカル CMS は公開サーバーの公開鍵を持たないため署名は検証せず、
// 画面の表示と、送る前の権限の確認にだけ使う（最終的な検証は公開サーバーが行う）。
type remoteTokenInfo struct {
	// ID はトークン ID（jti）。
	ID        string
	Subject   string
	Role      auth.Role
	IssuedAt  time.Time
	ExpiresAt time.Time
	Expired   bool
}

// remotePermissions はトークンのロールで実行できる同期 API の操作。
type remotePermissions struct {
	SubmitDraft    bool `json:"submit_draft"`
	FetchPending   bool `json:"fetch_pending"`
	PublishArticle bool `json:"publish_article"`
	SyncSettings   bool `json:"sync_settings"`
}

func permissionsFor(role auth.Role) remotePermissions {
	return remotePermissions{
		SubmitDraft:    auth.CanSubmitDraft(role),
		FetchPending:   auth.CanFetchPending(role),
		PublishArticle: auth.CanPublish(role),
		SyncSettings:   auth.CanSyncSettings(role),
	}
}

var errRemoteTokenFormat = errors.New("トークンの形式が正しくありません。発行されたトークンを改行や空白を含めずにそのまま貼り付けてください")
var errRemoteTokenNeedsRenewal = errors.New("この招待コードは現在使えません。公開サーバーのサイト管理者に新しい招待コードを発行してもらってください")

// normalizeRemoteToken は貼り付けたトークンから改行・空白を取り除く。
func normalizeRemoteToken(raw string) string { return strings.Join(strings.Fields(raw), "") }

// parseRemoteToken はトークンの payload を読む。署名は検証しない（remoteTokenInfo を参照）。
func parseRemoteToken(token string, now time.Time) (*remoteTokenInfo, error) {
	if token == "" || len(token) > maxRemoteTokenLength {
		return nil, errRemoteTokenFormat
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return nil, errRemoteTokenFormat
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errRemoteTokenFormat
	}
	var claims auth.Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errRemoteTokenFormat
	}
	if claims.ID == "" {
		return nil, errRemoteTokenNeedsRenewal
	}
	switch claims.Role {
	case auth.RoleWriter, auth.RoleEditor, auth.RoleAdmin:
	default:
		return nil, errRemoteTokenFormat
	}
	if strings.TrimSpace(claims.Subject) == "" || claims.ExpiresAt <= 0 || !auth.ValidTokenID(claims.ID) {
		return nil, errRemoteTokenFormat
	}
	info := &remoteTokenInfo{
		ID:        claims.ID,
		Subject:   claims.Subject,
		Role:      claims.Role,
		ExpiresAt: time.Unix(claims.ExpiresAt, 0),
		Expired:   now.Unix() >= claims.ExpiresAt,
	}
	if claims.IssuedAt > 0 {
		info.IssuedAt = time.Unix(claims.IssuedAt, 0)
	}
	return info, nil
}

// normalizeRemoteURL は公開サーバーの URL を検査し、公開サーバーの URL（スキームとホスト）と同期 API のパスに分ける。
// URL にパスがあれば（例: sorus token create が案内する https://example.com/api/sync-<秘匿パス>）、そのパスを
// 同期 API のパスにする。パスが無ければ syncPath は空（DefaultSyncAPIPath を使う）。
// トークンを平文で流さないよう、http:// はこの PC 内（localhost）のサーバーにだけ許す。
func normalizeRemoteURL(raw string) (base, syncPath string, err error) {
	errFormat := errors.New("公開サーバーの URL は https://example.com または https://example.com/api/sync-... の形式で入力してください")
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", "", errFormat
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", errors.New("公開サーバーの URL にはユーザー名・パスワード・? 以降・# 以降を含めないでください")
	}
	switch u.Scheme = strings.ToLower(u.Scheme); u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", "", errors.New("暗号化されない http:// はこの PC 内のサーバー（localhost）にだけ使えます。https:// で始まる URL を入力してください")
		}
	default:
		return "", "", errFormat
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		if syncPath, err = NormalizeSyncAPIPath(p); err != nil {
			return "", "", errors.New("公開サーバーの URL のパス（同期 API のパス）が正しくありません。公開サーバーの管理者から受け取った https://example.com/api/sync-... の URL をそのまま貼り付けてください")
		}
	}
	u.Path, u.RawPath = "", ""
	return u.String(), syncPath, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// remoteError は公開サーバーとの通信の失敗。Status は公開サーバーの HTTP ステータス（接続できなければ 0）。
// Code は公開サーバーがエラーの種類を示した場合の値（応答の code。閲覧制限の API などが付ける）。
type remoteError struct {
	Status  int
	Message string
	Code    string
	// ServerMessage は Message が公開サーバーの応答に書かれた理由か（ステータスから作った説明でないか）。
	ServerMessage bool
	cause         error
	// header は公開サーバーのエラー応答のヘッダー（429 の Retry-After を読む）。
	header http.Header
	// body は公開サーバーのエラー応答の本文（409 の食い違いの内容を読む。remoteSyncFailure）。
	body []byte
}

func (e *remoteError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("remote sync: %s: %v", e.Message, e.cause)
	}
	return fmt.Sprintf("remote sync: status %d: %s", e.Status, e.Message)
}

func (e *remoteError) Unwrap() error { return e.cause }

// callRemote は req を公開サーバーの同期 API に送り、200 の応答を out に読み込む（out が nil なら読み捨てる）。
// 200 以外は remoteError を返す。
func callRemote(ctx context.Context, cfg remoteConfig, req any, out any) error {
	return callRemoteWith(ctx, remoteHTTPClient, cfg, req, out)
}

// 公開サーバーの受付上限（429 Too Many Requests）に当たったときの送り直し。公開サーバーは接続元の IP アドレスごとに1分あたりの
// 要求の数を数え（syncRequestLimit）、超えると期間が明けるまでの秒数を Retry-After で返す。429 は要求を処理する前に返す
// （前段のプロキシの上限も同じ）ため、待ってから同じ要求を送り直してよい。
//
// 画面の操作から送る要求は、サーバーの WriteTimeout（60秒）の中で応答を返せるよう、短い待ちだけ送り直す（既定）。
// 数百件の記事を送る一括同期と、応答の期限を延ばした記事の公開は withRemotePatience で、期間が明けるまで待って送り直す。
const (
	// remoteRateLimitBackoff は Retry-After が無い・読めないときの最初の待ち時間（送り直すたびに倍にする）。
	remoteRateLimitBackoff = 2 * time.Second
)

// remoteRetryPolicy は 429 のあとに送り直す回数の上限（Retries）と、1回に待つ時間の上限（MaxWait。Retry-After がこれより
// 長い＝認証の失敗が多すぎて締め出された などのときは待たずに失敗とする）。Notify は待つたびに待ち時間を、待ち終えたら 0 を受け取る。
type remoteRetryPolicy struct {
	Retries int
	MaxWait time.Duration
	Notify  func(time.Duration)
}

var (
	// remoteRetryDefault は画面の操作から送る要求の送り直し（短い待ちだけ）。
	remoteRetryDefault = remoteRetryPolicy{Retries: 2, MaxWait: 20 * time.Second}
	// remoteRetryPatient は一括同期・記事の公開の送り直し（公開サーバーの1分の期間が明けるまで待つ）。
	remoteRetryPatient = remoteRetryPolicy{Retries: 6, MaxWait: 2 * time.Minute}
)

// remoteRetrySleep は d だけ待つ。ctx が先に終われば ctx.Err() を返す（テストでは待たずに記録するものに差し替える）。
var remoteRetrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// remoteRetryKey は ctx に入れる送り直しの方針（remoteRetryPolicy）のキー。
type remoteRetryKey struct{}

// withRemotePatience は、公開サーバーの受付上限に当たったら期間が明けるまで待って送り直す ctx を返す。notify（nil 可）は
// 待つたびに待ち時間を、待ち終えたら 0 を受け取る（一括同期は進み具合の画面に「待っています」と出す）。
func withRemotePatience(ctx context.Context, notify func(time.Duration)) context.Context {
	p := remoteRetryPatient
	p.Notify = notify
	return context.WithValue(ctx, remoteRetryKey{}, p)
}

// remoteRetryPolicyFor は ctx の送り直しの方針を返す（無ければ remoteRetryDefault）。
func remoteRetryPolicyFor(ctx context.Context) remoteRetryPolicy {
	if p, ok := ctx.Value(remoteRetryKey{}).(remoteRetryPolicy); ok {
		return p
	}
	return remoteRetryDefault
}

// remoteRetryAfter は 429 の応答の Retry-After（秒数か HTTP の日時）から待つ時間を決める。無い・読めないときは
// attempt（0 から数えた送り直しの回数）に応じて倍々に延ばす。
func remoteRetryAfter(h http.Header, attempt int, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := t.Sub(now); d > 0 {
				return d
			}
			return 0
		}
	}
	return remoteRateLimitBackoff << attempt
}

// callRemoteWith は callRemote と同じ要求を client で送る（制限時間の違う要求に使う）。公開サーバーが受付上限（429）を
// 返したときは、Retry-After の時間（無ければ倍々に延ばした時間）だけ待って、ctx の方針（remoteRetryPolicyFor）の回数まで送り直す。
func callRemoteWith(ctx context.Context, client *http.Client, cfg remoteConfig, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("server: remote sync: marshal: %w", err)
	}
	policy := remoteRetryPolicyFor(ctx)
	for attempt := 0; ; attempt++ {
		err := sendRemoteOnce(ctx, client, cfg, body, out)
		var re *remoteError
		if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || attempt >= policy.Retries {
			return err
		}
		wait := remoteRetryAfter(re.header, attempt, time.Now())
		if wait > policy.MaxWait {
			return err
		}
		if policy.Notify != nil {
			policy.Notify(wait)
		}
		serr := remoteRetrySleep(ctx, wait)
		if policy.Notify != nil {
			policy.Notify(0)
		}
		if serr != nil {
			return err
		}
	}
}

// sendRemoteOnce は JSON の本文 body を公開サーバーの同期 API に1回送り、200 の応答を out に読み込む。
func sendRemoteOnce(ctx context.Context, client *http.Client, cfg remoteConfig, body []byte, out any) error {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint(), bytes.NewReader(body))
	if err != nil {
		return &remoteError{Message: "公開サーバーの URL が正しくありません", cause: err}
	}
	hreq.Header.Set(echo.HeaderAuthorization, "Bearer "+cfg.Token)
	hreq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	hreq.Header.Set(echo.HeaderAccept, echo.MIMEApplicationJSON)
	hreq.Header.Set("User-Agent", "sorus-remote-sync")
	res, err := client.Do(hreq)
	if err != nil {
		return &remoteError{Message: "公開サーバーに接続できませんでした。URL とインターネット接続を確認してください", cause: err}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxRemoteResponseBytes+1))
	if err != nil {
		return &remoteError{Status: res.StatusCode, Message: "公開サーバーの応答を受信できませんでした", cause: err}
	}
	if len(data) > maxRemoteResponseBytes {
		return &remoteError{Status: res.StatusCode, Message: "公開サーバーの応答が大きすぎます"}
	}
	if res.StatusCode != http.StatusOK {
		msg := remoteErrorMessage(res.StatusCode, data)
		if res.StatusCode == http.StatusUnauthorized && isBasicChallenge(res.Header) {
			// 公開サーバーの閲覧制限（Basic 認証）は同期 API（そのパスへの POST）にかからない。Basic 認証を求められたのは、
			// 同期 API に届いていない（URL に同期 API のパスが無い・違う）ため。トークンの誤りとは案内しない。
			msg = remoteErrorMessage(http.StatusNotFound, nil)
		}
		return &remoteError{Status: res.StatusCode, Message: msg, Code: remoteErrorCode(data),
			ServerMessage: res.StatusCode >= 400 && remoteServerMessage(data, res.StatusCode) != "", header: res.Header, body: data}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &remoteError{Status: res.StatusCode, Message: "公開サーバーの応答を読み取れませんでした。URL が sorus の公開サーバーか確認してください", cause: err}
	}
	return nil
}

// remoteErrorMessage は公開サーバーのエラー応答（{"message": ...}）から利用者に見せる文言を作る。
// 本文が無い・Echo 既定の英語の文言しか無いときは、ステータスに応じた説明に置き換える。
func remoteErrorMessage(status int, body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	msg := strings.TrimSpace(e.Message)
	if status >= 300 && status < 400 {
		return "公開サーバーの URL が別の URL に転送されました。転送先の URL（https:// で始まる URL など）を設定してください"
	}
	if msg != "" && msg != http.StatusText(status) {
		return msg
	}
	switch status {
	case http.StatusUnauthorized:
		return "トークンが公開サーバーで認められませんでした。有効期限が切れていないか、正しいサーバーのトークンか確認してください"
	case http.StatusForbidden:
		return "このトークンにはこの操作の権限がありません"
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return "公開サーバーで同期 API が見つかりません。URL に同期 API のパス（/api/sync-...）まで正しく含めたか、公開サーバーの管理者に確認してください"
	case http.StatusRequestEntityTooLarge:
		return "送信するデータが大きすぎます。画像を減らしてから再度お試しください"
	case http.StatusTooManyRequests:
		return "公開サーバーが混み合っていて受け付けてもらえませんでした。しばらく待ってから、もう一度お試しください"
	}
	return fmt.Sprintf("公開サーバーでエラーが発生しました（%d）", status)
}

// remoteServerMessage は公開サーバーのエラー応答に書かれた理由を返す。無い・Echo 既定の英語の文言だけなら空。
func remoteServerMessage(body []byte, status int) string {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	if msg := strings.TrimSpace(e.Message); msg != "" && msg != http.StatusText(status) {
		return msg
	}
	return ""
}

// remoteErrorCode は公開サーバーのエラー応答（{"code": ...}）からエラーの種類を取り出す。無ければ空。
func remoteErrorCode(body []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	return strings.TrimSpace(e.Code)
}

// remoteFailure は err をブラウザへの JSON 応答にする。公開サーバーとの通信の失敗は 502 で返す
// （管理画面自体のセッション切れなどと区別できるよう、公開サーバーの 401・403 をそのまま返さない）。
func remoteFailure(c echo.Context, err error) error {
	var re *remoteError
	if !errors.As(err, &re) {
		return err
	}
	if re.cause != nil {
		c.Logger().Warnf("%v", re)
	}
	return c.JSON(http.StatusBadGateway, map[string]any{"message": re.Message, "remote_status": re.Status})
}

// requireRemote は保存済みの設定を返す。未設定・トークンの期限切れ、またはトークンのロールで allowed が
// 許されなければ、公開サーバーに送る前にエラーを返す。
func (s *Server) requireRemote(allowed func(auth.Role) bool, action string) (remoteConfig, error) {
	cfg := s.loadRemoteConfig()
	if cfg.URL == "" || cfg.Token == "" {
		return cfg, echo.NewHTTPError(http.StatusConflict, "公開サーバーが未設定です。"+InviteJoinHint)
	}
	info, err := parseRemoteToken(cfg.Token, time.Now())
	if err != nil {
		return cfg, echo.NewHTTPError(http.StatusConflict, "保存されているトークンを読み取れません。サイト管理者に新しい招待コードを発行してもらい、"+InviteJoinHint)
	}
	if info.Expired {
		return cfg, echo.NewHTTPError(http.StatusConflict, "招待の有効期限が切れています。サイト管理者に新しい招待コードを発行してもらい、"+InviteJoinHint)
	}
	if !allowed(info.Role) {
		return cfg, echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf("設定中のトークン（%s）では%sはできません", info.Role, action))
	}
	return cfg, nil
}

// remoteSyncView はエディタ・ダッシュボードに公開サーバーの部品を出すか。
// CanSubmit は外部ライター向け提出ボタン（partials の remote_sync_submit_button）、
// CanReview は校閲待ち記事の一覧（remote_pending_panel）、CanPublish は直接公開を許可するかを表す。
type remoteSyncView struct {
	CanSubmit  bool
	CanReview  bool
	CanPublish bool
}

// remoteSyncViewFor は保存済みの設定と、ローカルのユーザー user のロールから remoteSyncView を決める。
// 未設定・トークンが読めない・期限切れなら何も表示しない。
func (s *Server) remoteSyncViewFor(user *auth.User) remoteSyncView {
	cfg := s.loadRemoteConfig()
	if user == nil || cfg.URL == "" || cfg.Token == "" {
		return remoteSyncView{}
	}
	info, err := parseRemoteToken(cfg.Token, time.Now())
	if err != nil || info.Expired {
		return remoteSyncView{}
	}
	// 編集者・管理者は、編集者以上のトークンなら直接公開を優先し、提出ボタンは出さない。
	canPublish := user.HasRole(auth.RoleEditor) && auth.CanPublish(info.Role)
	return remoteSyncView{
		CanSubmit:  !canPublish && user.HasRole(auth.RoleWriter) && auth.CanSubmitDraft(info.Role),
		CanReview:  user.HasRole(auth.RoleEditor) && auth.CanFetchPending(info.Role),
		CanPublish: canPublish,
	}
}

// remotePublishDeadline は記事を公開サーバーへ公開する操作の、ブラウザへの応答の書き込みの期限。フォトギャラリーの画像・
// 画像の多い記事の分割送信・添付資料（1件ずつ）を待つため、サーバーの WriteTimeout（60秒）より延ばす。
const remotePublishDeadline = 15 * time.Minute

// remotePublishOutcome は記事を公開サーバーへ公開した結果。記事の公開が成功したあとの、添付資料・フォトギャラリーの
// 画像の送信の結果を持つ（これらを送れなくても記事の公開は取り消さない）。
type remotePublishOutcome struct {
	// Images は送った画像ファイルの数、Requests は記事の公開に使った要求の数（画像が多い記事は分けて送る）。
	Images   int
	Requests int
	// Attachments は添付資料の送信の結果。AttachmentsUnsupported は公開サーバーの sorus が古く、添付資料を送れなかったか
	// （記事に添付資料があるときだけ true）。AttachmentsErr は添付資料を送れなかった理由。
	Attachments            remoteAttachmentResult
	AttachmentsUnsupported bool
	AttachmentsErr         error
	// GalleryUnsupported は公開サーバーの sorus が古く、本文のフォトギャラリーの画像を送れなかったか。
	GalleryUnsupported bool
}

// remoteSendOptions は sendRemoteArticle の選択。
type remoteSendOptions struct {
	// SkipGallery は本文のフォトギャラリーの画像を送らないか（一括同期は、先にまとめて送る）。
	SkipGallery bool
}

// remoteSyncArticle は記事 a を同期 API で送る形にする。publishedAt を付けると、直接公開する記事の公開日時として送る。
func (s *Server) remoteSyncArticle(ctx context.Context, a *article.Article, withPublishedAt bool) (*SyncArticle, error) {
	var category string
	if err := s.db.GetContext(ctx, &category, `SELECT slug FROM categories WHERE id = ?`, a.CategoryID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("server: remote publish: category: %w", err)
	}
	sourceID := ""
	if a.SourceArticleID.Valid {
		sourceID = a.SourceArticleID.String
	}
	out := &SyncArticle{
		ID:                 a.ID,
		Title:              a.Title,
		Slug:               a.Slug,
		ContentMarkdown:    a.ContentMarkdown,
		Excerpt:            a.Excerpt,
		FeaturedImageURL:   remoteFeaturedImageURL(a),
		Category:           category,
		Kind:               a.Kind,
		Lang:               a.Lang,
		TranslationGroupID: a.TranslationGroupID,
		SourceArticleID:    sourceID,
	}
	if withPublishedAt && a.PublishedAt != nil && !a.PublishedAt.IsZero() {
		t := a.PublishedAt.UTC()
		out.PublishedAt = &t
	}
	return out, nil
}

// remoteFeaturedImageURL は記事 a のアイキャッチ画像の URL を同期 API で送る形にする。未設定でも空文字を送り、
// PC で外したアイキャッチを公開サーバーでも外す（nil は「変えない」の意味になるため使わない）。
func remoteFeaturedImageURL(a *article.Article) *string {
	u := strings.TrimSpace(a.FeaturedImage)
	return &u
}

// sendRemoteArticle は記事 a を公開サーバーへ直接公開する（publish_article）。本文のフォトギャラリー（:::gallery）の
// 画像の記録とファイルを先に送り、記事の画像が1回の要求に収まらなければ、同じ記事を画像を分けて何回か送る（同じ内容の
// 公開の繰り返しになるだけで、記事の状態は変わらない）。記事を公開できたら、その記事の添付資料（PDF など）を送る。
// 記事の公開に失敗したらエラーを返し、添付資料・ギャラリーの画像の失敗は結果（remotePublishOutcome）に入れる。
func (s *Server) sendRemoteArticle(ctx context.Context, cfg remoteConfig, a *article.Article, opts remoteSendOptions, warnf func(string, ...any)) (remotePublishOutcome, error) {
	var out remotePublishOutcome
	syncArticle, err := s.remoteSyncArticle(ctx, a, true)
	if err != nil {
		return out, err
	}
	batches, err := s.collectRemoteImageBatches(a)
	if err != nil {
		return out, err
	}
	// 本文のフォトギャラリー（:::gallery）は公開サーバーの media をタグで引くため、公開する前にそのタグの画像の記録と
	// ファイルを送る（handlers_remote_media.go）。sync_media を知らない古い公開サーバーには送らずに記事だけを公開する。
	if tags := galleryTagsIn(a.ContentMarkdown); len(tags) > 0 && !opts.SkipGallery {
		if _, err := s.pushRemoteMedia(ctx, cfg, tags); errors.Is(err, errRemoteMediaUnsupported) {
			warnf("remote publish article %s: the public server is too old to receive gallery images", a.ID)
			out.GalleryUnsupported = true
		} else if err != nil {
			return out, err
		}
	}
	if len(batches) == 0 {
		batches = [][]SyncImage{nil}
	}
	// remoteAttachments は公開サーバーにあるこの記事の添付資料の数（最後の publish_article の応答。古い公開サーバーは返さない）。
	var remoteAttachments *int
	var remoteUpdatedAt time.Time
	var remoteSlug string
	for _, images := range batches {
		var res SyncSubmitResponse
		req := SyncRequest{Action: SyncActionPublishArticle, ArticleID: a.ID, Article: syncArticle, Images: images,
			ClientID: s.remoteClientID(ctx)}
		if err := callRemote(ctx, cfg, req, &res); err != nil {
			return out, err
		}
		out.Requests++
		out.Images += len(images)
		remoteAttachments = res.Attachments
		remoteUpdatedAt = res.Article.UpdatedAt
		remoteSlug = res.Article.Slug
	}
	// 公開サーバーがスラッグを付け直した（同じスラッグの記事があったなど）ときは、この PC の記事もそれに合わせ、
	// 公開ページの URL と食い違わないようにする。
	if remoteSlug != "" && remoteSlug != a.Slug {
		if err := s.articles.AdoptSlug(ctx, a.ID, remoteSlug); err != nil && !errors.Is(err, article.ErrNotFound) {
			warnf("remote publish article %s: adopt slug %q: %v", a.ID, remoteSlug, err)
		} else if err == nil {
			a.Slug = remoteSlug
			s.reindex(s.echo.Logger, a.ID)
		}
	}
	// 直接公開で公開サーバーの更新日時が進んだため、次の提出で食い違いと見なさないよう残す。
	if !remoteUpdatedAt.IsZero() {
		if err := s.articles.SetRemoteUpdatedAt(ctx, a.ID, remoteUpdatedAt); err != nil && !errors.Is(err, article.ErrNotFound) {
			warnf("remote publish article %s: remember remote updated_at: %v", a.ID, err)
		}
	}
	att, err := s.pushRemoteAttachments(ctx, cfg, a, remoteAttachments)
	out.Attachments = att
	switch {
	case errors.Is(err, errRemoteAttachmentsUnsupported):
		warnf("remote publish article %s: the public server is too old to receive attachments", a.ID)
		out.AttachmentsUnsupported = true
	case err != nil:
		warnf("remote publish article %s: attachments: %v", a.ID, err)
		out.AttachmentsErr = err
	}
	return out, nil
}

// publishRemoteArticle はローカルで保存・公開した記事を、編集者以上の招待権限で公開サーバーにも公開する
// （添付資料・フォトギャラリーの画像も送る。sendRemoteArticle）。
func (s *Server) publishRemoteArticle(c echo.Context, a *article.Article) (remotePublishOutcome, error) {
	cfg, err := s.requireRemote(auth.CanPublish, "記事の公開")
	if err != nil {
		return remotePublishOutcome{}, err
	}
	if err := http.NewResponseController(c.Response()).SetWriteDeadline(time.Now().Add(remotePublishDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.Logger().Warnf("remote publish: extend write deadline: %v", err)
	}
	// 応答の期限を延ばしたため、公開サーバーの受付上限に当たっても期間が明けるまで待って送り直す。
	return s.sendRemoteArticle(withRemotePatience(c.Request().Context(), nil), cfg, a, remoteSendOptions{}, c.Logger().Warnf)
}

// remotePublishFlash は記事を公開サーバーへ公開し、結果を知らせる通知のキーを返す（remote_published・
// remote_publish_failed、添付資料だけを送れなかったときは remote_published_attachments_old・_failed）。
func (s *Server) remotePublishFlash(c echo.Context, a *article.Article) string {
	out, err := s.publishRemoteArticle(c, a)
	switch {
	case err != nil:
		c.Logger().Warnf("remote publish article %s: %v", a.ID, err)
		return "remote_publish_failed"
	case out.AttachmentsUnsupported:
		return "remote_published_attachments_old"
	case out.AttachmentsErr != nil:
		return "remote_published_attachments_failed"
	}
	return "remote_published"
}

// collectRemoteImageBatches は記事が参照する画像（サイズ違いを含む）を、1回の要求に収まる組（maxSyncImages ファイル・
// 合計 maxRemoteImageBytes まで）に分けて返す。1つの画像のファイル（4サイズ）は同じ組に入れる。1つの画像だけで上限を
// 超えるときは 413 を返す。ローカルに無い画像は送らない（collectRemoteImages と同じ）。
func (s *Server) collectRemoteImageBatches(a *article.Article) ([][]SyncImage, error) {
	var batches [][]SyncImage
	var cur []SyncImage
	size := 0
	for _, hash := range referencedImageHashes(a.ContentMarkdown + "\n" + a.FeaturedImage) {
		var group []SyncImage
		groupSize := 0
		for _, variant := range remoteImageVariants {
			name := hash + "-" + variant + ".webp"
			data, err := os.ReadFile(filepath.Join(s.cfg.UploadDir, name))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("server: remote publish: read image: %w", err)
			}
			group = append(group, SyncImage{Name: name, Data: data})
			groupSize += len(data)
		}
		if len(group) == 0 {
			continue
		}
		if groupSize > maxRemoteImageBytes {
			return nil, echo.NewHTTPError(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("画像が大きすぎるため公開サーバーへ送れません（1枚の画像は合計%dMBまで）", maxRemoteImageBytes>>20))
		}
		if len(cur)+len(group) > maxSyncImages || size+groupSize > maxRemoteImageBytes {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
		cur = append(cur, group...)
		size += groupSize
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches, nil
}

// unpublishRemoteArticle はローカルで非公開にした記事を公開サーバーでも下書きに戻す。
// 公開サーバーに記事が無い場合は、すでに非公開と同等として成功扱いにする。
func (s *Server) unpublishRemoteArticle(c echo.Context, a *article.Article) error {
	return s.unpublishRemoteArticleCtx(c.Request().Context(), a)
}

// unpublishRemoteArticleCtx は unpublishRemoteArticle の本体。要求の無い呼び出し（MCP の cms_unpublish_article）から使う。
func (s *Server) unpublishRemoteArticleCtx(ctx context.Context, a *article.Article) error {
	cfg, err := s.requireRemote(auth.CanPublish, "記事の非公開化")
	if err != nil {
		return err
	}
	var out SyncPublishResponse
	err = callRemote(ctx, cfg, SyncRequest{Action: SyncActionUnpublishArticle, ArticleID: a.ID}, &out)
	var re *remoteError
	if errors.As(err, &re) && re.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// --- 設定画面 ---

// remoteSettingsData は settings_remote.html に渡すデータ。トークン自体は渡さず、先頭と末尾だけを見せる。
type remoteSettingsData struct {
	// Connected は公開サーバーの接続先とトークンが保存されているか。
	Connected    bool
	URL          string
	HasToken     bool
	TokenPreview string
	Token        *remoteTokenInfo
	TokenError   string
	Permissions  remotePermissions
	// CanPushSettings はサイト基本設定を公開サーバーへ反映できるか（ローカルの管理者で、トークンが admin）。
	CanPushSettings bool
	// PushConfirm は「公開サーバーへ反映する」の確認文（remotePushConfirm。外観テーマ・サイト構成の画面でも使う）。
	PushConfirm string
	// CanReview は校閲待ちの記事を扱えるか（ローカルの編集者）。
	CanReview bool

	// ServerHost は接続先の公開サーバーのホスト名。Role・ExpiryText は保存済みのトークンの立場と有効期限を言葉にしたもの。
	ServerHost string
	Role       remoteRole
	ExpiryText string
	// CanInvite は招待の発行・取り消しができるか（ローカルの管理者で、トークンが admin・期限内）。
	CanInvite bool
	// CanManageSiteAccess は公開サーバーの閲覧制限を確認・変更できるか（ローカルの管理者で、トークンが admin・期限内）。
	CanManageSiteAccess bool
	// CanManageMaintenance は公開サーバーのメンテナンス表示を確認・切り替えできるか（閲覧制限と同じ条件）。
	CanManageMaintenance bool
	// MaintenancePage はこの PC のこのサイトで選んでいるメンテナンス画面（公開サーバーの状態とは別）。
	MaintenancePage *maintenancePageView
	// LocalBasicAuth はこの sorus の「サイト設定」に閲覧制限があるか（デスクトップモードでないか。basicAuthApplies）。
	// 公開サーバーの閲覧制限との関係の案内を切り替える（デスクトップモードには、案内する「サイト設定」の閲覧制限が無い）。
	LocalBasicAuth bool
	// JoinCode は参加に失敗したときに、貼り付けた招待コードを入力欄に残すための値。
	JoinCode string
	// AdvancedOpen は「詳細設定」を開いて表示するか（詳細設定の保存に失敗したとき）。
	AdvancedOpen bool
	// StaticConfigured は静的サイトの公開先（/admin/publish）を設定済みか。StaticLabel はその公開先の表示名。
	// 公開サーバーに未接続のとき、現在の公開方式（静的サイト）として見せる。
	StaticConfigured bool
	StaticLabel      string
	// CanBulkSync は「公開中の記事・添付ファイルを公開サーバーへ一括同期する」を出すか（ローカルの管理者で、招待が直接公開
	// できる・期限内）。BulkActive はいまの公開方式が公開サーバーか（待機中は押せない）。BulkPlan は送るものの数、
	// BulkStatus は一括同期の状態。
	CanBulkSync bool
	BulkActive  bool
	BulkPlan    remoteBulkPlan
	BulkStatus  remoteBulkStatus
}

func (s *Server) remoteSettingsData(c echo.Context, cfg remoteConfig) remoteSettingsData {
	d := remoteSettingsData{Connected: cfg.connected(), URL: cfg.formURL(), HasToken: cfg.Token != "", LocalBasicAuth: s.basicAuthApplies()}
	if pub := s.loadPublishConfig(); pub.configured() {
		d.StaticConfigured, d.StaticLabel = true, pub.label()
	}
	user := currentUser(c)
	d.CanReview = user.HasRole(auth.RoleEditor)
	if !d.HasToken {
		return d
	}
	d.TokenPreview = maskToken(cfg.Token)
	// 招待コードが壊れていても、いまの公開方式（#remote-current-method）に接続先を出せるよう先に決める。
	d.ServerHost = remoteServerHost(cfg.URL)
	info, err := parseRemoteToken(cfg.Token, time.Now())
	if err != nil {
		d.TokenError = err.Error()
		return d
	}
	d.Token = info
	d.Permissions = permissionsFor(info.Role)
	d.CanPushSettings = user.HasRole(auth.RoleAdmin) && d.Permissions.SyncSettings
	if d.CanPushSettings {
		d.PushConfirm = s.remotePushConfirm(c)
	}
	d.Role = roleWordsFor(info.Role, string(language(c)))
	d.ExpiryText = expiryTextFor(info.ExpiresAt, time.Now(), string(language(c)))
	d.CanInvite = cfg.URL != "" && user.HasRole(auth.RoleAdmin) && auth.CanManageInvites(info.Role) && !info.Expired
	d.CanManageSiteAccess = cfg.URL != "" && user.HasRole(auth.RoleAdmin) && auth.CanSyncSettings(info.Role) && !info.Expired
	d.CanManageMaintenance = d.CanManageSiteAccess
	if cfg.URL != "" && user.HasRole(auth.RoleAdmin) && auth.CanPublish(info.Role) && !info.Expired {
		d.CanBulkSync = true
		d.BulkActive = s.remotePublishActive()
		d.BulkPlan = s.remoteBulkPlanFor(c.Request().Context())
		d.BulkStatus = s.remoteBulk.snapshot()
	}
	if d.CanManageMaintenance {
		d.MaintenancePage = s.localMaintenancePageView(c.Request().Context(), language(c))
	}
	return d
}

// maskToken はトークンの先頭6文字と末尾4文字だけを残す。
func maskToken(token string) string {
	if len(token) <= 16 {
		return strings.Repeat("*", 8)
	}
	return token[:6] + "..." + token[len(token)-4:]
}

func (s *Server) handleRemoteSettings(c echo.Context) error {
	return s.render(c, http.StatusOK, "settings_remote", pageTitle(c, "settings_nav_publish"), s.remoteSettingsData(c, s.loadRemoteConfig()))
}

// handleRemoteSettingsSave は URL とトークンを保存する。トークン欄が空なら保存済みのトークンを残し、
// clear_token が指定されれば削除する。入力に誤りがあれば、入力した URL を残して画面を表示し直す。
func (s *Server) handleRemoteSettingsSave(c echo.Context) error {
	cfg := s.loadRemoteConfig()
	rawURL := strings.TrimSpace(c.FormValue("remote_url"))
	rawToken := normalizeRemoteToken(c.FormValue("remote_token"))

	fail := func(msg string) error {
		d := s.remoteSettingsData(c, cfg)
		d.URL = rawURL
		d.AdvancedOpen = true
		return s.renderError(c, http.StatusUnprocessableEntity, "settings_remote", pageTitle(c, "settings_nav_publish"), msg, d)
	}

	next := remoteConfig{Token: cfg.Token}
	if rawURL != "" {
		base, syncPath, err := normalizeRemoteURL(rawURL)
		if err != nil {
			return fail(err.Error())
		}
		next.URL, next.SyncPath = base, syncPath
	}
	switch {
	case c.FormValue("clear_token") != "":
		next.Token = ""
	case rawToken != "":
		if _, err := parseRemoteToken(rawToken, time.Now()); err != nil {
			return fail(err.Error())
		}
		next.Token = rawToken
	}

	// [H-2] URL を空にして保存した場合は、残存トークンも確実に破棄する
	if next.URL == "" {
		next.Token = ""
		next.SyncPath = ""
	}

	// [H-2] 既存設定のトークンが残っている状態で URL が設定・変更される場合（next.URL != cfg.URL）、
	// 新しいトークン（rawToken != ""）の再入力を必須とし、古いトークンを別ホストへ引き継がない
	if next.URL != "" && cfg.Token != "" && next.URL != cfg.URL && rawToken == "" && c.FormValue("clear_token") == "" {
		return fail("接続先を変更する場合は、新しいトークンを入力してください")
	}

	if err := s.saveRemoteConfig(next); err != nil {
		return err
	}
	return c.Redirect(http.StatusSeeOther, "/admin/settings/remote?flash=settings_saved")
}

// saveRemoteConfig は接続の設定を保存する。接続先（URL）が変わったとき（接続をやめたときを含む）は、前の接続先に
// 対して記録していた未反映の設定（remote_dirty_settings）も消す（新しい接続先の設定とは比べていないため）。
func (s *Server) saveRemoteConfig(cfg remoteConfig) error {
	s.remoteDirtyMu.Lock()
	defer s.remoteDirtyMu.Unlock()
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 最初の文を書き込みにして、トランザクションの初めから書き込みのロックを取る（読んでから書き込みに上げると、
	// ほかの接続の書き込みと競合して SQLITE_BUSY になる）。保存済みの URL が cfg.URL と違えば（無い場合を含む）消す。
	if _, err := tx.Exec(`DELETE FROM settings WHERE key = ? AND NOT EXISTS (
			SELECT 1 FROM settings WHERE key = ? AND value = ?)`, remoteSettingDirty, remoteSettingURL, cfg.URL); err != nil {
		return err
	}
	for _, kv := range [][2]string{{remoteSettingURL, cfg.URL}, {remoteSettingSyncPath, cfg.SyncPath}, {remoteSettingToken, cfg.Token}} {
		if _, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- 公開サーバーへ未反映の設定 ---
//
// 公開サーバーに接続しているとき、「公開サーバーへ反映する」（handleRemoteSettingsPush）で送る設定を保存したら、
// どの設定が未反映かを settings の remote_dirty_settings（JSON の配列）に記録する。管理画面の共通バナー
// （layout.html。page.RemoteDirty）がこれを見て、反映のボタンを1つだけ出す。反映が成功したら消す。

const (
	// remoteSettingDirty は公開サーバーへ未反映の設定の種類（remoteDirtyGeneral など）を保存する settings のキー。
	remoteSettingDirty = "remote_dirty_settings"

	// remoteDirtyGeneral はサイト基本設定（サイト名・説明・公開サイトの URL・著作権表示・アクセス解析タグ）。
	remoteDirtyGeneral = "general"
	// remoteDirtyTheme は外観のテーマ（独自テーマのファイルを含む）と公開カードの画像表示。
	remoteDirtyTheme = "theme"
	// remoteDirtyStructure はサイト構成（メニュー・トップページ・カード・ギャラリー）。
	remoteDirtyStructure = "structure"
	// remoteDirtyMedia はメディアライブラリの画像のタグ・説明・削除（フォトギャラリーに出る画像）。反映では、トップページの
	// ギャラリーと公開中の記事の :::gallery のタグの画像を送り、外したタグも公開サーバーで外す（handlers_remote_media.go）。
	remoteDirtyMedia = "media"
)

// remoteDirtyKinds は未反映の設定の種類。画面にはこの順で並べる。
var remoteDirtyKinds = []string{remoteDirtyGeneral, remoteDirtyTheme, remoteDirtyStructure, remoteDirtyMedia}

// connected は公開サーバーの接続先とトークンが保存されているか。
func (c remoteConfig) connected() bool { return c.URL != "" && c.Token != "" }

// parseRemoteDirty は保存した値を、知っている種類だけ remoteDirtyKinds の順にして返す。
func parseRemoteDirty(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		// 手で書き換えられた値などは、カンマ区切りとしても読む。
		items = strings.Split(raw, ",")
	}
	var out []string
	for _, kind := range remoteDirtyKinds {
		for _, it := range items {
			if strings.TrimSpace(it) == kind {
				out = append(out, kind)
				break
			}
		}
	}
	return out
}

// remoteDirty は公開サーバーへ未反映の設定の種類を返す。
func (s *Server) remoteDirty() []string {
	return parseRemoteDirty(s.getSetting(remoteSettingDirty, ""))
}

// saveRemoteDirtyLocked は未反映の設定の種類を保存する（空なら消す）。remoteDirtyMu を持って呼ぶ。
func (s *Server) saveRemoteDirtyLocked(items []string) error {
	if len(items) == 0 {
		_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, remoteSettingDirty)
		return err
	}
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return upsertSetting(s.db, remoteSettingDirty, string(b))
}

// markRemoteDirty は kind の設定を保存したことを、公開サーバーへ未反映として記録する。公開サーバーに接続して
// いないときは記録しない（接続したときに古い記録が残らないようにする）。記録に失敗しても保存そのものは成功している
// ため、ログに残すだけにする。
func (s *Server) markRemoteDirty(c echo.Context, kind string) {
	s.recordRemoteDirty(c.Logger(), kind)
}

// recordRemoteDirty は markRemoteDirty の本体。要求の無い呼び出し（MCP のサイト構成の変更など）から使う。
func (s *Server) recordRemoteDirty(logger echo.Logger, kind string) {
	if !s.loadRemoteConfig().connected() {
		return
	}
	s.remoteDirtyMu.Lock()
	defer s.remoteDirtyMu.Unlock()
	s.remoteDirtyRev++
	items := s.remoteDirty()
	if slices.Contains(items, kind) {
		return
	}
	if err := s.saveRemoteDirtyLocked(parseRemoteDirty(strings.Join(append(items, kind), ","))); err != nil {
		logger.Warnf("remote: record unreflected settings %q: %v", kind, err)
	}
}

// remoteDirtyRevision は未反映の記録の版を返す。反映を始める前に読み、clearRemoteDirty に渡す。
func (s *Server) remoteDirtyRevision() uint64 {
	s.remoteDirtyMu.Lock()
	defer s.remoteDirtyMu.Unlock()
	return s.remoteDirtyRev
}

// clearRemoteDirty は反映できた設定 kinds を未反映の記録から消す。反映を始めた後（rev の後）に別の保存が
// 記録されていれば、その変更は送っていないかもしれないため、何も消さない（バナーは残り、もう一度反映できる）。
func (s *Server) clearRemoteDirty(c echo.Context, rev uint64, kinds ...string) {
	s.remoteDirtyMu.Lock()
	defer s.remoteDirtyMu.Unlock()
	if s.remoteDirtyRev != rev {
		return
	}
	var rest []string
	for _, it := range s.remoteDirty() {
		if !slices.Contains(kinds, it) {
			rest = append(rest, it)
		}
	}
	if err := s.saveRemoteDirtyLocked(rest); err != nil {
		c.Logger().Warnf("remote: clear unreflected settings: %v", err)
	}
}

// layoutRemoteState は管理画面の共通バナーに出す、公開サーバーへ未反映の設定（p.RemoteDirty）と、反映できるか
// （p.RemoteCanPushSettings。設定画面の CanPushSettings と同じ条件）、反映の確認文（p.RemotePushConfirm）を入れる。
// メディアライブラリの画面では、画面を読み込み直さずにタグを変えても出せるよう、未反映が無くてもバナーを隠して置く
// （p.RemoteBannerStandby。static/js/media.js が出す）。公開サーバーに接続していなければ何も入れない。
func (s *Server) layoutRemoteState(c echo.Context, p *page) {
	cfg := s.loadRemoteConfig()
	if p.User == nil || !cfg.connected() {
		return
	}
	p.RemoteDirty = s.remoteDirty()
	if !p.User.HasRole(auth.RoleAdmin) {
		return
	}
	if info, err := parseRemoteToken(cfg.Token, time.Now()); err == nil && auth.CanSyncSettings(info.Role) {
		p.RemoteCanPushSettings = true
		p.RemoteBannerStandby = p.AtPath("/admin/media")
		if len(p.RemoteDirty) > 0 || p.RemoteBannerStandby {
			p.RemotePushConfirm = s.remotePushConfirm(c)
		}
	}
}

// --- 接続テスト ---

// remoteTestResult は接続テストの結果。トークンの内容は署名を検証していない記載どおりの値。
type remoteTestResult struct {
	OK           bool              `json:"ok"`
	Message      string            `json:"message"`
	RemoteStatus int               `json:"remote_status"`
	Subject      string            `json:"subject,omitempty"`
	Role         auth.Role         `json:"role,omitempty"`
	ExpiresAt    string            `json:"expires_at,omitempty"`
	Expired      bool              `json:"expired"`
	Permissions  remotePermissions `json:"permissions"`
	LatencyMS    int64             `json:"latency_ms"`
}

// handleRemoteTest は公開サーバーに接続し、トークンが受け入れられるかを確かめる。フォームに URL・トークンが
// 入力されていればそれを、無ければ保存済みの値を使う（保存前に試せるようにする）。結果は常に 200 で返す。
func (s *Server) handleRemoteTest(c echo.Context) error {
	cfg := s.loadRemoteConfig()
	urlChanged := false
	if raw := strings.TrimSpace(c.FormValue("remote_url")); raw != "" {
		base, syncPath, err := normalizeRemoteURL(raw)
		if err != nil {
			return c.JSON(http.StatusOK, remoteTestResult{Message: err.Error()})
		}
		if base != cfg.URL {
			urlChanged = true
		}
		cfg.URL, cfg.SyncPath = base, syncPath
	}
	rawToken := normalizeRemoteToken(c.FormValue("remote_token"))
	if rawToken != "" {
		cfg.Token = rawToken
	} else if urlChanged && cfg.Token != "" {
		return c.JSON(http.StatusOK, remoteTestResult{Message: "接続先を変更する場合は、新しいトークンを入力してください"})
	}
	if cfg.URL == "" || cfg.Token == "" {
		return c.JSON(http.StatusOK, remoteTestResult{Message: "公開サーバーの URL とトークンを入力してください"})
	}
	info, err := parseRemoteToken(cfg.Token, time.Now())
	if err != nil {
		return c.JSON(http.StatusOK, remoteTestResult{Message: err.Error()})
	}
	res := remoteTestResult{
		Subject:     info.Subject,
		Role:        info.Role,
		ExpiresAt:   info.ExpiresAt.In(jst).Format("2006/01/02 15:04"),
		Expired:     info.Expired,
		Permissions: permissionsFor(info.Role),
	}
	if info.Expired {
		res.Message = "トークンの有効期限が切れています。新しいトークンを発行してもらってください"
		return c.JSON(http.StatusOK, res)
	}

	start := time.Now()
	status, err := pingRemote(c.Request().Context(), cfg)
	res.LatencyMS = time.Since(start).Milliseconds()
	var re *remoteError
	switch {
	case err == nil:
		res.OK, res.RemoteStatus = true, status
	case errors.As(err, &re):
		res.RemoteStatus, res.Message = re.Status, re.Message
		return c.JSON(http.StatusOK, res)
	default:
		return err
	}
	res.Message = fmt.Sprintf("接続できました（%s として %s の権限）", info.Subject, info.Role)
	return c.JSON(http.StatusOK, res)
}

// --- 記事の提出 ---

// handleRemoteSubmit は保存済みの記事と、本文・アイキャッチが参照する画像を公開サーバーへ提出する。
// 公開サーバーでは校閲待ちとして保存され、編集者が承認するまで公開ページには出ない。
// 執筆チーム内では非公開の記事を共有するため、誰の記事でも提出できる。
func (s *Server) handleRemoteSubmit(c echo.Context) error {
	cfg, err := s.requireRemote(auth.CanSubmitDraft, "記事の提出")
	if err != nil {
		return err
	}
	a, err := s.articles.GetByID(c.Param("id"))
	if errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "記事が見つかりません")
	}
	if err != nil {
		return err
	}
	if a.Status == article.StatusDeleted {
		return echo.NewHTTPError(http.StatusConflict, "ゴミ箱の記事は提出できません")
	}
	ctx := c.Request().Context()
	var category string
	if err := s.db.GetContext(ctx, &category, `SELECT slug FROM categories WHERE id = ?`, a.CategoryID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("server: remote submit: category: %w", err)
	}
	images, err := s.collectRemoteImages(a)
	if err != nil {
		return err
	}

	sourceID := ""
	if a.SourceArticleID.Valid {
		sourceID = a.SourceArticleID.String
	}
	req := SyncRequest{
		Action: SyncActionSubmitDraft,
		Article: &SyncArticle{
			ID:                 a.ID,
			Title:              a.Title,
			Slug:               a.Slug,
			ContentMarkdown:    a.ContentMarkdown,
			Excerpt:            a.Excerpt,
			FeaturedImageURL:   remoteFeaturedImageURL(a),
			Category:           category,
			Kind:               a.Kind,
			Lang:               a.Lang,
			TranslationGroupID: a.TranslationGroupID,
			SourceArticleID:    sourceID,
		},
		Images: images,
	}
	var out SyncSubmitResponse
	if err := s.remoteArticleCall(c, cfg, a, req, &out); err != nil {
		return remoteSyncFailure(c, err, a)
	}
	slugAdopted := s.adoptRemoteRef(c, out.Article)
	msg := "公開サーバーへ提出しました。編集者の承認を待っています"
	if !out.Created {
		msg = "公開サーバーの記事を更新しました。編集者の承認を待っています"
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message":      msg,
		"created":      out.Created,
		"article":      out.Article,
		"images":       len(out.Images),
		"slug_adopted": slugAdopted,
	})
}

// remoteArticleCall は記事を変える同期の要求 req（提出・公開・予約）を公開サーバーへ送り、応答を out に読む。
// req には、この PC の記事 local（nil 可）と公開サーバーの食い違いを確かめる base_updated_at（remoteBaseUpdatedAt）、
// 利用者が選んだ上書き（force）、この PC の ID を足す。送れなければそのエラー（base_updated_at の形式の誤りは 400 の
// echo.HTTPError、ほかは公開サーバーとの通信の失敗）を返し、応答の作り方は呼び出し側が決める（ふつうは remoteSyncFailure）。
func (s *Server) remoteArticleCall(c echo.Context, cfg remoteConfig, local *article.Article, req SyncRequest, out any) error {
	base, err := remoteBaseUpdatedAt(c, local)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	req.BaseUpdatedAt, req.Force, req.ClientID = base, remoteFlag(c, "force"), s.remoteClientID(ctx)
	return callRemote(ctx, cfg, req, out)
}

// referencedImageHashes は text が参照するアップロード画像のハッシュを、出現順・重複なしで返す。
func referencedImageHashes(text string) []string {
	var hashes []string
	for _, m := range remoteImageRefPattern.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(hashes, m[1]) {
			hashes = append(hashes, m[1])
		}
	}
	return hashes
}

// collectRemoteImages は記事が参照する画像を、サイズ違いのファイルも含めて UploadDir から読む。
// ローカルに無い画像（公開サーバーから取り込んだ記事の画像など）は公開サーバーに既にあるものとして送らない。
func (s *Server) collectRemoteImages(a *article.Article) ([]SyncImage, error) {
	var images []SyncImage
	total := 0
	tooMany := echo.NewHTTPError(http.StatusRequestEntityTooLarge,
		fmt.Sprintf("画像が多すぎるため一度に提出できません（最大%dファイル・合計%dMBまで）", maxSyncImages, maxRemoteImageBytes>>20))
	for _, hash := range referencedImageHashes(a.ContentMarkdown + "\n" + a.FeaturedImage) {
		for _, variant := range remoteImageVariants {
			name := hash + "-" + variant + ".webp"
			data, err := os.ReadFile(filepath.Join(s.cfg.UploadDir, name))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("server: remote submit: read image: %w", err)
			}
			total += len(data)
			if len(images) >= maxSyncImages || total > maxRemoteImageBytes {
				return nil, tooMany
			}
			images = append(images, SyncImage{Name: name, Data: data})
		}
	}
	return images, nil
}

// --- 校閲待ちの記事 ---

// remotePendingItem は校閲待ちの記事1件の表示用データ。HTML はローカルで Markdown から生成したサニタイズ済みの
// プレビューで、画像は公開サーバーの URL を参照させる。
type remotePendingItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Slug  string `json:"slug"`
	// Kind は記事の種類。種類を返さない古い公開サーバーでは post。
	Kind         article.Kind `json:"kind"`
	Excerpt      string       `json:"excerpt"`
	CategoryName string       `json:"category_name"`
	AuthorName   string       `json:"author_name"`
	UpdatedAt    string       `json:"updated_at"`
	HTML         string       `json:"html"`
	// LocalStatus はローカル CMS に同じ ID の記事があればその状態（取り込み済みか）。無ければ空。
	LocalStatus article.Status `json:"local_status,omitempty"`
}

// fetchRemoteQueue は公開サーバーの fetch_pending を呼び、校閲待ちと予約中の記事を返す。予約を知らない古い公開サーバーでは
// Scheduled が nil になる。
func fetchRemoteQueue(ctx context.Context, cfg remoteConfig) (*SyncPendingResponse, error) {
	var out SyncPendingResponse
	if err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionFetchPending}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// handleRemotePending は公開サーバーの校閲待ちの記事を一覧で返す。あわせて、公開サーバーで予約中の記事（scheduled）と、
// 「日時を決めて予約」の選択肢を出すか（schedule、remoteScheduleViewFor）を返す。
func (s *Server) handleRemotePending(c echo.Context) error {
	cfg, err := s.requireRemote(auth.CanFetchPending, "校閲待ちの記事の取得")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	queue, err := fetchRemoteQueue(ctx, cfg)
	if err != nil {
		return remoteFailure(c, err)
	}
	pending := queue.Articles
	local, err := s.localStatuses(ctx, pending)
	if err != nil {
		return err
	}
	items := make([]remotePendingItem, 0, len(pending))
	for _, p := range pending {
		rendered, err := markdown.Render(p.ContentMarkdown)
		if err != nil {
			return fmt.Errorf("server: remote pending: render %s: %w", p.ID, err)
		}
		kind := p.Kind
		if kind == "" {
			kind = article.KindPost
		}
		items = append(items, remotePendingItem{
			ID:           p.ID,
			Title:        p.Title,
			Slug:         p.Slug,
			Kind:         kind,
			Excerpt:      p.Excerpt,
			CategoryName: p.CategoryName,
			AuthorName:   p.AuthorName,
			UpdatedAt:    p.UpdatedAt.In(jst).Format("2006/01/02 15:04"),
			HTML:         remotePreviewHTML(rendered.HTML, cfg),
			LocalStatus:  local[p.ID],
		})
	}
	lang := language(c)
	return c.JSON(http.StatusOK, map[string]any{
		"count":      len(items),
		"articles":   items,
		"remote_url": cfg.URL,
		"scheduled":  remoteScheduledItems(lang, queue.Scheduled),
		"schedule":   remoteScheduleViewFor(ctx, lang, cfg),
	})
}

// localStatuses は pending と同じ ID のローカルの記事の状態を返す。
func (s *Server) localStatuses(ctx context.Context, pending []SyncPendingArticle) (map[string]article.Status, error) {
	statuses := make(map[string]article.Status, len(pending))
	if len(pending) == 0 {
		return statuses, nil
	}
	ids := make([]string, len(pending))
	for i, p := range pending {
		ids[i] = p.ID
	}
	q, args, err := sqlx.In(`SELECT id, status FROM articles WHERE id IN (?)`, ids)
	if err != nil {
		return nil, fmt.Errorf("server: remote pending: %w", err)
	}
	var rows []struct {
		ID     string         `db:"id"`
		Status article.Status `db:"status"`
	}
	if err := s.db.SelectContext(ctx, &rows, s.db.Rebind(q), args...); err != nil {
		return nil, fmt.Errorf("server: remote pending: %w", err)
	}
	for _, r := range rows {
		statuses[r.ID] = r.Status
	}
	return statuses, nil
}

// remotePendingID は URL の記事 ID を検査する。公開サーバーと同じ形式だけを受け付ける。
func remotePendingID(c echo.Context) (string, error) {
	id := c.Param("id")
	if !syncArticleIDPattern.MatchString(id) {
		return "", echo.NewHTTPError(http.StatusBadRequest, "記事 ID の形式が正しくありません")
	}
	return id, nil
}

// findRemotePending は公開サーバーの校閲待ちと作成中の下書きから記事 id を探す。無ければ 404 を返す。
// 下書きの記事は Status が draft（下書きを返さない古い公開サーバーでは校閲待ちだけを探す）。
func (s *Server) findRemotePending(c echo.Context, cfg remoteConfig, id string) (*SyncPendingArticle, error) {
	queue, err := fetchRemoteQueue(c.Request().Context(), cfg)
	if err != nil {
		return nil, remoteFailure(c, err)
	}
	for _, list := range [][]SyncPendingArticle{queue.Articles, queue.Drafts} {
		if i := slices.IndexFunc(list, func(p SyncPendingArticle) bool { return p.ID == id }); i >= 0 {
			return &list[i], nil
		}
	}
	return nil, echo.NewHTTPError(http.StatusNotFound, "公開サーバーの校閲待ち・下書きにこの記事がありません。既に公開されたか、取り下げられた可能性があります")
}

// remotePendingIsDraft は公開サーバーの記事 p が作成中の下書きか（校閲待ちでないか）を返す。
func remotePendingIsDraft(p *SyncPendingArticle) bool { return p.Status == article.StatusDraft }

// remoteOverwriteReason は、公開サーバーの記事でローカルの記事を上書きする前に見せる理由。
const remoteOverwriteReason = "上書きすると、この PC の記事は公開サーバーに届いた内容に変わります（変更前の内容は履歴に残ります）。"

// handleRemotePreview は公開サーバーの校閲待ちの記事を取り込まずに、「届いた原稿」で見せる形（manuscriptPreview）で
// 返す。ローカルに同じ記事があれば、その差分と上書きの確認を付ける。
// 本文の画像は公開サーバーのものを PC を通して見せる（remotePreviewHTML）。
func (s *Server) handleRemotePreview(c echo.Context) error {
	id, err := remotePendingID(c)
	if err != nil {
		return err
	}
	cfg, err := s.requireRemote(auth.CanFetchPending, "校閲待ちの記事の取得")
	if err != nil {
		return err
	}
	p, err := s.findRemotePending(c, cfg, id)
	if err != nil {
		return err
	}
	if p.TranslationGroupID != "" && !syncArticleIDPattern.MatchString(p.TranslationGroupID) {
		return echo.NewHTTPError(http.StatusBadRequest, "公開サーバーの記事の translation_group_id の形式が正しくありません")
	}
	if p.SourceArticleID != "" && !syncArticleIDPattern.MatchString(p.SourceArticleID) {
		return echo.NewHTTPError(http.StatusBadRequest, "公開サーバーの記事の source_article_id の形式が正しくありません")
	}
	existing, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		existing = nil
	} else if err != nil {
		return err
	} else if err := checkLocalImportable(existing); err != nil {
		return err
	}
	rendered, err := markdown.Render(p.ContentMarkdown)
	if err != nil {
		return fmt.Errorf("server: remote preview: %w", err)
	}
	pv := manuscriptPreview{
		Title:      p.Title,
		Kind:       p.Kind,
		Category:   p.CategoryName,
		AuthorName: p.AuthorName,
		AuthorNote: "あなたが受け取った記事として、この PC に入れます。",
		CreatedAt:  p.UpdatedAt.In(jst).Format("2006/01/02 15:04"),
		Images:     len(referencedImageHashes(p.ContentMarkdown)),
		HTML:       remotePreviewHTML(rendered.HTML, cfg),
	}
	if pv.Kind == "" {
		pv.Kind = article.KindPost
	}
	if existing != nil {
		pv.AuthorNote = "今ある記事の書き手のまま受け取ります。"
		pv.Existing = existingView(existing, p.Title, p.ContentMarkdown, true, remoteOverwriteReason)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, pv)
}

// handleRemoteImport は公開サーバーの校閲待ちの記事・作成中の下書きを、同じ ID・同じ状態（校閲待ちは校閲待ち、
// 下書きは下書き）の記事としてローカルに取り込み、参照している画像をダウンロードする。ローカルで推敲してから提出し直し、
// 承認して公開できるようにする。取り込む前に公開サーバーの記事にこの PC で編集中の印（checkout）を付け、ほかの人の印が
// あれば何も変えずに 409（code: checked_out、need_force）を返す。force=1（「引き継ぐ」）ならその印を引き継ぐ。
// 取り込んだ版の公開サーバーの更新日時は、次の提出の base_updated_at として記事に残す。
//
// ローカルに同じ記事があれば、下書き・校閲待ちで他の人が編集中でなく、プレビューで差分を見て overwrite=1
// （「この内容で上書きする」）を送った場合に限り上書きする（変更前はリビジョンに残る）。overwrite=1 が無ければ
// 何も変えずに 409 を返す。画像のダウンロードには時間がかかるため、その後に編集ロックを獲得してから記事の状態を
// 確かめ直して保存する。ローカルに無かった記事は作成だけを行い、ダウンロードの間に同じ ID の記事が作成されていれば
// 上書きせずに 409 を返す。
func (s *Server) handleRemoteImport(c echo.Context) error {
	return s.importRemote(c, remoteFlag(c, "overwrite"))
}

// handleRemotePullLatest は、提出したときの食い違いの画面で「サーバー版を取り込む」を選んだときに、公開サーバーの今の記事で
// この PC の記事を上書きする（差分は見せた後のため、overwrite=1 の取り込みと同じ）。書き手（writer のトークン）も使えるよう、
// 記事1件の取得（pull_article）で読む。pull_article を知らない古い公開サーバーでは、編集者以上のトークンなら校閲待ち・下書きの
// 一覧（fetch_pending）から探す。公開サーバーの「編集中」の印は編集者以上の操作のため、書き手のトークンでは付けない。
func (s *Server) handleRemotePullLatest(c echo.Context) error {
	id, err := remotePendingID(c)
	if err != nil {
		return err
	}
	cfg, err := s.requireRemote(auth.CanSubmitDraft, "公開サーバーの記事の取り込み")
	if err != nil {
		return err
	}
	canFetch := false
	if info, err := parseRemoteToken(cfg.Token, time.Now()); err == nil {
		canFetch = auth.CanFetchPending(info.Role)
	}
	var out SyncPullArticleResponse
	err = callRemote(c.Request().Context(), cfg, SyncRequest{Action: SyncActionPullArticle, ArticleID: id}, &out)
	var re *remoteError
	switch {
	case err == nil:
		if out.Article.ID != id {
			return echo.NewHTTPError(http.StatusBadGateway, "公開サーバーから別の記事が返りました")
		}
		return s.importRemoteArticle(c, cfg, &out.Article, true, canFetch)
	case errors.As(err, &re) && re.Status == http.StatusBadRequest && canFetch:
		// pull_article を知らない古い公開サーバー。
		p, err := s.findRemotePending(c, cfg, id)
		if err != nil {
			return err
		}
		return s.importRemoteArticle(c, cfg, p, true, true)
	case errors.As(err, &re) && re.Status == http.StatusBadRequest:
		return echo.NewHTTPError(http.StatusConflict, "公開サーバーの sorus が古いため、書き手の招待では記事を取り込めません。公開サーバーを更新するよう管理者に依頼してください")
	case errors.As(err, &re) && (re.Status == http.StatusNotFound || re.Status == http.StatusConflict):
		return echo.NewHTTPError(re.Status, re.Message)
	}
	return remoteFailure(c, err)
}

func (s *Server) importRemote(c echo.Context, overwrite bool) error {
	id, err := remotePendingID(c)
	if err != nil {
		return err
	}
	cfg, err := s.requireRemote(auth.CanFetchPending, "校閲待ちの記事の取得")
	if err != nil {
		return err
	}
	p, err := s.findRemotePending(c, cfg, id)
	if err != nil {
		return err
	}
	return s.importRemoteArticle(c, cfg, p, overwrite, true)
}

// verifyRemoteImport は公開サーバーの記事 p をこの PC に取り込めるかを、画像をダウンロードする前に確かめる
// （importRemoteArticle の確認）。取り込めればこの PC の同じ記事 a を返し、無ければ created（a は nil）を返す。
// p の ID などの形式が正しくない、p やこの PC の記事が下書き・校閲待ちでない、ほかの人がローカルで編集中なら断る。
func (s *Server) verifyRemoteImport(c echo.Context, p *SyncPendingArticle) (a *article.Article, created bool, err error) {
	if !syncArticleIDPattern.MatchString(p.ID) {
		return nil, false, echo.NewHTTPError(http.StatusBadGateway, "公開サーバーの記事 ID の形式が正しくありません")
	}
	if p.Status != "" && p.Status != article.StatusDraft && p.Status != article.StatusReview {
		return nil, false, echo.NewHTTPError(http.StatusConflict, "公開サーバーの記事が"+statusLabel(p.Status)+"のため取り込めません")
	}
	if p.TranslationGroupID != "" && !syncArticleIDPattern.MatchString(p.TranslationGroupID) {
		return nil, false, echo.NewHTTPError(http.StatusBadRequest, "公開サーバーの記事の translation_group_id の形式が正しくありません")
	}
	if p.SourceArticleID != "" && !syncArticleIDPattern.MatchString(p.SourceArticleID) {
		return nil, false, echo.NewHTTPError(http.StatusBadRequest, "公開サーバーの記事の source_article_id の形式が正しくありません")
	}
	a, err = s.articles.GetByID(p.ID)
	if errors.Is(err, article.ErrNotFound) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := checkLocalImportable(a); err != nil {
		return nil, false, err
	}
	info, err := s.locker.Check(c.Request().Context(), p.ID, currentUser(c).ID)
	if err != nil {
		return nil, false, err
	}
	if info.IsLocked && !info.IsOwnLock {
		return nil, false, echo.NewHTTPError(http.StatusConflict, info.UserName+"さんがローカルで編集中のため取り込めません")
	}
	return a, false, nil
}

// saveImportedArticle は公開サーバーの記事の内容を写した a を save（Create か Save）で保存する（取り込み・変更案の反映で共通）。
// 保存できたら検索の索引を更新し、公開サーバーの更新日時 remoteUpdatedAt を次の base_updated_at として残し、label の版を残す。
// save のエラーはそのまま返す（応答の作り方は呼び出し側が決める）。
func (s *Server) saveImportedArticle(c echo.Context, a *article.Article, save func(*article.Article) error, remoteUpdatedAt time.Time, label string) error {
	if err := save(a); err != nil {
		return err
	}
	s.reindexArticle(c, a.ID)
	if err := s.articles.SetRemoteUpdatedAt(c.Request().Context(), a.ID, remoteUpdatedAt); err != nil {
		c.Logger().Warnf("remote sync: remember remote updated_at of article %s: %v", a.ID, err)
	}
	s.syncSnapshot(c, a, currentUser(c).ID, label)
	return nil
}

// importRemoteArticle は公開サーバーの記事 p をこの PC に取り込む（handleRemoteImport の本体）。checkout なら取り込む前に
// 公開サーバーに「この PC で編集中」の印を付ける（編集者以上のトークンだけができる）。
func (s *Server) importRemoteArticle(c echo.Context, cfg remoteConfig, p *SyncPendingArticle, overwrite, checkout bool) error {
	// 取り込めない記事なら、画像をダウンロードする前に断る。
	a, created, err := s.verifyRemoteImport(c, p)
	if err != nil {
		return err
	}
	if !created && !overwrite {
		return c.JSON(http.StatusConflict, map[string]any{
			"message":      "この PC に同じ記事があります。" + remoteOverwriteReason + "内容を確かめてから「この内容で上書きする」を押してください。",
			"need_confirm": true,
		})
	}
	id := p.ID
	ctx := c.Request().Context()
	user := currentUser(c)

	// 公開サーバーに「この PC で編集中」の印を付ける。ほかの人が編集中なら、引き継ぐ（force=1）まで取り込まない。
	imported := false
	if checkout {
		if err := s.checkoutRemote(ctx, cfg, id, remoteFlag(c, "force")); err != nil {
			return remoteSyncFailure(c, err, a)
		}
		defer func() {
			if !imported {
				s.releaseRemoteCheckout(c, cfg, id)
			}
		}()
	}

	saved, failed := s.downloadRemoteImages(ctx, cfg, p.ContentMarkdown)

	save := s.articles.Save
	if created {
		a = &article.Article{ID: id, AuthorID: user.ID}
		if p.Lang != "" {
			a.Lang = p.Lang
		} else {
			a.Lang = "ja"
		}
		if p.TranslationGroupID != "" {
			a.TranslationGroupID = p.TranslationGroupID
		}
		if p.SourceArticleID != "" {
			a.SourceArticleID = sql.NullString{String: p.SourceArticleID, Valid: true}
		}
		save = s.articles.Create
	} else {
		// ダウンロードの間に他の人が編集を始めていないか、ロックを獲得してから読み直して確かめる。
		release, err := s.lockForSync(ctx, id, user.ID)
		if err != nil {
			return err
		}
		defer release()
		if a, err = s.articles.GetByID(id); err != nil {
			return err
		}
		if err := checkLocalImportable(a); err != nil {
			return err
		}
		s.syncSnapshot(c, a, user.ID, "公開サーバーから取り込む前の版")
	}

	categoryID, err := s.syncCategoryID(ctx, p.Category)
	if err != nil {
		return err
	}
	if categoryID == 0 && a.CategoryID == 0 {
		if categoryID, err = s.syncDefaultCategoryID(ctx); err != nil {
			return err
		}
	}
	if categoryID != 0 {
		a.CategoryID = categoryID
	}
	a.Title, a.Slug, a.ContentMarkdown, a.Excerpt = p.Title, p.Slug, p.ContentMarkdown, p.Excerpt
	// 種類を返さない古い公開サーバーからは、新しい記事は post（Save の既定）、既存の記事は現在の種類のまま取り込む。
	if p.Kind != "" {
		a.Kind = p.Kind
	}
	// 多言語メタデータの反映（公開サーバー側の設定を尊重）
	if p.Lang != "" {
		a.Lang = p.Lang
	}
	if p.TranslationGroupID != "" {
		a.TranslationGroupID = p.TranslationGroupID
	}
	if p.SourceArticleID != "" {
		a.SourceArticleID = sql.NullString{String: p.SourceArticleID, Valid: true}
	}
	// 公開サーバーで作成中の下書きは、この PC でも下書きのまま続きを書く。
	a.Status = article.StatusReview
	if remotePendingIsDraft(p) {
		a.Status = article.StatusDraft
	}
	if a.Kind == article.KindPage && s.articles.IsReservedPageSlug(article.NormalizeSlug(a.Slug)) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, i18n.Translate(string(i18n.JA), "reserved_page_slug", a.Slug))
	}

	// 取り込んだ版の公開サーバーの更新日時を残し、次の提出で、その後に公開サーバーで変わっていないかを確かめる。
	if err := s.saveImportedArticle(c, a, save, p.UpdatedAt, "公開サーバーから取り込み"); err != nil {
		if errors.Is(err, article.ErrExists) {
			return echo.NewHTTPError(http.StatusConflict, "取り込みの間にローカルで同じ記事が作成されました。内容を確認してから、もう一度取り込んでください")
		}
		return fmt.Errorf("server: remote import: %w", err)
	}
	imported = true

	msg := "公開サーバーの記事をローカルに取り込みました"
	if a.Status == article.StatusDraft {
		msg = "公開サーバーの下書きをこの PC に取り込みました。続きを書けます"
	}
	if failed > 0 {
		msg += fmt.Sprintf("（画像%d件をダウンロードできませんでした）", failed)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message":           msg,
		"created":           created,
		"id":                a.ID,
		"status":            a.Status,
		"edit_url":          "/admin/articles/" + url.PathEscape(a.ID) + "/edit",
		"images_saved":      saved,
		"images_failed":     failed,
		"remote_author":     p.AuthorName,
		"remote_updated_at": p.UpdatedAt,
	})
}

// checkLocalImportable は公開サーバーの記事でローカルの記事 a を上書きできるか（下書き・校閲待ちか）を確かめる。
func checkLocalImportable(a *article.Article) error {
	if a.Status != article.StatusDraft && a.Status != article.StatusReview {
		return echo.NewHTTPError(http.StatusConflict, "ローカルの同じ記事が"+statusLabel(a.Status)+"のため取り込めません")
	}
	return nil
}

// downloadRemoteImages は text が参照する画像を、サイズ違いも含めて公開サーバーの /uploads/ からダウンロードし、
// UploadDir に同じ名前で保存する。既にある画像は取得しない。本文が直接参照しているファイルを取得・保存できなかった
// 数を failed に数える（サイズ違いのファイルが無いことはエラーにしない）。
func (s *Server) downloadRemoteImages(ctx context.Context, cfg remoteConfig, text string) (saved, failed int) {
	referenced := remoteImageRefPattern.FindAllString(text, -1)
	if err := os.MkdirAll(s.cfg.UploadDir, 0o755); err != nil {
		return 0, len(referenced)
	}
	attempts := 0
	for _, hash := range referencedImageHashes(text) {
		for _, variant := range remoteImageVariants {
			name := hash + "-" + variant + ".webp"
			dst := filepath.Join(s.cfg.UploadDir, name)
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			isReferenced := slices.Contains(referenced, UploadURLPrefix+name)
			if attempts >= maxSyncImages {
				if isReferenced {
					failed++
				}
				continue
			}
			attempts++
			err := s.downloadRemoteImage(ctx, cfg, name, dst)
			switch {
			case err == nil:
				saved++
			case errors.Is(err, fs.ErrExist):
			case isReferenced:
				failed++
			}
		}
	}
	return saved, failed
}

func (s *Server) downloadRemoteImage(ctx context.Context, cfg remoteConfig, name, dst string) error {
	data, err := s.fetchRemoteImage(ctx, cfg, name)
	if err != nil {
		return err
	}
	return writeFileExclusive(dst, data)
}

// handleRemotePublish は公開サーバーの校閲待ちの記事を承認して公開する。この PC に同じ記事があれば、取り込んだときの
// 公開サーバーの更新日時を base_updated_at として送り、その後に公開サーバーで変わっていれば差分付きの 409 を返す。
func (s *Server) handleRemotePublish(c echo.Context) error {
	id, err := remotePendingID(c)
	if err != nil {
		return err
	}
	cfg, err := s.requireRemote(auth.CanPublish, "記事の公開")
	if err != nil {
		return err
	}
	local, err := s.localArticle(id)
	if err != nil {
		return err
	}
	var out SyncPublishResponse
	req := SyncRequest{Action: SyncActionPublishArticle, ArticleID: id}
	if err := s.remoteArticleCall(c, cfg, local, req, &out); err != nil {
		return remoteSyncFailure(c, err, local)
	}
	s.adoptRemoteRef(c, out.Article)
	// 公開サーバーに BaseURL が無いと URL はサイトルートからのパスになるため、設定中の URL を補う。
	publicURL := out.Article.URL
	if strings.HasPrefix(publicURL, "/") {
		publicURL = cfg.URL + publicURL
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message": "承認して公開サーバーで公開しました",
		"article": out.Article,
		"url":     publicURL,
	})
}

// --- 予約公開 ---
//
// 公開サーバーで予約した記事は、公開サーバーに状態「公開」と未来の公開日時として保存され、その時刻から読者に見え始める。
// 公開サーバーは読者が来るたびに時刻で絞るため、この PC の sorus を閉じていても予定どおり公開される。
//
// 予約できるかどうかは、同期 API の server_info で確かめる（remoteCanSchedule）。予約を知らない古い公開サーバーは
// 未対応の action を 400 で断るため、確かめる要求で記事が変わることはなく、予約の要求（schedule_article）も
// 同じく 400 で断られる。どの場合も即時公開（publish_article）に切り替えない。

const (
	// remoteScheduleRecheck は、公開サーバーが予約に対応していないと分かった結果を覚えておく時間。
	// 対応しているという結果は、接続先（URL・トークン）を変えるまでこの起動の間ずっと覚えておく。
	// 対応していない結果は、案内に従って公開サーバーを更新した後に画面を開き直せば予約できるよう、この時間で確かめ直す。
	remoteScheduleRecheck = 5 * time.Minute
	// remoteScheduleProbeTimeout は予約に対応しているかを確かめる要求の制限時間。画面の表示を長く待たせない。
	remoteScheduleProbeTimeout = 10 * time.Second
	// remoteScheduleDefaultMargin は日時の入力欄の初期値（次の正時）を、今からこれより近くにしない間隔。
	remoteScheduleDefaultMargin = 5 * time.Minute
)

// remoteScheduleProbes は接続先ごと（remoteProbeKey）に、公開サーバーが予約に対応しているかを確かめた結果。
// 接続先を変えると鍵が変わるため、別の公開サーバーやトークンの結果を使い回さない。
var remoteScheduleProbes = struct {
	sync.Mutex
	m map[string]remoteScheduleProbe
}{m: map[string]remoteScheduleProbe{}}

type remoteScheduleProbe struct {
	supported bool
	checkedAt time.Time
}

// remoteProbeKey は接続先（同期 API の URL とトークン）を表す鍵。トークンをそのまま鍵にしないようハッシュにする。
func remoteProbeKey(cfg remoteConfig) string {
	sum := sha256.Sum256([]byte(cfg.endpoint() + "\n" + cfg.Token))
	return hex.EncodeToString(sum[:])
}

// rememberRemoteSchedule は接続先 cfg が予約に対応しているかを覚える。
func rememberRemoteSchedule(cfg remoteConfig, supported bool) {
	remoteScheduleProbes.Lock()
	defer remoteScheduleProbes.Unlock()
	remoteScheduleProbes.m[remoteProbeKey(cfg)] = remoteScheduleProbe{supported: supported, checkedAt: time.Now()}
}

// remoteCanSchedule は公開サーバーが予約（schedule_article・unschedule_article）に対応しているかを返す。
// 同期 API に server_info を送り、200 で schedule が true なら対応、400（未対応の action）なら対応していないと判断し、
// 結果を接続先ごとに覚える（remoteScheduleRecheck）。server_info を知らない古い公開サーバーは、トークンを検証してから
// 未対応の action として 400 で断るため、記事は何も変わらない。接続できない・トークンが認められないなど、どちらとも
// 判断できなければ remoteError を返し、結果を覚えない（予約の選択肢は出さない）。
func remoteCanSchedule(ctx context.Context, cfg remoteConfig) (bool, error) {
	key := remoteProbeKey(cfg)
	remoteScheduleProbes.Lock()
	p, ok := remoteScheduleProbes.m[key]
	remoteScheduleProbes.Unlock()
	if ok && (p.supported || time.Since(p.checkedAt) < remoteScheduleRecheck) {
		return p.supported, nil
	}
	ctx, cancel := context.WithTimeout(ctx, remoteScheduleProbeTimeout)
	defer cancel()
	var out SyncServerInfoResponse
	err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionServerInfo}, &out)
	var re *remoteError
	switch {
	case err == nil:
	case errors.As(err, &re) && re.Status == http.StatusBadRequest:
		out.Schedule = false
	default:
		return false, err
	}
	rememberRemoteSchedule(cfg, out.Schedule)
	return out.Schedule, nil
}

// remoteScheduleView は「日時を決めて予約」の選択肢を出すかと、日時の入力欄の値。
type remoteScheduleView struct {
	// Available は予約の選択肢を出すか（トークンに公開の権限があり、公開サーバーが予約に対応している）。
	Available bool `json:"available"`
	// Note は選択肢を出さない理由の案内。公開サーバーが予約に対応していないときだけ入れる。
	Note string `json:"note,omitempty"`
	// Default・Min は日時の入力欄（datetime-local、日本時間）の初期値と最小値。初期値は次の正時。
	Default string `json:"default,omitempty"`
	Min     string `json:"min,omitempty"`
}

// remoteScheduleViewFor は接続先 cfg で予約の選択肢を出すかを決める。トークンに公開の権限が無ければ何も出さない。
// 公開サーバーが予約に対応していなければ、選択肢の代わりに公開サーバーの更新を案内する。確かめられなかったとき
// （接続できないなど）は、選択肢も案内も出さない（一覧の読み込みの失敗として別に伝わる）。
func remoteScheduleViewFor(ctx context.Context, lang i18n.Lang, cfg remoteConfig) remoteScheduleView {
	info, err := parseRemoteToken(cfg.Token, time.Now())
	if err != nil || info.Expired || !auth.CanPublish(info.Role) {
		return remoteScheduleView{}
	}
	supported, err := remoteCanSchedule(ctx, cfg)
	if err != nil {
		return remoteScheduleView{}
	}
	if !supported {
		return remoteScheduleView{Note: i18n.Translate(string(lang), "remote_schedule_unsupported")}
	}
	now := clockNow()
	// 入力欄は分単位のため、分の途中なら次の分に切り上げ、選べる最小の日時が MinScheduleLead より近くならないようにする。
	minAt := now.Add(MinScheduleLead)
	if t := minAt.Truncate(time.Minute); !t.Equal(minAt) {
		minAt = t.Add(time.Minute)
	}
	return remoteScheduleView{
		Available: true,
		Default:   defaultScheduleAt(now).In(jst).Format(datetimeLocalLayout),
		Min:       minAt.In(jst).Format(datetimeLocalLayout),
	}
}

// remoteSchedulePicker は「日時を決めて予約」の入力欄（remote_sync_modal.html の remote_schedule_picker）に渡すデータ。
type remoteSchedulePicker struct {
	Page page
	// ID・Title は予約する公開サーバーの記事。空なら、ブラウザ（remote_sync.js）が入力欄を複製して後から入れる。
	ID    string
	Title string
	// Default・Min は日時の入力欄の初期値と最小値（remoteScheduleView）。空ならブラウザが後から入れる。
	Default string
	Min     string
	// Reload は予約できたら画面を読み込み直すか（サーバーで描いた一覧の画面）。
	Reload bool
}

// SchedulePicker はテンプレートから remote_schedule_picker に渡すデータを作る。
func (p page) SchedulePicker(id, title, def, min string, reload bool) remoteSchedulePicker {
	return remoteSchedulePicker{Page: p, ID: id, Title: title, Default: def, Min: min, Reload: reload}
}

// defaultScheduleAt は日時の入力欄の初期値。now から remoteScheduleDefaultMargin 以上先の、最も近い正時。
// 日本時間は UTC との差が時間単位のため、Truncate で日本時間の正時にそろう。
func defaultScheduleAt(now time.Time) time.Time {
	return now.Add(remoteScheduleDefaultMargin).Truncate(time.Hour).Add(time.Hour).UTC()
}

// scheduleTimeText は予約の日時を日本時間で「11月3日 9:00」（英語では「Nov 3, 9:00」）の形にする。
// now と年が違えば年も付ける。
func scheduleTimeText(lang i18n.Lang, t, now time.Time) string {
	t, now = t.In(jst), now.In(jst)
	if lang == i18n.EN {
		date := fmt.Sprintf("%s %d", t.Month().String()[:3], t.Day())
		if t.Year() != now.Year() {
			date += fmt.Sprintf(", %d", t.Year())
		}
		return fmt.Sprintf("%s, %d:%02d", date, t.Hour(), t.Minute())
	}
	date := fmt.Sprintf("%d月%d日", int(t.Month()), t.Day())
	if t.Year() != now.Year() {
		date = fmt.Sprintf("%d年", t.Year()) + date
	}
	return fmt.Sprintf("%s %d:%02d", date, t.Hour(), t.Minute())
}

// remoteScheduledItem は公開サーバーで予約中の記事1件の表示用データ。
type remoteScheduledItem struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Kind         article.Kind `json:"kind"`
	AuthorName   string       `json:"author_name"`
	CategoryName string       `json:"category_name"`
	// PublishAt は予約した公開日時。Label は「予約: 11月3日 9:00」の表示。
	PublishAt time.Time `json:"publish_at"`
	Label     string    `json:"label"`
	// UnscheduleConfirm は「予約を取り消す」を押したときの確認の文。
	UnscheduleConfirm string `json:"unschedule_confirm"`
}

// remoteScheduledItems は fetch_pending の予約中の記事を表示用にする。予約を知らない古い公開サーバー（scheduled が nil）や
// 公開日時の無い記事は除き、常に配列（空なら空配列）を返す。
func remoteScheduledItems(lang i18n.Lang, scheduled []SyncPendingArticle) []remoteScheduledItem {
	now := clockNow()
	items := make([]remoteScheduledItem, 0, len(scheduled))
	for _, p := range scheduled {
		if p.PublishedAt == nil {
			continue
		}
		kind := p.Kind
		if kind == "" {
			kind = article.KindPost
		}
		items = append(items, remoteScheduledItem{
			ID:                p.ID,
			Title:             p.Title,
			Kind:              kind,
			AuthorName:        p.AuthorName,
			CategoryName:      p.CategoryName,
			PublishAt:         p.PublishedAt.UTC(),
			Label:             i18n.Translate(string(lang), "remote_scheduled_label", scheduleTimeText(lang, *p.PublishedAt, now)),
			UnscheduleConfirm: i18n.Translate(string(lang), "remote_unschedule_confirm", p.Title),
		})
	}
	return items
}

// remoteScheduleUnsupported は公開サーバーが予約に対応していないときの応答。即時公開には切り替えない。
func remoteScheduleUnsupported(c echo.Context) error {
	return c.JSON(http.StatusConflict, map[string]any{
		"message":              i18n.Translate(string(language(c)), "remote_schedule_unsupported"),
		"schedule_unsupported": true,
	})
}

// handleRemoteSchedule は公開サーバーの校閲待ちの記事を承認し、publish_at（datetime-local、日本時間）に公開する予約にする。
// 日時は UTC にして schedule_article で送る。公開サーバーが予約に対応していなければ（server_info・schedule_article が
// 400）、即時公開（publish_article）に切り替えずに 409 と更新の案内を返す。
func (s *Server) handleRemoteSchedule(c echo.Context) error {
	id, err := remotePendingID(c)
	if err != nil {
		return err
	}
	cfg, err := s.requireRemote(auth.CanPublish, "記事の予約")
	if err != nil {
		return err
	}
	lang := string(language(c))
	v := strings.TrimSpace(c.FormValue("publish_at"))
	if v == "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, i18n.Translate(lang, "remote_schedule_time_required"))
	}
	publishAt, err := parseDatetimeLocal(v)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, i18n.Translate(lang, "remote_schedule_time_invalid"))
	}
	if publishAt.Before(clockNow().Add(MinScheduleLead)) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, i18n.Translate(lang, "remote_schedule_time_past"))
	}
	ctx := c.Request().Context()
	supported, err := remoteCanSchedule(ctx, cfg)
	if err != nil {
		return remoteFailure(c, err)
	}
	if !supported {
		return remoteScheduleUnsupported(c)
	}
	local, err := s.localArticle(id)
	if err != nil {
		return err
	}
	var out SyncPublishResponse
	err = s.remoteArticleCall(c, cfg, local, SyncRequest{Action: SyncActionScheduleArticle, ArticleID: id, PublishAt: &publishAt}, &out)
	var re *remoteError
	if errors.As(err, &re) && re.Status == http.StatusBadRequest {
		// 確かめた後に公開サーバーが古い版に戻された場合も、即時公開には切り替えない。送る値はこの PC で検査済みのため、
		// 400 は schedule_article を知らないことを表す。
		rememberRemoteSchedule(cfg, false)
		return remoteScheduleUnsupported(c)
	}
	if err != nil {
		return remoteSyncFailure(c, err, local)
	}
	s.adoptRemoteRef(c, out.Article)
	when := publishAt
	if out.Article.PublishedAt != nil {
		when = *out.Article.PublishedAt
	}
	now := clockNow()
	return c.JSON(http.StatusOK, map[string]any{
		"message":    i18n.Translate(lang, "remote_scheduled_done", scheduleTimeText(language(c), when, now)),
		"article":    out.Article,
		"publish_at": when.UTC(),
		"label":      i18n.Translate(lang, "remote_scheduled_label", scheduleTimeText(language(c), when, now)),
	})
}

// handleRemoteUnschedule は公開サーバーで予約中の記事の予約を取り消し、校閲待ちに戻す（unschedule_article）。
// 日時を変えるときは、取り消してから予約し直す。
func (s *Server) handleRemoteUnschedule(c echo.Context) error {
	id, err := remotePendingID(c)
	if err != nil {
		return err
	}
	cfg, err := s.requireRemote(auth.CanPublish, "予約の取り消し")
	if err != nil {
		return err
	}
	var out SyncPublishResponse
	err = callRemote(c.Request().Context(), cfg, SyncRequest{Action: SyncActionUnscheduleArticle, ArticleID: id}, &out)
	var re *remoteError
	if errors.As(err, &re) && re.Status == http.StatusBadRequest {
		rememberRemoteSchedule(cfg, false)
		return remoteScheduleUnsupported(c)
	}
	if err != nil {
		return remoteFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message": i18n.Translate(string(language(c)), "remote_unscheduled_done"),
		"article": out.Article,
	})
}

// --- サイト基本設定の反映 ---

// handleRemoteSettingsPush はローカルのサイト基本設定・テーマ・サイト構成（メニュー・トップページ・カード）を
// 公開サーバーへ反映する。ローカルの BaseURL が未設定またはこの PC 内（localhost）を指す場合は、公開サーバーの
// BaseURL を変えない。サイト構成を知らない古い公開サーバーは基本設定だけを反映するため、そのことを伝える。
//
// 適用中のテーマが独自テーマなら、テーマのファイルもいっしょに送る（handlers_remote_theme.go）。公開サーバーが
// 受け取れない（古い）ときは何も送らずに伝え、送った後に失敗したときは、何も変わっていないか、反映されたかどうか
// 分からないかを伝える（もう一度押せば、このPCの設定とテーマで上書きし直す）。
func (s *Server) handleRemoteSettingsPush(c echo.Context) error {
	cfg, err := s.requireRemote(auth.CanSyncSettings, "サイト基本設定の反映")
	if err != nil {
		return err
	}
	// 送る設定を読む前に未反映の記録の版を読み、反映の途中で保存された変更を、完了したときに消さないようにする。
	dirtyRev := s.remoteDirtyRevision()
	str := func(v string) *string { return &v }
	general := s.remoteGeneralSettings()
	settings := &SyncSettings{
		SiteName:        str(general.SiteName),
		SiteDescription: str(general.SiteDescription),
		Copyright:       str(general.Copyright),
		AnalyticsTag:    str(general.AnalyticsTag),
	}
	if general.BaseURL != "" {
		settings.BaseURL = str(general.BaseURL)
	}
	if t := s.getSetting("theme", ""); t != "" {
		settings.Theme = str(t)
	}
	st := s.loadSiteStructure(c.Request().Context())
	menu, cards := st.Menu, st.FrontCards
	if menu == nil {
		menu = []MenuItem{}
	}
	if cards == nil {
		cards = []string{}
	}
	footer := st.FooterMenu
	if footer == nil {
		footer = []MenuItem{}
	}
	tag, limit, title := st.TopGalleryTag, st.TopGalleryLimit, st.TopGalleryTitle
	settings.SiteMenu, settings.FrontPage, settings.FrontCards = &menu, &st.FrontPage, &cards
	settings.SiteFooterMenu = &footer
	settings.TopGalleryTag, settings.TopGalleryLimit, settings.TopGalleryTitle = &tag, &limit, &title
	layout, linkURL, linkLabel := st.TopGalleryLayout, st.TopGalleryLinkURL, st.TopGalleryLinkLabel
	settings.TopGalleryLayout, settings.TopGalleryLinkURL, settings.TopGalleryLinkLabel = &layout, &linkURL, &linkLabel
	cardImages := s.cardImageSettings()
	settings.ShowFrontCardImages, settings.ShowArticleCardImages, settings.ShowRelatedCardImages =
		&cardImages.Front, &cardImages.Article, &cardImages.Related
	// 多言語対応の ON/OFF も送り、公開サーバーの英語ページ（/en 以下）を PC の設定に揃える。保存していない PC では、
	// 英語の記事の有無から決めた今の値（isMultilingualEnabled）を送る。
	multilingual := s.isMultilingualEnabled()
	settings.MultilingualEnabled = &multilingual
	showAuthor := s.getShowAuthor()
	settings.ShowAuthor = &showAuthor
	// AI 機能の ON/OFF も送り、PC で OFF にしたら公開サーバーの外部 AI 連携（MCP・OAuth）も止める。「AI・外部連携」で
	// OFF にするときに「許可済みの連携も解除する」を選んでいたら、公開サーバーの連携も取り消してもらう。
	aiOn := general.AIEnabled
	settings.AIEnabled = &aiOn
	revokeMCP := s.remoteRevokeMCPPending()
	settings.RevokeAllMCP = revokeMCP

	ctx := c.Request().Context()
	client := remoteHTTPClient
	var pkg *SyncThemePackage
	var themeLabel string
	if settings.Theme != nil {
		var t theme.Theme
		pkg, t, err = s.remoteThemePackage(*settings.Theme)
		themeLabel = t.Label("ja")
		var local *remoteThemeError
		if errors.As(err, &local) {
			return sendSettingsPushResult(c, &settingsPushResult{status: http.StatusUnprocessableEntity, NotSent: true,
				Message: joinSentences(local.message, settingsNotSentNote)})
		} else if err != nil {
			return err
		}
	}
	if pkg != nil {
		// 独自テーマのファイルを送ると、server_info と大きな要求を待つため、応答の書き込みの期限を延ばす。
		if err := http.NewResponseController(c.Response()).SetWriteDeadline(time.Now().Add(remoteThemePushDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			c.Logger().Warnf("remote settings push: extend write deadline: %v", err)
		}
		if fail := checkRemoteThemeSupport(ctx, cfg, pkg, themeLabel); fail != nil {
			logRemoteFailure(c, fail)
			return sendSettingsPushResult(c, fail)
		}
		settings.ThemePackage = pkg
		client = remoteThemeHTTPClient()
	}
	var out SyncSettingsResponse
	if err := callRemoteWith(ctx, client, cfg, SyncRequest{Action: SyncActionSyncSettings, Settings: settings}, &out); err != nil {
		fail := settingsPushFailure(err, true)
		if fail == nil {
			return err
		}
		var re *remoteError
		if errors.As(err, &re) && re.cause != nil {
			c.Logger().Warnf("%v", re)
		}
		if fail.RemoteStatus == http.StatusUnprocessableEntity && st.usesNestedMenus() {
			// 子項目・見出しのみの項目を知らない古い公開サーバーは、見出しのみ（group）を未知の種類として断る。
			fail.Message = joinSentences(fail.Message, remoteNestedMenuRejectedNote)
		}
		if pkg != nil && fail.RemoteStatus == http.StatusRequestEntityTooLarge {
			fail.Message = fmt.Sprintf("独自テーマ「%s」のファイルが大きすぎて、公開サーバーが受け取れませんでした。画像やフォントを減らしてから取り込み直してください（公開サーバーの前にプロキシを置いている場合は、プロキシの要求の上限を 20MB 以上にしてください）。%s",
				themeLabel, settingsRejectedNote)
		}
		return sendSettingsPushResult(c, fail)
	}
	if out.Action != SyncActionSyncSettings {
		return sendSettingsPushResult(c, &settingsPushResult{status: http.StatusBadGateway, OutcomeUnknown: true, RemoteStatus: http.StatusOK,
			Message: joinSentences("公開サーバーの応答を読み取れませんでした", settingsUnknownNote)})
	}
	if pkg != nil && (out.ThemePackage == nil || out.ThemePackage.ID != pkg.ID || !strings.EqualFold(out.ThemePackage.SHA256, pkg.SHA256)) {
		// 設定は反映されたが、公開サーバーが送ったテーマのファイルを受け取ったことを確かめられない。
		return sendSettingsPushResult(c, &settingsPushResult{status: http.StatusBadGateway, Partial: true, RemoteStatus: http.StatusOK,
			Message: fmt.Sprintf("サイト基本設定は公開サーバーへ反映しましたが、独自テーマ「%s」のファイルを公開サーバーが受け取ったことを確かめられませんでした。公開サーバーの sorus を更新してから、もう一度「公開サーバーへ反映する」を押してください", themeLabel)})
	}
	msg := "サイト基本設定とサイト構成を公開サーバーへ反映しました"
	if pkg != nil {
		msg = fmt.Sprintf("サイト基本設定・サイト構成と、独自テーマ「%s」のファイルを公開サーバーへ反映しました", themeLabel)
	}
	reflected := remoteDirtyKinds
	if out.Settings.SiteMenu == nil {
		msg = "サイト基本設定を公開サーバーへ反映しました。公開サーバーの sorus が古いため、メニュー・トップページは反映されていません。公開サーバーの sorus を更新してください"
		// サイト構成は反映されていないため、未反映のまま残す。
		reflected = []string{remoteDirtyGeneral, remoteDirtyTheme}
	} else if out.Settings.ShowAuthor == nil {
		msg += "。公開サーバーの sorus が古いため、執筆者名の表示の設定は反映されていません。公開サーバーの sorus を更新してください"
	}
	msg += s.remoteAIResultNote(c, out, aiOn, revokeMCP)
	if out.Settings.SiteMenu != nil && out.Settings.SiteFooterMenu == nil && st.usesNestedMenus() {
		msg += "。公開サーバーの sorus が古いため、メニューのドロップダウン（子項目）とフッターのサイトマップは反映されていません。公開サーバーの sorus を更新してください"
	}
	if out.Settings.SiteMenu != nil && out.Settings.TopGalleryLayout == nil {
		msg += "。公開サーバーの sorus が古いため、フォトギャラリーの並べ方と「すべて見る」リンクは反映されていません。公開サーバーの sorus を更新してください"
	}
	if out.Settings.SiteMenu != nil {
		// フォトギャラリー（トップページのギャラリー・公開中の記事の :::gallery）は公開サーバーの media をタグで引くため、
		// そのタグの画像の記録とファイルも送り、このPCで外したタグは公開サーバーでも外す（handlers_remote_media.go）。
		// 送れなかったときは「フォトギャラリー／メディア」だけを未反映のまま残し、もう一度押せるようにする。
		tags, err := s.remoteGalleryTags(ctx, st.TopGalleryTag)
		if err != nil {
			return err
		}
		if len(tags) > 0 {
			if err := http.NewResponseController(c.Response()).SetWriteDeadline(time.Now().Add(remoteMediaPushDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				c.Logger().Warnf("remote settings push: extend write deadline: %v", err)
			}
		}
		res, err := s.pushRemoteMedia(ctx, cfg, tags)
		var re *remoteError
		switch {
		case errors.Is(err, errRemoteMediaUnsupported):
			msg += "。公開サーバーの sorus が古いため、フォトギャラリーの画像は反映されていません。公開サーバーの sorus を更新してください"
			reflected = []string{remoteDirtyGeneral, remoteDirtyTheme, remoteDirtyStructure}
		case err != nil:
			reason := "このPCの画像を読み込めませんでした"
			if errors.As(err, &re) {
				reason = re.Message
			}
			c.Logger().Warnf("remote settings push: gallery media: %v", err)
			s.clearRemoteDirty(c, dirtyRev, remoteDirtyGeneral, remoteDirtyTheme, remoteDirtyStructure)
			fail := &settingsPushResult{status: http.StatusBadGateway, Partial: true,
				Message: fmt.Sprintf("サイト基本設定とサイト構成は公開サーバーへ反映しましたが、フォトギャラリーの画像を反映できませんでした（%s）。もう一度「公開サーバーへ反映する」を押してください", reason)}
			if re != nil {
				fail.RemoteStatus = re.Status
			}
			return sendSettingsPushResult(c, fail)
		case res.UntagUnsupported:
			msg += "。公開サーバーの sorus が古いため、フォトギャラリーから外した写真は公開サイトに残っています。公開サーバーの sorus を更新してください"
		}
	}
	s.clearRemoteDirty(c, dirtyRev, reflected...)
	return c.JSON(http.StatusOK, map[string]any{
		"message":       msg,
		"settings":      out.Settings,
		"theme_package": out.ThemePackage,
	})
}

// logRemoteFailure は公開サーバーとの通信の失敗をログに残す（利用者には r.Message だけを見せる）。
func logRemoteFailure(c echo.Context, r *settingsPushResult) {
	c.Logger().Warnf("remote settings push: %s (remote status %d)", r.Message, r.RemoteStatus)
}
