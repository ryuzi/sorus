package server

import (
	"fmt"
	htmltemplate "html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/templates"
)

// pageLinksString は pageLinks の並びを「1 … 4 5 [6] 7 8 … 47」の形にする。
func pageLinksString(links []pageLink) string {
	parts := make([]string, 0, len(links))
	for _, l := range links {
		switch {
		case l.Gap:
			parts = append(parts, "…")
		case l.Current:
			parts = append(parts, "["+strconv.Itoa(l.Num)+"]")
		default:
			parts = append(parts, strconv.Itoa(l.Num))
		}
	}
	return strings.Join(parts, " ")
}

func TestPageLinksWindow(t *testing.T) {
	for _, tc := range []struct {
		current, total int
		want           string
	}{
		{1, 1, ""},
		{1, 2, "[1] 2"},
		{2, 2, "1 [2]"},
		{1, 7, "[1] 2 3 … 7"},
		{3, 7, "1 2 [3] 4 5 6 7"},
		{4, 7, "1 2 3 [4] 5 6 7"},
		{1, 47, "[1] 2 3 … 47"},
		{4, 47, "1 2 3 [4] 5 6 … 47"},
		{5, 47, "1 2 3 4 [5] 6 7 … 47"}, // 1 と 3 の間は1ページだけなので省略せず番号を出す
		{6, 47, "1 … 4 5 [6] 7 8 … 47"},
		{43, 47, "1 … 41 42 [43] 44 45 46 47"},
		{47, 47, "1 … 45 46 [47]"},
		{99, 47, "1 … 45 46 [47]"},
	} {
		links := pageLinks(tc.current, tc.total, func(n int) string { return fmt.Sprintf("/p/%d", n) })
		if got := pageLinksString(links); got != tc.want {
			t.Errorf("pageLinks(%d, %d) = %q, want %q", tc.current, tc.total, got, tc.want)
		}
		for _, l := range links {
			if !l.Gap && l.URL != fmt.Sprintf("/p/%d", l.Num) {
				t.Errorf("link %d URL = %q", l.Num, l.URL)
			}
		}
	}
}

var paginationNav = regexp.MustCompile(`(?s)<nav class="pagination".*?</nav>`)

// /articles のページ送りは「前へ」・ページ番号（先頭・末尾・現在の前後2ページ、離れた所は …）・「次へ」・現在位置を出し、
// 現在のページは aria-current="page" の番号（リンクにしない）にする。
func TestArticlesPaginationNumberedWindow(t *testing.T) {
	const pages = 12
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		for i := 1; i <= publicPerPage*pages; i++ {
			seedArticle(t, db, article.Article{Title: fmt.Sprintf("記事%03d", i), Slug: fmt.Sprintf("post-%d", i),
				Status: article.StatusPublished, PublishedAt: ago(time.Duration(i) * time.Minute)})
		}
	})
	c := env.newClient()

	res := c.get("/articles/page/6")
	expectStatus(t, res, http.StatusOK)
	nav := paginationNav.FindString(res.Body)
	if nav == "" {
		t.Fatalf("no pagination: %s", res.Body)
	}
	expectContains(t, nav,
		`<a class="page-prev" href="/articles/page/5" rel="prev">← 前へ</a>`,
		`<a class="page-number" href="/articles" aria-label="1ページ目">1</a>`,
		`<span class="page-ellipsis" aria-hidden="true">…</span>`,
		`<a class="page-number" href="/articles/page/4" aria-label="4ページ目">4</a>`,
		`<span class="page-number" aria-current="page">6</span>`,
		`<a class="page-number" href="/articles/page/8" aria-label="8ページ目">8</a>`,
		`<a class="page-number" href="/articles/page/12" aria-label="12ページ目">12</a>`,
		`<a class="page-next" href="/articles/page/7" rel="next">次へ →</a>`,
		`<span class="page-status">6 / 12</span>`)
	if got := pageLinksString(navLinks(nav)); got != "1 … 4 5 [6] 7 8 … 12" {
		t.Errorf("numbers = %q", got)
	}
	if n := strings.Count(nav, `aria-current="page"`); n != 1 {
		t.Errorf("aria-current count = %d", n)
	}
	for _, bad := range []string{`href="/articles/page/3"`, `href="/articles/page/9"`, `href="/articles/page/6"`} {
		if strings.Contains(nav, bad) {
			t.Errorf("unexpected %s in %s", bad, nav)
		}
	}

	// 先頭のページ: 「前へ」は押せず、1 が現在のページ。絞り込みの問い合わせはページ番号のリンクに引き継ぐ。
	res = c.get("/articles?year=" + time.Now().UTC().Format("2006"))
	expectStatus(t, res, http.StatusOK)
	nav = paginationNav.FindString(res.Body)
	expectContains(t, nav, `<span class="disabled page-prev">← 前へ</span>`, `<span class="page-number" aria-current="page">1</span>`)
	if !regexp.MustCompile(`href="/articles\?year=\d{4}&amp;page=2" aria-label="2ページ目"`).MatchString(nav) {
		t.Errorf("filtered page link missing: %s", nav)
	}

	// 最後のページ: 「次へ」は押せない。
	res = c.get(fmt.Sprintf("/articles/page/%d", pages))
	nav = paginationNav.FindString(res.Body)
	expectContains(t, nav, `<span class="disabled page-next">次へ →</span>`, fmt.Sprintf(`<span class="page-number" aria-current="page">%d</span>`, pages))

	if pageLinksString(navLinks(nav)) != "1 … 10 11 [12]" {
		t.Errorf("last page numbers = %q", pageLinksString(navLinks(nav)))
	}
}

// 英語のページのページ送りは英語の文言・ラベルにする。
func TestPaginationPartialEnglishLabels(t *testing.T) {
	tmpl, err := htmltemplate.ParseFS(templates.FS, "public/partials/pagination.html")
	if err != nil {
		t.Fatal(err)
	}
	pg := pagination{Page: 2, TotalPages: 3, PrevURL: "/en/articles", NextURL: "/en/articles/page/3", Lang: "en"}
	pg.Pages = pageLinks(2, 3, func(n int) string { return paginationURL("/en/articles", n) })
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, "pagination", pg); err != nil {
		t.Fatal(err)
	}
	expectContains(t, b.String(), `aria-label="Pagination"`, `<a class="page-prev" href="/en/articles" rel="prev">← Previous</a>`,
		`<a class="page-next" href="/en/articles/page/3" rel="next">Next →</a>`, `href="/en/articles" aria-label="Page 1">1</a>`,
		`<span class="page-number" aria-current="page">2</span>`, `href="/en/articles/page/3" aria-label="Page 3">3</a>`,
		`<span class="page-status">2 / 3</span>`)
}

var navLinkPattern = regexp.MustCompile(`<a class="page-number"[^>]*>(\d+)</a>|<span class="page-number" aria-current="page">(\d+)</span>|<span class="page-ellipsis"`)

// navLinks はページ送りの HTML のページ番号の並びを pageLink にする。
func navLinks(nav string) []pageLink {
	var out []pageLink
	for _, m := range navLinkPattern.FindAllStringSubmatch(nav, -1) {
		switch {
		case m[1] != "":
			n, _ := strconv.Atoi(m[1])
			out = append(out, pageLink{Num: n})
		case m[2] != "":
			n, _ := strconv.Atoi(m[2])
			out = append(out, pageLink{Num: n, Current: true})
		default:
			out = append(out, pageLink{Gap: true})
		}
	}
	return out
}
