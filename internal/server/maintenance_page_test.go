package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

const customMaintMarker = "engineer-page-marker"

func customMaintHTML(marker string) string {
	return `<!doctype html><html lang="ja"><head><meta charset="utf-8"><title>工事中</title></head>` +
		`<body><h1>独自メンテ</h1><p>` + marker + `</p></body></html>`
}

func customMaintPage(name, html string) *SyncMaintenancePage {
	return &SyncMaintenancePage{Kind: SyncMaintenancePageCustom, Name: name, HTML: html}
}

func defaultMaintPage() *SyncMaintenancePage {
	return &SyncMaintenancePage{Kind: SyncMaintenancePageDefault}
}

func maintenanceHTMLSHA(html string) string {
	sum := sha256.Sum256([]byte(html))
	return hex.EncodeToString(sum[:])
}

func (e *syncEnv) setMaintenanceWithPage(token string, on bool, page *SyncMaintenancePage) SyncMaintenanceResponse {
	e.t.Helper()
	res := e.maintenanceHTTP(token, SyncRequest{Action: SyncActionSetMaintenance,
		Maintenance: &SyncMaintenanceUpdate{Enabled: boolRef(on), Page: page}})
	expectStatus(e.t, res, http.StatusOK)
	return decodeResponse[SyncMaintenanceResponse](e.t, res)
}

func (e *syncEnv) applyMaintenancePage(token string, page *SyncMaintenancePage) *response {
	e.t.Helper()
	return e.maintenanceHTTP(token, SyncRequest{Action: SyncActionSetMaintenance,
		Maintenance: &SyncMaintenanceUpdate{Page: page}})
}

func expectCustomMaintenancePage(t *testing.T, what string, res *response, marker string, unwanted ...string) {
	t.Helper()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%s: status %d, want 503\nbody: %.1000s", what, res.StatusCode, res.Body)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", what, got)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Errorf("%s: Content-Type = %q, want text/html", what, res.Header.Get("Content-Type"))
	}
	if res.Request.Method != http.MethodHead {
		expectContains(t, res.Body, marker)
		expectNotContains(t, res.Body, unwanted...)
	}
}

func (c *client) postMaintenanceHTML(filename string, data []byte, extra url.Values) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			c.t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			c.t.Fatal(err)
		}
	}
	for k, vs := range extra {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				c.t.Fatal(err)
			}
		}
	}
	if err := mw.Close(); err != nil {
		c.t.Fatal(err)
	}
	return c.postAPI("/admin/remote/maintenance/page", mw.FormDataContentType(), &buf)
}

// 独自 HTML で始めると訪問者にその HTML を 503 で返し、表示中に別の HTML を画面だけ更新でき、終えると元に戻る。
// 画面だけの更新では ON/OFF は変わらない。
func TestSyncMaintenanceCustomPageLifecycle(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	remote.insertPublished("m-html-1", "見せない記事", "hidden-html")
	owner := remote.token("owner", auth.RoleAdmin)
	anon := remote.newClient()
	html1 := customMaintHTML(customMaintMarker)
	html2 := customMaintHTML("second-engineer-page")

	on := remote.setMaintenanceWithPage(owner, true, customMaintPage("campaign.html", html1))
	if !on.Enabled || !on.Changed || !on.PageChanged || on.Page == nil || on.Page.Kind != SyncMaintenancePageCustom ||
		on.Page.Name != "campaign.html" || on.Page.SHA256 != maintenanceHTMLSHA(html1) {
		t.Fatalf("start with custom = %+v", on)
	}
	expectCustomMaintenancePage(t, "custom on", anon.get("/articles/hidden-html"), customMaintMarker, "見せない記事", maintenanceTitleJA)
	got := remote.getMaintenance(owner)
	if !got.Enabled || got.Page == nil || got.Page.SHA256 != on.Page.SHA256 || got.PageChanged {
		t.Fatalf("get after custom on = %+v", got)
	}

	applied := decodeResponse[SyncMaintenanceResponse](t, expectOK(t, remote.applyMaintenancePage(owner, customMaintPage("next.html", html2))))
	if !applied.Enabled || applied.Changed || !applied.PageChanged || applied.Page.Name != "next.html" ||
		applied.Page.SHA256 != maintenanceHTMLSHA(html2) {
		t.Fatalf("apply while on = %+v", applied)
	}
	expectCustomMaintenancePage(t, "updated custom", anon.get("/"), "second-engineer-page", customMaintMarker, maintenanceTitleJA)
	if got := remote.getMaintenance(owner); !got.Enabled || got.ChangedAt == nil || !got.ChangedAt.Equal(*on.ChangedAt) {
		t.Fatalf("apply changed ON/OFF timestamp: %+v vs %+v", got, on)
	}

	same := decodeResponse[SyncMaintenanceResponse](t, expectOK(t, remote.applyMaintenancePage(owner, customMaintPage("next.html", html2))))
	if same.Changed || same.PageChanged || !same.Enabled {
		t.Fatalf("apply same page = %+v", same)
	}

	off := remote.setMaintenanceWithPage(owner, false, customMaintPage("ignored.html", html1))
	if off.Enabled || !off.Changed || off.PageChanged {
		t.Fatalf("stop should ignore page: %+v", off)
	}
	expectStatus(t, anon.get("/articles/hidden-html"), http.StatusOK)
	if got := remote.getMaintenance(owner); got.Enabled || got.Page == nil || got.Page.Kind != SyncMaintenancePageCustom ||
		got.Page.SHA256 != maintenanceHTMLSHA(html2) {
		t.Fatalf("stored page after stop = %+v", got)
	}

	def := remote.setMaintenanceWithPage(owner, true, defaultMaintPage())
	if !def.Enabled || !def.PageChanged || def.Page == nil || def.Page.Kind != SyncMaintenancePageDefault {
		t.Fatalf("start with default = %+v", def)
	}
	expectMaintenancePage(t, "default after custom", anon.get("/articles/hidden-html"), "見せない記事")
}

// 画面だけの要求は表示中でなければ断り、ON/OFF は変えない。
func TestSyncMaintenanceApplyPageDoesNotChangeMode(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	html1 := customMaintHTML(customMaintMarker)
	res := remote.applyMaintenancePage(owner, customMaintPage("campaign.html", html1))
	expectSiteAccessCode(t, res, http.StatusConflict, maintenanceCodePageNotActive)
	if st := remote.getMaintenance(owner); st.Enabled || (st.Page != nil && st.Page.Kind == SyncMaintenancePageCustom) {
		t.Fatalf("apply while off changed state: %+v", st)
	}
	expectStatus(t, remote.newClient().get("/"), http.StatusOK)

	remote.setMaintenance(owner, true)
	res = remote.applyMaintenancePage(owner, customMaintPage("campaign.html", html1))
	out := decodeResponse[SyncMaintenanceResponse](t, expectOK(t, res))
	if !out.Enabled || out.Changed || !out.PageChanged {
		t.Fatalf("apply while on = %+v", out)
	}
}

// サイズ・UTF-8・空・危険なファイル名・NUL は断り、何も変えない。
func TestSyncMaintenanceCustomPageValidation(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	okHTML := customMaintHTML(customMaintMarker)
	remote.setMaintenanceWithPage(owner, true, customMaintPage("ok.html", okHTML))
	tooBig := strings.Repeat("a", maxMaintenancePageBytes+1)
	for _, tc := range []struct {
		name string
		page *SyncMaintenancePage
	}{
		{"empty kind", &SyncMaintenancePage{Kind: "", HTML: okHTML, Name: "a.html"}},
		{"bad kind", &SyncMaintenancePage{Kind: "file", HTML: okHTML, Name: "a.html"}},
		{"default with html", &SyncMaintenancePage{Kind: SyncMaintenancePageDefault, HTML: okHTML}},
		{"empty html", customMaintPage("a.html", "")},
		{"whitespace html", customMaintPage("a.html", "   \n")},
		{"not utf-8", customMaintPage("a.html", string([]byte{0xff, 0xfe, 0x00, 0x41}))},
		{"nul", customMaintPage("a.html", "<html>\x00</html>")},
		{"too large", customMaintPage("a.html", tooBig)},
		{"path name", customMaintPage("../x.html", okHTML)},
		{"absolute name", customMaintPage("/tmp/x.html", okHTML)},
		{"no extension", customMaintPage("page", okHTML)},
		{"txt", customMaintPage("page.txt", okHTML)},
		{"empty name", customMaintPage("", okHTML)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := remote.maintenanceHTTP(owner, SyncRequest{Action: SyncActionSetMaintenance,
				Maintenance: &SyncMaintenanceUpdate{Page: tc.page}})
			expectSiteAccessCode(t, res, http.StatusBadRequest, maintenanceCodePageInvalid)
		})
	}
	if st := remote.getMaintenance(owner); !st.Enabled || st.Page == nil || st.Page.SHA256 != maintenanceHTMLSHA(okHTML) {
		t.Fatalf("invalid pages changed the stored page: %+v", st)
	}
	expectCustomMaintenancePage(t, "still custom", remote.newClient().get("/"), customMaintMarker)
}

// 再起動しても独自画面と ON を続ける。
func TestSyncMaintenanceCustomPagePersistsAcrossRestart(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	html1 := customMaintHTML(customMaintMarker)
	on := remote.setMaintenanceWithPage(owner, true, customMaintPage("keep.html", html1))

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
	rec := httptest.NewRecorder()
	restarted.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), customMaintMarker) ||
		strings.Contains(rec.Body.String(), maintenanceTitleJA) {
		t.Fatalf("after restart: status %d body %.500s", rec.Code, rec.Body.String())
	}
	b, _ := json.Marshal(SyncRequest{Action: SyncActionGetMaintenance})
	req := httptest.NewRequest(http.MethodPost, remote.server.cfg.SyncAPIPath, bytes.NewReader(b))
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(rec, req)
	st := decodeSync[SyncMaintenanceResponse](t, rec)
	if !st.Enabled || st.Page == nil || st.Page.SHA256 != on.Page.SHA256 || st.Page.Name != "keep.html" {
		t.Fatalf("get after restart = %+v", st)
	}
}

// 通常の反映と閲覧制限の変更は、独自画面も ON/OFF も変えない。Page なしでオンにするときは標準画面になる。
func TestSyncSettingsDoesNotChangeMaintenancePage(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	html1 := customMaintHTML(customMaintMarker)
	on := remote.setMaintenanceWithPage(owner, true, customMaintPage("keep.html", html1))
	raw := map[string]any{"action": SyncActionSyncSettings, "settings": map[string]any{
		"site_name": "反映したサイト", "maintenance_page": "nope",
	}}
	expectStatus(t, remote.maintenanceHTTP(owner, raw), http.StatusOK)
	if st := remote.getMaintenance(owner); !st.Enabled || st.Page == nil || st.Page.SHA256 != on.Page.SHA256 {
		t.Fatalf("sync_settings changed the page: %+v", st)
	}
	expectCustomMaintenancePage(t, "after settings", remote.newClient().get("/"), customMaintMarker)

	// Page 無しでオフ→オンは標準画面。
	remote.setMaintenance(owner, false)
	plain := remote.setMaintenance(owner, true)
	if plain.Page == nil || plain.Page.Kind != SyncMaintenancePageDefault || !plain.PageChanged {
		t.Fatalf("old client start = %+v", plain)
	}
	expectMaintenancePage(t, "old client default", remote.newClient().get("/"), customMaintMarker)
}

// 復元すると、ON/OFF と独自画面の両方を復元したデータに揃える。
func TestSyncMaintenanceCustomPageRestore(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	remote.insertPublished("m-html-r", "復元後の記事", "restored-article")
	owner := remote.token("owner", auth.RoleAdmin)
	html1 := customMaintHTML(customMaintMarker)
	remote.setMaintenanceWithPage(owner, true, customMaintPage("keep.html", html1))
	if _, err := remote.db.Exec(`UPDATE settings SET value = '0' WHERE key = ?`, maintenanceSetting); err != nil {
		t.Fatal(err)
	}
	if err := remote.server.reloadAfterRestore(context.Background()); err != nil {
		t.Fatalf("reloadAfterRestore: %v", err)
	}
	expectStatus(t, remote.newClient().get("/articles/restored-article"), http.StatusOK)
	if st := remote.getMaintenance(owner); st.Enabled {
		t.Fatalf("enabled after restore = %+v", st)
	}

	remote.setMaintenanceWithPage(owner, true, customMaintPage("keep.html", html1))
	empty, _ := json.Marshal(storedMaintenancePage{})
	if _, err := remote.db.Exec(`UPDATE settings SET value = ? WHERE key = ?`, string(empty), maintenancePageSetting); err != nil {
		t.Fatal(err)
	}
	if err := remote.server.reloadAfterRestore(context.Background()); err != nil {
		t.Fatalf("reload page: %v", err)
	}
	expectMaintenancePage(t, "page restored to default", remote.newClient().get("/"), customMaintMarker)
}

// PC に HTML を保存しても公開サーバーは変わらない。開始で保存済み画面を送り、表示中は「画面を反映」で更新する。
func TestRemoteMaintenanceLocalPageAndApply(t *testing.T) {
	env, adm := newRemoteMaintenanceEnv(t)
	env.remote.insertPublished("m-html-pc", "公開の記事", "remote-html")
	anon := env.remote.newClient()
	html1 := customMaintHTML(customMaintMarker)
	html2 := customMaintHTML("applied-second")

	saved := decodeMaintenanceReply(t, expectOK(t, adm.postMaintenanceHTML("campaign.html", []byte(html1), nil)))
	if saved.LocalPage == nil || saved.LocalPage.Kind != SyncMaintenancePageCustom || saved.LocalPage.Name != "campaign.html" ||
		!strings.Contains(saved.Message, "この PC に保存しました") {
		t.Fatalf("local save = %+v", saved)
	}
	expectStatus(t, anon.get("/articles/remote-html"), http.StatusOK)
	if st := env.remote.getMaintenance(env.remote.token("check", auth.RoleAdmin)); st.Enabled || (st.Page != nil && st.Page.Kind == SyncMaintenancePageCustom) {
		t.Fatalf("saving locally changed the public server: %+v", st)
	}
	if n := env.local.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, maintenanceSetting); n != 0 {
		t.Errorf("the PC stored ON/OFF (%d rows)", n)
	}
	if n := env.local.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, maintenancePageSetting); n != 0 {
		t.Errorf("the PC stored the public applied page (%d rows)", n)
	}
	if n := env.local.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, maintenancePageLocalSetting); n != 1 {
		t.Fatalf("the PC did not store the selected page")
	}

	applyOff := decodeMaintenanceReply(t, adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil))
	if applyOff.Status != maintenanceStatusUnknown || !strings.Contains(applyOff.Message, "メンテナンス表示中ではない") {
		t.Fatalf("apply while off = %+v", applyOff)
	}
	expectStatus(t, anon.get("/articles/remote-html"), http.StatusOK)

	on := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
	if on.State == nil || !on.State.Enabled || on.State.Page == nil || on.State.Page.Kind != SyncMaintenancePageCustom ||
		!on.State.Page.MatchesLocal || !strings.Contains(on.Message, "campaign.html") {
		t.Fatalf("start with local custom = %+v (state %+v)", on, on.State)
	}
	expectCustomMaintenancePage(t, "started custom", anon.get("/articles/remote-html"), customMaintMarker, "公開の記事")

	expectOK(t, adm.postMaintenanceHTML("next.html", []byte(html2), nil))
	st := decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if st.LocalPage.Name != "next.html" || st.Page.Name != "campaign.html" || st.Page.MatchesLocal {
		t.Fatalf("mismatch after local change = local %+v remote %+v", st.LocalPage, st.Page)
	}
	applied := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil)))
	if applied.Changed || applied.State == nil || !applied.State.Enabled || applied.State.Page.Name != "next.html" ||
		!applied.State.Page.MatchesLocal || !strings.Contains(applied.Message, "next.html") {
		t.Fatalf("apply = %+v (state %+v)", applied, applied.State)
	}
	expectCustomMaintenancePage(t, "applied second", anon.get("/"), "applied-second", customMaintMarker)

	cleared := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance/page", url.Values{"clear": {"1"}}, nil)))
	if cleared.LocalPage == nil || cleared.LocalPage.Kind != SyncMaintenancePageDefault {
		t.Fatalf("clear local = %+v", cleared)
	}
	expectCustomMaintenancePage(t, "remote unchanged after clear", anon.get("/"), "applied-second")
}

// 不正なファイルは PC に保存せず、公開サーバーにも送らない。
func TestRemoteMaintenanceLocalPageValidation(t *testing.T) {
	env, adm := newRemoteMaintenanceEnv(t)
	ok := []byte(customMaintHTML(customMaintMarker))
	for _, tc := range []struct {
		name, file string
		data       []byte
		status     int
	}{
		{"empty", "a.html", nil, http.StatusUnprocessableEntity},
		{"not utf-8", "a.html", []byte{0xff, 0xfe, 'A'}, http.StatusUnprocessableEntity},
		{"txt", "note.txt", ok, http.StatusUnprocessableEntity},
		{"too large", "a.html", bytes.Repeat([]byte("あ"), maxMaintenancePageBytes), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := adm.postMaintenanceHTML(tc.file, tc.data, nil)
			if res.StatusCode != tc.status {
				t.Fatalf("status %d, want %d\nbody: %s", res.StatusCode, tc.status, res.Body)
			}
			r := decodeMaintenanceReply(t, res)
			if r.Message == "" || r.LocalPage != nil && r.LocalPage.Kind == SyncMaintenancePageCustom {
				t.Fatalf("reply = %+v", r)
			}
		})
	}
	if n := env.local.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, maintenancePageLocalSetting); n != 0 {
		t.Fatalf("invalid uploads were stored (%d rows)", n)
	}
	expectStatus(t, adm.postMaintenanceHTML("", nil, nil), http.StatusBadRequest)
}

// 応答に必要な page が無いときは読み取れない（結果不明）。独自画面の成功とは見せない。
func TestRemoteMaintenanceMissingPageIsUnreadable(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	html1 := customMaintHTML(customMaintMarker)
	placeholder := newFakeRemote(t, func(SyncRequest) (int, string) {
		return http.StatusOK, `{"action":"get_maintenance","enabled":false,"page":{"kind":"default"}}`
	})
	configureFake(t, local, placeholder)
	expectOK(t, adm.postMaintenanceHTML("campaign.html", []byte(html1), nil))

	expectUnreadable := func(t *testing.T, what string, res *response, outcomeUnknown bool) {
		t.Helper()
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: status %d, want 502\nbody: %s", what, res.StatusCode, res.Body)
		}
		r := decodeMaintenanceReply(t, res)
		if r.Status != maintenanceStatusUnknown || r.State != nil || r.OutcomeUnknown != outcomeUnknown ||
			!strings.Contains(r.Message, "読み取れませんでした") {
			t.Fatalf("%s: reply = %+v, want unreadable (outcome unknown %v)", what, r, outcomeUnknown)
		}
		if outcomeUnknown {
			expectContains(t, r.Message, "反映されたかどうか分かりません", "「状態を確かめる」")
		}
		if strings.Contains(r.Message, "独自の画面は反映されていません") || strings.Contains(r.Message, "独自の画面に対応していない") {
			t.Errorf("%s: claimed the missing-page compatibility path: %s", what, r.Message)
		}
	}

	t.Run("GET missing page", func(t *testing.T) {
		f := newFakeRemote(t, func(SyncRequest) (int, string) {
			return http.StatusOK, `{"action":"get_maintenance","enabled":false}`
		})
		configureFake(t, local, f)
		expectUnreadable(t, "GET", adm.getJSON("/admin/remote/maintenance", nil), false)
	})

	t.Run("start missing page", func(t *testing.T) {
		calls := 0
		f := newFakeRemote(t, func(req SyncRequest) (int, string) {
			calls++
			if req.Maintenance == nil || req.Maintenance.Enabled == nil || !*req.Maintenance.Enabled {
				t.Errorf("request = %+v, want enabled=true", req)
			}
			if req.Maintenance.Page == nil || req.Maintenance.Page.Kind != SyncMaintenancePageCustom {
				t.Errorf("the page field should still be sent: %+v", req.Maintenance)
			}
			return http.StatusOK, `{"action":"set_maintenance","enabled":true,"changed":true}`
		})
		configureFake(t, local, f)
		expectUnreadable(t, "POST start", adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil), true)
		if calls != 1 {
			t.Errorf("requests = %d, want 1 (no automatic retry)", calls)
		}
	})

	t.Run("start default page when custom was sent", func(t *testing.T) {
		f := newFakeRemote(t, func(SyncRequest) (int, string) {
			return http.StatusOK, `{"action":"set_maintenance","enabled":true,"changed":true,"page":{"kind":"default"}}`
		})
		configureFake(t, local, f)
		res := adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("status %d, want 502\n%s", res.StatusCode, res.Body)
		}
		r := decodeMaintenanceReply(t, res)
		if r.Status != maintenanceStatusUnknown || r.OutcomeUnknown || !strings.Contains(r.Message, "独自の画面は反映されていません") {
			t.Fatalf("custom start with default page = %+v", r)
		}
	})
}

// 画面の反映（apply）で送信後に切断・5xx・読めない応答のときは結果不明とし、自動ではやり直さない。
// v0.2.8 の未対応 action は切り替わっていない（結果不明ではない）。
func TestRemoteMaintenanceApplyFailuresAreNeverSuccess(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	placeholder := newFakeRemote(t, func(SyncRequest) (int, string) {
		return http.StatusOK, `{"action":"get_maintenance","enabled":true,"page":{"kind":"default"}}`
	})
	configureFake(t, local, placeholder)

	expectApplyUnknown := func(t *testing.T, what string, res *response, wantMsg string, outcomeUnknown bool) maintenanceReply {
		t.Helper()
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: status %d, want 502\nbody: %s", what, res.StatusCode, res.Body)
		}
		r := decodeMaintenanceReply(t, res)
		if r.Status != maintenanceStatusUnknown || r.State != nil || r.Changed || r.OutcomeUnknown != outcomeUnknown ||
			!strings.Contains(r.Message, wantMsg) {
			t.Fatalf("%s: reply = %+v, want unknown with %q (outcome unknown %v)", what, r, wantMsg, outcomeUnknown)
		}
		if outcomeUnknown {
			expectContains(t, r.Message, "画面が反映されたかどうか分かりません", "「状態を確かめる」")
		}
		return r
	}

	for _, tc := range []struct {
		name, msg string
		status    int
		body      string
	}{
		{"dropped after sending", "接続できませんでした", fakeDropConnection, ""},
		{"server error", "公開サーバーでエラーが発生しました", http.StatusInternalServerError, ""},
		{"unreadable result", "読み取れませんでした", http.StatusOK, `{"action":"get_maintenance","enabled":true}`},
		{"missing page", "読み取れませんでした", http.StatusOK, `{"action":"set_maintenance","enabled":true,"changed":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f := newFakeRemote(t, func(req SyncRequest) (int, string) {
				calls++
				if req.Maintenance != nil && req.Maintenance.Enabled != nil {
					t.Errorf("apply sent enabled: %+v", req.Maintenance)
				}
				return tc.status, tc.body
			})
			configureFake(t, local, f)
			expectApplyUnknown(t, "POST apply", adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil), tc.msg, true)
			if calls != 1 {
				t.Errorf("requests = %d, want 1 (no automatic retry)", calls)
			}
		})
	}

	t.Run("unsupported action", func(t *testing.T) {
		calls := 0
		f := newFakeRemote(t, func(req SyncRequest) (int, string) {
			calls++
			return http.StatusBadRequest, `{"message":"未対応の action です: \"` + req.Action + `\""}`
		})
		configureFake(t, local, f)
		r := expectApplyUnknown(t, "POST apply", adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil), "sorus を更新してください", false)
		expectContains(t, r.Message, "画面は反映していません")
		if calls != 1 {
			t.Errorf("requests = %d, want 1 (no automatic retry)", calls)
		}
	})
}

// ほかの PC が先に独自画面を出していても、開始（すでにオン）では上書きせず、明示的な反映だけが更新する。
func TestRemoteMaintenanceDoesNotOverwriteOnAlreadyOn(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	htmlA := customMaintHTML("from-pc-a")
	htmlB := customMaintHTML("from-pc-b")
	owner := remote.token("owner", auth.RoleAdmin)
	remote.setMaintenanceWithPage(owner, true, customMaintPage("a.html", htmlA))

	pcB, admB := newRemoteMaintenanceEnv(t)
	pcB.remote = remote
	if err := pcB.local.server.saveRemoteConfig(remoteConfig{URL: remote.srv.URL, SyncPath: remote.server.cfg.SyncAPIPath,
		Token: owner}); err != nil {
		t.Fatal(err)
	}
	expectOK(t, admB.postMaintenanceHTML("b.html", []byte(htmlB), nil))
	again := decodeMaintenanceReply(t, expectOK(t, admB.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
	if again.Changed || again.State.Page == nil || again.State.Page.Name != "a.html" || again.State.Page.MatchesLocal {
		t.Fatalf("already on should keep PC A's page: %+v (state %+v)", again, again.State)
	}
	expectCustomMaintenancePage(t, "still A", remote.newClient().get("/"), "from-pc-a", "from-pc-b")

	applied := decodeMaintenanceReply(t, expectOK(t, admB.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil)))
	if !applied.State.Enabled || applied.State.Page.Name != "b.html" || applied.Changed {
		t.Fatalf("apply from B = %+v", applied)
	}
	expectCustomMaintenancePage(t, "now B", remote.newClient().get("/"), "from-pc-b", "from-pc-a")
}

// 管理付きサーバー（自分もメンテ ON）で、この PC の HTML 選択は訪問者向け画面を変えない。逆に公開側への反映も選択を変えない。
// 再起動・復元でも二つの設定は独立する。
func TestManagedServerLocalPageDoesNotChangePublicPage(t *testing.T) {
	managed := newSyncEnv(t)
	other := newPublicOnlyEnv(t, nil)
	if err := managed.server.saveRemoteConfig(remoteConfig{URL: other.srv.URL, SyncPath: other.server.cfg.SyncAPIPath,
		Token: other.token("peer", auth.RoleAdmin)}); err != nil {
		t.Fatal(err)
	}
	htmlPub := customMaintHTML("public-applied")
	htmlLocal := customMaintHTML("pc-selected")
	htmlFromPC := customMaintHTML("from-other-pc")
	owner := managed.token("owner", auth.RoleAdmin)
	managed.setMaintenanceWithPage(owner, true, customMaintPage("public.html", htmlPub))
	expectCustomMaintenancePage(t, "before local save", managed.newClient().get("/"), "public-applied")

	adm := managed.loggedIn(admin)
	saved := decodeMaintenanceReply(t, expectOK(t, adm.postMaintenanceHTML("local.html", []byte(htmlLocal), nil)))
	if saved.LocalPage == nil || saved.LocalPage.Name != "local.html" {
		t.Fatalf("local save = %+v", saved)
	}
	expectCustomMaintenancePage(t, "after local save", managed.newClient().get("/"), "public-applied", "pc-selected")
	if n := managed.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, maintenancePageLocalSetting); n != 1 {
		t.Fatalf("local selection was not stored separately (%d rows)", n)
	}

	st := decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if st.LocalPage == nil || st.LocalPage.Name != "local.html" {
		t.Fatalf("status local page = %+v", st.LocalPage)
	}
	expectCustomMaintenancePage(t, "after status GET", managed.newClient().get("/"), "public-applied", "pc-selected")

	applied := decodeResponse[SyncMaintenanceResponse](t, expectOK(t, managed.applyMaintenancePage(owner, customMaintPage("other.html", htmlFromPC))))
	if applied.Page == nil || applied.Page.Name != "other.html" {
		t.Fatalf("apply onto managed = %+v", applied)
	}
	expectCustomMaintenancePage(t, "after remote apply", managed.newClient().get("/"), "from-other-pc", "pc-selected")
	st = decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if st.LocalPage == nil || st.LocalPage.Name != "local.html" {
		t.Fatalf("remote apply changed local selection: %+v", st.LocalPage)
	}

	restarted, err := New(Config{DB: managed.db, UploadDir: managed.uploadDir, AttachmentDir: managed.attachmentDir,
		DisableScheduler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = restarted.Shutdown(ctx)
	})
	rec := httptest.NewRecorder()
	restarted.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "from-other-pc") ||
		strings.Contains(rec.Body.String(), "pc-selected") {
		t.Fatalf("after restart: status %d body %.500s", rec.Code, rec.Body.String())
	}
	if err := restarted.reloadAfterRestore(context.Background()); err != nil {
		t.Fatalf("reload after restart: %v", err)
	}

	empty, _ := json.Marshal(storedMaintenancePage{})
	if _, err := managed.db.Exec(`UPDATE settings SET value = ? WHERE key = ?`, string(empty), maintenancePageSetting); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reloadAfterRestore(context.Background()); err != nil {
		t.Fatalf("reload public page: %v", err)
	}
	rec = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "from-other-pc") ||
		strings.Contains(rec.Body.String(), "pc-selected") {
		t.Fatalf("after restoring public page: status %d body %.500s", rec.Code, rec.Body.String())
	}
	st = decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if st.LocalPage == nil || st.LocalPage.Name != "local.html" {
		t.Fatalf("restoring the public page changed local selection: %+v", st.LocalPage)
	}

	if _, err := managed.db.Exec(`UPDATE settings SET value = ? WHERE key = ?`, string(empty), maintenancePageLocalSetting); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reloadAfterRestore(context.Background()); err != nil {
		t.Fatalf("reload local page: %v", err)
	}
	rec = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "pc-selected") ||
		strings.Contains(rec.Body.String(), "from-other-pc") {
		t.Fatalf("restoring local selection changed the public page: status %d body %.500s", rec.Code, rec.Body.String())
	}
	st = decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if st.LocalPage == nil || st.LocalPage.Kind != SyncMaintenancePageDefault {
		t.Fatalf("local selection after restore = %+v", st.LocalPage)
	}
}

// 同じ内容の別名は、保存直後も「状態を確かめる」後も一致と扱う。
func TestRemoteMaintenanceSameContentDifferentNameMatchesLocal(t *testing.T) {
	_, adm := newRemoteMaintenanceEnv(t)
	html := customMaintHTML("same-bytes")
	expectOK(t, adm.postMaintenanceHTML("a.html", []byte(html), nil))
	on := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance", maintenanceForm("1"), nil)))
	if on.State == nil || on.State.Page == nil || !on.State.Page.MatchesLocal || on.State.Page.Name != "a.html" {
		t.Fatalf("start = %+v (state %+v)", on, on.State)
	}

	saved := decodeMaintenanceReply(t, expectOK(t, adm.postMaintenanceHTML("b.html", []byte(html), nil)))
	if saved.LocalPage == nil || saved.LocalPage.Name != "b.html" {
		t.Fatalf("save alias = %+v", saved)
	}
	st := decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if st.LocalPage == nil || st.LocalPage.Name != "b.html" || st.Page == nil || st.Page.Name != "a.html" || !st.Page.MatchesLocal {
		t.Fatalf("same content different name after reload = local %+v remote %+v", st.LocalPage, st.Page)
	}

	applied := decodeMaintenanceReply(t, expectOK(t, adm.postJSON("/admin/remote/maintenance/apply", url.Values{}, nil)))
	if applied.State == nil || applied.State.Page == nil || applied.State.Page.Name != "b.html" || !applied.State.Page.MatchesLocal {
		t.Fatalf("apply alias = %+v (state %+v)", applied, applied.State)
	}
	st = decodeMaintenanceReply(t, expectOK(t, adm.getJSON("/admin/remote/maintenance", nil)))
	if st.Page == nil || st.Page.Name != "b.html" || !st.Page.MatchesLocal {
		t.Fatalf("after apply reload = local %+v remote %+v", st.LocalPage, st.Page)
	}
}

func TestParseMaintenanceHTMLRejectsNonUTF8(t *testing.T) {
	if utf8.Valid([]byte{0xff, 0xfe}) {
		t.Fatal("fixture is utf-8")
	}
	_, err := parseCustomMaintenancePage("a.html", []byte{0xff, 0xfe, 0x41})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("err = %v", err)
	}
	page, err := parseCustomMaintenancePage("a.html", append([]byte{0xEF, 0xBB, 0xBF}, []byte(customMaintHTML("bom"))...))
	if err != nil || page.HTML == "" || strings.HasPrefix(page.HTML, "\ufeff") {
		t.Fatalf("BOM: page=%+v err=%v", page, err)
	}
}
