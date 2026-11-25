package publish

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNetlifyValidate(t *testing.T) {
	valid := NetlifyConfig{SiteID: "site-id", Token: "token"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*NetlifyConfig){
		"site id":       func(c *NetlifyConfig) { c.SiteID = "../other" },
		"blank site id": func(c *NetlifyConfig) { c.SiteID = " " },
		"token":         func(c *NetlifyConfig) { c.Token = " " },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid
			change(&c)
			if c.Validate() == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
}

func TestNetlifyTestConnectionChecksTokenAndSiteAccess(t *testing.T) {
	var got string
	c := NewNetlify(NetlifyConfig{SiteID: "site-123", Token: " secret ", API: "https://netlify.test", HTTPClient: &http.Client{Transport: netlifyTestTransport(func(r *http.Request) (*http.Response, error) {
		got = r.Method + " " + r.URL.Path
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		return netlifyResponse(http.StatusOK, `{"id":"site-123"}`), nil
	})}})
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "GET /sites/site-123" {
		t.Fatalf("request = %q", got)
	}

	c = NewNetlify(NetlifyConfig{SiteID: "site-123", Token: "secret", API: "https://netlify.test", HTTPClient: &http.Client{Transport: netlifyTestTransport(func(r *http.Request) (*http.Response, error) {
		return netlifyResponse(http.StatusForbidden, `{"message":"site access denied"}`), nil
	})}})
	if err := c.TestConnection(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("TestConnection error = %v, want ErrAuth", err)
	}
}

type netlifyRoundTrip func(*http.Request) (*http.Response, error)

func (f netlifyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func netlifyTestTransport(f netlifyRoundTrip) http.RoundTripper              { return f }
func netlifyResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestNetlifyDeployUsesCompleteDigestAndUploadsRequiredDelta(t *testing.T) {
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "home", "assets/app.js": "javascript", "keep space/a.txt": "same"})
	gotDigests := map[string]string{}
	gotUploads := map[string]string{}
	var paths []string
	var progress []Progress
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/sites/site-id/deploys":
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type = %q", r.Header.Get("Content-Type"))
			}
			var body struct {
				Files map[string]string `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			gotDigests = body.Files
			h := sha1.Sum([]byte("home"))
			return netlifyResponse(http.StatusCreated, `{"id":"deploy-42","required":["`+hex.EncodeToString(h[:])+`"],"deploy_ssl_url":"https://deploy--site.netlify.app"}`), nil
		case "PUT /api/v1/deploys/deploy-42/files/index.html":
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Errorf("upload content type = %q", r.Header.Get("Content-Type"))
			}
			b, _ := io.ReadAll(r.Body)
			gotUploads[r.URL.Path] = string(b)
			return netlifyResponse(http.StatusCreated, ""), nil
		case "GET /api/v1/sites/site-id/deploys/deploy-42":
			return netlifyResponse(http.StatusOK, `{"id":"deploy-42","state":"ready","deploy_ssl_url":"https://deploy--site.netlify.app"}`), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return netlifyResponse(http.StatusNotFound, "unexpected"), nil
		}
	})
	prev := Manifest{"index.html": {SHA256: "old", Size: 3}, "removed.html": {SHA256: "old2", Size: 3}, "keep space/a.txt": {SHA256: "unchanged", Size: 4}}
	next := mustManifest(t, dir)
	got, err := NewNetlify(NetlifyConfig{SiteID: "site-id", Token: "tok", API: "https://netlify.test/api/v1", HTTPClient: &http.Client{Transport: transport}}).Deploy(context.Background(), dir, prev, next, func(p Progress) { progress = append(progress, p) })
	if err != nil {
		t.Fatal(err)
	}
	wantDigests := map[string]string{}
	for name, content := range map[string]string{"index.html": "home", "assets/app.js": "javascript", "keep space/a.txt": "same"} {
		h := sha1.Sum([]byte(content))
		wantDigests["/"+name] = hex.EncodeToString(h[:])
	}
	if !reflect.DeepEqual(gotDigests, wantDigests) {
		t.Errorf("digest = %v, want %v", gotDigests, wantDigests)
	}
	if !reflect.DeepEqual(gotUploads, map[string]string{"/api/v1/deploys/deploy-42/files/index.html": "home"}) {
		t.Errorf("uploads = %v", gotUploads)
	}
	if got.Uploaded != 1 || got.Unchanged != 2 || got.Deleted != 1 || got.FirstTime || got.URL != "https://deploy--site.netlify.app" {
		t.Errorf("result = %+v", got)
	}
	if !reflect.DeepEqual(paths, []string{"POST /api/v1/sites/site-id/deploys", "PUT /api/v1/deploys/deploy-42/files/index.html", "GET /api/v1/sites/site-id/deploys/deploy-42"}) {
		t.Errorf("requests = %v", paths)
	}
	if len(progress) != 2 || progress[0].Path != "index.html" || progress[1].Done != 1 || progress[1].Total != 1 {
		t.Errorf("progress = %+v", progress)
	}
}

func TestNetlifyDeployRefusesUnsafeManifestPath(t *testing.T) {
	called := false
	c := NewNetlify(NetlifyConfig{SiteID: "site", Token: "token", API: "https://netlify.test", HTTPClient: &http.Client{Transport: netlifyRoundTrip(func(*http.Request) (*http.Response, error) { called = true; return netlifyResponse(200, `{}`), nil })}})
	_, err := c.Deploy(context.Background(), t.TempDir(), nil, Manifest{"../escape": {SHA256: "x"}}, nil)
	if !errors.Is(err, ErrUnsafePath) || called {
		t.Fatalf("Deploy err=%v called=%v", err, called)
	}
}

func TestNetlifyDeployRejectsUnknownRequiredFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewNetlify(NetlifyConfig{SiteID: "site", Token: "token", API: "https://netlify.test", HTTPClient: &http.Client{Transport: netlifyRoundTrip(func(*http.Request) (*http.Response, error) {
		return netlifyResponse(200, `{"id":"d1","required":["not-in-manifest"]}`), nil
	})}})
	_, err := c.Deploy(context.Background(), dir, nil, Manifest{"index.html": {SHA256: "ignored"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not present in the manifest") {
		t.Fatalf("Deploy error = %v", err)
	}
}

func TestNetlifyDeployMapsAPIAuthFailure(t *testing.T) {
	c := NewNetlify(NetlifyConfig{SiteID: "site", Token: "bad", API: "https://netlify.test", HTTPClient: &http.Client{Transport: netlifyRoundTrip(func(*http.Request) (*http.Response, error) { return netlifyResponse(401, `invalid token`), nil })}})
	if err := c.TestConnection(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("TestConnection error = %v, want ErrAuth", err)
	}
}

func TestNetlifyValidateRejectsControlCharactersInToken(t *testing.T) {
	if err := (NetlifyConfig{SiteID: "site", Token: "tok\r\nX-Injected: 1"}).Validate(); !errors.Is(err, ErrInvalidNetlifyConfig) {
		t.Fatalf("Validate error = %v, want ErrInvalidNetlifyConfig", err)
	}
}

func TestNetlifySiteNotFoundAndNameMismatchAreReported(t *testing.T) {
	for name, resp := range map[string]func() *http.Response{
		"404":           func() *http.Response { return netlifyResponse(http.StatusNotFound, `{"code":404,"message":"Not Found"}`) },
		"name resolved": func() *http.Response { return netlifyResponse(http.StatusOK, `{"id":"0c9f6d1e-uuid"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			c := NewNetlify(NetlifyConfig{SiteID: "my-site", Token: "tok", API: "https://netlify.test", HTTPClient: &http.Client{Transport: netlifyRoundTrip(func(*http.Request) (*http.Response, error) { return resp(), nil })}})
			if err := c.TestConnection(context.Background()); !errors.Is(err, ErrNetlifySiteNotFound) || errors.Is(err, ErrAuth) {
				t.Fatalf("TestConnection error = %v, want ErrNetlifySiteNotFound", err)
			}
		})
	}
}

func TestNetlifyErrorMessageIsBoundedAndOmitsToken(t *testing.T) {
	big := strings.Repeat("<html>error page</html>", 1000)
	c := NewNetlify(NetlifyConfig{SiteID: "site", Token: "nfp-secret-token", API: "https://netlify.test", DisableRetryDelay: true, HTTPClient: &http.Client{Transport: netlifyRoundTrip(func(*http.Request) (*http.Response, error) { return netlifyResponse(http.StatusBadGateway, big), nil })}})
	err := c.TestConnection(context.Background())
	var nl *NetlifyError
	if !errors.As(err, &nl) {
		t.Fatalf("error = %v, want *NetlifyError", err)
	}
	if len(nl.Message) > netlifyMaxErrorMessage+3 {
		t.Errorf("message length = %d, want at most %d", len(nl.Message), netlifyMaxErrorMessage+3)
	}
	if strings.Contains(err.Error(), "nfp-secret-token") {
		t.Fatal("error leaks the token")
	}
}

func TestNetlifyUploadsSendKnownContentLength(t *testing.T) {
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "home", "empty.txt": ""})
	sizes := map[string]int64{}
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && (r.URL.Path == "/sites/site" || r.URL.Path == "/api/v1/sites/site") {
			return netlifyResponse(http.StatusOK, `{"id":"site"}`), nil
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/sites/site/deploys/d1" || r.URL.Path == "/api/v1/sites/site/deploys/d1") {
			return netlifyResponse(http.StatusOK, `{"id":"d1","state":"ready"}`), nil
		}
		if r.Method == http.MethodPost {
			var body struct {
				Files map[string]string `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			return netlifyResponse(http.StatusCreated, `{"id":"d1","required":["`+body.Files["/index.html"]+`","`+body.Files["/empty.txt"]+`"]}`), nil
		}
		if len(r.TransferEncoding) != 0 {
			t.Errorf("%s transfer encoding = %v, want none", r.URL.Path, r.TransferEncoding)
		}
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		if int64(len(b)) != r.ContentLength {
			t.Errorf("%s body %d bytes, content length %d", r.URL.Path, len(b), r.ContentLength)
		}
		sizes[r.URL.Path] = r.ContentLength
		return netlifyResponse(http.StatusOK, ""), nil
	})
	c := NewNetlify(NetlifyConfig{SiteID: "site", Token: "tok", API: "https://netlify.test", HTTPClient: &http.Client{Transport: transport}})
	if _, err := c.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sizes, map[string]int64{"/deploys/d1/files/index.html": 4, "/deploys/d1/files/empty.txt": 0}) {
		t.Fatalf("upload sizes = %v", sizes)
	}
}

func TestNetlifyWaitDeployReady_Success(t *testing.T) {
	polls := 0
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-1/deploys/dep-1" {
			polls++
			if polls == 1 {
				return netlifyResponse(http.StatusOK, `{"id":"dep-1","state":"building"}`), nil
			}
			return netlifyResponse(http.StatusOK, `{"id":"dep-1","state":"ready","ssl_url":"https://site-1.netlify.app"}`), nil
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		return nil, nil
	})
	c := NewNetlify(NetlifyConfig{
		SiteID:            "site-1",
		Token:             "token-1",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		PollInterval:      time.Millisecond,
		PollTimeout:       time.Second,
		DisableRetryDelay: true,
	})
	dep, err := c.waitDeployReady(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("waitDeployReady failed: %v", err)
	}
	if dep.State != "ready" || dep.SSLURL != "https://site-1.netlify.app" {
		t.Fatalf("unexpected deploy: %+v", dep)
	}
	if polls != 2 {
		t.Fatalf("polls = %d, want 2", polls)
	}
}

func TestNetlifyWaitDeployReady_Error(t *testing.T) {
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-1/deploys/dep-fail" {
			return netlifyResponse(http.StatusOK, `{"id":"dep-fail","state":"error","error_message":"build script failed"}`), nil
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		return nil, nil
	})
	c := NewNetlify(NetlifyConfig{
		SiteID:            "site-1",
		Token:             "token-1",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		PollInterval:      time.Millisecond,
		PollTimeout:       time.Second,
		DisableRetryDelay: true,
	})
	_, err := c.waitDeployReady(context.Background(), "dep-fail")
	if err == nil || !strings.Contains(err.Error(), "build script failed") {
		t.Fatalf("expected build script failed error, got: %v", err)
	}
}

func TestNetlifyWaitDeployReady_Timeout(t *testing.T) {
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		return netlifyResponse(http.StatusOK, `{"id":"dep-stuck","state":"enqueued"}`), nil
	})
	c := NewNetlify(NetlifyConfig{
		SiteID:            "site-1",
		Token:             "token-1",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		PollInterval:      time.Millisecond,
		PollTimeout:       10 * time.Millisecond,
		DisableRetryDelay: true,
	})
	_, err := c.waitDeployReady(context.Background(), "dep-stuck")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected context deadline exceeded error, got: %v", err)
	}
}

func TestNetlifyRateLimitAnd5xxRetry(t *testing.T) {
	attempts := 0
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		attempts++
		switch attempts {
		case 1:
			resp := netlifyResponse(http.StatusTooManyRequests, `{"message":"rate limit exceeded"}`)
			resp.Header.Set("Retry-After", "1")
			return resp, nil
		case 2:
			return netlifyResponse(http.StatusServiceUnavailable, `{"message":"temporary unavailable"}`), nil
		default:
			return netlifyResponse(http.StatusOK, `{"id":"site-1","ssl_url":"https://ok.netlify.app"}`), nil
		}
	})
	c := NewNetlify(NetlifyConfig{
		SiteID:            "site-1",
		Token:             "token-1",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		DisableRetryDelay: true,
	})
	url, err := c.SiteURL(context.Background())
	if err != nil {
		t.Fatalf("SiteURL failed after retries: %v", err)
	}
	if url != "https://ok.netlify.app" {
		t.Fatalf("url = %q, want https://ok.netlify.app", url)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestNetlifyExistingSiteOverwriteProtection(t *testing.T) {
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "hello"})
	manifest := mustManifest(t, dir)

	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/existing-site" {
			return netlifyResponse(http.StatusOK, `{"id":"existing-site","deploy_id":"dep-prev-123"}`), nil
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sites/existing-site/deploys" {
			return netlifyResponse(http.StatusCreated, `{"id":"new-dep","required":[]}`), nil
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/existing-site/deploys/new-dep" {
			return netlifyResponse(http.StatusOK, `{"id":"new-dep","state":"ready","ssl_url":"https://existing-site.netlify.app"}`), nil
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		return nil, nil
	})

	// 1. prev == nil && ReplaceLiveSite == false -> ErrLiveSite
	c1 := NewNetlify(NetlifyConfig{
		SiteID:            "existing-site",
		Token:             "token",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		ReplaceLiveSite:   false,
		DisableRetryDelay: true,
	})
	_, err := c1.Deploy(context.Background(), dir, nil, manifest, nil)
	if !errors.Is(err, ErrLiveSite) {
		t.Fatalf("expected ErrLiveSite, got: %v", err)
	}

	// 2. prev == nil && ReplaceLiveSite == true -> 成功
	c2 := NewNetlify(NetlifyConfig{
		SiteID:            "existing-site",
		Token:             "token",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		ReplaceLiveSite:   true,
		DisableRetryDelay: true,
	})
	res, err := c2.Deploy(context.Background(), dir, nil, manifest, nil)
	if err != nil {
		t.Fatalf("expected success with ReplaceLiveSite, got error: %v", err)
	}
	if res.URL != "https://existing-site.netlify.app" {
		t.Fatalf("res.URL = %q", res.URL)
	}
}

func TestNetlifyMasksSensitiveData(t *testing.T) {
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		return netlifyResponse(http.StatusBadRequest, `{"message":"invalid request for site-secret-id with token-secret-xyz"}`), nil
	})
	c := NewNetlify(NetlifyConfig{
		SiteID:            "site-secret-id",
		Token:             "token-secret-xyz",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		DisableRetryDelay: true,
	})
	err := c.TestConnection(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "site-secret-id") {
		t.Fatalf("error leaks SiteID: %s", errMsg)
	}
	if strings.Contains(errMsg, "token-secret-xyz") {
		t.Fatalf("error leaks Token: %s", errMsg)
	}
	if !strings.Contains(errMsg, "[REDACTED_SITE_ID]") {
		t.Fatalf("error should contain [REDACTED_SITE_ID]: %s", errMsg)
	}
	if !strings.Contains(errMsg, "[REDACTED_TOKEN]") {
		t.Fatalf("error should contain [REDACTED_TOKEN]: %s", errMsg)
	}
}

func TestNetlifyRejectsInvalidBaseURL(t *testing.T) {
	for name, rawURL := range map[string]string{
		"localhost":      "http://localhost:8080",
		"127.0.0.1":      "http://127.0.0.1:3000",
		"invalid scheme": "ftp://example.com",
		"malformed":      "https://",
	} {
		t.Run(name, func(t *testing.T) {
			transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
				return netlifyResponse(http.StatusOK, `{"id":"site-1","ssl_url":"`+rawURL+`"}`), nil
			})
			c := NewNetlify(NetlifyConfig{
				SiteID:            "site-1",
				Token:             "token-1",
				API:               "https://netlify.test/api/v1",
				HTTPClient:        &http.Client{Transport: transport},
				DisableRetryDelay: true,
			})
			_, err := c.SiteURL(context.Background())
			if err == nil {
				t.Fatalf("%s: expected error for invalid URL %q, got nil", name, rawURL)
			}
		})
	}
}

func TestNetlifyMasksUrlError(t *testing.T) {
	// 接続不能なトランスポートで *url.Error を発生させる
	c := NewNetlify(NetlifyConfig{
		SiteID:     "super-secret-site-id-999",
		Token:      "super-secret-token-888",
		API:        "http://invalid.domain.that.does.not.exist.test:9999",
		HTTPClient: &http.Client{Transport: netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: errors.New("dial tcp failed")}
		})},
		DisableRetryDelay: true,
	})

	err := c.TestConnection(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "super-secret-site-id-999") {
		t.Fatalf("url.Error leaked site ID: %s", errMsg)
	}
	if strings.Contains(errMsg, "super-secret-token-888") {
		t.Fatalf("url.Error leaked token: %s", errMsg)
	}
	if !strings.Contains(errMsg, "[REDACTED_SITE_ID]") {
		t.Fatalf("expected [REDACTED_SITE_ID] in error message: %s", errMsg)
	}
}

func TestNetlifyRetryBackoffCalculation(t *testing.T) {
	c := NewNetlify(NetlifyConfig{SiteID: "site", Token: "token"})

	// 1. Retry-After: 1秒
	b1 := c.retryBackoff(0, "1")
	if b1 != 1*time.Second {
		t.Fatalf("retryBackoff with Retry-After 1 = %v, want 1s", b1)
	}

	// 2. Retry-After: 120秒 -> 上限60秒にクランプ
	b120 := c.retryBackoff(0, "120")
	if b120 != 60*time.Second {
		t.Fatalf("retryBackoff with Retry-After 120 = %v, want 60s", b120)
	}

	// 3. ヘッダーなし: 指数バックオフ + ジッター (0〜500ms)
	bAttempt0 := c.retryBackoff(0, "")
	if bAttempt0 < 1*time.Second || bAttempt0 > 1500*time.Millisecond {
		t.Fatalf("retryBackoff attempt 0 = %v, want [1s, 1.5s]", bAttempt0)
	}

	bAttempt1 := c.retryBackoff(1, "")
	if bAttempt1 < 2*time.Second || bAttempt1 > 2500*time.Millisecond {
		t.Fatalf("retryBackoff attempt 1 = %v, want [2s, 2.5s]", bAttempt1)
	}
}

func TestNetlifyDeployPostDoesNotRetry5xx(t *testing.T) {
	postCalls := 0
	transport := netlifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-1" {
			return netlifyResponse(http.StatusOK, `{"id":"site-1"}`), nil
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sites/site-1/deploys" {
			postCalls++
			return netlifyResponse(http.StatusInternalServerError, `{"message":"internal server error"}`), nil
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		return nil, nil
	})

	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "home"})
	c := NewNetlify(NetlifyConfig{
		SiteID:            "site-1",
		Token:             "token-1",
		API:               "https://netlify.test/api/v1",
		HTTPClient:        &http.Client{Transport: transport},
		DisableRetryDelay: true,
	})

	_, err := c.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil)
	if err == nil {
		t.Fatal("expected deploy error on 500, got nil")
	}
	if postCalls != 1 {
		t.Fatalf("deploy POST calls = %d, want exactly 1 (no 5xx retry on non-idempotent deploy POST)", postCalls)
	}
}

