package markdown

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// generatedTabButton は expandTabs が出力するボタンの形。名前は文字としてエスケープ済み（< を含まない）。
var generatedTabButton = regexp.MustCompile(`<button type="button" class="doc-tab" role="tab" id="doc-tabs-[0-9]+--tab-[0-9]+" aria-controls="doc-tabs-[0-9]+--panel-[0-9]+" aria-selected="(?:true|false)" tabindex="(?:0|-1)">[^<]*</button>`)

// assertSafeTabsHTML は sorus が出力した形のタブのボタンを除いてから assertSafeHTML で調べる。
// 生 HTML の button や、形の違うボタンが残っていれば assertSafeHTML が見つける。
func assertSafeTabsHTML(t *testing.T, input, out string) {
	t.Helper()
	assertSafeHTML(t, input, generatedTabButton.ReplaceAllString(out, ""))
}

const tabsSource = ":::tabs\n:::tab[macOS]\nbrew install sorus\n:::tab[Windows]\nwinget install sorus\n:::\n\n後ろの段落"

// タブ全体・タブの並び・ボタン・中身を決まった形で出力する。JS が無くても読めるよう、タブの並びは hidden、
// 中身はすべて見えたまま（hidden なし）で、先頭に名前の行を置く。
func TestRender_TabsStructure(t *testing.T) {
	res := mustRender(t, tabsSource)
	want := `<div class="doc-tabs" data-tabs>` + "\n" +
		`<div class="doc-tab-list" role="tablist" hidden>` +
		`<button type="button" class="doc-tab" role="tab" id="doc-tabs-1--tab-1" aria-controls="doc-tabs-1--panel-1" aria-selected="true" tabindex="0">macOS</button>` +
		`<button type="button" class="doc-tab" role="tab" id="doc-tabs-1--tab-2" aria-controls="doc-tabs-1--panel-2" aria-selected="false" tabindex="-1">Windows</button>` +
		"</div>\n" +
		`<div class="doc-tab-panel" role="tabpanel" id="doc-tabs-1--panel-1" aria-labelledby="doc-tabs-1--tab-1">` + "\n" +
		`<p class="doc-tab-title">macOS</p>` + "\n" +
		"<p>brew install sorus</p>\n" +
		"</div>\n" +
		`<div class="doc-tab-panel" role="tabpanel" id="doc-tabs-1--panel-2" aria-labelledby="doc-tabs-1--tab-2">` + "\n" +
		`<p class="doc-tab-title">Windows</p>` + "\n" +
		"<p>winget install sorus</p>\n" +
		"</div>\n" +
		"</div>\n" +
		"<p>後ろの段落</p>\n"
	if res.HTML != want {
		t.Errorf("got\n%s\nwant\n%s", res.HTML, want)
	}
	assertSafeTabsHTML(t, tabsSource, res.HTML)
}

// JS が無いとき（DOM をそのまま表示したとき）の見え方: タブの並びだけが hidden で、すべての中身が名前付きで見える。
func TestRender_TabsProgressiveEnhancement(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(mustRender(t, tabsSource).HTML))
	if err != nil {
		t.Fatal(err)
	}
	var tablists, panels, titles []string
	var walk func(n *html.Node, hidden bool)
	walk = func(n *html.Node, hidden bool) {
		if n.Type == html.ElementNode {
			attrs := map[string]string{}
			for _, a := range n.Attr {
				attrs[a.Key] = a.Val
			}
			if _, ok := attrs["hidden"]; ok {
				hidden = true
			}
			switch {
			case attrs["role"] == "tablist":
				tablists = append(tablists, map[bool]string{true: "hidden", false: "visible"}[hidden])
			case attrs["role"] == "tabpanel":
				panels = append(panels, map[bool]string{true: "hidden", false: "visible"}[hidden])
			case attrs["class"] == TabTitleClass && !hidden:
				titles = append(titles, n.FirstChild.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, hidden)
		}
	}
	walk(doc, false)
	if !reflect.DeepEqual(tablists, []string{"hidden"}) || !reflect.DeepEqual(panels, []string{"visible", "visible"}) ||
		!reflect.DeepEqual(titles, []string{"macOS", "Windows"}) {
		t.Errorf("tablists %v panels %v titles %v", tablists, panels, titles)
	}
}

// 同じ本文は何度描画しても同じ HTML になる（推測できない印は残らず、静的な書き出しの差分にならない）。
func TestRender_TabsAreDeterministic(t *testing.T) {
	a, b := mustRender(t, tabsSource).HTML, mustRender(t, tabsSource).HTML
	if a != b {
		t.Errorf("renders differ:\n%s\n---\n%s", a, b)
	}
	if strings.Contains(a, "[[sorus-tabs:") {
		t.Errorf("marker is left:\n%s", a)
	}
}

// 中身は通常の Markdown として解釈し、見出しの ID・目次、フォトギャラリー、数式も使える。
func TestRender_TabsPanelContent(t *testing.T) {
	src := ":::tabs\n:::tab[Linux]\n## インストール\n- `curl` を使う\n\n$x^2$\n:::tab[写真]\n:::gallery\n![a](/a.webp)\n:::\n:::\n\n## 後ろ"
	res := mustRender(t, src)
	assertContains(t, res.HTML,
		`<h2 id="インストール">インストール</h2>`,
		"<li><code>curl</code> を使う</li>",
		`<span class="math-inline">`,
		`<figure class="`+GalleryFigureClass(DefaultGalleryLayout)+`" data-gallery-layout="justified">`,
		`<img src="/a.webp" data-full="/a.webp" alt="a" loading="lazy" decoding="async">`,
		"</figure>\n</div>\n</div>\n"+`<h2 id="後ろ">後ろ</h2>`)
	want := []Heading{{Level: 2, Text: "インストール", ID: "インストール"}, {Level: 2, Text: "後ろ", ID: "後ろ"}}
	if !reflect.DeepEqual(res.Headings, want) {
		t.Errorf("Headings = %#v, want %#v", res.Headings, want)
	}
	if got := strings.Count(res.HTML, `role="tabpanel"`); got != 2 {
		t.Errorf("panels = %d, want 2:\n%s", got, res.HTML)
	}
}

// フェンスコードブロックの中の ::: や :::tab[名前] では、タブを閉じたり分けたりしない。
func TestRender_TabsIgnoreMarkersInCode(t *testing.T) {
	src := ":::tabs\n:::tab[記法]\n```md\n:::tabs\n:::tab[中]\n:::\n```\n:::tab[次]\n~~~~\n:::\n~~~~\n:::\n"
	res := mustRender(t, src)
	assertContains(t, res.HTML,
		"<pre><code>:::tabs\n:::tab[中]\n:::\n</code></pre>",
		"<pre><code>:::\n</code></pre>")
	if got := strings.Count(res.HTML, `role="tabpanel"`); got != 2 {
		t.Errorf("panels = %d, want 2:\n%s", got, res.HTML)
	}
	if strings.Contains(res.HTML, ">中</button>") {
		t.Errorf("tab inside code is used:\n%s", res.HTML)
	}
}

// 入れ子のタブ・複数のタブ全体では、id が重ならない（タブ全体ごとの番号）。
func TestRender_TabsNestedAndMultipleHaveUniqueIDs(t *testing.T) {
	src := ":::tabs\n:::tab[外1]\n:::tabs\n:::tab[中1]\nx\n:::tab[中2]\ny\n:::\n:::tab[外2]\nz\n:::\n\n:::tabs\n:::tab[別]\nw\n:::\n"
	res := mustRender(t, src)
	doc, err := html.Parse(strings.NewReader(res.HTML))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	var labelled []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				switch a.Key {
				case "id":
					if ids[a.Val] {
						t.Errorf("duplicate id %q", a.Val)
					}
					ids[a.Val] = true
				case "aria-labelledby", "aria-controls":
					labelled = append(labelled, a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if len(ids) != 10 || len(labelled) != 10 {
		t.Errorf("ids %v references %v", ids, labelled)
	}
	for _, ref := range labelled {
		if !ids[ref] {
			t.Errorf("reference %q has no element", ref)
		}
	}
	// 外のタブ全体は外1・外2の2つ、中のタブ全体は中1・中2の2つ。
	assertContains(t, res.HTML,
		`aria-controls="doc-tabs-1--panel-2" aria-selected="false" tabindex="-1">外2</button>`,
		`id="doc-tabs-2--tab-2" aria-controls="doc-tabs-2--panel-2" aria-selected="false" tabindex="-1">中2</button>`,
		`id="doc-tabs-3--tab-1" aria-controls="doc-tabs-3--panel-1" aria-selected="true" tabindex="0">別</button>`)
}

// 引用・注意書き・リストの中にも書ける。
func TestRender_TabsInsideContainers(t *testing.T) {
	for _, src := range []string{
		"> :::tabs\n> :::tab[A]\n> a\n> :::tab[B]\n> b\n> :::",
		"> [!TIP]\n> :::tabs\n> :::tab[A]\n> a\n> :::tab[B]\n> b\n> :::",
		"- 項目\n\n  :::tabs\n  :::tab[A]\n  a\n  :::tab[B]\n  b\n  :::",
	} {
		res := mustRender(t, src)
		if strings.Count(res.HTML, `role="tabpanel"`) != 2 || !strings.Contains(res.HTML, ">B</button>") || strings.Contains(res.HTML, ":::") {
			t.Errorf("%q:\n%s", src, res.HTML)
		}
	}
}

// 閉じの ::: が無いもの、:::tabs の次が :::tab[名前] でないもの、タブ全体の外の :::tab[名前] は通常の段落。
func TestRender_TabsFallBackToParagraph(t *testing.T) {
	for _, src := range []string{
		":::tabs\n:::tab[A]\n閉じが無い",
		":::tabs\n本文\n:::tab[A]\nx\n:::",
		":::tabs\n:::tab[ ]\nx\n:::",
		":::tabs extra\n:::tab[A]\nx\n:::",
		":::tab[A]\nタブ全体の外\n:::",
		":::tabs\n:::tab[A]\n:::gallery\n:::",
	} {
		res := mustRender(t, src)
		if strings.Contains(res.HTML, "doc-tab") || !strings.Contains(res.HTML, ":::") {
			t.Errorf("%q: rendered as tabs:\n%s", src, res.HTML)
		}
	}
	// 空行を挟んだ開きの行、CRLF の改行、行末の空白は受け付ける。
	for _, src := range []string{
		":::tabs\n\n:::tab[A]\na\n\n:::tab[B]\nb\n\n:::",
		":::tabs\r\n:::tab[A]\r\na\r\n:::tab[B]\r\nb\r\n:::\r\n",
		":::tabs  \n:::tab[A]  \na\n:::tab[B]\nb\n:::  ",
	} {
		res := mustRender(t, src)
		if strings.Count(res.HTML, `role="tabpanel"`) != 2 || strings.Contains(res.HTML, ":::") {
			t.Errorf("%q:\n%s", src, res.HTML)
		}
	}
}

// タブの名前は Markdown・HTML として解釈せず、ボタンにも名前の行にも文字としてエスケープして出す。
func TestRender_TabLabelsAreEscaped(t *testing.T) {
	src := ":::tabs\n:::tab[<img src=x onerror=alert(1)>]\nx\n:::tab[\"><script>alert(1)</script>]\ny\n:::tab[A &amp; B [v2]]\nz\n:::"
	res := mustRender(t, src)
	assertContains(t, res.HTML,
		`tabindex="0">&lt;img src=x onerror=alert(1)&gt;</button>`,
		`<p class="doc-tab-title">&lt;img src=x onerror=alert(1)&gt;</p>`,
		`tabindex="-1">&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;</button>`,
		`tabindex="-1">A &amp; B [v2]</button>`,
		`<p class="doc-tab-title">A &amp; B [v2]</p>`)
	assertSafeTabsHTML(t, src, res.HTML)
}

// 中身の生 HTML は今までどおりサニタイズする。生 HTML では button・data-tabs・role="tablist"・外枠とタブの並びのクラスを
// 作れず（スクリプトが動かすタブは sorus の出力だけ）、本文に書いた印の文字は外枠やボタンにならない。
func TestRender_TabsSanitizeContentAndRawHTML(t *testing.T) {
	src := ":::tabs\n:::tab[A]\n<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n" +
		"<button type=\"button\" class=\"doc-tab\" role=\"tab\" onclick=\"alert(1)\">偽</button>\n\n" +
		"[[sorus-tabs:AAAAAAAAAAAAAAAAAAAAAAAAAA:1]] [[sorus-tabs-end:AAAAAAAAAAAAAAAAAAAAAAAAAA]]\n:::\n\n" +
		`<div class="doc-tabs" data-tabs="" role="tablist"><div class="doc-tab-list" hidden>x</div></div>` +
		`<div class="doc-tabs extra" data-tabs aria-labelledby="other" role="dialog"><div class="doc-tab">x</div><p class="doc-tab-panel">y</p></div>`
	res := mustRender(t, src)
	assertSafeTabsHTML(t, src, res.HTML)
	if got := len(generatedTabButton.FindAllString(res.HTML, -1)); got != 1 {
		t.Errorf("buttons = %d, want 1:\n%s", got, res.HTML)
	}
	assertContains(t, res.HTML, "<p>[[sorus-tabs:AAAAAAAAAAAAAAAAAAAAAAAAAA:1]] [[sorus-tabs-end:AAAAAAAAAAAAAAAAAAAAAAAAAA]]</p>", "偽",
		"</div>\n</div>\n<div><div hidden=\"\">x</div></div><div><div>x</div><p>y</p></div>")
	for _, once := range []string{`data-tabs`, `role="tablist"`, `class="doc-tabs"`, `class="doc-tab-list"`} {
		if got := strings.Count(res.HTML, once); got != 1 {
			t.Errorf("%s appears %d times, want only the generated one:\n%s", once, got, res.HTML)
		}
	}
	rest := generatedTabButton.ReplaceAllString(res.HTML, "")
	for _, bad := range []string{`aria-labelledby="other"`, `role="dialog"`, `class="doc-tabs extra"`, `class="doc-tab"`, `<p class="doc-tab-panel">`} {
		if strings.Contains(rest, bad) {
			t.Errorf("%s is kept:\n%s", bad, res.HTML)
		}
	}
}

// タブの目印のクラスは生 HTML の汎用クラスに使えない（公開サイトのスクリプトが利用者の要素を拾わないように）。
func TestClassListPattern_TabsClassesAreReserved(t *testing.T) {
	for value, want := range map[string]bool{
		"doc-tabs": false, "doc-tab-list": false, "doc-tab": false, "doc-tab-panel": false, "doc-tab-title": false,
		"ok doc-tab": false, "doc-tabs ok": false,
		"doc": true, "doc-ta": true, "doc-tabsx": true, "doc-tab-panels": true, "my-doc-tab": true, "doc-tab-": true,
		"callout": true, "callout callout-note": true, "callout-title": true,
	} {
		if got := classListPattern.MatchString(value); got != want {
			t.Errorf("classListPattern.MatchString(%q) = %v, want %v", value, got, want)
		}
	}
}
