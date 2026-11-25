package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/internal/auth"
)

// syncAttachmentOf は data を添付資料 id・filename として送る形にする（withData なら中身も付ける）。
func syncAttachmentOf(id, filename string, data []byte, withData bool) SyncAttachment {
	a := SyncAttachment{ID: id, Filename: filename, Size: int64(len(data)), SHA256: sha256Hex(data), CreatedAt: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)}
	if withData {
		a.Data = data
	}
	return a
}

// 公開サーバーは sync_attachments で、記事の添付資料を PC と同じ ID で保存する。中身の無い記録は、同じ中身のファイルが
// あれば保存し、無ければ Missing として返す（PC は Missing だけ中身を付けて送り直す）。保存した添付資料は公開サイトの
// /attachments/<ID>/download から落とせる。一覧に無い添付資料は PruneAttachments で消す。
func TestSyncAttachmentsStoresAndPrunes(t *testing.T) {
	env := newSyncEnv(t)
	ed := env.loggedIn(chief)
	articleID := ed.createArticle("研究発表", "資料を添付します。")
	env.publish(articleID, "research")
	editor := env.token("編集者", auth.RoleEditor)
	const attID = "0123456789abcdef0123456789abcdef"
	req := func(prune bool, atts ...SyncAttachment) SyncRequest {
		return SyncRequest{Action: SyncActionSyncAttachments, ArticleID: articleID, Attachments: atts, PruneAttachments: prune}
	}

	rec := env.call(editor, req(true, syncAttachmentOf(attID, "研究 資料.pdf", pdfData, false)))
	expectSyncStatus(t, rec, http.StatusOK)
	out := decodeSync[SyncAttachmentsResponse](t, rec)
	if len(out.Missing) != 1 || out.Missing[0] != attID || len(out.Saved) != 0 {
		t.Fatalf("metadata only (no file yet) = %+v, want missing", out)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM attachments WHERE id = ?`, attID); n != 0 {
		t.Fatalf("missing attachment saved a row (%d)", n)
	}

	rec = env.call(editor, req(false, syncAttachmentOf(attID, "研究 資料.pdf", pdfData, true)))
	expectSyncStatus(t, rec, http.StatusOK)
	if out := decodeSync[SyncAttachmentsResponse](t, rec); len(out.Saved) != 1 || len(out.Missing) != 0 {
		t.Fatalf("with data = %+v, want saved", out)
	}
	var row struct {
		ArticleID string `db:"article_id"`
		Filename  string `db:"filename"`
		FilePath  string `db:"file_path"`
		Size      int64  `db:"file_size"`
		Mime      string `db:"mime_type"`
	}
	if err := env.db.Get(&row, `SELECT article_id, filename, file_path, file_size, mime_type FROM attachments WHERE id = ?`, attID); err != nil {
		t.Fatal(err)
	}
	if row.ArticleID != articleID || row.Filename != "研究 資料.pdf" || row.FilePath != attID+".pdf" || row.Size != int64(len(pdfData)) || row.Mime != "application/pdf" {
		t.Fatalf("row = %+v", row)
	}
	if got, err := os.ReadFile(filepath.Join(env.attachmentDir, attID+".pdf")); err != nil || !bytes.Equal(got, pdfData) {
		t.Fatalf("stored file = %q, %v", got, err)
	}
	dl := env.get(AttachmentURLPrefix + attID + "/download")
	expectSyncStatus(t, dl, http.StatusOK)
	if !bytes.Equal(dl.Body.Bytes(), pdfData) {
		t.Errorf("download body = %q", dl.Body.Bytes())
	}

	// 同じ中身なら、中身を付けなくても保存済みとして扱う。中身が変わっていれば送り直してもらう。
	rec = env.call(editor, req(true, syncAttachmentOf(attID, "研究 資料（改）.pdf", pdfData, false)))
	expectSyncStatus(t, rec, http.StatusOK)
	if out := decodeSync[SyncAttachmentsResponse](t, rec); len(out.Saved) != 1 || len(out.Missing) != 0 || len(out.Removed) != 0 {
		t.Fatalf("unchanged file = %+v, want saved", out)
	}
	changed := append(append([]byte{}, pdfData...), []byte("% v2\n")...)
	rec = env.call(editor, req(true, syncAttachmentOf(attID, "研究 資料.pdf", changed, false)))
	if out := decodeSync[SyncAttachmentsResponse](t, rec); len(out.Missing) != 1 {
		t.Fatalf("changed file = %+v, want missing", out)
	}

	// 一覧から外した添付資料は、記録とファイルを消す。
	rec = env.call(editor, req(true))
	expectSyncStatus(t, rec, http.StatusOK)
	if out := decodeSync[SyncAttachmentsResponse](t, rec); len(out.Removed) != 1 || out.Removed[0] != attID {
		t.Fatalf("prune = %+v", out)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM attachments WHERE id = ?`, attID); n != 0 {
		t.Errorf("pruned row remains (%d)", n)
	}
	if _, err := os.Stat(filepath.Join(env.attachmentDir, attID+".pdf")); !os.IsNotExist(err) {
		t.Errorf("pruned file remains: %v", err)
	}
}

// 送られた添付資料は、管理画面での添付と同じく検査する（形式・中身・大きさ・ID）。記事が無い・権限が無いときは断る。
func TestSyncAttachmentsRejectsBadInput(t *testing.T) {
	env := newSyncEnv(t)
	ed := env.loggedIn(chief)
	articleID := ed.createArticle("検査", "本文")
	editor := env.token("編集者", auth.RoleEditor)
	const attID = "fedcba9876543210fedcba9876543210"
	call := func(token, article string, a SyncAttachment) *httptest.ResponseRecorder {
		return env.call(token, SyncRequest{Action: SyncActionSyncAttachments, ArticleID: article, Attachments: []SyncAttachment{a}})
	}
	tampered := syncAttachmentOf(attID, "a.pdf", pdfData, true)
	tampered.SHA256 = sha256Hex([]byte("other"))
	short := syncAttachmentOf(attID, "a.pdf", pdfData, true)
	short.Size--
	for _, tc := range []struct {
		name string
		a    SyncAttachment
		code int
	}{
		{"bad id", syncAttachmentOf("../../etc/passwd", "a.pdf", pdfData, true), http.StatusUnprocessableEntity},
		{"executable", syncAttachmentOf(attID, "virus.exe", []byte("MZ\x90\x00"), true), http.StatusUnprocessableEntity},
		{"fake pdf", syncAttachmentOf(attID, "fake.pdf", []byte("this is not a pdf"), true), http.StatusUnsupportedMediaType},
		{"sha mismatch", tampered, http.StatusUnprocessableEntity},
		{"size mismatch", short, http.StatusUnprocessableEntity},
		{"bad sha", SyncAttachment{ID: attID, Filename: "a.pdf", Size: 3, SHA256: "xyz"}, http.StatusUnprocessableEntity},
	} {
		if rec := call(editor, articleID, tc.a); rec.Code != tc.code {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.code, rec.Body.String())
		}
	}
	if n := env.countRows(`SELECT COUNT(*) FROM attachments`); n != 0 {
		t.Errorf("rejected attachments saved %d rows", n)
	}
	if entries, _ := os.ReadDir(env.attachmentDir); len(entries) != 0 {
		t.Errorf("rejected attachments left %d files", len(entries))
	}
	if rec := call(editor, "0123456789abcdef0123456789abcdef", syncAttachmentOf(attID, "a.pdf", pdfData, true)); rec.Code != http.StatusNotFound {
		t.Errorf("unknown article: status %d, want 404", rec.Code)
	}
	if rec := call(env.token("ライター", auth.RoleWriter), articleID, syncAttachmentOf(attID, "a.pdf", pdfData, true)); rec.Code != http.StatusForbidden {
		t.Errorf("writer: status %d, want 403", rec.Code)
	}
	env.server.cfg.MaxAttachmentSize = int64(len(pdfData)) - 1
	if rec := call(editor, articleID, syncAttachmentOf(attID, "a.pdf", pdfData, true)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: status %d, want 413", rec.Code)
	}

	rec := env.call(editor, SyncRequest{Action: SyncActionServerInfo})
	expectSyncStatus(t, rec, http.StatusOK)
	if info := decodeSync[SyncServerInfoResponse](t, rec); !info.Attachments || !info.AISwitch || info.MaxAttachmentBytes != env.server.cfg.MaxAttachmentSize {
		t.Errorf("server_info = %+v, want attachments and the AI switch", info)
	}
}

// PC で記事を公開すると、添付資料（PDF）も公開サーバーへ送り、公開サーバーの /attachments/<ID>/download から落とせる。
// 添付資料を外して公開し直すと、公開サーバーからも消える。
func TestRemotePublishSendsAttachments(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("editor", auth.RoleEditor)
	writer := env.local.loggedIn(studentA)
	id := writer.createArticle("研究発表の資料", "資料をご覧ください。")
	att := writer.mustAttach(id, "発表資料.pdf", pdfData)
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)

	ed := env.local.loggedIn(chief)
	a := env.local.mustGetArticle(id)
	form := url.Values{"title": {a.Title}, "slug": {"research-slides"}, "category_id": {"1"},
		"content_markdown": {a.ContentMarkdown + "\n\n" + att.Markdown}, "action": {"publish"}}
	loc := expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	expectFlash(t, loc, "remote_published")

	dl := env.remote.get(AttachmentURLPrefix + att.ID + "/download")
	expectSyncStatus(t, dl, http.StatusOK)
	if !bytes.Equal(dl.Body.Bytes(), pdfData) {
		t.Errorf("remote download = %q", dl.Body.Bytes())
	}
	page := env.remote.get("/articles/research-slides")
	expectSyncStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), AttachmentURLPrefix+att.ID+"/download") {
		t.Error("remote article does not link the attachment")
	}

	// 添付資料を外して公開し直すと、公開サーバーからも消える。
	if res := ed.request(http.MethodDelete, "/admin/articles/"+id+"/attachments/"+att.ID, nil, nil); res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete attachment = %d: %s", res.StatusCode, res.Body)
	}
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	expectFlash(t, loc, "remote_published")
	if n := env.remote.countRows(`SELECT COUNT(*) FROM attachments WHERE id = ?`, att.ID); n != 0 {
		t.Errorf("removed attachment remains on the publish server (%d rows)", n)
	}
}

// 添付資料の同期を知らない古い公開サーバーには、記事だけを公開し、添付資料を送れなかったことを知らせる。
func TestRemotePublishAttachmentsOldServer(t *testing.T) {
	env := newRemoteEnv(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"action":"`+SyncActionSyncAttachments+`"`)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"未対応の action です: \"sync_attachments\""}`))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		env.remote.server.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	if err := env.local.server.saveRemoteConfig(remoteConfig{URL: proxy.URL, SyncPath: env.remote.server.cfg.SyncAPIPath,
		Token: env.remote.token("editor", auth.RoleEditor)}); err != nil {
		t.Fatal(err)
	}
	ed := env.local.loggedIn(chief)
	id := ed.createArticle("古い公開サーバー", "本文")
	ed.mustAttach(id, "資料.pdf", pdfData)
	a := env.local.mustGetArticle(id)
	form := url.Values{"title": {a.Title}, "slug": {"old-server"}, "category_id": {"1"}, "content_markdown": {a.ContentMarkdown}, "action": {"publish"}}
	loc := expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	expectFlash(t, loc, "remote_published_attachments_old")
	env.remote.mustGetArticle(id)
	page := ed.get(loc)
	expectContains(t, page.Body, "公開サーバーの sorus が古いため、添付ファイル（PDF など）は送れませんでした")
}
