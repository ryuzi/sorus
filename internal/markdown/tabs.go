package markdown

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// OS・環境別のタブ切り替え。:::tabs と ::: で囲んだ範囲を、:::tab[名前] の行ごとのタブに分ける。
//
//	:::tabs
//	:::tab[macOS]
//	brew install sorus
//	:::tab[Windows]
//	winget install sorus
//	:::
//
// を次の形で出力する（JS が無くても、すべてのタブの中身が名前付きで縦に並んで読める）。
//
//	<div class="doc-tabs" data-tabs>
//	<div class="doc-tab-list" role="tablist" hidden><button type="button" class="doc-tab" role="tab" ...>macOS</button>...</div>
//	<div class="doc-tab-panel" role="tabpanel" id="doc-tabs-1--panel-1" aria-labelledby="doc-tabs-1--tab-1">
//	<p class="doc-tab-title">macOS</p>
//	...
//	</div>
//	...
//	</div>
//
// タブの並び（doc-tab-list）は hidden で出し、公開サイトのスクリプトが hidden を外して選んでいないタブの中身を隠す
// （そのときタブの名前の行 doc-tab-title も隠す）。外枠の開き・タブの並びと外枠の閉じは、描画のときは印だけを置き、
// サニタイズの後に expandTabs が HTML に置き換える（「もっと見る」のボタンと同じ）。button・data-tabs・role="tablist" を
// サニタイズで許可しないため、本文の生 HTML ではスクリプトが動かすタブを作れない。サニタイズを通すのは中身（tabpanel）だけ。
// 各タブの中身は通常の Markdown として解釈する（フォトギャラリー・コードブロック・入れ子のタブも書ける）。
// 閉じの ::: が無い :::tabs、:::tabs の次の行（空行は除く）が :::tab[名前] でないものは、通常の段落として扱う。

const (
	// TabsClass はタブ全体を囲む div のクラス。目印の属性 data-tabs と組み合わせて出す。
	TabsClass = "doc-tabs"
	// TabListClass はタブのボタンを並べる div（role="tablist"）のクラス。
	TabListClass = "doc-tab-list"
	// TabClass はタブ1つのボタン（role="tab"）のクラス。
	TabClass = "doc-tab"
	// TabPanelClass はタブ1つの中身を囲む div（role="tabpanel"）のクラス。
	TabPanelClass = "doc-tab-panel"
	// TabTitleClass は中身の先頭に置くタブの名前の p のクラス。JS が無いときの見出しで、スクリプトがタブにしたら隠す。
	TabTitleClass = "doc-tab-title"
	// tabIDPrefix はタブのボタン・中身の id の接頭辞。見出しの id（slugify）は「-」を続けないため、「--」を含む id とは重ならない。
	tabIDPrefix = "doc-tabs-"
)

var (
	kindTabs     = ast.NewNodeKind("Tabs")
	kindTabPanel = ast.NewNodeKind("TabPanel")
	// tabsOpenPattern はタブ全体の開きの行。tabOpenPattern はタブ1つの開きの行（名前は [ と最後の ] の間）。
	tabsOpenPattern = regexp.MustCompile(`^:::tabs$`)
	tabOpenPattern  = regexp.MustCompile(`^:::tab\[(.*\S.*)\]$`)
	// tabPanelClassPattern は中身の div の class 属性として許可する値。
	tabPanelClassPattern = regexp.MustCompile(`^` + TabPanelClass + `$`)
	// tabLabelledByPattern は中身の div の aria-labelledby 属性として許可する値（ボタンの id）。
	tabLabelledByPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(tabIDPrefix) + `[0-9]{1,6}--tab-[0-9]{1,6}$`)
	// tabsOpenMarkerPattern・tabsCloseMarkerPattern はタブ全体の開き・閉じの印（tabsOpenMarker・tabsCloseMarker）。
	tabsOpenMarkerPattern  = regexp.MustCompile(`\[\[sorus-tabs:([A-Za-z0-9]+):([0-9]{1,6})\]\]`)
	tabsCloseMarkerPattern = regexp.MustCompile(`\[\[sorus-tabs-end:([A-Za-z0-9]+)\]\]`)
)

// tabsClassNames はタブの生成物と、それを動かすスクリプトが目印に使うクラス（生 HTML の汎用クラスには使えない）。
var tabsClassNames = []string{TabsClass, TabListClass, TabClass, TabPanelClass, TabTitleClass}

// tabsNode はタブ全体。子は tabPanelNode だけ。
type tabsNode struct {
	ast.BaseBlock
}

func (n *tabsNode) Kind() ast.NodeKind { return kindTabs }

func (n *tabsNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// tabPanelNode はタブ1つ。子には通常の Markdown として解釈したブロックを持つ。
type tabPanelNode struct {
	ast.BaseBlock
	// Label はタブの名前（Markdown のソースのまま）。
	Label string
	// tabID・panelID はボタンと中身の id（描画するときに決める）。
	tabID, panelID string
}

func (n *tabPanelNode) Kind() ast.NodeKind { return kindTabPanel }

func (n *tabPanelNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Label": n.Label}, nil)
}

// tabsState は1回の描画のタブの情報。描画中に名前を集め、サニタイズの後にボタンを作るときに使う。
type tabsState struct {
	// token は1回の描画ごとの推測できない文字列（GalleryOptions.buttonToken と同じもの）。
	token string
	// groups はタブ全体ごとのタブの名前（出現順。印の番号は1から）。
	groups [][]string
}

// tabsExtension はタブのパーサーとレンダラーを Goldmark に登録する。
type tabsExtension struct{ state *tabsState }

func (e tabsExtension) Extend(m goldmark.Markdown) {
	// フォトギャラリーと同じく、フェンスコードブロック（優先度 700）より前に試す。
	m.Parser().AddOptions(parser.WithBlockParsers(
		util.Prioritized(tabsParser{}, 640),
		util.Prioritized(tabPanelParser{}, 641),
	))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(tabsRenderer{state: e.state}, 500)))
}

type tabsParser struct{}

func (tabsParser) Trigger() []byte { return []byte{':'} }

func (tabsParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || !tabsOpenPattern.Match(util.TrimRightSpace(line[pos:])) || !hasTabsBody(reader) {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &tabsNode{}, parser.HasChildren
}

// Continue は閉じの ::: でタブ全体を閉じる。中にまだ閉じていないフェンスコードブロック・フォトギャラリー・入れ子の
// タブがあれば、その行は中のブロックに任せる（中の ::: や、コードの中の ::: で閉じない）。
func (tabsParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	if hasOpenFenceBelow(node, pc) {
		return parser.Continue | parser.HasChildren
	}
	line, _ := reader.PeekLine()
	if isGalleryClose(line, reader.LineOffset()) {
		reader.AdvanceToEOL()
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

func (tabsParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (tabsParser) CanInterruptParagraph() bool { return true }

func (tabsParser) CanAcceptIndentedLine() bool { return false }

type tabPanelParser struct{}

func (tabPanelParser) Trigger() []byte { return []byte{':'} }

// Open はタブ全体の直下の :::tab[名前] の行でタブ1つを開く。タブ全体の外の :::tab[名前] は通常の段落になる。
func (tabPanelParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	if parent.Kind() != kindTabs {
		return nil, parser.NoChildren
	}
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 {
		return nil, parser.NoChildren
	}
	label, ok := tabLabel(util.TrimRightSpace(line[pos:]))
	if !ok {
		return nil, parser.NoChildren
	}
	reader.AdvanceToEOL()
	return &tabPanelNode{Label: label}, parser.HasChildren
}

// Continue は次のタブの開きの行で閉じる（その行はタブ全体の直下で次のタブを開く）。閉じの ::: はタブ全体が先に受け取る。
func (tabPanelParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	if hasOpenFenceBelow(node, pc) {
		return parser.Continue | parser.HasChildren
	}
	line, _ := reader.PeekLine()
	w, pos := util.IndentWidth(line, reader.LineOffset())
	if _, ok := tabLabel(util.TrimRightSpace(line[pos:])); w < 4 && ok {
		return parser.Close
	}
	return parser.Continue | parser.HasChildren
}

func (tabPanelParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}

func (tabPanelParser) CanInterruptParagraph() bool { return true }

func (tabPanelParser) CanAcceptIndentedLine() bool { return false }

// tabLabel は :::tab[名前] の行の名前を返す。名前が空白だけ・行の形が違うときは ok=false。
func tabLabel(line []byte) (string, bool) {
	m := tabOpenPattern.FindSubmatch(line)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(string(m[1])), true
}

// hasOpenFenceBelow は、開いているブロックのうち node より内側に、::: や :::tab[名前] の行を自分で読むブロック
// （フェンスコードブロック・フォトギャラリー・入れ子のタブ）があるかを返す。
func hasOpenFenceBelow(node ast.Node, pc parser.Context) bool {
	below := false
	for _, b := range pc.OpenedBlocks() {
		if !below {
			below = b.Node == node
			continue
		}
		switch b.Node.(type) {
		case *ast.FencedCodeBlock, *galleryNode, *tabsNode:
			return true
		}
	}
	return false
}

// hasTabsBody は開きの行より後に、タブ全体の閉じの ::: があり、その前の最初の行（空行は除く）が :::tab[名前] かを、
// 読み取り位置を変えずに調べる。中のフェンスコードブロックの行は読み飛ばし、中のフォトギャラリー・入れ子のタブの
// ::: はその分を数えて飛ばす。引用・リストの中のタブも調べられるよう、行頭の空白と「>」は除いて比べる。
func hasTabsBody(reader text.Reader) bool {
	l, pos := reader.Position()
	defer reader.SetPosition(l, pos)
	depth, first := 0, true
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
		if len(line) == 0 {
			continue
		}
		if first {
			if _, ok := tabLabel(line); !ok {
				return false
			}
			first = false
			continue
		}
		switch {
		case bytes.Equal(line, galleryCloseMarker):
			if depth == 0 {
				return true
			}
			depth--
		case tabsOpenPattern.Match(line) || isGalleryOpen(line):
			depth++
		default:
			fence = fenceStart(line)
		}
	}
}

// stripQuotePrefix は行頭の空白と引用の「>」、行末の空白を除いた行を返す。
func stripQuotePrefix(line []byte) []byte {
	for {
		line = bytes.TrimLeft(line, " \t")
		if len(line) == 0 || line[0] != '>' {
			return util.TrimRightSpace(line)
		}
		line = line[1:]
	}
}

// fenceStart は line がフェンスコードブロックの開き（``` か ~~~ を3つ以上）なら、その記号の並びを返す。違えば nil。
func fenceStart(line []byte) []byte {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return nil
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return nil
	}
	return line[:n]
}

// isFenceEnd は line が fence（開きの記号の並び）で開いたフェンスコードブロックの閉じかを返す。
func isFenceEnd(line, fence []byte) bool {
	return len(line) >= len(fence) && len(bytes.Trim(line, string(fence[:1]))) == 0
}

type tabsRenderer struct{ state *tabsState }

func (r tabsRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindTabs, r.renderTabs)
	reg.Register(kindTabPanel, renderTabPanel)
}

// renderTabs はタブ全体の開き（外枠とタブの並び）と閉じの印を出力する。中身のタブの id はここで決める。
// 印は1回の描画ごとの token を含み、本文に同じ文字を書かれても外枠やボタンにならない。
func (r tabsRenderer) renderTabs(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		_, _ = w.WriteString(tabsCloseMarker(r.state.token) + "\n")
		return ast.WalkContinue, nil
	}
	var labels []string
	group := len(r.state.groups) + 1
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		panel, ok := c.(*tabPanelNode)
		if !ok {
			continue
		}
		labels = append(labels, panel.Label)
		panel.tabID, panel.panelID = tabIDs(group, len(labels))
	}
	r.state.groups = append(r.state.groups, labels)
	_, _ = w.WriteString(tabsOpenMarker(r.state.token, group) + "\n")
	return ast.WalkContinue, nil
}

// renderTabPanel はタブ1つの中身を、名前の行を先頭にして出力する。
func renderTabPanel(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		_, _ = w.WriteString("</div>\n")
		return ast.WalkContinue, nil
	}
	p := n.(*tabPanelNode)
	_, _ = w.WriteString(`<div class="` + TabPanelClass + `" role="tabpanel"`)
	if p.panelID != "" {
		_, _ = w.WriteString(` id="` + p.panelID + `" aria-labelledby="` + p.tabID + `"`)
	}
	_, _ = w.WriteString(">\n")
	_, _ = w.WriteString(`<p class="` + TabTitleClass + `">` + plainLabelHTML(p.Label) + "</p>\n")
	return ast.WalkContinue, nil
}

// tabIDs は group 番目のタブ全体の index 番目（どちらも1から）のタブの、ボタンと中身の id を返す。
func tabIDs(group, index int) (tabID, panelID string) {
	base := tabIDPrefix + strconv.Itoa(group) + "--"
	return base + "tab-" + strconv.Itoa(index), base + "panel-" + strconv.Itoa(index)
}

// tabsOpenMarker は group 番目のタブ全体の開き（外枠とタブの並び）を置く場所の印。tabsCloseMarker は閉じの印。
// サニタイズの後に expandTabs が HTML に置き換える。
func tabsOpenMarker(token string, group int) string {
	return "[[sorus-tabs:" + token + ":" + strconv.Itoa(group) + "]]"
}

func tabsCloseMarker(token string) string { return "[[sorus-tabs-end:" + token + "]]" }

// expandTabs はサニタイズ済みの HTML のタブ全体の印（この描画の token のものだけ）を、外枠・タブの並び・閉じの HTML にする。
// ボタンは最初のタブを選んだ状態（aria-selected="true"、矢印キーで移るため他は tabindex="-1"）で出す。
func (s *tabsState) expandTabs(safe string) string {
	if s.token == "" || len(s.groups) == 0 || !strings.Contains(safe, "[[sorus-tabs") {
		return safe
	}
	safe = tabsOpenMarkerPattern.ReplaceAllStringFunc(safe, func(m string) string {
		sub := tabsOpenMarkerPattern.FindStringSubmatch(m)
		group, err := strconv.Atoi(sub[2])
		if sub[1] != s.token || err != nil || group < 1 || group > len(s.groups) {
			return m
		}
		var b strings.Builder
		b.WriteString(`<div class="` + TabsClass + `" data-tabs>` + "\n")
		b.WriteString(`<div class="` + TabListClass + `" role="tablist" hidden>`)
		for i, label := range s.groups[group-1] {
			tabID, panelID := tabIDs(group, i+1)
			selected, tabindex := "false", "-1"
			if i == 0 {
				selected, tabindex = "true", "0"
			}
			b.WriteString(`<button type="button" class="` + TabClass + `" role="tab" id="` + tabID + `" aria-controls="` + panelID +
				`" aria-selected="` + selected + `" tabindex="` + tabindex + `">` + plainLabelHTML(label) + `</button>`)
		}
		b.WriteString(`</div>`)
		return b.String()
	})
	return tabsCloseMarkerPattern.ReplaceAllStringFunc(safe, func(m string) string {
		if tabsCloseMarkerPattern.FindStringSubmatch(m)[1] != s.token {
			return m
		}
		return `</div>`
	})
}
