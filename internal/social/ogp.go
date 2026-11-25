// Package social は記事の拡散を支援する。純 Go（image・image/draw・golang.org/x/image）による
// 1200×630 の動的 OGP 画像の合成と、SNS 告知文の整形（X の文字数の計算・AI 未設定時のテンプレート文面）を提供する。
package social

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// OGP 画像の大きさ（X・Facebook・LINE などが推奨する 1.91:1）。
const (
	OGPWidth  = 1200
	OGPHeight = 630
)

// Card の各項目の最大文字数。これより長い値は切り詰めてから描く。
const (
	maxTitleRunes    = 200
	maxCategoryRunes = 30
	maxSiteRunes     = 40
	maxFooterRunes   = 40
)

// fontGzip は日本語の描画に使う Noto Sans JP Bold のサブセット（fonts/README.md）。
//
//go:embed fonts/NotoSansJP-Bold-subset.ttf.gz
var fontGzip []byte

var (
	fontOnce   sync.Once
	parsedFont *opentype.Font
	fontErr    error
)

// loadFont は埋め込みフォントを初回だけ展開して解析する。*opentype.Font は並行に使ってよい（Face は不可）。
func loadFont() (*opentype.Font, error) {
	fontOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(fontGzip))
		if err != nil {
			fontErr = fmt.Errorf("social: open embedded font: %w", err)
			return
		}
		data, err := io.ReadAll(zr)
		if err != nil {
			fontErr = fmt.Errorf("social: read embedded font: %w", err)
			return
		}
		if parsedFont, err = opentype.Parse(data); err != nil {
			fontErr = fmt.Errorf("social: parse embedded font: %w", err)
		}
	})
	return parsedFont, fontErr
}

func newFace(f *opentype.Font, size float64) (font.Face, error) {
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
}

// Card は OGP 画像に描く内容。
type Card struct {
	// Title は記事タイトル。3行に収まる最大の文字サイズで、禁則処理をして折り返す。
	Title string
	// Category はタイトルの上に出すカテゴリのバッジ。空ならバッジを出さない。背景の配色もカテゴリで決まる。
	Category string
	// SiteName は左下に出すメディア名。頭文字をロゴマークにする。
	SiteName string
	// Footer は右下に添える補足（公開日など）。
	Footer string
}

// normalized は空白を詰め、各項目を最大文字数で切り詰めた Card を返す。
func (c Card) normalized() Card {
	return Card{
		Title:    clip(c.Title, maxTitleRunes),
		Category: clip(c.Category, maxCategoryRunes),
		SiteName: clip(c.SiteName, maxSiteRunes),
		Footer:   clip(c.Footer, maxFooterRunes),
	}
}

// key は Card の内容から決まるキャッシュキー。タイトルやカテゴリ名が変われば別の画像になる。
func (c Card) key() string {
	h := sha256.New()
	for _, s := range []string{c.Title, c.Category, c.SiteName, c.Footer} {
		io.WriteString(h, s)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// clip は連続する空白（改行を含む）を1つの半角空白にまとめ、max 文字を超える分を省略記号にする。
func clip(s string, max int) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + ellipsis
}

// palette は背景のグラデーションと、バッジ・ロゴの配色。
type palette struct {
	from, to color.RGBA
}

// palettes は落ち着いた濃色のグラデーション。カテゴリごとに固定で選ぶ。
var palettes = []palette{
	{from: rgb(0x1e1b4b), to: rgb(0x4f46e5)}, // インディゴ
	{from: rgb(0x0c2a43), to: rgb(0x0e7490)}, // ディープシー
	{from: rgb(0x3f0d24), to: rgb(0xbe123c)}, // ボルドー
	{from: rgb(0x052e2b), to: rgb(0x047857)}, // フォレスト
	{from: rgb(0x2e1065), to: rgb(0x7c3aed)}, // バイオレット
	{from: rgb(0x292524), to: rgb(0xb45309)}, // セピア
}

func rgb(v uint32) color.RGBA {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// paletteFor はカテゴリ名から配色を選ぶ。同じカテゴリの記事は常に同じ配色になる。
func paletteFor(category string) palette {
	if category == "" {
		return palettes[0]
	}
	h := fnv.New32a()
	io.WriteString(h, category)
	return palettes[h.Sum32()%uint32(len(palettes))]
}

// レイアウト（px）。白いカードの内側に、カテゴリ・タイトル・区切り線・メディア名を上から並べる。
const (
	cardMargin   = 56
	cardRadius   = 32
	contentLeft  = 128
	contentRight = OGPWidth - 128
	contentTop   = 116
	badgeHeight  = 52
	badgeSize    = 26
	badgePadding = 24
	titleBottom  = 440
	dividerY     = 466
	footerCenter = 520
	logoRadius   = 22
	siteSize     = 28
	footerSize   = 24
	titleMaxLine = 3
	// titleLineHeight は行送り（文字サイズの倍率）。
	titleLineHeight = 1.4
	// emTop・emCenter は和文の仮想ボディ（em ボックス）の上端・中心のベースラインからの高さ（文字サイズの倍率）。
	emTop    = 0.88
	emCenter = 0.38
)

// titleSizes はタイトルの文字サイズの候補（大きい順）。長いタイトルほど小さくして3行に収める。
var titleSizes = []float64{72, 66, 60, 56, 52, 48, 44, 40}

var (
	white     = color.RGBA{0xff, 0xff, 0xff, 0xff}
	titleInk  = rgb(0x0f172a)
	mutedInk  = rgb(0x64748b)
	ruleColor = rgb(0xe2e8f0)
)

// Render は card を 1200×630 の OGP 画像に描く。並行に呼んでよい。
func Render(card Card) (*image.RGBA, error) {
	f, err := loadFont()
	if err != nil {
		return nil, err
	}
	card = card.normalized()
	pal := paletteFor(card.Category)
	img := image.NewRGBA(image.Rect(0, 0, OGPWidth, OGPHeight))

	// 背景: 左上から右下への斜めのグラデーションに、半透明の円と輪を重ねる。
	fillGradient(img, pal.from, pal.to)
	fillCircle(img, 1110, 40, 250, color.NRGBA{0xff, 0xff, 0xff, 0x1c})
	strokeCircle(img, 1110, 40, 330, 2, color.NRGBA{0xff, 0xff, 0xff, 0x30})
	fillCircle(img, 70, 640, 170, color.NRGBA{0xff, 0xff, 0xff, 0x14})
	fillRoundedRect(img, cardMargin, cardMargin, OGPWidth-cardMargin, OGPHeight-cardMargin, cardRadius, white)

	// 描けない文字（絵文字など、フォントに無い字）は豆腐（□）にせず取り除く。
	probe, err := newFace(f, siteSize)
	if err != nil {
		return nil, err
	}
	defer probe.Close()
	title := printable(probe, card.Title)
	category := printable(probe, card.Category)
	siteName := printable(probe, card.SiteName)
	footer := printable(probe, card.Footer)
	if title == "" {
		title = siteName
	}

	top := contentTop
	if category != "" {
		face, err := newFace(f, badgeSize)
		if err != nil {
			return nil, err
		}
		category = fitWidth(face, category, fixed.I(contentRight-contentLeft-2*badgePadding))
		w := measure(face, category).Ceil() + 2*badgePadding
		fillRoundedRect(img, contentLeft, top, contentLeft+w, top+badgeHeight, badgeHeight/2, pal.to)
		drawString(img, face, contentLeft+badgePadding, baselineAt(top+badgeHeight/2, badgeSize), category, white)
		face.Close()
		top += badgeHeight + 28
	}

	// タイトル: 収まる最大の文字サイズで折り返し、残りの領域の上下中央に置く。
	face, size, lines, err := fitLines(title, titleSizes, titleMaxLine, fixed.I(contentRight-contentLeft),
		func(size float64) (font.Face, error) { return newFace(f, size) })
	if err != nil {
		return nil, err
	}
	lineHeight := size * titleLineHeight
	blockHeight := float64(len(lines)-1)*lineHeight + size
	y := float64(top) + (float64(titleBottom-top)-blockHeight)/2
	for i, line := range lines {
		drawString(img, face, contentLeft, int(y+emTop*size+float64(i)*lineHeight+0.5), line, titleInk)
	}
	face.Close()

	fillRect(img, image.Rect(contentLeft, dividerY, contentRight, dividerY+2), ruleColor)

	// フッター: ロゴマーク（メディア名の頭文字）とメディア名、右端に公開日など。
	if siteName != "" {
		fillCircle(img, contentLeft+logoRadius, footerCenter, logoRadius, pal.from)
		initial, _ := utf8.DecodeRuneInString(siteName)
		face, err := newFace(f, 24)
		if err != nil {
			return nil, err
		}
		iw := measure(face, string(initial)).Round()
		drawString(img, face, contentLeft+logoRadius-iw/2, baselineAt(footerCenter, 24), string(initial), white)
		face.Close()

		face, err = newFace(f, siteSize)
		if err != nil {
			return nil, err
		}
		// 右端の公開日などと重ならないよう、メディア名の幅を抑える。
		nameLeft := contentLeft + 2*logoRadius + 16
		name := fitWidth(face, siteName, fixed.I(contentRight-nameLeft-280))
		drawString(img, face, nameLeft, baselineAt(footerCenter, siteSize), name, pal.from)
		face.Close()
	}
	if footer != "" {
		face, err := newFace(f, footerSize)
		if err != nil {
			return nil, err
		}
		w := measure(face, footer).Ceil()
		drawString(img, face, contentRight-w, baselineAt(footerCenter, footerSize), footer, mutedInk)
		face.Close()
	}
	return img, nil
}

// EncodePNG は card を描いた PNG を返す。
func EncodePNG(card Card) ([]byte, error) {
	img, err := Render(card)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("social: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// printable は face に字形の無い文字と制御文字を除き、空白を詰めた文字列を返す。
func printable(face font.Face, s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			b.WriteRune(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		if _, ok := face.GlyphAdvance(r); ok {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// baselineAt は文字サイズ size の行を、高さ center に上下中央揃えで置くときのベースラインを返す。
func baselineAt(center int, size float64) int {
	return center + int(emCenter*size+0.5)
}

func drawString(dst *image.RGBA, face font.Face, x, baseline int, s string, c color.Color) {
	d := font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x, baseline)}
	d.DrawString(s)
}

// fillGradient は画像全体を左上の from から右下の to への線形グラデーションで塗る。
func fillGradient(img *image.RGBA, from, to color.RGBA) {
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	// 対角線 (w, h) 方向への射影で 0〜1 の位置を求める。
	denom := w*w + h*h
	for y := 0; y < b.Dy(); y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < b.Dx(); x++ {
			t := (float64(x)*w + float64(y)*h) / denom
			i := 4 * x
			row[i+0] = lerp8(from.R, to.R, t)
			row[i+1] = lerp8(from.G, to.G, t)
			row[i+2] = lerp8(from.B, to.B, t)
			row[i+3] = 0xff
		}
	}
}

func lerp8(a, b uint8, t float64) uint8 {
	return uint8(float64(a) + (float64(b)-float64(a))*t + 0.5)
}

func fillRect(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

// fillPath は path で描いた図形を c で塗る（アンチエイリアス付き）。bounds は図形の外接矩形で、
// ラスタライザーはその大きさだけ確保する。path には外接矩形の左上を原点とする座標変換 pt が渡される。
func fillPath(img *image.RGBA, bounds image.Rectangle, c color.Color, path func(z *vector.Rasterizer, pt func(x, y float64) (float32, float32))) {
	r := bounds.Intersect(img.Bounds())
	if r.Empty() {
		return
	}
	z := vector.NewRasterizer(r.Dx(), r.Dy())
	ox, oy := float64(r.Min.X), float64(r.Min.Y)
	path(z, func(x, y float64) (float32, float32) { return float32(x - ox), float32(y - oy) })
	z.Draw(img, r, image.NewUniform(c), image.Point{})
}

// kappa は4本の3次ベジェ曲線で円を近似するときの制御点の比率。
const kappa = 0.5522847498

// addCircle は中心 (cx, cy)・半径 r の円をパスに加える。clockwise で向きを変える（輪の内側の穴に使う）。
func addCircle(z *vector.Rasterizer, pt func(x, y float64) (float32, float32), cx, cy, r float64, clockwise bool) {
	k := kappa * r
	s := 1.0
	if !clockwise {
		s = -1
	}
	cubic := func(x1, y1, x2, y2, x3, y3 float64) {
		ax, ay := pt(x1, y1)
		bx, by := pt(x2, y2)
		dx, dy := pt(x3, y3)
		z.CubeTo(ax, ay, bx, by, dx, dy)
	}
	mx, my := pt(cx+r, cy)
	z.MoveTo(mx, my)
	cubic(cx+r, cy+s*k, cx+k, cy+s*r, cx, cy+s*r)
	cubic(cx-k, cy+s*r, cx-r, cy+s*k, cx-r, cy)
	cubic(cx-r, cy-s*k, cx-k, cy-s*r, cx, cy-s*r)
	cubic(cx+k, cy-s*r, cx+r, cy-s*k, cx+r, cy)
	z.ClosePath()
}

func circleBounds(cx, cy, r float64) image.Rectangle {
	return image.Rect(int(cx-r)-1, int(cy-r)-1, int(cx+r)+2, int(cy+r)+2)
}

func fillCircle(img *image.RGBA, cx, cy, r float64, c color.Color) {
	fillPath(img, circleBounds(cx, cy, r), c, func(z *vector.Rasterizer, pt func(x, y float64) (float32, float32)) {
		addCircle(z, pt, cx, cy, r, true)
	})
}

// strokeCircle は半径 r・太さ width の輪を描く。外周と逆向きの内周で中を抜く。
func strokeCircle(img *image.RGBA, cx, cy, r, width float64, c color.Color) {
	fillPath(img, circleBounds(cx, cy, r+width/2), c, func(z *vector.Rasterizer, pt func(x, y float64) (float32, float32)) {
		addCircle(z, pt, cx, cy, r+width/2, true)
		addCircle(z, pt, cx, cy, r-width/2, false)
	})
}

// fillRoundedRect は角の半径 radius の角丸矩形を塗る。
func fillRoundedRect(img *image.RGBA, x0, y0, x1, y1, radius int, c color.Color) {
	fx0, fy0, fx1, fy1 := float64(x0), float64(y0), float64(x1), float64(y1)
	r := min(float64(radius), (fx1-fx0)/2, (fy1-fy0)/2)
	k := kappa * r
	fillPath(img, image.Rect(x0, y0, x1, y1), c, func(z *vector.Rasterizer, pt func(x, y float64) (float32, float32)) {
		move := func(x, y float64) { px, py := pt(x, y); z.MoveTo(px, py) }
		line := func(x, y float64) { px, py := pt(x, y); z.LineTo(px, py) }
		cubic := func(x1, y1, x2, y2, x3, y3 float64) {
			ax, ay := pt(x1, y1)
			bx, by := pt(x2, y2)
			dx, dy := pt(x3, y3)
			z.CubeTo(ax, ay, bx, by, dx, dy)
		}
		move(fx0+r, fy0)
		line(fx1-r, fy0)
		cubic(fx1-r+k, fy0, fx1, fy0+r-k, fx1, fy0+r)
		line(fx1, fy1-r)
		cubic(fx1, fy1-r+k, fx1-r+k, fy1, fx1-r, fy1)
		line(fx0+r, fy1)
		cubic(fx0+r-k, fy1, fx0, fy1-r+k, fx0, fy1-r)
		line(fx0, fy0+r)
		cubic(fx0, fy0+r-k, fx0+r-k, fy0, fx0+r, fy0)
		z.ClosePath()
	})
}
