package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
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

// 標準のデザインは static/icon.svg と同じ配色・形（角は透明、縁は濃紺、中央付近に白い「s」）。
func TestRenderDefaultFollowsTheSVGDesign(t *testing.T) {
	svg, err := os.ReadFile(filepath.Join("..", "..", "..", "static", "icon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`rx="128"`, `fill="#0f172a"`, `fill="#f8fafc"`, `font-size="280"`, `>s</text>`} {
		if !strings.Contains(string(svg), want) {
			t.Fatalf("static/icon.svg changed (missing %s); update the design constants in render.go", want)
		}
	}
	img := RenderDefault(256)
	if c := img.NRGBAAt(0, 0); c.A != 0 {
		t.Errorf("corner = %v, want transparent (rounded)", c)
	}
	if c := img.NRGBAAt(128, 4); c != designBackground {
		t.Errorf("top edge = %v, want %v", c, designBackground)
	}
	white := 0
	for y := 80; y < 200; y++ {
		for x := 80; x < 176; x++ {
			if img.NRGBAAt(x, y) == designForeground {
				white++
			}
		}
	}
	if white < 1500 {
		t.Errorf("only %d glyph pixels in the center; the letter is missing or misplaced", white)
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
