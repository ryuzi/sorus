package backup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/database"
)

var attachmentFiles = map[string]string{
	"ab/abcdef.pdf":  "%PDF-1.7 paper",
	"cd/handout.pdf": "%PDF-1.7 handout",
}

// autoSource は自動バックアップの元（DB・画像・添付資料）と、保存先フォルダ・作業ディレクトリを用意する。
func autoSource(t *testing.T) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	db := openDB(t, filepath.Join(dir, "data", "database.db"), "alice", "自動バックアップのメディア")
	uploads := filepath.Join(dir, "data", "uploads")
	attachments := filepath.Join(dir, "data", "attachments")
	writeFiles(t, uploads, sourceFiles)
	writeFiles(t, attachments, attachmentFiles)
	folder := filepath.Join(dir, "OneDrive", "sorus バックアップ")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{
		DB: db, UploadDir: uploads, AttachmentDir: attachments, WorkDir: filepath.Join(dir, "data"),
		Folder: func() string { return folder },
	}
	return s, folder
}

func TestSchedulerWritesArchiveWithAttachmentsAtomically(t *testing.T) {
	s, folder := autoSource(t)
	var results []Result
	s.OnResult = func(r Result) { results = append(results, r) }
	// 書き終える前の保存先フォルダには、同じフォルダの一時ファイル（"." で始まり ".tmp" で終わる）だけがあり、
	// 完成した名前のファイルも DB の複製も無い。
	var checked bool
	beforePublish = func(tmpPath, dest string) {
		checked = true
		if filepath.Dir(tmpPath) != folder || !strings.HasPrefix(filepath.Base(tmpPath), ".") || !strings.HasSuffix(tmpPath, ".tmp") {
			t.Errorf("temp file %s is not a hidden .tmp file in %s", tmpPath, folder)
		}
		if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists before the archive is complete", dest)
		}
		entries, _ := os.ReadDir(folder)
		if len(entries) != 1 {
			t.Errorf("folder entries while writing = %v, want only the temp file", entries)
		}
	}
	t.Cleanup(func() { beforePublish = nil })
	res, err := s.RunNow()
	if err != nil || res.Err != nil {
		t.Fatalf("RunNow = %+v, %v", res, err)
	}
	if !checked {
		t.Error("beforePublish was not called")
	}
	if len(results) != 1 || results[0].File != res.File {
		t.Fatalf("OnResult = %+v, want the result %+v", results, res)
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(res.File) || !autoNamePattern.MatchString(entries[0].Name()) {
		t.Fatalf("folder entries = %v, want only the archive %s", entries, res.File)
	}
	m, err := ReadManifest(res.File)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.FormatVersion != FormatVersion || m.Attachments == nil || m.Attachments.FileCount != len(attachmentFiles) {
		t.Fatalf("manifest = %+v, want format 2 with %d attachments", m, len(attachmentFiles))
	}
	names := map[string]bool{}
	for _, e := range readEntries(t, res.File) {
		names[e.name] = true
	}
	for _, want := range []string{"attachments/", "attachments/ab/abcdef.pdf", "attachments/cd/handout.pdf", "uploads/notes.txt"} {
		if !names[want] {
			t.Errorf("archive has no entry %q", want)
		}
	}
	// DB の複製を置いた作業ディレクトリも片付いている。
	if leftovers, _ := filepath.Glob(filepath.Join(s.WorkDir, ".sorus-backup-*")); len(leftovers) != 0 {
		t.Errorf("work dir leftovers: %v", leftovers)
	}
}

func TestSchedulerFailureLeavesNoFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("パーミッションで読めないファイルを作れない")
	}
	s, folder := autoSource(t)
	unreadable := filepath.Join(s.AttachmentDir, "secret.pdf")
	if err := os.WriteFile(unreadable, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(unreadable); err == nil {
		f.Close()
		t.Skip("権限に関係なく読める（root で実行している）")
	}
	res, err := s.RunNow()
	if err != nil || res.Err == nil || res.File != "" {
		t.Fatalf("RunNow = %+v, %v; want a failed result", res, err)
	}
	if entries, _ := os.ReadDir(folder); len(entries) != 0 {
		t.Fatalf("folder entries after failure = %v, want none (no half-written file)", entries)
	}
}

func TestSchedulerPrunesToKeepGenerations(t *testing.T) {
	s, folder := autoSource(t)
	writeFiles(t, folder, map[string]string{"メモ.txt": "利用者のファイル", "sorus_backup_old.zip": "名前の違う Zip"})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	for i := 0; i < DefaultKeep+2; i++ {
		if res, err := s.RunNow(); err != nil || res.Err != nil || res.PruneErr != nil {
			t.Fatalf("run %d: %+v, %v", i, res, err)
		}
		now = now.Add(DailyInterval)
	}
	list, err := listAuto(folder, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != DefaultKeep {
		t.Fatalf("kept %d backups, want %d", len(list), DefaultKeep)
	}
	if first := list[0].name; first != "sorus_backup_20261003_090000.zip" {
		t.Errorf("oldest kept = %s, want the 3rd day's backup", first)
	}
	for _, name := range []string{"メモ.txt", "sorus_backup_old.zip"} {
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	latest, ok, err := Latest(folder, "")
	if err != nil || !ok || !latest.Equal(now.Add(-DailyInterval)) {
		t.Errorf("Latest = %v, %v, %v", latest, ok, err)
	}
}

func TestSchedulerSameSecondGetsUniqueName(t *testing.T) {
	s, folder := autoSource(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return at }
	var files []string
	for i := 0; i < 2; i++ {
		res, err := s.RunNow()
		if err != nil || res.Err != nil {
			t.Fatalf("run %d: %+v, %v", i, res, err)
		}
		files = append(files, filepath.Base(res.File))
	}
	if files[0] != "sorus_backup_20261001_090000.zip" || files[1] != "sorus_backup_20261001_090000-2.zip" {
		t.Fatalf("files = %v", files)
	}
	list, _ := listAuto(folder, "")
	if len(list) != 2 || list[1].name != files[1] {
		t.Fatalf("listAuto = %+v, want the -2 file last", list)
	}
}

func TestSchedulerConcurrentTriggerRunsOnce(t *testing.T) {
	s, folder := autoSource(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.beforeCreate = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.RunNow()
		done <- err
	}()
	<-entered
	if _, err := s.RunNow(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second RunNow while running = %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first RunNow: %v", err)
	}
	if list, _ := listAuto(folder, ""); len(list) != 1 {
		t.Fatalf("backups = %d, want 1", len(list))
	}
}

func TestAutoBackupPerSite(t *testing.T) {
	s, folder := autoSource(t)
	seed := map[string]string{
		"sorus_backup_abc_20261001_090000.zip":  "abc-1",
		"sorus_backup_20261002_090000.zip":      "empty-1",
		"sorus_backup_xyz_20261003_090000.zip":  "xyz-1",
		"sorus_backup_abc_20261004_090000.zip":  "abc-2",
		"sorus_backup_20261005_090000.zip":      "empty-2",
		"sorus_backup_xyz_20261006_090000.zip":  "xyz-2",
		"sorus_backup_1bad_20261001_090000.zip": "数字始まりのキーではないファイル",
		"メモ.txt":                                "利用者のファイル",
	}
	writeFiles(t, folder, seed)

	wantLatest := map[string]time.Time{
		"":    time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC),
		"abc": time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC),
		"xyz": time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC),
	}
	for site, want := range wantLatest {
		got, ok, err := Latest(folder, site)
		if err != nil || !ok || !got.Equal(want) {
			t.Errorf("Latest(%q) = %v, %v, %v; want %v", site, got, ok, err, want)
		}
	}
	for _, unknown := range []string{"none", "1bad"} {
		if got, ok, err := Latest(folder, unknown); err != nil || ok {
			t.Errorf("Latest(%q) = %v, %v, %v; want no backup", unknown, got, ok, err)
		}
	}

	assertFiles := func(want ...string) {
		t.Helper()
		entries, err := os.ReadDir(folder)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("folder files = %v, want %v", got, want)
		}
	}

	// 空キーの整理: より古い abc (10/01) も、より新しい xyz (10/06) も巻き込まず、空キーの古い方 (10/02) だけを消す。
	if err := Prune(folder, "", 1); err != nil {
		t.Fatalf("Prune(empty): %v", err)
	}
	assertFiles(
		"sorus_backup_1bad_20261001_090000.zip",
		"sorus_backup_20261005_090000.zip",
		"sorus_backup_abc_20261001_090000.zip",
		"sorus_backup_abc_20261004_090000.zip",
		"sorus_backup_xyz_20261003_090000.zip",
		"sorus_backup_xyz_20261006_090000.zip",
		"メモ.txt",
	)

	if err := Prune(folder, "abc", 1); err != nil {
		t.Fatalf("Prune(abc): %v", err)
	}
	assertFiles(
		"sorus_backup_1bad_20261001_090000.zip",
		"sorus_backup_20261005_090000.zip",
		"sorus_backup_abc_20261004_090000.zip",
		"sorus_backup_xyz_20261003_090000.zip",
		"sorus_backup_xyz_20261006_090000.zip",
		"メモ.txt",
	)

	if err := Prune(folder, "xyz", 1); err != nil {
		t.Fatalf("Prune(xyz): %v", err)
	}
	assertFiles(
		"sorus_backup_1bad_20261001_090000.zip",
		"sorus_backup_20261005_090000.zip",
		"sorus_backup_abc_20261004_090000.zip",
		"sorus_backup_xyz_20261006_090000.zip",
		"メモ.txt",
	)

	// サイト xyz のバックアップが30分前にあっても、サイト abc の Due は自分の最新（2日前）だけを見る。
	now := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	s.Site = "abc"
	s.Keep = 1
	s.Now = func() time.Time { return now }
	if !s.Due(StartupInterval) {
		t.Error("Due(abc) = false, want true when only xyz has a recent backup")
	}

	res, err := s.RunNow()
	if err != nil || res.Err != nil || res.PruneErr != nil {
		t.Fatalf("RunNow(abc) = %+v, %v", res, err)
	}
	if got := filepath.Base(res.File); got != "sorus_backup_abc_20261006_093000.zip" {
		t.Fatalf("created file = %s, want sorus_backup_abc_20261006_093000.zip", got)
	}
	assertFiles(
		"sorus_backup_1bad_20261001_090000.zip",
		"sorus_backup_20261005_090000.zip",
		"sorus_backup_abc_20261006_093000.zip",
		"sorus_backup_xyz_20261006_090000.zip",
		"メモ.txt",
	)

	// 同じ秒に2回目を作ると -2 が付き、abc のファイルだけが数えられる。
	s.Keep = 2
	res2, err := s.RunNow()
	if err != nil || res2.Err != nil || res2.PruneErr != nil {
		t.Fatalf("second RunNow(abc) = %+v, %v", res2, err)
	}
	if got := filepath.Base(res2.File); got != "sorus_backup_abc_20261006_093000-2.zip" {
		t.Fatalf("second created file = %s, want sorus_backup_abc_20261006_093000-2.zip", got)
	}
	assertFiles(
		"sorus_backup_1bad_20261001_090000.zip",
		"sorus_backup_20261005_090000.zip",
		"sorus_backup_abc_20261006_093000-2.zip",
		"sorus_backup_abc_20261006_093000.zip",
		"sorus_backup_xyz_20261006_090000.zip",
		"メモ.txt",
	)
}

func TestSchedulerRespectsRestoreLockAndFolder(t *testing.T) {
	s, folder := autoSource(t)
	s.TryLock = func() (func(), bool) { return nil, false }
	if _, err := s.RunNow(); !errors.Is(err, ErrBusy) {
		t.Fatalf("RunNow while restoring = %v, want ErrBusy", err)
	}
	s.TryLock = nil
	s.Folder = func() string { return "" }
	if _, err := s.RunNow(); !errors.Is(err, ErrNoFolder) {
		t.Fatalf("RunNow without folder = %v, want ErrNoFolder", err)
	}
	if s.Due(time.Hour) {
		t.Error("Due without folder = true")
	}
	s.Folder = func() string { return folder }
	if !s.Due(StartupInterval) {
		t.Error("Due with no backups = false")
	}
	if _, err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	if s.Due(StartupInterval) {
		t.Error("Due right after a backup = true")
	}
	now := time.Now().Add(DailyInterval + time.Minute)
	s.Now = func() time.Time { return now }
	if !s.Due(DailyInterval) {
		t.Error("Due a day later = false")
	}
	// 無くなったフォルダは作ってみて、失敗を知らせる。
	s.Folder = func() string { return filepath.Join(folder, "消えたフォルダ") }
	if !s.Due(DailyInterval) {
		t.Error("Due with a missing folder = false")
	}
	res, err := s.RunNow()
	if err != nil || res.Err == nil {
		t.Fatalf("RunNow with a missing folder = %+v, %v; want a failed result", res, err)
	}
}

func TestRestoreLiveWithReplacesAttachments(t *testing.T) {
	s, _ := autoSource(t)
	res, err := s.RunNow()
	if err != nil || res.Err != nil {
		t.Fatalf("RunNow = %+v, %v", res, err)
	}
	tg := newTarget(t)
	attach := filepath.Join(tg.dir, "attachments")
	writeFiles(t, attach, map[string]string{"zz/old.pdf": "old"})
	live, err := database.Connect(tg.db)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := RestoreLiveWith(live, res.File, RestoreTargets{UploadDir: tg.uploads, AttachmentDir: attach}); err != nil {
		t.Fatalf("RestoreLiveWith: %v", err)
	}
	assertTree(t, attach, attachmentFiles)
	assertTree(t, tg.uploads, sourceFiles)
	prev, _ := filepath.Glob(filepath.Join(tg.dir, "attachments"+PreRestoreInfix+"*"))
	if len(prev) != 1 {
		t.Fatalf("previous attachments = %v, want 1", prev)
	}
	assertTree(t, prev[0], map[string]string{"zz/old.pdf": "old"})
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(attach, "ab", "abcdef.pdf"))
		if err != nil || info.Mode().Perm() != attachFileMode {
			t.Errorf("restored attachment perm = %v, %v; want %o", info, err, attachFileMode)
		}
	}
}

func TestRestoreWithOldArchiveKeepsAttachments(t *testing.T) {
	zipPath := newSource(t) // Create（添付資料なし・形式のバージョン1）。以前の sorus が作ったバックアップと同じ形。
	tg := newTarget(t)
	attach := filepath.Join(tg.dir, "attachments")
	writeFiles(t, attach, map[string]string{"zz/keep.pdf": "keep"})
	if err := RestoreWith(zipPath, RestoreTargets{DBPath: tg.db, UploadDir: tg.uploads, AttachmentDir: attach}); err != nil {
		t.Fatalf("RestoreWith: %v", err)
	}
	if got := queryValue(t, tg.db, "SELECT username FROM users"); got != "alice" {
		t.Errorf("restored users = %q, want alice", got)
	}
	assertTree(t, attach, map[string]string{"zz/keep.pdf": "keep"})
	if prev, _ := filepath.Glob(filepath.Join(tg.dir, "attachments"+PreRestoreInfix+"*")); len(prev) != 0 {
		t.Errorf("attachments were moved aside: %v", prev)
	}
}

func TestRestoreRejectsUnlistedAttachments(t *testing.T) {
	cases := map[string]func(m *Manifest){
		// バージョン1の manifest に添付資料のエントリがある。
		"v1 with attachments": func(m *Manifest) { m.FormatVersion = 1; m.Attachments = nil },
		"count mismatch":      func(m *Manifest) { m.Attachments.FileCount++ },
		"v1 listing them":     func(m *Manifest) { m.FormatVersion = 1 },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := autoSource(t)
			res, err := s.RunNow()
			if err != nil || res.Err != nil {
				t.Fatalf("RunNow = %+v, %v", res, err)
			}
			bad := filepath.Join(t.TempDir(), "bad.zip")
			writeEntries(t, bad, editManifest(t, readEntries(t, res.File), edit))
			tg := newTarget(t)
			err = RestoreWith(bad, RestoreTargets{DBPath: tg.db, UploadDir: tg.uploads, AttachmentDir: filepath.Join(tg.dir, "attachments")})
			if !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("RestoreWith = %v, want ErrInvalidArchive", err)
			}
			tg.assertUnchanged(t)
			if !strings.Contains(err.Error(), "attachments") {
				t.Errorf("error %q does not mention attachments", err)
			}
		})
	}
}

func TestCloudFoldersFindsSyncFolders(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"OneDrive", "Dropbox", "Library/CloudStorage/GoogleDrive-a@example.com/マイドライブ", "Documents"} {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"OneDrive": filepath.Join(home, "OneDrive")}
	got := cloudFoldersFor("darwin", home, func(k string) string { return env[k] }, isDir)
	want := []CloudFolder{
		{Service: "OneDrive", Path: filepath.Join(home, "OneDrive")},
		{Service: "Google ドライブ", Path: filepath.Join(home, "Library", "CloudStorage", "GoogleDrive-a@example.com", "マイドライブ")},
		{Service: "Dropbox", Path: filepath.Join(home, "Dropbox")},
	}
	if len(got) != len(want) {
		t.Fatalf("CloudFolders = %+v, want %+v", got, want)
	}
	for i, w := range want {
		if got[i].Service != w.Service || got[i].Path != w.Path || got[i].Suggested != filepath.Join(w.Path, FolderName) {
			t.Errorf("CloudFolders[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	if none := cloudFoldersFor("linux", "", func(string) string { return "" }, isDir); len(none) != 0 {
		t.Errorf("CloudFolders without home = %+v", none)
	}
}
