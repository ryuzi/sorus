package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/auth"
)

// syncProxy は PC と公開サーバーのあいだに置く、同期 API の要求を数えるプロキシ。block が true を返した要求には、
// 公開サーバーへ渡さずに 429 と Retry-After（retryAfter）を返す。stripAttachments なら publish_article の応答から
// 添付資料の数（attachments）を消す（数を返さない古い公開サーバーのふり）。
type syncProxy struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []syncProxyRequest
	// block は action と、その action の何回目の要求か（1 から）を受け取り、429 を返すなら true と Retry-After の値を返す。
	block            func(action string, n int) (string, bool)
	stripAttachments bool
}

// syncProxyRequest は公開サーバーへ届いた（または 429 を返した）要求1件。
type syncProxyRequest struct {
	Action    string
	ArticleID string
	Limited   bool
}

func newSyncProxy(t *testing.T, remote *syncEnv) *syncProxy {
	t.Helper()
	target, err := url.Parse(remote.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &syncProxy{}
	forward := httputil.NewSingleHostReverseProxy(target)
	forward.ModifyResponse = func(res *http.Response) error {
		p.mu.Lock()
		strip := p.stripAttachments
		p.mu.Unlock()
		if !strip || res.StatusCode != http.StatusOK {
			return nil
		}
		data, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			return err
		}
		var body map[string]any
		if json.Unmarshal(data, &body) == nil && body["action"] == SyncActionPublishArticle {
			delete(body, "attachments")
			data, _ = json.Marshal(body)
		}
		res.Body = io.NopCloser(bytes.NewReader(data))
		res.ContentLength = int64(len(data))
		res.Header.Del("Content-Length")
		return nil
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req SyncRequest
		_ = json.Unmarshal(data, &req)
		p.mu.Lock()
		n := 1
		for _, prev := range p.requests {
			if prev.Action == req.Action {
				n++
			}
		}
		retryAfter, limited := "", false
		if p.block != nil {
			retryAfter, limited = p.block(req.Action, n)
		}
		p.requests = append(p.requests, syncProxyRequest{Action: req.Action, ArticleID: req.ArticleID, Limited: limited})
		p.mu.Unlock()
		if limited {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"要求が多すぎます。しばらく待ってから、もう一度試してください"}`))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		// 応答を書き換えられるよう、圧縮させない。
		r.Header.Del("Accept-Encoding")
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// configureVia は PC の公開サーバーの接続先を、このプロキシ経由にする。
func (p *syncProxy) configureVia(e *remoteEnv, sub string, role auth.Role) {
	e.local.t.Helper()
	cfg := remoteConfig{URL: p.srv.URL, SyncPath: e.remote.server.cfg.SyncAPIPath, Token: e.remote.token(sub, role)}
	if err := e.local.server.saveRemoteConfig(cfg); err != nil {
		e.local.t.Fatal(err)
	}
}

// count は action の要求の数（articleID が空でなければその記事の分だけ。429 を返したものを含む）を返す。
func (p *syncProxy) count(action, articleID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.requests {
		if r.Action == action && (articleID == "" || r.ArticleID == articleID) {
			n++
		}
	}
	return n
}

func (p *syncProxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = nil
}

// recordRemoteRetrySleeps は送り直しの前の待ちを、実際には待たずに記録するものに差し替え、記録を返す。
func recordRemoteRetrySleeps(t *testing.T) func() []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	orig := remoteRetrySleep
	remoteRetrySleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { remoteRetrySleep = orig })
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), waits...)
	}
}

// 一括同期は、PC にも公開サーバーにも添付資料が無い記事には sync_attachments を送らない（数百件の一括同期で要求の数が
// 倍になり、公開サーバーの受付上限に当たらないように）。添付資料のある記事には今までどおり送る。
func TestRemoteBulkSyncSkipsAttachmentsWhenNone(t *testing.T) {
	env := newRemoteEnv(t)
	ed := env.local.loggedIn(chief)
	adm := env.local.loggedIn(admin)
	var plain []string
	for _, slug := range []string{"plain-1", "plain-2", "plain-3"} {
		loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("添付の無い記事 "+slug, slug, "publish")), "/admin/articles/")
		plain = append(plain, editPathPattern.FindStringSubmatch(loc)[1])
	}
	writer := env.local.loggedIn(studentA)
	pdfID := writer.createArticle("資料のある記事", "資料をご覧ください。")
	writer.mustAttach(pdfID, "資料.pdf", pdfData)
	expectStatus(t, writer.postAPI("/admin/articles/"+pdfID+"/unlock", "", nil), http.StatusNoContent)
	env.local.publish(pdfID, "with-pdf")

	proxy := newSyncProxy(t, env.remote)
	proxy.configureVia(env, "editor", auth.RoleEditor)
	expectStatus(t, adm.postJSON("/admin/remote/bulk-sync", nil, nil), http.StatusAccepted)
	st := waitRemoteBulk(t, adm)
	if st.Outcome != remoteBulkOK || st.Articles != 4 || st.Attachments != 1 || len(st.Failures) != 0 {
		t.Fatalf("finished = %+v", st)
	}
	for _, id := range plain {
		if n := proxy.count(SyncActionSyncAttachments, id); n != 0 {
			t.Errorf("sync_attachments for article %s without attachments sent %d time(s), want 0", id, n)
		}
		if n := proxy.count(SyncActionPublishArticle, id); n != 1 {
			t.Errorf("publish_article for %s sent %d time(s), want 1", id, n)
		}
	}
	if n := proxy.count(SyncActionSyncAttachments, pdfID); n < 1 {
		t.Errorf("sync_attachments for the article with a PDF sent %d time(s), want at least 1", n)
	}
	if n := env.remote.countRows(`SELECT COUNT(*) FROM attachments WHERE article_id = ?`, pdfID); n != 1 {
		t.Fatalf("remote attachments = %d, want 1", n)
	}

	// PC で添付資料をすべて消した記事は、公開サーバーにまだ残っているため sync_attachments を送り、公開サーバーからも消す。
	var attID string
	if err := env.local.db.Get(&attID, `SELECT id FROM attachments WHERE article_id = ?`, pdfID); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, ed.request(http.MethodDelete, "/admin/articles/"+pdfID+"/attachments/"+attID, nil, nil), http.StatusNoContent)
	expectStatus(t, ed.postAPI("/admin/articles/"+pdfID+"/unlock", "", nil), http.StatusNoContent)
	proxy.reset()
	expectStatus(t, adm.postJSON("/admin/remote/bulk-sync", nil, nil), http.StatusAccepted)
	if st := waitRemoteBulk(t, adm); st.Outcome != remoteBulkOK {
		t.Fatalf("second bulk sync = %+v", st)
	}
	if n := proxy.count(SyncActionSyncAttachments, pdfID); n != 1 {
		t.Errorf("sync_attachments after deleting the last attachment sent %d time(s), want 1 (to prune it on the server)", n)
	}
	if n := env.remote.countRows(`SELECT COUNT(*) FROM attachments WHERE article_id = ?`, pdfID); n != 0 {
		t.Errorf("remote attachments after pruning = %d, want 0", n)
	}
	if n := proxy.count(SyncActionSyncAttachments, ""); n != 1 {
		t.Errorf("sync_attachments requests in the second run = %d, want only the pruning one", n)
	}

	// 添付資料の数を返さない古い公開サーバーには、数が分からないため今までどおり送る（消し忘れを残さない）。
	proxy.mu.Lock()
	proxy.stripAttachments = true
	proxy.mu.Unlock()
	proxy.reset()
	expectStatus(t, adm.postJSON("/admin/remote/bulk-sync", nil, nil), http.StatusAccepted)
	if st := waitRemoteBulk(t, adm); st.Outcome != remoteBulkOK {
		t.Fatalf("bulk sync with an old server = %+v", st)
	}
	if n := proxy.count(SyncActionSyncAttachments, ""); n != 4 {
		t.Errorf("sync_attachments to a server that does not report counts = %d, want 4 (one per article)", n)
	}
}

// 公開サーバーが受付上限（429 Too Many Requests）を返したら、Retry-After の時間だけ待って同じ要求を送り直し、一括同期を
// 最後まで続ける（途中の記事を失敗にしない）。待っているあいだは、進み具合に待ち時間を出す。
func TestRemoteBulkSyncRetriesAfterTooManyRequests(t *testing.T) {
	env := newRemoteEnv(t)
	ed := env.local.loggedIn(chief)
	adm := env.local.loggedIn(admin)
	var ids []string
	for _, slug := range []string{"rate-1", "rate-2", "rate-3", "rate-4", "rate-5"} {
		loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("受付上限の記事 "+slug, slug, "publish")), "/admin/articles/")
		ids = append(ids, editPathPattern.FindStringSubmatch(loc)[1])
	}
	waits := recordRemoteRetrySleeps(t)
	var sawWaiting sync.Once
	waitingSeen := make(chan int, 1)
	proxy := newSyncProxy(t, env.remote)
	// 3件目と4件目の公開で受付上限に当たる（3件目は2回続けて、Retry-After は秒数。4件目は Retry-After 無し）。
	proxy.block = func(action string, n int) (string, bool) {
		if action != SyncActionPublishArticle {
			return "", false
		}
		switch n {
		case 3, 4:
			return "7", true
		case 6:
			return "", true
		}
		return "", false
	}
	proxy.configureVia(env, "editor", auth.RoleEditor)
	orig := remoteRetrySleep
	remoteRetrySleep = func(ctx context.Context, d time.Duration) error {
		sawWaiting.Do(func() { waitingSeen <- env.local.server.remoteBulk.snapshot().Waiting })
		return orig(ctx, d)
	}

	expectStatus(t, adm.postJSON("/admin/remote/bulk-sync", nil, nil), http.StatusAccepted)
	st := waitRemoteBulk(t, adm)
	if st.Outcome != remoteBulkOK || st.Articles != 5 || st.Done != 5 || len(st.Failures) != 0 || st.Waiting != 0 {
		t.Fatalf("finished = %+v, want all 5 articles sent after waiting", st)
	}
	for _, id := range ids {
		if n := env.remote.countRows(`SELECT COUNT(*) FROM articles WHERE id = ? AND status = 'published'`, id); n != 1 {
			t.Errorf("remote article %s not published", id)
		}
	}
	got := waits()
	want := []time.Duration{7 * time.Second, 7 * time.Second, remoteRateLimitBackoff}
	if len(got) != len(want) {
		t.Fatalf("retry waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("retry wait #%d = %v, want %v", i+1, got[i], want[i])
		}
	}
	if n := proxy.count(SyncActionPublishArticle, ""); n != 8 {
		t.Errorf("publish_article requests = %d, want 8 (5 articles + 3 retries)", n)
	}
	select {
	case w := <-waitingSeen:
		if w != 7 {
			t.Errorf("bulk status while waiting = %d s, want 7", w)
		}
	default:
		t.Error("the bulk status was never read while waiting")
	}
}

// 受付上限で待つ時間が長すぎる（認証の失敗が多すぎて締め出された など）ときと、送り直しても受け付けられないときは、
// 待たずに・送り直しの上限で失敗として知らせる。画面の操作からの要求は短い待ちしか送り直さない。
func TestCallRemoteRetryLimits(t *testing.T) {
	var calls int
	retryAfter := "600"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	waits := recordRemoteRetrySleeps(t)
	cfg := remoteConfig{URL: srv.URL, Token: "token"}

	// Retry-After が長すぎる: 一括同期でも待たない。
	err := callRemote(withRemotePatience(context.Background(), nil), cfg, SyncRequest{Action: SyncActionServerInfo}, nil)
	var re *remoteError
	if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || calls != 1 || len(waits()) != 0 {
		t.Fatalf("long Retry-After: err=%v calls=%d waits=%v, want one call and no wait", err, calls, waits())
	}
	if !strings.Contains(re.Message, "混み合って") {
		t.Errorf("message = %q", re.Message)
	}

	// 画面の操作（既定）: 30秒の待ちは長すぎるため送り直さない。5秒なら2回まで送り直す。
	calls, retryAfter = 0, "30"
	_ = callRemote(context.Background(), cfg, SyncRequest{Action: SyncActionServerInfo}, nil)
	if calls != 1 {
		t.Errorf("interactive call with Retry-After 30: calls = %d, want 1", calls)
	}
	calls, retryAfter = 0, "5"
	_ = callRemote(context.Background(), cfg, SyncRequest{Action: SyncActionServerInfo}, nil)
	if calls != 1+remoteRetryDefault.Retries {
		t.Errorf("interactive call with Retry-After 5: calls = %d, want %d", calls, 1+remoteRetryDefault.Retries)
	}

	// 一括同期: 送り直しの上限まで送っても受け付けられなければ失敗にする。
	calls = 0
	err = callRemote(withRemotePatience(context.Background(), nil), cfg, SyncRequest{Action: SyncActionServerInfo}, nil)
	if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || calls != 1+remoteRetryPatient.Retries {
		t.Errorf("patient call: err=%v calls=%d, want %d calls", err, calls, 1+remoteRetryPatient.Retries)
	}
}

// Retry-After は秒数と HTTP の日時の両方を読み、無い・読めないときは倍々に延ばす。
func TestRemoteRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	for _, tc := range []struct {
		name    string
		header  http.Header
		attempt int
		want    time.Duration
	}{
		{"seconds", h("42"), 0, 42 * time.Second},
		{"zero", h("0"), 3, 0},
		{"http date", h(now.Add(90 * time.Second).Format(http.TimeFormat)), 0, 90 * time.Second},
		{"past date", h(now.Add(-time.Minute).Format(http.TimeFormat)), 0, 0},
		{"missing", http.Header{}, 0, remoteRateLimitBackoff},
		{"missing third", http.Header{}, 2, 4 * remoteRateLimitBackoff},
		{"garbage", h("soon"), 1, 2 * remoteRateLimitBackoff},
	} {
		if got := remoteRetryAfter(tc.header, tc.attempt, now); got != tc.want {
			t.Errorf("%s: remoteRetryAfter = %v, want %v", tc.name, got, tc.want)
		}
	}
}
