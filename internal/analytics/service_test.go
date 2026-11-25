package analytics

import (
	"testing"

	"github.com/jmoiron/sqlx"
	"time"

	"sorus/internal/database"
)

func analyticsDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO users (id, username, password_hash, role) VALUES ('u1', 'writer', 'x', 'writer')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO categories (name, slug) VALUES ('News', 'news')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id, status, published_at) VALUES
		('a1', 'First', 'first', 'u1', 1, 'published', CURRENT_TIMESTAMP),
		('a2', 'Second', 'second', 'u1', 1, 'published', CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRecordViewDeduplicatesAndExcludesBots(t *testing.T) {
	db := analyticsDB(t)
	s := NewService(db)
	if !s.RecordView("a1", "192.0.2.1", "Mozilla/5.0") {
		t.Fatal("first view was rejected")
	}
	if !s.RecordView("a1", "192.0.2.1", "Mozilla/5.0") {
		t.Fatal("repeat view was rejected")
	}
	if s.RecordView("a1", "192.0.2.2", "Googlebot/2.1") {
		t.Fatal("bot view was accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetArticleViews("a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Views != 2 || got.UniqueVisitors != 1 {
		t.Fatalf("article stats = %+v, want 2 views / 1 unique", got)
	}
	var site Stats
	if err := db.Get(&site, `SELECT date, views, unique_visitors FROM site_views_daily`); err != nil {
		t.Fatal(err)
	}
	if site.Views != 2 || site.UniqueVisitors != 1 {
		t.Fatalf("site stats = %+v, want 2 views / 1 unique", site)
	}
}

func TestRecordViewUniqueVisitorsAcrossArticlesAndQueries(t *testing.T) {
	db := analyticsDB(t)
	s := NewService(db)
	for _, id := range []string{"a1", "a2"} {
		if !s.RecordView(id, "192.0.2.1", "Mozilla/5.0") {
			t.Fatalf("record %s rejected", id)
		}
	}
	if !s.RecordView("a1", "192.0.2.3", "Mozilla/5.0") {
		t.Fatal("distinct visitor rejected")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err := s.GetSiteStats(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Views != 3 || stats[0].UniqueVisitors != 2 {
		t.Fatalf("site stats = %+v", stats)
	}
	popular, err := s.GetPopularArticles(2, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(popular) != 2 || popular[0].ArticleID != "a1" || popular[0].Views != 2 {
		t.Fatalf("popular articles = %+v", popular)
	}
}

func TestCloseIsIdempotentAndRejectsViews(t *testing.T) {
	s := NewService(analyticsDB(t))
	if !s.RecordView("a1", "192.0.2.1", "Mozilla/5.0") {
		t.Fatal("record rejected")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.RecordView("a1", "192.0.2.1", "Mozilla/5.0") {
		t.Fatal("view accepted after close")
	}
}

func TestGetPopularArticlesUsesTrailingDays(t *testing.T) {
	db := analyticsDB(t)
	oldDate := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
	_, err := db.Exec(`INSERT INTO article_views_daily (article_id, date, views, unique_visitors) VALUES ('a1', ?, 100, 50)`, oldDate)
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(db)
	defer s.Close()
	got, err := s.GetPopularArticles(5, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("popular articles = %+v, want no rows in trailing period", got)
	}
}

// 集計を書き出す前に記事が無くなっても（バックアップからの復元・完全削除）、他の記事とサイト全体の集計は保存する。
func TestFlushSkipsViewsOfRemovedArticles(t *testing.T) {
	db := analyticsDB(t)
	s := NewService(db)
	for _, id := range []string{"a1", "a2", "missing"} {
		if !s.RecordView(id, "192.0.2.1", "Mozilla/5.0") {
			t.Fatalf("record %s rejected", id)
		}
	}
	if _, err := db.Exec(`DELETE FROM articles WHERE id = 'a2'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := s.GetArticleViews("a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Views != 1 {
		t.Errorf("a1 views = %d, want 1", got.Views)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM article_views_daily WHERE article_id IN ('a2', 'missing')`); err != nil || n != 0 {
		t.Errorf("views of removed articles = %d (%v), want 0", n, err)
	}
	stats, err := s.GetSiteStats(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Views != 3 {
		t.Errorf("site stats = %+v, want 3 views", stats)
	}
}

// jst は試験で使う日本時間（UTC+9。tzdata に依存しない固定オフセット）。
var jst = time.FixedZone("JST", 9*60*60)

// 既定のタイムゾーンは time.Local。
func TestDefaultLocationIsLocal(t *testing.T) {
	s := NewService(analyticsDB(t))
	defer s.Close()
	if s.Location() != time.Local {
		t.Fatalf("Location() = %v, want time.Local", s.Location())
	}
	s2 := NewService(analyticsDB(t), WithLocation(nil))
	defer s2.Close()
	if s2.Location() != time.Local {
		t.Fatalf("WithLocation(nil): Location() = %v, want time.Local", s2.Location())
	}
}

// 日次集計の日付は UTC ではなく、サービスのタイムゾーンの暦日で区切る。
// 同じ人の閲覧は、UTC の日付が変わってもローカルの同じ日なら 1 人と数え、ローカルの日付が変わったら別の日に数える。
func TestRecordViewUsesLocalDates(t *testing.T) {
	db := analyticsDB(t)
	now := time.Date(2026, 10, 19, 23, 50, 0, 0, time.UTC) // JST 10/20 08:50
	s := NewService(db, WithLocation(jst), WithClock(func() time.Time { return now }))
	view := func(at time.Time) {
		t.Helper()
		now = at
		if !s.RecordView("a1", "192.0.2.1", "Mozilla/5.0") {
			t.Fatalf("view at %v rejected", at)
		}
	}
	view(time.Date(2026, 10, 19, 23, 50, 0, 0, time.UTC)) // JST 10/20 08:50
	view(time.Date(2026, 10, 20, 0, 10, 0, 0, time.UTC))  // JST 10/20 09:10（UTC では翌日）
	view(time.Date(2026, 10, 20, 15, 10, 0, 0, time.UTC)) // JST 10/21 00:10（UTC では同じ日）
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var got []Stats
	if err := db.Select(&got, `SELECT date, views, unique_visitors FROM site_views_daily ORDER BY date`); err != nil {
		t.Fatal(err)
	}
	want := []Stats{{Date: "2026-10-20", Views: 2, UniqueVisitors: 1}, {Date: "2026-10-21", Views: 1, UniqueVisitors: 1}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("site_views_daily = %+v, want %+v", got, want)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM article_views_daily WHERE article_id = 'a1' AND date IN ('2026-10-20', '2026-10-21')`); err != nil || n != 2 {
		t.Errorf("article_views_daily rows on local dates = %d (%v), want 2", n, err)
	}
}

// 「過去 N 日」はサービスのタイムゾーンの今日を含む N 日で、今日より後の日付は含めない。
func TestTrailingDaysUseLocalToday(t *testing.T) {
	db := analyticsDB(t)
	for _, r := range []struct {
		date  string
		views int64
	}{{"2026-10-21", 9000}, {"2026-10-20", 5}, {"2026-10-14", 3}, {"2026-10-13", 700}} {
		if _, err := db.Exec(`INSERT INTO site_views_daily (date, views, unique_visitors) VALUES (?, ?, 1)`, r.date, r.views); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO article_views_daily (article_id, date, views, unique_visitors) VALUES ('a1', ?, ?, 1)`, r.date, r.views); err != nil {
			t.Fatal(err)
		}
	}
	// UTC では 10/19 だが、JST では 10/20 01:30。過去7日は 10/14〜10/20。
	now := time.Date(2026, 10, 19, 16, 30, 0, 0, time.UTC)
	s := NewService(db, WithLocation(jst), WithClock(func() time.Time { return now }))
	defer s.Close()
	stats, err := s.GetSiteStats(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 || stats[0].Date != "2026-10-20" || stats[1].Date != "2026-10-14" {
		t.Fatalf("site stats = %+v, want 10/20 and 10/14", stats)
	}
	popular, err := s.GetPopularArticles(5, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(popular) != 1 || popular[0].Views != 8 {
		t.Fatalf("popular = %+v, want a1 with 8 views", popular)
	}
}
