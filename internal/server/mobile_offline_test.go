package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMobilePWAAndOfflineIntegration(t *testing.T) {
	env := newTestEnv(t)
	public := env.newClient()
	publicHome := public.get("/")
	expectStatus(t, publicHome, http.StatusOK)
	expectNotContains(t, publicHome.Body, `<link rel="manifest"`, "navigator.serviceWorker.register", `register('/sw.js')`)
	adminHome := env.loggedIn(admin).get("/admin")
	expectStatus(t, adminHome, http.StatusOK)
	expectContains(t, adminHome.Body, `<link rel="manifest" href="/manifest.json">`, "navigator.serviceWorker.register", `register('/sw.js')`)

	offline := public.get("/admin/offline")
	expectStatus(t, offline, http.StatusOK)
	expectContains(t, offline.Body,
		"<title>オフライン | sorus</title>",
		"通信が切断されています",
	)

	manifest := public.get("/manifest.json")
	expectStatus(t, manifest, http.StatusOK)
	if got := manifest.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q, want application/manifest+json", got)
	}
	var appManifest struct {
		StartURL  string `json:"start_url"`
		Shortcuts []struct {
			URL string `json:"url"`
		} `json:"shortcuts"`
	}
	if err := json.Unmarshal([]byte(manifest.Body), &appManifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if appManifest.StartURL == "" {
		t.Error("manifest start_url is empty")
	}
	if len(appManifest.Shortcuts) == 0 {
		t.Fatal("manifest has no shortcuts")
	}

	serviceWorker := public.get("/sw.js")
	expectStatus(t, serviceWorker, http.StatusOK)
	if got := serviceWorker.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
		t.Errorf("service worker Content-Type = %q, want application/javascript", got)
	}
	if got := serviceWorker.Header.Get("Service-Worker-Allowed"); got != "/" {
		t.Errorf("Service-Worker-Allowed = %q, want /", got)
	}
	if got := serviceWorker.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	expectContains(t, serviceWorker.Body, "sorus-v3", "PRECACHE_URLS", "cache.add(url).catch", "url.pathname === '/admin'", "url.pathname.startsWith('/admin/')")

	editor := env.loggedIn(chief)
	newArticle := editor.get("/admin/articles/new")
	expectStatus(t, newArticle, http.StatusOK)
	expectContains(t, newArticle.Body, `data-updated-at="0"`, `id="offline-draft-banner"`, `/static/js/offline_draft.js`)
	// 新規作成でも、端末の下書きと現在の入力の差分を確かめてから復元・破棄を選べる。
	expectContains(t, newArticle.Body, `id="offline-draft-diff-toggle"`, `id="offline-draft-diff"`, `data-msg-view-diff=`)

	location := expectRedirect(t, editor.postForm("/admin/articles/new", articleFormFor("オフライン下書き", "offline-integration-draft", "draft")), "/admin/articles/")
	id := editPathPattern.FindStringSubmatch(location)[1]
	existingArticle := editor.get("/admin/articles/" + id + "/edit")
	expectStatus(t, existingArticle, http.StatusOK)
	expectContains(t, existingArticle.Body, `id="offline-draft-banner"`, `/static/js/offline_draft.js`)
	if !strings.Contains(existingArticle.Body, `data-updated-at="`) || strings.Contains(existingArticle.Body, `data-updated-at="0"`) {
		t.Error("existing article editor should have a nonzero data-updated-at")
	}
}
