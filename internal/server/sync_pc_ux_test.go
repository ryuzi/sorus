package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/mcp"
)

// newPCUXEnv は「届いた原稿」の画面も描けるローカル CMS と、公開サーバーの組を起動し、sub のトークンで接続する。
func newPCUXEnv(t *testing.T, sub string, role auth.Role) *remoteEnv {
	t.Helper()
	e := &remoteEnv{local: newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil),
		remote: newSyncEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)}
	e.configure(sub, role)
	return e
}

// bumpRemote は公開サーバーの記事 id を、ほかの人が公開サーバーで書き換えたことにする（更新日時を1時間進める）。
func bumpRemote(t *testing.T, e *remoteEnv, id, title, content string) time.Time {
	t.Helper()
	at := e.remote.getArticle(id).UpdatedAt.Add(time.Hour).UTC()
	if _, err := e.remote.db.Exec(`UPDATE articles SET title = ?, content_markdown = ?, updated_at = ? WHERE id = ?`,
		title, content, at, id); err != nil {
		t.Fatal(err)
	}
	return e.remote.getArticle(id).UpdatedAt
}

func sameInstant(a *time.Time, b time.Time) bool {
	return a != nil && !syncUpdatedAtDiffers(*a, b)
}

// 公開サーバーの作成中の下書きは、この PC に下書きのまま取り込み、公開サーバーに「編集中」の印を付け、
// 取り込んだ版の更新日時を残す。ほかの人が編集中なら 409（引き継ぐ案内）で、force=1 なら引き継ぐ。
func TestRemoteImportDraftChecksOut(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	ed := e.local.loggedIn(chief)
	id := seedArticle(t, e.remote.db, article.Article{Title: "書きかけ", Slug: "wip", ContentMarkdown: "途中まで",
		Status: article.StatusDraft, AuthorID: studentA.ID})
	remoteUpdated := e.remote.getArticle(id).UpdatedAt

	// ほかの人が PC で編集中なら、何も取り込まずに 409 で引き継ぐかを尋ねる。
	if err := e.remote.server.articles.SetCheckout(t.Context(), id, "鈴木", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	res := ed.postJSON("/admin/remote/pending/"+id+"/import", nil, nil)
	expectStatus(t, res, http.StatusConflict)
	var co struct {
		Code         string `json:"code"`
		Message      string `json:"message"`
		CheckedOutBy string `json:"checked_out_by"`
		NeedForce    bool   `json:"need_force"`
	}
	if err := json.Unmarshal([]byte(res.Body), &co); err != nil {
		t.Fatal(err)
	}
	if co.Code != SyncCheckedOutCode || co.CheckedOutBy != "鈴木" || !co.NeedForce || !strings.Contains(co.Message, "鈴木さん") {
		t.Errorf("checked out response = %+v", co)
	}
	if n := e.local.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, id); n != 0 {
		t.Fatal("import created a local article despite another user's checkout")
	}

	// 引き継ぐ（force=1）: 下書きのまま取り込み、印は自分のものになる。
	var imported struct {
		Status  article.Status `json:"status"`
		EditURL string         `json:"edit_url"`
	}
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/import", url.Values{"force": {"1"}}, &imported), http.StatusOK)
	if imported.Status != article.StatusDraft || imported.EditURL != "/admin/articles/"+id+"/edit" {
		t.Errorf("import = %+v", imported)
	}
	la := e.local.mustGetArticle(id)
	if la.Status != article.StatusDraft || la.Title != "書きかけ" || !sameInstant(la.RemoteUpdatedAt, remoteUpdated) {
		t.Errorf("local draft = status %s title %q remote_updated_at %v (want %v)", la.Status, la.Title, la.RemoteUpdatedAt, remoteUpdated)
	}
	if ra := e.remote.getArticle(id); ra.CheckedOutBy != "佐藤" || !ra.IsCheckedOut(time.Now()) || ra.Status != article.StatusDraft {
		t.Errorf("remote after import: checked_out_by %q status %s", ra.CheckedOutBy, ra.Status)
	}
}

// 提出は取り込んだ版の更新日時を base_updated_at に付ける。公開サーバーがスラッグを付け直したらこの PC も合わせ、
// その後に公開サーバーで変わっていれば差分付きの 409 を返す。差分を見て上書きを選ぶ（base_updated_at に公開サーバーの
// 今の更新日時を付ける）と提出できる。
func TestRemoteSubmitSlugAdoptionAndConflict(t *testing.T) {
	e := newPCUXEnv(t, "yamada", auth.RoleWriter)
	seedArticle(t, e.remote.db, article.Article{Title: "先にある記事", Slug: "exhibit", ContentMarkdown: "x", Status: article.StatusPublished})
	c := e.local.loggedIn(studentA)
	id := c.createArticle("展示会", "")
	la := e.local.mustGetArticle(id)
	la.Slug, la.ContentMarkdown = "exhibit", "一行目\n二行目\n三行目"
	if err := e.local.server.articles.Save(la); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Article     SyncArticleRef `json:"article"`
		SlugAdopted bool           `json:"slug_adopted"`
	}
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, &out), http.StatusOK)
	if out.Article.Slug == "exhibit" || !out.SlugAdopted {
		t.Fatalf("submit = %+v (server should have renamed the slug)", out)
	}
	la = e.local.mustGetArticle(id)
	if la.Slug != out.Article.Slug || !sameInstant(la.RemoteUpdatedAt, out.Article.UpdatedAt) {
		t.Errorf("local after submit: slug %q remote_updated_at %v, want %q %v", la.Slug, la.RemoteUpdatedAt, out.Article.Slug, out.Article.UpdatedAt)
	}

	// 続けて提出しても（公開サーバーは変わっていない）食い違いにならない。
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusOK)

	// ほかの人が公開サーバーで書き換えた後の提出は、保存せずに差分付きの 409 を返す。
	serverAt := bumpRemote(t, e, id, "サーバーで直した題", "一行目\nサーバーで直した二行目\n三行目")
	la = e.local.mustGetArticle(id)
	la.ContentMarkdown = "一行目\nPC で直した二行目\n三行目"
	if err := e.local.server.articles.Save(la); err != nil {
		t.Fatal(err)
	}
	res := c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil)
	expectStatus(t, res, http.StatusConflict)
	var cv remoteConflictView
	if err := json.Unmarshal([]byte(res.Body), &cv); err != nil {
		t.Fatal(err)
	}
	if cv.Code != SyncConflictCode || cv.ServerTitle != "サーバーで直した題" || !strings.Contains(cv.ServerContentMarkdown, "サーバーで直した二行目") ||
		syncUpdatedAtDiffers(cv.ServerUpdatedAt, serverAt) || cv.ServerAuthorName == "" || !cv.Changed ||
		cv.TitleRow != (diffRow{Op: "change", Left: "展示会", Right: "サーバーで直した題"}) {
		t.Errorf("conflict = %+v", cv)
	}
	wantRow := diffRow{Op: "change", Left: "PC で直した二行目", Right: "サーバーで直した二行目"}
	if len(cv.Rows) != 3 || cv.Rows[1] != wantRow {
		t.Errorf("conflict rows = %+v", cv.Rows)
	}
	if got := e.remote.getArticle(id).ContentMarkdown; !strings.Contains(got, "サーバーで直した二行目") {
		t.Fatalf("stale submit overwrote the server: %q", got)
	}

	// 形式の正しくない base_updated_at は 400。
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", url.Values{"base_updated_at": {"yesterday"}}, nil), http.StatusBadRequest)

	// 「この PC の内容で上書き提出」: 公開サーバーの今の更新日時を base_updated_at に付けて送り直す。
	base := cv.ServerUpdatedAt.Format(time.RFC3339Nano)
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", url.Values{"base_updated_at": {base}}, &out), http.StatusOK)
	if got := e.remote.getArticle(id).ContentMarkdown; !strings.Contains(got, "PC で直した二行目") {
		t.Errorf("overwrite submit did not save: %q", got)
	}
	if la := e.local.mustGetArticle(id); !sameInstant(la.RemoteUpdatedAt, out.Article.UpdatedAt) {
		t.Errorf("remote_updated_at after overwrite = %v, want %v", la.RemoteUpdatedAt, out.Article.UpdatedAt)
	}
}

// 承認（公開）も、取り込んだ後に公開サーバーで変わっていれば差分付きの 409 を返す。「サーバー版を取り込む」
// （pull-latest）で公開サーバーの今の内容をこの PC に取り込むと、そのまま承認できる。
func TestRemotePublishConflictAndPullLatest(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	ed := e.local.loggedIn(chief)
	id := e.remote.seedReview("届いた記事", "arrived")
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/import", nil, nil), http.StatusOK)

	serverAt := bumpRemote(t, e, id, "書き手が直した題", "書き手が直した本文")
	res := ed.postJSON("/admin/remote/pending/"+id+"/publish", nil, nil)
	expectStatus(t, res, http.StatusConflict)
	var cv remoteConflictView
	if err := json.Unmarshal([]byte(res.Body), &cv); err != nil {
		t.Fatal(err)
	}
	if cv.Code != SyncConflictCode || cv.LocalTitle != "届いた記事" || cv.ServerTitle != "書き手が直した題" ||
		len(cv.Rows) != 1 || cv.Rows[0].Op != "change" {
		t.Errorf("publish conflict = %+v", cv)
	}
	if e.remote.getArticle(id).Status != article.StatusReview {
		t.Fatal("stale approval published the article")
	}

	// サーバー版を取り込む: 確認なしで上書きし、取り込んだ版の更新日時を残す。
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/pull-latest", nil, nil), http.StatusOK)
	la := e.local.mustGetArticle(id)
	if la.Title != "書き手が直した題" || la.ContentMarkdown != "書き手が直した本文" || !sameInstant(la.RemoteUpdatedAt, serverAt) {
		t.Errorf("after pull-latest: %q %q %v", la.Title, la.ContentMarkdown, la.RemoteUpdatedAt)
	}
	var pub struct {
		Article SyncArticleRef `json:"article"`
	}
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/publish", nil, &pub), http.StatusOK)
	if pub.Article.Status != article.StatusPublished {
		t.Errorf("publish = %+v", pub)
	}
	if la := e.local.mustGetArticle(id); !sameInstant(la.RemoteUpdatedAt, pub.Article.UpdatedAt) {
		t.Errorf("remote_updated_at after publish = %v, want %v", la.RemoteUpdatedAt, pub.Article.UpdatedAt)
	}
}

// 予約も同じく、取り込んだ後に公開サーバーで変わっていれば 409 にし、上書きを選べば予約できる。
func TestRemoteScheduleConflict(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	ed := e.local.loggedIn(chief)
	id := e.remote.seedReview("予約する記事", "to-schedule")
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/import", nil, nil), http.StatusOK)
	serverAt := bumpRemote(t, e, id, "直した題", "直した本文")

	at := time.Now().In(jst).Add(48 * time.Hour).Format("2006-01-02T15:04")
	res := ed.postJSON("/admin/remote/pending/"+id+"/schedule", url.Values{"publish_at": {at}}, nil)
	expectStatus(t, res, http.StatusConflict)
	if !strings.Contains(res.Body, `"code":"conflict"`) || !strings.Contains(res.Body, "直した本文") {
		t.Errorf("schedule conflict = %s", res.Body)
	}
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/schedule",
		url.Values{"publish_at": {at}, "base_updated_at": {serverAt.Format(time.RFC3339Nano)}}, nil), http.StatusOK)
	if ra := e.remote.getArticle(id); ra.Status != article.StatusPublished || ra.PublishedAt == nil {
		t.Errorf("remote after schedule: %s %v", ra.Status, ra.PublishedAt)
	}
}

// 変更案は「届いた原稿」で差分を見せ、破棄すると記事を変えずに案だけを消し、反映すると公開サーバーとこの PC の
// 記事の両方に写す（その後の提出は食い違いにならない）。
func TestRemoteResolveProposal(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	ed := e.local.loggedIn(chief)
	id := seedArticle(t, e.remote.db, article.Article{Title: "元の題", Slug: "orig", ContentMarkdown: "一\n二\n三",
		Status: article.StatusReview, AuthorID: studentA.ID})
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/import", nil, nil), http.StatusOK)
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/proposal", url.Values{"proposal_action": {"merge"}}, nil), http.StatusBadRequest)

	// 破棄: 記事は変わらず、案だけが消える。
	e.remote.seedProposal(id, "p1", mcp.ProposalChanges{Title: strPtr("AI の題")})
	var out struct {
		Action       string `json:"action"`
		LocalUpdated bool   `json:"local_updated"`
	}
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/proposal",
		url.Values{"proposal_action": {"discard"}, "proposal_id": {"p1"}}, &out), http.StatusOK)
	if out.Action != SyncProposalDiscard || out.LocalUpdated || e.remote.hasProposal(id) || e.remote.getArticle(id).Title != "元の題" {
		t.Errorf("discard = %+v, proposal left %v", out, e.remote.hasProposal(id))
	}

	// 新しい案に置き換わっていれば、確かめた案の ID では反映しない。
	e.remote.seedProposal(id, "p2", mcp.ProposalChanges{Title: strPtr("AI の題"), ContentMarkdown: strPtr("一\nAI の二\n三")})
	res := ed.postJSON("/admin/remote/pending/"+id+"/proposal", url.Values{"proposal_action": {"accept"}, "proposal_id": {"old"}}, nil)
	expectStatus(t, res, http.StatusBadGateway)
	if !strings.Contains(res.Body, `"remote_status":409`) {
		t.Errorf("stale proposal = %s", res.Body)
	}

	// 「届いた原稿」は変更案ありのピルと、差分・反映・破棄のボタンを出す。
	page := ed.get("/admin/manuscripts")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `data-pill="proposal"`, "変更案あり", `data-proposal-open="`+id+`"`, "差分を見て反映",
		`data-proposal-template="`+id+`"`, `data-diff-field="title"`, "AI の二", `data-proposal-resolve="accept"`,
		`data-proposal-resolve="discard"`, `data-proposal-id="p2"`)

	// 反映: 公開サーバーとこの PC の記事の両方に写す。
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/proposal",
		url.Values{"proposal_action": {"accept"}, "proposal_id": {"p2"}}, &out), http.StatusOK)
	ra := e.remote.getArticle(id)
	if !out.LocalUpdated || ra.Title != "AI の題" || ra.ContentMarkdown != "一\nAI の二\n三" || e.remote.hasProposal(id) {
		t.Errorf("accept = %+v, remote %q %q", out, ra.Title, ra.ContentMarkdown)
	}
	la := e.local.mustGetArticle(id)
	if la.Title != "AI の題" || la.ContentMarkdown != "一\nAI の二\n三" || !sameInstant(la.RemoteUpdatedAt, ra.UpdatedAt) {
		t.Errorf("local after accept = %q %q %v (remote %v)", la.Title, la.ContentMarkdown, la.RemoteUpdatedAt, ra.UpdatedAt)
	}
	expectNotContains(t, ed.get("/admin/manuscripts").Body, `data-pill="proposal"`)
	expectStatus(t, ed.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusOK)
}

// 「届いた原稿」は校閲待ちと作成中の下書きを分けて並べ、状態を短いピルで、操作を短いボタンで示す。
func TestManuscriptInboxDraftsAndPills(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	ed := e.local.loggedIn(chief)
	review := e.remote.seedReview("校閲してほしい記事", "please-review")
	draft := seedArticle(t, e.remote.db, article.Article{Title: "書きかけの下書き", Slug: "wip-draft", ContentMarkdown: "途中",
		Status: article.StatusDraft, AuthorID: studentA.ID})
	taken := seedArticle(t, e.remote.db, article.Article{Title: "鈴木さんの下書き", Slug: "suzuki-draft", ContentMarkdown: "途中",
		Status: article.StatusDraft, AuthorID: studentA.ID})
	if err := e.remote.server.articles.SetCheckout(t.Context(), taken, "鈴木", "", time.Now()); err != nil {
		t.Fatal(err)
	}

	res := ed.get("/admin/manuscripts")
	expectStatus(t, res, http.StatusOK)
	body := res.Body
	expectContains(t, body, "校閲を待っている記事（1件）", `id="manuscript-drafts"`, "作成中の下書き（2件）",
		`data-id="`+review+`" data-status="review"`, `data-id="`+draft+`" data-status="draft"`, `data-pill="draft"`,
		"鈴木さんが別PCで編集中", `data-pill="checked-out" data-checkout="elsewhere"`)
	// 校閲待ちは「受け取る」、下書きは「この PC で続きを書く」、ほかの人が編集中なら「引き継ぐ」。
	expectContains(t, itemHTML(t, body, review), `data-remote-inbox-import="`+review+`"`, ">受け取る<", "data-remote-inbox-publish=")
	d := itemHTML(t, body, draft)
	expectContains(t, d, `data-remote-inbox-import="`+draft+`" data-draft`, "この PC で続きを書く")
	expectNotContainsAny(t, "draft item", d, "data-remote-inbox-publish=", `data-force="1"`, "変更案あり")
	expectContains(t, itemHTML(t, body, taken), `data-force="1"`, "引き継ぐ", "鈴木さんの編集を引き継ぎます")

	// この PC で編集中の記事には「引き継ぐ」を出さない。
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+draft+"/import", nil, nil), http.StatusOK)
	d = itemHTML(t, ed.get("/admin/manuscripts").Body, draft)
	expectContains(t, d, "この PC で編集中", `data-checkout="here"`, `href="/admin/articles/`+draft+`/edit"`, "この PC で続きを書く")
	expectNotContainsAny(t, "checked out here", d, `data-force="1"`, "別PCで編集中")

	// スクリプトは下書きの取り込み・引き継ぎ・変更案の操作を持つ。
	js := ed.get("/static/js/manuscript.js").Body
	expectContains(t, js, "continueDraft", "data-force", "/proposal", "data-proposal-template")
}

// 記事の編集画面は、提出の食い違い（409）で開く差分の画面を持ち、スクリプトがそれを動かす。
func TestRemoteSubmitConflictDialogMarkup(t *testing.T) {
	e := newPCUXEnv(t, "yamada", auth.RoleWriter)
	c := e.local.loggedIn(studentA)
	id := c.createArticle("差分の画面", "")
	edit := c.get("/admin/articles/" + id + "/edit")
	expectStatus(t, edit, http.StatusOK)
	expectContains(t, edit.Body, `id="remote-conflict-modal"`, `data-remote-conflict="`+id+`"`, "公開サーバー側が新しく更新されています",
		"サーバー版を取り込む", "この PC の内容で上書き提出", "data-remote-conflict-overwrite", "data-remote-conflict-pull")
	js := c.get("/static/js/remote_sync.js")
	expectStatus(t, js, http.StatusOK)
	expectContains(t, js.Body, "showConflict", "base_updated_at", "/pull-latest", "server_updated_at")
}

// itemHTML は「届いた原稿」の HTML から記事 id の1件（<li data-inbox-item ...>…</li>）を切り出す。
func itemHTML(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `data-inbox-item data-id="`+id+`"`)
	if start < 0 {
		t.Fatalf("inbox item %s not found", id)
	}
	end := strings.Index(body[start:], "</li>")
	if end < 0 {
		t.Fatalf("inbox item %s is not closed", id)
	}
	return body[start : start+end]
}

// 書き手（writer のトークン）も、提出の食い違いの画面から「サーバー版を取り込む」（pull-latest）で公開サーバーの今の記事を
// この PC に取り込める。記事1件の取得（pull_article）で読み、状態（校閲待ち）・スラッグ・更新日時を写す。編集者だけの
// 「編集中」の印は付けない。取り込んだ後の提出は食い違いにならない。公開中の記事は取り込まない。
func TestRemotePullLatestByWriter(t *testing.T) {
	e := newPCUXEnv(t, "yamada", auth.RoleWriter)
	c := e.local.loggedIn(studentA)
	id := c.createArticle("書き手の記事", "")
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusOK)

	serverAt := bumpRemote(t, e, id, "編集者が直した題", "編集者が直した本文")
	if _, err := e.remote.db.Exec(`UPDATE articles SET slug = ? WHERE id = ?`, "edited-slug", id); err != nil {
		t.Fatal(err)
	}
	la := e.local.mustGetArticle(id)
	la.ContentMarkdown = "PC で直した本文"
	if err := e.local.server.articles.Save(la); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusConflict)

	var out struct {
		Status  article.Status `json:"status"`
		EditURL string         `json:"edit_url"`
	}
	expectStatus(t, c.postJSON("/admin/remote/pending/"+id+"/pull-latest", nil, &out), http.StatusOK)
	if out.Status != article.StatusReview || out.EditURL != "/admin/articles/"+id+"/edit" {
		t.Errorf("pull-latest = %+v", out)
	}
	la = e.local.mustGetArticle(id)
	if la.Title != "編集者が直した題" || la.ContentMarkdown != "編集者が直した本文" || la.Slug != "edited-slug" ||
		la.Status != article.StatusReview || !sameInstant(la.RemoteUpdatedAt, serverAt) {
		t.Errorf("local after writer pull-latest: %q %q slug %q status %s remote_updated_at %v (want %v)",
			la.Title, la.ContentMarkdown, la.Slug, la.Status, la.RemoteUpdatedAt, serverAt)
	}
	if n := e.local.countRows(`SELECT COUNT(*) FROM article_revisions WHERE article_id = ? AND comment = ?`, id, "公開サーバーから取り込む前の版"); n != 1 {
		t.Errorf("snapshots before pull = %d, want 1", n)
	}
	if ra := e.remote.getArticle(id); ra.CheckedOutBy != "" {
		t.Errorf("writer pull-latest checked out the remote article: %q", ra.CheckedOutBy)
	}
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusOK)

	// 公開中の記事は取り込まない（この PC の記事も変えない）。
	pub := seedArticle(t, e.remote.db, article.Article{Title: "公開中", Slug: "live", ContentMarkdown: "x", Status: article.StatusPublished})
	expectStatus(t, c.postJSON("/admin/remote/pending/"+pub+"/pull-latest", nil, nil), http.StatusConflict)
	if n := e.local.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, pub); n != 0 {
		t.Error("pull-latest imported a published article")
	}
}

// 変更案をこの PC の記事に写したときは、写した後の版を履歴に残す。
func TestRemoteResolveProposalSnapshots(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	ed := e.local.loggedIn(chief)
	id := e.remote.seedReview("元の題", "orig-snap")
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/import", nil, nil), http.StatusOK)
	e.remote.seedProposal(id, "p1", mcp.ProposalChanges{Title: strPtr("AI の題")})
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+id+"/proposal",
		url.Values{"proposal_action": {"accept"}, "proposal_id": {"p1"}}, nil), http.StatusOK)
	// 写す前の版は取り込んだときの版と同じため重ねて残らない。写した後の版が履歴の最新になる。
	if n := e.local.countRows(`SELECT COUNT(*) FROM article_revisions WHERE article_id = ? AND comment = ? AND title = ?`, id, "変更案を反映", "AI の題"); n != 1 {
		t.Errorf("post-apply revisions = %d, want 1", n)
	}
}

// 直接公開（publish_article）で公開サーバーがスラッグを付け直したら、この PC の記事もそれに合わせる。
func TestRemoteSendArticleAdoptsSlug(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	seedArticle(t, e.remote.db, article.Article{Title: "先にある記事", Slug: "taken", ContentMarkdown: "x", Status: article.StatusPublished})
	ed := e.local.loggedIn(chief)
	id := ed.createArticle("直接公開する記事", "")
	a := e.local.mustGetArticle(id)
	a.Slug = "taken"
	if err := e.local.server.articles.Save(a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.local.server.sendRemoteArticle(t.Context(), e.local.server.loadRemoteConfig(), a, remoteSendOptions{}, t.Logf); err != nil {
		t.Fatalf("sendRemoteArticle: %v", err)
	}
	remoteSlug := e.remote.getArticle(id).Slug
	if remoteSlug == "taken" {
		t.Fatal("public server kept the duplicate slug")
	}
	if got := e.local.mustGetArticle(id).Slug; got != remoteSlug || a.Slug != remoteSlug {
		t.Errorf("local slug = %q (arg %q), want %q", got, a.Slug, remoteSlug)
	}
}

// スクリプトは、ほかの人が編集中の記事の引き継ぎ（force=1）と、公開・予約の食い違いの扱いを持つ。食い違いの画面の
// 日時は管理画面と同じ形（YYYY/MM/DD HH:MM）にする。
func TestRemoteScriptsTakeoverAndConflict(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	ed := e.local.loggedIn(chief)
	js := ed.get("/static/js/remote_sync.js").Body
	expectContains(t, js, "function takeover", "'checked_out'", "need_force", "goReview", "fmtDate(d.server_updated_at)", "sorus.fmtDate")
	expectNotContains(t, js, "toLocaleString")
	common := ed.get("/static/js/sorus_common.js").Body
	expectContains(t, common, "function fmtDate", "Asia/Tokyo", "function confirmTakeover")
	expectNotContains(t, common, "toLocaleString")
	ms := ed.get("/static/js/manuscript.js").Body
	expectContains(t, ms, "needForce", "confirmTakeover", "publishRemote", "data-inbox-edit", "js_ms_continue")
}

// remoteClientID は PC ごとに一度だけ 16バイトの乱数（32桁の16進数）を作って settings に保存し、以後は同じ値を返す。
func TestRemoteClientIDIsPersistentPerPC(t *testing.T) {
	a := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	b := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	idA := a.server.remoteClientID(t.Context())
	if len(idA) != 32 || strings.Trim(idA, "0123456789abcdef") != "" {
		t.Fatalf("client id = %q, want 32 hex digits", idA)
	}
	if again := a.server.remoteClientID(t.Context()); again != idA {
		t.Errorf("client id changed: %q -> %q", idA, again)
	}
	if got := a.server.getSetting(remoteSettingClientID, ""); got != idA {
		t.Errorf("stored client id = %q, want %q", got, idA)
	}
	if idB := b.server.remoteClientID(t.Context()); idB == "" || idB == idA {
		t.Errorf("second PC client id = %q (first %q), want a different id", idB, idA)
	}
}

// 同じ人の同じ招待のトークンを2台の PC で同時に使っても、「届いた原稿」は編集中の印を PC ごとに見分ける。
// 取り込んだ PC には「この PC で編集中」、もう1台には「別PCで編集中」と「引き継ぐ」を出し、もう1台からの取り込みは
// 引き継ぐ（force=1）まで 409 で断る。引き継いだ後は、元の PC からの提出が断られる。
func TestManuscriptInboxSameTokenTwoPCs(t *testing.T) {
	e := newPCUXEnv(t, "佐藤", auth.RoleEditor)
	pcB := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	if err := pcB.server.saveRemoteConfig(e.local.server.loadRemoteConfig()); err != nil {
		t.Fatal(err)
	}
	if e.local.server.loadRemoteConfig().Token != pcB.server.loadRemoteConfig().Token {
		t.Fatal("the two PCs must share the same token")
	}
	edA, edB := e.local.loggedIn(chief), pcB.loggedIn(chief)
	clientA, clientB := e.local.server.remoteClientID(t.Context()), pcB.server.remoteClientID(t.Context())
	if clientA == clientB {
		t.Fatalf("both PCs have client id %q", clientA)
	}
	id := seedArticle(t, e.remote.db, article.Article{Title: "二台で書く下書き", Slug: "two-pcs", ContentMarkdown: "途中",
		Status: article.StatusDraft, AuthorID: studentA.ID})

	// PC A が取り込むと、公開サーバーの印は PC A の識別子を持つ。
	expectStatus(t, edA.postJSON("/admin/remote/pending/"+id+"/import", nil, nil), http.StatusOK)
	if ra := e.remote.getArticle(id); ra.CheckedOutBy != "佐藤" || ra.CheckedOutClient != clientA {
		t.Fatalf("remote checkout = %q on %q, want 佐藤 on PC A %q", ra.CheckedOutBy, ra.CheckedOutClient, clientA)
	}
	a := itemHTML(t, edA.get("/admin/manuscripts").Body, id)
	expectContains(t, a, `data-checkout="here"`, "この PC で編集中")
	expectNotContainsAny(t, "PC A item", a, `data-force="1"`, "別PCで編集中")

	// PC B（同じ名前・同じトークン）には「別PCで編集中」と「引き継ぐ」を出す。
	b := itemHTML(t, edB.get("/admin/manuscripts").Body, id)
	expectContains(t, b, `data-checkout="elsewhere"`, ">別PCで編集中<", `data-force="1"`, "引き継ぐ", "別の PC での編集を引き継ぎます")
	expectNotContains(t, b, "この PC で編集中")

	// PC B の取り込みは、引き継ぐまで 409 で断り、何も取り込まない。
	res := edB.postJSON("/admin/remote/pending/"+id+"/import", nil, nil)
	expectStatus(t, res, http.StatusConflict)
	var co struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		NeedForce bool   `json:"need_force"`
	}
	if err := json.Unmarshal([]byte(res.Body), &co); err != nil {
		t.Fatal(err)
	}
	if co.Code != SyncCheckedOutCode || !co.NeedForce || !strings.Contains(co.Message, "別の PC で編集中") {
		t.Errorf("PC B import = %+v", co)
	}
	if n := pcB.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, id); n != 0 {
		t.Fatal("PC B imported the article despite PC A's checkout")
	}

	// PC B が引き継ぐと、印は PC B のものになり、表示が入れ替わる。
	expectStatus(t, edB.postJSON("/admin/remote/pending/"+id+"/import", url.Values{"force": {"1"}}, nil), http.StatusOK)
	if ra := e.remote.getArticle(id); ra.CheckedOutClient != clientB {
		t.Fatalf("remote checkout client after takeover = %q, want PC B %q", ra.CheckedOutClient, clientB)
	}
	expectContains(t, itemHTML(t, edB.get("/admin/manuscripts").Body, id), `data-checkout="here"`, "この PC で編集中")
	expectContains(t, itemHTML(t, edA.get("/admin/manuscripts").Body, id), `data-checkout="elsewhere"`, ">別PCで編集中<", `data-force="1"`)

	// 引き継がれた後の PC A からの提出は、別の PC で編集中として断る。
	res = edA.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil)
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, SyncCheckedOutCode, "別の PC で編集中")
	if ra := e.remote.getArticle(id); ra.CheckedOutClient != clientB || ra.Status != article.StatusDraft {
		t.Errorf("remote after PC A submit: client %q status %s", ra.CheckedOutClient, ra.Status)
	}

	// PC B の提出は通る。
	expectStatus(t, edB.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusOK)
}
