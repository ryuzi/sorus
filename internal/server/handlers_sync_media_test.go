package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/internal/auth"
)

// syncMediaFor は localImages の4ファイル（原寸・大・中・小）を参照する sync_media の記録を作る。
func syncMediaFor(id string, images []SyncImage, alt, caption, tags string, created time.Time) SyncMedia {
	return SyncMedia{ID: id, Filename: id + ".jpg", PathOriginal: images[0].Name, PathLarge: images[1].Name,
		PathMedium: images[2].Name, PathSmall: images[3].Name, Width: 64, Height: 48, MimeType: "image/jpeg",
		Alt: alt, Caption: caption, AITags: tags, CreatedAt: created}
}

// syncMediaRow は公開サーバーの media の1行（検査用）。
type syncMediaRow struct {
	Filename   string  `db:"filename"`
	PathLarge  string  `db:"path_large"`
	PathSmall  string  `db:"path_small"`
	Alt        string  `db:"alt"`
	Caption    string  `db:"caption"`
	AITags     string  `db:"ai_tags"`
	MimeType   string  `db:"mime_type"`
	CreatedAt  string  `db:"created_at"`
	UploadedBy *string `db:"uploaded_by"`
}

func (e *syncEnv) mediaRow(id string) (syncMediaRow, bool) {
	e.t.Helper()
	var row syncMediaRow
	err := e.db.Get(&row, `SELECT filename, path_large, path_small, alt, caption, ai_tags, mime_type,
		strftime('%Y-%m-%d %H:%M:%S', created_at) AS created_at, uploaded_by FROM media WHERE id = ?`, id)
	if err != nil {
		return syncMediaRow{}, false
	}
	return row, true
}

// 公開サーバーのフォトギャラリーは media の行をタグで引く。sync_media は記録だけを先に受け取り、無い画像ファイルを
// Missing で返し、ファイルとともに送り直された記録を同じ ID で保存（追加・更新）する。保存した画像はトップページの
// ギャラリーに出る。
func TestSyncAPISyncMedia(t *testing.T) {
	env := newSyncEnv(t)
	tok := env.token("chief", auth.RoleEditor)
	images := localImages(t, 64, 48)
	created := time.Date(2024, 4, 1, 9, 30, 0, 0, time.UTC)
	m := syncMediaFor("0123456789abcdef0123456789abcdef", images, "校舎", "春の校舎", "学校， 景色", created)

	// 1. 記録だけでは、画像ファイルが無いため保存せず、無いファイル名を返す。
	rec := env.call(tok, SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}})
	expectSyncStatus(t, rec, http.StatusOK)
	out := decodeSync[SyncMediaResponse](t, rec)
	if out.Action != SyncActionSyncMedia || len(out.Saved) != 0 || len(out.Missing) != 4 {
		t.Fatalf("records only: %+v", out)
	}
	if _, ok := env.mediaRow(m.ID); ok {
		t.Fatal("media row was saved without its files")
	}

	// 2. ファイルを付けて送り直すと、公開サーバーのアップロード先のパスで保存する。
	rec = env.call(tok, SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}, Images: images})
	expectSyncStatus(t, rec, http.StatusOK)
	out = decodeSync[SyncMediaResponse](t, rec)
	if len(out.Saved) != 1 || out.Saved[0] != m.ID || len(out.Missing) != 0 {
		t.Fatalf("with files: %+v", out)
	}
	row, ok := env.mediaRow(m.ID)
	if !ok {
		t.Fatal("media row was not saved")
	}
	wantLarge := filepath.ToSlash(filepath.Join(env.uploadDir, images[1].Name))
	if row.PathLarge != wantLarge || row.Alt != "校舎" || row.Caption != "春の校舎" || row.AITags != "学校, 景色" ||
		row.MimeType != "image/jpeg" || row.CreatedAt != "2024-04-01 09:30:00" {
		t.Errorf("saved row = %+v, want path_large %q", row, wantLarge)
	}
	if row.UploadedBy == nil || !strings.HasPrefix(*row.UploadedBy, syncUserIDPrefix) {
		t.Errorf("uploaded_by = %v, want the sync user", row.UploadedBy)
	}
	for _, img := range images {
		if _, err := os.Stat(filepath.Join(env.uploadDir, img.Name)); err != nil {
			t.Errorf("file %s: %v", img.Name, err)
		}
	}

	// 3. ファイルがそろっていれば、記録だけで更新できる（同じ ID は UPSERT）。
	m.Alt, m.Caption, m.AITags = "新校舎", "夏の校舎", "学校"
	rec = env.call(tok, SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}})
	expectSyncStatus(t, rec, http.StatusOK)
	out = decodeSync[SyncMediaResponse](t, rec)
	if len(out.Saved) != 1 || len(out.Missing) != 0 {
		t.Fatalf("update: %+v", out)
	}
	if row, _ = env.mediaRow(m.ID); row.Alt != "新校舎" || row.Caption != "夏の校舎" || row.AITags != "学校" {
		t.Errorf("updated row = %+v", row)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media`); n != 1 {
		t.Errorf("media rows = %d, want 1", n)
	}

	// 4. 公開サーバーのトップページのギャラリーに出る。
	rec = env.call(env.token("root", auth.RoleAdmin), SyncRequest{Action: SyncActionSyncSettings,
		Settings: &SyncSettings{TopGalleryTag: strPtr("学校"), TopGalleryTitle: strPtr("学校の写真")}})
	expectSyncStatus(t, rec, http.StatusOK)
	front := env.get("/")
	expectSyncStatus(t, front, http.StatusOK)
	expectContains(t, front.Body.String(), `<h2 id="front-gallery-title" class="page-title">学校の写真</h2>`,
		`data-gallery-tag="学校"`, `alt="新校舎"`, `夏の校舎`, UploadURLPrefix+images[1].Name)
	expectSyncStatus(t, env.get(UploadURLPrefix+images[1].Name), http.StatusOK)
}

func TestSyncAPISyncMediaValidation(t *testing.T) {
	env := newSyncEnv(t)
	tok := env.token("chief", auth.RoleEditor)
	images := localImages(t, 40, 30)
	valid := func() SyncMedia {
		return syncMediaFor("abcdefabcdefabcdefabcdefabcdef01", images, "", "", "学校", time.Now())
	}
	other := localImages(t, 20, 10)

	// 記録の追加・更新は公開サイトのギャラリーを変えるため、ライターのトークンでは断る。
	expectSyncStatus(t, env.call(env.token("yamada", auth.RoleWriter),
		SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{valid()}}), http.StatusForbidden)

	cases := []struct {
		name   string
		req    func() SyncRequest
		status int
	}{
		{"no media", func() SyncRequest { return SyncRequest{Action: SyncActionSyncMedia} }, http.StatusUnprocessableEntity},
		{"bad id", func() SyncRequest {
			m := valid()
			m.ID = "../etc"
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}}
		}, http.StatusUnprocessableEntity},
		{"duplicate id", func() SyncRequest {
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{valid(), valid()}}
		}, http.StatusUnprocessableEntity},
		{"path with directory", func() SyncRequest {
			m := valid()
			m.PathLarge = "../" + m.PathLarge
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}}
		}, http.StatusUnprocessableEntity},
		{"no files", func() SyncRequest {
			m := valid()
			m.PathOriginal, m.PathLarge, m.PathMedium, m.PathSmall = "", "", "", ""
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}}
		}, http.StatusUnprocessableEntity},
		{"not an image mime type", func() SyncRequest {
			m := valid()
			m.MimeType = "text/html"
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}}
		}, http.StatusUnprocessableEntity},
		{"too long alt", func() SyncRequest {
			m := valid()
			m.Alt = strings.Repeat("あ", maxMediaAltRunes+1)
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{m}}
		}, http.StatusUnprocessableEntity},
		{"too many records", func() SyncRequest {
			media := make([]SyncMedia, maxSyncMediaItems+1)
			return SyncRequest{Action: SyncActionSyncMedia, Media: media}
		}, http.StatusUnprocessableEntity},
		{"unreferenced image", func() SyncRequest {
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{valid()}, Images: other[:1]}
		}, http.StatusUnprocessableEntity},
		{"not webp", func() SyncRequest {
			return SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{valid()},
				Images: []SyncImage{{Name: images[0].Name, Data: pngBytes(t, 4, 4)}}}
		}, http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.call(tok, tc.req())
			expectSyncStatus(t, rec, tc.status)
		})
	}
	// 断った要求では、記録もファイルも保存しない。
	if n := env.countRows(`SELECT COUNT(*) FROM media`); n != 0 {
		t.Errorf("media rows = %d, want 0", n)
	}
	if files := env.uploadedFiles(); len(files) != 0 {
		t.Errorf("uploaded files = %v, want none", files)
	}
}
