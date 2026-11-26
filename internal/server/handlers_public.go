package server

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/microcosm-cc/bluemonday"

	"sorus/internal/article"
	"sorus/internal/image"
	"sorus/internal/markdown"
	"sorus/internal/mcp"
	"sorus/internal/search"
	"sorus/internal/theme"
	"sorus/static"
)

const (
	// publicPerPage は公開サイトの記事一覧の1ページあたりの件数。
	publicPerPage = 12
	// searchPerPage は検索結果の1ページあたりの件数。
	searchPerPage = 10
	// maxQueryRunes は検索語の最大文字数。長すぎる語でインデックスに負荷をかけないよう切り詰める。
	maxQueryRunes = 100
	// descriptionRunes は抜粋が無い記事で本文から作る meta description の最大文字数。
	descriptionRunes = 120

	// DefaultSiteName は Config.SiteName を省略したときのサイト名。
	DefaultSiteName = "sorus"
)

// publicPageNames は public/layout.html に埋め込んで描画するページテンプレート（templates/public/<name>.html）。
var publicPageNames = []string{"index", "front", "articles", "article", "page", "category", "search", "not_found"}

var publicTemplateFuncs = template.FuncMap{
	// asset は静的アセットの URL を、内容のハッシュ付き（/static/<name>?v=<hash>）で返す。
	"asset": static.URL,
	"formatDate": func(t time.Time, lang ...string) string {
		if t.IsZero() {
			return ""
		}
		if len(lang) > 0 && lang[0] == "en" {
			return t.In(jst).Format("Jan 2, 2006")
		}
		return t.In(jst).Format("2006年1月2日")
	},
	"upper": strings.ToUpper,
	"isoTime": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(jst).Format(time.RFC3339)
	},
	"fileSize": formatFileSize,
	// contentPath は記事（/articles/<slug>）・ページ（/<slug>）の公開 URL のパス。
	"contentPath": contentPath,
	// galleryImages はタグ tag の写真を partials/gallery.html に渡す形で返す。options は順に表示する最大の枚数（int）・
	// 並び順（latest・oldest・random）・並べ方（justified・masonry・cards。知らない値は既定の justified）。
	"galleryImages": func(p publicPage, tag string, options ...any) (galleryTemplateData, error) {
		data := galleryTemplateData{Tag: tag, Layout: galleryLayoutJustified, Images: []markdown.GalleryImage{}, Lang: p.Lang}
		query := markdown.GalleryQuery{Tag: tag, Sort: "latest"}
		if len(options) > 0 {
			if limit, ok := options[0].(int); ok && limit > 0 {
				query.Limit = min(limit, 100)
			}
		}
		if len(options) > 1 {
			if sort, ok := options[1].(string); ok {
				switch sort {
				case "latest", "oldest", "random":
					query.Sort = sort
				}
			}
		}
		if len(options) > 2 {
			if layout, ok := options[2].(string); ok {
				data.Layout = normalizeGalleryLayout(layout)
			}
		}
		if p.GalleryQueryResolver != nil {
			images, err := p.GalleryQueryResolver(query)
			data.Images = images
			return data, err
		}
		if p.GalleryResolver == nil {
			return data, nil
		}
		images, err := p.GalleryResolver(tag)
		data.Images = images
		return data, err
	},
	// galleryLayout は partials/gallery.html の並べ方を選べる値（justified・masonry・cards）にする。空・知らない値は
	// 既定の justified（クラス名に任意の文字列を出さない）。
	"galleryLayout": normalizeGalleryLayout,
}

// textPolicy は HTML からタグを除去して meta description 用のテキストを作る。
var textPolicy = bluemonday.StrictPolicy()

// publicPage は公開サイトの全ページに渡すデータ。SEO 用のメタ情報と、ページ固有の Data を持つ。
type publicPage struct {
	SiteName string
	// Title は <title> に出すページ名。空ならサイト名だけを出す。
	Title       string
	Description string
	// Canonical はこのページの正規 URL（絶対 URL）。
	Canonical string
	// OGType は og:type（website / article）。
	OGType  string
	OGImage string
	// NoIndex は検索結果ページなど、検索エンジンに登録させないページか。
	NoIndex bool
	// JSONLD は <script type="application/ld+json"> に出す構造化データ。
	JSONLD []any
	// Meta は追加の <meta name=... content=...>（論文記事の Google Scholar 向け citation_* など）。
	Meta []metaTag
	// Menu はサイト構成のナビゲーションメニュー（参照先を解決済み）。空ならナビにはカテゴリの一覧（Categories）を出す。
	// 子項目のある項目はドロップダウン（スマートフォンではハンバーガーメニューの中の開閉）で出す。
	Menu []publicMenuItem
	// FooterMenu はフッターのサイトマップの列（参照先を解決済み。見出しと、その子項目のリンク）。空なら出さない。
	FooterMenu []publicMenuItem
	// DocNav は固定ページ・記事のドキュメント表示（左のサイドバーと前後のページ送り）。固定ページの親子階層に入っている
	// ページは renderPageDetail が先に埋め（pageHierarchyFor）、それ以外は表示中のページがメニューの子項目のある項目
	// （セクション）に入っているときだけ renderPublic が埋める（buildDocNav）。nil なら今までどおりの見た目。
	DocNav *docNav
	// PageAncestors は固定ページのパンくずリストに出す祖先のページ（ルートが先頭、親が末尾。表示中のページは含まない）。
	// 親ページを持つ固定ページだけ renderPageDetail が埋める。トップに出しているページ（ホームと同じ URL）は含めない。
	PageAncestors []docNavLink
	// SearchIndexURL は静的エクスポートの検索用インデックス（/search-index.json）の URL。静的エクスポートのときだけ
	// renderPublic が埋め、Cmd+K の検索（public.js）はサーバーの /search の代わりにこれを static-search.js で絞り込む。
	SearchIndexURL string
	Categories     []publicCategory
	Query          string
	// Theme は <html data-theme> に出すテーマ ID。ThemeStylesheet はそのテーマ CSS の版付き URL、
	// ColorScheme は <meta name="color-scheme"> に出す配色モード、Layout は theme.json の宣言的レイアウト設定。
	Theme           string
	ThemeStylesheet string
	ColorScheme     string
	Layout          theme.ThemeLayout
	AnalyticsTag    template.HTML
	Copyright       string
	SiteStructure   SiteStructure
	// FaviconURL・FaviconType は <link rel="icon"> に出す Favicon の URL と MIME タイプ。
	FaviconURL  string
	FaviconType string
	// Preview は管理画面のテーマのプレビューとして描画しているときの情報（プレビューバー）。通常は nil。
	Preview *themePreview
	// WindowScript はデスクトップモードで、この画面を開いている画面として数えてもらう接続のスクリプト（WindowPath）。
	// デスクトップモードでないときと静的な書き出しでは空。
	WindowScript template.HTML
	// ShowFrontCardImages・ShowArticleCardImages・ShowRelatedCardImages は公開カード3種（トップのページカード・
	// 記事一覧のカード・関連記事のカード）にアイキャッチ画像を出すか。renderPublic が設定から埋める。
	ShowFrontCardImages   bool
	ShowArticleCardImages bool
	ShowRelatedCardImages bool
	// ArticlesLabel は記事の一覧（/articles）の呼び名（site_menu の articles 項目の表示名。無ければ「記事一覧」・
	// "Articles"）。ArticlesBackLabel は記事ページの「〜一覧へ戻る」、ArticlesAllLabel はトップの「〜一覧へ」の文言。
	// renderPublic がサイト構成と表示言語から埋める（articlesLabels）。
	ArticlesLabel     string
	ArticlesBackLabel string
	ArticlesAllLabel  string
	Data              any
	// structure はハンドラーが読み込み済みのサイト構成。renderPublic はこれを使い、1つの要求で設定を読み直さない。
	// nil なら renderPublic が読み込む。
	structure            *SiteStructure
	GalleryResolver      markdown.GalleryResolver
	GalleryQueryResolver markdown.GalleryQueryResolver

	// 多言語化・国際SEO
	Lang         string
	Translations []PublicTranslation
	Hreflangs    []HreflangTag
}

// PublicTranslation は言語スイッチャー用の各言語リンク。
type PublicTranslation struct {
	Lang      string // "ja", "en"
	URL       string // "/articles/foo" または "/en/articles/foo"
	Title     string
	Available bool
}

// HreflangTag は <link rel="alternate" hreflang="..." href="..."> タグ用。
type HreflangTag struct {
	Lang string // "ja", "en", "x-default"
	URL  string // フルURL
}

// metaTag は <meta name="Name" content="Content">。
type metaTag struct{ Name, Content string }

// galleryTemplateData は partials/gallery.html（{{template "gallery" .}}）に渡すフォトギャラリー1つ。
type galleryTemplateData struct {
	// Tag は写真を選んだメディアのタグ（data-gallery-tag）。空なら属性を出さない。
	Tag string
	// Layout は並べ方（justified・masonry・cards）。空・知らない値は既定の justified として描く。
	Layout string
	// Images は並べる写真。ThumbURL を一覧の src、FullURL を拡大の data-full に使い、Width・Height を属性に出す。
	Images []markdown.GalleryImage
	// Lang は表示言語（"en" なら英語の読み上げ文）。
	Lang string
}

// publicCategory はナビゲーションとカテゴリページに出すカテゴリ。
type publicCategory struct {
	Name        string `db:"name"`
	Slug        string `db:"slug"`
	Description string `db:"description"`
	// Current はナビゲーションで、表示中のカテゴリページを指すか（aria-current="page"）。
	Current bool `db:"-"`
}

// Path はカテゴリページの URL パスを返す。lang が "en" なら /en/categories/<slug>。
func (c publicCategory) Path(lang ...string) string {
	if len(lang) > 0 && lang[0] == "en" {
		return "/en" + categoryPath(c.Slug)
	}
	return categoryPath(c.Slug)
}

// publicArticle は公開記事の1件。著者名とカテゴリを結合して読む。
type publicArticle struct {
	ID            string    `db:"id"`
	Title         string    `db:"title"`
	Slug          string    `db:"slug"`
	Excerpt       string    `db:"excerpt"`
	FeaturedImage string    `db:"featured_image_url"`
	Markdown      string    `db:"content_markdown"`
	ContentHTML   string    `db:"content_html"`
	AuthorName    string    `db:"author_name"`
	CategoryName  string    `db:"category_name"`
	CategorySlug  string    `db:"category_slug"`
	PublishedAt   time.Time `db:"published_at"`
	UpdatedAt     time.Time `db:"updated_at"`
	// 学術メタデータ（論文・学会発表）。
	DOI            string `db:"doi"`
	JournalName    string `db:"journal_name"`
	ConferenceName string `db:"conference_name"`
	CoAuthors      string `db:"co_authors"`
	// ImageHTML はアイキャッチの <img>（srcset 付き）。アイキャッチが無ければ空。
	ImageHTML template.HTML `db:"-"`
	// Kind は記事の種類。ページ（page）は /<slug> で配信する。
	Kind article.Kind `db:"kind"`
	// HideCardMedia は記事カード（partials/article_card.html）で画像の枠を出さないか。
	// 記事一覧のカードの画像表示を切ったとき renderPublic が立てる。既定（false）は画像を出す。
	HideCardMedia bool `db:"-"`

	// 多言語化・翻訳クラスタ
	Lang               string `db:"lang"`
	TranslationGroupID string `db:"translation_group_id"`
	// ParentID は固定ページの親ページの ID（最上位のページと記事では空）。公開ページの階層ナビゲーション
	// （pageHierarchyFor）に使う。
	ParentID string `db:"parent_id"`
}

// URL は記事詳細ページ（ページなら /<slug>）のパス。英語記事は /en プレフィックスを付ける。
func (a publicArticle) URL() string {
	base := contentPath(a.Kind, a.Slug)
	if a.Lang == "en" {
		return "/en" + base
	}
	return base
}

// CategoryURL はカテゴリページのパス。英語記事なら /en/categories/<slug>。
func (a publicArticle) CategoryURL() string {
	if a.CategorySlug == "" {
		return ""
	}
	if a.Lang == "en" {
		return "/en/categories/" + url.PathEscape(a.CategorySlug)
	}
	return categoryPath(a.CategorySlug)
}

// IsScholarly は学術メタデータを持つ論文・学会発表の記事か。
func (a publicArticle) IsScholarly() bool {
	return a.DOI != "" || a.JournalName != "" || a.ConferenceName != "" || a.CoAuthors != ""
}

// CoAuthorList は共著者をカンマ・読点・改行で区切った一覧。
func (a publicArticle) CoAuthorList() []string {
	var names []string
	for _, n := range strings.FieldsFunc(a.CoAuthors, func(r rune) bool {
		return r == ',' || r == '、' || r == '，' || r == '\n' || r == ';'
	}) {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// DOIURL は DOI の解決用 URL。DOI が無ければ空。
func (a publicArticle) DOIURL() string {
	if a.DOI == "" {
		return ""
	}
	return "https://doi.org/" + a.DOI
}

// pagination は partials/pagination.html に渡すデータ。前後ページが無ければ URL は空。
type pagination struct {
	Page       int
	TotalPages int
	PrevURL    string
	NextURL    string
	Lang       string
	// Pages はページ番号のリンクの並び（先頭・末尾・現在のページの前後。離れた所は省略の印）。2ページ以上のときだけ埋める。
	Pages []pageLink
}

// pageLink はページ送りの番号1つ。Gap なら省略の印（…）で、Num・URL は使わない。
type pageLink struct {
	Num     int
	URL     string
	Current bool
	Gap     bool
}

// pageWindowRadius は、ページ番号のリンクに現在のページの前後それぞれ何ページまで出すか。
const pageWindowRadius = 2

// pageLinks は全 total ページのうち current ページを表示しているときのページ番号の並びを返す。先頭・末尾と現在のページの
// 前後 pageWindowRadius ページを出し、間が2ページ以上離れたところは省略の印にする（1ページだけ離れるならその番号を出す）。
// 例: 47 ページ中の 6 ページ目は 1 … 4 5 [6] 7 8 … 47。pageURL は番号のリンク先。1ページだけなら空。
func pageLinks(current, total int, pageURL func(int) string) []pageLink {
	if total < 2 {
		return nil
	}
	current = min(max(current, 1), total)
	lo, hi := max(current-pageWindowRadius, 1), min(current+pageWindowRadius, total)
	var nums []int
	add := func(from, to int) {
		for n := from; n <= to; n++ {
			if len(nums) == 0 || n > nums[len(nums)-1] {
				nums = append(nums, n)
			}
		}
	}
	add(1, 1)
	if lo == 3 {
		add(2, 2)
	}
	add(lo, hi)
	if hi == total-2 {
		add(total-1, total-1)
	}
	add(total, total)
	links := make([]pageLink, 0, len(nums)+2)
	for i, n := range nums {
		if i > 0 && n > nums[i-1]+1 {
			links = append(links, pageLink{Gap: true})
		}
		links = append(links, pageLink{Num: n, URL: pageURL(n), Current: n == current})
	}
	return links
}

type indexData struct {
	Articles   []publicArticle
	Pagination pagination
}

// articlesData は記事の一覧（/articles・/en/articles、articles.html）に渡すデータ。
// カテゴリ（?category=<slug>）と年（?year=YYYY）で絞り込める。
type articlesData struct {
	Articles   []publicArticle
	Pagination pagination
	// ArchiveBaseURL は絞り込みの無い記事の一覧のパス（/articles か /en/articles）。
	ArchiveBaseURL string
	// Categories は現在の言語で公開中の記事があるカテゴリ（選んでいる年の記事数付き）。
	Categories []archiveCategory
	// ArchiveYears は公開中の記事がある年（選んでいるカテゴリの記事数付き）。
	ArchiveYears []archiveYear
	// CurrentCategory・CurrentCategoryName は選んでいるカテゴリのスラッグと名前。CurrentYear は選んでいる年。空なら絞り込まない。
	CurrentCategory     string
	CurrentCategoryName string
	CurrentYear         string
	// AllCategoriesURL は年を保ったままカテゴリの絞り込みを外す URL、CategoryTotal はその記事数。
	AllCategoriesURL string
	CategoryTotal    int
	// AllYearsURL はカテゴリを保ったまま年の絞り込みを外す URL、TotalArchiveCount はその記事数。
	AllYearsURL       string
	TotalArchiveCount int
}

type archiveYear struct {
	Year    string
	Count   int
	URL     string
	Current bool
}

// archiveCategory は記事の一覧のカテゴリの絞り込みの1項目。
type archiveCategory struct {
	Name    string `db:"name"`
	Slug    string `db:"slug"`
	Count   int    `db:"count"`
	URL     string `db:"-"`
	Current bool   `db:"-"`
}

// articlesFilterURL は記事の一覧 base をカテゴリ category・年 year で絞り込む URL。空の値は付けない。
func articlesFilterURL(base, category, year string) string {
	q := url.Values{}
	if category != "" {
		q.Set("category", category)
	}
	if year != "" {
		q.Set("year", year)
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

type categoryData struct {
	Category   publicCategory
	Articles   []publicArticle
	Pagination pagination
}

type articleData struct {
	Article         publicArticle
	RelatedArticles []publicArticle
	HTML            template.HTML
	Headings        []markdown.Heading
	// HasMath は本文に数式があり、KaTeX を読み込むか。
	HasMath bool
	// HasMermaid は本文に Mermaid の図があり、Mermaid を読み込むか。
	HasMermaid bool
	// Attachments は記事末尾のダウンロード欄に出す添付資料。
	Attachments []attachmentView
	// GeneratedOGImage は og:image がアイキャッチではなく動的 OGP 画像（1200×630 の PNG）か。
	GeneratedOGImage bool
	// ShowHero はタイトル直下にアイキャッチ（.hero）を出すか。アイキャッチがあり、本文に画像が1枚も無いときだけ true。
	// WordPress から移した記事のアイキャッチは一覧カード用の小さなサムネイルで、本文にも同じ写真が（別の URL でも）
	// 置かれていることが多い。本文に写真がある記事は本文の配置に任せ、重複や小画像の引き伸ばしを避ける。
	ShowHero bool
}

// searchData は search.html と partials/search_results.html に渡すデータ。
type searchData struct {
	Query      string
	Result     *search.SearchResult
	Pagination pagination
	// Static は静的エクスポート用の検索ページか。結果はサーバーで作らず、static-search.js が
	// /search-index.json を読んでブラウザで絞り込む。
	Static bool
	Lang   string
}

// publicArticleColumns は publicArticle を読む SELECT 句。articles a / users u / categories c を結合して使う。
const publicArticleColumns = `a.id, a.title, a.slug, a.excerpt, a.featured_image_url, a.content_markdown, a.content_html,
	COALESCE(NULLIF(u.display_name, ''), u.username, '') AS author_name,
	COALESCE(c.name, '') AS category_name, COALESCE(c.slug, '') AS category_slug, a.kind,
	a.published_at, a.updated_at, a.doi, a.journal_name, a.conference_name, a.co_authors,
	COALESCE(NULLIF(a.lang, ''), 'ja') AS lang, COALESCE(NULLIF(a.translation_group_id, ''), a.id) AS translation_group_id,
	COALESCE(a.parent_id, '') AS parent_id`

// publishedCondition は公開中で公開日時を迎えた（読者に見える）記事の WHERE 条件（article.VisibleCondition）。
// プレースホルダは状態・現在時刻の順で、現在時刻には clockNow を使う。
const publishedCondition = article.VisibleCondition

// clockNow は読者に見せるか（公開日時を迎えたか）を決める現在時刻を返す。試験で時計を差し替える。
var clockNow = time.Now

// postKindCondition は記事（kind = post）に限る WHERE 条件。時系列の一覧（トップ・記事一覧・カテゴリ・RSS・
// 関連記事）はすべてこの条件を付け、固定ページを出さない。
const postKindCondition = "a.kind = 'post'"

const publicArticleFrom = ` FROM articles a
	LEFT JOIN users u ON u.id = a.author_id
	LEFT JOIN categories c ON c.id = a.category_id `

// currentLang はリクエストのパスから現在の言語コード（"ja" または "en"）を判定する。
func currentLang(c echo.Context) string {
	if c == nil || c.Request() == nil {
		return "ja"
	}
	p := c.Request().URL.Path
	if p == "/en" || strings.HasPrefix(p, "/en/") {
		return "en"
	}
	return "ja"
}

// handlePublicIndex は最新の公開記事一覧（トップページ）を返す。
// ?category=<slug> はカテゴリページへ恒久的に転送し、正規 URL を1つにする。
func (s *Server) handlePublicIndex(c echo.Context) error {
	lang := currentLang(c)
	if slug := strings.TrimSpace(c.QueryParam("category")); slug != "" {
		dest := categoryPath(slug)
		if lang == "en" {
			dest = "/en" + dest
		}
		return c.Redirect(http.StatusMovedPermanently, dest)
	}
	pageNum, ok := pageParam(c)
	if !ok {
		return echo.ErrNotFound
	}
	return s.publicIndex(c, pageNum)
}

// publicIndex はトップページの pageNum ページ目を返す。2ページ目以降は /page/N（旧形式の /?page=N も同じ内容）。
func (s *Server) publicIndex(c echo.Context, pageNum int) error {
	lang := currentLang(c)
	st := s.loadSiteStructure(c.Request().Context())
	front, hasFront, err := s.frontPage(st)
	if err != nil {
		return err
	}
	if hasFront && lang == "en" {
		var enFront publicArticle
		err := s.db.Get(&enFront, `SELECT `+publicArticleColumns+publicArticleFrom+
			`WHERE a.translation_group_id = ? AND a.lang = 'en' AND `+publishedCondition,
			front.TranslationGroupID, article.StatusPublished, clockNow().UTC())
		if err == nil {
			front = enFront
		} else {
			hasFront = false
		}
	}
	topPath := "/"
	articlesPath := articlesListPath
	if lang == "en" {
		topPath = "/en/"
		articlesPath = "/en" + articlesListPath
	}
	// トップに固定ページを出すサイトでは、トップにページ送りが無い。以前の新着記事の /?page=N は、同じ並びの
	// 記事の一覧（/articles?page=N）へ一時的に転送する。サイト構成を戻せば元の /?page=N に戻るため、恒久的な 301 にしない。
	if hasFront && pageNum > 1 {
		return c.Redirect(http.StatusFound, pagedURL(articlesPath, pageNum))
	}
	var (
		articles []publicArticle
		pg       pagination
	)
	if !hasFront {
		basePath := "/"
		if lang == "en" {
			basePath = "/en"
		}
		articles, pg, err = s.listPublic(c, 0, "", pageNum, basePath)
		if err != nil {
			return err
		}
		if pageNum > 1 && len(articles) == 0 {
			return echo.ErrNotFound
		}
	}
	base := s.baseURL(c)
	canonical := pagedURL(base+topPath, pageNum)
	p := s.newPublicPage("", s.getSiteDescription(), canonical)
	p.Lang = lang
	if s.hasPublishedEnContent() {
		p.Translations = []PublicTranslation{
			{Lang: "ja", URL: "/", Title: "日本語", Available: true},
			{Lang: "en", URL: "/en/", Title: "English", Available: true},
		}
		p.Hreflangs = []HreflangTag{
			{Lang: "ja", URL: base + "/"},
			{Lang: "en", URL: base + "/en/"},
			{Lang: "x-default", URL: base + "/"},
		}
	}
	searchTarget := base + "/search?q={search_term_string}"
	if lang == "en" {
		searchTarget = base + "/en/search?q={search_term_string}"
	}
	p.JSONLD = []any{map[string]any{
		"@context": "https://schema.org",
		"@type":    "WebSite",
		"name":     s.siteNameFor(lang),
		"url":      base + topPath,
		"potentialAction": map[string]any{
			"@type":       "SearchAction",
			"target":      searchTarget,
			"query-input": "required name=search_term_string",
		},
	}}
	p.structure = &st
	if hasFront {
		// トップに出したページの閲覧は、/<slug> で開いたときと同じくそのページの PV に数える。
		if !isThemePreview(c) {
			s.recordView(c, front.ID)
		}
		return s.renderFront(c, p, front, st)
	}
	if pageNum > 1 {
		if lang == "en" {
			p.Title = fmt.Sprintf("Latest Articles (Page %d)", pageNum)
		} else {
			p.Title = fmt.Sprintf("新着記事（%dページ目）", pageNum)
		}
	}
	p.Data = indexData{Articles: articles, Pagination: pg}
	return s.renderPublic(c, http.StatusOK, "index", p)
}

// handlePublicArticles は記事の一覧（/articles）を返す。トップに固定ページを出すサイトでも記事の一覧へ行けるよう、
// トップの新着記事と同じ部品で全カテゴリの記事を公開日時の新しい順に並べる。
func (s *Server) handlePublicArticles(c echo.Context) error {
	pageNum, ok := pageParam(c)
	if !ok {
		return echo.ErrNotFound
	}
	return s.publicArticles(c, pageNum)
}

// publicArticles は記事の一覧の pageNum ページ目を返す。2ページ目以降は /articles/page/N（旧形式の ?page=N も同じ内容）。
func (s *Server) publicArticles(c echo.Context, pageNum int) error {
	lang := currentLang(c)
	articlesPath := articlesListPath
	if lang == "en" {
		articlesPath = "/en" + articlesListPath
	}
	year := c.QueryParam("year")
	if year != "" && !validArchiveYear(year) {
		return echo.ErrNotFound
	}
	// ?category=<slug> は年と組み合わせて絞り込める。存在しないカテゴリは 404。
	var cat struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
		Slug string `db:"slug"`
	}
	if slug := strings.TrimSpace(c.QueryParam("category")); slug != "" {
		err := s.db.Get(&cat, `SELECT id, name, slug FROM categories WHERE slug = ?`, slug)
		if errors.Is(err, sql.ErrNoRows) {
			return echo.ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	listPath := articlesFilterURL(articlesPath, cat.Slug, year)
	articles, pg, err := s.listPublic(c, cat.ID, year, pageNum, listPath)
	if err != nil {
		return err
	}
	if pageNum > 1 && len(articles) == 0 {
		return echo.ErrNotFound
	}
	base := s.baseURL(c)
	st := s.loadSiteStructure(c.Request().Context())
	title := articlesLabels(st, lang).Label
	desc := title + " | " + s.getSiteName()
	if lang == "en" {
		desc = title + " | " + s.siteNameFor(lang)
	}
	canonical := paginationURL(base+listPath, pageNum)
	p := s.newPublicPage(title, desc, canonical)
	p.structure = &st
	p.Lang = lang
	if s.hasPublishedEnContent() {
		p.Translations = []PublicTranslation{
			{Lang: "ja", URL: articlesListPath, Title: "日本語", Available: true},
			{Lang: "en", URL: "/en" + articlesListPath, Title: "English", Available: true},
		}
		p.Hreflangs = []HreflangTag{
			{Lang: "ja", URL: base + articlesListPath},
			{Lang: "en", URL: base + "/en" + articlesListPath},
			{Lang: "x-default", URL: base + articlesListPath},
		}
	}
	if pageNum > 1 {
		if lang == "en" {
			p.Title = fmt.Sprintf("%s (Page %d)", title, pageNum)
		} else {
			p.Title = fmt.Sprintf("%s（%dページ目）", title, pageNum)
		}
	}
	homeLabel := "ホーム"
	topURL := base + "/"
	if lang == "en" {
		homeLabel = "Home"
		topURL = base + "/en/"
	}
	p.JSONLD = []any{breadcrumbList(
		crumb{homeLabel, topURL},
		crumb{title, base + articlesPath},
	)}
	data := articlesData{
		Articles: articles, Pagination: pg, ArchiveBaseURL: articlesPath,
		CurrentCategory: cat.Slug, CurrentCategoryName: cat.Name, CurrentYear: year,
		AllCategoriesURL: articlesFilterURL(articlesPath, "", year),
		AllYearsURL:      articlesFilterURL(articlesPath, cat.Slug, ""),
	}
	if data.ArchiveYears, data.TotalArchiveCount, err = s.publicArchiveYears(c, articlesPath, cat.ID, cat.Slug, year); err != nil {
		return err
	}
	if data.Categories, data.CategoryTotal, err = s.publicArchiveCategories(c, articlesPath, cat.Slug, cat.Name, year); err != nil {
		return err
	}
	p.Data = data
	return s.renderPublic(c, http.StatusOK, "articles", p)
}

// handlePublicCategory はカテゴリ別の公開記事一覧を返す。2ページ目以降は /categories/<slug>/page/N
// （旧形式の ?page=N も同じ内容）。
func (s *Server) handlePublicCategory(c echo.Context) error {
	slug, pageNum, ok := splitPagedParam(pathParam(c, "slug"))
	if !ok || slug == "" {
		return echo.ErrNotFound
	}
	if pageNum == 0 {
		if pageNum, ok = pageParam(c); !ok {
			return echo.ErrNotFound
		}
	}
	var cat struct {
		ID int64 `db:"id"`
		publicCategory
	}
	err := s.db.Get(&cat, `SELECT id, name, slug, description FROM categories WHERE slug = ?`, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	lang := currentLang(c)
	basePath := categoryPath(cat.Slug)
	if lang == "en" {
		basePath = "/en" + basePath
	}
	articles, pg, err := s.listPublic(c, cat.ID, "", pageNum, basePath)
	if err != nil {
		return err
	}
	if pageNum > 1 && len(articles) == 0 {
		return echo.ErrNotFound
	}

	base := s.baseURL(c)
	canonical := pagedURL(base+basePath, pageNum)
	desc := cat.Description
	if desc == "" {
		if lang == "en" {
			desc = cat.Name + " | " + s.siteNameFor(lang)
		} else {
			desc = cat.Name + "の記事一覧 | " + s.getSiteName()
		}
	}
	p := s.newPublicPage(cat.Name, desc, canonical)
	p.Lang = lang
	if s.hasPublishedEnContent() {
		p.Translations = []PublicTranslation{
			{Lang: "ja", URL: categoryPath(cat.Slug), Title: "日本語", Available: true},
			{Lang: "en", URL: "/en" + categoryPath(cat.Slug), Title: "English", Available: true},
		}
		p.Hreflangs = []HreflangTag{
			{Lang: "ja", URL: base + categoryPath(cat.Slug)},
			{Lang: "en", URL: base + "/en" + categoryPath(cat.Slug)},
			{Lang: "x-default", URL: base + categoryPath(cat.Slug)},
		}
	}
	if pageNum > 1 {
		if lang == "en" {
			p.Title = fmt.Sprintf("%s (Page %d)", cat.Name, pageNum)
		} else {
			p.Title = fmt.Sprintf("%s（%dページ目）", cat.Name, pageNum)
		}
	}
	homeLabel := "ホーム"
	topURL := base + "/"
	if lang == "en" {
		homeLabel = "Home"
		topURL = base + "/en/"
	}
	p.JSONLD = []any{breadcrumbList(
		crumb{homeLabel, topURL},
		crumb{cat.Name, base + basePath},
	)}
	p.Data = categoryData{Category: cat.publicCategory, Articles: articles, Pagination: pg}
	return s.renderPublic(c, http.StatusOK, "category", p)
}

// publishedBySlugAndLang はスラッグと指定言語の、公開中で公開日時を迎えた記事・ページを返す。
func (s *Server) publishedBySlugAndLang(slug, lang string) (publicArticle, error) {
	var a publicArticle
	var err error
	if lang == "en" {
		err = s.db.Get(&a, `SELECT `+publicArticleColumns+publicArticleFrom+
			`WHERE a.slug = ? AND a.lang = 'en' AND `+publishedCondition, slug, article.StatusPublished, clockNow().UTC())
	} else {
		err = s.db.Get(&a, `SELECT `+publicArticleColumns+publicArticleFrom+
			`WHERE a.slug = ? AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL) AND `+publishedCondition, slug, article.StatusPublished, clockNow().UTC())
	}
	return a, err
}

// publishedBySlug はスラッグが slug の、公開中で公開日時を迎えた記事・ページを返す。無ければ sql.ErrNoRows を返す。
func (s *Server) publishedBySlug(slug string) (publicArticle, error) {
	a, err := s.publishedBySlugAndLang(slug, "ja")
	if errors.Is(err, sql.ErrNoRows) {
		return s.publishedBySlugAndLang(slug, "en")
	}
	return a, err
}

// hasPublishedEnContent はサイト全体に公開中の英語コンテンツ（記事またはページ）が1件以上存在するかを返す。
// 多言語対応を OFF にしたサイト（isMultilingualEnabled）では、英語の記事があっても false を返し、言語の切り替え
// （Translations）と hreflang を出さない。
func (s *Server) hasPublishedEnContent() bool {
	if !s.isMultilingualEnabled() {
		return false
	}
	var count int
	err := s.db.Get(&count, `SELECT COUNT(*) FROM articles a WHERE a.lang = 'en' AND `+publishedCondition, article.StatusPublished, clockNow().UTC())
	return err == nil && count > 0
}

// enExcludedCondition は多言語対応が OFF のとき、英語の記事・ページ（/en 以下で配信するもの）を除く SQL の条件
// （" AND ..."。articles の別名は a）を返す。ON なら空文字。フィード・サイトマップ・llms.txt に、OFF では開けない
// /en の URL を載せないために使う。
func (s *Server) enExcludedCondition() string {
	if s.isMultilingualEnabled() {
		return ""
	}
	return " AND COALESCE(NULLIF(a.lang, ''), 'ja') <> 'en'"
}

// buildTranslationsAndHreflangs は同一翻訳グループの公開記事から言語スイッチャー用リンクと hreflang タグを生成する。
func (s *Server) buildTranslationsAndHreflangs(base string, a publicArticle) ([]PublicTranslation, []HreflangTag) {
	if !s.hasPublishedEnContent() {
		return nil, nil
	}
	var cluster []publicArticle
	if a.TranslationGroupID != "" {
		_ = s.db.Select(&cluster, `SELECT `+publicArticleColumns+publicArticleFrom+
			`WHERE a.translation_group_id = ? AND `+publishedCondition+
			` ORDER BY a.lang ASC, a.created_at ASC`, a.TranslationGroupID, article.StatusPublished, clockNow().UTC())
	}

	byLang := make(map[string]publicArticle)
	for _, item := range cluster {
		l := item.Lang
		if l == "" {
			l = "ja"
		}
		byLang[l] = item
	}
	curLang := a.Lang
	if curLang == "" {
		curLang = "ja"
	}
	byLang[curLang] = a

	supported := []string{"ja", "en"}
	for l := range byLang {
		if !slices.Contains(supported, l) {
			supported = append(supported, l)
		}
	}

	var trans []PublicTranslation
	var hreflangs []HreflangTag
	var jaURL string

	for _, l := range supported {
		if item, ok := byLang[l]; ok {
			u := item.URL()
			trans = append(trans, PublicTranslation{
				Lang:      l,
				URL:       u,
				Title:     item.Title,
				Available: true,
			})
			hreflangs = append(hreflangs, HreflangTag{
				Lang: l,
				URL:  base + u,
			})
			if l == "ja" {
				jaURL = base + u
			}
		} else {
			title := "英語翻訳は準備中です"
			if l == "ja" {
				title = "日本語翻訳は準備中です"
			}
			trans = append(trans, PublicTranslation{
				Lang:      l,
				URL:       "",
				Title:     title,
				Available: false,
			})
		}
	}

	if jaURL != "" {
		hreflangs = append(hreflangs, HreflangTag{
			Lang: "x-default",
			URL:  jaURL,
		})
	} else {
		hreflangs = append(hreflangs, HreflangTag{
			Lang: "x-default",
			URL:  base + a.URL(),
		})
	}

	return trans, hreflangs
}

// loadArticleBody は記事・ページの本文（目次付き）・添付資料・アイキャッチを読み込んだ articleData を返す。
// OGP 画像は p に設定する（アイキャッチが無ければタイトルを描いた動的 OGP 画像）。
// タグのギャラリーは、limit を書かなければ最初の一部だけを出して「もっと見る」で続きを読む（galleryRenderOptions）。
func (s *Server) loadArticleBody(c echo.Context, a publicArticle, p *publicPage, base string) (articleData, error) {
	// 目次の見出し ID と本文の ID を揃えるため、保存済み HTML ではなく Markdown から描画し直す。
	data := articleData{Article: a}
	if res, err := markdown.RenderWithGalleryOptions(a.Markdown, s.galleryRenderOptions(c)); err == nil {
		data.HTML = template.HTML(res.HTML)
		data.Headings = res.Headings
	} else {
		// 保存時に markdown.Render でサニタイズ済みの HTML。
		data.HTML = template.HTML(a.ContentHTML)
	}
	data.HasMath = markdown.HasMath(string(data.HTML))
	data.HasMermaid = markdown.HasMermaid(string(data.HTML))
	data.Attachments = []attachmentView{}
	if err := s.db.Select(&data.Attachments, `SELECT `+attachmentColumns+` FROM attachments
		WHERE article_id = ? ORDER BY julianday(created_at), id`, a.ID); err != nil {
		return data, err
	}
	// 本文より上のアイキャッチは LCP 要素になるため、遅延読み込みせず優先して取得させる。
	data.Article.ImageHTML = template.HTML(strings.Replace(
		string(s.responsiveImage(a.FeaturedImage, a.Title, containFitClass)), `loading="lazy"`, `fetchpriority="high"`, 1))
	data.ShowHero = data.Article.ImageHTML != "" && !contentHasImage(string(data.HTML), a.ContentHTML) &&
		!eyecatchInContent(a.FeaturedImage, string(data.HTML), a.ContentHTML)
	if a.FeaturedImage != "" {
		p.OGImage = absoluteURL(base, a.FeaturedImage)
	} else {
		// アイキャッチの無い記事も SNS で大きなカードになるよう、タイトルを描いた画像を使う。
		p.OGImage = base + ogImagePath(a.Slug)
		data.GeneratedOGImage = true
	}
	return data, nil
}

// imgTag は HTML の img 要素の開始タグに一致する（大文字小文字を問わない）。
var imgTag = regexp.MustCompile(`(?i)<img[\s/>]`)

// contentHasImage は contents のどれかに img 要素が1つでもあるかを返す。
func contentHasImage(contents ...string) bool {
	for _, content := range contents {
		if imgTag.MatchString(content) {
			return true
		}
	}
	return false
}

// imageRefAttr は本文 HTML の画像の参照（src・srcset・data-src・data-srcset の値）に一致する。
var imageRefAttr = regexp.MustCompile(`(?i)\b(src|srcset|data-src|data-srcset)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// imageVariantSuffix はファイル名の末尾にある、同じ画像の別サイズを表す接尾辞に一致する。
// 本 CMS の縮小版（-original・-1200・-800・-400）と、WordPress の縮小版・大きな画像の縮小（-300x200・-scaled）。
var imageVariantSuffix = regexp.MustCompile(fmt.Sprintf(`(?:-(?:original|%d|%d|%d)|(?:-scaled)?(?:-\d+x\d+)?)$`,
	image.WidthLarge, image.WidthMedium, image.WidthSmall))

// imageStem は画像の URL から、サイズ違いを同じとみなす比較用の名前（拡張子と縮小版の接尾辞を除いた小文字のファイル名）を返す。
func imageStem(ref string) string {
	ref = html.UnescapeString(strings.TrimSpace(ref))
	if i := strings.IndexAny(ref, "?#"); i >= 0 {
		ref = ref[:i]
	}
	if u, err := url.PathUnescape(ref); err == nil {
		ref = u
	}
	name := baseName(ref)
	if name == "" || name == "." || name == "/" {
		return ""
	}
	stem := strings.TrimSuffix(name, path.Ext(name))
	return strings.ToLower(imageVariantSuffix.ReplaceAllString(stem, ""))
}

// eyecatchInContent はアイキャッチ src と同じ画像（URL が同じか、サイズ違いの同じファイル）が本文 contents のいずれかに
// 画像として含まれているかを返す。src が空なら false。
func eyecatchInContent(src string, contents ...string) bool {
	src = strings.TrimSpace(src)
	if src == "" {
		return false
	}
	want := imageStem(src)
	for _, content := range contents {
		if content == "" {
			continue
		}
		if strings.Contains(content, src) || strings.Contains(content, html.EscapeString(src)) {
			return true
		}
		if want == "" {
			continue
		}
		for _, m := range imageRefAttr.FindAllStringSubmatch(content, -1) {
			value := m[2] + m[3]
			refs := []string{value}
			if strings.HasSuffix(strings.ToLower(m[1]), "srcset") {
				// srcset は「URL 幅」をカンマで区切った並び。各候補の先頭の URL を比べる。
				refs = refs[:0]
				for _, candidate := range strings.Split(value, ",") {
					if fields := strings.Fields(candidate); len(fields) > 0 {
						refs = append(refs, fields[0])
					}
				}
			}
			for _, ref := range refs {
				if imageStem(ref) == want {
					return true
				}
			}
		}
	}
	return false
}

// handlePublicArticle は記事詳細ページを返す。公開中で公開日時を迎えた記事だけを表示する。
// ページ（kind = page）は /<slug> へ恒久的に転送し、種類を変えても古いリンクが切れないようにする。
// 記事の一覧の2ページ目以降（/articles/page/N）もこのルートに届くため、記事の一覧を返す。
func (s *Server) handlePublicArticle(c echo.Context) error {
	slug, pageNum, ok := splitPagedParam(pathParam(c, "slug"))
	if !ok {
		return echo.ErrNotFound
	}
	if pageNum > 0 {
		if slug != "" {
			return echo.ErrNotFound
		}
		return s.publicArticles(c, pageNum)
	}
	lang := currentLang(c)
	a, err := s.publishedBySlugAndLang(slug, lang)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.Kind == article.KindPage {
		return c.Redirect(http.StatusMovedPermanently, a.URL())
	}
	return s.renderArticleDetail(c, a, lang)
}

// renderArticleDetail は記事 a の詳細ページを公開テーマで描画する。公開記事（handlePublicArticle）と、
// 管理画面の下書きプレビュー（handleArticleThemePreview）が使う。プレビューでは閲覧数を数えない。
func (s *Server) renderArticleDetail(c echo.Context, a publicArticle, lang string) error {
	base := s.baseURL(c)
	canonical := base + a.URL()
	p := s.newPublicPage(a.Title, "", canonical)
	p.Lang = lang
	p.Translations, p.Hreflangs = s.buildTranslationsAndHreflangs(base, a)

	data, err := s.loadArticleBody(c, a, &p, base)
	if err != nil {
		return err
	}
	data.RelatedArticles = s.relatedPublicArticles(c, a)
	desc := articleDescription(a, string(data.HTML))
	p.Description = desc
	p.OGType = "article"

	articleType := "NewsArticle"
	if a.IsScholarly() {
		articleType = "ScholarlyArticle"
	}
	topURL := base + "/"
	homeLabel := "ホーム"
	if lang == "en" {
		topURL = base + "/en/"
		homeLabel = "Home"
	}
	news := map[string]any{
		"@context":         "https://schema.org",
		"@type":            articleType,
		"headline":         a.Title,
		"description":      desc,
		"datePublished":    a.PublishedAt.In(jst).Format(time.RFC3339),
		"dateModified":     a.UpdatedAt.In(jst).Format(time.RFC3339),
		"mainEntityOfPage": map[string]any{"@type": "WebPage", "@id": canonical},
		"url":              canonical,
		"publisher":        map[string]any{"@type": "Organization", "name": s.siteNameFor(a.Lang), "url": topURL},
	}
	var authors []any
	if (s.getShowAuthor() || a.IsScholarly()) && a.AuthorName != "" {
		authors = append(authors, map[string]any{"@type": "Person", "name": a.AuthorName})
	}
	for _, name := range a.CoAuthorList() {
		authors = append(authors, map[string]any{"@type": "Person", "name": name})
	}
	if len(authors) > 0 {
		news["author"] = authors
	} else {
		news["author"] = map[string]any{"@type": "Organization", "name": s.siteNameFor(a.Lang), "url": topURL}
	}
	if a.IsScholarly() {
		addScholarlyFields(news, a, data.Attachments, base)
		p.Meta = scholarMeta(a, data.Attachments, base)
	}
	if p.OGImage != "" {
		news["image"] = []string{p.OGImage}
	}
	if a.CategoryName != "" {
		news["articleSection"] = a.CategoryName
	}
	crumbs := []crumb{{homeLabel, topURL}}
	if a.CategorySlug != "" {
		catURL := base + categoryPath(a.CategorySlug)
		if lang == "en" {
			catURL = base + "/en" + categoryPath(a.CategorySlug)
		}
		crumbs = append(crumbs, crumb{a.CategoryName, catURL})
	}
	crumbs = append(crumbs, crumb{a.Title, canonical})
	p.JSONLD = []any{news, breadcrumbList(crumbs...)}
	p.Data = data
	// 管理画面のテーマ・下書きのプレビューは読者の閲覧ではないため数えない。
	if !isThemePreview(c) && !isDraftPreview(c) {
		s.recordView(c, a.ID)
	}
	return s.renderPublic(c, http.StatusOK, "article", p)
}

// handlePublicPage は公開中のページ（/<slug>）を返す。目次・添付資料は記事と同じに出し、日付・著者・カテゴリ・
// 関連記事は出さない。スラッグが記事のものなら /articles/<slug> へ恒久的に転送する。
// トップに出しているページは / へ一時的に転送し、同じ内容の URL を1つにする（トップの設定は変えられるため 301 にしない）。
// テーマの上書き（エクスポートの --theme・管理画面のプレビュー）の内部要求は転送せずにページを描画する。
// 固定のパス（/search など）は先に登録したルートが受けるため、ここには届かない（ページのスラッグは予約語を使えない）。
// トップの新着記事の2ページ目以降（/page/N）もこのルートに届くため、トップページを返す。
func (s *Server) handlePublicPage(c echo.Context) error {
	slug, pageNum, ok := splitPagedParam(pathParam(c, "slug"))
	if !ok {
		return echo.ErrNotFound
	}
	if pageNum > 0 {
		if slug != "" {
			return echo.ErrNotFound
		}
		return s.publicIndex(c, pageNum)
	}
	if slug == "" || strings.Contains(slug, "/") {
		return echo.ErrNotFound
	}
	lang := currentLang(c)
	a, err := s.publishedBySlugAndLang(slug, lang)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.Kind != article.KindPage {
		return c.Redirect(http.StatusMovedPermanently, a.URL())
	}
	st := s.loadSiteStructure(c.Request().Context())
	if _, ok := themeOverrideFrom(c); !ok {
		front, hasFront, _ := s.frontPage(st)
		isFront := (st.FrontPage != "" && st.FrontPage == a.Slug) || (hasFront && front.TranslationGroupID != "" && front.TranslationGroupID == a.TranslationGroupID)
		if isFront {
			topURL := "/"
			if lang == "en" {
				topURL = "/en/"
			}
			return c.Redirect(http.StatusFound, topURL)
		}
	}
	return s.renderPageDetail(c, a, lang, st)
}

// renderPageDetail はページ a を公開テーマで描画する。公開ページ（handlePublicPage）と、管理画面の
// 下書きプレビュー（handleArticleThemePreview）が使う。プレビューでは閲覧数を数えない。
func (s *Server) renderPageDetail(c echo.Context, a publicArticle, lang string, st SiteStructure) error {
	base := s.baseURL(c)
	canonical := base + a.URL()
	p := s.newPublicPage(a.Title, "", canonical)
	p.Lang = lang
	p.Translations, p.Hreflangs = s.buildTranslationsAndHreflangs(base, a)
	p.structure = &st
	data, err := s.loadArticleBody(c, a, &p, base)
	if err != nil {
		return err
	}
	p.Description = articleDescription(a, string(data.HTML))
	topURL := base + "/"
	homeLabel := "ホーム"
	if lang == "en" {
		topURL = base + "/en/"
		homeLabel = "Home"
	}
	ld := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "WebPage",
		"name":        a.Title,
		"description": p.Description,
		"url":         canonical,
		"isPartOf":    map[string]any{"@type": "WebSite", "name": s.siteNameFor(a.Lang), "url": topURL},
	}
	if p.OGImage != "" {
		ld["primaryImageOfPage"] = map[string]any{"@type": "ImageObject", "url": p.OGImage}
	}
	// 親子階層に入っている固定ページは、サイドバー・ページ送りを階層から作り（メニューより優先）、パンくずリストに祖先を出す。
	h, err := s.pageHierarchyFor(c.Request().Context(), a, st)
	if err != nil {
		return err
	}
	p.DocNav = h.Nav
	crumbs := []crumb{{homeLabel, topURL}}
	homePath := strings.TrimPrefix(topURL, base)
	for _, anc := range h.Ancestors {
		// トップに出しているページはホームと同じ URL のため、ホームの後に重ねない。
		if anc.URL == homePath {
			continue
		}
		p.PageAncestors = append(p.PageAncestors, anc)
		crumbs = append(crumbs, crumb{anc.Label, base + anc.URL})
	}
	crumbs = append(crumbs, crumb{a.Title, canonical})
	p.JSONLD = []any{ld, breadcrumbList(crumbs...)}
	p.Data = data
	// 管理画面のテーマ・下書きのプレビューは読者の閲覧ではないため数えない。
	if !isThemePreview(c) && !isDraftPreview(c) {
		s.recordView(c, a.ID)
	}
	return s.renderPublic(c, http.StatusOK, "page", p)
}

// relatedPublicArticles は記事 a の関連記事（埋め込みのコサイン類似度と全文検索のハイブリッドで求め、
// キャッシュしたもの）を公開可能な記事カードに展開する。RAG が無効なら nil を返す。
func (s *Server) relatedPublicArticles(c echo.Context, a publicArticle) []publicArticle {
	if s.ragService == nil {
		return nil
	}
	articleID := a.ID
	related, err := s.findRelated(c.Request().Context(), a)
	if err != nil {
		c.Logger().Warnf("rag: related articles for %s: %v", articleID, err)
		return nil
	}
	if len(related) == 0 {
		return nil
	}
	ids := make([]string, 0, len(related))
	for _, r := range related {
		ids = append(ids, r.ArticleID)
	}
	query, args, err := sqlx.In(`SELECT `+publicArticleColumns+publicArticleFrom+`WHERE a.id IN (?) AND `+postKindCondition+
		` AND `+publishedCondition, ids, article.StatusPublished, clockNow().UTC())
	if err != nil {
		return nil
	}
	var rows []publicArticle
	if err := s.db.SelectContext(c.Request().Context(), &rows, query, args...); err != nil {
		c.Logger().Warnf("rag: load related articles for %s: %v", articleID, err)
		return nil
	}
	byID := make(map[string]publicArticle, len(rows))
	for _, row := range rows {
		row.ImageHTML = s.responsiveImage(row.FeaturedImage, row.Title, "related-image")
		row.Excerpt = articleDescription(row, row.ContentHTML)
		byID[row.ID] = row
	}
	ordered := make([]publicArticle, 0, len(rows))
	for _, r := range related {
		if row, ok := byID[r.ArticleID]; ok {
			ordered = append(ordered, row)
		}
	}
	return ordered
}

// addScholarlyFields は ScholarlyArticle の構造化データに DOI・掲載誌・学会・論文 PDF を加える。
func addScholarlyFields(ld map[string]any, a publicArticle, atts []attachmentView, base string) {
	if a.DOI != "" {
		ld["identifier"] = map[string]any{"@type": "PropertyValue", "propertyID": "DOI", "value": a.DOI}
		ld["sameAs"] = a.DOIURL()
	}
	if a.JournalName != "" {
		ld["isPartOf"] = map[string]any{"@type": "Periodical", "name": a.JournalName}
	}
	if a.ConferenceName != "" {
		ld["recordedAt"] = map[string]any{"@type": "Event", "name": a.ConferenceName}
	}
	var media []any
	for _, at := range atts {
		if at.IsPDF() {
			media = append(media, map[string]any{
				"@type": "MediaObject", "name": at.Filename, "encodingFormat": at.MimeType,
				"contentUrl": base + at.URL(), "contentSize": strconv.FormatInt(at.FileSize, 10) + " B",
			})
		}
	}
	if len(media) > 0 {
		ld["associatedMedia"] = media
	}
}

// scholarMeta は Google Scholar が論文の書誌情報として読む citation_* メタタグを返す。
func scholarMeta(a publicArticle, atts []attachmentView, base string) []metaTag {
	tags := []metaTag{{"citation_title", a.Title}}
	if a.AuthorName != "" {
		tags = append(tags, metaTag{"citation_author", a.AuthorName})
	}
	for _, name := range a.CoAuthorList() {
		tags = append(tags, metaTag{"citation_author", name})
	}
	tags = append(tags, metaTag{"citation_publication_date", a.PublishedAt.In(jst).Format("2006/01/02")})
	if a.JournalName != "" {
		tags = append(tags, metaTag{"citation_journal_title", a.JournalName})
	}
	if a.ConferenceName != "" {
		tags = append(tags, metaTag{"citation_conference_title", a.ConferenceName})
	}
	if a.DOI != "" {
		tags = append(tags, metaTag{"citation_doi", a.DOI})
	}
	for _, at := range atts {
		if at.IsPDF() {
			tags = append(tags, metaTag{"citation_pdf_url", base + at.URL()})
			break
		}
	}
	return tags
}

// handlePublicSearch はサイト内検索の結果を返す。HTMX（HX-Request ヘッダー）からの要求には
// 結果部分のパーシャルだけを返し、検索窓の入力に合わせて部分更新できるようにする。
func (s *Server) handlePublicSearch(c echo.Context) error {
	lang := currentLang(c)
	q := strings.TrimSpace(c.QueryParam("q"))
	if utf8.RuneCountInString(q) > maxQueryRunes {
		q = string([]rune(q)[:maxQueryRunes])
	}
	searchPath := "/search"
	if lang == "en" {
		searchPath = "/en/search"
	}
	if isStaticExport(c) {
		title := "検索"
		desc := s.getSiteName() + "のサイト内検索"
		if lang == "en" {
			title = "Search"
			desc = "Search | " + s.siteNameFor(lang)
		}
		p := s.newPublicPage(title, desc, s.baseURL(c)+searchPath)
		p.Lang = lang
		if s.hasPublishedEnContent() {
			p.Translations = []PublicTranslation{
				{Lang: "ja", URL: "/search", Title: "日本語", Available: true},
				{Lang: "en", URL: "/en/search", Title: "English", Available: true},
			}
		}
		p.NoIndex = true
		p.Data = searchData{Static: true, Lang: lang}
		return s.renderPublic(c, http.StatusOK, "search", p)
	}
	pageNum, ok := pageParam(c)
	if !ok {
		pageNum = 1
	}
	// 執筆者名を表示しない設定では、表示しない著者名で記事が一致したり件数・ページ数が変わったりしないよう、
	// 検索クエリの段階で著者名を照合から外す（論文記事は書誌情報として著者を出すため除く）。
	res, err := s.search.SearchWithOptions(c.Request().Context(), q, pageNum, searchPerPage,
		search.Options{Lang: lang, HideAuthor: !s.getShowAuthor()})
	if err != nil {
		return err
	}
	data := searchData{Query: q, Result: res, Pagination: pagination{Page: res.Page, TotalPages: res.TotalPages, Lang: lang}, Lang: lang}
	pageURL := func(n int) string {
		v := url.Values{"q": {q}}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		return searchPath + "?" + v.Encode()
	}
	if res.Page > 1 {
		data.Pagination.PrevURL = pageURL(res.Page - 1)
	}
	if res.Page < res.TotalPages {
		data.Pagination.NextURL = pageURL(res.Page + 1)
	}
	data.Pagination.Pages = pageLinks(res.Page, res.TotalPages, pageURL)

	// 同じ URL でもヘッダーにより全体とパーシャルを返し分けるため、キャッシュを分けさせる。
	c.Response().Header().Add(echo.HeaderVary, "HX-Request")
	if c.Request().Header.Get("HX-Request") == "true" {
		if c.Response().Header().Get("Content-Security-Policy") == "" {
			c.Response().Header().Set("Content-Security-Policy", s.publicCSP())
		}
		if !s.getShowAuthor() && data.Result != nil {
			for i := range data.Result.Hits {
				data.Result.Hits[i].Author = ""
			}
		}
		return c.Render(http.StatusOK, "public/search_results", data)
	}
	title := "検索"
	if q != "" {
		if lang == "en" {
			title = fmt.Sprintf("Search results for %q", q)
		} else {
			title = "「" + q + "」の検索結果"
		}
	} else if lang == "en" {
		title = "Search"
	}
	desc := s.getSiteName() + "のサイト内検索"
	if lang == "en" {
		desc = "Search | " + s.siteNameFor(lang)
	}
	p := s.newPublicPage(title, desc, s.baseURL(c)+searchPath)
	p.Lang = lang
	if s.hasPublishedEnContent() {
		p.Translations = []PublicTranslation{
			{Lang: "ja", URL: "/search", Title: "日本語", Available: true},
			{Lang: "en", URL: "/en/search", Title: "English", Available: true},
		}
	}
	p.NoIndex = true
	p.Query = q
	p.Data = data
	return s.renderPublic(c, http.StatusOK, "search", p)
}

// publicListCondition は現在の言語の、公開中の記事（ページを除く）の WHERE 条件と引数。
// categoryID が 0 でなければそのカテゴリ、year（YYYY）が空でなければその年の記事に絞る。
func publicListCondition(lang string, categoryID int64, year string) (string, []any) {
	where := postKindCondition + ` AND ` + publishedCondition
	args := []any{article.StatusPublished, clockNow().UTC()}
	if lang == "en" {
		where += ` AND a.lang = 'en'`
	} else {
		where += ` AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL)`
	}
	if categoryID != 0 {
		where += ` AND a.category_id = ?`
		args = append(args, categoryID)
	}
	if year != "" {
		where += ` AND strftime('%Y', a.published_at) = ?`
		args = append(args, year)
	}
	return where, args
}

// listPublic は公開記事（ページを除く）を公開日時の新しい順に pageNum ページ目だけ返す。categoryID が 0 なら全カテゴリ、
// year（検査済みの YYYY）が空なら全期間。basePath はページ送りのリンク先。
func (s *Server) listPublic(c echo.Context, categoryID int64, year string, pageNum int, basePath string) ([]publicArticle, pagination, error) {
	lang := currentLang(c)
	where, args := publicListCondition(lang, categoryID, year)
	var total int
	if err := s.db.Get(&total, `SELECT COUNT(*) FROM articles a WHERE `+where, args...); err != nil {
		return nil, pagination{}, err
	}
	articles := []publicArticle{}
	err := s.db.Select(&articles, `SELECT `+publicArticleColumns+publicArticleFrom+`WHERE `+where+
		` ORDER BY julianday(a.published_at) DESC, a.id LIMIT ? OFFSET ?`,
		append(args, publicPerPage, (pageNum-1)*publicPerPage)...)
	if err != nil {
		return nil, pagination{}, err
	}
	for i := range articles {
		articles[i].ImageHTML = s.responsiveImage(articles[i].FeaturedImage, articles[i].Title, "card-image")
	}

	pg := pagination{Page: pageNum, TotalPages: (total + publicPerPage - 1) / publicPerPage, Lang: lang}
	if pageNum > 1 {
		pg.PrevURL = paginationURL(basePath, pageNum-1)
	}
	if pageNum < pg.TotalPages {
		pg.NextURL = paginationURL(basePath, pageNum+1)
	}
	pg.Pages = pageLinks(pageNum, pg.TotalPages, func(n int) string { return paginationURL(basePath, n) })
	return articles, pg, nil
}

func validArchiveYear(year string) bool {
	if len(year) != 4 {
		return false
	}
	for _, r := range year {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(year)
	return err == nil && n >= 1000 && n <= 9999
}

// publicArchiveYears は記事の一覧の年別の絞り込み（新しい年から）と、全期間の記事数を返す。記事数は選んでいる
// カテゴリ（categoryID・categorySlug）の中で数え、各年の URL はそのカテゴリを保つ。選んでいる年 selected に記事が無くても
// 項目には残す（記事数 0）。
func (s *Server) publicArchiveYears(c echo.Context, basePath string, categoryID int64, categorySlug, selected string) ([]archiveYear, int, error) {
	where, args := publicListCondition(currentLang(c), categoryID, "")
	var years []struct {
		Year  string `db:"year"`
		Count int    `db:"count"`
	}
	if err := s.db.Select(&years, `SELECT strftime('%Y', a.published_at) AS year, COUNT(*) AS count
		FROM articles a WHERE `+where+` GROUP BY year ORDER BY year DESC`, args...); err != nil {
		return nil, 0, err
	}
	archives := make([]archiveYear, 0, len(years)+1)
	total, found := 0, selected == ""
	for _, y := range years {
		total += y.Count
		found = found || y.Year == selected
		archives = append(archives, archiveYear{Year: y.Year, Count: y.Count,
			URL: articlesFilterURL(basePath, categorySlug, y.Year), Current: y.Year == selected})
	}
	if !found {
		archives = append(archives, archiveYear{Year: selected, URL: articlesFilterURL(basePath, categorySlug, selected), Current: true})
		slices.SortStableFunc(archives, func(a, b archiveYear) int { return strings.Compare(b.Year, a.Year) })
	}
	return archives, total, nil
}

// publicArchiveCategories は記事の一覧のカテゴリの絞り込み（カテゴリの登録順）と、全カテゴリの記事数を返す。記事数は
// 選んでいる年 year の中で数え、各カテゴリの URL はその年を保つ。記事の無いカテゴリは出さないが、選んでいるカテゴリ
// （selectedSlug・selectedName）は記事が無くても残す（記事数 0）。
func (s *Server) publicArchiveCategories(c echo.Context, basePath, selectedSlug, selectedName, year string) ([]archiveCategory, int, error) {
	where, args := publicListCondition(currentLang(c), 0, year)
	var total int
	if err := s.db.Get(&total, `SELECT COUNT(*) FROM articles a WHERE `+where, args...); err != nil {
		return nil, 0, err
	}
	cats := []archiveCategory{}
	if err := s.db.Select(&cats, `SELECT c.name, c.slug, COUNT(*) AS count
		FROM articles a JOIN categories c ON c.id = a.category_id WHERE `+where+`
		GROUP BY c.id ORDER BY c.id`, args...); err != nil {
		return nil, 0, err
	}
	found := selectedSlug == ""
	for i := range cats {
		cats[i].URL = articlesFilterURL(basePath, cats[i].Slug, year)
		cats[i].Current = cats[i].Slug == selectedSlug
		found = found || cats[i].Current
	}
	if !found {
		cats = append(cats, archiveCategory{Name: selectedName, Slug: selectedSlug,
			URL: articlesFilterURL(basePath, selectedSlug, year), Current: true})
	}
	return cats, total, nil
}

// containFitClass は、図・縦長・極端な横長の画像に is-contain（余白をとって全体を収める）を付ける <img> のクラス。
// 記事ページのアイキャッチ（.hero）だけに付け、カード（記事一覧・トップの新着記事とページのカード・関連記事）の
// 画像には付けない。カードは枠（既定 16:9）いっぱいに object-fit: cover で切り抜き、上下左右に余白を作らない。
const containFitClass = "hero-image"

// responsiveImage はアイキャッチ URL の <img> を返す。アップロード済みの画像なら 1200/800/400px の
// WebP を srcset に並べ、それ以外の URL は単純な <img> にする。どちらも遅延読み込みにする。
func (s *Server) responsiveImage(src, alt, class string) template.HTML {
	if src == "" {
		return ""
	}
	if name, ok := strings.CutPrefix(src, UploadURLPrefix); ok && name != "" && !strings.Contains(name, "/") {
		var m struct {
			Original string `db:"path_original"`
			Large    string `db:"path_large"`
			Medium   string `db:"path_medium"`
			Small    string `db:"path_small"`
			Width    int    `db:"width"`
			Height   int    `db:"height"`
		}
		// 保存パスはアップロード先ディレクトリを含むため、ファイル名（末尾）で照合する。
		like := "%" + escapeLike(name)
		err := s.db.Get(&m, `SELECT path_original, path_large, path_medium, path_small, width, height FROM media
			WHERE path_large LIKE ? ESCAPE '\' OR path_medium LIKE ? ESCAPE '\'
				OR path_small LIKE ? ESCAPE '\' OR path_original LIKE ? ESCAPE '\'
			LIMIT 1`, like, like, like, like)
		// LIKE の後方一致では別名のファイル（"xa.webp" と "a.webp"）も拾うため、ファイル名の一致を確かめる。
		if err == nil && slices.Contains([]string{baseName(m.Original), baseName(m.Large), baseName(m.Medium), baseName(m.Small)}, name) {
			img := &image.ProcessedImage{
				OriginalPath: uploadURL(m.Original),
				LargePath:    uploadURL(m.Large),
				MediumPath:   uploadURL(m.Medium),
				SmallPath:    uploadURL(m.Small),
				Width:        m.Width,
				Height:       m.Height,
			}
			if class == containFitClass && m.Width > 0 && m.Height > 0 {
				ratio := float64(m.Width) / float64(m.Height)
				if ratio > 2.2 || ratio < 1.45 {
					class = strings.TrimSpace(class + " is-contain")
				}
			}
			// RenderResponsiveImg は属性値をすべてエスケープする。
			if tag := image.RenderResponsiveImg(img, alt, class); tag != "" {
				return template.HTML(tag)
			}
		}
	}
	return template.HTML(fmt.Sprintf(`<img src="%s" alt="%s" class="%s" loading="lazy" decoding="async">`,
		html.EscapeString(safeURL(src)), html.EscapeString(alt), html.EscapeString(class)))
}

// newPublicPage は全ページ共通の値（サイト名・ナビゲーションのカテゴリ）を埋めた publicPage を返す。
func (s *Server) newPublicPage(title, description, canonical string) publicPage {
	p := publicPage{
		SiteName:     s.getSiteName(),
		Title:        title,
		Description:  description,
		Canonical:    canonical,
		OGType:       "website",
		AnalyticsTag: s.getAnalyticsTag(),
		Copyright:    s.getCopyright(),
	}
	p.FaviconURL, p.FaviconType = s.layoutFavicon()
	return p
}

// handleFavicon は /favicon.ico への要求を現在の Favicon（未設定なら DefaultFaviconURL）へ 302 でリダイレクトする。
// <link rel="icon"> を読まないクローラーや古いブラウザがルート直下を要求しても 404 にしない。
// Favicon を変えたらすぐ反映されるよう、リダイレクト自体はキャッシュさせない。
// 保存値が同じサイト内のパスでなければ（DB の改ざんなど）、外部サイトへ転送せず既定の Favicon へ転送する。
func (s *Server) handleFavicon(c echo.Context) error {
	u, _ := s.favicon()
	if !isLocalPath(u) {
		u = DefaultFaviconURL
	}
	c.Response().Header().Set("Cache-Control", "no-cache")
	return c.Redirect(http.StatusFound, u)
}

// isLocalPath は u が同じオリジン内を指す "/" 始まりの相対パスか返す。"//host" や "/\host" のように
// ブラウザが別ホストと解釈するもの、スキームやホストを含むもの、空白・制御文字・バックスラッシュを含むものは拒否する。
func isLocalPath(u string) bool {
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") {
		return false
	}
	for _, r := range u {
		if r <= ' ' || r == 0x7f || r == '\\' {
			return false
		}
	}
	p, err := url.Parse(u)
	return err == nil && p.Scheme == "" && p.Host == "" && p.User == nil
}

// publicCSPHead は公開ページの CSP のうち、アクセス解析タグに左右されない前半。<base> の差し替え・ほかのサイトへの
// フォームの送信・プラグイン（<object>・<embed>）を禁じ、画像は自分と data: と https の外部画像だけを許す
// （平文の http の画像は許さない）。YouTube の埋め込み（https://www.youtube-nocookie.com）は許す。
const publicCSPHead = "default-src 'self'; base-uri 'self'; form-action 'self'; object-src 'none'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; font-src 'self'; frame-src 'self' https://www.youtube-nocookie.com"

// DefaultPublicCSP はアクセス解析タグが無い場合の基本 Content-Security-Policy。
// スクリプトや通信先は原則 self（およびインラインスクリプト/スタイル）に制限する。
const DefaultPublicCSP = publicCSPHead + "; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'"

var (
	scriptSrcRegex = regexp.MustCompile(`(?i)<script\b[^>]*\bsrc=["']([^"']+)["']`)
	cspHostPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]+)?$`)
)

// buildPublicCSP はアクセス解析タグ（AnalyticsTag）の内容に応じて、
// 必要なスクリプト配信元や通信先オリジンを動的に許可した公開用 CSP を生成する。
func buildPublicCSP(analyticsTag string) string {
	tag := strings.TrimSpace(analyticsTag)
	if tag == "" {
		return DefaultPublicCSP
	}

	scriptOrigins := make(map[string]bool)
	connectOrigins := make(map[string]bool)

	// <script src="..."> からオリジンを抽出（ホスト名に英数字・ハイフン・ドット・ポート番号以外を含む場合は注入防止のためスキップ）
	matches := scriptSrcRegex.FindAllStringSubmatch(tag, -1)
	for _, m := range matches {
		if len(m) > 1 {
			u, err := url.Parse(m[1])
			if err == nil && (u.Scheme == "https" || u.Scheme == "http") && cspHostPattern.MatchString(u.Host) {
				origin := u.Scheme + "://" + u.Host
				scriptOrigins[origin] = true
				connectOrigins[origin] = true
			}
		}
	}

	// 代表的なアクセス解析サービスのオリジン自動許可
	tagLower := strings.ToLower(tag)
	if strings.Contains(tagLower, "cloudflareinsights.com") {
		scriptOrigins["https://static.cloudflareinsights.com"] = true
		connectOrigins["https://cloudflareinsights.com"] = true
		connectOrigins["https://static.cloudflareinsights.com"] = true
	}
	if strings.Contains(tagLower, "googletagmanager.com") || strings.Contains(tagLower, "google-analytics.com") {
		scriptOrigins["https://www.googletagmanager.com"] = true
		scriptOrigins["https://www.google-analytics.com"] = true
		connectOrigins["https://www.google-analytics.com"] = true
		connectOrigins["https://*.google-analytics.com"] = true
		connectOrigins["https://*.analytics.google.com"] = true
		connectOrigins["https://www.googletagmanager.com"] = true
		connectOrigins["https://*.googletagmanager.com"] = true
	}

	var scriptParts []string
	for o := range scriptOrigins {
		scriptParts = append(scriptParts, o)
	}
	sort.Strings(scriptParts)

	var connectParts []string
	for o := range connectOrigins {
		connectParts = append(connectParts, o)
	}
	sort.Strings(connectParts)

	scriptSrc := "script-src 'self' 'unsafe-inline'"
	if len(scriptParts) > 0 {
		scriptSrc += " " + strings.Join(scriptParts, " ")
	}

	connectSrc := "connect-src 'self'"
	if len(connectParts) > 0 {
		connectSrc += " " + strings.Join(connectParts, " ")
	}

	return fmt.Sprintf("%s; %s; %s; frame-ancestors 'self'", publicCSPHead, scriptSrc, connectSrc)
}

func (s *Server) publicCSP() string {
	return buildPublicCSP(string(s.getAnalyticsTag()))
}

// renderPublic はナビゲーション（サイト構成のメニュー、無ければカテゴリの一覧）を読み込み、公開サイトのページ name を描画する。
// 表示中のページを指す項目には aria-current="page" を付ける。
func (s *Server) renderPublic(c echo.Context, code int, name string, p publicPage) error {
	if c.Response().Header().Get("Content-Security-Policy") == "" {
		c.Response().Header().Set("Content-Security-Policy", s.publicCSP())
	}
	p.GalleryResolver = s.galleryResolver()
	p.GalleryQueryResolver = s.galleryQueryResolver()
	cardImages := s.cardImageSettings()
	p.ShowFrontCardImages = cardImages.Front
	p.ShowArticleCardImages = cardImages.Article
	p.ShowRelatedCardImages = cardImages.Related
	if !cardImages.Article {
		hideArticleCardMedia(p.Data)
	}
	if !s.getShowAuthor() {
		switch d := p.Data.(type) {
		case articleData:
			d.Article.AuthorName = ""
			for i := range d.RelatedArticles {
				d.RelatedArticles[i].AuthorName = ""
			}
			p.Data = d
		case *articleData:
			d.Article.AuthorName = ""
			for i := range d.RelatedArticles {
				d.RelatedArticles[i].AuthorName = ""
			}
		case frontData:
			d.Article.AuthorName = ""
			for i := range d.RelatedArticles {
				d.RelatedArticles[i].AuthorName = ""
			}
			for i := range d.Cards {
				d.Cards[i].AuthorName = ""
			}
			for i := range d.News {
				d.News[i].AuthorName = ""
			}
			p.Data = d
		case *frontData:
			d.Article.AuthorName = ""
			for i := range d.RelatedArticles {
				d.RelatedArticles[i].AuthorName = ""
			}
			for i := range d.Cards {
				d.Cards[i].AuthorName = ""
			}
			for i := range d.News {
				d.News[i].AuthorName = ""
			}
		case indexData:
			for i := range d.Articles {
				d.Articles[i].AuthorName = ""
			}
			p.Data = d
		case *indexData:
			for i := range d.Articles {
				d.Articles[i].AuthorName = ""
			}
		case articlesData:
			for i := range d.Articles {
				d.Articles[i].AuthorName = ""
			}
			p.Data = d
		case *articlesData:
			for i := range d.Articles {
				d.Articles[i].AuthorName = ""
			}
		case categoryData:
			for i := range d.Articles {
				d.Articles[i].AuthorName = ""
			}
			p.Data = d
		case *categoryData:
			for i := range d.Articles {
				d.Articles[i].AuthorName = ""
			}
		case searchData:
			if d.Result != nil {
				for i := range d.Result.Hits {
					d.Result.Hits[i].Author = ""
				}
			}
			p.Data = d
		case *searchData:
			if d.Result != nil {
				for i := range d.Result.Hits {
					d.Result.Hits[i].Author = ""
				}
			}
		}
	}
	if p.Lang == "" {
		p.Lang = currentLang(c)
	}
	if p.Lang == "en" {
		if en := s.getSiteNameEn(); en != "" {
			p.SiteName = en
		}
	}
	current := c.Request().URL.Path
	// ページ送りの2ページ目以降（/articles/page/2 など）は、1ページ目と同じ項目を現在の項目にする。
	if rest, n, ok := splitPagedParam(strings.TrimPrefix(current, "/")); ok && n > 0 {
		current = "/" + rest
	}
	st := p.structure
	if st == nil {
		loaded := s.loadSiteStructure(c.Request().Context())
		st = &loaded
	}
	menu, footer, err := s.publicNav(c.Request().Context(), *st, current, p.Lang)
	if err != nil {
		return err
	}
	p.Menu, p.FooterMenu = menu, footer
	if p.DocNav == nil && (name == "page" || name == "article") {
		p.DocNav = buildDocNav(menu)
	}
	if isStaticExport(c) {
		p.SearchIndexURL = searchIndexPath
	}
	p.SiteStructure = *st
	labels := articlesLabels(*st, p.Lang)
	p.ArticlesLabel, p.ArticlesBackLabel, p.ArticlesAllLabel = labels.Label, labels.Back, labels.All
	if len(menu) == 0 {
		cats, err := s.publicCategories()
		if err != nil {
			return err
		}
		prefix := ""
		if p.Lang == "en" {
			prefix = "/en"
		}
		for i := range cats {
			cats[i].Current = prefix+"/categories/"+cats[i].Slug == current
		}
		p.Categories = cats
	}

	t := s.publicTheme(c)
	p.Theme = t.ID
	p.ThemeStylesheet = themeStylesheetURL(t)
	p.ColorScheme = t.ColorScheme
	p.Layout = t.Layout
	if ov, ok := themeOverrideFrom(c); ok && ov.Preview != nil {
		// プレビューは検索エンジンに登録させず、解析タグにも閲覧として送らない。
		p.Preview = ov.Preview
		p.NoIndex = true
		p.AnalyticsTag = ""
	}
	p.WindowScript = s.windowScriptFor(c)
	if isDraftPreview(c) {
		// 下書きのプレビューも検索エンジン・解析タグに渡さない。デスクトップの開いている画面としても数えない。
		p.NoIndex = true
		p.AnalyticsTag = ""
		p.WindowScript = ""
	}
	return c.Render(code, "public/"+name, p)
}

// hideArticleCardMedia はページのデータに含まれる記事カード（partials/article_card.html で描く一覧）の画像の枠を消す。
// スライスは元のデータと要素を共有するため、値で渡されたデータもそのまま書き換わる。
func hideArticleCardMedia(data any) {
	var lists [][]publicArticle
	switch d := data.(type) {
	case indexData:
		lists = append(lists, d.Articles)
	case *indexData:
		lists = append(lists, d.Articles)
	case articlesData:
		lists = append(lists, d.Articles)
	case *articlesData:
		lists = append(lists, d.Articles)
	case categoryData:
		lists = append(lists, d.Articles)
	case *categoryData:
		lists = append(lists, d.Articles)
	case frontData:
		lists = append(lists, d.News)
	case *frontData:
		lists = append(lists, d.News)
	}
	for _, list := range lists {
		for i := range list {
			list[i].HideCardMedia = true
		}
	}
}

// isThemePreview は管理画面のテーマのプレビューとして描画している内部要求か返す。
func isThemePreview(c echo.Context) bool {
	ov, ok := themeOverrideFrom(c)
	return ok && ov.Preview != nil
}

// publicTheme は公開ページに使うテーマを返す。優先順位は、文脈の上書き（エクスポートの --theme・
// 管理画面のプレビュー）、保存済みの設定（settings.theme）、theme.Default の順。カタログに無い ID は使わない。
func (s *Server) publicTheme(c echo.Context) theme.Theme {
	if ov, ok := themeOverrideFrom(c); ok {
		if t, ok := s.themeCatalog().Get(ov.ID); ok {
			return t
		}
	}
	return s.themeCatalog().Resolve(s.getSetting("theme", theme.Default))
}

// publicCategories は公開記事が1件以上あるカテゴリを返す。ページはカテゴリに出さないため数えない。
func (s *Server) publicCategories() ([]publicCategory, error) {
	cats := []publicCategory{}
	err := s.db.Select(&cats, `SELECT c.name, c.slug, c.description FROM categories c
		WHERE EXISTS (SELECT 1 FROM articles a WHERE a.category_id = c.id AND `+postKindCondition+` AND `+publishedCondition+`)
		ORDER BY c.id`, article.StatusPublished, clockNow().UTC())
	return cats, err
}

// baseURL はサイトの絶対 URL の起点（末尾の / なし）。Config.BaseURL を優先し、無ければリクエストから組み立てる。
// リクエストから組み立てるときは、Host ヘッダーを Config.AllowedHosts で確かめ（publicHost）、スキームは TLS で
// 届いたか、信頼したプロキシ（Config.TrustedProxies）が付けた X-Forwarded-Proto などだけで決める（publicScheme）。
// 誰でも書ける Host・X-Forwarded-Proto で canonical・OGP・フィードの URL を差し替えさせない。
func (s *Server) baseURL(c echo.Context) string {
	if b, ok := c.Request().Context().Value(exportBaseURLKey{}).(string); ok && b != "" {
		return b
	}
	if base := s.getBaseURL(); base != "" {
		return base
	}
	return s.publicScheme(c) + "://" + s.publicHost(c)
}

// publicScheme は公開サイトの URL のスキーム（http か https）を返す。TLS で届いた要求は https。信頼したプロキシから
// 届いた要求だけは、プロキシが伝えるスキーム（X-Forwarded-Proto など）に従う。それ以外の転送ヘッダーは無視する
// （mcp.TrustedProxies.Scheme。OAuth・招待コードのオリジンと同じ判定）。
func (s *Server) publicScheme(c echo.Context) string {
	return s.proxies.Scheme(c.Request())
}

// publicHost は公開サイトの URL のホスト（ポートを含むことがある）を返す。Config.AllowedHosts を指定したサイトで、
// 一覧に無い Host ヘッダーで届いた要求には、その Host ではなく一覧の先頭の値を返す。
func (s *Server) publicHost(c echo.Context) string {
	host := requestHost(c)
	if len(s.allowedHosts) == 0 || hostAllowed(host, s.allowedHosts) {
		return host
	}
	return s.allowedHosts[0]
}

// hostAllowed は要求の Host（ポートを含むことがある）が allowed（normalizeAllowedHosts でそろえた値）のどれかに
// 当たるかを返す。ポートを書いた値はポートまで、書かない値はホスト名だけで比べる（mcp.HostAllowed。MCP サーバーと
// OAuth のオリジンも同じ照合を使う）。
func hostAllowed(host string, allowed []string) bool {
	return mcp.HostAllowed(host, allowed)
}

// requestOrigin はいまの要求が届いたオリジン（スキーム・ホスト・ポート。末尾の / なし）を返す。公開サイトの URL
// （Config.BaseURL）ではなく、この sorus が実際に待ち受けている場所を指す。スキームは publicScheme（TLS か、信頼した
// プロキシの転送ヘッダーだけ。クライアントが書いた X-Forwarded-Proto は信じない）。
func (s *Server) requestOrigin(c echo.Context) string {
	return s.publicScheme(c) + "://" + requestHost(c)
}

// requestHost はいまの要求の Host ヘッダー（ポートを含む）から、制御文字や不審な文字を除いて返す。
func requestHost(c echo.Context) string {
	host := strings.Map(func(r rune) rune {
		if r > 32 && r < 127 && r != '<' && r != '>' && r != '"' && r != '`' && r != '\\' {
			return r
		}
		return -1
	}, c.Request().Host)
	if host == "" {
		host = "localhost"
	}
	return host
}

type crumb struct{ Name, URL string }

// breadcrumbList は schema.org/BreadcrumbList の構造化データを返す。
func breadcrumbList(crumbs ...crumb) map[string]any {
	items := make([]any, len(crumbs))
	for i, cr := range crumbs {
		items[i] = map[string]any{"@type": "ListItem", "position": i + 1, "name": cr.Name, "item": cr.URL}
	}
	return map[string]any{"@context": "https://schema.org", "@type": "BreadcrumbList", "itemListElement": items}
}

// articleDescription は抜粋、無ければ本文の先頭から meta description を作る。
func articleDescription(a publicArticle, bodyHTML string) string {
	if a.Excerpt != "" {
		return a.Excerpt
	}
	text := strings.Join(strings.Fields(html.UnescapeString(textPolicy.Sanitize(bodyHTML))), " ")
	if utf8.RuneCountInString(text) > descriptionRunes {
		text = string([]rune(text)[:descriptionRunes]) + "…"
	}
	return text
}

// maxPageNumber は ?page= で受け付ける最大ページ番号。整数のオーバーフローを防ぐ。
const maxPageNumber = 10000

// pageParam は ?page= を返す。未指定は 1、不正な値または上限超過なら ok=false。
// 一覧のページ送りのリンクはパスの形（pagedURL）で出すが、以前のリンクが切れないよう ?page=N も同じ内容を返す。
func pageParam(c echo.Context) (int, bool) {
	v := c.QueryParam("page")
	if v == "" {
		return 1, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxPageNumber {
		return 0, false
	}
	return n, true
}

// splitPagedParam は末尾のパスパラメータ（例: "news/page/2"、トップの "page/2"）を、前の部分とページ番号に分ける。
// ページ送りの形でなければ (param, 0, true) を返す。番号が数字でない・2 未満・上限超過なら ok=false
// （1ページ目の URL は /page/1 ではなく元の URL だけにする）。末尾の / は静的ホスティングと同じく無視する。
func splitPagedParam(param string) (rest string, pageNum int, ok bool) {
	p := strings.TrimSuffix(param, "/")
	var num string
	if i := strings.LastIndex(p, "/page/"); i >= 0 {
		rest, num = p[:i], p[i+len("/page/"):]
	} else if strings.HasPrefix(p, "page/") {
		num = p[len("page/"):]
	} else {
		return param, 0, true
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 2 || n > maxPageNumber || strconv.Itoa(n) != num {
		return "", 0, false
	}
	return rest, n, true
}

// pathParam はパスパラメータをデコードして返す。日本語スラッグはパーセントエンコードされて届く。
func pathParam(c echo.Context, name string) string {
	v := c.Param(name)
	if dec, err := url.PathUnescape(v); err == nil {
		return dec
	}
	return v
}

// articlesListPath は記事の一覧のパス。
const articlesListPath = "/articles"

func articlePath(slug string) string  { return "/articles/" + url.PathEscape(slug) }
func pagePath(slug string) string     { return "/" + url.PathEscape(slug) }
func categoryPath(slug string) string { return "/categories/" + url.PathEscape(slug) }

// articlePublicPath は種類 kind とスラッグ slug、言語 lang に応じた公開 URL のパスを返す。
func articlePublicPath(kind article.Kind, slug, lang string) string {
	prefix := ""
	if strings.ToLower(strings.TrimSpace(lang)) == "en" {
		prefix = "/en"
	}
	if kind == article.KindPage {
		return prefix + "/" + url.PathEscape(slug)
	}
	return prefix + "/articles/" + url.PathEscape(slug)
}

// contentPath は種類 kind の記事の公開 URL のパス。
// 第3引数に lang を指定すると言語に応じたプレフィックスが付与される。
func contentPath(kind article.Kind, slug string, lang ...string) string {
	l := ""
	if len(lang) > 0 {
		l = lang[0]
	}
	return articlePublicPath(kind, slug, l)
}

// pagedURL は 2ページ目以降に /page/N を付けた URL を返す（例: /articles/page/2、トップは /page/2）。
// 静的に書き出したサイトでも同じ URL がファイル（articles/page/2/index.html）として開けるよう、クエリではなくパスで表す。
func pagedURL(u string, pageNum int) string {
	if pageNum <= 1 {
		return u
	}
	return strings.TrimSuffix(u, "/") + "/page/" + strconv.Itoa(pageNum)
}

func paginationURL(u string, pageNum int) string {
	if pageNum <= 1 {
		return u
	}
	if strings.Contains(u, "?") {
		return u + "&page=" + strconv.Itoa(pageNum)
	}
	return pagedURL(u, pageNum)
}

// absoluteURL は / 始まりのパスを base からの絶対 URL にする。それ以外はそのまま返す。
func absoluteURL(base, u string) string {
	if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") {
		return base + u
	}
	return u
}

// safeURL は http(s) の絶対 URL とサイト内パスだけを通し、javascript: などは空にする。
func safeURL(u string) string {
	if strings.HasPrefix(u, "/") {
		return u
	}
	if p, err := url.Parse(u); err == nil && (p.Scheme == "http" || p.Scheme == "https") {
		return u
	}
	return ""
}

// baseName は OS の区切り文字に依らず保存パスのファイル名を返す。
func baseName(p string) string { return path.Base(strings.ReplaceAll(p, `\`, "/")) }

// escapeLike は LIKE のワイルドカードを ESCAPE '\' 用にエスケープする。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// draftPreviewContextKey は、下書きプレビュー（管理画面の handleArticleThemePreview と、デスクトップモードの既定の
// ブラウザで開く handleTokenThemePreview）として公開テーマで描画している要求であることを echo.Context に入れるキー。
const draftPreviewContextKey = "server.draftPreview"

// isDraftPreview は管理画面の下書きプレビューとして描画している要求か返す。
func isDraftPreview(c echo.Context) bool {
	v, _ := c.Get(draftPreviewContextKey).(bool)
	return v
}

// unpublishedArticleColumns は公開日時の無い（NULL の）記事を publicArticle に読む列。公開日時を更新日時で代える。
// COALESCE では列の型（DATETIME）が失われて時刻として読めないため、列をそのまま差し替える。
var unpublishedArticleColumns = strings.Replace(publicArticleColumns, "a.published_at,", "a.updated_at AS published_at,", 1)

// handleArticleThemePreview は記事を、状態（下書き・校閲待ち・予約中・公開中）を問わず、いま適用中の公開テーマで
// 公開ページと同じに描画して返す（GET /admin/articles/:id/preview。ライター・編集者・管理者）。ゴミ箱の記事は 404。
// 読者の閲覧ではないため閲覧数を数えず、検索エンジン・キャッシュにも残させない（renderPublic の isDraftPreview）。
func (s *Server) handleArticleThemePreview(c echo.Context) error {
	return s.renderDraftPreview(c, c.Param("id"))
}

// handleTokenThemePreview は、デスクトップモードで管理画面が発行した使い捨てのトークン（handleDesktopPreviewOpen）で、
// 記事を handleArticleThemePreview と同じに公開テーマで描画して返す（GET PreviewTokenPath/:token）。既定のブラウザには
// ログインの Cookie が無いため、トークンをログインの代わりにする。トークンは最初の要求で消すため、2回目・期限切れ・
// 知らないトークン・ゴミ箱の記事は、存在しないページと同じ 404 にする。閲覧数は数えず、検索エンジン・キャッシュにも
// 残させず、開いている画面の接続のスクリプトも入れない（renderPublic の isDraftPreview）。
func (s *Server) handleTokenThemePreview(c echo.Context) error {
	if !s.cfg.Desktop {
		return echo.ErrNotFound
	}
	id, ok := s.previewTokens.consume(c.Param("token"), clockNow())
	if !ok {
		return echo.ErrNotFound
	}
	return s.renderDraftPreview(c, id)
}

// renderDraftPreview は記事 id を、状態を問わず適用中の公開テーマで公開ページと同じに描画する。ゴミ箱・存在しない記事は 404。
func (s *Server) renderDraftPreview(c echo.Context, id string) error {
	src, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if src.Status == article.StatusDeleted {
		return echo.ErrNotFound
	}
	columns := publicArticleColumns
	if src.PublishedAt == nil {
		columns = unpublishedArticleColumns
	}
	var a publicArticle
	if err := s.db.Get(&a, `SELECT `+columns+publicArticleFrom+`WHERE a.id = ?`, src.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.ErrNotFound
		}
		return err
	}
	c.Set(draftPreviewContextKey, true)
	h := c.Response().Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	lang := a.Lang
	if lang != "en" {
		lang = "ja"
	}
	if a.Kind == article.KindPage {
		return s.renderPageDetail(c, a, lang, s.loadSiteStructure(c.Request().Context()))
	}
	return s.renderArticleDetail(c, a, lang)
}
