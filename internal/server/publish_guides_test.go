package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestSettingsHubShowsBalancedCategoryCards(t *testing.T) {
	env := newPagesEnv(t, Config{})
	c := env.loggedIn(admin)
	res := c.get(settingsHubPath)
	expectStatus(t, res, http.StatusOK)
	for _, href := range []string{
		`href="/admin/settings/general"`, `href="/admin/settings/theme"`, `href="/admin/publish"`,
		`href="/admin/settings/ai-integrations"`, `href="/admin/settings/backup"`, `href="/admin/settings/migration"`,
	} {
		expectContains(t, res.Body, href)
	}
	expectContains(t, res.Body, `id="settings-hub"`, "多言語対応の ON/OFF、Favicon、アクセス解析",
		"公開サイトのテーマ・色・メニュー・トップページの構成を整えます。",
		"記事・画像・設定を1つの Zip にまとめて保存し、必要なときに復元します。",
		"公開サーバーに接続する、または静的サイトとして",
		"外部サービス・ブログ（WordPress 等）から")
	// 公開方法は公開サーバーか静的サイトのどちらか1つなので、入口は「公開先」1枚にまとめる。サイト運用は別のカード。
	expectContains(t, res.Body, `href="/admin/publish" id="hub-publish"`, `href="/admin/settings/operations" id="hub-operations"`)
	expectNotContains(t, res.Body, `id="hub-remote"`, `href="/admin/settings/remote"`)
	expectNotContains(t, res.Body, `id="settings-hub-publish"`, `data-hub-publishing-method=`, "必要なもの", "向いている使い方")
	if got := strings.Count(res.Body, `id="hub-`); got < 6 {
		t.Errorf("settings hub has %d category cards, want at least 6", got)
	}

	for _, u := range []testUser{chief, studentA} {
		res := env.loggedIn(u).get(settingsHubPath)
		expectStatus(t, res, http.StatusOK)
		expectNotContains(t, res.Body, `href="/admin/publish"`, `href="/admin/settings/remote"`, `id="hub-operations"`)
	}
	c.useEnglish()
	english := c.get(settingsHubPath)
	expectContains(t, english.Body, `href="/admin/publish"`, ">Publishing</span>",
		"connect to a publish server, or publish a static site",
		"from external services and blogs (WordPress, etc.)")
}

func TestUnifiedSettingsSubnavigation(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	pages := []struct {
		path, current string
	}{
		{"/admin/settings/general", "/admin/settings/general"},
		{"/admin/settings/theme", "/admin/settings/theme"},
		{"/admin/settings/structure", "/admin/settings/theme"},
		{"/admin/publish", "/admin/publish"},
		// 公開サーバーの画面も「公開先」の一部なので、左メニューでは「公開先」を選択中にする。
		{"/admin/settings/remote", "/admin/publish"},
		{"/admin/settings/operations", "/admin/settings/operations"},
		// AI の3画面は「AI」1項目（/admin/settings/ai-integrations）にまとめ、どの画面でも選択中にする。
		{"/admin/settings/ai", "/admin/settings/ai-integrations"},
		{"/admin/settings/mcp", "/admin/settings/ai-integrations"},
		{"/admin/settings/ai-integrations", "/admin/settings/ai-integrations"},
		{"/admin/settings/backup", "/admin/settings/backup"},
		{"/admin/settings/migration", "/admin/settings/migration"},
		{"/admin/import", "/admin/settings/migration"},
	}
	for _, tc := range pages {
		res := c.get(tc.path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `class="mx-auto max-w-7xl px-4 py-6"`, `md:grid-cols-[15rem_minmax(0,1fr)]`, `class="min-w-0"`)
		start := strings.Index(res.Body, `<nav class="admin-settings-sidebar md:sticky md:top-20 self-start`)
		if start < 0 {
			t.Errorf("%s: settings sidebar not found", tc.path)
			continue
		}
		end := strings.Index(res.Body[start:], `</nav>`)
		if end < 0 {
			t.Errorf("%s: settings sub-navigation is not closed", tc.path)
			continue
		}
		nav := res.Body[start : start+end]
		// 公開サーバーと静的サイトは「公開先」1項目にまとめ、サイト運用は別の項目にする。「スマートフォンで書く」は削除した。
		for _, href := range []string{`/admin/settings/general`, `/admin/settings/theme`, `/admin/publish`,
			`/admin/settings/operations`, `/admin/settings/ai-integrations`, `/admin/settings/backup`, `/admin/settings/migration`} {
			if !strings.Contains(nav, `href="`+href+`"`) {
				t.Errorf("%s: settings navigation is missing %s", tc.path, href)
			}
		}
		if got := strings.Count(nav, `href="/admin/publish"`); got != 1 {
			t.Errorf("%s: settings navigation has %d publishing items, want 1", tc.path, got)
		}
		if got := strings.Count(nav, `aria-current="page"`); got != 1 {
			t.Errorf("%s: settings navigation marks %d items as current, want 1", tc.path, got)
		}
		expectNotContains(t, nav, `href="/admin/settings/mobile"`, `href="/admin/settings/remote"`, `href="/admin/settings/mcp"`, `href="/admin/settings/ai"`, ">公開サーバー<", "公開サーバー連携", "Public server connection")
		if !strings.Contains(nav, `href="`+tc.current+`" aria-current="page"`) {
			t.Errorf("%s: current settings page %s is not marked", tc.path, tc.current)
		}
	}
}

// 初回設定では公開先の種類を先に選び、入力欄と保存へ進める。
func TestPublishPageFirstSetupFlow(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	res := c.get("/admin/publish")
	expectStatus(t, res, http.StatusOK)
	expectNotContains(t, res.Body, `id="publish-guide"`, `id="publish-reflect-section"`)
	form := between(t, res.Body, `<form id="publish-form"`, `</form>`)
	if strings.Index(form, `aria-label="公開先の種類"`) > strings.Index(form, `data-kind-panel="server"`) {
		t.Fatal("first setup must present target choices before the input panels")
	}
	expectContains(t, form, "さくらのレンタルサーバー、エックスサーバー", "指定がある場合のみ入力（既定: 22）", "暗号化なし（非推奨）")
	expectContains(t, res.Body, "反映仕様")
	expectNotContains(t, form, `type="checkbox" name="cdn_purge_enabled"`)
	// どちらも未設定なら、公開方法の2択（① 公開サーバー ② 静的サイト）をフォームより前に出す。
	choice := between(t, res.Body, `<section id="publish-method-choice"`, `</section>`)
	expectContains(t, choice, ">公開方法を選ぶ</h2>",
		`href="/admin/settings/remote" id="publish-choice-remote" data-publish-choice="remote"`, "① 公開サーバーに接続する",
		`href="#publish-form" id="publish-choice-static" data-publish-choice="static"`, "② 静的サイトとして公開する")
	if strings.Index(res.Body, `id="publish-method-choice"`) > strings.Index(res.Body, `<form id="publish-form"`) {
		t.Error("the method choice must come before the static site form")
	}
	expectContains(t, form, "><span>② 静的サイトとして公開する: 公開先を選ぶ</span>")
	// 反映仕様は見出しの ? にまとめ、画面に長い説明の欄を出さない。
	expectContains(t, between(t, form, `id="publish-form-title"`, `</h2>`), `id="publish-rules" hidden`, "反映仕様", "初回反映では")
	expectNotContains(t, res.Body, `id="publish-rules-title"`, `id="publish-manage"`)
	expectNotContains(t, res.Body, `id="publish-current-method"`, `id="publish-remote-title"`, `id="publish-switch-to-remote"`)
}

// 公開先を設定済みなら、現在の方式とその入力欄だけを出し、ほかの方式への切り替えは閉じた折りたたみに収める。
func TestPublishPageShowsCurrentTargetOnly(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	dir := t.TempDir()
	saveFolderDestination(t, c, dir)
	page := c.get("/admin/publish").Body
	form := between(t, page, `<form id="publish-form"`, `</form>`)
	expectContains(t, page, `id="publish-reflect-section"`)
	expectContains(t, form, `現在の公開先`, `>フォルダに書き出す</p>`,
		`<details id="publish-target-switch" class="mt-6 border-t border-neutral-100 pt-5">`, `>別の公開先に切り替える</summary>`,
		`<div data-kind-panel="folder" class="mt-6 space-y-4">`, `<div data-kind-panel="cloudflare" class="mt-6 space-y-4" hidden>`)
	expectNotContains(t, form, `id="publish-guide"`, `>公開先の種類を選ぶ</summary>`)
	// 静的サイトを設定済みなら、先頭のタブで静的サイトが「動作中」であることを示す。選んでいるタブ（静的サイト）の内容だけを出し、
	// 未設定の公開サーバーのカード・導線は重ねて出さない。片方だけの設定なら、どちらが有効かの1行も出さない。
	tabs := between(t, page, `<nav id="publish-method-tabs"`, `</nav>`)
	expectContains(t, tabs, `id="publish-tab-static" class="publish-tab is-current" aria-current="page">静的サイト<span class="badge badge-static">動作中</span>`,
		`href="/admin/settings/remote" id="publish-tab-remote" class="publish-tab">公開サーバー</a>`)
	expectNotContains(t, page, `id="publish-method-status"`, `id="publish-method-banner"`, `data-method-card=`, `id="publish-method-setup-remote"`,
		"公開サーバーに接続する</a>", `id="publish-method-switch-`, `id="publish-method-clear-remote"`)
	// 設定の削除は、最初に目に入る位置に出さず、最下部の閉じた「接続・設定の管理」に収める。
	manage := between(t, page, `<details id="publish-manage" class="publish-manage">`, `</details>`)
	expectContains(t, manage, ">接続・設定の管理</summary>", `id="publish-method-clear-static"`, ">設定を削除する</button>", "data-confirm-danger",
		"公開先にあるファイルは消えません")
	if strings.Index(page, `id="publish-method-clear-static"`) < strings.Index(page, `<form id="publish-form"`) {
		t.Error("the delete button must not come before the settings form")
	}
	if n := strings.Count(page, `id="publish-method-clear-static"`); n != 1 {
		t.Errorf("delete button appears %d times, want 1", n)
	}
	expectNotContains(t, page, `id="publish-method-choice"`, `id="publish-remote-title"`, `id="publish-method-both"`, `id="publish-reflect-standby"`,
		`id="publish-rules-title"`)
}

// 公開サーバーの画面（/admin/settings/remote）も見出しは「公開先」にそろえ、公開サーバーへ送る画面であることを案内する。
// 先頭の「公開サーバー｜静的サイト」のタブで、静的サイトの設定へ移れる（主ボタンではない）。どちらも未設定なら、そう案内する。
func TestRemotePageGuide(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	res := c.get("/admin/settings/remote")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, ">公開先</h1>", "このPCで書いた記事を、インターネット上の公開サーバー（sorus 公開サーバー）へ送ります。",
		`id="publish-method-none" data-publish-method="none"`, "まだ公開方式が設定されていません。",
		`href="/admin/settings/remote" id="publish-tab-remote" class="publish-tab is-current" aria-current="page">公開サーバー</a>`,
		`href="/admin/publish?view=static" id="publish-tab-static" class="publish-tab">静的サイト</a>`)
	expectNotContains(t, res.Body, `id="remote-guide"`, `id="remote-publish-title"`, "静的サイト公開の設定を開く →", ">公開サーバー</h1>",
		`id="publish-method-banner"`)
	tabs := between(t, res.Body, `<nav id="publish-method-tabs"`, `</nav>`)
	expectNotContains(t, tabs, "btn-primary", "btn ")
	expectStatus(t, c.get("/admin/publish"), http.StatusOK)

	c.useEnglish()
	res = c.get("/admin/settings/remote")
	expectStatus(t, res, http.StatusOK)
	expectNotContains(t, res.Body, `id="remote-guide"`, "How this differs from static site publishing")
	expectContains(t, res.Body, ">Publishing</h1>", `href="/admin/publish?view=static" id="publish-tab-static" class="publish-tab">Static site</a>`,
		"No publishing method is set up yet.")
	expectNotContains(t, res.Body, "まだ公開方式が設定されていません。")
}

// 公開サーバーに接続済みなら、公開サーバーのタブに「動作中」を出し、いまの接続（接続先）を最初に見せる。接続の解除は最下部の
// 閉じた「接続・設定の管理」に収める。静的サイトの設定（?view=static）を開いたときも、タブで公開サーバーが動作中であることを示し、
// 未設定の静的サイトの画面には公開サーバーのカードを重ねない。
func TestPublishMethodStatusWhenRemoteConnected(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	if err := env.server.saveRemoteConfig(remoteConfig{URL: "https://media.example.jp/api/sync", Token: "configured-token"}); err != nil {
		t.Fatal(err)
	}
	res := c.get("/admin/settings/remote")
	expectStatus(t, res, http.StatusOK)
	tabs := between(t, res.Body, `<nav id="publish-method-tabs"`, `</nav>`)
	expectContains(t, tabs, `id="publish-tab-remote" class="publish-tab is-current" aria-current="page">公開サーバー<span class="badge badge-remote">動作中</span>`,
		`id="publish-tab-static" class="publish-tab">静的サイト</a>`)
	expectContains(t, between(t, res.Body, `<section id="remote-connection"`, `</section>`), "media.example.jp")
	expectNotContains(t, res.Body, `id="publish-method-status"`, `id="publish-method-setup-static"`, "静的サイトを設定する", `data-method-card=`,
		`id="publish-method-clear-static"`)
	manage := afterMarker(t, res.Body, `<details id="remote-manage" class="publish-manage"`)
	expectContains(t, manage, `id="publish-method-clear-remote"`, ">接続を解除する</button>", `id="remote-join-form"`, ">別の招待コードで参加し直す</h2>")
	if strings.Index(res.Body, `id="publish-method-clear-remote"`) < strings.Index(res.Body, `<details id="remote-manage"`) {
		t.Error("the disconnect button must be inside the connection management section at the bottom")
	}
	nav := between(t, res.Body, `<nav class="admin-settings-sidebar`, `</nav>`)
	expectContains(t, nav, `href="/admin/publish" aria-current="page"`)

	// 静的サイトの設定（?view=static）を開いたときも、タブで公開サーバーが動作中であることを示し、タブで公開サーバーの画面へ戻れる。
	static := c.get("/admin/publish?view=static")
	expectStatus(t, static, http.StatusOK)
	expectContains(t, static.Body, `href="/admin/settings/remote" id="publish-tab-remote" class="publish-tab">公開サーバー<span class="badge badge-remote">動作中</span></a>`,
		`id="publish-tab-static" class="publish-tab is-current" aria-current="page">静的サイト</a>`, `id="publish-form"`)
	expectNotContains(t, static.Body, `id="publish-method-choice"`, `id="publish-method-status"`, `data-method-card=`, `id="publish-manage"`,
		`id="publish-method-clear-remote"`, `id="remote-connection"`)

	c.useEnglish()
	res = c.get("/admin/settings/remote")
	expectContains(t, res.Body, `>Publish server<span class="badge badge-remote">Active</span>`, ">Connection and settings</summary>", ">Disconnect</button>")
	expectNotContains(t, between(t, res.Body, `<nav id="publish-method-tabs"`, `</nav>`), "動作中", "公開サーバー")
}

// 静的サイトを設定済みで公開サーバーに未接続なら、公開サーバーのタブの画面では、静的サイトのタブに「動作中」を出すだけにし、
// 参加のフォーム（主な操作）を最初に見せる。静的サイトのカード・設定の削除は重ねて出さない。
func TestRemotePageShowsStaticMethod(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, t.TempDir())
	res := c.get("/admin/settings/remote")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `href="/admin/publish?view=static" id="publish-tab-static" class="publish-tab">静的サイト<span class="badge badge-static">動作中</span></a>`,
		`<section id="remote-join" class="mb-6 `, ">公開サーバーに参加する</h2>")
	expectNotContains(t, res.Body, `id="publish-method-status"`, `data-method-card=`, `id="publish-method-setup-remote"`, "フォルダ（",
		`id="publish-method-clear-static"`, `id="remote-manage"`, `id="publish-method-none"`)
}

func TestPublishPageRedirectsToConfiguredRemoteServer(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	if err := env.server.saveRemoteConfig(remoteConfig{URL: "https://media.example.jp", Token: "configured-token"}); err != nil {
		t.Fatal(err)
	}

	checkRedirect := c.http.CheckRedirect
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { c.http.CheckRedirect = checkRedirect }()

	res := c.get("/admin/publish")
	expectStatus(t, res, http.StatusSeeOther)
	if got := res.Header.Get("Location"); got != "/admin/settings/remote" {
		t.Errorf("redirect location = %q, want /admin/settings/remote", got)
	}

	static := c.get("/admin/publish?view=static")
	expectStatus(t, static, http.StatusOK)
	expectContains(t, static.Body, `id="publish-form"`)
}

// between は body の start から、そのあとで最初に現れる end までを返す。
func between(t *testing.T, body, start, end string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, start)
	if !ok {
		t.Fatalf("%q not found\nbody: %.3000s", start, body)
	}
	part, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatalf("%q not found after %q", end, start)
	}
	return part
}

// afterMarker は body のうち marker から後ろ（marker を含む）を返す。入れ子の折りたたみを含む区画を調べるのに使う。
func afterMarker(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("%q not found", marker)
	}
	return body[i:]
}
