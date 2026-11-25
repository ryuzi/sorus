package server

import (
	"net/http"
	"net/url"
	"testing"

	"sorus/internal/auth"
)

// 再現: 公開サーバーの閲覧制限を、接続した管理者が同期 API で確認・変更できない（get_site_access・set_site_access が
// 未対応の action として 400 になる）。PC の「公開サーバー」画面にも、公開サーバーの閲覧制限を扱う API が無い。
func TestReproRemoteSiteAccessUnavailable(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	adminToken := remote.token("owner", auth.RoleAdmin)
	rec := remote.call(adminToken, map[string]any{"action": "get_site_access"})
	if rec.Code != http.StatusOK {
		t.Errorf("get_site_access: status %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	res := remote.newClient().syncHTTP("Bearer "+adminToken, map[string]any{"action": "set_site_access", "site_access": map[string]any{
		"enabled": true, "user": "preview", "password": "s3cret-preview", "expected_revision": ""}})
	if res.StatusCode != http.StatusOK {
		t.Errorf("set_site_access: status %d, want 200\nbody: %s", res.StatusCode, res.Body)
	}
	if got := remote.get("/").Code; got != http.StatusUnauthorized {
		t.Errorf("public page after set_site_access: status %d, want 401", got)
	}

	env := &remoteEnv{local: newTestEnv(t), remote: remote}
	env.configure("owner", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	if res := adm.getJSON("/admin/remote/site-access", nil); res.StatusCode != http.StatusOK {
		t.Errorf("GET /admin/remote/site-access: status %d, want 200\nbody: %.500s", res.StatusCode, res.Body)
	}
	if res := adm.postJSON("/admin/remote/site-access", url.Values{"enabled": {"0"}, "revision": {""}}, nil); res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		t.Errorf("POST /admin/remote/site-access: status %d, want a handler", res.StatusCode)
	}
}
