package publish

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// GitHubPagesConfig describes a GitHub repository and branch used as the Pages source.
type GitHubPagesConfig struct {
	Owner, Repository, Branch, Directory, Token, API string
	HTTPClient                                       *http.Client
}

var (
	ErrInvalidGitHubPagesConfig = errors.New("publish: invalid github pages config")
	ErrGitHubFileTooLarge       = errors.New("publish: github file exceeds 100 MB")
	githubOwnerPattern          = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepositoryPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

type GitHubErrorCode string

const (
	ErrGitHubAuth        GitHubErrorCode = "auth"
	ErrGitHubRateLimited GitHubErrorCode = "rate_limited"
	ErrGitHubPermission  GitHubErrorCode = "permission"
	ErrGitHubNotFound    GitHubErrorCode = "not_found"
	ErrGitHubConflict    GitHubErrorCode = "conflict"
	ErrGitHubValidation  GitHubErrorCode = "validation"
)

type GitHubError struct {
	Status  int
	Message string
	Code    GitHubErrorCode
}

func (e *GitHubError) Error() string {
	return fmt.Sprintf("publish: github api status %d: %s", e.Status, e.Message)
}

func (c GitHubPagesConfig) Validate() error {
	if !githubOwnerPattern.MatchString(c.Owner) {
		return fmt.Errorf("%w: owner", ErrInvalidGitHubPagesConfig)
	}
	if !githubRepositoryPattern.MatchString(c.Repository) {
		return fmt.Errorf("%w: repository", ErrInvalidGitHubPagesConfig)
	}
	if !validGitBranch(c.Branch) {
		return fmt.Errorf("%w: branch", ErrInvalidGitHubPagesConfig)
	}
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("%w: token", ErrInvalidGitHubPagesConfig)
	}
	if c.Directory != "" {
		if c.Directory != "docs" {
			return fmt.Errorf("%w: directory", ErrInvalidGitHubPagesConfig)
		}
		if _, err := CleanRel(c.Directory); err != nil {
			return fmt.Errorf("%w: directory", ErrInvalidGitHubPagesConfig)
		}
	}
	return nil
}

func validGitBranch(s string) bool {
	if strings.TrimSpace(s) == "" || strings.ContainsAny(s, " ~^:?*[\\\x00\r\n") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func isUTF8Text(b []byte) bool { return utf8.Valid(b) }

// GitHubPages commits a whole site update to a Pages source branch in one commit.
type GitHubPages struct {
	cfg  GitHubPagesConfig
	http *http.Client
}

func NewGitHubPages(c GitHubPagesConfig) *GitHubPages {
	if c.API == "" {
		c.API = "https://api.github.com"
	}
	c.API = strings.TrimRight(c.API, "/")
	h := c.HTTPClient
	if h == nil {
		h = &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &GitHubPages{cfg: c, http: h}
}

type ghRef struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}
type ghCommit struct {
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
}
type ghBlob struct {
	SHA string `json:"sha"`
}
type ghTreeEntry struct {
	Path    string  `json:"path"`
	Mode    string  `json:"mode"`
	Type    string  `json:"type"`
	SHA     *string `json:"sha,omitempty"`
	Content *string `json:"content,omitempty"`
}

func (g *GitHubPages) endpoint(parts ...string) string {
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return g.cfg.API + "/repos/" + strings.Join(parts, "/")
}

func (g *GitHubPages) request(ctx context.Context, method, endpoint string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(b))
		low := strings.ToLower(msg)
		code := ErrGitHubValidation
		switch resp.StatusCode {
		case 401:
			code = ErrGitHubAuth
		case 403, 429:
			if resp.StatusCode == 429 || resp.Header.Get("X-RateLimit-Remaining") == "0" || strings.Contains(low, "rate limit") {
				code = ErrGitHubRateLimited
			} else {
				code = ErrGitHubPermission
			}
		case 404, 301:
			code = ErrGitHubNotFound
		case 409:
			code = ErrGitHubConflict
		}
		return &GitHubError{Status: resp.StatusCode, Message: msg, Code: code}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func (g *GitHubPages) branchEndpoint() string {
	parts := strings.Split(g.cfg.Branch, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return g.cfg.API + "/repos/" + url.PathEscape(g.cfg.Owner) + "/" + url.PathEscape(g.cfg.Repository) + "/git/ref/heads/" + strings.Join(parts, "/")
}

func (g *GitHubPages) branchRefEndpoint() string {
	parts := strings.Split(g.cfg.Branch, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return g.cfg.API + "/repos/" + url.PathEscape(g.cfg.Owner) + "/" + url.PathEscape(g.cfg.Repository) + "/git/refs/heads/" + strings.Join(parts, "/")
}

func (g *GitHubPages) contentsEndpoint(filePath string) string {
	parts := strings.Split(filePath, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return g.cfg.API + "/repos/" + url.PathEscape(g.cfg.Owner) + "/" + url.PathEscape(g.cfg.Repository) + "/contents/" + strings.Join(parts, "/")
}

func (g *GitHubPages) branchRef(ctx context.Context) (string, error) {
	var ref ghRef
	err := g.request(ctx, http.MethodGet, g.branchEndpoint(), nil, &ref)
	return ref.Object.SHA, err
}

func (g *GitHubPages) createBranch(ctx context.Context) (string, error) {
	var repo map[string]any
	if err := g.request(ctx, http.MethodGet, g.endpoint(g.cfg.Owner, g.cfg.Repository), nil, &repo); err != nil {
		return "", err
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	nojekyll := ".nojekyll"
	if g.cfg.Directory != "" {
		nojekyll = path.Join(g.cfg.Directory, nojekyll)
	}
	if err := g.request(ctx, http.MethodPost, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "trees"), map[string]any{"tree": []ghTreeEntry{{Path: nojekyll, Mode: "100644", Type: "blob", Content: ptrString("")}}}, &tree); err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := g.request(ctx, http.MethodPost, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "commits"), map[string]any{"message": "Initialize GitHub Pages", "tree": tree.SHA, "parents": []string{}}, &commit); err != nil {
		return "", err
	}
	if commit.SHA == "" {
		return "", errors.New("publish: github branch initialization returned no commit")
	}
	var created struct {
		Ref string `json:"ref"`
	}
	if err := g.request(ctx, http.MethodPost, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "refs"), map[string]string{"ref": "refs/heads/" + g.cfg.Branch, "sha": commit.SHA}, &created); err != nil {
		return "", err
	}
	return commit.SHA, nil
}

func ptrString(s string) *string { return &s }

func (g *GitHubPages) seedEmptyRepo(ctx context.Context) (string, error) {
	nojekyll := ".nojekyll"
	if g.cfg.Directory != "" {
		nojekyll = path.Join(g.cfg.Directory, nojekyll)
	}
	var created struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	body := map[string]string{"message": "Initialize repository for GitHub Pages", "content": "", "branch": g.cfg.Branch}
	if err := g.request(ctx, http.MethodPut, g.contentsEndpoint(nojekyll), body, &created); err != nil {
		return "", err
	}
	if created.Commit.SHA == "" {
		return "", errors.New("publish: github contents initialization returned no commit")
	}
	return created.Commit.SHA, nil
}

// TestConnection checks that the configured branch is readable using the token.
func (g *GitHubPages) TestConnection(ctx context.Context) error {
	if err := g.cfg.Validate(); err != nil {
		return err
	}
	sha, err := g.branchRef(ctx)
	if err != nil {
		var gh *GitHubError
		if errors.As(err, &gh) && (gh.Status == 404 || gh.Status == 409) {
			var probe ghBlob
			return g.request(ctx, http.MethodPost, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "blobs"), map[string]string{"content": "sorus connection test", "encoding": "utf-8"}, &probe)
		}
		return err
	}
	if sha == "" {
		return errors.New("publish: github branch returned no commit")
	}
	var probe ghBlob
	return g.request(ctx, http.MethodPost, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "blobs"), map[string]string{"content": "sorus connection test", "encoding": "utf-8"}, &probe)
}

// Deploy writes changed blobs, removes only paths in the previous manifest, and advances the branch atomically.
func (g *GitHubPages) Deploy(ctx context.Context, dir string, prev, next Manifest, progress func(Progress)) (Result, error) {
	res := Result{FirstTime: prev == nil}
	if err := g.cfg.Validate(); err != nil {
		return res, err
	}
	ref, err := g.branchRef(ctx)
	if err != nil {
		var gh *GitHubError
		if errors.As(err, &gh) && gh.Status == 404 {
			ref, err = g.createBranch(ctx)
		} else if errors.As(err, &gh) && gh.Status == 409 {
			ref, err = g.seedEmptyRepo(ctx)
		}
		if err != nil {
			return res, err
		}
	}
	var parent ghCommit
	if err = g.request(ctx, http.MethodGet, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "commits", ref), nil, &parent); err != nil {
		return res, err
	}
	plan := NewPlan(prev, next)
	res.Unchanged = plan.Unchanged
	entries := make([]any, 0, len(plan.Upload)+len(plan.Delete))
	total := plan.Total()
	done := 0
	for _, name := range plan.Upload {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rel, e := CleanRel(name)
		if e != nil {
			return res, e
		}
		if progress != nil {
			progress(Progress{Done: done, Total: total, Path: rel})
		}
		f, e := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if e != nil {
			return res, e
		}
		data, e := io.ReadAll(f)
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return res, e
		}
		if len(data) > 100<<20 {
			return res, fmt.Errorf("%w: %s", ErrGitHubFileTooLarge, rel)
		}
		p := rel
		if g.cfg.Directory != "" {
			p = path.Join(g.cfg.Directory, rel)
		}
		entry := ghTreeEntry{Path: p, Mode: "100644", Type: "blob"}
		if len(data) < 100<<10 && isUTF8Text(data) {
			content := string(data)
			entry.Content = &content
		} else {
			blob := map[string]string{"content": base64.StdEncoding.EncodeToString(data), "encoding": "base64"}
			var created ghBlob
			if e = g.request(ctx, http.MethodPost, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "blobs"), blob, &created); e != nil {
				return res, fmt.Errorf("publish: github upload %s: %w", rel, e)
			}
			entry.SHA = &created.SHA
		}
		entries = append(entries, entry)
		res.Uploaded++
		done++
	}
	for _, name := range plan.Delete {
		if e := ctx.Err(); e != nil {
			return res, e
		}
		rel, e := CleanRel(name)
		if e != nil {
			return res, e
		}
		if progress != nil {
			progress(Progress{Done: done, Total: total, Path: rel})
		}
		p := rel
		if g.cfg.Directory != "" {
			p = path.Join(g.cfg.Directory, rel)
		}
		entries = append(entries, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": nil})
		res.Deleted++
		done++
	}
	if len(plan.Upload) > 0 || len(plan.Delete) > 0 {
		p := ".nojekyll"
		if g.cfg.Directory != "" {
			p = path.Join(g.cfg.Directory, p)
		}
		empty := ""
		entries = append(entries, ghTreeEntry{Path: p, Mode: "100644", Type: "blob", Content: &empty})
	}
	if len(entries) > 0 {
		var tree struct {
			SHA string `json:"sha"`
		}
		treeBody := map[string]any{"base_tree": parent.Tree.SHA, "tree": entries}
		treeEndpoint := g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "trees")
		if err = g.request(ctx, http.MethodPost, treeEndpoint, treeBody, &tree); err != nil {
			return res, err
		}
		var commit struct {
			SHA string `json:"sha"`
		}
		if err = g.request(ctx, http.MethodPost, g.endpoint(g.cfg.Owner, g.cfg.Repository, "git", "commits"), map[string]any{"message": "Publish site with sorus", "tree": tree.SHA, "parents": []string{ref}}, &commit); err != nil {
			return res, err
		}
		if err = g.request(ctx, http.MethodPatch, g.branchRefEndpoint(), map[string]any{"sha": commit.SHA, "force": false}, nil); err != nil {
			return res, err
		}
	}
	if progress != nil {
		progress(Progress{Done: total, Total: total})
	}
	return res, nil
}
