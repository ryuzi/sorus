package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/rag"
	"sorus/internal/theme"
)

// setCardImageSetting は公開カードの画像表示の設定 key を value にする（空文字は未設定と同じ）。
func (e *testEnv) setCardImageSetting(key, value string) {
	e.t.Helper()
	if _, err := e.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		e.t.Fatal(err)
	}
}

func TestCardImageSettingsDefaults(t *testing.T) {
	env := newTestEnv(t)
	want := cardImageSettings{Front: false, Article: true, Related: true}
	if got := env.server.cardImageSettings(); got != want {
		t.Fatalf("unset settings = %+v, want %+v", got, want)
	}
	// 空の値・不明な値は既定値のまま。
	env.setCardImageSetting(settingShowFrontCardImages, "")
	env.setCardImageSetting(settingShowArticleCardImages, "")
	env.setCardImageSetting(settingShowRelatedCardImages, "maybe")
	if got := env.server.cardImageSettings(); got != want {
		t.Fatalf("empty settings = %+v, want %+v", got, want)
	}
	env.setCardImageSetting(settingShowFrontCardImages, "true")
	env.setCardImageSetting(settingShowArticleCardImages, "false")
	env.setCardImageSetting(settingShowRelatedCardImages, "0")
	if got := env.server.cardImageSettings(); got != (cardImageSettings{Front: true}) {
		t.Fatalf("explicit settings = %+v", got)
	}
}

func TestCardImageSettingsSaveFromThemePage(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	res := adm.get("/admin/settings/theme")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "公開カードのアイキャッチ画像表示", `action="/admin/settings/theme/card-images"`,
		"トップページのページカードに画像を表示する", "記事一覧のカードに画像を表示する", "関連記事のカードに画像を表示する")
	// 未設定なら既定値（トップのカードは表示しない、記事一覧・関連記事は表示する）をチェック状態に出す。
	expectCheckbox(t, res.Body, "show-front-card-images", false)
	expectCheckbox(t, res.Body, "show-article-card-images", true)
	expectCheckbox(t, res.Body, "show-related-card-images", true)

	form := url.Values{settingShowFrontCardImages: {"1"}, settingShowRelatedCardImages: {"1"}}
	expectRedirect(t, adm.postForm("/admin/settings/theme/card-images", form), "/admin/settings/theme?flash=card_images_saved")
	for key, want := range map[string]string{
		settingShowFrontCardImages: "true", settingShowArticleCardImages: "false", settingShowRelatedCardImages: "true",
	} {
		if got := env.setting(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	res = adm.get("/admin/settings/theme")
	expectCheckbox(t, res.Body, "show-front-card-images", true)
	expectCheckbox(t, res.Body, "show-article-card-images", false)
	expectCheckbox(t, res.Body, "show-related-card-images", true)

	// テーマの適用はカードの設定を変えない。
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {theme.Default}}), "/admin/settings/theme?flash=theme_saved")
	if got := env.server.cardImageSettings(); got != (cardImageSettings{Front: true, Article: false, Related: true}) {
		t.Errorf("settings after applying a theme = %+v", got)
	}

	// 管理者以外は変えられない。
	res = env.loggedIn(chief).postForm("/admin/settings/theme/card-images", url.Values{settingShowArticleCardImages: {"1"}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("editor save status = %d, want 403", res.StatusCode)
	}
	if got := env.setting(settingShowArticleCardImages); got != "false" {
		t.Errorf("editor changed %s to %q", settingShowArticleCardImages, got)
	}
}

// expectCheckbox は id のチェックボックスのチェック状態が checked か確かめる。
func expectCheckbox(t *testing.T, body, id string, checked bool) {
	t.Helper()
	_, rest, ok := strings.Cut(body, `id="`+id+`"`)
	if !ok {
		t.Fatalf("checkbox %s is missing", id)
	}
	tag, _, _ := strings.Cut(rest, ">")
	if got := strings.Contains(tag, " checked"); got != checked {
		t.Errorf("checkbox %s checked = %v, want %v", id, got, checked)
	}
}

func TestPublicFrontCardImagesToggle(t *testing.T) {
	env := newFrontEnv(t)
	if _, err := env.db.Exec(`UPDATE articles SET featured_image_url = '/uploads/access.webp' WHERE slug = 'access'`); err != nil {
		t.Fatal(err)
	}
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontPage: "research", FrontCards: []string{"access"}}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()

	// 既定ではトップのページカードに画像の枠を出さない。
	body := c.get("/").Body
	expectContains(t, body, `<li class="front-card front-card--no-media">`, `<h2 class="front-card-title"><a href="/access">`)
	expectNotContainsAny(t, "front default", body, `class="front-card-media`, "/uploads/access.webp")

	env.setCardImageSetting(settingShowFrontCardImages, "true")
	body = c.get("/").Body
	expectContains(t, body, `<li class="front-card">`, `<a href="/access" class="front-card-media"`, `src="/uploads/access.webp"`)
	expectNotContainsAny(t, "front on", body, "front-card--no-media")

	env.setCardImageSetting(settingShowFrontCardImages, "false")
	expectNotContainsAny(t, "front off", c.get("/").Body, `class="front-card-media`)
}

func TestPublicArticleCardImagesToggle(t *testing.T) {
	env := newPublicEnv(t)
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()
	pages := []string{"/", "/articles", "/categories/circle"}

	// 既定では記事のカードに画像の枠（アイキャッチが無ければプレースホルダ）を出す。
	for _, p := range pages {
		body := c.get(p).Body
		expectContains(t, body, `<article class="card">`, `class="card-media`)
		expectNotContainsAny(t, p+" default", body, "card--no-media")
	}

	env.setCardImageSetting(settingShowArticleCardImages, "false")
	for _, p := range pages {
		res := c.get(p)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<article class="card card--no-media">`, `<h2 class="card-title">`)
		expectNotContainsAny(t, p+" off", res.Body, `class="card-media`)
	}

	env.setCardImageSetting(settingShowArticleCardImages, "true")
	expectContains(t, c.get("/articles").Body, `class="card-media`)
}

func TestPublicArticleCardImagesOffOnFrontNews(t *testing.T) {
	env := newFrontEnv(t)
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontPage: "research"}); err != nil {
		t.Fatal(err)
	}
	c := env.newClient()
	expectContains(t, c.get("/").Body, `<article class="card">`, `src="/uploads/news.webp"`)
	env.setCardImageSetting(settingShowArticleCardImages, "false")
	body := c.get("/").Body
	expectContains(t, body, `<article class="card card--no-media">`)
	expectNotContainsAny(t, "front news off", body, `class="card-media`, "/uploads/news.webp")
}

func TestPublicRelatedCardImagesToggle(t *testing.T) {
	env := newTestEnv(t)
	env.server.ragService = rag.NewService(env.db, nil)
	targetID := seedArticle(t, env.db, article.Article{Title: "研究の対象", Slug: "rag-target", ContentMarkdown: "対象の本文。",
		Status: article.StatusPublished, PublishedAt: ago(2 * time.Hour)})
	relatedID := seedArticle(t, env.db, article.Article{Title: "関連する研究記事", Slug: "rag-related", Excerpt: "研究の紹介です。",
		FeaturedImage: "/uploads/related.webp", ContentMarkdown: "近いテーマの本文。",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	setEmbedding(t, env.db, targetID, []float32{1, 0})
	setEmbedding(t, env.db, relatedID, []float32{1, 0})
	c := env.newClient()

	body := c.get("/articles/rag-target").Body
	expectContains(t, body, `<article class="related-article-card">`, `<div class="related-article-media">`, `src="/uploads/related.webp"`)

	env.setCardImageSetting(settingShowRelatedCardImages, "false")
	body = c.get("/articles/rag-target").Body
	expectContains(t, body, `<article class="related-article-card related-article-card--no-media">`, "<h3>関連する研究記事</h3>", "研究の紹介です。")
	expectNotContainsAny(t, "related off", body, `class="related-article-media`, "/uploads/related.webp")
}

func TestSyncSettingsCardImages(t *testing.T) {
	env := newSyncEnv(t)
	adminToken := env.token("chief-admin", auth.RoleAdmin)
	on, off := true, false
	rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{
		ShowFrontCardImages: &on, ShowArticleCardImages: &off,
	}})
	expectSyncStatus(t, rec, http.StatusOK)
	// 指定しなかった項目（関連記事）は既定値のまま。
	if got := env.server.cardImageSettings(); got != (cardImageSettings{Front: true, Article: false, Related: true}) {
		t.Errorf("settings after sync = %+v", got)
	}
}
