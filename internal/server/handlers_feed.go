package server

import (
	"encoding/xml"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
)

const (
	// feedItems は RSS フィードに載せる最新記事の件数。
	feedItems = 20
	// sitemapMaxURLs はサイトマップ1ファイルに載せられる URL の上限（sitemaps.org の仕様）。
	sitemapMaxURLs = 50000
	// feedCacheControl はフィード・サイトマップ・robots.txt の Cache-Control。
	feedCacheControl = "public, max-age=300"
)

type rssFeed struct {
	XMLName   xml.Name   `xml:"rss"`
	Version   string     `xml:"version,attr"`
	AtomNS    string     `xml:"xmlns:atom,attr"`
	ContentNS string     `xml:"xmlns:content,attr"`
	DCNS      string     `xml:"xmlns:dc,attr"`
	Channel   rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	Language      string    `xml:"language"`
	LastBuildDate string    `xml:"lastBuildDate,omitempty"`
	AtomLink      atomLink  `xml:"atom:link"`
	Items         []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	GUID        rssGUID  `xml:"guid"`
	PubDate     string   `xml:"pubDate"`
	Creator     string   `xml:"dc:creator,omitempty"`
	Category    string   `xml:"category,omitempty"`
	Description string   `xml:"description"`
	Content     cdataStr `xml:"content:encoded"`
}

type rssGUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

// cdataStr は本文 HTML を CDATA セクションとして出力する。
type cdataStr struct {
	Value string `xml:",cdata"`
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	NS      string       `xml:"xmlns,attr"`
	XHTMLNS string       `xml:"xmlns:xhtml,attr,omitempty"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapLink struct {
	Rel      string `xml:"rel,attr"`
	Hreflang string `xml:"hreflang,attr"`
	Href     string `xml:"href,attr"`
}

type sitemapURL struct {
	Loc     string        `xml:"loc"`
	LastMod string        `xml:"lastmod,omitempty"`
	Links   []sitemapLink `xml:"xhtml:link,omitempty"`
}

// handleFeed は最新の公開記事の RSS 2.0 フィードを返す。固定ページは時系列の記事ではないため載せない。
func (s *Server) handleFeed(c echo.Context) error {
	articles := []publicArticle{}
	err := s.db.Select(&articles, `SELECT `+publicArticleColumns+publicArticleFrom+`WHERE `+postKindCondition+` AND `+publishedCondition+
		s.enExcludedCondition()+` ORDER BY julianday(a.published_at) DESC, a.id LIMIT ?`, article.StatusPublished, clockNow().UTC(), feedItems)
	if err != nil {
		return err
	}

	base := s.baseURL(c)
	siteName, siteDesc := s.getSiteInfo()
	ch := rssChannel{
		Title:       siteName,
		Link:        base + "/",
		Description: siteDesc,
		Language:    "ja",
		AtomLink:    atomLink{Href: base + "/feed.xml", Rel: "self", Type: "application/rss+xml"},
		Items:       make([]rssItem, 0, len(articles)),
	}
	if ch.Description == "" {
		ch.Description = siteName
	}
	if len(articles) > 0 {
		latest := articles[0].PublishedAt
		for _, a := range articles {
			if a.UpdatedAt.After(latest) {
				latest = a.UpdatedAt
			}
		}
		ch.LastBuildDate = latest.UTC().Format(time.RFC1123Z)
	}
	for _, a := range articles {
		link := base + a.URL()
		creator := ""
		if s.getShowAuthor() {
			creator = a.AuthorName
		}
		ch.Items = append(ch.Items, rssItem{
			Title:       a.Title,
			Link:        link,
			GUID:        rssGUID{IsPermaLink: "true", Value: link},
			PubDate:     a.PublishedAt.UTC().Format(time.RFC1123Z),
			Creator:     creator,
			Category:    a.CategoryName,
			Description: articleDescription(a, a.ContentHTML),
			// 保存時に markdown.Render でサニタイズ済み。本文中の "]]>" は encoding/xml が CDATA を分割して出力する。
			Content: cdataStr{Value: a.ContentHTML},
		})
	}
	feed := rssFeed{
		Version:   "2.0",
		AtomNS:    "http://www.w3.org/2005/Atom",
		ContentNS: "http://purl.org/rss/1.0/modules/content/",
		DCNS:      "http://purl.org/dc/elements/1.1/",
		Channel:   ch,
	}
	return writeXML(c, "application/rss+xml; charset=utf-8", feed)
}

// handleSitemap はトップ・カテゴリ・公開記事・公開ページの XML サイトマップを返す。
// トップに出しているページは / として載せ、/<slug> は載せない（/ へ転送するため）。
func (s *Server) handleSitemap(c echo.Context) error {
	base := s.baseURL(c)
	set := sitemapURLSet{
		NS:      "http://www.sitemaps.org/schemas/sitemap/0.9",
		XHTMLNS: "http://www.w3.org/1999/xhtml",
	}
	front, err := s.frontPageSlug(c.Request().Context())
	if err != nil {
		return err
	}
	var frontGroupID string
	if front != "" {
		_ = s.db.Get(&frontGroupID, `SELECT COALESCE(NULLIF(translation_group_id, ''), id) FROM articles WHERE slug = ? AND kind = ?`, front, article.KindPage)
	}

	cats, err := s.publicCategories()
	if err != nil {
		return err
	}
	var rows []struct {
		ID                 string       `db:"id"`
		Slug               string       `db:"slug"`
		UpdatedAt          time.Time    `db:"updated_at"`
		Kind               article.Kind `db:"kind"`
		Lang               string       `db:"lang"`
		TranslationGroupID string       `db:"translation_group_id"`
	}
	// トップ・記事の一覧・カテゴリを含めて上限に収まる件数だけ、新しい記事・ページから載せる。
	// 多言語（ホーム2件、記事一覧2件、カテゴリ各2件）の重複を考慮して安全に上限を計算する。
	err = s.db.Select(&rows, `SELECT a.id, a.slug, a.updated_at, a.kind,
		COALESCE(NULLIF(a.lang, ''), 'ja') AS lang,
		COALESCE(NULLIF(a.translation_group_id, ''), a.id) AS translation_group_id
		FROM articles a WHERE `+publishedCondition+s.enExcludedCondition()+
		` ORDER BY julianday(a.published_at) DESC, a.id LIMIT ?`,
		article.StatusPublished, clockNow().UTC(), max(sitemapMaxURLs-4-len(cats)*2, 0))
	if err != nil {
		return err
	}

	type itemInfo struct {
		Slug string
		Kind article.Kind
		Lang string
	}
	groupItems := make(map[string][]itemInfo)
	for _, r := range rows {
		groupItems[r.TranslationGroupID] = append(groupItems[r.TranslationGroupID], itemInfo{
			Slug: r.Slug,
			Kind: r.Kind,
			Lang: r.Lang,
		})
	}

	// トップの更新日時は、トップに並ぶ最新の記事（ページを除く）と、トップに出しているページのうち新しいほう。
	var homeMod time.Time
	for _, r := range rows {
		if r.Kind != article.KindPage {
			homeMod = r.UpdatedAt
			break
		}
	}
	for _, r := range rows {
		if r.Kind == article.KindPage && r.Slug == front && r.UpdatedAt.After(homeMod) {
			homeMod = r.UpdatedAt
		}
	}
	// 多言語対応が OFF なら、英語の記事は上の問い合わせで除いてあり、/en の URL と hreflang も載せない。
	hasTranslations := false
	for _, r := range rows {
		if r.Lang != "ja" && r.Lang != "" {
			hasTranslations = true
			break
		}
	}

	if !hasTranslations {
		set := sitemapURLSet{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
		home := sitemapURL{Loc: base + "/"}
		if !homeMod.IsZero() {
			home.LastMod = homeMod.UTC().Format(time.RFC3339)
		}
		set.URLs = append(set.URLs, home)
		for _, r := range rows {
			if r.Kind == article.KindPage {
				set.URLs = append(set.URLs, sitemapURL{Loc: base + articlesListPath})
				break
			}
		}
		for _, cat := range cats {
			set.URLs = append(set.URLs, sitemapURL{Loc: base + categoryPath(cat.Slug)})
		}
		for _, r := range rows {
			if publicURLPath(r.Kind, r.Slug, front) == "/" {
				continue
			}
			set.URLs = append(set.URLs, sitemapURL{Loc: base + contentPath(r.Kind, r.Slug), LastMod: r.UpdatedAt.UTC().Format(time.RFC3339)})
		}
		return writeXML(c, "application/xml; charset=utf-8", set)
	}

	homeLinks := []sitemapLink{
		{Rel: "alternate", Hreflang: "ja", Href: base + "/"},
		{Rel: "alternate", Hreflang: "en", Href: base + "/en/"},
		{Rel: "alternate", Hreflang: "x-default", Href: base + "/"},
	}
	home := sitemapURL{Loc: base + "/", Links: homeLinks}
	if !homeMod.IsZero() {
		home.LastMod = homeMod.UTC().Format(time.RFC3339)
	}
	set.URLs = append(set.URLs, home)

	homeEN := sitemapURL{Loc: base + "/en/", Links: homeLinks}
	if !homeMod.IsZero() {
		homeEN.LastMod = homeMod.UTC().Format(time.RFC3339)
	}
	set.URLs = append(set.URLs, homeEN)

	// 記事の一覧（/articles）は、ページのあるサイト（トップをページにできるサイト）でだけ載せる。
	// ページの無いサイトのサイトマップは今までと同じにする。
	for _, r := range rows {
		if r.Kind == article.KindPage {
			articlesLinks := []sitemapLink{
				{Rel: "alternate", Hreflang: "ja", Href: base + articlesListPath},
				{Rel: "alternate", Hreflang: "en", Href: base + "/en" + articlesListPath},
				{Rel: "alternate", Hreflang: "x-default", Href: base + articlesListPath},
			}
			set.URLs = append(set.URLs, sitemapURL{Loc: base + articlesListPath, Links: articlesLinks})
			set.URLs = append(set.URLs, sitemapURL{Loc: base + "/en" + articlesListPath, Links: articlesLinks})
			break
		}
	}
	for _, cat := range cats {
		catLinks := []sitemapLink{
			{Rel: "alternate", Hreflang: "ja", Href: base + categoryPath(cat.Slug)},
			{Rel: "alternate", Hreflang: "en", Href: base + "/en" + categoryPath(cat.Slug)},
			{Rel: "alternate", Hreflang: "x-default", Href: base + categoryPath(cat.Slug)},
		}
		set.URLs = append(set.URLs, sitemapURL{Loc: base + categoryPath(cat.Slug), Links: catLinks})
		set.URLs = append(set.URLs, sitemapURL{Loc: base + "/en" + categoryPath(cat.Slug), Links: catLinks})
	}
	for _, r := range rows {
		if r.Kind == article.KindPage {
			if r.Lang == "ja" && r.Slug == front {
				continue
			}
			if r.Lang == "en" && ((frontGroupID != "" && r.TranslationGroupID == frontGroupID) || r.Slug == front) {
				continue
			}
		}
		itemPath := contentPath(r.Kind, r.Slug)
		if r.Lang == "en" {
			itemPath = "/en" + itemPath
		}
		var links []sitemapLink
		group := groupItems[r.TranslationGroupID]
		if len(group) > 1 {
			var jaHref string
			for _, g := range group {
				gPath := contentPath(g.Kind, g.Slug)
				if g.Lang == "en" {
					gPath = "/en" + gPath
				}
				links = append(links, sitemapLink{
					Rel:      "alternate",
					Hreflang: g.Lang,
					Href:     base + gPath,
				})
				if g.Lang == "ja" {
					jaHref = base + gPath
				}
			}
			if jaHref != "" {
				links = append(links, sitemapLink{
					Rel:      "alternate",
					Hreflang: "x-default",
					Href:     jaHref,
				})
			}
		}
		set.URLs = append(set.URLs, sitemapURL{
			Loc:     base + itemPath,
			LastMod: r.UpdatedAt.UTC().Format(time.RFC3339),
			Links:   links,
		})
	}
	return writeXML(c, "application/xml; charset=utf-8", set)
}

// handleRobots は管理画面と検索結果をクロール対象から外し、サイトマップの場所を伝える robots.txt を返す。
func (s *Server) handleRobots(c echo.Context) error {
	body := "User-agent: *\nAllow: /\nDisallow: /admin/\nDisallow: /search\n\nSitemap: " + s.baseURL(c) + "/sitemap.xml\n"
	c.Response().Header().Set(echo.HeaderCacheControl, feedCacheControl)
	return c.String(http.StatusOK, body)
}

func writeXML(c echo.Context, contentType string, v any) error {
	out, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	c.Response().Header().Set(echo.HeaderCacheControl, feedCacheControl)
	return c.Blob(http.StatusOK, contentType, append([]byte(xml.Header), out...))
}
