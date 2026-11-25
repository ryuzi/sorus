package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/mcp"
	"sorus/internal/rag"
)

// setAIEnabled は基本設定の「AI機能を使う」（settings の ai_enabled）を保存する。
func (e *testEnv) setAIEnabled(on bool) {
	e.t.Helper()
	if _, err := e.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		settingAIEnabled, formatAIEnabled(on)); err != nil {
		e.t.Fatalf("set ai_enabled: %v", err)
	}
}

// calls は fakeAI が受けた依頼（本文など）の数を数えるために、記録した文字列の写しを返す。
func (f *fakeAI) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

// rawRequest は Cookie も CSRF トークンも付けずに、外部の AI アプリと同じように要求を送る。
func (e *testEnv) rawRequest(method, path, contentType, body string, header map[string]string) int {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// SORUS_AI の off・false・0（大文字・小文字・前後の空白は問わない）だけが強制 OFF。それ以外（未設定を含む）は強制しない。
func TestAIEnvForcedOffValue(t *testing.T) {
	for _, v := range []string{"off", "OFF", "Off", "false", "FALSE", "0", " off "} {
		if !aiEnvForcedOffValue(v) {
			t.Errorf("SORUS_AI=%q should force AI off", v)
		}
	}
	for _, v := range []string{"", "on", "1", "true", "yes", "offline"} {
		if aiEnvForcedOffValue(v) {
			t.Errorf("SORUS_AI=%q should not force AI off", v)
		}
	}
}

// ai_enabled を保存していないサイトは、今までどおり AI 機能が ON。
func TestAIEnabledByDefault(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	if !env.server.aiEnabled() {
		t.Fatal("AI features should be on when ai_enabled is not saved")
	}
	env.setAIEnabled(false)
	if env.server.aiEnabled() {
		t.Fatal("AI features should be off after saving ai_enabled=0")
	}
	env.setAIEnabled(true)
	if !env.server.aiEnabled() {
		t.Fatal("AI features should be on after saving ai_enabled=1")
	}
}

// AI 機能を OFF にしたサイトでは、AI が関わるルート（記事エディタの AI・翻訳・画像の AI 自動生成・RAG 検索・AI 設定・
// 変更案・MCP の設定・MCP・OAuth）がすべて存在しないページと同じ 404 になり、AI のプロバイダーを一度も呼ばない。
// ON のときは同じ要求が 404 にならない（ルートが確かにあることを確かめる）。
func TestAIDisabledRoutesReturnNotFound(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	fake := &fakeAI{}
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir(), AI: fake}, nil)
	adm := env.loggedIn(admin)
	articleID := adm.createArticle("AIの記事", "AIのテスト本文。")
	env.insertMedia("m-ai-switch", "photo.png", "", "", "日常")
	token := issueMCPToken(adm, "AIアプリ", "30")

	text := url.Values{"text": {"本文の文章です。"}, "title": {"題名"}}
	adminRoutes := []struct {
		method, path string
		form         url.Values
		// onNot404 は、AI 機能が ON のときに 404 以外になることを確かめるか（存在しない ID を指定する要求は確かめない）。
		onNot404 bool
	}{
		{http.MethodPost, "/admin/ai/proofread", text, true},
		{http.MethodPost, "/admin/ai/suggest-titles", text, true},
		{http.MethodPost, "/admin/ai/summarize", text, true},
		{http.MethodPost, "/admin/ai/draft", url.Values{"topic": {"題名"}, "notes": {"メモ"}}, true},
		{http.MethodPost, "/admin/ai/fact-check", text, true},
		{http.MethodPost, "/admin/ai/academic", url.Values{"text": {"本文の文章です。"}, "action": {"simplify"}}, true},
		{http.MethodPost, "/admin/ai/social-post", text, true},
		{http.MethodPost, "/admin/articles/translate", url.Values{"article_id": {articleID}, "target_lang": {"en"}}, true},
		{http.MethodPost, "/admin/api/media/m-ai-switch/analyze", nil, true},
		{http.MethodGet, "/admin/api/rag/search?q=test", nil, true},
		{http.MethodGet, "/admin/settings/ai", nil, true},
		{http.MethodPost, "/admin/settings/ai", url.Values{"writing_style": {"standard"}}, true},
		{http.MethodPost, "/admin/settings/ai/test", nil, false},
		{http.MethodPost, "/admin/settings/ai/reset", nil, true},
		{http.MethodPost, "/admin/articles/" + articleID + "/proposal/discard", url.Values{"proposal_id": {"none"}}, true},
		{http.MethodGet, "/admin/settings/mcp", nil, true},
		{http.MethodPost, "/admin/settings/mcp", url.Values{"name": {"別のAIアプリ"}, "expires": {"30"}}, true},
		{http.MethodPost, "/admin/settings/mcp/no-such-token/revoke", nil, false},
		{http.MethodPost, "/admin/settings/mcp/oauth/no-such-app/revoke", nil, false},
		{http.MethodGet, "/admin/settings/mcp/remote", nil, true},
		{http.MethodPost, "/admin/settings/mcp/remote/tokens", url.Values{"name": {"公開サーバー"}}, false},
		{http.MethodPost, "/admin/settings/mcp/remote/login-code", nil, false},
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	mcpHeader := map[string]string{"Accept": "application/json, text/event-stream", "Authorization": "Bearer " + token}
	publicRoutes := []struct {
		method, path, contentType, body string
		header                          map[string]string
	}{
		{http.MethodPost, mcp.DefaultBasePath, "application/json", initialize, mcpHeader},
		{http.MethodGet, oauthAuthServerMetadataPath, "", "", nil},
		{http.MethodGet, mcp.ProtectedResourceMetadataPath, "", "", nil},
		{http.MethodGet, mcp.ProtectedResourceMetadataPath + mcp.DefaultBasePath, "", "", nil},
		{http.MethodPost, OAuthRegisterPath, "application/json", `{"client_name":"AIアプリ","redirect_uris":["http://127.0.0.1:9999/callback"]}`, nil},
		{http.MethodOptions, OAuthTokenPath, "", "", nil},
		{http.MethodPost, OAuthTokenPath, "application/x-www-form-urlencoded", "grant_type=authorization_code&code=x", nil},
		{http.MethodGet, OAuthAuthorizePath + "?response_type=code&client_id=x", "", "", nil},
	}
	adminStatus := func(method, path string, form url.Values) int {
		t.Helper()
		if method == http.MethodGet {
			return adm.get(path).StatusCode
		}
		return adm.postForm(path, form).StatusCode
	}

	// ON: ルートが存在する（404 にならない）。
	for _, r := range adminRoutes {
		if !r.onNot404 {
			continue
		}
		if got := adminStatus(r.method, r.path, r.form); got == http.StatusNotFound {
			t.Errorf("AI on: %s %s = 404, want the route to exist", r.method, r.path)
		}
	}
	for _, r := range publicRoutes {
		if got := env.rawRequest(r.method, r.path, r.contentType, r.body, r.header); got == http.StatusNotFound {
			t.Errorf("AI on: %s %s = 404, want the route to exist", r.method, r.path)
		}
	}
	if len(fake.calls()) == 0 {
		t.Fatal("AI on: the editor AI routes should have called the AI provider")
	}

	// OFF: すべて 404 で、AI のプロバイダーを呼ばない。
	env.setAIEnabled(false)
	calls := len(fake.calls())
	for _, r := range adminRoutes {
		if got := adminStatus(r.method, r.path, r.form); got != http.StatusNotFound {
			t.Errorf("AI off: %s %s = %d, want 404", r.method, r.path, got)
		}
	}
	for _, r := range publicRoutes {
		if got := env.rawRequest(r.method, r.path, r.contentType, r.body, r.header); got != http.StatusNotFound {
			t.Errorf("AI off: %s %s = %d, want 404", r.method, r.path, got)
		}
	}
	if got := len(fake.calls()); got != calls {
		t.Errorf("AI off: the AI provider was called %d more time(s)", got-calls)
	}
	// AI と関係のない画面・操作は今までどおり使える（記事の編集・メディアのタグの保存）。
	expectStatus(t, adm.get("/admin/articles/"+articleID+"/edit"), http.StatusOK)
	expectStatus(t, adm.get("/admin/media"), http.StatusOK)
	expectStatus(t, adm.get("/admin/settings/general"), http.StatusOK)

	// ON に戻せば、保存済みの MCP のトークンもそのまま使える（トークンは消していない）。
	env.setAIEnabled(true)
	if got := env.rawRequest(http.MethodPost, mcp.DefaultBasePath, "application/json", initialize, mcpHeader); got != http.StatusOK {
		t.Errorf("AI back on: MCP initialize with the saved token = %d, want 200", got)
	}
}

// aiEditorMarkers は記事エディタに AI の要素が出ているかを見分ける印。
var aiEditorMarkers = []string{`id="ai-toolbar"`, `id="ai-modal"`, `id="social-modal"`, `id="partial-translate-modal"`,
	`id="translate-confirm-modal"`, `data-ai-action`, `js/ai.js`, `href="/admin/settings/ai"`}

// AI 機能を OFF にしたサイトでは、記事エディタ・記事一覧・メディアライブラリ・設定のナビと入口に、AI のボタン・メニュー・
// モーダル・AI 設定と MCP 連携への導線を DOM ごと出さない。AI と関係のない機能（画像の挿入・タグの編集）は残す。
func TestAIDisabledHidesAIFromAdminHTML(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir(), AI: &fakeAI{}}, nil)
	adm := env.loggedIn(admin)
	writer := env.loggedIn(studentA)
	articleID := adm.createArticle("AIを隠す記事", "本文。")
	pubLoc := expectRedirect(t, adm.postForm("/admin/articles/new", articleFormFor("公開した記事", "ai-hidden-published", "publish")), "/admin/articles/")
	if editPathPattern.FindStringSubmatch(pubLoc) == nil {
		t.Fatalf("unexpected redirect %q", pubLoc)
	}
	env.insertMedia("m-ai-hide", "photo.png", "代替テキスト", "", "日常, 学会")

	pages := func() (edit, list, media, general, hub, writerHub string) {
		t.Helper()
		get := func(c *client, path string) string {
			t.Helper()
			res := c.get(path)
			expectStatus(t, res, http.StatusOK)
			return res.Body
		}
		return get(adm, "/admin/articles/"+articleID+"/edit"), get(adm, "/admin/articles?status=published"), get(adm, "/admin/media"),
			get(adm, "/admin/settings/general"), get(adm, "/admin/settings"), get(writer, "/admin/settings")
	}

	// ON（既定）: AI の要素が出ている。
	edit, list, media, general, hub, writerHub := pages()
	expectContains(t, edit, aiEditorMarkers...)
	expectContains(t, list, `data-social-open`)
	expectContains(t, media, `id="media-ai-panel"`, `id="media-ai-analyze"`)
	// AI の3画面は左メニュー・設定の入口とも「AI」1つにまとめる（画面の中のタブで切り替える）。
	expectContains(t, general, `href="/admin/settings/ai-integrations"`)
	expectNotContains(t, general, `href="/admin/settings/mcp"`)
	expectContains(t, hub, `id="hub-ai"`)
	expectContains(t, writerHub, `id="hub-ai-member"`, `href="/admin/settings/ai"`)
	expectNotContains(t, hub, `id="hub-mcp"`)
	expectNoGeneralAIToggle(t, general)
	expectAIToggle(t, adm.get(aiIntegrationsPath).Body, true)

	// OFF: AI の要素を DOM ごと出さない。
	env.setAIEnabled(false)
	edit, list, media, general, hub, writerHub = pages()
	expectNotContains(t, edit, aiEditorMarkers...)
	expectNotContains(t, edit, `id="retranslate-button"`, `id="source-partial-translate-btn"`, `id="translate-article-btn"`,
		`id="article-proposal"`, `id="proposal-modal"`, `AI で下書き`, `AI執筆アシスタント`)
	// 画像の挿入（メディアライブラリ・ギャラリー）は AI と関係がないため残す。
	expectContains(t, edit, `id="article-form"`, `id="media-picker"`, `id="gallery-modal"`)
	expectNotContains(t, list, `data-social-open`, `id="social-modal"`)
	expectNotContains(t, media, `id="media-ai-panel"`, `id="media-ai-analyze"`, `id="media-ai-hint"`, `id="media-ai-suggestions"`,
		`AIで作成`, `AI自動生成`, `data-analyzing=`, `href="/admin/settings/ai"`)
	// タグは手動でも使う機能のため、AI と書かずに「タグ」として残す。
	expectContains(t, media, `id="media-tags"`, `タグ（カンマ区切り）`, `日常, 学会`)
	expectNotContains(t, general, `href="/admin/settings/ai"`, `href="/admin/settings/mcp"`)
	// 管理者の「AI」は ON に戻す入口として残し、管理者以外の「AI」は出さない。
	expectContains(t, hub, `id="hub-ai"`)
	expectNotContains(t, hub, `id="hub-ai-member"`, `id="hub-mcp"`)
	expectNotContains(t, writerHub, `id="hub-ai-member"`, `id="hub-mcp"`, `href="/admin/settings/ai"`, `href="/admin/settings/mcp"`)
	// OFF に戻す入口は「AI・外部連携」だけ（左メニューに残す）。基本設定には重ねて出さない。
	expectNoGeneralAIToggle(t, general)
	expectContains(t, general, `href="/admin/settings/ai-integrations"`)
	expectAIToggle(t, adm.get(aiIntegrationsPath).Body, false)
}

// aiToggleInput は「AI・外部連携」の「AI機能を使う」のチェックボックス。
var aiToggleInput = regexp.MustCompile(`<input id="ai-int-enabled"[^>]*>`)

// expectNoGeneralAIToggle は、基本設定の画面 body に「AI機能を使う」のチェックボックス・説明が重ねて出ていないかを確かめる
// （AI 機能の ON/OFF は「AI・外部連携」に一本化した）。
func expectNoGeneralAIToggle(t *testing.T, body string) {
	t.Helper()
	expectNotContains(t, body, `id="ai-feature"`, `name="ai_enabled"`, `id="ai_enabled"`, `id="ai-forced-off-note"`, "AI機能を使う",
		`id="ai-settings-link"`, `id="ai-integrations-link"`)
}

// expectAIToggle は「AI・外部連携」の画面 body の「AI機能を使う」が、操作できるチェックボックスで checked かを確かめる。
func expectAIToggle(t *testing.T, body string, checked bool) {
	t.Helper()
	input := aiToggleInput.FindString(body)
	if input == "" {
		t.Fatalf("the AI toggle is missing:\n%.3000s", body)
	}
	if strings.Contains(input, "disabled") || !strings.Contains(input, `name="ai_enabled"`) {
		t.Errorf("the AI toggle should be editable: %s", input)
	}
	if got := strings.Contains(input, "checked"); got != checked {
		t.Errorf("AI toggle checked = %v, want %v: %s", got, checked, input)
	}
	if !strings.Contains(body, `<input type="hidden" name="ai_enabled" value="0">`) {
		t.Error("the AI toggle needs the hidden 0 so that unchecking it is saved")
	}
}

// AI 機能を OFF にしても、多言語対応のサイトの翻訳編集（英語版を手で書く）は使える。AI の翻訳ボタンは出さず、
// 表示の2択（原文と比較 / 仕上がりプレビュー）は原文バーに置く。
func TestAIDisabledTranslationEditorKeepsManualTranslation(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir(), AI: &fakeAI{}}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "翻訳", "translation", "")
		seedArticle(t, db, article.Article{ID: "art-ai-off-src", Title: "原文の記事", Slug: "ai-off-source", CategoryID: catID,
			ContentMarkdown: "原文の本文。", Status: article.StatusPublished, Lang: "ja", TranslationGroupID: "grp-ai-off"})
	})
	enableMultilingual(t, env.db)
	env.setAIEnabled(false)
	ed := env.loggedIn(chief)
	res := ed.get("/admin/articles/new?source_article_id=art-ai-off-src&lang=en")
	expectStatus(t, res, http.StatusOK)
	expectNotContains(t, res.Body, aiEditorMarkers...)
	expectNotContains(t, res.Body, `id="translate-article-btn"`, `id="source-partial-translate-btn"`, `下の「全文をAI翻訳」から始めると`)
	expectContains(t, res.Body, `id="split-source-pane"`, `id="source-body-display"`)
	at := strings.Index(res.Body, `id="source-article-banner"`)
	if at < 0 {
		t.Fatal("the source article banner is missing")
	}
	banner := res.Body[at:]
	banner = banner[:strings.Index(banner, `id="title"`)]
	if n := strings.Count(res.Body, `id="translation-view-toggle"`); n != 1 {
		t.Errorf("translation-view-toggle appears %d times, want 1", n)
	}
	expectContains(t, banner, `id="translation-view-toggle"`)
}

// AI 機能を OFF にしたサイトでは、記事の保存・公開・下書きへの戻し・ゴミ箱への移動で、外部の Embeddings API を
// 一度も呼ばず、保存済みの埋め込み（article_embeddings）も消さない。ON に戻すと、また埋め込みを作る。
func TestAIDisabledStopsEmbeddingsAndKeepsStoredOnes(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(api.Close)

	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	emb, err := rag.NewEmbedder(rag.EmbedderConfig{Kind: rag.KindOpenAI, APIKey: "test-key", Model: "test-embed", BaseURL: api.URL})
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	env.server.ragService = rag.NewService(env.db, emb)
	ed := env.loggedIn(chief)
	id := ed.createArticle("埋め込みの記事", "下書きの本文。")
	edit := "/admin/articles/" + id
	save := func(action, body string) {
		t.Helper()
		form := url.Values{"title": {"埋め込みの記事"}, "category_id": {"1"}, "content_markdown": {body}, "action": {action}}
		expectRedirect(t, ed.postForm(edit, form), edit+"/edit")
		env.server.waitRAGIndex()
	}

	// ON: 公開すると Embeddings API を呼んで埋め込みを作る（この試験の API が確かに使われることを確かめる）。
	save("publish", "公開した本文。")
	if requests.Load() == 0 {
		t.Fatal("AI on: publishing should call the Embeddings API")
	}
	chunks := env.embeddingChunks(id)
	if chunks == 0 {
		t.Fatal("AI on: no embeddings were stored")
	}

	// OFF: 公開中の本文を変えても、下書きに戻しても、ゴミ箱に移して戻しても、API を呼ばず埋め込みも消さない。
	env.setAIEnabled(false)
	before := requests.Load()
	save("publish", "OFFのあいだに直した本文。")
	save("draft", "下書きに戻した本文。")
	expectRedirect(t, ed.postForm(edit+"/delete", nil), "/admin/articles")
	expectStatus(t, ed.postForm(edit+"/restore", nil), http.StatusSeeOther)
	env.server.waitRAGIndex()
	if got := requests.Load(); got != before {
		t.Errorf("AI off: the Embeddings API was called %d time(s), want none", got-before)
	}
	if got := env.embeddingChunks(id); got != chunks {
		t.Errorf("AI off: stored embeddings = %d chunk(s), want the %d saved before (must not be deleted)", got, chunks)
	}

	// ON に戻すと、次の保存からまた埋め込みを作る。
	env.setAIEnabled(true)
	save("publish", "ONに戻して書き直した本文。")
	if requests.Load() == before {
		t.Error("AI back on: publishing should call the Embeddings API again")
	}
}

// 環境変数 SORUS_AI=off は、基本設定で保存した ai_enabled=1 より優先して AI 機能を OFF に固定する。基本設定の画面は
// トグルを無効にして理由を示し、保存しても ai_enabled は変わらない。環境変数を外せば保存した値（ON）に戻る。
func TestAIEnvForcedOffOverridesSetting(t *testing.T) {
	t.Setenv(AIEnvVar, "off")
	fake := &fakeAI{}
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir(), AI: fake}, nil)
	env.setAIEnabled(true)
	adm := env.loggedIn(admin)
	articleID := adm.createArticle("強制OFFの記事", "本文。")

	if env.server.aiEnabled() {
		t.Fatal("SORUS_AI=off should turn AI off even when ai_enabled=1")
	}
	expectStatus(t, adm.get("/admin/settings/ai"), http.StatusNotFound)
	expectStatus(t, adm.postForm("/admin/ai/proofread", url.Values{"text": {"本文。"}}), http.StatusNotFound)
	expectStatus(t, adm.get("/admin/settings/mcp"), http.StatusNotFound)
	if got := env.rawRequest(http.MethodGet, oauthAuthServerMetadataPath, "", "", nil); got != http.StatusNotFound {
		t.Errorf("OAuth metadata = %d, want 404", got)
	}
	if len(fake.calls()) != 0 {
		t.Error("the AI provider must not be called while SORUS_AI=off")
	}
	edit := adm.get("/admin/articles/" + articleID + "/edit")
	expectStatus(t, edit, http.StatusOK)
	expectNotContains(t, edit.Body, aiEditorMarkers...)

	integrations := adm.get(aiIntegrationsPath)
	expectStatus(t, integrations, http.StatusOK)
	expectContains(t, integrations.Body, `id="ai-int-forced"`, `SORUS_AI=off`)
	input := aiToggleInput.FindString(integrations.Body)
	if !strings.Contains(input, "disabled") || strings.Contains(input, `name=`) || strings.Contains(input, "checked") {
		t.Errorf("while SORUS_AI=off the AI toggle should be an unchecked, disabled box without a name: %s", input)
	}
	expectNotContains(t, integrations.Body, `<input type="hidden" name="ai_enabled"`, `href="/admin/settings/ai"`, `href="/admin/settings/mcp"`)
	general := adm.get("/admin/settings/general")
	expectStatus(t, general, http.StatusOK)
	expectNoGeneralAIToggle(t, general.Body)

	// 画面から送られても（無効にしたトグルを書き換えた要求・古い基本設定の画面からの要求でも）、保存済みの ai_enabled は変えない。
	expectRedirect(t, adm.postForm("/admin/settings/general", url.Values{"site_name": {"強制OFFのサイト"}, "ai_enabled": {"0"}}),
		"/admin/settings/general")
	expectRedirect(t, adm.postForm(aiIntegrationsPath, url.Values{"ai_enabled": {"0"}}), aiIntegrationsPath)
	if got := env.setting(settingAIEnabled); got != "1" {
		t.Errorf("ai_enabled = %q after saving under SORUS_AI=off, want the saved %q kept", got, "1")
	}

	for _, v := range []string{"OFF", "false", "0"} {
		t.Setenv(AIEnvVar, v)
		if env.server.aiEnabled() {
			t.Errorf("SORUS_AI=%q should turn AI off", v)
		}
	}
	// 環境変数を外せば、保存した値（ON）に戻る。
	t.Setenv(AIEnvVar, "")
	if !env.server.aiEnabled() {
		t.Fatal("without SORUS_AI the saved ai_enabled=1 should turn AI on")
	}
	expectStatus(t, adm.get("/admin/settings/ai"), http.StatusOK)
	expectAIToggle(t, adm.get(aiIntegrationsPath).Body, true)
}

// 「AI・外部連携」の「AI機能を使う」で OFF にして ON に戻しても、保存済みの AI の設定（ai_config）・画像のタグは残る。
// 基本設定から ai_enabled が送られてきても（AI の欄があった古い画面から保存しても）、保存済みの ON/OFF を変えない。
func TestAIToggleKeepsSavedAIConfig(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	adm := env.loggedIn(admin)
	const savedConfig = `{"provider":"openai","api_key":"sk-test-keep-0123456789","model":"gpt-keep-model"}`
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, aiSettingKey, savedConfig); err != nil {
		t.Fatal(err)
	}
	env.insertMedia("m-ai-keep", "photo.png", "代替テキスト", "キャプション", "日常, 学会")
	mediaTags := func() string {
		t.Helper()
		var tags string
		if err := env.db.Get(&tags, `SELECT ai_tags FROM media WHERE id = 'm-ai-keep'`); err != nil {
			t.Fatal(err)
		}
		return tags
	}
	saveIntegrations := func(form url.Values) {
		t.Helper()
		expectRedirect(t, adm.postForm(aiIntegrationsPath, form), aiIntegrationsPath)
	}
	saveGeneral := func(form url.Values) {
		t.Helper()
		form.Set("site_name", "AIを切り替えるサイト")
		expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general")
	}

	// OFF（チェックを外す = hidden の "0" だけが送られる）。
	saveIntegrations(url.Values{"ai_enabled": {"0"}})
	if got := env.setting(settingAIEnabled); got != "0" {
		t.Fatalf("ai_enabled = %q, want 0", got)
	}
	if env.server.aiEnabled() {
		t.Fatal("AI should be off after unchecking the toggle")
	}
	if got := env.setting(aiSettingKey); got != savedConfig {
		t.Errorf("ai_config changed after turning AI off:\n got %s\nwant %s", got, savedConfig)
	}
	if got := mediaTags(); got != "日常, 学会" {
		t.Errorf("media tags = %q after turning AI off", got)
	}
	expectStatus(t, adm.get("/admin/settings/ai"), http.StatusNotFound)
	expectAIToggle(t, adm.get(aiIntegrationsPath).Body, false)

	// 基本設定から保存しても（AI の欄があった古い画面が ON を送っても）、OFF のまま。
	saveGeneral(url.Values{})
	saveGeneral(url.Values{"ai_enabled": {"0", "1"}})
	if got := env.setting(settingAIEnabled); got != "0" {
		t.Errorf("ai_enabled = %q after saving the general settings, want 0 kept", got)
	}
	if env.server.aiEnabled() {
		t.Error("saving the general settings must not turn AI back on")
	}

	// ON（hidden の "0" とチェックの "1" が送られる）。保存済みの AI の設定がそのまま使える。
	saveIntegrations(url.Values{"ai_enabled": {"0", "1"}})
	if got := env.setting(settingAIEnabled); got != "1" {
		t.Fatalf("ai_enabled = %q, want 1", got)
	}
	if got := env.setting(aiSettingKey); got != savedConfig {
		t.Errorf("ai_config changed after turning AI back on:\n got %s\nwant %s", got, savedConfig)
	}
	res := adm.get("/admin/settings/ai")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "gpt-keep-model")
	if got := mediaTags(); got != "日常, 学会" {
		t.Errorf("media tags = %q after turning AI back on", got)
	}

	// ON のまま基本設定から OFF（hidden の "0" だけ）を送られても、ON のまま。
	saveGeneral(url.Values{"ai_enabled": {"0"}})
	if !env.server.aiEnabled() {
		t.Error("saving the general settings must not turn AI off")
	}
}

// AI 機能が OFF のサイトでは、パスワード変更の通知に AI アプリ（MCP のトークン）のことを書かない。
func TestAIDisabledPasswordFlashOmitsAIApps(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	adm := env.loggedIn(admin)
	res := adm.get("/admin/users?flash=user_password_updated")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "AIアプリはトークンの発行し直しが必要です")

	env.setAIEnabled(false)
	res = adm.get("/admin/users?flash=user_password_updated")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "パスワードを変えました。その人はログアウトされました")
	expectNotContains(t, res.Body, "AIアプリはトークンの発行し直しが必要です")
}

// AI 機能を OFF にした公開サーバーでは、同期 API の MCP 連携の操作（一覧・トークンの発行・失効・確認コード）も 404 を返し、
// トークンを作らない。ON に戻せば、OFF の前に発行したトークンもそのまま一覧に出る。
func TestAIDisabledSyncMCPReturnsNotFound(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newPublicMCPEnv(t)
	writer := env.token("ライターA", auth.RoleWriter)

	expectSyncStatus(t, env.call(writer, SyncRequest{Action: SyncActionMCPInfo}), http.StatusOK)
	rec := env.call(writer, SyncRequest{Action: SyncActionMCPIssueToken, MCP: &SyncMCPRequest{Name: "Claude Code", ExpiresDays: 30}})
	expectSyncStatus(t, rec, http.StatusOK)
	issued := decodeSync[SyncMCPIssueTokenResponse](t, rec)
	tokens := env.countRows(`SELECT COUNT(*) FROM mcp_tokens`)

	env.setAIEnabled(false)
	for _, req := range []SyncRequest{
		{Action: SyncActionMCPInfo},
		{Action: SyncActionMCPIssueToken, MCP: &SyncMCPRequest{Name: "別のAIアプリ", ExpiresDays: 30}},
		{Action: SyncActionMCPRevokeToken, MCP: &SyncMCPRequest{ID: issued.ID}},
		{Action: SyncActionMCPRevokeOAuthApp, MCP: &SyncMCPRequest{ID: "no-such-app"}},
		{Action: SyncActionMCPIssueLoginCode},
	} {
		if rec := env.call(writer, req); rec.Code != http.StatusNotFound {
			t.Errorf("AI off: sync %s = %d, want 404\nbody: %.500s", req.Action, rec.Code, rec.Body.String())
		}
	}
	if got := env.countRows(`SELECT COUNT(*) FROM mcp_tokens`); got != tokens {
		t.Errorf("AI off: mcp_tokens rows = %d, want %d (no token issued or revoked)", got, tokens)
	}

	env.setAIEnabled(true)
	rec = env.call(writer, SyncRequest{Action: SyncActionMCPInfo})
	expectSyncStatus(t, rec, http.StatusOK)
	if info := decodeSync[SyncMCPInfoResponse](t, rec); len(info.Tokens) != 1 || info.Tokens[0].ID != issued.ID {
		t.Errorf("AI back on: mcp_info tokens = %+v, want the token issued before", info.Tokens)
	}
}

// AI 機能を OFF にしたサイトでは、AI アプリ（MCP）から届いていた変更案を編集画面に読み込まず（差分の確認ダイアログも
// 出さない）、保存・校閲申請のフォームに proposal_id があっても案の確認・確定（削除）をしない（ふだんの保存として扱う）。
// 案は消さずに残し、ON に戻せばまた確かめられる。
func TestAIDisabledIgnoresArticleProposal(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir(), AI: &fakeAI{}}, nil)
	suzuki := env.loggedIn(studentB)
	id := suzuki.createArticle("鈴木の調査メモ", "一行目\n春の調査結果")
	token := issueMCPToken(env.loggedIn(studentA), "山田のエージェント", "30")
	if isErr, res, text := env.mcpTool(token, "cms_save_draft", map[string]any{"article_id": id, "content_markdown": "一行目\n秋に見直した案"}); isErr || res["needs_confirmation"] != true {
		t.Fatalf("cms_save_draft = %v %s", isErr, text)
	}
	_, pid, base := openProposal(t, suzuki, id)

	env.setAIEnabled(false)
	// 編集画面: 変更案の知らせも、差分の確認ダイアログ（data-proposal-id）も出さない。
	res := suzuki.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectNotContains(t, res.Body, `id="article-proposal"`, `id="article-proposal-waiting"`, `id="proposal-modal"`, `data-proposal-id=`,
		"AIアプリからの変更案")

	// 案の ID を送っても案を確かめない: 置き換わった（存在しない）案の ID でも 409 にせず、ふだんどおり保存する。
	a := env.mustGetArticle(id)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+id, proposalForm(a, a.Title, "手で書いた本文", "0123456789abcdef", base, "draft")),
		"/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id); got.ContentMarkdown != "手で書いた本文" {
		t.Errorf("AI off: save with a stale proposal_id = %q, want the normal save", got.ContentMarkdown)
	}
	// 今の案の ID を送っても案を確定しない（消さない）。版のメモにも「変更案を反映」と残さない。
	a = env.mustGetArticle(id)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+id, proposalForm(a, a.Title, "手で直した本文", pid, articleFingerprint(a), "draft")),
		"/admin/articles/"+id+"/edit")
	if got := env.mustGetArticle(id); got.ContentMarkdown != "手で直した本文" {
		t.Errorf("AI off: save with the current proposal_id = %q", got.ContentMarkdown)
	}
	if p := loadProposal(t, env.db, id); p == nil || p.ID != pid {
		t.Errorf("AI off: the proposal was finished by a save: %+v", p)
	}
	for _, c := range revisionCommentsOf(t, env.db, id) {
		if strings.Contains(c, "変更案を反映") {
			t.Errorf("AI off: revision comment %q says the proposal was applied", c)
		}
	}
	// 校閲申請も同じ: 存在しない案の ID でも 409 にせず申請し、案は残す。
	a = env.mustGetArticle(id)
	expectRedirect(t, suzuki.postForm("/admin/articles/"+id+"/submit-review", proposalForm(a, a.Title, "申請する本文", "0123456789abcdef", base, "review")),
		"/admin/articles?status=review")
	if got := env.mustGetArticle(id); got.Status != article.StatusReview || got.ContentMarkdown != "申請する本文" {
		t.Errorf("AI off: review submit with a stale proposal_id = %+v", got)
	}
	if p := loadProposal(t, env.db, id); p == nil || p.ID != pid {
		t.Errorf("AI off: the proposal was finished by a review submit: %+v", p)
	}
}
