package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

func TestHeadlessExport_SitePagesArticlesAndPrivacyFilter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	setClock(t, now)

	var (
		newsCatID  int64
		emptyCatID int64
		oldPostID  string
		newPostID  string
		aboutID    string
		contactID  string
	)
	env := newTestEnvWith(t, Config{
		BaseURL:         testBaseURL,
		SiteName:        "Headless テストサイト",
		SiteDescription: "Headless JSON 出力の検証",
		ShowAuthor:      false,
	}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		newsCatID = seedCategory(t, db, "お知らせ", "news", "最新のお知らせ")
		emptyCatID = seedCategory(t, db, "下書き専用カテゴリ", "draft-only-cat", "")

		tOld := now.Add(-2 * time.Hour)
		tNew := now.Add(-1 * time.Hour)
		tFuture := now.Add(2 * time.Hour)

		oldPostID = seedArticle(t, db, article.Article{
			Title:           "最初の公開記事",
			Slug:            "first-post",
			Excerpt:         "最初の記事の抜粋",
			FeaturedImage:   "/uploads/first-hero.webp",
			ContentMarkdown: "## はじめに\n\n最初の公開記事の本文です。",
			CategoryID:      newsCatID,
			AuthorID:        studentA.ID,
			Status:          article.StatusPublished,
			PublishedAt:     &tOld,
		})
		newPostID = seedArticle(t, db, article.Article{
			Title:           "新しい公開記事",
			Slug:            "second-post",
			Excerpt:         "2件目の抜粋",
			FeaturedImage:   "/uploads/second-hero.webp",
			ContentMarkdown: "新しい記事の本文です。",
			CategoryID:      newsCatID,
			AuthorID:        studentA.ID,
			Status:          article.StatusPublished,
			PublishedAt:     &tNew,
		})

		// 非公開状態の記事（下書き・校閲待ち・予約投稿・ゴミ箱）
		seedArticle(t, db, article.Article{
			Title:           "SECRET_DRAFT_POST",
			Slug:            "secret-draft-post",
			ContentMarkdown: "SECRET_DRAFT_BODY",
			CategoryID:      emptyCatID,
			AuthorID:        studentA.ID,
			Status:          article.StatusDraft,
		})
		seedArticle(t, db, article.Article{
			Title:           "SECRET_REVIEW_POST",
			Slug:            "secret-review-post",
			ContentMarkdown: "SECRET_REVIEW_BODY",
			CategoryID:      newsCatID,
			AuthorID:        studentA.ID,
			Status:          article.StatusReview,
		})
		seedArticle(t, db, article.Article{
			Title:           "SECRET_SCHEDULED_POST",
			Slug:            "secret-scheduled-post",
			ContentMarkdown: "SECRET_SCHEDULED_BODY",
			CategoryID:      newsCatID,
			AuthorID:        studentA.ID,
			Status:          article.StatusPublished,
			PublishedAt:     &tFuture,
		})
		trashedPostID := seedArticle(t, db, article.Article{
			Title:           "SECRET_TRASHED_POST",
			Slug:            "secret-trashed-post",
			ContentMarkdown: "SECRET_TRASHED_BODY",
			CategoryID:      newsCatID,
			AuthorID:        studentA.ID,
			Status:          article.StatusPublished,
			PublishedAt:     &tOld,
		})
		if _, err := db.Exec(`UPDATE articles SET status = ? WHERE id = ?`, article.StatusDeleted, trashedPostID); err != nil {
			t.Fatal(err)
		}

		// 公開記事に内部コメントを付与（JSON に絶対に出力されないこと）
		if _, err := db.Exec(`INSERT INTO article_comments (id, article_id, user_id, content) VALUES ('c1', ?, ?, 'SECRET_INTERNAL_COMMENT')`,
			newPostID, studentA.ID); err != nil {
			t.Fatal(err)
		}

		// 固定ページ（公開2件 + 下書き・校閲待ち・予約投稿・ゴミ箱）
		aboutID = seedPage(t, db, article.Article{
			Title:           "研究室について",
			Slug:            "about",
			Excerpt:         "研究室の概要",
			FeaturedImage:   "/uploads/about-hero.webp",
			ContentMarkdown: "研究室の紹介文です。",
			SortOrder:       1,
			Status:          article.StatusPublished,
			PublishedAt:     &tOld,
		})
		contactID = seedPage(t, db, article.Article{
			Title:           "お問い合わせ",
			Slug:            "contact",
			Excerpt:         "連絡先",
			ContentMarkdown: "お問い合わせフォームです。",
			SortOrder:       2,
			Status:          article.StatusPublished,
			PublishedAt:     &tNew,
		})
		seedPage(t, db, article.Article{
			Title:           "SECRET_DRAFT_PAGE",
			Slug:            "secret-draft-page",
			ContentMarkdown: "SECRET_DRAFT_PAGE_BODY",
			Status:          article.StatusDraft,
		})
		seedPage(t, db, article.Article{
			Title:           "SECRET_REVIEW_PAGE",
			Slug:            "secret-review-page",
			ContentMarkdown: "SECRET_REVIEW_PAGE_BODY",
			Status:          article.StatusReview,
		})
		seedPage(t, db, article.Article{
			Title:           "SECRET_SCHEDULED_PAGE",
			Slug:            "secret-scheduled-page",
			ContentMarkdown: "SECRET_SCHEDULED_PAGE_BODY",
			Status:          article.StatusPublished,
			PublishedAt:     &tFuture,
		})
		trashedPageID := seedPage(t, db, article.Article{
			Title:           "SECRET_TRASHED_PAGE",
			Slug:            "secret-trashed-page",
			ContentMarkdown: "SECRET_TRASHED_PAGE_BODY",
			Status:          article.StatusPublished,
			PublishedAt:     &tOld,
		})
		if _, err := db.Exec(`UPDATE articles SET status = ? WHERE id = ?`, article.StatusDeleted, trashedPageID); err != nil {
			t.Fatal(err)
		}
	})

	env.setTheme("tech")
	writeTestFile(t, filepath.Join(env.uploadDir, "first-hero.webp"), "RIFFfirstWEBP")
	writeTestFile(t, filepath.Join(env.uploadDir, "second-hero.webp"), "RIFFsecondWEBP")
	writeTestFile(t, filepath.Join(env.uploadDir, "about-hero.webp"), "RIFFaboutWEBP")

	st := SiteStructure{
		FrontPage:     "about",
		FrontSections: []FrontSection{{Type: FrontSectionCards, Pages: []string{"contact", "secret-draft-page"}}},
		Menu: []MenuItem{
			{Type: MenuItemPage, Target: "about", Label: "概要", LabelEn: "About"},
			{
				Type:  MenuItemGroup,
				Label: "案内",
				Children: []MenuItem{
					{Type: MenuItemPage, Target: "contact"},
					{Type: MenuItemPage, Target: "secret-draft-page", Label: "非公開ページ"},
					{Type: MenuItemCategory, Target: "news"},
				},
			},
			{Type: MenuItemArticles, Label: "ニュース", LabelEn: "News"},
			{
				Type:   MenuItemPage,
				Target: "secret-draft-page",
				Label:  "下書き親メニュー",
				Children: []MenuItem{
					{Type: MenuItemPage, Target: "contact"},
				},
			},
		},
		FooterMenu: []MenuItem{
			{
				Type:  MenuItemGroup,
				Label: "サイトマップ",
				Children: []MenuItem{
					{Type: MenuItemPage, Target: "contact", Label: "連絡先"},
					{Type: MenuItemURL, Target: "https://example.org", Label: "外部リンク"},
				},
			},
			{
				Type:   MenuItemCategory,
				Target: "nonexistent-category",
				Label:  "未作成カテゴリ見出し",
				Children: []MenuItem{
					{Type: MenuItemPage, Target: "contact", Label: "連絡先"},
				},
			},
		},
	}
	if err := env.server.saveSiteStructure(context.Background(), st); err != nil {
		t.Fatalf("saveSiteStructure: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if n := len(filesIn(t, outDir)); n != rep.FilesCount+rep.PagesCount {
		t.Errorf("exported file count = %d, report says %d files + %d pages", n, rep.FilesCount, rep.PagesCount)
	}

	// 1. api-data/site.json の検証
	siteBytes, err := os.ReadFile(filepath.Join(outDir, "api-data", "site.json"))
	if err != nil {
		t.Fatalf("read api-data/site.json: %v", err)
	}
	var site headlessSiteData
	if err := json.Unmarshal(siteBytes, &site); err != nil {
		t.Fatalf("unmarshal site.json: %v", err)
	}
	if site.SiteName != "Headless テストサイト" || site.SiteDescription != "Headless JSON 出力の検証" {
		t.Errorf("site info = %+v", site)
	}
	if site.BaseURL != testBaseURL || site.Lang != "ja" || site.Theme != "tech" {
		t.Errorf("site base/lang/theme = %q / %q / %q", site.BaseURL, site.Lang, site.Theme)
	}
	if site.FrontPage != "about" {
		t.Errorf("front_page = %q, want about", site.FrontPage)
	}
	if len(site.FrontSections) != 1 || !slices.Equal(site.FrontSections[0].Pages, []string{"contact"}) {
		t.Errorf("front_sections = %v, want cards section with [contact] (draft excluded)", site.FrontSections)
	}
	if len(site.Categories) != 1 || site.Categories[0].Slug != "news" || site.Categories[0].Name != "お知らせ" {
		t.Errorf("categories = %+v, want only news", site.Categories)
	}
	if len(site.SiteMenu) != 4 {
		t.Fatalf("site_menu len = %d, want 4: %+v", len(site.SiteMenu), site.SiteMenu)
	}
	if site.SiteMenu[0].Label != "概要" || site.SiteMenu[0].URL != "/" || site.SiteMenu[0].Target != "about" {
		t.Errorf("site_menu[0] = %+v", site.SiteMenu[0])
	}
	if len(site.SiteMenu[1].Children) != 2 || site.SiteMenu[1].Children[0].URL != "/contact" || site.SiteMenu[1].Children[1].URL != "/categories/news" {
		t.Errorf("site_menu[1].Children = %+v (draft child should be omitted)", site.SiteMenu[1].Children)
	}
	if site.SiteMenu[2].Label != "ニュース" || site.SiteMenu[2].URL != "/articles" {
		t.Errorf("site_menu[2] = %+v", site.SiteMenu[2])
	}
	if site.SiteMenu[3].Label != "下書き親メニュー" || site.SiteMenu[3].Type != MenuItemGroup || site.SiteMenu[3].Target != "" || site.SiteMenu[3].URL != "" ||
		len(site.SiteMenu[3].Children) != 1 || site.SiteMenu[3].Children[0].Target != "contact" || site.SiteMenu[3].Children[0].URL != "/contact" {
		t.Errorf("site_menu[3] (draft parent with published child) = %+v", site.SiteMenu[3])
	}
	if len(site.SiteFooterMenu) != 2 || len(site.SiteFooterMenu[0].Children) != 2 {
		t.Fatalf("site_footer_menu = %+v", site.SiteFooterMenu)
	}
	if site.SiteFooterMenu[1].Label != "未作成カテゴリ見出し" || site.SiteFooterMenu[1].Type != MenuItemGroup || site.SiteFooterMenu[1].Target != "" || site.SiteFooterMenu[1].URL != "" ||
		len(site.SiteFooterMenu[1].Children) != 1 || site.SiteFooterMenu[1].Children[0].Target != "contact" || site.SiteFooterMenu[1].Children[0].URL != "/contact" {
		t.Errorf("site_footer_menu[1] (missing category parent with published child) = %+v", site.SiteFooterMenu[1])
	}

	// 2. api-data/pages.json の検証
	pagesBytes, err := os.ReadFile(filepath.Join(outDir, "api-data", "pages.json"))
	if err != nil {
		t.Fatalf("read api-data/pages.json: %v", err)
	}
	var pages []headlessPage
	if err := json.Unmarshal(pagesBytes, &pages); err != nil {
		t.Fatalf("unmarshal pages.json: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages len = %d, want 2: %+v", len(pages), pages)
	}
	if pages[0].ID != aboutID || pages[0].Slug != "about" || pages[0].URL != "/" || pages[0].SortOrder != 1 ||
		pages[0].HeroImage != "/uploads/about-hero.webp" || !strings.Contains(pages[0].HTML, "<p>研究室の紹介文です。</p>") {
		t.Errorf("pages[0] = %+v", pages[0])
	}
	if pages[1].ID != contactID || pages[1].Slug != "contact" || pages[1].URL != "/contact" || pages[1].SortOrder != 2 {
		t.Errorf("pages[1] = %+v", pages[1])
	}

	// 3. api-data/articles.json の検証（公開日時降順）
	articlesBytes, err := os.ReadFile(filepath.Join(outDir, "api-data", "articles.json"))
	if err != nil {
		t.Fatalf("read api-data/articles.json: %v", err)
	}
	var articles []headlessArticle
	if err := json.Unmarshal(articlesBytes, &articles); err != nil {
		t.Fatalf("unmarshal articles.json: %v", err)
	}
	if len(articles) != 2 {
		t.Fatalf("articles len = %d, want 2: %+v", len(articles), articles)
	}
	if articles[0].ID != newPostID || articles[0].Slug != "second-post" || articles[0].URL != "/articles/second-post" ||
		articles[0].Category.Slug != "news" || articles[0].Category.Name != "お知らせ" ||
		articles[0].HeroImage != "/uploads/second-hero.webp" || articles[0].Tags == nil {
		t.Errorf("articles[0] = %+v", articles[0])
	}
	if articles[1].ID != oldPostID || articles[1].Slug != "first-post" || articles[1].URL != "/articles/first-post" ||
		!strings.Contains(articles[1].HTML, "最初の公開記事の本文です。") || !strings.Contains(articles[1].Markdown, "## はじめに") {
		t.Errorf("articles[1] = %+v", articles[1])
	}
	for i, a := range articles {
		if _, err := time.Parse(time.RFC3339, a.PublishedAt); err != nil {
			t.Errorf("articles[%d].PublishedAt = %q: %v", i, a.PublishedAt, err)
		}
		if _, err := time.Parse(time.RFC3339, a.UpdatedAt); err != nil {
			t.Errorf("articles[%d].UpdatedAt = %q: %v", i, a.UpdatedAt, err)
		}
	}

	// 4. プライバシー境界の検証（非公開コンテンツ・内部コメント・著者情報が一切含まれないこと）
	combined := string(siteBytes) + "\n" + string(pagesBytes) + "\n" + string(articlesBytes)
	for _, forbidden := range []string{
		"SECRET_DRAFT",
		"SECRET_REVIEW",
		"SECRET_SCHEDULED",
		"SECRET_TRASHED",
		"SECRET_INTERNAL_COMMENT",
		"secret-draft",
		"secret-review",
		"secret-scheduled",
		"secret-trashed",
		"draft-only-cat",
		"nonexistent-category",
		studentA.ID,
		studentA.Username,
		studentA.DisplayName,
	} {
		if forbidden != "" && strings.Contains(combined, forbidden) {
			t.Errorf("api-data JSON leaked forbidden string %q", forbidden)
		}
	}

	// _headers に /api-data/*.json の Content-Type が含まれること
	headersBytes, err := os.ReadFile(filepath.Join(outDir, "_headers"))
	if err != nil {
		t.Fatalf("read _headers: %v", err)
	}
	if !strings.Contains(string(headersBytes), "/api-data/*.json") || !strings.Contains(string(headersBytes), "Content-Type: application/json") {
		t.Errorf("_headers missing /api-data/*.json Content-Type: %s", headersBytes)
	}
}

func TestHeadlessExport_PageHierarchyAndAttachmentLinkRewrite(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	setClock(t, now)
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL, SiteName: "階層と添付のテスト"}, nil)
	past := now.Add(-time.Hour)

	parentID := seedPage(t, env.db, article.Article{
		Title:           "研究紹介",
		Slug:            "research",
		ContentMarkdown: "親ページの本文 [要覧PDF](/attachments/att-page/download)",
		SortOrder:       1,
		Status:          article.StatusPublished,
		PublishedAt:     &past,
	})
	child2ID := seedPage(t, env.db, article.Article{
		Title:           "テーマB",
		Slug:            "theme-b",
		ParentID:        parentID,
		SortOrder:       20,
		ContentMarkdown: "テーマBの本文",
		Status:          article.StatusPublished,
		PublishedAt:     &past,
	})
	child1ID := seedPage(t, env.db, article.Article{
		Title:           "テーマA",
		Slug:            "theme-a",
		ParentID:        parentID,
		SortOrder:       10,
		ContentMarkdown: "テーマAの本文",
		Status:          article.StatusPublished,
		PublishedAt:     &past,
	})
	draftParentID := seedPage(t, env.db, article.Article{
		Title:           "下書きの親",
		Slug:            "secret-parent",
		ContentMarkdown: "下書き",
		SortOrder:       0,
		Status:          article.StatusDraft,
	})
	orphanChildID := seedPage(t, env.db, article.Article{
		Title:           "親が下書きの子ページ",
		Slug:            "visible-child",
		ParentID:        draftParentID,
		SortOrder:       30,
		ContentMarkdown: "公開されている子ページ",
		Status:          article.StatusPublished,
		PublishedAt:     &past,
	})

	postID := seedArticle(t, env.db, article.Article{
		Title:           "資料付き記事",
		Slug:            "post-with-att",
		ContentMarkdown: "記事の資料: [配布PDF](/attachments/att-post/download) と [下書き資料](/attachments/att-draft/download)",
		Status:          article.StatusPublished,
		PublishedAt:     &past,
	})
	draftPostID := seedArticle(t, env.db, article.Article{
		Title:           "下書き記事",
		Slug:            "draft-post",
		ContentMarkdown: "下書き",
		Status:          article.StatusDraft,
	})

	seedAttachmentRow(t, env, parentID, "att-page", "brochure.pdf", []byte("PUBLIC brochure"))
	seedAttachmentRow(t, env, postID, "att-post", "handout.pdf", []byte("PUBLIC handout"))
	seedAttachmentRow(t, env, draftPostID, "att-draft", "secret.pdf", []byte("SECRET draft pdf"))

	outDir := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	pagesBytes, err := os.ReadFile(filepath.Join(outDir, "api-data", "pages.json"))
	if err != nil {
		t.Fatalf("read pages.json: %v", err)
	}
	var pages []headlessPage
	if err := json.Unmarshal(pagesBytes, &pages); err != nil {
		t.Fatalf("unmarshal pages.json: %v", err)
	}

	// sort_order ASC, title ASC の順: research(1) -> theme-a(10) -> theme-b(20) -> visible-child(30)
	wantOrder := []string{parentID, child1ID, child2ID, orphanChildID}
	var gotOrder []string
	for _, p := range pages {
		gotOrder = append(gotOrder, p.ID)
	}
	if !slices.Equal(gotOrder, wantOrder) {
		t.Fatalf("pages order = %v, want %v", gotOrder, wantOrder)
	}

	if pages[0].ParentID != "" || pages[0].ParentSlug != "" {
		t.Errorf("root page parent = (%q, %q), want empty", pages[0].ParentID, pages[0].ParentSlug)
	}
	if !strings.Contains(pages[0].HTML, `href="/attachments/brochure.pdf"`) || strings.Contains(pages[0].HTML, "/attachments/att-page/download") {
		t.Errorf("pages[0].HTML attachment link not rewritten: %q", pages[0].HTML)
	}
	if pages[1].ParentID != parentID || pages[1].ParentSlug != "research" || pages[1].SortOrder != 10 {
		t.Errorf("pages[1] hierarchy = %+v", pages[1])
	}
	if pages[2].ParentID != parentID || pages[2].ParentSlug != "research" || pages[2].SortOrder != 20 {
		t.Errorf("pages[2] hierarchy = %+v", pages[2])
	}
	// 親が下書きの場合は、下書きの親 ID・スラッグを漏らさず最上位として出力する
	if pages[3].ParentID != "" || pages[3].ParentSlug != "" || strings.Contains(string(pagesBytes), "secret-parent") {
		t.Errorf("orphan child leaked draft parent: %+v", pages[3])
	}

	articlesBytes, err := os.ReadFile(filepath.Join(outDir, "api-data", "articles.json"))
	if err != nil {
		t.Fatalf("read articles.json: %v", err)
	}
	var articles []headlessArticle
	if err := json.Unmarshal(articlesBytes, &articles); err != nil {
		t.Fatalf("unmarshal articles.json: %v", err)
	}
	if len(articles) != 1 {
		t.Fatalf("articles len = %d, want 1", len(articles))
	}
	if !strings.Contains(articles[0].HTML, `href="/attachments/handout.pdf"`) || strings.Contains(articles[0].HTML, "/attachments/att-post/download") {
		t.Errorf("articles[0].HTML attachment link not rewritten: %q", articles[0].HTML)
	}
	// 下書きの添付資料リンクは書き換えない
	if !strings.Contains(articles[0].HTML, `href="/attachments/att-draft/download"`) {
		t.Errorf("articles[0].HTML unexpectedly rewrote draft attachment link: %q", articles[0].HTML)
	}
}

func TestHeadlessExport_ReservedSlugAPIData(t *testing.T) {
	for _, slug := range []string{"api-data", "API-DATA", " api-data "} {
		if !article.IsReservedPageSlug(slug) {
			t.Errorf("IsReservedPageSlug(%q) = false, want true", slug)
		}
	}

	env := newTestEnv(t)
	for _, slug := range []string{"api-data", "API-DATA"} {
		page := &article.Article{
			Title:      "API データ",
			Slug:       slug,
			Kind:       article.KindPage,
			AuthorID:   studentA.ID,
			CategoryID: 1,
		}
		err := env.server.articles.Save(page)
		if !errors.Is(err, article.ErrReservedSlug) || !errors.Is(err, article.ErrInvalid) {
			t.Errorf("Save(page slug %q) = %v, want ErrReservedSlug and ErrInvalid", slug, err)
		}
	}

	// タイトルから自動生成されるスラッグが api-data になる固定ページも拒否する
	titled := &article.Article{
		Title:      "API Data",
		Kind:       article.KindPage,
		AuthorID:   studentA.ID,
		CategoryID: 1,
	}
	if err := env.server.articles.Save(titled); !errors.Is(err, article.ErrReservedSlug) {
		t.Errorf("Save(page titled 'API Data') = %v, want ErrReservedSlug", err)
	}

	// 通常記事（post）は /articles/api-data で配信されるため保存可能
	post := &article.Article{
		Title:      "API データの記事",
		Slug:       "api-data",
		Kind:       article.KindPost,
		AuthorID:   studentA.ID,
		CategoryID: 1,
	}
	if err := env.server.articles.Save(post); err != nil {
		t.Fatalf("Save(post slug 'api-data') failed: %v", err)
	}

	// DB に直接 api-data スラッグの固定ページが投入された場合でも Export が拒否すること
	past := time.Now().Add(-time.Hour).UTC()
	if _, err := env.db.Exec(`INSERT INTO articles (id, title, slug, content_markdown, status, kind, author_id, category_id, published_at)
		VALUES ('bad-page', '予約語ページ', 'api-data-temp', '本文', ?, ?, ?, 1, ?)`,
		article.StatusPublished, article.KindPage, studentA.ID, past); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`DELETE FROM articles WHERE id = ?`, post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE articles SET slug = 'api-data' WHERE id = 'bad-page'`); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "dist")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("Export with reserved page slug 'api-data' = %v, want reserved slug error", err)
	}
}
