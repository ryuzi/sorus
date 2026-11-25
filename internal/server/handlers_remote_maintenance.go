package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/theme"
)

// 「公開サーバー」画面の「公開サーバーのメンテナンス表示」。公開サーバーのメンテナンス表示を、同期 API の
// get_maintenance・set_maintenance で確認し、手動で始める・終える（公開サーバー側は maintenance.go）。
//
//   - ON/OFF はこの PC には保存しない。画面を開くたび・操作のたびに公開サーバーに問い合わせ、公開サーバーが返した状態だけを
//     表示する。独自 HTML の選択はこの PC のこのサイトに保存する（選択・保存だけでは公開サーバーは変わらない）。
//   - 始めるときは、この PC で選んでいる画面をいっしょに送り、公開サーバーの画面と ON を一体で反映する。表示中の画面の更新は
//     「画面を反映」（Page だけを送る。ON/OFF は変えない）。
//   - 切り替えは「始める」「終える」の状態を指定して送る（反転ではない）。同じ要求を何度送っても同じ状態になるため、版の
//     照合は要らない。ほかの PC が先に同じ状態にしていれば「すでに…」と伝える。
//   - 公開サーバーに問い合わせられない・古い（未対応の action）・応答を読めないときは「分からない」とし、成功とは見せない。
//     切り替えを送った後に応答が無い・読めない・公開サーバーのエラーのときは、反映されたかどうか分からないと伝え、
//     「状態を確かめる」（公開サーバーへの問い合わせ）で確かめてもらう。自動ではやり直さない。
//   - get_maintenance・set_maintenance の応答には page が必要。無ければ読み取れない応答（結果不明）として扱う。
//   - 権限は「公開サーバーの閲覧制限」と同じ（ローカルの管理者が、管理者のトークンで接続しているとき）。HTML の保存と反映も
//     管理者専用で、CSRF で守る。
//   - 画面の優先順位は公開サーバーと同じ（resolveMaintenanceHTML）: ① この PC で選んだ独自 HTML、② 適用中のテーマに同梱した
//     maintenance.html、③ 標準画面。「現在のメンテナンス画面をダウンロード」（GET /admin/remote/maintenance/page/download）
//     は、この順で決まる画面を maintenance.html として返す。手元で編集して「この PC に保存」で選び直せる（ダウンロード→
//     編集→アップロード）。テーマ同梱の画面を使うときは、そのことを画面に出す（maintenancePageView の Source・ThemeName）。

const (
	// remoteMaintenanceTimeout はメンテナンス表示の確認・切り替えで、公開サーバーへの1回の要求の制限時間。
	remoteMaintenanceTimeout = 20 * time.Second
	// maintenanceTimeLayout は画面に出す日時の書式（日本時間）。
	maintenanceTimeLayout = "2006/01/02 15:04:05"
)

// 画面に出す状態（remoteMaintenanceView.Status・remoteMaintenanceFailure.Status）。
const (
	maintenanceStatusOn      = "on"
	maintenanceStatusOff     = "off"
	maintenanceStatusUnknown = "unknown"
)

// remoteMaintenanceView は画面に出す公開サーバーのメンテナンス表示の状態。
type remoteMaintenanceView struct {
	Enabled bool `json:"enabled"`
	// Status・Tone・Summary は状態（on・off）と、その色（warn・neutral）・説明（表示言語）。
	Status     string `json:"status"`
	Tone       string `json:"tone"`
	Summary    string `json:"summary"`
	StateLabel string `json:"state_label"`
	// ChangedLabel は最後に始めた・終えた時刻の表示。一度も切り替えていなければ空。
	ChangedLabel string `json:"changed_label"`
	// CheckedLabel は公開サーバーに問い合わせた時刻の表示。
	CheckedLabel string `json:"checked_label"`
	// LocalPage はこの PC のこのサイトで選んでいる画面。公開サーバーの状態とは別に出す。
	LocalPage *maintenancePageView `json:"local_page,omitempty"`
	// Page は公開サーバーが返した、保存済みの画面（対応しているサーバーは必ず付ける）。
	Page *maintenancePageView `json:"page,omitempty"`
}

// maintenancePageView は画面に出す、選んでいる／反映済みの画面。
type maintenancePageView struct {
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Label  string `json:"label"`
	// MatchesLocal は公開サーバーの画面が、この PC で選んでいる画面と同じか。
	MatchesLocal bool   `json:"matches_local"`
	UpdatedLabel string `json:"updated_label,omitempty"`
	// Source はこの PC で選んでいる画面の、実際の出どころ（custom・theme・default）。この PC の画面だけに付ける。
	Source string `json:"source,omitempty"`
	// ThemeName は適用中のテーマが maintenance.html を同梱しているときの、そのテーマの表示名（無ければ空）。
	ThemeName string `json:"theme_name,omitempty"`
	// SourceNote はテーマ同梱の画面との関係の説明（独自 HTML を優先している・テーマの画面を使う）。無ければ空。
	SourceNote string `json:"source_note,omitempty"`
}

// remoteMaintenanceFailure はメンテナンス表示の確認・切り替えの失敗の応答。Status は分からないことを表す "unknown"。
type remoteMaintenanceFailure struct {
	Message      string `json:"message"`
	Status       string `json:"status"`
	RemoteStatus int    `json:"remote_status"`
	// OutcomeUnknown は切り替えを送った後に失敗し、反映されたかどうか分からないか。
	OutcomeUnknown bool `json:"outcome_unknown,omitempty"`
	// State は公開サーバーが返した状態が、指定した状態と違ったときの、その状態（切り替わっていない）。
	State *remoteMaintenanceView `json:"state,omitempty"`
	// LocalPage は失敗しても、この PC の選択はそのまま見せる。
	LocalPage *maintenancePageView `json:"local_page,omitempty"`
}

// handleRemoteMaintenance は公開サーバーのメンテナンス表示の状態を返す。問い合わせられなければ 502 と理由を返す。
func (s *Server) handleRemoteMaintenance(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	cfg, err := s.requireRemote(auth.CanSyncSettings, "公開サーバーのメンテナンス表示の確認")
	if err != nil {
		return err
	}
	lang := language(c)
	local := s.localMaintenancePageView(c.Request().Context(), lang)
	st, err := callRemoteMaintenance(c.Request().Context(), cfg, SyncRequest{Action: SyncActionGetMaintenance})
	if err != nil {
		return remoteMaintenanceFail(c, lang, err, false, local)
	}
	return c.JSON(http.StatusOK, buildMaintenanceView(lang, st, local))
}

// handleRemoteMaintenanceSave は公開サーバーのメンテナンス表示を始める（フォームの enabled が "1"）・終える（"0"）。
// 始めるときは、この PC で選んでいる画面をいっしょに送る。公開サーバーが返した状態とともに結果を返す。
func (s *Server) handleRemoteMaintenanceSave(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	cfg, err := s.requireRemote(auth.CanSyncSettings, "公開サーバーのメンテナンス表示の切り替え")
	if err != nil {
		return err
	}
	lang := language(c)
	l := string(lang)
	localPage, localView, err := s.loadLocalMaintenancePageView(c.Request().Context(), lang)
	if err != nil {
		return err
	}
	var want bool
	switch c.FormValue("enabled") {
	case "1":
		want = true
	case "0":
	default:
		return c.JSON(http.StatusUnprocessableEntity, remoteMaintenanceFailure{
			Message: i18n.Translate(l, "remote_maint_invalid"), Status: maintenanceStatusUnknown, LocalPage: localView})
	}
	upd := &SyncMaintenanceUpdate{Enabled: &want}
	if want {
		upd.Page = localPage.toSync()
	}
	out, err := callRemoteMaintenance(c.Request().Context(), cfg,
		SyncRequest{Action: SyncActionSetMaintenance, Maintenance: upd})
	if err != nil {
		return remoteMaintenanceFail(c, lang, err, true, localView)
	}
	view := buildMaintenanceView(lang, out, localView)
	if out.Enabled != want {
		// 公開サーバーは受け付けたが、指定した状態になっていない。返された状態を表示し、切り替えたとは言わない。
		return c.JSON(http.StatusBadGateway, remoteMaintenanceFailure{Message: i18n.Translate(l, "remote_maint_mismatch"),
			Status: maintenanceStatusUnknown, RemoteStatus: http.StatusOK, State: &view, LocalPage: localView})
	}
	if want && out.Changed && localPage.isCustom() && !maintenancePageApplied(localPage, out) {
		return c.JSON(http.StatusBadGateway, remoteMaintenanceFailure{Message: i18n.Translate(l, "remote_maint_page_not_applied"),
			Status: maintenanceStatusUnknown, RemoteStatus: http.StatusOK, State: &view, LocalPage: localView})
	}
	msg := maintenanceSaveMessage(l, want, localPage, out)
	if want && out.Changed && !localPage.isCustom() && localView != nil && localView.Source == maintenanceSourceTheme {
		msg = maintText(l,
			"メンテナンス表示を始めました。公開サイトの訪問者には、テーマ同梱のメンテナンス画面（公開サーバーで適用中のテーマに無ければ標準の画面）を表示しています。CDN（Cloudflare など）を使っている場合、CDN に保存済みのページは表示され続けることがあります。",
			"Maintenance started. Visitors to the public site now see the theme's bundled maintenance page (or the standard page if the theme on the public server has none). If you use a CDN (such as Cloudflare), pages already cached by the CDN may still be shown.")
	}
	return c.JSON(http.StatusOK, map[string]any{"message": msg, "changed": out.Changed, "page_changed": out.PageChanged, "state": view})
}

// handleRemoteMaintenancePage はこの PC のこのサイトに、独自 HTML を保存する（または標準に戻す）。公開サーバーには送らない。
func (s *Server) handleRemoteMaintenancePage(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	if _, err := s.requireRemote(auth.CanSyncSettings, "メンテナンス画面の保存"); err != nil {
		return err
	}
	lang := language(c)
	l := string(lang)
	var page maintenancePage
	var err error
	if c.FormValue("clear") == "1" {
		page = defaultMaintenancePage()
	} else {
		page, err = readUploadedMaintenancePage(c)
		if err != nil {
			code := http.StatusUnprocessableEntity
			var he *echo.HTTPError
			if errors.As(err, &he) && he.Code != 0 {
				code = he.Code
				if msg, ok := he.Message.(string); ok {
					err = errors.New(msg)
				}
			}
			return c.JSON(code, remoteMaintenanceFailure{Message: err.Error(), Status: maintenanceStatusUnknown})
		}
	}
	ctx := c.Request().Context()
	at := clockNow().UTC().Truncate(time.Second)
	s.maintenanceMu.Lock()
	saveErr := saveLocalMaintenancePage(ctx, s.db, page, at)
	if saveErr == nil {
		page.UpdatedAt = &at
	}
	s.maintenanceMu.Unlock()
	if saveErr != nil {
		return saveErr
	}
	view := s.buildLocalMaintenancePageView(lang, page)
	key := "remote_maint_local_saved"
	if !page.isCustom() {
		key = "remote_maint_local_cleared"
	}
	return c.JSON(http.StatusOK, map[string]any{"message": i18n.Translate(l, key), "local_page": view})
}

// handleRemoteMaintenanceApply は、この PC で選んでいる画面を、表示中の公開サーバーへ送る。ON/OFF は変えない。
func (s *Server) handleRemoteMaintenanceApply(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	cfg, err := s.requireRemote(auth.CanSyncSettings, "メンテナンス画面の反映")
	if err != nil {
		return err
	}
	lang := language(c)
	l := string(lang)
	localPage, localView, err := s.loadLocalMaintenancePageView(c.Request().Context(), lang)
	if err != nil {
		return err
	}
	out, err := callRemoteMaintenance(c.Request().Context(), cfg,
		SyncRequest{Action: SyncActionSetMaintenance, Maintenance: &SyncMaintenanceUpdate{Page: localPage.toSync()}})
	if err != nil {
		return remoteMaintenanceApplyFail(c, lang, err, localView)
	}
	view := buildMaintenanceView(lang, out, localView)
	if !out.Enabled {
		return c.JSON(http.StatusBadGateway, remoteMaintenanceFailure{Message: i18n.Translate(l, "remote_maint_page_not_on"),
			Status: maintenanceStatusUnknown, RemoteStatus: http.StatusOK, State: &view, LocalPage: localView})
	}
	if !maintenancePageApplied(localPage, out) {
		return c.JSON(http.StatusBadGateway, remoteMaintenanceFailure{Message: i18n.Translate(l, "remote_maint_page_not_applied"),
			Status: maintenanceStatusUnknown, RemoteStatus: http.StatusOK, State: &view, LocalPage: localView})
	}
	msg := i18n.Translate(l, "remote_maint_page_applied_default")
	switch {
	case localPage.isCustom():
		msg = i18n.Translate(l, "remote_maint_page_applied", localPage.Name)
	case localView != nil && localView.Source == maintenanceSourceTheme:
		msg = maintText(l,
			"公開サーバーの画面を、テーマ同梱の画面（公開サーバーで適用中のテーマに無ければ標準の画面）に更新しました。メンテナンス表示の ON/OFF は変えていません。",
			"Updated the public server's page to the theme's bundled page (or the standard page if the theme on the public server has none). Maintenance on/off was not changed.")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message": msg, "changed": false, "page_changed": out.PageChanged, "state": view})
}

func readUploadedMaintenancePage(c echo.Context) (maintenancePage, error) {
	fh, err := c.FormFile("file")
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return maintenancePage{}, he
		}
		return maintenancePage{}, echo.NewHTTPError(http.StatusBadRequest, i18n.Translate(string(language(c)), "remote_maint_page_choose"))
	}
	if fh.Size > maxMaintenancePageBytes {
		return maintenancePage{}, echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("HTML ファイルが大きすぎます（上限 %s）", formatByteSize(maxMaintenancePageBytes)))
	}
	f, err := fh.Open()
	if err != nil {
		return maintenancePage{}, fmt.Errorf("server: maintenance page upload: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMaintenancePageBytes+1))
	if err != nil {
		return maintenancePage{}, fmt.Errorf("server: maintenance page upload: %w", err)
	}
	page, err := parseCustomMaintenancePage(fh.Filename, data)
	if err != nil {
		code := http.StatusUnprocessableEntity
		if strings.Contains(err.Error(), "大きすぎます") {
			code = http.StatusRequestEntityTooLarge
		}
		return maintenancePage{}, echo.NewHTTPError(code, err.Error())
	}
	return page, nil
}

// callRemoteMaintenance は公開サーバーの get_maintenance・set_maintenance（req）を呼ぶ。応答が同じ action のものでない、
// または必要な page が無いときは、読み取れない応答としてエラーにする。
func callRemoteMaintenance(ctx context.Context, cfg remoteConfig, req SyncRequest) (SyncMaintenanceResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteMaintenanceTimeout)
	defer cancel()
	var out SyncMaintenanceResponse
	if err := callRemote(ctx, cfg, req, &out); err != nil {
		return out, err
	}
	if out.Action != req.Action || out.Page == nil {
		return out, &remoteError{Status: http.StatusOK, Code: "unreadable"}
	}
	return out, nil
}

// remoteMaintenanceFail はメンテナンス表示の確認・切り替えの失敗を、状態が分からないことを表す 502 の応答にする。
// sent（切り替えを送った後の失敗）なら、応答が無い・読めない・公開サーバーのエラー（5xx）のときは反映されたかどうか
// 分からないと伝え、公開サーバーが断ったとき（4xx。古い公開サーバーの未対応の action を含む）は切り替わっていないと伝える。
func remoteMaintenanceFail(c echo.Context, lang i18n.Lang, err error, sent bool, local *maintenancePageView) error {
	var re *remoteError
	if !errors.As(err, &re) {
		return err
	}
	if re.cause != nil {
		c.Logger().Warnf("%v", re)
	}
	l := string(lang)
	fail := remoteMaintenanceFailure{Message: re.Message, Status: maintenanceStatusUnknown, RemoteStatus: re.Status, LocalPage: local}
	switch {
	case re.Status == http.StatusBadRequest && strings.HasPrefix(re.Message, "未対応の action"):
		fail.Message = i18n.Translate(l, "remote_maint_unsupported")
	case re.Code == "unreadable" || re.Status == http.StatusOK:
		fail.Message = i18n.Translate(l, "remote_access_unreadable")
		fail.OutcomeUnknown = sent
	case re.Status == 0 || re.Status >= http.StatusInternalServerError:
		fail.OutcomeUnknown = sent
	}
	switch {
	case fail.OutcomeUnknown:
		fail.Message = strings.TrimSpace(fail.Message + " " + i18n.Translate(l, "remote_maint_outcome_unknown"))
	case sent:
		fail.Message = strings.TrimSpace(fail.Message + " " + i18n.Translate(l, "remote_maint_not_changed"))
	}
	return c.JSON(http.StatusBadGateway, fail)
}

// remoteMaintenanceApplyFail は画面の反映の失敗。ON/OFF は変えない要求なので、4xx は「画面は反映していません」と伝える。
// 未対応の action（v0.2.8）は切り替わっていない。切断・5xx・読めない応答は結果不明（自動ではやり直さない）。
func remoteMaintenanceApplyFail(c echo.Context, lang i18n.Lang, err error, local *maintenancePageView) error {
	var re *remoteError
	if !errors.As(err, &re) {
		return err
	}
	if re.cause != nil {
		c.Logger().Warnf("%v", re)
	}
	l := string(lang)
	fail := remoteMaintenanceFailure{Message: re.Message, Status: maintenanceStatusUnknown, RemoteStatus: re.Status, LocalPage: local}
	switch {
	case re.Code == maintenanceCodePageNotActive || strings.Contains(re.Message, "メンテナンス表示中ではない"):
		fail.Message = i18n.Translate(l, "remote_maint_page_not_on")
	case re.Status == http.StatusBadRequest && strings.HasPrefix(re.Message, "未対応の action"):
		fail.Message = i18n.Translate(l, "remote_maint_unsupported")
	case re.Code == "unreadable" || re.Status == http.StatusOK:
		fail.Message = i18n.Translate(l, "remote_access_unreadable")
		fail.OutcomeUnknown = true
	case re.Status == 0 || re.Status >= http.StatusInternalServerError:
		fail.OutcomeUnknown = true
	}
	switch {
	case fail.OutcomeUnknown:
		fail.Message = strings.TrimSpace(fail.Message + " " + i18n.Translate(l, "remote_maint_page_outcome_unknown"))
	default:
		fail.Message = strings.TrimSpace(fail.Message + " " + i18n.Translate(l, "remote_maint_page_not_changed"))
	}
	return c.JSON(http.StatusBadGateway, fail)
}

func (s *Server) localMaintenancePageView(ctx context.Context, lang i18n.Lang) *maintenancePageView {
	_, v, err := s.loadLocalMaintenancePageView(ctx, lang)
	if err != nil {
		d := defaultMaintenancePage()
		return buildMaintenancePageView(lang, d, d)
	}
	return v
}

func (s *Server) loadLocalMaintenancePageView(ctx context.Context, lang i18n.Lang) (maintenancePage, *maintenancePageView, error) {
	s.maintenanceMu.Lock()
	page, err := loadLocalMaintenancePage(ctx, s.db)
	s.maintenanceMu.Unlock()
	if err != nil {
		return maintenancePage{}, nil, err
	}
	return page, s.buildLocalMaintenancePageView(lang, page), nil
}

// buildLocalMaintenancePageView はこの PC で選んでいる画面 page の表示を作る。独自 HTML を選んでいなければ、適用中の
// テーマに同梱した maintenance.html を使うことを、選んでいれば独自 HTML を優先することを、テーマの名前とともに出す。
func (s *Server) buildLocalMaintenancePageView(lang i18n.Lang, page maintenancePage) *maintenancePageView {
	v := buildMaintenancePageView(lang, page, page)
	l := string(lang)
	v.Source = maintenanceSourceDefault
	if page.isCustom() {
		v.Source = maintenanceSourceCustom
	}
	t, ok := s.maintenanceTheme()
	if !ok {
		return v
	}
	v.ThemeName = t.Label(l)
	if page.isCustom() {
		v.SourceNote = maintText(l,
			fmt.Sprintf("適用中のテーマ「%s」にもメンテナンス画面（maintenance.html）がありますが、ここで選んだ独自の HTML を優先します。", v.ThemeName),
			fmt.Sprintf("The applied theme “%s” also bundles a maintenance page (maintenance.html), but the custom HTML selected here takes priority.", v.ThemeName))
		return v
	}
	v.Source = maintenanceSourceTheme
	v.Label = maintText(l, fmt.Sprintf("テーマ「%s」の画面（maintenance.html）", v.ThemeName),
		fmt.Sprintf("Page bundled with the theme “%s” (maintenance.html)", v.ThemeName))
	v.SourceNote = maintText(l,
		"適用中のテーマにメンテナンス画面が同梱されているため、独自の HTML を選ばなければこの画面を表示します（公開サーバーでも同じテーマを適用しているとき）。",
		"The applied theme bundles a maintenance page, so it is shown unless you select a custom HTML file (when the public server uses the same theme).")
	return v
}

// maintText は表示言語 l に合わせて ja か en を返す（テーマ同梱の画面に関する文言）。
func maintText(l, ja, en string) string {
	if l == string(i18n.EN) {
		return en
	}
	return ja
}

// handleRemoteMaintenancePageDownload は、いまこの PC で有効なメンテナンス画面（① この PC で選んだ独自 HTML、
// ② 適用中のテーマに同梱した maintenance.html、③ 標準画面）を maintenance.html としてダウンロードさせる。
// 手元で編集してから「この PC に保存」で選び直すための入口で、公開サーバーには問い合わせない。表示はさせない
// （attachment・CSP の sandbox）。
func (s *Server) handleRemoteMaintenancePageDownload(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	if _, err := s.requireRemote(auth.CanSyncSettings, "メンテナンス画面のダウンロード"); err != nil {
		return err
	}
	s.maintenanceMu.Lock()
	page, err := loadLocalMaintenancePage(c.Request().Context(), s.db)
	s.maintenanceMu.Unlock()
	if err != nil {
		return err
	}
	body, source := s.resolveMaintenanceHTML(c, page)
	h := c.Response().Header()
	h.Set(echo.HeaderContentDisposition, `attachment; filename="`+theme.MaintenanceFile+`"`)
	h.Set(echo.HeaderXContentTypeOptions, "nosniff")
	h.Set(echo.HeaderContentSecurityPolicy, "sandbox")
	h.Set(maintenanceSourceHeader, source)
	return c.Blob(http.StatusOK, "text/html; charset=utf-8", []byte(body))
}

// maintenanceSourceHeader はダウンロードした画面の出どころ（custom・theme・default）を返す応答ヘッダー。
const maintenanceSourceHeader = "X-Sorus-Maintenance-Source"

func buildMaintenancePageView(lang i18n.Lang, page, local maintenancePage) *maintenancePageView {
	l := string(lang)
	v := &maintenancePageView{Kind: SyncMaintenancePageDefault, Label: i18n.Translate(l, "remote_maint_page_default")}
	if page.isCustom() {
		v.Kind = SyncMaintenancePageCustom
		v.Name = page.Name
		v.SHA256 = page.SHA256
		v.Label = i18n.Translate(l, "remote_maint_page_custom", page.Name)
	}
	v.MatchesLocal = page.sameContent(local)
	if page.UpdatedAt != nil {
		v.UpdatedLabel = i18n.Translate(l, "remote_maint_page_updated_at", page.UpdatedAt.In(jst).Format(maintenanceTimeLayout))
	}
	return v
}

// buildMaintenanceView は公開サーバーが返した状態 st から、画面に出す状態を作る。
func buildMaintenanceView(lang i18n.Lang, st SyncMaintenanceResponse, local *maintenancePageView) remoteMaintenanceView {
	l := string(lang)
	v := remoteMaintenanceView{
		Enabled:      st.Enabled,
		Status:       maintenanceStatusOff,
		Tone:         "neutral",
		CheckedLabel: i18n.Translate(l, "remote_access_checked_at", clockNow().In(jst).Format(maintenanceTimeLayout)),
		LocalPage:    local,
	}
	if st.Enabled {
		// 公開サイトを止めている状態のため、成功の色にはせず注意の色にする。
		v.Status, v.Tone = maintenanceStatusOn, "warn"
	}
	v.StateLabel = i18n.Translate(l, "remote_maint_state_"+v.Status)
	v.Summary = i18n.Translate(l, "remote_maint_summary_"+v.Status)
	if st.ChangedAt != nil {
		v.ChangedLabel = i18n.Translate(l, "remote_maint_changed_at", st.ChangedAt.In(jst).Format(maintenanceTimeLayout))
	}
	if st.Page != nil {
		remote := maintenancePage{Kind: st.Page.Kind, Name: st.Page.Name, SHA256: st.Page.SHA256, UpdatedAt: st.Page.UpdatedAt}
		if st.Page.Kind == SyncMaintenancePageCustom && st.Page.SHA256 != "" {
			remote.HTML = "x"
		}
		localPage := defaultMaintenancePage()
		if local != nil {
			localPage = maintenancePage{Kind: local.Kind, Name: local.Name, SHA256: local.SHA256}
			if local.Kind == SyncMaintenancePageCustom {
				localPage.HTML = "x"
			}
		}
		v.Page = buildMaintenancePageView(lang, remote, localPage)
		if v.Page.Kind == SyncMaintenancePageDefault && local != nil && local.ThemeName != "" {
			// 独自 HTML を送っていない公開サーバーは、適用中のテーマに同梱した画面を出す（公開サーバーのテーマに無ければ
			// 標準画面）。PC で独自 HTML を選んでいても、公開サーバーが表示しているのはテーマの画面。
			v.Page.Label = maintText(l, "テーマ同梱の画面（無ければ標準の画面）", "The theme's bundled page (or the standard page)")
		}
		if !st.Enabled {
			v.Page.Label = i18n.Translate(l, "remote_maint_page_not_showing")
		}
	}
	return v
}

func maintenancePageApplied(local maintenancePage, out SyncMaintenanceResponse) bool {
	if out.Page == nil {
		return false
	}
	remote := maintenancePage{Kind: out.Page.Kind, Name: out.Page.Name, SHA256: out.Page.SHA256}
	if out.Page.Kind == SyncMaintenancePageCustom && out.Page.SHA256 != "" {
		remote.HTML = "x"
	}
	return remote.sameContent(local)
}

func maintenanceSaveMessage(l string, want bool, local maintenancePage, out SyncMaintenanceResponse) string {
	switch {
	case !want && !out.Changed:
		return i18n.Translate(l, "remote_maint_already_off")
	case !want:
		return i18n.Translate(l, "remote_maint_stopped")
	case !out.Changed:
		return i18n.Translate(l, "remote_maint_already_on")
	case local.isCustom():
		return i18n.Translate(l, "remote_maint_started_custom", local.Name)
	default:
		return i18n.Translate(l, "remote_maint_started_default")
	}
}
