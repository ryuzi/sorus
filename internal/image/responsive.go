package image

import (
	"fmt"
	"html"
	stdimage "image"
	"net/url"
	"path"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// 生成する WebP の横幅 (px)。
const (
	WidthLarge  = 1200
	WidthMedium = 800
	WidthSmall  = 400
)

// scaledSize は横幅 target に縮小したときの寸法を返す。元画像より大きくは拡大しない。
func scaledSize(width, height, target int) (int, int) {
	if width <= target {
		return width, height
	}
	h := (height*target + width/2) / width // 四捨五入
	if h < 1 {
		h = 1
	}
	return target, h
}

// resizeToWidth は横幅 target に縮小した新しい画像を返す。元画像が target 以下ならそのまま返す。
func resizeToWidth(src *stdimage.RGBA, target int) *stdimage.RGBA {
	b := src.Bounds()
	w, h := scaledSize(b.Dx(), b.Dy(), target)
	if w == b.Dx() && h == b.Dy() {
		return src
	}
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	return dst
}

// RenderResponsiveImg は srcset を含むレスポンシブな <img> HTML タグ文字列を生成する。
// 属性値はすべて HTML エスケープする。img が nil または未生成なら空文字を返す。
func RenderResponsiveImg(img *ProcessedImage, alt string, className string) string {
	if img == nil || img.LargePath == "" || img.MediumPath == "" || img.SmallPath == "" || img.Width <= 0 || img.Height <= 0 {
		return ""
	}

	variants := []struct {
		path  string
		width int
	}{
		{img.SmallPath, WidthSmall},
		{img.MediumPath, WidthMedium},
		{img.LargePath, WidthLarge},
	}
	// 元画像が小さいと拡大しないため複数サイズが同じ横幅になる。同じ幅は最初の 1 つだけ載せる。
	var srcset []string
	seen := map[int]bool{}
	for _, v := range variants {
		w, _ := scaledSize(img.Width, img.Height, v.width)
		if seen[w] {
			continue
		}
		seen[w] = true
		srcset = append(srcset, fmt.Sprintf("%s %dw", imageURL(v.path), w))
	}

	largeW, largeH := scaledSize(img.Width, img.Height, WidthLarge)
	var b strings.Builder
	fmt.Fprintf(&b, `<img src="%s"`, html.EscapeString(imageURL(img.MediumPath)))
	fmt.Fprintf(&b, ` srcset="%s"`, html.EscapeString(strings.Join(srcset, ", ")))
	fmt.Fprintf(&b, ` sizes="(max-width: %dpx) 100vw, %dpx"`, largeW, largeW)
	fmt.Fprintf(&b, ` width="%d" height="%d"`, largeW, largeH)
	fmt.Fprintf(&b, ` alt="%s"`, html.EscapeString(alt))
	if className != "" {
		fmt.Fprintf(&b, ` class="%s"`, html.EscapeString(className))
	}
	b.WriteString(` loading="lazy" decoding="async">`)
	return b.String()
}

// imageURL は保存パス (例: "static/uploads/x.webp") をサイトルートからの URL パスに変換する。
// 空白等はパーセントエンコードし、srcset の区切りと衝突しないようにする。
func imageURL(p string) string {
	p = path.Clean("/" + strings.TrimPrefix(p, "./"))
	return (&url.URL{Path: p}).EscapedPath()
}
