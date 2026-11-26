package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// TestMultilingual_AISettings はAI設定画面でのDeepLおよび翻訳専用設定の保存・読み込みを検証する。
func TestMultilingual_AISettings(t *testing.T) {
	clearAIEnv(t)
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	// 1. 設定保存
	form := url.Values{
		"provider":             {"gemini"},
		"api_key_gemini":       {"AQ.Ab8RN6-test"},
		"translation_provider": {"deepl"},
		"api_key_deepl":        {"test-deepl-key:fx"},
		"glossary":             {"sorus=sorus CMS\nGo=Golang"},
		"writing_style":        {"academic"},
	}
	res := adm.postForm("/admin/settings/ai", form)
	if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusFound && res.StatusCode != http.StatusOK {
		t.Fatalf("POST /admin/settings/ai returned %d (body: %s)", res.StatusCode, res.Body)
	}

	// 2. 設定読み込み確認
	saved, ok := env.server.loadAIConfig()
	if !ok {
		t.Fatalf("loadAIConfig returned false")
	}
	if saved.TranslationProvider != "deepl" {
		t.Errorf("saved.TranslationProvider = %q, want 'deepl'", saved.TranslationProvider)
	}
	if saved.Providers["deepl"].APIKey != "test-deepl-key:fx" {
		t.Errorf("saved.Providers[deepl].APIKey = %q, want 'test-deepl-key:fx'", saved.Providers["deepl"].APIKey)
	}
	if !strings.Contains(saved.Glossary, "sorus=sorus CMS") {
		t.Errorf("saved.Glossary missing term: %q", saved.Glossary)
	}
	if saved.WritingStyle != "academic" {
		t.Errorf("saved.WritingStyle = %q, want 'academic'", saved.WritingStyle)
	}

	// 3. AI接続テスト（APIキー未設定エラー）
	testForm := url.Values{
		"provider": {"openai"},
	}
	testRes := adm.postForm("/admin/settings/ai/test", testForm)
	if testRes.StatusCode != http.StatusOK {
		t.Fatalf("POST /admin/settings/ai/test returned %d", testRes.StatusCode)
	}
	if !strings.Contains(testRes.Body, "APIキー") {
		t.Errorf("expected API key error message, got %s", testRes.Body)
	}
}

// TestMultilingual_ArticleList_FilteringAndBadges は記事一覧の言語タブとバッジ表示を検証する。
func TestMultilingual_ArticleList_FilteringAndBadges(t *testing.T) {
	env := newTestEnvWith(t, Config{
		BaseURL:  "http://example.com",
		SiteName: "Multilingual CMS",
	}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "一般", "general", "一般カテゴリ")

		// 記事1: JA（EN翻訳あり）
		seedArticle(t, db, article.Article{
			ID:                 "art-ja-1",
			Title:              "日本語記事1",
			Slug:               "ja-article-1",
			CategoryID:         catID,
			Excerpt:            "抜粋1",
			ContentMarkdown:    "本文1",
			Status:             article.StatusDraft,
			Lang:               "ja",
			TranslationGroupID: "group-1",
		})
		// 記事1のEN翻訳（要更新フラグ付き）
		seedArticle(t, db, article.Article{
			ID:                 "art-en-1",
			Title:              "English Article 1",
			Slug:               "en-article-1",
			CategoryID:         catID,
			Excerpt:            "Excerpt 1",
			ContentMarkdown:    "Content 1",
			Status:             article.StatusDraft,
			Lang:               "en",
			TranslationGroupID: "group-1",
			IsOutdated:         true,
		})
		// 英語単独記事は未指定の一覧でも独立行として表示する。
		seedArticle(t, db, article.Article{
			ID: "art-en-only", Title: "English Only Article", Slug: "en-only", CategoryID: catID,
			ContentMarkdown: "English only", Status: article.StatusDraft, Lang: "en", TranslationGroupID: "group-en-only",
		})
		// 記事2: JA（EN未作成）
		seedArticle(t, db, article.Article{
			ID:                 "art-ja-2",
			Title:              "日本語記事2未翻訳",
			Slug:               "ja-article-2",
			CategoryID:         catID,
			Excerpt:            "抜粋2",
			ContentMarkdown:    "本文2",
			Status:             article.StatusDraft,
			Lang:               "ja",
			TranslationGroupID: "group-2",
		})
	})

	ed := env.loggedIn(chief)

	// 1. 全言語一覧
	resAll := ed.get("/admin/articles")
	if resAll.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/articles returned %d", resAll.StatusCode)
	}
	bodyAll := resAll.Body
	if !strings.Contains(bodyAll, "日本語記事1") || strings.Contains(bodyAll, "English Article 1") || !strings.Contains(bodyAll, "日本語記事2未翻訳") || !strings.Contains(bodyAll, "English Only Article") {
		t.Errorf("unfiltered view should show Japanese parents and standalone English articles, but suppress translated English rows")
	}
	// バッジの確認
	if !strings.Contains(bodyAll, "/admin/articles/art-en-1/edit") {
		t.Errorf("expected EN edit link for art-ja-1, not found in:\n%s", bodyAll)
	}
	if !strings.Contains(bodyAll, "/admin/articles/new?source_article_id=art-ja-2&amp;lang=en") &&
		!strings.Contains(bodyAll, "/admin/articles/new?source_article_id=art-ja-2&lang=en") {
		t.Errorf("expected '+ EN' link for art-ja-2, not found in:\n%s", bodyAll)
	}
	if !strings.Contains(bodyAll, "要更新") {
		t.Errorf("expected '要更新' badge on art-en-1, not found")
	}

	// 2. 日本語のみフィルタ (?lang=ja)
	resJA := ed.get("/admin/articles?lang=ja")
	if resJA.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/articles?lang=ja returned %d", resJA.StatusCode)
	}
	bodyJA := resJA.Body
	if !strings.Contains(bodyJA, "日本語記事1") || !strings.Contains(bodyJA, "日本語記事2未翻訳") {
		t.Errorf("JA articles must be visible in ?lang=ja")
	}
	if strings.Contains(bodyJA, "English Article 1") {
		t.Errorf("EN article must NOT be visible in ?lang=ja")
	}

	// 3. 英語のみフィルタ (?lang=en)
	resEN := ed.get("/admin/articles?lang=en")
	if resEN.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/articles?lang=en returned %d", resEN.StatusCode)
	}
	bodyEN := resEN.Body
	if !strings.Contains(bodyEN, "English Article 1") {
		t.Errorf("EN article must be visible in ?lang=en")
	}
	if !strings.Contains(bodyEN, "English Only Article") {
		t.Errorf("standalone EN article must be visible in ?lang=en")
	}
	if strings.Contains(bodyEN, "日本語記事1") || strings.Contains(bodyEN, "日本語記事2未翻訳") {
		t.Errorf("JA articles must NOT be visible in ?lang=en")
	}
}

// TestMultilingual_TranslateEndpoint は /admin/articles/translate API の動作を検証する。
func TestMultilingual_TranslateEndpoint(t *testing.T) {
	clearAIEnv(t)
	env := newTestEnvWith(t, Config{
		BaseURL: "http://example.com",
	}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "一般", "general", "一般")
		seedArticle(t, db, article.Article{
			ID:                 "art-source-1",
			Title:              "テスト記事タイトル",
			Slug:               "test-source",
			CategoryID:         catID,
			Excerpt:            "テストの抜粋",
			ContentMarkdown:    "テスト本文です。",
			Status:             article.StatusDraft,
			Lang:               "ja",
			TranslationGroupID: "grp-test",
		})
	})

	ed := env.loggedIn(chief)

	// 1. source_article_id 未指定 -> 400 Bad Request
	resNoID := ed.postForm("/admin/articles/translate", url.Values{
		"target_lang": {"en"},
	})
	if resNoID.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing source_article_id, got %d", resNoID.StatusCode)
	}

	// 2. 存在しない記事ID -> 404 Not Found
	resNotFound := ed.postForm("/admin/articles/translate", url.Values{
		"source_article_id": {"non-existent-id"},
		"target_lang":       {"en"},
	})
	if resNotFound.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent source_article_id, got %d", resNotFound.StatusCode)
	}

	// 3. 存在する記事でAIキー未設定 -> 400 Bad Request (APIキー未設定エラー)
	resValid := ed.postForm("/admin/articles/translate", url.Values{
		"source_article_id": {"art-source-1"},
		"target_lang":       {"en"},
	})
	if resValid.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 when AI key is missing, got %d", resValid.StatusCode)
	}
	if !strings.Contains(resValid.Body, "APIキー") {
		t.Errorf("expected API key error message, got %s", resValid.Body)
	}
}

// TestMultilingual_NewTranslationWorkflow は新規翻訳画面の初期値とスプリットビュー構成を検証する。
func TestMultilingual_NewTranslationWorkflow(t *testing.T) {
	env := newTestEnvWith(t, Config{
		BaseURL: "http://example.com",
	}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "テクノロジー", "tech", "技術情報")
		seedArticle(t, db, article.Article{
			ID:                 "art-ja-origin",
			Title:              "日本の先端科学",
			Slug:               "japan-science",
			CategoryID:         catID,
			Excerpt:            "最先端研究のレポート",
			ContentMarkdown:    "## はじめに\n\n日本の科学技術について。",
			Status:             article.StatusPublished,
			Lang:               "ja",
			TranslationGroupID: "grp-science",
		})
	})
	// 翻訳の画面は多言語対応を ON にしたサイトだけで使える（新しいサイトの既定は OFF）。
	enableMultilingual(t, env.db)

	ed := env.loggedIn(chief)

	// 原文指定で新規作成画面を開く
	res := ed.get("/admin/articles/new?source_article_id=art-ja-origin&lang=en")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/articles/new?source_article_id=art-ja-origin&lang=en returned %d", res.StatusCode)
	}
	body := res.Body

	// 1. 言語・グループ・原文IDの隠しフィールド
	if !strings.Contains(body, `value="en"`) {
		t.Errorf("expected hidden input with lang='en'")
	}
	if !strings.Contains(body, `value="grp-science"`) {
		t.Errorf("expected hidden input with translation_group_id='grp-science'")
	}
	if !strings.Contains(body, `value="art-ja-origin"`) {
		t.Errorf("expected hidden input with source_article_id='art-ja-origin'")
	}

	// 2. 原文バーとスプリットビュー
	if !strings.Contains(body, "日本の先端科学") {
		t.Errorf("source article title should be rendered in banner/split pane")
	}
	if !strings.Contains(body, "全文をAI翻訳") {
		t.Errorf("expected '全文をAI翻訳' button")
	}
	if !strings.Contains(body, "原文と比較") || !strings.Contains(body, "仕上がりプレビュー") {
		t.Errorf("expected split view toggle (compare / result preview)")
	}
	if !strings.Contains(body, "id=\"split-source-pane\"") {
		t.Errorf("expected split source pane element")
	}
}

// TestMultilingual_OutdatedTrackingAndClearing は原文更新時の要更新フラグ付与と翻訳保存時のクリアを検証する。
func TestMultilingual_OutdatedTrackingAndClearing(t *testing.T) {
	env := newTestEnv(t)
	enableMultilingual(t, env.db)
	ed := env.loggedIn(chief)
	ctx := context.Background()

	// 1. 日本語記事を新規作成
	locJA := expectRedirect(t, ed.postForm("/admin/articles/new", url.Values{
		"title":            {"日本語オリジナル"},
		"slug":             {"ja-original"},
		"category_id":      {"1"},
		"content_markdown": {"最初の日本語本文"},
		"action":           {"publish"},
	}), "/admin/articles/")
	jaID := editPathPattern.FindStringSubmatch(locJA)[1]

	jaArt, err := env.server.articles.GetByID(jaID)
	if err != nil {
		t.Fatalf("GetByID(jaID): %v", err)
	}

	// 2. 英語翻訳記事を新規作成
	locEN := expectRedirect(t, ed.postForm("/admin/articles/new", url.Values{
		"title":                {"English Original"},
		"slug":                 {"en-original"},
		"category_id":          {"1"},
		"content_markdown":     {"First English body"},
		"lang":                 {"en"},
		"translation_group_id": {jaArt.TranslationGroupID},
		"source_article_id":    {jaArt.ID},
		"action":               {"publish"},
	}), "/admin/articles/")
	enID := editPathPattern.FindStringSubmatch(locEN)[1]

	enArt, err := env.server.articles.GetByID(enID)
	if err != nil {
		t.Fatalf("GetByID(enID): %v", err)
	}
	if enArt.IsOutdated {
		t.Errorf("newly created translation should not be outdated")
	}

	// 3. 日本語記事を更新保存
	ed.postForm("/admin/articles/"+jaID, url.Values{
		"title":                {"日本語オリジナル（更新版）"},
		"slug":                 {"ja-original"},
		"category_id":          {"1"},
		"content_markdown":     {"改訂された日本語本文"},
		"lang":                 {"ja"},
		"translation_group_id": {jaArt.TranslationGroupID},
		"action":               {"publish"},
	})

	// 英語記事が is_outdated == true になっているか検証
	enArtUpdated, err := env.server.articles.GetByID(enID)
	if err != nil {
		t.Fatalf("GetByID(enID) after JA update: %v", err)
	}
	if !enArtUpdated.IsOutdated {
		t.Errorf("expected enArtUpdated.IsOutdated = true after original updated")
	}

	// 英語記事の編集画面で要更新バナーが表示されるか検証
	resEdit := ed.get("/admin/articles/" + enID + "/edit")
	if resEdit.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/articles/%s/edit returned %d", enID, resEdit.StatusCode)
	}
	bodyEdit := resEdit.Body
	if !strings.Contains(bodyEdit, "id=\"outdated-banner\"") {
		t.Errorf("expected #outdated-banner in edit view")
	}
	if !strings.Contains(bodyEdit, "AIで再翻訳") {
		t.Errorf("expected 'AIで再翻訳' button in outdated banner")
	}

	// 4. 英語記事を更新保存 -> is_outdated がクリアされるか検証
	ed.postForm("/admin/articles/"+enID, url.Values{
		"title":                {"English Original (Updated)"},
		"slug":                 {"en-original"},
		"category_id":          {"1"},
		"content_markdown":     {"Revised English body"},
		"lang":                 {"en"},
		"translation_group_id": {jaArt.TranslationGroupID},
		"source_article_id":    {jaArt.ID},
		"action":               {"publish"},
	})

	enArtSaved, err := env.server.articles.GetByID(enID)
	if err != nil {
		t.Fatalf("GetByID(enID) after EN update: %v", err)
	}
	if enArtSaved.IsOutdated {
		t.Errorf("expected enArtSaved.IsOutdated = false after saving translation")
	}

	// 5. 本文（title, content_markdown）が変わらない更新では is_outdated が立たないこと（3.8）
	ed.postForm("/admin/articles/"+jaID, url.Values{
		"title":                {"日本語オリジナル（更新版）"}, // 同じ
		"slug":                 {"ja-original"},
		"category_id":          {"1"},
		"content_markdown":     {"改訂された日本語本文"}, // 同じ
		"lang":                 {"ja"},
		"translation_group_id": {jaArt.TranslationGroupID},
		"action":               {"publish"},
	})
	enArtCheck, err := env.server.articles.GetByID(enID)
	if err != nil {
		t.Fatalf("GetByID(enID) after JA metadata update: %v", err)
	}
	if enArtCheck.IsOutdated {
		t.Errorf("expected enArtCheck.IsOutdated = false when JA body did not change")
	}

	// 6. 記事一覧（?status=published&lang=en）での英語記事「確認」リンクが /en/articles/<slug> になっていること（3.5）。
	// 言語を指定しない一覧では、日本語の親記事がある英語記事は親の行の EN バッジにまとめるため、英語を選んで確かめる。
	resList := ed.get("/admin/articles?status=published&lang=en")
	if resList.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/articles?status=published&lang=en returned %d", resList.StatusCode)
	}
	if !strings.Contains(resList.Body, `href="/en/articles/en-original"`) {
		t.Errorf("expected published English article view link to be /en/articles/en-original, got body:\n%s", resList.Body)
	}

	_ = ctx
}
