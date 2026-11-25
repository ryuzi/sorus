package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gen2brain/webp"
)

// EXIF/テキストチャンクに埋め込む目印。出力にこれが残っていればメタデータが漏れている。
const (
	secretMake     = "SECRET-CAMERA-MAKER"
	secretPNGText  = "SECRET-PNG-LOCATION-35.6812N"
	gpsLatitudeRef = "N"
)

// --- テスト用画像の生成 ---

// gradientImage は圧縮後も中身のある絵柄の w×h 画像を返す。
func gradientImage(w, h int) *stdimage.NRGBA {
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 255 / w), uint8(y * 255 / h), uint8((x + y) % 256), 255})
		}
	}
	return img
}

// buildExif は Make・Orientation・GPS 位置情報 (北緯35度41分22.32秒 / 東経139度46分0.48秒) を含む
// リトルエンディアンの TIFF 形式 EXIF ブロックを返す。
func buildExif(orientation uint16) []byte {
	le := binary.LittleEndian
	var b bytes.Buffer
	w16 := func(v uint16) { binary.Write(&b, le, v) }
	w32 := func(v uint32) { binary.Write(&b, le, v) }
	entry := func(tag, typ uint16, count, value uint32) { w16(tag); w16(typ); w32(count); w32(value) }

	makeStr := secretMake + "\x00"
	const ifd0 = 8
	const ifd0Size = 2 + 3*12 + 4
	makeOff := uint32(ifd0 + ifd0Size)
	gpsOff := makeOff + uint32(len(makeStr))
	if gpsOff%2 == 1 {
		gpsOff++
	}
	const gpsSize = 2 + 4*12 + 4
	latOff := gpsOff + gpsSize
	lonOff := latOff + 24

	b.WriteString("II")
	w16(42)
	w32(ifd0)
	// IFD0
	w16(3)
	entry(0x010F, 2, uint32(len(makeStr)), makeOff) // Make (ASCII)
	entry(0x0112, 3, 1, uint32(orientation))        // Orientation (SHORT)
	entry(0x8825, 4, 1, gpsOff)                     // GPS IFD ポインタ
	w32(0)
	b.WriteString(makeStr)
	for uint32(b.Len()) < gpsOff {
		b.WriteByte(0)
	}
	// GPS IFD
	w16(4)
	entry(0x0001, 2, 2, uint32(gpsLatitudeRef[0])) // GPSLatitudeRef "N"
	entry(0x0002, 5, 3, latOff)                    // GPSLatitude
	entry(0x0003, 2, 2, uint32('E'))               // GPSLongitudeRef "E"
	entry(0x0004, 5, 3, lonOff)                    // GPSLongitude
	w32(0)
	for _, r := range [][2]uint32{{35, 1}, {41, 1}, {2232, 100}, {139, 1}, {46, 1}, {48, 100}} {
		w32(r[0])
		w32(r[1])
	}
	return b.Bytes()
}

// jpegWithExif は w×h の JPEG の SOI 直後に EXIF (APP1) セグメントを差し込んで返す。
func jpegWithExif(t *testing.T, img stdimage.Image, orientation uint16) []byte {
	t.Helper()
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("Exif\x00\x00"), buildExif(orientation)...)
	var out bytes.Buffer
	out.Write(enc.Bytes()[:2]) // SOI
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(enc.Bytes()[2:])
	return out.Bytes()
}

// pngChunk は CRC 付きの PNG チャンクを返す。
func pngChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

// pngWithMetadata は eXIf (GPS 入り EXIF) と tEXt チャンクを IEND の直前に差し込んだ PNG を返す。
func pngWithMetadata(t *testing.T, img stdimage.Image) []byte {
	t.Helper()
	var enc bytes.Buffer
	if err := png.Encode(&enc, img); err != nil {
		t.Fatal(err)
	}
	raw := enc.Bytes()
	iend := len(raw) - 12
	var out bytes.Buffer
	out.Write(raw[:iend])
	out.Write(pngChunk("eXIf", buildExif(1)))
	out.Write(pngChunk("tEXt", []byte("Location\x00"+secretPNGText)))
	out.Write(raw[iend:])
	return out.Bytes()
}

func process(t *testing.T, data []byte, filename string) (*ProcessedImage, string) {
	t.Helper()
	dir := t.TempDir()
	res, err := Process(bytes.NewReader(data), filename, ProcessOptions{OutputDir: dir})
	if err != nil {
		t.Fatalf("Process(%s) error: %v", filename, err)
	}
	return res, dir
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// assertWebP はファイルが指定寸法の WebP であることを検証し、その中身を返す。
func assertWebP(t *testing.T, path string, wantW, wantH int) []byte {
	t.Helper()
	data := readFile(t, path)
	if len(data) == 0 {
		t.Fatalf("%s is empty", path)
	}
	if DetectMIME(data) != MimeWebP {
		t.Fatalf("%s is not WebP: header %q", path, data[:min(len(data), 16)])
	}
	if got := binary.LittleEndian.Uint32(data[4:8]); int(got) != len(data)-8 {
		t.Errorf("%s RIFF size = %d, want %d", path, got, len(data)-8)
	}
	cfg, err := webp.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s DecodeConfig: %v", path, err)
	}
	if cfg.Width != wantW || cfg.Height != wantH {
		t.Errorf("%s size = %dx%d, want %dx%d", path, cfg.Width, cfg.Height, wantW, wantH)
	}
	if _, err := webp.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("%s Decode: %v", path, err)
	}
	return data
}

// assertNoMetadata は WebP の RIFF チャンクを走査し、画像データ以外 (EXIF, XMP, ICCP) がないことを検証する。
func assertNoMetadata(t *testing.T, path string, data []byte) {
	t.Helper()
	for _, secret := range []string{"Exif", secretMake, secretPNGText} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("%s contains metadata marker %q", path, secret)
		}
	}
	if _, err := webp.DecodeExif(bytes.NewReader(data)); !errors.Is(err, webp.ErrNoExif) {
		t.Errorf("%s DecodeExif error = %v, want ErrNoExif", path, err)
	}
	allowed := map[string]bool{"VP8 ": true, "VP8L": true, "VP8X": true, "ALPH": true}
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if !allowed[id] {
			t.Errorf("%s has unexpected chunk %q", path, id)
		}
		if id == "VP8X" && pos+9 <= len(data) {
			// フラグ: ICC=0x20, EXIF=0x08, XMP=0x04
			if flags := data[pos+8]; flags&(0x20|0x08|0x04) != 0 {
				t.Errorf("%s VP8X flags = %#x, metadata bits set", path, flags)
			}
		}
		pos += 8 + size + size%2
	}
}

// --- 変換・リサイズ ---

func TestProcess_JPEGToThreeWebPSizes(t *testing.T) {
	t.Parallel()
	src := jpegWithExif(t, gradientImage(1600, 1000), 1)
	res, dir := process(t, src, "photo.JPG")

	if res.MimeType != MimeJPEG || res.Width != 1600 || res.Height != 1000 {
		t.Errorf("got mime=%s %dx%d, want image/jpeg 1600x1000", res.MimeType, res.Width, res.Height)
	}
	assertWebP(t, res.OriginalPath, 1600, 1000)
	large := assertWebP(t, res.LargePath, 1200, 750)
	medium := assertWebP(t, res.MediumPath, 800, 500)
	small := assertWebP(t, res.SmallPath, 400, 250)
	if !(len(small) < len(medium) && len(medium) < len(large)) {
		t.Errorf("file sizes not increasing with resolution: small=%d medium=%d large=%d", len(small), len(medium), len(large))
	}

	for _, p := range []string{res.OriginalPath, res.LargePath, res.MediumPath, res.SmallPath} {
		if !strings.HasPrefix(p, filepath.ToSlash(dir)+"/") || !strings.HasSuffix(p, ".webp") {
			t.Errorf("path %q is not a .webp under %s", p, dir)
		}
	}
	for suffix, p := range map[string]string{"-1200.webp": res.LargePath, "-800.webp": res.MediumPath, "-400.webp": res.SmallPath} {
		if !strings.HasSuffix(p, suffix) {
			t.Errorf("path %q does not end with %s", p, suffix)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Errorf("output dir has %d files, want 4 (no temp files left)", len(entries))
	}
}

func TestProcess_PNGToThreeWebPSizes(t *testing.T) {
	t.Parallel()
	res, _ := process(t, pngWithMetadata(t, gradientImage(2400, 1200)), "diagram.png")
	if res.MimeType != MimePNG || res.Width != 2400 || res.Height != 1200 {
		t.Errorf("got mime=%s %dx%d, want image/png 2400x1200", res.MimeType, res.Width, res.Height)
	}
	assertWebP(t, res.LargePath, 1200, 600)
	assertWebP(t, res.MediumPath, 800, 400)
	assertWebP(t, res.SmallPath, 400, 200)
}

func TestProcess_PNGKeepsTransparency(t *testing.T) {
	t.Parallel()
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, 900, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 900; x++ {
			if x >= 450 {
				img.Set(x, y, color.NRGBA{255, 0, 0, 255})
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	res, _ := process(t, buf.Bytes(), "logo.png")

	data := assertWebP(t, res.MediumPath, 800, 267)
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := decoded.At(10, 100).RGBA(); a != 0 {
		t.Errorf("transparent area alpha = %d, want 0", a)
	}
	if r, _, _, a := decoded.At(700, 100).RGBA(); a != 0xffff || r < 0xc000 {
		t.Errorf("opaque red area = r%d a%d, want opaque red", r, a)
	}
}

func TestProcess_WebPInput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := webp.Encode(&buf, gradientImage(1000, 500)); err != nil {
		t.Fatal(err)
	}
	res, _ := process(t, buf.Bytes(), "already.webp")
	if res.MimeType != MimeWebP {
		t.Errorf("mime = %s, want image/webp", res.MimeType)
	}
	assertWebP(t, res.LargePath, 1000, 500) // 拡大しない
	assertWebP(t, res.MediumPath, 800, 400)
	assertWebP(t, res.SmallPath, 400, 200)
}

func TestProcess_SmallImageIsNotUpscaled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	png.Encode(&buf, gradientImage(300, 200))
	res, _ := process(t, buf.Bytes(), "icon.png")
	assertWebP(t, res.LargePath, 300, 200)
	assertWebP(t, res.MediumPath, 300, 200)
	assertWebP(t, res.SmallPath, 300, 200)
}

func TestProcess_FilenameWithoutExtensionUsesContent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	png.Encode(&buf, gradientImage(50, 50))
	res, _ := process(t, buf.Bytes(), "blob")
	if res.MimeType != MimePNG {
		t.Errorf("mime = %s, want image/png", res.MimeType)
	}
}

func TestProcess_CreatesOutputDir(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	png.Encode(&buf, gradientImage(50, 50))
	dir := filepath.Join(t.TempDir(), "static", "uploads")
	res, err := Process(&buf, "a.png", ProcessOptions{OutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	assertWebP(t, res.SmallPath, 50, 50)
}

func TestProcess_NoOutputDir(t *testing.T) {
	t.Parallel()
	_, err := Process(strings.NewReader("x"), "a.png", ProcessOptions{})
	if !errors.Is(err, ErrNoOutputDir) {
		t.Errorf("error = %v, want ErrNoOutputDir", err)
	}
}

// --- EXIF 破棄 ---

func TestProcess_StripsJPEGExifAndGPS(t *testing.T) {
	t.Parallel()
	src := jpegWithExif(t, gradientImage(1600, 1000), 1)
	// 前提: 入力には EXIF と GPS が入っている
	if !bytes.Contains(src, []byte("Exif\x00\x00")) || !bytes.Contains(src, []byte(secretMake)) {
		t.Fatal("test fixture lacks EXIF")
	}
	if got := jpegOrientation(src); got != 1 {
		t.Fatalf("fixture orientation = %d", got)
	}

	res, _ := process(t, src, "gps.jpg")
	for _, p := range []string{res.OriginalPath, res.LargePath, res.MediumPath, res.SmallPath} {
		assertNoMetadata(t, p, readFile(t, p))
	}
}

func TestProcess_StripsPNGMetadataChunks(t *testing.T) {
	t.Parallel()
	src := pngWithMetadata(t, gradientImage(500, 500))
	if !bytes.Contains(src, []byte("eXIf")) || !bytes.Contains(src, []byte(secretPNGText)) {
		t.Fatal("test fixture lacks metadata")
	}
	res, _ := process(t, src, "meta.png")
	for _, p := range []string{res.OriginalPath, res.LargePath, res.MediumPath, res.SmallPath} {
		assertNoMetadata(t, p, readFile(t, p))
	}
}

// --- EXIF の向き ---

func TestProcess_AppliesJPEGOrientation(t *testing.T) {
	t.Parallel()
	// 横長 60x30 の左半分を赤、右半分を青にする。Orientation=6 (時計回り90度) なら正立後は縦長で上が赤になる。
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 60, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 60; x++ {
			c := color.RGBA{0, 0, 255, 255}
			if x < 30 {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	res, _ := process(t, jpegWithExif(t, img, 6), "portrait.jpg")
	if res.Width != 30 || res.Height != 60 {
		t.Fatalf("size = %dx%d, want 30x60", res.Width, res.Height)
	}
	decoded, err := webp.Decode(bytes.NewReader(assertWebP(t, res.OriginalPath, 30, 60)))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, b, _ := decoded.At(15, 10).RGBA(); r < 0xc000 || b > 0x4000 {
		t.Errorf("top should be red, got r=%#x b=%#x", r, b)
	}
	if r, _, b, _ := decoded.At(15, 50).RGBA(); b < 0xc000 || r > 0x4000 {
		t.Errorf("bottom should be blue, got r=%#x b=%#x", r, b)
	}
}

func TestApplyOrientation_AllEight(t *testing.T) {
	// 2x3 の画像の各画素に番号を振り、変換後の並びを確認する。
	src := stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 3))
	for i := 0; i < 6; i++ {
		src.Pix[i*4] = uint8(i + 1)
	}
	// 元画像:
	// 1 2
	// 3 4
	// 5 6
	tests := map[int][]uint8{
		1: {1, 2, 3, 4, 5, 6},
		2: {2, 1, 4, 3, 6, 5},
		3: {6, 5, 4, 3, 2, 1},
		4: {5, 6, 3, 4, 1, 2},
		5: {1, 3, 5, 2, 4, 6},
		6: {5, 3, 1, 6, 4, 2},
		7: {6, 4, 2, 5, 3, 1},
		8: {2, 4, 6, 1, 3, 5},
	}
	for o, want := range tests {
		dst := applyOrientation(src, o)
		var got []uint8
		for i := 0; i < len(dst.Pix); i += 4 {
			got = append(got, dst.Pix[i])
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("orientation %d: got %v, want %v", o, got, want)
		}
		if wantW := map[bool]int{true: 3, false: 2}[o >= 5]; dst.Bounds().Dx() != wantW {
			t.Errorf("orientation %d: width = %d, want %d", o, dst.Bounds().Dx(), wantW)
		}
	}
}

func TestJPEGOrientation_MalformedExifIsIgnored(t *testing.T) {
	cases := map[string][]byte{
		"no segments":       {0xFF, 0xD8},
		"truncated segment": {0xFF, 0xD8, 0xFF, 0xE1, 0x10, 0x00, 'E'},
		"bad tiff header":   {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10, 'E', 'x', 'i', 'f', 0, 0, 'X', 'X', 0, 0, 0, 0, 0, 0},
		"ifd out of range":  append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10}, append([]byte("Exif\x00\x00II*\x00"), 0xFF, 0xFF, 0, 0)...),
	}
	for name, data := range cases {
		if got := jpegOrientation(data); got != 1 {
			t.Errorf("%s: orientation = %d, want 1", name, got)
		}
	}
}

// --- MIME 詐称・不正入力 ---

func TestProcess_RejectsInvalidInput(t *testing.T) {
	t.Parallel()
	var validPNG bytes.Buffer
	png.Encode(&validPNG, gradientImage(20, 20))
	validJPEG := jpegWithExif(t, gradientImage(20, 20), 1)

	cases := []struct {
		name     string
		data     []byte
		filename string
		want     error
	}{
		{"text disguised as jpg", []byte("Hello, this is not an image.\n"), "photo.jpg", ErrUnsupportedType},
		{"html disguised as png", []byte("<script>alert(1)</script>"), "image.png", ErrUnsupportedType},
		{"php disguised as webp", []byte("<?php system($_GET['c']); ?>"), "x.webp", ErrUnsupportedType},
		{"empty", nil, "empty.jpg", ErrUnsupportedType},
		{"random binary", []byte{0x00, 0x01, 0x02, 0x03, 0xDE, 0xAD, 0xBE, 0xEF}, "a.png", ErrUnsupportedType},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), "anim.gif", ErrUnsupportedType},
		{"jpeg magic then garbage", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x41}, 200)...), "broken.jpg", ErrCorruptImage},
		{"truncated jpeg", validJPEG[:len(validJPEG)/2], "half.jpg", ErrCorruptImage},
		{"truncated png", validPNG.Bytes()[:40], "half.png", ErrCorruptImage},
		{"riff without webp payload", []byte("RIFF\x10\x00\x00\x00WEBPVP8 \x00\x00\x00\x00"), "broken.webp", ErrCorruptImage},
		{"png named jpg", validPNG.Bytes(), "photo.jpg", ErrMIMEMismatch},
		{"jpeg named php", validJPEG, "shell.php", ErrMIMEMismatch},
		{"jpeg named svg", validJPEG, "image.svg", ErrMIMEMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			res, err := Process(bytes.NewReader(c.data), c.filename, ProcessOptions{OutputDir: dir})
			if !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
			if res != nil {
				t.Errorf("result = %+v, want nil", res)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("output dir has %d files after failure, want 0", len(entries))
			}
		})
	}
}

func TestProcess_RejectsOversizedUpload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	png.Encode(&buf, gradientImage(200, 200))
	_, err := Process(bytes.NewReader(buf.Bytes()), "a.png", ProcessOptions{OutputDir: t.TempDir(), MaxUploadSize: int64(buf.Len() - 1)})
	if !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("error = %v, want ErrFileTooLarge", err)
	}
	// ちょうど上限なら通る
	if _, err := Process(bytes.NewReader(buf.Bytes()), "a.png", ProcessOptions{OutputDir: t.TempDir(), MaxUploadSize: int64(buf.Len())}); err != nil {
		t.Errorf("upload at exact limit: %v", err)
	}
}

func TestProcess_DefaultLimitIs15MB(t *testing.T) {
	t.Parallel()
	// JPEG のマジックナンバーで始まる 15MB+1 バイト。既定の上限で拒否される。
	r := io.MultiReader(bytes.NewReader([]byte{0xFF, 0xD8, 0xFF}), io.LimitReader(zeroReader{}, DefaultMaxUploadSize-2))
	_, err := Process(r, "huge.jpg", ProcessOptions{OutputDir: t.TempDir()})
	if !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("error = %v, want ErrFileTooLarge", err)
	}
	if DefaultMaxUploadSize != 15*1024*1024 {
		t.Errorf("DefaultMaxUploadSize = %d", DefaultMaxUploadSize)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// headerOnlyPNG は IHDR で w x h を名乗るだけで画素データを持たない PNG を返す。
func headerOnlyPNG(w, h uint32) *bytes.Buffer {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8bit RGBA
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	b.Write(pngChunk("IHDR", ihdr))
	b.Write(pngChunk("IEND", nil))
	return &b
}

func TestProcess_RejectsDecompressionBomb(t *testing.T) {
	t.Parallel()
	// 100000x100000 を名乗るだけの小さな PNG。デコード前に寸法で拒否する。
	_, err := Process(headerOnlyPNG(100000, 100000), "bomb.png", ProcessOptions{OutputDir: t.TempDir()})
	if !errors.Is(err, ErrTooManyPixels) {
		t.Errorf("error = %v, want ErrTooManyPixels", err)
	}
}

func TestProcess_PixelLimitIs25MP(t *testing.T) {
	t.Parallel()
	if MaxPixels != 25_000_000 {
		t.Fatalf("MaxPixels = %d, want 25000000", MaxPixels)
	}
	// 6000x4000 (2400万画素) と 5000x5000 (ちょうど上限) は寸法検査を通過し、画素データがないため破損として扱われる。
	for _, size := range [][2]uint32{{6000, 4000}, {5000, 5000}} {
		_, err := Process(headerOnlyPNG(size[0], size[1]), "ok.png", ProcessOptions{OutputDir: t.TempDir()})
		if !errors.Is(err, ErrCorruptImage) {
			t.Errorf("%dx%d: error = %v, want ErrCorruptImage (within pixel limit)", size[0], size[1], err)
		}
	}
	// 上限を 1 行でも超えれば拒否する。
	for _, size := range [][2]uint32{{5000, 5001}, {6001, 4200}} {
		_, err := Process(headerOnlyPNG(size[0], size[1]), "big.png", ProcessOptions{OutputDir: t.TempDir()})
		if !errors.Is(err, ErrTooManyPixels) {
			t.Errorf("%dx%d: error = %v, want ErrTooManyPixels", size[0], size[1], err)
		}
	}
}

func TestDetectMIME(t *testing.T) {
	cases := map[string]string{
		"\xFF\xD8\xFF\xE0rest":         MimeJPEG,
		"\x89PNG\r\n\x1a\nrest":        MimePNG,
		"RIFF\x00\x00\x00\x00WEBPVP8":  MimeWebP,
		"RIFF\x00\x00\x00\x00AVI LIST": "",
		"\xFF\xD8":                     "",
		"GIF89a":                       "",
		"BM":                           "",
		"<svg xmlns=":                  "",
		"":                             "",
	}
	for in, want := range cases {
		if got := DetectMIME([]byte(in)); got != want {
			t.Errorf("DetectMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- 並行実行 ---

func TestProcess_Concurrent(t *testing.T) {
	t.Parallel()
	src := jpegWithExif(t, gradientImage(900, 600), 1)
	dir := t.TempDir()
	const n = 4
	var wg sync.WaitGroup
	results := make([]*ProcessedImage, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = Process(bytes.NewReader(src), "same.jpg", ProcessOptions{OutputDir: dir})
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if seen[results[i].LargePath] {
			t.Errorf("duplicate output path %s", results[i].LargePath)
		}
		seen[results[i].LargePath] = true
		assertWebP(t, results[i].SmallPath, 400, 267)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != n*4 {
		t.Errorf("output dir has %d files, want %d", len(entries), n*4)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	cases := map[int]int{0: 2, 1: 2, 2: 4, 3: 6, 4: 8, 16: 8, 128: 8}
	for cpus, want := range cases {
		if got := concurrencyLimit(cpus); got != want {
			t.Errorf("concurrencyLimit(%d) = %d, want %d", cpus, got, want)
		}
	}
	if got, want := cap(processSlots), concurrencyLimit(runtime.NumCPU()); got != want {
		t.Errorf("cap(processSlots) = %d, want %d", got, want)
	}
}

// t.Parallel にしない: セマフォを占有するため、並列テストと同時に走らせない。
func TestProcess_WaitsForFreeSlot(t *testing.T) {
	src := jpegWithExif(t, gradientImage(64, 48), 1)
	for i := 0; i < cap(processSlots); i++ {
		processSlots <- struct{}{}
	}
	released := 0
	defer func() {
		for ; released < cap(processSlots); released++ {
			<-processSlots
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := Process(bytes.NewReader(src), "wait.jpg", ProcessOptions{OutputDir: t.TempDir()})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Process finished while all slots were taken (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	<-processSlots
	released++
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Process did not proceed after a slot was released")
	}
	if n := len(processSlots); n != cap(processSlots)-1 {
		t.Errorf("slots in use after Process = %d, want %d (slot must be released)", n, cap(processSlots)-1)
	}
}

func TestProcess_ReleasesSlotOnError(t *testing.T) {
	// 寸法超過で失敗しても枠を返却する（漏れると以降のアップロードが永久に待つ）。
	for i := 0; i < cap(processSlots)+2; i++ {
		_, err := Process(headerOnlyPNG(100000, 100000), "bomb.png", ProcessOptions{OutputDir: t.TempDir()})
		if !errors.Is(err, ErrTooManyPixels) {
			t.Fatalf("error = %v, want ErrTooManyPixels", err)
		}
	}
	if n := len(processSlots); n != 0 {
		t.Errorf("slots in use = %d, want 0", n)
	}
}

// --- srcset ヘルパー ---

func TestRenderResponsiveImg(t *testing.T) {
	img := &ProcessedImage{
		LargePath:  "static/uploads/abc-1200.webp",
		MediumPath: "static/uploads/abc-800.webp",
		SmallPath:  "static/uploads/abc-400.webp",
		Width:      1600,
		Height:     1000,
	}
	got := RenderResponsiveImg(img, "サークル集合写真", "featured")
	want := `<img src="/static/uploads/abc-800.webp"` +
		` srcset="/static/uploads/abc-400.webp 400w, /static/uploads/abc-800.webp 800w, /static/uploads/abc-1200.webp 1200w"` +
		` sizes="(max-width: 1200px) 100vw, 1200px" width="1200" height="750"` +
		` alt="サークル集合写真" class="featured" loading="lazy" decoding="async">`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRenderResponsiveImg_FromProcessResult(t *testing.T) {
	t.Parallel()
	res, _ := process(t, jpegWithExif(t, gradientImage(1600, 1000), 1), "p.jpg")
	got := RenderResponsiveImg(res, "alt", "")
	for _, p := range []string{res.SmallPath + " 400w", res.MediumPath + " 800w", res.LargePath + " 1200w"} {
		if !strings.Contains(got, "/"+strings.TrimPrefix(p, "/")) {
			t.Errorf("output lacks %q:\n%s", p, got)
		}
	}
	if strings.Contains(got, "class=") {
		t.Errorf("empty className should omit class attribute: %s", got)
	}
}

func TestRenderResponsiveImg_EscapesAttributes(t *testing.T) {
	img := &ProcessedImage{
		LargePath:  `up loads/"x"-1200.webp`,
		MediumPath: `up loads/"x"-800.webp`,
		SmallPath:  `up loads/"x"-400.webp`,
		Width:      2000,
		Height:     1000,
	}
	got := RenderResponsiveImg(img, `"><script>alert(1)</script>`, `a" onerror="alert(1)`)
	for _, bad := range []string{"<script>", `" onerror="`, `"x"`, "up loads"} {
		if strings.Contains(got, bad) {
			t.Errorf("output contains unescaped %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, `alt="&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`) {
		t.Errorf("alt not escaped as expected:\n%s", got)
	}
	if !strings.Contains(got, "/up%20loads/") {
		t.Errorf("space in path should be percent-encoded:\n%s", got)
	}
}

func TestRenderResponsiveImg_SmallImageDedupesWidths(t *testing.T) {
	img := &ProcessedImage{LargePath: "u/a-1200.webp", MediumPath: "u/a-800.webp", SmallPath: "u/a-400.webp", Width: 600, Height: 300}
	got := RenderResponsiveImg(img, "", "")
	if !strings.Contains(got, `srcset="/u/a-400.webp 400w, /u/a-800.webp 600w"`) {
		t.Errorf("unexpected srcset:\n%s", got)
	}
	if !strings.Contains(got, `width="600" height="300"`) || !strings.Contains(got, `alt=""`) {
		t.Errorf("unexpected attributes:\n%s", got)
	}
}

func TestRenderResponsiveImg_Empty(t *testing.T) {
	if got := RenderResponsiveImg(nil, "a", "b"); got != "" {
		t.Errorf("nil image: got %q", got)
	}
	if got := RenderResponsiveImg(&ProcessedImage{Width: 10, Height: 10}, "a", ""); got != "" {
		t.Errorf("image without paths: got %q", got)
	}
}
