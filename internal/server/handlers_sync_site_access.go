package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
)

// 公開サーバー自身の閲覧制限（Basic 認証）を、接続した PC の管理者が同期 API で確認・変更する（get_site_access・
// set_site_access）。公開専用モードの公開サーバーには管理画面が無いため、閲覧制限はこの API だけで変える。
// 保存先は管理画面の「サイト設定」と同じ settings のキーで、判定も同じミドルウェア（basicAuthMiddleware）が行う。
// PC 側の画面とプロキシは handlers_remote_site_access.go。

// 同期 API の閲覧制限のエラーの種類（応答の code）。PC はこれを見て、利用者の表示言語の文言にする。
const (
	siteAccessCodeMissing          = "site_access_missing"
	siteAccessCodeRevisionRequired = "site_access_revision_required"
	siteAccessCodeConflict         = "site_access_conflict"
	siteAccessCodeInsecure         = "site_access_insecure"
)

// syncCodedError は message と、種類を表す code を JSON で返す。
func syncCodedError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]string{"message": message, "code": code})
}

// siteAccessResponse は保存済みの設定 st と、サーバーのメモリ上で閲覧制限が働いているか active から、要求 c への
// 応答を作る。c が TLS で直接届いた要求でなければ InsecureTransport を付ける（この接続では set_site_access を受け付けない）。
func (s *Server) siteAccessResponse(c echo.Context, action string, st basicAuthState, active, changed bool) SyncSiteAccessResponse {
	return SyncSiteAccessResponse{
		Action:            action,
		Enabled:           st.Enabled,
		User:              st.User,
		PasswordSet:       st.PassHash != "",
		Active:            active,
		Revision:          st.Revision,
		SiteURL:           s.getSetting("base_url", s.getBaseURL()),
		CheckPaths:        s.siteAccessCheckPaths(c.Request().Context()),
		Changed:           changed,
		InsecureTransport: !secureSyncRequest(c.Request()),
	}
}

// siteAccessCheckPaths は、PC がトップページに加えて外から開いてみるパスを返す。CDN は閲覧制限を有効にする前の
// 記事（s-maxage）・画像（max-age 1年、immutable）を保持していることがあるため、トップページだけでは確かめきれない。
// フィード、最新の公開記事、公開中の画像（最新の記事のアイキャッチ。無ければアップロード済みの最新の画像）を並べる。
// これは代表の数件で、CDN に残るすべてのキャッシュを確かめるものではない。読めないものは省く。
func (s *Server) siteAccessCheckPaths(ctx context.Context) []string {
	paths := []string{"/feed.xml"}
	var latest struct {
		publicArticle
		Image string `db:"image"`
	}
	err := s.db.GetContext(ctx, &latest, `SELECT a.kind, a.slug, COALESCE(NULLIF(a.lang, ''), 'ja') AS lang, a.featured_image_url AS image
		FROM articles a WHERE `+postKindCondition+` AND `+publishedCondition+` ORDER BY julianday(a.published_at) DESC, a.id LIMIT 1`,
		article.StatusPublished, clockNow().UTC())
	if err == nil {
		paths = append(paths, latest.URL())
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.echo.Logger.Warnf("server: site access check paths: %v", err)
	}
	if img := s.siteAccessCheckImage(ctx, latest.Image); img != "" {
		paths = append(paths, img)
	}
	return paths
}

// siteAccessCheckImage は外から開いてみる公開中の画像のパスを返す。featured が実在するアップロード画像ならそれ、
// そうでなければアップロード先にある最新の画像（同期で受け取る名前のもの）。無ければ空。
func (s *Server) siteAccessCheckImage(ctx context.Context, featured string) string {
	exists := func(name string) bool {
		if !syncImageNamePattern.MatchString(name) {
			return false
		}
		fi, err := os.Stat(filepath.Join(s.cfg.UploadDir, name))
		return err == nil && fi.Mode().IsRegular()
	}
	if name, ok := strings.CutPrefix(featured, UploadURLPrefix); ok && exists(name) {
		return UploadURLPrefix + name
	}
	if ctx.Err() != nil {
		return ""
	}
	entries, err := os.ReadDir(s.cfg.UploadDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if !e.Type().IsRegular() || !syncImageNamePattern.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || fi.ModTime().After(newestAt) {
			newest, newestAt = e.Name(), fi.ModTime()
		}
	}
	if newest == "" {
		return ""
	}
	return UploadURLPrefix + newest
}

// syncGetSiteAccess は公開サーバーの閲覧制限の状態を返す。保存済みの設定とメモリ上の判定を同じロックの中で読み、
// 書き込みの途中の状態を返さない。
func (s *Server) syncGetSiteAccess(c echo.Context) error {
	s.basicAuthMu.Lock()
	st, err := loadBasicAuthState(c.Request().Context(), s.db)
	active := s.basicAuthActive()
	s.basicAuthMu.Unlock()
	if err != nil {
		return fmt.Errorf("server: sync get site access: %w", err)
	}
	return c.JSON(http.StatusOK, s.siteAccessResponse(c, SyncActionGetSiteAccess, st, active, false))
}

// syncSetSiteAccess は公開サーバーの閲覧制限を変更する。入力は「サイト設定」と同じ規則で検査し、パスワードが空なら
// 保存済みのパスワードを使い続ける。読み込んだときの版（ExpectedRevision）が保存済みの版と違えば、ほかの PC が先に
// 変更したとして何も変えずに 409 を返す。保存はトランザクションで行い、コミットしてから同じロックの中でメモリに
// 反映するため、DB とメモリの設定が食い違わない（保存に失敗すれば、どちらも変わらない）。
func (s *Server) syncSetSiteAccess(c echo.Context, req *SyncRequest) error {
	in := req.SiteAccess
	if in == nil {
		return syncCodedError(c, http.StatusBadRequest, siteAccessCodeMissing, "site_access を指定してください")
	}
	if in.ExpectedRevision == nil {
		return syncCodedError(c, http.StatusBadRequest, siteAccessCodeRevisionRequired,
			"expected_revision を指定してください（get_site_access の応答の revision）")
	}
	// 閲覧用のパスワードは、sorus が TLS で直接受けた接続でだけ受け取る（クライアントが付けられるヘッダーや、
	// ループバックからの接続かどうかでは判断しない。secureSyncRequest を参照）。
	if !secureSyncRequest(c.Request()) {
		return syncCodedError(c, http.StatusForbidden, siteAccessCodeInsecure,
			"閲覧制限は、公開サーバーの sorus が https（TLS）で直接受けた接続でだけ変更できます。AUTOCERT_DOMAIN を設定して sorus に https を直接受けさせ、https:// で接続してください（前段のプロキシから http で中継した接続では変更できません）")
	}
	user := strings.TrimSpace(in.User)
	// 形式の誤りは、パスワードのハッシュを作る前とロックを取る前に断る（保存済みのパスワードの有無はロックの中で確かめる）。
	if err := validateBasicAuthInput(in.Enabled, user, in.Password, true); err != nil {
		return syncCodedError(c, http.StatusUnprocessableEntity, err.Error(), i18n.Translate(string(i18n.JA), err.Error()))
	}
	var newHash string
	if in.Password != "" {
		var err error
		if newHash, err = auth.HashPassword(in.Password); err != nil {
			return fmt.Errorf("server: sync set site access: hash: %w", err)
		}
	}

	ctx := c.Request().Context()
	s.basicAuthMu.Lock()
	defer s.basicAuthMu.Unlock()
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("server: sync set site access: %w", err)
	}
	defer tx.Rollback()
	cur, err := loadBasicAuthState(ctx, tx)
	if err != nil {
		return fmt.Errorf("server: sync set site access: %w", err)
	}
	if cur.Revision != *in.ExpectedRevision {
		return syncCodedError(c, http.StatusConflict, siteAccessCodeConflict,
			"閲覧制限はほかの PC で変更されています。最新の状態を読み込んでから、もう一度変更してください")
	}
	if err := validateBasicAuthInput(in.Enabled, user, in.Password, cur.PassHash != ""); err != nil {
		return syncCodedError(c, http.StatusUnprocessableEntity, err.Error(), i18n.Translate(string(i18n.JA), err.Error()))
	}
	next := basicAuthSettings{Enabled: in.Enabled, User: user, PassHash: cur.PassHash}
	if newHash != "" {
		next.PassHash = newHash
	}
	saved, changed, err := saveBasicAuthTx(ctx, tx, cur, next)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("server: sync set site access: %w", err)
	}
	if changed {
		s.setBasicAuth(saved.basicAuthSettings)
	}
	return c.JSON(http.StatusOK, s.siteAccessResponse(c, SyncActionSetSiteAccess, saved, s.basicAuthActive(), changed))
}

// secureSyncRequest は、閲覧用のパスワードを受け取ってよい接続で要求が届いたか（TLS のハンドシェイクを終えた接続で、
// このサーバーに直接届いたか）を返す。本番の公開サーバーは AUTOCERT_DOMAIN で sorus 自身が TLS を受ける（平文の
// ポートは https へ転送する）。
//
// 平文の http で届いた要求は、この PC 内（ループバック）からの接続でも受け付けない。同じマシンのリバースプロキシ
// （既定の nginx の proxy_pass http://127.0.0.1:PORT; など）は、外から届いた要求を、Host を上流のアドレスにし、
// 転送ヘッダーを付けずにループバックで中継する。sorus から見ると接続元・接続先・Host がすべてこの PC 内になり、
// 直接の要求と見分けられない（sorus を 127.0.0.1 だけで待ち受けても、前段のプロキシの有無は分からない）。
// 転送ヘッダー（X-Forwarded-Proto・Forwarded など）と Host も、クライアントが自由に付けられるため信頼しない（Echo の
// Context.Scheme は接続元を見ずにこれらを採用するため使わない）。sorus には信頼するプロキシを設定する仕組みが無いため、
// 前段のプロキシ・CDN で TLS を終えて平文で sorus に渡す構成では閲覧制限を変更できない（site_access_insecure を返す。
// 閲覧制限の確認 get_site_access は今までどおり使え、応答の InsecureTransport でこの状態を PC に伝える）。
func secureSyncRequest(r *http.Request) bool {
	return r.TLS != nil && r.TLS.HandshakeComplete
}
