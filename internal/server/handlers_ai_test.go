package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"sorus/internal/ai"
)

type phase7TestAI struct{ fakeAI }

func (*phase7TestAI) FactCheck(_ context.Context, text string) (*ai.FactCheckResult, error) {
	return &ai.FactCheckResult{Summary: "確認結果", Grounded: true, Claims: []ai.FactCheckClaim{{
		Claim: text, Verdict: ai.VerdictInaccurate, Explanation: "出典と一致しません", Suggestion: "訂正文",
		Sources: []ai.Source{{Title: "大学公式", URL: "https://example.edu/news"}},
	}}, Sources: []ai.Source{{Title: "大学公式", URL: "https://example.edu/news"}}, SearchEntryPoint: "<div>Search</div>"}, nil
}

func (*phase7TestAI) Simplify(_ context.Context, text, _ string) (*ai.ProofreadResult, error) {
	return &ai.ProofreadResult{OriginalText: text, ImprovedText: "やさしい文章", Diffs: ai.Diff(text, "やさしい文章")}, nil
}
func (*phase7TestAI) GenerateAbstract(_ context.Context, _ string) (string, error) {
	return "要旨", nil
}
func (*phase7TestAI) Translate(_ context.Context, _ string, _, _ string, _ ai.TranslateOptions) (string, error) {
	return "Translated", nil
}
func (*phase7TestAI) StyleCheck(_ context.Context, text, _ string) (*ai.ProofreadResult, error) {
	return &ai.ProofreadResult{OriginalText: text, ImprovedText: text, Diffs: ai.Diff(text, text)}, nil
}

func TestAIPhase7Endpoints(t *testing.T) {
	env := newTestEnvWith(t, Config{AI: &phase7TestAI{}}, nil)
	c := env.loggedIn(studentA)
	post := func(path string, values url.Values) *response {
		t.Helper()
		values.Set("text", "本文")
		return c.postAPI(path, "application/x-www-form-urlencoded", strings.NewReader(values.Encode()))
	}
	for _, path := range []string{"/admin/ai/fact-check", "/admin/ai/academic"} {
		anon := env.newClient()
		res := anon.postAPI(path, "application/x-www-form-urlencoded", strings.NewReader("text=本文"))
		expectStatus(t, res, http.StatusUnauthorized)
	}
	res := post("/admin/ai/fact-check", url.Values{"use_rag": {"true"}})
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"verdict":"inaccurate"`, `"suggestion":"訂正文"`, `"search_entry_point":"\u003cdiv\u003eSearch\u003c/div\u003e"`)
	res = post("/admin/ai/academic", url.Values{"action": {"simplify"}})
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"improved_text":"やさしい文章"`, `"changed":true`)
	res = post("/admin/ai/academic", url.Values{"action": {"translate"}, "target_lang": {"en"}})
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"text":"Translated"`, `"target_lang":"en"`)
	res = post("/admin/ai/academic", url.Values{"action": {"unknown"}})
	expectStatus(t, res, http.StatusBadRequest)
	id := c.createArticle("AIツール", "本文")
	res = c.get("/admin/articles/" + id + "/edit")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "裏取り", "わかりやすく", "翻訳", "自サイトの過去記事を参照する")
}

func TestAIDraftEndpoint(t *testing.T) {
	// AI 未設定時は guide を返すこと
	unconfiguredEnv := newTestEnv(t)
	anon := unconfiguredEnv.loggedIn(studentA)
	resUnconf := anon.postAPI("/admin/ai/draft", "application/x-www-form-urlencoded", strings.NewReader(url.Values{"topic": {"テーマ"}}.Encode()))
	expectStatus(t, resUnconf, http.StatusOK)
	var gotUnconf struct {
		Configured bool   `json:"configured"`
		Guide      string `json:"guide"`
	}
	if err := json.Unmarshal([]byte(resUnconf.Body), &gotUnconf); err != nil {
		t.Fatal(err)
	}
	if gotUnconf.Configured || gotUnconf.Guide == "" {
		t.Fatalf("expected unconfigured guide, got %+v", gotUnconf)
	}

	fake := &fakeAI{}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)

	// 正常系: topic + notes + context
	res := c.postAPI("/admin/ai/draft", "application/x-www-form-urlencoded", strings.NewReader(url.Values{
		"topic":   {"春の活動"},
		"notes":   {"- 4月に開催\n- 参加者は20人"},
		"context": {"親しみやすい語り口で"},
	}.Encode()))
	expectStatus(t, res, http.StatusOK)
	var got struct {
		Configured bool           `json:"configured"`
		Draft      ai.DraftResult `json:"draft"`
	}
	if err := json.Unmarshal([]byte(res.Body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Configured || got.Draft.Title != "生成タイトル" || got.Draft.Content != "# 生成本文" || got.Draft.Summary != "生成リード" {
		t.Fatalf("draft response = %+v", got)
	}

	// notes が空でも topic があれば成功すること
	resTopicOnly := c.postAPI("/admin/ai/draft", "application/x-www-form-urlencoded", strings.NewReader(url.Values{"topic": {"春の活動"}}.Encode()))
	expectStatus(t, resTopicOnly, http.StatusOK)

	// topic が空の場合は 400
	resEmptyTopic := c.postAPI("/admin/ai/draft", "application/x-www-form-urlencoded", strings.NewReader("topic=&notes=%E3%83%A1%E3%83%A2"))
	expectStatus(t, resEmptyTopic, http.StatusBadRequest)

	// 2万字を超える入力は 413
	hugeNotes := strings.Repeat("あ", 20001)
	resTooLarge := c.postAPI("/admin/ai/draft", "application/x-www-form-urlencoded", strings.NewReader(url.Values{"topic": {"テーマ"}, "notes": {hugeNotes}}.Encode()))
	expectStatus(t, resTooLarge, http.StatusRequestEntityTooLarge)
}

func TestRAGSearchEndpointUnavailableWithoutConfiguration(t *testing.T) {
	env := newTestEnv(t)
	anon := env.newClient()
	expectStatus(t, anon.get("/admin/api/rag/search?q=article"), http.StatusSeeOther)
	c := env.loggedIn(studentA)
	res := c.get("/admin/api/rag/search?q=article")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `"configured":false`, `"results":[]`)
}
