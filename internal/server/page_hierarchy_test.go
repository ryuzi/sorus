package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
)

// TestBuildPageHierarchyNav は、固定ページの一覧からルートのページとその子孫でサイドバー・ページ送り・パンくずリストの
// 祖先を作る決まり（並び・開く枝・前後のページ・循環・階層に入っていないページ）を確かめる。
func TestBuildPageHierarchyNav(t *testing.T) {
	// guide
	//   install（並び順 1）
	//     install-linux
	//   quickstart（並び順 2）
	//   faq（並び順 3）
	// other（子ページなし）
	pages := []pageNavNode{
		{ID: "guide", Title: "ガイド", Slug: "guide"},
		{ID: "other", Title: "ほか", Slug: "other"},
		{ID: "install", Title: "インストール", Slug: "install", ParentID: "guide"},
		{ID: "quickstart", Title: "クイックスタート", Slug: "quickstart", ParentID: "guide"},
		{ID: "faq", Title: "よくある質問", Slug: "faq", ParentID: "guide"},
		{ID: "install-linux", Title: "Linux", Slug: "install-linux", ParentID: "install"},
	}
	urlOf := func(p pageNavNode) string { return "/" + p.Slug }
	l := func(label, u string, current bool, children ...docNavLink) docNavLink {
		return docNavLink{Label: label, URL: u, Current: current, Children: children}
	}
	ptr := func(label, u string) *docNavLink { return &docNavLink{Label: label, URL: u} }

	for _, tc := range []struct {
		name string
		cur  string
		want pageHierarchy
	}{
		{"child: siblings in order, its own children open, pager through descendants", "install", pageHierarchy{
			Nav: &docNav{Title: "ガイド", TitleURL: "/guide",
				Links: []docNavLink{l("インストール", "/install", true, l("Linux", "/install-linux", false)),
					l("クイックスタート", "/quickstart", false), l("よくある質問", "/faq", false)},
				Prev: ptr("ガイド", "/guide"), Next: ptr("Linux", "/install-linux")},
			Ancestors: []docNavLink{{Label: "ガイド", URL: "/guide"}},
		}},
		{"grandchild: ancestors from the root, branch open", "install-linux", pageHierarchy{
			Nav: &docNav{Title: "ガイド", TitleURL: "/guide",
				Links: []docNavLink{l("インストール", "/install", false, l("Linux", "/install-linux", true)),
					l("クイックスタート", "/quickstart", false), l("よくある質問", "/faq", false)},
				Prev: ptr("インストール", "/install"), Next: ptr("クイックスタート", "/quickstart")},
			Ancestors: []docNavLink{{Label: "ガイド", URL: "/guide"}, {Label: "インストール", URL: "/install"}},
		}},
		{"root: title is current, closed branches stay closed", "guide", pageHierarchy{
			Nav: &docNav{Title: "ガイド", TitleURL: "/guide", TitleCurrent: true,
				Links: []docNavLink{l("インストール", "/install", false), l("クイックスタート", "/quickstart", false), l("よくある質問", "/faq", false)},
				Next:  ptr("インストール", "/install")},
		}},
		{"collapsed descendants still take part in the pager", "quickstart", pageHierarchy{
			Nav: &docNav{Title: "ガイド", TitleURL: "/guide",
				Links: []docNavLink{l("インストール", "/install", false), l("クイックスタート", "/quickstart", true), l("よくある質問", "/faq", false)},
				Prev:  ptr("Linux", "/install-linux"), Next: ptr("よくある質問", "/faq")},
			Ancestors: []docNavLink{{Label: "ガイド", URL: "/guide"}},
		}},
		{"last page has no next page", "faq", pageHierarchy{
			Nav: &docNav{Title: "ガイド", TitleURL: "/guide",
				Links: []docNavLink{l("インストール", "/install", false), l("クイックスタート", "/quickstart", false), l("よくある質問", "/faq", true)},
				Prev:  ptr("クイックスタート", "/quickstart")},
			Ancestors: []docNavLink{{Label: "ガイド", URL: "/guide"}},
		}},
		{"top-level page without children is not in a hierarchy", "other", pageHierarchy{}},
		{"unknown page", "missing", pageHierarchy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildPageHierarchyNav(pages, tc.cur, urlOf)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildPageHierarchyNav(%q) =\n%s\nancestors %+v\nwant\n%s\nancestors %+v",
					tc.cur, describeHierarchyNav(got.Nav), got.Ancestors, describeHierarchyNav(tc.want.Nav), tc.want.Ancestors)
			}
		})
	}

	t.Run("a page whose parent is not visible becomes the root", func(t *testing.T) {
		orphans := []pageNavNode{
			{ID: "a", Title: "A", Slug: "a", ParentID: "draft-parent"},
			{ID: "b", Title: "B", Slug: "b", ParentID: "a"},
		}
		got := buildPageHierarchyNav(orphans, "b", urlOf)
		if got.Nav == nil || got.Nav.Title != "A" || len(got.Ancestors) != 1 || got.Ancestors[0].URL != "/a" {
			t.Fatalf("got %s ancestors %+v, want root A", describeHierarchyNav(got.Nav), got.Ancestors)
		}
		if got := buildPageHierarchyNav(orphans[:1], "a", urlOf); got.Nav != nil {
			t.Errorf("orphan without children = %s, want nil", describeHierarchyNav(got.Nav))
		}
	})

	t.Run("cycles from direct DB edits terminate", func(t *testing.T) {
		cyc := []pageNavNode{
			{ID: "x", Title: "X", Slug: "x", ParentID: "y"},
			{ID: "y", Title: "Y", Slug: "y", ParentID: "x"},
			{ID: "self", Title: "S", Slug: "s", ParentID: "self"},
		}
		got := buildPageHierarchyNav(cyc, "x", urlOf)
		if got.Nav == nil || got.Nav.Title != "Y" || len(got.Nav.Links) != 1 || got.Nav.Links[0].URL != "/x" {
			t.Fatalf("cycle nav = %s", describeHierarchyNav(got.Nav))
		}
		if got := buildPageHierarchyNav(cyc, "self", urlOf); got.Nav != nil {
			t.Errorf("self-parented page = %s, want nil", describeHierarchyNav(got.Nav))
		}
	})
}

// describeHierarchyNav は入れ子の子ページも展開した、比較の失敗を読むための文字列。
func describeHierarchyNav(n *docNav) string {
	if n == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(describeDocNav(n))
	var walk func(ls []docNavLink, depth int)
	walk = func(ls []docNavLink, depth int) {
		for _, l := range ls {
			b.WriteString("\n  tree:" + strings.Repeat("  ", depth) + l.Label + " " + l.URL)
			walk(l.Children, depth+1)
		}
	}
	walk(n.Links, 0)
	return b.String()
}

// hierarchySite は newPagesEnv のサイトに、固定ページの階層（ガイドとその子孫）を足した ID の組。
type hierarchySite struct {
	guide, install, linux, quickstart, faq, draft, scheduled, en string
}

// seedHierarchy は env に固定ページの階層を足す。読者に見えるのは guide とその子孫（install・install-linux・
// quickstart・faq）で、下書きと予約中の子ページ、英語の子ページは日本語のサイドバーに出ない。
func seedHierarchy(t *testing.T, env *testEnv) hierarchySite {
	t.Helper()
	pub := func(title, slug, parent string, order int) string {
		return seedPage(t, env.db, article.Article{Title: title, Slug: slug, ContentMarkdown: title + "の本文",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour), ParentID: parent, SortOrder: order})
	}
	var h hierarchySite
	h.guide = pub("ガイド", "guide", "", 0)
	// 並び順は作った順と逆にし、作成日時ではなく sort_order で並ぶことを確かめる。
	h.faq = pub("よくある質問", "guide-faq", h.guide, 3)
	h.quickstart = pub("クイックスタート", "guide-quickstart", h.guide, 2)
	h.install = pub("インストール", "guide-install", h.guide, 1)
	h.linux = pub("Linux へのインストール", "guide-install-linux", h.install, 0)
	h.draft = seedPage(t, env.db, article.Article{Title: "準備中の子ページ", Slug: "guide-draft", ContentMarkdown: "x",
		ParentID: h.guide, SortOrder: 0})
	future := time.Now().UTC().Add(48 * time.Hour)
	h.scheduled = seedPage(t, env.db, article.Article{Title: "予約中の子ページ", Slug: "guide-scheduled", ContentMarkdown: "x",
		Status: article.StatusPublished, PublishedAt: &future, ParentID: h.guide, SortOrder: 0})
	h.en = seedPage(t, env.db, article.Article{Title: "Quickstart (EN)", Slug: "guide-quickstart-en", ContentMarkdown: "x",
		Lang: "en", Status: article.StatusPublished, PublishedAt: ago(time.Hour), ParentID: h.guide, SortOrder: 9})
	return h
}

// TestPageHierarchyPublicNav は、親ページを持つ・子ページを持つ固定ページに、サイト構成のメニューに並べなくても
// 階層から左のサイドバー・前後のページ送り・パンくずリスト（祖先つき）を出すことを確かめる。
func TestPageHierarchyPublicNav(t *testing.T) {
	env := newPagesEnv(t, Config{})
	seedHierarchy(t, env)
	// ヘッダーには親ページだけを並べる（子ページはメニューに無い）。
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{Menu: []MenuItem{
		{Type: MenuItemPage, Target: "guide"}, {Type: MenuItemArticles},
	}}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	res := c.get("/guide-quickstart")
	expectStatus(t, res, http.StatusOK)
	sidebar := docSidebar(res.Body)
	expectContains(t, sidebar,
		`<nav class="doc-sidebar-nav" aria-label="ガイド">`,
		`<p class="doc-sidebar-title"><a href="/guide">ガイド</a></p>`,
		`<li><a href="/guide-install">インストール</a></li>`,
		`<li><a href="/guide-quickstart" aria-current="page">クイックスタート</a></li>`,
		`<li><a href="/guide-faq">よくある質問</a></li>`)
	// 並び順（sort_order）の順に並ぶ。
	if i, j, k := strings.Index(sidebar, "/guide-install\""), strings.Index(sidebar, "/guide-quickstart\""), strings.Index(sidebar, "/guide-faq\""); !(i >= 0 && i < j && j < k) {
		t.Errorf("sidebar order install(%d) < quickstart(%d) < faq(%d) is broken:\n%s", i, j, k, sidebar)
	}
	// 読者に見えない子ページ・別の言語の子ページと、閉じた枝の孫ページはサイドバーに出さない。
	expectNotContainsAny(t, "sidebar of /guide-quickstart", sidebar, "準備中の子ページ", "予約中の子ページ", "Quickstart (EN)", "guide-install-linux")
	expectContains(t, res.Body,
		`<div class="doc-layout">`,
		// 前のページは閉じた枝の孫ページ（深さ優先の順）、次のページは次の子ページ。
		`<a class="doc-pager-link doc-pager-prev" href="/guide-install-linux" rel="prev"><span class="doc-pager-label">前のページ</span><span class="doc-pager-title">Linux へのインストール</span></a>`,
		`<a class="doc-pager-link doc-pager-next" href="/guide-faq" rel="next">`,
		// パンくずリスト: ホーム › ガイド › クイックスタート。
		"<li><a href=\"/\">ホーム</a></li>\n    <li><a href=\"/guide\">ガイド</a></li>\n    <li aria-current=\"page\">クイックスタート</li>")
	crumbs, _ := jsonLD(t, res.Body)["BreadcrumbList"]["itemListElement"].([]any)
	if len(crumbs) != 3 {
		t.Fatalf("JSON-LD breadcrumb = %v, want ホーム › ガイド › クイックスタート", crumbs)
	}
	if mid, _ := crumbs[1].(map[string]any); mid["name"] != "ガイド" || mid["item"] != testBaseURL+"/guide" {
		t.Errorf("JSON-LD breadcrumb parent = %v", mid)
	}

	// 孫ページ: 祖先を2つ並べ、表示中のページの枝を入れ子で開く。
	res = c.get("/guide-install-linux")
	expectStatus(t, res, http.StatusOK)
	sidebar = docSidebar(res.Body)
	expectContains(t, res.Body, "<li><a href=\"/guide\">ガイド</a></li>\n    <li><a href=\"/guide-install\">インストール</a></li>\n    <li aria-current=\"page\">Linux へのインストール</li>")
	expectContains(t, sidebar, "<li><a href=\"/guide-install\">インストール</a>\n          <ul>",
		`<li><a href="/guide-install-linux" aria-current="page">Linux へのインストール</a></li>`)
	expectContains(t, res.Body, `class="doc-pager-link doc-pager-prev" href="/guide-install"`, `class="doc-pager-link doc-pager-next" href="/guide-quickstart"`)

	// ルートのページ: 見出しが現在地で、次のページは最初の子ページ。パンくずリストに祖先は無い。
	res = c.get("/guide")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, docSidebar(res.Body), `<p class="doc-sidebar-title"><a href="/guide" aria-current="page">ガイド</a></p>`)
	expectContains(t, res.Body, `class="doc-pager-link doc-pager-next" href="/guide-install"`,
		"<li><a href=\"/\">ホーム</a></li>\n    <li aria-current=\"page\">ガイド</li>")
	expectNotContainsAny(t, "/guide", res.Body, "doc-pager-prev")

	// 階層に入っていない固定ページ・記事は今までどおり（メニューにも無いのでサイドバーを出さない）。
	for _, path := range []string{"/research", "/articles/keion-live"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		expectNotContainsAny(t, path, res.Body, `class="doc-layout"`, `class="doc-sidebar"`, `class="doc-pager"`)
	}
	expectContains(t, c.get("/research").Body, "<li><a href=\"/\">ホーム</a></li>\n    <li aria-current=\"page\">研究紹介</li>")
}

// TestPageHierarchyAndMenuSections は、メニューのセクションのサイドバー（site_menu）が今までどおり動き、階層に入っている
// ページでは階層のサイドバーがメニューより優先されることを確かめる。
func TestPageHierarchyAndMenuSections(t *testing.T) {
	env := newPagesEnv(t, Config{})
	seedHierarchy(t, env)
	menu := docsMenu()
	// メニューのセクションに、階層に入っているページ（クイックスタート）も並べる。
	menu[0].Children = append(menu[0].Children, MenuItem{Type: MenuItemPage, Target: "guide-quickstart"})
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{Menu: menu}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	// 階層に入っていないページは、メニューのセクションのサイドバーのまま（後方互換）。
	access := docSidebar(c.get("/access").Body)
	expectContains(t, access, `<p class="doc-sidebar-title">ドキュメント</p>`, `<li><a href="/access" aria-current="page">アクセス</a></li>`,
		`<li><a href="/guide-quickstart">クイックスタート</a></li>`)
	// 記事もメニューのセクションのサイドバーのまま。
	expectContains(t, docSidebar(c.get("/articles/keion-live").Body), `<p class="doc-sidebar-title">ドキュメント</p>`)

	// 階層に入っているページは、メニューのセクションにあっても階層のサイドバー。
	quick := c.get("/guide-quickstart").Body
	expectContains(t, docSidebar(quick), `<p class="doc-sidebar-title"><a href="/guide">ガイド</a></p>`)
	expectNotContainsAny(t, "/guide-quickstart sidebar", docSidebar(quick), "ドキュメント", `href="/access"`)
}

// TestPageHierarchyFrontPageRoot は、トップに出しているページがルートのとき、見出し・ページ送りのリンクを / にし、
// パンくずリストでホームと重ねないことを確かめる。
func TestPageHierarchyFrontPageRoot(t *testing.T) {
	env := newPagesEnv(t, Config{})
	seedHierarchy(t, env)
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{FrontPage: "guide"}); err != nil {
		t.Fatal(err)
	}
	res := env.newClient().get("/guide-install")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, docSidebar(res.Body), `<p class="doc-sidebar-title"><a href="/">ガイド</a></p>`)
	expectContains(t, res.Body, `<a class="doc-pager-link doc-pager-prev" href="/" rel="prev">`,
		"<li><a href=\"/\">ホーム</a></li>\n    <li aria-current=\"page\">インストール</li>")
	expectNotContainsAny(t, "/guide-install", res.Body, `href="/guide"`)
	crumbs, _ := jsonLD(t, res.Body)["BreadcrumbList"]["itemListElement"].([]any)
	if len(crumbs) != 2 {
		t.Errorf("JSON-LD breadcrumb = %v, want ホーム › インストール", crumbs)
	}
}

// TestPageHierarchyEnglishTree は、英語の固定ページの階層は英語のページだけで作り、日本語の親は使わないことを確かめる。
func TestPageHierarchyEnglishTree(t *testing.T) {
	env := newPagesEnv(t, Config{})
	h := seedHierarchy(t, env)
	enRoot := seedPage(t, env.db, article.Article{Title: "Guide", Slug: "guide-en", ContentMarkdown: "x", Lang: "en",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	seedPage(t, env.db, article.Article{Title: "Install", Slug: "install-en", ContentMarkdown: "x", Lang: "en",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour), ParentID: enRoot})
	c := env.newClient()

	res := c.get("/en/install-en")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, docSidebar(res.Body), `<p class="doc-sidebar-title"><a href="/en/guide-en">Guide</a></p>`,
		`<li><a href="/en/install-en" aria-current="page">Install</a></li>`)
	expectContains(t, res.Body, "<li><a href=\"/en/\">Home</a></li>\n    <li><a href=\"/en/guide-en\">Guide</a></li>")
	// 日本語の親ページを指す英語のページは、日本語の親を使わず、子ページも無いので階層に入らない。
	res = c.get("/en/guide-quickstart-en")
	expectStatus(t, res, http.StatusOK)
	expectNotContainsAny(t, "/en/guide-quickstart-en", res.Body, `class="doc-sidebar"`, "ガイド</a></li>")
	_ = h
}

// TestPageHierarchyDraftPreview は、管理画面の下書きのプレビューでは、まだ読者に見えない子ページ自身もサイドバーに入れて
// 公開後の見た目を確かめられることを確かめる。
func TestPageHierarchyDraftPreview(t *testing.T) {
	env := newPagesEnv(t, Config{})
	h := seedHierarchy(t, env)
	res := env.loggedIn(chief).get("/admin/articles/" + h.draft + "/preview")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, docSidebar(res.Body), `<p class="doc-sidebar-title"><a href="/guide">ガイド</a></p>`,
		`<a href="/guide-draft" aria-current="page">準備中の子ページ</a>`)
	expectContains(t, res.Body, "<li><a href=\"/guide\">ガイド</a></li>\n    <li aria-current=\"page\">準備中の子ページ</li>")
	// 公開サイトには出さない。
	expectNotContainsAny(t, "/guide-quickstart", env.newClient().get("/guide-quickstart").Body, "準備中の子ページ")
}

// TestPageHierarchyStaticExport は、静的な書き出しでも固定ページの階層のサイドバー・ページ送り・パンくずリストを出す
// ことを確かめる。
func TestPageHierarchyStaticExport(t *testing.T) {
	env := newPagesEnv(t, Config{})
	seedHierarchy(t, env)
	outDir := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	read := func(urlPath string) string {
		t.Helper()
		b, err := os.ReadFile(exportedFile(outDir, urlPath))
		if err != nil {
			t.Fatalf("read exported %s: %v", urlPath, err)
		}
		return string(b)
	}
	quick := read("/guide-quickstart")
	expectContains(t, docSidebar(quick), `<p class="doc-sidebar-title"><a href="/guide">ガイド</a></p>`,
		`<li><a href="/guide-quickstart" aria-current="page">クイックスタート</a></li>`)
	expectContains(t, quick, `class="doc-pager-link doc-pager-next" href="/guide-faq"`,
		"<li><a href=\"/guide\">ガイド</a></li>\n    <li aria-current=\"page\">クイックスタート</li>")
	expectNotContainsAny(t, "exported /guide-quickstart", quick, "準備中の子ページ", "予約中の子ページ")
	// 書き出したページのリンクの先がすべて書き出されている（ページ送り・サイドバーのリンク切れが無い）。
	for _, p := range []string{"/guide", "/guide-install", "/guide-install-linux", "/guide-faq"} {
		if _, err := os.Stat(exportedFile(outDir, p)); err != nil {
			t.Errorf("exported %s: %v", p, err)
		}
	}
	// 階層に入っていないページは今までどおり。
	expectNotContainsAny(t, "exported /research", read("/research"), `class="doc-sidebar"`, `class="doc-pager"`)
}
