package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestParseImageAnalysis(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want *ImageAnalysis
		err  error
	}{
		{
			name: "plain JSON",
			out:  `{"alt":"桜並木の下を歩く学生","caption":"入学式の朝の正門前。","tags":["桜","キャンパス","春"]}`,
			want: &ImageAnalysis{Alt: "桜並木の下を歩く学生", Caption: "入学式の朝の正門前。", Tags: []string{"桜", "キャンパス", "春"}},
		},
		{
			name: "code fence, whitespace, hash and duplicate tags",
			out:  "```json\n{\"alt\":\"  図書館の\\n閲覧室  \",\"caption\":\" 静かな午後 \",\"tags\":[\"#図書館\",\" 図書館 \",\"\",\"＃読書\"]}\n```",
			want: &ImageAnalysis{Alt: "図書館の 閲覧室", Caption: "静かな午後", Tags: []string{"図書館", "読書"}},
		},
		{
			name: "too many tags are cut",
			out:  `{"alt":"a","caption":"","tags":["1","2","3","4","5","6","7","8","9","10"]}`,
			want: &ImageAnalysis{Alt: "a", Tags: []string{"1", "2", "3", "4", "5", "6", "7", "8"}},
		},
		{
			name: "missing tags",
			out:  `{"alt":"a","caption":"b"}`,
			want: &ImageAnalysis{Alt: "a", Caption: "b", Tags: []string{}},
		},
		{name: "empty alt", out: `{"alt":"  ","caption":"b","tags":["x"]}`, err: ErrEmptyResponse},
		{name: "not JSON", out: "この画像は桜です。", err: ErrEmptyResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseImageAnalysis(tc.out)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNewImageAnalysisRequest(t *testing.T) {
	data := []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	req, err := newImageAnalysisRequest(data, " IMAGE/WEBP ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.SystemInstruction == nil || !strings.Contains(req.SystemInstruction.Parts[0].Text, "alt, caption, tags") {
		t.Errorf("system instruction = %+v", req.SystemInstruction)
	}
	if len(req.Contents) != 1 || len(req.Contents[0].Parts) != 2 {
		t.Fatalf("contents = %+v", req.Contents)
	}
	inline := req.Contents[0].Parts[0].InlineData
	if inline == nil || inline.MimeType != "image/webp" || string(inline.Data) != string(data) {
		t.Errorf("inline data = %+v", inline)
	}
	if text := req.Contents[0].Parts[1].Text; text == "" || strings.Contains(text, "既存のタグ") {
		t.Errorf("user instruction text = %q, want text without an existing tag list", text)
	}
	gc := req.GenerationConfig
	if gc == nil || gc.ResponseMimeType != "application/json" || gc.ResponseSchema == nil {
		t.Fatalf("generation config = %+v", gc)
	}
	if !reflect.DeepEqual(gc.ResponseSchema.Required, []string{"alt", "caption", "tags"}) ||
		gc.ResponseSchema.Properties["tags"].Items.Type != "STRING" {
		t.Errorf("schema = %+v", gc.ResponseSchema)
	}

	for _, tc := range []struct {
		data []byte
		mime string
	}{{nil, "image/png"}, {data, "application/pdf"}, {data, ""}} {
		if _, err := newImageAnalysisRequest(tc.data, tc.mime, nil); !errors.Is(err, ErrInvalidImage) {
			t.Errorf("newImageAnalysisRequest(%d bytes, %q) err = %v, want ErrInvalidImage", len(tc.data), tc.mime, err)
		}
	}
}

// 既存のタグを渡すと、表記ゆれを防ぐ指示（システムプロンプト）と、整えたタグの一覧（指示文）をリクエストに含める。
func TestNewImageAnalysisRequestExistingTags(t *testing.T) {
	data := []byte{0xff, 0xd8, 0xff, 1}
	tags := []string{" 実験風景 ", "#トップ", "", "実験風景", "改行\nを含む", strings.Repeat("長", maxImageHintTagRunes+1), `引用"符`}
	for i := 0; i < MaxImageHintTags+10; i++ {
		tags = append(tags, fmt.Sprintf("タグ%02d", i))
	}
	req, err := newImageAnalysisRequest(data, "image/jpeg", tags)
	if err != nil {
		t.Fatal(err)
	}
	if sys := req.SystemInstruction.Parts[0].Text; !strings.Contains(sys, "既存のタグを優先") || !strings.Contains(sys, "新しいタグを作る") {
		t.Errorf("system instruction does not ask to reuse existing tags: %q", sys)
	}
	text := req.Contents[0].Parts[1].Text
	const prefix = "既存のタグ（よく使われている順）: "
	at := strings.Index(text, prefix)
	if at < 0 {
		t.Fatalf("user text = %q, want an existing tag list", text)
	}
	var got []string
	if err := json.Unmarshal([]byte(text[at+len(prefix):]), &got); err != nil {
		t.Fatalf("existing tag list is not a JSON array: %v (%q)", err, text)
	}
	if len(got) != MaxImageHintTags {
		t.Errorf("hint tags = %d, want %d", len(got), MaxImageHintTags)
	}
	if want := []string{"実験風景", "トップ", "改行 を含む", `引用"符`, "タグ00"}; !reflect.DeepEqual(got[:len(want)], want) {
		t.Errorf("hint tags start with %q, want %q", got[:len(want)], want)
	}
	if strings.Contains(text, "長長長") {
		t.Error("an overlong tag was passed to the AI")
	}
}

func TestGeminiAnalyzeImage(t *testing.T) {
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, geminiReply(`{"alt":"実験装置を操作する学生","caption":"研究室での測定の様子。","tags":["研究室","実験"]}`))
	})
	p, err := NewProvider(Config{Kind: KindGemini, APIKey: "gem-secret", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	analyzer, ok := p.(MultimodalAnalyzer)
	if !ok {
		t.Fatalf("%T does not implement MultimodalAnalyzer", p)
	}
	img := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	got, err := analyzer.AnalyzeImage(context.Background(), img, "image/png", []string{"実験風景", "トップ"})
	if err != nil {
		t.Fatal(err)
	}
	want := &ImageAnalysis{Alt: "実験装置を操作する学生", Caption: "研究室での測定の様子。", Tags: []string{"研究室", "実験"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(*reqs))
	}
	r := (*reqs)[0]
	if r.Path != "/models/"+DefaultGeminiModel+":generateContent" || r.Header.Get("X-Goog-Api-Key") != "gem-secret" {
		t.Errorf("path = %s, key header = %q", r.Path, r.Header.Get("X-Goog-Api-Key"))
	}
	parts := r.Body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	inline, _ := parts[0].(map[string]any)["inline_data"].(map[string]any)
	if inline["mime_type"] != "image/png" || inline["data"] != base64.StdEncoding.EncodeToString(img) {
		t.Errorf("inline_data = %v", inline)
	}
	if _, hasText := parts[0].(map[string]any)["text"]; hasText {
		t.Error("image part also carries an empty text field")
	}
	// 既存のタグは、よく使う順のまま JSON の配列で指示文に添える。
	if text, _ := parts[1].(map[string]any)["text"].(string); !strings.Contains(text, `既存のタグ（よく使われている順）: ["実験風景","トップ"]`) {
		t.Errorf("user text = %q, want the existing tags", text)
	}
	gc, _ := r.Body["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" || gc["responseSchema"] == nil {
		t.Errorf("generationConfig = %v", gc)
	}
}

func TestGeminiAnalyzeImageErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(w http.ResponseWriter)
		check   func(t *testing.T, err error)
	}{
		{
			name: "rate limited",
			handler: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "quota"}})
			},
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
					t.Errorf("err = %v, want APIError 429", err)
				}
			},
		},
		{
			name:    "not JSON",
			handler: func(w http.ResponseWriter) { writeJSON(w, http.StatusOK, geminiReply("桜の写真です")) },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrEmptyResponse) {
					t.Errorf("err = %v, want ErrEmptyResponse", err)
				}
			},
		},
		{
			name: "blocked",
			handler: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusOK, map[string]any{"promptFeedback": map[string]any{"blockReason": "SAFETY"}})
			},
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "SAFETY") {
					t.Errorf("err = %v, want blocked APIError", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) { tc.handler(w) })
			_, err := NewGemini(Config{APIKey: "k", BaseURL: srv.URL}).AnalyzeImage(context.Background(), []byte("img"), "image/jpeg", nil)
			tc.check(t, err)
		})
	}

	// 画像でない入力は API を呼ばずに ErrInvalidImage。
	srv, reqs := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, http.StatusOK, geminiReply(`{"alt":"a"}`))
	})
	if _, err := NewGemini(Config{APIKey: "k", BaseURL: srv.URL}).AnalyzeImage(context.Background(), nil, "image/png", []string{"桜"}); !errors.Is(err, ErrInvalidImage) {
		t.Errorf("err = %v, want ErrInvalidImage", err)
	}
	if len(*reqs) != 0 {
		t.Errorf("requests = %d for invalid input, want 0", len(*reqs))
	}
}

func TestDummyAnalyzeImageNotConfigured(t *testing.T) {
	var p Provider = NewDummy("")
	analyzer, ok := p.(MultimodalAnalyzer)
	if !ok {
		t.Fatal("Dummy does not implement MultimodalAnalyzer")
	}
	if _, err := analyzer.AnalyzeImage(context.Background(), []byte("img"), "image/png", []string{"桜"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}
