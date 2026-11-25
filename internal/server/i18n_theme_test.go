package server

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

func TestAdminSetLanguage(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	// 1. 正常な英語切り替えとリダイレクト
	res := c.get("/admin/set-lang?lang=en&return=/admin/login")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/admin/login" {
		t.Errorf("Location = %q, want %q", loc, "/admin/login")
	}

	// Cookie の属性確認
	var foundCookie bool
	for _, cookie := range res.Cookies() {
		if cookie.Name == "sorus_lang" {
			foundCookie = true
			if cookie.Value != "en" {
				t.Errorf("cookie value = %q, want %q", cookie.Value, "en")
			}
			if cookie.Path != "/admin" {
				t.Errorf("cookie path = %q, want %q", cookie.Path, "/admin")
			}
			if !cookie.HttpOnly {
				t.Errorf("cookie HttpOnly = false, want true")
			}
			if cookie.SameSite != http.SameSiteLaxMode {
				t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
			}
			if cookie.MaxAge <= 0 {
				t.Errorf("cookie MaxAge = %d, want > 0", cookie.MaxAge)
			}
		}
	}
	if !foundCookie {
		t.Fatal("sorus_lang cookie not set in response")
	}

	// 2. 英語 Cookie を持った状態でのログイン画面レンダリング検証
	loginRes := c.get("/admin/login")
	expectContains(t, loginRes.Body, `<html lang="en">`, `Username`, `Password`, `Log in`)
	if strings.Contains(loginRes.Body, `ユーザー名`) {
		t.Errorf("login body in EN unexpectedly contains 'ユーザー名'")
	}

	// 3. ログイン後のヘッダー英語レンダリング検証
	adminClient := env.loggedIn(admin)
	adminClient.get("/admin/set-lang?lang=en&return=/admin")
	dashRes := adminClient.get("/admin")
	expectContains(t, dashRes.Body, `<html lang="en">`, `Dashboard`, `Articles`, `Log out`)

	// 4. オープンリダイレクトの拒絶
	for _, badReturn := range []string{
		"//evil.com",
		"https://evil.com",
		"/\\evil.com",
		"http://evil.com/admin",
		"javascript:alert(1)",
		"//",
	} {
		r := c.get("/admin/set-lang?lang=ja&return=" + badReturn)
		if loc := r.Header.Get("Location"); loc != "/admin" {
			t.Errorf("open redirect bypass for %q: Location = %q, want %q", badReturn, loc, "/admin")
		}
	}
}

func TestPublicThemes(t *testing.T) {
	env := newTestEnv(t)
	admin := env.loggedIn(admin)
	page := admin.get("/admin/settings/theme")
	if page.StatusCode != http.StatusOK {
		t.Fatalf("theme settings status = %d", page.StatusCode)
	}
	if !strings.Contains(page.Body, `href="/admin/settings/theme"`) {
		t.Error("admin navigation does not show the theme settings link")
	}
	// 名前・説明はカタログ（theme.json）から出す。
	expectContains(t, page.Body, "テーマ設定", `value="modern"`, `value="tech"`, `value="news"`,
		"モダン", "テック", "ニュース", "明朝体の見出しと紙面のようなレイアウト")
	for _, legacy := range []string{"minimal", "editorial", "mono"} {
		if strings.Contains(page.Body, `value="`+legacy+`"`) {
			t.Errorf("theme settings offers legacy theme %s", legacy)
		}
	}

	// 前後の空白は除いて保存する。
	res := admin.postForm("/admin/settings/theme", url.Values{"theme": {" news "}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save theme status = %d, want 303", res.StatusCode)
	}
	var selected string
	if err := env.db.Get(&selected, `SELECT value FROM settings WHERE key = 'theme'`); err != nil {
		t.Fatal(err)
	}
	if selected != "news" {
		t.Fatalf("stored theme = %q, want news", selected)
	}
	expectContains(t, admin.get("/admin/settings/theme").Body, `value="news" required checked`)

	public := env.newClient()
	res = public.get("/?theme=tech")
	expectContains(t, res.Body, `data-theme="news"`, `/static/themes/news/theme.css?v=`, `/static/css/public.css?v=`,
		`<meta name="color-scheme" content="light dark">`)
	for _, unwanted := range []string{`sorus_theme`, `#0f172a`, `name="theme-color"`, `/static/themes/tech/`} {
		if strings.Contains(res.Body, unwanted) {
			t.Errorf("public response contains %q", unwanted)
		}
	}
}

// TestStoredThemeOutsideCatalogFallsBack は、カタログに無い ID が保存されていても（マイグレーション前に
// 書き込まれた値など）、公開ページと設定画面が Default で表示されることを確かめる。
func TestStoredThemeOutsideCatalogFallsBack(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.db.Exec(`UPDATE settings SET value = 'mono' WHERE key = 'theme'`); err != nil {
		t.Fatal(err)
	}
	res := env.newClient().get("/")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `data-theme="modern"`, `/static/themes/modern/theme.css?v=`)
	if strings.Contains(res.Body, "mono") {
		t.Error("public page references the unknown theme")
	}
	expectContains(t, env.loggedIn(admin).get("/admin/settings/theme").Body, `value="modern" required checked`)
}

// staticRefPattern は HTML の src・href 属性にある /static/ の参照。
var staticRefPattern = regexp.MustCompile(`(?:src|href)="(/static/[^"]*)"`)

// versionedStaticPattern は内容のハッシュ付きの /static/ の URL。
var versionedStaticPattern = regexp.MustCompile(`^/static/[a-z0-9_./-]+\?v=[0-9a-f]{16}$`)

// TestLayoutStaticAssetsAreVersioned は、管理画面・公開サイトの HTML が出す /static/ の参照が
// すべて ?v=<hash> 付きで、その URL が実在し、不変キャッシュで配信されることを確かめる。
func TestLayoutStaticAssetsAreVersioned(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedCategory(t, db, "未分類", "uncategorized", "")
		seedArticle(t, db, article.Article{Title: "版付き URL", Slug: "versioned", ContentMarkdown: "本文 $x^2$",
			Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	adm := env.loggedIn(admin)
	writer := env.loggedIn(studentA)
	pub := env.newClient()
	pages := []struct {
		c    *client
		path string
	}{
		{pub, "/admin/login"},
		{adm, "/admin"},
		{adm, "/admin/articles"},
		{writer, "/admin/articles/new"},
		{adm, "/admin/media"},
		{adm, "/admin/settings/theme"},
		{pub, "/"},
		{pub, "/articles/versioned"},
	}
	seen := map[string]bool{}
	for _, p := range pages {
		res := p.c.get(p.path)
		expectStatus(t, res, http.StatusOK)
		refs := staticRefPattern.FindAllStringSubmatch(res.Body, -1)
		if len(refs) == 0 {
			t.Errorf("%s has no /static/ references", p.path)
		}
		for _, m := range refs {
			ref := html.UnescapeString(m[1])
			if !versionedStaticPattern.MatchString(ref) {
				t.Errorf("%s references unversioned %s", p.path, ref)
				continue
			}
			seen[ref] = true
		}
	}
	for _, want := range []string{"/static/css/admin.css?v=", "/static/js/admin.js?v=", "/static/css/public.css?v=",
		"/static/js/public.js?v=", "/static/icon.svg?v="} {
		found := false
		for ref := range seen {
			found = found || strings.HasPrefix(ref, want)
		}
		if !found {
			t.Errorf("no page references %s", want)
		}
	}
	for ref := range seen {
		res := pub.get(ref)
		expectStatus(t, res, http.StatusOK)
		if cc := res.Header.Get("Cache-Control"); cc != StaticCacheControl {
			t.Errorf("%s Cache-Control = %q, want %q", ref, cc, StaticCacheControl)
		}
	}
}

func TestThemeSettingsRequireAdmin(t *testing.T) {
	env := newTestEnv(t)
	for _, user := range []testUser{studentA, chief} {
		c := env.loggedIn(user)
		if page := c.get("/admin"); strings.Contains(page.Body, `href="/admin/settings/theme"`) {
			t.Errorf("admin navigation shows theme settings link to %s", user.Role)
		}
		if res := c.get("/admin/settings/theme"); res.StatusCode != http.StatusForbidden {
			t.Errorf("GET theme settings as %s = %d, want 403", user.Role, res.StatusCode)
		}
		if res := c.postForm("/admin/settings/theme", url.Values{"theme": {"news"}}); res.StatusCode != http.StatusForbidden {
			t.Errorf("POST theme settings as %s = %d, want 403", user.Role, res.StatusCode)
		}
	}
}

func TestThemeSettingsRejectInvalidTheme(t *testing.T) {
	env := newTestEnv(t)
	admin := env.loggedIn(admin)
	// 廃止した mono なども、同期 API・エクスポートと同じくカタログで拒否する。
	for _, bad := range []string{"unknown-theme", "mono", "minimal", "editorial", "../news", "News", ""} {
		res := admin.postForm("/admin/settings/theme", url.Values{"theme": {bad}})
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("theme %q status = %d, want 422", bad, res.StatusCode)
		}
	}
	var stored string
	if err := env.db.Get(&stored, `SELECT value FROM settings WHERE key = 'theme'`); err != nil || stored != "modern" {
		t.Errorf("stored theme = %q (%v), want modern", stored, err)
	}
}
