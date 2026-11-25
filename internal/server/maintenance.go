package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/theme"
)

// 公開サーバーのメンテナンス表示。接続した PC の管理者が「設定」→「公開サーバー」から手動で始め、手動で終える
// （同期 API の get_maintenance・set_maintenance。PC 側は handlers_remote_maintenance.go）。自動では始めず、終えない
// （テーマの反映などとは連動しない）。
//
//   - 表示中は、公開サイトへの要求（ページ・フィード・画像・テーマのファイル・存在しないパス。メソッドを問わない）に、
//     メンテナンス画面と HTTP 503 を返す。画面は ① サイト運用で個別に選んだ独自 HTML（maintenancePageSetting）、
//     ② 適用中のテーマに同梱した maintenance.html（theme.MaintenanceFile）、③ sorus の標準画面、の順に決める
//     （resolveMaintenanceHTML）。どれも単一の HTML で、テーマの CSS・公開サイトのテンプレートは使わない。
//     Cache-Control: no-store を付け、CDN・ブラウザに保存させない。
//     ルーティングより前に判定し（e.Pre）、転送ルールのリダイレクトや 404 ページも返さない。
//   - 対象外は閲覧制限と同じ（siteGateExempt）: 同期 API（POST だけ。ほかのメソッドではほかのパスと同じ 503 にし、秘匿した
//     パスの有無を見せない）、管理画面と MCP サーバー（管理画面のあるサーバーモードだけ）、組み込みの共通アセット、稼働の
//     監視と、サーバー内部の要求（静的エクスポート・テーマのプレビュー）。ルーティングで実際にそれらの処理に届く要求だけで、
//     公開専用モードの /admin、/api の下の存在しないパス、対象外のパスに似せた要求（"/healthz/"・"//static/..." など）の
//     ように公開サイトの 404 ページ（テーマ・サイト名・メニュー）を返しうる要求は 503 にする。
//   - 閲覧制限（Basic 認証）は先に判定する（basicAuthMiddleware の後に置く）。公開サイトのページ・画像などを開いた、パスワードを
//     知らない人には今までどおり 401 だけを返し、メンテナンス中であることも見せない。
//   - 管理画面にログイン中の編集部員（サーバーモード）と、同期 API のトークンで画像を取得する PC（syncTokenReadsUpload）は
//     今までどおり通す（管理画面に出す画像・校閲待ちの記事の取り込みのため）。その応答は共有のキャッシュに残させない
//     （basicAuthPrivateCache が private にする）。
//   - 設定は settings の maintenanceSetting（ON/OFF）と maintenancePageSetting（訪問者に出す画面。標準または独自 HTML）
//     に保存し、起動時に読む（再起動しても続く）。この PC で選んでいる画面は maintenancePageLocalSetting に別保存し、
//     選んだだけでは訪問者向けの画面もメモリ（Server.maintenancePage）も変えない。書き込みは Server.maintenanceMu で排他し、
//     公開側は DB に保存してから同じロックの中でメモリ（Server.maintenance・Server.maintenancePage）に反映する。サイト基本設定の
//     反映（sync_settings。独自テーマのファイルを含む）・閲覧制限の変更（set_site_access）では変わらない。独自 HTML はテーマとは
//     独立してサイトごとに保存する。
//   - 働くのはデスクトップモード以外（maintenanceApplies）。デスクトップモードには同期 API が無く、復元などで設定が DB に
//     残っていても、自分の PC の公開サイトを止めない。
//   - sorus が直接配信する公開サイトにだけかかる。CDN に保存済みの応答、公開サーバーが止まっている・届かないとき、「サイトに
//     反映」で書き出した静的サイトは、この機能では制御できない（PC の画面とガイドで案内する）。

const (
	// maintenanceSetting はメンテナンス表示をしているか（"1" ならしている）を保存する settings のキー。updated_at は
	// 最後に始めた・終えた日時。
	maintenanceSetting = "maintenance_enabled"
	// maintenancePageSetting は訪問者に出す画面（JSON。name・sha256・html）。空または無いときは標準画面。
	// テーマとは独立し、サイトの DB（settings）に保存する。バックアップに含まれる。
	maintenancePageSetting = "maintenance_page"
	// maintenancePageLocalSetting はこの PC のこのサイトで選んでいる画面（JSON。同じ形）。公開サーバーの
	// maintenancePageSetting とは別キーで、選んだだけでは訪問者向けの画面は変わらない。
	maintenancePageLocalSetting = "maintenance_page_local"
	// maintenanceCacheControl はメンテナンス画面の Cache-Control。CDN・ブラウザに保存させない（終えた後に古い 503 を
	// 配らせない）。
	maintenanceCacheControl = "no-store"
	// maintenanceCodeMissing は set_maintenance に切り替え先（maintenance.enabled）も画面（maintenance.page）も無いときの
	// 応答の code。
	maintenanceCodeMissing = "maintenance_missing"
	// maintenanceCodePageInvalid は画面の指定が不正なときの応答の code。
	maintenanceCodePageInvalid = "maintenance_page_invalid"
	// maintenanceCodePageNotActive は表示中でないときに画面だけを送ったときの応答の code（ON/OFF は変えない）。
	maintenanceCodePageNotActive = "maintenance_page_not_active"
	// maxMaintenancePageBytes は独自 HTML の上限（1 MiB。テーマ同梱の maintenance.html と同じ）。CSS や画像は単一
	// ファイルに含める。
	maxMaintenancePageBytes = theme.MaxMaintenanceSize
	// settingsTimeLayout は settings の updated_at（SQLite の CURRENT_TIMESTAMP と同じ UTC の書式）。
	settingsTimeLayout = "2006-01-02 15:04:05"
)

// maintenanceState は保存済みのメンテナンス表示の設定。
type maintenanceState struct {
	Enabled bool
	// ChangedAt は最後に始めた・終えた日時。一度も切り替えていなければ nil。
	ChangedAt *time.Time
	Page      maintenancePage
}

// maintenancePage は訪問者に出す画面。HTML が空なら標準画面。
type maintenancePage struct {
	Kind string
	Name string
	// HTML は独自画面の本文。標準画面のときは空。メモリと DB に持ち、要求のたびにファイルは読まない。
	HTML      string
	SHA256    string
	UpdatedAt *time.Time
}

func (p maintenancePage) isCustom() bool {
	return p.Kind == SyncMaintenancePageCustom && p.HTML != ""
}

func (p maintenancePage) info() *SyncMaintenancePageInfo {
	kind := p.Kind
	if kind == "" {
		kind = SyncMaintenancePageDefault
	}
	info := &SyncMaintenancePageInfo{Kind: kind, Name: p.Name, SHA256: p.SHA256, UpdatedAt: p.UpdatedAt}
	if !p.isCustom() {
		info.Kind = SyncMaintenancePageDefault
		info.Name = ""
		info.SHA256 = ""
	}
	return info
}

func (p maintenancePage) equal(o maintenancePage) bool {
	return p.info().Kind == o.info().Kind && p.Name == o.Name && p.SHA256 == o.SHA256
}

// sameContent は訪問者に見える内容が同じか（種類と HTML の SHA-256。ファイル名は表示用なので含めない）。
func (p maintenancePage) sameContent(o maintenancePage) bool {
	return p.info().Kind == o.info().Kind && p.SHA256 == o.SHA256
}

func (p maintenancePage) toSync() *SyncMaintenancePage {
	if !p.isCustom() {
		return &SyncMaintenancePage{Kind: SyncMaintenancePageDefault}
	}
	return &SyncMaintenancePage{Kind: SyncMaintenancePageCustom, Name: p.Name, HTML: p.HTML}
}

// storedMaintenancePage は settings の maintenance_page に保存する JSON。
type storedMaintenancePage struct {
	Name   string `json:"name,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	HTML   string `json:"html,omitempty"`
}

// loadMaintenanceState は保存済みのメンテナンス表示の設定を q（DB またはトランザクション）から読む。
func loadMaintenanceState(ctx context.Context, q sqlx.QueryerContext) (maintenanceState, error) {
	st := maintenanceState{Page: defaultMaintenancePage()}
	var row struct {
		Value     string    `db:"value"`
		UpdatedAt time.Time `db:"updated_at"`
	}
	err := sqlx.GetContext(ctx, q, &row, `SELECT value, updated_at FROM settings WHERE key = ?`, maintenanceSetting)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return maintenanceState{}, fmt.Errorf("server: load maintenance setting: %w", err)
	}
	if err == nil {
		at := row.UpdatedAt.UTC()
		st.Enabled = row.Value == "1"
		st.ChangedAt = &at
	}
	page, err := loadMaintenancePage(ctx, q)
	if err != nil {
		return maintenanceState{}, err
	}
	st.Page = page
	return st, nil
}

func defaultMaintenancePage() maintenancePage {
	return maintenancePage{Kind: SyncMaintenancePageDefault}
}

// loadMaintenancePage は訪問者に出す画面を読む。無ければ標準画面。JSON が壊れても標準画面（表示中なら 503 は続ける）。
func loadMaintenancePage(ctx context.Context, q sqlx.QueryerContext) (maintenancePage, error) {
	return loadStoredMaintenancePage(ctx, q, maintenancePageSetting)
}

// loadLocalMaintenancePage はこの PC で選んでいる画面を読む。訪問者向けの画面とは別キー。
func loadLocalMaintenancePage(ctx context.Context, q sqlx.QueryerContext) (maintenancePage, error) {
	return loadStoredMaintenancePage(ctx, q, maintenancePageLocalSetting)
}

func loadStoredMaintenancePage(ctx context.Context, q sqlx.QueryerContext, key string) (maintenancePage, error) {
	var row struct {
		Value     string    `db:"value"`
		UpdatedAt time.Time `db:"updated_at"`
	}
	err := sqlx.GetContext(ctx, q, &row, `SELECT value, updated_at FROM settings WHERE key = ?`, key)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultMaintenancePage(), nil
	}
	if err != nil {
		return maintenancePage{}, fmt.Errorf("server: load maintenance page: %w", err)
	}
	at := row.UpdatedAt.UTC()
	var stored storedMaintenancePage
	if err := json.Unmarshal([]byte(row.Value), &stored); err != nil {
		return maintenancePage{Kind: SyncMaintenancePageDefault, UpdatedAt: &at}, nil
	}
	if stored.HTML == "" {
		return maintenancePage{Kind: SyncMaintenancePageDefault, UpdatedAt: &at}, nil
	}
	return maintenancePage{
		Kind:      SyncMaintenancePageCustom,
		Name:      stored.Name,
		HTML:      stored.HTML,
		SHA256:    stored.SHA256,
		UpdatedAt: &at,
	}, nil
}

// saveMaintenance はメンテナンス表示の設定 enabled を、切り替えた日時 at とともに保存する。呼び出し側は
// Server.maintenanceMu を持ち、保存できてからメモリに反映する。
func saveMaintenance(ctx context.Context, ext sqlx.ExtContext, enabled bool, at time.Time) error {
	value := "0"
	if enabled {
		value = "1"
	}
	if _, err := ext.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		maintenanceSetting, value, at.UTC().Format(settingsTimeLayout)); err != nil {
		return fmt.Errorf("server: save maintenance setting: %w", err)
	}
	return nil
}

// saveMaintenancePage は訪問者に出す画面 page を日時 at とともに保存する。呼び出し側は Server.maintenanceMu を持つ。
func saveMaintenancePage(ctx context.Context, ext sqlx.ExtContext, page maintenancePage, at time.Time) error {
	return saveStoredMaintenancePage(ctx, ext, maintenancePageSetting, page, at)
}

// saveLocalMaintenancePage はこの PC で選んでいる画面を保存する。訪問者向けの画面とメモリは変えない。
func saveLocalMaintenancePage(ctx context.Context, ext sqlx.ExtContext, page maintenancePage, at time.Time) error {
	return saveStoredMaintenancePage(ctx, ext, maintenancePageLocalSetting, page, at)
}

func saveStoredMaintenancePage(ctx context.Context, ext sqlx.ExtContext, key string, page maintenancePage, at time.Time) error {
	stored := storedMaintenancePage{}
	if page.isCustom() {
		stored = storedMaintenancePage{Name: page.Name, SHA256: page.SHA256, HTML: page.HTML}
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("server: encode maintenance page: %w", err)
	}
	if _, err := ext.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(raw), at.UTC().Format(settingsTimeLayout)); err != nil {
		return fmt.Errorf("server: save maintenance page: %w", err)
	}
	return nil
}

func (s *Server) setMaintenancePage(p maintenancePage) {
	cp := p
	s.maintenancePage.Store(&cp)
}

func (s *Server) currentMaintenancePage() maintenancePage {
	if p := s.maintenancePage.Load(); p != nil {
		return *p
	}
	return defaultMaintenancePage()
}

// maintenanceApplies は、このサーバーでメンテナンス表示を働かせるか（デスクトップモードでないか）を返す。
func (s *Server) maintenanceApplies() bool {
	return !s.cfg.Desktop
}

// maintenanceActive は、いま公開サイトの訪問者にメンテナンス画面を返しているかを返す。
func (s *Server) maintenanceActive() bool {
	return s.maintenance.Load() && s.maintenanceApplies()
}

// reloadMaintenance は保存済みのメンテナンス表示の設定を DB から読み直してメモリに反映する（復元の後）。
// 読めなければ今の状態のまま。
func (s *Server) reloadMaintenance() error {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	st, err := loadMaintenanceState(context.Background(), s.db)
	if err != nil {
		return err
	}
	s.maintenance.Store(st.Enabled)
	s.setMaintenancePage(st.Page)
	return nil
}

// maintenanceMiddleware はメンテナンス表示中、対象の要求にメンテナンス画面（503）を返す。閲覧制限より後、転送ルールより
// 前に e.Pre で登録する（ファイルの先頭の説明を参照）。
func (s *Server) maintenanceMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !s.maintenanceActive() || s.siteGateExempt(c) {
				return next(c)
			}
			if s.isStaff(c) || s.syncTokenReadsUpload(c) {
				// 訪問者に見せない内容のため、共有のキャッシュ（CDN）に残させない。
				c.Set(basicAuthGuardedKey, true)
				return next(c)
			}
			return s.serveMaintenancePage(c)
		}
	}
}

func setMaintenanceHeaders(c echo.Context) {
	h := c.Response().Header()
	h.Set(echo.HeaderCacheControl, maintenanceCacheControl)
	h.Set(echo.HeaderXContentTypeOptions, "nosniff")
}

// serveMaintenancePage はメンテナンス画面を 503 で返す。どの画面を出すかは resolveMaintenanceHTML が決める。
// サイトの内容（サイト名を含む）は標準画面には出さない。
func (s *Server) serveMaintenancePage(c echo.Context) error {
	body, _ := s.resolveMaintenanceHTML(c, s.currentMaintenancePage())
	setMaintenanceHeaders(c)
	return c.HTML(http.StatusServiceUnavailable, body)
}

// メンテナンス画面の出どころ（resolveMaintenanceHTML）。
const (
	maintenanceSourceCustom  = "custom"
	maintenanceSourceTheme   = "theme"
	maintenanceSourceDefault = "default"
)

// resolveMaintenanceHTML は出すメンテナンス画面の HTML と出どころを返す。① page が独自 HTML ならそれ、② 適用中の
// テーマが maintenance.html を同梱していればそれ、③ どちらも無ければ sorus の標準画面（表示言語は c で決める）。
// 公開サーバーでは page に訪問者向けの画面（maintenancePageSetting）を、PC のダウンロードでは PC で選んでいる画面
// （maintenancePageLocalSetting）を渡す。
func (s *Server) resolveMaintenanceHTML(c echo.Context, page maintenancePage) (string, string) {
	if page.isCustom() {
		return page.HTML, maintenanceSourceCustom
	}
	if t, ok := s.maintenanceTheme(); ok {
		body, _ := t.MaintenanceHTML()
		return body, maintenanceSourceTheme
	}
	return defaultMaintenanceHTML(c), maintenanceSourceDefault
}

// maintenanceTheme は適用中のテーマ（サイトの設定の theme。カタログに無ければ既定のテーマ）が maintenance.html を
// 同梱していれば、そのテーマを返す。テーマのプレビュー（?theme_preview など）では変えない。
func (s *Server) maintenanceTheme() (theme.Theme, bool) {
	t := s.themeCatalog().Resolve(s.getSetting("theme", theme.Default))
	if _, ok := t.MaintenanceHTML(); !ok {
		return theme.Theme{}, false
	}
	return t, true
}

// defaultMaintenanceHTML は sorus の標準のメンテナンス画面（表示言語は c で決める）。
func defaultMaintenanceHTML(c echo.Context) string {
	lang := string(language(c))
	title := html.EscapeString(localizedMessage(c, "maintenance_page_title"))
	body := html.EscapeString(localizedMessage(c, "maintenance_page_body"))
	return `<!doctype html>
<html lang="` + lang + `"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex"><title>` + title + `</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 36rem; margin: 4rem auto; padding: 0 1rem; color: #333; line-height: 1.7">
<h1 style="font-size: 1.25rem">` + title + `</h1>
<p>` + body + `</p>
</body></html>
`
}

// syncGetMaintenance は公開サーバーのメンテナンス表示の状態を返す。保存済みの設定とメモリ上の状態を同じロックの中で読み、
// 切り替えの途中の状態を返さない。
func (s *Server) syncGetMaintenance(c echo.Context) error {
	s.maintenanceMu.Lock()
	st, err := loadMaintenanceState(c.Request().Context(), s.db)
	active := s.maintenanceActive()
	s.maintenanceMu.Unlock()
	if err != nil {
		return fmt.Errorf("server: sync get maintenance: %w", err)
	}
	return c.JSON(http.StatusOK, SyncMaintenanceResponse{Action: SyncActionGetMaintenance, Enabled: active, ChangedAt: st.ChangedAt,
		Page: st.Page.info()})
}

// syncSetMaintenance は公開サーバーのメンテナンス表示を、指定された状態（Maintenance.Enabled）にする。すでにその状態なら
// ON/OFF は書かない（切り替えた日時もそのまま）。オフからオンにするときは画面もいっしょに保存する（Page が無ければ標準画面）。
// すでにオンのときに Enabled だけを付けて送っても画面は変えない。画面だけを送ると、表示中なら画面を更新し、表示中でなければ
// 何も変えずに断る。DB に保存できてから同じロックの中でメモリに反映するため、保存に失敗すればどちらも変わらない。
func (s *Server) syncSetMaintenance(c echo.Context, req *SyncRequest) error {
	if req.Maintenance == nil || (req.Maintenance.Enabled == nil && req.Maintenance.Page == nil) {
		return syncCodedError(c, http.StatusBadRequest, maintenanceCodeMissing,
			"maintenance.enabled（始めるなら true、終えるなら false）か、maintenance.page（表示中の画面の更新）を指定してください")
	}
	var parsed *maintenancePage
	if req.Maintenance.Page != nil {
		p, err := parseSyncMaintenancePage(req.Maintenance.Page)
		if err != nil {
			return syncCodedError(c, http.StatusBadRequest, maintenanceCodePageInvalid, err.Error())
		}
		parsed = &p
	}
	ctx := c.Request().Context()
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	cur, err := loadMaintenanceState(ctx, s.db)
	if err != nil {
		return fmt.Errorf("server: sync set maintenance: %w", err)
	}

	writeEnabled := false
	writePage := false
	wantEnabled := cur.Enabled
	nextPage := cur.Page
	if req.Maintenance.Enabled != nil {
		wantEnabled = *req.Maintenance.Enabled
		if cur.Enabled != wantEnabled {
			writeEnabled = true
		}
		if wantEnabled && !cur.Enabled {
			// オフからオン：指定の画面を一体で反映。Page が無ければ標準画面。
			if parsed != nil {
				nextPage = *parsed
			} else {
				nextPage = defaultMaintenancePage()
			}
			writePage = !cur.Page.equal(nextPage)
		}
	} else {
		// 画面だけ：表示中でなければモードを変えない。
		if !cur.Enabled {
			return syncCodedError(c, http.StatusConflict, maintenanceCodePageNotActive,
				"メンテナンス表示中ではないため、画面は反映しませんでした（表示の開始・終了は変わっていません）")
		}
		nextPage = *parsed
		writePage = !cur.Page.equal(nextPage)
	}

	at := clockNow().UTC().Truncate(time.Second)
	if writeEnabled || writePage {
		tx, err := s.db.BeginTxx(ctx, nil)
		if err != nil {
			return fmt.Errorf("server: sync set maintenance: %w", err)
		}
		defer tx.Rollback()
		if writeEnabled {
			if err := saveMaintenance(ctx, tx, wantEnabled, at); err != nil {
				return err
			}
			cur.Enabled = wantEnabled
			cur.ChangedAt = &at
		}
		if writePage {
			if err := saveMaintenancePage(ctx, tx, nextPage, at); err != nil {
				return err
			}
			nextPage.UpdatedAt = &at
			cur.Page = nextPage
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("server: sync set maintenance: %w", err)
		}
	}
	s.maintenance.Store(wantEnabled)
	s.setMaintenancePage(cur.Page)
	return c.JSON(http.StatusOK, SyncMaintenanceResponse{Action: SyncActionSetMaintenance, Enabled: s.maintenanceActive(),
		ChangedAt: cur.ChangedAt, Changed: writeEnabled, Page: cur.Page.info(), PageChanged: writePage})
}

func parseSyncMaintenancePage(in *SyncMaintenancePage) (maintenancePage, error) {
	if in == nil {
		return maintenancePage{}, errors.New("画面の指定がありません")
	}
	switch strings.TrimSpace(in.Kind) {
	case SyncMaintenancePageDefault:
		if strings.TrimSpace(in.HTML) != "" || strings.TrimSpace(in.Name) != "" {
			return maintenancePage{}, errors.New("標準の画面を使うときは html と name を空にしてください")
		}
		return defaultMaintenancePage(), nil
	case SyncMaintenancePageCustom:
		return parseCustomMaintenancePage(in.Name, []byte(in.HTML))
	default:
		return maintenancePage{}, errors.New("画面の種類（custom または default）を指定してください")
	}
}

// parseCustomMaintenancePage はエンジニアが用意した単一 HTML を検査して保存用の画面にする。
// ファイル名は表示にだけ使い、パスには使わない。
func parseCustomMaintenancePage(rawName string, data []byte) (maintenancePage, error) {
	name, err := parseMaintenanceFileName(rawName)
	if err != nil {
		return maintenancePage{}, err
	}
	html, err := parseMaintenanceHTML(data)
	if err != nil {
		return maintenancePage{}, err
	}
	sum := sha256.Sum256([]byte(html))
	return maintenancePage{
		Kind:   SyncMaintenancePageCustom,
		Name:   name,
		HTML:   html,
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func parseMaintenanceFileName(raw string) (string, error) {
	if strings.ContainsAny(raw, `/\`) {
		return "", errors.New("ファイル名にパスは使えません")
	}
	name := sanitizeFilename(raw)
	ext := strings.ToLower(path.Ext(name))
	if name == "" || name == "." || name == ".." || name == "file" || (ext != ".html" && ext != ".htm") {
		return "", errors.New("HTML ファイル（.html または .htm）を選んでください")
	}
	return name, nil
}

// parseMaintenanceHTML は独自 HTML を検査する（検査はテーマ同梱の maintenance.html と同じ theme.NormalizeMaintenanceHTML）。
// エラーは利用者に見せる文にする。
func parseMaintenanceHTML(data []byte) (string, error) {
	html, err := theme.NormalizeMaintenanceHTML(data)
	switch {
	case err == nil:
		return html, nil
	case errors.Is(err, theme.ErrMaintenanceTooLarge):
		return "", fmt.Errorf("HTML ファイルが大きすぎます（上限 %s）", formatByteSize(maxMaintenancePageBytes))
	case errors.Is(err, theme.ErrMaintenanceBinary):
		return "", errors.New("HTML ファイルの内容を読めません")
	case errors.Is(err, theme.ErrMaintenanceNotUTF8):
		return "", errors.New("HTML ファイルは UTF-8 で保存してください")
	default:
		return "", errors.New("HTML ファイルが空です")
	}
}
