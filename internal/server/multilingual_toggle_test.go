package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// seedBilingualPair は公開中の日本語記事と、その英語版を登録する seed を返す。
func seedBilingualPair(t *testing.T) func(db *sqlx.DB) {
	return func(db *sqlx.DB) { seedBilingualPairInto(t, db) }
}

func seedBilingualPairInto(t *testing.T, db *sqlx.DB) {
	t.Helper()
	catID := seedCategory(t, db, "ニュース", "news", "")
	seedArticle(t, db, article.Article{ID: "ja-1", Title: "日本語の記事", Slug: "ja-article", CategoryID: catID,
		ContentMarkdown: "本文", Status: article.StatusPublished, PublishedAt: ago(2 * time.Hour), Lang: "ja", TranslationGroupID: "g1"})
	seedArticle(t, db, article.Article{ID: "en-1", Title: "English Article", Slug: "en-article", CategoryID: catID,
		ContentMarkdown: "Body", Status: article.StatusPublished, PublishedAt: ago(time.Hour), Lang: "en", TranslationGroupID: "g1"})
}

// saveMultilingual は管理者で基本設定を保存し、多言語対応のチェックを on にするか外す。
func saveMultilingual(t *testing.T, adm *client, on bool) {
	t.Helper()
	form := generalSettingsForm()
	if on {
		// 未チェックを表す hidden の "0" をチェックボックスの前に置くフォームでも ON になる。
		form["multilingual_enabled"] = []string{"0", "1"}
	}
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=")
}

func TestMultilingualDefaultOffForNewSite(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		catID := seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{ID: "ja-only", Title: "日本語だけ", Slug: "ja-only", ContentMarkdown: "本文",
			CategoryID: catID, Status: article.StatusPublished, PublishedAt: ago(time.Hour), Lang: "ja"})
	})
	if env.server.isMultilingualEnabled() {
		t.Fatal("a site without English content must start with multilingual OFF")
	}
	reader := env.newClient()
	for _, p := range []string{"/en", "/en/", "/en/articles", "/en/search", "/en/categories/uncategorized", "/en/about"} {
		if res := reader.get(p); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 while multilingual is OFF", p, res.StatusCode)
		}
	}
	res := reader.get("/")
	expectStatus(t, res, http.StatusOK)
	expectNotContains(t, res.Body, `hreflang=`)

	// 英語の下書きを1件作っただけでも、保存していないサイトは今までどおり ON とみなす（既存サイトの互換）。
	seedArticle(t, env.db, article.Article{ID: "en-draft", Title: "Draft", Slug: "en-draft", ContentMarkdown: "x",
		Status: article.StatusDraft, Lang: "en"})
	if !env.server.isMultilingualEnabled() {
		t.Error("a site with an English draft must be treated as multilingual ON when the setting is not saved")
	}
	expectStatus(t, reader.get("/en/"), http.StatusOK)

	// ゴミ箱の英語記事だけなら ON にはしない。
	if _, err := env.db.Exec(`UPDATE articles SET status = 'deleted' WHERE id = 'en-draft'`); err != nil {
		t.Fatal(err)
	}
	if env.server.isMultilingualEnabled() {
		t.Error("an English article in the trash must not turn multilingual ON")
	}
}

func TestMultilingualAutoOnForExistingSiteAndToggle(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, seedBilingualPair(t))
	reader := env.newClient()

	// 英語記事のある既存サイトは、保存していなくても ON（今までどおり /en と hreflang を出す）。
	if !env.server.isMultilingualEnabled() {
		t.Fatal("existing site with English articles must be multilingual ON")
	}
	expectStatus(t, reader.get("/en/articles/en-article"), http.StatusOK)
	res := reader.get("/articles/ja-article")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `hreflang="en"`, testBaseURL+"/en/articles/en-article")
	expectContains(t, reader.get("/sitemap.xml").Body, testBaseURL+"/en/articles/en-article")

	// 基本設定で OFF にすると、英語のページは 404、言語の切り替え・hreflang は出さない。
	adm := env.loggedIn(admin)
	saveMultilingual(t, adm, false)
	if got := env.setting(settingMultilingualEnabled); got != "0" {
		t.Fatalf("multilingual_enabled = %q, want \"0\"", got)
	}
	if env.server.isMultilingualEnabled() {
		t.Fatal("explicit OFF must win over the English articles")
	}
	for _, p := range []string{"/en", "/en/", "/en/articles", "/en/articles/en-article", "/en/categories/news", "/en/search?q=x"} {
		res := reader.get(p)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 while multilingual is OFF", p, res.StatusCode)
		}
		// 404 ページも、開けない英語のトップ・検索へのリンクを出さない。
		expectNotContains(t, res.Body, `href="/en/"`, `href="/en/search"`)
	}
	res = reader.get("/articles/ja-article")
	expectStatus(t, res, http.StatusOK)
	expectNotContains(t, res.Body, `hreflang=`, `/en/articles/en-article`)
	expectNotContains(t, reader.get("/").Body, `hreflang=`)
	expectNotContains(t, reader.get("/sitemap.xml").Body, "/en/", "hreflang")
	expectNotContains(t, reader.get("/llms.txt").Body, "/en/")
	expectNotContains(t, reader.get("/llms-full.txt").Body, "/en/")
	expectNotContains(t, reader.get("/feed.xml").Body, "/en/")
	// 英語記事が日本語の URL で見えるようにはならない。
	expectStatus(t, reader.get("/articles/en-article"), http.StatusNotFound)

	// ON に戻すと元どおり。
	saveMultilingual(t, adm, true)
	if got := env.setting(settingMultilingualEnabled); got != "1" {
		t.Fatalf("multilingual_enabled = %q, want \"1\"", got)
	}
	expectStatus(t, reader.get("/en/articles/en-article"), http.StatusOK)
	expectContains(t, reader.get("/articles/ja-article").Body, `hreflang="en"`)
}

// TestMultilingualFlagInAdminData は管理画面の共通データ（page）と、基本設定・記事一覧・記事編集・AI 設定のデータに
// MultilingualEnabled を渡していることを確かめる（テンプレートはこれで翻訳まわりの表示を切り替える）。
func TestMultilingualFlagInAdminData(t *testing.T) {
	env := newTestEnv(t)
	pages := recordPages(env)
	adm := env.loggedIn(admin)
	id := adm.createArticle("記事", "本文")

	check := func(want bool) {
		t.Helper()
		gp := pages.renderedPage(t, adm, "/admin/settings/general", "settings_general")
		if gp.MultilingualEnabled != want || gp.Data.(generalSettingsData).MultilingualEnabled != want {
			t.Errorf("settings_general: page %v, data %v, want %v", gp.MultilingualEnabled, gp.Data.(generalSettingsData).MultilingualEnabled, want)
		}
		lp := pages.renderedPage(t, adm, "/admin/articles", "article_list")
		if lp.MultilingualEnabled != want || lp.Data.(listData).MultilingualEnabled != want {
			t.Errorf("article_list: page %v, data %v, want %v", lp.MultilingualEnabled, lp.Data.(listData).MultilingualEnabled, want)
		}
		ep := pages.renderedPage(t, adm, "/admin/articles/"+id+"/edit", "article_edit")
		if ep.MultilingualEnabled != want || ep.Data.(editData).MultilingualEnabled != want {
			t.Errorf("article_edit: page %v, data %v, want %v", ep.MultilingualEnabled, ep.Data.(editData).MultilingualEnabled, want)
		}
		ap := pages.renderedPage(t, adm, "/admin/settings/ai", "settings_ai")
		if ap.MultilingualEnabled != want || ap.Data.(aiSettingsData).MultilingualEnabled != want {
			t.Errorf("settings_ai: page %v, data %v, want %v", ap.MultilingualEnabled, ap.Data.(aiSettingsData).MultilingualEnabled, want)
		}
	}
	check(false)
	saveMultilingual(t, adm, true)
	check(true)
	saveMultilingual(t, adm, false)
	check(false)
}

// TestMultilingualOffKeepsHiddenFields は多言語対応が OFF の画面で隠した欄（英語のサイト名・翻訳に使う AI）が
// 送られてこなくても、保存済みの値を空欄で上書きしないことを確かめる。
func TestMultilingualOffKeepsHiddenFields(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	form := generalSettingsForm()
	form.Set("site_name_en", "English Name")
	form.Set("multilingual_enabled", "1")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=")

	saveMultilingual(t, adm, false) // site_name_en の欄なし
	if got := env.setting("site_name_en"); got != "English Name" {
		t.Errorf("site_name_en = %q, want it kept when the field is not sent", got)
	}
	// 欄を送って空にしたときは消す。
	form = generalSettingsForm()
	form.Set("site_name_en", "")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=")
	var v string
	_ = env.db.Get(&v, `SELECT value FROM settings WHERE key = 'site_name_en'`)
	if v != "" {
		t.Errorf("site_name_en = %q, want empty when the field is sent empty", v)
	}

	// 翻訳に使う AI（執筆スタイルだけを保存する編集者の画面）も、欄が無ければ保存済みのまま。
	if err := upsertSetting(env.db, aiSettingKey, `{"provider":"gemini","translation_provider":"deepl"}`); err != nil {
		t.Fatal(err)
	}
	ed := env.loggedIn(chief)
	expectRedirect(t, ed.postForm(aiSettingsPath, url.Values{"writing_style": {"business"}}), aiSettingsPath+"?flash=")
	if cfg, _ := env.server.loadAIConfig(); cfg.TranslationProvider != "deepl" || cfg.WritingStyle != "business" {
		t.Errorf("after style-only save: translation_provider %q, style %q", cfg.TranslationProvider, cfg.WritingStyle)
	}
	expectRedirect(t, ed.postForm(aiSettingsPath, url.Values{"writing_style": {"business"}, "translation_provider": {""}}), aiSettingsPath+"?flash=")
	if cfg, _ := env.server.loadAIConfig(); cfg.TranslationProvider != "" {
		t.Errorf("translation_provider = %q, want cleared when the field is sent empty", cfg.TranslationProvider)
	}
}

// TestExportSkipsEnglishWhenMultilingualOff は多言語対応が OFF なら、静的サイトの書き出しで /en 以下を書き出さず
// （書き出しも失敗しない）、ON なら書き出すことを確かめる。
func TestExportSkipsEnglishWhenMultilingualOff(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, seedBilingualPair(t))
	adm := env.loggedIn(admin)

	saveMultilingual(t, adm, false)
	off := filepath.Join(t.TempDir(), "off")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: off}); err != nil {
		t.Fatalf("Export with multilingual OFF: %v", err)
	}
	if _, err := os.Stat(filepath.Join(off, "en")); !os.IsNotExist(err) {
		t.Errorf("en/ was exported while multilingual is OFF (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(off, "articles", "ja-article", "index.html")); err != nil {
		t.Errorf("Japanese article was not exported: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(off, "sitemap.xml")); err != nil || strings.Contains(string(b), "/en/") {
		t.Errorf("sitemap.xml contains /en/ while OFF (err %v)", err)
	}

	saveMultilingual(t, adm, true)
	on := filepath.Join(t.TempDir(), "on")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: on}); err != nil {
		t.Fatalf("Export with multilingual ON: %v", err)
	}
	for _, p := range []string{"en/index.html", "en/articles/en-article/index.html", "en/search/index.html"} {
		if _, err := os.Stat(filepath.Join(on, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s was not exported while multilingual is ON: %v", p, err)
		}
	}
}
