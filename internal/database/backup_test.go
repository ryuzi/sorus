package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// dirNames は dir のエントリ名をソートして返す。
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// openBackup はバックアップファイルを開き、整合性を確認して返す。
func openBackup(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db, err := Connect(path)
	if err != nil {
		t.Fatalf("Connect(backup %s): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	assertPragma(t, db, "integrity_check", "ok")
	return db
}

func TestBackupCreatesConsistentCopy(t *testing.T) {
	db := openDB(t, "file")
	insertUser(t, db, "u1", "alice")
	catID := insertCategory(t, db, "News", "news")
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, scheduled_at, doi)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, "a1", "論文紹介", "paper", "u1", catID, "scheduled",
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "10.1234/example")

	dir := filepath.Join(t.TempDir(), "backups", "nested")
	dest := filepath.Join(dir, BackupFileName(time.Date(2026, 10, 20, 3, 4, 5, 0, time.UTC)))
	if err := Backup(db, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// 一時ファイルや WAL などの付随ファイルが残らず、バックアップ1つだけがあること。
	if got := dirNames(t, dir); len(got) != 1 || got[0] != "backup_20261020_030405.db" {
		t.Errorf("backup dir entries = %v, want [backup_20261020_030405.db]", got)
	}
	if runtime.GOOS != "windows" {
		assertFileMode(t, dest, 0o600)
	}

	b := openBackup(t, dest)
	var version int
	if err := b.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() {
		t.Errorf("backup user_version = %d, want %d", version, SchemaVersion())
	}
	var row struct {
		Title  string `db:"title"`
		Status string `db:"status"`
		DOI    string `db:"doi"`
	}
	if err := b.Get(&row, "SELECT title, status, doi FROM articles WHERE id = ?", "a1"); err != nil {
		t.Fatalf("select from backup: %v", err)
	}
	if row.Title != "論文紹介" || row.Status != "scheduled" || row.DOI != "10.1234/example" {
		t.Errorf("backup article = %+v", row)
	}

	// バックアップ後の変更は元DBにだけ反映され、バックアップは独立していること。
	mustExec(t, db, `DELETE FROM articles WHERE id = ?`, "a1")
	var n int
	if err := b.Get(&n, "SELECT COUNT(*) FROM articles"); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("backup articles count after deleting from source = %d, want 1", n)
	}
}

func TestBackupDoesNotBlockWriters(t *testing.T) {
	db := openDB(t, "file")
	db.SetMaxOpenConns(4)
	insertUser(t, db, "u1", "alice")
	catID := insertCategory(t, db, "News", "news")
	for i := 0; i < 200; i++ {
		mustExec(t, db, `INSERT INTO articles (id, title, slug, content_markdown, author_id, category_id) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("seed-%d", i), "T", fmt.Sprintf("seed-%d", i), fmt.Sprintf("%01000d", i), "u1", catID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var writes int
	var writeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
				fmt.Sprintf("w-%d", i), "T", fmt.Sprintf("w-%d", i), "u1", catID)
			if err != nil {
				writeErr = err
				return
			}
			writes++
		}
	}()

	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := Backup(db, filepath.Join(dir, fmt.Sprintf("online-%d.db", i))); err != nil {
			cancel()
			wg.Wait()
			t.Fatalf("Backup #%d during writes: %v", i, err)
		}
	}
	cancel()
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("concurrent write failed during backup: %v", writeErr)
	}
	if writes == 0 {
		t.Error("no writes completed while backing up")
	}

	for i := 0; i < 3; i++ {
		b := openBackup(t, filepath.Join(dir, fmt.Sprintf("online-%d.db", i)))
		var n int
		if err := b.Get(&n, "SELECT COUNT(*) FROM articles"); err != nil {
			t.Fatal(err)
		}
		if n < 200 || n > 200+writes {
			t.Errorf("backup #%d articles = %d, want between 200 and %d", i, n, 200+writes)
		}
	}
}

func TestBackupMemoryDatabase(t *testing.T) {
	db := openDB(t, "memory")
	insertUser(t, db, "u1", "alice")
	dest := filepath.Join(t.TempDir(), "mem.db")
	if err := Backup(db, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	var name string
	if err := openBackup(t, dest).Get(&name, "SELECT username FROM users WHERE id = ?", "u1"); err != nil {
		t.Fatalf("select from backup: %v", err)
	}
	if name != "alice" {
		t.Errorf("username = %q, want alice", name)
	}
}

func TestBackupRefusesExistingDestination(t *testing.T) {
	db := openDB(t, "memory")
	dir := t.TempDir()
	dest := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(dest, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Backup(db, dest); err == nil {
		t.Fatal("Backup over existing file succeeded, want error")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Errorf("existing file content = %q, want unchanged", got)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("dir entries = %v, want only existing.db", names)
	}
}

func TestBackupFailureLeavesNoFiles(t *testing.T) {
	db, err := Connect(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	dir := t.TempDir()
	if err := Backup(db, filepath.Join(dir, "x.db")); err == nil {
		t.Fatal("Backup with closed db succeeded, want error")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries after failed backup = %v, want none", names)
	}
}

func TestBackupRejectsInvalidArgs(t *testing.T) {
	if err := Backup(nil, filepath.Join(t.TempDir(), "x.db")); err == nil {
		t.Error("Backup(nil db) succeeded, want error")
	}
	if err := Backup(openDB(t, "memory"), ""); err == nil {
		t.Error(`Backup(destPath "") succeeded, want error`)
	}
}

func TestBackupFileName(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	got := BackupFileName(time.Date(2026, 10, 20, 9, 8, 7, 0, jst))
	if want := "backup_20261020_000807.db"; got != want {
		t.Errorf("BackupFileName = %q, want %q (UTC)", got, want)
	}
	if !backupNamePattern.MatchString(got) {
		t.Errorf("BackupFileName %q does not match the rotation pattern", got)
	}
}

func TestRotateBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	var backups []string
	// 作成順とファイルの更新時刻を逆にし、名前（時刻）だけで世代を判定することを確かめる。
	for i := 0; i < 10; i++ {
		name := BackupFileName(start.AddDate(0, 0, i))
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := start.AddDate(0, 0, -i)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		backups = append(backups, name)
	}
	others := []string{"backup_manual.db", "notes.txt", ".backup_20200101_000000.db.tmp-123", "backup_20200101_000000.db.bak"}
	for _, name := range others {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "backup_20000101_000000.db"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := RotateBackups(dir, 7); err != nil {
		t.Fatalf("RotateBackups: %v", err)
	}
	want := append(append([]string{}, backups[3:]...), others...)
	want = append(want, "backup_20000101_000000.db")
	sort.Strings(want)
	got := dirNames(t, dir)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after rotate = %v\nwant %v", got, want)
	}

	// 世代数が上限以下なら何も消さない。
	if err := RotateBackups(dir, 7); err != nil {
		t.Fatal(err)
	}
	if err := RotateBackups(dir, 100); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, dir); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("second rotate changed entries: %v", got)
	}
}

func TestRotateBackupsWithRealBackups(t *testing.T) {
	db := openDB(t, "file")
	dir := filepath.Join(t.TempDir(), "backups")
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if err := Backup(db, filepath.Join(dir, BackupFileName(start.Add(time.Duration(i)*24*time.Hour)))); err != nil {
			t.Fatal(err)
		}
		if err := RotateBackups(dir, 2); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"backup_20261022_000000.db", "backup_20261023_000000.db"}
	if got := dirNames(t, dir); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("backups = %v, want %v", got, want)
	}
}

func TestRotateBackupsArgs(t *testing.T) {
	if err := RotateBackups(t.TempDir(), 0); err == nil {
		t.Error("RotateBackups(keepCount 0) succeeded, want error")
	}
	if err := RotateBackups(filepath.Join(t.TempDir(), "missing"), 7); err != nil {
		t.Errorf("RotateBackups(missing dir) = %v, want nil", err)
	}
}
