package ai

import (
	"context"
	"net/http"
)

// DefaultOpenAIBaseURL は OpenAI API のベース URL。
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

// OpenAI は OpenAI Chat Completions API のクライアント。OpenAI 互換のエンドポイント（Custom）にも使う。
type OpenAI struct {
	assistant
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
	label   string
}

// NewOpenAI は cfg の API キーとモデルで OpenAI クライアントを作る。
func NewOpenAI(cfg Config) *OpenAI {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultOpenAIBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultOpenAIModel
	}
	return newOpenAICompatible(cfg, "OpenAI")
}

func newOpenAICompatible(cfg Config, label string) *OpenAI {
	o := &OpenAI{client: cfg.HTTPClient, baseURL: cfg.BaseURL, apiKey: cfg.APIKey, model: cfg.Model, label: label}
	if o.client == nil {
		o.client = defaultHTTPClient(cfg)
	}
	name := label
	if o.model != "" {
		name += " (" + o.model + ")"
	}
	o.assistant = assistant{name: name, c: o}
	return o
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model    string          `json:"model,omitempty"`
	Messages []openAIMessage `json:"messages"`
}

type openAIResponse struct {
	Choices []struct {
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
}

func (o *OpenAI) complete(ctx context.Context, system, user string) (completion, error) {
	header := http.Header{}
	if o.apiKey != "" {
		header.Set("Authorization", "Bearer "+o.apiKey)
	}
	req := openAIRequest{
		Model: o.model,
		Messages: []openAIMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	var res openAIResponse
	if err := postJSON(ctx, o.client, o.label, o.baseURL+"/chat/completions", header, req, &res); err != nil {
		return completion{}, err
	}
	if len(res.Choices) == 0 {
		return completion{}, ErrEmptyResponse
	}
	return completion{text: res.Choices[0].Message.Content, finishReason: res.Choices[0].FinishReason}, nil
}

// Translate はテキストを翻訳する。
func (o *OpenAI) Translate(ctx context.Context, text string, srcLang, targetLang string, opts TranslateOptions) (string, error) {
	return o.assistant.Translate(ctx, text, srcLang, targetLang, opts)
}
