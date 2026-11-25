package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

func TestFeedShowAuthor(t *testing.T) {
	// show_author=false の場合、RSS フィードに <dc:creator> を出力せず、執筆者名が漏洩しないこと
	t.Run("show_author=false suppresses dc:creator", func(t *testing.T) {
		env := newTestEnvWith(t, Config{BaseURL: testBaseURL, ShowAuthor: false}, func(db *sqlx.DB) {
			seedCategory(t, db, "一般", "general", "")
			seedArticle(t, db, article.Article{
				Title:           "著者を隠す記事",
				Slug:            "hide-author-post",
				ContentMarkdown: "本文コンテンツです。",
				Status:          article.StatusPublished,
				PublishedAt:     ago(time.Hour),
			})
		})

		res := env.newClient().get("/feed.xml")
		expectStatus(t, res, http.StatusOK)
		if strings.Contains(res.Body, "<dc:creator>") {
			t.Errorf("feed.xml must not contain <dc:creator> when show_author=false, got:\n%s", res.Body)
		}
		if strings.Contains(res.Body, "山田") {
			t.Errorf("feed.xml must not leak author name when show_author=false, got:\n%s", res.Body)
		}
	})

	// show_author=true の場合、RSS フィードに <dc:creator>著者名</dc:creator> が出力されること
	t.Run("show_author=true includes dc:creator", func(t *testing.T) {
		env := newTestEnvWith(t, Config{BaseURL: testBaseURL, ShowAuthor: true}, func(db *sqlx.DB) {
			seedCategory(t, db, "一般", "general", "")
			seedArticle(t, db, article.Article{
				Title:           "著者を公開する記事",
				Slug:            "show-author-post",
				ContentMarkdown: "本文コンテンツです。",
				Status:          article.StatusPublished,
				PublishedAt:     ago(time.Hour),
			})
		})

		res := env.newClient().get("/feed.xml")
		expectStatus(t, res, http.StatusOK)
		if !strings.Contains(res.Body, "<dc:creator>山田</dc:creator>") {
			t.Errorf("feed.xml must contain <dc:creator>山田</dc:creator> when show_author=true, got:\n%s", res.Body)
		}
	})
}
