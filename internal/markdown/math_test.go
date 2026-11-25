package markdown

import (
	"strings"
	"testing"
)

func TestRender_InlineMath(t *testing.T) {
	res := mustRender(t, "質量とエネルギーは $E=mc^2$ の関係にあり、$a_1 * b_2 * c_3$ も強調にならない。")
	assertContains(t, res.HTML,
		`<span class="math-inline">\(E=mc^2\)</span>`,
		`<span class="math-inline">\(a_1 * b_2 * c_3\)</span>`)
	if strings.Contains(res.HTML, "<em>") {
		t.Errorf("math content was parsed as emphasis: %s", res.HTML)
	}
	if !HasMath(res.HTML) {
		t.Error("HasMath = false")
	}
}

func TestRender_DisplayMath(t *testing.T) {
	res := mustRender(t, "ガウス積分:\n\n$$\n\\int_0^\\infty e^{-x^2} dx = \\frac{\\sqrt{\\pi}}{2}\n$$\n\n以上。")
	assertContains(t, res.HTML, `<span class="math-display">\[\int_0^\infty e^{-x^2} dx = \frac{\sqrt{\pi}}{2}\]</span>`)

	res = mustRender(t, "1行で $$x^2 + y^2 = r^2$$ と書ける。")
	assertContains(t, res.HTML, `<span class="math-display">\[x^2 + y^2 = r^2\]</span>`)
}

func TestRender_MathIsEscaped(t *testing.T) {
	res := mustRender(t, `$a < b & <script>alert(1)</script>$`)
	assertContains(t, res.HTML, `\(a &lt; b &amp; &lt;script&gt;alert(1)&lt;/script&gt;\)`)
	if strings.Contains(res.HTML, "<script") {
		t.Fatalf("unsanitized script in math: %s", res.HTML)
	}
}

func TestRender_DollarSignsThatAreNotMath(t *testing.T) {
	for _, src := range []string{
		"参加費は $5 と $10 の2種類。", // 閉じの直後が数字
		"$ x$ は数式にしない。",       // 開きの直後が空白
		"$x $ も数式にしない。",       // 閉じの直前が空白
		"閉じない $x",
		"エスケープした \\$x\\$ は文字。",
		"`$code$` はコード。",
		"$$ 閉じないブロック",
	} {
		res := mustRender(t, src)
		if HasMath(res.HTML) {
			t.Errorf("Render(%q) produced math: %s", src, res.HTML)
		}
	}
}

func TestRender_MathClassOnlyOnMathSpans(t *testing.T) {
	// 利用者が書いた生 HTML の class は従来どおり除去する。
	res := mustRender(t, `<span class="math-inline evil">x</span><span class="math-display" onclick="x()">y</span>`)
	if strings.Contains(res.HTML, "evil") || strings.Contains(res.HTML, "onclick") {
		t.Fatalf("unexpected attributes kept: %s", res.HTML)
	}
}
