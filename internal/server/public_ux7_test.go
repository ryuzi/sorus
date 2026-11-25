package server

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/static"
)

// 正方形に近い図（100x80。以前は is-contain を付けていた比率）のアイキャッチでも、カード（記事一覧・トップの
// 新着記事とページのカード・関連記事）の画像には is-contain を付けず、枠いっぱいに cover で表示させる。
// 記事ページのアイキャッチ（.hero）には従来どおり付け、図の全体を収める。
func TestCardImagesAreNeverContainFit(t *testing.T) {
	env := newTestEnv(t)
	env.insertMedia("diagram", "diagram.png", "図", "", "")
	const src = "/uploads/diagram-1200.webp"
	for _, class := range []string{"card-image", "front-card-image", "related-image"} {
		tag := string(env.server.responsiveImage(src, "図", class))
		// アップロード済みの画像として解決されている（縦横の大きさを読んだ）ことを srcset で確かめる。
		if !strings.Contains(tag, "srcset=") || !strings.Contains(tag, `class="`+class+`"`) {
			t.Fatalf("%s: tag = %q, want a responsive image with only the card class", class, tag)
		}
		if strings.Contains(tag, "is-contain") {
			t.Errorf("%s: card image must not be contain-fit: %q", class, tag)
		}
	}
	if tag := string(env.server.responsiveImage(src, "図", "hero-image")); !strings.Contains(tag, `class="hero-image is-contain"`) {
		t.Errorf("hero image = %q, want the contain class for a diagram", tag)
	}

	seedArticle(t, env.db, article.Article{Title: "図の記事", Slug: "diagram-post", FeaturedImage: src,
		ContentMarkdown: "本文に画像はありません。", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	c := env.newClient()
	cardImg := regexp.MustCompile(`<a href="/articles/diagram-post" class="card-media"[^>]*><img [^>]*>`)
	for _, path := range []string{"/", "/articles"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		card := cardImg.FindString(res.Body)
		if card == "" || !strings.Contains(card, `class="card-image"`) || strings.Contains(card, "is-contain") {
			t.Errorf("%s: card image = %q, want card-image without is-contain", path, card)
		}
	}
	res := c.get("/articles/diagram-post")
	expectStatus(t, res, http.StatusOK)
	if hero := regexp.MustCompile(`<figure class="hero"><img [^>]*>`).FindString(res.Body); !strings.Contains(hero, "is-contain") {
		t.Errorf("article hero = %q, want the contain class", hero)
	}
}

// 「記事や一覧に執筆者名を表示する」が OFF のとき、トップに固定ページを出しているサイトの「新着記事」のカードにも
// 執筆者名を出さない。ON に戻せば出る。
func TestFrontNewsHidesAuthorWhenShowAuthorOff(t *testing.T) {
	env := newFrontEnv(t) // ShowAuthor: false（既定）
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontPage: "research"}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()
	frontNews := func() string {
		t.Helper()
		res := c.get("/")
		expectStatus(t, res, http.StatusOK)
		_, news, ok := strings.Cut(res.Body, `<section class="front-news"`)
		if !ok {
			t.Fatalf("front page has no news section: %s", res.Body)
		}
		news, _, _ = strings.Cut(news, `</section>`)
		return news
	}

	news := frontNews()
	expectContains(t, news, "お知らせ1", "お知らせ5")
	if strings.Contains(news, studentA.DisplayName) {
		t.Errorf("front news shows the author %q while show_author is off:\n%s", studentA.DisplayName, news)
	}

	env.server.setShowAuthor(true)
	expectContains(t, frontNews(), "・"+studentA.DisplayName)
}

// 記事一覧のフィルタバーは2列のグリッド（左: カテゴリのピル、右: 年のセレクト。上端で揃える）。長いカテゴリ名で
// ピルが折り返しても、年のセレクトが縦中央に浮いたりピルの並びに割り込んだりしない。狭い画面では1列に積む。
func TestArticlesFilterBarGridLayout(t *testing.T) {
	data, err := fs.ReadFile(static.FS, "css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	ruleBody := func(within, selector string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`).FindStringSubmatch(within)
		if m == nil {
			t.Fatalf("no rule for %s", selector)
		}
		return m[1]
	}
	bar := ruleBody(css, ".articles-filter-bar")
	for _, want := range []string{"display: grid", "grid-template-columns: minmax(0, 1fr) auto", "align-items: start", "gap: 12px 16px"} {
		if !strings.Contains(bar, want) {
			t.Errorf(".articles-filter-bar { %s } is missing %q", bar, want)
		}
	}
	if h := ruleBody(css, ".articles-header-row"); !strings.Contains(h, "justify-content: space-between") || !strings.Contains(h, "flex-wrap: wrap") {
		t.Errorf(".articles-header-row { %s } must put the title and the year select on one row", h)
	}
	if !strings.Contains(ruleBody(css, ".articles-header-row .page-title"), "flex: 1 1 auto") {
		t.Error(".articles-header-row .page-title must take the free width")
	}
	if !strings.Contains(ruleBody(css, ".articles-filter-bar > .category-filter-pills:only-child"), "grid-column: 1 / -1") {
		t.Error("a lone category pills row must span the full filter bar")
	}
	if pills := ruleBody(css, ".category-filter-pills"); !strings.Contains(pills, "flex-wrap: wrap") || !strings.Contains(pills, "min-width: 0") {
		t.Errorf(".category-filter-pills { %s } must wrap inside its column", pills)
	}
	_, narrow, ok := strings.Cut(css, "@media (max-width: 640px) {\n    .articles-filter-bar")
	if !ok {
		t.Fatal("no narrow-screen rule for .articles-filter-bar")
	}
	if !strings.HasPrefix(narrow, " { grid-template-columns: minmax(0, 1fr); }") {
		t.Errorf("narrow .articles-filter-bar = %.80q, want a single stacked column", narrow)
	}
	// カードの画像は is-contain に頼らず、どれも cover で枠を埋める。
	for _, sel := range []string{".card-media img", ".front-card-media img", ".related-article-media img"} {
		if d := ruleBody(css, sel); !strings.Contains(d, "object-fit: cover") {
			t.Errorf("%s { %s } must use object-fit: cover", sel, d)
		}
	}

	// 長いカテゴリ名を含む記事一覧で、フィルタバーの直下がピルの列と年のセレクトの列の2つだけになっている。
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		for i, name := range []string{"研究室の日常・教育", "学会・研究発表", "受賞・表彰", "メディア掲載"} {
			cat := seedCategory(t, db, name, "cat-"+string(rune('a'+i)), "")
			published := time.Date(2024+i%2, time.April, 1, 9, i, 0, 0, time.UTC)
			seedArticle(t, db, article.Article{Title: name + "の記事", Slug: "ux7-post-" + string(rune('a'+i)), CategoryID: cat,
				ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: &published})
		}
	})
	res := env.newClient().get("/articles")
	expectStatus(t, res, http.StatusOK)
	_, headerHTML, ok := strings.Cut(res.Body, `<div class="articles-header-row">`)
	if !ok {
		t.Fatalf("no header row: %s", res.Body)
	}
	headerHTML, _, _ = strings.Cut(headerHTML, `<div class="card-grid">`)
	// 見出し行: 見出し（.page-title）と年のセレクトが並ぶ。年のセレクトは見出し行の右端にあり、フィルタバーには入らない。
	headerRow := regexp.MustCompile(`(?s)^\s*<h1 class="page-title">[^<]*</h1>\s*<div class="year-filter-select">.*?</div>\s*</div>\s*<div class="articles-filter-bar">`)
	if !headerRow.MatchString(headerHTML) {
		t.Errorf("header row must hold the page title and the year select, followed by the filter bar:\n%.600s", headerHTML)
	}
	// フィルタバーには .category-filter-pills のみ（年のセレクトは含まない）。
	_, barHTML, _ := strings.Cut(headerHTML, `<div class="articles-filter-bar">`)
	expectContains(t, barHTML, ">研究室の日常・教育 <span class=\"category-filter-count\">(1)</span></a>")
	pillsOnly := regexp.MustCompile(`(?s)^\s*<nav class="category-filter-pills"[^>]*>.*?</nav>\s*</div>`)
	if !pillsOnly.MatchString(barHTML) {
		t.Errorf("filter bar must hold only the category pills:\n%.600s", barHTML)
	}
}

// バックアップ画面の手動バックアップは「手動でバックアップをダウンロード」と呼び、自動バックアップと中身が同じ
// （違いは保存先と作るタイミングだけ）ことを、自動バックアップのあるデスクトップモードで補足する。
// MCP 連携画面の説明文は幅を絞らずに1行で流し、「?」は文末に並べる。
func TestUX7AdminBackupAndMCPCopy(t *testing.T) {
	server := newTestEnv(t).loggedIn(admin)
	body := server.get("/admin/settings/backup").Body
	expectContains(t, body, `<h2 id="backup-create-title"`, "手動でバックアップをダウンロード",
		`<p id="backup-manual-note"`, "データベース・アップロード画像・添付資料（ログイン中の情報と独自テーマは含みません）")
	expectNotContains(t, body, "完全バックアップを作成", "自動バックアップと同じ内容")

	desktop := newTestEnvWith(t, Config{Desktop: true}, nil).loggedIn(admin)
	body = desktop.get("/admin/settings/backup").Body
	expectContains(t, body, `id="auto-backup"`, "手動でバックアップをダウンロード",
		"自動バックアップと同じ内容を、いますぐこのブラウザでダウンロードします。", "違いは保存先と作るタイミングだけです")

	body = server.get("/admin/settings/mcp").Body
	desc := regexp.MustCompile(`(?s)<p id="mcp-description"[^>]*>.*?</p>`).FindString(body)
	if !strings.HasPrefix(desc, `<p id="mcp-description" class="mb-5 text-sm text-neutral-600">`) ||
		!strings.Contains(desc, `<span class="help-tip">`) || strings.Contains(desc, "max-w-") || strings.Contains(desc, "flex") {
		t.Errorf("MCP description = %q, want a full-width paragraph with the help tip inline at the end", desc)
	}
}
