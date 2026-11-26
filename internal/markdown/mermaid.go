package markdown

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Mermaid の図のサポート。言語に mermaid を指定したコードブロック
//
//	```mermaid
//	flowchart LR
//	  A --> B
//	```
//
// を、図の定義（HTML エスケープしたテキストだけ）を入れた <pre class="mermaid"> として出力する。
// 描画は閲覧時にクライアント側の Mermaid（static/vendor/mermaid に同梱。static/js/mermaid.js が呼ぶ）が SVG にする。
// スクリプトが動かないとき・定義に誤りがあるときは、定義のテキストがそのまま読める。
// 中身が空のコードブロックは図にせず、通常のコードブロックのまま出力する。

// MermaidClass は Mermaid の図の定義を入れる pre のクラス。mermaid.js が描画対象を見分ける目印にする。
const MermaidClass = "mermaid"

// mermaidLanguage はコードブロックの言語（info 文字列の最初の語）のうち、Mermaid の図として扱う値（大文字・小文字は問わない）。
const mermaidLanguage = "mermaid"

// mermaidOpenTag は Render が出力する Mermaid の図の開始タグ（HasMermaid が探す）。
const mermaidOpenTag = `<pre class="` + MermaidClass + `">`

var kindMermaid = ast.NewNodeKind("Mermaid")

// mermaidNode は Mermaid の図1つ。行（Lines）は元のコードブロックの中身（図の定義）。
type mermaidNode struct {
	ast.BaseBlock
}

func (n *mermaidNode) Kind() ast.NodeKind { return kindMermaid }

func (n *mermaidNode) IsRaw() bool { return true }

func (n *mermaidNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// mermaidExtension は mermaid のコードブロックを図に置き換える変換と、そのレンダラーを Goldmark に登録する。
type mermaidExtension struct{}

func (mermaidExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithASTTransformers(util.Prioritized(mermaidTransformer{}, 500)))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(mermaidRenderer{}, 500)))
}

// mermaidTransformer は言語が mermaid のフェンス付きコードブロックを mermaidNode に置き換える。
// 注意書き・タブ・引用・リストの中のコードブロックも対象にする。
type mermaidTransformer struct{}

func (mermaidTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	var blocks []*ast.FencedCodeBlock
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if fcb, ok := n.(*ast.FencedCodeBlock); ok {
			if isMermaidFence(fcb, source) {
				blocks = append(blocks, fcb)
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	// 走査中に木を組み替えないよう、集め終えてから置き換える。
	for _, fcb := range blocks {
		node := &mermaidNode{}
		node.SetLines(fcb.Lines())
		if parent := fcb.Parent(); parent != nil {
			parent.ReplaceChild(parent, fcb, node)
		}
	}
}

// isMermaidFence はコードブロックの言語が mermaid で、中身が空白だけではないかを返す。
func isMermaidFence(fcb *ast.FencedCodeBlock, source []byte) bool {
	if fcb.Info == nil || !strings.EqualFold(string(fcb.Language(source)), mermaidLanguage) {
		return false
	}
	lines := fcb.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		if strings.TrimSpace(string(seg.Value(source))) != "" {
			return true
		}
	}
	return false
}

type mermaidRenderer struct{}

func (mermaidRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindMermaid, renderMermaid)
}

// renderMermaid は図の定義を HTML エスケープして <pre class="mermaid"> に入れる。生 HTML は差し込まない。
func renderMermaid(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString(mermaidOpenTag)
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		_, _ = w.Write(util.EscapeHTML(seg.Value(source)))
	}
	_, _ = w.WriteString("</pre>\n")
	return ast.WalkSkipChildren, nil
}

// HasMermaid は Render が出力した HTML に Mermaid の図が含まれるかを返す。Mermaid を読み込むかの判定に使う。
func HasMermaid(html string) bool {
	return strings.Contains(html, mermaidOpenTag)
}
