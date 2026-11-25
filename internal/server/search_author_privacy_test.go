package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/rag"
)

// newHiddenAuthorSearchEnv は執筆者名を表示しない設定で、studentA（山田）の通常記事・論文記事と、
// タイトルに「山田」を含む studentB（鈴木）の記事を用意したサーバーを起動する。本文などに「山田」は含めない。
func newHiddenAuthorSearchEnv(t *testing.T) (env *testEnv, normalID, paperID, townID string) {
	t.Helper()
	env = newTestEnvWith(t, Config{BaseURL: testBaseURL, ShowAuthor: false}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		normalID = seedArticle(t, db, article.Article{
			Title: "新歓合宿のお知らせ", Slug: "camp", Excerpt: "春の合宿",
			ContentMarkdown: "長野で合宿をします。", AuthorID: studentA.ID,
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
		paperID = seedArticle(t, db, article.Article{
			Title: "量子計算の研究", Slug: "paper", ContentMarkdown: "量子ビットの実験。", AuthorID: studentA.ID,
			DOI:    "10.1234/qc.2026.01",
			Status: article.StatusPublished, PublishedAt: ago(2 * time.Hour),
		})
		townID = seedArticle(t, db, article.Article{
			Title: "山田町の夏祭り", Slug: "town", ContentMarkdown: "屋台が並んだ。", AuthorID: studentB.ID,
			Status: article.StatusPublished, PublishedAt: ago(3 * time.Hour),
		})
	})
	return env, normalID, paperID, townID
}

// 執筆者名を表示しない設定では、公開サイトの検索で通常の記事を著者名だけで一致させない（件数にも数えない）。
// タイトル・本文の一致、著者を書誌情報として公開する論文記事の著者名での一致、設定の即時反映は保つ。
func TestPublicSearchDoesNotMatchHiddenAuthor(t *testing.T) {
	env, _, _, _ := newHiddenAuthorSearchEnv(t)
	c := env.newClient()
	search := func(q string, htmx bool) *response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/search?q="+url.QueryEscape(q), nil)
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		res := c.do(req)
		expectStatus(t, res, http.StatusOK)
		return res
	}

	for _, htmx := range []bool{false, true} {
		res := search("山田", htmx)
		expectContains(t, res.Body, "「山田」の検索結果: 2件", `href="/articles/town"`, `href="/articles/paper"`)
		expectNotContains(t, res.Body, `href="/articles/camp"`)

		res = search("合宿 山田", htmx)
		expectContains(t, res.Body, "の検索結果: 0件")
		expectNotContains(t, res.Body, `href="/articles/camp"`)

		res = search("合宿", htmx)
		expectContains(t, res.Body, "「合宿」の検索結果: 1件", `href="/articles/camp"`)
	}

	// 執筆者名を表示する設定に切り替えると、再起動・再索引なしで著者名でも一致する。
	env.server.setShowAuthor(true)
	res := search("山田", false)
	expectContains(t, res.Body, "「山田」の検索結果: 3件", `href="/articles/camp"`)

	env.server.setShowAuthor(false)
	res = search("山田", true)
	expectContains(t, res.Body, "「山田」の検索結果: 2件")
	expectNotContains(t, res.Body, `href="/articles/camp"`)
}

// 管理画面での保存（再索引）を経ても、執筆者名を表示しない設定では著者名で一致させない。論文記事にすれば一致する。
func TestPublicSearchHiddenAuthorAfterReindex(t *testing.T) {
	env, normalID, _, _ := newHiddenAuthorSearchEnv(t)
	reader := env.newClient()
	count := func(q string) string {
		t.Helper()
		res := reader.get("/search?q=" + url.QueryEscape(q))
		expectStatus(t, res, http.StatusOK)
		const marker = "の検索結果: "
		i := strings.Index(res.Body, marker)
		if i < 0 {
			t.Fatalf("result count not found: %.1000s", res.Body)
		}
		rest := res.Body[i+len(marker):]
		return rest[:strings.Index(rest, "件")]
	}

	a := env.mustGetArticle(normalID)
	a.Excerpt = "春の合宿（更新）"
	if err := env.server.articles.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	env.server.reindex(env.server.echo.Logger, normalID)
	if n := count("山田"); n != "2" {
		t.Errorf("hits for hidden author after reindex = %s, want 2", n)
	}

	a = env.mustGetArticle(normalID)
	a.JournalName = "学生物理学会誌"
	if err := env.server.articles.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	env.server.reindex(env.server.echo.Logger, normalID)
	if n := count("山田"); n != "3" {
		t.Errorf("hits after making the article scholarly = %s, want 3", n)
	}
}

// 関連記事の全文検索のスコアも、執筆者名を表示しない間は著者名で一致させない。
func TestRelatedKeywordScoresIgnoreHiddenAuthor(t *testing.T) {
	env, normalID, paperID, townID := newHiddenAuthorSearchEnv(t)
	src := publicArticle{ID: "src", Title: "山田"}

	scores := env.server.keywordScores(context.Background(), src)
	if _, ok := scores[normalID]; ok {
		t.Errorf("keywordScores matched normal article by hidden author: %v", scores)
	}
	if _, ok := scores[townID]; !ok {
		t.Errorf("keywordScores lost title match: %v", scores)
	}
	if _, ok := scores[paperID]; !ok {
		t.Errorf("keywordScores lost scholarly author match: %v", scores)
	}

	env.server.setShowAuthor(true)
	if scores := env.server.keywordScores(context.Background(), src); scores[normalID] == 0 {
		t.Errorf("keywordScores with show_author=true should match author: %v", scores)
	}
}

// 執筆者名の表示を切り替えたら、関連記事の推薦結果のキャッシュを捨てて新しい設定で求め直す。
func TestSetShowAuthorInvalidatesRelatedCache(t *testing.T) {
	env, _, _, _ := newHiddenAuthorSearchEnv(t)
	put := func() {
		t.Helper()
		_, gen, _ := env.server.related.get("a1")
		env.server.related.put("a1", gen, []rag.RelatedArticle{{ArticleID: "a2"}})
		if _, _, ok := env.server.related.get("a1"); !ok {
			t.Fatal("related cache entry was not stored")
		}
	}

	put()
	env.server.setShowAuthor(false) // 変わらなければ捨てない。
	if _, _, ok := env.server.related.get("a1"); !ok {
		t.Error("related cache was invalidated although show_author did not change")
	}
	env.server.setShowAuthor(true)
	if _, _, ok := env.server.related.get("a1"); ok {
		t.Error("related cache survived show_author change to true")
	}
	put()
	env.server.setShowAuthor(false)
	if _, _, ok := env.server.related.get("a1"); ok {
		t.Error("related cache survived show_author change to false")
	}
}
