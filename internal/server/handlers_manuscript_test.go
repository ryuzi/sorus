package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// postFile は fetch と同じく X-CSRF-Token 付きで、data を file として multipart で POST する。
func (c *client) postFile(path, filename string, data []byte) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
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

// manuscriptFileWords は、原稿ファイル（.sorus）のやり取りがあった頃の画面・スクリプトにあった言葉。
var manuscriptFileWords = []string{"原稿ファイル", "manuscript file", `accept=".sorus"`, "（.sorus）", "(.sorus)",
	"data-manuscript-drop", "data-manuscript-export", "/admin/manuscripts/export/", "/admin/manuscripts/button/"}

// 原稿ファイル（.sorus）のやり取りは無い。書き出し（記事の画面のボタン）も、ファイルの読み込み（「届いた原稿」の
// プレビュー・受け取り）も、どの役割でも 404 になり、何も保存しない。画面・スクリプトにも入口や案内を出さない。
func TestManuscriptFileExchangeRemoved(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	id := seedArticle(t, env.db, article.Article{Title: "山田の記事", ContentMarkdown: "本文", Status: article.StatusDraft, AuthorID: studentA.ID})
	for _, u := range []testUser{studentA, chief, admin} {
		c := env.loggedIn(u)
		expectStatus(t, c.get("/admin/manuscripts/export/"+id), http.StatusNotFound)
		expectStatus(t, c.get("/admin/manuscripts/button/"+id), http.StatusNotFound)
		for _, path := range []string{"/admin/manuscripts/preview", "/admin/manuscripts/accept"} {
			expectStatus(t, c.postFile(path, "a.sorus", []byte("PK\x03\x04")), http.StatusNotFound)
		}
		edit := c.get("/admin/articles/" + id + "/edit")
		expectStatus(t, edit, http.StatusOK)
		expectNotContainsAny(t, u.Username+" editor", edit.Body, manuscriptFileWords...)
		c.postJSON("/admin/articles/"+id+"/unlock", nil, nil)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles`); n != 1 {
		t.Errorf("articles = %d, want 1", n)
	}

	// 公開サーバーに参加していなければ、「届いた原稿」は公開サーバーへの参加の案内だけを出す（ファイルの受け取り口は無い）。
	// ダッシュボードには公開サーバーの校閲待ち・公開待ちのパネルを出さない。
	for _, u := range []testUser{chief, admin} {
		c := env.loggedIn(u)
		inbox := c.get("/admin/manuscripts")
		expectStatus(t, inbox, http.StatusOK)
		expectContains(t, inbox.Body, `id="manuscript-join-guide"`, "招待コード")
		expectNotContainsAny(t, u.Username+" inbox", inbox.Body, append(manuscriptFileWords, `type="file"`, `id="manuscript-inbox-list"`)...)
		expectNotContainsAny(t, u.Username+" dashboard", c.get("/admin").Body,
			append(manuscriptFileWords, "data-remote-pending", `id="manuscript-inbox-link"`)...)
	}
	// 公開サーバーの設定を開ける管理者には、設定への入口を出す。
	expectContains(t, env.loggedIn(admin).get("/admin/manuscripts").Body, `href="/admin/settings/remote"`)

	// スクリプトは公開サーバーの原稿を受け取る処理だけを持つ。
	js := env.loggedIn(chief).get("/static/js/manuscript.js")
	expectStatus(t, js, http.StatusOK)
	expectContains(t, js.Body, "previewRemote", "/admin/remote/pending/")
	expectNotContainsAny(t, "manuscript.js", js.Body, append(manuscriptFileWords,
		"/admin/manuscripts/preview", "/admin/manuscripts/accept", "data-manuscript-file")...)
}

// 「届いた原稿」の画面は編集者（と管理者）だけが開け、ナビの入口も編集者・管理者にだけ出す。ダッシュボードの
// 「届いた原稿」の部分は校閲待ち・公開待ちのパネルに統合したため、部分を返す URL は無い。
func TestManuscriptPermissions(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	expectStatus(t, env.loggedIn(studentA).get("/admin/manuscripts"), http.StatusForbidden)
	for _, u := range []testUser{chief, admin} {
		home := env.loggedIn(u).get("/admin").Body
		expectContains(t, home, `href="/admin/manuscripts"`)
		expectNotContains(t, home, `data-manuscript-panel=`, "js/manuscript.js")
		expectStatus(t, env.loggedIn(u).get("/admin/manuscripts/panel"), http.StatusNotFound)
		// スクリプトは static/js/manuscript.js から配信し、Go の手作りの配信口は無い。
		expectStatus(t, env.loggedIn(u).get("/admin/manuscripts/manuscript.js"), http.StatusNotFound)
	}
	writer := env.loggedIn(studentA)
	expectNotContainsAny(t, "writer home", writer.get("/admin").Body, `href="/admin/manuscripts"`, `data-manuscript-panel=`)
}

// 公開サーバーに参加していれば、「届いた原稿」は公開サーバーの校閲待ちを並べ、「この PC に受け取る」で中身と差分を
// 確かめてから取り込める（今までどおり）。ダッシュボードは校閲待ち・公開待ちのパネル1つだけを出し、そこに
// 「届いた原稿」への入口を置く（以前の「届いた原稿」の部分と「公開サーバーの校閲待ち記事」の重複を解消）。
func TestManuscriptInboxListsServerPending(t *testing.T) {
	e := &remoteEnv{local: newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil),
		remote: newSyncEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)}
	e.configure("sato", auth.RoleEditor)
	serverID := e.remote.seedReview("サーバーに届いた原稿", "server-one")
	ed := e.local.loggedIn(chief)

	res := ed.get("/admin/manuscripts")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="manuscript-inbox-list"`, "サーバーに届いた原稿", `data-id="`+serverID+`"`,
		`data-remote-inbox-import="`+serverID+`"`, "data-remote-inbox-publish=", "/static/js/manuscript.js?v=", "/static/js/sorus_common.js?v=")
	if i, j := strings.Index(res.Body, "/static/js/sorus_common.js"), strings.Index(res.Body, "/static/js/manuscript.js"); i < 0 || i > j {
		t.Errorf("sorus_common.js must be loaded before manuscript.js")
	}
	expectNotContainsAny(t, "inbox with a public server", res.Body, append(manuscriptFileWords, `id="manuscript-join-guide"`)...)
	// 受け取りは、まず中身と差分を見せる（いきなり取り込まない）。
	expectContains(t, ed.get("/static/js/manuscript.js").Body, "/preview", "/import", "previewRemote", "sorus.diffTable(")
	dash := ed.get("/admin")
	expectStatus(t, dash, http.StatusOK)
	expectContains(t, dash.Body, "校閲待ち・公開待ち（公開サーバー", `<a href="/admin/manuscripts" id="manuscript-inbox-link"`, "届いた原稿を開く")
	expectNotContainsAny(t, "dashboard", dash.Body, append(manuscriptFileWords, `data-manuscript-panel=`, `id="manuscript-inbox-summary"`)...)
	if n := strings.Count(dash.Body, "data-remote-pending "); n != 1 {
		t.Errorf("dashboard has %d publish server review panels, want 1", n)
	}

	// 「この PC に受け取る」: 取り込まずに中身を見せ、受け取ると同じ ID の校閲待ちの記事になる。
	var preview manuscriptPreview
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+serverID+"/preview", nil, &preview), http.StatusOK)
	if preview.Title != "サーバーに届いた原稿" || preview.Existing != nil || !strings.Contains(preview.HTML, "サーバーに届いた原稿の本文") {
		t.Errorf("preview = %+v", preview)
	}
	if n := e.local.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, serverID); n != 0 {
		t.Fatal("preview created a local article")
	}
	var imported struct {
		Created bool   `json:"created"`
		EditURL string `json:"edit_url"`
	}
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+serverID+"/import", nil, &imported), http.StatusOK)
	if !imported.Created || imported.EditURL != "/admin/articles/"+serverID+"/edit" {
		t.Errorf("import = %+v", imported)
	}
	if a := e.local.mustGetArticle(serverID); a.Status != article.StatusReview || a.Title != "サーバーに届いた原稿" {
		t.Errorf("imported article = %+v", a)
	}
	expectContains(t, ed.get("/admin/manuscripts").Body, "この PC に取り込み済み（校閲待ち）", `href="/admin/articles/`+serverID+`/edit"`)

	// この PC の記事を変えていれば、差分を確かめて「この内容で上書きする」で受け取り直せる。
	if _, err := e.local.db.Exec(`UPDATE articles SET status = 'draft', content_markdown = 'この PC で直した本文' WHERE id = ?`, serverID); err != nil {
		t.Fatal(err)
	}
	expectContains(t, ed.get("/admin/manuscripts").Body, `data-remote-inbox-import="`+serverID+`"`, "この PC に取り込み済み（下書き）")
	preview = manuscriptPreview{}
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+serverID+"/preview", nil, &preview), http.StatusOK)
	if ex := preview.Existing; ex == nil || !ex.NeedConfirm || !ex.Changed ||
		fmt.Sprint(ex.Rows) != fmt.Sprint([]diffRow{{"change", "この PC で直した本文", "サーバーに届いた原稿の本文"}}) {
		t.Fatalf("preview existing = %+v", preview.Existing)
	}
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+serverID+"/import", nil, nil), http.StatusConflict)
	expectStatus(t, ed.postJSON("/admin/remote/pending/"+serverID+"/import", url.Values{"overwrite": {"1"}}, nil), http.StatusOK)
	if a := e.local.mustGetArticle(serverID); a.ContentMarkdown != "サーバーに届いた原稿の本文" || a.Status != article.StatusReview {
		t.Errorf("after overwrite = %+v", a)
	}

	// この PC で公開中なら受け取れない。
	if _, err := e.local.db.Exec(`UPDATE articles SET status = 'published', published_at = CURRENT_TIMESTAMP WHERE id = ?`, serverID); err != nil {
		t.Fatal(err)
	}
	expectNotContains(t, ed.get("/admin/manuscripts").Body, `data-remote-inbox-import="`+serverID+`"`)

	// 公開サーバーに届かないときは、届かなかったことを伝える。
	e.remote.srv.Close()
	res = ed.get("/admin/manuscripts")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="manuscript-remote-error"`)
}

func TestLineDiff(t *testing.T) {
	for _, tc := range []struct {
		old, new string
		want     []diffRow
	}{
		{"", "", []diffRow{}},
		{"a\nb\n", "a\nb", []diffRow{{"same", "a", "a"}, {"same", "b", "b"}}},
		{"a\nb\nc", "a\nc", []diffRow{{"same", "a", "a"}, {"del", "b", ""}, {"same", "c", "c"}}},
		{"a\r\nc", "a\nb\nc", []diffRow{{"same", "a", "a"}, {"add", "", "b"}, {"same", "c", "c"}}},
		{"x\ny", "p\nq\nr", []diffRow{{"change", "x", "p"}, {"change", "y", "q"}, {"add", "", "r"}}},
	} {
		got := lineDiff(tc.old, tc.new)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("lineDiff(%q, %q) = %v, want %v", tc.old, tc.new, got, tc.want)
		}
	}
	// 差分の行は JSON で op・left・right として届く（manuscript.js が読む形）。
	b, _ := json.Marshal(diffRow{Op: "add", Right: "x"})
	if string(b) != `{"op":"add","left":"","right":"x"}` {
		t.Errorf("diffRow json = %s", b)
	}
}
