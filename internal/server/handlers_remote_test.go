package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// remoteEnv はローカル CMS（local）と、同期 API を有効にした公開サーバー（remote）の組。
type remoteEnv struct {
	local  *testEnv
	remote *syncEnv
}

// newRemoteEnv はローカル CMS と、同期 API を有効にした公開サーバーを起動する。
func newRemoteEnv(t *testing.T) *remoteEnv {
	t.Helper()
	remote := newSyncEnv(t)
	return &remoteEnv{local: newTestEnv(t), remote: remote}
}

// configure はローカル CMS に公開サーバーの URL・同期 API のパスと、sub・role のトークンを保存する。
func (e *remoteEnv) configure(sub string, role auth.Role) string {
	e.local.t.Helper()
	tok := e.remote.token(sub, role)
	cfg := remoteConfig{URL: e.remote.srv.URL, SyncPath: e.remote.server.cfg.SyncAPIPath, Token: tok}
	if err := e.local.server.saveRemoteConfig(cfg); err != nil {
		e.local.t.Fatal(err)
	}
	return tok
}

// postJSON は fetch と同じく X-CSRF-Token 付きでフォームを POST し、JSON の応答を v に読む。
func (c *client) postJSON(path string, form url.Values, v any) *response {
	c.t.Helper()
	res := c.postAPI(path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if v != nil && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal([]byte(res.Body), v); err != nil {
			c.t.Fatalf("decode %s: %v\nbody: %.2000s", path, err, res.Body)
		}
	}
	return res
}

// getJSON は fetch と同じく Accept: application/json で GET する。
func (c *client) getJSON(path string, v any) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	res := c.do(req)
	if v != nil && res.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(res.Body), v); err != nil {
			c.t.Fatalf("decode %s: %v\nbody: %.2000s", path, err, res.Body)
		}
	}
	return res
}

func jsonMessage(t *testing.T, res *response) string {
	t.Helper()
	var v struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(res.Body), &v); err != nil {
		t.Fatalf("decode message: %v\nbody: %.2000s", err, res.Body)
	}
	return v.Message
}

// URL にパスがあれば同期 API のパスとして分け、無ければ同期 API のパスは空（既定の /api/sync）にする。
func TestNormalizeRemoteURL(t *testing.T) {
	const secret = "/api/sync-0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		in, wantBase, wantPath string
	}{
		{"https://media.example.jp", "https://media.example.jp", ""},
		{"  HTTPS://media.example.jp/  ", "https://media.example.jp", ""},
		{"https://media.example.jp" + secret, "https://media.example.jp", secret},
		{" https://media.example.jp" + secret + "/ ", "https://media.example.jp", secret},
		{"https://media.example.jp:8443/api/sync", "https://media.example.jp:8443", "/api/sync"},
		{"https://example.com/sub/dir/", "https://example.com", "/sub/dir"},
		{"http://localhost:8080", "http://localhost:8080", ""},
		{"http://127.0.0.1:9000/", "http://127.0.0.1:9000", ""},
		{"http://127.0.0.1:9000" + secret, "http://127.0.0.1:9000", secret},
		{"http://[::1]:8080", "http://[::1]:8080", ""},
	} {
		base, syncPath, err := normalizeRemoteURL(tc.in)
		if err != nil || base != tc.wantBase || syncPath != tc.wantPath {
			t.Errorf("normalizeRemoteURL(%q) = %q, %q, %v; want %q, %q", tc.in, base, syncPath, err, tc.wantBase, tc.wantPath)
		}
	}
	for _, in := range []string{
		"", "media.example.jp", "http://media.example.jp", "ftp://example.com", "https://user:pass@example.com",
		"https://example.com/?a=1", "https://example.com/#top", "https://", "javascript:alert(1)", "http://localhost.example.com",
		"http://media.example.jp" + secret, "https://example.com/admin/sync", "https://example.com/api/:id",
		"https://example.com/api//sync", "https://example.com/api/sync-%E5%90%8C",
	} {
		if base, syncPath, err := normalizeRemoteURL(in); err == nil {
			t.Errorf("normalizeRemoteURL(%q) = %q, %q, want error", in, base, syncPath)
		}
	}
}

func TestRemoteConfigEndpoint(t *testing.T) {
	const secret = "/api/sync-0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		cfg                  remoteConfig
		wantEndpoint, wantUI string
	}{
		{remoteConfig{}, DefaultSyncAPIPath, ""},
		{remoteConfig{URL: "https://example.com"}, "https://example.com" + DefaultSyncAPIPath, "https://example.com"},
		{remoteConfig{URL: "https://example.com", SyncPath: secret}, "https://example.com" + secret, "https://example.com" + secret},
		// 同期 API のパスを分けて保存する前の設定（URL のパスは接頭辞）も、従来どおり URL + /api/sync に送る。
		{remoteConfig{URL: "https://example.com/blog"}, "https://example.com/blog" + DefaultSyncAPIPath, "https://example.com/blog" + DefaultSyncAPIPath},
	} {
		if got := tc.cfg.endpoint(); got != tc.wantEndpoint {
			t.Errorf("%+v endpoint() = %q, want %q", tc.cfg, got, tc.wantEndpoint)
		}
		if got := tc.cfg.formURL(); got != tc.wantUI {
			t.Errorf("%+v formURL() = %q, want %q", tc.cfg, got, tc.wantUI)
		}
	}
}

func TestParseRemoteToken(t *testing.T) {
	_, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.IssueToken(priv, "tanaka", string(auth.RoleEditor), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := parseRemoteToken(normalizeRemoteToken(" "+tok[:20]+"\n"+tok[20:]+"\n"), time.Now())
	if err != nil {
		t.Fatalf("parseRemoteToken: %v", err)
	}
	if info.Subject != "tanaka" || info.Role != auth.RoleEditor || info.Expired || info.ExpiresAt.Before(time.Now()) {
		t.Errorf("info = %+v", info)
	}
	if info, err := parseRemoteToken(tok, time.Now().Add(2*time.Hour)); err != nil || !info.Expired {
		t.Errorf("token after exp: %+v, %v; want expired", info, err)
	}
	for _, bad := range []string{
		"", "abc", "a.b.c", strings.Repeat("a", maxRemoteTokenLength+1),
		signedToken(priv, `{"alg":"EdDSA","typ":"sorus-cap"}`, `{"sub":"x","role":"owner","exp":9999999999}`),
		signedToken(priv, `{"alg":"EdDSA","typ":"sorus-cap"}`, `{"sub":"","role":"writer","exp":9999999999}`),
		signedToken(priv, `{"alg":"EdDSA","typ":"sorus-cap"}`, `{"sub":"x","role":"writer"}`),
	} {
		if _, err := parseRemoteToken(bad, time.Now()); err == nil {
			t.Errorf("parseRemoteToken(%.40q) succeeded, want error", bad)
		}
	}
}

func TestRemoteSettingsPage(t *testing.T) {
	env := newRemoteEnv(t)
	c := env.local.loggedIn(admin)

	res := c.get("/admin/settings/remote")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, ">公開先</h1>", `name="remote_url"`, `name="remote_token"`, `id="remote-test-button"`, "/static/js/remote_sync.js")

	t.Run("invalid url keeps input", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote", url.Values{"remote_url": {"http://media.example.jp"}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, `value="http://media.example.jp"`, "https://")
		if got := env.local.server.getSetting(remoteSettingURL, ""); got != "" {
			t.Errorf("invalid url was saved: %q", got)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://media.example.jp"}, "remote_token": {"not-a-token"}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "トークンの形式が正しくありません")
	})

	tok := env.remote.token("yamada", auth.RoleWriter)
	res = c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://media.example.jp/"}, "remote_token": {tok + "\n"}})
	expectRedirect(t, res, "/admin/settings/remote?flash=settings_saved")
	if cfg := env.local.server.loadRemoteConfig(); cfg.URL != "https://media.example.jp" || cfg.Token != tok {
		t.Fatalf("saved config = %+v", cfg)
	}

	page := c.get("/admin/settings/remote")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `value="https://media.example.jp"`, maskToken(tok), "yamada", `data-remote-role="writer"`, "記事の提出")
	if strings.Contains(page.Body, tok) {
		t.Error("settings page must not include the full token")
	}
	// writer のトークンでは、校閲待ちの一覧・サイト設定の反映を使えない。
	if strings.Contains(page.Body, "data-remote-pending") || strings.Contains(page.Body, "remote-push-settings") {
		t.Error("page with a writer token shows editor/admin-only controls")
	}

	t.Run("sync url splits into server url and sync path", func(t *testing.T) {
		const secret = "/api/sync-0123456789abcdef0123456789abcdef"
		res := c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://media.example.jp" + secret + "/"}, "remote_token": {""}})
		expectRedirect(t, res, "/admin/settings/remote?flash=settings_saved")
		if cfg := env.local.server.loadRemoteConfig(); cfg.URL != "https://media.example.jp" || cfg.SyncPath != secret || cfg.Token != tok {
			t.Fatalf("config = %+v", cfg)
		}
		// 画面には同期 API のパスまで含めた URL を表示し、そのまま保存し直しても変わらない。
		expectContains(t, c.get("/admin/settings/remote").Body, `value="https://media.example.jp`+secret+`"`)

		// URL だけを保存し直すと、同期 API のパスは既定に戻る。
		res = c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://media.example.jp"}})
		expectRedirect(t, res, "/admin/settings/remote")
		if cfg := env.local.server.loadRemoteConfig(); cfg.URL != "https://media.example.jp" || cfg.SyncPath != "" {
			t.Fatalf("config after saving the server url only = %+v", cfg)
		}
	})

	t.Run("invalid sync path keeps input", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://media.example.jp/admin/sync"}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, `value="https://media.example.jp/admin/sync"`, "同期 API のパス")
		if cfg := env.local.server.loadRemoteConfig(); cfg.URL != "https://media.example.jp" || cfg.SyncPath != "" {
			t.Errorf("invalid sync path changed the config: %+v", cfg)
		}
	})

	t.Run("empty token keeps saved token for same host", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://media.example.jp"}, "remote_token": {""}})
		expectRedirect(t, res, "/admin/settings/remote?flash=settings_saved")
		if cfg := env.local.server.loadRemoteConfig(); cfg.URL != "https://media.example.jp" || cfg.Token != tok {
			t.Errorf("config = %+v", cfg)
		}
	})

	t.Run("changing host without new token is rejected", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://other.example.jp"}, "remote_token": {""}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "接続先を変更する場合は、新しいトークンを入力してください")
		if cfg := env.local.server.loadRemoteConfig(); cfg.URL != "https://media.example.jp" || cfg.Token != tok {
			t.Errorf("rejected host change must not change config: %+v", cfg)
		}
	})

	t.Run("clear token", func(t *testing.T) {
		res := c.postForm("/admin/settings/remote", url.Values{"remote_url": {"https://other.example.jp"}, "clear_token": {"1"}})
		expectRedirect(t, res, "/admin/settings/remote")
		if cfg := env.local.server.loadRemoteConfig(); cfg.Token != "" {
			t.Errorf("token was not cleared: %+v", cfg)
		}
	})

	t.Run("csrf required", func(t *testing.T) {
		res := c.postRaw("/admin/settings/remote", url.Values{"remote_url": {"https://evil.example.com"}})
		expectStatus(t, res, http.StatusForbidden)
	})

	t.Run("login required", func(t *testing.T) {
		res := env.local.newClient().get("/admin/settings/remote")
		expectRedirect(t, res, "/admin/login")
	})
}

func TestRemoteConnectionTest(t *testing.T) {
	env := newRemoteEnv(t)
	c := env.local.loggedIn(admin)

	var res remoteTestResult
	expectStatus(t, c.postJSON("/admin/remote/test", nil, &res), http.StatusOK)
	if res.OK || !strings.Contains(res.Message, "URL とトークン") {
		t.Errorf("unconfigured: %+v", res)
	}

	// 保存前の入力値で試せる。
	tok := env.remote.token("yamada", auth.RoleWriter)
	res = remoteTestResult{}
	expectStatus(t, c.postJSON("/admin/remote/test", url.Values{"remote_url": {env.remote.srv.URL}, "remote_token": {tok}}, &res), http.StatusOK)
	if !res.OK || res.RemoteStatus != http.StatusBadRequest || res.Role != auth.RoleWriter || res.Subject != "yamada" ||
		!res.Permissions.SubmitDraft || res.Permissions.PublishArticle {
		t.Errorf("valid token: %+v", res)
	}

	t.Run("token from another server", func(t *testing.T) {
		_, otherPriv, _ := auth.GenerateKeyPair()
		forged, _ := auth.IssueToken(otherPriv, "yamada", string(auth.RoleAdmin), time.Hour)
		var res remoteTestResult
		c.postJSON("/admin/remote/test", url.Values{"remote_url": {env.remote.srv.URL}, "remote_token": {forged}}, &res)
		if res.OK || res.RemoteStatus != http.StatusUnauthorized || res.Message == "" {
			t.Errorf("forged token: %+v", res)
		}
	})

	t.Run("server without sync api", func(t *testing.T) {
		var res remoteTestResult
		c.postJSON("/admin/remote/test", url.Values{"remote_url": {env.local.srv.URL}, "remote_token": {tok}}, &res)
		if res.OK || res.RemoteStatus != http.StatusNotFound || !strings.Contains(res.Message, "同期 API が見つかりません") {
			t.Errorf("no sync api: %+v", res)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		var res remoteTestResult
		c.postJSON("/admin/remote/test", url.Values{"remote_url": {dead.URL}, "remote_token": {tok}}, &res)
		if res.OK || res.RemoteStatus != 0 || !strings.Contains(res.Message, "接続できませんでした") {
			t.Errorf("unreachable: %+v", res)
		}
	})

	t.Run("redirect is not followed", func(t *testing.T) {
		redirect := httptest.NewServer(http.RedirectHandler(env.remote.srv.URL+DefaultSyncAPIPath, http.StatusMovedPermanently))
		t.Cleanup(redirect.Close)
		var res remoteTestResult
		c.postJSON("/admin/remote/test", url.Values{"remote_url": {redirect.URL}, "remote_token": {tok}}, &res)
		if res.OK || res.RemoteStatus != http.StatusMovedPermanently || !strings.Contains(res.Message, "転送") {
			t.Errorf("redirect: %+v", res)
		}
	})

	t.Run("secret sync path", func(t *testing.T) {
		secret := newSecretSyncAPIPath(t)
		hidden := newSyncEnvWith(t, Config{PublicOnly: true, SyncAPIPath: secret}, nil)
		tok := hidden.token("yamada", auth.RoleWriter)
		// 公開サーバーの URL だけでは固定の /api/sync に送り、存在しないページとして断られる。
		var res remoteTestResult
		c.postJSON("/admin/remote/test", url.Values{"remote_url": {hidden.srv.URL}, "remote_token": {tok}}, &res)
		if res.OK || res.RemoteStatus != http.StatusNotFound || !strings.Contains(res.Message, "同期 API のパス") {
			t.Errorf("server url only: %+v", res)
		}
		res = remoteTestResult{}
		c.postJSON("/admin/remote/test", url.Values{"remote_url": {hidden.srv.URL + secret}, "remote_token": {tok}}, &res)
		if !res.OK || res.RemoteStatus != http.StatusBadRequest || res.Subject != "yamada" {
			t.Errorf("sync url: %+v", res)
		}
		// 保存済みの同期 API のパスで接続テストできる（フォームの入力が空のとき）。
		if err := env.local.server.saveRemoteConfig(remoteConfig{URL: hidden.srv.URL, SyncPath: secret, Token: tok}); err != nil {
			t.Fatal(err)
		}
		res = remoteTestResult{}
		c.postJSON("/admin/remote/test", nil, &res)
		if !res.OK {
			t.Errorf("saved sync path: %+v", res)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		expired := signedToken(env.remote.priv, `{"alg":"EdDSA","typ":"sorus-cap"}`, `{"sub":"yamada","role":"writer","exp":1000,"jti":"0123456789abcdef0123456789abcdef"}`)
		var res remoteTestResult
		c.postJSON("/admin/remote/test", url.Values{"remote_url": {env.remote.srv.URL}, "remote_token": {expired}}, &res)
		if res.OK || !res.Expired || !strings.Contains(res.Message, "有効期限") {
			t.Errorf("expired: %+v", res)
		}
	})
}

func TestRemoteSubmit(t *testing.T) {
	env := newRemoteEnv(t)
	c := env.local.loggedIn(studentA)
	id := c.createArticle("写真部の展示会", "")

	// 未設定なら送らずに知らせる。
	res := c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil)
	expectStatus(t, res, http.StatusConflict)
	if !strings.Contains(jsonMessage(t, res), "未設定") {
		t.Errorf("message = %s", res.Body)
	}

	// 画像付きの本文にして提出する。
	up := c.upload("photo.png", pngBytes(t, 1600, 900), "展示の様子")
	expectStatus(t, up, http.StatusOK)
	var media struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(up.Body), &media); err != nil || media.Markdown == "" {
		t.Fatalf("upload response: %v %s", err, up.Body)
	}
	a := env.local.mustGetArticle(id)
	a.ContentMarkdown = "文化祭で展示した。\n\n" + media.Markdown
	if err := env.local.server.articles.Save(a); err != nil {
		t.Fatal(err)
	}

	env.configure("yamada", auth.RoleWriter)
	var out struct {
		Message string         `json:"message"`
		Created bool           `json:"created"`
		Article SyncArticleRef `json:"article"`
		Images  int            `json:"images"`
	}
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, &out), http.StatusOK)
	if !out.Created || out.Article.ID != id || out.Article.Status != article.StatusReview || out.Images != 4 {
		t.Errorf("submit response = %+v", out)
	}
	ra := env.remote.mustGetArticle(id)
	if ra.Status != article.StatusReview || ra.Title != "写真部の展示会" || ra.ContentMarkdown != a.ContentMarkdown {
		t.Errorf("remote article = %+v", ra)
	}
	if n := len(env.remote.uploadedFiles()); n != 4 {
		t.Errorf("remote uploads = %v, want 4 files", env.remote.uploadedFiles())
	}
	// 公開ページには出ない。
	expectSyncStatus(t, env.remote.get(articlePath(ra.Slug)), http.StatusNotFound)

	// 再提出は同じ記事の更新になる。
	a.Title = "写真部の展示会（追記）"
	if err := env.local.server.articles.Save(a); err != nil {
		t.Fatal(err)
	}
	out.Created = true
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, &out), http.StatusOK)
	if out.Created || !strings.Contains(out.Message, "更新") {
		t.Errorf("resubmit response = %+v", out)
	}
	if got := env.remote.mustGetArticle(id).Title; got != "写真部の展示会（追記）" {
		t.Errorf("remote title = %q", got)
	}

	// 執筆チーム内では非公開の記事を共有するため、他のライターの記事も提出できる。
	t.Run("writer can submit another writer's article", func(t *testing.T) {
		res := env.local.loggedIn(studentB).postJSON("/admin/remote/articles/"+id+"/submit", nil, nil)
		expectStatus(t, res, http.StatusOK)
	})

	// 管理者はライターの役割も持つため提出できる。
	t.Run("admin can submit", func(t *testing.T) {
		res := env.local.loggedIn(admin).postJSON("/admin/remote/articles/"+id+"/submit", nil, nil)
		expectStatus(t, res, http.StatusOK)
	})

	t.Run("missing article", func(t *testing.T) {
		expectStatus(t, c.postJSON("/admin/remote/articles/nope/submit", nil, nil), http.StatusNotFound)
	})

	t.Run("remote rejection is reported as bad gateway", func(t *testing.T) {
		_, otherPriv, _ := auth.GenerateKeyPair()
		forged, _ := auth.IssueToken(otherPriv, "yamada", string(auth.RoleWriter), time.Hour)
		if err := env.local.server.saveRemoteConfig(remoteConfig{URL: env.remote.srv.URL, Token: forged}); err != nil {
			t.Fatal(err)
		}
		res := c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil)
		expectStatus(t, res, http.StatusBadGateway)
		if !strings.Contains(res.Body, `"remote_status":401`) {
			t.Errorf("body = %s", res.Body)
		}
	})
}

func TestCollectRemoteImagesLimit(t *testing.T) {
	env := newTestEnv(t)
	if err := os.MkdirAll(env.uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 16)
	big := bytes.Repeat([]byte{0}, maxRemoteImageBytes/2+1)
	for _, v := range remoteImageVariants[:3] {
		if err := os.WriteFile(filepath.Join(env.uploadDir, hash+"-"+v+".webp"), big, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := env.server.collectRemoteImages(&article.Article{ContentMarkdown: "![x](/uploads/" + hash + "-1200.webp)"})
	if he, ok := err.(*echo.HTTPError); !ok || he.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("err = %v, want 413", err)
	}
	// 参照されていない画像・ローカルに無い画像は送らない。
	images, err := env.server.collectRemoteImages(&article.Article{ContentMarkdown: "![x](/uploads/" + strings.Repeat("cd", 16) + "-800.webp)"})
	if err != nil || len(images) != 0 {
		t.Errorf("images = %d, err = %v", len(images), err)
	}
}

func TestRemotePendingImportPublish(t *testing.T) {
	env := newRemoteEnv(t)
	editor := env.local.loggedIn(chief)

	// 公開サーバーに画像付きの校閲待ち記事を提出しておく。
	images := localImages(t, 1300, 700)
	ref := "/uploads/" + images[1].Name
	submitted := decodeSync[SyncSubmitResponse](t, env.remote.call(env.remote.token("tanaka", auth.RoleWriter), submitRequest(SyncArticle{
		Title: "陸上部の記録会", Slug: "track-meet", ContentMarkdown: "自己ベストが続出した。\n\n![記録会](" + ref + ")", Excerpt: "記録会の報告",
	}, images...)))
	id := submitted.Article.ID

	t.Run("writer cannot list pending", func(t *testing.T) {
		env.configure("yamada", auth.RoleWriter)
		expectStatus(t, env.local.loggedIn(studentA).getJSON("/admin/remote/pending", nil), http.StatusForbidden)
	})

	t.Run("writer token is rejected locally", func(t *testing.T) {
		env.configure("yamada", auth.RoleWriter)
		res := editor.getJSON("/admin/remote/pending", nil)
		expectStatus(t, res, http.StatusForbidden)
		if !strings.Contains(jsonMessage(t, res), "writer") {
			t.Errorf("message = %s", res.Body)
		}
	})

	env.configure("sato", auth.RoleEditor)
	var list struct {
		Count    int                 `json:"count"`
		Articles []remotePendingItem `json:"articles"`
	}
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if list.Count != 1 || len(list.Articles) != 1 {
		t.Fatalf("pending = %+v", list)
	}
	item := list.Articles[0]
	if item.ID != id || item.Title != "陸上部の記録会" || item.AuthorName != "tanaka" || item.LocalStatus != "" {
		t.Errorf("item = %+v", item)
	}
	// プレビューの画像は、公開サーバーの画像を PC を通して見せるパスを参照する（公開サーバーを直接は指さない）。
	proxied := `"` + remoteUploadProxyPrefix + images[1].Name + `"`
	if !strings.Contains(item.HTML, proxied) || strings.Contains(item.HTML, env.remote.srv.URL) || !strings.Contains(item.HTML, "自己ベスト") {
		t.Errorf("preview html = %s", item.HTML)
	}

	// 受け取る前に、取り込まずに中身を確かめられる（ローカルにまだ無いので差分は無い）。
	var preview manuscriptPreview
	expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/preview", nil, &preview), http.StatusOK)
	if preview.Title != "陸上部の記録会" || preview.AuthorName != "tanaka" || preview.Existing != nil ||
		!strings.Contains(preview.HTML, proxied) || strings.Contains(preview.HTML, env.remote.srv.URL) {
		t.Errorf("preview = %+v", preview)
	}
	if n := env.local.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, id); n != 0 {
		t.Errorf("preview created a local article")
	}

	// ローカルに取り込むと、同じ ID の校閲待ちの記事と画像ができる。
	var imported struct {
		Message      string `json:"message"`
		Created      bool   `json:"created"`
		EditURL      string `json:"edit_url"`
		ImagesSaved  int    `json:"images_saved"`
		ImagesFailed int    `json:"images_failed"`
	}
	expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/import", nil, &imported), http.StatusOK)
	if !imported.Created || imported.EditURL != "/admin/articles/"+id+"/edit" || imported.ImagesSaved != 4 || imported.ImagesFailed != 0 {
		t.Errorf("import = %+v", imported)
	}
	la := env.local.mustGetArticle(id)
	if la.Status != article.StatusReview || la.Title != "陸上部の記録会" || la.Excerpt != "記録会の報告" || la.AuthorID != chief.ID {
		t.Errorf("local article = %+v", la)
	}
	for _, img := range images {
		got, err := os.ReadFile(filepath.Join(env.local.uploadDir, img.Name))
		if err != nil || !bytes.Equal(got, img.Data) {
			t.Errorf("local image %s: %v", img.Name, err)
		}
	}
	expectStatus(t, editor.get("/admin/articles/"+id+"/edit"), http.StatusOK)

	list.Articles = nil
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if len(list.Articles) != 1 || list.Articles[0].LocalStatus != article.StatusReview {
		t.Errorf("pending after import = %+v", list.Articles)
	}

	t.Run("reimport overwrites local review article", func(t *testing.T) {
		// エディタを開いたロックを外し、取り込み直せる状態にする。
		editor.postJSON("/admin/articles/"+id+"/unlock", nil, nil)
		la.ContentMarkdown = "ローカルで書き換えた本文"
		if err := env.local.server.articles.Save(la); err != nil {
			t.Fatal(err)
		}
		revisions := env.local.countRows(`SELECT COUNT(*) FROM article_revisions WHERE article_id = ?`, id)

		// 確かめる前の取り込みは、ローカルの記事を変えずに断る。
		res := editor.postJSON("/admin/remote/pending/"+id+"/import", nil, nil)
		expectStatus(t, res, http.StatusConflict)
		if !strings.Contains(res.Body, `"need_confirm":true`) {
			t.Errorf("import without confirmation = %s", res.Body)
		}
		if got := env.local.mustGetArticle(id); got.ContentMarkdown != "ローカルで書き換えた本文" || got.Status != la.Status {
			t.Errorf("local article changed without confirmation: %+v", got)
		}
		if n := env.local.countRows(`SELECT COUNT(*) FROM article_revisions WHERE article_id = ?`, id); n != revisions {
			t.Errorf("revisions = %d, want %d", n, revisions)
		}

		// プレビューは、ローカルの記事との差分と上書きの確認を出す。
		var preview manuscriptPreview
		expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/preview", nil, &preview), http.StatusOK)
		if ex := preview.Existing; ex == nil || !ex.NeedConfirm || !ex.Changed || ex.Reason == "" {
			t.Fatalf("preview existing = %+v", preview.Existing)
		}

		imported.Created = true
		expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/import", url.Values{"overwrite": {"1"}}, &imported), http.StatusOK)
		if imported.Created || imported.ImagesSaved != 0 {
			t.Errorf("reimport = %+v", imported)
		}
		if got := env.local.mustGetArticle(id).ContentMarkdown; !strings.Contains(got, "自己ベスト") {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("import is refused while another user edits", func(t *testing.T) {
		other := env.local.loggedIn(studentB)
		expectStatus(t, other.get("/admin/articles/"+id+"/edit"), http.StatusOK)
		res := editor.postJSON("/admin/remote/pending/"+id+"/import", url.Values{"overwrite": {"1"}}, nil)
		expectStatus(t, res, http.StatusConflict)
		if !strings.Contains(jsonMessage(t, res), "編集中") {
			t.Errorf("message = %s", res.Body)
		}
		other.postJSON("/admin/articles/"+id+"/unlock", nil, nil)
	})

	t.Run("invalid id", func(t *testing.T) {
		expectStatus(t, editor.postJSON("/admin/remote/pending/bad%20id/publish", nil, nil), http.StatusBadRequest)
		expectStatus(t, editor.postJSON("/admin/remote/pending/unknown/import", nil, nil), http.StatusNotFound)
	})

	// 承認して本番公開すると、公開サーバーの公開ページに出る。
	var published struct {
		Message string         `json:"message"`
		Article SyncArticleRef `json:"article"`
		URL     string         `json:"url"`
	}
	expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/publish", nil, &published), http.StatusOK)
	if published.Article.Status != article.StatusPublished || published.URL != env.remote.srv.URL+"/articles/track-meet" {
		t.Errorf("publish = %+v", published)
	}
	expectSyncStatus(t, env.remote.get("/articles/track-meet"), http.StatusOK)

	list.Articles = nil
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if list.Count != 0 || list.Articles == nil {
		t.Errorf("pending after publish = %+v (want empty array)", list)
	}

	t.Run("publishing again is a conflict", func(t *testing.T) {
		res := editor.postJSON("/admin/remote/pending/"+id+"/publish", nil, nil)
		expectStatus(t, res, http.StatusBadGateway)
		if !strings.Contains(res.Body, `"remote_status":409`) {
			t.Errorf("body = %s", res.Body)
		}
	})
}

// 取り込みは画像のダウンロードに時間がかかる。その間に同じ ID のローカルの記事が編集され始めたり、
// 作成されたりしても、取り込みはその記事を上書きせず 409 を返す。
func TestRemoteImportRaceDuringImageDownload(t *testing.T) {
	newEnv := func(t *testing.T, onImage func(id string)) (*remoteEnv, *client, string) {
		t.Helper()
		env := newRemoteEnv(t)
		images := localImages(t, 1300, 700)
		submitted := decodeSync[SyncSubmitResponse](t, env.remote.call(env.remote.token("tanaka", auth.RoleWriter), submitRequest(SyncArticle{
			Title: "公開サーバーの版", ContentMarkdown: "![写真](/uploads/" + images[1].Name + ")",
		}, images...)))
		id := submitted.Article.ID

		// 公開サーバーへの中継。最初の画像の取得要求で onImage を呼び、取り込みの途中にローカルで起きた操作を再現する。
		var once sync.Once
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, UploadURLPrefix) {
				once.Do(func() { onImage(id) })
			}
			env.remote.srv.Config.Handler.ServeHTTP(w, r)
		}))
		t.Cleanup(proxy.Close)
		if err := env.local.server.saveRemoteConfig(remoteConfig{URL: proxy.URL, Token: env.remote.token("sato", auth.RoleEditor)}); err != nil {
			t.Fatal(err)
		}
		return env, env.local.loggedIn(chief), id
	}

	t.Run("local article locked by another user", func(t *testing.T) {
		var env *remoteEnv
		env, editor, id := newEnv(t, func(id string) {
			if _, err := env.local.server.locker.Acquire(context.Background(), id, studentB.ID); err != nil {
				t.Errorf("Acquire: %v", err)
			}
		})
		seedArticle(t, env.local.db, article.Article{ID: id, Title: "ローカルの版", ContentMarkdown: "ローカルの本文", Status: article.StatusReview})

		res := editor.postJSON("/admin/remote/pending/"+id+"/import", url.Values{"overwrite": {"1"}}, nil)
		expectStatus(t, res, http.StatusConflict)
		if msg := jsonMessage(t, res); !strings.Contains(msg, studentB.DisplayName) || !strings.Contains(msg, "編集中") {
			t.Errorf("message = %s", msg)
		}
		if a := env.local.mustGetArticle(id); a.Title != "ローカルの版" || a.ContentMarkdown != "ローカルの本文" {
			t.Errorf("local article overwritten while another user edits it: %+v", a)
		}
		info, err := env.local.server.locker.Check(context.Background(), id, studentB.ID)
		if err != nil || !info.IsOwnLock {
			t.Errorf("lock of the other user = %+v, %v, want kept", info, err)
		}
		if n := env.local.countRows(`SELECT COUNT(*) FROM article_revisions WHERE article_id = ?`, id); n != 0 {
			t.Errorf("revisions = %d, want 0 (nothing imported)", n)
		}
	})

	t.Run("local article created meanwhile", func(t *testing.T) {
		var env *remoteEnv
		env, editor, id := newEnv(t, func(id string) {
			seedArticle(t, env.local.db, article.Article{ID: id, Title: "同時に作られた版", ContentMarkdown: "別の本文", Status: article.StatusDraft})
		})

		res := editor.postJSON("/admin/remote/pending/"+id+"/import", nil, nil)
		expectStatus(t, res, http.StatusConflict)
		if a := env.local.mustGetArticle(id); a.Title != "同時に作られた版" || a.ContentMarkdown != "別の本文" || a.AuthorID != studentA.ID {
			t.Errorf("concurrently created article overwritten: %+v", a)
		}

		// 確かめてから取り込み直すと、既存の記事の更新として取り込める。
		var imported struct {
			Created bool `json:"created"`
		}
		expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/import", url.Values{"overwrite": {"1"}}, &imported), http.StatusOK)
		if a := env.local.mustGetArticle(id); imported.Created || a.Title != "公開サーバーの版" || a.Status != article.StatusReview {
			t.Errorf("reimport: created=%v article=%+v", imported.Created, a)
		}
		if info, err := env.local.server.locker.Check(context.Background(), id, chief.ID); err != nil || info.IsLocked {
			t.Errorf("lock after import = %+v, %v, want released", info, err)
		}
	})
}

func TestRemoteSettingsPush(t *testing.T) {
	env := newRemoteEnv(t)
	adm := env.local.loggedIn(admin)
	res := adm.postForm("/admin/settings/general", url.Values{
		"site_name": {"新聞部ウェブ（準備中）"}, "site_description": {"学内ニュース"}, "base_url": {"http://localhost:8080"},
		"copyright": {"(c) 新聞部"}, "analytics_tag": {""},
	})
	expectRedirect(t, res, "/admin/settings/general")

	t.Run("editor token is not enough", func(t *testing.T) {
		env.configure("sato", auth.RoleEditor)
		expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, nil), http.StatusForbidden)
	})

	t.Run("editor user cannot push", func(t *testing.T) {
		env.configure("root", auth.RoleAdmin)
		expectStatus(t, env.local.loggedIn(chief).postJSON("/admin/remote/settings/push", nil, nil), http.StatusForbidden)
	})

	env.configure("root", auth.RoleAdmin)
	// 「公開サーバーへ反映する」は、未反映の設定があるときだけ管理画面の共通バナーに出す（SPEC §2-3）。
	page := adm.get("/admin/settings/remote")
	expectStatus(t, page, http.StatusOK)
	expectNotContains(t, page.Body, `id="remote-push-settings"`)
	form := url.Values{
		"site_name": {"新聞部ウェブ"}, "site_description": {"学内ニュース"}, "base_url": {"http://localhost:8080"},
		"copyright": {"(c) 新聞部"}, "analytics_tag": {""},
	}
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general")
	page = adm.get("/admin/settings/remote")
	expectContains(t, page.Body, `id="remote-dirty-banner"`, `id="remote-push-settings"`, `data-remote-dirty-item="general"`)

	var out struct {
		Settings SyncSettingsValues `json:"settings"`
	}
	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if out.Settings.SiteName != "新聞部ウェブ" || out.Settings.Copyright != "(c) 新聞部" {
		t.Errorf("settings = %+v", out.Settings)
	}
	if got := env.remote.setting("site_description"); got != "学内ニュース" {
		t.Errorf("remote site_description = %q", got)
	}
	// ローカルの localhost の URL は公開サーバーへ送らない。
	if strings.Contains(out.Settings.BaseURL, "localhost") {
		t.Errorf("remote base_url = %q", out.Settings.BaseURL)
	}
	// 反映したら共通バナーは消える。
	expectNotContains(t, adm.get("/admin/settings/remote").Body, `id="remote-dirty-banner"`, `id="remote-push-settings"`)
}

func TestRemoteSyncView(t *testing.T) {
	env := newRemoteEnv(t)
	writer := &auth.User{ID: studentA.ID, Role: auth.RoleWriter}
	editor := &auth.User{ID: chief.ID, Role: auth.RoleEditor}
	adminUser := &auth.User{ID: admin.ID, Role: auth.RoleAdmin}

	if v := env.local.server.remoteSyncViewFor(editor); v.CanSubmit || v.CanReview {
		t.Errorf("unconfigured view = %+v", v)
	}
	env.configure("yamada", auth.RoleWriter)
	if v := env.local.server.remoteSyncViewFor(writer); !v.CanSubmit || v.CanReview {
		t.Errorf("writer token, writer user = %+v", v)
	}
	if v := env.local.server.remoteSyncViewFor(editor); v.CanSubmit || v.CanReview || v.CanPublish {
		t.Errorf("writer token, editor user = %+v", v)
	}
	// 管理者は編集者・ライターの役割も持つ。writer のトークンでは提出だけ、editor のトークンでは校閲と公開ができる。
	if v := env.local.server.remoteSyncViewFor(adminUser); !v.CanSubmit || v.CanReview || v.CanPublish {
		t.Errorf("writer token, admin user = %+v", v)
	}
	env.configure("sato", auth.RoleEditor)
	if v := env.local.server.remoteSyncViewFor(editor); v.CanSubmit || !v.CanReview || !v.CanPublish {
		t.Errorf("editor token, editor user = %+v", v)
	}
	if v := env.local.server.remoteSyncViewFor(adminUser); v.CanSubmit || !v.CanReview || !v.CanPublish {
		t.Errorf("editor token, admin user = %+v", v)
	}
}

func TestRemoteDirectPublishFromArticleEditor(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("editor", auth.RoleEditor)
	ed := env.local.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("公開サーバーへ直接公開", "remote-direct-publish", "draft")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	form := articleFormFor("公開サーバーへ直接公開・更新", "remote-direct-publish", "publish")
	res := ed.postForm("/admin/articles/"+id, form)
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=remote_published")
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished || a.Title != "公開サーバーへ直接公開・更新" {
		t.Fatalf("remote article = %+v", a)
	}
	update := articleFormFor("公開サーバーへ再更新", "remote-direct-publish", "publish")
	res = ed.postForm("/admin/articles/"+id, update)
	expectRedirect(t, res, "/admin/articles/"+id+"/edit?flash=remote_published")
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished || a.Title != "公開サーバーへ再更新" {
		t.Fatalf("updated remote article = %+v", a)
	}
	page := ed.get("/admin/articles/" + id + "/edit")
	expectNotContains(t, page.Body, "公開サーバーへ提出")

	// Writer トークンでは従来どおり提出ボタンだけを表示し、直接公開アクションは拒否される。
	env.configure("writer", auth.RoleWriter)
	writer := env.local.loggedIn(studentA)
	writerLoc := expectRedirect(t, writer.postForm("/admin/articles/new", articleFormFor("提出する原稿", "remote-writer-draft", "draft")), "/admin/articles/")
	writerID := editPathPattern.FindStringSubmatch(writerLoc)[1]
	writerPage := writer.get("/admin/articles/" + writerID + "/edit")
	expectContains(t, writerPage.Body, "原稿を下書きとして提出")
	expectNotContains(t, writerPage.Body, "data-remote-publish")
}

func TestRemoteUnpublishFromArticleEditor(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("editor", auth.RoleEditor)
	ed := env.local.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("取り下げ連携", "remote-unpublish", "publish")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished {
		t.Fatalf("remote initial status = %q", a.Status)
	}
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+id, articleFormFor("取り下げ連携", "remote-unpublish", "draft")), "/admin/articles/"+id+"/edit")
	expectFlash(t, loc, "remote_unpublished")
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusDraft {
		t.Fatalf("remote status after unpublish = %q", a.Status)
	}
	if got := env.remote.get("/articles/remote-unpublish").Code; got != http.StatusNotFound {
		t.Fatalf("remote public page status = %d, want 404", got)
	}
}

func TestRemoteUnpublishOnDelete(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("editor", auth.RoleEditor)
	ed := env.local.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("削除時の取り下げ", "remote-unpublish-delete", "publish")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+id+"/delete", nil), "/admin/articles?status=deleted")
	expectFlash(t, loc, "remote_unpublished")
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusDraft {
		t.Fatalf("remote status after local delete = %q", a.Status)
	}
}

func TestRemoteUnpublishOnDeleteScheduled(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("editor", auth.RoleEditor)
	ed := env.local.loggedIn(chief)
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("予約中の削除", "remote-unpublish-scheduled", "publish")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	// Turn both sides into a scheduled article (published_at in the future).
	future := clockNow().UTC().Add(24 * time.Hour)
	if _, err := env.local.db.Exec(`UPDATE articles SET published_at = ? WHERE id = ?`, future, id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.db.Exec(`UPDATE articles SET published_at = ? WHERE id = ?`, future, id); err != nil {
		t.Fatal(err)
	}
	loc = expectRedirect(t, ed.postForm("/admin/articles/"+id+"/delete", nil), "/admin/articles?status=deleted")
	expectFlash(t, loc, "remote_unpublished")
	if a := env.remote.mustGetArticle(id); a.Status != article.StatusDraft || a.PublishedAt != nil {
		t.Fatalf("remote scheduled article after local delete = %+v", a)
	}
}

func TestRemoteUnpublishMissingArticleAndOldServer(t *testing.T) {
	t.Run("missing article is already unpublished", func(t *testing.T) {
		env := newRemoteEnv(t)
		env.configure("editor", auth.RoleEditor)
		ed := env.local.loggedIn(chief)
		loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("公開後に削除", "remote-unpublish-missing", "publish")), "/admin/articles/")
		id := editPathPattern.FindStringSubmatch(loc)[1]
		// Remove the remote row before the local draft transition; 404 is success for unpublish.
		if _, err := env.remote.db.Exec(`DELETE FROM articles WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		loc = expectRedirect(t, ed.postForm("/admin/articles/"+id, articleFormFor("公開後に削除", "remote-unpublish-missing", "draft")), "/admin/articles/"+id+"/edit")
		expectFlash(t, loc, "remote_unpublished")
	})
	t.Run("old server failure is visible", func(t *testing.T) {
		env := newRemoteEnv(t)
		oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"unknown action"}`))
		}))
		defer oldServer.Close()
		tok := env.remote.token("editor", auth.RoleEditor)
		if err := env.local.server.saveRemoteConfig(remoteConfig{URL: oldServer.URL, SyncPath: "/api/sync", Token: tok}); err != nil {
			t.Fatal(err)
		}
		ed := env.local.loggedIn(chief)
		loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("古い公開サーバー", "remote-unpublish-old", "draft")), "/admin/articles/")
		id := editPathPattern.FindStringSubmatch(loc)[1]
		// Seed local as previously published, then return it to draft.
		a := env.local.mustGetArticle(id)
		a.Status = article.StatusPublished
		a.PublishedAt = ago(time.Hour)
		if err := env.local.server.articles.Save(a); err != nil {
			t.Fatal(err)
		}
		loc = expectRedirect(t, ed.postForm("/admin/articles/"+id, articleFormFor("古い公開サーバー", "remote-unpublish-old", "draft")), "/admin/articles/"+id+"/edit")
		expectFlash(t, loc, "remote_unpublish_failed")
	})
}

func TestRemoteSyncPartials(t *testing.T) {
	env := newTestEnv(t)
	r := env.server.echo.Renderer
	var buf bytes.Buffer
	if err := r.Render(&buf, "remote_sync_submit_button", remoteSubmitButton{ArticleID: "abc123", Lang: "ja"}, nil); err != nil {
		t.Fatal(err)
	}
	expectContains(t, buf.String(), `data-remote-submit="abc123"`, "原稿を下書きとして提出")

	buf.Reset()
	if err := r.Render(&buf, "remote_sync_submit_button", remoteSubmitButton{Lang: "ja"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "<button") {
		t.Error("submit button must not render for a new (unsaved) article")
	}

	// 一覧は予約の文言（i18n）にページの言語を使うため、ページ（page）を渡して描く。
	buf.Reset()
	if err := r.Render(&buf, "remote_pending_panel", page{Lang: "ja"}, nil); err != nil {
		t.Fatal(err)
	}
	expectContains(t, buf.String(), "data-remote-pending", "data-remote-pending-list", `id="remote-sync-modal"`, "承認して本番公開")
	buf.Reset()
	if err := r.Render(&buf, "remote_pending_panel", page{Lang: "en"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), `>Close</button>`) != 2 {
		t.Errorf("English remote sync modal has %d Close buttons, want 2", strings.Count(buf.String(), `>Close</button>`))
	}

	res := env.newClient().get("/static/js/remote_sync.js")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "/admin/remote/pending", "/admin/remote/test")
}

// デスクトップ版の利用者はすべての役割を持つが、編集者以上のトークンで参加しているときは「公開する」で直接公開するため、
// 外部ライター向けの下書き提出ボタンは出さない（提出ボタンは執筆者トークンのときだけ）。
func TestRemoteSyncViewDesktopEditorTokenPublishesDirectly(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.newClient()
	c.setupCSRF()
	expectRedirect(t, c.postForm(SetupPath, setupForm()), "/admin")
	var userID string
	if err := env.db.Get(&userID, `SELECT id FROM users LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	id := seedArticle(t, env.db, article.Article{Title: "私の記事", ContentMarkdown: "本文", Status: article.StatusDraft, AuthorID: userID})

	remote := newSyncEnv(t)
	for _, tc := range []struct {
		role       auth.Role
		wantSubmit bool
	}{
		{auth.RoleWriter, true},
		{auth.RoleEditor, false},
	} {
		cfg := remoteConfig{URL: remote.srv.URL, SyncPath: remote.server.cfg.SyncAPIPath, Token: remote.token("hanako", tc.role)}
		if err := env.server.saveRemoteConfig(cfg); err != nil {
			t.Fatal(err)
		}
		body := c.get("/admin/articles/" + id + "/edit").Body
		if got := strings.Contains(body, `data-remote-submit="`+id+`"`); got != tc.wantSubmit {
			t.Errorf("%s token: submit button shown = %v, want %v", tc.role, got, tc.wantSubmit)
		}
	}
}

func TestRemoteImport_MultilingualMetadata(t *testing.T) {
	env := newRemoteEnv(t)
	editor := env.local.loggedIn(chief)
	env.configure(chief.Username, chief.Role)

	id := "remote-en-art"
	now := time.Now().UTC()
	if _, err := env.remote.db.Exec(`INSERT INTO articles (id, title, slug, content_markdown, content_html, author_id, category_id, status, lang, translation_group_id, source_article_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, 'review', 'en', 'grp-multi-1', 'ja-source-1', ?, ?)`,
		id, "English Research Paper", "en-research", "English content", "<p>English content</p>", chief.ID, now, now); err != nil {
		t.Fatal(err)
	}

	var imported struct {
		Message string `json:"message"`
		Created bool   `json:"created"`
		EditURL string `json:"edit_url"`
	}
	expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/import", nil, &imported), http.StatusOK)
	if !imported.Created {
		t.Errorf("imported.Created = false, want true")
	}

	la := env.local.mustGetArticle(id)
	if la.Lang != "en" {
		t.Errorf("local article lang = %q, want 'en'", la.Lang)
	}
	if la.TranslationGroupID != "grp-multi-1" {
		t.Errorf("local article translation_group_id = %q, want 'grp-multi-1'", la.TranslationGroupID)
	}
	if !la.SourceArticleID.Valid || la.SourceArticleID.String != "ja-source-1" {
		t.Errorf("local article source_article_id = %+v, want valid 'ja-source-1'", la.SourceArticleID)
	}
}

// 公開サーバーに接続中でも、静的サイトの設定（/admin/publish?view=static）で保存・鍵の信頼をしたら、静的サイトの
// 設定画面に留まり（view=static を付けて戻す）、保存完了のフラッシュが出る。未接続なら従来どおり ?flash= だけで戻す。
func TestPublishStaticViewRedirectKeepsViewWhileRemoteConnected(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, t.TempDir())
	if loc := expectRedirect(t, c.postForm("/admin/publish", url.Values{
		"kind": {"folder"}, "folder_path": {t.TempDir()}, "folder_site_url": {testBaseURL},
	}), "/admin/publish"); loc != "/admin/publish?flash=publish_saved" {
		t.Fatalf("Location (not connected) = %q, want /admin/publish?flash=publish_saved", loc)
	}

	if err := env.server.saveRemoteConfig(remoteConfig{URL: "https://media.example.jp/api/sync", Token: "configured-token"}); err != nil {
		t.Fatal(err)
	}
	loc := expectRedirect(t, c.postForm("/admin/publish", url.Values{
		"kind": {"folder"}, "folder_path": {t.TempDir()}, "folder_site_url": {testBaseURL},
	}), "/admin/publish")
	if loc != "/admin/publish?view=static&flash=publish_saved" {
		t.Fatalf("Location (connected) = %q, want /admin/publish?view=static&flash=publish_saved", loc)
	}
	page := c.get(loc)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="publish-form"`, "公開先を保存しました")

	// 鍵の信頼も同じく静的サイトの設定画面へ戻す。
	st := env.server.publishState()
	st.mu.Lock()
	st.hostKey = &pendingHostKey{Addr: "127.0.0.1:2222", Known: "SHA256:old", Got: "SHA256:new"}
	st.mu.Unlock()
	if loc := expectRedirect(t, c.postForm("/admin/publish/trust-host-key", url.Values{"fingerprint": {"SHA256:new"}}),
		"/admin/publish"); loc != "/admin/publish?view=static&flash=publish_trusted" {
		t.Fatalf("Location (trust host key) = %q, want /admin/publish?view=static&flash=publish_trusted", loc)
	}

	// フラッシュの無い戻り（フォームからの「サイトに反映」など）も view=static を保つ。
	if got := env.server.publishPageURL(""); got != "/admin/publish?view=static" {
		t.Errorf("publishPageURL(\"\") while connected = %q, want /admin/publish?view=static", got)
	}
	if err := env.server.saveRemoteConfig(remoteConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := env.server.publishPageURL(""); got != "/admin/publish" {
		t.Errorf("publishPageURL(\"\") when not connected = %q, want /admin/publish", got)
	}
}

// 招待コードが壊れていても（読み取れない）、いまの接続（#remote-connection）に接続先のホスト名と、参加し直す案内を出し、
// 「接続・設定の管理」を開いて見せる。
func TestRemoteCurrentMethodShowsHostWithBrokenToken(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	if err := env.server.saveRemoteConfig(remoteConfig{URL: "https://media.example.jp/api/sync", Token: "broken-token"}); err != nil {
		t.Fatal(err)
	}
	res := c.get("/admin/settings/remote")
	expectStatus(t, res, http.StatusOK)
	conn := between(t, res.Body, `<section id="remote-connection"`, `</section>`)
	expectContains(t, conn, "media.example.jp", `id="remote-connection-error"`, "保存済みのトークンを読み取れません")
	expectContains(t, res.Body, `<details id="remote-manage" class="publish-manage" open>`, `id="publish-method-clear-remote"`)
}

// 静的サイトを設定済みなら、公開サーバーの画面のタブは英語でも「動作中」を訳して出す（日本語を混ぜない）。
func TestRemoteCurrentMethodStaticBannerEnglish(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, t.TempDir())
	c.useEnglish()
	res := c.get("/admin/settings/remote")
	expectStatus(t, res, http.StatusOK)
	tabs := between(t, res.Body, `<nav id="publish-method-tabs"`, `</nav>`)
	expectContains(t, tabs, `>Static site<span class="badge badge-static">Active</span>`, `>Publish server</a>`)
	expectNotContains(t, tabs, "動作中", "未設定", "静的サイト", "公開サーバー")
	expectNotContains(t, res.Body, "Delete settings", `id="publish-method-status"`)
}

// PC の記事のアイキャッチ画像は、提出・直接公開のどちらでも画像ファイルと URL の両方が公開サーバーへ届き、
// PC で外すと公開サーバーでも外れる（以前は画像ファイルだけを送り、URL が同期されていなかった）。
func TestRemoteSyncsFeaturedImageURL(t *testing.T) {
	env := newRemoteEnv(t)
	c := env.local.loggedIn(chief)
	id := c.createArticle("アイキャッチ付きの記事", "")
	up := c.upload("cover.png", pngBytes(t, 1200, 800), "表紙")
	expectStatus(t, up, http.StatusOK)
	var media struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(up.Body), &media); err != nil || media.URL == "" {
		t.Fatalf("upload response: %v %s", err, up.Body)
	}
	a := env.local.mustGetArticle(id)
	a.FeaturedImage = media.URL
	a.ContentMarkdown = "本文には画像なし"
	if err := env.local.server.articles.Save(a); err != nil {
		t.Fatal(err)
	}

	// 提出（Writer 相当の submit_draft）。
	env.configure("yamada", auth.RoleWriter)
	expectStatus(t, c.postJSON("/admin/remote/articles/"+id+"/submit", nil, nil), http.StatusOK)
	if got := env.remote.mustGetArticle(id).FeaturedImage; got != media.URL {
		t.Fatalf("remote featured_image_url after submit = %q, want %q", got, media.URL)
	}
	if n := len(env.remote.uploadedFiles()); n == 0 {
		t.Fatal("featured image files were not sent")
	}

	// 直接公開（publish_article）で外すと、公開サーバーでも外れる。
	env.configure("editor", auth.RoleEditor)
	a = env.local.mustGetArticle(id)
	a.FeaturedImage = ""
	if err := env.local.server.articles.Save(a); err != nil {
		t.Fatal(err)
	}
	if _, err := env.local.server.sendRemoteArticle(context.Background(), env.local.server.loadRemoteConfig(), a, remoteSendOptions{}, t.Logf); err != nil {
		t.Fatalf("sendRemoteArticle: %v", err)
	}
	ra := env.remote.mustGetArticle(id)
	if ra.Status != article.StatusPublished || ra.FeaturedImage != "" {
		t.Fatalf("remote article after clearing: status %q featured %q", ra.Status, ra.FeaturedImage)
	}

	// もう一度設定して直接公開すると、公開ページのアイキャッチにも出る。
	a.FeaturedImage = media.URL
	if err := env.local.server.articles.Save(a); err != nil {
		t.Fatal(err)
	}
	if _, err := env.local.server.sendRemoteArticle(context.Background(), env.local.server.loadRemoteConfig(), a, remoteSendOptions{}, t.Logf); err != nil {
		t.Fatalf("sendRemoteArticle: %v", err)
	}
	if got := env.remote.mustGetArticle(id).FeaturedImage; got != media.URL {
		t.Fatalf("remote featured_image_url after republish = %q, want %q", got, media.URL)
	}
}
