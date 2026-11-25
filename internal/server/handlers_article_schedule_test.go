package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
)

// scheduleForm は記事フォームの「日時を決めて予約」の送信（action=publish と予約日時 publish_at）。
func scheduleForm(title, body, publishAt string) url.Values {
	return url.Values{"title": {title}, "category_id": {"1"}, "content_markdown": {body},
		"action": {"publish"}, "publish_at": {publishAt}}
}

// 下書きを「公開する」→「日時を決めて予約」で予約すると、編集した内容を保存して予約投稿（公開＋未来の公開日時）にし、
// 予約した日時まで読者には見えない。予約だった記事の集合に入り、時刻が来れば記事を保存し直さなくても読者と検索に出る。
func TestSchedulePublishFromArticleForm(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	ed := env.loggedIn(chief)
	reader := env.newClient()
	id := ed.createArticle("予約する記事", "初稿")
	edit := "/admin/articles/" + id

	docs := func() uint64 {
		t.Helper()
		n, err := env.server.search.DocCount()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	indexed := docs()

	when := futureLocal(2 * time.Hour)
	expectRedirect(t, ed.postForm(edit, scheduleForm("予約する記事", "直した本文の合言葉", when)), edit+"/edit")
	a := env.mustGetArticle(id)
	if a.Status != article.StatusPublished || !article.IsScheduled(a, time.Now()) || a.ContentMarkdown != "直した本文の合言葉" {
		t.Fatalf("after schedule: %+v", a)
	}
	if got := a.PublishedAt.In(jst).Format(datetimeLocalLayout); got != when {
		t.Fatalf("published_at = %s, want %s (JST)", got, when)
	}
	env.expectNoStoredScheduled("schedule from the article form")
	if at, ok := scheduledSetIn(t, env)[id]; !ok || !at.Equal(*a.PublishedAt) {
		t.Fatalf("scheduled set = %v (%v), want %v", at, ok, a.PublishedAt)
	}
	if got := env.revisionComments(id); got[len(got)-1] != "予約公開" {
		t.Fatalf("revision comments = %v, want the last to be 予約公開", got)
	}
	expectContains(t, ed.get(edit+"/edit").Body, `id="schedule-banner"`, "保存（予約を維持）")
	expectStatus(t, reader.get("/articles/"+a.Slug), http.StatusNotFound)
	// 検索インデックスには予約した日時とともに入れ、検索は公開日時を迎えるまで出さない。
	if n := docs(); n != indexed+1 {
		t.Fatalf("search index has %d documents, want %d", n, indexed+1)
	}
	expectNotContains(t, reader.get("/search?q="+url.QueryEscape("合言葉")).Body, "予約する記事")

	setClock(t, time.Now().Add(3*time.Hour))
	expectStatus(t, reader.get("/articles/"+a.Slug), http.StatusOK)
}

// 新しい記事は、作成と同じ1回の送信で予約できる。
func TestSchedulePublishNewArticle(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	ed := env.loggedIn(chief)
	reader := env.newClient()

	loc := expectRedirect(t, ed.postForm("/admin/articles/new", scheduleForm("作成と同時に予約", "本文", futureLocal(time.Hour))), "/admin/articles/")
	m := editPathPattern.FindStringSubmatch(loc)
	if m == nil {
		t.Fatalf("unexpected redirect %q", loc)
	}
	a := env.mustGetArticle(m[1])
	if !article.IsScheduled(a, time.Now()) {
		t.Fatalf("after create with schedule: %+v", a)
	}
	if _, ok := scheduledSetIn(t, env)[a.ID]; !ok {
		t.Fatal("new scheduled article is not in the scheduled set")
	}
	if got := env.revisionComments(a.ID); len(got) != 1 || got[0] != "予約公開" {
		t.Fatalf("revision comments = %v", got)
	}
	expectStatus(t, reader.get("/articles/"+a.Slug), http.StatusNotFound)
	setClock(t, time.Now().Add(2*time.Hour))
	expectStatus(t, reader.get("/articles/"+a.Slug), http.StatusOK)
}

// 予約日時が空なら、今と同じくすぐに公開する。
func TestPublishWithEmptyPublishAtIsImmediate(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	id := ed.createArticle("すぐに公開", "本文")
	before := time.Now().UTC().Add(-time.Second)
	expectRedirect(t, ed.postForm("/admin/articles/"+id, scheduleForm("すぐに公開", "本文", "")), "/admin/articles/"+id+"/edit?flash=published")
	a := env.mustGetArticle(id)
	if !article.IsVisible(a, time.Now()) || a.PublishedAt.Before(before) {
		t.Fatalf("after publish: %+v", a)
	}
	if _, ok := scheduledSetIn(t, env)[id]; ok {
		t.Fatal("immediately published article is in the scheduled set")
	}
	expectStatus(t, env.newClient().get("/articles/"+a.Slug), http.StatusOK)

	expectRedirect(t, ed.postForm("/admin/articles/new", scheduleForm("作成と同時に公開", "本文", "")), "/admin/articles/")
}

// 過去・近すぎる・形式の正しくない予約日時は、わかる言葉で断り、記事を保存も公開もしない（入力した内容は画面に残す）。
func TestSchedulePublishRejectsBadTimes(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	id := ed.createArticle("予約に失敗する記事", "保存済みの本文")
	edit := "/admin/articles/" + id
	stored := env.mustGetArticle(id)
	articles := env.countRows(`SELECT COUNT(*) FROM articles`)

	now := time.Date(2030, 1, 1, 9, 0, 30, 0, jst)
	setClock(t, now)
	for _, tc := range []struct{ name, publishAt, want string }{
		{"malformed", "tomorrow", "予約日時の形式が正しくありません"},
		{"past", "2030-01-01T08:00", "現在より後の日時"},
		{"now", "2030-01-01T09:00", "現在より後の日時"},
		{"too soon", "2030-01-01T09:01", "今から1分以上先"},
	} {
		res := ed.postForm(edit, scheduleForm("予約に失敗する記事", "直しかけの本文", tc.publishAt))
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, tc.want, "直しかけの本文", `id="schedule-box"`, `value="`+tc.publishAt+`"`)
		a := env.mustGetArticle(id)
		if a.Status != article.StatusDraft || a.PublishedAt != nil || a.ContentMarkdown != "保存済みの本文" || !a.UpdatedAt.Equal(stored.UpdatedAt) {
			t.Fatalf("%s: article changed: %+v", tc.name, a)
		}

		res = ed.postForm("/admin/articles/new", scheduleForm("作られない記事", "本文", tc.publishAt))
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, tc.want)
		if n := env.countRows(`SELECT COUNT(*) FROM articles`); n != articles {
			t.Fatalf("%s: %d articles, want %d", tc.name, n, articles)
		}
	}
	if got := env.revisionComments(id); len(got) != 1 {
		t.Fatalf("revisions after refused schedules = %v", got)
	}
	// 1分より先なら予約できる。
	expectRedirect(t, ed.postForm(edit, scheduleForm("予約に失敗する記事", "本文", "2030-01-01T09:02")), edit+"/edit")
	if a := env.mustGetArticle(id); a.Status != article.StatusPublished || !a.PublishedAt.Equal(time.Date(2030, 1, 1, 9, 2, 0, 0, jst)) {
		t.Fatalf("after schedule at the earliest time: %+v", a)
	}
}

// 読者に見えたことのある記事は、日時を付けた「公開する」を断り、最初の公開日時と内容を保つ（D9）。
func TestSchedulePublishRefusesPublishedArticle(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	id := ed.createArticle("公開済みの記事", "公開した本文")
	edit := "/admin/articles/" + id
	expectRedirect(t, ed.postForm(edit, scheduleForm("公開済みの記事", "公開した本文", "")), edit+"/edit?flash=published")
	first := env.mustGetArticle(id)

	res := ed.postForm(edit, scheduleForm("公開済みの記事", "直した本文", futureLocal(time.Hour)))
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "一度公開した記事は予約投稿にできません")
	expectNotContains(t, res.Body, `id="schedule-box"`)
	a := env.mustGetArticle(id)
	if !article.IsVisible(a, time.Now()) || !a.PublishedAt.Equal(*first.PublishedAt) || a.ContentMarkdown != "公開した本文" {
		t.Fatalf("after refused schedule: %+v (first published_at %v)", a, first.PublishedAt)
	}

	// 下書きに戻した記事も、一度公開しているため予約にできない。
	draft := scheduleForm("公開済みの記事", "公開した本文", "")
	draft.Set("action", "draft")
	expectRedirect(t, ed.postForm(edit, draft), edit+"/edit?flash=saved")
	expectStatus(t, ed.postForm(edit, scheduleForm("公開済みの記事", "直した本文", futureLocal(time.Hour))), http.StatusUnprocessableEntity)
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft || !a.PublishedAt.Equal(*first.PublishedAt) {
		t.Fatalf("after refused schedule of an unpublished article: %+v", a)
	}
}

// 公開の権限の無い執筆者は、予約もできない。
func TestWriterCannotSchedulePublish(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("執筆者の記事", "本文")
	expectStatus(t, writer.postForm("/admin/articles/"+id, scheduleForm("執筆者の記事", "本文", futureLocal(time.Hour))), http.StatusForbidden)
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft || a.PublishedAt != nil {
		t.Fatalf("after writer's schedule: %+v", a)
	}
	articles := env.countRows(`SELECT COUNT(*) FROM articles`)
	expectStatus(t, writer.postForm("/admin/articles/new", scheduleForm("執筆者の新しい記事", "本文", futureLocal(time.Hour))), http.StatusForbidden)
	if n := env.countRows(`SELECT COUNT(*) FROM articles`); n != articles {
		t.Fatalf("writer created an article: %d, want %d", n, articles)
	}
}

// 「公開する」から予約した記事は、公開先を設定していれば、まだ反映していない予約として数える。
func TestSchedulePublishIsPendingScheduledReflect(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	ed := env.loggedIn(chief)
	saveFolderDestination(t, env.loggedIn(admin), filepath.Join(t.TempDir(), "site"))
	id := ed.createArticle("反映を待つ予約", "本文")
	expectRedirect(t, ed.postForm("/admin/articles/"+id, scheduleForm("反映を待つ予約", "本文", futureLocal(2*time.Hour))), "/admin/articles/"+id+"/edit")
	at := *env.mustGetArticle(id).PublishedAt
	expectPendingOn(t, env.server, "scheduled from the article form", 1, at)
}

// 「日時を決めて予約」は、公開できる人が一度も公開していない記事（新規・下書き）を書くときだけ出し、公開中・予約中の
// 記事と、執筆者の画面には出さない。
func TestScheduleControlOnArticleEdit(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	writer := env.loggedIn(studentA)
	control := []string{`id="schedule-box"`, "日時を決めて予約", `name="publish_at"`, `type="datetime-local"`, `id="schedule-button"`}

	draft := ed.createArticle("下書き", "本文")
	for _, path := range []string{"/admin/articles/new", "/admin/articles/new?kind=page", "/admin/articles/" + draft + "/edit"} {
		res := ed.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, append(control, `id="publish-button"`, "公開する")...)
	}

	published := ed.createArticle("公開中", "本文")
	expectRedirect(t, ed.postForm("/admin/articles/"+published, scheduleForm("公開中", "本文", "")), "/admin/articles/")
	scheduled := ed.createArticle("予約中", "本文")
	expectRedirect(t, ed.postForm("/admin/articles/"+scheduled, scheduleForm("予約中", "本文", futureLocal(time.Hour))), "/admin/articles/")
	unpublished := ed.createArticle("公開してから下書きへ", "本文")
	expectRedirect(t, ed.postForm("/admin/articles/"+unpublished, scheduleForm("公開してから下書きへ", "本文", "")), "/admin/articles/")
	back := scheduleForm("公開してから下書きへ", "本文", "")
	back.Set("action", "draft")
	expectRedirect(t, ed.postForm("/admin/articles/"+unpublished, back), "/admin/articles/")
	for _, id := range []string{published, scheduled, unpublished} {
		res := ed.get("/admin/articles/" + id + "/edit")
		expectStatus(t, res, http.StatusOK)
		expectNotContains(t, res.Body, control...)
	}

	writerDraft := writer.createArticle("執筆者の下書き", "本文")
	for _, path := range []string{"/admin/articles/new", "/admin/articles/" + writerDraft + "/edit"} {
		res := writer.get(path)
		expectStatus(t, res, http.StatusOK)
		expectNotContains(t, res.Body, control...)
	}
}

// デスクトップの利用者（すべての役割を持つ1人）は、新しい記事の画面から予約できる。
func TestScheduleControlForDesktopUser(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	res := c.get("/admin/articles/new")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="schedule-box"`, "日時を決めて予約", `name="publish_at"`)

	loc := expectRedirect(t, c.postForm("/admin/articles/new", scheduleForm("ひとりで予約", "本文", futureLocal(time.Hour))), "/admin/articles/")
	m := editPathPattern.FindStringSubmatch(loc)
	if m == nil {
		t.Fatalf("unexpected redirect %q", loc)
	}
	if a := env.mustGetArticle(m[1]); !article.IsScheduled(a, time.Now()) {
		t.Fatalf("desktop user's scheduled article: %+v", a)
	}
}

// flashAttr は管理画面のトースト通知（layout の data-flash）に msg を出す属性。
func flashAttr(msg string) string { return `data-flash="` + msg + `"` }

// scheduledFlash は予約した直後のトースト通知。日時は予約のバナー（formatTimePtr）と同じ書式で出す。
func scheduledFlash(at time.Time) string {
	return flashAttr(at.In(jst).Format("2006/01/02 15:04") + " に公開するよう予約しました")
}

// 「日時を決めて予約」のあとは「保存しました」ではなく、予約した日時を書いた通知を出す（下書きからでも、新しい記事の
// 作成と同時でも）。すぐに公開したとき・予約を保ったまま保存したときは、これまでの通知のまま。
func TestSchedulePublishFlashNamesTheTime(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	ed := env.loggedIn(chief)

	id := ed.createArticle("予約を知らせる記事", "本文")
	edit := "/admin/articles/" + id
	loc := expectRedirect(t, ed.postForm(edit, scheduleForm("予約を知らせる記事", "本文", futureLocal(2*time.Hour))), edit+"/edit?flash=scheduled")
	at := *env.mustGetArticle(id).PublishedAt
	body := ed.get(loc).Body
	expectContains(t, body, scheduledFlash(at), `id="schedule-banner"`)
	expectNotContains(t, body, flashAttr("保存しました"))

	// 予約を保ったままの保存は「保存しました」。
	keep := scheduleForm("予約を知らせる記事", "直した本文", "")
	keep.Set("action", "save")
	loc = expectRedirect(t, ed.postForm(edit, keep), edit+"/edit?flash=saved")
	expectContains(t, ed.get(loc).Body, flashAttr("保存しました"))

	// 新しい記事を作成と同時に予約しても、予約した日時を知らせる。
	loc = expectRedirect(t, ed.postForm("/admin/articles/new", scheduleForm("作成して予約を知らせる", "本文", futureLocal(3*time.Hour))), "/admin/articles/")
	m := editPathPattern.FindStringSubmatch(loc)
	if m == nil || !strings.HasSuffix(loc, "?flash=scheduled") {
		t.Fatalf("unexpected redirect %q", loc)
	}
	expectContains(t, ed.get(loc).Body, scheduledFlash(*env.mustGetArticle(m[1]).PublishedAt))

	// すぐに公開したときは「公開しました」「記事を作成しました」のまま。
	now := ed.createArticle("すぐに公開を知らせる記事", "本文")
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+now, scheduleForm("すぐに公開を知らせる記事", "本文", "")), "/admin/articles/"+now+"/edit?flash=published")
	body = ed.get(loc).Body
	expectContains(t, body, flashAttr("公開しました"))
	expectNotContains(t, body, "に公開するよう予約しました")
	loc = expectRedirect(t, ed.postForm("/admin/articles/new", scheduleForm("作成してすぐに公開", "本文", "")), "/admin/articles/")
	if !strings.HasSuffix(loc, "?flash=created") {
		t.Fatalf("immediate publish on create redirected to %q", loc)
	}
	expectContains(t, ed.get(loc).Body, flashAttr("記事を作成しました"))

	// 予約の通知の画面を、予約でなくなってから（「今すぐ公開」のあとで）開いたときは、日時を付けない。
	expectRedirect(t, ed.postForm(edit, scheduleForm("予約を知らせる記事", "直した本文", "")), edit+"/edit?flash=published")
	expectContains(t, ed.get(edit+"/edit?flash=scheduled").Body, flashAttr("公開するよう予約しました"))
}

// 校閲の承認で予約したときも、予約した日時を知らせる（画面の遷移でも fetch の応答でも）。すぐに承認して公開したときは、
// これまでの「承認して公開しました」のまま。
func TestApproveScheduleFlashNamesTheTime(t *testing.T) {
	env := newTestEnvWith(t, Config{DisableScheduler: true}, nil)
	writer := env.loggedIn(studentA)
	ed := env.loggedIn(chief)
	review := func(title string) string {
		t.Helper()
		id := writer.createArticle(title, "本文")
		expectRedirect(t, writer.postForm("/admin/articles/"+id+"/submit-review", nil), "/admin/articles?status=review")
		return id
	}

	id := review("承認して予約する記事")
	loc := expectRedirect(t, ed.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"publish_at": {futureLocal(2 * time.Hour)}}),
		"/admin/articles/"+id+"/edit?flash=scheduled")
	body := ed.get(loc).Body
	expectContains(t, body, scheduledFlash(*env.mustGetArticle(id).PublishedAt))
	expectNotContains(t, body, flashAttr("保存しました"), flashAttr("承認して予約投稿にしました"))

	id = review("fetch で承認して予約する記事")
	res := ed.postAPI("/admin/articles/"+id+"/approve-publish", "application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"publish_at": {futureLocal(3 * time.Hour)}}.Encode()))
	expectStatus(t, res, http.StatusOK)
	var msg struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Body), &msg); err != nil {
		t.Fatalf("approve body = %s (%v)", res.Body, err)
	}
	want := env.mustGetArticle(id).PublishedAt.In(jst).Format("2006/01/02 15:04") + " に公開するよう予約しました"
	if msg.Status != string(article.StatusScheduled) || msg.Message != want {
		t.Fatalf("approve response = %+v, want message %q", msg, want)
	}

	id = review("承認してすぐに公開する記事")
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+id+"/approve-publish", nil), "/admin/articles/"+id+"/edit?flash=approved")
	body = ed.get(loc).Body
	expectContains(t, body, flashAttr("承認して公開しました"))
	expectNotContains(t, body, "に公開するよう予約しました")
}
