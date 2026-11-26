package markdown

import (
	"reflect"
	"strings"
	"testing"
)

// :::card と ::: で囲んだ範囲を1枚ずつのカードにし、全体を doc-cards で囲む。カードの間の空行は無視する。
func TestRender_CardsExplicit(t *testing.T) {
	res := mustRender(t, "前\n\n:::cards\n:::card\n### はじめる\nインストールから。\n[手順を見る](/docs/start)\n:::\n\n:::card\n### テーマ\n見た目を変える。\n:::\n:::\n\n後ろ")
	want := "<p>前</p>\n" +
		`<div class="doc-cards">` + "\n" +
		`<div class="doc-card">` + "\n" +
		`<h3 id="はじめる">はじめる</h3>` + "\n" +
		"<p>インストールから。\n" + `<a href="/docs/start" rel="nofollow">手順を見る</a></p>` + "\n" +
		"</div>\n" +
		`<div class="doc-card">` + "\n" +
		`<h3 id="テーマ">テーマ</h3>` + "\n" +
		"<p>見た目を変える。</p>\n" +
		"</div>\n" +
		"</div>\n" +
		"<p>後ろ</p>\n"
	if res.HTML != want {
		t.Errorf("got\n%s\nwant\n%s", res.HTML, want)
	}
}

// --- の行でも区切れる。直前の段落を Setext 見出しにせず、--- の行は出力しない。
func TestRender_CardsSeparator(t *testing.T) {
	res := mustRender(t, ":::cards\n### A\n説明A\n---\n### B\n説明B\n[開く](/b)\n---\n- 項目\n:::\n後ろ")
	want := `<div class="doc-cards">` + "\n" +
		`<div class="doc-card">` + "\n" + `<h3 id="a">A</h3>` + "\n" + "<p>説明A</p>\n" + "</div>\n" +
		`<div class="doc-card">` + "\n" + `<h3 id="b">B</h3>` + "\n" + "<p>説明B\n" + `<a href="/b" rel="nofollow">開く</a></p>` + "\n" + "</div>\n" +
		`<div class="doc-card">` + "\n" + "<ul>\n<li>項目</li>\n</ul>\n" + "</div>\n" +
		"</div>\n" +
		"<p>後ろ</p>\n"
	if res.HTML != want {
		t.Errorf("got\n%s\nwant\n%s", res.HTML, want)
	}
	if strings.Contains(res.HTML, "<h2") || strings.Contains(res.HTML, "<hr") {
		t.Errorf("separator became a heading or a rule:\n%s", res.HTML)
	}
}

// 閉じずに次の :::card を書いたら前のカードを閉じる。:::card の閉じの後ろに書いた中身もカードに包み、空のカードは出さない。
func TestRender_CardsLooseContent(t *testing.T) {
	res := mustRender(t, ":::cards\n:::card\nA\n:::card\nB\n:::\nC\n:::")
	want := `<div class="doc-cards">` + "\n" +
		`<div class="doc-card">` + "\n<p>A</p>\n</div>\n" +
		`<div class="doc-card">` + "\n<p>B</p>\n</div>\n" +
		`<div class="doc-card">` + "\n<p>C</p>\n</div>\n" +
		"</div>\n"
	if res.HTML != want {
		t.Errorf("got\n%s\nwant\n%s", res.HTML, want)
	}
}

// カードの中のコードの ::: と ---、フォトギャラリー・タブ・注意書きの ::: では、カードもカード全体も閉じない。
func TestRender_CardsNestedBlocks(t *testing.T) {
	src := ":::cards\n:::card\n```\n:::\n---\n```\n:::gallery\n![a](/a.webp)\n:::\n:::tabs\n:::tab[X]\nx\n:::\n> [!NOTE]\n> メモ\n:::\n:::card\n最後\n:::\n:::\n\n後ろ"
	res := mustRender(t, src)
	assertContains(t, res.HTML,
		"<pre><code>:::\n---\n</code></pre>",
		`<div class="gallery-item" tabindex="0"`,
		`<div class="doc-tab-panel" role="tabpanel"`,
		`<div class="callout callout-note" role="note">`,
		`<div class="doc-card">`+"\n<p>最後</p>\n</div>\n</div>\n<p>後ろ</p>")
	if n := strings.Count(res.HTML, `<div class="doc-card">`); n != 2 {
		t.Errorf("cards = %d, want 2:\n%s", n, res.HTML)
	}
	if strings.Contains(res.HTML, "<p>:::") {
		t.Errorf("a ::: line leaked as text:\n%s", res.HTML)
	}
}

// 行動の呼びかけは、最後のリンクだけの段落をボタンの欄に、残りを本文の欄にする。説明のすぐ下の行のリンクも分ける。
func TestRender_CTA(t *testing.T) {
	want := `<div class="doc-cta">` + "\n" +
		`<div class="doc-cta-body">` + "\n" +
		`<h2 id="今すぐ始めましょう">今すぐ始めましょう</h2>` + "\n" +
		"<p>1行でインストールできます。</p>\n" +
		"</div>\n" +
		`<div class="doc-cta-actions">` + "\n" +
		`<p><a href="/download" rel="nofollow">ダウンロード</a> <a href="/docs" rel="nofollow">ドキュメント</a></p>` + "\n" +
		"</div>\n" +
		"</div>\n"
	for _, src := range []string{
		":::cta\n## 今すぐ始めましょう\n1行でインストールできます。\n\n[ダウンロード](/download) [ドキュメント](/docs)\n:::",
		":::cta\n## 今すぐ始めましょう\n1行でインストールできます。\n[ダウンロード](/download) [ドキュメント](/docs)\n:::",
	} {
		if res := mustRender(t, src); res.HTML != want {
			t.Errorf("%q:\ngot\n%s\nwant\n%s", src, res.HTML, want)
		}
	}
}

// リンクだけの箇条書きもボタンの欄にする。リンクが無い・文の途中のリンクだけなら、ボタンの欄は出さない。
func TestRender_CTAActionsDetection(t *testing.T) {
	res := mustRender(t, ":::cta\n説明\n\n- [A](/a)\n- [B](/b)\n:::")
	assertContains(t, res.HTML,
		`<div class="doc-cta-body">`+"\n<p>説明</p>\n</div>",
		`<div class="doc-cta-actions">`+"\n<ul>\n"+`<li><a href="/a" rel="nofollow">A</a></li>`)

	for _, src := range []string{
		":::cta\n## 見出しだけ\n説明\n:::",
		":::cta\n説明の [途中のリンク](/x) です。\n:::",
		":::cta\n[前のリンク](/x)\n\n後ろの説明\n:::",
	} {
		res := mustRender(t, src)
		if strings.Contains(res.HTML, CTAActionsClass) {
			t.Errorf("%q: actions without trailing links:\n%s", src, res.HTML)
		}
		if !strings.Contains(res.HTML, `<div class="doc-cta">`+"\n"+`<div class="doc-cta-body">`) {
			t.Errorf("%q: not rendered as a cta:\n%s", src, res.HTML)
		}
	}

	res = mustRender(t, ":::cta\n[A](/a)\n:::")
	if strings.Contains(res.HTML, CTABodyClass) || !strings.Contains(res.HTML, CTAActionsClass) {
		t.Errorf("links only: want only the actions:\n%s", res.HTML)
	}
}

// よくある質問は、:::q[質問] と ### 質問 の行ごとに details にする。質問は文字のまま出し、目次に載せない。
func TestRender_FAQ(t *testing.T) {
	res := mustRender(t, ":::faq\n:::q[無料で使えますか]\nはい。**無料**です。\n\n### 予約投稿はできますか ##\nできます。\n#### 補足\n- 日時を選ぶ\n:::\n\n後ろ")
	want := `<div class="doc-faq">` + "\n" +
		`<details class="doc-faq-item">` + "\n" +
		`<summary class="doc-faq-q">無料で使えますか</summary>` + "\n" +
		`<div class="doc-faq-a">` + "\n" +
		"<p>はい。<strong>無料</strong>です。</p>\n" +
		"</div>\n</details>\n" +
		`<details class="doc-faq-item">` + "\n" +
		`<summary class="doc-faq-q">予約投稿はできますか</summary>` + "\n" +
		`<div class="doc-faq-a">` + "\n" +
		"<p>できます。</p>\n" +
		`<h4 id="補足">補足</h4>` + "\n" +
		"<ul>\n<li>日時を選ぶ</li>\n</ul>\n" +
		"</div>\n</details>\n" +
		"</div>\n" +
		"<p>後ろ</p>\n"
	if res.HTML != want {
		t.Errorf("got\n%s\nwant\n%s", res.HTML, want)
	}
	if want := []Heading{{Level: 4, Text: "補足", ID: "補足"}}; !reflect.DeepEqual(res.Headings, want) {
		t.Errorf("Headings = %#v, want %#v", res.Headings, want)
	}
}

// 質問は Markdown・HTML として解釈せず、文字としてエスケープする。
func TestRender_FAQQuestionIsEscaped(t *testing.T) {
	res := mustRender(t, ":::faq\n:::q[<script>alert(1)</script> &amp; *強調*]\n答え\n### <img src=x onerror=alert(1)>\n答え\n:::")
	assertContains(t, res.HTML,
		`<summary class="doc-faq-q">&lt;script&gt;alert(1)&lt;/script&gt; &amp; *強調*</summary>`,
		`<summary class="doc-faq-q">&lt;img src=x onerror=alert(1)&gt;</summary>`)
	assertSafeHTML(t, "faq question", res.HTML)
}

// 閉じの ::: が無いもの、:::faq の次の行が質問でないもの、外に書いた :::card・:::q[...] は、今までどおりの段落・見出し。
func TestRender_SectionsRequireStructure(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{":::cards\n### A\n本文", "<p>:::cards</p>\n" + `<h3 id="a">A</h3>` + "\n<p>本文</p>\n"},
		{":::cta\n本文", "<p>:::cta\n本文</p>\n"},
		{":::faq\n:::q[質問]\n答え", "<p>:::faq\n:::q[質問]\n答え</p>\n"},
		{":::faq\n説明\n:::q[質問]\n答え\n:::", "<p>:::faq\n説明\n:::q[質問]\n答え\n:::</p>\n"},
		{":::card\n本文\n:::", "<p>:::card\n本文\n:::</p>\n"},
		{":::q[質問]\n### 見出し", "<p>:::q[質問]</p>\n" + `<h3 id="見出し">見出し</h3>` + "\n"},
		{"```\n:::cards\n:::card\nx\n:::\n:::\n```", "<pre><code>:::cards\n:::card\nx\n:::\n:::\n</code></pre>\n"},
	} {
		if res := mustRender(t, tc.src); res.HTML != tc.want {
			t.Errorf("%q:\ngot\n%s\nwant\n%s", tc.src, res.HTML, tc.want)
		}
	}
}

// 引用の中にも書け、セクションどうしを入れ子にできる。
func TestRender_SectionsNested(t *testing.T) {
	res := mustRender(t, "> :::cards\n> ### A\n> ---\n> B\n> :::")
	assertContains(t, res.HTML,
		"<blockquote>\n"+`<div class="doc-cards">`+"\n"+`<div class="doc-card">`+"\n"+`<h3 id="a">A</h3>`+"\n</div>\n"+
			`<div class="doc-card">`+"\n<p>B</p>\n</div>\n</div>\n</blockquote>")

	res = mustRender(t, ":::cards\n:::card\n:::faq\n:::q[Q]\n:::cta\n[A](/a)\n:::\n:::\n:::\n:::\n\n後ろ")
	assertContains(t, res.HTML,
		`<div class="doc-card">`+"\n"+`<div class="doc-faq">`+"\n"+`<details class="doc-faq-item">`,
		`<div class="doc-faq-a">`+"\n"+`<div class="doc-cta">`+"\n"+`<div class="doc-cta-actions">`,
		"</div>\n</div>\n</div>\n</details>\n</div>\n</div>\n</div>\n<p>後ろ</p>")
}

// 中身の生 HTML・危険な URL は、セクションの中でも今までどおりサニタイズする。
func TestRender_SectionsAreSanitized(t *testing.T) {
	for _, src := range []string{
		":::cards\n:::card\n<script>alert(1)</script>\n<div onclick=\"alert(1)\" style=\"color:red\">x</div>\n:::\n:::",
		":::cards\n<img src=x onerror=alert(1)>\n---\n[x](javascript:alert(1))\n:::",
		":::cta\n<iframe src=\"https://evil.example/\"></iframe>\n\n[押す](javascript:alert(1)) [押す](vbscript:x)\n:::",
		":::cta\n- [押す](javascript:alert(1))\n- <a href=\"javascript:alert(1)\" onclick=\"x\">a</a>\n:::",
		":::faq\n:::q[Q]\n<details open ontoggle=\"alert(1)\"><summary onclick=\"x\">s</summary></details>\n<form><button>b</button></form>\n:::",
	} {
		res := mustRender(t, src)
		if !strings.Contains(res.HTML, `<div class="doc-`) {
			t.Errorf("%q: not rendered as a section:\n%s", src, res.HTML)
		}
		if strings.Contains(res.HTML, "javascript:") || strings.Contains(res.HTML, "vbscript:") {
			t.Errorf("%q: dangerous URL remains:\n%s", src, res.HTML)
		}
		assertSafeHTML(t, src, res.HTML)
	}
}

// 生 HTML でも同じクラスを使える（スクリプトを使わないため予約しない）。details・summary には決まったクラスだけを残す。
func TestRender_SectionClassesInRawHTML(t *testing.T) {
	res := mustRender(t, "<div class=\"doc-cards\" onclick=\"x\"><div class=\"doc-card\" style=\"color:red\">a</div></div>\n\n"+
		"<details class=\"doc-faq-item\" onclick=\"x\"><summary class=\"doc-faq-q\">q</summary><div class=\"doc-faq-a\">a</div></details>\n\n"+
		"<details class=\"evil\"><summary class=\"doc-faq-q extra\">q</summary></details>")
	assertContains(t, res.HTML,
		`<div class="doc-cards"><div class="doc-card">a</div></div>`,
		`<details class="doc-faq-item"><summary class="doc-faq-q">q</summary><div class="doc-faq-a">a</div></details>`,
		`<details><summary>q</summary></details>`)
	assertSafeHTML(t, "raw section classes", res.HTML)
}
