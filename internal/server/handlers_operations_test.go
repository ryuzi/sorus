package server

import (
	"net/http"
	"testing"

	"sorus/internal/auth"
)

// operationsData は「サイト運用」の画面に渡したデータ。
func operationsData(t *testing.T, pages *pageRecorder, c *client) (page, remoteSettingsData) {
	t.Helper()
	p := pages.renderedPage(t, c, operationsPagePath, "settings_operations")
	d, ok := p.Data.(remoteSettingsData)
	if !ok {
		t.Fatalf("settings_operations data = %T, want remoteSettingsData", p.Data)
	}
	return p, d
}

// 「サイト運用」（/admin/settings/operations）は管理者だけが開け、公開サーバーの閲覧制限・メンテナンス表示を扱えるかを
// 「公開サーバー」画面と同じ条件（remoteSettingsData）で渡す。確認・切り替えの API はそのまま残す。
func TestOperationsPage(t *testing.T) {
	env := newRemoteEnv(t)
	local := env.local
	pages := recordPages(local)
	adm := local.loggedIn(admin)

	// 接続していなければ、閲覧制限・メンテナンス表示は扱えない（画面は接続の案内を出す）。
	p, d := operationsData(t, pages, adm)
	if d.Connected || d.CanManageSiteAccess || d.CanManageMaintenance || d.MaintenancePage != nil {
		t.Errorf("not connected: data = %+v", d)
	}
	if p.Title != translateOr("ja", "operations_title") || p.Title == "operations_title" {
		t.Errorf("title = %q", p.Title)
	}
	if !d.LocalBasicAuth {
		t.Error("server mode has the local Basic authentication setting")
	}

	// 管理者のトークンで接続していれば扱える。
	env.configure("root", auth.RoleAdmin)
	_, d = operationsData(t, pages, adm)
	if !d.Connected || !d.CanManageSiteAccess || !d.CanManageMaintenance || d.MaintenancePage == nil {
		t.Errorf("admin token: data = %+v", d)
	}

	// 設定を反映できないトークンでは扱えない（接続はしている）。
	env.configure("sato", auth.RoleEditor)
	_, d = operationsData(t, pages, adm)
	if !d.Connected || d.CanManageSiteAccess || d.CanManageMaintenance {
		t.Errorf("editor token: data = %+v", d)
	}

	// 管理者以外は開けない。
	for _, u := range []testUser{chief, studentA} {
		expectStatus(t, local.loggedIn(u).get(operationsPagePath), http.StatusForbidden)
	}

	// 既存の API はそのまま使える（管理者だけ）。
	env.configure("root", auth.RoleAdmin)
	for _, path := range []string{"/admin/remote/site-access", "/admin/remote/maintenance"} {
		if res := adm.getJSON(path, nil); res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status %d", path, res.StatusCode)
		}
		expectStatus(t, local.loggedIn(chief).getJSON(path, nil), http.StatusForbidden)
	}
}

// デスクトップモードでも「サイト運用」を開ける（デスクトップの sorus は公開サーバーへ接続する母艦）。デスクトップには
// この sorus 自身の閲覧制限が無いことを渡す。
func TestOperationsPageDesktop(t *testing.T) {
	env, _ := newDesktopEnv(t)
	pages := recordPages(env)
	c := env.completeSetup()
	_, d := operationsData(t, pages, c)
	if d.Connected || d.LocalBasicAuth {
		t.Errorf("desktop data = %+v", d)
	}
}

// 「スマートフォンで書く」（/admin/settings/mobile）は削除した。
func TestMobileConnectRemoved(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{admin, studentA} {
		expectStatus(t, env.loggedIn(u).get("/admin/settings/mobile"), http.StatusNotFound)
	}
	for _, name := range pageNames {
		if name == "settings_mobile" {
			t.Error("settings_mobile must not be registered as a page")
		}
	}
}
