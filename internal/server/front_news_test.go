package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

func TestShowFrontNews(t *testing.T) {
	news := []publicArticle{{Title: "お知らせ"}}
	for _, c := range []struct {
		name string
		news []publicArticle
		menu []MenuItem
		want bool
	}{
		{"新着記事もメニューも無い", nil, nil, false},
		{"固定ページだけのメニュー", nil, []MenuItem{{Type: MenuItemPage, Target: "guide"}, {Label: "外部", Type: MenuItemURL, Target: "https://example.com"}}, false},
		{"メニューに記事の一覧", nil, []MenuItem{{Type: MenuItemPage, Target: "guide"}, {Type: MenuItemArticles}}, true},
		{"見出しの子項目に記事の一覧", nil, []MenuItem{{Label: "読む", Type: MenuItemGroup, Children: []MenuItem{{Type: MenuItemArticles}}}}, true},
		{"新着記事がある", news, nil, true},
		{"新着記事があり、メニューに記事の一覧は無い", news, []MenuItem{{Type: MenuItemPage, Target: "guide"}}, true},
	} {
		if got := showFrontNews(c.news, c.menu); got != c.want {
			t.Errorf("%s: showFrontNews = %v, want %v", c.name, got, c.want)
		}
	}
}

// トップに固定ページを出すドキュメントサイト（記事が無い）では、メニューに記事の一覧が無ければ空の「新着記事」の欄を出さない。
// メニューに記事の一覧を足したとき・記事を公開したときは、今までどおり出す。
func TestFrontNewsHiddenOnSitesWithoutArticles(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL, SiteName: "ドキュメント"}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedPage(t, db, article.Article{Title: "はじめに", Slug: "intro", ContentMarkdown: "製品の使い方。",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
		seedPage(t, db, article.Article{Title: "インストール", Slug: "install", ContentMarkdown: "手順。",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	c := env.newClient()
	front := func(st SiteStructure) string {
		t.Helper()
		st.FrontPage = "intro"
		if err := env.server.saveSiteStructure(t.Context(), st); err != nil {
			t.Fatal(err)
		}
		res := c.get("/")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<h1 id="front-hero-title" class="article-title">はじめに</h1>`)
		return res.Body
	}
	hidden := []string{`class="front-news"`, "新着記事", "まだ公開された記事はありません。", `class="front-news-more"`}

	expectNotContainsAny(t, "no menu", front(SiteStructure{}), hidden...)
	expectNotContainsAny(t, "page menu", front(SiteStructure{Menu: []MenuItem{
		{Type: MenuItemPage, Target: "intro"}, {Type: MenuItemPage, Target: "install"}}}), hidden...)

	// メニューに記事の一覧があれば、記事がまだ無くても欄を出す（従来どおり）。
	body := front(SiteStructure{Menu: []MenuItem{{Type: MenuItemPage, Target: "install"}, {Type: MenuItemArticles}}})
	expectContains(t, body, `<section class="front-news" aria-labelledby="front-news-title">`,
		"まだ公開された記事はありません。", `<p class="front-news-more"><a href="/articles">`)
	body = front(SiteStructure{Menu: []MenuItem{{Label: "読む", Type: MenuItemGroup, Children: []MenuItem{{Type: MenuItemArticles}}}}})
	expectContains(t, body, `<section class="front-news"`, "まだ公開された記事はありません。")

	// 記事を公開すれば、メニューに記事の一覧が無くても新着記事を出す。
	seedArticle(t, env.db, article.Article{Title: "リリースのお知らせ", Slug: "release", ContentMarkdown: "公開しました。",
		Status: article.StatusPublished, PublishedAt: ago(time.Minute)})
	body = front(SiteStructure{Menu: []MenuItem{{Type: MenuItemPage, Target: "install"}}})
	_, news, ok := strings.Cut(body, `<section class="front-news"`)
	if !ok {
		t.Fatalf("front page with an article has no news section: %.3000s", body)
	}
	expectContains(t, news, "リリースのお知らせ")
	expectNotContainsAny(t, "front news", news, "まだ公開された記事はありません。")
}
