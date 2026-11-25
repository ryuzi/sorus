package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"sorus/internal/ai"
)

// postAIWith は記事エディタと同じく /admin/ai/<action> に、使う AI（provider）を指定して送る。
func (c *client) postAIWith(action, text, provider string) (*response, aiResult) {
	c.t.Helper()
	form := url.Values{"text": {text}}
	if provider != "" {
		form.Set("provider", provider)
	}
	res := c.postAPI("/admin/ai/"+action, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	var out aiResult
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			c.t.Fatalf("POST /admin/ai/%s: %v\n%s", action, err, res.Body)
		}
	}
	return res, out
}

// AI の設定は、プロバイダーごとの欄（api_key_<種類> など）でまとめて保存でき、メインのプロバイダーを切り替えても
// ほかのプロバイダーのキーは消えない。保存データには古い版でも読める旧形式の項目も書く。
func TestAISettingsKeepsKeysPerProvider(t *testing.T) {
	clearAIEnv(t)
	t.Setenv("DEEPL_API_KEY", "")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{
		"provider":             {"gemini"},
		"api_key_gemini":       {"AQ.gemini-key"},
		"api_key_openai":       {"sk-openai-key"},
		"api_key_claude":       {"sk-ant-claude-key"},
		"model_claude":         {"claude-test-model"},
		"api_key_deepl":        {"deepl-key:fx"},
		"base_url_openai":      {"https://evil.example/v1"},
		"translation_provider": {"claude"},
	}), aiSettingsPath+"?flash=ai_saved")
	saved, ok := savedAI(t, env)
	if !ok || saved.Provider != ai.KindGemini || saved.TranslationProvider != ai.KindClaude {
		t.Fatalf("saved = %+v", saved)
	}
	want := map[string]savedProviderConfig{
		ai.KindGemini: {APIKey: "AQ.gemini-key"},
		ai.KindOpenAI: {APIKey: "sk-openai-key"},
		ai.KindClaude: {APIKey: "sk-ant-claude-key", Model: "claude-test-model"},
		ai.KindDeepL:  {APIKey: "deepl-key:fx"},
	}
	if len(saved.Providers) != len(want) {
		t.Fatalf("providers = %+v", saved.Providers)
	}
	for k, pc := range want {
		if saved.Providers[k] != pc {
			t.Errorf("providers[%s] = %+v, want %+v", k, saved.Providers[k], pc)
		}
	}
	// 旧形式の項目は、メインのプロバイダーと DeepL の設定を写す。
	if saved.APIKey != "AQ.gemini-key" || saved.DeepLAPIKey != "deepl-key:fx" || saved.BaseURL != "" {
		t.Errorf("legacy fields = %+v", saved)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(env.setting(aiSettingKey)), &raw); err != nil || raw["providers"] == nil || raw["api_key"] != "AQ.gemini-key" {
		t.Errorf("stored JSON = %s", env.setting(aiSettingKey))
	}

	// メインを OpenAI に切り替える（キーの欄は空）。ほかのプロバイダーのキーは残り、OpenAI のキーでメインが動く。
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"openai"}, "translation_provider": {"deepl"}}),
		aiSettingsPath+"?flash=ai_saved")
	saved, _ = savedAI(t, env)
	if saved.Provider != ai.KindOpenAI || saved.APIKey != "sk-openai-key" || saved.Providers[ai.KindGemini].APIKey != "AQ.gemini-key" ||
		saved.Providers[ai.KindClaude] != want[ai.KindClaude] || saved.TranslationProvider != ai.KindDeepL {
		t.Fatalf("after switching = %+v", saved)
	}
	if name := env.server.aiProvider().Name(); !strings.Contains(name, "OpenAI") {
		t.Errorf("main provider = %q", name)
	}

	// clear_key_<種類> は、そのプロバイダーのキーだけを消す。
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"openai"}, "clear_key_gemini": {"1"}}),
		aiSettingsPath+"?flash=ai_saved")
	saved, _ = savedAI(t, env)
	if _, ok := saved.Providers[ai.KindGemini]; ok || saved.Providers[ai.KindOpenAI].APIKey != "sk-openai-key" || saved.Providers[ai.KindDeepL].APIKey != "deepl-key:fx" {
		t.Fatalf("after clearing gemini = %+v", saved.Providers)
	}

	// ほかのプロバイダーの欄の誤りも保存しない。どのプロバイダーの欄かを伝える。
	res := adm.postForm(aiSettingsPath, url.Values{"provider": {"openai"}, "api_key_claude": {"two words"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "Claudeの")
	if after, _ := savedAI(t, env); after.Providers[ai.KindClaude] != want[ai.KindClaude] {
		t.Errorf("rejected save changed claude: %+v", after.Providers)
	}
	// 翻訳用 AI は保存済みのプロバイダー（ローカル LLM を含む）から選ぶ。知らない種類は保存しない。
	res = adm.postForm(aiSettingsPath, url.Values{"provider": {"openai"}, "translation_provider": {"bogus"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
}

// 設定画面には、プロバイダーごとの保存済みの状態（ProviderConfigs）を渡す。キーそのものは画面に出さない。管理者以外にも
// 一覧（翻訳用 AI の選択肢）は渡すが、キーの伏せ字やローカル LLM の送り先は渡さない。
func TestAISettingsProviderConfigsData(t *testing.T) {
	clearAIEnv(t)
	t.Setenv("DEEPL_API_KEY", "")
	mock := newCompatMock(t, "案")
	env := newTestEnv(t)
	pages := recordPages(env)
	adm := env.loggedIn(admin)
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{
		"provider": {"custom"}, "base_url_custom": {mock.URL + "/v1"}, "model_custom": {"qwen2.5"},
		"api_key_claude": {"sk-ant-secret-1234"},
	}), aiSettingsPath+"?flash=ai_saved")

	body := adm.get(aiSettingsPath).Body
	if strings.Contains(body, "sk-ant-secret-1234") {
		t.Fatal("the saved key must not be shown in full")
	}
	d, ok := pages.last(t, "settings_ai").Data.(aiSettingsData)
	if !ok {
		t.Fatalf("data = %T", pages.last(t, "settings_ai").Data)
	}
	items := map[string]aiProviderConfigItem{}
	var keys []string
	for _, it := range d.ProviderConfigs {
		items[it.Key] = it
		keys = append(keys, it.Key)
	}
	if strings.Join(keys, ",") != "gemini,openai,claude,custom,deepl" {
		t.Fatalf("provider configs = %v", keys)
	}
	if c := items[ai.KindClaude]; !c.HasSavedKey || c.KeyPreview != maskAIKey("sk-ant-secret-1234") || !c.Configured || c.KeyEnv != "ANTHROPIC_API_KEY" || c.DefaultModel != ai.DefaultClaudeModel {
		t.Errorf("claude = %+v", c)
	}
	if c := items[ai.KindCustom]; c.Model != "qwen2.5" || c.BaseURL != mock.URL+"/v1" || !c.Configured || c.HasSavedKey || !c.KeyOptional {
		t.Errorf("custom = %+v", c)
	}
	if g := items[ai.KindGemini]; g.HasSavedKey || g.Configured || len(g.Models) == 0 || g.KeyURL == "" {
		t.Errorf("gemini = %+v", g)
	}
	for k, pc := range d.Form.Providers {
		if pc.APIKey != "" {
			t.Errorf("form providers[%s] carries a key", k)
		}
	}
	if d.Form.Provider != ai.KindCustom {
		t.Errorf("form provider = %q", d.Form.Provider)
	}

	writer := env.loggedIn(studentA)
	expectStatus(t, writer.get(aiSettingsPath), http.StatusOK)
	wd := pages.last(t, "settings_ai").Data.(aiSettingsData)
	if len(wd.ProviderConfigs) != 5 {
		t.Fatalf("non-admin provider configs = %d", len(wd.ProviderConfigs))
	}
	for _, it := range wd.ProviderConfigs {
		if it.KeyPreview != "" || it.BaseURL != "" || it.KeySource != "" {
			t.Errorf("non-admin item %s leaks %+v", it.Key, it)
		}
	}
	if wd.Form.BaseURL != "" || wd.KeyPreview != "" {
		t.Errorf("non-admin form = %+v", wd.Form)
	}

	// 管理者以外がスタイルを保存しても、プロバイダーごとの設定は消えない。
	expectRedirect(t, writer.postForm(aiSettingsPath, url.Values{"writing_style": {"business"}, "translation_provider": {"custom"}}),
		aiSettingsPath+"?flash=ai_saved")
	saved, _ := savedAI(t, env)
	if saved.Providers[ai.KindClaude].APIKey != "sk-ant-secret-1234" || saved.Providers[ai.KindCustom].BaseURL != mock.URL+"/v1" ||
		saved.WritingStyle != "business" || saved.TranslationProvider != ai.KindCustom {
		t.Errorf("after a non-admin save = %+v", saved)
	}
}

// 旧形式（Provider・APIKey・Model・BaseURL・DeepLAPIKey だけ）の保存データは、読み込むときに Providers へ補完する。
func TestLoadAIConfigBackfillsLegacyFormat(t *testing.T) {
	clearAIEnv(t)
	env := newTestEnv(t)
	legacy := `{"provider":"custom","api_key":"legacy-key","model":"llama3.1","base_url":"http://localhost:11434/v1",` +
		`"translation_provider":"deepl","deepl_api_key":"legacy-deepl:fx","writing_style":"academic"}`
	if err := upsertSetting(env.db, aiSettingKey, legacy); err != nil {
		t.Fatal(err)
	}
	cfg, ok := env.server.loadAIConfig()
	if !ok {
		t.Fatal("legacy settings must load")
	}
	if got := cfg.Providers[ai.KindCustom]; got != (savedProviderConfig{APIKey: "legacy-key", Model: "llama3.1", BaseURL: "http://localhost:11434/v1"}) {
		t.Errorf("custom = %+v", got)
	}
	if got := cfg.Providers[ai.KindDeepL]; got != (savedProviderConfig{APIKey: "legacy-deepl:fx"}) {
		t.Errorf("deepl = %+v", got)
	}
	if cfg.APIKey != "legacy-key" || cfg.Model != "llama3.1" || cfg.BaseURL != "http://localhost:11434/v1" || cfg.DeepLAPIKey != "legacy-deepl:fx" || !cfg.AcademicStyle {
		t.Errorf("legacy fields = %+v", cfg)
	}

	// DeepL をメインにした古いデータは、メインのキーを DeepL のキーとして使う。
	for raw, want := range map[string]string{
		`{"provider":"deepl","api_key":"main-deepl"}`:                                                       "main-deepl",
		`{"provider":"deepl","deepl_api_key":"only-deepl"}`:                                                 "only-deepl",
		`{"provider":"gemini","api_key":"g","providers":{"deepl":{"api_key":"new"}},"deepl_api_key":"old"}`: "new",
	} {
		if err := upsertSetting(env.db, aiSettingKey, raw); err != nil {
			t.Fatal(err)
		}
		if cfg, _ := env.server.loadAIConfig(); cfg.Providers[ai.KindDeepL].APIKey != want || cfg.DeepLAPIKey != want {
			t.Errorf("%s: deepl = %+v", raw, cfg.Providers[ai.KindDeepL])
		}
	}
	// 知らないプロバイダーと、custom 以外のベース URL は捨てる。
	if err := upsertSetting(env.db, aiSettingKey, `{"provider":"gemini","providers":{"gemini":{"api_key":"g","base_url":"https://evil.example"},"bogus":{"api_key":"x"}}}`); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := env.server.loadAIConfig(); len(cfg.Providers) != 1 || cfg.Providers[ai.KindGemini] != (savedProviderConfig{APIKey: "g"}) {
		t.Errorf("sanitized providers = %+v", cfg.Providers)
	}
}

// 記事エディタの AI（/admin/ai/*）は provider で、保存済みのメイン以外のプロバイダーも使える。指定しなければメイン。
// 記事の編集画面には、選べる（使える）AI の一覧を渡す。
func TestAIRequestsUseRequestedProvider(t *testing.T) {
	clearAIEnv(t)
	t.Setenv("DEEPL_API_KEY", "")
	var geminiKeys []string
	gemini := geminiMock(t, http.StatusOK, geminiTitlesBody, &geminiKeys)
	// Gemini の送り先は環境変数（AI_BASE_URL）でモックへ向ける。キーは画面で保存したものを使う。
	t.Setenv("AI_PROVIDER", "gemini")
	t.Setenv("AI_BASE_URL", gemini.URL)
	compat := newCompatMock(t, "ローカル案A\nローカル案B")
	env := newTestEnv(t)
	pages := recordPages(env)
	adm := env.loggedIn(admin)
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{
		"provider": {"custom"}, "base_url_custom": {compat.URL}, "model_custom": {"llama3.1"},
		"api_key_gemini": {"AQ.saved-gemini"}, "api_key_deepl": {"deepl-key:fx"},
	}), aiSettingsPath+"?flash=ai_saved")

	writer := env.loggedIn(studentA)
	res, out := writer.postAIWith("suggest-titles", "本文", "")
	expectStatus(t, res, http.StatusOK)
	if !out.Configured || strings.Join(out.Titles, ",") != "ローカル案A,ローカル案B" || len(geminiKeys) != 0 {
		t.Fatalf("main provider = %+v (gemini calls %v)", out, geminiKeys)
	}

	res, out = writer.postAIWith("suggest-titles", "本文", "gemini")
	expectStatus(t, res, http.StatusOK)
	if !out.Configured || len(out.Titles) != 3 || !strings.Contains(out.Provider, "Gemini") {
		t.Fatalf("gemini = %+v", out)
	}
	if len(geminiKeys) != 1 || geminiKeys[0] != "AQ.saved-gemini" {
		t.Errorf("gemini keys = %v", geminiKeys)
	}
	if len(compat.authorizations()) != 1 {
		t.Errorf("the main provider must not be called for gemini: %v", compat.authorizations())
	}

	// キーの無いプロバイダーは設定ガイドを返し、知らない種類は 400。
	res, out = writer.postAIWith("summarize", "本文", "claude")
	expectStatus(t, res, http.StatusOK)
	if out.Configured || !strings.Contains(out.Guide, "ANTHROPIC_API_KEY") {
		t.Errorf("claude without a key = %+v", out)
	}
	res, _ = writer.postAIWith("proofread", "本文", "bogus")
	expectStatus(t, res, http.StatusBadRequest)
	// ファクトチェック・推敲（handlePhase7AI）とドラフトも provider を受け付ける。
	for _, path := range []string{"/admin/ai/academic", "/admin/ai/draft", "/admin/ai/social-post"} {
		res := writer.postAPI(path, "application/x-www-form-urlencoded",
			strings.NewReader(url.Values{"text": {"本文"}, "topic": {"テーマ"}, "action": {ai.ActionSimplify}, "title": {"題"}, "provider": {"bogus"}}.Encode()))
		expectStatus(t, res, http.StatusBadRequest)
	}

	// 編集画面には、メイン（先頭）と、キーなどが設定されて使える AI を渡す。翻訳専用の DeepL は含めない。
	expectStatus(t, writer.get("/admin/articles/new"), http.StatusOK)
	d, ok := pages.last(t, "article_edit").Data.(editData)
	if !ok {
		t.Fatalf("article_edit data = %T", pages.last(t, "article_edit").Data)
	}
	var got []string
	for _, p := range d.AIConfiguredProviders {
		got = append(got, p.Value+"="+p.Label)
	}
	if len(d.AIConfiguredProviders) != 2 || d.AIConfiguredProviders[0].Value != ai.KindCustom || d.AIConfiguredProviders[1].Value != ai.KindGemini ||
		!strings.Contains(d.AIConfiguredProviders[1].Label, "Gemini") || d.AIProviderKind != ai.KindCustom || d.AIProvider != d.AIConfiguredProviders[0].Label {
		t.Errorf("configured providers = %v, kind %q, provider %q", got, d.AIProviderKind, d.AIProvider)
	}
}

// 翻訳は、要求で指定した種類・翻訳用 AI・メインの順に決め、保存済みのその種類の設定で作る。
func TestBuildTranslationProviderUsesSavedProviders(t *testing.T) {
	clearAIEnv(t)
	t.Setenv("DEEPL_API_KEY", "")
	compat := newCompatMock(t, "訳")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{
		"provider": {"claude"}, "api_key_claude": {"sk-ant-key"},
		"base_url_custom": {compat.URL}, "api_key_deepl": {"deepl-key:fx"}, "translation_provider": {"deepl"},
	}), aiSettingsPath+"?flash=ai_saved")
	s := env.server
	for requested, want := range map[string]string{
		"":       "DeepL (Free)",
		"deepl":  "DeepL (Free)",
		"claude": s.aiProvider().Name(),
		"custom": "",
	} {
		p, err := s.buildTranslationProvider(requested)
		if err != nil || !ai.IsConfigured(p) {
			t.Errorf("%q: provider %v, err %v", requested, p, err)
			continue
		}
		if want != "" && p.Name() != want {
			t.Errorf("%q: name = %q, want %q", requested, p.Name(), want)
		}
	}
	if _, err := s.buildTranslationProvider("bogus"); err == nil {
		t.Error("an unknown translation provider must be rejected")
	}
	if p, err := s.buildTranslationProvider("openai"); err != nil || ai.IsConfigured(p) {
		t.Errorf("openai without a key = %v, %v", p, err)
	}
	// 部分翻訳の API もローカル LLM を受け付ける。
	res := env.loggedIn(studentA).postAPI("/admin/articles/translate", "application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"text": {"こんにちは"}, "provider": {"custom"}, "target_lang": {"en"}}.Encode()))
	if res.StatusCode == http.StatusBadRequest && strings.Contains(res.Body, "無効な翻訳プロバイダー") {
		t.Errorf("custom translation provider rejected: %s", res.Body)
	}
}

// 接続テストは test_provider で、そのプロバイダーのカードの欄だけで試せる。ほかのカードの書きかけの欄の誤りでは
// 止めない。
func TestAISettingsConnectionTestPerProvider(t *testing.T) {
	clearAIEnv(t)
	gemini := geminiMock(t, http.StatusOK, geminiTitlesBody, nil)
	t.Setenv("AI_PROVIDER", "gemini")
	t.Setenv("AI_BASE_URL", gemini.URL)
	compat := newCompatMock(t, "案X")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	post := func(form url.Values) aiTestResponse {
		t.Helper()
		res := adm.postAPI(aiSettingsTestPath, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		expectStatus(t, res, http.StatusOK)
		var out aiTestResponse
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			t.Fatalf("not JSON: %s", res.Body)
		}
		return out
	}
	// メインはローカル LLM（ベース URL が空で誤り）だが、Gemini のカードは試せる。
	out := post(url.Values{"provider": {"custom"}, "base_url_custom": {""}, "test_provider": {"gemini"}, "api_key_gemini": {"AQ.form-key"}})
	if !out.OK || len(out.Titles) != 3 || !strings.Contains(out.Provider, "Gemini") {
		t.Fatalf("gemini card test = %+v", out)
	}
	out = post(url.Values{"test_provider": {"custom"}, "base_url_custom": {compat.URL}, "model_custom": {"qwen2.5"}})
	if !out.OK || strings.Join(out.Titles, ",") != "案X" || !strings.Contains(out.Provider, "qwen2.5") {
		t.Fatalf("custom card test = %+v", out)
	}
	// 試すカードの欄の誤りは伝える。ベース URL の無いローカル LLM は、その旨を伝える。
	if out := post(url.Values{"provider": {"gemini"}, "test_provider": {"claude"}, "api_key_claude": {"a b"}}); out.OK || !strings.Contains(out.Message, "空白や改行") {
		t.Errorf("invalid claude card = %+v", out)
	}
	if out := post(url.Values{"provider": {"gemini"}, "test_provider": {"custom"}}); out.OK || !strings.Contains(out.Message, "ベースURL") {
		t.Errorf("custom without base URL = %+v", out)
	}
	if out := post(url.Values{"provider": {"gemini"}, "test_provider": {"bogus"}}); out.OK || out.Message == "" {
		t.Errorf("unknown test provider = %+v", out)
	}
	if _, saved := savedAI(t, env); saved {
		t.Error("connection tests must not save the settings")
	}
}
