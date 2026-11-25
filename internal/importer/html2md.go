package importer

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTMLToMarkdown は WordPress の本文 HTML を GFM Markdown に変換する。
// Gutenberg のブロックコメント（<!-- wp:... -->）は除去し、主要なショートコードは
// HTML に展開してから変換する。クラシックエディタの改行だけの段落（wpautop 前の本文）も段落として扱う。
// script・style などの実行・装飾要素は捨てる。
func HTMLToMarkdown(src string) string {
	src = expandShortcodes(src)
	src = autop(src)
	nodes, err := html.ParseFragment(strings.NewReader(src), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		// x/net/html は不正な HTML も復元して解析するため、通常ここには来ない。
		return strings.TrimSpace(src) + "\n"
	}
	root := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	var c converter
	md := strings.Join(c.blocks(root), "\n\n")
	md = strings.TrimSpace(md)
	if md == "" {
		return ""
	}
	return md + "\n"
}

var (
	// shortcodeCaption は [caption ...]<img ...> 説明文[/caption]。
	shortcodeCaption = regexp.MustCompile(`(?s)\[caption[^\]]*\](.*?)\[/caption\]`)
	// shortcodeEmbed は [embed]URL[/embed]。
	shortcodeEmbed = regexp.MustCompile(`(?s)\[embed[^\]]*\]\s*(.*?)\s*\[/embed\]`)
	// shortcodeMediaSrc は [video src="..."] / [audio mp3="..."] などのメディア URL を持つショートコード。
	shortcodeMediaSrc = regexp.MustCompile(`\[(?:video|audio)\b[^\]]*?\b(?:src|mp4|webm|ogv|mp3|m4a|ogg|wav)="([^"]+)"[^\]]*\]`)
	// shortcodeTag は既知のショートコードの開始・終了タグ。中身は残してタグだけ除去する。
	shortcodeTag = regexp.MustCompile(`\[/?(?:caption|wp_caption|embed|gallery|video|audio|playlist|contact-form-7|contact-form|su_[a-z_]+|vc_[a-z_]+|et_pb_[a-z_]+)\b[^\]]*\]`)
	// imgTagPrefix は caption の中身を <img> とそれ以降（説明文）に分ける。
	imgTagPrefix = regexp.MustCompile(`(?is)^\s*((?:<a\b[^>]*>\s*)?<img\b[^>]*>(?:\s*</a>)?)(.*)$`)
	// blockTagStart は autop で段落化しない、ブロック要素で始まる塊。
	blockTagStart = regexp.MustCompile(`(?i)^<(?:p|div|h[1-6]|ul|ol|li|blockquote|pre|table|thead|tbody|tr|td|th|figure|figcaption|hr|section|article|aside|header|footer|iframe|dl|dt|dd|form|address|!--)\b`)
)

// expandShortcodes は WordPress のショートコードを等価な HTML に置き換え、残りの既知ショートコードを除去する。
func expandShortcodes(s string) string {
	s = shortcodeCaption.ReplaceAllStringFunc(s, func(m string) string {
		inner := shortcodeCaption.FindStringSubmatch(m)[1]
		if p := imgTagPrefix.FindStringSubmatch(inner); p != nil {
			caption := strings.TrimSpace(p[2])
			if caption == "" {
				return "<figure>" + p[1] + "</figure>"
			}
			return "<figure>" + p[1] + "<figcaption>" + caption + "</figcaption></figure>"
		}
		return inner
	})
	s = shortcodeEmbed.ReplaceAllStringFunc(s, func(m string) string {
		u := strings.TrimSpace(shortcodeEmbed.FindStringSubmatch(m)[1])
		return "\n\n<p>" + linkHTML(u) + "</p>\n\n"
	})
	s = shortcodeMediaSrc.ReplaceAllStringFunc(s, func(m string) string {
		return "<p>" + linkHTML(shortcodeMediaSrc.FindStringSubmatch(m)[1]) + "</p>"
	})
	return shortcodeTag.ReplaceAllString(s, "")
}

func linkHTML(u string) string {
	e := html.EscapeString(u)
	return `<a href="` + e + `">` + e + `</a>`
}

// autop はクラシックエディタの本文（<p> を含まない）で、空行区切りを段落、単独の改行を <br> にする。
// Gutenberg の本文や <p> を含む本文はそのまま返す。<pre> の中身は対象外。
func autop(s string) string {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "<p>") || strings.Contains(lower, "<p ") || strings.Contains(s, "<!-- wp:") || strings.Contains(lower, "<pre") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	chunks := regexp.MustCompile(`\n\s*\n`).Split(s, -1)
	var b strings.Builder
	for _, c := range chunks {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if blockTagStart.MatchString(c) {
			b.WriteString(c)
		} else {
			b.WriteString("<p>" + strings.ReplaceAll(c, "\n", "<br>\n") + "</p>")
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// converter は DOM を Markdown のブロック列に変換する。
type converter struct{}

// blocks は n の子をブロック単位の Markdown に変換する。連続するインライン要素は1段落にまとめる。
func (c *converter) blocks(n *html.Node) []string {
	var out []string
	var inline strings.Builder
	flush := func() {
		if p := finishParagraph(inline.String()); p != "" {
			out = append(out, p)
		}
		inline.Reset()
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode && isBlock(ch) {
			flush()
			out = append(out, c.block(ch)...)
			continue
		}
		inline.WriteString(c.inline(ch))
	}
	flush()
	return out
}

// block はブロック要素 n を Markdown のブロック列に変換する。
func (c *converter) block(n *html.Node) []string {
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		text := strings.Join(strings.Fields(strings.ReplaceAll(c.inlineChildren(n), "\\\n", " ")), " ")
		if text == "" {
			return nil
		}
		return []string{strings.Repeat("#", level) + " " + text}
	case atom.P:
		if p := finishParagraph(c.inlineChildren(n)); p != "" {
			return []string{p}
		}
		return nil
	case atom.Hr:
		return []string{"---"}
	case atom.Pre:
		return []string{c.codeBlock(n)}
	case atom.Blockquote:
		inner := strings.Join(c.blocks(n), "\n\n")
		if inner == "" {
			return nil
		}
		return []string{prefixLines(inner, "> ", ">")}
	case atom.Ul, atom.Ol:
		if l := c.list(n); l != "" {
			return []string{l}
		}
		return nil
	case atom.Table:
		if t := c.table(n); t != "" {
			return []string{t}
		}
		return nil
	case atom.Figcaption:
		text := strings.TrimSpace(c.inlineChildren(n))
		if text == "" {
			return nil
		}
		return []string{"*" + text + "*"}
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Head, atom.Title, atom.Form, atom.Select, atom.Button:
		return nil
	case atom.Iframe:
		src := strings.TrimSpace(attr(n, "src"))
		if !isSafeURL(src) {
			return nil
		}
		return []string{"[" + escapeText(src) + "](" + mdURL(src) + ")"}
	}
	// div, figure, section, li 以外のコンテナなどは中身のブロックをそのまま並べる。
	return c.blocks(n)
}

// inline はインライン要素・テキストを Markdown に変換する。改行（<br>）は "\\\n" で表す。
func (c *converter) inline(n *html.Node) string {
	switch n.Type {
	case html.TextNode:
		t := escapeText(collapseSpace(n.Data))
		if strings.HasSuffix(t, "!") {
			t = t[:len(t)-1] + `\!` // 直後のリンクが画像構文 ![...](...) と解釈されないようにする
		}
		return t
	case html.ElementNode:
	default:
		return "" // コメント（Gutenberg のブロック区切りを含む）は捨てる。
	}
	switch n.DataAtom {
	case atom.Br:
		return "\\\n"
	case atom.Strong, atom.B:
		return wrapInline(c.inlineChildren(n), "**")
	case atom.Em, atom.I, atom.Cite:
		return wrapInline(c.inlineChildren(n), "*")
	case atom.Del, atom.S, atom.Strike:
		return wrapInline(c.inlineChildren(n), "~~")
	case atom.Code, atom.Kbd, atom.Samp, atom.Tt:
		return inlineCode(textContent(n))
	case atom.A:
		return c.link(n)
	case atom.Img:
		return imageMarkdown(n)
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Button, atom.Select:
		return ""
	}
	if isBlock(n) {
		// インライン要素の中にブロック要素がある不正な入れ子は、空白区切りの文字列にする。
		return " " + strings.Join(c.block(n), " ") + " "
	}
	return c.inlineChildren(n)
}

func (c *converter) inlineChildren(n *html.Node) string {
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		b.WriteString(c.inline(ch))
	}
	return b.String()
}

func (c *converter) link(n *html.Node) string {
	text := strings.TrimSpace(c.inlineChildren(n))
	href := strings.TrimSpace(attr(n, "href"))
	if href == "" || !isSafeURL(href) {
		return text
	}
	if text == "" {
		text = escapeText(href)
	}
	if title := strings.TrimSpace(attr(n, "title")); title != "" {
		return "[" + text + "](" + mdURL(href) + ` "` + strings.ReplaceAll(title, `"`, `\"`) + `")`
	}
	return "[" + text + "](" + mdURL(href) + ")"
}

func imageMarkdown(n *html.Node) string {
	src := strings.TrimSpace(attr(n, "src"))
	if src == "" {
		src = strings.TrimSpace(attr(n, "data-src")) // 遅延読み込みプラグインの本来の画像
	}
	if src == "" || !isSafeURL(src) {
		return ""
	}
	alt := markdownAltReplacer.Replace(collapseSpace(attr(n, "alt")))
	return "![" + strings.TrimSpace(alt) + "](" + mdURL(src) + ")"
}

// markdownAltReplacer は代替テキストから Markdown の画像構文を壊す文字を除く。
var markdownAltReplacer = strings.NewReplacer("[", "", "]", "", "\\", "", "\r", "", "\n", " ")

func (c *converter) codeBlock(n *html.Node) string {
	lang := codeLanguage(n)
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode && ch.DataAtom == atom.Code && lang == "" {
			lang = codeLanguage(ch)
		}
	}
	code := strings.TrimRight(strings.TrimLeft(textContent(n), "\n"), " \t\n")
	fence := "```"
	for strings.Contains(code, fence) {
		fence += "`"
	}
	return fence + lang + "\n" + code + "\n" + fence
}

// codeLanguage は class の language-xxx / lang-xxx からコードの言語名を取り出す。
func codeLanguage(n *html.Node) string {
	for _, cls := range strings.Fields(attr(n, "class")) {
		for _, p := range []string{"language-", "lang-"} {
			if l, ok := strings.CutPrefix(cls, p); ok && regexp.MustCompile(`^[\w+#.-]+$`).MatchString(l) {
				return l
			}
		}
	}
	return ""
}

// list は ul / ol を Markdown のリストに変換する。入れ子のリストはマーカーの幅だけ字下げする。
func (c *converter) list(n *html.Node) string {
	ordered := n.DataAtom == atom.Ol
	num := 1
	if s, err := strconv.Atoi(attr(n, "start")); err == nil && ordered {
		num = s
	}
	var items []string
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode {
			continue
		}
		var body []string
		if li.DataAtom == atom.Li {
			body = c.blocks(li)
		} else {
			body = c.block(li) // li 以外の直下要素（不正な入れ子のリストなど）
		}
		marker := "- "
		if ordered {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		text := joinListItemBlocks(body)
		pad := strings.Repeat(" ", len(marker))
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			switch {
			case i == 0:
				lines[i] = marker + l
			case l == "":
			default:
				lines[i] = pad + l
			}
		}
		items = append(items, strings.TrimRight(strings.Join(lines, "\n"), " "))
	}
	return strings.Join(items, "\n")
}

// table は table を GFM のテーブルに変換する。先頭行を見出し行とし、列数は最大の行に揃える。
func (c *converter) table(n *html.Node) string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode {
				continue
			}
			switch ch.DataAtom {
			case atom.Tr:
				var row []string
				for cell := ch.FirstChild; cell != nil; cell = cell.NextSibling {
					if cell.Type == html.ElementNode && (cell.DataAtom == atom.Td || cell.DataAtom == atom.Th) {
						row = append(row, c.tableCell(cell))
					}
				}
				rows = append(rows, row)
			case atom.Thead, atom.Tbody, atom.Tfoot:
				walk(ch)
			}
		}
	}
	walk(n)
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if cols == 0 {
		return ""
	}
	line := func(cells []string) string {
		for len(cells) < cols {
			cells = append(cells, "")
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	out := []string{line(rows[0]), line(slicesRepeat("---", cols))}
	for _, r := range rows[1:] {
		out = append(out, line(r))
	}
	return strings.Join(out, "\n")
}

// tableCell はセルの中身を1行のインライン Markdown にする。改行は <br> で表す。
func (c *converter) tableCell(n *html.Node) string {
	parts := c.blocks(n)
	s := strings.Join(parts, "<br>")
	s = strings.ReplaceAll(s, "\\\n", "<br>")
	s = strings.ReplaceAll(s, "\n", " ")
	// テキストの | はエスケープ済みなので、インラインコード内などの未エスケープの | だけを対象にする。
	var b strings.Builder
	escaped := false
	for _, r := range s {
		if r == '|' && !escaped {
			b.WriteByte('\\')
		}
		escaped = r == '\\' && !escaped
		b.WriteRune(r)
	}
	return b.String()
}

// listMarker はリスト記法で始まるブロック（入れ子のリスト）。本文の行頭記号はエスケープ済みなので誤認しない。
var listMarker = regexp.MustCompile(`^(?:- |\d+\. )`)

// joinListItemBlocks は li の中のブロックをつなぐ。入れ子のリストは空行を挟まず、
// 段落どうしは空行で区切る。
func joinListItemBlocks(blocks []string) string {
	var b strings.Builder
	for i, blk := range blocks {
		if i > 0 {
			if listMarker.MatchString(blk) {
				b.WriteString("\n")
			} else {
				b.WriteString("\n\n")
			}
		}
		b.WriteString(blk)
	}
	return b.String()
}

// prefixLines は各行に prefix を付ける。空行には empty を付ける（引用の段落区切り）。
func prefixLines(s, prefix, empty string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = empty
		} else {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

func slicesRepeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// isBlock は Markdown のブロックとして扱う要素かを判定する。
func isBlock(n *html.Node) bool {
	switch n.DataAtom {
	case atom.P, atom.Div, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Ul, atom.Ol, atom.Li, atom.Blockquote, atom.Pre, atom.Table, atom.Hr,
		atom.Figure, atom.Figcaption, atom.Section, atom.Article, atom.Aside, atom.Header,
		atom.Footer, atom.Nav, atom.Main, atom.Dl, atom.Dt, atom.Dd, atom.Address, atom.Iframe,
		atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Form, atom.Details, atom.Summary:
		return true
	}
	return false
}

// finishParagraph はインライン Markdown を段落として整える。
// 行頭・行末の空白と改行を除き、行頭がブロック記法と誤認される文字をエスケープする。
func finishParagraph(s string) string {
	lines := strings.Split(s, "\n")
	var kept []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || l == "\\" {
			continue
		}
		kept = append(kept, escapeLineStart(l))
	}
	if len(kept) == 0 {
		return ""
	}
	// 段落末尾の改行（<br>）は意味を持たないので除く。
	last := strings.TrimSuffix(kept[len(kept)-1], "\\")
	kept[len(kept)-1] = strings.TrimSpace(last)
	return strings.Join(kept, "\n")
}

var lineStartMarker = regexp.MustCompile(`^([#>+-]|=+$|\d+[.)])`)

// escapeLineStart は行頭の見出し・引用・リスト記法をバックスラッシュでエスケープする。
// インラインの Markdown 記法は # > + - = や数字で始まらないため、変換結果を壊さない。
func escapeLineStart(l string) string {
	m := lineStartMarker.FindStringIndex(l)
	if m == nil {
		return l
	}
	if l[0] >= '0' && l[0] <= '9' {
		end := m[1] - 1 // "." か ")" の直前にエスケープを入れる
		return l[:end] + "\\" + l[end:]
	}
	return "\\" + l
}

// escapeText はテキストノードの Markdown の特殊文字をエスケープする。
var textEscaper = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "]", `\]`, "<", `\<`, "~", `\~`, "|", `\|`,
)

// entityLike は文字参照として解釈されてしまう & の並び。
var entityLike = regexp.MustCompile(`&(#?[0-9A-Za-z]+;)`)

func escapeText(s string) string {
	return entityLike.ReplaceAllString(textEscaper.Replace(s), `\&$1`)
}

// collapseSpace は HTML と同じく空白文字の連続を1つの空白にする。
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// wrapInline は強調記号で囲む。記号の内側に空白があると強調にならないため、空白は外へ出す。
func wrapInline(s, mark string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}
	lead := s[:strings.Index(s, trimmed)]
	trail := s[len(lead)+len(trimmed):]
	return lead + mark + trimmed + mark + trail
}

// inlineCode はバッククォートを含むコードも壊れないよう、より長いバッククォートで囲む。
func inlineCode(s string) string {
	s = collapseSpace(s)
	if strings.TrimSpace(s) == "" {
		return ""
	}
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// mdURL はリンク先を Markdown のリンク先として安全に書けるようにする。
// 空白や括弧を含む URL は <> で囲む。
func mdURL(u string) string {
	if strings.ContainsAny(u, " ()<>") {
		return "<" + strings.NewReplacer("<", "%3C", ">", "%3E").Replace(u) + ">"
	}
	return u
}

// isSafeURL は javascript: などの危険なスキームでないことを確認する。相対 URL は許可する。
func isSafeURL(u string) bool {
	lower := strings.ToLower(strings.TrimSpace(u))
	if i := strings.IndexAny(lower, ":/?#"); i >= 0 && lower[i] == ':' {
		scheme := lower[:i]
		return scheme == "http" || scheme == "https" || scheme == "mailto" || scheme == "tel"
	}
	return true
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.ElementNode && n.DataAtom == atom.Br {
		return "\n"
	}
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		b.WriteString(textContent(ch))
	}
	return b.String()
}
