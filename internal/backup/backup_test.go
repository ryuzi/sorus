package backup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/database"
)

// openDB は path にマイグレーション済みの DB を作り、ユーザー名 username の管理者と設定 site_name を入れて返す。
func openDB(t *testing.T, path, username, siteName string) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("Connect(%s): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, 'x', 'admin')`,
		"id-"+username, username, username+" さん")
	mustExec(t, db, `INSERT INTO settings (key, value) VALUES ('site_name', ?)`, siteName)
	return db
}

func mustExec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// writeFiles は dir の下に files（スラッシュ区切りの相対パス -> 内容）を作る。
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree は dir の下の通常ファイルを（スラッシュ区切りの相対パス -> 内容）で返す。
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return got
}

func assertTree(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := readTree(t, dir)
	if len(got) != len(want) {
		t.Errorf("%s files = %v, want %v", dir, keys(got), keys(want))
	}
	for rel, w := range want {
		if g, ok := got[rel]; !ok || g != w {
			t.Errorf("%s/%s = %q (exists=%v), want %q", dir, rel, g, ok, w)
		}
	}
}

func keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// queryValue は path の DB（停止中の DB として新しく開く）で query の1件目の値を返す。
func queryValue(t *testing.T, path, query string) string {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("Connect(%s): %v", path, err)
	}
	defer db.Close()
	var v string
	if err := db.Get(&v, query); err != nil {
		t.Fatalf("%s on %s: %v", query, path, err)
	}
	return v
}

var sourceFiles = map[string]string{
	"2026/10/photo-1200.webp":      "webp-large",
	"2026/10/photo-480.webp":       "webp-small",
	"favicon-0123456789abcdef.svg": `<svg xmlns="http://www.w3.org/2000/svg"/>`,
	"notes.txt":                    strings.Repeat("compressible text ", 200),
}

// newSource は source.db と uploads を用意し、完全バックアップを作ってそのパスを返す。
func newSource(t *testing.T) (zipPath string) {
	t.Helper()
	dir := t.TempDir()
	db := openDB(t, filepath.Join(dir, "source.db"), "alice", "バックアップ元メディア")
	uploads := filepath.Join(dir, "uploads")
	writeFiles(t, uploads, sourceFiles)
	if err := os.MkdirAll(filepath.Join(uploads, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath = filepath.Join(dir, "backups", FileName(time.Date(2026, 10, 22, 3, 4, 5, 0, time.UTC)))
	if err := Create(db, uploads, zipPath); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return zipPath
}

// target は復元先の DB と uploads。復元前の内容（bob・旧メディア・old.webp）を持つ。
type target struct {
	dir, db, uploads string
}

func newTarget(t *testing.T) target {
	t.Helper()
	dir := t.TempDir()
	tg := target{dir: dir, db: filepath.Join(dir, "database.db"), uploads: filepath.Join(dir, "uploads")}
	db := openDB(t, tg.db, "bob", "旧メディア")
	db.Close()
	writeFiles(t, tg.uploads, map[string]string{"old.webp": "old-image"})
	return tg
}

// assertUnchanged は復元に失敗した target が復元前のまま（退避先も一時ファイルも無い）であることを確かめる。
func (tg target) assertUnchanged(t *testing.T) {
	t.Helper()
	if got := queryValue(t, tg.db, "SELECT username FROM users"); got != "bob" {
		t.Errorf("target users = %q, want bob", got)
	}
	assertTree(t, tg.uploads, map[string]string{"old.webp": "old-image"})
	entries, err := os.ReadDir(tg.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), PreRestoreInfix) || strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover %s in target dir after failed restore", e.Name())
		}
	}
}

func TestCreateWritesCompleteArchive(t *testing.T) {
	zipPath := newSource(t)
	if filepath.Base(zipPath) != "sorus_backup_20261022_030405.zip" {
		t.Errorf("FileName = %s", filepath.Base(zipPath))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("archive perm = %o, want 600", perm)
		}
	}
	// 書き出し途中の一時ファイルや作業ディレクトリが残らない。
	entries, err := os.ReadDir(filepath.Dir(zipPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("backup dir entries = %v, want only the archive", entries)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := map[string]*zip.File{}
	for _, f := range zr.File {
		names[f.Name] = f
	}
	for _, want := range []string{ManifestName, DatabaseName, "uploads/", "uploads/empty-dir/",
		"uploads/2026/10/photo-1200.webp", "uploads/favicon-0123456789abcdef.svg", "uploads/notes.txt"} {
		if names[want] == nil {
			t.Errorf("archive has no entry %q (entries: %v)", want, zr.File)
		}
	}
	if f := names["uploads/2026/10/photo-1200.webp"]; f != nil && f.Method != zip.Store {
		t.Errorf("webp method = %d, want Store", f.Method)
	}
	if f := names["uploads/notes.txt"]; f != nil && f.Method != zip.Deflate {
		t.Errorf("txt method = %d, want Deflate", f.Method)
	}

	m, err := ReadManifest(zipPath)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	// 添付資料を含めない Create は、以前の sorus でも復元できる形式のバージョン1で作る。
	if m.Format != FormatName || m.FormatVersion != formatVersionNoAttachments || m.Attachments != nil ||
		m.SchemaVersion != database.SchemaVersion() {
		t.Errorf("manifest = %+v", m)
	}
	if m.AppVersion == "" || m.CreatedAt.IsZero() || time.Since(m.CreatedAt) > time.Minute {
		t.Errorf("manifest app_version/created_at = %q / %v", m.AppVersion, m.CreatedAt)
	}
	if m.Uploads.FileCount != len(sourceFiles) {
		t.Errorf("manifest file_count = %d, want %d", m.Uploads.FileCount, len(sourceFiles))
	}
	var total int64
	for _, c := range sourceFiles {
		total += int64(len(c))
	}
	if m.Uploads.TotalSize != total {
		t.Errorf("manifest total_size = %d, want %d", m.Uploads.TotalSize, total)
	}
	if f := names[DatabaseName]; f != nil {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		sum := sha256.Sum256(data)
		if m.Database.SHA256 != hex.EncodeToString(sum[:]) || m.Database.Size != int64(len(data)) {
			t.Errorf("manifest database = %+v, want size %d", m.Database, len(data))
		}
	}
}

func TestCreateExcludesLoginSessions(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, filepath.Join(dir, "source.db"), "alice", "x")
	const sessionID = "5e55105e55105e55105e55105e55105e55105e55105e55105e55105e55105e55"
	mustExec(t, db, `INSERT INTO sessions (id, user_id, expires_at) VALUES (?, 'id-alice', ?)`, sessionID, time.Now().Add(time.Hour).UTC())
	dest := filepath.Join(dir, "out.zip")
	if err := Create(db, filepath.Join(dir, "uploads"), dest); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var snapshot []byte
	for _, e := range readEntries(t, dest) {
		if e.name == DatabaseName {
			snapshot = e.data
		}
	}
	// 行を消すだけでなく、空き領域にもセッション ID が残らないこと。
	if bytes.Contains(snapshot, []byte(sessionID)) {
		t.Error("backup database still contains the session id")
	}
	tg := newTarget(t)
	if err := Restore(dest, tg.db, tg.uploads); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n := queryValue(t, tg.db, "SELECT COUNT(*) FROM sessions"); n != "0" {
		t.Errorf("restored sessions = %s, want 0", n)
	}
	// 稼働中の DB からはセッションを消さない。
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM sessions"); err != nil || n != 1 {
		t.Errorf("live sessions = %d (%v), want 1", n, err)
	}
}

func TestCreateRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, filepath.Join(dir, "source.db"), "alice", "x")
	dest := filepath.Join(dir, "out.zip")
	if err := os.WriteFile(dest, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Create(db, filepath.Join(dir, "uploads"), dest); err == nil {
		t.Fatal("Create over existing file succeeded, want error")
	}
	if data, _ := os.ReadFile(dest); string(data) != "keep" {
		t.Errorf("existing destination was overwritten: %q", data)
	}
}

func TestCreateWithoutUploadDir(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, filepath.Join(dir, "source.db"), "alice", "x")
	dest := filepath.Join(dir, "out.zip")
	if err := Create(db, filepath.Join(dir, "missing-uploads"), dest); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m, err := ReadManifest(dest)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Uploads.FileCount != 0 || m.Uploads.TotalSize != 0 {
		t.Errorf("uploads = %+v, want empty", m.Uploads)
	}
}

func TestCreateSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	dir := t.TempDir()
	db := openDB(t, filepath.Join(dir, "source.db"), "alice", "x")
	// UPLOAD_DIR 自体がリンクなら実体を辿り、中のリンクは辿らない。
	real := filepath.Join(dir, "data", "uploads")
	writeFiles(t, real, map[string]string{"a.webp": "a"})
	writeFiles(t, dir, map[string]string{"secret.txt": "secret"})
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(real, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "uploads")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.zip")
	if err := Create(db, link, dest); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m, err := ReadManifest(dest)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Uploads.FileCount != 1 {
		t.Errorf("file_count = %d, want 1 (a.webp only)", m.Uploads.FileCount)
	}
}

func TestRestoreReplacesDataAndKeepsPrevious(t *testing.T) {
	zipPath := newSource(t)
	tg := newTarget(t)

	if err := Restore(zipPath, tg.db, tg.uploads); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := queryValue(t, tg.db, "SELECT username FROM users"); got != "alice" {
		t.Errorf("restored users = %q, want alice", got)
	}
	if got := queryValue(t, tg.db, "SELECT value FROM settings WHERE key = 'site_name'"); got != "バックアップ元メディア" {
		t.Errorf("restored site_name = %q", got)
	}
	assertTree(t, tg.uploads, sourceFiles)
	if info, err := os.Stat(filepath.Join(tg.uploads, "empty-dir")); err != nil || !info.IsDir() {
		t.Errorf("empty-dir not restored: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(tg.db); err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("restored db perm = %o, want 600", info.Mode().Perm())
		}
		if info, err := os.Stat(filepath.Join(tg.uploads, "notes.txt")); err == nil && info.Mode().Perm() != 0o644 {
			t.Errorf("restored upload perm = %o, want 644", info.Mode().Perm())
		}
	}

	// 復元前の DB と uploads は .pre-restore-<日時> の名前で残り、一時ファイルは残らない。
	var prevDB, prevUploads string
	entries, err := os.ReadDir(tg.dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := regexp.MustCompile(`^\d{8}_\d{6}$`)
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "database.db"+PreRestoreInfix) && stamp.MatchString(strings.TrimPrefix(name, "database.db"+PreRestoreInfix)):
			prevDB = filepath.Join(tg.dir, name)
		case strings.HasPrefix(name, "uploads"+PreRestoreInfix) && stamp.MatchString(strings.TrimPrefix(name, "uploads"+PreRestoreInfix)):
			prevUploads = filepath.Join(tg.dir, name)
		case name == "database.db", name == "uploads", name == "database.db-wal", name == "database.db-shm":
		default:
			t.Errorf("unexpected entry %s in target dir", name)
		}
	}
	if prevDB == "" || prevUploads == "" {
		t.Fatalf("previous data not kept: entries = %v", entries)
	}
	if got := queryValue(t, prevDB, "SELECT username FROM users"); got != "bob" {
		t.Errorf("kept previous users = %q, want bob", got)
	}
	assertTree(t, prevUploads, map[string]string{"old.webp": "old-image"})
}

func TestRestoreIntoEmptyLocation(t *testing.T) {
	zipPath := newSource(t)
	dir := filepath.Join(t.TempDir(), "new-server")
	dbPath, uploads := filepath.Join(dir, "data", "database.db"), filepath.Join(dir, "uploads")
	if err := Restore(zipPath, dbPath, uploads); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := queryValue(t, dbPath, "SELECT username FROM users"); got != "alice" {
		t.Errorf("restored users = %q, want alice", got)
	}
	assertTree(t, uploads, sourceFiles)
}

func TestRestoreOverCorruptedDatabase(t *testing.T) {
	zipPath := newSource(t)
	dir := t.TempDir()
	dbPath, uploads := filepath.Join(dir, "database.db"), filepath.Join(dir, "uploads")
	// 壊れて開けなくなった DB からの復旧。壊れたファイルは退避先に残す。
	garbage := bytes.Repeat([]byte("broken database "), 512)
	if err := os.WriteFile(dbPath, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(zipPath, dbPath, uploads); err != nil {
		t.Fatalf("Restore over corrupted db: %v", err)
	}
	if got := queryValue(t, dbPath, "SELECT username FROM users"); got != "alice" {
		t.Errorf("restored users = %q, want alice", got)
	}
	prev, _ := filepath.Glob(dbPath + PreRestoreInfix + "*")
	if len(prev) != 1 {
		t.Fatalf("previous db = %v, want 1", prev)
	}
	if data, _ := os.ReadFile(prev[0]); !bytes.Equal(data, garbage) {
		t.Errorf("corrupted db was not kept as is")
	}
}

func TestRestoreRejectsDatabaseInUse(t *testing.T) {
	zipPath := newSource(t)
	tg := newTarget(t)
	// 稼働中のサーバーに相当する接続を開いておく。
	live, err := database.Connect(tg.db)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	var n int
	if err := live.Get(&n, "SELECT COUNT(*) FROM users"); err != nil {
		t.Fatal(err)
	}

	err = Restore(zipPath, tg.db, tg.uploads)
	if !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("Restore while in use: err = %v, want ErrDatabaseInUse", err)
	}
	var name string
	if err := live.Get(&name, "SELECT username FROM users"); err != nil || name != "bob" {
		t.Errorf("live db users = %q (%v), want bob", name, err)
	}
	assertTree(t, tg.uploads, map[string]string{"old.webp": "old-image"})
}

func TestRestoreLiveReplacesRunningDatabase(t *testing.T) {
	zipPath := newSource(t)
	tg := newTarget(t)
	live, err := database.Connect(tg.db)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	// プールに複数の接続を用意し、復元後にどの接続からも新しい中身が見えることを確かめる。
	tx, err := live.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := live.Get(&n, "SELECT COUNT(*) FROM users"); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()

	if err := RestoreLive(live, zipPath, tg.uploads); err != nil {
		t.Fatalf("RestoreLive: %v", err)
	}
	for i := 0; i < 3; i++ {
		var name string
		if err := live.Get(&name, "SELECT username FROM users"); err != nil || name != "alice" {
			t.Fatalf("live users = %q (%v), want alice", name, err)
		}
	}
	var mode string
	if err := live.Get(&mode, "PRAGMA journal_mode"); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (%v), want wal", mode, err)
	}
	var version int
	if err := live.Get(&version, "PRAGMA user_version"); err != nil || version != database.SchemaVersion() {
		t.Errorf("user_version = %d (%v), want %d", version, err, database.SchemaVersion())
	}
	mustExec(t, live, `INSERT INTO settings (key, value) VALUES ('after_restore', 'ok')`)
	assertTree(t, tg.uploads, sourceFiles)

	matches, _ := filepath.Glob(filepath.Join(tg.dir, "database.db"+PreRestoreInfix+"*"))
	if len(matches) != 1 {
		t.Fatalf("previous db snapshots = %v, want 1", matches)
	}
	if got := queryValue(t, matches[0], "SELECT username FROM users"); got != "bob" {
		t.Errorf("previous db users = %q, want bob", got)
	}
	matches, _ = filepath.Glob(filepath.Join(tg.dir, "uploads"+PreRestoreInfix+"*"))
	if len(matches) != 1 {
		t.Fatalf("previous uploads = %v, want 1", matches)
	}
	assertTree(t, matches[0], map[string]string{"old.webp": "old-image"})
	if leftovers, _ := filepath.Glob(filepath.Join(tg.dir, ".*")); len(leftovers) != 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}
}

func TestRestoreLiveRejectsInvalidArchiveWithoutChanges(t *testing.T) {
	tg := newTarget(t)
	live, err := database.Connect(tg.db)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	bad := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreLive(live, bad, tg.uploads); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("RestoreLive(bad): err = %v, want ErrInvalidArchive", err)
	}
	var name string
	if err := live.Get(&name, "SELECT username FROM users"); err != nil || name != "bob" {
		t.Errorf("live users = %q (%v), want bob", name, err)
	}
	live.Close()
	tg.assertUnchanged(t)
}

// entry はテスト用に組み立てるアーカイブのエントリ。
type entry struct {
	name string
	data []byte
	mode fs.FileMode
}

// readEntries は zipPath のエントリを読み出す。
func readEntries(t *testing.T, zipPath string) []entry {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var es []entry
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		es = append(es, entry{name: f.Name, data: data, mode: f.Mode()})
	}
	return es
}

// writeEntries は es を Zip として path に書く。圧縮しない（Store）ので、中身をそのままの位置で書き換えられる。
func writeEntries(t *testing.T, path string, es []entry) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range es {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// editManifest は es の manifest.json を edit で書き換える。
func editManifest(t *testing.T, es []entry, edit func(m *Manifest)) []entry {
	t.Helper()
	for i, e := range es {
		if e.name != ManifestName {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(e.data, &m); err != nil {
			t.Fatal(err)
		}
		edit(&m)
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		es[i].data = data
	}
	return es
}

func TestRestoreRejectsZipSlip(t *testing.T) {
	valid := readEntries(t, newSource(t))
	cases := []struct {
		name  string
		entry entry
	}{
		{"parent dir", entry{name: "../evil.txt", data: []byte("x")}},
		{"parent inside uploads", entry{name: "uploads/../../evil.txt", data: []byte("x")}},
		{"deep traversal", entry{name: "uploads/a/../../../evil.txt", data: []byte("x")}},
		{"absolute", entry{name: "/tmp/evil.txt", data: []byte("x")}},
		{"backslash traversal", entry{name: "uploads/..\\..\\evil.txt", data: []byte("x")}},
		{"windows drive", entry{name: "C:/evil.txt", data: []byte("x")}},
		{"drive inside uploads", entry{name: "uploads/C:evil.txt", data: []byte("x")}},
		{"empty segment", entry{name: "uploads//evil.txt", data: []byte("x")}},
		{"dot segment", entry{name: "uploads/./evil.txt", data: []byte("x")}},
		{"parent dir entry", entry{name: "uploads/../", mode: fs.ModeDir | 0o755}},
		{"nul byte", entry{name: "uploads/evil\x00.txt", data: []byte("x")}},
		{"symlink", entry{name: "uploads/link", data: []byte("/etc/passwd"), mode: fs.ModeSymlink | 0o777}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := newTarget(t)
			zipPath := filepath.Join(t.TempDir(), "slip.zip")
			// 悪意のあるエントリの他は正しいバックアップのまま（件数も合わせる）にし、名前の検査だけで拒否されることを確かめる。
			es := append(append([]entry{}, valid...), tc.entry)
			if !tc.entry.mode.IsDir() {
				es = editManifest(t, es, func(m *Manifest) {
					m.Uploads.FileCount++
					m.Uploads.TotalSize += int64(len(tc.entry.data))
				})
			}
			writeEntries(t, zipPath, es)

			if _, err := ReadManifest(zipPath); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("ReadManifest: err = %v, want ErrUnsafePath", err)
			}
			if err := Restore(zipPath, tg.db, tg.uploads); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("Restore: err = %v, want ErrUnsafePath", err)
			}
			tg.assertUnchanged(t)
			for _, p := range []string{filepath.Join(tg.dir, "evil.txt"), filepath.Join(filepath.Dir(tg.dir), "evil.txt"), "/tmp/evil.txt"} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s was written outside the destination", p)
				}
			}
		})
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	base := filepath.Join(t.TempDir(), "uploads")
	for _, rel := range []string{"../x", "a/../../x", "..", ""} {
		if _, err := safeJoin(base, rel); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("safeJoin(%q): err = %v, want ErrUnsafePath", rel, err)
		}
	}
	got, err := safeJoin(base, "2026/10/a.webp")
	if err != nil || got != filepath.Join(base, "2026", "10", "a.webp") {
		t.Errorf("safeJoin(valid) = %q, %v", got, err)
	}
}

func TestRestoreRejectsCorruptedOrMismatchedArchives(t *testing.T) {
	srcZip := newSource(t)
	raw, err := os.ReadFile(srcZip)
	if err != nil {
		t.Fatal(err)
	}
	valid := readEntries(t, srcZip)

	// without は name のエントリを除いた es を返す。
	without := func(name string) []entry {
		var es []entry
		for _, e := range valid {
			if e.name != name {
				es = append(es, e)
			}
		}
		return es
	}
	replaceData := func(name string, data []byte) []entry {
		es := append([]entry{}, valid...)
		for i := range es {
			if es[i].name == name {
				es[i].data = data
			}
		}
		return es
	}

	cases := []struct {
		name  string
		write func(t *testing.T, path string)
		want  error
	}{
		{"not a zip", func(t *testing.T, path string) {
			os.WriteFile(path, []byte("this is not a zip archive"), 0o600)
		}, ErrInvalidArchive},
		{"empty file", func(t *testing.T, path string) { os.WriteFile(path, nil, 0o600) }, ErrInvalidArchive},
		{"truncated", func(t *testing.T, path string) { os.WriteFile(path, raw[:len(raw)/2], 0o600) }, ErrInvalidArchive},
		{"flipped byte in stored upload", func(t *testing.T, path string) {
			writeEntries(t, path, valid)
			data, _ := os.ReadFile(path)
			i := bytes.Index(data, []byte("webp-large"))
			if i < 0 {
				t.Fatal("stored content not found")
			}
			data[i] ^= 0xff
			os.WriteFile(path, data, 0o600)
		}, ErrInvalidArchive},
		{"missing manifest", func(t *testing.T, path string) { writeEntries(t, path, without(ManifestName)) }, ErrInvalidArchive},
		{"missing database", func(t *testing.T, path string) { writeEntries(t, path, without(DatabaseName)) }, ErrInvalidArchive},
		{"broken manifest json", func(t *testing.T, path string) {
			writeEntries(t, path, replaceData(ManifestName, []byte("{broken")))
		}, ErrInvalidArchive},
		{"unknown format", func(t *testing.T, path string) {
			writeEntries(t, path, editManifest(t, append([]entry{}, valid...), func(m *Manifest) { m.Format = "other" }))
		}, ErrInvalidArchive},
		{"file count mismatch", func(t *testing.T, path string) {
			writeEntries(t, path, without("uploads/notes.txt"))
		}, ErrInvalidArchive},
		{"unexpected root entry", func(t *testing.T, path string) {
			writeEntries(t, path, append(append([]entry{}, valid...), entry{name: "extra.txt", data: []byte("x")}))
		}, ErrInvalidArchive},
		{"duplicate entry", func(t *testing.T, path string) {
			es := append(append([]entry{}, valid...), entry{name: "uploads/notes.txt", data: []byte("dup")})
			writeEntries(t, path, editManifest(t, es, func(m *Manifest) { m.Uploads.FileCount++; m.Uploads.TotalSize += 3 }))
		}, ErrInvalidArchive},
		{"database checksum mismatch", func(t *testing.T, path string) {
			es := without(DatabaseName)
			var db []byte
			for _, e := range valid {
				if e.name == DatabaseName {
					db = append([]byte{}, e.data...)
				}
			}
			db[len(db)-1] ^= 0xff
			writeEntries(t, path, append(es, entry{name: DatabaseName, data: db}))
		}, ErrInvalidArchive},
		{"database is not sqlite", func(t *testing.T, path string) {
			garbage := bytes.Repeat([]byte("garbage!"), 512)
			sum := sha256.Sum256(garbage)
			es := editManifest(t, replaceData(DatabaseName, garbage), func(m *Manifest) {
				m.Database.Size, m.Database.SHA256 = int64(len(garbage)), hex.EncodeToString(sum[:])
			})
			writeEntries(t, path, es)
		}, ErrInvalidArchive},
		{"newer format", func(t *testing.T, path string) {
			writeEntries(t, path, editManifest(t, append([]entry{}, valid...), func(m *Manifest) { m.FormatVersion = FormatVersion + 1 }))
		}, ErrIncompatible},
		{"newer schema", func(t *testing.T, path string) {
			writeEntries(t, path, editManifest(t, append([]entry{}, valid...), func(m *Manifest) { m.SchemaVersion = database.SchemaVersion() + 1 }))
		}, ErrIncompatible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := newTarget(t)
			zipPath := filepath.Join(t.TempDir(), "bad.zip")
			tc.write(t, zipPath)
			if err := Restore(zipPath, tg.db, tg.uploads); !errors.Is(err, tc.want) {
				t.Fatalf("Restore: err = %v, want %v", err, tc.want)
			}
			tg.assertUnchanged(t)
		})
	}
}

func TestRestoreMissingArchive(t *testing.T) {
	tg := newTarget(t)
	err := Restore(filepath.Join(t.TempDir(), "missing.zip"), tg.db, tg.uploads)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Restore(missing): err = %v, want not exist", err)
	}
	tg.assertUnchanged(t)
}
