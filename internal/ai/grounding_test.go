package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// segment は part の中で sub が現れる範囲を、grounding_supports の segment（バイト位置）にする。
func segment(t *testing.T, partIndex int, part, sub string) map[string]any {
	t.Helper()
	start := strings.Index(part, sub)
	if start < 0 {
		t.Fatalf("%q not found in part %d", sub, partIndex)
	}
	return map[string]any{"partIndex": partIndex, "startIndex": start, "endIndex": start + len(sub), "text": sub}
}

// geminiSystem はモック API が受け取った Gemini リクエストの system プロンプトを返す。
func geminiSystem(body map[string]any) string {
	return body["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
}

// geminiUser はモック API が受け取った Gemini リクエストのユーザーのテキストを返す。
func geminiUser(body map[string]any) string {
	return body["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
}

const factCheckArticle = "東京大学は1877年に創立された。富士山の標高は3,000メートルである。学内サークルの部員は120人だ。\n"

func TestGeminiFactCheckGrounded(t *testing.T) {
	// 応答を2つのパートに分け、コードブロックで囲む。grounding の位置はパートごとのバイト位置になる。
	part0 := "```json\n" + `{"summary": "富士山の標高に誤りがあります。", "claims": [` +
		`{"claim": "東京大学は1877年に創立された", "verdict": "verified", "explanation": "東京大学の公式サイトの沿革と一致しています。", "suggestion": "",` +
		` "sources": [{"title": "モデルが作ったURL", "url": "https://hallucinated.example/"}]}, `
	part1 := `{"claim": "富士山の標高は3,000メートル", "verdict": "Inaccurate", "explanation": "国土地理院によると標高は3,776メートルです。", "suggestion": "富士山の標高は3,776メートル"}, ` +
		`{"claim": "学内サークルの部員は120人", "verdict": "unknown", "explanation": "公開情報が見つかりませんでした。", "suggestion": ""}]}` + "\n```"
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": part0}, map[string]any{"text": part1},
			}},
			"groundingMetadata": map[string]any{
				"webSearchQueries": []any{"東京大学 創立年", "富士山 標高", " "},
				"groundingChunks": []any{
					map[string]any{"web": map[string]any{"uri": "https://www.u-tokyo.ac.jp/history", "title": "東京大学 沿革"}},
					map[string]any{"web": map[string]any{"uri": "https://www.gsi.go.jp/fuji", "title": ""}},
					map[string]any{"web": map[string]any{"uri": "https://news.example.jp/summary", "title": "ニュース\n記事"}},
					map[string]any{"web": map[string]any{"uri": "javascript:alert(1)", "title": "危険"}},
					map[string]any{"web": map[string]any{"uri": "https://www.u-tokyo.ac.jp/history", "title": "重複"}},
				},
				"groundingSupports": []any{
					map[string]any{"segment": segment(t, 0, part0, "東京大学の公式サイトの沿革と一致しています。"), "groundingChunkIndices": []any{0, 3, 99}, "confidenceScores": []any{0.9, 0.8, 0.1}},
					map[string]any{"segment": segment(t, 1, part1, "国土地理院によると標高は3,776メートルです。"), "groundingChunkIndices": []any{1}},
					map[string]any{"segment": segment(t, 0, part0, "富士山の標高に誤りがあります。"), "groundingChunkIndices": []any{2}},
				},
				"searchEntryPoint": map[string]any{"renderedContent": "<div class=\"search\">検索候補</div>"},
			},
		}}})
	})
	p, err := NewProvider(Config{Kind: KindGemini, APIKey: "gem-secret", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.(FactChecker).FactCheck(context.Background(), factCheckArticle)
	if err != nil {
		t.Fatalf("FactCheck: %v", err)
	}

	// リクエストは Google 検索のツールを有効にする。
	got := (*reqs)[0]
	if tools, _ := json.Marshal(got.Body["tools"]); string(tools) != `[{"google_search":{}}]` {
		t.Errorf("tools = %s", tools)
	}
	if sys := geminiSystem(got.Body); !strings.Contains(sys, "ファクトチェッカー") || !strings.Contains(sys, "Google 検索") {
		t.Errorf("system prompt = %q", sys)
	}
	if user := geminiUser(got.Body); user != factCheckArticle {
		t.Errorf("user text = %q", user)
	}

	if !res.Grounded || res.Summary != "富士山の標高に誤りがあります。" {
		t.Errorf("grounded=%v summary=%q", res.Grounded, res.Summary)
	}
	if want := []string{"東京大学 創立年", "富士山 標高"}; !reflect.DeepEqual(res.SearchQueries, want) {
		t.Errorf("SearchQueries = %q, want %q", res.SearchQueries, want)
	}
	if res.SearchEntryPoint != `<div class="search">検索候補</div>` {
		t.Errorf("SearchEntryPoint = %q", res.SearchEntryPoint)
	}
	utokyo := Source{Title: "東京大学 沿革", URL: "https://www.u-tokyo.ac.jp/history"}
	gsi := Source{Title: "www.gsi.go.jp", URL: "https://www.gsi.go.jp/fuji"} // タイトルが無ければホスト名。
	news := Source{Title: "ニュース 記事", URL: "https://news.example.jp/summary"}
	// javascript: の URL は除き、重複する URL は1件にする。
	if want := []Source{utokyo, gsi, news}; !reflect.DeepEqual(res.Sources, want) {
		t.Errorf("Sources = %+v, want %+v", res.Sources, want)
	}

	want := []FactCheckClaim{
		// モデルが書いた URL は使わず、グラウンディングの検索結果だけを根拠にする。
		{Claim: "東京大学は1877年に創立された", Verdict: VerdictVerified, Explanation: "東京大学の公式サイトの沿革と一致しています。", Sources: []Source{utokyo}},
		{Claim: "富士山の標高は3,000メートル", Verdict: VerdictInaccurate, Explanation: "国土地理院によると標高は3,776メートルです。",
			Suggestion: "富士山の標高は3,776メートル", Sources: []Source{gsi}},
		// サマリーの範囲の裏付けは、どの主張にも割り当てない。
		{Claim: "学内サークルの部員は120人", Verdict: VerdictUnverified, Explanation: "公開情報が見つかりませんでした。", Sources: []Source{}},
	}
	if !reflect.DeepEqual(res.Claims, want) {
		t.Errorf("Claims =\n%+v\nwant\n%+v", res.Claims, want)
	}

	// 根拠の無い主張も sources は null ではなく空の配列で返す（画面の JavaScript が扱いやすいように）。
	data, _ := json.Marshal(res)
	if !strings.Contains(string(data), `"sources":[]`) || strings.Contains(string(data), "hallucinated") {
		t.Errorf("JSON = %s", data)
	}
}

func TestGeminiFactCheckSnakeCaseMetadata(t *testing.T) {
	answer := `{"summary": "確認しました。", "claims": [{"claim": "東京大学は1877年に創立された", "verdict": "verified", "explanation": "公式の沿革と一致しています。"}]}`
	srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		seg := segment(t, 0, answer, "公式の沿革と一致しています。")
		writeJSON(w, http.StatusOK, map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{"text": answer}}},
			"grounding_metadata": map[string]any{
				"web_search_queries": []any{"東京大学 創立"},
				"grounding_chunks":   []any{map[string]any{"web": map[string]any{"uri": "https://www.u-tokyo.ac.jp/", "title": "東京大学"}}},
				"grounding_supports": []any{map[string]any{
					"segment":                 map[string]any{"start_index": seg["startIndex"], "end_index": seg["endIndex"], "text": seg["text"]},
					"grounding_chunk_indices": []any{0},
				}},
				"search_entry_point": map[string]any{"rendered_content": "<div>候補</div>"},
			},
		}}})
	})
	p := NewGemini(Config{APIKey: "k", BaseURL: srv.URL})
	res, err := p.FactCheck(context.Background(), factCheckArticle)
	if err != nil {
		t.Fatalf("FactCheck: %v", err)
	}
	if !res.Grounded || len(res.SearchQueries) != 1 || res.SearchEntryPoint != "<div>候補</div>" {
		t.Errorf("result = %+v", res)
	}
	if len(res.Claims) != 1 || !reflect.DeepEqual(res.Claims[0].Sources, []Source{{Title: "東京大学", URL: "https://www.u-tokyo.ac.jp/"}}) {
		t.Errorf("Claims = %+v", res.Claims)
	}
}

func TestGeminiFactCheckMatchesSupportsByText(t *testing.T) {
	// 主張の外にも "claim" キーがあると、"claim" キーの数が主張の数と合わず位置では割り当てられないため、
	// 裏付けの範囲のテキストで割り当てる。
	answer := `{"summary": "", "format": {"claim": "形式の例"}, "claims": [` +
		`{"claim": "ポスターには定員50名と書かれていた", "verdict": "unverified", "explanation": "ポスターの画像を確認できませんでした。"}, ` +
		`{"claim": "富士山の標高は3,000メートル", "verdict": "false", "explanation": "国土地理院によると標高は3,776メートルです。"}]}`
	srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{"text": answer}}},
			"groundingMetadata": map[string]any{
				"groundingChunks": []any{
					map[string]any{"web": map[string]any{"uri": "https://www.gsi.go.jp/", "title": "国土地理院"}},
					map[string]any{"web": map[string]any{"uri": "https://event.example/poster", "title": "イベント告知"}},
					map[string]any{"web": map[string]any{"uri": "https://other.example/", "title": "その他"}},
				},
				"groundingSupports": []any{
					map[string]any{"segment": segment(t, 0, answer, `"explanation": "国土地理院によると標高は3,776メートルです。"`), "groundingChunkIndices": []any{0}},
					// 位置の無い範囲は、判定の根拠の一部と一致すれば割り当てる。
					map[string]any{"segment": map[string]any{"text": "ポスターの画像を確認できません"}, "groundingChunkIndices": []any{1}},
					// 短すぎる範囲（「。」）はどの主張にも割り当てない。
					map[string]any{"segment": map[string]any{"text": "。"}, "groundingChunkIndices": []any{2}},
				},
			},
		}}})
	})
	res, err := NewGemini(Config{APIKey: "k", BaseURL: srv.URL}).FactCheck(context.Background(), factCheckArticle)
	if err != nil {
		t.Fatalf("FactCheck: %v", err)
	}
	if len(res.Claims) != 2 {
		t.Fatalf("Claims = %+v", res.Claims)
	}
	if !reflect.DeepEqual(res.Claims[0].Sources, []Source{{Title: "イベント告知", URL: "https://event.example/poster"}}) {
		t.Errorf("claim 0 sources = %+v", res.Claims[0].Sources)
	}
	if !reflect.DeepEqual(res.Claims[1].Sources, []Source{{Title: "国土地理院", URL: "https://www.gsi.go.jp/"}}) || res.Claims[1].Verdict != VerdictInaccurate {
		t.Errorf("claim 1 = %+v", res.Claims[1])
	}
	// サマリーが空なら判定の件数から作る。
	if res.Summary != "確認済み 0件・要確認 1件・要修正 1件" {
		t.Errorf("Summary = %q", res.Summary)
	}
}

func TestGeminiFactCheckFallsBackWithoutGrounding(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if body["tools"] != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "Search Grounding is not supported."}})
			return
		}
		writeJSON(w, http.StatusOK, geminiReply(`{"summary": "知識による判定です。", "claims": [{"claim": "東京大学は1877年に創立された", "verdict": "verified", "explanation": "一般に知られた事実です。"}]}`))
	})
	res, err := NewGemini(Config{APIKey: "k", BaseURL: srv.URL}).FactCheck(context.Background(), factCheckArticle)
	if err != nil {
		t.Fatalf("FactCheck: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (grounded, then fallback)", len(*reqs))
	}
	if (*reqs)[1].Body["tools"] != nil {
		t.Errorf("fallback request must not use tools: %v", (*reqs)[1].Body["tools"])
	}
	if sys := geminiSystem((*reqs)[1].Body); !strings.Contains(sys, "Web 検索を利用できません") {
		t.Errorf("fallback system prompt = %q", sys)
	}
	if res.Grounded || len(res.Sources) != 0 || len(res.Claims) != 1 || res.Claims[0].Verdict != VerdictVerified {
		t.Errorf("result = %+v", res)
	}
}

func TestGeminiFactCheckDoesNotFallBackOnOtherErrors(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "quota exceeded"}})
	})
	_, err := NewGemini(Config{APIKey: "k", BaseURL: srv.URL}).FactCheck(context.Background(), factCheckArticle)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want 429 APIError", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("requests = %d, want 1", len(*reqs))
	}
}

func TestFactCheckWithoutGrounding(t *testing.T) {
	answer := "```json\n" + `{"summary": "1件の誤りがあります。", "claims": [` +
		`{"claim": "富士山の標高は3,000メートル", "verdict": "誤り", "explanation": "標高は3,776メートルです。", "suggestion": "富士山の標高は3,776メートル",` +
		` "sources": [{"title": "存在しないページ", "url": "https://hallucinated.example/"}]},` +
		`{"claim": "  ", "verdict": "verified"},` +
		`{"claim": "東京大学は1877年に創立された", "verdict": "correct", "explanation": "一般に知られた事実です。", "suggestion": "東京大学は1877年に創立された"}]}` + "\n```"
	for _, tc := range []struct {
		kind  string
		reply map[string]any
	}{
		{KindOpenAI, openAIReply(answer)},
		{KindClaude, claudeReply(answer)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				writeJSON(w, http.StatusOK, tc.reply)
			})
			p, err := NewProvider(Config{Kind: tc.kind, APIKey: "k", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			res, err := p.(FactChecker).FactCheck(context.Background(), factCheckArticle)
			if err != nil {
				t.Fatalf("FactCheck: %v", err)
			}
			if body, _ := json.Marshal((*reqs)[0].Body); !strings.Contains(string(body), "Web 検索を利用できません") {
				t.Errorf("request must use the ungrounded prompt: %s", body)
			}
			want := []FactCheckClaim{
				{Claim: "富士山の標高は3,000メートル", Verdict: VerdictInaccurate, Explanation: "標高は3,776メートルです。",
					Suggestion: "富士山の標高は3,776メートル", Sources: []Source{}},
				// 引用と同じ修正案は捨てる。空の引用は飛ばす。
				{Claim: "東京大学は1877年に創立された", Verdict: VerdictVerified, Explanation: "一般に知られた事実です。", Sources: []Source{}},
			}
			if res.Grounded || res.Summary != "1件の誤りがあります。" || len(res.Sources) != 0 || !reflect.DeepEqual(res.Claims, want) {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestFactCheckUnstructuredResponse(t *testing.T) {
	for _, tc := range []struct {
		name, reply, summary string
	}{
		{"prose", "記事の内容に明らかな誤りは見当たりませんでした。", "記事の内容に明らかな誤りは見当たりませんでした。"},
		{"broken JSON", `{"summary": "途中で切れた`, `{"summary": "途中で切れた`},
		{"no claims", `{"claims": []}`, "検証が必要な事実の記述は見つかりませんでした。"},
		// JSON の前後に文章や {} が付いていても、最初の JSON オブジェクトを読む。
		{"surrounded by prose", "結果は次のとおりです。\n{\"summary\": \"誤りはありません。\", \"claims\": []}\n補足: {なし}", "誤りはありません。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				writeJSON(w, http.StatusOK, openAIReply(tc.reply))
			})
			res, err := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL}).FactCheck(context.Background(), factCheckArticle)
			if err != nil {
				t.Fatalf("FactCheck: %v", err)
			}
			if res.Summary != tc.summary || res.Claims == nil || len(res.Claims) != 0 {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestNormalizeVerdict(t *testing.T) {
	for in, want := range map[string]string{
		"verified": VerdictVerified, " Verified ": VerdictVerified, "true": VerdictVerified, "確認済み": VerdictVerified,
		"inaccurate": VerdictInaccurate, "FALSE": VerdictInaccurate, "incorrect": VerdictInaccurate, "要修正": VerdictInaccurate,
		"unverified": VerdictUnverified, "": VerdictUnverified, "partially true": VerdictUnverified,
	} {
		if got := normalizeVerdict(in); got != want {
			t.Errorf("normalizeVerdict(%q) = %q, want %q", in, got, want)
		}
	}
}
