package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

func TestRevisionHistoryDiffAndRestore(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("リビジョンの記事", "1行目\n2行目")
	save := func(title, body string) {
		t.Helper()
		form := url.Values{"title": {title}, "category_id": {"1"}, "content_markdown": {body}, "action": {"draft"}}
		expectRedirect(t, writer.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	}
	save("リビジョンの記事", "1行目\n2行目を<b>修正</b>\n3行目")
	save("リビジョンの記事", "1行目\n2行目を<b>修正</b>\n3行目") // 内容が同じなら版を増やさない。
	save("改題した記事", "1行目\n2行目を<b>修正</b>\n3行目")
	if got := env.revisionComments(id); len(got) != 3 || got[0] != "作成" || got[1] != "下書き保存" {
		t.Fatalf("revisions = %q", got)
	}

	// 一覧は新しい順で、現在の版・作成者・文字数の増減を示す。
	res := writer.getHX("/admin/articles/" + id + "/revisions")
	expectStatus(t, res, http.StatusOK)
	// "+" は html/template が &#43; にエスケープする。
	expectContains(t, res.Body, "第3版", "第2版", "第1版", "現在", "山田", "作成", "&#43;14")
	if strings.Index(res.Body, "第3版") > strings.Index(res.Body, "第1版") {
		t.Error("revisions are not in newest-first order")
	}
	expectContains(t, res.Body, `data-revision-diff="/admin/articles/`+id+`/revisions/1/diff"`)

	// 第1版から現在への差分。本文の HTML はエスケープし、<ins>/<del> だけを出す。
	res = writer.getHX("/admin/articles/" + id + "/revisions/1/diff")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "第1版 → 現在の内容", `<ins class="diff-add">`, "&lt;b&gt;修正&lt;/b&gt;",
		`<del class="diff-del">リビジョンの</del>`, `<ins class="diff-add">改題した</ins>`,
		`action="/admin/articles/`+id+`/revisions/1/restore"`, "このバージョンに復元")
	if strings.Contains(res.Body, "<b>修正") {
		t.Fatal("diff is not escaped")
	}
	res = writer.getHX("/admin/articles/" + id + "/revisions/3/diff")
	expectContains(t, res.Body, "この版は現在の内容と同じです")
	if strings.Contains(res.Body, "このバージョンに復元") {
		t.Error("restore offered for the current revision")
	}
	for _, p := range []string{"/revisions/99/diff", "/revisions/0/diff", "/revisions/abc/diff"} {
		expectStatus(t, writer.getHX("/admin/articles/"+id+p), http.StatusNotFound)
	}
	expectStatus(t, writer.getHX("/admin/articles/nonexistent/revisions"), http.StatusNotFound)

	// 他のユーザーが編集中なら復元できず、差分画面にも復元ボタンを出さない。
	other := env.loggedIn(studentB)
	expectStatus(t, writer.get("/admin/articles/"+id+"/edit"), http.StatusOK)
	expectStatus(t, other.postForm("/admin/articles/"+id+"/revisions/1/restore", nil), http.StatusConflict)
	if strings.Contains(other.getHX("/admin/articles/"+id+"/revisions/1/diff").Body, "このバージョンに復元") {
		t.Error("restore offered while another user holds the lock")
	}

	// 第1版に復元する。状態は変えず、復元の記録を版として残す。
	res = writer.postForm("/admin/articles/"+id+"/revisions/1/restore", nil)
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=restored")
	a := env.mustGetArticle(id)
	if a.Title != "リビジョンの記事" || a.ContentMarkdown != "1行目\n2行目" || a.Status != article.StatusDraft {
		t.Fatalf("after restore: %+v", a)
	}
	if got := env.revisionComments(id); len(got) != 4 || got[3] != "第1版から復元" {
		t.Fatalf("revisions after restore = %q", got)
	}
	expectContains(t, writer.get("/admin/articles/"+id+"/edit?flash=restored").Body, "過去のバージョンに復元しました")

	// 復元前の内容（第3版）にも戻せる。
	res = writer.postAPI("/admin/articles/"+id+"/revisions/3/restore", "", nil)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"revision_num":3`)
	if a := env.mustGetArticle(id); a.Title != "改題した記事" {
		t.Fatalf("restore back to latest: title=%q", a.Title)
	}
	expectStatus(t, writer.postForm("/admin/articles/"+id+"/revisions/42/restore", nil), http.StatusNotFound)
	expectStatus(t, writer.postForm("/admin/articles/nonexistent/revisions/1/restore", nil), http.StatusNotFound)
}

func TestRevisionRestoreRequiresPublisherForPublishedArticle(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	writer := env.loggedIn(studentA)
	id := ed.createArticle("公開済み", "旧本文")
	form := url.Values{"title": {"公開済み"}, "category_id": {"1"}, "content_markdown": {"新本文"}, "action": {"publish"}}
	expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=published")
	expectStatus(t, ed.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	expectStatus(t, writer.postForm("/admin/articles/"+id+"/revisions/1/restore", nil), http.StatusForbidden)
	if a := env.mustGetArticle(id); a.ContentMarkdown != "新本文" || a.LockedBy != nil {
		t.Fatalf("after forbidden restore: content=%q locked_by=%v", a.ContentMarkdown, a.LockedBy)
	}
	if strings.Contains(writer.getHX("/admin/articles/"+id+"/revisions/1/diff").Body, "このバージョンに復元") {
		t.Error("writer is offered restore on a published article")
	}

	expectRedirect(t, ed.postForm("/admin/articles/"+id+"/revisions/1/restore", nil), "/admin/articles/"+id+"/edit?flash=restored")
	a := env.mustGetArticle(id)
	if a.ContentMarkdown != "旧本文" || a.Status != article.StatusPublished {
		t.Fatalf("after restore by editor: %+v", a)
	}
	// 公開中の記事の復元は公開ページにも反映される。
	expectContains(t, env.newClient().get("/articles/"+a.Slug).Body, "旧本文")
}

func TestRevisionRestoreKeepsUnsnapshottedContent(t *testing.T) {
	// Phase 6 より前に作られた記事（リビジョンが無い）でも、復元前の内容を版として残す。
	var id string
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		id = seedArticle(t, db, article.Article{Title: "旧記事", ContentMarkdown: "移行前の本文"})
	})
	writer := env.loggedIn(studentA)
	expectContains(t, writer.getHX("/admin/articles/"+id+"/revisions").Body, "まだ履歴はありません")

	form := url.Values{"title": {"旧記事"}, "category_id": {"1"}, "content_markdown": {"書き換えた本文"}, "action": {"draft"}}
	expectRedirect(t, writer.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	expectRedirect(t, writer.postForm("/admin/articles/"+id+"/revisions/1/restore", nil), "/admin/articles/"+id+"/edit?flash=restored")
	if got := env.revisionComments(id); len(got) != 1 {
		// 第1版（書き換え後）からの復元は内容が同じため、新しい版を作らない。
		t.Fatalf("revisions = %q", got)
	}
}
