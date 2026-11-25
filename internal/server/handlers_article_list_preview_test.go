package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 記事一覧の表で、タイトル以外の列（作成者・ステータス・更新日時・総PV・操作）は折り返さず、タイトルの列だけを
// 折り返す（長いタイトルでほかの列が縦に潰れないように）。
func TestArticleListKeepsMetaColumnsOnOneLine(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(chief)
	c.createArticle(strings.Repeat("とても長いタイトルの記事", 12), "本文")
	body := c.get("/admin/articles?status=draft").Body

	row := regexp.MustCompile(`(?s)<tr class="article-row.*?</tr>`).FindString(body)
	if row == "" {
		t.Fatalf("no article row\n%.3000s", body)
	}
	cells := regexp.MustCompile(`<td class="([^"]*)"`).FindAllStringSubmatch(row, -1)
	if len(cells) != 6 {
		t.Fatalf("row has %d cells, want 6\n%s", len(cells), row)
	}
	title := cells[0][1]
	if strings.Contains(title, "whitespace-nowrap") || !strings.Contains(title, "min-w-0") || !strings.Contains(title, "max-w-md") {
		t.Errorf("title cell class = %q, want a wrapping cell (min-w-0 max-w-md, no whitespace-nowrap)", title)
	}
	for i, cell := range cells[1:] {
		if !strings.Contains(cell[1], "whitespace-nowrap") {
			t.Errorf("cell %d class = %q, want whitespace-nowrap", i+2, cell[1])
		}
	}
	expectContains(t, body, `<th class="w-full px-4 py-3">`,
		`<th class="whitespace-nowrap px-4 py-3">`, `<th class="whitespace-nowrap px-4 py-3 text-right">`)
}

// 一覧で「ゴミ箱へ」を押したら、ゴミ箱のタブではなく元の一覧（タブ・言語・検索語）へ戻し、移した記事の名前と
// ゴミ箱への導線を出す。
func TestArticleDeleteReturnsToCurrentListWithTrashNotice(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(chief)
	id := c.createArticle(`<b>移す</b>記事`, "本文")
	keep := c.createArticle("残る記事", "本文")

	// 一覧の削除フォームは、表示中のタブ・言語・検索語を送る。
	list := c.get("/admin/articles?status=draft&lang=ja&q=" + url.QueryEscape("記事")).Body
	expectContains(t, list, `action="/admin/articles/`+id+`/delete"`,
		`name="return_status" value="draft"`, `name="return_lang" value="ja"`, `name="return_q" value="記事"`)

	res := c.postForm("/admin/articles/"+id+"/delete", url.Values{
		"return_status": {"draft"}, "return_lang": {"ja"}, "return_q": {"記事"}})
	loc := expectRedirect(t, res, "/admin/articles?status=draft&lang=ja&q="+url.QueryEscape("記事")+"&flash=deleted&deleted="+id)
	if got := env.mustGetArticle(id).Status; got != "deleted" {
		t.Fatalf("status = %q, want deleted", got)
	}

	page := c.get(loc)
	expectStatus(t, page, http.StatusOK)
	// タイトルはエスケープして出す。
	expectContains(t, page.Body, `id="trash-notice"`, "「&lt;b&gt;移す&lt;/b&gt;記事」をゴミ箱に移動しました。",
		`<a href="/admin/articles?status=deleted" id="trash-notice-link"`, "ゴミ箱を見る",
		`aria-selected="true"`, `data-article-id="`+keep+`"`)
	expectNotContainsAny(t, "trash notice", page.Body, "<b>移す</b>", `data-flash="ゴミ箱に移動しました"`)

	t.Run("english", func(t *testing.T) {
		en := env.loggedIn(chief)
		en.get("/admin/set-lang?lang=en&return=/admin")
		body := en.get(loc).Body
		expectContains(t, body, "Moved “&lt;b&gt;移す&lt;/b&gt;記事” to the trash.", ">View trash</a>")
	})

	t.Run("pages return to the page list", func(t *testing.T) {
		res := c.postForm("/admin/articles/new", url.Values{"title": {"固定ページ"}, "kind": {"page"}, "category_id": {"1"},
			"content_markdown": {"本文"}, "action": {"draft"}})
		m := editPathPattern.FindStringSubmatch(expectRedirect(t, res, "/admin/articles/"))
		if m == nil {
			t.Fatal("no page id")
		}
		loc := expectRedirect(t, c.postForm("/admin/articles/"+m[1]+"/delete", url.Values{"return_status": {"draft"}}),
			"/admin/articles?status=draft&kind=page&flash=deleted&deleted="+m[1])
		expectContains(t, c.get(loc).Body, `href="/admin/articles?status=deleted&kind=page" id="trash-notice-link"`)
	})

	t.Run("without a list to return to it opens the trash", func(t *testing.T) {
		other := c.createArticle("編集画面から移す記事", "本文")
		loc := expectRedirect(t, c.postForm("/admin/articles/"+other+"/delete", url.Values{"return_status": {"bogus"}}),
			"/admin/articles?status=deleted&flash=deleted&deleted="+other)
		body := c.get(loc).Body
		// ゴミ箱の一覧ではお知らせだけを出し、「ゴミ箱を見る」は出さない。
		expectContains(t, body, "「編集画面から移す記事」をゴミ箱に移動しました。")
		expectNotContainsAny(t, "trash tab", body, `id="trash-notice-link"`)
	})

	t.Run("only titles of trashed articles are shown", func(t *testing.T) {
		body := c.get("/admin/articles?flash=deleted&deleted=" + keep).Body
		expectNotContainsAny(t, "not trashed", body, `id="trash-notice"`)
		body = c.get("/admin/articles?flash=deleted&deleted=no-such-id").Body
		expectNotContainsAny(t, "unknown id", body, `id="trash-notice"`)
	})
}

// 記事一覧の空状態と取り込みの画面では、WordPress に限らない「他のサービスから取り込む」と書く。
func TestImportWordingIsGeneric(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	body := c.get("/admin/articles").Body
	expectContains(t, body, `id="import-wordpress-link" class="text-zinc-600 underline">他のサービスから取り込む</a>`)
	expectNotContainsAny(t, "article list (ja)", body, "WordPress から取り込む")
	expectContains(t, c.get(importPagePath).Body, "他のサービスから取り込む")

	c.get("/admin/set-lang?lang=en&return=/admin")
	body = c.get("/admin/articles").Body
	expectContains(t, body, ">Import from another service</a>")
	expectNotContainsAny(t, "article list (en)", body, "Import from WordPress")
}

// 下書き・校閲待ち・予約中の記事も、適用中の公開テーマで公開ページと同じに描画して確かめられる。
func TestArticleThemePreviewRendersUnpublishedArticlesWithCurrentTheme(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnvWith(t, Config{CountStaffViews: true}, nil)
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES ('theme', 'news'), ('analytics_tag', '<script id="site-analytics"></script>')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}
	writer := env.loggedIn(studentA)
	draft := writer.createArticle("下書きの記事", "## はじめに\n\n本文の段落です。\n\n$$E = mc^2$$")

	res := writer.get("/admin/articles/" + draft + "/preview")
	expectStatus(t, res, http.StatusOK)
	for header, want := range map[string]string{"Cache-Control": "no-store", "X-Robots-Tag": "noindex"} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	expectContains(t, res.Body, `data-theme="news"`, `/static/themes/news/theme.css?v=`, `<meta name="robots" content="noindex">`,
		"<title>下書きの記事 | ", "本文の段落です。", "はじめに")
	expectNotContainsAny(t, "draft preview", res.Body, `/static/themes/modern/`, `id="site-analytics"`, windowScriptMarker,
		`class="theme-preview-bar"`)

	// ほかの人の下書き・校閲待ち・予約中・公開中の記事も、ライター・編集者・管理者なら開ける。
	other := env.loggedIn(studentB).createArticle("鈴木さんの記事", "本文")
	for _, st := range []struct{ status, publishedAt string }{
		{"draft", ""}, {"review", ""},
		{"published", time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)},
		{"published", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)},
	} {
		var pub any
		if st.publishedAt != "" {
			pub = st.publishedAt
		}
		if _, err := env.db.Exec(`UPDATE articles SET status = ?, published_at = ? WHERE id = ?`, st.status, pub, other); err != nil {
			t.Fatal(err)
		}
		for _, u := range []testUser{studentA, chief, admin} {
			res := env.loggedIn(u).get("/admin/articles/" + other + "/preview")
			expectStatus(t, res, http.StatusOK)
			expectContains(t, res.Body, "鈴木さんの記事", `data-theme="news"`)
		}
	}
	// プレビューは閲覧数に数えない（スタッフの閲覧も数える設定でも）。
	env.flushAnalytics()
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily`); n != 0 {
		t.Errorf("previews counted %d views, want 0", n)
	}

	t.Run("pages use the page template", func(t *testing.T) {
		res := writer.postForm("/admin/articles/new", url.Values{"title": {"アクセス"}, "kind": {"page"}, "category_id": {"1"},
			"content_markdown": {"地図の説明"}, "action": {"draft"}})
		m := editPathPattern.FindStringSubmatch(expectRedirect(t, res, "/admin/articles/"))
		if m == nil {
			t.Fatal("no page id")
		}
		page := writer.get("/admin/articles/" + m[1] + "/preview")
		expectStatus(t, page, http.StatusOK)
		expectContains(t, page.Body, "地図の説明", `data-theme="news"`)
	})

	t.Run("trashed and unknown articles are not found", func(t *testing.T) {
		expectStatus(t, writer.postForm("/admin/articles/"+draft+"/delete", nil), http.StatusSeeOther)
		expectStatus(t, writer.get("/admin/articles/"+draft+"/preview"), http.StatusNotFound)
		expectStatus(t, writer.get("/admin/articles/no-such-id/preview"), http.StatusNotFound)
	})

	t.Run("requires login", func(t *testing.T) {
		expectRedirect(t, env.newClient().get("/admin/articles/"+other+"/preview"), "/admin/login")
	})
}

// デスクトップモードでも、下書きのプレビューには開いている画面として数える接続のスクリプトを入れない。
func TestArticleThemePreviewOmitsDesktopWindowScript(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	id := c.createArticle("デスクトップの下書き", "本文")
	expectContains(t, c.get("/admin/articles").Body, windowScriptMarker)
	res := c.get("/admin/articles/" + id + "/preview")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "デスクトップの下書き")
	expectNotContainsAny(t, "desktop draft preview", res.Body, windowScriptMarker)
}
