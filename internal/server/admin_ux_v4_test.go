package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/static"
)

// 管理画面 UX v4: 記事一覧の検索、アイキャッチの「この記事で使っている画像」、AI の3画面のタブ、メディアの使用中の記事。

// 検索は2段目（状態のタブの行）に置き、明示の「検索」ボタンと入力欄の中の × を付ける。「＋ 新しい記事」とは離す。
func TestV4ArticleListSearchBar(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	body := adm.get("/admin/articles").Body
	newBtn := strings.Index(body, `id="new-article-button"`)
	bar := strings.Index(body, `id="article-filter-bar"`)
	form := strings.Index(body, `id="article-search"`)
	tabs := strings.Index(body[max(bar, 0):], `role="tablist"`)
	if newBtn < 0 || bar < 0 || form < 0 || tabs < 0 || !(newBtn < bar && bar < form) {
		t.Fatalf("search must sit in the filter bar below the header: new=%d bar=%d form=%d tabs=%d", newBtn, bar, form, tabs)
	}
	expectContains(t, body, `role="search"`, `id="article-search-input"`, `name="q"`,
		`id="article-search-submit" class="btn btn-secondary btn-md"`)
	// 検索していないときは × を出さない。
	expectNotContains(t, body, `id="article-search-clear"`)

	searched := adm.get("/admin/articles?q=" + "桜").Body
	expectContains(t, searched, `id="article-search-clear"`, `aria-label="検索をクリア"`, `value="桜"`)
	// × は検索語だけを外し、種類・状態の絞り込みは残す。
	if !strings.Contains(searched, `href="/admin/articles?kind=`) {
		t.Error("the clear link must keep the kind/status filters")
	}
}

// アイキャッチは、本文で使っている画像をピッカーの先頭に出し、未設定なら「本文の最初の画像を使う」で1回で設定できる。
func TestV4FeaturedImageFromBody(t *testing.T) {
	env := newTestEnv(t)
	body := env.loggedIn(admin).get("/admin/articles/new").Body
	expectContains(t, body,
		`id="media-picker-used"`, `id="media-picker-used-grid"`, `data-msg-used="この記事で使っている画像（{n}枚）"`,
		`id="featured-image-use-first"`, "本文の最初の画像を使う")
	// どちらも、出すかどうかはスクリプトが本文を見て決めるため、最初は隠しておく。
	for _, id := range []string{"media-picker-used", "featured-image-use-first"} {
		if !strings.Contains(tagWithID(t, body, id), " hidden") {
			t.Errorf("#%s must start hidden", id)
		}
	}

	js, err := fs.ReadFile(static.FS, "js/media_picker.js")
	if err != nil {
		t.Fatal(err)
	}
	expectContains(t, string(js),
		"function collectBodyImages(text)", "window.cmsCollectBodyImages = collectBodyImages;",
		// コードブロック・インラインコード・コメントの中の画像は数えない。
		"var fence = null;", "/^ {0,3}[`~]+\\s*$/", "replace(/<!--[\\s\\S]*?-->/g, ' ')",
		// data: の画像や // で始まるアドレスは使わない。
		"function usableImageURL", "'featured'", "featured-image-use-first")
}

// AI の3画面は左メニューの「AI」1項目から開き、画面の中はタブで切り替える。スイッチは管理者だけ。
func TestV4AISectionTabs(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	current := func(body, id string) bool {
		return strings.Contains(tagWithID(t, body, id), `aria-current="page"`)
	}
	for _, tc := range []struct {
		path, current string
	}{
		{aiIntegrationsPath, ""},
		{aiSettingsPath, "ai-tab-ai"},
		{"/admin/settings/mcp", "ai-tab-mcp"},
	} {
		res := adm.get(tc.path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `id="ai-section-title"`, `id="ai-integrations-form"`, `id="ai-tabs"`, "使う AI", "外部の AI アプリとつなぐ")
		expectNotContains(t, res.Body, `id="ai-int-links"`)
		for _, id := range []string{"ai-tab-ai", "ai-tab-mcp"} {
			if got := current(res.Body, id); got != (id == tc.current) {
				t.Errorf("%s: #%s current = %v", tc.path, id, got)
			}
		}
		// 左メニューは「AI」1項目で、どの画面でも選択中。
		if !current(res.Body, "subnav-ai") {
			t.Errorf("%s: the AI item of the settings menu is not current", tc.path)
		}
	}

	// 編集者は文章スタイルと外部の AI アプリとの連携のタブだけで、AI 機能のスイッチは出さない。
	editor := env.loggedIn(chief).get(aiSettingsPath)
	expectStatus(t, editor, http.StatusOK)
	expectContains(t, editor.Body, `id="ai-tabs"`, "文章スタイル", "外部の AI アプリとつなぐ")
	expectNotContains(t, editor.Body, `id="ai-integrations-form"`, "使う AI")
	expectStatus(t, env.loggedIn(chief).get(aiIntegrationsPath), http.StatusForbidden)
}

// 使用中の記事は、公開中なら公開ページの URL、それ以外は編集画面のプレビューで開く。
func TestV4MediaUsagePublicURL(t *testing.T) {
	env := newTestEnv(t)
	// 記事を保存した時刻（公開日時）より後の時計で確かめる。予約は1時間後なので、まだ読者に見えない。
	now := time.Now()
	setClock(t, now.Add(time.Minute))
	file := "v4_photo.webp"
	if _, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, path_medium, path_small,
			width, height, mime_type, uploaded_by, alt, caption) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"v4-media", file, "uploads/"+file, "", "", "", 800, 600, "image/webp", admin.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	body := "![写真](/uploads/" + file + ")"
	post := seedArticle(t, env.db, article.Article{Title: "公開中の記事", Slug: "v4-post", ContentMarkdown: body, Status: article.StatusPublished})
	page := seedArticle(t, env.db, article.Article{Title: "English page", Slug: "v4-page", Kind: article.KindPage, Lang: "en",
		ContentMarkdown: body, Status: article.StatusPublished})
	draft := seedArticle(t, env.db, article.Article{Title: "下書き", Slug: "v4-draft", ContentMarkdown: body, Status: article.StatusDraft})
	scheduled := seedScheduled(t, env.db, article.Article{Title: "予約", Slug: "v4-scheduled", ContentMarkdown: body}, now.Add(time.Hour))

	res := env.loggedIn(admin).get("/admin/api/media/v4-media/articles")
	expectStatus(t, res, http.StatusOK)
	var usage MediaUsageResponse
	if err := json.Unmarshal([]byte(res.Body), &usage); err != nil {
		t.Fatal(err)
	}
	got := map[string]MediaUsedArticle{}
	for _, a := range usage.Articles {
		got[a.ID] = a
	}
	for _, want := range []struct {
		id, url   string
		status    article.Status
		published bool
	}{
		{post, "/articles/v4-post", article.StatusPublished, true},
		{page, "/en/v4-page", article.StatusPublished, true},
		{draft, "/articles/v4-draft", article.StatusDraft, false},
		{scheduled, "/articles/v4-scheduled", article.StatusPublished, false},
	} {
		a, ok := got[want.id]
		if !ok {
			t.Errorf("article %s missing from usage %+v", want.id, usage.Articles)
			continue
		}
		if a.PublicURL != want.url || a.Status != want.status || a.Published != want.published {
			t.Errorf("usage of %s = {url %q status %q published %v}, want {%q %q %v}",
				a.Title, a.PublicURL, a.Status, a.Published, want.url, want.status, want.published)
		}
	}
	expectContains(t, res.Body, `"public_url":"/articles/v4-post"`, `"published":true`, `"status":"draft"`)
}
