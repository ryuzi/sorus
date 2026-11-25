// Package static は CSS・JavaScript などの静的アセットをバイナリに埋め込む。
package static

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sync"
)

// FS は css/、js/、themes/、vendor/、および PWA アセット（manifest.json, sw.js, icon.svg）を保持する。
//
//go:embed css js themes icon.svg manifest.json sw.js vendor
var FS embed.FS

var (
	versionsOnce sync.Once
	// versions は FS の各ファイルのパスから、内容のハッシュ（URL の ?v= に付ける値）への表。
	versions map[string]string
)

// URL は FS のファイル name（例: "css/public.css"）を配信する URL を、内容のハッシュを付けて返す
// （例: "/static/css/public.css?v=0123456789abcdef"）。/static/ は immutable でキャッシュするため、
// 内容が変わったら URL も変わるようにする。ハッシュは初回の呼び出しで FS 全体について1回だけ計算する。
// FS に無い name はクエリを付けずに返す。
func URL(name string) string {
	versionsOnce.Do(func() { versions = hashFiles(FS) })
	return versionedURL(versions, name)
}

// hashFiles は fsys の全ファイルについて、SHA-256 の先頭8バイトを16桁の16進数にした値を返す。
// 読めないファイルは表に入れない（URL はクエリなしになる）。
func hashFiles(fsys fs.FS) map[string]string {
	out := map[string]string{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		out[p] = hex.EncodeToString(sum[:8])
		return nil
	})
	return out
}

// versionedURL は表 table を使って name の URL を作る。
func versionedURL(table map[string]string, name string) string {
	if v, ok := table[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}
