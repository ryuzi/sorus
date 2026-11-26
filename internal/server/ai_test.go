package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"sorus/internal/ai"
)

func TestAIErrorStatusExplainsTruncationAndLostProtectedContent(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{ai.ErrTruncated, "AIの応答が途中で切れたため、保存していません。記事を短く分けてもう一度試してください"},
		{ai.ErrProtectedContentMissing, "翻訳でコードや画像の一部が失われたため、保存していません。もう一度試してください"},
	}
	for _, tc := range tests {
		status, message := aiErrorStatus(tc.err)
		if status != http.StatusBadGateway || message != tc.want {
			t.Errorf("aiErrorStatus(%v) = (%d, %q), want (%d, %q)", tc.err, status, message, http.StatusBadGateway, tc.want)
		}
	}
	if errors.Is(ai.ErrTruncated, ai.ErrProtectedContentMissing) {
		t.Fatal("AI error sentinels must remain distinct")
	}
}

// fakeAI は受け取った本文を記録し、決まった結果（または err）を返す ai.Provider。
type fakeAI struct {
	mu                sync.Mutex
	texts             []string
	err               error
	lastTranslateOpts ai.TranslateOptions
}

func (f *fakeAI) Name() string { return "Fake (test-model)" }

func (f *fakeAI) record(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return f.err
}

func (f *fakeAI) Proofread(_ context.Context, text string) (*ai.ProofreadResult, error) {
	if err := f.record(text); err != nil {
		return nil, err
	}
	improved := strings.ReplaceAll(text, "誤時", "誤字")
	return &ai.ProofreadResult{OriginalText: text, ImprovedText: improved, Diffs: ai.Diff(text, improved)}, nil
}

func (f *fakeAI) SuggestTitles(_ context.Context, text string) ([]string, error) {
	if err := f.record(text); err != nil {
		return nil, err
	}
	return []string{"案1", "案2", "案3", "案4", "案5"}, nil
}

func (f *fakeAI) Summarize(_ context.Context, text string) (string, error) {
	if err := f.record(text); err != nil {
		return "", err
	}
	return "読みたくなるリード文。", nil
}

func (f *fakeAI) Translate(_ context.Context, text string, _, _ string, opts ai.TranslateOptions) (string, error) {
	f.mu.Lock()
	f.lastTranslateOpts = opts
	f.mu.Unlock()
	if err := f.record(text); err != nil {
		return "", err
	}
	return "Translated: " + text, nil
}

func (f *fakeAI) GenerateDraft(_ context.Context, topic, notes string) (*ai.DraftResult, error) {
	if err := f.record(topic + "\n" + notes); err != nil {
		return nil, err
	}
	return &ai.DraftResult{Title: "生成タイトル", Slug: "generated-title", Content: "# 生成本文", Summary: "生成リード"}, nil
}

// aiResult は AI API の JSON レスポンスのうちテストで確かめる項目。
type aiResult struct {
	Configured   bool               `json:"configured"`
	Provider     string             `json:"provider"`
	Guide        string             `json:"guide"`
	OriginalText string             `json:"original_text"`
	ImprovedText string             `json:"improved_text"`
	Diffs        []ai.ProofreadDiff `json:"diffs"`
	DiffHTML     string             `json:"diff_html"`
	Changed      bool               `json:"changed"`
	Titles       []string           `json:"titles"`
	Summary      string             `json:"summary"`
	Message      string             `json:"message"`
}

func (c *client) postAI(action, text string) (*response, aiResult) {
	c.t.Helper()
	res := c.postAPI("/admin/ai/"+action, "application/x-www-form-urlencoded", strings.NewReader(url.Values{"text": {text}}.Encode()))
	var out aiResult
	if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
		c.t.Fatalf("POST /admin/ai/%s: response is not JSON (status %d): %v\n%s", action, res.StatusCode, err, res.Body)
	}
	return res, out
}

func TestAIEndpointsRequireAuthAndCSRF(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	for _, action := range []string{"proofread", "suggest-titles", "summarize"} {
		anon := env.newClient()
		res := anon.postAPI("/admin/ai/"+action, "application/x-www-form-urlencoded", strings.NewReader("text=x"))
		expectStatus(t, res, http.StatusUnauthorized)

		c := env.loggedIn(studentA)
		res = c.postRaw("/admin/ai/"+action, url.Values{"text": {"x"}})
		expectStatus(t, res, http.StatusForbidden)
	}
}

func TestAINotConfiguredReturnsGuide(t *testing.T) {
	env := newTestEnv(t) // Config.AI 省略時は ai.Dummy。
	c := env.loggedIn(studentA)
	for _, action := range []string{"proofread", "suggest-titles", "summarize"} {
		res, out := c.postAI(action, "本文")
		expectStatus(t, res, http.StatusOK)
		if out.Configured || !strings.Contains(out.Guide, "GEMINI_API_KEY") || !strings.Contains(out.Guide, ".env") {
			t.Errorf("%s: response = %+v", action, out)
		}
	}

	// エディタ（既存記事・新規作成とも）はツールバーを未設定として描画し、設定ガイドのモーダルを含める。
	id := c.createArticle("AI未設定", "本文")
	for _, path := range []string{"/admin/articles/" + id + "/edit", "/admin/articles/new"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `id="ai-toolbar"`, `data-ai-configured="false"`, "自動で推敲する", "タイトルを提案", "リード文（抜粋）を作成",
			`id="ai-modal"`, "環境変数に `GEMINI_API_KEY` を設定してください")
		// 未設定のときは「AI: 未設定」ではなく、控えめな設定リンクを出す。
		if strings.Contains(res.Body, "AI: 未設定") {
			t.Errorf("%s: toolbar must not show \"AI: 未設定\"", path)
		}
		if status := tagWithID(t, res.Body, "ai-status"); !strings.Contains(status, `href="/admin/settings/ai"`) {
			t.Errorf("%s: AI status = %s, want a link to the AI settings", path, status)
		}
	}
}

// AI ツールバーは「AI で下書き」「文章を整える ▾」「仕上げ・告知 ▾」の3つにまとめ、各ボタンの data-ai-action と
// #social-open-button はそのまま残す（desktop_quit_and_editor_ux_spec.md §1.3 (4)）。
func TestAIToolbarGroupsActions(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	c := env.loggedIn(studentA)
	id := c.createArticle("AIツールバー", "本文")
	res := c.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)

	start := strings.Index(res.Body, `id="ai-toolbar"`)
	if start < 0 {
		t.Fatal("ai-toolbar not found")
	}
	toolbar := res.Body[start:]
	toolbar = toolbar[:strings.Index(toolbar, `src="/static/js/ai.js`)]

	// メニューは2つ。それぞれの中に入る項目を確かめる。
	menus := strings.Split(toolbar, "<details")
	if len(menus) != 3 {
		t.Fatalf("toolbar has %d menus, want 2:\n%s", len(menus)-1, toolbar)
	}
	head, polish, finish := menus[0], menus[1], menus[2]
	if i := strings.Index(finish, "</details>"); i >= 0 {
		finish = finish[:i]
	}
	expectContains(t, head, `data-ai-action="draft"`, "AI で下書き")
	expectContains(t, polish, "data-ai-menu", "文章を整える", `data-ai-partial`,
		`data-ai-action="proofread"`, `data-ai-action="simplify"`, `data-ai-action="fact-check"`, `data-ai-action="translate"`)
	expectContains(t, finish, "data-ai-menu", "仕上げ・告知",
		`data-ai-action="suggest-titles"`, `data-ai-action="summarize"`,
		`id="social-open-button" data-social-open data-social-editor data-article-id="`+id+`"`)
	for _, action := range []string{"draft", "proofread", "simplify", "fact-check", "translate", "suggest-titles", "summarize"} {
		if n := strings.Count(toolbar, `data-ai-action="`+action+`"`); n != 1 {
			t.Errorf("data-ai-action=%q appears %d times, want 1", action, n)
		}
	}
	// 過去記事の参照と使う AI はメニューの外（右側）に残す。
	if !strings.Contains(toolbar[strings.LastIndex(toolbar, "</details>"):], `id="ai-use-rag"`) {
		t.Error("ai-use-rag must stay outside the menus")
	}

	// メニューを開閉するスクリプトを読み込み、配信できる。
	if !strings.Contains(res.Body, `<script src="/static/js/ai.js`) {
		t.Error("ai.js is not loaded")
	}
	js := c.get("/static/js/ai.js")
	expectStatus(t, js, http.StatusOK)
	// 「選択範囲をAI推敲」は「文章を整える ▾」の先頭の項目から、部分推敲の画面を直接開く（重複ボタンを押し直さない）。
	expectContains(t, js.Body, "data-ai-menu", "Escape", "data-ai-partial", "partial-translate-modal", "window.sorusOpenPartialTranslate('editor')")
	expectNotContains(t, js.Body, "partial-translate-open-btn")
	// 折り返しでメニューが画面の左右からはみ出さないよう、開いたときに寄せる向きを直す。
	expectContains(t, js.Body, "function fitPanel(", "getBoundingClientRect()", "fitPanel(panel)")
	if i, j := strings.Index(polish, `data-ai-partial`), strings.Index(polish, `data-ai-action="proofread"`); i < 0 || i > j {
		t.Error("the selection polish item should be the first item of the polish menu")
	}
	expectNotContains(t, res.Body, `id="partial-translate-open-btn"`)

	// 「文章を整える ▾」の項目は役割が重ならないよう、名前と一言の説明で分ける（ux_security_docs_final_polish_spec.md §1.3）。
	// 「選択範囲をAI推敲…」と「文章を推敲する」のような、どちらも「推敲」で区別のつかない名前は使わない。
	expectContains(t, polish,
		"指示・トーンを指定して直す…", "好みのトーンや自由な指示で書き直し",
		"自動で推敲する", "誤字脱字・文体・読みやすさをまとめて直す",
		"平易な表現に書き直す", "事実関係・裏取りを確認", "書き換えず、正しいかどうかだけを確かめる",
		"翻訳する", "本文を別の言語に翻訳")
	expectNotContains(t, polish, "選択範囲をAI推敲", "選択範囲をAI翻訳・推敲", "文章を推敲する")
	for _, item := range []string{"指示・トーンを指定して直す…", "自動で推敲する", "平易な表現に書き直す", "事実関係・裏取りを確認", "翻訳する"} {
		if n := strings.Count(polish, item); n != 1 {
			t.Errorf("polish menu item %q appears %d times, want 1", item, n)
		}
	}

	// 過去記事の参照・使う AI は #ai-toolbar-side にまとめ、sm 以上では1段目の右端・同じ高さに固定する。
	side := tagWithID(t, res.Body, "ai-toolbar-side")
	for _, want := range []string{"ml-auto", "h-8", "shrink-0", "whitespace-nowrap"} {
		if !strings.Contains(side, want) {
			t.Errorf("ai-toolbar-side should have %q: %s", want, side)
		}
	}
	if bar := tagWithID(t, res.Body, "ai-toolbar"); !strings.Contains(bar, "sm:grid-cols-[minmax(0,1fr)_auto]") {
		t.Errorf("ai-toolbar should keep the side block in its own column: %s", bar)
	}
	if strings.Index(toolbar, `id="ai-toolbar-side"`) > strings.Index(toolbar, `id="ai-use-rag"`) ||
		strings.Index(toolbar, `id="ai-toolbar-side"`) < strings.LastIndex(toolbar, "</details>") {
		t.Error("ai-use-rag should sit in ai-toolbar-side after the menus")
	}

	// 英語の画面ではメニューの名前も英語になる。
	c.get("/admin/set-lang?lang=en&return=/admin")
	en := c.get("/admin/articles/" + id + "/edit")
	expectStatus(t, en, http.StatusOK)
	expectContains(t, en.Body, "AI draft", "Polish text", "Finish &amp; share", "Suggest titles", "Create social posts",
		"Revise with your own instructions…", "Proofread automatically", "Rewrite in plain words", "Check facts and sources")
}

func TestAIProofread(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)

	res, out := c.postAI("proofread", "誤時脱字<b>\r\nを直す")
	expectStatus(t, res, http.StatusOK)
	if !out.Configured || out.Provider != "Fake (test-model)" || !out.Changed {
		t.Fatalf("response = %+v", out)
	}
	// CRLF は LF に揃えてから AI に渡す。
	if got := fake.texts[0]; got != "誤時脱字<b>\nを直す" {
		t.Errorf("text sent to provider = %q", got)
	}
	if out.ImprovedText != "誤字脱字<b>\nを直す" || len(out.Diffs) == 0 {
		t.Errorf("improved = %q diffs = %+v", out.ImprovedText, out.Diffs)
	}
	// 差分の HTML は本文をエスケープし、追加・削除だけをタグで囲む。
	want := `誤<del class="diff-del">時</del><ins class="diff-add">字</ins>脱字&lt;b&gt;` + "\n" + `を直す`
	if out.DiffHTML != want {
		t.Errorf("diff_html = %s\nwant      %s", out.DiffHTML, want)
	}

	// 変更が無ければ changed は false。
	_, out = c.postAI("proofread", "直すところの無い文章。")
	if out.Changed {
		t.Errorf("unchanged text reported as changed: %+v", out)
	}

	// 編集画面（既存記事・新規作成とも）のツールバーは設定済みとして描画する。
	id := c.createArticle("AI設定済み", "本文")
	for _, path := range []string{"/admin/articles/" + id + "/edit", "/admin/articles/new"} {
		page := c.get(path)
		expectStatus(t, page, http.StatusOK)
		expectContains(t, page.Body, `id="ai-toolbar"`, `data-ai-configured="true"`, "AI: Fake (test-model)")
	}
}

func TestAISuggestTitlesAndSummarize(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)

	res, out := c.postAI("suggest-titles", "記事本文")
	expectStatus(t, res, http.StatusOK)
	if len(out.Titles) != ai.TitleCount || out.Titles[0] != "案1" {
		t.Errorf("titles = %q", out.Titles)
	}

	res, out = c.postAI("summarize", "記事本文")
	expectStatus(t, res, http.StatusOK)
	if out.Summary != "読みたくなるリード文。" {
		t.Errorf("summary = %q", out.Summary)
	}
}

func TestAIInputValidation(t *testing.T) {
	fake := &fakeAI{}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)

	res, out := c.postAI("proofread", "  \n ")
	expectStatus(t, res, http.StatusBadRequest)
	if !strings.Contains(out.Message, "本文を入力") {
		t.Errorf("message = %q", out.Message)
	}

	res, _ = c.postAI("summarize", strings.Repeat("あ", maxAITextRunes+1))
	expectStatus(t, res, http.StatusRequestEntityTooLarge)

	res, _ = c.postAI("summarize", strings.Repeat("あ", maxAITextRunes))
	expectStatus(t, res, http.StatusOK)
	if len(fake.texts) != 1 {
		t.Errorf("provider called %d times, want 1 (invalid input must not reach the provider)", len(fake.texts))
	}
}

func TestAIProviderErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"rate limited", &ai.APIError{Provider: "Fake", StatusCode: 429, Message: "quota"}, http.StatusServiceUnavailable, "利用上限"},
		{"bad key", &ai.APIError{Provider: "Fake", StatusCode: 401, Message: "invalid key sk-xxx"}, http.StatusBadGateway, "APIキーが無効"},
		{"upstream 500", &ai.APIError{Provider: "Fake", StatusCode: 500, Message: "internal"}, http.StatusBadGateway, "再試行"},
		{"empty", ai.ErrEmptyResponse, http.StatusBadGateway, "有効な応答"},
		{"timeout", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "時間がかかりすぎ"},
		{"not configured", ai.ErrNotConfigured, http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnvWith(t, Config{AI: &fakeAI{err: tt.err}}, nil)
			c := env.loggedIn(studentA)
			res, out := c.postAI("proofread", "本文")
			expectStatus(t, res, tt.code)
			if !strings.Contains(out.Message, tt.msg) {
				t.Errorf("message = %q, want %q", out.Message, tt.msg)
			}
			// AI API の生のエラー本文（キーの断片などを含みうる）は利用者に返さない。
			if strings.Contains(res.Body, "sk-xxx") || strings.Contains(res.Body, "quota") {
				t.Errorf("raw provider error leaked: %s", res.Body)
			}
			if tt.code == http.StatusOK && (out.Configured || out.Guide == "") {
				t.Errorf("not configured response = %+v", out)
			}
		})
	}
}

// TestAIGeminiEndToEnd は実際の Gemini クライアントをモック API に向け、管理画面 API から推敲できることを確かめる。
func TestAIGeminiEndToEnd(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"新入生歓迎会が開かれました。"}]}}]}`)
	}))
	t.Cleanup(mock.Close)
	p, err := ai.NewProvider(ai.Config{Kind: ai.KindGemini, APIKey: "test-key", BaseURL: mock.URL})
	if err != nil {
		t.Fatal(err)
	}
	env := newTestEnvWith(t, Config{AI: p}, nil)
	c := env.loggedIn(studentA)

	res, out := c.postAI("proofread", "新入生歓迎会が開かれまた。")
	expectStatus(t, res, http.StatusOK)
	if out.ImprovedText != "新入生歓迎会が開かれました。" || !strings.Contains(out.DiffHTML, `<ins class="diff-add">し</ins>`) {
		t.Errorf("response = %+v", out)
	}
	if !strings.Contains(out.Provider, "Gemini") {
		t.Errorf("provider = %q", out.Provider)
	}
}

func TestAIToolbarHiddenInReadOnlyMode(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &fakeAI{}}, nil)
	a := env.loggedIn(studentA)
	b := env.loggedIn(studentB)
	id := a.createArticle("共同執筆", "本文")
	a.get("/admin/articles/" + id + "/edit") // A がロックを取る。

	res := b.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, lockWarningMessage)
	if strings.Contains(res.Body, `id="ai-toolbar"`) || strings.Contains(res.Body, `id="ai-modal"`) {
		t.Error("AI toolbar must not be shown in read-only mode")
	}
}

// 用語集は 5,000 文字まで、改行・タブ以外の制御文字は受け付けない。管理者・編集者・ライターのどの経路でも
// 422 で入力の誤りを返し、保存しない。
func TestAISettingsGlossaryValidation(t *testing.T) {
	clearAIEnv(t)
	mock := newCompatMock(t, "案A")
	env := newTestEnv(t)
	base := url.Values{"provider": {"custom"}, "base_url_custom": {mock.URL + "/v1/"}, "model_custom": {"llama3.1:8b"}}
	form := func(glossary string) url.Values {
		f := url.Values{}
		for k, v := range base {
			f[k] = v
		}
		f.Set("glossary", glossary)
		return f
	}
	tooLong := strings.Repeat("語", maxAIGlossaryRunes+1)
	for _, u := range []testUser{admin, chief, studentA} {
		c := env.loggedIn(u)
		for _, tc := range []struct {
			name, glossary, msg string
		}{
			{"too long", tooLong, "用語集が長すぎます（5000文字まで"},
			{"nul", "AI=人工知能\x00", "使えない制御文字"},
			{"escape", "AI=\x1b[31m人工知能", "使えない制御文字"},
			{"del", "AI=人工知能\x7f", "使えない制御文字"},
		} {
			res := c.postForm(aiSettingsPath, form(tc.glossary))
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, res.Body, tc.msg)
			if _, ok := savedAI(t, env); ok {
				t.Fatalf("%s/%s: invalid glossary was saved", u.Username, tc.name)
			}
		}
	}

	// ちょうど 5,000 文字（マルチバイト）と、改行・タブを含む用語集は保存できる。
	exact := strings.Repeat("語", maxAIGlossaryRunes)
	expectRedirect(t, env.loggedIn(admin).postForm(aiSettingsPath, form(exact)), aiSettingsPath+"?flash=ai_saved")
	if saved, ok := savedAI(t, env); !ok || saved.Glossary != exact {
		t.Fatalf("glossary of exactly %d runes was not saved", maxAIGlossaryRunes)
	}
	multiline := "AI=人工知能\r\nLLM\t大規模言語モデル"
	expectRedirect(t, env.loggedIn(chief).postForm(aiSettingsPath, url.Values{"glossary": {multiline}}),
		aiSettingsPath+"?flash=ai_saved")
	if saved, ok := savedAI(t, env); !ok || saved.Glossary != multiline {
		t.Fatalf("glossary with newline and tab = %q, want %q", saved.Glossary, multiline)
	}
}

// 管理者以外（編集者・ライター）の用語集の入力の誤りの画面（422）も、通常の画面と同じく、キーの出どころ・環境変数の
// 名前・ローカル LLM の送り先を出さない（redactForNonAdmin）。見出しも「AI執筆支援設定」のまま。
func TestAISettingsGlossaryErrorRedactsForNonAdmin(t *testing.T) {
	clearAIEnv(t)
	t.Setenv("GEMINI_API_KEY", "AIza-env-key-for-test")
	mock := newCompatMock(t, "案A")
	env := newTestEnv(t)
	// 管理者がローカル LLM（custom）の送り先を保存しておく。
	expectRedirect(t, env.loggedIn(admin).postForm(aiSettingsPath, url.Values{"provider": {"custom"},
		"base_url_custom": {mock.URL + "/v1/"}, "model_custom": {"llama3.1:8b"}}), aiSettingsPath+"?flash=ai_saved")

	bad := url.Values{"glossary": {"AI=人工知能\x00"}}
	res := env.loggedIn(admin).postForm(aiSettingsPath, bad)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, `id="ai-env-keys"`, "GEMINI_API_KEY")

	for _, u := range []testUser{chief, studentA} {
		res := env.loggedIn(u).postForm(aiSettingsPath, bad)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "使えない制御文字", "<title>AI執筆支援設定")
		expectNotContains(t, res.Body, `id="ai-env-keys"`, "GEMINI_API_KEY", mock.URL, "この画面で保存したキー", "環境変数 ")
	}
}
