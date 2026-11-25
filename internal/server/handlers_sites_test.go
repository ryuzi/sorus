package server

import (
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeSites はテスト用の Sites 実装。
type fakeSites struct {
	sites      []SiteInfo
	created    SiteInfo
	createErr  error
	createCall int
	backupPath string
	deleteErr  error
	deletedIDs []string
	forgotten  []string
	forgetErr  error
}

func (f *fakeSites) List() []SiteInfo {
	return slices.Clone(f.sites)
}

func (f *fakeSites) Create() (SiteInfo, error) {
	f.createCall++
	if f.createErr != nil {
		return SiteInfo{}, f.createErr
	}
	return f.created, nil
}

func (f *fakeSites) Delete(id string) (string, error) {
	f.deletedIDs = append(f.deletedIDs, id)
	if f.deleteErr != nil {
		return "", f.deleteErr
	}
	return f.backupPath, nil
}

func (f *fakeSites) Forget(id string) error {
	f.forgotten = append(f.forgotten, id)
	return f.forgetErr
}

func newSitesDesktopClient(t *testing.T, sites Sites, siteKey string) (*testEnv, *client) {
	t.Helper()
	env := newDesktopSiteEnvWith(t, Config{
		Sites:   sites,
		SiteKey: siteKey,
		LogPath: "/tmp/sorus-test-data/sorus.log",
	})
	c := env.newClient()
	csrfName := env.server.cookies.CSRF
	expectStatus(t, c.get(SetupPath), http.StatusOK)
	form := setupForm()
	form.Set(CSRFFormField, c.cookie(csrfName))
	expectRedirect(t, c.postRaw(SetupPath, form), "/admin")
	return env, c
}

func TestSitesSettingsHubCardVisibility(t *testing.T) {
	// Config.Sites が nil のとき: カードは出ず、ルートは 404 になる。
	nilSitesEnv := newTestEnv(t)
	nilAdmin := nilSitesEnv.loggedIn(admin)
	hub := nilAdmin.get(settingsHubPath)
	expectStatus(t, hub, http.StatusOK)
	if strings.Contains(hub.Body, sitesPagePath) || strings.Contains(hub.Body, `id="hub-sites"`) {
		t.Fatal("settings hub shows the sites card when Config.Sites is nil")
	}
	expectStatus(t, nilAdmin.get(sitesPagePath), http.StatusNotFound)
	expectStatus(t, nilAdmin.postForm(sitesPagePath, nil), http.StatusNotFound)
	expectStatus(t, nilAdmin.get(sitesDeletePath), http.StatusNotFound)
	expectStatus(t, nilAdmin.postForm(sitesDeletePath, url.Values{"site_name": {"学生新聞"}}), http.StatusNotFound)

	// Config.Sites があるとき: 管理者以外の役割にはカードを出さず、ルートは 403 になる。
	fake := &fakeSites{
		sites: []SiteInfo{{ID: "sitea", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Current: true}},
	}
	roleEnv := newPagesEnv(t, Config{Sites: fake})
	for _, tc := range []struct {
		name string
		user testUser
	}{
		{"editor", chief},
		{"writer", studentA},
	} {
		c := roleEnv.loggedIn(tc.user)
		res := c.get(settingsHubPath)
		expectStatus(t, res, http.StatusOK)
		if strings.Contains(res.Body, sitesPagePath) || strings.Contains(res.Body, `id="hub-sites"`) {
			t.Errorf("%s sees the sites card in settings hub", tc.name)
		}
		expectStatus(t, c.get(sitesPagePath), http.StatusForbidden)
	}

	// Config.Sites があり管理者のとき: 共通の6カテゴリのあとに「サイト」のカードが出る。
	adm := roleEnv.loggedIn(admin)
	cards, _ := settingsHubCards(t, adm.get(settingsHubPath).Body)
	sitesIdx := slices.Index(cards, "/admin/settings/sites サイト")
	backupIdx := slices.Index(cards, "/admin/settings/backup バックアップと復元")
	if sitesIdx < 0 {
		t.Fatalf("admin hub cards = %v, want /admin/settings/sites サイト", cards)
	}
	if backupIdx < 0 || sitesIdx <= backupIdx {
		t.Errorf("sites card index = %d, backup card index = %d; want sites after the six shared categories (%v)", sitesIdx, backupIdx, cards)
	}
}

func TestSitesPageSingleSiteAndMultiSiteList(t *testing.T) {
	createdAt := time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local)

	// 1サイトだけのとき: 削除ボタンは一切出さない。
	single := &fakeSites{
		sites: []SiteInfo{
			{ID: "", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true},
		},
	}
	_, singleClient := newSitesDesktopClient(t, single, "")
	res := singleClient.get(sitesPagePath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"学生新聞",
		"このサイトのアドレス",
		"http://127.0.0.1:8780/admin",
		"開く",
		`data-current-site="true"`,
		"いま開いているサイト",
		"新しいサイトを作る",
		// 新しいサイトは同じウィンドウで開く（アプリのウィンドウでは、新しいタブが普通のブラウザのタブになる）。
		`<form method="post" action="/admin/settings/sites" class="mt-4">`,
		"この画面で「はじめに」が開き",
		// ほかの設定画面と同じ左メニュー付きの2ペイン（§2-3）。
		`<nav class="admin-settings-sidebar`, `md:grid-cols-[15rem_minmax(0,1fr)]`,
	)
	expectNotContainsAny(t, "single-site page", withoutSettingsNav(withoutJSDictionary(res.Body)),
		"このサイトを削除",
		sitesDeletePath,
		`/delete"`,
		"ポート",
		"インスタンス",
		"サーバー",
	)

	// 複数サイト（名前つき・名前の無いサイト・開けなかったサイト）の一覧表示。
	multi := &fakeSites{
		sites: []SiteInfo{
			{ID: "site1", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true},
			{ID: "site2", Name: "", AdminURL: "http://127.0.0.1:8781/admin", Created: createdAt},
			{ID: "site3", Name: "壊れたサイト", Created: createdAt, OpenError: "database disk image is malformed"},
		},
	}
	_, multiClient := newSitesDesktopClient(t, multi, "")
	res = multiClient.get(sitesPagePath)
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"学生新聞",
		"名前の無いサイト（2026年10月29日）",
		`data-current-site="true"`,
		"このサイトのアドレス",
		"http://127.0.0.1:8780/admin",
		"http://127.0.0.1:8781/admin",
		"開けませんでした",
		"database disk image is malformed",
		"/tmp/sorus-test-data/sorus.log",
		`href="/admin/settings/sites/delete"`,
		"このサイトを削除",
		`action="/admin/settings/sites/site2/delete"`,
	)
	expectNotContainsAny(t, "multi-site page wording", withoutSettingsNav(withoutJSDictionary(res.Body)), "ポート", "インスタンス", "サーバー")

	// 英語表示の切り替えも確かめる。
	multiClient.useEnglish()
	enBody := multiClient.get(sitesPagePath).Body
	expectContains(t, enBody,
		"Unnamed site (2026-10-29)",
		"Current site",
		"Address of this site",
		"Could not open",
		"Create a new site",
		"Delete this site",
	)
}

func TestSiteCreateRedirectAndError(t *testing.T) {
	fake := &fakeSites{
		sites: []SiteInfo{
			{ID: "site1", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Current: true},
		},
		created: SiteInfo{
			ID:       "site2",
			AdminURL: "http://127.0.0.1:8781/admin",
			Created:  time.Now(),
		},
	}
	_, c := newSitesDesktopClient(t, fake, "")

	loc := expectRedirect(t, c.postForm(sitesPagePath, nil), "http://127.0.0.1:8781/admin")
	if loc != "http://127.0.0.1:8781/admin" {
		t.Errorf("Location = %q, want http://127.0.0.1:8781/admin", loc)
	}
	if fake.createCall != 1 {
		t.Errorf("Create called %d times, want 1", fake.createCall)
	}

	fake.createErr = errors.New("新しいサイトを作れませんでした: disk full")
	res := c.postForm(sitesPagePath, nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "新しいサイトを作れませんでした: disk full")
}

func TestSiteDeleteCurrentAndUnnamedAndCSRF(t *testing.T) {
	createdAt := time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local)
	fake := &fakeSites{
		sites: []SiteInfo{
			{ID: "site1", Name: "第一サイト", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt},
			{ID: "site2", Name: "学生新聞", AdminURL: "http://127.0.0.1:8781/admin", Created: createdAt, Current: true},
			{ID: "site3", Name: "", AdminURL: "http://127.0.0.1:8782/admin", Created: createdAt},
		},
		backupPath: "/tmp/sorus-backups/sorus_deleted_site_site2_20261029_120000.zip",
	}
	env, c := newSitesDesktopClient(t, fake, "")

	// すべての POST に CSRF トークンが必要。
	for _, path := range []string{sitesPagePath, sitesDeletePath, sitesPagePath + "/site3/delete", sitesPagePath + "/site3/forget"} {
		expectStatus(t, c.postRaw(path, url.Values{"site_name": {"学生新聞"}}), http.StatusForbidden)
	}
	if fake.createCall != 0 || len(fake.deletedIDs) != 0 {
		t.Fatalf("CSRF-less POST mutated state: create=%d, deleted=%v", fake.createCall, fake.deletedIDs)
	}

	// 削除確認画面（GET）: 消えるもの・バックアップ先（自動バックアップ未設定時は「削除したサイトのバックアップ」）・復元方法を示す。
	confirmPage := c.get(sitesDeletePath)
	expectStatus(t, confirmPage, http.StatusOK)
	expectContains(t, confirmPage.Body,
		"記事・画像・添付・設定・公開先の設定・招待",
		"公開先にすでに公開したサイトは消えません",
		"削除したサイトのバックアップ",
		"バックアップから始める",
		"独自テーマのファイルはバックアップに含まれません。",
		"削除する前に、取り込み元の URL または Zip を保管してください。",
		"新しいサイトに復元したあと、そのサイトでテーマが見つからない場合は、保管した URL または Zip から再登録してください。",
		`data-expected-name="学生新聞"`,
		`id="site-delete-submit"`,
	)
	c.get("/admin/set-lang?lang=en&return=/admin")
	enConfirm := c.get(sitesDeletePath)
	expectContains(t, enConfirm.Body, "Custom theme files are not included in this backup.", "Before deleting this site, keep the source URL or Zip file.", "After restoring to a new site, if the theme is missing on that site, add it again from the saved URL or Zip file.")
	c.get("/admin/set-lang?lang=ja&return=/admin")
	// JavaScript が無い環境でも使えるよう、HTML の初期属性としては disabled を付けない。
	if strings.Contains(confirmPage.Body, `id="site-delete-submit" disabled`) {
		t.Error("delete submit button is permanently disabled without JavaScript")
	}

	// 自動バックアップ先が設定されていれば、確認画面にはそのフォルダを出す。
	autoFolder := filepath.Join(t.TempDir(), "AutoBackups")
	if err := env.server.putSetting(autoBackupFolderSetting, autoFolder); err != nil {
		t.Fatal(err)
	}
	expectContains(t, c.get(sitesDeletePath).Body, autoFolder)

	// 自動バックアップ先が削除で消える範囲（このサーバーでは uploads/）の中なら、確認画面は実際に使う
	// 「削除したサイトのバックアップ」を出す。
	inside := env.uploadDir
	if err := env.server.putSetting(autoBackupFolderSetting, inside); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(filepath.Dir(env.uploadDir), "削除したサイトのバックアップ")
	moved := c.get(sitesDeletePath)
	expectContains(t, moved.Body, fallback)
	_, rest, ok := strings.Cut(moved.Body, `id="site-delete-backup-folder"`)
	if !ok {
		t.Fatal("confirm page has no backup folder")
	}
	folder, _, _ := strings.Cut(rest, "</p>")
	if strings.Contains(folder, inside) {
		t.Errorf("confirm page offers a folder inside the data that deletion removes: %s", folder)
	}
	if err := env.server.putSetting(autoBackupFolderSetting, autoFolder); err != nil {
		t.Fatal(err)
	}

	// サイト名が違う（または空の）ときは断り、Delete は呼ばない。
	for _, wrong := range []string{"", "第一サイト", "学生新聞 "} {
		res := c.postForm(sitesDeletePath, url.Values{"site_name": {wrong}})
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "サイト名が一致しません")
		if len(fake.deletedIDs) != 0 {
			t.Fatalf("Delete called with wrong name %q: %v", wrong, fake.deletedIDs)
		}
	}

	// Delete がエラーを返したときは、理由を表示してサイトを残す。
	fake.deleteErr = errors.New("削除の前のバックアップを作れなかったため、削除しませんでした: no space left")
	errRes := c.postForm(sitesDeletePath, url.Values{"site_name": {"学生新聞"}})
	expectStatus(t, errRes, http.StatusUnprocessableEntity)
	expectContains(t, errRes.Body, "削除の前のバックアップを作れなかったため、削除しませんでした")
	if !slices.Equal(fake.deletedIDs, []string{"site2"}) {
		t.Fatalf("deletedIDs after failed delete = %v, want [site2]", fake.deletedIDs)
	}

	// 名前の無い別サイト（site3）は確認だけで削除でき、一覧へリダイレクトする。
	fake.deleteErr = nil
	fake.deletedIDs = nil
	expectRedirect(t, c.postForm(sitesPagePath+"/site3/delete", nil), sitesPagePath+"?flash=site_deleted")
	if !slices.Equal(fake.deletedIDs, []string{"site3"}) {
		t.Fatalf("deletedIDs after unnamed site delete = %v, want [site3]", fake.deletedIDs)
	}
	expectContains(t, c.get(sitesPagePath+"?flash=site_deleted").Body, "サイトを削除しました")

	// 名前のある別サイト（site1）を他サイトから削除しようとしても断り、Delete は呼ばない。
	fake.deletedIDs = nil
	refused := c.postForm(sitesPagePath+"/site1/delete", nil)
	expectStatus(t, refused, http.StatusForbidden)
	expectContains(t, refused.Body, "そのサイトにログインしてから削除してください")
	if len(fake.deletedIDs) != 0 {
		t.Fatalf("Delete called for named other site: %v", fake.deletedIDs)
	}

	// 正しいサイト名を入力したときは Delete(currentID) を呼び、自己完結した完了画面を返す。
	done := c.postForm(sitesDeletePath, url.Values{"site_name": {"学生新聞"}})
	expectStatus(t, done, http.StatusOK)
	if !slices.Equal(fake.deletedIDs, []string{"site2"}) {
		t.Fatalf("deletedIDs = %v, want [site2]", fake.deletedIDs)
	}
	if strings.Contains(done.Body, "/static") {
		t.Fatalf("completion page references /static:\n%s", done.Body)
	}
	expectContains(t, done.Body,
		fake.backupPath,
		"バックアップから始める",
		"http://127.0.0.1:8780/admin",
		"第一サイト を開く",
	)
}

func TestHeaderSiteMenuAndQuitConfirmText(t *testing.T) {
	createdAt := time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local)

	// 1サイトのとき: ヘッダーのサイト名はメニューにならず、Config.Sites == nil のときとバイト単位で同じ描画になる。
	// サーバーごとに違う CSRF のトークンと開いている画面の合言葉は、置き換えて比べる。
	nilEnv, nilClient := newSitesDesktopClient(t, nil, "")
	singleFake := &fakeSites{
		sites: []SiteInfo{
			{ID: "", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true},
		},
	}
	singleEnv, singleClient := newSitesDesktopClient(t, singleFake, "")

	nilBody := strings.NewReplacer(nilClient.csrfToken(), "CSRF_TOKEN", nilEnv.server.windowState().token, "WINDOW_TOKEN").
		Replace(nilClient.get("/admin").Body)
	singleBody := singleClient.get("/admin").Body
	if got := strings.NewReplacer(singleClient.csrfToken(), "CSRF_TOKEN", singleEnv.server.windowState().token, "WINDOW_TOKEN").
		Replace(singleBody); got != nilBody {
		t.Fatalf("1-site /admin rendering differs from nil-Sites rendering")
	}
	if strings.Contains(singleBody, `id="site-menu"`) || strings.Contains(singleBody, "サイトの一覧と追加") {
		t.Fatal("header shows the site menu when there is only 1 site")
	}
	expectContains(t, singleBody,
		`<a href="/admin" class="min-w-0 truncate font-bold text-neutral-900 md:max-w-[12rem] md:shrink-0 lg:max-w-xs">学生新聞</a>`,
		`data-confirm="sorus を終了しますか？"`,
	)

	// 2サイト以上のとき: ヘッダーのサイト名が details/summary のメニュー（▼ つき）になり、開けた全サイトと
	// 「サイトの一覧と追加」が並び、終了の確認文が「すべてのサイトを閉じて、sorus を終了しますか？」に変わる。
	multiFake := &fakeSites{
		sites: []SiteInfo{
			{ID: "site1", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true},
			{ID: "site2", Name: "", AdminURL: "http://127.0.0.1:8781/admin", Created: createdAt},
			{ID: "site3", Name: "壊れたサイト", Created: createdAt, OpenError: "corrupt db"},
		},
	}
	_, multiClient := newSitesDesktopClient(t, multiFake, "")
	multiBody := multiClient.get("/admin").Body
	_, rest, ok := strings.Cut(multiBody, `<details id="site-menu"`)
	if !ok {
		t.Fatal("site-menu details not found with 2+ sites")
	}
	siteMenu, _, _ := strings.Cut(rest, "</details>")
	expectContains(t, siteMenu,
		"<summary",
		"▼",
		`href="http://127.0.0.1:8780/admin"`,
		`aria-current="true">学生新聞</a>`,
		`href="http://127.0.0.1:8781/admin"`,
		"名前の無いサイト（2026年10月29日）</a>",
		`href="/admin/settings/sites"`,
		"サイトの一覧と追加",
	)
	if strings.Contains(siteMenu, "壊れたサイト") {
		t.Error("header site menu lists a site that failed to open")
	}
	expectContains(t, multiBody, `data-confirm="すべてのサイトを閉じて、sorus を終了しますか？"`)

	// 英語での終了確認文とヘッダーメニュー。
	multiClient.useEnglish()
	enBody := multiClient.get("/admin").Body
	expectContains(t, enBody,
		`data-confirm="Close all sites and quit sorus?"`,
		"Unnamed site (2026-10-29)",
		"All sites and new site",
	)
}

func TestUnnamedDeleteRefusesNameReadError(t *testing.T) {
	createdAt := time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local)
	fake := &fakeSites{
		sites: []SiteInfo{
			{ID: "site1", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true},
			{ID: "site2", Name: "", NameError: "SQL logic error: no such table: users (1)", AdminURL: "http://127.0.0.1:8781/admin", Created: createdAt},
		},
	}
	_, c := newSitesDesktopClient(t, fake, "")
	page := c.get(sitesPagePath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body,
		"サイトの情報を読めませんでした",
		"http://127.0.0.1:8781/admin",
		"開く",
		"SQL logic error: no such table: users (1)",
	)
	if strings.Contains(page.Body, `/admin/settings/sites/site2/delete`) {
		t.Fatalf("sites page offers unnamed delete when the name could not be read:\n%s", page.Body)
	}
	if strings.Contains(page.Body, "「はじめに」がまだ済んでいません") {
		t.Error("a name read error is shown as setup not finished")
	}
	if strings.Contains(page.Body, `/admin/settings/sites/site2/forget`) {
		t.Error("a site that is open but unreadable offers removal from the list")
	}
	res := c.postForm(sitesPagePath+"/site2/delete", nil)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "サイトの情報を読めませんでした")
	if len(fake.deletedIDs) != 0 {
		t.Fatalf("Delete called for a site whose name could not be read: %v", fake.deletedIDs)
	}
}

func TestForgetFailedSiteKeepsTheFolder(t *testing.T) {
	createdAt := time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local)
	const folder = "/data/sites/site3"
	fake := &fakeSites{
		sites: []SiteInfo{
			{ID: "site1", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true, Dir: "/data"},
			{ID: "site2", Name: "", AdminURL: "http://127.0.0.1:8781/admin", Created: createdAt},
			{ID: "site3", Name: "壊れたサイト", Created: createdAt, OpenError: "database disk image is malformed", Dir: folder},
		},
	}
	_, c := newSitesDesktopClient(t, fake, "")
	page := c.get(sitesPagePath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body,
		"サイトのデータが壊れているため開けませんでした",
		"database disk image is malformed",
		"詳しい内容",
		"フォルダ: "+folder,
		"一覧から外す",
		`action="/admin/settings/sites/site3/forget"`,
		"バックアップから始める",
	)
	if strings.Contains(page.Body, `/admin/settings/sites/site3/delete`) {
		t.Error("a site that failed to open offers deletion of its folder")
	}

	c.useEnglish()
	en := c.get(sitesPagePath).Body
	expectContains(t, en,
		"Could not open this site because its data is damaged",
		"Details",
		"Folder: "+folder,
		"Remove from the list",
		"database disk image is malformed",
		"left as they are",
		"automatic backup",
	)
	if strings.Contains(en, "A backup in it") {
		t.Error("English confirm says a backup inside the folder can be restored")
	}
	expectStatus(t, c.get("/admin/set-lang?lang=ja&return=/admin"), http.StatusSeeOther)

	for _, id := range []string{"site1", "site2"} {
		res := c.postForm(sitesPagePath+"/"+id+"/forget", nil)
		expectStatus(t, res, http.StatusNotFound)
	}
	if len(fake.forgotten) != 0 {
		t.Fatalf("Forget called for a site that is open: %v", fake.forgotten)
	}

	if strings.Contains(page.Body, "中のバックアップは") {
		t.Errorf("confirm text says a backup inside the folder can be restored:\n%s", page.Body)
	}
	expectContains(t, page.Body, "そのまま残します", "自動バックアップ")

	loc := expectRedirect(t, c.postForm(sitesPagePath+"/site3/forget", nil), sitesPagePath+"?flash=site_forgotten")
	if strings.Contains(loc, "folder=") {
		t.Errorf("Location = %q, want no folder query", loc)
	}
	if !slices.Equal(fake.forgotten, []string{"site3"}) {
		t.Fatalf("forgotten = %v, want [site3]", fake.forgotten)
	}
	done := c.get(loc)
	expectStatus(t, done, http.StatusOK)
	expectContains(t, done.Body,
		"一覧から外しました",
		"バックアップから始める",
		"そのままの場所に残しています",
		"自動バックアップ",
	)
	if strings.Contains(done.Body, "中のバックアップは") {
		t.Errorf("result text says a backup inside the folder can be restored:\n%s", done.Body)
	}
}

func TestForgetFlashIgnoresFolderQuery(t *testing.T) {
	createdAt := time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local)
	const evil = "INJECTED_BANNER_TEXT"
	fake := &fakeSites{
		sites: []SiteInfo{
			{ID: "site1", Name: "学生新聞", AdminURL: "http://127.0.0.1:8780/admin", Created: createdAt, Current: true, Dir: "/data"},
			{ID: "site3", Name: "壊れたサイト", Created: createdAt, OpenError: "database disk image is malformed", Dir: "/data/sites/site3"},
		},
	}
	_, c := newSitesDesktopClient(t, fake, "")
	page := c.get(sitesPagePath + "?flash=site_forgotten&folder=" + url.QueryEscape(evil+" <img src=x>"))
	expectStatus(t, page, http.StatusOK)
	notice := sitesNotice(page.Body)
	if strings.Contains(notice, evil) || strings.Contains(notice, "img src") {
		t.Fatalf("arbitrary folder query was shown in the notice: %s", notice)
	}
	expectContains(t, notice, "そのままの場所に残しています", "自動バックアップ")
	if strings.Contains(notice, "中のバックアップは") {
		t.Fatalf("notice still says a backup inside the folder can be restored: %s", notice)
	}

	c.useEnglish()
	en := sitesNotice(c.get(sitesPagePath + "?flash=site_forgotten&folder=" + url.QueryEscape(evil)).Body)
	if strings.Contains(en, evil) {
		t.Fatalf("arbitrary folder query was shown in the English notice: %s", en)
	}
	expectContains(t, en, "where they are", "automatic backup")
	if strings.Contains(en, "A backup in it") {
		t.Fatalf("English notice still says a backup inside the folder can be restored: %s", en)
	}
}

// sitesNotice は「サイト」画面の案内（id=sites-notice）の中身を返す。無ければ空。
func sitesNotice(body string) string {
	const marker = `id="sites-notice"`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	_, after, ok := strings.Cut(rest, ">")
	if !ok {
		return ""
	}
	text, _, _ := strings.Cut(after, "</div>")
	return text
}

// withoutJSDictionary は、管理画面のどのページにも埋め込む JavaScript の文言辞書（partials/js_i18n.html の
// window.SORUS_I18N）を除いた本文を返す。画面に出る言葉だけを確かめるときに使う。
func withoutJSDictionary(body string) string {
	before, rest, ok := strings.Cut(body, "<script>window.SORUS_I18N")
	if !ok {
		return body
	}
	_, after, _ := strings.Cut(rest, "</script>")
	return before + after
}

// withoutSettingsNav は、設定画面の共通の左メニュー（partials/settings_subnav.html）を除いた本文を返す。
// サイトの画面も左メニュー付きの2ペインにした（admin_ux_polish_and_mcp_spec.md §2-3）ので、メニューの項目名
// （「公開サーバー」など）ではなく、サイトの画面自身の文言だけを確かめるために使う。
func withoutSettingsNav(body string) string {
	before, rest, ok := strings.Cut(body, `<nav class="admin-settings-sidebar`)
	if !ok {
		return body
	}
	_, after, _ := strings.Cut(rest, "</nav>")
	return before + after
}
