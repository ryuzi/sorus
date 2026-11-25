package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// articlesLabels は site_menu の articles 項目の表示名から一覧・戻る・すべてへの文言を作り、
// 表示名が無ければ「記事一覧」・"Articles" にし、「一覧」を重ねない。
func TestArticlesLabels(t *testing.T) {
	menu := func(items ...MenuItem) SiteStructure { return SiteStructure{Menu: items} }
	beyondMax := SiteStructure{}
	for i := 0; i < maxMenuItems; i++ {
		beyondMax.Menu = append(beyondMax.Menu, MenuItem{Type: MenuItemURL, Label: "x", Target: "/x"})
	}
	beyondMax.Menu = append(beyondMax.Menu, MenuItem{Type: MenuItemArticles, Label: "圏外"})
	cases := []struct {
		name string
		st   SiteStructure
		lang string
		want articlesLabelSet
	}{
		{"custom ja", menu(MenuItem{Type: MenuItemArticles, Label: "ニュース", LabelEn: "News"}), "ja",
			articlesLabelSet{"ニュース", "ニュース一覧へ戻る", "ニュース一覧へ"}},
		{"custom en", menu(MenuItem{Type: MenuItemArticles, Label: "ニュース", LabelEn: "News"}), "en",
			articlesLabelSet{"News", "Back to News", "All News"}},
		{"ja label ending with 一覧", menu(MenuItem{Type: MenuItemArticles, Label: " 活動一覧 "}), "ja",
			articlesLabelSet{"活動一覧", "活動一覧へ戻る", "活動一覧へ"}},
		{"empty labels ja", menu(MenuItem{Type: MenuItemArticles}), "ja",
			articlesLabelSet{"記事一覧", "記事一覧へ戻る", "記事一覧へ"}},
		{"empty labels en", menu(MenuItem{Type: MenuItemArticles, Label: "ニュース"}), "en",
			articlesLabelSet{"Articles", "Back to Articles", "All Articles"}},
		{"no menu ja", SiteStructure{}, "ja", articlesLabelSet{"記事一覧", "記事一覧へ戻る", "記事一覧へ"}},
		{"no menu en", SiteStructure{}, "en", articlesLabelSet{"Articles", "Back to Articles", "All Articles"}},
		{"articles item in a group", menu(
			MenuItem{Type: MenuItemURL, Label: "ホーム", Target: "/"},
			MenuItem{Type: MenuItemGroup, Label: "活動", Children: []MenuItem{
				{Type: MenuItemArticles, Label: "お知らせ", LabelEn: "Notices"},
			}},
		), "ja", articlesLabelSet{"お知らせ", "お知らせ一覧へ戻る", "お知らせ一覧へ"}},
		// 改ざんされた孫項目（公開メニューに出ない）の表示名は使わない。
		{"grandchild articles item ignored", menu(
			MenuItem{Type: MenuItemGroup, Label: "G", Children: []MenuItem{
				{Type: MenuItemURL, Label: "子", Target: "/x", Children: []MenuItem{{Type: MenuItemArticles, Label: "深い"}}},
			}},
		), "ja", articlesLabelSet{"記事一覧", "記事一覧へ戻る", "記事一覧へ"}},
		// 公開メニューは maxMenuItems 件で切り詰めるので、それ以降の articles 項目は使わない。
		{"articles item beyond maxMenuItems ignored", beyondMax, "ja",
			articlesLabelSet{"記事一覧", "記事一覧へ戻る", "記事一覧へ"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := articlesLabels(tc.st, tc.lang); got != tc.want {
				t.Errorf("articlesLabels = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// newArticlesLabelEnv は日本語・英語の記事と、トップに出す日英のページを持つ環境を作る。
func newArticlesLabelEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, Config{BaseURL: testBaseURL + "/", SiteName: "テスト研究室"}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{
			Title: "学会発表", Slug: "talk", ContentMarkdown: "発表した。", Lang: "ja", TranslationGroupID: "grp-talk",
			Status: article.StatusPublished, PublishedAt: ago(2 * time.Hour),
		})
		seedArticle(t, db, article.Article{
			Title: "Conference talk", Slug: "talk-en", ContentMarkdown: "We gave a talk.", Lang: "en", TranslationGroupID: "grp-talk",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
		seedArticle(t, db, article.Article{
			Title: "ようこそ", Slug: "welcome", Kind: article.KindPage, ContentMarkdown: "研究室の紹介。", Lang: "ja",
			TranslationGroupID: "grp-front", Status: article.StatusPublished, PublishedAt: ago(5 * time.Hour),
		})
		seedArticle(t, db, article.Article{
			Title: "Welcome", Slug: "welcome-en", Kind: article.KindPage, ContentMarkdown: "About the lab.", Lang: "en",
			TranslationGroupID: "grp-front", Status: article.StatusPublished, PublishedAt: ago(4 * time.Hour),
		})
	})
}

// site_menu の articles 項目の表示名（「ニュース」・"News"）が、/articles のタイトル・見出し・パンくず、
// 記事ページの戻るリンク、トップの一覧へのリンクに通る。
func TestPublicArticlesLabelFromSiteMenu(t *testing.T) {
	env := newArticlesLabelEnv(t)
	if err := env.server.saveSiteStructure(context.Background(), SiteStructure{
		Menu:      []MenuItem{{Type: MenuItemArticles, Label: "ニュース", LabelEn: "News"}},
		FrontPage: "welcome",
	}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	res := c.get("/articles")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"<title>ニュース | テスト研究室</title>",
		`<meta name="description" content="ニュース | テスト研究室">`,
		`<h1 class="page-title">ニュース</h1>`,
		`<li aria-current="page">ニュース</li>`,
		`"name":"ニュース"`)

	res = c.get("/en/articles")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"<title>News | ",
		`<h1 class="page-title">News</h1>`,
		`<li aria-current="page">News</li>`,
		`"name":"News"`)

	res = c.get("/articles/talk")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<div class="article-back-nav"><a href="/articles">← ニュース一覧へ戻る</a></div>`)
	res = c.get("/en/articles/talk-en")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<div class="article-back-nav"><a href="/en/articles">← Back to News</a></div>`)

	res = c.get("/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<p class="front-news-more"><a href="/articles">ニュース一覧へ</a></p>`)
	res = c.get("/en/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<p class="front-news-more"><a href="/en/articles">All News</a></p>`)
}

// site_menu に articles 項目が無い・表示名が空なら「記事一覧」・"Articles" に戻り、「記事一覧一覧」とは出さない。
func TestPublicArticlesLabelFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		menu []MenuItem
	}{
		{"empty label", []MenuItem{{Type: MenuItemArticles}}},
		{"no articles item", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newArticlesLabelEnv(t)
			if err := env.server.saveSiteStructure(context.Background(), SiteStructure{Menu: tc.menu, FrontPage: "welcome"}); err != nil {
				t.Fatal(err)
			}
			c := env.newClient()
			checks := []struct {
				path string
				want []string
			}{
				{"/articles", []string{"<title>記事一覧 | テスト研究室</title>", `<h1 class="page-title">記事一覧</h1>`, `<li aria-current="page">記事一覧</li>`}},
				{"/en/articles", []string{"<title>Articles | ", `<h1 class="page-title">Articles</h1>`, `<li aria-current="page">Articles</li>`}},
				{"/articles/talk", []string{`← 記事一覧へ戻る</a></div>`}},
				{"/en/articles/talk-en", []string{`← Back to Articles</a></div>`}},
				{"/", []string{`<a href="/articles">記事一覧へ</a></p>`}},
				{"/en/", []string{`<a href="/en/articles">All Articles</a></p>`}},
			}
			for _, ch := range checks {
				res := c.get(ch.path)
				expectStatus(t, res, http.StatusOK)
				expectContains(t, res.Body, ch.want...)
				if strings.Contains(res.Body, "記事一覧一覧") {
					t.Errorf("%s: duplicated 記事一覧一覧", ch.path)
				}
			}
		})
	}
}
