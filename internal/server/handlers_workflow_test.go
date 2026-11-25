package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
)

// request は CSRF ヘッダー付きで任意のメソッドの要求を送る。headers は追加のヘッダー。
func (c *client) request(method, path string, body io.Reader, headers map[string]string) *response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.env.srv.URL+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set(CSRFHeaderName, c.csrfToken())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.do(req)
}

// postHX は HTMX と同じく HX-Request ヘッダー付きでフォームを POST する。
func (c *client) postHX(path string, form url.Values) *response {
	c.t.Helper()
	return c.request(http.MethodPost, path, strings.NewReader(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"HX-Request":   "true",
	})
}

// getHX は HTMX と同じく HX-Request ヘッダー付きで GET する。
func (c *client) getHX(path string) *response {
	c.t.Helper()
	return c.request(http.MethodGet, path, nil, map[string]string{"HX-Request": "true"})
}

// comments は記事の内部コメントを古い順に「ユーザーID: 本文」で返す。
func (e *testEnv) comments(articleID string) []string {
	e.t.Helper()
	var rows []struct {
		UserID  string `db:"user_id"`
		Content string `db:"content"`
	}
	if err := e.db.Select(&rows, `SELECT user_id, content FROM article_comments WHERE article_id = ?
		ORDER BY julianday(created_at), rowid`, articleID); err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.UserID + ": " + r.Content
	}
	return out
}

// revisionComments は記事のリビジョンのメモを古い順に返す。
func (e *testEnv) revisionComments(articleID string) []string {
	e.t.Helper()
	var out []string
	if err := e.db.Select(&out, `SELECT comment FROM article_revisions WHERE article_id = ? ORDER BY revision_num`, articleID); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func futureLocal(d time.Duration) string {
	return time.Now().Add(d).In(jst).Format(datetimeLocalLayout)
}

func TestReviewWorkflowSubmitRejectApprove(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	ed := env.loggedIn(chief)
	id := writer.createArticle("校閲される記事", "初稿")

	// 執筆者の編集画面には「校閲を申請」があり、公開ボタンは無い。
	res := writer.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `formaction="/admin/articles/`+id+`/submit-review"`, "校閲を申請", `name="note"`)
	if strings.Contains(res.Body, `value="publish"`) {
		t.Error("writer sees a publish button")
	}

	// 編集画面から、入力中の内容を保存しつつ校閲を申請する。ロックは校閲者のために解放される。
	res = writer.postForm("/admin/articles/"+id+"/submit-review", url.Values{
		"title": {"校閲される記事（改稿）"}, "category_id": {"1"}, "content_markdown": {"第二稿"},
		"action": {"review"}, "note": {"数字の確認をお願いします"}, "draft_token": {"workflow-token"},
	})
	loc := expectRedirect(t, res, "/admin/articles?status=review&flash=submitted")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("saved_draft_token"); got != "workflow-token" {
		t.Fatalf("saved_draft_token = %q, want workflow-token (Location %q)", got, loc)
	}
	a := env.mustGetArticle(id)
	if a.Status != article.StatusReview || a.Title != "校閲される記事（改稿）" || a.ContentMarkdown != "第二稿" || a.LockedBy != nil {
		t.Fatalf("after submit: %+v", a)
	}
	if got := env.comments(id); len(got) != 1 || got[0] != studentA.ID+": 【校閲申請】数字の確認をお願いします" {
		t.Fatalf("comments = %q", got)
	}
	if got := env.revisionComments(id); len(got) != 2 || got[1] != "校閲申請" {
		t.Fatalf("revisions = %q", got)
	}
	res = ed.get("/admin/articles?status=review")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "校閲される記事（改稿）", "校閲待ち", "status-review")

	// 執筆者は差し戻し・承認ができず、校閲待ちの記事を再申請することもできない。
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/reject-review", url.Values{"reason": {"x"}}), http.StatusForbidden)
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/approve-publish", nil), http.StatusForbidden)
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/submit-review", nil), http.StatusConflict)

	// 編集者の編集画面には承認・差し戻しのパネルが出る。
	res = ed.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="review-panel"`, `action="/admin/articles/`+id+`/approve-publish"`,
		`action="/admin/articles/`+id+`/reject-review"`, `name="publish_at"`, `name="reason"`)
	expectStatus(t, ed.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	// 他のユーザー（執筆者など）が編集中の場合は差し戻し・承認が 409 Conflict で弾かれる。
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/lock", "", nil), http.StatusOK)
	expectStatus(t, ed.postForm("/admin/articles/"+id+"/reject-review", url.Values{"reason": {"競合テスト"}}), http.StatusConflict)
	expectStatus(t, ed.postForm("/admin/articles/"+id+"/approve-publish", nil), http.StatusConflict)
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	// 差し戻しには理由が必須。理由は申し送りメモに自動で残る。
	res = ed.postForm("/admin/articles/"+id+"/reject-review", url.Values{"reason": {"  "}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "差し戻しの理由を入力してください")
	res = ed.postForm("/admin/articles/"+id+"/reject-review", url.Values{"reason": {"出典を追記してください"}})
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=rejected")
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft {
		t.Fatalf("status after reject = %s", a.Status)
	}
	if got := env.comments(id); len(got) != 2 || got[1] != chief.ID+": 【差し戻し】出典を追記してください" {
		t.Fatalf("comments = %q", got)
	}
	// 校閲待ちでない記事の差し戻し・承認は 409（二重操作の防止）。
	expectStatus(t, ed.postForm("/admin/articles/"+id+"/reject-review", url.Values{"reason": {"再度"}}), http.StatusConflict)
	expectStatus(t, ed.postForm("/admin/articles/"+id+"/approve-publish", nil), http.StatusConflict)
	expectStatus(t, ed.postForm("/admin/articles/nonexistent/approve-publish", nil), http.StatusNotFound)

	// 編集者は編集画面を離れる。執筆者がフォームなしで（API として）再申請する。
	expectStatus(t, ed.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	res = writer.postAPI("/admin/articles/"+id+"/submit-review", "", nil)
	expectStatus(t, res, http.StatusOK)
	if a := env.mustGetArticle(id); a.Status != article.StatusReview || a.ContentMarkdown != "第二稿" {
		t.Fatalf("after resubmit: %+v", a)
	}

	// 予約日時は未来であること。
	res = ed.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"publish_at": {futureLocal(-time.Hour)}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "現在より後の日時")
	expectStatus(t, ed.postAPI("/admin/articles/"+id+"/approve-publish", "application/x-www-form-urlencoded",
		strings.NewReader("publish_at=tomorrow")), http.StatusUnprocessableEntity)

	// 即時公開で承認する。公開サイトに表示され、承認の記録が残る。
	res = ed.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"comment": {"よく書けています"}})
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=approved")
	a = env.mustGetArticle(id)
	if a.Status != article.StatusPublished || a.PublishedAt == nil || !article.IsVisible(a, time.Now()) {
		t.Fatalf("after approve: %+v", a)
	}
	got := env.comments(id)
	if last := got[len(got)-1]; last != chief.ID+": 【承認】公開しました。\nよく書けています" {
		t.Fatalf("last comment = %q", last)
	}
	expectStatus(t, env.newClient().get("/articles/"+a.Slug), http.StatusOK)
	if revs := env.revisionComments(id); revs[len(revs)-1] == "承認・公開" {
		// 内容が承認前の版と同じため、新しい版は作らない（重複保存のスキップ）。
		t.Fatalf("duplicate revision recorded on approve: %q", revs)
	}
}

func TestSubmitReviewFromNewArticle(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	res := writer.postForm("/admin/articles/new", url.Values{
		"title": {"新規の申請"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"review"},
	})
	expectRedirect(t, res, "/admin/articles?status=review&flash=submitted")
	var id string
	if err := env.db.Get(&id, `SELECT id FROM articles WHERE title = ?`, "新規の申請"); err != nil {
		t.Fatal(err)
	}
	if a := env.mustGetArticle(id); a.Status != article.StatusReview {
		t.Fatalf("status = %s", a.Status)
	}
	if got := env.comments(id); len(got) != 1 || got[0] != studentA.ID+": 【校閲申請】校閲をお願いします。" {
		t.Fatalf("comments = %q", got)
	}
	expectContains(t, writer.get("/admin/articles?status=review&flash=submitted").Body, "新規の申請", "校閲を申請しました")
}

func TestWriterEditsDuringReview(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("申請中の記事", "初稿")
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/submit-review", nil), "/admin/articles?status=review")

	// 校閲待ちの画面では「保存（校閲待ちのまま）」と「申請を取り下げる」を出す。
	res := writer.get("/admin/articles/" + id + "/edit")
	expectContains(t, res.Body, "保存（校閲待ちのまま）", "申請を取り下げる", "編集長・管理者の確認をお待ちください")
	if strings.Contains(res.Body, `id="approve-form"`) {
		t.Error("writer sees approve form")
	}

	form := url.Values{"title": {"申請中の記事"}, "category_id": {"1"}, "content_markdown": {"誤字を修正"}, "action": {"save"}}
	expectRedirect(t, writer.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=saved")
	if a := env.mustGetArticle(id); a.Status != article.StatusReview || a.ContentMarkdown != "誤字を修正" {
		t.Fatalf("after save during review: %+v", a)
	}
	form.Set("action", "draft")
	expectRedirect(t, writer.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft {
		t.Fatalf("status after withdraw = %s", a.Status)
	}
}

func TestWriterCannotChangePublishedArticle(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	writer := env.loggedIn(studentA)
	id := ed.createArticle("公開記事", "本文")
	form := url.Values{"title": {"公開記事"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"publish"}}
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=published")
	expectStatus(t, ed.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	// 執筆者の画面では保存ボタンが無効になり、案内が出る。
	res := writer.get("/admin/articles/" + id + "/edit")
	expectContains(t, res.Body, `id="publisher-only-banner"`)
	if !buttonDisabled(t, res.Body, saveButtonPattern) {
		t.Error("save button is enabled for writer on a published article")
	}
	for _, action := range []string{"draft", "save", "publish", "review", ""} {
		f := url.Values{"title": {"書き換え"}, "category_id": {"1"}, "content_markdown": {"改ざん"}, "action": {action}}
		expectStatus(t, writer.postForm("/admin/articles/"+id, f), http.StatusForbidden)
	}
	if a := env.mustGetArticle(id); a.Status != article.StatusPublished || a.ContentMarkdown != "本文" {
		t.Fatalf("published article modified by writer: %+v", a)
	}
}

func TestEditorUnpublishAndDirectPublish(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	id := ed.createArticle("編集者の記事", "本文")
	form := url.Values{"title": {"編集者の記事"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"publish"}}
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=published")
	expectContains(t, ed.get("/admin/articles/"+id+"/edit").Body, "下書きに戻す", "更新して公開")
	form.Set("action", "draft")
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=saved")
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft {
		t.Fatalf("status after unpublish = %s", a.Status)
	}
}

func TestApproveWithScheduleIsPublishedByScheduler(t *testing.T) {
	env := newTestEnvWith(t, Config{SchedulerInterval: 20 * time.Millisecond}, nil)
	writer := env.loggedIn(studentA)
	ed := env.loggedIn(chief)
	reader := env.newClient()
	id := writer.createArticle("予約される記事", "合同説明会のお知らせ")
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/submit-review", nil), "/admin/articles?status=review")

	when := futureLocal(2 * time.Hour)
	res := ed.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"publish_at": {when}})
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=scheduled")
	// 予約投稿は、状態が公開で公開日時が予約した日時の記事として保存する。
	a := env.mustGetArticle(id)
	if a.Status != article.StatusPublished || a.PublishedAt == nil || !article.IsScheduled(a, time.Now()) {
		t.Fatalf("after scheduled approve: %+v", a)
	}
	if got := a.PublishedAt.In(jst).Format(datetimeLocalLayout); got != when {
		t.Fatalf("published_at = %s, want %s (JST)", got, when)
	}
	expectContains(t, ed.get("/admin/articles?status=scheduled").Body, "予約される記事", "公開予定", "予約投稿")
	expectContains(t, ed.get("/admin/articles/"+id+"/edit").Body, `id="schedule-banner"`, "保存（予約を維持）", "今すぐ公開")
	expectStatus(t, reader.get("/articles/"+a.Slug), http.StatusNotFound)

	// 予約日時が今来たことにすると、読者に見え始め、スケジューラーが知らせて検索にも出す。
	env.arriveScheduled(id)
	deadline := time.Now().Add(5 * time.Second)
	for !article.IsVisible(env.mustGetArticle(id), time.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("article did not become visible")
		}
		time.Sleep(20 * time.Millisecond)
	}
	expectStatus(t, reader.get("/articles/"+a.Slug), http.StatusOK)
	for time.Now().Before(deadline) && !strings.Contains(reader.get("/search?q="+url.QueryEscape("合同説明会")).Body, "予約される記事") {
		time.Sleep(20 * time.Millisecond)
	}
	expectContains(t, reader.get("/search?q="+url.QueryEscape("合同説明会")).Body, "予約される記事")
}

// arriveScheduled は予約投稿 id の公開日時を今に移し、予約日時が来たことにする。承認したのは予約日時より前のため、
// 更新日時（承認した時刻）も予約日時より前に置く。
//
// 動いているスケジューラーは、確かめるたびに「どこまで確かめたか」の時刻を自分の現在時刻へ進め、それより前の公開日時を
// 二度と探さない。移す先の「今」を書き込みのロックの外で決めると、その間にスケジューラーが確かめ終え、確かめた時刻が
// 移す先の公開日時を追い越すことがある（記事は読者に見えるが、知らされず検索にも出ない）。そこで、スケジューラーと同じ
// BEGIN IMMEDIATE のロックを取ってから「今」を決める。保存済みの確かめた時刻はどれもこの「今」より前になり、このあとに
// 確かめるスケジューラーは公開日時を迎えた記事として必ず見つける。
func (e *testEnv) arriveScheduled(id string) {
	e.t.Helper()
	ctx := context.Background()
	conn, err := e.db.Connx(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := conn.ExecContext(ctx, `UPDATE articles SET published_at = ?, updated_at = ? WHERE id = ?`,
		now, now.Add(-time.Minute), id); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		e.t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		e.t.Fatal(err)
	}
}

func TestKeepScheduleWhileEditing(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	id := ed.createArticle("予約記事", "本文")
	expectRedirect(t, ed.postForm("/admin/articles/"+id+"/submit-review", nil), "/admin/articles?status=review")
	expectRedirect(t, ed.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"publish_at": {futureLocal(24 * time.Hour)}}), "/admin/articles/")
	scheduled := env.mustGetArticle(id).PublishedAt

	form := url.Values{"title": {"予約記事"}, "category_id": {"1"}, "content_markdown": {"修正した本文"}, "action": {"save"}}
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	a := env.mustGetArticle(id)
	if !article.IsScheduled(a, time.Now()) || !a.PublishedAt.Equal(*scheduled) || a.ContentMarkdown != "修正した本文" {
		t.Fatalf("after save keeping schedule: %+v", a)
	}
	// 下書きに戻すと予約は取り消される。
	form.Set("action", "draft")
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	if a := env.mustGetArticle(id); a.Status != article.StatusDraft || a.PublishedAt != nil {
		t.Fatalf("after draft: status=%s published_at=%v", a.Status, a.PublishedAt)
	}
}

// 同じミリ秒に書かれたコメントも、書いた順に並ぶ（julianday はミリ秒単位で、ID はランダム）。
func TestCommentsSameInstantKeepInsertOrder(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("同時のコメント", "本文")
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct{ id, content string }{{"ffff", "先に書いた"}, {"0000", "後に書いた"}} {
		if _, err := env.db.Exec(`INSERT INTO article_comments (id, article_id, user_id, content, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.id, id, studentA.ID, c.content, at); err != nil {
			t.Fatal(err)
		}
	}
	body := writer.getHX("/admin/articles/" + id + "/comments").Body
	if first, second := strings.Index(body, "先に書いた"), strings.Index(body, "後に書いた"); first < 0 || second < 0 || first > second {
		t.Fatalf("thread order: 先=%d 後=%d", first, second)
	}
}

func TestComments(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	ed := env.loggedIn(chief)
	id := writer.createArticle("コメントする記事", "本文")

	res := writer.getHX("/admin/articles/" + id + "/comments")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "まだメモはありません")
	if strings.Contains(res.Body, "<html") {
		t.Fatal("comment list is not a partial")
	}

	// HTMX からの投稿はスレッドのパーシャルを返す。本文はエスケープする。
	res = ed.postHX("/admin/articles/"+id+"/comments", url.Values{"content": {"見出しを短く<script>alert(1)</script>\n2行目"}})
	expectStatus(t, res, http.StatusCreated)
	expectContains(t, res.Body, "佐藤デスク", "編集長", "見出しを短く&lt;script&gt;", "2行目")
	if strings.Contains(res.Body, "<script>alert") {
		t.Fatal("comment is not escaped")
	}
	res = writer.postHX("/admin/articles/"+id+"/comments", url.Values{"content": {"修正しました"}})
	expectStatus(t, res, http.StatusCreated)
	expectContains(t, res.Body, "修正しました", "is-mine")
	if got := env.comments(id); len(got) != 2 || !strings.HasPrefix(got[0], chief.ID+": 見出しを短く") || got[1] != studentA.ID+": 修正しました" {
		t.Fatalf("comments = %q", got)
	}

	// 通常のフォーム送信は編集画面のスレッドへ戻る。編集画面にはスレッドが表示される。
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/comments", url.Values{"content": {"フォームから"}}),
		"/admin/articles/"+id+"/edit#comments")
	expectContains(t, writer.get("/admin/articles/"+id+"/edit").Body, `id="comment-form"`, "修正しました", "フォームから")

	expectStatus(t, writer.postHX("/admin/articles/"+id+"/comments", url.Values{"content": {" \n "}}), http.StatusUnprocessableEntity)
	expectStatus(t, writer.postHX("/admin/articles/"+id+"/comments", url.Values{"content": {strings.Repeat("長", maxCommentRunes+1)}}), http.StatusUnprocessableEntity)
	expectStatus(t, writer.postHX("/admin/articles/nonexistent/comments", url.Values{"content": {"x"}}), http.StatusNotFound)
	expectStatus(t, writer.getHX("/admin/articles/nonexistent/comments"), http.StatusNotFound)
	expectStatus(t, writer.postRaw("/admin/articles/"+id+"/comments", url.Values{"content": {"CSRFなし"}}), http.StatusForbidden)
	if res := env.newClient().getHX("/admin/articles/" + id + "/comments"); res.StatusCode == http.StatusOK {
		t.Fatal("anonymous user can read comments")
	}

	// 内部コメントは公開サイトに出さない。
	form := url.Values{"title": {"コメントする記事"}, "slug": {"commented"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"publish"}}
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=published")
	res = env.newClient().get("/articles/commented")
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "修正しました") || strings.Contains(res.Body, "見出しを短く") {
		t.Fatal("internal comments leaked to the public article page")
	}
}

func TestWorkflowAsyncResponses(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	ed := env.loggedIn(chief)
	id := writer.createArticle("非同期", "本文")
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/submit-review", "", nil), http.StatusOK)

	res := ed.postAPI("/admin/articles/"+id+"/reject-review", "application/x-www-form-urlencoded", strings.NewReader("reason="))
	expectStatus(t, res, http.StatusUnprocessableEntity)
	var msg struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Body), &msg); err != nil || msg.Message != "差し戻しの理由を入力してください" {
		t.Fatalf("error body = %s (%v)", res.Body, err)
	}
	res = ed.postAPI("/admin/articles/"+id+"/approve-publish", "application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"publish_at": {futureLocal(time.Hour)}}.Encode()))
	expectStatus(t, res, http.StatusOK)
	if err := json.Unmarshal([]byte(res.Body), &msg); err != nil || msg.Status != string(article.StatusScheduled) {
		t.Fatalf("approve body = %s (%v)", res.Body, err)
	}
}

// 「承認して公開」で今すぐ公開したときも、記事公開時の自動反映の設定に従って反映し、その旨を知らせる（フォームの送信は
// 通知のキー、fetch には文言）。日時を決めて承認した予約投稿では、自動の反映を予約しない。
func TestApprovePublishAutoReflects(t *testing.T) {
	env := newTestEnv(t)
	const debounce = 100 * time.Millisecond
	out := configureFolderReflect(t, env, debounce)
	writer := env.loggedIn(studentA)
	ed := env.loggedIn(chief)
	submit := func(title, slug string) string {
		t.Helper()
		id := writer.createArticle(title, "本文")
		if _, err := env.db.Exec(`UPDATE articles SET slug = ? WHERE id = ?`, slug, id); err != nil {
			t.Fatal(err)
		}
		expectStatus(t, writer.postAPI("/admin/articles/"+id+"/submit-review", "", nil), http.StatusOK)
		return id
	}

	id := submit("承認して公開", "approved-now")
	expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/"+id+"/approve-publish", nil), "/admin/articles/"+id+"/edit"),
		flashPublishedReflecting)
	if got := waitReflectDone(t, env.server); !got.OK || got.Trigger != publishTriggerArticlePublish {
		t.Fatalf("reflect after approving = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(out, "articles", "approved-now", "index.html")); err != nil {
		t.Errorf("approved article not reflected: %v", err)
	}

	// fetch で承認したときは、応答の文言で知らせる。
	id = submit("一覧から承認", "approved-async")
	res := ed.postAPI("/admin/articles/"+id+"/approve-publish", "application/x-www-form-urlencoded", nil)
	expectStatus(t, res, http.StatusOK)
	var msg struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Body), &msg); err != nil || msg.Status != string(article.StatusPublished) ||
		msg.Message != "記事を公開しました。静的サイトへの反映を開始しました" {
		t.Fatalf("approve body = %s (%v)", res.Body, err)
	}
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("reflect after approving with fetch = %+v", got)
	}

	// 予約投稿にした承認では、予約時刻が来るまで反映しない。
	id = submit("承認して予約", "approved-later")
	expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/"+id+"/approve-publish", url.Values{"publish_at": {futureLocal(2 * time.Hour)}}),
		"/admin/articles/"+id+"/edit"), "scheduled")
	time.Sleep(3 * debounce)
	if reflects, pending := reflectState(env.server); reflects != 2 || pending {
		t.Errorf("after approving a schedule: reflects = %d (timer pending %v), want no automatic reflect", reflects, pending)
	}

	// 自動反映を切っていれば、承認しても反映せず、「サイトに反映」を押すよう知らせる。
	if err := upsertSetting(env.db, publishSettingAutoReflect, "0"); err != nil {
		t.Fatal(err)
	}
	id = submit("自動で送らない承認", "approved-manual")
	expectFlash(t, expectRedirect(t, ed.postForm("/admin/articles/"+id+"/approve-publish", nil), "/admin/articles/"+id+"/edit"),
		flashPublishedManual)
	time.Sleep(3 * debounce)
	if reflects, pending := reflectState(env.server); reflects != 2 || pending {
		t.Errorf("with the automatic reflect off: reflects = %d (timer pending %v), want no reflect", reflects, pending)
	}
}
