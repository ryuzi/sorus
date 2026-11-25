package ai

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// ImageAnalysis は画像を AI に見せて作った代替テキスト・キャプション・タグ。
type ImageAnalysis struct {
	Alt     string   `json:"alt"`
	Caption string   `json:"caption"`
	Tags    []string `json:"tags"`
}

// MultimodalAnalyzer は画像の内容を読み取ってメタデータを作る。画像を扱える Gemini と Dummy が満たす。
// Provider に含めないのは、画像を扱えないプロバイダーや Provider だけを実装した既存の実装を壊さないため。
// 呼び出し側は p.(MultimodalAnalyzer) で取り出す。
type MultimodalAnalyzer interface {
	// AnalyzeImage は imageData（mimeType の画像）から代替テキスト・キャプション・タグを返す。
	// existingTags はサイトで使っているタグ（よく使う順）。当てはまるものがあれば AI はその表記のまま使い、
	// 無い内容にだけ新しいタグを作る（同じ意味のタグが表記ゆれで増えないようにする）。nil なら候補を渡さない。
	AnalyzeImage(ctx context.Context, imageData []byte, mimeType string, existingTags []string) (*ImageAnalysis, error)
}

var (
	_ MultimodalAnalyzer = (*Gemini)(nil)
	_ MultimodalAnalyzer = (*Dummy)(nil)
)

// MaxImageTags は AnalyzeImage が返すタグの上限。
const MaxImageTags = 8

// MaxImageHintTags は AnalyzeImage が AI に渡す既存タグの上限（プロンプトを長くしすぎない）。
// maxImageHintTagRunes より長いタグは候補にしない。
const (
	MaxImageHintTags     = 50
	maxImageHintTagRunes = 50
)

// ErrInvalidImage は AnalyzeImage に渡した画像が空か、画像の MIME タイプでないことを表す。
var ErrInvalidImage = errors.New("ai: invalid image data")

const imageAnalysisPrompt = "あなたはWebサイトのフォトエディターです。渡された1枚の画像を観察し、記事で使うためのメタデータを日本語で作成してください。\n" +
	"出力は alt, caption, tags の3項目を持つ JSON オブジェクトだけにしてください。\n" +
	"- alt: 画像が見えない読者に内容を伝える代替テキスト。写っている人・物・場所・状況を具体的に、100文字以内の1文で書く。「画像」「写真」で始めない。\n" +
	"- caption: 記事の画像の下に添えるキャプション。1〜2文、120文字以内。\n" +
	"- tags: 画像を検索するためのタグ。3〜8個の短い名詞（例: 桜, 建物, 風景）。「#」は付けない。\n" +
	"- 既存のタグの一覧が渡されたときは、画像に当てはまる既存のタグを優先し、一覧の表記のまま使う（同じ意味の言い換え・ひらがな・カタカナ・英語の別表記を新しく作らない）。" +
	"当てはまる既存のタグが無い内容についてだけ、新しいタグを作る。\n" +
	"画像から読み取れないこと（人物の氏名、正確な日付や場所の固有名詞など）は推測で書かないでください。"

// imageAnalysisSchema は Gemini の構造化出力（responseSchema）に渡す ImageAnalysis のスキーマ。
var imageAnalysisSchema = &geminiSchema{
	Type: "OBJECT",
	Properties: map[string]*geminiSchema{
		"alt":     {Type: "STRING", Description: "代替テキスト（100文字以内）"},
		"caption": {Type: "STRING", Description: "キャプション（120文字以内）"},
		"tags":    {Type: "ARRAY", Description: "検索用のタグ（3〜8個）", Items: &geminiSchema{Type: "STRING"}},
	},
	Required: []string{"alt", "caption", "tags"},
}

// AnalyzeImage は画像を inline_data で送り、JSON スキーマで形を指定した応答から代替テキスト・キャプション・タグを返す。
func (g *Gemini) AnalyzeImage(ctx context.Context, imageData []byte, mimeType string, existingTags []string) (*ImageAnalysis, error) {
	req, err := newImageAnalysisRequest(imageData, mimeType, existingTags)
	if err != nil {
		return nil, err
	}
	cand, err := g.generate(ctx, req)
	if err != nil {
		return nil, err
	}
	return parseImageAnalysis(cand.text())
}

func (d *Dummy) AnalyzeImage(context.Context, []byte, string, []string) (*ImageAnalysis, error) {
	return nil, ErrNotConfigured
}

// newImageAnalysisRequest は画像解析の generateContent リクエストを組み立てる。既存のタグ（existingTags）は
// 整えて（imageHintTags）、JSON の配列として指示文の後ろに添える。
func newImageAnalysisRequest(imageData []byte, mimeType string, existingTags []string) (geminiRequest, error) {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if len(imageData) == 0 || !strings.HasPrefix(mimeType, "image/") {
		return geminiRequest{}, ErrInvalidImage
	}
	text := "この画像の代替テキスト・キャプション・タグを作成してください。"
	if hints := imageHintTags(existingTags); len(hints) > 0 {
		list, err := json.Marshal(hints)
		if err != nil {
			return geminiRequest{}, err
		}
		text += "\n既存のタグ（よく使われている順）: " + string(list)
	}
	return geminiRequest{
		SystemInstruction: &geminiContent{Parts: []geminiPart{{Text: imageAnalysisPrompt}}},
		Contents: []geminiContent{{Role: "user", Parts: []geminiPart{
			{InlineData: &geminiInlineData{MimeType: mimeType, Data: imageData}},
			{Text: text},
		}}},
		GenerationConfig: &geminiGenerationConfig{ResponseMimeType: "application/json", ResponseSchema: imageAnalysisSchema},
	}, nil
}

// imageHintTags は AI に渡す既存のタグを、順番を保ったまま 1 行にし、「#」・空・重複・長すぎるものを除いて
// MaxImageHintTags 個までにする。
func imageHintTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		t = singleLine(strings.TrimLeft(strings.TrimSpace(t), "#＃"))
		if t == "" || utf8.RuneCountInString(t) > maxImageHintTagRunes || slices.Contains(out, t) {
			continue
		}
		out = append(out, t)
		if len(out) == MaxImageHintTags {
			break
		}
	}
	return out
}

// parseImageAnalysis は AI の応答（JSON。コードブロックで囲まれていてもよい）を ImageAnalysis にする。
// 前後の空白を除き、タグは「#」・空・重複を除いて MaxImageTags 個までにする。代替テキストが無ければ ErrEmptyResponse。
func parseImageAnalysis(out string) (*ImageAnalysis, error) {
	var res ImageAnalysis
	if err := json.Unmarshal([]byte(stripCodeFence(strings.TrimSpace(out))), &res); err != nil {
		return nil, ErrEmptyResponse
	}
	res.Alt = singleLine(res.Alt)
	res.Caption = strings.TrimSpace(res.Caption)
	tags := make([]string, 0, len(res.Tags))
	for _, t := range res.Tags {
		t = singleLine(strings.TrimLeft(strings.TrimSpace(t), "#＃"))
		if t != "" && !slices.Contains(tags, t) && len(tags) < MaxImageTags {
			tags = append(tags, t)
		}
	}
	res.Tags = tags
	if res.Alt == "" {
		return nil, ErrEmptyResponse
	}
	return &res, nil
}
