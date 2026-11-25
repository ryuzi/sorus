package ai

import (
	"context"
	"encoding/json"
	"strings"
)

const draftPrompt = "あなたは編集者です。記事テーマと対話メモから、事実を補わずに確認可能な内容だけで記事の下書きを作成してください。" +
	"出力は title, slug, content, summary の4項目を持つ JSON オブジェクトだけにしてください。" +
	"content は Markdown、slug は英小文字・数字・ハイフンを使った短い URL 用文字列、summary は簡潔なリード文にしてください。" +
	"メモにない具体的な事実、日時、発言、数値は作らず、不明点は本文で断定しないでください。"

func (a *assistant) GenerateDraft(ctx context.Context, topic, notes string) (*DraftResult, error) {
	user := "記事テーマ:\n" + strings.TrimSpace(topic) + "\n\n対話メモ・箇条書き:\n" + strings.TrimSpace(notes)
	out, err := a.ask(ctx, a.withWritingStyle(ctx, draftPrompt), user)
	if err != nil {
		return nil, err
	}
	var result DraftResult
	if err := json.Unmarshal([]byte(stripCodeFence(strings.TrimSpace(out))), &result); err != nil {
		return nil, ErrEmptyResponse
	}
	result.Title = strings.TrimSpace(result.Title)
	result.Slug = strings.Trim(strings.ToLower(strings.TrimSpace(result.Slug)), "-")
	result.Content = strings.TrimSpace(result.Content)
	result.Summary = strings.TrimSpace(result.Summary)
	if result.Title == "" || result.Content == "" {
		return nil, ErrEmptyResponse
	}
	return &result, nil
}
