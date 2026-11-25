package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var pdfData = []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")

// zipBytes は names のエントリを持つ ZIP を返す。
func zipBytes(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("<x/>"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// attach は記事 id に filename の資料を添付する。headers は追加のヘッダー（HX-Request など）。
func (c *client) attach(id, filename string, data []byte, headers map[string]string) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		c.t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	h := map[string]string{"Content-Type": mw.FormDataContentType(), "Accept": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	return c.request(http.MethodPost, "/admin/articles/"+id+"/attachments", &buf, h)
}

type attachmentJSON struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	FileSize int64  `json:"file_size"`
	MimeType string `json:"mime_type"`
	URL      string `json:"url"`
	Markdown string `json:"markdown"`
	Message  string `json:"message"`
}

func (c *client) mustAttach(id, filename string, data []byte) attachmentJSON {
	c.t.Helper()
	res := c.attach(id, filename, data, nil)
	expectStatus(c.t, res, http.StatusCreated)
	var out attachmentJSON
	if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
		c.t.Fatalf("attach response: %v\n%s", err, res.Body)
	}
	return out
}

// publish は編集者として記事を公開する（記事のロックは呼び出し前に解放しておくこと）。
func (e *testEnv) publish(id, slug string) {
	e.t.Helper()
	ed := e.loggedIn(chief)
	a := e.mustGetArticle(id)
	form := url.Values{"title": {a.Title}, "slug": {slug}, "category_id": {"1"}, "content_markdown": {a.ContentMarkdown}, "action": {"publish"}}
	expectRedirect(e.t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit?flash=published")
	expectStatus(e.t, ed.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
}

func TestAttachmentUploadAndDownload(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("論文の紹介", "本文")

	// HTMX からのアップロードは一覧のパーシャルを返す。
	res := writer.attach(id, "研究発表 資料.pdf", pdfData, map[string]string{"HX-Request": "true"})
	expectStatus(t, res, http.StatusCreated)
	expectContains(t, res.Body, "研究発表 資料.pdf", "PDF", `data-attachment-delete=`, `id="attachment-form"`)

	att := writer.mustAttach(id, `C:\Users\student\Desktop\..\データ.csv`, []byte("名前,点数\n山田,90\n"))
	if att.Filename != "データ.csv" || att.MimeType != "text/csv" || !strings.HasPrefix(att.URL, AttachmentURLPrefix) ||
		att.Markdown != "[データ.csv]("+att.URL+")" {
		t.Fatalf("attach json = %+v", att)
	}

	// 保存先は静的配信しないディレクトリで、ファイル名は乱数、実行権限は付けない。
	var stored []struct {
		Filename string `db:"filename"`
		FilePath string `db:"file_path"`
		Size     int64  `db:"file_size"`
	}
	if err := env.db.Select(&stored, `SELECT filename, file_path, file_size FROM attachments WHERE article_id = ? ORDER BY filename`, id); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored = %+v", stored)
	}
	for _, st := range stored {
		if !regexp.MustCompile(`^[0-9a-f]{32}\.(pdf|csv)$`).MatchString(st.FilePath) {
			t.Errorf("file_path = %q", st.FilePath)
		}
		fi, err := os.Stat(filepath.Join(env.attachmentDir, st.FilePath))
		if err != nil {
			t.Fatalf("stored file: %v", err)
		}
		if fi.Size() != st.Size {
			t.Errorf("size = %d, want %d", fi.Size(), st.Size)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 != 0 {
			t.Errorf("stored file is executable: %v", fi.Mode())
		}
	}
	if entries, _ := os.ReadDir(env.uploadDir); len(entries) != 0 {
		t.Errorf("attachments were written to the public upload dir: %v", entries)
	}
	// アップロード画像の静的配信からは読めない。
	expectStatus(t, env.newClient().get(UploadURLPrefix+stored[0].FilePath), http.StatusNotFound)

	// 公開前の記事の資料は公開サイトからダウンロードできない。管理画面からは確認できる（数えない）。
	reader := env.newClient()
	expectStatus(t, reader.get(att.URL), http.StatusNotFound)
	res = writer.get("/admin/attachments/" + att.ID + "/download")
	expectStatus(t, res, http.StatusOK)
	if res.Body != "名前,点数\n山田,90\n" {
		t.Fatalf("admin download body = %q", res.Body)
	}

	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	env.publish(id, "paper")

	// 公開後は安全なヘッダーでダウンロードでき、回数を数える。
	res = reader.get(att.URL)
	expectStatus(t, res, http.StatusOK)
	if res.Body != "名前,点数\n山田,90\n" {
		t.Fatalf("download body = %q", res.Body)
	}
	h := res.Header
	if h.Get("Content-Type") != "text/csv" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Content-Disposition") != `attachment; filename="___.csv"; filename*=UTF-8''%E3%83%87%E3%83%BC%E3%82%BF.csv` ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("download headers = %v", h)
	}
	// 分割ダウンロードの続きは数えない。
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+att.URL, nil)
	req.Header.Set("Range", "bytes=5-")
	if r := reader.do(req); r.StatusCode != http.StatusPartialContent {
		t.Fatalf("range request status = %d", r.StatusCode)
	}
	if n := env.countRows(`SELECT download_count FROM attachments WHERE id = ?`, att.ID); n != 1 {
		t.Fatalf("download_count = %d, want 1", n)
	}

	// 公開ページの末尾にダウンロードカードを出す。
	res = reader.get("/articles/paper")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "添付ファイル（PDF / 配布資料）", "研究発表 資料.pdf", "データ.csv", `href="`+att.URL+`"`, "ダウンロード 1回")
	expectStatus(t, reader.get(AttachmentURLPrefix+"nonexistent/download"), http.StatusNotFound)
}

func TestAttachmentRejectsDangerousFiles(t *testing.T) {
	env := newTestEnvWith(t, Config{MaxAttachmentSize: 4 << 10}, nil)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("危険なファイル", "本文")

	docx := zipBytes(t, "[Content_Types].xml", "word/document.xml")
	for _, tc := range []struct {
		name string
		data []byte
		code int
	}{
		{"virus.exe", []byte("MZ\x90\x00"), http.StatusUnsupportedMediaType},
		{"script.sh", []byte("#!/bin/sh\nrm -rf /\n"), http.StatusUnsupportedMediaType},
		{"shell.php", []byte("<?php system($_GET['c']); ?>"), http.StatusUnsupportedMediaType},
		{"app.js", []byte("alert(1)"), http.StatusUnsupportedMediaType},
		{"noext", pdfData, http.StatusUnsupportedMediaType},
		{"exe-as.pdf", []byte("MZ\x90\x00\x03\x00\x00\x00"), http.StatusUnsupportedMediaType},
		{"elf.pdf", []byte("\x7fELF\x02\x01\x01"), http.StatusUnsupportedMediaType},
		{"fake.pdf", []byte("this is not a pdf"), http.StatusUnsupportedMediaType},
		{"shebang.txt", []byte("#!/usr/bin/env python\nprint(1)\n"), http.StatusUnsupportedMediaType},
		{"php.txt", []byte("<?php echo 1; ?>"), http.StatusUnsupportedMediaType},
		{"page.csv", []byte("<html><script>alert(1)</script></html>"), http.StatusUnsupportedMediaType},
		{"binary.txt", []byte{0x00, 0x01, 0x02, 0xff}, http.StatusUnsupportedMediaType},
		{"broken.zip", []byte("PK\x03\x04broken"), http.StatusUnsupportedMediaType},
		{"notword.docx", zipBytes(t, "[Content_Types].xml", "xl/workbook.xml"), http.StatusUnsupportedMediaType},
		{"macro.docx", zipBytes(t, "[Content_Types].xml", "word/document.xml", "word/vbaProject.bin"), http.StatusUnsupportedMediaType},
		{"invoice\u202Efdp.exe", pdfData, http.StatusUnsupportedMediaType}, // RLO で .pdf に見せかけた .exe
		{"large.pdf", append(append([]byte{}, pdfData...), bytes.Repeat([]byte("x"), 5<<10)...), http.StatusRequestEntityTooLarge},
		{"report.docx", docx, http.StatusCreated},
		{"data.zip", zipBytes(t, "a.txt"), http.StatusCreated},
		{"sjis.csv", []byte{0x8e, 0x81, 0x96, 0xbc, ',', 0x93, 0x5f, '\n'}, http.StatusCreated},
		{"Notes.TXT", []byte("plain text\n"), http.StatusCreated},
	} {
		res := writer.attach(id, tc.name, tc.data, nil)
		if res.StatusCode != tc.code {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, res.StatusCode, tc.code, res.Body)
		}
	}
	if n := env.countRows(`SELECT COUNT(*) FROM attachments WHERE article_id = ?`, id); n != 4 {
		t.Fatalf("attachments = %d, want 4", n)
	}
	// 拒否したファイルは保存先に残さない（一時ファイルを含む）。
	if entries, _ := os.ReadDir(env.attachmentDir); len(entries) != 4 {
		t.Fatalf("attachment dir has %d entries, want 4", len(entries))
	}

	// 表示用のファイル名から書式文字（RLO）を除く。
	att := writer.mustAttach(id, "report\u202Etxt.pdf", pdfData)
	if att.Filename != "reporttxt.pdf" {
		t.Fatalf("filename = %q", att.Filename)
	}
	expectStatus(t, writer.attach("nonexistent", "a.pdf", pdfData, nil), http.StatusNotFound)
	expectStatus(t, env.newClient().attach(id, "a.pdf", pdfData, nil), http.StatusUnauthorized)
}

func TestAttachmentPermissionsAndDelete(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	other := env.loggedIn(studentB)
	id := writer.createArticle("共同の記事", "本文")
	att := writer.mustAttach(id, "slides.pdf", pdfData)
	var stored string
	if err := env.db.Get(&stored, `SELECT file_path FROM attachments WHERE id = ?`, att.ID); err != nil {
		t.Fatal(err)
	}

	// 他のユーザーが編集中なら追加・削除できない。
	expectStatus(t, writer.get("/admin/articles/"+id+"/edit"), http.StatusOK)
	// 編集できる人の編集画面にはアップロード欄と削除ボタンを出し、閲覧専用の人には出さない。
	expectContains(t, writer.get("/admin/articles/"+id+"/edit").Body, `id="attachment-form"`, `data-attachment-delete="`+att.ID+`"`)
	res := other.get("/admin/articles/" + id + "/edit")
	expectContains(t, res.Body, "slides.pdf")
	if strings.Contains(res.Body, `id="attachment-form"`) || strings.Contains(res.Body, "data-attachment-delete") {
		t.Error("read-only user can edit attachments")
	}
	expectStatus(t, other.attach(id, "b.pdf", pdfData, nil), http.StatusConflict)
	delPath := "/admin/articles/" + id + "/attachments/" + att.ID
	expectStatus(t, other.request(http.MethodDelete, delPath, nil, nil), http.StatusConflict)
	noCSRF, _ := http.NewRequest(http.MethodDelete, env.srv.URL+delPath, nil)
	expectStatus(t, writer.do(noCSRF), http.StatusForbidden)

	// 削除するとファイルも消え、HTMX には一覧を返す。
	res = writer.request(http.MethodDelete, delPath, nil, map[string]string{"HX-Request": "true"})
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "添付資料はありません")
	if _, err := os.Stat(filepath.Join(env.attachmentDir, stored)); !os.IsNotExist(err) {
		t.Fatalf("stored file still exists: %v", err)
	}
	expectStatus(t, writer.request(http.MethodDelete, delPath, nil, nil), http.StatusNotFound)

	// 公開中の記事の資料は執筆者が変更できない。
	att = writer.mustAttach(id, "final.pdf", pdfData)
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	env.publish(id, "shared")
	expectStatus(t, writer.attach(id, "c.pdf", pdfData, nil), http.StatusForbidden)
	expectStatus(t, writer.request(http.MethodDelete, "/admin/articles/"+id+"/attachments/"+att.ID, nil, nil), http.StatusForbidden)
	ed := env.loggedIn(chief)
	expectStatus(t, ed.request(http.MethodDelete, "/admin/articles/"+id+"/attachments/"+att.ID, nil, nil), http.StatusNoContent)
}

func TestAttachmentDownloadRejectsTraversal(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("改ざん", "本文")
	att := writer.mustAttach(id, "a.pdf", pdfData)
	expectStatus(t, writer.postAPI("/admin/articles/"+id+"/unlock", "", nil), http.StatusNoContent)
	env.publish(id, "tampered")

	// 保存先の外を指すパス（DB の改ざんを想定）は配信しない。
	secret := filepath.Join(filepath.Dir(env.attachmentDir), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret.txt", "..", secret, `..\secret.txt`, "sub/../../secret.txt"} {
		if _, err := env.db.Exec(`UPDATE attachments SET file_path = ? WHERE id = ?`, p, att.ID); err != nil {
			t.Fatal(err)
		}
		res := env.newClient().get(att.URL)
		if res.StatusCode != http.StatusNotFound || strings.Contains(res.Body, "secret") {
			t.Errorf("file_path %q: status %d body %q", p, res.StatusCode, res.Body)
		}
	}
}

func TestContentDisposition(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":           `attachment; filename="report.pdf"; filename*=UTF-8''report.pdf`,
		`a"b\c.pdf`:            `attachment; filename="a_b_c.pdf"; filename*=UTF-8''a%22b%5Cc.pdf`,
		"論文 2026.pdf":          `attachment; filename="__ 2026.pdf"; filename*=UTF-8''%E8%AB%96%E6%96%87%202026.pdf`,
		"x\r\nSet-Cookie:.pdf": `attachment; filename="x__Set-Cookie:.pdf"; filename*=UTF-8''x%0D%0ASet-Cookie%3A.pdf`,
	} {
		if got := contentDisposition(in); got != want {
			t.Errorf("contentDisposition(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAttachmentBodyLimitIsRaisedOnlyForUploads(t *testing.T) {
	env := newTestEnvWith(t, Config{MaxAttachmentSize: 25 << 20}, nil)
	writer := env.loggedIn(studentA)
	id := writer.createArticle("大きな資料", "本文")
	// 画像アップロードなどの上限（20MB）を超える資料も、添付の上限内なら受け付ける。
	big := append(append([]byte{}, pdfData...), bytes.Repeat([]byte("0"), 21<<20)...)
	expectStatus(t, writer.attach(id, "big.pdf", big, nil), http.StatusCreated)
	// 他のエンドポイントは従来どおり 20MB で打ち切る。
	res := writer.postAPI("/admin/preview", "application/x-www-form-urlencoded", bytes.NewReader(big))
	expectStatus(t, res, http.StatusRequestEntityTooLarge)
}
