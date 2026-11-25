package markdown

import (
	"reflect"
	"strings"
	"testing"
)

// GitHub の5種類の印（大文字・小文字は問わない）を、種類のクラスと既定の見出しを付けた注意書きにする。
func TestRender_CalloutTypes(t *testing.T) {
	for _, tc := range []struct{ marker, variant, title string }{
		{"[!NOTE]", "note", "メモ"},
		{"[!TIP]", "tip", "ヒント"},
		{"[!IMPORTANT]", "important", "重要"},
		{"[!WARNING]", "warning", "警告"},
		{"[!CAUTION]", "caution", "注意"},
		{"[!note]", "note", "メモ"},
		{"[!Caution]", "caution", "注意"},
	} {
		res := mustRender(t, "> "+tc.marker+"\n> 本文の **強調**。\n\n後ろの段落")
		want := `<div class="callout callout-` + tc.variant + `" role="note">` + "\n" +
			`<p class="callout-title">` + tc.title + "</p>\n" +
			`<div class="callout-body">` + "\n" +
			"<p>本文の <strong>強調</strong>。</p>\n" +
			"</div>\n</div>\n" +
			"<p>後ろの段落</p>\n"
		if res.HTML != want {
			t.Errorf("%s:\ngot\n%s\nwant\n%s", tc.marker, res.HTML, want)
		}
	}
}

// 本文は引用と同じく Markdown として解釈し、見出しには目次の ID を付ける。リスト・コード・遅延継続行も引用と同じ。
func TestRender_CalloutBodyIsMarkdown(t *testing.T) {
	res := mustRender(t, "> [!TIP]\n> ## 手順\n> - 一つ目\n> - 二つ目\n>\n> ```\n> <b>code</b>\n> ```\n> 続き\n遅延継続行")
	assertContains(t, res.HTML,
		`<div class="callout callout-tip" role="note">`,
		`<h2 id="手順">手順</h2>`,
		"<li>一つ目</li>",
		"<pre><code>&lt;b&gt;code&lt;/b&gt;\n</code></pre>",
		"<p>続き\n遅延継続行</p>\n</div>\n</div>")
	if want := []Heading{{Level: 2, Text: "手順", ID: "手順"}}; !reflect.DeepEqual(res.Headings, want) {
		t.Errorf("Headings = %#v, want %#v", res.Headings, want)
	}
}

// 印の後ろの文字は見出しになる。Markdown・HTML として解釈せず、文字としてエスケープする。
func TestRender_CalloutCustomTitle(t *testing.T) {
	res := mustRender(t, "> [!WARNING] 消す前に <script>alert(1)</script> &amp; *確認*\n> 元に戻せません。")
	assertContains(t, res.HTML,
		`<div class="callout callout-warning" role="note">`,
		`<p class="callout-title">消す前に &lt;script&gt;alert(1)&lt;/script&gt; &amp; *確認*</p>`)
	if strings.Contains(res.HTML, "警告") {
		t.Errorf("default title is used with a custom title:\n%s", res.HTML)
	}
	assertSafeHTML(t, "custom title", res.HTML)
}

// 本文の生 HTML は、注意書きの中でも今までどおりサニタイズする。
func TestRender_CalloutBodyIsSanitized(t *testing.T) {
	for _, src := range []string{
		"> [!NOTE]\n> <script>alert(1)</script>",
		"> [!NOTE]\n> <img src=x onerror=alert(1)>",
		"> [!CAUTION]\n> [クリック](javascript:alert(1))",
		"> [!IMPORTANT]\n> <div onclick=\"alert(1)\" style=\"color:red\" role=\"alert\">x</div>",
		"> [!TIP]\n> <iframe src=\"https://evil.example/\"></iframe>",
	} {
		res := mustRender(t, src)
		if !strings.Contains(res.HTML, `<div class="callout-body">`) {
			t.Errorf("%q: not rendered as a callout:\n%s", src, res.HTML)
		}
		if strings.Contains(res.HTML, `role="alert"`) {
			t.Errorf("%q: role other than note is kept:\n%s", src, res.HTML)
		}
		assertSafeHTML(t, src, res.HTML)
	}
}

// 印の行の形が違うもの・引用の途中の印・コードの中の印は、今までどおりの引用・コードのまま。
func TestRender_CalloutRequiresMarkerLine(t *testing.T) {
	for _, src := range []string{
		"> [!NOTE]本文",        // 印の直後に空白が無い
		"> [!UNKNOWN]\n> 本文", // 知らない種類
		"> [!NOTE\n> 本文",     // 閉じの ] が無い
		"> 本文\n> [!NOTE]",    // 1行目でない
		"> [NOTE]\n> 本文",     // ! が無い
		"```\n> [!NOTE]\n```",
		"    > [!NOTE]", // インデントしたコード
		"[!NOTE] 本文",
	} {
		res := mustRender(t, src)
		if strings.Contains(res.HTML, "callout") {
			t.Errorf("%q: rendered as a callout:\n%s", src, res.HTML)
		}
	}
	res := mustRender(t, "> 引用\n> [!NOTE]")
	assertContains(t, res.HTML, "<blockquote>\n<p>引用\n[!NOTE]</p>\n</blockquote>")
}

// 印だけの注意書き、リスト・引用の中の注意書き、続けて書いた2つの注意書きも描画できる。
func TestRender_CalloutNesting(t *testing.T) {
	res := mustRender(t, "> [!NOTE]")
	if want := `<div class="callout callout-note" role="note">` + "\n" + `<p class="callout-title">メモ</p>` + "\n" +
		`<div class="callout-body">` + "\n</div>\n</div>\n"; res.HTML != want {
		t.Errorf("marker only:\ngot\n%s\nwant\n%s", res.HTML, want)
	}

	res = mustRender(t, "- 項目\n\n  > [!TIP]\n  > リストの中")
	assertContains(t, res.HTML, "<li>\n<p>項目</p>\n"+`<div class="callout callout-tip" role="note">`, "<p>リストの中</p>")

	res = mustRender(t, "> 外の引用\n>\n> > [!WARNING]\n> > 中の注意書き")
	assertContains(t, res.HTML, "<blockquote>\n<p>外の引用</p>\n"+`<div class="callout callout-warning" role="note">`, "<p>中の注意書き</p>\n</div>\n</div>\n</blockquote>")

	res = mustRender(t, "> [!NOTE]\n> 一つ目\n\n> [!CAUTION]\n> 二つ目")
	if got := strings.Count(res.HTML, `role="note"`); got != 2 {
		t.Errorf("callouts = %d, want 2:\n%s", got, res.HTML)
	}
}

// 生 HTML の callout クラスは予約しない（WordPress などから取り込んだ本文の class="callout" を落とさない）。
// role は note だけを残す。
func TestRender_CalloutRawHTMLClasses(t *testing.T) {
	res := mustRender(t, `<div class="callout">a</div><div class="callout callout-note" role="note">b</div><p class="callout-title">c</p><div class="callout-body extra">d</div><div role="alert">e</div>`)
	want := []string{"div.callout", "div.callout callout-note", "p.callout-title", "div.callout-body extra"}
	if got := classAttrs(t, res.HTML); !reflect.DeepEqual(got, want) {
		t.Errorf("classes = %q, want %q\n%s", got, want, res.HTML)
	}
	assertContains(t, res.HTML, `role="note"`)
	if strings.Contains(res.HTML, `role="alert"`) {
		t.Errorf("role=alert is kept:\n%s", res.HTML)
	}
}
