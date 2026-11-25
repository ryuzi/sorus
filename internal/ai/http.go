package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxErrorMessage は APIError.Message に残すエラー本文の上限（バイト）。
const maxErrorMessage = 300

// postJSON は body を JSON で POST し、2xx の応答を out にデコードする。
// 2xx 以外は応答本文の error.message（無ければ本文の先頭）を持つ *APIError を返す。
func postJSON(ctx context.Context, client *http.Client, provider, url string, header http.Header, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("ai: encode %s request: %w", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ai: build %s request: %w", provider, err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		// *url.Error は URL を含むが、API キーは URL に載せないため、そのまま返してよい。
		return fmt.Errorf("ai: %s request: %w", provider, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("ai: read %s response: %w", provider, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		status, reason := errorStatusReason(data)
		return &APIError{Provider: provider, StatusCode: res.StatusCode, Message: errorMessage(data), Status: status, Reason: reason}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ai: decode %s response: %w", provider, err)
	}
	return nil
}

// errorStatusReason は Google 形式の {"error": {"status": "...", "details": [{"reason": "..."}]}} から
// status と最初の reason を取り出す。他社の形式なら空を返す。
func errorStatusReason(data []byte) (status, reason string) {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return "", ""
	}
	for _, d := range e.Error.Details {
		if d.Reason != "" {
			reason = d.Reason
			break
		}
	}
	// ログに出す値なので、想定外に長い・制御文字を含む値は切り詰める。
	clean := func(s string) string {
		s = strings.Map(func(r rune) rune {
			if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, s)
		if len(s) > 64 {
			s = s[:64]
		}
		return s
	}
	return clean(e.Error.Status), clean(reason)
}

// errorMessage は各社共通の {"error": {"message": "..."}} 形式からメッセージを取り出す。
func errorMessage(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	if len(msg) > maxErrorMessage {
		msg = strings.ToValidUTF8(msg[:maxErrorMessage], "") + "…"
	}
	return msg
}
