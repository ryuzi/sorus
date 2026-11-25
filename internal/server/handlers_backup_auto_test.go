package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"sorus/internal/backup"
)

// writeAttachment は添付資料のディレクトリに rel のファイルを作る。
func (e *testEnv) writeAttachment(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.attachmentDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		e.t.Fatal(err)
	}
}

// autoBackups は folder にある自動バックアップのファイル名を返す。
func autoBackups(t *testing.T, folder string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(folder, "sorus_backup_*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDesktopAutoBackupChooseFolderAndRun(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	env.writeUpload("2026/10/photo.webp", "photo")
	env.writeAttachment("ab/paper.pdf", "%PDF paper")

	// まだ保存先が無い: ダッシュボードの部分は選ぶように勧め、設定画面に自動バックアップの欄が出る。
	expectContains(t, c.get("/admin").Body, `data-desktop-part="/admin/backup/status"`, `src="/static/js/desktop_care.js?v=`)
	expectContains(t, c.get(backupStatusPath).Body, "バックアップ先がまだ決まっていません", "バックアップ先を選ぶ")
	backupPage := c.get(backupPagePath).Body
	expectContains(t, backupPage, `id="auto-backup"`, `action="/admin/settings/backup/auto"`, "OneDrive",
		"自動バックアップにもパスワードや API キーなどの秘密情報が含まれます。人に渡さないでください。",
		"サイトを渡すときは「今のサイトを Zip ファイルでダウンロード」を使ってください。")
	expectNotContainsAny(t, "backup page", backupPage, "原稿ファイル", "（.sorus）")

	// 相対パスや書き込めない場所は保存しない。
	res := c.postForm(autoBackupPath, url.Values{"folder": {"backups"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "最初から入力してください", `value="backups"`)
	if v := env.server.getSetting(autoBackupFolderSetting, ""); v != "" {
		t.Fatalf("relative folder was saved: %q", v)
	}

	folder := filepath.Join(t.TempDir(), "OneDrive", backup.FolderName)
	expectRedirect(t, c.postForm(autoBackupPath, url.Values{"folder": {folder}}), backupPagePath+"?flash=backup_auto_saved")
	files := autoBackups(t, folder)
	if len(files) != 1 {
		t.Fatalf("backups after choosing the folder = %v, want 1", files)
	}
	m, err := backup.ReadManifest(files[0])
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Attachments == nil || m.Attachments.FileCount != 1 || m.Uploads.FileCount != 1 {
		t.Errorf("manifest = %+v, want 1 upload and 1 attachment", m)
	}
	// 同期フォルダには完成した Zip だけが置かれる（DB の複製や一時ファイルを残さない）。
	if entries, _ := os.ReadDir(folder); len(entries) != 1 {
		t.Errorf("folder entries = %v", entries)
	}

	status := c.get(backupStatusPath).Body
	// 成功は日時で分かるので「うまくいきました」は添えない。
	expectContains(t, status, `data-backup-result="ok"`, "最後のバックアップ: ", folder)
	expectNotContains(t, status, "うまくいきました")
	page := c.get(backupPagePath).Body
	expectContains(t, page, "今すぐバックアップする", "最後のバックアップ: ", folder)
	expectNotContains(t, page, "うまくいきました")

	expectRedirect(t, c.postForm(autoBackupRunPath, nil), backupPagePath+"?flash=backup_auto_done")
	if files := autoBackups(t, folder); len(files) != 2 {
		t.Fatalf("backups after back up now = %v, want 2", files)
	}

	expectRedirect(t, c.postForm(autoBackupOffPath, nil), backupPagePath+"?flash=backup_auto_off")
	expectContains(t, c.get(backupStatusPath).Body, "バックアップ先がまだ決まっていません")
	if files := autoBackups(t, folder); len(files) != 2 {
		t.Errorf("stopping removed backups: %v", files)
	}
}

func TestDesktopAutoBackupUsesSiteKey(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "OneDrive", backup.FolderName)
	runForSite := func(key string) {
		t.Helper()
		env := newDesktopSiteEnv(t, key)
		c := env.newClient()
		csrfName := env.server.cookies.CSRF
		expectStatus(t, c.get(SetupPath), http.StatusOK)
		form := setupForm()
		form.Set(CSRFFormField, c.cookie(csrfName))
		expectRedirect(t, c.postRaw(SetupPath, form), "/admin")

		saveForm := url.Values{"folder": {folder}, CSRFFormField: {c.cookie(csrfName)}}
		expectRedirect(t, c.postRaw(autoBackupPath, saveForm), backupPagePath+"?flash=backup_auto_saved")
	}

	runForSite("")
	firstSiteFiles, err := filepath.Glob(filepath.Join(folder, "sorus_backup_2*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(firstSiteFiles) != 1 {
		t.Fatalf("backups for empty SiteKey = %v, want 1 sorus_backup_<timestamp>.zip", autoBackups(t, folder))
	}

	runForSite("abc")
	abcFiles, err := filepath.Glob(filepath.Join(folder, "sorus_backup_abc_*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(abcFiles) != 1 {
		t.Fatalf("backups for SiteKey abc = %v, want 1 sorus_backup_abc_*.zip", autoBackups(t, folder))
	}
	if all := autoBackups(t, folder); len(all) != 2 {
		t.Fatalf("total backups in shared folder = %v, want 2", all)
	}
}

func TestDesktopAutoBackupFailureIsShownPlainly(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	folder := filepath.Join(t.TempDir(), "Dropbox", backup.FolderName)
	expectRedirect(t, c.postForm(autoBackupPath, url.Values{"folder": {folder}}), backupPagePath)
	// 同期フォルダが消えた（同期アプリを外した・ドライブを外した）。
	if err := os.RemoveAll(filepath.Dir(folder)); err != nil {
		t.Fatal(err)
	}
	expectRedirect(t, c.postForm(autoBackupRunPath, nil), backupPagePath+"?flash=backup_auto_failed")
	status := c.get(backupStatusPath).Body
	expectContains(t, status, `data-backup-result="failed"`, "バックアップに失敗しました", "バックアップ先のフォルダが見つかりません")
	if strings.Contains(status, "no such file") {
		t.Errorf("status shows a raw error: %s", status)
	}
	expectContains(t, c.get(backupPagePath).Body, "バックアップ先のフォルダが見つかりません")
	// アプリは動き続ける。
	expectStatus(t, c.get("/admin"), http.StatusOK)
}

func TestAutoBackupIsDesktopOnly(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	expectStatus(t, c.get(backupStatusPath), http.StatusNotFound)
	expectStatus(t, c.postForm(autoBackupPath, url.Values{"folder": {t.TempDir()}}), http.StatusNotFound)
	body := c.get(backupPagePath).Body
	if strings.Contains(body, `id="auto-backup"`) {
		t.Error("server mode shows the automatic backup section")
	}
	if strings.Contains(c.get("/admin").Body, "data-desktop-part") {
		t.Error("server mode dashboard loads desktop parts")
	}
}

func TestBackupDownloadIncludesAttachments(t *testing.T) {
	env := newTestEnv(t)
	env.writeAttachment("ab/paper.pdf", "%PDF paper")
	zipPath := env.loggedIn(admin).downloadBackup()
	m, err := backup.ReadManifest(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.Attachments == nil || m.Attachments.FileCount != 1 {
		t.Fatalf("manifest attachments = %+v, want 1 file", m.Attachments)
	}
}

// postRestoreTo は復元フォームと同じ multipart で path に data を送る。CSRF トークンはヘッダーで送る。
func (c *client) postRestoreTo(path string, data []byte) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="backup"; filename="sorus_backup.zip"`)
	h.Set("Content-Type", "application/zip")
	fw, err := mw.CreatePart(h)
	if err != nil {
		c.t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set(CSRFHeaderName, c.csrfToken())
	return c.do(req)
}

// movingBackup は「前の PC」のデスクトップ版で「はじめに」を済ませ、記事・画像・添付資料を入れて作ったバックアップを返す。
func movingBackup(t *testing.T) []byte {
	t.Helper()
	old, _ := newDesktopEnv(t)
	c := old.completeSetup()
	old.writeUpload("2026/10/moved.webp", "moved image")
	old.writeAttachment("cd/moved.pdf", "%PDF moved")
	if _, err := old.db.Exec(`INSERT INTO articles (id, title, slug, content_markdown, excerpt, status, author_id, category_id)
		SELECT 'moved-1', '引っ越した記事', 'moved-article', '本文', '', 'draft', id, 1 FROM users`); err != nil {
		t.Fatal(err)
	}
	return readFile(t, c.downloadBackup())
}

func TestSetupRestoreStartsFromBackup(t *testing.T) {
	data := movingBackup(t)
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	expectContains(t, c.get(SetupPath).Body, "バックアップから始める", `action="/admin/setup/restore"`)

	res := c.postRestoreTo(setupRestorePath, data)
	expectStatus(t, res, http.StatusOK)
	var body backupRestoreResponse
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("response: %v\n%s", err, res.Body)
	}
	if body.Redirect != "/admin/login?flash=setup_restored" {
		t.Errorf("redirect = %q", body.Redirect)
	}
	// 復元した DB にあるアカウントで、ログイン画面からそのまま入れる。
	expectContains(t, c.get(body.Redirect).Body, "バックアップを作ったときのユーザー名とパスワードでログインしてください")
	expectRedirect(t, c.login("hanako", "password123"), "/admin/articles")
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE id = 'moved-1'`); n != 1 {
		t.Errorf("moved article rows = %d", n)
	}
	if got, err := os.ReadFile(filepath.Join(env.attachmentDir, "cd", "moved.pdf")); err != nil || string(got) != "%PDF moved" {
		t.Errorf("moved attachment = %q, %v", got, err)
	}
	expectStatus(t, c.get("/uploads/2026/10/moved.webp"), http.StatusOK)

	// アカウントができたあとは、ここからは復元できない（他人のデータで置き換えさせない）。
	other := env.newClient()
	res = other.postRestoreTo(setupRestorePath, data)
	expectStatus(t, res, http.StatusForbidden)
	expectContains(t, res.Body, "ログインしてから")
}

func TestSetupRestoreRefusedAfterSetup(t *testing.T) {
	data := movingBackup(t)
	env, _ := newDesktopEnv(t)
	env.completeSetup()
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES ('restore_marker', 'keep')`); err != nil {
		t.Fatal(err)
	}
	res := env.newClient().postRestoreTo(setupRestorePath, data)
	expectStatus(t, res, http.StatusForbidden)
	if env.setting("restore_marker") != "keep" {
		t.Error("data changed by a refused restore")
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE id = 'moved-1'`); n != 0 {
		t.Error("backup was restored after setup")
	}
}

// 古い版の取り込みで著者のユーザーだけがある（管理者がいない）デスクトップでは、「はじめに」は出すが、
// 「バックアップから始める」は出さず、送られても断る（取り込んだデータを確かめないまま置き換えない）。
func TestSetupRestoreRefusedWhenNonAdminUsersExist(t *testing.T) {
	data := movingBackup(t)
	env, _ := newDesktopEnv(t)
	if _, err := env.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w1', 'taro', '太郎', '', 'writer')`); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()
	c.setupCSRF()
	body := c.get(SetupPath).Body
	expectContains(t, body, `id="setup-form"`)
	for _, hidden := range []string{"バックアップから始める", `action="/admin/setup/restore"`, `href="#setup-restore"`} {
		if strings.Contains(body, hidden) {
			t.Errorf("setup page offers the restore (%q) although users exist", hidden)
		}
	}
	// 入力の誤りで出し直した画面にも出さない。
	form := setupForm()
	form.Set("password_confirm", "different")
	res := c.postForm(SetupPath, form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if strings.Contains(res.Body, `action="/admin/setup/restore"`) {
		t.Error("setup page with errors offers the restore although users exist")
	}

	res = c.postRestoreTo(setupRestorePath, data)
	expectStatus(t, res, http.StatusForbidden)
	expectContains(t, res.Body, "先に「はじめに」を済ませて")
	if n := env.countRows(`SELECT COUNT(*) FROM users WHERE id = 'w1'`); n != 1 {
		t.Error("data changed by a refused restore")
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE id = 'moved-1'`); n != 0 {
		t.Error("backup was restored over existing users")
	}
}

func TestSetupRestoreRequiresCSRFHeaderAndValidArchive(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	// ヘッダーの CSRF トークンが無ければ本文を読まずに断る。
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+setupRestorePath, strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	expectStatus(t, c.do(req), http.StatusForbidden)
	// sorus のバックアップでないファイルは、既存の検査で断る（はじめに画面のまま）。
	res := c.postRestoreTo(setupRestorePath, []byte("not a zip"))
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "読み取れませんでした")
	if n := env.countRows(`SELECT COUNT(*) FROM users`); n != 0 {
		t.Errorf("users = %d after a rejected restore", n)
	}
	expectRedirect(t, c.get("/admin"), SetupPath)
}

func TestSetupRestoreNotFoundInServerMode(t *testing.T) {
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		if _, err := db.Exec(`DELETE FROM users`); err != nil {
			t.Fatal(err)
		}
	})
	// サーバーモードには「はじめに」画面が無い。ログインしていなければ断り、管理者でも存在しないページになる。
	expectStatus(t, env.newClient().postRestoreTo(setupRestorePath, []byte("x")), http.StatusUnauthorized)
}

func TestSetupRestoreNotFoundForServerAdmin(t *testing.T) {
	env := newTestEnv(t)
	expectStatus(t, env.loggedIn(admin).postRestoreTo(setupRestorePath, []byte("x")), http.StatusNotFound)
}
