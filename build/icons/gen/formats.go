package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
)

// icoSizes は .ico と Windows のリソースに入れる大きさ（Windows のエクスプローラー・タスクバー・ショートカットが使う）。
var icoSizes = []int{16, 24, 32, 48, 64, 128, 256}

// iconImage は .ico・RT_ICON に入れる画像1つ。data は DIB（BITMAPINFOHEADER から）か PNG。
type iconImage struct {
	size int
	data []byte
}

// encodeDIB は img を .ico・RT_ICON 用の 32bit の DIB（BITMAPINFOHEADER、高さは2倍、下から上の BGRA と AND マスク）にする。
func encodeDIB(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	maskStride := ((w + 31) / 32) * 4
	var buf bytes.Buffer
	le := binary.LittleEndian
	hdr := make([]byte, 40)
	le.PutUint32(hdr[0:], 40)
	le.PutUint32(hdr[4:], uint32(w))
	le.PutUint32(hdr[8:], uint32(2*h))
	le.PutUint16(hdr[12:], 1)  // planes
	le.PutUint16(hdr[14:], 32) // bit count
	le.PutUint32(hdr[20:], uint32(w*h*4+maskStride*h))
	buf.Write(hdr)
	mask := make([]byte, maskStride*h)
	px := Resize(img, w)
	for y := h - 1; y >= 0; y-- {
		row := h - 1 - y
		for x := 0; x < w; x++ {
			c := px.NRGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
			if c.A == 0 {
				mask[row*maskStride+x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	buf.Write(mask)
	return buf.Bytes()
}

// iconDirEntry は ICONDIRENTRY・GRPICONDIRENTRY の共通部分（幅・高さは 256 を 0 で表す）。
func iconDirEntry(img iconImage) []byte {
	e := make([]byte, 12)
	dim := byte(img.size)
	if img.size >= 256 {
		dim = 0
	}
	e[0], e[1] = dim, dim
	binary.LittleEndian.PutUint16(e[4:], 1)  // planes
	binary.LittleEndian.PutUint16(e[6:], 32) // bit count
	binary.LittleEndian.PutUint32(e[8:], uint32(len(img.data)))
	return e
}

// encodeICO は .ico ファイル（ICONDIR・ICONDIRENTRY・画像）を作る。
func encodeICO(icons []iconImage) []byte {
	var buf bytes.Buffer
	le := binary.LittleEndian
	head := make([]byte, 6)
	le.PutUint16(head[2:], 1) // type: icon
	le.PutUint16(head[4:], uint16(len(icons)))
	buf.Write(head)
	offset := 6 + 16*len(icons)
	for _, img := range icons {
		buf.Write(iconDirEntry(img))
		off := make([]byte, 4)
		le.PutUint32(off, uint32(offset))
		buf.Write(off)
		offset += len(img.data)
	}
	for _, img := range icons {
		buf.Write(img.data)
	}
	return buf.Bytes()
}

// encodeGroupIcon は RT_GROUP_ICON のデータ（GRPICONDIR と GRPICONDIRENTRY。画像は RT_ICON の ID 1..n を指す）を作る。
func encodeGroupIcon(icons []iconImage) []byte {
	var buf bytes.Buffer
	le := binary.LittleEndian
	head := make([]byte, 6)
	le.PutUint16(head[2:], 1)
	le.PutUint16(head[4:], uint16(len(icons)))
	buf.Write(head)
	for i, img := range icons {
		buf.Write(iconDirEntry(img))
		id := make([]byte, 2)
		le.PutUint16(id, uint16(i+1))
		buf.Write(id)
	}
	return buf.Bytes()
}

// icnsTypes は .icns に入れる種類と、その画像の大きさ（すべて PNG。macOS 10.7 以降）。
var icnsTypes = []struct {
	typ  string
	size int
}{
	{"icp4", 16}, {"icp5", 32}, {"icp6", 64}, {"ic07", 128}, {"ic08", 256}, {"ic09", 512}, {"ic10", 1024},
	{"ic11", 32}, {"ic12", 64}, {"ic13", 256}, {"ic14", 512},
}

// encodeICNS は .icns ファイルを作る。pngs は大きさごとの PNG。
func encodeICNS(pngs map[int][]byte) ([]byte, error) {
	var body bytes.Buffer
	for _, t := range icnsTypes {
		data, ok := pngs[t.size]
		if !ok {
			return nil, fmt.Errorf("gen: no %dpx image for %s", t.size, t.typ)
		}
		body.WriteString(t.typ)
		n := make([]byte, 4)
		binary.BigEndian.PutUint32(n, uint32(8+len(data)))
		body.Write(n)
		body.Write(data)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	n := make([]byte, 4)
	binary.BigEndian.PutUint32(n, uint32(8+body.Len()))
	out.Write(n)
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// coffMachine は .syso（COFF のオブジェクトファイル）の対象の CPU。
type coffMachine struct {
	goarch  string
	machine uint16 // IMAGE_FILE_MACHINE_*
	reloc   uint16 // ADDR32NB（イメージの先頭からの相対アドレス）の再配置の種類
}

var (
	machineAMD64 = coffMachine{goarch: "amd64", machine: 0x8664, reloc: 0x0003} // IMAGE_REL_AMD64_ADDR32NB
	machineARM64 = coffMachine{goarch: "arm64", machine: 0xAA64, reloc: 0x0002} // IMAGE_REL_ARM64_ADDR32NB
)

// リソースの種類と言語。
const (
	rtIcon      = 3
	rtGroupIcon = 14
	langEnUS    = 0x0409
)

// encodeSyso は icons を RT_ICON（ID 1..n）と RT_GROUP_ICON（ID 1）にした .rsrc セクションを持つ COFF の
// オブジェクトファイルを作る。Go のリンカーはパッケージのディレクトリにある *_windows_<arch>.syso を自動で
// リンクし、.rsrc を実行ファイルのリソースにする（cmd/link の loadpe・addpersrc）。データの位置
// （IMAGE_RESOURCE_DATA_ENTRY の OffsetToData）はセクションの先頭からの位置を書き、.rsrc のセクションの
// シンボルへの ADDR32NB の再配置でリンカーに RVA へ直させる。
func encodeSyso(m coffMachine, icons []iconImage) []byte {
	type leaf struct {
		typ, id int
		data    []byte
	}
	var leaves []leaf
	for i, img := range icons {
		leaves = append(leaves, leaf{rtIcon, i + 1, img.data})
	}
	leaves = append(leaves, leaf{rtGroupIcon, 1, encodeGroupIcon(icons)})

	const dirSize, entrySize, dataEntrySize = 16, 8, 16
	types := []int{rtIcon, rtGroupIcon}
	idsOf := map[int][]int{}
	for _, l := range leaves {
		idsOf[l.typ] = append(idsOf[l.typ], l.id)
	}
	// 配置: ルート → 種類ごとのディレクトリ → ID ごとの言語のディレクトリ → データの項目 → データ。
	off := dirSize + entrySize*len(types)
	typeDirOff := map[int]int{}
	for _, t := range types {
		typeDirOff[t] = off
		off += dirSize + entrySize*len(idsOf[t])
	}
	langDirOff := make([]int, len(leaves))
	for i := range leaves {
		langDirOff[i] = off
		off += dirSize + entrySize
	}
	dataEntryOff := make([]int, len(leaves))
	for i := range leaves {
		dataEntryOff[i] = off
		off += dataEntrySize
	}
	dataOff := make([]int, len(leaves))
	for i, l := range leaves {
		off = align(off, 8)
		dataOff[i] = off
		off += len(l.data)
	}
	sect := make([]byte, align(off, 8))
	le := binary.LittleEndian
	writeDir := func(at, n int) {
		le.PutUint16(sect[at+14:], uint16(n)) // NumberOfIdEntries（名前の項目は無い）
	}
	writeEntry := func(at, id int, target uint32) {
		le.PutUint32(sect[at:], uint32(id))
		le.PutUint32(sect[at+4:], target)
	}
	const subdir = 0x80000000
	writeDir(0, len(types))
	for i, t := range types {
		writeEntry(dirSize+entrySize*i, t, subdir|uint32(typeDirOff[t]))
	}
	for _, t := range types {
		writeDir(typeDirOff[t], len(idsOf[t]))
		n := 0
		for i, l := range leaves {
			if l.typ != t {
				continue
			}
			writeEntry(typeDirOff[t]+dirSize+entrySize*n, l.id, subdir|uint32(langDirOff[i]))
			n++
		}
	}
	var relocs []uint32
	for i, l := range leaves {
		writeDir(langDirOff[i], 1)
		writeEntry(langDirOff[i]+dirSize, langEnUS, uint32(dataEntryOff[i]))
		le.PutUint32(sect[dataEntryOff[i]:], uint32(dataOff[i])) // OffsetToData（再配置で RVA にする）
		le.PutUint32(sect[dataEntryOff[i]+4:], uint32(len(l.data)))
		relocs = append(relocs, uint32(dataEntryOff[i]))
		copy(sect[dataOff[i]:], l.data)
	}

	const fileHeaderSize, sectionHeaderSize, relocSize, symbolSize = 20, 40, 10, 18
	rawOff := fileHeaderSize + sectionHeaderSize
	relocOff := rawOff + len(sect)
	symOff := relocOff + relocSize*len(relocs)

	var out bytes.Buffer
	fh := make([]byte, fileHeaderSize)
	le.PutUint16(fh[0:], m.machine)
	le.PutUint16(fh[2:], 1) // NumberOfSections
	le.PutUint32(fh[8:], uint32(symOff))
	le.PutUint32(fh[12:], 1) // NumberOfSymbols
	out.Write(fh)

	sh := make([]byte, sectionHeaderSize)
	copy(sh[0:8], ".rsrc")
	le.PutUint32(sh[16:], uint32(len(sect))) // SizeOfRawData
	le.PutUint32(sh[20:], uint32(rawOff))    // PointerToRawData
	le.PutUint32(sh[24:], uint32(relocOff))  // PointerToRelocations
	le.PutUint16(sh[32:], uint16(len(relocs)))
	le.PutUint32(sh[36:], 0x40000040) // IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ
	out.Write(sh)
	out.Write(sect)

	for _, at := range relocs {
		r := make([]byte, relocSize)
		le.PutUint32(r[0:], at)
		le.PutUint32(r[4:], 0) // シンボル 0（.rsrc のセクションのシンボル）
		le.PutUint16(r[8:], m.reloc)
		out.Write(r)
	}

	sym := make([]byte, symbolSize)
	copy(sym[0:8], ".rsrc")
	le.PutUint16(sym[12:], 1) // SectionNumber
	sym[16] = 3               // IMAGE_SYM_CLASS_STATIC
	out.Write(sym)
	out.Write([]byte{4, 0, 0, 0}) // 文字列表（空。大きさの4バイトだけ）
	return out.Bytes()
}

func align(n, a int) int { return (n + a - 1) / a * a }
