package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	testCFAccount = "0123456789abcdef0123456789abcdef"
	testCFToken   = "cf-api-token"
)

// fakeCloudflareAPI は Cloudflare Pages の API の偽物。live のプロジェクトには本番の版（公開中のサイト）があり、
// deployments はプロジェクトごとに作った版の数。publishCloudflareAPI をこの偽物に向ける。
type fakeCloudflareAPI struct {
	mu          sync.Mutex
	live        map[string]bool
	deployments map[string]int
}

func newFakeCloudflareAPI(t *testing.T) *fakeCloudflareAPI {
	t.Helper()
	f := &fakeCloudflareAPI{live: map[string]bool{}, deployments: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	old := publishCloudflareAPI
	publishCloudflareAPI = srv.URL
	t.Cleanup(func() { publishCloudflareAPI = old })
	return f
}

func (f *fakeCloudflareAPI) deployed(project string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deployments[project]
}

func (f *fakeCloudflareAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}
	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	prefix := "/accounts/" + testCFAccount + "/pages/projects/"
	rest, isProject := strings.CutPrefix(r.URL.Path, prefix)
	project, sub, _ := strings.Cut(rest, "/")
	switch {
	case isProject && r.Method == http.MethodGet && sub == "":
		var canonical any
		if f.live[project] {
			canonical = map[string]any{"id": "dep-0", "environment": "production", "url": "https://old." + project + ".pages.dev"}
		}
		ok(map[string]any{"name": project, "subdomain": project + ".pages.dev", "production_branch": "main", "canonical_deployment": canonical})
	case isProject && sub == "upload-token":
		ok(map[string]any{"jwt": "jwt-1"})
	case r.URL.Path == "/pages/assets/check-missing":
		var in struct{ Hashes []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		ok(in.Hashes)
	case r.URL.Path == "/pages/assets/upload", r.URL.Path == "/pages/assets/upsert-hashes":
		ok(nil)
	case isProject && r.Method == http.MethodPost && sub == "deployments":
		f.deployments[project]++
		f.live[project] = true
		ok(map[string]any{"id": "dep-new", "url": "https://new." + project + ".pages.dev"})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 7003, "message": "No route"}}})
	}
}

func saveCloudflareDestination(t *testing.T, c *client, project string) {
	t.Helper()
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"cloudflare"},
		"cf_account_id": {testCFAccount}, "cf_api_token": {testCFToken}, "cf_project": {project}}), "/admin/publish?flash=publish_saved")
}

const replaceLiveSiteQuestion = "この Cloudflare Pages のプロジェクトには、すでに公開中のサイトがあります。sorus のサイトで置き換えますか？"

// 初めて反映する Cloudflare Pages のプロジェクトに公開中のサイトがあれば、「置き換える」を押すまで送らない。
// 「置き換える」は、尋ねた公開先への次の1回の反映だけに使う。
func TestPublishCloudflareAsksBeforeReplacingLiveSite(t *testing.T) {
	cf := newFakeCloudflareAPI(t)
	cf.live["my-site"], cf.live["other-site"] = true, true
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	saveCloudflareDestination(t, c, "my-site")

	// 確認を待っていないうちは、置き換えを受け付けない。
	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusConflict)

	out := c.reflectAndWait()
	if out.OK || !out.ReplaceSite || out.Message != replaceLiveSiteQuestion {
		t.Fatalf("first reflect to a live project = %+v", out)
	}
	if n := cf.deployed("my-site"); n != 0 {
		t.Fatalf("deployed %d times without confirmation", n)
	}
	page := c.get("/admin/publish").Body
	expectContains(t, page, replaceLiveSiteQuestion, `action="/admin/publish/replace-site"`, ">置き換える</button>", `data-publish-result="confirm"`)
	expectContains(t, c.get("/admin/publish/panel").Body, "置き換えるかの確認を待っています")
	var st publishStatus
	expectStatus(t, c.getJSON("/admin/publish/status", &st), http.StatusOK)
	if st.Last == nil || !st.Last.ReplaceSite {
		t.Errorf("status = %+v, want replace_site", st.Last)
	}
	expectContains(t, c.get("/static/js/reflect.js").Body, "replace_site", "/admin/publish/replace-site", "T('js_pub_replace')")

	// 「サイトに反映」をもう一度押しても、置き換えずに尋ねる。
	if out := c.reflectAndWait(); !out.ReplaceSite || cf.deployed("my-site") != 0 {
		t.Fatalf("second reflect without confirmation = %+v, deployed %d", out, cf.deployed("my-site"))
	}

	// 「置き換える」を押すと反映する。
	res := c.postJSONAccept("/admin/publish/replace-site")
	expectStatus(t, res, http.StatusAccepted)
	if out := waitReflectDone(t, env.server); !out.OK || out.ReplaceSite || cf.deployed("my-site") != 1 {
		t.Fatalf("reflect after confirming = %+v, deployed %d", out, cf.deployed("my-site"))
	}
	if strings.Contains(c.get("/admin/publish").Body, `action="/admin/publish/replace-site"`) {
		t.Error("the replace button is still shown after replacing")
	}
	// 2回目からは前回の反映を覚えているため、尋ねずに反映する。
	if out := c.reflectAndWait(); !out.OK || cf.deployed("my-site") != 2 {
		t.Fatalf("next reflect = %+v, deployed %d", out, cf.deployed("my-site"))
	}

	// 確認は使い切り。ほかのプロジェクトに変えると、また尋ねる。
	saveCloudflareDestination(t, c, "other-site")
	if out := c.reflectAndWait(); !out.ReplaceSite || cf.deployed("other-site") != 0 {
		t.Fatalf("reflect to another live project = %+v, deployed %d", out, cf.deployed("other-site"))
	}
	// 尋ねたあとで公開先を変えたら、前の確認の「置き換える」は使えない。
	saveCloudflareDestination(t, c, "my-site")
	if strings.Contains(c.get("/admin/publish").Body, `action="/admin/publish/replace-site"`) {
		t.Error("the replace button is shown for a destination that was not asked about")
	}
	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusConflict)
	if cf.deployed("other-site") != 0 {
		t.Errorf("other-site deployed %d times without confirmation", cf.deployed("other-site"))
	}
}

// 空のプロジェクト（まだ版が無い）には、尋ねずに反映する。
func TestPublishCloudflareFirstDeployToEmptyProject(t *testing.T) {
	cf := newFakeCloudflareAPI(t)
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	saveCloudflareDestination(t, c, "empty-site")
	if out := c.reflectAndWait(); !out.OK || out.ReplaceSite || cf.deployed("empty-site") != 1 {
		t.Fatalf("first reflect to an empty project = %+v, deployed %d", out, cf.deployed("empty-site"))
	}
}

func TestPublishCloudflareKeepsConfiguredCustomDomain(t *testing.T) {
	newFakeCloudflareAPI(t)
	const customDomain = "https://www.example.test"
	env := newTestEnvWith(t, Config{BaseURL: customDomain}, nil)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	saveCloudflareDestination(t, c, "custom-domain-site")

	if out := c.reflectAndWait(); !out.OK || out.SiteURL != customDomain {
		t.Fatalf("reflect = %+v, want SiteURL %q", out, customDomain)
	}
}

func TestPublishReplaceSiteAdminOnly(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{studentA, chief} {
		if res := env.loggedIn(u).postJSONAccept("/admin/publish/replace-site"); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s POST replace-site = %d, want 403", u.Username, res.StatusCode)
		}
	}
}
