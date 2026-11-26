package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/auth"
	"sorus/internal/theme"
	"sorus/static"
)

// 独自テーマの公開サーバーへの反映。PC で取り込んで適用した独自テーマを、「公開サーバーへ反映する」で
// テーマのファイルごと公開サーバーへ送り、公開サーバーは再起動なしにそのテーマで公開ページを配信する。

// newRemoteThemeEnv は独自テーマの置き場所を別々にした PC と公開専用モードの公開サーバーを起動し、PC に管理者の
// トークンで接続する。useTLS なら公開サーバーは本番（AUTOCERT_DOMAIN）と同じく sorus が TLS を直接受ける。
func newRemoteThemeEnv(t *testing.T, useTLS bool) (env *remoteEnv, pcThemes, vpsThemes string) {
	t.Helper()
	pcThemes = filepath.Join(t.TempDir(), "pc-themes")
	vpsThemes = filepath.Join(t.TempDir(), "vps-themes")
	local := newTestEnvWith(t, Config{ThemesDir: pcThemes, BaseURL: testBaseURL}, nil)
	remote := startSyncEnv(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t), ThemesDir: vpsThemes}, nil, useTLS)
	env = &remoteEnv{local: local, remote: remote}
	env.configure("owner", auth.RoleAdmin)
	return env, pcThemes, vpsThemes
}

// themeStylesheetPattern は公開ページの HTML から独自テーマの CSS の URL を取り出す。
var themeStylesheetPattern = regexp.MustCompile(`href="(/custom-themes/[a-z0-9_-]+/theme\.css[^"]*)"`)

// remoteThemeStylesheet は公開サーバーのトップページを開き、data-theme が id であることを確かめて、
// 独自テーマの CSS の URL を返す。
func remoteThemeStylesheet(t *testing.T, reader *client, id string) string {
	t.Helper()
	page := reader.get("/")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `data-theme="`+id+`"`)
	m := themeStylesheetPattern.FindStringSubmatch(page.Body)
	if m == nil || !strings.HasPrefix(m[1], "/custom-themes/"+id+"/theme.css?v=") {
		t.Fatalf("public page does not link the custom theme %q stylesheet: %v\nbody: %.2000s", id, m, page.Body)
	}
	return strings.ReplaceAll(m[1], "&amp;", "&")
}

// pushRemoteSettings は PC の「公開サーバーへ反映する」を押し、応答の文言を返す。
func pushRemoteSettings(t *testing.T, adm *client, wantStatus int) string {
	t.Helper()
	res := adm.postJSON("/admin/remote/settings/push", nil, nil)
	expectStatus(t, res, wantStatus)
	return jsonMessage(t, res)
}

// PC で Zip から取り込んで適用した独自テーマが、反映でファイルごと公開サーバーへ送られ、公開サーバーは再起動なしに
// HTML・CSS・画像を配信する。同じ ID のテーマを PC で更新して反映し直すと、公開サーバーのテーマも新しい内容になる。
func TestRemotePushTransfersCustomTheme(t *testing.T) {
	env, _, vpsThemes := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	reader := env.remote.newClient()

	files := testThemeFiles("paper", "ペーパー")
	files["README.md"] = "公開サーバーには送らない説明書"
	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", files)),
		"installed", "paper", "テーマ「ペーパー」を取り込みました")
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"paper"}}), "/admin/settings/theme")

	msg := pushRemoteSettings(t, adm, http.StatusOK)
	expectContains(t, msg, "ペーパー")
	if got := env.remote.setting("theme"); got != "paper" {
		t.Fatalf("remote theme = %q, want paper", got)
	}
	got, ok := env.remote.server.themeCatalog().Get("paper")
	if !ok || !got.IsCustom || got.Label("ja") != "ペーパー" {
		t.Fatalf("remote catalog paper = %v %+v", ok, got)
	}
	css1 := remoteThemeStylesheet(t, reader, "paper")
	expectContains(t, reader.get(css1).Body, "#123456")
	img := reader.get("/custom-themes/paper/img/bg.png")
	expectStatus(t, img, http.StatusOK)
	if img.Body != files["img/bg.png"] {
		t.Fatalf("remote image = %q", img.Body)
	}
	expectStatus(t, reader.get("/custom-themes/paper/fonts/a.woff"), http.StatusOK)
	// 公開サーバーが使わないファイル（README など）は送らない。
	if _, err := os.Stat(filepath.Join(vpsThemes, "paper", "README.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("README.md was sent to the public server: %v", err)
	}
	// URL に版の無い画像は、使う前に毎回確かめさせる（同じ ID の更新で古い画像を使わせない）。
	if got := img.Header.Get("Cache-Control"); got != customThemeAssetCacheControl {
		t.Errorf("image Cache-Control = %q, want %q", got, customThemeAssetCacheControl)
	}
	if got := reader.get("/custom-themes/paper/img/missing.png").Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("missing asset Cache-Control = %q, want no-store", got)
	}

	// 同じ ID のテーマを PC で更新して反映し直す。
	files = testThemeFiles("paper", "ペーパー改")
	files["theme.css"] = "body { color: #abcdef; background: url(img/bg.png); }\n"
	files["img/bg.png"] = "\x89PNG\r\n\x1a\nnew-image"
	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", files)),
		"installed", "paper", "テーマ「ペーパー改」を取り込みました")
	expectContains(t, pushRemoteSettings(t, adm, http.StatusOK), "ペーパー改")
	css2 := remoteThemeStylesheet(t, reader, "paper")
	if css2 == css1 {
		t.Errorf("stylesheet URL did not change after the update: %s", css2)
	}
	body := reader.get(css2).Body
	expectContains(t, body, "#abcdef")
	expectNotContains(t, body, "#123456")
	if img := reader.get("/custom-themes/paper/img/bg.png"); img.Body != files["img/bg.png"] {
		t.Fatalf("remote image after update = %q", img.Body)
	}
	expectStatus(t, reader.get("/custom-themes/paper/fonts/a.woff"), http.StatusOK)
	if got, _ := env.remote.server.themeCatalog().Get("paper"); got.Label("ja") != "ペーパー改" {
		t.Errorf("remote catalog after update = %+v", got)
	}
	// 結果が分からなかったときのやり直しと同じく、同じ内容をもう一度反映しても同じ状態になる。
	pushRemoteSettings(t, adm, http.StatusOK)
	expectContains(t, reader.get(remoteThemeStylesheet(t, reader, "paper")).Body, "#abcdef")
	expectNoStaging(t, vpsThemes)
}

// expectNoStaging は独自テーマの置き場所に作業ディレクトリ（.install-*）が残っていないことを確かめる。
func expectNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("staging directory left in %s: %s", dir, e.Name())
		}
	}
}

// themePackage は files をトップに置いた Zip を、id の独自テーマとして sync_settings に付ける形にする。
func themePackage(t *testing.T, id string, files map[string]string) *SyncThemePackage {
	t.Helper()
	data := testThemeZip(t, "", files)
	return &SyncThemePackage{ID: id, Archive: data, SHA256: syncThemeDigest(data)}
}

// rawThemeZip は name・body・mode のエントリを順に入れた Zip を返す（危険なエントリを作る）。
func rawThemeZip(t *testing.T, files map[string]string, extra ...zip.FileHeader) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for rel, body := range files {
		w, err := zw.Create(rel)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	for _, h := range extra {
		h := h
		w, err := zw.CreateHeader(&h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("/etc/passwd"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 標準テーマを適用しているときは、従来どおりテーマの ID だけを送り、テーマのファイルは送らない
// （公開サーバーの対応状況の問い合わせもしない）。
func TestRemotePushBuiltinThemeSendsNoFiles(t *testing.T) {
	env, _, vpsThemes := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"news"}}), "/admin/settings/theme")
	msg := pushRemoteSettings(t, adm, http.StatusOK)
	if msg != "サイト基本設定とサイト構成を公開サーバーへ反映しました" {
		t.Errorf("message = %q", msg)
	}
	if got := env.remote.setting("theme"); got != "news" {
		t.Errorf("remote theme = %q, want news", got)
	}
	if _, err := os.Stat(vpsThemes); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pushing a built-in theme touched the public server's theme directory: %v", err)
	}

	local, _ := newCustomThemeEnv(t, nil)
	local.setTheme("news")
	fake := newFakeRemote(t, func(req SyncRequest) (int, string) {
		return http.StatusOK, `{"action":"sync_settings","settings":{"theme":"news","site_menu":[],"front_cards":[]}}`
	})
	configureFake(t, local, fake)
	pushRemoteSettings(t, local.loggedIn(admin), http.StatusOK)
	seen := fake.requests()
	if strings.Count(seen, "POST ") != 1 || strings.Contains(seen, "theme_package") || strings.Contains(seen, SyncActionServerInfo) {
		t.Errorf("requests for a built-in theme = %.1000s", seen)
	}
}

// 独自テーマのファイルを受け取れるのは、サイト設定を反映できる管理者のトークンだけ。PC でも管理者だけが反映できる。
func TestRemotePushCustomThemeRequiresAdmin(t *testing.T) {
	env, pcThemes, vpsThemes := newRemoteThemeEnv(t, false)
	writeTestTheme(t, pcThemes, "paper", "ペーパー")
	if err := env.local.server.themeCatalog().Reload(static.FS); err != nil {
		t.Fatal(err)
	}
	env.local.setTheme("paper")
	before := env.remote.setting("theme")
	pkg := themePackage(t, "paper", testThemeFiles("paper", "ペーパー"))
	req := SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{Theme: strPtr("paper"), ThemePackage: pkg}}
	for _, role := range []auth.Role{auth.RoleEditor, auth.RoleWriter} {
		rec := env.remote.call(env.remote.token("someone", role), req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s token: status %d, want 403", role, rec.Code)
		}
	}
	rec := env.remote.callRaw("", "application/json", []byte(mustJSON(t, req)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(vpsThemes, "paper")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rejected request installed the theme: %v", err)
	}

	expectStatus(t, env.local.loggedIn(chief).postJSON("/admin/remote/settings/push", nil, nil), http.StatusForbidden)
	env.configure("sato", auth.RoleEditor)
	expectStatus(t, env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, nil), http.StatusForbidden)
	if got := env.remote.setting("theme"); got != before {
		t.Errorf("remote theme = %q after rejected pushes, want %q", got, before)
	}
}

// optionalSetting は settings の key の値を返す。無ければ空。
func optionalSetting(t *testing.T, e *syncEnv, key string) string {
	t.Helper()
	var v []string
	if err := e.db.Select(&v, `SELECT value FROM settings WHERE key = ?`, key); err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// 公開サーバーは、壊れた・上限を超えた・危険な・形式の正しくないテーマのファイルを、何も置かずに 422 で断る。
// 断ったときは設定も変えず、すでに公開中のテーマ（同じ ID の以前の版を含む）はそのまま使い続ける。
func TestSyncSettingsRejectsBadThemePackages(t *testing.T) {
	vpsThemes := filepath.Join(t.TempDir(), "vps-themes")
	remote := startSyncEnv(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t), ThemesDir: vpsThemes}, nil, false)
	owner := remote.token("owner", auth.RoleAdmin)
	good := testThemeFiles("paper", "ペーパー")
	rec := remote.call(owner, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{Theme: strPtr("paper"), ThemePackage: themePackage(t, "paper", good)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("initial push: %d %s", rec.Code, rec.Body)
	}
	if res := decodeRecorder[SyncSettingsResponse](t, rec); res.ThemePackage == nil || res.ThemePackage.ID != "paper" {
		t.Fatalf("response theme_package = %+v", res.ThemePackage)
	}

	evil := func(files map[string]string, extra ...zip.FileHeader) *SyncThemePackage {
		data := rawThemeZip(t, files, extra...)
		return &SyncThemePackage{ID: "paper", Archive: data, SHA256: syncThemeDigest(data)}
	}
	v2 := testThemeFiles("paper", "ペーパー改")
	v2["theme.css"] = "body { color: #abcdef; }"
	corrupted := themePackage(t, "paper", v2)
	corrupted.Archive = corrupted.Archive[:len(corrupted.Archive)-10]
	noCSS := testThemeFiles("paper", "ペーパー改")
	delete(noCSS, "theme.css")
	externalCSS := testThemeFiles("paper", "ペーパー改")
	externalCSS["theme.css"] = "body { background: url(https://evil.example/x.png); }"
	slip := testThemeFiles("paper", "ペーパー改")
	slip["../../escape.css"] = "x"
	symlink := zip.FileHeader{Name: "link.css"}
	symlink.SetMode(fs.ModeSymlink | 0o777)
	bomb := testThemeFiles("paper", "ペーパー改")
	bomb["img/bomb.png"] = strings.Repeat("0", theme.MaxExtractedSize+1)
	many := testThemeFiles("paper", "ペーパー改")
	for i := 0; len(many) <= theme.MaxArchiveFiles; i++ {
		many[fmt.Sprintf("img/%d.png", i)] = "x"
	}
	oversized := make([]byte, theme.MaxArchiveSize+1)
	builtinPkg := themePackage(t, "modern", testThemeFiles("modern", "偽"))

	for _, tc := range []struct {
		name     string
		settings SyncSettings
		want     string
	}{
		{"checksum mismatch", SyncSettings{Theme: strPtr("paper"), ThemePackage: corrupted}, "途中で壊れました"},
		{"not a zip", SyncSettings{Theme: strPtr("paper"), ThemePackage: &SyncThemePackage{ID: "paper", Archive: []byte("nope"), SHA256: syncThemeDigest([]byte("nope"))}}, "形式"},
		{"empty", SyncSettings{Theme: strPtr("paper"), ThemePackage: &SyncThemePackage{ID: "paper"}}, "空"},
		{"oversized archive", SyncSettings{Theme: strPtr("paper"), ThemePackage: &SyncThemePackage{ID: "paper", Archive: oversized, SHA256: syncThemeDigest(oversized)}}, "上限"},
		{"zip bomb", SyncSettings{Theme: strPtr("paper"), ThemePackage: themePackage(t, "paper", bomb)}, "上限"},
		{"too many files", SyncSettings{Theme: strPtr("paper"), ThemePackage: themePackage(t, "paper", many)}, "上限"},
		{"zip slip", SyncSettings{Theme: strPtr("paper"), ThemePackage: evil(slip)}, "不正なファイルパス"},
		{"symlink", SyncSettings{Theme: strPtr("paper"), ThemePackage: evil(v2, symlink)}, "不正なファイルパス"},
		{"missing theme.css", SyncSettings{Theme: strPtr("paper"), ThemePackage: themePackage(t, "paper", noCSS)}, "形式"},
		{"external css", SyncSettings{Theme: strPtr("paper"), ThemePackage: themePackage(t, "paper", externalCSS)}, "形式"},
		{"built-in id", SyncSettings{Theme: strPtr("modern"), ThemePackage: builtinPkg}, "標準テーマと同じ ID"},
		{"id differs from theme", SyncSettings{Theme: strPtr("other"), ThemePackage: themePackage(t, "paper", v2)}, "一致しません"},
		{"package without theme", SyncSettings{SiteName: strPtr("変えない"), ThemePackage: themePackage(t, "paper", v2)}, "theme も指定"},
		{"invalid structure", SyncSettings{Theme: strPtr("paper"), ThemePackage: themePackage(t, "paper", v2), FrontSections: &[]FrontSection{{Type: FrontSectionGallery, Limit: -1}}}, "サイト構成"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := tc.settings
			rec := remote.call(owner, SyncRequest{Action: SyncActionSyncSettings, Settings: &settings})
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status %d body %s, want 422 containing %q", rec.Code, rec.Body, tc.want)
			}
			if got := remote.setting("theme"); got != "paper" {
				t.Errorf("remote theme = %q, want paper (unchanged)", got)
			}
			if got := optionalSetting(t, remote, "site_name"); got == "変えない" {
				t.Error("a rejected request changed the site name")
			}
			css, err := os.ReadFile(filepath.Join(vpsThemes, "paper", "theme.css"))
			if err != nil || string(css) != good["theme.css"] {
				t.Errorf("published theme.css = %q, %v; want the previous version", css, err)
			}
			if got, _ := remote.server.themeCatalog().Get("paper"); got.Label("ja") != "ペーパー" {
				t.Errorf("catalog paper = %+v, want the previous version", got)
			}
			if _, ok := remote.server.themeCatalog().Get("modern"); !ok {
				t.Error("the built-in theme disappeared")
			} else if m, _ := remote.server.themeCatalog().Get("modern"); m.IsCustom {
				t.Error("the built-in theme was replaced")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(vpsThemes), "escape.css")); err == nil {
				t.Error("zip slip wrote outside the themes directory")
			}
			expectNoStaging(t, vpsThemes)
			if entries, _ := os.ReadDir(vpsThemes); len(entries) != 1 {
				t.Errorf("themes dir has %d entries, want only paper", len(entries))
			}
		})
	}
}

func decodeRecorder[T any](t *testing.T, rec interface{ Result() *http.Response }) T {
	t.Helper()
	var v T
	res := rec.Result()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

// 公開サーバーでテーマを配置できなかったときは、設定もテーマも変えずに理由を返す。PC はその理由を伝える。
func TestSyncSettingsThemeInstallFailureKeepsSettings(t *testing.T) {
	env, pcThemes, vpsThemes := newRemoteThemeEnv(t, false)
	writeTestTheme(t, pcThemes, "paper", "ペーパー")
	if err := env.local.server.themeCatalog().Reload(static.FS); err != nil {
		t.Fatal(err)
	}
	env.local.setTheme("paper")
	// 公開サーバーの置き場所に、同じ名前のディレクトリでないもの（ファイル）がある。
	if err := os.MkdirAll(vpsThemes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vpsThemes, "paper"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeTheme, beforeName := env.remote.setting("theme"), optionalSetting(t, env.remote, "site_name")
	res := env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, nil)
	expectStatus(t, res, http.StatusBadGateway)
	expectContains(t, jsonMessage(t, res), "配置できませんでした", "変更していません")
	if got := env.remote.setting("theme"); got != beforeTheme {
		t.Errorf("remote theme = %q after a failed install, want %q", got, beforeTheme)
	}
	if got := optionalSetting(t, env.remote, "site_name"); got != beforeName {
		t.Errorf("remote site_name = %q after a failed install, want %q", got, beforeName)
	}
	expectNoStaging(t, vpsThemes)
}

// 同じ ID のテーマの反映が同時に届いても、1つずつ処理し、公開サーバーにはどれか1つの版がそのまま残る
// （版の違うファイルが混ざらない）。反映の途中に公開ページが読んでも、混ざった版は返さない。
func TestSyncSettingsConcurrentThemePushes(t *testing.T) {
	vpsThemes := filepath.Join(t.TempDir(), "vps-themes")
	remote := startSyncEnv(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t), ThemesDir: vpsThemes}, nil, false)
	owner := remote.token("owner", auth.RoleAdmin)
	versions := map[string]string{}
	var pkgs []*SyncThemePackage
	for i := 0; i < 4; i++ {
		files := testThemeFiles("paper", fmt.Sprintf("版%d", i))
		files["theme.css"] = fmt.Sprintf("body { color: #00000%d; background: url(img/bg.png); }\n", i)
		files["img/bg.png"] = fmt.Sprintf("png-%d", i)
		versions[files["theme.css"]] = files["img/bg.png"]
		pkgs = append(pkgs, themePackage(t, "paper", files))
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var mixed sync.Map
	for r := 0; r < 2; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := remote.get("/custom-themes/paper/theme.css")
				if rec.Code == http.StatusOK {
					if _, ok := versions[rec.Body.String()]; !ok {
						mixed.Store(rec.Body.String(), true)
					}
				}
			}
		}()
	}
	var wg sync.WaitGroup
	codes := make([]int, len(pkgs))
	for i, pkg := range pkgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := remote.call(owner, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{Theme: strPtr("paper"), ThemePackage: pkg}})
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("push %d: status %d", i, code)
		}
	}
	mixed.Range(func(k, _ any) bool {
		t.Errorf("a reader saw an unknown theme.css: %q", k)
		return true
	})
	css, err := os.ReadFile(filepath.Join(vpsThemes, "paper", "theme.css"))
	if err != nil {
		t.Fatal(err)
	}
	img, _ := os.ReadFile(filepath.Join(vpsThemes, "paper", "img", "bg.png"))
	wantImg, ok := versions[string(css)]
	if !ok || string(img) != wantImg {
		t.Fatalf("installed theme mixes versions: css %q, image %q", css, img)
	}
	got, _ := remote.server.themeCatalog().Get("paper")
	if !strings.Contains(string(css), "#00000"+strings.TrimPrefix(got.Label("ja"), "版")) {
		t.Errorf("catalog label %q does not match the installed css %q", got.Label("ja"), css)
	}
	expectNoStaging(t, vpsThemes)
}

// PC は独自テーマのファイルを読む間、同じ ID の取り込み直し・削除と重ならないようにする（themeMu を待つ）。
func TestRemotePushWaitsForThemeOperations(t *testing.T) {
	env, pcThemes, _ := newRemoteThemeEnv(t, false)
	writeTestTheme(t, pcThemes, "paper", "ペーパー")
	if err := env.local.server.themeCatalog().Reload(static.FS); err != nil {
		t.Fatal(err)
	}
	env.local.setTheme("paper")
	adm := env.local.loggedIn(admin)
	env.local.server.themeMu.Lock()
	done := make(chan int, 1)
	go func() {
		done <- adm.postJSON("/admin/remote/settings/push", nil, nil).StatusCode
	}()
	select {
	case code := <-done:
		env.local.server.themeMu.Unlock()
		t.Fatalf("push finished (%d) while a theme operation held the lock", code)
	case <-time.After(200 * time.Millisecond):
	}
	env.local.server.themeMu.Unlock()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("push after the lock was released: %d", code)
	}
}

// PC で送る前に分かる問題（上限を超えるテーマ・取り込んだ後に壊れたテーマ）は、公開サーバーに何も送らずに伝える。
func TestRemotePushRejectsUnsendableThemeLocally(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{"too many files", func(t *testing.T, root string) {
			for i := 0; i < theme.MaxArchiveFiles; i++ {
				if err := os.WriteFile(filepath.Join(root, "img", fmt.Sprintf("%d.png", i)), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}, "上限"},
		{"archive too large", func(t *testing.T, root string) {
			noise := make([]byte, theme.MaxArchiveSize+1)
			if _, err := rand.Read(noise); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "img", "noise.png"), noise, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "上限"},
		{"broken after install", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "theme.css"), []byte("@import url(x.css);"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "正しくない"},
		{"skipped by the catalog", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "theme.css")); err != nil {
				t.Fatal(err)
			}
		}, "正しくない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, themesDir := newCustomThemeEnv(t, func(dir string) { writeTestTheme(t, dir, "paper", "ペーパー") })
			tc.setup(t, filepath.Join(themesDir, "paper"))
			if tc.name == "skipped by the catalog" {
				if err := local.server.themeCatalog().Reload(static.FS); err != nil {
					t.Fatal(err)
				}
				if _, ok := local.server.themeCatalog().Get("paper"); ok {
					t.Fatal("the broken theme is still in the catalog")
				}
			}
			local.setTheme("paper")
			fake := newFakeRemote(t, func(req SyncRequest) (int, string) { return http.StatusOK, `{}` })
			configureFake(t, local, fake)
			res := local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, nil)
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, jsonMessage(t, res), tc.want, settingsNotSentNote)
			if seen := fake.requests(); seen != "" {
				t.Errorf("requests were sent: %.300s", seen)
			}
		})
	}
}

// 公開サーバーの sorus が古く、独自テーマのファイルを受け取れないときは、設定も送らずに更新を案内する
// （古い公開サーバーはテーマのファイルを無視して設定だけを変えてしまうため）。
func TestRemotePushCustomThemeToOldServer(t *testing.T) {
	for name, info := range map[string]func() (int, string){
		"server_info unknown": func() (int, string) {
			return http.StatusBadRequest, `{"message":"未対応の action です: \"server_info\""}`
		},
		"no custom_themes": func() (int, string) { return http.StatusOK, `{"action":"server_info","schedule":true}` },
	} {
		t.Run(name, func(t *testing.T) {
			local, _ := newCustomThemeEnv(t, func(dir string) { writeTestTheme(t, dir, "paper", "ペーパー") })
			local.setTheme("paper")
			fake := newFakeRemote(t, func(req SyncRequest) (int, string) {
				if req.Action == SyncActionServerInfo {
					return info()
				}
				return http.StatusOK, `{"action":"sync_settings","settings":{"site_menu":[],"front_cards":[]}}`
			})
			configureFake(t, local, fake)
			res := local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, nil)
			expectStatus(t, res, http.StatusBadGateway)
			expectContains(t, jsonMessage(t, res), "公開サーバーの sorus が古い", "ペーパー", "更新", settingsNotSentNote)
			if seen := fake.requests(); strings.Contains(seen, SyncActionSyncSettings) {
				t.Errorf("settings were sent to an old server: %.300s", seen)
			}
		})
	}
}

// 通信に失敗したときは、何も変わっていないのか、反映されたかどうか分からないのかを伝える。分からないときは
// やり直しを案内する（同じ内容で上書きし直すだけなので安全）。
func TestRemotePushCustomThemeFailures(t *testing.T) {
	supports := `{"action":"server_info","schedule":true,"custom_themes":true,"max_theme_package_bytes":10485760}`
	for _, tc := range []struct {
		name       string
		info       func() (int, string)
		settings   func(req SyncRequest) (int, string)
		closed     bool
		wantStatus int
		want       []string
		flag       string
	}{
		{name: "server unreachable", closed: true, wantStatus: http.StatusBadGateway,
			want: []string{"接続できませんでした", settingsNotSentNote}, flag: "not_sent"},
		{name: "server_info fails", info: func() (int, string) { return http.StatusInternalServerError, `` }, wantStatus: http.StatusBadGateway,
			want: []string{settingsNotSentNote}, flag: "not_sent"},
		{name: "connection dropped after sending", settings: func(SyncRequest) (int, string) { return fakeDropConnection, "" },
			wantStatus: http.StatusBadGateway, want: []string{"分かりません", "もう一度"}, flag: "outcome_unknown"},
		{name: "generic server error", settings: func(SyncRequest) (int, string) {
			return http.StatusInternalServerError, `{"message":"Internal Server Error"}`
		},
			wantStatus: http.StatusBadGateway, want: []string{"分かりません"}, flag: "outcome_unknown"},
		{name: "proxy timeout", settings: func(SyncRequest) (int, string) { return http.StatusGatewayTimeout, `` },
			wantStatus: http.StatusBadGateway, want: []string{"分かりません"}, flag: "outcome_unknown"},
		{name: "server explains a failure", settings: func(SyncRequest) (int, string) {
			return http.StatusInternalServerError, `{"message":"公開サーバーで独自テーマ「ペーパー」を配置できませんでした。設定とテーマは変更していません。もう一度お試しください"}`
		}, wantStatus: http.StatusBadGateway, want: []string{"配置できませんでした", "変更していません"}},
		{name: "server could not undo the replacement", settings: func(SyncRequest) (int, string) {
			b, _ := json.Marshal(map[string]string{"message": syncThemeNotRestoredMessage("ペーパー", "独自テーマ「%s」を配置できず、前の版も元の場所に戻せませんでした。")})
			return http.StatusInternalServerError, string(b)
		}, wantStatus: http.StatusBadGateway, want: []string{"前の版も元の場所に戻せませんでした", "設定は変更していません", "もう一度「公開サーバーへ反映する」を押す"}},
		{name: "rejected", settings: func(SyncRequest) (int, string) {
			return http.StatusUnprocessableEntity, `{"message":"独自テーマ「paper」の形式（theme.json・theme.css）が正しくないため、公開サーバーで受け付けませんでした"}`
		}, wantStatus: http.StatusBadGateway, want: []string{"受け付けませんでした", settingsRejectedNote}, flag: "not_sent"},
		{name: "too large for the server", settings: func(SyncRequest) (int, string) { return http.StatusRequestEntityTooLarge, `` },
			wantStatus: http.StatusBadGateway, want: []string{"大きすぎて", "プロキシ", settingsRejectedNote}, flag: "not_sent"},
		{name: "theme not confirmed", settings: func(SyncRequest) (int, string) {
			return http.StatusOK, `{"action":"sync_settings","settings":{"theme":"paper","site_menu":[],"front_cards":[]}}`
		}, wantStatus: http.StatusBadGateway, want: []string{"サイト基本設定は公開サーバーへ反映しました", "確かめられませんでした"}, flag: "partial"},
		{name: "unreadable success", settings: func(SyncRequest) (int, string) { return http.StatusOK, `{"hello":"world"}` },
			wantStatus: http.StatusBadGateway, want: []string{"分かりません"}, flag: "outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, _ := newCustomThemeEnv(t, func(dir string) { writeTestTheme(t, dir, "paper", "ペーパー") })
			local.setTheme("paper")
			fake := newFakeRemote(t, func(req SyncRequest) (int, string) {
				if req.Action == SyncActionServerInfo {
					if tc.info != nil {
						return tc.info()
					}
					return http.StatusOK, supports
				}
				if req.Settings == nil || req.Settings.ThemePackage == nil || req.Settings.ThemePackage.ID != "paper" {
					return http.StatusBadRequest, `{"message":"no theme package"}`
				}
				return tc.settings(req)
			})
			configureFake(t, local, fake)
			if tc.closed {
				fake.srv.Close()
			}
			res := local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, nil)
			expectStatus(t, res, tc.wantStatus)
			expectContains(t, jsonMessage(t, res), tc.want...)
			var flags map[string]any
			if err := json.Unmarshal([]byte(res.Body), &flags); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"not_sent", "outcome_unknown", "partial"} {
				if got := flags[f] == true; got != (f == tc.flag) {
					t.Errorf("%s = %v, want %v (body %s)", f, flags[f], f == tc.flag, res.Body)
				}
			}
			if tc.info != nil && strings.Contains(fake.requests(), SyncActionSyncSettings) {
				t.Error("settings were sent although the capability check failed")
			}
		})
	}
}

// 公開サーバーの閲覧制限（Basic 認証）が有効でも、PC はトークンで独自テーマを反映できる。反映しても閲覧制限は
// 変わらず、テーマのファイルもパスワードを知る人にだけ、共有キャッシュに残さない形で配信される。
func TestRemotePushCustomThemeUnderSiteAccess(t *testing.T) {
	env, _, _ := newRemoteThemeEnv(t, true)
	adm := env.local.loggedIn(admin)
	owner := env.remote.token("owner2", auth.RoleAdmin)
	on := env.remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")})
	expectStatus(t, on, http.StatusOK)
	before := env.remote.getSiteAccess(owner)

	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", testThemeFiles("paper", "ペーパー"))),
		"installed", "paper", "テーマ「ペーパー」を取り込みました")
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"paper"}}), "/admin/settings/theme")
	pushRemoteSettings(t, adm, http.StatusOK)

	if after := env.remote.getSiteAccess(owner); after.Revision != before.Revision || !after.Active || after.User != remoteAccessUser {
		t.Fatalf("pushing a custom theme changed the restriction: %+v", after)
	}
	reader := env.remote.newClient()
	for _, p := range []string{"/", "/custom-themes/paper/theme.css", "/custom-themes/paper/img/bg.png"} {
		res := reader.get(p)
		expectBasicAuthChallenge(t, res)
		expectNotContains(t, res.Body, "#123456", "PNG")
	}
	good := basicAuthHeader(remoteAccessUser, remoteAccessPass)
	page := reader.getAs("/", browserUA, good)
	expectStatus(t, page, http.StatusOK)
	m := themeStylesheetPattern.FindStringSubmatch(page.Body)
	if m == nil {
		t.Fatalf("public page does not link the custom theme: %.1000s", page.Body)
	}
	for _, p := range []string{strings.ReplaceAll(m[1], "&amp;", "&"), "/custom-themes/paper/img/bg.png"} {
		res := reader.getAs(p, browserUA, good)
		expectStatus(t, res, http.StatusOK)
		if got := res.Header.Get("Cache-Control"); got != basicAuthCacheControl {
			t.Errorf("%s Cache-Control = %q, want %q", p, got, basicAuthCacheControl)
		}
	}
}

// URL から取り込んだ独自テーマも同じく公開サーバーへ送られ、取り込み元の URL も公開サーバーのテーマに残る。
func TestRemotePushTransfersURLInstalledTheme(t *testing.T) {
	env, _, _ := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	const source = "https://github.com/example/fancy"
	orig := themeDownloadArchive
	themeDownloadArchive = func(ctx context.Context, rawURL string) (*theme.DownloadedArchive, error) {
		return &theme.DownloadedArchive{Data: testThemeZip(t, "fancy-main", testThemeFiles("fancy", "ファンシー")),
			Hint: "fancy", SourceURL: rawURL}, nil
	}
	t.Cleanup(func() { themeDownloadArchive = orig })
	expectThemeFlash(t, adm, adm.postForm("/admin/settings/theme/install-url", url.Values{"theme_url": {source}}),
		"installed", "fancy", "テーマ「ファンシー」を取り込みました")
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"fancy"}}), "/admin/settings/theme")

	pushRemoteSettings(t, adm, http.StatusOK)
	got, ok := env.remote.server.themeCatalog().Get("fancy")
	if !ok || !got.IsCustom || got.SourceURL != source {
		t.Fatalf("remote catalog fancy = %v %+v", ok, got)
	}
	reader := env.remote.newClient()
	expectContains(t, reader.get(remoteThemeStylesheet(t, reader, "fancy")).Body, "#123456")
}

func intPtr(v int) *int { return &v }

// 「公開サーバーへ反映する」は、未反映の設定があるときに管理画面の共通バナーへ1つだけ出す（SPEC §2-3）。どの画面でも
// 同じ操作（同じ API）を使い、結果をトーストが消えた後も読めるようバナーに残す。未反映が無ければ出さない。
func TestRemotePushButtonsShareTheFlow(t *testing.T) {
	env, _, _ := newRemoteThemeEnv(t, false)
	adm := env.local.loggedIn(admin)
	paths := []string{"/admin", "/admin/settings/theme", "/admin/settings/structure", "/admin/settings/remote", "/admin/settings/general"}
	for _, path := range paths {
		page := adm.get(path)
		expectStatus(t, page, http.StatusOK)
		expectNotContains(t, page.Body, `id="remote-push-settings"`, `id="remote-dirty-banner"`)
	}
	env.local.markAllRemoteDirty()
	for _, path := range paths {
		page := adm.get(path)
		expectStatus(t, page, http.StatusOK)
		expectContains(t, page.Body, `id="remote-dirty-banner"`, `data-running-label="反映中…"`, `id="remote-push-result"`, "/static/js/remote_sync.js",
			`data-remote-dirty-item="general"`, `data-remote-dirty-item="theme"`, `data-remote-dirty-item="structure"`)
		if n := strings.Count(page.Body, `id="remote-push-settings"`); n != 1 {
			t.Errorf("%s has %d push buttons, want 1 in the shared banner", path, n)
		}
	}
	js := env.local.newClient().get("/static/js/remote_sync.js")
	expectStatus(t, js, http.StatusOK)
	expectContains(t, js.Body, "'/admin/remote/settings/push'", "remote-push-result", "data-running-label")
}
