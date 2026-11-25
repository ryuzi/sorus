package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
)

const checkedOutMessage = "山田さんが PC で編集中のため、そちらの作業が終わるまで変更できません"

// checkout は記事 id に、at から 山田 さんが PC で編集中の印を付ける。
func (e *testEnv) checkout(id string, at time.Time) {
	e.t.Helper()
	if err := article.NewService(e.db).SetCheckout(context.Background(), id, "山田", "", at); err != nil {
		e.t.Fatalf("SetCheckout(%s): %v", id, err)
	}
}

func TestSaveDraftRejectsCheckedOutArticle(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})
	env.checkout(id, time.Now().Add(-time.Hour))

	// 文章の変更（変更案）も、設定だけの変更も断る。
	for _, args := range []map[string]any{
		{"article_id": id, "content_markdown": "書き直した本文"},
		{"article_id": id, "slug": "changed"},
	} {
		expectContains(t, env.toolErr(writerB.ID, "cms_save_draft", args), checkedOutMessage)
	}
	a := env.article(id)
	if a.ContentMarkdown != "本文" || a.Slug == "changed" {
		t.Errorf("article changed while checked out: %+v", a)
	}
	if p, err := LoadProposal(context.Background(), env.db, id); err != nil || p != nil {
		t.Errorf("proposal staged while checked out: %+v (%v)", p, err)
	}

	// 24時間以上前の印は無視して保存できる。
	env.checkout(id, time.Now().Add(-25*time.Hour))
	var res saveDraftResult
	env.toolOK(writerB.ID, "cms_save_draft", map[string]any{"article_id": id, "slug": "changed"}, &res)
	if res.Article.Slug != "changed" {
		t.Errorf("save after stale checkout = %+v", res.Article)
	}

	// 印を外せば保存できる。
	env.checkout(id, time.Now())
	expectContains(t, env.toolErr(writerB.ID, "cms_save_draft", map[string]any{"article_id": id, "slug": "again"}), checkedOutMessage)
	if err := article.NewService(env.db).ClearCheckout(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	env.toolOK(writerB.ID, "cms_save_draft", map[string]any{"article_id": id, "slug": "again"}, &res)
	if res.Article.Slug != "again" {
		t.Errorf("save after clear = %+v", res.Article)
	}
}

func TestSubmitForReviewRejectsCheckedOutArticle(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})
	env.checkout(id, time.Now().Add(-time.Minute))
	expectContains(t, env.toolErr(writerA.ID, "cms_submit_for_review", map[string]any{"article_id": id}), checkedOutMessage)
	if a := env.article(id); a.Status != article.StatusDraft {
		t.Errorf("status = %s, want draft", a.Status)
	}
	if by := lockedBy(t, env, id); by != "" {
		t.Errorf("lock left by %q", by)
	}
}

func TestApproveAndPublishRejectsCheckedOutArticle(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "校閲待ち", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
	env.checkout(id, time.Now().Add(-time.Minute))
	expectContains(t, env.toolErr(editor.ID, "cms_approve_and_publish", map[string]any{"article_id": id}), checkedOutMessage)
	if a := env.article(id); a.Status != article.StatusReview {
		t.Errorf("status = %s, want review", a.Status)
	}
}

func TestPublicOnlyOmitsAdminLinks(t *testing.T) {
	env := newTestEnv(t, Config{PublicOnly: true, BaseURL: "https://example.jp"})
	id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})

	// 変更案: confirm_url・edit_url を返さず、PC の sorus の「届いた原稿」へ案内する。
	res := env.callTool(writerA.ID, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "書き直した本文"})
	if res.IsError {
		t.Fatalf("propose: %s", res.Content[0].Text)
	}
	raw := string(res.StructuredContent)
	for _, key := range []string{"confirm_url", "confirm_path", "edit_url", "edit_path", "/admin"} {
		if strings.Contains(raw, key) {
			t.Errorf("public-only proposal contains %q: %s", key, raw)
		}
	}
	var proposal saveDraftResult
	if err := json.Unmarshal(res.StructuredContent, &proposal); err != nil {
		t.Fatal(err)
	}
	if !proposal.NeedsConfirmation || proposal.Proposal == nil {
		t.Fatalf("proposal = %+v", proposal)
	}
	expectContains(t, proposal.Message, "PC の sorus の「届いた原稿」")
	if strings.Contains(proposal.Message, "confirm_url") {
		t.Errorf("message mentions confirm_url: %q", proposal.Message)
	}

	// 新規作成・記事の取得でも編集画面の URL を返さない。
	created := env.callTool(writerA.ID, "cms_save_draft", map[string]any{"title": "新規"})
	if created.IsError || strings.Contains(string(created.StructuredContent), "/admin") {
		t.Errorf("public-only create = %s", created.StructuredContent)
	}
	got := env.callTool(writerA.ID, "cms_get_article", map[string]any{"article_id": id})
	if got.IsError || strings.Contains(string(got.StructuredContent), "/admin") || strings.Contains(string(got.StructuredContent), "confirm_url") {
		t.Errorf("public-only get_article = %s", got.StructuredContent)
	}

	// initialize の使い方も「届いた原稿」へ案内する。
	var init initializeResult
	if err := json.Unmarshal(env.call(writerA.ID, "initialize", map[string]any{"protocolVersion": LatestProtocolVersion}), &init); err != nil {
		t.Fatal(err)
	}
	expectContains(t, init.Instructions, "「届いた原稿」")
	if strings.Contains(init.Instructions, "confirm_url") {
		t.Errorf("instructions mention confirm_url: %q", init.Instructions)
	}
}

func TestDefaultModeKeepsAdminLinks(t *testing.T) {
	env := newTestEnv(t, Config{BaseURL: "https://example.jp"})
	id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})
	var res saveDraftResult
	env.toolOK(writerA.ID, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "書き直した本文"}, &res)
	wantPath := "/admin/articles/" + id + "/edit"
	if res.EditPath != wantPath || res.EditURL != "https://example.jp"+wantPath || res.Proposal == nil ||
		res.Proposal.ConfirmURL != "https://example.jp"+wantPath+proposalAnchor || res.Proposal.ConfirmPath != wantPath+proposalAnchor {
		t.Errorf("default proposal links = %+v / %+v", res, res.Proposal)
	}
	expectContains(t, res.Message, "confirm_url")
}

// PC の sorus で編集中の翻訳の記事には、cms_create_translation・cms_sync_translation とも AI を呼ぶ前に断り、案も預からない。
func TestTranslationToolsRejectCheckedOutArticle(t *testing.T) {
	f := &fakeAI{}
	env := newTestEnv(t, Config{AI: f})
	jaID, enID := seedTranslationPair(env)
	mustExec(t, env.db, `UPDATE articles SET is_outdated = 1 WHERE id = ?`, enID)
	env.checkout(enID, time.Now().Add(-time.Minute))
	before := env.article(enID)

	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"cms_create_translation", map[string]any{"source_article_id": jaID, "target_lang": "en"}},
		{"cms_sync_translation", map[string]any{"article_id": enID}},
	} {
		f.last.op = ""
		expectContains(t, env.toolErr(writerB.ID, c.tool, c.args), checkedOutMessage)
		if f.last.op != "" {
			t.Errorf("%s: AI called while checked out (op=%q)", c.tool, f.last.op)
		}
		after := env.article(enID)
		if after.Title != before.Title || after.ContentMarkdown != before.ContentMarkdown || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("%s: article changed while checked out: %+v", c.tool, after)
		}
		if p, err := LoadProposal(context.Background(), env.db, enID); err != nil || p != nil {
			t.Errorf("%s: proposal staged while checked out: %+v (%v)", c.tool, p, err)
		}
	}
}

// AI の翻訳を待つ間に PC の sorus で編集中になったら、翻訳の案を預けない。
func TestTranslationToolsRejectCheckoutDuringTranslation(t *testing.T) {
	for _, c := range []struct {
		tool string
		args func(jaID, enID string) map[string]any
	}{
		{"cms_create_translation", func(jaID, _ string) map[string]any {
			return map[string]any{"source_article_id": jaID, "target_lang": "en"}
		}},
		{"cms_sync_translation", func(_, enID string) map[string]any { return map[string]any{"article_id": enID} }},
	} {
		t.Run(c.tool, func(t *testing.T) {
			h := &hookAI{}
			env := newTestEnv(t, Config{AI: h})
			jaID, enID := seedTranslationPair(env)
			mustExec(t, env.db, `UPDATE articles SET is_outdated = 1 WHERE id = ?`, enID)
			h.onTranslate = func() {
				h.onTranslate = nil
				env.checkout(enID, time.Now())
			}
			expectContains(t, env.toolErr(writerB.ID, c.tool, c.args(jaID, enID)), checkedOutMessage)
			if p, err := LoadProposal(context.Background(), env.db, enID); err != nil || p != nil {
				t.Errorf("proposal staged after checkout: %+v (%v)", p, err)
			}
		})
	}
}

// 記事・翻訳を変える MCP の道具はどれも、PC の sorus で編集中（チェックアウト中）の記事を同じ文言で断り、
// 記事を変えず、変更案もロックも残さず、AI も呼ばない。印を外せば同じ呼び出しが通る（断ったのが印のためであることを確かめる）。
func TestEveryMutatingToolRejectsCheckedOutArticle(t *testing.T) {
	for _, c := range []struct {
		name string
		user string
		tool string
		// setup は対象の記事を用意し、その ID と道具の引数を返す。
		setup func(env *testEnv) (id string, args map[string]any)
	}{
		{"save_draft content (proposal)", writerB.ID, "cms_save_draft", func(env *testEnv) (string, map[string]any) {
			id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})
			return id, map[string]any{"article_id": id, "content_markdown": "書き直した本文"}
		}},
		{"save_draft settings only", writerB.ID, "cms_save_draft", func(env *testEnv) (string, map[string]any) {
			id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})
			return id, map[string]any{"article_id": id, "slug": "changed"}
		}},
		{"submit_for_review", writerA.ID, "cms_submit_for_review", func(env *testEnv) (string, map[string]any) {
			id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})
			return id, map[string]any{"article_id": id}
		}},
		{"approve_and_publish now", editor.ID, "cms_approve_and_publish", func(env *testEnv) (string, map[string]any) {
			id := env.seed(article.Article{Title: "校閲待ち", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
			return id, map[string]any{"article_id": id}
		}},
		{"approve_and_publish scheduled", editor.ID, "cms_approve_and_publish", func(env *testEnv) (string, map[string]any) {
			id := env.seed(article.Article{Title: "校閲待ち", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusReview})
			return id, map[string]any{"article_id": id, "publish_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
		}},
		{"create_translation on existing", writerB.ID, "cms_create_translation", func(env *testEnv) (string, map[string]any) {
			jaID, enID := seedTranslationPair(env)
			return enID, map[string]any{"source_article_id": jaID, "target_lang": "en"}
		}},
		{"sync_translation draft", writerB.ID, "cms_sync_translation", func(env *testEnv) (string, map[string]any) {
			_, enID := seedTranslationPair(env)
			mustExec(t, env.db, `UPDATE articles SET is_outdated = 1 WHERE id = ?`, enID)
			return enID, map[string]any{"article_id": enID}
		}},
		{"sync_translation published with force", editor.ID, "cms_sync_translation", func(env *testEnv) (string, map[string]any) {
			_, enID := seedTranslationPair(env)
			mustExec(t, env.db, `UPDATE articles SET status = ?, published_at = ?, is_outdated = 1 WHERE id = ?`,
				article.StatusPublished, time.Now().Add(-time.Hour).UTC(), enID)
			return enID, map[string]any{"article_id": enID, "force": true}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAI{}
			env := newTestEnv(t, Config{AI: f})
			id, args := c.setup(env)
			env.checkout(id, time.Now().Add(-time.Minute))
			before := env.article(id)

			expectContains(t, env.toolErr(c.user, c.tool, args), checkedOutMessage)
			if f.last.op != "" {
				t.Errorf("AI called while checked out (op=%q)", f.last.op)
			}
			after := env.article(id)
			if after.Status != before.Status || after.Title != before.Title || after.ContentMarkdown != before.ContentMarkdown ||
				after.Slug != before.Slug || !after.UpdatedAt.Equal(before.UpdatedAt) || !timePtrEqual(after.PublishedAt, before.PublishedAt) {
				t.Errorf("article changed while checked out:\nbefore %+v\nafter  %+v", before, after)
			}
			if p, err := LoadProposal(context.Background(), env.db, id); err != nil || p != nil {
				t.Errorf("proposal staged while checked out: %+v (%v)", p, err)
			}
			if by := lockedBy(t, env, id); by != "" {
				t.Errorf("lock left by %q", by)
			}

			// 印を外せば同じ呼び出しが通る。
			if err := article.NewService(env.db).ClearCheckout(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if res := env.callTool(c.user, c.tool, args); res.IsError {
				t.Errorf("after clearing checkout: %s", res.Content[0].Text)
			}
		})
	}
}

// ロックを獲得するまでに PC で編集が始まったら（手元に読んだ記事には印が無くても）、印だけを読み直して断り、
// 獲得したロックは戻す。
func TestGuardWriteRechecksCheckoutAfterLock(t *testing.T) {
	env := newTestEnv(t, Config{})
	id := env.seed(article.Article{Title: "下書き", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusDraft})
	stale := env.article(id) // 印を付ける前に読んだ記事
	env.checkout(id, time.Now())
	user := &auth.User{ID: writerA.ID, Role: auth.RoleWriter}

	_, err := env.server.guardWrite(context.Background(), user, stale, checkEditable)
	var te *toolError
	if !errors.As(err, &te) {
		t.Fatalf("guardWrite err = %v, want toolError", err)
	}
	expectContains(t, err.Error(), checkedOutMessage)
	if by := lockedBy(t, env, id); by != "" {
		t.Errorf("lock left by %q", by)
	}

	// 24時間以上前の印は、読み直しでも編集中としない。
	env.checkout(id, time.Now().Add(-25*time.Hour))
	release, err := env.server.guardWrite(context.Background(), user, stale, checkEditable)
	if err != nil {
		t.Fatalf("guardWrite after stale checkout: %v", err)
	}
	if by := lockedBy(t, env, id); by != writerA.ID {
		t.Errorf("locked_by = %q, want %q", by, writerA.ID)
	}
	release(false)
	if by := lockedBy(t, env, id); by != "" {
		t.Errorf("lock left by %q after release", by)
	}
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
