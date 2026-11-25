package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordedRequest は模擬サーバーが受け取ったリクエスト。
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   map[string]any
}

// mockServer は handler で応答し、受け取ったリクエストを記録する httptest サーバーを返す。
func mockServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body map[string]any)) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("request body is not JSON: %v: %s", err, data)
		}
		mu.Lock()
		reqs = append(reqs, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// vectorFor はテスト用に、テキストの長さと位置から決まる2次元のベクトルを返す。
func vectorFor(text string, i int) []float32 {
	return []float32{float32(len([]rune(text))), float32(i + 1)}
}

func mustEmbedder(t *testing.T, cfg EmbedderConfig) Embedder {
	t.Helper()
	e, err := NewEmbedder(cfg)
	if err != nil {
		t.Fatalf("NewEmbedder(%+v): %v", cfg, err)
	}
	return e
}

// geminiHandler は batchEmbedContents の模擬。モデルごとの応答ステータスを notFound で切り替える。
func geminiHandler(t *testing.T, notFound map[string]bool) func(w http.ResponseWriter, r *http.Request, body map[string]any) {
	return func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		model := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/models/"), ":batchEmbedContents")
		if notFound[model] {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
				"code": 404, "message": "models/" + model + " is not found for API version v1beta", "status": "NOT_FOUND"}})
			return
		}
		reqs, _ := body["requests"].([]any)
		var embeddings []map[string]any
		for i, r := range reqs {
			req := r.(map[string]any)
			text := req["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
			embeddings = append(embeddings, map[string]any{"values": vectorFor(text, i)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"embeddings": embeddings})
	}
}

func TestGeminiEmbedder(t *testing.T) {
	srv, requests := mockServer(t, geminiHandler(t, nil))
	e := mustEmbedder(t, EmbedderConfig{Kind: KindGemini, APIKey: "secret-key", BaseURL: srv.URL + "/"})
	if got, want := e.Name(), "Gemini (text-embedding-004)"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got := e.Model(); got != DefaultGeminiEmbeddingModel {
		t.Errorf("Model = %q, want %q", got, DefaultGeminiEmbeddingModel)
	}

	res, err := e.Embed(context.Background(), []string{"量子", "計算機科学"}, TaskDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if res.Model != DefaultGeminiEmbeddingModel {
		t.Errorf("Model = %q, want %q", res.Model, DefaultGeminiEmbeddingModel)
	}
	if fmt.Sprint(res.Vectors) != "[[2 1] [5 2]]" {
		t.Errorf("Vectors = %v, want [[2 1] [5 2]]", res.Vectors)
	}

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/models/text-embedding-004:batchEmbedContents" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	if got := r.Header.Get("X-Goog-Api-Key"); got != "secret-key" {
		t.Errorf("X-Goog-Api-Key = %q, want secret-key", got)
	}
	if strings.Contains(r.Query, "secret-key") {
		t.Errorf("API key leaked into the query string: %q", r.Query)
	}
	first := r.Body["requests"].([]any)[0].(map[string]any)
	if first["model"] != "models/text-embedding-004" || first["taskType"] != "RETRIEVAL_DOCUMENT" || first["outputDimensionality"] != float64(768) {
		t.Errorf("request entry = %v, want model, RETRIEVAL_DOCUMENT and outputDimensionality 768", first)
	}

	if _, err := e.Embed(context.Background(), []string{"検索語"}, TaskQuery); err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	if q := requests()[1].Body["requests"].([]any)[0].(map[string]any); q["taskType"] != "RETRIEVAL_QUERY" {
		t.Errorf("query taskType = %v, want RETRIEVAL_QUERY", q["taskType"])
	}
}

func TestGeminiEmbedderBatches(t *testing.T) {
	srv, requests := mockServer(t, geminiHandler(t, nil))
	e := mustEmbedder(t, EmbedderConfig{Kind: KindGemini, APIKey: "k", BaseURL: srv.URL})
	texts := make([]string, 250)
	for i := range texts {
		texts[i] = strings.Repeat("あ", i+1)
	}
	res, err := e.Embed(context.Background(), texts, TaskDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	var sizes []int
	for _, r := range requests() {
		sizes = append(sizes, len(r.Body["requests"].([]any)))
	}
	if fmt.Sprint(sizes) != "[100 100 50]" {
		t.Errorf("batch sizes = %v, want [100 100 50]", sizes)
	}
	if len(res.Vectors) != 250 {
		t.Fatalf("got %d vectors, want 250", len(res.Vectors))
	}
	// バッチをまたいでも入力と同じ順に並ぶ。
	for i, v := range res.Vectors {
		if int(v[0]) != i+1 {
			t.Fatalf("vector %d = %v, want it to belong to text %d", i, v, i)
		}
	}
}

func TestGeminiEmbedderFallsBackWhenModelNotFound(t *testing.T) {
	srv, requests := mockServer(t, geminiHandler(t, map[string]bool{DefaultGeminiEmbeddingModel: true}))
	e := mustEmbedder(t, EmbedderConfig{Kind: KindGemini, APIKey: "k", BaseURL: srv.URL})
	res, err := e.Embed(context.Background(), []string{"本文"}, TaskDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if res.Model != DefaultGeminiEmbeddingFallbackModel {
		t.Errorf("Model = %q, want fallback %q", res.Model, DefaultGeminiEmbeddingFallbackModel)
	}
	reqs := requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].Path, DefaultGeminiEmbeddingFallbackModel) {
		t.Fatalf("requests = %+v, want default model then fallback", reqs)
	}
	if got := e.Model(); got != DefaultGeminiEmbeddingFallbackModel {
		t.Errorf("Model() after fallback = %q, want %q", got, DefaultGeminiEmbeddingFallbackModel)
	}
	// 以降は見つからなかったモデルを試さず、後継モデルから始める。
	if res, err := e.Embed(context.Background(), []string{"次"}, TaskQuery); err != nil || res.Model != DefaultGeminiEmbeddingFallbackModel {
		t.Fatalf("second Embed = %+v, %v", res, err)
	}
	if n := len(requests()); n != 3 {
		t.Errorf("got %d requests in total, want 3 (no retry of the missing model)", n)
	}
	// 後継モデルでも次元数を揃える。
	if d := reqs[1].Body["requests"].([]any)[0].(map[string]any)["outputDimensionality"]; d != float64(768) {
		t.Errorf("fallback outputDimensionality = %v, want 768", d)
	}

	// モデルを明示したときは、見つからなくても別のモデルに切り替えない。
	srv2, requests2 := mockServer(t, geminiHandler(t, map[string]bool{"my-model": true}))
	e = mustEmbedder(t, EmbedderConfig{Kind: KindGemini, APIKey: "k", BaseURL: srv2.URL, Model: "my-model", Dimensions: 256})
	_, err = e.Embed(context.Background(), []string{"本文"}, TaskDocument)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || !strings.Contains(apiErr.Message, "not found") {
		t.Errorf("Embed with missing explicit model: err = %v, want *APIError 404", err)
	}
	if n := len(requests2()); n != 1 {
		t.Errorf("got %d requests, want 1 (no fallback)", n)
	}
	if d := requests2()[0].Body["requests"].([]any)[0].(map[string]any)["outputDimensionality"]; d != float64(256) {
		t.Errorf("outputDimensionality = %v, want configured 256", d)
	}
}

func TestOpenAIEmbedder(t *testing.T) {
	srv, requests := mockServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		input := body["input"].([]any)
		// index の逆順で返し、クライアントが並べ直すことを確かめる。
		var data []map[string]any
		for i := len(input) - 1; i >= 0; i-- {
			data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vectorFor(input[i].(string), i)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "model": "text-embedding-3-small"})
	})
	e := mustEmbedder(t, EmbedderConfig{Kind: KindOpenAI, APIKey: "sk-test", BaseURL: srv.URL})
	if got, want := e.Name(), "OpenAI (text-embedding-3-small)"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	res, err := e.Embed(context.Background(), []string{"a", "bb", "ccc"}, TaskQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if res.Model != DefaultOpenAIEmbeddingModel || e.Model() != DefaultOpenAIEmbeddingModel || fmt.Sprint(res.Vectors) != "[[1 1] [2 2] [3 3]]" {
		t.Errorf("Embed = %s %v, want input order", res.Model, res.Vectors)
	}
	r := requests()[0]
	if r.Path != "/embeddings" || r.Header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("request = %s with Authorization %q", r.Path, r.Header.Get("Authorization"))
	}
	if r.Body["model"] != "text-embedding-3-small" || r.Body["encoding_format"] != "float" {
		t.Errorf("body = %v", r.Body)
	}
	if _, ok := r.Body["dimensions"]; ok {
		t.Errorf("dimensions sent without configuration: %v", r.Body)
	}

	e = mustEmbedder(t, EmbedderConfig{Kind: KindOpenAI, APIKey: "sk-test", BaseURL: srv.URL, Dimensions: 512})
	if _, err := e.Embed(context.Background(), []string{"a"}, TaskDocument); err != nil {
		t.Fatal(err)
	}
	if d := requests()[1].Body["dimensions"]; d != float64(512) {
		t.Errorf("dimensions = %v, want 512", d)
	}
}

func TestOpenAIEmbedderRejectsBadIndexes(t *testing.T) {
	for name, indexes := range map[string][]int{"duplicate": {0, 0}, "out of range": {0, 2}, "missing": {0}} {
		t.Run(name, func(t *testing.T) {
			srv, _ := mockServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				var data []map[string]any
				for _, i := range indexes {
					data = append(data, map[string]any{"index": i, "embedding": []float32{1, 2}})
				}
				writeJSON(w, http.StatusOK, map[string]any{"data": data})
			})
			e := mustEmbedder(t, EmbedderConfig{Kind: KindOpenAI, APIKey: "k", BaseURL: srv.URL})
			if _, err := e.Embed(context.Background(), []string{"a", "b"}, TaskDocument); !errors.Is(err, ErrInvalidEmbeddings) {
				t.Errorf("err = %v, want ErrInvalidEmbeddings", err)
			}
		})
	}
}

func TestOllamaEmbedder(t *testing.T) {
	srv, requests := mockServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		var embeddings [][]float32
		for i, in := range body["input"].([]any) {
			embeddings = append(embeddings, vectorFor(in.(string), i))
		}
		writeJSON(w, http.StatusOK, map[string]any{"model": "nomic-embed-text:latest", "embeddings": embeddings})
	})
	e := mustEmbedder(t, EmbedderConfig{Kind: KindOllama, BaseURL: srv.URL})
	if got, want := e.Name(), "Ollama (nomic-embed-text)"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	texts := make([]string, 40)
	for i := range texts {
		texts[i] = "x"
	}
	res, err := e.Embed(context.Background(), texts, TaskDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if res.Model != DefaultOllamaEmbeddingModel || e.Model() != DefaultOllamaEmbeddingModel || len(res.Vectors) != 40 {
		t.Errorf("Embed = %s with %d vectors, want %s with 40", res.Model, len(res.Vectors), DefaultOllamaEmbeddingModel)
	}
	reqs := requests()
	if len(reqs) != 2 || reqs[0].Path != "/api/embed" || reqs[0].Body["model"] != "nomic-embed-text" {
		t.Errorf("requests = %+v, want 2 batches to /api/embed", reqs)
	}
	if reqs[0].Header.Get("Authorization") != "" {
		t.Errorf("Ollama request has Authorization header")
	}
}

func TestEmbedderEmptyInputSkipsRequest(t *testing.T) {
	srv, requests := mockServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		t.Error("API called for empty input")
	})
	for _, kind := range []string{KindGemini, KindOpenAI, KindOllama} {
		e := mustEmbedder(t, EmbedderConfig{Kind: kind, APIKey: "k", BaseURL: srv.URL})
		res, err := e.Embed(context.Background(), nil, TaskDocument)
		if err != nil || res.Model == "" || len(res.Vectors) != 0 {
			t.Errorf("%s: Embed(nil) = %+v, %v, want empty result with model", kind, res, err)
		}
	}
	if n := len(requests()); n != 0 {
		t.Errorf("got %d requests, want 0", n)
	}
}

func TestEmbedderAPIErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantMsg string
	}{
		{"nested error", http.StatusUnauthorized, `{"error": {"message": "API key not valid"}}`, "API key not valid"},
		{"plain error", http.StatusBadRequest, `{"error": "model \"x\" not found, try pulling it first"}`, `model "x" not found`},
		{"text body", http.StatusBadGateway, "upstream unavailable", "upstream unavailable"},
		{"long body", http.StatusInternalServerError, strings.Repeat("エ", 200), "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := mockServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			for _, kind := range []string{KindGemini, KindOpenAI, KindOllama} {
				e := mustEmbedder(t, EmbedderConfig{Kind: kind, APIKey: "secret-key", BaseURL: srv.URL})
				_, err := e.Embed(context.Background(), []string{"a"}, TaskDocument)
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || !strings.Contains(apiErr.Message, tc.wantMsg) {
					t.Errorf("%s: err = %v, want *APIError %d containing %q", kind, err, tc.status, tc.wantMsg)
					continue
				}
				if len(apiErr.Message) > maxErrorMessage+len("…") || strings.Contains(err.Error(), "secret-key") {
					t.Errorf("%s: error message too long or leaks the key: %q", kind, err)
				}
			}
		})
	}
}

func TestEmbedderRejectsInvalidVectors(t *testing.T) {
	cases := map[string]any{
		"count mismatch":       [][]float32{{1, 2}},
		"empty vector":         [][]float32{{1, 2}, {}},
		"dimension mismatch":   [][]float32{{1, 2}, {1, 2, 3}},
		"dimension mismatch 2": [][]float32{{1}, {1, 2}},
	}
	for name, embeddings := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := mockServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				writeJSON(w, http.StatusOK, map[string]any{"embeddings": embeddings})
			})
			e := mustEmbedder(t, EmbedderConfig{Kind: KindOllama, BaseURL: srv.URL})
			if _, err := e.Embed(context.Background(), []string{"a", "b"}, TaskDocument); !errors.Is(err, ErrInvalidEmbeddings) {
				t.Errorf("err = %v, want ErrInvalidEmbeddings", err)
			}
		})
	}
	if err := validateVectors([][]float32{{1, float32(math.NaN())}}); !errors.Is(err, ErrInvalidEmbeddings) {
		t.Errorf("validateVectors with NaN: err = %v, want ErrInvalidEmbeddings", err)
	}
}

func TestEmbedderHonorsContext(t *testing.T) {
	srv, _ := mockServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		<-r.Context().Done()
	})
	e := mustEmbedder(t, EmbedderConfig{Kind: KindGemini, APIKey: "k", BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Embed(ctx, []string{"a"}, TaskDocument); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestNewEmbedder(t *testing.T) {
	notConfigured := []EmbedderConfig{
		{},
		{Kind: KindGemini},
		{Kind: KindOpenAI},
	}
	for _, cfg := range notConfigured {
		if e, err := NewEmbedder(cfg); !errors.Is(err, ErrNotConfigured) || e != nil {
			t.Errorf("NewEmbedder(%+v) = %v, %v, want ErrNotConfigured", cfg, e, err)
		}
	}
	if _, err := NewEmbedder(EmbedderConfig{Kind: "cohere", APIKey: "k"}); err == nil || errors.Is(err, ErrNotConfigured) {
		t.Errorf("unknown kind: err = %v, want an error other than ErrNotConfigured", err)
	}
	if _, err := NewEmbedder(EmbedderConfig{Kind: KindGemini, APIKey: "k", Dimensions: -1}); err == nil {
		t.Error("negative dimensions accepted")
	}
	// OpenAI 互換のローカルサーバーは API キー無しでも使える。Ollama は常に使える。
	for _, cfg := range []EmbedderConfig{{Kind: KindOpenAI, BaseURL: "http://localhost:1234/v1"}, {Kind: KindOllama}} {
		if _, err := NewEmbedder(cfg); err != nil {
			t.Errorf("NewEmbedder(%+v): %v", cfg, err)
		}
	}
}

func TestEmbedderConfigFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want EmbedderConfig
	}{
		{"nothing", nil, EmbedderConfig{}},
		{"gemini key", map[string]string{"GEMINI_API_KEY": " g "}, EmbedderConfig{Kind: KindGemini, APIKey: "g"}},
		{"openai key", map[string]string{"OPENAI_API_KEY": "o"}, EmbedderConfig{Kind: KindOpenAI, APIKey: "o"}},
		{"gemini preferred", map[string]string{"GEMINI_API_KEY": "g", "OPENAI_API_KEY": "o"}, EmbedderConfig{Kind: KindGemini, APIKey: "g"}},
		{"follows AI_PROVIDER", map[string]string{"AI_PROVIDER": "OpenAI", "GEMINI_API_KEY": "g", "OPENAI_API_KEY": "o"},
			EmbedderConfig{Kind: KindOpenAI, APIKey: "o"}},
		{"claude chat uses gemini embeddings", map[string]string{"AI_PROVIDER": "claude", "ANTHROPIC_API_KEY": "a", "GEMINI_API_KEY": "g"},
			EmbedderConfig{Kind: KindGemini, APIKey: "g"}},
		{"AI_API_KEY for same provider", map[string]string{"AI_PROVIDER": "google", "AI_API_KEY": "x"}, EmbedderConfig{Kind: KindGemini, APIKey: "x"}},
		{"explicit provider wins", map[string]string{"EMBEDDING_PROVIDER": "ollama", "AI_PROVIDER": "gemini", "GEMINI_API_KEY": "g",
			"EMBEDDING_MODEL": "mxbai-embed-large", "EMBEDDING_BASE_URL": "http://gpu:11434"},
			EmbedderConfig{Kind: KindOllama, Model: "mxbai-embed-large", BaseURL: "http://gpu:11434"}},
		{"embedding key wins", map[string]string{"EMBEDDING_PROVIDER": "gemini", "EMBEDDING_API_KEY": "e", "GEMINI_API_KEY": "g",
			"EMBEDDING_DIMENSIONS": "256"}, EmbedderConfig{Kind: KindGemini, APIKey: "e", Dimensions: 256}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := embedderConfigFromEnv(func(k string) string { return tc.env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", tc.want) {
				t.Errorf("config = %+v, want %+v", got, tc.want)
			}
		})
	}
	for _, dims := range []string{"abc", "0", "-5"} {
		env := map[string]string{"GEMINI_API_KEY": "g", "EMBEDDING_DIMENSIONS": dims}
		if _, err := embedderConfigFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Errorf("EMBEDDING_DIMENSIONS=%q accepted, want error", dims)
		}
	}
}

func TestNewEmbedderFromEnv(t *testing.T) {
	for _, k := range []string{"EMBEDDING_PROVIDER", "EMBEDDING_API_KEY", "EMBEDDING_MODEL", "EMBEDDING_BASE_URL",
		"EMBEDDING_DIMENSIONS", "AI_PROVIDER", "AI_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	if _, err := NewEmbedderFromEnv(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("NewEmbedderFromEnv without settings: err = %v, want ErrNotConfigured", err)
	}
	t.Setenv("GEMINI_API_KEY", "g")
	e, err := NewEmbedderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*GeminiEmbedder); !ok {
		t.Errorf("NewEmbedderFromEnv = %T, want *GeminiEmbedder", e)
	}
}
