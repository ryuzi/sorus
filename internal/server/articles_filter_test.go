package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// newArticlesFilterEnv はカテゴリ2つ（学会・お知らせ）× 年（2026・2025・2007）の記事があるサイトを起動する。
//
//	学会: 2026 に1件、2025 に publicPerPage+2 件
//	お知らせ: 2026 に2件、2007 に1件
//	英語の記事: 学会 2025 に1件、お知らせ 2026 に1件
func newArticlesFilterEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		conf := seedCategory(t, db, "学会・研究発表", "conference", "")
		news := seedCategory(t, db, "お知らせ", "news", "")
		seedCategory(t, db, "空のカテゴリ", "empty", "")
		n := 0
		add := func(cat int64, year int, title, lang string) {
			n++
			published := time.Date(year, time.March, 1, 12, n, 0, 0, time.UTC)
			seedArticle(t, db, article.Article{Title: title, Slug: fmt.Sprintf("filter-post-%d", n), CategoryID: cat,
				ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: &published, Lang: lang})
		}
		add(conf, 2026, "学会2026", "ja")
		for i := 1; i <= publicPerPage+2; i++ {
			add(conf, 2025, fmt.Sprintf("学会2025-%02d", i), "ja")
		}
		add(news, 2026, "お知らせ2026-1", "ja")
		add(news, 2026, "お知らせ2026-2", "ja")
		add(news, 2007, "お知らせ2007", "ja")
		add(conf, 2025, "Conference 2025", "en")
		add(news, 2026, "News 2026", "en")
	})
}

// articleTitles は記事の一覧のカードのタイトルを返す。
func articleTitles(body string) []string {
	var titles []string
	for {
		_, rest, ok := strings.Cut(body, `<h2 class="card-title">`)
		if !ok {
			return titles
		}
		_, rest, _ = strings.Cut(rest, `">`)
		title, rest, _ := strings.Cut(rest, "</a>")
		titles = append(titles, title)
		body = rest
	}
}

// filterBar は記事の一覧の見出し行（年のセレクトを含む）から絞り込み欄までの HTML を返す。
func filterBar(t *testing.T, body string) string {
	t.Helper()
	_, bar, ok := strings.Cut(body, `<div class="articles-header-row">`)
	if !ok {
		t.Fatal("articles-filter-bar is missing")
	}
	bar, _, _ = strings.Cut(bar, `<div class="card-grid">`)
	return bar
}

func TestPublicArticlesFilterBar(t *testing.T) {
	env := newArticlesFilterEnv(t)
	c := env.newClient()

	res := c.get("/articles")
	expectStatus(t, res, http.StatusOK)
	bar := filterBar(t, res.Body)
	expectContains(t, bar,
		`<nav class="category-filter-pills" aria-label="カテゴリ絞り込み">`,
		`<a class="category-filter-pill is-current" href="/articles" aria-current="page">すべて <span class="category-filter-count">(`+fmt.Sprint(publicPerPage+6)+`)</span></a>`,
		`href="/articles?category=conference">学会・研究発表 <span class="category-filter-count">(`+fmt.Sprint(publicPerPage+3)+`)</span>`,
		`href="/articles?category=news">お知らせ <span class="category-filter-count">(3)</span>`,
		`<div class="year-filter-select">`,
		`<select id="year-filter" aria-label="年別アーカイブ" onchange="if(this.value) window.location.href=this.value;">`,
		`<option value="/articles" selected>すべての年 (`+fmt.Sprint(publicPerPage+6)+`)</option>`,
		`<option value="/articles?year=2026">2026年 (3)</option>`,
		`<option value="/articles?year=2025">2025年 (`+fmt.Sprint(publicPerPage+2)+`)</option>`,
		`<option value="/articles?year=2007">2007年 (1)</option>`,
		// JavaScript が無効でも年で絞り込めるリンク。
		`<nav class="archive-years"`, `href="/articles?year=2026"`, `href="/articles?year=2007"`,
	)
	// 記事の無いカテゴリ・未分類は出さない。
	expectNotContainsAny(t, "filter bar", bar, "空のカテゴリ", "未分類")
	// カテゴリはカテゴリの登録順。
	if strings.Index(bar, "学会・研究発表") > strings.Index(bar, "お知らせ") {
		t.Error("categories are not in registration order")
	}
}

func TestPublicArticlesCategoryAndYearFilter(t *testing.T) {
	env := newArticlesFilterEnv(t)
	c := env.newClient()

	// カテゴリと年の両方で絞り込む。
	res := c.get("/articles?category=news&year=2026")
	expectStatus(t, res, http.StatusOK)
	if got := fmt.Sprint(articleTitles(res.Body)); got != "[お知らせ2026-2 お知らせ2026-1]" {
		t.Errorf("category=news&year=2026 titles = %s", got)
	}
	expectContains(t, res.Body, `<link rel="canonical" href="https://media.example.ac.jp/articles?category=news&amp;year=2026">`)
	bar := filterBar(t, res.Body)
	expectContains(t, bar,
		// カテゴリの切り替えは年を保ち、記事数はその年の中で数える。
		`<a class="category-filter-pill" href="/articles?year=2026">すべて <span class="category-filter-count">(3)</span></a>`,
		`<a class="category-filter-pill is-current" href="/articles?category=news&amp;year=2026" aria-current="page">お知らせ <span class="category-filter-count">(2)</span></a>`,
		`href="/articles?category=conference&amp;year=2026">学会・研究発表 <span class="category-filter-count">(1)</span>`,
		// 年の切り替えはカテゴリを保ち、記事数はそのカテゴリの中で数える。
		`<option value="/articles?category=news">すべての年 (3)</option>`,
		`<option value="/articles?category=news&amp;year=2026" selected>2026年 (2)</option>`,
		`<option value="/articles?category=news&amp;year=2007">2007年 (1)</option>`,
	)
	if strings.Contains(bar, `category=news&amp;year=2025`) {
		t.Error("year list for news contains 2025, which has no news articles")
	}

	// カテゴリだけ。
	res = c.get("/articles?category=news")
	expectStatus(t, res, http.StatusOK)
	if got := fmt.Sprint(articleTitles(res.Body)); got != "[お知らせ2026-2 お知らせ2026-1 お知らせ2007]" {
		t.Errorf("category=news titles = %s", got)
	}

	// 組み合わせに記事が無くても、選んだカテゴリ・年は絞り込み欄に残す。
	res = c.get("/articles?category=news&year=2025")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "この条件に合う記事はありません。", `aria-current="page">お知らせ <span class="category-filter-count">(0)</span>`,
		`<option value="/articles?category=news&amp;year=2025" selected>2025年 (0)</option>`)

	// 存在しないカテゴリ・不正な年は 404。
	expectStatus(t, c.get("/articles?category=nope"), http.StatusNotFound)
	expectStatus(t, c.get("/articles?category=news&year=20ab"), http.StatusNotFound)
}

func TestPublicArticlesFilterPagination(t *testing.T) {
	env := newArticlesFilterEnv(t)
	c := env.newClient()

	res := c.get("/articles?category=conference&year=2025")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "1 / 2", `href="/articles?category=conference&amp;year=2025&amp;page=2"`)
	titles := articleTitles(res.Body)
	if len(titles) != publicPerPage {
		t.Fatalf("page 1 has %d articles, want %d", len(titles), publicPerPage)
	}

	res = c.get("/articles?category=conference&year=2025&page=2")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "2 / 2", `href="/articles?category=conference&amp;year=2025" rel="prev"`,
		`<link rel="canonical" href="https://media.example.ac.jp/articles?category=conference&amp;year=2025&amp;page=2">`)
	if got := fmt.Sprint(articleTitles(res.Body)); got != "[学会2025-02 学会2025-01]" {
		t.Errorf("page 2 titles = %s", got)
	}
	expectStatus(t, c.get("/articles?category=conference&year=2025&page=3"), http.StatusNotFound)
}

func TestPublicArticlesFilterEnglish(t *testing.T) {
	env := newArticlesFilterEnv(t)
	c := env.newClient()

	res := c.get("/en/articles")
	expectStatus(t, res, http.StatusOK)
	bar := filterBar(t, res.Body)
	expectContains(t, bar, `aria-label="Filter by category"`,
		`href="/en/articles" aria-current="page">All <span class="category-filter-count">(2)</span>`,
		`href="/en/articles?category=conference">学会・研究発表 <span class="category-filter-count">(1)</span>`,
		`<option value="/en/articles" selected>All Years (2)</option>`,
		`<option value="/en/articles?year=2026">2026 (1)</option>`)
	expectNotContainsAny(t, "en filter bar", bar, "2007")

	res = c.get("/en/articles?category=news&year=2026")
	expectStatus(t, res, http.StatusOK)
	if got := fmt.Sprint(articleTitles(res.Body)); got != "[News 2026]" {
		t.Errorf("en category=news&year=2026 titles = %s", got)
	}
	expectContains(t, res.Body, `<option value="/en/articles?category=news&amp;year=2026" selected>2026 (1)</option>`)
}
