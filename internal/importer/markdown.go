package importer

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxMarkdownArchiveFiles       = 1000
	maxMarkdownArchiveSize  int64 = 200 << 20
)

var (
	// ErrZipTooLarge は Markdown または Zip のサイズ・ファイル数が上限を超えたことを示す。
	ErrZipTooLarge = errors.New("importer: markdown zip is too large")
	// ErrNoMarkdownFiles は Zip 内に取り込み対象の Markdown (.md) が1件も無いことを示す。
	ErrNoMarkdownFiles = errors.New("importer: markdown zip has no markdown files")
	// ErrUnsafeZipPath は Zip 内にディレクトリトラバーサル等の危険なパスが含まれることを示す。
	ErrUnsafeZipPath = errors.New("importer: unsafe zip path")
	// ErrInvalidMarkdown は Markdown（主に YAML frontmatter）の構文が不正であることを示す。
	ErrInvalidMarkdown = errors.New("importer: invalid markdown")
)

// ParseMarkdown reads a single Markdown document and converts it to the common import format.
func ParseMarkdown(r io.Reader, filename string) (*Export, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxMarkdownArchiveSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > int(maxMarkdownArchiveSize) {
		return nil, fmt.Errorf("%w: markdown file is too large", ErrZipTooLarge)
	}
	p, err := parseMarkdownPost(b, filename)
	if err != nil {
		return nil, err
	}
	return &Export{Posts: []Post{p}}, nil
}

// ParseMarkdownZip reads Markdown files from a ZIP without extracting paths to disk.
func ParseMarkdownZip(r io.ReaderAt, size int64) (*Export, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("importer: invalid markdown zip: %w", err)
	}
	if len(zr.File) > maxMarkdownArchiveFiles {
		return nil, fmt.Errorf("%w: too many files in markdown zip", ErrZipTooLarge)
	}
	exp := &Export{LocalImages: map[string][]byte{}}
	var total int64
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		clean := path.Clean(name)
		if strings.HasPrefix(name, "/") || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(name) || path.IsAbs(name) || (len(name) >= 2 && name[1] == ':') {
			return nil, fmt.Errorf("%w %q", ErrUnsafeZipPath, f.Name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > uint64(maxMarkdownArchiveSize) || total+int64(f.UncompressedSize64) > maxMarkdownArchiveSize {
			return nil, ErrZipTooLarge
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		remaining := maxMarkdownArchiveSize - total
		b, readErr := io.ReadAll(io.LimitReader(rc, remaining+1))
		closeErr := rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if int64(len(b)) > remaining {
			return nil, ErrZipTooLarge
		}
		total += int64(len(b))
		ext := strings.ToLower(path.Ext(clean))
		if isMarkdownZipImageExt(ext) {
			exp.LocalImages[clean] = b
			continue
		}
		if ext != ".md" {
			continue
		}
		p, err := parseMarkdownPost(b, clean)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", clean, err)
		}
		exp.Posts = append(exp.Posts, p)
	}
	if len(exp.Posts) == 0 {
		return nil, ErrNoMarkdownFiles
	}
	return exp, nil
}

func isMarkdownZipImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
	}
}

type markdownFrontmatter struct {
	Title       string   `yaml:"title"`
	Slug        string   `yaml:"slug"`
	Date        string   `yaml:"date"`
	Kind        string   `yaml:"kind"`
	Category    string   `yaml:"category"`
	Tags        []string `yaml:"tags"`
	Status      string   `yaml:"status"`
	Lang        string   `yaml:"lang"`
	Description string   `yaml:"description"`
}

func parseMarkdownPost(src []byte, filename string) (Post, error) {
	body := strings.TrimSpace(string(bytes.TrimPrefix(src, []byte{0xef, 0xbb, 0xbf})))
	var fm markdownFrontmatter
	if strings.HasPrefix(body, "---\n") || strings.HasPrefix(body, "---\r\n") {
		lines := strings.Split(body, "\n")
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(strings.TrimSuffix(lines[i], "\r")) == "---" {
				end = i
				break
			}
		}
		if end < 0 {
			return Post{}, fmt.Errorf("%w: unterminated frontmatter", ErrInvalidMarkdown)
		}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fm); err != nil {
			return Post{}, fmt.Errorf("%w: invalid frontmatter: %w", ErrInvalidMarkdown, err)
		}
		body = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	}
	cleanPath := path.Clean(strings.ReplaceAll(filename, "\\", "/"))
	base := strings.TrimSuffix(path.Base(cleanPath), path.Ext(cleanPath))
	title := strings.TrimSpace(fm.Title)
	if title == "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "# ") {
				title = strings.TrimSpace(strings.TrimLeft(line, "# "))
				break
			}
		}
	}
	if title == "" {
		title = strings.ReplaceAll(strings.ReplaceAll(base, "-", " "), "_", " ")
	}
	slug := strings.TrimSpace(fm.Slug)
	if slug == "" {
		slug = base
	}
	p := Post{Title: title, Slug: slug, ContentMarkdown: body, Type: "post", Status: "draft", Creator: DefaultAuthor, Lang: fm.Lang, Description: fm.Description, SourcePath: cleanPath}
	if fm.Kind == "page" {
		p.Type = "page"
	}
	if fm.Status == "publish" || fm.Status == "published" {
		p.Status = "publish"
	}
	if fm.Date != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02 15:04:05"} {
			if d, e := time.Parse(layout, fm.Date); e == nil {
				p.Date = d.UTC()
				break
			}
		}
	}
	if fm.Category != "" {
		p.Categories = []Term{{Name: fm.Category}}
	}
	for _, tag := range fm.Tags {
		if strings.TrimSpace(tag) != "" {
			p.Tags = append(p.Tags, Term{Name: strings.TrimSpace(tag)})
		}
	}
	return p, nil
}
