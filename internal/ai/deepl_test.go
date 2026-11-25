package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepLEndpointSelection(t *testing.T) {
	free := NewDeepL(Config{APIKey: "abcd-1234:fx"})
	if free.baseURL != DefaultDeepLFreeBaseURL {
		t.Errorf("got %q, want %q", free.baseURL, DefaultDeepLFreeBaseURL)
	}
	if free.Name() != "DeepL (Free)" {
		t.Errorf("got name %q, want %q", free.Name(), "DeepL (Free)")
	}

	pro := NewDeepL(Config{APIKey: "abcd-1234-pro"})
	if pro.baseURL != DefaultDeepLProBaseURL {
		t.Errorf("got %q, want %q", pro.baseURL, DefaultDeepLProBaseURL)
	}
	if pro.Name() != "DeepL" {
		t.Errorf("got name %q, want %q", pro.Name(), "DeepL")
	}

	custom := NewDeepL(Config{APIKey: "k", BaseURL: "http://localhost:8080/v2"})
	if custom.baseURL != "http://localhost:8080/v2" {
		t.Errorf("got %q, want custom url", custom.baseURL)
	}
}

func TestDeepLTranslate(t *testing.T) {
	var capturedReq deepLRequest
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		capturedReq = deepLRequest{}
		_ = json.Unmarshal(body, &capturedReq)

		// 翻訳モック応答: 入力テキストをエコーしつつ少し英語っぽく返す
		resp := deepLResponse{
			Translations: []deepLTranslation{
				{
					DetectedSourceLanguage: "JA",
					Text:                   "Translated: " + capturedReq.Text[0],
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	d := NewDeepL(Config{
		APIKey:  "test-api-key:fx",
		BaseURL: srv.URL,
	})

	input := "これはテストです。数式 $E=mc^2$ と燃料デブリについて。"
	opts := TranslateOptions{
		Glossary: map[string]string{
			"燃料デブリ": "fuel debris",
		},
		WritingStyle: WritingStyleAcademic,
	}

	out, err := d.Translate(context.Background(), input, "ja", "en", opts)
	if err != nil {
		t.Fatalf("Translate error: %v", err)
	}

	// 認証ヘッダーの確認
	if authHeader != "DeepL-Auth-Key test-api-key:fx" {
		t.Errorf("unexpected auth header: %s", authHeader)
	}

	// リクエストパラメータの確認
	if capturedReq.TagHandling != "xml" {
		t.Errorf("expected tag_handling=xml, got %q", capturedReq.TagHandling)
	}
	if capturedReq.TargetLang != "EN-US" {
		t.Errorf("expected target_lang=EN-US, got %q", capturedReq.TargetLang)
	}

	// 出力の検証: 数式 $E=mc^2$ が保持され、用語集「燃料デブリ」が「fuel debris」になっていること
	if !strings.Contains(out, "$E=mc^2$") {
		t.Errorf("math formula missing in output: %s", out)
	}
	if !strings.Contains(out, "fuel debris") {
		t.Errorf("glossary translation missing in output: %s", out)
	}
	if strings.Contains(out, "燃料デブリ") {
		t.Errorf("original glossary key was not replaced: %s", out)
	}

	// スタイルによる formality のテスト
	for _, tc := range []struct {
		style         string
		wantFormality string
	}{
		{StyleProfessional, "more"},
		{StyleFriendly, "less"},
		{StyleNatural, ""},
	} {
		_, err = d.Translate(context.Background(), "テスト文章", "ja", "en", TranslateOptions{Style: tc.style})
		if err != nil {
			t.Fatalf("Translate with style %q error: %v", tc.style, err)
		}
		if capturedReq.Formality != tc.wantFormality {
			t.Errorf("style %q: got formality %q, want %q", tc.style, capturedReq.Formality, tc.wantFormality)
		}
	}
}

func TestDeepLAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Wrong endpoint or invalid key"}`))
	}))
	defer srv.Close()

	d := NewDeepL(Config{
		APIKey:  "bad-key",
		BaseURL: srv.URL,
	})

	_, err := d.Translate(context.Background(), "hello", "en", "ja", TranslateOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("got status %d, want 403", apiErr.StatusCode)
	}
}

func TestDeepLUnsupportedOps(t *testing.T) {
	d := NewDeepL(Config{APIKey: "k"})
	if _, err := d.Proofread(context.Background(), "text"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("got %v, want ErrNotSupported", err)
	}
	if _, err := d.SuggestTitles(context.Background(), "text"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("got %v, want ErrNotSupported", err)
	}
	if _, err := d.Summarize(context.Background(), "text"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("got %v, want ErrNotSupported", err)
	}
}

func TestDeepLGlossaryTokenRobustness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req deepLRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// DeepL がトークンに空白を入れたり小文字化して返したシミュレーション
		tampered := strings.ReplaceAll(req.Text[0], "__SORUS_GLOSSARY_0__", "__ sorus_glossary_0 __")
		resp := deepLResponse{
			Translations: []deepLTranslation{
				{
					DetectedSourceLanguage: "JA",
					Text:                   "Result: " + tampered,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	d := NewDeepL(Config{
		APIKey:  "test-key",
		BaseURL: srv.URL,
	})

	out, err := d.Translate(context.Background(), "用語集テストです", "ja", "en", TranslateOptions{
		Glossary: map[string]string{
			"用語集": "glossary term",
		},
	})
	if err != nil {
		t.Fatalf("Translate error: %v", err)
	}
	if !strings.Contains(out, "glossary term") {
		t.Errorf("expected glossary term in output, got: %s", out)
	}
	if strings.Contains(out, "__") {
		t.Errorf("unrestored token found in output: %s", out)
	}
}
