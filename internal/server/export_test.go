package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/database"
)

func TestServerExport_Integration(t *testing.T) {
	tempDir := t.TempDir()
	db, err := database.Connect(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("Connect db: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.Exec(`UPDATE settings SET value = 'tech' WHERE key = 'theme'`); err != nil {
		t.Fatal(err)
	}

	// カテゴリ作成
	_, err = db.Exec(`INSERT INTO categories (id, name, slug, description) VALUES (1, 'ニュース', 'news', '最新情報')`)
	if err != nil {
		t.Fatalf("insert category: %v", err)
	}

	// ユーザー作成
	_, err = db.Exec(`INSERT INTO users (id, username, password_hash, role) VALUES ('u1', 'editor', 'hash', 'admin')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// 記事作成（公開済み記事 1 件、下書き 1 件）
	pubTime := time.Now().Add(-1 * time.Hour).UTC()
	_, err = db.Exec(`
		INSERT INTO articles (id, title, slug, content_markdown, excerpt, status, author_id, category_id, published_at)
		VALUES ('a1', '静的エクスポートのテスト', 'export-test-post', '# 見出し\n\n静的HTML本文です。 ![サンプル](/uploads/sample.webp)', '抜粋文', ?, 'u1', 1, ?)
	`, article.StatusPublished, pubTime)
	if err != nil {
		t.Fatalf("insert published article: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO articles (id, title, slug, content_markdown, excerpt, status, author_id, category_id, published_at)
		VALUES ('a2', '下書き記事（出力されないこと）', 'draft-post', '下書き', '', ?, 'u1', 1, NULL)
	`, article.StatusDraft)
	if err != nil {
		t.Fatalf("insert draft article: %v", err)
	}

	uploadDir := filepath.Join(tempDir, "uploads")
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "sample.webp"), []byte("RIFFtestWEBP"), 0644); err != nil {
		t.Fatal(err)
	}
	// 公開ページが使わない画像（下書きの記事だけが使う画像など）は書き出さない。
	if err := os.WriteFile(filepath.Join(uploadDir, "unused.webp"), []byte("RIFFunusedWEBP"), 0644); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		DB:            db,
		UploadDir:     uploadDir,
		AttachmentDir: filepath.Join(tempDir, "attachments"),
		SiteName:      "テスト配信サイト",
		BaseURL:       "http://example.com",
	})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}

	outDir := filepath.Join(tempDir, "dist")
	rep, err := srv.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export error: %v", err)
	}

	if rep.ArticlesCount != 1 {
		t.Errorf("ArticlesCount = %d, want 1", rep.ArticlesCount)
	}

	// 1. トップページ
	indexPath := filepath.Join(outDir, "index.html")
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	indexHTML := string(indexBytes)
	if !strings.Contains(indexHTML, "テスト配信サイト") {
		t.Errorf("index.html missing SiteName")
	}
	if !strings.Contains(indexHTML, "静的エクスポートのテスト") {
		t.Errorf("index.html missing published article title")
	}
	if strings.Contains(indexHTML, "下書き記事") {
		t.Errorf("index.html contains draft article")
	}
	if !strings.Contains(indexHTML, `data-theme="tech"`) {
		t.Errorf("index.html does not use the database theme")
	}
	// エクスポートの HTML も版付きの URL を出す（静的ホスティングはクエリを無視して配信する）。
	for _, want := range []string{"/static/themes/tech/theme.css?v=", "/static/css/public.css?v="} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("index.html does not reference %s", want)
		}
	}

	// _headers ファイル（静的ホスティング向けの CSP ヘッダー定義）
	headersPath := filepath.Join(outDir, "_headers")
	headersBytes, err := os.ReadFile(headersPath)
	if err != nil {
		t.Fatalf("read _headers: %v", err)
	}
	headersContent := string(headersBytes)
	if !strings.Contains(headersContent, "Content-Security-Policy: "+DefaultPublicCSP) {
		t.Errorf("_headers missing DefaultPublicCSP: %s", headersContent)
	}
	if strings.Contains(headersContent, "ws:") || strings.Contains(headersContent, "wss:") {
		t.Errorf("_headers DefaultPublicCSP contains WebSocket: %s", headersContent)
	}

	overrideDir := filepath.Join(tempDir, "dist-news")
	if _, err := srv.Export(context.Background(), ExportOptions{OutDir: overrideDir, Theme: "news"}); err != nil {
		t.Fatalf("Export with theme override: %v", err)
	}
	override, err := os.ReadFile(filepath.Join(overrideDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(override), `data-theme="news"`) || !strings.Contains(string(override), "/static/themes/news/theme.css?v=") {
		t.Error("export theme override was not applied")
	}

	// カタログに無いテーマ（廃止した mono を含む）は、何も書き出さずにエラーにする。
	for _, bad := range []string{"mono", "minimal", "../news", " "} {
		badDir := filepath.Join(tempDir, "dist-bad")
		_, err := srv.Export(context.Background(), ExportOptions{OutDir: badDir, Theme: bad})
		if err == nil || !strings.Contains(err.Error(), "unknown theme") {
			t.Errorf("Export(Theme=%q) error = %v, want unknown theme", bad, err)
		}
		if _, err := os.Stat(badDir); !os.IsNotExist(err) {
			t.Errorf("Export(Theme=%q) created %s", bad, badDir)
		}
	}

	// 2. 記事個別ページ
	articlePath := filepath.Join(outDir, "articles", "export-test-post", "index.html")
	articleBytes, err := os.ReadFile(articlePath)
	if err != nil {
		t.Fatalf("read article html: %v", err)
	}
	if !strings.Contains(string(articleBytes), "静的HTML本文です。") {
		t.Errorf("article html missing content")
	}

	// 下書きのファイルが存在しないこと
	draftPath := filepath.Join(outDir, "articles", "draft-post", "index.html")
	if _, err := os.Stat(draftPath); !os.IsNotExist(err) {
		t.Errorf("draft article was exported: %v", draftPath)
	}

	// 3. カテゴリページ
	categoryPath := filepath.Join(outDir, "categories", "news", "index.html")
	if _, err := os.Stat(categoryPath); err != nil {
		t.Errorf("category page missing: %v", err)
	}

	// 4. RSS フィード & サイトマップ
	if _, err := os.Stat(filepath.Join(outDir, "feed.xml")); err != nil {
		t.Errorf("feed.xml missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "sitemap.xml")); err != nil {
		t.Errorf("sitemap.xml missing: %v", err)
	}
	llms, err := os.ReadFile(filepath.Join(outDir, "llms.txt"))
	if err != nil || !strings.Contains(string(llms), "## Articles") || !strings.Contains(string(llms), "export-test-post") || strings.Contains(string(llms), "draft-post") {
		t.Errorf("llms.txt content/error = %q, %v", llms, err)
	}
	llmsFull, err := os.ReadFile(filepath.Join(outDir, "llms-full.txt"))
	if err != nil || !strings.Contains(string(llmsFull), "静的HTML本文です。") || strings.Contains(string(llmsFull), "下書き") {
		t.Errorf("llms-full.txt content/error = %q, %v", llmsFull, err)
	}

	// 5. 静的アセットと画像
	for _, f := range []string{"css/public.css", "themes/modern/theme.css", "themes/tech/theme.css", "themes/news/theme.css", "themes/news/theme.json"} {
		if _, err := os.Stat(filepath.Join(outDir, "static", filepath.FromSlash(f))); err != nil {
			t.Errorf("static/%s missing: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "static", "themes", "mono.css")); !os.IsNotExist(err) {
		t.Errorf("legacy static/themes/mono.css exported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "uploads", "sample.webp")); err != nil {
		t.Errorf("uploads/sample.webp missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "uploads", "unused.webp")); !os.IsNotExist(err) {
		t.Errorf("uploads/unused.webp exported: %v", err)
	}
}

func TestServerExport_PathTraversalPrevention(t *testing.T) {
	tempDir := t.TempDir()
	db, err := database.Connect(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("Connect db: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err = db.Exec(`INSERT INTO categories (id, name, slug, description) VALUES (1, 'ニュース', 'news', '')`)
	if err != nil {
		t.Fatalf("insert category: %v", err)
	}
	_, err = db.Exec(`INSERT INTO users (id, username, password_hash, role) VALUES ('u1', 'editor', 'hash', 'admin')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// パストラバーサルを含む悪意ある slug を投入
	pubTime := time.Now().Add(-1 * time.Hour).UTC()
	_, err = db.Exec(`
		INSERT INTO articles (id, title, slug, content_markdown, excerpt, status, author_id, category_id, published_at)
		VALUES ('a1', '悪意ある記事', '../../escape', '本文', '', ?, 'u1', 1, ?)
	`, article.StatusPublished, pubTime)
	if err != nil {
		t.Fatalf("insert evil article: %v", err)
	}

	srv, err := New(Config{
		DB:       db,
		SiteName: "テスト",
		BaseURL:  "http://example.com",
	})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}

	outDir := filepath.Join(tempDir, "dist")
	_, err = srv.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err == nil {
		t.Fatal("expected Export to fail on path traversal slug, but got nil error")
	}
	if !strings.Contains(err.Error(), "invalid article slug") {
		t.Errorf("error = %v, want invalid article slug", err)
	}

	// outDir 外にファイルが作られていないことを確認
	escapePath := filepath.Join(tempDir, "escape", "index.html")
	if _, err := os.Stat(escapePath); !os.IsNotExist(err) {
		t.Errorf("file was created outside export root: %v", escapePath)
	}
}

// newPagedExportEnv は記事の一覧・カテゴリが3ページになる記事数（publicPerPage*2+1 件）と、下書き・予約投稿・
// 固定ページを用意したサーバーを起動する。
func newPagedExportEnv(t *testing.T, baseURL string) *testEnv {
	t.Helper()
	return newTestEnvWith(t, Config{BaseURL: baseURL, SiteName: "書き出しテスト"}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		news := seedCategory(t, db, "お知らせ", "news", "")
		for i := 1; i <= publicPerPage*2+1; i++ {
			seedArticle(t, db, article.Article{Title: fmt.Sprintf("記事%02d", i), Slug: fmt.Sprintf("post-%d", i), CategoryID: news,
				ContentMarkdown: fmt.Sprintf("本文%02dの内容。量子計算", i), Status: article.StatusPublished, PublishedAt: ago(time.Duration(i) * time.Hour)})
		}
		seedArticle(t, db, article.Article{Title: "下書きの記事", Slug: "draft-post", CategoryID: news, ContentMarkdown: "下書き",
			Status: article.StatusDraft})
		seedArticle(t, db, article.Article{Title: "予約投稿の記事", Slug: "future-post", CategoryID: news, ContentMarkdown: "予約",
			Status: article.StatusPublished, PublishedAt: ago(-24 * time.Hour)})
		seedPage(t, db, article.Article{Title: "アクセス", Slug: "access", ContentMarkdown: "駅から徒歩5分。",
			Status: article.StatusPublished, PublishedAt: ago(time.Minute)})
	})
}

// pagerLinkRe はページ送りの「前へ」「次へ」のリンク先を取り出す。
var pagerLinkRe = regexp.MustCompile(`href="(/[^"?#]*)" rel="(?:next|prev)"`)

// exportedFile は静的ホスティングと同じ規則で、URL のパスを書き出したファイルにする（/x は x/index.html）。
func exportedFile(outDir, urlPath string) string {
	p := filepath.Join(outDir, filepath.FromSlash(strings.TrimPrefix(urlPath, "/")))
	if path.Ext(urlPath) == "" {
		p = filepath.Join(p, "index.html")
	}
	return p
}

func TestServerExport_AllListPages(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	outDir := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none for %s", rep.Warnings, testBaseURL)
	}

	// 記事の一覧・カテゴリ・トップの新着記事は 3 ページすべてを、公開サーバーと同じ URL で開ける場所に書き出す。
	for _, list := range []string{"/", "/articles", "/categories/news"} {
		for n := 1; n <= 3; n++ {
			u := pagedURL(list, n)
			data, err := os.ReadFile(exportedFile(outDir, u))
			if err != nil {
				t.Errorf("%s: %v", u, err)
				continue
			}
			if want := fmt.Sprintf("%d / 3", n); !strings.Contains(string(data), want) {
				t.Errorf("%s does not contain %q", u, want)
			}
		}
		if _, err := os.Stat(exportedFile(outDir, pagedURL(list, 4))); !os.IsNotExist(err) {
			t.Errorf("%s was exported: %v", pagedURL(list, 4), err)
		}
	}
	// 公開サーバーも同じ URL で開け、以前のクエリの形（?page=N）も同じページを返す。
	c := env.newClient()
	for _, u := range []string{"/categories/news/page/2", "/categories/news/page/3", "/categories/news?page=3", "/articles/page/3", "/page/3"} {
		expectStatus(t, c.get(u), http.StatusOK)
	}
	for _, u := range []string{"/categories/news/page/4", "/categories/news/page/1", "/categories/news/page/x", "/categories/missing/page/2"} {
		expectStatus(t, c.get(u), http.StatusNotFound)
	}
	last, err := os.ReadFile(exportedFile(outDir, "/articles/page/3"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(last), "記事25") || strings.Contains(string(last), ">記事01<") {
		t.Error("/articles/page/3 does not list the oldest article")
	}

	// 書き出した HTML のページ送りのリンクは、すべて書き出したファイルを指す。
	links := 0
	err = filepath.WalkDir(outDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".html" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range pagerLinkRe.FindAllStringSubmatch(string(data), -1) {
			links++
			if _, err := os.Stat(exportedFile(outDir, m[1])); err != nil {
				t.Errorf("%s links to %s, which was not exported", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if links < 12 {
		t.Errorf("found %d pager links, want at least 12", links)
	}
}

func TestServerExport_StaticSearch(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	outDir := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// 検索ページはサーバーで検索せず、static-search.js が検索用インデックスを読む形で書き出す。
	page, err := os.ReadFile(filepath.Join(outDir, "search", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	expectContains(t, string(page), `data-static-search="#static-search-results"`, `data-index="/search-index.json"`,
		"/static/js/static-search.js?v=", `name="q"`, `<meta name="robots" content="noindex">`)
	if _, err := os.Stat(filepath.Join(outDir, "static", "js", "static-search.js")); err != nil {
		t.Errorf("static-search.js was not exported: %v", err)
	}
	// 公開サーバーの検索ページは今までどおりサーバーで検索する。
	res := env.newClient().get("/search?q=" + url.QueryEscape("量子計算"))
	expectStatus(t, res, http.StatusOK)
	expectNotContainsAny(t, "/search", res.Body, "data-static-search", "static-search.js")

	data, err := os.ReadFile(filepath.Join(outDir, "search-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Articles []searchIndexEntry `json:"articles"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("search-index.json: %v", err)
	}
	byURL := map[string]searchIndexEntry{}
	for _, e := range index.Articles {
		byURL[e.URL] = e
	}
	// 公開中で公開日時を迎えた記事 25 件とページ 1 件だけを入れる（下書き・予約投稿は入れない）。
	if len(index.Articles) != publicPerPage*2+2 {
		t.Errorf("search index has %d items, want %d", len(index.Articles), publicPerPage*2+2)
	}
	for _, u := range []string{"/articles/draft-post", "/articles/future-post"} {
		if _, ok := byURL[u]; ok {
			t.Errorf("search index contains %s", u)
		}
	}
	if strings.Contains(string(data), "下書きの記事") || strings.Contains(string(data), "予約投稿の記事") {
		t.Error("search index contains an unpublished title")
	}
	post, ok := byURL["/articles/post-1"]
	if !ok {
		t.Fatalf("search index does not contain /articles/post-1: %s", data)
	}
	if post.Title != "記事01" || post.Kind != "post" || post.Category != "お知らせ" || post.Text != "本文01の内容。量子計算" {
		t.Errorf("post-1 entry = %+v", post)
	}
	if _, err := time.Parse(time.RFC3339, post.Published); err != nil {
		t.Errorf("published = %q: %v", post.Published, err)
	}
	if p, ok := byURL["/access"]; !ok || p.Kind != "page" {
		t.Errorf("page entry = %+v, %v", p, ok)
	}
	// どの項目の URL も書き出したファイルを指す。
	for _, e := range index.Articles {
		if _, err := os.Stat(exportedFile(outDir, e.URL)); err != nil {
			t.Errorf("search index links to %s, which was not exported", e.URL)
		}
	}
}

func TestServerExport_SearchIndexTruncatesText(t *testing.T) {
	long := strings.Repeat("あ", searchTextRunes+500)
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{Title: "長い記事", Slug: "long", ContentMarkdown: long,
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	dest := filepath.Join(t.TempDir(), "search-index.json")
	if err := env.server.writeSearchIndex(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Articles []searchIndexEntry `json:"articles"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Articles) != 1 || utf8.RuneCountInString(index.Articles[0].Text) != searchTextRunes {
		t.Errorf("text runes = %d, want %d", utf8.RuneCountInString(index.Articles[0].Text), searchTextRunes)
	}
	if index.Articles[0].Lang != "ja" {
		t.Errorf("article lang = %q, want 'ja'", index.Articles[0].Lang)
	}
}

func TestServerExport_CleanRemovesStaleFiles(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	parent := t.TempDir()
	outDir := filepath.Join(parent, "dist")

	// 上書きだけの書き出し（既定）は、前回の残りをそのままにする。
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(outDir, "articles", "removed-post", "index.html")
	if err := os.MkdirAll(filepath.Dir(stale), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("non-clean export removed %s: %v", stale, err)
	}

	// Clean の書き出しは前回の残りを消し、新しいサイト一式と入れ替える。一時フォルダは残さない。
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true})
	if err != nil {
		t.Fatalf("clean Export: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("clean export kept %s: %v", stale, err)
	}
	for _, f := range []string{"index.html", "search-index.json", "articles/page/3/index.html", "categories/news/index.html"} {
		if _, err := os.Stat(filepath.Join(outDir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s missing after clean export: %v", f, err)
		}
	}
	if rep.OutDir != outDir {
		t.Errorf("OutDir = %q, want %q", rep.OutDir, outDir)
	}
	if info, err := os.Stat(outDir); err != nil || info.Mode().Perm() != 0755 {
		t.Errorf("out dir mode = %v, %v; want 0755", info, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "dist" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("parent dir contains %q, want only dist", names)
	}

	// まだ無いフォルダへの Clean の書き出しも作れる。
	fresh := filepath.Join(parent, "new", "site")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: fresh, Clean: true}); err != nil {
		t.Fatalf("clean Export into new dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "index.html")); err != nil {
		t.Error(err)
	}
}

func TestServerExport_CleanRefusesUnsafeDirs(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	mkdir := func(rel string, files ...string) string {
		d := filepath.Join(base, rel)
		for _, f := range files {
			p := filepath.Join(d, filepath.FromSlash(f))
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("keep"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	withGit := mkdir("repo", ".git/HEAD", "index.html")
	withDB := mkdir("data", "backup/sorus.db", "index.html")
	withWAL := mkdir("wal", "sorus.db-wal")
	link := filepath.Join(base, "link")
	if err := os.Symlink(mkdir("real", "index.html"), link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(mkdir("files", "plain.txt"), "plain.txt")

	cases := map[string]string{
		"empty":         "",
		"blank":         "  ",
		"root":          string(filepath.Separator),
		"home":          home,
		"working dir":   wd,
		"wd parent":     filepath.Dir(wd),
		"dot":           ".",
		"git":           withGit,
		"database":      withDB,
		"database wal":  withWAL,
		"symlink":       link,
		"file":          file,
		"uploads":       env.uploadDir,
		"uploads owner": filepath.Dir(env.uploadDir),
	}
	for name, dir := range cases {
		_, err := env.server.Export(context.Background(), ExportOptions{OutDir: dir, Clean: true})
		if !errors.Is(err, ErrUnsafeExportDir) {
			t.Errorf("%s (%q): err = %v, want ErrUnsafeExportDir", name, dir, err)
		}
	}
	// 断ったフォルダの中身は消さない。
	for _, f := range []string{filepath.Join(withGit, ".git", "HEAD"), filepath.Join(withDB, "backup", "sorus.db"), filepath.Join(withWAL, "sorus.db-wal"), file} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed: %v", f, err)
		}
	}
	if entries, _ := os.ReadDir(base); len(entries) != 6 {
		t.Errorf("base dir has %d entries after refused exports, want 6 (no temp dirs left)", len(entries))
	}
	// ErrUnsafeExportDir は日本語で理由を伝える。
	_, err = env.server.Export(context.Background(), ExportOptions{OutDir: withGit, Clean: true})
	if err == nil || !strings.Contains(err.Error(), ".git") || !strings.Contains(err.Error(), "使えません") {
		t.Errorf("err = %v, want a message naming .git", err)
	}
}

// dirNames は dir の中の名前を並べて返す。
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// writeTestFile は p に s を書く（親フォルダも作る）。
func writeTestFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
}

// Clean の書き出しの入れ替えの途中（前回の書き出しを退避し、新しい書き出しを置く前）でプロセスが止まると、
// 書き出し先が無くなる。次の Clean の書き出しは、最初に退避先から前回の書き出しを戻し、書きかけの一時フォルダを消す。
func TestServerExport_CleanRecoversInterruptedSwap(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	parent := t.TempDir()
	outDir := filepath.Join(parent, "dist")
	previous := filepath.Join(outDir, "previous.txt")
	old := filepath.Join(parent, ".dist.export-old")
	interrupted := func() {
		t.Helper()
		if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, previous, "previous")
		if err := os.Rename(outDir, old); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(parent, ".dist.export-new", "index.html"), "partial")
	}

	// 続く書き出しが失敗しても、前回の書き出しは書き出し先に戻っている。
	interrupted()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.server.Export(canceled, ExportOptions{OutDir: outDir, Clean: true}); err == nil {
		t.Fatal("clean Export with a canceled context succeeded")
	}
	if b, err := os.ReadFile(previous); err != nil || string(b) != "previous" {
		t.Errorf("previous export not restored: %q, %v", b, err)
	}
	if names := dirNames(t, parent); len(names) != 1 || names[0] != "dist" {
		t.Errorf("parent dir contains %q, want only dist", names)
	}

	// 続く書き出しが成功すれば、新しい書き出しと入れ替わり、隣に何も残らない。
	interrupted()
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err != nil {
		t.Fatalf("clean Export after an interrupted swap: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(outDir, "index.html")); err != nil || string(b) == "partial" {
		t.Errorf("index.html = %.40q, %v; want the new export", b, err)
	}
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Errorf("clean export kept %s: %v", previous, err)
	}
	if names := dirNames(t, parent); len(names) != 1 || names[0] != "dist" {
		t.Errorf("parent dir contains %q, want only dist", names)
	}

	// 入れ替えのあと退避先を消し終える前に止まったときは、残った退避先を消してから書き出す。
	writeTestFile(t, filepath.Join(old, "index.html"), "older")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err != nil {
		t.Fatalf("clean Export with a leftover previous export: %v", err)
	}
	if names := dirNames(t, parent); len(names) != 1 || names[0] != "dist" {
		t.Errorf("parent dir contains %q, want only dist", names)
	}
}

// 入れ替えの途中で止まったあと、書き出し先に空のフォルダが作られていても（利用者が作り直したなど）、退避先の前回の
// 書き出しを捨てずに書き出し先へ戻す。空でない書き出し先は退避先の消し損ねとして扱い、退避先を消す。
func TestServerExport_CleanRestoresIntoEmptyOutDir(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	parent := t.TempDir()
	outDir := filepath.Join(parent, "dist")
	previous := filepath.Join(outDir, "previous.txt")
	_, old := cleanExportSiblings(outDir)
	interrupted := func() {
		t.Helper()
		if err := os.RemoveAll(outDir); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(old, "previous.txt"), "previous")
		if err := os.Mkdir(outDir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	interrupted()
	restored, err := recoverCleanExport(outDir)
	if err != nil || !restored {
		t.Fatalf("recoverCleanExport = %v, %v; want restored", restored, err)
	}
	if b, err := os.ReadFile(previous); err != nil || string(b) != "previous" {
		t.Errorf("previous export not restored into the empty out dir: %q, %v", b, err)
	}
	if names := dirNames(t, parent); len(names) != 1 || names[0] != "dist" {
		t.Errorf("parent dir contains %q, want only dist", names)
	}

	// 続く書き出しが失敗しても、前回の書き出しは書き出し先に戻っている。
	interrupted()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.server.Export(canceled, ExportOptions{OutDir: outDir, Clean: true}); err == nil {
		t.Fatal("clean Export with a canceled context succeeded")
	}
	if b, err := os.ReadFile(previous); err != nil || string(b) != "previous" {
		t.Errorf("previous export lost: %q, %v", b, err)
	}

	// 書き出し先に中身があれば、退避先は消し損ねた前回の書き出しなので消し、書き出し先の中身は残す。
	writeTestFile(t, filepath.Join(old, "index.html"), "older")
	if restored, err := recoverCleanExport(outDir); err != nil || restored {
		t.Fatalf("recoverCleanExport with a non-empty out dir = %v, %v; want not restored", restored, err)
	}
	if b, err := os.ReadFile(previous); err != nil || string(b) != "previous" {
		t.Errorf("out dir content changed: %q, %v", b, err)
	}
	if names := dirNames(t, parent); len(names) != 1 || names[0] != "dist" {
		t.Errorf("parent dir contains %q, want only dist", names)
	}
}

// 新しい書き出しを書き出し先に置けなかったときは、前回の書き出しを書き出し先に戻す。戻すこともできなかったときは、
// 次の Clean の書き出しで戻す。
func TestServerExport_CleanRollsBackFailedSwap(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	parent := t.TempDir()
	outDir := filepath.Join(parent, "dist")
	previous := filepath.Join(outDir, "previous.txt")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, previous, "previous")
	tmp, old := cleanExportSiblings(outDir)
	failing := map[[2]string]bool{}
	orig := exportRename
	exportRename = func(from, to string) error {
		if failing[[2]string{from, to}] {
			return errors.New("simulated rename failure")
		}
		return orig(from, to)
	}
	t.Cleanup(func() { exportRename = orig })

	failing[[2]string{tmp, outDir}] = true
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err == nil {
		t.Fatal("clean Export succeeded although the new export could not be moved into place")
	}
	if b, err := os.ReadFile(previous); err != nil || string(b) != "previous" {
		t.Errorf("previous export not rolled back: %q, %v", b, err)
	}
	if names := dirNames(t, parent); len(names) != 1 || names[0] != "dist" {
		t.Errorf("parent dir contains %q, want only dist", names)
	}

	failing[[2]string{old, outDir}] = true
	_, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true})
	if err == nil || !strings.Contains(err.Error(), old) {
		t.Fatalf("err = %v, want one naming %s", err, old)
	}
	if _, err := os.Lstat(outDir); !os.IsNotExist(err) {
		t.Fatalf("out dir after a failed rollback: %v", err)
	}
	clear(failing)
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err != nil {
		t.Fatalf("clean Export after a failed rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "index.html")); err != nil {
		t.Error(err)
	}
	if names := dirNames(t, parent); len(names) != 1 || names[0] != "dist" {
		t.Errorf("parent dir contains %q, want only dist", names)
	}
}

// Clean の書き出しが片付けるのは、書き出し先の名前から決まる sorus の一時フォルダと退避先だけ。
// 名前の似たフォルダや、退避先の名前のショートカット（とその先のフォルダ）には触れない。
func TestServerExport_CleanLeavesUnrelatedSiblings(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	parent := t.TempDir()
	outDir := filepath.Join(parent, "dist")
	unrelated := []string{"dist-backup", "dist.export-old", ".dist.export-old.bak", ".dist.export-older", ".dist.export-12345", ".site.export-old", "mine"}
	for _, name := range unrelated {
		writeTestFile(t, filepath.Join(parent, name, "keep.txt"), "keep")
	}
	link := filepath.Join(parent, ".dist.export-old")
	if err := os.Symlink(filepath.Join(parent, "mine"), link); err != nil {
		t.Fatal(err)
	}
	// 書き出し先が無く、退避先の名前にあるのがショートカットでも、それを書き出し先に戻さない。
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err != nil {
		t.Fatalf("clean Export: %v", err)
	}
	if info, err := os.Lstat(outDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("out dir = %v, %v; want a new directory", info, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink at the previous-export name = %v, %v; want it left alone", info, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	// 書き出し先がある通常の入れ替えでも、似た名前のフォルダには触れない。
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true}); err != nil {
		t.Fatalf("second clean Export: %v", err)
	}
	for _, name := range unrelated {
		if b, err := os.ReadFile(filepath.Join(parent, name, "keep.txt")); err != nil || string(b) != "keep" {
			t.Errorf("%s/keep.txt = %q, %v; want it untouched", name, b, err)
		}
	}
	if names := dirNames(t, parent); len(names) != len(unrelated)+1 {
		t.Errorf("parent dir contains %q, want dist and the unrelated folders only", names)
	}
}

func TestServerExportToTemp(t *testing.T) {
	env := newPagedExportEnv(t, testBaseURL)
	dir, rep, err := env.server.ExportToTemp(context.Background(), ExportOptions{OutDir: "ignored", Clean: true})
	if err != nil {
		t.Fatalf("ExportToTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir == "" || rep == nil || rep.OutDir != dir {
		t.Fatalf("dir = %q, rep = %+v", dir, rep)
	}
	if rep.ArticlesCount != publicPerPage*2+1 {
		t.Errorf("ArticlesCount = %d, want %d", rep.ArticlesCount, publicPerPage*2+1)
	}
	for _, f := range []string{"index.html", "search/index.html", "search-index.json", "articles/post-1/index.html", "static/js/static-search.js"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0755 {
		t.Errorf("temp dir mode = %v, %v; want 0755", info, err)
	}
	if _, err := os.Stat("ignored"); !os.IsNotExist(err) {
		t.Errorf("ExportToTemp wrote to opts.OutDir: %v", err)
	}

	// 失敗したときは一時フォルダを残さない。
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "sorus-export-*"))
	if _, _, err := env.server.ExportToTemp(context.Background(), ExportOptions{Theme: "mono"}); err == nil {
		t.Fatal("ExportToTemp with an unknown theme succeeded")
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "sorus-export-*"))
	if len(after) != len(before) {
		t.Errorf("failed ExportToTemp left a temp dir: %d -> %d", len(before), len(after))
	}
}

func TestServerExport_BaseURLWarnings(t *testing.T) {
	for _, base := range []string{"", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]", "http://0.0.0.0", "http://sorus.localhost"} {
		env := newPagedExportEnv(t, base)
		_, rep, err := env.server.ExportToTemp(context.Background(), ExportOptions{})
		if err != nil {
			t.Fatalf("%q: ExportToTemp: %v", base, err)
		}
		os.RemoveAll(rep.OutDir)
		if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "公開サイトURL") {
			t.Errorf("%q: Warnings = %q, want one warning about 公開サイトURL", base, rep.Warnings)
		}
	}
	for _, base := range []string{testBaseURL, "https://localhost.example.jp", "http://192.168.1.10"} {
		if w := baseURLWarnings(base); len(w) != 0 {
			t.Errorf("%q: warnings = %q, want none", base, w)
		}
	}
}

// TestExportWithCustomTheme は、カスタムテーマを使うサイトの書き出しで、テーマの CSS とアセットが公開サーバーと
// 同じ URL（/custom-themes/<id>/）に書き出され、HTML の CSS のリンクから読めることを確かめる。
func TestExportWithCustomTheme(t *testing.T) {
	env, _ := newCustomThemeEnv(t, func(dir string) {
		root := writeTestTheme(t, dir, "acme", "アクメ")
		if err := os.WriteFile(filepath.Join(root, ".secret.css"), []byte("SECRET"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeTestTheme(t, dir, "beta", "ベータ")
	})
	stylesheet := regexp.MustCompile(`<link rel="stylesheet" href="(/custom-themes/[^"]+|/static/themes/[^"]+)">`)
	exportWith := func(t *testing.T, opts ExportOptions) string {
		t.Helper()
		opts.OutDir = filepath.Join(t.TempDir(), "dist")
		if _, err := env.server.Export(context.Background(), opts); err != nil {
			t.Fatalf("Export: %v", err)
		}
		return opts.OutDir
	}
	expectCustomTheme := func(t *testing.T, out, id string) {
		t.Helper()
		index, err := os.ReadFile(filepath.Join(out, "index.html"))
		if err != nil {
			t.Fatal(err)
		}
		m := stylesheet.FindStringSubmatch(string(index))
		if m == nil {
			t.Fatalf("index.html has no stylesheet link: %.1000s", index)
		}
		u, err := url.Parse(m[1])
		if err != nil || u.Path != "/custom-themes/"+id+"/theme.css" {
			t.Fatalf("stylesheet = %q, want /custom-themes/%s/theme.css", m[1], id)
		}
		// HTML のリンク（クエリを除いたパス）が、書き出したファイルを指す。
		css, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(strings.TrimPrefix(u.Path, "/"))))
		if err != nil {
			t.Fatalf("linked stylesheet was not exported: %v", err)
		}
		if !strings.Contains(string(css), "#123456") {
			t.Errorf("exported theme.css = %q", css)
		}
		for _, rel := range []string{"img/bg.png", "fonts/a.woff"} {
			if _, err := os.Stat(filepath.Join(out, "custom-themes", id, filepath.FromSlash(rel))); err != nil {
				t.Errorf("asset %s was not exported: %v", rel, err)
			}
		}
		// 公開サーバーが配信しないもの（theme.json・隠しファイル）は書き出さない。
		for _, rel := range []string{"theme.json", ".secret.css"} {
			if _, err := os.Stat(filepath.Join(out, "custom-themes", id, rel)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s should not be exported: %v", rel, err)
			}
		}
	}

	t.Run("saved custom theme", func(t *testing.T) {
		env.setTheme("acme")
		out := exportWith(t, ExportOptions{})
		expectCustomTheme(t, out, "acme")
		// 使わないカスタムテーマは書き出さない。
		if _, err := os.Stat(filepath.Join(out, "custom-themes", "beta")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("unused custom theme was exported: %v", err)
		}
	})
	t.Run("theme override", func(t *testing.T) {
		env.setTheme("modern")
		expectCustomTheme(t, exportWith(t, ExportOptions{Theme: "beta"}), "beta")
	})
	t.Run("built-in theme exports no custom themes", func(t *testing.T) {
		env.setTheme("tech")
		out := exportWith(t, ExportOptions{})
		if _, err := os.Stat(filepath.Join(out, "custom-themes")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("custom-themes was exported for a built-in theme: %v", err)
		}
	})
}
