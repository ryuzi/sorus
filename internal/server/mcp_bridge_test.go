package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/mcp"
	"sorus/internal/version"
)

// mcpToolJSON はローカルの MCP サーバーのツールを呼び、ツールのエラーでなければ structuredContent を out に読む。
// ツールのエラーなら本文を返す（呼び出し側が wantErr で確かめる）。
func (e *testEnv) mcpToolJSON(token, name string, args map[string]any, out any) (isError bool, text string) {
	e.t.Helper()
	_, response := e.mcpCall(token, "tools/call", map[string]any{"name": name, "arguments": args})
	if response["error"] != nil {
		e.t.Fatalf("%s: JSON-RPC error %v", name, response["error"])
	}
	result, _ := response["result"].(map[string]any)
	isError, _ = result["isError"].(bool)
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	if !isError && out != nil {
		raw, err := json.Marshal(result["structuredContent"])
		if err != nil {
			e.t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s: decode %s: %v", name, raw, err)
		}
	}
	return isError, text
}

// MCP の cms_get_site_context・cms_update_site_structure は、管理画面の「サイト構成」と同じ設定を読み書きし、同じ検査を通す。
func TestMCPSiteStructureUsesAdminValidationAndStorage(t *testing.T) {
	env := newTestEnv(t)
	seedArticle(t, env.db, article.Article{Title: "研究室について", Slug: "about", Kind: article.KindPage, Status: article.StatusPublished})
	ctx := context.Background()
	if err := env.server.saveSiteStructure(ctx, SiteStructure{
		Menu:            []MenuItem{{Type: MenuItemPage, Target: "about"}, {Type: MenuItemArticles, Label: "ニュース"}},
		FrontPage:       "about",
		TopGalleryTag:   "風景",
		TopGalleryLimit: 12,
	}); err != nil {
		t.Fatal(err)
	}
	editorToken := issueMCPToken(env.loggedIn(chief), "editor", "30")
	writerToken := issueMCPToken(env.loggedIn(studentA), "writer", "30")

	var ctxRes struct {
		SiteStructure mcp.SiteStructure `json:"site_structure"`
	}
	if isErr, text := env.mcpToolJSON(writerToken, "cms_get_site_context", map[string]any{}, &ctxRes); isErr {
		t.Fatalf("cms_get_site_context: %s", text)
	}
	wantMenu := []mcp.SiteMenuItem{{Type: "page", Target: "about"}, {Type: "articles", Label: "ニュース"}}
	if !reflect.DeepEqual(ctxRes.SiteStructure.SiteMenu, wantMenu) || len(ctxRes.SiteStructure.SiteFooterMenu) != 0 ||
		len(ctxRes.SiteStructure.FrontCards) != 0 {
		t.Fatalf("site_structure = %+v", ctxRes.SiteStructure)
	}

	// 執筆者は変更できない。
	if isErr, text := env.mcpToolJSON(writerToken, "cms_update_site_structure", map[string]any{"front_cards": []string{"about"}}, nil); !isErr ||
		!strings.Contains(text, "権限がありません") {
		t.Fatalf("writer update = %v %q", isErr, text)
	}

	// フッターとカードだけを変え、メニュー・トップページ・ギャラリーの設定は保つ。カードの空白と重なりは管理画面と同じく整える。
	var upd struct {
		SiteStructure mcp.SiteStructure `json:"site_structure"`
		Warnings      []string          `json:"warnings"`
	}
	footer := []map[string]any{{"type": "group", "label": "研究室", "children": []map[string]any{
		{"type": "page", "target": "about"}, {"type": "category", "target": "no-such-category"},
	}}}
	if isErr, text := env.mcpToolJSON(editorToken, "cms_update_site_structure", map[string]any{
		"site_footer_menu": footer, "front_cards": []string{" about ", "about", "missing-page"},
	}, &upd); isErr {
		t.Fatalf("cms_update_site_structure: %s", text)
	}
	saved := env.server.loadSiteStructure(ctx)
	if !reflect.DeepEqual(saved.Menu, []MenuItem{{Type: MenuItemPage, Target: "about"}, {Type: MenuItemArticles, Label: "ニュース"}}) ||
		saved.FrontPage != "about" || saved.TopGalleryTag != "風景" || saved.TopGalleryLimit != 12 {
		t.Errorf("untouched settings changed: %+v", saved)
	}
	if !reflect.DeepEqual(saved.FrontCards, []string{"about", "missing-page"}) {
		t.Errorf("front cards = %v", saved.FrontCards)
	}
	wantFooter := []MenuItem{{Type: MenuItemGroup, Label: "研究室", Children: []MenuItem{
		{Type: MenuItemPage, Target: "about"}, {Type: MenuItemCategory, Target: "no-such-category"},
	}}}
	if !reflect.DeepEqual(saved.FooterMenu, wantFooter) {
		t.Errorf("footer = %+v", saved.FooterMenu)
	}
	if !reflect.DeepEqual(upd.SiteStructure, toMCPSiteStructure(saved)) {
		t.Errorf("returned structure = %+v, saved %+v", upd.SiteStructure, saved)
	}
	// 参照先の無いカテゴリ・ページは保存したうえで、管理画面と同じ警告を返す。
	if len(upd.Warnings) != 2 || !strings.Contains(strings.Join(upd.Warnings, "\n"), "no-such-category") ||
		!strings.Contains(strings.Join(upd.Warnings, "\n"), "missing-page") {
		t.Errorf("warnings = %v", upd.Warnings)
	}

	// 管理画面と同じ検査で断り、何も保存しない（件数・階層・種類・URL・見出しのみの項目）。
	tooMany := make([]map[string]any, maxMenuItems+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"type": "articles"}
	}
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"too many items", map[string]any{"site_menu": tooMany}},
		{"nested too deep", map[string]any{"site_menu": []map[string]any{{"type": "group", "label": "親", "children": []map[string]any{
			{"type": "page", "target": "about", "children": []map[string]any{{"type": "articles"}}},
		}}}}},
		{"unknown type", map[string]any{"site_menu": []map[string]any{{"type": "script", "target": "x"}}}},
		{"javascript url", map[string]any{"site_menu": []map[string]any{{"type": "url", "label": "x", "target": "javascript:alert(1)"}}}},
		{"empty group", map[string]any{"site_footer_menu": []map[string]any{{"type": "group", "label": "空"}}}},
		{"too many cards", map[string]any{"front_cards": []string{"a", "b", "c", "d", "e", "f", "g"}}},
		{"empty card", map[string]any{"front_cards": []string{""}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			isErr, text := env.mcpToolJSON(editorToken, "cms_update_site_structure", c.args, nil)
			if !isErr || text == "" || strings.Contains(text, "内部エラー") {
				t.Fatalf("update = %v %q, want a validation message", isErr, text)
			}
			if after := env.server.loadSiteStructure(ctx); !reflect.DeepEqual(after, saved) {
				t.Errorf("structure changed by rejected update:\n%+v\n%+v", after, saved)
			}
		})
	}
	// 階層の誤りは管理画面と同じ文言（validateSiteStructure）で返す。
	deep := SiteStructure{Menu: []MenuItem{{Type: MenuItemGroup, Label: "親", Children: []MenuItem{
		{Type: MenuItemPage, Target: "about", Children: []MenuItem{{Type: MenuItemArticles}}},
	}}}}
	_, text := env.mcpToolJSON(editorToken, "cms_update_site_structure", map[string]any{"site_menu": []map[string]any{{"type": "group", "label": "親",
		"children": []map[string]any{{"type": "page", "target": "about", "children": []map[string]any{{"type": "articles"}}}}}}}, nil)
	if want := validateSiteStructure(deep).Error(); text != want {
		t.Errorf("depth error = %q, want %q", text, want)
	}
}

// 公開サーバー（--public-only）では、PC から反映するサイト構成を MCP で変えさせない。
func TestMCPSiteStructureReadOnlyOnPublicServer(t *testing.T) {
	env := newTestEnvWith(t, Config{PublicOnly: true}, nil)
	tok, _, err := mcp.IssueToken(context.Background(), env.db, admin.ID, "public", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, isErr, text := env.mcpToolAt(env.server.cfg.MCPAPIPath, tok, "cms_update_site_structure", map[string]any{"front_cards": []string{}})
	if !isErr || !strings.Contains(text, "公開サーバーではサイト構成を変更できません") {
		t.Fatalf("public-only update = %v %q", isErr, text)
	}
	_, isErr, text = env.mcpToolAt(env.server.cfg.MCPAPIPath, tok, "cms_get_site_context", map[string]any{})
	if isErr || !strings.Contains(text, `"site_structure"`) {
		t.Fatalf("public-only site context = %v %q", isErr, text)
	}
}

// MCP でサイト構成を保存したら、管理画面で保存したときと同じく、公開サーバーへ未反映として記録する。
func TestMCPSiteStructureMarksRemoteDirty(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("root", auth.RoleAdmin)
	resetRemoteDirty(t, env.local)
	token := issueMCPToken(env.local.loggedIn(admin), "admin", "30")
	if isErr, text := env.local.mcpToolJSON(token, "cms_update_site_structure", map[string]any{"site_menu": []map[string]any{{"type": "articles"}}}, nil); isErr {
		t.Fatalf("update: %s", text)
	}
	if got := env.local.server.remoteDirty(); !slices.Equal(got, []string{remoteDirtyStructure}) {
		t.Errorf("remote dirty = %v, want [%s]", got, remoteDirtyStructure)
	}
}

// cms_list_media はメディアライブラリと同じ検索・URL・貼り付け用の Markdown を返す。
func TestMCPListMediaUsesLibrary(t *testing.T) {
	env := newTestEnv(t)
	env.insertMedia("m-gate", "gate.jpg", "正門", "春の正門", "風景, 2026年")
	env.insertMedia("m-lab", "lab.png", "", "", "")
	env.insertMedia("m-hall", "hall.jpg", "講堂", "", "建物")
	token := issueMCPToken(env.loggedIn(studentA), "writer", "30")

	var all struct {
		Items      []mcp.MediaItem `json:"items"`
		Total      int             `json:"total"`
		NextOffset *int            `json:"next_offset"`
	}
	if isErr, text := env.mcpToolJSON(token, "cms_list_media", map[string]any{"limit": 2}, &all); isErr {
		t.Fatalf("cms_list_media: %s", text)
	}
	if all.Total != 3 || len(all.Items) != 2 || all.NextOffset == nil || *all.NextOffset != 2 {
		t.Fatalf("first page = %+v", all)
	}
	var rest struct {
		Items []mcp.MediaItem `json:"items"`
	}
	env.mcpToolJSON(token, "cms_list_media", map[string]any{"limit": 2, "offset": 2}, &rest)
	ids := []string{all.Items[0].ID, all.Items[1].ID}
	if len(rest.Items) != 1 || slices.Contains(ids, rest.Items[0].ID) {
		t.Fatalf("second page = %+v (first %v)", rest.Items, ids)
	}

	var hit struct {
		Items []mcp.MediaItem `json:"items"`
		Total int             `json:"total"`
	}
	env.mcpToolJSON(token, "cms_list_media", map[string]any{"q": "春", "tag": "風景"}, &hit)
	if hit.Total != 1 || len(hit.Items) != 1 {
		t.Fatalf("search = %+v", hit)
	}
	rec, err := env.server.getMedia("m-gate")
	if err != nil {
		t.Fatal(err)
	}
	want := newMediaItem(rec)
	got := hit.Items[0]
	if got.ID != "m-gate" || got.URL != want.URL || got.ThumbnailURL != want.ThumbnailURL || got.Markdown != want.Markdown ||
		got.Alt != "正門" || got.Caption != "春の正門" || !slices.Equal(got.Tags, []string{"風景", "2026年"}) ||
		got.Width != 100 || got.Height != 80 || got.MimeType != "image/webp" || got.CreatedAt.IsZero() {
		t.Errorf("item = %+v, want URL %q markdown %q", got, want.URL, want.Markdown)
	}
	if !strings.HasPrefix(got.URL, UploadURLPrefix) || strings.Contains(got.URL, env.uploadDir) {
		t.Errorf("URL %q is not a public upload path", got.URL)
	}
	// タグの完全一致（"風" では "風景" に一致しない）。
	env.mcpToolJSON(token, "cms_list_media", map[string]any{"tag": "風"}, &hit)
	if hit.Total != 0 {
		t.Errorf("partial tag matched: %+v", hit)
	}
}

// MCP で公開中の記事を下書きに戻したら、管理画面で戻したときと同じく静的サイトへ反映し、記事のページを消す。
// 下書きに戻した記事はゴミ箱へ移せる。
func TestMCPUnpublishReflectsAndDeleteMovesToTrash(t *testing.T) {
	env := newTestEnv(t)
	out := configureFolderReflect(t, env, 10*time.Millisecond)
	id := seedArticle(t, env.db, article.Article{Title: "MCP取り下げ", Slug: "mcp-unpublish", ContentMarkdown: "取り下げる本文",
		AuthorID: studentA.ID, Status: article.StatusReview})
	token := issueMCPToken(env.loggedIn(chief), "editor", "30")
	if isErr, text := env.mcpToolJSON(token, "cms_approve_and_publish", map[string]any{"article_id": id}, nil); isErr {
		t.Fatalf("approve: %s", text)
	}
	if r := waitReflectDone(t, env.server); !r.OK {
		t.Fatalf("reflect after publish = %+v", r)
	}
	page := filepath.Join(out, "articles", "mcp-unpublish", "index.html")
	if _, err := os.Stat(page); err != nil {
		t.Fatalf("published page not reflected: %v", err)
	}

	var res struct {
		PreviousStatus string `json:"previous_status"`
		Warning        string `json:"warning"`
	}
	if isErr, text := env.mcpToolJSON(token, "cms_unpublish_article", map[string]any{"article_id": id}, &res); isErr {
		t.Fatalf("unpublish: %s", text)
	}
	if res.PreviousStatus != "published" || res.Warning != "" || env.mustGetArticle(id).Status != article.StatusDraft {
		t.Fatalf("unpublish result = %+v, status %s", res, env.mustGetArticle(id).Status)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(page); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unpublished page is still on the static site")
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitReflectDone(t, env.server)

	if isErr, text := env.mcpToolJSON(token, "cms_delete_article", map[string]any{"article_id": id}, nil); isErr {
		t.Fatalf("delete: %s", text)
	}
	if got := env.mustGetArticle(id).Status; got != article.StatusDeleted {
		t.Errorf("status after delete = %s", got)
	}
}

// 公開サーバーに直接公開できる PC では、MCP で下書きに戻した記事を公開サーバーからも取り下げる（管理画面と同じ）。
func TestMCPUnpublishWithdrawsFromPublicServer(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("editor", auth.RoleEditor)
	ed := env.local.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("MCPで取り下げ", "mcp-remote-unpublish", "publish")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished {
		t.Fatalf("remote initial status = %q", a.Status)
	}
	token := issueMCPToken(ed, "editor", "30")
	var res struct {
		Warning string `json:"warning"`
	}
	if isErr, text := env.local.mcpToolJSON(token, "cms_unpublish_article", map[string]any{"article_id": id}, &res); isErr || res.Warning != "" {
		t.Fatalf("unpublish = %v %q warning %q", isErr, text, res.Warning)
	}
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusDraft {
		t.Fatalf("remote status after MCP unpublish = %q", a.Status)
	}
}

// CMS に組み込んだ MCP サーバーは、initialize の serverInfo.version にリリース版（version.Version）を返す。
// mcp パッケージの既定値（Config.Version を省略したときの値）ではないことを、埋め込みを模した版で確かめる。
func TestMCPInitializeReturnsReleaseVersion(t *testing.T) {
	prev := version.Version
	version.Version = "v9.8.7-test"
	t.Cleanup(func() { version.Version = prev })

	env := newTestEnv(t)
	tok, _, err := mcp.IssueToken(context.Background(), env.db, admin.ID, "version", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, out := env.mcpCall(tok, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1.0"},
	})
	result, _ := out["result"].(map[string]any)
	info, _ := result["serverInfo"].(map[string]any)
	if got := info["version"]; got != "v9.8.7-test" {
		t.Fatalf("serverInfo.version = %v, want %q (response %v)", got, "v9.8.7-test", out)
	}
}
