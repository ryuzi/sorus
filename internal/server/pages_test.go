package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/rag"
)

// seedPage は公開日時 publishedAt の固定ページ（kind = page）を保存し、ID を返す。
func seedPage(t *testing.T, db *sqlx.DB, a article.Article) string {
	t.Helper()
	a.Kind = article.KindPage
	return seedArticle(t, db, a)
}

// newPagesEnv は記事と固定ページが混在するサイトを起動する。最も新しく公開したのはページ（研究紹介）で、
// ページだけを持つカテゴリ（研究室）がある。
func newPagesEnv(t *testing.T, cfg Config) *testEnv {
	t.Helper()
	cfg.BaseURL, cfg.SiteName, cfg.SiteDescription = testBaseURL, "テスト研究室", "研究室のサイト"
	return newTestEnvWith(t, cfg, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		circle := seedCategory(t, db, "サークル", "circle", "")
		lab := seedCategory(t, db, "研究室", "lab", "")
		seedArticle(t, db, article.Article{Title: "軽音楽部の新歓ライブ", Slug: "keion-live", CategoryID: circle,
			ContentMarkdown: "量子計算の話題も出たライブ。", Status: article.StatusPublished, PublishedAt: ago(2 * time.Hour)})
		seedArticle(t, db, article.Article{Title: "図書館の新しい自習室", Slug: "library-room",
			ContentMarkdown: "自習室がオープンした。", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
		seedPage(t, db, article.Article{Title: "研究紹介", Slug: "research", Excerpt: "量子計算の研究室です",
			ContentMarkdown: "## 研究テーマ\n\n量子計算を研究しています。\n\n## 設備\n\n実験室があります。",
			Status:          article.StatusPublished, PublishedAt: ago(30 * time.Minute)})
		seedPage(t, db, article.Article{Title: "アクセス", Slug: "access", ContentMarkdown: "駅から徒歩5分。",
			Status: article.StatusPublished, PublishedAt: ago(3 * time.Hour)})
		seedPage(t, db, article.Article{Title: "メンバー紹介", Slug: "メンバー", CategoryID: lab, ContentMarkdown: "教員と学生。",
			Status: article.StatusPublished, PublishedAt: ago(4 * time.Hour)})
		seedPage(t, db, article.Article{Title: "準備中のページ", Slug: "draft-page", ContentMarkdown: "準備中"})
	})
}

// expectMovedTo は 301 で location へ転送することを確かめる。
func expectMovedTo(t *testing.T, res *response, location string) {
	t.Helper()
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != location {
		t.Fatalf("%s: status %d Location %q, want 301 %q", res.Request.URL.Path, res.StatusCode, res.Header.Get("Location"), location)
	}
}

func expectNotContainsAny(t *testing.T, label, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("%s contains %q", label, u)
		}
	}
}

func TestPublicPageServedAtSlug(t *testing.T) {
	env := newPagesEnv(t, Config{})
	c := env.newClient()

	res := c.get("/research")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`<article class="page">`,
		`<h1 class="article-title">研究紹介</h1>`,
		`<span class="toc-title">目次</span>`, `<div class="prose" data-math-root>`, "量子計算を研究しています。",
		"<title>研究紹介 | テスト研究室</title>",
		`<link rel="canonical" href="https://media.example.ac.jp/research">`,
		`<meta property="og:type" content="website">`,
		`<meta property="og:image" content="https://media.example.ac.jp/og-image/research.png">`,
		`<li aria-current="page">研究紹介</li>`,
	)
	// 日付・著者・カテゴリ・関連記事・学術メタデータは出さない。
	expectNotContainsAny(t, "page", res.Body, "<time", "・文:", `class="badge"`, "related-articles", "scholarly-meta",
		`class="article"`, "NewsArticle")

	ld := jsonLD(t, res.Body)
	page, ok := ld["WebPage"]
	if !ok {
		t.Fatalf("no WebPage JSON-LD: %v", ld)
	}
	if page["name"] != "研究紹介" || page["url"] != "https://media.example.ac.jp/research" || page["description"] != "量子計算の研究室です" {
		t.Errorf("WebPage = %v", page)
	}
	if _, ok := page["datePublished"]; ok {
		t.Errorf("WebPage has datePublished: %v", page)
	}
	crumbs, _ := ld["BreadcrumbList"]["itemListElement"].([]any)
	if len(crumbs) != 2 {
		t.Fatalf("breadcrumb = %v, want ホーム › 研究紹介", crumbs)
	}
	if last, _ := crumbs[1].(map[string]any); last["name"] != "研究紹介" || last["item"] != "https://media.example.ac.jp/research" {
		t.Errorf("breadcrumb last = %v", last)
	}

	// 日本語スラッグのページも /<slug> で開ける。
	res = c.get("/" + url.PathEscape("メンバー"))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "教員と学生。")

	head, err := http.NewRequest(http.MethodHead, env.srv.URL+"/research", nil)
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, c.do(head), http.StatusOK)
	// 動的 OGP 画像も描ける。
	og := c.get("/og-image/research.png")
	expectStatus(t, og, http.StatusOK)
	if ct := og.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("og image Content-Type = %q", ct)
	}
}

func TestPublicPageRedirectsAndNotFound(t *testing.T) {
	env := newPagesEnv(t, Config{})
	c := env.newClient()

	// 種類を変えても古いリンクが切れないよう、互いの URL へ 301 で転送する。
	expectMovedTo(t, c.get("/articles/research"), "/research")
	expectMovedTo(t, c.get("/keion-live"), "/articles/keion-live")
	expectMovedTo(t, c.get("/articles/"+url.PathEscape("メンバー")), "/"+url.PathEscape("メンバー"))

	for _, p := range []string{"/draft-page", "/articles/draft-page", "/no-such-page", "/research/extra"} {
		res := c.get(p)
		expectStatus(t, res, http.StatusNotFound)
		expectContains(t, res.Body, "ページが見つかりません")
	}
	// GET・HEAD 以外は、ページのルートが無かったときと同じ 404。
	for _, p := range []string{"/research", "/no-such-page", "/api/sync"} {
		req, err := http.NewRequest(http.MethodPost, env.srv.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, c.do(req), http.StatusNotFound)
	}
	// 固定のパスはページのルートより優先する。
	expectContains(t, c.get("/search?q=量子").Body, `<meta name="robots" content="noindex">`)
	expectStatus(t, c.get("/articles"), http.StatusOK)
}

func TestPagesAreNotInTimeOrderedLists(t *testing.T) {
	env := newPagesEnv(t, Config{})
	env.server.ragService = rag.NewService(env.db, nil)
	// すべて同じ埋め込みにして、ページが関連記事の候補に入る状況を作る。
	for _, slug := range []string{"keion-live", "library-room", "research", "access"} {
		setEmbedding(t, env.db, env.articleIDBySlug(slug), []float32{1, 0})
	}
	c := env.newClient()
	pages := []string{"研究紹介", "アクセス", "メンバー紹介"}

	home := c.get("/")
	expectStatus(t, home, http.StatusOK)
	expectContains(t, home.Body, "軽音楽部の新歓ライブ", "図書館の新しい自習室")
	expectNotContainsAny(t, "/", home.Body, pages...)
	// ページしか無いカテゴリはナビゲーションに出さない。
	expectContains(t, home.Body, `href="/categories/circle"`)
	expectNotContainsAny(t, "nav", home.Body, `href="/categories/lab"`)

	list := c.get("/articles")
	expectStatus(t, list, http.StatusOK)
	expectContains(t, list.Body, "<title>記事一覧 | テスト研究室</title>", `<h1 class="page-title">記事一覧</h1>`,
		`<link rel="canonical" href="https://media.example.ac.jp/articles">`, `<div class="card-grid">`,
		`href="/articles/keion-live"`, `href="/articles/library-room"`)
	expectNotContainsAny(t, "/articles", list.Body, pages...)

	expectNotContainsAny(t, "/categories/uncategorized", c.get("/categories/uncategorized").Body, "アクセス", "研究紹介")
	lab := c.get("/categories/lab")
	expectStatus(t, lab, http.StatusOK)
	expectNotContainsAny(t, "/categories/lab", lab.Body, "メンバー紹介")

	feed := c.get("/feed.xml")
	expectContains(t, feed.Body, "軽音楽部の新歓ライブ")
	expectNotContainsAny(t, "feed", feed.Body, append(pages, "/research")...)

	// 記事の関連記事にページを出さず、ページからは関連記事を推薦しない。
	related := relatedTitles(t, c.get("/articles/keion-live").Body)
	if fmt.Sprint(related) != "[図書館の新しい自習室]" {
		t.Errorf("related of a post = %v, want only the other post", related)
	}
	expectNotContainsAny(t, "page", c.get("/research").Body, "related-articles")
}

func TestPagesAreDiscoverable(t *testing.T) {
	env := newPagesEnv(t, Config{})
	c := env.newClient()

	sitemap := c.get("/sitemap.xml").Body
	expectContains(t, sitemap, "<loc>https://media.example.ac.jp/research</loc>", "<loc>https://media.example.ac.jp/access</loc>",
		"<loc>https://media.example.ac.jp/articles/keion-live</loc>")
	expectNotContainsAny(t, "sitemap", sitemap, "/articles/research", "draft-page")

	llms := c.get("/llms.txt").Body
	expectContains(t, llms, "## Pages\n- [研究紹介](https://media.example.ac.jp/research): 量子計算の研究室です\n",
		"- [アクセス](https://media.example.ac.jp/access): ", "## Articles\n- [図書館の新しい自習室](https://media.example.ac.jp/articles/library-room)")
	pagesAt, articlesAt := strings.Index(llms, "## Pages"), strings.Index(llms, "## Articles")
	if pagesAt < 0 || articlesAt < pagesAt || strings.Contains(llms[articlesAt:], "/research") {
		t.Errorf("llms.txt sections are wrong:\n%s", llms)
	}
	full := c.get("/llms-full.txt").Body
	expectContains(t, full, "# 研究紹介\n\nURL: https://media.example.ac.jp/research\n\n## 研究テーマ",
		"URL: https://media.example.ac.jp/articles/keion-live")

	res := c.get("/search?q=" + url.QueryEscape("量子計算"))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<a href="/research">`, `<a href="/articles/keion-live">`)
	expectNotContainsAny(t, "search", res.Body, `href="/articles/research"`)
	// ページの検索結果には日付を出さない（記事には出す）。
	if n := strings.Count(res.Body, "<time "); n != 1 {
		t.Errorf("search results have %d dates, want 1 (only the post)", n)
	}
}

func TestPageViewsAreCounted(t *testing.T) {
	env := newPagesEnv(t, Config{})
	reader := env.newClient()
	expectStatus(t, reader.getAs("/research", browserUA, nil), http.StatusOK)
	env.flushAnalytics()
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily WHERE article_id = ?`, env.articleIDBySlug("research")); n != 1 {
		t.Errorf("page views = %d, want 1", n)
	}
	rows, err := env.server.popularArticles(context.Background(), 5, 7)
	if err != nil {
		t.Fatal(err)
	}
	// 公開サイトの URL（testBaseURL）があるので、本番の URL で開く。
	if len(rows) != 1 || rows[0].PublicURL != testBaseURL+"/research" {
		t.Errorf("popular = %+v, want the page with %s/research", rows, testBaseURL)
	}
}

// 週間人気記事の公開ページ: 公開サイトの URL（ループバック以外）か公開サーバーの URL があれば本番の URL、
// どちらも無ければこの PC の相対パスで開く。
func TestPopularArticlesPublicURL(t *testing.T) {
	env := newPagesEnv(t, Config{})
	env.server.setBaseURL("") // 起動時の公開サイトの URL も無いことにして、保存した設定だけで決まるようにする
	reader := env.newClient()
	expectStatus(t, reader.getAs("/research", browserUA, nil), http.StatusOK)
	env.flushAnalytics()
	set := func(key, value string) {
		t.Helper()
		if err := upsertSetting(env.db, key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, baseURL, remoteURL, want string
	}{
		{"none", "", "", "/research"},
		{"loopback base_url", "http://127.0.0.1:8080", "", "/research"},
		{"localhost base_url", "http://localhost:8080/", "", "/research"},
		{"base_url", "https://test-driver.com", "", "https://test-driver.com/research"},
		{"base_url with trailing slash and sub path", "https://user.github.io/site/", "", "https://user.github.io/site/research"},
		{"base_url wins over remote", "https://test-driver.com/", "https://remote.example.com", "https://test-driver.com/research"},
		{"remote when base_url is loopback", "http://127.0.0.1:8080", "https://remote.example.com", "https://remote.example.com/research"},
		{"remote when base_url is empty", "", "https://remote.example.com/", "https://remote.example.com/research"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set("base_url", tc.baseURL)
			set(remoteSettingURL, tc.remoteURL)
			rows, err := env.server.popularArticles(context.Background(), 5, 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].PublicURL != tc.want {
				t.Errorf("popular = %+v, want PublicURL %q", rows, tc.want)
			}
		})
	}
}

func TestArticlesListPagination(t *testing.T) {
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		for i := 1; i <= publicPerPage+1; i++ {
			seedArticle(t, db, article.Article{Title: fmt.Sprintf("記事%02d", i), Slug: fmt.Sprintf("post-%d", i),
				Status: article.StatusPublished, PublishedAt: ago(time.Duration(i) * time.Hour)})
		}
		seedPage(t, db, article.Article{Title: "最新のページ", Slug: "newest-page", Status: article.StatusPublished, PublishedAt: ago(time.Minute)})
	})
	c := env.newClient()
	first := c.get("/articles")
	expectStatus(t, first, http.StatusOK)
	if n := strings.Count(first.Body, `<article class="card">`); n != publicPerPage {
		t.Errorf("first page has %d cards, want %d", n, publicPerPage)
	}
	expectContains(t, first.Body, `href="/articles/page/2" rel="next"`, "記事01")
	expectStatus(t, c.get("/articles?page=2"), http.StatusOK)
	second := c.get("/articles/page/2")
	expectStatus(t, second, http.StatusOK)
	expectContains(t, second.Body, "<title>記事一覧（2ページ目） | sorus</title>", "記事13", `href="/articles" rel="prev"`)
	if n := strings.Count(second.Body, `<article class="card">`); n != 1 {
		t.Errorf("second page has %d cards, want 1", n)
	}
	expectNotContainsAny(t, "/articles", first.Body+second.Body, "最新のページ")
	expectStatus(t, c.get("/articles?page=3"), http.StatusNotFound)
	expectStatus(t, c.get("/articles/page/3"), http.StatusNotFound)
	expectStatus(t, c.get("/articles?page=x"), http.StatusNotFound)

	empty := newTestEnv(t).newClient().get("/articles")
	expectStatus(t, empty, http.StatusOK)
	expectContains(t, empty.Body, "まだ公開された記事はありません。")
}

// reservedPageWords は仕様（サイト構成 3.2）の予約語。
var reservedPageWords = []string{"admin", "api", "articles", "categories", "search", "static", "uploads", "attachments",
	"og", "feed.xml", "sitemap.xml", "robots.txt", "favicon.ico", "llms.txt", "llms-full.txt", "manifest.json",
	"sw.js", "healthz"}

// pageForm はページとして保存する編集フォームの値。
func pageForm(title, slug, action string) url.Values {
	return url.Values{"title": {title}, "slug": {slug}, "kind": {"page"}, "category_id": {"1"},
		"content_markdown": {"本文"}, "action": {action}}
}

func TestPublishingExistingFrontPageKeepsSlugAndHTMLClasses(t *testing.T) {
	env := newTestEnv(t)
	id := seedPage(t, env.db, article.Article{Title: "トップページ", Slug: "front-page",
		ContentMarkdown: "旧本文", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	if _, err := env.db.Exec(`UPDATE articles SET slug = 'front_page' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	a := env.mustGetArticle(id)
	form := pageForm("トップページ更新", "front_page", "publish")
	form.Set("base_updated_at", fmt.Sprint(a.UpdatedAt.Unix()))
	form.Set("content_markdown", `<p class="hero-affiliation">研究室</p>

<figure class="emblem-fig"><img src="/uploads/emblem.webp" alt="校章" class="emblem-img"></figure>`)
	ed := env.loggedIn(chief)
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")

	updated := env.mustGetArticle(id)
	if updated.Slug != "front_page" {
		t.Fatalf("updated slug = %q, want front_page", updated.Slug)
	}
	res := env.newClient().get("/front_page")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `class="hero-affiliation"`, `class="emblem-fig"`, `class="emblem-img"`)
}

func TestPageReservedSlugIsRejected(t *testing.T) {
	env := newTestEnvWith(t, Config{SyncAPIPath: "/sync-secret/endpoint"}, nil)
	writer := env.loggedIn(studentA)

	for _, w := range reservedPageWords {
		for _, slug := range []string{w, strings.ToUpper(w)} {
			res := writer.postForm("/admin/articles/new", pageForm("ページ", slug, "draft"))
			if article.NormalizeSlug(w) != w {
				// 記号を含む予約語（feed.xml など）は正規化で記号が落ち、予約したパスとは重ならない。
				expectRedirect(t, res, "/admin/articles/")
				continue
			}
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, res.Body, "スラッグ「"+w+"」はページには使えません", `value="page" selected`)
		}
	}
	// 同期 API のパスの先頭部分も使えない。スラッグが空ならタイトルから作ったスラッグを検査する。
	for _, f := range []url.Values{pageForm("同期", "sync-secret", "draft"), pageForm("Search", "", "draft")} {
		expectStatus(t, writer.postForm("/admin/articles/new", f), http.StatusUnprocessableEntity)
	}
	// 英語の画面では英語で知らせる。
	writer.get("/admin/set-lang?lang=en")
	res := writer.postForm("/admin/articles/new?lang=en", pageForm("Page", "admin", "draft"))
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "cannot be used for a page")

	// 記事（/articles/<slug>）は予約語のスラッグでもよい。
	post := url.Values{"title": {"検索の記事"}, "slug": {"search"}, "category_id": {"1"}, "action": {"draft"}}
	expectRedirect(t, writer.postForm("/admin/articles/new?lang=ja", post), "/admin/articles/")

	// 保存済みのページを予約語に変える保存・校閲申請も拒む。
	loc := expectRedirect(t, writer.postForm("/admin/articles/new?lang=ja", pageForm("研究", "research", "draft")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	expectStatus(t, writer.postForm("/admin/articles/"+id+"?lang=ja", pageForm("研究", "api", "draft")), http.StatusUnprocessableEntity)
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/submit-review?lang=ja", pageForm("研究", "static", "review")), http.StatusUnprocessableEntity)
	if a := env.mustGetArticle(id); a.Slug != "research" || a.Status != article.StatusDraft {
		t.Errorf("page after rejected saves = %s / %s, want research / draft", a.Slug, a.Status)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE kind = 'page' AND slug IN ('admin', 'api', 'search', 'sync-secret', 'static')`); n != 0 {
		t.Errorf("%d pages were saved with reserved slugs", n)
	}
}

var (
	kindPageSelected = regexp.MustCompile(`<option value="page" selected>`)
	categoryHidden   = regexp.MustCompile(`<div id="category-field" hidden>`)
)

func TestAdminArticleListKindSwitch(t *testing.T) {
	env := newPagesEnv(t, Config{})
	writer := env.loggedIn(studentA)

	posts := writer.get("/admin/articles?status=published")
	expectStatus(t, posts, http.StatusOK)
	expectContains(t, posts.Body, "軽音楽部の新歓ライブ", `id="kind-switch"`,
		`id="new-article-button"`,
		`href="/admin/articles?kind=post" aria-current="page"`, `href="/admin/articles?kind=page&status=published"`)
	expectNotContainsAny(t, "article list header", posts.Body, `id="nav-new-article"`)
	expectNotContainsAny(t, "post list", posts.Body, "研究紹介", "アクセス", `id="new-page-button"`)

	pages := writer.get("/admin/articles?status=published&kind=page")
	expectStatus(t, pages, http.StatusOK)
	expectContains(t, pages.Body, "研究紹介", "アクセス", "メンバー紹介", `href="/research"`,
		`id="new-page-button"`, `href="/admin/articles/new?kind=page"`, "新しいページ",
		`href="/admin/articles?kind=page&status=published" aria-current="page"`,
		// 状態のタブは表示中の種類のまま切り替える。
		`href="/admin/articles?status=draft&kind=page" role="tab"`)
	expectNotContainsAny(t, "page list", pages.Body, "軽音楽部の新歓ライブ", `id="new-article-button"`)
	// 種類の切り替えに、総件数を出す（記事2件・ページ4件）。
	if !regexp.MustCompile(`(?s)記事.*2件.*ページ.*4件`).MatchString(pages.Body) {
		t.Errorf("kind switch counts are missing:\n%s", pages.Body)
	}
	// kind=page は status 未指定時、デフォルトで published が表示される。
	defaultPages := writer.get("/admin/articles?kind=page")
	expectContains(t, defaultPages.Body, "研究紹介", "アクセス")
	// 下書きタブでは準備中のページが表示される。
	drafts := writer.get("/admin/articles?kind=page&status=draft")
	expectContains(t, drafts.Body, "準備中のページ")
	// 不明な種類は記事の一覧にする。
	expectContains(t, writer.get("/admin/articles?kind=widget&status=published").Body, "軽音楽部の新歓ライブ")

	// 英語の画面の切り替え。
	writer.useEnglish()
	en := writer.get("/admin/articles?kind=page")
	expectContains(t, en.Body, "New page", "Article", "Page")
}

func TestPageWritingFlowFollowsRoles(t *testing.T) {
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		seedCategory(t, db, "ニュース", "news", "")
		seedCategory(t, db, "未分類", "uncategorized", "")
	})
	writer := env.loggedIn(studentA)
	editor := env.loggedIn(chief)

	newPage := writer.get("/admin/articles/new?kind=page")
	expectStatus(t, newPage, http.StatusOK)
	expectContains(t, newPage.Body, "新しいページ")
	if !kindPageSelected.MatchString(newPage.Body) || !categoryHidden.MatchString(newPage.Body) {
		t.Errorf("new page form does not select page and hide the category:\n%.3000s", newPage.Body)
	}
	// 新しい記事の画面は今までどおり記事で、カテゴリを選べる。
	newPost := writer.get("/admin/articles/new")
	if kindPageSelected.MatchString(newPost.Body) || categoryHidden.MatchString(newPost.Body) {
		t.Error("new article form is a page")
	}

	// ライターはページの下書きを作れる。カテゴリの欄が無くても既定のカテゴリ（未分類）に入る。
	form := pageForm("研究室について", "about", "draft")
	form.Del("category_id")
	loc := expectRedirect(t, writer.postForm("/admin/articles/new", form), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	a := env.mustGetArticle(id)
	var uncategorized int64
	if err := env.db.Get(&uncategorized, `SELECT id FROM categories WHERE slug = 'uncategorized'`); err != nil {
		t.Fatal(err)
	}
	if a.Kind != article.KindPage || a.Status != article.StatusDraft || a.CategoryID != uncategorized {
		t.Fatalf("created page = kind %s status %s category %d, want page / draft / %d", a.Kind, a.Status, a.CategoryID, uncategorized)
	}
	edit := writer.get(loc)
	expectContains(t, edit.Body, "ページの編集", `href="/admin/articles?kind=page"`)
	if !kindPageSelected.MatchString(edit.Body) {
		t.Error("edit form does not select page")
	}
	// 種類の欄の無い送信（古い画面など）では種類を変えない。
	noKind := pageForm("研究室について", "about", "draft")
	noKind.Del("kind")
	expectRedirect(t, writer.postForm("/admin/articles/"+id, noKind), "/admin/articles/"+id)
	if env.mustGetArticle(id).Kind != article.KindPage {
		t.Error("saving without the kind field changed the kind")
	}

	// ライターは公開できず、校閲を申請する。
	expectStatus(t, writer.postForm("/admin/articles/"+id, pageForm("研究室について", "about", "publish")), http.StatusForbidden)
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/submit-review", pageForm("研究室について", "about", "review")),
		"/admin/articles?status=review&kind=page&flash=submitted")
	if a := env.mustGetArticle(id); a.Status != article.StatusReview || a.Kind != article.KindPage {
		t.Fatalf("after submit = %s / %s, want review / page", a.Status, a.Kind)
	}
	expectStatus(t, env.newClient().get("/about"), http.StatusNotFound)

	// 編集長が承認して公開する。
	expectRedirect(t, editor.postForm("/admin/articles/"+id+"/approve-publish", url.Values{}), "/admin/articles/"+id+"/edit")
	expectStatus(t, env.newClient().get("/about"), http.StatusOK)

	// 公開中のページの種類を変えられるのは編集長だけ（他の変更と同じ権限）。
	toPost := pageForm("研究室について", "about", "save")
	toPost.Set("kind", "post")
	expectStatus(t, writer.postForm("/admin/articles/"+id, toPost), http.StatusForbidden)
	if env.mustGetArticle(id).Kind != article.KindPage {
		t.Fatal("writer changed the kind of a published page")
	}
	writerEdit := writer.get("/admin/articles/" + id + "/edit")
	if !regexp.MustCompile(`<select id="kind" name="kind" disabled`).MatchString(writerEdit.Body) {
		t.Error("kind selector is not disabled for a writer on a published page")
	}
	// ライターが画面を閉じて編集ロックを外してから、編集長が種類を変えて公開する。
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	toPost.Set("action", "publish")
	toPost.Del(CSRFFormField) // ライターのトークンを送らない
	expectRedirect(t, editor.postForm("/admin/articles/"+id, toPost), "/admin/articles/"+id+"/edit")
	if a := env.mustGetArticle(id); a.Kind != article.KindPost || a.Status != article.StatusPublished {
		t.Fatalf("after editor change = %s / %s, want post / published", a.Kind, a.Status)
	}
	c := env.newClient()
	expectMovedTo(t, c.get("/about"), "/articles/about")
	expectStatus(t, c.get("/articles/about"), http.StatusOK)

	// 不明な種類は保存しない。
	bad := pageForm("不明", "unknown", "draft")
	bad.Set("kind", "widget")
	res := writer.postForm("/admin/articles/new", bad)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "種類は「記事」か「ページ」を選んでください")
}

func TestPageTrashRedirectKeepsKind(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	loc := expectRedirect(t, writer.postForm("/admin/articles/new", pageForm("消すページ", "to-trash", "draft")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/delete", nil), "/admin/articles?status=deleted&kind=page")
}

func TestRedirectsKeepWorkingWithPages(t *testing.T) {
	env := newPagesEnv(t, Config{})
	// WordPress から取り込んだ固定ページは、旧パーマリンク（/research/）と /?page_id= から /articles/<slug> へ転送している。
	if _, err := env.db.Exec(`INSERT INTO redirects (old_path, new_path) VALUES
		('/research/', '/articles/research'),
		('/?page_id=2', '/articles/research'),
		('/old-post', '/articles/keion-live'),
		('/library-room', '/articles/keion-live')`); err != nil {
		t.Fatal(err)
	}
	env.server.redirects.reset()
	c := env.newClient()

	// ページ自身の URL では、転送より公開中のページを優先する（/research → /articles/research → /research の循環を防ぐ）。
	expectStatus(t, c.get("/research"), http.StatusOK)
	// 転送先がページなら、301 を重ねずにページへ直接転送する。
	expectMovedTo(t, c.get("/research/"), "/research")
	expectMovedTo(t, c.get("/?page_id=2"), "/research")
	// それ以外の転送は今までどおり。ページでないパスは登録した転送が優先する。
	expectMovedTo(t, c.get("/old-post"), "/articles/keion-live")
	expectMovedTo(t, c.get("/library-room"), "/articles/keion-live")
}

func TestThemePreviewUsesNewestPost(t *testing.T) {
	env := newPagesEnv(t, Config{})
	res := env.loggedIn(admin).get("/admin/settings/theme/preview?theme=tech&page=article")
	expectStatus(t, res, http.StatusOK)
	// 最も新しく公開したのはページ（研究紹介）だが、プレビューの「記事」は最新の記事を出す。
	expectContains(t, res.Body, `<article class="article">`, "図書館の新しい自習室")
	expectNotContainsAny(t, "preview", res.Body, `<article class="page">`)
}

func TestExportWritesPagesAndArticlesList(t *testing.T) {
	env := newPagesEnv(t, Config{})
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
	expectContains(t, read("research", "index.html"), `<article class="page">`, "量子計算を研究しています。")
	expectContains(t, read("メンバー", "index.html"), "教員と学生。")
	list := read("articles", "index.html")
	expectContains(t, list, `<h1 class="page-title">記事一覧</h1>`, "軽音楽部の新歓ライブ")
	expectNotContainsAny(t, "exported list", list, "研究紹介")
	expectContains(t, read("articles", "keion-live", "index.html"), `<article class="article">`)
	for _, p := range [][]string{{"articles", "research"}, {"draft-page"}} {
		if _, err := os.Stat(filepath.Join(append([]string{out}, p...)...)); !os.IsNotExist(err) {
			t.Errorf("%v exists (err %v), want not exported", p, err)
		}
	}
}

func TestSocialPostUsesPageURL(t *testing.T) {
	env := newPagesEnv(t, Config{})
	res := env.loggedIn(studentA).postForm("/admin/ai/social-post", url.Values{"article_id": {env.articleIDBySlug("research")}})
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"url":"https://media.example.ac.jp/research"`)
}

func TestAdminArticleReadOnlyViewPermissions(t *testing.T) {
	env := newTestEnv(t)
	id := seedArticle(t, env.db, article.Article{
		ID: "view-private-draft", Title: "確認用の下書き", Slug: "private-draft", ContentMarkdown: "本文を表示する",
		Status: article.StatusDraft, AuthorID: studentA.ID,
	})

	adminView := env.loggedIn(admin).get("/admin/articles/" + id)
	expectStatus(t, adminView, http.StatusOK)
	// 管理者も記事を編集できるため、見るだけの画面から編集画面へ移れ、一覧の行も編集画面へのリンクになる。
	expectContains(t, adminView.Body, "確認用の下書き", "本文を表示する", "private-draft", "山田", `href="/admin/articles/`+id+`/edit"`)
	// 執筆チーム内では非公開の記事を共有するため、他のライターも見るだけの画面を開ける。
	expectStatus(t, env.loggedIn(studentB).get("/admin/articles/"+id), http.StatusOK)
	expectStatus(t, env.loggedIn(studentA).get("/admin/articles/"+id), http.StatusOK)
	expectContains(t, env.loggedIn(admin).get("/admin/articles").Body, `href="/admin/articles/`+id+`/edit"`)
}

func TestAdminSettingsPagesIncludeSubnav(t *testing.T) {
	env := newTestEnv(t)
	adminClient := env.loggedIn(admin)
	for _, path := range []string{"/admin/settings/mcp", "/admin/users"} {
		res := adminClient.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `md:grid-cols-[15rem_minmax(0,1fr)]`, `href="/admin/settings/ai-integrations"`, `href="/admin/users"`)
	}
}
