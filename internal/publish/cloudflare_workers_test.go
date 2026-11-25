package publish

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
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
	testWorker    = "my-site"
	testSubdomain = "my-account"
)

// fakeWorkers は Cloudflare Workers の Static Assets の Direct Upload の API の偽物。受けた要求を
// 「メソッド パス 認証ヘッダー」の形で記録する（パスにはクエリ文字列も含める）。
type fakeWorkers struct {
	t *testing.T

	mu       sync.Mutex
	requests []string
	// sessionJWTs は assets-upload-session が順に返すアップロード用の JWT。
	sessionJWTs []string
	// expireFirstUpload なら最初の upload を 401 で断る（JWT の期限切れ）。
	expireFirstUpload bool
	// bucketSize は buckets の1つのまとまりに入れるハッシュの数。
	bucketSize int
	// present は Cloudflare にすでにあるハッシュ（buckets に入れない）。
	present map[string]bool
	// manifest は最後に受けた manifest、uploaded はハッシュごとの「中身|Content-Type」。
	manifest map[string]cfwManifestEntry
	uploaded map[string]string
	// pending は今の session でまだ送られていないハッシュ。空になった upload の応答で完了の JWT を返す。
	pending map[string]bool
	// metadata は最後の PUT のスクリプトの metadata、parts はその PUT の multipart の部分の名前。
	metadata map[string]any
	parts    []string
	// exists なら Worker がもうある。noSubdomain ならアカウントに workers.dev のサブドメインが無い。
	exists, noSubdomain bool
	// subdomainBody は workers.dev を有効にする要求の本文。
	subdomainBody map[string]any
}

func (f *fakeWorkers) ok(w http.ResponseWriter, status int, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result})
}

func (f *fakeWorkers) fail(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": code, "message": msg}}})
}

func (f *fakeWorkers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" "+auth)
	account := "/client/v4/accounts/" + testAccount
	script := account + "/workers/scripts/" + testWorker
	tokenOK := auth == "Bearer "+testAPIToken
	switch r.Method + " " + r.URL.Path {
	case "GET " + script + "/settings":
		if !tokenOK {
			f.fail(w, 403, 10000, "Authentication error")
			return
		}
		if !f.exists {
			f.fail(w, 404, cfwNotFoundCode, "This Worker does not exist on your account.")
			return
		}
		f.ok(w, 200, map[string]any{"compatibility_date": "2024-01-01", "bindings": []any{}})
	case "POST " + script + "/assets-upload-session":
		if !tokenOK {
			f.fail(w, 403, 10000, "Authentication error")
			return
		}
		var in struct {
			Manifest map[string]cfwManifestEntry `json:"manifest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.t.Errorf("session body: %v", err)
		}
		f.manifest = in.Manifest
		var missing []string
		seen := map[string]bool{}
		for _, e := range in.Manifest {
			if !f.present[e.Hash] && !seen[e.Hash] {
				seen[e.Hash] = true
				missing = append(missing, e.Hash)
			}
		}
		slices.Sort(missing)
		f.pending = map[string]bool{}
		buckets := [][]string{}
		for start := 0; start < len(missing); start += f.bucketSize {
			end := min(start+f.bucketSize, len(missing))
			buckets = append(buckets, missing[start:end])
			for _, h := range missing[start:end] {
				f.pending[h] = true
			}
		}
		jwt := f.sessionJWTs[0]
		if len(f.sessionJWTs) > 1 {
			f.sessionJWTs = f.sessionJWTs[1:]
		}
		if len(buckets) == 0 {
			jwt = "completion-" + jwt
		}
		f.ok(w, 200, map[string]any{"jwt": jwt, "buckets": buckets})
	case "POST " + account + "/workers/assets/upload":
		if r.URL.Query().Get("base64") != "true" {
			f.t.Errorf("upload query = %q, want base64=true", r.URL.RawQuery)
		}
		if f.expireFirstUpload {
			f.expireFirstUpload = false
			f.fail(w, 401, 10001, "JWT expired")
			return
		}
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			f.t.Errorf("upload content type = %q", r.Header.Get("Content-Type"))
			f.fail(w, 400, 10021, "bad request")
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				f.t.Errorf("upload part: %v", err)
				break
			}
			raw, _ := io.ReadAll(p)
			data, err := base64.StdEncoding.DecodeString(string(raw))
			if err != nil {
				f.t.Errorf("upload part %s is not base64: %v", p.FormName(), err)
			}
			if p.FileName() != p.FormName() {
				f.t.Errorf("upload part name %q, file name %q, want the hash for both", p.FormName(), p.FileName())
			}
			f.uploaded[p.FormName()] = string(data) + "|" + p.Header.Get("Content-Type")
			delete(f.pending, p.FormName())
		}
		if len(f.pending) == 0 {
			f.ok(w, 201, map[string]any{"jwt": "completion-" + strings.TrimPrefix(auth, "Bearer ")})
			return
		}
		f.ok(w, 202, map[string]any{"jwt": nil})
	case "PUT " + script:
		if !tokenOK {
			f.fail(w, 403, 10000, "Authentication error")
			return
		}
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			f.t.Errorf("script content type = %q", r.Header.Get("Content-Type"))
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		f.parts = nil
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			f.parts = append(f.parts, p.FormName())
			if p.FormName() == "metadata" {
				if err := json.NewDecoder(p).Decode(&f.metadata); err != nil {
					f.t.Errorf("metadata: %v", err)
				}
			}
		}
		f.exists = true
		f.ok(w, 200, map[string]any{"id": testWorker, "etag": "etag-1"})
	case "POST " + script + "/subdomain":
		_ = json.NewDecoder(r.Body).Decode(&f.subdomainBody)
		f.ok(w, 200, map[string]any{"enabled": true, "previews_enabled": false})
	case "GET " + account + "/workers/subdomain":
		if f.noSubdomain {
			f.fail(w, 404, cfwNotFoundCode, "workers.api.error.subdomain_not_found")
			return
		}
		f.ok(w, 200, map[string]any{"subdomain": testSubdomain})
	default:
		f.fail(w, 404, 7003, "No route for that URI")
	}
}

func newFakeWorkers(t *testing.T) (*fakeWorkers, *CloudflareWorkers) {
	f := &fakeWorkers{t: t, sessionJWTs: []string{"upload-1"}, bucketSize: 2, present: map[string]bool{}, uploaded: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewCloudflareWorkers(CloudflareWorkersConfig{AccountID: testAccount, APIToken: testAPIToken, Script: testWorker, API: srv.URL + "/client/v4/"})
}

func TestCloudflareWorkersDeployFollowsDirectUploadFlow(t *testing.T) {
	f, cw := newFakeWorkers(t)
	dir := t.TempDir()
	headers := "/*\n  X-Content-Type-Options: nosniff\n"
	writeSite(t, dir, map[string]string{"index.html": "<h1>top</h1>", "static/site.css": "body{}", "llms.txt": "# site",
		"feed.xml": "<rss/>", "_headers": headers, "articles/_headers": "not a config file"})
	next := mustManifest(t, dir)
	// static/site.css は前回の版で送り済み（Cloudflare にある）。
	f.present[cfAssetKey("static/site.css", next["static/site.css"])] = true
	prev := Manifest{"articles/gone/index.html": {SHA256: "old", Size: 3}, "static/site.css": next["static/site.css"]}

	var last Progress
	res, err := cw.Deploy(context.Background(), dir, prev, next, func(p Progress) { last = p })
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	account := "/client/v4/accounts/" + testAccount
	script := account + "/workers/scripts/" + testWorker
	upload := "POST " + account + "/workers/assets/upload?base64=true Bearer upload-1"
	want := []string{
		"POST " + script + "/assets-upload-session Bearer " + testAPIToken,
		upload, upload, // まだ無い4ファイルを2つずつ送る。
		"PUT " + script + " Bearer " + testAPIToken,
		"GET " + account + "/workers/subdomain Bearer " + testAPIToken,
	}
	if !reflect.DeepEqual(f.requests, want) {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(f.requests, "\n"), strings.Join(want, "\n"))
	}

	// いちばん上の _headers は manifest に入れず、配信の設定として送る。下の階層の _headers はふつうのファイル。
	wantManifest := map[string]cfwManifestEntry{}
	for name, file := range next {
		if name != "_headers" {
			wantManifest["/"+name] = cfwManifestEntry{Hash: cfAssetKey(name, file), Size: file.Size}
		}
	}
	if !reflect.DeepEqual(f.manifest, wantManifest) {
		t.Errorf("manifest = %v, want %v", f.manifest, wantManifest)
	}
	if len(f.uploaded) != 4 {
		t.Errorf("uploaded %d files, want the 4 missing ones: %v", len(f.uploaded), f.uploaded)
	}
	for name, want := range map[string]string{
		"index.html": "<h1>top</h1>|text/html; charset=utf-8",
		// .txt は送るパソコンの MIME の設定によらず text/plain で公開する（Pages と同じ）。
		"llms.txt": "# site|text/plain; charset=utf-8",
	} {
		if got := f.uploaded[cfAssetKey(name, next[name])]; got != want {
			t.Errorf("uploaded %s = %q, want %q", name, got, want)
		}
	}

	if !reflect.DeepEqual(f.parts, []string{"metadata"}) {
		t.Errorf("script parts = %v, want only metadata (an assets-only Worker)", f.parts)
	}
	if _, ok := f.metadata["main_module"]; ok {
		t.Errorf("metadata has main_module: %v", f.metadata)
	}
	if f.metadata["compatibility_date"] == "" {
		t.Errorf("metadata lacks compatibility_date: %v", f.metadata)
	}
	assets, _ := f.metadata["assets"].(map[string]any)
	config, _ := assets["config"].(map[string]any)
	if assets["jwt"] != "completion-upload-1" {
		t.Errorf("assets.jwt = %v, want the completion token from the last upload", assets["jwt"])
	}
	if config["_headers"] != headers || config["html_handling"] != "auto-trailing-slash" || config["not_found_handling"] != "none" {
		t.Errorf("assets.config = %v", config)
	}
	if f.subdomainBody != nil {
		t.Errorf("enabled workers.dev on a later deploy: %v", f.subdomainBody)
	}

	if res.FirstTime || res.Uploaded != 4 || res.Unchanged != 2 || res.Deleted != 1 || res.URL != "https://"+testWorker+"."+testSubdomain+".workers.dev" {
		t.Errorf("result = %+v", res)
	}
	if last.Done != 4 || last.Total != 4 {
		t.Errorf("last progress = %+v", last)
	}
}

// Cloudflare にすべてのファイルがあれば（buckets が空）、何も送らずに session の JWT で公開する。
func TestCloudflareWorkersDeploySkipsUploadWhenNothingIsMissing(t *testing.T) {
	f, cw := newFakeWorkers(t)
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top", "404.html": "not found"})
	next := mustManifest(t, dir)
	for name, file := range next {
		f.present[cfAssetKey(name, file)] = true
	}
	res, err := cw.Deploy(context.Background(), dir, next, next, nil)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/workers/assets/upload") {
			t.Errorf("uploaded although nothing was missing: %v", f.requests)
		}
	}
	assets, _ := f.metadata["assets"].(map[string]any)
	config, _ := assets["config"].(map[string]any)
	if assets["jwt"] != "completion-upload-1" {
		t.Errorf("assets.jwt = %v, want the session token", assets["jwt"])
	}
	// 404.html があれば、無いページにそのページを返す。
	if config["not_found_handling"] != "404-page" {
		t.Errorf("not_found_handling = %v, want 404-page", config["not_found_handling"])
	}
	if res.Uploaded != 0 || res.Unchanged != 2 {
		t.Errorf("result = %+v", res)
	}
}

// 初めて反映する Worker がもうあれば、置き換えてよいと言われるまで何も送らない。
func TestCloudflareWorkersFirstDeployRefusesToReplaceExistingWorker(t *testing.T) {
	f, cw := newFakeWorkers(t)
	f.exists = true
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	next := mustManifest(t, dir)
	res, err := cw.Deploy(context.Background(), dir, nil, next, nil)
	if !errors.Is(err, ErrLiveSite) {
		t.Fatalf("Deploy = %+v, %v, want ErrLiveSite", res, err)
	}
	settings := "GET /client/v4/accounts/" + testAccount + "/workers/scripts/" + testWorker + "/settings Bearer " + testAPIToken
	if want := []string{settings}; !reflect.DeepEqual(f.requests, want) {
		t.Errorf("requests = %v, want only %v", f.requests, want)
	}
	if f.metadata != nil || len(f.uploaded) != 0 {
		t.Errorf("deployed despite the existing Worker: metadata %v, uploaded %v", f.metadata, f.uploaded)
	}

	// 利用者が「置き換える」を選べば反映し、workers.dev での公開も有効にする。
	confirmed := NewCloudflareWorkers(CloudflareWorkersConfig{AccountID: testAccount, APIToken: testAPIToken, Script: testWorker,
		API: cw.cfg.API, ReplaceLiveSite: true})
	if _, err := confirmed.Deploy(context.Background(), dir, nil, next, nil); err != nil {
		t.Fatalf("Deploy after confirming = %v", err)
	}
	if f.metadata == nil || f.subdomainBody["enabled"] != true {
		t.Errorf("after confirming: metadata %v, subdomain %v", f.metadata, f.subdomainBody)
	}

	// 2回目からは（前回の Manifest があれば）尋ねない。
	f.metadata, f.requests = nil, nil
	if _, err := cw.Deploy(context.Background(), dir, next, next, nil); err != nil || f.metadata == nil {
		t.Errorf("second Deploy = %v, metadata %v", err, f.metadata)
	}
	if slices.Contains(f.requests, settings) {
		t.Errorf("second deploy looked for a live site again: %v", f.requests)
	}
}

// まだ無い Worker には、尋ねずに反映し、workers.dev で公開する。
func TestCloudflareWorkersFirstDeployCreatesWorker(t *testing.T) {
	f, cw := newFakeWorkers(t)
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	res, err := cw.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil)
	if err != nil {
		t.Fatalf("Deploy = %v", err)
	}
	script := "/client/v4/accounts/" + testAccount + "/workers/scripts/" + testWorker
	if len(f.requests) == 0 || f.requests[0] != "GET "+script+"/settings Bearer "+testAPIToken {
		t.Errorf("first deploy did not look for the Worker first: %v", f.requests)
	}
	if !slices.Contains(f.requests, "POST "+script+"/subdomain Bearer "+testAPIToken) || f.subdomainBody["enabled"] != true {
		t.Errorf("first deploy did not enable workers.dev: %v, body %v", f.requests, f.subdomainBody)
	}
	if !res.FirstTime || res.Uploaded != 1 || len(f.manifest) != 1 || len(res.Notes) != 0 {
		t.Errorf("result = %+v, manifest %v", res, f.manifest)
	}
}

// アップロード用の JWT が途中で切れたら、manifest を送り直して新しい JWT で続ける。
func TestCloudflareWorkersDeployRestartsSessionWhenUploadTokenExpires(t *testing.T) {
	f, cw := newFakeWorkers(t)
	f.sessionJWTs = []string{"upload-1", "upload-2"}
	f.expireFirstUpload = true
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	next := mustManifest(t, dir)
	res, err := cw.Deploy(context.Background(), dir, next, next, nil)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	var uploads []string
	for _, r := range f.requests {
		if strings.Contains(r, "/workers/assets/upload") {
			uploads = append(uploads, strings.TrimPrefix(r, "POST /client/v4/accounts/"+testAccount+"/workers/assets/upload?base64=true "))
		}
	}
	if want := []string{"Bearer upload-1", "Bearer upload-2"}; !reflect.DeepEqual(uploads, want) {
		t.Errorf("uploads = %v, want a retry with a fresh token %v", uploads, want)
	}
	assets, _ := f.metadata["assets"].(map[string]any)
	if assets["jwt"] != "completion-upload-2" || res.Uploaded != 1 {
		t.Errorf("assets.jwt = %v, result %+v", assets["jwt"], res)
	}
}

func TestCloudflareWorkersErrors(t *testing.T) {
	f, cw := newFakeWorkers(t)
	bad := NewCloudflareWorkers(CloudflareWorkersConfig{AccountID: testAccount, APIToken: "wrong", Script: testWorker, API: cw.cfg.API})
	if _, err := bad.ScriptExists(context.Background()); !errors.Is(err, ErrAuth) {
		t.Errorf("ScriptExists with a wrong token = %v, want ErrAuth", err)
	}
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "top"})
	next := mustManifest(t, dir)
	if _, err := bad.Deploy(context.Background(), dir, next, next, nil); !errors.Is(err, ErrAuth) {
		t.Errorf("Deploy with a wrong token = %v, want ErrAuth", err)
	}

	// workers.dev のサブドメインが無いアカウント。URL は分からないが、公開はできる。
	f.noSubdomain = true
	if _, err := cw.URL(context.Background()); !errors.Is(err, ErrWorkersSubdomainNotFound) {
		t.Errorf("URL without a subdomain = %v, want ErrWorkersSubdomainNotFound", err)
	}
	res, err := cw.Deploy(context.Background(), dir, next, next, nil)
	if err != nil || res.URL != "" || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "workers.dev") {
		t.Errorf("Deploy without a subdomain = %+v, %v", res, err)
	}
}

func TestCloudflareWorkersRefusesOversizedFile(t *testing.T) {
	f, cw := newFakeWorkers(t)
	dir := t.TempDir()
	big := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, cfMaxFileSize+1); err != nil {
		t.Fatal(err)
	}
	if _, err := cw.Deploy(context.Background(), dir, nil, mustManifest(t, dir), nil); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("Deploy = %v, want ErrFileTooLarge", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("requests were sent for an oversized file: %v", f.requests)
	}
}

func TestCloudflareWorkersConfigValidate(t *testing.T) {
	ok := CloudflareWorkersConfig{AccountID: testAccount, APIToken: "t", Script: "my-site"}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate(valid) = %v", err)
	}
	for _, c := range []CloudflareWorkersConfig{
		{AccountID: "short", APIToken: "t", Script: "my-site"},
		{AccountID: testAccount, APIToken: "t", Script: "My Site"},
		{AccountID: testAccount, APIToken: "t", Script: "../x"},
		{AccountID: testAccount, APIToken: "t", Script: "my-site-"},
		{AccountID: testAccount, APIToken: "t", Script: strings.Repeat("a", 64)},
		{AccountID: testAccount, APIToken: " ", Script: "my-site"},
	} {
		if err := c.Validate(); !errors.Is(err, ErrInvalidCloudflareWorkersConfig) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidCloudflareWorkersConfig", c, err)
		}
	}
}
