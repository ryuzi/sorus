package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"sorus/internal/article"
	"sorus/internal/markdown"
)

// headlessDataDir は静的サイト書き出しで Headless フロントエンド向けの構造化 JSON データを置くフォルダ。
const headlessDataDir = "api-data"

// headlessSiteData は api-data/site.json に書き出すサイト全体のメタ情報と構成。
type headlessSiteData struct {
	SiteName        string             `json:"site_name"`
	SiteDescription string             `json:"site_description"`
	BaseURL         string             `json:"base_url"`
	Lang            string             `json:"lang"`
	Theme           string             `json:"theme"`
	SiteMenu        []headlessMenuItem `json:"site_menu"`
	SiteFooterMenu  []headlessMenuItem `json:"site_footer_menu"`
	FrontPage       string             `json:"front_page"`
	FrontCards      []string           `json:"front_cards"`
	Categories      []headlessCategory `json:"categories"`
}

// headlessMenuItem は api-data/site.json のメニュー（site_menu・site_footer_menu）の1項目。
// 公開済みの参照先を解決した表示名（label）とリンク先（url）に加え、種別（type）・参照先（target）・
// 英語表示名（label_en）と子項目（children）を持つ。未公開・存在しない参照先の項目は含めない。
type headlessMenuItem struct {
	Label    string             `json:"label"`
	LabelEn  string             `json:"label_en,omitempty"`
	Type     MenuItemType       `json:"type,omitempty"`
	Target   string             `json:"target,omitempty"`
	URL      string             `json:"url,omitempty"`
	Children []headlessMenuItem `json:"children,omitempty"`
}

// headlessCategory は公開カテゴリ（name, slug）。
type headlessCategory struct {
	Name string `db:"name" json:"name"`
	Slug string `db:"slug" json:"slug"`
}

// headlessPage は api-data/pages.json の1要素（公開済みの固定ページ）。
type headlessPage struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	URL         string `json:"url"`
	Lang        string `json:"lang"`
	ParentID    string `json:"parent_id"`
	ParentSlug  string `json:"parent_slug"`
	SortOrder   int    `json:"sort_order"`
	Excerpt     string `json:"excerpt"`
	HTML        string `json:"html"`
	Markdown    string `json:"markdown"`
	HeroImage   string `json:"hero_image"`
	PublishedAt string `json:"published_at"`
	UpdatedAt   string `json:"updated_at"`
}

// headlessArticle は api-data/articles.json の1要素（公開済みの記事）。
type headlessArticle struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Slug        string           `json:"slug"`
	URL         string           `json:"url"`
	Lang        string           `json:"lang"`
	Category    headlessCategory `json:"category"`
	Tags        []string         `json:"tags"`
	Excerpt     string           `json:"excerpt"`
	HTML        string           `json:"html"`
	Markdown    string           `json:"markdown"`
	HeroImage   string           `json:"hero_image"`
	PublishedAt string           `json:"published_at"`
	UpdatedAt   string           `json:"updated_at"`
}

type headlessContentRow struct {
	ID              string    `db:"id"`
	Title           string    `db:"title"`
	Slug            string    `db:"slug"`
	Lang            string    `db:"lang"`
	ParentID        string    `db:"parent_id"`
	SortOrder       int       `db:"sort_order"`
	CategoryName    string    `db:"category_name"`
	CategorySlug    string    `db:"category_slug"`
	Excerpt         string    `db:"excerpt"`
	ContentMarkdown string    `db:"content_markdown"`
	ContentHTML     string    `db:"content_html"`
	FeaturedImage   string    `db:"featured_image_url"`
	PublishedAt     time.Time `db:"published_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}

const headlessContentColumns = `a.id, a.title, a.slug,
	COALESCE(NULLIF(a.lang, ''), 'ja') AS lang,
	COALESCE(a.parent_id, '') AS parent_id,
	a.sort_order,
	COALESCE(c.name, '') AS category_name,
	COALESCE(c.slug, '') AS category_slug,
	a.excerpt, a.content_markdown, a.content_html, a.featured_image_url,
	a.published_at, a.updated_at`

// writeHeadlessData は公開済みのサイト情報・固定ページ一覧・記事一覧を outDir/api-data/ 配下に JSON で書き出す。
// 下書き・校閲待ち・予約投稿・ゴミ箱の記事や固定ページ、内部コメント、著者メールアドレス・ユーザー識別子は一切含めない。
func (s *Server) writeHeadlessData(ctx context.Context, outDir, baseURL string, now time.Time, override *themeOverride, attachments *attachmentExport, rep *ExportReport) error {
	site, err := s.buildHeadlessSiteData(ctx, baseURL, now, override)
	if err != nil {
		return err
	}
	pages, err := s.buildHeadlessPages(ctx, now, site.FrontPage, attachments)
	if err != nil {
		return err
	}
	articles, err := s.buildHeadlessArticles(ctx, now, attachments)
	if err != nil {
		return err
	}

	dir := filepath.Join(outDir, headlessDataDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	files := []struct {
		name string
		data any
	}{
		{"site.json", site},
		{"pages.json", pages},
		{"articles.json", articles},
	}
	for _, f := range files {
		raw, err := json.Marshal(f.data)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), raw, 0644); err != nil {
			return err
		}
		rep.FilesCount++
	}
	return nil
}

func (s *Server) buildHeadlessSiteData(ctx context.Context, baseURL string, now time.Time, override *themeOverride) (headlessSiteData, error) {
	st := s.loadSiteStructure(ctx)
	header := st.Menu
	if len(header) > maxMenuItems {
		header = header[:maxMenuItems]
	}
	foot := st.FooterMenu
	if len(foot) > maxFooterColumns {
		foot = foot[:maxFooterColumns]
	}
	refs, err := s.resolveStructureRefs(ctx, SiteStructure{
		Menu:       header,
		FooterMenu: foot,
		FrontPage:  st.FrontPage,
		FrontCards: st.FrontCards,
	})
	if err != nil {
		return headlessSiteData{}, err
	}
	front := resolvedFrontPage(st, refs)
	cards := make([]string, 0, len(st.FrontCards))
	for _, slug := range st.FrontCards {
		if _, ok := refs.Pages[slug]; ok && !slices.Contains(cards, slug) {
			cards = append(cards, slug)
		}
		if len(cards) >= maxFrontCards {
			break
		}
	}

	cats := []headlessCategory{}
	if err := s.db.SelectContext(ctx, &cats, `SELECT c.name, c.slug FROM categories c
		WHERE EXISTS (
			SELECT 1 FROM articles a
			WHERE a.category_id = c.id
			  AND `+postKindCondition+`
			  AND `+publishedCondition+s.enExcludedCondition()+`
		)
		ORDER BY c.id`, article.StatusPublished, now); err != nil {
		return headlessSiteData{}, err
	}

	siteName, siteDesc := s.getSiteInfo()
	return headlessSiteData{
		SiteName:        siteName,
		SiteDescription: siteDesc,
		BaseURL:         baseURL,
		Lang:            "ja",
		Theme:           s.exportTheme(override).ID,
		SiteMenu:        resolveHeadlessMenu(header, refs, front),
		SiteFooterMenu:  resolveHeadlessMenu(foot, refs, front),
		FrontPage:       front,
		FrontCards:      cards,
		Categories:      cats,
	}, nil
}

func resolveHeadlessMenu(items []MenuItem, refs structureRefs, front string) []headlessMenuItem {
	r := menuResolver{
		refs:  refs,
		front: front,
	}
	out := make([]headlessMenuItem, 0, len(items))
	for _, it := range items {
		if validateMenuItem(it) != nil {
			continue
		}
		var children []headlessMenuItem
		for j, ch := range it.Children {
			if j >= maxMenuChildren {
				break
			}
			if validateMenuChild(ch) != nil {
				continue
			}
			if c, ok := r.item(ch); ok {
				children = append(children, headlessMenuItem{
					Label:   c.Label,
					LabelEn: strings.TrimSpace(ch.LabelEn),
					Type:    ch.Type,
					Target:  ch.Target,
					URL:     c.URL,
				})
			}
		}
		itemType := it.Type
		itemTarget := it.Target
		p, ok := r.item(it)
		if !ok {
			label := r.ownLabel(it)
			if label == "" || len(children) == 0 {
				continue
			}
			p = publicMenuItem{Label: label}
			itemType = MenuItemGroup
			itemTarget = ""
		}
		if p.URL == "" && len(children) == 0 {
			continue
		}
		out = append(out, headlessMenuItem{
			Label:    p.Label,
			LabelEn:  strings.TrimSpace(it.LabelEn),
			Type:     itemType,
			Target:   itemTarget,
			URL:      p.URL,
			Children: children,
		})
	}
	return out
}

func (s *Server) buildHeadlessPages(ctx context.Context, now time.Time, front string, attachments *attachmentExport) ([]headlessPage, error) {
	var rows []headlessContentRow
	err := s.db.SelectContext(ctx, &rows, `SELECT `+headlessContentColumns+`
		FROM articles a
		LEFT JOIN categories c ON c.id = a.category_id
		WHERE a.kind = ? AND `+publishedCondition+s.enExcludedCondition()+`
		ORDER BY `+article.PageOrder,
		article.KindPage, article.StatusPublished, now)
	if err != nil {
		return nil, err
	}

	visibleSlugByID := make(map[string]string, len(rows))
	for _, r := range rows {
		visibleSlugByID[r.ID] = r.Slug
	}

	pages := make([]headlessPage, 0, len(rows))
	for _, r := range rows {
		lang := normalizeHeadlessLang(r.Lang)
		parentID, parentSlug := "", ""
		if r.ParentID != "" && r.ParentID != r.ID {
			if slug, ok := visibleSlugByID[r.ParentID]; ok {
				parentID = r.ParentID
				parentSlug = slug
			}
		}
		pages = append(pages, headlessPage{
			ID:          r.ID,
			Title:       r.Title,
			Slug:        r.Slug,
			URL:         publicURLPath(article.KindPage, r.Slug, front, lang),
			Lang:        lang,
			ParentID:    parentID,
			ParentSlug:  parentSlug,
			SortOrder:   r.SortOrder,
			Excerpt:     r.Excerpt,
			HTML:        s.headlessHTML(r.ContentMarkdown, r.ContentHTML, attachments),
			Markdown:    rewriteHeadlessText(r.ContentMarkdown, attachments),
			HeroImage:   safeURL(strings.TrimSpace(r.FeaturedImage)),
			PublishedAt: formatHeadlessTime(r.PublishedAt),
			UpdatedAt:   formatHeadlessTime(r.UpdatedAt),
		})
	}
	return pages, nil
}

func (s *Server) buildHeadlessArticles(ctx context.Context, now time.Time, attachments *attachmentExport) ([]headlessArticle, error) {
	var rows []headlessContentRow
	err := s.db.SelectContext(ctx, &rows, `SELECT `+headlessContentColumns+`
		FROM articles a
		LEFT JOIN categories c ON c.id = a.category_id
		WHERE `+postKindCondition+` AND `+publishedCondition+s.enExcludedCondition()+`
		ORDER BY julianday(a.published_at) DESC, a.id ASC`,
		article.StatusPublished, now)
	if err != nil {
		return nil, err
	}

	articles := make([]headlessArticle, 0, len(rows))
	for _, r := range rows {
		lang := normalizeHeadlessLang(r.Lang)
		articles = append(articles, headlessArticle{
			ID:    r.ID,
			Title: r.Title,
			Slug:  r.Slug,
			URL:   articlePublicPath(article.KindPost, r.Slug, lang),
			Lang:  lang,
			Category: headlessCategory{
				Name: r.CategoryName,
				Slug: r.CategorySlug,
			},
			Tags:        []string{},
			Excerpt:     r.Excerpt,
			HTML:        s.headlessHTML(r.ContentMarkdown, r.ContentHTML, attachments),
			Markdown:    rewriteHeadlessText(r.ContentMarkdown, attachments),
			HeroImage:   safeURL(strings.TrimSpace(r.FeaturedImage)),
			PublishedAt: formatHeadlessTime(r.PublishedAt),
			UpdatedAt:   formatHeadlessTime(r.UpdatedAt),
		})
	}
	return articles, nil
}

func (s *Server) headlessHTML(md, cachedHTML string, attachments *attachmentExport) string {
	body := cachedHTML
	if md != "" && (body == "" || strings.Contains(md, ":::gallery")) {
		opts := markdown.GalleryOptions{Resolver: s.galleryPageResolver(), Years: s.galleryYears}
		if res, err := markdown.RenderWithGalleryOptions(md, opts); err == nil {
			body = res.HTML
		}
	}
	return rewriteHeadlessText(body, attachments)
}

func rewriteHeadlessText(text string, attachments *attachmentExport) string {
	if attachments == nil || text == "" {
		return text
	}
	return string(attachments.rewriteLinks([]byte(text)))
}

func normalizeHeadlessLang(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		return "ja"
	}
	return lang
}

func formatHeadlessTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
