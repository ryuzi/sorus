package server

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/rag"
)

const testBaseURL = "https://media.example.ac.jp"

// seedArticle は公開日時 publishedAt の記事を直接保存し、ID を返す。
func seedArticle(t *testing.T, db *sqlx.DB, a article.Article) string {
	t.Helper()
	if a.AuthorID == "" {
		a.AuthorID = studentA.ID
	}
	if a.CategoryID == 0 {
		a.CategoryID = 1
	}
	if err := article.NewService(db).Save(&a); err != nil {
		t.Fatalf("Save(%q): %v", a.Title, err)
	}
	return a.ID
}

func seedCategory(t *testing.T, db *sqlx.DB, name, slug, desc string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO categories (name, slug, description) VALUES (?, ?, ?)`, name, slug, desc)
	if err != nil {
		t.Fatalf("insert category: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func ago(d time.Duration) *time.Time {
	t := time.Now().UTC().Add(-d)
	return &t
}

// newPublicEnv は公開記事・下書き・予約投稿・カテゴリを用意した、BaseURL 設定済みのサーバーを起動する。
func newPublicEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, Config{BaseURL: testBaseURL + "/", SiteName: "テスト大学新聞", SiteDescription: "学生が運営するメディア", ShowAuthor: true}, func(db *sqlx.DB) {
		// ensureDefaultCategory より前に入れるため、未分類（id=1）もここで作る。
		seedCategory(t, db, "未分類", "uncategorized", "")
		circle := seedCategory(t, db, "サークル", "circle", "学内サークルの活動を紹介します")
		seedCategory(t, db, "空のカテゴリ", "empty", "")

		seedArticle(t, db, article.Article{
			Title: "軽音楽部の新歓ライブ", Slug: "keion-live", CategoryID: circle,
			Excerpt:         "春の新歓ライブをレポート",
			ContentMarkdown: "## ライブの様子\n\n満員の観客がスタンディングで盛り上がった。\n\n### セットリスト\n\n全10曲。\n\n## 次回予定\n\n秋の学園祭に出演。",
			Status:          article.StatusPublished, PublishedAt: ago(2 * time.Hour),
		})
		seedArticle(t, db, article.Article{
			Title: "図書館の新しい自習室", Slug: "library-room",
			ContentMarkdown: "図書館3階に24時間利用できる自習室がオープンした。<script>alert(1)</script>",
			Status:          article.StatusPublished, PublishedAt: ago(time.Hour),
		})
		seedArticle(t, db, article.Article{
			Title: "日本語スラッグの記事", Slug: "学園祭レポート",
			ContentMarkdown: "模擬店が並んだ。", Status: article.StatusPublished, PublishedAt: ago(3 * time.Hour),
		})
		seedArticle(t, db, article.Article{Title: "未公開の下書き記事", Slug: "secret-draft", ContentMarkdown: "下書き本文"})
		seedArticle(t, db, article.Article{
			Title: "来週公開の予約記事", Slug: "scheduled", ContentMarkdown: "予約本文",
			Status: article.StatusPublished, PublishedAt: ago(-7 * 24 * time.Hour),
		})
		deleted := seedArticle(t, db, article.Article{
			Title: "ゴミ箱の記事", Slug: "trashed", ContentMarkdown: "削除済み",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
		if err := article.NewService(db).SoftDelete(deleted); err != nil {
			t.Fatalf("SoftDelete: %v", err)
		}
	})
}

var jsonLDPattern = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)

// jsonLD は body の JSON-LD を @type ごとに返す。
func jsonLD(t *testing.T, body string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, m := range jsonLDPattern.FindAllStringSubmatch(body, -1) {
		var v map[string]any
		if err := json.Unmarshal([]byte(m[1]), &v); err != nil {
			t.Fatalf("invalid JSON-LD %q: %v", m[1], err)
		}
		typ, _ := v["@type"].(string)
		out[typ] = v
	}
	return out
}

// metaContent は <meta property|name="key" content="..."> の content を返す。
func metaContent(body, key string) string {
	re := regexp.MustCompile(`<meta (?:property|name)="` + regexp.QuoteMeta(key) + `" content="([^"]*)">`)
	if m := re.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func TestPublicIndex(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()

	res := c.get("/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"<title>テスト大学新聞</title>",
		`<meta name="description" content="学生が運営するメディア">`,
		`<link rel="canonical" href="https://media.example.ac.jp/">`,
		`href="/feed.xml"`,
		"軽音楽部の新歓ライブ", "春の新歓ライブをレポート", "図書館の新しい自習室", "日本語スラッグの記事",
		`href="/articles/keion-live"`,
		// ナビゲーションには公開記事のあるカテゴリだけを出す。
		`href="/categories/circle"`,
	)
	for _, hidden := range []string{"未公開の下書き記事", "来週公開の予約記事", "ゴミ箱の記事", "空のカテゴリ"} {
		if strings.Contains(res.Body, hidden) {
			t.Errorf("top page shows %q", hidden)
		}
	}
	// 公開日時の新しい順。
	if i, j := strings.Index(res.Body, "図書館の新しい自習室"), strings.Index(res.Body, "軽音楽部の新歓ライブ"); i < 0 || j < 0 || i > j {
		t.Errorf("articles are not ordered by published_at desc")
	}
	ld := jsonLD(t, res.Body)
	if ld["WebSite"]["url"] != testBaseURL+"/" {
		t.Errorf("WebSite JSON-LD = %v", ld["WebSite"])
	}

	// ?category= はカテゴリページへ恒久的に転送する。
	res = c.get("/?category=circle")
	expectStatus(t, res, http.StatusMovedPermanently)
	if loc := res.Header.Get("Location"); loc != "/categories/circle" {
		t.Errorf("Location = %q", loc)
	}
	// 範囲外・不正なページ番号は 404。
	expectStatus(t, c.get("/?page=2"), http.StatusNotFound)
	expectStatus(t, c.get("/?page=abc"), http.StatusNotFound)
}

func TestPublicIndexPagination(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		for i := 1; i <= publicPerPage+3; i++ {
			seedArticle(t, db, article.Article{
				Title: fmt.Sprintf("記事%02d", i), Slug: fmt.Sprintf("post-%02d", i), ContentMarkdown: "本文",
				Status: article.StatusPublished, PublishedAt: ago(time.Duration(100-i) * time.Minute),
			})
		}
	})
	c := env.newClient()

	res := c.get("/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "記事15", "1 / 2", `href="/page/2"`, `rel="next"`)
	if strings.Contains(res.Body, ">記事03<") {
		t.Error("page 1 contains an article of page 2")
	}
	// 2ページ目はパスの形（/page/2）。以前の /?page=2 も同じ内容を返す。
	for _, p := range []string{"/page/2", "/?page=2"} {
		res = c.get(p)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "記事03", "記事01", "2 / 2", `href="/"`, `rel="prev"`,
			`<link rel="canonical" href="https://media.example.ac.jp/page/2">`)
		if strings.Contains(res.Body, ">記事15<") {
			t.Errorf("%s contains an article of page 1", p)
		}
	}
}

func TestPublicArticlesYearArchive(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		for i, year := range []int{2026, 2007, 2007, 2025} {
			published := time.Date(year, time.January, i+1, 12, 0, 0, 0, time.UTC)
			seedArticle(t, db, article.Article{Title: fmt.Sprintf("年別記事%d", i+1), Slug: fmt.Sprintf("year-post-%d", i+1),
				ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: &published})
		}
		for i := 0; i < publicPerPage; i++ {
			published := time.Date(2007, time.February, i+1, 12, 0, 0, 0, time.UTC)
			seedArticle(t, db, article.Article{Title: fmt.Sprintf("古い記事%d", i+1), Slug: fmt.Sprintf("old-year-post-%d", i+1),
				ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: &published})
		}
	})
	c := env.newClient()

	res := c.get("/articles")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "年別アーカイブ", `href="/articles?year=2026"`, `href="/articles?year=2025"`, `href="/articles?year=2007"`, "(14)")

	res = c.get("/articles?year=2007")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "古い記事12", `href="/articles?year=2007"`, `aria-current="page"`, `href="/articles?year=2007&amp;page=2"`)
	if strings.Contains(res.Body, "年別記事1") || strings.Contains(res.Body, "年別記事4") {
		t.Fatal("year=2007 returned articles outside the selected year")
	}
	res = c.get("/articles?year=2007&page=2")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "年別記事2", "年別記事3", `href="/articles?year=2007"`)
	for _, invalid := range []string{"07", "20ab", "10000"} {
		expectStatus(t, c.get("/articles?year="+invalid), http.StatusNotFound)
	}
}

func TestPublicArticleCardsRenderPlaceholderMedia(t *testing.T) {
	env := newPublicEnv(t)
	res := env.newClient().get("/articles")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `class="card-media is-placeholder" tabindex="-1" aria-hidden="true"`)
}

func TestPublicFrontCardsRenderPlaceholderMedia(t *testing.T) {
	env := newFrontEnv(t)
	// トップのページカードの画像は既定で出さないため、表示する設定にして確かめる。
	env.setCardImageSetting(settingShowFrontCardImages, "true")
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontPage: "research", FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: []string{"access"}}}}); err != nil {
		t.Fatal(err)
	}
	res := env.newClient().get("/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `class="front-card-media is-placeholder" tabindex="-1" aria-hidden="true"`)
}

func TestPublicArticle(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()

	res := c.get("/articles/keion-live")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"<title>軽音楽部の新歓ライブ | テスト大学新聞</title>",
		`<link rel="canonical" href="https://media.example.ac.jp/articles/keion-live">`,
		"満員の観客がスタンディングで盛り上がった。",
		"山田", // 著者
		`<time datetime="`,
		// 目次と本文の見出し ID が一致する。
		`aria-label="目次"`, `id="ライブの様子"`, `id="セットリスト"`, `class="toc-level-3"`,
		// パンくず
		`href="/categories/circle"`,
	)
	// html/template は href の日本語をパーセントエンコードする（ブラウザはデコードして id と照合する）。
	for _, id := range []string{"ライブの様子", "セットリスト", "次回予定"} {
		if href := `href="#` + url.PathEscape(id) + `"`; !strings.Contains(strings.ToLower(res.Body), strings.ToLower(href)) {
			t.Errorf("TOC link %s is missing", href)
		}
	}
	if got := metaContent(res.Body, "og:type"); got != "article" {
		t.Errorf("og:type = %q", got)
	}
	if got := metaContent(res.Body, "og:url"); got != testBaseURL+"/articles/keion-live" {
		t.Errorf("og:url = %q", got)
	}
	if got := metaContent(res.Body, "og:description"); got != "春の新歓ライブをレポート" {
		t.Errorf("og:description = %q", got)
	}
	if got := metaContent(res.Body, "og:site_name"); got != "テスト大学新聞" {
		t.Errorf("og:site_name = %q", got)
	}
	if got := metaContent(res.Body, "twitter:card"); got == "" {
		t.Error("twitter:card is missing")
	}

	ld := jsonLD(t, res.Body)
	news := ld["NewsArticle"]
	if news == nil || news["headline"] != "軽音楽部の新歓ライブ" || news["datePublished"] == "" || news["dateModified"] == "" {
		t.Fatalf("NewsArticle JSON-LD = %v", news)
	}
	if authors, _ := news["author"].([]any); len(authors) != 1 || authors[0].(map[string]any)["name"] != "山田" {
		t.Errorf("NewsArticle author = %v", news["author"])
	}

	// 執筆者名非表示（show_author=false）の検証
	env.server.setShowAuthor(false)
	resNoAuthor := c.get("/articles/keion-live")
	expectStatus(t, resNoAuthor, http.StatusOK)
	if strings.Contains(resNoAuthor.Body, "・文: <span class=\"author\">山田</span>") {
		t.Errorf("author name must not appear when show_author=false: %s", resNoAuthor.Body)
	}
	ldNoAuthor := jsonLD(t, resNoAuthor.Body)
	newsNoAuthor := ldNoAuthor["NewsArticle"]
	authorObj, _ := newsNoAuthor["author"].(map[string]any)
	if authorObj == nil || authorObj["@type"] != "Organization" || authorObj["name"] != "テスト大学新聞" {
		t.Errorf("author should fallback to publisher Organization when show_author=false, got: %v", newsNoAuthor["author"])
	}
	env.server.setShowAuthor(true) // 元に戻す
	crumbs, _ := ld["BreadcrumbList"]["itemListElement"].([]any)
	if len(crumbs) != 3 || crumbs[1].(map[string]any)["name"] != "サークル" ||
		crumbs[2].(map[string]any)["item"] != testBaseURL+"/articles/keion-live" {
		t.Errorf("BreadcrumbList = %v", ld["BreadcrumbList"])
	}

	// 抜粋の無い記事は本文から description を作り、本文はサニタイズ済み。
	res = c.get("/articles/library-room")
	expectStatus(t, res, http.StatusOK)
	if got := metaContent(res.Body, "description"); !strings.HasPrefix(got, "図書館3階に24時間利用できる自習室") {
		t.Errorf("description = %q", got)
	}
	if strings.Contains(res.Body, "<script>alert(1)") {
		t.Error("article body contains unsanitized script")
	}

	// 日本語スラッグ（パーセントエンコード）でも開ける。
	res = c.get("/articles/" + url.PathEscape("学園祭レポート"))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "模擬店が並んだ。")

	// 下書き・予約投稿・ゴミ箱・存在しない記事は読者向けの 404 ページ。
	for _, slug := range []string{"secret-draft", "scheduled", "trashed", "nope"} {
		res := c.get("/articles/" + slug)
		expectStatus(t, res, http.StatusNotFound)
		expectContains(t, res.Body, "ページが見つかりません", `<meta name="robots" content="noindex">`)
	}
}

func TestPublicArticleFeaturedImage(t *testing.T) {
	env := newPublicEnv(t)
	editor := env.loggedIn(chief) // 公開できるのは編集者・管理者。
	res := editor.upload("campus.png", pngBytes(t, 1000, 1000), "キャンパス")
	expectStatus(t, res, http.StatusOK)
	var media struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(res.Body), &media); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE articles SET featured_image_url = ? WHERE slug = 'keion-live'`, media.URL); err != nil {
		t.Fatal(err)
	}

	c := env.newClient()
	res = c.get("/articles/keion-live")
	expectStatus(t, res, http.StatusOK)
	if got := metaContent(res.Body, "og:image"); got != testBaseURL+media.URL {
		t.Errorf("og:image = %q, want %q", got, testBaseURL+media.URL)
	}
	if got := metaContent(res.Body, "twitter:card"); got != "summary_large_image" {
		t.Errorf("twitter:card = %q", got)
	}
	// LCP となるアイキャッチは srcset 付きで、遅延読み込みしない。
	hero := regexp.MustCompile(`<figure class="hero"><img [^>]*>`).FindString(res.Body)
	if !strings.Contains(hero, "srcset=") || !strings.Contains(hero, "400w") || strings.Contains(hero, `loading="lazy"`) ||
		!strings.Contains(hero, `fetchpriority="high"`) {
		t.Errorf("hero image = %q", hero)
	}
	if !strings.Contains(hero, "is-contain") {
		t.Errorf("square diagram image is missing the contain class: %q", hero)
	}
	if news := jsonLD(t, res.Body)["NewsArticle"]; fmt.Sprint(news["image"]) != "["+testBaseURL+media.URL+"]" {
		t.Errorf("NewsArticle image = %v", news["image"])
	}

	// 一覧のカードは遅延読み込み。
	res = c.get("/")
	card := regexp.MustCompile(`<a href="/articles/keion-live" class="card-media"[^>]*><img [^>]*>`).FindString(res.Body)
	if !strings.Contains(card, `loading="lazy"`) || !strings.Contains(card, "srcset=") {
		t.Errorf("card image = %q", card)
	}
}

// 本文に写真が1枚でもあるときは、タイトル直下の .hero を出さず本文の配置に任せる（同じ写真が別の URL で上下に
// 2回出たり、一覧用の小さなサムネイルが引き伸ばされたりしないようにする）。本文に画像の無い記事だけ .hero に出す。
func TestPublicArticleHeroSkipsEyecatchInContent(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()
	set := func(slug, featured, md string) {
		t.Helper()
		if _, err := env.db.Exec(`UPDATE articles SET featured_image_url = ?, content_markdown = ?, content_html = '' WHERE slug = ?`,
			featured, md, slug); err != nil {
			t.Fatal(err)
		}
	}
	hero := regexp.MustCompile(`<figure class="hero">`)
	cases := []struct {
		name, featured, md string
		wantHero           bool
	}{
		{"same URL in body", "/uploads/live-photo.jpg", "![ライブ](/uploads/live-photo.jpg)\n\n満員だった。", false},
		{"resized variant in body", "/uploads/abc123def-original.webp", "![ライブ](/uploads/abc123def-800.webp)\n\n満員だった。", false},
		{"WordPress thumbnail in body", "/uploads/2017/05/stage.jpg", "![ライブ](/uploads/2017/05/stage-1024x768.jpg)\n\n満員だった。", false},
		{"different photo in body", "/uploads/live-photo.jpg", "![会場](/uploads/hall.jpg)\n\n満員だった。", false},
		{"similar name but another photo", "/uploads/photo-1.jpg", "![会場](/uploads/photo-2.jpg)\n\n満員だった。", false},
		{"raw uppercase IMG in body", "/uploads/live-photo.jpg", "<P><IMG SRC=\"/uploads/hash-abc.jpg\" ALT=\"会場\"></P>\n\n満員だった。", false},
		{"image link only in body", "/uploads/live-photo.jpg", "[写真を見る](/uploads/hall.jpg)\n\n満員だった。", true},
		{"no image in body", "/uploads/live-photo.jpg", "満員だった。", true},
		{"no eyecatch", "", "![ライブ](/uploads/live-photo.jpg)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set("keion-live", tc.featured, tc.md)
			res := c.get("/articles/keion-live")
			expectStatus(t, res, http.StatusOK)
			if got := len(hero.FindAllString(res.Body, -1)); got != map[bool]int{false: 0, true: 1}[tc.wantHero] {
				t.Errorf("hero count = %d, want hero %v", got, tc.wantHero)
			}
			if tc.featured != "" {
				// og:image はアイキャッチのまま。
				if got := metaContent(res.Body, "og:image"); got != testBaseURL+tc.featured {
					t.Errorf("og:image = %q, want %q", got, testBaseURL+tc.featured)
				}
			}
		})
	}
}

// 目次が出る（見出しが2つ以上の）記事でも、アイキャッチは本文の列の先頭に置き、目次の列まで全幅に広げない。
// 本文の後ろ（添付ファイル・関連記事の前）には記事一覧へ戻る導線を出す。
func TestPublicArticleHeroInProseColumnAndBackNav(t *testing.T) {
	env := newPublicEnv(t)
	if _, err := env.db.Exec(`UPDATE articles SET featured_image_url = '/uploads/live-photo.jpg',
		content_markdown = ?, content_html = '' WHERE slug = 'keion-live'`, "## 当日\n\n満員だった。\n\n## 次回\n\n来月も開く。"); err != nil {
		t.Fatal(err)
	}
	res := env.newClient().get("/articles/keion-live")
	expectStatus(t, res, http.StatusOK)
	layout := strings.Index(res.Body, `<div class="article-layout">`)
	hero := strings.Index(res.Body, `<div class="prose" data-math-root><figure class="hero"><img `)
	if layout < 0 || hero < layout {
		t.Fatalf("hero is not at the top of the prose column inside .article-layout (layout %d, hero %d)", layout, hero)
	}
	if strings.Count(res.Body, `<figure class="hero">`) != 1 {
		t.Errorf("hero count = %d, want 1", strings.Count(res.Body, `<figure class="hero">`))
	}
	back := strings.Index(res.Body, `<div class="article-back-nav"><a href="/articles">← 記事一覧へ戻る</a></div>`)
	if back < hero || back > strings.Index(res.Body, `</article>`) {
		t.Errorf("back navigation is missing or misplaced (hero %d, back %d)", hero, back)
	}

	// 英語の記事は英語の一覧へ戻す。
	seedArticle(t, env.db, article.Article{
		Title: "Live concert", Slug: "live-concert-en", Lang: "en", TranslationGroupID: "group-live-concert",
		ContentMarkdown: "It was packed.", Status: article.StatusPublished, PublishedAt: ago(time.Hour),
	})
	res = env.newClient().get("/en/articles/live-concert-en")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<div class="article-back-nav"><a href="/en/articles">← Back to Articles</a></div>`)
}

// 記事末尾の関連記事は、候補が多くてもデスクトップの3列×1行に揃う3件（rag.DefaultRelatedLimit）に絞る。
func TestPublicRelatedArticlesShowThree(t *testing.T) {
	ids := map[string]string{}
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		cat := seedCategory(t, db, "研究", "research", "")
		for i, slug := range []string{"target", "r1", "r2", "r3", "r4", "r5"} {
			ids[slug] = seedArticle(t, db, article.Article{Title: "記事" + slug, Slug: "three-" + slug, CategoryID: cat,
				ContentMarkdown: "本文です。", Status: article.StatusPublished, PublishedAt: ago(time.Duration(i+1) * 30 * 24 * time.Hour)})
		}
	})
	env.server.ragService = rag.NewService(env.db, nil)
	setEmbedding(t, env.db, ids["target"], []float32{1, 0})
	for i, slug := range []string{"r1", "r2", "r3", "r4", "r5"} {
		setEmbedding(t, env.db, ids[slug], []float32{1, float32(i+1) * 0.1})
	}
	res := env.newClient().get("/articles/three-target")
	expectStatus(t, res, http.StatusOK)
	if got := relatedTitles(t, res.Body); fmt.Sprint(got) != "[記事r1 記事r2 記事r3]" {
		t.Errorf("related = %q, want the top 3", got)
	}
}

// 固定ページはアイキャッチ（カード用のサムネイル）をタイトル上の .hero に出さない。図は本文の中で見切れずに見せる。
func TestPublicPageOmitsHero(t *testing.T) {
	env := newPublicEnv(t)
	seedArticle(t, env.db, article.Article{
		Title: "研究紹介", Slug: "research", Kind: article.KindPage,
		ContentMarkdown: "研究の概要。", Status: article.StatusPublished, PublishedAt: ago(time.Hour),
	})
	if _, err := env.db.Exec(`UPDATE articles SET featured_image_url = '/uploads/research-banner.png' WHERE slug = 'research'`); err != nil {
		t.Fatal(err)
	}
	res := env.newClient().get("/research")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<h1 class="article-title">研究紹介</h1>`, "研究の概要。")
	if strings.Contains(res.Body, `<figure class="hero">`) || strings.Contains(res.Body, "research-banner.png\" alt") {
		t.Errorf("page renders the eyecatch as a hero image")
	}
	if got := metaContent(res.Body, "og:image"); got != testBaseURL+"/uploads/research-banner.png" {
		t.Errorf("og:image = %q, want the eyecatch", got)
	}
}

func TestContentHasImage(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    bool
	}{
		{"", false},
		{"<p>text</p>", false},
		{`<p><img src="/a.jpg" alt=""></p>`, true},
		{`<P><IMG SRC="/a.jpg"></P>`, true},
		{"<img/>", true},
		{`<p><imgx></imgx></p>`, false},
		{`<a href="/uploads/a.jpg">写真</a>`, false},
	} {
		if got := contentHasImage(tc.content); got != tc.want {
			t.Errorf("contentHasImage(%q) = %v, want %v", tc.content, got, tc.want)
		}
	}
	if !contentHasImage("<p>none</p>", `<img src="/a.jpg">`) {
		t.Error("contentHasImage does not check the second content")
	}
}

func TestEyecatchInContent(t *testing.T) {
	for _, tc := range []struct {
		src, content string
		want         bool
	}{
		{"", `<img src="/uploads/a.jpg">`, false},
		{"/uploads/a.jpg", `<p>text</p>`, false},
		{"/uploads/a.jpg", `<img src="/uploads/a.jpg" alt="">`, true},
		{"/uploads/x9-original.webp", `<img src="/uploads/x9-1200.webp" srcset="/uploads/x9-400.webp 400w, /uploads/x9-800.webp 800w">`, true},
		{"/uploads/x9-1200.webp", `<img srcset="/uploads/y-400.webp 400w, /uploads/x9-800.webp 800w">`, true},
		{"https://example.com/wp-content/uploads/2016/04/Lab%20Photo-scaled.jpg", `<img src='/uploads/2016/04/lab photo-300x200.jpg'>`, true},
		{"/uploads/a.jpg?v=2", `<img src="/uploads/a.png">`, true},
		{"/uploads/photo-1.jpg", `<img src="/uploads/photo-2.jpg">`, false},
		{"/uploads/a.jpg", `<a href="/uploads/a-large.jpg">リンク</a>`, false},
		{"/uploads/2024-05-01.jpg", `<img src="/uploads/2024-05-02.jpg">`, false},
	} {
		if got := eyecatchInContent(tc.src, tc.content); got != tc.want {
			t.Errorf("eyecatchInContent(%q, %q) = %v, want %v", tc.src, tc.content, got, tc.want)
		}
	}
	if !eyecatchInContent("/uploads/a.jpg", "", `<img src="/uploads/a.jpg">`) {
		t.Error("eyecatchInContent does not check the second content")
	}
}

func TestPublicCategory(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()

	res := c.get("/categories/circle")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"<title>サークル | テスト大学新聞</title>",
		"学内サークルの活動を紹介します",
		"軽音楽部の新歓ライブ",
		`<link rel="canonical" href="https://media.example.ac.jp/categories/circle">`,
	)
	if strings.Contains(res.Body, "図書館の新しい自習室") {
		t.Error("category page shows an article of another category")
	}
	if ld := jsonLD(t, res.Body); ld["BreadcrumbList"] == nil {
		t.Error("BreadcrumbList JSON-LD is missing")
	}

	res = c.get("/categories/empty")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "このカテゴリにはまだ記事がありません。")

	expectStatus(t, c.get("/categories/nope"), http.StatusNotFound)
}

func TestPublicSearch(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()

	res := c.get("/search?q=" + url.QueryEscape("自習室"))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`<meta name="robots" content="noindex">`,
		`id="search-results"`, `value="自習室"`,
		"「自習室」の検索結果: 1件",
		"<mark>自習室</mark>",
		`href="/articles/library-room"`,
	)

	// HTMX からの要求には結果のパーシャルだけを返す。
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/search?q="+url.QueryEscape("ライブ"), nil)
	req.Header.Set("HX-Request", "true")
	res = c.do(req)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "「ライブ」の検索結果: 1件", `href="/articles/keion-live"`, "<mark>ライブ</mark>")
	if strings.Contains(res.Body, "<html") || strings.Contains(res.Body, "<header") {
		t.Errorf("HTMX response is not a partial: %.500s", res.Body)
	}
	if vary := strings.Join(res.Header.Values("Vary"), ","); !strings.Contains(vary, "HX-Request") {
		t.Errorf("Vary = %q, want HX-Request", vary)
	}

	// 下書き・予約投稿・ゴミ箱の記事は検索に出ない。
	for _, q := range []string{"下書き本文", "予約本文", "削除済み"} {
		res := c.get("/search?q=" + url.QueryEscape(q))
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "の検索結果: 0件", "一致する記事は見つかりませんでした")
	}

	// 検索語のエスケープ（XSS）。
	res = c.get("/search?q=" + url.QueryEscape(`<script>alert(1)</script>`))
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "<script>alert(1)") {
		t.Error("search page reflects unescaped query")
	}

	res = c.get("/search")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "キーワードを入力して記事を検索できます。")
}

// 管理画面での公開・下書き戻し・削除が、検索インデックスへ即時に反映される。
func TestPublishingUpdatesSearchIndex(t *testing.T) {
	env := newPublicEnv(t)
	editor := env.loggedIn(chief) // 公開できるのは編集者・管理者。
	reader := env.newClient()
	searchCount := func(q string) string {
		t.Helper()
		res := reader.get("/search?q=" + url.QueryEscape(q))
		expectStatus(t, res, http.StatusOK)
		m := regexp.MustCompile(`の検索結果: (\d+)件`).FindStringSubmatch(res.Body)
		if m == nil {
			t.Fatalf("result count not found: %.1000s", res.Body)
		}
		return m[1]
	}

	id := editor.createArticle("吹奏楽部の定期演奏会", "ホールで定期演奏会が開かれた。")
	if n := searchCount("定期演奏会"); n != "0" {
		t.Fatalf("draft is searchable: %s hits", n)
	}

	form := url.Values{"title": {"吹奏楽部の定期演奏会"}, "slug": {"brass-concert"}, "category_id": {"1"},
		"content_markdown": {"ホールで定期演奏会が開かれた。"}, "action": {"publish"}}
	expectRedirect(t, editor.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if n := searchCount("定期演奏会"); n != "1" {
		t.Fatalf("published article hits = %s, want 1", n)
	}
	expectStatus(t, reader.get("/articles/brass-concert"), http.StatusOK)

	form.Set("action", "draft")
	expectRedirect(t, editor.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if n := searchCount("定期演奏会"); n != "0" {
		t.Fatalf("article reverted to draft is still searchable: %s hits", n)
	}

	form.Set("action", "publish")
	expectRedirect(t, editor.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	expectRedirect(t, editor.postForm("/admin/articles/"+id+"/delete", nil), "/admin/articles?status=deleted")
	if n := searchCount("定期演奏会"); n != "0" {
		t.Fatalf("deleted article is still searchable: %s hits", n)
	}
	expectStatus(t, reader.get("/articles/brass-concert"), http.StatusNotFound)
}

func TestFeed(t *testing.T) {
	env := newPublicEnv(t)
	res := env.newClient().get("/feed.xml")
	expectStatus(t, res, http.StatusOK)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Errorf("Content-Type = %q", ct)
	}
	expectContains(t, res.Body, `xmlns:atom="http://www.w3.org/2005/Atom"`,
		`<atom:link href="`+testBaseURL+`/feed.xml" rel="self" type="application/rss+xml">`)
	var feed struct {
		Version string `xml:"version,attr"`
		Channel struct {
			Title string `xml:"title"`
			// atom:link（self）も同じローカル名のため、両方を受けて RSS の link を探す。
			Links []string `xml:"link"`
			Items []struct {
				Title    string `xml:"title"`
				Link     string `xml:"link"`
				GUID     string `xml:"guid"`
				PubDate  string `xml:"pubDate"`
				Creator  string `xml:"http://purl.org/dc/elements/1.1/ creator"`
				Content  string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
				Category string `xml:"category"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal([]byte(res.Body), &feed); err != nil {
		t.Fatalf("invalid RSS: %v\n%s", err, res.Body)
	}
	if feed.Version != "2.0" || feed.Channel.Title != "テスト大学新聞" || !slices.Contains(feed.Channel.Links, testBaseURL+"/") {
		t.Errorf("channel = %+v", feed.Channel)
	}
	var titles []string
	for _, it := range feed.Channel.Items {
		titles = append(titles, it.Title)
	}
	if want := []string{"図書館の新しい自習室", "軽音楽部の新歓ライブ", "日本語スラッグの記事"}; strings.Join(titles, ",") != strings.Join(want, ",") {
		t.Fatalf("items = %v, want %v", titles, want)
	}
	it := feed.Channel.Items[1]
	if it.Link != testBaseURL+"/articles/keion-live" || it.GUID != it.Link || it.Creator != "山田" || it.Category != "サークル" ||
		!strings.Contains(it.Content, "<h2") {
		t.Errorf("item = %+v", it)
	}
	if _, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil {
		t.Errorf("pubDate %q: %v", it.PubDate, err)
	}
	if want := testBaseURL + "/articles/" + url.PathEscape("学園祭レポート"); feed.Channel.Items[2].Link != want {
		t.Errorf("Japanese slug link = %q, want %q", feed.Channel.Items[2].Link, want)
	}
}

func TestSitemapAndRobots(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()

	res := c.get("/sitemap.xml")
	expectStatus(t, res, http.StatusOK)
	var set struct {
		URLs []struct {
			Loc     string `xml:"loc"`
			LastMod string `xml:"lastmod"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal([]byte(res.Body), &set); err != nil {
		t.Fatalf("invalid sitemap: %v", err)
	}
	locs := map[string]string{}
	for _, u := range set.URLs {
		locs[u.Loc] = u.LastMod
	}
	for _, want := range []string{"/", "/categories/circle", "/articles/keion-live", "/articles/library-room"} {
		if _, ok := locs[testBaseURL+want]; !ok {
			t.Errorf("sitemap is missing %s: %v", want, locs)
		}
	}
	if lm := locs[testBaseURL+"/articles/keion-live"]; lm == "" {
		t.Error("article has no lastmod")
	} else if _, err := time.Parse(time.RFC3339, lm); err != nil {
		t.Errorf("lastmod %q: %v", lm, err)
	}
	for _, hidden := range []string{"/articles/secret-draft", "/articles/scheduled", "/articles/trashed", "/categories/empty"} {
		if _, ok := locs[testBaseURL+hidden]; ok {
			t.Errorf("sitemap contains %s", hidden)
		}
	}

	res = c.get("/robots.txt")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "User-agent: *", "Disallow: /admin/", "Sitemap: "+testBaseURL+"/sitemap.xml")
}

func TestRedirectMiddleware(t *testing.T) {
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		for _, r := range []struct {
			from, to string
			code     int
		}{
			{"/2023/04/old-post/", "/articles/keion-live", 301},
			{"/archives/" + url.PathEscape("新歓") + "/", "/categories/circle", 302},
			{"/?p=123", "/articles/library-room", 301},
			{"/loop", "/loop/", 301},
			{"/?p=self", "/?p=self", 301},
			{"/admin/old", "/admin/articles", 301},
		} {
			if _, err := db.Exec(`INSERT INTO redirects (old_path, new_path, status_code) VALUES (?, ?, ?)`, r.from, r.to, r.code); err != nil {
				t.Fatal(err)
			}
		}
	})
	c := env.newClient()

	cases := []struct {
		path, wantLoc string
		wantCode      int
	}{
		// 末尾の / の有無は問わず、パスだけの一致では元のクエリを引き継ぐ。
		{"/2023/04/old-post/", "/articles/keion-live", 301},
		{"/2023/04/old-post", "/articles/keion-live", 301},
		{"/2023/04/old-post/?utm_source=x", "/articles/keion-live?utm_source=x", 301},
		// パーセントエンコードした日本語パスでも一致する。
		{"/archives/" + url.PathEscape("新歓"), "/categories/circle", 302},
		// クエリ付きで登録された旧 URL。
		{"/?p=123", "/articles/library-room", 301},
	}
	for _, tc := range cases {
		res := c.get(tc.path)
		if res.StatusCode != tc.wantCode || res.Header.Get("Location") != tc.wantLoc {
			t.Errorf("GET %s = %d %q, want %d %q", tc.path, res.StatusCode, res.Header.Get("Location"), tc.wantCode, tc.wantLoc)
		}
	}

	// 自分自身への転送（クエリ付きを含む）・管理画面のパス・POST は転送しない。
	expectStatus(t, c.get("/loop"), http.StatusNotFound)
	expectStatus(t, c.get("/?p=self"), http.StatusOK)
	if res := c.get("/admin/old"); res.Header.Get("Location") == "/admin/articles" {
		t.Error("admin path was redirected by the redirects table")
	}
	expectStatus(t, c.get("/?p=999"), http.StatusOK)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/2023/04/old-post/", nil)
	if res := c.do(req); res.StatusCode == http.StatusMovedPermanently {
		t.Error("POST was redirected")
	}
}
