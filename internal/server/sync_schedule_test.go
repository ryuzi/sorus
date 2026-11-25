package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
)

// 公開サーバー連携での予約公開の試験。公開サーバー側は同期 API の
// schedule_article・unschedule_article・server_info と fetch_pending の予約中の記事、ローカル CMS 側は
// 予約の選択肢・予約中の記事の表示・取り消しと、予約を知らない古い公開サーバーでの振る舞いを確かめる。

func scheduleRequest(id string, at time.Time) SyncRequest {
	return SyncRequest{Action: SyncActionScheduleArticle, ArticleID: id, PublishAt: &at}
}

func unscheduleRequest(id string) SyncRequest {
	return SyncRequest{Action: SyncActionUnscheduleArticle, ArticleID: id}
}

// syncSearchCount は公開サーバーのサイト内検索で q の件数を返す。
func syncSearchCount(t *testing.T, env *syncEnv, q string) string {
	t.Helper()
	rec := env.get("/search?q=" + url.QueryEscape(q))
	expectSyncStatus(t, rec, http.StatusOK)
	m := regexp.MustCompile(`の検索結果: (\d+)件`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("result count not found: %.1000s", rec.Body.String())
	}
	return m[1]
}

// 予約した記事は、公開専用モードの公開サーバーで、公開日時まで読者向けのどこ（記事・トップ・一覧・RSS・サイトマップ・
// 検索）にも出ず、公開日時を過ぎると記事を保存し直さなくても出る。公開サーバーで時刻のために動く処理は無い。
func TestSyncAPIScheduleArticleIsShownFromPublishTime(t *testing.T) {
	env := newSyncEnvWith(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t), BaseURL: testBaseURL + "/"}, nil)
	// 予約の受け付けと読者向けのページは差し替えた時計（clockNow）で、検索は実際の時刻で絞る。検索でも公開日時の
	// 前後を確かめられるよう、実際の時刻の数秒後を公開日時にし、時計はその1時間前にしておく。
	publishAt := time.Now().UTC().Add(4 * time.Second).Truncate(time.Second)
	setClock(t, publishAt.Add(-time.Hour))

	submitted := decodeSync[SyncSubmitResponse](t, env.call(env.token("tanaka", auth.RoleWriter), submitRequest(SyncArticle{
		Title: "文化祭の模擬店レポート", Slug: "festival-report", ContentMarkdown: "模擬店の焼きそばが昼までに完売した。",
	})))
	id := submitted.Article.ID

	rec := env.call(env.token("desk", auth.RoleEditor), scheduleRequest(id, publishAt))
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncPublishResponse](t, rec)
	if res.Action != SyncActionScheduleArticle || res.Article.ID != id || res.Article.Status != article.StatusPublished ||
		res.Article.PublishedAt == nil || !res.Article.PublishedAt.Equal(publishAt) {
		t.Errorf("response = %+v", res)
	}
	stored := env.mustGetArticle(id)
	if stored.Status != article.StatusPublished || stored.PublishedAt == nil || !stored.PublishedAt.Equal(publishAt) || stored.LockedBy != nil {
		t.Fatalf("scheduled article = status %s published_at %v locked_by %v", stored.Status, stored.PublishedAt, stored.LockedBy)
	}
	env.expectNoStoredScheduled("schedule_article")

	shown := func() map[string]bool {
		t.Helper()
		got := map[string]bool{}
		got["article page"] = env.get("/articles/festival-report").Code == http.StatusOK
		got["top"] = strings.Contains(env.get("/").Body.String(), "文化祭の模擬店レポート")
		got["list"] = strings.Contains(env.get(articlesListPath).Body.String(), "文化祭の模擬店レポート")
		got["rss"] = strings.Contains(env.get("/feed.xml").Body.String(), "文化祭の模擬店レポート")
		got["sitemap"] = strings.Contains(env.get("/sitemap.xml").Body.String(), testBaseURL+"/articles/festival-report")
		return got
	}

	if n := syncSearchCount(t, env, "焼きそば"); n != "0" {
		t.Errorf("before publish time: search hits = %s", n)
	}
	if !time.Now().Before(publishAt) {
		t.Fatal("the search before the publish time ran after it; the check is not valid")
	}
	for _, at := range []time.Time{publishAt.Add(-time.Hour), publishAt.Add(-time.Second)} {
		setClock(t, at)
		for name, ok := range shown() {
			if ok {
				t.Errorf("at %v (before publish time): shown in %s", at, name)
			}
		}
	}

	setClock(t, publishAt.Add(time.Second))
	got := shown()
	for name, ok := range got {
		if !ok {
			t.Errorf("after publish time: not shown in %s", name)
		}
	}
	if len(got) != 5 {
		t.Errorf("checked %d readers: %v", len(got), got)
	}
	// 検索は実際の時刻で絞るため、公開日時を過ぎるのを待つ。索引は予約のときに作ったまま、作り直さない。
	// 負荷の高い環境では索引への反映（server.reindex の再試行）が遅れることがあるため、期限つきで待つ。
	time.Sleep(time.Until(publishAt) + 100*time.Millisecond)
	n := syncSearchCount(t, env, "焼きそば")
	for deadline := time.Now().Add(10 * time.Second); n != "1" && time.Now().Before(deadline); n = syncSearchCount(t, env, "焼きそば") {
		time.Sleep(200 * time.Millisecond)
	}
	if n != "1" {
		t.Errorf("after publish time: search hits = %s, want 1", n)
	}
	// 公開日時を迎えても記事は保存し直さない（状態・更新日時はそのまま）。
	if a := env.mustGetArticle(id); a.Status != article.StatusPublished || !a.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Errorf("article changed at publish time: status=%s updated_at=%v, want %v", a.Status, a.UpdatedAt, stored.UpdatedAt)
	}
}

// 予約できるのは公開と同じ立場（editor・admin）で、校閲待ちの記事だけ。読者に見えている記事や一度公開した記事は
// 見えなくなる事故を防ぐため 422、今から1分以上先でない日時も 422 で断り、断ったときは記事を変えない。
func TestSyncAPIScheduleArticlePreconditions(t *testing.T) {
	env := newSyncEnv(t)
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	setClock(t, now)
	editor := env.token("desk", auth.RoleEditor)
	future := now.Add(48 * time.Hour)

	expectRefused := func(t *testing.T, rec *httptest.ResponseRecorder, status int, msg, id string) {
		t.Helper()
		before := env.mustGetArticle(id)
		expectSyncStatus(t, rec, status)
		if msg != "" && !strings.Contains(syncErrorMessage(t, rec), msg) {
			t.Errorf("message = %q, want it to contain %q", syncErrorMessage(t, rec), msg)
		}
		after := env.mustGetArticle(id)
		if after.Status != before.Status || !sameTimePtr(after.PublishedAt, before.PublishedAt) || after.LockedBy != nil {
			t.Errorf("refused request changed the article: %s %v -> %s %v (locked_by %v)",
				before.Status, before.PublishedAt, after.Status, after.PublishedAt, after.LockedBy)
		}
	}

	t.Run("writer is forbidden", func(t *testing.T) {
		id := env.seedReview("ライターの予約", "writer-schedule")
		expectRefused(t, env.call(env.token("tanaka", auth.RoleWriter), scheduleRequest(id, future)), http.StatusForbidden, "", id)
		expectRefused(t, env.call(env.token("tanaka", auth.RoleWriter), unscheduleRequest(id)), http.StatusForbidden, "", id)
	})
	t.Run("visible article is refused", func(t *testing.T) {
		id := seedArticle(t, env.db, article.Article{Title: "公開中の記事", Slug: "visible", Status: article.StatusPublished,
			PublishedAt: ptrTime(now.Add(-time.Hour))})
		expectRefused(t, env.call(editor, scheduleRequest(id, future)), http.StatusUnprocessableEntity, "公開中", id)
	})
	t.Run("once published article in review is refused", func(t *testing.T) {
		id := seedArticle(t, env.db, article.Article{Title: "一度公開した記事", Slug: "once-published", Status: article.StatusReview,
			PublishedAt: ptrTime(now.Add(-24 * time.Hour))})
		expectRefused(t, env.call(editor, scheduleRequest(id, future)), http.StatusUnprocessableEntity, "一度公開した記事", id)
	})
	t.Run("time not in the future is refused", func(t *testing.T) {
		id := env.seedReview("日時を確かめる記事", "check-time")
		for _, at := range []time.Time{now.Add(-time.Hour), now, now.Add(59 * time.Second)} {
			expectRefused(t, env.call(editor, scheduleRequest(id, at)), http.StatusUnprocessableEntity, "1分以上先", id)
		}
		expectRefused(t, env.call(editor, SyncRequest{Action: SyncActionScheduleArticle, ArticleID: id}), http.StatusBadRequest, "publish_at", id)
		// ちょうど1分後は受け付ける。
		expectSyncStatus(t, env.call(editor, scheduleRequest(id, now.Add(MinScheduleLead))), http.StatusOK)
	})
	t.Run("not in review is a conflict", func(t *testing.T) {
		draft := seedArticle(t, env.db, article.Article{Title: "下書きの記事", Slug: "draft", Status: article.StatusDraft})
		expectRefused(t, env.call(editor, scheduleRequest(draft, future)), http.StatusConflict, "下書き", draft)
		scheduled := seedArticle(t, env.db, article.Article{Title: "予約済みの記事", Slug: "already-scheduled", Status: article.StatusPublished,
			PublishedAt: ptrTime(now.Add(time.Hour))})
		expectRefused(t, env.call(editor, scheduleRequest(scheduled, future)), http.StatusConflict, "予約投稿", scheduled)
	})
	t.Run("locked by another user", func(t *testing.T) {
		id := env.seedReview("編集中の記事", "locked-schedule")
		if _, err := env.server.locker.Acquire(context.Background(), id, studentB.ID); err != nil {
			t.Fatal(err)
		}
		rec := env.call(editor, scheduleRequest(id, future))
		expectSyncStatus(t, rec, http.StatusConflict)
		if a := env.mustGetArticle(id); a.Status != article.StatusReview || a.PublishedAt != nil {
			t.Errorf("locked article = %s %v", a.Status, a.PublishedAt)
		}
	})
	t.Run("bad requests", func(t *testing.T) {
		expectSyncStatus(t, env.call(editor, scheduleRequest("", future)), http.StatusBadRequest)
		expectSyncStatus(t, env.call(editor, scheduleRequest("bad id", future)), http.StatusBadRequest)
		expectSyncStatus(t, env.call(editor, scheduleRequest("missing-article", future)), http.StatusNotFound)
		expectSyncStatus(t, env.call(editor, unscheduleRequest("")), http.StatusBadRequest)
		expectSyncStatus(t, env.call(editor, unscheduleRequest("missing-article")), http.StatusNotFound)
	})
	t.Run("admin can schedule", func(t *testing.T) {
		id := env.seedReview("管理者の予約", "admin-schedule")
		expectSyncStatus(t, env.call(env.token("chief-admin", auth.RoleAdmin), scheduleRequest(id, future)), http.StatusOK)
		if a := env.mustGetArticle(id); !article.IsScheduled(a, now) {
			t.Errorf("article is not scheduled: %s %v", a.Status, a.PublishedAt)
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

func sameTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// unschedule_article は予約中の記事だけを校閲待ちに戻し、公開日時を消す。戻した記事は fetch_pending の校閲待ちに戻り、
// もう一度予約できる。公開日時を過ぎて読者に見えた記事や、予約中でない記事は 409 で断る。
func TestSyncAPIUnscheduleArticle(t *testing.T) {
	env := newSyncEnv(t)
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	setClock(t, now)
	editor := env.token("desk", auth.RoleEditor)
	id := env.seedReview("取り消す記事", "to-unschedule")
	at := now.Add(2 * time.Hour)
	expectSyncStatus(t, env.call(editor, scheduleRequest(id, at)), http.StatusOK)

	rec := env.call(env.token("chief-admin", auth.RoleAdmin), unscheduleRequest(id))
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncPublishResponse](t, rec)
	if res.Action != SyncActionUnscheduleArticle || res.Article.Status != article.StatusReview || res.Article.PublishedAt != nil {
		t.Errorf("response = %+v", res)
	}
	if a := env.mustGetArticle(id); a.Status != article.StatusReview || a.PublishedAt != nil || a.LockedBy != nil {
		t.Errorf("unscheduled article = %s %v locked_by %v", a.Status, a.PublishedAt, a.LockedBy)
	}
	pending := decodeSync[SyncPendingResponse](t, env.call(editor, SyncRequest{Action: SyncActionFetchPending}))
	if len(pending.Articles) != 1 || pending.Articles[0].ID != id || len(pending.Scheduled) != 0 {
		t.Errorf("fetch_pending after unschedule = %+v", pending)
	}

	t.Run("not scheduled", func(t *testing.T) {
		rec := env.call(editor, unscheduleRequest(id))
		expectSyncStatus(t, rec, http.StatusConflict)
		if !strings.Contains(syncErrorMessage(t, rec), "校閲待ち") {
			t.Errorf("message = %s", rec.Body.String())
		}
	})
	t.Run("visible article stays visible", func(t *testing.T) {
		visible := seedArticle(t, env.db, article.Article{Title: "公開中の記事", Slug: "visible-stays", Status: article.StatusPublished,
			PublishedAt: ptrTime(now.Add(-time.Hour))})
		expectSyncStatus(t, env.call(editor, unscheduleRequest(visible)), http.StatusConflict)
		if a := env.mustGetArticle(visible); !article.IsVisible(a, now) {
			t.Errorf("visible article changed: %s %v", a.Status, a.PublishedAt)
		}
	})
	t.Run("publish time passed", func(t *testing.T) {
		again := env.seedReview("時刻を過ぎた予約", "passed")
		expectSyncStatus(t, env.call(editor, scheduleRequest(again, at)), http.StatusOK)
		setClock(t, at.Add(time.Minute))
		rec := env.call(editor, unscheduleRequest(again))
		expectSyncStatus(t, rec, http.StatusConflict)
		if !strings.Contains(syncErrorMessage(t, rec), "すでに公開") {
			t.Errorf("message = %s", rec.Body.String())
		}
		if a := env.mustGetArticle(again); a.Status != article.StatusPublished || a.PublishedAt == nil || !a.PublishedAt.Equal(at) {
			t.Errorf("published article changed: %s %v", a.Status, a.PublishedAt)
		}
	})
}

// fetch_pending は予約中の記事を、校閲待ち（articles）とは別の scheduled に公開日時つきで返す。予約を知らない古い
// ローカル CMS は、今までの形のまま校閲待ちだけを読み、予約中の記事を校閲待ちとして扱わない。
func TestSyncAPIFetchPendingScheduled(t *testing.T) {
	env := newSyncEnv(t)
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	setClock(t, now)
	editor := env.token("desk", auth.RoleEditor)
	review := env.seedReview("校閲待ちの記事", "waiting")
	later := env.seedReview("あとの予約", "later")
	sooner := env.seedReview("さきの予約", "sooner")
	expectSyncStatus(t, env.call(editor, scheduleRequest(later, now.Add(48*time.Hour))), http.StatusOK)
	expectSyncStatus(t, env.call(editor, scheduleRequest(sooner, now.Add(3*time.Hour))), http.StatusOK)
	seedArticle(t, env.db, article.Article{Title: "公開中の記事", Status: article.StatusPublished, PublishedAt: ptrTime(now.Add(-time.Hour))})

	rec := env.call(editor, SyncRequest{Action: SyncActionFetchPending})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncPendingResponse](t, rec)
	if len(res.Articles) != 1 || res.Articles[0].ID != review || res.Articles[0].PublishedAt != nil {
		t.Errorf("articles = %+v, want only the article in review", res.Articles)
	}
	if len(res.Scheduled) != 2 || res.Scheduled[0].ID != sooner || res.Scheduled[1].ID != later {
		t.Fatalf("scheduled = %+v, want [sooner later]", res.Scheduled)
	}
	if s := res.Scheduled[0]; s.Status != article.StatusPublished || s.PublishedAt == nil || !s.PublishedAt.Equal(now.Add(3*time.Hour)) ||
		s.Title != "さきの予約" || s.ContentMarkdown != "さきの予約の本文" {
		t.Errorf("scheduled[0] = %+v", s)
	}

	t.Run("old desktop still parses it", func(t *testing.T) {
		// 予約を知らない版のローカル CMS が読む形（この変更の前の SyncPendingResponse・SyncPendingArticle）。
		var old struct {
			Action   string `json:"action"`
			Articles []struct {
				ID              string         `json:"id"`
				Title           string         `json:"title"`
				Slug            string         `json:"slug"`
				ContentMarkdown string         `json:"content_markdown"`
				Excerpt         string         `json:"excerpt"`
				Status          article.Status `json:"status"`
				Kind            article.Kind   `json:"kind,omitempty"`
				Category        string         `json:"category"`
				CategoryName    string         `json:"category_name"`
				AuthorID        string         `json:"author_id"`
				AuthorName      string         `json:"author_name"`
				CreatedAt       time.Time      `json:"created_at"`
				UpdatedAt       time.Time      `json:"updated_at"`
			} `json:"articles"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &old); err != nil {
			t.Fatalf("old client cannot parse fetch_pending: %v", err)
		}
		if old.Action != SyncActionFetchPending || len(old.Articles) != 1 || old.Articles[0].ID != review ||
			old.Articles[0].Status != article.StatusReview || old.Articles[0].ContentMarkdown != "校閲待ちの記事の本文" {
			t.Errorf("old client reads %+v", old)
		}
		// 今までの項目は名前を変えず、すべて残している（足した項目だけが増える）。
		var raw struct {
			Articles []map[string]json.RawMessage `json:"articles"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"id", "title", "slug", "content_markdown", "excerpt", "status", "kind", "category",
			"category_name", "author_id", "author_name", "created_at", "updated_at"} {
			if _, ok := raw.Articles[0][key]; !ok {
				t.Errorf("articles[0] lost the field %q", key)
			}
		}
	})
}

// server_info は、有効なトークンならどのロールにも、予約に対応していることを返す。トークンが無ければ 401。
func TestSyncAPIServerInfo(t *testing.T) {
	env := newSyncEnv(t)
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleEditor, auth.RoleAdmin} {
		rec := env.call(env.token("someone", role), SyncRequest{Action: SyncActionServerInfo})
		expectSyncStatus(t, rec, http.StatusOK)
		if res := decodeSync[SyncServerInfoResponse](t, rec); res.Action != SyncActionServerInfo || !res.Schedule {
			t.Errorf("%s: server_info = %+v", role, res)
		}
	}
	body, _ := json.Marshal(SyncRequest{Action: SyncActionServerInfo})
	expectSyncStatus(t, env.callRaw("", "application/json", body), http.StatusUnauthorized)
}

// --- ローカル CMS（デスクトップ） ---

func TestScheduleTimeText(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, jst)
	for _, tc := range []struct {
		at   time.Time
		lang i18n.Lang
		want string
	}{
		{time.Date(2026, 11, 3, 9, 0, 0, 0, jst), i18n.JA, "11月3日 9:00"},
		{time.Date(2026, 11, 3, 0, 5, 0, 0, time.UTC), i18n.JA, "11月3日 9:05"},
		{time.Date(2027, 1, 2, 18, 30, 0, 0, jst), i18n.JA, "2027年1月2日 18:30"},
		{time.Date(2026, 11, 3, 9, 0, 0, 0, jst), i18n.EN, "Nov 3, 9:00"},
		{time.Date(2027, 1, 2, 18, 30, 0, 0, jst), i18n.EN, "Jan 2, 2027, 18:30"},
	} {
		if got := scheduleTimeText(tc.lang, tc.at, now); got != tc.want {
			t.Errorf("scheduleTimeText(%s, %v) = %q, want %q", tc.lang, tc.at, got, tc.want)
		}
	}
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 11, 1, 10, 0, 0, 0, jst), time.Date(2026, 11, 1, 11, 0, 0, 0, jst)},
		{time.Date(2026, 11, 1, 10, 54, 59, 0, jst), time.Date(2026, 11, 1, 11, 0, 0, 0, jst)},
		{time.Date(2026, 11, 1, 10, 56, 0, 0, jst), time.Date(2026, 11, 1, 12, 0, 0, 0, jst)},
		{time.Date(2026, 11, 1, 23, 58, 0, 0, jst), time.Date(2026, 11, 2, 1, 0, 0, 0, jst)},
	} {
		if got := defaultScheduleAt(tc.now); !got.Equal(tc.want) {
			t.Errorf("defaultScheduleAt(%v) = %v, want %v", tc.now, got.In(jst), tc.want)
		}
	}
}

// remotePendingList は /admin/remote/pending の応答。
type remotePendingList struct {
	Count     int                   `json:"count"`
	Articles  []remotePendingItem   `json:"articles"`
	Scheduled []remoteScheduledItem `json:"scheduled"`
	Schedule  remoteScheduleView    `json:"schedule"`
}

// 予約に対応した公開サーバーでは、「届いた原稿」と「公開サーバー」の一覧に「日時を決めて予約」を出し、日本時間で選んだ
// 日時を UTC にして予約する。予約中の記事は「予約: 11月3日 9:00」と「予約を取り消す」つきで並び、取り消すと校閲待ちに戻る。
// 予約と取り消しは CSRF トークンの無い要求を断る。
func TestRemoteScheduleFromDesktop(t *testing.T) {
	env := newRemoteEnv(t)
	setClock(t, time.Date(2026, 11, 1, 10, 0, 0, 0, jst))
	env.configure("sato", auth.RoleEditor)
	submitted := decodeSync[SyncSubmitResponse](t, env.remote.call(env.remote.token("tanaka", auth.RoleWriter), submitRequest(SyncArticle{
		Title: "合唱部の冬の演奏会", Slug: "choir-winter", ContentMarkdown: "冬の演奏会の案内。",
	})))
	id := submitted.Article.ID
	editor := env.local.loggedIn(chief)

	var list remotePendingList
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if want := (remoteScheduleView{Available: true, Default: "2026-11-01T11:00", Min: "2026-11-01T10:01"}); list.Schedule != want {
		t.Errorf("schedule = %+v, want %+v", list.Schedule, want)
	}
	if len(list.Articles) != 1 || len(list.Scheduled) != 0 {
		t.Fatalf("pending = %+v", list)
	}

	// 「届いた原稿」の公開サーバーの原稿に、「日時を決めて予約」の入力欄（次の正時が初期値）を出す。
	inbox := editor.get("/admin/manuscripts")
	expectStatus(t, inbox, http.StatusOK)
	expectContains(t, inbox.Body, "日時を決めて予約", `type="datetime-local"`, `name="publish_at"`, `value="2026-11-01T11:00"`,
		`min="2026-11-01T10:01"`, `data-remote-schedule-form="`+id+`"`, "公開サーバーで公開する", "/static/js/remote_sync.js")
	if strings.Contains(inbox.Body, "公開サーバーを更新すると") {
		t.Error("inbox shows the update note for a server that supports scheduling")
	}

	path := "/admin/remote/pending/" + id + "/schedule"
	t.Run("csrf is required", func(t *testing.T) {
		expectStatus(t, editor.postRaw(path, url.Values{"publish_at": {"2026-11-03T09:00"}}), http.StatusForbidden)
		if a := env.remote.mustGetArticle(id); a.Status != article.StatusReview {
			t.Errorf("scheduled without a CSRF token: %s", a.Status)
		}
	})
	t.Run("invalid times", func(t *testing.T) {
		for _, v := range []string{"", "11月3日", "2026-11-01T10:00", "2026-10-31T09:00"} {
			res := editor.postJSON(path, url.Values{"publish_at": {v}}, nil)
			expectStatus(t, res, http.StatusUnprocessableEntity)
		}
	})
	t.Run("writer cannot schedule", func(t *testing.T) {
		expectStatus(t, env.local.loggedIn(studentA).postJSON(path, url.Values{"publish_at": {"2026-11-03T09:00"}}, nil), http.StatusForbidden)
	})

	var scheduled struct {
		Message   string         `json:"message"`
		Article   SyncArticleRef `json:"article"`
		PublishAt time.Time      `json:"publish_at"`
		Label     string         `json:"label"`
	}
	expectStatus(t, editor.postJSON(path, url.Values{"publish_at": {"2026-11-03T09:00"}}, &scheduled), http.StatusOK)
	wantAt := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC) // 日本時間の 11月3日 9:00
	if scheduled.Label != "予約: 11月3日 9:00" || !scheduled.PublishAt.Equal(wantAt) || !strings.Contains(scheduled.Message, "11月3日 9:00") {
		t.Errorf("schedule = %+v", scheduled)
	}
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished || a.PublishedAt == nil || !a.PublishedAt.Equal(wantAt) {
		t.Fatalf("remote article = %s %v, want published at %v", a.Status, a.PublishedAt, wantAt)
	}
	expectSyncStatus(t, env.remote.get("/articles/choir-winter"), http.StatusNotFound)

	list = remotePendingList{}
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if list.Count != 0 || len(list.Scheduled) != 1 {
		t.Fatalf("pending after schedule = %+v", list)
	}
	if s := list.Scheduled[0]; s.ID != id || s.Label != "予約: 11月3日 9:00" || !s.PublishAt.Equal(wantAt) ||
		s.UnscheduleConfirm != "「合唱部の冬の演奏会」の予約を取り消し、校閲待ちに戻します。よろしいですか？" {
		t.Errorf("scheduled item = %+v", s)
	}

	// 「届いた原稿」には、予約中の記事が日時と「予約を取り消す」（確認つき）とともに並ぶ。
	inbox = editor.get("/admin/manuscripts")
	expectStatus(t, inbox, http.StatusOK)
	expectContains(t, inbox.Body, "公開サーバーで予約中の記事", "予約: 11月3日 9:00", "合唱部の冬の演奏会", "予約を取り消す",
		`data-remote-unschedule="`+id+`"`, `data-confirm-message="「合唱部の冬の演奏会」の予約を取り消し、校閲待ちに戻します。よろしいですか？"`)

	unschedule := "/admin/remote/scheduled/" + id + "/unschedule"
	t.Run("unschedule requires csrf", func(t *testing.T) {
		expectStatus(t, editor.postRaw(unschedule, nil), http.StatusForbidden)
		if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished {
			t.Errorf("unscheduled without a CSRF token: %s", a.Status)
		}
	})
	var cancelled struct {
		Message string         `json:"message"`
		Article SyncArticleRef `json:"article"`
	}
	expectStatus(t, editor.postJSON(unschedule, nil, &cancelled), http.StatusOK)
	if cancelled.Article.Status != article.StatusReview || cancelled.Message != "予約を取り消し、校閲待ちに戻しました" {
		t.Errorf("unschedule = %+v", cancelled)
	}
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusReview || a.PublishedAt != nil {
		t.Errorf("remote article after unschedule = %s %v", a.Status, a.PublishedAt)
	}
	list = remotePendingList{}
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if list.Count != 1 || len(list.Scheduled) != 0 {
		t.Errorf("pending after unschedule = %+v", list)
	}

	t.Run("publishing a scheduled article now is a conflict", func(t *testing.T) {
		expectStatus(t, editor.postJSON(path, url.Values{"publish_at": {"2026-11-03T09:00"}}, nil), http.StatusOK)
		res := editor.postJSON("/admin/remote/pending/"+id+"/publish", nil, nil)
		expectStatus(t, res, http.StatusBadGateway)
		if a := env.remote.mustGetArticle(id); !article.IsScheduled(a, clockNow()) {
			t.Errorf("scheduled article was published now: %s %v", a.Status, a.PublishedAt)
		}
	})
}

// 一覧の部品（ダッシュボードと「公開サーバー」の画面）は、「承認して本番公開」の横に「日時を決めて予約」の入力欄と、
// 予約中の記事の一覧を置く。ブラウザのスクリプトは予約・取り消しの API を CSRF トークン付きで呼ぶ。
func TestRemoteSchedulePartials(t *testing.T) {
	env := newTestEnv(t)
	var buf bytes.Buffer
	if err := env.server.echo.Renderer.Render(&buf, "remote_pending_panel", page{Lang: "ja"}, nil); err != nil {
		t.Fatal(err)
	}
	expectContains(t, buf.String(), "承認して本番公開", "日時を決めて予約", `type="datetime-local"`, `name="publish_at"`,
		"data-remote-schedule-template", `id="remote-sync-modal-schedule"`, "data-remote-scheduled-list", "公開サーバーで予約中の記事",
		`data-unschedule-label="予約を取り消す"`, "data-remote-schedule-note")

	buf.Reset()
	if err := env.server.echo.Renderer.Render(&buf, "remote_pending_panel", page{Lang: "en"}, nil); err != nil {
		t.Fatal(err)
	}
	expectContains(t, buf.String(), "Schedule for this time", "Scheduled on the public server", "Cancel schedule")

	res := env.newClient().get("/static/js/remote_sync.js")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "/schedule'", "/admin/remote/scheduled/", "/unschedule'", "data-remote-unschedule", "sorus.postJSON(")
	// CSRF トークンは共通部品（sorus_common.js）の要求が付ける。
	expectContains(t, env.newClient().get("/static/js/sorus_common.js").Body, "X-CSRF-Token")
}

// oldSyncServer は予約を知らない古い公開サーバーを模す。同期 API への要求を本物の公開サーバー（remote）に中継するが、
// server_info・schedule_article・unschedule_article は古い公開サーバーと同じく未対応の action として 400 で断り、
// fetch_pending の応答から scheduled・published_at を除く。届いた action を記録する。
type oldSyncServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	actions []string
}

func newOldSyncServer(t *testing.T, remote *syncEnv) *oldSyncServer {
	t.Helper()
	o := &oldSyncServer{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(body, &req)
		o.mu.Lock()
		o.actions = append(o.actions, req.Action)
		o.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		switch req.Action {
		case SyncActionServerInfo, SyncActionScheduleArticle, SyncActionUnscheduleArticle:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"message":%q}`, fmt.Sprintf("未対応の action です: %q", req.Action))
		case SyncActionFetchPending:
			// 応答を書き換えるため、圧縮していない応答を受け取る。
			r.Header.Del("Accept-Encoding")
			rec := httptest.NewRecorder()
			remote.server.Handler().ServeHTTP(rec, r)
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			delete(res, "scheduled")
			for _, a := range res["articles"].([]any) {
				delete(a.(map[string]any), "published_at")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Code)
			_ = json.NewEncoder(w).Encode(res)
		default:
			remote.server.Handler().ServeHTTP(w, r)
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *oldSyncServer) called(action string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Contains(o.actions, action)
}

// 予約を知らない古い公開サーバーでは、予約の選択肢を出さずに公開サーバーの更新を案内する。直接予約しようとしても
// 409 と同じ案内で断り、即時公開（publish_article）には切り替えない。予約に対応していると覚えていた後に公開サーバーが
// 古い版に戻されても同じ。
func TestRemoteScheduleAgainstOldServer(t *testing.T) {
	env := newRemoteEnv(t)
	setClock(t, time.Date(2026, 11, 1, 10, 0, 0, 0, jst))
	old := newOldSyncServer(t, env.remote)
	cfg := remoteConfig{URL: old.srv.URL, SyncPath: env.remote.server.cfg.SyncAPIPath, Token: env.remote.token("sato", auth.RoleEditor)}
	if err := env.local.server.saveRemoteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	submitted := decodeSync[SyncSubmitResponse](t, env.remote.call(env.remote.token("tanaka", auth.RoleWriter), submitRequest(SyncArticle{
		Title: "古いサーバーの記事", Slug: "old-server", ContentMarkdown: "古い公開サーバーに届いた原稿。",
	})))
	id := submitted.Article.ID
	editor := env.local.loggedIn(chief)
	const note = "公開サーバーを更新すると、予約して公開できます"

	var list remotePendingList
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if list.Schedule != (remoteScheduleView{Note: note}) || list.Scheduled == nil || len(list.Scheduled) != 0 || list.Count != 1 {
		t.Errorf("pending from an old server = %+v", list)
	}
	if !old.called(SyncActionServerInfo) {
		t.Error("the desktop did not ask the server whether it supports scheduling")
	}

	inbox := editor.get("/admin/manuscripts")
	expectStatus(t, inbox, http.StatusOK)
	expectContains(t, inbox.Body, note, "公開サーバーで公開する")
	for _, hidden := range []string{"日時を決めて予約", `type="datetime-local"`, "data-remote-schedule-form"} {
		if strings.Contains(inbox.Body, hidden) {
			t.Errorf("inbox for an old server shows %q", hidden)
		}
	}

	expectRefused := func(t *testing.T) {
		t.Helper()
		res := editor.postJSON("/admin/remote/pending/"+id+"/schedule", url.Values{"publish_at": {"2026-11-03T09:00"}}, nil)
		expectStatus(t, res, http.StatusConflict)
		if msg := jsonMessage(t, res); msg != note || !strings.Contains(res.Body, `"schedule_unsupported":true`) {
			t.Errorf("schedule on an old server = %s", res.Body)
		}
		if old.called(SyncActionPublishArticle) {
			t.Fatal("the desktop fell back to publish_article")
		}
		if a := env.remote.mustGetArticle(id); a.Status != article.StatusReview || a.PublishedAt != nil {
			t.Fatalf("remote article changed: %s %v", a.Status, a.PublishedAt)
		}
	}
	t.Run("direct schedule attempt", func(t *testing.T) {
		expectRefused(t)
		if old.called(SyncActionScheduleArticle) {
			t.Error("schedule_article was sent although the server does not support it")
		}
	})
	t.Run("server downgraded after the check", func(t *testing.T) {
		rememberRemoteSchedule(cfg, true)
		expectRefused(t)
		if !old.called(SyncActionScheduleArticle) {
			t.Error("schedule_article was not tried")
		}
		// 断られたことを覚え、次からは選択肢を出さない。
		list = remotePendingList{}
		expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
		if list.Schedule.Available || list.Schedule.Note != note {
			t.Errorf("schedule view after refusal = %+v", list.Schedule)
		}
	})
	t.Run("unschedule on an old server", func(t *testing.T) {
		res := editor.postJSON("/admin/remote/scheduled/"+id+"/unschedule", nil, nil)
		expectStatus(t, res, http.StatusConflict)
		if jsonMessage(t, res) != note {
			t.Errorf("unschedule on an old server = %s", res.Body)
		}
	})
}

// 予約できるかは、接続先（URL・トークン）ごとに一度だけ確かめて覚える。対応していない結果は、公開サーバーを更新した
// 後に開き直せば予約できるよう、remoteScheduleRecheck を過ぎたら確かめ直す。接続できないときは覚えない。
func TestRemoteCanScheduleCachesPerConfig(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	supported := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		w.Header().Set("Content-Type", "application/json")
		if !supported {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message":"未対応の action です: \"server_info\""}`)
			return
		}
		fmt.Fprint(w, `{"action":"server_info","schedule":true}`)
	}))
	t.Cleanup(srv.Close)
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	ctx := context.Background()
	cfg := remoteConfig{URL: srv.URL, Token: "token-a"}

	for range 3 {
		if ok, err := remoteCanSchedule(ctx, cfg); ok || err != nil {
			t.Fatalf("old server: %v, %v", ok, err)
		}
	}
	if n := count(); n != 1 {
		t.Errorf("probes = %d, want 1 (cached)", n)
	}
	// 公開サーバーを更新した。覚えている「対応していない」の期限が切れると確かめ直す。
	mu.Lock()
	supported = true
	mu.Unlock()
	remoteScheduleProbes.Lock()
	key := remoteProbeKey(cfg)
	p := remoteScheduleProbes.m[key]
	p.checkedAt = p.checkedAt.Add(-remoteScheduleRecheck)
	remoteScheduleProbes.m[key] = p
	remoteScheduleProbes.Unlock()
	for range 3 {
		if ok, err := remoteCanSchedule(ctx, cfg); !ok || err != nil {
			t.Fatalf("updated server: %v, %v", ok, err)
		}
	}
	if n := count(); n != 2 {
		t.Errorf("probes = %d, want 2", n)
	}
	// トークンを変えると、別の接続先として確かめ直す。
	if ok, err := remoteCanSchedule(ctx, remoteConfig{URL: srv.URL, Token: "token-b"}); !ok || err != nil {
		t.Fatalf("other token: %v, %v", ok, err)
	}
	if n := count(); n != 3 {
		t.Errorf("probes = %d, want 3", n)
	}
	// 接続できなければエラーを返し、覚えない。
	down := remoteConfig{URL: "http://127.0.0.1:1", Token: "token-a"}
	if _, err := remoteCanSchedule(ctx, down); err == nil {
		t.Error("unreachable server: want an error")
	}
	remoteScheduleProbes.Lock()
	_, cached := remoteScheduleProbes.m[remoteProbeKey(down)]
	remoteScheduleProbes.Unlock()
	if cached {
		t.Error("an unreachable server was cached")
	}
}
