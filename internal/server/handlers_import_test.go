package server

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// importTestWXR は取り込みの画面のテスト用の WXR。公開記事（カテゴリ「サークル」）・下書き（著者 newbie）・
// 固定ページ・ゴミ箱の記事を1件ずつ含む。画像は含めない（外部への通信をしない）。
const importTestWXR = `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0"
	xmlns:excerpt="http://wordpress.org/export/1.2/excerpt/"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:wp="http://wordpress.org/export/1.2/">
<channel>
	<title>旧ブログ</title>
	<link>https://old.example.com</link>
	<wp:wxr_version>1.2</wp:wxr_version>
	<wp:base_site_url>https://old.example.com</wp:base_site_url>
	<wp:author>
		<wp:author_login><![CDATA[hanako]]></wp:author_login>
		<wp:author_display_name><![CDATA[山田花子]]></wp:author_display_name>
	</wp:author>
	<wp:category>
		<wp:category_nicename><![CDATA[circle]]></wp:category_nicename>
		<wp:cat_name><![CDATA[サークル]]></wp:cat_name>
	</wp:category>
	<item>
		<title><![CDATA[軽音部ライブレポート]]></title>
		<link>https://old.example.com/2023/04/keion-live/</link>
		<dc:creator><![CDATA[hanako]]></dc:creator>
		<content:encoded><![CDATA[<p>春の<strong>定期ライブ</strong>が開催されました。</p>]]></content:encoded>
		<excerpt:encoded><![CDATA[]]></excerpt:encoded>
		<wp:post_id>101</wp:post_id>
		<wp:post_date_gmt><![CDATA[2023-04-10 10:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[keion-live]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
		<category domain="category" nicename="circle"><![CDATA[サークル]]></category>
	</item>
	<item>
		<title><![CDATA[下書きの企画書]]></title>
		<link>https://old.example.com/?p=102</link>
		<dc:creator><![CDATA[newbie]]></dc:creator>
		<content:encoded><![CDATA[企画の下書きです。]]></content:encoded>
		<wp:post_id>102</wp:post_id>
		<wp:post_date_gmt><![CDATA[2023-05-01 00:00:00]]></wp:post_date_gmt>
		<wp:status><![CDATA[draft]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[サークル紹介]]></title>
		<link>https://old.example.com/about-circle/</link>
		<dc:creator><![CDATA[hanako]]></dc:creator>
		<content:encoded><![CDATA[<p>固定ページです。</p>]]></content:encoded>
		<wp:post_id>103</wp:post_id>
		<wp:post_date_gmt><![CDATA[2022-01-01 00:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[about-circle]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[page]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[消した記事]]></title>
		<link>https://old.example.com/?p=104</link>
		<dc:creator><![CDATA[hanako]]></dc:creator>
		<content:encoded><![CDATA[<p>ゴミ箱</p>]]></content:encoded>
		<wp:post_id>104</wp:post_id>
		<wp:status><![CDATA[trash]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
	</item>
</channel>
</rss>`

// postImport は取り込みのフォームを multipart/form-data で送る（CSRF トークンは _csrf フィールドで付ける）。
// filename が空ならファイルを付けない。
func (c *client) postImport(path string, fields map[string]string, filename, data string) *response {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField(CSRFFormField, c.csrfToken()); err != nil {
		c.t.Fatal(err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			c.t.Fatal(err)
		}
	}
	if filename != "" {
		fw, err := mw.CreateFormFile(importFileField, filename)
		if err != nil {
			c.t.Fatal(err)
		}
		fw.Write([]byte(data))
	}
	if err := mw.Close(); err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "text/html")
	return c.do(req)
}

// importDefaults は画面の既定のオプション（画像を取り込む・固定ページも取り込む・自分の記事にまとめる）。
var importDefaults = map[string]string{"download_images": "1", "account_mode": importAccountCurrent}

// importCounts は取り込みで増える表の行数。
type importCounts struct{ articles, categories, users, redirects, media int }

func (e *testEnv) importCounts() importCounts {
	e.t.Helper()
	return importCounts{
		articles:   e.countRows(`SELECT COUNT(*) FROM articles`),
		categories: e.countRows(`SELECT COUNT(*) FROM categories`),
		users:      e.countRows(`SELECT COUNT(*) FROM users`),
		redirects:  e.countRows(`SELECT COUNT(*) FROM redirects`),
		media:      e.countRows(`SELECT COUNT(*) FROM media`),
	}
}

var importTokenPattern = regexp.MustCompile(`name="token" value="([0-9a-f]+)"`)

func TestImportRequiresAdmin(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{studentA, chief} {
		c := env.loggedIn(u)
		expectStatus(t, c.get(importPagePath), http.StatusForbidden)
		for _, path := range []string{importPreviewPath, importRunPath} {
			res := c.postImport(path, importDefaults, "wordpress.xml", importTestWXR)
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s POST %s: status %d, want 403", u.Username, path, res.StatusCode)
			}
		}
		// 管理者でない人には、記事一覧に取り込みへの導線を出さない。
		if body := c.get("/admin/articles").Body; strings.Contains(body, `id="import-wordpress-link"`) {
			t.Errorf("%s: article list shows the import link", u.Username)
		}
	}
	// ログインしていない要求は、本文を読む前に断る。
	anon := env.newClient()
	for _, path := range []string{importPreviewPath, importRunPath} {
		res := anon.postImport(path, importDefaults, "wordpress.xml", importTestWXR)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("anonymous POST %s: status %d, want 403", path, res.StatusCode)
		}
	}
	if got := env.importCounts(); got.articles != 0 {
		t.Errorf("articles = %d after rejected imports, want 0", got.articles)
	}
}

func TestImportPageShowsFormAndBackupNotice(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	res := c.get(importPagePath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"他のサービスから取り込む",
		`action="/admin/import/preview"`, `enctype="multipart/form-data"`, `name="wxr"`, `accept=".xml`,
		`name="download_images" value="1" class="mt-0.5" checked`,
		`name="skip_pages" value="1" class="mt-0.5">`,
		`name="account_mode" value="current" class="mt-0.5" checked`, `name="account_mode" value="authors"`,
		`name="dry_run" value="1" class="mt-0.5" checked`,
		"取り込む前に、完全バックアップを取ってください", `action="/admin/settings/backup/download"`,
		// 上部メニューでは「設定」の画面として扱う。
		`id="nav-settings" class="rounded-md px-2 py-1.5 hover:bg-neutral-100" aria-current="page"`)
	// 管理者には記事がまだ無いときの空状態にだけ、取り込みへの導線を出す。
	body := c.get("/admin/articles").Body
	expectContains(t, body, `id="import-wordpress-link"`, `href="/admin/import"`)
	if table := strings.Index(body, "<table"); table >= 0 && strings.Contains(body[:table], `id="import-wordpress-link"`) {
		t.Error("article list header shows the import link")
	}
}

func TestImportBackupNoticeWarnsAboutSecretsAndSharingAlternatives(t *testing.T) {
	t.Setenv("THEMES_DIR", t.TempDir())
	env := newTestEnv(t)
	ja := env.loggedIn(admin).get(importPagePath)
	expectContains(t, ja.Body,
		"パスワードや API キーなどの秘密情報が含まれます。人に渡さないでください。",
		"サイトを渡すときは「今のサイトを Zip ファイルでダウンロード」を使ってください。")
	expectNotContainsAny(t, "import page (ja)", ja.Body, "原稿ファイル", "（.sorus）")
	c := env.loggedIn(admin)
	c.get("/admin/set-lang?lang=en&return=/admin")
	en := c.get(importPagePath)
	expectContains(t, en.Body,
		"This backup contains passwords and API keys. Do not share it.",
		"To share the site, use “Download the site as a Zip file.”")
	expectNotContainsAny(t, "import page (en)", en.Body, "manuscript file", "(.sorus)")
}

func TestDesktopImportPageOffersAuthorChoices(t *testing.T) {
	env := newTestEnvWith(t, Config{Desktop: true}, nil)
	c := env.loggedIn(admin)
	res := c.get(importPagePath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`name="account_mode" value="current" class="mt-0.5" checked`,
		`name="account_mode" value="authors"`,
		"WordPress の著者ごとにユーザーを作る")
	expectNotContains(t, res.Body, `id="import-account-desktop"`)
}

func TestImportRejectsInvalidFiles(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	before := env.importCounts()
	for _, tc := range []struct {
		name, path, filename, data, want string
	}{
		{"no file", importPreviewPath, "", "", "WordPress で書き出したファイル（.xml）を選んでください"},
		{"empty file", importPreviewPath, "empty.xml", "", "WordPress で書き出したファイル（.xml）を選んでください"},
		{"broken xml", importPreviewPath, "broken.xml", `<?xml version="1.0"?><rss><channel><item><title>壊れた`, "XML として読み取れませんでした"},
		{"not xml", importPreviewPath, "photo.xml", "\x89PNG\r\n\x1a\n\x00\x00binary", "XML として読み取れませんでした"},
		{"not wxr", importPreviewPath, "other.xml", `<?xml version="1.0"?><html><body>hello</body></html>`, "WordPress の書き出しファイル（WXR）ではありません"},
		{"no posts", importPreviewPath, "feed.xml", `<?xml version="1.0"?><rss version="2.0"><channel><title>feed</title><item><title>RSS</title></item></channel></rss>`, "取り込める記事や固定ページが見つかりませんでした"},
		{"run not wxr", importRunPath, "other.xml", `<?xml version="1.0"?><html/>`, "WordPress の書き出しファイル（WXR）ではありません"},
		{"run broken xml", importRunPath, "broken.xml", `<rss><channel>`, "XML として読み取れませんでした"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := c.postImport(tc.path, importDefaults, tc.filename, tc.data)
			expectStatus(t, res, http.StatusBadRequest)
			// 入力画面に戻し、オプションを選び直せるようにする。
			expectContains(t, res.Body, tc.want, `id="import-error"`, `id="import-form"`)
		})
	}
	if after := env.importCounts(); after != before {
		t.Errorf("counts changed by rejected imports: before %+v, after %+v", before, after)
	}
}

func TestImportPreviewDoesNotChangeDatabase(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	before := env.importCounts()

	res := c.postImport(importPreviewPath, map[string]string{"download_images": "1", "account_mode": importAccountAuthors, "dry_run": "1"}, "wordpress.xml", importTestWXR)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "取り込む内容の確認", "まだ何も取り込んでいません", "wordpress.xml", "https://old.example.com",
		`id="import-run-form"`, `action="/admin/import/run"`, "この内容で取り込む", "取り込むときにダウンロードします",
		"WordPress の著者ごとにユーザーを作る")
	// 記事 3 件（公開 2・下書き 1。固定ページ 1 を含む）、スキップ 1、新しいカテゴリ 1、新しいユーザー 2（hanako・newbie）。
	for id, want := range map[string]string{
		"import-stat-articles": "3", "import-stat-pages": "1", "import-stat-skipped": "1",
		"import-stat-categories": "1", "import-stat-users": "2",
	} {
		expectContains(t, res.Body, `id="`+id+`" class="mt-1 text-xl font-bold tabular-nums text-neutral-900">`+want+`<`)
	}
	expectContains(t, res.Body, "公開 2 件 / 下書き 1 件", "「消した記事」をスキップしました")

	if after := env.importCounts(); after != before {
		t.Errorf("preview changed the database: before %+v, after %+v", before, after)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM categories WHERE name = 'サークル'`); n != 0 {
		t.Errorf("preview saved category サークル (%d rows)", n)
	}
	m := importTokenPattern.FindStringSubmatch(res.Body)
	if m == nil {
		t.Fatalf("preview has no token\n%s", res.Body)
	}
	// 「この内容で取り込む」まで、ファイルを預かっておく。
	stage, ok := env.server.imports.get(m[1], admin.ID)
	if !ok {
		t.Fatal("preview did not stage the uploaded file")
	}
	if _, err := os.Stat(stage.Path); err != nil {
		t.Fatalf("staged file: %v", err)
	}
	// 確認した人のほかは、預かったファイルを取り込めない。
	if _, ok := env.server.imports.get(m[1], chief.ID); ok {
		t.Error("another user could use the staged file")
	}
}

func TestMarkdownImportPreview(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	res := c.postImport(importPreviewPath, importDefaults, "welcome.md", "# Welcome\n\nMarkdown body")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "welcome.md", "id=\"import-stat-articles\"", ">1</p>")
	if n := env.countRows(`SELECT COUNT(*) FROM articles`); n != 0 {
		t.Errorf("articles after Markdown preview = %d, want 0", n)
	}
}

func TestImportMarkdownZipErrorsAndLang(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)

	makeZip := func(files map[string]string) string {
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		for name, content := range files {
			f, err := w.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.Write([]byte(content))
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	// 1. 不正な Zip
	res := c.postImport(importPreviewPath, importDefaults, "broken.zip", "not a zip archive")
	expectStatus(t, res, http.StatusBadRequest)
	expectContains(t, res.Body, "Zip ファイルを読み取れませんでした")

	// 2. Markdown が1件もない Zip
	res = c.postImport(importPreviewPath, importDefaults, "empty.zip", makeZip(map[string]string{"readme.txt": "hello"}))
	expectStatus(t, res, http.StatusBadRequest)
	expectContains(t, res.Body, "取り込める Markdown ファイル（.md）が見つかりませんでした")

	// 3. Zip Slip
	res = c.postImport(importPreviewPath, importDefaults, "slip.zip", makeZip(map[string]string{"../evil.md": "# evil"}))
	expectStatus(t, res, http.StatusBadRequest)
	expectContains(t, res.Body, "不正なパスが含まれているため取り込めません")

	// 4. frontmatter 構文エラー
	res = c.postImport(importPreviewPath, importDefaults, "bad-fm.zip", makeZip(map[string]string{"bad.md": "---\ntitle: [broken\n---\nbody"}))
	expectStatus(t, res, http.StatusBadRequest)
	expectContains(t, res.Body, "Markdown ファイルを読み取れませんでした")

	// 5. lang: en の Markdown Zip 取り込み
	res = c.postImport(importRunPath, importDefaults, "docs.zip", makeZip(map[string]string{
		"en-doc.md": "---\ntitle: English Doc\nslug: en-doc\nlang: en\nstatus: publish\n---\nHello world",
	}))
	expectStatus(t, res, http.StatusOK)
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE slug = 'en-doc' AND lang = 'en'`); n != 1 {
		t.Errorf("en-doc with lang='en' count = %d, want 1", n)
	}
}

func TestImportRunFromPreview(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	// 取り込む前に旧 URL を開き、転送のキャッシュを読み込ませておく（取り込み後に読み直すことを確かめる）。
	anon := env.newClient()
	expectStatus(t, anon.get("/2023/04/keion-live/"), http.StatusNotFound)
	before := env.importCounts()

	preview := c.postImport(importPreviewPath, map[string]string{"download_images": "1", "account_mode": importAccountCurrent, "dry_run": "1"}, "wordpress.xml", importTestWXR)
	expectStatus(t, preview, http.StatusOK)
	expectContains(t, preview.Body, `id="import-stat-author"`, "編集長")
	m := importTokenPattern.FindStringSubmatch(preview.Body)
	if m == nil {
		t.Fatalf("preview has no token\n%s", preview.Body)
	}
	stage, ok := env.server.imports.get(m[1], admin.ID)
	if !ok {
		t.Fatal("preview did not stage the uploaded file")
	}

	res := c.postForm(importRunPath, map[string][]string{importTokenField: {m[1]}})
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "取り込みが終わりました", `id="import-to-articles"`, `href="/admin/articles"`,
		`id="import-stat-articles" class="mt-1 text-xl font-bold tabular-nums text-neutral-900">3<`,
		`id="import-stat-indexed"`, `id="import-stat-images"`, "成功 0 枚 / 失敗 0 枚")

	after := env.importCounts()
	if after.articles != before.articles+3 {
		t.Errorf("articles = %d, want %d", after.articles, before.articles+3)
	}
	if after.categories != before.categories+1 {
		t.Errorf("categories = %d, want %d", after.categories, before.categories+1)
	}
	// 自分の記事にまとめるため、ユーザーは作らない。
	if after.users != before.users {
		t.Errorf("users = %d, want %d (no new users)", after.users, before.users)
	}
	if after.redirects <= before.redirects {
		t.Errorf("redirects = %d, want more than %d", after.redirects, before.redirects)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE author_id = ?`, admin.ID); n != 3 {
		t.Errorf("articles by admin = %d, want 3", n)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles a JOIN categories c ON c.id = a.category_id
		WHERE a.slug = 'keion-live' AND a.status = 'published' AND c.name = 'サークル'`); n != 1 {
		t.Errorf("published article keion-live in サークル = %d, want 1", n)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE slug = 'about-circle' AND kind = 'page'`); n != 1 {
		t.Errorf("page about-circle = %d, want 1", n)
	}
	// 旧 URL は新しい記事へ転送する（取り込みで転送のキャッシュを読み直す）。
	old := anon.get("/2023/04/keion-live/")
	expectStatus(t, old, http.StatusMovedPermanently)
	if loc := old.Header.Get("Location"); loc != "/articles/keion-live" {
		t.Errorf("redirect Location = %q, want /articles/keion-live", loc)
	}
	// 取り込みに使ったファイルは消し、同じ合言葉ではもう取り込めない。
	if _, err := os.Stat(stage.Path); !os.IsNotExist(err) {
		t.Errorf("staged file still exists: %v", err)
	}
	again := c.postForm(importRunPath, map[string][]string{importTokenField: {m[1]}})
	expectStatus(t, again, http.StatusBadRequest)
	expectContains(t, again.Body, "有効期限が切れたか、すでに取り込み済みです")
	if got := env.importCounts(); got != after {
		t.Errorf("reused token changed the database: %+v, want %+v", got, after)
	}
}

func TestImportRunDirectlyWithAuthorsAndSkipPages(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	before := env.importCounts()
	// 「先に確認する」を外し、画像を取り込まず、固定ページを取り込まず、WordPress の著者ごとにユーザーを作る。
	res := c.postImport(importRunPath, map[string]string{"skip_pages": "1", "account_mode": importAccountAuthors}, "wordpress.xml", importTestWXR)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "取り込みが終わりました", "画像は元のアドレスのまま", "固定ページは取り込まない",
		`id="import-stat-articles" class="mt-1 text-xl font-bold tabular-nums text-neutral-900">2<`,
		`id="import-stat-users" class="mt-1 text-xl font-bold tabular-nums text-neutral-900">2<`)

	after := env.importCounts()
	if after.articles != before.articles+2 || after.users != before.users+2 || after.categories != before.categories+1 {
		t.Errorf("counts = %+v, want +2 articles, +2 users, +1 category from %+v", after, before)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE kind = 'page'`); n != 0 {
		t.Errorf("pages = %d, want 0 (skip_pages)", n)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles a JOIN users u ON u.id = a.author_id
		WHERE a.slug = 'keion-live' AND u.username = 'hanako' AND u.display_name = '山田花子'`); n != 1 {
		t.Errorf("keion-live by hanako = %d, want 1", n)
	}

	// 同じファイルをもう一度取り込んでも、取り込み済みの記事は重ねて登録しない。
	res = c.postImport(importRunPath, map[string]string{"skip_pages": "1", "account_mode": importAccountAuthors}, "wordpress.xml", importTestWXR)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="import-stat-articles" class="mt-1 text-xl font-bold tabular-nums text-neutral-900">0<`)
	if got := env.importCounts(); got.articles != after.articles {
		t.Errorf("articles = %d after re-import, want %d", got.articles, after.articles)
	}
}

// 設定の入口の「データの取り込み」は、取り込み元を選ぶ画面（/admin/settings/migration）を開く。管理者だけに出す。
func TestSettingsHubShowsMigrationCard(t *testing.T) {
	env := newTestEnv(t)
	res := env.loggedIn(admin).get(settingsHubPath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `href="/admin/settings/migration" id="hub-migration"`, translateOr("ja", "settings_nav_migration"),
		translateOr("ja", "hub_import_desc"))
	expectNotContains(t, res.Body, `id="hub-import"`)
	for _, u := range []testUser{studentA, chief} {
		if body := env.loggedIn(u).get(settingsHubPath).Body; strings.Contains(body, `id="hub-migration"`) {
			t.Errorf("%s: settings hub shows the migration card", u.Username)
		}
	}
}

func TestImportStageExpires(t *testing.T) {
	var st importState
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "stage-*.xml")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	token, err := st.put(&importStage{Path: f.Name(), UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	// Expires を過ぎたものは返さず、ファイルも消す。
	if _, ok := st.get(token, "u1"); ok {
		t.Fatal("expired stage was returned")
	}
	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
		t.Errorf("expired stage file still exists: %v", err)
	}
}

// 確認をやり直すと、同じ人が前に確認したファイルは消し、最新の1つだけを預かる（ほかの人の分は残す）。
func TestImportStageKeepsLatestPerUser(t *testing.T) {
	var st importState
	dir := t.TempDir()
	stage := func(user string) (string, string) {
		f, err := os.CreateTemp(dir, "stage-*.xml")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		token, err := st.put(&importStage{Path: f.Name(), UserID: user, Expires: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return token, f.Name()
	}
	first, firstPath := stage("u1")
	other, _ := stage("u2")
	second, _ := stage("u1")
	if _, ok := st.get(first, "u1"); ok {
		t.Error("older stage of the same user was kept")
	}
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Errorf("older stage file still exists: %v", err)
	}
	if _, ok := st.get(second, "u1"); !ok {
		t.Error("latest stage was dropped")
	}
	if _, ok := st.get(other, "u2"); !ok {
		t.Error("another user's stage was dropped")
	}
	st.clear()
	if _, ok := st.get(second, "u1"); ok {
		t.Error("clear left a stage")
	}
}
