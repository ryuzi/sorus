package mcp

import (
	"context"
	"slices"
	"testing"

	"sorus/internal/article"
)

// cms_list_articles・cms_get_article の結果に kind を返し、
// cms_save_draft は任意の kind（既定 post）を受け付けることを確かめる。

func TestListAndGetArticleKind(t *testing.T) {
	env := newTestEnv(t, Config{})
	post := env.seed(article.Article{Title: "新歓ライブ", Slug: "live", AuthorID: writerA.ID, Status: article.StatusPublished})
	page := env.seed(article.Article{Title: "アクセス", Slug: "access", AuthorID: writerA.ID, Status: article.StatusPublished, Kind: article.KindPage})
	draftPage := env.seed(article.Article{Title: "メンバー", Slug: "members", AuthorID: writerA.ID, Kind: article.KindPage})

	var all listArticlesResult
	env.toolOK(editor.ID, "cms_list_articles", map[string]any{}, &all)
	kinds := map[string]article.Kind{}
	for _, a := range all.Articles {
		kinds[a.ID] = a.Kind
	}
	if kinds[post] != article.KindPost || kinds[page] != article.KindPage || kinds[draftPage] != article.KindPage {
		t.Errorf("kinds = %v", kinds)
	}

	for kind, want := range map[string][]string{"page": sortedIDs(page, draftPage), "post": {post}, "all": sortedIDs(post, page, draftPage)} {
		var res listArticlesResult
		env.toolOK(editor.ID, "cms_list_articles", map[string]any{"kind": kind}, &res)
		if got := sortedIDs(listIDs(res)...); !slices.Equal(got, want) || res.Total != len(want) {
			t.Errorf("kind %s: ids = %v (total %d), want %v", kind, got, res.Total, want)
		}
	}
	// 他の条件と組み合わせられる。
	var published listArticlesResult
	env.toolOK(editor.ID, "cms_list_articles", map[string]any{"kind": "page", "status": "published"}, &published)
	if got := listIDs(published); len(got) != 1 || got[0] != page {
		t.Errorf("published pages = %v", got)
	}
	expectContains(t, env.toolErr(editor.ID, "cms_list_articles", map[string]any{"kind": "news"}), `kind "news" は不正`)

	var detail articleDetail
	env.toolOK(editor.ID, "cms_get_article", map[string]any{"slug": "access"}, &detail)
	if detail.Kind != article.KindPage || detail.PublicPath != "/access" {
		t.Errorf("page detail: kind %q public_path %q", detail.Kind, detail.PublicPath)
	}
	env.toolOK(editor.ID, "cms_get_article", map[string]any{"article_id": post}, &detail)
	if detail.Kind != article.KindPost || detail.PublicPath != "/articles/live" {
		t.Errorf("post detail: kind %q public_path %q", detail.Kind, detail.PublicPath)
	}
}

// 設定で加えた予約語（同期 API のパスの先頭部分）も、固定の予約語と同じくページには使えない。
func TestSaveDraftConfiguredReservedPageSlug(t *testing.T) {
	env := newTestEnv(t, Config{ReservedPageSlugs: []string{"hooks"}})
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "x", "slug": "Hooks", "kind": "page"}),
		`スラッグ "hooks" は固定ページには使えません`)
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "Hooks", "kind": "page"}),
		`スラッグ "hooks" は固定ページには使えません`)
	var post saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "連携", "slug": "hooks"}, &post)
	if post.Article.Slug != "hooks" || post.Article.Kind != article.KindPost {
		t.Errorf("post with slug hooks = %+v", post.Article)
	}
}

func TestSaveDraftKind(t *testing.T) {
	env := newTestEnv(t, Config{})

	var post saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "種類を指定しない下書き"}, &post)
	if post.Article.Kind != article.KindPost || env.article(post.Article.ID).Kind != article.KindPost {
		t.Errorf("default kind = %q, want post", post.Article.Kind)
	}

	var page saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "研究紹介", "slug": "research", "kind": "page"}, &page)
	if page.Article.Kind != article.KindPage || env.article(page.Article.ID).Kind != article.KindPage {
		t.Errorf("page kind = %q, want page", page.Article.Kind)
	}

	// 更新で kind を省略すると現在の種類を保ち、指定すると変える。本文の変更は案として預かり、種類はそのまま。
	var updated saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": page.Article.ID, "content_markdown": "量子計算"}, &updated)
	if a := env.article(page.Article.ID); a.Kind != article.KindPage || a.ContentMarkdown != "" || !updated.NeedsConfirmation {
		t.Errorf("after update without kind: kind %q body %q (result %+v)", a.Kind, a.ContentMarkdown, updated)
	}
	if p, err := LoadProposal(context.Background(), env.db, page.Article.ID); err != nil || p == nil || p.Changes.Kind != nil {
		t.Errorf("proposal without kind = %+v, %v", p, err)
	}
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": post.Article.ID, "kind": "page"}, &updated)
	if updated.Article.Kind != article.KindPage {
		t.Errorf("after changing kind: %q", updated.Article.Kind)
	}

	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "x", "kind": "news"}), `kind "news" は不正`)
	// 予約語のスラッグはページには使えない（スラッグを省略したときはタイトルから作ったスラッグで判定する）。
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "x", "slug": "admin", "kind": "page"}),
		`スラッグ "admin" は固定ページには使えません`)
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"title": "Search", "kind": "page"}),
		`スラッグ "search" は固定ページには使えません`)
	expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", map[string]any{"article_id": page.Article.ID, "slug": "articles"}),
		`スラッグ "articles" は固定ページには使えません`)
	if a := env.article(page.Article.ID); a.Slug != "research" {
		t.Errorf("slug after rejected update = %q", a.Slug)
	}
	// 記事（post）のスラッグとしては使える。
	var adminPost saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "管理の話", "slug": "admin"}, &adminPost)
	if adminPost.Article.Slug != "admin" || adminPost.Article.Kind != article.KindPost {
		t.Errorf("post with slug admin = %+v", adminPost.Article)
	}
}
