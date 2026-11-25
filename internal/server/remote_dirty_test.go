package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/theme"
)

// pageRecorder は描画したページのデータ（page）を記録してから、元の Renderer で描く。テンプレートの見た目に頼らずに、
// ハンドラが layout.html やページに渡す値を確かめる。
type pageRecorder struct {
	echo.Renderer
	mu    sync.Mutex
	pages map[string]page
}

func (r *pageRecorder) Render(w io.Writer, name string, data any, c echo.Context) error {
	if p, ok := data.(page); ok {
		r.mu.Lock()
		r.pages[name] = p
		r.mu.Unlock()
	}
	return r.Renderer.Render(w, name, data, c)
}

// recordPages は env のサーバーが描くページのデータを記録する。
func recordPages(env *testEnv) *pageRecorder {
	r := &pageRecorder{Renderer: env.server.echo.Renderer, pages: map[string]page{}}
	env.server.echo.Renderer = r
	return r
}

// last は最後に描いたページ name のデータ。
func (r *pageRecorder) last(t *testing.T, name string) page {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pages[name]
	if !ok {
		t.Fatalf("page %q was not rendered", name)
	}
	return p
}

// renderedPage は c で path を開き、描いたページ name のデータを返す。
func (r *pageRecorder) renderedPage(t *testing.T, c *client, path, name string) page {
	t.Helper()
	expectStatus(t, c.get(path), http.StatusOK)
	return r.last(t, name)
}

func (e *testEnv) remoteDirtyList() string { return strings.Join(e.server.remoteDirty(), ",") }

// markAllRemoteDirty は未反映の記録（基本設定・外観・サイト構成）を直接書く。公開サーバーへ反映できる管理者には、
// 管理画面の共通バナー（「公開サーバーへ反映する」のボタン）が出る。
func (e *testEnv) markAllRemoteDirty() {
	e.t.Helper()
	if err := upsertSetting(e.db, remoteSettingDirty, `["general","theme","structure"]`); err != nil {
		e.t.Fatal(err)
	}
}

// saveStructure はサイト構成をメニュー1行で保存する。
func saveStructure(t *testing.T, adm *client, label string) {
	t.Helper()
	expectRedirect(t, adm.postForm("/admin/settings/structure", structureForm([][3]string{{"url", "https://www.example.ac.jp/", label}}, "")),
		"/admin/settings/structure?flash=structure_saved")
}

// 公開サーバーに接続しているとき、基本設定・外観・サイト構成を保存すると未反映として記録し、共通のバナーのデータ
// （page.RemoteDirty・RemoteCanPushSettings・RemotePushConfirm）に出す。「公開サーバーへ反映する」が成功したら消す。
func TestRemoteDirtyRecordedAndClearedByPush(t *testing.T) {
	env := newRemoteEnv(t)
	local := env.local
	adm := local.loggedIn(admin)
	pages := recordPages(local)

	// 接続していないあいだの保存は記録しない（接続したときに古い記録を出さない）。
	expectRedirect(t, adm.postForm("/admin/settings/general", generalSettingsForm()), "/admin/settings/general?flash=settings_saved")
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {theme.Default}}), "/admin/settings/theme?flash=theme_saved")
	saveStructure(t, adm, "研究室")
	if got := local.remoteDirtyList(); got != "" {
		t.Fatalf("dirty without a connection = %q", got)
	}
	if p := pages.renderedPage(t, adm, "/admin", "dashboard"); len(p.RemoteDirty) != 0 || p.RemoteCanPushSettings || p.RemotePushConfirm != "" {
		t.Errorf("not connected: page = %v %v %q", p.RemoteDirty, p.RemoteCanPushSettings, p.RemotePushConfirm)
	}

	env.configure("root", auth.RoleAdmin)
	if p := pages.renderedPage(t, adm, "/admin", "dashboard"); len(p.RemoteDirty) != 0 || !p.RemoteCanPushSettings || p.RemotePushConfirm != "" {
		t.Errorf("connected and clean: page = %v %v %q", p.RemoteDirty, p.RemoteCanPushSettings, p.RemotePushConfirm)
	}

	// 保存した順ではなく、基本設定・外観・サイト構成の順に並べる。
	saveStructure(t, adm, "ニュース")
	expectRedirect(t, adm.postForm("/admin/settings/theme/card-images", url.Values{settingShowFrontCardImages: {"1"}}),
		"/admin/settings/theme?flash=card_images_saved#card-images")
	form := generalSettingsForm()
	form.Set("site_name", "反映前のサイト名")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if got := local.remoteDirtyList(); got != "general,theme,structure" {
		t.Fatalf("dirty = %q", got)
	}
	var stored []string
	if err := json.Unmarshal([]byte(local.setting(remoteSettingDirty)), &stored); err != nil || len(stored) != 3 {
		t.Errorf("remote_dirty_settings must be a JSON array: %q (%v)", local.setting(remoteSettingDirty), err)
	}

	p := pages.renderedPage(t, adm, "/admin/settings/general", "settings_general")
	if strings.Join(p.RemoteDirty, ",") != "general,theme,structure" || !p.RemoteCanPushSettings || p.RemotePushConfirm == "" {
		t.Errorf("admin page = %v %v %q", p.RemoteDirty, p.RemoteCanPushSettings, p.RemotePushConfirm)
	}
	// 編集長にも未反映は見えるが、反映はできない（管理者だけ）。
	p = pages.renderedPage(t, local.loggedIn(chief), "/admin", "dashboard")
	if strings.Join(p.RemoteDirty, ",") != "general,theme,structure" || p.RemoteCanPushSettings || p.RemotePushConfirm != "" {
		t.Errorf("editor page = %v %v %q", p.RemoteDirty, p.RemoteCanPushSettings, p.RemotePushConfirm)
	}

	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, nil), http.StatusOK)
	if got := local.remoteDirtyList(); got != "" {
		t.Fatalf("dirty after push = %q", got)
	}
	if got := env.remote.setting("site_name"); got != "反映前のサイト名" {
		t.Errorf("remote site_name = %q", got)
	}
	if p := pages.renderedPage(t, adm, "/admin", "dashboard"); len(p.RemoteDirty) != 0 || p.RemotePushConfirm != "" {
		t.Errorf("after push: page = %v %q", p.RemoteDirty, p.RemotePushConfirm)
	}

	// 失敗した反映（設定を反映できないトークン）では消さない。トークンでは反映できないことも共通のデータに出す。
	saveStructure(t, adm, "お知らせ")
	env.configure("sato", auth.RoleEditor)
	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, nil), http.StatusForbidden)
	if got := local.remoteDirtyList(); got != "structure" {
		t.Errorf("dirty after a rejected push = %q", got)
	}
	if p := pages.renderedPage(t, adm, "/admin", "dashboard"); strings.Join(p.RemoteDirty, ",") != "structure" || p.RemoteCanPushSettings {
		t.Errorf("editor token: page = %v %v", p.RemoteDirty, p.RemoteCanPushSettings)
	}
}

// サイト基本設定の保存は、公開サーバーへ送る項目（サイト名・説明・公開サイトの URL・著作権表示・アクセス解析タグ）が
// 変わったときだけ未反映にする。この sorus 自身の閲覧制限（basic_auth_*）だけの変更は未反映にしない。
func TestRemoteDirtyGeneralIgnoresBasicAuthOnly(t *testing.T) {
	env := newRemoteEnv(t)
	local := env.local
	adm := local.loggedIn(admin)
	expectRedirect(t, adm.postForm("/admin/settings/general", generalSettingsForm()), "/admin/settings/general?flash=settings_saved")
	env.configure("root", auth.RoleAdmin)

	enableBasicAuth(t, adm, testBasicAuthUser, testBasicAuthPass)
	if got := local.remoteDirtyList(); got != "" {
		t.Fatalf("basic auth only change made general dirty: %q", got)
	}
	// 同じ内容でもう一度保存しても、送る値は変わらない。
	form := generalSettingsForm()
	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	form.Set("basic_auth_enabled", "1")
	form.Set("basic_auth_user", testBasicAuthUser)
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if got := local.remoteDirtyList(); got != "" {
		t.Fatalf("unchanged save made general dirty: %q", got)
	}

	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	form.Set("site_description", "説明を変えた")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if got := local.remoteDirtyList(); got != "general" {
		t.Fatalf("description change: dirty = %q", got)
	}
}

// 外観のテーマの取り込み（Zip）・削除も、外観の未反映として記録する。
func TestRemoteDirtyThemeInstallAndUninstall(t *testing.T) {
	env, _ := newCustomThemeEnv(t, nil)
	if err := env.server.saveRemoteConfig(remoteConfig{URL: "https://pub.example.jp", Token: "configured-token"}); err != nil {
		t.Fatal(err)
	}
	adm := env.loggedIn(admin)
	res := adm.postThemeZip("lab.zip", testThemeZip(t, "lab-theme", testThemeFiles("lab-theme", "研究室テーマ")))
	expectStatus(t, res, http.StatusSeeOther)
	if got := env.remoteDirtyList(); got != "theme" {
		t.Fatalf("dirty after install = %q", got)
	}
	if _, err := env.db.Exec(`DELETE FROM settings WHERE key = ?`, remoteSettingDirty); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adm.postForm("/admin/settings/theme/uninstall", url.Values{"theme_id": {"lab-theme"}}), http.StatusSeeOther)
	if got := env.remoteDirtyList(); got != "theme" {
		t.Fatalf("dirty after uninstall = %q", got)
	}
	// 失敗した削除（無いテーマ）は記録しない。
	if _, err := env.db.Exec(`DELETE FROM settings WHERE key = ?`, remoteSettingDirty); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adm.postForm("/admin/settings/theme/uninstall", url.Values{"theme_id": {"lab-theme"}}), http.StatusSeeOther)
	if got := env.remoteDirtyList(); got != "" {
		t.Fatalf("dirty after a failed uninstall = %q", got)
	}
}

// サイト構成を知らない古い公開サーバーへの反映は、基本設定・外観だけを消し、サイト構成は未反映のまま残す。
func TestRemoteDirtyOldServerKeepsStructure(t *testing.T) {
	env := newRemoteEnv(t)
	adm := env.local.loggedIn(admin)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"sync_settings","settings":{"site_name":"古い公開サーバー"}}`))
	}))
	defer old.Close()
	if err := env.local.server.saveRemoteConfig(remoteConfig{URL: old.URL, SyncPath: "/api/sync", Token: env.remote.token("root", auth.RoleAdmin)}); err != nil {
		t.Fatal(err)
	}
	form := generalSettingsForm()
	form.Set("site_name", "古いサーバー向け")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	saveStructure(t, adm, "研究室")
	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, nil), http.StatusOK)
	if got := env.local.remoteDirtyList(); got != "structure" {
		t.Errorf("dirty after pushing to an old server = %q", got)
	}
}

// 反映の途中で別の保存が記録されたら、その反映の完了では消さない（送っていない変更を反映済みと見せない）。
func TestRemoteDirtyKeepsChangesMadeDuringPush(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	if err := s.saveRemoteConfig(remoteConfig{URL: "https://pub.example.jp", Token: "configured-token"}); err != nil {
		t.Fatal(err)
	}
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
	s.markRemoteDirty(c, remoteDirtyTheme)
	rev := s.remoteDirtyRevision()
	s.markRemoteDirty(c, remoteDirtyGeneral)
	s.clearRemoteDirty(c, rev, remoteDirtyKinds...)
	if got := env.remoteDirtyList(); got != "general,theme" {
		t.Fatalf("dirty after a push that raced with a save = %q", got)
	}
	s.clearRemoteDirty(c, s.remoteDirtyRevision(), remoteDirtyKinds...)
	if got := env.remoteDirtyList(); got != "" {
		t.Fatalf("dirty after a clean push = %q", got)
	}

	// 接続先を変えたとき（接続をやめたときを含む）は、前の接続先に対する記録を消す。トークンだけの更新では残す。
	s.markRemoteDirty(c, remoteDirtyStructure)
	if err := s.saveRemoteConfig(remoteConfig{URL: "https://pub.example.jp", Token: "renewed-token"}); err != nil {
		t.Fatal(err)
	}
	if got := env.remoteDirtyList(); got != "structure" {
		t.Fatalf("token renewal must keep the record: %q", got)
	}
	if err := s.saveRemoteConfig(remoteConfig{URL: "https://other.example.jp", Token: "other-token"}); err != nil {
		t.Fatal(err)
	}
	if got := env.remoteDirtyList(); got != "" {
		t.Fatalf("another server must not inherit the record: %q", got)
	}
}

func TestParseRemoteDirty(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                 "",
		`["structure","general"]`:          "general,structure",
		`["theme","theme","unknown"]`:      "theme",
		"structure, theme ,general":        "general,theme,structure",
		`not json, structure`:              "structure",
		`["general","theme","structure"]`:  "general,theme,structure",
		`{"general":true}`:                 "",
		`["GENERAL"]`:                      "",
		`["general","theme","structure",]`: "",
	} {
		if got := strings.Join(parseRemoteDirty(raw), ","); got != want {
			t.Errorf("parseRemoteDirty(%q) = %q, want %q", raw, got, want)
		}
	}
}
