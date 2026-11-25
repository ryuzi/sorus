package main

import (
	"image"
	"image/color"
	"image/draw"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// 標準のデザイン。static/icon.svg（viewBox 512）と同じ: 角丸（rx 128）の濃紺の正方形に、白い太字の「s」を中央
// （x 50%・y 54%）に置く。SVG の font-family（system-ui）の代わりに、Go に同梱の Go Bold を使う。
var (
	designBackground = color.NRGBA{0x0f, 0x17, 0x2a, 0xff} // #0f172a
	designForeground = color.NRGBA{0xf8, 0xfa, 0xfc, 0xff} // #f8fafc
)

const (
	designViewBox  = 512.0
	designRadius   = 128.0
	designFontSize = 280.0
	designCenterX  = 0.50
	designCenterY  = 0.54
	designGlyph    = 's'
)

// RenderDefault は標準のデザインを size x size で描く。
func RenderDefault(size int) *image.NRGBA {
	s := float32(size) / designViewBox
	dst := image.NewRGBA(image.Rect(0, 0, size, size))

	bg := vector.NewRasterizer(size, size)
	roundedRect(bg, float32(size), designRadius*s)
	bg.Draw(dst, dst.Bounds(), image.NewUniform(designBackground), image.Point{})

	fg := vector.NewRasterizer(size, size)
	if glyphPath(fg, float64(size), designFontSize*float64(s)) {
		fg.Draw(dst, dst.Bounds(), image.NewUniform(designForeground), image.Point{})
	}
	out := image.NewNRGBA(dst.Bounds())
	draw.Draw(out, out.Bounds(), dst, image.Point{}, draw.Src)
	return out
}

// roundedRect は一辺 w、角の半径 r の角丸の正方形の輪郭を z に加える。
func roundedRect(z *vector.Rasterizer, w, r float32) {
	const k = 0.5522847498 // 4 分の 1 円を 3 次ベジェで近似する係数
	c := r * k
	z.MoveTo(r, 0)
	z.LineTo(w-r, 0)
	z.CubeTo(w-r+c, 0, w, r-c, w, r)
	z.LineTo(w, w-r)
	z.CubeTo(w, w-r+c, w-r+c, w, w-r, w)
	z.LineTo(r, w)
	z.CubeTo(r-c, w, 0, w-r+c, 0, w-r)
	z.LineTo(0, r)
	z.CubeTo(0, r-c, r-c, 0, r, 0)
	z.ClosePath()
}

// glyphPath は designGlyph の輪郭を、大きさ px で、外接矩形の中心が (designCenterX, designCenterY) に来るよう z に加える。
func glyphPath(z *vector.Rasterizer, size, px float64) bool {
	f, err := sfnt.Parse(gobold.TTF)
	if err != nil {
		panic("gen: parse Go Bold: " + err.Error())
	}
	var buf sfnt.Buffer
	idx, err := f.GlyphIndex(&buf, designGlyph)
	if err != nil || idx == 0 {
		return false
	}
	ppem := fixed.Int26_6(px * 64)
	segs, err := f.LoadGlyph(&buf, idx, ppem, nil)
	if err != nil || len(segs) == 0 {
		return false
	}
	b := segs.Bounds()
	minX, minY := float64(b.Min.X)/64, float64(b.Min.Y)/64
	maxX, maxY := float64(b.Max.X)/64, float64(b.Max.Y)/64
	dx := size*designCenterX - (minX+maxX)/2
	dy := size*designCenterY - (minY+maxY)/2
	pt := func(p fixed.Point26_6) (float32, float32) {
		return float32(float64(p.X)/64 + dx), float32(float64(p.Y)/64 + dy)
	}
	for _, seg := range segs {
		switch seg.Op {
		case sfnt.SegmentOpMoveTo:
			x, y := pt(seg.Args[0])
			z.MoveTo(x, y)
		case sfnt.SegmentOpLineTo:
			x, y := pt(seg.Args[0])
			z.LineTo(x, y)
		case sfnt.SegmentOpQuadTo:
			x1, y1 := pt(seg.Args[0])
			x, y := pt(seg.Args[1])
			z.QuadTo(x1, y1, x, y)
		case sfnt.SegmentOpCubeTo:
			x1, y1 := pt(seg.Args[0])
			x2, y2 := pt(seg.Args[1])
			x, y := pt(seg.Args[2])
			z.CubeTo(x1, y1, x2, y2, x, y)
		}
	}
	z.ClosePath()
	return true
}

// Resize は img を size x size に縮小・拡大する（Catmull-Rom。縮小では範囲を広げて平均するため、小さいアイコンでも
// ギザギザになりにくい）。結果は非乗算アルファ（NRGBA）。
func Resize(img image.Image, size int) *image.NRGBA {
	b := img.Bounds()
	if b.Dx() == size && b.Dy() == size {
		out := image.NewNRGBA(image.Rect(0, 0, size, size))
		draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
		return out
	}
	// 乗算済みアルファで補間してから非乗算に戻す（透明な縁の色がにじまない）。
	tmp := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(tmp, tmp.Bounds(), img, b, xdraw.Src, nil)
	out := image.NewNRGBA(tmp.Bounds())
	draw.Draw(out, out.Bounds(), tmp, image.Point{}, draw.Src)
	return out
}
