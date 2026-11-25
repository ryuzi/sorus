package server

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"

	"sorus/internal/auth"
	"sorus/internal/mcp"
	"sorus/internal/theme"
	"sorus/static"
)

const (
	testBasicAuthUser = "preview"
	testBasicAuthPass = "s3cret-preview"
)

// basicAuthHeader は user・pass の Basic 認証の Authorization ヘッダーを返す。
func basicAuthHeader(user, pass string) map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))}
}

// enableBasicAuth は「サイト設定」の画面から閲覧制限を有効にする。
func enableBasicAuth(t *testing.T, adm *client, user, pass string) {
	t.Helper()
	form := generalSettingsForm()
	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	form.Set("basic_auth_enabled", "1")
	form.Set("basic_auth_user", user)
	form.Set("basic_auth_pass", pass)
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
}

// siteSettingsRevision は「サイト設定」の画面を開き、ブラウザと同じく、フォームが送り返す閲覧制限の版
// （basic_auth_revision）を返す。
func siteSettingsRevision(t *testing.T, adm *client) string {
	t.Helper()
	page := adm.get("/admin/settings/general")
	expectStatus(t, page, http.StatusOK)
	m := regexp.MustCompile(`name="basic_auth_revision" value="([^"]*)"`).FindStringSubmatch(page.Body)
	if m == nil {
		t.Fatal("the general settings form has no basic_auth_revision")
	}
	return m[1]
}

// expectBasicAuthChallenge は res が閲覧制限の 401 応答かを確かめる。
func expectBasicAuthChallenge(t *testing.T, res *response) {
	t.Helper()
	expectStatus(t, res, http.StatusUnauthorized)
	if got := res.Header.Get("WWW-Authenticate"); got != `Basic realm="Protected Site"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Basic realm="Protected Site"`)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control of 401 = %q, want no-store", got)
	}
}

// expectNotBasicAuthChallenge は res が閲覧制限で止められていないかを確かめる。
func expectNotBasicAuthChallenge(t *testing.T, res *response, what string) {
	t.Helper()
	if strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("%s: blocked by basic auth (status %d)", what, res.StatusCode)
	}
}

// 閲覧制限を設定していなければ、公開ページは今までどおり誰でも開ける。
func TestBasicAuthDisabledAllowsPublicPages(t *testing.T) {
	env := newTestEnv(t)
	env.insertPublished("ba-1", "公開記事", "open-article")
	anon := env.newClient()
	for _, p := range []string{"/", "/articles/open-article", "/feed.xml"} {
		res := anon.get(p)
		expectStatus(t, res, http.StatusOK)
		expectNotBasicAuthChallenge(t, res, p)
	}

	// 有効のチェックを付けずにユーザー名とパスワードだけ保存しても、制限はかからない。
	adm := env.loggedIn(admin)
	form := generalSettingsForm()
	form.Set("basic_auth_user", testBasicAuthUser)
	form.Set("basic_auth_pass", testBasicAuthPass)
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	expectStatus(t, anon.get("/"), http.StatusOK)
	if got := env.setting(basicAuthEnabledSetting); got != "0" {
		t.Errorf("basic_auth_enabled = %q, want 0", got)
	}
}

func TestBasicAuthProtectsPublicSite(t *testing.T) {
	env := newTestEnv(t)
	env.insertPublished("ba-1", "準備中の記事", "secret-article")
	env.writeUpload("photo.webp", "image")
	adm := env.loggedIn(admin)
	enableBasicAuth(t, adm, testBasicAuthUser, testBasicAuthPass)

	// パスワードは平文で保存せず、設定画面にも出さない。
	hash := env.setting(basicAuthPassHashSetting)
	if hash == "" || strings.Contains(hash, testBasicAuthPass) {
		t.Fatalf("basic_auth_pass_hash = %q, want a bcrypt hash", hash)
	}
	if env.countRows(`SELECT COUNT(*) FROM settings WHERE value = ?`, testBasicAuthPass) != 0 {
		t.Error("plain password is stored in settings")
	}
	page := adm.get("/admin/settings/general")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "サイト閲覧制限（Basic認証）", "sorus サーバーが直接配信する公開サイト", "Cloudflare Pages、S3",
		`name="basic_auth_enabled" type="checkbox" value="1" checked`,
		`value="`+testBasicAuthUser+`"`, "パスワードは設定済みです")
	expectNotContains(t, page.Body, testBasicAuthPass, hash)

	anon := env.newClient()
	// 公開サイトの内容（ページ・記事・フィード・画像・存在しないページ）は、ヘッダーが無ければ 401。
	protected := []string{"/", "/articles/secret-article", "/articles", "/categories/uncategorized", "/search?q=x",
		"/feed.xml", "/sitemap.xml", "/robots.txt", "/llms.txt", "/uploads/photo.webp", "/no-such-page"}
	for _, p := range protected {
		res := anon.get(p)
		expectBasicAuthChallenge(t, res)
		expectNotContains(t, res.Body, "準備中の記事")
	}
	expectContains(t, anon.get("/").Body, "このサイトは閲覧が制限されています")
	expectBasicAuthChallenge(t, anon.send(http.MethodHead, "/", nil))

	// 正しいユーザー名とパスワードなら開ける。CDN にはキャッシュさせない。
	ok := basicAuthHeader(testBasicAuthUser, testBasicAuthPass)
	for _, p := range []string{"/", "/articles/secret-article", "/feed.xml", "/uploads/photo.webp"} {
		res := anon.getAs(p, browserUA, ok)
		expectStatus(t, res, http.StatusOK)
		if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
			t.Errorf("%s: Cache-Control = %q, want %q", p, got, basicAuthCacheControl)
		}
	}
	expectContains(t, anon.getAs("/articles/secret-article", browserUA, ok).Body, "準備中の記事")
	// 2回目以降（照合済みの資格情報）も開ける。
	expectStatus(t, anon.getAs("/", browserUA, ok), http.StatusOK)

	// 違うパスワード・違うユーザー名・Basic 以外の Authorization は 401。
	for _, h := range []map[string]string{
		basicAuthHeader(testBasicAuthUser, "wrong"),
		basicAuthHeader("someone", testBasicAuthPass),
		basicAuthHeader(testBasicAuthUser, ""),
		{"Authorization": "Bearer " + testBasicAuthPass},
	} {
		expectBasicAuthChallenge(t, anon.getAs("/", browserUA, h))
	}

	// 管理画面・ログイン・共通の静的アセット・ヘルスチェック・API は閲覧制限の対象外（二重ログインにしない）。
	login := anon.get("/admin/login")
	expectStatus(t, login, http.StatusOK)
	expectNotBasicAuthChallenge(t, login, "/admin/login")
	expectRedirect(t, anon.get("/admin/articles"), "/admin/login")
	for _, p := range []string{"/static/css/admin.css", "/static/icon.svg", "/healthz", "/manifest.json", "/sw.js"} {
		res := anon.get(p)
		expectStatus(t, res, http.StatusOK)
		expectNotBasicAuthChallenge(t, res, p)
	}
	expectNotBasicAuthChallenge(t, anon.send(http.MethodPost, "/api/mcp", nil), "/api/mcp")

	// 管理画面にログイン中の編集部員は、パスワードを入れずに公開サイトと画像を開ける。
	for _, p := range []string{"/", "/articles/secret-article", "/uploads/photo.webp"} {
		res := adm.get(p)
		expectStatus(t, res, http.StatusOK)
		if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
			t.Errorf("staff %s: Cache-Control = %q, want %q", p, got, basicAuthCacheControl)
		}
	}
	// 管理画面のテーマのプレビュー（サーバー内部の要求で公開ページを描く）も開ける。
	preview := adm.get(themePreviewURL(theme.Default, previewPageHome, false))
	expectStatus(t, preview, http.StatusOK)
	expectNotContains(t, preview.Body, "このサイトは閲覧が制限されています")

	// パスワードを変えると、すぐに新しいパスワードだけが通る（照合済みの古い資格情報は使えない）。
	enableBasicAuth(t, adm, testBasicAuthUser, "new-password")
	expectBasicAuthChallenge(t, anon.getAs("/", browserUA, ok))
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader(testBasicAuthUser, "new-password")), http.StatusOK)

	// 無効にすると、すぐに誰でも開ける。
	form := generalSettingsForm()
	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	form.Set("basic_auth_user", testBasicAuthUser)
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	res := anon.get("/articles/secret-article")
	expectStatus(t, res, http.StatusOK)
	expectNotBasicAuthChallenge(t, res, "/articles/secret-article after disabling")
	if got := res.Header.Get("Cache-Control"); got != PublicHTMLCacheControl {
		t.Errorf("Cache-Control after disabling = %q, want %q", got, PublicHTMLCacheControl)
	}
}

// 対象外のパス（/admin・/api・/static）に見せかけた表記では、閲覧制限を通り抜けられない。
func TestBasicAuthCannotBeBypassedWithPathTricks(t *testing.T) {
	env := newTestEnv(t)
	env.insertPublished("ba-1", "準備中の記事", "secret-article")
	enableBasicAuth(t, env.loggedIn(admin), testBasicAuthUser, testBasicAuthPass)
	anon := env.newClient()
	for _, p := range []string{
		"/api/../articles/secret-article",
		"/static/../feed.xml",
		"/admin/../articles/secret-article",
		"/admin%2F..%2Farticles%2Fsecret-article",
		"/%61dmin/../articles/secret-article",
		"/%61pi/x",
	} {
		res := anon.get(p)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", p, res.StatusCode)
		}
		expectNotContains(t, res.Body, "準備中の記事")
	}
}

func TestBasicAuthSettingsValidation(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	cases := []struct {
		name, user, pass, want string
	}{
		{"user required", "", testBasicAuthPass, "閲覧用ユーザー名を入力してください"},
		{"pass required", testBasicAuthUser, "", "閲覧用パスワードを入力してください"},
		{"colon in user", "pre:view", testBasicAuthPass, "「:」（コロン）"},
		{"control char in pass", testBasicAuthUser, "a\tb", "制御文字は使えません"},
		{"pass too long", testBasicAuthUser, strings.Repeat("a", maxBasicAuthPassBytes+1), "72バイト"},
	}
	for _, tc := range cases {
		form := generalSettingsForm()
		form.Set("site_name", "保存されない名前")
		form.Set("basic_auth_enabled", "1")
		form.Set("basic_auth_user", tc.user)
		form.Set("basic_auth_pass", tc.pass)
		res := adm.postForm("/admin/settings/general", form)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", tc.name, res.StatusCode)
			continue
		}
		expectContains(t, res.Body, tc.want)
		// 入力したユーザー名とチェックは残し、パスワードは表示し直さない。
		expectContains(t, res.Body, `value="保存されない名前"`, `name="basic_auth_enabled" type="checkbox" value="1" checked`)
		if tc.pass != "" {
			expectNotContains(t, res.Body, `value="`+tc.pass+`"`)
		}
	}
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN (?, ?)`, "site_name", basicAuthEnabledSetting); n != 0 {
		t.Errorf("settings were saved despite validation errors (%d rows)", n)
	}
	expectStatus(t, env.newClient().get("/"), http.StatusOK)

	// パスワードを設定したあとは、空欄のまま保存すると今のパスワードを使い続ける。
	enableBasicAuth(t, adm, testBasicAuthUser, testBasicAuthPass)
	hash := env.setting(basicAuthPassHashSetting)
	enableBasicAuth(t, adm, "renamed", "")
	if got := env.setting(basicAuthPassHashSetting); got != hash {
		t.Errorf("password hash changed after saving with a blank password")
	}
	anon := env.newClient()
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader("renamed", testBasicAuthPass)), http.StatusOK)
	expectBasicAuthChallenge(t, anon.getAs("/", browserUA, basicAuthHeader(testBasicAuthUser, testBasicAuthPass)))

	// 英語の画面では英語で案内する。
	form := generalSettingsForm()
	form.Set("basic_auth_enabled", "1")
	form.Set("basic_auth_user", "")
	en := env.loggedIn(admin)
	expectStatus(t, en.get("/admin/set-lang?lang=en"), http.StatusSeeOther)
	res := en.postForm("/admin/settings/general", form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "Enter a username for viewing")
}

// 閲覧制限を有効にしても、「サイトに反映」（静的エクスポート）は公開ページを書き出せる。
func TestBasicAuthDoesNotBlockStaticExport(t *testing.T) {
	env := newTestEnv(t)
	env.insertPublished("ba-1", "準備中の記事", "secret-article")
	enableBasicAuth(t, env.loggedIn(admin), testBasicAuthUser, testBasicAuthPass)

	out := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(t.Context(), ExportOptions{OutDir: out}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "articles", "secret-article", "index.html"))
	if err != nil {
		t.Fatalf("exported article: %v", err)
	}
	if !strings.Contains(string(data), "準備中の記事") {
		t.Error("exported article does not contain the article")
	}
}

// バックアップから復元すると、閲覧制限の設定も復元した内容になる。
func TestBasicAuthReloadedAfterRestore(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	enableBasicAuth(t, adm, testBasicAuthUser, testBasicAuthPass)
	zipPath := adm.downloadBackup()

	form := generalSettingsForm()
	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	anon := env.newClient()
	expectStatus(t, anon.get("/"), http.StatusOK)

	expectStatus(t, adm.postRestore(readFile(t, zipPath), true), http.StatusOK)
	expectBasicAuthChallenge(t, anon.get("/"))
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader(testBasicAuthUser, testBasicAuthPass)), http.StatusOK)
}

func TestValidateBasicAuthInput(t *testing.T) {
	cases := []struct {
		enabled       bool
		user, pass    string
		hasStoredPass bool
		want          error
	}{
		{false, "", "", false, nil},
		{true, "preview", "pass", false, nil},
		{true, "preview", "", true, nil},
		{true, "閲覧者", "日本語のパスワード", false, nil},
		{true, "", "pass", false, errBasicAuthUserRequired},
		{true, "preview", "", false, errBasicAuthPassRequired},
		{false, "a:b", "", false, errBasicAuthUserInvalid},
		{false, "a\x00b", "", false, errBasicAuthUserInvalid},
		{false, strings.Repeat("あ", maxBasicAuthUserRunes+1), "", false, errBasicAuthUserLong},
		{false, "", "a\nb", false, errBasicAuthPassInvalid},
		{false, "", strings.Repeat("a", maxBasicAuthPassBytes+1), false, errBasicAuthPassLong},
		{true, "preview", "pass:with:colons", false, nil},
	}
	for _, tc := range cases {
		if got := validateBasicAuthInput(tc.enabled, tc.user, tc.pass, tc.hasStoredPass); !errors.Is(got, tc.want) {
			t.Errorf("validateBasicAuthInput(%v, %q, %q, %v) = %v, want %v", tc.enabled, tc.user, tc.pass, tc.hasStoredPass, got, tc.want)
		}
	}
}

// siteGateExemptPath（閲覧制限・メンテナンス表示の対象外）は、ルーティングで実際に対象外の処理に届く要求だけを対象外にする。
// 管理画面はサーバーモードだけ、MCP サーバーと保護されたリソースのメタデータは設定されたパス（公開専用モードでは秘匿パス）だけ。
func TestSiteGateExemptPath(t *testing.T) {
	const sync = "/api/sync-0123abcd"
	const mcpSecret = "/api/mcp-0123abcd"
	type key struct{ method, path string }
	get := func(p string) key { return key{http.MethodGet, p} }
	post := func(p string) key { return key{http.MethodPost, p} }
	head := func(p string) key { return key{http.MethodHead, p} }
	both := map[key]bool{
		post(sync): true, get(sync): false, post(sync + "/"): false, post("/" + sync): false, post(sync + "/x"): false,
		post("/api/sync"): false, get("/api"): false, get("/api/no-such"): false, post("/api/no-such"): false, get("/apis"): false,
		get("/static/css/public.css"): true, head("/static/css/public.css"): true, post("/static/css/public.css"): false,
		get("/static//css/public.css"): true, get("/static"): false, get("/static/"): false, get("//static/css/public.css"): false,
		get("/static/../feed.xml"): false, get("/manifest.json"): true, head("/manifest.json"): true, get("/sw.js"): true,
		head("/sw.js"):         true,
		post("/manifest.json"): false, get("/manifest.json/"): false, get("/sw.js/"): false,
		get("/healthz"): true, head("/healthz"): false, post("/healthz"): false, get("/healthz/"): false, get("//healthz"): false,
		get("/./healthz"): false, get("/healthz/."): false, get(WindowPath): false,
		get(oauthAuthServerMetadataPath): true, head(oauthAuthServerMetadataPath): true, post(oauthAuthServerMetadataPath): false,
		post(OAuthRegisterPath): true, get(OAuthRegisterPath): false, post(OAuthTokenPath): true, get(OAuthTokenPath): false,
		get(OAuthAuthorizePath): true, post(OAuthAuthorizePath): true,
		get("/"): false, get("/articles/x"): false, get("/feed.xml"): false, get("/uploads/a.webp"): false,
		get("/attachments/1/download"): false, get("/og-image/x.png"): false, get("/favicon.ico"): false, get("/administrator"): false,
		get("/api/../articles/x"): false, get("/admin/../feed.xml"): false, get("//admin/../x"): false, get("//admin/login"): false,
		get("//api/mcp"): false, get("/api/mcpx"): false, post("/api/mcp/../../about"): false,
	}
	serverOnly := map[key]bool{
		get("/admin"): true, get("/admin/"): true, get("/admin/login"): true, get("/admin//login"): true, get("/admin/./login"): true,
		post("/admin/settings/general"): true, get("/api/mcp"): true, post("/api/mcp"): true, post("/api/mcp/"): true,
		get(mcp.ProtectedResourceMetadataPath): true, get(mcp.ProtectedResourceMetadataPath + mcp.DefaultBasePath): true,
	}
	for _, publicOnly := range []bool{true, false} {
		s := &Server{cfg: Config{PublicOnly: publicOnly, SyncAPIPath: sync}}
		check := func(k key, want bool) {
			t.Helper()
			if got := s.siteGateExemptPath(k.method, k.path); got != want {
				t.Errorf("publicOnly=%v: siteGateExemptPath(%s %q) = %v, want %v", publicOnly, k.method, k.path, got, want)
			}
		}
		for k, want := range both {
			check(k, want)
		}
		for k, want := range serverOnly {
			check(k, want && !publicOnly)
		}

		withSecretMCP := &Server{cfg: Config{PublicOnly: publicOnly, SyncAPIPath: sync, MCPAPIPath: mcpSecret}}
		for k, want := range map[key]bool{
			get(mcpSecret): true, post(mcpSecret): true, post(mcpSecret + "/"): true,
			get(mcp.ProtectedResourceMetadataPath + mcpSecret): true, post(mcp.ProtectedResourceMetadataPath + mcpSecret): false,
			get("/api/mcp"): false, post("/api/mcp"): false,
			get(mcp.ProtectedResourceMetadataPath): false, get(mcp.ProtectedResourceMetadataPath + mcp.DefaultBasePath): false,
		} {
			if got := withSecretMCP.siteGateExemptPath(k.method, k.path); got != want {
				t.Errorf("publicOnly=%v secretMCP: siteGateExemptPath(%s %q) = %v, want %v", publicOnly, k.method, k.path, got, want)
			}
		}
	}
}

// 閲覧制限をかけないのは、ルーティングで実際に対象外の処理（管理画面・トークンで守る API・組み込みのアセット・稼働の
// 監視）に届く要求だけ。末尾の "/"・"//"・"." の表記・符号化・登録していないメソッドで対象外のパスに似せた要求、管理画面・
// MCP の無い公開専用モードの /admin・/api/mcp、/api の下の存在しないパス、デスクトップモードにしか無い WindowPath は、
// ルートが合わずに公開サイトの 404 ページ（サイト名・メニュー）になるため、パスワードを求める（秘匿した同期 API の
// パスの有無も見せない）。管理画面にログイン中の編集部員と、同期 API のトークンで画像を読む PC は今までどおり通す。
func TestBasicAuthExemptsOnlyRoutedPaths(t *testing.T) {
	const siteName = "閲覧制限の試験メディア"
	seed := func(db *sqlx.DB) {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('site_name', ?)`, siteName); err != nil {
			t.Fatal(err)
		}
	}
	js := static.URL("js/public.js")
	html := map[string]string{"Accept": "text/html"}
	type req struct{ method, path string }
	for _, publicOnly := range []bool{true, false} {
		name := "server mode"
		if publicOnly {
			name = "public-only"
		}
		t.Run(name, func(t *testing.T) {
			var remote *syncEnv
			if publicOnly {
				remote = newPublicOnlyTLSEnv(t, seed)
			} else {
				remote = registerSyncEnv(t, startTestEnv(t, Config{}, seed, true))
			}
			remote.writeUpload("photo.webp", "image")
			enableSiteAccess(t, remote, testBasicAuthUser, testBasicAuthPass)
			sp := remote.server.cfg.SyncAPIPath
			hidden := []req{
				{http.MethodGet, "/healthz/"}, {http.MethodGet, "/manifest.json/"}, {http.MethodGet, "/sw.js/"},
				{http.MethodGet, "//healthz"}, {http.MethodGet, "/./healthz"}, {http.MethodGet, "/healthz/."},
				{http.MethodGet, "/healthz%2F"}, {http.MethodGet, "/%68ealthz"}, {http.MethodGet, "/" + js},
				{http.MethodHead, "/healthz"}, {http.MethodPost, "/healthz"}, {http.MethodPost, "/manifest.json"},
				{http.MethodPut, "/sw.js"}, {http.MethodPost, js}, {http.MethodGet, WindowPath},
				{http.MethodGet, "/api"}, {http.MethodGet, "/api/no-such"}, {http.MethodPost, "/api/no-such"},
				{http.MethodGet, sp}, {http.MethodGet, sp + "/x"}, {http.MethodPost, sp + "/"}, {http.MethodPost, "/" + sp},
				{http.MethodPost, sp + "/."}, {http.MethodPost, sp + "%2F"},
				{http.MethodGet, "//admin/login"}, {http.MethodGet, "/admin/../about"}, {http.MethodGet, "//api/mcp"},
				{http.MethodPost, "//api/mcp"}, {http.MethodGet, "/api/mcpx"}, {http.MethodPost, "/api/mcp/../../about"},
			}
			if publicOnly {
				hidden = append(hidden, req{http.MethodGet, "/admin"}, req{http.MethodGet, "/admin/login"},
					req{http.MethodGet, "/admin/offline"}, req{http.MethodGet, "/api/mcp"}, req{http.MethodPost, "/api/mcp"})
			}
			anon := remote.newClient()
			for _, r := range hidden {
				res := anon.rawRequest(r.method, r.path, html)
				if res.StatusCode != http.StatusUnauthorized {
					t.Errorf("anonymous %s %s: status %d, want 401 (site name shown: %v)", r.method, r.path, res.StatusCode,
						strings.Contains(res.Body, siteName))
					continue
				}
				expectBasicAuthChallenge(t, res)
				expectNotContains(t, res.Body, siteName)
			}

			// ルートが合う要求は今までどおり対象外。
			for _, r := range []req{{http.MethodGet, "/healthz"}, {http.MethodGet, "/manifest.json"}, {http.MethodHead, "/manifest.json"},
				{http.MethodGet, "/sw.js"}, {http.MethodHead, "/sw.js"}, {http.MethodGet, js}, {http.MethodHead, js}} {
				res := anon.rawRequest(r.method, r.path, html)
				if res.StatusCode != http.StatusOK {
					t.Errorf("anonymous %s %s: status %d, want 200", r.method, r.path, res.StatusCode)
				}
				expectNotBasicAuthChallenge(t, res, r.method+" "+r.path)
			}
			expectNotBasicAuthChallenge(t, anon.rawRequest(http.MethodGet, strings.Replace(js, "/static/", "/static//", 1), html), "GET /static//...")
			// 同期 API（秘匿したパスへの POST）はトークンで守る。トークンが無ければ同期 API の 401（Bearer）。
			noToken := anon.rawRequest(http.MethodPost, sp, nil)
			expectStatus(t, noToken, http.StatusUnauthorized)
			expectNotBasicAuthChallenge(t, noToken, "POST "+sp+" without a token")
			if st := remote.getSiteAccess(remote.token("owner", auth.RoleAdmin)); !st.Active {
				t.Fatalf("get_site_access = %+v, want active", st)
			}
			// 同期 API のトークン（編集者以上）で画像を読める。ルートが合わない表記ではパスワードを求める。
			editor := "Bearer " + remote.token("pc", auth.RoleEditor)
			expectStatus(t, anon.rawRequest(http.MethodGet, "/uploads/photo.webp", map[string]string{"Authorization": editor}), http.StatusOK)
			for _, p := range []string{"//uploads/photo.webp", "/uploads/../about", "/uploads%2Fphoto.webp"} {
				res := anon.rawRequest(http.MethodGet, p, map[string]string{"Authorization": editor, "Accept": "text/html"})
				if res.StatusCode != http.StatusUnauthorized {
					t.Errorf("GET %s with a sync token: status %d, want 401 (site name shown: %v)", p, res.StatusCode,
						strings.Contains(res.Body, siteName))
				}
			}

			if publicOnly {
				return
			}
			// サーバーモードの管理画面（ログイン画面を含む）と MCP サーバーは、管理画面のログイン・トークンで守るため対象外。
			for _, p := range []string{"/admin", "/admin/", "/admin/login", "/admin//login", "/admin/./login", "/admin/offline"} {
				expectNotBasicAuthChallenge(t, anon.get(p), "GET "+p)
			}
			for _, p := range []string{"/api/mcp", "/api/mcp/"} {
				res := anon.rawRequest(http.MethodPost, p, nil)
				expectStatus(t, res, http.StatusUnauthorized)
				expectNotBasicAuthChallenge(t, res, "POST "+p)
			}
			// 管理画面にログイン中の編集部員は、パスワードを入れずに公開サイトを開ける（存在しないパスも同じ）。
			staff := remote.loggedIn(admin)
			for _, p := range []string{"/", "/healthz/", "/api/no-such"} {
				expectNotBasicAuthChallenge(t, staff.get(p), "staff GET "+p)
			}
		})
	}
}

// 閲覧制限を有効にした公開サーバーに、同期 API のパスを含めずに公開サーバーの URL だけで接続テストをしても、同期 API が
// 見つからない（URL のパスを確かめる）案内を出す。閲覧制限の 401 を、トークンが認められなかったとは案内しない。
func TestRemoteTestWithoutSyncPathBehindBasicAuth(t *testing.T) {
	env := &remoteEnv{local: newTestEnv(t), remote: newPublicOnlyTLSEnv(t, nil)}
	enableSiteAccess(t, env.remote, testBasicAuthUser, testBasicAuthPass)
	adm := env.local.loggedIn(admin)
	tok := env.remote.token("owner", auth.RoleAdmin)
	var res remoteTestResult
	expectStatus(t, adm.postJSON("/admin/remote/test", url.Values{"remote_url": {env.remote.srv.URL}, "remote_token": {tok}}, &res), http.StatusOK)
	if res.OK || !strings.Contains(res.Message, "同期 API が見つかりません") || strings.Contains(res.Message, "トークン") {
		t.Errorf("connection test without the sync path: %+v", res)
	}
	res = remoteTestResult{}
	syncURL := env.remote.srv.URL + env.remote.server.cfg.SyncAPIPath
	expectStatus(t, adm.postJSON("/admin/remote/test", url.Values{"remote_url": {syncURL}, "remote_token": {tok}}, &res), http.StatusOK)
	if !res.OK {
		t.Errorf("connection test with the sync URL: %+v", res)
	}
}

// testBasicAuthRevision は v0.2.8 が保存した閲覧制限の版（seedBasicAuth が入れる）。
const testBasicAuthRevision = "0123456789abcdef0123456789abcdef"

// seedBasicAuth は、v0.2.8 の「サイト設定」または同期 API の set_site_access が保存したのと同じ形（同じ settings の
// キー・bcrypt のハッシュ・版）で、有効な閲覧制限を db に入れる。サーバーの起動前（seed）に呼び、更新前の版で保存した
// 設定を持つ DB を更新後の版で開いた状態にする。
func seedBasicAuth(t *testing.T, db *sqlx.DB, user, pass string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	for _, kv := range [][2]string{
		{basicAuthEnabledSetting, "1"},
		{basicAuthUserSetting, user},
		{basicAuthPassHashSetting, string(hash)},
		{basicAuthRevisionSetting, testBasicAuthRevision},
	} {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			t.Fatalf("seed %s: %v", kv[0], err)
		}
	}
}

// storedBasicAuth は保存済みの閲覧制限の設定と版を読む。
func (e *testEnv) storedBasicAuth() basicAuthState {
	e.t.Helper()
	st, err := loadBasicAuthState(e.t.Context(), e.db)
	if err != nil {
		e.t.Fatalf("load basic auth: %v", err)
	}
	return st
}

// enableSiteAccess は公開サーバー e の閲覧制限を、PC の「公開サーバー」画面と同じく同期 API の set_site_access で
// 有効にし、その応答を返す。e は TLS で起動しておく（newPublicOnlyTLSEnv・newSyncEnvTLS）。
func enableSiteAccess(t *testing.T, e *syncEnv, user, pass string) SyncSiteAccessResponse {
	t.Helper()
	owner := e.token("owner", auth.RoleAdmin)
	cur := e.getSiteAccess(owner)
	return decodeResponse[SyncSiteAccessResponse](t, expectOK(t, e.setSiteAccess(owner,
		SyncSiteAccessUpdate{Enabled: true, User: user, Password: pass, ExpectedRevision: &cur.Revision})))
}

// newDesktopBasicAuthEnv はデスクトップモードのサーバーを、テストユーザー入りの DB で起動する。legacy なら v0.2.8 の
// 「サイト設定」で閲覧制限を有効にして保存した DB にする。
func newDesktopBasicAuthEnv(t *testing.T, legacy bool) *testEnv {
	t.Helper()
	var seed func(db *sqlx.DB)
	if legacy {
		seed = func(db *sqlx.DB) { seedBasicAuth(t, db, testBasicAuthUser, testBasicAuthPass) }
	}
	return startTestEnv(t, Config{Desktop: true}, seed, false)
}

// basicAuthFormMarkers は「サイト設定」の閲覧制限（Basic 認証）の項目の目印。
var basicAuthFormMarkers = []string{
	`id="basic-auth"`, `name="basic_auth_enabled"`, `name="basic_auth_user"`, `name="basic_auth_pass"`,
	`name="basic_auth_revision"`, "basic-auth-remote-note", "サイト閲覧制限", "公開サイトをパスワードで保護する",
	"Site access restriction", "Protect the public site with a password",
}

// expectNoBasicAuthForm は page に「サイト設定」の閲覧制限の項目が無いことを確かめる。
func expectNoBasicAuthForm(t *testing.T, what, page string) {
	t.Helper()
	for _, m := range basicAuthFormMarkers {
		if strings.Contains(page, m) {
			t.Errorf("%s: the general settings page has %q", what, m)
		}
	}
}

// デスクトップモードの「サイト設定」には、閲覧制限（Basic 認証）の項目が無い（日本語・英語とも）。
func TestDesktopSiteSettingsHasNoBasicAuth(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		env := newDesktopBasicAuthEnv(t, legacy)
		adm := env.loggedIn(admin)
		page := adm.get("/admin/settings/general")
		expectStatus(t, page, http.StatusOK)
		expectContains(t, page.Body, `name="site_name"`)
		expectNoBasicAuthForm(t, "desktop", page.Body)
		expectStatus(t, adm.get("/admin/set-lang?lang=en"), http.StatusSeeOther)
		en := adm.get("/admin/settings/general")
		expectStatus(t, en, http.StatusOK)
		expectNoBasicAuthForm(t, "desktop (en)", en.Body)
	}
}

// v0.2.8 の「サイト設定」で閲覧制限を有効にしたデスクトップモードの PC でも、更新後は公開サイトにパスワードを求めない
// （保存済みの設定は残っていても働かせない）。
func TestDesktopIgnoresLegacyBasicAuth(t *testing.T) {
	env := newDesktopBasicAuthEnv(t, true)
	env.insertPublished("ba-1", "準備中の記事", "secret-article")
	env.writeUpload("photo.webp", "image")
	if env.server.basicAuthActive() {
		t.Error("the legacy restriction is active in desktop mode")
	}
	anon := env.newClient()
	for _, p := range []string{"/", "/articles/secret-article", "/feed.xml", "/uploads/photo.webp"} {
		res := anon.get(p)
		expectStatus(t, res, http.StatusOK)
		expectNotBasicAuthChallenge(t, res, p)
		if cc := res.Header.Get("Cache-Control"); strings.Contains(cc, "private") {
			t.Errorf("%s: Cache-Control = %q, want the public cache", p, cc)
		}
	}
	expectContains(t, anon.get("/articles/secret-article").Body, "準備中の記事")
	res := anon.get("/no-such-page")
	expectStatus(t, res, http.StatusNotFound)
	expectNotBasicAuthChallenge(t, res, "/no-such-page")
	// 復元などで保存済みの設定を読み直しても働かない。
	if err := env.server.reloadBasicAuth(); err != nil {
		t.Fatalf("reloadBasicAuth: %v", err)
	}
	expectNotBasicAuthChallenge(t, anon.get("/"), "/ after reload")
	if env.server.basicAuthActive() {
		t.Error("the legacy restriction became active after reload")
	}
}

// デスクトップモードの「サイト設定」の保存は閲覧制限を扱わない。v0.2.8 の古い画面から閲覧制限の項目（有効・無効、
// ユーザー名、パスワード、古い版、誤った値）が送られてきても無視し、保存済みの閲覧制限の設定を書き換えず（消しもせず）、
// 版の食い違い（409）や入力の誤り（422）で保存を止めない。
func TestDesktopSiteSettingsSaveIgnoresBasicAuth(t *testing.T) {
	legacyForms := []map[string]string{
		{},
		{"basic_auth_revision": ""},
		{"basic_auth_revision": "stale", "basic_auth_user": "pc-old"},
		{"basic_auth_revision": testBasicAuthRevision, "basic_auth_enabled": "1", "basic_auth_user": "renamed", "basic_auth_pass": "new-password"},
		{"basic_auth_enabled": "1", "basic_auth_user": "pre:view", "basic_auth_pass": strings.Repeat("a", maxBasicAuthPassBytes+1)},
		{"basic_auth_enabled": "1", "basic_auth_user": ""},
	}
	for _, legacy := range []bool{true, false} {
		env := newDesktopBasicAuthEnv(t, legacy)
		before := env.storedBasicAuth()
		adm := env.loggedIn(admin)
		anon := env.newClient()
		for i, fields := range legacyForms {
			form := generalSettingsForm()
			name := "PC のサイト " + string(rune('A'+i))
			form.Set("site_name", name)
			for k, v := range fields {
				form.Set(k, v)
			}
			expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
			if got := env.setting("site_name"); got != name {
				t.Fatalf("legacy=%v form %v: site_name = %q, want %q", legacy, fields, got, name)
			}
			if got := env.storedBasicAuth(); got != before {
				t.Fatalf("legacy=%v form %v: stored restriction %+v, want %+v", legacy, fields, got, before)
			}
			if env.server.basicAuthActive() {
				t.Fatalf("legacy=%v form %v: the restriction became active", legacy, fields)
			}
			expectNotBasicAuthChallenge(t, anon.get("/"), "/")
		}
		if !legacy {
			if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key LIKE 'basic_auth_%'`); n != 0 {
				t.Errorf("desktop saved %d basic auth settings", n)
			}
		}
	}
}

// v0.2.8 の「サイト設定」で閲覧制限を有効にした管理画面つきのサーバー版（同期 API なし）は、更新後も同じ設定で公開サイトを
// 守り続け、「サイト設定」で今までどおり確認・変更・解除でき、版の食い違いを 409 で防ぐ。
func TestServerModeKeepsSavedBasicAuthAfterUpdate(t *testing.T) {
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) { seedBasicAuth(t, db, testBasicAuthUser, testBasicAuthPass) })
	env.insertPublished("ba-1", "準備中の記事", "secret-article")
	anon := env.newClient()
	res := anon.get("/articles/secret-article")
	expectBasicAuthChallenge(t, res)
	expectNotContains(t, res.Body, "準備中の記事")
	ok := anon.getAs("/articles/secret-article", browserUA, basicAuthHeader(testBasicAuthUser, testBasicAuthPass))
	expectStatus(t, ok, http.StatusOK)
	expectContains(t, ok.Body, "準備中の記事")
	if got := ok.Header.Get("Cache-Control"); got != basicAuthCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, basicAuthCacheControl)
	}
	expectBasicAuthChallenge(t, anon.getAs("/", browserUA, basicAuthHeader(testBasicAuthUser, "wrong")))

	// 「サイト設定」に保存済みの設定と版が出る。
	adm := env.loggedIn(admin)
	page := adm.get("/admin/settings/general")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "サイト閲覧制限（Basic認証）", `name="basic_auth_enabled" type="checkbox" value="1" checked`,
		`value="`+testBasicAuthUser+`"`, "パスワードは設定済みです", `name="basic_auth_revision" value="`+testBasicAuthRevision+`"`)
	if got := siteSettingsRevision(t, adm); got != testBasicAuthRevision {
		t.Fatalf("form revision = %q, want %q", got, testBasicAuthRevision)
	}

	// 保存済みの版からユーザー名を変える（パスワード欄が空なら今のパスワードのまま）。
	enableBasicAuth(t, adm, "renamed", "")
	changed := env.storedBasicAuth()
	if changed.Revision == testBasicAuthRevision || changed.User != "renamed" {
		t.Fatalf("after renaming: %+v", changed)
	}
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader("renamed", testBasicAuthPass)), http.StatusOK)
	expectBasicAuthChallenge(t, anon.getAs("/", browserUA, basicAuthHeader(testBasicAuthUser, testBasicAuthPass)))

	// 更新前の版を持つ古い画面からの変更は 409 で、何も保存しない。
	form := generalSettingsForm()
	form.Set("site_name", "古い画面からの名前")
	form.Set("basic_auth_revision", testBasicAuthRevision)
	form.Set("basic_auth_enabled", "1")
	form.Set("basic_auth_user", testBasicAuthUser)
	expectStatus(t, adm.postForm("/admin/settings/general", form), http.StatusConflict)
	if got := env.storedBasicAuth(); got != changed {
		t.Fatalf("a stale form changed the restriction: %+v", got)
	}
	if got := env.setting("site_name"); got == "古い画面からの名前" {
		t.Fatal("the general settings were saved despite the conflict")
	}

	// 今の版から解除すると、すぐに誰でも開ける。
	form = generalSettingsForm()
	form.Set("basic_auth_revision", siteSettingsRevision(t, adm))
	form.Set("basic_auth_user", "renamed")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	expectStatus(t, anon.get("/articles/secret-article"), http.StatusOK)
	if env.server.basicAuthActive() {
		t.Fatal("the restriction is still active after disabling")
	}
}

// v0.2.8 の公開サーバー（公開専用モード・同期 API を持つ管理画面つきのサーバー）で有効にした閲覧制限は、更新後も同じ
// 設定で働き続け、同期 API で同じ版のまま確認・変更できる。
func TestPublicServerKeepsSavedBasicAuthAfterUpdate(t *testing.T) {
	seed := func(db *sqlx.DB) { seedBasicAuth(t, db, testBasicAuthUser, testBasicAuthPass) }
	envs := map[string]*syncEnv{
		"public-only":      newPublicOnlyTLSEnv(t, seed),
		"server with sync": startSyncEnv(t, Config{}, seed, true),
	}
	for name, remote := range envs {
		remote.insertPublished("ba-1", "準備中の記事", "secret-article")
		anon := remote.newClient()
		res := anon.get("/articles/secret-article")
		expectBasicAuthChallenge(t, res)
		expectNotContains(t, res.Body, "準備中の記事")
		ok := anon.getAs("/articles/secret-article", browserUA, basicAuthHeader(testBasicAuthUser, testBasicAuthPass))
		expectStatus(t, ok, http.StatusOK)
		expectContains(t, ok.Body, "準備中の記事")

		owner := remote.token("owner", auth.RoleAdmin)
		st := remote.getSiteAccess(owner)
		if !st.Enabled || !st.Active || st.User != testBasicAuthUser || !st.PasswordSet || st.Revision != testBasicAuthRevision {
			t.Fatalf("%s: get_site_access = %+v, want the saved restriction", name, st)
		}
		renamed := decodeResponse[SyncSiteAccessResponse](t, expectOK(t, remote.setSiteAccess(owner,
			SyncSiteAccessUpdate{Enabled: true, User: "renamed", ExpectedRevision: revPtr(testBasicAuthRevision)})))
		if !renamed.Changed || !renamed.Active || renamed.Revision == testBasicAuthRevision {
			t.Fatalf("%s: set_site_access = %+v", name, renamed)
		}
		expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader("renamed", testBasicAuthPass)), http.StatusOK)
		expectBasicAuthChallenge(t, anon.getAs("/", browserUA, basicAuthHeader(testBasicAuthUser, testBasicAuthPass)))
		// 古い版からの変更は 409 で、何も変えない。
		expectSiteAccessCode(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: false, ExpectedRevision: revPtr(testBasicAuthRevision)}),
			http.StatusConflict, siteAccessCodeConflict)
		expectBasicAuthChallenge(t, anon.get("/"))
	}
}

// 公開専用モードの公開サーバーの閲覧制限は、公開サイトの内容（ページ・記事・フィード・画像・存在しないページ）に
// パスワードを求め、正しい資格情報なら CDN に保存させずに開ける。同期 API・共通の静的アセット・ヘルスチェックは対象外で、
// 対象外のパスに見せかけた表記では通り抜けられない。
func TestPublicOnlyServerBasicAuthProtectsSite(t *testing.T) {
	remote := newPublicOnlyTLSEnv(t, nil)
	remote.insertPublished("ba-1", "準備中の記事", "secret-article")
	remote.writeUpload("photo.webp", "image")
	enableSiteAccess(t, remote, testBasicAuthUser, testBasicAuthPass)
	if hash := remote.setting(basicAuthPassHashSetting); hash == "" || strings.Contains(hash, testBasicAuthPass) {
		t.Fatalf("basic_auth_pass_hash = %q, want a bcrypt hash", hash)
	}

	anon := remote.newClient()
	for _, p := range []string{"/", "/articles/secret-article", "/articles", "/categories/uncategorized", "/search?q=x",
		"/feed.xml", "/sitemap.xml", "/robots.txt", "/llms.txt", "/uploads/photo.webp", "/no-such-page",
		"/api/../articles/secret-article", "/static/../feed.xml", "/admin%2F..%2Farticles%2Fsecret-article"} {
		res := anon.get(p)
		expectBasicAuthChallenge(t, res)
		expectNotContains(t, res.Body, "準備中の記事")
	}
	expectBasicAuthChallenge(t, anon.send(http.MethodHead, "/", nil))
	ok := basicAuthHeader(testBasicAuthUser, testBasicAuthPass)
	for _, p := range []string{"/", "/articles/secret-article", "/feed.xml", "/uploads/photo.webp"} {
		res := anon.getAs(p, browserUA, ok)
		expectStatus(t, res, http.StatusOK)
		if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
			t.Errorf("%s: Cache-Control = %q, want %q", p, got, basicAuthCacheControl)
		}
	}
	for _, h := range []map[string]string{
		basicAuthHeader(testBasicAuthUser, "wrong"),
		basicAuthHeader("someone", testBasicAuthPass),
		{"Authorization": "Bearer " + testBasicAuthPass},
	} {
		expectBasicAuthChallenge(t, anon.getAs("/", browserUA, h))
	}
	for _, p := range []string{"/static/css/public.css", "/healthz", "/manifest.json", "/sw.js"} {
		res := anon.get(p)
		expectStatus(t, res, http.StatusOK)
		expectNotBasicAuthChallenge(t, res, p)
	}
	// 管理画面の無い公開サーバーの /admin は存在しないページ（公開サイトの 404 ページ。サイト名・メニューを含む）と同じため、
	// ほかの存在しないページと同じくパスワードを求める。
	for _, p := range []string{"/admin", "/admin/login"} {
		res := anon.get(p)
		expectBasicAuthChallenge(t, res)
		expectNotContains(t, res.Body, "準備中の記事")
	}
	owner := remote.token("owner", auth.RoleAdmin)
	if st := remote.getSiteAccess(owner); !st.Active {
		t.Fatalf("get_site_access = %+v, want active", st)
	}

	// パスワードを変えると、すぐに新しいパスワードだけが通る。無効にすると、すぐに誰でも開ける。
	enableSiteAccess(t, remote, testBasicAuthUser, "new-password")
	expectBasicAuthChallenge(t, anon.getAs("/", browserUA, ok))
	expectStatus(t, anon.getAs("/", browserUA, basicAuthHeader(testBasicAuthUser, "new-password")), http.StatusOK)
	cur := remote.getSiteAccess(owner)
	expectOK(t, remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: false, User: testBasicAuthUser, ExpectedRevision: &cur.Revision}))
	res := anon.get("/articles/secret-article")
	expectStatus(t, res, http.StatusOK)
	expectNotBasicAuthChallenge(t, res, "after disabling")
}

// デスクトップモードの PC で「サイト設定」を保存して「公開サーバーへ反映する」を押しても、公開サーバーの閲覧制限は
// 変わらず、PC に残った v0.2.8 の閲覧制限は PC でも働かない。公開サーバーの閲覧制限は「公開サーバー」画面で変えられる。
func TestDesktopRemoteSettingsPushKeepsSiteAccess(t *testing.T) {
	env := &remoteEnv{local: newDesktopBasicAuthEnv(t, true), remote: newPublicOnlyTLSEnv(t, nil)}
	env.configure("owner", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	localBefore := env.local.storedBasicAuth()

	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	on := decodeSiteAccessReply(t, expectOK(t, adm.postJSON("/admin/remote/site-access", siteAccessForm(true, remoteAccessUser, remoteAccessPass, st.Revision), nil)))
	form := generalSettingsForm()
	form.Set("basic_auth_revision", "")
	form.Set("basic_auth_user", "local-only")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	expectOK(t, adm.postJSON("/admin/remote/settings/push", nil, nil))

	owner := env.remote.token("check", auth.RoleAdmin)
	if got := env.remote.getSiteAccess(owner); got.Revision != on.State.Revision || !got.Active || got.User != remoteAccessUser {
		t.Fatalf("saving and pushing settings changed the remote restriction: %+v", got)
	}
	expectBasicAuthChallenge(t, env.remote.newClient().get("/"))
	expectStatus(t, env.remote.newClient().getAs("/", browserUA, basicAuthHeader(remoteAccessUser, remoteAccessPass)), http.StatusOK)
	if got := env.local.storedBasicAuth(); got != localBefore {
		t.Fatalf("the PC's stored restriction changed: %+v", got)
	}
	expectNotBasicAuthChallenge(t, env.local.newClient().get("/"), "the PC's public site")
}

// 「サイト設定」の導入文は、モードに合わせて閲覧制限に触れる。デスクトップモードでは存在しない閲覧制限を挙げず、
// サーバーモードでは挙げる（日本語・英語とも）。
func TestSiteSettingsIntroMatchesMode(t *testing.T) {
	cases := []struct {
		name       string
		env        *testEnv
		withAccess bool
	}{
		{"desktop", newDesktopBasicAuthEnv(t, true), false},
		{"server", newTestEnv(t), true},
	}
	for _, tc := range cases {
		adm := tc.env.loggedIn(admin)
		ja := adm.get("/admin/settings/general")
		expectStatus(t, ja, http.StatusOK)
		expectStatus(t, adm.get("/admin/set-lang?lang=en"), http.StatusSeeOther)
		en := adm.get("/admin/settings/general")
		expectStatus(t, en, http.StatusOK)
		if tc.withAccess {
			// 見出しの下は短い1文にし、項目名を並べない（左メニュー・各項目と重複するため）。
			expectContains(t, ja.Body, "公開サイトの名前や URL、閲覧制限などを設定します。")
			expectContains(t, en.Body, "Set the public site&#39;s name, URL, access restriction and more.")
			continue
		}
		expectContains(t, ja.Body, "公開サイトの名前や URL などを設定します。")
		expectContains(t, en.Body, "Set the public site&#39;s name, URL and more.")
		if strings.Contains(ja.Body, "閲覧制限") {
			t.Errorf("%s: the Japanese general settings page mentions an access restriction", tc.name)
		}
		if strings.Contains(strings.ToLower(en.Body), "access restriction") {
			t.Errorf("%s: the English general settings page mentions an access restriction", tc.name)
		}
	}
}

// 「サイト運用」画面の閲覧制限の案内は、モードに合わせる。デスクトップモードでは PC に無い「サイト設定」の閲覧制限を
// 案内せず、公開サーバーに保存される設定だと示す。サーバーモードでは、この sorus の「サイト設定」の閲覧制限と互いに
// 独立していることを示す（日本語・英語とも）。
func TestRemoteSiteAccessNoteMatchesMode(t *testing.T) {
	cases := []struct {
		name  string
		local *testEnv
	}{
		{"desktop", newDesktopBasicAuthEnv(t, true)},
		{"server", newTestEnv(t)},
	}
	for _, tc := range cases {
		env := &remoteEnv{local: tc.local, remote: newPublicOnlyTLSEnv(t, nil)}
		env.configure("owner", auth.RoleAdmin)
		adm := env.local.loggedIn(admin)
		ja := adm.get(operationsPagePath)
		expectStatus(t, ja, http.StatusOK)
		expectContains(t, ja.Body, "data-remote-site-access", "ここで保存したときだけ変わります")
		expectStatus(t, adm.get("/admin/set-lang?lang=en"), http.StatusSeeOther)
		en := adm.get(operationsPagePath)
		expectStatus(t, en, http.StatusOK)
		expectContains(t, en.Body, "data-remote-site-access", "it changes only when you save it here")
		if tc.name == "server" {
			expectContains(t, ja.Body, "この sorus の「サイト設定」の閲覧制限とは別の、独立した設定です。どちらかを変えても、もう一方は変わりません。")
			expectContains(t, en.Body, "independent of this sorus&#39;s access restriction under Site settings: changing one does not change the other.")
			continue
		}
		expectContains(t, ja.Body, "公開サーバーに保存される設定で、この PC には保存しません。")
		expectContains(t, en.Body, "This setting is stored on the public server, not on this PC.")
		for _, s := range []string{"「サイト設定」の閲覧制限", "サイト閲覧制限"} {
			if strings.Contains(ja.Body, s) {
				t.Errorf("desktop: the Japanese public server page mentions %q", s)
			}
		}
		for _, s := range []string{"access restriction under site settings", "this pc&#39;s access restriction"} {
			if strings.Contains(strings.ToLower(en.Body), s) {
				t.Errorf("desktop: the English public server page mentions %q", s)
			}
		}
	}
}

// サーバーモードの「サイト設定」の閲覧制限の欄は、招待コードでつないだ公開サーバーの閲覧制限と互いに独立していることを
// 両方向で示す（日本語・英語とも）。
func TestServerSiteSettingsExplainsIndependentRemoteAccess(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	ja := adm.get("/admin/settings/general")
	expectStatus(t, ja, http.StatusOK)
	expectContains(t, ja.Body, `id="basic-auth-remote-note"`, "「設定」→「サイト運用」で別に設定します",
		"ここで変えても公開サーバーの閲覧制限は変わらず、公開サーバーで変えてもここは変わりません。")
	expectStatus(t, adm.get("/admin/set-lang?lang=en"), http.StatusSeeOther)
	en := adm.get("/admin/settings/general")
	expectStatus(t, en, http.StatusOK)
	expectContains(t, en.Body, `id="basic-auth-remote-note"`,
		"Changing it here does not change the public server&#39;s restriction, and changing the public server&#39;s does not change this one.")
}
