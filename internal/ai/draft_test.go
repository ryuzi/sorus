package ai

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestDummyGenerateDraft(t *testing.T) {
	d := NewDummy("未設定案内")
	res, err := d.GenerateDraft(context.Background(), "テーマ", "メモ")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
	if res != nil {
		t.Errorf("expected nil result, got %+v", res)
	}
}

func TestGenerateDraft(t *testing.T) {
	t.Run("success with code fence", func(t *testing.T) {
		const reply = "```json\n" +
			`{"title":"春の活動報告","slug":"--Spring-Activity--","summary":"4月の活動のまとめ。","content":"# 活動報告\n\n20名が参加しました。"}` +
			"\n```"
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, openAIReply(reply))
		})
		p := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL})
		res, err := p.GenerateDraft(context.Background(), "春の活動", "- 4月に開催\n- 20名参加")
		if err != nil {
			t.Fatalf("GenerateDraft: %v", err)
		}
		if res.Title != "春の活動報告" {
			t.Errorf("Title = %q", res.Title)
		}
		if res.Slug != "spring-activity" {
			t.Errorf("Slug = %q (expected trimmed lower case)", res.Slug)
		}
		if res.Summary != "4月の活動のまとめ。" {
			t.Errorf("Summary = %q", res.Summary)
		}
		if res.Content != "# 活動報告\n\n20名が参加しました。" {
			t.Errorf("Content = %q", res.Content)
		}
	})

	t.Run("invalid json returns ErrEmptyResponse", func(t *testing.T) {
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, openAIReply("申し訳ありませんが生成できませんでした"))
		})
		p := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL})
		_, err := p.GenerateDraft(context.Background(), "春の活動", "メモ")
		if !errors.Is(err, ErrEmptyResponse) {
			t.Errorf("expected ErrEmptyResponse, got %v", err)
		}
	})

	t.Run("empty title returns ErrEmptyResponse", func(t *testing.T) {
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, openAIReply(`{"title":"","slug":"spring","summary":"要約","content":"本文"}`))
		})
		p := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL})
		_, err := p.GenerateDraft(context.Background(), "春の活動", "メモ")
		if !errors.Is(err, ErrEmptyResponse) {
			t.Errorf("expected ErrEmptyResponse, got %v", err)
		}
	})

	t.Run("empty content returns ErrEmptyResponse", func(t *testing.T) {
		srv, _ := mockAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeJSON(w, http.StatusOK, openAIReply(`{"title":"タイトル","slug":"spring","summary":"要約","content":"   "}`))
		})
		p := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL})
		_, err := p.GenerateDraft(context.Background(), "春の活動", "メモ")
		if !errors.Is(err, ErrEmptyResponse) {
			t.Errorf("expected ErrEmptyResponse, got %v", err)
		}
	})
}
