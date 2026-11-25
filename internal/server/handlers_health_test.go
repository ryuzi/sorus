package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"sorus/internal/search"
)

func (c *client) health() (*response, healthResponse) {
	c.t.Helper()
	res := c.get("/healthz")
	var h healthResponse
	if err := json.Unmarshal([]byte(res.Body), &h); err != nil {
		c.t.Fatalf("/healthz is not JSON (status %d): %v\n%s", res.StatusCode, err, res.Body)
	}
	return res, h
}

// waitSchedulerRunning は予約投稿ワーカーのゴルーチンが動き出すまで待つ。
func waitSchedulerRunning(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !s.scheduler.Running() {
		if time.Now().After(deadline) {
			t.Fatal("scheduler did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// expectHealthBody は /healthz の本文が {"status": want} だけ（内部の様子を表すキーを含まない）かを確かめる。
func expectHealthBody(t *testing.T, res *response, want string) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(res.Body), &raw); err != nil {
		t.Fatalf("/healthz is not JSON: %v\n%s", err, res.Body)
	}
	if len(raw) != 1 || raw["status"] != want {
		t.Fatalf("/healthz body = %s, want only {\"status\":%q}", res.Body, want)
	}
}

// 認証なしの /healthz は status だけを返し、ディスクの空き容量・稼働時間・検索インデックスや予約投稿の状態を出さない。
// データベースに接続できなければ 503 の {"status":"error"}。
func TestHealthz(t *testing.T) {
	env := newTestEnv(t)
	waitSchedulerRunning(t, env.server)
	c := env.newClient() // 認証不要。

	res, h := c.health()
	expectStatus(t, res, http.StatusOK)
	if h.Status != "ok" {
		t.Fatalf("health = %+v", h)
	}
	expectHealthBody(t, res, "ok")
	for _, k := range []string{"database", "search_index", "disk_free_bytes", "scheduler", "uptime_seconds"} {
		expectNotContains(t, res.Body, k)
	}
	if res.Header.Get("Content-Type") != "application/json" || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", res.Header)
	}

	// 予約投稿ワーカーが止まっても、配信は続けられるため 200 の ok（内部の状態は出さない）。
	env.server.stopScheduler()
	<-env.server.schedulerDone
	res, _ = c.health()
	expectStatus(t, res, http.StatusOK)
	expectHealthBody(t, res, "ok")

	// データベースに接続できなければ 503 の error。
	env.db.Close()
	res, _ = c.health()
	expectStatus(t, res, http.StatusServiceUnavailable)
	expectHealthBody(t, res, "error")
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
}

// 検索インデックスの異常も、記事の配信はできるため 200 の ok のまま（異常はログに残し、応答には出さない）。
func TestHealthzSearchIndexAndDisabledScheduler(t *testing.T) {
	idx, err := search.NewMemIndex()
	if err != nil {
		t.Fatal(err)
	}
	env := newTestEnvWith(t, Config{Search: idx, DisableScheduler: true}, nil)
	c := env.newClient()
	res, _ := c.health()
	expectStatus(t, res, http.StatusOK)
	expectHealthBody(t, res, "ok")

	idx.Close()
	res, _ = c.health()
	expectStatus(t, res, http.StatusOK)
	expectHealthBody(t, res, "ok")
}
