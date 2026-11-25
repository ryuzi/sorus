package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/backup"
)

// downloadBackup は管理画面のボタンと同じフォーム送信でバックアップをダウンロードし、一時ファイルに保存して返す。
func (c *client) downloadBackup() string {
	c.t.Helper()
	res := c.postForm(backupDownloadPath, nil)
	expectStatus(c.t, res, http.StatusOK)
	p := filepath.Join(c.t.TempDir(), "downloaded.zip")
	if err := os.WriteFile(p, []byte(res.Body), 0o600); err != nil {
		c.t.Fatal(err)
	}
	return p
}

// postRestore は復元フォームと同じく multipart/form-data で backup フィールドに data を送る。
// withHeader が true なら fetch と同じく X-CSRF-Token ヘッダーを付け、false ならフォームの _csrf フィールドだけで送る。
func (c *client) postRestore(data []byte, withHeader bool) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField(CSRFFormField, c.csrfToken())
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="backup"; filename="sorus_backup.zip"`)
	h.Set("Content-Type", "application/zip")
	fw, err := mw.CreatePart(h)
	if err != nil {
		c.t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+backupRestorePath, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	if withHeader {
		req.Header.Set(CSRFHeaderName, c.csrfToken())
	}
	return c.do(req)
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (e *testEnv) writeUpload(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.uploadDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// insertPublished は公開中の記事を直接登録し、検索インデックスへ反映する。
func (e *testEnv) insertPublished(id, title, slug string) {
	e.t.Helper()
	if _, err := e.db.Exec(`INSERT INTO articles (id, title, slug, content_markdown, excerpt, status, author_id, category_id, published_at)
		VALUES (?, ?, ?, '本文', '', ?, ?, 1, ?)`, id, title, slug, article.StatusPublished, chief.ID, time.Now().Add(-time.Hour).UTC()); err != nil {
		e.t.Fatal(err)
	}
	if err := e.server.search.SyncFromDB(context.Background(), e.db); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) searchTotal(q string) uint64 {
	e.t.Helper()
	res, err := e.server.search.Search(q, 1, 10)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Total
}

// markCurrent は「今のデータ」の目印（設定 restore_marker と画像 current.webp）を作る。バックアップを取ったあとに
// 呼べば、復元が誤って成功したときに目印が消えるので、expectUnchangedAfterFailedRestore で見分けられる。
func (e *testEnv) markCurrent() {
	e.t.Helper()
	if _, err := e.db.Exec(`INSERT INTO settings (key, value) VALUES ('restore_marker', 'current')`); err != nil {
		e.t.Fatal(err)
	}
	e.writeUpload("current.webp", "current")
}

// expectUnchangedAfterFailedRestore は失敗した復元のあとで DB・アップロード画像が markCurrent のままで、退避先も残らないことを確かめる。
func (e *testEnv) expectUnchangedAfterFailedRestore() {
	e.t.Helper()
	if n := e.countRows(`SELECT COUNT(*) FROM settings WHERE key = 'restore_marker' AND value = 'current'`); n != 1 {
		e.t.Errorf("restore_marker missing: database changed by a failed restore")
	}
	if data, err := os.ReadFile(filepath.Join(e.uploadDir, "current.webp")); err != nil || string(data) != "current" {
		e.t.Errorf("uploads/current.webp = %q (%v), want unchanged", data, err)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(e.uploadDir), "*"+backup.PreRestoreInfix+"*")); len(m) != 0 {
		e.t.Errorf("failed restore left previous data: %v", m)
	}
}

func TestBackupSettingsPage(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	res := c.get(backupPagePath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `action="/admin/settings/backup/download"`, `action="/admin/settings/backup/restore"`,
		`name="backup"`, `id="restore-confirm"`, "4 GB", `href="/admin/settings"`)
	// 独自テーマの案内は、削除の予定がない人にも当てはまる言い方にし、
	// 同じサイトへの復元ではいまのテーマが残ることと矛盾させない。
	expectContains(t, res.Body,
		"独自テーマのファイルはバックアップに含まれません。",
		"取り込み元の URL または Zip は、バックアップといっしょに保管してください。",
		"復元しても、いま登録されている独自テーマはそのまま残ります。",
		"別のパソコンやサーバーへ移したときなど、復元先にテーマがない場合は、保管した URL または Zip から再登録してください。")
	expectNotContains(t, res.Body, "削除する前に")

	// 独自テーマの案内は英語表示でも確認できる。
	c.get("/admin/set-lang?lang=en&return=/admin")
	enRes := c.get(backupPagePath)
	expectContains(t, enRes.Body,
		"Custom theme files are not included in backups.",
		"Keep the source URL or Zip file together with your backup.",
		"Restoring keeps the custom themes already on this site.",
		"If the theme is missing where you restore, such as after moving to another computer or server, add it again from the saved URL or Zip file.")
	expectNotContains(t, enRes.Body, "Before deleting this site")
	c.get("/admin/set-lang?lang=ja&return=/admin")

	// 管理者以外には画面もナビゲーションのリンクも出さない。
	for _, u := range []testUser{studentA, chief} {
		cu := env.loggedIn(u)
		expectStatus(t, cu.get(backupPagePath), http.StatusForbidden)
		if strings.Contains(cu.get("/admin/articles").Body, `href="/admin/settings/backup"`) {
			t.Errorf("%s sees the backup link in the navigation", u.Role)
		}
	}
	expectRedirect(t, env.newClient().get(backupPagePath), "/admin/login")
}

func TestBackupSettingsPageWarnsAboutSecretsAndSharingAlternatives(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	ja := c.get(backupPagePath)
	expectContains(t, ja.Body,
		"パスワードや API キーなどの秘密情報が含まれます。人に渡さないでください。",
		"サイトを渡すときは「今のサイトを Zip ファイルでダウンロード」を使ってください。",
		"このバックアップには、パスワードや API キーなどの秘密情報が含まれます。人に渡さないでください。")
	expectNotContainsAny(t, "backup page (ja)", ja.Body, "原稿ファイル", "（.sorus）")
	c.get("/admin/set-lang?lang=en&return=/admin")
	en := c.get(backupPagePath)
	expectContains(t, en.Body,
		"This backup contains passwords and API keys. Do not share it.",
		"To share the site, use “Download the site as a Zip file.”")
	expectNotContainsAny(t, "backup page (en)", en.Body, "manuscript file", "(.sorus)")
}

func TestBackupDownloadCreatesCompleteArchive(t *testing.T) {
	env := newTestEnv(t)
	env.writeUpload("2026/10/photo-1200.webp", "photo")
	env.writeUpload("favicon-0123456789abcdef.svg", "<svg/>")
	c := env.loggedIn(admin)

	res := c.postForm(backupDownloadPath, nil)
	expectStatus(t, res, http.StatusOK)
	if ct := res.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if !regexp.MustCompile(`^attachment; filename="sorus_backup_\d{8}_\d{6}\.zip"$`).MatchString(res.Header.Get("Content-Disposition")) {
		t.Errorf("Content-Disposition = %q", res.Header.Get("Content-Disposition"))
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if res.Header.Get("Content-Encoding") != "" {
		t.Errorf("zip was re-compressed: Content-Encoding = %q", res.Header.Get("Content-Encoding"))
	}

	zr, err := zip.NewReader(bytes.NewReader([]byte(res.Body)), int64(len(res.Body)))
	if err != nil {
		t.Fatalf("downloaded body is not a zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{backup.ManifestName, backup.DatabaseName, "uploads/2026/10/photo-1200.webp", "uploads/favicon-0123456789abcdef.svg"} {
		if !names[want] {
			t.Errorf("archive has no %s (entries: %v)", want, names)
		}
	}
	p := filepath.Join(t.TempDir(), "b.zip")
	os.WriteFile(p, []byte(res.Body), 0o600)
	m, err := backup.ReadManifest(p)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Uploads.FileCount != 2 {
		t.Errorf("file_count = %d, want 2", m.Uploads.FileCount)
	}
	// 作成に使った一時ファイルはサーバーに残さない。
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "sorus-backup-*", "*.zip")); len(left) != 0 {
		t.Errorf("temporary backups left: %v", left)
	}
}

func TestBackupDownloadForbiddenForNonAdmins(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{studentA, chief} {
		res := env.loggedIn(u).postForm(backupDownloadPath, nil)
		expectStatus(t, res, http.StatusForbidden)
		if strings.HasPrefix(res.Body, "PK") {
			t.Errorf("%s received a backup archive", u.Role)
		}
	}
	anon := env.newClient()
	expectStatus(t, anon.postForm(backupDownloadPath, nil), http.StatusUnauthorized)
	// CSRF トークンの無い送信は管理者でも拒否する。
	expectStatus(t, env.loggedIn(admin).postRaw(backupDownloadPath, url.Values{}), http.StatusForbidden)
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	// バックアップ時点の状態: サイト名「バックアップ時点」、画像 kept.webp、公開記事「残る記事」。
	expectRedirect(t, c.postForm("/admin/settings/general", generalSettingsForm()), "/admin/settings/general")
	env.writeUpload("2026/10/kept.webp", "kept")
	env.insertPublished("kept-1", "残る記事", "kept-article")
	zipPath := c.downloadBackup()
	wantSiteName := env.setting("site_name")

	// バックアップ後の変更: サイト名・画像・記事を変える。
	form := generalSettingsForm()
	form.Set("site_name", "バックアップ後の名前")
	expectRedirect(t, c.postForm("/admin/settings/general", form), "/admin/settings/general")
	os.Remove(filepath.Join(env.uploadDir, "2026", "10", "kept.webp"))
	env.writeUpload("after.webp", "after")
	env.insertPublished("later-1", "後から書いた記事", "later-article")
	if env.searchTotal("後から書いた記事") == 0 {
		t.Fatal("article added after the backup is not searchable before restore")
	}

	res := c.postRestore(readFile(t, zipPath), true)
	expectStatus(t, res, http.StatusOK)
	var body backupRestoreResponse
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("restore response: %v\n%s", err, res.Body)
	}
	// バックアップにはログインセッションを含めないため、復元したデータのアカウントでログインし直す。
	if body.Redirect != "/admin/login?flash=backup_relogin" || body.Message == "" {
		t.Errorf("restore response = %+v", body)
	}
	expectRedirect(t, c.get(backupPagePath), "/admin/login")
	expectContains(t, env.newClient().get(body.Redirect).Body, "もう一度ログインしてください")
	c = env.loggedIn(admin)

	if got := env.setting("site_name"); got != wantSiteName {
		t.Errorf("site_name = %q, want %q", got, wantSiteName)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE id = 'later-1'`); n != 0 {
		t.Errorf("article added after the backup still exists")
	}
	if data, err := os.ReadFile(filepath.Join(env.uploadDir, "2026", "10", "kept.webp")); err != nil || string(data) != "kept" {
		t.Errorf("kept.webp = %q (%v), want restored", data, err)
	}
	if _, err := os.Stat(filepath.Join(env.uploadDir, "after.webp")); !os.IsNotExist(err) {
		t.Errorf("after.webp still exists after restore (err=%v)", err)
	}
	// 検索インデックス・メモリ上のサイト設定・公開ページも復元した内容に揃う。
	if env.searchTotal("後から書いた記事") != 0 || env.searchTotal("残る記事") == 0 {
		t.Errorf("search index not rebuilt: later=%d kept=%d", env.searchTotal("後から書いた記事"), env.searchTotal("残る記事"))
	}
	expectContains(t, env.newClient().get("/").Body, wantSiteName)
	expectStatus(t, env.newClient().get("/uploads/2026/10/kept.webp"), http.StatusOK)

	// 置き換える前の DB と画像は退避先に残る。
	prev, _ := filepath.Glob(filepath.Join(filepath.Dir(env.uploadDir), "uploads"+backup.PreRestoreInfix+"*"))
	if len(prev) != 1 {
		t.Fatalf("previous uploads = %v, want 1", prev)
	}
	if data, err := os.ReadFile(filepath.Join(prev[0], "after.webp")); err != nil || string(data) != "after" {
		t.Errorf("previous uploads/after.webp = %q (%v)", data, err)
	}
	if prevDB, _ := filepath.Glob(filepath.Join(filepath.Dir(env.uploadDir), "test.db"+backup.PreRestoreInfix+"*")); len(prevDB) != 1 {
		t.Errorf("previous db = %v, want 1", prevDB)
	}

	// 復元後もサーバーはそのまま使い続けられる。
	expectStatus(t, c.get(backupPagePath+"?flash=backup_restored"), http.StatusOK)
	expectRedirect(t, c.postForm("/admin/settings/general", generalSettingsForm()), "/admin/settings/general")
}

func TestBackupDownloadExcludesLoginSessions(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	session := c.cookie(auth.SessionCookieName)
	if session == "" {
		t.Fatal("no session cookie after login")
	}
	data := readFile(t, c.downloadBackup())
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name != backup.DatabaseName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		db, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Contains(db, []byte(session)) {
			t.Error("downloaded backup contains the admin's session id")
		}
	}
	// ダウンロードしても今のログインは続く。
	expectStatus(t, c.get(backupPagePath), http.StatusOK)
}

func TestBackupRestoreRejectsZipSlip(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	zipPath := c.downloadBackup()
	env.markCurrent()

	// 正しいバックアップに、保存先の外へ書き込むエントリを加える（件数は manifest.json に合わせる）。
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		if f.Name == backup.ManifestName {
			continue
		}
		if err := zw.Copy(f); err != nil {
			t.Fatal(err)
		}
	}
	m, err := backup.ReadManifest(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zr.Close()
	w, _ := zw.Create("uploads/../../evil.txt")
	w.Write([]byte("evil"))
	m.Uploads.FileCount++
	m.Uploads.TotalSize += 4
	mw, _ := zw.Create(backup.ManifestName)
	json.NewEncoder(mw).Encode(m)
	zw.Close()

	res := c.postRestore(buf.Bytes(), true)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "保存先の外へ書き込もうとする項目")
	env.expectUnchangedAfterFailedRestore()
	root := filepath.Dir(env.uploadDir)
	for _, p := range []string{filepath.Join(root, "evil.txt"), filepath.Join(filepath.Dir(root), "evil.txt")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written outside the upload directory", p)
		}
	}
}

func TestBackupRestoreRejectsCorruptedZip(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	valid := readFile(t, c.downloadBackup())
	env.markCurrent()

	for name, data := range map[string][]byte{
		"not a zip": []byte("this is not a zip file"),
		"truncated": valid[:len(valid)/2],
	} {
		t.Run(name, func(t *testing.T) {
			res := c.postRestore(data, true)
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, res.Body, "バックアップファイルとして読み取れませんでした")
			env.expectUnchangedAfterFailedRestore()
		})
	}
}

func TestBackupRestoreRequiresCSRFHeader(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	valid := readFile(t, c.downloadBackup())
	env.markCurrent()

	// フォームの _csrf フィールドだけでは受け付けない（本文を読む前に拒否する）。
	expectStatus(t, c.postRestore(valid, false), http.StatusForbidden)
	env.expectUnchangedAfterFailedRestore()
}

func TestBackupRestoreForbiddenForNonAdmins(t *testing.T) {
	env := newTestEnv(t)
	valid := readFile(t, env.loggedIn(admin).downloadBackup())
	env.markCurrent()
	for _, u := range []testUser{studentA, chief} {
		expectStatus(t, env.loggedIn(u).postRestore(valid, true), http.StatusForbidden)
	}
	// 未ログインでも CSRF トークン自体はログイン画面で手に入るため、ヘッダーを付けた要求はログインの確認で拒否する。
	expectStatus(t, env.newClient().postRestore(valid, true), http.StatusUnauthorized)
	env.expectUnchangedAfterFailedRestore()
}

func TestBackupRestoreRequestErrors(t *testing.T) {
	env := newTestEnvWith(t, Config{MaxRestoreSize: 64 << 10}, nil)
	env.markCurrent()
	c := env.loggedIn(admin)

	// ファイルが無い（空の）送信。
	expectStatus(t, c.postRestore(nil, true), http.StatusBadRequest)
	// 上限を超えるファイル。
	expectStatus(t, c.postRestore(bytes.Repeat([]byte("x"), 200<<10), true), http.StatusRequestEntityTooLarge)
	env.expectUnchangedAfterFailedRestore()
	expectContains(t, c.get(backupPagePath).Body, "64.0 KB")
}
