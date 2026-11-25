package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ドキュメント（README.md・docs/*.md）の表記の約束を確かめる。
//   - 絵文字を使わない。
//   - 図は Mermaid で書き、罫線文字の ASCII アート（┌─┐│└┘├ など）を使わない。
//   - 「古い公開サーバー」の注意書き、開発ツールの内部名称（Orca）、スマートフォンのホーム画面に
//     追加する（PWA）案内、dev.env.example を直接読む手順を載せない。

func docsForStyleCheck(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{
		filepath.Join("..", "README.md"),
		filepath.Join("..", "docs", "*.md"),
	} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("found %d documents, want README.md and docs/*.md", len(files))
	}
	return files
}

func TestDocsHaveNoEmojiOrBoxDrawing(t *testing.T) {
	emoji := regexp.MustCompile(`[\x{1F000}-\x{1FFFF}\x{2600}-\x{27BF}\x{2B00}-\x{2BFF}\x{FE0F}\x{200D}]`)
	box := regexp.MustCompile(`[\x{2500}-\x{257F}]`)
	for _, path := range docsForStyleCheck(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if emoji.MatchString(line) {
				t.Errorf("%s:%d contains an emoji:\n%s", path, i+1, line)
			}
			if box.MatchString(line) {
				t.Errorf("%s:%d contains box-drawing characters; draw diagrams and trees with Mermaid:\n%s", path, i+1, line)
			}
		}
	}
}

func TestDocsDropObsoleteWording(t *testing.T) {
	banned := []struct {
		re   *regexp.Regexp
		why  string
		skip string
	}{
		{regexp.MustCompile(`古い公開サーバー|公開サーバーの sorus が古い`), "notes about outdated public servers were removed", ""},
		{regexp.MustCompile(`Orca`), "internal development tool names must not appear", ""},
		{regexp.MustCompile(`ホーム画面に追加`), "sorus is not offered as a home-screen (PWA) app", "phase18_mobile_offline_pwa_spec.md"},
		{regexp.MustCompile(`ENV_FILE=deploy/environments/dev\.env\.example`), "scripts/dev.sh creates and reads deploy/environments/dev.env", ""},
		{regexp.MustCompile(`VPS の業者|(?:^|[^事])業者`), "use サーバー会社 / クラウド事業者 instead of 業者", ""},
	}
	for _, path := range docsForStyleCheck(t) {
		base := filepath.Base(path)
		if base == "ux_security_docs_final_polish_spec.md" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, b := range banned {
				if b.skip == base {
					continue
				}
				if b.re.MatchString(line) {
					t.Errorf("%s:%d: %s:\n%s", path, i+1, b.why, line)
				}
			}
		}
	}
}

// 図のコードブロックは mermaid とし、中身が flowchart / sequenceDiagram などの図の種類で始まることを確かめる。
func TestDocsMermaidBlocksDeclareDiagramType(t *testing.T) {
	kinds := regexp.MustCompile(`^(flowchart (TD|TB|LR|RL|BT)|graph (TD|TB|LR|RL|BT)|sequenceDiagram|stateDiagram-v2|classDiagram|erDiagram|gantt)\b`)
	total := 0
	for _, path := range docsForStyleCheck(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		for i := 0; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) != "```mermaid" {
				continue
			}
			total++
			first := ""
			j := i + 1
			for ; j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```"); j++ {
				if first == "" && strings.TrimSpace(lines[j]) != "" {
					first = strings.TrimSpace(lines[j])
				}
			}
			if j == len(lines) {
				t.Errorf("%s:%d: mermaid block is not closed", path, i+1)
			}
			if !kinds.MatchString(first) {
				t.Errorf("%s:%d: mermaid block starts with %q, want a diagram type such as flowchart TD", path, i+1, first)
			}
			i = j
		}
	}
	if total == 0 {
		t.Fatal("found no mermaid blocks")
	}
}
