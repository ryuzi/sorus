package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sorus/internal/auth"
	"sorus/internal/backup"
	"sorus/internal/database"
)

// newDesktopSiteEnv はサイトの鍵 key のデスクトップモードのサーバーを、ユーザーが1人もいない DB で起動する。
func newDesktopSiteEnv(t *testing.T, key string) *testEnv {
	t.Helper()
	return newDesktopSiteEnvWith(t, Config{SiteKey: key})
}

// newDesktopSiteEnvWith は cfg（DB・UploadDir・AttachmentDir・Desktop 以外）のデスクトップモードのサーバーを、
// ユーザーが1人もいない DB で起動する。
func newDesktopSiteEnvWith(t *testing.T, cfg Config) *testEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Connect(filepath.Join(dir, "database.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	cfg.DB, cfg.Desktop = db, true
	cfg.UploadDir, cfg.AttachmentDir = filepath.Join(dir, "uploads"), filepath.Join(dir, "attachments")
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return &testEnv{t: t, db: db, srv: srv, server: s, uploadDir: cfg.UploadDir, attachmentDir: cfg.AttachmentDir}
}

// 同じブラウザ（1つの Cookie の入れ物）で2つのサイトを使っても、片方のログイン・ログアウト・言語の切り替えが、
// もう片方のログインと CSRF・言語の Cookie を上書きしたり消したりしない。ブラウザは Cookie をポートで分けないため。
func TestTwoSitesKeepSeparateCookies(t *testing.T) {
	a := newDesktopSiteEnv(t, "")
	b := newDesktopSiteEnv(t, "abc")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := func(env *testEnv) *client {
		return &client{t: t, env: env, http: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
	}
	ca, cb := browser(a), browser(b)
	namesA, namesB := auth.CookieNamesFor(""), auth.CookieNamesFor("abc")
	setup := func(c *client, csrfName string) {
		t.Helper()
		expectStatus(t, c.get(SetupPath), http.StatusOK)
		form := setupForm()
		form.Set(CSRFFormField, c.cookie(csrfName))
		expectRedirect(t, c.postRaw(SetupPath, form), "/admin")
	}

	setup(ca, namesA.CSRF)
	sessionA, csrfA := ca.cookie(namesA.Session), ca.cookie(namesA.CSRF)
	if sessionA == "" || csrfA == "" {
		t.Fatalf("site A cookies after setup: session %q, csrf %q", sessionA, csrfA)
	}
	expectRedirect(t, ca.get("/admin/set-lang?lang=en"), "/admin")

	setup(cb, namesB.CSRF)
	if got := ca.cookie(namesA.Session); got != sessionA {
		t.Errorf("site B's setup changed site A's session cookie: %q -> %q", sessionA, got)
	}
	if got := ca.cookie(namesA.CSRF); got != csrfA {
		t.Errorf("site B's setup changed site A's CSRF cookie: %q -> %q", csrfA, got)
	}
	expectStatus(t, ca.get("/admin"), http.StatusOK)
	expectStatus(t, cb.get("/admin"), http.StatusOK)

	// B を日本語のままにしても、A の英語の設定は変わらない。
	expectRedirect(t, cb.get("/admin/set-lang?lang=ja"), "/admin")
	if got := ca.cookie(namesA.Lang); got != "en" {
		t.Errorf("site A's language cookie = %q after switching site B, want en", got)
	}

	// B からログアウトしても、A はログインしたまま。
	expectRedirect(t, cb.postRaw("/admin/logout", url.Values{CSRFFormField: {cb.cookie(namesB.CSRF)}}), "/admin/login")
	expectStatus(t, ca.get("/admin"), http.StatusOK)
	if got := ca.cookie(namesA.Session); got != sessionA {
		t.Errorf("site B's logout changed site A's session cookie: %q -> %q", sessionA, got)
	}
}

func TestNewRejectsInvalidSiteKey(t *testing.T) {
	db, err := database.Connect(filepath.Join(t.TempDir(), "database.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ABC", "a b", "a;b", "サイト"} {
		if _, err := New(Config{DB: db, UploadDir: t.TempDir(), AttachmentDir: t.TempDir(), SiteKey: key}); err == nil {
			t.Errorf("New accepted site key %q", key)
		}
	}
}

func TestSiteNameOf(t *testing.T) {
	env := newDesktopSiteEnv(t, "abc")
	if name, err := SiteNameOf(env.db); err != nil || name != "" {
		t.Errorf("before setup: %q, %v; want empty", name, err)
	}
	c := env.newClient()
	c.get(SetupPath)
	form := setupForm()
	form.Set(CSRFFormField, c.cookie(auth.CookieNamesFor("abc").CSRF))
	expectRedirect(t, c.postRaw(SetupPath, form), "/admin")
	if name, err := SiteNameOf(env.db); err != nil || name != "学生新聞" {
		t.Errorf("after setup: %q, %v; want 学生新聞", name, err)
	}
}

// StopBackground は予約投稿のワーカー・待っている自動の反映・自動バックアップ・新しい版の確認を止め、画面は応答し続ける。
// 2回呼んでも、そのあと Shutdown しても問題ない。
func TestStopBackground(t *testing.T) {
	upd := NewUpdateCheck()
	env := newDesktopSiteEnvWith(t, Config{UpdateCheck: upd})
	s := env.server
	upd.mu.Lock()
	joined := len(upd.servers)
	upd.mu.Unlock()
	if joined != 1 {
		t.Fatalf("servers in the shared update check = %d, want 1", joined)
	}
	var reflected atomic.Bool
	st := s.publishState()
	st.debounce = 50 * time.Millisecond
	st.scheduleReflect(func() { reflected.Store(true) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.StopBackground(ctx); err != nil {
		t.Fatalf("StopBackground: %v", err)
	}
	for name, done := range map[string]chan struct{}{"scheduler": s.schedulerDone, "automatic backups": s.careDone} {
		select {
		case <-done:
		default:
			t.Errorf("%s still running after StopBackground", name)
		}
	}
	time.Sleep(4 * st.debounce)
	if reflected.Load() {
		t.Error("a waiting scheduled reflect ran after StopBackground")
	}
	upd.mu.Lock()
	joined = len(upd.servers)
	upd.mu.Unlock()
	if joined != 0 {
		t.Error("the stopped server still receives update checks")
	}
	expectStatus(t, env.newClient().get("/healthz"), http.StatusOK)
	if err := s.StopBackground(ctx); err != nil {
		t.Errorf("second StopBackground: %v", err)
	}

	// 削除をやめたときは、止める前と同じように動かし直す。取り消した反映も、もう一度待ってから始める。
	s.ResumeBackground()
	for name, done := range map[string]chan struct{}{"scheduler": s.schedulerDone, "automatic backups": s.careDone} {
		select {
		case <-done:
			t.Errorf("%s is not running after ResumeBackground", name)
		default:
		}
	}
	st.mu.Lock()
	pending := st.timer != nil
	if pending {
		st.timer.Stop() // 公開先の無いサイトで反映を始めさせない。
	}
	st.mu.Unlock()
	if !pending {
		t.Error("the cancelled scheduled reflect was not scheduled again")
	}
	upd.mu.Lock()
	joined = len(upd.servers)
	upd.mu.Unlock()
	if joined != 1 {
		t.Error("the resumed server does not receive update checks")
	}

	// Shutdown のあとは動かし直さない。
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	s.ResumeBackground()
	select {
	case <-s.schedulerDone:
	default:
		t.Error("ResumeBackground restarted the scheduler after Shutdown")
	}
}

// Shutdown が処理中の要求を返し終えるのを待っているあいだに ResumeBackground を呼んでも（削除をやめたのと、アプリの
// 終了が重なったとき）、バックグラウンドの仕事と新しい版の確認を動かし直さない。
func TestResumeBackgroundDuringShutdown(t *testing.T) {
	upd := NewUpdateCheck()
	env := newDesktopSiteEnvWith(t, Config{UpdateCheck: upd})
	s := env.server
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.StopBackground(ctx); err != nil {
		t.Fatalf("StopBackground: %v", err)
	}

	// 返さずに待たせる要求を1つ処理中にして、Shutdown を待たせる。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	entered, release := make(chan struct{}), make(chan struct{})
	go s.Serve(ln, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hold" {
				close(entered)
				<-release
			}
			h.ServeHTTP(w, r)
		})
	})
	go func() {
		client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
		if res, err := client.Get("http://" + addr + "/hold"); err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the held request did not arrive")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(ctx) }()
	// 待ち受けが閉じたら、Shutdown は処理中の要求を待っている。
	waitForCond(t, "the listener to close", func() bool {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
		}
		return err != nil
	})
	s.ResumeBackground()
	close(release)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	upd.mu.Lock()
	joined := len(upd.servers)
	upd.mu.Unlock()
	if joined != 0 {
		t.Error("the server that was shut down still receives update checks")
	}
	for name, done := range map[string]chan struct{}{"scheduler": s.schedulerDone, "automatic backups": s.careDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Errorf("%s was restarted while the server was shutting down", name)
		}
	}
}

func TestBackupBeforeDelete(t *testing.T) {
	env := newDesktopSiteEnv(t, "abc")
	s := env.server
	writeFile := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(env.uploadDir, "2026", "a.webp"))
	writeFile(filepath.Join(env.attachmentDir, "b.pdf"))
	const name = "sorus_deleted_site_abc_20261029_120000.zip"

	fallback := filepath.Join(t.TempDir(), "削除したサイトのバックアップ")
	got, err := s.BackupBeforeDelete(fallback, name)
	if err != nil || got != filepath.Join(fallback, name) {
		t.Fatalf("BackupBeforeDelete = %q, %v; want %s", got, err, filepath.Join(fallback, name))
	}
	m, err := backup.ReadManifest(got)
	if err != nil || m.Uploads.FileCount != 1 || m.Attachments == nil || m.Attachments.FileCount != 1 {
		t.Fatalf("manifest = %+v, %v; want 1 upload and 1 attachment", m, err)
	}

	// 自動バックアップのフォルダが決まっていれば、その中に置く。
	auto := t.TempDir()
	if err := s.putSetting(autoBackupFolderSetting, auto); err != nil {
		t.Fatal(err)
	}
	if got, err := s.BackupBeforeDelete(fallback, name); err != nil || got != filepath.Join(auto, name) {
		t.Fatalf("with an automatic backup folder = %q, %v; want %s", got, err, filepath.Join(auto, name))
	}
	// 同じ名前のファイルは上書きしない（作れなければエラー）。
	if _, err := s.BackupBeforeDelete(fallback, name); err == nil {
		t.Error("BackupBeforeDelete overwrote an existing file")
	}

	// 復元の途中なら作らない。
	s.backupMu.Lock()
	_, err = s.BackupBeforeDelete(fallback, "other.zip")
	s.backupMu.Unlock()
	if !errors.Is(err, backup.ErrBusy) {
		t.Errorf("during a restore: %v, want backup.ErrBusy", err)
	}
	if entries, _ := os.ReadDir(fallback); len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".") {
		t.Errorf("files in the fallback folder = %v, want only the first backup", entries)
	}
}
