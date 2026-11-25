package templates

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"sorus/internal/i18n"
)

// 管理画面の配色と文章量の決まり（admin UX v4）。
//
//   - 色: 主要＝黒（.btn-primary）、副次＝白＋枠（.btn-secondary）、危険＝rose（.btn-danger）、成功＝emerald、注意＝amber、
//     情報＝sky、それ以外は neutral/zinc。青・indigo・slate は使わない。枠付きのお知らせの箱は .notice-*（admin.css）を使い、
//     色を手で書かない（画面ごとに枠や文字の濃さがばらつくため）。
//   - 文章: 本文の段落は短い1文（結論・次の操作）にし、理由・手順・例外は「?」か <details> に入れる。安全の警告
//     （.notice-warn・.notice-error・role="alert"）は本文に残す。

// adminUIFiles は管理画面のテンプレート、管理画面が読み込む JS（templates/admin から参照しているもの）、
// サーバーが画面に埋め込む JS（internal/server/*_script.go）を返す。公開サイトの JS は対象外。
func adminUIFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	jsRef := regexp.MustCompile(`js/([a-z0-9_.-]+\.js)`)
	err := filepath.WalkDir("admin", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		content := read(path)
		files[path] = content
		for _, m := range jsRef.FindAllStringSubmatch(content, -1) {
			js := filepath.Join("..", "static", "js", m[1])
			if _, ok := files[js]; !ok {
				files[js] = read(js)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Glob(filepath.Join("..", "internal", "server", "*_script.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		files[s] = read(s)
	}
	for _, must := range []string{filepath.Join("..", "static", "js", "admin.js"), filepath.Join("..", "static", "js", "editor.js"),
		filepath.Join("..", "static", "js", "social.js"), filepath.Join("admin", "article_list.html")} {
		if _, ok := files[must]; !ok {
			t.Fatalf("admin UI files do not include %s", must)
		}
	}
	return files
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var (
	// offPaletteColor は決まりにない色（青・indigo・slate）の Tailwind クラス。
	offPaletteColor = regexp.MustCompile(`\b(?:indigo|slate|blue)-\d{2,3}\b`)
	// classAttr はテンプレートの class 属性と、JS で className に入れる文字列。
	classAttr = regexp.MustCompile(`class="([^"]*)"|className\s*=\s*'([^']*)'|el\('[a-z]+',\s*'([^']*)'`)
	boxBorder = regexp.MustCompile(`\bborder-(amber|rose|emerald|sky)-\d{3}\b`)
	boxRound  = regexp.MustCompile(`\brounded(?:-md|-lg|-xl)?\b`)
)

func TestAdminUIUsesStandardColors(t *testing.T) {
	files := adminUIFiles(t)
	for _, name := range sortedKeys(files) {
		content := files[name]
		for i, line := range strings.Split(content, "\n") {
			if m := offPaletteColor.FindString(line); m != "" {
				t.Errorf("%s:%d uses %q; use .btn-*, .notice-* or neutral/zinc instead of blue, indigo or slate", name, i+1, m)
			}
			// 枠付きのお知らせの箱（角丸＋色の枠＋薄い色の背景）を手で書かず、.notice-* を使う。バッジ（rounded-full）は対象外。
			for _, m := range classAttr.FindAllStringSubmatch(line, -1) {
				cls := m[1] + m[2] + m[3]
				b := boxBorder.FindStringSubmatch(cls)
				if b == nil || strings.Contains(cls, "notice") || strings.Contains(cls, "rounded-full") || !boxRound.MatchString(cls) {
					continue
				}
				if regexp.MustCompile(`(?:^|\s)bg-` + b[1] + `-50(?:\s|$)`).MatchString(cls) {
					t.Errorf("%s:%d hand-writes a %s notice box (%q); use .notice .notice-*", name, i+1, b[1], cls)
				}
			}
		}
	}
	css := readTemplate(t, "..", "static", "css", "admin.css")
	expectAll(t, "admin.css", css, ".notice-success", ".notice-warn", ".notice-error", ".notice-info", ".notice-neutral")
}

// maxBodyTextRunes は本文の段落（<p>）に出す日本語の文字数（空白を除く）の上限。これを超える説明は「?」か <details> に入れる。
const maxBodyTextRunes = 90

func TestAdminBodyTextIsShort(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("admin", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	partials, err := filepath.Glob(filepath.Join("admin", "partials", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, partials...)
	para := regexp.MustCompile(`(?s)<p\b[^>]*>(.*?)</p>`)
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		html := resolveForJapanese(stripNonBody(string(b)))
		for _, m := range para.FindAllStringSubmatch(html, -1) {
			text := visibleText(m[1])
			if !hasJapanese(text) {
				continue
			}
			if n := len([]rune(text)); n > maxBodyTextRunes {
				t.Errorf("%s: body paragraph has %d characters (max %d); keep one short sentence and move the details into a ? tooltip or <details>: %.60s…",
					name, n, maxBodyTextRunes, text)
			}
		}
	}
}

// 文字数の数え方そのものを確かめる（言語の分岐は日本語、ほかの分岐は長いほう、注意・「?」・<details> は数えない）。
func TestAdminBodyTextCounter(t *testing.T) {
	long := strings.Repeat("あ", maxBodyTextRunes+1)
	cases := []struct {
		src  string
		want int
	}{
		{`<p>{{if $ja}}短い文です。{{else}}` + strings.Repeat("x", 200) + `{{end}}</p>`, len([]rune("短い文です。"))},
		{`<p>{{if eq $.Lang "en"}}English{{else}}日本語{{end}}</p>`, 3},
		{`<p>{{if .A}}一{{else if .B}}二二{{else}}三三三{{end}}</p>`, 3},
		{`<p>{{.T "settings"}}</p>`, len([]rune(i18n.Translate("ja", "settings")))},
		{`<p>本文<span class="help-tip"><button>?</button><span class="help-tip-pop" hidden><span>` + long + `</span></span></span></p>`, 2},
	}
	para := regexp.MustCompile(`(?s)<p\b[^>]*>(.*?)</p>`)
	for _, tc := range cases {
		m := para.FindStringSubmatch(resolveForJapanese(stripNonBody(tc.src)))
		if m == nil {
			t.Fatalf("%s: paragraph lost", tc.src)
		}
		if got := len([]rune(visibleText(m[1]))); got != tc.want {
			t.Errorf("%s: counted %d, want %d", tc.src, got, tc.want)
		}
	}
	for _, src := range []string{
		`<div class="notice notice-warn"><p>` + long + `</p></div>`,
		`<section role="alert"><p>` + long + `</p></section>`,
		`<details><summary>詳しく</summary><p>` + long + `</p></details>`,
	} {
		if strings.Contains(stripNonBody(src), long) {
			t.Errorf("%s: should not be counted as body text", src)
		}
	}
}

var (
	nonBodyBlock = regexp.MustCompile(`(?s)<script\b.*?</script>|<style\b.*?</style>|<pre\b.*?</pre>|\{\{/\*.*?\*/\}\}`)
	openTag      = regexp.MustCompile(`<(div|section|p|span|details|ul|ol)\b([^>]*)>`)
	warnAttr     = regexp.MustCompile(`notice-warn|notice-error|role="alert"`)
)

// stripNonBody はスクリプト・コメント・安全の警告の箱・「?」のポップオーバー・<details> の中身を除く。
func stripNonBody(s string) string {
	s = nonBodyBlock.ReplaceAllString(s, "")
	for pos := 0; ; {
		loc := openTag.FindStringSubmatchIndex(s[pos:])
		if loc == nil {
			return s
		}
		start := pos + loc[0]
		tag, attrs := s[pos+loc[2]:pos+loc[3]], s[pos+loc[4]:pos+loc[5]]
		drop := tag == "details" || warnAttr.MatchString(attrs) || (tag == "span" && strings.Contains(attrs, `class="help-tip"`))
		if !drop {
			pos = pos + loc[1]
			continue
		}
		end := closingTag(s, start, tag)
		if end < 0 {
			pos = pos + loc[1]
			continue
		}
		s = s[:start] + s[end:]
		pos = start
	}
}

// closingTag は s[start:] で始まる tag の要素の終わり（閉じタグの直後）を返す。見つからなければ -1。
func closingTag(s string, start int, tag string) int {
	open := regexp.MustCompile(`<` + tag + `\b|</` + tag + `>`)
	depth := 0
	for _, loc := range open.FindAllStringIndex(s[start:], -1) {
		if strings.HasPrefix(s[start+loc[0]:], "</") {
			depth--
			if depth == 0 {
				return start + loc[1]
			}
		} else {
			depth++
		}
	}
	return -1
}

var action = regexp.MustCompile(`(?s)\{\{-?\s*(.*?)\s*-?\}\}`)

// resolveForJapanese はテンプレートの {{if}} を、日本語の読者に見える分岐（$ja・.Lang の判定）か、それ以外は最も長い分岐に
// 置き換え、{{.T "key"}} を日本語の文に置き換える。ほかのアクションは消す（中身の分からない値は数えない）。
func resolveForJapanese(s string) string {
	type frame struct {
		cond     string
		branches []strings.Builder
		isIf     bool
	}
	stack := []*frame{{branches: make([]strings.Builder, 1)}}
	cur := func() *strings.Builder { f := stack[len(stack)-1]; return &f.branches[len(f.branches)-1] }
	last := 0
	for _, loc := range action.FindAllStringSubmatchIndex(s, -1) {
		cur().WriteString(s[last:loc[0]])
		last = loc[1]
		a := s[loc[2]:loc[3]]
		switch {
		case strings.HasPrefix(a, "if "):
			stack = append(stack, &frame{cond: strings.TrimPrefix(a, "if "), branches: make([]strings.Builder, 1), isIf: true})
		case strings.HasPrefix(a, "with "), strings.HasPrefix(a, "range "), strings.HasPrefix(a, "define "), strings.HasPrefix(a, "block "):
			stack = append(stack, &frame{branches: make([]strings.Builder, 1)})
		case a == "else" || strings.HasPrefix(a, "else "):
			f := stack[len(stack)-1]
			f.branches = append(f.branches, strings.Builder{})
		case a == "end":
			if len(stack) == 1 {
				continue
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			cur().WriteString(pickBranch(f.cond, f.isIf, f.branches))
		case strings.HasPrefix(a, ".T ") || strings.HasPrefix(a, "$.T "):
			if m := regexp.MustCompile(`^\$?\.T "([^"]+)"`).FindStringSubmatch(a); m != nil && i18n.Has(m[1]) {
				cur().WriteString(i18n.Translate("ja", m[1]))
			}
		}
	}
	cur().WriteString(s[last:])
	for len(stack) > 1 { // 閉じていないブロックは、そのまま足す
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		cur().WriteString(pickBranch(f.cond, f.isIf, f.branches))
	}
	return stack[0].branches[0].String()
}

var (
	jaFirst = regexp.MustCompile(`^(?:\$ja|ne \$?\.Lang "en"|eq \$?\.Lang "ja")$`)
	enFirst = regexp.MustCompile(`^(?:not \$ja|eq \$?\.Lang "en")$`)
)

func pickBranch(cond string, isIf bool, branches []strings.Builder) string {
	if !isIf {
		return branches[0].String()
	}
	switch {
	case jaFirst.MatchString(cond):
		return branches[0].String()
	case enFirst.MatchString(cond):
		if len(branches) > 1 {
			return branches[1].String()
		}
		return ""
	}
	best := ""
	for i := range branches {
		if b := branches[i].String(); len([]rune(visibleText(b))) > len([]rune(visibleText(best))) {
			best = b
		}
	}
	return best
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// visibleText は HTML のタグと空白を除いた文字列を返す。
func visibleText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func hasJapanese(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) {
			return true
		}
	}
	return false
}
