package image

import (
	"bytes"
	"encoding/binary"
	stdimage "image"
	"image/jpeg"
	"testing"
	"time"
)

// buildExifWithDate は IFD0 に ExifIFDPointer を、Exif IFD に DateTimeOriginal（date）を持つ
// order のバイト順の TIFF 形式 EXIF ブロックを返す。
func buildExifWithDate(order binary.ByteOrder, date string) []byte {
	var b bytes.Buffer
	if order == binary.LittleEndian {
		b.WriteString("II")
	} else {
		b.WriteString("MM")
	}
	binary.Write(&b, order, uint16(42))
	binary.Write(&b, order, uint32(8)) // IFD0
	// IFD0: エントリ 1 個（ExifIFDPointer）。8 + 2 + 12 + 4 = 26 から Exif IFD。
	binary.Write(&b, order, uint16(1))
	binary.Write(&b, order, uint16(0x8769))
	binary.Write(&b, order, uint16(4))
	binary.Write(&b, order, uint32(1))
	binary.Write(&b, order, uint32(26))
	binary.Write(&b, order, uint32(0))
	// Exif IFD: エントリ 1 個（DateTimeOriginal）。26 + 2 + 12 + 4 = 44 から値。
	value := append([]byte(date), 0)
	binary.Write(&b, order, uint16(1))
	binary.Write(&b, order, uint16(0x9003))
	binary.Write(&b, order, uint16(2))
	binary.Write(&b, order, uint32(len(value)))
	binary.Write(&b, order, uint32(44))
	binary.Write(&b, order, uint32(0))
	b.Write(value)
	return b.Bytes()
}

// jpegWithExifBlock は JPEG の SOI 直後に tiff を入れた EXIF (APP1) を差し込んで返す。
func jpegWithExifBlock(t *testing.T, tiff []byte) []byte {
	t.Helper()
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, stdimage.NewRGBA(stdimage.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	var out bytes.Buffer
	out.Write(enc.Bytes()[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(enc.Bytes()[2:])
	return out.Bytes()
}

func TestJPEGTakenAt(t *testing.T) {
	t.Parallel()
	jst := time.FixedZone("JST", 9*60*60)
	want := time.Date(2019, 4, 6, 10, 30, 15, 0, jst)
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		got, ok := jpegTakenAt(jpegWithExifBlock(t, buildExifWithDate(order, "2019:04:06 10:30:15")), jst)
		if !ok || !got.Equal(want) {
			t.Errorf("%v: jpegTakenAt = %v, %v; want %v", order, got, ok, want)
		}
	}
	for name, data := range map[string][]byte{
		"no exif":     jpegWithExifBlock(t, nil)[:0],
		"orientation": jpegWithExifBlock(t, buildExif(6)),
		"zero date":   jpegWithExifBlock(t, buildExifWithDate(binary.LittleEndian, "0000:00:00 00:00:00")),
		"bad date":    jpegWithExifBlock(t, buildExifWithDate(binary.LittleEndian, "2019-04-06 10:30:15")),
		"truncated":   jpegWithExifBlock(t, buildExifWithDate(binary.LittleEndian, "2019:04:06 10:30:15")[:40]),
	} {
		if got, ok := jpegTakenAt(data, jst); ok {
			t.Errorf("%s: jpegTakenAt = %v, want none", name, got)
		}
	}
}

// Process は WebP へ変換する前に JPEG の撮影日時を読み、TakenAt に返す（WebP には EXIF を残さない）。
func TestProcess_ReturnsJPEGTakenAt(t *testing.T) {
	t.Parallel()
	res, _ := process(t, jpegWithExifBlock(t, buildExifWithDate(binary.BigEndian, "2021:12:24 18:00:00")), "eve.jpg")
	want := time.Date(2021, 12, 24, 18, 0, 0, 0, time.Local)
	if !res.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", res.TakenAt, want)
	}
	assertNoMetadata(t, res.OriginalPath, readFile(t, res.OriginalPath))

	plain, _ := process(t, jpegWithExifBlock(t, buildExif(1)), "plain.jpg")
	if !plain.TakenAt.IsZero() {
		t.Errorf("TakenAt without DateTimeOriginal = %v, want zero", plain.TakenAt)
	}
}

func TestTakenAtValue(t *testing.T) {
	t.Parallel()
	jst := time.FixedZone("JST", 9*60*60)
	fb := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	exif := time.Date(2019, 6, 7, 17, 9, 10, 0, jst)
	if got := TakenAtValue(exif, fb); got != "2019-06-07 08:09:10" {
		t.Errorf("exif: got %q", got)
	}
	if got := TakenAtValue(time.Time{}, fb); got != "2020-01-02 03:04:05" {
		t.Errorf("fallback: got %q", got)
	}
}
