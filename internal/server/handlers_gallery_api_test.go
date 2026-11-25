package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/markdown"
)

// galleryEnv は記事・ページの :::gallery のための環境。本文 bodies を公開中の記事として、draft を下書きとして入れる。
func galleryEnv(t *testing.T, published map[string]string, draft string) *testEnv {
	t.Helper()
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		for slug, body := range published {
			seedArticle(t, db, article.Article{Title: slug, Slug: slug, ContentMarkdown: body,
				Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
		}
		if draft != "" {
			seedArticle(t, db, article.Article{Title: "下書き", Slug: "draft-post", ContentMarkdown: draft, Status: article.StatusDraft})
		}
	})
	return env
}

// insertGalleryMedia はタグ tag の写真を n 枚入れる（後から入れたものほど新しい）。幅は i が偶数なら wide、奇数なら narrow。
func insertGalleryMedia(env *testEnv, tag string, n, wide, narrow int) {
	env.t.Helper()
	for i := range n {
		id := fmt.Sprintf("%s-%03d", tag, i)
		env.insertMedia(id, id+".webp", fmt.Sprintf("写真%03d", i), "", tag)
		w := wide
		if i%2 == 1 {
			w = narrow
		}
		if _, err := env.db.Exec(`UPDATE media SET width = ?, height = 600 WHERE id = ?`, w, id); err != nil {
			env.t.Fatal(err)
		}
	}
}

var galleryAltPattern = regexp.MustCompile(`alt="(写真\d{3})"`)

func galleryAlts(html string) []string {
	var out []string
	for _, m := range galleryAltPattern.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

func getGalleryAPI(t *testing.T, c *client, query string) galleryAPIResponse {
	t.Helper()
	res := c.get(galleryAPIPath + "?" + query)
	expectStatus(t, res, http.StatusOK)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var out galleryAPIResponse
	if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	return out
}

// limit を書かない :::gallery は最初の 24 枚だけを出し、総数・続きの API・「もっと見る（残り N 件）」を出す。
// 続きの API は残りを重ならずに返し、合わせるとタグの写真がすべてそろう。
func TestPublicGalleryDefaultLimitAndMoreAPI(t *testing.T) {
	env := galleryEnv(t, map[string]string{"album": "# アルバム\n\n:::gallery tag=\"album\"\n:::\n"}, "")
	insertGalleryMedia(env, "album", 30, 1200, 1200)
	c := env.newClient()

	res := c.get("/articles/album")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`data-gallery-tag="album" data-gallery-total="30" data-gallery-more="/api/gallery?limit=24&amp;tag=album">`,
		`<p class="gallery-more"><span class="gallery-count">全 30 枚</span><button type="button" class="gallery-more-button">もっと見る（残り 6 件）</button></p>`)
	first := galleryAlts(res.Body)
	if len(first) != markdown.DefaultGalleryPageSize || first[0] != "写真029" {
		t.Fatalf("first page = %v", first)
	}
	if strings.Contains(res.Body, `hidden=""><img`) {
		t.Error("the server-rendered gallery has hidden items although the API serves the rest")
	}

	more := getGalleryAPI(t, c, "tag=album&limit=24&offset=24")
	if more.Count != 6 || more.Total != 30 || more.NextOffset != 30 || more.Remaining != 0 {
		t.Fatalf("more = %+v", more)
	}
	rest := galleryAlts(more.HTML)
	if len(rest) != 6 || rest[0] != "写真005" || rest[5] != "写真000" {
		t.Fatalf("rest = %v", rest)
	}
	if n := strings.Count(more.HTML, `<div class="gallery-item" tabindex="0" role="button"`); n != 6 {
		t.Errorf("items in API html = %d", n)
	}
	expectContains(t, more.HTML, `src="/uploads/album-005-800.webp" data-full="/uploads/album-005-1200.webp" width="1200" height="600"`)

	// 途中から少しずつ読んでも重ならない。
	mid := getGalleryAPI(t, c, "tag=album&limit=4&offset=20")
	if got := galleryAlts(mid.HTML); strings.Join(got, ",") != "写真009,写真008,写真007,写真006" || mid.Remaining != 6 || mid.NextOffset != 24 {
		t.Errorf("mid = %v %+v", got, mid)
	}
}

// min_width を書くと、元画像の幅がそれ以上の写真だけにする（ページの本文・続きの API の両方）。
func TestPublicGalleryMinWidth(t *testing.T) {
	env := galleryEnv(t, map[string]string{"wide": ":::gallery tag=\"mixed\" min_width=\"600\"\n:::\n"}, "")
	insertGalleryMedia(env, "mixed", 60, 1200, 400) // 偶数番は 1200px、奇数番は WordPress の縮小サムネイルのような 400px
	c := env.newClient()

	res := c.get("/articles/wide")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `data-gallery-total="30" data-gallery-more="/api/gallery?limit=24&amp;min_width=600&amp;tag=mixed">`, "全 30 枚", "残り 6 件")
	for _, alt := range galleryAlts(res.Body) {
		var n int
		fmt.Sscanf(alt, "写真%d", &n)
		if n%2 == 1 {
			t.Errorf("narrow image %s shown", alt)
		}
	}
	more := getGalleryAPI(t, c, "tag=mixed&min_width=600&offset=24")
	if more.Total != 30 || more.Count != 6 || more.Remaining != 0 {
		t.Errorf("more = %+v", more)
	}
	// 絞らなければ 60 枚。
	if all := getGalleryAPI(t, c, "tag=mixed&offset=0&limit=100"); all.Total != 60 || all.Count != 60 {
		t.Errorf("unfiltered = %+v", all.Total)
	}
}

// limit="all" と明示した limit は今までどおり尊重し、「もっと見る」にしない（後方互換）。
func TestPublicGalleryExplicitLimitBackwardCompatible(t *testing.T) {
	env := galleryEnv(t, map[string]string{
		"all":   ":::gallery tag=\"many\" limit=\"all\"\n:::\n",
		"six":   ":::gallery tag=\"many\" limit=\"6\" sort=\"oldest\"\n:::\n",
		"fewer": ":::gallery tag=\"few\"\n:::\n",
	}, "")
	insertGalleryMedia(env, "many", 40, 1200, 1200)
	insertGalleryMedia(env, "few", 5, 1200, 1200)
	c := env.newClient()
	for _, tc := range []struct {
		path  string
		count int
		first string
	}{{"/articles/all", 40, "写真039"}, {"/articles/six", 6, "写真000"}, {"/articles/fewer", 5, "写真004"}} {
		res := c.get(tc.path)
		expectStatus(t, res, http.StatusOK)
		alts := galleryAlts(res.Body)
		if len(alts) != tc.count || alts[0] != tc.first {
			t.Errorf("%s: alts = %v", tc.path, alts)
		}
		for _, bad := range []string{"data-gallery-total", "data-gallery-more", `class="gallery-more"`, "gallery-more-button"} {
			if strings.Contains(res.Body, bad) {
				t.Errorf("%s: unexpected %q", tc.path, bad)
			}
		}
	}
}

// 並び順 random の続きは、本文と同じ種の並べ替えで読み、重ならずにすべてそろう。
func TestPublicGalleryRandomMoreIsStable(t *testing.T) {
	env := galleryEnv(t, map[string]string{"shuffle": ":::gallery tag=\"rnd\" sort=\"random\"\n:::\n"}, "")
	insertGalleryMedia(env, "rnd", 50, 1200, 1200)
	c := env.newClient()
	res := c.get("/articles/shuffle")
	expectStatus(t, res, http.StatusOK)
	m := regexp.MustCompile(`data-gallery-more="(/api/gallery\?[^"]+)"`).FindStringSubmatch(res.Body)
	if m == nil {
		t.Fatalf("no data-gallery-more: %s", res.Body)
	}
	moreURL := strings.ReplaceAll(m[1], "&amp;", "&")
	if !strings.Contains(moreURL, "seed=") || !strings.Contains(moreURL, "sort=random") {
		t.Fatalf("more url = %s", moreURL)
	}
	seen := map[string]bool{}
	for _, alt := range galleryAlts(res.Body) {
		seen[alt] = true
	}
	for offset := 24; offset < 50; offset += 24 {
		page := getGalleryAPI(t, c, strings.TrimPrefix(moreURL, galleryAPIPath+"?")+fmt.Sprintf("&offset=%d", offset))
		for _, alt := range galleryAlts(page.HTML) {
			if seen[alt] {
				t.Errorf("offset %d repeats %s", offset, alt)
			}
			seen[alt] = true
		}
	}
	if len(seen) != 50 {
		t.Errorf("saw %d images, want 50", len(seen))
	}
}

// 続きの API は公開中の記事・ページの :::gallery が使うタグだけを返し、不正な値は断る。
func TestPublicGalleryAPIRejectsUnknownTagsAndBadParams(t *testing.T) {
	env := galleryEnv(t, map[string]string{"album": ":::gallery tag=\"album\"\n:::\n"}, ":::gallery tag=\"secret\"\n:::\n")
	insertGalleryMedia(env, "album", 3, 1200, 1200)
	insertGalleryMedia(env, "secret", 3, 1200, 1200)
	insertGalleryMedia(env, "untagged", 3, 1200, 1200)
	c := env.newClient()

	for _, q := range []string{"tag=secret", "tag=untagged", "tag=nothing"} {
		expectStatus(t, c.get(galleryAPIPath+"?"+q), http.StatusNotFound)
	}
	for _, q := range []string{"", "tag=", "tag=album&limit=0", "tag=album&limit=101", "tag=album&offset=-1", "tag=album&offset=x",
		"tag=album&min_width=-5", "tag=album&sort=bogus", "tag=album&sort=random", "tag=album&sort=random&seed=0",
		"tag=album&sort=random&seed=2147483647", "tag=" + strings.Repeat("a", 201)} {
		expectStatus(t, c.get(galleryAPIPath+"?"+q), http.StatusBadRequest)
	}
	got := getGalleryAPI(t, c, "tag=album")
	if got.Count != 3 || got.Total != 3 || got.Remaining != 0 {
		t.Errorf("album = %+v", got)
	}
	// 種は random のときだけ使い、ほかの並び順では無視する。
	if got := getGalleryAPI(t, c, "tag=album&sort=oldest&seed=5"); galleryAlts(got.HTML)[0] != "写真000" {
		t.Errorf("oldest with seed = %v", galleryAlts(got.HTML))
	}
	// GET・HEAD 以外では写真を返さない。
	if res := c.postForm(galleryAPIPath+"?tag=album", nil); res.StatusCode == http.StatusOK {
		t.Errorf("POST status = %d", res.StatusCode)
	}
}

// 静的な書き出しには続きの API が無いため、写真をすべて出力して 25 枚目から hidden にし、ボタンで順に見せる。
// 隠した写真の画像も書き出しに含める。
func TestExportGalleryRendersAllWithHiddenRest(t *testing.T) {
	env := galleryEnv(t, map[string]string{"album": ":::gallery tag=\"album\"\n:::\n"}, "")
	insertGalleryMedia(env, "album", 30, 1200, 1200)
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "articles", "album", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	if n := len(galleryAlts(page)); n != 30 {
		t.Errorf("exported items = %d, want 30", n)
	}
	if n := strings.Count(page, `hidden=""><img`); n != 6 {
		t.Errorf("hidden items = %d, want 6", n)
	}
	expectContains(t, page, `data-gallery-total="30">`, "もっと見る（残り 6 件）")
	if strings.Contains(page, "data-gallery-more") {
		t.Error("exported gallery points at the API")
	}
}

// insertYearGalleryMedia はタグ album の写真を入れる: 古い順に 2023年 を 6 枚、年の無いものを 4 枚、2025年 を 20 枚。
func insertYearGalleryMedia(env *testEnv) {
	env.t.Helper()
	groups := []struct {
		tags string
		n    int
	}{{"album, 2023年", 6}, {"album", 4}, {"2025年, album, 卒業式", 20}}
	i := 0
	for _, g := range groups {
		for range g.n {
			id := fmt.Sprintf("album-%03d", i)
			env.insertMedia(id, id+".webp", fmt.Sprintf("写真%03d", i), "", g.tags)
			i++
		}
	}
}

// year_filter="true" のギャラリーは、最初の 24 枚に無い年（2023年）もタグの写真すべてから集めてピルにし、
// 続きの API は year=<YYYY年> でその年の写真だけを、重ならずに返す。
func TestPublicGalleryYearFilter(t *testing.T) {
	env := galleryEnv(t, map[string]string{"album": "# アルバム\n\n:::gallery tag=\"album\" year_filter=\"true\"\n:::\n"}, "")
	insertYearGalleryMedia(env)
	c := env.newClient()

	res := c.get("/articles/album")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`<div class="gallery-year-filter" role="group" aria-label="年で絞り込む"><button type="button" class="gallery-year-pill is-active" data-gallery-year="" aria-pressed="true">すべて</button>`+
			`<button type="button" class="gallery-year-pill" data-gallery-year="2025年" aria-pressed="false">2025年</button>`+
			`<button type="button" class="gallery-year-pill" data-gallery-year="2023年" aria-pressed="false">2023年</button></div>`,
		`data-gallery-tag="album" data-gallery-total="30" data-gallery-more="/api/gallery?limit=24&amp;tag=album">`,
		`aria-label="画像を拡大表示: 写真029" data-gallery-years="2025年">`,
		`aria-label="画像を拡大表示: 写真009" data-gallery-years="">`,
		"もっと見る（残り 6 件）")
	if got := galleryAlts(res.Body); len(got) != markdown.DefaultGalleryPageSize {
		t.Fatalf("first page = %v", got)
	}

	old := getGalleryAPI(t, c, "tag=album&limit=24&offset=0&year="+url.QueryEscape("2023年"))
	if got := galleryAlts(old.HTML); old.Count != 6 || old.Total != 6 || old.Remaining != 0 || strings.Join(got, ",") != "写真005,写真004,写真003,写真002,写真001,写真000" {
		t.Fatalf("2023年 = %v %+v", got, old)
	}
	recent := getGalleryAPI(t, c, "tag=album&limit=8&offset=16&year="+url.QueryEscape("2025年"))
	if got := galleryAlts(recent.HTML); recent.Count != 4 || recent.Total != 20 || recent.Remaining != 0 || got[0] != "写真013" || got[3] != "写真010" {
		t.Fatalf("2025年 page = %v %+v", got, recent)
	}
	none := getGalleryAPI(t, c, "tag=album&year="+url.QueryEscape("1999年"))
	if none.Count != 0 || none.Total != 0 {
		t.Errorf("1999年 = %+v", none)
	}

	for _, q := range []string{"year=2025", "year=" + url.QueryEscape("2025年度"), "year=" + url.QueryEscape("２０２５年")} {
		expectStatus(t, c.get(galleryAPIPath+"?tag=album&"+q), http.StatusBadRequest)
	}
}

// year_filter を書かないギャラリーには、ピルも写真の年も出さない（今までの出力のまま）。
func TestPublicGalleryWithoutYearFilterUnchanged(t *testing.T) {
	env := galleryEnv(t, map[string]string{"album": ":::gallery tag=\"album\"\n:::\n"}, "")
	insertYearGalleryMedia(env)
	res := env.newClient().get("/articles/album")
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "gallery-year-pill") || strings.Contains(res.Body, "data-gallery-years") {
		t.Error("gallery without year_filter has year pills")
	}
}

// 静的な書き出しは続きの API が無いため、すべての写真に年（data-gallery-years）を付けて出し、ピルは出した写真の年から作る
// （justified_gallery.js がブラウザ側で絞り込む）。
func TestExportGalleryYearFilterUsesItemYears(t *testing.T) {
	env := galleryEnv(t, map[string]string{"album": ":::gallery tag=\"album\" year_filter=\"true\"\n:::\n"}, "")
	insertYearGalleryMedia(env)
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "articles", "album", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	expectContains(t, page, `data-gallery-year="2025年"`, `data-gallery-year="2023年"`,
		`aria-label="画像を拡大表示: 写真000" data-gallery-years="2023年" hidden="">`)
	if n := strings.Count(page, `data-gallery-years="2025年"`); n != 20 {
		t.Errorf("2025年 items = %d, want 20", n)
	}
	if strings.Contains(page, "data-gallery-more") {
		t.Error("exported gallery points at the API")
	}
}
