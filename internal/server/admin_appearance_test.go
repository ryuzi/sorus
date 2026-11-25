package server

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"sorus/internal/version"
)

// adminNavLinks は管理画面の上部メニュー（.admin-nav）の項目を「href 表示名」の並びで返す。現在の項目には末尾に " *" を付ける。
func adminNavLinks(t *testing.T, body string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<div class="admin-nav `)
	if !ok {
		t.Fatal("admin nav not found")
	}
	nav, _, _ := strings.Cut(rest, "</div>")
	var out []string
	for _, m := range regexp.MustCompile(`<a href="(/admin[^"?]*)"[^>]*?( aria-current="page")?>([^<]*)</a>`).FindAllStringSubmatch(nav, -1) {
		s := m[1] + " " + strings.TrimSpace(m[3])
		if m[2] != "" {
			s += " *"
		}
		out = append(out, s)
	}
	return out
}

// adminHeaderTargets は管理画面のヘッダー全体（上部メニュー・新しい記事・アカウントのメニュー）にあるリンクと
// フォームの送信先を、クエリを除いて出てくる順に返す。
func adminHeaderTargets(t *testing.T, body string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "<header")
	if !ok {
		t.Fatal("admin header not found")
	}
	header, _, _ := strings.Cut(rest, "</header>")
	var out []string
	for _, m := range regexp.MustCompile(`(?:href|action)="(/admin[^"?]*)`).FindAllStringSubmatch(header, -1) {
		out = append(out, m[1])
	}
	return out
}

// userMenu はアカウントのメニュー（details）の中身を返す。
func userMenu(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<details id="user-menu"`)
	if !ok {
		t.Fatal("user menu not found")
	}
	menu, _, _ := strings.Cut(rest, "</details>")
	return menu
}

// 上部メニューには、ふだん使う項目と「設定」だけを並べる。役割ごとの項目は従来と同じ条件で出し分け、
// 設定の画面は「設定」の入口にまとめる。名前・言語・ログアウト（デスクトップモードでは終了も）はアカウントのメニューに入れる。
func TestAdminNavAppearance(t *testing.T) {
	env := newPagesEnv(t, Config{})
	desktopEnv, _ := newDesktopEnv(t)
	for _, tc := range []struct {
		name    string
		c       *client
		nav     []string
		targets []string
	}{
		{"server admin", env.loggedIn(admin),
			[]string{"/admin ダッシュボード *", "/admin/articles 記事", "/admin/manuscripts 届いた原稿", "/admin/media メディア", "/admin/settings 設定"},
			[]string{"/admin", "/admin", "/admin/articles", "/admin/manuscripts", "/admin/media", "/admin/settings",
				"/admin/set-lang", "/admin/set-lang", "/admin/logout"}},
		{"server editor", env.loggedIn(chief),
			[]string{"/admin ダッシュボード *", "/admin/articles 記事", "/admin/manuscripts 届いた原稿", "/admin/media メディア", "/admin/settings 設定"},
			[]string{"/admin", "/admin", "/admin/articles", "/admin/manuscripts", "/admin/media", "/admin/settings",
				"/admin/set-lang", "/admin/set-lang", "/admin/logout"}},
		{"server writer", env.loggedIn(studentA),
			[]string{"/admin ダッシュボード *", "/admin/articles 記事", "/admin/media メディア", "/admin/settings 設定"},
			[]string{"/admin", "/admin", "/admin/articles", "/admin/media", "/admin/settings",
				"/admin/set-lang", "/admin/set-lang", "/admin/logout"}},
		{"desktop user", desktopEnv.completeSetup(),
			[]string{"/admin ダッシュボード *", "/admin/articles 記事", "/admin/manuscripts 届いた原稿", "/admin/media メディア", "/admin/settings 設定"},
			[]string{"/admin", "/admin", "/admin/articles", "/admin/manuscripts", "/admin/media", "/admin/settings",
				"/admin/set-lang", "/admin/set-lang", "/admin/logout", "/admin/desktop/quit"}},
	} {
		body := tc.c.get("/admin").Body
		if got := adminNavLinks(t, body); strings.Join(got, "\n") != strings.Join(tc.nav, "\n") {
			t.Errorf("%s nav =\n%s\nwant\n%s", tc.name, strings.Join(got, "\n"), strings.Join(tc.nav, "\n"))
		}
		if got := adminHeaderTargets(t, body); strings.Join(got, " ") != strings.Join(tc.targets, " ") {
			t.Errorf("%s header targets =\n%v\nwant\n%v", tc.name, got, tc.targets)
		}
	}
}

// 設定の入口から開く画面（公開先・外観・ユーザー管理など）にいる間は、上部メニューの「設定」を現在地にする。
// ほかの項目も、その画面と配下の画面で現在地になる。現在地は常に1つだけ。
func TestAdminNavCurrentItem(t *testing.T) {
	env := newPagesEnv(t, Config{})
	current := func(c *client, path string) []string {
		t.Helper()
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		var cur []string
		for _, l := range adminNavLinks(t, res.Body) {
			if strings.HasSuffix(l, " *") {
				cur = append(cur, l)
			}
		}
		return cur
	}
	adm := env.loggedIn(admin)
	for _, path := range []string{"/admin/settings", "/admin/settings/general", "/admin/settings/theme", "/admin/settings/structure",
		"/admin/settings/structure?flash=structure_saved", "/admin/settings/backup", "/admin/settings/mcp", "/admin/settings/remote",
		"/admin/publish", "/admin/users", "/admin/settings/ai", "/admin/settings/operations"} {
		if got := current(adm, path); len(got) != 1 || got[0] != "/admin/settings 設定 *" {
			t.Errorf("%s: current nav items = %v, want only 設定", path, got)
		}
	}
	for path, want := range map[string]string{
		"/admin":          "/admin ダッシュボード *",
		"/admin/articles": "/admin/articles 記事 *",
		"/admin/media":    "/admin/media メディア *",
	} {
		if got := current(adm, path); len(got) != 1 || got[0] != want {
			t.Errorf("%s: current nav items = %v, want %s", path, got, want)
		}
	}
	editor := env.loggedIn(chief)
	if got := current(editor, "/admin/manuscripts"); len(got) != 1 || got[0] != "/admin/manuscripts 届いた原稿 *" {
		t.Errorf("/admin/manuscripts: current nav items = %v", got)
	}
	if got := current(editor, "/admin/articles/new"); len(got) != 1 || got[0] != "/admin/articles 記事 *" {
		t.Errorf("/admin/articles/new: current nav items = %v", got)
	}
	writer := env.loggedIn(studentA)
	for _, path := range []string{"/admin/settings", "/admin/settings/mcp", "/admin/settings/ai"} {
		if got := current(writer, path); len(got) != 1 || got[0] != "/admin/settings 設定 *" {
			t.Errorf("writer %s: current nav items = %v, want only 設定", path, got)
		}
	}
}

// settingsHubCards は設定の入口のカードを「href 表示名」の並びで返す。上級者向けの項目は advanced に分ける。
func settingsHubCards(t *testing.T, body string) (cards, advanced []string) {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<ul id="settings-hub"`)
	if !ok {
		t.Fatal("settings hub not found")
	}
	main, adv, _ := strings.Cut(rest, `id="settings-hub-advanced"`)
	re := regexp.MustCompile(`<a href="([^"]*)" id="hub-[^"]*"[^>]*>\s*<span[^>]*>([^<]*)</span>\s*<span[^>]*>([^<]+)</span>`)
	for _, m := range re.FindAllStringSubmatch(main, -1) {
		cards = append(cards, m[1]+" "+html.UnescapeString(m[2]))
	}
	for _, m := range re.FindAllStringSubmatch(adv, -1) {
		advanced = append(advanced, m[1]+" "+html.UnescapeString(m[2]))
	}
	return cards, advanced
}

func TestAdminPageDisplaysVersion(t *testing.T) {
	env := newTestEnv(t)
	res := env.loggedIn(admin).get("/admin/settings")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/settings status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	versionText := "sorus " + version.Version
	if got := strings.Count(res.Body, versionText); got != 1 {
		t.Errorf("settings page contains %d instances of %q, want 1 in admin footer", got, versionText)
	}
	menu := userMenu(t, res.Body)
	expectNotContains(t, menu, versionText)
}

// 設定の入口には、その人が使える画面だけを決まった順（管理画面の情報設計。SPEC §2-5）で並べる。AI の3画面（ON/OFF・使う AI・
// 外部の AI アプリとつなぐ）は「AI」1枚にまとめ、「上級者向け」は出さない。表示名は左のメニューと同じキーで出す。並べた画面は
// どれもその人が開ける。「スマートフォンで書く」は出さない。
func TestSettingsHubCardsFollowRoles(t *testing.T) {
	env := newPagesEnv(t, Config{})
	desktopEnv, _ := newDesktopEnv(t)
	// 表示名は翻訳カタログ（別の作業で文言を見直す）から引く。カタログに無いキーは既定の文言（translateOr）。
	hubCard := func(lang, href, key string) string { return href + " " + translateOr(lang, key) }
	adminCards := func(lang string) []string {
		return []string{
			hubCard(lang, "/admin/settings/general", "settings_nav_general"),
			hubCard(lang, "/admin/settings/theme", "settings_nav_appearance"),
			// 公開サーバーと静的サイトは「公開先」1枚にまとめる（公開サーバーの画面へは公開先から進む）。
			hubCard(lang, "/admin/publish", "settings_nav_publish"),
			hubCard(lang, "/admin/settings/operations", "settings_nav_operations"),
			// AI 機能の ON/OFF・使う AI・外部の AI アプリとの連携は「AI」1枚（画面の中はタブ）。
			hubCard(lang, "/admin/settings/ai-integrations", "settings_nav_ai_section"),
			hubCard(lang, "/admin/settings/categories", "categories"),
			hubCard(lang, "/admin/settings/backup", "settings_nav_backup"),
			hubCard(lang, "/admin/settings/migration", "settings_nav_migration"),
			hubCard(lang, "/admin/users", "users"),
		}
	}
	for _, tc := range []struct {
		name  string
		c     *client
		cards []string
	}{
		{"server admin", env.loggedIn(admin), adminCards("ja")},
		{"server editor", env.loggedIn(chief), []string{hubCard("ja", "/admin/settings/ai", "settings_nav_ai_section"), hubCard("ja", "/admin/settings/categories", "categories")}},
		{"server writer", env.loggedIn(studentA), []string{hubCard("ja", "/admin/settings/ai", "settings_nav_ai_section")}},
		{"desktop user", desktopEnv.completeSetup(), adminCards("ja")},
	} {
		res := tc.c.get("/admin/settings")
		expectStatus(t, res, http.StatusOK)
		cards, advanced := settingsHubCards(t, res.Body)
		if strings.Join(cards, "\n") != strings.Join(tc.cards, "\n") {
			t.Errorf("%s hub =\n%s\nwant\n%s", tc.name, strings.Join(cards, "\n"), strings.Join(tc.cards, "\n"))
		}
		for _, card := range cards {
			if strings.Contains(card, "settings_nav_") || strings.Contains(card, "mobile") {
				t.Errorf("%s: card %q shows a raw key or the removed mobile page", tc.name, card)
			}
		}
		if got := strings.Count(res.Body, "sorus "+version.Version); got != 1 {
			t.Errorf("%s settings hub contains %d version labels, want 1 in the shared admin footer", tc.name, got)
		}
		if len(advanced) != 0 {
			t.Errorf("%s advanced = %v, want none (MCP is a tab of the AI page)", tc.name, advanced)
		}
		expectContains(t, res.Body, "サイトと sorus の設定です。")
		expectNotContains(t, res.Body, ">上級者向け</h2>", `id="hub-mcp"`)
		expectNotContains(t, res.Body, `id="hub-mobile"`)
		for _, card := range append(cards, advanced...) {
			href, _, _ := strings.Cut(card, " ")
			expectStatus(t, tc.c.get(href), http.StatusOK)
		}
	}
	// 公開先・サイト運用の名前は SPEC の表のとおり。
	if got := translateOr("ja", "settings_nav_publish"); got != "公開先" {
		t.Errorf("publish card label = %q", got)
	}
	if got := translateOr("ja", "settings_nav_operations"); got != "サイト運用" {
		t.Errorf("operations card label = %q", got)
	}

	// 英語でも同じ項目を英語の名前と説明で出す。
	adm := env.loggedIn(admin)
	adm.useEnglish()
	res := adm.get("/admin/settings")
	cards, advanced := settingsHubCards(t, res.Body)
	if want := adminCards("en"); strings.Join(cards, ",") != strings.Join(want, ",") {
		t.Errorf("english hub = %v\nwant %v", cards, want)
	}
	if len(advanced) != 0 {
		t.Errorf("english advanced = %v", advanced)
	}
	expectContains(t, res.Body, "connect AI apps such as ChatGPT or Claude")
	expectNotContains(t, res.Body, ">Advanced</h2>")
	if nav := strings.Join(adminNavLinks(t, res.Body), ", "); nav != "/admin Dashboard, /admin/articles Articles, /admin/manuscripts Manuscripts, /admin/media Media, /admin/settings Settings *" {
		t.Errorf("english nav = %s", nav)
	}
}

// デスクトップモードの「sorus を終了」は、アカウントのメニューの中から使える。サーバーモードでは出さない。
func TestDesktopQuitInUserMenu(t *testing.T) {
	desktopEnv, quits := newDesktopEnv(t)
	c := desktopEnv.completeSetup()
	menu := userMenu(t, c.get("/admin").Body)
	expectContains(t, menu, "<summary", "山田 花子 さん", `href="/admin/set-lang?lang=en`, `action="/admin/logout"`,
		`action="/admin/desktop/quit" id="nav-desktop-quit" data-confirm="sorus を終了しますか？"`, "sorus を終了</button>")
	if res := c.postForm(DesktopQuitPath, nil); res.StatusCode != http.StatusOK || quits.Load() != 1 {
		t.Errorf("quit from the user menu: status %d, OnQuit called %d times", res.StatusCode, quits.Load())
	}

	env := newPagesEnv(t, Config{})
	menu = userMenu(t, env.loggedIn(admin).get("/admin").Body)
	expectContains(t, menu, `action="/admin/logout"`)
	expectNotContainsAny(t, "server user menu", menu, "/admin/desktop/quit", "sorus を終了")
}

// 外観の2つの画面は共通の設定ナビを出し、外観の項目を現在地にする。
func TestAppearanceSettingsNavigation(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	for _, path := range []string{
		"/admin/settings/theme", "/admin/settings/structure",
	} {
		body := adm.get(path).Body
		expectContains(t, body, `aria-label="設定"`, `href="/admin/settings/theme" aria-current="page">外観</a>`,
			`href="/admin/publish">公開先</a>`, `href="/admin/settings/ai-integrations">AI</a>`)
	}
	adm.useEnglish()
	expectContains(t, adm.get("/admin/settings/structure").Body,
		`aria-label="Settings"`, `href="/admin/settings/theme" aria-current="page">Appearance</a>`,
		`href="/admin/publish">Publishing</a>`)
}

// メニューの編集欄は説明を一覧の上に1回だけ出し、各行の欄はそれを参照する。欄のラベルは行ごとに残す。
func TestStructureMenuEditorCompact(t *testing.T) {
	env := newPagesEnv(t, Config{})
	adm := env.loggedIn(admin)
	rows := [][3]string{{"page", "research", "研究"}, {"articles", "", "記事"}, {"url", "/search", "検索"}}
	f := structureForm(rows, "")
	f.Set("op", "add")
	res := adm.postForm("/admin/settings/structure", f)
	expectStatus(t, res, http.StatusOK)
	body := res.Body
	for _, hint := range []string{"表示名は、空ならページ名・カテゴリ名になります", "https:// で始まるアドレス"} {
		if n := strings.Count(body, hint); n != 1 {
			t.Errorf("hint %q appears %d times, want once", hint, n)
		}
	}
	if n := strings.Count(body, `aria-describedby="structure-label-hint"`); n != 4 {
		t.Errorf("label inputs referencing the hint = %d, want 4", n)
	}
	if n := strings.Count(body, `aria-describedby="structure-url-hint"`); n != 4 {
		t.Errorf("url inputs referencing the hint = %d, want 4", n)
	}
	for i := 0; i < 4; i++ {
		for _, id := range []string{"menu-type-", "menu-page-", "menu-category-", "menu-url-", "menu-label-"} {
			expectContains(t, body, `<label for="`+id+string(rune('0'+i))+`"`)
		}
	}
	expectContains(t, body, `role="group" aria-labelledby="menu-row-0"`, `id="menu-row-3" class="text-sm font-semibold text-neutral-800 md:sr-only">4番目の項目</span>`)
}
