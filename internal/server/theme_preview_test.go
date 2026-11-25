package server

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/theme"
)

// newThemePreviewEnv は公開記事を2件（新しいほうが「最新の記事」）用意したサーバーを起動する。
func newThemePreviewEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{Title: "先週の記事", Slug: "older", ContentMarkdown: "古い本文",
			Status: article.StatusPublished, PublishedAt: ago(7 * 24 * time.Hour)})
		seedArticle(t, db, article.Article{Title: "最新の記事", Slug: "newest", ContentMarkdown: "新しい本文",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
}

// useEnglish は管理画面の表示言語を英語にする。
func (c *client) useEnglish() {
	c.t.Helper()
	expectStatus(c.t, c.get("/admin/set-lang?lang=en&return=/admin"), http.StatusSeeOther)
}

func TestThemeSettingsPageListsCatalog(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.db.Exec(`UPDATE settings SET value = 'tech' WHERE key = 'theme'`); err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"ja", "en"} {
		t.Run(lang, func(t *testing.T) {
			adm := env.loggedIn(admin)
			if lang == "en" {
				adm.useEnglish()
			}
			res := adm.get("/admin/settings/theme")
			expectStatus(t, res, http.StatusOK)
			for _, th := range theme.Builtin.All() {
				// 名前・説明は管理画面の言語で、カタログ（theme.json）から出す。カードには縮小した実物と、プレビューへのリンクを出す。
				expectContains(t, res.Body, html.EscapeString(th.Label(lang)), html.EscapeString(th.Summary(lang)),
					`value="`+th.ID+`" required`,
					`<iframe src="/admin/settings/theme/preview?frame=1&amp;theme=`+th.ID+`" loading="lazy" tabindex="-1" aria-hidden="true" title=""`,
					`href="/admin/settings/theme/preview?theme=`+th.ID+`" target="_blank"`)
			}
			// 適用中のバッジは保存済みのテーマにだけ出す。
			expectContains(t, res.Body, `value="tech" required checked`, `id="theme-applied-tech"`)
			if n := strings.Count(res.Body, `class="theme-applied-badge`); n != 1 {
				t.Errorf("applied badges = %d, want 1", n)
			}
			expectContains(t, res.Body,
				`class="theme-applied-badge shrink-0 whitespace-nowrap`,
				`class="flex flex-wrap items-center justify-between gap-2"`,
				`class="min-w-0 break-words text-base text-neutral-900"`)
			wantText := map[string][]string{
				"ja": {"テーマ設定", "適用中", "このテーマを適用", "プレビュー"},
				"en": {"Theme settings", "Applied", "Apply this theme", "Preview"},
			}[lang]
			expectContains(t, res.Body, wantText...)
			// 連携を設定していなければ、公開サーバーへの反映は出さない。
			for _, unwanted := range []string{`id="remote-push-settings"`, "js/remote_sync.js", `id="theme-missing-warning"`} {
				if strings.Contains(res.Body, unwanted) {
					t.Errorf("theme settings page contains %s", unwanted)
				}
			}
		})
	}
}

func TestThemeSettingsSavedFlash(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	loc := expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"news"}}), "/admin/settings/theme")
	expectContains(t, adm.get(loc).Body,
		"テーマを適用しました。CDN を使っている場合、公開サイトへの反映には最大1時間かかります", `value="news" required checked`)
	adm.useEnglish()
	expectContains(t, adm.get(loc).Body, "Theme applied. If you use a CDN, it can take up to an hour to reach the public site")
}

// 保存値がカタログに無ければ、Default で表示していることを警告し、どのカードにも「適用中」を出さない。
func TestThemeSettingsWarnsAboutMissingTheme(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.db.Exec(`UPDATE settings SET value = 'retired-theme' WHERE key = 'theme'`); err != nil {
		t.Fatal(err)
	}
	def := theme.Builtin.Resolve(theme.Default)
	adm := env.loggedIn(admin)
	res := adm.get("/admin/settings/theme")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="theme-missing-warning"`,
		"保存済みのテーマ「retired-theme」が見つからないため、"+def.Label("ja")+" で表示しています",
		`value="`+def.ID+`" required checked`)
	if strings.Contains(res.Body, `class="theme-applied-badge`) {
		t.Error("a card is marked as applied although the stored theme is missing")
	}
	adm.useEnglish()
	expectContains(t, adm.get("/admin/settings/theme").Body,
		html.EscapeString(`The saved theme "retired-theme" was not found, so the site is shown with `+def.Label("en")))
}

// テーマを保存すると、公開サーバーへ未反映として共通バナーに「公開サーバーへ反映する」を出す。設定を反映できない
// トークンでは出さない。ボタンが呼ぶ既存の反映 API で、テーマが公開サーバーへ届き、バナーは消える。
func TestThemeSettingsOffersRemotePush(t *testing.T) {
	env := newRemoteEnv(t)
	adm := env.local.loggedIn(admin)

	env.configure("sato", auth.RoleEditor)
	expectRedirect(t, adm.postForm("/admin/settings/theme", url.Values{"theme": {"tech"}}), "/admin/settings/theme")
	if res := adm.get("/admin/settings/theme"); strings.Contains(res.Body, `id="remote-push-settings"`) {
		t.Error("theme settings offers push with a token that cannot sync settings")
	}

	env.configure("root", auth.RoleAdmin)
	res := adm.get("/admin/settings/theme")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="remote-dirty-banner"`, `data-remote-dirty-item="theme"`, `id="remote-push-settings"`,
		`data-confirm-message="公開サーバーのサイト基本設定を、このPCの設定で上書きします。よろしいですか？"`,
		"公開サーバーへ反映する", "/static/js/remote_sync.js?v=")

	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, nil), http.StatusOK)
	if got := env.remote.setting("theme"); got != "tech" {
		t.Errorf("remote theme = %q, want tech", got)
	}
	expectNotContains(t, adm.get("/admin/settings/theme").Body, `id="remote-dirty-banner"`, `id="remote-push-settings"`)
}

func TestThemePreview(t *testing.T) {
	env := newThemePreviewEnv(t)
	if _, err := env.db.Exec(`INSERT INTO settings (key, value) VALUES ('analytics_tag', '<script id="site-analytics"></script>')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}
	adm := env.loggedIn(admin)

	res := adm.get("/admin/settings/theme/preview?theme=tech")
	expectStatus(t, res, http.StatusOK)
	for header, want := range map[string]string{
		"Cache-Control":   "no-store",
		"X-Robots-Tag":    "noindex",
		"X-Frame-Options": "SAMEORIGIN",
	} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	// 本物の公開トップページを、保存済みの modern ではなく tech で描画する。
	expectContains(t, res.Body, `data-theme="tech"`, `/static/themes/tech/theme.css?v=`, `<meta name="robots" content="noindex">`,
		"最新の記事", "先週の記事",
		`class="theme-preview-bar"`, "プレビュー中: テック", `/static/css/theme_preview.css?v=`,
		`href="/admin/settings/theme/preview?theme=tech" aria-current="page"`,
		`href="/admin/settings/theme/preview?page=article&amp;theme=tech"`,
		`<form method="post" action="/admin/settings/theme">`,
		`name="_csrf" value="`+adm.csrfToken()+`"`, `name="theme" value="tech"`,
		`href="/admin/settings/theme">テーマ設定に戻る`)
	for _, unwanted := range []string{`/static/themes/modern/`, `id="site-analytics"`} {
		if strings.Contains(res.Body, unwanted) {
			t.Errorf("preview contains %s", unwanted)
		}
	}

	t.Run("article page shows the newest article", func(t *testing.T) {
		res := adm.get("/admin/settings/theme/preview?theme=news&page=article")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `data-theme="news"`, "<title>最新の記事 |", "新しい本文",
			`href="/admin/settings/theme/preview?page=article&amp;theme=news" aria-current="page"`)
	})

	t.Run("frame has no bar", func(t *testing.T) {
		res := adm.get("/admin/settings/theme/preview?theme=tech&frame=1")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `data-theme="tech"`, `data-theme-preview="frame"`, `<meta name="robots" content="noindex">`)
		for _, unwanted := range []string{"theme-preview-bar", `action="/admin/settings/theme"`, "_csrf"} {
			if strings.Contains(res.Body, unwanted) {
				t.Errorf("framed preview contains %s", unwanted)
			}
		}
		if got := res.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("framed Cache-Control = %q", got)
		}
	})

	t.Run("applied theme has no apply button", func(t *testing.T) {
		res := adm.get("/admin/settings/theme/preview?theme=modern")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "プレビュー中: モダン", `class="theme-preview-badge">適用中`)
		if strings.Contains(res.Body, `action="/admin/settings/theme"`) {
			t.Error("preview of the applied theme offers to apply it again")
		}
	})

	t.Run("bar follows the admin language", func(t *testing.T) {
		en := env.loggedIn(admin)
		en.useEnglish()
		res := en.get("/admin/settings/theme/preview?theme=tech")
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "Previewing: Tech", "Apply this theme", "Back to theme settings")
	})

	// プレビューは保存しない。訪問者は ?theme= でテーマを変えられず、プレビューバーも見えない。
	if got := env.server.getSetting("theme", ""); got != "modern" {
		t.Errorf("stored theme after preview = %q, want modern", got)
	}
	pub := env.newClient().get("/?theme=tech")
	expectStatus(t, pub, http.StatusOK)
	expectContains(t, pub.Body, `data-theme="modern"`, `id="site-analytics"`)
	for _, unwanted := range []string{`data-theme="tech"`, "theme-preview", `name="robots"`} {
		if strings.Contains(pub.Body, unwanted) {
			t.Errorf("public page contains %s", unwanted)
		}
	}
}

func TestThemePreviewWithoutArticles(t *testing.T) {
	env := newTestEnv(t)
	res := env.loggedIn(admin).get("/admin/settings/theme/preview?theme=tech&page=article")
	expectStatus(t, res, http.StatusOK)
	// 公開記事が無ければトップを表示し、「記事」への切り替えは出さない。
	expectContains(t, res.Body, `data-theme="tech"`, `href="/admin/settings/theme/preview?theme=tech" aria-current="page"`)
	if strings.Contains(res.Body, "page=article") {
		t.Error("preview offers the article page without published articles")
	}
}

func TestThemePreviewDoesNotCountViews(t *testing.T) {
	env := newThemePreviewEnv(t)
	adm := env.loggedIn(admin)
	reader := env.newClient()
	expectStatus(t, reader.getAs("/articles/newest", browserUA, nil), http.StatusOK)
	for _, q := range []string{"theme=tech&page=article", "theme=news&page=article&frame=1"} {
		expectStatus(t, adm.getAs("/admin/settings/theme/preview?"+q, browserUA, nil), http.StatusOK)
	}
	env.flushAnalytics()
	var id string
	if err := env.db.Get(&id, `SELECT id FROM articles WHERE slug = 'newest'`); err != nil {
		t.Fatal(err)
	}
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily WHERE article_id = ?`, id); n != 1 {
		t.Errorf("article views = %d, want 1 (the reader only)", n)
	}
}

func TestThemePreviewRequiresAdmin(t *testing.T) {
	env := newThemePreviewEnv(t)
	for _, u := range []testUser{studentA, chief} {
		if res := env.loggedIn(u).get("/admin/settings/theme/preview?theme=tech"); res.StatusCode != http.StatusForbidden {
			t.Errorf("preview as %s = %d, want 403", u.Role, res.StatusCode)
		}
	}
	res := env.newClient().get("/admin/settings/theme/preview?theme=tech")
	expectRedirect(t, res, "/admin/login")
}

func TestThemePreviewRejectsUnknownTheme(t *testing.T) {
	env := newThemePreviewEnv(t)
	adm := env.loggedIn(admin)
	for _, bad := range []string{"unknown-theme", "mono", "../news", "News", ""} {
		res := adm.get("/admin/settings/theme/preview?theme=" + url.QueryEscape(bad))
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("preview theme %q = %d, want 422", bad, res.StatusCode)
		}
	}
}

// 公開専用モードでは /admin を登録しないため、プレビューも存在しないページになる。
func TestThemePreviewHiddenInPublicOnlyMode(t *testing.T) {
	env := newTestEnvWith(t, Config{PublicOnly: true}, nil)
	pub := env.newClient()
	for _, p := range []string{"/admin/settings/theme/preview?theme=tech", "/admin/settings/theme/preview?theme=tech&frame=1"} {
		res := pub.get(p)
		expectStatus(t, res, http.StatusNotFound)
		if strings.Contains(res.Body, `data-theme="tech"`) {
			t.Errorf("%s rendered the preview in public-only mode", p)
		}
	}
}
