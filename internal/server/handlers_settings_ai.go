package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
	"sorus/internal/auth"
	"sorus/internal/i18n"
)

// 「設定」→「AI」（管理者だけ）。AI 執筆アシスタントのプロバイダー・API キー・モデル・ベース URL を画面から設定し、
// その場で接続を試せるようにする。環境変数（GEMINI_API_KEY など）を触れない人でも AI を使い始められるようにする。
//
// 使う設定の優先順位は「画面で入力したキー > 環境変数のキー > 未設定（Dummy）」。キーの欄を空のまま保存したときは、
// 同じプロバイダー（custom なら同じベース URL）で保存済みのキーを使い続け、無ければ環境変数のキーを使う。
// 別のプロバイダーや別のベース URL に切り替えたときは、保存済みのキーをそこへ送らない。
//
// キー・モデル・ベース URL はプロバイダー（gemini・openai・claude・custom・deepl）ごとに保存する
// （savedAIConfig.Providers）。メインのプロバイダー（既定で使う AI）を切り替えても、ほかのプロバイダーのキーは消えない。
// 記事エディタの AI（/admin/ai/*）と翻訳は、保存済みの任意のプロバイダーを指定して使える（aiProviderFor）。
const (
	aiSettingsPath     = "/admin/settings/ai"
	aiSettingsTestPath = "/admin/settings/ai/test"
	aiSettingsReset    = "/admin/settings/ai/reset"
	// aiSettingKey は settings テーブルに AI の設定（savedAIConfig の JSON）を保存するキー。
	aiSettingKey = "ai_config"

	maxAIKeyLength   = 512
	maxAIModelLength = 100
	// maxAIGlossaryRunes は用語集の上限（文字数）。用語集は AI へのプロンプトに毎回含めるため、長すぎる入力で
	// 送信量・料金が膨らんだり保存領域を圧迫したりしないよう制限する。
	maxAIGlossaryRunes = 5000
	maxAIBaseURLLength = 500
	// aiTestTimeout は接続テストの待ち時間の上限。管理画面サーバーの WriteTimeout（60秒）より短くする。
	aiTestTimeout = 30 * time.Second
	// aiTestSample は接続テストでタイトル提案に送る短い本文。
	aiTestSample = "sorus の接続テストです。地域の図書館で、子ども向けの読書会が毎月開かれるようになりました。" +
		"参加者は本の感想を話し合い、司書がおすすめの本を紹介します。"
)

func init() {
	if !slices.Contains(pageNames, "settings_ai") {
		pageNames = append(pageNames, "settings_ai")
	}
	flashMessages["ai_saved"] = "AIの設定を保存しました"
	flashMessages["ai_reset"] = "画面で保存したAIの設定を消し、環境変数の設定に戻しました"
}

// registerAISettingsRoutes は「設定」→「AI」の画面・保存・接続テストを登録する。
// 一般設定（文章スタイル・用語集）は全ログインユーザー、インフラ設定（APIキー等）や接続テスト・リセットは管理者のみ。
// AI 機能を OFF にしたサイト（aiEnabled）では、どれも存在しないページと同じ 404 にする（保存済みの設定は消さない）。
func (s *Server) registerAISettingsRoutes(authed *echo.Group, adminOnly echo.MiddlewareFunc) {
	aiOnly := s.requireAIEnabled
	authed.GET("/settings/ai", s.handleAISettings, aiOnly)
	authed.POST("/settings/ai", s.handleAISettingsSave, aiOnly)
	authed.POST("/settings/ai/test", s.handleAISettingsTest, aiOnly, adminOnly)
	authed.POST("/settings/ai/reset", s.handleAISettingsReset, aiOnly, adminOnly)
}

// savedAIConfig は画面で保存した AI の設定。APIKey が空なら環境変数のキーを使う（resolveAIConfig）。
//
// Provider はメインのプロバイダー（既定で使う AI）。プロバイダーごとの設定は Providers に持ち、APIKey・Model・BaseURL・
// DeepLAPIKey は、Providers のメインのプロバイダー・DeepL の設定を写した値（旧形式。古い版の sorus でも読めるよう
// 保存し続ける）。旧形式だけの保存データは loadAIConfig が Providers に補完する（normalizeSavedAI）。
type savedAIConfig struct {
	Provider string `json:"provider"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
	// BaseURL は Provider が custom のときだけ保存する（それ以外は常に空）。
	BaseURL string `json:"base_url,omitempty"`
	// Providers はプロバイダー（gemini・openai・claude・custom・deepl）ごとの設定。
	Providers map[string]savedProviderConfig `json:"providers,omitempty"`
	// 多言語・翻訳専用設定
	TranslationProvider string `json:"translation_provider,omitempty"`
	DeepLAPIKey         string `json:"deepl_api_key,omitempty"`
	Glossary            string `json:"glossary,omitempty"`
	AcademicStyle       bool   `json:"academic_style,omitempty"`
	WritingStyle        string `json:"writing_style,omitempty"`
}

// savedProviderConfig はプロバイダー1つの保存した設定。APIKey が空なら環境変数のキーを使う（resolveAIConfig）。
type savedProviderConfig struct {
	APIKey string `json:"api_key,omitempty"`
	Model  string `json:"model,omitempty"`
	// BaseURL は custom のときだけ保存する（それ以外は常に空）。
	BaseURL string `json:"base_url,omitempty"`
}

// aiProviderOption は画面のプロバイダーの選択肢。
type aiProviderOption struct {
	Value, Label, DefaultModel string
	// ShortName は入力の誤りの文などに出す短い名前。
	ShortName string
	// KeyEnv はこのプロバイダーのキーを読む環境変数名。KeyURL はキーを発行する画面、KeyHint はキーの見分け方。
	KeyEnv, KeyURL, KeyHint string
	// Models はモデル名の入力候補。
	Models []string
	// KeyOptional はキーを省略できる（ローカル LLM）か。
	KeyOptional bool
}

var aiProviderOptions = []aiProviderOption{
	{Value: ai.KindGemini, Label: "Gemini（Google AI Studio・無料枠あり）", ShortName: "Gemini", DefaultModel: ai.DefaultGeminiModel,
		KeyEnv: "GEMINI_API_KEY", KeyURL: "https://aistudio.google.com/app/apikey",
		KeyHint: "Google AI Studio で発行したキー（AQ. または AIza で始まる文字列）を貼り付けます。",
		Models:  []string{ai.DefaultGeminiModel, ai.DefaultGeminiFallbackModel, "gemini-4-argon"}},
	{Value: ai.KindOpenAI, Label: "OpenAI 互換（OpenAI の API）", ShortName: "OpenAI", DefaultModel: ai.DefaultOpenAIModel,
		KeyEnv: "OPENAI_API_KEY", KeyURL: "https://platform.openai.com/api-keys",
		KeyHint: "OpenAI で発行したキー（sk- で始まる文字列）を貼り付けます。",
		Models:  []string{ai.DefaultOpenAIModel}},
	{Value: ai.KindClaude, Label: "Claude（Anthropic）", ShortName: "Claude", DefaultModel: ai.DefaultClaudeModel,
		KeyEnv: "ANTHROPIC_API_KEY", KeyURL: "https://console.anthropic.com/settings/keys",
		KeyHint: "Anthropic で発行したキー（sk-ant- で始まる文字列）を貼り付けます。",
		Models:  []string{ai.DefaultClaudeModel}},
	{Value: ai.KindCustom, Label: "ローカルLLM・その他の互換API（Ollama・LM Studio など）", ShortName: "ローカルLLM",
		KeyEnv: "AI_API_KEY", KeyOptional: true,
		KeyHint: "このPCで動かす Ollama・LM Studio ならキーは不要です。キーが必要なサービスだけ入力します。",
		Models:  []string{"llama3.1", "qwen2.5", "gemma2"}},
	{Value: ai.KindDeepL, Label: "DeepL（高精度・自然な翻訳専用API）", ShortName: "DeepL", DefaultModel: "default",
		KeyEnv: "DEEPL_API_KEY", KeyURL: "https://www.deepl.com/your-account/keys",
		KeyHint: "DeepL で発行した API キー（Free版は末尾が :fx、Pro版はそれ以外）を貼り付けます。",
		Models:  []string{"default"}},
}

// aiProviderTextEN は、管理画面を英語で表示するときのプロバイダーの名前とキーの見分け方（aiProviderOptions の英語版）。
var aiProviderTextEN = map[string][2]string{
	ai.KindGemini: {"Gemini (Google AI Studio, free tier available)", "Paste a key issued in Google AI Studio (a string starting with AQ. or AIza)."},
	ai.KindOpenAI: {"OpenAI-compatible (OpenAI API)", "Paste a key issued by OpenAI (a string starting with sk-)."},
	ai.KindClaude: {"Claude (Anthropic)", "Paste a key issued by Anthropic (a string starting with sk-ant-)."},
	ai.KindCustom: {"Local LLM / other compatible APIs (Ollama, LM Studio, etc.)", "No key is needed for Ollama or LM Studio running on this PC. Enter one only for services that require it."},
	ai.KindDeepL:  {"DeepL (high-quality, natural translation API)", "Paste an API key issued by DeepL (Free keys end with :fx; Pro keys do not)."},
}

// localize は、画面に出すプロバイダーの名前・キーの見分け方・キーの出どころを lang（ja・en）の文言にする。
func (d *aiSettingsData) localize(lang string) {
	if i18n.Normalize(lang) != i18n.EN {
		return
	}
	for i := range d.ProviderConfigs {
		it := &d.ProviderConfigs[i]
		if en, ok := aiProviderTextEN[it.Key]; ok {
			it.Label, it.KeyHint = en[0], en[1]
		}
		it.KeySource = aiKeySourceLabelEN(it.KeySource)
	}
	d.KeySource = aiKeySourceLabelEN(d.KeySource)
}

// aiKeySourceLabelEN は aiKeySourceLabel の文（日本語）を英語にする。
func aiKeySourceLabelEN(label string) string {
	switch label {
	case "なし":
		return "None"
	case "この画面で保存したキー":
		return "Key saved on this page"
	case "環境変数":
		return "Environment variable"
	}
	if env, ok := strings.CutPrefix(label, "環境変数 "); ok {
		return "Environment variable " + env
	}
	return label
}

// aiProviderOptionFor は kind の選択肢を返す。
func aiProviderOptionFor(kind string) (aiProviderOption, bool) {
	for _, o := range aiProviderOptions {
		if o.Value == kind {
			return o, true
		}
	}
	return aiProviderOption{}, false
}

// isTranslationKind は翻訳に使えるプロバイダーか（翻訳用 AI の選択肢）。
func isTranslationKind(kind string) bool {
	switch kind {
	case ai.KindGemini, ai.KindClaude, ai.KindOpenAI, ai.KindCustom, ai.KindDeepL:
		return true
	}
	return false
}

// aiProviderConfigItem は設定画面のプロバイダーごとのカードに出す値。キーそのものは出さない。
type aiProviderConfigItem struct {
	Key, Label, KeyHint, KeyURL, KeyEnv, DefaultModel string
	Models                                            []string
	// KeyOptional はキーを省略できる（ローカル LLM）か。
	KeyOptional bool
	// HasSavedKey・KeyPreview は保存済みのキーの有無と、伏せ字にした末尾。
	HasSavedKey bool
	KeyPreview  string
	// Model・BaseURL は入力欄の値（保存済み、または入力の誤りで表示し直すときは入力した値）。
	Model, BaseURL string
	// Configured は保存済みの設定（または環境変数のキー）で使える状態か。
	Configured bool
	// KeySource はこのプロバイダーで使うキーの出どころ（aiKeySourceLabel）。
	KeySource string
}

// aiSettingsData は settings_ai.html に渡すデータ。
type aiSettingsData struct {
	Providers []aiProviderOption
	// ProviderConfigs はプロバイダー（gemini・openai・claude・custom・deepl）ごとの設定。
	ProviderConfigs []aiProviderConfigItem
	// Form は入力欄の値（キーは出さない）。
	Form savedAIConfig
	// HasSaved は画面で保存した設定があるか。false なら環境変数の設定で動いている。
	HasSaved bool
	// HasSavedKey・KeyPreview は保存済みのキーの有無と、伏せ字にした先頭・末尾。
	HasSavedKey bool
	KeyPreview  string
	// HasSavedDeepLKey・DeepLKeyPreview は保存済みの DeepL キーの有無とプレビュー。
	HasSavedDeepLKey bool
	DeepLKeyPreview  string
	// Current・Configured はいま動いている AI の名前と、使える状態か。
	Current    string
	Configured bool
	// IsAdmin は現在ログインしているユーザーが管理者であるか。
	IsAdmin bool
	// KeySource はいま使っているキーの出どころ（「この画面で保存したキー」・環境変数名など）。
	KeySource string
	// EnvKeys はキーが設定されている環境変数の名前（値は出さない）。
	EnvKeys []string
	Errors  []string
	// MultilingualEnabled は多言語対応を有効にしているか。OFF なら翻訳専用の設定（翻訳に使う AI など）を出さない。
	MultilingualEnabled bool
}

// loadAIConfig は画面で保存した AI の設定を読む。保存されていなければ ok=false。
func (s *Server) loadAIConfig() (savedAIConfig, bool) {
	var cfg savedAIConfig
	raw := s.getSetting(aiSettingKey, "")
	if raw == "" || json.Unmarshal([]byte(raw), &cfg) != nil || cfg.Provider == "" {
		return savedAIConfig{}, false
	}
	normalizeSavedAI(&cfg)
	// 下位互換性: AcademicStyle または ai_academic_style "1"
	if cfg.WritingStyle == "" {
		if cfg.AcademicStyle {
			cfg.WritingStyle = ai.WritingStyleAcademic
		} else {
			cfg.WritingStyle = ai.WritingStyleStandard
		}
	} else if cfg.WritingStyle == "1" {
		cfg.WritingStyle = ai.WritingStyleAcademic
	}
	if cfg.WritingStyle == ai.WritingStyleAcademic {
		cfg.AcademicStyle = true
	}
	return cfg, true
}

// normalizeSavedAI は旧形式（Provider・APIKey・Model・BaseURL・DeepLAPIKey だけ）の保存データを Providers に補完し、
// Providers のメインのプロバイダー・DeepL の設定を旧形式の項目に写す。知らないプロバイダーの設定と、custom 以外の
// ベース URL は捨てる。
func normalizeSavedAI(cfg *savedAIConfig) {
	providers := make(map[string]savedProviderConfig, len(cfg.Providers)+2)
	for k, pc := range cfg.Providers {
		if _, ok := aiProviderOptionFor(k); ok {
			providers[k] = pc
		}
	}
	if _, ok := providers[cfg.Provider]; !ok && cfg.Provider != "" {
		pc := savedProviderConfig{APIKey: cfg.APIKey, Model: cfg.Model, BaseURL: cfg.BaseURL}
		if cfg.Provider == ai.KindDeepL && pc.APIKey == "" {
			pc.APIKey = cfg.DeepLAPIKey
		}
		providers[cfg.Provider] = pc
	}
	if _, ok := providers[ai.KindDeepL]; !ok && cfg.DeepLAPIKey != "" {
		providers[ai.KindDeepL] = savedProviderConfig{APIKey: cfg.DeepLAPIKey}
	}
	for k, pc := range providers {
		if k != ai.KindCustom {
			pc.BaseURL = ""
		}
		if pc == (savedProviderConfig{}) {
			delete(providers, k)
			continue
		}
		providers[k] = pc
	}
	cfg.Providers = providers
	main := providers[cfg.Provider]
	cfg.APIKey, cfg.Model, cfg.BaseURL = main.APIKey, main.Model, main.BaseURL
	cfg.DeepLAPIKey = providers[ai.KindDeepL].APIKey
}

// savedAIFor は saved のうち kind の設定だけを、メインのプロバイダーの形（Provider・APIKey・Model・BaseURL）にして返す。
// 文章スタイルは全体の設定を使う。resolveAIConfig・buildAIProvider に渡してプロバイダーを作る。
func savedAIFor(saved savedAIConfig, kind string) savedAIConfig {
	pc := saved.Providers[kind]
	return savedAIConfig{Provider: kind, APIKey: pc.APIKey, Model: pc.Model, BaseURL: pc.BaseURL,
		WritingStyle: saved.WritingStyle, AcademicStyle: saved.AcademicStyle}
}

// resolveAIConfig は画面で保存した設定 saved に環境変数 env を合わせて、プロバイダーを作る設定と、使うキーの
// 出どころ（"saved"・環境変数名・空）を返す。優先順位は 保存したキー > 環境変数のキー > なし。
// 環境変数のキーは、そのキーが向けられたプロバイダー（custom なら同じベース URL）にだけ使い、別の送り先には送らない。
func resolveAIConfig(saved savedAIConfig, env ai.Config, getenv func(string) string) (ai.Config, string) {
	kind := saved.Provider
	cfg := ai.Config{Kind: kind, Model: saved.Model, MissingKeyEnv: ai.KeyEnvName(kind)}
	cfg.WritingStyle = saved.WritingStyle
	if cfg.WritingStyle == "" && saved.AcademicStyle {
		cfg.WritingStyle = ai.WritingStyleAcademic
	}
	sameAsEnv := env.Kind == kind
	if kind == ai.KindCustom {
		cfg.BaseURL = saved.BaseURL
		// custom の環境変数のキーは、環境変数で指定したのと同じ送り先のときだけ使う。
		sameAsEnv = sameAsEnv && strings.TrimRight(env.BaseURL, "/") == saved.BaseURL
	} else if sameAsEnv {
		// 環境変数で同じプロバイダーにプロキシ（AI_BASE_URL）を指定していれば、それを使う。
		cfg.BaseURL = env.BaseURL
	}
	if cfg.Model == "" && sameAsEnv {
		cfg.Model = env.Model
	}
	source := ""
	switch {
	case saved.APIKey != "":
		cfg.APIKey, source = saved.APIKey, "saved"
	case sameAsEnv && env.APIKey != "":
		cfg.APIKey, source = env.APIKey, ai.KeyEnvName(kind)
		if strings.TrimSpace(getenv("AI_API_KEY")) != "" {
			source = "AI_API_KEY"
		}
	case kind != ai.KindCustom:
		if name := ai.KeyEnvName(kind); name != "" {
			if v := strings.TrimSpace(getenv(name)); v != "" {
				cfg.APIKey, source = v, name
			}
		}
	}
	return cfg, source
}

// buildAIProvider は saved からプロバイダーを作る。使うキーの出どころも返す。カスタム AI の送り先を制限する
// とき（restrictAIBaseURL）は、公開されていないアドレスへ接続しないクライアントで作る（ai.Config.DenyPrivateNetwork）。
func (s *Server) buildAIProvider(saved savedAIConfig) (ai.Provider, string, error) {
	cfg, source := resolveAIConfig(saved, ai.ConfigFromEnv(), os.Getenv)
	cfg.DenyPrivateNetwork = cfg.Kind == ai.KindCustom && s.restrictAIBaseURL(cfg.BaseURL)
	p, err := ai.NewProvider(cfg)
	return p, source, err
}

// providerFromSavedAI は起動時に使うプロバイダーを返す。画面で保存した設定があればそれから作り、無ければ
// Config.AI（環境変数から作ったもの）を使う。
func (s *Server) providerFromSavedAI(logger echo.Logger) ai.Provider {
	saved, ok := s.loadAIConfig()
	if !ok {
		return s.cfg.AI
	}
	p, _, err := s.buildAIProvider(saved)
	if err != nil {
		logger.Warnf("ai: saved settings: %v", err)
		return s.cfg.AI
	}
	return p
}

// providerBox は atomic.Pointer に入れるためにプロバイダー（インターフェース）を包む。
type providerBox struct{ p ai.Provider }

// aiProvider は今使う AI プロバイダーを返す。並行に呼んでよい。
func (s *Server) aiProvider() ai.Provider {
	if b := s.aiCur.Load(); b != nil && b.p != nil {
		return b.p
	}
	return ai.NewDummy("")
}

// setAIProvider は使う AI プロバイダーを差し替える。処理中の要求は、読み出した時点のプロバイダーで最後まで動く。
func (s *Server) setAIProvider(p ai.Provider) {
	if p == nil {
		p = ai.NewDummy("")
	}
	s.aiCur.Store(&providerBox{p: p})
}

// errUnknownAIProvider は /admin/ai/* などで指定されたプロバイダーが、知らない種類だったときのエラー。
var errUnknownAIProvider = errors.New("使うAIの種類が正しくありません")

// mainAIKind はメインのプロバイダーの種類。画面で保存していなければ環境変数（AI_PROVIDER など）で決まる種類。
func mainAIKind(saved savedAIConfig, hasSaved bool) string {
	if hasSaved {
		return saved.Provider
	}
	return ai.ConfigFromEnv().Kind
}

// aiProviderFor は kind（gemini・openai・claude・custom・deepl）のプロバイダーを、保存済みのその種類の設定（無ければ
// 環境変数のキー）で作って返す。kind が空か、メインのプロバイダーと同じなら、いま使っているメインのプロバイダー
// （aiProvider）を返す。キーが無ければ ai.Dummy（ai.IsConfigured が false）を返す。知らない種類なら
// errUnknownAIProvider。並行に呼んでよい。
func (s *Server) aiProviderFor(kind string) (ai.Provider, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return s.aiProvider(), nil
	}
	kind = ai.NormalizeKind(kind)
	if _, ok := aiProviderOptionFor(kind); !ok {
		return nil, errUnknownAIProvider
	}
	saved, hasSaved := s.loadAIConfig()
	if kind == mainAIKind(saved, hasSaved) {
		return s.aiProvider(), nil
	}
	p, _, err := s.buildAIProvider(savedAIFor(saved, kind))
	return p, err
}

// aiProviderChoice は記事エディタで選べる AI の1つ。Value は /admin/ai/* の provider に送る値、Label は表示名。
type aiProviderChoice struct {
	Value, Label string
}

// aiConfiguredProviders は記事エディタで選べる（キーなどが設定されて使える）AI を返す。先頭はメインのプロバイダー
// （Value は種類。環境変数でも決まらないときは空で、provider を送らなければメインを使う）。翻訳専用の DeepL は、
// メインでなければ含めない（推敲・タイトル提案などには使えない）。
func (s *Server) aiConfiguredProviders() []aiProviderChoice {
	saved, hasSaved := s.loadAIConfig()
	mainKind := mainAIKind(saved, hasSaved)
	var out []aiProviderChoice
	if main := s.aiProvider(); ai.IsConfigured(main) {
		out = append(out, aiProviderChoice{Value: mainKind, Label: main.Name()})
	}
	for _, opt := range aiProviderOptions {
		if opt.Value == mainKind || opt.Value == ai.KindDeepL {
			continue
		}
		if p, _, err := s.buildAIProvider(savedAIFor(saved, opt.Value)); err == nil && ai.IsConfigured(p) {
			out = append(out, aiProviderChoice{Value: opt.Value, Label: p.Name()})
		}
	}
	return out
}

// aiFormError は AI の設定のフォームの入力の誤り。Kind はどのプロバイダーの欄の誤りか（全体の誤りは空）。
type aiFormError struct {
	Kind, Msg string
}

// aiFormMessages は誤りの文だけを並べる。
func aiFormMessages(errs []aiFormError) []string {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Msg
	}
	return msgs
}

// aiForm はフォームの入力から保存する設定を組み立てる。キーの欄が空なら、同じプロバイダー（custom なら同じ
// ベース URL）の保存済みのキーを引き継ぐ。プロバイダーや送り先が変わったときは引き継がない（別の会社・別の
// サーバーにキーを送らない）。custom 以外ではベース URL を保存しない。入力の誤りは利用者に見せる文で返す。
//
// プロバイダーごとの欄（api_key_<種類>・model_<種類>・base_url_<種類>・clear_key_<種類>）を受け付ける。送られて
// こなかったプロバイダーの設定は保存済みのまま残す。従来の1組の欄（api_key・model・base_url・clear_api_key）は
// メインのプロバイダー mainProvider の欄、deepl_api_key・clear_deepl_api_key は DeepL の欄として扱う（同じ
// プロバイダーの種類ごとの欄があれば、そちらを使う）。
func (s *Server) aiForm(c echo.Context, saved savedAIConfig, hasSaved bool, mainProvider string) (savedAIConfig, []aiFormError) {
	var errs []aiFormError
	form, _ := c.FormParams()
	has := func(name string) bool { _, ok := form[name]; return ok }
	next := savedAIConfig{Provider: ai.NormalizeKind(mainProvider), Providers: map[string]savedProviderConfig{}}
	if _, ok := aiProviderOptionFor(next.Provider); !ok {
		errs = append(errs, aiFormError{Msg: "使うAIの種類を選んでください"})
	}
	if !hasSaved {
		saved = savedAIConfig{}
	}

	for _, opt := range aiProviderOptions {
		kind := opt.Value
		isMain := kind == next.Provider
		// メインのプロバイダー以外の欄の誤りには、どのプロバイダーの欄かを付ける。
		prefix := ""
		if !isMain {
			prefix = opt.ShortName + "の"
		}
		fail := func(msg string) { errs = append(errs, aiFormError{Kind: kind, Msg: prefix + msg}) }
		prev := saved.Providers[kind]
		pc := savedProviderConfig{Model: prev.Model, BaseURL: prev.BaseURL}

		if name := pickField(has, "model_"+kind, isMain, "model"); name != "" {
			pc.Model = strings.TrimSpace(c.FormValue(name))
			switch {
			case len(pc.Model) > maxAIModelLength:
				fail("モデル名が長すぎます（100文字まで）")
			case strings.ContainsFunc(pc.Model, func(r rune) bool { return !validModelRune(r) }):
				fail("モデル名は半角の英数字と . _ - : / @ + で入力してください（例: " + ai.DefaultGeminiModel + "）")
			}
		}

		if kind == ai.KindCustom {
			if name := pickField(has, "base_url_"+kind, isMain, "base_url"); name != "" {
				raw := c.FormValue(name)
				if strings.TrimSpace(raw) == "" && !isMain {
					// メインでないローカル LLM は、ベース URL を空にすれば使わない設定になる。
					pc.BaseURL = ""
				} else {
					u, err := normalizeAIBaseURL(raw)
					// サーバーモードで送り先を制限するときは、平文 http の誤りより先に、公開されていないアドレス
					// （http://169.254.169.254 など）であることを伝える。
					if (err == nil || errors.Is(err, errPlainHTTPAIBaseURL)) && s.restrictAIBaseURL(u) {
						if perr := checkPublicAIBaseURL(u); perr != nil {
							err = perr
						}
					}
					if err != nil {
						fail(err.Error())
					}
					pc.BaseURL = u
				}
			}
			if isMain && pc.BaseURL == "" && !hasFailed(errs, kind) {
				_, err := normalizeAIBaseURL("")
				fail(err.Error())
			}
		} else {
			pc.BaseURL = ""
		}

		key := strings.TrimSpace(c.FormValue("api_key_" + kind))
		if key == "" && isMain {
			key = strings.TrimSpace(c.FormValue("api_key"))
		}
		if key == "" && kind == ai.KindDeepL {
			key = strings.TrimSpace(c.FormValue("deepl_api_key"))
		}
		clear := c.FormValue("clear_key_"+kind) != "" ||
			(isMain && c.FormValue("clear_api_key") != "") ||
			(kind == ai.KindDeepL && c.FormValue("clear_deepl_api_key") != "")
		switch {
		case len(key) > maxAIKeyLength:
			fail("APIキーが長すぎます")
		case strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
			fail("APIキーに空白や改行が含まれています。キーだけを貼り付けてください")
		case key != "":
			pc.APIKey = key
		case clear:
			// 「保存済みのキーを消す」: 環境変数のキー（あれば）に戻す。
		case pc.BaseURL == prev.BaseURL:
			// 同じ送り先（custom 以外はベース URL が常に空）なら、保存済みのキーを使い続ける。別の送り先には送らない。
			pc.APIKey = prev.APIKey
		}
		if pc != (savedProviderConfig{}) {
			next.Providers[kind] = pc
		}
	}

	tp := strings.TrimSpace(c.FormValue("translation_provider"))
	switch {
	case !has("translation_provider"):
		// 多言語対応が OFF の画面では翻訳の欄を出さない。欄が送られてこなければ保存済みの値を残す。
		next.TranslationProvider = saved.TranslationProvider
	case tp == "":
		next.TranslationProvider = ""
	case isTranslationKind(tp):
		next.TranslationProvider = tp
	default:
		errs = append(errs, aiFormError{Msg: "有効な翻訳プロバイダー（gemini・claude・openai・custom・deepl）を選択してください"})
	}
	next.Glossary = strings.TrimSpace(c.FormValue("glossary"))
	if msg := validateGlossary(next.Glossary); msg != "" {
		errs = append(errs, aiFormError{Msg: msg})
	}

	ws := strings.TrimSpace(c.FormValue("writing_style"))
	if ws == "" {
		ws = strings.TrimSpace(c.FormValue("ai_writing_style"))
	}
	acadVal := c.FormValue("academic_style")
	if acadVal == "" {
		acadVal = c.FormValue("ai_academic_style")
	}
	isAcad := acadVal == "1" || acadVal == "true" || acadVal == "on"
	if ws == "1" {
		ws = ai.WritingStyleAcademic
	}
	switch ws {
	case ai.WritingStyleStandard, ai.WritingStyleAcademic, ai.WritingStyleBusiness, ai.WritingStyleCreative:
		next.WritingStyle = ws
	default:
		if isAcad {
			next.WritingStyle = ai.WritingStyleAcademic
		} else {
			next.WritingStyle = ai.WritingStyleStandard
		}
	}
	next.AcademicStyle = (next.WritingStyle == ai.WritingStyleAcademic)

	normalizeSavedAI(&next)
	return next, errs
}

// validateGlossary は用語集の入力を調べ、誤りがあれば利用者に見せる文を、なければ空を返す。
// 文字数は maxAIGlossaryRunes まで。改行・タブ以外の制御文字（NUL・ESC など）は、AI へのプロンプトや画面の表示を
// 乱すため受け付けない。
func validateGlossary(g string) string {
	if !utf8.ValidString(g) {
		return "用語集に読み取れない文字が含まれています"
	}
	if n := utf8.RuneCountInString(g); n > maxAIGlossaryRunes {
		return fmt.Sprintf("用語集が長すぎます（%d文字まで。いまは%d文字です）", maxAIGlossaryRunes, n)
	}
	if strings.ContainsFunc(g, func(r rune) bool {
		return r != '\n' && r != '\r' && r != '\t' && unicode.IsControl(r)
	}) {
		return "用語集に使えない制御文字が含まれています。改行・タブ以外の特殊な文字を取り除いてください"
	}
	return ""
}

// pickField はプロバイダーごとの欄 perKind が送られていればその名前を、無くてメインのプロバイダーなら従来の欄
// legacy が送られていればその名前を返す。どちらも無ければ空（保存済みの値を残す）。
func pickField(has func(string) bool, perKind string, isMain bool, legacy string) string {
	switch {
	case has(perKind):
		return perKind
	case isMain && has(legacy):
		return legacy
	}
	return ""
}

// hasFailed は kind の欄にすでに誤りがあるか。
func hasFailed(errs []aiFormError, kind string) bool {
	for _, e := range errs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// validModelRune はモデル名に使える文字か（例: gemini-3.8-flash・llama3.1:8b・meta-llama/llama-3-8b）。
func validModelRune(r rune) bool {
	return r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-:/@+", r))
}

func isLocalOrPrivateHost(host string) bool {
	h := host
	if strings.Contains(h, ":") {
		if sh, _, err := net.SplitHostPort(h); err == nil {
			h = sh
		}
	}
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	if ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return false
}

// errPlainHTTPAIBaseURL は、外部ホストのベース URL に平文の http:// が使われていたときのエラー。
var errPlainHTTPAIBaseURL = errors.New("外部ホストへの接続には暗号化された https:// を使用してください（http:// は localhost またはプライベート IP のみ許可されています）")

// normalizeAIBaseURL は custom のベース URL を検査し、末尾の / を除いて返す。
// 外部ホストへの接続で平文 http:// が使われることを防ぐため、http:// は localhost またはプライベート IP のみ許可する。
func normalizeAIBaseURL(raw string) (string, error) {
	const msg = "ベースURLは http:// または https:// で始まる URL で入力してください（例: Ollama なら http://localhost:11434/v1）"
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", errors.New("ローカルLLM・その他の互換APIでは、ベースURLを入力してください（例: Ollama なら http://localhost:11434/v1）")
	}
	if len(raw) > maxAIBaseURLLength {
		return raw, errors.New("ベースURLが長すぎます")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return raw, errors.New(msg)
	}
	if u.Scheme == "http" && !isLocalOrPrivateHost(u.Host) {
		return raw, errPlainHTTPAIBaseURL
	}
	return raw, nil
}

// allowPrivateAIBaseURLEnv は、サーバーモードでもカスタム AI（ローカルLLM・その他の互換API）のベース URL に、
// このサーバー自身や社内ネットワークのアドレスを使えるようにする環境変数。同じサーバーで Ollama などを動かすときに
// 1 を設定する。
const allowPrivateAIBaseURLEnv = "ALLOW_PRIVATE_AI_BASE_URL"

// errPrivateAIBaseURL は、サーバーモードでカスタム AI のベース URL が公開されていないアドレスを指していたときに
// 利用者に見せる文。
var errPrivateAIBaseURL = errors.New("サーバーモードでは、このサーバー自身や社内ネットワークのアドレス（localhost・127.0.0.1・" +
	"192.168.x.x・169.254.169.254 など）はベースURLに使えません。同じサーバーで動かすLLMを使うときは、環境変数 " +
	allowPrivateAIBaseURLEnv + "=1 を設定して起動し直してください")

// restrictAIBaseURL は、カスタム AI のベース URL baseURL に、ループバック・私有・リンクローカル（クラウドのメタデータ
// 169.254.169.254 を含む）など公開されていないアドレスを使わせないか（SSRF を防ぐか）を返す。
// デスクトップモードは自分の PC のローカル LLM（Ollama・LM Studio）に繋ぐため制限しない。サーバーモードは
// ALLOW_PRIVATE_AI_BASE_URL=1 のときと、運用者が環境変数 AI_BASE_URL で指定したのと同じ送り先のときだけ制限しない
// （管理画面から入力した送り先で、サーバーの内部やクラウドのメタデータへ接続させない）。
func (s *Server) restrictAIBaseURL(baseURL string) bool {
	if s.cfg.Desktop || strings.TrimSpace(os.Getenv(allowPrivateAIBaseURLEnv)) == "1" {
		return false
	}
	env := strings.TrimRight(strings.TrimSpace(os.Getenv("AI_BASE_URL")), "/")
	return env == "" || strings.TrimRight(strings.TrimSpace(baseURL), "/") != env
}

// checkPublicAIBaseURL は normalizeAIBaseURL を通ったベース URL のホストが localhost か、公開されていない IP アドレス
// なら errPrivateAIBaseURL を返す。名前（localhost 以外）は、接続のたびに解決したアドレスを確かめる
// （ai.Config.DenyPrivateNetwork）。
func checkPublicAIBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || ai.CheckPublicHost(u.Hostname()) != nil {
		return errPrivateAIBaseURL
	}
	return nil
}

// aiTestErrorMessage は接続テストの失敗を利用者に見せる文にする。公開されていないアドレスへの接続を断ったときは、
// その理由と ALLOW_PRIVATE_AI_BASE_URL の案内を出す。
func aiTestErrorMessage(err error) string {
	if errors.Is(err, ai.ErrPrivateAddress) {
		return errPrivateAIBaseURL.Error()
	}
	_, msg := aiErrorStatus(err)
	return msg
}

// envAIKeys はキーが設定されている環境変数の名前を返す（値は返さない）。
func envAIKeys() []string {
	var names []string
	for _, n := range []string{"GEMINI_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AI_API_KEY", "DEEPL_API_KEY"} {
		if strings.TrimSpace(os.Getenv(n)) != "" {
			names = append(names, n)
		}
	}
	return names
}

// aiKeySourceLabel は使っているキーの出どころを画面に出す文にする。
func aiKeySourceLabel(source string) string {
	switch source {
	case "":
		return "なし"
	case "saved":
		return "この画面で保存したキー"
	}
	return "環境変数 " + source
}

// maskAIKey は API キーの末尾4文字のみを残し、他をマスクして返す。
func maskAIKey(key string) string {
	if len(key) <= 4 {
		return strings.Repeat("*", 8)
	}
	return strings.Repeat("*", 8) + key[len(key)-4:]
}

func (s *Server) aiSettingsData(form savedAIConfig, errs []string) aiSettingsData {
	saved, hasSaved := s.loadAIConfig()
	current := s.aiProvider()
	d := aiSettingsData{
		Providers: aiProviderOptions, Form: form, HasSaved: hasSaved,
		HasSavedKey: hasSaved && saved.APIKey != "",
		Current:     current.Name(), Configured: ai.IsConfigured(current),
		EnvKeys: envAIKeys(), Errors: errs,

		MultilingualEnabled: s.isMultilingualEnabled(),
	}
	if d.HasSavedKey {
		d.KeyPreview = maskAIKey(saved.APIKey)
	}
	if hasSaved && saved.DeepLAPIKey != "" {
		d.HasSavedDeepLKey = true
		d.DeepLKeyPreview = maskAIKey(saved.DeepLAPIKey)
	}
	if hasSaved {
		_, source := resolveAIConfig(saved, ai.ConfigFromEnv(), os.Getenv)
		d.KeySource = aiKeySourceLabel(source)
	} else if d.Configured {
		d.KeySource = "環境変数"
	} else {
		d.KeySource = aiKeySourceLabel("")
	}
	env := ai.ConfigFromEnv()
	for _, opt := range aiProviderOptions {
		kind := opt.Value
		in := form.Providers[kind]
		item := aiProviderConfigItem{Key: kind, Label: opt.Label, KeyHint: opt.KeyHint, KeyURL: opt.KeyURL, KeyEnv: opt.KeyEnv,
			DefaultModel: opt.DefaultModel, Models: opt.Models, KeyOptional: opt.KeyOptional, Model: in.Model, BaseURL: in.BaseURL}
		if key := saved.Providers[kind].APIKey; hasSaved && key != "" {
			item.HasSavedKey, item.KeyPreview = true, maskAIKey(key)
		}
		cfg, source := resolveAIConfig(savedAIFor(saved, kind), env, os.Getenv)
		if p, err := ai.NewProvider(cfg); err == nil {
			item.Configured = ai.IsConfigured(p)
		}
		item.KeySource = aiKeySourceLabel(source)
		d.ProviderConfigs = append(d.ProviderConfigs, item)
	}
	// 入力欄の値はキーを含めない。
	d.Form.APIKey, d.Form.DeepLAPIKey = "", ""
	if len(form.Providers) > 0 {
		d.Form.Providers = make(map[string]savedProviderConfig, len(form.Providers))
		for k, pc := range form.Providers {
			pc.APIKey = ""
			d.Form.Providers[k] = pc
		}
	}
	if d.Form.WritingStyle == "" {
		if d.Form.AcademicStyle {
			d.Form.WritingStyle = ai.WritingStyleAcademic
		} else {
			d.Form.WritingStyle = ai.WritingStyleStandard
		}
	}
	if d.Form.Provider == "" {
		d.Form.Provider = ai.KindGemini
		if env.Kind != "" {
			d.Form.Provider, d.Form.Model, d.Form.BaseURL = env.Kind, env.Model, env.BaseURL
			for i := range d.ProviderConfigs {
				if it := &d.ProviderConfigs[i]; it.Key == env.Kind && it.Model == "" && it.BaseURL == "" {
					it.Model = env.Model
					if env.Kind == ai.KindCustom {
						it.BaseURL = env.BaseURL
					}
				}
			}
		}
	}
	return d
}

// redactForNonAdmin は管理者以外（スタイル設定だけを使う人）に渡さない値を消す。プロバイダーの一覧
// （ProviderConfigs。翻訳用 AI の選択肢に使う）は残し、キーの伏せ字・出どころとローカル LLM の送り先は出さない。
func (d *aiSettingsData) redactForNonAdmin() {
	d.KeyPreview, d.DeepLKeyPreview, d.KeySource, d.EnvKeys = "", "", "", nil
	d.Form.BaseURL = ""
	for k, pc := range d.Form.Providers {
		pc.BaseURL = ""
		d.Form.Providers[k] = pc
	}
	for i := range d.ProviderConfigs {
		it := &d.ProviderConfigs[i]
		it.KeyPreview, it.KeySource, it.BaseURL = "", "", ""
	}
}

// handleAISettings は「設定」→「AI」の画面を返す。
func (s *Server) handleAISettings(c echo.Context) error {
	saved, _ := s.loadAIConfig()
	data := s.aiSettingsData(saved, nil)
	data.localize(string(language(c)))
	u, _ := auth.UserFromContext(c)
	data.IsAdmin = u != nil && u.HasRole(auth.RoleAdmin)
	if !data.IsAdmin {
		data.redactForNonAdmin()
	}
	title := pageTitle(c, "ai_settings_title")
	if !data.IsAdmin {
		title = pageTitle(c, "ai_style_settings")
	}
	return s.render(c, http.StatusOK, "settings_ai", title, data)
}

// handleAISettingsSave は AI の設定を保存し、使うプロバイダーをすぐに差し替える。
func (s *Server) handleAISettingsSave(c echo.Context) error {
	u, _ := auth.UserFromContext(c)
	isAdmin := u != nil && u.HasRole(auth.RoleAdmin)

	saved, hasSaved := s.loadAIConfig()

	// 管理者以外のユーザー（editor / writer）は、執筆スタイル・用語集・優先翻訳エンジンのみ設定可能
	if !isAdmin {
		if !hasSaved {
			if env := ai.ConfigFromEnv(); env.Kind != "" {
				saved.Provider = env.Kind
				saved.Model = env.Model
				saved.BaseURL = env.BaseURL
			} else {
				saved.Provider = ai.KindGemini
			}
		}
		ws := strings.TrimSpace(c.FormValue("writing_style"))
		if ws == "" {
			ws = strings.TrimSpace(c.FormValue("ai_writing_style"))
		}
		acadVal := c.FormValue("academic_style")
		if acadVal == "" {
			acadVal = c.FormValue("ai_academic_style")
		}
		isAcad := acadVal == "1" || acadVal == "true" || acadVal == "on"
		switch ws {
		case ai.WritingStyleStandard, ai.WritingStyleAcademic, ai.WritingStyleBusiness, ai.WritingStyleCreative:
			saved.WritingStyle = ws
		default:
			if isAcad {
				saved.WritingStyle = ai.WritingStyleAcademic
			} else {
				saved.WritingStyle = ai.WritingStyleStandard
			}
		}
		saved.AcademicStyle = (saved.WritingStyle == ai.WritingStyleAcademic)
		saved.Glossary = strings.TrimSpace(c.FormValue("glossary"))
		if msg := validateGlossary(saved.Glossary); msg != "" {
			// 誤りの画面も通常の画面（handleAISettings）と同じく、管理者向けの値（キーの出どころ・環境変数の名前・
			// ローカル LLM の送り先）を消し、見出しも「AI執筆支援設定」にする。
			data := s.aiSettingsData(saved, []string{msg})
			data.localize(string(language(c)))
			data.IsAdmin = false
			data.redactForNonAdmin()
			return s.renderError(c, http.StatusUnprocessableEntity, "settings_ai", pageTitle(c, "ai_style_settings"), msg, data)
		}

		// 多言語対応が OFF の画面では翻訳の欄を出さない。欄が送られてこなければ保存済みの値を残す。
		tp := strings.TrimSpace(c.FormValue("translation_provider"))
		if formHas(c, "translation_provider") && (tp == "" || isTranslationKind(tp)) {
			saved.TranslationProvider = tp
		}
		normalizeSavedAI(&saved)

		b, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := upsertSetting(s.db, aiSettingKey, string(b)); err != nil {
			return err
		}
		s.setAIProvider(s.providerFromSavedAI(c.Logger()))
		c.Logger().Infof("ai: style settings saved by %s: style=%s", u.Username, saved.WritingStyle)
		return c.Redirect(http.StatusSeeOther, aiSettingsPath+"?flash=ai_saved")
	}

	next, formErrs := s.aiForm(c, saved, hasSaved, c.FormValue("provider"))
	errs := aiFormMessages(formErrs)
	var p ai.Provider
	if len(errs) == 0 {
		var err error
		if p, _, err = s.buildAIProvider(next); err != nil {
			c.Logger().Warnf("ai: build provider: %v", err)
			errs = append(errs, "AIの設定を読み込めませんでした。入力を確かめてください")
		} else if !ai.IsConfigured(p) {
			opt, _ := aiProviderOptionFor(next.Provider)
			errs = append(errs, "APIキーを入力してください（環境変数 "+opt.KeyEnv+" も設定されていません）")
		}
	}
	if len(errs) > 0 {
		data := s.aiSettingsData(next, errs)
		data.localize(string(language(c)))
		data.IsAdmin = isAdmin
		return s.renderError(c, http.StatusUnprocessableEntity, "settings_ai", pageTitle(c, "ai_settings_title"), errs[0], data)
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := upsertSetting(s.db, aiSettingKey, string(b)); err != nil {
		return err
	}
	s.setAIProvider(p)
	c.Logger().Infof("ai: settings saved: %s", p.Name())
	return c.Redirect(http.StatusSeeOther, aiSettingsPath+"?flash=ai_saved")
}

// handleAISettingsReset は画面で保存した AI の設定を消し、起動時に環境変数から作ったプロバイダーに戻す。
func (s *Server) handleAISettingsReset(c echo.Context) error {
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, aiSettingKey); err != nil {
		return err
	}
	s.setAIProvider(s.cfg.AI)
	return c.Redirect(http.StatusSeeOther, aiSettingsPath+"?flash=ai_reset")
}

// aiTestResponse は接続テストの結果。Message は利用者に見せる文で、API の生のエラーは含めない。
type aiTestResponse struct {
	OK        bool     `json:"ok"`
	Provider  string   `json:"provider,omitempty"`
	Titles    []string `json:"titles,omitempty"`
	ElapsedMS int64    `json:"elapsed_ms"`
	Message   string   `json:"message,omitempty"`
}

// handleAISettingsTest はフォームに入力中の設定（保存前でもよい）で、タイトル提案を1回試し、結果と応答時間を
// JSON で返す。キーの欄が空なら保存のときと同じ規則でキーを選ぶ（別のプロバイダー・送り先には送らない）。
func (s *Server) handleAISettingsTest(c echo.Context) error {
	saved, hasSaved := s.loadAIConfig()
	// test_provider があれば、そのプロバイダーのカードの欄（api_key_<種類> など）で試す。無ければメインのプロバイダー。
	target := ai.NormalizeKind(c.FormValue("test_provider"))
	mainProvider := c.FormValue("provider")
	if strings.TrimSpace(mainProvider) == "" && target != "" {
		mainProvider = target
	}
	next, formErrs := s.aiForm(c, saved, hasSaved, mainProvider)
	kind := next.Provider
	if target != "" {
		if _, ok := aiProviderOptionFor(target); !ok {
			return c.JSON(http.StatusOK, aiTestResponse{Message: "使うAIの種類を選んでください"})
		}
		kind = target
	}
	// 試すプロバイダーの欄と、全体の欄の誤りだけを見る（ほかのカードの書きかけの欄で試せなくならないようにする）。
	for _, e := range formErrs {
		if e.Kind == "" || e.Kind == kind {
			return c.JSON(http.StatusOK, aiTestResponse{Message: e.Msg})
		}
	}
	if kind == ai.KindCustom && next.Providers[kind].BaseURL == "" {
		_, err := normalizeAIBaseURL("")
		return c.JSON(http.StatusOK, aiTestResponse{Message: err.Error()})
	}
	p, _, err := s.buildAIProvider(savedAIFor(next, kind))
	if err != nil {
		c.Logger().Warnf("ai: test: build provider: %v", err)
		return c.JSON(http.StatusOK, aiTestResponse{Message: "AIの設定を読み込めませんでした。入力を確かめてください"})
	}
	if !ai.IsConfigured(p) {
		return c.JSON(http.StatusOK, aiTestResponse{Message: "APIキーを入力してください"})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), aiTestTimeout)
	defer cancel()
	start := time.Now()
	var titles []string
	if kind == ai.KindDeepL {
		translated, err := p.Translate(ctx, "こんにちは世界", "ja", "en", ai.TranslateOptions{})
		res := aiTestResponse{Provider: p.Name(), ElapsedMS: time.Since(start).Milliseconds()}
		if err != nil {
			c.Logger().Warnf("ai: test: %s: %v", p.Name(), err)
			res.Message = aiTestErrorMessage(err)
			return c.JSON(http.StatusOK, res)
		}
		res.OK, res.Titles = true, []string{translated}
		return c.JSON(http.StatusOK, res)
	}

	titles, err = p.SuggestTitles(ctx, aiTestSample)
	res := aiTestResponse{Provider: p.Name(), ElapsedMS: time.Since(start).Milliseconds()}
	if err != nil {
		// 生のエラー（Google のメッセージなど）はログにだけ残す。
		c.Logger().Warnf("ai: test: %s: %v", p.Name(), err)
		res.Message = aiTestErrorMessage(err)
		return c.JSON(http.StatusOK, res)
	}
	res.OK, res.Titles = true, titles
	return c.JSON(http.StatusOK, res)
}

// parseGlossary は 1行に「原文=訳文」形式の用語集テキストを map に変換する。
func parseGlossary(text string) map[string]string {
	m := make(map[string]string)
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		parts := strings.SplitN(l, "=", 2)
		if len(parts) == 2 {
			src := strings.TrimSpace(parts[0])
			tgt := strings.TrimSpace(parts[1])
			if src != "" && tgt != "" {
				m[src] = tgt
			}
		}
	}
	return m
}

// buildTranslationProvider は翻訳実行時に使用する AI プロバイダーを返す。requested（翻訳の要求で指定された種類）、
// 翻訳用 AI（TranslationProvider）、メインのプロバイダーの順に決め、保存済みのその種類の設定で作る（aiProviderFor）。
func (s *Server) buildTranslationProvider(requested string) (ai.Provider, error) {
	saved, _ := s.loadAIConfig()
	providerKind := strings.TrimSpace(requested)
	if providerKind != "" && !isTranslationKind(providerKind) {
		return nil, fmt.Errorf("無効な翻訳プロバイダーです: %s", providerKind)
	}
	if providerKind == "" {
		providerKind = saved.TranslationProvider
	}
	if providerKind == "" {
		providerKind = saved.Provider
	}
	if providerKind == "" {
		return s.aiProvider(), nil
	}
	return s.aiProviderFor(providerKind)
}
