package publish

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	testAccount  = "0123456789abcdef0123456789abcdef"
	testProject  = "my-site"
	testAPIToken = "cf-api-token"
)

// fakePages は Cloudflare Pages の API の偽物。受けた要求を「メソッド パス 認証ヘッダー」の形で記録する。
type fakePages struct {
	t   *testing.T
	dir string

	mu       sync.Mutex
	requests []string
	// jwts は upload-token が順に返す JWT。expireFirstUpload なら最初の upload を 401 で断る。
	jwts              []string
	expireFirstUpload bool
	// present は Cloudflare にすでにあるハッシュ（check-missing で返さない）。
	present  map[string]bool
	uploaded map[string]string
	upserted []string
	manifest map[string]string
	// projectStatus が 0 でなければ、プロジェクトの取得をその状態コードで断る。
	projectStatus int
	// live なら、プロジェクトにはすでに本番の版（公開中のサイト）がある。
	live bool
}

func (f *fakePages) ok(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
}

func (f *fakePages) fail(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": code, "message": msg}}})
}

func (f *fakePages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+" "+auth)
	project := "/client/v4/accounts/" + testAccount + "/pages/projects/" + testProject
	switch r.Method + " " + r.URL.Path {
	case "GET " + project:
		if f.projectStatus != 0 {
			f.fail(w, f.projectStatus, 8000007, "Project not found")
			return
		}
		var canonical any
		if f.live {
			canonical = map[string]any{"id": "dep-0", "environment": "production", "url": "https://old." + testProject + ".pages.dev"}
		}
		f.ok(w, map[string]any{"name": testProject, "subdomain": testProject + "-x1y.pages.dev", "production_branch": "main",
			"canonical_deployment": canonical})
	case "GET " + project + "/upload-token":
		if auth != "Bearer "+testAPIToken {
			f.fail(w, 403, 10000, "Authentication error")
			return
		}
		jwt := f.jwts[0]
		if len(f.jwts) > 1 {
			f.jwts = f.jwts[1:]
		}
		f.ok(w, map[string]any{"jwt": jwt})
	case "POST /client/v4/pages/assets/check-missing":
		var in struct{ Hashes []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		missing := []string{}
		for _, h := range in.Hashes {
			if !f.present[h] {
				missing = append(missing, h)
			}
		}
		f.ok(w, missing)
	case "POST /client/v4/pages/assets/upload":
		if f.expireFirstUpload {
			f.expireFirstUpload = false
			f.fail(w, 401, 8000013, "JWT expired")
			return
		}
		var items []struct {
			Key      string `json:"key"`
			Value    string `json:"value"`
			Base64   bool   `json:"base64"`
			Metadata struct {
				ContentType string `json:"contentType"`
			} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
			f.t.Errorf("upload body: %v", err)
		}
		for _, it := range items {
			data, err := base64.StdEncoding.DecodeString(it.Value)
			if err != nil || !it.Base64 || it.Metadata.ContentType == "" {
				f.t.Errorf("upload item %s: base64 %v contentType %q err %v", it.Key, it.Base64, it.Metadata.ContentType, err)
			}
			f.uploaded[it.Key] = string(data) + "|" + it.Metadata.ContentType
		}
		f.ok(w, nil)
	case "POST /client/v4/pages/assets/upsert-hashes":
		var in struct{ Hashes []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.upserted = in.Hashes
		f.ok(w, nil)
	case "POST " + project + "/deployments":
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			f.t.Errorf("deployment content type = %q", r.Header.Get("Content-Type"))
		}
		if err := json.Unmarshal([]byte(r.FormValue("manifest")), &f.manifest); err != nil {
			f.t.Errorf("manifest: %v", err)
		}
		f.ok(w, map[string]any{"id": "dep-1", "url": "https://abc123." + testProject + ".pages.dev"})
	default:
		f.fail(w, 404, 7003, "No route for that URI")
	}
}

func newFakePages(t *testing.T) (*fakePages, *Cloudflare) {
	f := &fakePages{t: t, jwts: []string{"jwt-1"}, present: map[string]bool{}, uploaded: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewCloudflare(CloudflareConfig{AccountID: testAccount, APIToken: testAPIToken, Project: testProject, API: srv.URL + "/client/v4"})
}

func TestCloudflareDeployFollowsDirectUploadFlow(t *testing.T) {
	f, cf := newFakePages(t)
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "<h1>top</h1>", "static/site.css": "body{}", "feed.xml": "<rss/>"})
	next := mustManifest(t, dir)
	// static/site.css は前回の版で送り済み（Cloudflare にある）。
	f.present[cfAssetKey("static/site.css", next["static/site.css"])] = true
	prev := Manifest{"articles/gone/index.html": {SHA256: "old", Size: 3}, "static/site.css": next["static/site.css"]}

	var last Progress
	res, err := cf.Deploy(context.Background(), dir, prev, next, func(p Progress) { last = p })
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	project := "/client/v4/accounts/" + testAccount + "/pages/projects/" + testProject
	want := []string{
		"GET " + project + "/upload-token Bearer " + testAPIToken,
		"POST /client/v4/pages/assets/check-missing Bearer jwt-1",
		"POST /client/v4/pages/assets/upload Bearer jwt-1",
		"POST /client/v4/pages/assets/upsert-hashes Bearer jwt-1",
		"POST " + project + "/deployments Bearer " + testAPIToken,
	}
	if !reflect.DeepEqual(f.requests, want) {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(f.requests, "\n"), strings.Join(want, "\n"))
	}
	// まだ無い2ファイルだけを送る。
	if len(f.uploaded) != 2 || f.uploaded[cfAssetKey("index.html", next["index.html"])] != "<h1>top</h1>|text/html; charset=utf-8" {
		t.Errorf("uploaded = %v", f.uploaded)
	}
	if len(f.upserted) != 3 {
		t.Errorf("upserted %d hashes, want all 3", len(f.upserted))
	}
	wantManifest := map[string]string{}
	for name, file := range next {
		wantManifest["/"+name] = cfAssetKey(name, file)
	}
	if !reflect.DeepEqual(f.manifest, wantManifest) {
		t.Errorf("manifest = %v, want %v", f.manifest, wantManifest)
	}
	if res.Uploaded != 2 || res.Unchanged != 1 || res.Deleted != 1 || res.URL != "https://abc123."+testProject+".pages.dev" {
		t.Errorf("result = %+v", res)
	}
	if last.Done != 2 || last.Total != 2 {
		t.Errorf("last progress = %+v", last)
	}
}

func TestCloudflareDeployRefreshesExpiredUploadToken(t *testing.T) {
	f, cf := newFakePages(t)
	f.jwts = []string{"jwt-1", "jwt-2"}
	f.expireFirstUpload = true
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	if _, err := cf.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	var uploads []string
	for _, r := range f.requests {
		if strings.Contains(r, "/pages/assets/upload ") {
			uploads = append(uploads, r)
		}
	}
	if want := []string{"POST /client/v4/pages/assets/upload Bearer jwt-1", "POST /client/v4/pages/assets/upload Bearer jwt-2"}; !reflect.DeepEqual(uploads, want) {
		t.Errorf("uploads = %v, want retry with a fresh JWT %v", uploads, want)
	}
	if !slices.Contains(f.requests, "POST /client/v4/pages/assets/upsert-hashes Bearer jwt-2") {
		t.Errorf("later requests did not use the refreshed JWT: %v", f.requests)
	}
}

// 初めて反映するプロジェクトに公開中のサイトがあれば、置き換えてよいと言われるまで何も送らない。
func TestCloudflareFirstDeployRefusesToReplaceLiveSite(t *testing.T) {
	f, cf := newFakePages(t)
	f.live = true
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	next := mustManifest(t, dir)
	res, err := cf.Deploy(context.Background(), dir, nil, next, nil)
	if !errors.Is(err, ErrLiveSite) {
		t.Fatalf("Deploy = %+v, %v, want ErrLiveSite", res, err)
	}
	project := "/client/v4/accounts/" + testAccount + "/pages/projects/" + testProject
	if want := []string{"GET " + project + " Bearer " + testAPIToken}; !reflect.DeepEqual(f.requests, want) {
		t.Errorf("requests = %v, want only %v", f.requests, want)
	}
	if f.manifest != nil || len(f.uploaded) != 0 {
		t.Errorf("deployed despite the live site: manifest %v, uploaded %v", f.manifest, f.uploaded)
	}

	// 利用者が「置き換える」を選べば反映する。
	confirmed := NewCloudflare(CloudflareConfig{AccountID: testAccount, APIToken: testAPIToken, Project: testProject,
		API: cf.cfg.API, ReplaceLiveSite: true})
	if _, err := confirmed.Deploy(context.Background(), dir, nil, next, nil); err != nil {
		t.Fatalf("Deploy after confirming = %v", err)
	}
	if len(f.manifest) != 1 || f.manifest["/index.html"] == "" {
		t.Errorf("manifest after confirming = %v", f.manifest)
	}

	// 2回目からは（前回の Manifest があれば）尋ねない。
	f.manifest = nil
	if _, err := cf.Deploy(context.Background(), dir, next, next, nil); err != nil || f.manifest == nil {
		t.Errorf("second Deploy = %v, manifest %v", err, f.manifest)
	}
}

// 空のプロジェクト（まだ版が無い）には、尋ねずに反映する。
func TestCloudflareFirstDeployToEmptyProject(t *testing.T) {
	f, cf := newFakePages(t)
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	res, err := cf.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil)
	if err != nil {
		t.Fatalf("Deploy = %v", err)
	}
	if !res.FirstTime || res.Uploaded != 1 || len(f.manifest) != 1 {
		t.Errorf("result = %+v, manifest %v", res, f.manifest)
	}
	project := "/client/v4/accounts/" + testAccount + "/pages/projects/" + testProject
	if len(f.requests) == 0 || f.requests[0] != "GET "+project+" Bearer "+testAPIToken {
		t.Errorf("first deploy did not look at the project first: %v", f.requests)
	}
}

func TestCloudflareProjectErrors(t *testing.T) {
	f, cf := newFakePages(t)
	p, err := cf.Project(context.Background())
	if err != nil || p.URL() != "https://"+testProject+"-x1y.pages.dev" {
		t.Fatalf("Project = %+v, %v", p, err)
	}
	if f.requests[0] != "GET /client/v4/accounts/"+testAccount+"/pages/projects/"+testProject+" Bearer "+testAPIToken {
		t.Errorf("request = %q", f.requests[0])
	}
	f.projectStatus = http.StatusNotFound
	if _, err := cf.Project(context.Background()); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("Project(404) = %v, want ErrProjectNotFound", err)
	}
	f.projectStatus = http.StatusForbidden
	if _, err := cf.Project(context.Background()); !errors.Is(err, ErrAuth) {
		t.Errorf("Project(403) = %v, want ErrAuth", err)
	}
	bad := NewCloudflare(CloudflareConfig{AccountID: testAccount, APIToken: "wrong", Project: testProject, API: cf.cfg.API})
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	if _, err := bad.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil); !errors.Is(err, ErrAuth) {
		t.Errorf("Deploy with a wrong token = %v, want ErrAuth", err)
	}
}

func TestCloudflareRefusesOversizedFile(t *testing.T) {
	f, cf := newFakePages(t)
	dir := t.TempDir()
	big := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, cfMaxFileSize+1); err != nil {
		t.Fatal(err)
	}
	if _, err := cf.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("Deploy = %v, want ErrFileTooLarge", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("requests were sent for an oversized file: %v", f.requests)
	}
}

func TestCloudflareConfigValidate(t *testing.T) {
	ok := CloudflareConfig{AccountID: testAccount, APIToken: "t", Project: "my-site"}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate(valid) = %v", err)
	}
	for _, c := range []CloudflareConfig{
		{AccountID: "short", APIToken: "t", Project: "my-site"},
		{AccountID: testAccount, APIToken: "t", Project: "My Site"},
		{AccountID: testAccount, APIToken: "t", Project: "../x"},
		{AccountID: testAccount, APIToken: " ", Project: "my-site"},
	} {
		if err := c.Validate(); !errors.Is(err, ErrInvalidCloudflareConfig) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidCloudflareConfig", c, err)
		}
	}
}

func TestCloudflareErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "<html>bad gateway</html>")
	}))
	defer srv.Close()
	cf := NewCloudflare(CloudflareConfig{AccountID: testAccount, APIToken: "t", Project: testProject, API: srv.URL})
	_, err := cf.Project(context.Background())
	var cerr *CloudflareError
	if !errors.As(err, &cerr) || cerr.Status != http.StatusBadGateway {
		t.Errorf("Project = %v, want CloudflareError 502", err)
	}
	if !strings.Contains(fmt.Sprint(err), "502") {
		t.Errorf("error text = %v", err)
	}
}
