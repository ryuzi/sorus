package markdown

import (
	stdhtml "html"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// GitHub 風の注意書き（Callout）。引用の1行目に種類の印だけを書いた
//
//	> [!NOTE]
//	> 本文
//
// を、<div class="callout callout-note" role="note"> の中に見出し（p.callout-title）と本文（div.callout-body）を
// 並べた形で出力する。種類は NOTE・TIP・IMPORTANT・WARNING・CAUTION（大文字・小文字は問わない）。
// 印の後ろに文字を書くと（> [!WARNING] 消す前に）、既定の見出し（CalloutTitles）の代わりに使う。
// 本文は引用と同じく、通常の Markdown として解釈する。印の行の形が違う引用は、今までどおりの引用（blockquote）のまま。
// 見た目はテーマ・公開サイトの CSS（.callout・.callout-<種類>）が受け持ち、スクリプトは使わない。

const (
	// CalloutClass は注意書き全体を囲む div の共通のクラス。種類のクラス（CalloutTypeClassPrefix + 種類）と組み合わせて出す。
	CalloutClass = "callout"
	// CalloutTypeClassPrefix は種類のクラス（callout-note など）の接頭辞。
	CalloutTypeClassPrefix = "callout-"
	// CalloutTitleClass は見出しの p のクラス。
	CalloutTitleClass = "callout-title"
	// CalloutBodyClass は本文を囲む div のクラス。
	CalloutBodyClass = "callout-body"
)

// CalloutTypes は注意書きの種類（小文字。GitHub の Alerts と同じ並び）。
var CalloutTypes = []string{"note", "tip", "important", "warning", "caution"}

// CalloutTitles は種類ごとの既定の見出し（GitHub Docs の日本語版と同じ言葉）。
var CalloutTitles = map[string]string{
	"note":      "メモ",
	"tip":       "ヒント",
	"important": "重要",
	"warning":   "警告",
	"caution":   "注意",
}

var (
	kindCallout = ast.NewNodeKind("Callout")
	// calloutOpenPattern は注意書きの1行目（「>」と種類の印、任意の見出し）。
	calloutOpenPattern = regexp.MustCompile(`(?i)^>[ \t]?\[!(` + strings.Join(CalloutTypes, "|") + `)\](?:[ \t]+(.*))?$`)
	// calloutClassPattern は注意書きの div の class 属性として許可する値（全体の div と本文の div）。
	calloutClassPattern = regexp.MustCompile(`^(?:` + regexp.QuoteMeta(CalloutClass) + ` ` + regexp.QuoteMeta(CalloutTypeClassPrefix) +
		`(?:` + strings.Join(CalloutTypes, "|") + `)|` + regexp.QuoteMeta(CalloutBodyClass) + `)$`)
)

// calloutNode は注意書き1つ。子には引用の中身と同じく、通常の Markdown として解釈したブロックを持つ。
type calloutNode struct {
	ast.BaseBlock
	// Variant は種類（CalloutTypes のどれか）。
	Variant string
	// Title は印の後ろに書いた見出し（Markdown のソースのまま）。空なら既定の見出しを使う。
	Title string
}

func (n *calloutNode) Kind() ast.NodeKind { return kindCallout }

func (n *calloutNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Variant": n.Variant, "Title": n.Title}, nil)
}

// calloutExtension は注意書きのパーサーとレンダラーを Goldmark に登録する。
type calloutExtension struct{}

func (calloutExtension) Extend(m goldmark.Markdown) {
	// 引用（優先度 800）より前に試し、印の行で始まる引用だけを注意書きにする。
	m.Parser().AddOptions(parser.WithBlockParsers(util.Prioritized(calloutParser{}, 790)))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(calloutRenderer{}, 500)))
}

// calloutQuote は2行目からの「>」の読み方。引用（blockquote）と同じ規則にする。
var calloutQuote = parser.NewBlockquoteParser()

type calloutParser struct{}

func (calloutParser) Trigger() []byte { return []byte{'>'} }

func (calloutParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 {
		return nil, parser.NoChildren
	}
	m := calloutOpenPattern.FindSubmatch(util.TrimRightSpace(line[pos:]))
	if m == nil {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &calloutNode{Variant: strings.ToLower(string(m[1])), Title: strings.TrimSpace(string(m[2]))}, parser.HasChildren
}

func (calloutParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	return calloutQuote.Continue(node, reader, pc)
}

func (calloutParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (calloutParser) CanInterruptParagraph() bool { return true }

func (calloutParser) CanAcceptIndentedLine() bool { return false }

type calloutRenderer struct{}

func (calloutRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindCallout, renderCallout)
}

func renderCallout(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		_, _ = w.WriteString("</div>\n</div>\n")
		return ast.WalkContinue, nil
	}
	c := n.(*calloutNode)
	title := CalloutTitles[c.Variant]
	if c.Title != "" {
		title = c.Title
	}
	_, _ = w.WriteString(`<div class="` + CalloutClass + ` ` + CalloutTypeClassPrefix + c.Variant + `" role="note">` + "\n")
	_, _ = w.WriteString(`<p class="` + CalloutTitleClass + `">` + plainLabelHTML(title) + "</p>\n")
	_, _ = w.WriteString(`<div class="` + CalloutBodyClass + `">` + "\n")
	return ast.WalkContinue, nil
}

// plainLabelHTML は見出し・タブの名前など、Markdown を解釈しない1行の文字列を HTML の文字として出力できる形にする。
// 実体参照（&amp; など）は Markdown の本文と同じく文字に戻してから、改めてエスケープする。
func plainLabelHTML(s string) string {
	return stdhtml.EscapeString(stdhtml.UnescapeString(s))
}
