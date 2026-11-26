package markdown

import (
	"strings"
	"testing"
)

func TestRender_MermaidFence(t *testing.T) {
	res := mustRender(t, "図:\n\n```mermaid\nflowchart LR\n  A[開始] --> B{判定}\n  B -->|はい| C\n```\n\n以上。")
	assertContains(t, res.HTML,
		"<pre class=\"mermaid\">flowchart LR\n  A[開始] --&gt; B{判定}\n  B --&gt;|はい| C\n</pre>")
	if strings.Contains(res.HTML, "<code") {
		t.Errorf("mermaid fence was rendered as a code block: %s", res.HTML)
	}
	if !HasMermaid(res.HTML) {
		t.Error("HasMermaid = false")
	}
}

func TestRender_MermaidLanguageIsCaseInsensitive(t *testing.T) {
	res := mustRender(t, "```Mermaid\nsequenceDiagram\n  Alice->>Bob: Hi\n```\n")
	assertContains(t, res.HTML, "<pre class=\"mermaid\">sequenceDiagram\n  Alice-&gt;&gt;Bob: Hi\n</pre>")
}

// 図の定義に書いた HTML はエスケープしたテキストのまま出し、要素・属性として差し込ませない。
func TestRender_MermaidIsEscaped(t *testing.T) {
	src := "```mermaid\nflowchart LR\n  A[\"<img src=x onerror=alert(1)>\"] --> B[\"<script>alert(2)</script>\"]\n  click A \"javascript:alert(3)\"\n```\n"
	res := mustRender(t, src)
	assertContains(t, res.HTML,
		`&lt;img src=x onerror=alert(1)&gt;`,
		`&lt;script&gt;alert(2)&lt;/script&gt;`)
	for _, bad := range []string{"<img", "<script", "onerror=\"", "</pre><"} {
		if strings.Contains(res.HTML, bad) {
			t.Fatalf("unescaped %q in mermaid output: %s", bad, res.HTML)
		}
	}
	// 閉じタグを書いても pre の外に出られない。
	res = mustRender(t, "```mermaid\ngraph TD\n</pre><script>alert(1)</script>\n```\n")
	if strings.Contains(res.HTML, "<script") || strings.Count(res.HTML, "</pre>") != 1 {
		t.Fatalf("mermaid content escaped the pre element: %s", res.HTML)
	}
}

// 通常のコードブロック・空の mermaid ブロックは今までどおりのコードブロックで、図にしない。
func TestRender_NonMermaidFencesStayCode(t *testing.T) {
	for _, src := range []string{
		"```go\nfmt.Println(\"mermaid\")\n```\n",
		"```\nflowchart LR\n  A --> B\n```\n",
		"```mermaidx\nflowchart LR\n```\n",
		"```mermaid\n\n```\n",
		"    flowchart LR\n      A --> B\n", // インデントのコードブロック
		"`mermaid` はインラインコード。",
	} {
		res := mustRender(t, src)
		if HasMermaid(res.HTML) {
			t.Errorf("Render(%q) produced a mermaid diagram: %s", src, res.HTML)
		}
	}
	res := mustRender(t, "```go\nx := 1\n```\n")
	assertContains(t, res.HTML, "<pre><code>x := 1\n</code></pre>")
}

// 注意書き・タブ・リストの中の mermaid ブロックも図にする。
func TestRender_MermaidInsideContainers(t *testing.T) {
	for name, src := range map[string]string{
		"callout": "> [!NOTE]\n> ```mermaid\n> flowchart LR\n>   A --> B\n> ```\n",
		"list":    "- 手順\n\n  ```mermaid\n  flowchart LR\n    A --> B\n  ```\n",
		"tabs":    ":::tabs\n:::tab[図]\n```mermaid\nflowchart LR\n  A --> B\n```\n:::\n",
	} {
		res := mustRender(t, src)
		if !HasMermaid(res.HTML) || !strings.Contains(res.HTML, "A --&gt; B") {
			t.Errorf("%s: mermaid diagram not rendered: %s", name, res.HTML)
		}
	}
}

// 利用者が書いた生 HTML では、mermaid クラスは pre に単独で書いたときだけ残り、汎用クラスとしては使えない。
// 残った pre の中身もサニタイズされ、スクリプト・イベント属性は落ちる。
func TestRender_MermaidClassOnlyOnPre(t *testing.T) {
	res := mustRender(t, `<div class="mermaid">graph TD</div><span class="mermaid note">x</span><pre class="mermaid evil">y</pre>`)
	if strings.Contains(res.HTML, "mermaid") {
		t.Fatalf("reserved mermaid class kept on raw HTML: %s", res.HTML)
	}
	res = mustRender(t, `<pre class="mermaid" onclick="x()">graph TD<script>alert(1)</script><img src=x onerror=y></pre>`)
	for _, bad := range []string{"onclick", "<script", "onerror"} {
		if strings.Contains(res.HTML, bad) {
			t.Fatalf("unsafe %q kept in raw mermaid pre: %s", bad, res.HTML)
		}
	}
}

func TestHasMermaid(t *testing.T) {
	if HasMermaid(`<pre><code class="language-mermaid">graph TD</code></pre>`) {
		t.Error("HasMermaid matched a plain code block")
	}
	if !HasMermaid(`<p>x</p><pre class="mermaid">graph TD</pre>`) {
		t.Error("HasMermaid missed a diagram")
	}
}
