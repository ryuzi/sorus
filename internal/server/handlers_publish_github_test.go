package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestPublishGitHubSaveAndRenderKeepsTokenSecret(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"github"}, "github_owner": {"example-team"}, "github_repository": {"website"}, "github_branch": {"gh-pages"}, "github_directory": {"docs"}, "github_token": {"ghp-a-secret-token"}}
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	cfg := env.server.loadPublishConfig()
	if cfg.Kind != publishKindGitHub || !cfg.configured() || cfg.GitHub.Owner != "example-team" || cfg.GitHub.Repository != "website" || cfg.GitHub.Branch != "gh-pages" || cfg.GitHub.Directory != "docs" || cfg.GitHub.Token != "ghp-a-secret-token" {
		t.Fatalf("saved config = %+v", cfg)
	}
	var persisted map[string]string
	if err := json.Unmarshal([]byte(env.server.getSetting(publishSettingGitHub, "")), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["token"] != "ghp-a-secret-token" {
		t.Fatalf("persisted github settings omitted token")
	}
	page := c.get("/admin/publish").Body
	for _, want := range []string{`value="github" checked`, `value="example-team"`, `value="website"`, `value="gh-pages"`, `value="docs"`, `GitHub Pages`} {
		if !strings.Contains(page, want) {
			t.Errorf("publish page missing %q", want)
		}
	}
	if strings.Contains(page, "ghp-a-secret-token") {
		t.Error("personal access token was rendered")
	}
	if !strings.Contains(page, "https://example-team.github.io/website") {
		t.Error("GitHub Pages URL was not configured")
	}
}

func TestPublishGitHubValidationAndTokenRetention(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	expectStatus(t, c.postForm("/admin/publish", url.Values{"kind": {"github"}, "github_owner": {"bad/owner"}, "github_repository": {"site"}, "github_token": {"token"}}), http.StatusUnprocessableEntity)
	if env.server.loadPublishConfig().configured() {
		t.Fatal("invalid GitHub destination was saved")
	}
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"github"}, "github_owner": {"owner"}, "github_repository": {"site"}, "github_token": {"original-token"}}), "/admin/publish?flash=publish_saved")
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"github"}, "github_owner": {"owner"}, "github_repository": {"site"}}), "/admin/publish?flash=publish_saved")
	if got := env.server.loadPublishConfig().GitHub.Token; got != "original-token" {
		t.Fatalf("blank token save replaced saved secret: %q", got)
	}
}

func TestPublishGitHubPreservesCustomDomain(t *testing.T) {
	env := newTestEnv(t)
	const customDomain = "https://www.example.com"
	if err := env.server.saveBaseURL(customDomain); err != nil {
		t.Fatal(err)
	}
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"github"}, "github_owner": {"example-team"}, "github_repository": {"website"}, "github_branch": {"gh-pages"}, "github_token": {"token"}}
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	if got := env.server.getSetting("base_url", ""); got != customDomain {
		t.Fatalf("GitHub save overwrote custom base_url: got %q, want %q", got, customDomain)
	}
}

func TestPublishGitHubReplacesPlatformDomain(t *testing.T) {
	for _, oldURL := range []string{"https://project.pages.dev", "https://team.github.io/site"} {
		t.Run(oldURL, func(t *testing.T) {
			env := newTestEnv(t)
			if err := env.server.saveBaseURL(oldURL); err != nil {
				t.Fatal(err)
			}
			c := env.loggedIn(admin)
			form := url.Values{"kind": {"github"}, "github_owner": {"example-team"}, "github_repository": {"website"}, "github_token": {"token"}}
			expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
			if got := env.server.getSetting("base_url", ""); got == oldURL {
				t.Fatalf("platform URL was preserved: %q", got)
			}
		})
	}
}
