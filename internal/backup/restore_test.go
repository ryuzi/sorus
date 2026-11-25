package backup

import (
	"archive/zip"
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setRestoreLimits はテストの間だけ展開サイズの上限を差し替える。
func setRestoreLimits(t *testing.T, entry, total uint64) {
	t.Helper()
	oldEntry, oldTotal := maxRestoreEntrySize, maxRestoreTotalSize
	maxRestoreEntrySize, maxRestoreTotalSize = entry, total
	t.Cleanup(func() { maxRestoreEntrySize, maxRestoreTotalSize = oldEntry, oldTotal })
}

// writeDeflateEntries は es を Deflate で圧縮した Zip として path に書く（ゼロ埋めの大きなエントリが小さくなる）。
func writeDeflateEntries(t *testing.T, path string, es []entry) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range es {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withBomb は有効なアーカイブのエントリに uploads/bomb.bin（size バイトのゼロ）を足し、manifest.json の
// 画像の件数・合計サイズも合わせた（サイズの上限以外は正しい）エントリを返す。あわせて元のエントリの合計サイズと
// 最大のエントリのサイズを返す。
func withBomb(t *testing.T, valid []entry, size int) (es []entry, total, largest uint64) {
	t.Helper()
	for _, e := range valid {
		n := uint64(len(e.data))
		total += n
		largest = max(largest, n)
	}
	es = append([]entry{}, valid...)
	es = append(es, entry{name: UploadsDir + "/bomb.bin", data: make([]byte, size)})
	es = editManifest(t, es, func(m *Manifest) {
		m.Uploads.FileCount++
		m.Uploads.TotalSize += int64(size)
	})
	return es, total, largest
}

// 再現テスト: 圧縮率の高い巨大なエントリを含むアーカイブは、展開前（inspect）に拒み、復元先に触れない。
func TestRestoreRejectsOversizedEntry(t *testing.T) {
	valid := readEntries(t, newSource(t))
	bombSize := 1 << 20
	es, _, largest := withBomb(t, valid, bombSize)
	// 元のエントリは上限内、bomb.bin だけが上限を超えるようにする。
	setRestoreLimits(t, max(largest, uint64(bombSize)/2), 1<<40)
	zipPath := filepath.Join(t.TempDir(), "bomb.zip")
	writeDeflateEntries(t, zipPath, es)
	if st, err := os.Stat(zipPath); err != nil || st.Size() >= int64(bombSize)/4 {
		t.Fatalf("bomb archive should be highly compressed: %v, %v", st, err)
	}

	if _, err := ReadManifest(zipPath); !errors.Is(err, ErrInvalidArchive) || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("ReadManifest = %v, want too large entry error", err)
	}
	tg := newTarget(t)
	if err := Restore(zipPath, tg.db, tg.uploads); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Restore = %v, want ErrInvalidArchive", err)
	}
	tg.assertUnchanged(t)
}

// 再現テスト: 1エントリは上限内でも、展開後の合計が上限を超えるアーカイブは拒む。
func TestRestoreRejectsOversizedTotal(t *testing.T) {
	valid := readEntries(t, newSource(t))
	bombSize := 64 << 10
	es, total, _ := withBomb(t, valid, bombSize)
	setRestoreLimits(t, 1<<40, total+uint64(bombSize)/2)
	zipPath := filepath.Join(t.TempDir(), "total.zip")
	writeDeflateEntries(t, zipPath, es)

	if _, err := ReadManifest(zipPath); !errors.Is(err, ErrInvalidArchive) || !strings.Contains(err.Error(), "total uncompressed size") {
		t.Fatalf("ReadManifest = %v, want total size error", err)
	}
	tg := newTarget(t)
	if err := Restore(zipPath, tg.db, tg.uploads); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Restore = %v, want ErrInvalidArchive", err)
	}
	tg.assertUnchanged(t)

	// 上限ちょうどまでは受け付ける（上限の判定が厳しすぎない）。
	setRestoreLimits(t, 1<<40, total+uint64(bombSize))
	if _, err := ReadManifest(zipPath); err != nil {
		t.Fatalf("ReadManifest at the limit = %v, want nil", err)
	}
}

// countingWriter は書き込まれたバイト数を数える。
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// zeroReader は終わりのないゼロの列。
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// 申告より長い中身は、上限 +1 バイトを読んだ時点でやめる（終わりのない入力でも止まる）。
func TestCopyLimitedStopsAtLimit(t *testing.T) {
	w := &countingWriter{}
	_, err := copyLimited("bomb", 100, 100, zeroReader{}, w)
	if !errors.Is(err, ErrInvalidArchive) || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("copyLimited = %v, want larger than limit error", err)
	}
	if w.n > 101 {
		t.Errorf("wrote %d bytes, want at most limit+1 (101)", w.n)
	}

	// 申告どおりの長さなら SHA-256 を返す。
	sum, err := copyLimited("ok", 3, 3, strings.NewReader("abc"), io.Discard)
	if err != nil || sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("copyLimited(abc) = %q, %v", sum, err)
	}
	// 申告より短ければ切り詰めとして拒む。
	if _, err := copyLimited("short", 5, 5, strings.NewReader("abc"), io.Discard); !errors.Is(err, ErrInvalidArchive) ||
		!strings.Contains(err.Error(), "truncated") {
		t.Errorf("copyLimited(short) = %v, want truncated error", err)
	}
}

// 中央ディレクトリの申告サイズを偽った（実際はもっと長い）エントリは、申告サイズ +1 バイトで展開をやめる。
func TestExtractFileStopsAtDeclaredSize(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 4096)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name:               "uploads/liar.bin",
		Method:             zip.Store,
		CRC32:              crc32.ChecksumIEEE(payload[:10]),
		CompressedSize64:   uint64(len(payload)),
		UncompressedSize64: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	out := &countingWriter{}
	if _, err := extractFile(zr.File[0], out); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("extractFile = %v, want ErrInvalidArchive", err)
	}
	if out.n > 11 {
		t.Errorf("wrote %d bytes, want at most declared size+1 (11)", out.n)
	}

	// 申告サイズが上限を超えるエントリも、上限 +1 バイトで止める（inspect を経ずに呼ばれた場合の保険）。
	setRestoreLimits(t, 5, 1<<40)
	out = &countingWriter{}
	if _, err := extractFile(zr.File[0], out); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("extractFile over entry limit = %v, want ErrInvalidArchive", err)
	}
	if out.n > 6 {
		t.Errorf("wrote %d bytes, want at most limit+1 (6)", out.n)
	}
}
