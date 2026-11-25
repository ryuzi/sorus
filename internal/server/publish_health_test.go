package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPublishHealthCheck(t *testing.T) {
	tests := []struct {
		name, contentType  string
		status, sitemap    int
		wantState, wantKey string
	}{
		{name: "healthy", contentType: "text/html; charset=utf-8", status: http.StatusOK, sitemap: http.StatusOK, wantState: "ok", wantKey: "publish_health_ok"},
		{name: "forbidden", contentType: "text/html", status: http.StatusForbidden, sitemap: http.StatusOK, wantState: "error", wantKey: "publish_health_forbidden"},
		{name: "not found", contentType: "text/html", status: http.StatusNotFound, sitemap: http.StatusOK, wantState: "error", wantKey: "publish_health_not_found"},
		{name: "not html", contentType: "application/json", status: http.StatusOK, sitemap: http.StatusOK, wantState: "error", wantKey: "publish_health_not_html"},
		{name: "missing sitemap", contentType: "text/html", status: http.StatusOK, sitemap: http.StatusNotFound, wantState: "warning", wantKey: "publish_health_sitemap_warning"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, contentType := tt.status, tt.contentType
				if strings.HasSuffix(r.URL.Path, "/sitemap.xml") {
					status, contentType = tt.sitemap, "application/xml"
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader("<html>ok</html>")), Request: r}, nil
			})}
			result := runPublishHealthCheck(context.Background(), "https://example.com", client)
			if result.State != tt.wantState || result.MessageKey != tt.wantKey {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestRunPublishHealthCheckReportsDNSFailure(t *testing.T) {
	got := runPublishHealthCheck(context.Background(), "https://sorus-review.invalid", nil)
	if got.State != "error" || got.MessageKey != "publish_health_dns_error" {
		t.Fatalf("DNS failure result = %+v", got)
	}
}

func TestHealthCheckRedirectAllowsAnotherHost(t *testing.T) {
	first, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	redirect, _ := http.NewRequest(http.MethodGet, "https://www.example.com", nil)
	if err := healthCheckRedirect(redirect, []*http.Request{first}); err != nil {
		t.Fatalf("cross-host redirect was rejected: %v", err)
	}
}

func TestPublishHealthCheckHandlerPersistsResult(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {publishKindFolder}, "folder_path": {filepath.Join(t.TempDir(), "site")}, "folder_site_url": {testBaseURL}}
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	if err := env.server.saveBaseURL("http://127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	res := c.postJSONAccept("/admin/api/publish/health-check")
	expectStatus(t, res, http.StatusOK)
	got := env.server.loadPublishHealth()
	if got == nil || got.State != "error" || got.MessageKey != "publish_health_private_target" || got.CheckedAt.IsZero() {
		t.Fatalf("saved health status = %+v", got)
	}
	page := c.get("/admin/publish").Body
	expectContains(t, page, `id="publish-health"`, "公開状態を確認", "公開サイトのURLは、インターネットから見られるアドレスを設定してください。")
}

func TestRunPublishHealthCheckRejectsInvalidURL(t *testing.T) {
	for _, raw := range []string{"", "file:///tmp/site", "http://user:pass@example.com", "http://example.com/?x=1"} {
		if got := runPublishHealthCheck(context.Background(), raw, nil); got.State != "error" || got.MessageKey != "publish_health_unconfigured" {
			t.Errorf("runPublishHealthCheck(%q) = %+v", raw, got)
		}
	}
}

func TestRunPublishHealthCheckTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	got := runPublishHealthCheck(ctx, "https://example.com", client)
	if got.State != "error" || got.MessageKey != "publish_health_timeout" {
		t.Fatalf("result = %+v", got)
	}
}

func TestRunPublishHealthCheckTLS(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/html"}},
			Body: io.NopCloser(strings.NewReader("ok")), TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{NotAfter: time.Now().Add(10 * 24 * time.Hour)}}}, Request: r}, nil
	})}
	got := runPublishHealthCheck(context.Background(), "https://example.com", client)
	if got.State != "warning" || got.MessageKey != "publish_health_tls_expiring" || got.TLSExpiresAt.IsZero() {
		t.Fatalf("expiring certificate result = %+v", got)
	}
	badTLS := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("x509: certificate signed by unknown authority")
	})}
	got = runPublishHealthCheck(context.Background(), "https://example.com", badTLS)
	if got.State != "error" || got.MessageKey != "publish_health_tls_error" {
		t.Fatalf("invalid certificate result = %+v", got)
	}
}

func TestRunPublishHealthCheckKeepsTLSWarningWithMissingSitemap(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if r.URL.Path == "/sitemap.xml" {
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/html"}},
			Body: io.NopCloser(strings.NewReader("ok")), TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{NotAfter: time.Now().Add(10 * 24 * time.Hour)}}}, Request: r}, nil
	})}
	got := runPublishHealthCheck(context.Background(), "https://example.com", client)
	if got.State != "warning" || got.MessageKey != "publish_health_tls_expiring" || len(got.AdditionalMessageKeys) != 1 || got.AdditionalMessageKeys[0] != "publish_health_sitemap_warning" {
		t.Fatalf("combined TLS and sitemap result = %+v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
