package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/rag"
	"sorus/internal/scheduler"
)

// seedScheduled は公開日時 at の予約投稿（状態は公開）を用意して集合に記録し、ID を返す。承認した時点（公開日時より前）に
// 保存した予約投稿にするため、更新日時を at の1日前にする（at が過去なら、sorus を閉じている間に公開日時を迎えた記事になる）。
func seedScheduled(t *testing.T, db *sqlx.DB, a article.Article, at time.Time) string {
	t.Helper()
	a.Status, a.PublishedAt = article.StatusPublished, &at
	id := seedArticle(t, db, a)
	if _, err := db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, at.UTC().Add(-24*time.Hour), id); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Track(context.Background(), db, at.UTC().Add(-24*time.Hour), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// setClock は読者に見せるかを決める時計（clockNow）を now に固定し、試験の終わりに戻す。
func setClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := clockNow
	clockNow = func() time.Time { return now }
	t.Cleanup(func() { clockNow = prev })
}

// expectNoStoredScheduled は、状態 scheduled のまま保存した記事が無いことを確かめる（予約投稿は公開＋未来の公開日時）。
func (e *testEnv) expectNoStoredScheduled(step string) {
	e.t.Helper()
	if n := e.countRows(`SELECT COUNT(*) FROM articles WHERE status = ?`, article.StatusScheduled); n != 0 {
		e.t.Fatalf("%s: %d article(s) stored with status scheduled", step, n)
	}
}

// 予約投稿は読者向けのどの出口（記事・ページ・一覧・カテゴリ・RSS・サイトマップ・llms.txt・添付資料・告知文・静的書き出し）
// にも公開日時を迎えるまで出ず、迎えたら記事を保存し直さなくても出る。
func TestReadersShowScheduledArticleFromPublishTime(t *testing.T) {
	env := newPublicEnv(t)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var circle int64
	if err := env.db.Get(&circle, `SELECT id FROM categories WHERE slug = 'circle'`); err != nil {
		t.Fatal(err)
	}
	postID := seedScheduled(t, env.db, article.Article{Title: "予約した特集", Slug: "future-feature", CategoryID: circle,
		Excerpt: "予約した特集の抜粋", ContentMarkdown: "予約した特集の本文"}, at)
	seedScheduled(t, env.db, article.Article{Kind: article.KindPage, Title: "予約した案内ページ", Slug: "future-page",
		ContentMarkdown: "案内ページの本文"}, at)
	att := env.loggedIn(chief).mustAttach(postID, "資料.csv", []byte("a,b\n"))
	var attFile string
	if err := env.db.Get(&attFile, `SELECT file_path FROM attachments WHERE id = ?`, att.ID); err != nil {
		t.Fatal(err)
	}
	stored := env.mustGetArticle(postID)
	reader := env.newClient()
	// 他の執筆者の告知文にも、読者に見えるまで記事の URL は付かない。
	otherWriter := env.loggedIn(studentB)

	shown := func() map[string]bool {
		t.Helper()
		got := map[string]bool{}
		got["article page"] = reader.get("/articles/future-feature").StatusCode == http.StatusOK
		got["page"] = reader.get("/future-page").StatusCode == http.StatusOK
		got["top"] = strings.Contains(reader.get("/").Body, "予約した特集")
		got["list"] = strings.Contains(reader.get(articlesListPath).Body, "予約した特集")
		got["category"] = strings.Contains(reader.get("/categories/circle").Body, "予約した特集")
		got["rss"] = strings.Contains(reader.get("/feed.xml").Body, "予約した特集")
		sitemap := reader.get("/sitemap.xml").Body
		got["sitemap article"] = strings.Contains(sitemap, testBaseURL+"/articles/future-feature")
		got["sitemap page"] = strings.Contains(sitemap, testBaseURL+"/future-page")
		got["llms.txt"] = strings.Contains(reader.get("/llms.txt").Body, "予約した特集")
		got["llms-full.txt"] = strings.Contains(reader.get("/llms-full.txt").Body, "予約した特集の本文")
		got["attachment"] = reader.get(att.URL).StatusCode == http.StatusOK
		res, out := postSocial(otherWriter, url.Values{"article_id": {postID}})
		got["social"] = res.StatusCode == http.StatusOK && out.URL == testBaseURL+"/articles/future-feature"
		dir, _, err := env.server.ExportToTemp(context.Background(), ExportOptions{})
		if err != nil {
			t.Fatalf("ExportToTemp: %v", err)
		}
		defer os.RemoveAll(dir)
		for name, f := range map[string]string{"export article": "articles/future-feature/index.html", "export page": "future-page/index.html",
			"export attachment": "attachments/" + attFile} {
			_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f)))
			got[name] = err == nil
		}
		return got
	}

	setClock(t, at.Add(-time.Minute))
	for name, ok := range shown() {
		if ok {
			t.Errorf("before publish time: shown in %s", name)
		}
	}
	// 執筆者本人は告知文を作れるが、まだ読者に見えないため URL は付かない。
	res, out := postSocial(env.loggedIn(studentA), url.Values{"article_id": {postID}})
	expectStatus(t, res, http.StatusOK)
	if out.URL != "" || out.OGImage != "" {
		t.Errorf("social post of scheduled article: url=%q og=%q", out.URL, out.OGImage)
	}

	setClock(t, at.Add(time.Minute))
	got := shown()
	for name, ok := range got {
		if !ok {
			t.Errorf("after publish time: not shown in %s", name)
		}
	}
	if len(got) != 15 {
		t.Errorf("checked %d readers: %v", len(got), got)
	}
	// 公開日時を迎えても記事は保存し直さない（状態・更新日時はそのまま）。
	if a := env.mustGetArticle(postID); a.Status != article.StatusPublished || !a.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Errorf("article changed at publish time: status=%s updated_at=%v, want %v", a.Status, a.UpdatedAt, stored.UpdatedAt)
	}
	env.expectNoStoredScheduled("readers")
}

// 関連記事欄は、関連する記事が公開日時を迎えるまでその記事を出さない。
func TestRelatedArticlesWaitForPublishTime(t *testing.T) {
	env := newTestEnv(t)
	env.server.ragService = rag.NewService(env.db, nil)
	targetID := seedArticle(t, env.db, article.Article{Title: "研究の対象", Slug: "rag-target", ContentMarkdown: "対象の本文。",
		Status: article.StatusPublished, PublishedAt: ago(3 * time.Hour)})
	relatedAt := *ago(time.Hour)
	relatedID := seedScheduled(t, env.db, article.Article{Title: "あとから公開する関連記事", Slug: "rag-later",
		ContentMarkdown: "近いテーマの本文。"}, relatedAt)
	for _, id := range []string{targetID, relatedID} {
		setEmbedding(t, env.db, id, []float32{1, 0})
	}
	reader := env.newClient()

	setClock(t, relatedAt.Add(-time.Minute))
	res := reader.get("/articles/rag-target")
	expectStatus(t, res, http.StatusOK)
	if got := relatedTitles(t, res.Body); slices.Contains(got, "あとから公開する関連記事") {
		t.Errorf("before publish time: related = %q", got)
	}

	setClock(t, relatedAt.Add(time.Minute))
	res = reader.get("/articles/rag-target")
	expectStatus(t, res, http.StatusOK)
	if got := relatedTitles(t, res.Body); !slices.Contains(got, "あとから公開する関連記事") {
		t.Errorf("after publish time: related = %q", got)
	}
}

// dashboardCount はダッシュボードの「記事の状況」で label の欄に出ている件数を返す。
func dashboardCount(t *testing.T, body, label string) string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(label) + `</p><p class="mt-1 text-xl font-bold tabular-nums">(\d+)</p>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("dashboard has no %s count\nbody: %.3000s", label, body)
	}
	return m[1]
}

// 管理画面の「予約投稿」タブとダッシュボードの件数は、公開日時が未来の公開記事を予約投稿として扱い、公開日時を迎えたら
// 公開中として扱う。予約中の記事を変更・ゴミ箱へ移せるのは編集者だけ。
func TestAdminListsAndCountsScheduledArticles(t *testing.T) {
	env := newTestEnv(t)
	at := time.Now().UTC().Add(2 * time.Hour)
	id := seedScheduled(t, env.db, article.Article{Title: "予約中の記事", Slug: "sched", ContentMarkdown: "本文"}, at)
	seedArticle(t, env.db, article.Article{Title: "公開中の記事", Slug: "live", ContentMarkdown: "本文",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	ed := env.loggedIn(chief)
	tab := func(status string) string {
		t.Helper()
		res := ed.get("/admin/articles?status=" + status)
		expectStatus(t, res, http.StatusOK)
		return res.Body
	}

	if body := tab("scheduled"); !strings.Contains(body, "予約中の記事") || strings.Contains(body, "公開中の記事") {
		t.Errorf("scheduled tab:\n%.3000s", body)
	}
	if body := tab("published"); strings.Contains(body, "予約中の記事") || !strings.Contains(body, "公開中の記事") {
		t.Errorf("published tab:\n%.3000s", body)
	}
	dash := ed.get("/admin")
	expectStatus(t, dash, http.StatusOK)
	if s, p := dashboardCount(t, dash.Body, "予約投稿"), dashboardCount(t, dash.Body, "公開中"); s != "1" || p != "1" {
		t.Errorf("dashboard scheduled=%s published=%s, want 1 and 1", s, p)
	}

	// 執筆者は、自分の記事でも予約中なら変更・ゴミ箱への移動ができない。
	writer := env.loggedIn(studentA)
	form := url.Values{"title": {"書き換え"}, "category_id": {"1"}, "content_markdown": {"書き換え"}, "action": {"save"}}
	expectStatus(t, writer.postForm("/admin/articles/"+id, form), http.StatusForbidden)
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/delete", nil), http.StatusForbidden)
	if a := env.mustGetArticle(id); a.Title != "予約中の記事" || !article.IsScheduled(a, time.Now()) {
		t.Fatalf("writer changed scheduled article: %+v", a)
	}

	setClock(t, at.Add(time.Minute))
	if body := tab("scheduled"); strings.Contains(body, "予約中の記事") {
		t.Errorf("scheduled tab after publish time:\n%.3000s", body)
	}
	if body := tab("published"); !strings.Contains(body, "予約中の記事") || !strings.Contains(body, "公開中の記事") {
		t.Errorf("published tab after publish time:\n%.3000s", body)
	}
	dash = ed.get("/admin")
	if s, p := dashboardCount(t, dash.Body, "予約投稿"), dashboardCount(t, dash.Body, "公開中"); s != "0" || p != "2" {
		t.Errorf("dashboard after publish time scheduled=%s published=%s, want 0 and 2", s, p)
	}
}

// 予約・予約を保ったままの保存・ゴミ箱・戻す・今すぐ公開・下書きに戻す・承認し直す、のどの操作でも状態 scheduled を
// 保存しない。一度公開した記事は予約投稿にできず、承認し直すと最初の公開日時のまま公開する。
func TestAdminActionsNeverStoreScheduledStatus(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	id := ed.createArticle("予約の流れ", "本文")
	edit := "/admin/articles/" + id
	submit := func() {
		t.Helper()
		expectRedirect(t, ed.postForm(edit+"/submit-review", nil), "/admin/articles?status=review")
	}
	approveAt := func(d time.Duration) *response {
		t.Helper()
		return ed.postForm(edit+"/approve-publish", url.Values{"publish_at": {futureLocal(d)}})
	}
	form := url.Values{"title": {"予約の流れ"}, "category_id": {"1"}, "content_markdown": {"直した本文"}}
	save := func(action string) {
		t.Helper()
		form.Set("action", action)
		expectRedirect(t, ed.postForm(edit, form), edit+"/edit")
	}

	submit()
	expectRedirect(t, approveAt(2*time.Hour), edit+"/edit?flash=scheduled")
	env.expectNoStoredScheduled("approve with publish time")
	if a := env.mustGetArticle(id); !article.IsScheduled(a, time.Now()) {
		t.Fatalf("after scheduled approve: %+v", a)
	}
	save("save")
	env.expectNoStoredScheduled("save keeping schedule")

	// ゴミ箱へ移すと予約は取り消され、戻すと公開日時の無い下書きになる。
	expectRedirect(t, ed.postForm(edit+"/delete", nil), "/admin/articles?status=deleted")
	if a := env.mustGetArticle(id); a.Status != article.StatusDeleted || a.PublishedAt != nil {
		t.Fatalf("after trash: status=%s published_at=%v", a.Status, a.PublishedAt)
	}
	save("save")
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft || a.PublishedAt != nil {
		t.Fatalf("after restore: status=%s published_at=%v", a.Status, a.PublishedAt)
	}

	// もう一度予約してから「今すぐ公開」すると、予約した日時ではなく今を公開日時にする。
	submit()
	expectRedirect(t, approveAt(3*time.Hour), edit+"/edit?flash=scheduled")
	before := time.Now().UTC().Add(-time.Second)
	form.Set("action", "publish")
	expectRedirect(t, ed.postForm(edit, form), edit+"/edit?flash=published")
	a := env.mustGetArticle(id)
	if !article.IsVisible(a, time.Now()) || a.PublishedAt.Before(before) {
		t.Fatalf("after publish now: %+v", a)
	}
	first := *a.PublishedAt
	env.expectNoStoredScheduled("publish now")

	// 公開した記事を下書きに戻して承認し直すときは、予約日時の欄を出さず、日時を付けた承認は断る。
	save("draft")
	submit()
	page := ed.get(edit + "/edit")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="publish-at-kept"`, "最初の公開日時")
	if strings.Contains(page.Body, `name="publish_at"`) {
		t.Error("approve form of a once-published article has a publish time field")
	}
	res := approveAt(time.Hour)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "一度公開した記事は予約投稿にできません")
	if a := env.mustGetArticle(id); a.Status != article.StatusReview {
		t.Fatalf("after refused approve: status=%s", a.Status)
	}
	expectRedirect(t, ed.postForm(edit+"/approve-publish", nil), edit+"/edit?flash=approved")
	a = env.mustGetArticle(id)
	if !article.IsVisible(a, time.Now()) || !a.PublishedAt.Equal(first) {
		t.Fatalf("after re-approve: published_at=%v, want the first %v", a.PublishedAt, first)
	}
	env.expectNoStoredScheduled("re-approve")
}

// アプリを閉じている間に予約時刻を過ぎた記事は、起動したときに1回だけ反映するかを尋ね、もう一度起動しても尋ね直さない
// （どこまで知らせたかをデータベースに残す）。
func TestDesktopOverdueIsNotAskedAgainAfterRestart(t *testing.T) {
	out := filepath.Join(t.TempDir(), "site-export")
	var overdueID string
	first := newDesktopEnvWith(t, func(env *testEnv) { overdueID = seedOverdueDesktop(t, env, out) })
	st := first.server.publishState()
	waitForCond(t, "overdue article counted", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.overdue == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.server.StopBackground(ctx); err != nil {
		t.Fatalf("StopBackground: %v", err)
	}

	// 同じデータベースでアプリを起動し直す。
	restartedAt := time.Now().UTC()
	s, err := New(Config{DB: first.db, UploadDir: first.uploadDir, AttachmentDir: first.attachmentDir, Desktop: true,
		SchedulerInterval: 20 * time.Millisecond, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	// 起動後に2回確かめ終わるまで待つ（1回目に知らせた記事があれば、2回目より前に数え終わっている）。
	checked := func() time.Time {
		var v string
		if err := first.db.Get(&v, `SELECT value FROM settings WHERE key = ?`, scheduler.CheckpointSetting); err != nil {
			return time.Time{}
		}
		at, _ := time.Parse(time.RFC3339Nano, v)
		return at
	}
	var firstCheck time.Time
	waitForCond(t, "first check after restart", func() bool {
		firstCheck = checked()
		return firstCheck.After(restartedAt)
	})
	waitForCond(t, "second check after restart", func() bool { return checked().After(firstCheck) })

	again := s.publishState()
	again.mu.Lock()
	overdue := again.overdue
	again.mu.Unlock()
	if overdue != 0 {
		t.Errorf("overdue after restart = %d, want 0 (already asked)", overdue)
	}
	if a := first.mustGetArticle(overdueID); !article.IsVisible(a, time.Now()) {
		t.Errorf("overdue article is not visible: %+v", a)
	}
	first.expectNoStoredScheduled("restart")
}
