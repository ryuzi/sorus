package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sorus/internal/auth"
	"sorus/internal/markdown"
)

// フォトギャラリーの写真は、一覧に軽いサムネイル（-800 → -400）、拡大に高解像度（-1200 → 原寸）を使い、元画像の寸法を持つ。
// 無いサイズは今までの表示の画像（URL）で埋める。
func TestNewGalleryImageResolvesThumbFullAndSize(t *testing.T) {
	dir := "/var/sorus/uploads/"
	for _, tc := range []struct {
		name             string
		row              mediaRecord
		url, thumb, full string
		width, height    int
		ok               bool
	}{
		{"all sizes", mediaRecord{PathOriginal: dir + "a-original.webp", PathLarge: dir + "a-1200.webp", PathMedium: dir + "a-800.webp",
			PathSmall: dir + "a-400.webp", Width: 3000, Height: 2000},
			"/uploads/a-1200.webp", "/uploads/a-800.webp", "/uploads/a-1200.webp", 3000, 2000, true},
		{"no medium uses small", mediaRecord{PathOriginal: dir + "b-original.webp", PathLarge: dir + "b-1200.webp", PathSmall: dir + "b-400.webp",
			Width: 1600, Height: 900},
			"/uploads/b-1200.webp", "/uploads/b-400.webp", "/uploads/b-1200.webp", 1600, 900, true},
		{"no large uses original", mediaRecord{PathOriginal: dir + "c-original.webp", PathMedium: dir + "c-800.webp", Width: 900, Height: 1200},
			"/uploads/c-original.webp", "/uploads/c-800.webp", "/uploads/c-original.webp", 900, 1200, true},
		{"original only, unknown size", mediaRecord{PathOriginal: dir + "d.png"},
			"/uploads/d.png", "/uploads/d.png", "/uploads/d.png", 0, 0, true},
		{"half known size is dropped", mediaRecord{PathLarge: dir + "e-1200.webp", Width: 1200},
			"/uploads/e-1200.webp", "/uploads/e-1200.webp", "/uploads/e-1200.webp", 0, 0, true},
		{"no file", mediaRecord{Width: 100, Height: 80}, "", "", "", 0, 0, false},
		{"unsafe path", mediaRecord{PathLarge: "..", PathMedium: ""}, "", "", "", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.Alt, tc.row.Caption = "代替", "説明"
			got, ok := newGalleryImage(tc.row)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			want := markdown.GalleryImage{URL: tc.url, Alt: "代替", Caption: "説明", ThumbURL: tc.thumb, FullURL: tc.full, Width: tc.width, Height: tc.height}
			if got != want {
				t.Errorf("image = %+v\nwant    %+v", got, want)
			}
		})
	}
}

// タグのギャラリーの解決（:::gallery とトップページのギャラリーが使う）も、サムネイル・高解像度・寸法を返す。
func TestResolveGalleryReturnsThumbFullAndSize(t *testing.T) {
	env := newTestEnv(t)
	env.insertMedia("sized-photo", "sized.webp", "校舎", "", "学校")
	images, err := env.server.resolveGallery(markdown.GalleryQuery{Tag: "学校", Sort: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	want := markdown.GalleryImage{URL: "/uploads/sized-photo-1200.webp", Alt: "校舎", ThumbURL: "/uploads/sized-photo-800.webp",
		FullURL: "/uploads/sized-photo-1200.webp", Width: 100, Height: 80}
	if len(images) != 1 || images[0] != want {
		t.Fatalf("images = %+v, want [%+v]", images, want)
	}
}

func TestValidateTopGalleryLayoutAndLink(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      SiteStructure
		wantKey string // 空なら有効
	}{
		{"default layout", SiteStructure{}, ""},
		{"justified", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Layout: "justified"}}}, ""},
		{"masonry", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Layout: "masonry"}}}, ""},
		{"cards", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Layout: "cards"}}}, ""},
		{"unknown layout", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Layout: "grid"}}}, "structure_err_gallery_layout"},
		{"layout with markup", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Layout: `cards" onclick="x`}}}, "structure_err_gallery_layout"},
		{"site path link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "/highlights", LinkLabel: "もっと見る"}}}, ""},
		{"https link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "https://photos.example.ac.jp/album?id=1#top"}}}, ""},
		{"http link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "http://photos.example.ac.jp/"}}}, ""},
		{"javascript link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "javascript:alert(1)"}}}, "structure_err_gallery_link_url"},
		{"protocol-relative link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "//evil.example"}}}, "structure_err_gallery_link_url"},
		{"backslash link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "/\\evil.example"}}}, "structure_err_gallery_link_url"},
		{"http without host", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "http://"}}}, "structure_err_gallery_link_url"},
		{"ftp link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "ftp://example.com/"}}}, "structure_err_gallery_link_url"},
		{"relative link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "highlights"}}}, "structure_err_gallery_link_url"},
		{"link with quote", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: `/a"onmouseover="x`}}}, "structure_err_gallery_link_url"},
		{"credentials in link", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkURL: "http://user:pass@example.com/"}}}, "structure_err_gallery_link_url"},
		{"max label", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkLabel: strings.Repeat("写", maxGalleryLinkLabelRunes)}}}, ""},
		{"long label", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkLabel: strings.Repeat("写", maxGalleryLinkLabelRunes+1)}}}, "structure_err_gallery_link_label"},
		{"label control", SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, LinkLabel: "すべて\n見る"}}}, "structure_err_gallery_link_label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSiteStructure(tc.st)
			if tc.wantKey == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			se, ok := err.(*siteStructureError)
			if !ok || se.Key != tc.wantKey {
				t.Fatalf("error = %v, want key %s", err, tc.wantKey)
			}
		})
	}
}

// 外観の画面で並べ方と「すべて見る」リンクを保存すると、トップページのギャラリーがその並べ方とリンクで出る。
func TestTopGalleryLayoutAndLinkSaveAndRender(t *testing.T) {
	env := newFrontEnv(t)
	env.insertMedia("layout-one", "layout-one.webp", "校舎", "春の校舎", "学校")
	adm := env.loggedIn(admin)

	// 保存していなければ行揃え（おすすめ）を選んだ表示で、リンクは出さない。
	page := adm.get("/admin/settings/structure")
	expectContains(t, page.Body, `name="section_type"`, `value="gallery"`, `name="section_layout"`, `name="section_link_url"`, `name="section_link_label"`, `name="section_tag"`)

	form := structureForm(nil, "research")
	setGalleryFormField(form, "tag", "学校")
	setGalleryFormField(form, "layout", "cards")
	setGalleryFormField(form, "link_url", " /highlights ")
	setGalleryFormField(form, "link_label", "写真をもっと見る")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")
	got := env.server.loadSiteStructure(t.Context())
	if frontGallery(got).Layout != "cards" || frontGallery(got).LinkURL != "/highlights" || frontGallery(got).LinkLabel != "写真をもっと見る" {
		t.Fatalf("saved = (%q, %q, %q)", frontGallery(got).Layout, frontGallery(got).LinkURL, frontGallery(got).LinkLabel)
	}
	page = adm.get("/admin/settings/structure")
	expectContains(t, page.Body, `name="section_layout"`, `value="/highlights"`, `value="写真をもっと見る"`)

	front := env.newClient().get("/")
	expectStatus(t, front, http.StatusOK)
	expectContains(t, front.Body,
		`<figure class="photo-gallery gallery-layout-cards" data-gallery-layout="cards" data-gallery-tag="学校">`,
		`<img src="/uploads/layout-one-800.webp" data-full="/uploads/layout-one-1200.webp" width="100" height="80" alt="校舎" loading="lazy" decoding="async">`,
		`<p class="front-gallery-more"><a href="/highlights">写真をもっと見る</a></p>`)
	expectNotContains(t, front.Body, "masonry-grid")

	// 段組みは今までの masonry-grid のクラスも付ける（テーマの CSS が使っている）。文言が空なら「すべて見る」。
	setGalleryFormField(form, "layout", "masonry")
	setGalleryFormField(form, "link_url", "https://photos.example.ac.jp/album")
	setGalleryFormField(form, "link_label", "")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")
	front = env.newClient().get("/")
	expectContains(t, front.Body,
		`<figure class="photo-gallery gallery-layout-masonry masonry-grid" data-gallery-layout="masonry" data-gallery-tag="学校">`,
		`<p class="front-gallery-more"><a href="https://photos.example.ac.jp/album">すべて見る</a></p>`)

	// リンク先が空ならリンクを出さない。
	setGalleryFormField(form, "link_url", "")
	setGalleryFormField(form, "link_label", "使わない文言")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure?flash=structure_saved")
	front = env.newClient().get("/")
	expectContains(t, front.Body, `data-gallery-layout="masonry"`)
	expectNotContainsAny(t, "no link", front.Body, "front-gallery-more", "使わない文言")

	// 不正な値は保存せず、入力を残したまま画面に戻す。
	before := env.server.loadSiteStructure(t.Context())
	for name, set := range map[string][2]string{
		"unknown layout":  {"top_gallery_layout", "grid"},
		"javascript link": {"top_gallery_link_url", "javascript:alert(1)"},
		"other host link": {"top_gallery_link_url", "//evil.example/"},
		"long label":      {"top_gallery_link_label", strings.Repeat("あ", maxGalleryLinkLabelRunes+1)},
	} {
		bad := structureForm(nil, "research")
		setGalleryFormField(bad, "tag", "学校")
		setGalleryFormField(bad, "layout", "cards")
		field := strings.TrimPrefix(set[0], "top_gallery_")
		setGalleryFormField(bad, map[string]string{"link_url": "link_url", "link_label": "link_label", "layout": "layout"}[field], set[1])
		res := adm.postForm("/admin/settings/structure", bad)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		if got := env.server.loadSiteStructure(t.Context()); frontGallery(got).Layout != frontGallery(before).Layout ||
			frontGallery(got).LinkURL != frontGallery(before).LinkURL || frontGallery(got).LinkLabel != frontGallery(before).LinkLabel {
			t.Errorf("%s: saved %+v after a rejected form", name, got)
		}
	}
	bad := structureForm(nil, "research")
	setGalleryFormField(bad, "link_url", "javascript:alert(1)")
	res := adm.postForm("/admin/settings/structure", bad)
	expectContains(t, res.Body, "「すべて見る」のリンク先は、/ で始まるサイト内のパスか", `value="javascript:alert(1)"`)
}

// 保存値が改ざんされていても（知らない並べ方・javascript: のリンク・長すぎる文言）、公開側には出さない。
func TestTopGalleryTamperedSettingsAreIgnored(t *testing.T) {
	env := newTestEnv(t)
	env.insertMedia("tamper-photo", "tamper.webp", "校舎", "", "学校")
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Tag: "学校", Limit: 4}}}); err != nil {
		t.Fatal(err)
	}
	setSettingForTest(t, env.db, settingFrontSections, `[{"type":"gallery","tag":"学校","limit":4,"layout":"cards\" onmouseover=\"alert(1)","link_url":"javascript:alert(1)","link_label":"`+strings.Repeat("長", maxGalleryLinkLabelRunes+1)+`"}]`)
	st := env.server.loadSiteStructure(t.Context())
	if frontGallery(st).Layout != galleryLayoutJustified || frontGallery(st).LinkURL != "" || frontGallery(st).LinkLabel != "" {
		t.Fatalf("loaded = (%q, %q, %q)", frontGallery(st).Layout, frontGallery(st).LinkURL, frontGallery(st).LinkLabel)
	}
	body := env.newClient().get("/").Body
	expectContains(t, body, `class="photo-gallery gallery-layout-justified" data-gallery-layout="justified"`)
	expectNotContainsAny(t, "tampered", body, "onmouseover", "javascript:", "front-gallery-more")
}

// 英語のトップページでは、既定の見出し・リンクの文言・読み上げ文を英語で出す。
func TestTopGalleryEnglishDefaults(t *testing.T) {
	env := newTestEnv(t)
	enableMultilingual(t, env.db)
	env.insertMedia("en-photo", "en.webp", "Campus", "", "campus")
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Tag: "campus", Limit: 4, LinkURL: "/en/articles"}}}); err != nil {
		t.Fatal(err)
	}
	res := env.newClient().get("/en")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `>Photo Gallery</h2>`, `aria-label="Enlarge image: Campus"`,
		`<p class="front-gallery-more"><a href="/en/articles">View all</a></p>`)
}

// 公開サーバーの同期 API（sync_settings）でも並べ方と「すべて見る」リンクを受け取り、検証してから保存する。
func TestSyncAPISyncSettingsGalleryLayoutAndLink(t *testing.T) {
	env := newSyncEnv(t)
	adminToken := env.token("chief-admin", auth.RoleAdmin)
	layout, linkURL, linkLabel := "masonry", "/highlights", "もっと見る"
	sections := []FrontSection{{Type: FrontSectionGallery, Tag: "photos", Layout: layout, LinkURL: " " + linkURL + " ", LinkLabel: linkLabel}}
	rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{FrontSections: &sections}})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncSettingsResponse](t, rec)
	if len(res.Settings.FrontSections) != 1 || res.Settings.FrontSections[0].Layout != layout ||
		res.Settings.FrontSections[0].LinkURL != linkURL || res.Settings.FrontSections[0].LinkLabel != linkLabel {
		t.Errorf("response = %+v", res.Settings)
	}
	st := env.server.loadSiteStructure(t.Context())
	if frontGallery(st).Layout != layout || frontGallery(st).LinkURL != linkURL || frontGallery(st).LinkLabel != linkLabel {
		t.Fatalf("saved = (%q, %q, %q)", frontGallery(st).Layout, frontGallery(st).LinkURL, frontGallery(st).LinkLabel)
	}

	// 項目を送らない古い PC の反映では変えない。
	rec = env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{SiteName: strPtr("still valid")}})
	expectSyncStatus(t, rec, http.StatusOK)
	if got := env.server.loadSiteStructure(t.Context()); frontGallery(got).Layout != layout || frontGallery(got).LinkURL != linkURL {
		t.Errorf("after a sync without the fields = %+v", got)
	}

	for name, settings := range map[string]SyncSettings{
		"unknown layout":  {FrontSections: &[]FrontSection{{Type: FrontSectionGallery, Layout: "grid"}}},
		"javascript link": {FrontSections: &[]FrontSection{{Type: FrontSectionGallery, LinkURL: "javascript:alert(1)"}}},
		"long label":      {FrontSections: &[]FrontSection{{Type: FrontSectionGallery, LinkLabel: strings.Repeat("あ", maxGalleryLinkLabelRunes+1)}}},
	} {
		rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &settings})
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
		if msg := syncErrorMessage(t, rec); !strings.HasPrefix(msg, "サイト構成: ") {
			t.Errorf("%s: message = %q", name, msg)
		}
	}
	if got := env.server.loadSiteStructure(t.Context()); frontGallery(got).Layout != layout || frontGallery(got).LinkURL != linkURL || frontGallery(got).LinkLabel != linkLabel {
		t.Errorf("after rejected syncs = %+v", got)
	}
}

// 「公開サーバーへ反映する」で並べ方と「すべて見る」リンクも送り、公開サイトのトップページが同じ表示になる。
// 項目を知らない古い公開サーバーには、反映されていないことを伝える。
func TestRemoteSettingsPushGalleryLayoutAndLink(t *testing.T) {
	env := newRemoteEnv(t)
	env.remote.insertMedia("remote-photo", "remote.webp", "校舎", "", "学校")
	if err := env.local.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Tag: "学校", Limit: 6, Layout: "cards", LinkURL: "/articles", LinkLabel: "記事を見る"}}}); err != nil {
		t.Fatal(err)
	}
	env.configure("root", auth.RoleAdmin)
	var out struct {
		Message  string             `json:"message"`
		Settings SyncSettingsValues `json:"settings"`
	}
	expectStatus(t, env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if strings.Contains(out.Message, "並べ方") {
		t.Errorf("message = %q, want no old-server note", out.Message)
	}
	st := env.remote.server.loadSiteStructure(t.Context())
	if frontGallery(st).Layout != "cards" || frontGallery(st).LinkURL != "/articles" || frontGallery(st).LinkLabel != "記事を見る" {
		t.Fatalf("remote structure = %+v", st)
	}
	expectContains(t, env.remote.newClient().get("/").Body, `data-gallery-layout="cards"`,
		`<p class="front-gallery-more"><a href="/articles">記事を見る</a></p>`)

	t.Run("old remote is reported", func(t *testing.T) {
		// 並べ方を知らない公開サーバーは、応答の settings に top_gallery_layout を含めない。
		var got SyncRequest
		old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"action":"sync_settings","settings":{"site_name":"古い公開サーバー","site_menu":[],"front_cards":[],"show_author":false}}`))
		}))
		defer old.Close()
		// タグが無ければフォトギャラリーの画像（sync_media）は送らず、sync_settings の応答だけで判断する。
		if err := env.local.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Limit: 6, Layout: "cards", LinkURL: "/articles", LinkLabel: "記事を見る"}}}); err != nil {
			t.Fatal(err)
		}
		cfg := env.local.server.loadRemoteConfig()
		cfg.URL = old.URL
		if err := env.local.server.saveRemoteConfig(cfg); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Message string `json:"message"`
		}
		env.local.loggedIn(admin).postJSON("/admin/remote/settings/push", nil, &out)
		if !strings.Contains(out.Message, "トップページのセクションは反映されていません") {
			t.Errorf("message = %q", out.Message)
		}
		if s := got.Settings; s == nil || s.FrontSections == nil || len(*s.FrontSections) != 1 || (*s.FrontSections)[0].Layout != "cards" ||
			(*s.FrontSections)[0].LinkURL != "/articles" || (*s.FrontSections)[0].LinkLabel != "記事を見る" {
			t.Errorf("pushed settings = %+v", got.Settings)
		}
	})
}
