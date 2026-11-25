package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/publish"
	"sorus/internal/scheduler"
)

func TestDescribeGitHubFileTooLarge(t *testing.T) {
	got := describePublishError(publish.ErrGitHubFileTooLarge)
	if !strings.Contains(got, "100MB") || !strings.Contains(got, "GitHub Pages") {
		t.Fatalf("describePublishError = %q", got)
	}
}

// overdueQuestion は、ダッシュボードで起動する前に予約時刻を過ぎた記事について尋ねる文の始まり。
const overdueQuestion = "予約時刻を過ぎた記事が"

// expectPendingOn は s の PendingScheduledReflects が n 件・最後の日時 last であることを確かめる。
func expectPendingOn(t *testing.T, s *Server, step string, n int, last time.Time) {
	t.Helper()
	gotN, gotLast, err := s.PendingScheduledReflects()
	if err != nil {
		t.Fatalf("%s: PendingScheduledReflects: %v", step, err)
	}
	if gotN != n || !gotLast.Equal(last) {
		t.Fatalf("%s: PendingScheduledReflects = %d, %v; want %d, %v", step, gotN, gotLast, n, last)
	}
}

// scheduledSetIn は予約だった記事の集合（scheduler.Scheduled）を返す。
func scheduledSetIn(t *testing.T, env *testEnv) map[string]time.Time {
	t.Helper()
	set, err := scheduler.Scheduled(context.Background(), env.db)
	if err != nil {
		t.Fatalf("scheduler.Scheduled: %v", err)
	}
	return set
}

func (st *publishState) overdueCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.overdue
}

// 「予約をすべて反映してから終了する」を選んだとき、起動する前に予約時刻を過ぎた記事について尋ねていて予約投稿による
// 反映が止まっていても、ReflectDueNow は手動の反映を始め、その記事を公開先へ送る（予約時刻を過ぎた記事の確認も
// 終わる）。公開先が無い・送る記事が無い（まだ時刻の来ない予約だけ）なら何もしない。
func TestReflectDueNowSendsOverdueScheduledArticle(t *testing.T) {
	p := newPendingSite(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seedScheduled(t, p.db, article.Article{AuthorID: "w", Title: "閉じている間の予約", Slug: "while-closed", ContentMarkdown: "本文"}, at)
	if p.server.ReflectDueNow() {
		t.Fatal("ReflectDueNow started a reflect without a destination")
	}
	out := p.useFolderTarget(testBaseURL)
	p.server.loadOverdue()
	st := p.server.publishState()
	if n := st.overdueCount(); n != 1 {
		t.Fatalf("overdue = %d, want 1", n)
	}
	if p.server.startReflect(publishTriggerSchedule) {
		t.Fatal("a scheduled reflect started while the overdue article was being asked about")
	}

	if !p.server.ReflectDueNow() {
		t.Fatal("ReflectDueNow did not start a reflect")
	}
	if res := waitReflectDone(t, p.server); !res.OK || res.Trigger != publishTriggerManual {
		t.Fatalf("reflect = %+v, want a successful manual reflect", res)
	}
	if _, err := os.Stat(filepath.Join(out, "articles", "while-closed", "index.html")); err != nil {
		t.Fatalf("the overdue article was not sent: %v", err)
	}
	p.expectPending("after the reflect", 0, time.Time{})
	if n := st.overdueCount(); n != 0 {
		t.Errorf("overdue after the reflect = %d, want 0", n)
	}

	// まだ時刻の来ない予約だけなら、反映しない（早く送らない）。
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	seedScheduled(t, p.db, article.Article{AuthorID: "w", Title: "先の予約", Slug: "later", ContentMarkdown: "本文"}, future)
	if p.server.ReflectDueNow() {
		t.Error("ReflectDueNow started a reflect with only future scheduled articles")
	}
	p.expectPending("future only", 1, future)
}

// 「予約をすべて反映してから終了する」を選んだときに別の反映が動いていれば、その反映が失敗しても、終わったあとに
// 手動の反映をもう1回行い、予約時刻を過ぎた記事を送る（予約投稿による反映にすると、尋ねている間は断られる）。
func TestReflectDueNowWhileReflectRunning(t *testing.T) {
	p := newPendingSite(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seedScheduled(t, p.db, article.Article{AuthorID: "w", Title: "閉じている間の予約", Slug: "while-closed", ContentMarkdown: "本文"}, at)
	first := p.useFolderTarget(testBaseURL)
	p.server.loadOverdue()
	st := p.server.publishState()

	// 書き出しはバックアップの読み取りロックの中で行うため、書き込みを持って、動いている反映を書き出しの手前で待たせる。
	p.server.backupMu.Lock()
	locked := true
	defer func() {
		if locked {
			p.server.backupMu.Unlock()
		}
	}()
	if !p.server.startReflect(publishTriggerManual) {
		t.Fatal("the first reflect did not start")
	}
	waitForCond(t, "the first reflect to wait before the export", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.phase == publishPhaseExport
	})
	// 動いている反映は送れずに失敗する（接続した公開先のフォルダが、ファイルに置き換わった）。次の反映の公開先は別のフォルダ。
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(p.dir, "export-2")
	b, _ := json.Marshal(publishFolderConfig{Path: second})
	if err := upsertSetting(p.db, publishSettingFolder, string(b)); err != nil {
		t.Fatal(err)
	}

	if !p.server.ReflectDueNow() {
		t.Fatal("ReflectDueNow while a reflect is running = false, want a follow-up reflect")
	}
	st.mu.Lock()
	rerun, manual := st.rerun, st.rerunManual
	st.mu.Unlock()
	if !rerun || !manual {
		t.Fatalf("rerun = %v, manual = %v; want a manual follow-up reflect", rerun, manual)
	}
	p.server.backupMu.Unlock()
	locked = false

	waitForCond(t, "the follow-up reflect", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.reflects == 2 && !st.running && st.last != nil
	})
	st.mu.Lock()
	last := *st.last
	st.mu.Unlock()
	if !last.OK || last.Trigger != publishTriggerManual {
		t.Fatalf("follow-up reflect = %+v, want a successful manual reflect", last)
	}
	if _, err := os.Stat(filepath.Join(second, "articles", "while-closed", "index.html")); err != nil {
		t.Fatalf("the overdue article was not sent by the follow-up reflect: %v", err)
	}
	p.expectPending("after the follow-up reflect", 0, time.Time{})
	if n := st.overdueCount(); n != 0 {
		t.Errorf("overdue after the follow-up reflect = %d, want 0", n)
	}
}

// 公開日時を迎えた予約の反映が失敗したあと、編集画面で記事を保存しても（更新日時が公開日時を超えても）、まだ反映して
// いない予約として、同じ件数・同じ最後の日時で数えたままにする。
func TestPendingScheduledReflectKeptWhenSavedAfterFailedReflect(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	ed, adm := env.loggedIn(chief), env.loggedIn(admin)
	saveFolderDestination(t, adm, filepath.Join(t.TempDir(), "site"))
	// 公開サイトURLが無いため、反映は失敗する。
	if err := env.server.saveBaseURL(""); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	id := seedScheduled(t, env.db, article.Article{AuthorID: chief.ID, Title: "反映に失敗した予約", Slug: "failed",
		ContentMarkdown: "本文"}, at)
	expectPendingOn(t, env.server, "due, not reflected", 1, at)
	if got := adm.reflectAndWait(); got.OK {
		t.Fatalf("reflect without a site URL succeeded: %+v", got)
	}
	expectPendingOn(t, env.server, "after a failed reflect", 1, at)

	form := url.Values{"title": {"反映に失敗した予約"}, "category_id": {"1"}, "content_markdown": {"直した本文"}, "action": {"save"}}
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	a := env.mustGetArticle(id)
	if a.Status != article.StatusPublished || a.PublishedAt == nil || !a.PublishedAt.Equal(at) || !a.UpdatedAt.After(at) {
		t.Fatalf("saved article = status %s published_at %v updated_at %v; want published at %v, updated later",
			a.Status, a.PublishedAt, a.UpdatedAt, at)
	}
	expectPendingOn(t, env.server, "saved after the publish time", 1, at)
}

// seedDesktopUserAndTarget は、管理者 w（writer0）と、フォルダの公開先 out を用意する。siteURL が空なら公開サイトURLを
// 入れない（反映は失敗する）。
func seedDesktopUserAndTarget(t *testing.T, env *testEnv, out, siteURL string) {
	t.Helper()
	if _, err := env.db.Exec(`INSERT INTO categories (id, name, slug) VALUES (1, '未分類', 'uncategorized')`); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w', 'writer0', 'w', ?, 'admin')`, string(hash)); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(publishFolderConfig{Path: out})
	for k, v := range map[string]string{publishSettingKind: publishKindFolder, publishSettingFolder: string(b), "base_url": siteURL} {
		if err := upsertSetting(env.db, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// アプリを起動している間に公開日時を迎え、予約投稿による反映が失敗して保存し直した記事は、起動し直してもまだ反映して
// いない予約として数え、次の起動で1回だけ「予約時刻を過ぎた記事」として尋ねる。その次の起動では尋ね直さない。
func TestDesktopFailedScheduledReflectIsAskedOnceAfterRestart(t *testing.T) {
	out := filepath.Join(t.TempDir(), "site-export")
	at := time.Now().UTC().Add(1500 * time.Millisecond)
	var id string
	first := newDesktopEnvWith(t, func(env *testEnv) {
		seedDesktopUserAndTarget(t, env, out, "")
		id = seedScheduled(t, env.db, article.Article{AuthorID: "w", Title: "起動中に迎えた予約", Slug: "while-running",
			ContentMarkdown: "本文"}, at)
	})
	st := first.server.publishState()
	st.mu.Lock()
	st.debounce = 20 * time.Millisecond
	st.mu.Unlock()
	waitForCond(t, "the scheduled reflect to fail", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.last != nil && !st.last.OK && st.last.Trigger == publishTriggerSchedule
	})
	if n := st.overdueCount(); n != 0 {
		t.Fatalf("overdue = %d, want 0 (the article came due while sorus was running)", n)
	}
	expectPendingOn(t, first.server, "after the failed reflect", 1, at)
	a := first.mustGetArticle(id)
	a.ContentMarkdown = "直した本文"
	if err := article.NewService(first.db).Save(a); err != nil {
		t.Fatal(err)
	}
	expectPendingOn(t, first.server, "saved after the failed reflect", 1, at)

	restart := func(prev *testEnv) *testEnv {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := prev.server.StopBackground(ctx); err != nil {
			t.Fatalf("StopBackground: %v", err)
		}
		s, err := New(Config{DB: first.db, UploadDir: first.uploadDir, AttachmentDir: first.attachmentDir, Desktop: true,
			SchedulerInterval: 20 * time.Millisecond, LogOutput: io.Discard})
		if err != nil {
			t.Fatal(err)
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
		return &testEnv{t: t, db: first.db, srv: srv, server: s, uploadDir: first.uploadDir, attachmentDir: first.attachmentDir}
	}
	user := testUser{ID: "w", Username: "writer0"}

	second := restart(first)
	expectPendingOn(t, second.server, "after a restart", 1, at)
	c := second.loggedIn(user)
	expectContains(t, c.get("/admin/publish/panel").Body, "予約時刻を過ぎた記事が 1 件あります。サイトに反映しますか？")
	if body := c.get("/admin/publish/panel").Body; strings.Contains(body, overdueQuestion) {
		t.Error("overdue question shown twice in one run")
	}

	third := restart(second)
	expectPendingOn(t, third.server, "after another restart", 1, at)
	if body := third.loggedIn(user).get("/admin/publish/panel").Body; strings.Contains(body, overdueQuestion) {
		t.Error("overdue question asked again after another restart")
	}
	if n := third.server.publishState().overdueCount(); n != 0 {
		t.Errorf("overdue after another restart = %d, want 0", n)
	}
}

// すぐに公開した記事（作成と同時の公開・日時を決めない承認・予約からの「今すぐ公開」）は、予約だった記事の集合に
// 入らず、時刻が過ぎても、まだ反映していない予約として数えない。
func TestImmediatePublishesAreNeverPendingScheduledReflects(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	ed := env.loggedIn(chief)
	saveFolderDestination(t, env.loggedIn(admin), filepath.Join(t.TempDir(), "site"))
	expectNone := func(step string) {
		t.Helper()
		expectPendingOn(t, env.server, step, 0, time.Time{})
		if set := scheduledSetIn(t, env); len(set) != 0 {
			t.Fatalf("%s: scheduled set = %v, want empty", step, set)
		}
	}

	expectRedirect(t, ed.postForm("/admin/articles/new", url.Values{"title": {"作成と同時に公開"}, "category_id": {"1"},
		"content_markdown": {"本文"}, "action": {"publish"}}), "/admin/articles/")
	approved := ed.createArticle("日時を決めずに承認", "本文")
	expectRedirect(t, ed.postForm("/admin/articles/"+approved+"/submit-review", nil), "/admin/articles?status=review")
	// 公開先を決めてあるため、記事公開時の自動反映（既定で有効）が始まったことを知らせる。
	expectRedirect(t, ed.postForm("/admin/articles/"+approved+"/approve-publish", nil),
		"/admin/articles/"+approved+"/edit?flash="+flashPublishedReflecting)
	expectNone("published immediately")

	sched := ed.createArticle("予約してから今すぐ公開", "本文")
	expectRedirect(t, ed.postForm("/admin/articles/"+sched+"/submit-review", nil), "/admin/articles?status=review")
	expectRedirect(t, ed.postForm("/admin/articles/"+sched+"/approve-publish", url.Values{"publish_at": {futureLocal(2 * time.Hour)}}),
		"/admin/articles/"+sched+"/edit?flash=scheduled")
	at := *env.mustGetArticle(sched).PublishedAt
	if got, ok := scheduledSetIn(t, env)[sched]; !ok || !got.Equal(at) {
		t.Fatalf("scheduled article in the set = %v (%v), want %v", got, ok, at)
	}
	expectPendingOn(t, env.server, "scheduled", 1, at)
	form := url.Values{"title": {"予約してから今すぐ公開"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"publish"}}
	expectRedirect(t, ed.postForm("/admin/articles/"+sched, form), "/admin/articles/"+sched+"/edit")
	expectNone("published now")

	setClock(t, time.Now().Add(3*time.Hour))
	expectNone("hours later")
}

// ゴミ箱に移した・下書きに戻した予約と、公開サーバーの同期 API で取り消した予約は、予約だった記事の集合から外れる。
func TestScheduledSetForgetsDeletedAndUnscheduledArticles(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	ed := env.loggedIn(chief)
	schedule := func(title string) string {
		t.Helper()
		id := ed.createArticle(title, "本文")
		expectRedirect(t, ed.postForm("/admin/articles/"+id+"/submit-review", nil), "/admin/articles?status=review")
		expectRedirect(t, ed.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"publish_at": {futureLocal(2 * time.Hour)}}),
			"/admin/articles/"+id+"/edit?flash=scheduled")
		if _, ok := scheduledSetIn(t, env)[id]; !ok {
			t.Fatalf("%s: not in the scheduled set after scheduling", title)
		}
		return id
	}
	trashed := schedule("ゴミ箱に移す予約")
	expectRedirect(t, ed.postForm("/admin/articles/"+trashed+"/delete", nil), "/admin/articles?status=deleted")
	drafted := schedule("下書きに戻す予約")
	form := url.Values{"title": {"下書きに戻す予約"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"draft"}}
	expectRedirect(t, ed.postForm("/admin/articles/"+drafted, form), "/admin/articles/"+drafted+"/edit")
	set := scheduledSetIn(t, env)
	for name, id := range map[string]string{"trashed": trashed, "back to draft": drafted} {
		if _, ok := set[id]; ok {
			t.Errorf("%s article is still in the scheduled set", name)
		}
	}

	sync := newSyncEnv(t)
	editor := sync.token("desk", auth.RoleEditor)
	id := sync.seedReview("同期で予約して取り消す", "sync-unschedule")
	expectSyncStatus(t, sync.call(editor, scheduleRequest(id, time.Now().UTC().Add(2*time.Hour))), http.StatusOK)
	if _, ok := scheduledSetIn(t, sync.testEnv)[id]; !ok {
		t.Fatal("schedule_article did not add the article to the scheduled set")
	}
	expectSyncStatus(t, sync.call(editor, unscheduleRequest(id)), http.StatusOK)
	if _, ok := scheduledSetIn(t, sync.testEnv)[id]; ok {
		t.Error("unschedule_article left the article in the scheduled set")
	}
}

// 公開専用モードの公開サーバーでも、同期 API の schedule_article で予約した記事を予約だった記事の集合に入れ
// （scheduler が公開日時を迎えたことを知らせる）、読者に見え始めて知らせ終えたら外す（反映しないため残さない）。
func TestPublicServerForgetsScheduledArticleOnceVisible(t *testing.T) {
	env := newSyncEnvWith(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t), BaseURL: testBaseURL + "/",
		SchedulerInterval: 20 * time.Millisecond}, nil)
	// 予約の受け付けは差し替えた時計（clockNow）で、scheduler は実際の時刻で確かめる。
	publishAt := time.Now().UTC().Add(1500 * time.Millisecond)
	setClock(t, publishAt.Add(-time.Hour))
	id := env.seedReview("公開サーバーで予約", "public-scheduled")
	expectSyncStatus(t, env.call(env.token("desk", auth.RoleEditor), scheduleRequest(id, publishAt)), http.StatusOK)
	if got, ok := scheduledSetIn(t, env.testEnv)[id]; !ok || !got.Equal(publishAt) {
		t.Fatalf("scheduled set[%s] = %v (%v), want %v", id, got, ok, publishAt)
	}
	waitForCond(t, "the visible article to leave the scheduled set", func() bool {
		_, ok := scheduledSetIn(t, env.testEnv)[id]
		return !ok
	})
}
