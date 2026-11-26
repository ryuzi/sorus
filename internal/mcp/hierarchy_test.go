package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"sorus/internal/article"
)

// cms_save_draft・cms_get_article・cms_list_articles が固定ページの階層（parent_id・parent_slug・sort_order）を
// 扱うことを確かめる。

// orderedIDs は一覧の記事 ID を返された順のまま返す（listIDs は並べ替える）。
func orderedIDs(res listArticlesResult) []string {
	ids := []string{}
	for _, a := range res.Articles {
		ids = append(ids, a.ID)
	}
	return ids
}

func TestSaveDraftPageHierarchy(t *testing.T) {
	env := newTestEnv(t, Config{})

	var parent saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"title": "研究紹介", "slug": "research", "kind": "page"}, &parent)
	parentID := parent.Article.ID
	if parent.Article.ParentID != "" || parent.Article.SortOrder != 0 {
		t.Errorf("top-level page = %+v, want no parent and sort_order 0", parent.Article)
	}

	// parent_slug（先頭の / は省いてよい）で固定ページを解決し、sort_order を保存する。
	var child saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title": "メンバー", "slug": "members", "kind": "page", "parent_slug": "/research", "sort_order": 2,
	}, &child)
	if child.Article.ParentID != parentID || child.Article.ParentSlug != "research" || child.Article.SortOrder != 2 {
		t.Errorf("child row = %+v, want parent %s (research) and sort_order 2", child.Article, parentID)
	}
	if a := env.article(child.Article.ID); a.ParentID != parentID || a.SortOrder != 2 {
		t.Errorf("stored child ParentID = %q, SortOrder = %d", a.ParentID, a.SortOrder)
	}

	// parent_id でも指定できる。
	var child2 saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"title": "アクセス", "slug": "access", "kind": "page", "parent_id": parentID, "sort_order": 1,
	}, &child2)
	if child2.Article.ParentID != parentID || child2.Article.SortOrder != 1 {
		t.Errorf("child2 row = %+v", child2.Article)
	}

	// 親・並び順だけの変更は案にせず、そのまま保存する。省略した項目は今の値のまま。
	var updated saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": child.Article.ID, "sort_order": 5}, &updated)
	if updated.NeedsConfirmation || updated.Article.SortOrder != 5 || updated.Article.ParentID != parentID {
		t.Errorf("after sort_order update = %+v", updated)
	}
	// 空文字の parent_id で親から外し（最上位へ）、parent_slug で戻す。
	var detached saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": child.Article.ID, "parent_id": ""}, &detached)
	if a := env.article(child.Article.ID); a.ParentID != "" || a.SortOrder != 5 || detached.Article.ParentID != "" || detached.Article.ParentSlug != "" {
		t.Errorf("after detaching: ParentID %q SortOrder %d row %+v", a.ParentID, a.SortOrder, detached.Article)
	}
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": child.Article.ID, "parent_slug": "research"}, nil)
	if a := env.article(child.Article.ID); a.ParentID != parentID {
		t.Errorf("after reattaching: ParentID %q", a.ParentID)
	}

	// 本文の変更は案として預かる。親・並び順を変えないなら同時に指定してもよい。
	var proposed saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{
		"article_id": child.Article.ID, "content_markdown": "研究室のメンバー", "parent_id": parentID, "sort_order": 5,
	}, &proposed)
	if !proposed.NeedsConfirmation {
		t.Errorf("content change with unchanged hierarchy = %+v, want a proposal", proposed)
	}
}

func TestSaveDraftPageHierarchyErrors(t *testing.T) {
	env := newTestEnv(t, Config{})
	parentID := env.seed(article.Article{Title: "研究紹介", Slug: "research", AuthorID: writerA.ID, Kind: article.KindPage})
	childID := env.seed(article.Article{Title: "メンバー", Slug: "members", AuthorID: writerA.ID, Kind: article.KindPage, ParentID: parentID})
	postID := env.seed(article.Article{Title: "お知らせ", Slug: "news-1", AuthorID: writerA.ID})

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown parent_slug", map[string]any{"title": "x", "kind": "page", "parent_slug": "nope"}, `固定ページ "nope" が見つかりません`},
		{"parent_slug of a post", map[string]any{"title": "x", "kind": "page", "parent_slug": "news-1"}, `固定ページ "news-1" が見つかりません`},
		{"both parent_id and parent_slug", map[string]any{"title": "x", "kind": "page", "parent_id": parentID, "parent_slug": "research"}, `parent_id と parent_slug はどちらか一方`},
		{"post with parent (kind omitted)", map[string]any{"title": "x", "parent_id": parentID}, `固定ページ（kind=page）だけです`},
		{"post with parent", map[string]any{"title": "x", "kind": "post", "parent_slug": "research"}, `固定ページ（kind=page）だけです`},
		{"unknown parent_id", map[string]any{"title": "x", "kind": "page", "parent_id": "no-such-id"}, `親ページ "no-such-id" には指定できません`},
		{"parent_id of a post", map[string]any{"title": "x", "kind": "page", "parent_id": postID}, `親ページ "` + postID + `" には指定できません`},
		{"negative sort_order", map[string]any{"title": "x", "kind": "page", "sort_order": -1}, `sort_order は 0 以上`},
		{"self parent", map[string]any{"article_id": parentID, "parent_id": parentID}, `には指定できません`},
		{"cycle", map[string]any{"article_id": parentID, "parent_slug": "members"}, `親ページ "` + childID + `" には指定できません`},
		{"page with parent to post", map[string]any{"article_id": childID, "kind": "post"}, `固定ページ（kind=page）だけです`},
		{"page with children to post", map[string]any{"article_id": parentID, "kind": "post"}, `子ページのある固定ページは記事（post）に変えられません`},
		{"hierarchy with content change", map[string]any{"article_id": childID, "content_markdown": "新しい本文", "sort_order": 3}, `同時には指定できません`},
		{"parent change with title change", map[string]any{"article_id": childID, "title": "新しい題", "parent_id": ""}, `同時には指定できません`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectContains(t, env.toolErr(writerA.ID, "cms_save_draft", tc.args), tc.want)
		})
	}

	// 拒否した保存は記事を変えず、案も預けない。
	if a := env.article(parentID); a.ParentID != "" || a.Kind != article.KindPage {
		t.Errorf("parent after rejected saves: ParentID %q Kind %q", a.ParentID, a.Kind)
	}
	if a := env.article(childID); a.ParentID != parentID || a.SortOrder != 0 || a.ContentMarkdown != "" || a.Kind != article.KindPage {
		t.Errorf("child after rejected saves: %+v", a)
	}
	if p, err := LoadProposal(context.Background(), env.db, childID); err != nil || p != nil {
		t.Errorf("proposal after rejected saves = %+v, %v, want none", p, err)
	}

	// 親から外せば記事（post）に変えられる。
	var res saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": childID, "kind": "post", "parent_id": ""}, &res)
	if res.Article.Kind != article.KindPost || res.Article.ParentID != "" {
		t.Errorf("after converting to post = %+v", res.Article)
	}
}

func TestGetArticlePageHierarchy(t *testing.T) {
	env := newTestEnv(t, Config{})
	parentID := env.seed(article.Article{Title: "研究紹介", Slug: "research", AuthorID: writerA.ID, Kind: article.KindPage, Status: article.StatusPublished})
	childID := env.seed(article.Article{Title: "メンバー", Slug: "members", AuthorID: writerA.ID, Kind: article.KindPage,
		Status: article.StatusPublished, ParentID: parentID, SortOrder: 4})

	var detail articleDetail
	env.toolOK(editor.ID, "cms_get_article", map[string]any{"article_id": childID}, &detail)
	if detail.ParentID != parentID || detail.ParentSlug != "research" || detail.SortOrder != 4 {
		t.Errorf("child detail parent_id %q parent_slug %q sort_order %d", detail.ParentID, detail.ParentSlug, detail.SortOrder)
	}

	// 親の無いページでは parent_id・parent_slug を省き、sort_order は常に返す。
	raw := env.callTool(editor.ID, "cms_get_article", map[string]any{"slug": "research"})
	if raw.IsError || len(raw.Content) == 0 {
		t.Fatalf("cms_get_article(research) = %+v", raw)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw.Content[0].Text), &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := fields["parent_id"]; ok {
		t.Errorf("top-level page has parent_id: %v", fields["parent_id"])
	}
	if _, ok := fields["parent_slug"]; ok {
		t.Errorf("top-level page has parent_slug: %v", fields["parent_slug"])
	}
	if v, ok := fields["sort_order"]; !ok || v != float64(0) {
		t.Errorf("sort_order = %v (%v), want 0", v, ok)
	}
}

func TestListArticlesPageHierarchy(t *testing.T) {
	env := newTestEnv(t, Config{})
	parentID := env.seed(article.Article{Title: "研究紹介", Slug: "research", AuthorID: writerA.ID, Kind: article.KindPage, Status: article.StatusPublished})
	otherID := env.seed(article.Article{Title: "別の親", Slug: "other", AuthorID: writerA.ID, Kind: article.KindPage})
	// 子ページは sort_order、タイトルの順に並ぶ（更新日時の順ではない）。
	b := env.seed(article.Article{Title: "B", Slug: "b", AuthorID: writerA.ID, Kind: article.KindPage, ParentID: parentID, SortOrder: 2})
	z := env.seed(article.Article{Title: "Z", Slug: "z", AuthorID: writerA.ID, Kind: article.KindPage, ParentID: parentID, SortOrder: 1})
	a := env.seed(article.Article{Title: "A", Slug: "a", AuthorID: writerA.ID, Kind: article.KindPage, ParentID: parentID, SortOrder: 2, Status: article.StatusPublished})
	env.seed(article.Article{Title: "よその子", Slug: "other-child", AuthorID: writerA.ID, Kind: article.KindPage, ParentID: otherID})
	postID := env.seed(article.Article{Title: "お知らせ", Slug: "news-1", AuthorID: writerA.ID})

	for _, args := range []map[string]any{{"parent_id": parentID}, {"parent_slug": "research"}, {"parent_slug": "/research", "kind": "page"}} {
		var res listArticlesResult
		env.toolOK(editor.ID, "cms_list_articles", args, &res)
		if got, want := orderedIDs(res), []string{z, a, b}; !slices.Equal(got, want) || res.Total != 3 {
			t.Errorf("%v: ids = %v (total %d), want %v", args, got, res.Total, want)
		}
		for _, r := range res.Articles {
			if r.ParentID != parentID || r.ParentSlug != "research" {
				t.Errorf("%v: row %s parent = %q (%q)", args, r.ID, r.ParentID, r.ParentSlug)
			}
		}
	}

	// 他の条件と組み合わせられる。
	var published listArticlesResult
	env.toolOK(editor.ID, "cms_list_articles", map[string]any{"parent_slug": "research", "status": "published"}, &published)
	if got := orderedIDs(published); !slices.Equal(got, []string{a}) {
		t.Errorf("published children = %v, want [%s]", got, a)
	}

	// 絞り込まない一覧にも親・並び順を返す。
	var all listArticlesResult
	env.toolOK(editor.ID, "cms_list_articles", map[string]any{"limit": 100}, &all)
	rows := map[string]articleRow{}
	for _, r := range all.Articles {
		rows[r.ID] = r
	}
	if r := rows[z]; r.ParentID != parentID || r.ParentSlug != "research" || r.SortOrder != 1 {
		t.Errorf("row z = %+v", r)
	}
	if r := rows[postID]; r.ParentID != "" || r.ParentSlug != "" || r.SortOrder != 0 {
		t.Errorf("post row = %+v", r)
	}

	expectContains(t, env.toolErr(editor.ID, "cms_list_articles", map[string]any{"parent_slug": "nope"}), `固定ページ "nope" が見つかりません`)
	expectContains(t, env.toolErr(editor.ID, "cms_list_articles", map[string]any{"parent_slug": "news-1"}), `固定ページ "news-1" が見つかりません`)
	expectContains(t, env.toolErr(editor.ID, "cms_list_articles", map[string]any{"parent_id": parentID, "parent_slug": "research"}),
		`parent_id と parent_slug はどちらか一方`)
}
