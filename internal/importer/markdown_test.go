package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	stdimage "image"
	"image/color"
	"image/gif"
	"os"
	"strings"
	"testing"
)

func buildTestZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{20, 40, 200, 255}, color.RGBA{240, 240, 240, 255}}
	img := stdimage.NewPaletted(stdimage.Rect(0, 0, w, h), pal)
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseMarkdownFrontmatterAndDefaults(t *testing.T) {
	exp, err := ParseMarkdown(strings.NewReader("---\ntitle: Handbook\nslug: guide\nkind: page\nstatus: publish\ncategory: Help\ntags: [setup]\ndate: 2025-01-02\n---\n# Body\n\nText"), "fallback.md")
	if err != nil {
		t.Fatal(err)
	}
	p := exp.Posts[0]
	if p.Title != "Handbook" || p.Slug != "guide" || p.Type != "page" || p.Status != "publish" || p.ContentMarkdown != "# Body\n\nText" || len(p.Categories) != 1 || len(p.Tags) != 1 || p.Date.IsZero() {
		t.Fatalf("post = %+v", p)
	}
	exp, err = ParseMarkdown(strings.NewReader("# Hello World\n\nText"), "my-file.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := exp.Posts[0]; got.Title != "Hello World" || got.Slug != "my-file" || got.Status != "draft" {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestParseMarkdownZipRejectsZipSlip(t *testing.T) {
	for _, name := range []string{"../outside.md", `C:\outside.md`, "/etc/passwd.md"} {
		data := buildTestZip(t, map[string][]byte{name: []byte("# unsafe")})
		_, err := ParseMarkdownZip(bytes.NewReader(data), int64(len(data)))
		if err == nil || !errors.Is(err, ErrUnsafeZipPath) || !strings.Contains(err.Error(), "unsafe zip path") {
			t.Fatalf("name=%q: error = %v, want ErrUnsafeZipPath", name, err)
		}
	}
}

func TestImportMarkdownZipErrorDistinction(t *testing.T) {
	// 1. 対象 Markdown ファイル無し
	noMD := buildTestZip(t, map[string][]byte{
		"images/a.png": testPNG(t, 16, 16),
		"readme.txt":   []byte("no markdown here"),
	})
	if _, err := ParseMarkdownZip(bytes.NewReader(noMD), int64(len(noMD))); !errors.Is(err, ErrNoMarkdownFiles) {
		t.Fatalf("no markdown error = %v, want ErrNoMarkdownFiles", err)
	}

	// 2. Zip サイズ上限超過（ヘッダ上の UncompressedSize64 偽装を含む）
	var oversizeBuf bytes.Buffer
	zw := zip.NewWriter(&oversizeBuf)
	fh := &zip.FileHeader{Name: "huge.md", Method: zip.Store}
	w, err := zw.CreateHeader(fh)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("# small"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	// ファイル数上限超過
	var manyBuf bytes.Buffer
	manyZip := zip.NewWriter(&manyBuf)
	for i := 0; i <= maxMarkdownArchiveFiles; i++ {
		f, err := manyZip.Create(strings.Repeat("a", 1) + "_" + string(rune('0'+i%10)) + "/" + strings.Repeat("x", i%5+1) + "_" + string(rune('a'+i%26)) + string(rune('A'+(i/26)%26)) + string(rune('0'+(i/676)%10)) + ".md")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte("# hi"))
	}
	if err := manyZip.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseMarkdownZip(bytes.NewReader(manyBuf.Bytes()), int64(manyBuf.Len())); !errors.Is(err, ErrZipTooLarge) {
		t.Fatalf("too many files error = %v, want ErrZipTooLarge", err)
	}

	// 3. 不正な frontmatter
	badFM := buildTestZip(t, map[string][]byte{
		"bad.md": []byte("---\ntitle: [unterminated\n---\nbody"),
	})
	if _, err := ParseMarkdownZip(bytes.NewReader(badFM), int64(len(badFM))); !errors.Is(err, ErrInvalidMarkdown) {
		t.Fatalf("invalid frontmatter error = %v, want ErrInvalidMarkdown", err)
	}
}

func TestImportMarkdownZipImagesAndRelativePaths(t *testing.T) {
	pngA := testPNG(t, 48, 32)
	gifB := testGIF(t, 24, 24)
	zipBytes := buildTestZip(t, map[string][]byte{
		"index.md": []byte("---\ntitle: Root Post\nslug: root-post\nstatus: publish\n---\n![A1](./images/a.png)\n![A2](images/a.png)"),
		"docs/guide.md": []byte("---\ntitle: Sub Guide\nslug: sub-guide\nkind: page\nstatus: publish\n---\n![Sub](./images/sub.png)\n![Shared](../img/b.gif)"),
		"images/a.png":        pngA,
		"docs/images/sub.png": pngA,
		"img/b.gif":           gifB,
	})

	exp, err := ParseMarkdownZip(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.LocalImages) != 3 {
		t.Fatalf("len(LocalImages) = %d, want 3", len(exp.LocalImages))
	}

	db := openDB(t)
	im, uploadDir := newTestImporter(t, db, nil, Options{})
	sum, err := im.ImportExport(context.Background(), exp)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Articles != 2 || sum.ImagesSucceeded != 3 || sum.ImagesFailed != 0 {
		t.Fatalf("summary = %+v, want Articles=2 ImagesSucceeded=3 ImagesFailed=0", sum)
	}

	var mediaCount int
	if err := db.Get(&mediaCount, `SELECT COUNT(*) FROM media`); err != nil || mediaCount != 3 {
		t.Fatalf("media count = %d (err=%v), want 3", mediaCount, err)
	}
	entries, err := os.ReadDir(uploadDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("ReadDir(%s) = %d entries, err=%v", uploadDir, len(entries), err)
	}

	for _, slug := range []string{"root-post", "sub-guide"} {
		var md, html string
		if err := db.QueryRow(`SELECT content_markdown, content_html FROM articles WHERE slug = ?`, slug).Scan(&md, &html); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		if !strings.Contains(md, "/uploads/") || !strings.Contains(html, "/uploads/") {
			t.Errorf("%s: relative paths were not replaced with /uploads/:\nmd=%s\nhtml=%s", slug, md, html)
		}
		for _, old := range []string{"./images/a.png", "images/a.png", "./images/sub.png", "../img/b.gif"} {
			if strings.Contains(md, old) {
				t.Errorf("%s: markdown still contains %q: %s", slug, old, md)
			}
		}
	}
}

func TestImportMarkdownZipDryRunDoesNotWriteFilesOrDB(t *testing.T) {
	zipBytes := buildTestZip(t, map[string][]byte{
		"post.md":      []byte("---\ntitle: Dry Run Post\nslug: dry-run-post\nstatus: publish\n---\n![A](./images/a.png)"),
		"images/a.png": testPNG(t, 32, 32),
	})
	exp, err := ParseMarkdownZip(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	im, uploadDir := newTestImporter(t, db, nil, Options{DryRun: true})
	sum, err := im.ImportExport(context.Background(), exp)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.DryRun || sum.Articles != 1 {
		t.Fatalf("dryRun summary = %+v", sum)
	}
	if _, err := os.Stat(uploadDir); !os.IsNotExist(err) {
		t.Errorf("dryRun created uploadDir: %v", err)
	}
	for _, table := range []string{"articles", "media"} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM `+table); err != nil || n != 0 {
			t.Errorf("%s rows after dryRun = %d (err=%v), want 0", table, n, err)
		}
	}
}

func TestImportMarkdownLangSavedToDB(t *testing.T) {
	zipBytes := buildTestZip(t, map[string][]byte{
		"en.md": []byte("---\ntitle: English Guide\nslug: en-guide\nlang: en\nstatus: publish\n---\nHello in English."),
		"ja.md": []byte("---\ntitle: Japanese Guide\nslug: ja-guide\nstatus: publish\n---\n日本語の本文です。"),
	})
	exp, err := ParseMarkdownZip(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	im, _ := newTestImporter(t, db, nil, Options{})
	if _, err := im.ImportExport(context.Background(), exp); err != nil {
		t.Fatal(err)
	}
	var enLang, jaLang string
	if err := db.Get(&enLang, `SELECT lang FROM articles WHERE slug = 'en-guide'`); err != nil || enLang != "en" {
		t.Errorf("en-guide lang = %q (err=%v), want 'en'", enLang, err)
	}
	if err := db.Get(&jaLang, `SELECT lang FROM articles WHERE slug = 'ja-guide'`); err != nil || jaLang != "ja" {
		t.Errorf("ja-guide lang = %q (err=%v), want 'ja'", jaLang, err)
	}
}
