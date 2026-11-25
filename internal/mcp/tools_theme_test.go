package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"sorus/internal/theme"
	"sorus/static"
)

// themeTestEnv は独自テーマの保存先と、OnThemeChanged の呼び出し回数を持つテスト環境。
type themeTestEnv struct {
	*testEnv
	dir     string
	catalog *theme.Catalog
	changed *atomic.Int32
}

func newThemeTestEnv(t *testing.T) *themeTestEnv {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "themes")
	catalog, err := theme.LoadComposite(static.FS, dir)
	if err != nil {
		t.Fatal(err)
	}
	var changed atomic.Int32
	env := newTestEnv(t, Config{
		ThemesDir:      dir,
		ThemeCatalog:   catalog,
		ThemeMu:        new(sync.Mutex),
		OnThemeChanged: func() { changed.Add(1) },
	})
	return &themeTestEnv{testEnv: env, dir: dir, catalog: catalog, changed: &changed}
}

// themeZipBase64 は最上位フォルダ dir の下に正しい独自テーマを置いた Zip を Base64 で返す。
func themeZipBase64(t *testing.T, dir string) string {
	t.Helper()
	return themeZipBase64WithCSS(t, dir, ":root { --bg: #fff; }\n")
}

func themeZipBase64WithCSS(t *testing.T, dir, css string) string {
	t.Helper()
	manifest := `{"name": {"ja": "コーポレート", "en": "Corporate"}, "description": {"ja": "独自テーマ", "en": "Custom theme"}, "color_scheme": "light", "order": 100}`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		dir + "/theme.json": manifest,
		dir + "/theme.css":  css,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func (e *themeTestEnv) listThemes(userID string) listThemesResult {
	e.t.Helper()
	var res listThemesResult
	e.toolOK(userID, "cms_list_themes", map[string]any{}, &res)
	return res
}

func findTheme(res listThemesResult, id string) (themeInfo, bool) {
	for _, th := range res.Themes {
		if th.ID == id {
			return th, true
		}
	}
	return themeInfo{}, false
}

func (e *themeTestEnv) storedTheme() string {
	e.t.Helper()
	var v string
	if err := e.db.Get(&v, `SELECT value FROM settings WHERE key = 'theme'`); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *themeTestEnv) installCorporate() {
	e.t.Helper()
	var res installThemeResult
	e.toolOK(admin.ID, "cms_install_theme", map[string]any{"zip_base64": themeZipBase64(e.t, "corporate")}, &res)
	if res.Status != "installed" || res.Theme.ID != "corporate" || !res.Theme.IsCustom || res.Message == "" {
		e.t.Fatalf("install result = %+v", res)
	}
}

func TestListThemes(t *testing.T) {
	env := newThemeTestEnv(t)
	res := env.listThemes(writerA.ID)
	if res.ActiveThemeID != theme.Default {
		t.Errorf("active_theme_id = %q, want %q", res.ActiveThemeID, theme.Default)
	}
	if len(res.Themes) != len(theme.Builtin.IDs()) {
		t.Fatalf("themes = %d, want %d built-ins", len(res.Themes), len(theme.Builtin.IDs()))
	}
	def, ok := findTheme(res, theme.Default)
	if !ok || def.IsCustom || def.Name == "" || def.ColorScheme == "" {
		t.Errorf("default theme = %+v (found %v)", def, ok)
	}
	for _, th := range res.Themes {
		if th.IsCustom {
			t.Errorf("unexpected custom theme %+v", th)
		}
	}

	env.installCorporate()
	res = env.listThemes(editor.ID)
	got, ok := findTheme(res, "corporate")
	if !ok {
		t.Fatalf("corporate not listed: %+v", res.Themes)
	}
	want := themeInfo{ID: "corporate", Name: "コーポレート", NameEN: "Corporate", Description: "独自テーマ", DescriptionEN: "Custom theme",
		IsCustom: true, ColorScheme: "light"}
	if got != want {
		t.Errorf("corporate = %+v, want %+v", got, want)
	}
	// 管理画面と同じカタログを共有している。
	if _, ok := env.catalog.Get("corporate"); !ok {
		t.Error("shared catalog was not reloaded")
	}
}

func TestInstallTheme(t *testing.T) {
	env := newThemeTestEnv(t)
	env.installCorporate()
	if _, err := os.Stat(filepath.Join(env.dir, "corporate", "theme.css")); err != nil {
		t.Errorf("theme not written: %v", err)
	}
	if n := env.changed.Load(); n != 1 {
		t.Errorf("OnThemeChanged called %d times, want 1", n)
	}

	for _, tc := range []struct {
		name string
		user string
		args map[string]any
		want string
	}{
		{"no args", admin.ID, map[string]any{}, "どちらか一方"},
		{"both args", admin.ID, map[string]any{"url": "https://github.com/a/b", "zip_base64": themeZipBase64(t, "x")}, "どちらか一方"},
		{"bad base64", admin.ID, map[string]any{"zip_base64": "!!!"}, "Base64"},
		{"not a zip", admin.ID, map[string]any{"zip_base64": base64.StdEncoding.EncodeToString([]byte("not a zip"))}, "not a zip archive"},
		{"builtin id", admin.ID, map[string]any{"zip_base64": themeZipBase64(t, theme.Default)}, "ビルトインテーマと同じ ID"},
		{"non-https url", admin.ID, map[string]any{"url": "ftp://example.com/x.zip"}, "この URL からは取り込めません"},
		{"unsupported url", admin.ID, map[string]any{"url": "https://example.com/page"}, "この URL からは取り込めません"},
		{"private url", admin.ID, map[string]any{"url": "https://127.0.0.1/x.zip"}, "この URL からは取り込めません"},
		{"invalid css", admin.ID, map[string]any{"zip_base64": themeZipBase64WithCSS(t, "badcss", "@import url(x.css);")}, "theme.css"},
		{"writer", writerA.ID, map[string]any{"zip_base64": themeZipBase64(t, "other")}, "権限がありません"},
		{"editor", editor.ID, map[string]any{"zip_base64": themeZipBase64(t, "other")}, "権限がありません"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectContains(t, env.toolErr(tc.user, "cms_install_theme", tc.args), tc.want)
		})
	}
	if n := env.changed.Load(); n != 1 {
		t.Errorf("OnThemeChanged called %d times after failures, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "other")); !os.IsNotExist(err) {
		t.Errorf("rejected install wrote files: %v", err)
	}
}

func TestInstallThemeFromURL(t *testing.T) {
	env := newThemeTestEnv(t)
	var gotURL string
	orig := themeDownloadArchive
	t.Cleanup(func() { themeDownloadArchive = orig })
	themeDownloadArchive = func(ctx context.Context, rawURL string) (*theme.DownloadedArchive, error) {
		gotURL = rawURL
		// ダウンロード中に themeMu がロックされていないことを確認
		if !env.server.themeMu.TryLock() {
			t.Error("themeMu is locked during DownloadArchive in MCP tool")
		} else {
			env.server.themeMu.Unlock()
		}
		b, _ := base64.StdEncoding.DecodeString(themeZipBase64(t, "remote"))
		return &theme.DownloadedArchive{
			Data:      b,
			Hint:      "remote",
			SourceURL: rawURL,
		}, nil
	}
	var res installThemeResult
	env.toolOK(admin.ID, "cms_install_theme", map[string]any{"url": " https://github.com/example/remote "}, &res)
	if gotURL != "https://github.com/example/remote" {
		t.Errorf("gotURL = %q, want https://github.com/example/remote", gotURL)
	}
	if res.Theme.ID != "remote" || !res.Theme.IsCustom {
		t.Errorf("result = %+v", res)
	}
}

func TestInstallThemeWithoutThemesDir(t *testing.T) {
	env := newTestEnv(t, Config{})
	expectContains(t, env.toolErr(admin.ID, "cms_install_theme", map[string]any{"zip_base64": themeZipBase64(t, "corporate")}), "保存先が設定されていない")
	// 一覧と適用はビルトインテーマで動く。
	var list listThemesResult
	env.toolOK(writerA.ID, "cms_list_themes", map[string]any{}, &list)
	if list.ActiveThemeID != theme.Default || len(list.Themes) == 0 {
		t.Errorf("list = %+v", list)
	}
}

func TestActivateTheme(t *testing.T) {
	env := newThemeTestEnv(t)
	env.installCorporate()
	before := env.changed.Load()

	var res themeIDResult
	env.toolOK(admin.ID, "cms_activate_theme", map[string]any{"theme_id": "corporate"}, &res)
	if res.Status != "activated" || res.ThemeID != "corporate" || res.Message == "" {
		t.Errorf("result = %+v", res)
	}
	if got := env.storedTheme(); got != "corporate" {
		t.Errorf("settings.theme = %q", got)
	}
	if got := env.listThemes(writerA.ID).ActiveThemeID; got != "corporate" {
		t.Errorf("active_theme_id = %q", got)
	}
	if n := env.changed.Load(); n != before+1 {
		t.Errorf("OnThemeChanged called %d times, want %d", n, before+1)
	}

	// ビルトインへも切り替えられる（管理者）。
	builtin := ""
	for _, id := range theme.Builtin.IDs() {
		if id != theme.Default {
			builtin = id
			break
		}
	}
	env.toolOK(admin.ID, "cms_activate_theme", map[string]any{"theme_id": builtin}, &res)
	if got := env.storedTheme(); got != builtin {
		t.Errorf("settings.theme = %q, want %q", got, builtin)
	}

	expectContains(t, env.toolErr(admin.ID, "cms_activate_theme", map[string]any{"theme_id": "missing"}), "ありません")
	expectContains(t, env.toolErr(admin.ID, "cms_activate_theme", map[string]any{"theme_id": "../etc"}), "ありません")
	expectContains(t, env.toolErr(admin.ID, "cms_activate_theme", map[string]any{"theme_id": ""}), "theme_id を指定")
	expectContains(t, env.toolErr(editor.ID, "cms_activate_theme", map[string]any{"theme_id": "corporate"}), "権限がありません")
	expectContains(t, env.toolErr(writerA.ID, "cms_activate_theme", map[string]any{"theme_id": "corporate"}), "権限がありません")
	if got := env.storedTheme(); got != builtin {
		t.Errorf("rejected calls changed settings.theme to %q", got)
	}
}

func TestUninstallTheme(t *testing.T) {
	env := newThemeTestEnv(t)
	env.installCorporate()

	// 適用中の独自テーマは削除できない。
	env.toolOK(admin.ID, "cms_activate_theme", map[string]any{"theme_id": "corporate"}, nil)
	expectContains(t, env.toolErr(admin.ID, "cms_uninstall_theme", map[string]any{"theme_id": "corporate"}), "適用中")
	env.toolOK(admin.ID, "cms_activate_theme", map[string]any{"theme_id": theme.Default}, nil)

	expectContains(t, env.toolErr(admin.ID, "cms_uninstall_theme", map[string]any{"theme_id": theme.Default}), "ビルトインテーマ")
	expectContains(t, env.toolErr(admin.ID, "cms_uninstall_theme", map[string]any{"theme_id": "missing"}), "ありません")
	expectContains(t, env.toolErr(editor.ID, "cms_uninstall_theme", map[string]any{"theme_id": "corporate"}), "権限がありません")
	expectContains(t, env.toolErr(writerA.ID, "cms_uninstall_theme", map[string]any{"theme_id": "corporate"}), "権限がありません")
	if _, ok := findTheme(env.listThemes(admin.ID), "corporate"); !ok {
		t.Fatal("rejected uninstall removed the theme")
	}

	before := env.changed.Load()
	var res themeIDResult
	env.toolOK(admin.ID, "cms_uninstall_theme", map[string]any{"theme_id": "corporate"}, &res)
	if res.Status != "uninstalled" || res.ThemeID != "corporate" || res.Message == "" {
		t.Errorf("result = %+v", res)
	}
	if _, ok := findTheme(env.listThemes(admin.ID), "corporate"); ok {
		t.Error("corporate still listed after uninstall")
	}
	if _, err := os.Stat(filepath.Join(env.dir, "corporate")); !os.IsNotExist(err) {
		t.Errorf("theme directory remains: %v", err)
	}
	if n := env.changed.Load(); n != before+1 {
		t.Errorf("OnThemeChanged called %d times, want %d", n, before+1)
	}
}

// TestThemeToolsConcurrent は取り込み・適用・削除・一覧を並行に呼んでもデータ競合しないことを確かめる（-race 用）。
func TestThemeToolsConcurrent(t *testing.T) {
	env := newThemeTestEnv(t)
	zip := themeZipBase64(t, "corporate")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env.callTool(admin.ID, "cms_install_theme", map[string]any{"zip_base64": zip})
			env.callTool(writerA.ID, "cms_list_themes", map[string]any{})
			env.callTool(admin.ID, "cms_activate_theme", map[string]any{"theme_id": theme.Default})
			env.callTool(admin.ID, "cms_uninstall_theme", map[string]any{"theme_id": "corporate"})
		}()
	}
	wg.Wait()
}

// 前の版を元の場所に戻せなかったとき（theme.ErrNotRestored）は、前の版を消さずに残したこと・自動では戻らないこと・
// もう一度取り込めば使えることを伝え、再起動で戻るとは言わない。サーバーのパスは返さない。
func TestThemeInstallErrorWhenThePreviousVersionIsNotRestored(t *testing.T) {
	env := newThemeTestEnv(t)
	err := env.server.themeInstallError(fmt.Errorf("%w (/srv/sorus/themes/corp): injected", theme.ErrNotRestored), false)
	var te *toolError
	if !errors.As(err, &te) {
		t.Fatalf("themeInstallError = %v, want a tool error", err)
	}
	for _, want := range []string{"前の版も元の場所に戻せませんでした", "消さずに", "自動では戻りません", "もう一度取り込む"} {
		if !strings.Contains(te.msg, want) {
			t.Errorf("message %q does not contain %q", te.msg, want)
		}
	}
	for _, wrong := range []string{"再起動", "/srv/sorus"} {
		if strings.Contains(te.msg, wrong) {
			t.Errorf("message %q contains %q", te.msg, wrong)
		}
	}
}
