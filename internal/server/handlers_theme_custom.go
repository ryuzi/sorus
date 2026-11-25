package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/theme"
	"sorus/static"
)

const (
	// CustomThemeURLPrefix はカスタムテーマのファイル（theme.css と、CSS から相対 url() で読む画像・フォント）を
	// 配信する URL パス。テーマのディレクトリ（theme.Theme.Dir）から配信し、静的エクスポートでも同じパスに書き出す。
	CustomThemeURLPrefix = "/custom-themes/"
	// customThemeCacheControl はカスタムテーマの theme.css の Cache-Control。取り込み直すと中身が変わるため、
	// /static/ のような不変キャッシュにはしない（theme.css の URL には版を付ける: themeStylesheetURL）。
	customThemeCacheControl = "public, max-age=3600"
	// customThemeAssetCacheControl は theme.css 以外のカスタムテーマのファイル（CSS から相対 url() で読む画像・
	// フォント）の Cache-Control。URL に版を付けられないため、キャッシュしても使う前に毎回確かめさせ（更新時刻と
	// ETag で、変わっていなければ 304）、同じ ID のテーマを取り込み直す・公開サーバーへ反映し直したときに、
	// 新しい theme.css と古い画像が混ざらないようにする。
	customThemeAssetCacheControl = "public, no-cache"
	// customThemeAssetCSP はカスタムテーマのファイルを直接開いたときの CSP。SVG などを管理画面と同じオリジンで
	// 文書として開かれても、スクリプトを動かさない（スタイルシート・画像として読み込むときには効かない）。
	customThemeAssetCSP = "default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; sandbox"
	// themeInstallTimeout は URL からテーマを取り込むときの、ダウンロードを含む上限時間。応答の書き込みの
	// タイムアウト（WriteTimeout）より短くする。
	themeInstallTimeout = 45 * time.Second
	// themeSettingsPath はテーマ設定の画面。
	themeSettingsPath = "/admin/settings/theme"
)

// customThemeAssetTypes はカスタムテーマから配信するファイルの拡張子と Content-Type。これ以外（theme.json・
// HTML・スクリプトなど）は配信しない。
var customThemeAssetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".avif":  "image/avif",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
}

// themeCatalog は公開ページ・管理画面・同期 API・エクスポートが使うテーマのカタログを返す。
func (s *Server) themeCatalog() *theme.Catalog {
	if s.themes == nil {
		return theme.Builtin
	}
	return s.themes
}

// themeStylesheetURL はテーマ t の CSS の URL を返す。ビルトインは /static/ の版付き URL、カスタムテーマは
// CustomThemeURLPrefix の下の URL に、取り込み直したときにブラウザ・CDN の古いキャッシュを使わないよう
// theme.css の更新時刻を版として付ける。
func themeStylesheetURL(t theme.Theme) string {
	if !t.IsCustom {
		return static.URL(t.Stylesheet())
	}
	u := "/" + t.Stylesheet()
	if info, err := os.Stat(filepath.Join(t.Dir, "theme.css")); err == nil {
		u += "?v=" + strconv.FormatInt(info.ModTime().UnixNano(), 36)
	}
	return u
}

// handleCustomThemeAsset は GET /custom-themes/:id/* でカスタムテーマのファイルを配信する（認証不要）。
// カタログにあるカスタムテーマのディレクトリの中の、customThemeAssetTypes の拡張子の通常のファイルだけを返す。
// 親ディレクトリへの脱出（..）・隠しファイル・シンボリックリンクでディレクトリの外を指すものは 404 にする。
func (s *Server) handleCustomThemeAsset(c echo.Context) error {
	// 見つからない応答はキャッシュさせない（同じ ID のテーマを置き換える一瞬に届いた要求の 404 を、CDN などに
	// 残させない）。見つかったファイルは下で Cache-Control を決め直す。
	c.Response().Header().Set("Cache-Control", "no-store")
	// ルートのパラメータは RawPath の有無で escape の扱いが変わるため、デコード済みの URL.Path から読む。
	rest, ok := strings.CutPrefix(c.Request().URL.Path, CustomThemeURLPrefix)
	if !ok {
		return echo.ErrNotFound
	}
	id, rel, ok := strings.Cut(rest, "/")
	if !ok {
		return echo.ErrNotFound
	}
	t, ok := s.themeCatalog().Get(id)
	if !ok || !t.IsCustom || t.ID != id || t.Dir == "" {
		return echo.ErrNotFound
	}
	full, ok := customThemeFile(t.Dir, rel)
	if !ok {
		return echo.ErrNotFound
	}
	ctype := customThemeAssetTypes[strings.ToLower(path.Ext(rel))]
	f, err := os.Open(full)
	if err != nil {
		return echo.ErrNotFound
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return echo.ErrNotFound
	}
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, ctype)
	if rel == "theme.css" {
		h.Set("Cache-Control", customThemeCacheControl)
	} else {
		h.Set("Cache-Control", customThemeAssetCacheControl)
	}
	h.Set("Content-Security-Policy", customThemeAssetCSP)
	h.Set(echo.HeaderXContentTypeOptions, "nosniff")
	http.ServeContent(c.Response(), c.Request(), info.Name(), info.ModTime(), f)
	return nil
}

// customThemeFile は、テーマのディレクトリ dir からの相対パス rel（/ 区切り）が指すファイルの実パスを返す。
// 配信してよい拡張子でない、空・.・..・隠しファイル（. で始まる）・\・:・NUL を含む要素がある、
// シンボリックリンクを解決すると dir の外になる場合は false を返す。
func customThemeFile(dir, rel string) (string, bool) {
	if rel == "" || strings.ContainsAny(rel, "\\:\x00") {
		return "", false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return "", false
		}
	}
	if _, ok := customThemeAssetTypes[strings.ToLower(path.Ext(rel))]; !ok {
		return "", false
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !pathWithin(full, root) {
		return "", false
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil || !pathWithin(real, root) {
		return "", false
	}
	return real, true
}

// pathWithin は p が root の中（root 自身は含まない）にあるかを返す。どちらも整えた絶対パスで渡す。
func pathWithin(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel)
}

// テーマ設定の画面に出す通知（?theme_flash=）。取り込み・削除の結果を、リダイレクトの後に表示する。
// 文言はここで決め、URL からは決まった種類（キー）とテーマ ID だけを受け取る（任意の文を画面に出させない）。
const (
	themeFlashInstalled   = "installed"
	themeFlashUninstalled = "uninstalled"
)

// themeFlashErrors はテーマの取り込み・削除に失敗したときの通知のキーと文言。
var themeFlashErrors = map[string]string{
	"blocked_url":     "プライベート IP や不正な接続先のため、テーマの取得をブロックしました。公開されている GitHub・GitLab のリポジトリ、または https の Zip の URL を指定してください",
	"too_large":       "テーマアーカイブのサイズが上限（Zip は 10MB、展開後は 50MB）を超えています",
	"unsafe_archive":  "アーカイブに不正なファイルパス（親フォルダへの移動・絶対パス・シンボリックリンクなど）が含まれているため、取り込めません",
	"builtin":         "ビルトインテーマと同じ名前のテーマは取り込めません。テーマのフォルダ名を変えてください",
	"invalid":         "テーマの形式（theme.json・theme.css）が正しくありません。theme.json の name.ja・description.ja・color_scheme と、外部のファイルを読み込まない theme.css が必要です。メンテナンス画面（maintenance.html）を入れるときは、1MB 以下の UTF-8 の HTML にしてください",
	"unsupported_url": "テーマの URL を読み取れません。GitHub・GitLab のリポジトリの URL、または https で始まる .zip の URL を指定してください",
	"fetch_failed":    "テーマをダウンロードできませんでした。URL が正しいか、インターネットに接続できるかを確かめてください",
	"timeout":         "テーマのダウンロードに時間がかかりすぎたため、中止しました。時間をおいてもう一度お試しください",
	"no_url":          "取り込むテーマの URL を入力してください",
	"no_file":         "取り込むテーマの Zip ファイルを選んでください",
	"in_use":          "適用中のテーマは削除できません。先に別のテーマを適用してください",
	"not_found":       "削除するカスタムテーマが見つかりません",
	"builtin_delete":  "ビルトインテーマは削除できません",
	"failed":          "テーマの処理に失敗しました。もう一度お試しください",
	"replace_failed":  "テーマを置き換えられず、前の版も元の場所に戻せませんでした。このテーマはいま使えません（適用中なら標準テーマで表示されます）。前の版のファイルは消さずに、テーマのフォルダの中の「.install-」で始まるフォルダに残しています（自動では戻りません）。もう一度取り込むと、このテーマを使えるようになります",
}

// themeFlashRedirect はテーマ設定の画面へ、通知のキー（と対象のテーマ ID）を付けて戻す。
func themeFlashRedirect(c echo.Context, key, id string) error {
	v := url.Values{"theme_flash": {key}}
	if id != "" {
		v.Set("theme_id", id)
	}
	return c.Redirect(http.StatusSeeOther, themeSettingsPath+"?"+v.Encode())
}

// themeFlashMessage は ?theme_flash= の通知の文言と、エラーかどうかを返す。知らないキーなら空。
// テーマの名前はカタログから引く（URL の文字列をそのまま出さない）。ID の形式に合わなければ ID も出さない。
func (s *Server) themeFlashMessage(c echo.Context) (msg string, isErr bool) {
	key := c.QueryParam("theme_flash")
	id := c.QueryParam("theme_id")
	switch key {
	case themeFlashInstalled:
		t, ok := s.themeCatalog().Get(id)
		if !ok {
			return "テーマを取り込みました", false
		}
		return "テーマ「" + t.Label(string(language(c))) + "」を取り込みました。「このテーマを適用」で公開サイトに使えます", false
	case themeFlashUninstalled:
		if !customThemeID.MatchString(id) {
			return "カスタムテーマを削除しました", false
		}
		return "カスタムテーマ「" + id + "」を削除しました", false
	}
	if m, ok := themeFlashErrors[key]; ok {
		return m, true
	}
	return "", false
}

// customThemeID はテーマ ID の形式（theme パッケージと同じ）。通知に出す ID を確かめる。
var customThemeID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

// themeInstallErrorKey は取り込みのエラーを通知のキーに変える。
func themeInstallErrorKey(err error) string {
	switch {
	case errors.Is(err, theme.ErrNotRestored):
		return "replace_failed"
	case errors.Is(err, theme.ErrBlockedURL):
		return "blocked_url"
	case errors.Is(err, theme.ErrArchiveTooLarge):
		return "too_large"
	case errors.Is(err, theme.ErrUnsafeArchive):
		return "unsafe_archive"
	case errors.Is(err, theme.ErrBuiltinTheme):
		return "builtin"
	case errors.Is(err, theme.ErrInvalidTheme):
		return "invalid"
	case errors.Is(err, theme.ErrUnsupportedURL):
		return "unsupported_url"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "fetch_failed"
	}
}

// installTheme は install でテーマを取り込み、カタログを読み直して、結果の通知へリダイレクトする。
// 取り込み・削除は themeMu で1つずつ行う（同じ ID のディレクトリを同時に入れ替えない）。
func (s *Server) installTheme(c echo.Context, install func() (*theme.Theme, error), failKey string) error {
	s.themeMu.Lock()
	t, err := install()
	if err == nil {
		err = s.themeCatalog().Reload(static.FS)
		if err != nil {
			s.themeMu.Unlock()
			c.Logger().Errorf("theme: reload after install: %v", err)
			return themeFlashRedirect(c, "failed", "")
		}
	}
	if errors.Is(err, theme.ErrNotRestored) {
		// 前の版を元の場所に戻せなかった（配置先にテーマが無い）ので、カタログをディスクの状態に合わせる。
		if rerr := s.themeCatalog().Reload(static.FS); rerr != nil {
			c.Logger().Errorf("theme: reload after a failed install: %v", rerr)
		}
	}
	s.themeMu.Unlock()
	if err != nil {
		c.Logger().Warnf("theme: install failed: %v", err)
		key := themeInstallErrorKey(err)
		if key == "fetch_failed" && failKey != "" {
			key = failKey
		}
		return themeFlashRedirect(c, key, "")
	}
	// 取り込み（URL・Zip）は外観の変更として、公開サーバーへ未反映と記録する。
	s.markRemoteDirty(c, remoteDirtyTheme)
	// 適用中のテーマを更新したときは、新しいアセットを静的サイトにも反映する。
	if strings.TrimSpace(s.getSetting("theme", theme.Default)) == t.ID {
		s.reflectAfterPublish()
	}
	return themeFlashRedirect(c, themeFlashInstalled, t.ID)
}

// handleThemeInstallURL は POST /admin/settings/theme/install-url で、GitHub・GitLab のリポジトリ、または
// Zip の URL（フォームの theme_url）からカスタムテーマを取り込む（管理者専用）。
// ダウンロードはネットワーク取得を伴うため、themeMu のロックの外で行い、
// 展開・検証・配置（Install）および Catalog.Reload の間だけロックを取得する。
func (s *Server) handleThemeInstallURL(c echo.Context) error {
	raw := strings.TrimSpace(c.FormValue("theme_url"))
	if raw == "" {
		return themeFlashRedirect(c, "no_url", "")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), themeInstallTimeout)
	defer cancel()

	arc, err := themeDownloadArchive(ctx, raw)
	if err != nil {
		c.Logger().Warnf("theme: download failed: %v", err)
		return themeFlashRedirect(c, themeInstallErrorKey(err), "")
	}
	return s.installTheme(c, func() (*theme.Theme, error) {
		return arc.Install(s.cfg.ThemesDir)
	}, "")
}

// themeDownloadArchive は URL からアーカイブをダウンロードする（テストで差し替え可能）。
var themeDownloadArchive = theme.DownloadArchive

// handleThemeInstallZip は POST /admin/settings/theme/install-zip で、アップロードした Zip（フォームの theme_file）
// からカスタムテーマを取り込む（管理者専用）。Zip は theme.MaxArchiveSize（10MB）まで。
func (s *Server) handleThemeInstallZip(c echo.Context) error {
	fh, err := c.FormFile("theme_file")
	if err != nil || fh.Size <= 0 {
		return themeFlashRedirect(c, "no_file", "")
	}
	if fh.Size > theme.MaxArchiveSize {
		return themeFlashRedirect(c, "too_large", "")
	}
	f, err := fh.Open()
	if err != nil {
		c.Logger().Warnf("theme: open uploaded zip: %v", err)
		return themeFlashRedirect(c, "failed", "")
	}
	defer f.Close()
	return s.installTheme(c, func() (*theme.Theme, error) {
		return theme.InstallFromZip(f, fh.Size, s.cfg.ThemesDir)
	}, "failed")
}

// handleThemeUninstall は POST /admin/settings/theme/uninstall で、カスタムテーマ（フォームの theme_id）を削除する
// （管理者専用）。適用中のテーマとビルトインテーマは削除しない。
func (s *Server) handleThemeUninstall(c echo.Context) error {
	id := strings.TrimSpace(c.FormValue("theme_id"))
	s.themeMu.Lock()
	defer s.themeMu.Unlock()
	if id == strings.TrimSpace(s.getSetting("theme", theme.Default)) {
		return themeFlashRedirect(c, "in_use", "")
	}
	if err := theme.UninstallCustomTheme(s.cfg.ThemesDir, id); err != nil {
		switch {
		case errors.Is(err, theme.ErrBuiltinTheme):
			return themeFlashRedirect(c, "builtin_delete", "")
		case errors.Is(err, theme.ErrThemeNotFound):
			return themeFlashRedirect(c, "not_found", "")
		}
		c.Logger().Errorf("theme: uninstall %q: %v", id, err)
		return themeFlashRedirect(c, "failed", "")
	}
	if err := s.themeCatalog().Reload(static.FS); err != nil {
		c.Logger().Errorf("theme: reload after uninstall: %v", err)
		return themeFlashRedirect(c, "failed", "")
	}
	s.markRemoteDirty(c, remoteDirtyTheme)
	return themeFlashRedirect(c, themeFlashUninstalled, id)
}
