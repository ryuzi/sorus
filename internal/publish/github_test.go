package publish

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type githubRoundTrip func(*http.Request) (*http.Response, error)

func (f githubRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func ghResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGitHubPagesValidate(t *testing.T) {
	valid := GitHubPagesConfig{Owner: "sorus-team", Repository: "site", Branch: "main", Token: "token"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*GitHubPagesConfig){
		"owner":      func(c *GitHubPagesConfig) { c.Owner = "bad/name" },
		"repository": func(c *GitHubPagesConfig) { c.Repository = ".." },
		"branch":     func(c *GitHubPagesConfig) { c.Branch = "bad\nbranch" },
		"directory":  func(c *GitHubPagesConfig) { c.Directory = "../outside" },
		"token":      func(c *GitHubPagesConfig) { c.Token = " " },
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

func TestGitHubPagesDeployBuildsOneScopedCommit(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"index.html": "new home", "assets/app.js": "new js"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var requests []string
	var gotEntries []ghTreeEntry
	var rawTree []byte
	cfg := GitHubPagesConfig{Owner: "team", Repository: "docs", Branch: "pages", Directory: "docs", Token: "secret", API: "https://github.test"}
	cfg.HTTPClient = &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/pages"):
			return ghResponse(200, `{"object":{"sha":"parent"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/git/commits/parent"):
			return ghResponse(200, `{"tree":{"sha":"base-tree"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/git/blobs"):
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["encoding"] != "base64" || b["content"] == "" {
				t.Errorf("blob request = %#v", b)
			}
			return ghResponse(201, `{"sha":"blob-sha"}`), nil
		case strings.HasSuffix(r.URL.Path, "/git/trees"):
			bodyBytes, _ := io.ReadAll(r.Body)
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(bodyBytes, &raw)
			rawTree = raw["tree"]
			var body struct {
				BaseTree string        `json:"base_tree"`
				Tree     []ghTreeEntry `json:"tree"`
			}
			_ = json.Unmarshal(bodyBytes, &body)
			if body.BaseTree != "base-tree" {
				t.Errorf("base_tree = %q", body.BaseTree)
			}
			gotEntries = body.Tree
			return ghResponse(201, `{"sha":"tree-sha"}`), nil
		case strings.HasSuffix(r.URL.Path, "/git/commits"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["tree"] != "tree-sha" || body["message"] != "Publish site with sorus" {
				t.Errorf("commit request = %#v", body)
			}
			return ghResponse(201, `{"sha":"commit-sha"}`), nil
		case strings.HasSuffix(r.URL.Path, "/git/refs/heads/pages"):
			return ghResponse(200, `{"ref":"refs/heads/pages"}`), nil
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return ghResponse(404, `{}`), nil
		}
	})}
	prev := Manifest{"index.html": {SHA256: "old", Size: 3}, "old.txt": {SHA256: "removed", Size: 7}, "assets/app.js": {SHA256: "same", Size: 4}}
	next := Manifest{"index.html": {SHA256: "new", Size: 9}, "assets/app.js": {SHA256: "changed", Size: 6}}
	var progress []Progress
	got, err := NewGitHubPages(cfg).Deploy(context.Background(), dir, prev, next, func(p Progress) { progress = append(progress, p) })
	if err != nil {
		t.Fatal(err)
	}
	if got.Uploaded != 2 || got.Deleted != 1 || got.Unchanged != 0 || got.FirstTime {
		t.Fatalf("result = %+v", got)
	}
	if len(gotEntries) != 4 {
		t.Fatalf("tree entries = %+v", gotEntries)
	}
	paths := map[string]ghTreeEntry{}
	for _, e := range gotEntries {
		paths[e.Path] = e
	}
	if paths["docs/index.html"].Content == nil || *paths["docs/index.html"].Content != "new home" || paths["docs/assets/app.js"].Content == nil || *paths["docs/assets/app.js"].Content != "new js" {
		t.Fatalf("upload paths = %+v", paths)
	}
	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(rawTree, &rawEntries); err != nil {
		t.Fatal(err)
	}
	for _, entry := range rawEntries {
		var p string
		_ = json.Unmarshal(entry["path"], &p)
		if p == "docs/.nojekyll" || p == "docs/index.html" {
			if _, ok := entry["sha"]; ok {
				t.Errorf("inline entry %s unexpectedly has sha: %s", p, entry)
			}
			if _, ok := entry["content"]; !ok {
				t.Errorf("inline entry %s has no content: %s", p, entry)
			}
		}
		if p == "docs/old.txt" && string(entry["sha"]) != "null" {
			t.Errorf("delete entry sha = %s, want null", entry["sha"])
		}
	}
	if e, ok := paths["docs/old.txt"]; !ok || e.SHA != nil {
		t.Fatalf("delete entry = %+v, exists %v", e, ok)
	}
	if len(requests) != 5 {
		t.Fatalf("requests = %v", requests)
	}
	if len(progress) != 4 || progress[len(progress)-1].Done != 3 {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestGitHubBranchRefEscapesEachSegment(t *testing.T) {
	g := NewGitHubPages(GitHubPagesConfig{Owner: "team", Repository: "site", Branch: "release/pages", API: "https://github.test"})
	if got, want := g.branchRefEndpoint(), "https://github.test/repos/team/site/git/refs/heads/release/pages"; got != want {
		t.Fatalf("branch ref endpoint = %q, want %q", got, want)
	}
	if got, want := g.contentsEndpoint("docs/.nojekyll"), "https://github.test/repos/team/site/contents/docs/.nojekyll"; got != want {
		t.Fatalf("contents endpoint = %q, want %q", got, want)
	}
}

func TestGitHubPagesDeployNoChangesDoesNotCommit(t *testing.T) {
	called := false
	cfg := GitHubPagesConfig{Owner: "team", Repository: "site", Branch: "main", Token: "token", API: "https://github.test", HTTPClient: &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/git/ref/heads/main") {
			return ghResponse(200, `{"object":{"sha":"head"}}`), nil
		}
		if r.Method != http.MethodGet {
			called = true
		}
		return ghResponse(200, `{"tree":{"sha":"base"}}`), nil
	})}}
	m := Manifest{"index.html": {SHA256: "same", Size: 4}}
	got, err := NewGitHubPages(cfg).Deploy(context.Background(), "", m, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if called || got.Unchanged != 1 || got.Uploaded != 0 || got.Deleted != 0 {
		t.Fatalf("result=%+v committed=%v", got, called)
	}
}

func TestGitHubPagesAuthAndTestConnection(t *testing.T) {
	cfg := GitHubPagesConfig{Owner: "team", Repository: "site", Branch: "main", Token: "token", API: "https://github.test", HTTPClient: &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing bearer token")
		}
		return ghResponse(http.StatusForbidden, `{"message":"Resource not accessible"}`), nil
	})}}
	err := NewGitHubPages(cfg).TestConnection(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("TestConnection error = %v", err)
	}
}

func TestGitHubPagesDeployCreatesBranchOn404(t *testing.T) {
	testGitHubPagesDeployInitializesMissingRef(t, http.StatusNotFound)
}

func TestGitHubPagesDeployHandlesEmptyRepo(t *testing.T) {
	testGitHubPagesDeployInitializesMissingRef(t, http.StatusConflict)
}

func testGitHubPagesDeployInitializesMissingRef(t *testing.T, refStatus int) {
	t.Helper()
	var sawInitialize bool
	var sawCreateRef bool
	cfg := GitHubPagesConfig{Owner: "team", Repository: "site", Branch: "gh-pages", Token: "token", API: "https://github.test"}
	cfg.HTTPClient = &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/git/ref/heads/gh-pages"):
			return ghResponse(refStatus, `{"message":"not found"}`), nil
		case refStatus == http.StatusNotFound && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/team/site"):
			return ghResponse(http.StatusOK, `{}`), nil
		case refStatus == http.StatusNotFound && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/trees"):
			sawInitialize = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["base_tree"]; ok {
				t.Error("initial tree unexpectedly has base_tree")
			}
			return ghResponse(http.StatusCreated, `{"sha":"initial-tree"}`), nil
		case refStatus == http.StatusNotFound && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/commits"):
			return ghResponse(http.StatusCreated, `{"sha":"initial-commit"}`), nil
		case refStatus == http.StatusNotFound && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			sawCreateRef = true
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode refs body: %v", err)
			}
			if body["ref"] != "refs/heads/gh-pages" || body["sha"] != "initial-commit" {
				t.Errorf("refs body = %#v", body)
			}
			return ghResponse(http.StatusCreated, `{}`), nil
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/contents/.nojekyll"):
			if refStatus != http.StatusConflict {
				t.Error("Contents API used for missing branch")
			}
			sawInitialize = true
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode contents body: %v", err)
			}
			if body["message"] != "Initialize repository for GitHub Pages" || body["content"] != "" || body["branch"] != "gh-pages" {
				t.Errorf("contents initialization body = %#v", body)
			}
			return ghResponse(http.StatusCreated, `{"commit":{"sha":"initial-commit"}}`), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/git/commits/initial-commit"):
			return ghResponse(http.StatusOK, `{"tree":{"sha":"initial-tree"}}`), nil
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return ghResponse(http.StatusInternalServerError, `{}`), nil
		}
	})}
	if _, err := NewGitHubPages(cfg).Deploy(context.Background(), t.TempDir(), Manifest{}, Manifest{}, nil); err != nil {
		t.Fatal(err)
	}
	if !sawInitialize {
		t.Fatal("Deploy did not create the missing branch")
	}
	if refStatus == http.StatusNotFound && !sawCreateRef {
		t.Fatal("Deploy did not create git ref for missing branch")
	}
}

func TestGitHubPagesDeployTreesConflictReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("site"), 0o600); err != nil {
		t.Fatal(err)
	}
	sawRefs := false
	cfg := GitHubPagesConfig{Owner: "team", Repository: "site", Branch: "main", Token: "token", API: "https://github.test", HTTPClient: &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			return ghResponse(http.StatusOK, `{"object":{"sha":"head"}}`), nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/git/commits/head"):
			return ghResponse(http.StatusOK, `{"tree":{"sha":"base"}}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/trees"):
			return ghResponse(http.StatusConflict, `{"message":"conflict"}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			sawRefs = true
			return ghResponse(http.StatusCreated, `{}`), nil
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return ghResponse(http.StatusInternalServerError, `{}`), nil
		}
	})}}
	_, err := NewGitHubPages(cfg).Deploy(context.Background(), dir, Manifest{}, Manifest{"index.html": {SHA256: "site", Size: 4}}, nil)
	var gh *GitHubError
	if !errors.As(err, &gh) || gh.Status != http.StatusConflict {
		t.Fatalf("Deploy error = %v, want GitHub 409", err)
	}
	if sawRefs {
		t.Fatal("Deploy called git/refs after git/trees conflict")
	}
}

func TestGitHubPagesDeployRejectsOver100MB(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "large.bin")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(100<<20 + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := GitHubPagesConfig{Owner: "team", Repository: "site", Branch: "main", Token: "token", API: "https://github.test", HTTPClient: &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			return ghResponse(http.StatusOK, `{"object":{"sha":"head"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/git/commits/head"):
			return ghResponse(http.StatusOK, `{"tree":{"sha":"base"}}`), nil
		default:
			t.Errorf("unexpected request before size rejection: %s %s", r.Method, r.URL.Path)
			return ghResponse(http.StatusInternalServerError, `{}`), nil
		}
	})}}
	_, err = NewGitHubPages(cfg).Deploy(context.Background(), dir, Manifest{}, Manifest{"large.bin": {SHA256: "large", Size: 100<<20 + 1}}, nil)
	if !errors.Is(err, ErrGitHubFileTooLarge) {
		t.Fatalf("Deploy error = %v, want ErrGitHubFileTooLarge", err)
	}
}
