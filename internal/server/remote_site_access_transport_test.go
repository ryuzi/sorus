package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

// 閲覧制限の変更（set_site_access）を、sorus が TLS で直接受けた接続でだけ受け付けることと、PC の画面がそれに合わせて
// 保存のフォームを使えなくすることの結合テスト。

// get_site_access・set_site_access の応答は、この接続を sorus が TLS で直接受けたかを insecure_transport で返す。転送
// ヘッダー（X-Forwarded-Proto）は使わない。PC はこれを見て、平文で中継される公開サーバーには閲覧用のパスワードを送らない。
func TestSyncSiteAccessReportsInsecureTransport(t *testing.T) {
	plain := newPublicOnlyEnv(t, nil)
	if st := plain.getSiteAccess(plain.token("owner", auth.RoleAdmin)); !st.InsecureTransport {
		t.Errorf("get_site_access over plain http: %+v, want insecure_transport", st)
	}
	req := httptest.NewRequest(http.MethodPost, "http://media.example.jp"+plain.server.cfg.SyncAPIPath,
		strings.NewReader(mustJSON(t, SyncRequest{Action: SyncActionGetSiteAccess})))
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+plain.token("owner", auth.RoleAdmin))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set(echo.HeaderXForwardedProto, "https")
	rec := plain.serve(req)
	expectSyncStatus(t, rec, http.StatusOK)
	if st := decodeSync[SyncSiteAccessResponse](t, rec); !st.InsecureTransport {
		t.Errorf("get_site_access over plain http with X-Forwarded-Proto: https: %+v, want insecure_transport", st)
	}

	direct := newPublicOnlyTLSEnv(t, nil)
	owner := direct.token("owner", auth.RoleAdmin)
	if st := direct.getSiteAccess(owner); st.InsecureTransport {
		t.Errorf("get_site_access over direct TLS: %+v, want no insecure_transport", st)
	}
	res := expectOK(t, direct.setSiteAccess(owner, SyncSiteAccessUpdate{
		Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))
	if st := decodeResponse[SyncSiteAccessResponse](t, res); st.InsecureTransport || !st.Active {
		t.Errorf("set_site_access over direct TLS: %+v", st)
	}
	expectNotContains(t, res.Body, "insecure_transport")
}

// 公開サーバーが、この接続を TLS で直接受けていない（前段のプロキシで https を終えている）と答えたら、PC は閲覧用の
// パスワードを送らない。画面は状態を表示したうえで、保存のフォームを使えなくし、直し方（AUTOCERT_DOMAIN）を出す。
// 状態の確認では答えなかったのに、変更を公開サーバーが site_access_insecure で断ったとき（確認と変更の間に構成が
// 変わったなど）も、何も変わっていないと伝え、保存のフォームを使えなくする。
func TestRemoteSiteAccessBlockedWhenServerIsProxied(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	proxied := strings.Replace(fakeOnState, `"site_url":""`, `"site_url":"","insecure_transport":true`, 1)
	f := newFakeRemote(t, func(req SyncRequest) (int, string) {
		return http.StatusOK, strings.Replace(proxied, "%s", req.Action, 1)
	})
	configureFake(t, local, f)

	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	if st.CanSave || st.Status != siteAccessStatusChecked || st.User != "preview" || !strings.Contains(st.SaveBlocked, "AUTOCERT_DOMAIN") ||
		!strings.Contains(st.SaveBlocked, "何も変更していません") {
		t.Fatalf("status of a proxied server = %+v, want the state with saving blocked", st)
	}
	res := adm.postJSON("/admin/remote/site-access", siteAccessForm(false, "preview", "", st.Revision), nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	r := decodeSiteAccessReply(t, res)
	if !r.Blocked || r.OutcomeUnknown || r.Status != siteAccessCheckUnknown || r.State != nil || !strings.Contains(r.Message, "AUTOCERT_DOMAIN") {
		t.Fatalf("save to a proxied server = %+v, want blocked", r)
	}
	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, st.Revision), nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if got := f.requests(); strings.Contains(got, remoteAccessPass) || strings.Contains(got, SyncActionSetSiteAccess) {
		t.Fatalf("the change was sent to a server that does not receive TLS itself:\n%s", got)
	}

	// 英語の画面でも同じ理由を出す。
	adm.useEnglish()
	if st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil))); st.CanSave ||
		!strings.Contains(st.SaveBlocked, "AUTOCERT_DOMAIN") || !strings.Contains(st.SaveBlocked, "nothing was changed") {
		t.Fatalf("English status of a proxied server = %+v", st)
	}
	expectStatus(t, adm.get("/admin/set-lang?lang=ja&return=/admin"), http.StatusSeeOther)

	refusing := newFakeRemote(t, fakeStateThen(http.StatusForbidden,
		`{"code":"site_access_insecure","message":"閲覧制限は、公開サーバーの sorus が https（TLS）で直接受けた接続でだけ変更できます。"}`))
	configureFake(t, local, refusing)
	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, "r1"), nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if r := decodeSiteAccessReply(t, res); !r.Blocked || r.OutcomeUnknown || !strings.Contains(r.Message, "AUTOCERT_DOMAIN") ||
		!strings.Contains(r.Message, "何も変更していません") {
		t.Fatalf("save refused as insecure by the server = %+v, want blocked", r)
	}
}

// 前段のプロキシで https を終え、同じマシンの sorus までを http で中継する構成（nginx の proxy_pass http://127.0.0.1:PORT
// など）では、PC から閲覧制限を変更できない。PC は状態を表示し、保存のフォームを使えなくして理由を出す。閲覧用の
// パスワードはプロキシにも送らず、公開サーバーの閲覧制限は変わらない。
func TestRemoteSiteAccessBehindTLSTerminatingProxy(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil) // sorus は 127.0.0.1 の平文の http で待ち受ける
	upstream, err := url.Parse(remote.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var relayed []string
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		relayed = append(relayed, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	local := newTestEnv(t)
	cfg := remoteConfig{URL: front.URL, SyncPath: remote.server.cfg.SyncAPIPath, Token: remote.token("owner", auth.RoleAdmin)}
	if err := local.server.saveRemoteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	adm := local.loggedIn(admin)

	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	if st.CanSave || st.Status != siteAccessStatusOff || !strings.Contains(st.SaveBlocked, "AUTOCERT_DOMAIN") {
		t.Fatalf("status through a TLS-terminating proxy = %+v, want the state with saving blocked", st)
	}
	res := adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, st.Revision), nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if r := decodeSiteAccessReply(t, res); !r.Blocked || !strings.Contains(r.Message, "AUTOCERT_DOMAIN") {
		t.Fatalf("save through a TLS-terminating proxy = %+v, want blocked", r)
	}
	mu.Lock()
	got := strings.Join(relayed, "\n")
	mu.Unlock()
	if !strings.Contains(got, SyncActionGetSiteAccess) {
		t.Fatalf("the PC did not check the public server through the proxy:\n%s", got)
	}
	if strings.Contains(got, remoteAccessPass) || strings.Contains(got, SyncActionSetSiteAccess) {
		t.Fatalf("the change went through the proxy to plain http:\n%s", got)
	}
	if remote.server.basicAuthActive() {
		t.Fatal("the restriction was turned on through a plain relay")
	}
	expectStatus(t, remote.newClient().get("/"), http.StatusOK)
}
