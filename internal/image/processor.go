// Package image はアップロード画像の防弾処理を行う。
// マジックナンバーで実体の形式を判定し、デコードした画素だけを新しい RGBA 画像に描き直して
// EXIF 等のメタデータを不可逆的に破棄した上で、3サイズの WebP を生成する。
// WebP エンコードは github.com/gen2brain/webp（WASM を Go に変換した実装）を使い、CGO を必要としない。
package image

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	stdimage "image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gen2brain/webp"
)

const (
	// DefaultMaxUploadSize は ProcessOptions.MaxUploadSize 未指定時のアップロード上限 (15MB)。
	DefaultMaxUploadSize int64 = 15 << 20
	// MaxPixels はデコードを許可する最大画素数 (2500万画素、6000x4000 のカメラ画像が収まる)。
	// ヘッダだけ巨大な寸法を名乗る圧縮爆弾でメモリを使い切らないための上限。
	MaxPixels = 25_000_000

	// minConcurrency / maxConcurrency は同時に実行する画像処理数の下限・上限。
	minConcurrency = 2
	maxConcurrency = 8

	// webpQuality は生成する WebP の画質 (0-100)。
	webpQuality = 80
)

const (
	MimeJPEG = "image/jpeg"
	MimePNG  = "image/png"
	MimeWebP = "image/webp"
)

var (
	ErrFileTooLarge    = errors.New("image: file exceeds upload size limit")
	ErrUnsupportedType = errors.New("image: unsupported or unrecognized image type")
	ErrMIMEMismatch    = errors.New("image: file extension does not match content")
	ErrCorruptImage    = errors.New("image: corrupt image data")
	ErrTooManyPixels   = errors.New("image: image dimensions exceed limit")
	ErrNoOutputDir     = errors.New("image: output directory is not specified")
)

type ProcessedImage struct {
	OriginalPath string `json:"original_path"` // 元の解像度のまま再エンコードした WebP（アップロード原本は保存しない）
	LargePath    string `json:"large_path"`    // 横幅 1200px WebP
	MediumPath   string `json:"medium_path"`   // 横幅 800px WebP
	SmallPath    string `json:"small_path"`    // 横幅 400px WebP
	Width        int    `json:"width"`         // 元画像の幅（EXIF の回転を適用した後）
	Height       int    `json:"height"`        // 元画像の高さ（EXIF の回転を適用した後）
	MimeType     string `json:"mime_type"`     // アップロードされた画像の実体の MIME タイプ
	// TakenAt は JPEG の EXIF の撮影日時（DateTimeOriginal。このPCの時刻とみなす）。無ければゼロ値。
	// EXIF は保存する WebP に残さないため、変換の前に読み取っておく。
	TakenAt time.Time `json:"-"`
}

type ProcessOptions struct {
	MaxUploadSize int64  // 既定 15MB
	OutputDir     string // 保存先ディレクトリ (例: "static/uploads")
}

// processSlots はデコード〜WebP 書き出しを同時に実行できる数を制限するセマフォ。
// 上限画素の画像は 1 件で数百MBを使うため、並行アップロードでメモリを使い切らないようにする。
var processSlots = make(chan struct{}, concurrencyLimit(runtime.NumCPU()))

// concurrencyLimit は CPU 数の 2 倍を [minConcurrency, maxConcurrency] に収めた同時実行数を返す。
func concurrencyLimit(cpus int) int {
	return min(max(cpus*2, minConcurrency), maxConcurrency)
}

// extensionsByMime は実体の形式ごとに許可するファイル名の拡張子。
var extensionsByMime = map[string][]string{
	MimeJPEG: {".jpg", ".jpeg"},
	MimePNG:  {".png"},
	MimeWebP: {".webp"},
}

// Process はアップロードされた画像ストリームを検査し、EXIFを完全破棄した上で
// 3サイズ (1200/800/400px) の WebP を生成して保存する。MIME詐称時はエラーを返す。
// 生成ファイルのパスは OutputDir を前置したスラッシュ区切りで返す。
func Process(r io.Reader, filename string, opts ProcessOptions) (*ProcessedImage, error) {
	if opts.OutputDir == "" {
		return nil, ErrNoOutputDir
	}
	maxSize := opts.MaxUploadSize
	if maxSize <= 0 {
		maxSize = DefaultMaxUploadSize
	}

	// 上限 +1 バイトまで読み、超過を検知する。
	data, err := io.ReadAll(io.LimitReader(r, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("image: read upload: %w", err)
	}
	if int64(len(data)) > maxSize {
		return nil, ErrFileTooLarge
	}

	mime := DetectMIME(data)
	if mime == "" {
		return nil, ErrUnsupportedType
	}
	if err := checkExtension(filename, mime); err != nil {
		return nil, err
	}

	// 遅いクライアントの受信で枠を塞がないよう、読み込みと軽い検査を終えてから枠を獲得する。
	processSlots <- struct{}{}
	defer func() { <-processSlots }()

	img, err := decode(data, mime)
	if err != nil {
		return nil, err
	}

	// 画素だけを新しい RGBA に描き直すことで、元ファイルのメタデータ（EXIF/GPS, XMP, ICC, テキストチャンク等）を一切引き継がない。
	clean := toRGBA(img)
	var takenAt time.Time
	if mime == MimeJPEG {
		clean = applyOrientation(clean, jpegOrientation(data))
		if t, ok := jpegTakenAt(data, nil); ok {
			takenAt = t
		}
	}

	result, err := save(clean, mime, opts.OutputDir)
	if err != nil {
		return nil, err
	}
	result.TakenAt = takenAt
	return result, nil
}

// DetectMIME は先頭のマジックナンバーから画像形式を判定する。対象外の形式なら空文字を返す。
func DetectMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return MimeJPEG
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return MimePNG
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return MimeWebP
	}
	return ""
}

// checkExtension はファイル名の拡張子が実体の形式と一致することを確認する。
// 拡張子がない場合は実体の判定だけに従う。
func checkExtension(filename, mime string) error {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		return nil
	}
	for _, allowed := range extensionsByMime[mime] {
		if ext == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is %s", ErrMIMEMismatch, filename, mime)
}

// decode は寸法を先に検査してから、判定済みの形式のデコーダだけで画像を復号する。
func decode(data []byte, mime string) (stdimage.Image, error) {
	var (
		decodeConfig func(io.Reader) (stdimage.Config, error)
		decodeImage  func(io.Reader) (stdimage.Image, error)
	)
	switch mime {
	case MimeJPEG:
		decodeConfig, decodeImage = jpeg.DecodeConfig, jpeg.Decode
	case MimePNG:
		decodeConfig, decodeImage = png.DecodeConfig, png.Decode
	case MimeWebP:
		decodeConfig = webp.DecodeConfig
		decodeImage = func(r io.Reader) (stdimage.Image, error) {
			return webp.Decode(r, webp.Options{AutoRotate: true})
		}
	default:
		return nil, ErrUnsupportedType
	}

	cfg, err := decodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptImage, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("%w: invalid dimensions %dx%d", ErrCorruptImage, cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, fmt.Errorf("%w: %dx%d", ErrTooManyPixels, cfg.Width, cfg.Height)
	}

	img, err := decodeImage(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptImage, err)
	}
	if b := img.Bounds(); b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("%w: empty image", ErrCorruptImage)
	}
	return img, nil
}

// toRGBA は画素だけを原点 (0,0) の新しい RGBA 画像に描画する。
func toRGBA(src stdimage.Image) *stdimage.RGBA {
	b := src.Bounds()
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// save は原寸と 3 サイズの WebP をランダムな名前で OutputDir に書き出す。
// 途中で失敗した場合は書き出し済みのファイルを削除する。
func save(img *stdimage.RGBA, mime, outputDir string) (*ProcessedImage, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("image: create output dir: %w", err)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}

	b := img.Bounds()
	result := &ProcessedImage{Width: b.Dx(), Height: b.Dy(), MimeType: mime}
	outputs := []struct {
		dst   *string
		name  string
		image stdimage.Image
	}{
		{&result.OriginalPath, id + "-original.webp", img},
		{&result.LargePath, fmt.Sprintf("%s-%d.webp", id, WidthLarge), resizeToWidth(img, WidthLarge)},
		{&result.MediumPath, fmt.Sprintf("%s-%d.webp", id, WidthMedium), resizeToWidth(img, WidthMedium)},
		{&result.SmallPath, fmt.Sprintf("%s-%d.webp", id, WidthSmall), resizeToWidth(img, WidthSmall)},
	}

	var written []string
	for _, o := range outputs {
		path := filepath.Join(outputDir, o.name)
		if err := writeWebP(path, o.image); err != nil {
			for _, p := range written {
				os.Remove(p)
			}
			return nil, err
		}
		written = append(written, path)
		*o.dst = filepath.ToSlash(path)
	}
	return result, nil
}

// writeWebP は一時ファイルに書いてから rename し、書きかけのファイルを公開しない。
func writeWebP(path string, img stdimage.Image) error {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, webp.Options{Quality: webpQuality}); err != nil {
		return fmt.Errorf("image: encode webp: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*.tmp")
	if err != nil {
		return fmt.Errorf("image: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("image: write webp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("image: write webp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("image: chmod webp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("image: rename webp: %w", err)
	}
	return nil
}

// randomID は推測不能なファイル名用の 128bit 乱数を 16 進文字列で返す。
func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("image: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
