package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Embeddings のプロバイダーの種類（EMBEDDING_PROVIDER の値）。
const (
	KindGemini = "gemini"
	KindOpenAI = "openai"
	KindOllama = "ollama"
)

// 各プロバイダーの既定のモデルと API のベース URL。EMBEDDING_MODEL・EMBEDDING_BASE_URL で上書きできる。
const (
	DefaultGeminiEmbeddingModel = "text-embedding-004"
	// DefaultGeminiEmbeddingFallbackModel は既定のモデルが見つからない（404）ときに使う後継モデル。
	DefaultGeminiEmbeddingFallbackModel = "gemini-embedding-001"
	DefaultOpenAIEmbeddingModel         = "text-embedding-3-small"
	DefaultOllamaEmbeddingModel         = "nomic-embed-text"

	DefaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	DefaultOpenAIBaseURL = "https://api.openai.com/v1"
	DefaultOllamaBaseURL = "http://localhost:11434"
)

// DefaultGeminiDimensions は Gemini に要求する次元数（outputDimensionality）。
// text-embedding-004 の次元数に揃え、後継モデルに切り替わっても保存サイズを変えない。
const DefaultGeminiDimensions = 768

// DefaultEmbeddingTimeout は Embeddings API の1回の呼び出しの既定のタイムアウト。
const DefaultEmbeddingTimeout = 30 * time.Second

// maxEmbeddingResponseBytes は Embeddings API のレスポンスとして読み込む上限。
// 1,536 次元 × 100 件の JSON でも数 MB に収まる。
const maxEmbeddingResponseBytes = 32 << 20

// maxErrorMessage は APIError.Message に残すエラー本文の上限（バイト）。
const maxErrorMessage = 300

// 1回のリクエストで送るテキストの最大件数。Gemini の batchEmbedContents は 100 件が上限。
const (
	geminiBatchSize = 100
	openAIBatchSize = 100
	ollamaBatchSize = 32
)

var (
	// ErrNotConfigured は Embeddings の API キー等が未設定で、埋め込みを生成できないことを表す。
	ErrNotConfigured = errors.New("rag: embeddings provider is not configured")
	// ErrInvalidEmbeddings は Embeddings API の応答が入力と対応しない（件数・次元数の不一致、空、非有限値）ことを表す。
	ErrInvalidEmbeddings = errors.New("rag: invalid embeddings response")
)

// APIError は Embeddings API が成功以外のステータスを返したことを表す。Message に API キーは含めない。
type APIError struct {
	Provider   string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("rag: %s embeddings API returned %d: %s", e.Provider, e.StatusCode, e.Message)
}

// TaskType は埋め込みの用途。Gemini は用途に合わせて最適化したベクトルを返す。他のプロバイダーは区別しない。
type TaskType int

const (
	// TaskDocument は検索される側の文書（記事のチャンク）の埋め込み。
	TaskDocument TaskType = iota
	// TaskQuery は検索クエリの埋め込み。
	TaskQuery
)

// Embeddings は Embedder.Embed の結果。
type Embeddings struct {
	// Model は実際にベクトルを生成したモデル。異なるモデルのベクトルは比較できないため、保存と検索の絞り込みに使う。
	Model string
	// Vectors は入力のテキストと同じ順の埋め込みベクトル。すべて同じ次元数。
	Vectors [][]float32
}

// Embedder は Embeddings API のクライアントの共通インターフェース。
type Embedder interface {
	// Name は画面やログに出すプロバイダー名（例: "Gemini (text-embedding-004)"）。
	Name() string
	// Model は次の Embed で使う見込みのモデル。保存済みのベクトルを再利用できるかの判定に使う。
	// 実際に使ったモデルは Embeddings.Model で分かる。
	Model() string
	// Embed は texts の各要素の埋め込みベクトルを返す。texts が空なら API を呼ばずに空の結果を返す。
	Embed(ctx context.Context, texts []string, task TaskType) (*Embeddings, error)
}

// EmbedderConfig は Embedder の設定。
type EmbedderConfig struct {
	// Kind は KindGemini・KindOpenAI・KindOllama のいずれか。
	Kind string
	// APIKey は API キー。Ollama では不要。OpenAI でも BaseURL を差し替えた互換サーバーなら省略できる。
	APIKey string
	// Model は使用するモデル。省略すると各プロバイダーの既定のモデルを使う。
	Model string
	// BaseURL は API のベース URL。省略すると各プロバイダーの既定の URL を使う。
	BaseURL string
	// Dimensions は要求する次元数（Gemini の outputDimensionality、OpenAI の dimensions）。
	// 0 なら Gemini は DefaultGeminiDimensions、OpenAI はモデルの既定の次元数。Ollama では使わない。
	Dimensions int
	// HTTPClient は API 呼び出しに使うクライアント。省略すると Timeout 付きのクライアントを作る。
	HTTPClient *http.Client
	// Timeout は1回の呼び出しのタイムアウト。既定は DefaultEmbeddingTimeout。
	Timeout time.Duration
}

// NewEmbedder は cfg に従って Embedder を作る。Kind が空、または必要な API キーが無ければ ErrNotConfigured を返す。
func NewEmbedder(cfg EmbedderConfig) (Embedder, error) {
	if cfg.HTTPClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultEmbeddingTimeout
		}
		cfg.HTTPClient = &http.Client{Timeout: timeout}
	}
	if cfg.Dimensions < 0 {
		return nil, fmt.Errorf("rag: negative embedding dimensions %d", cfg.Dimensions)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	switch cfg.Kind {
	case "":
		return nil, ErrNotConfigured
	case KindGemini:
		if cfg.APIKey == "" {
			return nil, ErrNotConfigured
		}
		return newGeminiEmbedder(cfg), nil
	case KindOpenAI:
		if cfg.APIKey == "" && cfg.BaseURL == "" {
			return nil, ErrNotConfigured
		}
		return newOpenAIEmbedder(cfg), nil
	case KindOllama:
		return newOllamaEmbedder(cfg), nil
	}
	return nil, fmt.Errorf("rag: unknown EMBEDDING_PROVIDER %q (want gemini, openai or ollama)", cfg.Kind)
}

// NewEmbedderFromEnv は環境変数から Embedder を作る。
//
//   - EMBEDDING_PROVIDER: gemini / openai / ollama。省略すると AI_PROVIDER が gemini・openai ならそれを使い、
//     それ以外は設定済みの API キーから選ぶ（GEMINI_API_KEY → OPENAI_API_KEY の順）。
//   - EMBEDDING_API_KEY: API キー（GEMINI_API_KEY・OPENAI_API_KEY より優先）。
//   - EMBEDDING_MODEL: モデル名。EMBEDDING_BASE_URL: API のベース URL（Ollama なら http://localhost:11434 など）。
//   - EMBEDDING_DIMENSIONS: 要求する次元数。
//
// 何も設定されていなければ ErrNotConfigured を返す。
func NewEmbedderFromEnv() (Embedder, error) {
	cfg, err := embedderConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	return NewEmbedder(cfg)
}

// embedderConfigFromEnv は getenv で読んだ環境変数から EmbedderConfig を組み立てる。
func embedderConfigFromEnv(getenv func(string) string) (EmbedderConfig, error) {
	keyEnv := map[string]string{KindGemini: "GEMINI_API_KEY", KindOpenAI: "OPENAI_API_KEY"}
	cfg := EmbedderConfig{
		Kind:    normalizeKind(getenv("EMBEDDING_PROVIDER")),
		Model:   strings.TrimSpace(getenv("EMBEDDING_MODEL")),
		BaseURL: strings.TrimSpace(getenv("EMBEDDING_BASE_URL")),
	}
	aiKind := normalizeKind(getenv("AI_PROVIDER"))
	if cfg.Kind == "" && keyEnv[aiKind] != "" {
		cfg.Kind = aiKind
	}
	if cfg.Kind == "" {
		for _, k := range []string{KindGemini, KindOpenAI} {
			if strings.TrimSpace(getenv(keyEnv[k])) != "" {
				cfg.Kind = k
				break
			}
		}
	}
	if cfg.Kind == "" {
		return cfg, nil
	}
	cfg.APIKey = strings.TrimSpace(getenv("EMBEDDING_API_KEY"))
	if cfg.APIKey == "" && keyEnv[cfg.Kind] != "" {
		cfg.APIKey = strings.TrimSpace(getenv(keyEnv[cfg.Kind]))
	}
	if cfg.APIKey == "" && cfg.Kind == aiKind {
		// チャット用に AI_PROVIDER と AI_API_KEY だけを設定している場合も、同じキーで埋め込みを生成する。
		cfg.APIKey = strings.TrimSpace(getenv("AI_API_KEY"))
	}
	if s := strings.TrimSpace(getenv("EMBEDDING_DIMENSIONS")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("rag: invalid EMBEDDING_DIMENSIONS %q (want a positive integer)", s)
		}
		cfg.Dimensions = n
	}
	return cfg, nil
}

// normalizeKind は EMBEDDING_PROVIDER・AI_PROVIDER の表記ゆれ（大文字・別名）を Kind に揃える。
func normalizeKind(s string) string {
	switch k := strings.ToLower(strings.TrimSpace(s)); k {
	case "google":
		return KindGemini
	default:
		return k
	}
}

// providerName はプロバイダーのラベルにモデル名を添えた表示名を返す。
func providerName(label, model string) string {
	if model == "" {
		return label
	}
	return label + " (" + model + ")"
}

// GeminiEmbedder は Google Gemini の batchEmbedContents API のクライアント。並行に使用してよい。
// モデルを指定しなかった場合、既定のモデルが見つからない（404、提供終了など）ときは後継モデルで再試行し、
// 以降の呼び出しは後継モデルから始める。
type GeminiEmbedder struct {
	client     *http.Client
	baseURL    string
	apiKey     string
	models     []string
	dimensions int
	// active は最後に成功した models の位置。見つからなかったモデルを毎回試さないようにする。
	active atomic.Int32
}

func newGeminiEmbedder(cfg EmbedderConfig) *GeminiEmbedder {
	g := &GeminiEmbedder{client: cfg.HTTPClient, baseURL: cfg.BaseURL, apiKey: cfg.APIKey, dimensions: cfg.Dimensions}
	if g.baseURL == "" {
		g.baseURL = DefaultGeminiBaseURL
	}
	if g.dimensions == 0 {
		g.dimensions = DefaultGeminiDimensions
	}
	if cfg.Model != "" {
		g.models = []string{cfg.Model}
	} else {
		g.models = []string{DefaultGeminiEmbeddingModel, DefaultGeminiEmbeddingFallbackModel}
	}
	return g
}

func (g *GeminiEmbedder) Name() string { return providerName("Gemini", g.Model()) }

func (g *GeminiEmbedder) Model() string { return g.models[g.active.Load()] }

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiEmbedRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	TaskType             string        `json:"taskType,omitempty"`
	OutputDimensionality int           `json:"outputDimensionality,omitempty"`
}

type geminiBatchResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

func (g *GeminiEmbedder) Embed(ctx context.Context, texts []string, task TaskType) (*Embeddings, error) {
	if len(texts) == 0 {
		return &Embeddings{Model: g.Model()}, nil
	}
	taskType := "RETRIEVAL_DOCUMENT"
	if task == TaskQuery {
		taskType = "RETRIEVAL_QUERY"
	}
	var err error
	for i := int(g.active.Load()); i < len(g.models); i++ {
		var vectors [][]float32
		vectors, err = g.embedWith(ctx, g.models[i], texts, taskType)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			// 途中のバッチで見つからなくなった場合も、モデルが混ざらないよう後継モデルで最初からやり直す。
			continue
		}
		if err != nil {
			return nil, err
		}
		g.active.Store(int32(i))
		return &Embeddings{Model: g.models[i], Vectors: vectors}, nil
	}
	return nil, err
}

func (g *GeminiEmbedder) embedWith(ctx context.Context, model string, texts []string, taskType string) ([][]float32, error) {
	// API キーはクエリ（?key=）ではなくヘッダーで送り、エラーやログの URL に残らないようにする。
	header := http.Header{"X-Goog-Api-Key": {g.apiKey}}
	endpoint := g.baseURL + "/models/" + url.PathEscape(model) + ":batchEmbedContents"
	return embedInBatches(texts, geminiBatchSize, func(batch []string) ([][]float32, error) {
		reqs := make([]geminiEmbedRequest, len(batch))
		for i, text := range batch {
			reqs[i] = geminiEmbedRequest{
				Model:                "models/" + model,
				Content:              geminiContent{Parts: []geminiPart{{Text: text}}},
				TaskType:             taskType,
				OutputDimensionality: g.dimensions,
			}
		}
		var res geminiBatchResponse
		if err := postJSON(ctx, g.client, "Gemini", endpoint, header, map[string]any{"requests": reqs}, &res); err != nil {
			return nil, err
		}
		vectors := make([][]float32, len(res.Embeddings))
		for i, e := range res.Embeddings {
			vectors[i] = e.Values
		}
		return vectors, nil
	})
}

// OpenAIEmbedder は OpenAI の Embeddings API（/embeddings）のクライアント。OpenAI 互換のサーバーにも使える。
type OpenAIEmbedder struct {
	client     *http.Client
	baseURL    string
	apiKey     string
	model      string
	dimensions int
}

func newOpenAIEmbedder(cfg EmbedderConfig) *OpenAIEmbedder {
	o := &OpenAIEmbedder{client: cfg.HTTPClient, baseURL: cfg.BaseURL, apiKey: cfg.APIKey, model: cfg.Model, dimensions: cfg.Dimensions}
	if o.baseURL == "" {
		o.baseURL = DefaultOpenAIBaseURL
	}
	if o.model == "" {
		o.model = DefaultOpenAIEmbeddingModel
	}
	return o
}

func (o *OpenAIEmbedder) Name() string { return providerName("OpenAI", o.model) }

func (o *OpenAIEmbedder) Model() string { return o.model }

type openAIEmbedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (o *OpenAIEmbedder) Embed(ctx context.Context, texts []string, _ TaskType) (*Embeddings, error) {
	if len(texts) == 0 {
		return &Embeddings{Model: o.model}, nil
	}
	header := http.Header{}
	if o.apiKey != "" {
		header.Set("Authorization", "Bearer "+o.apiKey)
	}
	vectors, err := embedInBatches(texts, openAIBatchSize, func(batch []string) ([][]float32, error) {
		req := openAIEmbedRequest{Model: o.model, Input: batch, Dimensions: o.dimensions, EncodingFormat: "float"}
		var res openAIEmbedResponse
		if err := postJSON(ctx, o.client, "OpenAI", o.baseURL+"/embeddings", header, req, &res); err != nil {
			return nil, err
		}
		// data は index の順に並ぶとは限らないため、index で入力の位置に戻す。
		vectors := make([][]float32, len(batch))
		for _, d := range res.Data {
			if d.Index < 0 || d.Index >= len(batch) || vectors[d.Index] != nil {
				return nil, fmt.Errorf("%w: OpenAI returned unexpected index %d", ErrInvalidEmbeddings, d.Index)
			}
			vectors[d.Index] = d.Embedding
		}
		return vectors, nil
	})
	if err != nil {
		return nil, err
	}
	return &Embeddings{Model: o.model, Vectors: vectors}, nil
}

// OllamaEmbedder はローカルの Ollama の Embeddings API（/api/embed）のクライアント。API キーは不要。
type OllamaEmbedder struct {
	client  *http.Client
	baseURL string
	model   string
}

func newOllamaEmbedder(cfg EmbedderConfig) *OllamaEmbedder {
	o := &OllamaEmbedder{client: cfg.HTTPClient, baseURL: cfg.BaseURL, model: cfg.Model}
	if o.baseURL == "" {
		o.baseURL = DefaultOllamaBaseURL
	}
	if o.model == "" {
		o.model = DefaultOllamaEmbeddingModel
	}
	return o
}

func (o *OllamaEmbedder) Name() string { return providerName("Ollama", o.model) }

func (o *OllamaEmbedder) Model() string { return o.model }

func (o *OllamaEmbedder) Embed(ctx context.Context, texts []string, _ TaskType) (*Embeddings, error) {
	if len(texts) == 0 {
		return &Embeddings{Model: o.model}, nil
	}
	vectors, err := embedInBatches(texts, ollamaBatchSize, func(batch []string) ([][]float32, error) {
		req := map[string]any{"model": o.model, "input": batch}
		var res struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := postJSON(ctx, o.client, "Ollama", o.baseURL+"/api/embed", nil, req, &res); err != nil {
			return nil, err
		}
		return res.Embeddings, nil
	})
	if err != nil {
		return nil, err
	}
	return &Embeddings{Model: o.model, Vectors: vectors}, nil
}

// embedInBatches は texts を size 件ずつ embed に渡し、結果をつないで返す。
// 各バッチの結果が入力と同じ件数で、すべてのベクトルが同じ次元数の有限値であることを確かめる。
func embedInBatches(texts []string, size int, embed func(batch []string) ([][]float32, error)) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += size {
		batch := texts[start:min(start+size, len(texts))]
		vectors, err := embed(batch)
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(batch) {
			return nil, fmt.Errorf("%w: got %d vectors for %d texts", ErrInvalidEmbeddings, len(vectors), len(batch))
		}
		out = append(out, vectors...)
	}
	if err := validateVectors(out); err != nil {
		return nil, err
	}
	return out, nil
}

// validateVectors は vectors が空でなく、すべて同じ次元数の有限値であることを確かめる。
func validateVectors(vectors [][]float32) error {
	for i, v := range vectors {
		if len(v) == 0 {
			return fmt.Errorf("%w: vector %d is empty", ErrInvalidEmbeddings, i)
		}
		if len(v) != len(vectors[0]) {
			return fmt.Errorf("%w: vector %d has %d dimensions, want %d", ErrInvalidEmbeddings, i, len(v), len(vectors[0]))
		}
		for _, x := range v {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return fmt.Errorf("%w: vector %d has a non-finite value", ErrInvalidEmbeddings, i)
			}
		}
	}
	return nil
}

// postJSON は body を JSON で POST し、2xx の応答を out にデコードする。
// 2xx 以外は応答本文の error.message（無ければ本文の先頭）を持つ *APIError を返す。
func postJSON(ctx context.Context, client *http.Client, provider, endpoint string, header http.Header, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("rag: encode %s request: %w", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("rag: build %s request: %w", provider, err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		// *url.Error は URL を含むが、API キーは URL に載せないため、そのまま返してよい。
		return fmt.Errorf("rag: %s request: %w", provider, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxEmbeddingResponseBytes))
	if err != nil {
		return fmt.Errorf("rag: read %s response: %w", provider, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &APIError{Provider: provider, StatusCode: res.StatusCode, Message: errorMessage(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("rag: decode %s response: %w", provider, err)
	}
	return nil
}

// errorMessage は各社共通の {"error": {"message": "..."}} 形式（Ollama は {"error": "..."}）からメッセージを取り出す。
func errorMessage(data []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &e) == nil && len(e.Error) > 0 {
		var nested struct {
			Message string `json:"message"`
		}
		var plain string
		switch {
		case json.Unmarshal(e.Error, &nested) == nil && nested.Message != "":
			msg = nested.Message
		case json.Unmarshal(e.Error, &plain) == nil && plain != "":
			msg = plain
		}
	}
	if len(msg) > maxErrorMessage {
		msg = strings.ToValidUTF8(msg[:maxErrorMessage], "") + "…"
	}
	return msg
}
