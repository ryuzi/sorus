package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	glog "github.com/labstack/gommon/log"

	"sorus/internal/auth"
)

// 公開サーバー自身の閲覧制限（get_site_access・set_site_access）と、PC の「公開サーバー」画面からの確認・変更の結合テスト。

const (
	remoteAccessUser = "preview-team"
	// remoteAccessPass は応答・ログ・DB に出ていないことを文字列で探すため、ほかに現れない値にする。
	remoteAccessPass  = "Remote-Pass-7e3f9a"
	remoteAccessPass2 = "Second-Pass-41c8d2"
)

func revPtr(s string) *string { return &s }

// siteAccessHTTP は公開サーバーの同期 API に token で body を送る。閲覧制限の変更（set_site_access）は sorus が TLS で
// 直接受けた接続でしか受け付けないため、変更する試験では公開サーバーを TLS で起動する（newPublicOnlyTLSEnv・
// newSyncEnvTLS）。平文の http のサーバーでは site_access_insecure で断られる。
func (e *syncEnv) siteAccessHTTP(token string, body any) *response {
	e.t.Helper()
	return e.newClient().syncHTTP("Bearer "+token, body)
}

func (e *syncEnv) getSiteAccess(token string) SyncSiteAccessResponse {
	e.t.Helper()
	res := e.siteAccessHTTP(token, SyncRequest{Action: SyncActionGetSiteAccess})
	expectStatus(e.t, res, http.StatusOK)
	return decodeResponse[SyncSiteAccessResponse](e.t, res)
}

func (e *syncEnv) setSiteAccess(token string, upd SyncSiteAccessUpdate) *response {
	e.t.Helper()
	return e.siteAccessHTTP(token, SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &upd})
}

// expectSiteAccessCode は res が status で、code の種類のエラーかを確かめる。
func expectSiteAccessCode(t *testing.T, res *response, status int, code string) {
	t.Helper()
	expectStatus(t, res, status)
	var v struct {
		Code, Message string
	}
	if err := json.Unmarshal([]byte(res.Body), &v); err != nil || v.Code != code || v.Message == "" {
		t.Fatalf("error = %s, want code %q with a message", res.Body, code)
	}
}

// expectNoSecrets は body に閲覧用のパスワードとハッシュが含まれないかを確かめる。
func expectNoSecrets(t *testing.T, what, body string, secrets ...string) {
	t.Helper()
	for _, s := range append([]string{remoteAccessPass, remoteAccessPass2, "$2a$", "$2b$", "basic_auth_pass_hash"}, secrets...) {
		if s != "" && strings.Contains(body, s) {
			t.Errorf("%s contains a secret %q:\n%.2000s", what, s, body)
		}
	}
}

// 公開サーバーの閲覧制限を、管理者のトークンで有効化・変更・無効化できる。公開サイト（ページ・フィード・画像）は
// 閲覧用の資格情報が無ければ 401、同期 API はトークンだけで使い続けられる。
func TestSyncSiteAccessLifecycle(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	remote.insertPublished("sa-1", "限定公開の記事", "limited-article")
	remote.writeUpload("0123456789abcdef0123456789abcdef-original.webp", "image")
	owner := remote.token("owner", auth.RoleAdmin)
	anon := remote.newClient()

	st := remote.getSiteAccess(owner)
	if st.Enabled || st.Active || st.PasswordSet || st.User != "" || st.Revision != "" || st.Changed {
		t.Fatalf("initial state = %+v, want off with no revision", st)
	}
	expectStatus(t, anon.get("/"), http.StatusOK)

	// 有効にする。
	res := remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: "  " + remoteAccessUser + " ", Password: remoteAccessPass, ExpectedRevision: revPtr("")})
	expectStatus(t, res, http.StatusOK)
	expectNoSecrets(t, "set_site_access response", res.Body)
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control of the sync response = %q, want no-store", got)
	}
	on := decodeResponse[SyncSiteAccessResponse](t, res)
	if on.Action != SyncActionSetSiteAccess || !on.Enabled || !on.Active || !on.PasswordSet || on.User != remoteAccessUser || on.Revision == "" || !on.Changed {
		t.Fatalf("after enabling = %+v", on)
	}
	if got := remote.getSiteAccess(owner); got.Revision != on.Revision || !got.Active || got.User != remoteAccessUser {
		t.Fatalf("get after enabling = %+v, want revision %s", got, on.Revision)
	}
	hash := remote.setting(basicAuthPassHashSetting)
	if !strings.HasPrefix(hash, "$2") || remote.countRows(`SELECT COUNT(*) FROM settings WHERE value LIKE ?`, "%"+remoteAccessPass+"%") != 0 {
		t.Fatalf("stored hash = %q, or the plain password is stored", hash)
	}
	expectNoSecrets(t, "get_site_access response", remote.siteAccessHTTP(owner, SyncRequest{Action: SyncActionGetSiteAccess}).Body, hash)

	for _, p := range []string{"/", "/articles/limited-article", "/feed.xml", "/uploads/0123456789abcdef0123456789abcdef-original.webp", "/no-such-page"} {
		res := anon.get(p)
		expectBasicAuthChallenge(t, res)
		expectNotContains(t, res.Body, "限定公開の記事")
		expectBasicAuthChallenge(t, anon.getAs(p, browserUA, basicAuthHeader(remoteAccessUser, "wrong-password")))
	}
	good := basicAuthHeader(remoteAccessUser, remoteAccessPass)
	for _, p := range []string{"/", "/articles/limited-article", "/feed.xml", "/uploads/0123456789abcdef0123456789abcdef-original.webp"} {
		res := anon.getAs(p, browserUA, good)
		expectStatus(t, res, http.StatusOK)
		if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
			t.Errorf("%s: Cache-Control = %q, want %q (shared caches must not keep it)", p, got, basicAuthCacheControl)
		}
	}

	// 同期 API は閲覧用の資格情報なしで、トークンだけで使い続けられる。
	editor := remote.token("desk", auth.RoleEditor)
	sub := remote.newClient().syncHTTP("Bearer "+editor, submitRequest(SyncArticle{Title: "制限中の提出", ContentMarkdown: "本文"}))
	expectStatus(t, sub, http.StatusOK)
	expectStatus(t, remote.newClient().syncHTTP("Bearer "+editor, SyncRequest{Action: SyncActionFetchPending}), http.StatusOK)

	// 同じ内容の保存は何も変えず、版もそのまま。
	same := remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, ExpectedRevision: &on.Revision})
	expectStatus(t, same, http.StatusOK)
	if got := decodeResponse[SyncSiteAccessResponse](t, same); got.Changed || got.Revision != on.Revision {
		t.Fatalf("saving the same settings = %+v, want unchanged revision %s", got, on.Revision)
	}

	// パスワード欄を空にしてユーザー名だけ変えると、保存済みのパスワードを使い続ける。
	renamed := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: "reviewers", ExpectedRevision: &on.Revision})))
	if renamed.Revision == on.Revision || !renamed.PasswordSet || remote.setting(basicAuthPassHashSetting) != hash {
		t.Fatalf("rename = %+v, want a new revision and the same password hash", renamed)
	}
	expectBasicAuthChallenge(t, anon.getAs("/", browserUA, good))
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader("reviewers", remoteAccessPass)), http.StatusOK)

	// パスワードを変えると、古いパスワードでは開けない。
	changed := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: "reviewers", Password: remoteAccessPass2, ExpectedRevision: &renamed.Revision})))
	expectBasicAuthChallenge(t, anon.getAs("/", browserUA, basicAuthHeader("reviewers", remoteAccessPass)))
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader("reviewers", remoteAccessPass2)), http.StatusOK)

	// 無効にすると誰でも開ける。ユーザー名とパスワードは残し、有効に戻すときに入れ直さなくてよい。
	off := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: false, User: "reviewers", ExpectedRevision: &changed.Revision})))
	if off.Enabled || off.Active || !off.PasswordSet || off.User != "reviewers" {
		t.Fatalf("after disabling = %+v", off)
	}
	page := anon.get("/articles/limited-article")
	expectStatus(t, page, http.StatusOK)
	expectNotBasicAuthChallenge(t, page, "after disabling")
	back := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: "reviewers", ExpectedRevision: &off.Revision})))
	if !back.Active {
		t.Fatalf("re-enabling with the stored password = %+v", back)
	}
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader("reviewers", remoteAccessPass2)), http.StatusOK)
}

func expectOK(t *testing.T, res *response) *response {
	t.Helper()
	expectStatus(t, res, http.StatusOK)
	return res
}

// 閲覧制限の確認・変更は管理者のトークンだけ。権限が無ければ何も返さず、何も変えない。
func TestSyncSiteAccessPermissions(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	on := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})))
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleEditor} {
		tok := remote.token("member-"+string(role), role)
		get := remote.siteAccessHTTP(tok, SyncRequest{Action: SyncActionGetSiteAccess})
		expectStatus(t, get, http.StatusForbidden)
		expectNotContains(t, get.Body, remoteAccessUser, on.Revision)
		expectStatus(t, remote.setSiteAccess(tok, SyncSiteAccessUpdate{Enabled: false, ExpectedRevision: &on.Revision}), http.StatusForbidden)
	}
	res := remote.newClient().syncHTTP("", SyncRequest{Action: SyncActionGetSiteAccess})
	expectStatus(t, res, http.StatusUnauthorized)
	expectStatus(t, remote.newClient().syncHTTP("Bearer "+tamperRole(t, remote.token("w", auth.RoleWriter), auth.RoleAdmin),
		SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &SyncSiteAccessUpdate{ExpectedRevision: &on.Revision}}), http.StatusUnauthorized)
	// 閲覧用の資格情報では同期 API の権限にならない。
	expectStatus(t, remote.newClient().syncHTTP("Basic "+strings.TrimPrefix(basicAuthHeader(remoteAccessUser, remoteAccessPass)["Authorization"], "Basic "),
		SyncRequest{Action: SyncActionGetSiteAccess}), http.StatusUnauthorized)
	if st := remote.getSiteAccess(owner); st.Revision != on.Revision || !st.Active {
		t.Fatalf("state changed by unauthorized requests: %+v", st)
	}
}

// 入力は「サイト設定」と同じ規則で検査し、誤りがあれば何も変えない。
func TestSyncSiteAccessValidation(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	expectSiteAccessCode(t, remote.siteAccessHTTP(owner, SyncRequest{Action: SyncActionSetSiteAccess}), http.StatusBadRequest, siteAccessCodeMissing)
	expectSiteAccessCode(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass}),
		http.StatusBadRequest, siteAccessCodeRevisionRequired)
	for _, tc := range []struct {
		upd  SyncSiteAccessUpdate
		code string
	}{
		{SyncSiteAccessUpdate{Enabled: true, Password: remoteAccessPass}, errBasicAuthUserRequired.Error()},
		{SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser}, errBasicAuthPassRequired.Error()},
		{SyncSiteAccessUpdate{Enabled: true, User: "a:b", Password: remoteAccessPass}, errBasicAuthUserInvalid.Error()},
		{SyncSiteAccessUpdate{Enabled: true, User: strings.Repeat("u", maxBasicAuthUserRunes+1), Password: remoteAccessPass}, errBasicAuthUserLong.Error()},
		{SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: "pass\nword"}, errBasicAuthPassInvalid.Error()},
		{SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: strings.Repeat("p", maxBasicAuthPassBytes+1)}, errBasicAuthPassLong.Error()},
	} {
		tc.upd.ExpectedRevision = revPtr("")
		res := remote.setSiteAccess(owner, tc.upd)
		expectSiteAccessCode(t, res, http.StatusUnprocessableEntity, tc.code)
		expectNoSecrets(t, "validation error", res.Body)
	}
	if st := remote.getSiteAccess(owner); st.Enabled || st.User != "" || st.PasswordSet || st.Revision != "" {
		t.Fatalf("state changed by invalid input: %+v", st)
	}
	expectStatus(t, remote.newClient().get("/"), http.StatusOK)
}

// 閲覧用のパスワードは TLS で直接届いた要求でだけ受け取る。クライアントが付けた X-Forwarded-Proto は信頼しない
// （転送ヘッダー・Host の偽装は TestReproSiteAccessIgnoresSpoofedHTTPS）。
func TestSyncSiteAccessRequiresHTTPS(t *testing.T) {
	remote := newPublicOnlyEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	body, _ := json.Marshal(SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &SyncSiteAccessUpdate{
		Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}})
	send := func(target, proto string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target+remote.server.cfg.SyncAPIPath, bytes.NewReader(body))
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		if proto != "" {
			req.Header.Set(echo.HeaderXForwardedProto, proto)
		}
		return remote.serve(req)
	}
	for _, proto := range []string{"", "https"} {
		rec := send("http://media.example.jp", proto)
		expectSyncStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), siteAccessCodeInsecure) || remote.getSiteAccess(owner).Enabled {
			t.Fatalf("plain http set_site_access (X-Forwarded-Proto %q): %s", proto, rec.Body.String())
		}
	}
	// httptest.NewRequest は https の URL なら TLS の接続状態を付ける（TLS で直接届いた要求）。
	expectSyncStatus(t, send("https://media.example.jp", ""), http.StatusOK)
	if !remote.getSiteAccess(owner).Active {
		t.Fatal("set_site_access over https did not enable the restriction")
	}
}

// 複数の PC が同じ版から変更すると、先に保存した方だけが通り、後の方は 409 で何も変えない。同時に送っても、
// 通るのは1つだけで、DB とメモリの設定は一致する。
func TestSyncSiteAccessConflict(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	pcA, pcB := remote.getSiteAccess(owner), remote.getSiteAccess(owner)
	first := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: "pc-a", Password: remoteAccessPass, ExpectedRevision: &pcA.Revision})))
	expectSiteAccessCode(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: false, User: "pc-b", ExpectedRevision: &pcB.Revision}),
		http.StatusConflict, siteAccessCodeConflict)
	if st := remote.getSiteAccess(owner); st.User != "pc-a" || !st.Active || st.Revision != first.Revision {
		t.Fatalf("after the conflicting change = %+v, want pc-a's change", st)
	}

	// 同時に同じ版から変更する。
	const n = 8
	body := func(i int) []byte {
		b, _ := json.Marshal(SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &SyncSiteAccessUpdate{
			Enabled: i%2 == 0, User: "racer-" + string(rune('a'+i)), Password: remoteAccessPass2, ExpectedRevision: &first.Revision}})
		return b
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// https の URL の要求は TLS で直接届いた要求として扱われる。
			req := httptest.NewRequest(http.MethodPost, "https://media.example.jp"+remote.server.cfg.SyncAPIPath, bytes.NewReader(body(i)))
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			remote.server.Handler().ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	ok := 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
		default:
			t.Errorf("concurrent set_site_access: status %d", code)
		}
	}
	if ok != 1 {
		t.Fatalf("concurrent set_site_access: %d succeeded (%v), want exactly 1", ok, codes)
	}
	stored, err := loadBasicAuthState(context.Background(), remote.db)
	if err != nil {
		t.Fatal(err)
	}
	g := remote.server.basicAuth.Load()
	if g.basicAuthSettings != stored.basicAuthSettings || stored.Revision == first.Revision {
		t.Fatalf("memory %+v and DB %+v differ after concurrent changes", g.basicAuthSettings, stored)
	}
}

// 公開サーバーを再起動しても、保存した閲覧制限で公開サイトを守り続ける。
func TestSyncSiteAccessPersistsAcrossRestart(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	on := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})))

	restarted, err := New(Config{DB: remote.db, UploadDir: remote.uploadDir, AttachmentDir: remote.attachmentDir, PublicOnly: true,
		SyncPublicKey: remote.server.cfg.SyncPublicKey, SyncAPIPath: remote.server.cfg.SyncAPIPath})
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
	if rec := serve(httptest.NewRequest(http.MethodGet, "/", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after restart: status %d, want 401", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth(remoteAccessUser, remoteAccessPass)
	if rec := serve(req); rec.Code != http.StatusOK {
		t.Fatalf("after restart with credentials: status %d, want 200", rec.Code)
	}
	b, _ := json.Marshal(SyncRequest{Action: SyncActionGetSiteAccess})
	req = httptest.NewRequest(http.MethodPost, remote.server.cfg.SyncAPIPath, bytes.NewReader(b))
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+owner)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := serve(req)
	expectSyncStatus(t, rec, http.StatusOK)
	if st := decodeSync[SyncSiteAccessResponse](t, rec); st.Revision != on.Revision || !st.Active || st.User != remoteAccessUser {
		t.Fatalf("get_site_access after restart = %+v, want revision %s", st, on.Revision)
	}
}

// サイト基本設定の反映（sync_settings）は、閲覧制限の項目を送っても公開サーバーの閲覧制限を変えない。
func TestSyncSettingsDoesNotChangeSiteAccess(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	owner := remote.token("owner", auth.RoleAdmin)
	on := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})))
	hash := remote.setting(basicAuthPassHashSetting)
	raw := map[string]any{"action": SyncActionSyncSettings, "settings": map[string]any{
		"site_name": "反映したサイト", "basic_auth_enabled": false, "basic_auth_user": "", "basic_auth_pass_hash": "",
		"basic_auth_pass": "", "site_access": map[string]any{"enabled": false},
	}, "site_access": map[string]any{"enabled": false, "expected_revision": on.Revision}}
	expectStatus(t, remote.siteAccessHTTP(owner, raw), http.StatusOK)
	st := remote.getSiteAccess(owner)
	if st.Revision != on.Revision || !st.Active || st.User != remoteAccessUser || remote.setting(basicAuthPassHashSetting) != hash {
		t.Fatalf("sync_settings changed the site access: %+v", st)
	}
	if remote.setting("site_name") != "反映したサイト" {
		t.Fatal("sync_settings did not save the site name")
	}
	expectBasicAuthChallenge(t, remote.newClient().get("/"))
	// サイト基本設定の項目に閲覧制限を加えない（通常の反映で公開サーバーの閲覧制限が変わる経路を作らない）。
	typ := reflect.TypeOf(SyncSettings{})
	for i := range typ.NumField() {
		if tag := typ.Field(i).Tag.Get("json"); strings.Contains(tag, "basic_auth") || strings.Contains(tag, "site_access") {
			t.Errorf("SyncSettings has a site access field %q", tag)
		}
	}
}

// 閲覧制限を有効にした公開サーバーでも、PC は校閲待ちの記事の画像を同期 API のトークンで取得できる（取り込みのため）。
// 認めるのは編集者以上の有効なトークンで、画像（/uploads/）の GET・HEAD だけ。
func TestSyncTokenReadsUploadsUnderSiteAccess(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	const name = "0123456789abcdef0123456789abcdef-640.webp"
	remote.writeUpload(name, "image")
	owner := remote.token("owner", auth.RoleAdmin)
	expectOK(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))
	anon := remote.newClient()
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	editor := remote.token("desk", auth.RoleEditor)
	res := anon.send(http.MethodGet, "/uploads/"+name, bearer(editor))
	expectStatus(t, res, http.StatusOK)
	if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
		t.Errorf("Cache-Control of an image read with a token = %q, want %q", got, basicAuthCacheControl)
	}
	expectStatus(t, anon.send(http.MethodHead, "/uploads/"+name, bearer(owner)), http.StatusOK)

	revoked := remote.token("left", auth.RoleEditor)
	expectStatus(t, anon.send(http.MethodGet, "/uploads/"+name, bearer(revoked)), http.StatusOK)
	info, err := parseRemoteToken(revoked, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.db.Exec(`INSERT INTO sync_tokens (jti, subject, role, issued_at, expires_at, issued_via, revoked_at)
		VALUES (?, 'left', 'editor', CURRENT_TIMESTAMP, ?, 'seen', CURRENT_TIMESTAMP)
		ON CONFLICT(jti) DO UPDATE SET revoked_at = CURRENT_TIMESTAMP`, info.ID, info.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	for what, h := range map[string]map[string]string{
		"writer token":  bearer(remote.token("guest", auth.RoleWriter)),
		"revoked token": bearer(revoked),
		"forged token":  bearer(tamperSignature(t, editor)),
		"no token":      nil,
	} {
		res := anon.send(http.MethodGet, "/uploads/"+name, h)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", what, res.StatusCode)
		}
	}
	// 画像以外の公開ページはトークンでは開けない（閲覧用の資格情報が要る）。
	for _, p := range []string{"/", "/feed.xml", "/uploads/../feed.xml", "/uploads/%2e%2e/feed.xml"} {
		if res := anon.send(http.MethodGet, p, bearer(owner)); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a token: status %d, want 401", p, res.StatusCode)
		}
	}
}

// 同期 API を持つ通常モードのサーバーでは、「サイト設定」の保存と set_site_access が同じ版で食い違いを防ぐ。
// パスワード欄が空の保存は、同期 API で変わったパスワードを古い値で上書きしない。
func TestGeneralSettingsSaveSharesSiteAccessRevision(t *testing.T) {
	env := newSyncEnvTLS(t)
	owner := env.token("owner", auth.RoleAdmin)
	adm := env.loggedIn(admin)
	page := adm.get("/admin/settings/general")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="basic-auth-remote-note"`, "「設定」→「サイト運用」で別に設定します")

	on := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, env.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})))
	hash := env.setting(basicAuthPassHashSetting)

	// 閲覧制限を変えずにサイト設定を保存しても、版とパスワードは変わらない。
	form := generalSettingsForm()
	form.Set("basic_auth_enabled", "1")
	form.Set("basic_auth_user", remoteAccessUser)
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if st := env.getSiteAccess(owner); st.Revision != on.Revision || env.setting(basicAuthPassHashSetting) != hash || !st.Active {
		t.Fatalf("saving general settings without changes altered the site access: %+v", st)
	}

	// サイト設定の画面で変えると版が変わり、古い版からの set_site_access は 409 になる。
	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	form.Set("basic_auth_user", "changed-locally")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	st := env.getSiteAccess(owner)
	if st.Revision == on.Revision || st.User != "changed-locally" || env.setting(basicAuthPassHashSetting) != hash {
		t.Fatalf("after changing the general settings = %+v", st)
	}
	expectSiteAccessCode(t, env.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: false, User: remoteAccessUser, ExpectedRevision: &on.Revision}),
		http.StatusConflict, siteAccessCodeConflict)
	expectBasicAuthChallenge(t, env.newClient().get("/"))
	expectStatus(t, env.newClient().getAs("/", browserUA, basicAuthHeader("changed-locally", remoteAccessPass)), http.StatusOK)
}

// 「サイト設定」を開いた後にほかで閲覧制限が変わったときは、古い画面の内容で上書きせず 409 で何も保存しない。
// 表示し直した画面は最新の閲覧制限と版を出し、ほかの入力は残す。そこから保存し直せば通る。閲覧制限の内容が今の設定と
// 同じなら、版が古くても上書きにならないため、ほかの設定の保存を妨げない。
func TestGeneralSettingsStaleSiteAccessConflict(t *testing.T) {
	env := newSyncEnvTLS(t)
	owner := env.token("owner", auth.RoleAdmin)
	adm := env.loggedIn(admin)
	on := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, env.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})))
	opened := siteSettingsRevision(t, adm)
	if opened != on.Revision {
		t.Fatalf("form revision %q, want %q", opened, on.Revision)
	}
	changed := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, env.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: "pc-b", ExpectedRevision: &on.Revision})))

	form := generalSettingsForm()
	form.Set("site_name", "古い画面からの名前")
	form.Set("basic_auth_enabled", "1")
	form.Set("basic_auth_user", remoteAccessUser)
	form.Set("basic_auth_revision", opened)
	res := adm.postForm("/admin/settings/general", form)
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, "ほかの画面または PC で変更されていた", `value="古い画面からの名前"`, `name="basic_auth_user" type="text" value="pc-b"`,
		`name="basic_auth_revision" value="`+changed.Revision+`"`)
	if st := env.getSiteAccess(owner); st.User != "pc-b" || st.Revision != changed.Revision {
		t.Fatalf("site access after the conflict = %+v", st)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key = 'site_name'`); n != 0 {
		t.Fatalf("the general settings were saved despite the conflict")
	}
	// 古い版でパスワードだけを入れても上書きしない。
	form.Set("basic_auth_user", "pc-b")
	form.Set("basic_auth_pass", remoteAccessPass2)
	expectStatus(t, adm.postForm("/admin/settings/general", form), http.StatusConflict)
	if !env.server.basicAuth.Load().authorized(basicAuthRequest("pc-b", remoteAccessPass)) {
		t.Fatal("a stale form changed the password")
	}

	// 閲覧制限の内容が今の設定と同じなら、古い版でもほかの設定を保存できる（閲覧制限は変わらない）。
	form.Set("basic_auth_pass", "")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if st := env.getSiteAccess(owner); st.User != "pc-b" || st.Revision != changed.Revision {
		t.Fatalf("an unchanged restriction was rewritten: %+v", st)
	}
	// 表示し直した画面の版から保存し直せば、閲覧制限も変えられる。
	form.Set("basic_auth_revision", changed.Revision)
	form.Set("basic_auth_user", "renamed-here")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if st := env.getSiteAccess(owner); st.User != "renamed-here" || st.Revision == changed.Revision {
		t.Fatalf("saving from the latest revision = %+v", st)
	}
}

// basicAuthRequest は Basic 認証の資格情報を付けた要求（閲覧制限の照合の確認用）。
func basicAuthRequest(user, pass string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth(user, pass)
	return req
}

// --- PC の「公開サーバー」画面 ---

// siteAccessReply は PC の /admin/remote/site-access の応答（成功・失敗の項目を合わせたもの）。
type siteAccessReply struct {
	remoteSiteAccessView
	Message        string                `json:"message"`
	Changed        bool                  `json:"changed"`
	State          *remoteSiteAccessView `json:"state"`
	Conflict       bool                  `json:"conflict"`
	OutcomeUnknown bool                  `json:"outcome_unknown"`
	RemoteStatus   int                   `json:"remote_status"`
	Blocked        bool                  `json:"blocked"`
}

func decodeSiteAccessReply(t *testing.T, res *response) siteAccessReply {
	t.Helper()
	var v siteAccessReply
	if err := json.Unmarshal([]byte(res.Body), &v); err != nil {
		t.Fatalf("decode site access reply: %v\nbody: %.2000s", err, res.Body)
	}
	return v
}

// siteAccessForm は PC の保存のフォーム。
func siteAccessForm(enabled bool, user, pass, revision string) url.Values {
	v := url.Values{"user": {user}, "password": {pass}, "revision": {revision}}
	if enabled {
		v.Set("enabled", "1")
	}
	return v
}

// newRemoteSiteAccessEnv は PC と公開専用モードの公開サーバーを起動し、PC に管理者のトークンで接続する。公開サーバーは
// 本番（AUTOCERT_DOMAIN）と同じく sorus が TLS を直接受け、PC は https で接続する。
func newRemoteSiteAccessEnv(t *testing.T) (*remoteEnv, *client) {
	t.Helper()
	env := &remoteEnv{local: newTestEnv(t), remote: newPublicOnlyTLSEnv(t, nil)}
	env.configure("owner", auth.RoleAdmin)
	return env, env.local.loggedIn(admin)
}

func checkResults(v remoteSiteAccessView) []string {
	var out []string
	for _, c := range v.Checks {
		out = append(out, c.Result)
	}
	return out
}

// PC の管理者は、「サイト運用」の画面で公開サーバーの閲覧制限の状態を見て、有効化・変更・無効化できる。保存した後は公開サイトを外から開き、
// パスワードを求められることを確かめた結果を見せる。PC 自身の閲覧制限は変わらない。
func TestRemoteSiteAccessFlow(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	anonRemote := env.remote.newClient()

	page := adm.get(operationsPagePath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "data-remote-site-access", "公開サーバーの閲覧制限（Basic認証）", `src="/static/js/remote_site_access.js?v=`,
		`for="remote_access_user"`, `for="remote_access_pass"`, `type="password" autocomplete="new-password"`,
		"この sorus の「サイト設定」の閲覧制限とは別の、独立した設定です", "CDN のキャッシュを削除（パージ）")
	expectNotContains(t, page.Body, "onpaste", "readonly id=\"remote_access_pass\"")
	// 閲覧制限は「サイト運用」へ移し、「公開サーバー」（接続・参加・招待）の画面には出さない（SPEC §2-6）。
	expectNotContains(t, adm.get("/admin/settings/remote").Body, "data-remote-site-access", "js/remote_site_access.js")
	// hidden 属性で切り替える要素に display のクラスがあると、隠したつもりの古い状態が見えたままになる。
	for _, id := range []string{"remote-site-access-details", "remote-site-access-form", "remote-site-access-result", "remote-site-access-blocked"} {
		m := regexp.MustCompile(`<[a-z]+ id="` + id + `"[^>]*>`).FindString(page.Body)
		if m == "" || !strings.Contains(m, " hidden") || regexp.MustCompile(`class="[^"]*\b(grid|flex|block|inline-flex|table)\b`).MatchString(m) {
			t.Errorf("toggled element %s = %q, want hidden without a display class", id, m)
		}
	}
	expectStatus(t, adm.get("/admin/remote/site-access.js"), http.StatusNotFound)
	script := adm.get("/static/js/remote_site_access.js")
	expectStatus(t, script, http.StatusOK)
	// 変更できない構成（https でない・公開サーバーが TLS を直接受けていない）では、理由を出して保存のフォームを使えなくする。
	expectContains(t, script.Body, "textContent", "/admin/remote/site-access", "can_save", "save_blocked", "remote-site-access-blocked", "data.blocked")
	expectNotContains(t, script.Body, "innerHTML")

	var off siteAccessReply
	res := adm.getJSON("/admin/remote/site-access", nil)
	expectStatus(t, res, http.StatusOK)
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	off = decodeSiteAccessReply(t, res)
	if off.Status != siteAccessStatusOff || off.Enabled || off.Revision != "" || off.Tone != "neutral" || !off.CanSave || off.SaveBlocked != "" ||
		!reflect.DeepEqual(checkResults(off.remoteSiteAccessView), []string{siteAccessCheckOpen, siteAccessCheckOpen}) {
		t.Fatalf("initial state = %+v", off)
	}

	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, off.Revision), nil)
	expectStatus(t, res, http.StatusOK)
	expectNoSecrets(t, "save response", res.Body)
	saved := decodeSiteAccessReply(t, res)
	// 確かめた URL がすべてパスワードを求めても、確かめた範囲だけを示し、成功の色にしない（CDN のキャッシュなどは確かめていない）。
	if saved.State == nil || saved.State.Status != siteAccessStatusChecked || saved.State.Tone != "info" || !saved.Changed ||
		!saved.State.PasswordSet || saved.State.User != remoteAccessUser || !strings.Contains(saved.Message, "確かめた URL（下の一覧）は、すべてパスワードを求めました") ||
		!strings.Contains(saved.State.Summary, "確かめた URL（下の一覧）") || len([]rune(saved.State.Summary)) > 60 {
		t.Fatalf("save = %+v (state %+v)", saved, saved.State)
	}
	// トップページと、公開サーバーが挙げたフィードを開く（記事・画像が無い公開サーバーなので、ほかは無い）。
	if !reflect.DeepEqual(checkResults(*saved.State), []string{siteAccessCheckProtected, siteAccessCheckProtected}) ||
		saved.State.Checks[0].URL != env.remote.srv.URL+"/" || saved.State.Checks[1].URL != env.remote.srv.URL+"/feed.xml" {
		t.Fatalf("checks = %+v", saved.State.Checks)
	}
	expectBasicAuthChallenge(t, anonRemote.get("/"))
	expectStatus(t, anonRemote.getAs("/", browserUA, basicAuthHeader(remoteAccessUser, remoteAccessPass)), http.StatusOK)

	got := adm.getJSON("/admin/remote/site-access", nil)
	expectNoSecrets(t, "status response", got.Body, env.remote.setting(basicAuthPassHashSetting))
	if st := decodeSiteAccessReply(t, got); st.Status != siteAccessStatusChecked || st.Revision != saved.State.Revision {
		t.Fatalf("status after saving = %+v", st)
	}

	// パスワード欄が空なら、公開サーバーに保存済みのパスワードを使い続ける。
	renamed := decodeSiteAccessReply(t, expectOK(t, adm.postJSON("/admin/remote/site-access",
		siteAccessForm(true, "reviewers", "", saved.State.Revision), nil)))
	if renamed.State.User != "reviewers" || !renamed.State.PasswordSet || renamed.State.Status != siteAccessStatusChecked {
		t.Fatalf("rename = %+v", renamed.State)
	}
	expectStatus(t, anonRemote.getAs("/", browserUA, basicAuthHeader("reviewers", remoteAccessPass)), http.StatusOK)

	// 無効にする。
	disabled := decodeSiteAccessReply(t, expectOK(t, adm.postJSON("/admin/remote/site-access",
		siteAccessForm(false, "reviewers", "", renamed.State.Revision), nil)))
	if disabled.State.Status != siteAccessStatusOff || disabled.State.Enabled || !strings.Contains(disabled.Message, "無効にしました") {
		t.Fatalf("disable = %+v", disabled)
	}
	expectStatus(t, anonRemote.get("/"), http.StatusOK)

	// PC 自身の閲覧制限と、PC の公開サイトは変わらない。公開サーバーの状態を PC に保存しない。
	if n := env.local.countRows(`SELECT COUNT(*) FROM settings WHERE key LIKE 'basic_auth%' AND value != '' AND value != '0'`); n != 0 {
		t.Errorf("local basic auth settings were written (%d rows)", n)
	}
	if env.local.server.basicAuthActive() {
		t.Error("the local access restriction was turned on")
	}
	expectStatus(t, env.local.newClient().get("/"), http.StatusOK)
}

// 閲覧制限の画面と API は、ローカルの管理者が管理者のトークンで接続しているときだけ使える。
func TestRemoteSiteAccessPermissions(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	for _, u := range []testUser{studentA, chief} {
		c := env.local.loggedIn(u)
		expectStatus(t, c.getJSON("/admin/remote/site-access", nil), http.StatusForbidden)
		expectStatus(t, c.postJSON("/admin/remote/site-access", siteAccessForm(false, "", "", ""), nil), http.StatusForbidden)
	}
	// CSRF トークンの無い POST は断る。
	res := adm.postRaw("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, ""))
	expectStatus(t, res, http.StatusForbidden)
	if env.remote.server.basicAuthActive() {
		t.Fatal("a request without the CSRF token changed the remote restriction")
	}

	// 編集者のトークンでは、公開サーバーに問い合わせる前に断り、画面にも出さない。
	env.configure("desk", auth.RoleEditor)
	expectStatus(t, adm.getJSON("/admin/remote/site-access", nil), http.StatusForbidden)
	expectStatus(t, adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, ""), nil), http.StatusForbidden)
	expectNotContains(t, adm.get("/admin/settings/remote").Body, "data-remote-site-access", "js/remote_site_access.js")
	if env.remote.server.basicAuthActive() {
		t.Fatal("an editor token changed the remote restriction")
	}

	// 未設定なら案内を返す。
	if err := env.local.server.saveRemoteConfig(remoteConfig{}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adm.getJSON("/admin/remote/site-access", nil), http.StatusConflict)
}

// 別々の PC が同じ状態から変更すると、後から保存した PC には 409 と最新の状態を返し、公開サーバーは先の変更のまま。
func TestRemoteSiteAccessConflictBetweenPCs(t *testing.T) {
	env, pcA := newRemoteSiteAccessEnv(t)
	other := &remoteEnv{local: newTestEnv(t), remote: env.remote}
	other.configure("owner-2", auth.RoleAdmin)
	pcB := other.local.loggedIn(admin)

	a := decodeSiteAccessReply(t, expectOK(t, pcA.getJSON("/admin/remote/site-access", nil)))
	b := decodeSiteAccessReply(t, expectOK(t, pcB.getJSON("/admin/remote/site-access", nil)))
	expectOK(t, pcA.postJSON("/admin/remote/site-access", siteAccessForm(true, "pc-a", remoteAccessPass, a.Revision), nil))

	res := pcB.postJSON("/admin/remote/site-access", siteAccessForm(false, "pc-b", "", b.Revision), nil)
	expectStatus(t, res, http.StatusConflict)
	conflict := decodeSiteAccessReply(t, res)
	if !conflict.Conflict || conflict.State == nil || conflict.State.User != "pc-a" || conflict.State.Status != siteAccessStatusChecked ||
		!strings.Contains(conflict.Message, "ほかの PC") {
		t.Fatalf("conflict reply = %+v (state %+v)", conflict, conflict.State)
	}
	if st := env.remote.getSiteAccess(env.remote.token("check", auth.RoleAdmin)); st.User != "pc-a" || !st.Active {
		t.Fatalf("remote state after the conflict = %+v", st)
	}
}

// fakeRemote は PC から見た公開サーバーの代わり。同期 API の応答と、トップページの応答を決められる。本番の公開サーバーと
// 同じく https で受ける（PC は閲覧用のパスワードを https の公開サーバーにだけ送る）。
type fakeRemote struct {
	srv    *httptest.Server
	mu     sync.Mutex
	handle func(req SyncRequest) (int, string)
	root   int
	seen   []string
}

func newFakeRemote(t *testing.T, handle func(req SyncRequest) (int, string)) *fakeRemote {
	t.Helper()
	return startFakeRemote(t, handle, true)
}

// newPlainFakeRemote は newFakeRemote と同じ公開サーバーの代わりを、暗号化されない http（http://127.0.0.1:PORT）で起動する。
func newPlainFakeRemote(t *testing.T, handle func(req SyncRequest) (int, string)) *fakeRemote {
	t.Helper()
	return startFakeRemote(t, handle, false)
}

func startFakeRemote(t *testing.T, handle func(req SyncRequest) (int, string), useTLS bool) *fakeRemote {
	t.Helper()
	f := &fakeRemote{handle: handle, root: http.StatusUnauthorized}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.String()+" "+string(body))
		root := f.root
		f.mu.Unlock()
		if r.Method == http.MethodGet {
			if root == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", BasicAuthChallenge)
			}
			w.WriteHeader(root)
			return
		}
		var req SyncRequest
		_ = json.Unmarshal(body, &req)
		status, out := f.handle(req)
		if status == fakeDropConnection {
			// 応答を返さずに接続を切る（要求は届いたが、応答が無い）。
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	if useTLS {
		f.srv.StartTLS()
	} else {
		f.srv.Start()
	}
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRemote) requests() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.seen, "\n")
}

// configureFake は PC の接続先を f にし、管理者のトークン（署名は f が検証しない）を保存する。
func configureFake(t *testing.T, local *testEnv, f *fakeRemote) {
	t.Helper()
	_, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.IssueToken(priv, "owner", string(auth.RoleAdmin), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.server.saveRemoteConfig(remoteConfig{URL: f.srv.URL, SyncPath: "/api/sync-fake", Token: tok}); err != nil {
		t.Fatal(err)
	}
}

const fakeOnState = `{"action":"%s","enabled":true,"active":true,"user":"preview","password_set":true,"revision":"r1","site_url":""}`

// fakeDropConnection は fakeRemote の handle が返すと、応答を返さずに接続を切るステータス。
const fakeDropConnection = -1

// fakeStateThen は、状態の確認（get_site_access）には有効な状態を返し、変更（set_site_access）には status・body を返す
// fakeRemote の handle。PC は変更を送る前に状態を確かめるため、変更を送った後の失敗を試すときに使う。
func fakeStateThen(status int, body string) func(SyncRequest) (int, string) {
	return func(req SyncRequest) (int, string) {
		if req.Action == SyncActionGetSiteAccess {
			return http.StatusOK, strings.Replace(fakeOnState, "%s", req.Action, 1)
		}
		return status, body
	}
}

// 公開サーバーに問い合わせられない・未対応・読めない応答のときは「分からない」と返し、保護されているとは返さない。
// 変更を送った後に失敗したときは、反映されたかどうか分からないと伝える。PC は変更を送る前に状態を確かめ、そこで
// 失敗したときは変更を送っていない（何も変わっていない）と伝える。
func TestRemoteSiteAccessUnknownIsNeverProtected(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	expectUnknown := func(what string, res *response, wantMsg string) siteAccessReply {
		t.Helper()
		if res.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: status %d, want 502\nbody: %s", what, res.StatusCode, res.Body)
		}
		r := decodeSiteAccessReply(t, res)
		if r.Status != siteAccessCheckUnknown || r.State != nil || !strings.Contains(r.Message, wantMsg) {
			t.Fatalf("%s: reply = %+v, want unknown with %q", what, r, wantMsg)
		}
		expectNotContains(t, res.Body, `"protected"`, "保護されています")
		return r
	}

	// 接続できない。変更を送る前の確認で接続できないため、変更は送っていない。
	closed := newFakeRemote(t, nil)
	closed.srv.Close()
	configureFake(t, local, closed)
	expectUnknown("unreachable get", adm.getJSON("/admin/remote/site-access", nil), "接続できませんでした")
	r := expectUnknown("unreachable save", adm.postJSON("/admin/remote/site-access", siteAccessForm(true, "preview", remoteAccessPass, "r1"), nil), "公開サーバーに送っていません")
	if r.OutcomeUnknown || !strings.Contains(r.Message, "接続できませんでした") {
		t.Errorf("unreachable save: %+v, want the connection error and a known outcome (nothing was sent)", r)
	}

	// 変更を送った後に接続が切れた（応答が無い）。
	dropped := newFakeRemote(t, fakeStateThen(fakeDropConnection, ""))
	configureFake(t, local, dropped)
	r = expectUnknown("connection lost after sending", adm.postJSON("/admin/remote/site-access", siteAccessForm(true, "preview", remoteAccessPass, "r1"), nil), "反映されたかどうか分かりません")
	if !r.OutcomeUnknown || !strings.Contains(dropped.requests(), SyncActionSetSiteAccess) {
		t.Errorf("connection lost after sending: %+v (requests %s)", r, dropped.requests())
	}

	// この機能を知らない公開サーバー（未対応の action）。
	old := newFakeRemote(t, func(req SyncRequest) (int, string) {
		return http.StatusBadRequest, `{"message":"未対応の action です: \"` + req.Action + `\""}`
	})
	configureFake(t, local, old)
	expectUnknown("unsupported get", adm.getJSON("/admin/remote/site-access", nil), "対応していない")
	r = expectUnknown("unsupported save", adm.postJSON("/admin/remote/site-access", siteAccessForm(true, "preview", remoteAccessPass, ""), nil), "対応していない")
	if r.OutcomeUnknown {
		t.Errorf("unsupported save: the request was refused, so the outcome is known")
	}

	// 200 でも閲覧制限の応答でない（別のサーバー・壊れた応答）。
	for _, body := range []string{`{"action":"server_info","schedule":true}`, `<html>ok</html>`, `{}`} {
		weird := newFakeRemote(t, func(SyncRequest) (int, string) { return http.StatusOK, body })
		configureFake(t, local, weird)
		expectUnknown("unreadable get "+body, adm.getJSON("/admin/remote/site-access", nil), "読み取れませんでした")
		r = expectUnknown("unreadable before save "+body, adm.postJSON("/admin/remote/site-access", siteAccessForm(true, "preview", "", "r1"), nil), "公開サーバーに送っていません")
		if r.OutcomeUnknown || strings.Contains(weird.requests(), SyncActionSetSiteAccess) {
			t.Errorf("unreadable before save %s: %+v, want nothing sent (requests %s)", body, r, weird.requests())
		}
		// 状態は読めたが、変更の応答を読めない。
		weirdSet := newFakeRemote(t, fakeStateThen(http.StatusOK, body))
		configureFake(t, local, weirdSet)
		r = expectUnknown("unreadable save "+body, adm.postJSON("/admin/remote/site-access", siteAccessForm(true, "preview", "", "r1"), nil), "反映されたかどうか分かりません")
		if !r.OutcomeUnknown {
			t.Errorf("unreadable save %s: outcome_unknown = false", body)
		}
	}

	// 変更を送った後のサーバーエラー。
	broken := newFakeRemote(t, fakeStateThen(http.StatusInternalServerError, `{"message":"Internal Server Error"}`))
	configureFake(t, local, broken)
	r = expectUnknown("server error on save", adm.postJSON("/admin/remote/site-access", siteAccessForm(false, "preview", "", "r1"), nil), "反映されたかどうか分かりません")
	if !r.OutcomeUnknown || r.RemoteStatus != http.StatusInternalServerError {
		t.Errorf("server error on save: %+v", r)
	}

	// 公開サーバーでは有効でも、外から開くとパスワードなしで表示されるなら exposed、確かめられなければ unverified。
	f := newFakeRemote(t, func(req SyncRequest) (int, string) {
		return http.StatusOK, strings.Replace(fakeOnState, "%s", req.Action, 1)
	})
	configureFake(t, local, f)
	f.mu.Lock()
	f.root = http.StatusOK
	f.mu.Unlock()
	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	if st.Status != siteAccessStatusExposed || st.Tone != "error" || !strings.Contains(st.Summary, "パスワードなしで表示される") {
		t.Fatalf("site shown without a password: %+v", st)
	}
	for _, code := range []int{http.StatusFound, http.StatusForbidden, http.StatusServiceUnavailable} {
		f.mu.Lock()
		f.root = code
		f.mu.Unlock()
		st = decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
		if st.Status != siteAccessStatusUnverified || st.Tone != "warn" || !strings.Contains(st.Summary, "言い切れません") {
			t.Fatalf("site check answered %d: %+v", code, st)
		}
	}
	// 有効と答えても、働いていない（active でない）なら守られていない。
	inactive := newFakeRemote(t, func(req SyncRequest) (int, string) {
		return http.StatusOK, `{"action":"` + req.Action + `","enabled":true,"active":false,"user":"","password_set":false,"revision":"r2"}`
	})
	configureFake(t, local, inactive)
	if st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil))); st.Status != siteAccessStatusInactive || st.Tone != "error" {
		t.Fatalf("enabled but inactive: %+v", st)
	}
}

// CDN を通したサイトの URL（公開サーバーの BaseURL）も外から確かめ、キャッシュからパスワードなしで見えるなら
// 保護の完了と言わずに警告する。
func TestRemoteSiteAccessDetectsCachedCopies(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = io.WriteString(w, "cached page")
	}))
	t.Cleanup(cdn.Close)
	if _, err := env.remote.db.Exec(`INSERT INTO settings (key, value) VALUES ('base_url', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, cdn.URL); err != nil {
		t.Fatal(err)
	}
	// 試験の CDN はこの PC 内（127.0.0.1）で動くため、公開されたアドレスだけに接続する制限をこの試験でだけ外す
	// （制限そのものは TestReproSiteAccessProbeStaysOnPublicAddresses と TestPublicDialerPinsResolvedAddress）。
	env.local.server.siteAccessDialer.allow = func(net.IP) bool { return true }
	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	saved := decodeSiteAccessReply(t, expectOK(t, adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, st.Revision), nil)))
	if saved.State.Status != siteAccessStatusExposed || saved.State.Tone != "error" || !strings.Contains(saved.Message, "パスワードなしで表示される URL があります") ||
		!strings.Contains(saved.State.Summary, "CDN") {
		t.Fatalf("save behind a caching CDN = %+v (state %+v)", saved, saved.State)
	}
	want := map[string]string{
		env.remote.srv.URL + "/": siteAccessCheckProtected, env.remote.srv.URL + "/feed.xml": siteAccessCheckProtected,
		cdn.URL + "/": siteAccessCheckOpen, cdn.URL + "/feed.xml": siteAccessCheckOpen,
	}
	if len(saved.State.Checks) != len(want) {
		t.Fatalf("checks = %+v, want the server and the site URL", saved.State.Checks)
	}
	for _, c := range saved.State.Checks {
		if want[c.URL] != c.Result || c.Label == "" {
			t.Errorf("check %+v, want %s", c, want[c.URL])
		}
	}
	// 公開サーバー自体は守られている。CDN の設定は変えない（sorus は CDN に何も送らない）。
	expectBasicAuthChallenge(t, env.remote.newClient().get("/"))
}

// 閲覧用のパスワードは暗号化されない接続の公開サーバーには送らない。この PC 内の http://127.0.0.1 も同じ。画面は状態を
// 表示したうえで、変更できない理由を出し、保存のフォームを使えなくする（can_save が false）。
func TestRemoteSiteAccessRequiresHTTPS(t *testing.T) {
	local := newTestEnv(t)
	adm := local.loggedIn(admin)
	f := newFakeRemote(t, func(SyncRequest) (int, string) { return http.StatusOK, `{}` })
	configureFake(t, local, f)
	cfg := local.server.loadRemoteConfig()
	cfg.URL = "http://media.example.jp"
	if err := local.server.saveRemoteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	res := adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, ""), nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, jsonMessage(t, res), "https://")
	if got := f.requests(); got != "" {
		t.Fatalf("requests were sent: %s", got)
	}

	// この PC 内の平文の公開サーバー（試験用の http://127.0.0.1）。状態は確かめられるが、変更はできない。
	plain := newPlainFakeRemote(t, func(req SyncRequest) (int, string) {
		return http.StatusOK, strings.Replace(fakeOnState, "%s", req.Action, 1)
	})
	configureFake(t, local, plain)
	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	if st.CanSave || st.Status != siteAccessStatusChecked || !strings.Contains(st.SaveBlocked, "https://") || !strings.Contains(st.SaveBlocked, "http://localhost") {
		t.Fatalf("plain loopback status = %+v, want the state with saving blocked", st)
	}
	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, st.Revision), nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if r := decodeSiteAccessReply(t, res); !r.Blocked || !strings.Contains(r.Message, "https://") {
		t.Fatalf("save to a plain loopback server = %+v, want blocked", r)
	}
	if got := plain.requests(); strings.Contains(got, remoteAccessPass) || strings.Contains(got, SyncActionSetSiteAccess) {
		t.Fatalf("the change was sent over plain http:\n%s", got)
	}

	for raw, want := range map[string]bool{
		"https://media.example.jp": true, "HTTPS://media.example.jp": true,
		"http://127.0.0.1:8080": false, "http://localhost:8080": false, "http://[::1]:8080": false, "http://media.example.jp": false,
	} {
		if got := remoteEndpointSecure(remoteConfig{URL: raw}); got != want {
			t.Errorf("remoteEndpointSecure(%s) = %v, want %v", raw, got, want)
		}
	}
}

// PC の入力の誤りは公開サーバーに送る前に断り、公開サーバーが断った入力の誤りは表示言語の文言で返す。
func TestRemoteSiteAccessValidation(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{siteAccessForm(true, "a:b", remoteAccessPass, st.Revision), "「:」"},
		{siteAccessForm(true, "", remoteAccessPass, st.Revision), "ユーザー名を入力"},
		{siteAccessForm(true, remoteAccessUser, strings.Repeat("p", 73), st.Revision), "72バイト"},
		// 公開サーバーにパスワードが無いのに空欄で有効にしようとした（公開サーバーが確かめる）。
		{siteAccessForm(true, remoteAccessUser, "", st.Revision), "パスワードを入力"},
		{url.Values{"enabled": {"1"}, "user": {remoteAccessUser}, "password": {remoteAccessPass}}, "状態を読み込んでから"},
	} {
		res := adm.postJSON("/admin/remote/site-access", tc.form, nil)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, jsonMessage(t, res), tc.want)
		expectNoSecrets(t, "validation reply", res.Body)
	}
	if env.remote.server.basicAuthActive() {
		t.Fatal("invalid input changed the remote restriction")
	}
	// 英語の画面では英語で返す。
	adm.get("/admin/set-lang?lang=en")
	res := adm.postJSON("/admin/remote/site-access?lang=en", siteAccessForm(true, remoteAccessUser, "", st.Revision), nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, jsonMessage(t, res), "Enter a password for viewing")
}

// 閲覧用のパスワードは、成功・失敗のどちらでも、PC と公開サーバーのログ・応答・URL・PC の DB に出さない。
func TestRemoteSiteAccessDoesNotLeakSecrets(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	logs := &lockedBuffer{}
	for _, s := range []*Server{env.local.server, env.remote.server} {
		s.echo.Logger.SetOutput(logs)
		s.echo.Logger.SetLevel(glog.DEBUG)
	}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	var bodies []string
	res := adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, st.Revision), nil)
	expectStatus(t, res, http.StatusOK)
	bodies = append(bodies, res.Body)
	// 古い版で送った失敗（409）、検証の失敗も含める。
	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass2, st.Revision), nil)
	expectStatus(t, res, http.StatusConflict)
	bodies = append(bodies, res.Body)
	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, "bad:user", remoteAccessPass2, st.Revision), nil)
	bodies = append(bodies, res.Body)
	bodies = append(bodies, adm.getJSON("/admin/remote/site-access", nil).Body, adm.get("/admin/settings/remote").Body)

	// 変更を送った後に公開サーバーが失敗した（状態の確認には答えた）。そのあと公開サーバーが落ちた。
	f := newFakeRemote(t, fakeStateThen(http.StatusBadGateway, `{"message":"upstream failed"}`))
	cfg := env.local.server.loadRemoteConfig()
	cfg.URL, cfg.SyncPath = f.srv.URL, "/api/sync-fake"
	if err := env.local.server.saveRemoteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass2, "r1"), nil)
	bodies = append(bodies, res.Body)
	if !strings.Contains(f.requests(), SyncActionSetSiteAccess) {
		t.Fatalf("the change was not sent to the failing server: %s", f.requests())
	}
	f.srv.Close()
	res = adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass2, "r1"), nil)
	bodies = append(bodies, res.Body)

	hash := env.remote.setting(basicAuthPassHashSetting)
	for i, b := range bodies {
		expectNoSecrets(t, "response "+string(rune('0'+i)), b, hash)
	}
	expectNoSecrets(t, "logs", logs.String(), hash)
	// パスワードは要求の本文で送り、URL には入れない。
	for _, line := range strings.Split(f.requests(), "\n") {
		if u, _, _ := strings.Cut(line, " {"); strings.Contains(u, remoteAccessPass2) {
			t.Errorf("password in URL: %s", u)
		}
	}
	if n := env.local.countRows(`SELECT COUNT(*) FROM settings WHERE value LIKE ? OR value LIKE ?`, "%"+remoteAccessPass+"%", "%"+remoteAccessPass2+"%"); n != 0 {
		t.Errorf("the password is stored on the PC (%d rows)", n)
	}
	if n := env.remote.countRows(`SELECT COUNT(*) FROM settings WHERE value LIKE ? OR value LIKE ?`, "%"+remoteAccessPass+"%", "%"+remoteAccessPass2+"%"); n != 0 {
		t.Errorf("the plain password is stored on the public server (%d rows)", n)
	}
}

// 「公開サーバーへ反映する」（サイト基本設定の反映）は、PC の閲覧制限を公開サーバーに送らず、公開サーバーの閲覧制限を
// 変えない。
func TestRemoteSettingsPushKeepsSiteAccess(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	enableBasicAuth(t, adm, "local-only", "local-pass-123")
	expectOK(t, adm.postJSON("/admin/remote/settings/push", nil, nil))
	owner := env.remote.token("check", auth.RoleAdmin)
	if st := env.remote.getSiteAccess(owner); st.Enabled || st.User != "" || st.PasswordSet || st.Revision != "" {
		t.Fatalf("pushing settings changed the remote restriction: %+v", st)
	}
	expectStatus(t, env.remote.newClient().get("/"), http.StatusOK)

	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	on := decodeSiteAccessReply(t, expectOK(t, adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, st.Revision), nil)))
	// PC の閲覧制限を無効にしてから反映しても、公開サーバーの閲覧制限はそのまま。
	form := generalSettingsForm()
	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	expectOK(t, adm.postJSON("/admin/remote/settings/push", nil, nil))
	if got := env.remote.getSiteAccess(owner); got.Revision != on.State.Revision || !got.Active || got.User != remoteAccessUser {
		t.Fatalf("pushing settings changed the remote restriction: %+v", got)
	}
	expectBasicAuthChallenge(t, env.remote.newClient().get("/"))
	if env.local.server.basicAuthActive() {
		t.Fatal("the local restriction should be off")
	}
}

// 公開サーバーが閲覧制限を有効にしていても、PC は校閲待ちの記事を画像ごと取り込める（トークンで画像を取得する）。
func TestRemoteImportUnderSiteAccess(t *testing.T) {
	env, _ := newRemoteSiteAccessEnv(t)
	images := localImages(t, 320, 200)
	for _, img := range images {
		if err := os.MkdirAll(env.remote.uploadDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(env.remote.uploadDir+"/"+img.Name, img.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	owner := env.remote.token("check", auth.RoleAdmin)
	expectOK(t, env.remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))
	expectBasicAuthChallenge(t, env.remote.newClient().get("/uploads/"+images[0].Name))

	env.configure("desk", auth.RoleEditor)
	cfg := env.local.server.loadRemoteConfig()
	saved, failed := env.local.server.downloadRemoteImages(context.Background(), cfg, "![写真](/uploads/"+images[0].Name+")")
	if failed != 0 || saved != len(images) {
		t.Fatalf("download under the remote restriction: saved %d, failed %d, want %d saved", saved, failed, len(images))
	}
}
