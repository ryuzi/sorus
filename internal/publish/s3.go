package publish

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const s3MaxResponseBytes = 4 << 20

// S3Config configures an S3-compatible object storage destination. Endpoint should be the
// service endpoint (for example https://<account>.r2.cloudflarestorage.com or MinIO's URL).
type S3Config struct {
	Endpoint, Bucket, Region, AccessKeyID, SecretAccessKey, SiteURL string
	HTTPClient                                                      *http.Client
}

var ErrInvalidS3Config = errors.New("publish: invalid s3 config")

func (c S3Config) Validate() error {
	u, err := url.Parse(strings.TrimSpace(c.Endpoint))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: endpoint", ErrInvalidS3Config)
	}
	if strings.TrimSpace(c.Bucket) == "" || strings.ContainsAny(c.Bucket, "/\\\x00\r\n") {
		return fmt.Errorf("%w: bucket", ErrInvalidS3Config)
	}
	if strings.TrimSpace(c.AccessKeyID) == "" || strings.TrimSpace(c.SecretAccessKey) == "" {
		return fmt.Errorf("%w: credentials", ErrInvalidS3Config)
	}
	if strings.ContainsAny(c.Region, "\x00\r\n") {
		return fmt.Errorf("%w: region", ErrInvalidS3Config)
	}
	if c.Region != "" {
		if len(c.Region) > 64 {
			return fmt.Errorf("%w: region", ErrInvalidS3Config)
		}
		for _, r := range c.Region {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("%w: region", ErrInvalidS3Config)
			}
		}
	}
	return nil
}

// S3 implements Remote with path-style S3 requests and AWS Signature Version 4.
type S3 struct {
	cfg      S3Config
	endpoint *url.URL
	http     *http.Client
}

func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	u, _ := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/"))
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &S3{cfg: cfg, endpoint: u, http: hc}, nil
}

func (s *S3) objectURL(key string) *url.URL {
	u := *s.endpoint
	prefix := strings.TrimRight(u.EscapedPath(), "/")
	if prefix != "" {
		prefix += "/"
	}
	parts := []string{s.cfg.Bucket}
	if key != "" {
		parts = append(parts, strings.Split(key, "/")...)
	}
	encoded := make([]string, len(parts))
	decoded := make([]string, len(parts))
	for i, part := range parts {
		decoded[i] = part
		encoded[i] = awsEscape(part)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.Join(decoded, "/")
	u.RawPath = prefix + strings.Join(encoded, "/")
	return &u
}

func awsEscape(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

func sha256Hex(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func hmacSHA256(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(value))
	return h.Sum(nil)
}

func (s *S3) signedRequest(ctx context.Context, method, key string, body io.Reader, contentLength int64, payloadHash string, contentType string, query url.Values) (*http.Response, error) {
	u := s.objectURL(key)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = contentLength
	}
	now := time.Now().UTC()
	date, stamp := now.Format("20060102"), now.Format("20060102T150405Z")
	req.Header.Set("x-amz-date", stamp)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Host is signed in canonical headers and supplied by net/http from the URL.
	canonicalHeaders := "host:" + strings.ToLower(u.Host) + "\n" + "x-amz-content-sha256:" + payloadHash + "\n" + "x-amz-date:" + stamp + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalQuery := canonicalS3Query(query)
	canonicalURI := u.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonical := method + "\n" + canonicalURI + "\n" + canonicalQuery + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash
	scope := date + "/" + s.cfg.Region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	kDate := hmacSHA256([]byte("AWS4"+s.cfg.SecretAccessKey), date)
	kRegion := hmacSHA256(kDate, s.cfg.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.cfg.AccessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	return s.http.Do(req)
}

func canonicalS3Query(q url.Values) string {
	var parts []string
	for k, values := range q {
		for _, v := range values {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func (s *S3) checkResponse(resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, s3MaxResponseBytes))
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, s3MaxResponseBytes))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: s3 status %d: %s", ErrAuth, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return fmt.Errorf("publish: s3 status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}

func (s *S3) TestConnection(ctx context.Context) error {
	probeID := make([]byte, 16)
	if _, err := rand.Read(probeID); err != nil {
		return fmt.Errorf("publish: s3 connect probe: %w", err)
	}
	key := ".sorus-connection-test-" + hex.EncodeToString(probeID)
	payload := []byte("sorus")
	resp, err := s.signedRequest(ctx, http.MethodPut, key, bytes.NewReader(payload), int64(len(payload)), sha256Hex(payload), "application/octet-stream", nil)
	if err != nil {
		return fmt.Errorf("publish: s3 connect write probe: %w", err)
	}
	if err := s.checkResponse(resp); err != nil {
		return fmt.Errorf("publish: s3 connect write probe: %w", err)
	}
	resp, err = s.signedRequest(ctx, http.MethodDelete, key, nil, 0, sha256Hex(nil), "", nil)
	if err != nil {
		return fmt.Errorf("publish: s3 connect delete probe: %w", err)
	}
	if err := s.checkResponse(resp); err != nil {
		return fmt.Errorf("publish: s3 connect delete probe: %w", err)
	}
	return nil
}

func (s *S3) Put(ctx context.Context, rel string, body io.Reader, size int64) error {
	clean, err := CleanRel(rel)
	if err != nil {
		return err
	}
	var payload io.Reader
	var n int64
	var payloadHash string
	if seeker, ok := body.(io.ReadSeeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}
		h := sha256.New()
		n, err = io.Copy(h, seeker)
		if err != nil {
			_, _ = seeker.Seek(0, io.SeekStart)
			return err
		}
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}
		payload, payloadHash = seeker, hex.EncodeToString(h.Sum(nil))
	} else {
		tmp, err := os.CreateTemp("", "sorus-s3-payload-*")
		if err != nil {
			return err
		}
		defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
		h := sha256.New()
		n, err = io.Copy(io.MultiWriter(tmp, h), body)
		if err != nil {
			return err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		payload, payloadHash = tmp, hex.EncodeToString(h.Sum(nil))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ct := s3ContentType(clean)
	resp, err := s.signedRequest(ctx, http.MethodPut, clean, payload, n, payloadHash, ct, nil)
	if err != nil {
		return fmt.Errorf("publish: s3 put %s: %w", clean, err)
	}
	return s.checkResponse(resp)
}

func s3ContentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ct := s3MIMETypes[ext]; ct != "" {
		return ct
	}
	return "application/octet-stream"
}

var s3MIMETypes = map[string]string{
	".avif": "image/avif", ".css": "text/css; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".gif": "image/gif", ".htm": "text/html; charset=utf-8", ".html": "text/html; charset=utf-8",
	".ico": "image/x-icon", ".jpeg": "image/jpeg", ".jpg": "image/jpeg", ".js": "text/javascript; charset=utf-8",
	".eot": "application/vnd.ms-fontobject", ".json": "application/json", ".map": "application/json", ".mjs": "text/javascript; charset=utf-8",
	".mp3": "audio/mpeg", ".mp4": "video/mp4", ".otf": "font/otf", ".pdf": "application/pdf", ".png": "image/png",
	".svg": "image/svg+xml", ".ttf": "font/ttf", ".txt": "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json", ".webp": "image/webp", ".woff": "font/woff", ".woff2": "font/woff2",
	".xml": "application/xml", ".zip": "application/zip",
}

func (s *S3) Delete(ctx context.Context, rel string) error {
	clean, err := CleanRel(rel)
	if err != nil {
		return err
	}
	resp, err := s.signedRequest(ctx, http.MethodDelete, clean, nil, 0, sha256Hex(nil), "", nil)
	if err != nil {
		return fmt.Errorf("publish: s3 delete %s: %w", clean, err)
	}
	return s.checkResponse(resp)
}

func (s *S3) RemoveDir(context.Context, string) error { return nil }
func (s *S3) Notes() []string                         { return nil }
func (s *S3) Close() error                            { return nil }

type s3ListResult struct {
	Contents              []struct{} `xml:"Contents"`
	CommonPrefixes        []struct{} `xml:"CommonPrefixes"`
	IsTruncated           bool       `xml:"IsTruncated"`
	NextContinuationToken string     `xml:"NextContinuationToken"`
}

func (s *S3) CountEntries(ctx context.Context) (int, error) {
	count := 0
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}, "delimiter": {"/"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.signedRequest(ctx, http.MethodGet, "", nil, 0, sha256Hex(nil), "", q)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, s.checkResponse(resp)
		}
		var result s3ListResult
		dec := xml.NewDecoder(io.LimitReader(resp.Body, s3MaxResponseBytes))
		err = dec.Decode(&result)
		_ = resp.Body.Close()
		if err != nil {
			return 0, err
		}
		count += len(result.Contents) + len(result.CommonPrefixes)
		if !result.IsTruncated {
			return count, nil
		}
		if result.NextContinuationToken == "" {
			return 0, errors.New("publish: s3 truncated listing has no continuation token")
		}
		token = result.NextContinuationToken
	}
}

// Ensure interface conformance.
var _ Remote = (*S3)(nil)
