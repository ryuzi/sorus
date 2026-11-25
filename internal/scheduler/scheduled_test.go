package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// scheduledSetOf は保存した予約だった記事の集合を返す（キーが無ければ作る）。
func scheduledSetOf(t *testing.T, db *sqlx.DB) map[string]time.Time {
	t.Helper()
	set, err := Scheduled(context.Background(), db)
	if err != nil {
		t.Fatalf("Scheduled: %v", err)
	}
	return set
}

func expectInSet(t *testing.T, db *sqlx.DB, step, id string, at time.Time) {
	t.Helper()
	got, ok := scheduledSetOf(t, db)[id]
	if !ok || !got.Equal(at) {
		t.Fatalf("%s: scheduled set[%s] = %v (present %v), want %v", step, id, got, ok, at)
	}
}

func expectNotInSet(t *testing.T, db *sqlx.DB, step, id string) {
	t.Helper()
	if got, ok := scheduledSetOf(t, db)[id]; ok {
		t.Fatalf("%s: %s is still in the scheduled set (%v)", step, id, got)
	}
}

// Track は、予約中の記事を公開日時とともに覚え、公開日時を迎えたあとに保存し直しても覚えたままにする。すぐに公開した
// 記事は入れず、「今すぐ公開」・下書きに戻す・ゴミ箱・記事が無い、のどれでも忘れる。
func TestTrackScheduledArticles(t *testing.T) {
	db := openDB(t, ":memory:")
	ctx := context.Background()
	svc := article.NewService(db)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	track := func(step string, when time.Time, ids ...string) {
		t.Helper()
		if err := Track(ctx, db, when, ids...); err != nil {
			t.Fatalf("%s: Track: %v", step, err)
		}
	}

	immediate := saveArticle(t, db, &article.Article{Title: "すぐに公開", Status: article.StatusPublished})
	track("immediate", time.Now(), immediate.ID)
	expectNotInSet(t, db, "immediate publish", immediate.ID)

	kept := saveArticle(t, db, &article.Article{Title: "予約を保ったまま保存", Status: article.StatusPublished, PublishedAt: &at})
	track("schedule", time.Now(), kept.ID)
	expectInSet(t, db, "scheduled", kept.ID, at)
	// 公開日時を迎えたあとに保存し直した（更新日時が公開日時を超えた）。
	if _, err := db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, at.Add(time.Minute), kept.ID); err != nil {
		t.Fatal(err)
	}
	track("saved after the publish time", at.Add(2*time.Minute), kept.ID)
	expectInSet(t, db, "saved after the publish time", kept.ID, at)

	// 予約の日時を変えたら覚え直す。
	moved := saveArticle(t, db, &article.Article{Title: "日時を変えた予約", Status: article.StatusPublished, PublishedAt: &at})
	track("schedule", time.Now(), moved.ID)
	later := at.Add(time.Hour)
	moved.PublishedAt = &later
	if err := svc.Save(moved); err != nil {
		t.Fatal(err)
	}
	track("rescheduled", time.Now(), moved.ID)
	expectInSet(t, db, "rescheduled", moved.ID, later)

	// 「今すぐ公開」で公開日時を今にした記事は、すぐに公開した記事と同じく忘れる。
	nowPublished := saveArticle(t, db, &article.Article{Title: "今すぐ公開", Status: article.StatusPublished, PublishedAt: &at})
	track("schedule", time.Now(), nowPublished.ID)
	nowPublished.PublishedAt = ptr(time.Now().UTC())
	if err := svc.Save(nowPublished); err != nil {
		t.Fatal(err)
	}
	track("publish now", time.Now(), nowPublished.ID)
	expectNotInSet(t, db, "publish now", nowPublished.ID)

	drafted := saveArticle(t, db, &article.Article{Title: "下書きに戻す", Status: article.StatusPublished, PublishedAt: &at})
	trashed := saveArticle(t, db, &article.Article{Title: "ゴミ箱", Status: article.StatusPublished, PublishedAt: &at})
	track("schedule", time.Now(), drafted.ID, trashed.ID)
	drafted.Status = article.StatusDraft
	if err := svc.Save(drafted); err != nil {
		t.Fatal(err)
	}
	if err := svc.SoftDelete(trashed.ID); err != nil {
		t.Fatal(err)
	}
	track("unschedule", time.Now(), drafted.ID, trashed.ID, "no-such-article")
	expectNotInSet(t, db, "back to draft", drafted.ID)
	expectNotInSet(t, db, "trashed", trashed.ID)
	expectInSet(t, db, "untouched", kept.ID, at)
}

// 集合のキーが無ければ、過去の記事から推測せず空の集合を作って保存する。
func TestScheduledMissingSetStartsEmpty(t *testing.T) {
	db := openDB(t, ":memory:")
	immediate := saveArticle(t, db, &article.Article{Title: "すぐに公開", Status: article.StatusPublished})
	imported := saveArticle(t, db, &article.Article{Title: "取り込んだ記事", Status: article.StatusPublished,
		PublishedAt: ptr(time.Now().Add(-30 * 24 * time.Hour))})
	due := scheduleArticle(t, db, "閉じている間に迎えた予約", "", time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
	future := scheduleArticle(t, db, "まだ先の予約", "", time.Now().UTC().Add(time.Hour).Truncate(time.Second))

	set := scheduledSetOf(t, db)
	if len(set) != 0 {
		t.Fatalf("missing set initialized to %v, want empty (articles: %s, %s, %s, %s)", set, immediate.ID, imported.ID, due.ID, future.ID)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM settings WHERE key = ?`, ScheduledSetting); err != nil || n != 1 {
		t.Fatalf("saved set rows = %d (%v), want 1", n, err)
	}
	// 作ったあとは、保存した集合を使う。
	late := scheduleArticle(t, db, "あとから直接書いた予約", "", time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
	if _, ok := scheduledSetOf(t, db)[late.ID]; ok {
		t.Errorf("the set was rebuilt from articles after it had been saved")
	}
}

// 予約中に知らされる前に sorus を閉じ、公開日時を迎えたあとに保存し直した予約投稿も、起動し直した Scheduler が知らせる
// （更新日時が公開日時を超えても、予約だった記事の集合に残る）。
func TestNotifyDueArticles_ResavedAfterPublishTimeAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resaved.db")
	db := openDB(t, path)
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	a := saveArticle(t, db, &article.Article{Title: "閉じている間に迎えて保存し直した予約", Status: article.StatusPublished,
		PublishedAt: ptr(base.Add(time.Hour))})
	if err := Track(context.Background(), db, base, a.ID); err != nil {
		t.Fatal(err)
	}
	first := newTestScheduler(db, nil, 0, base)
	if n, err := first.NotifyDueArticles(context.Background()); n != 0 || err != nil {
		t.Fatalf("first run = (%d, %v), want (0, nil)", n, err)
	}
	db.Close()

	// 公開日時を過ぎたあとに保存し直した（Scheduler が確かめる前）。
	reopened := openExisting(t, path)
	if _, err := reopened.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, base.Add(2*time.Hour), a.ID); err != nil {
		t.Fatal(err)
	}
	restarted := newTestScheduler(reopened, nil, 0, base.Add(3*time.Hour))
	got := notifyInto(restarted)
	for range 2 {
		if _, err := restarted.NotifyDueArticles(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if ids := got.all(); len(ids) != 1 || ids[0] != a.ID {
		t.Errorf("after restart notified %v, want [%s] once", ids, a.ID)
	}
	expectInSet(t, reopened, "after notifying", a.ID, base.Add(time.Hour))
}

// Scheduler は確かめるたびに集合を記事の今の状態に合わせる。集合を保たない経路で予約した記事を覚え、予約を取り消した
// 記事・消えた記事を忘れる。
func TestNotifyDueArticles_TidiesScheduledSet(t *testing.T) {
	db := openDB(t, ":memory:")
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	untracked := saveArticle(t, db, &article.Article{Title: "集合の外で予約した記事", Status: article.StatusPublished,
		PublishedAt: ptr(base.Add(time.Hour))})
	unscheduled := saveArticle(t, db, &article.Article{Title: "あとで取り消す予約", Status: article.StatusPublished,
		PublishedAt: ptr(base.Add(time.Hour))})
	s := newTestScheduler(db, nil, 0, base)
	if _, err := s.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectInSet(t, db, "upcoming", untracked.ID, base.Add(time.Hour))
	expectInSet(t, db, "upcoming", unscheduled.ID, base.Add(time.Hour))

	if _, err := db.Exec(`UPDATE articles SET status = ?, published_at = NULL WHERE id = ?`, article.StatusReview, unscheduled.ID); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(time.Minute) }
	if _, err := s.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectNotInSet(t, db, "unscheduled outside the set", unscheduled.ID)
	expectInSet(t, db, "still scheduled", untracked.ID, base.Add(time.Hour))
}

// ForgetSent は、反映を始めた時刻以前に予約した公開日時を迎えた記事を外す。Scheduler がまだ確かめていない記事は残す。
func TestForgetSentKeepsArticlesNotYetChecked(t *testing.T) {
	db := openDB(t, ":memory:")
	ctx := context.Background()
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	sent := saveArticle(t, db, &article.Article{Title: "送った予約", Status: article.StatusPublished, PublishedAt: ptr(base.Add(time.Minute))})
	unchecked := saveArticle(t, db, &article.Article{Title: "まだ確かめていない予約", Status: article.StatusPublished, PublishedAt: ptr(base.Add(3 * time.Minute))})
	future := saveArticle(t, db, &article.Article{Title: "先の予約", Status: article.StatusPublished, PublishedAt: ptr(base.Add(time.Hour))})
	if err := Track(ctx, db, base, sent.ID, unchecked.ID, future.ID); err != nil {
		t.Fatal(err)
	}
	// 確かめる前は何も外さない。
	if err := ForgetSent(ctx, db, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	expectInSet(t, db, "before any check", sent.ID, base.Add(time.Minute))

	s := newTestScheduler(db, nil, 0, base.Add(2*time.Minute))
	if _, err := s.NotifyDueArticles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ForgetSent(ctx, db, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	expectNotInSet(t, db, "sent", sent.ID)
	expectInSet(t, db, "not yet checked", unchecked.ID, base.Add(3*time.Minute))
	expectInSet(t, db, "future", future.ID, base.Add(time.Hour))
}
