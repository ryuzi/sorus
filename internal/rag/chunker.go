package rag

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// チャンクの既定の目標サイズ（文字数 = rune 数）。
const (
	DefaultMinChunkChars = 300
	DefaultMaxChunkChars = 800
)

// HeadingSeparator は Chunk.Heading で見出しの階層をつなぐ区切り。
const HeadingSeparator = " > "

// Chunk は記事本文を見出し・段落単位で分割した1片。
type Chunk struct {
	// Index は記事内の通し番号（0 から）。
	Index int
	// Heading は直前の見出し（#〜###）までの階層を HeadingSeparator でつないだもの
	// （例: "研究紹介 > 量子計算の応用"）。最初の見出しより前のチャンクでは空文字。
	Heading string
	// Text はチャンクの本文。見出しの行は含まない。
	Text string
}

// EmbeddingText は埋め込みの生成に使うテキスト。見出しの文脈を先頭に付けて意味を補う
// （例: "[見出し: 量子計算の応用] 本研究では…"）。
func (c Chunk) EmbeddingText() string {
	if c.Heading == "" {
		return c.Text
	}
	return "[見出し: " + c.Heading + "] " + c.Text
}

// Chunker は Markdown を見出し・段落単位でチャンクに分割する。ゼロ値は既定のサイズで分割する。
//
//   - 見出し（#〜###）の位置では必ずチャンクを区切る。#### 以下の見出しは本文として扱う。
//   - 見出しの中の文章は段落（空行区切り）単位で MaxChars を超えない範囲にまとめる。
//     MaxChars を超える段落は行、文（。！？ など）の境界で、それでも長ければ MaxChars 文字ごとに分ける。
//   - 節の最後のチャンクが MinChars に満たなければ、直前のチャンクから段落を移して大きさを均す。
//   - コードブロック（``` / ~~~）の中の # や空行では区切らない。
type Chunker struct {
	// MinChars はチャンクの目標の最小文字数。0 以下なら DefaultMinChunkChars。
	MinChars int
	// MaxChars はチャンクの最大文字数（見出しの文脈は含まない）。0 以下なら DefaultMaxChunkChars。
	MaxChars int
}

// ChunkMarkdown は既定のサイズで markdown をチャンクに分割する。
func ChunkMarkdown(markdown string) []Chunk {
	return Chunker{}.Split(markdown)
}

var (
	// atxHeadingRe は #〜### の ATX 見出しの行に一致する。# の後に空白が無い「#タグ」は見出しではない。
	atxHeadingRe = regexp.MustCompile(`^ {0,3}(#{1,3})(?:[ \t]+(.*?))?[ \t]*$`)
	// closingHashesRe は見出しの末尾の閉じの # に一致する。
	closingHashesRe = regexp.MustCompile(`(?:^|[ \t]+)#+$`)
	// fenceRe はコードブロックの開始・終了の行に一致する。
	fenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
)

// Split は markdown をチャンクに分割する。本文の無い Markdown なら nil を返す。
func (c Chunker) Split(markdown string) []Chunk {
	minChars, maxChars := c.MinChars, c.MaxChars
	if maxChars <= 0 {
		maxChars = DefaultMaxChunkChars
	}
	if minChars <= 0 {
		minChars = DefaultMinChunkChars
	}
	minChars = min(minChars, maxChars)

	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")

	var (
		chunks    []Chunk
		headings  [3]string // #, ##, ### の直近の見出し
		paragraph []string  // 組み立て中の段落の行
		units     []unit    // 現在の節の段落（MaxChars 以下に分けたもの）
		fence     string    // 開いているコードブロックの記号（``` や ~~~）
	)
	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		text := strings.Join(paragraph, "\n")
		paragraph = nil
		if strings.TrimSpace(text) == "" {
			return
		}
		for i, piece := range splitLong(text, maxChars) {
			units = append(units, unit{text: piece, n: utf8.RuneCountInString(piece), cont: i > 0})
		}
	}
	flushSection := func() {
		flushParagraph()
		heading := joinHeadings(headings[:])
		for _, group := range packUnits(units, minChars, maxChars) {
			if text := joinUnits(group); text != "" {
				chunks = append(chunks, Chunk{Index: len(chunks), Heading: heading, Text: text})
			}
		}
		units = nil
	}

	for _, line := range strings.Split(markdown, "\n") {
		if fence != "" {
			paragraph = append(paragraph, line)
			if closesFence(line, fence) {
				fence = ""
				flushParagraph()
			}
			continue
		}
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			// コードブロックは前後の文章と別の段落にする。
			flushParagraph()
			paragraph = append(paragraph, line)
			fence = m[1]
			continue
		}
		if m := atxHeadingRe.FindStringSubmatch(line); m != nil {
			flushSection()
			level := len(m[1])
			headings[level-1] = strings.TrimSpace(closingHashesRe.ReplaceAllString(m[2], ""))
			for i := level; i < len(headings); i++ {
				headings[i] = ""
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			flushParagraph()
			continue
		}
		paragraph = append(paragraph, line)
	}
	flushSection()
	return chunks
}

// closesFence は line が fence で開いたコードブロックを閉じる行かを返す。
// 閉じの記号は開始と同じ文字で、同じ数以上並び、後ろに空白しか無いこと。
func closesFence(line, fence string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	n := len(t) - len(strings.TrimLeft(t, fence[:1]))
	return n >= len(fence) && strings.TrimSpace(t[n:]) == ""
}

// joinHeadings は空でない見出しを HeadingSeparator でつなぐ。
func joinHeadings(headings []string) string {
	var parts []string
	for _, h := range headings {
		if h != "" {
			parts = append(parts, h)
		}
	}
	return strings.Join(parts, HeadingSeparator)
}

// unit はチャンクを組み立てる単位（段落、または長い段落を分けた一部）。
type unit struct {
	text string
	n    int // text の文字数
	// cont は直前の unit と同じ段落の続きであることを表す。続きは区切りの空行を挟まずにつなぐ。
	cont bool
}

// paragraphSep はチャンク内で段落をつなぐ区切り。
const paragraphSep = "\n\n"

// sepLen は u を直前の unit の後ろにつなぐときの区切りの文字数。
func (u unit) sepLen() int {
	if u.cont {
		return 0
	}
	return len(paragraphSep)
}

// groupLen は group をつないだテキストの文字数。
func groupLen(group []unit) int {
	n := 0
	for i, u := range group {
		if i > 0 {
			n += u.sepLen()
		}
		n += u.n
	}
	return n
}

// joinUnits は group を段落の区切りでつなぎ、前後の空白を除く。
func joinUnits(group []unit) string {
	var b strings.Builder
	for i, u := range group {
		if i > 0 && !u.cont {
			b.WriteString(paragraphSep)
		}
		b.WriteString(u.text)
	}
	return strings.TrimSpace(b.String())
}

// packUnits は units を順に maxChars を超えない範囲でまとめる。
// 最後のまとまりが minChars に満たなければ、直前のまとまりが minChars を下回らない範囲で末尾の unit を移す。
func packUnits(units []unit, minChars, maxChars int) [][]unit {
	var groups [][]unit
	var cur []unit
	curLen := 0
	for _, u := range units {
		if len(cur) > 0 && curLen+u.sepLen()+u.n > maxChars {
			groups = append(groups, cur)
			cur, curLen = nil, 0
		}
		if len(cur) > 0 {
			curLen += u.sepLen()
		}
		cur = append(cur, u)
		curLen += u.n
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}

	for len(groups) >= 2 {
		prev, last := groups[len(groups)-2], groups[len(groups)-1]
		if groupLen(last) >= minChars || len(prev) < 2 {
			break
		}
		rest := prev[:len(prev)-1]
		moved := append([]unit{prev[len(prev)-1]}, last...)
		if groupLen(rest) < minChars || groupLen(moved) > maxChars {
			break
		}
		groups[len(groups)-2], groups[len(groups)-1] = rest, moved
	}
	return groups
}

// splitLong は text を maxChars 文字以下の断片に分ける。断片をつなぐと text に戻る。
// 行の境界、文の境界、maxChars 文字ごとの順に細かくし、収まる範囲で前から詰める。
func splitLong(text string, maxChars int) []string {
	if utf8.RuneCountInString(text) <= maxChars {
		return []string{text}
	}
	var segments []string
	for _, line := range strings.SplitAfter(text, "\n") {
		if utf8.RuneCountInString(line) <= maxChars {
			segments = append(segments, line)
			continue
		}
		for _, sentence := range splitSentences(line) {
			segments = append(segments, hardSplit(sentence, maxChars)...)
		}
	}

	var pieces []string
	var cur strings.Builder
	curLen := 0
	for _, s := range segments {
		n := utf8.RuneCountInString(s)
		if curLen > 0 && curLen+n > maxChars {
			pieces = append(pieces, cur.String())
			cur.Reset()
			curLen = 0
		}
		cur.WriteString(s)
		curLen += n
	}
	if curLen > 0 {
		pieces = append(pieces, cur.String())
	}
	return pieces
}

// sentenceEnds は文の終わりとみなす文字。半角のピリオドは "3.14" などを分けないよう、後ろが空白のときだけ区切る。
const sentenceEnds = "。！？!?．"

// sentenceClosers は文末の記号の直後に続けて前の文に含める閉じ括弧・引用符。
const sentenceClosers = "」』）)]】〕\"'”’"

// splitSentences は s を文の境界の直後で分ける。文末の閉じ括弧と空白は前の文に含める。断片をつなぐと s に戻る。
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		isEnd := strings.ContainsRune(sentenceEnds, r)
		if r == '.' {
			next, _ := utf8.DecodeRuneInString(s[i:])
			isEnd = i == len(s) || unicode.IsSpace(next)
		}
		if !isEnd {
			continue
		}
		for i < len(s) {
			next, nsize := utf8.DecodeRuneInString(s[i:])
			if !strings.ContainsRune(sentenceClosers, next) && !unicode.IsSpace(next) {
				break
			}
			i += nsize
		}
		out = append(out, s[start:i])
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// hardSplit は s を maxChars 文字ごとに分ける。
func hardSplit(s string, maxChars int) []string {
	var out []string
	for utf8.RuneCountInString(s) > maxChars {
		i, n := 0, 0
		for n < maxChars {
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
			n++
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
