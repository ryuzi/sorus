package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// 同期 API のパスの先頭部分（/hooks/sync の hooks）は、
// 管理画面・同期 API だけでなく MCP の保存と、承認・同期での公開でもページのスラッグに使えないことを確かめる。

// mcpSaveDraft は MCP の cms_save_draft を呼び、ツールがエラーを返したかとその結果を返す。
func (e *testEnv) mcpSaveDraft(token string, args map[string]any) (isError bool, result string) {
	e.t.Helper()
	_, out := e.mcpCall(token, "tools/call", map[string]any{"name": "cms_save_draft", "arguments": args})
	if out["error"] != nil {
		e.t.Fatalf("cms_save_draft: JSON-RPC error %v", out["error"])
	}
	result = fmt.Sprint(out["result"])
	return strings.Contains(result, "isError:true"), result
}

func TestSyncPathPrefixIsReservedOnEveryWritePath(t *testing.T) {
	env := newSyncEnvWith(t, Config{SyncAPIPath: "/hooks/sync"}, nil)
	token := issueMCPToken(env.loggedIn(studentA), "エージェント", "30")
	// 設定を変える前に保存したページ（スラッグ hooks・校閲待ち）は、下の承認・同期のどちらでも公開しない。
	id := seedArticle(t, env.db, article.Article{Title: "連携", Slug: "hooks", Kind: article.KindPage,
		ContentMarkdown: "本文", Status: article.StatusReview})

	t.Run("MCP save draft", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"title": "Hooks", "kind": "page"},
			{"title": "連携", "slug": "hooks", "kind": "page"},
			{"title": "連携", "slug": "HOOKS", "kind": "page"},
		} {
			isErr, res := env.mcpSaveDraft(token, args)
			if !isErr || !strings.Contains(res, "hooks") || !strings.Contains(res, "固定ページには使えません") {
				t.Errorf("cms_save_draft(%v) = %s, want a reserved slug error", args, res)
			}
		}
		if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE kind = 'page' AND id != ?`, id); n != 0 {
			t.Errorf("%d pages were saved with the sync path prefix", n)
		}
		// 記事（/articles/<slug>）のスラッグとしては使える。
		if isErr, res := env.mcpSaveDraft(token, map[string]any{"title": "連携の記事", "slug": "hooks"}); isErr {
			t.Errorf("cms_save_draft(post hooks) = %s", res)
		}
	})

	t.Run("approve publish", func(t *testing.T) {
		editor := env.loggedIn(chief)
		res := editor.postForm("/admin/articles/"+id+"/approve-publish", url.Values{})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "スラッグ「hooks」はページには使えません")
		// 予約公開も公開と同じく断る（公開予定日時にスケジューラーが公開してしまうため）。
		res = editor.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"publish_at": {"2099-01-01T09:00"}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		if a := env.mustGetArticle(id); a.Status != article.StatusReview || a.PublishedAt != nil {
			t.Errorf("after rejected approval: status %s published %v", a.Status, a.PublishedAt)
		}
	})

	t.Run("sync publish", func(t *testing.T) {
		rec := env.call(env.token("sato", auth.RoleEditor), publishRequest(id))
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
		if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "スラッグ「hooks」はページには使えません") {
			t.Errorf("message = %q", msg)
		}
		if a := env.mustGetArticle(id); a.Status != article.StatusReview || a.PublishedAt != nil {
			t.Errorf("after rejected sync publish: status %s published %v", a.Status, a.PublishedAt)
		}
	})
}

// 既定の同期 API のパス（/api/sync）では hooks は予約語ではなく、これまでどおり保存・公開できる。
func TestSyncPathPrefixDefaultConfigAllowsHooks(t *testing.T) {
	env := newSyncEnv(t)
	token := issueMCPToken(env.loggedIn(studentA), "エージェント", "30")
	if isErr, res := env.mcpSaveDraft(token, map[string]any{"title": "連携", "slug": "hooks", "kind": "page"}); isErr {
		t.Fatalf("cms_save_draft(page hooks) = %s", res)
	}
	if isErr, _ := env.mcpSaveDraft(token, map[string]any{"title": "API", "slug": "api", "kind": "page"}); !isErr {
		t.Error("cms_save_draft(page api) succeeded, want a reserved slug error")
	}

	approved := seedArticle(t, env.db, article.Article{Title: "連携（承認）", Slug: "hooks-approved", Kind: article.KindPage,
		Status: article.StatusReview})
	expectRedirect(t, env.loggedIn(chief).postForm("/admin/articles/"+approved+"/approve-publish", url.Values{}),
		"/admin/articles/"+approved+"/edit")
	synced := seedArticle(t, env.db, article.Article{Title: "連携（同期）", Slug: "hooks", Kind: article.KindPage,
		Status: article.StatusReview})
	expectSyncStatus(t, env.call(env.token("sato", auth.RoleEditor), publishRequest(synced)), http.StatusOK)
	for _, id := range []string{approved, synced} {
		if a := env.mustGetArticle(id); a.Status != article.StatusPublished {
			t.Errorf("%s: status %s, want published", a.Slug, a.Status)
		}
	}
}
