package templates

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// emojiPattern catches Unicode emoji and decorative symbols in source, docs, and UI assets.
var emojiPattern = regexp.MustCompile(emojiRanges())

func emojiRanges() string {
	ranges := [][2]rune{
		{0x1F000, 0x1FAFF}, {0x2600, 0x27BF}, {0x2300, 0x23FF},
		{0x2B05, 0x2B07}, {0x2B1B, 0x2B1B}, {0x2B1C, 0x2B1C},
		{0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x203C, 0x203C},
		{0x2049, 0x2049}, {0x2139, 0x2139}, {0x24C2, 0x24C2},
		{0x3297, 0x3297}, {0x3299, 0x3299},
	}
	var b strings.Builder
	b.WriteByte('[')
	for _, r := range ranges {
		b.WriteRune(r[0])
		if r[0] != r[1] {
			b.WriteByte('-')
			b.WriteRune(r[1])
		}
	}
	b.WriteByte(']')
	return b.String()
}

func TestProjectSourceDocsAndUIContainNoEmoji(t *testing.T) {
	root := filepath.Clean("..")
	extensions := map[string]bool{
		".go": true, ".html": true, ".md": true, ".js": true, ".css": true,
		".svg": true, ".json": true, ".yaml": true, ".yml": true, ".xml": true,
		".txt": true, ".sh": true, ".sql": true, ".toml": true,
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == ".navigator" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		// This protected requirements file is outside the permitted edit scope.
		if filepath.Clean(path) == filepath.Join(root, "docs", "development_spec.md") || !extensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := emojiPattern.Find(data); loc != nil {
			t.Errorf("%s contains emoji or decorative symbol %q", path, string(loc))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
