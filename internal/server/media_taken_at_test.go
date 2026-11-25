package server

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sorus/internal/database"
	"sorus/internal/image"
)

// v12（taken_at の無い media）の DB を最新へ移行すると、taken_at が足され、既存の行は登録日時で埋まる。
// 列が先に（手作業などで）足されている DB でも移行は失敗せず、入っている撮影日時は変えない。
func TestMediaTakenAtMigrationBackfill(t *testing.T) {
	for _, preAdded := range []bool{false, true} {
		db, err := database.Connect(filepath.Join(t.TempDir(), "v12.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := database.MigrateTo(db, 12); err != nil {
			t.Fatalf("MigrateTo(12): %v", err)
		}
		for _, row := range [][2]string{{"old-a", "2020-05-01 09:00:00"}, {"old-b", "2023-01-15 12:30:00"}} {
			if _, err := db.Exec(`INSERT INTO media (id, filename, path_original, mime_type, created_at)
				VALUES (?, ?, '', 'image/webp', ?)`, row[0], row[0]+".webp", row[1]); err != nil {
				t.Fatal(err)
			}
		}
		if preAdded {
			if _, err := db.Exec(`ALTER TABLE media ADD COLUMN taken_at DATETIME`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE media SET taken_at = '2001-02-03 04:05:06' WHERE id = 'old-b'`); err != nil {
				t.Fatal(err)
			}
		}

		if err := database.Migrate(db); err != nil {
			t.Fatalf("preAdded=%v: Migrate: %v", preAdded, err)
		}
		if err := database.Migrate(db); err != nil { // 2 回目は何もしない
			t.Fatalf("preAdded=%v: second Migrate: %v", preAdded, err)
		}
		want := map[string]string{"old-a": "2020-05-01 09:00:00", "old-b": "2023-01-15 12:30:00"}
		if preAdded {
			want["old-b"] = "2001-02-03 04:05:06"
		}
		for id, w := range want {
			var got string
			if err := db.Get(&got, `SELECT strftime('%Y-%m-%d %H:%M:%S', taken_at) FROM media WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
			if got != w {
				t.Errorf("preAdded=%v: %s taken_at = %q, want %q", preAdded, id, got, w)
			}
		}
		var idx int
		if err := db.Get(&idx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_media_taken_at'`); err != nil || idx != 1 {
			t.Errorf("preAdded=%v: idx_media_taken_at = %d (%v)", preAdded, idx, err)
		}
	}
}

// seedSortMedia は撮影日時と登録日時の順が異なる 4 枚を足す。nulltaken は taken_at の無い行（取り込み・同期で
// 足した画像など）で、登録日時で並ぶ。
//
//	id         taken_at              created_at
//	shot2018   2018-06-01            2024-03-01
//	shot2022   2022-08-10            2024-01-01
//	nulltaken  (NULL)                2021-01-01
//	shot2024   2024-02-01            2023-12-01
func seedSortMedia(env *testEnv) {
	env.t.Helper()
	for _, row := range []struct{ id, taken, created string }{
		{"shot2018", "2018-06-01 10:00:00", "2024-03-01 10:00:00"},
		{"shot2022", "2022-08-10 10:00:00", "2024-01-01 10:00:00"},
		{"nulltaken", "", "2021-01-01 10:00:00"},
		{"shot2024", "2024-02-01 10:00:00", "2023-12-01 10:00:00"},
	} {
		env.insertMedia(row.id, row.id+".webp", row.id, "", "trip")
		var taken any
		if row.taken != "" {
			taken = row.taken
		}
		if _, err := env.db.Exec(`UPDATE media SET taken_at = ?, created_at = ? WHERE id = ?`, taken, row.created, row.id); err != nil {
			env.t.Fatal(err)
		}
	}
}

func TestMediaSortOrders(t *testing.T) {
	env := newTestEnv(t)
	seedSortMedia(env)
	c := env.loggedIn(studentA)

	listIDs := func(path string) []string {
		t.Helper()
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		var listed mediaListResponse
		decodeJSON(t, res.Body, &listed)
		var ids []string
		for _, it := range listed.Items {
			ids = append(ids, it.ID)
		}
		return ids
	}
	takenDesc := []string{"shot2024", "shot2022", "nulltaken", "shot2018"}
	for _, tc := range []struct {
		sort string
		want []string
	}{
		{"", takenDesc},
		{"taken_desc", takenDesc},
		{"taken_asc", []string{"shot2018", "nulltaken", "shot2022", "shot2024"}},
		{"created_desc", []string{"shot2018", "shot2022", "shot2024", "nulltaken"}},
		{"bogus; DROP TABLE media", takenDesc},
		{"m.filename", takenDesc},
	} {
		path := "/admin/api/media?per_page=60&sort=" + strings.ReplaceAll(tc.sort, " ", "+")
		if got := listIDs(path); !slices.Equal(got, tc.want) {
			t.Errorf("sort=%q: %v, want %v", tc.sort, got, tc.want)
		}
	}
	// 並び順はタグ・検索の絞り込みと組み合わせられる。
	if got := listIDs("/admin/api/media?tag=trip&q=shot&sort=taken_asc"); !slices.Equal(got, []string{"shot2018", "shot2022", "shot2024"}) {
		t.Errorf("filtered taken_asc: %v", got)
	}

	// 同じ撮影日時なら、後に登録した画像が新しい順の先に来る（rowid）。
	env2 := newTestEnv(t)
	env2.insertMedia("first", "first.webp", "", "", "")
	env2.insertMedia("second", "second.webp", "", "", "")
	if _, err := env2.db.Exec(`UPDATE media SET taken_at = '2020-01-01 00:00:00', created_at = '2020-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	c2 := env2.loggedIn(studentA)
	for sort, want := range map[string][]string{"": {"second", "first"}, "taken_asc": {"first", "second"}, "created_desc": {"second", "first"}} {
		res := c2.get("/admin/api/media?sort=" + sort)
		var listed mediaListResponse
		decodeJSON(t, res.Body, &listed)
		var got []string
		for _, it := range listed.Items {
			got = append(got, it.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("tie sort=%q: %v, want %v", sort, got, want)
		}
	}
}

// 一覧の画面は並び順の選択肢を出し、選んだ並び順をページ送り・タグの絞り込みのリンクに残す。
func TestMediaSortSelectorPage(t *testing.T) {
	env := newTestEnv(t)
	seedSortMedia(env)
	for i := range mediaPerPage {
		env.insertMedia("bulk-"+strings.Repeat("x", i+1), "bulk.webp", "", "", "bulk")
	}
	c := env.loggedIn(studentA)

	def := c.get("/admin/media")
	expectStatus(t, def, http.StatusOK)
	expectContains(t, def.Body, `<select id="media-sort" name="sort"`, `<option value="taken_desc" selected>新しい順（撮影・記事日）</option>`,
		`<option value="taken_asc">古い順</option>`, `<option value="created_desc">登録が新しい順</option>`)
	expectNotContains(t, def.Body, "sort=")

	page := c.get("/admin/media?sort=taken_asc&q=")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `<option value="taken_asc" selected>古い順</option>`,
		`id="media-next"`, hrefAttr("/admin/media?page=2&sort=taken_asc"),
		hrefAttr("/admin/media?sort=taken_asc&tag=trip"), hrefAttr("/admin/media?sort=taken_asc&untagged=1"))
	// 古い順の 1 枚目は 2018 年の写真。
	if i, j := strings.Index(page.Body, `alt="shot2018"`), strings.Index(page.Body, `alt="shot2024"`); i < 0 || (j >= 0 && j < i) {
		t.Errorf("taken_asc page: shot2018 at %d, shot2024 at %d", i, j)
	}
	second := c.get("/admin/media?sort=taken_asc&page=2")
	expectContains(t, second.Body, `id="media-prev"`, hrefAttr("/admin/media?sort=taken_asc"))

	// 知らない並び順は既定（撮影日の新しい順）として扱い、リンクにも残さない。
	bad := c.get("/admin/media?sort=evil")
	expectStatus(t, bad, http.StatusOK)
	expectContains(t, bad.Body, `<option value="taken_desc" selected>`)
	expectNotContains(t, bad.Body, "sort=evil")
}

// アップロードした画像には撮影日時が入る。EXIF の撮影日時が無い PNG はアップロードした時刻（UTC）。
func TestMediaUploadSetsTakenAt(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	before := time.Now().UTC().Add(-time.Second)
	m := c.mustUpload("now.png", "", "")
	var taken string
	if err := env.db.Get(&taken, `SELECT strftime('%Y-%m-%d %H:%M:%S', taken_at) FROM media WHERE id = ?`, m.ID); err != nil {
		t.Fatal(err)
	}
	got, err := time.Parse(image.TakenAtLayout, taken)
	if err != nil {
		t.Fatalf("taken_at %q: %v", taken, err)
	}
	if got.Before(before.Truncate(time.Second)) || got.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("taken_at = %v, want about now (%v)", got, before)
	}
}
