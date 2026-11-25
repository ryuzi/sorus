package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/theme"
	"sorus/static"
)

// 公開サーバーのメンテナンス表示（get_maintenance・set_maintenance）と、PC の「公開サーバー」画面からの確認・切り替えの
// 結合テスト。

const (
	// maintenanceTitleJA はメンテナンス画面の見出し（日本語）。
	maintenanceTitleJA = "ただいまメンテナンス中です"
	maintImage         = "0123456789abcdef0123456789abcdef-640.webp"
)

func boolRef(v bool) *bool { return &v }

func (e *syncEnv) maintenanceHTTP(token string, body any) *response {
	e.t.Helper()
	return e.newClient().syncHTTP("Bearer "+token, body)
}

func (e *syncEnv) getMaintenance(token string) SyncMaintenanceResponse {
	e.t.Helper()
	res := e.maintenanceHTTP(token, SyncRequest{Action: SyncActionGetMaintenance})
	expectStatus(e.t, res, http.StatusOK)
	return decodeResponse[SyncMaintenanceResponse](e.t, res)
}

func (e *syncEnv) setMaintenance(token string, on bool) SyncMaintenanceResponse {
	e.t.Helper()
	res := e.maintenanceHTTP(token, SyncRequest{Action: SyncActionSetMaintenance, Maintenance: &SyncMaintenanceUpdate{Enabled: boolRef(on)}})
	expectStatus(e.t, res, http.StatusOK)
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		e.t.Errorf("Cache-Control of the sync response = %q, want no-store", got)
	}
	return decodeResponse[SyncMaintenanceResponse](e.t, res)
}

// rawRequest は CSRF トークンを付けずに method で path に要求を送る（公開専用モードには管理画面が無く、CSRF の Cookie を
// 発行できないため）。
func (c *client) rawRequest(method, path string, header map[string]string) *response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return c.do(req)
}

// expectMaintenancePage は res がメンテナンス画面（HTTP 503）で、どこにもキャッシュさせない応答かを確かめる。
// 公開サイトの内容（unwanted）を含まないことも確かめる。
func expectMaintenancePage(t *testing.T, what string, res *response, unwanted ...string) {
	t.Helper()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%s: status %d, want 503\nbody: %.1000s", what, res.StatusCode, res.Body)
	}
	h := res.Header
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
	}
	if got := h.Get("ETag"); got != "" {
		t.Errorf("%s: ETag = %q, want none", what, got)
	}
	if got := h.Get("WWW-Authenticate"); got != "" {
		t.Errorf("%s: WWW-Authenticate = %q, want none", what, got)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", what, got)
	}
	if !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Errorf("%s: Content-Type = %q, want text/html", what, h.Get("Content-Type"))
	}
	if res.Request.Method != http.MethodHead {
		expectContains(t, res.Body, maintenanceTitleJA, `name="robots" content="noindex"`)
		// テーマに依存しない（テーマの CSS・テーマのファイルを読み込まない）。
		expectNotContains(t, res.Body, "/custom-themes/", "/static/themes/", "<link", "<script")
		expectNotContains(t, res.Body, unwanted...)
	}
}

// expectNotMaintenance は res がメンテナンス画面でないかを確かめる。
func expectNotMaintenance(t *testing.T, what string, res *response) {
	t.Helper()
	if res.StatusCode == http.StatusServiceUnavailable || strings.Contains(res.Body, maintenanceTitleJA) {
		t.Errorf("%s: got the maintenance page (status %d)", what, res.StatusCode)
	}
}

// 機能を使っていない公開サーバーは、今までどおり公開サイトを配信する。状態の確認だけでは何も変わらない。
func TestMaintenanceOffByDefault(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	remote.insertPublished("m-0", "ふだんの記事", "usual-article")
	owner := remote.token("owner", auth.RoleAdmin)
	st := remote.getMaintenance(owner)
	if st.Action != SyncActionGetMaintenance || st.Enabled || st.ChangedAt != nil || st.Changed {
		t.Fatalf("initial state = %+v, want off and never changed", st)
	}
	anon := remote.newClient()
	res := anon.get("/articles/usual-article")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "ふだんの記事")
	if got := res.Header.Get("Cache-Control"); got != PublicHTMLCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, PublicHTMLCacheControl)
	}
	if n := remote.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, maintenanceSetting); n != 0 {
		t.Errorf("get_maintenance wrote the setting (%d rows)", n)
	}
}

// 管理者のトークンでメンテナンス表示を始めると、公開サイトのどのパス・メソッドにもテーマに依存しない 503 を返し、
// キャッシュさせない。同期 API・組み込みの共通アセット・稼働の監視は今までどおり使える。終えると元に戻る。
func TestSyncMaintenanceLifecycle(t *testing.T) {
	remote := newPublicOnlyEnv(t, func(db *sqlx.DB) {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('site_name', 'メンテ試験メディア')`); err != nil {
			t.Fatal(err)
		}
	})
	remote.insertPublished("m-1", "メンテ中は見せない記事", "hidden-article")
	seedArticle(t, remote.db, article.Article{Title: "会社案内のページ", Slug: "about", Kind: article.KindPage,
		ContentMarkdown: "ページ本文", Status: article.StatusPublished})
	remote.writeUpload(maintImage, "image-bytes")
	owner := remote.token("owner", auth.RoleAdmin)
	anon := remote.newClient()
	siteName := remote.server.getSiteName()
	if siteName != "メンテ試験メディア" {
		t.Fatalf("site name = %q", siteName)
	}

	before := time.Now().UTC().Add(-time.Second)
	on := remote.setMaintenance(owner, true)
	if on.Action != SyncActionSetMaintenance || !on.Enabled || !on.Changed || on.ChangedAt == nil ||
		on.ChangedAt.Before(before) || on.ChangedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("after turning on = %+v", on)
	}
	if got := remote.getMaintenance(owner); !got.Enabled || got.ChangedAt == nil || !got.ChangedAt.Equal(*on.ChangedAt) || got.Changed {
		t.Fatalf("get after turning on = %+v, want on since %v", got, on.ChangedAt)
	}

	for _, p := range []string{
		"/", "/articles", "/articles/hidden-article", "/about", "/categories/uncategorized", "/search?q=%E8%A8%98%E4%BA%8B",
		"/feed.xml", "/sitemap.xml", "/robots.txt", "/favicon.ico", "/llms.txt", "/llms-full.txt",
		"/en", "/en/", "/en/articles", "/en/about", "/no-such-page", "/uploads/" + maintImage,
		"/og-image/hidden-article.png", "/custom-themes/paper/theme.css", "/attachments/x/download",
		// 対象外のパスに見せかけた表記（正規化すると公開ページ）も 503 にする。
		"/api/../articles/hidden-article", "/static/../articles/hidden-article",
		// 同期 API のパスでも、POST 以外はほかのパスと同じ（秘匿したパスの有無を応答で見せない）。
		remote.server.cfg.SyncAPIPath, remote.server.cfg.SyncAPIPath + "/x", remote.server.cfg.SyncAPIPath + "/../../about",
		// 公開専用モードの管理画面のパス・/api の下の存在しないパスは、テーマつきの 404 ページ（サイト名・メニュー）を
		// 返していたため、ほかのパスと同じメンテナンス画面にする。
		"/admin", "/admin/login", "/api/no-such-api", "/api/mcp",
	} {
		expectMaintenancePage(t, "GET "+p, anon.get(p), "メンテ中は見せない記事", "会社案内のページ", "image-bytes", siteName)
	}
	expectMaintenancePage(t, "HEAD /", anon.head("/"))
	expectMaintenancePage(t, "POST /", anon.rawRequest(http.MethodPost, "/", nil))
	expectMaintenancePage(t, "POST /api/no-such-api", anon.rawRequest(http.MethodPost, "/api/no-such-api", nil))
	expectMaintenancePage(t, "GET / (browser)", anon.getAs("/articles/hidden-article", browserUA, nil), "メンテ中は見せない記事")
	// 英語の表示言語なら英語で案内する。
	en := anon.rawRequest(http.MethodGet, "/", map[string]string{"Accept-Language": "en-US,en;q=0.9"})
	if en.StatusCode != http.StatusServiceUnavailable || !strings.Contains(en.Body, "Under maintenance") || !strings.Contains(en.Body, `lang="en"`) {
		t.Errorf("English maintenance page: status %d\nbody: %.500s", en.StatusCode, en.Body)
	}

	// 組み込みの共通アセット・稼働の監視は今までどおり（サイトの内容を含まない）。
	for _, p := range []string{static.URL("js/public.js"), "/healthz", "/manifest.json", "/sw.js"} {
		res := anon.get(p)
		expectStatus(t, res, http.StatusOK)
	}
	expectNotContains(t, anon.get("/manifest.json").Body, siteName)
	// 同期 API に POST してもトークンが無ければ今までどおり 401（メンテナンス画面ではない）。
	expectStatus(t, anon.syncHTTP("", SyncRequest{Action: SyncActionServerInfo}), http.StatusUnauthorized)

	// 同期 API はトークンで今までどおり使える（記事の提出・校閲待ちの取得）。
	editor := remote.token("desk", auth.RoleEditor)
	sub := remote.newClient().syncHTTP("Bearer "+editor, submitRequest(SyncArticle{Title: "メンテ中の提出", ContentMarkdown: "本文"}))
	expectStatus(t, sub, http.StatusOK)
	expectStatus(t, remote.newClient().syncHTTP("Bearer "+editor, SyncRequest{Action: SyncActionFetchPending}), http.StatusOK)

	// 同じ状態を指定しても何も変えず、切り替えた時刻もそのまま。
	same := remote.setMaintenance(owner, true)
	if !same.Enabled || same.Changed || same.ChangedAt == nil || !same.ChangedAt.Equal(*on.ChangedAt) {
		t.Fatalf("turning on again = %+v, want unchanged since %v", same, on.ChangedAt)
	}

	off := remote.setMaintenance(owner, false)
	if off.Enabled || !off.Changed || off.ChangedAt == nil {
		t.Fatalf("after turning off = %+v", off)
	}
	res := anon.get("/articles/hidden-article")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "メンテ中は見せない記事")
	if got := res.Header.Get("Cache-Control"); got != PublicHTMLCacheControl {
		t.Errorf("Cache-Control after turning off = %q, want %q", got, PublicHTMLCacheControl)
	}
	img := anon.get("/uploads/" + maintImage)
	expectStatus(t, img, http.StatusOK)
	if got := img.Header.Get("Cache-Control"); got != StaticCacheControl {
		t.Errorf("image Cache-Control after turning off = %q, want %q", got, StaticCacheControl)
	}
	if got := remote.getMaintenance(owner); got.Enabled {
		t.Fatalf("get after turning off = %+v", got)
	}
}

// 確認・切り替えは管理者のトークンだけ。権限が無い・トークンが無い要求は何も返さず、何も変えない。
func TestSyncMaintenancePermissions(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleEditor} {
		tok := remote.token("member-"+string(role), role)
		expectStatus(t, remote.maintenanceHTTP(tok, SyncRequest{Action: SyncActionGetMaintenance}), http.StatusForbidden)
		set := remote.maintenanceHTTP(tok, SyncRequest{Action: SyncActionSetMaintenance, Maintenance: &SyncMaintenanceUpdate{Enabled: boolRef(true)}})
		expectStatus(t, set, http.StatusForbidden)
	}
	anon := remote.newClient()
	expectStatus(t, anon.syncHTTP("", SyncRequest{Action: SyncActionSetMaintenance, Maintenance: &SyncMaintenanceUpdate{Enabled: boolRef(true)}}), http.StatusUnauthorized)
	expectStatus(t, anon.syncHTTP("Bearer "+tamperRole(t, remote.token("w", auth.RoleWriter), auth.RoleAdmin),
		SyncRequest{Action: SyncActionSetMaintenance, Maintenance: &SyncMaintenanceUpdate{Enabled: boolRef(true)}}), http.StatusUnauthorized)
	if st := remote.getMaintenance(owner); st.Enabled {
		t.Fatalf("unauthorized requests turned maintenance on: %+v", st)
	}
	expectStatus(t, anon.get("/"), http.StatusOK)
}

// 切り替え先が無い要求は 400 で断り、何も変えない（既定値で始めたり終えたりしない）。
func TestSyncMaintenanceValidation(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	remote.setMaintenance(owner, true)
	for _, body := range []any{
		SyncRequest{Action: SyncActionSetMaintenance},
		map[string]any{"action": SyncActionSetMaintenance, "maintenance": map[string]any{}},
		map[string]any{"action": SyncActionSetMaintenance, "maintenance": map[string]any{"enabled": nil}},
	} {
		res := remote.maintenanceHTTP(owner, body)
		expectSiteAccessCode(t, res, http.StatusBadRequest, maintenanceCodeMissing)
	}
	expectSyncStatus(t, remote.call(owner, map[string]any{"action": SyncActionSetMaintenance, "maintenance": map[string]any{"enabled": "false"}}), http.StatusBadRequest)
	if st := remote.getMaintenance(owner); !st.Enabled {
		t.Fatalf("invalid requests turned maintenance off: %+v", st)
	}
}

// 公開サーバーを再起動しても、メンテナンス表示を続ける。
func TestSyncMaintenancePersistsAcrossRestart(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	on := remote.setMaintenance(owner, true)

	restarted, err := New(Config{DB: remote.db, UploadDir: remote.uploadDir, AttachmentDir: remote.attachmentDir, PublicOnly: true,
		SyncPublicKey: remote.server.cfg.SyncPublicKey, SyncAPIPath: remote.server.cfg.SyncAPIPath, DisableScheduler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = restarted.Shutdown(ctx)
	})
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		restarted.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := serve(httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), maintenanceTitleJA) {
		t.Fatalf("after restart: status %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	b, _ := json.Marshal(SyncRequest{Action: SyncActionGetMaintenance})
	req := httptest.NewRequest(http.MethodPost, remote.server.cfg.SyncAPIPath, bytes.NewReader(b))
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec = serve(req)
	expectSyncStatus(t, rec, http.StatusOK)
	if st := decodeSync[SyncMaintenanceResponse](t, rec); !st.Enabled || st.ChangedAt == nil || !st.ChangedAt.Equal(*on.ChangedAt) {
		t.Fatalf("get_maintenance after restart = %+v, want on since %v", st, on.ChangedAt)
	}
}

// 通常の反映（sync_settings。独自テーマのファイルを含む）と閲覧制限の変更は、メンテナンス表示を切り替えない。
// メンテナンス表示の切り替えも、閲覧制限を変えない。
func TestSyncSettingsAndSiteAccessDoNotChangeMaintenance(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	on := remote.setMaintenance(owner, true)
	raw := map[string]any{"action": SyncActionSyncSettings, "settings": map[string]any{
		"site_name": "反映したサイト", "maintenance": false, "maintenance_enabled": "0",
	}, "maintenance": map[string]any{"enabled": false}}
	expectStatus(t, remote.maintenanceHTTP(owner, raw), http.StatusOK)
	if remote.setting("site_name") != "反映したサイト" {
		t.Fatal("sync_settings did not save the site name")
	}
	if st := remote.getMaintenance(owner); !st.Enabled || st.ChangedAt == nil || !st.ChangedAt.Equal(*on.ChangedAt) {
		t.Fatalf("sync_settings changed maintenance: %+v", st)
	}
	// サイト基本設定の項目にメンテナンス表示を加えない（通常の反映で切り替わる経路を作らない）。
	typ := reflect.TypeOf(SyncSettings{})
	for i := range typ.NumField() {
		if tag := typ.Field(i).Tag.Get("json"); strings.Contains(tag, "maintenance") {
			t.Errorf("SyncSettings has a maintenance field %q", tag)
		}
	}

	// 閲覧制限を変えてもメンテナンス表示は続き、メンテナンス表示を終えても閲覧制限は変わらない。
	sa := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})))
	if st := remote.getMaintenance(owner); !st.Enabled {
		t.Fatalf("set_site_access turned maintenance off: %+v", st)
	}
	remote.setMaintenance(owner, false)
	if got := remote.getSiteAccess(owner); got.Revision != sa.Revision || !got.Active {
		t.Fatalf("set_maintenance changed the site access: %+v", got)
	}
}

// PC から独自テーマを反映しても、メンテナンス表示は続き、テーマのファイルも訪問者には見せない。
func TestRemoteThemePushKeepsMaintenance(t *testing.T) {
	env, _, _ := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	owner := env.remote.token("owner2", auth.RoleAdmin)
	on := env.remote.setMaintenance(owner, true)

	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", testThemeFiles("paper", "ペーパー"))),
		"installed", "paper", "テーマ「ペーパー」を取り込みました")
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"paper"}}), "/admin/settings/theme")
	pushRemoteSettings(t, adm, http.StatusOK)
	if got := env.remote.setting("theme"); got != "paper" {
		t.Fatalf("remote theme = %q, want paper", got)
	}
	if st := env.remote.getMaintenance(owner); !st.Enabled || !st.ChangedAt.Equal(*on.ChangedAt) {
		t.Fatalf("pushing a custom theme changed maintenance: %+v", st)
	}
	reader := env.remote.newClient()
	for _, p := range []string{"/", "/custom-themes/paper/theme.css", "/custom-themes/paper/img/bg.png"} {
		expectMaintenancePage(t, p, reader.get(p), "#123456", "PNG")
	}
}

// 閲覧制限とメンテナンス表示を両方使うときは、閲覧制限を先に判定する。パスワードを知らない人には今までどおり 401 だけを
// 返し（メンテナンス中であることも見せない）、パスワードを入れた人にメンテナンス画面を返す。
func TestMaintenanceAfterBasicAuth(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	remote.insertPublished("m-2", "限定公開の記事", "limited")
	owner := remote.token("owner", auth.RoleAdmin)
	expectOK(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))
	remote.setMaintenance(owner, true)
	anon := remote.newClient()
	for _, p := range []string{"/", "/articles/limited", "/no-such-page"} {
		res := anon.get(p)
		expectBasicAuthChallenge(t, res)
		expectNotContains(t, res.Body, maintenanceTitleJA, "限定公開の記事")
		expectMaintenancePage(t, p+" with credentials", anon.getAs(p, browserUA, basicAuthHeader(remoteAccessUser, remoteAccessPass)), "限定公開の記事")
	}
}

// メンテナンス表示中も、PC は校閲待ちの記事の画像を同期 API のトークン（編集者以上）で取得できる。その応答は共有の
// キャッシュに残させない。トークンが無い・足りない要求には画像を返さない。
func TestMaintenanceAllowsSyncTokenUploads(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	remote.writeUpload(maintImage, "image-bytes")
	owner := remote.token("owner", auth.RoleAdmin)
	remote.setMaintenance(owner, true)
	anon := remote.newClient()
	for _, role := range []auth.Role{auth.RoleEditor, auth.RoleAdmin} {
		res := anon.rawRequest(http.MethodGet, "/uploads/"+maintImage, map[string]string{"Authorization": "Bearer " + remote.token("pc", role)})
		expectStatus(t, res, http.StatusOK)
		if res.Body != "image-bytes" {
			t.Fatalf("image body = %q", res.Body)
		}
		if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
			t.Errorf("%s: Cache-Control = %q, want %q", role, got, basicAuthCacheControl)
		}
	}
	writer := remote.token("w", auth.RoleWriter)
	expectMaintenancePage(t, "writer token", anon.rawRequest(http.MethodGet, "/uploads/"+maintImage, map[string]string{"Authorization": "Bearer " + writer}), "image-bytes")
	expectMaintenancePage(t, "no token", anon.get("/uploads/"+maintImage), "image-bytes")
	// トークンでも、画像以外の公開ページは返さない。
	expectMaintenancePage(t, "page with a token", anon.rawRequest(http.MethodGet, "/", map[string]string{"Authorization": "Bearer " + owner}))
}

// 秘匿した同期 API のパスを /api の外に置いた公開サーバーでも、POST 以外ではほかのパスと同じメンテナンス画面にし、
// パスの有無を見せない。同期 API はトークンで使える。
func TestMaintenanceHidesCustomSyncPath(t *testing.T) {
	remote := newSyncEnvWith(t, Config{PublicOnly: true, SyncAPIPath: "/hooks/sync"}, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	remote.setMaintenance(owner, true)
	anon := remote.newClient()
	other := anon.get("/hooks/other")
	expectMaintenancePage(t, "GET /hooks/other", other)
	for _, p := range []string{"/hooks/sync", "/hooks/sync/x"} {
		res := anon.get(p)
		expectMaintenancePage(t, "GET "+p, res)
		if res.Body != other.Body {
			t.Errorf("GET %s differs from a missing path", p)
		}
	}
	if st := remote.getMaintenance(owner); !st.Enabled {
		t.Fatalf("get_maintenance = %+v", st)
	}
}

// メンテナンス表示をかけないのは、ルーティングで実際に対象外の処理（組み込みのアセット・稼働の監視・同期 API・サーバー
// モードの管理画面と MCP）に届く要求だけ。末尾の "/"・"//"・"." の表記・符号化・登録していないメソッドで対象外のパスに
// 似せた要求は、ルートが合わずに公開サイトの 404 ページ（サイト名・メニュー）になるため、ほかのパスと同じメンテナンス
// 画面にする（秘匿した同期 API のパスの有無も見せない）。閲覧制限の有無・サーバーモードのどれでも同じ。
func TestMaintenanceExemptsOnlyRoutedPaths(t *testing.T) {
	const siteName = "免除試験メディア"
	seed := func(db *sqlx.DB) {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('site_name', ?)`, siteName); err != nil {
			t.Fatal(err)
		}
	}
	js := static.URL("js/public.js")
	type req struct{ method, path string }
	for _, tc := range []struct {
		name      string
		start     func(t *testing.T) *syncEnv
		basicAuth bool
	}{
		{"public-only", func(t *testing.T) *syncEnv { return newPublicOnlyEnv(t, seed) }, false},
		{"public-only with Basic auth", func(t *testing.T) *syncEnv { return newPublicOnlyTLSEnv(t, seed) }, true},
		{"server mode", func(t *testing.T) *syncEnv { return newSyncEnvWith(t, Config{}, seed) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := tc.start(t)
			owner := remote.token("owner", auth.RoleAdmin)
			if tc.basicAuth {
				expectOK(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))
			}
			remote.setMaintenance(owner, true)
			sp := remote.server.cfg.SyncAPIPath
			hidden := []req{
				{http.MethodGet, "/healthz/"}, {http.MethodGet, "/manifest.json/"}, {http.MethodGet, "/sw.js/"},
				{http.MethodGet, "//healthz"}, {http.MethodGet, "/./healthz"}, {http.MethodGet, "/healthz/."},
				{http.MethodGet, "/healthz%2F"}, {http.MethodGet, "/%68ealthz"}, {http.MethodGet, "/" + js},
				{http.MethodHead, "/healthz"}, {http.MethodPost, "/healthz"}, {http.MethodPost, "/manifest.json"},
				{http.MethodPut, "/sw.js"}, {http.MethodPost, js},
				{http.MethodPost, sp + "/"}, {http.MethodPost, "/" + sp}, {http.MethodPost, sp + "/."},
				{http.MethodPost, sp + "%2F"}, {http.MethodGet, sp},
				{http.MethodGet, "//admin/login"}, {http.MethodGet, "/admin/../about"},
				{http.MethodGet, "//api/mcp"}, {http.MethodPost, "//api/mcp"}, {http.MethodPost, "/api/mcp/../../about"},
			}
			anonymous := map[string]string{"Accept": "text/html"}
			viewers := []map[string]string{anonymous}
			if tc.basicAuth {
				withCredentials := basicAuthHeader(remoteAccessUser, remoteAccessPass)
				withCredentials["Accept"] = "text/html"
				viewers = append(viewers, withCredentials)
			}
			c := remote.newClient()
			ref := c.rawRequest(http.MethodGet, "/no-such-page", viewers[len(viewers)-1])
			expectMaintenancePage(t, "missing page", ref, siteName)
			for i, h := range viewers {
				who := "anonymous"
				if i > 0 {
					who = "with credentials"
				}
				for _, r := range hidden {
					what := who + " " + r.method + " " + r.path
					res := c.rawRequest(r.method, r.path, h)
					if tc.basicAuth && i == 0 && res.StatusCode == http.StatusUnauthorized {
						// 閲覧制限の対象のパスは、パスワードを知らない人には 401 だけを返す。
						expectBasicAuthChallenge(t, res)
						expectNotContains(t, res.Body, siteName)
						continue
					}
					expectMaintenancePage(t, what, res, siteName)
					if r.method != http.MethodHead && res.Body != ref.Body {
						t.Errorf("%s differs from a missing page", what)
					}
				}

				// ルートが合う要求は今までどおり。
				for _, r := range []req{{http.MethodGet, "/healthz"}, {http.MethodGet, "/manifest.json"}, {http.MethodHead, "/manifest.json"},
					{http.MethodGet, "/sw.js"}, {http.MethodHead, "/sw.js"}, {http.MethodGet, js}, {http.MethodHead, js}} {
					if res := c.rawRequest(r.method, r.path, h); res.StatusCode != http.StatusOK {
						t.Errorf("%s %s %s: status %d, want 200", who, r.method, r.path, res.StatusCode)
					}
				}
				res := c.rawRequest(http.MethodGet, strings.Replace(js, "/static/", "/static//", 1), h)
				expectNotMaintenance(t, who+" GET /static//...", res)
				expectNotContains(t, res.Body, siteName)
			}
			if st := remote.getMaintenance(owner); !st.Enabled {
				t.Fatalf("get_maintenance = %+v", st)
			}
			if !remote.server.cfg.PublicOnly {
				expectStatus(t, c.get("/admin/login"), http.StatusOK)
				for _, p := range []string{"/admin//login", "/admin/./login"} {
					res := c.get(p)
					expectNotMaintenance(t, "GET "+p, res)
					expectNotContains(t, res.Body, siteName)
				}
				for _, p := range []string{"/api/mcp", "/api/mcp/"} {
					expectStatus(t, c.rawRequest(http.MethodPost, p, nil), http.StatusUnauthorized)
				}
			}
		})
	}
}

// サーバーモード（管理画面つきで同期 API を有効にしたサーバー）では、管理画面にログイン中の編集部員は管理画面と公開サイト
// （管理画面に出す画像を含む）を今までどおり開ける。その応答は共有のキャッシュに残させない。訪問者にはメンテナンス画面。
// 復元したデータのメンテナンス表示の設定にも揃える。
func TestMaintenanceOnServerMode(t *testing.T) {
	remote := newSyncEnv(t)
	remote.insertPublished("m-3", "サーバーモードの記事", "server-article")
	remote.writeUpload(maintImage, "image-bytes")
	owner := remote.token("owner", auth.RoleAdmin)
	remote.setMaintenance(owner, true)
	staff := remote.loggedIn(admin)
	expectStatus(t, staff.get("/admin/"), http.StatusOK)
	for _, p := range []string{"/articles/server-article", "/uploads/" + maintImage} {
		res := staff.get(p)
		expectStatus(t, res, http.StatusOK)
		if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
			t.Errorf("staff %s: Cache-Control = %q, want %q", p, got, basicAuthCacheControl)
		}
	}
	anon := remote.newClient()
	expectMaintenancePage(t, "anonymous", anon.get("/articles/server-article"), "サーバーモードの記事")
	expectMaintenancePage(t, "anonymous /api/no-such-api", anon.get("/api/no-such-api"), "サーバーモードの記事")
	// 管理画面（ログイン画面を含む）と MCP サーバーは今までどおり使える。
	expectStatus(t, anon.get("/admin/login"), http.StatusOK)
	expectStatus(t, anon.rawRequest(http.MethodPost, "/api/mcp", nil), http.StatusUnauthorized)

	// サーバー内部の要求（テーマのプレビュー・「サイトに反映」の静的エクスポート）はメンテナンス画面にしない。
	preview := staff.get("/admin/settings/theme/preview?theme=tech&page=article")
	expectStatus(t, preview, http.StatusOK)
	expectContains(t, preview.Body, `data-theme="tech"`)
	expectNotContains(t, preview.Body, maintenanceTitleJA)
	out := filepath.Join(t.TempDir(), "dist")
	if _, err := remote.server.Export(t.Context(), ExportOptions{OutDir: out}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "articles", "server-article", "index.html"))
	if err != nil {
		t.Fatalf("exported article: %v", err)
	}
	if !strings.Contains(string(data), "サーバーモードの記事") || strings.Contains(string(data), maintenanceTitleJA) {
		t.Error("the static export contains the maintenance page instead of the article")
	}

	// 復元で DB の設定が変わったら、それに揃える。
	if _, err := remote.db.Exec(`UPDATE settings SET value = '0' WHERE key = ?`, maintenanceSetting); err != nil {
		t.Fatal(err)
	}
	if err := remote.server.reloadAfterRestore(context.Background()); err != nil {
		t.Fatalf("reloadAfterRestore: %v", err)
	}
	expectStatus(t, anon.get("/articles/server-article"), http.StatusOK)
}

// デスクトップモードには同期 API が無く、メンテナンス表示は働かない（復元などで設定が DB に残っていても、自分の PC の
// 公開サイトを止めない）。
func TestMaintenanceIgnoredOnDesktop(t *testing.T) {
	env := newTestEnvWith(t, Config{Desktop: true}, func(db *sqlx.DB) {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, '1')`, maintenanceSetting); err != nil {
			t.Fatal(err)
		}
	})
	env.insertPublished("m-4", "デスクトップの記事", "desktop-article")
	res := env.newClient().get("/articles/desktop-article")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "デスクトップの記事")
}

// --- PC の「公開サーバー」画面 ---

// maintenanceReply は PC の /admin/remote/maintenance の応答（成功・失敗の項目を合わせたもの）。
type maintenanceReply struct {
	remoteMaintenanceView
	Message        string                 `json:"message"`
	Changed        bool                   `json:"changed"`
	State          *remoteMaintenanceView `json:"state"`
	OutcomeUnknown bool                   `json:"outcome_unknown"`
	RemoteStatus   int                    `json:"remote_status"`
}

func decodeMaintenanceReply(t *testing.T, res *response) maintenanceReply {
	t.Helper()
	var v maintenanceReply
	if err := json.Unmarshal([]byte(res.Body), &v); err != nil {
		t.Fatalf("decode maintenance reply: %v\nbody: %.2000s", err, res.Body)
	}
	return v
}

func maintenanceForm(enabled string) url.Values { return url.Values{"enabled": {enabled}} }

// newRemoteMaintenanceEnv は PC と公開専用モードの公開サーバーを起動し、PC に管理者のトークンで接続する。
func newRemoteMaintenanceEnv(t *testing.T) (*remoteEnv, *client) {
	t.Helper()
	env := &remoteEnv{local: newTestEnv(t), remote: newPublicOnlyEnv(t, nil)}
	env.configure("owner", auth.RoleAdmin)
	return env, env.local.loggedIn(admin)
}

// PC の管理者は、「サイト運用」の画面で公開サーバーのメンテナンス表示の状態を見て、始める・終えることができる。結果は公開サーバーが返した
// 状態で表示する。PC 自身の公開サイトと設定は変わらない。
func TestRemoteMaintenanceFlow(t *testing.T) {
	env, adm := newRemoteMaintenanceEnv(t)
	env.remote.insertPublished("m-5", "公開サーバーの記事", "remote-article")
	anonRemote := env.remote.newClient()

	page := adm.get(operationsPagePath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "data-remote-maintenance", "公開サーバーのメンテナンス表示", static.URL("js/remote_maintenance.js"),
		`id="remote-maintenance-toggle"`, `id="remote-maintenance-reload"`, "HTTP 503",
		`id="remote-maintenance-file"`, `id="remote-maintenance-local-save"`, `id="remote-maintenance-apply"`,
		"この PC で使う画面", "画面を反映", "標準の画面",
		// CDN のキャッシュとサーバーの停止はこの機能で制御できないことを明記する。
		"CDN", "公開サーバーが停止している", "「公開サーバーへ反映する」")
	// メンテナンス表示は「サイト運用」へ移し、「公開サーバー」（接続・参加・招待）の画面には出さない（SPEC §2-6）。
	expectNotContains(t, adm.get("/admin/settings/remote").Body, "data-remote-maintenance", static.URL("js/remote_maintenance.js"))
	script := adm.get(static.URL("js/remote_maintenance.js"))
	expectStatus(t, script, http.StatusOK)
	expectContains(t, script.Body, "/admin/remote/maintenance", "/admin/remote/maintenance/page", "/admin/remote/maintenance/apply",
		"textContent", "outcome", "confirmStart", "confirmStartDefault")
	expectNotContains(t, script.Body, "innerHTML", "setInterval", "setTimeout")

	res := adm.getJSON("/admin/remote/maintenance", nil)
	expectStatus(t, res, http.StatusOK)
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	off := decodeMaintenanceReply(t, res)
	if off.Enabled || off.Status != maintenanceStatusOff || off.Tone != "neutral" || off.ChangedLabel != "" || off.Summary == "" || off.CheckedLabel == "" {
		t.Fatalf("initial state = %+v", off)
	}
	if off.LocalPage == nil || off.LocalPage.Kind != SyncMaintenancePageDefault || off.Page == nil || off.Page.Kind != SyncMaintenancePageDefault {
		t.Fatalf("initial pages = local %+v remote %+v", off.LocalPage, off.Page)
	}

	on := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
	if on.State == nil || !on.State.Enabled || on.State.Status != maintenanceStatusOn || on.State.Tone != "warn" || !on.Changed ||
		on.State.ChangedLabel == "" || !strings.Contains(on.Message, "メンテナンス表示を始めました") || !strings.Contains(on.Message, "CDN") {
		t.Fatalf("turn on = %+v (state %+v)", on, on.State)
	}
	expectMaintenancePage(t, "remote after turning on", anonRemote.get("/articles/remote-article"), "公開サーバーの記事")
	if st := decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil))); !st.Enabled || st.Status != maintenanceStatusOn || st.ChangedLabel != on.State.ChangedLabel {
		t.Fatalf("status after turning on = %+v", st)
	}

	again := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
	if again.Changed || !again.State.Enabled || !strings.Contains(again.Message, "すでに") {
		t.Fatalf("turn on again = %+v", again)
	}

	stopped := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("0"), nil)))
	if stopped.State == nil || stopped.State.Enabled || stopped.State.Status != maintenanceStatusOff || !stopped.Changed ||
		!strings.Contains(stopped.Message, "メンテナンス表示を終えました") {
		t.Fatalf("turn off = %+v", stopped)
	}
	expectStatus(t, anonRemote.get("/articles/remote-article"), http.StatusOK)

	// PC の公開サイトと設定は変わらない（公開サーバーの状態を PC に保存しない）。
	if n := env.local.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, maintenanceSetting); n != 0 {
		t.Errorf("the PC stored the maintenance setting (%d rows)", n)
	}
	expectNotMaintenance(t, "PC public site", env.local.newClient().get("/"))
}

// メンテナンス表示の画面と API は、ローカルの管理者が管理者のトークンで接続しているときだけ使える。CSRF トークンの無い
// 切り替えは断る。
func TestRemoteMaintenancePermissions(t *testing.T) {
	env, adm := newRemoteMaintenanceEnv(t)
	owner := env.remote.token("check", auth.RoleAdmin)
	for _, u := range []testUser{studentA, chief} {
		c := env.local.loggedIn(u)
		expectStatus(t, c.getJSON("/admin/remote/maintenance", nil), http.StatusForbidden)
		expectStatus(t, c.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), http.StatusForbidden)
		expectStatus(t, c.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil), http.StatusForbidden)
		expectStatus(t, c.postJSON("/admin/remote/maintenance/page", url.Values{"clear": {"1"}}, nil), http.StatusForbidden)
	}
	expectStatus(t, adm.postRaw("/admin/remote/maintenance", maintenanceForm("1")), http.StatusForbidden)
	expectStatus(t, adm.postRaw("/admin/remote/maintenance/apply", url.Values{}), http.StatusForbidden)
	expectStatus(t, adm.postRaw("/admin/remote/maintenance/page", url.Values{"clear": {"1"}}), http.StatusForbidden)
	expectStatus(t, env.local.newClient().getJSON("/admin/remote/maintenance", nil), http.StatusUnauthorized)
	if st := env.remote.getMaintenance(owner); st.Enabled {
		t.Fatal("a request without permission or CSRF token turned maintenance on")
	}

	// 編集者のトークンでは、公開サーバーに問い合わせる前に断り、画面にも出さない。
	env.configure("desk", auth.RoleEditor)
	expectStatus(t, adm.getJSON("/admin/remote/maintenance", nil), http.StatusForbidden)
	expectStatus(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), http.StatusForbidden)
	expectStatus(t, adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil), http.StatusForbidden)
	expectStatus(t, adm.postJSON("/admin/remote/maintenance/page", url.Values{"clear": {"1"}}, nil), http.StatusForbidden)
	expectNotContains(t, adm.get("/admin/settings/remote").Body, "data-remote-maintenance", "remote_maintenance.js")
	if st := env.remote.getMaintenance(owner); st.Enabled {
		t.Fatal("an editor token turned maintenance on")
	}

	// 未設定なら案内を返す。
	if err := env.local.server.saveRemoteConfig(remoteConfig{}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adm.getJSON("/admin/remote/maintenance", nil), http.StatusConflict)
	expectStatus(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), http.StatusConflict)
	expectStatus(t, adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil), http.StatusConflict)
	expectStatus(t, adm.postJSON("/admin/remote/maintenance/page", url.Values{"clear": {"1"}}, nil), http.StatusConflict)
}

// デスクトップ版の PC（利用者はすべての役割を持つ）でも、管理者のトークンで接続していれば確認・切り替えできる。
func TestRemoteMaintenanceFromDesktop(t *testing.T) {
	env := &remoteEnv{local: newTestEnvWith(t, Config{Desktop: true}, nil), remote: newPublicOnlyEnv(t, nil)}
	env.configure("owner", auth.RoleAdmin)
	pc := env.local.loggedIn(admin)
	expectContains(t, pc.get(operationsPagePath).Body, "data-remote-maintenance")
	on := decodeMaintenanceReply(t, expectOK(t, pc.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
	if on.State == nil || !on.State.Enabled {
		t.Fatalf("turn on from the desktop app = %+v", on)
	}
	expectMaintenancePage(t, "remote", env.remote.newClient().get("/"))
	// 自分の PC の公開サイトは止めない。
	expectNotMaintenance(t, "desktop public site", env.local.newClient().get("/"))
}

// メンテナンス表示中も、PC は校閲待ちの記事の画像を公開サーバーから取り込める（同期 API のトークンで取得する）。
func TestRemoteImportDuringMaintenance(t *testing.T) {
	env, _ := newRemoteMaintenanceEnv(t)
	images := localImages(t, 320, 200)
	for _, img := range images {
		env.remote.writeUpload(img.Name, string(img.Data))
	}
	env.remote.setMaintenance(env.remote.token("check", auth.RoleAdmin), true)
	expectMaintenancePage(t, "image without a token", env.remote.newClient().get("/uploads/"+images[0].Name))

	env.configure("desk", auth.RoleEditor)
	cfg := env.local.server.loadRemoteConfig()
	saved, failed := env.local.server.downloadRemoteImages(context.Background(), cfg, "![写真](/uploads/"+images[0].Name+")")
	if failed != 0 || saved != len(images) {
		t.Fatalf("download during maintenance: saved %d, failed %d, want %d saved", saved, failed, len(images))
	}
}

// 切り替え先が無い・形式が違う要求は、公開サーバーに送らずに断る。
func TestRemoteMaintenanceValidation(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	f := newPlainFakeRemote(t, func(SyncRequest) (int, string) { return http.StatusOK, `{"action":"set_maintenance","enabled":true}` })
	configureFake(t, local, f)
	for _, form := range []url.Values{{}, maintenanceForm(""), maintenanceForm("yes"), maintenanceForm("true")} {
		res := adm.postJSON("/admin/remote/maintenance", form, nil)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		if r := decodeMaintenanceReply(t, res); r.Status != maintenanceStatusUnknown || r.Message == "" || r.State != nil {
			t.Fatalf("invalid form %v = %+v", form, r)
		}
	}
	if reqs := f.requests(); reqs != "" {
		t.Fatalf("invalid forms were sent to the public server:\n%s", reqs)
	}
}

// 公開サーバーに問い合わせられない・古い・読めない応答のときは「分からない」と返し、成功とは見せない。切り替えを送った後に
// 応答が無い・読めない・サーバーのエラーのときは、反映されたかどうか分からないと伝え、状態の確認を促す（自動でやり直さない）。
func TestRemoteMaintenanceFailuresAreNeverSuccess(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	expectUnknown := func(what string, res *response, wantMsg string, outcomeUnknown bool) maintenanceReply {
		t.Helper()
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: status %d, want 502\nbody: %s", what, res.StatusCode, res.Body)
		}
		r := decodeMaintenanceReply(t, res)
		if r.Status != maintenanceStatusUnknown || r.Enabled || r.State != nil || r.Changed || r.OutcomeUnknown != outcomeUnknown ||
			!strings.Contains(r.Message, wantMsg) {
			t.Fatalf("%s: reply = %+v, want unknown with %q (outcome unknown %v)", what, r, wantMsg, outcomeUnknown)
		}
		if outcomeUnknown {
			expectContains(t, r.Message, "反映されたかどうか分かりません", "「状態を確かめる」")
		}
		return r
	}
	onState := `{"action":"%s","enabled":true,"changed_at":"2026-11-13T01:00:00Z","changed":true,"page":{"kind":"default"}}`
	state := func(action string) string { return strings.Replace(onState, "%s", action, 1) }

	t.Run("cannot connect", func(t *testing.T) {
		f := newPlainFakeRemote(t, func(SyncRequest) (int, string) { return http.StatusOK, "" })
		configureFake(t, local, f)
		f.srv.Close()
		expectUnknown("GET", adm.getJSON("/admin/remote/maintenance", nil), "接続できませんでした", false)
		expectUnknown("POST", adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), "接続できませんでした", true)
	})

	t.Run("old server", func(t *testing.T) {
		calls := 0
		f := newFakeRemote(t, func(req SyncRequest) (int, string) {
			calls++
			return http.StatusBadRequest, `{"message":"未対応の action です: \"` + req.Action + `\""}`
		})
		configureFake(t, local, f)
		expectUnknown("GET", adm.getJSON("/admin/remote/maintenance", nil), "sorus を更新してください", false)
		r := expectUnknown("POST", adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), "sorus を更新してください", false)
		expectContains(t, r.Message, "切り替わっていません")
		if calls != 2 {
			t.Errorf("requests to the old server = %d, want 2 (no automatic retry)", calls)
		}
	})

	t.Run("unreadable status", func(t *testing.T) {
		f := newFakeRemote(t, func(SyncRequest) (int, string) { return http.StatusOK, `{"action":"get_site_access","enabled":true}` })
		configureFake(t, local, f)
		expectUnknown("GET wrong action", adm.getJSON("/admin/remote/maintenance", nil), "読み取れませんでした", false)
		f.handle = func(SyncRequest) (int, string) { return http.StatusOK, `<html>proxy</html>` }
		expectUnknown("GET html", adm.getJSON("/admin/remote/maintenance", nil), "読み取れませんでした", false)
	})

	for _, tc := range []struct {
		name, msg string
		status    int
		body      string
	}{
		{"dropped after sending", "接続できませんでした", fakeDropConnection, ""},
		{"server error", "公開サーバーでエラーが発生しました", http.StatusInternalServerError, ""},
		{"bad gateway", "公開サーバーでエラーが発生しました", http.StatusBadGateway, ""},
		{"unreadable result", "読み取れませんでした", http.StatusOK, `{"action":"get_maintenance","enabled":true}`},
		{"not json", "読み取れませんでした", http.StatusOK, `<html>ok</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f := newFakeRemote(t, func(req SyncRequest) (int, string) {
				calls++
				if req.Maintenance == nil || req.Maintenance.Enabled == nil || !*req.Maintenance.Enabled {
					t.Errorf("request = %+v, want explicit enabled=true", req)
				}
				return tc.status, tc.body
			})
			configureFake(t, local, f)
			expectUnknown("POST", adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), tc.msg, true)
			if calls != 1 {
				t.Errorf("requests = %d, want 1 (no automatic retry)", calls)
			}
		})
	}

	t.Run("rejected by the server", func(t *testing.T) {
		f := newFakeRemote(t, func(SyncRequest) (int, string) { return http.StatusUnauthorized, `{"message":"Unauthorized"}` })
		configureFake(t, local, f)
		r := expectUnknown("POST", adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), "トークンが公開サーバーで認められませんでした", false)
		expectContains(t, r.Message, "切り替わっていません")
	})

	t.Run("server reports another state", func(t *testing.T) {
		f := newFakeRemote(t, func(req SyncRequest) (int, string) {
			return http.StatusOK, `{"action":"set_maintenance","enabled":false,"changed":false,"page":{"kind":"default"}}`
		})
		configureFake(t, local, f)
		res := adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)
		expectStatus(t, res, http.StatusBadGateway)
		r := decodeMaintenanceReply(t, res)
		// 公開サーバーが返した状態（オフ）を表示し、始めたとは言わない。
		if r.State == nil || r.State.Enabled || r.State.Status != maintenanceStatusOff || r.Changed || !strings.Contains(r.Message, "切り替わっていません") {
			t.Fatalf("mismatched state = %+v (state %+v)", r, r.State)
		}
	})

	t.Run("success uses the reported state", func(t *testing.T) {
		f := newFakeRemote(t, func(req SyncRequest) (int, string) { return http.StatusOK, state(req.Action) })
		configureFake(t, local, f)
		r := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
		if r.State == nil || !r.State.Enabled || !strings.Contains(r.State.ChangedLabel, "2026/11/13 10:00:00") {
			t.Fatalf("reply = %+v (state %+v)", r, r.State)
		}
	})
}

// --- テーマに同梱したメンテナンス画面（maintenance.html） ---

const themeMaintMarker = "theme-bundled-maint"

// maintThemeFiles は maintenance.html を同梱した独自テーマのファイル。
func maintThemeFiles(id, nameJA string) map[string]string {
	files := testThemeFiles(id, nameJA)
	files[theme.MaintenanceFile] = `<!doctype html><html lang="ja"><head><meta charset="utf-8"><title>テーマの工事中</title></head>` +
		`<body><p>` + themeMaintMarker + `</p></body></html>`
	return files
}

// expectDownloadedMaintenance は res がメンテナンス画面のダウンロード（maintenance.html の添付）で、出どころが source かを確かめる。
func expectDownloadedMaintenance(t *testing.T, res *response, source string, want ...string) {
	t.Helper()
	expectStatus(t, res, http.StatusOK)
	h := res.Header
	if got := h.Get("Content-Disposition"); got != `attachment; filename="maintenance.html"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := h.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("Content-Security-Policy = %q, want sandbox", got)
	}
	if got := h.Get(maintenanceSourceHeader); got != source {
		t.Errorf("%s = %q, want %q", maintenanceSourceHeader, got, source)
	}
	expectContains(t, res.Body, want...)
}

// 適用中の独自テーマに同梱した maintenance.html は、「公開サーバーへ反映する」でテーマといっしょに公開サーバーへ送られ、
// 独自 HTML を選んでいないメンテナンス表示で訪問者に出る。独自 HTML を送るとそちらを優先し、標準に戻すとテーマの画面に戻る。
// maintenance.html は /custom-themes/ の下からは配信しない。
func TestMaintenanceUsesThemeBundledPage(t *testing.T) {
	env, pcThemes, vpsThemes := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	env.remote.insertPublished("m-theme", "公開の記事", "theme-article")
	reader := env.remote.newClient()

	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", maintThemeFiles("paper", "ペーパー"))),
		"installed", "paper", "テーマ「ペーパー」を取り込みました")
	if _, err := os.Stat(filepath.Join(pcThemes, "paper", theme.MaintenanceFile)); err != nil {
		t.Fatalf("maintenance.html was not installed on the PC: %v", err)
	}
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"paper"}}), "/admin/settings/theme")
	pushRemoteSettings(t, adm, http.StatusOK)
	if _, err := os.Stat(filepath.Join(vpsThemes, "paper", theme.MaintenanceFile)); err != nil {
		t.Fatalf("maintenance.html was not sent to the public server: %v", err)
	}
	// HTML はテーマのファイルとして配信しない（PC・公開サーバーとも）。
	for _, c := range []*client{env.local.newClient(), reader} {
		res := c.get("/custom-themes/paper/" + theme.MaintenanceFile)
		expectStatus(t, res, http.StatusNotFound)
		expectNotContains(t, res.Body, themeMaintMarker)
	}

	// 独自 HTML を選ばずに始めると、テーマの画面を出す。
	local := decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if lp := local.LocalPage; lp == nil || lp.Source != maintenanceSourceTheme || lp.ThemeName != "ペーパー" ||
		!strings.Contains(lp.Label, "ペーパー") || lp.SourceNote == "" {
		t.Fatalf("local page with a bundled theme page = %+v", local.LocalPage)
	}
	on := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
	if on.State == nil || !on.State.Enabled || on.State.Page == nil || on.State.Page.Kind != SyncMaintenancePageDefault ||
		!strings.Contains(on.State.Page.Label, "テーマ") || !strings.Contains(on.Message, "テーマ同梱") {
		t.Fatalf("start with the theme page = %+v (state %+v)", on, on.State)
	}
	for _, p := range []string{"/", "/articles/theme-article", "/custom-themes/paper/" + theme.MaintenanceFile} {
		expectCustomMaintenancePage(t, p, reader.get(p), themeMaintMarker, "公開の記事", maintenanceTitleJA)
	}

	// 独自 HTML（サイト運用で個別に選んだもの）はテーマの画面より優先する。
	expectOK(t, adm.postMaintenanceHTML("campaign.html", []byte(customMaintHTML(customMaintMarker)), nil))
	// 反映する前は、公開サーバーはテーマの画面のまま（PC で独自 HTML を選んでいても、そう表示する）。
	before := decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if before.Page == nil || !strings.Contains(before.Page.Label, "テーマ") || before.Page.MatchesLocal {
		t.Fatalf("remote page before applying the custom page = %+v", before.Page)
	}
	expectOK(t, adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil))
	expectCustomMaintenancePage(t, "custom over theme", reader.get("/"), customMaintMarker, themeMaintMarker)

	// 標準に戻すと、テーマの画面に戻る。
	expectOK(t, adm.postJSON("/admin/remote/maintenance/page", url.Values{"clear": {"1"}}, nil))
	applied := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil)))
	if !strings.Contains(applied.Message, "テーマ同梱") {
		t.Errorf("apply message = %q", applied.Message)
	}
	expectCustomMaintenancePage(t, "theme again", reader.get("/"), themeMaintMarker, customMaintMarker)

	// 標準テーマに戻して反映すると、標準画面に戻る。
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"modern"}}), "/admin/settings/theme")
	pushRemoteSettings(t, adm, http.StatusOK)
	expectMaintenancePage(t, "builtin theme", reader.get("/"), themeMaintMarker)
}

// 不正な maintenance.html（UTF-8 でない・1 MiB を超える）を同梱したテーマは取り込まず、理由を通知する。
func TestThemeImportRejectsInvalidMaintenanceHTML(t *testing.T) {
	env, pcThemes, _ := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	for name, body := range map[string]string{
		"not utf-8": "<p>\xff\xfe</p>",
		"too large": "<p>" + strings.Repeat("a", theme.MaxMaintenanceSize) + "</p>",
	} {
		t.Run(name, func(t *testing.T) {
			files := testThemeFiles("paper", "ペーパー")
			files[theme.MaintenanceFile] = body
			expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", files)),
				"invalid", "", "maintenance.html")
			if _, err := os.Stat(filepath.Join(pcThemes, "paper")); !os.IsNotExist(err) {
				t.Fatalf("an invalid theme was installed: %v", err)
			}
		})
	}
}

// 「現在のメンテナンス画面をダウンロード」は、この PC で有効な画面（独自 HTML → 適用中のテーマの maintenance.html → 標準画面）を
// maintenance.html として返す。公開サーバーは変えない。サイト運用の画面には、ダウンロードの導線とテーマ同梱の説明を出す。
func TestRemoteMaintenancePageDownload(t *testing.T) {
	env, _, _ := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	const download = "/admin/remote/maintenance/page/download"

	expectDownloadedMaintenance(t, adm.get(download), maintenanceSourceDefault, maintenanceTitleJA, `name="robots" content="noindex"`)
	page := adm.get(operationsPagePath).Body
	expectContains(t, page, `id="remote-maintenance-download-link"`, `href="`+download+`"`, "現在のメンテナンス画面をダウンロード")
	expectContains(t, page, `id="remote-maintenance-local-source" class=`)

	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", maintThemeFiles("paper", "ペーパー"))),
		"installed", "paper", "テーマ「ペーパー」を取り込みました")
	// 取り込んだだけ（適用していない）では使わない。
	expectDownloadedMaintenance(t, adm.get(download), maintenanceSourceDefault, maintenanceTitleJA)
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"paper"}}), "/admin/settings/theme")
	expectDownloadedMaintenance(t, adm.get(download), maintenanceSourceTheme, themeMaintMarker)
	page = adm.get(operationsPagePath).Body
	expectContains(t, page, "テーマ「ペーパー」の画面（maintenance.html）", `data-local-source="theme"`, `data-local-theme="ペーパー"`)

	saved := decodeMaintenanceReply(t, expectOK(t, adm.postMaintenanceHTML("campaign.html", []byte(customMaintHTML(customMaintMarker)), nil)))
	if lp := saved.LocalPage; lp == nil || lp.Source != maintenanceSourceCustom || lp.ThemeName != "ペーパー" ||
		!strings.Contains(lp.SourceNote, "優先") {
		t.Fatalf("local page after saving a custom page = %+v", saved.LocalPage)
	}
	res := adm.get(download)
	expectDownloadedMaintenance(t, res, maintenanceSourceCustom, customMaintMarker)
	expectNotContains(t, res.Body, themeMaintMarker)

	// 英語表示では標準画面も英語。
	expectOK(t, adm.postJSON("/admin/remote/maintenance/page", url.Values{"clear": {"1"}}, nil))
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"modern"}}), "/admin/settings/theme")
	adm.useEnglish()
	expectDownloadedMaintenance(t, adm.get(download), maintenanceSourceDefault, `<html lang="en">`)
	expectContains(t, adm.get(operationsPagePath).Body, "Download the current maintenance page", `download="maintenance.html"`)

	if st := env.remote.getMaintenance(env.remote.token("check", auth.RoleAdmin)); st.Enabled ||
		(st.Page != nil && st.Page.Kind == SyncMaintenancePageCustom) {
		t.Fatalf("downloading changed the public server: %+v", st)
	}
}

// ダウンロードも、メンテナンス表示のほかの操作と同じく、ローカルの管理者が管理者のトークンで接続しているときだけ。
func TestRemoteMaintenancePageDownloadPermissions(t *testing.T) {
	env, adm := newRemoteMaintenanceEnv(t)
	const download = "/admin/remote/maintenance/page/download"
	for _, u := range []testUser{studentA, chief} {
		expectStatus(t, env.local.loggedIn(u).getJSON(download, nil), http.StatusForbidden)
	}
	res := env.local.newClient().getJSON(download, nil)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("anonymous download: status %d", res.StatusCode)
	}
	env.configure("desk", auth.RoleEditor)
	expectStatus(t, adm.getJSON(download, nil), http.StatusForbidden)
	if err := env.local.server.saveRemoteConfig(remoteConfig{}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adm.getJSON(download, nil), http.StatusConflict)
}
