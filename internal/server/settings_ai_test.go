package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/gommon/log"

	"sorus/internal/ai"
)

// clearAIEnv は AI の環境変数を空にする（開発者の PC の GEMINI_API_KEY などに結果を左右されないようにする）。
func clearAIEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AI_PROVIDER", "AI_API_KEY", "AI_MODEL", "AI_BASE_URL", "GEMINI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	// 試験のモック AI（httptest）はループバックで動くため、サーバーモードのカスタム AI の送り先の制限を外す。
	// 制限そのものは handlers_settings_ai_test.go（isolateAIBaseURLPolicy で明示的に戻す）で確かめる。
	t.Setenv(allowPrivateAIBaseURLEnv, "1")
}

// compatMock は OpenAI 互換の API（/chat/completions）のモック。受け取った Authorization ヘッダーを記録する。
type compatMock struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newCompatMock(t *testing.T, reply string) *compatMock {
	t.Helper()
	m := &compatMock{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.auths = append(m.auths, r.Header.Get("Authorization"))
		m.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}}})
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *compatMock) authorizations() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.auths...)
}

// geminiMock は Gemini の generateContent のモック。status が 200 以外ならその body を返す。
func geminiMock(t *testing.T, status int, body string, keys *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if keys != nil {
			*keys = append(*keys, r.Header.Get("X-Goog-Api-Key"))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// タイトル案は「案1」のような番号だけの行にしない（parseTitles が番号として取り除く）。
const geminiTitlesBody = `{"candidates":[{"content":{"parts":[{"text":"春の読書会\n図書館だより\n子どもと本"}]}}]}`

// geminiKeyInvalidBody は Google が無効なキーに返す 400 の本文。
const geminiKeyInvalidBody = `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT",` +
	`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com"}]}}`

func savedAI(t *testing.T, env *testEnv) (savedAIConfig, bool) {
	t.Helper()
	return env.server.loadAIConfig()
}

func TestAISettingsRolePermissions(t *testing.T) {
	clearAIEnv(t)
	env := newTestEnv(t)
	// 翻訳用の DeepL の欄は多言語対応を ON にしたサイトだけに出す（新しいサイトの既定は OFF）。
	enableMultilingual(t, env.db)
	res := env.loggedIn(admin).get(aiSettingsPath)
	expectStatus(t, res, http.StatusOK)
	// プロバイダーごとのカード（キー・モデルの欄と接続テスト）と、メインAIの選択（SPEC §2-7）。
	expectContains(t, res.Body, `id="ai-settings-form"`, `id="ai-main-provider" name="provider"`,
		`<option value="gemini"`, `<option value="openai"`, `<option value="claude"`, `<option value="custom"`,
		`name="api_key_gemini"`, `name="api_key_deepl"`, `name="model_claude"`, `name="base_url_custom"`,
		`data-ai-test="gemini"`, `data-ai-test="deepl"`, "の接続テスト",
		ai.DefaultGeminiModel, "gemini-4-argon", "未設定（AIの機能は使えません）")
	// 結果は textContent で表示し、innerHTML は使わない（AI の応答やエラーの文を HTML として解釈しない）。
	_, script, ok := strings.Cut(res.Body, "getElementById('ai-test-result')")
	script, _, _ = strings.Cut(script, "</script>")
	if !ok || !strings.Contains(script, "statusEl.textContent") || !strings.Contains(script, "messageEl.textContent") ||
		!strings.Contains(script, "li.textContent") || strings.Contains(script, "innerHTML") {
		t.Errorf("connection test result must be rendered with textContent only")
	}

	// 編集者・ライターはスタイル設定画面を開け、インフラ設定（APIキー等）は触れず、スタイルのみ保存できる
	for _, u := range []testUser{chief, studentA} {
		c := env.loggedIn(u)
		view := c.get(aiSettingsPath)
		expectStatus(t, view, http.StatusOK)
		expectContains(t, view.Body, "AI執筆支援設定", "AIプロバイダー・インフラ設定（管理者管理）", "スタイル設定を保存する")
		expectNotContains(t, view.Body, `id="ai-main-provider"`, `name="api_key_`, `data-ai-test=`, "の接続テスト")

		// スタイル設定を保存
		postRes := c.postForm(aiSettingsPath, url.Values{
			"provider":       {"malicious_provider"},
			"api_key_gemini": {"evil_key"},
			"writing_style":  {"creative"},
			"glossary":       {"AI=Artificial Intelligence"},
		})
		expectRedirect(t, postRes, aiSettingsPath+"?flash=ai_saved")

		// 保存された設定を検証: インフラ（provider, api_key）は改ざんされず、スタイルのみ保存されていること
		saved, ok := savedAI(t, env)
		if !ok || saved.WritingStyle != "creative" || !strings.Contains(saved.Glossary, "AI=Artificial Intelligence") {
			t.Errorf("user %s should be able to save writing style: %+v", u.Username, saved)
		}
		if saved.Provider == "malicious_provider" || saved.Providers[ai.KindGemini].APIKey == "evil_key" {
			t.Errorf("non-admin %s must not tamper with provider or api_key: %+v", u.Username, saved)
		}

		// 接続テスト・リセットは管理者専用（Forbidden）
		expectStatus(t, c.postAPI(aiSettingsTestPath, "application/x-www-form-urlencoded", strings.NewReader("provider=gemini")), http.StatusForbidden)
		expectStatus(t, c.postForm(aiSettingsReset, nil), http.StatusForbidden)
	}
}

// 画面で保存すると、再起動しなくても記事エディタの AI がその設定で動く。custom 以外ではベース URL を保存しない。
func TestAISettingsSaveSwapsProvider(t *testing.T) {
	clearAIEnv(t)
	mock := newCompatMock(t, "案A\n案B")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	res := adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {mock.URL + "/v1/"}, "model_custom": {"llama3.1:8b"}})
	expectRedirect(t, res, aiSettingsPath+"?flash=ai_saved")
	saved, ok := savedAI(t, env)
	if !ok || saved.Provider != ai.KindCustom || saved.Providers[ai.KindCustom].BaseURL != mock.URL+"/v1" || saved.Providers[ai.KindCustom].Model != "llama3.1:8b" || saved.Providers[ai.KindCustom].APIKey != "" {
		t.Fatalf("saved = %+v", saved)
	}
	writer := env.loggedIn(studentA)
	r, out := writer.postAI("suggest-titles", "本文")
	expectStatus(t, r, http.StatusOK)
	if !out.Configured || strings.Join(out.Titles, ",") != "案A,案B" || !strings.Contains(out.Provider, "llama3.1:8b") {
		t.Fatalf("suggest-titles = %+v", out)
	}
	if auth := mock.authorizations(); len(auth) != 1 || auth[0] != "" {
		t.Errorf("local LLM without key must not send Authorization: %q", auth)
	}

	// Gemini に切り替えると、送られてきたベース URL は保存しない。
	res = adm.postForm(aiSettingsPath, url.Values{"provider": {"gemini"}, "api_key_gemini": {"AQ.Ab8RN6-test"}, "base_url_gemini": {"https://evil.example/v1"}})
	expectRedirect(t, res, aiSettingsPath+"?flash=ai_saved")
	saved, _ = savedAI(t, env)
	if saved.Provider != ai.KindGemini || saved.Providers[ai.KindGemini].BaseURL != "" || saved.Providers[ai.KindGemini].APIKey != "AQ.Ab8RN6-test" {
		t.Fatalf("saved = %+v", saved)
	}
	if name := env.server.aiProvider().Name(); name != "Gemini ("+ai.DefaultGeminiModel+")" {
		t.Errorf("provider = %q", name)
	}
	page := adm.get(aiSettingsPath + "?flash=ai_saved")
	expectContains(t, page.Body, "AIの設定を保存しました", "この画面で保存したキー", maskAIKey("AQ.Ab8RN6-test"))
	if strings.Contains(page.Body, "AQ.Ab8RN6-test") {
		t.Error("the saved key must not be shown in full")
	}

	// 画面の設定を消すと、起動時（環境変数）のプロバイダーに戻る。
	expectRedirect(t, adm.postForm(aiSettingsReset, nil), aiSettingsPath+"?flash=ai_reset")
	if _, ok := savedAI(t, env); ok || ai.IsConfigured(env.server.aiProvider()) {
		t.Errorf("reset must remove the saved settings and fall back to Config.AI")
	}
}

// 入力の誤りは保存せず、理由を表示する。
func TestAISettingsValidation(t *testing.T) {
	clearAIEnv(t)
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	for _, tc := range []struct {
		form url.Values
		msg  string
	}{
		{url.Values{"provider": {"unknown"}, "api_key_gemini": {"k"}}, "使うAIの種類を選んでください"},
		{url.Values{"provider": {"custom"}}, "ベースURLを入力してください"},
		{url.Values{"provider": {"custom"}, "base_url_custom": {"ftp://example.jp"}}, "http:// または https://"},
		{url.Values{"provider": {"custom"}, "base_url_custom": {"http://user:pass@example.jp/v1"}}, "http:// または https://"},
		{url.Values{"provider": {"gemini"}, "api_key_gemini": {"k"}, "model_gemini": {"bad model"}}, "モデル名は半角の英数字"},
		{url.Values{"provider": {"gemini"}, "api_key_gemini": {"two words"}}, "空白や改行"},
		// キーがどこにも無い設定は保存しない（保存すると AI が使えなくなる）。
		{url.Values{"provider": {"openai"}}, "APIキーを入力してください"},
	} {
		res := adm.postForm(aiSettingsPath, tc.form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, tc.msg)
		if _, ok := savedAI(t, env); ok {
			t.Fatalf("%v: invalid settings were saved", tc.form)
		}
	}
}

// 再現テスト: キーを空欄のまま保存しても、環境変数のキーで動き続ける（Dummy に降格しない）。再起動後も同じ。
func TestAISettingsBlankKeyKeepsEnvKey(t *testing.T) {
	clearAIEnv(t)
	var keys []string
	mock := geminiMock(t, http.StatusOK, geminiTitlesBody, &keys)
	t.Setenv("GEMINI_API_KEY", "env-gemini-key")
	t.Setenv("AI_PROVIDER", "gemini")
	t.Setenv("AI_BASE_URL", mock.URL)
	envAI, err := ai.NewProviderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	env := newTestEnvWith(t, Config{AI: envAI}, nil)
	adm := env.loggedIn(admin)

	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"gemini"}, "api_key_gemini": {""}, "model_gemini": {"gemini-3.5-flash-lite"}}),
		aiSettingsPath+"?flash=ai_saved")
	saved, _ := savedAI(t, env)
	if saved.Providers[ai.KindGemini].APIKey != "" {
		t.Fatalf("blank key must not be stored: %+v", saved)
	}
	if !ai.IsConfigured(env.server.aiProvider()) {
		t.Fatal("saving with a blank key must keep using GEMINI_API_KEY")
	}
	expectContains(t, adm.get(aiSettingsPath).Body, "環境変数 GEMINI_API_KEY")
	r, out := env.loggedIn(studentA).postAI("suggest-titles", "本文")
	expectStatus(t, r, http.StatusOK)
	if len(out.Titles) != 3 || len(keys) != 1 || keys[0] != "env-gemini-key" {
		t.Fatalf("titles = %v, keys = %v", out.Titles, keys)
	}

	// 再起動（同じ DB で New）しても、保存した設定と環境変数のキーで動く。
	s2, err := New(Config{DB: env.db, UploadDir: filepath.Join(t.TempDir(), "u"), AttachmentDir: filepath.Join(t.TempDir(), "a"),
		DisableScheduler: true, AI: ai.NewDummy("")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s2.Shutdown(ctx)
	})
	if p := s2.aiProvider(); !ai.IsConfigured(p) || p.Name() != "Gemini (gemini-3.5-flash-lite)" {
		t.Errorf("after restart provider = %q", p.Name())
	}
}

// 再現テスト: プロバイダーや送り先（custom のベース URL）を変えてキーを空欄で保存・接続テストしたとき、保存済みの
// キーをそこへ送らない。
func TestAISettingsKeyIsNotSentToAnotherDestination(t *testing.T) {
	clearAIEnv(t)
	first := newCompatMock(t, "案A")
	second := newCompatMock(t, "案B")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {first.URL}, "api_key_custom": {"secret-first"}}),
		aiSettingsPath+"?flash=ai_saved")
	// 同じ送り先なら、空欄のまま保存してもキーを引き継ぐ。
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {first.URL}, "model_custom": {"qwen2.5"}}),
		aiSettingsPath+"?flash=ai_saved")
	if saved, _ := savedAI(t, env); saved.Providers[ai.KindCustom].APIKey != "secret-first" || saved.Providers[ai.KindCustom].Model != "qwen2.5" {
		t.Fatalf("same destination must keep the key: %+v", saved)
	}

	// 接続テスト（保存前）で別の送り先を試しても、保存済みのキーは送らない。
	var tr aiTestResponse
	res := adm.postAPI(aiSettingsTestPath, "application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"provider": {"custom"}, "base_url_custom": {second.URL}}.Encode()))
	expectStatus(t, res, http.StatusOK)
	if err := json.Unmarshal([]byte(res.Body), &tr); err != nil || !tr.OK {
		t.Fatalf("test = %s (%v)", res.Body, err)
	}
	if auth := second.authorizations(); len(auth) != 1 || auth[0] != "" {
		t.Fatalf("the saved key was sent to another server: %q", auth)
	}

	// 別の送り先で保存すると、キーは引き継がない。
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {second.URL}}), aiSettingsPath+"?flash=ai_saved")
	if saved, _ := savedAI(t, env); saved.Providers[ai.KindCustom].APIKey != "" || saved.Providers[ai.KindCustom].BaseURL != second.URL {
		t.Fatalf("another destination must not inherit the key: %+v", saved)
	}
	env.loggedIn(studentA).postAI("suggest-titles", "本文")
	for _, a := range second.authorizations() {
		if strings.Contains(a, "secret-first") {
			t.Fatalf("the key leaked to the second server: %q", a)
		}
	}

	// 別のプロバイダーに切り替えたときも、保存済みのキーを使わない（キーが無いので保存しない）。
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {first.URL}, "api_key_custom": {"secret-first"}}),
		aiSettingsPath+"?flash=ai_saved")
	res = adm.postForm(aiSettingsPath, url.Values{"provider": {"openai"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if saved, _ := savedAI(t, env); saved.Provider != ai.KindCustom || saved.Providers[ai.KindCustom].APIKey != "secret-first" {
		t.Fatalf("rejected save must keep the previous settings: %+v", saved)
	}
	// 「保存済みのキーを消す」。
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {first.URL}, "clear_key_custom": {"1"}}),
		aiSettingsPath+"?flash=ai_saved")
	if saved, _ := savedAI(t, env); saved.Providers[ai.KindCustom].APIKey != "" {
		t.Fatalf("clear_key_custom must remove the key: %+v", saved)
	}
}

func TestResolveAIConfigPriority(t *testing.T) {
	env := ai.Config{Kind: ai.KindGemini, APIKey: "env-key", Model: "env-model", BaseURL: "http://proxy.example"}
	getenv := func(k string) string {
		return map[string]string{"GEMINI_API_KEY": "env-key", "OPENAI_API_KEY": "openai-env"}[k]
	}
	cases := []struct {
		name               string
		saved              savedAIConfig
		key, src, base, md string
	}{
		{"saved key wins", savedAIConfig{Provider: ai.KindGemini, Providers: map[string]savedProviderConfig{ai.KindGemini: {APIKey: "db-key"}}}, "db-key", "saved", "http://proxy.example", "env-model"},
		{"env key fallback", savedAIConfig{Provider: ai.KindGemini}, "env-key", "GEMINI_API_KEY", "http://proxy.example", "env-model"},
		{"provider env var", savedAIConfig{Provider: ai.KindOpenAI, Providers: map[string]savedProviderConfig{ai.KindOpenAI: {Model: "m"}}}, "openai-env", "OPENAI_API_KEY", "", "m"},
		{"no key", savedAIConfig{Provider: ai.KindClaude}, "", "", "", ""},
		{"custom ignores other env", savedAIConfig{Provider: ai.KindCustom, Providers: map[string]savedProviderConfig{ai.KindCustom: {BaseURL: "http://localhost:11434/v1"}}}, "", "", "http://localhost:11434/v1", ""},
	}
	for _, tc := range cases {
		cfg, src := resolveAIConfig(tc.saved, env, getenv)
		if cfg.APIKey != tc.key || src != tc.src || cfg.BaseURL != tc.base || cfg.Model != tc.md || cfg.Kind != tc.saved.Provider {
			t.Errorf("%s: cfg = %+v, source = %q", tc.name, cfg, src)
		}
	}
	// custom の環境変数のキーは、同じ送り先のときだけ使う。
	customEnv := ai.Config{Kind: ai.KindCustom, APIKey: "custom-env", BaseURL: "https://llm.example/v1/"}
	if cfg, _ := resolveAIConfig(savedAIConfig{Provider: ai.KindCustom, Providers: map[string]savedProviderConfig{ai.KindCustom: {BaseURL: "https://llm.example/v1"}}}, customEnv, getenv); cfg.APIKey != "custom-env" {
		t.Errorf("same custom destination: key = %q", cfg.APIKey)
	}
	if cfg, _ := resolveAIConfig(savedAIConfig{Provider: ai.KindCustom, Providers: map[string]savedProviderConfig{ai.KindCustom: {BaseURL: "https://other.example/v1"}}}, customEnv, getenv); cfg.APIKey != "" {
		t.Errorf("another custom destination must not get the env key: %q", cfg.APIKey)
	}
}

// 接続テストは、タイトル提案の結果と応答時間を返す。失敗したときは利用者に分かる文だけを返し、生のエラーはログに残す。
func TestAISettingsConnectionTest(t *testing.T) {
	clearAIEnv(t)
	logs := &lockedBuffer{}
	env := newTestEnvWith(t, Config{LogOutput: logs}, nil)
	adm := env.loggedIn(admin)
	post := func(form url.Values) aiTestResponse {
		t.Helper()
		res := adm.postAPI(aiSettingsTestPath, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		expectStatus(t, res, http.StatusOK)
		var out aiTestResponse
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			t.Fatalf("not JSON: %s", res.Body)
		}
		if strings.Contains(res.Body, "API key not valid") || strings.Contains(res.Body, "INVALID_ARGUMENT") {
			t.Errorf("raw provider error leaked: %s", res.Body)
		}
		return out
	}

	ok := newCompatMock(t, "<img src=x onerror=alert(1)>\n読書会のお知らせ")
	out := post(url.Values{"provider": {"custom"}, "base_url_custom": {ok.URL}, "model_custom": {"llama3.1"}})
	if !out.OK || len(out.Titles) != 2 || out.ElapsedMS < 0 || !strings.Contains(out.Provider, "llama3.1") {
		t.Fatalf("ok test = %+v", out)
	}
	// 接続テストは保存しない。
	if _, saved := savedAI(t, env); saved {
		t.Error("connection test must not save the settings")
	}

	// 再現テスト: Gemini の 400 API_KEY_INVALID は「キーが無効または未認可」と表示する。
	bad := geminiMock(t, http.StatusBadRequest, geminiKeyInvalidBody, nil)
	t.Setenv("AI_PROVIDER", "gemini")
	t.Setenv("AI_BASE_URL", bad.URL)
	out = post(url.Values{"provider": {"gemini"}, "api_key_gemini": {"AQ.invalid-key"}})
	if out.OK || out.Message != aiAuthErrorMessage {
		t.Fatalf("invalid key test = %+v", out)
	}
	if l := logs.String(); !strings.Contains(l, "API_KEY_INVALID") || !strings.Contains(l, "API key not valid") || strings.Contains(l, "AQ.invalid-key") {
		t.Errorf("log must keep the raw error without the key:\n%s", l)
	}

	if out := post(url.Values{"provider": {"claude"}}); out.OK || out.Message != "APIキーを入力してください" {
		t.Errorf("no key test = %+v", out)
	}
	if out := post(url.Values{"provider": {"custom"}}); out.OK || !strings.Contains(out.Message, "ベースURL") {
		t.Errorf("invalid form test = %+v", out)
	}
}

// 再現テスト: Gemini が 400（API_KEY_INVALID・FAILED_PRECONDITION）を返したとき、記事エディタの AI は
// 「キーが無効または未認可」と表示し、Google の生のメッセージは画面に出さずログ（sorus.log）に残す。
func TestAIGemini400ShowsKeyErrorAndLogs(t *testing.T) {
	for _, body := range []string{
		geminiKeyInvalidBody,
		`{"error":{"code":400,"message":"The API key is not bound to a service account yet.","status":"FAILED_PRECONDITION"}}`,
	} {
		mock := geminiMock(t, http.StatusBadRequest, body, nil)
		logs := &lockedBuffer{}
		env := newTestEnvWith(t, Config{AI: ai.NewGemini(ai.Config{APIKey: "AQ.secret-key", BaseURL: mock.URL}), LogOutput: logs}, nil)
		if lvl := env.server.echo.Logger.Level(); lvl != log.INFO {
			t.Fatalf("logger level = %v, want INFO", lvl)
		}
		c := env.loggedIn(studentA)
		res, out := c.postAI("proofread", "本文")
		expectStatus(t, res, http.StatusBadGateway)
		if out.Message != aiAuthErrorMessage {
			t.Errorf("message = %q", out.Message)
		}
		if strings.Contains(res.Body, "API key") || strings.Contains(res.Body, "service account") {
			t.Errorf("raw error leaked to the screen: %s", res.Body)
		}
		l := logs.String()
		if !strings.Contains(l, "Gemini API returned 400") || strings.Contains(l, "AQ.secret-key") {
			t.Errorf("sorus.log = %s", l)
		}
	}
}

// 設定の保存（プロバイダーの差し替え）と、AI を使う要求の処理が同時に起きても競合しない（go test -race で検出する）。
func TestAISettingsConcurrentSwap(t *testing.T) {
	clearAIEnv(t)
	mock := newCompatMock(t, "案")
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	adm := env.loggedIn(admin)
	writer := env.loggedIn(studentA)
	token := writer.csrfToken()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = env.server.aiProvider().Name()
				req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/admin/ai/suggest-titles", strings.NewReader("text=本文"))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set(CSRFHeaderName, token)
				if res, err := writer.http.Do(req); err == nil {
					res.Body.Close()
				}
			}
		}()
	}
	for j := 0; j < 5; j++ {
		adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {mock.URL}, "model_custom": {fmt.Sprintf("m%d", j)}})
	}
	wg.Wait()
	if name := env.server.aiProvider().Name(); !strings.Contains(name, "m4") {
		t.Errorf("provider = %q", name)
	}
}

func TestSettingsAI_WritingStylePresetsAndGlossary(t *testing.T) {
	clearAIEnv(t)
	mock := newCompatMock(t, "案")
	env := newTestEnv(t)
	// 多言語翻訳・文章スタイルの設定は多言語対応を ON にしたサイトだけに出す（新しいサイトの既定は OFF）。
	enableMultilingual(t, env.db)
	adm := env.loggedIn(admin)

	// 1. GET 画面表示の検証
	res := adm.get(aiSettingsPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`name="writing_style"`,
		`value="standard"`,
		`value="academic"`,
		`value="business"`,
		`value="creative"`,
		"人工知能=Artificial Intelligence",
		"機械学習=Machine Learning",
		"メインAI（執筆・推敲・要約に使うAI）",
		"多言語翻訳・文章スタイル設定",
	)
	if strings.Contains(res.Body, "燃料デブリ") {
		t.Error("settings_ai.html must not contain '燃料デブリ'")
	}

	// 2. 新しい writing_style の保存
	postData := url.Values{
		"provider":        {"custom"},
		"base_url_custom": {mock.URL + "/v1"},
		"writing_style":   {"business"},
		"glossary":        {"人工知能=AI\n深層学習=Deep Learning"},
	}
	expectRedirect(t, adm.postForm(aiSettingsPath, postData), aiSettingsPath+"?flash=ai_saved")
	saved, ok := savedAI(t, env)
	if !ok || saved.WritingStyle != "business" {
		t.Fatalf("saved writing_style want business, got %+v", saved)
	}
	if !strings.Contains(saved.Glossary, "人工知能=AI") {
		t.Fatalf("saved glossary want 人工知能=AI, got %s", saved.Glossary)
	}

	// 3. creative スタイルの保存
	postData.Set("writing_style", "creative")
	expectRedirect(t, adm.postForm(aiSettingsPath, postData), aiSettingsPath+"?flash=ai_saved")
	saved, _ = savedAI(t, env)
	if saved.WritingStyle != "creative" {
		t.Fatalf("saved writing_style want creative, got %+v", saved)
	}

	// 4. academic スタイルの保存
	postData.Set("writing_style", "academic")
	expectRedirect(t, adm.postForm(aiSettingsPath, postData), aiSettingsPath+"?flash=ai_saved")
	saved, _ = savedAI(t, env)
	if saved.WritingStyle != "academic" {
		t.Fatalf("saved writing_style want academic, got %+v", saved)
	}

	// 5. standard スタイルの保存
	postData.Set("writing_style", "standard")
	expectRedirect(t, adm.postForm(aiSettingsPath, postData), aiSettingsPath+"?flash=ai_saved")
	saved, _ = savedAI(t, env)
	if saved.WritingStyle != "standard" {
		t.Fatalf("saved writing_style want standard, got %+v", saved)
	}

	// 6. DB上の JSON（writing_style が空のときは standard になる）の読み込み
	rawJSON := `{"provider":"custom","providers":{"custom":{"base_url":"` + mock.URL + `/v1"}}}`
	if err := upsertSetting(env.db, aiSettingKey, rawJSON); err != nil {
		t.Fatal(err)
	}
	loaded, ok := env.server.loadAIConfig()
	if !ok || loaded.WritingStyle != "standard" {
		t.Fatalf("loaded JSON without writing_style want standard, got %+v", loaded)
	}
}
