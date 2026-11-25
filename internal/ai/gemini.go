package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DefaultGeminiBaseURL は Google Gemini API（Google AI Studio）のベース URL。
const DefaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Gemini は Google Gemini の generateContent API のクライアント。
// 既定のモデルが見つからない（404）ときは DefaultGeminiFallbackModel で再試行する。
type Gemini struct {
	assistant
	client  *http.Client
	baseURL string
	apiKey  string
	models  []string
}

// NewGemini は cfg の API キーとモデルで Gemini クライアントを作る。
func NewGemini(cfg Config) *Gemini {
	g := &Gemini{client: cfg.HTTPClient, baseURL: cfg.BaseURL, apiKey: cfg.APIKey}
	if g.client == nil {
		g.client = defaultHTTPClient(cfg)
	}
	if g.baseURL == "" {
		g.baseURL = DefaultGeminiBaseURL
	}
	if cfg.Model != "" {
		g.models = []string{cfg.Model}
	} else {
		g.models = []string{DefaultGeminiModel, DefaultGeminiFallbackModel}
	}
	g.assistant = assistant{name: "Gemini (" + g.models[0] + ")", c: g}
	return g
}

type geminiPart struct {
	Text string `json:"text,omitempty"`
	// InlineData は画像などのバイナリをリクエストに直接埋め込む（AnalyzeImage で使う）。
	InlineData *geminiInlineData `json:"inline_data,omitempty"`
}

// geminiInlineData は generateContent の inline_data。Data は JSON にするとき base64 になる。
type geminiInlineData struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiTool は generateContent の tools の1要素。GoogleSearch を {} にすると Google 検索によるグラウンディングを有効にする。
type geminiTool struct {
	GoogleSearch *struct{} `json:"google_search,omitempty"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	Tools             []geminiTool    `json:"tools,omitempty"`
	// GenerationConfig は応答の形式（JSON スキーマなど）を指定するときだけ送る。
	GenerationConfig *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

// geminiGenerationConfig は generateContent の generationConfig のうち、構造化出力に使う項目。
type geminiGenerationConfig struct {
	ResponseMimeType string        `json:"responseMimeType,omitempty"`
	ResponseSchema   *geminiSchema `json:"responseSchema,omitempty"`
}

// geminiSchema は responseSchema に渡す OpenAPI サブセットのスキーマ。
type geminiSchema struct {
	Type        string                   `json:"type"`
	Description string                   `json:"description,omitempty"`
	Properties  map[string]*geminiSchema `json:"properties,omitempty"`
	Items       *geminiSchema            `json:"items,omitempty"`
	Required    []string                 `json:"required,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
	// GroundingMetadata は Google 検索でグラウンディングしたときだけ入る。
	GroundingMetadata *groundingMetadata `json:"groundingMetadata"`
}

// text は候補のすべてのパートのテキストを連結する。
func (c *geminiCandidate) text() string {
	var b strings.Builder
	for _, p := range c.Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

type geminiResponse struct {
	Candidates     []geminiCandidate `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

func (g *Gemini) complete(ctx context.Context, system, user string) (completion, error) {
	cand, err := g.generate(ctx, geminiRequest{
		SystemInstruction: &geminiContent{Parts: []geminiPart{{Text: system}}},
		Contents:          []geminiContent{{Role: "user", Parts: []geminiPart{{Text: user}}}},
	})
	if err != nil {
		return completion{}, err
	}
	return completion{text: cand.text(), finishReason: cand.FinishReason}, nil
}

// generate は req をモデルの候補順に送り、最初の応答候補を返す。モデルが見つからない（404）ときは次のモデルで再試行する。
// 応答がブロックされたときは *APIError、テキストが空なら ErrEmptyResponse を返す。
func (g *Gemini) generate(ctx context.Context, req geminiRequest) (*geminiCandidate, error) {
	// API キーはクエリ（?key=）ではなくヘッダーで送り、エラーやログの URL に残らないようにする。
	header := http.Header{"X-Goog-Api-Key": {g.apiKey}}
	var err error
	for _, model := range g.models {
		var raw json.RawMessage
		endpoint := g.baseURL + "/models/" + url.PathEscape(model) + ":generateContent"
		err = postJSON(ctx, g.client, "Gemini", endpoint, header, req, &raw)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		var res *geminiResponse
		if res, err = decodeGeminiResponse(raw); err != nil {
			return nil, err
		}
		if res.PromptFeedback.BlockReason != "" {
			return nil, &APIError{Provider: "Gemini", StatusCode: http.StatusOK, Message: "blocked: " + res.PromptFeedback.BlockReason}
		}
		if len(res.Candidates) == 0 {
			return nil, ErrEmptyResponse
		}
		if isTruncatedFinishReason(res.Candidates[0].FinishReason) {
			return nil, ErrTruncated
		}
		if strings.TrimSpace(res.Candidates[0].text()) == "" {
			return nil, ErrEmptyResponse
		}
		return &res.Candidates[0], nil
	}
	return nil, err
}

// decodeGeminiResponse は generateContent の応答をデコードする。
// REST API の応答は lowerCamelCase だが、SDK 形式のプロキシが返す snake_case（grounding_metadata など）も受け付ける。
// テキストの取り出しに使うキー（candidates・content・parts・text）は表記が同じなので、snake_case の応答は
// グラウンディングやブロックの情報があるときだけキーを書き換える。
func decodeGeminiResponse(raw []byte) (*geminiResponse, error) {
	if bytes.Contains(raw, []byte(`"grounding_metadata"`)) || bytes.Contains(raw, []byte(`"prompt_feedback"`)) {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("ai: decode Gemini response: %w", err)
		}
		if converted, err := json.Marshal(camelizeKeys(v)); err == nil {
			raw = converted
		}
	}
	var res geminiResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("ai: decode Gemini response: %w", err)
	}
	return &res, nil
}

// camelizeKeys は JSON をデコードした値のオブジェクトのキーを、snake_case から lowerCamelCase にすべて書き換える。
// 値（テキストなど）は変えない。
func camelizeKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[snakeToCamel(k)] = camelizeKeys(val)
		}
		return m
	case []any:
		for i, val := range t {
			t[i] = camelizeKeys(val)
		}
		return t
	}
	return v
}

// snakeToCamel は "web_search_queries" を "webSearchQueries" にする。"_" を含まないキーはそのまま返す。
func snakeToCamel(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	parts := strings.Split(s, "_")
	var b strings.Builder
	b.WriteString(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// Translate はテキストを翻訳する。学術エディトリアルプロトコルおよび構文保護を適用する。
func (g *Gemini) Translate(ctx context.Context, text string, srcLang, targetLang string, opts TranslateOptions) (string, error) {
	return g.assistant.Translate(ctx, text, srcLang, targetLang, opts)
}
