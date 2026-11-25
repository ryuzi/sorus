package ai

import (
	"context"
	"net/http"
	"strings"
)

const (
	// DefaultClaudeBaseURL は Anthropic API のベース URL。
	DefaultClaudeBaseURL = "https://api.anthropic.com/v1"
	// claudeAPIVersion は anthropic-version ヘッダーに送る API バージョン。
	claudeAPIVersion = "2023-06-01"
	// claudeMaxTokens は1回の応答の最大トークン数。長い記事の推敲結果が途中で切れないよう大きめにする。
	claudeMaxTokens = 8192
)

// Claude は Anthropic Messages API のクライアント。
type Claude struct {
	assistant
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// NewClaude は cfg の API キーとモデルで Claude クライアントを作る。
func NewClaude(cfg Config) *Claude {
	c := &Claude{client: cfg.HTTPClient, baseURL: cfg.BaseURL, apiKey: cfg.APIKey, model: cfg.Model}
	if c.client == nil {
		c.client = defaultHTTPClient(cfg)
	}
	if c.baseURL == "" {
		c.baseURL = DefaultClaudeBaseURL
	}
	if c.model == "" {
		c.model = DefaultClaudeModel
	}
	c.assistant = assistant{name: "Claude (" + c.model + ")", c: c}
	return c
}

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []openAIMessage `json:"messages"`
}

type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

func (c *Claude) complete(ctx context.Context, system, user string) (completion, error) {
	header := http.Header{
		"X-Api-Key":         {c.apiKey},
		"Anthropic-Version": {claudeAPIVersion},
	}
	req := claudeRequest{
		Model:     c.model,
		MaxTokens: claudeMaxTokens,
		System:    system,
		Messages:  []openAIMessage{{Role: "user", Content: user}},
	}
	var res claudeResponse
	if err := postJSON(ctx, c.client, "Claude", c.baseURL+"/messages", header, req, &res); err != nil {
		return completion{}, err
	}
	var b strings.Builder
	for _, block := range res.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return completion{text: b.String(), finishReason: res.StopReason}, nil
}

// Translate はテキストを翻訳する。
func (c *Claude) Translate(ctx context.Context, text string, srcLang, targetLang string, opts TranslateOptions) (string, error) {
	return c.assistant.Translate(ctx, text, srcLang, targetLang, opts)
}
