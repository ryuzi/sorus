package markdown

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// LaTeX 数式のサポート。$...$ をインライン数式、$$...$$ をブロック数式として扱う。
// 数式の中身は Markdown として解釈せず（_ や * を強調にしない）、HTML エスケープしたまま
// <span class="math-inline">\(...\)</span> / <span class="math-display">\[...\]</span> として出力する。
// 描画は閲覧時にクライアント側の KaTeX（auto-render の \( \) と \[ \] 区切り）が行う。

const (
	// MathInlineClass はインライン数式を囲む span のクラス。
	MathInlineClass = "math-inline"
	// MathDisplayClass はブロック数式を囲む span のクラス。
	MathDisplayClass = "math-display"
)

var kindMath = ast.NewNodeKind("Math")

// mathNode は数式1つ。Value は区切り記号を除いた TeX のソース。
type mathNode struct {
	ast.BaseInline
	Display bool
	Value   []byte
}

func (n *mathNode) Kind() ast.NodeKind { return kindMath }

func (n *mathNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Value": string(n.Value)}, nil)
}

// mathExtension は数式のパーサーとレンダラーを Goldmark に登録する。
type mathExtension struct{}

func (mathExtension) Extend(m goldmark.Markdown) {
	// コードスパン（優先度 100）より後、強調（200）より前に試す。
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(mathParser{}, 150)))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(mathRenderer{}, 500)))
}

type mathParser struct{}

func (mathParser) Trigger() []byte { return []byte{'$'} }

// Parse は $ から始まる数式を読む。対応する閉じ記号が無ければ nil を返し、$ は通常の文字として扱われる。
//
// インライン数式は1行の中で閉じること、開きの直後と閉じの直前が空白でないこと、閉じの直後が数字でないことを
// 条件とする（Pandoc と同じ規則）。これにより「500円〜$5 と $10」のような金額の $ を数式と誤認しない。
// ブロック数式は段落内で複数行にまたがってよい。
func (mathParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if bytes.HasPrefix(line, []byte("$$")) {
		return parseDisplayMath(block)
	}
	return parseInlineMath(block, line)
}

func parseInlineMath(block text.Reader, line []byte) ast.Node {
	if len(line) < 3 || isMathSpace(line[1]) {
		return nil
	}
	for i := 2; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++ // \$ などのエスケープを読み飛ばす。
		case '$':
			if isMathSpace(line[i-1]) || (i+1 < len(line) && line[i+1] >= '0' && line[i+1] <= '9') {
				continue
			}
			node := &mathNode{Value: append([]byte(nil), line[1:i]...)}
			block.Advance(i + 1)
			return node
		}
	}
	return nil
}

func parseDisplayMath(block text.Reader) ast.Node {
	l, pos := block.Position()
	block.Advance(2)
	var value []byte
	for {
		line, _ := block.PeekLine()
		if line == nil {
			block.SetPosition(l, pos)
			return nil
		}
		if i := bytes.Index(line, []byte("$$")); i >= 0 {
			value = append(value, line[:i]...)
			block.Advance(i + 2)
			break
		}
		value = append(value, line...)
		block.AdvanceLine()
	}
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		block.SetPosition(l, pos)
		return nil
	}
	return &mathNode{Display: true, Value: value}
}

func isMathSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

type mathRenderer struct{}

func (mathRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindMath, renderMath)
}

func renderMath(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	m := n.(*mathNode)
	open, closing, class := `\(`, `\)`, MathInlineClass
	if m.Display {
		open, closing, class = `\[`, `\]`, MathDisplayClass
	}
	_, _ = w.WriteString(`<span class="` + class + `">` + open)
	_, _ = w.Write(util.EscapeHTML(m.Value))
	_, _ = w.WriteString(closing + `</span>`)
	return ast.WalkSkipChildren, nil
}

// HasMath は Render が出力した HTML に数式が含まれるかを返す。KaTeX を読み込むかの判定に使う。
func HasMath(html string) bool {
	return strings.Contains(html, `class="`+MathInlineClass+`"`) ||
		strings.Contains(html, `class="`+MathDisplayClass+`"`)
}
