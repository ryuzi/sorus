package server

import (
	"bytes"
	"context"
	"html"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/theme"
	"sorus/static"
)

const testAnalyticsTag = `<script defer src="https://static.cloudflareinsights.com/beacon.min.js" data-cf-beacon='{"token":"test-token"}'></script>`

func generalSettingsForm() url.Values {
	return url.Values{
		"site_name":        {"設定テストメディア"},
		"site_description": {"サイト設定の結合テスト"},
		"base_url":         {"https://media.example.jp/"},
		"copyright":        {"(C) 2026 メディア運営委員会"},
		"analytics_tag":    {testAnalyticsTag},
	}
}

func (e *testEnv) setting(key string) string {
	e.t.Helper()
	var v string
	if err := e.db.Get(&v, `SELECT value FROM settings WHERE key = ?`, key); err != nil {
		e.t.Fatalf("select setting %q: %v", key, err)
	}
	return v
}

func TestGeneralSettingsSave(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	res := adm.get("/admin/settings/general")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "サイト基本設定", `name="site_name"`, `name="copyright"`, `name="analytics_tag"`)

	res = adm.postForm("/admin/settings/general", generalSettingsForm())
	loc := expectRedirect(t, res, "/admin/settings/general?flash=settings_saved")

	want := map[string]string{
		"site_name":        "設定テストメディア",
		"site_description": "サイト設定の結合テスト",
		"base_url":         "https://media.example.jp", // 末尾のスラッシュは取り除く
		"copyright":        "(C) 2026 メディア運営委員会",
		"analytics_tag":    testAnalyticsTag,
	}
	for k, v := range want {
		if got := env.setting(k); got != v {
			t.Errorf("setting %s = %q, want %q", k, got, v)
		}
	}

	// 保存後の画面は保存した値を表示し、通知はキー名ではなく文言で出る。
	res = adm.get(loc)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `value="設定テストメディア"`, `value="(C) 2026 メディア運営委員会"`,
		`value="https://media.example.jp"`, `data-flash="設定を保存しました"`)

	// サイト名が空なら既定のサイト名に戻す。
	form := generalSettingsForm()
	form.Set("site_name", "  ")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if got := env.setting("site_name"); got != DefaultSiteName {
		t.Errorf("site_name = %q, want %q", got, DefaultSiteName)
	}
}

func TestGeneralSettingsAppliedToPublicPages(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{
			Title: "設定反映の確認", Slug: "settings-check", ContentMarkdown: "本文",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
	})
	pub := env.newClient()

	// 未設定のときは既定のフッター表記で、解析タグは出ない。
	res := pub.get("/")
	expectStatus(t, res, http.StatusOK)
	if strings.Contains(res.Body, "static.cloudflareinsights.com") {
		t.Fatal("analytics tag rendered before it was configured")
	}

	expectRedirect(t, env.loggedIn(admin).postForm("/admin/settings/general", generalSettingsForm()),
		"/admin/settings/general?flash=settings_saved")

	for _, path := range []string{"/", "/articles/settings-check"} {
		res := pub.get(path)
		expectStatus(t, res, http.StatusOK)
		head, _, ok := strings.Cut(res.Body, "</head>")
		if !ok {
			t.Fatalf("%s: </head> not found", path)
		}
		// 解析タグはエスケープせずに <head> 内へそのまま出力する。
		expectContains(t, head, testAnalyticsTag)
		footer := res.Body[strings.Index(res.Body, `<footer class="site-footer">`):]
		expectContains(t, footer, "(C) 2026 メディア運営委員会")
		expectContains(t, res.Body, "設定テストメディア")
	}
}

func TestGeneralSettingsAppliedToExport(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{
			Title: "エクスポート設定反映", Slug: "export-settings", ContentMarkdown: "本文",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
	})
	expectRedirect(t, env.loggedIn(admin).postForm("/admin/settings/general", generalSettingsForm()),
		"/admin/settings/general?flash=settings_saved")

	outDir := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, rel := range []string{"index.html", filepath.Join("articles", "export-settings", "index.html")} {
		b, err := os.ReadFile(filepath.Join(outDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		html := string(b)
		head, _, _ := strings.Cut(html, "</head>")
		expectContains(t, head, testAnalyticsTag)
		expectContains(t, html, "(C) 2026 メディア運営委員会", "設定テストメディア")
	}
}

func TestGeneralSettingsForbiddenForNonAdmins(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{studentA, chief} {
		t.Run(string(u.Role), func(t *testing.T) {
			c := env.loggedIn(u)
			expectStatus(t, c.get("/admin/settings/general"), http.StatusForbidden)
			expectStatus(t, c.postForm("/admin/settings/general", generalSettingsForm()), http.StatusForbidden)
		})
	}
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN ('analytics_tag', 'copyright')`); n != 0 {
		t.Errorf("non-admin saved %d settings", n)
	}
	expectRedirect(t, env.newClient().get("/admin/settings/general"), "/admin/login")
}

// サイト基本設定は DB に保存され、再起動後のサーバーや別プロセスの sorus export にも引き継がれる。
func TestGeneralSettingsPersistAcrossRestart(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{
			Title: "再起動後の確認", Slug: "after-restart", ContentMarkdown: "本文",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
	})
	expectRedirect(t, env.loggedIn(admin).postForm("/admin/settings/general", generalSettingsForm()),
		"/admin/settings/general?flash=settings_saved")

	dir := t.TempDir()
	// 環境変数由来の値（Config）より、管理画面で保存した値を優先する。
	restarted, err := New(Config{
		DB: env.db, UploadDir: filepath.Join(dir, "uploads"), AttachmentDir: filepath.Join(dir, "attachments"),
		SiteName: "環境変数のサイト名", BaseURL: "http://env.example.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := restarted.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if restarted.cfg.SiteName != "設定テストメディア" || restarted.cfg.SiteDescription != "サイト設定の結合テスト" ||
		restarted.cfg.BaseURL != "https://media.example.jp" {
		t.Fatalf("restarted cfg = name %q, desc %q, base %q", restarted.cfg.SiteName, restarted.cfg.SiteDescription, restarted.cfg.BaseURL)
	}

	outDir := filepath.Join(dir, "dist")
	if _, err := restarted.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(outDir, "articles", "after-restart", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	expectContains(t, string(b), "設定テストメディア", `href="https://media.example.jp/articles/after-restart"`)
	if strings.Contains(string(b), "環境変数のサイト名") {
		t.Error("export uses the environment site name instead of the saved one")
	}
}

// testFaviconSVG はスクリプトを含まない SVG の Favicon。
const testFaviconSVG = `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" fill="#0f172a"/></svg>`

// testFaviconSVGGradient は文書内の参照（xlink:href="#..."・url(#...)）・コメントを含む、安全な SVG の Favicon。
// <style> 要素と style 属性は外部 URL を読み込みうるため受け付けない（下の拒否するケースを参照）。
const testFaviconSVGGradient = `<?xml version="1.0" encoding="UTF-8"?>
<!-- <script>alert(1)</script> はコメントなので実行されない -->
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 32 32">
<defs><linearGradient id="a"><stop offset="0" stop-color="#0f172a"/></linearGradient>
<linearGradient id="b" xlink:href="#a"/></defs>
<rect class="bg" width="32" height="32" fill="url(#b)" stroke="URL( '#a' )"/></svg>`

// testFaviconICO は ICO ヘッダー（予約 0、種別 1、画像数 1）で始まるデータ。
var testFaviconICO = append([]byte("\x00\x00\x01\x00\x01\x00\x10\x10"), make([]byte, 32)...)

// testFaviconWebP は RIFF/WEBP ヘッダーで始まるデータ。
var testFaviconWebP = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)

var faviconURLPattern = regexp.MustCompile(`^/uploads/favicon-[0-9a-f]{16}\.(svg|png|ico|webp)$`)

// postSettingsWithFavicon は管理画面のフォームと同じく multipart/form-data でサイト基本設定を送る。
// filename が空ならファイルを添付しない。contentType はファイルパートに申告する Content-Type。
func (c *client) postSettingsWithFavicon(form url.Values, filename, contentType string, data []byte) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if !form.Has(CSRFFormField) {
		mw.WriteField(CSRFFormField, c.csrfToken())
	}
	for k, vs := range form {
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	if filename != "" {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="favicon"; filename="`+filename+`"`)
		h.Set("Content-Type", contentType)
		fw, err := mw.CreatePart(h)
		if err != nil {
			c.t.Fatal(err)
		}
		fw.Write(data)
	}
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+"/admin/settings/general", &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "text/html")
	return c.do(req)
}

// faviconFile は favicon_url が指すアップロード先のファイルパスを返す。
func (e *testEnv) faviconFile(u string) string {
	return filepath.Join(e.uploadDir, strings.TrimPrefix(u, UploadURLPrefix))
}

// expectFaviconLinks は HTML の <head> が Favicon として href と type を指していることを確かめる。
func expectFaviconLinks(t *testing.T, body, href, mimeType string) {
	t.Helper()
	head, _, ok := strings.Cut(body, "</head>")
	if !ok {
		t.Fatal("</head> not found")
	}
	// html/template は属性値の + を &#43; と書くため、文字参照を戻してから比べる。
	head = html.UnescapeString(head)
	expectContains(t, head, `<link rel="icon" type="`+mimeType+`" href="`+href+`">`,
		`<link rel="apple-touch-icon" href="`+href+`">`)
}

func TestGeneralSettingsFaviconUploadPreviewAndReset(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{
			Title: "Favicon の確認", Slug: "favicon-check", ContentMarkdown: "本文",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		})
	})
	adm := env.loggedIn(admin)
	pub := env.newClient()

	// 未設定のときは既定の Favicon を出し、削除ボタンは出さない。
	res := adm.get("/admin/settings/general")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `enctype="multipart/form-data"`, `name="favicon"`, `type="file"`,
		`id="favicon-preview" src="/static/icon.svg"`, "既定の Favicon を使用中")
	if strings.Contains(res.Body, `name="favicon_reset"`) {
		t.Error("reset button rendered before a custom favicon is set")
	}
	expectFaviconLinks(t, res.Body, static.URL("icon.svg"), "image/svg+xml")
	expectFaviconLinks(t, pub.get("/").Body, static.URL("icon.svg"), "image/svg+xml")

	// PNG をアップロードすると UploadDir に保存し、URL を settings に記録する。
	pngData := pngBytes(t, 64, 64)
	expectRedirect(t, adm.postSettingsWithFavicon(generalSettingsForm(), "icon.png", "image/png", pngData),
		"/admin/settings/general?flash=settings_saved")
	pngURL := env.setting("favicon_url")
	if !faviconURLPattern.MatchString(pngURL) || !strings.HasSuffix(pngURL, ".png") {
		t.Fatalf("favicon_url = %q", pngURL)
	}
	if got, err := os.ReadFile(env.faviconFile(pngURL)); err != nil || !bytes.Equal(got, pngData) {
		t.Fatalf("saved favicon: %v (equal %v)", err, bytes.Equal(got, pngData))
	}
	// フォームの他の項目も同時に保存される。
	if got := env.setting("site_name"); got != "設定テストメディア" {
		t.Errorf("site_name = %q", got)
	}
	// アップロードした Favicon は /uploads/ から配信される。
	res = pub.get(pngURL)
	expectStatus(t, res, http.StatusOK)
	if res.Body != string(pngData) {
		t.Error("served favicon differs from the uploaded file")
	}

	// 設定画面はプレビューと削除ボタンを表示し、管理画面・公開ページの <head> も新しい Favicon を指す。
	res = adm.get("/admin/settings/general")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="favicon-preview" src="`+pngURL+`"`, "カスタム Favicon を使用中",
		`name="favicon_reset" value="1"`, "デフォルトに戻す（削除）")
	expectFaviconLinks(t, res.Body, pngURL, "image/png")
	for _, path := range []string{"/", "/articles/favicon-check", "/no-such-page"} {
		expectFaviconLinks(t, pub.get(path).Body, pngURL, "image/png")
	}

	// ファイルを添付しない保存（従来の urlencoded の送信を含む）では Favicon を変えない。
	expectRedirect(t, adm.postSettingsWithFavicon(generalSettingsForm(), "", "", nil), "/admin/settings/general?flash=settings_saved")
	expectRedirect(t, adm.postForm("/admin/settings/general", generalSettingsForm()), "/admin/settings/general?flash=settings_saved")
	if got := env.setting("favicon_url"); got != pngURL {
		t.Fatalf("favicon_url changed to %q by a save without a file", got)
	}

	// SVG に差し替えると古いファイルを削除する。
	expectRedirect(t, adm.postSettingsWithFavicon(generalSettingsForm(), "icon.svg", "image/svg+xml", []byte(testFaviconSVG)),
		"/admin/settings/general?flash=settings_saved")
	svgURL := env.setting("favicon_url")
	if !faviconURLPattern.MatchString(svgURL) || !strings.HasSuffix(svgURL, ".svg") {
		t.Fatalf("favicon_url = %q", svgURL)
	}
	if _, err := os.Stat(env.faviconFile(pngURL)); !os.IsNotExist(err) {
		t.Errorf("old favicon still exists: %v", err)
	}
	expectFaviconLinks(t, pub.get("/").Body, svgURL, "image/svg+xml")

	// 「デフォルトに戻す」で設定とファイルを削除し、既定の Favicon に戻る（同時に選んだファイルは無視する）。
	form := generalSettingsForm()
	form.Set("favicon_reset", "1")
	expectRedirect(t, adm.postSettingsWithFavicon(form, "icon.png", "image/png", pngData), "/admin/settings/general?flash=settings_saved")
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key = 'favicon_url'`); n != 0 {
		t.Errorf("favicon_url remains after reset (%d rows)", n)
	}
	if _, err := os.Stat(env.faviconFile(svgURL)); !os.IsNotExist(err) {
		t.Errorf("favicon file still exists after reset: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(env.uploadDir, "favicon-*")); len(entries) != 0 {
		t.Errorf("favicon files left in upload dir: %v", entries)
	}
	res = adm.get("/admin/settings/general")
	expectContains(t, res.Body, `id="favicon-preview" src="/static/icon.svg"`, "既定の Favicon を使用中")
	expectFaviconLinks(t, pub.get("/").Body, static.URL("icon.svg"), "image/svg+xml")
}

func TestGeneralSettingsFaviconAcceptedTypes(t *testing.T) {
	cases := []struct {
		name, filename, contentType, ext, mimeType string
		data                                       []byte
	}{
		{"svg", "icon.svg", "image/svg+xml", ".svg", "image/svg+xml", []byte(testFaviconSVG)},
		{"svg with gradient reference", "icon.svg", "image/svg+xml", ".svg", "image/svg+xml", []byte(testFaviconSVGGradient)},
		{"ico", "favicon.ico", "image/x-icon", ".ico", "image/x-icon", testFaviconICO},
		{"ico vnd", "favicon.ico", "image/vnd.microsoft.icon", ".ico", "image/x-icon", testFaviconICO},
		{"webp", "icon.webp", "image/webp", ".webp", "image/webp", testFaviconWebP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			adm := env.loggedIn(admin)
			expectRedirect(t, adm.postSettingsWithFavicon(generalSettingsForm(), tc.filename, tc.contentType, tc.data),
				"/admin/settings/general?flash=settings_saved")
			u := env.setting("favicon_url")
			if !faviconURLPattern.MatchString(u) || !strings.HasSuffix(u, tc.ext) {
				t.Fatalf("favicon_url = %q, want *%s", u, tc.ext)
			}
			if _, err := os.Stat(env.faviconFile(u)); err != nil {
				t.Fatalf("saved favicon: %v", err)
			}
			expectFaviconLinks(t, adm.get("/admin/settings/general").Body, u, tc.mimeType)
		})
	}
}

// unsafeSVGMsg は安全でない SVG を拒否したときのエラーメッセージの一部。
const unsafeSVGMsg = "安全でない内容を含む SVG"

func TestGeneralSettingsFaviconValidation(t *testing.T) {
	jpeg := append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), make([]byte, 64)...)
	tooLarge := append(pngBytes(t, 8, 8), make([]byte, MaxFaviconSize)...)
	cases := []struct {
		name, filename, contentType, wantMsg string
		data                                 []byte
	}{
		{"text declared as png", "icon.png", "image/png", "SVG・PNG・ICO・WebP 形式", []byte("this is not an image")},
		{"jpeg", "photo.jpg", "image/jpeg", "SVG・PNG・ICO・WebP 形式", jpeg},
		{"html declared as svg", "icon.svg", "image/svg+xml", "SVG・PNG・ICO・WebP 形式", []byte("<!doctype html><p>hello</p>")},
		{"empty", "icon.png", "image/png", "SVG・PNG・ICO・WebP 形式", nil},
		{"too large", "big.png", "image/png", "上限（2MB）を超えています", tooLarge},
		{"svg with script", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
		{"svg with event handler", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`)},
		{"svg with uppercase event handler", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect ONCLICK="alert(1)"/></svg>`)},
		{"svg with namespaced script", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:h="http://www.w3.org/1999/xhtml"><h:script>alert(1)</h:script></svg>`)},
		{"svg with entity-encoded javascript href", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">` +
				`<a xlink:href="&#106;&#x61;vascript:alert(1)"><rect width="1" height="1"/></a></svg>`)},
		{"svg with entity-encoded javascript in attribute", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url(&#x6A;ava&#9;script:alert(1))"/></svg>`)},
		{"svg with javascript href split by whitespace", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><linearGradient id="g" href=" java&#10;script:alert(1)"/></svg>`)},
		{"svg with data href", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><linearGradient id="g" href="data:image/svg+xml,&lt;svg onload=alert(1)&gt;"/></svg>`)},
		{"svg with external href", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><filter id="f"><feImage href="https://evil.example/x.svg"/></filter></svg>`)},
		{"svg with dtd entity", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY js "javascript:alert(1)">]>` +
				`<svg xmlns="http://www.w3.org/2000/svg"><a href="&js;"><rect width="1" height="1"/></a></svg>`)},
		{"svg with external dtd", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd">` +
				`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"svg with undeclared entity", "icon.svg", "image/svg+xml", "SVG の内容を読み取れませんでした",
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="&js;"/></svg>`)},
		{"svg with animate", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><a><animate attributeName="href" values="javascript:alert(1)"/>` +
				`<rect width="1" height="1"/></a></svg>`)},
		{"svg with animate only", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"><animate attributeName="x" from="0" to="1" dur="1s"/></rect></svg>`)},
		{"svg with set", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"><set attributeName="fill" to="red"/></rect></svg>`)},
		{"svg with use", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><defs><rect id="r" width="1" height="1"/></defs><use href="#r"/></svg>`)},
		{"svg with image", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="#x" width="1" height="1"/></svg>`)},
		{"svg with foreignObject", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject width="1" height="1"><div xmlns="http://www.w3.org/1999/xhtml">x</div></foreignObject></svg>`)},
		{"svg with stylesheet instruction", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<?xml-stylesheet href="https://evil.example/x.css"?><svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"svg with javascript in style text", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><style>rect{fill:url(javascript:alert(1))}</style></svg>`)},
		// <style> 要素・style 属性は、CSS の url()・@import・エスケープで外部 URL を読み込みうるため一律に拒否する。
		{"svg with harmless style element", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><style>.bg{opacity:.9}</style><rect class="bg"/></svg>`)},
		{"svg with style import", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><style>@import "https://evil.example/x.css";</style></svg>`)},
		{"svg with css-escaped url in style", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><style>rect{fill:\75 rl(https://evil.example/t)}</style></svg>`)},
		{"svg with namespaced style element", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:h="http://www.w3.org/1999/xhtml"><h:style>a{}</h:style></svg>`)},
		{"svg with harmless style attribute", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:red" width="1" height="1"/></svg>`)},
		{"svg with uppercase style attribute", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect STYLE="background:url(https://evil.example/t)"/></svg>`)},
		{"svg with external url in presentation attribute", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect fill="url(https://evil.example/p.svg#g)" width="1" height="1"/></svg>`)},
		{"svg with quoted external url in filter", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect filter="UrL( 'https://evil.example/f.svg#f' )"/></svg>`)},
		{"svg with css escape in presentation attribute", "icon.svg", "image/svg+xml", unsafeSVGMsg,
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect fill="\75 rl(https://evil.example/p)"/></svg>`)},
		{"malformed svg", "icon.svg", "image/svg+xml", "SVG の内容を読み取れませんでした",
			[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect></svg>`)},
		{"svg not at root", "icon.svg", "image/svg+xml", "SVG・PNG・ICO・WebP 形式",
			[]byte(`<html><body><svg xmlns="http://www.w3.org/2000/svg"/></body></html>`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			adm := env.loggedIn(admin)
			res := adm.postSettingsWithFavicon(generalSettingsForm(), tc.filename, tc.contentType, tc.data)
			expectStatus(t, res, http.StatusUnprocessableEntity)
			// エラーを通知し、入力した値を保ったままフォームを表示し直す。
			expectContains(t, res.Body, tc.wantMsg, `data-flash-error="true"`, `value="設定テストメディア"`,
				`id="favicon-preview" src="/static/icon.svg"`)
			// Favicon も他の項目も保存しない。
			if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN ('favicon_url', 'site_name', 'copyright')`); n != 0 {
				t.Errorf("saved %d settings despite the invalid favicon", n)
			}
			if entries, _ := filepath.Glob(filepath.Join(env.uploadDir, "favicon-*")); len(entries) != 0 {
				t.Errorf("favicon files written: %v", entries)
			}
		})
	}
}

func TestGeneralSettingsFaviconForbiddenForNonAdmins(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{studentA, chief} {
		res := env.loggedIn(u).postSettingsWithFavicon(generalSettingsForm(), "icon.svg", "image/svg+xml", []byte(testFaviconSVG))
		expectStatus(t, res, http.StatusForbidden)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key = 'favicon_url'`); n != 0 {
		t.Errorf("non-admin saved favicon_url")
	}
	if entries, _ := filepath.Glob(filepath.Join(env.uploadDir, "favicon-*")); len(entries) != 0 {
		t.Errorf("favicon files written by non-admins: %v", entries)
	}
}

// TestThemeSettingsCustomThemeUI は独自テーマのバッジ・取り込み元リンク・削除ボタンと、取り込みフォームの描画を確かめる。
func TestThemeSettingsCustomThemeUI(t *testing.T) {
	env, _ := newCustomThemeEnv(t, func(dir string) {
		writeTestTheme(t, dir, "acme", "アクメ")
		writeTestTheme(t, dir, "beta", "ベータ")
		writeTestTheme(t, dir, "evil", "イービル")
		manifest := func(id, src string) {
			body := `{"name":{"ja":"` + id + `"},"description":{"ja":"x"},"color_scheme":"light","order":90,"source_url":"` + src + `"}`
			if err := os.WriteFile(filepath.Join(dir, id, "theme.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		manifest("beta", "https://github.com/owner/beta")
		manifest("evil", "javascript:alert(1)")
	})
	env.setTheme("acme")
	adm := env.loggedIn(admin)
	res := adm.get("/admin/settings/theme")
	expectStatus(t, res, http.StatusOK)
	body := res.Body

	// バッジは独自テーマにだけ出す。
	expectContains(t, body, `id="theme-custom-acme"`, `id="theme-custom-beta"`, "独自テーマ")
	if strings.Contains(body, `id="theme-custom-tech"`) {
		t.Error("built-in theme has the custom badge")
	}
	// 取り込み元は安全なリンクとして出し、危険なスキームは無効化する。
	expectContains(t, body, `href="https://github.com/owner/beta" target="_blank" rel="noopener noreferrer nofollow"`)
	if strings.Contains(body, `href="javascript:`) {
		t.Error("javascript: source_url is rendered as a live link")
	}
	// 適用中の独自テーマは削除できず、フォームも出さない。未適用のものは外側のフォームへ form 属性で送る。
	expectContains(t, body,
		`id="theme-delete-acme" disabled title="適用中のため削除できません"`,
		`id="theme-delete-beta" form="delete-theme-form-beta"`,
		`<form id="delete-theme-form-beta" method="post" action="/admin/settings/theme/uninstall" hidden`,
		`data-confirm="独自テーマ「beta」を削除しますか？"`,
		`name="theme_id" value="beta"`)
	if strings.Contains(body, `id="delete-theme-form-acme"`) || strings.Contains(body, `id="theme-delete-tech"`) {
		t.Error("delete form/button offered for the applied or a built-in theme")
	}
	// form の入れ子が無いこと（開始タグと終了タグが交互に並ぶ）。
	depth := 0
	for _, m := range regexp.MustCompile(`<form\b|</form>`).FindAllString(body, -1) {
		if m == "</form>" {
			depth--
		} else {
			depth++
		}
		if depth < 0 || depth > 1 {
			t.Fatalf("nested or unbalanced <form> (depth %d)", depth)
		}
	}
	// 取り込みフォーム。
	expectContains(t, body,
		`action="/admin/settings/theme/install-url"`, `type="url" id="theme-url" name="theme_url" required placeholder="https://github.com/owner/theme-name"`,
		`action="/admin/settings/theme/install-zip" enctype="multipart/form-data"`, `type="file" id="theme-file" name="theme_file" accept=".zip" required`,
		"独自テーマの追加", "URL から取り込む", "Zip から取り込む")

	adm.useEnglish()
	en := adm.get("/admin/settings/theme").Body
	expectContains(t, en, ">Custom<", `title="Cannot delete active theme"`,
		html.EscapeString(`Delete custom theme "beta"?`), "Add custom theme", "Import from URL", "Import from Zip")
}

func TestGeneralSettingsBaseURLValidation(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	invalidURLs := []string{
		"javascript:alert(1)",
		"ftp://example.jp",
		"https://example.jp?query=1",
		"https://example.jp#hash",
		"https://user:pass@example.jp",
		"not-a-url",
	}
	for _, bad := range invalidURLs {
		t.Run("invalid_"+bad, func(t *testing.T) {
			form := generalSettingsForm()
			form.Set("base_url", bad)
			res := adm.postForm("/admin/settings/general", form)
			expectStatus(t, res, http.StatusUnprocessableEntity)
			expectContains(t, res.Body, "「公開サイトのURL」は https://example.jp の形で入力してください")
		})
	}

	t.Run("trailing_slash_normalization", func(t *testing.T) {
		form := generalSettingsForm()
		form.Set("base_url", "https://valid.example.jp/")
		res := adm.postForm("/admin/settings/general", form)
		expectRedirect(t, res, "/admin/settings/general?flash=settings_saved")
		if got := env.setting("base_url"); got != "https://valid.example.jp" {
			t.Errorf("db base_url = %q, want %q", got, "https://valid.example.jp")
		}
		if got := env.server.getBaseURL(); got != "https://valid.example.jp" {
			t.Errorf("memory getBaseURL() = %q, want %q", got, "https://valid.example.jp")
		}
	})

	t.Run("clear_base_url", func(t *testing.T) {
		form := generalSettingsForm()
		form.Set("base_url", "")
		res := adm.postForm("/admin/settings/general", form)
		expectRedirect(t, res, "/admin/settings/general?flash=settings_saved")
		if got := env.setting("base_url"); got != "" {
			t.Errorf("db base_url = %q, want empty", got)
		}
		if got := env.server.getBaseURL(); got != "" {
			t.Errorf("memory getBaseURL() = %q, want empty", got)
		}
	})
}

// バックアップの復元や外部テーマの ID の違いで、保存済みの独自テーマがカタログから外れたときも、テーマ設定に
// 既定のテーマで表示していることを警告する（Resolve の黙ったフォールバックに気づけるように）。
func TestThemeSettingsWarnsWhenCustomThemeIsUnavailable(t *testing.T) {
	env, themesDir := newCustomThemeEnv(t, func(dir string) {
		writeTestTheme(t, dir, "acme", "アクメ")
		// theme.css が無い独自テーマは検証を通らず、カタログから外れる（Skipped）。
		writeTestTheme(t, dir, "broken", "壊れたテーマ")
		if err := os.Remove(filepath.Join(dir, "broken", "theme.css")); err != nil {
			t.Fatal(err)
		}
	})
	if _, skipped := env.server.themeCatalog().Skipped()["broken"]; !skipped {
		t.Fatalf("broken theme is not skipped: %v", env.server.themeCatalog().Skipped())
	}
	def := env.server.themeCatalog().Resolve(theme.Default)
	adm := env.loggedIn(admin)

	// 取り込み済みの独自テーマを適用しているあいだは警告しない。
	env.setTheme("acme")
	res := adm.get(themeSettingsPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `value="acme" required checked`, `id="theme-applied-acme"`)
	expectNotContains(t, res.Body, `id="theme-missing-warning"`)

	for _, tc := range []struct{ name, stored string }{
		{"files are invalid", "broken"},
		{"not installed on this PC", "lab-site-theme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env.setTheme(tc.stored)
			adm := env.loggedIn(admin)
			res := adm.get(themeSettingsPath)
			expectStatus(t, res, http.StatusOK)
			expectContains(t, res.Body, `id="theme-missing-warning"`, `role="alert"`,
				"保存済みのテーマ「"+tc.stored+"」が見つからないため、"+def.Label("ja")+" で表示しています",
				`value="`+def.ID+`" required checked`)
			expectNotContains(t, res.Body, `class="theme-applied-badge`, `value="`+tc.stored+`"`)
			adm.useEnglish()
			expectContains(t, adm.get(themeSettingsPath).Body, `id="theme-missing-warning"`,
				html.EscapeString(`The saved theme "`+tc.stored+`" was not found, so the site is shown with `+def.Label("en")))
		})
	}

	// 別のテーマを適用し直すと、警告は消える。
	env.setTheme("broken")
	expectRedirect(t, adm.postForm(themeSettingsPath, url.Values{"theme": {"acme"}}), themeSettingsPath)
	res = adm.get(themeSettingsPath)
	expectNotContains(t, res.Body, `id="theme-missing-warning"`)
	expectContains(t, res.Body, `id="theme-applied-acme"`)
	if _, err := os.Stat(filepath.Join(themesDir, "broken")); err != nil {
		t.Errorf("the broken theme directory should be left as is: %v", err)
	}
}

// 保存値は利用者が直接 DB に入れた値でもありうるため、警告の中ではエスケープして出す。前後の空白は同じテーマとして扱い、警告しない。
func TestThemeSettingsMissingWarningEdgeCases(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	adm := env.loggedIn(admin)

	env.setTheme(`<img src=x onerror=alert(1)>`)
	res := adm.get(themeSettingsPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="theme-missing-warning"`, "「&lt;img src=x onerror=alert(1)&gt;」")
	expectNotContains(t, res.Body, `<img src=x onerror=alert(1)>`)

	env.setTheme("  tech\n")
	res = adm.get(themeSettingsPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `value="tech" required checked`, `id="theme-applied-tech"`)
	expectNotContains(t, res.Body, `id="theme-missing-warning"`)
}

// 基本設定で公開サイトの多言語対応（multilingual_enabled）だけを切り替えて保存しても、公開サーバーへ未反映（general）
// として記録し、「公開サーバーへ反映する」で公開サーバーの multilingual_enabled に反映する。OFF に戻したときも同じ。
func TestGeneralSettingsMultilingualToggleMarksRemoteDirtyAndPushes(t *testing.T) {
	env := newRemoteEnv(t)
	local := env.local
	adm := local.loggedIn(admin)
	expectRedirect(t, adm.postForm("/admin/settings/general", generalSettingsForm()), "/admin/settings/general?flash=settings_saved")
	env.configure("root", auth.RoleAdmin)
	if got := local.remoteDirtyList(); got != "" {
		t.Fatalf("dirty before toggling = %q", got)
	}

	for _, on := range []bool{true, false} {
		form := generalSettingsForm()
		want := "0"
		if on {
			form.Set(settingMultilingualEnabled, "1")
			want = "1"
		}
		expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
		if got := local.remoteDirtyList(); got != "general" {
			t.Fatalf("multilingual=%v: dirty = %q, want general", on, got)
		}
		expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, nil), http.StatusOK)
		if got := local.remoteDirtyList(); got != "" {
			t.Fatalf("multilingual=%v: dirty after push = %q", on, got)
		}
		if got := env.remote.setting(settingMultilingualEnabled); got != want {
			t.Errorf("multilingual=%v: remote multilingual_enabled = %q, want %q", on, got, want)
		}
	}
}
