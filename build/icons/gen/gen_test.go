package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// iconsDir はコミットしてある資材のディレクトリ（build/icons）。
const iconsDir = ".."

// コミットしてある資材は、コミットしてある元画像（sorus.png）から作ったものと同じ。元画像を差し替えたら
// go run ./build/icons/gen で作り直す（docs/developer_guide.md）。
func TestCommittedIconsMatchMaster(t *testing.T) {
	master, err := readPNG(filepath.Join(iconsDir, MasterName))
	if err != nil {
		t.Fatal(err)
	}
	if b := master.Bounds(); b.Dx() != MasterSize || b.Dy() != MasterSize {
		t.Fatalf("master is %dx%d, want %dx%d", b.Dx(), b.Dy(), MasterSize, MasterSize)
	}
	files, err := Derive(master)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sorus.ico", "sorus.icns", "sorus-256.png", "rsrc_windows_amd64.syso", "rsrc_windows_arm64.syso"} {
		want, ok := files[name]
		if !ok {
			t.Fatalf("Derive did not produce %s", name)
		}
		got, err := os.ReadFile(filepath.Join(iconsDir, name))
		if err != nil {
			t.Fatalf("%s: %v (run: go run ./build/icons/gen)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is out of date with %s; run: go run ./build/icons/gen", name, MasterName)
		}
	}
}

// iconSVG は static/icon.svg の要素（transform の付いたグループの中に、外枠線付きの白い円と、線・円のグループ）。
type iconSVG struct {
	ViewBox string     `xml:"viewBox,attr"`
	Rects   []struct{} `xml:"rect"`
	Texts   []struct{} `xml:"text"`
	Body    []struct {
		Transform string      `xml:"transform,attr"`
		Circles   []svgCircle `xml:"circle"`
		Groups    []struct {
			Stroke      string      `xml:"stroke,attr"`
			StrokeWidth float64     `xml:"stroke-width,attr"`
			LineCap     string      `xml:"stroke-linecap,attr"`
			Fill        string      `xml:"fill,attr"`
			Lines       []svgLine   `xml:"line"`
			Circles     []svgCircle `xml:"circle"`
		} `xml:"g"`
	} `xml:"g"`
}

type svgCircle struct {
	CX          float64 `xml:"cx,attr"`
	CY          float64 `xml:"cy,attr"`
	R           float64 `xml:"r,attr"`
	Fill        string  `xml:"fill,attr"`
	Stroke      string  `xml:"stroke,attr"`
	StrokeWidth float64 `xml:"stroke-width,attr"`
}

type svgLine struct {
	X1 float64 `xml:"x1,attr"`
	Y1 float64 `xml:"y1,attr"`
	X2 float64 `xml:"x2,attr"`
	Y2 float64 `xml:"y2,attr"`
}

func hexColor(c color.NRGBA) string { return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B) }

// 標準のデザインは static/icon.svg と同じ配色・形（背景は透明、transform="translate(-31.7, -41.2) scale(1.353)" の中に、
// #255799 の外枠線付きの白い円と、その上の #255799 の線 5 本と円 6 つ）。render.go のデザインの値が SVG の値と一致し、
// RenderDefault で描いた画像もその配色・形になっていること。
func TestRenderDefaultFollowsTheSVGDesign(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "static", "icon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	var svg iconSVG
	if err := xml.Unmarshal(data, &svg); err != nil {
		t.Fatalf("static/icon.svg: %v", err)
	}
	changed := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("static/icon.svg changed ("+format+"); update the design values in render.go and run: go run ./build/icons/gen -render", args...)
	}
	if svg.ViewBox != fmt.Sprintf("0 0 %g %g", designIconViewBox, designIconViewBox) {
		changed("viewBox %q", svg.ViewBox)
	}
	if len(svg.Rects) != 0 || len(svg.Texts) != 0 {
		changed("has %d <rect> and %d <text>; the default design has a transparent background and no text", len(svg.Rects), len(svg.Texts))
	}
	if len(svg.Body) != 1 {
		changed("%d top-level groups, want 1", len(svg.Body))
	}
	body := svg.Body[0]
	if want := fmt.Sprintf("translate(%g, %g) scale(%g)", designIconTranslate[0], designIconTranslate[1], designIconScale); body.Transform != want {
		changed("transform %q, want %q", body.Transform, want)
	}
	if len(body.Circles) != 1 {
		changed("%d circles directly in the group, want 1 plate", len(body.Circles))
	}
	if p := body.Circles[0]; (designCircle{p.CX, p.CY, p.R}) != designPlate || !strings.EqualFold(p.Fill, hexColor(designPlateColor)) ||
		!strings.EqualFold(p.Stroke, hexColor(designPrimaryColor)) || p.StrokeWidth != designPlateStrokeWidth {
		changed("plate %+v", p)
	}
	var lines []designLine
	var dots []designCircle
	for _, g := range body.Groups {
		if len(g.Lines) > 0 {
			if !strings.EqualFold(g.Stroke, hexColor(designPrimaryColor)) || g.StrokeWidth != designStrokeWidth || g.LineCap != "round" {
				changed("line group stroke=%q width=%g linecap=%q", g.Stroke, g.StrokeWidth, g.LineCap)
			}
			for _, l := range g.Lines {
				lines = append(lines, designLine{l.X1, l.Y1, l.X2, l.Y2})
			}
		}
		if len(g.Circles) > 0 {
			if !strings.EqualFold(g.Fill, hexColor(designPrimaryColor)) {
				changed("circle group fill=%q", g.Fill)
			}
			for _, c := range g.Circles {
				dots = append(dots, designCircle{c.CX, c.CY, c.R})
			}
		}
	}
	if !slices.Equal(lines, designLines) {
		changed("lines %v, render.go has %v", lines, designLines)
	}
	if !slices.Equal(dots, designDots) {
		changed("circles %v, render.go has %v", dots, designDots)
	}

	// 256px（SVG と同じ大きさ）で描いて、場所ごとの色を確かめる。px はシンボルの座標を画像の画素にする。
	img := RenderDefault(256)
	px := func(x, y float64) (int, int) {
		return int(x*designIconScale + designIconTranslate[0]), int(y*designIconScale + designIconTranslate[1])
	}
	at := func(x, y float64) color.NRGBA { return img.NRGBAAt(px(x, y)) }
	for _, p := range [][2]int{{0, 0}, {255, 0}, {0, 255}, {255, 255}} {
		if c := img.NRGBAAt(p[0], p[1]); c.A != 0 {
			t.Errorf("corner %v = %v, want transparent", p, c)
		}
	}
	p := designPlate
	// 外枠線（円の縁、太さ 4）は #255799 で、その外（縁から 6 離れた所）は透明。
	for _, dir := range [][2]float64{{0, -1}, {-1, 0}, {1, 0}, {0, 1}} {
		if c := at(p.cx+dir[0]*p.r, p.cy+dir[1]*p.r); c != designPrimaryColor {
			t.Errorf("plate outline toward %v = %v, want %v", dir, c, designPrimaryColor)
		}
		if c := at(p.cx+dir[0]*(p.r+6), p.cy+dir[1]*(p.r+6)); c.A != 0 {
			t.Errorf("outside the plate toward %v = %v, want transparent", dir, c)
		}
	}
	// 白い円の縁の内側（上・左・右・下）で、線・円から離れた所は白。
	for _, q := range [][2]float64{{p.cx, p.cy - p.r + 6}, {p.cx - p.r + 6, p.cy}, {p.cx + p.r - 6, p.cy}, {p.cx, p.cy + p.r - 6}} {
		if c := at(q[0], q[1]); c != designPlateColor {
			t.Errorf("plate at %v = %v, want %v", q, c, designPlateColor)
		}
	}
	// 円の中心と、線の中ほどは #255799。
	for _, c := range designDots {
		if got := at(c.cx, c.cy); got != designPrimaryColor {
			t.Errorf("dot center %v = %v, want %v", c, got, designPrimaryColor)
		}
	}
	for _, l := range designLines {
		// 中心の円（半径 16.5）の外で、端の円に届く前の、線の 60% の所。
		if got := at(l.x1+(l.x2-l.x1)*0.6, l.y1+(l.y2-l.y1)*0.6); got != designPrimaryColor {
			t.Errorf("line %v at 60%% = %v, want %v", l, got, designPrimaryColor)
		}
	}
	// 線の太さ（5.5）より外は白い円のまま（線が太すぎない）。右上へ伸びる線の 60% の所で、法線の向きに 6 離れた所。
	l := designLines[1]
	mx, my := l.x1+(l.x2-l.x1)*0.6, l.y1+(l.y2-l.y1)*0.6
	n := math.Hypot(l.x2-l.x1, l.y2-l.y1)
	nx, ny := -(l.y2-l.y1)/n, (l.x2-l.x1)/n
	if got := at(mx+6*nx, my+6*ny); got != designPlateColor {
		t.Errorf("beside the line = %v, want the plate %v", got, designPlateColor)
	}
}

func TestNormalizeMaster(t *testing.T) {
	if _, err := NormalizeMaster(RenderDefault(128)); err == nil {
		t.Error("a 128px icon was accepted")
	}
	wide := Resize(RenderDefault(512), 512)
	wide.Rect.Max.X = 400
	if _, err := NormalizeMaster(wide); err == nil {
		t.Error("a non-square icon was accepted")
	}
	got, err := NormalizeMaster(RenderDefault(300))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != MasterSize || b.Dy() != MasterSize {
		t.Errorf("normalized to %v", b)
	}
}

func TestICOAndICNS(t *testing.T) {
	files, err := Derive(RenderDefault(MasterSize))
	if err != nil {
		t.Fatal(err)
	}
	ico := files["sorus.ico"]
	le := binary.LittleEndian
	if le.Uint16(ico[0:]) != 0 || le.Uint16(ico[2:]) != 1 || int(le.Uint16(ico[4:])) != len(icoSizes) {
		t.Fatalf("ICONDIR = % x", ico[:6])
	}
	for i, size := range icoSizes {
		e := ico[6+16*i:]
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		n, off := le.Uint32(e[8:]), le.Uint32(e[12:])
		data := ico[off : off+n]
		if w != size {
			t.Errorf("entry %d width = %d, want %d", i, w, size)
		}
		if size >= 256 {
			if !bytes.HasPrefix(data, []byte("\x89PNG")) {
				t.Errorf("%dpx entry is not PNG", size)
			}
			continue
		}
		if le.Uint32(data[0:]) != 40 || int(le.Uint32(data[4:])) != size || int(le.Uint32(data[8:])) != 2*size || le.Uint16(data[14:]) != 32 {
			t.Errorf("%dpx BITMAPINFOHEADER = % x", size, data[:16])
		}
		maskStride := ((size + 31) / 32) * 4
		if len(data) != 40+size*size*4+maskStride*size {
			t.Errorf("%dpx DIB is %d bytes", size, len(data))
		}
		// 左下の角（DIB の最初の画素）は透明で、AND マスクの最初のビットが立つ。
		if data[40+3] != 0 || data[40+size*size*4]&0x80 == 0 {
			t.Errorf("%dpx corner pixel/mask = %x / %x", size, data[40:44], data[40+size*size*4])
		}
	}

	icns := files["sorus.icns"]
	if string(icns[:4]) != "icns" || int(binary.BigEndian.Uint32(icns[4:])) != len(icns) {
		t.Fatalf("icns header = % x", icns[:8])
	}
	seen := map[string]int{}
	for off := 8; off < len(icns); {
		typ := string(icns[off : off+4])
		n := int(binary.BigEndian.Uint32(icns[off+4:]))
		img, err := png.Decode(bytes.NewReader(icns[off+8 : off+n]))
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		seen[typ] = img.Bounds().Dx()
		off += n
	}
	for _, want := range icnsTypes {
		if seen[want.typ] != want.size {
			t.Errorf("icns %s = %dpx, want %dpx", want.typ, seen[want.typ], want.size)
		}
	}
}

// resource は .rsrc セクションの中の、種類・ID・言語ごとのデータ。
type resource struct {
	typ, id, lang int
	data          []byte
}

// readResources は .rsrc セクションのデータ sect（先頭の RVA は base。COFF のオブジェクトなら 0 で、データの位置は
// 再配置で直す前のセクションの中の位置）を読む。
func readResources(t *testing.T, sect []byte, base uint32) []resource {
	t.Helper()
	le := binary.LittleEndian
	entries := func(at uint32) [][2]uint32 {
		n := int(le.Uint16(sect[at+12:])) + int(le.Uint16(sect[at+14:]))
		var out [][2]uint32
		for i := 0; i < n; i++ {
			e := at + 16 + uint32(8*i)
			out = append(out, [2]uint32{le.Uint32(sect[e:]), le.Uint32(sect[e+4:])})
		}
		return out
	}
	const subdir = 0x80000000
	var out []resource
	for _, te := range entries(0) {
		if te[1]&subdir == 0 {
			t.Fatalf("type %d is not a directory", te[0])
		}
		for _, ie := range entries(te[1] &^ subdir) {
			for _, le2 := range entries(ie[1] &^ subdir) {
				if le2[1]&subdir != 0 {
					t.Fatalf("language entry points to a directory")
				}
				de := le2[1]
				rva, size := le.Uint32(sect[de:]), le.Uint32(sect[de+4:])
				off := rva - base
				out = append(out, resource{int(te[0]), int(ie[0]), int(le2[0]), sect[off : off+size]})
			}
		}
	}
	return out
}

// checkIconResources は RT_GROUP_ICON（ID 1）が、icoSizes の大きさの RT_ICON を正しい大きさで指しているかを確かめる。
func checkIconResources(t *testing.T, res []resource) {
	t.Helper()
	icons := map[int][]byte{}
	var group []byte
	for _, r := range res {
		switch r.typ {
		case rtIcon:
			icons[r.id] = r.data
		case rtGroupIcon:
			if r.id != 1 {
				t.Errorf("RT_GROUP_ICON id = %d, want 1", r.id)
			}
			group = r.data
		default:
			t.Errorf("unexpected resource type %d", r.typ)
		}
	}
	if group == nil {
		t.Fatal("no RT_GROUP_ICON")
	}
	le := binary.LittleEndian
	if n := int(le.Uint16(group[4:])); n != len(icoSizes) || len(icons) != n {
		t.Fatalf("group has %d entries, %d RT_ICON, want %d", n, len(icons), len(icoSizes))
	}
	for i, size := range icoSizes {
		e := group[6+14*i:]
		id := int(le.Uint16(e[12:]))
		data, ok := icons[id]
		if !ok {
			t.Fatalf("group entry %d points to missing RT_ICON %d", i, id)
		}
		if int(le.Uint32(e[8:])) != len(data) {
			t.Errorf("group entry %d size = %d, RT_ICON %d is %d bytes", i, le.Uint32(e[8:]), id, len(data))
		}
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		if w != size {
			t.Errorf("group entry %d width = %d, want %d", i, w, size)
		}
	}
}

// .syso は .rsrc セクション1つと、データの項目ごとの ADDR32NB の再配置を持つ COFF のオブジェクト。
func TestSysoStructure(t *testing.T) {
	for _, m := range []coffMachine{machineAMD64, machineARM64} {
		t.Run(m.goarch, func(t *testing.T) {
			f, err := pe.Open(filepath.Join(iconsDir, "rsrc_windows_"+m.goarch+".syso"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if f.Machine != m.machine || len(f.Sections) != 1 || f.Sections[0].Name != ".rsrc" {
				t.Fatalf("machine %#x, sections %d", f.Machine, len(f.Sections))
			}
			s := f.Sections[0]
			data, err := s.Data()
			if err != nil {
				t.Fatal(err)
			}
			res := readResources(t, data, 0)
			checkIconResources(t, res)
			if len(s.Relocs) != len(res) {
				t.Errorf("%d relocations for %d resources", len(s.Relocs), len(res))
			}
			for _, r := range s.Relocs {
				if r.Type != m.reloc || r.SymbolTableIndex != 0 {
					t.Errorf("relocation %+v", r)
				}
			}
			if len(f.COFFSymbols) != 1 || f.COFFSymbols[0].SectionNumber != 1 || f.COFFSymbols[0].StorageClass != 3 {
				t.Errorf("symbols = %+v", f.COFFSymbols)
			}
		})
	}
}

// Go のリンカーが .syso を実行ファイルのリソースとして取り込み、データの位置を RVA に直す（Windows 向けの小さな
// プログラムをクロスビルドして、できた実行ファイルのリソースを読む）。
func TestSysoLinksIntoWindowsExecutable(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-builds a small Windows program")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go command not found")
	}
	for _, m := range []coffMachine{machineAMD64, machineARM64} {
		t.Run(m.goarch, func(t *testing.T) {
			dir := t.TempDir()
			syso, err := os.ReadFile(filepath.Join(iconsDir, "rsrc_windows_"+m.goarch+".syso"))
			if err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string][]byte{
				"go.mod":                             []byte("module icontest\n\ngo 1.21\n"),
				"main.go":                            []byte("package main\n\nfunc main() {}\n"),
				"rsrc_windows_" + m.goarch + ".syso": syso,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			exe := filepath.Join(dir, "icontest.exe")
			cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags=-s -w", "-o", exe, ".")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+m.goarch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, out)
			}
			f, err := pe.Open(exe)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			s := f.Section(".rsrc")
			if s == nil {
				t.Fatal("the executable has no .rsrc section")
			}
			data, err := s.Data()
			if err != nil {
				t.Fatal(err)
			}
			var dirRVA uint32
			switch oh := f.OptionalHeader.(type) {
			case *pe.OptionalHeader64:
				dirRVA = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE].VirtualAddress
			default:
				t.Fatalf("unexpected optional header %T", oh)
			}
			if dirRVA != s.VirtualAddress {
				t.Errorf("resource directory RVA %#x, .rsrc at %#x", dirRVA, s.VirtualAddress)
			}
			res := readResources(t, data, s.VirtualAddress)
			checkIconResources(t, res)
			for _, r := range res {
				if r.typ == rtIcon && r.id == len(icoSizes) && !bytes.HasPrefix(r.data, []byte("\x89PNG")) {
					t.Errorf("256px RT_ICON is not PNG after linking")
				}
			}
		})
	}
}
