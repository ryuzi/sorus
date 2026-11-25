package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
)

// handleLLMSTxt publishes a compact llms.txt index of public articles. Fixed pages (kind = page), which describe the
// organization itself, are listed first in their own "Pages" section; the section is omitted when there are none.
// The page shown on the front page stays in that section with the URL /, its only public URL.
func (s *Server) handleLLMSTxt(c echo.Context) error {
	rows, err := s.llmsArticles(c)
	if err != nil {
		return err
	}
	front, err := s.frontPageSlug(c.Request().Context())
	if err != nil {
		return err
	}
	base := s.baseURL(c)
	siteName, siteDesc := s.getSiteInfo()
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", siteName)
	desc := strings.TrimSpace(siteDesc)
	if desc == "" {
		desc = siteName
	}
	fmt.Fprintf(&b, "> %s\n\n", llmsPlain(desc))
	writeItems := func(kind article.Kind) {
		for _, a := range rows {
			if (a.Kind == article.KindPage) != (kind == article.KindPage) {
				continue
			}
			summary := strings.TrimSpace(a.Excerpt)
			if summary == "" {
				summary = articleDescription(a, "")
			}
			fmt.Fprintf(&b, "- [%s](%s): %s\n", llmsMarkdown(a.Title), base+publicURLPath(a.Kind, a.Slug, front, a.Lang), llmsPlain(summary))
		}
	}
	for _, a := range rows {
		if a.Kind == article.KindPage {
			b.WriteString("## Pages\n")
			writeItems(article.KindPage)
			b.WriteString("\n")
			break
		}
	}
	b.WriteString("## Articles\n")
	writeItems(article.KindPost)
	fmt.Fprintf(&b, "\n## Optional\n\n- [Full content](%s/llms-full.txt): Complete Markdown for all published articles.\n", base)
	c.Response().Header().Set(echo.HeaderCacheControl, feedCacheControl)
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(b.String()))
}

// handleLLMSFullTxt streams complete Markdown and metadata for every public article. Fixed pages come first and,
// having no date or category on the public site, carry only their URL (/ for the page shown on the front page).
func (s *Server) handleLLMSFullTxt(c echo.Context) error {
	front, err := s.frontPageSlug(c.Request().Context())
	if err != nil {
		return err
	}
	rows, err := s.db.QueryxContext(c.Request().Context(), `SELECT a.title, a.slug, a.content_markdown, a.excerpt, a.published_at,
		COALESCE(cat.name, '') AS category_name, a.kind, COALESCE(NULLIF(a.lang, ''), 'ja') AS lang FROM articles a LEFT JOIN categories cat ON cat.id = a.category_id
		WHERE `+publishedCondition+s.enExcludedCondition()+` ORDER BY a.kind = 'post', julianday(a.published_at) DESC, a.id`, article.StatusPublished, clockNow().UTC())
	if err != nil {
		return err
	}
	defer rows.Close()
	w := c.Response()
	w.Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
	w.Header().Set(echo.HeaderCacheControl, feedCacheControl)
	w.WriteHeader(http.StatusOK)
	base := s.baseURL(c)
	for rows.Next() {
		var a struct {
			Title       string       `db:"title"`
			Slug        string       `db:"slug"`
			Content     string       `db:"content_markdown"`
			Excerpt     string       `db:"excerpt"`
			PublishedAt time.Time    `db:"published_at"`
			Category    string       `db:"category_name"`
			Kind        article.Kind `db:"kind"`
			Lang        string       `db:"lang"`
		}
		if err := rows.StructScan(&a); err != nil {
			return err
		}
		var err error
		if a.Kind == article.KindPage {
			_, err = fmt.Fprintf(w, "# %s\n\nURL: %s%s\n\n%s\n\n---\n\n",
				llmsPlain(a.Title), base, publicURLPath(a.Kind, a.Slug, front, a.Lang), strings.TrimSpace(a.Content))
		} else {
			_, err = fmt.Fprintf(w, "# %s\n\nPublished: %s\nCategory: %s\nURL: %s%s\n\n%s\n\n---\n\n",
				llmsPlain(a.Title), a.PublishedAt.UTC().Format(time.RFC3339), llmsPlain(a.Category), base, articlePublicPath(a.Kind, a.Slug, a.Lang), strings.TrimSpace(a.Content))
		}
		if err != nil {
			return err
		}
		w.Flush()
	}
	return rows.Err()
}

func (s *Server) llmsArticles(c echo.Context) ([]publicArticle, error) {
	var rows []publicArticle
	// 多言語対応が OFF なら、開けない /en の URL を載せないよう英語の記事・ページを除く。
	err := s.db.SelectContext(c.Request().Context(), &rows, `SELECT `+publicArticleColumns+publicArticleFrom+`WHERE `+publishedCondition+
		s.enExcludedCondition()+` ORDER BY julianday(a.published_at) DESC, a.id`, article.StatusPublished, clockNow().UTC())
	return rows, err
}

func llmsPlain(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(v))
}

func llmsMarkdown(v string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "\r", " ", "\n", " ").Replace(strings.TrimSpace(v))
}
