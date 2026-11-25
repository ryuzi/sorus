// Command gen はアプリアイコンの資材（build/icons/ の下）を作る。リポジトリのルートで実行する。
//
//	go run ./build/icons/gen            # 元画像 build/icons/sorus.png から、ほかの資材をすべて作り直す
//	go run ./build/icons/gen -src my.png # 独自のアイコン（正方形の PNG）を元画像にしてから作り直す
//	go run ./build/icons/gen -render    # 標準のデザイン（static/icon.svg と同じ）を元画像に描き直してから作り直す
//
// 作るもの（すべて元画像 sorus.png（1024x1024）から作る。同じ元画像からは同じバイト列になる）:
//
//	sorus.ico                       Windows 用のアイコン（16〜256px）
//	sorus.icns                      macOS 用のアイコン（16〜1024px）。install.sh が sorus.app に入れる
//	sorus-256.png                   Linux 用のアイコン（256px）。install.sh が ~/.local/share/icons に置く
//	rsrc_windows_amd64.syso        Windows の実行ファイルに埋め込むアイコンのリソース（scripts/release.sh が使う）
//	rsrc_windows_arm64.syso        同上（ARM 版）
//
// 外部のツール（ImageMagick・rsrc・windres など）は使わない。Go と、go.mod にある golang.org/x/image だけで作る。
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// MasterName は元画像のファイル名。MasterSize はその大きさ（正方形）。
const (
	MasterName = "sorus.png"
	MasterSize = 1024
)

func main() {
	dir := flag.String("dir", filepath.Join("build", "icons"), "資材を置くディレクトリ")
	src := flag.String("src", "", "元画像にする独自のアイコン（正方形の PNG。256px 以上、1024px 推奨）")
	render := flag.Bool("render", false, "標準のデザイン（static/icon.svg と同じ）を元画像に描き直す")
	flag.Parse()
	if *src != "" && *render {
		log.Fatal("gen: -src と -render は同時に使えません")
	}
	if err := run(*dir, *src, *render); err != nil {
		log.Fatal(err)
	}
}

func run(dir, src string, render bool) error {
	masterPath := filepath.Join(dir, MasterName)
	switch {
	case render:
		if err := writePNG(masterPath, RenderDefault(MasterSize)); err != nil {
			return err
		}
	case src != "":
		img, err := readPNG(src)
		if err != nil {
			return err
		}
		master, err := NormalizeMaster(img)
		if err != nil {
			return fmt.Errorf("gen: %s: %w", src, err)
		}
		if err := writePNG(masterPath, master); err != nil {
			return err
		}
	}
	master, err := readPNG(masterPath)
	if err != nil {
		return err
	}
	files, err := Derive(master)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(dir, name), len(files[name]))
	}
	return nil
}

func readPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("gen: %s is not a PNG: %w", path, err)
	}
	return img, nil
}

func writePNG(path string, img image.Image) error {
	data, err := encodePNG(img)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// NormalizeMaster は独自のアイコン img を元画像（MasterSize の正方形）にする。正方形でない・小さすぎるときはエラー。
func NormalizeMaster(img image.Image) (image.Image, error) {
	b := img.Bounds()
	if b.Dx() != b.Dy() {
		return nil, fmt.Errorf("the icon must be square (got %dx%d)", b.Dx(), b.Dy())
	}
	if b.Dx() < 256 {
		return nil, errors.New("the icon must be at least 256x256 (1024x1024 recommended)")
	}
	return Resize(img, MasterSize), nil
}

// Derive は元画像 master から、元画像以外の資材（ファイル名 → 中身）を作る。
func Derive(master image.Image) (map[string][]byte, error) {
	if b := master.Bounds(); b.Dx() != b.Dy() {
		return nil, fmt.Errorf("gen: the master icon must be square (got %dx%d)", b.Dx(), b.Dy())
	}
	sized := map[int][]byte{}
	for _, size := range []int{16, 24, 32, 48, 64, 128, 256, 512, 1024} {
		data, err := encodePNG(Resize(master, size))
		if err != nil {
			return nil, err
		}
		sized[size] = data
	}
	icons := make([]iconImage, 0, len(icoSizes))
	for _, size := range icoSizes {
		img := Resize(master, size)
		data := sized[size]
		if size < 256 {
			// 256px 未満は互換性の高い BMP（DIB）で入れる。256px は PNG のまま。
			data = encodeDIB(img)
		}
		icons = append(icons, iconImage{size: size, data: data})
	}
	icns, err := encodeICNS(sized)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{
		"sorus.ico":     encodeICO(icons),
		"sorus.icns":    icns,
		"sorus-256.png": sized[256],
	}
	for _, m := range []coffMachine{machineAMD64, machineARM64} {
		out["rsrc_windows_"+m.goarch+".syso"] = encodeSyso(m, icons)
	}
	return out, nil
}
