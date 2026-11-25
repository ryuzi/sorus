package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sorus/internal/desktop"
)

// fakeReleases は GitHub の releases/latest と同じ形で tag を返すサーバーと、受けた要求の数を返す。
func fakeReleases(t *testing.T, tag *atomic.Value) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		v, _ := tag.Load().(string)
		if v == "" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"` + v + `","html_url":"https://github.com/ryuzi/sorus/releases/tag/` + v + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// withRunningVersion は動いている版を v として扱う。
func withRunningVersion(t *testing.T, v string) {
	t.Helper()
	prev := runningVersion
	runningVersion = func() string { return v }
	t.Cleanup(func() { runningVersion = prev })
}

func TestUpdateNoticeShowsNewerReleaseOnce(t *testing.T) {
	withRunningVersion(t, "v1.0.0")
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	s := env.server
	var tag atomic.Value
	tag.Store("v1.1.0")
	rel, hits := fakeReleases(t, &tag)
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()

	expectContains(t, c.get("/admin").Body, `data-desktop-part="/admin/update/banner"`)
	if body := c.get(updateBannerPath).Body; strings.TrimSpace(body) != "" {
		t.Fatalf("banner before any check = %q", body)
	}

	if err := s.checkUpdateIfDue(ctx, rel.Client(), rel.URL, "v1.0.0", now); err != nil {
		t.Fatalf("checkUpdateIfDue: %v", err)
	}
	banner := c.get(updateBannerPath).Body
	expectContains(t, banner, "新しいバージョン（v1.1.0）があります", desktop.InstallCommand("linux"),
		`data-copy-target="update-command"`, "コピー", `data-dismiss-update="/admin/update/dismiss"`, "閉じる")

	// 1日に1回まで: 同じ日のうちは問い合わせない。
	tag.Store("v1.2.0")
	if err := s.checkUpdateIfDue(ctx, rel.Client(), rel.URL, "v1.0.0", now.Add(23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests within a day = %d, want 1", hits.Load())
	}

	// 「閉じる」を押すと、その版のお知らせは出さない。
	res := c.postAPI(updateDismissPath, "", nil)
	expectStatus(t, res, http.StatusNoContent)
	if body := c.get(updateBannerPath).Body; strings.TrimSpace(body) != "" {
		t.Fatalf("banner after dismiss = %q", body)
	}

	// 翌日にさらに新しい版が出たら、また出す。
	if err := s.checkUpdateIfDue(ctx, rel.Client(), rel.URL, "v1.0.0", now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("requests after a day = %d, want 2", hits.Load())
	}
	expectContains(t, c.get(updateBannerPath).Body, "新しいバージョン（v1.2.0）があります")
}

func TestUpdateNoticeHiddenWhenUpToDateOrCheckFails(t *testing.T) {
	withRunningVersion(t, "v1.1.0")
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	s := env.server
	var tag atomic.Value
	tag.Store("v1.1.0")
	rel, hits := fakeReleases(t, &tag)
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

	if err := s.checkUpdateIfDue(context.Background(), rel.Client(), rel.URL, "v1.1.0", now); err != nil {
		t.Fatal(err)
	}
	if body := c.get(updateBannerPath).Body; strings.TrimSpace(body) != "" {
		t.Fatalf("banner for the same version = %q", body)
	}

	// 失敗はエラーを返すだけで（呼び出し側がログに残す）、画面には何も出さず、翌日まで問い合わせない。
	tag.Store("")
	if err := s.checkUpdateIfDue(context.Background(), rel.Client(), rel.URL, "v1.1.0", now.Add(25*time.Hour)); err == nil {
		t.Fatal("failed check returned nil")
	}
	if err := s.checkUpdateIfDue(context.Background(), rel.Client(), rel.URL, "v1.1.0", now.Add(26*time.Hour)); err != nil {
		t.Fatalf("second check on the same day = %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("requests = %d, want 2", hits.Load())
	}
	if body := c.get(updateBannerPath).Body; strings.TrimSpace(body) != "" {
		t.Fatalf("banner after a failed check = %q", body)
	}
	expectStatus(t, c.get("/admin"), http.StatusOK)
}

func TestUpdateCheckDisabledForDevAndByEnv(t *testing.T) {
	var tag atomic.Value
	tag.Store("v9.0.0")
	rel, hits := fakeReleases(t, &tag)
	t.Setenv(desktop.UpdateURLEnv, rel.URL)
	env, _ := newDesktopEnv(t)

	for name, setup := range map[string]func() string{
		"dev": func() string { return "dev" },
		"env": func() string { t.Setenv(desktop.NoUpdateCheckEnv, "1"); return "v1.0.0" },
	} {
		current := setup()
		done := make(chan struct{})
		go func() {
			env.server.runUpdateChecks(context.Background(), current)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: runUpdateChecks kept running although checks are disabled", name)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("requests = %d, want 0", hits.Load())
	}
	// 開発中の版では、保存されている版があってもお知らせを出さない。
	withRunningVersion(t, "dev")
	if err := env.server.putSetting(updateLatestSetting, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.server.updateNotice("dev"); ok {
		t.Error("update notice shown for dev")
	}
}

func TestUpdateRoutesAreDesktopOnly(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	expectStatus(t, c.get(updateBannerPath), http.StatusNotFound)
	expectStatus(t, c.postAPI(updateDismissPath, "", nil), http.StatusNotFound)
}

// アプリ全体で1つの UpdateCheck は、サイトがいくつあっても1日に1回だけ問い合わせ、どのサイトにもお知らせを出す。
// あとから加わったサイトにも、問い合わせずにそれまでの結果を出す。
func TestSharedUpdateCheckOncePerApp(t *testing.T) {
	withRunningVersion(t, "v1.0.0")
	var tag atomic.Value
	tag.Store("v1.1.0")
	rel, hits := fakeReleases(t, &tag)
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()
	upd := NewUpdateCheck()

	if err := upd.checkIfDue(ctx, rel.Client(), rel.URL, "v1.0.0", now); err != nil || hits.Load() != 0 {
		t.Fatalf("check without sites: %v, requests %d; want no request", err, hits.Load())
	}
	a := newDesktopSiteEnvWith(t, Config{UpdateCheck: upd})
	b := newDesktopSiteEnvWith(t, Config{SiteKey: "abc", UpdateCheck: upd})
	if err := upd.checkIfDue(ctx, rel.Client(), rel.URL, "v1.0.0", now); err != nil {
		t.Fatalf("checkIfDue: %v", err)
	}
	if err := upd.checkIfDue(ctx, rel.Client(), rel.URL, "v1.0.0", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests for two sites = %d, want 1", hits.Load())
	}
	for name, s := range map[string]*Server{"a": a.server, "b": b.server} {
		if v, ok := s.updateNotice("v1.0.0"); !ok || v != "v1.1.0" {
			t.Errorf("site %s notice = %q, %v; want v1.1.0", name, v, ok)
		}
	}
	c := newDesktopSiteEnvWith(t, Config{SiteKey: "xyz", UpdateCheck: upd})
	if v, ok := c.server.updateNotice("v1.0.0"); !ok || v != "v1.1.0" {
		t.Errorf("site added later: notice = %q, %v; want v1.1.0", v, ok)
	}
}

// 起動し直したアプリは、サイトの DB に保存された前回の確認（前の版の sorus が1つ目のサイトに保存したものを含む）を
// 使い、1日たつまで問い合わせない。
func TestSharedUpdateCheckUsesSavedCheck(t *testing.T) {
	withRunningVersion(t, "v1.0.0")
	var tag atomic.Value
	tag.Store("v1.2.0")
	rel, hits := fakeReleases(t, &tag)
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	env := newDesktopSiteEnv(t, "")
	if err := env.server.putSetting(updateCheckedSetting, now.Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := env.server.putSetting(updateLatestSetting, "v1.1.0"); err != nil {
		t.Fatal(err)
	}

	upd := NewUpdateCheck()
	upd.add(env.server)
	other := newDesktopSiteEnvWith(t, Config{SiteKey: "abc", UpdateCheck: upd})
	if err := upd.checkIfDue(context.Background(), rel.Client(), rel.URL, "v1.0.0", now); err != nil || hits.Load() != 0 {
		t.Fatalf("check within a day of the saved one: %v, requests %d; want none", err, hits.Load())
	}
	if v, ok := other.server.updateNotice("v1.0.0"); !ok || v != "v1.1.0" {
		t.Errorf("saved result on another site = %q, %v; want v1.1.0", v, ok)
	}
}

// あとから加わったサイトの DB に、それまでより新しい確認の結果があれば、それまでに加わったサイトにも保存する
// （1日たつまで問い合わせないため、保存しないとそのサイトにだけお知らせが出ない）。
func TestSharedUpdateCheckSavesNewerCheckToEarlierSites(t *testing.T) {
	withRunningVersion(t, "v1.0.0")
	checked := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	upd := NewUpdateCheck()
	earlier := newDesktopSiteEnvWith(t, Config{UpdateCheck: upd})
	later := newDesktopSiteEnv(t, "abc")
	if err := later.server.putSetting(updateCheckedSetting, checked); err != nil {
		t.Fatal(err)
	}
	if err := later.server.putSetting(updateLatestSetting, "v1.1.0"); err != nil {
		t.Fatal(err)
	}

	upd.add(later.server)
	if got := earlier.server.getSetting(updateCheckedSetting, ""); got != checked {
		t.Errorf("checked time on the earlier site = %q, want %q", got, checked)
	}
	if v, ok := earlier.server.updateNotice("v1.0.0"); !ok || v != "v1.1.0" {
		t.Errorf("earlier site notice = %q, %v; want v1.1.0", v, ok)
	}
}

// 止めたときの古い ctx の後始末が、動かし直したあとに遅れて動いても、サーバーは新しい版の確認の保存先から外れない。
// StopBackground が ctx を終わらせてから、ResumeBackground で加わり直したあとに、前の後始末が動いた順にする。
func TestUpdateCheckKeepsResumedServer(t *testing.T) {
	upd := NewUpdateCheck()
	env := newDesktopSiteEnvWith(t, Config{UpdateCheck: upd})
	s := env.server
	joined := func() bool {
		upd.mu.Lock()
		defer upd.mu.Unlock()
		_, ok := upd.servers[s]
		return ok
	}

	stopped, stop := context.WithCancel(context.Background())
	s.startUpdateChecks(stopped)
	s.leaveUpdateCheck()
	s.startUpdateChecks(context.Background())
	stop()
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !joined() {
			t.Fatal("the resumed server was removed from the shared update check by the stopped context")
		}
	}
}
