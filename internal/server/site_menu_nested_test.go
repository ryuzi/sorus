package server

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"sorus/internal/auth"
)

// menuRow はメニュー・フッターのサイトマップのフォームの1行（depth が 1 なら直前の最上位の行の子項目）。
type menuRow struct {
	depth              int
	typ, target, label string
	labelEn            string
}

// addMenuRows は f に prefix（menu・footer）の行を足す。
func addMenuRows(f url.Values, prefix string, rows []menuRow) url.Values {
	for _, r := range rows {
		page, cat, u := "", "", ""
		switch MenuItemType(r.typ) {
		case MenuItemPage:
			page = r.target
		case MenuItemCategory:
			cat = r.target
		case MenuItemURL:
			u = r.target
		}
		depth := "0"
		if r.depth == 1 {
			depth = "1"
		}
		f.Add(prefix+"_type", r.typ)
		f.Add(prefix+"_page", page)
		f.Add(prefix+"_category", cat)
		f.Add(prefix+"_url", u)
		f.Add(prefix+"_label", r.label)
		f.Add(prefix+"_label_en", r.labelEn)
		f.Add(prefix+"_depth", depth)
	}
	return f
}

// nestedMenu はドロップダウン2つ（見出しのみ・リンクのある親）とふつうの項目を持つメニュー。
func nestedMenu() []MenuItem {
	return []MenuItem{
		{Label: "研究内容", LabelEn: "Research", Type: MenuItemGroup, Children: []MenuItem{
			{Label: "研究概要", Type: MenuItemPage, Target: "research"},
			{Label: "サークル", Type: MenuItemCategory, Target: "circle"},
		}},
		{Type: MenuItemPage, Target: "access", Children: []MenuItem{
			{Label: "記事", Type: MenuItemArticles},
			{Label: "大学", Type: MenuItemURL, Target: "https://www.example.ac.jp/"},
		}},
		{Type: MenuItemArticles},
	}
}

// nestedFooter は見出しのみの列と、リンクの見出しの列を持つフッターのサイトマップ。
func nestedFooter() []MenuItem {
	return []MenuItem{
		{Label: "探す", Type: MenuItemGroup, Children: []MenuItem{
			{Type: MenuItemPage, Target: "research"},
			{Label: "RSS", Type: MenuItemURL, Target: "/feed.xml"},
		}},
		{Label: "連絡", Type: MenuItemPage, Target: "access"},
	}
}

func TestValidateNestedMenu(t *testing.T) {
	children := func(n int) []MenuItem {
		m := make([]MenuItem, n)
		for i := range m {
			m[i] = MenuItem{Type: MenuItemArticles}
		}
		return m
	}
	group := func(kids ...MenuItem) MenuItem {
		return MenuItem{Label: "見出し", Type: MenuItemGroup, Children: kids}
	}
	for _, tc := range []struct {
		name    string
		st      SiteStructure
		wantKey string // 空なら有効
		row     int
		child   int
		footer  bool
	}{
		{"nested menu and footer", SiteStructure{Menu: nestedMenu(), FooterMenu: nestedFooter()}, "", 0, 0, false},
		{"max children", SiteStructure{Menu: []MenuItem{group(children(maxMenuChildren)...)}}, "", 0, 0, false},
		{"too many children", SiteStructure{Menu: []MenuItem{{Type: MenuItemArticles}, group(children(maxMenuChildren + 1)...)}},
			"structure_err_children_count", 2, 0, false},
		{"group without children", SiteStructure{Menu: []MenuItem{group()}}, "structure_err_group_empty", 1, 0, false},
		{"group without label", SiteStructure{Menu: []MenuItem{{Label: " ", Type: MenuItemGroup, Children: children(1)}}},
			"structure_err_group_label", 1, 0, false},
		{"group with target", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemGroup, Target: "research", Children: children(1)}}},
			"structure_err_group_target", 1, 0, false},
		{"grandchild", SiteStructure{Menu: []MenuItem{group(MenuItem{Type: MenuItemArticles, Children: children(1)})}},
			"structure_err_menu_depth", 1, 1, false},
		{"group as child", SiteStructure{Menu: []MenuItem{group(MenuItem{Type: MenuItemArticles}, MenuItem{Label: "子の見出し", Type: MenuItemGroup})}},
			"structure_err_group_child", 1, 2, false},
		{"bad child url", SiteStructure{Menu: []MenuItem{{Type: MenuItemArticles}, group(MenuItem{Label: "x", Type: MenuItemURL, Target: "javascript:alert(1)"})}},
			"structure_err_url", 2, 1, false},
		{"child without target", SiteStructure{Menu: []MenuItem{group(MenuItem{Type: MenuItemPage})}}, "structure_err_target_required", 1, 1, false},
		{"max footer columns", SiteStructure{FooterMenu: children(maxFooterColumns)}, "", 0, 0, false},
		{"too many footer columns", SiteStructure{FooterMenu: children(maxFooterColumns + 1)}, "structure_err_footer_count", 0, 0, true},
		{"footer group without children", SiteStructure{FooterMenu: []MenuItem{{Type: MenuItemArticles}, group()}}, "structure_err_group_empty", 2, 0, true},
		{"footer bad link", SiteStructure{FooterMenu: []MenuItem{group(MenuItem{Type: MenuItemArticles}, MenuItem{Label: "x", Type: MenuItemURL, Target: "//evil.example"})}},
			"structure_err_url", 1, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSiteStructure(tc.st)
			if tc.wantKey == "" {
				if err != nil {
					t.Fatalf("validateSiteStructure = %v, want nil", err)
				}
				return
			}
			var se *siteStructureError
			if !errors.As(err, &se) || se.Key != tc.wantKey || se.Row != tc.row || se.Child != tc.child || se.Footer != tc.footer {
				t.Fatalf("validateSiteStructure = %#v, want key %s row %d child %d footer %v", err, tc.wantKey, tc.row, tc.child, tc.footer)
			}
		})
	}
}

// 子項目・フッターのサイトマップのエラーは、どの項目のどの子項目かを日本語と英語で示す。
func TestNestedMenuErrorMessages(t *testing.T) {
	bad := MenuItem{Label: "x", Type: MenuItemURL, Target: "javascript:x"}
	for _, tc := range []struct {
		st     SiteStructure
		ja, en string
	}{
		{SiteStructure{Menu: []MenuItem{{Type: MenuItemArticles}, {Label: "g", Type: MenuItemGroup, Children: []MenuItem{{Type: MenuItemArticles}, bad}}}},
			"メニューの2番目の項目の2番目の子項目: URL は", "Menu item 2, sub-item 2: Enter a URL"},
		{SiteStructure{FooterMenu: []MenuItem{{Label: "g", Type: MenuItemGroup, Children: []MenuItem{bad}}}},
			"フッターのサイトマップの1列目の1番目のリンク: URL は", "Footer sitemap column 1, link 1: Enter a URL"},
		{SiteStructure{FooterMenu: []MenuItem{{Label: "g", Type: MenuItemGroup}}},
			"フッターのサイトマップの1列目: 見出しのみの項目には、子項目", "Footer sitemap column 1: Add at least one sub-item"},
		{SiteStructure{Menu: []MenuItem{{Type: MenuItemGroup, Children: []MenuItem{{Type: MenuItemArticles}}}}},
			"メニューの1番目の項目: 見出しのみの項目には表示名を入力してください", "Menu item 1: Enter a label for a heading-only item"},
	} {
		err := validateSiteStructure(tc.st)
		var se *siteStructureError
		if !errors.As(err, &se) {
			t.Fatalf("validateSiteStructure(%+v) = %v", tc.st, err)
		}
		if !strings.Contains(err.Error(), tc.ja) || !strings.Contains(se.localize("en"), tc.en) {
			t.Errorf("messages = %q / %q, want %q / %q", err.Error(), se.localize("en"), tc.ja, tc.en)
		}
	}
}

// 子項目の無いメニューは、子項目を導入する前と同じ JSON で保存し、子項目のある項目だけ "children" を持つ。
func TestNestedMenuJSONBackwardCompatible(t *testing.T) {
	env := newPagesEnv(t, Config{})
	ctx := context.Background()
	flat := `[{"label":"ホーム","type":"url","target":"/"},{"label":"","type":"page","target":"research"}]`
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES ('site_menu', ?)`, flat); err != nil {
		t.Fatal(err)
	}
	st := env.server.loadSiteStructure(ctx)
	if len(st.Menu) != 2 || st.Menu[1].Children != nil || st.FooterMenu != nil {
		t.Fatalf("loaded = %+v", st)
	}
	if err := env.server.saveSiteStructure(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got := env.setting("site_menu"); got != flat {
		t.Errorf("re-saved flat menu = %s, want %s", got, flat)
	}
	if got := env.setting("site_footer_menu"); got != "[]" {
		t.Errorf("site_footer_menu = %s, want []", got)
	}

	st.Menu = nestedMenu()
	st.FooterMenu = nestedFooter()
	if err := env.server.saveSiteStructure(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got, want := env.setting("site_menu"), `[{"label":"研究内容","label_en":"Research","type":"group","children":[`+
		`{"label":"研究概要","type":"page","target":"research"},{"label":"サークル","type":"category","target":"circle"}]},`+
		`{"label":"","type":"page","target":"access","children":[{"label":"記事","type":"articles"},`+
		`{"label":"大学","type":"url","target":"https://www.example.ac.jp/"}]},{"label":"","type":"articles"}]`; got != want {
		t.Errorf("site_menu = %s\nwant %s", got, want)
	}
	if got := env.server.loadSiteStructure(ctx); !reflect.DeepEqual(got.Menu, nestedMenu()) || !reflect.DeepEqual(got.FooterMenu, nestedFooter()) {
		t.Errorf("reloaded = %+v / %+v", got.Menu, got.FooterMenu)
	}
}

// navSection は body の中の start から end までを返す（無ければ空）。
func navSection(body, start, end string) string {
	_, rest, ok := strings.Cut(body, start)
	if !ok {
		return ""
	}
	sec, _, _ := strings.Cut(rest, end)
	return sec
}

// footerLinks はフッターのサイトマップの見出しとリンクを「href 表示名」（見出しのみは「# 表示名」）の並びで返す。
func footerLinks(body string) []string {
	sec := navSection(body, `<nav class="footer-sitemap"`, "</nav>")
	var out []string
	for _, m := range regexp.MustCompile(`<h2 class="footer-sitemap-heading">(?:<a href="([^"]*)"[^>]*>([^<]*)</a>|([^<]*))</h2>|<li><a href="([^"]*)"[^>]*>([^<]*)</a>`).FindAllStringSubmatch(sec, -1) {
		switch {
		case m[1] != "":
			out = append(out, html.UnescapeString(m[1])+" "+html.UnescapeString(m[2]))
		case m[4] != "":
			out = append(out, "  "+html.UnescapeString(m[4])+" "+html.UnescapeString(m[5]))
		default:
			out = append(out, "# "+html.UnescapeString(m[3]))
		}
	}
	return out
}

func TestNestedMenuSavedFromFormAndRendered(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	f := structureForm(nil, "")
	addMenuRows(f, "menu", []menuRow{
		{0, "group", "", "研究内容", "Research"},
		{1, "page", "research", "研究概要", ""},
		{1, "category", "circle", "サークル", ""},
		{0, "page", "access", "", ""},
		{1, "articles", "", "記事", ""},
		{1, "url", "https://www.example.ac.jp/", "大学", ""},
		{1, "page", "", "", ""}, // 何も入れていない子の行は保存しない
		{0, "articles", "", "", ""},
	})
	addMenuRows(f, "footer", []menuRow{
		{0, "group", "", "探す", ""},
		{1, "page", "research", "", ""},
		{1, "url", "/feed.xml", "RSS", ""},
		{0, "page", "access", "連絡", ""},
	})
	expectRedirect(t, adm.postForm("/admin/settings/structure", f), "/admin/settings/structure?flash=structure_saved")
	st := env.server.loadSiteStructure(context.Background())
	if !reflect.DeepEqual(st.Menu, nestedMenu()) {
		t.Errorf("menu = %+v\nwant %+v", st.Menu, nestedMenu())
	}
	if !reflect.DeepEqual(st.FooterMenu, nestedFooter()) {
		t.Errorf("footer = %+v\nwant %+v", st.FooterMenu, nestedFooter())
	}

	// 管理画面は子項目を親の下に字下げして出し、子の行は depth=1 を送る。
	res := adm.get("/admin/settings/structure")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="menu-row-1" class="text-sm font-semibold text-neutral-800 md:sr-only">1番目の項目の子項目1</span>`,
		`name="menu_depth" value="1"`, `value="child:0"`, `value="outdent:1"`, `id="structure-footer"`,
		`id="footer-row-0" class="text-sm font-semibold text-neutral-800 md:sr-only">1列目の見出し</span>`, `value="footer-child:0"`,
		`<option value="group" selected>見出しのみ（リンクなし）</option>`)
	if n := strings.Count(res.Body, `name="menu_depth" value="1"`); n != 4 {
		t.Errorf("child rows = %d, want 4", n)
	}

	c := env.newClient()
	home := c.get("/")
	expectStatus(t, home, http.StatusOK)
	nav := navSection(home.Body, `<ul class="container site-menu">`, "</nav>")
	// 見出しのみの親は開くボタン、リンクのある親はリンクの横に開くボタン。子項目の一覧の id をボタンが指す。
	expectContains(t, nav,
		`<li class="has-submenu"><button type="button" class="submenu-toggle submenu-toggle-label" aria-expanded="false" aria-controls="site-submenu-1">研究内容<span class="submenu-caret" aria-hidden="true"></span></button>`,
		`<ul class="site-submenu" id="site-submenu-1"><li><a href="/research">研究概要</a></li><li><a href="/categories/circle">サークル</a></li></ul>`,
		`<li class="has-submenu"><a href="/access">アクセス</a><button type="button" class="submenu-toggle" aria-expanded="false" aria-controls="site-submenu-2">`,
		`<span class="visually-hidden">「アクセス」の項目を表示</span>`,
		`<ul class="site-submenu" id="site-submenu-2"><li><a href="/articles">記事</a></li><li><a href="https://www.example.ac.jp/">大学</a></li></ul>`,
		`<li><a href="/articles">記事一覧</a></li>`)
	// スマートフォンのメニューのボタン（スクリプトが hidden を外す）。
	expectContains(t, home.Body, `<button type="button" class="nav-toggle" aria-expanded="false" hidden>`, `<span class="nav-toggle-label">メニュー</span>`)
	if got, want := footerLinks(home.Body), []string{"# 探す", "  /research 研究紹介", "  /feed.xml RSS", "/access 連絡"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("footer sitemap =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	expectContains(t, home.Body, `<nav class="footer-sitemap" aria-label="サイトマップ">`)

	// 子項目のページを表示中なら、子項目に aria-current を付け、親を強調する。
	research := c.get("/research").Body
	expectContains(t, research, `<li class="has-submenu is-current-parent"><button type="button" class="submenu-toggle submenu-toggle-label"`,
		`<li><a href="/research" aria-current="page">研究概要</a></li>`, `<li><a href="/research" aria-current="page">研究紹介</a></li>`)
	access := c.get("/access").Body
	expectContains(t, access, `<li class="has-submenu"><a href="/access" aria-current="page">アクセス</a>`, `<a href="/access" aria-current="page">連絡</a>`)

	// 英語の表示では英語の表示名（見出しのみも LabelEn）を使い、読み上げの文も英語にする。
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES ('multilingual_enabled', '1') ON CONFLICT(key) DO UPDATE SET value = '1'`); err != nil {
		t.Fatal(err)
	}
	en := c.get("/en/").Body
	expectContains(t, en, `aria-controls="site-submenu-1">Research<span class="submenu-caret"`, `<span class="visually-hidden">Show submenu of `,
		`<span class="nav-toggle-label">Menu</span>`, `<nav class="footer-sitemap" aria-label="Sitemap">`)
}

func TestNestedMenuRowOperationsWithoutJavaScript(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	labels := func(body, prefix string) []string {
		var out []string
		re := regexp.MustCompile(`name="` + prefix + `_depth" value="([01])">(?s:.*?)name="` + prefix + `_label" type="text" maxlength="40" value="([^"]*)"`)
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if m[1] == "1" {
				out = append(out, "-"+m[2])
			} else {
				out = append(out, m[2])
			}
		}
		return out
	}
	rows := []menuRow{
		{0, "group", "", "A", ""},
		{1, "articles", "", "a1", ""},
		{1, "articles", "", "a2", ""},
		{0, "articles", "", "B", ""},
		{0, "articles", "", "C", ""},
	}
	post := func(prefix, op string) *response {
		f := structureForm(nil, "")
		addMenuRows(f, prefix, rows)
		f.Set("op", op)
		res := adm.postForm("/admin/settings/structure", f)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `id="structure-unsaved"`)
		return res
	}
	for _, prefix := range []string{"menu", "footer"} {
		opPrefix := ""
		if prefix == "footer" {
			opPrefix = footerOpPrefix
		}
		for _, tc := range []struct {
			op   string
			want []string
		}{
			{"child:0", []string{"A", "-a1", "-a2", "-", "B", "C"}},
			{"child:3", []string{"A", "-a1", "-a2", "B", "-", "C"}},
			{"up:2", []string{"A", "-a2", "-a1", "B", "C"}},
			{"down:1", []string{"A", "-a2", "-a1", "B", "C"}},
			{"up:1", []string{"A", "-a1", "-a2", "B", "C"}},
			{"delete:2", []string{"A", "-a1", "B", "C"}},
			{"outdent:1", []string{"A", "-a2", "a1", "B", "C"}},
			{"indent:3", []string{"A", "-a1", "-a2", "-B", "C"}},
			{"indent:4", []string{"A", "-a1", "-a2", "B", "-C"}},
			{"up:3", []string{"B", "A", "-a1", "-a2", "C"}},
			{"down:0", []string{"B", "A", "-a1", "-a2", "C"}},
			{"delete:0", []string{"B", "C"}},
			{"indent:0", []string{"A", "-a1", "-a2", "B", "C"}},
			{"delete:99", []string{"A", "-a1", "-a2", "B", "C"}},
		} {
			if got := labels(post(prefix, opPrefix+tc.op).Body, prefix); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("%s op %s: rows = %v, want %v", prefix, tc.op, got, tc.want)
			}
		}
		// 最初・最後の兄弟の「上へ」「下へ」は押せない。見出しのみの項目は「上の項目に入れる」を出さない。
		body := post(prefix, opPrefix+"child:99").Body
		expectContains(t, body, `value="`+opPrefix+`up:0" disabled`, `value="`+opPrefix+`up:1" disabled`, `value="`+opPrefix+`down:2" disabled`,
			`value="`+opPrefix+`down:4" disabled`, `value="`+opPrefix+`indent:3"`)
		expectNotContainsAny(t, prefix+" row ops", body, `value="`+opPrefix+`indent:0"`)
	}
	// フッターの操作はメニューを変えず、メニューの操作はフッターを変えない。
	f := structureForm([][3]string{{"articles", "", "M"}}, "")
	addMenuRows(f, "footer", rows)
	f.Set("op", footerOpPrefix+"delete:0")
	body := adm.postForm("/admin/settings/structure", f).Body
	if got := labels(body, "menu"); strings.Join(got, ",") != "M" {
		t.Errorf("menu after footer op = %v", got)
	}
	if got := labels(body, "footer"); strings.Join(got, ",") != "B,C" {
		t.Errorf("footer after footer op = %v", got)
	}
	// 子項目は最大数まで。最大数に達したら「ドロップダウンに項目を追加」を出さない。
	full := []menuRow{{0, "group", "", "A", ""}}
	for range maxMenuChildren {
		full = append(full, menuRow{1, "articles", "", "x", ""})
	}
	f = structureForm(nil, "")
	addMenuRows(f, "menu", full)
	f.Set("op", "child:0")
	body = adm.postForm("/admin/settings/structure", f).Body
	if n := strings.Count(body, `name="menu_depth" value="1"`); n != maxMenuChildren {
		t.Errorf("children after add at max = %d", n)
	}
	expectNotContainsAny(t, "full children", body, `value="child:0"`)
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN ('site_menu', 'site_footer_menu')`); n != 0 {
		t.Errorf("row operations saved %d settings", n)
	}
}

func TestNestedMenuSaveRejectsInvalidInput(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	for _, tc := range []struct {
		name   string
		prefix string
		rows   []menuRow
		want   string
	}{
		{"empty group", "menu", []menuRow{{0, "articles", "", "", ""}, {0, "group", "", "研究", ""}}, "メニューの2番目の項目: 見出しのみの項目には、子項目"},
		{"bad child url", "menu", []menuRow{{0, "group", "", "研究", ""}, {1, "url", "javascript:alert(1)", "x", ""}}, "メニューの1番目の項目の1番目の子項目: URL は"},
		{"footer group without label", "footer", []menuRow{{0, "group", "", "", ""}, {1, "articles", "", "", ""}}, "フッターのサイトマップの1列目: 見出しのみの項目には表示名"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := structureForm(nil, "")
			addMenuRows(f, tc.prefix, tc.rows)
			res := adm.postForm("/admin/settings/structure", f)
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, res.Body, `data-flash-error="true"`, html.EscapeString(tc.want))
		})
	}
	// 子項目を最大数より多く送っても保存しない。
	f := structureForm(nil, "")
	rows := []menuRow{{0, "group", "", "研究", ""}}
	for range maxMenuChildren + 1 {
		rows = append(rows, menuRow{1, "articles", "", "", ""})
	}
	addMenuRows(f, "menu", rows)
	res := adm.postForm("/admin/settings/structure", f)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, html.EscapeString("子項目は1つの項目に最大10項目です"))
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN ('site_menu', 'site_footer_menu')`); n != 0 {
		t.Errorf("invalid input saved %d settings", n)
	}
}

// 改ざんされた保存値（孫項目・子の見出しのみ・javascript: の子項目・参照切れ）は公開側に出さない。
func TestNestedMenuTamperedValuesNotRendered(t *testing.T) {
	env := newPagesEnv(t, Config{})
	for k, v := range map[string]string{
		"site_menu": `[{"label":"G","type":"group","children":[` +
			`{"label":"深い","type":"articles","children":[{"label":"孫","type":"url","target":"/deep"}]},` +
			`{"label":"子の見出し","type":"group","children":[{"label":"y","type":"articles"}]},` +
			`{"label":"悪い","type":"url","target":"javascript:alert(1)"},` +
			`{"label":"良い","type":"page","target":"research"}]},` +
			`{"label":"空","type":"group","children":[{"type":"page","target":"gone"}]},` +
			`{"label":"消えた親","type":"page","target":"gone","children":[{"type":"page","target":"access"}]},` +
			`{"label":"","type":"page","target":"gone","children":[{"type":"page","target":"access"}]},` +
			`{"label":"リンクだけ","type":"page","target":"access","children":[{"type":"category","target":"gone"}]}]`,
		"site_footer_menu": `{"broken"`,
	} {
		if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	body := env.newClient().get("/articles").Body
	nav := navSection(body, `<ul class="container site-menu">`, "</nav>")
	expectContains(t, nav, `<ul class="site-submenu" id="site-submenu-1"><li><a href="/research">良い</a></li></ul>`,
		// 参照切れの親でも、表示名があり子項目が残るなら見出しとして出す。
		`aria-controls="site-submenu-3">消えた親<span class="submenu-caret"`,
		// 子項目がすべて参照切れなら、ふつうのリンクとして出す。
		`<li><a href="/access">リンクだけ</a></li>`)
	expectNotContainsAny(t, "tampered nested menu", body, "javascript:", "/deep", "深い", "子の見出し", ">空<", "site-submenu-2", "site-submenu-4")
	if strings.Contains(body, "footer-sitemap") {
		t.Errorf("broken footer sitemap rendered")
	}
	if got := env.server.loadSiteStructure(context.Background()).FooterMenu; got != nil {
		t.Errorf("broken footer = %+v, want nil", got)
	}
}

// 静的な書き出しにも、ドロップダウンとフッターのサイトマップを出す。
func TestExportWritesNestedMenuAndFooterSitemap(t *testing.T) {
	env := newFrontEnv(t)
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{Menu: nestedMenu(), FooterMenu: nestedFooter()}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "research", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	expectContains(t, string(b), `<li class="has-submenu is-current-parent">`, `<li><a href="/research" aria-current="page">研究概要</a></li>`,
		`class="nav-toggle"`, `<nav class="footer-sitemap" aria-label="サイトマップ">`, `<a href="/feed.xml">RSS</a>`)
}

func TestSyncAPISyncSettingsNestedMenu(t *testing.T) {
	env := newSyncEnv(t)
	adminToken := env.token("chief-admin", auth.RoleAdmin)
	menu, footer := nestedMenu(), nestedFooter()
	rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{SiteMenu: &menu, SiteFooterMenu: &footer}})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncSettingsResponse](t, rec)
	if !reflect.DeepEqual(res.Settings.SiteMenu, menu) || res.Settings.SiteFooterMenu == nil || !reflect.DeepEqual(*res.Settings.SiteFooterMenu, footer) {
		t.Errorf("response = %+v / %v", res.Settings.SiteMenu, res.Settings.SiteFooterMenu)
	}
	got := env.server.loadSiteStructure(t.Context())
	if !reflect.DeepEqual(got.Menu, menu) || !reflect.DeepEqual(got.FooterMenu, footer) {
		t.Errorf("saved = %+v / %+v", got.Menu, got.FooterMenu)
	}

	// フッターのサイトマップを知らない古い PC の反映（site_footer_menu なし）は、公開サーバーのサイトマップを消さない。
	flat := []MenuItem{{Type: MenuItemArticles}}
	rec = env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{SiteMenu: &flat}})
	expectSyncStatus(t, rec, http.StatusOK)
	if got := env.server.loadSiteStructure(t.Context()); !reflect.DeepEqual(got.Menu, flat) || !reflect.DeepEqual(got.FooterMenu, footer) {
		t.Errorf("after old client sync = %+v / %+v", got.Menu, got.FooterMenu)
	}
	// 空の配列はサイトマップを消す。応答は空でも空の配列を返す（知っていることを伝える）。
	rec = env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{SiteFooterMenu: &[]MenuItem{}}})
	expectSyncStatus(t, rec, http.StatusOK)
	res = decodeSync[SyncSettingsResponse](t, rec)
	if res.Settings.SiteFooterMenu == nil || len(*res.Settings.SiteFooterMenu) != 0 {
		t.Errorf("response footer = %v, want empty array", res.Settings.SiteFooterMenu)
	}
	if got := env.server.loadSiteStructure(t.Context()).FooterMenu; got != nil {
		t.Errorf("footer after clearing = %+v", got)
	}

	// 不正な子項目・サイトマップは何も変えずに 422 で断る。
	for name, tc := range map[string]struct {
		settings SyncSettings
		want     string
	}{
		"grandchild": {SyncSettings{SiteMenu: &[]MenuItem{{Label: "g", Type: MenuItemGroup, Children: []MenuItem{
			{Type: MenuItemArticles, Children: []MenuItem{{Type: MenuItemArticles}}}}}}}, "メニューの1番目の項目の1番目の子項目: 子項目の下には"},
		"empty group": {SyncSettings{SiteMenu: &[]MenuItem{{Label: "g", Type: MenuItemGroup}}}, "メニューの1番目の項目: 見出しのみ"},
		"footer url": {SyncSettings{SiteFooterMenu: &[]MenuItem{{Label: "g", Type: MenuItemGroup, Children: []MenuItem{
			{Label: "x", Type: MenuItemURL, Target: "javascript:x"}}}}}, "フッターのサイトマップの1列目の1番目のリンク"},
	} {
		settings := tc.settings
		rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &settings})
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
		if msg := syncErrorMessage(t, rec); !strings.HasPrefix(msg, "サイト構成: ") || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: message = %q, want %q", name, msg, tc.want)
		}
	}
	if got := env.server.loadSiteStructure(t.Context()); !reflect.DeepEqual(got.Menu, flat) || got.FooterMenu != nil {
		t.Errorf("structure after rejected syncs = %+v / %+v", got.Menu, got.FooterMenu)
	}
}

// PC のドロップダウンとフッターのサイトマップを公開サーバーへ反映する。古い公開サーバーには更新を促す。
func TestRemoteSettingsPushNestedMenu(t *testing.T) {
	env := newRemoteEnv(t)
	local := SiteStructure{Menu: nestedMenu(), FooterMenu: nestedFooter(), TopGalleryLimit: 6}
	if err := env.local.server.saveSiteStructure(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	env.configure("root", auth.RoleAdmin)
	var out struct {
		Message string `json:"message"`
	}
	expectStatus(t, env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if strings.Contains(out.Message, "古い") {
		t.Errorf("message = %q", out.Message)
	}
	got := env.remote.server.loadSiteStructure(t.Context())
	if !reflect.DeepEqual(got.Menu, local.Menu) || !reflect.DeepEqual(got.FooterMenu, local.FooterMenu) {
		t.Errorf("remote = %+v / %+v", got.Menu, got.FooterMenu)
	}

	point := func(h http.HandlerFunc) func() {
		srv := httptest.NewServer(h)
		cfg := env.local.server.loadRemoteConfig()
		orig := cfg.URL
		cfg.URL = srv.URL
		if err := env.local.server.saveRemoteConfig(cfg); err != nil {
			t.Fatal(err)
		}
		return func() {
			srv.Close()
			cfg.URL = orig
			_ = env.local.server.saveRemoteConfig(cfg)
		}
	}
	t.Run("old remote without the footer sitemap", func(t *testing.T) {
		var sent SyncRequest
		done := point(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&sent)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"action":"sync_settings","settings":{"site_menu":[],"front_cards":[],"top_gallery_layout":"justified","show_author":false}}`))
		})
		defer done()
		var out struct {
			Message string `json:"message"`
		}
		env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, &out)
		if !strings.Contains(out.Message, "メニューのドロップダウン（子項目）とフッターのサイトマップは反映されていません") {
			t.Errorf("message = %q", out.Message)
		}
		if sent.Settings == nil || sent.Settings.SiteFooterMenu == nil || !reflect.DeepEqual(*sent.Settings.SiteFooterMenu, local.FooterMenu) ||
			sent.Settings.SiteMenu == nil || !reflect.DeepEqual(*sent.Settings.SiteMenu, local.Menu) {
			t.Errorf("sent = %+v", sent.Settings)
		}
	})
	t.Run("old remote rejecting heading-only items", func(t *testing.T) {
		done := point(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"サイト構成: メニューの1番目の項目: 種類「group」は使えません"}`))
		})
		defer done()
		var out struct {
			Message string `json:"message"`
		}
		env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, &out)
		if !strings.Contains(out.Message, "種類「group」は使えません") || !strings.Contains(out.Message, remoteNestedMenuRejectedNote) {
			t.Errorf("message = %q", out.Message)
		}
	})
}
