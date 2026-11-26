package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// 固定ページ（kind = page）とサイト構成
// （メニュー・トップページ・カード）が同期 API で公開サーバーへ届くことを確かめる。

func TestSyncAPISubmitKind(t *testing.T) {
	env := newSyncEnvWith(t, Config{SyncAPIPath: "/hooks/sync"}, nil)
	writer := env.token("yamada", auth.RoleWriter)

	t.Run("missing kind stores a post", func(t *testing.T) {
		// 種類を知らない古いローカル CMS の要求（kind の無い JSON）。
		rec := env.callRaw("Bearer "+writer, "application/json",
			[]byte(`{"action":"submit_draft","article":{"id":"old-client","title":"古いクライアント","slug":"old-client","content_markdown":"本文"}}`))
		expectSyncStatus(t, rec, http.StatusOK)
		res := decodeSync[SyncSubmitResponse](t, rec)
		if res.Article.Kind != article.KindPost || !strings.HasSuffix(res.Article.URL, "/articles/old-client") {
			t.Errorf("ref = %+v, want a post at /articles/old-client", res.Article)
		}
		if a := env.mustGetArticle("old-client"); a.Kind != article.KindPost {
			t.Errorf("kind = %q, want post", a.Kind)
		}
	})

	t.Run("page keeps its kind across resubmission without kind", func(t *testing.T) {
		rec := env.call(writer, submitRequest(SyncArticle{ID: "about-page", Title: "研究室について", Slug: "about",
			ContentMarkdown: "研究室の紹介", Kind: article.KindPage}))
		expectSyncStatus(t, rec, http.StatusOK)
		res := decodeSync[SyncSubmitResponse](t, rec)
		if res.Article.Kind != article.KindPage || !strings.HasSuffix(res.Article.URL, "/about") || strings.Contains(res.Article.URL, "/articles/") {
			t.Errorf("ref = %+v, want a page at /about", res.Article)
		}
		rec = env.call(writer, submitRequest(SyncArticle{ID: "about-page", Title: "研究室について（改）", ContentMarkdown: "直した紹介"}))
		expectSyncStatus(t, rec, http.StatusOK)
		if a := env.mustGetArticle("about-page"); a.Kind != article.KindPage || a.Slug != "about" || a.Status != article.StatusReview {
			t.Errorf("after resubmission: kind %q slug %q status %q", a.Kind, a.Slug, a.Status)
		}

		pending := decodeSync[SyncPendingResponse](t, env.call(env.token("sato", auth.RoleEditor), SyncRequest{Action: SyncActionFetchPending}))
		kinds := map[string]article.Kind{}
		for _, p := range pending.Articles {
			kinds[p.ID] = p.Kind
		}
		if kinds["about-page"] != article.KindPage || kinds["old-client"] != article.KindPost {
			t.Errorf("fetch_pending kinds = %v", kinds)
		}
	})

	t.Run("unknown kind is rejected", func(t *testing.T) {
		rec := env.call(writer, submitRequest(SyncArticle{ID: "bad-kind", Title: "種類が不正", Kind: "news"}))
		expectSyncStatus(t, rec, http.StatusBadRequest)
		if msg := syncErrorMessage(t, rec); !strings.Contains(msg, `"news"`) {
			t.Errorf("message = %q", msg)
		}
		if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE id = 'bad-kind'`); n != 0 {
			t.Errorf("rejected article was stored")
		}
	})

	t.Run("reserved page slugs are rejected", func(t *testing.T) {
		before := len(env.uploadedFiles())
		for _, tc := range []struct{ id, title, slug string }{
			{"reserved-admin", "管理", "admin"},
			{"reserved-upper", "カテゴリ", "Categories"},
			// スラッグを省略するとタイトルから作る。
			{"reserved-title", "Search", ""},
			// 公開サーバーの同期 API のパスの先頭部分（/hooks/sync の hooks）も使えない。
			{"reserved-sync", "連携", "hooks"},
		} {
			rec := env.call(writer, submitRequest(SyncArticle{ID: tc.id, Title: tc.title, Slug: tc.slug, Kind: article.KindPage},
				localImages(t, 40, 30)...))
			expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
			if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "ページには使えません") {
				t.Errorf("%s: message = %q", tc.id, msg)
			}
			if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE id = ?`, tc.id); n != 0 {
				t.Errorf("%s: rejected page was stored", tc.id)
			}
		}
		if after := len(env.uploadedFiles()); after != before {
			t.Errorf("images of rejected pages were stored: %d -> %d files", before, after)
		}
		// 記事（post）のスラッグとしては使える。
		rec := env.call(writer, submitRequest(SyncArticle{ID: "post-hooks", Title: "連携の記事", Slug: "hooks", Kind: article.KindPost}))
		expectSyncStatus(t, rec, http.StatusOK)
	})
}

// ローカル CMS で書いたページを公開専用の公開サーバーへ提出し、承認すると /<slug> で配信される。
// 公開サーバーの校閲待ちのページをローカルへ取り込んでも種類を保つ。
func TestE2ELocalPageToPublicOnlyServer(t *testing.T) {
	env := &remoteEnv{local: newTestEnv(t), remote: newPublicOnlyEnv(t, nil)}
	id := seedArticle(t, env.local.db, article.Article{Title: "アクセス", Slug: "access", Kind: article.KindPage,
		ContentMarkdown: "駅から徒歩5分", AuthorID: studentA.ID, Status: article.StatusDraft})
	reader := env.remote.newClient()

	env.configure("yamada", auth.RoleWriter)
	var submitted struct {
		Article SyncArticleRef `json:"article"`
	}
	expectStatus(t, env.local.loggedIn(studentA).postJSON("/admin/remote/articles/"+id+"/submit", nil, &submitted), http.StatusOK)
	if submitted.Article.Kind != article.KindPage {
		t.Fatalf("submit = %+v", submitted)
	}
	if a := env.remote.mustGetArticle(id); a.Kind != article.KindPage || a.Status != article.StatusReview {
		t.Fatalf("remote article: kind %q status %q", a.Kind, a.Status)
	}
	expectStatus(t, reader.get("/access"), http.StatusNotFound)

	env.configure("sato", auth.RoleEditor)
	editor := env.local.loggedIn(chief)
	var pending struct {
		Articles []remotePendingItem `json:"articles"`
	}
	expectStatus(t, editor.getJSON("/admin/remote/pending", &pending), http.StatusOK)
	if len(pending.Articles) != 1 || pending.Articles[0].Kind != article.KindPage {
		t.Fatalf("pending = %+v", pending.Articles)
	}

	var published struct {
		URL     string         `json:"url"`
		Article SyncArticleRef `json:"article"`
	}
	expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/publish", nil, &published), http.StatusOK)
	if published.URL != env.remote.srv.URL+"/access" || published.Article.Kind != article.KindPage {
		t.Errorf("publish = %+v", published)
	}
	page := reader.get("/access")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "アクセス", "駅から徒歩5分")
	// 記事の URL ではページへ転送し、記事の一覧には出さない。
	if res := reader.get("/articles/access"); res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/access" {
		t.Errorf("/articles/access = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	expectNotContains(t, reader.get("/articles").Body, "駅から徒歩5分", `href="/articles/access"`)

	t.Run("import from the remote keeps the kind", func(t *testing.T) {
		rec := env.remote.call(env.remote.token("yamada", auth.RoleWriter), submitRequest(SyncArticle{ID: "remote-members",
			Title: "メンバー", Slug: "members", ContentMarkdown: "教授と学生", Kind: article.KindPage}))
		expectSyncStatus(t, rec, http.StatusOK)
		expectStatus(t, editor.postJSON("/admin/remote/pending/remote-members/import", nil, nil), http.StatusOK)
		if a := env.local.mustGetArticle("remote-members"); a.Kind != article.KindPage || a.Slug != "members" {
			t.Errorf("imported: kind %q slug %q", a.Kind, a.Slug)
		}
	})
}

func TestSyncAPISyncSettingsStructure(t *testing.T) {
	env := newSyncEnv(t)
	adminToken := env.token("chief-admin", auth.RoleAdmin)
	menu := []MenuItem{
		{Label: "ホーム", Type: MenuItemURL, Target: "/"},
		{Type: MenuItemPage, Target: "research"},
		{Label: "サークル", Type: MenuItemCategory, Target: "circle"},
		{Type: MenuItemArticles},
	}
	cards := []string{"access", "メンバー"}
	tag, title := "学校", "写真"
	limit := 8
	structure := func() SiteStructure { return env.server.loadSiteStructure(t.Context()) }
	sections := []FrontSection{{Type: FrontSectionCards, Pages: cards}, {Type: FrontSectionGallery, Tag: tag, Limit: limit, Title: title, Layout: galleryLayoutJustified}}
	wantStructure := SiteStructure{Menu: menu, FrontPage: "research", FrontSections: sections}

	rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{
		SiteMenu: &menu, FrontPage: strPtr(" research "), FrontSections: &sections,
	}})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncSettingsResponse](t, rec)
	if !reflect.DeepEqual(res.Settings.SiteMenu, menu) || res.Settings.FrontPage != "research" || !reflect.DeepEqual(res.Settings.FrontSections, sections) {
		t.Errorf("response = %+v", res.Settings)
	}
	if got := structure(); !reflect.DeepEqual(got, wantStructure) {
		t.Errorf("saved structure = %+v, want %+v", got, wantStructure)
	}

	t.Run("absent fields keep the remote structure", func(t *testing.T) {
		// サイト構成を知らない古いローカル CMS の反映（基本設定だけ）。
		rec := env.callRaw("Bearer "+adminToken, "application/json",
			[]byte(`{"action":"sync_settings","settings":{"site_name":"古いクライアント","site_menu":null}}`))
		expectSyncStatus(t, rec, http.StatusOK)
		if got := structure(); !reflect.DeepEqual(got, wantStructure) {
			t.Errorf("structure after basic-only sync = %+v", got)
		}
		// 一部の項目だけを指定すると、その項目だけを変える。
		rec = env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{FrontPage: strPtr("")}})
		expectSyncStatus(t, rec, http.StatusOK)
		want := wantStructure
		want.FrontPage = ""
		if got := structure(); !reflect.DeepEqual(got, want) {
			t.Errorf("structure after front_page only = %+v, want %+v", got, want)
		}
		rec = env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{FrontPage: strPtr("research")}})
		expectSyncStatus(t, rec, http.StatusOK)
	})

	t.Run("invalid structure changes nothing", func(t *testing.T) {
		siteName := env.setting("site_name")
		tooManyCards := []FrontSection{{Type: FrontSectionCards, Pages: []string{"a", "b", "c", "d", "e", "f", "g"}}}
		for name, tc := range map[string]struct {
			settings SyncSettings
			want     string
		}{
			"script url":      {SyncSettings{SiteMenu: &[]MenuItem{{Label: "x", Type: MenuItemURL, Target: "javascript:alert(1)"}}}, "メニューの1番目の項目: URL は"},
			"unknown type":    {SyncSettings{SiteMenu: &[]MenuItem{{Type: MenuItemArticles}, {Label: "x", Type: "widget", Target: "x"}}}, "メニューの2番目の項目: 種類「widget」"},
			"too many cards":  {SyncSettings{FrontSections: &tooManyCards}, "トップのカードは最大"},
			"slug with slash": {SyncSettings{FrontPage: strPtr("a/b")}, "スラッグ「a/b」"},
			"with basic settings": {SyncSettings{SiteName: strPtr("変わらないはず"),
				SiteMenu: &[]MenuItem{{Type: MenuItemPage}}}, "リンク先を選んでください"},
		} {
			settings := tc.settings
			rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &settings})
			expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
			if msg := syncErrorMessage(t, rec); !strings.HasPrefix(msg, "サイト構成: ") || !strings.Contains(msg, tc.want) {
				t.Errorf("%s: message = %q, want %q", name, msg, tc.want)
			}
		}
		if got := structure(); !reflect.DeepEqual(got, wantStructure) {
			t.Errorf("structure after rejected syncs = %+v", got)
		}
		if got := env.setting("site_name"); got != siteName {
			t.Errorf("site_name = %q after a rejected sync, want %q", got, siteName)
		}
	})

	t.Run("empty arrays clear the menu and cards", func(t *testing.T) {
		rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{
			SiteMenu: &[]MenuItem{}, FrontSections: &[]FrontSection{},
		}})
		expectSyncStatus(t, rec, http.StatusOK)
		res := decodeSync[SyncSettingsResponse](t, rec)
		if res.Settings.SiteMenu == nil || len(res.Settings.SiteMenu) != 0 || len(res.Settings.FrontSections) != 0 {
			t.Errorf("response = %+v, want empty arrays", res.Settings)
		}
		if got := structure(); len(got.Menu) != 0 || len(got.FrontSections) != 0 || got.FrontPage != "research" {
			t.Errorf("structure = %+v", got)
		}
	})

	t.Run("only admin tokens", func(t *testing.T) {
		for _, role := range []auth.Role{auth.RoleWriter, auth.RoleEditor} {
			rec := env.call(env.token("someone", role), SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{FrontPage: strPtr("x")}})
			expectSyncStatus(t, rec, http.StatusForbidden)
		}
		if got := structure(); got.FrontPage != "research" {
			t.Errorf("front_page = %q after a forbidden sync", got.FrontPage)
		}
	})
}

// ローカルのサイト構成を公開サーバーへ反映すると、公開サーバーのメニュー・トップページが同じになる。
func TestRemoteSettingsPushStructure(t *testing.T) {
	env := newRemoteEnv(t)
	seedPage(t, env.remote.db, article.Article{Title: "研究紹介", Slug: "research", ContentMarkdown: "量子計算を研究しています。",
		Status: article.StatusPublished, PublishedAt: ago(time.Minute)})
	local := SiteStructure{
		Menu:      []MenuItem{{Type: MenuItemPage, Target: "research"}, {Label: "大学", Type: MenuItemURL, Target: "https://www.example.ac.jp/"}},
		FrontPage: "research", FrontSections: []FrontSection{{Type: FrontSectionHero}, {Type: FrontSectionArticleList}},
	}
	if err := env.local.server.saveSiteStructure(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	env.configure("root", auth.RoleAdmin)

	var out struct {
		Message  string             `json:"message"`
		Settings SyncSettingsValues `json:"settings"`
	}
	expectStatus(t, env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if !strings.Contains(out.Message, "サイト構成") {
		t.Errorf("message = %q", out.Message)
	}
	want := local
	if got := env.remote.server.loadSiteStructure(t.Context()); !reflect.DeepEqual(got, want) {
		t.Errorf("remote structure = %+v, want %+v", got, want)
	}
	home := env.remote.newClient().get("/")
	expectStatus(t, home, http.StatusOK)
	// 前のページ（フロントページ）は `/` で、現在のパスも `/` なので aria-current が付く。
	expectContains(t, home.Body, `<nav class="category-nav" aria-label="メニュー">`, `<a href="/" aria-current="page">研究紹介</a>`, `href="https://www.example.ac.jp/"`,
		`<h1 id="front-hero-title" class="article-title">研究紹介</h1>`)

	t.Run("old remote is reported", func(t *testing.T) {
		// サイト構成を知らない公開サーバーは、応答の settings に site_menu を含めない。
		var got SyncRequest
		old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"action":"sync_settings","settings":{"site_name":"古い公開サーバー"}}`))
		}))
		defer old.Close()
		cfg := env.local.server.loadRemoteConfig()
		cfg.URL = old.URL
		if err := env.local.server.saveRemoteConfig(cfg); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Message string `json:"message"`
		}
		expectStatus(t, env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
		if !strings.Contains(out.Message, "公開サーバーの sorus を更新してください") {
			t.Errorf("message = %q", out.Message)
		}
		if got.Settings == nil || got.Settings.SiteMenu == nil || len(*got.Settings.SiteMenu) != 2 {
			t.Errorf("pushed settings = %+v", got.Settings)
		}
	})
}
