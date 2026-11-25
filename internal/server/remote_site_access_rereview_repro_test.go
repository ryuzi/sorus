package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

// 再レビュー（公開サーバーの閲覧制限、2巡目）の指摘の再現テスト。修正前のコードでは失敗する。

// setSiteAccessOverTLS は、TLS で直接届いた要求として（httptest.NewRequest は https の URL なら TLS の接続状態を
// 付ける）set_site_access をサーバーの Handler に渡す。閲覧制限を有効にしてから確かめる再現テストの準備に使う。
func setSiteAccessOverTLS(t *testing.T, e *syncEnv, token string, upd SyncSiteAccessUpdate) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://media.example.jp"+e.server.cfg.SyncAPIPath,
		strings.NewReader(mustJSON(t, SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &upd})))
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return e.serve(req)
}

// 再現（中1）: 同じマシンのリバースプロキシが平文で中継した set_site_access を、この PC 内からの直接の要求とみなし、
// 閲覧用のパスワードを受け取ってしまう。既定の nginx（proxy_pass http://127.0.0.1:PORT;）と同じく、プロキシは
// 上流の 127.0.0.1:PORT へ接続し、Host を上流のアドレスにして、転送ヘッダーを付けない。sorus から見ると、接続元・
// 接続先・Host がすべてこの PC 内で、直接の要求と見分けられない（sorus を 127.0.0.1 だけで待ち受けても同じ）。
func TestReproSiteAccessRefusesPlainRelayThroughLoopback(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil) // sorus は 127.0.0.1 の平文の http で待ち受ける
	owner := remote.token("owner", auth.RoleAdmin)
	body := mustJSON(t, SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &SyncSiteAccessUpdate{
		Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}})
	post := func(c *http.Client, base, host string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+remote.server.cfg.SyncAPIPath, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		res, err := c.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", base, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	expectRefused := func(what string, status int, resBody string) {
		t.Helper()
		if status != http.StatusForbidden || !strings.Contains(resBody, siteAccessCodeInsecure) {
			t.Errorf("%s: status %d, want 403 %s\nbody: %.500s", what, status, siteAccessCodeInsecure, resBody)
		}
		if remote.server.basicAuthActive() {
			t.Fatalf("%s: a plain http request changed the restriction", what)
		}
	}

	// 前段のプロキシで TLS を終え、sorus までは同じマシンの中を平文でつなぐ（転送ヘッダーなし、Host は上流）。
	upstream, err := url.Parse(remote.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewTLSServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }})
	t.Cleanup(front.Close)
	status, resBody := post(front.Client(), front.URL, "")
	expectRefused("relayed by a same-machine reverse proxy", status, resBody)

	// 中継の印が無い、この PC 内からの平文の接続。プロキシの中継と見分けられないため、これも受け付けない。
	_, port, _ := net.SplitHostPort(upstream.Host)
	for _, host := range []string{"", "localhost", "localhost:" + port, "[::1]:" + port} {
		status, resBody := post(http.DefaultClient, remote.srv.URL, host)
		expectRefused("plain loopback with Host "+host, status, resBody)
	}

	// sorus が TLS を直接受けた要求（本番の AUTOCERT_DOMAIN と同じ）は受け付ける。
	direct := httptest.NewTLSServer(remote.server.Handler())
	t.Cleanup(direct.Close)
	if status, resBody := post(direct.Client(), direct.URL, ""); status != http.StatusOK {
		t.Fatalf("direct TLS: status %d, want 200\nbody: %.500s", status, resBody)
	}
	if !remote.server.basicAuthActive() {
		t.Fatal("set_site_access over direct TLS did not enable the restriction")
	}
}

// 再現（中1、PC 側）: 公開サーバーの URL がこの PC 内の平文の http（http://127.0.0.1:PORT）だと、閲覧用のパスワードを
// 平文で送ってしまう。この PC 内の http も、間にプロキシやトンネルがあれば暗号化されない。公開サーバーも平文では
// 受け付けないため、PC は https の公開サーバーにだけ送る。
func TestReproRemoteSiteAccessKeepsPasswordOffPlainHTTP(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	f := newPlainFakeRemote(t, func(req SyncRequest) (int, string) {
		return http.StatusOK, strings.Replace(fakeOnState, "%s", req.Action, 1)
	})
	configureFake(t, local, f) // http://127.0.0.1:PORT（この PC 内）
	res := adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, "r1"), nil)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("save to a plain http public server: status %d, want 422\nbody: %.500s", res.StatusCode, res.Body)
	}
	if !strings.Contains(res.Body, "https://") {
		t.Errorf("save to a plain http public server: reply %.500s does not ask for https://", res.Body)
	}
	if got := f.requests(); strings.Contains(got, remoteAccessPass) {
		t.Fatalf("the viewing password was sent over plain http:\n%s", got)
	}
}

// 再現（低2）: 公開先のヘルスチェックと、公開サーバーが返したサイトの URL の確認が「公開されたアドレス」とする範囲
// （publicIP）が、テーマの取り込みの SSRF の判定（internal/theme/ssrf.go）より広い。NAT64 に埋め込んだループバック・
// メタデータ・私有の IPv4、6to4、0.0.0.0/8 などに解決される名前へ、ピン留めしたまま接続してしまう。
func TestReproPublicAddressRulesMatchThemeSSRF(t *testing.T) {
	ctx := context.Background()
	var dialed []string
	d := publicDialer{dial: func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, errors.New("test: no connection")
	}}
	for _, s := range []string{
		// NAT64（64:ff9b::/96）に埋め込んだ 127.0.0.1・169.254.169.254（メタデータ）・10.0.0.1・192.168.1.1
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::a00:1", "64:ff9b::c0a8:101",
		// ローカル NAT64（RFC 8215）
		"64:ff9b:1::1",
		// 6to4 に埋め込んだ 127.0.0.1・192.168.1.1、Teredo
		"2002:7f00:1::", "2002:c0a8:101::1", "2001::1",
		// このネットワーク（0.0.0.0/8）、予約・ブロードキャスト
		"0.0.0.1", "0.255.255.255", "240.0.0.1", "255.255.255.255",
		// マルチキャスト、廃止されたサイトローカル、破棄用（100::/64）
		"224.0.0.1", "fec0::1", "100::1", "ff02::1",
	} {
		ip := net.ParseIP(s)
		if publicIP(ip) {
			t.Errorf("publicIP(%s) = true, want false", s)
		}
		// 名前がこのアドレスに解決された場合（公開サーバーが返したサイトの URL など）も、どこにも接続しない。
		d.lookup = func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: ip}}, nil }
		if _, err := d.DialContext(ctx, "tcp", "site.example:443"); !errors.Is(err, errNonPublicTarget) {
			t.Errorf("dial a name resolved to %s: err = %v, want errNonPublicTarget", s, err)
		}
		// 公開先のヘルスチェックも、接続する前に公開されていないアドレスとして断る。
		u := &url.URL{Scheme: "https", Host: net.JoinHostPort(s, "443"), Path: "/"}
		if err := publicHealthTarget(ctx, u); !errors.Is(err, errNonPublicTarget) {
			t.Errorf("publish health target %s: err = %v, want errNonPublicTarget", u, err)
		}
	}
	if len(dialed) != 0 {
		t.Fatalf("non-public addresses were dialed: %v", dialed)
	}
	// インターネットに公開されたアドレス（DNS64 が作る NAT64 の公開 IPv4 を含む）には今までどおり接続する。
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "2606:4700::1111", "64:ff9b::808:808"} {
		if !publicIP(net.ParseIP(s)) {
			t.Errorf("publicIP(%s) = false, want true", s)
		}
	}
}

// 再現（低3）: 閲覧制限中の画像（/uploads/）を、sync_tokens に記録の無いトークンで取得すると、許可したまま記録しない。
// 同期 API に送ったトークンは初めて使われたときに記録して取り消せるようにするのに、画像の取得だけに使ったトークンは
// 一覧に出ず、取り消せない。記録できないとき（DB の失敗）も画像を返してしまう。
func TestReproUploadTokenIsRecordedAndRevocable(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	const name = "0123456789abcdef0123456789abcdef-640.webp"
	remote.writeUpload(name, "image")
	owner, _ := remote.recordedToken("owner", auth.RoleAdmin)
	expectSyncStatus(t, setSiteAccessOverTLS(t, remote, owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}), http.StatusOK)
	anon := remote.newClient()
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	unrecorded := func(sub string) (string, *auth.Claims) {
		tok, claims, err := auth.IssueTokenClaims(remote.priv, sub, string(auth.RoleEditor), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok, claims
	}

	editor, claims := unrecorded("desk")
	expectStatus(t, anon.send(http.MethodGet, "/uploads/"+name, bearer(editor)), http.StatusOK)
	if n := remote.countRows(`SELECT COUNT(*) FROM sync_tokens WHERE jti = ? AND issued_via = ? AND revoked_at IS NULL`, claims.ID, TokenIssuedViaSeen); n != 1 {
		t.Errorf("a token used only for images was not recorded (%d rows)", n)
	}
	// 記録したトークンは、同期 API の取り消しで取り消せ、以後は画像も読めない。
	if rec := remote.call(owner, revokeRequest(claims.ID, false)); rec.Code != http.StatusOK {
		t.Errorf("revoke the token used for images: status %d, want 200\nbody: %.500s", rec.Code, rec.Body.String())
	}
	if res := anon.send(http.MethodGet, "/uploads/"+name, bearer(editor)); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("image with the revoked token: status %d, want 401", res.StatusCode)
	}

	// 記録できない（DB の書き込みが失敗する）ときは、画像を返さずに断る。
	if _, err := remote.db.Exec(`CREATE TRIGGER sync_tokens_refuse_insert BEFORE INSERT ON sync_tokens BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	fresh, freshClaims := unrecorded("fresh-desk")
	if res := anon.send(http.MethodGet, "/uploads/"+name, bearer(fresh)); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("image with a token that could not be recorded: status %d, want 401", res.StatusCode)
	}
	if n := remote.countRows(`SELECT COUNT(*) FROM sync_tokens WHERE jti = ?`, freshClaims.ID); n != 0 {
		t.Errorf("rows for the unrecordable token: %d", n)
	}
}
