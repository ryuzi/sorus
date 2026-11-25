package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

func TestSettingsGeneral_SiteNameEn(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	// 1. 初期表示で入力欄が存在すること
	res := adm.get("/admin/settings/general")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `name="site_name"`, `name="site_name_en"`, `maxlength="100"`, `name="show_author"`, "サイト名（英語）", "記事や一覧に執筆者名を表示する")

	// 1-b. バリデーション検証: 100文字超および制御文字の拒絶
	formTooLong := generalSettingsForm()
	formTooLong.Set("site_name_en", strings.Repeat("A", 101))
	resBad := adm.postForm("/admin/settings/general", formTooLong)
	expectStatus(t, resBad, http.StatusUnprocessableEntity)
	expectContains(t, resBad.Body, "サイト名（英語）は100文字以内で、改行や制御文字は使えません。")

	formControl := generalSettingsForm()
	formControl.Set("site_name_en", "Invalid\nName")
	resBadCtrl := adm.postForm("/admin/settings/general", formControl)
	expectStatus(t, resBadCtrl, http.StatusUnprocessableEntity)
	expectContains(t, resBadCtrl.Body, "サイト名（英語）は100文字以内で、改行や制御文字は使えません。")

	// 2. 英語サイト名と執筆者名表示フラグの保存
	form := generalSettingsForm()
	form.Set("site_name", "日本語ラボ")
	form.Set("site_name_en", "Japan Lab")
	form.Set("show_author", "1")
	res = adm.postForm("/admin/settings/general", form)
	loc := expectRedirect(t, res, "/admin/settings/general?flash=settings_saved")

	// 3. DBおよびメモリキャッシュの検証
	if got := env.setting("site_name"); got != "日本語ラボ" {
		t.Errorf("site_name = %q, want '日本語ラボ'", got)
	}
	if got := env.setting("site_name_en"); got != "Japan Lab" {
		t.Errorf("site_name_en = %q, want 'Japan Lab'", got)
	}
	if got := env.setting("show_author"); got != "1" {
		t.Errorf("show_author = %q, want '1'", got)
	}
	if !env.server.getShowAuthor() {
		t.Errorf("getShowAuthor() want true, got false")
	}
	if got := env.server.getSiteNameEn(); got != "Japan Lab" {
		t.Errorf("getSiteNameEn() = %q, want 'Japan Lab'", got)
	}
	if got := env.server.siteNameFor("en"); got != "Japan Lab" {
		t.Errorf("siteNameFor('en') = %q, want 'Japan Lab'", got)
	}
	if got := env.server.siteNameFor("ja"); got != "日本語ラボ" {
		t.Errorf("siteNameFor('ja') = %q, want '日本語ラボ'", got)
	}

	// 4. 再表示時に保存された英語サイト名がフォームに反映されていること
	res = adm.get(loc)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `value="Japan Lab"`)

	// 5. 英語サイト名が未設定のときは site_name にフォールバックすること
	form.Set("site_name_en", "")
	res = adm.postForm("/admin/settings/general", form)
	expectRedirect(t, res, "/admin/settings/general?flash=settings_saved")

	if got := env.server.getSiteNameEn(); got != "" {
		t.Errorf("getSiteNameEn() after clear = %q, want empty", got)
	}
	if got := env.server.siteNameFor("en"); got != "日本語ラボ" {
		t.Errorf("siteNameFor('en') fallback = %q, want '日本語ラボ'", got)
	}
}

func TestSettingsGeneral_SiteNameEn_ThreadSafety(t *testing.T) {
	env := newTestEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				env.server.setSiteNameEn("English Site")
			} else {
				env.server.setSiteInfo("Site JA", "Desc JA")
			}
		}(i)
		go func() {
			defer wg.Done()
			_ = env.server.getSiteNameEn()
			_ = env.server.siteNameFor("en")
			_ = env.server.siteNameFor("ja")
			_ = env.server.getSiteName()
		}()
	}
	wg.Wait()
}

func TestSiteStructure_SaveAndLoadMultilingualMenu(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	// 管理画面サイト構成のフォームから LabelEn を含むメニューを送信
	form := url.Values{
		"op":            {"save"},
		"menu_type":     {"url", "category", "articles"},
		"menu_url":      {"/", "", ""},
		"menu_category": {"", "news", ""},
		"menu_label":    {"ホーム", "お知らせ", "すべての記事"},
		"menu_label_en": {"Home", "News Feed", "All Articles"},
	}
	res := adm.postForm("/admin/settings/structure", form)
	expectRedirect(t, res, "/admin/settings/structure?flash=structure_saved")

	// DBから読み込んで検証
	st := env.server.loadSiteStructure(context.Background())
	if len(st.Menu) != 3 {
		t.Fatalf("menu count = %d, want 3", len(st.Menu))
	}
	if st.Menu[0].LabelEn != "Home" || st.Menu[0].Label != "ホーム" {
		t.Errorf("menu[0] = %+v", st.Menu[0])
	}
	if st.Menu[1].LabelEn != "News Feed" || st.Menu[1].Label != "お知らせ" {
		t.Errorf("menu[1] = %+v", st.Menu[1])
	}
	if st.Menu[2].LabelEn != "All Articles" || st.Menu[2].Label != "すべての記事" {
		t.Errorf("menu[2] = %+v", st.Menu[2])
	}

	// 編集画面に英語ラベルが表示されること
	res = adm.get("/admin/settings/structure")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `value="Home"`, `value="News Feed"`, `value="All Articles"`, "表示名（英語）")
}

func TestSiteStructure_MultilingualMenuResolution(t *testing.T) {
	published := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	env := newTestEnvWith(t, Config{
		SiteName: "日本語研究所",
	}, func(db *sqlx.DB) {
		catNews := seedCategory(t, db, "ニュース", "news", "")
		catCircle := seedCategory(t, db, "サークル", "circle", "")

		// 固定ページ: 日本語「研究概要」(slug: research) と英語「Research Overview」(slug: research-en)
		clusterID := "cluster-page-research"
		seedArticle(t, db, article.Article{
			ID:                 "page-ja",
			Title:              "研究概要",
			Slug:               "research",
			ContentMarkdown:    "# 概要",
			CategoryID:         catNews,
			Status:             article.StatusPublished,
			Kind:               article.KindPage,
			Lang:               "ja",
			TranslationGroupID: clusterID,
			PublishedAt:        &published,
		})
		seedArticle(t, db, article.Article{
			ID:                 "page-en",
			Title:              "Research Overview",
			Slug:               "research-en",
			ContentMarkdown:    "# Overview",
			CategoryID:         catNews,
			Status:             article.StatusPublished,
			Kind:               article.KindPage,
			Lang:               "en",
			TranslationGroupID: clusterID,
			PublishedAt:        &published,
		})

		// 固定ページ: 日本語のみ「アクセス」(slug: access)
		seedArticle(t, db, article.Article{
			ID:                 "page-access",
			Title:              "交通案内",
			Slug:               "access",
			ContentMarkdown:    "# アクセス",
			CategoryID:         catNews,
			Status:             article.StatusPublished,
			Kind:               article.KindPage,
			Lang:               "ja",
			TranslationGroupID: "cluster-access",
			PublishedAt:        &published,
		})
		_ = catCircle
	})

	env.server.setSiteNameEn("Japan Research Institute")

	st := SiteStructure{
		Menu: []MenuItem{
			// 1. 固定ページ（英語翻訳あり、LabelEn なし） -> 英語では英語ページのタイトルと英語スラッグを使用
			{Type: MenuItemPage, Target: "research", Label: ""},
			// 2. 固定ページ（英語翻訳あり、LabelEn あり） -> 英語では LabelEn と英語スラッグを使用
			{Type: MenuItemPage, Target: "research", Label: "研究", LabelEn: "Our Research"},
			// 3. 固定ページ（英語翻訳なし） -> 英語では日本語タイトルと /en/access を使用
			{Type: MenuItemPage, Target: "access", Label: ""},
			// 4. カテゴリ（既知標準カテゴリ news、LabelEn なし） -> 英語では "News" と /en/categories/news を使用
			{Type: MenuItemCategory, Target: "news", Label: ""},
			// 5. カテゴリ（LabelEn あり） -> 英語では LabelEn と /en/categories/circle を使用
			{Type: MenuItemCategory, Target: "circle", Label: "サークル活動", LabelEn: "Student Clubs"},
			// 6. 記事一覧（LabelEn なし） -> 英語では "Articles" と /en/articles を使用
			{Type: MenuItemArticles, Label: "", LabelEn: ""},
			// 7. URL（/ 始まり、LabelEn あり） -> 英語では LabelEn と /en/ を使用
			{Type: MenuItemURL, Target: "/", Label: "ホーム", LabelEn: "Home"},
		},
	}

	ctx := context.Background()

	// --- 日本語表示 (lang: "ja") ---
	jaMenu, err := env.server.publicMenu(ctx, st, "/research", "ja")
	if err != nil {
		t.Fatalf("publicMenu(ja): %v", err)
	}
	if len(jaMenu) != 7 {
		t.Fatalf("jaMenu len = %d, want 7", len(jaMenu))
	}
	// Item 0: 日本語タイトル「研究概要」、URL: "/research", Current: true
	if jaMenu[0].Label != "研究概要" || jaMenu[0].URL != "/research" || !jaMenu[0].Current {
		t.Errorf("jaMenu[0] = %+v", jaMenu[0])
	}
	// Item 1: Label「研究」
	if jaMenu[1].Label != "研究" || jaMenu[1].URL != "/research" {
		t.Errorf("jaMenu[1] = %+v", jaMenu[1])
	}
	// Item 2: access
	if jaMenu[2].Label != "交通案内" || jaMenu[2].URL != "/access" {
		t.Errorf("jaMenu[2] = %+v", jaMenu[2])
	}
	// Item 3: カテゴリ news
	if jaMenu[3].Label != "ニュース" || jaMenu[3].URL != "/categories/news" {
		t.Errorf("jaMenu[3] = %+v", jaMenu[3])
	}
	// Item 4: カテゴリ circle
	if jaMenu[4].Label != "サークル活動" || jaMenu[4].URL != "/categories/circle" {
		t.Errorf("jaMenu[4] = %+v", jaMenu[4])
	}
	// Item 5: articles
	if jaMenu[5].Label != "記事一覧" || jaMenu[5].URL != "/articles" {
		t.Errorf("jaMenu[5] = %+v", jaMenu[5])
	}
	// Item 6: URL /
	if jaMenu[6].Label != "ホーム" || jaMenu[6].URL != "/" {
		t.Errorf("jaMenu[6] = %+v", jaMenu[6])
	}

	// --- 英語表示 (lang: "en") ---
	enMenu, err := env.server.publicMenu(ctx, st, "/en/research-en", "en")
	if err != nil {
		t.Fatalf("publicMenu(en): %v", err)
	}
	if len(enMenu) != 7 {
		t.Fatalf("enMenu len = %d, want 7", len(enMenu))
	}
	// Item 0: 英語ページのタイトル「Research Overview」、URL: "/en/research-en", Current: true
	if enMenu[0].Label != "Research Overview" || enMenu[0].URL != "/en/research-en" || !enMenu[0].Current {
		t.Errorf("enMenu[0] = %+v, want Label='Research Overview', URL='/en/research-en', Current=true", enMenu[0])
	}
	// Item 1: LabelEn「Our Research」、URL: "/en/research-en"
	if enMenu[1].Label != "Our Research" || enMenu[1].URL != "/en/research-en" {
		t.Errorf("enMenu[1] = %+v, want Label='Our Research'", enMenu[1])
	}
	// Item 2: access (英語固定ページなし) -> 日本語タイトル、URL: "/en/access"
	if enMenu[2].Label != "交通案内" || enMenu[2].URL != "/en/access" {
		t.Errorf("enMenu[2] = %+v, want URL='/en/access'", enMenu[2])
	}
	// Item 3: カテゴリ news -> 英語名 "News"、URL: "/en/categories/news"
	if enMenu[3].Label != "News" || enMenu[3].URL != "/en/categories/news" {
		t.Errorf("enMenu[3] = %+v, want Label='News', URL='/en/categories/news'", enMenu[3])
	}
	// Item 4: カテゴリ circle (LabelEn: "Student Clubs") -> URL: "/en/categories/circle"
	if enMenu[4].Label != "Student Clubs" || enMenu[4].URL != "/en/categories/circle" {
		t.Errorf("enMenu[4] = %+v, want Label='Student Clubs', URL='/en/categories/circle'", enMenu[4])
	}
	// Item 5: articles -> "Articles", URL: "/en/articles"
	if enMenu[5].Label != "Articles" || enMenu[5].URL != "/en/articles" {
		t.Errorf("enMenu[5] = %+v, want Label='Articles', URL='/en/articles'", enMenu[5])
	}
	// Item 6: URL / -> LabelEn: "Home", URL: "/en/"
	if enMenu[6].Label != "Home" || enMenu[6].URL != "/en/" {
		t.Errorf("enMenu[6] = %+v, want Label='Home', URL='/en/'", enMenu[6])
	}
}

func TestSiteStructure_MenuItemURL_RewriteScope(t *testing.T) {
	env := newTestEnv(t)
	st := SiteStructure{
		Menu: []MenuItem{
			{Type: MenuItemURL, Target: "/", Label: "ホーム", LabelEn: "Home"},
			{Type: MenuItemURL, Target: "/articles", Label: "記事一覧", LabelEn: "Articles"},
			{Type: MenuItemURL, Target: "/categories/news", Label: "ニュース", LabelEn: "News"},
			{Type: MenuItemURL, Target: "/uploads/spec.pdf", Label: "資料", LabelEn: "Document"},
			{Type: MenuItemURL, Target: "/feed.xml", Label: "フィード", LabelEn: "Feed"},
			{Type: MenuItemURL, Target: "/static/style.css", Label: "スタイル", LabelEn: "Style"},
			{Type: MenuItemURL, Target: "https://example.com/ext", Label: "外部", LabelEn: "External"},
		},
	}
	ctx := context.Background()

	// 英語モード (lang: "en")
	enMenu, err := env.server.publicMenu(ctx, st, "/en/", "en")
	if err != nil {
		t.Fatalf("publicMenu(en): %v", err)
	}
	if len(enMenu) != 7 {
		t.Fatalf("enMenu len = %d, want 7", len(enMenu))
	}
	// 既知の内部コンテンツパスは /en/... に書き換わる
	if enMenu[0].URL != "/en/" {
		t.Errorf("enMenu[0].URL = %q, want '/en/'", enMenu[0].URL)
	}
	if enMenu[1].URL != "/en/articles" {
		t.Errorf("enMenu[1].URL = %q, want '/en/articles'", enMenu[1].URL)
	}
	if enMenu[2].URL != "/en/categories/news" {
		t.Errorf("enMenu[2].URL = %q, want '/en/categories/news'", enMenu[2].URL)
	}
	// 静的アセット・システム・外部URLは書き換わらない！
	if enMenu[3].URL != "/uploads/spec.pdf" {
		t.Errorf("enMenu[3].URL = %q, want '/uploads/spec.pdf'", enMenu[3].URL)
	}
	if enMenu[4].URL != "/feed.xml" {
		t.Errorf("enMenu[4].URL = %q, want '/feed.xml'", enMenu[4].URL)
	}
	if enMenu[5].URL != "/static/style.css" {
		t.Errorf("enMenu[5].URL = %q, want '/static/style.css'", enMenu[5].URL)
	}
	if enMenu[6].URL != "https://example.com/ext" {
		t.Errorf("enMenu[6].URL = %q, want 'https://example.com/ext'", enMenu[6].URL)
	}
}

func TestMultilingual_PublicSiteNameAndNavRendering(t *testing.T) {
	published := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	env := newTestEnvWith(t, Config{
		SiteName: "先端メディア研究室",
	}, func(db *sqlx.DB) {
		catNews := seedCategory(t, db, "ニュース", "news", "")
		db.MustExec(`INSERT INTO settings (key, value) VALUES ('site_name_en', 'Advanced Media Lab')`)

		// メニュー設定: 英語ラベル付き
		menuJSON := `[{"label":"ホーム","label_en":"Home","type":"url","target":"/"},{"label":"お知らせ","label_en":"News","type":"category","target":"news"},{"label":"記事","label_en":"Articles","type":"articles"}]`
		db.MustExec(`INSERT INTO settings (key, value) VALUES ('site_menu', ?)`, menuJSON)

		// 記事作成
		seedArticle(t, db, article.Article{
			ID:                 "a-ja",
			Title:              "新着お知らせ",
			Slug:               "news-ja",
			ContentMarkdown:    "# 本文",
			CategoryID:         catNews,
			Status:             article.StatusPublished,
			Kind:               article.KindPost,
			Lang:               "ja",
			TranslationGroupID: "grp-1",
			PublishedAt:        &published,
		})
		seedArticle(t, db, article.Article{
			ID:                 "a-en",
			Title:              "Latest News",
			Slug:               "news-en",
			ContentMarkdown:    "# Content",
			CategoryID:         catNews,
			Status:             article.StatusPublished,
			Kind:               article.KindPost,
			Lang:               "en",
			TranslationGroupID: "grp-1",
			PublishedAt:        &published,
		})
	})

	c := env.newClient()

	// 1. 日本語トップページ
	resJA := c.get("/")
	expectStatus(t, resJA, http.StatusOK)
	expectContains(t, resJA.Body, "先端メディア研究室", "ホーム", "お知らせ")
	if strings.Contains(resJA.Body, "Advanced Media Lab") {
		t.Errorf("JA page contains English site name: %s", resJA.Body)
	}

	// 2. 英語トップページ
	resEN := c.get("/en/")
	expectStatus(t, resEN, http.StatusOK)
	expectContains(t, resEN.Body,
		"Advanced Media Lab",
		`href="/en/"`,
		`Home`,
		`href="/en/categories/news"`,
		`News`,
		`href="/en/articles"`,
		`Articles`,
	)
	if !strings.Contains(resEN.Body, `<a href="/en/" class="site-title">Advanced Media Lab</a>`) {
		t.Errorf("EN header missing English site-title: %s", resEN.Body)
	}

	// 3. 英語記事一覧
	resArticlesEN := c.get("/en/articles")
	expectStatus(t, resArticlesEN, http.StatusOK)
	expectContains(t, resArticlesEN.Body, "Advanced Media Lab", "Articles | Advanced Media Lab")
}
