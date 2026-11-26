package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSite は GetSiteStructure・UpdateSiteStructure・ListMedia の代わり。受け取った値を覚え、決めた値を返す。
type fakeSite struct {
	mu        sync.Mutex
	structure SiteStructure
	updates   []SiteStructureUpdate
	warnings  []string
	updateErr error
	media     MediaList
	queries   []MediaQuery
}

func (f *fakeSite) config(cfg *Config) {
	cfg.GetSiteStructure = func(context.Context) (SiteStructure, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.structure, nil
	}
	cfg.UpdateSiteStructure = func(_ context.Context, upd SiteStructureUpdate) (SiteStructure, []string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.updates = append(f.updates, upd)
		if f.updateErr != nil {
			return SiteStructure{}, nil, f.updateErr
		}
		if upd.SiteMenu != nil {
			f.structure.SiteMenu = *upd.SiteMenu
		}
		if upd.SiteFooterMenu != nil {
			f.structure.SiteFooterMenu = *upd.SiteFooterMenu
		}
		if upd.FrontCards != nil {
			f.structure.FrontCards = *upd.FrontCards
		}
		return f.structure, f.warnings, nil
	}
	cfg.ListMedia = func(_ context.Context, q MediaQuery) (MediaList, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, q)
		return f.media, nil
	}
}

func (f *fakeSite) updateCount() int { return len(f.getUpdates()) }

func (f *fakeSite) getUpdates() []SiteStructureUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SiteStructureUpdate(nil), f.updates...)
}

func (f *fakeSite) getQueries() []MediaQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MediaQuery(nil), f.queries...)
}

// with は fn をロックを持って呼ぶ（テストから決めた値を差し替える）。
func (f *fakeSite) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func newSiteEnv(t *testing.T, cfg Config, f *fakeSite) *testEnv {
	t.Helper()
	f.config(&cfg)
	return newTestEnv(t, cfg)
}

func TestGetSiteContextIncludesSiteStructure(t *testing.T) {
	f := &fakeSite{structure: SiteStructure{
		SiteMenu: []SiteMenuItem{
			{Type: "page", Target: "about"},
			{Type: "group", Label: "研究", Children: []SiteMenuItem{{Type: "category", Target: "research"}}},
		},
		FrontCards: []string{"about"},
	}}
	env := newSiteEnv(t, Config{}, f)
	var got struct {
		SiteStructure *SiteStructure `json:"site_structure"`
	}
	env.toolOK(writerA.ID, "cms_get_site_context", map[string]any{}, &got)
	if got.SiteStructure == nil {
		t.Fatal("site_structure is missing")
	}
	want := f.structure
	want.SiteFooterMenu = []SiteMenuItem{} // 空でも空の配列で返す
	if !reflect.DeepEqual(*got.SiteStructure, want) {
		t.Errorf("site_structure = %+v, want %+v", *got.SiteStructure, want)
	}
	// 空のサイト構成は null ではなく空の配列で返す（AI が「未取得」と取り違えない）。
	f.with(func() { f.structure = SiteStructure{} })
	res := env.callTool(writerA.ID, "cms_get_site_context", map[string]any{})
	expectContains(t, string(res.StructuredContent), `"site_menu":[]`, `"site_footer_menu":[]`, `"front_cards":[]`)

	// 取得の仕組みが無ければ site_structure を返さない。
	plain := newTestEnv(t, Config{})
	res = plain.callTool(writerA.ID, "cms_get_site_context", map[string]any{})
	if strings.Contains(string(res.StructuredContent), "site_structure") {
		t.Errorf("site_structure returned without GetSiteStructure: %s", res.StructuredContent)
	}
}

func TestUpdateSiteStructure(t *testing.T) {
	f := &fakeSite{
		structure: SiteStructure{SiteMenu: []SiteMenuItem{{Type: "articles"}}, FrontCards: []string{"old"}},
		warnings:  []string{"トップのカードに選んだページ「missing」が見つからないか、公開されていません。このカードは表示されません"},
	}
	env := newSiteEnv(t, Config{}, f)

	// 執筆者は変更できない（呼び出し先まで届かない）。
	expectContains(t, env.toolErr(writerA.ID, "cms_update_site_structure", map[string]any{"front_cards": []string{"x"}}),
		"サイト構成を変更する権限がありません")
	if n := f.updateCount(); n != 0 {
		t.Fatalf("writer reached UpdateSiteStructure %d times", n)
	}
	// 変更する項目が無い・型の違う値・未知の項目は、保存の前に断る。
	expectContains(t, env.toolErr(editor.ID, "cms_update_site_structure", map[string]any{}), "変更する項目を指定")
	expectContains(t, env.toolErr(editor.ID, "cms_update_site_structure", map[string]any{"site_menu": "about"}), "引数が正しくありません")
	expectContains(t, env.toolErr(editor.ID, "cms_update_site_structure",
		map[string]any{"site_menu": []map[string]any{{"type": "page", "target": "about", "url": "x"}}}), "引数が正しくありません")
	expectContains(t, env.toolErr(admin.ID, "cms_update_site_structure", map[string]any{"front_page": "about"}), "引数が正しくありません")
	if n := f.updateCount(); n != 0 {
		t.Fatalf("invalid arguments reached UpdateSiteStructure %d times", n)
	}

	// 指定した項目だけを渡し、省略した項目は nil（今の値を保つ）。
	var res updateSiteStructureResult
	env.toolOK(editor.ID, "cms_update_site_structure", map[string]any{"front_cards": []string{"about", "missing"}}, &res)
	updates := f.getUpdates()
	if len(updates) != 1 || updates[0].SiteMenu != nil || updates[0].SiteFooterMenu != nil ||
		updates[0].FrontCards == nil || !reflect.DeepEqual(*updates[0].FrontCards, []string{"about", "missing"}) {
		t.Fatalf("update = %+v", updates)
	}
	if !reflect.DeepEqual(res.Updated, []string{"front_cards"}) || !reflect.DeepEqual(res.Warnings, f.warnings) ||
		!reflect.DeepEqual(res.SiteStructure.SiteMenu, []SiteMenuItem{{Type: "articles"}}) || res.Message == "" {
		t.Errorf("result = %+v", res)
	}

	// 子項目付きのメニューと、空配列での消去（フッター）。
	menu := []map[string]any{
		{"type": "group", "label": "研究室", "label_en": "Lab", "children": []map[string]any{
			{"type": "page", "target": "members"}, {"type": "url", "label": "論文", "target": "https://example.jp/papers"},
		}},
	}
	env.toolOK(admin.ID, "cms_update_site_structure", map[string]any{"site_menu": menu, "site_footer_menu": []any{}}, &res)
	upd := f.getUpdates()[1]
	if upd.FrontCards != nil || upd.SiteMenu == nil || upd.SiteFooterMenu == nil || len(*upd.SiteFooterMenu) != 0 {
		t.Fatalf("second update = %+v", upd)
	}
	wantMenu := []SiteMenuItem{{Type: "group", Label: "研究室", LabelEn: "Lab", Children: []SiteMenuItem{
		{Type: "page", Target: "members"}, {Type: "url", Label: "論文", Target: "https://example.jp/papers"},
	}}}
	if !reflect.DeepEqual(*upd.SiteMenu, wantMenu) {
		t.Errorf("menu = %+v, want %+v", *upd.SiteMenu, wantMenu)
	}
	if !reflect.DeepEqual(res.Updated, []string{"site_menu", "site_footer_menu"}) {
		t.Errorf("updated = %v", res.Updated)
	}

	// 検査で断られた理由（toolError）はそのまま返し、内部エラーは定型文にする。
	f.with(func() {
		f.updateErr = NewToolError("メニューの1行目: 子項目の下にはさらに項目を入れられません（1階層まで）")
	})
	expectContains(t, env.toolErr(editor.ID, "cms_update_site_structure", map[string]any{"site_menu": menu}), "1階層まで")
	f.with(func() { f.updateErr = errors.New("database is locked") })
	msg := env.toolErr(editor.ID, "cms_update_site_structure", map[string]any{"site_menu": menu})
	if strings.Contains(msg, "database") {
		t.Errorf("internal error leaked: %q", msg)
	}
}

func TestUpdateSiteStructureUnavailable(t *testing.T) {
	// 公開サーバー（管理画面の無いサーバー）では、PC からの反映で上書きされるため変更させない。
	f := &fakeSite{}
	public := newSiteEnv(t, Config{PublicOnly: true}, f)
	expectContains(t, public.toolErr(admin.ID, "cms_update_site_structure", map[string]any{"front_cards": []string{}}),
		"公開サーバーではサイト構成を変更できません", "PC の sorus")
	if n := f.updateCount(); n != 0 {
		t.Fatalf("public-only reached UpdateSiteStructure %d times", n)
	}
	// 読み取りは公開サーバーでもできる。
	var got map[string]any
	public.toolOK(writerA.ID, "cms_get_site_context", map[string]any{}, &got)
	if _, ok := got["site_structure"]; !ok {
		t.Errorf("public-only site context has no site_structure: %v", got)
	}

	// 保存の仕組みが無ければ使えない。
	plain := newTestEnv(t, Config{})
	expectContains(t, plain.toolErr(admin.ID, "cms_update_site_structure", map[string]any{"front_cards": []string{}}), "利用できません")
}

func TestListMedia(t *testing.T) {
	created := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	f := &fakeSite{media: MediaList{Total: 3, Items: []MediaItem{
		{ID: "m1", Filename: "campus.jpg", URL: "/uploads/campus-large.webp", Alt: "キャンパス", Tags: []string{"風景", "2026年"},
			Width: 1600, Height: 900, MimeType: "image/webp", CreatedAt: created, Markdown: "![キャンパス](/uploads/campus-large.webp)"},
		{ID: "m2", Filename: "lab.png", URL: "/uploads/lab.webp", MimeType: "image/webp", Markdown: "![lab](/uploads/lab.webp)"},
	}}}
	env := newSiteEnv(t, Config{}, f)

	var res listMediaResult
	env.toolOK(writerA.ID, "cms_list_media", map[string]any{"q": "  キャンパス 風景 ", "tag": " 風景 "}, &res)
	if q := f.getQueries(); len(q) != 1 || q[0] != (MediaQuery{Query: "キャンパス 風景", Tag: "風景", Limit: defaultListLimit}) {
		t.Fatalf("query = %+v", q)
	}
	if res.Total != 3 || len(res.Items) != 2 || res.NextOffset == nil || *res.NextOffset != 2 || res.Limit != defaultListLimit {
		t.Fatalf("result = %+v", res)
	}
	if got := res.Items[0]; got.ID != "m1" || got.Alt != "キャンパス" || got.Width != 1600 || !got.CreatedAt.Equal(created) ||
		got.Markdown != "![キャンパス](/uploads/campus-large.webp)" {
		t.Errorf("item = %+v", got)
	}
	// タグの無い画像も tags は空の配列で返す。
	raw := env.callTool(writerA.ID, "cms_list_media", map[string]any{"limit": 2, "offset": 1})
	var decoded struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw.StructuredContent, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Items[1]["tags"]) != "[]" {
		t.Errorf("tags of untagged image = %s", decoded.Items[1]["tags"])
	}
	if q := f.getQueries()[1]; q.Limit != 2 || q.Offset != 1 {
		t.Errorf("paged query = %+v", q)
	}

	// 範囲外の値は呼び出し先に渡さずに断る。
	for _, args := range []map[string]any{
		{"limit": maxListLimit + 1}, {"offset": -1}, {"q": strings.Repeat("あ", maxQueryRunes+1)},
		{"tag": strings.Repeat("あ", maxMediaTagRunes+1)}, {"query": "x"},
	} {
		env.toolErr(writerA.ID, "cms_list_media", args)
	}
	if q := f.getQueries(); len(q) != 2 {
		t.Errorf("invalid arguments reached ListMedia: %+v", q)
	}
	// 最後のページでは next_offset を返さない。
	f.with(func() { f.media = MediaList{Total: 1, Items: []MediaItem{{ID: "m1"}}} })
	res = listMediaResult{}
	env.toolOK(writerA.ID, "cms_list_media", map[string]any{}, &res)
	if res.NextOffset != nil {
		t.Errorf("next_offset on last page = %d", *res.NextOffset)
	}

	plain := newTestEnv(t, Config{})
	expectContains(t, plain.toolErr(writerA.ID, "cms_list_media", map[string]any{}), "利用できません")
}
