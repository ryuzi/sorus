package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/theme"
)

func frontGallery(st SiteStructure) FrontSection {
	for _, section := range st.FrontSections {
		if section.Type == FrontSectionGallery {
			return section
		}
	}
	return FrontSection{}
}

func setGalleryFormField(form url.Values, field, value string) {
	types := form["section_type"]
	index := -1
	for i, typ := range types {
		if typ == string(FrontSectionGallery) {
			index = i
			break
		}
	}
	if index < 0 {
		form.Add("section_type", string(FrontSectionGallery))
		types = form["section_type"]
		index = len(types) - 1
	}
	for _, key := range []string{"pages", "tag", "title", "limit", "layout", "link_url", "link_label", "article_tag"} {
		name := "section_" + key
		for len(form[name]) < len(types) {
			form.Add(name, "")
		}
	}
	form["section_"+field][index] = value
}

func TestValidateSiteStructure(t *testing.T) {
	items := func(n int) []MenuItem {
		m := make([]MenuItem, n)
		for i := range m {
			m[i] = MenuItem{Type: MenuItemArticles}
		}
		return m
	}
	slugs := func(n int) []string {
		s := make([]string, n)
		for i := range s {
			s[i] = fmt.Sprintf("page-%d", i)
		}
		return s
	}
	for _, tc := range []struct {
		name    string
		st      SiteStructure
		wantKey string // 空なら有効
	}{
		{"empty", SiteStructure{}, ""},
		{"gallery settings", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Tag: "campus", Limit: 16, Title: "Photos"}}}, ""},
		{"gallery limit too large", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Limit: maxTopGalleryLimit + 1}}}, "structure_err_gallery_limit"},
		{"gallery tag control", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Tag: "bad\ntag"}}}, "structure_err_gallery_tag"},
		{"all types", SiteStructure{Menu: []MenuItem{
			{Type: MenuItemPage, Target: "research"},
			{Label: "サークル", Type: MenuItemCategory, Target: "circle"},
			{Type: MenuItemArticles},
			{Label: "大学", Type: MenuItemURL, Target: "https://www.example.ac.jp/about?x=1#top"},
			{Label: "検索", Type: MenuItemURL, Target: "/search?q=研究"},
		}, FrontPage: "research", FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: []string{"members", "メンバー"}}}}, ""},
		{"max menu", SiteStructure{Menu: items(maxMenuItems)}, ""},
		{"too many menu items", SiteStructure{Menu: items(maxMenuItems + 1)}, "structure_err_menu_count"},
		{"max cards", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: slugs(maxFrontCards)}}}, ""},
		{"too many cards", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: slugs(maxFrontCards + 1)}}}, "structure_err_cards_count"},
		{"empty card", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: []string{""}}}}, "structure_err_target_required"},
		{"unknown type", SiteStructure{Menu: []MenuItem{{Label: "x", Type: "script", Target: "x"}}}, "structure_err_type"},
		{"empty type", SiteStructure{Menu: []MenuItem{{Label: "x"}}}, "structure_err_type"},
		{"page without target", SiteStructure{Menu: []MenuItem{{Type: MenuItemPage}}}, "structure_err_target_required"},
		{"category without target", SiteStructure{Menu: []MenuItem{{Type: MenuItemCategory}}}, "structure_err_target_required"},
		{"articles with target", SiteStructure{Menu: []MenuItem{{Type: MenuItemArticles, Target: "x"}}}, "structure_err_articles_target"},
		{"url without label", SiteStructure{Menu: []MenuItem{{Type: MenuItemURL, Target: "https://example.com/"}}}, "structure_err_label_required"},
		{"url blank label", SiteStructure{Menu: []MenuItem{{Label: "  ", Type: MenuItemURL, Target: "/x"}}}, "structure_err_label_required"},
		{"javascript url", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "javascript:alert(1)"}}}, "structure_err_url"},
		{"protocol-relative url", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "//evil.example"}}}, "structure_err_url"},
		{"backslash url", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "/\\evil.example"}}}, "structure_err_url"},
		{"http url", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "http://example.com/"}}}, "structure_err_url"},
		{"data url", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "data:text/html,x"}}}, "structure_err_url"},
		{"https without host", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "https://"}}}, "structure_err_url"},
		{"relative url", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "about"}}}, "structure_err_url"},
		{"url with space", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "/a b"}}}, "structure_err_url"},
		{"label max", SiteStructure{Menu: []MenuItem{{Label: strings.Repeat("研", maxMenuLabelRunes), Type: MenuItemArticles}}}, ""},
		{"label too long", SiteStructure{Menu: []MenuItem{{Label: strings.Repeat("研", maxMenuLabelRunes+1), Type: MenuItemArticles}}}, "structure_err_label_long"},
		{"label control", SiteStructure{Menu: []MenuItem{{Label: "a\nb", Type: MenuItemArticles}}}, "structure_err_control"},
		{"target control", SiteStructure{Menu: []MenuItem{{Type: MenuItemPage, Target: "a\x00b"}}}, "structure_err_control"},
		{"url control", SiteStructure{Menu: []MenuItem{{Label: "x", Type: MenuItemURL, Target: "/a\tb"}}}, "structure_err_url"},
		{"target slash", SiteStructure{Menu: []MenuItem{{Type: MenuItemPage, Target: "a/b"}}}, "structure_err_slug"},
		{"front page slash", SiteStructure{FrontPage: "../x"}, "structure_err_slug"},
		{"front card control", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: []string{"a\rb"}}}}, "structure_err_control"},
		{"long slug", SiteStructure{FrontPage: strings.Repeat("a", maxStructureSlugRunes+1)}, "structure_err_slug_long"},
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
			if !errors.As(err, &se) || se.Key != tc.wantKey {
				t.Fatalf("validateSiteStructure = %v, want %s", err, tc.wantKey)
			}
		})
	}

	// メニューの行のエラーは何番目の行かを示す（日本語と英語）。
	err := validateSiteStructure(SiteStructure{Menu: []MenuItem{{Type: MenuItemArticles}, {Label: "x", Type: MenuItemURL, Target: "javascript:x"}}})
	var se *siteStructureError
	if !errors.As(err, &se) || se.Row != 2 {
		t.Fatalf("err = %#v, want row 2", err)
	}
	if !strings.Contains(err.Error(), "メニューの2番目の項目") || !strings.Contains(se.localize("en"), "Menu item 2") {
		t.Errorf("messages = %q / %q", err.Error(), se.localize("en"))
	}
}

// structureForm はサイト構成のフォームの値。rows は [種類, 参照先, 表示名] の並び。
func structureForm(rows [][3]string, front string, cards ...string) url.Values {
	f := url.Values{"front_page": {front}}
	for _, r := range rows {
		f.Add("menu_type", r[0])
		page, cat, u := "", "", ""
		switch MenuItemType(r[0]) {
		case MenuItemPage:
			page = r[1]
		case MenuItemCategory:
			cat = r[1]
		case MenuItemURL:
			u = r[1]
		}
		f.Add("menu_page", page)
		f.Add("menu_category", cat)
		f.Add("menu_url", u)
		f.Add("menu_label", r[2])
	}
	if front != "" {
		f.Add("section_type", string(FrontSectionHero))
		f.Add("section_pages", "")
	}
	if len(cards) > 0 {
		f.Add("section_type", string(FrontSectionCards))
		f.Add("section_pages", strings.Join(cards, ", "))
	}
	if front != "" || len(cards) == 0 {
		f.Add("section_type", string(FrontSectionArticleList))
		f.Add("section_pages", "")
	}
	for i := 0; i < len(f["section_type"]); i++ {
		for _, key := range []string{"tag", "title", "limit", "layout", "link_url", "link_label", "article_tag"} {
			f.Add("section_"+key, "")
		}
	}
	return f
}

func TestStructureSettingsForbiddenForNonAdmins(t *testing.T) {
	env := newPagesEnv(t, Config{})
	form := structureForm([][3]string{{"page", "research", ""}}, "research")
	for _, u := range []testUser{studentA, chief} {
		t.Run(string(u.Role), func(t *testing.T) {
			c := env.loggedIn(u)
			expectStatus(t, c.get("/admin/settings/structure"), http.StatusForbidden)
			expectStatus(t, c.postForm("/admin/settings/structure", form), http.StatusForbidden)
			// 管理者以外のナビには出さない。
			expectNotContainsAny(t, "admin nav", c.get("/admin").Body, `href="/admin/settings/structure"`)
		})
	}
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN ('site_menu', 'front_page', 'front_cards')`); n != 0 {
		t.Errorf("non-admin saved %d structure settings", n)
	}
	expectRedirect(t, env.newClient().get("/admin/settings/structure"), "/admin/login")
	// CSRF トークンの無い送信は拒否する。
	adm := env.loggedIn(admin)
	if res := adm.postRaw("/admin/settings/structure", form); res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusBadRequest {
		t.Errorf("POST without CSRF: status %d", res.StatusCode)
	}
}

// menuLinks は公開ページのメニュー（.site-menu）のリンクを「href 表示名」の並びで返す。現在の項目には末尾に " *" を付ける。
func menuLinks(t *testing.T, body string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<ul class="container site-menu">`)
	if !ok {
		return nil
	}
	ul, _, _ := strings.Cut(rest, "</ul>")
	var out []string
	for _, m := range regexp.MustCompile(`<a href="([^"]*)"( aria-current="page")?>([^<]*)</a>`).FindAllStringSubmatch(ul, -1) {
		s := html.UnescapeString(m[1]) + " " + html.UnescapeString(m[3])
		if m[2] != "" {
			s += " *"
		}
		out = append(out, s)
	}
	return out
}

func expectMenu(t *testing.T, label, body string, want ...string) {
	t.Helper()
	if got := menuLinks(t, body); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s menu =\n%s\nwant\n%s", label, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStructureMenuSavedInOrderAndRendered(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)

	res := adm.get("/admin/settings/structure")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "サイト構成", `id="structure-menu-empty"`, `<option value="">記事の一覧（既定）</option>`,
		`<option value="research">研究紹介（/research）</option>`)
	// 未公開のページは選択肢に出さない。
	expectNotContainsAny(t, "structure screen", res.Body, `value="draft-page"`, `id="structure-warnings"`)

	member := "メンバー"
	form := structureForm([][3]string{
		{"url", "/", "ホーム"},
		{"page", "research", ""},
		{"page", member, "メンバー"},
		{"page", "draft-page", "準備中"},
		{"category", "circle", "サークル活動"},
		{"category", "gone", ""},
		{"articles", "", ""},
		{"url", "https://www.example.ac.jp/", "大学"},
	}, "")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")

	// 保存した順のまま JSON で保存する。
	if got, want := env.setting("site_menu"), `[{"label":"ホーム","type":"url","target":"/"},{"label":"","type":"page","target":"research"},`+
		`{"label":"メンバー","type":"page","target":"メンバー"},{"label":"準備中","type":"page","target":"draft-page"},`+
		`{"label":"サークル活動","type":"category","target":"circle"},{"label":"","type":"category","target":"gone"},`+
		`{"label":"","type":"articles"},{"label":"大学","type":"url","target":"https://www.example.ac.jp/"}]`; got != want {
		t.Errorf("site_menu = %s\nwant %s", got, want)
	}

	// 保存後のトーストは CDN の遅延を案内する。参照切れは警告に出し、選択肢に残す（保存し直しても失われない）。
	res = adm.get("/admin/settings/structure?flash=structure_saved")
	expectContains(t, res.Body, `data-flash="サイト構成を保存しました。CDN を使っている場合、公開サイトへの反映には最大1時間かかります"`,
		`id="structure-warnings"`,
		html.EscapeString("メニュー「準備中」のページ「draft-page」が見つからないか、公開されていません"),
		html.EscapeString("メニュー「6」のカテゴリ「gone」が見つかりません"),
		`<option value="draft-page" selected>draft-page（見つからないか未公開）</option>`,
		`<option value="gone" selected>gone（見つからないか未公開）</option>`,
		`<option value="research" selected>研究紹介（/research）</option>`)

	c := env.newClient()
	home := c.get("/")
	expectStatus(t, home, http.StatusOK)
	// 参照切れ（未公開のページ・無いカテゴリ）は出さず、表示名が空ならページ名・カテゴリ名を使う。
	wantMenu := []string{"/ ホーム", "/research 研究紹介", "/" + url.PathEscape(member) + " メンバー",
		"/categories/circle サークル活動", "/articles 記事一覧", "https://www.example.ac.jp/ 大学"}
	withCurrent := func(i int) []string {
		m := append([]string(nil), wantMenu...)
		m[i] += " *"
		return m
	}
	expectMenu(t, "/", home.Body, withCurrent(0)...)
	// メニューは既存の <nav class="category-nav"> の場所に出し、カテゴリの一覧は出さない。
	expectContains(t, home.Body, `<nav class="category-nav" aria-label="メニュー">`)
	expectNotContainsAny(t, "/", home.Body, `aria-label="カテゴリ"`, "draft-page", "/categories/gone")

	expectMenu(t, "/research", c.get("/research").Body, withCurrent(1)...)
	expectMenu(t, "/メンバー", c.get("/"+url.PathEscape(member)).Body, withCurrent(2)...)
	expectMenu(t, "/categories/circle", c.get("/categories/circle").Body, withCurrent(3)...)
	expectMenu(t, "/articles", c.get("/articles").Body, withCurrent(4)...)
	expectMenu(t, "/search", c.get("/search?q=x").Body, wantMenu...)
	expectMenu(t, "article", c.get("/articles/keion-live").Body, wantMenu...)

	// ページを公開すると、保存し直さなくてもメニューに出る。
	if _, err := env.db.Exec(`UPDATE articles SET status = ?, published_at = ? WHERE slug = 'draft-page'`,
		article.StatusPublished, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := menuLinks(t, c.get("/").Body)
	if len(got) != 7 || got[3] != "/draft-page 準備中" {
		t.Errorf("menu after publishing draft-page = %v", got)
	}
}

func TestStructureRowOperationsWithoutJavaScript(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	rows := [][3]string{{"page", "research", "研究"}, {"articles", "", "記事"}, {"url", "/search", "検索"}}

	order := func(body string) []string {
		var labels []string
		for _, m := range regexp.MustCompile(`name="menu_label" type="text" maxlength="40" value="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			labels = append(labels, m[1])
		}
		return labels
	}
	post := func(op string) *response {
		f := structureForm(rows, "")
		f.Set("op", op)
		res := adm.postForm("/admin/settings/structure", f)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `id="structure-unsaved" class="notice notice-warn`, `id="structure-unsaved-save" class="notice notice-warn`)
		return res
	}
	for _, tc := range []struct {
		op   string
		want []string
	}{
		{"up:1", []string{"記事", "研究", "検索"}},
		{"down:0", []string{"記事", "研究", "検索"}},
		{"down:2", []string{"研究", "記事", "検索"}},
		{"up:0", []string{"研究", "記事", "検索"}},
		{"delete:1", []string{"研究", "検索"}},
		{"add", []string{"研究", "記事", "検索", ""}},
		{"delete:9", []string{"研究", "記事", "検索"}},
	} {
		if got := order(post(tc.op).Body); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("op %s: rows = %v, want %v", tc.op, got, tc.want)
		}
	}
	// 種類とリンク先も行と一緒に動き、最初の行の「上へ」と最後の行の「下へ」は押せない。
	res := post("up:2")
	expectContains(t, res.Body, `<option value="url" selected>URL</option>`, `name="menu_url" type="text" inputmode="url" value="/search"`,
		`value="up:0" disabled`, `value="down:2" disabled`)
	// 編集しただけでは保存しない。
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key = 'site_menu'`); n != 0 {
		t.Errorf("row operation saved site_menu")
	}

	// 最大数に達したら行を追加できない。
	full := make([][3]string, maxMenuItems)
	for i := range full {
		full[i] = [3]string{"articles", "", fmt.Sprint(i)}
	}
	f := structureForm(full, "")
	f.Set("op", "add")
	res = adm.postForm("/admin/settings/structure", f)
	if got := len(order(res.Body)); got != maxMenuItems {
		t.Errorf("rows after add at max = %d", got)
	}
	expectNotContainsAny(t, "full menu", res.Body, `id="structure-add-row"`)

	// 何も入力していない行は保存時に除く。
	f = structureForm([][3]string{{"page", "", ""}, {"articles", "", ""}}, "")
	expectRedirect(t, adm.postForm("/admin/settings/structure", f), "/admin/settings/structure?flash=structure_saved")
	if got := env.setting("site_menu"); got != `[{"label":"","type":"articles"}]` {
		t.Errorf("site_menu = %s", got)
	}
}

func TestStructureSaveRejectsInvalidInput(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"javascript url", structureForm([][3]string{{"url", "javascript:alert(1)", "x"}}, ""), "URL は https:// で始まる"},
		{"protocol-relative", structureForm([][3]string{{"articles", "", ""}, {"url", "//evil.example", "x"}}, ""), "メニューの2番目の項目"},
		{"url without label", structureForm([][3]string{{"url", "/search", ""}}, ""), "URL の項目には表示名を入力してください"},
		{"unknown type", structureForm([][3]string{{"script", "x", "x"}}, ""), "種類「script」は使えません"},
		{"too many", structureForm(make([][3]string, maxMenuItems+1), ""), "メニューは最大8項目です"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "too many" {
				tc.form.Del("menu_type")
				for range maxMenuItems + 1 {
					tc.form.Add("menu_type", "articles")
				}
			}
			res := adm.postForm("/admin/settings/structure", tc.form)
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, res.Body, `data-flash-error="true"`, html.EscapeString(tc.want))
		})
	}
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN ('site_menu', 'front_page', 'front_cards')`); n != 0 {
		t.Errorf("invalid input saved %d settings", n)
	}

	// 英語の管理画面では英語で知らせる。
	adm.useEnglish()
	res := adm.postForm("/admin/settings/structure", structureForm([][3]string{{"url", "javascript:x", "x"}}, ""))
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "Menu item 1: Enter a URL starting with https://")
}

func TestStructureSettingsScreenEnglish(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	adm.useEnglish()
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		Menu: []MenuItem{{Type: MenuItemPage, Target: "nope"}}, FrontPage: "draft-page",
	}); err != nil {
		t.Fatal(err)
	}
	res := adm.get("/admin/settings/structure")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<html lang="en">`, "Site structure", "Menu", "Front page", "Article list (default)", "Add row", "Save",
		html.EscapeString(`The page "nope" of the menu item "1" is missing or unpublished`),
		html.EscapeString(`The page "draft-page" chosen for the front page is missing or unpublished`),
		`id="nav-settings" class="rounded-md px-2 py-1.5 hover:bg-neutral-100" aria-current="page">Settings</a>`,
		`href="/admin/settings/theme" aria-current="page">Appearance</a>`,
		`href="/admin/publish">Publishing</a>`)
	expectNotContainsAny(t, "english structure screen", res.Body, "サイト構成を", "記事の一覧（既定）")
}

func TestLoadSiteStructureToleratesBrokenSettings(t *testing.T) {
	env := newPagesEnv(t, Config{})
	for k, v := range map[string]string{
		"site_menu": `{"not": "an array"`, "front_page": " research ", "front_cards": `"research"`,
	} {
		if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	st := env.server.loadSiteStructure(context.Background())
	if st.Menu != nil || len(st.FrontSections) != 2 || st.FrontSections[0].Type != FrontSectionHero || st.FrontSections[1].Type != FrontSectionArticleList || st.FrontPage != "research" {
		t.Errorf("loadSiteStructure = %#v", st)
	}
	// 壊れたメニューはカテゴリの一覧に戻る。
	expectContains(t, env.newClient().get("/articles").Body, `aria-label="カテゴリ"`)

	// 改ざんされた保存値（javascript: の URL・//host・不明な種類）は公開側に出さない。
	if _, err := env.db.Exec(`UPDATE settings SET value = ? WHERE key = 'site_menu'`,
		`[{"label":"x","type":"url","target":"javascript:alert(1)"},{"label":"y","type":"url","target":"//evil.example"},`+
			`{"label":"z","type":"evil","target":"q"},{"label":"","type":"articles"}]`); err != nil {
		t.Fatal(err)
	}
	body := env.newClient().get("/articles").Body
	expectMenu(t, "tampered", body, "/articles 記事一覧 *")
	expectNotContainsAny(t, "tampered", body, "javascript:", "evil.example")
}

// newFrontEnv は newPagesEnv に、アイキャッチ付きの記事を足して新着記事が7件のサイトにする。
func newFrontEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newPagesEnv(t, Config{})
	for i := 1; i <= 5; i++ {
		seedArticle(t, env.db, article.Article{Title: fmt.Sprintf("お知らせ%d", i), Slug: fmt.Sprintf("news-%d", i),
			ContentMarkdown: "お知らせの本文。", FeaturedImage: "/uploads/news.webp",
			Status: article.StatusPublished, PublishedAt: ago(time.Duration(10+i) * time.Minute)})
	}
	if _, err := env.db.Exec(`UPDATE articles SET featured_image_url = '/uploads/research.webp' WHERE slug = 'research'`); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestFrontPageComposition(t *testing.T) {
	env := newFrontEnv(t)
	adm := env.loggedIn(admin)
	member := "メンバー"
	form := structureForm(nil, "research", "access", member, "draft-page", "nope", "access")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")
	if got := env.setting("front_page"); got != "research" {
		t.Errorf("front_page = %q", got)
	}
	// 重複は除き、選んだ順に保存する。
	if got := env.setting(settingFrontSections); !strings.Contains(got, `"pages":["access","メンバー","draft-page","nope"]`) {
		t.Errorf("front_sections = %s", got)
	}
	expectContains(t, adm.get("/admin/settings/structure").Body,
		html.EscapeString("トップのカードに選んだページ「draft-page」が見つからないか"),
		html.EscapeString("トップのカードに選んだページ「nope」が見つからないか"))

	c := env.newClient()
	res := c.get("/")
	expectStatus(t, res, http.StatusOK)
	body := res.Body
	expectContains(t, body,
		`<section class="front-hero" aria-labelledby="front-hero-title">`,
		`<img src="/uploads/research.webp" alt="研究紹介" class="hero-image"`,
		`<h1 id="front-hero-title" class="article-title">研究紹介</h1>`, "量子計算を研究しています。",
		`<ul class="front-cards">`, `<h2 class="front-card-title"><a href="/access">アクセス</a></h2>`,
		`<a href="/`+url.PathEscape(member)+`">メンバー紹介</a>`, `<p class="front-card-excerpt">駅から徒歩5分。</p>`,
		`<section class="front-news" aria-labelledby="front-news-title">`, `<h2 id="front-news-title" class="page-title">新着記事</h2>`,
		`<p class="front-news-more"><a href="/articles">記事一覧へ</a></p>`,
		// <title>・description・JSON-LD はサイトのトップのまま。og:image はトップに出すページのアイキャッチ。
		"<title>テスト研究室</title>", `<meta name="description" content="研究室のサイト">`,
		`<link rel="canonical" href="https://media.example.ac.jp/">`,
		`<meta property="og:image" content="https://media.example.ac.jp/uploads/research.webp">`)
	if ld := jsonLD(t, body); ld["WebSite"] == nil || ld["WebPage"] != nil {
		t.Errorf("JSON-LD = %v, want the site's WebSite only", ld)
	}
	// カードは選んだ順で、参照切れ・未公開は出さない。
	if a, m := strings.Index(body, `href="/access">アクセス`), strings.Index(body, ">メンバー紹介</a>"); a < 0 || m < a {
		t.Errorf("cards out of order: access at %d, members at %d", a, m)
	}
	if n := strings.Count(body, `<li class="front-card`); n != 2 {
		t.Errorf("front cards = %d, want 2", n)
	}
	expectNotContainsAny(t, "front", body, "準備中", "/nope", `<h1 class="page-title">新着記事</h1>`, `class="pagination"`)

	// 新着記事は記事だけの6件（新しい順）。ページは出さない。
	_, news, _ := strings.Cut(body, `<section class="front-news"`)
	if n := strings.Count(news, `<article class="card">`); n != frontNewsCount {
		t.Errorf("front news cards = %d, want %d", n, frontNewsCount)
	}
	expectContains(t, news, "お知らせ1", "お知らせ5", "図書館の新しい自習室")
	expectNotContainsAny(t, "front news", news, "軽音楽部の新歓ライブ", "研究紹介", "アクセス")

	// 記事の一覧は /articles にあり、トップのページ送りは記事の一覧へ一時的に転送する。
	expectContains(t, c.get("/articles").Body, `<h1 class="page-title">記事一覧</h1>`, "軽音楽部の新歓ライブ")
	for _, p := range []string{"/?page=2", "/page/2"} {
		res = c.get(p)
		if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/articles/page/2" {
			t.Errorf("%s: %d %q, want 302 /articles/page/2", p, res.StatusCode, res.Header.Get("Location"))
		}
	}
	expectStatus(t, c.get("/?page=1"), http.StatusOK)
	expectStatus(t, c.get("/?page=x"), http.StatusNotFound)
	expectMovedTo(t, c.get("/?category=circle"), "/categories/circle")

	// トップに出したページの URL は / だけ。/<slug> は / へ一時的に転送する（TestFrontPageHasOneURL）。
	if res := c.get("/research"); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
		t.Errorf("/research: %d %q, want 302 /", res.StatusCode, res.Header.Get("Location"))
	}

	// トップのページを非公開にすると、今までのトップ（新着記事の一覧）に戻る。
	if _, err := env.db.Exec(`UPDATE articles SET status = ? WHERE slug = 'research'`, article.StatusDraft); err != nil {
		t.Fatal(err)
	}
	body = c.get("/").Body
	expectContains(t, body, `<h1 class="page-title">新着記事</h1>`, `<div class="card-grid">`)
	expectNotContainsAny(t, "unresolved front", body, "front-hero", "front-cards")
	expectContains(t, adm.get("/admin/settings/structure").Body, html.EscapeString("トップページに選んだページ「research」が見つからないか"))
}

func TestTopGallerySettingsPersistAndRenderOnFrontPage(t *testing.T) {
	env := newFrontEnv(t)
	env.insertMedia("gallery-one", "gallery-one.webp", "校舎", "春の校舎", "学校, 景色")
	env.insertMedia("gallery-two", "gallery-two.webp", "図書館", "図書館の入口", "学校")

	form := structureForm(nil, "research")
	setGalleryFormField(form, "tag", "学校")
	setGalleryFormField(form, "limit", "4")
	setGalleryFormField(form, "title", "キャンパスの写真")
	adm := env.loggedIn(admin)
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")

	got := env.server.loadSiteStructure(t.Context())
	if frontGallery(got).Tag != "学校" || frontGallery(got).Limit != 4 || frontGallery(got).Title != "キャンパスの写真" {
		t.Fatalf("top gallery settings = (%q, %d, %q)", frontGallery(got).Tag, frontGallery(got).Limit, frontGallery(got).Title)
	}
	settings := adm.get("/admin/settings/structure")
	expectContains(t, settings.Body, `name="section_tag"`, `value="学校"`, `value="4"`, `name="section_title"`, `value="キャンパスの写真"`)

	front := env.newClient().get("/")
	expectStatus(t, front, http.StatusOK)
	expectContains(t, front.Body,
		`<section class="front-gallery" aria-labelledby="front-gallery-title">`,
		`<h2 id="front-gallery-title" class="page-title">キャンパスの写真</h2>`,
		`class="photo-gallery gallery-layout-justified" data-gallery-layout="justified" data-gallery-tag="学校"`,
		`alt="校舎"`, `alt="図書館"`, `春の校舎`, `図書館の入口`)
	if got := strings.Count(front.Body, `class="gallery-item"`); got != 2 {
		t.Errorf("gallery image count = %d, want 2", got)
	}
}

func TestTopGalleryRendersOnDefaultIndex(t *testing.T) {
	env := newTestEnv(t)
	env.insertMedia("index-gallery", "index-gallery.webp", "校舎", "春の校舎", "学校")
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionArticleList}, {Type: FrontSectionGallery, Tag: "学校", Title: "学校の写真", Limit: 4}}}); err != nil {
		t.Fatal(err)
	}

	res := env.newClient().get("/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`<h1 class="page-title">新着記事</h1>`,
		`<section class="front-gallery" aria-labelledby="front-gallery-title">`,
		`<h2 id="front-gallery-title" class="page-title">学校の写真</h2>`,
		`data-gallery-tag="学校"`, `alt="校舎"`, `春の校舎`)
}

func TestIndexArticleTagFiltersCategoryAndLanguage(t *testing.T) {
	env := newTestEnv(t)
	categoryID := seedCategory(t, env.db, "ニュース", "news", "")
	for _, a := range []article.Article{
		{Title: "日本語ニュース", Slug: "ja-news", CategoryID: categoryID, Lang: "ja", ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(time.Hour)},
		{Title: "English news", Slug: "en-news", CategoryID: categoryID, Lang: "en", ContentMarkdown: "Body", Status: article.StatusPublished, PublishedAt: ago(time.Hour)},
		{Title: "別カテゴリ", Slug: "other-news", CategoryID: 1, Lang: "ja", ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(time.Hour)},
	} {
		seedArticle(t, env.db, a)
	}
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionArticleList, ArticleTag: "news"}}}); err != nil {
		t.Fatal(err)
	}
	ja := env.newClient().get("/")
	expectStatus(t, ja, http.StatusOK)
	expectContains(t, ja.Body, "日本語ニュース")
	expectNotContainsAny(t, "Japanese tagged index", ja.Body, "English news", "別カテゴリ")
	en := env.newClient().get("/en/")
	expectStatus(t, en, http.StatusOK)
	expectContains(t, en.Body, "English news")
	expectNotContainsAny(t, "English tagged index", en.Body, "日本語ニュース", "別カテゴリ")
}

func TestFrontPageArticleTagFiltersLanguage(t *testing.T) {
	env := newPagesEnv(t, Config{})
	categoryID := seedCategory(t, env.db, "ニュース", "news", "")
	group := "front-translation"
	if _, err := env.db.Exec(`UPDATE articles SET translation_group_id = ? WHERE slug = 'research'`, group); err != nil {
		t.Fatal(err)
	}
	seedPage(t, env.db, article.Article{Title: "Research", Slug: "research-en", Lang: "en", TranslationGroupID: group,
		ContentMarkdown: "Research page", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	for _, a := range []article.Article{
		{Title: "日本語ニュース", Slug: "ja-front-news", CategoryID: categoryID, Lang: "ja", ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(time.Hour)},
		{Title: "English news", Slug: "en-front-news", CategoryID: categoryID, Lang: "en", ContentMarkdown: "Body", Status: article.StatusPublished, PublishedAt: ago(time.Hour)},
		{Title: "別カテゴリ", Slug: "other-front-news", CategoryID: 1, Lang: "ja", ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(time.Hour)},
	} {
		seedArticle(t, env.db, a)
	}
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontPage: "research", FrontSections: []FrontSection{{Type: FrontSectionArticleList, ArticleTag: "news"}}}); err != nil {
		t.Fatal(err)
	}
	ja := env.newClient().get("/")
	expectStatus(t, ja, http.StatusOK)
	expectContains(t, ja.Body, "日本語ニュース")
	expectNotContainsAny(t, "Japanese front", ja.Body, "English news", "別カテゴリ")
	en := env.newClient().get("/en/")
	expectStatus(t, en, http.StatusOK)
	expectContains(t, en.Body, "English news")
	expectNotContainsAny(t, "English front", en.Body, "日本語ニュース", "別カテゴリ")
}

func TestIndexArticleTagPaginationUsesFilteredCount(t *testing.T) {
	env := newTestEnv(t)
	tagged := seedCategory(t, env.db, "ニュース", "news", "")
	for i := 0; i < 1; i++ {
		seedArticle(t, env.db, article.Article{Title: fmt.Sprintf("tagged-%02d", i), Slug: fmt.Sprintf("tagged-%02d", i), CategoryID: tagged,
			ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(time.Duration(i+1) * time.Minute)})
	}
	for i := 0; i < publicPerPage; i++ {
		seedArticle(t, env.db, article.Article{Title: fmt.Sprintf("other-%02d", i), Slug: fmt.Sprintf("other-%02d", i), CategoryID: 1,
			ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(time.Duration(i+50) * time.Minute)})
	}
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionArticleList, ArticleTag: "news"}}}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, env.newClient().get("/?page=2"), http.StatusNotFound)
}

func TestFrontPageMustBePage(t *testing.T) {
	env := newPagesEnv(t, Config{})
	// 記事のスラッグをトップに選んでも（同期や手入力の保存値）、ページではないため今までのトップを出す。
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{FrontPage: "keion-live"}); err != nil {
		t.Fatal(err)
	}
	body := env.newClient().get("/").Body
	expectContains(t, body, `<h1 class="page-title">新着記事</h1>`)
	expectNotContainsAny(t, "front with post", body, "front-hero")
}

// 未設定のサイトは、サイト構成の画面で空のまま保存しても、トップとナビの HTML が導入前と同じ。
func TestEmptyStructureKeepsHomeAndNav(t *testing.T) {
	env := newNoPagesEnv(t)
	compare := func(stage string) {
		t.Helper()
		c := env.newClient()
		for name, path := range map[string]string{"index.html": "/", "index_p2.html": "/?page=2", "category.html": "/categories/circle"} {
			res := c.get(path)
			expectStatus(t, res, http.StatusOK)
			want, err := os.ReadFile(filepath.Join("testdata", "no_pages", name))
			if err != nil {
				t.Fatal(err)
			}
			if got := assetVersionPattern.ReplaceAllString(res.Body, "?v=X"); got != string(want) {
				t.Errorf("%s: GET %s differs from golden %s", stage, path, name)
			}
		}
	}
	compare("never configured")
	adm := env.loggedIn(admin)
	expectRedirect(t, adm.postForm("/admin/settings/structure", structureForm(nil, "")), "/admin/settings/structure?flash=structure_saved")
	if got := env.setting("site_menu"); got != "[]" {
		t.Errorf("site_menu = %q, want []", got)
	}
	compare("saved empty")
}

func TestExportWritesFrontPageAndMenu(t *testing.T) {
	env := newFrontEnv(t)
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		Menu:      []MenuItem{{Type: MenuItemPage, Target: "research"}, {Type: MenuItemArticles}},
		FrontPage: "research", FrontSections: []FrontSection{{Type: FrontSectionHero}, {Type: FrontSectionCards, Pages: []string{"access"}}, {Type: FrontSectionArticleList}},
	}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	read := func(rel ...string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(append([]string{out}, rel...)...))
		if err != nil {
			t.Fatalf("read %v: %v", rel, err)
		}
		return string(b)
	}
	index := read("index.html")
	expectContains(t, index, `<h1 id="front-hero-title" class="article-title">研究紹介</h1>`, `href="/access">アクセス</a>`,
		`<p class="front-news-more"><a href="/articles">記事一覧へ</a></p>`)
	// トップに出したページの URL は / だけ。メニューも / を指し、トップで現在の項目になる。
	expectMenu(t, "dist/index.html", index, "/ 研究紹介 *", "/articles 記事一覧")
	expectMenu(t, "dist/articles", read("articles", "index.html"), "/ 研究紹介", "/articles 記事一覧 *")
	expectMenu(t, "dist/article", read("articles", "keion-live", "index.html"), "/ 研究紹介", "/articles 記事一覧")
	expectMenu(t, "dist/category", read("categories", "circle", "index.html"), "/ 研究紹介", "/articles 記事一覧")
	expectMenu(t, "dist/access", read("access", "index.html"), "/ 研究紹介", "/articles 記事一覧")
	// /research/index.html は公開サーバーの 302 に合わせ、/ へ転送する HTML（登録させない・正規 URL は /）。
	stub := read("research", "index.html")
	expectContains(t, stub, `<meta http-equiv="refresh" content="0; url=/">`,
		`<link rel="canonical" href="https://media.example.ac.jp/">`, `<meta name="robots" content="noindex">`,
		`<title>テスト研究室</title>`, `<a href="/">テスト研究室</a>`)
	expectNotContainsAny(t, "dist/research", stub, "front-hero", "量子計算を研究しています", "site-menu")
	read("メンバー", "index.html")
}

func TestThemePreviewShowsConfiguredStructure(t *testing.T) {
	env := newFrontEnv(t)
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		Menu: []MenuItem{{Label: "研究", Type: MenuItemPage, Target: "research"}}, FrontPage: "access",
	}); err != nil {
		t.Fatal(err)
	}
	adm := env.loggedIn(admin)
	for _, id := range []string{"modern", "tech", "news", "portal"} {
		res := adm.get("/admin/settings/theme/preview?theme=" + id)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `data-theme="`+id+`"`, `<h1 id="front-hero-title" class="article-title">アクセス</h1>`)
		expectMenu(t, "preview "+id, res.Body, "/research 研究")
	}
}

func TestSitemapListsArticlesOnlyWithPages(t *testing.T) {
	expectContains(t, newPagesEnv(t, Config{}).newClient().get("/sitemap.xml").Body, "<loc>https://media.example.ac.jp/articles</loc>")
	expectNotContainsAny(t, "no-pages sitemap", newNoPagesEnv(t).newClient().get("/sitemap.xml").Body, "/articles</loc>")
}

// トップに出したページの URL は / だけ（同じ内容を2つの URL で配信しない）。/<slug> は / へ 302 で転送し、
// メニュー・カード・サイトマップ・llms.txt も / を指す。PV は / の閲覧でそのページに数える。
func TestFrontPageHasOneURL(t *testing.T) {
	env := newFrontEnv(t)
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		Menu:          []MenuItem{{Type: MenuItemPage, Target: "research"}, {Type: MenuItemPage, Target: "access"}, {Type: MenuItemArticles}},
		FrontPage:     "research",
		FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: []string{"research", "access"}}},
	}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	// トップのページは / へ一時的に（設定を変えられるため 301 ではなく 302 で）転送する。HEAD も同じ。
	for _, res := range []*response{c.get("/research"), c.head("/research"), c.get("/research?utm_source=x")} {
		if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
			t.Errorf("%s %s: %d %q, want 302 /", res.Request.Method, res.Request.URL, res.StatusCode, res.Header.Get("Location"))
		}
	}
	// トップに出していないページはそのまま開ける。
	access := c.get("/access")
	expectStatus(t, access, http.StatusOK)
	expectContains(t, access.Body, `<link rel="canonical" href="https://media.example.ac.jp/access">`)
	expectMenu(t, "/access", access.Body, "/ 研究紹介", "/access アクセス *", "/articles 記事一覧")

	// メニューのトップのページの項目は / を指し、/ で現在の項目になる。カードにはトップのページ自身を出さない。
	home := c.get("/")
	expectStatus(t, home, http.StatusOK)
	expectMenu(t, "/", home.Body, "/ 研究紹介 *", "/access アクセス", "/articles 記事一覧")
	expectMenu(t, "/articles", c.get("/articles").Body, "/ 研究紹介", "/access アクセス", "/articles 記事一覧 *")
	_, cards, _ := strings.Cut(home.Body, `<ul class="front-cards">`)
	cards, _, _ = strings.Cut(cards, "</ul>")
	if n := strings.Count(cards, `<li class="front-card`); n != 1 || !strings.Contains(cards, `href="/access"`) {
		t.Errorf("front cards = %d (%q), want only access", n, cards)
	}
	expectNotContainsAny(t, "front cards", cards, `href="/research"`, `href="/"`)

	// サイトマップは / を載せ、/research は載せない。llms.txt・llms-full.txt はページの欄に / で載せる。
	sitemap := c.get("/sitemap.xml").Body
	expectContains(t, sitemap, "<loc>https://media.example.ac.jp/</loc>", "<loc>https://media.example.ac.jp/access</loc>")
	expectNotContainsAny(t, "sitemap", sitemap, "/research</loc>")
	llms := c.get("/llms.txt").Body
	pages, _, _ := strings.Cut(llms, "## Articles")
	expectContains(t, pages, "## Pages\n", "- [研究紹介](https://media.example.ac.jp/): 量子計算の研究室です\n",
		"- [アクセス](https://media.example.ac.jp/access): ")
	expectNotContainsAny(t, "llms.txt", llms, "/research)")
	full := c.get("/llms-full.txt").Body
	expectContains(t, full, "# 研究紹介\n\nURL: https://media.example.ac.jp/\n\n", "# アクセス\n\nURL: https://media.example.ac.jp/access\n\n")
	expectNotContainsAny(t, "llms-full.txt", full, "/research\n")

	// テーマを上書きする内部要求（エクスポートの --theme・プレビュー）は転送せずにページを描画する。
	rec := env.server.serveInternal(httptest.NewRequest(http.MethodGet, "/research", nil), &themeOverride{ID: "tech"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<article class="page">`) {
		t.Errorf("theme override /research: %d, want 200 with the page", rec.Code)
	}

	// トップを別のページに変えると、前のトップのページは /research で開け、新しいトップのページが転送される。
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{FrontPage: "access"}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, c.get("/research"), http.StatusOK)
	if res := c.get("/access"); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
		t.Errorf("/access after switching: %d %q, want 302 /", res.StatusCode, res.Header.Get("Location"))
	}
	// トップのページが参照切れ（非公開）なら転送しない（ページ自体が 404）。サイトマップも今までどおり。
	if _, err := env.db.Exec(`UPDATE articles SET status = ? WHERE slug = 'access'`, article.StatusDraft); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, c.get("/access"), http.StatusNotFound)
	expectContains(t, c.get("/sitemap.xml").Body, "<loc>https://media.example.ac.jp/research</loc>")
	expectContains(t, c.get("/llms.txt").Body, "- [研究紹介](https://media.example.ac.jp/research): ")
}

// / の閲覧はトップに出したページの PV に数える（/<slug> で開いたときと同じ）。プレビュー・HEAD・先読みと、
// / へ転送する /<slug> の要求は数えない。
func TestFrontPageViewsAreCounted(t *testing.T) {
	env := newFrontEnv(t)
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{FrontPage: "research"}); err != nil {
		t.Fatal(err)
	}
	reader := env.newClient()
	expectStatus(t, reader.getAs("/", browserUA, nil), http.StatusOK)
	expectStatus(t, reader.head("/"), http.StatusOK)
	expectStatus(t, reader.getAs("/", browserUA, map[string]string{"Sec-Purpose": "prefetch"}), http.StatusOK)
	if res := reader.getAs("/research", browserUA, nil); res.StatusCode != http.StatusFound {
		t.Errorf("/research: %d, want 302", res.StatusCode)
	}
	tech, _ := theme.Builtin.Get("tech")
	pv := &themePreview{Theme: tech, Lang: "ja", Page: previewPageHome}
	if rec := env.server.serveInternal(httptest.NewRequest(http.MethodGet, "/", nil), &themeOverride{ID: tech.ID, Preview: pv}); rec.Code != http.StatusOK {
		t.Errorf("preview /: %d", rec.Code)
	}
	env.flushAnalytics()
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily WHERE article_id = ?`, env.articleIDBySlug("research")); n != 1 {
		t.Errorf("front page views = %d, want 1", n)
	}
	rows, err := env.server.popularArticles(context.Background(), 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	// トップに出したページの公開 URL は / だけ（/research は / へ転送する）。公開サイトの URL を起点にする。
	if len(rows) != 1 || rows[0].Title != "研究紹介" || rows[0].PublicURL != testBaseURL+"/" {
		t.Errorf("popular = %+v, want the front page with %s/", rows, testBaseURL)
	}
	body := env.loggedIn(admin).get("/admin").Body
	expectContains(t, body, `<a href="`+testBaseURL+`/" target="_blank" rel="noopener"`)
	if strings.Contains(body, `href="/research"`) {
		t.Error("dashboard links the front page as /research, want /")
	}
}

func TestSiteStructureSectionEditorUIAndI18n(t *testing.T) {
	env := newPagesEnv(t, Config{})
	env.insertMedia("sec-photo", "sec-photo.webp", "校舎", "", "学校")
	adm := env.loggedIn(admin)

	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		FrontPage: "research",
		FrontSections: []FrontSection{
			{Type: FrontSectionHero},
			{Type: FrontSectionCards, Pages: []string{"access"}},
			{Type: FrontSectionGallery, Tag: "学校", Title: "キャンパスの風景", Limit: 8, Layout: "masonry", LinkURL: "/articles", LinkLabel: "一覧を見る"},
			{Type: FrontSectionArticleList, Title: "お知らせ", ArticleTag: "circle"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 日本語表示: 1始まりのセクション番号、日英翻訳済みの種別・ラベル、選択式コントロール、ボタンの disabled / danger クラス
	res := adm.get("/admin/settings/structure")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"トップページのセクション",
		"セクション 1", "セクション 2", "セクション 3", "セクション 4",
		"ヒーロー（トップ固定ページ本文）",
		"固定ページカード一覧",
		"フォトギャラリー",
		"記事一覧",
		"上の「トップに出すページ」で選んだ固定ページの本文とアイキャッチを表示します。",
		"並べる固定ページのスラッグ（カンマ区切り）",
		"公開中のページ（押すと追加）:",
		"表示するメディアタグ",
		"見出し（任意）",
		"表示枚数（0で既定の6枚、最大24枚）",
		"並べ方",
		"行揃え（おすすめ）",
		"段組み",
		"絞り込むカテゴリ",
		"すべてのカテゴリ（新着順）",
		"「詳しく見る」のリンク先（任意）",
		`placeholder="詳しく見る"`,
		"「すべて見る」のリンク先（任意）",
		`placeholder="すべて見る"`,
		"副ボタンのリンク先（任意）",
		"副ボタンの文言",
		`placeholder="詳細を見る"`,
		`value="section-up:0" disabled`,
		`value="section-down:3" disabled`,
		`value="section-delete:0" class="btn btn-danger btn-sm"`,
		`data-section-row`,
		`data-section-type`,
		`data-section-show-for="hero"`,
		`data-section-show-for="hero gallery"`,
		`data-section-show-for="cards"`,
		`data-section-show-for="gallery"`,
		`data-section-show-for="gallery article_list"`,
		`data-section-show-for="article_list"`,
		`name="section_secondary_url"`,
		`name="section_secondary_label"`,
		`data-section-page-chip="research"`,
		`data-section-tag-chip="学校"`,
		`<option value="masonry" selected>段組み</option>`,
		`<option value="circle" selected>サークル</option>`,
		"＋ セクションを追加",
	)
	expectNotContainsAny(t, "Japanese section editor", res.Body,
		"Front page sections", "Section 0", "Move up", "Move down", "Add section",
		"Page slugs (comma separated)", "Media tag", "Article tag", "Link URL", "Link label",
	)

	// 英語表示: 英語ラベルに切り替わり、0始まりの Section 0 が出ないこと
	adm.useEnglish()
	enRes := adm.get("/admin/settings/structure")
	expectStatus(t, enRes, http.StatusOK)
	expectContains(t, enRes.Body,
		"Front page sections",
		"Section 1", "Section 2", "Section 3", "Section 4",
		"Hero (front page body)",
		"Page cards",
		"Photo gallery",
		"Article list",
		"Page slugs to show as cards (comma-separated)",
		"Published pages (click to add):",
		"Media tag to show",
		"Heading (optional)",
		"Number of images (0 for default 6, up to 24)",
		"Filter by category",
		"All categories (newest first)",
		`&#34;Learn more&#34; link destination (optional)`,
		`placeholder="Learn more"`,
		`&#34;View all&#34; link destination (optional)`,
		`placeholder="View all"`,
		"Secondary button destination (optional)",
		"Secondary button text",
		`placeholder="View details"`,
		`<option value="masonry" selected>Masonry</option>`,
		"+ Add section",
	)
	expectNotContainsAny(t, "English section editor", enRes.Body,
		"Section 0", "Move up", "Move down", "Page slugs (comma separated)", "トップページのセクション",
	)
}

func TestSiteStructureFormSaveClearsUnusedFieldsOnNonGallerySections(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)

	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		FrontPage: "research",
		FrontSections: []FrontSection{
			{Type: FrontSectionHero},
			{Type: FrontSectionCards, Pages: []string{"access"}},
			{Type: FrontSectionArticleList, Title: "お知らせ", ArticleTag: "circle"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 非ギャラリー行の section_layout <select> は空値が選択され、justified が selected にならないことを確認。
	page := adm.get("/admin/settings/structure")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `<option value="" selected hidden></option>`)
	expectNotContainsAny(t, "non-gallery layout select", page.Body, `<option value="justified" selected>`)

	// メニュー保存時に非ギャラリー行へ section_layout="justified" 等の不要フィールドが送信されてもクリアされることを確認。
	form := url.Values{
		"front_page":              {"research"},
		"menu_type":               {"page", "articles"},
		"menu_page":               {"research", ""},
		"menu_category":           {"", ""},
		"menu_url":                {"", ""},
		"menu_label":              {"研究", "記事一覧"},
		"section_type":            {string(FrontSectionHero), string(FrontSectionCards), string(FrontSectionArticleList)},
		"section_pages":           {"access", "access", "access"},
		"section_tag":             {"学校", "学校", "学校"},
		"section_title":           {"不要タイトル", "不要タイトル", "お知らせ"},
		"section_limit":           {"6", "6", "12"},
		"section_layout":          {"justified", "justified", "justified"},
		"section_link_url":        {"", "/articles", "/articles"},
		"section_link_label":      {"", "一覧", "一覧"},
		"section_secondary_url":   {"", "/access", "/access"},
		"section_secondary_label": {"", "詳細", "詳細"},
		"section_article_tag":     {"circle", "circle", "circle"},
	}
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")

	wantJSON := `[{"type":"hero"},{"type":"cards","pages":["access"]},{"type":"article_list","title":"お知らせ","article_tag":"circle"}]`
	if got := env.setting(settingFrontSections); got != wantJSON {
		t.Fatalf("saved front_sections JSON = %s, want %s", got, wantJSON)
	}

	st := env.server.loadSiteStructure(context.Background())
	if len(st.FrontSections) != 3 {
		t.Fatalf("len(FrontSections) = %d, want 3", len(st.FrontSections))
	}
	hero := st.FrontSections[0]
	if hero.Type != FrontSectionHero || len(hero.Pages) != 0 || hero.Tag != "" || hero.Title != "" ||
		hero.Limit != 0 || hero.Layout != "" || hero.ArticleTag != "" || hero.LinkURL != "" || hero.LinkLabel != "" ||
		hero.SecondaryURL != "" || hero.SecondaryLabel != "" {
		t.Errorf("hero section = %+v, want only Type=hero", hero)
	}
	cards := st.FrontSections[1]
	if cards.Type != FrontSectionCards || cards.Layout != "" || cards.Tag != "" || cards.Title != "" ||
		cards.Limit != 0 || cards.ArticleTag != "" || cards.LinkURL != "" ||
		cards.LinkLabel != "" || cards.SecondaryURL != "" || cards.SecondaryLabel != "" ||
		strings.Join(cards.Pages, ",") != "access" {
		t.Errorf("cards section = %+v", cards)
	}
	articles := st.FrontSections[2]
	if articles.Type != FrontSectionArticleList || len(articles.Pages) != 0 || articles.Tag != "" ||
		articles.Title != "お知らせ" || articles.Limit != 0 || articles.Layout != "" ||
		articles.ArticleTag != "circle" || articles.LinkURL != "" || articles.LinkLabel != "" ||
		articles.SecondaryURL != "" || articles.SecondaryLabel != "" {
		t.Errorf("article_list section = %+v", articles)
	}
}

func TestFrontHeroCTAValidationSaveAndRender(t *testing.T) {
	for _, tc := range []struct {
		name    string
		section FrontSection
		wantKey string
	}{
		{"primary and secondary valid", FrontSection{Type: FrontSectionHero, LinkURL: "/access", LinkLabel: "はじめる", SecondaryURL: "https://github.com/example/sorus", SecondaryLabel: "GitHub"}, ""},
		{"primary bad scheme", FrontSection{Type: FrontSectionHero, LinkURL: "javascript:alert(1)"}, "structure_err_gallery_link_url"},
		{"secondary bad scheme", FrontSection{Type: FrontSectionHero, SecondaryURL: "javascript:alert(1)"}, "structure_err_gallery_link_url"},
		{"secondary protocol relative", FrontSection{Type: FrontSectionHero, SecondaryURL: "//evil.example/x"}, "structure_err_gallery_link_url"},
		{"primary label too long", FrontSection{Type: FrontSectionHero, LinkURL: "/access", LinkLabel: strings.Repeat("あ", maxGalleryLinkLabelRunes+1)}, "structure_err_gallery_link_label"},
		{"secondary label too long", FrontSection{Type: FrontSectionHero, SecondaryURL: "/access", SecondaryLabel: strings.Repeat("あ", maxGalleryLinkLabelRunes+1)}, "structure_err_gallery_link_label"},
		{"secondary label control char", FrontSection{Type: FrontSectionHero, SecondaryURL: "/access", SecondaryLabel: "bad\nlabel"}, "structure_err_gallery_link_label"},
		{"gallery ignores invalid secondary url", FrontSection{Type: FrontSectionGallery, LinkURL: "/articles", SecondaryURL: "javascript:alert(1)"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSiteStructure(SiteStructure{FrontPage: "research", FrontSections: []FrontSection{tc.section}})
			if tc.wantKey == "" {
				if err != nil {
					t.Fatalf("validateSiteStructure = %v, want nil", err)
				}
				return
			}
			var se *siteStructureError
			if !errors.As(err, &se) || se.Key != tc.wantKey {
				t.Fatalf("validateSiteStructure = %v, want %s", err, tc.wantKey)
			}
		})
	}

	env := newFrontEnv(t)
	enableMultilingual(t, env.db)
	group := "hero-cta-translation"
	if _, err := env.db.Exec(`UPDATE articles SET translation_group_id = ? WHERE slug = 'research'`, group); err != nil {
		t.Fatal(err)
	}
	seedPage(t, env.db, article.Article{Title: "Research Lab", Slug: "research-en", Lang: "en", TranslationGroupID: group,
		ContentMarkdown: "Quantum computing research.", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})

	adm := env.loggedIn(admin)
	form := url.Values{
		"front_page":              {"research"},
		"section_type":            {string(FrontSectionHero), string(FrontSectionGallery), string(FrontSectionArticleList)},
		"section_pages":           {"", "", ""},
		"section_tag":             {"", "学校", ""},
		"section_title":           {"", "写真", ""},
		"section_limit":           {"", "6", ""},
		"section_layout":          {"", "justified", ""},
		"section_link_url":        {" /access ", "/articles", "/ignored"},
		"section_link_label":      {" 詳しくはこちら ", "一覧", "無視"},
		"section_secondary_url":   {" https://example.com/docs ", "https://example.com/ignored", "https://example.com/ignored"},
		"section_secondary_label": {" ドキュメント ", "無視される副ボタン", "無視される副ボタン"},
		"section_article_tag":     {"", "", ""},
	}
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")

	st := env.server.loadSiteStructure(context.Background())
	if len(st.FrontSections) != 3 {
		t.Fatalf("len(FrontSections) = %d, want 3", len(st.FrontSections))
	}
	if got := st.FrontSections[0]; got.LinkURL != "/access" || got.LinkLabel != "詳しくはこちら" ||
		got.SecondaryURL != "https://example.com/docs" || got.SecondaryLabel != "ドキュメント" {
		t.Fatalf("saved hero section = %+v", got)
	}
	if got := st.FrontSections[1]; got.SecondaryURL != "" || got.SecondaryLabel != "" || got.LinkURL != "/articles" {
		t.Fatalf("saved gallery section should clear secondary CTA: %+v", got)
	}
	if got := st.FrontSections[2]; got.LinkURL != "" || got.LinkLabel != "" || got.SecondaryURL != "" || got.SecondaryLabel != "" {
		t.Fatalf("saved article_list section should clear all CTA fields: %+v", got)
	}

	// 管理画面のフォームにも保存値が書き戻される。
	adminBody := adm.get("/admin/settings/structure").Body
	expectContains(t, adminBody,
		`name="section_link_url" value="/access"`,
		`name="section_link_label" maxlength="40" value="詳しくはこちら"`,
		`name="section_secondary_url" value="https://example.com/docs"`,
		`name="section_secondary_label" maxlength="40" value="ドキュメント"`,
	)

	// 公開トップページで主・副 CTA ボタンが描画される。
	c := env.newClient()
	home := c.get("/")
	expectStatus(t, home, http.StatusOK)
	expectContains(t, home.Body,
		`<div class="front-hero-actions">`,
		`<a href="/access" class="front-hero-btn">詳しくはこちら</a>`,
		`<a href="https://example.com/docs" class="front-hero-btn front-hero-btn--secondary">ドキュメント</a>`,
	)

	// hero_layout: "centered" のテーマでも front-hero--centered と front-hero-actions が組み合わさって描画される。
	customDir := t.TempDir()
	centeredThemeDir := filepath.Join(customDir, "centeredhero")
	if err := os.MkdirAll(centeredThemeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(centeredThemeDir, "theme.json"), []byte(`{"name":{"ja":"中央ヒーロー","en":"Centered Hero"},"description":{"ja":"テスト","en":"Test"},"color_scheme":"light","layout":{"hero_layout":"centered"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(centeredThemeDir, "theme.css"), []byte(`:root { --bg:#fff; --surface:#f8fafc; --fg:#0f172a; --muted:#475569; --border:#e2e8f0; --accent:#2563eb; --accent-fg:#fff; --accent-soft:#dbeafe; --mark:#fef08a; --code-bg:#0f172a; --code-fg:#e2e8f0; --font-body:sans-serif; --font-heading:sans-serif; --font-mono:monospace; --radius:8px; --radius-lg:12px; --content-width:1080px; --prose-width:760px; --shadow:none; }`), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := theme.LoadComposite(os.DirFS("../../static"), customDir)
	if err != nil {
		t.Fatal(err)
	}
	env.server.themes = cat
	rec := env.server.serveInternal(httptest.NewRequest(http.MethodGet, "/", nil), &themeOverride{ID: "centeredhero"})
	if rec.Code != http.StatusOK {
		t.Fatalf("serveInternal centeredhero: status %d", rec.Code)
	}
	expectContains(t, rec.Body.String(),
		`<section class="front-hero front-hero--centered" aria-labelledby="front-hero-title">`,
		`<div class="front-hero-actions">`,
		`<a href="/access" class="front-hero-btn">詳しくはこちら</a>`,
		`<a href="https://example.com/docs" class="front-hero-btn front-hero-btn--secondary">ドキュメント</a>`,
	)

	// ラベル省略時は日英の既定文言が使われ、URL が片方のみなら設定された側のみ描画される。
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		FrontPage: "research",
		FrontSections: []FrontSection{
			{Type: FrontSectionHero, LinkURL: "/access", SecondaryURL: "https://example.com/docs"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	expectContains(t, c.get("/").Body,
		`<a href="/access" class="front-hero-btn">詳しく見る</a>`,
		`<a href="https://example.com/docs" class="front-hero-btn front-hero-btn--secondary">詳細を見る</a>`,
	)
	expectContains(t, c.get("/en/").Body,
		`<a href="/access" class="front-hero-btn">Learn more</a>`,
		`<a href="https://example.com/docs" class="front-hero-btn front-hero-btn--secondary">View details</a>`,
	)

	// 副ボタンの URL のみ設定・主ボタンはラベルのみ（URL 空）の場合、副ボタンのみ描画される。
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		FrontPage: "research",
		FrontSections: []FrontSection{
			{Type: FrontSectionHero, LinkLabel: "URLなし主ラベル", SecondaryURL: "/access", SecondaryLabel: "アクセスを見る"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	onlySecondary := c.get("/").Body
	expectContains(t, onlySecondary, `<div class="front-hero-actions">`, `<a href="/access" class="front-hero-btn front-hero-btn--secondary">アクセスを見る</a>`)
	expectNotContainsAny(t, "only secondary CTA", onlySecondary, "URLなし主ラベル", `class="front-hero-btn"`)
}
