package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sorus/internal/i18n"
	"sorus/internal/publish"
)

func TestPublishNetlifySaveAndRenderKeepsTokenSecret(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"netlify"}, "netlify_site_id": {"site-123"}, "netlify_token": {"nfp-super-secret"}}
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	cfg := env.server.loadPublishConfig()
	if cfg.Kind != publishKindNetlify || !cfg.configured() || cfg.Netlify.SiteID != "site-123" || cfg.Netlify.Token != "nfp-super-secret" {
		t.Fatalf("saved config = %+v", cfg)
	}
	page := c.get("/admin/publish").Body
	for _, want := range []string{`value="netlify" checked`, `value="site-123"`, `Personal Access Token`, `Site details`} {
		if !strings.Contains(page, want) {
			t.Errorf("publish page missing %q", want)
		}
	}
	if strings.Contains(page, "nfp-super-secret") {
		t.Fatal("Netlify token was rendered")
	}
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"netlify"}, "netlify_site_id": {"site-123"}}), "/admin/publish?flash=publish_saved")
	if got := env.server.loadPublishConfig().Netlify.Token; got != "nfp-super-secret" {
		t.Fatalf("blank token save replaced saved secret: %q", got)
	}
}

func TestNetlifyManualAndAutomaticReflectionDeploy(t *testing.T) {
	for _, trigger := range []string{publishTriggerManual, publishTriggerArticlePublish} {
		t.Run(trigger, func(t *testing.T) {
			var calls int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-123":
					_, _ = w.Write([]byte(`{"id":"site-123","ssl_url":"https://example.netlify.app"}`))
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sites/site-123/deploys":
					var body struct {
						Files map[string]string `json:"files"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode deploy request: %v", err)
					}
					required := make([]string, 0, len(body.Files))
					for _, digest := range body.Files {
						required = append(required, digest)
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "deploy-1", "required": required, "ssl_url": "https://example.netlify.app"})
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/deploys/deploy-1/files/"):
					w.WriteHeader(http.StatusCreated)
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-123/deploys/deploy-1":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "deploy-1", "state": "ready", "ssl_url": "https://example.netlify.app"})
				default:
					t.Errorf("unexpected Netlify request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			oldAPI := publishNetlifyAPI
			publishNetlifyAPI = api.URL + "/api/v1"
			defer func() { publishNetlifyAPI = oldAPI }()

			env := newTestEnv(t)
			if err := env.server.saveBaseURL(""); err != nil {
				t.Fatal(err)
			}
			if err := env.server.savePublishConfig(publishConfig{Kind: publishKindNetlify, Netlify: publishNetlifyConfig{SiteID: "site-123", Token: "token"}}); err != nil {
				t.Fatal(err)
			}
			out := env.server.runReflect(context.Background(), &publishState{}, trigger)
			if !out.OK || out.Trigger != trigger || calls < 3 {
				t.Fatalf("reflection = %+v; API calls = %d", out, calls)
			}
			if got := env.server.getSetting("base_url", ""); got != "https://example.netlify.app" {
				t.Fatalf("base_url = %q", got)
			}
		})
	}
}

func TestPublishNetlifyConnectionTestUsesEnteredOrSavedToken(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/sites/site-123" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer saved-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"site-123","ssl_url":"https://example.netlify.app"}`))
	}))
	defer api.Close()
	oldAPI := publishNetlifyAPI
	publishNetlifyAPI = api.URL + "/api/v1"
	defer func() { publishNetlifyAPI = oldAPI }()

	env := newTestEnv(t)
	if err := env.server.savePublishConfig(publishConfig{Kind: publishKindNetlify, Netlify: publishNetlifyConfig{SiteID: "site-123", Token: "saved-token"}}); err != nil {
		t.Fatal(err)
	}
	c := env.loggedIn(admin)
	res := c.postForm("/admin/publish/test", url.Values{"kind": {"netlify"}, "netlify_site_id": {"site-123"}})
	expectStatus(t, res, http.StatusOK)
	var got publishTestResult
	if err := json.Unmarshal([]byte(res.Body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !strings.Contains(strings.Join(got.Details, " "), "https://example.netlify.app") {
		t.Fatalf("connection test = %+v", got)
	}
}

func TestPublishNetlifyInvalidConfigIsNotSaved(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	expectStatus(t, c.postForm("/admin/publish", url.Values{"kind": {"netlify"}, "netlify_site_id": {"../bad"}, "netlify_token": {"token"}}), http.StatusUnprocessableEntity)
	if env.server.loadPublishConfig().configured() {
		t.Fatal("invalid Netlify destination was saved")
	}
}

func TestPublishNetlifySaveAndTestRejectMissingCSRF(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"netlify"}, "netlify_site_id": {"site-123"}, "netlify_token": {"nfp-attacker"}}
	for _, path := range []string{"/admin/publish", "/admin/publish/test"} {
		req, err := http.NewRequest(http.MethodPost, env.srv.URL+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		expectStatus(t, c.do(req), http.StatusForbidden)
	}
	if env.server.loadPublishConfig().Netlify.Token != "" {
		t.Fatal("Netlify token was saved without a CSRF token")
	}
}

func TestPublishNetlifyPanelEnglish(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	c.get("/admin/set-lang?lang=en&return=/admin")
	if err := env.server.savePublishConfig(publishConfig{Kind: publishKindNetlify, Netlify: publishNetlifyConfig{SiteID: "site-123", Token: "nfp-secret"}}); err != nil {
		t.Fatal(err)
	}
	page := c.get("/admin/publish").Body
	for _, key := range []string{"publish_netlify_guide", "publish_netlify_site_help", "publish_netlify_token_help", "publish_netlify_kind_desc", "publish_github_token_saved"} {
		want := template.HTMLEscapeString(i18n.Translate("en", key))
		if !strings.Contains(page, want) {
			t.Errorf("English publish page missing %s: %q", key, want)
		}
	}
	expectNotContains(t, page, "Site ID とトークンを確認する", "nfp-secret")
}

func TestDescribePublishErrorNetlify(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"auth":      {&publish.NetlifyError{Status: http.StatusUnauthorized}, "Personal Access Token が無効"},
		"not found": {fmt.Errorf("wrap: %w", &publish.NetlifyError{Status: http.StatusNotFound}), "Site ID のサイトが見つかりません"},
		"mismatch":  {fmt.Errorf("x: %w", publish.ErrNetlifySiteNotFound), "Site ID のサイトが見つかりません"},
		"rate":      {&publish.NetlifyError{Status: http.StatusTooManyRequests}, "利用回数制限"},
		"other":     {&publish.NetlifyError{Status: http.StatusUnprocessableEntity}, "Netlify が反映を受け付けませんでした"},
	} {
		if got := describePublishError(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("%s: describePublishError = %q, want %q", name, got, tc.want)
		}
	}
}

func TestPublishNetlifyAdminAuthorizationOnly(t *testing.T) {
	env := newTestEnv(t)
	paths := []string{"/admin/publish", "/admin/publish/test", "/admin/publish/run", "/admin/publish/replace-site"}

	// 1. 未ログイン: /admin/login へリダイレクト
	for _, p := range paths {
		res := env.newClient().get(p)
		if res.StatusCode != http.StatusFound && res.StatusCode != http.StatusSeeOther {
			t.Errorf("unauthenticated %s: status = %d, want redirect", p, res.StatusCode)
		}
	}

	// 2. Editor, Writer: 403 Forbidden
	for _, u := range []testUser{chief, studentA} {
		client := env.loggedIn(u)
		for _, p := range paths {
			res := client.get(p)
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("role %s on %s: status = %d, want 403", u.Username, p, res.StatusCode)
			}
		}
	}
}

func TestNetlifyLiveSiteOverwriteProtection(t *testing.T) {
	var deploysCreated int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/existing-site":
			// 既存サイトに本番デプロイが存在することを模擬
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "existing-site",
				"deploy_id": "dep-old-production",
				"ssl_url":   "https://existing-site.netlify.app",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sites/existing-site/deploys":
			deploysCreated++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       "dep-new-1",
				"required": []string{},
				"ssl_url":  "https://existing-site.netlify.app",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/existing-site/deploys/dep-new-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "dep-new-1",
				"state":   "ready",
				"ssl_url": "https://existing-site.netlify.app",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	oldAPI := publishNetlifyAPI
	publishNetlifyAPI = api.URL + "/api/v1"
	defer func() { publishNetlifyAPI = oldAPI }()

	env := newTestEnv(t)
	if err := env.server.savePublishConfig(publishConfig{
		Kind:    publishKindNetlify,
		Netlify: publishNetlifyConfig{SiteID: "existing-site", Token: "token-secret"},
	}); err != nil {
		t.Fatal(err)
	}

	st := &publishState{}
	// 1回目の反映: 既存デプロイがあるため、上書き確認を要求しデプロイは作成されない
	out1 := env.server.runReflect(context.Background(), st, publishTriggerManual)
	if out1.OK || !out1.ReplaceSite {
		t.Fatalf("first reflection should require replace approval: %+v", out1)
	}
	if !strings.Contains(out1.Message, "Netlify") || !strings.Contains(out1.Message, "置き換えますか") {
		t.Fatalf("unexpected message: %s", out1.Message)
	}
	if deploysCreated != 0 {
		t.Fatalf("deploys should not be created without replace approval, got: %d", deploysCreated)
	}

	// 置き換えを承認（replaceLive をセット）して2回目の反映
	cfg := env.server.loadPublishConfig()
	st.mu.Lock()
	st.replaceLive = cfg.destinationID()
	st.mu.Unlock()

	out2 := env.server.runReflect(context.Background(), st, publishTriggerManual)
	if !out2.OK {
		t.Fatalf("second reflection after approval failed: %+v", out2)
	}
	if deploysCreated != 1 {
		t.Fatalf("deploy was not created after approval: %d", deploysCreated)
	}
}

func TestNetlifyDeployFailureDoesNotSaveManifest(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/fail-site":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fail-site", "ssl_url": "https://fail.netlify.app"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sites/fail-site/deploys":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "dep-fail-1", "required": []string{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/fail-site/deploys/dep-fail-1":
			// deploy が error 状態になる
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":            "dep-fail-1",
				"state":         "error",
				"error_message": "CDN sync failed",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	oldAPI := publishNetlifyAPI
	publishNetlifyAPI = api.URL + "/api/v1"
	defer func() { publishNetlifyAPI = oldAPI }()

	env := newTestEnv(t)
	cfg := publishConfig{Kind: publishKindNetlify, Netlify: publishNetlifyConfig{SiteID: "fail-site", Token: "token"}}
	if err := env.server.savePublishConfig(cfg); err != nil {
		t.Fatal(err)
	}

	out := env.server.runReflect(context.Background(), &publishState{}, publishTriggerManual)
	if out.OK {
		t.Fatalf("reflection unexpectedly succeeded: %+v", out)
	}
	destID := cfg.destinationID()
	manifest, err := env.server.loadPublishManifest(destID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest != nil {
		t.Fatalf("manifest should not be saved on deploy error, got: %+v", manifest)
	}
}

func TestNetlifyRejectsInvalidBaseURLDuringReflection(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-bad-url":
			// localhost を返す不正な URL
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "site-bad-url", "ssl_url": "http://localhost:3000"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	oldAPI := publishNetlifyAPI
	publishNetlifyAPI = api.URL + "/api/v1"
	defer func() { publishNetlifyAPI = oldAPI }()

	env := newTestEnv(t)
	if err := env.server.saveBaseURL("https://legit.example.jp"); err != nil {
		t.Fatal(err)
	}
	if err := env.server.savePublishConfig(publishConfig{Kind: publishKindNetlify, Netlify: publishNetlifyConfig{SiteID: "site-bad-url", Token: "token"}}); err != nil {
		t.Fatal(err)
	}

	out := env.server.runReflect(context.Background(), &publishState{}, publishTriggerManual)
	if out.OK {
		t.Fatalf("reflection should fail due to invalid site url: %+v", out)
	}
	if got := env.server.getBaseURL(); got != "https://legit.example.jp" {
		t.Fatalf("base_url was overwritten with invalid value: %q", got)
	}
}

func TestNetlifyBaseURLResolvedBeforeExport(t *testing.T) {
	deployState := "ready"
	existingDeployID := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-stage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "site-stage",
				"deploy_id": existingDeployID,
				"ssl_url":   "https://new-url.netlify.app",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sites/site-stage/deploys":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "dep-1", "required": []string{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sites/site-stage/deploys/dep-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "dep-1",
				"state":   deployState,
				"ssl_url": "https://new-url.netlify.app",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	oldAPI := publishNetlifyAPI
	publishNetlifyAPI = api.URL + "/api/v1"
	defer func() { publishNetlifyAPI = oldAPI }()

	env := newTestEnv(t)

	// 1. ExportOptions.BaseURL による一時的な上書き書き出しと DB/メモリ不変性の検証
	if err := env.server.saveBaseURL(""); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "export-override")
	rep, err := env.server.Export(context.Background(), ExportOptions{
		OutDir:  outDir,
		BaseURL: "https://temp-override.netlify.app",
	})
	if err != nil {
		t.Fatalf("Export with BaseURL failed: %v", err)
	}
	if got := env.server.getBaseURL(); got != "" {
		t.Errorf("server.getBaseURL() was modified during export: %q", got)
	}
	if len(rep.Warnings) > 0 {
		t.Errorf("unexpected warnings with valid BaseURL override: %v", rep.Warnings)
	}
	indexHTML, err := os.ReadFile(filepath.Join(outDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indexHTML), `https://temp-override.netlify.app`) {
		t.Errorf("index.html missing BaseURL override: %s", string(indexHTML))
	}

	cfg := publishConfig{Kind: publishKindNetlify, Netlify: publishNetlifyConfig{SiteID: "site-stage", Token: "token"}}
	if err := env.server.savePublishConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// 2. デプロイ未承認時（ErrLiveSite）に DB の base_url が書き換わらないこと
	existingDeployID = "dep-prev"
	stLive := &publishState{}
	outLive := env.server.runReflect(context.Background(), stLive, publishTriggerManual)
	if !outLive.ReplaceSite {
		t.Fatalf("expected ReplaceSite on existing deploy, got: %+v", outLive)
	}
	if got := env.server.getBaseURL(); got != "" {
		t.Fatalf("base_url was overwritten on ErrLiveSite: %q, want empty", got)
	}
	existingDeployID = ""

	// 3. デプロイ失敗時に DB の base_url が書き換わらないこと
	deployState = "error"
	stFail := &publishState{}
	outFail := env.server.runReflect(context.Background(), stFail, publishTriggerManual)
	if outFail.OK {
		t.Fatalf("expected reflect failure on deploy error, got: %+v", outFail)
	}
	if got := env.server.getBaseURL(); got != "" {
		t.Fatalf("base_url was overwritten on deploy failure: %q, want empty", got)
	}
	deployState = "ready"

	// 4. 初回公開成功時に Netlify の URL が BaseURL として確定保存されること
	st := &publishState{}
	out := env.server.runReflect(context.Background(), st, publishTriggerManual)
	if !out.OK {
		t.Fatalf("reflection failed: %+v", out)
	}
	if got := env.server.getBaseURL(); got != "https://new-url.netlify.app" {
		t.Fatalf("BaseURL was not saved after deploy success: %q, want https://new-url.netlify.app", got)
	}

	// 5. カスタムドメインが設定されている場合は上書きされず維持される
	const customDomain = "https://custom.example.jp"
	if err := env.server.saveBaseURL(customDomain); err != nil {
		t.Fatal(err)
	}
	outCustom := env.server.runReflect(context.Background(), st, publishTriggerManual)
	if !outCustom.OK {
		t.Fatalf("reflection with custom domain failed: %+v", outCustom)
	}
	if got := env.server.getBaseURL(); got != customDomain {
		t.Fatalf("custom domain was overwritten: %q, want %q", got, customDomain)
	}
}
