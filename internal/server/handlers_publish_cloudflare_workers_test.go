package server

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	testCFWSubdomain = "my-account"
	testCFWToken     = "cfw-api-token-abcdefghijklmnopqrstuvwxyz"
)

// fakeWorkersAPI は Cloudflare Workers の Static Assets の Direct Upload の API の偽物。exists の Worker はもうあり、
// deployments は Worker ごとに公開した版の数。noSubdomain ならアカウントに workers.dev のサブドメインが無い。
// publishCloudflareAPI をこの偽物に向ける。
type fakeWorkersAPI struct {
	mu          sync.Mutex
	exists      map[string]bool
	deployments map[string]int
	workersDev  map[string]bool
	noSubdomain bool
	// headers は最後に公開した版の assets.config._headers。
	headers string
}

func newFakeWorkersAPI(t *testing.T) *fakeWorkersAPI {
	t.Helper()
	f := &fakeWorkersAPI{exists: map[string]bool{}, deployments: map[string]int{}, workersDev: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	old := publishCloudflareAPI
	publishCloudflareAPI = srv.URL
	t.Cleanup(func() { publishCloudflareAPI = old })
	return f
}

func (f *fakeWorkersAPI) deployed(script string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deployments[script]
}

func (f *fakeWorkersAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, success bool, result any, code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body := map[string]any{"success": success, "errors": []any{}, "result": result}
		if !success {
			body["errors"] = []any{map[string]any{"code": code, "message": msg}}
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	if r.Header.Get("Authorization") == "Bearer wrong" {
		reply(http.StatusForbidden, false, nil, 10000, "Authentication error")
		return
	}
	account := "/accounts/" + testCFAccount
	rest, isScript := strings.CutPrefix(r.URL.Path, account+"/workers/scripts/")
	script, sub, _ := strings.Cut(rest, "/")
	switch {
	case isScript && r.Method == http.MethodGet && sub == "settings":
		if !f.exists[script] {
			reply(http.StatusNotFound, false, nil, 10007, "This Worker does not exist on your account.")
			return
		}
		reply(http.StatusOK, true, map[string]any{"bindings": []any{}}, 0, "")
	case isScript && r.Method == http.MethodPost && sub == "assets-upload-session":
		var in struct {
			Manifest map[string]struct{ Hash string } `json:"manifest"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		var hashes []string
		seen := map[string]bool{}
		for _, e := range in.Manifest {
			if !seen[e.Hash] {
				seen[e.Hash] = true
				hashes = append(hashes, e.Hash)
			}
		}
		reply(http.StatusOK, true, map[string]any{"jwt": "upload-jwt", "buckets": [][]string{hashes}}, 0, "")
	case r.Method == http.MethodPost && r.URL.Path == account+"/workers/assets/upload":
		reply(http.StatusCreated, true, map[string]any{"jwt": "completion-jwt"}, 0, "")
	case isScript && r.Method == http.MethodPut && sub == "":
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FormName() == "metadata" {
				var meta struct {
					Assets struct {
						JWT    string            `json:"jwt"`
						Config map[string]string `json:"config"`
					} `json:"assets"`
				}
				data, _ := io.ReadAll(p)
				_ = json.Unmarshal(data, &meta)
				if meta.Assets.JWT != "completion-jwt" {
					reply(http.StatusBadRequest, false, nil, 10021, "invalid assets jwt")
					return
				}
				f.headers = meta.Assets.Config["_headers"]
			}
		}
		f.deployments[script]++
		f.exists[script] = true
		reply(http.StatusOK, true, map[string]any{"id": script}, 0, "")
	case isScript && r.Method == http.MethodPost && sub == "subdomain":
		f.workersDev[script] = true
		reply(http.StatusOK, true, map[string]any{"enabled": true}, 0, "")
	case r.Method == http.MethodGet && r.URL.Path == account+"/workers/subdomain":
		if f.noSubdomain {
			reply(http.StatusNotFound, false, nil, 10007, "workers.api.error.subdomain_not_found")
			return
		}
		reply(http.StatusOK, true, map[string]any{"subdomain": testCFWSubdomain}, 0, "")
	default:
		reply(http.StatusNotFound, false, nil, 7003, "No route")
	}
}

func workersURL(script string) string {
	return "https://" + script + "." + testCFWSubdomain + ".workers.dev"
}

func saveWorkersDestination(t *testing.T, c *client, script string) {
	t.Helper()
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"cloudflare_workers"},
		"cfw_account_id": {testCFAccount}, "cfw_api_token": {testCFWToken}, "cfw_script": {script}}), "/admin/publish?flash=publish_saved")
}

// 公開先の画面に Cloudflare Workers を選べる欄があり、保存すると API トークンを画面に出さずに残す。
func TestPublishCloudflareWorkersSettings(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	page := c.get("/admin/publish").Body
	expectContains(t, page, `value="cloudflare_workers"`, `data-kind-panel="cloudflare_workers"`, `name="cfw_account_id"`,
		`name="cfw_api_token"`, `name="cfw_script"`, "「Workers スクリプト」「編集」", "おすすめ")

	// 入力の誤りは画面に出し、保存しない。貼り付けたトークンは画面に戻さない。
	res := c.postForm("/admin/publish", url.Values{"kind": {"cloudflare_workers"}, "cfw_account_id": {testCFAccount},
		"cfw_api_token": {testCFWToken}, "cfw_script": {"My Site!"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "Worker 名は、半角の英小文字・数字・ハイフン", `value="My Site!"`)
	if strings.Contains(res.Body, testCFWToken) {
		t.Error("the API token was rendered back on a validation error")
	}
	res = c.postForm("/admin/publish", url.Values{"kind": {"cloudflare_workers"}, "cfw_account_id": {"not-an-id"},
		"cfw_api_token": {testCFWToken}, "cfw_script": {"my-site"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "アカウント ID は、Cloudflare の画面に出ている32文字の英数字")
	if env.server.loadPublishConfig().configured() {
		t.Fatal("refused config was saved")
	}

	// 大文字や前後の空白は直して保存する。
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"cloudflare_workers"},
		"cfw_account_id": {" " + strings.ToUpper(testCFAccount) + " "}, "cfw_api_token": {testCFWToken}, "cfw_script": {" My-Site "}}),
		"/admin/publish?flash=publish_saved")
	cfg := env.server.loadPublishConfig()
	if cfg.Kind != publishKindCloudflareWorkers || cfg.CloudflareWorkers.AccountID != testCFAccount ||
		cfg.CloudflareWorkers.Script != "my-site" || cfg.CloudflareWorkers.APIToken != testCFWToken || !cfg.configured() {
		t.Fatalf("saved config = %+v", cfg)
	}
	if got := cfg.labelIn("ja"); got != "Cloudflare Workers（my-site）" {
		t.Errorf("label = %q", got)
	}
	page = c.get("/admin/publish").Body
	if strings.Contains(page, testCFWToken) {
		t.Error("publish page renders the API token")
	}
	expectContains(t, page, "保存済み: ", "Worker 名: my-site")

	// トークンの欄を空のまま保存し直しても、保存済みのトークンを残す。アカウントを変えたら残さない。
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"cloudflare_workers"},
		"cfw_account_id": {testCFAccount}, "cfw_script": {"my-site"}}), "/admin/publish?flash=publish_saved")
	if got := env.server.loadPublishConfig().CloudflareWorkers.APIToken; got != testCFWToken {
		t.Errorf("token after saving with an empty field = %q", got)
	}
	res = c.postForm("/admin/publish", url.Values{"kind": {"cloudflare_workers"},
		"cfw_account_id": {"fedcba9876543210fedcba9876543210"}, "cfw_script": {"my-site"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "API トークンを貼り付けてください")

	// ほかの公開先に切り替えても Workers の設定は残り、Cloudflare Pages の設定も壊さない。
	saveCloudflareDestination(t, c, "pages-site")
	cfg = env.server.loadPublishConfig()
	if cfg.Kind != publishKindCloudflare || cfg.Cloudflare.Project != "pages-site" || cfg.CloudflareWorkers.Script != "my-site" {
		t.Errorf("config after switching to Pages = %+v", cfg)
	}

	// 静的サイトの設定を解除すると、Workers の設定（トークンを含む）も消す。
	expectRedirect(t, c.postForm("/admin/publish/method/clear", url.Values{"method": {"static"}}), "/admin/")
	if got := env.server.getSetting(publishSettingCloudflareWorkers, ""); got != "" {
		t.Errorf("workers settings after clearing = %q", got)
	}

	c.useEnglish()
	expectContains(t, c.get("/admin/publish").Body, "Cloudflare Workers", "Recommended", `"Account" / "Workers Scripts" / "Edit"`)
}

// Cloudflare Pages の設定があれば、Workers の欄にも同じアカウント ID を入れておく（トークンは引き継がない）。
func TestPublishCloudflareWorkersPrefillsAccountFromPages(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	saveCloudflareDestination(t, c, "pages-site")
	page := c.get("/admin/publish").Body
	expectContains(t, page, `name="cfw_account_id" type="text" value="`+testCFAccount+`"`)
	if env.server.loadPublishConfig().CloudflareWorkers.APIToken != "" {
		t.Error("the Pages token was copied to Workers")
	}
}

// 新しい Worker に反映すると、公開サイトURLを workers.dev の URL にして書き出し、成功してから保存する。
func TestPublishCloudflareWorkersReflect(t *testing.T) {
	cf := newFakeWorkersAPI(t)
	env := newTestEnvWith(t, Config{BaseURL: "http://localhost:8080"}, nil)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	saveWorkersDestination(t, c, "my-media")
	// 保存しただけでは workers.dev の URL が分からないため、公開サイトURLは変えない。
	if got := env.server.getSetting("base_url", ""); got == workersURL("my-media") {
		t.Fatalf("base_url was filled before reflecting: %q", got)
	}

	out := c.reflectAndWait()
	if !out.OK || cf.deployed("my-media") != 1 || out.SiteURL != workersURL("my-media") {
		t.Fatalf("reflect = %+v, deployed %d", out, cf.deployed("my-media"))
	}
	if got := env.server.getSetting("base_url", ""); got != workersURL("my-media") {
		t.Errorf("base_url = %q, want %q", got, workersURL("my-media"))
	}
	if !cf.workersDev["my-media"] {
		t.Error("the first reflect did not enable workers.dev")
	}
	// 書き出しの _headers はファイルとしてではなく、配信の設定として送る。
	if !strings.Contains(cf.headers, "Content-Security-Policy:") {
		t.Errorf("assets.config._headers = %q", cf.headers)
	}
	expectContains(t, strings.Join(out.Details, "\n"), "公開をやめたファイル")
	expectContains(t, c.get("/admin/publish/panel").Body, "Cloudflare Workers（my-media）")

	// 2回目からは前回の反映を覚えているため、置き換えを尋ねずに反映する。
	if out := c.reflectAndWait(); !out.OK || out.ReplaceSite || cf.deployed("my-media") != 2 {
		t.Fatalf("second reflect = %+v, deployed %d", out, cf.deployed("my-media"))
	}
}

// 初めて反映する Worker がもうあれば、「置き換える」を押すまで送らない。
func TestPublishCloudflareWorkersAsksBeforeReplacingExistingWorker(t *testing.T) {
	cf := newFakeWorkersAPI(t)
	cf.exists["my-site"] = true
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	saveWorkersDestination(t, c, "my-site")

	out := c.reflectAndWait()
	const question = "Cloudflare Workers には、すでに同じ名前の Worker「my-site」があります。sorus のサイトで置き換えますか？"
	if out.OK || !out.ReplaceSite || out.Message != question {
		t.Fatalf("first reflect to an existing Worker = %+v", out)
	}
	if n := cf.deployed("my-site"); n != 0 {
		t.Fatalf("deployed %d times without confirmation", n)
	}
	expectContains(t, c.get("/admin/publish").Body, question, `action="/admin/publish/replace-site"`)

	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusAccepted)
	if out := waitReflectDone(t, env.server); !out.OK || cf.deployed("my-site") != 1 {
		t.Fatalf("reflect after confirming = %+v, deployed %d", out, cf.deployed("my-site"))
	}
}

// 独自ドメインの公開サイトURLは workers.dev の URL で上書きしない。workers.dev のサブドメインが無くても反映できる。
func TestPublishCloudflareWorkersKeepsCustomDomain(t *testing.T) {
	cf := newFakeWorkersAPI(t)
	cf.noSubdomain = true
	const customDomain = "https://www.example.test"
	env := newTestEnvWith(t, Config{BaseURL: customDomain}, nil)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	saveWorkersDestination(t, c, "custom-site")
	out := c.reflectAndWait()
	if !out.OK || out.SiteURL != customDomain || cf.deployed("custom-site") != 1 {
		t.Fatalf("reflect = %+v, deployed %d", out, cf.deployed("custom-site"))
	}
	if got := env.server.getSetting("base_url", customDomain); got != customDomain {
		t.Errorf("base_url = %q, want %q", got, customDomain)
	}
	expectContains(t, strings.Join(out.Warnings, "\n"), "workers.dev のサブドメインがまだ無い")
}

// workers.dev のサブドメインも独自ドメインも無ければ、何も送らずに、サブドメインの決め方を知らせる。
func TestPublishCloudflareWorkersNeedsSubdomainOrCustomDomain(t *testing.T) {
	cf := newFakeWorkersAPI(t)
	cf.noSubdomain = true
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	saveWorkersDestination(t, c, "my-site")
	out := c.reflectAndWait()
	if out.OK || cf.deployed("my-site") != 0 || !strings.Contains(out.Message, "workers.dev のサブドメインがまだありません") {
		t.Fatalf("reflect = %+v, deployed %d", out, cf.deployed("my-site"))
	}
}

func TestPublishCloudflareWorkersTestConnection(t *testing.T) {
	cf := newFakeWorkersAPI(t)
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"cloudflare_workers"}, "cfw_account_id": {testCFAccount}, "cfw_api_token": {testCFWToken}, "cfw_script": {"new-site"}}
	test := func() publishTestResult {
		t.Helper()
		res := c.postForm("/admin/publish/test", form)
		expectStatus(t, res, http.StatusOK)
		var tr publishTestResult
		if err := json.Unmarshal([]byte(res.Body), &tr); err != nil {
			t.Fatal(err)
		}
		return tr
	}

	tr := test()
	if !tr.OK || !strings.Contains(strings.Join(tr.Details, "\n"), "Worker「new-site」はまだありません") ||
		!strings.Contains(strings.Join(tr.Details, "\n"), "公開サイトの URL: "+workersURL("new-site")) {
		t.Errorf("test for a new Worker = %+v", tr)
	}
	if env.server.loadPublishConfig().configured() || cf.deployed("new-site") != 0 {
		t.Error("the connection test saved the config or deployed")
	}

	cf.exists["new-site"] = true
	if tr := test(); !tr.OK || !strings.Contains(strings.Join(tr.Details, "\n"), "すでにあります") {
		t.Errorf("test for an existing Worker = %+v", tr)
	}

	cf.noSubdomain = true
	if tr := test(); !tr.OK || !strings.Contains(strings.Join(tr.Details, "\n"), "workers.dev のサブドメインがまだありません") {
		t.Errorf("test without a workers.dev subdomain = %+v", tr)
	}

	form.Set("cfw_api_token", "wrong")
	if tr := test(); tr.OK || !strings.Contains(tr.Message, "Workers スクリプトの編集") {
		t.Errorf("test with a wrong token = %+v", tr)
	}
}
