package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/lock"
	"sorus/internal/scheduler"
)

// unpublishHook は OnArticleUnpublished の代わり。呼ばれた記事 ID を覚え、err を返す。
type unpublishHook struct {
	mu    sync.Mutex
	calls []string
	users []string
	err   error
}

func (h *unpublishHook) config(_ *testEnv, cfg *Config) {
	cfg.OnArticleUnpublished = func(_ context.Context, user *auth.User, id string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls = append(h.calls, id)
		h.users = append(h.users, user.ID)
		return h.err
	}
}

func (h *unpublishHook) called() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func (h *unpublishHook) callers() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.users)
}

func (h *unpublishHook) setErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

func articleComments(t *testing.T, env *testEnv, id string) []string {
	t.Helper()
	var out []string
	if err := env.db.Select(&out, `SELECT content FROM article_comments WHERE article_id = ? ORDER BY rowid`, id); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUnpublishPublishedArticle(t *testing.T) {
	h := &unpublishHook{}
	env := newTestEnv(t, Config{}, h.config)
	id := env.seed(article.Article{Title: "公開中", Slug: "live", ContentMarkdown: "本文", AuthorID: writerB.ID, Status: article.StatusPublished})
	publishedAt := env.article(id).PublishedAt

	// 執筆者は（自分の記事でも）公開中の記事を取り下げられない。
	expectContains(t, env.toolErr(writerB.ID, "cms_unpublish_article", map[string]any{"article_id": id}), "編集者・管理者だけ")
	if a := env.article(id); a.Status != article.StatusPublished {
		t.Fatalf("writer unpublished: %s", a.Status)
	}

	var res articleStateResult
	env.toolOK(editor.ID, "cms_unpublish_article", map[string]any{"article_id": id, "note": "誤りがあるため取り下げ"}, &res)
	if res.PreviousStatus != article.StatusPublished || res.Article.Status != article.StatusDraft || res.Warning != "" {
		t.Fatalf("result = %+v", res)
	}
	expectContains(t, res.Message, "公開を取り下げ")
	a := env.article(id)
	// 初回の公開日時は残す（同期 API の下書きに戻す操作と同じ）。
	if a.Status != article.StatusDraft || a.PublishedAt == nil || !a.PublishedAt.Equal(*publishedAt) {
		t.Errorf("article after unpublish: status %s published_at %v (was %v)", a.Status, a.PublishedAt, publishedAt)
	}
	if got := articleComments(t, env, id); !slices.Equal(got, []string{"【下書きに戻す】誤りがあるため取り下げ"}) {
		t.Errorf("comments = %v", got)
	}
	if got := revisionComments(t, env, id); len(got) == 0 || got[len(got)-1] != "下書きに戻す（MCP）" {
		t.Errorf("revisions = %v", got)
	}
	if !slices.Contains(env.changedIDs(), id) {
		t.Errorf("OnArticleChanged not called for %s", id)
	}
	if got := h.called(); !slices.Equal(got, []string{id}) || !slices.Equal(h.callers(), []string{editor.ID}) {
		t.Errorf("OnArticleUnpublished calls = %v users %v", got, h.callers())
	}
	if by := lockedBy(t, env, id); by != "" {
		t.Errorf("lock left by %q", by)
	}
	// もう一度呼んでも何も変わらない。
	expectContains(t, env.toolErr(editor.ID, "cms_unpublish_article", map[string]any{"article_id": id}), "すでに下書き")
	if got := h.called(); len(got) != 1 {
		t.Errorf("hook called again: %v", got)
	}
}

func TestUnpublishScheduledArticle(t *testing.T) {
	h := &unpublishHook{}
	env := newTestEnv(t, Config{}, h.config)
	at := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	id := env.seed(article.Article{Title: "予約", ContentMarkdown: "本文", AuthorID: writerA.ID, Status: article.StatusPublished, PublishedAt: &at})
	if err := scheduler.Track(context.Background(), env.db, time.Now(), id); err != nil {
		t.Fatal(err)
	}

	expectContains(t, env.toolErr(writerA.ID, "cms_unpublish_article", map[string]any{"article_id": id}), "編集者・管理者だけ")
	var res articleStateResult
	env.toolOK(admin.ID, "cms_unpublish_article", map[string]any{"article_id": id}, &res)
	if res.PreviousStatus != article.StatusScheduled || res.Article.Status != article.StatusDraft {
		t.Fatalf("result = %+v", res)
	}
	expectContains(t, res.Message, "予約を取り消し")
	// 未来の公開日時（予約）は消し、予約の記録からも外す。
	if a := env.article(id); a.Status != article.StatusDraft || a.PublishedAt != nil {
		t.Errorf("article after unschedule = %s %v", a.Status, a.PublishedAt)
	}
	tracked, err := scheduler.Scheduled(context.Background(), env.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tracked[id]; ok {
		t.Errorf("article still tracked as scheduled: %v", tracked)
	}
	if got := articleComments(t, env, id); !slices.Equal(got, []string{"【下書きに戻す】下書きに戻しました（MCP）"}) {
		t.Errorf("comments = %v", got)
	}
	if got := h.called(); !slices.Equal(got, []string{id}) {
		t.Errorf("OnArticleUnpublished calls = %v", got)
	}
}

func TestUnpublishReviewArticle(t *testing.T) {
	h := &unpublishHook{}
	env := newTestEnv(t, Config{}, h.config)
	own := env.seed(article.Article{Title: "山田の校閲待ち", AuthorID: writerA.ID, Status: article.StatusReview})
	other := env.seed(article.Article{Title: "鈴木の校閲待ち", AuthorID: writerB.ID, Status: article.StatusReview})

	// 作成者は自分の校閲待ちを取り下げられ、他の執筆者の記事は取り下げられない。
	expectContains(t, env.toolErr(writerA.ID, "cms_unpublish_article", map[string]any{"article_id": other}), "他の人の校閲待ち")
	var res articleStateResult
	env.toolOK(writerA.ID, "cms_unpublish_article", map[string]any{"article_id": own}, &res)
	if res.PreviousStatus != article.StatusReview || env.article(own).Status != article.StatusDraft {
		t.Fatalf("own review = %+v", res)
	}
	expectContains(t, res.Message, "校閲申請を取り下げ")
	env.toolOK(editor.ID, "cms_unpublish_article", map[string]any{"article_id": other}, &res)
	if env.article(other).Status != article.StatusDraft {
		t.Errorf("editor could not unpublish review")
	}
	// 読者に見えていなかった記事では、公開先の取り下げを呼ばない。
	if got := h.called(); len(got) != 0 {
		t.Errorf("OnArticleUnpublished called for review articles: %v", got)
	}
}

func TestUnpublishRejections(t *testing.T) {
	env := newTestEnv(t, Config{})
	lib := seedLibrary(env)
	expectContains(t, env.toolErr(editor.ID, "cms_unpublish_article", map[string]any{"article_id": lib.draftA}), "すでに下書き")
	expectContains(t, env.toolErr(editor.ID, "cms_unpublish_article", map[string]any{"article_id": lib.deletedA}), "ゴミ箱の記事")
	expectContains(t, env.toolErr(editor.ID, "cms_unpublish_article", map[string]any{"article_id": "missing"}), "見つからない")
	expectContains(t, env.toolErr(editor.ID, "cms_unpublish_article", map[string]any{}), "article_id を指定")
	expectContains(t, env.toolErr(editor.ID, "cms_unpublish_article", map[string]any{"article_id": lib.publishedB, "status": "draft"}), "引数が正しくありません")

	// 他の人が管理画面で編集中の記事は取り下げない。
	locker := lock.NewLocker(env.db, 0)
	if _, err := locker.Acquire(context.Background(), lib.publishedB, admin.ID); err != nil {
		t.Fatal(err)
	}
	expectContains(t, env.toolErr(editor.ID, "cms_unpublish_article", map[string]any{"article_id": lib.publishedB}), "管理画面で編集中")
	if a := env.article(lib.publishedB); a.Status != article.StatusPublished {
		t.Errorf("locked article changed: %s", a.Status)
	}
}

func TestUnpublishHookFailureBecomesWarning(t *testing.T) {
	h := &unpublishHook{err: errors.New("dial tcp: connection refused")}
	env := newTestEnv(t, Config{}, h.config)
	id := env.seed(article.Article{Title: "公開中", AuthorID: writerA.ID, Status: article.StatusPublished})

	// 取り下げ自体は済ませ、公開先の失敗は内部の詳細を出さない warning で伝える。
	var res articleStateResult
	env.toolOK(editor.ID, "cms_unpublish_article", map[string]any{"article_id": id}, &res)
	if res.Article.Status != article.StatusDraft || res.Warning == "" {
		t.Fatalf("result = %+v", res)
	}
	expectContains(t, res.Warning, "公開サーバーから記事を取り下げられませんでした")
	if strings.Contains(res.Warning, "dial") || strings.Contains(res.Warning, "refused") {
		t.Errorf("internal error leaked: %q", res.Warning)
	}

	h.setErr(NewToolError("公開サーバーのトークンの期限が切れています"))
	id2 := env.seed(article.Article{Title: "公開中2", AuthorID: writerA.ID, Status: article.StatusPublished})
	env.toolOK(editor.ID, "cms_unpublish_article", map[string]any{"article_id": id2}, &res)
	if res.Warning != "公開サーバーのトークンの期限が切れています" {
		t.Errorf("warning = %q", res.Warning)
	}
}

func TestDeleteArticle(t *testing.T) {
	env := newTestEnv(t, Config{})
	ownDraft := env.seed(article.Article{Title: "山田の下書き", AuthorID: writerA.ID, Status: article.StatusDraft})
	otherDraft := env.seed(article.Article{Title: "鈴木の下書き", AuthorID: writerB.ID, Status: article.StatusDraft})
	ownReview := env.seed(article.Article{Title: "山田の校閲待ち", AuthorID: writerA.ID, Status: article.StatusReview})

	// 執筆者は自分の下書きだけを移せる。
	var res articleStateResult
	env.toolOK(writerA.ID, "cms_delete_article", map[string]any{"article_id": ownDraft, "note": "重複のため"}, &res)
	if res.PreviousStatus != article.StatusDraft || res.Article.Status != article.StatusDeleted {
		t.Fatalf("result = %+v", res)
	}
	expectContains(t, res.Message, "ゴミ箱へ移しました", "30日後", "管理画面のゴミ箱")
	if a := env.article(ownDraft); a.Status != article.StatusDeleted {
		t.Errorf("status = %s, want deleted", a.Status)
	}
	if got := articleComments(t, env, ownDraft); !slices.Equal(got, []string{"【ゴミ箱へ移動】重複のため"}) {
		t.Errorf("comments = %v", got)
	}
	if !slices.Contains(env.changedIDs(), ownDraft) {
		t.Errorf("OnArticleChanged not called for %s", ownDraft)
	}
	if by := lockedBy(t, env, ownDraft); by != "" {
		t.Errorf("lock left by %q", by)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_delete_article", map[string]any{"article_id": ownDraft}), "すでにゴミ箱")
	expectContains(t, env.toolErr(writerA.ID, "cms_delete_article", map[string]any{"article_id": otherDraft}), "他の人の下書き")
	expectContains(t, env.toolErr(writerA.ID, "cms_delete_article", map[string]any{"article_id": ownReview}), "編集者・管理者だけ")
	for _, id := range []string{otherDraft, ownReview} {
		if a := env.article(id); a.Status == article.StatusDeleted {
			t.Errorf("%s deleted by writer", id)
		}
	}

	// 編集者・管理者は他の人の下書き・校閲待ちも移せる。
	env.toolOK(editor.ID, "cms_delete_article", map[string]any{"article_id": otherDraft}, &res)
	env.toolOK(admin.ID, "cms_delete_article", map[string]any{"article_id": ownReview}, &res)
	for _, id := range []string{otherDraft, ownReview} {
		if a := env.article(id); a.Status != article.StatusDeleted {
			t.Errorf("%s status = %s, want deleted", id, a.Status)
		}
	}
	// 物理削除はしない（ゴミ箱から元に戻せる）。
	if err := article.NewService(env.db).RestoreFromTrash(ownDraft); err != nil {
		t.Errorf("restore from trash: %v", err)
	}
}

func TestDeleteArticleRejectsPublicArticles(t *testing.T) {
	env := newTestEnv(t, Config{})
	published := env.seed(article.Article{Title: "公開中", AuthorID: editor.ID, Status: article.StatusPublished})
	at := time.Now().Add(time.Hour).UTC()
	scheduled := env.seed(article.Article{Title: "予約", AuthorID: editor.ID, Status: article.StatusPublished, PublishedAt: &at})

	// 公開中・予約中の記事は編集者・管理者でも直接は移せず、先に下書きに戻してもらう。
	for _, id := range []string{published, scheduled} {
		for _, user := range []string{editor.ID, admin.ID} {
			expectContains(t, env.toolErr(user, "cms_delete_article", map[string]any{"article_id": id}), "cms_unpublish_article")
		}
		if a := env.article(id); a.Status != article.StatusPublished {
			t.Errorf("%s status = %s, want published", id, a.Status)
		}
	}
	expectContains(t, env.toolErr(editor.ID, "cms_delete_article", map[string]any{"article_id": "missing"}), "見つからない")

	// 下書きに戻してからなら移せる。
	env.toolOK(editor.ID, "cms_unpublish_article", map[string]any{"article_id": published}, nil)
	env.toolOK(editor.ID, "cms_delete_article", map[string]any{"article_id": published}, nil)
	if a := env.article(published); a.Status != article.StatusDeleted {
		t.Errorf("status after unpublish + delete = %s", a.Status)
	}
}

func TestDeleteArticleRespectsAdminLockAndPublicOnly(t *testing.T) {
	env := newTestEnv(t, Config{PublicOnly: true})
	id := env.seed(article.Article{Title: "下書き", AuthorID: writerA.ID, Status: article.StatusDraft})
	locker := lock.NewLocker(env.db, 0)
	if _, err := locker.Acquire(context.Background(), id, editor.ID); err != nil {
		t.Fatal(err)
	}
	expectContains(t, env.toolErr(writerA.ID, "cms_delete_article", map[string]any{"article_id": id}), "管理画面で編集中")
	if err := locker.Release(context.Background(), id, editor.ID); err != nil {
		t.Fatal(err)
	}
	// 公開サーバーには管理画面が無いため、ゴミ箱から戻す案内をしない。
	var res articleStateResult
	env.toolOK(writerA.ID, "cms_delete_article", map[string]any{"article_id": id}, &res)
	if strings.Contains(res.Message, "管理画面") {
		t.Errorf("public-only message mentions the admin screen: %q", res.Message)
	}
}
