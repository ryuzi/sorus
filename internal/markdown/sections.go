package markdown

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// ドキュメントのページに使う汎用のセクション（カード・行動の呼びかけ・よくある質問）。注意書き（callout.go）と同じく
// スクリプトを使わず、見た目はテーマ・公開サイトの CSS（.doc-cards・.doc-cta・.doc-faq）が受け持つ。中身は通常の
// Markdown として解釈し、生 HTML は今までどおりサニタイズする。どれも閉じの ::: が無いものは、通常の段落として扱う。
//
// カード（:::cards）は、:::card と ::: で囲んだ範囲、または --- の行で区切った範囲を1枚のカードにする。
//
//	:::cards
//	:::card
//	### はじめる
//	インストールから最初の記事まで。
//	[手順を見る](/docs/start)
//	:::
//	:::card
//	### テーマ
//	見た目を変える。
//	:::
//	:::
//
// を <div class="doc-cards"><div class="doc-card">...</div>...</div> として出力する。--- で区切る書き方では、カードの中に
// 水平線（---）は書けない（*** なら書ける）。
//
// 行動の呼びかけ（:::cta）は、最後に並べたリンクだけの段落・リンクだけの箇条書きをボタンの欄にし、残りを本文の欄にする。
//
//	:::cta
//	## 今すぐ始めましょう
//	1行でインストールできます。
//	[ダウンロード](/download) [ドキュメント](/docs)
//	:::
//
// を <div class="doc-cta"><div class="doc-cta-body">...</div><div class="doc-cta-actions">...</div></div> として出力する
// （どちらかの欄が空なら、その欄の div は出さない）。
//
// よくある質問（:::faq）は、:::q[質問] の行、または ### 質問 の行ごとに、開閉できる項目（details）にする。
//
//	:::faq
//	:::q[無料で使えますか]
//	はい。
//	### 予約投稿はできますか
//	できます。
//	:::
//
// を <div class="doc-faq"><details class="doc-faq-item"><summary class="doc-faq-q">無料で使えますか</summary>
// <div class="doc-faq-a">...</div></details>...</div> として出力する。質問は Markdown として解釈せず、文字のまま出す
// （目次にも載せない）。:::faq の次の行（空行は除く）が質問の行でないものは、通常の段落として扱う。
//
// 注意: タブ（tabs.go）は中のブロックの ::: を知らないため、これらのセクションはタブの中には書けない。

const (
	// CardsClass はカード全体を囲む div のクラス。CardClass はカード1枚の div のクラス。
	CardsClass = "doc-cards"
	CardClass  = "doc-card"
	// CTAClass は行動の呼びかけ全体の div のクラス。CTABodyClass は本文の欄、CTAActionsClass はボタン（リンク）の欄の div のクラス。
	CTAClass        = "doc-cta"
	CTABodyClass    = "doc-cta-body"
	CTAActionsClass = "doc-cta-actions"
	// FAQClass はよくある質問全体の div のクラス。FAQItemClass は1問の details、FAQQuestionClass は質問の summary、
	// FAQAnswerClass は答えの div のクラス。
	FAQClass         = "doc-faq"
	FAQItemClass     = "doc-faq-item"
	FAQQuestionClass = "doc-faq-q"
	FAQAnswerClass   = "doc-faq-a"
)

// sectionDivClassNames はセクションの生成物が div に付けるクラス。
var sectionDivClassNames = []string{CardsClass, CardClass, CTAClass, CTABodyClass, CTAActionsClass, FAQClass, FAQAnswerClass}

var (
	kindCards      = ast.NewNodeKind("Cards")
	kindCard       = ast.NewNodeKind("Card")
	kindCTA        = ast.NewNodeKind("CTA")
	kindCTABody    = ast.NewNodeKind("CTABody")
	kindCTAActions = ast.NewNodeKind("CTAActions")
	kindFAQ        = ast.NewNodeKind("FAQ")
	kindFAQItem    = ast.NewNodeKind("FAQItem")

	cardsOpenPattern = regexp.MustCompile(`^:::cards$`)
	cardOpenPattern  = regexp.MustCompile(`^:::card$`)
	// cardSeparatorPattern はカードの区切りの行（- を3つ以上）。
	cardSeparatorPattern = regexp.MustCompile(`^-{3,}$`)
	ctaOpenPattern       = regexp.MustCompile(`^:::cta$`)
	faqOpenPattern       = regexp.MustCompile(`^:::faq$`)
	// faqQuestionPattern・faqHeadingPattern は質問の行（:::q[質問] と ### 質問。### の後ろの閉じの # の並びは除く）。
	faqQuestionPattern = regexp.MustCompile(`^:::q\[(.*\S.*)\]$`)
	faqHeadingPattern  = regexp.MustCompile(`^###[ \t]+(.*?\S)(?:[ \t]+#+)?$`)

	// sectionDivClassPattern は生成物の div の class 属性として許可する値。
	sectionDivClassPattern = regexp.MustCompile(`^(?:` + strings.Join(sectionDivClassNames, `|`) + `)$`)
	// faqItemClassPattern・faqQuestionClassPattern は details・summary の class 属性として許可する値。
	faqItemClassPattern     = regexp.MustCompile(`^` + FAQItemClass + `$`)
	faqQuestionClassPattern = regexp.MustCompile(`^` + FAQQuestionClass + `$`)
)

// cardsNode はカード全体。閉じたときに、子をすべて cardNode にそろえる。
type cardsNode struct{ ast.BaseBlock }

func (n *cardsNode) Kind() ast.NodeKind            { return kindCards }
func (n *cardsNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// cardNode はカード1枚。explicit は :::card の行で開き、::: で閉じるもの（--- で区切ったものは false）。
type cardNode struct {
	ast.BaseBlock
	explicit bool
}

func (n *cardNode) Kind() ast.NodeKind            { return kindCard }
func (n *cardNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// ctaNode は行動の呼びかけ全体。インライン要素を解釈した後に、子を ctaBodyNode と ctaActionsNode に分ける。
type ctaNode struct{ ast.BaseBlock }

func (n *ctaNode) Kind() ast.NodeKind            { return kindCTA }
func (n *ctaNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

type ctaBodyNode struct{ ast.BaseBlock }

func (n *ctaBodyNode) Kind() ast.NodeKind            { return kindCTABody }
func (n *ctaBodyNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

type ctaActionsNode struct{ ast.BaseBlock }

func (n *ctaActionsNode) Kind() ast.NodeKind            { return kindCTAActions }
func (n *ctaActionsNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// faqNode はよくある質問全体。子は faqItemNode だけ。
type faqNode struct{ ast.BaseBlock }

func (n *faqNode) Kind() ast.NodeKind            { return kindFAQ }
func (n *faqNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// faqItemNode は1問。子には答えを通常の Markdown として解釈したブロックを持つ。
type faqItemNode struct {
	ast.BaseBlock
	// Question は質問（Markdown のソースのまま）。
	Question string
}

func (n *faqItemNode) Kind() ast.NodeKind { return kindFAQItem }
func (n *faqItemNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Question": n.Question}, nil)
}

// sectionsExtension はカード・行動の呼びかけ・よくある質問のパーサーとレンダラーを Goldmark に登録する。
type sectionsExtension struct{}

func (sectionsExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithBlockParsers(
			// カード1枚・質問1つは、親が全体のブロックのときだけ開く。区切りの行（--- と ### 質問）を水平線・Setext 見出し
			// （優先度 100）・ATX 見出しより先に受け取るため、それらより前に試す。
			util.Prioritized(cardParser{}, 90),
			util.Prioritized(faqItemParser{}, 91),
			// 全体のブロックは、フォトギャラリー・タブと同じくフェンスコードブロック（優先度 700）より前に試す。
			util.Prioritized(cardsParser{}, 642),
			util.Prioritized(ctaParser{}, 643),
			util.Prioritized(faqParser{}, 644),
		),
		parser.WithASTTransformers(util.Prioritized(ctaTransformer{}, 500)),
	)
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(sectionsRenderer{}, 500)))
}

// sectionLine は今の行のうち、字下げが4文字未満なら行頭の空白と行末の空白を除いた中身を返す（ok=false なら字下げが深い）。
func sectionLine(reader text.Reader) ([]byte, bool) {
	line, _ := reader.PeekLine()
	w, pos := util.IndentWidth(line, reader.LineOffset())
	if w >= 4 || pos > len(line) {
		return nil, false
	}
	return util.TrimRightSpace(line[pos:]), true
}

// openLine は Open に渡された今の行の、ブロックの開始位置からの中身を返す。
func openLine(reader text.Reader, pc parser.Context) ([]byte, bool) {
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || pos > len(line) {
		return nil, false
	}
	return util.TrimRightSpace(line[pos:]), true
}

// --- カード ---

type cardsParser struct{}

func (cardsParser) Trigger() []byte { return []byte{':'} }

// Open は :::cards の行でカード全体を開く。行は読み進めず、同じ行をカード全体の直下で cardParser に渡して最初のカードを開く
// （--- で区切る書き方の最初のカードにも、--- を Setext 見出しにしないための入れ物が要るため）。
func (cardsParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	if parent.Kind() == kindCards {
		return nil, parser.NoChildren
	}
	line, ok := openLine(reader, pc)
	if !ok || !cardsOpenPattern.Match(line) || !hasSectionClose(reader, true) {
		return nil, parser.NoChildren
	}
	return &cardsNode{}, parser.HasChildren
}

// Continue は閉じの ::: でカード全体を閉じる。中に開いている :::card のカードがあれば、その ::: はカードに任せる。
func (cardsParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	if hasOpenSectionBelow(node, pc) {
		return parser.Continue | parser.HasChildren
	}
	line, ok := sectionLine(reader)
	if ok && bytes.Equal(line, galleryCloseMarker) {
		if card, isCard := openChildBlock(node, pc).(*cardNode); isCard && card.explicit {
			return parser.Continue | parser.HasChildren
		}
		reader.AdvanceToEOL()
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

// Close は子をカードにそろえる。空のカードを除き、カードの外に書いた中身（:::card の閉じの後ろの段落など）はカードで包む。
func (cardsParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {
	var loose *cardNode
	for c := node.FirstChild(); c != nil; {
		next := c.NextSibling()
		if card, ok := c.(*cardNode); ok {
			loose = nil
			if !card.HasChildren() {
				node.RemoveChild(node, c)
			}
			c = next
			continue
		}
		if loose == nil {
			loose = &cardNode{}
			node.InsertBefore(node, c, loose)
		}
		node.RemoveChild(node, c)
		loose.AppendChild(loose, c)
		c = next
	}
}

func (cardsParser) CanInterruptParagraph() bool { return true }

func (cardsParser) CanAcceptIndentedLine() bool { return false }

type cardParser struct{}

func (cardParser) Trigger() []byte { return []byte{':', '-'} }

// Open はカード全体の直下で、:::card の行（::: で閉じるカード）、--- の行と :::cards の行（区切りまでのカード）から
// カード1枚を開く。カード全体の外では何もしない。
func (cardParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	if parent.Kind() != kindCards {
		return nil, parser.NoChildren
	}
	line, ok := openLine(reader, pc)
	if !ok {
		return nil, parser.NoChildren
	}
	var card *cardNode
	switch {
	case cardOpenPattern.Match(line):
		card = &cardNode{explicit: true}
	case cardSeparatorPattern.Match(line) || cardsOpenPattern.Match(line):
		card = &cardNode{}
	default:
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return card, parser.HasChildren
}

// Continue は次のカードの開き（:::card・---）で閉じる（その行はカード全体の直下で次のカードを開く）。:::card で開いた
// カードは閉じの ::: を読んで閉じる。
func (cardParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	if hasOpenSectionBelow(node, pc) {
		return parser.Continue | parser.HasChildren
	}
	line, ok := sectionLine(reader)
	if !ok {
		return parser.Continue | parser.HasChildren
	}
	switch {
	case cardOpenPattern.Match(line) || cardSeparatorPattern.Match(line):
		return parser.Close
	case bytes.Equal(line, galleryCloseMarker):
		if node.(*cardNode).explicit {
			reader.AdvanceToEOL()
		}
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

func (cardParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (cardParser) CanInterruptParagraph() bool { return true }

func (cardParser) CanAcceptIndentedLine() bool { return false }

// --- 行動の呼びかけ ---

type ctaParser struct{}

func (ctaParser) Trigger() []byte { return []byte{':'} }

func (ctaParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, ok := openLine(reader, pc)
	if !ok || !ctaOpenPattern.Match(line) || !hasSectionClose(reader, false) {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &ctaNode{}, parser.HasChildren
}

func (ctaParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	return continueUntilClose(node, reader, pc)
}

func (ctaParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (ctaParser) CanInterruptParagraph() bool { return true }

func (ctaParser) CanAcceptIndentedLine() bool { return false }

// continueUntilClose は、中に自分で ::: を読むブロックが開いていなければ、閉じの ::: で閉じる。
func continueUntilClose(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	if hasOpenSectionBelow(node, pc) {
		return parser.Continue | parser.HasChildren
	}
	if line, ok := sectionLine(reader); ok && bytes.Equal(line, galleryCloseMarker) {
		reader.AdvanceToEOL()
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

// ctaTransformer は、インライン要素（リンク）を解釈した後に、行動の呼びかけの子を本文の欄とボタンの欄に分ける。
type ctaTransformer struct{}

func (ctaTransformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	var ctas []*ctaNode
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if c, ok := n.(*ctaNode); ok && entering {
			ctas = append(ctas, c)
		}
		return ast.WalkContinue, nil
	})
	for _, c := range ctas {
		splitCTA(c, reader.Source())
	}
}

// splitCTA は、最後に続くリンクだけのブロックをボタンの欄（ctaActionsNode）に、その前を本文の欄（ctaBodyNode）に移す。
func splitCTA(c *ctaNode, source []byte) {
	if p, ok := c.LastChild().(*ast.Paragraph); ok && !isLinkOnlyBlock(p, source) {
		splitTrailingLinks(p, source)
	}
	var firstAction ast.Node
	for n := c.LastChild(); n != nil && isLinkOnlyBlock(n, source); n = n.PreviousSibling() {
		firstAction = n
	}
	body, actions := &ctaBodyNode{}, &ctaActionsNode{}
	var dst ast.Node = body
	for n := c.FirstChild(); n != nil; {
		next := n.NextSibling()
		if n == firstAction {
			dst = actions
		}
		c.RemoveChild(c, n)
		dst.AppendChild(dst, n)
		n = next
	}
	if body.HasChildren() {
		c.AppendChild(c, body)
	}
	if actions.HasChildren() {
		c.AppendChild(c, actions)
	}
}

// splitTrailingLinks は、段落の最後の行（から後ろの行）がリンクだけのとき、その行を新しい段落に分けて後ろに置く。
// 説明の行のすぐ下に空行を入れずにリンクの行を書いても、リンクをボタンの欄に出すため。
func splitTrailingLinks(p *ast.Paragraph, source []byte) {
	var cut ast.Node
	var lineEnd *ast.Text
	links := 0
	for c := p.LastChild(); c != nil; c = c.PreviousSibling() {
		if !isLinkOrBlankText(c, source) {
			break
		}
		if _, ok := c.(*ast.Text); !ok {
			links++
		}
		if prev, ok := c.PreviousSibling().(*ast.Text); ok && links > 0 && (prev.SoftLineBreak() || prev.HardLineBreak()) {
			cut, lineEnd = c, prev
		}
	}
	if cut == nil {
		return
	}
	lineEnd.SetSoftLineBreak(false)
	lineEnd.SetHardLineBreak(false)
	actions := ast.NewParagraph()
	for c := cut; c != nil; {
		next := c.NextSibling()
		p.RemoveChild(p, c)
		actions.AppendChild(actions, c)
		c = next
	}
	p.Parent().InsertAfter(p.Parent(), p, actions)
}

// isLinkOrBlankText は、n がリンクか、空白だけの文字かを返す。
func isLinkOrBlankText(n ast.Node, source []byte) bool {
	switch v := n.(type) {
	case *ast.Link, *ast.AutoLink:
		return true
	case *ast.Text:
		return len(bytes.TrimSpace(v.Value(source))) == 0
	}
	return false
}

// isLinkOnlyBlock は、n がリンクだけの段落（リンクの間は空白・改行だけ）か、どの項目もリンクだけの箇条書きかを返す。
func isLinkOnlyBlock(n ast.Node, source []byte) bool {
	switch n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		links := 0
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if !isLinkOrBlankText(c, source) {
				return false
			}
			if _, isText := c.(*ast.Text); !isText {
				links++
			}
		}
		return links > 0
	case *ast.List:
		if !n.HasChildren() {
			return false
		}
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			if !item.HasChildren() {
				return false
			}
			for c := item.FirstChild(); c != nil; c = c.NextSibling() {
				if !isLinkOnlyBlock(c, source) {
					return false
				}
			}
		}
		return true
	}
	return false
}

// --- よくある質問 ---

type faqParser struct{}

func (faqParser) Trigger() []byte { return []byte{':'} }

func (faqParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, ok := openLine(reader, pc)
	if !ok || !faqOpenPattern.Match(line) || !faqStartsWithQuestion(reader) || !hasSectionClose(reader, false) {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &faqNode{}, parser.HasChildren
}

func (faqParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	return continueUntilClose(node, reader, pc)
}

func (faqParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (faqParser) CanInterruptParagraph() bool { return true }

func (faqParser) CanAcceptIndentedLine() bool { return false }

type faqItemParser struct{}

func (faqItemParser) Trigger() []byte { return []byte{':', '#'} }

// Open はよくある質問の直下の質問の行で1問を開く。よくある質問の外の :::q[質問]・### 質問 は今までどおり段落・見出しになる。
func (faqItemParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	if parent.Kind() != kindFAQ {
		return nil, parser.NoChildren
	}
	line, ok := openLine(reader, pc)
	if !ok {
		return nil, parser.NoChildren
	}
	q, ok := faqQuestion(line)
	if !ok {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &faqItemNode{Question: q}, parser.HasChildren
}

// Continue は次の質問の行で閉じる（その行はよくある質問の直下で次の1問を開く）。閉じの ::: はよくある質問が先に受け取る。
func (faqItemParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	if hasOpenSectionBelow(node, pc) {
		return parser.Continue | parser.HasChildren
	}
	if line, ok := sectionLine(reader); ok {
		if _, isQ := faqQuestion(line); isQ {
			return parser.Close
		}
	}
	return parser.Continue | parser.HasChildren
}

func (faqItemParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (faqItemParser) CanInterruptParagraph() bool { return true }

func (faqItemParser) CanAcceptIndentedLine() bool { return false }

// faqQuestion は質問の行（:::q[質問] か ### 質問）の質問を返す。行の形が違うときは ok=false。
func faqQuestion(line []byte) (string, bool) {
	if m := faqQuestionPattern.FindSubmatch(line); m != nil {
		return strings.TrimSpace(string(m[1])), true
	}
	if m := faqHeadingPattern.FindSubmatch(line); m != nil {
		return strings.TrimSpace(string(m[1])), true
	}
	return "", false
}

// faqStartsWithQuestion は :::faq の次の行（空行は除く）が質問の行かを、読み取り位置を変えずに調べる。
func faqStartsWithQuestion(reader text.Reader) bool {
	defer restorePosition(reader)()
	for {
		reader.AdvanceLine()
		raw, _ := reader.PeekLine()
		if raw == nil {
			return false
		}
		line := stripQuotePrefix(raw)
		if len(line) == 0 {
			continue
		}
		_, ok := faqQuestion(line)
		return ok
	}
}

// --- 共通 ---

// restorePosition は、先読みの後に読み取り位置を今の位置へ戻す関数を返す。SetPosition は先読みで覚えた行（PeekLine の結果）を
// 消さないため、Advance(0) で消してから戻す（消さないと、戻した後の PeekLine・AdvanceToEOL が先読みした行を使ってしまう）。
func restorePosition(reader text.Reader) func() {
	l, pos := reader.Position()
	return func() {
		reader.SetPosition(l, pos)
		reader.Advance(0)
	}
}

// openChildBlock は、開いているブロックのうち node のすぐ内側のものを返す（無ければ nil）。
func openChildBlock(node ast.Node, pc parser.Context) ast.Node {
	blocks := pc.OpenedBlocks()
	for i, b := range blocks {
		if b.Node == node && i+1 < len(blocks) {
			return blocks[i+1].Node
		}
	}
	return nil
}

// hasOpenSectionBelow は、開いているブロックのうち node より内側に、::: の行を自分で読むブロック（フェンスコードブロック・
// フォトギャラリー・タブ・カード・行動の呼びかけ・よくある質問）があるかを返す。
func hasOpenSectionBelow(node ast.Node, pc parser.Context) bool {
	below := false
	for _, b := range pc.OpenedBlocks() {
		if !below {
			below = b.Node == node
			continue
		}
		switch b.Node.(type) {
		case *ast.FencedCodeBlock, *galleryNode, *tabsNode, *cardsNode, *ctaNode, *faqNode:
			return true
		}
	}
	return false
}

// isSectionOpen は、閉じの ::: を持つブロックの開きの行（タブ・フォトギャラリー・行動の呼びかけ・よくある質問）かを返す。
// カード全体の開き（:::cards）は hasSectionClose が別に扱う。
func isSectionOpen(line []byte) bool {
	return tabsOpenPattern.Match(line) || isGalleryOpen(line) || ctaOpenPattern.Match(line) || faqOpenPattern.Match(line)
}

// sectionFrame は hasSectionClose が数える開いたブロック1つ。cards はカード全体で、explicitCard は中で :::card の
// カードを開いているとき（次の ::: はそのカードの閉じ）。
type sectionFrame struct{ cards, explicitCard bool }

// hasSectionClose は開きの行より後に、そのブロック（cards ならカード全体）の閉じの ::: があるかを、読み取り位置を変えずに
// 調べる。中のフェンスコードブロックの行は読み飛ばし、中のブロックの ::: と :::card のカードの ::: はその分を数えて飛ばす。
// 引用・リストの中のブロックも調べられるよう、行頭の空白と「>」は除いて比べる。
func hasSectionClose(reader text.Reader, cards bool) bool {
	defer restorePosition(reader)()
	stack := []sectionFrame{{cards: cards}}
	var fence []byte
	for {
		reader.AdvanceLine()
		raw, _ := reader.PeekLine()
		if raw == nil {
			return false
		}
		line := stripQuotePrefix(raw)
		if fence != nil {
			if isFenceEnd(line, fence) {
				fence = nil
			}
			continue
		}
		top := &stack[len(stack)-1]
		switch {
		case bytes.Equal(line, galleryCloseMarker):
			if top.cards && top.explicitCard {
				top.explicitCard = false
				continue
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return true
			}
		case top.cards && cardOpenPattern.Match(line):
			top.explicitCard = true
		case top.cards && cardSeparatorPattern.Match(line):
			top.explicitCard = false
		case cardsOpenPattern.Match(line):
			stack = append(stack, sectionFrame{cards: true})
		case isSectionOpen(line):
			stack = append(stack, sectionFrame{})
		default:
			fence = fenceStart(line)
		}
	}
}

type sectionsRenderer struct{}

func (sectionsRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindCards, renderSectionDiv(CardsClass))
	reg.Register(kindCard, renderSectionDiv(CardClass))
	reg.Register(kindCTA, renderSectionDiv(CTAClass))
	reg.Register(kindCTABody, renderSectionDiv(CTABodyClass))
	reg.Register(kindCTAActions, renderSectionDiv(CTAActionsClass))
	reg.Register(kindFAQ, renderSectionDiv(FAQClass))
	reg.Register(kindFAQItem, renderFAQItem)
}

// renderSectionDiv は子を class の div で囲んで出力するレンダラーを返す。
func renderSectionDiv(class string) renderer.NodeRendererFunc {
	return func(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			_, _ = w.WriteString(`<div class="` + class + `">` + "\n")
		} else {
			_, _ = w.WriteString("</div>\n")
		}
		return ast.WalkContinue, nil
	}
}

// renderFAQItem は1問を、質問を summary にした details として出力する。答えは div で囲む。
func renderFAQItem(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		_, _ = w.WriteString("</div>\n</details>\n")
		return ast.WalkContinue, nil
	}
	q := n.(*faqItemNode).Question
	_, _ = w.WriteString(`<details class="` + FAQItemClass + `">` + "\n")
	_, _ = w.WriteString(`<summary class="` + FAQQuestionClass + `">` + plainLabelHTML(q) + "</summary>\n")
	_, _ = w.WriteString(`<div class="` + FAQAnswerClass + `">` + "\n")
	return ast.WalkContinue, nil
}
