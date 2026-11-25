// Package ai は記事の執筆を支援するマルチ AI プロバイダー（Gemini / OpenAI / Claude / OpenAI 互換のローカル LLM）の
// クライアントを提供する。外部 SDK を使わず net/http と encoding/json だけで各社の REST API を呼び出す。
// API キーが未設定のときは Dummy が ErrNotConfigured を返し、管理画面は設定ガイドを表示する。
package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// 差分チャンクの種類。ProofreadDiff.Type に入る。
const (
	DiffDelete = -1
	DiffEqual  = 0
	DiffInsert = 1
)

// ProofreadDiff は推敲前後のテキストの差分の1チャンク。
type ProofreadDiff struct {
	// Type は DiffDelete（元テキストにだけある）・DiffEqual（一致）・DiffInsert（推敲後にだけある）のいずれか。
	Type int    `json:"type"`
	Text string `json:"text"`
}

// ProofreadResult は推敲の結果。Diffs を Type が DiffInsert 以外の順に連結すると OriginalText、
// DiffDelete 以外の順に連結すると ImprovedText になる。
type ProofreadResult struct {
	OriginalText string          `json:"original_text"`
	ImprovedText string          `json:"improved_text"`
	Diffs        []ProofreadDiff `json:"diffs"`
	Explanation  string          `json:"explanation,omitempty"`
}

// DraftResult は対話メモから生成した記事の下書き。
type DraftResult struct {
	Title   string `json:"title"`
	Slug    string `json:"slug"`
	Content string `json:"content"`
	Summary string `json:"summary"`
}

// スタイルのプリセット値。TranslateOptions.Style に渡す。
const (
	StyleProfessional = "professional"
	StyleFriendly     = "friendly"
	StyleCompelling   = "compelling"
	StyleConcise      = "concise"
	StyleNatural      = "natural"
)

// 文章トーン＆スタイル（Writing Style）のプリセット値。
const (
	WritingStyleStandard = "standard"
	WritingStyleAcademic = "academic"
	WritingStyleBusiness = "business"
	WritingStyleCreative = "creative"
)

// TranslateOptions は翻訳時のオプション。
type TranslateOptions struct {
	Glossary      map[string]string // 専門用語辞書（例: "人工知能": "Artificial Intelligence"）
	Academic      bool              // 後方互換性用（学術エディトリアルトーンの強制）
	WritingStyle  string            // 文章トーン＆スタイル（"standard", "academic", "business", "creative"）
	Style         string            // "professional", "friendly", "compelling", "concise", "natural"
	Instruction   string            // ユーザーからの追加こだわり指示（任意）
	SiteContext   string            // サイト名やサイト説明文（自動注入）
	BeforeContext string            // 選択テキストの前の文脈（任意）
	AfterContext  string            // 選択テキストの後の文脈（任意）
}

// Provider は AI 執筆アシスタントの共通インターフェース。
type Provider interface {
	// Name は画面やログに出すプロバイダー名（例: "Gemini (gemini-3.8-flash)"）。
	Name() string
	// Proofread は誤字脱字を直し、文章を推敲した結果と差分を返す。
	Proofread(ctx context.Context, text string) (*ProofreadResult, error)
	// SuggestTitles は記事本文から魅力的なタイトル候補（最大 TitleCount 件）を返す。
	SuggestTitles(ctx context.Context, text string) ([]string, error)
	// Summarize は記事本文からリード文を返す。
	Summarize(ctx context.Context, text string) (string, error)
	// Translate はテキストを srcLang から targetLang に翻訳する。
	Translate(ctx context.Context, text string, srcLang, targetLang string, opts TranslateOptions) (string, error)
}

// FactChecker は記事本文の事実関係を検証する。NewProvider が返すすべてのプロバイダー（Dummy を含む）が満たす。
// Provider に含めないのは、Provider だけを実装した既存の実装（テスト用の偽物など）を壊さないため。
// 呼び出し側は p.(FactChecker) で取り出す。
type FactChecker interface {
	// FactCheck は本文から検証が必要な記述を抜き出し、それぞれの判定と根拠を返す。
	// Gemini は Google 検索で裏取りし（Grounded=true）、それ以外は AI の知識だけで判定する。
	FactCheck(ctx context.Context, text string) (*FactCheckResult, error)
}

// AcademicAssistant は学術・広報向けの推敲を行う。NewProvider が返すすべてのプロバイダー（Dummy を含む）が満たす。
// 呼び出し側は p.(AcademicAssistant) で取り出す。
type AcademicAssistant interface {
	// Simplify は専門用語を targetAudience（AudienceHighSchool・AudienceGeneral または自由記述。空なら高校生〜一般読者）
	// が理解できる言葉に書き直した結果と差分を返す。
	Simplify(ctx context.Context, text, targetAudience string) (*ProofreadResult, error)
	// GenerateAbstract は研究・活動記事の本文から 200〜400 文字の学術的なアブストラクトを返す。
	GenerateAbstract(ctx context.Context, text string) (string, error)
	// StyleCheck は表記ゆれ・用字用語を guide（空なら既定の用字用語ルール）に合わせて直した結果と差分を返す。
	StyleCheck(ctx context.Context, text, guide string) (*ProofreadResult, error)
}

// DraftGenerator は記事のテーマと取材メモから下書きを生成する。
type DraftGenerator interface {
	GenerateDraft(ctx context.Context, topic, notes string) (*DraftResult, error)
}

// fullProvider は NewProvider が返すプロバイダーが満たすインターフェース。
type fullProvider interface {
	Provider
	FactChecker
	AcademicAssistant
	DraftGenerator
}

var (
	_ fullProvider = (*Gemini)(nil)
	_ fullProvider = (*OpenAI)(nil)
	_ fullProvider = (*Claude)(nil)
	_ fullProvider = (*Dummy)(nil)
)

// プロバイダーの種類（AI_PROVIDER の値）。
const (
	KindGemini = "gemini"
	KindOpenAI = "openai"
	KindClaude = "claude"
	KindCustom = "custom"
	KindDeepL  = "deepl"
)

// 各プロバイダーの既定のモデル。AI_MODEL（または管理画面の「設定」→「AI」）で上書きできる。
// Gemini は既定のモデルが見つからない（404）ときだけ、軽量なフォールバックのモデルで再試行する。
const (
	DefaultGeminiModel         = "gemini-3.8-flash"
	DefaultGeminiFallbackModel = "gemini-3.5-flash-lite"
	DefaultOpenAIModel         = "gpt-4o-mini"
	DefaultClaudeModel         = "claude-3-5-haiku-20241022"
)

// TitleCount は SuggestTitles が提案するタイトルの件数。
const TitleCount = 5

// DefaultTimeout は AI API の1回の呼び出しの既定のタイムアウト。管理画面サーバーの WriteTimeout（60秒）より短くする。
const DefaultTimeout = 45 * time.Second

// maxResponseBytes は AI API のレスポンスとして読み込む上限。
const maxResponseBytes = 4 << 20

var (
	// ErrNotConfigured は API キー等が未設定で AI を利用できないことを表す。
	ErrNotConfigured = errors.New("ai: provider is not configured")
	// ErrEmptyResponse は AI が空の応答を返したことを表す。
	ErrEmptyResponse = errors.New("ai: empty response")
	// ErrTruncated は AI の応答が出力上限で途中までになったことを表す。
	ErrTruncated = errors.New("ai: response was truncated")
	// ErrProtectedContentMissing は翻訳応答から保護した記法が失われたことを表す。
	ErrProtectedContentMissing = errors.New("ai: protected content missing from translation")
	// ErrNotSupported は指定された操作をこのプロバイダーがサポートしていないことを表す。
	ErrNotSupported = errors.New("ai: operation not supported by this provider")
)

// APIError は AI API が成功以外のステータスを返したことを表す。Message に API キーは含めない。
// Message・Status・Reason は API の生の応答なので、ログにだけ残し、利用者の画面には出さない。
type APIError struct {
	Provider   string
	StatusCode int
	Message    string
	// Status は Google 形式のエラーの error.status（例: INVALID_ARGUMENT・FAILED_PRECONDITION）。無ければ空。
	Status string
	// Reason は Google 形式のエラーの error.details[].reason（例: API_KEY_INVALID）。無ければ空。
	Reason string
}

func (e *APIError) Error() string {
	var detail string
	if e.Status != "" || e.Reason != "" {
		detail = " [" + strings.Trim(e.Status+" "+e.Reason, " ") + "]"
	}
	return fmt.Sprintf("ai: %s API returned %d%s: %s", e.Provider, e.StatusCode, detail, e.Message)
}

// authFailureReasons は、HTTP 400 でも API キーの無効・未認可を表す Google のエラーの reason・status。
// Gemini（Google AI Studio）は無効なキーや、サービスアカウントへの紐付けが済んでいない Auth キー（AQ. で始まる）に
// 401・403 ではなく 400 を返す。
var authFailureReasons = map[string]bool{
	"API_KEY_INVALID":               true,
	"API_KEY_EXPIRED":               true,
	"API_KEY_SERVICE_BLOCKED":       true,
	"API_KEY_HTTP_REFERRER_BLOCKED": true,
	"API_KEY_IP_ADDRESS_BLOCKED":    true,
	"ACCESS_TOKEN_TYPE_UNSUPPORTED": true,
	"SERVICE_DISABLED":              true,
	"FAILED_PRECONDITION":           true,
	"PERMISSION_DENIED":             true,
	"UNAUTHENTICATED":               true,
}

// IsAuthError は err が API キーの無効・期限切れ・未認可による失敗かを返す。401・403 のほか、
// Google が 400 で返す API_KEY_INVALID・FAILED_PRECONDITION などもキーの問題として扱う。
func IsAuthError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		if authFailureReasons[apiErr.Reason] || authFailureReasons[apiErr.Status] {
			return true
		}
		msg := strings.ToLower(apiErr.Message)
		return strings.Contains(msg, "api key") || strings.Contains(msg, "api_key")
	}
	return false
}

// Config はプロバイダーの設定。
type Config struct {
	// Kind は KindGemini・KindOpenAI・KindClaude・KindCustom のいずれか。
	Kind string
	// APIKey は API キー。KindCustom（ローカル LLM）では省略できる。
	APIKey string
	// Model は使用するモデル。省略すると各プロバイダーの既定のモデルを使う。
	Model string
	// BaseURL は API のベース URL（例: http://localhost:11434/v1）。KindCustom では必須。
	// それ以外ではプロキシやテスト用に既定のエンドポイントを差し替える。
	BaseURL string
	// HTTPClient は API 呼び出しに使うクライアント。省略すると Timeout 付きのクライアントを作る。
	HTTPClient *http.Client
	// Timeout は1回の呼び出しのタイムアウト。既定は DefaultTimeout。
	Timeout time.Duration
	// MissingKeyEnv は Kind に必要な API キーが未設定のとき、設定ガイドで案内する環境変数名。
	MissingKeyEnv string
	// WritingStyle はプロバイダーの既定の文章トーン＆スタイル（standard, academic, business, creative）。
	WritingStyle string
	// DenyPrivateNetwork が true なら、ループバック・私有・リンクローカル（クラウドのメタデータ 169.254.169.254 を
	// 含む）など、インターネットに公開されていないアドレスへは接続しない（ErrPrivateAddress）。名前は接続のたびに
	// 解決したアドレスを確かめ（DNS リバインディング対策）、そうしたアドレスへのリダイレクトも断る。サーバーモードで、
	// 管理画面から入力したベース URL による SSRF を防ぐために使う。HTTPClient を指定したときは使わない。
	DenyPrivateNetwork bool
}

// applyWritingStyle は p に cfg.WritingStyle を反映する。
func applyWritingStyle(p Provider, style string) Provider {
	if style == "" {
		return p
	}
	switch v := p.(type) {
	case *Gemini:
		v.writingStyle = style
	case *OpenAI:
		v.writingStyle = style
	case *Claude:
		v.writingStyle = style
	}
	return p
}

// NewProvider は cfg に従ってプロバイダーを作る。Kind が空、または必要な API キーが無ければ Dummy を返す。
func NewProvider(cfg Config) (Provider, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultHTTPClient(cfg)
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	// 貼り付けたキーに付きがちな前後の空白・改行を除く。キーの形式（Google の AIza… と、新しい Auth キーの AQ.…
	// など）は確かめず、そのまま API に送る（形式は各社が予告なく変えるため）。
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.Model = strings.TrimSpace(cfg.Model)
	switch cfg.Kind {
	case "":
		return NewDummy(cfg.MissingKeyEnv), nil
	case KindGemini:
		if cfg.APIKey == "" {
			return NewDummy("GEMINI_API_KEY"), nil
		}
		return applyWritingStyle(NewGemini(cfg), cfg.WritingStyle), nil
	case KindOpenAI:
		if cfg.APIKey == "" {
			return NewDummy("OPENAI_API_KEY"), nil
		}
		return applyWritingStyle(NewOpenAI(cfg), cfg.WritingStyle), nil
	case KindClaude:
		if cfg.APIKey == "" {
			return NewDummy("ANTHROPIC_API_KEY"), nil
		}
		return applyWritingStyle(NewClaude(cfg), cfg.WritingStyle), nil
	case KindCustom:
		if cfg.BaseURL == "" {
			return NewDummy("AI_BASE_URL"), nil
		}
		return applyWritingStyle(NewCustom(cfg), cfg.WritingStyle), nil
	case KindDeepL:
		if cfg.APIKey == "" {
			return NewDummy("DEEPL_API_KEY"), nil
		}
		return NewDeepL(cfg), nil
	}
	return NewDummy(cfg.MissingKeyEnv), fmt.Errorf("ai: unknown AI_PROVIDER %q (want gemini, openai, claude, custom or deepl)", cfg.Kind)
}

// NewProviderFromEnv は環境変数からプロバイダーを作る。
//
//   - AI_PROVIDER: gemini / openai / claude / custom。省略すると設定済みの API キーから自動で選ぶ
//     （GEMINI_API_KEY → OPENAI_API_KEY → ANTHROPIC_API_KEY → AI_BASE_URL の順）。
//   - GEMINI_API_KEY / OPENAI_API_KEY / ANTHROPIC_API_KEY: 各社の API キー。
//   - AI_API_KEY: 選んだプロバイダーの API キー（各社の変数より優先。custom ではこちらを使う）。
//   - AI_MODEL: モデル名。AI_BASE_URL: API のベース URL（custom では必須）。
//
// 何も設定されていなければ Dummy を返す。AI_PROVIDER が不正なときは Dummy とエラーを返す。
func NewProviderFromEnv() (Provider, error) {
	return NewProvider(ConfigFromEnv())
}

// ConfigFromEnv は環境変数（NewProviderFromEnv と同じ変数）から Config を組み立てる。
func ConfigFromEnv() Config { return configFromEnv(os.Getenv) }

// keyEnv はプロバイダーごとの API キーの環境変数名。KindCustom は AI_API_KEY だけを使う。
var keyEnv = map[string]string{
	KindGemini: "GEMINI_API_KEY",
	KindOpenAI: "OPENAI_API_KEY",
	KindClaude: "ANTHROPIC_API_KEY",
	KindDeepL:  "DEEPL_API_KEY",
}

// KeyEnvName は kind の API キーを設定する環境変数名を返す（KindCustom なら AI_API_KEY、不明なら空）。
func KeyEnvName(kind string) string {
	if kind == KindCustom {
		return "AI_API_KEY"
	}
	return keyEnv[kind]
}

// NormalizeKind は AI_PROVIDER などの表記ゆれ（大文字・別名）を Kind に揃える。
func NormalizeKind(s string) string { return normalizeKind(s) }

// configFromEnv は getenv で読んだ環境変数から Config を組み立てる。
func configFromEnv(getenv func(string) string) Config {
	cfg := Config{
		Kind:    normalizeKind(getenv("AI_PROVIDER")),
		Model:   strings.TrimSpace(getenv("AI_MODEL")),
		BaseURL: strings.TrimSpace(getenv("AI_BASE_URL")),
	}
	if cfg.Kind == "" {
		for _, k := range []string{KindGemini, KindOpenAI, KindClaude} {
			if getenv(keyEnv[k]) != "" {
				cfg.Kind = k
				break
			}
		}
		if cfg.Kind == "" && cfg.BaseURL != "" {
			cfg.Kind = KindCustom
		}
	}
	if cfg.Kind == "" {
		cfg.MissingKeyEnv = "GEMINI_API_KEY"
		return cfg
	}
	cfg.APIKey = strings.TrimSpace(getenv("AI_API_KEY"))
	if cfg.APIKey == "" && keyEnv[cfg.Kind] != "" {
		cfg.APIKey = strings.TrimSpace(getenv(keyEnv[cfg.Kind]))
	}
	return cfg
}

// normalizeKind は AI_PROVIDER の表記ゆれ（大文字・別名）を Kind に揃える。
func normalizeKind(s string) string {
	switch k := strings.ToLower(strings.TrimSpace(s)); k {
	case "google":
		return KindGemini
	case "anthropic":
		return KindClaude
	case "ollama", "local", "lmstudio", "openai-compatible", "openai_compatible":
		return KindCustom
	default:
		return k
	}
}

// IsConfigured は p が実際に AI API を呼び出せる（Dummy でない）かを返す。
func IsConfigured(p Provider) bool {
	if p == nil {
		return false
	}
	_, dummy := p.(*Dummy)
	return !dummy
}

// 推敲・タイトル提案・要約のプロンプト。基本の文言に、Markdown を壊さないための指示を足している。
const (
	proofreadPrompt = "あなたはWebサイトを支える優秀な編集者です。書き手が書いた文章を尊重しつつ、" +
		"誤字脱字の修正、より自然で伝わりやすい語彙・文脈の推敲を行ってください。出力は推敲後のテキストのみを返してください。\n" +
		"- 解説・前置き・コードブロックの囲みは付けないでください。\n" +
		"- Markdown 記法（見出し・リスト・リンク・画像）と URL はそのまま維持してください。\n" +
		"- 段落や改行の構成は大きく変えないでください。"
	titlesPrompt = "以下の記事本文を読み、読者の興味を強く惹きつける、SEOにも強い魅力的なタイトル候補を厳密に5案提案してください。" +
		"各案を改行で区切って出力してください。\n" +
		"- 番号・記号・解説は付けず、タイトルだけを1行に1案ずつ出力してください。"
	summaryPrompt = "以下の記事本文から、読者が読み進めたくなる魅力的で簡潔な導入文（リード文・150〜200文字程度）を作成してください。\n" +
		"- リード文だけを出力し、見出し・前置き・解説は付けないでください。"
)

// completer は system プロンプトと本文を1往復の会話として送り、応答テキストを返す各社 API の最小単位。
type completer interface {
	complete(ctx context.Context, system, user string) (completion, error)
}

// completion は各 API の応答テキストと終了理由。終了理由の判定は共通層で行う。
type completion struct {
	text         string
	finishReason string
}

// assistant は completer の上に推敲・タイトル提案・要約・ファクトチェック・学術推敲を実装する。各プロバイダーはこれを埋め込む。
type assistant struct {
	name         string
	c            completer
	writingStyle string
}

func (a *assistant) Name() string { return a.name }

// ask は ctx に載った RAG コンテキスト（WithRAGContext）を system プロンプトに注入してから completer を呼ぶ。
// AI API を呼ぶ操作はすべてここを通す。
func (a *assistant) ask(ctx context.Context, system, user string) (string, error) {
	out, err := a.c.complete(ctx, BuildSystemPrompt(system, RAGContextFrom(ctx)), user)
	if err != nil {
		return "", err
	}
	if isTruncatedFinishReason(out.finishReason) {
		return "", ErrTruncated
	}
	if strings.TrimSpace(out.text) == "" {
		return "", ErrEmptyResponse
	}
	return out.text, nil
}

func isTruncatedFinishReason(reason string) bool {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS", "LENGTH":
		return true
	default:
		return false
	}
}

// withWritingStyle は文章作成・推敲・翻訳など対象タスクにのみスタイルガイド指示を注入する。
// タイトル提案やファクトチェックなどの構造化応答タスクには適用しない。
func (a *assistant) withWritingStyle(ctx context.Context, system string) string {
	ws := WritingStyleFrom(ctx)
	if ws == "" {
		ws = a.writingStyle
	}
	if instr := WritingStyleInstruction(ws); instr != "" {
		return system + "\n\n" + instr
	}
	return system
}

func (a *assistant) Proofread(ctx context.Context, text string) (*ProofreadResult, error) {
	return a.rewrite(ctx, proofreadPrompt, text)
}

func (a *assistant) SuggestTitles(ctx context.Context, text string) ([]string, error) {
	out, err := a.ask(ctx, titlesPrompt, text)
	if err != nil {
		return nil, err
	}
	titles := parseTitles(out)
	if len(titles) == 0 {
		return nil, ErrEmptyResponse
	}
	return titles, nil
}

func (a *assistant) Summarize(ctx context.Context, text string) (string, error) {
	out, err := a.ask(ctx, summaryPrompt, text)
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(stripCodeFence(out))
	if summary == "" {
		return "", ErrEmptyResponse
	}
	return summary, nil
}

// codeFenceRe は応答全体を囲む ``` のコードブロックに一致する。
var codeFenceRe = regexp.MustCompile("^\\s*```[A-Za-z]*\\n([\\s\\S]*?)\\n?```\\s*$")

// stripCodeFence は応答全体が ``` で囲まれていれば中身だけを返す。
func stripCodeFence(s string) string {
	if m := codeFenceRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// cleanProofread は推敲結果からモデルが付けがちなコードブロックの囲みを外し、
// 元テキストの先頭・末尾の空白（末尾の改行など）を引き継いで、余計な差分が出ないようにする。
func cleanProofread(original, improved string) string {
	if !codeFenceRe.MatchString(original) {
		improved = stripCodeFence(improved)
	}
	body := strings.TrimSpace(improved)
	if body == "" {
		return ""
	}
	lead := original[:len(original)-len(strings.TrimLeftFunc(original, unicode.IsSpace))]
	trail := original[len(strings.TrimRightFunc(original, unicode.IsSpace)):]
	return lead + body + trail
}

// titleMarkerRe はタイトル候補の行頭に付きがちな番号・箇条書き記号に一致する。
// 「2025年の〜」のように数字から始まるタイトルを誤って削らないよう、数字の場合は区切り文字または空白を必須とする。
var titleMarkerRe = regexp.MustCompile(`^(?:[-*・•●◆■]+|(?:案)?[0-9０-９]+[.．、):）:：]|[0-9０-９]+\s+|(?:案)[0-9０-９]+|[①-⑳])\s*`)

// parseTitles は改行区切りの応答をタイトルのスライスにする。番号・記号・囲みの括弧を除き、重複を省いて最大 TitleCount 件を返す。
func parseTitles(s string) []string {
	var titles []string
	seen := map[string]bool{}
	for _, line := range strings.Split(stripCodeFence(s), "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimSpace(titleMarkerRe.ReplaceAllString(t, ""))
		t = strings.TrimSpace(strings.Trim(t, "*#"))
		t = trimPair(t, "「", "」")
		t = trimPair(t, "『", "』")
		t = trimPair(t, `"`, `"`)
		t = trimPair(t, "“", "”")
		if t == "" || seen[t] || strings.HasSuffix(t, "：") || strings.HasSuffix(t, ":") {
			continue
		}
		seen[t] = true
		titles = append(titles, t)
		if len(titles) == TitleCount {
			break
		}
	}
	return titles
}

// trimPair は s 全体が open と close で囲まれていれば外す。
func trimPair(s, open, close string) string {
	if len(s) > len(open)+len(close) && strings.HasPrefix(s, open) && strings.HasSuffix(s, close) {
		inner := s[len(open) : len(s)-len(close)]
		// 「A」と「B」のように途中にも括弧がある行は、外すと対応が崩れるのでそのまま返す。
		if !strings.Contains(inner, open) && !strings.Contains(inner, close) {
			return strings.TrimSpace(inner)
		}
	}
	return s
}
