package ai

// NewCustom は OpenAI 互換のエンドポイント（Ollama・LM Studio・Groq・DeepSeek など）のクライアントを作る。
// cfg.BaseURL は /chat/completions の手前まで（例: Ollama なら http://localhost:11434/v1）。
// ローカル LLM では API キーを省略でき、その場合は Authorization ヘッダーを送らない。
func NewCustom(cfg Config) *OpenAI {
	return newOpenAICompatible(cfg, "OpenAI互換")
}
