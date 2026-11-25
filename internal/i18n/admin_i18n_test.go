package i18n

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"text/template/parse"
	"unicode"
)

// 管理画面を英語（EN）で表示したときに、日本語が画面に出ないことを確かめるテスト群。
//
// テンプレートは text/template/parse で構文木にし、表示言語の分岐（{{if $ja}}…{{else}}…{{end}}、
// {{if eq .Lang "en"}} など）は英語側の枝だけをたどる。たどった先のテキスト（HTML コメントを除く）と、
// テンプレート中の文字列リテラル（"…"）に日本語が含まれていれば失敗にする。
// 翻訳は {{$.T "key"}} で辞書から引くので、辞書の EN 側に日本語が無いことも別に確かめる。

const adminTemplateDir = "../../templates/admin"

// isJapanese はひらがな・カタカナ・漢字・全角記号（。、「」など）を日本語の文字として扱う。
func isJapanese(r rune) bool {
	return unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Han, r) ||
		(r >= 0x3000 && r <= 0x303f) || (r >= 0xff01 && r <= 0xff60)
}

func containsJapanese(s string) bool { return strings.IndexFunc(s, isJapanese) >= 0 }

// allowedJapanese は英語表示でもそのまま出してよい日本語。
//   - 「日本語」: 言語の切り替え先や記事の言語を、その言語自身の名前で示すもの。
//   - 「sorus バックアップ」: 同期フォルダの中に実際に作るフォルダの名前（internal/backup.FolderName）。
var allowedJapanese = []string{"日本語", "sorus バックアップ"}

func stripAllowed(s string) string {
	for _, a := range allowedJapanese {
		s = strings.ReplaceAll(s, a, "")
	}
	return s
}

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	jsLineComment = regexp.MustCompile(`(?m)(^|[^:\\'"])//.*$`)
	jsBlockComent = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// langCond は {{if …}} の条件が表示言語の判定かどうかを調べる。
// 判定なら、条件が真のときの言語（"ja" か "en"）を返す。dotIsPage は . がページのデータか（range・with の中では
// . が別の値になるので、.Lang は記事の言語などで、表示言語ではない）。
func langCond(pipe *parse.PipeNode, jaVars map[string]bool, dotIsPage bool) (string, bool) {
	if pipe == nil || len(pipe.Cmds) != 1 || len(pipe.Decl) > 0 {
		return "", false
	}
	args := pipe.Cmds[0].Args
	if len(args) == 1 {
		if v, ok := args[0].(*parse.VariableNode); ok && len(v.Ident) == 1 {
			if jaTrue, ok := jaVars[v.Ident[0]]; ok {
				if jaTrue {
					return "ja", true
				}
				return "en", true
			}
		}
		// Go から返す自己完結の画面（internal/server の siteDeletedTemplate など）は {{if .JA}} で分ける。
		if f, ok := args[0].(*parse.FieldNode); ok && dotIsPage && len(f.Ident) == 1 && f.Ident[0] == "JA" {
			return "ja", true
		}
		return "", false
	}
	if len(args) != 3 {
		return "", false
	}
	fn, ok := args[0].(*parse.IdentifierNode)
	if !ok || (fn.Ident != "eq" && fn.Ident != "ne") {
		return "", false
	}
	if !isUILangField(args[1], dotIsPage) {
		return "", false
	}
	lit, ok := args[2].(*parse.StringNode)
	if !ok || (lit.Text != "ja" && lit.Text != "en") {
		return "", false
	}
	trueLang := lit.Text
	if fn.Ident == "ne" {
		if trueLang == "ja" {
			trueLang = "en"
		} else {
			trueLang = "ja"
		}
	}
	return trueLang, true
}

// isUILangField は .Lang / $.Lang（ページの表示言語）かどうかを返す。
// $a.Lang や $.Data.Lang、range・with の中の .Lang（dotIsPage が false）は記事の言語なので含めない。
func isUILangField(n parse.Node, dotIsPage bool) bool {
	switch v := n.(type) {
	case *parse.FieldNode:
		return dotIsPage && len(v.Ident) == 1 && v.Ident[0] == "Lang"
	case *parse.VariableNode:
		return len(v.Ident) == 2 && v.Ident[0] == "$" && v.Ident[1] == "Lang"
	}
	return false
}

// blankOut は re に一致した部分を、改行だけ残して消す。
func blankOut(s string, re *regexp.Regexp) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		return strings.Repeat("\n", strings.Count(m, "\n"))
	})
}

type finding struct {
	file string
	line int
	text string
}

type walker struct {
	file     string
	src      string
	jaVars   map[string]bool
	findings []finding
	// innerDot は range・with の本体の深さ。0 より大きければ . はページのデータではない。
	innerDot int
}

func (w *walker) report(pos parse.Pos, text string) {
	line := 1 + strings.Count(w.src[:int(pos)], "\n")
	w.findings = append(w.findings, finding{file: w.file, line: line, text: strings.TrimSpace(text)})
}

func (w *walker) walk(n parse.Node) {
	switch n := n.(type) {
	case nil:
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			w.walk(c)
		}
	case *parse.TextNode:
		// コメントは改行を残して空白に置き換え、行番号がずれないようにする。
		text := blankOut(string(n.Text), htmlCommentRe)
		text = blankOut(text, jsBlockComent)
		text = jsLineComment.ReplaceAllString(text, "$1")
		offset, cursor := 0, int(n.Pos)
		for _, ln := range strings.Split(text, "\n") {
			if containsJapanese(stripAllowed(ln)) {
				// 空白の除去（{{- や -}}）で位置がずれることがあるので、元の行を探して位置を決める。
				pos := n.Pos + parse.Pos(offset)
				if i := strings.Index(w.src[cursor:], strings.TrimSpace(ln)); i >= 0 {
					pos = parse.Pos(cursor + i)
					cursor += i + 1
				}
				w.report(pos, ln)
			}
			offset += len(ln) + 1
		}
	case *parse.ActionNode:
		w.walkPipe(n.Pipe)
	case *parse.IfNode:
		if lang, ok := langCond(n.Pipe, w.jaVars, w.innerDot == 0); ok {
			if lang == "en" {
				w.walk(n.List)
			} else {
				w.walk(n.ElseList)
			}
			return
		}
		w.walkPipe(n.Pipe)
		w.walk(n.List)
		w.walk(n.ElseList)
	case *parse.RangeNode:
		w.walkPipe(n.Pipe)
		w.walkInner(n.List)
		w.walk(n.ElseList)
	case *parse.WithNode:
		w.walkPipe(n.Pipe)
		w.walkInner(n.List)
		w.walk(n.ElseList)
	case *parse.TemplateNode:
		w.walkPipe(n.Pipe)
	}
}

// walkInner は range・with の本体（. が別の値になる部分）をたどる。
func (w *walker) walkInner(n *parse.ListNode) {
	w.innerDot++
	w.walk(n)
	w.innerDot--
}

func (w *walker) walkPipe(p *parse.PipeNode) {
	if p == nil {
		return
	}
	// {{$ja := ne .Lang "en"}} のような表示言語の変数を覚えておく。
	if len(p.Decl) == 1 {
		if lang, ok := langCond(&parse.PipeNode{Cmds: p.Cmds}, w.jaVars, w.innerDot == 0); ok {
			w.jaVars[p.Decl[0].Ident[0]] = lang == "ja"
			return
		}
	}
	for _, c := range p.Cmds {
		// eq・ne の比較に使う文字列は画面に出ない（サーバーが返す値との比較など）ので調べない。
		compare := false
		if len(c.Args) > 0 {
			if fn, ok := c.Args[0].(*parse.IdentifierNode); ok && (fn.Ident == "eq" || fn.Ident == "ne") {
				compare = true
			}
		}
		for _, a := range c.Args {
			if _, ok := a.(*parse.StringNode); ok && compare {
				continue
			}
			w.walkArg(a)
		}
	}
}

func (w *walker) walkArg(a parse.Node) {
	switch a := a.(type) {
	case *parse.StringNode:
		if containsJapanese(stripAllowed(a.Text)) {
			w.report(a.Pos, a.Quoted)
		}
	case *parse.PipeNode:
		w.walkPipe(a)
	}
}

func adminTemplateFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(adminTemplateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".html") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk admin templates: %v", err)
	}
	if len(files) < 30 {
		t.Fatalf("expected the admin templates under %s, found only %d files", adminTemplateDir, len(files))
	}
	sort.Strings(files)
	return files
}

func parseTemplateFile(t *testing.T, path string) (string, map[string]*parse.Tree) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)
	tr := parse.New(path)
	tr.Mode = parse.SkipFuncCheck
	trees := map[string]*parse.Tree{}
	if _, err := tr.Parse(src, "{{", "}}", trees); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return src, trees
}

// enTemplateFindings は、英語表示でたどる部分に残っている日本語を集める。
func enTemplateFindings(t *testing.T) []finding {
	t.Helper()
	var all []finding
	for _, path := range adminTemplateFiles(t) {
		src, trees := parseTemplateFile(t, path)
		names := make([]string, 0, len(trees))
		for name := range trees {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			w := &walker{file: strings.TrimPrefix(path, "../../"), src: src, jaVars: map[string]bool{}}
			w.walk(trees[name].Root)
			all = append(all, w.findings...)
		}
	}
	return all
}

// TestAdminTemplatesHaveNoJapaneseInEnglishMode は、管理画面のすべてのテンプレートで、
// 英語表示のときに通る部分に日本語のハードコードが残っていないことを確かめる。
// 日本語を書き足すときは {{$.T "key"}} で辞書から引くか、{{if $ja}}日本語{{else}}English{{end}} にする。
func TestAdminTemplatesHaveNoJapaneseInEnglishMode(t *testing.T) {
	findings := enTemplateFindings(t)
	seen := map[string]bool{}
	for _, f := range findings {
		key := fmt.Sprintf("%s:%d", f.file, f.line)
		if seen[key] {
			continue
		}
		seen[key] = true
		text := f.text
		if len([]rune(text)) > 120 {
			text = string([]rune(text)[:120]) + "…"
		}
		t.Errorf("%s:%d: Japanese text shown in English mode: %s", f.file, f.line, text)
	}
}

// goTemplateRe は internal/server の Go のソースに書いた HTML テンプレート（template.New("…").Parse(`…`)）を取り出す。
var goTemplateRe = regexp.MustCompile("(?s)template\\.New\\(\"([^\"]+)\"\\)(?:\\.Funcs\\([^)]*\\))?\\.Parse\\(`(.*?)`\\)")

// TestServedTemplatesHaveNoJapaneseInEnglishMode は、Go から返す管理画面の部分・画面（更新のお知らせ・自動バックアップの
// 状態・サイトの削除の完了画面・終了の確認など）も、英語表示で日本語が出ないことを確かめる。
func TestServedTemplatesHaveNoJapaneseInEnglishMode(t *testing.T) {
	files, err := filepath.Glob("../../internal/server/*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		for _, m := range goTemplateRe.FindAllStringSubmatchIndex(src, -1) {
			found++
			name, body := src[m[2]:m[3]], src[m[4]:m[5]]
			tr := parse.New(name)
			tr.Mode = parse.SkipFuncCheck
			trees := map[string]*parse.Tree{}
			if _, err := tr.Parse(body, "{{", "}}", trees); err != nil {
				t.Fatalf("%s: parse %s: %v", path, name, err)
			}
			w := &walker{file: name, src: body, jaVars: map[string]bool{}}
			w.walk(trees[name].Root)
			// テンプレートの中の行番号を、ソースファイルの行番号に直す。
			base := strings.Count(src[:m[4]], "\n")
			for _, f := range w.findings {
				t.Errorf("%s:%d (template %s): Japanese text shown in English mode: %s", strings.TrimPrefix(path, "../../"), base+f.line, name, f.text)
			}
		}
	}
	if found < 5 {
		t.Fatalf("expected the HTML templates in internal/server, found only %d", found)
	}
}

// TestAdminWalkerFollowsEnglishBranch は、上の検査が言語の分岐を正しく読み分けることを確かめる。
func TestAdminWalkerFollowsEnglishBranch(t *testing.T) {
	cases := []struct {
		src  string
		want int
	}{
		{`{{$ja := ne .Lang "en"}}{{if $ja}}日本語の文{{else}}English{{end}}`, 0},
		{`{{$ja := eq .Lang "ja"}}{{if $ja}}日本語の文{{else}}英語のはず{{end}}`, 1},
		{`{{if eq $.Lang "en"}}English{{else}}日本語の文{{end}}`, 0},
		{`{{if eq .Lang "ja"}} さん{{end}}`, 0},
		{`{{if ne .Lang "en"}}日本語の文{{end}}`, 0},
		{`{{if eq $.Data.Lang "ja"}}記事の言語は表示言語ではない{{end}}`, 1},
		{`<p>保存</p>`, 1},
		{`<!-- コメント --><p>Save</p>`, 0},
		{`<script>// コメント
var a = 1; /* コメント */</script>`, 0},
		{`<button title="閉じる">x</button>`, 1},
		{`{{or .Name "無題"}}`, 1},
		{`{{$.T "close"}} 日本語`, 0},
		{`{{if eq .SavedBy "不明"}}Unknown{{end}}`, 0},
		// range・with の中の .Lang は記事の言語なので、表示言語の分岐として扱わない（両方の枝を調べる）。
		{`{{range .Data.Rows}}{{if eq .Lang "en"}}EN{{else}}要更新{{end}}{{end}}`, 1},
		{`{{with .Data}}{{if eq .Lang "en"}}EN{{else}}要更新{{end}}{{end}}`, 1},
		{`{{range .Data.Rows}}{{if eq $.Lang "en"}}Needs update{{else}}要更新{{end}}{{end}}`, 0},
		{`{{range .Data.Rows}}x{{end}}{{if eq .Lang "en"}}EN{{else}}日本語{{end}}`, 0},
		{`{{if .JA}}削除しました{{else}}Deleted{{end}}`, 0},
		{`{{with .Site}}{{if .JA}}削除しました{{else}}Deleted{{end}}{{end}}`, 1},
	}
	for _, c := range cases {
		tr := parse.New("t")
		tr.Mode = parse.SkipFuncCheck
		trees := map[string]*parse.Tree{}
		if _, err := tr.Parse(c.src, "{{", "}}", trees); err != nil {
			t.Fatalf("parse %q: %v", c.src, err)
		}
		w := &walker{file: "t", src: c.src, jaVars: map[string]bool{}}
		w.walk(trees["t"].Root)
		if len(w.findings) != c.want {
			t.Errorf("%q: got %d findings %v, want %d", c.src, len(w.findings), w.findings, c.want)
		}
	}
}

var tKeyRe = regexp.MustCompile(`\.T "([^"]+)"`)

// TestAdminTemplateKeysExist は、テンプレートが {{$.T "key"}} で引くキーが辞書の JA・EN 両方にあることを確かめる。
// 辞書に無いキーはキー名そのものが画面に出てしまう。
func TestAdminTemplateKeysExist(t *testing.T) {
	for _, path := range adminTemplateFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tKeyRe.FindAllStringSubmatch(string(data), -1) {
			key := m[1]
			if _, ok := catalog[JA][key]; !ok {
				t.Errorf("%s: key %q is missing in the JA catalog", path, key)
			}
			if _, ok := catalog[EN][key]; !ok {
				t.Errorf("%s: key %q is missing in the EN catalog", path, key)
			}
		}
	}
}

// TestEnglishCatalogHasNoJapanese は、辞書の EN 側に日本語が混ざっていないことを確かめる。
func TestEnglishCatalogHasNoJapanese(t *testing.T) {
	keys := make([]string, 0, len(catalog[EN]))
	for k := range catalog[EN] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := catalog[EN][k]; containsJapanese(stripAllowed(v)) {
			t.Errorf("EN catalog %q contains Japanese: %q", k, v)
		}
	}
}

// TestSettingsNavLabels は、設定の入口と左メニューで同じ名前を使う項目の文言を固定する（admin_ux_redesign_spec.md §2-5）。
func TestSettingsNavLabels(t *testing.T) {
	want := map[string][2]string{
		"settings_nav_ai":         {"AI設定", "AI Settings"},
		"settings_nav_backup":     {"バックアップと復元", "Backup & Restore"},
		"settings_nav_migration":  {"データの取り込み", "Import Data"},
		"settings_nav_remote":     {"公開サーバー", "Publish Server"},
		"settings_nav_operations": {"サイト運用", "Site Operations"},
		"import_title":            {"データの取り込み", "Import Data"},
	}
	for key, w := range want {
		if got := Translate("ja", key); got != w[0] {
			t.Errorf("JA %q = %q, want %q", key, got, w[0])
		}
		if got := Translate("en", key); got != w[1] {
			t.Errorf("EN %q = %q, want %q", key, got, w[1])
		}
	}
	for _, key := range []string{"hub_remote_desc", "hub_operations_desc", "operations_title"} {
		for _, lang := range []Lang{JA, EN} {
			if catalog[lang][key] == "" {
				t.Errorf("%s catalog is missing %q", lang, key)
			}
		}
	}
}

// adminScripts は管理画面で読み込む static/js のファイル。公開サイト用（public.js・static-search.js・lightbox.js・math.js）は含めない。
var adminScripts = []string{
	"admin.js", "sorus_common.js", "dashboard.js", "desktop_care.js", "editor.js", "gallery_modal.js", "manuscript.js", "media.js",
	"media_picker.js", "offline_draft.js", "reflect.js", "remote_invite.js", "remote_maintenance.js", "remote_site_access.js",
	"remote_sync.js", "social.js",
}

// adminScriptSources は、管理画面の JavaScript（static/js）のソースを名前ごとに返す。
func adminScriptSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range adminScripts {
		data, err := os.ReadFile(filepath.Join("../../static/js", name))
		if err != nil {
			t.Fatal(err)
		}
		out["static/js/"+name] = string(data)
	}
	return out
}

// jsStringLiterals は JavaScript のソースから、コメントと正規表現リテラルを除いた文字列リテラル（'…'・"…"・`…`）の
// 中身と行番号を返す。正規表現リテラルは、直前の記号（( , = : など）や return のあとの / で始まるものとみなす。
func jsStringLiterals(src string) []finding {
	var out []finding
	line := 1
	last := byte(0) // 直前の、空白でもコメントでもない文字
	lastWord := ""
	regexAllowed := func() bool {
		if last == 0 || strings.IndexByte("(,=:[!&|?{};+-*%~^<>", last) >= 0 {
			return true
		}
		return lastWord == "return" || lastWord == "typeof"
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			line++
		case c == ' ' || c == '\t' || c == '\r':
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return out
			}
			line += strings.Count(src[i:i+2+end+2], "\n")
			i += 2 + end + 1
		case c == '/' && regexAllowed():
			inClass := false
			for i++; i < len(src) && src[i] != '\n'; i++ {
				if src[i] == '\\' {
					i++
				} else if src[i] == '[' {
					inClass = true
				} else if src[i] == ']' {
					inClass = false
				} else if src[i] == '/' && !inClass {
					break
				}
			}
			last, lastWord = ')', ""
		case c == '\'' || c == '"' || c == '`':
			start, startLine := i+1, line
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' {
					i++
				} else if src[i] == '\n' {
					line++
				}
			}
			out = append(out, finding{line: startLine, text: src[start:min(i, len(src))]})
			last, lastWord = ')', ""
		default:
			if c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				j := i
				for j < len(src) && (src[j] == '_' || src[j] == '$' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
					j++
				}
				lastWord, last, i = src[i:j], 'a', j-1
			} else {
				last, lastWord = c, ""
			}
		}
	}
	return out
}

// TestAdminScriptsHaveNoJapaneseStrings は、管理画面の JavaScript の文字列に日本語を直接書いていないことを確かめる。
// 画面に出す文言は window.sorusT("key") で辞書から引く（partials/js_i18n.html が表示言語の文言を埋め込む）。
func TestAdminScriptsHaveNoJapaneseStrings(t *testing.T) {
	for name, src := range adminScriptSources(t) {
		for _, lit := range jsStringLiterals(src) {
			if containsJapanese(stripAllowed(lit.text)) {
				t.Errorf("%s (script line %d): Japanese string literal %q; use window.sorusT with a catalog key", name, lit.line, lit.text)
			}
		}
	}
}

var jsKeyRe = regexp.MustCompile(`\bT\('([a-z0-9_]+)'`)

// TestAdminScriptKeysAreEmbedded は、JavaScript が T('key') で引くキーが辞書の JA・EN にあり、
// partials/js_i18n.html で window.SORUS_I18N に埋め込まれていることを確かめる。
func TestAdminScriptKeysAreEmbedded(t *testing.T) {
	partial, err := os.ReadFile(filepath.Join(adminTemplateDir, "partials", "js_i18n.html"))
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range adminScriptSources(t) {
		for _, m := range jsKeyRe.FindAllStringSubmatch(src, -1) {
			key := m[1]
			for _, lang := range []Lang{JA, EN} {
				if _, ok := catalog[lang][key]; !ok {
					t.Errorf("%s: key %q is missing in the %s catalog", name, key, lang)
				}
			}
			if !strings.Contains(string(partial), `"`+key+`": {{.T "`+key+`"}}`) {
				t.Errorf("%s: key %q is not embedded in partials/js_i18n.html", name, key)
			}
		}
	}
	layout, err := os.ReadFile(filepath.Join(adminTemplateDir, "layout.html"))
	if err != nil {
		t.Fatal(err)
	}
	if i, j := strings.Index(string(layout), `{{template "js_i18n" .}}`), strings.Index(string(layout), `js/admin.js`); i < 0 || j < 0 || i > j {
		t.Errorf("layout.html must embed js_i18n before loading admin.js")
	}
}

// TestJSStringLiterals は、JavaScript の文字列の取り出しが、コメントと正規表現を文字列と取り違えないことを確かめる。
func TestJSStringLiterals(t *testing.T) {
	src := "var a = '保存'; // 'コメント'\n/* \"コメント\" */ var r = s.replace(/[,，、']/g, \"x\");\nreturn /'/.test(b) ? `テンプレート` : 1 / 2 / 3;"
	var got []string
	for _, f := range jsStringLiterals(src) {
		got = append(got, f.text)
	}
	want := []string{"保存", "x", "テンプレート"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}
