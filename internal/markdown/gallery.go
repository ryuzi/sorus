package markdown

import (
	"bufio"
	"bytes"
	stdhtml "html"
	"math/rand/v2"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// フォトギャラリー記法。:::gallery と ::: で囲んだ範囲の画像 ![alt](url "caption") を
// <figure class="photo-gallery gallery-layout-<並べ方>"> のサムネイル一覧として出力する。
// 出力は公開テンプレートの {{template "gallery"}}（templates/public/partials/gallery.html）と同じ形にそろえる。
// 並べ方は layout="justified|masonry|cards"（既定 justified）。行揃えは static/js/justified_gallery.js が計算する。
// 拡大表示は公開サイト共通の <dialog id="cms-lightbox">（static/js/lightbox.js）が行う。
// ブロック内の画像以外（文章など）は出力しない。閉じの ::: が無い :::gallery は通常の段落として扱う。

const (
	// GalleryClass はギャラリー全体を囲む figure の共通のクラス。並べ方のクラス（GalleryFigureClass）と組み合わせて出す。
	GalleryClass = "photo-gallery"
	// GalleryItemClass は画像1枚を囲む div のクラス。
	GalleryItemClass = "gallery-item"
	// GalleryMasonryGridClass は段組み（masonry）に付ける、今までの段組みのクラス（既存の CSS・テーマの互換用）。
	GalleryMasonryGridClass = "masonry-grid"
	// GalleryLayoutClassPrefix は並べ方のクラス（gallery-layout-<並べ方>）の接頭辞。
	GalleryLayoutClassPrefix = "gallery-layout-"

	// GalleryLayoutJustified は、元の縦横比のまま行の高さをそろえる行揃え（既定）。
	GalleryLayoutJustified = "justified"
	// GalleryLayoutMasonry は、元の縦横比のまま段に詰める段組み。
	GalleryLayoutMasonry = "masonry"
	// GalleryLayoutCards は、額縁のような余白で囲み下にキャプションを添えるカード。
	GalleryLayoutCards = "cards"
	// DefaultGalleryLayout は layout の指定が無い・知らない値のときの並べ方。
	DefaultGalleryLayout = GalleryLayoutJustified

	// legacyGalleryClass は並べ方を導入する前の figure のクラス。生 HTML に書かれた古い形を残すためだけに許可する。
	legacyGalleryClass = GalleryClass + " " + GalleryMasonryGridClass

	// GalleryMoreClass は「もっと見る」の行（総枚数とボタン）の p のクラス。ギャラリーの figure の直後に置く。
	GalleryMoreClass = "gallery-more"
	// GalleryCountClass は総枚数の表示（「全 320 枚」）の span のクラス。
	GalleryCountClass = "gallery-count"
	// GalleryMoreButtonClass は「もっと見る（残り N 件）」のボタンのクラス。justified_gallery.js が押されたら続きを読む。
	GalleryMoreButtonClass = "gallery-more-button"

	// DefaultGalleryPageSize は limit を書かないタグのギャラリーで最初に出す枚数と、「もっと見る」で1回に足す枚数。
	// タグを付けるだけで写真が増え続ける運用（/album など）でも、ページが重くならないようにする。
	DefaultGalleryPageSize = 24
	// MaxGalleryLimit は limit に書ける枚数・続きの API で1回に返す枚数の上限。
	MaxGalleryLimit = 100
	// MaxGalleryMinWidth は min_width に書ける幅（px）の上限。
	MaxGalleryMinWidth = 20000
	// GalleryAPIPath は「もっと見る」で続きの写真を返す公開の API のパス（internal/server が登録する）。
	GalleryAPIPath = "/api/gallery"
	// GalleryYearFilterClass は年別の絞り込み（year_filter="true"）のピルを並べる div のクラス。ギャラリーの figure の直前に置く。
	GalleryYearFilterClass = "gallery-year-filter"
	// GalleryYearPillClass は年別の絞り込みのピル（「すべて」「2025年」…）のボタンのクラス。選択中には is-active を足す。
	GalleryYearPillClass = "gallery-year-pill"
	// maxGalleryYearPills は年別の絞り込みに並べる年の数の上限（新しい順）。
	maxGalleryYearPills = 60
	// gallerySeedModulus は並び順 random の続きを読むときの、乱数の種で決まる並べ替えの法（素数 2^31-1）。
	gallerySeedModulus = 2147483647
)

// GalleryLayouts は :::gallery の layout に書ける並べ方（既定が先頭）。
var GalleryLayouts = []string{GalleryLayoutJustified, GalleryLayoutMasonry, GalleryLayoutCards}

// NormalizeGalleryLayout は並べ方を justified・masonry・cards のどれかにする。空・知らない値は既定（justified）。
func NormalizeGalleryLayout(layout string) string {
	for _, l := range GalleryLayouts {
		if layout == l {
			return l
		}
	}
	return DefaultGalleryLayout
}

// GalleryFigureClass は並べ方 layout のギャラリーの figure の class 属性の値を返す。
// 段組みには今までの masonry-grid も付ける（テンプレートの {{template "gallery"}} と同じ）。
func GalleryFigureClass(layout string) string {
	layout = NormalizeGalleryLayout(layout)
	class := GalleryClass + " " + GalleryLayoutClassPrefix + layout
	if layout == GalleryLayoutMasonry {
		class += " " + GalleryMasonryGridClass
	}
	return class
}

var (
	galleryCloseMarker = []byte(":::")
	kindGallery        = ast.NewNodeKind("Gallery")
	galleryTagAttr     = regexp.MustCompile(`(?:^|\s)tag\s*=\s*"([^"]*)"`)
	galleryLimitAttr   = regexp.MustCompile(`(?:^|\s)limit\s*=\s*"([^"]*)"`)
	gallerySortAttr    = regexp.MustCompile(`(?:^|\s)sort\s*=\s*"([^"]*)"`)
	galleryLayoutAttr  = regexp.MustCompile(`(?:^|\s)layout\s*=\s*"([^"]*)"`)
	galleryMinWAttr    = regexp.MustCompile(`(?:^|\s)min_width\s*=\s*"([^"]*)"`)
	galleryYearAttr    = regexp.MustCompile(`(?:^|\s)(?:year_filter|year_pills)\s*=\s*"([^"]*)"`)
	galleryYearTag     = regexp.MustCompile(`^[0-9]{4}年$`)
	galleryAttrs       = regexp.MustCompile(`^:::gallery(?:\s+[A-Za-z][A-Za-z0-9_-]*\s*=\s*"[^"]*")*\s*$`)
)

// GalleryImage is an image returned by a tag based gallery resolver.
type GalleryImage struct {
	URL     string
	Alt     string
	Caption string
	// ThumbURL は一覧に出す軽量なサムネイル（-800.webp → -400.webp）の URL。無ければ空（URL を使う）。
	ThumbURL string
	// FullURL は拡大表示（ライトボックスの data-full）に使う高解像度（-1200.webp → 原寸）の URL。無ければ空（URL を使う）。
	FullURL string
	// Width・Height は元画像のピクセル寸法（縦横比）。<img> の width・height 属性（CLS 防止）と行揃えの計算に使う。
	// 不明なら 0。
	Width  int
	Height int
	// Years は写真の AI タグのうち「YYYY年」の形のものを空白で区切った文字列（GalleryYearList）。年別の絞り込み
	// （year_filter="true"）のギャラリーで、写真ごとの data-gallery-years に出し、静的な書き出しでもブラウザ側で
	// 絞り込めるようにする（構造体を == で比べられるよう、スライスにはしない）。
	Years string
}

// GalleryResolver returns the current images for a gallery tag.
type GalleryResolver func(tag string) ([]GalleryImage, error)

// GalleryQuery describes a tag gallery lookup. A zero limit means no explicit limit.
type GalleryQuery struct {
	Tag   string
	Limit int
	Sort  string
	// MinWidth が正なら、元画像の幅がこれ以上（px）の写真だけにする（WordPress の縮小サムネイルなどの小さな画像を外す）。
	// 幅の分からない写真（0）も外す。
	MinWidth int
	// Offset は先頭から飛ばす枚数（「もっと見る」の続き）。
	Offset int
	// Seed が正なら、並び順 random を種で決まる並べ替えにする（続きを読んでも同じ並びで重ならない）。0 なら毎回ばらばら。
	Seed int64
	// Year が空でなければ、AI タグにその年（「2025年」の形。ValidGalleryYear）も付いた写真だけにする（年別の絞り込み）。
	Year string
}

// GalleryQueryResolver resolves a tag gallery with display options.
type GalleryQueryResolver func(query GalleryQuery) ([]GalleryImage, error)

// GalleryPage はタグのギャラリーの写真の一部（Offset から Limit 枚）と、条件に合う写真の総数 Total。
type GalleryPage struct {
	Images []GalleryImage
	Total  int
}

// GalleryPageResolver は総数付きで写真の一部を返す resolver。
type GalleryPageResolver func(query GalleryQuery) (GalleryPage, error)

// GalleryOptions はタグのギャラリーの描画のしかた。
type GalleryOptions struct {
	Resolver GalleryPageResolver
	// MoreURL は「もっと見る」で続きを読む API のパス（GalleryAPIPath）。空なら（静的な書き出し・プレビュー）limit を書かない
	// ギャラリーの写真をすべて出力し、DefaultGalleryPageSize 枚より後ろを hidden にしてボタンで順に見せる。
	MoreURL string
	// Years は年別の絞り込み（year_filter="true"）のピルに並べる年を返す。query の条件（タグ・min_width）に合う写真の
	// AI タグの「YYYY年」を新しい順に返す。続きの API で読むギャラリー（本文に一部の写真しか無い）にだけ使い、nil や
	// 本文にすべての写真があるときは、出力する写真の Years から集める。
	Years func(query GalleryQuery) ([]string, error)
	// buttonToken は1回の描画ごとの推測できない文字列。「もっと見る」のボタンの置き場所の印に使い、サニタイズの後で
	// ボタンに置き換える（本文に同じ文字を書かれてもボタンにならない）。
	buttonToken string
}

// pageResolverFrom は総数を返さない resolver を GalleryPageResolver にする（返した写真の数を総数とみなす）。
func pageResolverFrom(resolver GalleryQueryResolver) GalleryPageResolver {
	if resolver == nil {
		return nil
	}
	return func(query GalleryQuery) (GalleryPage, error) {
		images, err := resolver(query)
		return GalleryPage{Images: images, Total: len(images)}, err
	}
}

// NewGallerySeed は並び順 random の続きの読み込みに使う種（1 以上 gallerySeedModulus 未満）を返す。
func NewGallerySeed() int64 {
	return 1 + rand.Int64N(gallerySeedModulus-1)
}

// GallerySeedOrder は種 seed で決まる並べ替えの SQL の ORDER BY 式（rowid 列 col に使う）。seed は 1 以上 gallerySeedModulus
// 未満に限る（素数の法で掛け算するため、rowid が法より小さい間は並びが重ならない）。値は呼び出し側が検査する。
func GallerySeedOrder(col string, seed int64) string {
	return "((" + col + " * " + strconv.FormatInt(seed, 10) + ") % " + strconv.Itoa(gallerySeedModulus) + ")"
}

// ValidGallerySeed は seed が GallerySeedOrder に使える種かを返す。
func ValidGallerySeed(seed int64) bool {
	return seed >= 1 && seed < gallerySeedModulus
}

// GalleryMoreQuery は続きの API（GalleryAPIPath）の問い合わせの文字列（offset を除く）を返す。
func GalleryMoreQuery(q GalleryQuery) string {
	v := url.Values{"tag": {q.Tag}}
	if q.Sort != "" && q.Sort != "latest" {
		v.Set("sort", q.Sort)
	}
	if q.MinWidth > 0 {
		v.Set("min_width", strconv.Itoa(q.MinWidth))
	}
	if q.Seed > 0 {
		v.Set("seed", strconv.FormatInt(q.Seed, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Year != "" {
		v.Set("year", q.Year)
	}
	return v.Encode()
}

// ValidGalleryYear は year が年別の絞り込みの年（「2025年」のような4桁の数字と「年」）かを返す。
func ValidGalleryYear(year string) bool {
	return galleryYearTag.MatchString(year)
}

// GalleryYearTags はメディアの AI タグ（カンマ区切り）のうち「YYYY年」の形のものを、重ねずに新しい順で返す。
func GalleryYearTags(aiTags string) []string {
	var years []string
	for _, tag := range strings.Split(aiTags, ",") {
		years = appendGalleryYear(years, strings.TrimSpace(tag))
	}
	return SortGalleryYears(years)
}

// GalleryYearList は AI タグ（カンマ区切り）の「YYYY年」を新しい順に空白で区切って返す（GalleryImage.Years の値）。
func GalleryYearList(aiTags string) string {
	return strings.Join(GalleryYearTags(aiTags), " ")
}

// appendGalleryYear は year が年の形で、まだ years に無ければ足す。
func appendGalleryYear(years []string, year string) []string {
	if !ValidGalleryYear(year) || slices.Contains(years, year) {
		return years
	}
	return append(years, year)
}

// SortGalleryYears は年（「2025年」の形）を新しい順に並べ替えて返す（同じ桁数なので文字列の逆順でよい）。
func SortGalleryYears(years []string) []string {
	slices.SortFunc(years, func(a, b string) int { return strings.Compare(b, a) })
	return years
}

// galleryNode はギャラリー1つ。子には通常の Markdown として解釈したブロックを持つ。
type galleryNode struct {
	ast.BaseBlock
	Tag   string
	Limit int
	// All は limit="all"（すべての写真を一度に出す。「もっと見る」にしない）。
	All      bool
	Sort     string
	MinWidth int
	// Layout は並べ方（justified・masonry・cards のどれか。NormalizeGalleryLayout 済み）。
	Layout string
	// YearFilter は year_filter="true"（年別の絞り込みのピルを出す。タグのギャラリーだけ）。
	YearFilter bool
}

func (n *galleryNode) Kind() ast.NodeKind { return kindGallery }

func (n *galleryNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// galleryExtension はギャラリーのパーサーとレンダラーを Goldmark に登録する。
type galleryExtension struct{ opts GalleryOptions }

func (e galleryExtension) Extend(m goldmark.Markdown) {
	// フェンスコードブロック（優先度 700）より前に試す。
	m.Parser().AddOptions(parser.WithBlockParsers(util.Prioritized(galleryParser{}, 650)))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(galleryRenderer{opts: e.opts}, 500)))
}

type galleryParser struct{}

func (galleryParser) Trigger() []byte { return []byte{':'} }

func (galleryParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || !isGalleryOpen(util.TrimRightSpace(line[pos:])) {
		return nil, parser.NoChildren
	}
	if !hasGalleryClose(reader) {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	open := util.TrimRightSpace(line[pos:])
	limit, all := galleryLimit(open)
	return &galleryNode{
		Tag:        galleryTag(open),
		Limit:      limit,
		All:        all,
		Sort:       gallerySort(open),
		MinWidth:   galleryMinWidth(open),
		Layout:     galleryLayout(open),
		YearFilter: galleryYearFilter(open),
	}, parser.HasChildren
}

func isGalleryOpen(line []byte) bool {
	return galleryAttrs.Match(line)
}

func galleryTag(line []byte) string {
	match := galleryTagAttr.FindSubmatch(line[len(":::gallery"):])
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(string(match[1]))
}

// galleryLimit は開きの行の limit="..." を返す。"all" なら all=true（すべて出す）。無い・数でない・1 未満は 0
// （limit の指定なし。最初に DefaultGalleryPageSize 枚を出し、残りは「もっと見る」）。MaxGalleryLimit を超える数は上限にする。
func galleryLimit(line []byte) (limit int, all bool) {
	match := galleryLimitAttr.FindSubmatch(line[len(":::gallery"):])
	if len(match) != 2 {
		return 0, false
	}
	value := strings.TrimSpace(string(match[1]))
	if strings.EqualFold(value, "all") {
		return 0, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		return 0, false
	}
	return min(limit, MaxGalleryLimit), false
}

// galleryMinWidth は開きの行の min_width="..."（px）を返す。無い・数でない・1 未満は 0（幅で絞らない）。
func galleryMinWidth(line []byte) int {
	match := galleryMinWAttr.FindSubmatch(line[len(":::gallery"):])
	if len(match) != 2 {
		return 0
	}
	w, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(match[1])), "px")))
	if err != nil || w < 1 {
		return 0
	}
	return min(w, MaxGalleryMinWidth)
}

func gallerySort(line []byte) string {
	match := gallerySortAttr.FindSubmatch(line[len(":::gallery"):])
	if len(match) != 2 {
		return "latest"
	}
	sort := string(match[1])
	switch sort {
	case "latest", "oldest", "random":
		return sort
	default:
		return "latest"
	}
}

// galleryLayout は開きの行の layout="..." を並べ方にする。無い・知らない値は既定（justified）。
func galleryLayout(line []byte) string {
	match := galleryLayoutAttr.FindSubmatch(line[len(":::gallery"):])
	if len(match) != 2 {
		return DefaultGalleryLayout
	}
	return NormalizeGalleryLayout(strings.TrimSpace(string(match[1])))
}

// galleryYearFilter は開きの行の year_filter="..."（year_pills="..." も同じ）が true・1・yes・on かを返す。
func galleryYearFilter(line []byte) bool {
	match := galleryYearAttr.FindSubmatch(line[len(":::gallery"):])
	if len(match) != 2 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(string(match[1]))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// hasGalleryClose は開きの行より後に閉じの ::: があるかを、読み取り位置を変えずに調べる。
func hasGalleryClose(reader text.Reader) bool {
	l, pos := reader.Position()
	defer reader.SetPosition(l, pos)
	for {
		reader.AdvanceLine()
		line, _ := reader.PeekLine()
		if line == nil {
			return false
		}
		if isGalleryClose(line, reader.LineOffset()) {
			return true
		}
	}
}

func isGalleryClose(line []byte, offset int) bool {
	w, pos := util.IndentWidth(line, offset)
	return w < 4 && bytes.Equal(util.TrimRightSpace(line[pos:]), galleryCloseMarker)
}

func (galleryParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	line, _ := reader.PeekLine()
	if isGalleryClose(line, reader.LineOffset()) {
		reader.AdvanceToEOL()
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

func (galleryParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (galleryParser) CanInterruptParagraph() bool { return true }

func (galleryParser) CanAcceptIndentedLine() bool { return false }

type galleryRenderer struct{ opts GalleryOptions }

func (r galleryRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindGallery, func(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		return renderGallery(w, source, n, entering, r.opts)
	})
}

func renderGallery(w util.BufWriter, source []byte, n ast.Node, entering bool, opts GalleryOptions) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var images []*ast.Image
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if img, ok := c.(*ast.Image); ok && entering {
			images = append(images, img)
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})

	gn := n.(*galleryNode)
	// 属性の順はテンプレートの {{template "gallery"}} と同じ（class・data-gallery-layout・data-gallery-tag）。
	open := `<figure class="` + GalleryFigureClass(gn.Layout) + `" data-gallery-layout="` + NormalizeGalleryLayout(gn.Layout) + `"`
	if gn.Tag != "" && opts.Resolver != nil {
		return renderTagGallery(w, gn, open, opts)
	}
	opened := false
	for _, img := range images {
		dest := util.URLEscape(img.Destination, true)
		if len(dest) == 0 || html.IsDangerousURL(dest) {
			continue
		}
		if !opened {
			_, _ = w.WriteString(open + `>` + "\n")
			opened = true
		}
		renderGalleryItem(w, dest, imageAltText(img, source), img.Title)
	}
	if opened {
		_, _ = w.WriteString("</figure>\n")
	}
	return ast.WalkSkipChildren, nil
}

// renderTagGallery はタグのギャラリーを出力する。limit を書いたらその枚数、limit="all" ならすべてを出す（今までどおり）。
// limit を書かないときは最初の DefaultGalleryPageSize 枚だけを出し、総数がそれより多ければ figure に総数
// （data-gallery-total）と続きの API（data-gallery-more。MoreURL が空なら付けず、残りを hidden で出力しておく）を付け、
// 直後に総枚数と「もっと見る（残り N 件）」のボタンの行を出す。写真が無ければ何も出さない。
func renderTagGallery(w util.BufWriter, gn *galleryNode, open string, opts GalleryOptions) (ast.WalkStatus, error) {
	query := GalleryQuery{Tag: gn.Tag, Limit: gn.Limit, Sort: gn.Sort, MinWidth: gn.MinWidth}
	paged := gn.Limit == 0 && !gn.All
	if paged && opts.MoreURL != "" {
		query.Limit = DefaultGalleryPageSize
		if query.Sort == "random" {
			query.Seed = NewGallerySeed()
		}
	}
	page, err := opts.Resolver(query)
	if err != nil {
		return ast.WalkSkipChildren, err
	}
	items := page.Images
	if len(items) == 0 {
		return ast.WalkSkipChildren, nil
	}
	total := max(page.Total, len(items))
	shown := len(items)
	if paged && opts.MoreURL == "" {
		shown = min(shown, DefaultGalleryPageSize)
	}
	more := paged && total > shown
	if gn.YearFilter {
		years, err := tagGalleryYears(query, items, more && opts.MoreURL != "", opts)
		if err != nil {
			return ast.WalkSkipChildren, err
		}
		if len(years) > 0 && opts.buttonToken != "" {
			_, _ = w.WriteString(galleryYearsMarker(opts.buttonToken, years) + "\n")
		}
	}
	_, _ = w.WriteString(open + ` data-gallery-tag="` + stdhtml.EscapeString(gn.Tag) + `"`)
	if more {
		_, _ = w.WriteString(` data-gallery-total="` + strconv.Itoa(total) + `"`)
		if opts.MoreURL != "" {
			_, _ = w.WriteString(` data-gallery-more="` + stdhtml.EscapeString(opts.MoreURL+"?"+GalleryMoreQuery(query)) + `"`)
		}
	}
	_, _ = w.WriteString(">\n")
	for i, item := range items {
		renderGalleryItemStrings(w, item, i >= shown, gn.YearFilter)
	}
	_, _ = w.WriteString("</figure>\n")
	if more {
		_, _ = w.WriteString(`<p class="` + GalleryMoreClass + `"><span class="` + GalleryCountClass + `">全 ` + strconv.Itoa(total) + ` 枚</span>`)
		if opts.buttonToken != "" {
			_, _ = w.WriteString(galleryButtonMarker(opts.buttonToken, total-shown))
		}
		_, _ = w.WriteString("</p>\n")
	}
	return ast.WalkSkipChildren, nil
}

// tagGalleryYears は年別の絞り込みのピルに並べる年を新しい順に返す。続きの API で読む（本文に一部の写真しか無い）
// ギャラリーは opts.Years でタグの写真すべてから集め、それ以外（静的な書き出し・limit を書いたもの・すべて出したもの）は
// 出力する写真の Years から集める（ブラウザ側で絞り込むので、本文に無い年のピルは出さない）。
func tagGalleryYears(query GalleryQuery, items []GalleryImage, remote bool, opts GalleryOptions) ([]string, error) {
	var years []string
	if remote && opts.Years != nil {
		all, err := opts.Years(GalleryQuery{Tag: query.Tag, MinWidth: query.MinWidth})
		if err != nil {
			return nil, err
		}
		for _, y := range all {
			years = appendGalleryYear(years, y)
		}
	} else {
		for _, item := range items {
			for _, y := range strings.Fields(item.Years) {
				years = appendGalleryYear(years, y)
			}
		}
	}
	years = SortGalleryYears(years)
	return years[:min(len(years), maxGalleryYearPills)], nil
}

// galleryYearsMarker は年別の絞り込みのピルを置く場所の印。年は「年」を除いた4桁の数字をカンマでつなぐ。
// サニタイズの後に expandGalleryMoreButtons がピルの並びに置き換える（button はサニタイズで許可しないため）。
func galleryYearsMarker(token string, years []string) string {
	digits := make([]string, len(years))
	for i, y := range years {
		digits[i] = strings.TrimSuffix(y, "年")
	}
	return "[[sorus-gallery-years:" + token + ":" + strings.Join(digits, ",") + "]]"
}

var galleryYearsMarkerPattern = regexp.MustCompile(`\[\[sorus-gallery-years:([A-Za-z0-9]+):([0-9]{4}(?:,[0-9]{4}){0,99})\]\]`)

// galleryYearFilterHTML は年別の絞り込みのピル（「すべて」と年の並び）の HTML を返す。digits は4桁の数字をカンマで
// つないだもの。押されたときの絞り込みと英語のページの文言は justified_gallery.js が受け持つ。
func galleryYearFilterHTML(digits string) string {
	var b strings.Builder
	b.WriteString(`<div class="` + GalleryYearFilterClass + `" role="group" aria-label="年で絞り込む">`)
	b.WriteString(`<button type="button" class="` + GalleryYearPillClass + ` is-active" data-gallery-year="" aria-pressed="true">すべて</button>`)
	for _, d := range strings.Split(digits, ",") {
		y := d + "年"
		b.WriteString(`<button type="button" class="` + GalleryYearPillClass + `" data-gallery-year="` + y + `" aria-pressed="false">` + y + `</button>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// galleryButtonMarker は「もっと見る」のボタン（残り remaining 件）を置く場所の印。サニタイズの後に
// expandGalleryMoreButtons がボタンに置き換える。
func galleryButtonMarker(token string, remaining int) string {
	return "[[sorus-gallery-more:" + token + ":" + strconv.Itoa(remaining) + "]]"
}

var galleryButtonMarkerPattern = regexp.MustCompile(`\[\[sorus-gallery-more:([A-Za-z0-9]+):([0-9]{1,9})\]\]`)

// expandGalleryMoreButtons はサニタイズ済みの HTML の「もっと見る」の印（この描画の token のものだけ）をボタンにする。
// 文言は justified_gallery.js が読み込みのたびに（英語のページでは英語に）書き換える。
// 年別の絞り込みの印（galleryYearsMarker）も同じようにピルの並びにする。
func expandGalleryMoreButtons(safe, token string) string {
	if token == "" {
		return safe
	}
	if strings.Contains(safe, "[[sorus-gallery-more:") {
		safe = galleryButtonMarkerPattern.ReplaceAllStringFunc(safe, func(m string) string {
			sub := galleryButtonMarkerPattern.FindStringSubmatch(m)
			if sub[1] != token {
				return m
			}
			return galleryMoreButtonHTML(sub[2])
		})
	}
	if strings.Contains(safe, "[[sorus-gallery-years:") {
		safe = galleryYearsMarkerPattern.ReplaceAllStringFunc(safe, func(m string) string {
			sub := galleryYearsMarkerPattern.FindStringSubmatch(m)
			if sub[1] != token {
				return m
			}
			return galleryYearFilterHTML(sub[2])
		})
	}
	return safe
}

// galleryMoreButtonHTML は「もっと見る（残り remaining 件）」のボタンの HTML を返す。remaining は数字の文字列。
func galleryMoreButtonHTML(remaining string) string {
	return `<button type="button" class="` + GalleryMoreButtonClass + `">もっと見る（残り ` + remaining + ` 件）</button>`
}

// GalleryItemsHTML はタグのギャラリーの写真の一覧（div.gallery-item の並び）を、本文と同じ形・同じサニタイズで返す。
// 続きの API（GalleryAPIPath）が「もっと見る」で足す分に使う。
func GalleryItemsHTML(items []GalleryImage) string {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	for _, item := range items {
		renderGalleryItemStrings(w, item, false, false)
	}
	_ = w.Flush()
	return sanitizeHTML(buf.String())
}

// renderGalleryItemStrings はタグのギャラリー（resolver が返した写真）の1枚分を出力する。hidden なら hidden 属性を付ける
// （静的な書き出しで「もっと見る」まで見せない写真）。withYears なら写真の年（Years）を data-gallery-years に出す
// （年別の絞り込みのギャラリー。年の無い写真は空の値にし、「すべて」以外では見せない）。
// 一覧の src は軽いサムネイル（ThumbURL）、拡大の data-full は高解像度（FullURL）で、空なら URL を使う。
// 元画像の寸法（Width・Height）が分かれば width・height 属性に出し、読み込み中の表示のずれ（CLS）を防ぐ。
func renderGalleryItemStrings(w util.BufWriter, item GalleryImage, hidden, withYears bool) {
	src := galleryURL(item.ThumbURL, item.URL)
	if src == "" {
		return
	}
	full := galleryURL(item.FullURL, item.URL)
	if full == "" {
		full = src
	}
	label := item.Alt
	if label == "" {
		label = item.Caption
	}
	_, _ = w.WriteString(`<div class="` + GalleryItemClass + `" tabindex="0" role="button" aria-label="画像を拡大表示`)
	if label != "" {
		_, _ = w.WriteString(": " + stdhtml.EscapeString(label))
	}
	_, _ = w.WriteString(`"`)
	if withYears {
		var years []string
		for _, y := range strings.Fields(item.Years) {
			years = appendGalleryYear(years, y)
		}
		_, _ = w.WriteString(` data-gallery-years="` + strings.Join(SortGalleryYears(years), " ") + `"`)
	}
	if hidden {
		_, _ = w.WriteString(` hidden`)
	}
	_, _ = w.WriteString(`><img src="` + stdhtml.EscapeString(src) + `" data-full="` + stdhtml.EscapeString(full) + `"`)
	if item.Width > 0 && item.Height > 0 {
		_, _ = w.WriteString(` width="` + strconv.Itoa(item.Width) + `" height="` + strconv.Itoa(item.Height) + `"`)
	}
	_, _ = w.WriteString(` alt="` + stdhtml.EscapeString(item.Alt) + `" loading="lazy" decoding="async">`)
	if item.Caption != "" {
		_, _ = w.WriteString("<figcaption>" + stdhtml.EscapeString(item.Caption) + "</figcaption>")
	}
	_, _ = w.WriteString("</div>\n")
}

// galleryURL は候補の URL のうち、空でなく危険でない最初のものを返す。どれも使えなければ空。
func galleryURL(candidates ...string) string {
	for _, u := range candidates {
		if u != "" && !html.IsDangerousURL([]byte(u)) {
			return u
		}
	}
	return ""
}

// renderGalleryItem は画像1枚分を出力する。alt と caption はバックスラッシュエスケープと実体参照が未解決の Markdown ソース。
func renderGalleryItem(w util.BufWriter, dest, alt, caption []byte) {
	label := alt
	if len(label) == 0 {
		label = caption
	}
	_, _ = w.WriteString(`<div class="` + GalleryItemClass + `" tabindex="0" role="button" aria-label="画像を拡大表示`)
	if len(label) > 0 {
		_, _ = w.WriteString(": ")
		html.DefaultWriter.Write(w, label)
	}
	_, _ = w.WriteString(`"><img src="`)
	_, _ = w.Write(util.EscapeHTML(dest))
	_, _ = w.WriteString(`" data-full="`)
	_, _ = w.Write(util.EscapeHTML(dest))
	_, _ = w.WriteString(`" alt="`)
	html.DefaultWriter.Write(w, alt)
	_, _ = w.WriteString(`" loading="lazy" decoding="async">`)
	if len(caption) > 0 {
		_, _ = w.WriteString("<figcaption>")
		html.DefaultWriter.Write(w, caption)
		_, _ = w.WriteString("</figcaption>")
	}
	_, _ = w.WriteString("</div>\n")
}

// imageAltText は画像の代替テキスト（強調などの装飾を除いた文字列）を返す。
func imageAltText(n ast.Node, source []byte) []byte {
	var buf []byte
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			buf = append(buf, t.Segment.Value(source)...)
			if t.SoftLineBreak() || t.HardLineBreak() {
				buf = append(buf, ' ')
			}
		case *ast.String:
			buf = append(buf, t.Value...)
		default:
			buf = append(buf, imageAltText(c, source)...)
		}
	}
	return bytes.TrimSpace(buf)
}
