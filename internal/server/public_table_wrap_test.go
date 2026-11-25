package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"sorus/static"
)

// 本文（.prose）は長い URL ではみ出さないよう overflow-wrap: anywhere にしているが、表のセルに継がれると
// セルの最小幅が1文字になり、短い列（年の「2024」など）が1文字ずつ縦に折り返される。セルでは通常の折り返しに戻し、
// 幅の足りない表は .prose table の横スクロールで見せる。セルの中のリンク（長い URL）とコードは途中で折ってよい。
func TestProseTableCellsDoNotBreakAnywhere(t *testing.T) {
	data, err := fs.ReadFile(static.FS, "css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	rule := func(selector string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`).FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("no rule for %s", selector)
		}
		return m[1]
	}
	if body := rule(".prose"); !strings.Contains(body, "overflow-wrap: anywhere") {
		t.Fatalf(".prose = {%s}, want overflow-wrap: anywhere (precondition of this test)", body)
	}
	cell := rule(".prose th, .prose td")
	for _, want := range []string{"overflow-wrap: normal", "word-break: normal"} {
		if !strings.Contains(cell, want) {
			t.Errorf(".prose th, .prose td = {%s}, want %s", cell, want)
		}
	}
	// セルの中の長い URL・コードは途中で折り、1つの語のために表全体（ほかの列）が押し縮められないようにする。
	if long := rule(".prose th a, .prose td a, .prose th code, .prose td code"); !strings.Contains(long, "overflow-wrap: anywhere") {
		t.Errorf("links and code in cells = {%s}, want overflow-wrap: anywhere", long)
	}
	if table := rule(".prose table"); !strings.Contains(table, "overflow-x: auto") {
		t.Errorf(".prose table = {%s}, want overflow-x: auto for wide tables", table)
	}
}
