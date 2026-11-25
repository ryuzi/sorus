package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
)

func TestMCPPublishSite(t *testing.T) {
	t.Run("returns immediately and finishes in background", func(t *testing.T) {
		env := newTestEnv(t)
		out := configureFolderReflect(t, env, 10*time.Millisecond)

		result, err := env.server.mcpPublishSite(context.Background(), true, 0)
		if err != nil {
			t.Fatalf("mcpPublishSite(wait=0): %v", err)
		}
		if result.Status != "in_progress" || !strings.Contains(result.Message, "cms_get_publish_status") {
			t.Fatalf("mcpPublishSite(wait=0) = %+v, want background progress", result)
		}

		finished := waitReflectDone(t, env.server)
		if !finished.OK {
			t.Fatalf("background reflect = %+v", finished)
		}
		status := env.server.mcpGetPublishStatus()
		if status.Running || status.Progress != 0 || status.Last == nil || !status.Last.OK || status.Last.FinishedAt.IsZero() {
			t.Fatalf("cms_get_publish_status after reflection = %+v", status)
		}
		if _, err := os.Stat(filepath.Join(out, "index.html")); err != nil {
			t.Errorf("background reflect did not write index.html: %v", err)
		}
	})

	t.Run("waits for a successful reflect", func(t *testing.T) {
		env := newTestEnv(t)
		out := configureFolderReflect(t, env, 10*time.Millisecond)

		result, err := env.server.mcpPublishSite(context.Background(), true, 2*time.Second)
		if err != nil {
			t.Fatalf("mcpPublishSite(wait=2s): %v", err)
		}
		if result.Status != "success" {
			t.Fatalf("mcpPublishSite(wait=2s) status = %q, want success", result.Status)
		}
		if result.Uploaded == 0 && result.Unchanged == 0 {
			t.Errorf("mcpPublishSite result = %+v, want uploaded or unchanged files", result)
		}
		if _, err := os.Stat(filepath.Join(out, "index.html")); err != nil {
			t.Errorf("reflect did not write index.html: %v", err)
		}
	})

	t.Run("returns in progress after wait timeout", func(t *testing.T) {
		env := newTestEnv(t)
		configureFolderReflect(t, env, 10*time.Millisecond)
		result, err := env.server.mcpPublishSite(context.Background(), false, time.Nanosecond)
		if err != nil || result.Status != "in_progress" || !strings.Contains(result.Message, "cms_get_publish_status") {
			t.Fatalf("mcpPublishSite with short wait = %+v, err = %v", result, err)
		}
		if !waitReflectDone(t, env.server).OK {
			t.Fatal("timed-out reflection did not finish successfully in background")
		}
	})

	t.Run("returns in progress when request is canceled", func(t *testing.T) {
		env := newTestEnv(t)
		configureFolderReflect(t, env, 10*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := env.server.mcpPublishSite(ctx, false, time.Second)
		if err != nil || result.Status != "in_progress" || !strings.Contains(result.Message, "cms_get_publish_status") {
			t.Fatalf("mcpPublishSite with canceled context = %+v, err = %v", result, err)
		}
		if !waitReflectDone(t, env.server).OK {
			t.Fatal("canceled request stopped the background reflection")
		}
	})

	t.Run("rejects a second reflect", func(t *testing.T) {
		env := newTestEnv(t)
		st := env.server.publishState()
		st.mu.Lock()
		st.running = true
		st.mu.Unlock()
		defer func() {
			st.mu.Lock()
			st.running = false
			st.mu.Unlock()
		}()

		_, err := env.server.mcpPublishSite(context.Background(), true, time.Second)
		if err == nil || !strings.Contains(err.Error(), "進行中です") {
			t.Fatalf("mcpPublishSite while running error = %v, want in-progress rejection", err)
		}
	})

	t.Run("rejects public-only servers", func(t *testing.T) {
		env := newTestEnvWith(t, Config{PublicOnly: true}, nil)
		_, err := env.server.mcpPublishSite(context.Background(), true, time.Second)
		if err == nil {
			t.Fatal("mcpPublishSite on public-only server succeeded, want rejection")
		}
	})
}

func TestMCPPublishSiteFailureReturnsReason(t *testing.T) {
	env := newTestEnv(t)
	// Editor は拒絶される（[M-2] Admin 認可徹底）
	editorToken := issueMCPToken(env.loggedIn(chief), "editor publish", "30")
	_, editorResponse := env.mcpCall(editorToken, "tools/call", map[string]any{"name": "cms_publish_site", "arguments": map[string]any{"wait_seconds": 20}})
	editorResult := fmt.Sprint(editorResponse["result"])
	if !strings.Contains(editorResult, "isError:true") || !strings.Contains(editorResult, "管理者のみ") {
		t.Fatalf("cms_publish_site with editor response = %v, want admin only error", editorResponse)
	}

	token := issueMCPToken(env.loggedIn(admin), "publish failure", "30")
	_, response := env.mcpCall(token, "tools/call", map[string]any{"name": "cms_publish_site", "arguments": map[string]any{"wait_seconds": 20}})
	result := fmt.Sprint(response["result"])
	if !strings.Contains(result, "isError:true") || !strings.Contains(result, "公開先がまだ決まっていません") {
		t.Fatalf("cms_publish_site response = %v, want the configured destination reason", response)
	}
}

func TestMCPPublishStatusHTTPReportsBackgroundCompletion(t *testing.T) {
	env := newTestEnv(t)
	configureFolderReflect(t, env, 10*time.Millisecond)
	token := issueMCPToken(env.loggedIn(admin), "publish status", "30")
	_, publishResponse := env.mcpCall(token, "tools/call", map[string]any{"name": "cms_publish_site", "arguments": map[string]any{"wait_seconds": 0}})
	if !strings.Contains(fmt.Sprint(publishResponse["result"]), "cms_get_publish_status") {
		t.Fatalf("cms_publish_site response = %v, want status-tool guidance", publishResponse)
	}
	if !waitReflectDone(t, env.server).OK {
		t.Fatal("background reflection failed")
	}
	_, statusResponse := env.mcpCall(token, "tools/call", map[string]any{"name": "cms_get_publish_status", "arguments": map[string]any{}})
	status := fmt.Sprint(statusResponse["result"])
	if !strings.Contains(status, "progress:0") {
		t.Fatalf("cms_get_publish_status response = %v, want progress 0 when stopped", statusResponse)
	}
	if strings.Contains(status, "isError:true") || !strings.Contains(status, "running:false") || !strings.Contains(status, "finished_at") {
		t.Fatalf("cms_get_publish_status response = %v, want completed last result", statusResponse)
	}
}

func TestMCPCheckPublishHealth(t *testing.T) {
	t.Run("reports an unset public URL", func(t *testing.T) {
		env := newTestEnv(t)
		result := env.server.mcpCheckPublishHealth(context.Background())
		if result.Healthy || len(result.Issues) == 0 {
			t.Fatalf("mcpCheckPublishHealth without URL = %+v, want an unhealthy result with an issue", result)
		}
	})

	t.Run("maps a healthy site and sitemap", func(t *testing.T) {
		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/sitemap.xml" {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte("<urlset></urlset>"))
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><title>site</title>"))
		}))
		defer site.Close()

		// Supplying a client skips the public-address restriction for this local test server.
		status := runPublishHealthCheck(context.Background(), site.URL, site.Client())
		if status.State != "ok" || status.HTTPStatus != http.StatusOK || strings.Contains(status.Message, "サイトマップ") {
			t.Fatalf("runPublishHealthCheck for healthy test site = %+v", status)
		}
	})

	t.Run("reports a missing page", func(t *testing.T) {
		site := httptest.NewServer(http.NotFoundHandler())
		defer site.Close()

		status := runPublishHealthCheck(context.Background(), site.URL, site.Client())
		if status.State != "error" || status.HTTPStatus != http.StatusNotFound {
			t.Fatalf("runPublishHealthCheck for 404 test site = %+v, want error and 404", status)
		}
	})
}

func TestMapPublishHealthResult(t *testing.T) {
	t.Run("healthy with sitemap and TLS", func(t *testing.T) {
		checked := time.Now().UTC()
		result := mapPublishHealthResult(publishHealthStatus{State: "ok", CheckedAt: checked, HTTPStatus: http.StatusOK,
			LatencyMS: 12, TLSExpiresAt: checked.Add(72 * time.Hour)})
		if !result.Healthy || result.StatusCode != http.StatusOK || !result.SitemapFound || result.ResponseTimeMS != 12 || result.TLSDaysRemaining < 2 {
			t.Fatalf("mapped healthy result = %+v", result)
		}
	})
	t.Run("unhealthy and missing sitemap", func(t *testing.T) {
		result := mapPublishHealthResult(publishHealthStatus{State: "error", CheckedAt: time.Now(), HTTPStatus: http.StatusNotFound,
			Message: "公開サイトを確認できませんでした。", AdditionalMessageKeys: []string{"publish_health_sitemap_missing"}})
		if result.Healthy || result.SitemapFound || result.StatusCode != http.StatusNotFound || len(result.Issues) != 2 {
			t.Fatalf("mapped unhealthy result = %+v", result)
		}
	})
}

func TestMCPApproveAndPublishReflectsArticle(t *testing.T) {
	env := newTestEnv(t)
	out := configureFolderReflect(t, env, 10*time.Millisecond)

	articleID := seedArticle(t, env.db, article.Article{Title: "MCP承認公開", Slug: "mcp-approval-publish",
		ContentMarkdown: "承認後に公開される本文", AuthorID: studentA.ID, Status: article.StatusReview})
	token := issueMCPToken(env.loggedIn(chief), "テスト用", "30")
	_, response := env.mcpCall(token, "tools/call", map[string]any{"name": "cms_approve_and_publish", "arguments": map[string]any{
		"article_id": articleID, "comment": "確認しました",
	}})
	if response["error"] != nil {
		t.Fatalf("cms_approve_and_publish returned an MCP error: %v", response["error"])
	}
	if got := env.mustGetArticle(articleID); got.Status != article.StatusPublished {
		t.Fatalf("approved article status = %q, want published", got.Status)
	}

	reflected := waitReflectDone(t, env.server)
	if !reflected.OK || reflected.Trigger != publishTriggerArticlePublish {
		t.Fatalf("reflect after MCP approval = %+v, want a successful article-publish reflect", reflected)
	}
	if _, err := os.Stat(filepath.Join(out, "articles", "mcp-approval-publish", "index.html")); err != nil {
		t.Errorf("approved article was not reflected to folder: %v", err)
	}
}

func TestMCPApproveAndPublishWithoutDestinationLeavesNoReflectRecord(t *testing.T) {
	env := newTestEnv(t)
	articleID := seedArticle(t, env.db, article.Article{Title: "公開先なし", Slug: "no-destination",
		ContentMarkdown: "本文", AuthorID: studentA.ID, Status: article.StatusReview})
	token := issueMCPToken(env.loggedIn(chief), "no destination", "30")
	_, response := env.mcpCall(token, "tools/call", map[string]any{"name": "cms_approve_and_publish", "arguments": map[string]any{"article_id": articleID}})
	if response["error"] != nil || strings.Contains(fmt.Sprint(response["result"]), "isError:true") {
		t.Fatalf("cms_approve_and_publish without destination = %v", response)
	}
	if got := env.mustGetArticle(articleID); got.Status != article.StatusPublished {
		t.Fatalf("status = %q, want published", got.Status)
	}
	st := env.server.publishState()
	st.mu.Lock()
	running, pending, last := st.running, st.timer != nil, st.last
	st.mu.Unlock()
	if running || pending || last != nil || env.server.loadLastOutcome() != nil {
		t.Fatalf("reflect record left: running=%v pending=%v last=%+v", running, pending, last)
	}
}

func TestMCPPublishSitePurgeCDNFalseSkipsPurge(t *testing.T) {
	for _, tc := range []struct {
		purge     bool
		wantCalls int
	}{{false, 0}, {true, 1}} {
		t.Run(fmt.Sprintf("purge=%v", tc.purge), func(t *testing.T) {
			env := newTestEnv(t)
			configureFolderReflect(t, env, 10*time.Millisecond)
			for key, value := range map[string]string{"cdn_purge_enabled": "true", "cdn_cf_zone_id": "zone-123", "cdn_cf_api_token": "token-secret"} {
				if err := upsertSetting(env.db, key, value); err != nil {
					t.Fatal(err)
				}
			}
			oldPurge := purgeCDNCache
			t.Cleanup(func() { purgeCDNCache = oldPurge })
			calls := 0
			purgeCDNCache = func(context.Context, *http.Client, string, string, []string) error {
				calls++
				return nil
			}
			result, err := env.server.mcpPublishSite(context.Background(), tc.purge, 5*time.Second)
			if err != nil || result.Status != "success" {
				t.Fatalf("purge=%v: result = %+v, err = %v", tc.purge, result, err)
			}
			if calls != tc.wantCalls || result.CDNPurged != tc.purge {
				t.Fatalf("purge=%v: calls = %d, CDNPurged = %v", tc.purge, calls, result.CDNPurged)
			}
			if len(result.Details) == 0 {
				t.Errorf("purge=%v: details are empty", tc.purge)
			}
		})
	}
}
