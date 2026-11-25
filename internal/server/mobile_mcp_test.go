package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sorus/internal/article"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

// getWithHost は Host ヘッダーを host にして GET する（デスクトップモードの別ポートのサイト・LAN のアドレスなど、
// 実際に届く要求の Host を再現する）。
func (c *client) getWithHost(path, host string) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Accept", "text/html")
	return c.do(req)
}

// 再現テスト（Issue #2）: 公開サイトの URL（BaseURL）を設定していても、MCP 連携の画面に出す接続先は、
// 管理画面を開いているこの sorus（要求が届いたホスト・ポート）にする。
func TestMCPEndpointUsesRequestOrigin(t *testing.T) {
	const public = "https://media.example.jp"
	env := newTestEnvWith(t, Config{BaseURL: public}, nil)
	c := env.loggedIn(studentA)
	for _, host := range []string{"127.0.0.1:8780", "127.0.0.1:8781", "localhost:8080"} {
		res := c.getWithHost(MCPSettingsPath, host)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "http://"+host+"/api/mcp")
		expectNotContains(t, res.Body, public+"/api/mcp")
	}

	// 公開サイトの URL が未設定でも同じ。
	plain := newTestEnv(t)
	res := plain.loggedIn(studentA).get(MCPSettingsPath)
	expectContains(t, res.Body, plain.srv.URL+"/api/mcp")
}

// MCP の接続先のスキームは、TLS か、信頼したリバースプロキシ（Config.TrustedProxies）が付けた X-Forwarded-Proto だけで
// 決める。信頼していない接続元（httptest.NewRequest の 192.0.2.1）が書いた X-Forwarded-Proto は信じない。
func TestMCPEndpointURL(t *testing.T) {
	e := echo.New()
	srv := &Server{cfg: Config{MCPAPIPath: "/api/mcp"}}
	proxied := &Server{cfg: Config{MCPAPIPath: "/api/mcp"}, proxies: testProxies(t, "192.0.2.1")}
	for _, tc := range []struct {
		srv               *Server
		host, proto, want string
	}{
		{srv, "127.0.0.1:8781", "", "http://127.0.0.1:8781/api/mcp"},
		{srv, "cms.example.jp", "https", "http://cms.example.jp/api/mcp"},
		{proxied, "cms.example.jp", "https", "https://cms.example.jp/api/mcp"},
		{proxied, "cms.example.jp", "", "http://cms.example.jp/api/mcp"},
		{srv, `evil"<host>`, "", "http://evilhost/api/mcp"},
		{srv, "", "", "http://localhost/api/mcp"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/admin/settings/mcp", nil)
		req.Host = tc.host
		if tc.proto != "" {
			req.Header.Set(echo.HeaderXForwardedProto, tc.proto)
		}
		if got := tc.srv.mcpEndpointURL(e.NewContext(req, httptest.NewRecorder())); got != tc.want {
			t.Errorf("host %q, proto %q, proxied %v: %q, want %q", tc.host, tc.proto, tc.srv == proxied, got, tc.want)
		}
	}
	// TLS で直接届いた要求は https。
	req := httptest.NewRequest(http.MethodGet, "https://cms.example.jp/admin/settings/mcp", nil)
	if got := srv.mcpEndpointURL(e.NewContext(req, httptest.NewRecorder())); got != "https://cms.example.jp/api/mcp" {
		t.Errorf("tls: %q", got)
	}
}

// Issue #4: スマートフォン向けの記事エディタ（編集・プレビューのタブ、音声入力向けの本文欄、横にスワイプできる
// AI ツールバー、画面下の操作バー）。
func TestEditorMobileLayout(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	c := env.loggedIn(studentA)
	id := c.createArticle("スマホで書く", "本文")
	for _, path := range []string{"/admin/articles/new", "/admin/articles/" + id + "/edit"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		body := res.Body
		expectContains(t, body,
			`id="editor-tabs"`, `data-editor-tab="edit"`, `data-editor-tab="preview"`, `aria-selected="true"`,
			`id="editor-pane-edit" data-editor-pane="edit"`,
			`enterkeyhint="enter"`, `autocapitalize="sentences"`,
			`id="mobile-action-bar"`, `data-proxy-for="save-button"`, `data-proxy-for="publish-button"`, `id="save-button"`)
		// タブは lg 未満だけ。プレビューは lg 未満で隠すだけ（lg 以上では常に並べて表示し、編集欄は消えない）。
		tabs := tagWithID(t, body, "editor-tabs")
		if !strings.Contains(tabs, "lg:hidden") {
			t.Errorf("tabs must be hidden on lg screens: %s", tabs)
		}
		pane := tagWithID(t, body, "editor-pane-preview")
		if !strings.Contains(pane, "max-lg:hidden") || strings.Contains(pane, " hidden") {
			t.Errorf("preview pane = %s", pane)
		}
		edit := tagWithID(t, body, "editor-pane-edit")
		if strings.Contains(edit, "hidden") {
			t.Errorf("edit pane must be visible by default: %s", edit)
		}
		ta := tagWithID(t, body, "content_markdown")
		if !strings.Contains(ta, " text-base ") || !strings.Contains(ta, "lg:text-sm") {
			t.Errorf("textarea must use 16px on small screens: %s", ta)
		}
		// AI のボタンは3つにまとめたので、狭い画面でも横スクロールにせず折り返す（スクロール枠だとメニューが切れる）。
		toolbar := tagWithID(t, body, "ai-toolbar")
		if !strings.Contains(toolbar, "flex-wrap") || strings.Contains(toolbar, "overflow-x-auto") {
			t.Errorf("toolbar must wrap without a scroll container: %s", toolbar)
		}
		// 「AI で下書き」→「文章を整える」→「仕上げ・告知」の順に並べる。
		draft := strings.Index(body, `data-ai-action="draft"`)
		proof := strings.Index(body, `data-ai-action="proofread"`)
		titles := strings.Index(body, `data-ai-action="suggest-titles"`)
		if !(draft >= 0 && draft < proof && proof < titles) {
			t.Errorf("toolbar order: draft=%d proofread=%d titles=%d", draft, proof, titles)
		}
		bar := tagWithID(t, body, "mobile-action-bar")
		if !strings.Contains(bar, "fixed") || !strings.Contains(bar, "lg:hidden") {
			t.Errorf("action bar = %s", bar)
		}
	}
}

// 翻訳編集画面（+EN）のスマートフォン表示（admin_ux_phase5_spec.md §1.5）: 原文バーと AI ツールバーは折り返し、
// 「選択範囲を英語に翻訳・推敲」→「文章を整える」→「仕上げ・告知」の順。原文の枠は狭い画面でも編集欄と同じ高さで、
// 文字は 16px（iOS で拡大されない大きさ）。編集・プレビューのタブはそのまま使える。
func TestEditorMobileTranslationLayout(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "スマホ", "mobile", "")
		seedArticle(t, db, article.Article{ID: "art-ja-mobile", Title: "スマホの原文", Slug: "mobile-ja", CategoryID: catID,
			ContentMarkdown: "原文の本文", Status: article.StatusPublished, Lang: "ja", TranslationGroupID: "grp-mobile"})
	})
	enableMultilingual(t, env.db)
	c := env.loggedIn(chief)
	res := c.get("/admin/articles/new?source_article_id=art-ja-mobile&lang=en")
	expectStatus(t, res, http.StatusOK)
	body := res.Body
	expectContains(t, body, `id="editor-tabs"`, `data-editor-tab="preview"`, `id="mobile-action-bar"`)
	for _, id := range []string{"source-article-banner", "ai-toolbar"} {
		if tag := tagWithID(t, body, id); !strings.Contains(tag, "flex-wrap") || strings.Contains(tag, "overflow-x-auto") {
			t.Errorf("%s must wrap on small screens: %s", id, tag)
		}
	}
	partial := strings.Index(body, `data-ai-partial`)
	proof := strings.Index(body, `data-ai-action="proofread"`)
	titles := strings.Index(body, `data-ai-action="suggest-titles"`)
	if !(partial >= 0 && partial < proof && proof < titles) {
		t.Errorf("translation toolbar order: partial=%d proofread=%d titles=%d", partial, proof, titles)
	}
	src := tagWithID(t, body, "source-body-scroll")
	for _, want := range []string{"h-[60vh]", "lg:h-[65vh]", "text-base", "lg:text-sm", "overflow-y-auto"} {
		if !strings.Contains(src, want) {
			t.Errorf("source body should have %q: %s", want, src)
		}
	}
}

// tagWithID は body の id="id" を持つ開始タグを返す。
func tagWithID(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("id %q not found", id)
	}
	start := strings.LastIndex(body[:i], "<")
	end := strings.Index(body[i:], ">")
	return body[start : i+end+1]
}
