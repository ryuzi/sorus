package image

import (
	"bytes"
	"encoding/binary"
	stdimage "image"
)

// jpegOrientation は JPEG の EXIF (APP1) から Orientation タグ (1-8) を読み取る。
// EXIF は破棄するため、スマートフォンで縦向きに撮った写真が横倒しにならないよう向きだけを先に取り出す。
// 見つからない・壊れている場合は 1（回転なし）を返す。
func jpegOrientation(data []byte) int {
	tiff := jpegExif(data)
	if tiff == nil {
		return 1
	}
	return tiffOrientation(tiff)
}

// jpegExif は JPEG の EXIF (APP1) セグメントの TIFF 形式のブロックを返す。無い・壊れている場合は nil。
func jpegExif(data []byte) []byte {
	const (
		markerAPP1 = 0xE1
		markerSOS  = 0xDA
		markerEOI  = 0xD9
	)
	pos := 2 // SOI の直後
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			return nil
		}
		marker := data[pos+1]
		if marker == 0xFF { // フィルバイト
			pos++
			continue
		}
		if marker == markerSOS || marker == markerEOI {
			return nil
		}
		size := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if size < 2 || pos+2+size > len(data) {
			return nil
		}
		payload := data[pos+4 : pos+2+size]
		if marker == markerAPP1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			return payload[6:]
		}
		pos += 2 + size
	}
	return nil
}

// tiffOrientation は TIFF 形式の EXIF ブロックの IFD0 から Orientation タグを探す。
func tiffOrientation(tiff []byte) int {
	const tagOrientation = 0x0112
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[ifd : ifd+2]))
	for i := 0; i < count; i++ {
		entry := ifd + 2 + i*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:entry+2]) != tagOrientation {
			continue
		}
		// 型 SHORT (3) の値は値フィールドの先頭 2 バイトに格納される。
		if order.Uint16(tiff[entry+2:entry+4]) != 3 {
			return 1
		}
		if v := int(order.Uint16(tiff[entry+8 : entry+10])); v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}

// applyOrientation は EXIF Orientation に従って画像を正立させる。
func applyOrientation(src *stdimage.RGBA, orientation int) *stdimage.RGBA {
	if orientation < 2 || orientation > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if orientation >= 5 { // 5-8 は縦横が入れ替わる
		dw, dh = h, w
	}
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, dw, dh))

	// 出力座標 (x, y) に対応する元画像の座標を返す。
	var from func(x, y int) (int, int)
	switch orientation {
	case 2: // 左右反転
		from = func(x, y int) (int, int) { return w - 1 - x, y }
	case 3: // 180度回転
		from = func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }
	case 4: // 上下反転
		from = func(x, y int) (int, int) { return x, h - 1 - y }
	case 5: // 転置
		from = func(x, y int) (int, int) { return y, x }
	case 6: // 時計回りに90度回転
		from = func(x, y int) (int, int) { return y, h - 1 - x }
	case 7: // 反転置
		from = func(x, y int) (int, int) { return w - 1 - y, h - 1 - x }
	case 8: // 反時計回りに90度回転
		from = func(x, y int) (int, int) { return w - 1 - y, x }
	}

	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			sx, sy := from(x, y)
			copy(dst.Pix[dst.PixOffset(x, y):dst.PixOffset(x, y)+4], src.Pix[src.PixOffset(sx, sy):src.PixOffset(sx, sy)+4])
		}
	}
	return dst
}
