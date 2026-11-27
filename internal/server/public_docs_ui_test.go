package server

import (
	"context"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/static"
)

// TestBuildDocNav は、解決済みのメニューから表示中のページのセクションを選び、サイドバーの一覧と前後のページ送りを
// 作る決まりを確かめる。
func TestBuildDocNav(t *testing.T) {
	link := func(label, u string, current bool) publicMenuItem {
		return publicMenuItem{Label: label, URL: u, Current: current}
	}
	section := func(label, u string, current bool, children ...publicMenuItem) publicMenuItem {
		p := publicMenuItem{Label: label, URL: u, Current: current, Children: children}
		for _, c := range children {
			p.HasCurrent = p.HasCurrent || c.Current
		}
		return p
	}
	ptr := func(label, u string) *docNavLink { return &docNavLink{Label: label, URL: u} }

	for _, tc := range []struct {
		name string
		menu []publicMenuItem
		want *docNav
	}{
		{"no menu", nil, nil},
		{"top-level page without children", []publicMenuItem{link("会社概要", "/about", true)}, nil},
		{"section without the current page", []publicMenuItem{
			section("ドキュメント", "", false, link("はじめに", "/intro", false), link("設定", "/setup", false)),
		}, nil},
		{"heading-only section: middle child", []publicMenuItem{
			link("ホーム", "/", false),
			section("ドキュメント", "", false, link("はじめに", "/intro", false), link("インストール", "/install", true), link("設定", "/setup", false)),
		}, &docNav{
			Title: "ドキュメント",
			Links: []docNavLink{{Label: "はじめに", URL: "/intro", Current: false}, {Label: "インストール", URL: "/install", Current: true}, {Label: "設定", URL: "/setup", Current: false}},
			Prev:  ptr("はじめに", "/intro"), Next: ptr("設定", "/setup"),
		}},
		{"heading-only section: first child has no previous page", []publicMenuItem{
			section("ドキュメント", "", false, link("はじめに", "/intro", true), link("設定", "/setup", false)),
		}, &docNav{
			Title: "ドキュメント",
			Links: []docNavLink{{Label: "はじめに", URL: "/intro", Current: true}, {Label: "設定", URL: "/setup", Current: false}},
			Next:  ptr("設定", "/setup"),
		}},
		{"linked parent is the current page and comes first", []publicMenuItem{
			section("ガイド", "/guide", true, link("基本", "/guide-basic", false), link("応用", "/guide-advanced", false)),
		}, &docNav{
			Title: "ガイド", TitleURL: "/guide", TitleCurrent: true,
			Links: []docNavLink{{Label: "基本", URL: "/guide-basic", Current: false}, {Label: "応用", URL: "/guide-advanced", Current: false}},
			Next:  ptr("基本", "/guide-basic"),
		}},
		{"first child goes back to the linked parent", []publicMenuItem{
			section("ガイド", "/guide", false, link("基本", "/guide-basic", true), link("応用", "/guide-advanced", false)),
		}, &docNav{
			Title: "ガイド", TitleURL: "/guide",
			Links: []docNavLink{{Label: "基本", URL: "/guide-basic", Current: true}, {Label: "応用", URL: "/guide-advanced", Current: false}},
			Prev:  ptr("ガイド", "/guide"), Next: ptr("応用", "/guide-advanced"),
		}},
		{"external links and duplicates of the current page are skipped in the pager", []publicMenuItem{
			section("リンク", "", false,
				link("公式サイト", "https://example.com/", false),
				link("前", "/prev", false),
				link("別ホスト", "//evil.example/x", false),
				link("ここ", "/here", true),
				link("ここ（再掲）", "/here", true),
				link("GitHub", "https://github.com/", false),
			),
		}, &docNav{
			Title: "リンク",
			Links: []docNavLink{{Label: "公式サイト", URL: "https://example.com/", Current: false}, {Label: "前", URL: "/prev", Current: false}, {Label: "別ホスト", URL: "//evil.example/x", Current: false},
				{Label: "ここ", URL: "/here", Current: true}, {Label: "ここ（再掲）", URL: "/here", Current: true}, {Label: "GitHub", URL: "https://github.com/", Current: false}},
			Prev: ptr("前", "/prev"),
		}},
		{"the first section that contains the page wins", []publicMenuItem{
			section("A", "", false, link("x", "/x", true)),
			section("B", "", false, link("x", "/x", true), link("y", "/y", false)),
		}, &docNav{Title: "A", Links: []docNavLink{{Label: "x", URL: "/x", Current: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDocNav(tc.menu)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildDocNav =\n%+v\nwant\n%+v", describeDocNav(got), describeDocNav(tc.want))
			}
		})
	}
}

// describeDocNav は比較の失敗を読みやすく出すため、ポインターの先も展開した文字列にする。
func describeDocNav(n *docNav) string {
	if n == nil {
		return "<nil>"
	}
	s := func(l *docNavLink) string {
		if l == nil {
			return "nil"
		}
		return l.Label + " " + l.URL
	}
	var b strings.Builder
	b.WriteString(n.Title + " " + n.TitleURL)
	if n.TitleCurrent {
		b.WriteString(" (current)")
	}
	for _, l := range n.Links {
		b.WriteString("\n  " + l.Label + " " + l.URL)
		if l.Current {
			b.WriteString(" (current)")
		}
	}
	b.WriteString("\n  prev=" + s(n.Prev) + " next=" + s(n.Next))
	return b.String()
}

// docsMenu は固定ページ2つ・記事・外部のサイトを子項目に持つ見出しのみのセクションと、ふつうの項目のメニュー。
func docsMenu() []MenuItem {
	return []MenuItem{
		{Label: "ドキュメント", LabelEn: "Docs", Type: MenuItemGroup, Children: []MenuItem{
			{Type: MenuItemPage, Target: "research"},
			{Type: MenuItemPage, Target: "access"},
			{Label: "新歓ライブの記録", Type: MenuItemURL, Target: "/articles/keion-live"},
			{Label: "大学", Type: MenuItemURL, Target: "https://www.example.ac.jp/"},
		}},
		{Type: MenuItemArticles},
	}
}

// docSidebar は本文の左のサイドバー（<aside class="doc-sidebar">…</aside>）を切り出す。無ければ空。
func docSidebar(body string) string {
	start := strings.Index(body, `<aside class="doc-sidebar">`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</aside>")
	if end < 0 {
		return ""
	}
	return body[start : start+end]
}

// TestDocLayoutForPagesInMenuSection は、メニューのセクションに入っている固定ページ・記事に左のサイドバーと前後の
// ページ送りを出し、メニューに無いページ・記事は今までどおりの見た目のままにすることを確かめる。
func TestDocLayoutForPagesInMenuSection(t *testing.T) {
	env := newPagesEnv(t, Config{})
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{Menu: docsMenu()}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	// 2番目の子項目: 現在地を強調し、前は1番目、次は3番目（記事）。
	access := c.get("/access")
	expectStatus(t, access, http.StatusOK)
	sidebar := docSidebar(access.Body)
	expectContains(t, sidebar,
		`<details class="doc-sidebar-collapsible" open>`,
		`<summary class="doc-sidebar-summary">このセクションのページ</summary>`,
		`<nav class="doc-sidebar-nav" aria-label="ドキュメント">`,
		`<p class="doc-sidebar-title">ドキュメント</p>`,
		`<li><a href="/research">研究紹介</a></li>`,
		`<li><a href="/access" aria-current="page">アクセス</a></li>`,
		`<li><a href="/articles/keion-live">新歓ライブの記録</a></li>`,
		`<li><a href="https://www.example.ac.jp/">大学</a></li>`)
	expectContains(t, access.Body,
		`<div class="doc-layout">`, `<div class="doc-main">`,
		`<nav class="doc-pager" aria-label="前後のページ">`,
		`<a class="doc-pager-link doc-pager-prev" href="/research" rel="prev"><span class="doc-pager-label">前のページ</span><span class="doc-pager-title">研究紹介</span></a>`,
		`<a class="doc-pager-link doc-pager-next" href="/articles/keion-live" rel="next"><span class="doc-pager-label">次のページ</span><span class="doc-pager-title">新歓ライブの記録</span></a>`)
	// サイドバーはパンくずリストと本文より前（左の列）に置き、本文はサイドバーの外に出す。
	if i, j := strings.Index(access.Body, `<aside class="doc-sidebar">`), strings.Index(access.Body, `<nav class="breadcrumb"`); i < 0 || j < i {
		t.Errorf("doc sidebar (%d) must come before the breadcrumb (%d)", i, j)
	}
	if strings.Contains(sidebar, `class="prose"`) {
		t.Error("article body is inside the doc sidebar")
	}

	// 1番目の子項目（目次のあるページ）: 前のページは無く、目次の2列もそのまま出す。
	research := c.get("/research").Body
	expectContains(t, research, `<div class="doc-layout">`, `<li><a href="/research" aria-current="page">研究紹介</a></li>`,
		`class="doc-pager-link doc-pager-next" href="/access"`, `<div class="article-layout">`, `class="toc toc-sidebar"`)
	expectNotContainsAny(t, "/research", research, "doc-pager-prev")

	// メニューの URL の項目で入れた記事: 前は固定ページ、次は外部のサイトなので出さない。
	live := c.get("/articles/keion-live").Body
	expectContains(t, live, `<div class="doc-layout">`, `<li><a href="/articles/keion-live" aria-current="page">新歓ライブの記録</a></li>`,
		`class="doc-pager-link doc-pager-prev" href="/access"`, `<article class="article">`)
	expectNotContainsAny(t, "/articles/keion-live", live, "doc-pager-next", `href="https://www.example.ac.jp/" rel="next"`)

	// メニューに無い固定ページ・記事は今までどおり（サイドバー・ページ送り・3列の枠を出さない）。
	for _, path := range []string{"/" + url.PathEscape("メンバー"), "/articles/library-room"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		expectNotContainsAny(t, path, res.Body, `class="doc-layout"`, `class="doc-sidebar"`, `class="doc-pager"`, `class="doc-main"`)
	}
	// 記事の一覧などのページには出さない（記事一覧はメニューの最上位の項目）。
	expectNotContainsAny(t, "/articles", c.get("/articles").Body, `class="doc-layout"`, `class="doc-sidebar"`)
}

// TestDocLayoutLinkedParent は、リンクのある親の項目のページではセクションの見出しを現在地にし、次のページを最初の
// 子項目にすることを確かめる。
func TestDocLayoutLinkedParent(t *testing.T) {
	env := newPagesEnv(t, Config{})
	st := SiteStructure{Menu: []MenuItem{{Type: MenuItemPage, Target: "access", Children: []MenuItem{
		{Type: MenuItemPage, Target: "research"},
	}}}}
	if err := env.server.saveSiteStructure(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()
	access := c.get("/access").Body
	expectContains(t, docSidebar(access), `<p class="doc-sidebar-title"><a href="/access" aria-current="page">アクセス</a></p>`,
		`<li><a href="/research">研究紹介</a></li>`, `<nav class="doc-sidebar-nav" aria-label="アクセス">`)
	expectContains(t, access, `class="doc-pager-link doc-pager-next" href="/research"`)
	expectNotContainsAny(t, "/access", access, "doc-pager-prev")

	research := c.get("/research").Body
	expectContains(t, docSidebar(research), `<p class="doc-sidebar-title"><a href="/access">アクセス</a></p>`,
		`<li><a href="/research" aria-current="page">研究紹介</a></li>`)
	expectContains(t, research, `<a class="doc-pager-link doc-pager-prev" href="/access" rel="prev">`)
}

// TestQuickSearchModalMarkup は、どの公開ページにも Cmd+K の検索の <dialog> を出し、公開サーバーでは /search を、
// 静的エクスポートでは検索用インデックスと static-search.js を使う目印を付けることを確かめる。
func TestQuickSearchModalMarkup(t *testing.T) {
	env := newPagesEnv(t, Config{})
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{Menu: docsMenu()}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()
	for _, path := range []string{"/", "/articles", "/access", "/articles/library-room", "/search?q=" + url.QueryEscape("自習室")} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<dialog id="search-modal" class="search-modal" aria-label="すばやく検索"`,
			`<form class="search-modal-form" action="/search" method="get" role="search">`,
			`role="combobox" aria-expanded="false" aria-controls="search-modal-results" aria-autocomplete="list"`,
			`<ul id="search-modal-results" class="search-modal-results" role="listbox" aria-label="検索結果"></ul>`,
			`data-msg-count="{n}件"`, `data-msg-all="「{q}」のすべての検索結果を見る"`,
			// JavaScript が無いときのヘッダーの検索窓はそのままフォームを送る。
			`<form class="search-form" action="/search" method="get" role="search">`)
		// 公開サーバーでは検索用インデックスを読まない。
		expectNotContainsAny(t, path, res.Body, "data-search-index", "static-search.js")
	}

	// 結果のパーシャルには、Cmd+K の検索が件数を読む data-total を付ける。
	partial := c.rawRequest(http.MethodGet, "/search?q="+url.QueryEscape("自習室"), map[string]string{"HX-Request": "true"})
	expectStatus(t, partial, http.StatusOK)
	expectContains(t, partial.Body, `<p class="result-count" role="status" data-total="1">`, `href="/articles/library-room"`)

	out := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, rel := range []string{"index.html", filepath.Join("access", "index.html"), filepath.Join("articles", "library-room", "index.html")} {
		b, err := os.ReadFile(filepath.Join(out, rel))
		if err != nil {
			t.Fatal(err)
		}
		expectContains(t, string(b), `<dialog id="search-modal" class="search-modal" aria-label="すばやく検索" data-search-index="/search-index.json" data-search-script="/static/js/static-search.js?v=`)
	}
	// 静的エクスポートでもドキュメント表示を書き出す。
	b, err := os.ReadFile(filepath.Join(out, "access", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	expectContains(t, string(b), `<aside class="doc-sidebar">`, `<li><a href="/access" aria-current="page">アクセス</a></li>`,
		`class="doc-pager-link doc-pager-prev" href="/research"`)
}

// cssLayerDepths は public.css の各位置の波かっこの深さを返す（コメントの中は数えない）。
func cssLayerDepths(css string) []int {
	depths := make([]int, len(css))
	depth := 0
	for i := 0; i < len(css); i++ {
		if strings.HasPrefix(css[i:], "/*") {
			end := strings.Index(css[i+2:], "*/")
			if end < 0 {
				end = len(css) - i - 2
			}
			for j := i; j < i+end+4 && j < len(css); j++ {
				depths[j] = depth
			}
			i += end + 3
			continue
		}
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
		}
		depths[i] = depth
	}
	return depths
}

// TestPublicDocsStyles は、ドキュメント表示・注意書き・タブ・コードブロック・Cmd+K の検索の見た目が public.css の
// レイヤー sorus-base の中にあり（テーマの CSS が優先される）、色はテーマのトークンを使うことを確かめる。
func TestPublicDocsStyles(t *testing.T) {
	data, err := fs.ReadFile(static.FS, "css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	layerStart := strings.Index(css, "@layer sorus-base {")
	if layerStart < 0 {
		t.Fatal("public.css has no @layer sorus-base")
	}
	depths := cssLayerDepths(css)
	for _, sel := range []string{
		".doc-layout {", ".doc-sidebar {", ".doc-sidebar-collapsible {", ".doc-sidebar-summary {", ".doc-sidebar a[aria-current=\"page\"]",
		".doc-pager {", ".doc-pager-prev", ".doc-pager-next {",
		".callout {", ".callout-note {", ".callout-tip {", ".callout-important {", ".callout-warning {", ".callout-caution {",
		".callout .callout-title {", ".callout-body > :last-child",
		".doc-tabs {", ".doc-tab-list {", ".doc-tab {", ".doc-tab[aria-selected=\"true\"]", ".doc-tab-panel {",
		".doc-tab-list[hidden], .doc-tab-panel[hidden], .doc-tab-title[hidden] { display: none !important; }",
		".code-block {", ".code-block-toolbar {", ".code-block-lang {", ".code-copy {", ".code-copy.is-copied",
		".search-field {", ".search-shortcut {", ".search-modal {", ".search-modal::backdrop", ".search-modal-result[aria-selected=\"true\"]",
	} {
		i := strings.Index(css, sel)
		if i < 0 {
			t.Errorf("public.css has no rule %q", sel)
			continue
		}
		if i < layerStart || depths[i] < 1 {
			t.Errorf("public.css rule %q is outside @layer sorus-base", sel)
		}
	}
	// 注意書きの note・タブ・ページ送り・サイドバーの現在地はテーマのアクセント色に従う。
	for _, want := range []string{
		"--callout-color: var(--callout-note, var(--accent));",
		".doc-tab[aria-selected=\"true\"] { border-bottom-color: var(--accent); color: var(--accent); }",
		".doc-sidebar a[aria-current=\"page\"] { background: var(--accent-soft); color: var(--accent);",
		"color-mix(in srgb, var(--callout-color) 62%, var(--fg))",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("public.css lacks %q", want)
		}
	}
	// 3列は PC の幅だけ。狭い画面のサイドバーは開閉する一覧のまま。
	if !strings.Contains(css, "@media (min-width: 1024px) {\n    main.container:has(> .doc-layout)") {
		t.Error("public.css does not widen the doc layout only on wide screens")
	}
}

// TestPublicDocsScript は、public.js がテンプレートと T1 の Markdown の出力（注意書き・タブ）に合わせて、タブの切り替え・
// コードのコピー・サイドバーの開閉・Cmd+K の検索を受け持つことを確かめる。動きそのものは実ブラウザで確認する。
func TestPublicDocsScript(t *testing.T) {
	read := func(name string) string {
		data, err := fs.ReadFile(static.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	js := read("js/public.js")
	for _, c := range []struct{ feature, code string }{
		{"タブを探す", `document.querySelectorAll(".doc-tabs[data-tabs]")`},
		{"タブの並びの hidden を外す", `list.hidden = false;`},
		{"選んでいないタブの中身を隠す", `p.panel.hidden = !on;`},
		{"タブの名前の行を隠す", `title.classList.contains("doc-tab-title")) title.hidden = true;`},
		{"aria-selected を切り替える", `p.tab.setAttribute("aria-selected", on ? "true" : "false");`},
		{"右の矢印キー", `e.key === "ArrowRight"`},
		{"左の矢印キー", `e.key === "ArrowLeft"`},
		{"コードブロックを探す（Mermaid の図は除く）", `document.querySelectorAll(".prose pre:not(.mermaid)")`},
		{"言語のクラス", `language-([A-Za-z0-9_+#.-]{1,32})`},
		{"クリップボードに写す", `navigator.clipboard.writeText(text)`},
		{"コピーできたことを知らせる", `"コピーしました"`},
		{"狭い画面ではサイドバーを閉じる", `details.open = wide ? wide.matches : window.innerWidth >= 1024;`},
		{"Cmd+K・Ctrl+K", `(e.metaKey || e.ctrlKey)`},
		{"検索の dialog", `document.getElementById("search-modal")`},
		{"静的エクスポートの検索用インデックス", `dialog.getAttribute("data-search-index")`},
		{"静的エクスポートの検索の部品", `window.sorusStaticSearch`},
		{"公開サーバーの /search のパーシャル", `{ headers: { "HX-Request": "true" }`},
		{"上下の矢印キーで選ぶ", `e.key === "ArrowDown" || e.key === "ArrowUp"`},
		{"変換の確定の Enter では移らない", `e.isComposing || e.keyCode === 229`},
		{"結果は文字と mark だけ写す", `child.tagName === "MARK"`},
		{"サイト内のリンクだけ", `/^\/(?!\/)/.test(url)`},
	} {
		if !strings.Contains(js, c.code) {
			t.Errorf("public.js lacks %s (%s)", c.feature, c.code)
		}
	}
	// 静的エクスポートの検索ページと Cmd+K の検索が、同じ照合の部品を使う。
	staticSearch := read("js/static-search.js")
	for _, code := range []string{"if (window.sorusStaticSearch) return;", "window.sorusStaticSearch = {", "find: find,", "load: load,",
		"function find(items, words, lang)", "load(indexURL)"} {
		if !strings.Contains(staticSearch, code) {
			t.Errorf("static-search.js lacks %q", code)
		}
	}
	// public.js が使う dialog の部品は、レイアウトにすべてある（名前を片方だけ変えると黙って動かなくなる）。
	layout, err := os.ReadFile(filepath.Join("..", "..", "templates", "public", "layout.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"search-modal-form", "search-modal-input", "search-modal-status", "search-modal-results", "search-modal-close"} {
		if !strings.Contains(js, `".`+class+`"`) {
			t.Errorf("public.js does not use .%s", class)
		}
		if !strings.Contains(string(layout), `class="`+class) {
			t.Errorf("layout.html has no .%s used by public.js", class)
		}
	}
}

// TestBuildFrontPageHierarchyNav は、トップページ向けの全体固定ページ階層サイドバー構築で、
// front ページ自身の除外・トップレベルと子ページの入れ子（.Children）・空のときの nil を確かめる。
func TestBuildFrontPageHierarchyNav(t *testing.T) {
	urlOf := func(p pageNavNode) string { return "/" + p.Slug }
	isFront := func(p pageNavNode) bool { return p.Slug == "home" }

	if got := buildFrontPageHierarchyNav(nil, "home-id", "ホーム", "/", isFront, urlOf); got != nil {
		t.Errorf("empty pages = %+v, want nil", got)
	}
	onlyFront := []pageNavNode{{ID: "home-id", Title: "ホーム", Slug: "home"}}
	if got := buildFrontPageHierarchyNav(onlyFront, "home-id", "ホーム", "/", isFront, urlOf); got != nil {
		t.Errorf("only front page = %+v, want nil", got)
	}

	pages := []pageNavNode{
		{ID: "home-id", Title: "ホーム", Slug: "home"},
		{ID: "guide", Title: "ガイド", Slug: "guide"},
		{ID: "install", Title: "インストール", Slug: "guide-install", ParentID: "guide"},
		{ID: "linux", Title: "Linux", Slug: "guide-install-linux", ParentID: "install"},
		{ID: "quickstart", Title: "クイックスタート", Slug: "guide-quickstart", ParentID: "guide"},
		{ID: "ref", Title: "リファレンス", Slug: "reference", ParentID: "home-id"},
	}
	got := buildFrontPageHierarchyNav(pages, "home-id", "ホーム", "/", isFront, urlOf)
	want := &docNav{
		Title:        "ホーム",
		TitleURL:     "/",
		TitleCurrent: true,
		Links: []docNavLink{
			{
				Label: "ガイド",
				URL:   "/guide",
				Children: []docNavLink{
					{
						Label:    "インストール",
						URL:      "/guide-install",
						Children: []docNavLink{{Label: "Linux", URL: "/guide-install-linux"}},
					},
					{Label: "クイックスタート", URL: "/guide-quickstart"},
				},
			},
			{Label: "リファレンス", URL: "/reference"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildFrontPageHierarchyNav =\n%s\nwant\n%s", describeHierarchyNav(got), describeHierarchyNav(want))
	}
}

// TestFrontPageDocSidebarLayout は、theme.json の layout.front_sidebar が true のときに GET / と GET /en/ で
// 該当言語の公開固定ページ階層が .doc-layout / .doc-sidebar に描画され、非公開ページや別言語ページが混入しないこと、
// また front_sidebar が false（既定）のときはシングルカラムを維持することを確かめる。
func TestFrontPageDocSidebarLayout(t *testing.T) {
	themesDir := t.TempDir()
	docThemeDir := filepath.Join(themesDir, "docs-theme")
	if err := os.MkdirAll(docThemeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docThemeDir, "theme.json"), []byte(`{
		"name": {"ja": "ドキュメント", "en": "Docs"},
		"description": {"ja": "ドキュメント用テーマ", "en": "Documentation theme"},
		"color_scheme": "light dark",
		"order": 100,
		"layout": {"front_sidebar": true}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docThemeDir, "theme.css"), []byte(`:root { --bg: #ffffff; }`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := newPagesEnv(t, Config{ThemesDir: themesDir})
	seedHierarchy(t, env)

	// 日本語と英語のフロント固定ページ（同じ translation_group_id で紐付け）を用意する。
	homeJA := seedPage(t, env.db, article.Article{
		Title: "SORUS ドキュメント", Slug: "home", ContentMarkdown: "トップ本文",
		Lang: "ja", TranslationGroupID: "grp-front-home",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour),
	})
	_ = homeJA
	seedPage(t, env.db, article.Article{
		Title: "SORUS Documentation", Slug: "home-en", ContentMarkdown: "English home body",
		Lang: "en", TranslationGroupID: "grp-front-home",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour),
	})

	// 英語の公開固定ページ階層と、英語の下書き固定ページを用意する。
	enGuide := seedPage(t, env.db, article.Article{
		Title: "User Guide", Slug: "user-guide", ContentMarkdown: "Guide body",
		Lang: "en", Status: article.StatusPublished, PublishedAt: ago(time.Hour), SortOrder: 1,
	})
	seedPage(t, env.db, article.Article{
		Title: "Installation", Slug: "installation", ContentMarkdown: "Install body",
		Lang: "en", Status: article.StatusPublished, PublishedAt: ago(time.Hour), ParentID: enGuide, SortOrder: 1,
	})
	seedPage(t, env.db, article.Article{
		Title: "CLI Reference", Slug: "cli-reference", ContentMarkdown: "CLI body",
		Lang: "en", Status: article.StatusPublished, PublishedAt: ago(time.Hour), SortOrder: 2,
	})
	seedPage(t, env.db, article.Article{
		Title: "Draft English Page", Slug: "draft-en", ContentMarkdown: "Draft",
		Lang: "en", Status: article.StatusDraft, ParentID: enGuide,
	})

	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		FrontPage: "home",
		FrontSections: []FrontSection{
			{Type: FrontSectionHero},
			{Type: FrontSectionCards, Pages: []string{"guide", "access"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	// 1. 既定テーマ（front_sidebar: false）では / と /en/ に doc-sidebar / doc-layout を出さずシングルカラムを維持する。
	for _, p := range []string{"/", "/en/"} {
		res := c.get(p)
		expectStatus(t, res, http.StatusOK)
		expectNotContainsAny(t, "default theme "+p, res.Body, `class="doc-layout"`, `class="doc-sidebar"`, `class="doc-main"`)
	}

	// 2. front_sidebar: true のテーマを適用すると、/ と /en/ に該当言語の公開固定ページ階層サイドバーが出る。
	if _, err := env.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('theme', 'docs-theme', CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}

	jaHome := c.get("/")
	expectStatus(t, jaHome, http.StatusOK)
	expectContains(t, jaHome.Body, `<div class="doc-layout">`, `<div class="doc-main">`, `<section class="front-hero"`, `<ul class="front-cards">`)
	// hero と cards の複数セクションがいずれも <div class="doc-main"> の内側にあり、途中で閉じたり余分な </div></div> が出ないこと。
	mainStart := strings.Index(jaHome.Body, `<main id="main" class="container">`)
	mainEnd := strings.Index(jaHome.Body, `</main>`)
	if mainStart < 0 || mainEnd <= mainStart {
		t.Fatalf("main container not found in GET /")
	}
	mainHTML := jaHome.Body[mainStart:mainEnd]
	docMainStart := strings.Index(mainHTML, `<div class="doc-main">`)
	heroIdx := strings.Index(mainHTML, `<section class="front-hero"`)
	cardsIdx := strings.Index(mainHTML, `<ul class="front-cards">`)
	cardsEndIdx := -1
	if cardsIdx >= 0 {
		if rel := strings.Index(mainHTML[cardsIdx:], `</ul>`); rel >= 0 {
			cardsEndIdx = cardsIdx + rel
		}
	}
	docCloseIdx := strings.LastIndex(mainHTML, `</div></div>`)
	if !(docMainStart >= 0 && docMainStart < heroIdx && heroIdx < cardsIdx && cardsIdx < cardsEndIdx && cardsEndIdx < docCloseIdx) {
		t.Errorf("hero and cards must both be inside .doc-main before closing </div></div>: docMain=%d hero=%d cards=%d cardsEnd=%d docClose=%d\n%s",
			docMainStart, heroIdx, cardsIdx, cardsEndIdx, docCloseIdx, mainHTML)
	}
	if strings.Contains(mainHTML[heroIdx:cardsIdx], "</div></div>") {
		t.Errorf(".doc-main/.doc-layout closed prematurely between hero and cards:\n%s", mainHTML[heroIdx:cardsIdx])
	}
	if openDivs, closeDivs := strings.Count(mainHTML, "<div"), strings.Count(mainHTML, "</div>"); openDivs != closeDivs {
		t.Errorf("unbalanced div tags in <main>: open=%d close=%d\n%s", openDivs, closeDivs, mainHTML)
	}
	jaSidebar := docSidebar(jaHome.Body)
	expectContains(t, jaSidebar,
		`<nav class="doc-sidebar-nav" aria-label="SORUS ドキュメント">`,
		`<p class="doc-sidebar-title"><a href="/" aria-current="page">SORUS ドキュメント</a></p>`,
		`<li><a href="/guide">ガイド</a>`,
		`<li><a href="/guide-install">インストール</a>`,
		`<li><a href="/guide-install-linux">Linux へのインストール</a></li>`,
		`<li><a href="/guide-quickstart">クイックスタート</a></li>`,
		`<li><a href="/guide-faq">よくある質問</a></li>`)
	// sort_order の並び順（install -> quickstart -> faq）が守られていること。
	if i, j, k := strings.Index(jaSidebar, "/guide-install\""), strings.Index(jaSidebar, "/guide-quickstart\""), strings.Index(jaSidebar, "/guide-faq\""); !(i >= 0 && i < j && j < k) {
		t.Errorf("ja front sidebar order install(%d) < quickstart(%d) < faq(%d) is broken:\n%s", i, j, k, jaSidebar)
	}
	// 非公開ページ（下書き・予約中）、英語ページ、フロントページ自身（/home）がサイドバーリンクに混入しないこと。
	expectNotContainsAny(t, "ja front sidebar", jaSidebar,
		"準備中の子ページ", "準備中のページ", "予約中の子ページ",
		"Quickstart (EN)", "User Guide", "Installation", "CLI Reference", "Draft English Page",
		`href="/home"`)

	enHome := c.get("/en/")
	expectStatus(t, enHome, http.StatusOK)
	expectContains(t, enHome.Body, `<div class="doc-layout">`, `<div class="doc-main">`, `<section class="front-hero"`)
	enSidebar := docSidebar(enHome.Body)
	expectContains(t, enSidebar,
		`<nav class="doc-sidebar-nav" aria-label="SORUS Documentation">`,
		`<p class="doc-sidebar-title"><a href="/en/" aria-current="page">SORUS Documentation</a></p>`,
		`<li><a href="/en/user-guide">User Guide</a>`,
		`<li><a href="/en/installation">Installation</a></li>`,
		`<li><a href="/en/cli-reference">CLI Reference</a></li>`)
	// 英語トップページのサイドバーに日本語ページや非公開英語ページ、フロントページ自身（/en/home-en）が混入しないこと。
	expectNotContainsAny(t, "en front sidebar", enSidebar,
		"ガイド", "インストール", "クイックスタート", "よくある質問", "研究紹介", "アクセス", "メンバー紹介",
		"Draft English Page", `href="/en/home-en"`)
}
