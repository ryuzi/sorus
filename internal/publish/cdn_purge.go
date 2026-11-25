package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var cloudflarePurgeEndpoint = "https://api.cloudflare.com/client/v4/zones/"

const maxCloudflarePurgeResponseBytes = 200 * 4

func limitCloudflarePurgeResponse(data []byte) []byte {
	runes := []rune(string(data))
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return []byte(string(runes))
}

// PurgeCloudflare clears the requested URLs from a Cloudflare zone. An empty
// files slice clears the entire zone. The request is bounded to ten seconds.
func PurgeCloudflare(ctx context.Context, client *http.Client, apiToken, zoneID string, files []string) error {
	if strings.TrimSpace(apiToken) == "" || strings.TrimSpace(zoneID) == "" {
		return errors.New("cloudflare cache purge: API token and zone ID are required")
	}
	if strings.ContainsAny(apiToken, "\r\n") {
		return errors.New("cloudflare cache purge: API token contains a line break")
	}
	for _, r := range zoneID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return errors.New("cloudflare cache purge: invalid zone ID")
		}
	}
	var payload any
	if len(files) == 0 {
		payload = struct {
			PurgeEverything bool `json:"purge_everything"`
		}{true}
	} else {
		payload = struct {
			Files []string `json:"files"`
		}{files}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("cloudflare cache purge: encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := cloudflarePurgeEndpoint + zoneID + "/purge_cache"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cloudflare cache purge: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare cache purge: request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudflarePurgeResponseBytes))
	if err != nil {
		return fmt.Errorf("cloudflare cache purge: read response: %w", err)
	}
	data = limitCloudflarePurgeResponse(data)
	var result struct {
		Success *bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Messages []struct {
			Message string `json:"message"`
		} `json:"messages"`
	}
	decodeErr := json.Unmarshal(data, &result)
	apiErrors := make([]string, 0, len(result.Errors))
	for _, e := range result.Errors {
		msg := strings.TrimSpace(e.Message)
		if msg != "" {
			if e.Code != 0 {
				msg = fmt.Sprintf("%d: %s", e.Code, msg)
			}
			apiErrors = append(apiErrors, msg)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil || result.Success == nil || !*result.Success {
		if len(apiErrors) == 0 {
			for _, m := range result.Messages {
				if strings.TrimSpace(m.Message) != "" {
					apiErrors = append(apiErrors, strings.TrimSpace(m.Message))
				}
			}
		}
		if len(apiErrors) == 0 {
			if decodeErr != nil {
				apiErrors = append(apiErrors, "invalid API response")
			} else if len(strings.TrimSpace(string(data))) > 0 {
				apiErrors = append(apiErrors, strings.TrimSpace(string(data)))
			}
		}
		return fmt.Errorf("cloudflare cache purge: HTTP %d: %s", resp.StatusCode, strings.Join(apiErrors, "; "))
	}
	return nil
}
