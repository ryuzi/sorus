package server

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// updateGoldenEnv が "1" のとき、TestNoPagesSiteUnchanged は比較せずに golden を書き直す。
// golden は固定ページ（kind=page）の導入前に記録したもので、ページを作っていないサイトの公開側の出力が
// 導入前と変わらないことを確かめる。サイト構成の導入時に、ナビで表示中のカテゴリへ aria-current="page" を
// 付けた分だけ category.html を更新した（未設定のサイトのナビで唯一変えてよい差分）。
const updateGoldenEnv = "SORUS_UPDATE_GOLDEN"

// assetVersionPattern は静的アセットの版（内容のハッシュ）。CSS を足すと変わるため比較から外す。
var assetVersionPattern = regexp.MustCompile(`\?v=[0-9a-f]+`)

// newNoPagesEnv は固定の公開日時の記事（ページ送りが出る13件）だけを持つサイトを起動する。
func newNoPagesEnv(t *testing.T) *testEnv {
	t.Helper()
	base := time.Date(2025, 6, 1, 3, 0, 0, 0, time.UTC)
	return newTestEnvWith(t, Config{BaseURL: testBaseURL, SiteName: "回帰テスト", SiteDescription: "固定ページのないサイト", ShowAuthor: true}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		circle := seedCategory(t, db, "サークル", "circle", "学内サークルの活動")
		for i := 1; i <= 13; i++ {
			cat := int64(1)
			if i%3 == 0 {
				cat = circle
			}
			published := base.Add(time.Duration(i) * time.Hour)
			seedArticle(t, db, article.Article{
				Title: fmt.Sprintf("記事その%d", i), Slug: fmt.Sprintf("post-%d", i), CategoryID: cat,
				Excerpt:         fmt.Sprintf("抜粋%d", i),
				ContentMarkdown: fmt.Sprintf("## 見出し%d\n\n本文%d。", i, i),
				Status:          article.StatusPublished, PublishedAt: &published,
			})
		}
		seedArticle(t, db, article.Article{Title: "下書き", Slug: "draft-1", ContentMarkdown: "下書き"})
		if _, err := db.Exec(`UPDATE articles SET updated_at = ?, created_at = ?`, base, base); err != nil {
			t.Fatalf("fix timestamps: %v", err)
		}
	})
}

func TestNoPagesSiteUnchanged(t *testing.T) {
	env := newNoPagesEnv(t)
	c := env.newClient()
	for name, path := range map[string]string{
		"index.html":    "/",
		"index_p2.html": "/?page=2",
		"category.html": "/categories/circle",
		"article.html":  "/articles/post-3",
		"feed.xml":      "/feed.xml",
		"sitemap.xml":   "/sitemap.xml",
		"llms.txt":      "/llms.txt",
	} {
		res := c.get(path)
		if res.StatusCode != 200 {
			t.Fatalf("GET %s: status %d", path, res.StatusCode)
		}
		got := assetVersionPattern.ReplaceAllString(res.Body, "?v=X")
		golden := filepath.Join("testdata", "no_pages", name)
		if os.Getenv(updateGoldenEnv) == "1" {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("read golden %s: %v", golden, err)
		}
		if got != string(want) {
			t.Errorf("GET %s differs from the golden recorded before pages existed (%s)\ngot:\n%s", path, golden, got)
		}
	}
}
