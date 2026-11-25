package mcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
)

// 予約投稿（公開日時が未来の公開記事）は、MCP では状態 scheduled・公開予定日時 scheduled_at の記事として見せる
// （予約を状態で持っていたときと同じ形）。管理画面と同じく執筆チームには他人の予約投稿も見せるが、
// 公開日時を迎えるまで公開記事としては数えない（読者・サイトの情報には出さない）。
func TestScheduledArticlesInMCP(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)
	at := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	scheduled := env.seed(article.Article{Title: "鈴木の予約投稿", Slug: "suzuki-later", ContentMarkdown: "予約の本文",
		AuthorID: writerB.ID, CategoryID: categoryResearch, Status: article.StatusPublished, PublishedAt: &at})
	list := func(userID string, args map[string]any) listArticlesResult {
		t.Helper()
		var res listArticlesResult
		env.toolOK(userID, "cms_list_articles", args, &res)
		return res
	}

	res := list(editor.ID, map[string]any{"status": "scheduled"})
	if got := listIDs(res); !slices.Equal(got, []string{scheduled}) || res.Total != 1 {
		t.Fatalf("editor scheduled = %v (total %d), want [%s]", got, res.Total, scheduled)
	}
	if row := res.Articles[0]; row.Status != article.StatusScheduled || row.ScheduledAt == nil || !row.ScheduledAt.Equal(at) ||
		row.PublishedAt != nil {
		t.Errorf("scheduled row = %+v", row)
	}
	if got := listIDs(list(editor.ID, map[string]any{"status": "published"})); !slices.Equal(got, []string{lib.publishedB}) {
		t.Errorf("editor published = %v, want [%s]", got, lib.publishedB)
	}

	// 管理画面の「予約」タブと同じく、執筆者にも他人の予約投稿を見せる。本人にも見せる。
	if got := listIDs(list(writerA.ID, map[string]any{"status": "scheduled"})); !slices.Equal(got, []string{scheduled}) {
		t.Errorf("writer scheduled = %v, want [%s]", got, scheduled)
	}
	if got, want := listIDs(list(writerA.ID, nil)), sortedIDs(lib.draftA, lib.draftB, lib.publishedB, lib.reviewEditor, scheduled); !slices.Equal(got, want) {
		t.Errorf("writer list = %v, want %v", got, want)
	}
	var other articleDetail
	env.toolOK(writerA.ID, "cms_get_article", map[string]any{"article_id": scheduled}, &other)
	if other.Status != article.StatusScheduled || other.PublicPath != "" {
		t.Errorf("writer detail of another writer's scheduled article = %+v", other)
	}
	if got := listIDs(list(writerB.ID, map[string]any{"status": "scheduled"})); !slices.Equal(got, []string{scheduled}) {
		t.Errorf("author scheduled = %v, want [%s]", got, scheduled)
	}

	var d articleDetail
	env.toolOK(writerB.ID, "cms_get_article", map[string]any{"article_id": scheduled}, &d)
	if d.Status != article.StatusScheduled || d.ScheduledAt == nil || !d.ScheduledAt.Equal(at) || d.PublishedAt != nil || d.PublicPath != "" {
		t.Errorf("scheduled detail = %+v", d)
	}
	var read readResourceResult
	if err := json.Unmarshal(env.call(writerB.ID, "resources/read", map[string]any{"uri": "cms://articles/" + scheduled}), &read); err != nil {
		t.Fatal(err)
	}
	text := read.Contents[0].Text
	expectContains(t, text, "status: scheduled", "scheduled_at: ", at.Format(time.RFC3339))
	if strings.Contains(text, "published_at") {
		t.Errorf("frontmatter of scheduled article has published_at:\n%s", text)
	}

	// 予約投稿は下書きではないため MCP からは変更できない。
	expectContains(t, env.toolErr(writerB.ID, "cms_save_draft", map[string]any{"article_id": scheduled, "content_markdown": "書き換え"}),
		"現在: scheduled")

	// カテゴリの公開記事数・サイトの最近の記事には、公開日時を迎えるまで数えない。
	if err := json.Unmarshal(env.call(writerA.ID, "resources/read", map[string]any{"uri": categoriesURI}), &read); err != nil {
		t.Fatal(err)
	}
	var cats []categoryRow
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &cats); err != nil {
		t.Fatal(err)
	}
	for _, c := range cats {
		if c.Slug == "research" && c.PublishedArticles != 1 {
			t.Errorf("research published_articles = %d, want 1", c.PublishedArticles)
		}
	}
	var site struct {
		Recent []struct {
			ID string `json:"id"`
		} `json:"recent_articles"`
	}
	env.toolOK(writerA.ID, "cms_get_site_context", map[string]any{}, &site)
	for _, r := range site.Recent {
		if r.ID == scheduled {
			t.Error("site context lists the scheduled article as recent")
		}
	}

	var stored string
	if err := env.db.Get(&stored, `SELECT status FROM articles WHERE id = ?`, scheduled); err != nil || stored != string(article.StatusPublished) {
		t.Errorf("stored status = %q, %v; want published", stored, err)
	}
}
