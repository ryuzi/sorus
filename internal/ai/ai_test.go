package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded はモックサーバーが受け取ったリクエスト。
type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   map[string]any
}

// mockAPI は handler の応答を返し、受け取ったリクエストを記録するモック AI API を起動する。
func mockAPI(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body map[string]any)) (*httptest.Server, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("request body is not JSON: %v\n%s", err, data)
		}
		mu.Lock()
		reqs = append(reqs, recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func geminiReply(text string) map[string]any {
	return map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
	}}}
}

func openAIReply(text string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}}}}
}

func claudeReply(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

// joinDiffs は差分から元テキスト（skip=DiffInsert）または推敲後のテキスト（skip=DiffDelete）を組み立て直す。
func joinDiffs(diffs []ProofreadDiff, skip int) string {
	var b strings.Builder
	for _, d := range diffs {
		if d.Type != skip {
			b.WriteString(d.Text)
		}
	}
	return b.String()
}

func TestGeminiProofread(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, geminiReply("```\n今日は良い天気です。\n```"))
	})
	p, err := NewProvider(Config{Kind: KindGemini, APIKey: "gem-secret", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !IsConfigured(p) || !strings.Contains(p.Name(), DefaultGeminiModel) {
		t.Fatalf("provider = %q, configured=%v", p.Name(), IsConfigured(p))
	}

	res, err := p.Proofread(context.Background(), "今日は良い天気でした。\n")
	if err != nil {
		t.Fatalf("Proofread: %v", err)
	}
	// コードブロックの囲みは外し、元テキスト末尾の改行は引き継ぐ。
	if res.ImprovedText != "今日は良い天気です。\n" {
		t.Errorf("ImprovedText = %q", res.ImprovedText)
	}
	want := []ProofreadDiff{
		{DiffEqual, "今日は良い天気で"},
		{DiffDelete, "した"},
		{DiffInsert, "す"},
		{DiffEqual, "。\n"},
	}
	if !equalDiffs(res.Diffs, want) {
		t.Errorf("Diffs = %+v, want %+v", res.Diffs, want)
	}

	got := (*reqs)[0]
	if got.Path != "/models/"+DefaultGeminiModel+":generateContent" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Header.Get("X-Goog-Api-Key") != "gem-secret" || strings.Contains(got.Query, "gem-secret") {
		t.Errorf("API key must be sent in the header only: header=%q query=%q", got.Header.Get("X-Goog-Api-Key"), got.Query)
	}
	sys := got.Body["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(sys, "優秀な編集者") {
		t.Errorf("system prompt = %q", sys)
	}
	user := got.Body["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]
	if user != "今日は良い天気でした。\n" {
		t.Errorf("user text = %q", user)
	}
}

func TestGeminiFallsBackToSecondaryModelOn404(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if strings.Contains(r.URL.Path, DefaultGeminiModel) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "model not found"}})
			return
		}
		writeJSON(w, http.StatusOK, geminiReply("要約です。"))
	})
	p := NewGemini(Config{APIKey: "k", BaseURL: srv.URL})
	got, err := p.Summarize(context.Background(), "本文")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got != "要約です。" {
		t.Errorf("summary = %q", got)
	}
	if len(*reqs) != 2 || !strings.Contains((*reqs)[1].Path, DefaultGeminiFallbackModel) {
		t.Errorf("requests = %+v", *reqs)
	}
}

func TestOpenAISuggestTitles(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, openAIReply("タイトル案：\n1. 「春の図書館で出会う一冊」\n2) 文学部生が選ぶ名作10選\n・**読書の秋を120%楽しむ方法**\n- 文学部生が選ぶ名作10選\n④ 本と珈琲と私\n5．なぜ今、紙の本なのか\n6. 余分な6案目"))
	})
	p, err := NewProvider(Config{Kind: KindOpenAI, APIKey: "sk-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	titles, err := p.SuggestTitles(context.Background(), "本文")
	if err != nil {
		t.Fatalf("SuggestTitles: %v", err)
	}
	want := []string{"春の図書館で出会う一冊", "文学部生が選ぶ名作10選", "読書の秋を120%楽しむ方法", "本と珈琲と私", "なぜ今、紙の本なのか"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Errorf("titles = %q, want %q", titles, want)
	}

	got := (*reqs)[0]
	if got.Path != "/chat/completions" || got.Header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("path=%q auth=%q", got.Path, got.Header.Get("Authorization"))
	}
	if got.Body["model"] != DefaultOpenAIModel {
		t.Errorf("model = %v", got.Body["model"])
	}
	msgs := got.Body["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || !strings.Contains(msgs[0].(map[string]any)["content"].(string), "厳密に5案") {
		t.Errorf("system message = %v", msgs[0])
	}
	if msgs[1].(map[string]any)["role"] != "user" || msgs[1].(map[string]any)["content"] != "本文" {
		t.Errorf("user message = %v", msgs[1])
	}
}

func TestClaudeSummarize(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, claudeReply("  読み進めたくなるリード文。  \n"))
	})
	p, err := NewProvider(Config{Kind: KindClaude, APIKey: "sk-ant", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Summarize(context.Background(), "本文")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got != "読み進めたくなるリード文。" {
		t.Errorf("summary = %q", got)
	}

	r := (*reqs)[0]
	if r.Path != "/messages" || r.Header.Get("X-Api-Key") != "sk-ant" || r.Header.Get("Anthropic-Version") != claudeAPIVersion {
		t.Errorf("path=%q headers=%v", r.Path, r.Header)
	}
	if r.Body["model"] != DefaultClaudeModel || r.Body["max_tokens"] == nil {
		t.Errorf("body = %v", r.Body)
	}
	if !strings.Contains(r.Body["system"].(string), "リード文") {
		t.Errorf("system = %v", r.Body["system"])
	}
}

func TestCustomProviderWithoutAPIKey(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, openAIReply("推敲後の文章です。"))
	})
	p, err := NewProvider(Config{Kind: KindCustom, BaseURL: srv.URL + "/v1/", Model: "llama3.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Name(), "llama3.1") {
		t.Errorf("name = %q", p.Name())
	}
	res, err := p.Proofread(context.Background(), "推敲前の文章です。")
	if err != nil {
		t.Fatalf("Proofread: %v", err)
	}
	if res.ImprovedText != "推敲後の文章です。" || !HasChanges(res.Diffs) {
		t.Errorf("result = %+v", res)
	}
	r := (*reqs)[0]
	if r.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "" || r.Body["model"] != "llama3.1" {
		t.Errorf("path=%q auth=%q model=%v", r.Path, r.Header.Get("Authorization"), r.Body["model"])
	}
}

func TestAPIErrorDoesNotLeakKey(t *testing.T) {
	srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "quota exceeded"}})
	})
	for _, kind := range []string{KindGemini, KindOpenAI, KindClaude, KindCustom} {
		p, _ := NewProvider(Config{Kind: kind, APIKey: "very-secret-key", BaseURL: srv.URL})
		_, err := p.SuggestTitles(context.Background(), "本文")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Message != "quota exceeded" {
			t.Errorf("%s: err = %v", kind, err)
			continue
		}
		if strings.Contains(err.Error(), "very-secret-key") {
			t.Errorf("%s: error leaks API key: %v", kind, err)
		}
	}
}

func TestEmptyResponse(t *testing.T) {
	srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, openAIReply("   "))
	})
	p := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL})
	if _, err := p.Proofread(context.Background(), "本文"); !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("err = %v, want ErrEmptyResponse", err)
	}
}

func TestTimeout(t *testing.T) {
	srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	p, _ := NewProvider(Config{Kind: KindClaude, APIKey: "k", BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	if _, err := p.Summarize(context.Background(), "本文"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDummyProviderFallback(t *testing.T) {
	p, err := NewProvider(Config{Kind: KindGemini})
	if err != nil {
		t.Fatal(err)
	}
	if IsConfigured(p) {
		t.Fatal("provider without API key must not be configured")
	}
	d, ok := p.(*Dummy)
	if !ok {
		t.Fatalf("provider = %T, want *Dummy", p)
	}
	if !strings.Contains(d.SetupGuide(), "GEMINI_API_KEY") || !strings.Contains(d.SetupGuide(), ".env") {
		t.Errorf("guide = %q", d.SetupGuide())
	}
	ctx := context.Background()
	if _, err := p.Proofread(ctx, "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Proofread err = %v", err)
	}
	if _, err := p.SuggestTitles(ctx, "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("SuggestTitles err = %v", err)
	}
	if _, err := p.Summarize(ctx, "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Summarize err = %v", err)
	}

	if p, _ := NewProvider(Config{Kind: KindClaude}); p.(*Dummy).KeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("claude without key: %+v", p)
	}
	if p, err := NewProvider(Config{Kind: "bard"}); err == nil || IsConfigured(p) {
		t.Errorf("unknown kind: p=%v err=%v", p, err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{"nothing", nil, Config{MissingKeyEnv: "GEMINI_API_KEY"}},
		{"gemini auto", map[string]string{"GEMINI_API_KEY": "g", "OPENAI_API_KEY": "o"}, Config{Kind: KindGemini, APIKey: "g"}},
		{"openai auto", map[string]string{"OPENAI_API_KEY": "o", "ANTHROPIC_API_KEY": "a"}, Config{Kind: KindOpenAI, APIKey: "o"}},
		{"claude auto", map[string]string{"ANTHROPIC_API_KEY": "a"}, Config{Kind: KindClaude, APIKey: "a"}},
		{"custom auto", map[string]string{"AI_BASE_URL": "http://localhost:11434/v1", "AI_MODEL": "qwen"},
			Config{Kind: KindCustom, BaseURL: "http://localhost:11434/v1", Model: "qwen"}},
		{"explicit provider", map[string]string{"AI_PROVIDER": "Anthropic", "GEMINI_API_KEY": "g", "ANTHROPIC_API_KEY": "a"}, Config{Kind: KindClaude, APIKey: "a"}},
		{"generic key wins", map[string]string{"AI_PROVIDER": "openai", "OPENAI_API_KEY": "o", "AI_API_KEY": "x"}, Config{Kind: KindOpenAI, APIKey: "x"}},
		{"ollama alias", map[string]string{"AI_PROVIDER": "ollama", "AI_BASE_URL": "http://h/v1", "AI_API_KEY": "x"}, Config{Kind: KindCustom, APIKey: "x", BaseURL: "http://h/v1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configFromEnv(func(k string) string { return tt.env[k] })
			if got != tt.want {
				t.Errorf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func equalDiffs(a, b []ProofreadDiff) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		want     []ProofreadDiff
		wantNone bool
	}{
		{name: "identical", a: "同じ文章。\n", b: "同じ文章。\n", want: []ProofreadDiff{{DiffEqual, "同じ文章。\n"}}},
		{name: "empty", a: "", b: "", wantNone: true},
		{name: "insert only", a: "", b: "追加", want: []ProofreadDiff{{DiffInsert, "追加"}}},
		{name: "delete only", a: "削除", b: "", want: []ProofreadDiff{{DiffDelete, "削除"}}},
		{name: "typo", a: "誤時脱字を直す", b: "誤字脱字を直す",
			want: []ProofreadDiff{{DiffEqual, "誤"}, {DiffDelete, "時"}, {DiffInsert, "字"}, {DiffEqual, "脱字を直す"}}},
		{name: "english words", a: "the quick brown fox", b: "the slow brown fox",
			want: []ProofreadDiff{{DiffEqual, "the "}, {DiffDelete, "quick"}, {DiffInsert, "slow"}, {DiffEqual, " brown fox"}}},
		{name: "multi line", a: "# 見出し\n\n一行目です。\n二行目でした。\n", b: "# 見出し\n\n一行目です。\n二行目です。\n三行目。\n",
			want: []ProofreadDiff{{DiffEqual, "# 見出し\n\n一行目です。\n二行目で"}, {DiffDelete, "した"}, {DiffInsert, "す。\n三行目"}, {DiffEqual, "。\n"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Diff(tt.a, tt.b)
			if tt.wantNone {
				if len(got) != 0 {
					t.Errorf("Diff = %+v, want none", got)
				}
				return
			}
			if !equalDiffs(got, tt.want) {
				t.Errorf("Diff = %+v\nwant   %+v", got, tt.want)
			}
		})
	}
}

// TestDiffReconstructs は差分から元テキストと推敲後のテキストを正確に組み立て直せることを、様々な入力で確かめる。
func TestDiffReconstructs(t *testing.T) {
	pairs := [][2]string{
		{"abc", "axc"},
		{"春はあけぼの。やうやう白くなりゆく山ぎは。", "春はあけぼの。ようやく白くなっていく山際。"},
		{"a\nb\nc\nd\n", "a\nc\nb\nd"},
		{"one two three", "three two one"},
		{"寿司を食べた", "寿司と茶を飲んだ"},
		{strings.Repeat("同じ行です。\n", 300) + "最後の行", "最初に追加\n" + strings.Repeat("同じ行です。\n", 300) + "最後の行です"},
	}
	for _, p := range pairs {
		d := Diff(p[0], p[1])
		if got := joinDiffs(d, DiffInsert); got != p[0] {
			t.Errorf("original not reconstructed for %q: %q", p[0], got)
		}
		if got := joinDiffs(d, DiffDelete); got != p[1] {
			t.Errorf("improved not reconstructed for %q: %q", p[1], got)
		}
		for i := 1; i < len(d); i++ {
			if d[i].Type == d[i-1].Type {
				t.Errorf("adjacent chunks of the same type are not merged: %+v", d)
			}
		}
	}
}

// TestDiffLargeRewriteFallsBack は編集距離の上限を超える全面書き換えでも、正しい差分を素早く返すことを確かめる。
func TestDiffLargeRewriteFallsBack(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 5000; i++ {
		a.WriteString("あ")
		a.WriteString(string(rune('a'+i%26)) + "\n")
		b.WriteString("い")
		b.WriteString(string(rune('A'+i%26)) + "\n")
	}
	start := time.Now()
	d := Diff(a.String(), b.String())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Diff took %v", elapsed)
	}
	if joinDiffs(d, DiffInsert) != a.String() || joinDiffs(d, DiffDelete) != b.String() {
		t.Error("large rewrite not reconstructed")
	}
}

func TestRenderDiffHTML(t *testing.T) {
	got := RenderDiffHTML([]ProofreadDiff{
		{DiffEqual, "<b>"},
		{DiffDelete, "古い&"},
		{DiffInsert, `新しい"`},
	})
	want := `&lt;b&gt;<del class="diff-del">古い&amp;</del><ins class="diff-add">新しい&#34;</ins>`
	if got != want {
		t.Errorf("RenderDiffHTML = %s\nwant %s", got, want)
	}
}

func TestParseTitles(t *testing.T) {
	input := `1. 「A」と「B」の違い
2. 『夏の読書』
3) "Quoted"
4: 2025年の最新トレンド徹底解説
案5: 学祭レポート
① サークル新歓のコツ
1.
`
	got := parseTitles(input)
	want := []string{"「A」と「B」の違い", "夏の読書", "Quoted", "2025年の最新トレンド徹底解説", "学祭レポート"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("parseTitles = %q, want %q", got, want)
	}
}

func TestClaudeGenerateSocialPosts(t *testing.T) {
	reply := "告知文を作成しました。\n```json\n" +
		`{"x_thread": ["春の図書館で出会った一冊 #読書", "続きはこちら https://media.example.ac.jp/articles/a"],` +
		` "instagram": "春の図書館", "line": "新着記事です", "hashtags": ["#読書", "#学生メディア"]}` + "\n```"
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, claudeReply(reply))
	})
	p, err := NewProvider(Config{Kind: KindClaude, APIKey: "sk-ant-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	writer, ok := p.(SocialPostWriter)
	if !ok {
		t.Fatal("Claude provider does not implement SocialPostWriter")
	}
	posts, err := writer.GenerateSocialPosts(context.Background(), SocialPostRequest{
		Title: "春の図書館", Category: "キャンパス", SiteName: "学生メディア", URL: "https://media.example.ac.jp/articles/a", Text: "本文",
	})
	if err != nil {
		t.Fatalf("GenerateSocialPosts: %v", err)
	}
	if len(posts.XThread) != 2 || posts.Instagram != "春の図書館" || posts.LINE != "新着記事です" || len(posts.Hashtags) != 2 {
		t.Errorf("posts = %+v", posts)
	}
	body := (*reqs)[0].Body
	if !strings.Contains(body["system"].(string), "x_thread") {
		t.Errorf("system prompt = %v", body["system"])
	}
	user := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, want := range []string{"メディア名: 学生メディア", "カテゴリ: キャンパス", "タイトル: 春の図書館", "記事URL: https://media.example.ac.jp/articles/a", "本文:\n本文"} {
		if !strings.Contains(user, want) {
			t.Errorf("user message %q does not contain %q", user, want)
		}
	}
}

func TestParseSocialPostsRejectsInvalidResponses(t *testing.T) {
	for _, out := range []string{"", "告知文です", `{"x_thread": []}`, `{"x_thread": [`} {
		if _, err := parseSocialPosts(out); !errors.Is(err, ErrEmptyResponse) {
			t.Errorf("parseSocialPosts(%q) error = %v, want ErrEmptyResponse", out, err)
		}
	}
	if _, ok := Provider(NewDummy("")).(SocialPostWriter); ok {
		t.Error("Dummy should not implement SocialPostWriter (it is never configured)")
	}
}

func TestTranslateOnAllProviders(t *testing.T) {
	ctx := context.Background()
	sampleText := "# タイトル\n\n数式 $E=mc^2$ と燃料デブリの測定。\n"
	opts := TranslateOptions{
		WritingStyle: WritingStyleAcademic,
		Glossary:     map[string]string{"燃料デブリ": "fuel debris"},
	}

	// 1. Gemini
	t.Run("Gemini", func(t *testing.T) {
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, geminiReply("# Title\n\nFormula __SORUS_MASK_0__ and measurement of fuel debris.\n"))
		})
		p := NewGemini(Config{APIKey: "k", BaseURL: srv.URL})
		out, err := p.Translate(ctx, sampleText, "ja", "en", opts)
		if err != nil {
			t.Fatalf("Gemini.Translate: %v", err)
		}
		if !strings.Contains(out, "$E=mc^2$") {
			t.Errorf("formula not restored: %s", out)
		}
	})

	// 2. OpenAI
	t.Run("OpenAI", func(t *testing.T) {
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, openAIReply("# Title\n\nFormula __SORUS_MASK_0__ and measurement.\n"))
		})
		p := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL})
		out, err := p.Translate(ctx, sampleText, "ja", "en", opts)
		if err != nil {
			t.Fatalf("OpenAI.Translate: %v", err)
		}
		if !strings.Contains(out, "$E=mc^2$") {
			t.Errorf("formula not restored: %s", out)
		}
	})

	// 3. Claude
	t.Run("Claude", func(t *testing.T) {
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, claudeReply("# Title\n\nFormula __SORUS_MASK_0__ and measurement.\n"))
		})
		p := NewClaude(Config{APIKey: "k", BaseURL: srv.URL})
		out, err := p.Translate(ctx, sampleText, "ja", "en", opts)
		if err != nil {
			t.Fatalf("Claude.Translate: %v", err)
		}
		if !strings.Contains(out, "$E=mc^2$") {
			t.Errorf("formula not restored: %s", out)
		}
	})

	// 4. Custom (OpenAI 互換)
	t.Run("Custom", func(t *testing.T) {
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, openAIReply("# Title\n\nFormula __SORUS_MASK_0__ and measurement.\n"))
		})
		p := NewCustom(Config{BaseURL: srv.URL})
		out, err := p.Translate(ctx, sampleText, "ja", "en", opts)
		if err != nil {
			t.Fatalf("Custom.Translate: %v", err)
		}
		if !strings.Contains(out, "$E=mc^2$") {
			t.Errorf("formula not restored: %s", out)
		}
	})

	// 5. Dummy
	t.Run("Dummy", func(t *testing.T) {
		d := NewDummy("GEMINI_API_KEY")
		_, err := d.Translate(ctx, sampleText, "ja", "en", opts)
		if !errors.Is(err, ErrNotConfigured) {
			t.Errorf("Dummy.Translate err = %v, want ErrNotConfigured", err)
		}
	})
}

func TestProvidersRejectTruncatedResponses(t *testing.T) {
	tests := []struct {
		name  string
		kind  string
		reply map[string]any
	}{
		{name: "Claude max_tokens", kind: KindClaude, reply: map[string]any{"content": []any{map[string]any{"type": "text", "text": "途中"}}, "stop_reason": "max_tokens"}},
		{name: "OpenAI length", kind: KindOpenAI, reply: map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "途中"}, "finish_reason": "length"}}}},
		{name: "Gemini MAX_TOKENS", kind: KindGemini, reply: map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "途中"}}}, "finishReason": "MAX_TOKENS"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := mockAPI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				writeJSON(w, http.StatusOK, tc.reply)
			})
			p, err := NewProvider(Config{Kind: tc.kind, APIKey: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Summarize(context.Background(), "本文")
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("Summarize error = %v, want ErrTruncated", err)
			}
		})
	}
}

func TestTranslateRejectsMissingProtectedMask(t *testing.T) {
	srv, _ := mockAPI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		writeJSON(w, http.StatusOK, openAIReply("翻訳された文章です。"))
	})
	p := NewOpenAI(Config{APIKey: "test", BaseURL: srv.URL})
	_, err := p.Translate(context.Background(), "説明文 `keep this code` の続き", LangJapanese, LangEnglish, TranslateOptions{})
	if !errors.Is(err, ErrProtectedContentMissing) {
		t.Fatalf("Translate error = %v, want ErrProtectedContentMissing", err)
	}
}
