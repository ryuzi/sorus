// Package markdown は記事本文の Markdown を安全な HTML に変換する。
// Goldmark (GFM + LaTeX 数式・Mermaid の図・フォトギャラリー・注意書き・タブ・カード・行動の呼びかけ・よくある質問) でパースし、見出しにアンカー ID を付与して目次を抽出した上で、
// Bluemonday で XSS サニタイズする。
package markdown

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// RenderResult は Render の結果。HTML はサニタイズ済み。
type RenderResult struct {
	HTML     string    `json:"html"`
	Headings []Heading `json:"headings"`
}

// headingIDPattern は見出しの id 属性として許可する値。
// UGCPolicy 標準の id パターンは ASCII 前提で日本語の見出し ID を落とすため、見出し要素にだけ追加で許可する。
var headingIDPattern = regexp.MustCompile(`^[\p{L}\p{N}_-]+$`)

// altTextPattern は画像の代替テキスト（alt 属性）として許可する値。
// UGCPolicy 標準の Paragraph パターンは ASCII 記号のみで日本語の句読点（、。）や全角記号を落とすため、
// 制御文字と HTML タグ区切り文字（<>）を除いた安全な文字列を追加で許可する。
var altTextPattern = regexp.MustCompile(`^[^\x00-\x1f<>]*$`)

// galleryFullURLPattern はギャラリー画像の拡大表示用 URL（data-full 属性）として許可する値。
var galleryFullURLPattern = regexp.MustCompile(`^(?:https?://[^\s"'<>]+|[^\s:"'<>]+)$`)

// semanticClassElements は、本文の生 HTML に書いたクラスを残し、テーマの CSS から選べるようにする要素。
var semanticClassElements = []string{
	"div", "p", "figure", "figcaption", "img", "ul", "ol", "li", "table", "blockquote", "section", "span",
}

// reservedClassNames は sorus の生成物（数式・Mermaid の図・フォトギャラリー・タブ）と、それを描画するスクリプト・スタイルが目印に使うクラス。
// 生 HTML の汎用クラスには使えず、生成物と同じ要素・同じ値のとき（newPolicy の個別の許可）だけ残す。
// 注意書き（callout.go）はスクリプトを使わず、生 HTML の callout クラスも今までどおり使えるよう、予約しない。
var reservedClassNames = append(append([]string{MathInlineClass, MathDisplayClass, MermaidClass, GalleryItemClass, GalleryClass, GalleryMasonryGridClass,
	GalleryMoreClass, GalleryCountClass, GalleryMoreButtonClass},
	galleryLayoutClassNames()...), tabsClassNames...)

// galleryLayoutClassNames は並べ方のクラス（gallery-layout-justified など）の一覧を返す。
func galleryLayoutClassNames() []string {
	names := make([]string, 0, len(GalleryLayouts))
	for _, l := range GalleryLayouts {
		names = append(names, GalleryLayoutClassPrefix+l)
	}
	return names
}

// galleryFigureClassPattern はギャラリーの figure の class 属性として許可する値。gallery.go が出力する並べ方ごとの値
// （GalleryFigureClass）と、並べ方を導入する前の値（photo-gallery masonry-grid）だけに一致する。
var galleryFigureClassPattern = regexp.MustCompile(galleryFigureClassRegexp())

func galleryFigureClassRegexp() string {
	alts := []string{regexp.QuoteMeta(legacyGalleryClass)}
	for _, l := range GalleryLayouts {
		alts = append(alts, regexp.QuoteMeta(GalleryFigureClass(l)))
	}
	return `^(?:` + strings.Join(alts, `|`) + `)$`
}

// galleryMoreURLPattern はギャラリーの figure の data-gallery-more 属性（続きの API の URL）として許可する値。
// 同じサイトの GalleryAPIPath への問い合わせだけに限る。
var galleryMoreURLPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(GalleryAPIPath) + `\?[A-Za-z0-9_.~%&=+-]*$`)

// galleryLayoutPattern はギャラリーの figure の data-gallery-layout 属性（justified_gallery.js の目印）として許可する値。
var galleryLayoutPattern = regexp.MustCompile(`^(?:` + strings.Join(GalleryLayouts, `|`) + `)$`)

// classListPattern は semanticClassElements の class 属性として許可する値。
// クラス名（英数字・_・-）を空白区切りで1つ以上並べ、どのクラス名も reservedClassNames に一致しないもの。
var classListPattern = regexp.MustCompile(classListExcluding(reservedClassNames))

// classNameChars はクラス名に使える文字。
const classNameChars = "-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz"

// classListExcluding は、クラス名を空白区切りで1つ以上並べ、どのクラス名も reserved に一致しない値に一致する
// 正規表現を返す。Go の正規表現（RE2）には否定先読みが無いため、「reserved のどれとも一致しないクラス名」を
// reserved の接頭辞の木から組み立てる。
func classListExcluding(reserved []string) string {
	name := classNameExcluding(reserved, true)
	return `^` + name + `(?:\s+` + name + `)*$`
}

// classNameExcluding は、読み終えた接頭辞に続く reserved の語の残り（rest）から、その続きとして reserved の語に
// ならない文字列に一致する正規表現を返す。start は、まだ1文字も読んでいない（空のクラス名を許さない）とき。
func classNameExcluding(rest []string, start bool) string {
	next := map[byte][]string{}
	isReserved := false
	for _, r := range rest {
		if r == "" {
			isReserved = true
			continue
		}
		next[r[0]] = append(next[r[0]], r[1:])
	}
	var alts []string
	if !start && !isReserved {
		alts = append(alts, "") // 読み終えた接頭辞は reserved の語ではないため、ここで終わってよい。
	}
	// reserved の語から外れる文字が来たら、残りは任意のクラス名の文字でよい。
	var other []byte
	for i := 0; i < len(classNameChars); i++ {
		if _, ok := next[classNameChars[i]]; !ok {
			other = append(other, classNameChars[i])
		}
	}
	alts = append(alts, charClass(other)+charClass([]byte(classNameChars))+`*`)
	// reserved の語の途中の文字が来たら、その続きを同じ規則で調べる。
	for i := 0; i < len(classNameChars); i++ {
		if r, ok := next[classNameChars[i]]; ok {
			alts = append(alts, regexp.QuoteMeta(string(classNameChars[i]))+classNameExcluding(r, false))
		}
	}
	return `(?:` + strings.Join(alts, `|`) + `)`
}

// charClass は chars（classNameChars の並び順の部分集合）の1文字に一致する文字クラスを、連続する文字を範囲にまとめて返す。
func charClass(chars []byte) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < len(chars); {
		j := i
		for j+1 < len(chars) && chars[j+1] == chars[j]+1 {
			j++
		}
		writeClassChar(&b, chars[i])
		if j-i >= 2 {
			b.WriteByte('-')
			writeClassChar(&b, chars[j])
		} else if j > i {
			writeClassChar(&b, chars[j])
		}
		i = j + 1
	}
	b.WriteByte(']')
	return b.String()
}

func writeClassChar(b *strings.Builder, c byte) {
	if c == '-' {
		b.WriteByte('\\')
	}
	b.WriteByte(c)
}

var iframeElementPattern = regexp.MustCompile(`(?is)<iframe\b[^>]*>.*?</iframe\s*>`)
var approvedYouTubeSrcPattern = regexp.MustCompile(`\bsrc="https://www\.youtube-nocookie\.com/embed/[A-Za-z0-9_-]{11}(?:\?[^"<>]*)?"`)

// policy は生成後に状態を変更しないため、並行に使用してよい。
var policy = newPolicy()

func newMarkdown(opts GalleryOptions, tabs *tabsState) goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			// extension.GFM 相当。表の揃えは UGCPolicy が style 属性を落とすため align 属性で出力する。
			extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute)),
			extension.Strikethrough,
			extension.Linkify,
			extension.TaskList,
			mathExtension{},
			mermaidExtension{},
			galleryExtension{opts: opts},
			calloutExtension{},
			tabsExtension{state: tabs},
			sectionsExtension{},
		),
		// 生 HTML は Goldmark で捨てずに出力し、Bluemonday で許可要素のみ残す。
		// WordPress 由来の本文など、安全な生 HTML を含む記事を扱うため。
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)
}

func newPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("id").Matching(headingIDPattern).OnElements("h1", "h2", "h3", "h4", "h5", "h6")
	// GFM タスクリストのチェックボックス（表示専用）
	p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
	p.AllowAttrs("checked", "disabled").Matching(regexp.MustCompile(`^$`)).OnElements("input")
	// LaTeX 数式（math.go）。KaTeX が描画対象を見分けるクラスだけを許可する。
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^(` + MathInlineClass + `|` + MathDisplayClass + `)$`)).OnElements("span")
	// Mermaid の図（mermaid.go）。mermaid.js が描画対象を見分ける pre のクラスだけを許可する。中身はテキストとしてサニタイズされる。
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^` + MermaidClass + `$`)).OnElements("pre")
	// フォトギャラリー（gallery.go）。lightbox.js が拡大表示に使う属性だけを許可する。
	p.AllowElements("figure", "figcaption")
	p.AllowAttrs("class").Matching(galleryFigureClassPattern).OnElements("figure")
	p.AllowAttrs("data-gallery-layout").Matching(galleryLayoutPattern).OnElements("figure")
	p.AllowAttrs("data-gallery-tag").Matching(regexp.MustCompile(`^[^\x00-\x1f<>"']{1,200}$`)).OnElements("figure")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^` + GalleryItemClass + `$`)).OnElements("div")
	// 「もっと見る」（gallery.go の renderTagGallery）。総数・続きの API（同じサイトの GalleryAPIPath だけ）・まだ見せない写真の
	// hidden・総枚数の行のクラスだけを許可する。ボタン（button は属性が無くても残る要素のため許可しない）はサニタイズの後に
	// 差し込む（expandGalleryMoreButtons）。
	p.AllowAttrs("data-gallery-total").Matching(regexp.MustCompile(`^[0-9]{1,9}$`)).OnElements("figure")
	p.AllowAttrs("data-gallery-more").Matching(galleryMoreURLPattern).OnElements("figure")
	p.AllowAttrs("hidden").Matching(regexp.MustCompile(`^$`)).OnElements("div")
	// 年別の絞り込み（year_filter="true"）の写真の年。ピル（button）はボタンと同じくサニタイズの後に差し込む。
	p.AllowAttrs("data-gallery-years").Matching(regexp.MustCompile(`^(?:[0-9]{4}年(?: [0-9]{4}年)*)?$`)).OnElements("div")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^` + GalleryMoreClass + `$`)).OnElements("p")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^` + GalleryCountClass + `$`)).OnElements("span")
	p.AllowAttrs("tabindex").Matching(regexp.MustCompile(`^0$`)).OnElements("div")
	p.AllowAttrs("role").Matching(regexp.MustCompile(`^button$`)).OnElements("div")
	p.AllowAttrs("aria-label").OnElements("div")
	// GitHub 風の注意書き（callout.go）。全体・本文の div と見出しの p のクラス、role="note" だけを許可する。
	p.AllowAttrs("class").Matching(calloutClassPattern).OnElements("div")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^` + CalloutTitleClass + `$`)).OnElements("p")
	p.AllowAttrs("role").Matching(regexp.MustCompile(`^note$`)).OnElements("div")
	// タブ切り替え（tabs.go）の中身。中身の div のクラス・role="tabpanel"・ボタンへの aria-labelledby と、名前の p のクラス
	// だけを許可する（id は UGCPolicy が許可している）。外枠（data-tabs）・タブの並び（role="tablist"）・ボタン（button）は
	// 「もっと見る」と同じく、サニタイズの後に差し込む（expandTabs）。
	p.AllowAttrs("class").Matching(tabPanelClassPattern).OnElements("div")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^` + TabTitleClass + `$`)).OnElements("p")
	p.AllowAttrs("role").Matching(regexp.MustCompile(`^tabpanel$`)).OnElements("div")
	p.AllowAttrs("aria-labelledby").Matching(tabLabelledByPattern).OnElements("div")
	// カード・行動の呼びかけ・よくある質問（sections.go）。生成物の div のクラスと、質問の details・summary のクラスだけを許可する。
	// スクリプトを使わないため予約せず、生 HTML でも同じクラスを使える（details の open は UGCPolicy が許可している）。
	p.AllowAttrs("class").Matching(sectionDivClassPattern).OnElements("div")
	p.AllowAttrs("class").Matching(faqItemClassPattern).OnElements("details")
	p.AllowAttrs("class").Matching(faqQuestionClassPattern).OnElements("summary")
	// altTextPattern で日本語の句読点（、。）を含む安全な代替テキストを許可する。
	p.AllowAttrs("alt").Matching(altTextPattern).OnElements("img")
	// data-full は bluemonday の URL 検査の対象外のため、http(s) の絶対 URL か ":" を含まない相対 URL に限る。
	p.AllowAttrs("data-full").Matching(galleryFullURLPattern).OnElements("img")
	p.AllowAttrs("loading").Matching(regexp.MustCompile(`^lazy$`)).OnElements("img")
	p.AllowAttrs("decoding").Matching(regexp.MustCompile(`^async$`)).OnElements("img")
	// img の width・height（元画像の寸法。CLS 防止と行揃えの縦横比）は UGCPolicy（AllowImages）が数値・百分率だけ許可している。
	// YouTube privacy-enhanced embeds. Only the youtube-nocookie host and a video ID are allowed.
	p.AllowElements("iframe")
	p.AllowAttrs("src").Matching(regexp.MustCompile(`^https://www\.youtube-nocookie\.com/embed/[A-Za-z0-9_-]{11}(\?.*)?$`)).OnElements("iframe")
	p.AllowAttrs("title").OnElements("iframe")
	p.AllowAttrs("loading").Matching(regexp.MustCompile(`^lazy$`)).OnElements("iframe")
	p.AllowAttrs("allowfullscreen").Matching(regexp.MustCompile(`^$`)).OnElements("iframe")
	p.AllowAttrs("referrerpolicy").Matching(regexp.MustCompile(`^strict-origin-when-cross-origin$`)).OnElements("iframe")
	// テーマの CSS から選べる汎用クラス（docs/theme_development_spec.md「本文の生 HTML のクラス」）。
	// bluemonday は同じ属性の許可のどれかに一致すれば残すため、上の生成物のクラスはそれぞれの個別の許可で残る。
	p.AllowAttrs("class").Matching(classListPattern).OnElements(semanticClassElements...)
	return p
}

// Render は Markdown を GFM でパースし、見出しIDを付与した上で Bluemonday で XSS サニタイズした安全な HTML を返す
func Render(mdText string) (*RenderResult, error) {
	return RenderWithGallery(mdText, nil)
}

// RenderWithGallery renders Markdown and resolves tag based galleries against current media records.
func RenderWithGallery(mdText string, resolver GalleryResolver) (*RenderResult, error) {
	var queryResolver GalleryQueryResolver
	if resolver != nil {
		queryResolver = func(query GalleryQuery) ([]GalleryImage, error) { return resolver(query.Tag) }
	}
	return RenderWithGalleryQuery(mdText, queryResolver)
}

// RenderWithGalleryQuery renders Markdown and resolves tag galleries with limit and sort options.
// limit を書かないギャラリーは、続きの API を使わない形（すべて出力し、DefaultGalleryPageSize 枚より後ろを hidden）にする。
func RenderWithGalleryQuery(mdText string, resolver GalleryQueryResolver) (*RenderResult, error) {
	return RenderWithGalleryOptions(mdText, GalleryOptions{Resolver: pageResolverFrom(resolver)})
}

// RenderWithGalleryOptions は Markdown を描画し、タグのギャラリーを opts に従って出力する（総数付きの resolver と、
// 「もっと見る」の続きの API）。
func RenderWithGalleryOptions(mdText string, opts GalleryOptions) (*RenderResult, error) {
	src := []byte(mdText)
	opts.buttonToken = rand.Text()
	tabs := &tabsState{token: opts.buttonToken}
	md := newMarkdown(opts, tabs)
	doc := md.Parser().Parse(text.NewReader(src))
	headings := annotateHeadings(doc, src)

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return nil, fmt.Errorf("markdown: render: %w", err)
	}
	return &RenderResult{
		HTML:     tabs.expandTabs(expandGalleryMoreButtons(sanitizeHTML(buf.String()), opts.buttonToken)),
		Headings: headings,
	}, nil
}

func sanitizeHTML(raw string) string {
	safe := policy.Sanitize(raw)
	return iframeElementPattern.ReplaceAllStringFunc(safe, func(frame string) string {
		if !approvedYouTubeSrcPattern.MatchString(frame) {
			return ""
		}
		return frame
	})
}
