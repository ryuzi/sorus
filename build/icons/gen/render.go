package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/vector"
)

// 標準のデザイン（アイコン static/icon.svg と、ロゴ docs/images/sorus-logo.svg のシンボルマークの形と色）。
//
// シンボルマークは、白い円（designPlate）の上に、#255799（コバルト・スレート）の端の丸い線 5 本（designLines）と
// 円 6 つ（designDots）で、中心 (112, 133) から枝分かれする形を描く。座標はロゴの座標系（viewBox 600x250）で、
// アイコンでは designIconTranslate・designIconScale で 256x256 に収める。値は static/icon.svg と同じにする
// （TestRenderDefaultFollowsTheSVGDesign が確かめる）。
const (
	designStrokeWidth      = 5.5 // 線の太さ（stroke-width）
	designPlateStrokeWidth = 4.0 // アイコンの白い円の外枠線の太さ

	designIconViewBox = 256.0 // アイコンの viewBox（正方形）
	designIconScale   = 1.353 // アイコンの transform="translate(-31.7, -41.2) scale(1.353)"
)

var (
	designPrimaryColor = color.NRGBA{0x25, 0x57, 0x99, 0xff} // #255799
	designPlateColor   = color.NRGBA{0xff, 0xff, 0xff, 0xff} // #FFFFFF

	designIconTranslate = [2]float64{-31.7, -41.2}
)

// designCircle は SVG の <circle>（中心 cx, cy・半径 r）。designLine は SVG の <line>（端点 x1, y1 と x2, y2）。
type designCircle struct{ cx, cy, r float64 }
type designLine struct{ x1, y1, x2, y2 float64 }

var (
	designPlate = designCircle{118, 125, 87}
	designLines = []designLine{
		{112, 133, 88, 90},
		{112, 133, 156, 85},
		{112, 133, 168, 125},
		{112, 133, 142, 169},
		{112, 133, 76, 163},
	}
	designDots = []designCircle{
		{112, 133, 16.5},
		{88, 90, 8.5},
		{156, 85, 12.5},
		{168, 125, 8},
		{142, 169, 11},
		{76, 163, 11.5},
	}
)

// RenderDefault は標準のデザイン（static/icon.svg と同じ）を size x size で描く。背景は透明で、
// SVG と同じ transform="translate(-31.7, -41.2) scale(1.353)" で、#255799 の外枠線（太さ 4）付きの白い円と、
// その上の #255799 の端の丸い線 5 本・円 6 つを描く。
func RenderDefault(size int) *image.NRGBA {
	s := float64(size) / designIconViewBox
	// pt はシンボルの座標（ロゴの座標系）を画像の座標にする。
	pt := func(x, y float64) (float32, float32) {
		return float32((x*designIconScale + designIconTranslate[0]) * s), float32((y*designIconScale + designIconTranslate[1]) * s)
	}
	scale := designIconScale * s
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	fill := func(c color.NRGBA, add func(z *vector.Rasterizer)) {
		z := vector.NewRasterizer(size, size)
		add(z)
		z.Draw(dst, dst.Bounds(), image.NewUniform(c), image.Point{})
	}

	// 外枠線は円の縁を中心に内外へ半分ずつ。外側の円を #255799 で塗り、内側の円を白で塗り重ねる。
	p := designPlate
	fill(designPrimaryColor, func(z *vector.Rasterizer) {
		circlePath(z, pt, p.cx, p.cy, (p.r+designPlateStrokeWidth/2)*scale)
	})
	fill(designPlateColor, func(z *vector.Rasterizer) {
		circlePath(z, pt, p.cx, p.cy, (p.r-designPlateStrokeWidth/2)*scale)
	})

	// 線と円は同じ向きの輪郭として 1 つのラスタライザーに加える（重なりは足し合わせて 1 で止まるため、
	// 合わせた形の縁にだけアンチエイリアスがかかり、重なりの境目に筋が出ない）。
	fill(designPrimaryColor, func(z *vector.Rasterizer) {
		for _, l := range designLines {
			capsulePath(z, pt, l, designStrokeWidth/2, scale)
		}
		for _, c := range designDots {
			circlePath(z, pt, c.cx, c.cy, c.r*scale)
		}
	})

	out := image.NewNRGBA(dst.Bounds())
	draw.Draw(out, out.Bounds(), dst, image.Point{}, draw.Src)
	return out
}

// circlePath は、シンボルの座標で中心 (cx, cy) の、画像の座標で半径 r の円を z に加える（pt で中心を画像の座標に
// する。4 分の 1 円ずつ 3 次ベジェで近似する。画面の座標で時計回り）。
func circlePath(z *vector.Rasterizer, pt func(x, y float64) (float32, float32), cx, cy, r float64) {
	const k = 0.5522847498 // 4 分の 1 円を 3 次ベジェで近似する係数
	x, y := pt(cx, cy)
	rr := float32(r)
	d := rr * k
	z.MoveTo(x+rr, y)
	z.CubeTo(x+rr, y+d, x+d, y+rr, x, y+rr)
	z.CubeTo(x-d, y+rr, x-rr, y+d, x-rr, y)
	z.CubeTo(x-rr, y-d, x-d, y-rr, x, y-rr)
	z.CubeTo(x+d, y-rr, x+rr, y-d, x+rr, y)
	z.ClosePath()
}

// capsulePath は線分 l（シンボルの座標）を、太さ 2*hw の端の丸い線（stroke-linecap="round"）として z に加える
// （pt で座標を、scale で太さを画像の座標にする）。線の本体の長方形と、両端の半径 hw の円を、circlePath と同じ向きで加える。
func capsulePath(z *vector.Rasterizer, pt func(x, y float64) (float32, float32), l designLine, hw, scale float64) {
	dx, dy := l.x2-l.x1, l.y2-l.y1
	if n := math.Hypot(dx, dy); n > 0 {
		// 法線（-dy, dx）。この順の長方形は、画面の座標で circlePath と同じ時計回りになる。
		nx, ny := -dy/n*hw, dx/n*hw
		z.MoveTo(pt(l.x1-nx, l.y1-ny))
		z.LineTo(pt(l.x2-nx, l.y2-ny))
		z.LineTo(pt(l.x2+nx, l.y2+ny))
		z.LineTo(pt(l.x1+nx, l.y1+ny))
		z.ClosePath()
	}
	circlePath(z, pt, l.x1, l.y1, hw*scale)
	circlePath(z, pt, l.x2, l.y2, hw*scale)
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
