package theme

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sorus/static"
)

// テーマの CSS の約束。テーマは :root でトークンを上書きし、
// 部品の上書きは公開クラスに限る。本体の public.css は全体を @layer sorus-base に入れ、全トークンの既定値を持つ。

// colorTokens は色のトークン。color_scheme に dark を含むテーマは、ダークモードの :root でも定義する。
var colorTokens = []string{
	"--bg", "--surface", "--fg", "--muted", "--border", "--accent", "--accent-fg", "--accent-soft", "--mark",
	"--code-bg", "--code-fg",
}

// layoutTokens は書体と形・幅のトークン。明るいときの :root で定義すればよい。
var layoutTokens = []string{
	"--font-body", "--font-heading", "--font-mono",
	"--radius", "--radius-lg", "--content-width", "--prose-width", "--shadow",
}

// pageWideElements はページ全体に効くため、公開クラスの外で選んではいけない要素。
var pageWideElements = map[string]bool{
	"h1": true, "h2": true, "h3": true, "code": true, "pre": true, "blockquote": true, "body": true, "a": true,
}

// isLikePseudos は引数のセレクタが要素そのものに当たる疑似クラス。引数がすべて公開クラスの内側なら、その要素も内側にある。
var isLikePseudos = map[string]bool{"is": true, "where": true, "matches": true, "-webkit-any": true, "-moz-any": true}

var (
	// externalURL はネットワークから読み込む url()。シングルバイナリでオフラインでも動かすため禁止する。
	externalURL = regexp.MustCompile(`(?i)url\(\s*['"]?\s*(https?:|//)`)
	// literalRadius は public.css がトークンを使わずに書いた角丸。
	literalRadius = regexp.MustCompile(`border-radius:\s*(8|10|12)px`)
	// darkMedia はダークモードの :root を置く @media の前置き。肯定の (prefers-color-scheme: dark) だけを数え、
	// screen・all との and は許す。not・light・ほかの条件との組み合わせ・カンマ区切りは数えない。
	darkMedia = regexp.MustCompile(`^@media\s*((only\s+)?(screen|all)\s+and\s*)?\(\s*prefers-color-scheme\s*:\s*dark\s*\)$`)
	// scopePseudo は @scope の中で scope の根を指す :scope。
	scopePseudo = regexp.MustCompile(`(?i):scope\b`)
)

// cssRule は CSS の1つの規則。
type cssRule struct {
	prelude  string    // セレクタ、または @ 規則の前置き（連続する空白は1つにまとめる）
	body     string    // { } の中身。@import のように本体の無い文では空
	block    bool      // { } を持つか
	children []cssRule // @media・@supports・@layer・@container・@scope・@starting-style の中の規則と、入れ子の規則を持つ規則の中身
}

// checkThemeCSS はテーマの CSS（static/themes/<id>/theme.css）が約束を守っているかを調べ、違反の一覧を返す。
func checkThemeCSS(src, colorScheme string) []string {
	src, err := stripComments(src)
	if err != nil {
		return []string{err.Error()}
	}
	v := resourceViolations(src)
	rules, err := parseCSS(src)
	if err != nil {
		return append(v, err.Error())
	}
	v = append(v, tokenViolations(rules, colorScheme)...)
	v = append(v, selectorViolations(rules, "", false)...)
	// 配色モードは theme.json の color_scheme が <meta name="color-scheme"> で決める。CSS で別の値にしない。
	for _, d := range declarations(rules, false) {
		if d.name == "color-scheme" && schemeKey(d.value) != schemeKey(colorScheme) {
			v = append(v, fmt.Sprintf("color-scheme: %s contradicts theme.json color_scheme %q", d.value, colorScheme))
		}
	}
	return v
}

// checkBaseCSS は本体の public.css が、全体を @layer sorus-base に入れ、全トークンの既定値を持つかを調べる。
func checkBaseCSS(src string) []string {
	src, err := stripComments(src)
	if err != nil {
		return []string{err.Error()}
	}
	v := resourceViolations(src)
	for _, m := range literalRadius.FindAllString(src, -1) {
		v = append(v, fmt.Sprintf("literal %q: use var(--radius) or var(--radius-lg)", m))
	}
	rules, err := parseCSS(src)
	if err != nil {
		return append(v, err.Error())
	}
	if len(rules) != 1 || rules[0].prelude != "@layer sorus-base" || !rules[0].block {
		return append(v, "all rules must be inside a single @layer sorus-base { ... } block")
	}
	inner := rules[0].children
	v = append(v, tokenViolations(inner, "light dark")...)
	// CSS の color-scheme は <meta name="color-scheme"> より優先されるため、本体が書くとテーマの color_scheme が効かない。
	for _, d := range declarations(inner, false) {
		if d.name == "color-scheme" {
			v = append(v, fmt.Sprintf("color-scheme: %s overrides the theme.json color_scheme in <meta name=\"color-scheme\">: remove it", d.value))
		}
	}
	return v
}

// resourceViolations は外部のリソースを読み込む指定（@import・外部 url()）を探す。
func resourceViolations(src string) []string {
	var v []string
	if strings.Contains(strings.ToLower(src), "@import") {
		v = append(v, "@import is not allowed")
	}
	for _, m := range externalURL.FindAllString(src, -1) {
		v = append(v, fmt.Sprintf("external url() is not allowed: %s...)", m))
	}
	return v
}

// tokenViolations は最上位の :root と、color_scheme に dark を含むときはダークモードの :root に、必須トークンがあるかを調べる。
func tokenViolations(rules []cssRule, colorScheme string) []string {
	var v []string
	root := rootDeclarations(rules)
	for _, tok := range append(append([]string{}, colorTokens...), layoutTokens...) {
		if root[tok] == "" {
			v = append(v, "missing "+tok+" in :root")
		}
	}
	if !strings.Contains(colorScheme, "dark") {
		return v
	}
	var dark map[string]string
	for _, r := range rules {
		if isDarkMedia(r.prelude) {
			if dark == nil {
				dark = map[string]string{}
			}
			for k, val := range rootDeclarations(r.children) {
				dark[k] = val
			}
		}
	}
	if dark == nil {
		return append(v, fmt.Sprintf("missing @media (prefers-color-scheme: dark) { :root { ... } } for color_scheme %q", colorScheme))
	}
	for _, tok := range colorTokens {
		if dark[tok] == "" {
			v = append(v, "missing "+tok+" in the dark :root")
		}
	}
	return v
}

// selectorViolations は、ページ全体に効く要素（h1・code など）を公開クラスの外で選ぶセレクタを探す。
// parent は外側の規則のセレクタ一覧（最上位なら空）、inScope は rules が @scope の直下にあるか。
// 入れ子の規則（CSS Nesting）と @scope の中は、外側のセレクタを補った完全なセレクタにしてから調べる。
func selectorViolations(rules []cssRule, parent string, inScope bool) []string {
	var v []string
	for _, r := range rules {
		switch {
		case !r.block:
		case strings.HasPrefix(r.prelude, "@"):
			if r.children == nil {
				continue // @font-face・@keyframes などは要素を選ばない
			}
			if isScopeRule(r.prelude) {
				v = append(v, selectorViolations(r.children, scopeRoots(r.prelude, parent), true)...)
			} else {
				v = append(v, selectorViolations(r.children, parent, inScope)...)
			}
		default:
			prelude := r.prelude
			if inScope {
				prelude = scopePseudo.ReplaceAllString(prelude, "&")
			}
			sels := nestSelectors(prelude, parent)
			for _, sel := range sels {
				if bad, _, _ := checkComplex(sel, false); bad {
					v = append(v, fmt.Sprintf("selector %q styles a page-wide element outside a public class: scope it to a public class such as .prose", sel))
				}
			}
			if r.children != nil {
				v = append(v, selectorViolations(r.children, strings.Join(sels, ", "), false)...)
			}
		}
	}
	return v
}

// nestSelectors は入れ子の規則のセレクタ一覧 prelude を、外側のセレクタ一覧 parent（最上位なら空）を補った完全なセレクタにする。
// & は :is(parent)（最上位では :root）に置き換え、& の無いセレクタは parent の子孫とみなす（CSS Nesting と @scope の決まり）。
func nestSelectors(prelude, parent string) []string {
	outer := ":root"
	if parent != "" {
		outer = ":is(" + parent + ")"
	}
	var out []string
	for _, sel := range selectorList(prelude) {
		switch s, ok := replaceNesting(sel, outer); {
		case ok:
			out = append(out, s)
		case parent != "":
			out = append(out, outer+" "+sel)
		default:
			out = append(out, sel)
		}
	}
	return out
}

// replaceNesting は文字列の外にある & を outer に置き換える。& が無ければ ok は false。
func replaceNesting(sel, outer string) (string, bool) {
	var b strings.Builder
	ok := false
	for i := 0; i < len(sel); i++ {
		switch sel[i] {
		case '"', '\'':
			end, err := skipString(sel, i)
			if err != nil {
				end = len(sel) - 1
			}
			b.WriteString(sel[i : end+1])
			i = end
		case '&':
			b.WriteString(outer)
			ok = true
		default:
			b.WriteByte(sel[i])
		}
	}
	return b.String(), ok
}

func isScopeRule(prelude string) bool {
	p := strings.ToLower(prelude)
	return p == "@scope" || strings.HasPrefix(p, "@scope ") || strings.HasPrefix(p, "@scope(")
}

// scopeRoots は @scope (<根>) to (<境界>) の根のセレクタ一覧を、外側の parent を補って返す。根が無ければ parent のまま。
func scopeRoots(prelude, parent string) string {
	rest := strings.TrimSpace(prelude[len("@scope"):])
	if !strings.HasPrefix(rest, "(") {
		return parent
	}
	end := closeBracket(rest, 0)
	return strings.Join(nestSelectors(rest[1:min(end, len(rest))], parent), ", ")
}

// selectorList はセレクタ一覧を、前後の空白を除いた1つずつのセレクタに分ける。
func selectorList(s string) []string {
	var out []string
	for _, sel := range splitTopLevel(s, ',') {
		if sel = strings.TrimSpace(sel); sel != "" {
			out = append(out, sel)
		}
	}
	return out
}

// checkComplex は1つのセレクタ sel を調べる。scoped は sel の外側にすでに公開クラスの祖先があるか。
// bad はページ全体に効く要素を公開クラスの外で選ぶか。inside は対象の要素に公開クラスの祖先があるか、
// scoping は対象の要素自身が公開クラス（.x）か [data-theme] を持つか、または inside か。
// 公開クラスの祖先として数えるのは、子孫（空白）か子（>）の結合子で後ろにつながる複合セレクタだけ（兄弟の + と ~ は祖先ではない）。
func checkComplex(sel string, scoped bool) (bad, inside, scoping bool) {
	s := scoped   // 今の複合セレクタに公開クラスの祖先があるか
	prev := false // 1つ前の複合セレクタが公開クラスか、その内側か
	for i, c := range splitCompounds(sel) {
		if i > 0 && prev && (c.comb == ' ' || c.comb == '>') {
			s = true
		}
		info := parseCompound(c.text)
		in, sc := s, s || info.scoping
		for _, p := range info.pseudos {
			if !isLikePseudos[p.name] {
				continue
			}
			allIn, allSc := true, true
			for _, a := range selectorList(p.arg) {
				b, ai, as := checkComplex(a, s)
				bad = bad || b
				allIn, allSc = allIn && ai, allSc && as
			}
			in, sc = in || allIn, sc || allSc
		}
		for _, p := range info.pseudos {
			switch p.name {
			case "not":
				for _, a := range selectorList(p.arg) {
					b, _, _ := checkComplex(a, s)
					bad = bad || b
				}
			case "has":
				// :has の引数は、この要素を起点にした相対セレクタ。子孫・子なら、この要素が公開クラスであれば内側にある。
				for _, a := range selectorList(p.arg) {
					sibling := strings.HasPrefix(a, "+") || strings.HasPrefix(a, "~")
					b, _, _ := checkComplex(a, s || (sc && !sibling))
					bad = bad || b
				}
			case "nth-child", "nth-last-child":
				if i := strings.Index(strings.ToLower(p.arg), " of "); i >= 0 {
					for _, a := range selectorList(p.arg[i+len(" of "):]) {
						b, _, _ := checkComplex(a, s)
						bad = bad || b
					}
				}
			}
		}
		if pageWideElements[info.elem] && !in {
			bad = true
		}
		prev, inside, scoping = sc, in, sc
	}
	return bad, inside, scoping
}

// compound はセレクタの中の1つの複合セレクタ（h1.title:hover のように結合子を含まない部分）。
type compound struct {
	comb byte // 前の複合セレクタとの結合子（' '・'>'・'+'・'~'）。先頭は 0、相対セレクタの先頭はその結合子
	text string
}

// splitCompounds はセレクタを、括弧と文字列の外にある結合子で複合セレクタに分ける。
func splitCompounds(sel string) []compound {
	var out []compound
	var comb byte
	start := -1
	flush := func(end int) {
		if start >= 0 {
			out = append(out, compound{comb: comb, text: sel[start:end]})
			start, comb = -1, 0
		}
	}
	for i := 0; i < len(sel); i++ {
		switch c := sel[i]; c {
		case ' ', '\t', '\n', '\r', '\f':
			flush(i)
			if comb == 0 && len(out) > 0 {
				comb = ' '
			}
		case '>', '+', '~':
			flush(i)
			comb = c
		case '(', '[', '"', '\'':
			if start < 0 {
				start = i
			}
			if c == '"' || c == '\'' {
				end, err := skipString(sel, i)
				if err != nil {
					end = len(sel)
				}
				i = end
			} else {
				i = closeBracket(sel, i)
			}
		case '\\':
			if start < 0 {
				start = i
			}
			i++
		default:
			if start < 0 {
				start = i
			}
		}
	}
	flush(len(sel))
	return out
}

// compoundInfo は複合セレクタを読み取った結果。
type compoundInfo struct {
	elem    string // 型セレクタ（小文字、名前空間を除く）。無ければ空
	scoping bool   // 公開クラス（.x）か [data-theme] を直接持つか
	pseudos []pseudoClass
}

// pseudoClass は疑似クラス・疑似要素の名前（小文字）と、括弧の中身。
type pseudoClass struct{ name, arg string }

func parseCompound(s string) compoundInfo {
	var c compoundInfo
	end := typeEnd(s, 0)
	name := s[:end]
	if end < len(s) && s[end] == '|' { // ns|h1・*|h1・|h1
		start := end + 1
		end = typeEnd(s, start)
		name = s[start:end]
	}
	c.elem = strings.ToLower(name)
	for i := end; i < len(s); {
		switch s[i] {
		case '.':
			if e := identEnd(s, i+1); e > i+1 {
				c.scoping = true
				i = e
			} else {
				i++
			}
		case '[':
			e := closeBracket(s, i)
			attr := strings.ToLower(strings.TrimSpace(s[i+1 : min(e, len(s))]))
			if rest, ok := strings.CutPrefix(attr, "data-theme"); ok && identEnd(rest, 0) == 0 {
				c.scoping = true
			}
			i = e + 1
		case ':':
			j := i + 1
			if j < len(s) && s[j] == ':' {
				j++
			}
			e := identEnd(s, j)
			p := pseudoClass{name: strings.ToLower(s[j:e])}
			i = e
			if e < len(s) && s[e] == '(' {
				ce := closeBracket(s, e)
				p.arg = s[e+1 : min(ce, len(s))]
				i = ce + 1
			}
			c.pseudos = append(c.pseudos, p)
		default:
			i++
		}
	}
	return c
}

// typeEnd は s[i] から始まる型セレクタ（名前か *）の終わりの位置を返す。
func typeEnd(s string, i int) int {
	if i < len(s) && s[i] == '*' {
		return i + 1
	}
	return identEnd(s, i)
}

// identEnd は s[i] から始まる識別子の終わりの位置を返す。識別子が無ければ i。
func identEnd(s string, i int) int {
	for i < len(s) {
		switch c := s[i]; {
		case c == '\\':
			i += 2
		case c == '-' || c == '_' || c >= 0x80 || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9'):
			i++
		default:
			return i
		}
	}
	return len(s)
}

// closeBracket は s[open] の ( か [ に対応する閉じ括弧の位置を返す。閉じていなければ len(s)。
func closeBracket(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			end, err := skipString(s, i)
			if err != nil {
				return len(s)
			}
			i = end
		case '(', '[':
			depth++
		case ')', ']':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(s)
}

// declaration は1つの宣言。名前はカスタムプロパティ（--x）以外を小文字にする。
type declaration struct{ name, value string }

func parseDeclaration(d string) (declaration, bool) {
	name, val, ok := strings.Cut(d, ":")
	if !ok {
		return declaration{}, false
	}
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "--") {
		name = strings.ToLower(name)
	}
	return declaration{name: name, value: strings.TrimSpace(val)}, true
}

// declarations は rules の中のすべての宣言を、@media などや入れ子の規則の中も含めて返す。inStyle は rules が規則の本体の中にあるか。
func declarations(rules []cssRule, inStyle bool) []declaration {
	var out []declaration
	for _, r := range rules {
		var parts []string
		switch {
		case !r.block:
			if inStyle && !strings.HasPrefix(r.prelude, "@") {
				parts = []string{r.prelude}
			}
		case r.children != nil:
			out = append(out, declarations(r.children, inStyle || !strings.HasPrefix(r.prelude, "@"))...)
		case !strings.HasPrefix(r.prelude, "@"):
			parts = splitTopLevel(r.body, ';')
		}
		for _, p := range parts {
			if d, ok := parseDeclaration(p); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

// schemeKey は color-scheme の値を比べられる形（only と !important を除き、語を並べ替えたもの）にする。
func schemeKey(v string) string {
	var words []string
	for _, w := range strings.Fields(strings.ReplaceAll(strings.ToLower(v), "!important", " ")) {
		if w != "only" {
			words = append(words, w)
		}
	}
	slices.Sort(words)
	return strings.Join(words, " ")
}

// rootDeclarations は rules の中で、セレクタがちょうど :root の規則の宣言をまとめて返す（後のものが勝つ）。
func rootDeclarations(rules []cssRule) map[string]string {
	out := map[string]string{}
	for _, r := range rules {
		if !r.block || r.prelude != ":root" {
			continue
		}
		for _, p := range splitTopLevel(r.body, ';') {
			if d, ok := parseDeclaration(p); ok {
				out[d.name] = d.value
			}
		}
	}
	return out
}

func isDarkMedia(prelude string) bool {
	return darkMedia.MatchString(strings.ToLower(prelude))
}

// stripComments は /* */ のコメントを取り除く。文字列の中の /* はコメントとして扱わない。
func stripComments(src string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '"' || src[i] == '\'':
			end, err := skipString(src, i)
			if err != nil {
				return "", err
			}
			b.WriteString(src[i : end+1])
			i = end
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return "", errors.New("unterminated comment")
			}
			b.WriteByte(' ')
			i += end + 3
		default:
			b.WriteByte(src[i])
		}
	}
	return b.String(), nil
}

// parseCSS はコメントを除いた CSS を規則に分ける。括弧の対応と文字列だけを見る、約束の検査に足りるだけの読み取り。
func parseCSS(src string) ([]cssRule, error) { return parseRules(src, false) }

// parseRules は parseCSS の本体。inStyle は src が規則の本体の中か。本体の中では、最後の宣言の ; を省いてよい。
func parseRules(src string, inStyle bool) ([]cssRule, error) {
	var rules []cssRule
	start := 0
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '"', '\'':
			end, err := skipString(src, i)
			if err != nil {
				return nil, err
			}
			i = end
		case ';':
			if p := normalizeSpace(src[start:i]); p != "" {
				rules = append(rules, cssRule{prelude: p})
			}
			start = i + 1
		case '{':
			end, err := matchBrace(src, i)
			if err != nil {
				return nil, err
			}
			r := cssRule{prelude: normalizeSpace(src[start:i]), body: src[i+1 : end], block: true}
			nested, bodyStyle := hasNestedRules(r.prelude), inStyle
			if !strings.HasPrefix(r.prelude, "@") && hasBlock(r.body) {
				nested, bodyStyle = true, true // 入れ子の規則（CSS Nesting）を持つ規則
			}
			if nested {
				if r.children, err = parseRules(r.body, bodyStyle); err != nil {
					return nil, err
				}
				if r.children == nil {
					r.children = []cssRule{}
				}
			}
			rules = append(rules, r)
			i, start = end, end+1
		case '}':
			return nil, errors.New("unbalanced }")
		}
	}
	if rest := normalizeSpace(src[start:]); rest != "" {
		if !inStyle {
			return nil, fmt.Errorf("unterminated rule %q", rest)
		}
		rules = append(rules, cssRule{prelude: rest})
	}
	return rules, nil
}

// hasBlock は文字列の外に { があるか。
func hasBlock(src string) bool {
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '"', '\'':
			end, err := skipString(src, i)
			if err != nil {
				return false
			}
			i = end
		case '{':
			return true
		}
	}
	return false
}

func hasNestedRules(prelude string) bool {
	p := strings.ToLower(prelude)
	for _, at := range []string{"@media", "@supports", "@layer", "@container", "@scope", "@starting-style"} {
		if p == at || strings.HasPrefix(p, at+" ") || strings.HasPrefix(p, at+"(") {
			return true
		}
	}
	return false
}

// matchBrace は src[open] の { に対応する } の位置を返す。
func matchBrace(src string, open int) (int, error) {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '"', '\'':
			end, err := skipString(src, i)
			if err != nil {
				return 0, err
			}
			i = end
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("unbalanced {")
}

// skipString は src[i] の引用符で始まる文字列の、閉じる引用符の位置を返す。
func skipString(src string, i int) (int, error) {
	q := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case q:
			return j, nil
		}
	}
	return 0, errors.New("unterminated string")
}

// splitTopLevel は括弧と文字列の外にある sep で s を分ける。
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\'':
			if end, err := skipString(s, i); err == nil {
				i = end
			}
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == sep && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestBuiltinThemesHonorContract(t *testing.T) {
	for _, th := range Builtin.All() {
		data, err := fs.ReadFile(static.FS, th.Stylesheet())
		if err != nil {
			t.Fatalf("theme %s: %v", th.ID, err)
		}
		for _, v := range checkThemeCSS(string(data), th.ColorScheme) {
			t.Errorf("theme %s (%s): %s", th.ID, th.Stylesheet(), v)
		}
	}
}

func TestBaseStylesheetDefinesTokens(t *testing.T) {
	data, err := fs.ReadFile(static.FS, "css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range checkBaseCSS(string(data)) {
		t.Errorf("css/public.css: %s", v)
	}
}

// readContract は testdata/contract の CSS を読む。
func readContract(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "contract", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestContractCheckerRejectsBrokenThemes(t *testing.T) {
	if v := checkThemeCSS(readContract(t, "valid.css"), "light dark"); len(v) != 0 {
		t.Fatalf("valid.css has violations: %v", v)
	}
	if v := checkThemeCSS(readContract(t, "light_only.css"), "light"); len(v) != 0 {
		t.Fatalf("light_only.css has violations for a light theme: %v", v)
	}
	// 公開クラス・[data-theme] の内側で要素を選ぶ形（入れ子・@scope・:is() などを含む）、screen と組み合わせたダークモード、
	// theme.json と同じ color-scheme は許す。
	for _, tc := range []struct{ file, colorScheme string }{
		{"scoped_selectors.css", "light dark"},
		{"dark_media_screen.css", "light dark"},
		{"color_scheme_match.css", "light"},
	} {
		if v := checkThemeCSS(readContract(t, tc.file), tc.colorScheme); len(v) != 0 {
			t.Errorf("%s has violations for color_scheme %q: %v", tc.file, tc.colorScheme, v)
		}
	}
	for _, tc := range []struct {
		file, colorScheme, want string
	}{
		{"missing_accent_fg.css", "light dark", "missing --accent-fg in :root"},
		{"missing_dark_token.css", "light dark", "missing --code-bg in the dark :root"},
		{"light_only.css", "light dark", "missing @media (prefers-color-scheme: dark)"},
		{"light_only.css", "dark", "missing @media (prefers-color-scheme: dark)"},
		{"external_url.css", "light dark", "external url("},
		{"protocol_relative_url.css", "light dark", "external url("},
		{"import.css", "light dark", "@import is not allowed"},
		{"bare_element.css", "light dark", `selector "h1"`},
		{"bare_element.css", "light dark", `selector "pre code"`},
		{"bare_element.css", "light dark", `selector "a:hover"`},
		{"unbalanced.css", "light dark", "unbalanced {"},
		{"dark_media_negated.css", "light dark", "missing @media (prefers-color-scheme: dark)"},
		{"dark_media_light.css", "light dark", "missing @media (prefers-color-scheme: dark)"},
		{"color_scheme_contradiction.css", "light", `color-scheme: light dark contradicts theme.json color_scheme "light"`},
		{"color_scheme_match.css", "dark", `color-scheme: only light contradicts theme.json color_scheme "dark"`},
	} {
		t.Run(tc.file+"/"+tc.colorScheme, func(t *testing.T) {
			v := checkThemeCSS(readContract(t, tc.file), tc.colorScheme)
			if !containsViolation(v, tc.want) {
				t.Errorf("violations %q do not include %q", v, tc.want)
			}
		})
	}
	// 要素から始まらない書き方・入れ子の規則・@scope でも、公開クラスの外で要素を選べば違反にする。
	bypass := checkThemeCSS(readContract(t, "bare_element_bypass.css"), "light dark")
	for _, sel := range []string{
		"html h1", ":root h2", ":is(h1)", ":where(code)", ".card:not(pre)", ":not(.prose) blockquote",
		".card ~ h3", ".card:has(+ pre)", ".card + :nth-child(1 of a)", ":is(.prose, body)",
		":is(:root) h3", ":is(:root) pre", ":is(:root) blockquote", "a", ":is(.prose) + code",
	} {
		if !containsViolation(bypass, `selector "`+sel+`" `) {
			t.Errorf("bare_element_bypass.css: selector %q was not reported: %q", sel, bypass)
		}
	}
	if containsViolation(bypass, `selector ".card" `) {
		t.Errorf("bare_element_bypass.css: scoped selector .card was reported: %q", bypass)
	}
	// 公開クラスに絞った部品の上書きと、相対 url() は許す。
	v := checkThemeCSS(readContract(t, "bare_element.css"), "light dark")
	for _, ok := range []string{".prose h2", ".article-title", "abbr", "article"} {
		if containsViolation(v, `selector "`+ok) {
			t.Errorf("scoped selector %q was reported: %v", ok, v)
		}
	}
}

func TestBaseCheckerRejectsUnlayeredRules(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"base_unlayered.css", "@layer sorus-base"},
		{"base_literal_radius.css", `literal "border-radius: 12px"`},
		{"base_literal_radius.css", "missing --shadow in :root"},
		{"base_color_scheme.css", "color-scheme: light dark overrides the theme.json color_scheme"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			if v := checkBaseCSS(readContract(t, tc.file)); !containsViolation(v, tc.want) {
				t.Errorf("violations %q do not include %q", v, tc.want)
			}
		})
	}
}

func containsViolation(v []string, want string) bool {
	for _, s := range v {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
