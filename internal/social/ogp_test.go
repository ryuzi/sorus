package social

import (
	"bytes"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

func testFace(t *testing.T, size float64) font.Face {
	t.Helper()
	f, err := loadFont()
	if err != nil {
		t.Fatalf("loadFont: %v", err)
	}
	face, err := newFace(f, size)
	if err != nil {
		t.Fatalf("newFace: %v", err)
	}
	t.Cleanup(func() { face.Close() })
	return face
}

func TestEmbeddedFontHasJapaneseGlyphs(t *testing.T) {
	face := testFace(t, 10)
	for _, r := range "あア亜漢字、。「」ー々ＡａＺ0123456789AZaz…—𠮟" {
		if _, ok := face.GlyphAdvance(r); !ok {
			t.Errorf("embedded font has no glyph for %q (U+%04X)", r, r)
		}
	}
	// 和文は全角（1em）幅。折り返しのテストはこの前提で幅を決める。
	if adv, _ := face.GlyphAdvance('あ'); adv != fixed.I(10) {
		t.Errorf("advance of あ at 10px = %v, want 10px", adv)
	}
}

func TestWrapTextKinsoku(t *testing.T) {
	face := testFace(t, 10)
	// 1行5文字。「、」は行頭に置けないため、前の「お」と一緒に次の行へ送る（追い出し）。
	got := wrapText(face, "あいうえお、かきくけこ", fixed.I(50))
	want := []string{"あいうえ", "お、かきく", "けこ"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q, want %q", got, want)
	}

	// 開き括弧は行末に置かず、次の文字と一緒に送る。
	got = wrapText(face, "あいうえ「かきくけこ」", fixed.I(50))
	want = []string{"あいうえ", "「かきくけ", "こ」"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q, want %q", got, want)
	}

	text := "学生記者が「春の図書館」で出会った一冊。読書会（参加無料）は、毎週金曜日に開催！ぜひ来てください。"
	for _, width := range []int{40, 50, 60, 70, 80, 90, 130} {
		lines := wrapText(face, text, fixed.I(width))
		if strings.Join(lines, "") != text {
			t.Errorf("width %d: lines %q do not reconstruct the text", width, lines)
		}
		for i, line := range lines {
			first, _ := utf8.DecodeRuneInString(line)
			last, _ := utf8.DecodeLastRuneInString(line)
			if i > 0 && strings.ContainsRune(noLineStart, first) {
				t.Errorf("width %d: line %d %q starts with a no-line-start character", width, i, line)
			}
			if i < len(lines)-1 && strings.ContainsRune(noLineEnd, last) {
				t.Errorf("width %d: line %d %q ends with a no-line-end character", width, i, line)
			}
		}
	}
}

func TestWrapTextKeepsLatinWords(t *testing.T) {
	face := testFace(t, 20)
	text := "Open campus 2026 with AIと学生メディア"
	lines := wrapText(face, text, fixed.I(160))
	if len(lines) < 2 {
		t.Fatalf("lines = %q, want wrapping", lines)
	}
	words := map[string]bool{"Open": true, "campus": true, "2026": true, "with": true, "AI": true}
	for _, line := range lines {
		if strings.HasPrefix(line, " ") || strings.HasSuffix(line, " ") {
			t.Errorf("line %q has leading/trailing space", line)
		}
		for _, f := range strings.Fields(line) {
			if w := strings.TrimRight(f, "と学生メディア"); w != "" && !words[w] {
				t.Errorf("word %q was split across lines: %q", f, lines)
			}
		}
		if measure(face, line) > fixed.I(160) {
			t.Errorf("line %q overflows", line)
		}
	}

	// 1行に収まらない長い単語（URL など）は文字単位で折り返す。
	long := "https://example.ac.jp/a/very/long/path/that/never/fits"
	lines = wrapText(face, long, fixed.I(120))
	if len(lines) < 2 || strings.Join(lines, "") != long {
		t.Errorf("long word lines = %q", lines)
	}
	for _, line := range lines {
		if measure(face, line) > fixed.I(120) {
			t.Errorf("line %q overflows", line)
		}
	}
}

func TestFitLinesShrinksThenTruncates(t *testing.T) {
	f, _ := loadFont()
	newF := func(size float64) (font.Face, error) { return newFace(f, size) }
	width := fixed.I(contentRight - contentLeft)

	face, size, lines, err := fitLines("短いタイトル", titleSizes, titleMaxLine, width, newF)
	if err != nil {
		t.Fatal(err)
	}
	face.Close()
	if size != titleSizes[0] || len(lines) != 1 {
		t.Errorf("short title: size %v lines %q, want largest size on one line", size, lines)
	}

	medium := strings.Repeat("学生メディアの記事タイトル", 4)
	face, size, lines, err = fitLines(medium, titleSizes, titleMaxLine, width, newF)
	if err != nil {
		t.Fatal(err)
	}
	face.Close()
	if size >= titleSizes[0] || len(lines) > titleMaxLine || strings.Join(lines, "") != medium {
		t.Errorf("medium title: size %v lines %q, want a smaller size without truncation", size, lines)
	}

	long := strings.Repeat("とても長いタイトルが続きます。", 20)
	face, size, lines, err = fitLines(long, titleSizes, titleMaxLine, width, newF)
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
	if size != titleSizes[len(titleSizes)-1] || len(lines) != titleMaxLine {
		t.Fatalf("long title: size %v, %d lines, want smallest size and %d lines", size, len(lines), titleMaxLine)
	}
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, ellipsis) || measure(face, last) > width {
		t.Errorf("last line %q should end with an ellipsis and fit", last)
	}
}

func TestRenderOGPImage(t *testing.T) {
	card := Card{Title: "文学部の学生が「春の図書館」で出会った一冊", Category: "キャンパスライフ", SiteName: "学生メディア", Footer: "2026.10.20"}
	data, err := EncodePNG(card)
	if err != nil {
		t.Fatalf("EncodePNG: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != OGPWidth || b.Dy() != OGPHeight {
		t.Fatalf("size = %v, want %dx%d", b, OGPWidth, OGPHeight)
	}
	// 背景はカテゴリの配色のグラデーション（左上が from、右下が to に近い）、中央は白いカード。
	pal := paletteFor(card.Category)
	if c := color.RGBAModel.Convert(img.At(2, 2)).(color.RGBA); !near(c, pal.from, 12) {
		t.Errorf("top-left = %v, want near %v", c, pal.from)
	}
	if c := color.RGBAModel.Convert(img.At(OGPWidth-cardMargin-cardRadius, OGPHeight-3)).(color.RGBA); !near(c, pal.to, 24) {
		t.Errorf("bottom-right = %v, want near %v", c, pal.to)
	}
	if c := color.RGBAModel.Convert(img.At(OGPWidth/2, 100)).(color.RGBA); c != white {
		t.Errorf("card background = %v, want white", c)
	}
	// タイトル領域に濃い文字が描かれている。
	dark := 0
	for y := contentTop + badgeHeight; y < titleBottom; y++ {
		for x := contentLeft; x < contentRight; x++ {
			if c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA); c.R < 0x40 && c.G < 0x40 && c.B < 0x60 {
				dark++
			}
		}
	}
	if dark < 5000 {
		t.Errorf("title area has only %d dark pixels; title was not drawn", dark)
	}
}

func near(a, b color.RGBA, tol int) bool {
	d := func(x, y uint8) bool { v := int(x) - int(y); return v <= tol && v >= -tol }
	return d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B)
}

func TestPrintableDropsMissingGlyphs(t *testing.T) {
	face := testFace(t, 20)
	if got := printable(face, "新歓　2026\tキャンパス\x00ツアー"); got != "新歓 2026 キャンパスツアー" {
		t.Errorf("printable = %q", got)
	}
}

func TestPaletteIsStablePerCategory(t *testing.T) {
	if paletteFor("研究") != paletteFor("研究") {
		t.Error("palette differs for the same category")
	}
	if paletteFor("") != palettes[0] {
		t.Error("empty category should use the default palette")
	}
	seen := map[palette]bool{}
	for _, c := range []string{"研究", "キャンパスライフ", "サークル", "就活", "国際", "インタビュー", "イベント", "コラム"} {
		seen[paletteFor(c)] = true
	}
	if len(seen) < 3 {
		t.Errorf("categories map to only %d palettes", len(seen))
	}
}

func TestGeneratorCachesAndEvicts(t *testing.T) {
	g, err := NewGenerator(2)
	if err != nil {
		t.Fatal(err)
	}
	var renders atomic.Int32
	g.render = func(c Card) ([]byte, error) {
		renders.Add(1)
		return []byte("png:" + c.Title), nil
	}
	get := func(title string) (string, string) {
		t.Helper()
		b, etag, err := g.PNG(Card{Title: title, SiteName: "学生メディア"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b), etag
	}
	a1, etagA := get("A")
	if etagA != ETag(Card{Title: "A", SiteName: "学生メディア"}) {
		t.Errorf("PNG etag %s differs from ETag()", etagA)
	}
	a2, etagA2 := get("  A ") // 空白の違いは同じ画像
	if a1 != "png:A" || a2 != a1 || etagA != etagA2 || renders.Load() != 1 {
		t.Fatalf("second request should hit the cache: %q %q renders=%d", a1, a2, renders.Load())
	}
	if !strings.HasPrefix(etagA, `"`) || !strings.HasSuffix(etagA, `"`) {
		t.Errorf("etag %s is not quoted", etagA)
	}
	if _, etagB := get("B"); etagB == etagA {
		t.Error("different cards share an etag")
	}
	get("A") // A を最近使ったものにする
	get("C") // 上限2枚: 最も古い B を捨てる
	if g.Len() != 2 {
		t.Errorf("cache len = %d, want 2", g.Len())
	}
	before := renders.Load()
	get("A")
	if renders.Load() != before {
		t.Error("A should still be cached")
	}
	get("B")
	if renders.Load() != before+1 {
		t.Error("B should have been evicted and re-rendered")
	}
}

func TestGeneratorDeduplicatesConcurrentRenders(t *testing.T) {
	g, err := NewGenerator(10)
	if err != nil {
		t.Fatal(err)
	}
	var renders atomic.Int32
	release := make(chan struct{})
	g.render = func(c Card) ([]byte, error) {
		renders.Add(1)
		<-release
		return []byte("png"), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, _, err := g.PNG(Card{Title: "同時アクセス"}); err != nil || string(b) != "png" {
				t.Errorf("PNG = %q, %v", b, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := renders.Load(); n != 1 {
		t.Errorf("rendered %d times, want 1", n)
	}
}

func TestGeneratorDoesNotCacheErrors(t *testing.T) {
	g, err := NewGenerator(10)
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	g.render = func(Card) ([]byte, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return []byte("ok"), nil
	}
	if _, _, err := g.PNG(Card{Title: "x"}); err == nil {
		t.Fatal("want error")
	}
	fail = false
	if b, _, err := g.PNG(Card{Title: "x"}); err != nil || string(b) != "ok" {
		t.Fatalf("retry = %q, %v", b, err)
	}
}

func TestGeneratorCachedResponseIsFast(t *testing.T) {
	g, err := NewGenerator(DefaultCacheSize)
	if err != nil {
		t.Fatal(err)
	}
	card := Card{Title: "キャッシュの速度", Category: "研究", SiteName: "学生メディア"}
	if _, _, err := g.PNG(card); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if _, _, err := g.PNG(card); err != nil {
			t.Fatal(err)
		}
	}
	// 仕様の目標（応答 10ms 未満）に対して、キャッシュ命中は1回あたりマイクロ秒単位で返る。
	if per := time.Since(start) / 1000; per > time.Millisecond {
		t.Errorf("cached PNG took %v per call", per)
	}
}

func BenchmarkRender(b *testing.B) {
	card := Card{Title: "文学部の学生が「春の図書館」で出会った一冊", Category: "キャンパスライフ", SiteName: "学生メディア", Footer: "2026.10.20"}
	for i := 0; i < b.N; i++ {
		if _, err := EncodePNG(Card{Title: fmt.Sprint(card.Title, i), Category: card.Category, SiteName: card.SiteName}); err != nil {
			b.Fatal(err)
		}
	}
}
