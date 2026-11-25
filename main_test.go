package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/auth"
	"sorus/internal/backup"
	"sorus/internal/database"
	"sorus/internal/desktop"
	"sorus/internal/search"
	"sorus/internal/server"
	"sorus/internal/theme"

	"golang.org/x/net/websocket"
)

func TestParseImportArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		ok      bool
		wantErr bool
		want    importConfig
	}{
		{name: "server mode", args: nil, ok: false},
		{name: "server mode with other args", args: []string{"serve"}, ok: false},
		{name: "subcommand", args: []string{"import", "wp.xml"}, ok: true,
			want: importConfig{file: "wp.xml", downloadImages: true, accountMode: "current", defaultAuthor: "admin", timezone: "UTC"}},
		{name: "subcommand flags after file", args: []string{"import", "wp.xml", "--dry-run", "--download-images=false", "--default-author=editor"}, ok: true,
			want: importConfig{file: "wp.xml", dryRun: true, accountMode: "current", defaultAuthor: "editor", defaultAuthorSet: true, timezone: "UTC"}},
		{name: "author mode", args: []string{"import", "wp.xml", "--account-mode=authors"}, ok: true,
			want: importConfig{file: "wp.xml", downloadImages: true, accountMode: "authors", defaultAuthor: "admin", timezone: "UTC"}},
		{name: "default author before file", args: []string{"import", "--default-author", "admin", "wp.xml"}, ok: true,
			want: importConfig{file: "wp.xml", downloadImages: true, accountMode: "current", defaultAuthor: "admin", defaultAuthorSet: true, timezone: "UTC"}},
		{name: "flag form", args: []string{"--import=wp.xml", "--skip-pages", "--timezone=Asia/Tokyo"}, ok: true,
			want: importConfig{file: "wp.xml", downloadImages: true, accountMode: "current", defaultAuthor: "admin", skipPages: true, timezone: "Asia/Tokyo"}},
		{name: "flag form separate value", args: []string{"--import", "wp.xml"}, ok: true,
			want: importConfig{file: "wp.xml", downloadImages: true, accountMode: "current", defaultAuthor: "admin", timezone: "UTC"}},
		{name: "missing file", args: []string{"import"}, ok: true, wantErr: true},
		{name: "two files", args: []string{"import", "a.xml", "b.xml"}, ok: true, wantErr: true},
		{name: "unknown flag", args: []string{"import", "--nope", "a.xml"}, ok: true, wantErr: true},
		{name: "invalid account mode", args: []string{"import", "wp.xml", "--account-mode=other"}, ok: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := parseImportArgs(tt.args)
			if ok != tt.ok || (err != nil) != tt.wantErr {
				t.Fatalf("ok=%v err=%v, want ok=%v wantErr=%v", ok, err, tt.ok, tt.wantErr)
			}
			if ok && !tt.wantErr && got != tt.want {
				t.Errorf("cfg = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `# コメント行
TEST_CMS_KEY1=value1
export TEST_CMS_KEY2="value2"
TEST_CMS_KEY3='value3'
TEST_CMS_EXISTING=new_value
`
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TEST_CMS_EXISTING", "original_value")

	loadEnvFile(envPath)

	if got := os.Getenv("TEST_CMS_KEY1"); got != "value1" {
		t.Errorf("TEST_CMS_KEY1 = %q, want value1", got)
	}
	if got := os.Getenv("TEST_CMS_KEY2"); got != "value2" {
		t.Errorf("TEST_CMS_KEY2 = %q, want value2", got)
	}
	if got := os.Getenv("TEST_CMS_KEY3"); got != "value3" {
		t.Errorf("TEST_CMS_KEY3 = %q, want value3", got)
	}
	if got := os.Getenv("TEST_CMS_EXISTING"); got != "original_value" {
		t.Errorf("TEST_CMS_EXISTING = %q, want original_value (should not overwrite)", got)
	}
}

func TestRunImport_Integration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_import.db")
	searchDir := filepath.Join(dir, "search_index")
	uploadDir := filepath.Join(dir, "uploads")
	wxrPath := filepath.Join(dir, "test.wxr")

	t.Setenv("DB_PATH", dbPath)
	t.Setenv("SEARCH_INDEX_DIR", searchDir)
	t.Setenv("UPLOAD_DIR", uploadDir)
	t.Setenv("ADMIN_PASSWORD", "secret123456")

	sampleWXR := `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:wp="http://wordpress.org/export/1.2/">
<channel>
	<title>テストメディア</title>
	<link>https://example.com</link>
	<wp:wxr_version>1.2</wp:wxr_version>
	<item>
		<title>CLI インポートテスト記事</title>
		<link>https://example.com/2026/04/cli-import-test/</link>
		<dc:creator><![CDATA[admin]]></dc:creator>
		<content:encoded><![CDATA[<p>これは CLI 経由の WordPress WXR インポートの結合テスト本文です。</p>]]></content:encoded>
		<wp:post_name><![CDATA[cli-import-test]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
		<category domain="category" nicename="news"><![CDATA[ニュース]]></category>
	</item>
</channel>
</rss>`

	if err := os.WriteFile(wxrPath, []byte(sampleWXR), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := importConfig{
		file:           wxrPath,
		downloadImages: false,
		defaultAuthor:  "admin",
		dryRun:         false,
		skipPages:      false,
		timezone:       "UTC",
	}

	if err := runImport(cfg, io.Discard, io.Discard); err != nil {
		t.Fatalf("runImport failed: %v", err)
	}

	// DB の確認
	db := openDatabase()
	defer db.Close()

	var articleCount int
	if err := db.Get(&articleCount, "SELECT COUNT(*) FROM articles"); err != nil || articleCount != 1 {
		t.Errorf("articleCount = %d (%v), want 1", articleCount, err)
	}

	var redirectCount int
	if err := db.Get(&redirectCount, "SELECT COUNT(*) FROM redirects"); err != nil || redirectCount != 1 {
		t.Errorf("redirectCount = %d (%v), want 1", redirectCount, err)
	}

	var userCount int
	if err := db.Get(&userCount, "SELECT COUNT(*) FROM users"); err != nil || userCount != 1 {
		t.Errorf("userCount = %d (%v), want 1", userCount, err)
	}

	// 検索インデックスの確認
	idx, err := search.OpenOrCreateIndex(searchDir)
	if err != nil {
		t.Fatalf("open search index: %v", err)
	}
	defer idx.Close()

	res, err := idx.Search("インポート", 1, 10)
	if err != nil {
		t.Fatalf("search index search: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("search Total = %d, want 1", res.Total)
	}
}

func TestParseExportArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		ok      bool
		wantErr bool
		want    exportConfig
	}{
		{name: "server mode", args: nil, ok: false},
		{name: "other subcommand", args: []string{"import", "wp.xml"}, ok: false},
		{name: "export default", args: []string{"export"}, ok: true, want: exportConfig{outDir: "dist"}},
		{name: "export custom dir", args: []string{"export", "public_html"}, ok: true, want: exportConfig{outDir: "public_html"}},
		{name: "export theme override", args: []string{"export", "--theme", "news", "public_html"}, ok: true, want: exportConfig{outDir: "public_html", theme: "news"}},
		{name: "export clean", args: []string{"export", "--clean", "public_html"}, ok: true, want: exportConfig{outDir: "public_html", clean: true}},
		{name: "export clean off", args: []string{"export", "--clean=false", "--theme", "tech"}, ok: true, want: exportConfig{outDir: "dist", theme: "tech"}},
		{name: "export too many args", args: []string{"export", "a", "b"}, ok: true, wantErr: true},
		{name: "export legacy theme", args: []string{"export", "--theme", "mono"}, ok: true, wantErr: true},
		{name: "export unknown theme", args: []string{"export", "--theme", "../news", "out"}, ok: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := parseExportArgs(tt.args)
			if ok != tt.ok || (err != nil) != tt.wantErr {
				t.Fatalf("ok=%v err=%v, want ok=%v wantErr=%v", ok, err, tt.ok, tt.wantErr)
			}
			if ok && !tt.wantErr && got != tt.want {
				t.Errorf("cfg = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseExportArgsThemeChoices は、--theme の判定と選択肢の表示がテーマのカタログから作られることを確かめる。
func TestParseExportArgsThemeChoices(t *testing.T) {
	_, _, err := parseExportArgs([]string{"export", "--theme", "mono"})
	if err == nil || !strings.Contains(err.Error(), `"mono"`) || !strings.Contains(err.Error(), "modern, tech, news") {
		t.Errorf("error = %v, want unknown theme listing modern, tech, news", err)
	}
	// テーマはディレクトリを足すだけで増えるため、カタログと一致し、標準テーマを含むことを確かめる。
	got := exportThemeChoices()
	if want := strings.Join(theme.Builtin.IDs(), "|"); got != want {
		t.Errorf("exportThemeChoices() = %q, want %q", got, want)
	}
	for _, id := range []string{"modern", "tech", "news"} {
		if !slices.Contains(strings.Split(got, "|"), id) {
			t.Errorf("exportThemeChoices() = %q, want it to contain %q", got, id)
		}
	}
	if cfg, _, err := parseExportArgs([]string{"export", "--theme", "tech"}); err != nil || cfg.theme != "tech" {
		t.Errorf("--theme tech: cfg = %+v, err = %v", cfg, err)
	}
}

func TestParseBackupArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		ok      bool
		wantErr bool
		want    backupConfig
	}{
		{name: "server mode", args: nil, ok: false},
		{name: "other subcommand", args: []string{"import", "wp.xml"}, ok: false},
		{name: "backup not first", args: []string{"serve", "backup"}, ok: false},
		{name: "default destination", args: []string{"backup"}, ok: true, want: backupConfig{}},
		{name: "explicit destination", args: []string{"backup", "/tmp/out.db"}, ok: true, want: backupConfig{dest: "/tmp/out.db"}},
		{name: "db only", args: []string{"backup", "--db-only"}, ok: true, want: backupConfig{dbOnly: true}},
		{name: "db only with destination", args: []string{"backup", "--db-only", "/tmp/out"}, ok: true, want: backupConfig{dest: "/tmp/out", dbOnly: true}},
		{name: "too many arguments", args: []string{"backup", "a.db", "b.db"}, ok: true, wantErr: true},
		{name: "unknown flag", args: []string{"backup", "--nope"}, ok: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := parseBackupArgs(tt.args)
			if ok != tt.ok || (err != nil) != tt.wantErr {
				t.Fatalf("ok=%v err=%v, want ok=%v wantErr=%v", ok, err, tt.ok, tt.wantErr)
			}
			if ok && !tt.wantErr && got != tt.want {
				t.Errorf("cfg = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// setupBackupSource は DB_PATH に初期管理者入りの DB を作り、そのパスを返す。
func setupBackupSource(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "source.db")
	t.Setenv("DB_PATH", dbPath)
	// 添付資料の既定（カレントディレクトリの attachments）をテストで読み書きしないよう、DB の隣に置く。
	t.Setenv("ATTACHMENT_DIR", filepath.Join(filepath.Dir(dbPath), "attachments"))
	t.Setenv("ADMIN_PASSWORD", "secret123456")
	db := openDatabase()
	if _, err := db.Exec("UPDATE users SET display_name = ? WHERE username = 'admin'", "バックアップ確認"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return dbPath
}

// assertBackupContents は path のバックアップに setupBackupSource のデータが入っているかを確かめる。
func assertBackupContents(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("backup not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup perm = %o, want 600", perm)
	}
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer db.Close()
	var name string
	if err := db.Get(&name, "SELECT display_name FROM users WHERE username = 'admin'"); err != nil || name != "バックアップ確認" {
		t.Errorf("display_name = %q (%v), want バックアップ確認", name, err)
	}
}

func TestRunBackup_Integration(t *testing.T) {
	dbPath := setupBackupSource(t)
	dest := filepath.Join(t.TempDir(), "nested", "manual.db")

	var out bytes.Buffer
	if err := runBackup(backupConfig{dest: dest}, &out); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	assertBackupContents(t, dest)
	for _, want := range []string{"Backup completed", "Source:      " + dbPath, "Destination: " + dest, "bytes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, out.String())
		}
	}

	// 既存のバックアップは上書きしない。
	if err := runBackup(backupConfig{dest: dest}, &bytes.Buffer{}); err == nil {
		t.Error("runBackup to existing file succeeded, want error")
	}
}

func TestRunBackupCompleteWarnsAboutSecrets(t *testing.T) {
	setupBackupSource(t)
	dest := filepath.Join(t.TempDir(), "complete.zip")
	var out bytes.Buffer
	if err := runBackup(backupConfig{dest: dest}, &out); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	for _, want := range []string{
		"This backup contains passwords and API keys.",
		"Do not share it.",
		"To share the site, download the static site as a Zip.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("backup warning missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "manuscript") || strings.Contains(out.String(), ".sorus") {
		t.Errorf("backup warning mentions manuscript files:\n%s", out.String())
	}
}

func TestRunBackupDBOnlyWarnsAboutSecrets(t *testing.T) {
	setupBackupSource(t)
	dest := filepath.Join(t.TempDir(), "database.db")
	var out bytes.Buffer
	if err := runBackup(backupConfig{dbOnly: true, dest: dest}, &out); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	for _, want := range []string{
		"This backup contains passwords and API keys.",
		"Do not share it.",
		"To share the site, download the static site as a Zip.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("database-only backup warning missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "manuscript") || strings.Contains(out.String(), ".sorus") {
		t.Errorf("database-only backup warning mentions manuscript files:\n%s", out.String())
	}
}

func TestRunBackup_DefaultDestination(t *testing.T) {
	setupBackupSource(t)
	uploadDir := filepath.Join(t.TempDir(), "uploads")
	writeTestFile(t, filepath.Join(uploadDir, "2026", "photo.webp"), "photo")
	t.Setenv("UPLOAD_DIR", uploadDir)
	backupDir := filepath.Join(t.TempDir(), "backups")
	t.Setenv("BACKUP_DIR", backupDir)

	var out bytes.Buffer
	if err := runBackup(backupConfig{}, &out); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	// 既定ではデータベースと画像をまとめた完全バックアップ（Zip）を作る。
	if len(entries) != 1 || !regexp.MustCompile(`^sorus_backup_\d{8}_\d{6}\.zip$`).MatchString(entries[0].Name()) {
		t.Fatalf("backup dir entries = %v, want one sorus_backup_YYYYMMDD_HHMMSS.zip", entries)
	}
	m, err := backup.ReadManifest(filepath.Join(backupDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Uploads.FileCount != 1 {
		t.Errorf("uploads file_count = %d, want 1", m.Uploads.FileCount)
	}
	if !strings.Contains(out.String(), "Uploads:     "+uploadDir+" (1 files, 5 bytes)") {
		t.Errorf("summary missing uploads:\n%s", out.String())
	}
}

func TestRunBackup_DBOnlyDefaultDestination(t *testing.T) {
	setupBackupSource(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	t.Setenv("BACKUP_DIR", backupDir)

	if err := runBackup(backupConfig{dbOnly: true}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !regexp.MustCompile(`^backup_\d{8}_\d{6}\.db$`).MatchString(entries[0].Name()) {
		t.Fatalf("backup dir entries = %v, want one backup_YYYYMMDD_HHMMSS.db", entries)
	}
	assertBackupContents(t, filepath.Join(backupDir, entries[0].Name()))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseRestoreArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		ok      bool
		wantErr bool
		want    restoreConfig
	}{
		{name: "server mode", args: nil, ok: false},
		{name: "other subcommand", args: []string{"backup", "a.zip"}, ok: false},
		{name: "restore", args: []string{"restore", "a.zip"}, ok: true, want: restoreConfig{src: "a.zip"}},
		{name: "restore yes", args: []string{"restore", "--yes", "a.zip"}, ok: true, want: restoreConfig{src: "a.zip", yes: true}},
		{name: "missing file", args: []string{"restore"}, ok: true, wantErr: true},
		{name: "too many arguments", args: []string{"restore", "a.zip", "b.zip"}, ok: true, wantErr: true},
		{name: "unknown flag", args: []string{"restore", "--nope", "a.zip"}, ok: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := parseRestoreArgs(tt.args)
			if ok != tt.ok || (err != nil) != tt.wantErr {
				t.Fatalf("ok=%v err=%v, want ok=%v wantErr=%v", ok, err, tt.ok, tt.wantErr)
			}
			if ok && !tt.wantErr && got != tt.want {
				t.Errorf("cfg = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// adminDisplayName は dbPath の DB の初期管理者の表示名を返す。
func adminDisplayName(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := database.Connect(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.Get(&name, "SELECT display_name FROM users WHERE username = 'admin'"); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestRunRestore_Integration(t *testing.T) {
	dbPath := setupBackupSource(t)
	uploadDir := filepath.Join(filepath.Dir(dbPath), "uploads")
	writeTestFile(t, filepath.Join(uploadDir, "kept.webp"), "kept")
	t.Setenv("UPLOAD_DIR", uploadDir)
	zipPath := filepath.Join(t.TempDir(), "full.zip")
	if err := runBackup(backupConfig{dest: zipPath}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runBackup: %v", err)
	}

	// バックアップ後の変更。
	db, err := database.Connect(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE users SET display_name = 'バックアップ後' WHERE username = 'admin'"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.Remove(filepath.Join(uploadDir, "kept.webp"))
	writeTestFile(t, filepath.Join(uploadDir, "after.webp"), "after")

	// yes 以外の入力では何も変えない。
	var out bytes.Buffer
	if err := runRestore(restoreConfig{src: zipPath}, strings.NewReader("no\n"), &out); !errors.Is(err, errRestoreCancelled) {
		t.Fatalf("runRestore(no): err = %v, want errRestoreCancelled", err)
	}
	if got := adminDisplayName(t, dbPath); got != "バックアップ後" {
		t.Errorf("display_name after cancel = %q", got)
	}
	for _, want := range []string{"Backup to restore", "Uploads:     1 files, 4 bytes", "Database:    " + dbPath, "Type yes to restore"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runRestore(restoreConfig{src: zipPath}, strings.NewReader("yes\n"), &out); err != nil {
		t.Fatalf("runRestore(yes): %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Restore completed") {
		t.Errorf("output missing completion:\n%s", out.String())
	}
	if got := adminDisplayName(t, dbPath); got != "バックアップ確認" {
		t.Errorf("display_name after restore = %q, want バックアップ確認", got)
	}
	if data, err := os.ReadFile(filepath.Join(uploadDir, "kept.webp")); err != nil || string(data) != "kept" {
		t.Errorf("kept.webp = %q (%v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, "after.webp")); !os.IsNotExist(err) {
		t.Errorf("after.webp still exists (err=%v)", err)
	}
	if prev, _ := filepath.Glob(dbPath + backup.PreRestoreInfix + "*"); len(prev) != 1 {
		t.Errorf("previous db copies = %v, want 1", prev)
	}

	// --yes は確認を省く。稼働中のサーバーに相当する接続があれば復元しない。
	live, err := database.Connect(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	var n int
	if err := live.Get(&n, "SELECT COUNT(*) FROM users"); err != nil {
		t.Fatal(err)
	}
	if err := runRestore(restoreConfig{src: zipPath, yes: true}, strings.NewReader(""), &bytes.Buffer{}); !errors.Is(err, backup.ErrDatabaseInUse) {
		t.Errorf("runRestore while in use: err = %v, want ErrDatabaseInUse", err)
	}
}

// sorus backup は添付資料も含めた完全バックアップ（形式のバージョン2）を作り、sorus restore はそれを添付資料まで戻す。
func TestRunBackupRestore_Attachments(t *testing.T) {
	dbPath := setupBackupSource(t)
	dir := filepath.Dir(dbPath)
	uploadDir := filepath.Join(dir, "uploads")
	attachDir := filepath.Join(dir, "attachments")
	t.Setenv("UPLOAD_DIR", uploadDir)
	writeTestFile(t, filepath.Join(uploadDir, "photo.webp"), "photo")
	writeTestFile(t, filepath.Join(attachDir, "paper.pdf"), "paper")
	zipPath := filepath.Join(t.TempDir(), "full.zip")

	var out bytes.Buffer
	if err := runBackup(backupConfig{dest: zipPath}, &out); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	m, err := backup.ReadManifest(zipPath)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.FormatVersion != 2 || m.Attachments == nil || m.Attachments.FileCount != 1 {
		t.Fatalf("manifest = format v%d attachments %+v, want v2 with 1 attachment", m.FormatVersion, m.Attachments)
	}
	if !strings.Contains(out.String(), "Attachments: "+attachDir+" (1 files, 5 bytes)") {
		t.Errorf("backup summary missing attachments:\n%s", out.String())
	}

	// バックアップ後に添付資料を消し、別のものを置く。
	os.Remove(filepath.Join(attachDir, "paper.pdf"))
	writeTestFile(t, filepath.Join(attachDir, "after.pdf"), "after")

	out.Reset()
	if err := runRestore(restoreConfig{src: zipPath, yes: true}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runRestore: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Attachments: 1 files, 5 bytes", "Attachments: " + attachDir} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("restore output missing %q:\n%s", want, out.String())
		}
	}
	if data, err := os.ReadFile(filepath.Join(attachDir, "paper.pdf")); err != nil || string(data) != "paper" {
		t.Errorf("paper.pdf = %q (%v), want restored", data, err)
	}
	if _, err := os.Stat(filepath.Join(attachDir, "after.pdf")); !os.IsNotExist(err) {
		t.Errorf("after.pdf still exists (err=%v)", err)
	}
	if prev, _ := filepath.Glob(attachDir + backup.PreRestoreInfix + "*"); len(prev) != 1 {
		t.Errorf("previous attachment dirs = %v, want 1", prev)
	}
}

// 添付資料を含まない以前の形式（バージョン1）のバックアップも sorus restore で戻せる。添付資料のディレクトリには触れない。
func TestRunRestore_FormatV1(t *testing.T) {
	dbPath := setupBackupSource(t)
	dir := filepath.Dir(dbPath)
	uploadDir := filepath.Join(dir, "uploads")
	attachDir := filepath.Join(dir, "attachments")
	t.Setenv("UPLOAD_DIR", uploadDir)
	writeTestFile(t, filepath.Join(uploadDir, "photo.webp"), "photo")
	writeTestFile(t, filepath.Join(attachDir, "paper.pdf"), "paper")
	zipPath := filepath.Join(t.TempDir(), "v1.zip")
	db, err := database.Connect(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	err = backup.Create(db, uploadDir, zipPath)
	db.Close()
	if err != nil {
		t.Fatalf("backup.Create: %v", err)
	}
	if m, err := backup.ReadManifest(zipPath); err != nil || m.FormatVersion != 1 {
		t.Fatalf("manifest = %+v (%v), want format v1", m, err)
	}
	os.Remove(filepath.Join(uploadDir, "photo.webp"))

	var out bytes.Buffer
	if err := runRestore(restoreConfig{src: zipPath, yes: true}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runRestore: %v\n%s", err, out.String())
	}
	if data, err := os.ReadFile(filepath.Join(uploadDir, "photo.webp")); err != nil || string(data) != "photo" {
		t.Errorf("photo.webp = %q (%v), want restored", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(attachDir, "paper.pdf")); err != nil || string(data) != "paper" {
		t.Errorf("paper.pdf = %q (%v), want untouched", data, err)
	}
	if prev, _ := filepath.Glob(attachDir + backup.PreRestoreInfix + "*"); len(prev) != 0 {
		t.Errorf("attachment dir was swapped for a v1 backup: %v", prev)
	}
}

func TestRunRestore_RejectsInvalidInput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPLOAD_DIR", filepath.Join(dir, "uploads"))
	bad := filepath.Join(dir, "bad.zip")
	writeTestFile(t, bad, "not a zip")

	t.Setenv("DB_PATH", "file:"+filepath.Join(dir, "x.db")+"?mode=rw")
	if err := runRestore(restoreConfig{src: bad, yes: true}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "DB_PATH") {
		t.Errorf("runRestore with URI DB_PATH: err = %v", err)
	}
	t.Setenv("DB_PATH", filepath.Join(dir, "x.db"))
	if err := runRestore(restoreConfig{src: bad, yes: true}, strings.NewReader(""), &bytes.Buffer{}); !errors.Is(err, backup.ErrInvalidArchive) {
		t.Errorf("runRestore(bad zip): err = %v, want ErrInvalidArchive", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.db")); !os.IsNotExist(err) {
		t.Errorf("failed restore created the database (err=%v)", err)
	}
}

func TestRunBackup_MissingSource(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "missing.db")
	t.Setenv("DB_PATH", dbPath)
	dest := filepath.Join(dir, "out.db")

	if err := runBackup(backupConfig{dest: dest}, &bytes.Buffer{}); err == nil {
		t.Fatal("runBackup with missing source succeeded, want error")
	}
	for _, p := range []string{dbPath, dest} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after failed backup (err=%v)", p, err)
		}
	}

	// file: URI 指定でも実ファイルが存在しなければエラーになること
	t.Setenv("DB_PATH", "file:"+filepath.Join(dir, "missing_uri.db")+"?mode=rw")
	if err := runBackup(backupConfig{dest: dest}, &bytes.Buffer{}); err == nil {
		t.Fatal("runBackup with missing file: URI succeeded, want error")
	}
}

func TestAutocertConfigFromEnv(t *testing.T) {
	t.Setenv("AUTOCERT_DOMAIN", "")
	if _, ok := autocertConfigFromEnv(); ok {
		t.Fatal("ok = true without AUTOCERT_DOMAIN")
	}

	t.Setenv("AUTOCERT_DOMAIN", " cms.example.ac.jp , www.example.ac.jp ,")
	t.Setenv("AUTOCERT_EMAIL", "ops@example.ac.jp")
	t.Setenv("AUTOCERT_CACHE_DIR", "")
	t.Setenv("AUTOCERT_HTTP_ADDR", "")
	t.Setenv("AUTOCERT_HTTPS_ADDR", ":8443")
	cfg, ok := autocertConfigFromEnv()
	if !ok {
		t.Fatal("ok = false with AUTOCERT_DOMAIN")
	}
	if !slices.Equal(cfg.domains, []string{"cms.example.ac.jp", "www.example.ac.jp"}) {
		t.Errorf("domains = %q", cfg.domains)
	}
	if cfg.email != "ops@example.ac.jp" || cfg.cacheDir != "certs" || cfg.httpAddr != ":80" || cfg.httpsAddr != ":8443" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestNewAutocertServers(t *testing.T) {
	cfg := autocertConfig{
		domains:   []string{"cms.example.ac.jp"},
		cacheDir:  filepath.Join(t.TempDir(), "certs"),
		httpAddr:  ":80",
		httpsAddr: ":443",
	}
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("app")) })
	httpSrv, httpsSrv := newAutocertServers(app, cfg)

	if httpSrv.Addr != ":80" || httpsSrv.Addr != ":443" {
		t.Errorf("addrs = %q, %q", httpSrv.Addr, httpsSrv.Addr)
	}
	if httpsSrv.Handler == nil || httpsSrv.TLSConfig == nil || httpsSrv.TLSConfig.GetCertificate == nil {
		t.Fatal("HTTPS server is missing handler or autocert TLS config")
	}
	if !slices.Contains(httpsSrv.TLSConfig.NextProtos, "acme-tls/1") {
		t.Errorf("NextProtos = %q, want acme-tls/1 for TLS-ALPN-01", httpsSrv.TLSConfig.NextProtos)
	}
	// HTTPS サーバーにも、通常のサーバー（server.New）と同じ待ち時間の上限を付ける（低速な接続による資源の占有を防ぐ）。
	if httpsSrv.ReadHeaderTimeout != 10*time.Second || httpsSrv.ReadTimeout != 30*time.Second ||
		httpsSrv.WriteTimeout != 60*time.Second || httpsSrv.IdleTimeout != 120*time.Second {
		t.Errorf("HTTPS timeouts = header %v, read %v, write %v, idle %v; want 10s, 30s, 1m, 2m (same as the normal server)",
			httpsSrv.ReadHeaderTimeout, httpsSrv.ReadTimeout, httpsSrv.WriteTimeout, httpsSrv.IdleTimeout)
	}

	// ポート 80: チャレンジ以外は HTTPS へリダイレクトする。
	rec := httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://cms.example.ac.jp/articles/hello?p=1", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://cms.example.ac.jp/articles/hello?p=1" {
		t.Errorf("redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// 許可していないホストのチャレンジは拒否する。
	rec = httptest.NewRecorder()
	httpSrv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://evil.example.com/.well-known/acme-challenge/token", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("challenge for unknown host = %d, want 403", rec.Code)
	}

	// 許可していない SNI には証明書を発行しない（ACME への問い合わせ前に拒否される）。
	if _, err := httpsSrv.TLSConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Error("GetCertificate for unknown host succeeded, want error")
	}
}

// 自動 HTTPS のサーバーは TLS の上で HTTP/2 も使う。待ち時間の上限を付けても、長くつなぐ通信が通常のサーバーと同じく
// 動くことを確かめる。期限を延ばす応答（MCP のストリーム・同期・バックアップと同じく http.ResponseController を使う）は
// HTTP/2 でも上限を超えて続けられ、WebSocket（デスクトップ版の画面の数え方と同じく接続を乗っ取る。ブラウザは HTTP/1.1
// でつなぐ）は上限を超えてもやり取りできる。期限を延ばさない応答は上限で切れる（上限が効いている）。
// 上限は試験を速くするため短くする（値そのものは TestNewAutocertServers で確かめる）。
func TestAutocertHTTPSServerLongConnections(t *testing.T) {
	const limit = 200 * time.Millisecond
	stream := func(extend bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			if extend {
				if err := rc.SetWriteDeadline(time.Time{}); err != nil {
					t.Errorf("SetWriteDeadline: %v", err)
				}
			}
			for i := range 3 {
				fmt.Fprintf(w, "chunk %d\n", i)
				_ = rc.Flush()
				time.Sleep(limit)
			}
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/stream", stream(true))
	mux.Handle("/cut", stream(false))
	mux.Handle("/ws", websocket.Handler(func(ws *websocket.Conn) { _, _ = io.Copy(ws, ws) }))
	_, httpsSrv := newAutocertServers(mux, autocertConfig{domains: []string{"127.0.0.1"}, cacheDir: t.TempDir(), httpsAddr: "127.0.0.1:0"})
	httpsSrv.ReadHeaderTimeout, httpsSrv.ReadTimeout, httpsSrv.WriteTimeout = limit, limit, limit

	// 証明書は ACME の代わりに httptest の自己署名の証明書を使う。ALPN（h2 など）は autocert の設定のまま。
	certSrc := httptest.NewTLSServer(http.NotFoundHandler())
	certSrc.Close()
	tlsCfg := httpsSrv.TLSConfig.Clone()
	tlsCfg.GetCertificate = nil
	tlsCfg.Certificates = certSrc.TLS.Certificates
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = httpsSrv.Serve(tls.NewListener(ln, tlsCfg)) }()
	t.Cleanup(func() { _ = httpsSrv.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(certSrc.Certificate())
	base := "https://" + ln.Addr().String()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true}}
	get := func(path string) (*http.Response, string, error) {
		res, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		return res, string(b), err
	}

	res, body, err := get("/stream")
	if err != nil || strings.Count(body, "chunk") != 3 {
		t.Errorf("long stream: body %q, err %v; want all 3 chunks", body, err)
	}
	if res.ProtoMajor != 2 {
		t.Errorf("long stream: protocol %s, want HTTP/2", res.Proto)
	}
	if _, body, err := get("/cut"); err == nil && strings.Count(body, "chunk") == 3 {
		t.Error("a response that does not extend its deadline was not cut by WriteTimeout")
	}

	wsCfg, err := websocket.NewConfig("wss://"+ln.Addr().String()+"/ws", base)
	if err != nil {
		t.Fatal(err)
	}
	wsCfg.TlsConfig = &tls.Config{RootCAs: pool}
	ws, err := websocket.DialConfig(wsCfg)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer ws.Close()
	for i := range 2 {
		if i > 0 {
			time.Sleep(3 * limit) // 読み書きの上限を超えて待ってからもう一度やり取りする。
		}
		msg := fmt.Sprintf("ping %d", i)
		_ = ws.SetDeadline(time.Now().Add(5 * time.Second))
		var got string
		if err := websocket.Message.Send(ws, msg); err != nil {
			t.Fatalf("websocket send %d: %v", i, err)
		}
		if err := websocket.Message.Receive(ws, &got); err != nil || got != msg {
			t.Fatalf("websocket receive %d = %q, %v; want %q", i, got, err, msg)
		}
	}
}

func TestNewPlainHealthCheckHandler(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("sensitive content"))
	})

	handler := newPlainHealthCheckHandler(app, "cms.example.ac.jp")

	// /healthz はそのまま 200 OK を返すこと
	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("/healthz response = %d %q, want 200 'ok'", rec.Code, rec.Body.String())
	}

	// /login やその他のパスは平文でアクセスさせず、HTTPS に 301 リダイレクトすること
	for _, path := range []string{"/login", "/admin", "/articles/1", "/", "/articles?page=2&tag=go"} {
		req = httptest.NewRequest(http.MethodGet, "http://localhost:8080"+path, nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s response status = %d, want 301 Moved Permanently", path, rec.Code)
		}
		wantLocation := "https://cms.example.ac.jp" + path
		if loc := rec.Header().Get("Location"); loc != wantLocation {
			t.Errorf("%s redirect location = %q, want %q", path, loc, wantLocation)
		}
	}
}

// security_hardening_spec.md §2.5: LISTEN_ADDR・BIND_ADDR で --server・--public-only の待ち受けアドレスを絞れる。
func TestListenAddrFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, listen, bind, want string
		wantErr                  bool
	}{
		{name: "default is every address", want: ":8080"},
		{name: "LISTEN_ADDR", listen: "127.0.0.1:9000", want: "127.0.0.1:9000"},
		{name: "BIND_ADDR", bind: "127.0.0.1:9001", want: "127.0.0.1:9001"},
		{name: "LISTEN_ADDR wins", listen: "127.0.0.1:9000", bind: "0.0.0.0:9001", want: "127.0.0.1:9000"},
		{name: "address only uses PORT", bind: " 127.0.0.1 ", want: "127.0.0.1:8080"},
		{name: "localhost", listen: "localhost", want: "localhost:8080"},
		{name: "ipv6 loopback", listen: "::1", want: "[::1]:8080"},
		{name: "bracketed ipv6", listen: "[::1]", want: "[::1]:8080"},
		{name: "bracketed ipv6 with port", listen: "[::1]:9000", want: "[::1]:9000"},
		{name: "port only", listen: ":9000", want: ":9000"},
		{name: "bad port", listen: "127.0.0.1:http", wantErr: true},
		{name: "port out of range", listen: "127.0.0.1:70000", wantErr: true},
		{name: "garbage", bind: "127.0.0.1:80:90", wantErr: true},
		{name: "url", listen: "http://127.0.0.1:8080", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LISTEN_ADDR", tc.listen)
			t.Setenv("BIND_ADDR", tc.bind)
			got, err := listenAddrFromEnv("8080")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("listenAddrFromEnv = %q, want error", got)
				}
				if !strings.Contains(err.Error(), "LISTEN_ADDR") && !strings.Contains(err.Error(), "BIND_ADDR") {
					t.Errorf("error %q does not name the variable", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("listenAddrFromEnv = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// 127.0.0.1 に絞ったアドレスは、実際にこの PC のループバックだけで待ち受ける。
func TestListenAddrBindsLoopbackOnly(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("BIND_ADDR", "127.0.0.1")
	addr, err := listenAddrFromEnv("0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Errorf("listening on %v, want loopback only", ln.Addr())
	}
}

func TestAutocertPlainAddr(t *testing.T) {
	t.Setenv("AUTOCERT_PLAIN_ADDR", "")
	if got, err := autocertPlainAddr(":8080", "8080"); err != nil || got != ":8080" {
		t.Errorf("default = %q, %v; want :8080", got, err)
	}
	// LISTEN_ADDR・BIND_ADDR で絞ったアドレスを、平文のヘルスチェックのポートにも使う。
	if got, err := autocertPlainAddr("127.0.0.1:8080", "8080"); err != nil || got != "127.0.0.1:8080" {
		t.Errorf("from listen addr = %q, %v; want 127.0.0.1:8080", got, err)
	}
	t.Setenv("AUTOCERT_PLAIN_ADDR", "127.0.0.1")
	if got, err := autocertPlainAddr(":8080", "8080"); err != nil || got != "127.0.0.1:8080" {
		t.Errorf("AUTOCERT_PLAIN_ADDR = %q, %v; want 127.0.0.1:8080", got, err)
	}
	t.Setenv("AUTOCERT_PLAIN_ADDR", "127.0.0.1:9090")
	if got, err := autocertPlainAddr(":8080", "8080"); err != nil || got != "127.0.0.1:9090" {
		t.Errorf("AUTOCERT_PLAIN_ADDR with port = %q, %v; want 127.0.0.1:9090", got, err)
	}
	t.Setenv("AUTOCERT_PLAIN_ADDR", "nope nope")
	if _, err := autocertPlainAddr(":8080", "8080"); err == nil || !strings.Contains(err.Error(), "AUTOCERT_PLAIN_ADDR") {
		t.Errorf("invalid AUTOCERT_PLAIN_ADDR: err = %v, want error naming the variable", err)
	}
}

func TestNewPlainHealthCheckServer(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	srv := newPlainHealthCheckServer(app, autocertConfig{domains: []string{"cms.example.ac.jp"}}, "127.0.0.1:8080")
	if srv.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want 127.0.0.1:8080", srv.Addr)
	}
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Errorf("timeouts = header %v, idle %v", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/admin", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "https://cms.example.ac.jp/admin" {
		t.Errorf("redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestListenURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080": "http://localhost:8080", "0.0.0.0:8080": "http://localhost:8080", "[::]:8080": "http://localhost:8080",
		"127.0.0.1:8080": "http://127.0.0.1:8080", "[::1]:9000": "http://[::1]:9000",
	} {
		if got := listenURL(addr); got != want {
			t.Errorf("listenURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestSplitEnvList(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", " example.jp , ,www.example.jp,")
	if got := splitEnvList("ALLOWED_HOSTS"); !slices.Equal(got, []string{"example.jp", "www.example.jp"}) {
		t.Errorf("splitEnvList = %q", got)
	}
	t.Setenv("ALLOWED_HOSTS", "")
	if got := splitEnvList("ALLOWED_HOSTS"); got != nil {
		t.Errorf("splitEnvList(empty) = %q, want nil", got)
	}
}

func TestParseServeArgs(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		args    []string
		want    bool
		wantErr bool
	}{
		{name: "default", want: false},
		{name: "flag", args: []string{"--public-only"}, want: true},
		{name: "single dash flag", args: []string{"-public-only"}, want: true},
		{name: "env true", env: "true", want: true},
		{name: "env 1", env: "1", want: true},
		{name: "env false", env: "false", want: false},
		{name: "flag overrides env", env: "true", args: []string{"--public-only=false"}, want: false},
		// 綴りの誤りで管理画面を公開したまま起動しないよう、読めない値は起動を止める。
		{name: "env typo", env: "ture", wantErr: true},
		{name: "unknown flag", args: []string{"--public-onyl"}, wantErr: true},
		{name: "unknown command", args: []string{"serve"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PUBLIC_ONLY", tt.env)
			t.Setenv("CAPABILITY_KEY_FILE", "")
			t.Setenv("SYNC_API_PATH", "")
			got, err := parseServeArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && (got.publicOnly != tt.want || got.keyFile != defaultCapabilityKeyFile || got.syncPath != "") {
				t.Errorf("cfg = %+v, want publicOnly=%v keyFile=%s syncPath=\"\"", got, tt.want, defaultCapabilityKeyFile)
			}
		})
	}
	t.Run("key file from env", func(t *testing.T) {
		t.Setenv("PUBLIC_ONLY", "")
		t.Setenv("CAPABILITY_KEY_FILE", "/data/keys/cap.key")
		t.Setenv("SYNC_API_PATH", "")
		got, err := parseServeArgs(nil)
		if err != nil || got.keyFile != "/data/keys/cap.key" {
			t.Errorf("cfg = %+v, err = %v", got, err)
		}
	})
	t.Run("sync path", func(t *testing.T) {
		t.Setenv("PUBLIC_ONLY", "")
		t.Setenv("CAPABILITY_KEY_FILE", "")
		for _, tc := range []struct {
			env  string
			args []string
			want string
		}{
			{env: "/api/sync-abc", want: "/api/sync-abc"},
			{env: " api/sync-abc/ ", want: "/api/sync-abc"},
			{env: "  ", want: ""},
			{args: []string{"--sync-path", "/hidden/sync"}, want: "/hidden/sync"},
			{env: "/api/sync-env", args: []string{"--public-only", "--sync-path=/api/sync-flag"}, want: "/api/sync-flag"},
		} {
			t.Setenv("SYNC_API_PATH", tc.env)
			got, err := parseServeArgs(tc.args)
			if err != nil || got.syncPath != tc.want {
				t.Errorf("env %q args %q: cfg = %+v, err = %v; want syncPath %q", tc.env, tc.args, got, err, tc.want)
			}
		}
		// 管理画面と重なるパスや Echo のルート記法を含むパスでは起動しない。
		for _, bad := range []string{"/admin/sync", "/api/:id", "/api/../sync", "/"} {
			t.Setenv("SYNC_API_PATH", bad)
			if got, err := parseServeArgs(nil); err == nil || !strings.Contains(err.Error(), "SYNC_API_PATH") {
				t.Errorf("SYNC_API_PATH=%q: cfg = %+v, err = %v; want an error naming SYNC_API_PATH", bad, got, err)
			}
		}
	})
}

func TestParseTokenArgs(t *testing.T) {
	t.Setenv("CAPABILITY_KEY_FILE", "")
	t.Setenv("SYNC_API_PATH", "")
	t.Setenv("BASE_URL", "")
	tests := []struct {
		name    string
		args    []string
		ok      bool
		wantErr bool
		want    tokenConfig
	}{
		{name: "server mode", args: nil, ok: false},
		{name: "other subcommand", args: []string{"backup"}, ok: false},
		{name: "create", args: []string{"token", "create", "--role", "writer", "--sub", "yamada"}, ok: true,
			want: tokenConfig{role: "writer", sub: "yamada", ttl: 90 * 24 * time.Hour, keyFile: defaultCapabilityKeyFile}},
		{name: "all flags", args: []string{"token", "create", "--role=editor", "--sub=  佐藤 デスク ", "--ttl=12h", "--key-file=/tmp/k"}, ok: true,
			want: tokenConfig{role: "editor", sub: "佐藤 デスク", ttl: 12 * time.Hour, keyFile: "/tmp/k"}},
		{name: "days ttl", args: []string{"token", "create", "--role", "admin", "--sub", "root", "--ttl", "30d"}, ok: true,
			want: tokenConfig{role: "admin", sub: "root", ttl: 30 * 24 * time.Hour, keyFile: defaultCapabilityKeyFile}},
		{name: "sync path and base url", args: []string{"token", "create", "--role", "writer", "--sub", "a", "--sync-path", "api/sync-abc/", "--base-url", "https://example.com/"}, ok: true,
			want: tokenConfig{role: "writer", sub: "a", ttl: 90 * 24 * time.Hour, keyFile: defaultCapabilityKeyFile, syncPath: "/api/sync-abc", baseURL: "https://example.com"}},
		{name: "invalid sync path", args: []string{"token", "create", "--role", "writer", "--sub", "a", "--sync-path", "/admin/x"}, ok: true, wantErr: true},
		{name: "invalid base url", args: []string{"token", "create", "--role", "writer", "--sub", "a", "--base-url", "example.com"}, ok: true, wantErr: true},
		{name: "base url with query", args: []string{"token", "create", "--role", "writer", "--sub", "a", "--base-url", "https://example.com/?a=1"}, ok: true, wantErr: true},
		{name: "missing subcommand", args: []string{"token"}, ok: true, wantErr: true},
		{name: "unknown subcommand", args: []string{"token", "revoke"}, ok: true, wantErr: true},
		{name: "missing role", args: []string{"token", "create", "--sub", "yamada"}, ok: true, wantErr: true},
		{name: "invalid role", args: []string{"token", "create", "--role", "owner", "--sub", "yamada"}, ok: true, wantErr: true},
		{name: "reader role is not issuable", args: []string{"token", "create", "--role", "reader", "--sub", "yamada"}, ok: true, wantErr: true},
		{name: "missing sub", args: []string{"token", "create", "--role", "writer"}, ok: true, wantErr: true},
		{name: "blank sub", args: []string{"token", "create", "--role", "writer", "--sub", "  "}, ok: true, wantErr: true},
		{name: "extra argument", args: []string{"token", "create", "--role", "writer", "--sub", "a", "b"}, ok: true, wantErr: true},
		{name: "unknown flag", args: []string{"token", "create", "--role", "writer", "--sub", "a", "--nope"}, ok: true, wantErr: true},
		{name: "invalid ttl", args: []string{"token", "create", "--role", "writer", "--sub", "a", "--ttl", "soon"}, ok: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := parseTokenArgs(tt.args)
			if ok != tt.ok || (err != nil) != tt.wantErr {
				t.Fatalf("ok=%v err=%v, want ok=%v wantErr=%v", ok, err, tt.ok, tt.wantErr)
			}
			if ok && !tt.wantErr && got != tt.want {
				t.Errorf("cfg = %+v, want %+v", got, tt.want)
			}
		})
	}
	// 公開サーバーと同じ .env（SYNC_API_PATH・BASE_URL）を読めば、同じ接続先を案内できる。
	t.Run("sync path and base url from env", func(t *testing.T) {
		t.Setenv("SYNC_API_PATH", "/api/sync-env")
		t.Setenv("BASE_URL", "https://media.example.jp")
		got, ok, err := parseTokenArgs([]string{"token", "create", "--role", "writer", "--sub", "a"})
		if !ok || err != nil || got.syncPath != "/api/sync-env" || got.baseURL != "https://media.example.jp" {
			t.Errorf("cfg = %+v, ok = %v, err = %v", got, ok, err)
		}
	})
}

func TestParseTokenTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"1d": 24 * time.Hour, "90d": 90 * 24 * time.Hour, "3650d": maxTokenTTL, "1s": time.Second, "36h": 36 * time.Hour,
	} {
		if got, err := parseTokenTTL(in); err != nil || got != want {
			t.Errorf("parseTokenTTL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "0d", "-1d", "3651d", "1.5d", "500ms", "0s", "-1h", "87601h", "99999999999999999d", "forever"} {
		if got, err := parseTokenTTL(in); err == nil {
			t.Errorf("parseTokenTTL(%q) = %v, want error", in, got)
		}
	}
}

func TestLoadOrCreateCapabilityKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "capability.key")
	if _, err := loadCapabilityKey(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadCapabilityKey(missing) err = %v, want ErrNotExist", err)
	}

	priv, created, err := loadOrCreateCapabilityKey(path)
	if err != nil || !created || len(priv) != ed25519.PrivateKeySize {
		t.Fatalf("create: len=%d created=%v err=%v", len(priv), created, err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("key file permission = %o, want 600", perm)
		}
	}

	again, created, err := loadOrCreateCapabilityKey(path)
	if err != nil || created || !again.Equal(priv) {
		t.Fatalf("reload: created=%v err=%v equal=%v, want the same key", created, err, again.Equal(priv))
	}
	loaded, err := loadCapabilityKey(path)
	if err != nil || !loaded.Equal(priv) {
		t.Fatalf("loadCapabilityKey: err=%v equal=%v", err, loaded.Equal(priv))
	}

	// 壊れた鍵は作り直さずにエラーにする（作り直すと発行済みのトークンがすべて使えなくなる）。
	if err := os.WriteFile(path, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateCapabilityKey(path); err == nil || !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("corrupt key err = %v, want ErrInvalidKey", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "not-a-key\n" {
		t.Errorf("corrupt key file was overwritten: %q", data)
	}
}

// 所有者以外が読み書きできる既存の鍵は、漏洩している恐れがあるため読み込まない。
// Windows の権限ビットは読み取り専用属性だけを表すため検査せず、どの権限でも読み込む。
func TestLoadCapabilityKeyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capability.key")
	priv, _, err := loadOrCreateCapabilityKey(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })

	for _, perm := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666, 0o700} {
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		_, loadErr := loadCapabilityKey(path)
		if runtime.GOOS == "windows" {
			if loadErr != nil {
				t.Errorf("perm %o: %v, want the key loaded on windows", perm, loadErr)
			}
			continue
		}
		if perm&0o077 == 0 {
			if loadErr != nil {
				t.Errorf("perm %o: %v, want the key loaded", perm, loadErr)
			}
			continue
		}
		if !errors.Is(loadErr, errInsecureKeyFile) {
			t.Fatalf("perm %o: loadCapabilityKey err = %v, want errInsecureKeyFile", perm, loadErr)
		}
		if msg := loadErr.Error(); !strings.Contains(msg, "chmod 600") || !strings.Contains(msg, path) {
			t.Errorf("perm %o: message does not tell how to fix it: %s", perm, msg)
		}
		if _, _, err := loadOrCreateCapabilityKey(path); !errors.Is(err, errInsecureKeyFile) {
			t.Errorf("perm %o: loadOrCreateCapabilityKey err = %v, want errInsecureKeyFile", perm, err)
		}
		for _, publicOnly := range []bool{false, true} {
			if _, err := syncPublicKey(serveConfig{publicOnly: publicOnly, keyFile: path}); !errors.Is(err, errInsecureKeyFile) {
				t.Errorf("perm %o publicOnly=%v: syncPublicKey err = %v, want errInsecureKeyFile", perm, publicOnly, err)
			}
		}
		var stdout bytes.Buffer
		cfg := tokenConfig{role: "writer", sub: "yamada", ttl: time.Hour, keyFile: path}
		if err := runTokenCreate(cfg, &stdout, io.Discard); !errors.Is(err, errInsecureKeyFile) || stdout.Len() != 0 {
			t.Errorf("perm %o: runTokenCreate err = %v stdout = %q, want refusal without a token", perm, err, stdout.String())
		}
		// 拒否した鍵を作り直したり、権限を勝手に変えたりしない。
		if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
			t.Errorf("perm %o: key file was rewritten", perm)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != perm {
			t.Errorf("perm %o: key file mode changed: %v %v", perm, st.Mode(), err)
		}
	}

	for _, perm := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		if loaded, err := loadCapabilityKey(path); err != nil || !loaded.Equal(priv) {
			t.Errorf("perm %o: err=%v equal=%v, want the key loaded", perm, err, loaded.Equal(priv))
		}
	}
}

// 鍵のパスがディレクトリなどの通常のファイルでなければ読み込まない。
func TestLoadCapabilityKeyRejectsNonRegularFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capability.key")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCapabilityKey(dir); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("loadCapabilityKey(dir) err = %v, want a non-regular file error", err)
	}
	if _, _, err := loadOrCreateCapabilityKey(dir); err == nil {
		t.Error("loadOrCreateCapabilityKey(dir) succeeded")
	}
}

func TestRunTokenCreate(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "capability.key")
	var stdout, stderr bytes.Buffer
	cfg := tokenConfig{role: "writer", sub: "yamada", ttl: 48 * time.Hour, keyFile: keyFile}
	if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
		t.Fatalf("runTokenCreate: %v", err)
	}
	token := strings.TrimSpace(stdout.String())
	if strings.Count(stdout.String(), "\n") != 1 || strings.Count(token, ".") != 2 {
		t.Fatalf("stdout must be only the token line, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Created a new signing key") || !strings.Contains(stderr.String(), "/admin/settings/remote") {
		t.Errorf("stderr = %q", stderr.String())
	}

	priv, err := loadCapabilityKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := auth.VerifyToken(priv.Public().(ed25519.PublicKey), token)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if claims.Subject != "yamada" || claims.Role != auth.RoleWriter {
		t.Errorf("claims = %+v", claims)
	}
	if exp := time.Unix(claims.ExpiresAt, 0); exp.Before(time.Now().Add(47*time.Hour)) || exp.After(time.Now().Add(49*time.Hour)) {
		t.Errorf("expires at %v, want about 48h from now", exp)
	}

	// 2回目は同じ鍵で署名する（公開サーバーが検証できる）。
	stdout.Reset()
	stderr.Reset()
	cfg.role, cfg.sub = "editor", "sato"
	if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "Created a new signing key") {
		t.Error("second run created a new key")
	}
	if claims, err := auth.VerifyToken(priv.Public().(ed25519.PublicKey), strings.TrimSpace(stdout.String())); err != nil || claims.Role != auth.RoleEditor {
		t.Errorf("second token: %+v, %v", claims, err)
	}
}

func TestSyncPublicKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "capability.key")

	// 公開専用モードでなく鍵も無ければ、同期 API は無効（鍵を作らない）。
	pub, err := syncPublicKey(serveConfig{keyFile: keyFile})
	if err != nil || pub != nil {
		t.Fatalf("normal mode without key: pub=%v err=%v", pub, err)
	}
	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal mode created a key file: %v", err)
	}

	// 公開専用モードでは鍵を作成し、token create と同じ鍵で検証する。
	pub, err = syncPublicKey(serveConfig{publicOnly: true, keyFile: keyFile})
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("public-only: pub=%v err=%v", pub, err)
	}
	var stdout bytes.Buffer
	if err := runTokenCreate(tokenConfig{role: "editor", sub: "sato", ttl: time.Hour, keyFile: keyFile}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.VerifyToken(pub, strings.TrimSpace(stdout.String())); err != nil {
		t.Errorf("token from CLI does not verify with the server key: %v", err)
	}

	// 鍵があれば通常モードでも同期 API を有効にする。
	again, err := syncPublicKey(serveConfig{keyFile: keyFile})
	if err != nil || !again.Equal(pub) {
		t.Errorf("normal mode with key: equal=%v err=%v", again.Equal(pub), err)
	}

	if err := os.WriteFile(keyFile, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, publicOnly := range []bool{false, true} {
		if _, err := syncPublicKey(serveConfig{publicOnly: publicOnly, keyFile: keyFile}); err == nil {
			t.Errorf("publicOnly=%v: broken key accepted", publicOnly)
		}
	}
}

func TestLoadOrCreateSyncAPIPath(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys", "capability.key")
	path := syncPathFile(keyFile)
	if path != filepath.Join(dir, "keys", "sync_path.txt") {
		t.Fatalf("syncPathFile(%q) = %q, want sync_path.txt next to the key", keyFile, path)
	}
	if _, err := loadSyncAPIPath(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadSyncAPIPath(missing) err = %v, want ErrNotExist", err)
	}

	p, created, err := loadOrCreateSyncAPIPath(path)
	if err != nil || !created || !regexp.MustCompile(`^/api/sync-[0-9a-f]{32}$`).MatchString(p) {
		t.Fatalf("create: path=%q created=%v err=%v", p, created, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != p+"\n" {
		t.Fatalf("file content = %q, %v; want %q", data, err, p+"\n")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("sync path file permission = %o, want 600", perm)
		}
	}

	again, created, err := loadOrCreateSyncAPIPath(path)
	if err != nil || created || again != p {
		t.Fatalf("reload: path=%q created=%v err=%v, want %q", again, created, err, p)
	}
	// resolveSyncAPIPath は指定されたパスを優先し、無ければ署名鍵の隣のファイルを使う。
	if got, created, err := resolveSyncAPIPath("", keyFile); err != nil || created || got != p {
		t.Errorf("resolveSyncAPIPath(\"\") = %q, %v, %v; want %q", got, created, err, p)
	}
	otherKey := filepath.Join(dir, "other", "capability.key")
	if got, created, err := resolveSyncAPIPath("/api/sync-explicit", otherKey); err != nil || created || got != "/api/sync-explicit" {
		t.Errorf("resolveSyncAPIPath(explicit) = %q, %v, %v", got, created, err)
	}
	if _, err := os.Stat(syncPathFile(otherKey)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("explicit sync path created a file: %v", err)
	}

	// 壊れたファイルは作り直さずにエラーにする（作り直すとローカル CMS に設定済みの URL が使えなくなる）。
	for _, broken := range []string{"", "\n", "/admin/sync\n", "not a path\n"} {
		if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _, err := loadOrCreateSyncAPIPath(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("broken %q: path=%q err=%v, want an error naming the file", broken, got, err)
		}
		if data, _ := os.ReadFile(path); string(data) != broken {
			t.Errorf("broken %q: file was overwritten with %q", broken, data)
		}
	}
	// 手で書いたパスも、検査して正規化して使う。
	if err := os.WriteFile(path, []byte("  /hidden/sync-endpoint/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadSyncAPIPath(path); err != nil || got != "/hidden/sync-endpoint" {
		t.Errorf("hand-written path = %q, %v", got, err)
	}
}

// インポートは、サーバーと同じ同期 API のパス（SYNC_API_PATH か署名鍵の隣の sync_path.txt）を読み、作成はしない。
// その先頭部分のスラッグを持つ固定ページには接尾辞を付けて取り込む。
func TestImportSyncAPIPath(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "capability.key")
	t.Setenv("CAPABILITY_KEY_FILE", keyFile)
	t.Setenv("SYNC_API_PATH", "")
	if p, err := importSyncAPIPath(); err != nil || p != server.DefaultSyncAPIPath {
		t.Errorf("without settings = %q, %v; want %q", p, err, server.DefaultSyncAPIPath)
	}
	if _, err := os.Stat(syncPathFile(keyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("import created the sync path file: %v", err)
	}
	if err := os.WriteFile(syncPathFile(keyFile), []byte("/hooks/sync\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := importSyncAPIPath(); err != nil || p != "/hooks/sync" {
		t.Errorf("from file = %q, %v; want /hooks/sync", p, err)
	}
	t.Setenv("SYNC_API_PATH", "/env/sync")
	if p, err := importSyncAPIPath(); err != nil || p != "/env/sync" {
		t.Errorf("from SYNC_API_PATH = %q, %v; want /env/sync", p, err)
	}
	t.Setenv("SYNC_API_PATH", "not a path")
	if _, err := importSyncAPIPath(); err == nil {
		t.Error("invalid SYNC_API_PATH was accepted")
	}

	t.Setenv("SYNC_API_PATH", "/hooks/sync")
	t.Setenv("DB_PATH", filepath.Join(dir, "import.db"))
	t.Setenv("SEARCH_INDEX_DIR", filepath.Join(dir, "search_index"))
	t.Setenv("UPLOAD_DIR", filepath.Join(dir, "uploads"))
	t.Setenv("ADMIN_PASSWORD", "secret123456")
	wxrPath := filepath.Join(dir, "pages.wxr")
	wxr := `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:wp="http://wordpress.org/export/1.2/">
<channel>
	<title>研究室</title>
	<link>https://example.com</link>
	<wp:wxr_version>1.2</wp:wxr_version>
	<item>
		<title>連携</title>
		<link>https://example.com/hooks/</link>
		<content:encoded><![CDATA[<p>外部との連携です。</p>]]></content:encoded>
		<wp:post_name><![CDATA[hooks]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[page]]></wp:post_type>
	</item>
</channel>
</rss>`
	if err := os.WriteFile(wxrPath, []byte(wxr), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runImport(importConfig{file: wxrPath, defaultAuthor: "admin", timezone: "UTC"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	db := openDatabase()
	defer db.Close()
	var slugs []string
	if err := db.Select(&slugs, `SELECT slug FROM articles WHERE kind = 'page'`); err != nil || len(slugs) != 1 || slugs[0] != "hooks-page" {
		t.Errorf("page slugs = %q (%v), want [hooks-page]", slugs, err)
	}
}

// 同期 API のパスのファイルも、署名鍵と同じく所有者以外が読み書きできるなら読み込まない。
func TestLoadSyncAPIPathPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), syncPathFileName)
	p, _, err := loadOrCreateSyncAPIPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	for _, perm := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		got, loadErr := loadSyncAPIPath(path)
		if runtime.GOOS == "windows" {
			if loadErr != nil || got != p {
				t.Errorf("perm %o: %q, %v; want the path loaded on windows", perm, got, loadErr)
			}
			continue
		}
		if !errors.Is(loadErr, errInsecureKeyFile) {
			t.Fatalf("perm %o: loadSyncAPIPath err = %v, want errInsecureKeyFile", perm, loadErr)
		}
		if msg := loadErr.Error(); !strings.Contains(msg, "chmod 600") || !strings.Contains(msg, path) {
			t.Errorf("perm %o: message does not tell how to fix it: %s", perm, msg)
		}
		if _, _, err := loadOrCreateSyncAPIPath(path); !errors.Is(err, errInsecureKeyFile) {
			t.Errorf("perm %o: loadOrCreateSyncAPIPath err = %v, want errInsecureKeyFile", perm, err)
		}
		if data, _ := os.ReadFile(path); string(data) != p+"\n" {
			t.Errorf("perm %o: file was rewritten: %q", perm, data)
		}
	}
	for _, perm := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		if got, err := loadSyncAPIPath(path); err != nil || got != p {
			t.Errorf("perm %o: %q, %v; want %q", perm, got, err, p)
		}
	}
}

// sorus token create は公開サーバーと同じ同期 API のパス（sync_path.txt または SYNC_API_PATH）を案内する。
func TestRunTokenCreateSyncGuidance(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "capability.key")
	cfg := tokenConfig{role: "writer", sub: "yamada", ttl: time.Hour, keyFile: keyFile}

	var stdout, stderr bytes.Buffer
	if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	p, err := loadSyncAPIPath(syncPathFile(keyFile))
	if err != nil {
		t.Fatalf("token create did not save the sync path: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Created a new secret sync API path: "+syncPathFile(keyFile)) ||
		!strings.Contains(out, "Sync path: "+p+"\n") || !strings.Contains(out, "https://example.com"+p) ||
		!strings.Contains(out, "/admin/settings/remote") || strings.Contains(out, "Sync URL:") {
		t.Errorf("stderr without base url = %q", out)
	}
	if strings.Count(stdout.String(), "\n") != 1 || strings.Contains(stdout.String(), p) {
		t.Errorf("stdout must be only the token line, got %q", stdout.String())
	}

	// 2回目は同じパスを案内し、--base-url があれば完全な URL を案内する。
	stdout.Reset()
	stderr.Reset()
	cfg.baseURL = "https://media.example.jp"
	if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	out = stderr.String()
	if strings.Contains(out, "Created a new secret sync API path") || !strings.Contains(out, "Sync URL:  https://media.example.jp"+p+"\n") {
		t.Errorf("stderr with base url = %q", out)
	}

	// SYNC_API_PATH（--sync-path）を指定すれば、そのパスを案内する。
	stderr.Reset()
	cfg.syncPath = "/api/sync-from-env"
	if err := runTokenCreate(cfg, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Sync URL:  https://media.example.jp/api/sync-from-env\n") {
		t.Errorf("stderr with explicit sync path = %q", stderr.String())
	}

	// パスのファイルが読めなければ、トークンを発行しない。
	if err := os.WriteFile(syncPathFile(keyFile), []byte("/admin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	cfg.syncPath = ""
	if err := runTokenCreate(cfg, &stdout, io.Discard); err == nil || stdout.Len() != 0 {
		t.Errorf("broken sync path file: err = %v stdout = %q, want refusal without a token", err, stdout.String())
	}
}

// 公開専用モードのサーバーと sorus token create が同じ秘匿パスを使い、案内した URL で同期できる。
// 固定の /api/sync は存在しないパスと同じ 404 になる。
func TestSecretSyncPathServerAndTokenAgree(t *testing.T) {
	dir := t.TempDir()
	serveCfg := serveConfig{publicOnly: true, keyFile: filepath.Join(dir, "capability.key")}
	pub, err := syncPublicKey(serveCfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := syncAPIPath(serveCfg)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := syncAPIPath(serveCfg); err != nil || again != p {
		t.Fatalf("restart: path = %q, %v; want the saved %q", again, err, p)
	}

	db, err := database.Connect(filepath.Join(dir, "public.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{DB: db, UploadDir: filepath.Join(dir, "uploads"), AttachmentDir: filepath.Join(dir, "attachments"),
		DisableScheduler: true, PublicOnly: true, SyncPublicKey: pub, SyncAPIPath: p})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		srv.Shutdown(context.Background())
	})

	var stdout, stderr bytes.Buffer
	cfg := tokenConfig{role: "editor", sub: "sato", ttl: time.Hour, keyFile: serveCfg.keyFile, baseURL: ts.URL}
	if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`Sync URL:\s+(\S+)`).FindStringSubmatch(stderr.String())
	if m == nil || m[1] != ts.URL+p {
		t.Fatalf("token create guided %q, want %s", stderr.String(), ts.URL+p)
	}
	token := strings.TrimSpace(stdout.String())

	post := func(target string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"action":"fetch_pending"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := post(m[1]); got != http.StatusOK {
		t.Errorf("POST guided sync URL = %d, want 200", got)
	}
	if got := post(ts.URL + server.DefaultSyncAPIPath); got != http.StatusNotFound {
		t.Errorf("POST fixed %s = %d, want 404", server.DefaultSyncAPIPath, got)
	}
}

// desktopTestPaths はデスクトップモードのデータディレクトリを一時ディレクトリにし（SORUS_DATA_DIR）、ブラウザを開かない
// ようにして useDesktopPaths を呼ぶ。useDesktopPaths が書き換える環境変数はテストの後に元へ戻す。
func desktopTestPaths(t *testing.T) desktop.AppPaths {
	t.Helper()
	prevListen := desktopListen
	prevListenSite := desktopListenSite
	desktopListen = func() (net.Listener, error) {
		ln, err := net.Listen("tcp", net.JoinHostPort(desktop.ListenHost, "0"))
		if err != nil {
			return nil, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if port >= desktop.PreferredPort && port <= desktop.PreferredPort+2 {
			ln.Close()
			return nil, fmt.Errorf("OS selected reserved desktop test port %d", port)
		}
		return ln, nil
	}
	desktopListenSite = func(port int, avoid []int) (net.Listener, error) {
		if port >= desktop.PreferredPort && port <= desktop.PreferredPort+2 {
			port = 0
		}
		safeAvoid := append([]int(nil), avoid...)
		safeAvoid = append(safeAvoid, desktop.PreferredPort, desktop.PreferredPort+1, desktop.PreferredPort+2)
		if port == 0 {
			return net.Listen("tcp", net.JoinHostPort(desktop.ListenHost, "0"))
		}
		return desktop.ListenSite(port, safeAvoid)
	}
	t.Cleanup(func() {
		desktopListen = prevListen
		desktopListenSite = prevListenSite
	})
	dir := t.TempDir()
	t.Setenv(desktop.DataDirEnv, dir)
	t.Setenv(desktop.NoBrowserEnv, "1")
	for _, k := range []string{"DB_PATH", "UPLOAD_DIR", "ATTACHMENT_DIR", "SEARCH_INDEX_DIR", "BACKUP_DIR"} {
		t.Setenv(k, "")
	}
	p, err := useDesktopPaths()
	if err != nil {
		t.Fatalf("useDesktopPaths: %v", err)
	}
	if p != desktop.AppPathsIn(dir) {
		t.Fatalf("paths = %+v, want AppPathsIn(%q)", p, dir)
	}
	return p
}

func expectDesktopTestPort(t *testing.T, port int) {
	t.Helper()
	if port >= desktop.PreferredPort && port <= desktop.PreferredPort+2 {
		t.Fatalf("runDesktop test used reserved port %d", port)
	}
}

// setDesktopMode はテストの間だけ desktopMode を v にする。
func setDesktopMode(t *testing.T, v bool) {
	t.Helper()
	prev := desktopMode
	desktopMode = v
	t.Cleanup(func() { desktopMode = prev })
}

func TestUseDesktopPaths(t *testing.T) {
	t.Setenv("DB_PATH", "elsewhere.db")
	p := desktop.SitePathsIn(desktopTestPaths(t).Dir)
	for k, want := range map[string]string{
		"DB_PATH": p.DB, "UPLOAD_DIR": p.Uploads, "ATTACHMENT_DIR": p.Attachments,
		"SEARCH_INDEX_DIR": p.SearchIndex, "BACKUP_DIR": p.Backups,
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// デスクトップモードのサブコマンドは ENV_FILE なしでデータディレクトリのデータを使い、初期管理者を作らない。
func TestDesktopSubcommandsUseDataDir(t *testing.T) {
	p := desktop.SitePathsIn(desktopTestPaths(t).Dir)
	setDesktopMode(t, true)
	t.Setenv("ADMIN_PASSWORD", "secret123456")

	db := openDatabase()
	var users int
	if err := db.Get(&users, "SELECT COUNT(*) FROM users"); err != nil || users != 0 {
		t.Fatalf("users = %d (err %v), want 0: desktop mode must leave the first user to the setup screen", users, err)
	}
	// 「はじめに」の前に取り込むと誰もログインできなくなるため、取り込みは拒否する。
	if err := checkDesktopSetupDone(db); err == nil {
		t.Error("checkDesktopSetupDone: no error before setup")
	}
	if _, err := auth.CreateFirstUser(db, "hanako", "花子", "password123", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := checkDesktopSetupDone(db); err != nil {
		t.Errorf("checkDesktopSetupDone after setup: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	if err := runBackup(backupConfig{}, &out); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	entries, err := os.ReadDir(p.Backups)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backups in %s = %v (err %v), want 1", p.Backups, entries, err)
	}
	if !strings.Contains(out.String(), "Source:      "+p.DB) {
		t.Errorf("backup source is not the desktop database:\n%s", out.String())
	}

	outDir := filepath.Join(t.TempDir(), "dist")
	if err := runExport(exportConfig{outDir: outDir}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "index.html")); err != nil {
		t.Errorf("export did not write index.html: %v", err)
	}
}

// runDesktop は一時データディレクトリで起動し、Host を確かめ、2回目の起動を既存のものへ案内し、
// 「はじめに」→「sorus を終了」で instance.json を消して止まる。
func TestRunDesktop(t *testing.T) {
	p := desktopTestPaths(t)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runDesktop(p, nil, stop) }()
	finished := false
	t.Cleanup(func() {
		close(stop)
		if !finished {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("runDesktop did not stop")
			}
		}
	})

	inst, ok := desktop.WaitRunning(context.Background(), p.Instance, 30*time.Second)
	if !ok {
		select {
		case err := <-done:
			finished = true
			t.Fatalf("runDesktop returned before serving: %v", err)
		default:
			t.Fatal("runDesktop did not write a reachable instance.json")
		}
	}
	expectDesktopTestPort(t, inst.Port)
	if inst.PID != os.Getpid() {
		t.Errorf("instance pid = %d, want %d", inst.PID, os.Getpid())
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar:           jar,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	do := func(method, path, host string, form url.Values) (*http.Response, string) {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequest(method, inst.BaseURL()+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Accept", "text/html")
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}

	if res, _ := do(http.MethodGet, "/healthz", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %d", res.StatusCode)
	}
	if res, _ := do(http.MethodGet, "/admin", "", nil); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != server.SetupPath {
		t.Errorf("/admin: %d %q, want 303 to %s", res.StatusCode, res.Header.Get("Location"), server.SetupPath)
	}
	if res, _ := do(http.MethodGet, "/admin", "localhost:"+strconv.Itoa(inst.Port), nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("/admin via localhost: %d, want 303", res.StatusCode)
	}
	for _, host := range []string{"evil.example", "evil.example:" + strconv.Itoa(inst.Port)} {
		if res, _ := do(http.MethodGet, "/admin", host, nil); res.StatusCode != http.StatusForbidden {
			t.Errorf("Host %s: %d, want 403", host, res.StatusCode)
		}
	}

	// 2回目の起動は、起動中のものを開いてすぐに終わる。
	second := make(chan error, 1)
	go func() { second <- runDesktop(p, nil, nil) }()
	select {
	case err := <-second:
		if err != nil {
			t.Errorf("second launch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second launch did not return")
	}
	if _, err := os.Stat(p.Instance); err != nil {
		t.Errorf("second launch removed instance.json: %v", err)
	}

	csrf := func() string {
		u, _ := url.Parse(inst.BaseURL() + "/admin/")
		for _, c := range jar.Cookies(u) {
			if c.Name == auth.CSRFCookieName {
				return c.Value
			}
		}
		t.Fatal("no CSRF cookie")
		return ""
	}
	if res, _ := do(http.MethodGet, server.SetupPath, "", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("setup page: %d", res.StatusCode)
	}
	setup := url.Values{"_csrf": {csrf()}, "display_name": {"山田 花子"}, "username": {"hanako"}, "password": {"password123"},
		"password_confirm": {"password123"}, "site_name": {"学生新聞"}}
	if res, _ := do(http.MethodPost, server.SetupPath, "", setup); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup: %d", res.StatusCode)
	}
	res, body := do(http.MethodGet, "/admin", "", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "sorus を終了") {
		t.Fatalf("dashboard after setup: %d, quit action shown: %v", res.StatusCode, strings.Contains(body, "sorus を終了"))
	}
	res, body = do(http.MethodPost, server.DesktopQuitPath, "", url.Values{"_csrf": {csrf()}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "終了しました") {
		t.Fatalf("quit: %d %q", res.StatusCode, body)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("runDesktop after quit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("sorus did not stop after quit")
	}
	if _, err := os.Stat(p.Instance); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("instance.json after quit: %v, want removed", err)
	}
	if logData, err := os.ReadFile(p.Log); err != nil || !strings.Contains(string(logData), "sorus stopped.") {
		t.Errorf("sorus.log does not record the shutdown (err %v)", err)
	}

	if err := runDesktop(p, []string{"--sync-path", "/api/sync-x"}, nil); err == nil {
		t.Error("runDesktop accepted a server-only argument")
	}
}

// 終了の途中（処理中の要求を待っている間）に起動し直すと、2回目の起動は前のものが止まるのを待ってから自分で起動する。
func TestRunDesktopRelaunchDuringQuit(t *testing.T) {
	p := desktopTestPaths(t)
	stop1 := make(chan struct{})
	done1 := make(chan error, 1)
	go func() { done1 <- runDesktop(p, nil, stop1) }()
	inst, ok := desktop.WaitRunning(context.Background(), p.Instance, 30*time.Second)
	if !ok {
		close(stop1)
		t.Fatal("first launch did not serve")
	}
	expectDesktopTestPort(t, inst.Port)

	// 本文を送り終えない要求で、1つ目の Shutdown を処理中の要求の待ちに留める。
	conn, err := net.Dial("tcp", inst.BaseURL()[len("http://"):])
	if err != nil {
		close(stop1)
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /admin/login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nusername=a",
		inst.BaseURL()[len("http://"):])
	time.Sleep(300 * time.Millisecond)

	close(stop1)
	// 待ち受けが閉じる（終了が始まった）まで待つ。
	for deadline := time.Now().Add(10 * time.Second); desktop.Healthy(context.Background(), inst); {
		if time.Now().After(deadline) {
			t.Fatal("first launch kept serving after stop")
		}
		time.Sleep(50 * time.Millisecond)
	}

	stop2 := make(chan struct{})
	done2 := make(chan error, 1)
	go func() { done2 <- runDesktop(p, nil, stop2) }()
	t.Cleanup(func() {
		close(stop2)
		select {
		case <-done2:
		case <-time.After(15 * time.Second):
			t.Error("second launch did not stop")
		}
	})
	time.Sleep(700 * time.Millisecond)
	conn.Close()

	select {
	case err := <-done1:
		if err != nil {
			t.Fatalf("first launch: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first launch did not stop")
	}
	running := make(chan desktop.Instance, 1)
	go func() {
		inst, _ := desktop.WaitRunning(context.Background(), p.Instance, 30*time.Second)
		running <- inst
	}()
	select {
	case err := <-done2:
		done2 <- err
		t.Fatalf("second launch returned instead of serving: %v", err)
	case inst := <-running:
		if inst.Port == 0 {
			t.Fatal("second launch is not serving")
		}
		expectDesktopTestPort(t, inst.Port)
	}
}

// twoAuthorsWXR は著者が2人（taro・jiro）の記事を1件ずつ持つ WXR を dir に書き、そのパスを返す。
func twoAuthorsWXR(t *testing.T, dir string) string {
	t.Helper()
	item := func(login, slug string) string {
		return `
	<item>
		<title>` + slug + `</title>
		<link>https://example.com/` + slug + `/</link>
		<dc:creator><![CDATA[` + login + `]]></dc:creator>
		<content:encoded><![CDATA[<p>本文</p>]]></content:encoded>
		<wp:post_name><![CDATA[` + slug + `]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
	</item>`
	}
	wxr := `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:wp="http://wordpress.org/export/1.2/">
<channel>
	<title>テスト</title>
	<link>https://example.com</link>
	<wp:wxr_version>1.2</wp:wxr_version>
	<wp:author><wp:author_login><![CDATA[taro]]></wp:author_login><wp:author_display_name><![CDATA[太郎]]></wp:author_display_name></wp:author>
	<wp:author><wp:author_login><![CDATA[jiro]]></wp:author_login><wp:author_display_name><![CDATA[次郎]]></wp:author_display_name></wp:author>` +
		item("taro", "taro-post") + item("jiro", "jiro-post") + `
</channel>
</rss>`
	path := filepath.Join(dir, "two-authors.xml")
	if err := os.WriteFile(path, []byte(wxr), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// articleAuthors は記事のスラッグ → 著者のユーザー名を返す。
func articleAuthors(t *testing.T) (map[string]string, int) {
	t.Helper()
	db := openDatabase()
	defer db.Close()
	var rows []struct {
		Slug     string `db:"slug"`
		Username string `db:"username"`
	}
	if err := db.Select(&rows, `SELECT a.slug, u.username FROM articles a JOIN users u ON u.id = a.author_id`); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Slug] = r.Username
	}
	var users int
	if err := db.Get(&users, "SELECT COUNT(*) FROM users"); err != nil {
		t.Fatal(err)
	}
	return got, users
}

// デスクトップモードの取り込みは WordPress の著者のユーザーを作らず、すべての記事をデスクトップのアカウントの記事にする
// （デスクトップモードは1人1アカウント。作ったユーザーには誰もログインできない）。
func TestRunImport_DesktopAttributesToAccount(t *testing.T) {
	desktopTestPaths(t)
	setDesktopMode(t, true)
	db := openDatabase()
	if _, err := auth.CreateFirstUser(db, "hanako", "花子", "password123", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	db.Close()

	wxr := twoAuthorsWXR(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := runImport(importConfig{file: wxr, defaultAuthor: "admin", defaultAuthorSet: true, timezone: "UTC"}, &stdout, &stderr); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	got, users := articleAuthors(t)
	if users != 1 {
		t.Errorf("users = %d, want 1 (no users created in desktop mode)", users)
	}
	want := map[string]string{"taro-post": "hanako", "jiro-post": "hanako"}
	if !maps.Equal(got, want) {
		t.Errorf("article authors = %v, want %v", got, want)
	}
	if !strings.Contains(stdout.String(), "記事 2 件を「花子」さんの記事として取り込みました") {
		t.Errorf("summary does not say the articles went to the account:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--default-author") {
		t.Errorf("no notice that --default-author is ignored:\n%s", stderr.String())
	}
}

func TestRunImport_DesktopCanCreateWordPressAuthors(t *testing.T) {
	desktopTestPaths(t)
	setDesktopMode(t, true)
	db := openDatabase()
	if _, err := auth.CreateFirstUser(db, "admin", "管理者", "password123", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	db.Close()

	wxr := twoAuthorsWXR(t, t.TempDir())
	if err := runImport(importConfig{file: wxr, accountMode: "authors", defaultAuthor: "admin", timezone: "UTC"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	got, users := articleAuthors(t)
	if users != 3 {
		t.Errorf("users = %d, want 3 (admin + imported authors)", users)
	}
	want := map[string]string{"taro-post": "taro", "jiro-post": "jiro"}
	if !maps.Equal(got, want) {
		t.Errorf("article authors = %v, want %v", got, want)
	}
}

// デスクトップモードで「はじめに」の前に取り込むと、何をすればよいかを日本語で伝えて断り、ユーザーも記事も作らない。
// 古い版の取り込みで著者のユーザーだけがあり、「はじめに」で登録したアカウント（admin）が無いときも同じ。そのときも
// 「はじめに」は済んでいない扱いで実行でき（サーバーと同じ auth.SetupDone）、済ませれば取り込みはそのアカウントの記事になる。
func TestRunImport_DesktopBeforeSetup(t *testing.T) {
	desktopTestPaths(t)
	setDesktopMode(t, true)
	wxr := twoAuthorsWXR(t, t.TempDir())
	check := func(name string, wantUsers int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := runImport(importConfig{file: wxr, timezone: "UTC"}, &stdout, &stderr)
		if err == nil {
			t.Fatalf("%s: runImport succeeded", name)
		}
		for _, want := range []string{"sorus を一度起動して", "「はじめに」を済ませてから", "もう一度取り込んで"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not contain %q", name, err, want)
			}
		}
		if w := regexp.MustCompile(`[A-Za-z]+`).FindString(strings.ReplaceAll(err.Error(), "sorus", "")); w != "" {
			t.Errorf("%s: error %q has the English word %q", name, err, w)
		}
		got, users := articleAuthors(t)
		if users != wantUsers || len(got) != 0 {
			t.Errorf("%s: users = %d, articles = %v; want %d users and no articles", name, users, got, wantUsers)
		}
	}
	check("no users", 0)

	db := openDatabase()
	if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w1', 'taro', '太郎', '', 'writer')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	check("no account", 1)

	// 案内どおり「はじめに」を済ませれば（画面と同じ auth.CreateFirstUser）、取り込める。
	db = openDatabase()
	if _, err := auth.CreateFirstUser(db, "hanako", "花子", "password123", auth.RoleAdmin); err != nil {
		t.Fatalf("「はじめに」 with only a writer: %v", err)
	}
	db.Close()
	var stdout, stderr bytes.Buffer
	if err := runImport(importConfig{file: wxr, timezone: "UTC"}, &stdout, &stderr); err != nil {
		t.Fatalf("runImport after setup: %v", err)
	}
	got, users := articleAuthors(t)
	if want := map[string]string{"taro-post": "hanako", "jiro-post": "hanako"}; !maps.Equal(got, want) {
		t.Errorf("article authors = %v, want %v", got, want)
	}
	if users != 2 {
		t.Errorf("users = %d, want 2 (the old writer and the account)", users)
	}
}

// CLI の既定はどちらのモードでも current で、管理者アカウントの記事として取り込む。
func TestRunImport_ServerDefaultsToCurrentAccount(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DB_PATH", filepath.Join(dir, "server.db"))
	t.Setenv("SEARCH_INDEX_DIR", filepath.Join(dir, "search_index"))
	t.Setenv("UPLOAD_DIR", filepath.Join(dir, "uploads"))
	t.Setenv("ADMIN_PASSWORD", "secret123456")
	setDesktopMode(t, false)

	wxr := twoAuthorsWXR(t, dir)
	var stdout, stderr bytes.Buffer
	if err := runImport(importConfig{file: wxr, timezone: "UTC"}, &stdout, &stderr); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	got, users := articleAuthors(t)
	if users != 1 {
		t.Errorf("users = %d, want 1 (admin only)", users)
	}
	want := map[string]string{"taro-post": "admin", "jiro-post": "admin"}
	if !maps.Equal(got, want) {
		t.Errorf("article authors = %v, want %v", got, want)
	}
}

func TestIsHelpArgs(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"-help"}, {"--help"}} {
		if !isHelpArgs(args) {
			t.Errorf("isHelpArgs(%q) = false", args)
		}
	}
	for _, args := range [][]string{nil, {"--desktop"}, {"help", "x"}, {"import", "-h"}} {
		if isHelpArgs(args) {
			t.Errorf("isHelpArgs(%q) = true", args)
		}
	}
}

// sorus help・-h・--help は、すべての動作モードとサブコマンドを並べた使い方を標準出力に書いて 0 で終わる。
// テストのバイナリを子プロセスとして起動し、その中で main を動かして確かめる。
func TestHelpPrintsFullUsage(t *testing.T) {
	if args, ok := os.LookupEnv("SORUS_TEST_MAIN_ARGS"); ok {
		os.Args = append([]string{"sorus"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	want := []string{
		"--desktop", "--server", "--version", "public-only", "sync-path",
		"sorus token create", "sorus import", "--import=", "sorus export [--clean]", "sorus backup", "sorus restore", "sorus help",
	}
	for _, args := range []string{"help", "-h", "--help", "--desktop --help", "--server help"} {
		t.Run(args, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelpPrintsFullUsage$")
			cmd.Env = append(os.Environ(), "SORUS_TEST_MAIN_ARGS="+args,
				desktop.NoBrowserEnv+"=1", desktop.DataDirEnv+"="+t.TempDir())
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("sorus %s: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
			}
			for _, w := range want {
				if !strings.Contains(stdout.String(), w) {
					t.Errorf("sorus %s: usage on stdout lacks %q:\n%s", args, w, stdout.String())
				}
			}
			if strings.Contains(stderr.String(), "unknown") {
				t.Errorf("sorus %s: stderr = %q", args, stderr.String())
			}
		})
	}
}

// 環境変数 SYNC_API_PATH の同期 API のパスは秘匿するため、どの使い方の表示にも出さない（--public-only や
// token create の -h、引数の誤りで出す使い方も含む）。
func TestHelpHidesSyncPathFromEnv(t *testing.T) {
	if args, ok := os.LookupEnv("SORUS_TEST_MAIN_ARGS"); ok {
		os.Args = append([]string{"sorus"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	const secret = "/api/sync-0123456789abcdef"
	for _, tc := range []struct {
		args     string
		wantCode int
	}{
		{"--public-only -h", 0},
		{"--public-only --help", 0},
		{"--server --public-only -h", 0},
		{"token create -h", 0},
		{"--public-only extra", 2},
	} {
		t.Run(tc.args, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelpHidesSyncPathFromEnv$")
			cmd.Env = append(os.Environ(), "SORUS_TEST_MAIN_ARGS="+tc.args, "SYNC_API_PATH="+secret,
				"ENV_FILE="+filepath.Join(t.TempDir(), "none.env"), desktop.NoBrowserEnv+"=1", desktop.DataDirEnv+"="+t.TempDir())
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("sorus %s: %v", tc.args, err)
			}
			out := stdout.String() + stderr.String()
			if code != tc.wantCode {
				t.Errorf("sorus %s: exit code %d, want %d\n%s", tc.args, code, tc.wantCode, out)
			}
			if !strings.Contains(out, "sync-path") {
				t.Errorf("sorus %s: no usage printed:\n%s", tc.args, out)
			}
			if strings.Contains(out, "0123456789abcdef") {
				t.Errorf("sorus %s: usage shows the sync path from SYNC_API_PATH:\n%s", tc.args, out)
			}
		})
	}
}

func TestIsVersionArgs(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-version"}, {"version"}} {
		if !isVersionArgs(args) {
			t.Errorf("isVersionArgs(%q) = false", args)
		}
	}
	for _, args := range [][]string{nil, {"--desktop"}, {"version", "x"}, {"backup", "--version"}} {
		if isVersionArgs(args) {
			t.Errorf("isVersionArgs(%q) = true", args)
		}
	}
}

// リリースと同じ -X の指定でビルドした sorus が、埋め込んだ版を表示することを確かめる（main が internal/version を
// 参照していないと、リンカーが -X の指定を捨てて "dev" のままになる）。
func TestVersionStampedIntoBinary(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("ビルドの確認は linux/amd64 だけで行う")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go コマンドが見つからない")
	}
	const want = "v9.9.9"
	bin := filepath.Join(t.TempDir(), "sorus")
	build := exec.Command(gobin, "build", "-ldflags", "-X sorus/internal/version.Version="+want, "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, arg := range []string{"--version", "version"} {
		cmd := exec.Command(bin, arg)
		cmd.Env = append(os.Environ(), desktop.NoBrowserEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sorus %s: %v\n%s", arg, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("sorus %s = %q, want %q", arg, got, want)
		}
	}
}

// freePort は、いま空いている 127.0.0.1 のポートを返す。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(desktop.ListenHost, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// desktopHTTP は 127.0.0.1 のデスクトップサイトに、リダイレクトを追わず Cookie を覚えて送る。
type desktopHTTP struct {
	t    *testing.T
	base string
	http *http.Client
}

func newDesktopHTTP(t *testing.T, port int) *desktopHTTP {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &desktopHTTP{t: t, base: desktop.Instance{Port: port}.BaseURL(), http: &http.Client{
		Jar:           jar,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type desktopHTTPResponse struct {
	status int
	header http.Header
	body   string
}

func (c *desktopHTTP) do(req *http.Request) desktopHTTPResponse {
	c.t.Helper()
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return desktopHTTPResponse{status: res.StatusCode, header: res.Header, body: string(body)}
}

func (c *desktopHTTP) get(path string) desktopHTTPResponse {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(req)
}

func (c *desktopHTTP) cookie(name string) string {
	c.t.Helper()
	u, err := url.Parse(c.base + "/admin")
	if err != nil {
		c.t.Fatal(err)
	}
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *desktopHTTP) postForm(path string, form url.Values) desktopHTTPResponse {
	c.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if form.Get("_csrf") == "" {
		form.Set("_csrf", c.cookie(auth.CSRFCookieName))
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

// completeSetup はサイト鍵 key（1つ目は空）の「はじめに」を済ませる。
func (c *desktopHTTP) completeSetup(key string) {
	c.t.Helper()
	if res := c.get(server.SetupPath); res.status != http.StatusOK {
		c.t.Fatalf("setup page = %d", res.status)
	}
	csrfName := auth.CookieNamesFor(key).CSRF
	form := url.Values{
		"display_name": {"山田 花子"}, "username": {"hanako"}, "password": {"password123"},
		"password_confirm": {"password123"}, "site_name": {"学生新聞"},
		"_csrf": {c.cookie(csrfName)},
	}
	res := c.postForm(server.SetupPath, form)
	if res.status != http.StatusSeeOther {
		c.t.Fatalf("setup = %d\n%s", res.status, res.body)
	}
}

// postZip は「バックアップから始める」と同じ multipart で path に data を送る。CSRF はヘッダーで送る。
func (c *desktopHTTP) postZip(path, csrfName string, data []byte) desktopHTTPResponse {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="backup"; filename="sorus_deleted_site.zip"`)
	h.Set("Content-Type", "application/zip")
	fw, err := mw.CreatePart(h)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		c.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set(server.CSRFHeaderName, c.cookie(csrfName))
	return c.do(req)
}

// login はサイト鍵 key のログイン画面から username でログインする。
func (c *desktopHTTP) login(key, username, password string) desktopHTTPResponse {
	c.t.Helper()
	if res := c.get("/admin/login"); res.status != http.StatusOK {
		c.t.Fatalf("login page = %d", res.status)
	}
	return c.postForm("/admin/login", url.Values{
		"username": {username}, "password": {password},
		"_csrf": {c.cookie(auth.CookieNamesFor(key).CSRF)},
	})
}

// siteHealthy は 127.0.0.1:port のサイトがヘルスチェックに応答するかを返す。
func siteHealthy(port int) bool {
	return desktop.Healthy(context.Background(), desktop.Instance{Port: port})
}

// writeTwoSites は、1つ目のサイト（first1）と、port を記録した2つ目のサイト（second）の sites.json を p に書く。
func writeTwoSites(t *testing.T, p desktop.AppPaths, port int, last string) {
	t.Helper()
	reg := desktop.Registry{Sites: []desktop.Site{
		{ID: "first1", Dir: ".", Port: desktop.PreferredPort, Created: time.Now()},
		{ID: "second", Dir: "sites/second", Port: port, Created: time.Now()},
	}, Last: last}
	if err := desktop.SaveRegistry(p, reg); err != nil {
		t.Fatal(err)
	}
}

// loadSites は p の sites.json を読む。無ければテストを失敗させる。
func loadSites(t *testing.T, p desktop.AppPaths) desktop.Registry {
	t.Helper()
	reg, exists, err := desktop.LoadRegistry(p)
	if err != nil || !exists {
		t.Fatalf("sites.json: exists %v, err %v", exists, err)
	}
	return reg
}

// startRunDesktop は runDesktop を別の goroutine で動かし、instance.json に応答するインスタンスを返す。返す関数で止め、
// runDesktop の戻り値を返す。
func startRunDesktop(t *testing.T, p desktop.AppPaths) (desktop.Instance, func() error) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runDesktop(p, nil, stop) }()
	var once sync.Once
	var result error
	stopFn := func() error {
		once.Do(func() {
			close(stop)
			select {
			case result = <-done:
			case <-time.After(20 * time.Second):
				result = errors.New("runDesktop did not stop")
			}
		})
		return result
	}
	t.Cleanup(func() {
		if err := stopFn(); err != nil {
			t.Error(err)
		}
	})
	inst, ok := desktop.WaitRunning(context.Background(), p.Instance, 30*time.Second)
	if !ok {
		select {
		case err := <-done:
			once.Do(func() {})
			t.Fatalf("runDesktop returned before serving: %v", err)
		default:
			t.Fatal("runDesktop did not write a reachable instance.json")
		}
	}
	expectDesktopTestPort(t, inst.Port)
	return inst, stopFn
}

// getAdmin は 127.0.0.1:port の path を、accept を付けて GET し、状態コードを返す。
func getAdmin(t *testing.T, port int, path, accept string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, desktop.Instance{Port: port}.BaseURL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	client := &http.Client{Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s on port %d: %v", path, port, err)
	}
	res.Body.Close()
	return res.StatusCode
}

// 登録した2つのサイトを同時に開き、2つ目は記録したポートで待ち受ける。instance.json は最後に使ったサイトを指し、
// 別のサイトの管理画面を開くとそちらに変わる（sites.json の last も）。止めると両方とも止まる。
func TestRunDesktopTwoSites(t *testing.T) {
	p := desktopTestPaths(t)
	secondPort := freePort(t)
	writeTwoSites(t, p, secondPort, "second")

	inst, stop := startRunDesktop(t, p)
	if inst.Port != secondPort {
		t.Errorf("instance.json port = %d, want the last used site's port %d", inst.Port, secondPort)
	}
	reg := loadSites(t, p)
	firstPort := reg.Sites[0].Port
	if reg.Sites[1].Port != secondPort {
		t.Errorf("second site port = %d, want the recorded %d", reg.Sites[1].Port, secondPort)
	}
	for _, port := range []int{firstPort, secondPort} {
		if !siteHealthy(port) {
			t.Errorf("site on port %d does not answer", port)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "sites", "second", "database.db")); err != nil {
		t.Errorf("second site database: %v", err)
	}

	// 裏で読み込む部分（fetch）では変わらず、管理画面のページを開くと変わる。
	getAdmin(t, firstPort, "/admin/backup/status", "*/*")
	if got, _ := desktop.ReadInstance(p.Instance); got.Port != secondPort {
		t.Errorf("instance.json port after a background fetch = %d, want %d", got.Port, secondPort)
	}
	getAdmin(t, firstPort, "/admin", "text/html")
	if got, _ := desktop.ReadInstance(p.Instance); got.Port != firstPort {
		t.Errorf("instance.json port after opening the first site = %d, want %d", got.Port, firstPort)
	}
	if reg := loadSites(t, p); reg.Last != "first1" {
		t.Errorf("last = %q, want first1", reg.Last)
	}

	if err := stop(); err != nil {
		t.Fatalf("runDesktop: %v", err)
	}
	for _, port := range []int{firstPort, secondPort} {
		if siteHealthy(port) {
			t.Errorf("site on port %d still answers after quit", port)
		}
	}
	if _, err := os.Stat(p.Instance); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("instance.json after quit: %v, want removed", err)
	}
}

// 記録したポートがほかのプログラムに使われていれば、別のポートで待ち受け、sites.json に記録し直してログに残す。
func TestRunDesktopReplacesBusyPort(t *testing.T) {
	p := desktopTestPaths(t)
	busy, err := net.Listen("tcp", net.JoinHostPort(desktop.ListenHost, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	writeTwoSites(t, p, busyPort, "second")

	inst, stop := startRunDesktop(t, p)
	reg := loadSites(t, p)
	newPort := reg.Sites[1].Port
	if newPort == busyPort || newPort == 0 || inst.Port != newPort {
		t.Errorf("second site port = %d (instance %d), want a new port instead of the busy %d", newPort, inst.Port, busyPort)
	}
	if !siteHealthy(newPort) {
		t.Errorf("second site does not answer on its new port %d", newPort)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	logData, _ := os.ReadFile(p.Log)
	if want := fmt.Sprintf("port %d is not available; using port %d", busyPort, newPort); !strings.Contains(string(logData), want) {
		t.Errorf("sorus.log does not record the new port (%q)", want)
	}
}

// データベースが壊れたサイトがあっても、ほかのサイトは開く。開けなかった理由は一覧に出る。1つも開けなければ起動しない。
func TestRunDesktopCorruptSite(t *testing.T) {
	p := desktopTestPaths(t)
	writeTwoSites(t, p, freePort(t), "second")
	corrupt := desktop.SitePathsIn(filepath.Join(p.Dir, "sites", "second")).DB
	writeTestFile(t, corrupt, strings.Repeat("this is not a database ", 100))

	inst, stop := startRunDesktop(t, p)
	if inst.Port != loadSites(t, p).Sites[0].Port || !siteHealthy(inst.Port) {
		t.Errorf("instance %d does not point to the first site that opened", inst.Port)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if logData, _ := os.ReadFile(p.Log); !strings.Contains(string(logData), "could not open the site in "+filepath.Dir(corrupt)) {
		t.Error("sorus.log does not record the site that could not be opened")
	}

	app := startDesktopApp(t, p)
	list := desktopSiteView{app: app, site: app.sites[0]}.List()
	if len(list) != 2 || list[0].OpenError != "" || list[0].AdminURL == "" ||
		list[1].ID != "second" || list[1].OpenError == "" || list[1].AdminURL != "" {
		t.Errorf("List = %+v, want the second site with its open error", list)
	}
	if _, err := (desktopSiteView{app: app, site: app.sites[0]}).Delete("second"); err == nil {
		t.Error("Delete removed a site that could not be opened")
	}
	app.beginStop()
	app.shutdown(context.Background())

	// 1つ目のサイトも壊れていれば、起動しない。
	writeTestFile(t, desktop.SitePathsIn(p.Dir).DB+"-wal", "")
	os.Remove(desktop.SitePathsIn(p.Dir).DB + "-shm")
	writeTestFile(t, desktop.SitePathsIn(p.Dir).DB, strings.Repeat("broken ", 200))
	if err := runDesktop(p, nil, nil); err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("runDesktop with no site that opens = %v, want a database error", err)
	}
}

// startDesktopApp はデータディレクトリ p のアプリを開いて動かす（runDesktop の待ち受けの部分だけ）。テストの後に止める。
func startDesktopApp(t *testing.T, p desktop.AppPaths) *desktopApp {
	t.Helper()
	app, err := newDesktopApp(p, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.openSites(); err != nil {
		t.Fatal(err)
	}
	app.start()
	t.Cleanup(func() {
		app.beginStop()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		app.shutdown(ctx)
	})
	return app
}

// view はアプリの i 番目のサイトのサーバーに渡す Sites を返す。
func (a *desktopApp) view(i int) server.Sites {
	a.mu.Lock()
	defer a.mu.Unlock()
	return desktopSiteView{app: a, site: a.sites[i]}
}

// サイトを作ると sites.json ができ、1つ目のサイトにも id が付く。新しいサイトは別のポートで、名前の付いた Cookie を使う。
// 削除すると、バックアップを作ってから、フォルダと一覧から消える。最後に残ったサイトは削除できない。
func TestDesktopSitesCreateListDelete(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	first := app.view(0)
	list := first.List()
	if len(list) != 1 || list[0].ID != "" || !list[0].Current || list[0].AdminURL == "" || list[0].Name != "" {
		t.Fatalf("List with one site = %+v", list)
	}
	if _, err := os.Stat(p.Sites); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sites.json exists before a second site is created: %v", err)
	}
	if _, err := first.Delete(""); err == nil {
		t.Error("Delete accepted an empty id")
	}

	created, err := first.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	reg := loadSites(t, p)
	if len(reg.Sites) != 2 || reg.Sites[0].Dir != "." || !desktop.ValidSiteID(reg.Sites[0].ID) ||
		reg.Sites[1].ID != created.ID || reg.Sites[1].Dir != "sites/"+created.ID {
		t.Fatalf("sites.json after Create = %+v (created %+v)", reg, created)
	}
	if created.Current || created.Name != "" || created.OpenError != "" ||
		created.AdminURL != (desktop.Instance{Port: reg.Sites[1].Port}).AdminURL() {
		t.Errorf("created = %+v", created)
	}
	if !siteHealthy(reg.Sites[1].Port) {
		t.Fatal("the new site does not answer")
	}
	// 新しいサイトの「はじめに」は、名前に id を付けた CSRF の Cookie を出す（1つ目のサイトの Cookie に触れない）。
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Transport: &http.Transport{Proxy: nil}}
	res, err := client.Get(desktop.Instance{Port: reg.Sites[1].Port}.BaseURL() + server.SetupPath)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var names []string
	for _, c := range res.Cookies() {
		names = append(names, c.Name)
	}
	if !slices.Contains(names, auth.CookieNamesFor(created.ID).CSRF) || slices.Contains(names, auth.CSRFCookieName) {
		t.Errorf("cookies from the new site = %v, want %s", names, auth.CookieNamesFor(created.ID).CSRF)
	}
	newPort := reg.Sites[1].Port
	list = app.view(1).List()
	if len(list) != 2 || list[0].ID != reg.Sites[0].ID || list[0].Current || !list[1].Current {
		t.Errorf("List from the new site = %+v", list)
	}

	backupFile, err := app.view(1).Delete(created.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if filepath.Dir(backupFile) != p.DeletedBackups || !strings.Contains(filepath.Base(backupFile), created.ID) {
		t.Errorf("backup = %s, want a file with the id in %s", backupFile, p.DeletedBackups)
	}
	if _, err := backup.ReadManifest(backupFile); err != nil {
		t.Errorf("backup: %v", err)
	}
	app.deletions.Wait()
	if _, err := os.Stat(filepath.Join(p.Dir, "sites", created.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("site folder after Delete: %v", err)
	}
	if after := loadSites(t, p); len(after.Sites) != 1 || after.Sites[0].Dir != "." {
		t.Errorf("sites.json after Delete = %+v", after)
	}
	if siteHealthy(newPort) {
		t.Error("the deleted site still answers")
	}
	if list := first.List(); len(list) != 1 || !list[0].Current {
		t.Errorf("List after Delete = %+v", list)
	}
	if _, err := first.Delete(reg.Sites[0].ID); err == nil || !strings.Contains(err.Error(), "最後") {
		t.Errorf("Delete of the last site = %v, want refused", err)
	}
}

// 1つ目のサイトを削除すると、サイトのデータとして決めたものだけを消し、データフォルダ・アプリのファイル・ほかのサイトは
// 残す。最後に使ったサイトと instance.json は、残ったサイトに移る。
func TestDesktopDeleteFirstSite(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	firstID := loadSites(t, p).Sites[0].ID
	app.useSite(app.sites[0])
	first := desktop.SitePathsIn(p.Dir)
	writeTestFile(t, filepath.Join(first.Uploads, "a.webp"), "image")
	writeTestFile(t, filepath.Join(first.Attachments, "b.pdf"), "pdf")
	writeTestFile(t, first.Recovery, "code")
	writeTestFile(t, filepath.Join(first.Backups, "keep.zip"), "manual backup")
	writeTestFile(t, p.Log, "log")

	backupFile, err := app.view(1).Delete(firstID)
	if err != nil {
		t.Fatalf("Delete(first): %v", err)
	}
	m, err := backup.ReadManifest(backupFile)
	if err != nil || m.Uploads.FileCount != 1 || m.Attachments == nil || m.Attachments.FileCount != 1 {
		t.Errorf("backup of the first site = %+v, %v", m, err)
	}
	app.deletions.Wait()
	for _, gone := range []string{first.DB, first.DB + "-wal", first.DB + "-shm", first.Uploads, first.Attachments, first.SearchIndex, first.Recovery} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after deleting the first site: %v", gone, err)
		}
	}
	for _, kept := range []string{p.Dir, p.Log, p.Sites, p.Instance, backupFile, filepath.Join(first.Backups, "keep.zip"),
		desktop.SitePathsIn(filepath.Join(p.Dir, "sites", created.ID)).DB} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed with the first site: %v", kept, err)
		}
	}
	reg := loadSites(t, p)
	if len(reg.Sites) != 1 || reg.Sites[0].ID != created.ID || reg.Last != created.ID {
		t.Errorf("sites.json = %+v, want only %s as the last used site", reg, created.ID)
	}
	if inst, err := desktop.ReadInstance(p.Instance); err != nil || inst.Port != reg.Sites[0].Port {
		t.Errorf("instance.json = %+v (%v), want the remaining site's port %d", inst, err, reg.Sites[0].Port)
	}
}

// バックアップを作れなければ、何も消さずにエラーを返し、サイトは動き続ける。
func TestDesktopDeleteKeepsSiteWhenBackupFails(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	failBackups(t, app.sites[1])

	if _, err := app.view(1).Delete(created.ID); err == nil || !strings.Contains(err.Error(), "バックアップ") {
		t.Fatalf("Delete with a failing backup = %v, want an error about the backup", err)
	}
	app.deletions.Wait()
	sp := desktop.SitePathsIn(filepath.Join(p.Dir, "sites", created.ID))
	if _, err := os.Stat(sp.DB); err != nil {
		t.Errorf("site database after a failed delete: %v", err)
	}
	if reg := loadSites(t, p); len(reg.Sites) != 2 {
		t.Errorf("sites.json after a failed delete = %+v", reg)
	}
	if list := app.view(0).List(); len(list) != 2 {
		t.Errorf("List after a failed delete = %+v", list)
	}
	if !siteHealthy(loadSites(t, p).Sites[1].Port) {
		t.Error("the site stopped answering after a failed delete")
	}
	if entries, _ := os.ReadDir(p.DeletedBackups); len(entries) != 0 {
		t.Errorf("files in %s = %v", p.DeletedBackups, entries)
	}
}

// 一覧の保存に失敗した削除は、ファイルを残し、そのサイトを開けなかったサイトとして一覧に残す。
// instance.json は、開いているサイトを指す。
func TestDesktopDeleteKeepsSiteWhenListCannotBeSaved(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	app.useSite(app.sites[1])
	deletedPort := app.sites[1].port
	openPort := app.sites[0].port
	siteDir := filepath.Join(p.Dir, "sites", created.ID)
	dbPath := desktop.SitePathsIn(siteDir).DB
	if err := os.Remove(p.Sites); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.Sites, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := app.view(1).Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	app.deletions.Wait()

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database after a failed site-list save: %v", err)
	}
	if _, err := os.Stat(siteDir); err != nil {
		t.Errorf("site folder after a failed site-list save: %v", err)
	}
	if siteHealthy(deletedPort) {
		t.Error("the site still answers after deletion stopped it")
	}
	list := app.view(0).List()
	var found *server.SiteInfo
	for i := range list {
		if list[i].ID == created.ID {
			found = &list[i]
		}
	}
	if found == nil || found.OpenError == "" {
		t.Fatalf("List = %+v, want the site kept with an open error", list)
	}
	app.mu.Lock()
	failed := app.sites[1]
	if failed.entry.ID != created.ID || failed.gone || failed.deleting || failed.srv != nil || failed.db != nil || failed.openErr == nil {
		t.Errorf("site after a failed save: id %s gone %v deleting %v srv %v db %v err %v",
			failed.entry.ID, failed.gone, failed.deleting, failed.srv != nil, failed.db != nil, failed.openErr)
	}
	app.mu.Unlock()
	inst, err := desktop.ReadInstance(p.Instance)
	if err != nil || inst.Port != openPort {
		t.Errorf("instance.json = %+v, %v, want the open site's port %d", inst, err, openPort)
	}
}

// failBackups は、サイト s の削除の前のバックアップを作れないようにする（自動バックアップのフォルダに、作れない場所
// （ファイルの下）を決めておく）。
func failBackups(t *testing.T, s *desktopSite) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "file")
	writeTestFile(t, blocker, "x")
	if _, err := s.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('backup_auto_folder', ?, CURRENT_TIMESTAMP)`,
		filepath.Join(blocker, "backups")); err != nil {
		t.Fatal(err)
	}
}

// 削除の前のバックアップを作れなかったときにアプリが止まり始めていても、そのサイトも止めて閉じる。終了が動いている
// サイトを集めたあとにバックアップが失敗した順と、止まり始めた（instance.json を消した）だけのときに失敗した順。
func TestDesktopShutdownDuringFailedDelete(t *testing.T) {
	for _, collected := range []bool{true, false} {
		name := "after shutdown collected the running sites"
		if !collected {
			name = "after stopping began"
		}
		t.Run(name, func(t *testing.T) {
			p := desktopTestPaths(t)
			app := startDesktopApp(t, p)
			created, err := app.view(0).Create()
			if err != nil {
				t.Fatal(err)
			}
			site := app.sites[1]
			failBackups(t, site)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stopped := make(chan struct{})
			shutdown := func() {
				go func() {
					app.shutdown(ctx)
					close(stopped)
				}()
			}

			if _, err := app.beginDelete(created.ID); err != nil {
				t.Fatalf("beginDelete: %v", err)
			}
			app.beginStop()
			if collected {
				shutdown()
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
					app.mu.Lock()
					closed := app.closed
					app.mu.Unlock()
					if closed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("shutdown did not start")
					}
				}
			}
			if _, err := app.backupAndDelete(site); err == nil {
				t.Fatal("backupAndDelete succeeded although the backup cannot be written")
			}
			if !collected {
				shutdown()
			}
			select {
			case <-stopped:
			case <-time.After(30 * time.Second):
				t.Fatal("shutdown did not finish")
			}
			if err := site.db.Ping(); err == nil {
				t.Error("the database of the site whose delete failed is still open after shutdown")
			}
			if siteHealthy(site.port) {
				t.Error("the site whose delete failed still answers after shutdown")
			}
		})
	}
}

// 開けなかったサイトは残りに数えない。開いているサイトが1つだけなら、そのサイトは削除しない（削除すると開いている
// サイトが無くなり、instance.json も閉じたサイトを指したままになる）。開けなかったサイトの削除は今までどおり断る。
func TestDesktopDeleteKeepsLastOpenSite(t *testing.T) {
	p := desktopTestPaths(t)
	writeTwoSites(t, p, freePort(t), "first1")
	writeTestFile(t, desktop.SitePathsIn(filepath.Join(p.Dir, "sites", "second")).DB, strings.Repeat("this is not a database ", 100))
	app := startDesktopApp(t, p)
	view := app.view(0)

	if _, err := view.Delete("first1"); err == nil || !strings.Contains(err.Error(), "最後") {
		t.Errorf("Delete of the only open site = %v, want refused as the last site", err)
	}
	app.deletions.Wait()
	port := loadSites(t, p).Sites[0].Port
	if !siteHealthy(port) {
		t.Error("the only open site stopped answering")
	}
	if inst, err := desktop.ReadInstance(p.Instance); err != nil || inst.Port != port {
		t.Errorf("instance.json = %+v (%v), want the open site's port %d", inst, err, port)
	}
	if _, err := view.Delete("second"); err == nil || !strings.Contains(err.Error(), "開けなかった") {
		t.Errorf("Delete of the site that could not be opened = %v, want the open error", err)
	}
}

// 削除は、削除するサイトで処理中の要求（削除を頼んだ要求）を待たずに戻り、その要求を返し終えてから後片付けをする。
func TestDesktopDeleteDoesNotWaitForInFlightRequest(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort(desktop.ListenHost, strconv.Itoa(loadSites(t, p).Sites[1].Port))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /admin/login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nusername=a", addr)
	time.Sleep(300 * time.Millisecond)

	returned := make(chan error, 1)
	go func() {
		_, err := app.view(1).Delete(created.ID)
		returned <- err
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete waited for the request in flight")
	}
	// 処理中の要求があるあいだは、フォルダを消さない。
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(p.Dir, "sites", created.ID)); err != nil {
		t.Errorf("site folder removed while a request was in flight: %v", err)
	}
	conn.Close()
	finished := make(chan struct{})
	go func() { app.deletions.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("the deletion did not finish after the request ended")
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "sites", created.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("site folder after the deletion finished: %v", err)
	}
}

// パスワード再設定コードのファイルは、サーバーが書く場所とサイトの削除で消す場所が同じ。
func TestRecoveryFileNameMatchesServer(t *testing.T) {
	if desktop.RecoveryFileName != server.RecoveryFileName {
		t.Errorf("desktop.RecoveryFileName = %q, server.RecoveryFileName = %q", desktop.RecoveryFileName, server.RecoveryFileName)
	}
}

// デスクトップモードの backup・restore・export・import は --site を受け付ける。サーバーモードでは受け付けない。
func TestParseSiteFlag(t *testing.T) {
	setDesktopMode(t, true)
	if cfg, _, err := parseBackupArgs([]string{"backup", "--site", "abc", "out.zip"}); err != nil || cfg.site != "abc" || cfg.dest != "out.zip" {
		t.Errorf("backup: %+v, %v", cfg, err)
	}
	if cfg, _, err := parseRestoreArgs([]string{"restore", "--site=abc", "--yes", "b.zip"}); err != nil || cfg.site != "abc" || cfg.src != "b.zip" || !cfg.yes {
		t.Errorf("restore: %+v, %v", cfg, err)
	}
	if cfg, _, err := parseExportArgs([]string{"export", "--site", "abc", "out"}); err != nil || cfg.site != "abc" || cfg.outDir != "out" {
		t.Errorf("export: %+v, %v", cfg, err)
	}
	if cfg, _, err := parseImportArgs([]string{"import", "wp.xml", "--site", "abc"}); err != nil || cfg.site != "abc" || cfg.file != "wp.xml" {
		t.Errorf("import: %+v, %v", cfg, err)
	}
	if cfg, _, err := parseBackupArgs([]string{"backup"}); err != nil || cfg.site != "" {
		t.Errorf("backup without --site: %+v, %v", cfg, err)
	}

	setDesktopMode(t, false)
	for _, args := range [][]string{{"backup", "--site", "abc"}, {"restore", "--site", "abc", "b.zip"}, {"export", "--site", "abc"}, {"import", "--site", "abc", "wp.xml"}} {
		var err error
		switch args[0] {
		case "backup":
			_, _, err = parseBackupArgs(args)
		case "restore":
			_, _, err = parseRestoreArgs(args)
		case "export":
			_, _, err = parseExportArgs(args)
		case "import":
			_, _, err = parseImportArgs(args)
		}
		if err == nil {
			t.Errorf("server mode accepted %v", args)
		}
	}
}

// サイトが1つなら --site を省ける。2つ以上なら、省くと id の一覧と sorus sites を示すエラーになる。無い id もエラー。
func TestDesktopSiteSelection(t *testing.T) {
	p := desktopTestPaths(t)
	setDesktopMode(t, true)
	if site, err := desktopSiteFor(p, ""); err != nil || !site.First() {
		t.Errorf("one site without --site = %+v, %v; want the data directory", site, err)
	}
	if _, err := desktopSiteFor(p, "abc"); err == nil {
		t.Error("an unknown id was accepted with one site")
	}

	writeTwoSites(t, p, freePort(t), "")
	_, err := desktopSiteFor(p, "")
	if err == nil || !strings.Contains(err.Error(), "first1, second") || !strings.Contains(err.Error(), "sorus sites") ||
		!strings.Contains(err.Error(), "--site") {
		t.Errorf("two sites without --site = %v, want the ids and sorus sites", err)
	}
	if _, err := desktopSiteFor(p, "nosuch"); err == nil || !strings.Contains(err.Error(), "sorus sites") {
		t.Errorf("unknown id = %v", err)
	}
	if err := useDesktopSite(p, "second"); err != nil {
		t.Fatal(err)
	}
	second := desktop.SitePathsIn(filepath.Join(p.Dir, "sites", "second"))
	for k, want := range map[string]string{"DB_PATH": second.DB, "UPLOAD_DIR": second.Uploads, "ATTACHMENT_DIR": second.Attachments,
		"SEARCH_INDEX_DIR": second.SearchIndex, "BACKUP_DIR": second.Backups} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := useDesktopSite(p, ""); err == nil {
		t.Error("useDesktopSite chose a site among two without --site")
	}

	// 選んだサイトのデータをバックアップする。
	writeTestFile(t, filepath.Join(second.Uploads, "a.webp"), "image")
	db, err := database.Connect(second.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var out bytes.Buffer
	if err := runBackup(backupConfig{}, &out); err != nil {
		t.Fatalf("runBackup: %v", err)
	}
	if entries, err := os.ReadDir(second.Backups); err != nil || len(entries) != 1 {
		t.Errorf("backups of the second site = %v (%v)", entries, err)
	}
	if _, err := os.Stat(desktop.SitePathsIn(p.Dir).Backups); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the first site got a backup: %v", err)
	}
}

// sorus sites はサイトを1行に1つずつ、id・サイト名（無ければ「名前の無いサイト（作成日）」）・アドレス・フォルダで表示する。
func TestRunSites(t *testing.T) {
	p := desktopTestPaths(t)
	var out bytes.Buffer
	if err := runSites(p, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "-  ") || !strings.Contains(lines[0], "名前の無いサイト（") ||
		!strings.Contains(lines[0], desktop.Instance{Port: desktop.PreferredPort}.AdminURL()) || !strings.HasSuffix(lines[0], p.Dir) {
		t.Errorf("one site:\n%s", out.String())
	}
	if _, err := os.Stat(desktop.SitePathsIn(p.Dir).DB); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sorus sites created a database: %v", err)
	}

	port := freePort(t)
	writeTwoSites(t, p, port, "")
	second := desktop.SitePathsIn(filepath.Join(p.Dir, "sites", "second"))
	if err := os.MkdirAll(second.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := database.Connect(second.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateFirstUser(db, "hanako", "花子", "password123", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('site_name', '学生新聞', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	out.Reset()
	if err := runSites(p, &out); err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("two sites:\n%s", out.String())
	}
	if f := strings.Fields(lines[0]); len(f) != 4 || f[0] != "first1" || !strings.HasPrefix(f[1], "名前の無いサイト（") ||
		f[2] != (desktop.Instance{Port: desktop.PreferredPort}).AdminURL() || f[3] != p.Dir {
		t.Errorf("first line = %q", lines[0])
	}
	if f := strings.Fields(lines[1]); len(f) != 4 || f[0] != "second" || f[1] != "学生新聞" ||
		f[2] != (desktop.Instance{Port: port}).AdminURL() || f[3] != filepath.Join(p.Dir, "sites", "second") {
		t.Errorf("second line = %q", lines[1])
	}

	setDesktopMode(t, false)
	if _, err := parseSitesArgs([]string{"sites"}); err == nil {
		t.Error("sorus sites ran in server mode")
	}
	setDesktopMode(t, true)
	if ok, err := parseSitesArgs([]string{"sites"}); !ok || err != nil {
		t.Errorf("parseSitesArgs(sites) = %v, %v", ok, err)
	}
	if _, err := parseSitesArgs([]string{"sites", "extra"}); err == nil {
		t.Error("sorus sites accepted an extra argument")
	}
	if ok, _ := parseSitesArgs([]string{"backup"}); ok {
		t.Error("parseSitesArgs matched another command")
	}
}

// 自動バックアップの保存先が削除で消える範囲の中でも、削除直前の zip は「削除したサイトのバックアップ」に残り、
// 完了時に返すパスもそこを指す。2つ目のサイトは sites/<id>/ の中、1つ目のサイトは uploads/ の中。
// 外から中を指すシンボリックリンクも、解決した先で同じように扱う。
func TestDesktopDeleteBackupInsideRemovedAreaIsKept(t *testing.T) {
	setAutoBackupFolder := func(t *testing.T, db interface {
		Exec(string, ...any) (sql.Result, error)
	}, folder string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('backup_auto_folder', ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, folder); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("second site folder", func(t *testing.T) {
		p := desktopTestPaths(t)
		app := startDesktopApp(t, p)
		created, err := app.view(0).Create()
		if err != nil {
			t.Fatal(err)
		}
		inside := filepath.Join(p.Dir, "sites", created.ID, "backups")
		if err := os.MkdirAll(inside, 0o755); err != nil {
			t.Fatal(err)
		}
		setAutoBackupFolder(t, app.sites[1].db, inside)
		backupFile, err := app.view(1).Delete(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(backupFile) != p.DeletedBackups {
			t.Errorf("backup = %s, want it in %s", backupFile, p.DeletedBackups)
		}
		app.deletions.Wait()
		if _, err := os.Stat(backupFile); err != nil {
			t.Errorf("backup after delete: %v", err)
		}
		if _, err := os.Stat(filepath.Join(p.Dir, "sites", created.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("site folder still exists: %v", err)
		}
	})

	t.Run("symlink into second site", func(t *testing.T) {
		p := desktopTestPaths(t)
		app := startDesktopApp(t, p)
		created, err := app.view(0).Create()
		if err != nil {
			t.Fatal(err)
		}
		inside := filepath.Join(p.Dir, "sites", created.ID, "backups")
		if err := os.MkdirAll(inside, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "backup-link")
		if err := os.Symlink(inside, link); err != nil {
			t.Fatal(err)
		}
		setAutoBackupFolder(t, app.sites[1].db, link)
		backupFile, err := app.view(1).Delete(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(backupFile) != p.DeletedBackups {
			t.Errorf("backup = %s, want it in %s", backupFile, p.DeletedBackups)
		}
		app.deletions.Wait()
		if _, err := os.Stat(backupFile); err != nil {
			t.Errorf("backup after delete: %v", err)
		}
	})

	t.Run("first site uploads", func(t *testing.T) {
		p := desktopTestPaths(t)
		app := startDesktopApp(t, p)
		if _, err := app.view(0).Create(); err != nil {
			t.Fatal(err)
		}
		firstID := loadSites(t, p).Sites[0].ID
		uploads := desktop.SitePathsIn(p.Dir).Uploads
		if err := os.MkdirAll(uploads, 0o755); err != nil {
			t.Fatal(err)
		}
		setAutoBackupFolder(t, app.sites[0].db, uploads)
		backupFile, err := app.view(1).Delete(firstID)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(backupFile) != p.DeletedBackups {
			t.Errorf("backup = %s, want it in %s", backupFile, p.DeletedBackups)
		}
		app.deletions.Wait()
		if _, err := os.Stat(backupFile); err != nil {
			t.Errorf("backup after delete: %v", err)
		}
		if _, err := os.Stat(uploads); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("uploads after deleting the first site: %v", err)
		}
	})
}

// sites.json を保存できなければ、サイトのファイルは消さない。一覧に残ったサイトを、空の database.db で開き直さない。
func TestDesktopDeleteKeepsFilesWhenRegistrySaveFails(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(p.Dir, "sites", created.ID, "keep.txt")
	writeTestFile(t, marker, "keep")
	blocker := filepath.Join(p.Dir, "not-a-directory")
	writeTestFile(t, blocker, "x")
	app.mu.Lock()
	app.paths.Sites = filepath.Join(blocker, "sites.json")
	app.mu.Unlock()

	if _, err := app.view(1).Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	app.deletions.Wait()
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("site file after a failed sites.json save: %v", err)
	}
	if reg := loadSites(t, p); len(reg.Sites) != 2 {
		t.Errorf("sites.json = %+v, want both sites kept", reg)
	}
}

// サイト名の読み取りに失敗したサイトは、未設定ではない。別サイトの「削除」だけでは消えない。
func TestDesktopNameReadErrorDoesNotAllowUnnamedDelete(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.sites[1].db.Exec(`DROP TABLE users`); err != nil {
		t.Fatal(err)
	}
	reg := loadSites(t, p)
	firstPort := reg.Sites[0].Port
	c := newDesktopHTTP(t, firstPort)
	c.completeSetup("")

	page := c.get("/admin/settings/sites")
	if page.status != http.StatusOK {
		t.Fatalf("sites page = %d\n%s", page.status, page.body)
	}
	deleteAction := `/admin/settings/sites/` + created.ID + `/delete`
	if strings.Contains(page.body, deleteAction) {
		t.Fatalf("sites page offers unnamed delete for a site whose name could not be read:\n%s", page.body)
	}
	if !strings.Contains(page.body, "サイトの情報を読めませんでした") {
		t.Errorf("sites page does not explain that the site could not be read:\n%s", page.body)
	}
	if strings.Contains(page.body, "「はじめに」がまだ済んでいません") {
		t.Error("a name read error is shown as setup not finished")
	}
	res := c.postForm(deleteAction, nil)
	if res.status == http.StatusSeeOther {
		t.Fatalf("unnamed delete redirected: %s", res.header.Get("Location"))
	}
	app.deletions.Wait()
	if _, err := os.Stat(filepath.Join(p.Dir, "sites", created.ID, "database.db")); err != nil {
		t.Errorf("site database: %v", err)
	}
	if after := loadSites(t, p); len(after.Sites) != 2 {
		t.Errorf("sites.json = %+v, want both sites", after)
	}
	list := app.view(0).List()
	var found bool
	for _, si := range list {
		if si.ID == created.ID {
			found = true
			if si.Name != "" || si.OpenError != "" || si.NameError == "" {
				t.Errorf("site after a name read error = %+v, want an empty name, no open error, and NameError set", si)
			}
		}
	}
	if !found {
		t.Fatalf("List = %+v, want site %s", list, created.ID)
	}
}

func TestDesktopForgetFailedSite(t *testing.T) {
	t.Run("refuses the only site and an unknown id", func(t *testing.T) {
		p := desktopTestPaths(t)
		if err := desktop.SaveRegistry(p, desktop.Registry{Sites: []desktop.Site{
			{ID: "only1", Dir: ".", Created: time.Now()},
		}}); err != nil {
			t.Fatal(err)
		}
		db := desktop.SitePathsIn(p.Dir).DB
		writeTestFile(t, db, strings.Repeat("this is not a database ", 40))
		app, err := newDesktopApp(p, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.openSites(); err == nil {
			t.Fatal("openSites succeeded when the only site is damaged")
		}
		view := desktopSiteView{app: app, site: app.sites[0]}
		if err := view.Forget("missing"); err == nil || !strings.Contains(err.Error(), "ありません") {
			t.Errorf("Forget(unknown) = %v, want an unknown-id error", err)
		}
		if err := view.Forget(""); err == nil || !strings.Contains(err.Error(), "ありません") {
			t.Errorf("Forget(empty) = %v, want an unknown-id error", err)
		}
		if err := view.Forget("only1"); err == nil || !strings.Contains(err.Error(), "最後") {
			t.Errorf("Forget(only site) = %v, want a refusal to leave zero sites", err)
		}
		if _, err := os.Stat(db); err != nil {
			t.Errorf("only site database after a refused Forget: %v", err)
		}
		if reg := loadSites(t, p); len(reg.Sites) != 1 || reg.Sites[0].ID != "only1" {
			t.Errorf("sites.json after a refused Forget = %+v", reg)
		}
	})

	t.Run("refuses an open site and keeps a failed site folder", func(t *testing.T) {
		p := desktopTestPaths(t)
		writeTwoSites(t, p, freePort(t), "second")
		firstDB := desktop.SitePathsIn(p.Dir).DB
		writeTestFile(t, firstDB, strings.Repeat("this is not a database ", 40))
		app := startDesktopApp(t, p)
		firstPort := app.sites[0].port
		secondPort := app.sites[1].port
		if app.sites[0].openErr == nil || app.sites[1].srv == nil {
			t.Fatalf("open state = first %v, second server %v", app.sites[0].openErr, app.sites[1].srv != nil)
		}
		if err := app.view(0).Forget("second"); err == nil || !strings.Contains(err.Error(), "開いている") {
			t.Errorf("Forget(open site) = %v, want a refusal", err)
		}
		if err := app.view(0).Forget("missing"); err == nil || !strings.Contains(err.Error(), "ありません") {
			t.Errorf("Forget(unknown) = %v", err)
		}
		if reg := loadSites(t, p); len(reg.Sites) != 2 {
			t.Fatalf("sites.json after refused Forget = %+v", reg)
		}
		app.mu.Lock()
		app.reg.Last = "first1"
		app.instancePort = firstPort
		if err := desktop.WriteInstance(p.Instance, desktop.Instance{PID: os.Getpid(), Port: firstPort}); err != nil {
			app.mu.Unlock()
			t.Fatal(err)
		}
		app.mu.Unlock()

		if err := app.view(1).Forget("first1"); err != nil {
			t.Fatalf("Forget(failed first site) = %v", err)
		}
		if data, err := os.ReadFile(firstDB); err != nil || !strings.Contains(string(data), "not a database") {
			t.Errorf("first site database after Forget = %q, %v", data, err)
		}
		if _, err := os.Stat(filepath.Join(p.Dir, "sites", "second")); err != nil {
			t.Errorf("open site folder: %v", err)
		}
		reg := loadSites(t, p)
		if len(reg.Sites) != 1 || reg.Sites[0].ID != "second" || reg.Last != "second" {
			t.Errorf("sites.json after Forget = %+v", reg)
		}
		inst, err := desktop.ReadInstance(p.Instance)
		if err != nil || inst.Port != secondPort {
			t.Errorf("instance.json = %+v, %v, want port %d", inst, err, secondPort)
		}
	})

	t.Run("refuses the first site when nothing else is open", func(t *testing.T) {
		p := desktopTestPaths(t)
		writeTwoSites(t, p, freePort(t), "first1")
		writeTestFile(t, desktop.SitePathsIn(p.Dir).DB, strings.Repeat("broken ", 40))
		secondDB := desktop.SitePathsIn(filepath.Join(p.Dir, "sites", "second")).DB
		if err := os.MkdirAll(filepath.Dir(secondDB), 0o700); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, secondDB, strings.Repeat("broken ", 40))
		app, err := newDesktopApp(p, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.openSites(); err == nil {
			t.Fatal("openSites succeeded when every site is damaged")
		}
		view := desktopSiteView{app: app, site: app.sites[0]}
		if err := view.Forget("first1"); err == nil || !strings.Contains(err.Error(), "ほかにない") {
			t.Errorf("Forget(first site with nothing open) = %v", err)
		}
		if _, err := os.Stat(desktop.SitePathsIn(p.Dir).DB); err != nil {
			t.Errorf("first site database: %v", err)
		}
		if err := view.Forget("second"); err != nil {
			t.Fatalf("Forget(non-first failed site) = %v", err)
		}
		if _, err := os.Stat(secondDB); err != nil {
			t.Errorf("second site database after Forget: %v", err)
		}
		reg := loadSites(t, p)
		if len(reg.Sites) != 1 || reg.Sites[0].ID != "first1" {
			t.Errorf("sites.json = %+v, want only the first site", reg)
		}
	})

	t.Run("updates instance.json when the failed site never listened", func(t *testing.T) {
		p := desktopTestPaths(t)
		recorded := freePort(t)
		writeTwoSites(t, p, recorded, "second")
		db := desktop.SitePathsIn(filepath.Join(p.Dir, "sites", "second")).DB
		writeTestFile(t, db, strings.Repeat("not a database ", 40))
		app := startDesktopApp(t, p)
		app.mu.Lock()
		failed := app.sites[1]
		if failed.openErr == nil || failed.entry.Port != recorded {
			app.mu.Unlock()
			t.Fatalf("failed site = port %d entry %d err %v", failed.port, failed.entry.Port, failed.openErr)
		}
		failed.port = 0
		app.instancePort = recorded
		if err := desktop.WriteInstance(p.Instance, desktop.Instance{PID: os.Getpid(), Port: recorded}); err != nil {
			app.mu.Unlock()
			t.Fatal(err)
		}
		openPort := app.sites[0].port
		app.mu.Unlock()

		if err := app.view(0).Forget("second"); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if _, err := os.Stat(db); err != nil {
			t.Errorf("failed site database after Forget: %v", err)
		}
		inst, err := desktop.ReadInstance(p.Instance)
		if err != nil || inst.Port != openPort {
			t.Errorf("instance.json = %+v, %v, want the open site's port %d (recorded %d)", inst, err, openPort, recorded)
		}
	})
}

func TestDesktopScheduledPostPublishesWhileOtherSiteIsInUse(t *testing.T) {
	p := desktopTestPaths(t)
	app, err := newDesktopApp(p, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	app.schedulerInterval = 20 * time.Millisecond
	if err := app.openSites(); err != nil {
		t.Fatal(err)
	}
	app.start()
	t.Cleanup(func() {
		app.beginStop()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		app.shutdown(ctx)
	})
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	siteB := app.sites[1].db
	if _, err := siteB.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('user-b', 'taro', '太郎', 'x', 'admin')`); err != nil {
		t.Fatal(err)
	}
	var catID int64
	if err := siteB.QueryRow(`SELECT id FROM categories LIMIT 1`).Scan(&catID); err != nil {
		t.Fatal(err)
	}
	// 予約投稿は状態を公開、公開日時を予約した日時として保存する。公開日時を迎えると、サイト B のスケジューラーが
	// 知らせて検索に出す（状態は変えない）。
	due := time.Now().UTC().Add(300 * time.Millisecond)
	if _, err := siteB.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id, status, published_at, updated_at) VALUES ('due-1', '予約の記事', 'scheduled-post', 'user-b', ?, 'published', ?, ?)`,
		catID, due, due.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := app.sites[1].index.Search("予約の記事", 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if res.Total == 1 {
			break
		}
		getAdmin(t, app.sites[0].port, "/admin", "text/html")
		if time.Now().After(deadline) {
			t.Fatal("site B scheduled post was not published to search while site A is in use")
		}
		time.Sleep(30 * time.Millisecond)
	}
	var status string
	if err := siteB.QueryRow(`SELECT status FROM articles WHERE id = 'due-1'`).Scan(&status); err != nil || status != "published" {
		t.Errorf("site B article status = %q, %v; want published", status, err)
	}
	var n int
	if err := app.sites[0].db.QueryRow(`SELECT COUNT(*) FROM articles WHERE id = 'due-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("site A has %d copies of the article published on site B", n)
	}
	if created.ID == "" {
		t.Fatal("site B has no id")
	}
}

func TestDesktopDeleteZipRestoresOnNewSite(t *testing.T) {
	p := desktopTestPaths(t)
	app := startDesktopApp(t, p)
	created, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	reg := loadSites(t, p)
	site := newDesktopHTTP(t, reg.Sites[1].Port)
	site.completeSetup(created.ID)
	upload := filepath.Join(p.Dir, "sites", created.ID, "uploads", "kept.webp")
	writeTestFile(t, upload, "kept-image")
	if _, err := app.sites[1].db.Exec(`INSERT INTO articles (id, title, slug, content_markdown, excerpt, status, author_id, category_id)
		SELECT 'kept-1', '残した記事', 'kept-article', '本文', '', 'draft', id, 1 FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	backupFile, err := app.view(1).Delete(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	zipData, err := os.ReadFile(backupFile)
	if err != nil {
		t.Fatal(err)
	}
	app.deletions.Wait()

	restored, err := app.view(0).Create()
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	for _, s := range loadSites(t, p).Sites {
		if s.ID == restored.ID {
			port = s.Port
		}
	}
	if port == 0 {
		t.Fatalf("restored site %s has no port", restored.ID)
	}
	c := newDesktopHTTP(t, port)
	if res := c.get(server.SetupPath); res.status != http.StatusOK || !strings.Contains(res.body, "バックアップから始める") {
		t.Fatalf("setup page = %d, has restore wording %v", res.status, strings.Contains(res.body, "バックアップから始める"))
	}
	csrfName := auth.CookieNamesFor(restored.ID).CSRF
	res := c.postZip(server.SetupPath+"/restore", csrfName, zipData)
	if res.status != http.StatusOK || !strings.Contains(res.body, "setup_restored") {
		t.Fatalf("restore = %d\n%s", res.status, res.body)
	}
	login := c.login(restored.ID, "hanako", "password123")
	if login.status != http.StatusSeeOther {
		t.Fatalf("login after restore = %d\n%s", login.status, login.body)
	}
	article := c.get("/admin/articles/kept-1/edit")
	if article.status != http.StatusOK || !strings.Contains(article.body, "残した記事") {
		t.Fatalf("restored article page = %d, contains title %v\n%s", article.status, strings.Contains(article.body, "残した記事"), article.body)
	}
	got, err := os.ReadFile(filepath.Join(p.Dir, "sites", restored.ID, "uploads", "kept.webp"))
	if err != nil || string(got) != "kept-image" {
		t.Errorf("restored upload = %q, %v", got, err)
	}
}
