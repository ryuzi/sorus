package static

import (
	"regexp"
	"strings"
	"testing"
)

// TestMermaidVendorIsEmbeddedOffline は、Mermaid 本体（公式の IIFE ビルド）と MIT ライセンスが埋め込まれ、
// 本体が外部の配信元からの読み込みや eval（公開ページの CSP が許さない）に頼らないことを確かめる。
func TestMermaidVendorIsEmbeddedOffline(t *testing.T) {
	data, err := FS.ReadFile("vendor/mermaid/mermaid.min.js")
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(data)
	// IIFE ビルドは window.mermaid（globalThis）に本体を置く。mermaid.js はこれを使う。
	if !strings.Contains(bundle, `globalThis["mermaid"]`) {
		t.Error("mermaid.min.js is not the IIFE build that defines globalThis.mermaid")
	}
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`cdn\.jsdelivr\.net|unpkg\.com|cdnjs\.cloudflare\.com|fonts\.googleapis\.com`),
		regexp.MustCompile(`\bimportScripts\(`),
		regexp.MustCompile(`[^.\w]eval\(`),
		regexp.MustCompile(`new Function\(`),
	} {
		if loc := bad.FindString(bundle); loc != "" {
			t.Errorf("mermaid.min.js contains %q", loc)
		}
	}
	license, err := FS.ReadFile("vendor/mermaid/LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(license), "The MIT License") {
		t.Error("vendor/mermaid/LICENSE is not the MIT license")
	}
}

// TestMermaidScriptFeatures は、図の描画スクリプトが所定の動作を備えることを、mermaid.js の中身から確かめる。
// 描画そのものは実ブラウザで確認する。
func TestMermaidScriptFeatures(t *testing.T) {
	data, err := FS.ReadFile("js/mermaid.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, c := range []struct{ feature, code string }{
		{"描画対象は pre.mermaid", `'pre.mermaid'`},
		{"本体は data-mermaid-src の URL（同梱）から読み込む", `getAttribute('data-mermaid-src')`},
		{"自動の描画（.mermaid の走査）は使わない", `startOnLoad: false`},
		{"図の文字をサニタイズし、click のスクリプトを許さない", `securityLevel: 'strict'`},
		{"OS の暗い配色を見る", `'(prefers-color-scheme: dark)'`},
		{"暗い配色では dark のテーマ", `return 'dark'`},
		{"明るい配色では default のテーマ", `return 'default'`},
		{"ページの配色（meta color-scheme）を見る", `meta[name="color-scheme"]`},
		{"配色が変わったら描き直す", `'change'`},
		{"定義の誤りはエラー図を出さず定義を見せる", `suppressErrorRendering: true`},
		{"管理画面のプレビューから呼ぶ", `window.cmsRenderMermaid`},
	} {
		if !strings.Contains(script, c.code) {
			t.Errorf("mermaid.js lacks %s (%s)", c.feature, c.code)
		}
	}
	for _, bad := range []string{"http://", "https://", "eval("} {
		if strings.Contains(script, bad) {
			t.Errorf("mermaid.js contains %q", bad)
		}
	}
}

// TestPublicCodeCopySkipsMermaid は、コードブロックのコピーボタンが Mermaid の図（pre.mermaid）を対象にしないことを確かめる。
func TestPublicCodeCopySkipsMermaid(t *testing.T) {
	data, err := FS.ReadFile("js/public.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `document.querySelectorAll(".prose pre:not(.mermaid)")`) {
		t.Error("public.js wraps Mermaid diagrams (pre.mermaid) as code blocks with a copy button")
	}
}
