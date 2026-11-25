package rag

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// para は n 文字の段落（「あ」の繰り返し）を返す。
func para(r rune, n int) string { return strings.Repeat(string(r), n) }

func headingsAndTexts(chunks []Chunk) []string {
	var out []string
	for _, c := range chunks {
		out = append(out, c.Heading+"|"+c.Text)
	}
	return out
}

func assertChunks(t *testing.T, got []Chunk, want []string) {
	t.Helper()
	g := headingsAndTexts(got)
	if strings.Join(g, "\n---\n") != strings.Join(want, "\n---\n") {
		t.Errorf("chunks =\n%q\nwant\n%q", g, want)
	}
	for i, c := range got {
		if c.Index != i {
			t.Errorf("chunk %d has Index %d", i, c.Index)
		}
	}
}

func TestChunkMarkdownSplitsAtHeadings(t *testing.T) {
	md := `前書きの段落です。

# 研究紹介

研究室の概要です。

## 量子計算の応用

本研究では量子計算を扱います。

### 実験方法

実験の手順です。

## 今後の展望

展望です。

# 活動報告

報告です。
`
	assertChunks(t, ChunkMarkdown(md), []string{
		"|前書きの段落です。",
		"研究紹介|研究室の概要です。",
		"研究紹介 > 量子計算の応用|本研究では量子計算を扱います。",
		"研究紹介 > 量子計算の応用 > 実験方法|実験の手順です。",
		// 上位の見出しが変わると、下位の見出しの文脈はリセットされる。
		"研究紹介 > 今後の展望|展望です。",
		"活動報告|報告です。",
	})
}

func TestChunkMarkdownHeadingSyntax(t *testing.T) {
	md := `## 閉じの付いた見出し ##

本文A

#### 小見出しは本文の一部

本文B

#ハッシュタグは見出しではない

## C# と F#

本文C

    # 字下げされたコードは見出しではない
`
	assertChunks(t, ChunkMarkdown(md), []string{
		"閉じの付いた見出し|本文A\n\n#### 小見出しは本文の一部\n\n本文B\n\n#ハッシュタグは見出しではない",
		"C# と F#|本文C\n\n    # 字下げされたコードは見出しではない",
	})
}

func TestChunkMarkdownHeadingWithoutBody(t *testing.T) {
	// 本文の無い見出しはチャンクにならないが、下位の見出しの文脈には残る。
	md := "# 第1章\n## 概要\n\n本文です。\n\n# \n\n見出しの無い節"
	assertChunks(t, ChunkMarkdown(md), []string{
		"第1章 > 概要|本文です。",
		"|見出しの無い節",
	})
}

func TestChunkMarkdownKeepsCodeBlocksIntact(t *testing.T) {
	md := "## 使い方\n\n手順です。\n```bash\n# コメントは見出しではない\n\necho hello\n```\n\n~~~~\n```\n## これもコード\n~~~~\n続き"
	assertChunks(t, ChunkMarkdown(md), []string{
		"使い方|手順です。\n\n```bash\n# コメントは見出しではない\n\necho hello\n```\n\n~~~~\n```\n## これもコード\n~~~~\n\n続き",
	})
}

func TestChunkMarkdownEmpty(t *testing.T) {
	for _, md := range []string{"", "  \n\n\t\n", "# 見出しだけ\n## 小見出し\n"} {
		if got := ChunkMarkdown(md); got != nil {
			t.Errorf("ChunkMarkdown(%q) = %+v, want nil", md, got)
		}
	}
}

func TestChunkMarkdownNormalizesNewlines(t *testing.T) {
	md := "# 見出し\r\n\r\n段落1\r\n\r\n段落2\r段落2の続き"
	assertChunks(t, ChunkMarkdown(md), []string{"見出し|段落1\n\n段落2\n段落2の続き"})
}

func TestChunkerPacksParagraphsUpToMax(t *testing.T) {
	// 300 文字の段落 5 つ: 300+2+300 = 602 <= 800 なので2段落ずつまとまる。
	var paras []string
	for _, r := range "あいうえお" {
		paras = append(paras, para(r, 300))
	}
	md := "# 節\n\n" + strings.Join(paras, "\n\n")
	got := ChunkMarkdown(md)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3: %q", len(got), headingsAndTexts(got))
	}
	// 最後の 300 文字はちょうど MinChars なので均さない。
	wants := []string{paras[0] + "\n\n" + paras[1], paras[2] + "\n\n" + paras[3], paras[4]}
	for i, want := range wants {
		if got[i].Text != want || got[i].Heading != "節" {
			t.Errorf("chunk %d = %q (%d chars), want %d chars", i, got[i].Heading, utf8.RuneCountInString(got[i].Text), utf8.RuneCountInString(want))
		}
	}
}

func TestChunkerRebalancesShortTail(t *testing.T) {
	md := strings.Join([]string{para('あ', 250), para('い', 250), para('う', 250), para('え', 100)}, "\n\n")
	got := ChunkMarkdown(md)
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2: %q", len(got), headingsAndTexts(got))
	}
	// 詰めるだけだと [754, 100] だが、末尾が 300 未満なので1段落移して [502, 352] にする。
	if n := utf8.RuneCountInString(got[0].Text); n != 502 {
		t.Errorf("first chunk has %d chars, want 502", n)
	}
	if want := para('う', 250) + "\n\n" + para('え', 100); got[1].Text != want {
		t.Errorf("second chunk = %q, want the last two paragraphs", got[1].Text)
	}

	// 移すと直前のチャンクが MinChars を下回るなら移さない。
	md = strings.Join([]string{para('あ', 200), para('い', 500), para('う', 100)}, "\n\n")
	got = ChunkMarkdown(md)
	if len(got) != 2 || utf8.RuneCountInString(got[1].Text) != 100 {
		t.Errorf("chunks = %q, want [702, 100] unchanged", headingsAndTexts(got))
	}
}

func TestChunkerSplitsLongParagraphAtSentences(t *testing.T) {
	sentence := para('あ', 99) + "。"      // 100 文字
	long := strings.Repeat(sentence, 20) // 2,000 文字の1段落
	got := ChunkMarkdown("## 長い段落\n\n" + long + "\n\n短い段落")
	var joined strings.Builder
	for i, c := range got {
		n := utf8.RuneCountInString(c.Text)
		if n > DefaultMaxChunkChars {
			t.Errorf("chunk %d has %d chars, want <= %d", i, n, DefaultMaxChunkChars)
		}
		if c.Heading != "長い段落" {
			t.Errorf("chunk %d heading = %q", i, c.Heading)
		}
		// 文の途中では分けない。
		if !strings.HasSuffix(c.Text, "。") && !strings.HasSuffix(c.Text, "短い段落") {
			t.Errorf("chunk %d does not end at a sentence boundary: …%q", i, string([]rune(c.Text)[utf8.RuneCountInString(c.Text)-5:]))
		}
		joined.WriteString(c.Text)
	}
	if want := long + "短い段落"; strings.ReplaceAll(joined.String(), "\n", "") != want {
		t.Errorf("concatenated chunks lost or duplicated text")
	}
	if len(got) != 3 {
		t.Errorf("got %d chunks, want 3 (800 + 800 + 400 + short paragraph)", len(got))
	}
}

func TestChunkerSplitsLongParagraphAtLinesFirst(t *testing.T) {
	line := para('か', 59) + "\n" // 60 文字の行（文末記号なし）
	got := Chunker{MinChars: 50, MaxChars: 200}.Split(strings.Repeat(line, 10))
	if len(got) != 4 {
		t.Fatalf("got %d chunks, want 4: %q", len(got), headingsAndTexts(got))
	}
	for i, c := range got[:3] {
		if c.Text != strings.TrimSpace(strings.Repeat(line, 3)) {
			t.Errorf("chunk %d = %q, want 3 whole lines", i, c.Text)
		}
	}
}

func TestChunkerHardSplitsUnbreakableText(t *testing.T) {
	text := strings.Repeat("abcdefghij", 25) // 250 文字、文末記号・改行なし
	got := Chunker{MinChars: 10, MaxChars: 100}.Split(text)
	var lens []int
	var joined string
	for _, c := range got {
		lens = append(lens, utf8.RuneCountInString(c.Text))
		joined += c.Text
	}
	if fmt.Sprint(lens) != "[100 100 50]" || joined != text {
		t.Errorf("chunk lengths = %v, want [100 100 50] covering the whole text", lens)
	}
}

func TestSplitSentences(t *testing.T) {
	cases := map[string][]string{
		"一文目。二文目！三文目？":                   {"一文目。", "二文目！", "三文目？"},
		"「本当？」と聞いた。終わり":                  {"「本当？」", "と聞いた。", "終わり"},
		"Pi is 3.14. It is irrational. ": {"Pi is 3.14. ", "It is irrational. "},
		"末尾に記号なし":                        {"末尾に記号なし"},
		"":                               nil,
	}
	for in, want := range cases {
		got := splitSentences(in)
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
			t.Errorf("splitSentences(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChunkerRespectsMaxForGeneratedMarkdown(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		switch i % 7 {
		case 0:
			fmt.Fprintf(&b, "## 見出し%d\n\n", i)
		case 3:
			fmt.Fprintf(&b, "```\n%s\n\n%s\n```\n\n", strings.Repeat("code line\n", i), strings.Repeat("x", i*10))
		default:
			fmt.Fprintf(&b, "%s\n\n", strings.Repeat(fmt.Sprintf("文%d。", i), i*7))
		}
	}
	md := b.String()
	for _, c := range []Chunker{{}, {MinChars: 50, MaxChars: 120}, {MinChars: 500, MaxChars: 300}} {
		max := c.MaxChars
		if max <= 0 {
			max = DefaultMaxChunkChars
		}
		got := c.Split(md)
		var text strings.Builder
		for i, ch := range got {
			if ch.Index != i || strings.TrimSpace(ch.Text) == "" || utf8.RuneCountInString(ch.Text) > max {
				t.Errorf("%+v: chunk %d invalid (index %d, %d chars)", c, i, ch.Index, utf8.RuneCountInString(ch.Text))
			}
			text.WriteString(ch.Text)
		}
		// 見出しの行を除き、本文の文字は失われず重複もしない。
		var want strings.Builder
		for _, line := range strings.Split(md, "\n") {
			if !strings.HasPrefix(line, "## ") {
				want.WriteString(line)
			}
		}
		if stripSpace(text.String()) != stripSpace(want.String()) {
			t.Errorf("%+v: chunks do not cover the body exactly", c)
		}
	}
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func TestChunkEmbeddingText(t *testing.T) {
	c := Chunk{Heading: "量子計算の応用", Text: "本研究では…"}
	if got, want := c.EmbeddingText(), "[見出し: 量子計算の応用] 本研究では…"; got != want {
		t.Errorf("EmbeddingText = %q, want %q", got, want)
	}
	if got := (Chunk{Text: "前書き"}).EmbeddingText(); got != "前書き" {
		t.Errorf("EmbeddingText without heading = %q, want 前書き", got)
	}
}
