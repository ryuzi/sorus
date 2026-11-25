package scheduler

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/database"
	"sorus/internal/search"
)

const (
	testAuthorID   = "user-1"
	testCategoryID = int64(1)
)

// openDB は著者とカテゴリを1件ずつ登録した、マイグレーション済みのDBを返す。
func openDB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("Connect(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, 'yamada', '山田研究室', 'h', 'writer')`, testAuthorID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO categories (id, name, slug) VALUES (?, '研究発表', 'research')`, testCategoryID); err != nil {
		t.Fatal(err)
	}
	return db
}

func newMemIndex(t *testing.T) *search.Index {
	t.Helper()
	idx, err := search.NewMemIndex()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// newTestScheduler はログを捨て、現在時刻を now に固定した Scheduler を返す。
func newTestScheduler(db *sqlx.DB, index Indexer, interval time.Duration, now time.Time) *Scheduler {
	s := New(db, index, interval)
	s.log = log.New(io.Discard, "", 0)
	if !now.IsZero() {
		s.now = func() time.Time { return now }
	}
	return s
}

func saveArticle(t *testing.T, db *sqlx.DB, a *article.Article) *article.Article {
	t.Helper()
	a.AuthorID = testAuthorID
	a.CategoryID = testCategoryID
	if err := article.NewService(db).Save(a); err != nil {
		t.Fatalf("Save(%q): %v", a.Title, err)
	}
	return a
}

// scheduleArticle は公開日時 at の予約投稿（状態は公開）を用意する。承認した時点（公開日時より前）に保存した
// 予約投稿にするため、更新日時を at の1日前にする（at が過去なら、sorus を閉じている間に公開日時を迎えた記事になる）。
func scheduleArticle(t *testing.T, db *sqlx.DB, title, body string, at time.Time) *article.Article {
	t.Helper()
	a := saveArticle(t, db, &article.Article{
		Title: title, ContentMarkdown: body, Status: article.StatusPublished, PublishedAt: &at,
	})
	approved := at.UTC().Add(-24 * time.Hour)
	if _, err := db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, approved, a.ID); err != nil {
		t.Fatal(err)
	}
	a.UpdatedAt = approved
	return a
}

func mustGet(t *testing.T, db *sqlx.DB, id string) *article.Article {
	t.Helper()
	a, err := article.NewService(db).GetByID(id)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", id, err)
	}
	return a
}

func searchIDs(t *testing.T, idx *search.Index, q string) []string {
	t.Helper()
	res, err := idx.Search(q, 1, search.MaxPageSize)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	var ids []string
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	return ids
}

func assertPublishedAt(t *testing.T, a *article.Article, want time.Time) {
	t.Helper()
	if a.Status != article.StatusPublished {
		t.Errorf("%q status = %q, want published", a.Title, a.Status)
	}
	if a.PublishedAt == nil || !a.PublishedAt.Equal(want) {
		t.Errorf("%q published_at = %v, want %v", a.Title, a.PublishedAt, want)
	}
}

// assertUnchanged は記事 a の状態・公開日時・更新日時が保存したときのままかを確かめる（Scheduler は記事を書き換えない）。
func assertUnchanged(t *testing.T, db *sqlx.DB, a *article.Article) {
	t.Helper()
	got := mustGet(t, db, a.ID)
	if got.Status != a.Status || !got.UpdatedAt.Equal(a.UpdatedAt) || (got.PublishedAt == nil) != (a.PublishedAt == nil) ||
		(a.PublishedAt != nil && !got.PublishedAt.Equal(*a.PublishedAt)) {
		t.Errorf("%q = status %q published_at %v updated_at %v, want unchanged %q %v %v",
			a.Title, got.Status, got.PublishedAt, got.UpdatedAt, a.Status, a.PublishedAt, a.UpdatedAt)
	}
}

// notified は SetOnPublished で知らされた記事 ID を集める。
type notified struct {
	mu    sync.Mutex
	calls [][]string
	ch    chan []string
}

func notifyInto(s *Scheduler) *notified {
	n := &notified{ch: make(chan []string, 16)}
	s.SetOnPublished(func(ids []string) {
		n.mu.Lock()
		n.calls = append(n.calls, append([]string(nil), ids...))
		n.mu.Unlock()
		n.ch <- ids
	})
	return n
}

func (n *notified) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, c := range n.calls {
		out = append(out, c...)
	}
	return sortedCopy(out)
}

// waitFor は cond が true になるまで最大 timeout 待つ。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNotifyDueArticles_NotifiesOnlyDueArticlesAndIndexesThem(t *testing.T) {
	db := openDB(t, ":memory:")
	idx := newMemIndex(t)
	// 検索は公開日時が未来の記事を除外するため、実時刻より過去を基準にする。
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)

	overdue := scheduleArticle(t, db, "量子コンピュータの研究成果", "超伝導量子ビットの実験報告", base.Add(-time.Hour))
	onTime := scheduleArticle(t, db, "学会発表のお知らせ", "情報処理学会で発表します", base)
	future := scheduleArticle(t, db, "卒業論文発表会", "来月の発表会の案内", base.Add(time.Hour))
	draft := saveArticle(t, db, &article.Article{Title: "下書きの記事", ContentMarkdown: "未完成の原稿"})
	review := saveArticle(t, db, &article.Article{Title: "校閲中の記事", ContentMarkdown: "確認待ちの原稿",
		Status: article.StatusReview})

	s := newTestScheduler(db, idx, 0, base)
	got := notifyInto(s)
	n, err := s.NotifyDueArticles(context.Background())
	if err != nil {
		t.Fatalf("NotifyDueArticles: %v", err)
	}
	if n != 2 {
		t.Errorf("notified = %d, want 2", n)
	}
	if ids, want := got.all(), sortedCopy([]string{overdue.ID, onTime.ID}); !slices.Equal(ids, want) {
		t.Errorf("notified ids = %v, want %v", ids, want)
	}

	// 状態は書き換えない。予約投稿は公開日時を迎えた時点で、状態を変えずに読者に見え始める。
	assertPublishedAt(t, mustGet(t, db, overdue.ID), base.Add(-time.Hour))
	assertPublishedAt(t, mustGet(t, db, onTime.ID), base)
	for _, a := range []*article.Article{overdue, onTime, future, draft, review} {
		assertUnchanged(t, db, a)
	}

	if ids := searchIDs(t, idx, "量子ビット"); len(ids) != 1 || ids[0] != overdue.ID {
		t.Errorf("search 量子ビット = %v, want [%s]", ids, overdue.ID)
	}
	if ids := searchIDs(t, idx, "山田研究室 学会"); len(ids) != 1 || ids[0] != onTime.ID {
		t.Errorf("search by author = %v, want [%s]", ids, onTime.ID)
	}
	for _, q := range []string{"卒業論文", "未完成", "確認待ち"} {
		if ids := searchIDs(t, idx, q); len(ids) != 0 {
			t.Errorf("search %s = %v, want no hits for unpublished articles", q, ids)
		}
	}

	// 同じ時刻での再実行では何も知らせない。
	if n, err := s.NotifyDueArticles(context.Background()); n != 0 || err != nil {
		t.Errorf("second run = (%d, %v), want (0, nil)", n, err)
	}

	// 時刻が進むと残りの予約投稿を知らせる。
	s.now = func() time.Time { return base.Add(2 * time.Hour) }
	if n, err := s.NotifyDueArticles(context.Background()); n != 1 || err != nil {
		t.Fatalf("run after time passes = (%d, %v), want (1, nil)", n, err)
	}
	assertPublishedAt(t, mustGet(t, db, future.ID), base.Add(time.Hour))
	if ids := searchIDs(t, idx, "卒業論文"); len(ids) != 1 || ids[0] != future.ID {
		t.Errorf("search 卒業論文 = %v, want [%s]", ids, future.ID)
	}
}

func TestNotifyDueArticles_ComparesInstantsAcrossTimeZones(t *testing.T) {
	db := openDB(t, ":memory:")
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// JST 08:59 は UTC の前日 23:59 で、base より前（公開対象）。
	jst := time.FixedZone("JST", 9*60*60)
	due := scheduleArticle(t, db, "入学式", "", time.Date(2026, 4, 1, 8, 59, 0, 0, jst))
	notDue := scheduleArticle(t, db, "オリエンテーション", "", time.Date(2026, 4, 1, 9, 1, 0, 0, jst))

	s := newTestScheduler(db, nil, 0, base)
	got := notifyInto(s)
	n, err := s.NotifyDueArticles(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("NotifyDueArticles = (%d, %v), want (1, nil)", n, err)
	}
	if ids := got.all(); len(ids) != 1 || ids[0] != due.ID {
		t.Errorf("notified = %v, want only the JST 08:59 article %s", ids, due.ID)
	}
	if a := mustGet(t, db, notDue.ID); !article.IsScheduled(a, base) {
		t.Errorf("JST 09:01 article is not scheduled at %v: %+v", base, a)
	}
}

// flakyIndexer は最初の failures 回の呼び出しを失敗させ、成功した記事IDを記録する。
type flakyIndexer struct {
	mu       sync.Mutex
	failures int
	calls    int
	indexed  []string
}

func (f *flakyIndexer) IndexArticle(a *article.Article, authorName, categoryName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures > 0 {
		f.failures--
		return errors.New("index unavailable")
	}
	if authorName != "山田研究室" || categoryName != "研究発表" || a.Status != article.StatusPublished {
		return errors.New("unexpected article or names")
	}
	f.indexed = append(f.indexed, a.ID)
	return nil
}

func TestNotifyDueArticles_RetriesFailedIndexing(t *testing.T) {
	db := openDB(t, ":memory:")
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	a := scheduleArticle(t, db, "研究室公開", "本文", base.Add(-time.Minute))
	idx := &flakyIndexer{failures: 1}
	s := newTestScheduler(db, idx, 0, base)

	n, err := s.NotifyDueArticles(context.Background())
	if n != 1 || err == nil || !strings.Contains(err.Error(), "index unavailable") {
		t.Fatalf("first run = (%d, %v), want (1, index error)", n, err)
	}
	// 検索への反映に失敗しても記事は公開のまま（読者に見えている）。
	if got := mustGet(t, db, a.ID); !article.IsVisible(got, base) {
		t.Errorf("article after index failure = %+v, want visible", got)
	}

	n, err = s.NotifyDueArticles(context.Background())
	if n != 0 || err != nil {
		t.Fatalf("retry run = (%d, %v), want (0, nil)", n, err)
	}
	if len(idx.indexed) != 1 || idx.indexed[0] != a.ID {
		t.Errorf("indexed = %v, want [%s]", idx.indexed, a.ID)
	}

	calls := idx.calls
	if _, err := s.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if idx.calls != calls {
		t.Errorf("indexer called %d more times after successful retry, want 0", idx.calls-calls)
	}
}

func TestNotifyDueArticles_WithoutIndexer(t *testing.T) {
	db := openDB(t, ":memory:")
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	a := scheduleArticle(t, db, "お知らせ", "", base)
	n, err := newTestScheduler(db, nil, 0, base).NotifyDueArticles(context.Background())
	if n != 1 || err != nil {
		t.Fatalf("NotifyDueArticles = (%d, %v), want (1, nil)", n, err)
	}
	assertPublishedAt(t, mustGet(t, db, a.ID), base)
}

func TestNotifyDueArticles_CanceledContext(t *testing.T) {
	db := openDB(t, ":memory:")
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	scheduleArticle(t, db, "お知らせ", "", base.Add(-time.Hour))
	s := newTestScheduler(db, nil, 0, base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := s.NotifyDueArticles(ctx); n != 0 || err == nil {
		t.Fatalf("NotifyDueArticles(canceled) = (%d, %v), want (0, error)", n, err)
	}
	// 確かめた時刻を進めておらず、トランザクションも残らないため、次の実行で知らせられること。
	if n, err := s.NotifyDueArticles(context.Background()); n != 1 || err != nil {
		t.Errorf("run after cancel = (%d, %v), want (1, nil)", n, err)
	}
}

func TestNotifyDueArticles_ConcurrentCallsNotifyEachArticleOnce(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "sched.db"))
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	const articles = 10
	for i := 0; i < articles; i++ {
		scheduleArticle(t, db, "予約記事", "", base.Add(-time.Duration(i)*time.Minute))
	}
	idx := &flakyIndexer{}
	s := newTestScheduler(db, idx, 0, base)

	const workers = 4
	var wg sync.WaitGroup
	counts := make(chan int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.NotifyDueArticles(context.Background())
			if err != nil {
				t.Errorf("NotifyDueArticles: %v", err)
			}
			counts <- n
		}()
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	if total != articles {
		t.Errorf("total notified = %d, want %d", total, articles)
	}
	if len(idx.indexed) != articles {
		t.Errorf("indexed %d articles, want %d", len(idx.indexed), articles)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM articles WHERE status = 'published'`); err != nil {
		t.Fatal(err)
	}
	if n != articles {
		t.Errorf("published rows = %d, want %d", n, articles)
	}
}

func TestDueQueriesUseStatusIndex(t *testing.T) {
	db := openDB(t, ":memory:")
	now := time.Now().UTC()
	for name, q := range map[string]struct {
		query string
		args  []any
	}{
		"due":      {dueQuery, []any{article.StatusPublished, now, now.Add(-time.Minute)}},
		"upcoming": {upcomingQuery, []any{article.StatusPublished, now}},
	} {
		var plan []struct {
			ID      int    `db:"id"`
			Parent  int    `db:"parent"`
			NotUsed int    `db:"notused"`
			Detail  string `db:"detail"`
		}
		if err := db.Select(&plan, "EXPLAIN QUERY PLAN "+q.query, q.args...); err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		if len(plan) == 0 || !strings.Contains(plan[0].Detail, "USING INDEX idx_articles_status_published_at") {
			t.Errorf("%s query plan %+v does not use idx_articles_status_published_at", name, plan)
		}
	}
}

func TestRun_NotifiesOverdueArticlesImmediatelyOnStart(t *testing.T) {
	db := openDB(t, ":memory:")
	idx := newMemIndex(t)
	// サーバー停止中に公開日時を過ぎた記事。周期が長くても起動直後に知らせる。
	a := scheduleArticle(t, db, "停止中に予定日時を迎えた記事", "再起動直後の公開", time.Now().Add(-time.Hour))
	s := newTestScheduler(db, idx, time.Hour, time.Time{})
	got := notifyInto(s)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case ids := <-got.ch:
		if len(ids) != 1 || ids[0] != a.ID {
			t.Errorf("notified on start = %v, want [%s]", ids, a.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("overdue article was not notified on start")
	}
	// 検索インデックスへの反映は確かめた時刻の保存の後に行われる。
	waitFor(t, 5*time.Second, "published article to be searchable", func() bool {
		ids := searchIDs(t, idx, "再起動直後")
		return len(ids) == 1 && ids[0] == a.ID
	})
	if !s.Running() {
		t.Error("Running() = false while Run is active")
	}
	if err := s.Run(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Run = %v, want ErrAlreadyRunning", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if s.Running() {
		t.Error("Running() = true after Run returned")
	}
}

func TestRun_NotifiesOnTick(t *testing.T) {
	db := openDB(t, ":memory:")
	idx := newMemIndex(t)
	s := newTestScheduler(db, idx, 10*time.Millisecond, time.Time{})
	got := notifyInto(s)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	waitFor(t, 5*time.Second, "scheduler to start", s.Running)

	// 起動後に予約され、公開日時を迎えた記事も次の周期で知らせる。
	a := saveArticle(t, db, &article.Article{Title: "周期実行で公開される記事", ContentMarkdown: "ティッカーによる公開",
		Status: article.StatusPublished, PublishedAt: ptr(time.Now().Add(50 * time.Millisecond))})
	waitFor(t, 5*time.Second, "scheduled article to be notified on tick", func() bool {
		return slices.Contains(got.all(), a.ID)
	})
	waitFor(t, 5*time.Second, "published article to be searchable", func() bool {
		ids := searchIDs(t, idx, "ティッカー")
		return len(ids) == 1 && ids[0] == a.ID
	})
}

func TestSetOnPublished_CalledOncePerRunWithPublishedIDs(t *testing.T) {
	db := openDB(t, ":memory:")
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	a := scheduleArticle(t, db, "朝のお知らせ", "", base.Add(-time.Hour))
	b := scheduleArticle(t, db, "昼のお知らせ", "", base.Add(-time.Minute))
	later := scheduleArticle(t, db, "夜のお知らせ", "", base.Add(time.Hour))
	// 検索インデックスへの反映に失敗しても、記事は読者に見え始めているため呼ぶ。
	s := newTestScheduler(db, &flakyIndexer{failures: 1}, 0, base)

	var calls [][]string
	s.SetOnPublished(func(ids []string) { calls = append(calls, ids) })

	if n, _ := s.NotifyDueArticles(context.Background()); n != 2 {
		t.Fatalf("notified = %d, want 2", n)
	}
	if len(calls) != 1 {
		t.Fatalf("hook called %d times, want 1 for one run", len(calls))
	}
	got := strings.Join(sortedCopy(calls[0]), ",")
	if want := strings.Join(sortedCopy([]string{a.ID, b.ID}), ","); got != want {
		t.Errorf("hook ids = %s, want %s", got, want)
	}

	// 何も知らせなかった実行では呼ばない。
	if _, err := s.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Errorf("hook called on a run that published nothing (calls = %d)", len(calls))
	}

	// 解除すると呼ばない。
	s.SetOnPublished(nil)
	s.now = func() time.Time { return base.Add(2 * time.Hour) }
	if n, err := s.NotifyDueArticles(context.Background()); n != 1 || err != nil {
		t.Fatalf("run after time passes = (%d, %v), want (1, nil)", n, err)
	}
	if len(calls) != 1 {
		t.Errorf("hook called after SetOnPublished(nil) (calls = %d)", len(calls))
	}
	if got := mustGet(t, db, later.ID); got.Status != article.StatusPublished {
		t.Errorf("status = %q, want published", got.Status)
	}
}

func sortedCopy(ids []string) []string {
	out := append([]string(nil), ids...)
	slices.Sort(out)
	return out
}

func TestNew_DefaultInterval(t *testing.T) {
	db := openDB(t, ":memory:")
	if s := New(db, nil, 0); s.interval != DefaultInterval {
		t.Errorf("interval = %v, want %v", s.interval, DefaultInterval)
	}
	if s := New(db, nil, 5*time.Second); s.interval != 5*time.Second {
		t.Errorf("interval = %v, want 5s", s.interval)
	}
}

func ptr[T any](v T) *T { return &v }

// すぐに公開した記事（公開日時が保存した時刻と同じ）や、過去の日付で取り込んだ記事は、予約投稿ではないため知らせない。
func TestNotifyDueArticles_IgnoresImmediatelyPublishedArticles(t *testing.T) {
	db := openDB(t, ":memory:")
	now := saveArticle(t, db, &article.Article{Title: "すぐに公開", Status: article.StatusPublished})
	imported := saveArticle(t, db, &article.Article{Title: "取り込んだ記事", Status: article.StatusPublished,
		PublishedAt: ptr(time.Now().Add(-30 * 24 * time.Hour))})
	due := scheduleArticle(t, db, "予約投稿", "", time.Now().Add(-time.Minute))

	s := newTestScheduler(db, nil, 0, time.Now().Add(time.Second))
	got := notifyInto(s)
	if n, err := s.NotifyDueArticles(context.Background()); n != 1 || err != nil {
		t.Fatalf("NotifyDueArticles = (%d, %v), want (1, nil)", n, err)
	}
	if ids := got.all(); len(ids) != 1 || ids[0] != due.ID {
		t.Errorf("notified = %v, want only %s (not %s, %s)", ids, due.ID, now.ID, imported.ID)
	}
}

// 確かめた時刻は DB に残り、再起動した Scheduler は閉じている間に公開日時を迎えた記事だけを1回知らせる。
func TestNotifyDueArticles_CheckpointSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db := openDB(t, path)
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	before := scheduleArticle(t, db, "前回の起動中に迎えた予約", "", base.Add(-time.Hour))
	whileClosed := scheduleArticle(t, db, "閉じている間に迎えた予約", "", base.Add(time.Hour))
	later := scheduleArticle(t, db, "まだ先の予約", "", base.Add(5*time.Hour))

	first := newTestScheduler(db, nil, 0, base)
	got := notifyInto(first)
	if _, err := first.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ids := got.all(); len(ids) != 1 || ids[0] != before.ID {
		t.Fatalf("first run notified %v, want [%s]", ids, before.ID)
	}
	db.Close()

	// sorus を閉じて2時間後に開き直す。新しいプロセスの Scheduler は前回の upcoming を知らない。
	reopened := openExisting(t, path)
	restarted := newTestScheduler(reopened, nil, 0, base.Add(2*time.Hour))
	got = notifyInto(restarted)
	for range 2 {
		if _, err := restarted.NotifyDueArticles(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if ids := got.all(); len(ids) != 1 || ids[0] != whileClosed.ID {
		t.Errorf("after restart notified %v, want only [%s] once", ids, whileClosed.ID)
	}
	var saved string
	if err := reopened.Get(&saved, `SELECT value FROM settings WHERE key = ?`, CheckpointSetting); err != nil {
		t.Fatal(err)
	}
	if want := base.Add(2 * time.Hour).Format(time.RFC3339Nano); saved != want {
		t.Errorf("checkpoint = %q, want %q", saved, want)
	}

	// 時計が戻っても確かめた時刻は戻さず、知らせ終えた記事を再び知らせない。
	restarted.now = func() time.Time { return base.Add(-2 * time.Hour) }
	if n, err := restarted.NotifyDueArticles(context.Background()); n != 0 || err != nil {
		t.Errorf("run with clock moved back = (%d, %v), want (0, nil)", n, err)
	}
	restarted.now = func() time.Time { return base.Add(6 * time.Hour) }
	if _, err := restarted.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ids, want := got.all(), sortedCopy([]string{whileClosed.ID, later.ID}); !slices.Equal(ids, want) {
		t.Errorf("notified %v, want %v", ids, want)
	}
}

// 公開日時を迎えてから確かめるまでの間に、公開日時を変えずに保存し直した予約投稿も知らせる。
// 「今すぐ公開」で公開日時を変えた記事は、すぐに公開した記事と同じく知らせない。
func TestNotifyDueArticles_ResavedScheduledArticle(t *testing.T) {
	db := openDB(t, ":memory:")
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	kept := scheduleArticle(t, db, "予約を維持して保存", "", base.Add(30*time.Second))
	nowPublished := scheduleArticle(t, db, "今すぐ公開", "", base.Add(40*time.Second))
	s := newTestScheduler(db, nil, 0, base)
	got := notifyInto(s)
	if n, err := s.NotifyDueArticles(context.Background()); n != 0 || err != nil {
		t.Fatalf("first run = (%d, %v), want (0, nil)", n, err)
	}

	if _, err := db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, base.Add(45*time.Second), kept.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE articles SET published_at = ?, updated_at = ? WHERE id = ?`,
		base.Add(10*time.Second), base.Add(10*time.Second), nowPublished.ID); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(time.Minute) }
	if _, err := s.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ids := got.all(); len(ids) != 1 || ids[0] != kept.ID {
		t.Errorf("notified %v, want only [%s]", ids, kept.ID)
	}
}

// openExisting は path の DB をマイグレーションして開き直す（データは入れない）。
func openExisting(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}
