package theme

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"sorus/static"
)

// fixtureFS は testdata/themes の正しいテーマ一式を、書き換えられる MapFS に写して返す。
func fixtureFS(t *testing.T) fstest.MapFS {
	t.Helper()
	src := os.DirFS("testdata")
	out := fstest.MapFS{}
	err := fs.WalkDir(src, "themes", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		out[p] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLoadFixture(t *testing.T) {
	c, err := Load(os.DirFS("testdata"))
	if err != nil {
		t.Fatalf("Load(testdata): %v", err)
	}
	// Order の昇順、同じ Order なら ID の順（classic と sample はどちらも 20）。
	if got, want := c.IDs(), []string{"modern", "classic", "sample"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	all := c.All()
	if len(all) != 3 || all[2].ID != "sample" {
		t.Fatalf("All() = %+v", all)
	}
	sample := all[2]
	if sample.Label("ja") != "サンプル" || sample.Label("en") != "Sample" ||
		sample.Summary("en") != "Proves a theme is added in one place" || sample.ColorScheme != "dark" || sample.Order != 20 {
		t.Errorf("sample theme = %+v", sample)
	}
	if got := sample.Stylesheet(); got != "themes/sample/theme.css" {
		t.Errorf("Stylesheet() = %q", got)
	}
	// 英語が無ければ日本語を使う。
	classic, _ := c.Get("classic")
	if classic.Label("en") != "クラシック" || classic.Summary("en") != "英語の名前と説明を持たないテーマ" {
		t.Errorf("classic fallback: Label(en)=%q Summary(en)=%q", classic.Label("en"), classic.Summary("en"))
	}
	if got := (Theme{ID: "bare"}).Label("en"); got != "bare" {
		t.Errorf("Label without names = %q, want the ID", got)
	}
	// All は複製を返し、呼び出し元が並びを壊せない。
	all[0] = Theme{}
	if c.All()[0].ID != "modern" {
		t.Error("All() exposes the internal slice")
	}
}

func TestLoadRejectsInvalidThemes(t *testing.T) {
	const sampleJSON = "themes/sample/theme.json"
	for _, tc := range []struct {
		name   string
		mutate func(fstest.MapFS)
		want   string
	}{
		{"invalid directory name", func(m fstest.MapFS) {
			m["themes/Upper/theme.json"] = m[sampleJSON]
			m["themes/Upper/theme.css"] = m["themes/sample/theme.css"]
		}, "Upper"},
		{"malformed json", func(m fstest.MapFS) {
			m[sampleJSON] = &fstest.MapFile{Data: []byte(`{"name": {"ja": "サンプル"`)}
		}, "sample"},
		{"trailing data", func(m fstest.MapFS) {
			m[sampleJSON] = &fstest.MapFile{Data: append(append([]byte{}, m[sampleJSON].Data...), []byte(`{}`)...)}
		}, "sample"},
		{"unknown field", func(m fstest.MapFS) {
			m[sampleJSON] = &fstest.MapFile{Data: []byte(`{"name": {"ja": "サンプル"}, "description": {"ja": "説明"},
				"color_scheme": "light", "theme_color": "#0f172a"}`)}
		}, "theme_color"},
		{"missing name.ja", func(m fstest.MapFS) {
			m[sampleJSON] = &fstest.MapFile{Data: []byte(`{"name": {"en": "Sample"}, "description": {"ja": "説明"}, "color_scheme": "light"}`)}
		}, "name.ja"},
		{"missing description.ja", func(m fstest.MapFS) {
			m[sampleJSON] = &fstest.MapFile{Data: []byte(`{"name": {"ja": "サンプル"}, "description": {"en": "Text"}, "color_scheme": "light"}`)}
		}, "description.ja"},
		{"invalid color_scheme", func(m fstest.MapFS) {
			m[sampleJSON] = &fstest.MapFile{Data: []byte(`{"name": {"ja": "サンプル"}, "description": {"ja": "説明"}, "color_scheme": "auto"}`)}
		}, "color_scheme"},
		{"missing color_scheme", func(m fstest.MapFS) {
			m[sampleJSON] = &fstest.MapFile{Data: []byte(`{"name": {"ja": "サンプル"}, "description": {"ja": "説明"}}`)}
		}, "color_scheme"},
		{"missing theme.css", func(m fstest.MapFS) {
			delete(m, "themes/sample/theme.css")
		}, "theme.css"},
		{"missing theme.json", func(m fstest.MapFS) {
			delete(m, sampleJSON)
		}, "sample"},
		{"missing default theme", func(m fstest.MapFS) {
			delete(m, "themes/modern/theme.json")
			delete(m, "themes/modern/theme.css")
		}, Default},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureFS(t)
			if _, err := Load(m); err != nil {
				t.Fatalf("fixture itself is invalid: %v", err)
			}
			tc.mutate(m)
			_, err := Load(m)
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestGetAndResolve(t *testing.T) {
	c, err := Load(os.DirFS("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Get("  sample\n"); !ok || got.ID != "sample" {
		t.Errorf("Get with spaces = %+v, %v", got, ok)
	}
	for _, id := range []string{"../x", "../sample", "Upper", "a.css", "", "  ", "sample/theme", "nonexistent"} {
		if _, ok := c.Get(id); ok {
			t.Errorf("Get(%q) succeeded, want false", id)
		}
		if got := c.Resolve(id); got.ID != Default {
			t.Errorf("Resolve(%q) = %q, want %q", id, got.ID, Default)
		}
	}
	if got := c.Resolve(" classic "); got.ID != "classic" {
		t.Errorf("Resolve(classic) = %q", got.ID)
	}
}

func TestBuiltinCatalog(t *testing.T) {
	// テーマはディレクトリを足すだけで増えるため、公式テーマの存在と並び順を確かめる。
	ids := Builtin.IDs()
	if !slices.Contains(ids, Default) {
		t.Fatalf("Builtin.IDs() = %v, want it to contain Default %q", ids, Default)
	}
	prev := -1
	for _, id := range []string{"modern", "tech", "news", "portal", "portfolio"} {
		i := slices.Index(ids, id)
		if i < 0 {
			t.Fatalf("Builtin.IDs() = %v, want it to contain %q", ids, id)
		}
		if i <= prev {
			t.Fatalf("Builtin.IDs() = %v, want modern, tech, news, portal, portfolio in this relative order", ids)
		}
		prev = i
	}
	for _, legacy := range []string{"minimal", "editorial", "mono"} {
		if _, ok := Builtin.Get(legacy); ok {
			t.Errorf("legacy theme %q is still in the catalog", legacy)
		}
		if got := Builtin.Resolve(legacy); got.ID != Default {
			t.Errorf("Resolve(%q) = %q, want %q", legacy, got.ID, Default)
		}
	}
	for _, th := range Builtin.All() {
		if th.Label("ja") == th.ID || th.Label("en") == th.ID || th.Summary("ja") == "" || th.Summary("en") == "" {
			t.Errorf("theme %s lacks a ja/en name or description: %+v", th.ID, th)
		}
		if _, err := fs.Stat(static.FS, th.Stylesheet()); err != nil {
			t.Errorf("theme %s stylesheet: %v", th.ID, err)
		}
	}
	news, _ := Builtin.Get("news")
	if news.Label("ja") != "ニュース" || news.Label("en") != "News" || news.ColorScheme != "light dark" {
		t.Errorf("news = %+v", news)
	}
	portfolio, ok := Builtin.Get("portfolio")
	if !ok || portfolio.Label("ja") != "ポートフォリオ" || portfolio.Label("en") != "Portfolio" || portfolio.ColorScheme != "light dark" {
		t.Errorf("portfolio = %+v, want official bilingual light/dark theme", portfolio)
	}
}

// writeCustomTheme は dir/<id>/ にカスタムテーマを書く。
func writeCustomTheme(t *testing.T, dir, id, manifestJSON, css string) {
	t.Helper()
	p := filepath.Join(dir, id)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "theme.json"), []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if css != "" {
		if err := os.WriteFile(filepath.Join(p, "theme.css"), []byte(css), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func customJSON(ja string, order int) string {
	return fmt.Sprintf(`{"name": {"ja": %q}, "description": {"ja": "説明"}, "color_scheme": "light", "order": %d, "source_url": "https://github.com/example/%s"}`, ja, order, ja)
}

func TestLoadCompositeMergesCustomThemes(t *testing.T) {
	custom := t.TempDir()
	writeCustomTheme(t, custom, "zeta", customJSON("zeta", 5), validCSS)     // 一番前に並ぶ
	writeCustomTheme(t, custom, "alpha", customJSON("alpha", 20), validCSS)  // classic・sample と同じ 20。ID の順
	writeCustomTheme(t, custom, "classic", customJSON("over", 99), validCSS) // ビルトインと同じ ID はカスタムが優先
	writeCustomTheme(t, custom, "broken", customJSON("broken", 1), "")       // theme.css が無い
	writeCustomTheme(t, custom, "remote", customJSON("remote", 1), `@import url("https://cdn.example.com/x.css");`)
	writeCustomTheme(t, custom, ".install-123", customJSON("tmp", 1), validCSS) // 取り込み中の作業ディレクトリ
	if err := os.WriteFile(filepath.Join(custom, "README.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := LoadComposite(os.DirFS("testdata"), custom)
	if err != nil {
		t.Fatalf("LoadComposite: %v", err)
	}
	if got, want := c.IDs(), []string{"zeta", "modern", "alpha", "sample", "classic"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	if c.CustomDir() != custom {
		t.Errorf("CustomDir() = %q", c.CustomDir())
	}
	zeta, _ := c.Get("zeta")
	if !zeta.IsCustom || zeta.Dir != filepath.Join(custom, "zeta") || zeta.SourceURL != "https://github.com/example/zeta" ||
		zeta.Stylesheet() != "custom-themes/zeta/theme.css" {
		t.Errorf("zeta = %+v (stylesheet %q)", zeta, zeta.Stylesheet())
	}
	classic, _ := c.Get("classic")
	if !classic.IsCustom || classic.Label("ja") != "over" {
		t.Errorf("custom classic does not override the built-in: %+v", classic)
	}
	modern, _ := c.Get("modern")
	if modern.IsCustom || modern.Dir != "" || modern.Stylesheet() != "themes/modern/theme.css" {
		t.Errorf("modern = %+v", modern)
	}
	skipped := c.Skipped()
	if len(skipped) != 2 || skipped["broken"] == nil || skipped["remote"] == nil {
		t.Errorf("Skipped() = %v, want broken and remote", skipped)
	}
	if !errors.Is(skipped["remote"], ErrInvalidTheme) || !strings.Contains(skipped["remote"].Error(), "@import") {
		t.Errorf("remote skip reason = %v", skipped["remote"])
	}
}

func TestLoadCompositeWithoutCustomDir(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		c, err := LoadComposite(os.DirFS("testdata"), dir)
		if err != nil {
			t.Fatalf("LoadComposite(%q): %v", dir, err)
		}
		if got, want := c.IDs(), []string{"modern", "classic", "sample"}; !reflect.DeepEqual(got, want) {
			t.Errorf("LoadComposite(%q).IDs() = %v, want %v", dir, got, want)
		}
	}
	// customDir がファイルなら読めないのでエラー。
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadComposite(os.DirFS("testdata"), file); err == nil {
		t.Error("LoadComposite with a file as customDir succeeded")
	}
	// ビルトインの Default が無ければ、カスタムがあってもエラー（カスタムの modern で代用しない）。
	m := fixtureFS(t)
	delete(m, "themes/modern/theme.json")
	delete(m, "themes/modern/theme.css")
	custom := t.TempDir()
	writeCustomTheme(t, custom, "modern", customJSON("m", 1), validCSS)
	if _, err := LoadComposite(m, custom); err == nil || !strings.Contains(err.Error(), Default) {
		t.Errorf("LoadComposite without the built-in default = %v", err)
	}
}

func TestCatalogReload(t *testing.T) {
	custom := t.TempDir()
	c, err := LoadComposite(os.DirFS("testdata"), custom)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("fresh"); ok {
		t.Fatal("fresh exists before it is written")
	}
	writeCustomTheme(t, custom, "fresh", customJSON("fresh", 1), validCSS)
	if err := c.Reload(os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Get("fresh"); !ok || !got.IsCustom {
		t.Errorf("Reload did not pick up the new theme: %+v %v", got, ok)
	}
	if err := UninstallCustomTheme(custom, "fresh"); err != nil {
		t.Fatal(err)
	}
	// 失敗した Reload は今の中身を保つ。
	if err := c.Reload(fstest.MapFS{}); err == nil {
		t.Fatal("Reload from an empty FS succeeded")
	}
	if _, ok := c.Get("fresh"); !ok {
		t.Error("failed Reload changed the catalog")
	}
	if err := c.Reload(os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("fresh"); ok {
		t.Error("Reload kept an uninstalled theme")
	}
}

func TestCatalogConcurrentReload(t *testing.T) {
	custom := t.TempDir()
	writeCustomTheme(t, custom, "zeta", customJSON("zeta", 5), validCSS)
	c, err := LoadComposite(os.DirFS("testdata"), custom)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := c.Reload(os.DirFS("testdata")); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = c.All()
				_ = c.IDs()
				_ = c.Resolve("zeta")
				_ = c.Skipped()
			}
		}()
	}
	wg.Wait()
}

func TestValidateThemeDirCSSResources(t *testing.T) {
	const okJSON = `{"name": {"ja": "x"}, "description": {"ja": "y"}, "color_scheme": "light"}`
	for _, tc := range []struct {
		name string
		css  string
		want string // 空なら通る
	}{
		{"relative url", `.a { background: url(img/bg.png); } .b { background: url("../x.png"); }`, ""},
		{"data url", `.a { background: url("data:image/png;base64,AAAA"); }`, ""},
		{"url in a comment", "/* see https://example.com and @import */ .a { color: red; }", ""},
		{"https url", `@font-face { src: url( "https://cdn.example.com/font.woff2" ); }`, "external URL"},
		{"http url", `.a { background: URL(http://cdn.example.com/a.png); }`, "external URL"},
		{"protocol-relative", `.a { background: url(//cdn.example.com/a.png); }`, "protocol-relative"},
		{"protocol-relative string", `.a { background: image-set("//cdn.example.com/a.png" 1x); }`, "protocol-relative"},
		{"image-set string", `.a { background: image-set("https://cdn.example.com/a.png" 1x); }`, "external URL"},
		{"import", `@import "other.css";`, "@import"},
		{"import uppercase", `@IMPORT url(other.css);`, "@import"},
		{"escaped import", `@\69mport "other.css";`, "@import"},
		{"escaped scheme", `.a { background: url(ht\74ps://cdn.example.com/a.png); }`, "external URL"},
		{"comment hides nothing", `.a { background: url(/**/https://cdn.example.com/a.png); }`, "external URL"},
		{"attribute selector", `.prose a[href^="https://"]::after, a[href*='http://'] { content: " (link)"; }`, ""},
		{"escaped bracket", `.a { background: url(a.png)\[ , url("https://cdn.example.com/a.png"); }`, "external URL"},
		{"bracket in string", `.a { content: "["; background: url("https://cdn.example.com/a.png"); content: "]"; }`, "external URL"},
		{"bracket inside unquoted url", `.a { background: image-set(url(x[) 1x, url("https://cdn.example.com/a.png") 2x); }`, "external URL"},
		{"string in brackets without =", `.a { background: [ url("https://cdn.example.com/a.png") ]; }`, "external URL"},
		{"bare url in brackets", `.a { background: [ url(https://cdn.example.com/a.png) ]; }`, "external URL"},
		{"unterminated comment", `.a { color: red; } /* @import "x.css";`, ""},
		{"tab in scheme", "background: url(\"ht\ttps://evil.example/t.png\")", "external URL"},
		{"escaped tab in scheme", "background: url(\"ht\\9 tps://evil.example/t.png\")", "external URL"},
		{"line continuation in scheme", "background: url(\"ht\\\ntps://evil.example/x.png\")", "external URL"},
		{"escaped backslashes protocol relative", "background: url(\"\\\\\\\\evil.example/t.png\")", "protocol-relative"},
		{"slash and escaped backslash protocol relative", "background: url(\"/\\\\evil.example/t.png\")", "protocol-relative"},
		{"control char 0x01 before protocol relative", "background: url(\"\x01 //evil.example/x.png\")", "protocol-relative"},
		{"control char 0x0b before protocol relative", "background: url(\"\x0b //evil.example/x.png\")", "protocol-relative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fstest.MapFS{
				"x/custom/theme.json": {Data: []byte(okJSON)},
				"x/custom/theme.css":  {Data: []byte(tc.css)},
			}
			th, err := ValidateThemeDir(m, "x/custom")
			if tc.want == "" {
				if err != nil || th.ID != "custom" {
					t.Fatalf("ValidateThemeDir = %+v, %v; want success", th, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidTheme) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateThemeDir = %v, want ErrInvalidTheme mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := ValidateThemeDir(fstest.MapFS{}, "x/Bad.Name"); !errors.Is(err, ErrInvalidTheme) {
		t.Errorf("invalid ID = %v", err)
	}
}

func TestCleanupStaleStaging(t *testing.T) {
	dir := t.TempDir()
	staleInstall := filepath.Join(dir, ".install-stale")
	recentInstall := filepath.Join(dir, ".install-recent")
	staleUninstall := filepath.Join(dir, ".uninstall-stale")
	normalDir := filepath.Join(dir, "mytheme")
	normalFile := filepath.Join(dir, "README.txt")

	for _, d := range []string{staleInstall, recentInstall, staleUninstall, normalDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(normalFile, []byte("readme"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 古い作業ディレクトリ（2時間前）のタイムスタンプを設定
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleInstall, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleUninstall, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// 30分以上の残骸のみ掃除する
	CleanupStaleStaging(dir, 30*time.Minute)

	if _, err := os.Stat(staleInstall); !os.IsNotExist(err) {
		t.Errorf("stale .install-* was not cleaned up: %v", err)
	}
	if _, err := os.Stat(staleUninstall); !os.IsNotExist(err) {
		t.Errorf("stale .uninstall-* was not cleaned up: %v", err)
	}
	if _, err := os.Stat(recentInstall); err != nil {
		t.Errorf("recent .install-* was prematurely deleted: %v", err)
	}
	if _, err := os.Stat(normalDir); err != nil {
		t.Errorf("normal theme directory was removed: %v", err)
	}
	if _, err := os.Stat(normalFile); err != nil {
		t.Errorf("normal file was removed: %v", err)
	}
}

// selectorDecls は rules の中で、セレクタ一覧に selector をそのまま含む規則の宣言をまとめて返す（後のものが勝つ）。
// @layer の中は辿るが、@media などの条件付きの規則は通常時の見た目ではないため数えない。
func selectorDecls(rules []cssRule, selector string) map[string]string {
	out := map[string]string{}
	for _, r := range rules {
		if !r.block {
			continue
		}
		if strings.HasPrefix(r.prelude, "@layer") {
			for k, v := range selectorDecls(r.children, selector) {
				out[k] = v
			}
			continue
		}
		if strings.HasPrefix(r.prelude, "@") || !slices.Contains(selectorList(r.prelude), selector) {
			continue
		}
		for _, p := range splitTopLevel(r.body, ';') {
			if d, ok := parseDeclaration(p); ok {
				out[d.name] = strings.TrimSpace(strings.TrimSuffix(d.value, "!important"))
			}
		}
	}
	return out
}

// TestBasePublicCSSEditorialFallbacks は、テーマが無くても本体の public.css が
// 画像なしカード・is-contain の余白・記事一覧のフィルタバーを崩さず表示できることを確かめる。
func TestBasePublicCSSEditorialFallbacks(t *testing.T) {
	data, err := fs.ReadFile(static.FS, "css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	src, err := stripComments(string(data))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := parseCSS(src)
	if err != nil {
		t.Fatal(err)
	}
	if d := selectorDecls(rules, "img.is-contain"); d["object-fit"] != "contain" || d["padding"] == "" {
		t.Errorf("img.is-contain = %v, want contain with padding", d)
	}
	for _, sel := range []string{".card-media img", ".front-card-media img", ".related-article-media img"} {
		if d := selectorDecls(rules, sel); d["object-position"] != "center 25%" {
			t.Errorf("%s object-position = %q, want center 25%%", sel, d["object-position"])
		}
	}
	if d := selectorDecls(rules, ".articles-filter-bar"); d["display"] != "grid" || d["grid-template-columns"] != "minmax(0, 1fr) auto" {
		t.Errorf(".articles-filter-bar = %v, want a two-column grid toolbar (minmax(0, 1fr) auto)", d)
	}
	for _, sel := range []string{
		".category-filter-pills", "select.year-filter-select", ".card--no-media", ".front-card--no-media", ".related-article-card--no-media", ".front-card-index",
		".site-header--centered", ".site-header--minimal", ".front-hero--centered", ".front-hero--split", ".card-grid--list", ".card-grid--magazine",
	} {
		if len(selectorDecls(rules, sel)) == 0 {
			t.Errorf("%s has no base style", sel)
		}
	}
}

func TestValidateThemeDirLayout(t *testing.T) {
	defaultLayout := ThemeLayout{
		HeaderStyle: HeaderStyleInline,
		HeroLayout:  HeroLayoutStacked,
		ArticleList: ArticleListGrid,
	}
	for _, tc := range []struct {
		name       string
		layoutJSON string
		want       ThemeLayout
	}{
		{"omitted", "", defaultLayout},
		{"empty object", `, "layout": {}`, defaultLayout},
		{"partial", `, "layout": {"header_style": "centered"}`, ThemeLayout{
			HeaderStyle: HeaderStyleCentered,
			HeroLayout:  HeroLayoutStacked,
			ArticleList: ArticleListGrid,
		}},
		{"all defaults explicit", `, "layout": {"header_style": "inline", "hero_layout": "stacked", "article_list": "grid"}`, defaultLayout},
		{"all non-default values", `, "layout": {"header_style": "minimal", "hero_layout": "split", "article_list": "magazine"}`, ThemeLayout{
			HeaderStyle: HeaderStyleMinimal,
			HeroLayout:  HeroLayoutSplit,
			ArticleList: ArticleListMagazine,
		}},
		{"centered and list", `, "layout": {"header_style": "centered", "hero_layout": "centered", "article_list": "list"}`, ThemeLayout{
			HeaderStyle: HeaderStyleCentered,
			HeroLayout:  HeroLayoutCentered,
			ArticleList: ArticleListList,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifestJSON := `{"name": {"ja": "テスト"}, "description": {"ja": "説明"}, "color_scheme": "light"` + tc.layoutJSON + `}`
			m := fstest.MapFS{
				"x/custom/theme.json": {Data: []byte(manifestJSON)},
				"x/custom/theme.css":  {Data: []byte(`.a { color: red; }`)},
			}
			th, err := ValidateThemeDir(m, "x/custom")
			if err != nil {
				t.Fatalf("ValidateThemeDir(%s): %v", tc.name, err)
			}
			if th.Layout != tc.want {
				t.Errorf("th.Layout = %+v, want %+v", th.Layout, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name       string
		layoutJSON string
		wantErr    string
	}{
		{"invalid header_style", `, "layout": {"header_style": "floating"}`, "layout.header_style"},
		{"invalid hero_layout", `, "layout": {"hero_layout": "fullbleed"}`, "layout.hero_layout"},
		{"invalid article_list", `, "layout": {"article_list": "masonry"}`, "layout.article_list"},
		{"unknown layout field", `, "layout": {"sidebar": "left"}`, "sidebar"},
		{"layout not an object", `, "layout": "centered"`, "theme.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifestJSON := `{"name": {"ja": "テスト"}, "description": {"ja": "説明"}, "color_scheme": "light"` + tc.layoutJSON + `}`
			m := fstest.MapFS{
				"x/custom/theme.json": {Data: []byte(manifestJSON)},
				"x/custom/theme.css":  {Data: []byte(`.a { color: red; }`)},
			}
			_, err := ValidateThemeDir(m, "x/custom")
			if !errors.Is(err, ErrInvalidTheme) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateThemeDir(%s) = %v, want ErrInvalidTheme mentioning %q", tc.name, err, tc.wantErr)
			}
		})
	}
}
