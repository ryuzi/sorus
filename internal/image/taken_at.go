package image

import (
	"bytes"
	"encoding/binary"
	"time"
)

// TakenAtLayout は media.taken_at の書式。created_at（CURRENT_TIMESTAMP）と同じ UTC 文字列にそろえる。
const TakenAtLayout = "2006-01-02 15:04:05"

// TakenAtValue は media.taken_at に書く値を返す。EXIF の撮影日時（exif）があればそれ、無ければ fallback を UTC の文字列にする。
func TakenAtValue(exif, fallback time.Time) string {
	if exif.IsZero() {
		exif = fallback
	}
	return exif.UTC().Format(TakenAtLayout)
}

// exifDateLayout は EXIF の日時（DateTimeOriginal など）の書式。タイムゾーンは含まない。
const exifDateLayout = "2006:01:02 15:04:05"

// jpegTakenAt は JPEG の EXIF から撮影日時（DateTimeOriginal）を読み取る。EXIF の日時にはタイムゾーンが無いため、
// loc（撮影した場所の時刻とみなす。nil ならこのPCの時刻）の時刻として解釈する。無い・壊れている場合は ok=false。
func jpegTakenAt(data []byte, loc *time.Location) (time.Time, bool) {
	tiff := jpegExif(data)
	if tiff == nil {
		return time.Time{}, false
	}
	return tiffTakenAt(tiff, loc)
}

// tiffTakenAt は TIFF 形式の EXIF ブロックの Exif IFD（IFD0 の ExifIFDPointer の先）から DateTimeOriginal を探す。
func tiffTakenAt(tiff []byte, loc *time.Location) (time.Time, bool) {
	const (
		tagExifIFD          = 0x8769
		tagDateTimeOriginal = 0x9003
		typeASCII           = 2
		typeLong            = 4
	)
	if len(tiff) < 8 {
		return time.Time{}, false
	}
	var order binary.ByteOrder
	switch string(tiff[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return time.Time{}, false
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return time.Time{}, false
	}
	// findEntry は offset の IFD から tag のエントリ（12 バイト）を返す。
	findEntry := func(offset int, tag uint16) []byte {
		if offset < 8 || offset+2 > len(tiff) {
			return nil
		}
		count := int(order.Uint16(tiff[offset : offset+2]))
		for i := 0; i < count; i++ {
			entry := offset + 2 + i*12
			if entry+12 > len(tiff) {
				return nil
			}
			if order.Uint16(tiff[entry:entry+2]) == tag {
				return tiff[entry : entry+12]
			}
		}
		return nil
	}
	ptr := findEntry(int(order.Uint32(tiff[4:8])), tagExifIFD)
	if ptr == nil || order.Uint16(ptr[2:4]) != typeLong {
		return time.Time{}, false
	}
	entry := findEntry(int(order.Uint32(ptr[8:12])), tagDateTimeOriginal)
	if entry == nil || order.Uint16(entry[2:4]) != typeASCII {
		return time.Time{}, false
	}
	// "YYYY:MM:DD HH:MM:SS\0" の 20 バイト。4 バイトを超える値は値フィールドが値の場所（オフセット）を指す。
	n := int(order.Uint32(entry[4:8]))
	if n < len(exifDateLayout) || n > 64 {
		return time.Time{}, false
	}
	start := int(order.Uint32(entry[8:12]))
	if start < 0 || start+n > len(tiff) {
		return time.Time{}, false
	}
	value := bytes.TrimRight(tiff[start:start+n], "\x00 ")
	if loc == nil {
		loc = time.Local
	}
	t, err := time.ParseInLocation(exifDateLayout, string(value), loc)
	// 未設定のカメラは "0000:00:00 00:00:00" を書くため、解釈できない値は無いものとして扱う。
	if err != nil || t.Year() < 1900 {
		return time.Time{}, false
	}
	return t, true
}
