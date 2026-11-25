package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"sorus/internal/ai"
	"sorus/internal/markdown"
)

// uploadWithMeta は代替テキスト・キャプション付きで画像をアップロードし、レスポンスを返す。
func (c *client) uploadWithMeta(filename string, data []byte, alt, caption string) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("alt", alt)
	mw.WriteField("caption", caption)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		c.t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	return c.postAPI("/admin/media/upload", mw.FormDataContentType(), &buf)
}

// mustUpload は画像をアップロードし、そのレスポンスを返す。
func (c *client) mustUpload(filename, alt, caption string) mediaResponse {
	c.t.Helper()
	res := c.uploadWithMeta(filename, pngBytes(c.t, 64, 48), alt, caption)
	expectStatus(c.t, res, http.StatusOK)
	var m mediaResponse
	if err := json.Unmarshal([]byte(res.Body), &m); err != nil {
		c.t.Fatalf("decode %s: %v", res.Body, err)
	}
	return m
}

// mediaFiles は画像 m の保存ファイル（原寸・各サイズ）のパスを返す。
func (e *testEnv) mediaFiles(m mediaResponse) []string {
	var files []string
	for _, u := range []string{m.OriginalURL, m.LargeURL, m.MediumURL, m.SmallURL} {
		files = append(files, filepath.Join(e.uploadDir, strings.TrimPrefix(u, UploadURLPrefix)))
	}
	return files
}

// insertMedia は画像ファイルを作らずに media の行だけを足す（一覧・ページ送りの検査用）。
func (e *testEnv) insertMedia(id, filename, alt, caption, tags string) {
	e.t.Helper()
	if _, err := e.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, path_medium, path_small,
			width, height, mime_type, uploaded_by, alt, caption, ai_tags)
		VALUES (?, ?, ?, ?, ?, ?, 100, 80, 'image/webp', ?, ?, ?, ?)`,
		id, filename, e.uploadDir+"/"+id+"-original.webp", e.uploadDir+"/"+id+"-1200.webp",
		e.uploadDir+"/"+id+"-800.webp", e.uploadDir+"/"+id+"-400.webp", studentA.ID, alt, caption, tags); err != nil {
		e.t.Fatalf("insert media: %v", err)
	}
}

func countCards(body string) int { return strings.Count(body, `class="media-card `) }

func TestMediaUploadStoresAltAndCaption(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)

	m := c.mustUpload("library.png", "図書館[閲覧室]", "  陽光が差し込む \"閲覧スペース\"  ")
	if m.Alt != "図書館閲覧室" || m.Caption != `陽光が差し込む "閲覧スペース"` {
		t.Fatalf("alt = %q, caption = %q", m.Alt, m.Caption)
	}
	// キャプションは画像のタイトルとして添え、タイトルを壊す引用符は除く。
	if want := `![図書館閲覧室](` + m.LargeURL + ` "陽光が差し込む 閲覧スペース")`; m.Markdown != want {
		t.Errorf("markdown = %q, want %q", m.Markdown, want)
	}
	var row struct{ Alt, Caption, Tags string }
	if err := env.db.QueryRow(`SELECT alt, caption, ai_tags FROM media WHERE id = ?`, m.ID).Scan(&row.Alt, &row.Caption, &row.Tags); err != nil {
		t.Fatal(err)
	}
	if row.Alt != "図書館閲覧室" || row.Caption != `陽光が差し込む "閲覧スペース"` || row.Tags != "" {
		t.Errorf("stored row = %+v", row)
	}

	// 代替テキストを省略した画像は、保存値は空のまま（AI・手入力で後から補う）、挿入用にはファイル名を使う。
	m = c.mustUpload("gate.png", "", "")
	if m.Alt != "gate" || m.Markdown != "![gate]("+m.LargeURL+")" {
		t.Errorf("alt = %q, markdown = %q", m.Alt, m.Markdown)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '' AND caption = ''`, m.ID); n != 1 {
		t.Errorf("media without alt rows = %d, want 1", n)
	}

	// 長すぎる代替テキストは画像を保存せずに 400。
	files, _ := os.ReadDir(env.uploadDir)
	res := c.uploadWithMeta("long.png", pngBytes(t, 8, 8), strings.Repeat("あ", maxMediaAltRunes+1), "")
	expectStatus(t, res, http.StatusBadRequest)
	if after, _ := os.ReadDir(env.uploadDir); len(after) != len(files) {
		t.Errorf("upload dir has %d files after rejected upload, want %d", len(after), len(files))
	}
}

func TestMediaBulkTags(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	env.insertMedia("bulk-a", "a.webp", "", "", "existing, remove")
	env.insertMedia("bulk-b", "b.webp", "", "", "existing")
	post := func(body string) *response {
		return c.postAPI("/admin/api/media/bulk-tags", "application/json", strings.NewReader(body))
	}
	res := post(`{"media_ids":["bulk-a","bulk-b"],"add_tags":["event","旅行","ラーメン","イベント 2026","コーヒー","サッカー"],"remove_tags":["remove"]}`)
	expectStatus(t, res, http.StatusOK)
	var got struct {
		OK      bool `json:"ok"`
		Updated int  `json:"updated_count"`
	}
	if err := json.Unmarshal([]byte(res.Body), &got); err != nil || !got.OK || got.Updated != 2 {
		t.Fatalf("response = %s (%v)", res.Body, err)
	}
	var a, b string
	if err := env.db.QueryRow(`SELECT ai_tags FROM media WHERE id='bulk-a'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(`SELECT ai_tags FROM media WHERE id='bulk-b'`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	wantTags := "existing, event, 旅行, ラーメン, イベント 2026, コーヒー, サッカー"
	if a != wantTags || b != wantTags {
		t.Fatalf("tags = %q and %q", a, b)
	}

	res = post(`{"media_ids":["bulk-a"],"add_tags":[],"remove_tags":["event"]}`)
	expectStatus(t, res, http.StatusOK)
	if err := env.db.QueryRow(`SELECT ai_tags FROM media WHERE id='bulk-a'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if a != "existing, 旅行, ラーメン, イベント 2026, コーヒー, サッカー" {
		t.Fatalf("removed tag, tags = %q", a)
	}
	for _, body := range []string{
		`{"media_ids":["bulk-a"],"add_tags":[],"remove_tags":[]}`,
		`{"media_ids":["bulk-a"],"add_tags":["bad\"tag"]}`,
		`{"media_ids":["bulk-a"],"add_tags":["bad,tag"]}`,
		`{"media_ids":[],"add_tags":["x"]}`,
	} {
		expectStatus(t, post(body), http.StatusBadRequest)
	}
	csrfReq, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/admin/api/media/bulk-tags", strings.NewReader(`{"media_ids":["bulk-a"],"add_tags":["x"]}`))
	csrfReq.Header.Set("Content-Type", "application/json")
	expectStatus(t, c.do(csrfReq), http.StatusForbidden)
}

func TestMediaLibraryListAndSearch(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	campus := c.mustUpload("campus.png", "春の校舎", "正門から見た本館")
	c.mustUpload("lab.png", "研究室の実験装置", "")
	env.insertMedia("tagged", "seminar.webp", "", "", "ゼミ, 発表")

	res := c.get("/admin/media")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "メディアライブラリ", `id="media-grid"`, "campus.png", "lab.png", "seminar.webp",
		`data-alt="春の校舎"`, `data-caption="正門から見た本館"`, `data-markdown="![春の校舎](`+campus.LargeURL,
		`src="`+campus.SmallURL+`"`, `id="media-dropzone"`, `id="media-inspector"`, `href="/admin/media"`)
	if n := countCards(res.Body); n != 3 {
		t.Errorf("cards = %d, want 3", n)
	}
	// 執筆者は削除できない（削除ボタン・確認ダイアログを出さない）。
	if strings.Contains(res.Body, `id="media-delete-dialog"`) {
		t.Error("writer sees the delete dialog")
	}

	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"campus", []string{"campus.png"}}, // ファイル名
		{"実験装置", []string{"lab.png"}},      // 代替テキスト
		{"正門", []string{"campus.png"}},     // キャプション
		{"ゼミ", []string{"seminar.webp"}},   // タグ
		{"春 本館", []string{"campus.png"}},   // 複数語はすべてを含む画像
		{"春 研究室", nil},                     // どの画像も両方は含まない
		{"100%", nil},                      // LIKE のワイルドカードは文字として扱う
	} {
		res := c.get("/admin/media?q=" + url.QueryEscape(tc.q))
		expectStatus(t, res, http.StatusOK)
		if n := countCards(res.Body); n != len(tc.want) {
			t.Errorf("q=%q: cards = %d, want %d", tc.q, n, len(tc.want))
		}
		for _, w := range tc.want {
			expectContains(t, res.Body, w)
		}
		if len(tc.want) == 0 {
			expectContains(t, res.Body, `id="media-empty"`)
		}
	}
	// 検索語はエスケープして表示する。
	res = c.get("/admin/media?q=" + url.QueryEscape(`<script>x</script>`))
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "<script>x</script>") {
		t.Error("search query is rendered without escaping")
	}

	// 管理者も一覧を見られ、削除の操作とアップロード欄が表示される（管理者は執筆者・編集者の役割も持つ）。
	res = env.loggedIn(admin).get("/admin/media")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="media-delete-dialog"`, `id="media-delete-open"`, `id="media-dropzone"`)

	// 未ログインはログイン画面へ。
	expectRedirect(t, env.newClient().get("/admin/media"), "/admin/login")
}

func TestMediaTagAPIAndExactTagFilter(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	env.insertMedia("tag-land", "land.webp", "", "", "land, landscape")
	env.insertMedia("tag-landscape", "other.webp", "", "", "landscape")

	res := c.get("/admin/api/media?tag=land")
	expectStatus(t, res, http.StatusOK)
	var listed mediaListResponse
	if err := json.Unmarshal([]byte(res.Body), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != "tag-land" {
		t.Fatalf("tag filter result = %+v", listed)
	}

	res = c.get("/admin/api/media/tags")
	expectStatus(t, res, http.StatusOK)
	var tags mediaTagsResponse
	if err := json.Unmarshal([]byte(res.Body), &tags); err != nil {
		t.Fatal(err)
	}
	if want := []string{"land", "landscape"}; !slices.Equal(tags.Tags, want) {
		t.Fatalf("tags = %v, want %v", tags.Tags, want)
	}
}

func TestGalleryResolverReflectsNewMediaTag(t *testing.T) {
	env := newTestEnv(t)
	resolver := env.server.galleryResolver()
	images, err := resolver("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Fatalf("initial images = %v", images)
	}
	env.insertMedia("fresh-photo", "fresh.webp", "Fresh view", "Morning light", "fresh, view")
	images, err = resolver("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Alt != "Fresh view" || images[0].Caption != "Morning light" {
		t.Fatalf("resolved images = %+v", images)
	}
}

func TestGalleryQueryResolverAppliesLimitAndSort(t *testing.T) {
	env := newTestEnv(t)
	for _, id := range []string{"first", "second", "third"} {
		env.insertMedia(id, id+".webp", id, "", "options")
	}
	resolve := env.server.galleryQueryResolver()
	newest, err := resolve(markdown.GalleryQuery{Tag: "options", Limit: 1, Sort: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) != 1 || newest[0].Alt != "third" {
		t.Fatalf("latest limited images = %+v", newest)
	}
	oldest, err := resolve(markdown.GalleryQuery{Tag: "options", Sort: "oldest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldest) != 3 || oldest[0].Alt != "first" || oldest[2].Alt != "third" {
		t.Fatalf("oldest images = %+v", oldest)
	}
}

func TestMediaLibraryPagination(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < mediaPerPage+5; i++ {
		env.insertMedia(fmt.Sprintf("m%02d", i), fmt.Sprintf("photo-%02d.webp", i), "", "", "")
	}
	c := env.loggedIn(studentA)

	res := c.get("/admin/media")
	expectStatus(t, res, http.StatusOK)
	if n := countCards(res.Body); n != mediaPerPage {
		t.Fatalf("page 1 cards = %d, want %d", n, mediaPerPage)
	}
	// 新しい（後から登録した）順に並ぶ。
	if i, j := strings.Index(res.Body, "photo-28.webp"), strings.Index(res.Body, "photo-05.webp"); i < 0 || j < 0 || i > j {
		t.Errorf("order: photo-28 at %d, photo-05 at %d", i, j)
	}
	expectContains(t, res.Body, `href="/admin/media?page=2"`, "全29件")
	if strings.Contains(res.Body, `id="media-prev"`) {
		t.Error("page 1 has a previous link")
	}

	res = c.get("/admin/media?page=2")
	expectStatus(t, res, http.StatusOK)
	if n := countCards(res.Body); n != 5 {
		t.Errorf("page 2 cards = %d, want 5", n)
	}
	expectContains(t, res.Body, "photo-00.webp", `id="media-prev"`)
	if strings.Contains(res.Body, `id="media-next"`) {
		t.Error("last page has a next link")
	}

	// 最後より後ろのページは、空である旨と最初のページへのリンクを出す。
	res = c.get("/admin/media?page=9")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="media-empty"`, "このページに画像はありません", `href="/admin/media?page=8"`)

	// 検索語はページ送りのリンクに引き継ぐ。不正なページ番号は 1 ページ目として扱う。
	res = c.get("/admin/media?q=photo&page=abc")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `href="/admin/media?page=2&amp;q=photo"`)
}

func TestMediaPickerAPI(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	campus := c.mustUpload("campus.png", "春の校舎", "正門から見た本館")
	for i := 0; i < 3; i++ {
		env.insertMedia(fmt.Sprintf("x%d", i), fmt.Sprintf("extra-%d.webp", i), "", "", "")
	}

	var list mediaListResponse
	res := c.getJSON("/admin/api/media?q="+url.QueryEscape("正門"), &list)
	expectStatus(t, res, http.StatusOK)
	if list.Total != 1 || len(list.Items) != 1 || list.HasNext || list.Page != 1 {
		t.Fatalf("list = %+v", list)
	}
	got := list.Items[0]
	if got.ID != campus.ID || got.ThumbnailURL != campus.SmallURL || got.URL != campus.LargeURL ||
		got.Alt != "春の校舎" || got.Caption != "正門から見た本館" || got.Filename != "campus.png" ||
		got.Width != 64 || got.Height != 48 {
		t.Errorf("item = %+v", got)
	}
	if want := `![春の校舎](` + campus.LargeURL + ` "正門から見た本館")`; got.Markdown != want {
		t.Errorf("markdown = %q, want %q", got.Markdown, want)
	}

	// ページ送り: per_page=2 で 4 件を 2 ページに分ける。
	res = c.getJSON("/admin/api/media?per_page=2", &list)
	expectStatus(t, res, http.StatusOK)
	if list.Total != 4 || len(list.Items) != 2 || !list.HasNext || list.PerPage != 2 {
		t.Fatalf("page 1 = %+v", list)
	}
	first := list.Items[0].ID
	res = c.getJSON("/admin/api/media?per_page=2&page=2", &list)
	expectStatus(t, res, http.StatusOK)
	if len(list.Items) != 2 || list.HasNext || list.Items[0].ID == first {
		t.Fatalf("page 2 = %+v", list)
	}
	// per_page は上限で切り詰める。
	res = c.getJSON("/admin/api/media?per_page=100000", &list)
	expectStatus(t, res, http.StatusOK)
	if list.PerPage != mediaAPIMaxPerPage {
		t.Errorf("per_page = %d, want %d", list.PerPage, mediaAPIMaxPerPage)
	}
	// 該当なしでも items は null ではなく空配列。
	res = c.getJSON("/admin/api/media?q=nothing-matches", nil)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"items":[]`)

	// 未ログインの API 呼び出しは 401。
	expectStatus(t, env.newClient().getJSON("/admin/api/media", nil), http.StatusUnauthorized)
}

func TestMediaUpdateMetadata(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "", "")

	form := url.Values{"alt": {" 正門[桜] "}, "caption": {"満開の桜\r\n入学式の朝"}, "ai_tags": {"桜、 春 ,キャンパス,春,, "}}
	res := c.postAPI("/admin/media/"+m.ID, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	expectStatus(t, res, http.StatusOK)
	var item mediaItem
	if err := json.Unmarshal([]byte(res.Body), &item); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	if item.ID != m.ID || item.Alt != "正門桜" || item.Caption != "満開の桜\n入学式の朝" || item.AITags != "桜, 春, キャンパス" {
		t.Errorf("item = %+v", item)
	}
	if want := `![正門桜](` + m.LargeURL + ` "満開の桜 入学式の朝")`; item.Markdown != want {
		t.Errorf("markdown = %q, want %q", item.Markdown, want)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '正門桜' AND ai_tags = '桜, 春, キャンパス'`, m.ID); n != 1 {
		t.Errorf("updated rows = %d, want 1", n)
	}

	// フォーム送信（JavaScript なし）は一覧へ戻る。管理者もメタデータを直せる。
	res = env.loggedIn(admin).postForm("/admin/media/"+m.ID, url.Values{"alt": {"本館"}, "caption": {""}, "ai_tags": {""}})
	expectRedirect(t, res, "/admin/media?flash=media_saved")
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '本館' AND caption = '' AND ai_tags = ''`, m.ID); n != 1 {
		t.Errorf("rows after form update = %d, want 1", n)
	}

	// 長すぎる値は保存しない。
	long := url.Values{"alt": {"x"}, "caption": {strings.Repeat("あ", maxMediaCaptionRunes+1)}}
	res = c.postAPI("/admin/media/"+m.ID, "application/x-www-form-urlencoded", strings.NewReader(long.Encode()))
	expectStatus(t, res, http.StatusBadRequest)
	if msg := jsonMessage(t, res); !strings.Contains(msg, "キャプション") {
		t.Errorf("message = %q", msg)
	}
	expectRedirect(t, c.postForm("/admin/media/"+m.ID, long), "/admin/media?error=invalid")
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '本館'`, m.ID); n != 1 {
		t.Error("too long metadata changed the row")
	}
	expectContains(t, c.get("/admin/media?error=invalid").Body, `id="media-error"`)

	// 存在しない画像は 404（フォーム送信は一覧へ戻してエラーを表示）。
	res = c.postAPI("/admin/media/missing", "application/x-www-form-urlencoded", strings.NewReader("alt=x"))
	expectStatus(t, res, http.StatusNotFound)
	expectRedirect(t, c.postForm("/admin/media/missing", url.Values{"alt": {"x"}}), "/admin/media?error=not_found")
}

func TestMediaDeleteByAdmin(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	target := writer.mustUpload("campus.png", "春の校舎", "")
	keep := writer.mustUpload("lab.png", "研究室", "")
	writer.createArticle("校舎の紹介", "本文\n\n"+target.Markdown)

	// 使用中の記事数を一覧に出し、削除の確認ダイアログで警告できるようにする。
	a := env.loggedIn(admin)
	res := a.get("/admin/media")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `data-usage="1"`, "使用中 1")

	res = a.postForm("/admin/media/"+target.ID+"/delete", nil)
	expectRedirect(t, res, "/admin/media?flash=media_deleted")
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ?`, target.ID); n != 0 {
		t.Errorf("media rows after delete = %d, want 0", n)
	}
	for _, f := range env.mediaFiles(target) {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s remains after delete (%v)", f, err)
		}
	}
	// ほかの画像の記録・ファイルは残る。
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ?`, keep.ID); n != 1 {
		t.Errorf("other media rows = %d, want 1", n)
	}
	for _, f := range env.mediaFiles(keep) {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("other media file %s: %v", f, err)
		}
	}
	expectStatus(t, a.get(target.LargeURL), http.StatusNotFound)
	expectContains(t, a.get("/admin/media?flash=media_deleted").Body, "画像を削除しました")

	// fetch からの削除は JSON で応える。消えた画像の削除は 404。
	res = a.postAPI("/admin/media/"+keep.ID+"/delete", "", nil)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"deleted":"`+keep.ID+`"`)
	expectStatus(t, a.postAPI("/admin/media/"+keep.ID+"/delete", "", nil), http.StatusNotFound)
	expectRedirect(t, a.postForm("/admin/media/"+keep.ID+"/delete", nil), "/admin/media?error=not_found")
}

func TestMediaDeleteRemovesOnlyFilesInsideUploadDir(t *testing.T) {
	env := newTestEnv(t)
	if err := os.MkdirAll(env.uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 記録のパスがアップロード先の外を指していても、外のファイルは消さない。
	outside := filepath.Join(filepath.Dir(env.uploadDir), "secret.webp")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, mime_type) VALUES (?, ?, ?, ?, ?)`,
		"evil", "evil.webp", env.uploadDir+"/../secret.webp", "../../..", "image/webp"); err != nil {
		t.Fatal(err)
	}
	// 2 つの記録が同じファイルを共有していれば、片方を消してもファイルは残す。
	shared := filepath.Join(env.uploadDir, "shared-1200.webp")
	if err := os.WriteFile(shared, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2"} {
		if _, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, mime_type) VALUES (?, ?, ?, ?)`,
			id, "shared.webp", env.uploadDir+"/shared-1200.webp", "image/webp"); err != nil {
			t.Fatal(err)
		}
	}

	a := env.loggedIn(admin)
	expectRedirect(t, a.postForm("/admin/media/evil/delete", nil), "/admin/media?flash=media_deleted")
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside the upload dir was removed: %v", err)
	}
	if _, err := os.Stat(env.uploadDir); err != nil {
		t.Errorf("upload dir was removed: %v", err)
	}

	expectRedirect(t, a.postForm("/admin/media/s1/delete", nil), "/admin/media?flash=media_deleted")
	if _, err := os.Stat(shared); err != nil {
		t.Errorf("shared file removed while another record uses it: %v", err)
	}
	expectRedirect(t, a.postForm("/admin/media/s2/delete", nil), "/admin/media?flash=media_deleted")
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Errorf("shared file remains after its last record was deleted (%v)", err)
	}
}

func TestMediaDeleteForbiddenForNonAdmins(t *testing.T) {
	env := newTestEnv(t)
	writer := env.loggedIn(studentA)
	m := writer.mustUpload("campus.png", "春の校舎", "")

	for _, u := range []testUser{studentA, chief} {
		c := env.loggedIn(u)
		expectStatus(t, c.postForm("/admin/media/"+m.ID+"/delete", nil), http.StatusForbidden)
		expectStatus(t, c.postAPI("/admin/media/"+m.ID+"/delete", "", nil), http.StatusForbidden)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ?`, m.ID); n != 1 {
		t.Errorf("media rows = %d after forbidden deletes, want 1", n)
	}
	for _, f := range env.mediaFiles(m) {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("file %s removed by a forbidden delete: %v", f, err)
		}
	}
}

func TestMediaCSRFProtection(t *testing.T) {
	env := newTestEnv(t)
	m := env.loggedIn(studentA).mustUpload("campus.png", "春の校舎", "")
	a := env.loggedIn(admin)

	// トークン欠落・不正なトークンの更新・削除は 403 で、何も変えない。
	expectStatus(t, a.postRaw("/admin/media/"+m.ID, url.Values{"alt": {"改ざん"}}), http.StatusForbidden)
	expectStatus(t, a.postRaw("/admin/media/"+m.ID, url.Values{"alt": {"改ざん"}, CSRFFormField: {"forged"}}), http.StatusForbidden)
	expectStatus(t, a.postRaw("/admin/media/"+m.ID+"/delete", nil), http.StatusForbidden)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/admin/media/"+m.ID+"/delete", nil)
	req.Header.Set(CSRFHeaderName, "forged-token")
	req.Header.Set("Accept", "application/json")
	expectStatus(t, a.do(req), http.StatusForbidden)

	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '春の校舎'`, m.ID); n != 1 {
		t.Errorf("media changed by requests without a valid CSRF token")
	}
	for _, f := range env.mediaFiles(m) {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("file %s removed without a valid CSRF token: %v", f, err)
		}
	}
}

func TestArticleEditorMediaPickerAndFeaturedImage(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "春の校舎", "")

	res := c.get("/admin/articles/new")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="media-picker"`, `data-api="/admin/api/media"`, `data-media-picker-open="insert"`,
		`data-media-picker-open="featured"`, `name="featured_image_url"`, `/static/js/media_picker.js`)

	id := c.createArticle("アイキャッチ付き", "本文")
	save := func(featured *string) *response {
		form := url.Values{"title": {"アイキャッチ付き"}, "category_id": {"1"}, "content_markdown": {"本文"}, "action": {"draft"}}
		if featured != nil {
			form.Set("featured_image_url", *featured)
		}
		return c.postForm("/admin/articles/"+id, form)
	}
	url1 := m.LargeURL
	expectRedirect(t, save(&url1), "/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id).FeaturedImage; got != m.LargeURL {
		t.Fatalf("featured image = %q, want %q", got, m.LargeURL)
	}
	expectContains(t, c.get("/admin/articles/"+id+"/edit").Body, `value="`+m.LargeURL+`"`, `id="featured-image-preview" alt="" src="`+m.LargeURL+`"`)

	// 入力欄の無い送信（古い画面・API 的な送信）は既存のアイキャッチを消さない。
	expectRedirect(t, save(nil), "/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id).FeaturedImage; got != m.LargeURL {
		t.Errorf("featured image after save without the field = %q, want kept", got)
	}

	// 危険な URL は保存しない。
	for _, bad := range []string{"javascript:alert(1)", "//evil.example/x.png", "/uploads/a.webp\" onerror=\"x"} {
		v := bad
		res := save(&v)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "アイキャッチ画像の URL が正しくありません")
		if got := env.mustGetArticle(id).FeaturedImage; got != m.LargeURL {
			t.Errorf("featured image after %q = %q", bad, got)
		}
	}

	// 空にすると外れる。外部の http(s) URL も設定できる。
	empty := ""
	expectRedirect(t, save(&empty), "/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id).FeaturedImage; got != "" {
		t.Errorf("featured image after clear = %q", got)
	}
	ext := "https://cdn.example.ac.jp/photo.webp"
	expectRedirect(t, save(&ext), "/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id).FeaturedImage; got != ext {
		t.Errorf("featured image = %q, want %q", got, ext)
	}
}

// fakeVisionAI は画像の解析に対応した偽の AI。受け取った画像の MIME タイプと大きさ・既存タグの候補を記録し、
// 決まった結果（または err）を返す。
type fakeVisionAI struct {
	fakeAI
	mu     sync.Mutex
	mimes  []string
	sizes  []int
	hints  [][]string
	result *ai.ImageAnalysis
	err    error
}

func (f *fakeVisionAI) AnalyzeImage(_ context.Context, data []byte, mimeType string, existingTags []string) (*ai.ImageAnalysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mimes = append(f.mimes, mimeType)
	f.sizes = append(f.sizes, len(data))
	f.hints = append(f.hints, existingTags)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeVisionAI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mimes)
}

func analyzePath(id string) string { return "/admin/api/media/" + id + "/analyze" }

func TestMediaAnalyzeWithAI(t *testing.T) {
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{
		Alt:     "満開の桜[正門]",
		Caption: "入学式の朝\r\n正門前の桜並木",
		Tags:    []string{"桜", "春", "キャンパス", "桜"},
	}}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "", "")
	c.createArticle("入学式", "本文\n\n"+m.Markdown)

	res := c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusOK)
	var item mediaItem
	if err := json.Unmarshal([]byte(res.Body), &item); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	// 手入力と同じ規則で整える（代替テキストから Markdown を壊す文字を除き、改行とタグの重複を整える）。
	if item.ID != m.ID || item.Alt != "満開の桜正門" || item.Caption != "入学式の朝\n正門前の桜並木" || item.AITags != "桜, 春, キャンパス" {
		t.Errorf("item = %+v", item)
	}
	if want := `![満開の桜正門](` + m.LargeURL + ` "入学式の朝 正門前の桜並木")`; item.Markdown != want {
		t.Errorf("markdown = %q, want %q", item.Markdown, want)
	}
	if item.UsageCount != 1 {
		t.Errorf("usage count = %d, want 1", item.UsageCount)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '満開の桜正門' AND caption = ? AND ai_tags = '桜, 春, キャンパス'`,
		m.ID, "入学式の朝\n正門前の桜並木"); n != 1 {
		t.Errorf("updated rows = %d, want 1", n)
	}
	// AI には保存した大きいサイズの WebP を送る。
	large, err := os.ReadFile(env.mediaFiles(m)[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.mimes) != 1 || fake.mimes[0] != "image/webp" || fake.sizes[0] != len(large) {
		t.Errorf("analyzer got mimes=%v sizes=%v, want image/webp %d bytes", fake.mimes, fake.sizes, len(large))
	}

	// 一覧の詳細ダイアログに自動生成ボタンを出し、設定の案内は出さない。
	page := c.get("/admin/media")
	expectContains(t, page.Body, `id="media-ai-analyze"`, `data-ai-ready="true"`, "AI自動生成", `data-analyzing=`)
	if strings.Contains(page.Body, `id="media-ai-hint"`) {
		t.Error("setup hint is shown although the AI can analyze images")
	}
}

func TestMediaAnalyzeTruncatesLongAIOutput(t *testing.T) {
	var tags []string
	for i := 0; i < 60; i++ {
		tags = append(tags, fmt.Sprintf("タグ%02d", i))
	}
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{
		Alt:     strings.Repeat("あ", maxMediaAltRunes+50),
		Caption: strings.Repeat("い", maxMediaCaptionRunes+50),
		Tags:    tags,
	}}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "", "")

	res := c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusOK)
	var item mediaItem
	if err := json.Unmarshal([]byte(res.Body), &item); err != nil {
		t.Fatal(err)
	}
	if item.Alt != strings.Repeat("あ", maxMediaAltRunes) || item.Caption != strings.Repeat("い", maxMediaCaptionRunes) {
		t.Errorf("alt = %d runes, caption = %d runes", len([]rune(item.Alt)), len([]rune(item.Caption)))
	}
	// 合計の上限を超えるタグは途中で切らずに捨てる。
	if n := len([]rune(item.AITags)); n > maxMediaTagsRunes || !strings.HasPrefix(item.AITags, "タグ00, タグ01") || strings.HasSuffix(item.AITags, ",") {
		t.Errorf("tags = %q (%d runes)", item.AITags, n)
	}
	for _, tag := range strings.Split(item.AITags, ", ") {
		if len([]rune(tag)) != 4 {
			t.Errorf("tag %q was cut", tag)
		}
	}
}

func TestMediaAnalyzeUnconfiguredAI(t *testing.T) {
	// AI 未設定（Dummy）では AI を呼ばず、設定方法を添えたエラーを返す。保存済みの値は変えない。
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "春の校舎", "")

	res := c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusServiceUnavailable)
	if msg := jsonMessage(t, res); !strings.Contains(msg, "AIが設定されていない") || !strings.Contains(msg, "GEMINI_API_KEY") || strings.Contains(msg, "`") {
		t.Errorf("message = %q", msg)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '春の校舎'`, m.ID); n != 1 {
		t.Error("media changed without a configured AI")
	}
	expectContains(t, c.get("/admin/media").Body, `id="media-ai-analyze"`, `data-ai-ready="false"`, `id="media-ai-hint"`)

	// 画像を扱えないプロバイダー（AnalyzeImage が無い）は対応していないことを伝える。
	env = newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	c = env.loggedIn(studentA)
	m = c.mustUpload("campus.png", "", "")
	res = c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusNotImplemented)
	if msg := jsonMessage(t, res); !strings.Contains(msg, "画像の解析に対応していません") {
		t.Errorf("message = %q", msg)
	}

	// 解析中に ErrNotConfigured が返っても設定の案内にする。
	fake := &fakeVisionAI{err: ai.ErrNotConfigured}
	env = newTestEnvWith(t, Config{AI: fake}, nil)
	c = env.loggedIn(studentA)
	m = c.mustUpload("campus.png", "", "")
	res = c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusServiceUnavailable)
	expectContains(t, jsonMessage(t, res), "AIが設定されていない")
}

func TestMediaAnalyzeErrors(t *testing.T) {
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{Alt: "x"}}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)

	// 存在しない画像は 404 で、AI を呼ばない。
	res := c.postAPI(analyzePath("missing"), "", nil)
	expectStatus(t, res, http.StatusNotFound)
	expectContains(t, jsonMessage(t, res), "画像が見つかりません")

	// 記録はあるがファイルが無い画像、アップロード先の外を指す画像は 404。
	env.insertMedia("nofile", "gone.webp", "元の代替テキスト", "", "")
	if err := os.WriteFile(filepath.Join(filepath.Dir(env.uploadDir), "secret.webp"), []byte("RIFF0000WEBPVP8 "), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, mime_type) VALUES (?, ?, ?, ?, ?)`,
		"evil", "evil.webp", env.uploadDir+"/../secret.webp", "../../..", "image/webp"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"nofile", "evil"} {
		res = c.postAPI(analyzePath(id), "", nil)
		expectStatus(t, res, http.StatusNotFound)
		expectContains(t, jsonMessage(t, res), "画像ファイルが見つからない")
	}
	if fake.calls() != 0 {
		t.Errorf("analyzer called %d times for missing images", fake.calls())
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = 'nofile' AND alt = '元の代替テキスト'`); n != 1 {
		t.Error("media without a file changed")
	}

	// AI の失敗は学生に分かる文言で返し、保存済みの値は変えない。
	m := c.mustUpload("campus.png", "春の校舎", "")
	for _, tc := range []struct {
		err  error
		code int
		want string
	}{
		{&ai.APIError{Provider: "Gemini", StatusCode: http.StatusTooManyRequests}, http.StatusServiceUnavailable, "利用上限"},
		{ai.ErrEmptyResponse, http.StatusBadGateway, "有効な応答がありませんでした"},
		{ai.ErrInvalidImage, http.StatusUnprocessableEntity, "解析できない形式"},
		{errors.New("boom"), http.StatusBadGateway, "AIの応答を取得できませんでした"},
	} {
		fake.err = tc.err
		res := c.postAPI(analyzePath(m.ID), "", nil)
		expectStatus(t, res, tc.code)
		if msg := jsonMessage(t, res); !strings.Contains(msg, tc.want) || strings.Contains(msg, "boom") {
			t.Errorf("%v: message = %q, want %q", tc.err, msg, tc.want)
		}
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '春の校舎'`, m.ID); n != 1 {
		t.Error("media changed by a failed analysis")
	}
}

func TestMediaAnalyzeRequiresLoginAndCSRF(t *testing.T) {
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{Alt: "改ざん"}}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)
	m := c.mustUpload("campus.png", "春の校舎", "")

	// 未ログインは（CSRF トークンがあっても）401。
	expectStatus(t, env.newClient().postAPI(analyzePath(m.ID), "", nil), http.StatusUnauthorized)

	// トークン欠落・不正なトークンは 403。
	expectStatus(t, c.postRaw(analyzePath(m.ID), nil), http.StatusForbidden)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+analyzePath(m.ID), nil)
	req.Header.Set(CSRFHeaderName, "forged-token")
	req.Header.Set("Accept", "application/json")
	expectStatus(t, c.do(req), http.StatusForbidden)

	// GET では解析しない。
	if res := c.get(analyzePath(m.ID)); res.StatusCode == http.StatusOK {
		t.Errorf("GET analyze: status %d", res.StatusCode)
	}

	if fake.calls() != 0 {
		t.Errorf("analyzer called %d times without a valid session or CSRF token", fake.calls())
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND alt = '春の校舎'`, m.ID); n != 1 {
		t.Error("media changed by requests without a valid session or CSRF token")
	}
}
