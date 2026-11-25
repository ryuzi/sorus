package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var deepLGlossaryTokenRe = regexp.MustCompile(`(?i)__\s*SORUS_GLOSSARY_(\d+)\s*__`)

const (
	// DefaultDeepLFreeBaseURL は DeepL API Free 版のベース URL。
	DefaultDeepLFreeBaseURL = "https://api-free.deepl.com/v2"
	// DefaultDeepLProBaseURL は DeepL API Pro 版のベース URL。
	DefaultDeepLProBaseURL = "https://api.deepl.com/v2"
)

// DeepL は DeepL REST API（Free版 / Pro版）による翻訳プロバイダー。
type DeepL struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewDeepL は cfg の設定で DeepL プロバイダーを作成する。
// API キーが ":fx" で終わる場合は自動的に Free 版のエンドポイントを使用する。
func NewDeepL(cfg Config) *DeepL {
	client := cfg.HTTPClient
	if client == nil {
		client = defaultHTTPClient(cfg)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		if strings.HasSuffix(cfg.APIKey, ":fx") {
			baseURL = DefaultDeepLFreeBaseURL
		} else {
			baseURL = DefaultDeepLProBaseURL
		}
	}

	return &DeepL{
		client:  client,
		baseURL: baseURL,
		apiKey:  cfg.APIKey,
	}
}

func (d *DeepL) Name() string {
	if strings.HasSuffix(d.apiKey, ":fx") {
		return "DeepL (Free)"
	}
	return "DeepL"
}

// deepLRequest は /v2/translate に送る JSON ペイロード。
type deepLRequest struct {
	Text        []string `json:"text"`
	TargetLang  string   `json:"target_lang"`
	SourceLang  string   `json:"source_lang,omitempty"`
	TagHandling string   `json:"tag_handling,omitempty"`
	Formality   string   `json:"formality,omitempty"`
}

// deepLTranslation は API レスポンスの翻訳項目。
type deepLTranslation struct {
	DetectedSourceLanguage string `json:"detected_source_language"`
	Text                   string `json:"text"`
}

// deepLResponse は /v2/translate のレスポンス JSON。
type deepLResponse struct {
	Translations []deepLTranslation `json:"translations"`
	Message      string             `json:"message,omitempty"`
}

// Translate はテキストを翻訳する。
func (d *DeepL) Translate(ctx context.Context, text string, srcLang, targetLang string, opts TranslateOptions) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", nil
	}

	// 1. 学術構文（数式、画像、コードブロック）の安全退避
	masked, restoreSyntax := MaskAcademicSyntax(text)

	// 2. 用語集（Glossary）の前処理:
	// 用語集の単語をプレースホルダートークンに置換し、翻訳後に指定された訳語へ置換する
	var glossaryTokens []string
	var glossaryMap = make(map[string]string)
	if len(opts.Glossary) > 0 {
		// 長いキー順にソートして部分一致の誤置換を防止
		keys := make([]string, 0, len(opts.Glossary))
		for k := range opts.Glossary {
			if strings.TrimSpace(k) != "" {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			return len(keys[i]) > len(keys[j])
		})

		for i, k := range keys {
			val := opts.Glossary[k]
			token := fmt.Sprintf("__SORUS_GLOSSARY_%d__", i)
			if strings.Contains(masked, k) {
				masked = strings.ReplaceAll(masked, k, token)
				glossaryTokens = append(glossaryTokens, token)
				glossaryMap[token] = val
			}
		}
	}

	// 3. 言語コードの正規化
	target := normalizeDeepLLang(targetLang, true)
	if target == "" {
		// 自動判別: 原文が日本語なら英語 (EN-US)、それ以外なら日本語 (JA)
		if isMostlyJapanese(text) {
			target = "EN-US"
		} else {
			target = "JA"
		}
	}
	source := normalizeDeepLLang(srcLang, false)

	// 4. DeepL API リクエスト作成
	formality := ""
	switch strings.ToLower(strings.TrimSpace(opts.Style)) {
	case StyleProfessional:
		formality = "more"
	case StyleFriendly:
		formality = "less"
	}

	reqBody := deepLRequest{
		Text:        []string{masked},
		TargetLang:  target,
		SourceLang:  source,
		TagHandling: "xml",
		Formality:   formality,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	endpoint := d.baseURL
	if !strings.HasSuffix(endpoint, "/translate") {
		endpoint += "/translate"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr deepLResponse
		_ = json.Unmarshal(respBytes, &apiErr)
		msg := apiErr.Message
		if msg == "" {
			msg = string(respBytes)
		}
		return "", &APIError{
			Provider:   d.Name(),
			StatusCode: resp.StatusCode,
			Message:    msg,
		}
	}

	var res deepLResponse
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return "", err
	}
	if len(res.Translations) == 0 {
		return "", ErrEmptyResponse
	}

	translated := res.Translations[0].Text

	// 5. 用語集（Glossary）の置換（変形耐性: 空白や大文字小文字の揺れを許容）
	translated = deepLGlossaryTokenRe.ReplaceAllStringFunc(translated, func(m string) string {
		match := deepLGlossaryTokenRe.FindStringSubmatch(m)
		if len(match) > 1 {
			idx, err := strconv.Atoi(match[1])
			if err == nil && idx >= 0 && idx < len(glossaryTokens) {
				origToken := fmt.Sprintf("__SORUS_GLOSSARY_%d__", idx)
				if val, ok := glossaryMap[origToken]; ok {
					return val
				}
			}
		}
		return m
	})
	for _, token := range glossaryTokens {
		val := glossaryMap[token]
		translated = strings.ReplaceAll(translated, token, val)
	}

	// 6. 学術構文の復元
	restored, err := restoreSyntax(translated)
	if err != nil {
		return "", err
	}

	// 7. 元テキストの前後の空白を保持
	return cleanProofread(text, restored), nil
}

func (d *DeepL) Proofread(context.Context, string) (*ProofreadResult, error) {
	return nil, ErrNotSupported
}

func (d *DeepL) SuggestTitles(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

func (d *DeepL) Summarize(context.Context, string) (string, error) {
	return "", ErrNotSupported
}

// normalizeDeepLLang は DeepL の言語コード形式に正規化する。
func normalizeDeepLLang(lang string, isTarget bool) string {
	l := strings.ToUpper(strings.TrimSpace(lang))
	switch l {
	case "JA", "JAPANESE", "日本語":
		return "JA"
	case "EN", "ENGLISH", "英語":
		if isTarget {
			return "EN-US"
		}
		return "EN"
	case "EN-US", "EN-GB":
		return l
	case "DE", "GERMAN":
		return "DE"
	case "FR", "FRENCH":
		return "FR"
	case "ZH", "CHINESE":
		return "ZH"
	default:
		return l
	}
}
