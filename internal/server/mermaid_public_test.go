package server

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/static"
)

// mermaidDiagramMarkdown は Mermaid の図を1つ含む本文。
const mermaidDiagramMarkdown = "手順の流れ:\n\n```mermaid\nflowchart LR\n  A[申請] --> B{承認}\n  B -->|はい| C[公開]\n```\n"

// mermaidVendorPath は同梱の Mermaid 本体の、公開サイトでの URL のパス（?v= のハッシュを除く）。
const mermaidVendorPath = "/static/vendor/mermaid/mermaid.min.js"

// expectNoExternalMermaid は、ページが外部の配信元から Mermaid を読み込まないことを確かめる。
func expectNoExternalMermaid(t *testing.T, label, body string) {
	t.Helper()
	for _, cdn := range []string{"cdn.jsdelivr.net", "unpkg.com", "cdnjs.cloudflare.com", "esm.sh"} {
		if strings.Contains(body, cdn) {
			t.Errorf("%s loads Mermaid from an external CDN (%s)", label, cdn)
		}
	}
}

// 図のある記事・固定ページ・トップに出したページだけが、同梱の Mermaid を読み込む。
func TestPublicPagesLoadMermaidOnlyWithDiagrams(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{Title: "申請の流れ", Slug: "flow", ContentMarkdown: mermaidDiagramMarkdown,
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
		seedArticle(t, db, article.Article{Title: "コードだけ", Slug: "code-only",
			ContentMarkdown: "```go\nfmt.Println(\"mermaid\")\n```\n", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
		seedPage(t, db, article.Article{Title: "構成図", Slug: "architecture", ContentMarkdown: mermaidDiagramMarkdown,
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	c := env.newClient()

	for _, path := range []string{"/articles/flow", "/architecture"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			"<pre class=\"mermaid\">flowchart LR\n  A[申請] --&gt; B{承認}",
			`<script defer src="/static/js/mermaid.js?v=`,
			`data-mermaid-src="`+mermaidVendorPath+`?v=`)
		expectNoExternalMermaid(t, path, res.Body)
		if strings.Contains(res.Body, `language-mermaid`) {
			t.Errorf("%s rendered the diagram as a code block", path)
		}
	}

	res := c.get("/articles/code-only")
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "mermaid.js") || strings.Contains(res.Body, "vendor/mermaid") {
		t.Errorf("article without diagrams loads Mermaid: %.2000s", res.Body)
	}

	// トップに出したページに図があれば、トップページでも読み込む。
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontPage: "architecture"}); err != nil {
		t.Fatal(err)
	}
	res = c.get("/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<pre class="mermaid">`, `data-mermaid-src="`+mermaidVendorPath+`?v=`)

	// 同梱の本体と描画スクリプトは自分のサーバーから配信する。
	for _, p := range []string{mermaidVendorPath, "/static/js/mermaid.js"} {
		expectStatus(t, c.get(p), http.StatusOK)
	}
}

// 公開ページの CSP は、同梱の Mermaid（自分のサーバーのスクリプト）と、Mermaid が SVG に入れる <style>・data: の画像を
// 妨げない。外部の配信元・eval は許さないまま。
func TestPublicCSPAllowsOfflineMermaid(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{Title: "申請の流れ", Slug: "flow", ContentMarkdown: mermaidDiagramMarkdown,
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	res := env.newClient().get("/articles/flow")
	expectStatus(t, res, http.StatusOK)
	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "style-src 'self' 'unsafe-inline'", "img-src 'self' data:", "font-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q needed by Mermaid", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "cdn.jsdelivr.net") {
		t.Errorf("CSP %q was loosened for Mermaid", csp)
	}
}

// 静的サイトの書き出しは、図のあるページが参照する同梱の Mermaid を書き出し、_headers の CSP もそれを妨げない。
func TestExportWritesMermaidVendor(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{Title: "申請の流れ", Slug: "flow", ContentMarkdown: mermaidDiagramMarkdown,
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(out, "articles", "flow", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	expectContains(t, string(page), `<pre class="mermaid">`, `data-mermaid-src="`+mermaidVendorPath+`?v=`,
		`<script defer src="/static/js/mermaid.js?v=`)
	expectNoExternalMermaid(t, "exported page", string(page))

	for _, name := range []string{"vendor/mermaid/mermaid.min.js", "vendor/mermaid/LICENSE", "js/mermaid.js"} {
		got, err := os.ReadFile(filepath.Join(out, "static", filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("exported %s: %v", name, err)
		}
		want, err := static.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("exported %s differs from the embedded file", name)
		}
	}

	headers, err := os.ReadFile(filepath.Join(out, "_headers"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"script-src 'self'", "style-src 'self' 'unsafe-inline'"} {
		if !strings.Contains(string(headers), want) {
			t.Errorf("_headers CSP lacks %q: %s", want, headers)
		}
	}
}

// 記事の編集画面のプレビューも Mermaid の図を描く（本体は図があるときだけ mermaid.js が読み込む）。
func TestEditorPreviewLoadsMermaid(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	id := c.createArticle("図", mermaidDiagramMarkdown)
	res := c.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<script defer src="/static/js/mermaid.js?v=`, `data-mermaid-src="`+mermaidVendorPath+`?v=`,
		`id="preview"`, "<pre class=\"mermaid\">flowchart LR")
	expectNoExternalMermaid(t, "editor", res.Body)
}
