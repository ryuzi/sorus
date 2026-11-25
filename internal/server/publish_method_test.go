package server

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"sorus/internal/auth"
)

// 保存した公開方式が無いときは、今までと同じく公開サーバー（接続済みなら）→静的サイトの順に決める。保存した方式は、
// その方式が設定済みのときだけ使う。
func TestResolvePublishMethod(t *testing.T) {
	for _, tc := range []struct {
		stored         string
		remote, static bool
		want           string
	}{
		{"", false, false, ""},
		{"", true, false, publishMethodRemote},
		{"", false, true, publishMethodStatic},
		{"", true, true, publishMethodRemote},
		{publishMethodStatic, true, true, publishMethodStatic},
		{publishMethodRemote, true, true, publishMethodRemote},
		{publishMethodStatic, true, false, publishMethodRemote},
		{publishMethodRemote, false, true, publishMethodStatic},
		{publishMethodRemote, false, false, ""},
		{"bogus", true, true, publishMethodRemote},
	} {
		if got := resolvePublishMethod(tc.stored, tc.remote, tc.static); got != tc.want {
			t.Errorf("resolvePublishMethod(%q, remote=%v, static=%v) = %q, want %q", tc.stored, tc.remote, tc.static, got, tc.want)
		}
	}
}

// 公開サーバーと静的サイトの両方を設定済みなら、使うのは1つだけ（既定は公開サーバー）。待機中の静的サイトへは「サイトに反映」
// も記事の状態のパネルも動かさない。方式バナーの「この方式に切り替える」で切り替えると、もう一方が待機中になる。
func TestPublishMethodSwitchAndStandby(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, t.TempDir())
	if err := env.server.saveRemoteConfig(remoteConfig{URL: "https://media.example.jp/api/sync", Token: "configured-token"}); err != nil {
		t.Fatal(err)
	}
	if got := env.server.activePublishMethod(); got != publishMethodRemote {
		t.Fatalf("active method with both configured = %q, want remote (compatible default)", got)
	}

	// 両方を設定済みなら、どちらが有効かを先頭に1行で示す。動作中の方式のタブ（公開サーバー）には切り替えのボタンを出さず、
	// 待機中の方式（静的サイト）のカード・設定の削除も重ねて出さない（選んでいるタブの内容だけ）。
	remotePage := c.get("/admin/settings/remote")
	expectStatus(t, remotePage, http.StatusOK)
	status := between(t, remotePage.Body, `<div id="publish-method-status"`, `</div>`)
	expectContains(t, status, `data-publish-method="remote"`, `role="status"`, `aria-label="現在の公開方式"`,
		"現在：公開サーバーが有効です（静的サイトは待機中）", "二重に公開されることはありません")
	expectNotContains(t, remotePage.Body, `id="publish-method-switch-`, `id="publish-method-clear-static"`, `data-method-card=`, `id="publish-method-both"`)
	expectContains(t, afterMarker(t, remotePage.Body, `<details id="remote-manage"`), `id="publish-method-clear-remote"`)
	tabs := between(t, remotePage.Body, `<nav id="publish-method-tabs"`, `</nav>`)
	expectContains(t, tabs, `<span class="badge badge-remote">動作中</span>`, `静的サイト<span class="badge badge-standby">待機中</span>`)

	// 静的サイトの画面（待機中）では、同じ1行から静的サイトへ切り替えられる。「サイトに反映」は押せず、待機中であることを知らせる。
	staticPage := c.get("/admin/publish?view=static")
	expectStatus(t, staticPage, http.StatusOK)
	status = between(t, staticPage.Body, `<div id="publish-method-status"`, `</form>`)
	expectContains(t, status, "現在：公開サーバーが有効です（静的サイトは待機中）",
		`<form method="post" action="/admin/publish/method" class="shrink-0" data-confirm="これからは記事を公開すると静的サイトへ反映します。`,
		`<input type="hidden" name="method" value="static">`, `<input type="hidden" name="return" value="static">`,
		`id="publish-method-switch-static" class="btn btn-secondary btn-sm">静的サイトに切り替える</button>`)
	expectNotContains(t, staticPage.Body, `id="publish-method-clear-remote"`, `id="publish-method-switch-remote"`)
	expectContains(t, between(t, staticPage.Body, `<details id="publish-manage" class="publish-manage">`, `</details>`), `id="publish-method-clear-static"`)
	expectContains(t, staticPage.Body, `id="publish-reflect-standby"`, `<button type="button" id="publish-reflect-button" disabled`, "静的サイトに切り替える」を押してください")
	expectNotContains(t, staticPage.Body, `action="/admin/publish/reflect"`)
	expectRedirect(t, c.postForm("/admin/publish/reflect", nil), "/admin/publish?view=static&flash=publish_static_standby")
	res := c.postAPI("/admin/publish/reflect", "", nil)
	expectStatus(t, res, http.StatusConflict)
	expectContains(t, res.Body, "静的サイトは待機中のため反映していません")
	expectStatus(t, c.get("/admin/publish/panel"), http.StatusNoContent)
	if st := env.server.publishState(); st.running {
		t.Fatal("standby static site must not start a reflect")
	}

	// 静的サイトへ切り替える。保存した方式が公開サーバーより優先され、公開先の画面も公開サーバーへ移らなくなる。
	expectRedirect(t, c.postForm("/admin/publish/method", url.Values{"method": {"static"}, "return": {"remote"}}),
		"/admin/settings/remote?flash=publish_method_switched_static")
	if got := env.server.activePublishMethod(); got != publishMethodStatic {
		t.Fatalf("active method after switching = %q, want static", got)
	}
	expectStatus(t, c.get("/admin/publish"), http.StatusOK)
	remotePage = c.get("/admin/settings/remote")
	status = between(t, remotePage.Body, `<div id="publish-method-status"`, `</form>`)
	expectContains(t, status, `data-publish-method="static"`, "現在：静的サイトが有効です（公開サーバーは待機中）",
		`<input type="hidden" name="method" value="remote">`, `id="publish-method-switch-remote" class="btn btn-secondary btn-sm">公開サーバーに切り替える</button>`)
	expectContains(t, between(t, remotePage.Body, `<nav id="publish-method-tabs"`, `</nav>`),
		`公開サーバー<span class="badge badge-standby">待機中</span>`, `静的サイト<span class="badge badge-static">動作中</span>`)
	expectNotContains(t, c.get("/admin/publish").Body, `id="publish-method-switch-`)
	staticPage = c.get("/admin/publish")
	expectContains(t, staticPage.Body, `action="/admin/publish/reflect"`)
	expectNotContains(t, staticPage.Body, `id="publish-reflect-standby"`)
	if res := c.get("/admin/publish/panel"); res.StatusCode == http.StatusNoContent {
		t.Error("active static site: panel = 204, want the panel")
	}

	// 公開サーバーへ戻す。
	expectRedirect(t, c.postForm("/admin/publish/method", url.Values{"method": {"remote"}, "return": {"static"}}),
		"/admin/publish?flash=publish_method_switched_remote&view=static")
	if got := env.server.activePublishMethod(); got != publishMethodRemote {
		t.Fatalf("active method after switching back = %q, want remote", got)
	}

	// 設定していない方式・知らない方式へは切り替えない。
	if err := env.server.saveRemoteConfig(remoteConfig{}); err != nil {
		t.Fatal(err)
	}
	expectRedirect(t, c.postForm("/admin/publish/method", url.Values{"method": {"remote"}, "return": {"remote"}}),
		"/admin/settings/remote?flash=publish_method_unavailable")
	expectStatus(t, c.postForm("/admin/publish/method", url.Values{"method": {"ftp"}}), http.StatusBadRequest)

	// 切り替え・解除は管理者だけ。
	ed := env.loggedIn(chief)
	if res := ed.postForm("/admin/publish/method", url.Values{"method": {"static"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("editor switch = %d, want 403", res.StatusCode)
	}
	if res := ed.postForm("/admin/publish/method/clear", url.Values{"method": {"static"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("editor clear = %d, want 403", res.StatusCode)
	}
}

// 「設定解除」は、静的サイトなら公開先の設定（パスワードなど）を消し、公開サーバーなら接続を切る。保存した方式が解除した方式なら、
// 残っている方式へ移る。
func TestPublishMethodClear(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, t.TempDir())
	if err := env.server.saveRemoteConfig(remoteConfig{URL: "https://media.example.jp/api/sync", Token: "configured-token"}); err != nil {
		t.Fatal(err)
	}
	expectRedirect(t, c.postForm("/admin/publish/method", url.Values{"method": {"static"}}), "/admin/publish")
	if got := env.server.storedPublishMethod(); got != publishMethodStatic {
		t.Fatalf("stored method = %q", got)
	}

	expectRedirect(t, c.postForm("/admin/publish/method/clear", url.Values{"method": {"static"}}),
		"/admin/settings/remote?flash=publish_method_cleared_static")
	if env.server.loadPublishConfig().configured() {
		t.Error("static destination is still configured after clearing it")
	}
	if got := env.server.storedPublishMethod(); got != publishMethodRemote {
		t.Errorf("stored method after clearing static = %q, want remote", got)
	}
	if got := env.server.activePublishMethod(); got != publishMethodRemote {
		t.Errorf("active method after clearing static = %q, want remote", got)
	}

	expectRedirect(t, c.postForm("/admin/publish/method/clear", url.Values{"method": {"remote"}}),
		"/admin/publish?flash=publish_method_cleared_remote&view=static")
	if env.server.loadRemoteConfig().connected() {
		t.Error("remote is still connected after clearing it")
	}
	if got := env.server.storedPublishMethod(); got != "" {
		t.Errorf("stored method with nothing configured = %q, want empty", got)
	}
	expectStatus(t, c.postForm("/admin/publish/method/clear", url.Values{"method": {"zip"}}), http.StatusBadRequest)
}

// 両方を設定していても、記事を公開したときに送るのは「動作中」の方式だけ（公開サーバーと静的サイトへ二重に公開しない）。
func TestArticlePublishGoesOnlyToActiveMethod(t *testing.T) {
	env := newRemoteEnv(t)
	adm := env.local.loggedIn(admin)
	saveFolderDestination(t, adm, t.TempDir())
	env.configure("editor", auth.RoleEditor)
	ed := env.local.loggedIn(chief)
	// 静的サイトへの反映は待ち時間のあとに始まる。この試験のあいだは始めず、予約したか（タイマー）だけを見る。
	shortenReflectDebounce(env.local.server, time.Hour)

	// 既定（公開サーバー）: 公開サーバーへ送り、静的サイトへの反映は予約しない。
	loc := expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("公開サーバーへ", "to-remote", "publish")), "/admin/articles/")
	expectFlash(t, loc, "remote_published")
	id := editPathPattern.FindStringSubmatch(loc)[1]
	env.remote.mustGetArticle(id)
	if reflects, pending := reflectState(env.local.server); reflects != 0 || pending {
		t.Errorf("remote active: static reflects=%d pending=%v, want none", reflects, pending)
	}

	// 静的サイトへ切り替えると、静的サイトへの反映を予約し、公開サーバーへは送らない。
	expectRedirect(t, adm.postForm("/admin/publish/method", url.Values{"method": {"static"}}), "/admin/publish")
	loc = expectRedirect(t, ed.postForm("/admin/articles/new", articleFormFor("静的サイトへ", "to-static", "publish")), "/admin/articles/")
	expectFlash(t, loc, flashPublishedReflecting)
	id = editPathPattern.FindStringSubmatch(loc)[1]
	if got := env.remote.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, id); got != 0 {
		t.Errorf("static active: article reached the publish server (%d rows)", got)
	}
	if _, pending := reflectState(env.local.server); !pending {
		t.Error("static active: static reflect was not scheduled")
	}

	// 公開サーバーへ戻すと、待っていた静的サイトへの反映を取り消す。
	expectRedirect(t, adm.postForm("/admin/publish/method", url.Values{"method": {"remote"}}), "/admin/publish")
	if reflects, pending := reflectState(env.local.server); reflects != 0 || pending {
		t.Errorf("after switching to remote: static reflects=%d pending=%v, want the scheduled reflect cancelled", reflects, pending)
	}
}
