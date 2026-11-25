package markdown

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark/ast"
	"golang.org/x/text/unicode/norm"
)

// Heading は目次 (TOC) の 1 項目。
type Heading struct {
	Level int    `json:"level"` // 2 (H2), 3 (H3), etc.
	Text  string `json:"text"`
	ID    string `json:"id"`
}

// annotateHeadings は文書中のすべての見出しに id 属性を付与し、出現順の目次を返す。
// ID は見出しのプレーンテキストから生成し、文書内で一意になるよう連番を付ける。
func annotateHeadings(doc ast.Node, src []byte) []Heading {
	headings := []Heading{}
	used := map[string]bool{}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		txt := strings.TrimSpace(plainText(h, src))
		id := uniqueID(slugify(txt), used)
		h.SetAttributeString("id", []byte(id))
		headings = append(headings, Heading{Level: h.Level, Text: txt, ID: id})
		return ast.WalkSkipChildren, nil
	})
	return headings
}

// plainText はインライン要素の装飾を除いた表示テキストを返す。生 HTML は含めない。
func plainText(n ast.Node, src []byte) string {
	var sb strings.Builder
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			switch v := c.(type) {
			case *ast.Text:
				sb.Write(v.Value(src))
				if v.SoftLineBreak() || v.HardLineBreak() {
					sb.WriteByte(' ')
				}
			case *ast.String:
				sb.Write(v.Value)
			case *ast.AutoLink:
				sb.Write(v.Label(src))
			case *ast.RawHTML:
				// タグは目次テキストに含めない
			default:
				walk(c)
			}
		}
	}
	walk(n)
	return sb.String()
}

// slugify は見出しテキストをアンカー ID に変換する。
// 日本語などの Unicode 文字・数字は残し、空白は "-" に、その他の記号は除去する。
// NFD の結合文字（濁点など）が記号として落ちないよう、先に NFC へ正規化する。
func slugify(s string) string {
	var sb strings.Builder
	pendingDash := false
	for _, r := range strings.ToLower(norm.NFC.String(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_':
			if pendingDash && sb.Len() > 0 {
				sb.WriteByte('-')
			}
			pendingDash = false
			sb.WriteRune(r)
		case unicode.IsSpace(r) || r == '-':
			pendingDash = true
		}
	}
	if sb.Len() == 0 {
		return "heading"
	}
	return sb.String()
}

func uniqueID(base string, used map[string]bool) string {
	id := base
	for i := 1; used[id]; i++ {
		id = base + "-" + strconv.Itoa(i)
	}
	used[id] = true
	return id
}
