package social

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// ellipsis は行に収まらない文字列の末尾に付ける省略記号。
const ellipsis = "…"

// 日本語組版の禁則文字（JIS X 4051 の行頭禁則・行末禁則のうち、記事タイトルに現れやすいもの）。
const (
	// noLineStart は行頭に置かない文字（句読点・閉じ括弧・小書きの仮名・長音記号など）。
	noLineStart = "、。，．,.・：；:;？！?!゛゜ヽヾゝゞ々〻ー‐゠–〜～…‥）)］]｝}〕〉》」』】〙〗〟’”»" +
		"ぁぃぅぇぉっゃゅょゎゕゖァィゥェォッャュョヮヵヶㇰㇱㇲㇳㇴㇵㇶㇷㇸㇹㇺㇻㇼㇽㇾㇿ%％‰℃°′″"
	// noLineEnd は行末に置かない文字（開き括弧・引用符の始め）。
	noLineEnd = "（(［[｛{〔〈《「『【〘〖〝‘“«"
)

// segment は改行の単位。英単語・数字の並びは途中で改行せず、和文は1文字ずつ改行できる。
// 禁則で離せない前後の単位はひとつの segment にまとめる。
type segment struct {
	text string
	// spaceBefore は直前に空白があるか。行頭では空白を詰める。
	spaceBefore bool
}

// isWordRune は欧文の単語として途中で改行しない文字か（ラテン・ギリシャ・キリル文字、数字、ASCII 記号）。
func isWordRune(r rune) bool {
	return r < 0x2000 && !unicode.IsSpace(r)
}

// segments は text を改行の単位に分ける。連続する空白は1つにまとめ、segment の spaceBefore で表す。
func segments(text string) []segment {
	var segs []segment
	// last は直前の単位の最後の文字、pendingSpace は直前の単位の後に空白があったか。
	var last rune
	pendingSpace := false
	add := func(unit string) {
		first, _ := utf8.DecodeRuneInString(unit)
		n := len(segs)
		// 空白の後は常に改行できる。そうでなければ禁則（行頭禁則の文字の前・行末禁則の文字の後）では改行しない。
		if n > 0 && !pendingSpace && (strings.ContainsRune(noLineStart, first) || strings.ContainsRune(noLineEnd, last)) {
			segs[n-1].text += unit
		} else {
			segs = append(segs, segment{text: unit, spaceBefore: pendingSpace && n > 0})
		}
		last, _ = utf8.DecodeLastRuneInString(unit)
		pendingSpace = false
	}
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case unicode.IsSpace(r):
			pendingSpace = true
			i += size
		case isWordRune(r):
			j := i + size
			for j < len(text) {
				r2, s2 := utf8.DecodeRuneInString(text[j:])
				if !isWordRune(r2) {
					break
				}
				j += s2
			}
			add(text[i:j])
			i = j
		default:
			add(text[i : i+size])
			i += size
		}
	}
	return segs
}

// measure は face で描いた s の幅を返す。
func measure(face font.Face, s string) fixed.Int26_6 {
	return font.MeasureString(face, s)
}

// wrapText は text を幅 maxWidth に収まるよう禁則処理をして折り返し、行の一覧を返す。
// 1つの segment が1行に収まらないとき（長い英単語・URL など）は、その segment を文字単位で折り返す。
func wrapText(face font.Face, text string, maxWidth fixed.Int26_6) []string {
	var lines []string
	var cur strings.Builder
	var width fixed.Int26_6
	space := measure(face, " ")
	newLine := func() {
		if cur.Len() > 0 {
			lines = append(lines, cur.String())
		}
		cur.Reset()
		width = 0
	}
	for _, seg := range segments(text) {
		w := measure(face, seg.text)
		gap := fixed.Int26_6(0)
		if seg.spaceBefore && cur.Len() > 0 {
			gap = space
		}
		if width+gap+w <= maxWidth {
			if gap > 0 {
				cur.WriteByte(' ')
			}
			cur.WriteString(seg.text)
			width += gap + w
			continue
		}
		if w <= maxWidth {
			newLine()
			cur.WriteString(seg.text)
			width = w
			continue
		}
		// 1行に収まらない segment は、今の行の続きから文字単位で詰める。
		if gap > 0 && width+gap < maxWidth {
			cur.WriteByte(' ')
			width += gap
		}
		for _, r := range seg.text {
			rw := measure(face, string(r))
			if width+rw > maxWidth && cur.Len() > 0 {
				newLine()
			}
			cur.WriteRune(r)
			width += rw
		}
	}
	newLine()
	return lines
}

// truncateLine は line の末尾を削って省略記号を付け、幅 maxWidth に収める。
func truncateLine(face font.Face, line string, maxWidth fixed.Int26_6) string {
	runes := []rune(strings.TrimRightFunc(line, unicode.IsSpace))
	for len(runes) > 0 {
		s := strings.TrimRightFunc(string(runes), unicode.IsSpace) + ellipsis
		if measure(face, s) <= maxWidth {
			return s
		}
		runes = runes[:len(runes)-1]
	}
	return ellipsis
}

// fitWidth は s が幅 maxWidth に収まればそのまま、収まらなければ末尾を省略記号にして返す。
func fitWidth(face font.Face, s string, maxWidth fixed.Int26_6) string {
	if measure(face, s) <= maxWidth {
		return s
	}
	return truncateLine(face, s, maxWidth)
}

// fitLines は text を maxLines 行以内に収まる最大の文字サイズで折り返す。sizes は大きい順の候補。
// 最小のサイズでも収まらなければ maxLines 行で打ち切り、最終行に省略記号を付ける。
// newFace は文字サイズからフェイスを作る。戻り値のフェイスは呼び出し側が描画に使う。
func fitLines(text string, sizes []float64, maxLines int, maxWidth fixed.Int26_6, newFace func(size float64) (font.Face, error)) (font.Face, float64, []string, error) {
	var (
		face  font.Face
		size  float64
		lines []string
	)
	for _, size = range sizes {
		f, err := newFace(size)
		if err != nil {
			return nil, 0, nil, err
		}
		if face != nil {
			face.Close()
		}
		face = f
		lines = wrapText(face, text, maxWidth)
		if len(lines) <= maxLines {
			return face, size, lines, nil
		}
	}
	lines = lines[:maxLines]
	lines[maxLines-1] = truncateLine(face, lines[maxLines-1], maxWidth)
	return face, size, lines, nil
}
