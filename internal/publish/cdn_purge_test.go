package publish

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withPurgeServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	old := cloudflarePurgeEndpoint
	cloudflarePurgeEndpoint = srv.URL + "/zones/"
	t.Cleanup(func() { cloudflarePurgeEndpoint = old; srv.Close() })
	return srv
}

func TestPurgeCloudflareRequestAndSuccess(t *testing.T) {
	withPurgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/zones/zone-123/purge_cache" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["purge_everything"] != true {
			t.Errorf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	if err := PurgeCloudflare(context.Background(), nil, "secret", "zone-123", nil); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeCloudflareSpecificFilesAndAPIError(t *testing.T) {
	withPurgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		files, ok := body["files"].([]any)
		if !ok || len(files) != 1 || files[0] != "https://example.com/a" {
			t.Errorf("body = %#v", body)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Invalid token"}]}`))
	})
	err := PurgeCloudflare(context.Background(), nil, "secret", "zone-123", []string{"https://example.com/a"})
	if err == nil || !strings.Contains(err.Error(), "10000: Invalid token") {
		t.Fatalf("error = %v", err)
	}
}

func TestPurgeCloudflareMalformedAndZoneErrors(t *testing.T) {
	withPurgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Zone not found"}]}`))
	})
	err := PurgeCloudflare(context.Background(), nil, "secret", "missing", nil)
	if err == nil || !strings.Contains(err.Error(), "Zone not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestPurgeCloudflareLimitsErrorResponseTo200Characters(t *testing.T) {
	largeBody := strings.Repeat("x", 1000)
	withPurgeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(largeBody))
	})
	err := PurgeCloudflare(context.Background(), nil, "secret", "zone-123", nil)
	if err == nil {
		t.Fatal("expected an API error")
	}
	message := strings.TrimPrefix(err.Error(), "cloudflare cache purge: HTTP 502: ")
	if len([]rune(message)) > 200 {
		t.Fatalf("response detail has %d characters, want at most 200", len([]rune(message)))
	}
	if strings.Contains(message, largeBody) {
		t.Fatal("large response body was included in the error")
	}
}

func TestLimitCloudflarePurgeResponseCountsUnicodeCharacters(t *testing.T) {
	got := limitCloudflarePurgeResponse([]byte(strings.Repeat("界", 201)))
	if n := len([]rune(string(got))); n != 200 {
		t.Fatalf("response has %d characters, want 200", n)
	}
}

type timeoutRoundTripper struct{}

func (timeoutRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestPurgeCloudflareTimeout(t *testing.T) {
	old := cloudflarePurgeEndpoint
	cloudflarePurgeEndpoint = "https://example.invalid/zones/"
	t.Cleanup(func() { cloudflarePurgeEndpoint = old })
	client := &http.Client{Transport: timeoutRoundTripper{}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := PurgeCloudflare(ctx, client, "secret", "zone", nil)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error = %v", err)
	}
}
