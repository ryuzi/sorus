package server

import (
	"database/sql"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/markdown"
	"sorus/internal/social"
)

const (
	// OGImageURLPrefix は動的 OGP 画像を配信する URL パス（/og-image/<slug>.png）。
	OGImageURLPrefix = "/og-image/"
	// ogImageMaxAge は動的 OGP 画像をブラウザ・CDN にキャッシュさせる秒数。記事を直したら ETag で再検証させる。
	ogImageMaxAge = 3600
	// socialSummaryRunes は抜粋の無い記事で、本文の先頭から告知文の要約に使う文字数。
	socialSummaryRunes = 300
)

// ogImagePath は記事 slug の動的 OGP 画像のパス。
func ogImagePath(slug string) string { return OGImageURLPrefix + url.PathEscape(slug) + ".png" }

// handleOGImage は公開中の記事のタイトル・カテゴリ・サイト名を描いた 1200×630 の PNG を返す。
// 固定ページはカテゴリと日付を持たないため、タイトルとサイト名だけを描く。
// 描いた画像は内容ごとにメモリにキャッシュし（social.Generator）、ETag で条件付き GET に 304 を返す。
func (s *Server) handleOGImage(c echo.Context) error {
	slug, ok := strings.CutSuffix(pathParam(c, "file"), ".png")
	if !ok || slug == "" {
		return echo.ErrNotFound
	}
	var row struct {
		Title       string       `db:"title"`
		Category    string       `db:"category_name"`
		PublishedAt time.Time    `db:"published_at"`
		Kind        article.Kind `db:"kind"`
	}
	err := s.db.Get(&row, `SELECT a.title, COALESCE(c.name, '') AS category_name, a.published_at, a.kind
		FROM articles a LEFT JOIN categories c ON c.id = a.category_id
		WHERE a.slug = ? AND `+publishedCondition, slug, article.StatusPublished, clockNow().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	card := social.Card{
		Title:    row.Title,
		Category: row.Category,
		SiteName: s.getSiteName(),
		Footer:   row.PublishedAt.In(jst).Format("2006.01.02"),
	}
	if row.Kind == article.KindPage {
		card.Category, card.Footer = "", ""
	}
	h := c.Response().Header()
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(ogImageMaxAge))
	// 再検証（If-None-Match）は画像を描かずに ETag だけで応じる。
	if etag := social.ETag(card); etagMatches(c.Request().Header.Get("If-None-Match"), etag) {
		h.Set("ETag", etag)
		return c.NoContent(http.StatusNotModified)
	}
	png, etag, err := s.ogp.PNG(card)
	if err != nil {
		return err
	}
	h.Set("ETag", etag)
	return c.Blob(http.StatusOK, "image/png", png)
}

// etagMatches は If-None-Match（カンマ区切り・弱い ETag・* を含む）が etag に一致するかを返す。
func etagMatches(header, etag string) bool {
	for _, v := range strings.Split(header, ",") {
		v = strings.TrimPrefix(strings.TrimSpace(v), "W/")
		if v == "*" || v == etag {
			return true
		}
	}
	return false
}

// socialPostResponse は POST /admin/ai/social-post の JSON レスポンス。
type socialPostResponse struct {
	// Configured が false なら AI が未設定で、テンプレートで作った告知文と、AI の設定方法の案内（Guide）を返す。
	Configured bool   `json:"configured"`
	Provider   string `json:"provider,omitempty"`
	Guide      string `json:"guide,omitempty"`
	// Source は告知文の作り方（"ai" または "template"）。
	Source string `json:"source"`
	social.Posts
	// XLengths は X の各投稿の文字数（全角を2と数える重み付き）、XLimit はその上限。
	XLengths []int `json:"x_lengths"`
	XLimit   int   `json:"x_limit"`
	// URL は記事の公開 URL。未公開の記事なら空。
	URL string `json:"url,omitempty"`
	// OGImage は SNS で表示される OGP 画像の URL。未公開の記事なら空。
	OGImage string `json:"og_image,omitempty"`
}

// socialArticle は告知文の材料にする記事。
type socialArticle struct {
	Title         string         `db:"title"`
	Slug          string         `db:"slug"`
	Excerpt       string         `db:"excerpt"`
	Markdown      string         `db:"content_markdown"`
	Status        article.Status `db:"status"`
	PublishedAt   *time.Time     `db:"published_at"`
	FeaturedImage string         `db:"featured_image_url"`
	Category      string         `db:"category_name"`
	Kind          article.Kind   `db:"kind"`
	Lang          string         `db:"lang"`
}

// handleSocialPost は記事から X の告知スレッド・Instagram のリード文・LINE の配信文を作る。
// フォームの article_id（保存済みの記事）があれば記事の URL・カテゴリを使い、title・text・excerpt が
// 送られていればエディタで編集中の内容を優先する。AI が未設定ならテンプレートで作った告知文を返す。
func (s *Server) handleSocialPost(c echo.Context) error {
	title := strings.TrimSpace(c.FormValue("title"))
	text := normalizeNewlines(c.FormValue("text"))
	excerpt := strings.TrimSpace(c.FormValue("excerpt"))
	in := social.PostInput{SiteName: s.getSiteName()}
	res := socialPostResponse{XLimit: social.XMaxLength}

	if id := strings.TrimSpace(c.FormValue("article_id")); id != "" {
		var a socialArticle
		err := s.db.Get(&a, `SELECT a.title, a.slug, a.excerpt, a.content_markdown, a.status, a.published_at,
				a.featured_image_url, COALESCE(c.name, '') AS category_name, a.kind, COALESCE(NULLIF(a.lang, ''), 'ja') AS lang
			FROM articles a LEFT JOIN categories c ON c.id = a.category_id WHERE a.id = ?`, id)
		if errors.Is(err, sql.ErrNoRows) {
			return echo.ErrNotFound
		}
		if err != nil {
			return err
		}
		// 執筆チーム内では非公開の記事を共有するため、誰の記事でも告知文を作れる（読者に見えるまで URL は付けない）。
		visible := article.IsVisible(&article.Article{Status: a.Status, PublishedAt: a.PublishedAt}, clockNow())
		if title == "" {
			title = a.Title
		}
		if text == "" {
			text = a.Markdown
		}
		if _, sent := c.Request().Form["excerpt"]; !sent {
			excerpt = a.Excerpt
		}
		// ページはカテゴリを公開サイトに出さないため、告知文にも書かない。
		if a.Kind != article.KindPage {
			in.Category = a.Category
		}
		// 告知文の URL は、読者が開ける公開中の記事のときだけ付ける。
		if visible {
			base := s.baseURL(c)
			in.URL = base + articlePublicPath(a.Kind, a.Slug, a.Lang)
			res.OGImage = absoluteURL(base, a.FeaturedImage)
			if a.FeaturedImage == "" {
				res.OGImage = base + ogImagePath(a.Slug)
			}
		}
	}
	if catID, err := strconv.ParseInt(c.FormValue("category_id"), 10, 64); err == nil {
		// エディタで選び直したカテゴリ（未保存）を優先する。
		var name string
		if err := s.db.Get(&name, `SELECT name FROM categories WHERE id = ?`, catID); err == nil {
			in.Category = name
		}
	}
	if title == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "タイトルを入力してから実行してください")
	}
	if utf8.RuneCountInString(text) > maxAITextRunes {
		// 告知文には記事の前半で足りる。長い記事も AI の上限に収まるよう切り詰める。
		text = string([]rune(text)[:maxAITextRunes])
	}
	in.Title = title
	in.Summary = excerpt
	if in.Summary == "" {
		in.Summary = markdownSummary(text, socialSummaryRunes)
	}
	res.URL = in.URL

	provider, err := s.requestedAIProvider(c)
	if err != nil {
		return err
	}
	writer, ok := provider.(ai.SocialPostWriter)
	if ai.IsConfigured(provider) && ok {
		posts, err := writer.GenerateSocialPosts(c.Request().Context(), ai.SocialPostRequest{
			Title: in.Title, Category: in.Category, SiteName: in.SiteName, URL: in.URL, Text: text,
		})
		switch {
		case err == nil:
			res.Configured, res.Provider, res.Source = true, provider.Name(), "ai"
			res.Posts = social.Finalize(social.Posts{
				XThread: posts.XThread, Instagram: posts.Instagram, LINE: posts.LINE, Hashtags: posts.Hashtags,
			}, in)
		case errors.Is(err, ai.ErrNotConfigured):
		default:
			return s.aiErrorFor(c, provider, err)
		}
	}
	if res.Source == "" {
		res.Source = "template"
		res.Posts = social.TemplatePosts(in)
		if !ai.IsConfigured(provider) {
			res.Guide = aiSetupGuideFor(provider)
		} else {
			res.Configured, res.Provider = true, provider.Name()
		}
	}
	res.XLengths = make([]int, len(res.XThread))
	for i, post := range res.XThread {
		res.XLengths[i] = social.XLength(post)
	}
	return c.JSON(http.StatusOK, res)
}

// markdownSummary は Markdown の本文をプレーンテキストにして、先頭の max 文字を返す。
func markdownSummary(md string, max int) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}
	body := md
	if res, err := markdown.Render(md); err == nil {
		body = res.HTML
	}
	text := strings.Join(strings.Fields(html.UnescapeString(textPolicy.Sanitize(body))), " ")
	if utf8.RuneCountInString(text) > max {
		text = string([]rune(text)[:max]) + "…"
	}
	return text
}
