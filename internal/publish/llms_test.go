package publish

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// 静的な書き出し（internal/server の exportInto）がサイトの直下に書く、AI 向けのサイト案内（llms.txt）と全文（llms-full.txt）。
// 公開先へ反映する処理は、書き出しのフォルダのファイルをそのまま送るため、どの公開先にもこの2つが届き、文字として読めること。
const (
	testLLMSTxt     = "# サイト名\n\n> サイトの説明\n\n## Articles\n\n- [記事](https://example.jp/a): 要約\n"
	testLLMSFullTxt = "# 記事\n\n本文です。\n"
)

func writeExportedSite(t *testing.T, dir string) {
	t.Helper()
	writeSite(t, dir, map[string]string{
		"index.html":      "<h1>top</h1>",
		"llms.txt":        testLLMSTxt,
		"llms-full.txt":   testLLMSFullTxt,
		"robots.txt":      "User-agent: *\n",
		"static/site.css": "body{}",
		"a/index.html":    "<p>a</p>",
	})
}

// 書き出しの Manifest・作業に llms.txt・llms-full.txt が入り、ページより先に送られ、手元のフォルダへ同じ内容で届く。
// 内容が変われば次の反映で送り直し、書き出しから消えれば公開先からも消える。
func TestSyncPublishesLLMSFiles(t *testing.T) {
	dir, dest := t.TempDir(), t.TempDir()
	writeExportedSite(t, dir)
	next := mustManifest(t, dir)
	for _, name := range []string{"llms.txt", "llms-full.txt"} {
		if _, ok := next[name]; !ok {
			t.Fatalf("manifest has no %s: %v", name, next)
		}
	}
	plan := NewPlan(nil, next)
	want := []string{"llms-full.txt", "llms.txt", "robots.txt", "static/site.css", "a/index.html", "index.html"}
	if !reflect.DeepEqual(plan.Upload, want) {
		t.Errorf("upload order = %v, want %v", plan.Upload, want)
	}

	r, err := OpenFolder(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), r, dir, nil, next, nil); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for name, body := range map[string]string{"llms.txt": testLLMSTxt, "llms-full.txt": testLLMSFullTxt} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != body {
			t.Errorf("%s = %q, %v; want %q", name, got, err, body)
		}
	}

	// 記事が増えて llms.txt だけが変わった次の書き出し。
	updated := testLLMSTxt + "- [新しい記事](https://example.jp/b): 要約\n"
	writeSite(t, dir, map[string]string{"llms.txt": updated})
	if err := os.Remove(filepath.Join(dir, "llms-full.txt")); err != nil {
		t.Fatal(err)
	}
	next2 := mustManifest(t, dir)
	res, err := Sync(context.Background(), r, dir, next, next2, nil)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if res.Uploaded != 1 || res.Deleted != 1 || res.Unchanged != 4 {
		t.Errorf("second result = %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "llms.txt")); string(got) != updated {
		t.Errorf("updated llms.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "llms-full.txt")); !os.IsNotExist(err) {
		t.Errorf("llms-full.txt is left on the destination: %v", err)
	}
}

// 書き出しを Zip でダウンロードするときも、llms.txt・llms-full.txt をサイトの直下に入れる。
func TestWriteZipIncludesLLMSFiles(t *testing.T) {
	dir := t.TempDir()
	writeExportedSite(t, dir)
	var buf bytes.Buffer
	if err := WriteZip(&buf, dir); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[f.Name] = string(b)
	}
	if got["llms.txt"] != testLLMSTxt || got["llms-full.txt"] != testLLMSFullTxt {
		t.Errorf("zip llms files = %q / %q (entries %v)", got["llms.txt"], got["llms-full.txt"], slices.Sorted(maps.Keys(got)))
	}
}

// Cloudflare Pages・S3 では送る側が Content-Type を決める。.txt は送るパソコンの MIME の設定によらず UTF-8 の
// text/plain にする（Go の標準の表に .txt が無く、設定の無い環境では application/octet-stream になっていた）。
func TestTextFilesAreSentAsUTF8PlainText(t *testing.T) {
	const want = "text/plain; charset=utf-8"
	for _, name := range []string{"llms.txt", "llms-full.txt", "robots.txt", "docs/LLMS.TXT"} {
		if got := contentType(name); got != want {
			t.Errorf("cloudflare contentType(%q) = %q, want %q", name, got, want)
		}
		if got := s3ContentType(name); got != want {
			t.Errorf("s3ContentType(%q) = %q, want %q", name, got, want)
		}
	}
	// ほかの拡張子は今までどおり。
	if got := contentType("index.html"); got != "text/html; charset=utf-8" {
		t.Errorf("contentType(index.html) = %q", got)
	}
}

// legacyCFAssetKey は Content-Type をキーに含めていなかったときの cfAssetKey。
func legacyCFAssetKey(name string, f File) string {
	sum := sha256.Sum256([]byte(f.SHA256 + ":" + strings.TrimPrefix(path.Ext(name), ".")))
	return hex.EncodeToString(sum[:])[:32]
}

// 以前に application/octet-stream で送った llms.txt は、内容が同じでも Cloudflare にあるものとみなさず送り直す。
// Content-Type の決め方を変えていないファイル（HTML・CSS など）のキーは変えない（すべてを送り直さない）。
func TestCloudflareAssetKeyResendsTextFilesOnly(t *testing.T) {
	f := File{SHA256: strings.Repeat("ab", 32), Size: 10}
	for _, name := range []string{"llms.txt", "llms-full.txt", "robots.txt"} {
		if cfAssetKey(name, f) == legacyCFAssetKey(name, f) {
			t.Errorf("%s keeps the legacy key", name)
		}
	}
	for _, name := range []string{"index.html", "static/site.css", "feed.xml", "manifest.json", "uploads/a.webp"} {
		if got, want := cfAssetKey(name, f), legacyCFAssetKey(name, f); got != want {
			t.Errorf("%s key = %s, want legacy %s", name, got, want)
		}
	}
	if k := cfAssetKey("llms.txt", f); len(k) != 32 || k == cfAssetKey("llms.html", f) {
		t.Errorf("llms.txt key = %q", k)
	}
}

// Cloudflare Pages の版に llms.txt・llms-full.txt がサイトの直下のパスで載り、UTF-8 の text/plain で送られる。
// 以前に古いキー（違う Content-Type）で送ってあっても送り直す。
func TestCloudflareDeployPublishesLLMSFilesAsPlainText(t *testing.T) {
	f, cf := newFakePages(t)
	dir := t.TempDir()
	writeExportedSite(t, dir)
	next := mustManifest(t, dir)
	f.present[legacyCFAssetKey("llms.txt", next["llms.txt"])] = true
	f.present[cfAssetKey("static/site.css", next["static/site.css"])] = true

	if _, err := cf.Deploy(context.Background(), dir, Manifest{"static/site.css": next["static/site.css"]}, next, nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for name, body := range map[string]string{"llms.txt": testLLMSTxt, "llms-full.txt": testLLMSFullTxt} {
		key := cfAssetKey(name, next[name])
		if got, want := f.uploaded[key], body+"|text/plain; charset=utf-8"; got != want {
			t.Errorf("uploaded %s = %q, want %q", name, got, want)
		}
		if f.manifest["/"+name] != key {
			t.Errorf("manifest /%s = %q, want %q", name, f.manifest["/"+name], key)
		}
	}
	if _, ok := f.uploaded[cfAssetKey("static/site.css", next["static/site.css"])]; ok {
		t.Errorf("unchanged static/site.css is uploaded again")
	}
}
