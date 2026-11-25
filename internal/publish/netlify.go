package publish

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Netlify's API supports atomic deploys by first submitting a complete digest,
// then uploading only the files it does not already have.
const (
	DefaultNetlifyAPI       = "https://api.netlify.com/api/v1"
	netlifyMaxResponseBytes = 8 << 20
	// netlifyMaxErrorMessage caps the API body kept in errors shown under the
	// publish screen's details, so an HTML error page does not flood the UI.
	netlifyMaxErrorMessage = 2048
	netlifyTimeout         = 5 * time.Minute
	defaultPollInterval    = 1 * time.Second
	defaultPollTimeout     = 3 * time.Minute
	maxBackoff             = 16 * time.Second
	maxRetries             = 3
)

var (
	ErrInvalidNetlifyConfig = errors.New("publish: invalid netlify config")
	// ErrNetlifySiteNotFound means the token cannot see a site with the configured ID
	// (a wrong Site ID, or a site name pasted where the ID belongs).
	ErrNetlifySiteNotFound = errors.New("publish: netlify site not found")
)

// NetlifyConfig identifies a site and API token. API and HTTPClient are
// injectable for tests and Netlify-compatible endpoints.
type NetlifyConfig struct {
	SiteID            string
	Token             string
	API               string
	HTTPClient        *http.Client
	ReplaceLiveSite   bool
	PollInterval      time.Duration
	PollTimeout       time.Duration
	DisableRetryDelay bool
}

func (c NetlifyConfig) Validate() error {
	if strings.TrimSpace(c.SiteID) == "" || strings.ContainsAny(c.SiteID, "/\\\x00\r\n") {
		return fmt.Errorf("%w: site id", ErrInvalidNetlifyConfig)
	}
	if t := strings.TrimSpace(c.Token); t == "" || strings.ContainsFunc(t, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%w: token", ErrInvalidNetlifyConfig)
	}
	return nil
}

type Netlify struct {
	cfg     NetlifyConfig
	http    *http.Client
	sleepFn func(ctx context.Context, d time.Duration) error
}

func NewNetlify(cfg NetlifyConfig) *Netlify {
	if cfg.API == "" {
		cfg.API = DefaultNetlifyAPI
	}
	cfg.API = strings.TrimRight(cfg.API, "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	hc := cfg.HTTPClient
	if hc == nil {
		tr := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 1 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		}
		hc = &http.Client{
			Timeout:       0, // Per-request context manages dynamic timeout
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Netlify{cfg: cfg, http: hc}
}

type NetlifyError struct {
	Status  int
	Message string
}

func (e *NetlifyError) Error() string {
	return fmt.Sprintf("publish: netlify api: status %d: %s", e.Status, e.Message)
}
func (e *NetlifyError) Is(target error) bool {
	switch target {
	case ErrAuth:
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	case ErrNetlifySiteNotFound:
		return e.Status == http.StatusNotFound
	}
	return false
}

type maskedError struct {
	err  error
	mask func(string) string
}

func (e *maskedError) Error() string {
	if e.err == nil {
		return ""
	}
	if e.mask != nil {
		return e.mask(e.err.Error())
	}
	return e.err.Error()
}

func (e *maskedError) Unwrap() error {
	return e.err
}

func (n *Netlify) wrapError(endpoint string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("publish: netlify api %s: %w", n.mask(endpoint), &maskedError{err: err, mask: n.mask})
}

func (n *Netlify) mask(s string) string {
	if t := strings.TrimSpace(n.cfg.Token); t != "" {
		s = strings.ReplaceAll(s, t, "[REDACTED_TOKEN]")
	}
	if id := strings.TrimSpace(n.cfg.SiteID); id != "" {
		s = strings.ReplaceAll(s, id, "[REDACTED_SITE_ID]")
	}
	return s
}

func (n *Netlify) sleep(ctx context.Context, d time.Duration) error {
	if n.cfg.DisableRetryDelay || d <= 0 {
		return ctx.Err()
	}
	if n.sleepFn != nil {
		return n.sleepFn(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (n *Netlify) retryBackoff(attempt int, retryAfterHeader string) time.Duration {
	if n.cfg.DisableRetryDelay {
		return 0
	}
	if s := strings.TrimSpace(retryAfterHeader); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			if secs > 60 {
				secs = 60
			}
			return time.Duration(secs) * time.Second
		}
	}
	base := time.Second * time.Duration(1<<attempt)
	if base > maxBackoff {
		base = maxBackoff
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	jitter := time.Duration(binary.BigEndian.Uint16(b[:])%500) * time.Millisecond
	return base + jitter
}

type netlifySiteInfo struct {
	ID              string `json:"id"`
	URL             string `json:"url"`
	SSLURL          string `json:"ssl_url"`
	DeployID        string `json:"deploy_id"`
	PublishedDeploy *struct {
		ID string `json:"id"`
	} `json:"published_deploy"`
}

func (n *Netlify) getSite(ctx context.Context) (*netlifySiteInfo, error) {
	var site netlifySiteInfo
	if err := n.call(ctx, http.MethodGet, "/sites/"+url.PathEscape(n.cfg.SiteID), "", nil, &site, true); err != nil {
		return nil, err
	}
	return &site, nil
}

// TestConnection verifies that the token can read the configured site.
func (n *Netlify) TestConnection(ctx context.Context) error {
	_, err := n.SiteURL(ctx)
	return err
}

// SiteURL reads the site's canonical public URL. Netlify's site endpoint is
// also the authoritative check that the token can access the configured site.
func (n *Netlify) SiteURL(ctx context.Context) (string, error) {
	if err := n.cfg.Validate(); err != nil {
		return "", err
	}
	site, err := n.getSite(ctx)
	if err != nil {
		return "", fmt.Errorf("publish: netlify connection test: %w", err)
	}
	if site.ID == "" || !strings.EqualFold(site.ID, n.cfg.SiteID) {
		// Netlify also resolves a site name or domain here; sorus stores and deploys by ID only.
		return "", fmt.Errorf("%w: response id does not match the configured site id", ErrNetlifySiteNotFound)
	}
	target := site.SSLURL
	if target == "" {
		target = site.URL
	}
	target = strings.TrimRight(strings.TrimSpace(target), "/")
	if target != "" {
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", fmt.Errorf("publish: netlify invalid site url %q", target)
		}
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || host == "127.0.0.1" || strings.HasPrefix(host, "127.") || host == "::1" {
			return "", fmt.Errorf("publish: netlify invalid site host %q", host)
		}
	}
	return target, nil
}

type netlifyDeploy struct {
	ID           string   `json:"id"`
	State        string   `json:"state"`
	Required     []string `json:"required"`
	URL          string   `json:"url"`
	SSLURL       string   `json:"ssl_url"`
	DeployURL    string   `json:"deploy_url"`
	DeploySSLURL string   `json:"deploy_ssl_url"`
	ErrorMessage string   `json:"error_message"`
}

func (n *Netlify) waitDeployReady(ctx context.Context, deployID string) (*netlifyDeploy, error) {
	interval := n.cfg.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	timeout := n.cfg.PollTimeout
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		var dep netlifyDeploy
		endpoint := "/sites/" + url.PathEscape(n.cfg.SiteID) + "/deploys/" + url.PathEscape(deployID)
		if err := n.call(waitCtx, http.MethodGet, endpoint, "", nil, &dep, true); err != nil {
			return nil, err
		}
		switch dep.State {
		case "ready":
			return &dep, nil
		case "error":
			msg := dep.ErrorMessage
			if msg == "" {
				msg = "deploy failed"
			}
			return nil, fmt.Errorf("publish: netlify deploy failed: %s", n.mask(msg))
		}

		if err := n.sleep(waitCtx, interval); err != nil {
			return nil, fmt.Errorf("publish: wait netlify deploy ready: %w", err)
		}
		if interval < 2*time.Second {
			interval = 2 * time.Second
		}
	}
}

// Deploy creates a new atomic Netlify deploy from next. Netlify receives the
// complete file digest, so omitted paths from prev are removed in this deploy.
func (n *Netlify) Deploy(ctx context.Context, dir string, prev, next Manifest, progress func(Progress)) (Result, error) {
	res := Result{FirstTime: prev == nil}
	if err := n.cfg.Validate(); err != nil {
		return res, err
	}

	names := make([]string, 0, len(next))
	files := make(map[string]string, len(next))
	for name := range next {
		clean, err := CleanRel(name)
		if err != nil {
			return res, err
		}
		names = append(names, clean)
	}
	slices.Sort(names)
	for _, name := range names {
		digest, err := netlifyFileDigest(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return res, fmt.Errorf("publish: read %s: %w", name, err)
		}
		files["/"+name] = digest
	}

	// [H-3] Existing site overwrite protection
	if prev == nil && !n.cfg.ReplaceLiveSite {
		site, err := n.getSite(ctx)
		if err != nil {
			return res, err
		}
		if site.DeployID != "" || site.PublishedDeploy != nil {
			depID := site.DeployID
			if depID == "" && site.PublishedDeploy != nil {
				depID = site.PublishedDeploy.ID
			}
			return res, fmt.Errorf("%w: netlify site already has published deploy %s", ErrLiveSite, depID)
		}
	}
	var dep netlifyDeploy
	if err := n.call(ctx, http.MethodPost, "/sites/"+url.PathEscape(n.cfg.SiteID)+"/deploys", "application/json", map[string]any{"files": files}, &dep, false); err != nil {
		return res, err
	}
	if dep.ID == "" {
		return res, errors.New("publish: netlify api: deploy response has no id")
	}
	need := map[string]bool{}
	for _, digest := range dep.Required {
		need[strings.ToLower(digest)] = true
	}
	total := len(dep.Required)
	done := 0
	for _, name := range names {
		digest := files["/"+name]
		if !need[digest] {
			continue
		}
		if progress != nil {
			progress(Progress{Done: done, Total: total, Path: name})
		}
		filePath := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Stat(filePath)
		if err != nil {
			return res, fmt.Errorf("publish: stat %s: %w", name, err)
		}

		// [M-1] Dynamic timeout based on file size
		uploadTimeout := 30*time.Second + time.Duration(info.Size()/(100*1024))*time.Second
		if uploadTimeout < 1*time.Minute {
			uploadTimeout = 1 * time.Minute
		}
		if uploadTimeout > 10*time.Minute {
			uploadTimeout = 10 * time.Minute
		}
		fileCtx, fileCancel := context.WithTimeout(ctx, uploadTimeout)

		endpoint := "/deploys/" + url.PathEscape(dep.ID) + "/files/" + netlifyFilePath(name)
		producer := func() (io.Reader, int64, error) {
			f, err := os.Open(filePath)
			if err != nil {
				return nil, 0, fmt.Errorf("publish: open %s: %w", name, err)
			}
			return f, info.Size(), nil
		}
		err = n.doWithRetry(fileCtx, http.MethodPut, endpoint, "application/octet-stream", producer, nil, true)
		fileCancel()
		if err != nil {
			return res, err
		}
		delete(need, digest)
		done++
	}
	if len(need) != 0 {
		return res, fmt.Errorf("publish: netlify deploy requested %d files not present in the manifest", len(need))
	}
	res.Uploaded = done
	res.Unchanged = len(next) - done
	for name := range prev {
		if _, ok := next[name]; !ok {
			res.Deleted++
		}
	}

	// [H-1] Wait for deploy ready
	readyDep, err := n.waitDeployReady(ctx, dep.ID)
	if err != nil {
		return res, err
	}

	res.URL = readyDep.DeploySSLURL
	if res.URL == "" {
		res.URL = readyDep.SSLURL
	}
	if res.URL == "" {
		res.URL = readyDep.DeployURL
	}
	if res.URL == "" {
		res.URL = readyDep.URL
	}
	if progress != nil {
		progress(Progress{Done: done, Total: total})
	}
	return res, nil
}

// netlifyFileDigest streams the file through SHA-1 (the digest Netlify's
// deploy API expects) so large media is not loaded into memory at once.
func netlifyFileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func netlifyFilePath(name string) string {
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (n *Netlify) call(ctx context.Context, method, endpoint, contentType string, in, out any, retry5xx bool) error {
	var data []byte
	if in != nil {
		var err error
		data, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	producer := func() (io.Reader, int64, error) {
		if data == nil {
			return nil, 0, nil
		}
		return bytes.NewReader(data), int64(len(data)), nil
	}
	return n.doWithRetry(ctx, method, endpoint, contentType, producer, out, retry5xx)
}

func (n *Netlify) doWithRetry(ctx context.Context, method, endpoint, contentType string, bodyProducer func() (io.Reader, int64, error), out any, retry5xx bool) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var body io.Reader
		var size int64
		if bodyProducer != nil {
			var err error
			body, size, err = bodyProducer()
			if err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, n.cfg.API+endpoint, body)
		if err != nil {
			if c, ok := body.(io.Closer); ok {
				c.Close()
			}
			return n.wrapError(endpoint, err)
		}
		if body != nil {
			req.ContentLength = size
			if size == 0 {
				// An empty file still needs an explicit zero length rather than chunked encoding.
				req.Body = http.NoBody
				if c, ok := body.(io.Closer); ok {
					c.Close()
				}
			}
		}
		req.Header.Set("Authorization", "Bearer "+n.cfg.Token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := n.http.Do(req)
		if err != nil {
			// [H-2] デプロイ作成 (retry5xx==false) はネットワーク失敗時も多重作成防止のためリトライしない
			if retry5xx && attempt < maxRetries && ctx.Err() == nil {
				d := n.retryBackoff(attempt, "")
				if sleepErr := n.sleep(ctx, d); sleepErr == nil {
					continue
				}
			}
			return n.wrapError(endpoint, err)
		}

		data, readErr := io.ReadAll(io.LimitReader(resp.Body, netlifyMaxResponseBytes))
		resp.Body.Close()
		if readErr != nil {
			return n.wrapError(endpoint, readErr)
		}

		// [H-2] 429 は常にリトライ、5xx は retry5xx が true のときのみリトライ
		if (resp.StatusCode == http.StatusTooManyRequests || (retry5xx && resp.StatusCode >= 500)) && attempt < maxRetries {
			retryAfter := resp.Header.Get("Retry-After")
			d := n.retryBackoff(attempt, retryAfter)
			if sleepErr := n.sleep(ctx, d); sleepErr == nil {
				continue
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg := strings.TrimSpace(string(data))
			if len(msg) > netlifyMaxErrorMessage {
				msg = strings.ToValidUTF8(msg[:netlifyMaxErrorMessage], "") + "..."
			}
			if msg == "" {
				msg = resp.Status
			}
			msg = n.mask(msg)
			return &NetlifyError{Status: resp.StatusCode, Message: msg}
		}

		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return n.wrapError(endpoint, err)
			}
		}
		return nil
	}
}
