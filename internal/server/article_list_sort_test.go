package server

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"

	"github.com/jmoiron/sqlx"
)

// listOrder はタブごとの既定の並び順と、選んだ並び順の ORDER BY を返す。既定の公開中の並びは公開サイトと同じ公開日の新しい順。
func TestListOrderDefaultsPerTabAndExplicitSorts(t *testing.T) {
	cases := []struct {
		status article.Status
		sort   string
		want   string
	}{
		{article.StatusPublished, "", `julianday(a.published_at) DESC, a.id`},
		{article.StatusScheduled, "", `julianday(a.published_at) ASC, a.id`},
		{article.StatusDraft, "", `julianday(a.updated_at) DESC, a.id`},
		{article.StatusReview, "", `julianday(a.updated_at) DESC, a.id`},
		{article.StatusDeleted, "", `julianday(a.updated_at) DESC, a.id`},
		{article.StatusDraft, "published_desc", `julianday(COALESCE(a.published_at, a.updated_at)) DESC, a.id`},
		{article.StatusPublished, "updated_desc", `julianday(a.updated_at) DESC, a.id`},
		{article.StatusScheduled, "updated_asc", `julianday(a.updated_at) ASC, a.id`},
		{article.StatusPublished, "title_asc", `a.title COLLATE NOCASE ASC, a.id`},
	}
	for _, c := range cases {
		if got := listOrder(c.status, c.sort); got != c.want {
			t.Errorf("listOrder(%q, %q) = %q, want %q", c.status, c.sort, got, c.want)
		}
	}
}

// 並び順のクエリは選択肢に無い値を既定（""）に戻す（SQL に任意の文字列を入れない）。
func TestValidSortWhitelist(t *testing.T) {
	for _, s := range []string{"", "published_desc", "updated_desc", "updated_asc", "title_asc"} {
		if got := validSort(s); got != s {
			t.Errorf("validSort(%q) = %q, want unchanged", s, got)
		}
	}
	for _, s := range []string{"title_desc", "a.title; DROP TABLE articles", "TITLE_ASC", " updated_desc"} {
		if got := validSort(s); got != "" {
			t.Errorf("validSort(%q) = %q, want \"\"", s, got)
		}
	}
}

// 「日付」の列: 公開中は公開日時（公開から1日を超えて直したときは更新日を添える）、予約中は公開予定日時、
// 下書き・校閲待ち・ゴミ箱は更新日時。日時は JST。
func TestSetRowDates(t *testing.T) {
	pub := time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC) // JST 2026/11/01 10:30
	at := func(d time.Duration) *time.Time { v := pub.Add(d); return &v }
	rows := []articleRow{
		{Status: article.StatusPublished, PublishedAt: &pub, UpdatedAt: pub.Add(48 * time.Hour)},
		{Status: article.StatusPublished, PublishedAt: &pub, UpdatedAt: pub.Add(23 * time.Hour)},
		{Status: article.StatusScheduled, PublishedAt: at(10 * 24 * time.Hour), UpdatedAt: pub},
		{Status: article.StatusDraft, PublishedAt: at(-time.Hour), UpdatedAt: pub.Add(14 * time.Hour)},
		{Status: article.StatusReview, UpdatedAt: pub},
		{Status: article.StatusDeleted, PublishedAt: &pub, UpdatedAt: pub.Add(72 * time.Hour)},
	}
	type want struct{ primary, note string }
	ja := []want{
		{"公開 2026/11/01 10:30", "更新 2026/11/03"},
		{"公開 2026/11/01 10:30", ""},
		{"公開予定 2026/11/11 10:30", ""},
		{"更新 2026/11/02 00:30", ""},
		{"更新 2026/11/01 10:30", ""},
		{"更新 2026/11/04 10:30", ""},
	}
	en := []want{
		{"Published 2026/11/01 10:30", "Updated 2026/11/03"},
		{"Published 2026/11/01 10:30", ""},
		{"Scheduled 2026/11/11 10:30", ""},
		{"Updated 2026/11/02 00:30", ""},
		{"Updated 2026/11/01 10:30", ""},
		{"Updated 2026/11/04 10:30", ""},
	}
	for lang, wants := range map[string][]want{"ja": ja, "en": en} {
		setRowDates(rows, lang)
		for i, w := range wants {
			if rows[i].DatePrimary != w.primary || rows[i].DateNote != w.note {
				t.Errorf("%s row %d (%s) = %q / %q, want %q / %q", lang, i, rows[i].Status, rows[i].DatePrimary, rows[i].DateNote, w.primary, w.note)
			}
		}
	}
}

// seedListArticle は状態 st・公開日時 published・更新日時 updated の記事を直接保存し、ID を返す。
func seedListArticle(t *testing.T, db *sqlx.DB, title, slug string, st article.Status, published *time.Time, updated time.Time) string {
	t.Helper()
	id := seedArticle(t, db, article.Article{Title: title, Slug: slug, ContentMarkdown: "本文", Status: st, PublishedAt: published})
	if _, err := db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, updated.UTC(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// expectSlugOrder は一覧の本文で slugs がこの順に並んでいることを確かめる。
func expectSlugOrder(t *testing.T, label, body string, slugs ...string) {
	t.Helper()
	prev := -1
	for _, s := range slugs {
		i := strings.Index(body, ">/"+s+"<")
		if i < 0 {
			t.Fatalf("%s: slug %q not in list\n%.4000s", label, s, body)
		}
		if i < prev {
			t.Errorf("%s: slug %q is out of order (want %v)", label, s, slugs)
		}
		prev = i
	}
}

// 記事一覧の既定の並び順はタブごとに変わり、?sort= で選んだ並び順に変えられる。選択肢に無い値は既定に戻す。
func TestArticleListSortDefaultsAndExplicitSort(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(chief)
	now := time.Now().UTC()
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }

	// 公開中: 公開日は old < mid < new。更新日時は old が最も新しい（公開後に直した）。
	seedListArticle(t, env.db, "Charlie 古い公開", "pub-old", article.StatusPublished, ago(72*time.Hour), now.Add(-time.Hour))
	seedListArticle(t, env.db, "alpha 中くらい", "pub-mid", article.StatusPublished, ago(48*time.Hour), now.Add(-47*time.Hour))
	seedListArticle(t, env.db, "Bravo 新しい公開", "pub-new", article.StatusPublished, ago(24*time.Hour), now.Add(-23*time.Hour))
	// 予約中: 公開予定は soon < later。
	later, soon := now.Add(72*time.Hour), now.Add(24*time.Hour)
	seedListArticle(t, env.db, "後の予約", "sch-later", article.StatusPublished, &later, now.Add(-time.Hour))
	seedListArticle(t, env.db, "近い予約", "sch-soon", article.StatusPublished, &soon, now.Add(-2*time.Hour))
	// 下書き: 更新日時は d-old < d-new。
	seedListArticle(t, env.db, "古い下書き", "d-old", article.StatusDraft, nil, now.Add(-5*time.Hour))
	seedListArticle(t, env.db, "新しい下書き", "d-new", article.StatusDraft, nil, now.Add(-time.Hour))

	pub := c.get("/admin/articles?status=published").Body
	expectSlugOrder(t, "published default", pub, "pub-new", "pub-mid", "pub-old")
	expectSlugOrder(t, "scheduled default", c.get("/admin/articles?status=scheduled").Body, "sch-soon", "sch-later")
	expectSlugOrder(t, "draft default", c.get("/admin/articles?status=draft").Body, "d-new", "d-old")

	expectSlugOrder(t, "updated_desc", c.get("/admin/articles?status=published&sort=updated_desc").Body, "pub-old", "pub-new", "pub-mid")
	expectSlugOrder(t, "updated_asc", c.get("/admin/articles?status=published&sort=updated_asc").Body, "pub-mid", "pub-new", "pub-old")
	expectSlugOrder(t, "title_asc", c.get("/admin/articles?status=published&sort=title_asc").Body, "pub-mid", "pub-new", "pub-old")
	expectSlugOrder(t, "published_desc", c.get("/admin/articles?status=scheduled&sort=published_desc").Body, "sch-later", "sch-soon")
	expectSlugOrder(t, "draft updated_asc", c.get("/admin/articles?status=draft&sort=updated_asc").Body, "d-old", "d-new")

	// 選択肢に無い値は既定の並び順にし、リンクにも付けない。
	bad := c.get("/admin/articles?status=published&sort=" + url.QueryEscape("a.title DESC--")).Body
	expectSlugOrder(t, "invalid sort", bad, "pub-new", "pub-mid", "pub-old")
	expectNotContains(t, bad, "sort=a.title", `name="return_sort"`)
	expectContains(t, bad, `<option value="" selected>標準</option>`)
}

// 選んだ並び順は、種類・状態・言語のリンク、検索欄、クリア、ページ送り、「ゴミ箱へ」の戻り先に引き継ぐ。
func TestArticleListSortIsPlumbedThroughLinksAndForms(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	enableMultilingual(t, env.db)
	c := env.loggedIn(chief)
	id := c.createArticle("並び順の記事", "本文")
	for i := 0; i < articlesPerPage; i++ {
		c.createArticle("埋め草", "本文")
	}

	body := c.get("/admin/articles?status=draft&lang=ja&q=" + url.QueryEscape("記事") + "&sort=title_asc").Body
	expectContains(t, body,
		`<option value="title_asc" selected>タイトル順</option>`,
		`id="article-sort"`, `<select name="sort" id="article-sort-select"`,
		`href="/admin/articles?kind=post&lang=ja&q=%e8%a8%98%e4%ba%8b&sort=title_asc"`,
		`href="/admin/articles?status=review&lang=ja&q=%e8%a8%98%e4%ba%8b&sort=title_asc"`,
		`href="/admin/articles?status=draft&lang=en&q=%e8%a8%98%e4%ba%8b&sort=title_asc"`,
		`href="/admin/articles?kind=post&status=draft&lang=ja&sort=title_asc" id="article-search-clear"`,
		`<input type="hidden" name="sort" value="title_asc">`,
		`name="return_sort" value="title_asc"`,
	)
	// 並び順の欄は、ほかの絞り込み（種類・状態・言語・検索語）を送る。
	form := regexp.MustCompile(`(?s)<form id="article-sort".*?</form>`).FindString(body)
	expectContains(t, form, `name="kind" value="post"`, `name="status" value="draft"`, `name="lang" value="ja"`, `name="q" value="記事"`)

	// ページ送りのリンク。
	all := c.get("/admin/articles?status=draft&sort=updated_asc").Body
	expectContains(t, all, `href="/admin/articles?status=draft&sort=updated_asc&page=2"`)

	// 「ゴミ箱へ」は並び順を保った一覧へ戻す。不正な値は捨てる。
	res := c.postForm("/admin/articles/"+id+"/delete", url.Values{"return_status": {"draft"}, "return_sort": {"title_asc"}})
	expectRedirect(t, res, "/admin/articles?status=draft&sort=title_asc&flash=deleted&deleted="+id)
	other := c.createArticle("別の記事", "本文")
	res = c.postForm("/admin/articles/"+other+"/delete", url.Values{"return_status": {"draft"}, "return_sort": {"bogus"}})
	expectRedirect(t, res, "/admin/articles?status=draft&flash=deleted&deleted="+other)
}

// 「日付」の列は1列のまま、状態に合わせた日付を出す。予約中の公開予定日時はステータスの欄から日付の欄へ移す。
func TestArticleListSmartDateColumn(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(chief)
	jstLoc := time.FixedZone("JST", 9*60*60)
	now := time.Now().UTC().Truncate(time.Minute)
	published := now.Add(-72 * time.Hour)
	seedListArticle(t, env.db, "直した公開記事", "edited", article.StatusPublished, &published, now.Add(-time.Hour))
	at := now.Add(48 * time.Hour)
	seedScheduled(t, env.db, article.Article{Title: "予約の記事", Slug: "scheduled-one", ContentMarkdown: "本文"}, at)

	pub := c.get("/admin/articles?status=published").Body
	expectContains(t, pub, `<th class="whitespace-nowrap px-4 py-3">日付</th>`,
		`<div class="article-date-primary">公開 `+published.In(jstLoc).Format("2006/01/02 15:04")+`</div>`,
		`<div class="article-date-note mt-0.5 text-xs text-neutral-400">更新 `+now.Add(-time.Hour).In(jstLoc).Format("2006/01/02")+`</div>`)
	expectNotContains(t, pub, ">更新日時</th>")
	if n := strings.Count(regexp.MustCompile(`(?s)<thead.*?</thead>`).FindString(pub), "<th "); n != 6 {
		t.Errorf("header has %d columns, want 6", n)
	}

	sch := c.get("/admin/articles?status=scheduled").Body
	row := regexp.MustCompile(`(?s)<tr class="article-row.*?</tr>`).FindString(sch)
	want := `<div class="article-date-primary">公開予定 ` + at.In(jstLoc).Format("2006/01/02 15:04") + `</div>`
	expectContains(t, row, want)
	if strings.Count(row, at.In(jstLoc).Format("2006/01/02 15:04")) != 1 {
		t.Errorf("scheduled date should appear once (date cell only)\n%s", row)
	}

	c.createArticle("下書き", "本文")
	draft := c.get("/admin/articles?status=draft").Body
	expectContains(t, draft, `<div class="article-date-primary">更新 `)

	// 英語の画面。
	en := c.getAs("/admin/articles?status=published", "", map[string]string{"Accept-Language": "en"}).Body
	expectContains(t, en, `<th class="whitespace-nowrap px-4 py-3">Date</th>`,
		`<div class="article-date-primary">Published `+published.In(jstLoc).Format("2006/01/02 15:04")+`</div>`,
		`>Updated `+now.Add(-time.Hour).In(jstLoc).Format("2006/01/02")+`</div>`,
		`<option value="title_asc">Title (A–Z)</option>`)
}
