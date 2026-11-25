package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testS3Key = "test-secret-key"
	testS3ID  = "test-access-key"
)

func TestS3SigV4AndOperations(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		verifyS3Signature(t, r, "us-east-1")
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(r.URL.Path, ".sorus-connection-test-") {
				if string(body) != "sorus" {
					t.Errorf("probe body = %q", body)
				}
			} else {
				if got := r.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
					t.Errorf("Content-Type = %q", got)
				}
				if string(body) != "<h1>hello</h1>" {
					t.Errorf("body = %q", body)
				}
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	r, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "site-bucket", Region: "us-east-1", AccessKeyID: testS3ID, SecretAccessKey: testS3Key, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(context.Background(), "index.html", strings.NewReader("<h1>hello</h1>"), 14); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(context.Background(), "old/page.html"); err != nil {
		t.Fatal(err)
	}
	want := []string{http.MethodPut, http.MethodDelete, http.MethodPut, http.MethodDelete}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestS3SyncDiff(t *testing.T) {
	var put, deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			put = append(put, strings.TrimPrefix(r.URL.Path, "/site-bucket/"))
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodDelete {
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/site-bucket/"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s request", r.Method)
	}))
	defer server.Close()
	remote, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "site-bucket", Region: "auto", AccessKeyID: testS3ID, SecretAccessKey: testS3Key, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := Manifest{"index.html": {SHA256: strings.Repeat("0", 64), Size: 3}, "removed.html": {SHA256: "old", Size: 1}}
	next, err := BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Sync(context.Background(), remote, dir, prev, next, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Deleted != 1 {
		t.Fatalf("result = %+v", res)
	}
	if fmt.Sprint(put) != "[index.html]" || fmt.Sprint(deleted) != "[removed.html]" {
		t.Fatalf("put=%v delete=%v", put, deleted)
	}
}

func TestS3ConnectionTestHandlesAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "AccessDenied", http.StatusForbidden) }))
	defer server.Close()
	client, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKeyID: testS3ID, SecretAccessKey: testS3Key, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.TestConnection(context.Background()); !errorsIsAuth(err) {
		t.Fatalf("TestConnection error = %v, want ErrAuth", err)
	}
}

func verifyS3Signature(t *testing.T, r *http.Request, region string) {
	t.Helper()
	if got := r.Header.Get("x-amz-content-sha256"); got == "" {
		t.Fatal("missing payload hash")
	}
	date := r.Header.Get("x-amz-date")
	if len(date) != 16 {
		t.Fatalf("x-amz-date = %q", date)
	}
	stamp := date
	day := date[:8]
	query, _ := url.ParseQuery(r.URL.RawQuery)
	canonical := r.Method + "\n" + r.URL.EscapedPath() + "\n" + canonicalS3Query(query) + "\n"
	canonical += "host:" + strings.ToLower(r.Host) + "\n" + "x-amz-content-sha256:" + r.Header.Get("x-amz-content-sha256") + "\n" + "x-amz-date:" + stamp + "\n\n"
	canonical += "host;x-amz-content-sha256;x-amz-date\n" + r.Header.Get("x-amz-content-sha256")
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	kDate := hmacSHA256([]byte("AWS4"+testS3Key), day)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	expected := hex.EncodeToString(hmacSHA256(kSigning, toSign))
	if !strings.Contains(r.Header.Get("Authorization"), "Credential="+testS3ID+"/"+scope) || !strings.HasSuffix(r.Header.Get("Authorization"), "Signature="+expected) {
		t.Fatalf("invalid Signature V4 authorization: %s", r.Header.Get("Authorization"))
	}
	if r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if hex.EncodeToString(sum[:]) != r.Header.Get("x-amz-content-sha256") {
			t.Fatal("payload hash does not match body")
		}
	}
}

func TestS3ValidationAndContentTypes(t *testing.T) {
	if _, err := NewS3(S3Config{Endpoint: "file:///tmp", Bucket: "x", AccessKeyID: "a", SecretAccessKey: "b"}); err == nil {
		t.Fatal("expected invalid endpoint")
	}
	if _, err := NewS3(S3Config{Endpoint: "https://example.com", Bucket: "x", Region: "us east 1", AccessKeyID: "a", SecretAccessKey: "b"}); err == nil {
		t.Fatal("expected invalid region")
	}
	for name, want := range map[string]string{"index.html": "text/html; charset=utf-8", "site.css": "text/css; charset=utf-8", "image.webp": "image/webp", "logo.svg": "image/svg+xml", "data.json": "application/json", "photo.jpg": "image/jpeg", "font.woff2": "font/woff2", "unknown.xyz": "application/octet-stream"} {
		if got := s3ContentType(name); got != want {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
}

func TestS3CountEntriesUsesDelimiterAndCountsPrefixes(t *testing.T) {
	var gotDelimiter string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDelimiter = r.URL.Query().Get("delimiter")
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>index.html</Key></Contents><CommonPrefixes><Prefix>articles/</Prefix></CommonPrefixes></ListBucketResult>`)
	}))
	defer server.Close()
	client, err := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKeyID: testS3ID, SecretAccessKey: testS3Key, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	count, err := client.CountEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotDelimiter != "/" || count != 2 {
		t.Fatalf("delimiter=%q count=%d, want '/' and 2", gotDelimiter, count)
	}
}

func errorsIsAuth(err error) bool { return errors.Is(err, ErrAuth) }
