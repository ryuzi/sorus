package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"sorus/static"
)

// publicCSS は埋め込みの public.css を返す。
func publicCSS(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(static.FS, "css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// cssRuleBody は css の中で、行頭（字下げのあと）が selector で始まる最初の規則の { と } の間を返す。無ければ空と false。
func cssRuleBody(css, selector string) (string, bool) {
	re := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// TestDocTabListHasNoVerticalScrollbar は、タブの並び（.doc-tab-list）に 1px の縦のスクロールバーが出ないことを
// 確かめる（再現: 横スクロールの枠 overflow-x: auto は overflow-y も auto にし、タブの margin-bottom: -1px で中身が
// 1px はみ出すと縦のスクロールバーが出ていた）。
func TestDocTabListHasNoVerticalScrollbar(t *testing.T) {
	css := publicCSS(t)
	list, ok := cssRuleBody(css, ".doc-tab-list")
	if !ok {
		t.Fatal("public.css has no .doc-tab-list rule")
	}
	if !strings.Contains(list, "overflow-x: auto") || !strings.Contains(list, "overflow-y: hidden") {
		t.Errorf(".doc-tab-list must scroll only horizontally (overflow-x: auto; overflow-y: hidden): %s", list)
	}
	// 下の線は枠線ではなく内側の影で描き、選んだタブの下線を重ねる（枠線に重ねるための負の余白を要らなくする）。
	if strings.Contains(list, "border-bottom") || !strings.Contains(list, "box-shadow: inset 0 -1px 0 var(--border)") {
		t.Errorf(".doc-tab-list must draw its bottom line with an inset shadow: %s", list)
	}
	tab, ok := cssRuleBody(css, ".doc-tab")
	if !ok {
		t.Fatal("public.css has no .doc-tab rule")
	}
	if regexp.MustCompile(`margin[^;]*-\d`).MatchString(tab) {
		t.Errorf(".doc-tab must not use a negative margin (it overflows the scroll box by 1px): %s", tab)
	}
}

// TestCodeCopyButtonContrast は、コードのコピーボタンがコードの背景と同じ色に溶けず、明るい・暗い配色のどちらでも
// 背景・枠をコードの文字色と背景色を混ぜて作ることを確かめる。
func TestCodeCopyButtonContrast(t *testing.T) {
	css := publicCSS(t)
	btn, ok := cssRuleBody(css, ".code-copy")
	if !ok {
		t.Fatal("public.css has no .code-copy rule")
	}
	if strings.Contains(btn, "background: var(--code-bg)") {
		t.Errorf(".code-copy must not use the plain code background (it disappears into the code block): %s", btn)
	}
	if !strings.Contains(btn, "box-shadow:") {
		t.Errorf(".code-copy should be lifted from the code with a shadow: %s", btn)
	}
	for _, want := range []string{
		".code-copy { border-color: color-mix(in srgb, var(--code-fg) 42%, var(--code-bg)); background: color-mix(in srgb, var(--code-fg) 14%, var(--code-bg)); }",
		".code-copy:hover { background: color-mix(in srgb, var(--code-fg) 24%, var(--code-bg)); }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("public.css lacks %q", want)
		}
	}
	if lang, _ := cssRuleBody(css, ".code-block-lang"); !strings.Contains(lang, "opacity: .78") {
		t.Errorf(".code-block-lang should be readable (opacity .78): %s", lang)
	}
}

// TestSectionBlockStyles は、汎用のセクション（:::cards・:::cta・:::faq）の見た目が public.css のレイヤー sorus-base
// の中にあり、色・角丸・影をテーマのトークンだけで描き、行動の呼びかけの最初のリンクを主のボタンにすることを確かめる。
func TestSectionBlockStyles(t *testing.T) {
	css := publicCSS(t)
	depths := cssLayerDepths(css)
	for _, sel := range []string{
		".doc-cards", ".doc-card", ".doc-card a", ".doc-cta", ".doc-cta-body", ".doc-cta-actions", ".doc-cta-actions a",
		".doc-faq", ".doc-faq-item + .doc-faq-item", ".doc-faq-q", ".doc-faq-q::after", ".doc-faq-a",
		".doc-sidebar-links ul",
	} {
		body, ok := cssRuleBody(css, sel)
		if !ok {
			t.Errorf("public.css has no rule %q", sel)
			continue
		}
		if i := strings.Index(css, body); depths[i] < 1 {
			t.Errorf("public.css rule %q is outside @layer sorus-base", sel)
		}
		// 色は直書きせずトークンで（影の rgb() は除く）。
		if m := regexp.MustCompile(`(?:^|[\s:])#[0-9a-fA-F]{3,8}\b`).FindString(body); m != "" {
			t.Errorf("rule %q hard-codes a color %q; use the theme tokens", sel, m)
		}
	}
	card, _ := cssRuleBody(css, ".doc-card")
	for _, want := range []string{"var(--border)", "var(--radius-lg)", "var(--surface)", "var(--shadow)"} {
		if !strings.Contains(card, want) {
			t.Errorf(".doc-card lacks %s: %s", want, card)
		}
	}
	cards, _ := cssRuleBody(css, ".doc-cards")
	if !strings.Contains(cards, "grid-template-columns: repeat(auto-fit, minmax(min(100%, 210px), 1fr))") {
		t.Errorf(".doc-cards must be a responsive grid: %s", cards)
	}
	// 行動の呼びかけ: 最初のリンク（段落の中・箇条書きの中のどちらでも）を主のボタンにする。
	if !strings.Contains(css, ".doc-cta-actions > :first-child > a:first-child, .doc-cta-actions > :first-child > li:first-child > a { border-color: var(--accent); background: var(--accent); color: var(--accent-fg);") {
		t.Error("the first .doc-cta-actions link is not styled as the primary button")
	}
	if a, _ := cssRuleBody(css, ".doc-cta-actions a"); !strings.Contains(a, "min-height: 44px") || !strings.Contains(a, "var(--radius)") {
		t.Errorf(".doc-cta-actions a must be a touch-sized button: %s", a)
	}
	if !strings.Contains(css, ".doc-faq-item[open] > .doc-faq-q { color: var(--accent); }") {
		t.Error("an open FAQ question is not highlighted with the accent color")
	}
	// 動きを減らす設定では動かさない。
	if !strings.Contains(css, ".doc-card, .doc-card > p:last-child > a:last-child::after, .doc-cta-actions a, .doc-faq-q, .doc-faq-q::after { transition: none; }") {
		t.Error("section blocks ignore prefers-reduced-motion")
	}
}
