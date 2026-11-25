package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/theme"
)

type themeSettingsData struct {
	// Selected は表示に使っているテーマの ID（保存値がカタログに無ければ theme.Default）。ラジオボタンの初期値にする。
	Selected string
	// Applied は保存済みのテーマの ID。保存値がカタログに無ければ空（どのカードにも「適用中」を出さない）。
	Applied string
	Themes  []theme.Theme
	// Missing は保存値がカタログに無いときの、その保存値。Fallback は代わりに表示しているテーマ。
	Missing  string
	Fallback theme.Theme
	// CanPushSettings は公開サーバーを設定済みで、テーマを含むサイト設定を公開サーバーへ反映できるか
	// （連携設定画面の「公開サーバーへ反映する」と同じ条件）。PushConfirm はその確認文。
	CanPushSettings bool
	PushConfirm     string
	// JustSaved は保存した直後の表示か。
	JustSaved bool
	// CardImages は公開カード3種のアイキャッチ画像表示の設定。
	CardImages cardImageSettings
}

// ThemePreviewURL はテーマ id のトップページのプレビュー URL。frame なら設定画面のカードの縮小表示用。
func (themeSettingsData) ThemePreviewURL(id string, frame bool) string {
	return themePreviewURL(id, previewPageHome, frame)
}

func (s *Server) handleThemeSettings(c echo.Context) error {
	stored := s.getSetting("theme", theme.Default)
	selected := s.themeCatalog().Resolve(stored)
	remote := s.remoteSettingsData(c, s.loadRemoteConfig())
	d := themeSettingsData{
		Selected:        selected.ID,
		Themes:          s.themeCatalog().All(),
		Fallback:        selected,
		CanPushSettings: remote.CanPushSettings,
		PushConfirm:     remote.PushConfirm,
		JustSaved:       c.QueryParam("flash") == "theme_saved" || c.QueryParam("flash") == "card_images_saved",
		CardImages:      s.cardImageSettings(),
	}
	if t, ok := s.themeCatalog().Get(stored); ok {
		d.Applied = t.ID
	} else {
		d.Missing = stored
	}
	// カスタムテーマの取り込み・削除の結果（handlers_theme_custom.go）は、保存の通知と同じトーストで出す。
	if msg, isErr := s.themeFlashMessage(c); msg != "" {
		return s.renderPage(c, http.StatusOK, "settings_theme", localizedMessage(c, "theme_title"), d, msg, isErr)
	}
	return s.render(c, http.StatusOK, "settings_theme", localizedMessage(c, "theme_title"), d)
}

// handleThemeSettingsSave は公開テーマを保存する。カタログに無いテーマは 422 で拒否する。
func (s *Server) handleThemeSettingsSave(c echo.Context) error {
	// カタログの確認から保存までを themeMu で排他し、確認した直後にそのテーマが削除されるのを防ぐ。
	s.themeMu.Lock()
	defer s.themeMu.Unlock()
	selected, ok := s.themeCatalog().Get(c.FormValue("theme"))
	if !ok {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid theme")
	}
	if _, err := s.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('theme', ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, selected.ID); err != nil {
		return err
	}
	s.markRemoteDirty(c, remoteDirtyTheme)
	// 切り替えたテーマを静的サイトにも反映する（記事の公開と同じ自動反映の設定に従う）。
	s.reflectAfterPublish()
	return c.Redirect(http.StatusSeeOther, "/admin/settings/theme?flash=theme_saved")
}

// settingShowAuthor は「記事や一覧に執筆者名を表示する」の settings のキー。値は "1" / "0"
// （読み込みでは "true" も表示とみなす）。サイト基本設定の保存と、同期 API の sync_settings で書き込む。
const settingShowAuthor = "show_author"

// formatShowAuthor は「記事や一覧に執筆者名を表示する」を settings に保存する値にする。
func formatShowAuthor(show bool) string {
	if show {
		return "1"
	}
	return "0"
}

// 公開カード3種のアイキャッチ画像を表示するかの settings のキー。値は "true" / "false"。
const (
	settingShowFrontCardImages   = "show_front_card_images"
	settingShowArticleCardImages = "show_article_card_images"
	settingShowRelatedCardImages = "show_related_card_images"
)

// cardImageSettings は公開カード3種（トップのページカード・記事一覧のカード・関連記事のカード）で
// アイキャッチ画像を表示するか。
type cardImageSettings struct {
	Front   bool
	Article bool
	Related bool
}

// defaultCardImageSettings は未設定のときの値。トップのページカードは画像をそろえにくいため文字だけのカードにし、
// 記事一覧・関連記事は画像を出す。
func defaultCardImageSettings() cardImageSettings {
	return cardImageSettings{Front: false, Article: true, Related: true}
}

// parseSettingBool は settings の真偽値（"true" / "false"、旧形式の "1" / "0"）を読む。空や不明な値は def。
func parseSettingBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on":
		return true
	case "false", "0", "off":
		return false
	}
	return def
}

func formatSettingBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// cardImageSettings は保存済みの公開カード3種の画像表示の設定を読む。未設定の項目は既定値。
func (s *Server) cardImageSettings() cardImageSettings {
	cfg := defaultCardImageSettings()
	var rows []struct{ Key, Value string }
	if err := s.db.Select(&rows, `SELECT key, value FROM settings WHERE key IN (?, ?, ?)`,
		settingShowFrontCardImages, settingShowArticleCardImages, settingShowRelatedCardImages); err != nil {
		return cfg
	}
	for _, r := range rows {
		switch r.Key {
		case settingShowFrontCardImages:
			cfg.Front = parseSettingBool(r.Value, cfg.Front)
		case settingShowArticleCardImages:
			cfg.Article = parseSettingBool(r.Value, cfg.Article)
		case settingShowRelatedCardImages:
			cfg.Related = parseSettingBool(r.Value, cfg.Related)
		}
	}
	return cfg
}

// saveCardImageSettings は公開カード3種の画像表示の設定を3つまとめて保存する。
func (s *Server) saveCardImageSettings(cfg cardImageSettings) error {
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for key, value := range map[string]bool{
		settingShowFrontCardImages:   cfg.Front,
		settingShowArticleCardImages: cfg.Article,
		settingShowRelatedCardImages: cfg.Related,
	} {
		if _, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, key, formatSettingBool(value)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// handleCardImageSettingsSave は POST /admin/settings/theme/card-images で、テーマ設定画面の
// 「公開カードのアイキャッチ画像表示」（3つのチェックボックス）を保存する。チェックの無い項目は「表示しない」。
func (s *Server) handleCardImageSettingsSave(c echo.Context) error {
	cfg := cardImageSettings{
		Front:   c.FormValue(settingShowFrontCardImages) == "1",
		Article: c.FormValue(settingShowArticleCardImages) == "1",
		Related: c.FormValue(settingShowRelatedCardImages) == "1",
	}
	if err := s.saveCardImageSettings(cfg); err != nil {
		return err
	}
	s.markRemoteDirty(c, remoteDirtyTheme)
	// 静的サイトにも反映する（テーマの適用と同じ自動反映の設定に従う）。
	s.reflectAfterPublish()
	return c.Redirect(http.StatusSeeOther, "/admin/settings/theme?flash=card_images_saved#card-images")
}

const (
	// DefaultFaviconURL はカスタム Favicon を設定していないときの Favicon。
	DefaultFaviconURL = "/static/icon.svg"
	// MaxFaviconSize はアップロードできる Favicon の上限バイト数（2MB）。
	MaxFaviconSize = 2 << 20
	// faviconFilePrefix はアップロードした Favicon の保存ファイル名の接頭辞。
	faviconFilePrefix = "favicon-"
)

// faviconExts は受け付ける Favicon の MIME タイプと保存時の拡張子。
var faviconExts = map[string]string{
	"image/svg+xml": ".svg",
	"image/png":     ".png",
	"image/x-icon":  ".ico",
	"image/webp":    ".webp",
}

// faviconMIMETypes は Favicon の URL の拡張子から <link rel="icon"> の type 属性を決める。
var faviconMIMETypes = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
	".webp": "image/webp",
}

var (
	errFaviconTooLarge   = errors.New("Favicon のファイルサイズが上限（2MB）を超えています")
	errFaviconBadType    = errors.New("Favicon には SVG・PNG・ICO・WebP 形式の画像を選択してください")
	errFaviconUnsafeSVG  = errors.New("スクリプトやリンクなど安全でない内容を含む SVG は Favicon に設定できません")
	errFaviconInvalidSVG = errors.New("SVG の内容を読み取れませんでした。正しい形式の SVG 画像を選択してください")
)

// svgForbiddenElements は Favicon の SVG に含めてはいけない要素（小文字）。スクリプトの実行、リンクによる遷移、
// アニメーションによる属性の書き換え、外部リソースや別文書の読み込みができる要素を拒否する。
// <style> は CSS の url()・@import・エスケープ（\6a avascript: など）で外部 URL を読み込みうるため、要素ごと拒否する
// （style 属性は safeSVGAttr で拒否する）。
var svgForbiddenElements = map[string]bool{
	"script":           true,
	"style":            true,
	"a":                true,
	"animate":          true,
	"animatemotion":    true,
	"animatetransform": true,
	"set":              true,
	"discard":          true,
	"use":              true,
	"image":            true,
	"foreignobject":    true,
	"handler":          true,
	"listener":         true,
	"iframe":           true,
	"embed":            true,
	"object":           true,
}

type generalSettingsData struct {
	SiteName        string
	SiteNameEn      string
	SiteDescription string
	BaseURL         string
	Copyright       string
	AnalyticsTag    string
	// AnalyticsDashboardURL は外部の詳細なアクセス解析の画面（Google Analytics・Cloudflare Web Analytics など）の URL。
	// ダッシュボードに「詳細なアクセス解析を開く」として出す。
	AnalyticsDashboardURL string
	// FaviconURL は現在の Favicon（未設定なら DefaultFaviconURL）。FaviconCustom はカスタム Favicon を設定済みか。
	FaviconURL    string
	FaviconCustom bool
	// ShowAuthor は記事や一覧に執筆者名を表示するか。
	ShowAuthor bool
	// BasicAuth は「サイト設定」で閲覧制限（Basic 認証）を扱うか（デスクトップモードでないか。basicAuthApplies）。
	// false なら閲覧制限の項目を出さない。
	BasicAuth bool
	// BasicAuthEnabled・BasicAuthUser はサイト閲覧制限（Basic 認証）の有効・無効と閲覧用のユーザー名。
	// BasicAuthPassSet は閲覧用のパスワードを設定済みか（パスワードそのものは画面に出さない）。
	BasicAuthEnabled bool
	BasicAuthUser    string
	BasicAuthPassSet bool
	// BasicAuthRevision は画面に出した閲覧制限の設定の版。保存のときに送り返し、画面を開いた後にほかで（同期 API の
	// set_site_access など）変わっていれば、古い画面の内容で上書きしない。
	BasicAuthRevision string
	// MultilingualEnabled は公開コンテンツの多言語対応（英語ページ・翻訳機能）を有効にしているか（isMultilingualEnabled）。
	MultilingualEnabled bool
}

// settingMultilingualEnabled は多言語対応の ON/OFF を保存する settings のキー。値は "1"（ON）/ "0"（OFF）。
const settingMultilingualEnabled = "multilingual_enabled"

// isMultilingualEnabled は公開コンテンツの多言語対応（/en 以下の英語ページ・言語切り替え・hreflang・管理画面の翻訳機能）を
// 有効にしているかを返す。基本設定で保存した値（multilingual_enabled）があればそれに従う。保存していなければ、英語の記事・
// ページ（lang = 'en'。公開中・下書きなどを含み、ゴミ箱は除く）が1件でもあるサイトは今までどおり ON、無ければ OFF とする
// （新しいサイトは OFF から始まり、英語ページを作っていた既存のサイトはそのまま動く）。
func (s *Server) isMultilingualEnabled() bool {
	var v string
	if err := s.db.Get(&v, `SELECT value FROM settings WHERE key = ?`, settingMultilingualEnabled); err == nil {
		return parseSettingBool(v, false)
	}
	var exists bool
	if err := s.db.Get(&exists, `SELECT EXISTS(SELECT 1 FROM articles WHERE lang = 'en' AND status <> 'deleted')`); err != nil {
		return false
	}
	return exists
}

// requireMultilingual は多言語対応が OFF のとき、英語のページ（/en 以下）を存在しないページと同じ 404 にする。
func (s *Server) requireMultilingual(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !s.isMultilingualEnabled() {
			return echo.ErrNotFound
		}
		return next(c)
	}
}

// formHas はフォーム（クエリを含む）に name の欄が送られてきたかを返す。画面で隠した欄（多言語対応が OFF のときの
// 英語のサイト名など）を、空欄で上書きしないために使う。
func formHas(c echo.Context, name string) bool {
	form, err := c.FormParams()
	if err != nil {
		return false
	}
	_, ok := form[name]
	return ok
}

// formChecked はチェックボックス name が "1" で送られてきたかを返す。未チェックを表す hidden の "0" を前に置く
// フォームでも、チェックされていれば ON とみなす。
func formChecked(c echo.Context, name string) bool {
	form, err := c.FormParams()
	if err != nil {
		return c.FormValue(name) == "1"
	}
	for _, v := range form[name] {
		if v == "1" {
			return true
		}
	}
	return false
}

func (s *Server) handleGeneralSettings(c echo.Context) error {
	return s.render(c, http.StatusOK, "settings_general", pageTitle(c, "general_settings_title"), s.generalSettings())
}

// generalSettings は保存済みのサイト基本設定を返す。閲覧制限は版と食い違わないよう、1回の問い合わせで読む
// （読めなければ未設定として出す。その画面から保存しても、保存済みの版と違うため上書きしない）。デスクトップモードでは
// 閲覧制限を扱わないため読まない。
func (s *Server) generalSettings() generalSettingsData {
	favicon, _ := s.favicon()
	manageBasicAuth := s.basicAuthApplies()
	var basicAuth basicAuthState
	if manageBasicAuth {
		var err error
		if basicAuth, err = loadBasicAuthState(context.Background(), s.db); err != nil {
			s.echo.Logger.Warnf("server: general settings: load basic auth: %v", err)
			basicAuth = basicAuthState{}
		}
	}
	return generalSettingsData{
		SiteName:              s.getSetting("site_name", s.getSiteName()),
		SiteNameEn:            s.getSetting("site_name_en", s.getSiteNameEn()),
		SiteDescription:       s.getSetting("site_description", s.getSiteDescription()),
		BaseURL:               s.getSetting("base_url", s.getBaseURL()),
		Copyright:             s.getSetting("copyright", ""),
		AnalyticsTag:          s.getSetting("analytics_tag", ""),
		AnalyticsDashboardURL: s.analyticsDashboardURL(),
		FaviconURL:            favicon,
		FaviconCustom:         favicon != DefaultFaviconURL,
		ShowAuthor:            s.getShowAuthor(),
		BasicAuth:             manageBasicAuth,
		BasicAuthEnabled:      basicAuth.Enabled,
		BasicAuthUser:         basicAuth.User,
		BasicAuthPassSet:      basicAuth.PassHash != "",
		BasicAuthRevision:     basicAuth.Revision,
		MultilingualEnabled:   s.isMultilingualEnabled(),
	}
}

// errAnalyticsDashboardURL は外部解析ダッシュボードの URL が http(s) の URL でないときのエラー。
var errAnalyticsDashboardURL = errors.New("詳細なアクセス解析の URL は https:// で始まる URL を入力してください")

// generalSettingsErrorKeys は、サイト基本設定の保存で画面に出すエラーと、その文言の辞書（internal/i18n）のキー。
var generalSettingsErrorKeys = map[error]string{
	errFaviconTooLarge:       "favicon_err_too_large",
	errFaviconBadType:        "favicon_err_bad_type",
	errFaviconUnsafeSVG:      "favicon_err_unsafe_svg",
	errFaviconInvalidSVG:     "favicon_err_invalid_svg",
	errAnalyticsDashboardURL: "settings_err_analytics_url",
}

// generalSettingsErrorMessage は err の文言を表示言語で返す。辞書のキーが無いエラーはそのままの文言を返す。
func generalSettingsErrorMessage(c echo.Context, err error) string {
	for e, key := range generalSettingsErrorKeys {
		if errors.Is(err, e) {
			return localizedMessage(c, key)
		}
	}
	return err.Error()
}

// normalizeAnalyticsDashboardURL は外部解析ダッシュボードの URL を検証して返す。空欄は未設定（空文字）。
// リンクとして開くため、http・https 以外（javascript: など）とホスト名の無い URL は受け付けない。
func normalizeAnalyticsDashboardURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errAnalyticsDashboardURL
	}
	return u.String(), nil
}

// analyticsDashboardURL は保存済みの外部解析ダッシュボードの URL（未設定・不正なら空文字）。
func (s *Server) analyticsDashboardURL() string {
	u, err := normalizeAnalyticsDashboardURL(s.getSetting("analytics_dashboard_url", ""))
	if err != nil {
		return ""
	}
	return u
}

// handleGeneralSettingsSave はサイト基本設定を保存する。フォームは multipart/form-data で、favicon フィールドの
// 画像をカスタム Favicon として保存し、favicon_reset=1 なら既定の Favicon に戻す。
//
// 閲覧制限（Basic 認証）はデスクトップモードでは扱わない（basicAuthApplies）。v0.2.8 の古い画面から basic_auth_* が
// 送られてきても読まず、閲覧制限の settings のキーを読み書きせず、版の食い違いや入力の誤りで保存を止めない。
func (s *Server) handleGeneralSettingsSave(c echo.Context) error {
	siteName := strings.TrimSpace(c.FormValue("site_name"))
	if siteName == "" {
		siteName = DefaultSiteName
	}
	siteNameEn := strings.TrimSpace(c.FormValue("site_name_en"))
	// 多言語対応が OFF の画面では「サイト名（英語）」の欄を出さない。欄が送られてこなければ保存済みの値を残す。
	if !formHas(c, "site_name_en") {
		siteNameEn = s.getSetting("site_name_en", s.getSiteNameEn())
	}
	multilingual := formChecked(c, settingMultilingualEnabled)
	// AI 機能の ON/OFF（ai_enabled）は「AI・外部連携」（handlers_ai_integrations.go）だけで変える。この PC と公開サーバーの
	// 両方に効かせ、外部の AI アプリの連携も解除できるのはそちらだけのため、ここでは送られてきても（古い画面から）読まない。
	siteDescription := strings.TrimSpace(c.FormValue("site_description"))
	baseURL := strings.TrimRight(strings.TrimSpace(c.FormValue("base_url")), "/")
	copyright := strings.TrimSpace(c.FormValue("copyright"))
	analyticsTag := strings.TrimSpace(c.FormValue("analytics_tag"))
	rawDashboardURL := strings.TrimSpace(c.FormValue("analytics_dashboard_url"))
	// サイト閲覧制限（Basic 認証）。パスワードは空欄なら保存済みのものを使い続ける（画面には出さないため）。
	// デスクトップモードでは扱わない（項目を読まない）。
	manageBasicAuth := s.basicAuthApplies()
	var basicAuthEnabled bool
	var basicAuthUser, basicAuthPass, basicAuthRevision string
	if manageBasicAuth {
		basicAuthEnabled = c.FormValue("basic_auth_enabled") == "1"
		basicAuthUser = strings.TrimSpace(c.FormValue("basic_auth_user"))
		basicAuthPass = c.FormValue("basic_auth_pass")
		basicAuthRevision = c.FormValue("basic_auth_revision")
	}
	showAuthor := c.FormValue("show_author") == "1"
	// withInput は保存済みの設定 data に、閲覧制限以外の入力内容を重ねる。
	withInput := func(data generalSettingsData) generalSettingsData {
		data.SiteName, data.SiteNameEn, data.SiteDescription, data.BaseURL = siteName, siteNameEn, siteDescription, baseURL
		data.Copyright, data.AnalyticsTag, data.AnalyticsDashboardURL = copyright, analyticsTag, rawDashboardURL
		data.ShowAuthor = showAuthor
		data.MultilingualEnabled = multilingual
		return data
	}
	// rerender は入力内容を保ったまま、何も保存せずにフォームを表示し直す（パスワードは表示し直さない）。閲覧制限の
	// 版は送られてきたものを返し、古い画面の内容が、表示し直したときの新しい版で通ってしまわないようにする。
	rerender := func(msg string) error {
		data := withInput(s.generalSettings())
		data.BasicAuthEnabled, data.BasicAuthUser, data.BasicAuthRevision = basicAuthEnabled, basicAuthUser, basicAuthRevision
		return s.renderError(c, http.StatusUnprocessableEntity, "settings_general", pageTitle(c, "general_settings_title"), msg, data)
	}
	if utf8.RuneCountInString(siteName) > 100 || hasControl(siteName) {
		return rerender(localizedMessage(c, "settings_err_site_name"))
	}
	if utf8.RuneCountInString(siteNameEn) > 100 || hasControl(siteNameEn) {
		return rerender(localizedMessage(c, "settings_err_site_name_en"))
	}
	if baseURL != "" {
		valid, err := normalizeSiteURL(baseURL)
		if err != nil {
			return rerender(localizedMessage(c, "settings_err_base_url"))
		}
		baseURL = valid
	}
	dashboardURL, err := normalizeAnalyticsDashboardURL(rawDashboardURL)
	if err != nil {
		return rerender(generalSettingsErrorMessage(c, err))
	}
	var basicAuth basicAuthSettings
	if manageBasicAuth {
		basicAuth = basicAuthSettings{Enabled: basicAuthEnabled, User: basicAuthUser,
			PassHash: s.getSetting(basicAuthPassHashSetting, "")}
		if err := validateBasicAuthInput(basicAuthEnabled, basicAuthUser, basicAuthPass, basicAuth.PassHash != ""); err != nil {
			return rerender(localizedMessage(c, err.Error()))
		}
		if basicAuthPass != "" {
			if basicAuth.PassHash, err = auth.HashPassword(basicAuthPass); err != nil {
				return err
			}
		}
	}
	showAuthorFlag := formatShowAuthor(showAuthor)
	multilingualFlag := "0"
	if multilingual {
		multilingualFlag = "1"
	}

	settings := map[string]string{
		"site_name":                siteName,
		"site_name_en":             siteNameEn,
		"site_description":         siteDescription,
		"base_url":                 baseURL,
		"copyright":                copyright,
		"analytics_tag":            analyticsTag,
		"analytics_dashboard_url":  dashboardURL,
		settingShowAuthor:          showAuthorFlag,
		settingMultilingualEnabled: multilingualFlag,
	}

	oldFavicon := s.getSetting("favicon_url", "")
	// newFavicon は保存後の favicon_url。空文字は既定の Favicon に戻すことを表す。
	newFavicon, newFile := oldFavicon, ""
	if c.FormValue("favicon_reset") == "1" {
		newFavicon = ""
	} else if fh, err := c.FormFile("favicon"); err == nil {
		u, err := s.saveFavicon(fh)
		if err != nil {
			if errors.Is(err, errFaviconTooLarge) || errors.Is(err, errFaviconBadType) ||
				errors.Is(err, errFaviconUnsafeSVG) || errors.Is(err, errFaviconInvalidSVG) {
				return rerender(generalSettingsErrorMessage(c, err))
			}
			return err
		}
		newFavicon = u
		if u != oldFavicon {
			newFile = u
		}
	} else if !errors.Is(err, http.ErrMissingFile) && !errors.Is(err, http.ErrNotMultipart) {
		return echo.NewHTTPError(http.StatusBadRequest, localizedMessage(c, "favicon_err_upload"))
	}

	// 公開サーバーへ送るサイト基本設定が変わったかを、保存の前後で比べる（閲覧制限だけの変更は未反映にしない）。
	pushedBefore := s.remoteGeneralSettings()

	// 閲覧制限は同期 API（set_site_access）からも変わりうるため、書き込みからメモリへの反映までを basicAuthMu で排他する。
	basicAuthLocked := false
	if manageBasicAuth {
		s.basicAuthMu.Lock()
		basicAuthLocked = true
	}
	unlockBasicAuth := func() {
		if basicAuthLocked {
			basicAuthLocked = false
			s.basicAuthMu.Unlock()
		}
	}
	defer unlockBasicAuth()
	tx, err := s.db.Beginx()
	if err != nil {
		s.removeFavicon(newFile)
		return err
	}
	defer tx.Rollback()

	for k, v := range settings {
		if _, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, k, v); err != nil {
			s.removeFavicon(newFile)
			return err
		}
	}
	if newFavicon == "" {
		_, err = tx.Exec(`DELETE FROM settings WHERE key = 'favicon_url'`)
	} else {
		_, err = tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('favicon_url', ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, newFavicon)
	}
	if err != nil {
		s.removeFavicon(newFile)
		return err
	}
	var savedAuth basicAuthState
	if manageBasicAuth {
		// 閲覧制限は、同じトランザクションで読み直した保存済みの設定に重ねて保存する。パスワード欄が空なら、
		// 画面を開いた後に変わったパスワードも含めて保存済みのものを使い続け、変わった場合だけ版を作り直す。
		ctx := c.Request().Context()
		curAuth, err := loadBasicAuthState(ctx, tx)
		if err != nil {
			s.removeFavicon(newFile)
			return err
		}
		// 画面を開いた後に閲覧制限がほかで（同期 API の set_site_access、別の画面など）変わり、画面の内容が今の設定と
		// 違うなら、古い画面の有効・無効とユーザー名で上書きせず、何も保存しない（今の設定と同じ内容なら上書きにならない）。
		if basicAuthRevision != curAuth.Revision &&
			(basicAuthPass != "" || basicAuthEnabled != curAuth.Enabled || basicAuthUser != curAuth.User) {
			_ = tx.Rollback()
			unlockBasicAuth()
			s.removeFavicon(newFile)
			return s.renderError(c, http.StatusConflict, "settings_general", pageTitle(c, "general_settings_title"),
				localizedMessage(c, "basic_auth_conflict"), withInput(s.generalSettings()))
		}
		if basicAuthPass == "" {
			basicAuth.PassHash = curAuth.PassHash
		}
		if savedAuth, _, err = saveBasicAuthTx(ctx, tx, curAuth, basicAuth); err != nil {
			s.removeFavicon(newFile)
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		s.removeFavicon(newFile)
		return err
	}
	// 閲覧制限は次の要求から新しい設定で判定する。
	if manageBasicAuth {
		s.setBasicAuth(savedAuth.basicAuthSettings)
	}
	unlockBasicAuth()
	if oldFavicon != newFavicon {
		s.removeFavicon(oldFavicon)
	}

	// サーバーメモリ上の設定値も更新
	s.setSiteInfo(siteName, siteDescription)
	s.setSiteNameEn(siteNameEn)
	s.setShowAuthor(showAuthor)
	s.setBaseURL(baseURL)
	if s.remoteGeneralSettings() != pushedBefore {
		s.markRemoteDirty(c, remoteDirtyGeneral)
	}

	flash := "settings_saved"
	switch s.reflectAfterPublish() {
	case flashPublishedManual:
		flash = "settings_saved_manual"
	case flashPublishedReflecting:
		flash = "settings_saved_reflecting"
	}
	return c.Redirect(http.StatusSeeOther, "/admin/settings/general?flash="+flash)
}

// remoteGeneralSettingsValues は「公開サーバーへ反映する」（handleRemoteSettingsPush）で送るサイト基本設定の値。
type remoteGeneralSettingsValues struct {
	SiteName, SiteDescription, Copyright, AnalyticsTag string
	// BaseURL は公開サーバーへ送る公開サイトの URL。未設定またはこの PC 内（localhost）を指すなら送らないため空。
	BaseURL string
	// MultilingualEnabled は公開サーバーへ送る公開サイトの多言語対応の ON/OFF（isMultilingualEnabled）。
	MultilingualEnabled bool
	// ShowAuthor は公開サーバーへ送る「記事や一覧に執筆者名を表示する」（getShowAuthor）。
	ShowAuthor bool
	// AIEnabled は公開サーバーへ送る AI 機能の ON/OFF（aiEnabled。環境変数の強制 OFF を含めたこの PC の実際の状態）。
	AIEnabled bool
}

// remoteGeneralSettings は公開サーバーへ送るサイト基本設定の値を、handleRemoteSettingsPush と同じ規則で読む。
// サイト基本設定の保存で、公開サーバーへ未反映になる変更があったか（閲覧制限・Favicon など送らない設定だけの
// 変更ではないか）を確かめるのに使う。
func (s *Server) remoteGeneralSettings() remoteGeneralSettingsValues {
	v := remoteGeneralSettingsValues{
		SiteName:            s.getSetting("site_name", s.getSiteName()),
		SiteDescription:     s.getSetting("site_description", s.getSiteDescription()),
		Copyright:           s.getSetting("copyright", ""),
		AnalyticsTag:        s.getSetting("analytics_tag", ""),
		MultilingualEnabled: s.isMultilingualEnabled(),
		ShowAuthor:          s.getShowAuthor(),
		AIEnabled:           s.aiEnabled(),
	}
	if base := s.getSetting("base_url", s.getBaseURL()); base != "" {
		if u, err := url.Parse(base); err == nil && !isLoopbackHost(u.Hostname()) {
			v.BaseURL = base
		}
	}
	return v
}

func (s *Server) getSetting(key, fallback string) string {
	var val string
	if err := s.db.Get(&val, `SELECT value FROM settings WHERE key = ?`, key); err == nil && val != "" {
		return val
	}
	return fallback
}

func (s *Server) getAnalyticsTag() template.HTML {
	var tag string
	_ = s.db.Get(&tag, `SELECT value FROM settings WHERE key = 'analytics_tag'`)
	return template.HTML(tag)
}

func (s *Server) getCopyright() string {
	var cp string
	_ = s.db.Get(&cp, `SELECT value FROM settings WHERE key = 'copyright'`)
	return cp
}

// favicon は現在の Favicon の URL と MIME タイプを返す。未設定なら DefaultFaviconURL を返す。
func (s *Server) favicon() (url, mimeType string) {
	url = s.getSetting("favicon_url", DefaultFaviconURL)
	return url, faviconMIMETypes[strings.ToLower(path.Ext(url))]
}

// saveFavicon はアップロードされた Favicon を検査して UploadDir に保存し、配信 URL を返す。
// 形式は申告された Content-Type や拡張子ではなく内容から判定する。ファイル名は内容のハッシュから作るため、
// 同じ画像を再びアップロードしても同じ URL になり、内容が変われば URL も変わる（不変キャッシュのまま更新できる）。
func (s *Server) saveFavicon(fh *multipart.FileHeader) (string, error) {
	if fh.Size > MaxFaviconSize {
		return "", errFaviconTooLarge
	}
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFaviconSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxFaviconSize {
		return "", errFaviconTooLarge
	}
	mimeType, err := detectFaviconType(data)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)
	name := faviconFilePrefix + hex.EncodeToString(sum[:8]) + faviconExts[mimeType]
	dest := filepath.Join(s.cfg.UploadDir, name)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", fmt.Errorf("server: save favicon %q: %w", fh.Filename, err)
	}
	return UploadURLPrefix + name, nil
}

// detectFaviconType は data が Favicon として受け付ける画像か判定し、MIME タイプを返す。
func detectFaviconType(data []byte) (string, error) {
	if len(data) == 0 {
		return "", errFaviconBadType
	}
	sniffed := http.DetectContentType(data)
	if _, ok := faviconExts[sniffed]; ok && sniffed != "image/svg+xml" {
		return sniffed, nil
	}
	// SVG はテキストとして判定されるため、<svg 要素を含むかで見分ける。
	if !strings.HasPrefix(sniffed, "text/") {
		return "", errFaviconBadType
	}
	if !bytes.Contains(bytes.ToLower(data), []byte("<svg")) {
		return "", errFaviconBadType
	}
	if err := validateFaviconSVG(data); err != nil {
		return "", err
	}
	return "image/svg+xml", nil
}

// validateFaviconSVG は data を XML として解析し、/uploads/ から直接開かれたときに同じオリジンで
// スクリプトが動いたり別のページへ遷移したりしない SVG か確かめる。文字参照は解析器が展開した後の値で判定するため、
// &#106;avascript: のような符号化では検査をすり抜けられない。DOCTYPE（DTD・実体宣言）は一切受け付けない。
func validateFaviconSVG(data []byte) error {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	root := true
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errFaviconInvalidSVG
		}
		switch t := tok.(type) {
		case xml.Directive:
			// <!DOCTYPE ...> や <!ENTITY ...>。実体で値を組み立てて検査を回避されないよう拒否する。
			return errFaviconUnsafeSVG
		case xml.ProcInst:
			// XML 宣言以外の処理命令（<?xml-stylesheet ...?> など）は外部リソースを読み込みうる。
			if t.Target != "xml" {
				return errFaviconUnsafeSVG
			}
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if root {
				if name != "svg" {
					return errFaviconBadType
				}
				root = false
			}
			if svgForbiddenElements[name] {
				return errFaviconUnsafeSVG
			}
			for _, a := range t.Attr {
				if !safeSVGAttr(a) {
					return errFaviconUnsafeSVG
				}
			}
		case xml.CharData:
			// <style> 内の url(javascript:...) など。
			if containsScriptURL(string(t)) {
				return errFaviconUnsafeSVG
			}
		}
	}
	if root {
		return errFaviconBadType
	}
	return nil
}

// safeSVGAttr は SVG の属性 a が安全か返す。イベントハンドラ（on*）と style 属性（CSS の url()・エスケープで
// 外部 URL を読み込みうる）を拒否し、href・xlink:href は同じ文書内の参照（#id）だけを許す（javascript: や data: などの
// URL や外部ファイルへの参照を拒否する）。
func safeSVGAttr(a xml.Attr) bool {
	name := strings.ToLower(a.Name.Local)
	if strings.HasPrefix(name, "on") || name == "style" {
		return false
	}
	// fill・filter・mask などのプレゼンテーション属性は CSS として解釈されるため、エスケープ（u\72l( など）を拒否し、
	// url() は同じ文書内の参照（url(#id)）だけを許す。
	if strings.ContainsRune(a.Value, '\\') || !onlyLocalURLRefs(a.Value) {
		return false
	}
	if name == "href" {
		v := strings.TrimSpace(a.Value)
		if v != "" && !strings.HasPrefix(v, "#") {
			return false
		}
	}
	return !containsScriptURL(a.Value)
}

// onlyLocalURLRefs は s に含まれる url(...) がすべて同じ文書内の参照（#id）か返す。空白・制御文字は読み飛ばし、
// 大文字・小文字を区別せずに判定する。
func onlyLocalURLRefs(s string) bool {
	s = strings.ToLower(strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s))
	for {
		i := strings.Index(s, "url(")
		if i < 0 {
			return true
		}
		s = strings.TrimLeft(s[i+len("url("):], `"'`)
		if !strings.HasPrefix(s, "#") {
			return false
		}
	}
}

// containsScriptURL は s がスクリプトを実行する URL スキームを含むか返す。ブラウザは URL 中の空白や制御文字を
// 読み飛ばすため、それらを取り除き小文字にしてから判定する。
func containsScriptURL(s string) bool {
	s = strings.ToLower(strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s))
	return strings.Contains(s, "javascript:") || strings.Contains(s, "vbscript:")
}

// removeFavicon は u がアップロードした Favicon（/uploads/favicon-*）なら、そのファイルを削除する。
// 既定の Favicon や空文字は何もしない。
func (s *Server) removeFavicon(u string) {
	name, ok := strings.CutPrefix(u, UploadURLPrefix)
	if !ok || !strings.HasPrefix(name, faviconFilePrefix) || name != filepath.Base(name) {
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.UploadDir, name))
}
