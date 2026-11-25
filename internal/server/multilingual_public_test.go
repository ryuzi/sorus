package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/database"
)

func TestMultilingual_PublicRoutingAndSEO(t *testing.T) {
	env := newTestEnvWith(t, Config{
		BaseURL:         "http://example.com",
		SiteName:        "Multilingual News",
		SiteDescription: "Global editorial",
	}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "ニュース", "news", "最新情報")

		// 翻訳クラスタ 1: 日本語と英語のペア記事
		seedArticle(t, db, article.Article{
			ID:                 "art-ja-1",
			Title:              "日本のAI技術革新",
			Slug:               "japan-ai-innovation",
			CategoryID:         catID,
			Excerpt:            "AIの最新動向",
			ContentMarkdown:    "## 本文\n\n日本のAI技術が急速に進展しています。",
			Status:             article.StatusPublished,
			PublishedAt:        ago(2 * time.Hour),
			Lang:               "ja",
			TranslationGroupID: "cluster-1",
		})
		seedArticle(t, db, article.Article{
			ID:                 "art-en-1",
			Title:              "Japan AI Innovation",
			Slug:               "japan-ai-innovation-en",
			CategoryID:         catID,
			Excerpt:            "Latest trends in AI",
			ContentMarkdown:    "## Overview\n\nJapanese AI technology is progressing rapidly.",
			Status:             article.StatusPublished,
			PublishedAt:        ago(1 * time.Hour),
			Lang:               "en",
			TranslationGroupID: "cluster-1",
		})

		// 翻訳クラスタ 2: 日本語のみの記事（英語未翻訳）
		seedArticle(t, db, article.Article{
			ID:                 "art-ja-solo",
			Title:              "ローカルな学術通信",
			Slug:               "local-academic-note",
			CategoryID:         catID,
			Excerpt:            "地域限定の通信",
			ContentMarkdown:    "学内限定のレポートです。",
			Status:             article.StatusPublished,
			PublishedAt:        ago(3 * time.Hour),
			Lang:               "ja",
			TranslationGroupID: "cluster-solo",
		})

		// 固定ページペア（About Us）
		seedArticle(t, db, article.Article{
			ID:                 "page-ja-1",
			Title:              "当サイトについて",
			Slug:               "about",
			Kind:               article.KindPage,
			ContentMarkdown:    "サイトの紹介です。",
			Status:             article.StatusPublished,
			PublishedAt:        ago(10 * time.Hour),
			Lang:               "ja",
			TranslationGroupID: "cluster-about",
		})
		seedArticle(t, db, article.Article{
			ID:                 "page-en-1",
			Title:              "About Us",
			Slug:               "about-en",
			Kind:               article.KindPage,
			ContentMarkdown:    "About our editorial mission.",
			Status:             article.StatusPublished,
			PublishedAt:        ago(9 * time.Hour),
			Lang:               "en",
			TranslationGroupID: "cluster-about",
		})
	})

	c := env.newClient()

	// 1. トップページ
	t.Run("Japanese Home", func(t *testing.T) {
		res := c.get("/")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="ja"`,
			`content="ja_JP"`,
			`class="lang-switcher"`,
			`<span class="lang-switcher-current" aria-current="true">JA</span>`,
			`<a href="/en/" class="lang-switcher-link" hreflang="en"`,
			`日本のAI技術革新`,
			`ローカルな学術通信`,
		)
		// 英語記事が日本語トップに混入していないこと
		if strings.Contains(res.Body, "Japan AI Innovation") {
			t.Errorf("English article leaked into Japanese home: %s", res.Body)
		}
	})

	t.Run("English Home", func(t *testing.T) {
		for _, path := range []string{"/en", "/en/"} {
			res := c.get(path)
			expectStatus(t, res, http.StatusOK)
			expectContains(t, res.Body,
				`<html lang="en"`,
				`content="en_US"`,
				`class="lang-switcher"`,
				`<span class="lang-switcher-current" aria-current="true">EN</span>`,
				`<a href="/" class="lang-switcher-link" hreflang="ja"`,
				`Japan AI Innovation`,
				`Latest Articles`,
			)
			// 日本語記事が英語トップに混入していないこと
			if strings.Contains(res.Body, "日本のAI技術革新") {
				t.Errorf("Japanese article leaked into English home: %s", res.Body)
			}
		}
	})

	// 2. 記事一覧
	t.Run("Japanese Articles List", func(t *testing.T) {
		res := c.get("/articles")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="ja"`,
			`<span class="lang-switcher-current" aria-current="true">JA</span>`,
			`<a href="/en/articles" class="lang-switcher-link" hreflang="en"`,
			`日本のAI技術革新`,
		)
		if strings.Contains(res.Body, "Japan AI Innovation") {
			t.Errorf("English article leaked into Japanese articles list")
		}
	})

	t.Run("English Articles List", func(t *testing.T) {
		res := c.get("/en/articles")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="en"`,
			`<span class="lang-switcher-current" aria-current="true">EN</span>`,
			`<a href="/articles" class="lang-switcher-link" hreflang="ja"`,
			`Japan AI Innovation`,
		)
		if strings.Contains(res.Body, "日本のAI技術革新") {
			t.Errorf("Japanese article leaked into English articles list")
		}
	})

	// 3. 記事詳細 & hreflang
	t.Run("Japanese Article Detail with Translation", func(t *testing.T) {
		res := c.get("/articles/japan-ai-innovation")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="ja"`,
			`<link rel="canonical" href="http://example.com/articles/japan-ai-innovation">`,
			`<link rel="alternate" hreflang="ja" href="http://example.com/articles/japan-ai-innovation">`,
			`<link rel="alternate" hreflang="en" href="http://example.com/en/articles/japan-ai-innovation-en">`,
			`<link rel="alternate" hreflang="x-default" href="http://example.com/articles/japan-ai-innovation">`,
			`<span class="lang-switcher-current" aria-current="true">JA</span>`,
			`<a href="/en/articles/japan-ai-innovation-en" class="lang-switcher-link" hreflang="en" title="Japan AI Innovation">EN</a>`,
			`日本のAI技術革新`,
		)
	})

	t.Run("English Article Detail with Translation", func(t *testing.T) {
		res := c.get("/en/articles/japan-ai-innovation-en")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="en"`,
			`<link rel="canonical" href="http://example.com/en/articles/japan-ai-innovation-en">`,
			`<link rel="alternate" hreflang="ja" href="http://example.com/articles/japan-ai-innovation">`,
			`<link rel="alternate" hreflang="en" href="http://example.com/en/articles/japan-ai-innovation-en">`,
			`<link rel="alternate" hreflang="x-default" href="http://example.com/articles/japan-ai-innovation">`,
			`<span class="lang-switcher-current" aria-current="true">EN</span>`,
			`<a href="/articles/japan-ai-innovation" class="lang-switcher-link" hreflang="ja" title="日本のAI技術革新">JA</a>`,
			`Japan AI Innovation`,
		)
	})

	t.Run("Japanese Article Without English Translation", func(t *testing.T) {
		res := c.get("/articles/local-academic-note")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="ja"`,
			`<span class="lang-switcher-current" aria-current="true">JA</span>`,
			`<span class="lang-switcher-disabled" title="Not available in EN">EN</span>`,
			`<link rel="alternate" hreflang="ja" href="http://example.com/articles/local-academic-note">`,
		)
		if strings.Contains(res.Body, `hreflang="en"`) {
			t.Errorf("Should not contain en hreflang tag when no English translation exists")
		}
	})

	// 4. URL 言語分離（別言語のスラッグにアクセスしたときの 404）
	t.Run("Cross-language isolation", func(t *testing.T) {
		// 英語スラッグを /articles/ で要求 -> 404 (日本語 not_found)
		res := c.get("/articles/japan-ai-innovation-en")
		expectStatus(t, res, http.StatusNotFound)
		expectContains(t, res.Body, "ページが見つかりません", "トップページへ戻る")

		// 日本語スラッグを /en/articles/ で要求 -> 404 (英語 not_found)
		res = c.get("/en/articles/japan-ai-innovation")
		expectStatus(t, res, http.StatusNotFound)
		expectContains(t, res.Body, "Page Not Found", "Back to Top")
	})

	// 5. 固定ページ（Page）
	t.Run("Japanese Page with English Translation", func(t *testing.T) {
		res := c.get("/about")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="ja"`,
			`<link rel="canonical" href="http://example.com/about">`,
			`<link rel="alternate" hreflang="ja" href="http://example.com/about">`,
			`<link rel="alternate" hreflang="en" href="http://example.com/en/about-en">`,
			`<a href="/en/about-en" class="lang-switcher-link" hreflang="en" title="About Us">EN</a>`,
			`当サイトについて`,
		)
	})

	t.Run("English Page with Japanese Translation", func(t *testing.T) {
		res := c.get("/en/about-en")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`<html lang="en"`,
			`<link rel="canonical" href="http://example.com/en/about-en">`,
			`<link rel="alternate" hreflang="ja" href="http://example.com/about">`,
			`<link rel="alternate" hreflang="en" href="http://example.com/en/about-en">`,
			`<a href="/about" class="lang-switcher-link" hreflang="ja" title="当サイトについて">JA</a>`,
			`About Us`,
		)
	})

	// 6. サイトマップ（多言語 & alternate リンク）
	t.Run("Multilingual Sitemap", func(t *testing.T) {
		res := c.get("/sitemap.xml")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body,
			`xmlns:xhtml="http://www.w3.org/1999/xhtml"`,
			`<loc>http://example.com/</loc>`,
			`<loc>http://example.com/en/</loc>`,
			`<loc>http://example.com/articles/japan-ai-innovation</loc>`,
			`<loc>http://example.com/en/articles/japan-ai-innovation-en</loc>`,
			`xhtml:link rel="alternate" hreflang="ja" href="http://example.com/articles/japan-ai-innovation"`,
			`xhtml:link rel="alternate" hreflang="en" href="http://example.com/en/articles/japan-ai-innovation-en"`,
			`xhtml:link rel="alternate" hreflang="x-default" href="http://example.com/articles/japan-ai-innovation"`,
		)
	})
}

func TestMultilingual_StaticExport(t *testing.T) {
	tempDir := t.TempDir()
	db, err := database.Connect(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("Connect db: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// ユーザー作成（記事の著者外部キー用）
	_, err = db.Exec(`INSERT INTO users (id, username, password_hash, role) VALUES ('user-a', 'yamada', 'hash', 'writer')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	catID := seedCategory(t, db, "ニュース", "news", "最新情報")
	now := time.Now().UTC()

	// 日本語記事
	seedArticle(t, db, article.Article{
		ID:                 "ja-1",
		Title:              "日本語記事",
		Slug:               "ja-post",
		CategoryID:         catID,
		ContentMarkdown:    "日本語の本文です。",
		Status:             article.StatusPublished,
		PublishedAt:        &now,
		Lang:               "ja",
		TranslationGroupID: "cluster-1",
	})

	// 英語記事
	seedArticle(t, db, article.Article{
		ID:                 "en-1",
		Title:              "English Post",
		Slug:               "en-post",
		CategoryID:         catID,
		ContentMarkdown:    "English post content.",
		Status:             article.StatusPublished,
		PublishedAt:        &now,
		Lang:               "en",
		TranslationGroupID: "cluster-1",
	})

	// 英語固定ページ
	seedArticle(t, db, article.Article{
		ID:                 "en-page",
		Title:              "English Page",
		Slug:               "terms-en",
		Kind:               article.KindPage,
		ContentMarkdown:    "Terms and conditions.",
		Status:             article.StatusPublished,
		PublishedAt:        &now,
		Lang:               "en",
		TranslationGroupID: "cluster-terms",
	})

	srv, err := New(Config{
		DB:            db,
		UploadDir:     filepath.Join(tempDir, "uploads"),
		AttachmentDir: filepath.Join(tempDir, "attachments"),
		SiteName:      "Multilingual CMS",
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

	if rep.ArticlesCount != 2 {
		t.Errorf("ArticlesCount = %d, want 2", rep.ArticlesCount)
	}

	// 各種出力ファイルの存在確認
	checks := []struct {
		file     string
		contains string
	}{
		{"index.html", `lang="ja"`},
		{"en/index.html", `lang="en"`},
		{"articles/index.html", `lang="ja"`},
		{"en/articles/index.html", `lang="en"`},
		{"articles/ja-post/index.html", "日本語の本文です。"},
		{"en/articles/en-post/index.html", "English post content."},
		{"en/terms-en/index.html", "Terms and conditions."},
		{"sitemap.xml", `href="http://example.com/en/"`},
	}

	for _, tc := range checks {
		p := filepath.Join(outDir, tc.file)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("expected file %s not found: %v", tc.file, err)
			continue
		}
		if !strings.Contains(string(data), tc.contains) {
			t.Errorf("file %s does not contain %q", tc.file, tc.contains)
		}
	}
}

func TestMultilingual_SwitcherHiddenWithoutEnglishContent(t *testing.T) {
	env := newTestEnvWith(t, Config{
		BaseURL:         "http://example.com",
		SiteName:        "Single Lang CMS",
		SiteDescription: "Japanese only initial state",
	}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "お知らせ", "news", "最新のお知らせ")

		// 英語記事は作成せず、日本語記事のみをシード
		seedArticle(t, db, article.Article{
			ID:              "art-ja-only",
			Title:           "日本語の公開記事",
			Slug:            "single-post",
			CategoryID:      catID,
			Excerpt:         "日本語記事の抜粋",
			ContentMarkdown: "## 見出し\n\n日本語記事の本文です。",
			Status:          article.StatusPublished,
			PublishedAt:     ago(1 * time.Hour),
			Lang:            "ja",
		})
	})

	c := env.newClient()

	// 1. 日本語トップ: 英語記事がないときはスイッチャーが非表示になること
	t.Run("Japanese Home hides switcher without English content", func(t *testing.T) {
		res := c.get("/")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<html lang="ja"`, `日本語の公開記事`)
		expectNotContainsAny(t, "home switcher", res.Body, `class="lang-switcher"`, `lang-switcher-current`)
	})

	// 2. 日本語記事一覧: スイッチャー非表示
	t.Run("Japanese Articles List hides switcher without English content", func(t *testing.T) {
		res := c.get("/articles")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<html lang="ja"`, `日本語の公開記事`)
		expectNotContainsAny(t, "articles switcher", res.Body, `class="lang-switcher"`, `lang-switcher-current`)
	})

	// 3. 日本語カテゴリ: スイッチャー非表示
	t.Run("Japanese Category hides switcher without English content", func(t *testing.T) {
		res := c.get("/categories/news")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<html lang="ja"`, `お知らせ`)
		expectNotContainsAny(t, "category switcher", res.Body, `class="lang-switcher"`, `lang-switcher-current`)
	})

	// 4. 日本語検索: スイッチャー非表示
	t.Run("Japanese Search hides switcher without English content", func(t *testing.T) {
		res := c.get("/search")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<html lang="ja"`)
		expectNotContainsAny(t, "search switcher", res.Body, `class="lang-switcher"`, `lang-switcher-current`)
	})

	// 5. 日本語記事詳細: 英語コンテンツがないときはスイッチャー非表示
	t.Run("Japanese Article Detail hides switcher without English content", func(t *testing.T) {
		res := c.get("/articles/single-post")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<html lang="ja"`, `日本語の公開記事`)
		expectNotContainsAny(t, "article switcher", res.Body, `class="lang-switcher"`, `lang-switcher-current`)
	})
}

func TestMultilingual_SearchAndFrontRedirectAndSitemap(t *testing.T) {
	env := newTestEnvWith(t, Config{
		BaseURL:  "http://example.com",
		SiteName: "Multilingual Lab",
	}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "技術", "tech", "テクノロジー")
		seedArticle(t, db, article.Article{
			ID:                 "post-ja-1",
			Title:              "量子コンピュータ入門",
			Slug:               "quantum-intro",
			CategoryID:         catID,
			ContentMarkdown:    "量子計算の基本概念を解説します。",
			Status:             article.StatusPublished,
			PublishedAt:        ago(2 * time.Hour),
			Lang:               "ja",
			TranslationGroupID: "grp-quantum",
		})
		seedArticle(t, db, article.Article{
			ID:                 "post-en-1",
			Title:              "Quantum Computing Introduction",
			Slug:               "quantum-intro-en",
			CategoryID:         catID,
			ContentMarkdown:    "An introduction to quantum computing fundamentals.",
			Status:             article.StatusPublished,
			PublishedAt:        ago(1 * time.Hour),
			Lang:               "en",
			TranslationGroupID: "grp-quantum",
		})
		// フロントページ用の日本語・英語ペア
		seedArticle(t, db, article.Article{
			ID:                 "page-front-ja",
			Title:              "トップ案内",
			Slug:               "welcome",
			Kind:               article.KindPage,
			ContentMarkdown:    "当サイトへようこそ。",
			Status:             article.StatusPublished,
			PublishedAt:        ago(5 * time.Hour),
			Lang:               "ja",
			TranslationGroupID: "grp-front",
		})
		seedArticle(t, db, article.Article{
			ID:                 "page-front-en",
			Title:              "Welcome Page",
			Slug:               "welcome-en",
			Kind:               article.KindPage,
			ContentMarkdown:    "Welcome to our multilingual website.",
			Status:             article.StatusPublished,
			PublishedAt:        ago(4 * time.Hour),
			Lang:               "en",
			TranslationGroupID: "grp-front",
		})
	})

	c := env.newClient()

	// 1. サイト内検索の言語分離（起動時 SyncFromDB 後の検索結果検証）
	t.Run("Search language separation", func(t *testing.T) {
		// 日本語検索: 日本語記事のみヒットし、英語記事が混ざらないこと
		resJA := c.get("/search?q=量子")
		expectStatus(t, resJA, http.StatusOK)
		if !strings.Contains(resJA.Body, "quantum-intro") {
			t.Errorf("Japanese search missing quantum-intro: %s", resJA.Body)
		}
		if strings.Contains(resJA.Body, "quantum-intro-en") {
			t.Errorf("Japanese search contains English article quantum-intro-en: %s", resJA.Body)
		}

		// 英語検索: 英語記事のみヒットし、日本語記事が混ざらないこと
		resEN := c.get("/en/search?q=quantum")
		expectStatus(t, resEN, http.StatusOK)
		if !strings.Contains(resEN.Body, "quantum-intro-en") {
			t.Errorf("English search missing quantum-intro-en: %s", resEN.Body)
		}
		if strings.Contains(resEN.Body, "/articles/quantum-intro\"") {
			t.Errorf("English search contains Japanese article quantum-intro: %s", resEN.Body)
		}
	})

	// 2. 英語フロントページのリダイレクト (302)
	t.Run("Front page English redirect", func(t *testing.T) {
		// フロントページを設定
		if err := env.server.saveSiteStructure(context.Background(), SiteStructure{FrontPage: "welcome"}); err != nil {
			t.Fatal(err)
		}
		// /welcome は日本語トップ / へ 302
		resJA := c.get("/welcome")
		if resJA.StatusCode != http.StatusFound || resJA.Header.Get("Location") != "/" {
			t.Errorf("/welcome: %d %q, want 302 /", resJA.StatusCode, resJA.Header.Get("Location"))
		}
		// /en/welcome-en は英語トップ /en/ へ 302（一時転送に統一）
		resEN := c.get("/en/welcome-en")
		if resEN.StatusCode != http.StatusFound || resEN.Header.Get("Location") != "/en/" {
			t.Errorf("/en/welcome-en: %d %q, want 302 /en/", resEN.StatusCode, resEN.Header.Get("Location"))
		}
	})

	// 2.5 英語トップ /en および /en/ へのリクエストがリダイレクトテーブルに登録されていても横取りされず 200 OK を返すこと（slug en 保護の回帰防止）
	t.Run("English top not intercepted by redirects", func(t *testing.T) {
		if _, err := env.db.Exec(`INSERT INTO redirects (old_path, new_path, status_code) VALUES ('/en', '/somewhere', 301), ('/en/', '/somewhere', 301)`); err != nil {
			t.Fatalf("insert en redirects: %v", err)
		}
		env.server.redirects.reset()
		defer func() {
			_, _ = env.db.Exec(`DELETE FROM redirects WHERE old_path IN ('/en', '/en/')`)
			env.server.redirects.reset()
		}()

		resEn := c.get("/en")
		expectStatus(t, resEn, http.StatusOK)
		expectContains(t, resEn.Body, "Welcome Page", "Latest News", "All Articles")
		resEnSlash := c.get("/en/")
		expectStatus(t, resEnSlash, http.StatusOK)
	})

	// 3. サイトマップでの多言語リンクとフロントページの重複除外
	t.Run("Sitemap multilingual links", func(t *testing.T) {
		res := c.get("/sitemap.xml")
		expectStatus(t, res, http.StatusOK)
		body := res.Body
		expectContains(t, body,
			"<loc>http://example.com/</loc>",
			"<loc>http://example.com/en/</loc>",
			"<loc>http://example.com/articles</loc>",
			"<loc>http://example.com/en/articles</loc>",
			"<loc>http://example.com/articles/quantum-intro</loc>",
			"<loc>http://example.com/en/articles/quantum-intro-en</loc>",
		)
		// フロントページは / と /en/ で出ているため、/<slug> や /en/<slug> は除外されていること
		if strings.Contains(body, "<loc>http://example.com/welcome</loc>") {
			t.Errorf("sitemap should not contain Japanese front page slug /welcome")
		}
		if strings.Contains(body, "<loc>http://example.com/en/welcome-en</loc>") {
			t.Errorf("sitemap should not contain English front page slug /en/welcome-en")
		}
	})

	// 4. 静的エクスポートの完全化検証
	t.Run("Static export completeness", func(t *testing.T) {
		outDir := t.TempDir()
		_, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
		if err != nil {
			t.Fatalf("Export failed: %v", err)
		}
		// 検索ページ確認
		for _, rel := range []string{
			"search/index.html",
			"en/search/index.html",
			"en/categories/tech/index.html",
			"en/articles/index.html",
			"en/articles/quantum-intro-en/index.html",
			"en/welcome-en/index.html", // フロント転送スタブ
		} {
			p := filepath.Join(outDir, filepath.FromSlash(rel))
			if _, err := os.Stat(p); err != nil {
				t.Errorf("Expected exported file %s to exist, but got: %v", rel, err)
			}
		}
	})
}

func TestMultilingual_StaticExportFrontPageNoEnTranslation(t *testing.T) {
	tempDir := t.TempDir()
	db, err := database.Connect(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("Connect db: %v", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err = db.Exec(`INSERT INTO users (id, username, password_hash, role) VALUES ('user-a', 'yamada', 'hash', 'writer')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	catID := seedCategory(t, db, "ニュース", "news", "最新情報")
	now := time.Now().UTC()

	// トップ用固定ページ（日本語のみ・英訳なし）
	seedArticle(t, db, article.Article{
		ID:              "page-front-ja-only",
		Title:           "フロントページ日本語",
		Slug:            "front-ja",
		Kind:            article.KindPage,
		ContentMarkdown: "フロントページの本文です。",
		Status:          article.StatusPublished,
		PublishedAt:     &now,
		Lang:            "ja",
	})

	// 英語記事を 13件（publicPerPage=12 を超えて 2 ページ分）シード
	for i := 1; i <= 13; i++ {
		pub := now.Add(-time.Duration(i) * time.Hour)
		seedArticle(t, db, article.Article{
			ID:                 fmt.Sprintf("en-art-%d", i),
			Title:              fmt.Sprintf("English Article %02d", i),
			Slug:               fmt.Sprintf("en-art-%02d", i),
			CategoryID:         catID,
			ContentMarkdown:    fmt.Sprintf("English article content %d.", i),
			Status:             article.StatusPublished,
			PublishedAt:        &pub,
			Lang:               "en",
			TranslationGroupID: fmt.Sprintf("cluster-en-%d", i),
		})
	}

	srv, err := New(Config{
		DB:            db,
		UploadDir:     filepath.Join(tempDir, "uploads"),
		AttachmentDir: filepath.Join(tempDir, "attachments"),
		SiteName:      "Multilingual Front Page Test",
		BaseURL:       "http://example.com",
	})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}

	// フロントページに日本語のみのページを設定
	if err := srv.saveSiteStructure(context.Background(), SiteStructure{FrontPage: "front-ja"}); err != nil {
		t.Fatalf("saveSiteStructure: %v", err)
	}

	outDir := filepath.Join(tempDir, "dist")
	_, err = srv.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export error: %v", err)
	}

	// 1. 日本語トップは固定ページのため、page/2/index.html は出力されないこと
	jaPage2 := filepath.Join(outDir, "page", "2", "index.html")
	if _, err := os.Stat(jaPage2); !os.IsNotExist(err) {
		t.Errorf("expected Japanese front page/2/index.html to NOT exist, but stat returned: %v", err)
	}

	// 2. 英語トップは英訳フロントが存在しないため新着一覧となり、en/page/2/index.html が正しく出力されること
	enPage2 := filepath.Join(outDir, "en", "page", "2", "index.html")
	data, err := os.ReadFile(enPage2)
	if err != nil {
		t.Fatalf("expected en/page/2/index.html to exist, but got: %v", err)
	}
	if !strings.Contains(string(data), `lang="en"`) {
		t.Errorf("en/page/2/index.html does not contain lang=\"en\"")
	}
	if !strings.Contains(string(data), "English Article 13") {
		t.Errorf("en/page/2/index.html missing oldest English article")
	}
}

func TestPublicNoExternalResources(t *testing.T) {
	// 1. 公開テンプレート内に外部リソース (cdn, external js/css) の読み込みが無いこと
	pubTmplDir := filepath.Join("..", "..", "templates", "public")
	err := filepath.Walk(pubTmplDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		for _, bad := range []string{
			"cdn.jsdelivr.net",
			"cdnjs.cloudflare.com",
			"unpkg.com",
			"fonts.googleapis.com",
			"fonts.gstatic.com",
		} {
			if strings.Contains(content, bad) {
				t.Errorf("template %s references forbidden external resource: %s", path, bad)
			}
		}
		if strings.Contains(content, `onload=`) {
			t.Errorf("template %s contains forbidden inline onload handler", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates/public: %v", err)
	}

	// 2. CSS ファイル内に外部 URL やプロトコル相対 URL の読み込みが無いこと
	cssDirs := []string{
		filepath.Join("..", "..", "themes"),
		filepath.Join("..", "..", "static", "css"),
	}
	for _, dir := range cssDirs {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".css") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content := string(data)
			if strings.Contains(content, "://") || strings.Contains(content, "url(//") || strings.Contains(content, "url(\"//") || strings.Contains(content, "url('//") {
				t.Errorf("CSS %s contains external url reference", path)
			}
			return nil
		})
	}
}

func TestPublicContentSecurityPolicy(t *testing.T) {
	env := newTestEnv(t)
	// 英語のページ（/en/）の CSP も確かめるため、多言語対応を ON にする（英語の記事の無い新しいサイトの既定は OFF で、/en は 404）。
	if err := upsertSetting(env.db, settingMultilingualEnabled, "1"); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	for _, path := range []string{"/", "/en/", "/articles", "/search", "/en/search"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		csp := res.Header.Get("Content-Security-Policy")
		if csp == "" {
			t.Errorf("path %s missing Content-Security-Policy header", path)
		}
		if !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("path %s Content-Security-Policy missing default-src 'self': %q", path, csp)
		}
		if !strings.Contains(csp, "frame-src 'self' https://www.youtube-nocookie.com") {
			t.Errorf("path %s Content-Security-Policy missing YouTube frame-src: %q", path, csp)
		}
		// 外部の画像は https だけを許す（平文の http の画像は許さない。security_hardening_spec.md §2.5）。
		if !strings.Contains(csp, "img-src 'self' data: https:;") {
			t.Errorf("path %s Content-Security-Policy missing img-src external https images: %q", path, csp)
		}
		if strings.Contains(csp, " http:") {
			t.Errorf("path %s Content-Security-Policy still allows plain http sources: %q", path, csp)
		}
	}

	// 解析タグ設定時に動的オリジンが追加されること
	const tag = `<script defer src="https://static.cloudflareinsights.com/beacon.min.js" data-cf-beacon='{"token":"test-token"}'></script>`
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES ('analytics_tag', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, tag); err != nil {
		t.Fatal(err)
	}

	res := c.get("/")
	expectStatus(t, res, http.StatusOK)
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "https://static.cloudflareinsights.com") {
		t.Errorf("dynamic CSP missing script origin: %q", csp)
	}
	if !strings.Contains(csp, "https://cloudflareinsights.com") {
		t.Errorf("dynamic CSP missing connect origin: %q", csp)
	}

	// エクスポート時の _headers にも動的 CSP が反映されること
	outDir := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatal(err)
	}
	headersData, err := os.ReadFile(filepath.Join(outDir, "_headers"))
	if err != nil {
		t.Fatal(err)
	}
	headersText := string(headersData)
	if !strings.Contains(headersText, "https://static.cloudflareinsights.com") || !strings.Contains(headersText, "https://cloudflareinsights.com") {
		t.Errorf("_headers missing dynamic analytics origins: %s", headersText)
	}
	if !strings.Contains(headersText, "https://www.youtube-nocookie.com") {
		t.Errorf("_headers missing YouTube frame-src: %s", headersText)
	}

	// Google Analytics の検出と connect-src ドメイン追加の検証
	const gaTag = `<script async src="https://www.googletagmanager.com/gtag/js?id=G-TEST1234"></script>`
	gaCSP := buildPublicCSP(gaTag)
	for _, want := range []string{
		"https://www.googletagmanager.com",
		"https://www.google-analytics.com",
		"https://*.google-analytics.com",
		"https://*.analytics.google.com",
		"https://*.googletagmanager.com",
	} {
		if !strings.Contains(gaCSP, want) {
			t.Errorf("Google Analytics CSP missing %q: %s", want, gaCSP)
		}
	}

	// 不正な文字（セミコロン等）を含むスクリプトオリジンが CSP に注入されないこと
	const maliciousTag = `<script src="https://evil.example; script-src 'unsafe-eval'"></script>`
	maliciousCSP := buildPublicCSP(maliciousTag)
	if strings.Contains(maliciousCSP, "evil.example") || strings.Contains(maliciousCSP, "unsafe-eval") {
		t.Errorf("Malicious tag was improperly parsed into CSP: %s", maliciousCSP)
	}
}

func TestPublicMultilingualEditorialCompleteness(t *testing.T) {
	env := newTestEnvWith(t, Config{
		BaseURL:  "http://example.com",
		SiteName: "Academic Review",
	}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "論文", "papers", "学術論文")
		pubDate := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
		seedArticle(t, db, article.Article{
			ID:              "paper-en-1",
			Title:           "Quantum Measurement",
			Slug:            "quantum-measurement",
			CategoryID:      catID,
			Excerpt:         "Study on quantum measurement",
			ContentMarkdown: "## Section\n\nContent with math $E = mc^2$",
			Status:          article.StatusPublished,
			PublishedAt:     &pubDate,
			Lang:            "en",
			CoAuthors:       "Alice, Bob, Charlie",
			JournalName:     "Journal of Physics",
		})
	})

	if err := env.server.search.SyncFromDB(context.Background(), env.db); err != nil {
		t.Fatal(err)
	}

	c := env.newClient()

	// 1. 英語検索ページ
	resSearch := c.get("/en/search")
	expectStatus(t, resSearch, http.StatusOK)
	expectContains(t, resSearch.Body,
		`action="/en/search"`,
		`placeholder="Enter keywords"`,
		`Enter keywords to search articles.`,
	)
	if strings.Contains(resSearch.Body, `action="/search"`) {
		t.Errorf("English search form has Japanese action: %s", resSearch.Body)
	}
	if strings.Contains(resSearch.Body, "キーワードを入力して記事を検索できます。") {
		t.Errorf("English search page has Japanese empty text: %s", resSearch.Body)
	}

	// 2. 英語検索結果
	resSearchResult := c.get("/en/search?q=Quantum")
	expectStatus(t, resSearchResult, http.StatusOK)
	expectContains(t, resSearchResult.Body,
		`Search results for &ldquo;Quantum&rdquo;: 1 results`,
		`Measurement`,
		`Nov 9, 2026`,
	)
	if strings.Contains(resSearchResult.Body, "の検索結果") {
		t.Errorf("English search results leaked Japanese label: %s", resSearchResult.Body)
	}

	// 3. 英語記事詳細（共著者区切りがカンマであること、KaTeX がローカルから読み込まれること）
	resArticle := c.get("/en/articles/quantum-measurement")
	expectStatus(t, resArticle, http.StatusOK)
	expectContains(t, resArticle.Body,
		"Alice, Bob, Charlie",
		"/static/vendor/katex/katex.min.js",
		"/static/vendor/katex/katex.min.css",
	)
	if strings.Contains(resArticle.Body, "Alice、Bob") {
		t.Errorf("English article co-authors separated by Japanese comma: %s", resArticle.Body)
	}
	if strings.Contains(resArticle.Body, "cdn.jsdelivr.net") {
		t.Errorf("Math article loaded external CDN: %s", resArticle.Body)
	}
}
