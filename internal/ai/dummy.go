package ai

import "context"

// Dummy は API キーが未設定のときに使うプロバイダー。AI API を呼ばず、どの操作も ErrNotConfigured を返す。
// 管理画面はこのエラーを受けて、エラー画面ではなく設定ガイド（SetupGuide）を表示する。
type Dummy struct {
	// KeyEnv は設定ガイドで案内する環境変数名（例: GEMINI_API_KEY）。
	KeyEnv string
}

// NewDummy は keyEnv の設定を案内する Dummy を作る。keyEnv が空なら GEMINI_API_KEY を案内する。
func NewDummy(keyEnv string) *Dummy {
	if keyEnv == "" {
		keyEnv = "GEMINI_API_KEY"
	}
	return &Dummy{KeyEnv: keyEnv}
}

func (d *Dummy) Name() string { return "未設定" }

// SetupGuide は AI アシスタントを有効にする方法の案内文。
func (d *Dummy) SetupGuide() string {
	return "AIアシスタントを利用するには、管理画面の「設定」→「AI」でAPIキーを登録するか、`.env` または環境変数に `" + d.KeyEnv + "` を設定してください。"
}

func (d *Dummy) Proofread(context.Context, string) (*ProofreadResult, error) {
	return nil, ErrNotConfigured
}

func (d *Dummy) SuggestTitles(context.Context, string) ([]string, error) {
	return nil, ErrNotConfigured
}

func (d *Dummy) Summarize(context.Context, string) (string, error) {
	return "", ErrNotConfigured
}

func (d *Dummy) FactCheck(context.Context, string) (*FactCheckResult, error) {
	return nil, ErrNotConfigured
}

func (d *Dummy) Simplify(context.Context, string, string) (*ProofreadResult, error) {
	return nil, ErrNotConfigured
}

func (d *Dummy) GenerateAbstract(context.Context, string) (string, error) {
	return "", ErrNotConfigured
}

func (d *Dummy) Translate(context.Context, string, string, string, TranslateOptions) (string, error) {
	return "", ErrNotConfigured
}

func (d *Dummy) StyleCheck(context.Context, string, string) (*ProofreadResult, error) {
	return nil, ErrNotConfigured
}

func (d *Dummy) GenerateDraft(context.Context, string, string) (*DraftResult, error) {
	return nil, ErrNotConfigured
}
