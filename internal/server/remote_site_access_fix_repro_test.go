package server

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/static"
)

// 独立レビュー（公開サーバーの閲覧制限）の指摘の再現テスト。修正前のコードでは失敗する。

// 再現（中1）: 平文 HTTP で届いた set_site_access を、クライアントが付けた転送ヘッダー（X-Forwarded-Proto など）や
// Host: localhost だけで https・この PC 内の要求とみなし、閲覧用のパスワードを受け取ってしまう。
func TestReproSiteAccessIgnoresSpoofedHTTPS(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	body, _ := json.Marshal(SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &SyncSiteAccessUpdate{
		Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}})
	newReq := func(target string, header map[string]string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, target+remote.server.cfg.SyncAPIPath, bytes.NewReader(body))
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		return req
	}
	expectRefused := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), siteAccessCodeInsecure) {
			t.Errorf("%s: status %d, want 403 %s\nbody: %s", name, rec.Code, siteAccessCodeInsecure, rec.Body.String())
		}
		if remote.server.basicAuthActive() {
			t.Fatalf("%s: a plain http request changed the restriction", name)
		}
	}

	// インターネットから平文で届いた要求（httptest.NewRequest の接続元は 192.0.2.1）。
	for name, header := range map[string]map[string]string{
		"X-Forwarded-Proto":    {"X-Forwarded-Proto": "https"},
		"X-Forwarded-Protocol": {"X-Forwarded-Protocol": "https"},
		"X-Forwarded-Ssl":      {"X-Forwarded-Ssl": "on"},
		"X-Url-Scheme":         {"X-Url-Scheme": "https"},
		"Forwarded":            {"Forwarded": "for=192.0.2.1;proto=https"},
	} {
		expectRefused(name, remote.serve(newReq("http://media.example.jp", header)))
	}
	for _, host := range []string{"http://localhost", "http://127.0.0.1:8080", "http://[::1]"} {
		expectRefused("Host "+host, remote.serve(newReq(host, nil)))
	}

	// この PC 内からの接続でも受け付けない（転送ヘッダーがあってもなくても。ヘッダーの無い中継は
	// TestReproSiteAccessRefusesPlainRelayThroughLoopback）。
	for name, header := range map[string]map[string]string{
		"X-Forwarded-For":   {"X-Forwarded-For": "203.0.113.9"},
		"X-Forwarded-Proto": {"X-Forwarded-Proto": "https"},
		"X-Real-IP":         {"X-Real-IP": "203.0.113.9"},
		"Forwarded":         {"Forwarded": "for=203.0.113.9"},
	} {
		req, err := http.NewRequest(http.MethodPost, remote.srv.URL+remote.server.cfg.SyncAPIPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res := remote.newClient().do(req)
		if res.StatusCode != http.StatusForbidden || !strings.Contains(res.Body, siteAccessCodeInsecure) {
			t.Errorf("loopback with %s: status %d, want 403\nbody: %s", name, res.StatusCode, res.Body)
		}
		if remote.server.basicAuthActive() {
			t.Fatalf("loopback with %s: a relayed request changed the restriction", name)
		}
	}

	// TLS で直接届いた要求だけを受け付ける。
	tlsRec := remote.serve(newReq("https://media.example.jp", nil))
	expectSyncStatus(t, tlsRec, http.StatusOK)
	if st := remote.getSiteAccess(owner); !st.Active {
		t.Fatal("set_site_access over TLS did not enable the restriction")
	}
}

// 再現（中2）: 公開サーバーが返したサイトの URL（site_url）が、この PC 内・私有アドレスを指していても、PC が
// そこへ接続してしまう（SSRF）。
func TestReproSiteAccessProbeStaysOnPublicAddresses(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "internal service")
	}))
	t.Cleanup(internal.Close)
	internalTLS := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	internalTLS.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			hits.Add(1)
		}
	}
	internalTLS.StartTLS()
	t.Cleanup(internalTLS.Close)
	_, plainPort, _ := net.SplitHostPort(strings.TrimPrefix(internal.URL, "http://"))
	_, tlsPort, _ := net.SplitHostPort(strings.TrimPrefix(internalTLS.URL, "https://"))

	for _, site := range []string{
		internal.URL,
		"http://localhost:" + plainPort,
		"https://localhost:" + tlsPort + "/admin",
		"https://127.0.0.1:" + tlsPort,
	} {
		if _, err := env.remote.db.Exec(`INSERT INTO settings (key, value) VALUES ('base_url', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, site); err != nil {
			t.Fatal(err)
		}
		st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
		if n := hits.Load(); n != 0 {
			t.Fatalf("site_url %s: the PC connected to an internal address (%d hits)", site, n)
		}
		found := false
		for _, c := range st.Checks {
			if strings.HasPrefix(c.URL, env.remote.srv.URL) {
				continue
			}
			found = true
			if c.Result != siteAccessCheckUnknown || c.Status != 0 || !strings.Contains(c.Label, "公開されていないアドレス") {
				t.Errorf("site_url %s: check %+v, want unknown (not a public address)", site, c)
			}
		}
		if !found {
			t.Errorf("site_url %s: checks %+v do not show that the site URL was not checked", site, st.Checks)
		}
	}
}

// 再現（中3）: トップページだけが 401 なら「保護されています」になり、CDN にキャッシュされたままの記事・フィード・
// 画像を見ていない。すべて 401 でも、確かめていない URL のキャッシュが無いとは言えない。
func TestReproSiteAccessTopPageOnlyIsNotProtection(t *testing.T) {
	const img = "0123456789abcdef0123456789abcdef-800.webp"
	remote := newPublicOnlyTLSEnv(t, func(db *sqlx.DB) {
		seedArticle(t, db, article.Article{Title: "キャッシュされた記事", Slug: "cached-news", ContentMarkdown: "本文",
			FeaturedImage: UploadURLPrefix + img, CategoryID: seedCategory(t, db, "ニュース", "news", ""),
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	if err := os.MkdirAll(remote.uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote.uploadDir, img), []byte("RIFF....WEBP"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner := remote.token("owner", auth.RoleAdmin)
	expectOK(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))

	// PC は CDN を通した URL で公開サーバーにつないでいる。CDN は有効にする前の記事・フィード・画像を保持している。
	origin, _ := url.Parse(remote.srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.Transport = remote.srv.Client().Transport // 公開サーバーは TLS で受ける
	var cached atomic.Bool
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path != "/" && cached.Load() {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("Age", "1200")
			_, _ = io.WriteString(w, "cached copy")
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(cdn.Close)
	local := newTestEnv(t)
	if err := local.server.saveRemoteConfig(remoteConfig{URL: cdn.URL, SyncPath: remote.server.cfg.SyncAPIPath, Token: owner}); err != nil {
		t.Fatal(err)
	}
	adm := local.loggedIn(admin)

	cached.Store(true)
	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	want := map[string]string{
		cdn.URL + "/":                     siteAccessCheckProtected,
		cdn.URL + "/feed.xml":             siteAccessCheckOpen,
		cdn.URL + "/articles/cached-news": siteAccessCheckOpen,
		cdn.URL + UploadURLPrefix + img:   siteAccessCheckOpen,
	}
	got := map[string]string{}
	for _, c := range st.Checks {
		got[c.URL] = c.Result
	}
	for u, r := range want {
		if got[u] != r {
			t.Errorf("cached CDN: check %s = %q, want %q (checks %+v)", u, got[u], r, st.Checks)
		}
	}
	if st.Status != siteAccessStatusExposed || st.Tone != "error" {
		t.Errorf("cached CDN: status %q tone %q, want exposed (the CDN serves cached copies)", st.Status, st.Tone)
	}

	// CDN のキャッシュが無くなり、確かめた URL がすべて 401 でも、確かめていない URL について保証しない。
	cached.Store(false)
	st = decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	if len(st.Checks) != len(want) {
		t.Fatalf("checks = %+v, want the top page, feed, latest article and an image", st.Checks)
	}
	for _, c := range st.Checks {
		if c.Result != siteAccessCheckProtected {
			t.Errorf("purged CDN: check %+v, want protected", c)
		}
	}
	if st.Status == "protected" || st.Tone == "ok" {
		t.Errorf("purged CDN: status %q tone %q, must not claim the whole site is protected", st.Status, st.Tone)
	}
	// 要約は結論の1文だけにし、確かめた URL に限った言い方にする（全体が守られているとは言わない）。確かめた範囲と CDN の
	// キャッシュの注意は、画面の「?」（remote_access_checked_scope・remote_access_cache_note）に1回だけ書く。
	if !strings.Contains(st.Summary, "確かめた URL（下の一覧）") || strings.Contains(st.Summary, "保護されています") {
		t.Errorf("purged CDN: summary %q, want it limited to the checked URLs", st.Summary)
	}
	page := adm.get(operationsPagePath).Body
	expectContains(t, page, `id="remote-site-access-scope" hidden>`+i18n.Translate("ja", "remote_access_checked_scope"),
		"ほかの記事や画像、http:// など別のアドレスで開いた場合は確かめていません", "CDN のキャッシュを削除（パージ）し、ブラウザのプライベートウィンドウで")
	// 画面のスクリプトは、確かめた範囲だけの結果を成功（緑のトースト）として出さない。
	script, err := fs.ReadFile(static.FS, "js/remote_site_access.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "'protected'") || !strings.Contains(string(script), "status === 'off'") {
		t.Error("the script still treats a partial check as success")
	}
}

// 再現（低5）: 「サイト設定」を開いたあとに、ほかの PC が set_site_access で閲覧制限を変えても、古い画面のユーザー名と
// 有効フラグで上書きしてしまう。
func TestReproGeneralSettingsDoesNotRevertSiteAccess(t *testing.T) {
	env := newSyncEnvTLS(t)
	owner := env.token("owner", auth.RoleAdmin)
	adm := env.loggedIn(admin)
	on := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, env.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})))

	page := adm.get("/admin/settings/general")
	expectStatus(t, page, http.StatusOK)
	rev := ""
	if m := regexp.MustCompile(`name="basic_auth_revision" value="([^"]*)"`).FindStringSubmatch(page.Body); m != nil {
		rev = m[1]
	}
	// 画面を開いたあとで、ほかの PC がユーザー名を変える。
	changed := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, env.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: "pc-b", ExpectedRevision: &on.Revision})))

	form := generalSettingsForm()
	form.Set("basic_auth_enabled", "1")
	form.Set("basic_auth_user", remoteAccessUser)
	form.Set("basic_auth_revision", rev)
	res := adm.postForm("/admin/settings/general", form)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale general settings save: status %d, want 409\nbody: %.1500s", res.StatusCode, res.Body)
	}
	if st := env.getSiteAccess(owner); st.User != "pc-b" || st.Revision != changed.Revision {
		t.Fatalf("stale general settings save overwrote the site access: %+v", st)
	}
}
