package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// waitRemoteBulk は一括同期が終わるまで状態を読み、終わった状態を返す。
func waitRemoteBulk(t *testing.T, c *client) remoteBulkStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var st remoteBulkStatus
		expectStatus(t, c.getJSON("/admin/remote/bulk-sync", &st), http.StatusOK)
		if !st.Running {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("bulk sync did not finish: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 「公開中の記事・添付ファイルを公開サーバーへ一括同期する」は、この PC で公開中の記事をすべて（画像・添付ファイル・
// フォトギャラリーの写真とともに）公開サーバーへ送る。下書きは送らず、公開日時は PC と同じにする。
func TestRemoteBulkSync(t *testing.T) {
	env := newRemoteEnv(t)
	ed := env.local.loggedIn(chief)
	adm := env.local.loggedIn(admin)

	// 公開サーバーにつなぐ前に公開した記事（一括同期をしないと公開サーバーに届かない）。
	m := ed.mustUpload("festival.png", "文化祭", "模擬店の様子")
	env.local.tagMedia(m.ID, "文化祭")
	gallery := articleFormFor("文化祭の記録", "festival", "publish")
	gallery.Set("content_markdown", "当日の写真です。\n\n:::gallery tag=\"文化祭\"\n:::\n")
	galleryID := editPathPattern.FindStringSubmatch(expectRedirect(t, ed.postForm("/admin/articles/new", gallery), "/admin/articles/"))[1]
	oldID := editPathPattern.FindStringSubmatch(expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("昨年のお知らせ", "last-year", "publish")), "/admin/articles/"))[1]
	oldPublished := time.Date(2025, 4, 1, 9, 30, 0, 0, time.UTC)
	if _, err := env.local.db.Exec(`UPDATE articles SET published_at = ? WHERE id = ?`, oldPublished, oldID); err != nil {
		t.Fatal(err)
	}
	writer := env.local.loggedIn(studentA)
	pdfID := writer.createArticle("研究発表の資料", "資料をご覧ください。")
	att := writer.mustAttach(pdfID, "発表資料.pdf", pdfData)
	expectStatus(t, writer.postAPI("/admin/articles/"+pdfID+"/unlock", "", nil), http.StatusNoContent)
	env.local.publish(pdfID, "slides")
	draftID := editPathPattern.FindStringSubmatch(expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("書きかけ", "draft", "draft")), "/admin/articles/"))[1]

	env.configure("editor", auth.RoleEditor)
	page := adm.get("/admin/settings/remote")
	expectStatus(t, page, http.StatusOK)
	section := between(t, page.Body, `<section id="remote-bulk-sync"`, `</section>`)
	expectContains(t, section, ">公開中の記事・添付ファイルを公開サーバーへ一括同期する<span class=\"help-tip\">", "公開中の記事 3 件と、その画像・添付ファイル 1 件",
		`id="remote-bulk-start" class="btn btn-primary btn-md"`, `data-bulk-active="true"`)
	expectNotContains(t, section, `id="remote-bulk-standby"`, `data-bulk-result=`)

	var started remoteBulkStatus
	expectStatus(t, adm.postJSON("/admin/remote/bulk-sync", nil, &started), http.StatusAccepted)
	if !started.Running || started.Total != 3 {
		t.Fatalf("started = %+v, want 3 articles running", started)
	}
	st := waitRemoteBulk(t, adm)
	if st.Outcome != remoteBulkOK || st.Articles != 3 || st.Done != 3 || st.Attachments != 1 || st.Media < 1 || len(st.Failures) != 0 {
		t.Fatalf("finished = %+v", st)
	}
	for _, id := range []string{galleryID, oldID, pdfID} {
		if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished {
			t.Errorf("remote article %s = %s, want published", id, a.Status)
		}
	}
	if n := env.remote.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, draftID); n != 0 {
		t.Error("draft was sent to the publish server")
	}
	if a := env.remote.mustGetArticle(oldID); a.PublishedAt == nil || !a.PublishedAt.Equal(oldPublished) {
		t.Errorf("remote published_at = %v, want %v (kept from the PC)", a.PublishedAt, oldPublished)
	}
	expectSyncStatus(t, env.remote.get(AttachmentURLPrefix+att.ID+"/download"), http.StatusOK)
	if n := env.remote.countRows(`SELECT COUNT(*) FROM media WHERE id = ?`, m.ID); n != 1 {
		t.Errorf("remote gallery media rows = %d, want 1", n)
	}
	expectSyncStatus(t, env.remote.get("/articles/festival"), http.StatusOK)

	// 終わった結果は画面を開き直しても見える（英語でも訳して出す）。
	result := between(t, adm.get("/admin/settings/remote").Body, `<div id="remote-bulk-result"`, `</section>`)
	expectContains(t, result, `data-bulk-result="ok"`, "一括同期が終わりました。記事 3 件・添付ファイル 1 件")
	adm.useEnglish()
	result = between(t, adm.get("/admin/settings/remote").Body, `<div id="remote-bulk-result"`, `</section>`)
	expectContains(t, result, "Bulk sync finished. Sent 3 articles, 1 attachments")
	expectNotContains(t, result, "一括同期")
	adm.useJapanese()

	// 静的サイトに切り替えたあと（公開サーバーは待機中）は、一括同期を始めない。
	saveStatic := url.Values{"kind": {"folder"}, "folder_path": {t.TempDir()}, "folder_site_url": {testBaseURL}}
	expectRedirect(t, adm.postForm("/admin/publish", saveStatic), "/admin/publish")
	expectRedirect(t, adm.postForm("/admin/publish/method", url.Values{"method": {"static"}}), "/admin/publish")
	var refused struct {
		Message string `json:"message"`
	}
	expectStatus(t, adm.postJSON("/admin/remote/bulk-sync", nil, &refused), http.StatusConflict)
	if !strings.Contains(refused.Message, "いまの公開方式が「静的サイト」のため") {
		t.Errorf("standby message = %q", refused.Message)
	}
	section = between(t, adm.get("/admin/settings/remote").Body, `<section id="remote-bulk-sync"`, `</section>`)
	expectContains(t, section, `id="remote-bulk-standby"`, `data-bulk-active="false"`)
	if !strings.Contains(section, `id="remote-bulk-start"`) || !strings.Contains(between(t, section, `id="remote-bulk-start"`, `>`), "disabled") {
		t.Error("bulk sync button is not disabled while the publish server is on standby")
	}
}

// 記事を直接公開できない招待（writer）では、一括同期を出さず、始められない。
func TestRemoteBulkSyncNeedsPublishRole(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("writer", auth.RoleWriter)
	adm := env.local.loggedIn(admin)
	expectNotContains(t, adm.get("/admin/settings/remote").Body, `id="remote-bulk-sync"`)
	if res := adm.postJSON("/admin/remote/bulk-sync", nil, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("writer invite: bulk sync = %d, want 403", res.StatusCode)
	}
	if res := env.local.loggedIn(chief).postJSON("/admin/remote/bulk-sync", nil, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("local editor: bulk sync = %d, want 403", res.StatusCode)
	}
}
