package markdown

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func mustRender(t *testing.T, src string) *RenderResult {
	t.Helper()
	res, err := Render(src)
	if err != nil {
		t.Fatalf("Render(%q) error: %v", src, err)
	}
	return res
}

func assertContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output does not contain %q\noutput:\n%s", w, got)
		}
	}
}

// --- GFM ---

func TestRender_GFMTable(t *testing.T) {
	res := mustRender(t, "| 名前 | 点数 | 備考 |\n|:-----|-----:|:----:|\n| 太郎 | 90 | ~~欠席~~ |\n")
	assertContains(t, res.HTML,
		"<table>", "<thead>", "<tbody>",
		`<th align="left">名前</th>`,
		`<th align="right">点数</th>`,
		`<th align="center">備考</th>`,
		`<td align="left">太郎</td>`,
		`<td align="right">90</td>`,
		`<td align="center"><del>欠席</del></td>`,
	)
}

func TestRender_GFMStrikethrough(t *testing.T) {
	res := mustRender(t, "これは~~間違い~~正解です")
	assertContains(t, res.HTML, "<del>間違い</del>")
}

func TestRender_GFMTaskList(t *testing.T) {
	res := mustRender(t, "- [x] 取材\n- [ ] 執筆\n")
	assertContains(t, res.HTML,
		`<li><input checked="" disabled="" type="checkbox"> 取材</li>`,
		`<li><input disabled="" type="checkbox"> 執筆</li>`,
	)
}

func TestRender_GFMAutolink(t *testing.T) {
	res := mustRender(t, "公式サイト https://example.com/path?q=1 と www.example.org を参照\n")
	assertContains(t, res.HTML,
		`<a href="https://example.com/path?q=1" rel="nofollow">https://example.com/path?q=1</a>`,
		`<a href="http://www.example.org" rel="nofollow">www.example.org</a>`,
	)
}

func TestRender_BasicMarkdown(t *testing.T) {
	src := "段落の**強調**と*斜体*と`code`。\n\n> 引用\n\n1. 一\n2. 二\n\n![写真](/static/uploads/a.webp \"キャプション\")\n\n[リンク](https://example.com)\n"
	res := mustRender(t, src)
	assertContains(t, res.HTML,
		"<strong>強調</strong>", "<em>斜体</em>", "<code>code</code>",
		"<blockquote>", "<ol>", "<li>一</li>",
		`<img src="/static/uploads/a.webp" alt="写真" title="キャプション">`,
		`<a href="https://example.com" rel="nofollow">リンク</a>`,
	)
}

func TestRender_CodeBlockIsEscaped(t *testing.T) {
	res := mustRender(t, "```html\n<script>alert(1)</script>\n```\n")
	assertContains(t, res.HTML, "&lt;script&gt;alert(1)&lt;/script&gt;")
	if strings.Contains(res.HTML, "<script") {
		t.Errorf("code block content was emitted as a raw tag: %s", res.HTML)
	}
}

func TestRender_SafeRawHTMLIsKept(t *testing.T) {
	res := mustRender(t, "H<sub>2</sub>O と E=mc<sup>2</sup><br>改行\n")
	assertContains(t, res.HTML, "<sub>2</sub>", "<sup>2</sup>", "<br>")
}

func TestRender_Empty(t *testing.T) {
	res := mustRender(t, "")
	if res.HTML != "" {
		t.Errorf("HTML = %q, want empty", res.HTML)
	}
	if res.Headings == nil || len(res.Headings) != 0 {
		t.Errorf("Headings = %#v, want empty non-nil slice", res.Headings)
	}
}

// --- XSS ---

// dangerousElements はサニタイズ後の HTML に現れてはならない要素。
var dangerousElements = map[string]bool{
	"script": true, "frame": true, "frameset": true, "object": true,
	"embed": true, "applet": true, "style": true, "link": true, "meta": true,
	"base": true, "form": true, "svg": true, "math": true, "textarea": true,
	"button": true, "select": true,
}

// urlAttrs は URL を値に取る属性。
var urlAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "formaction": true,
	"xlink:href": true, "background": true, "poster": true, "data": true, "cite": true,
}

// assertSafeHTML はサニタイズ後の HTML を DOM として解析し、スクリプト実行経路が残っていないことを検証する。
func assertSafeHTML(t *testing.T, input, out string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if dangerousElements[n.Data] {
				t.Errorf("input %q: dangerous element <%s> remains in %q", input, n.Data, out)
			}
			if n.Data == "iframe" {
				validSrc := false
				for _, a := range n.Attr {
					if a.Key == "src" && strings.HasPrefix(a.Val, "https://www.youtube-nocookie.com/embed/") {
						validSrc = true
					}
				}
				if !validSrc {
					t.Errorf("input %q: iframe without approved src remains in %q", input, out)
				}
			}
			if n.Data == "input" {
				for _, a := range n.Attr {
					if a.Key == "type" && a.Val != "checkbox" {
						t.Errorf("input %q: <input type=%q> remains in %q", input, a.Val, out)
					}
				}
			}
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				if strings.HasPrefix(key, "on") {
					t.Errorf("input %q: event handler attribute %q remains in %q", input, a.Key, out)
				}
				if key == "style" || key == "srcdoc" {
					t.Errorf("input %q: attribute %q remains in %q", input, a.Key, out)
				}
				if urlAttrs[key] && !isSafeURL(a.Val) {
					t.Errorf("input %q: unsafe URL %s=%q remains in %q", input, a.Key, a.Val, out)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// 属性・URL は DOM で検証済み。エスケープ済みの本文テキスト（&lt;img onerror=...&gt; 等）は無害なので、
	// 文字列ではタグの開始だけを検査する。
	lower := strings.ToLower(out)
	for _, bad := range []string{"<script", "<iframe", "<object", "<embed", "<svg"} {
		if strings.Contains(lower, bad) {
			t.Errorf("input %q: output contains %q: %q", input, bad, out)
		}
	}
}

func isSafeURL(u string) bool {
	// ブラウザと同様に制御文字・空白を除去してからスキームを判定する
	cleaned := strings.Map(func(r rune) rune {
		if r <= ' ' {
			return -1
		}
		return r
	}, strings.ToLower(u))
	scheme, _, found := strings.Cut(cleaned, ":")
	if !found || strings.ContainsAny(scheme, "/?#") {
		return true // 相対 URL
	}
	switch scheme {
	case "http", "https", "mailto":
		return true
	}
	return false
}

func TestRender_XSSIsNeutralized(t *testing.T) {
	cases := map[string]string{
		"script tag":                  `<script>alert(1)</script>`,
		"script inline":               `本文 <script>alert(document.cookie)</script> 本文`,
		"script uppercase":            `<SCRIPT SRC=https://evil.example/x.js></SCRIPT>`,
		"script unclosed":             "<script>\nalert(1)\n",
		"img onerror":                 `<img src=x onerror=alert(1)>`,
		"img onerror quoted":          `<img src="x" onerror="alert('xss')">`,
		"img onerror inline":          `テキスト <img src=x onerror=alert(1)> テキスト`,
		"svg onload":                  `<svg onload=alert(1)>`,
		"svg script":                  `<svg><script>alert(1)</script></svg>`,
		"body onload":                 `<body onload=alert(1)>`,
		"div onmouseover":             `<div onmouseover="alert(1)">hover</div>`,
		"iframe src":                  `<iframe src="https://evil.example/"></iframe>`,
		"iframe javascript":           `<iframe src="javascript:alert(1)"></iframe>`,
		"iframe srcdoc":               `<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
		"object data":                 `<object data="https://evil.example/x.swf"></object>`,
		"embed":                       `<embed src="https://evil.example/x.swf">`,
		"style tag":                   `<style>body{background:url("javascript:alert(1)")}</style>`,
		"style attr":                  `<p style="background:url(javascript:alert(1))">x</p>`,
		"meta refresh":                `<meta http-equiv="refresh" content="0;url=javascript:alert(1)">`,
		"base href":                   `<base href="javascript:alert(1)//">`,
		"form action":                 `<form action="javascript:alert(1)"><button>送信</button></form>`,
		"input autofocus":             `<input autofocus onfocus=alert(1)>`,
		"a javascript":                `<a href="javascript:alert(1)">クリック</a>`,
		"a javascript mixed case":     `<a href="JaVaScRiPt:alert(1)">クリック</a>`,
		"a javascript entity":         `<a href="&#106;avascript:alert(1)">クリック</a>`,
		"a javascript hex entity":     `<a href="&#x6A;&#x61;vascript:alert(1)">クリック</a>`,
		"a javascript tab":            "<a href=\"java\tscript:alert(1)\">クリック</a>",
		"a javascript leading space":  `<a href="  javascript:alert(1)">クリック</a>`,
		"a vbscript":                  `<a href="vbscript:msgbox(1)">クリック</a>`,
		"a data html":                 `<a href="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">クリック</a>`,
		"md link javascript":          `[クリック](javascript:alert(1))`,
		"md link javascript encoded":  `[クリック](javascript&#58;alert(1))`,
		"md link javascript percent":  `[クリック](javascript%3Aalert(1))`,
		"md image javascript":         `![画像](javascript:alert(1))`,
		"md autolink javascript":      `<javascript:alert(1)>`,
		"md reference javascript":     "[クリック][x]\n\n[x]: javascript:alert(1)\n",
		"md link data":                `[クリック](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)`,
		"heading script":              `## <script>alert(1)</script>見出し`,
		"heading img onerror":         `## 見出し<img src=x onerror=alert(1)>`,
		"table cell script":           "| a |\n|---|\n| <script>alert(1)</script> |\n",
		"task list onclick":           "- [ ] <span onclick=alert(1)>todo</span>\n",
		"mutation xss math":           `<math><mtext><table><mglyph><style><img src=x onerror=alert(1)>`,
		"mutation xss noscript":       `<noscript><p title="</noscript><img src=x onerror=alert(1)>">`,
		"comment breakout":            `<!--><img src=x onerror=alert(1)>-->`,
		"attribute breakout":          `<a href="https://example.com" title="x" onclick="alert(1)">a</a>`,
		"nested quote breakout":       `<img src="x"" onerror="alert(1)">`,
		"html block multiple vectors": "<div>\n<script>alert(1)</script>\n<img src=x onerror=alert(1)>\n<iframe src=//evil.example></iframe>\n</div>\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			res := mustRender(t, input)
			assertSafeHTML(t, input, res.HTML)
		})
	}
}

func TestRender_YouTubeNoCookieEmbed(t *testing.T) {
	res := mustRender(t, `<div class="video-container"><iframe src="https://www.youtube-nocookie.com/embed/tQMkVSRwo7M" title="動画" loading="lazy" allowfullscreen referrerpolicy="strict-origin-when-cross-origin"></iframe></div>`)
	assertContains(t, res.HTML,
		`<div class="video-container">`,
		`<iframe src="https://www.youtube-nocookie.com/embed/tQMkVSRwo7M"`,
		`title="動画"`, `loading="lazy"`, `allowfullscreen=""`,
		`referrerpolicy="strict-origin-when-cross-origin"`,
	)

	for name, src := range map[string]string{
		"evil host":  "https://evil.com/embed/tQMkVSRwo7M",
		"javascript": "javascript:alert(1)",
		"wrong host": "https://www.youtube.com/embed/tQMkVSRwo7M",
		"invalid id": "https://www.youtube-nocookie.com/embed/short",
	} {
		t.Run(name, func(t *testing.T) {
			got := mustRender(t, `<iframe src="`+src+`" title="bad"></iframe>`).HTML
			if strings.Contains(got, "<iframe") {
				t.Errorf("unapproved iframe remains: %s", got)
			}
		})
	}
}

func TestRender_XSSKeepsSurroundingContent(t *testing.T) {
	res := mustRender(t, "前の段落\n\n<script>alert(1)</script>\n\n後の段落 [安全なリンク](https://example.com)\n")
	assertContains(t, res.HTML, "<p>前の段落</p>", "<p>後の段落", `href="https://example.com"`)
	if strings.Contains(res.HTML, "alert(1)") {
		t.Errorf("script content should be removed entirely: %s", res.HTML)
	}
}

func TestRender_HeadingTOCExcludesRawHTML(t *testing.T) {
	res := mustRender(t, "## <script>alert(1)</script>見出し\n")
	if len(res.Headings) != 1 {
		t.Fatalf("Headings = %#v, want 1", res.Headings)
	}
	h := res.Headings[0]
	if strings.Contains(h.Text, "<") || strings.Contains(h.ID, "<") || strings.Contains(h.ID, "script") {
		t.Errorf("heading leaked raw HTML: %+v", h)
	}
}

// --- 見出し・目次 ---

func TestRender_HeadingsAndAnchorIDs(t *testing.T) {
	src := strings.Join([]string{
		"# 記事タイトル",
		"## はじめに",
		"本文",
		"### 背景と目的",
		"## Getting Started",
		"### `go test` の実行",
		"## **強調** と [リンク](https://example.com)",
		"#### 深い見出し",
		"",
	}, "\n\n")
	res := mustRender(t, src)

	want := []Heading{
		{Level: 1, Text: "記事タイトル", ID: "記事タイトル"},
		{Level: 2, Text: "はじめに", ID: "はじめに"},
		{Level: 3, Text: "背景と目的", ID: "背景と目的"},
		{Level: 2, Text: "Getting Started", ID: "getting-started"},
		{Level: 3, Text: "go test の実行", ID: "go-test-の実行"},
		{Level: 2, Text: "強調 と リンク", ID: "強調-と-リンク"},
		{Level: 4, Text: "深い見出し", ID: "深い見出し"},
	}
	if !reflect.DeepEqual(res.Headings, want) {
		t.Errorf("Headings =\n%#v\nwant\n%#v", res.Headings, want)
	}
	for _, h := range want {
		tag := "<h" + string(rune('0'+h.Level)) + ` id="` + h.ID + `">`
		assertContains(t, res.HTML, tag)
	}
}

func TestRender_DuplicateHeadingIDsAreUnique(t *testing.T) {
	res := mustRender(t, "## まとめ\n\n## まとめ\n\n## まとめ\n\n## まとめ-1\n")
	var ids []string
	for _, h := range res.Headings {
		ids = append(ids, h.ID)
	}
	want := []string{"まとめ", "まとめ-1", "まとめ-2", "まとめ-1-1"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("IDs = %v, want %v", ids, want)
	}
	for _, id := range want {
		assertContains(t, res.HTML, `id="`+id+`"`)
	}
}

func TestRender_SetextHeadings(t *testing.T) {
	res := mustRender(t, "大見出し\n========\n\n小見出し\n--------\n")
	want := []Heading{
		{Level: 1, Text: "大見出し", ID: "大見出し"},
		{Level: 2, Text: "小見出し", ID: "小見出し"},
	}
	if !reflect.DeepEqual(res.Headings, want) {
		t.Errorf("Headings = %#v, want %#v", res.Headings, want)
	}
}

func TestRender_HeadingIDsAreIndependentPerCall(t *testing.T) {
	for i := 0; i < 2; i++ {
		res := mustRender(t, "## 概要\n")
		if res.Headings[0].ID != "概要" {
			t.Fatalf("call %d: ID = %q, want 概要 (IDs must not leak between calls)", i, res.Headings[0].ID)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"  前後の空白  ", "前後の空白"},
		{"Q&A: よくある質問!?", "qa-よくある質問"},
		{"2024年 春 の 新歓", "2024年-春-の-新歓"},
		{"multi   space\tand-dash -- x", "multi-space-and-dash-x"},
		{"snake_case_name", "snake_case_name"},
		{"全角　スペース", "全角-スペース"},
		{"!!!", "heading"},
		{"", "heading"},
		// NFD の結合濁点・半濁点は NFC に合成してから判定する（結合文字を落とさない）。
		{"\u304b\u3099\u304f\u305b\u3044", "がくせい"},
		{"\u30cf\u309a\u30fc\u30c6\u30a3\u30fc", "パーティー"},
		{"Cafe\u0301 Menu", "café-menu"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := slugify(c.in); !headingIDPattern.MatchString(got) {
			t.Errorf("slugify(%q) = %q is rejected by the sanitizer pattern", c.in, got)
		}
	}
}

func TestRender_NFDHeadingIDIsNFC(t *testing.T) {
	// macOS 由来の NFD テキストでも、NFC の見出しと同じ ID になり重複は連番で区別される。
	res := mustRender(t, "## \u304b\u3099\u304f\u305b\u3044\n\n## がくせい\n")
	if res.Headings[0].ID != "がくせい" || res.Headings[1].ID != "がくせい-1" {
		t.Errorf("Headings = %#v", res.Headings)
	}
	assertContains(t, res.HTML, `<h2 id="がくせい">`, `<h2 id="がくせい-1">`)
}

func TestRender_SymbolOnlyHeadingFallsBack(t *testing.T) {
	res := mustRender(t, "## !!!\n\n## ???\n")
	if res.Headings[0].ID != "heading" || res.Headings[1].ID != "heading-1" {
		t.Errorf("Headings = %#v", res.Headings)
	}
	assertContains(t, res.HTML, `<h2 id="heading">!!!</h2>`, `<h2 id="heading-1">???</h2>`)
}

func TestRender_Concurrent(t *testing.T) {
	src := "## 見出し\n\n| a |\n|---|\n| 1 |\n\n<script>alert(1)</script>\n\n## 見出し\n"
	want := mustRender(t, src)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Render(src)
			if err != nil {
				t.Errorf("Render error: %v", err)
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("concurrent Render result differs:\n%#v\nwant\n%#v", got, want)
			}
		}()
	}
	wg.Wait()
}

// --- フロントマター ---

func TestParseFrontmatter_Basic(t *testing.T) {
	content := "---\ntitle: 新歓特集\nslug: welcome-2024\ndraft: false\norder: 3\ntags:\n  - 新歓\n  - サークル\nauthor:\n  name: 山田\n---\n# 本文\n\n内容\n"
	meta, body, err := ParseFrontmatter(content)
	if err != nil {
		t.Fatalf("ParseFrontmatter error: %v", err)
	}
	want := map[string]interface{}{
		"title":  "新歓特集",
		"slug":   "welcome-2024",
		"draft":  false,
		"order":  3,
		"tags":   []interface{}{"新歓", "サークル"},
		"author": map[string]interface{}{"name": "山田"},
	}
	if !reflect.DeepEqual(meta, want) {
		t.Errorf("meta =\n%#v\nwant\n%#v", meta, want)
	}
	if body != "# 本文\n\n内容\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParseFrontmatter_Timestamp(t *testing.T) {
	meta, _, err := ParseFrontmatter("---\npublished_at: 2024-04-01T09:00:00+09:00\nraw: \"2024-04-01\"\n---\n")
	if err != nil {
		t.Fatalf("ParseFrontmatter error: %v", err)
	}
	ts, ok := meta["published_at"].(time.Time)
	if !ok {
		t.Fatalf("published_at = %#v (%T), want time.Time", meta["published_at"], meta["published_at"])
	}
	if !ts.Equal(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("published_at = %v", ts)
	}
	if meta["raw"] != "2024-04-01" {
		t.Errorf("quoted date should stay a string: %#v", meta["raw"])
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	for _, content := range []string{
		"",
		"# 見出し\n\n本文\n",
		"本文\n---\nkey: value\n---\n",
		"----\nkey: value\n----\n",
	} {
		meta, body, err := ParseFrontmatter(content)
		if err != nil {
			t.Errorf("ParseFrontmatter(%q) error: %v", content, err)
		}
		if meta == nil || len(meta) != 0 {
			t.Errorf("ParseFrontmatter(%q) meta = %#v, want empty non-nil map", content, meta)
		}
		if body != content {
			t.Errorf("ParseFrontmatter(%q) body = %q, want unchanged", content, body)
		}
	}
}

func TestParseFrontmatter_UnclosedIsTreatedAsBody(t *testing.T) {
	content := "---\n\n本文（先頭の水平線）\n"
	meta, body, err := ParseFrontmatter(content)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(meta) != 0 || body != content {
		t.Errorf("meta = %#v, body = %q", meta, body)
	}
}

func TestParseFrontmatter_EmptyBlock(t *testing.T) {
	meta, body, err := ParseFrontmatter("---\n---\n本文\n")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if meta == nil || len(meta) != 0 || body != "本文\n" {
		t.Errorf("meta = %#v, body = %q", meta, body)
	}
}

func TestParseFrontmatter_CommentOnlyBlock(t *testing.T) {
	meta, body, err := ParseFrontmatter("---\n# コメントのみ\n---\n本文\n")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if meta == nil || len(meta) != 0 || body != "本文\n" {
		t.Errorf("meta = %#v, body = %q", meta, body)
	}
}

func TestParseFrontmatter_CRLFAndBOM(t *testing.T) {
	meta, body, err := ParseFrontmatter("\uFEFF---\r\ntitle: Windows\r\n---\r\n本文\r\n2行目\r\n")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if meta["title"] != "Windows" {
		t.Errorf("meta = %#v", meta)
	}
	if body != "本文\n2行目\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParseFrontmatter_DotsTerminatorAndTrailingSpaces(t *testing.T) {
	meta, body, err := ParseFrontmatter("--- \ntitle: T\n...\n本文")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if meta["title"] != "T" || body != "本文" {
		t.Errorf("meta = %#v, body = %q", meta, body)
	}
}

func TestParseFrontmatter_BodyKeepsLaterDelimiters(t *testing.T) {
	meta, body, err := ParseFrontmatter("---\ntitle: T\n---\n前半\n\n---\n\n後半\n")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if meta["title"] != "T" || body != "前半\n\n---\n\n後半\n" {
		t.Errorf("meta = %#v, body = %q", meta, body)
	}
}

func TestParseFrontmatter_NoBodyAfterClosing(t *testing.T) {
	meta, body, err := ParseFrontmatter("---\ntitle: T\n---")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if meta["title"] != "T" || body != "" {
		t.Errorf("meta = %#v, body = %q", meta, body)
	}
}

func TestParseFrontmatter_InvalidYAML(t *testing.T) {
	for name, content := range map[string]string{
		"syntax error": "---\ntitle: [unclosed\n---\n本文\n",
		"bad indent":   "---\ntitle: a\n  b: c\n---\n本文\n",
		"sequence":     "---\n- a\n- b\n---\n本文\n",
		"scalar":       "---\njust a string\n---\n本文\n",
	} {
		t.Run(name, func(t *testing.T) {
			meta, body, err := ParseFrontmatter(content)
			if !errors.Is(err, ErrInvalidFrontmatter) {
				t.Fatalf("err = %v, want ErrInvalidFrontmatter", err)
			}
			if meta != nil || body != "" {
				t.Errorf("meta = %#v, body = %q, want zero values on error", meta, body)
			}
		})
	}
}

func TestSerializeFrontmatter_RoundTrip(t *testing.T) {
	meta := map[string]interface{}{
		"title": "新歓: 特集 #1",
		"tags":  []interface{}{"新歓", "サークル"},
		"order": 2,
		"draft": true,
		"multi": "1行目\n2行目",
	}
	body := "# 本文\n\n---\n\n続き\n"
	s, err := SerializeFrontmatter(meta, body)
	if err != nil {
		t.Fatalf("SerializeFrontmatter error: %v", err)
	}
	if !strings.HasPrefix(s, "---\n") {
		t.Errorf("serialized = %q, want leading delimiter", s)
	}
	gotMeta, gotBody, err := ParseFrontmatter(s)
	if err != nil {
		t.Fatalf("ParseFrontmatter error: %v\n%s", err, s)
	}
	if !reflect.DeepEqual(gotMeta, meta) {
		t.Errorf("meta =\n%#v\nwant\n%#v", gotMeta, meta)
	}
	if gotBody != body {
		t.Errorf("body = %q, want %q", gotBody, body)
	}
}

func TestSerializeFrontmatter_EmptyMeta(t *testing.T) {
	for _, meta := range []map[string]interface{}{nil, {}} {
		s, err := SerializeFrontmatter(meta, "本文\n")
		if err != nil || s != "本文\n" {
			t.Errorf("SerializeFrontmatter(%#v) = %q, %v", meta, s, err)
		}
	}
}

func TestParseFrontmatterThenRender(t *testing.T) {
	meta, body, err := ParseFrontmatter("---\ntitle: 記事\n---\n## 目次1\n\n<script>alert(1)</script>\n")
	if err != nil {
		t.Fatalf("ParseFrontmatter error: %v", err)
	}
	res := mustRender(t, body)
	if meta["title"] != "記事" {
		t.Errorf("meta = %#v", meta)
	}
	if strings.Contains(res.HTML, "title:") || strings.Contains(res.HTML, "<hr") {
		t.Errorf("frontmatter leaked into HTML: %s", res.HTML)
	}
	if len(res.Headings) != 1 || res.Headings[0].ID != "目次1" {
		t.Errorf("Headings = %#v", res.Headings)
	}
	assertSafeHTML(t, body, res.HTML)
}

// --- 本文の生 HTML のクラス（テーマの CSS から選ぶ） ---

// classAttrs は HTML の class 属性を持つ要素を、文書順に「要素名.class の値」で返す。
func classAttrs(t *testing.T, out string) []string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	var got []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "class" {
					got = append(got, n.Data+"."+a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return got
}

func TestRender_SemanticHTMLClassesAreKept(t *testing.T) {
	src := strings.Join([]string{
		`<div class="emblem-card">`,
		`<figure class="emblem-fig"><img class="emblem-img" src="/uploads/emblem.webp" alt="校章"><figcaption class="emblem-caption">校章</figcaption></figure>`,
		`<p class="hero-lead-text">リード文</p>`,
		`<ul class="company-grid-list"><li class="company-item">A社</li></ul>`,
		`<ol class="steps steps--numbered"><li>手順</li></ol>`,
		`<table class="data_table"><tr><td>1</td></tr></table>`,
		`<blockquote class="pull-quote">引用</blockquote>`,
		"<section class=\"intro two-col\tWide\">紹介</section>",
		`<span class="label-new">新着</span>`,
		`<figure class="article-figure"><img src="/uploads/a.webp" alt=""></figure>`,
		`</div>`,
	}, "\n")
	res := mustRender(t, src)
	want := []string{
		"div.emblem-card",
		"figure.emblem-fig", "img.emblem-img", "figcaption.emblem-caption",
		"p.hero-lead-text",
		"ul.company-grid-list", "li.company-item",
		"ol.steps steps--numbered",
		"table.data_table",
		"blockquote.pull-quote",
		"section.intro two-col\tWide",
		"span.label-new",
		"figure.article-figure",
	}
	if got := classAttrs(t, res.HTML); !reflect.DeepEqual(got, want) {
		t.Errorf("classes =\n%q\nwant\n%q\nhtml:\n%s", got, want, res.HTML)
	}
	assertContains(t, res.HTML, `<img class="emblem-img" src="/uploads/emblem.webp" alt="校章">`, "リード文", "A社", "引用")
	assertSafeHTML(t, src, res.HTML)
}

// クラスを残す要素でも、危険な属性・要素・URL は従来どおり落とす。
func TestRender_SemanticHTMLClassesKeepXSSProtection(t *testing.T) {
	src := strings.Join([]string{
		`<figure class="article-figure" style="background:url(javascript:alert(1))" onclick="alert(1)">`,
		`<img class="emblem-img" src="javascript:alert(2)" onerror="alert(3)" alt="x">`,
		`<img class="emblem-img" src="/ok.webp" srcset="javascript:alert(4)" onload="alert(5)">`,
		`<figcaption class="cap" onmouseover="alert(6)">説明</figcaption>`,
		`</figure>`,
		`<p class="hero-lead-text" style="color:red" onfocus="alert(7)" tabindex="1">本文</p>`,
		`<div class="emblem-card" data-x="1" contenteditable="true"><iframe class="frame" src="https://evil.example/"></iframe><script class="x">alert(8)</script></div>`,
		`<span class="note"><a class="btn" href="javascript:alert(9)">リンク</a></span>`,
	}, "\n")
	res := mustRender(t, src)
	assertSafeHTML(t, src, res.HTML)
	assertNoEventHandlers(t, res.HTML)
	for _, bad := range []string{"alert(", "javascript:", "style=", "srcset", "tabindex", "data-x", "contenteditable", "evil.example", `class="btn"`, `class="frame"`} {
		if strings.Contains(res.HTML, bad) {
			t.Errorf("unsafe %q kept: %s", bad, res.HTML)
		}
	}
	want := []string{"figure.article-figure", "img.emblem-img", "img.emblem-img", "figcaption.cap", "p.hero-lead-text", "div.emblem-card", "span.note"}
	if got := classAttrs(t, res.HTML); !reflect.DeepEqual(got, want) {
		t.Errorf("classes =\n%q\nwant\n%q\nhtml:\n%s", got, want, res.HTML)
	}
}

// クラス名の形に合わない値は、値ごと落とす（要素と中身は残す）。対象外の要素のクラスも落とす。
func TestRender_InvalidClassValuesAreDropped(t *testing.T) {
	for name, src := range map[string]string{
		"colon":            `<p class="md:flex">x</p>`,
		"javascript":       `<p class="javascript:alert(1)">x</p>`,
		"one bad token":    `<p class="lead bad.token">x</p>`,
		"quote breakout":   `<p class="lead&quot; onclick=&quot;alert(1)">x</p>`,
		"angle bracket":    `<p class="lead<img">x</p>`,
		"non ascii":        `<p class="見出し">x</p>`,
		"empty":            `<p class="">x</p>`,
		"leading space":    `<p class=" lead">x</p>`,
		"trailing space":   `<p class="lead ">x</p>`,
		"template braces":  `<p class="{{.X}}">x</p>`,
		"heading":          `<h2 class="section-title">x</h2>`,
		"link":             `<p><a class="btn" href="https://example.com/">x</a></p>`,
		"table cell":       `<table><tr><td class="num">x</td></tr></table>`,
		"inline emphasis":  `<p><em class="accent">x</em></p>`,
		"youtube iframe":   `<iframe class="video" src="https://www.youtube-nocookie.com/embed/tQMkVSRwo7M"></iframe>`,
		"input checkbox":   `<p><input class="check" type="checkbox" disabled></p>`,
		"article element":  `<article class="card">x</article>`,
		"preformatted":     "<pre class=\"code\">x</pre>",
		"code":             `<p><code class="language-go">x</code></p>`,
		"class duplicated": `<p class="lead" class="bad:one">x</p>`,
	} {
		t.Run(name, func(t *testing.T) {
			res := mustRender(t, src)
			got := classAttrs(t, res.HTML)
			switch name {
			case "class duplicated":
				// HTML の解析は最初の class 属性だけを使う。
				if !reflect.DeepEqual(got, []string{"p.lead"}) {
					t.Errorf("classes = %q, html = %s", got, res.HTML)
				}
			default:
				if len(got) != 0 {
					t.Errorf("classes = %q, want none\nhtml: %s", got, res.HTML)
				}
			}
			if name != "youtube iframe" { // assertSafeHTML は許可した YouTube の iframe も拒否する
				assertSafeHTML(t, src, res.HTML)
			}
		})
	}
}

// 数式・フォトギャラリーの目印のクラスは、生成物と同じ要素・同じ値のときだけ残す。
// 生 HTML の汎用クラスに混ぜると、class 属性ごと落とす（lightbox.js・KaTeX が利用者の要素を拾わないように）。
func TestRender_ReservedClassesOnlyInGeneratedForm(t *testing.T) {
	for _, src := range []string{
		`<p class="gallery-item">x</p>`,
		`<div class="gallery-item extra">x</div>`,
		`<div class="math-inline">x</div>`,
		`<span class="note math-display">x</span>`,
		`<section class="photo-gallery">x</section>`,
		`<figure class="masonry-grid wide">x</figure>`,
		`<figure class="photo-gallery masonry-grid">x</figure>`,
		`<figure class="photo-gallery masonry-grid extra">x</figure>`,
		`<img class="gallery-item" src="/a.webp">`,
		`<div class="gallery-layout-cards">x</div>`,
		`<figure class="gallery-layout-justified extra">x</figure>`,
		`<figure class="photo-gallery gallery-layout-masonry">x</figure>`, // 段組みは masonry-grid も付けた形だけを残す
		`<figure class="photo-gallery gallery-layout-cards masonry-grid">x</figure>`,
	} {
		if got := classAttrs(t, mustRender(t, src).HTML); len(got) != 0 {
			t.Errorf("%s: classes = %q, want none", src, got)
		}
	}

	// 予約名に似ているだけのクラス名は汎用クラスとして残す。
	res := mustRender(t, `<div class="gallery-items my-gallery-item math photo-gallery-wide Math-Inline">x</div>`)
	if got := classAttrs(t, res.HTML); !reflect.DeepEqual(got, []string{"div.gallery-items my-gallery-item math photo-gallery-wide Math-Inline"}) {
		t.Errorf("classes = %q", got)
	}

	// 生成物の形はこれまでどおり残る。
	res = mustRender(t, "数式 $x^2$ と\n\n$$y$$\n\n:::gallery\n![a](/a.webp)\n:::\n\n"+
		`<span class="math-inline">\(z\)</span><figure class="photo-gallery gallery-layout-masonry masonry-grid"><div class="gallery-item">x</div></figure>`)
	want := []string{
		"span." + MathInlineClass, "span." + MathDisplayClass,
		"figure." + GalleryFigureClass(DefaultGalleryLayout), "div." + GalleryItemClass,
		"span." + MathInlineClass, "figure." + GalleryFigureClass(GalleryLayoutMasonry), "div." + GalleryItemClass,
	}
	if got := classAttrs(t, res.HTML); !reflect.DeepEqual(got, want) {
		t.Errorf("classes =\n%q\nwant\n%q\nhtml:\n%s", got, want, res.HTML)
	}
}

func TestClassListPattern(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"emblem-card", true},
		{"photo-gallery-item", true},
		{"a", true},
		{"-", true},
		{"_x", true},
		{"A0_-z", true},
		{"one two  three", true},
		{"one\ttwo\nthree\r\nfour", true},
		{"gallery-items", true},
		{"xgallery-item", true},
		{"gallery-ite", true},
		{"math-inline2", true},
		{"masonry", true},
		{"", false},
		{" ", false},
		{" one", false},
		{"one ", false},
		{"one\vtwo", false},
		{"one,two", false},
		{"one.two", false},
		{"one:two", false},
		{"<b>", false},
		{"クラス", false},
		{"math-inline", false},
		{"math-display", false},
		{"gallery-item", false},
		{"photo-gallery", false},
		{"masonry-grid", false},
		{"photo-gallery masonry-grid", false},
		{"gallery-layout-justified", false},
		{"gallery-layout-masonry", false},
		{"gallery-layout-cards", false},
		{"photo-gallery gallery-layout-cards", false},
		{"ok gallery-layout-masonry", false},
		{"gallery-layout", true},
		{"gallery-layout-grid", true},
		{"gallery-layout-cardsx", true},
		{"ok gallery-item", false},
		{"gallery-item ok", false},
		{"ok\tmath-display\tok", false},
	} {
		if got := classListPattern.MatchString(tc.value); got != tc.want {
			t.Errorf("classListPattern.MatchString(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
	// 予約名の一方がもう一方の接頭辞でも、どちらも拒否し、その間の長さは許可する。
	p := regexp.MustCompile(classListExcluding([]string{"ab", "abcd", "a-c"}))
	for value, want := range map[string]bool{
		"a": true, "ab": false, "abc": true, "abcd": false, "abcde": true, "a-": true, "a-c": false, "a-cd": true, "b": true,
		"abc a-": true, "abc ab": false,
	} {
		if got := p.MatchString(value); got != want {
			t.Errorf("classListExcluding MatchString(%q) = %v, want %v", value, got, want)
		}
	}
}
