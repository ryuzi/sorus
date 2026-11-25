package backup

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/database"
)

// openV9DB はスキーマ 9 までの DB を作り、ユーザー名 username の管理者と設定 site_name を入れて返す。
func openV9DB(t *testing.T, path, username, siteName string) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("Connect(%s): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.MigrateTo(db, 9); err != nil {
		t.Fatalf("MigrateTo: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, 'x', 'admin')`,
		"id-"+username, username, username+" さん")
	mustExec(t, db, `INSERT INTO settings (key, value) VALUES ('site_name', ?)`, siteName)
	return db
}

// 予約投稿を状態 scheduled で持っていた古い版（スキーマ 9）のバックアップを復元すると、復元の Migrate で
// 公開日時が未来の公開記事に変わる（schedule_unify_spec の D2）。
func TestRestoreLiveMigratesScheduledArticlesFromOldBackup(t *testing.T) {
	dir := t.TempDir()
	src := openV9DB(t, filepath.Join(dir, "old.db"), "alice", "古い版")
	mustExec(t, src, `INSERT INTO categories (id, name, slug) VALUES (1, '未分類', 'uncategorized')`)
	at := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	mustExec(t, src, `INSERT INTO articles (id, title, slug, author_id, category_id, status, scheduled_at)
		VALUES ('a1', '予約', 'yoyaku', 'id-alice', 1, 'scheduled', ?)`, at)
	zipPath := filepath.Join(dir, "backups", FileName(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
	if err := Create(src, filepath.Join(dir, "uploads"), zipPath); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tg := newTarget(t)
	live, err := database.Connect(tg.db)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := RestoreLive(live, zipPath, tg.uploads); err != nil {
		t.Fatalf("RestoreLive: %v", err)
	}
	var row struct {
		Status      string     `db:"status"`
		PublishedAt *time.Time `db:"published_at"`
		ScheduledAt *time.Time `db:"scheduled_at"`
	}
	if err := live.Get(&row, `SELECT status, published_at, scheduled_at FROM articles WHERE id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	if row.Status != "published" || row.PublishedAt == nil || !row.PublishedAt.Equal(at) || row.ScheduledAt != nil {
		t.Errorf("restored article = %+v, want published at %v without scheduled_at", row, at)
	}
	var version int
	if err := live.Get(&version, "PRAGMA user_version"); err != nil || version != database.SchemaVersion() {
		t.Errorf("user_version = %d (%v), want %d", version, err, database.SchemaVersion())
	}
}
