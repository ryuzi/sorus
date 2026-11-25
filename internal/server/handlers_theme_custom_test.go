package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/theme"
)

// testThemeFiles は検証を通るカスタムテーマ id のファイル（テーマのディレクトリからの相対パス → 中身）。
func testThemeFiles(id, nameJA string) map[string]string {
	return map[string]string{
		"theme.json": `{"name":{"ja":"` + nameJA + `","en":"` + id + `"},"description":{"ja":"テスト用のテーマ"},` +
			`"color_scheme":"light","order":90}`,
		"theme.css":    "body { color: #123456; background: url(img/bg.png); }\n",
		"img/bg.png":   "\x89PNG\r\n\x1a\nfake",
		"fonts/a.woff": "wOFFfake",
	}
}

// writeTestTheme は dir/<id>/ にカスタムテーマを置く。
func writeTestTheme(t *testing.T, dir, id, nameJA string) string {
	t.Helper()
	root := filepath.Join(dir, id)
	for rel, body := range testThemeFiles(id, nameJA) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// testThemeZip は files を wrap/ の下に入れた Zip を返す（wrap が空ならトップに置く）。
func testThemeZip(t *testing.T, wrap string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for rel, body := range files {
		name := rel
		if wrap != "" {
			name = wrap + "/" + rel
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newCustomThemeEnv は themesDir をカスタムテーマの置き場所にしたサーバーを起動する。seed はサーバーの起動前に
// themesDir へテーマを置く。
func newCustomThemeEnv(t *testing.T, seed func(themesDir string)) (*testEnv, string) {
	t.Helper()
	themesDir := filepath.Join(t.TempDir(), "themes")
	if seed != nil {
		seed(themesDir)
	}
	return newTestEnvWith(t, Config{ThemesDir: themesDir, BaseURL: testBaseURL}, nil), themesDir
}

// setTheme は保存済みの公開テーマを id にする。
func (e *testEnv) setTheme(id string) {
	e.t.Helper()
	if _, err := e.db.Exec(`INSERT INTO settings (key, value) VALUES ('theme', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, id); err != nil {
		e.t.Fatal(err)
	}
}

// rawGet は path を加工せずに（.. や %2e をそのまま）送る。
func (c *client) rawGet(method, rawPath string) *response {
	c.t.Helper()
	u, err := url.Parse(c.env.srv.URL)
	if err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(method, c.env.srv.URL, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.URL = &url.URL{Scheme: u.Scheme, Host: u.Host, Opaque: rawPath}
	return c.do(req)
}

// postThemeZip は Zip を theme_file としてテーマの取り込みに送る。
func (c *client) postThemeZip(filename string, data []byte) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField(CSRFFormField, c.csrfToken()); err != nil {
		c.t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("theme_file", filename)
	if err != nil {
		c.t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+"/admin/settings/theme/install-zip", &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "text/html")
	return c.do(req)
}

// expectThemeFlash は応答がテーマ設定の画面へ、通知 key（と theme_id）を付けて戻すことを確かめ、
// その画面を開いて文言 wantMsg が出ることを確かめる。
func expectThemeFlash(t *testing.T, c *client, res *response, key, id, wantMsg string) {
	t.Helper()
	loc := expectRedirect(t, res, themeSettingsPath+"?")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("theme_flash"); got != key {
		t.Fatalf("theme_flash = %q, want %q (Location %q)", got, key, loc)
	}
	if got := u.Query().Get("theme_id"); got != id {
		t.Fatalf("theme_id = %q, want %q (Location %q)", got, id, loc)
	}
	page := c.get(loc)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, wantMsg)
}

// expectUnauthorized は未ログイン時の POST 等に対する未認証拒否（401 Unauthorized）を確かめる。
func expectUnauthorized(t *testing.T, res *response) {
	t.Helper()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 Unauthorized", res.StatusCode)
	}
}

func TestCustomThemeAssetHandler(t *testing.T) {
	var themeRoot string
	env, themesDir := newCustomThemeEnv(t, func(dir string) {
		themeRoot = writeTestTheme(t, dir, "acme", "アクメ")
		// テーマのディレクトリの外のファイル。パストラバーサル・シンボリックリンクで読めてはいけない。
		if err := os.WriteFile(filepath.Join(dir, "secret.css"), []byte("SECRET"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(themeRoot, ".hidden.css"), []byte("HIDDEN"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	symlinked := os.Symlink(filepath.Join(themesDir, "secret.css"), filepath.Join(themeRoot, "link.css")) == nil
	c := env.newClient()

	t.Run("serves css with type and cache headers", func(t *testing.T) {
		res := c.get("/custom-themes/acme/theme.css")
		expectStatus(t, res, http.StatusOK)
		if got := res.Header.Get("Content-Type"); got != "text/css; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := res.Header.Get("Cache-Control"); got != "public, max-age=3600" {
			t.Errorf("Cache-Control = %q", got)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q", got)
		}
		expectContains(t, res.Body, "#123456")
	})
	t.Run("serves images and fonts in subdirectories", func(t *testing.T) {
		for p, ctype := range map[string]string{"/custom-themes/acme/img/bg.png": "image/png",
			"/custom-themes/acme/fonts/a.woff": "font/woff"} {
			res := c.get(p)
			expectStatus(t, res, http.StatusOK)
			if got := res.Header.Get("Content-Type"); got != ctype {
				t.Errorf("%s Content-Type = %q, want %q", p, got, ctype)
			}
		}
		head := c.rawGet(http.MethodHead, "/custom-themes/acme/theme.css")
		expectStatus(t, head, http.StatusOK)
	})
	t.Run("rejects traversal and files outside the asset types", func(t *testing.T) {
		paths := []string{
			"/custom-themes/acme/../secret.css",
			"/custom-themes/acme/%2e%2e/secret.css",
			"/custom-themes/acme/..%2fsecret.css",
			"/custom-themes/acme/img/../../secret.css",
			"/custom-themes/acme/..%5csecret.css",
			"/custom-themes/../custom-themes/acme/theme.css",
			"/custom-themes/acme//theme.css",
			"/custom-themes/acme/theme.json",
			"/custom-themes/acme/.hidden.css",
			"/custom-themes/acme/",
			"/custom-themes/acme/img",
		}
		if symlinked {
			paths = append(paths, "/custom-themes/acme/link.css")
		}
		for _, p := range paths {
			res := c.rawGet(http.MethodGet, p)
			if res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusBadRequest {
				t.Errorf("GET %s: status %d, want 404 or 400", p, res.StatusCode)
			}
			if strings.Contains(res.Body, "SECRET") || strings.Contains(res.Body, "HIDDEN") || strings.Contains(res.Body, "color_scheme") {
				t.Errorf("GET %s leaked a file: %.200s", p, res.Body)
			}
		}
	})
	t.Run("unknown and built-in themes are 404", func(t *testing.T) {
		for _, p := range []string{"/custom-themes/nope/theme.css", "/custom-themes/modern/theme.css",
			"/custom-themes/ACME/theme.css", "/custom-themes/acme%20/theme.css"} {
			expectStatus(t, c.rawGet(http.MethodGet, p), http.StatusNotFound)
		}
	})
	t.Run("public pages link the custom stylesheet", func(t *testing.T) {
		env.setTheme("acme")
		res := c.get("/")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<link rel="stylesheet" href="/custom-themes/acme/theme.css?v=`, `data-theme="acme"`)
	})
}

func TestInstallThemeFromURL(t *testing.T) {
	zipData := testThemeZip(t, "fancy-main", testThemeFiles("fancy", "ファンシー"))
	// 取り込み元の Zipball を配るローカルの https サーバー。SSRF 対策の本物のクライアントはループバックに繋がないため、
	// テストでは httptest の TLS クライアント（このサーバーだけを信頼する）で取得して Zip の取り込みに渡す。
	zipball := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fancy-main.zip" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipData)
	}))
	t.Cleanup(zipball.Close)

	env, _ := newCustomThemeEnv(t, nil)
	adm := env.loggedIn(admin)

	t.Run("installs from a local zipball and reloads the catalog", func(t *testing.T) {
		var gotURL string
		orig := themeDownloadArchive
		themeDownloadArchive = func(ctx context.Context, rawURL string) (*theme.DownloadedArchive, error) {
			gotURL = rawURL
			// ダウンロード中に themeMu がロックされていないことを確認
			if !env.server.themeMu.TryLock() {
				t.Error("themeMu is locked during DownloadArchive")
			} else {
				env.server.themeMu.Unlock()
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				return nil, err
			}
			resp, err := zipball.Client().Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			return &theme.DownloadedArchive{
				Data:      data,
				Hint:      "fancy",
				SourceURL: rawURL,
			}, nil
		}
		t.Cleanup(func() { themeDownloadArchive = orig })

		res := adm.postForm("/admin/settings/theme/install-url", url.Values{"theme_url": {"  " + zipball.URL + "/fancy-main.zip "}})
		expectThemeFlash(t, adm, res, "installed", "fancy", "テーマ「ファンシー」を取り込みました")
		if gotURL != zipball.URL+"/fancy-main.zip" {
			t.Errorf("DownloadArchive gotURL = %q, want %q", gotURL, zipball.URL+"/fancy-main.zip")
		}
		got, ok := env.server.themeCatalog().Get("fancy")
		if !ok || !got.IsCustom {
			t.Fatalf("catalog has fancy = %v (%+v), want a custom theme", ok, got)
		}
		// 取り込んだテーマはすぐに選べ、配信できる。
		page := adm.get(themeSettingsPath)
		expectContains(t, page.Body, `value="fancy"`)
		expectStatus(t, adm.get("/custom-themes/fancy/theme.css"), http.StatusOK)
		expectRedirect(t, adm.postForm(themeSettingsPath, url.Values{"theme": {"fancy"}}), themeSettingsPath)
		expectContains(t, env.newClient().get("/").Body, "/custom-themes/fancy/theme.css")
	})

	t.Run("blocks private and non-https URLs", func(t *testing.T) {
		for _, u := range []string{
			"https://127.0.0.1/theme.zip",
			"https://10.0.0.8/theme.zip",
			"https://[::1]/theme.zip",
			"https://169.254.169.254/latest/theme.zip",
			"http://example.com/theme.zip",
			zipball.URL + "/fancy-main.zip", // ループバックの httptest サーバーも本物のクライアントでは拒む
		} {
			res := adm.postForm("/admin/settings/theme/install-url", url.Values{"theme_url": {u}})
			expectThemeFlash(t, adm, res, "blocked_url", "", "プライベート IP や不正な接続先のため")
		}
	})

	t.Run("reports unsupported and empty URLs", func(t *testing.T) {
		res := adm.postForm("/admin/settings/theme/install-url", url.Values{"theme_url": {"https://example.com/not-a-repo"}})
		expectThemeFlash(t, adm, res, "unsupported_url", "", "テーマの URL を読み取れません")
		res = adm.postForm("/admin/settings/theme/install-url", url.Values{"theme_url": {"  "}})
		expectThemeFlash(t, adm, res, "no_url", "", "取り込むテーマの URL を入力してください")
	})

	t.Run("requires admin and CSRF", func(t *testing.T) {
		downloadCalls := 0
		orig := themeDownloadArchive
		defer func() { themeDownloadArchive = orig }()
		themeDownloadArchive = func(ctx context.Context, rawURL string) (*theme.DownloadedArchive, error) {
			downloadCalls++
			return nil, errors.New("stub download called unexpectedly")
		}

		form := url.Values{"theme_url": {"https://127.0.0.1/theme.zip"}}
		expectStatus(t, adm.postRaw("/admin/settings/theme/install-url", form), http.StatusForbidden)
		expectStatus(t, env.loggedIn(studentA).postForm("/admin/settings/theme/install-url", form), http.StatusForbidden)
		expectStatus(t, env.loggedIn(chief).postForm("/admin/settings/theme/install-url", form), http.StatusForbidden)
		anonRes := env.newClient().postForm("/admin/settings/theme/install-url", form)
		expectUnauthorized(t, anonRes)

		if downloadCalls != 0 {
			t.Errorf("themeDownloadArchive was called %d times on unauthorized/forbidden requests; want 0", downloadCalls)
		}
	})
}

func TestInstallThemeFromZip(t *testing.T) {
	env, themesDir := newCustomThemeEnv(t, nil)
	adm := env.loggedIn(admin)

	t.Run("installs an uploaded zip and reloads the catalog", func(t *testing.T) {
		res := adm.postThemeZip("paper.zip", testThemeZip(t, "paper", testThemeFiles("paper", "ペーパー")))
		expectThemeFlash(t, adm, res, "installed", "paper", "テーマ「ペーパー」を取り込みました")
		if _, err := os.Stat(filepath.Join(themesDir, "paper", "theme.css")); err != nil {
			t.Fatalf("installed theme.css: %v", err)
		}
		if got, ok := env.server.themeCatalog().Get("paper"); !ok || !got.IsCustom || got.Label("ja") != "ペーパー" {
			t.Fatalf("catalog paper = %v %+v", ok, got)
		}
		expectContains(t, adm.get(themeSettingsPath).Body, `value="paper"`)
		// 同じ ID の Zip は置き換える（更新）。
		files := testThemeFiles("paper", "ペーパー改")
		files["theme.css"] = "body { color: #abcdef; }\n"
		res = adm.postThemeZip("paper.zip", testThemeZip(t, "paper", files))
		expectThemeFlash(t, adm, res, "installed", "paper", "テーマ「ペーパー改」を取り込みました")
		expectContains(t, adm.get("/custom-themes/paper/theme.css").Body, "#abcdef")
	})

	t.Run("reports invalid, built-in and unsafe archives", func(t *testing.T) {
		noCSS := testThemeFiles("broken", "壊れた")
		delete(noCSS, "theme.css")
		expectThemeFlash(t, adm, adm.postThemeZip("broken.zip", testThemeZip(t, "broken", noCSS)),
			"invalid", "", "テーマの形式（theme.json・theme.css）が正しくありません")
		if _, ok := env.server.themeCatalog().Get("broken"); ok {
			t.Error("invalid theme was added to the catalog")
		}

		expectThemeFlash(t, adm, adm.postThemeZip("modern.zip", testThemeZip(t, "modern", testThemeFiles("modern", "偽"))),
			"builtin", "", "ビルトインテーマと同じ名前のテーマは取り込めません")
		if got := env.server.themeCatalog().Resolve("modern"); got.IsCustom {
			t.Error("built-in theme was replaced by an upload")
		}

		evil := testThemeFiles("evil", "悪")
		evil["../escape.css"] = "x"
		expectThemeFlash(t, adm, adm.postThemeZip("evil.zip", testThemeZip(t, "", evil)),
			"unsafe_archive", "", "アーカイブに不正なファイルパス")
		if _, err := os.Stat(filepath.Join(filepath.Dir(themesDir), "escape.css")); err == nil {
			t.Error("zip slip wrote outside the themes directory")
		}

		expectThemeFlash(t, adm, adm.postThemeZip("junk.zip", []byte("not a zip")),
			"invalid", "", "テーマの形式（theme.json・theme.css）が正しくありません")
	})

	t.Run("rejects oversized and missing uploads", func(t *testing.T) {
		big := make([]byte, theme.MaxArchiveSize+1)
		expectThemeFlash(t, adm, adm.postThemeZip("big.zip", big), "too_large", "", "上限（Zip は 10MB、展開後は 50MB）")
		res := adm.postForm("/admin/settings/theme/install-zip", url.Values{})
		expectThemeFlash(t, adm, res, "no_file", "", "取り込むテーマの Zip ファイルを選んでください")
	})

	t.Run("requires admin and CSRF", func(t *testing.T) {
		zipBytes := testThemeZip(t, "unauthed", testThemeFiles("unauthed", "未認証"))
		expectStatus(t, adm.postRaw("/admin/settings/theme/install-zip", url.Values{}), http.StatusForbidden)
		expectStatus(t, env.loggedIn(studentA).postThemeZip("unauthed.zip", zipBytes), http.StatusForbidden)
		expectStatus(t, env.loggedIn(chief).postThemeZip("unauthed.zip", zipBytes), http.StatusForbidden)
		anonRes := env.newClient().postThemeZip("unauthed.zip", zipBytes)
		expectUnauthorized(t, anonRes)
		if _, ok := env.server.themeCatalog().Get("unauthed"); ok {
			t.Error("anonymous request created theme in catalog")
		}
		if _, err := os.Stat(filepath.Join(themesDir, "unauthed")); err == nil {
			t.Error("anonymous request created theme on disk")
		}
	})
}

func TestUninstallCustomTheme(t *testing.T) {
	env, themesDir := newCustomThemeEnv(t, func(dir string) {
		writeTestTheme(t, dir, "acme", "アクメ")
		writeTestTheme(t, dir, "beta", "ベータ")
	})
	env.setTheme("acme")
	adm := env.loggedIn(admin)
	uninstall := func(id string) *response {
		return adm.postForm("/admin/settings/theme/uninstall", url.Values{"theme_id": {id}})
	}

	t.Run("refuses the applied theme", func(t *testing.T) {
		expectThemeFlash(t, adm, uninstall("acme"), "in_use", "", "適用中のテーマは削除できません")
		if _, err := os.Stat(filepath.Join(themesDir, "acme")); err != nil {
			t.Fatalf("applied theme was removed: %v", err)
		}
		if _, ok := env.server.themeCatalog().Get("acme"); !ok {
			t.Fatal("applied theme left the catalog")
		}
	})

	t.Run("removes an unused custom theme and updates the catalog", func(t *testing.T) {
		expectStatus(t, adm.get("/custom-themes/beta/theme.css"), http.StatusOK)
		expectThemeFlash(t, adm, uninstall("beta"), "uninstalled", "beta", "カスタムテーマ「beta」を削除しました")
		if _, err := os.Stat(filepath.Join(themesDir, "beta")); !os.IsNotExist(err) {
			t.Fatalf("beta directory still exists: %v", err)
		}
		if _, ok := env.server.themeCatalog().Get("beta"); ok {
			t.Fatal("beta is still in the catalog")
		}
		expectStatus(t, adm.get("/custom-themes/beta/theme.css"), http.StatusNotFound)
		if strings.Contains(adm.get(themeSettingsPath).Body, `value="beta"`) {
			t.Error("settings page still lists beta")
		}
		expectThemeFlash(t, adm, uninstall("beta"), "not_found", "", "削除するカスタムテーマが見つかりません")
	})

	t.Run("refuses built-in and malformed IDs", func(t *testing.T) {
		expectThemeFlash(t, adm, uninstall("tech"), "builtin_delete", "", "ビルトインテーマは削除できません")
		for _, id := range []string{"../acme", "acme/..", ""} {
			expectThemeFlash(t, adm, uninstall(id), "not_found", "", "削除するカスタムテーマが見つかりません")
		}
		if _, err := os.Stat(filepath.Join(themesDir, "acme")); err != nil {
			t.Fatalf("acme was removed: %v", err)
		}
	})

	t.Run("requires admin and CSRF", func(t *testing.T) {
		form := url.Values{"theme_id": {"acme"}}
		expectStatus(t, adm.postRaw("/admin/settings/theme/uninstall", form), http.StatusForbidden)
		expectStatus(t, env.loggedIn(studentB).postForm("/admin/settings/theme/uninstall", form), http.StatusForbidden)
		expectStatus(t, env.loggedIn(chief).postForm("/admin/settings/theme/uninstall", form), http.StatusForbidden)
		anonRes := env.newClient().postForm("/admin/settings/theme/uninstall", form)
		expectUnauthorized(t, anonRes)
		if _, ok := env.server.themeCatalog().Get("acme"); !ok {
			t.Error("anonymous request deleted theme from catalog")
		}
		if _, err := os.Stat(filepath.Join(themesDir, "acme")); err != nil {
			t.Error("anonymous request deleted theme from disk")
		}
	})
}

// TestThemeFlashDoesNotReflectInput は ?theme_flash= の通知が、URL の文字列をそのまま画面に出さないことを確かめる。
func TestThemeFlashDoesNotReflectInput(t *testing.T) {
	env, _ := newCustomThemeEnv(t, nil)
	adm := env.loggedIn(admin)
	res := adm.get(themeSettingsPath + "?theme_flash=uninstalled&theme_id=" + url.QueryEscape("<b>悪意</b>"))
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "カスタムテーマを削除しました")
	if strings.Contains(res.Body, "悪意") {
		t.Error("theme_id was reflected into the page")
	}
	res = adm.get(themeSettingsPath + "?theme_flash=" + url.QueryEscape("任意の文"))
	if strings.Contains(res.Body, "任意の文") {
		t.Error("unknown theme_flash was reflected into the page")
	}
}

// テーマの切り替えと、適用中のテーマの更新は静的サイトへ自動で反映する。適用していないテーマの取り込みは反映しない。
func TestThemeChangesReflectToStaticSite(t *testing.T) {
	env, _ := newCustomThemeEnv(t, nil)
	adm := env.loggedIn(admin)
	const debounce = 20 * time.Millisecond
	out := configureFolderReflect(t, env, debounce)
	exportedCSS := func(t *testing.T) string {
		t.Helper()
		css, err := os.ReadFile(filepath.Join(out, "custom-themes", "paper", "theme.css"))
		if err != nil {
			t.Fatalf("exported theme.css: %v", err)
		}
		return string(css)
	}

	files := testThemeFiles("paper", "ペーパー")
	files["theme.css"] = "body { color: #111111; }\n"
	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", files)), "installed", "paper", "")
	expectNoReflect(t, env, "install of an inactive theme", debounce)

	expectRedirect(t, adm.postForm(themeSettingsPath, url.Values{"theme": {"paper"}}), themeSettingsPath)
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("reflect after theme switch = %+v", got)
	}
	if reflects, _ := reflectState(env.server); reflects != 1 {
		t.Fatalf("reflects after theme switch = %d, want 1", reflects)
	}
	expectContains(t, exportedCSS(t), "#111111")

	files["theme.css"] = "body { color: #abcdef; }\n"
	expectThemeFlash(t, adm, adm.postThemeZip("paper.zip", testThemeZip(t, "paper", files)), "installed", "paper", "")
	if got := waitReflectDone(t, env.server); !got.OK {
		t.Fatalf("reflect after updating the active theme = %+v", got)
	}
	if reflects, _ := reflectState(env.server); reflects != 2 {
		t.Fatalf("reflects after updating the active theme = %d, want 2", reflects)
	}
	expectContains(t, exportedCSS(t), "#abcdef")
}

// TestInstallThemeRejectsBypassCSS は WHATWG URL パーサの挙動を突いた CSS 外部リソース読み込み（M-1）が
// テーマ取り込み時に確実に拒絶されることを確かめる。
func TestInstallThemeRejectsBypassCSS(t *testing.T) {
	env, _ := newCustomThemeEnv(t, nil)
	adm := env.loggedIn(admin)

	for _, tc := range []struct {
		name string
		id   string
		css  string
	}{
		{"tab in scheme", "bypass-tab", "body { background: url(\"ht\ttps://evil.example/t.png\"); }\n"},
		{"escaped tab in scheme", "bypass-esctab", "body { background: url(\"ht\\9 tps://evil.example/t.png\"); }\n"},
		{"escaped backslashes", "bypass-bs", "body { background: url(\"\\\\\\\\evil.example/t.png\"); }\n"},
		{"slash and escaped backslash", "bypass-slashbs", "body { background: url(\"/\\\\evil.example/t.png\"); }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := testThemeFiles(tc.id, "すり抜けテスト")
			files["theme.css"] = tc.css
			res := adm.postThemeZip(tc.id+".zip", testThemeZip(t, tc.id, files))
			expectThemeFlash(t, adm, res, "invalid", "", "テーマの形式（theme.json・theme.css）が正しくありません")
			if _, ok := env.server.themeCatalog().Get(tc.id); ok {
				t.Fatalf("theme %s with bypass CSS was accepted into catalog", tc.id)
			}
		})
	}
}

// TestCustomThemeConcurrentOperations は複数のテーマ取り込み・削除・設定変更・閲覧が並行して走っても
// データ破壊やパニックが発生せず、カタログが常に健全な状態を保つことを確かめる。
func TestCustomThemeConcurrentOperations(t *testing.T) {
	env, themesDir := newCustomThemeEnv(t, func(dir string) {
		writeTestTheme(t, dir, "base-a", "ベースA")
		writeTestTheme(t, dir, "base-b", "ベースB")
	})
	adm := env.loggedIn(admin)

	const workers = 4
	const iterations = 10

	// 1. ゴルーチン実行前のメインゴルーチンで Zip データを事前生成
	precomputedZips := make([][]byte, workers)
	for i := 0; i < workers; i++ {
		id := fmt.Sprintf("conc-%d", i)
		precomputedZips[i] = testThemeZip(t, id, testThemeFiles(id, "並行テーマ"))
	}

	var wg sync.WaitGroup

	// 並行ワーカー 1: テーマ Zip 取り込み
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			idx := i % workers
			id := fmt.Sprintf("conc-%d", idx)
			res := adm.postThemeZip(id+".zip", precomputedZips[idx])
			if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusFound {
				t.Errorf("worker 1 (install-zip) status = %d, want 302/303", res.StatusCode)
			}
		}
	}()

	// 並行ワーカー 2: テーマ設定変更
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			target := "modern"
			if i%2 == 0 {
				target = "base-a"
			}
			res := adm.postForm(themeSettingsPath, url.Values{"theme": {target}})
			if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusFound {
				t.Errorf("worker 2 (setting) status = %d, want 302/303", res.StatusCode)
			}
		}
	}()

	// 並行ワーカー 3: カタログ読み込みと配信
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations*5; i++ {
			_ = env.server.themeCatalog().All()
			_ = env.server.themeCatalog().IDs()
			_ = env.server.themeCatalog().Resolve("base-a")
			res := env.newClient().get("/custom-themes/base-a/theme.css")
			if res.StatusCode != http.StatusOK {
				t.Errorf("worker 3 (asset get) status = %d, want 200", res.StatusCode)
			}
		}
	}()

	// 並行ワーカー 4: テーマ削除
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			id := fmt.Sprintf("conc-%d", i%workers)
			res := adm.postForm("/admin/settings/theme/uninstall", url.Values{"theme_id": {id}})
			if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusFound {
				t.Errorf("worker 4 (uninstall) status = %d, want 302/303", res.StatusCode)
			}
		}
	}()

	wg.Wait()

	// 完了後、Default テーマが必ずカタログに存在し健全であることを確認
	cat := env.server.themeCatalog()
	if _, ok := cat.Get(theme.Default); !ok {
		t.Fatalf("catalog corrupted: default theme %q missing", theme.Default)
	}

	// カタログ内のカスタムテーマ一覧とディスク上のカスタムディレクトリが完全に一致していること、
	// および残骸ディレクトリが残っていないことを厳密にアサート
	entries, err := os.ReadDir(themesDir)
	if err != nil {
		t.Fatalf("ReadDir(themesDir): %v", err)
	}

	diskCustom := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".install-") || strings.HasPrefix(name, ".uninstall-") {
			t.Errorf("stale staging directory remained on disk: %s", name)
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			diskCustom[name] = true
		}
	}

	catCustom := make(map[string]bool)
	for _, th := range cat.All() {
		if th.IsCustom {
			catCustom[th.ID] = true
		}
	}

	for id := range diskCustom {
		if !catCustom[id] {
			t.Errorf("theme directory %q exists on disk but is missing from catalog", id)
		}
	}
	for id := range catCustom {
		if !diskCustom[id] {
			t.Errorf("custom theme %q is in catalog but missing on disk", id)
		}
	}
}
