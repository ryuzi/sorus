package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// geminiError は Google API の 400 エラーの応答本文（error.status と error.details[].reason を持つ）。
func geminiError(code int, status, reason, message string) map[string]any {
	return map[string]any{"error": map[string]any{
		"code": code, "message": message, "status": status,
		"details": []any{map[string]any{
			"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
			"reason": reason, "domain": "googleapis.com",
		}},
	}}
}

func TestGeminiDefaultModels(t *testing.T) {
	if DefaultGeminiModel != "gemini-3.8-flash" || DefaultGeminiFallbackModel != "gemini-3.5-flash-lite" {
		t.Fatalf("default models = %q / %q", DefaultGeminiModel, DefaultGeminiFallbackModel)
	}
	g := NewGemini(Config{APIKey: "k"})
	if len(g.models) != 2 || g.models[0] != DefaultGeminiModel || g.models[1] != DefaultGeminiFallbackModel {
		t.Errorf("models = %v", g.models)
	}
	if g.Name() != "Gemini (gemini-3.8-flash)" {
		t.Errorf("Name = %q", g.Name())
	}
}

// Google AI Studio の新しい Auth キー（AQ. で始まる）も、従来のキーと同じく X-Goog-Api-Key ヘッダーで
// そのまま送る（形式で弾かない。前後の空白・改行は除く）。
func TestGeminiSendsAuthKeyInHeader(t *testing.T) {
	const key = "AQ.Ab8RN6Lx-example_auth.key-0123456789"
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		// 「案1」のような番号だけの行は parseTitles が取り除くため、実際のタイトルらしい行にする。
		writeJSON(w, http.StatusOK, geminiReply("春の読書会\n図書館だより"))
	})
	p, err := NewProvider(Config{Kind: KindGemini, APIKey: "  " + key + "\n", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !IsConfigured(p) {
		t.Fatal("AQ. key must configure Gemini")
	}
	titles, err := p.SuggestTitles(context.Background(), "本文")
	if err != nil || len(titles) != 2 {
		t.Fatalf("SuggestTitles = %v, %v", titles, err)
	}
	got := (*reqs)[0]
	if got.Header.Get("X-Goog-Api-Key") != key {
		t.Errorf("X-Goog-Api-Key = %q", got.Header.Get("X-Goog-Api-Key"))
	}
	if strings.Contains(got.Query, "AQ.") || got.Header.Get("Authorization") != "" {
		t.Errorf("key must be sent only in X-Goog-Api-Key: query=%q auth=%q", got.Query, got.Header.Get("Authorization"))
	}
	if got.Path != "/models/"+DefaultGeminiModel+":generateContent" {
		t.Errorf("path = %q", got.Path)
	}
}

// 再現テスト: Gemini はキーの無効・未認可に 400（API_KEY_INVALID・FAILED_PRECONDITION）を返す。これを
// キーの問題（IsAuthError）として判定し、400 ではフォールバックのモデルに切り替えない。
func TestGemini400IsAuthError(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"API_KEY_INVALID", geminiError(400, "INVALID_ARGUMENT", "API_KEY_INVALID", "API key not valid. Please pass a valid API key.")},
		{"FAILED_PRECONDITION", geminiError(400, "FAILED_PRECONDITION", "", "The API key is not yet bound to a service account.")},
		{"message only", map[string]any{"error": map[string]any{"code": 400, "message": "API key expired. Please renew the API key."}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				writeJSON(w, http.StatusBadRequest, tc.body)
			})
			p := NewGemini(Config{APIKey: "AQ.invalid", BaseURL: srv.URL})
			_, err := p.SuggestTitles(context.Background(), "本文")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("err = %v", err)
			}
			if !IsAuthError(err) {
				t.Errorf("IsAuthError(%v) = false", err)
			}
			if len(*reqs) != 1 {
				t.Errorf("a 400 must not retry with the fallback model: %d requests", len(*reqs))
			}
			// 生のエラー（ログに残す）には Google の status・reason を含める。キーは含めない。
			if strings.Contains(err.Error(), "AQ.invalid") {
				t.Errorf("error must not contain the key: %v", err)
			}
		})
	}

	srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusBadRequest, geminiError(400, "INVALID_ARGUMENT", "API_KEY_INVALID", "API key not valid."))
	})
	_, err := NewGemini(Config{APIKey: "k", BaseURL: srv.URL}).Summarize(context.Background(), "本文")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != "INVALID_ARGUMENT" || apiErr.Reason != "API_KEY_INVALID" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "API_KEY_INVALID") || !strings.Contains(err.Error(), "API key not valid") {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&APIError{StatusCode: 401}, true},
		{&APIError{StatusCode: 403}, true},
		{&APIError{StatusCode: 400, Reason: "API_KEY_INVALID"}, true},
		{&APIError{StatusCode: 400, Status: "FAILED_PRECONDITION"}, true},
		{&APIError{StatusCode: 400, Message: "Invalid JSON payload received."}, false},
		{&APIError{StatusCode: 429}, false},
		{&APIError{StatusCode: 500, Reason: "API_KEY_INVALID"}, false},
		{errors.New("API key"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := IsAuthError(tc.err); got != tc.want {
			t.Errorf("IsAuthError(%#v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestKeyEnvName(t *testing.T) {
	for kind, want := range map[string]string{
		KindGemini: "GEMINI_API_KEY", KindOpenAI: "OPENAI_API_KEY", KindClaude: "ANTHROPIC_API_KEY", KindCustom: "AI_API_KEY", "x": "",
	} {
		if got := KeyEnvName(kind); got != want {
			t.Errorf("KeyEnvName(%q) = %q, want %q", kind, got, want)
		}
	}
}
