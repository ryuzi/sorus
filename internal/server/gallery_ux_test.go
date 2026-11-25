package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sorus/internal/ai"
	"sorus/internal/auth"
)

// フォトギャラリーの初心者向けの使い勝手（gallery-ux-and-stats.md §1 の P1〜P5）の試験。

// resetRemoteDirty は未反映の記録を消す（次の操作で記録されるかを確かめるため）。
func resetRemoteDirty(t *testing.T, e *testEnv) {
	t.Helper()
	e.server.remoteDirtyMu.Lock()
	defer e.server.remoteDirtyMu.Unlock()
	if err := e.server.saveRemoteDirtyLocked(nil); err != nil {
		t.Fatal(err)
	}
}

// expectMediaDirty は「フォトギャラリー／メディア」が未反映として記録されているか（want）を確かめる。
func expectMediaDirty(t *testing.T, e *testEnv, want bool, step string) {
	t.Helper()
	if got := slices.Contains(e.server.remoteDirty(), remoteDirtyMedia); got != want {
		t.Errorf("%s: media dirty = %v, want %v (dirty = %v)", step, got, want, e.server.remoteDirty())
	}
}

// bulkTags はメディアライブラリの一括タグ付け（POST /admin/api/media/bulk-tags）を送る。
func bulkTags(t *testing.T, c *client, body string) *response {
	t.Helper()
	return c.postAPI("/admin/api/media/bulk-tags", "application/json", strings.NewReader(body))
}

// P1: メディアライブラリでタグ・説明を変えたり画像を消したりすると、「フォトギャラリー／メディア」が公開サーバーへ未反映に
// なる（以前は記録されず、共通バナーが出なかったため反映のボタンが分からなかった）。タグの無い画像の変更・アップロードは
// どのギャラリーにも出ないため記録しない。
func TestMediaChangesMarkRemoteDirtyMedia(t *testing.T) {
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{Alt: "AIの代替テキスト", Caption: "AIの説明", Tags: []string{"学校"}}}
	env := &remoteEnv{local: newTestEnvWith(t, Config{AI: fake}, nil), remote: newSyncEnv(t)}
	env.configure("root", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)

	gate := adm.mustUpload("gate.png", "正門", "")
	hall := adm.mustUpload("hall.png", "講堂", "")
	plain := adm.mustUpload("plain.png", "タグなし", "")
	expectMediaDirty(t, env.local, false, "upload")

	// タグの無い画像の説明だけを変えても、ギャラリーには関わらない。
	expectRedirect(t, adm.postForm("/admin/media/"+plain.ID, url.Values{"alt": {"タグなしの写真"}}), "/admin/media?flash=media_saved")
	expectMediaDirty(t, env.local, false, "untagged alt change")

	// 個別の編集でタグを付ける（fetch の応答は remote_dirty を返し、画面が共通バナーを出せる）。
	var item struct {
		AITags      string `json:"ai_tags"`
		RemoteDirty bool   `json:"remote_dirty"`
	}
	expectStatus(t, adm.postJSON("/admin/media/"+gate.ID, url.Values{"alt": {"正門"}, "ai_tags": {"学校"}}, &item), http.StatusOK)
	if item.AITags != "学校" || !item.RemoteDirty {
		t.Errorf("update response = %+v, want tags and remote_dirty", item)
	}
	expectMediaDirty(t, env.local, true, "update tags")

	// 同じ内容で保存し直しても、変わっていないため記録しない。
	resetRemoteDirty(t, env.local)
	item.RemoteDirty = false // remote_dirty は false なら応答に無い。
	expectStatus(t, adm.postJSON("/admin/media/"+gate.ID, url.Values{"alt": {"正門"}, "ai_tags": {"学校"}}, &item), http.StatusOK)
	if item.RemoteDirty {
		t.Error("unchanged update reported remote_dirty")
	}
	expectMediaDirty(t, env.local, false, "unchanged update")

	// タグの付いた画像の説明を変えると、ギャラリーの表示（代替テキスト）が変わる。
	expectRedirect(t, adm.postForm("/admin/media/"+gate.ID, url.Values{"alt": {"新しい正門"}, "ai_tags": {"学校"}}), "/admin/media?flash=media_saved")
	expectMediaDirty(t, env.local, true, "tagged alt change")

	// 一括タグ付け。
	resetRemoteDirty(t, env.local)
	var bulk struct {
		UpdatedCount int  `json:"updated_count"`
		RemoteDirty  bool `json:"remote_dirty"`
	}
	res := bulkTags(t, adm, `{"media_ids":["`+hall.ID+`"],"add_tags":["学校"]}`)
	expectStatus(t, res, http.StatusOK)
	decodeJSON(t, res.Body, &bulk)
	if bulk.UpdatedCount != 1 || !bulk.RemoteDirty {
		t.Errorf("bulk response = %+v", bulk)
	}
	expectMediaDirty(t, env.local, true, "bulk add")
	// 付いていないタグを外す一括操作は何も変えない。
	resetRemoteDirty(t, env.local)
	expectStatus(t, bulkTags(t, adm, `{"media_ids":["`+plain.ID+`"],"remove_tags":["学校"]}`), http.StatusOK)
	expectMediaDirty(t, env.local, false, "bulk remove of a missing tag")

	// AI の自動生成はタグを付ける。
	expectStatus(t, adm.postAPI(analyzePath(plain.ID), "", nil), http.StatusOK)
	expectMediaDirty(t, env.local, true, "analyze")

	// タグの付いた画像を消すと、公開サイトのギャラリーからも外す必要がある。タグの無い画像を消しても記録しない。
	resetRemoteDirty(t, env.local)
	if _, err := env.local.db.Exec(`UPDATE media SET ai_tags = '' WHERE id = ?`, plain.ID); err != nil {
		t.Fatal(err)
	}
	expectRedirect(t, adm.postForm("/admin/media/"+plain.ID+"/delete", nil), "/admin/media?flash=media_deleted")
	expectMediaDirty(t, env.local, false, "delete untagged")
	expectRedirect(t, adm.postForm("/admin/media/"+hall.ID+"/delete", nil), "/admin/media?flash=media_deleted")
	expectMediaDirty(t, env.local, true, "delete tagged")

	// 共通バナーに「フォトギャラリー／メディア」が出る。
	page := adm.get("/admin/articles")
	expectContains(t, page.Body, `id="remote-dirty-banner"`, `data-remote-dirty-item="media">フォトギャラリー／メディア</span>`, `id="remote-push-settings"`)
}

// 公開サーバーに接続していなければ、メディアの変更を未反映として記録しない。
func TestMediaChangesWithoutRemoteDoNotMarkDirty(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	m := adm.mustUpload("gate.png", "正門", "")
	var bulk struct {
		RemoteDirty bool `json:"remote_dirty"`
	}
	res := bulkTags(t, adm, `{"media_ids":["`+m.ID+`"],"add_tags":["学校"]}`)
	expectStatus(t, res, http.StatusOK)
	decodeJSON(t, res.Body, &bulk)
	if bulk.RemoteDirty || len(env.server.remoteDirty()) != 0 {
		t.Errorf("remote_dirty = %v, dirty = %v without a public server", bulk.RemoteDirty, env.server.remoteDirty())
	}
}

// メディアライブラリの画面では、未反映が無くても共通バナーを隠して置き（タグを変えたら media.js が出す）、ほかの画面には
// 未反映が無ければ出さない。
func TestMediaLibraryKeepsHiddenRemoteBanner(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("root", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	page := adm.get("/admin/media")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="remote-dirty-banner"`, `data-remote-dirty-label-media="フォトギャラリー／メディア"`,
		`id="remote-push-settings"`, "/static/js/remote_sync.js")
	if !regexpMatch(`id="remote-dirty-banner"[^>]*\shidden>`, page.Body) {
		t.Error("the standby banner on the media library must be hidden while nothing is unreflected")
	}
	expectNotContains(t, adm.get("/admin/articles").Body, `id="remote-dirty-banner"`)
	// 公開サーバーへ反映できない立場（編集者）には出さない。
	expectNotContains(t, env.local.loggedIn(chief).get("/admin/media").Body, `id="remote-dirty-banner"`)
}

// P1: 「公開サーバーへ反映する」でフォトギャラリーの画像を送れたら「フォトギャラリー／メディア」の未反映を消す。画像を送れ
// なかったときは、反映できたサイト基本設定・サイト構成の未反映は消し、「フォトギャラリー／メディア」だけを残す。
func TestRemoteSettingsPushClearsMediaDirty(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run("fail="+strconv.FormatBool(fail), func(t *testing.T) {
			env := newRemoteEnv(t)
			if fail {
				// sync_media だけを断る（理由の無い 500）公開サーバー。
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if bytes.Contains(body, []byte(`"action":"`+SyncActionSyncMedia+`"`)) {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = w.Write([]byte(`{"message":"保存できませんでした"}`))
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					env.remote.server.Handler().ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				cfg := remoteConfig{URL: proxy.URL, SyncPath: env.remote.server.cfg.SyncAPIPath, Token: env.remote.token("root", auth.RoleAdmin)}
				if err := env.local.server.saveRemoteConfig(cfg); err != nil {
					t.Fatal(err)
				}
			} else {
				env.configure("root", auth.RoleAdmin)
			}
			adm := env.local.loggedIn(admin)
			m := adm.mustUpload("gate.png", "正門", "")
			form := structureForm(nil, "")
			form.Set("top_gallery_tag", "学校")
			expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")
			expectStatus(t, bulkTags(t, adm, `{"media_ids":["`+m.ID+`"],"add_tags":["学校"]}`), http.StatusOK)
			if got := env.local.server.remoteDirty(); !reflect.DeepEqual(got, []string{remoteDirtyStructure, remoteDirtyMedia}) {
				t.Fatalf("dirty before push = %v", got)
			}

			var out struct {
				Message string `json:"message"`
				Partial bool   `json:"partial"`
			}
			res := adm.postJSON("/admin/remote/settings/push", nil, &out)
			if !fail {
				expectStatus(t, res, http.StatusOK)
				if got := env.local.server.remoteDirty(); len(got) != 0 {
					t.Errorf("dirty after push = %v, want none", got)
				}
				if n := env.remote.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND ai_tags = '学校'`, m.ID); n != 1 {
					t.Errorf("remote media rows = %d, want 1", n)
				}
				return
			}
			expectStatus(t, res, http.StatusBadGateway)
			if !out.Partial || !strings.Contains(out.Message, "フォトギャラリーの画像を反映できませんでした") {
				t.Errorf("push = %+v", out)
			}
			if got := env.local.server.remoteDirty(); !reflect.DeepEqual(got, []string{remoteDirtyMedia}) {
				t.Errorf("dirty after partial push = %v, want only media", got)
			}
		})
	}
}

// P2: sync_media の tag_media_ids は、このトークンの同期ユーザーが保存した画像のうち、そのタグが付いているのに一覧に無い画像
// からタグだけを外す。画像の記録とファイルは消さず、ほかの招待（ほかの PC）が送った画像と公開サーバーで直接登録した画像には
// 触れない。記録の無い（タグを外すだけの）要求も受け付ける。
func TestSyncAPISyncMediaRemovesUnlistedTags(t *testing.T) {
	env := newSyncEnv(t)
	tokA := env.token("chief", auth.RoleEditor)
	tokB := env.token("other-pc", auth.RoleEditor)
	imgA, imgB := localImages(t, 64, 48), localImages(t, 40, 30)
	created := time.Date(2024, 4, 1, 9, 30, 0, 0, time.UTC)
	keep := syncMediaFor("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", imgA, "残す", "", "学校, 景色", created)
	drop := syncMediaFor("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2", imgA, "外す", "", "学校", created)
	other := syncMediaFor("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1", imgB, "ほかのPC", "", "学校", created)
	for _, c := range []struct {
		tok    string
		media  []SyncMedia
		images []SyncImage
	}{{tokA, []SyncMedia{keep, drop}, imgA}, {tokB, []SyncMedia{other}, imgB}} {
		rec := env.call(c.tok, SyncRequest{Action: SyncActionSyncMedia, Media: c.media, Images: c.images})
		expectSyncStatus(t, rec, http.StatusOK)
	}
	// 公開サーバーで直接登録した画像。
	env.insertMedia("cccccccccccccccccccccccccccccc01", "direct.webp", "直接", "", "学校")

	// 1. このPCでは keep だけに「学校」が付いている。
	rec := env.call(tokA, SyncRequest{Action: SyncActionSyncMedia, TagMediaIDs: map[string][]string{"学校": {keep.ID}}})
	expectSyncStatus(t, rec, http.StatusOK)
	out := decodeSync[SyncMediaResponse](t, rec)
	if !reflect.DeepEqual(out.Untagged, []string{drop.ID}) || len(out.Saved) != 0 {
		t.Fatalf("untag response = %+v", out)
	}
	wantTags := map[string]string{keep.ID: "学校, 景色", drop.ID: "", other.ID: "学校", "cccccccccccccccccccccccccccccc01": "学校"}
	for id, want := range wantTags {
		row, ok := env.mediaRow(id)
		if !ok {
			t.Fatalf("media row %s was deleted", id)
		}
		if row.AITags != want {
			t.Errorf("tags of %s = %q, want %q", id, row.AITags, want)
		}
	}

	// 2. 画像が1枚も無くなったタグは空の一覧で送り、ほかのタグは残す。
	rec = env.call(tokA, SyncRequest{Action: SyncActionSyncMedia, TagMediaIDs: map[string][]string{"景色": {}}})
	expectSyncStatus(t, rec, http.StatusOK)
	if out = decodeSync[SyncMediaResponse](t, rec); !reflect.DeepEqual(out.Untagged, []string{keep.ID}) {
		t.Errorf("untag 景色 = %+v", out)
	}
	if row, _ := env.mediaRow(keep.ID); row.AITags != "学校" {
		t.Errorf("tags of keep = %q, want 学校", row.AITags)
	}
	// 3. 記録の保存とタグを外すことを同じ要求で行える（送った記録は一覧に入っていれば外さない）。
	drop.AITags = "学校"
	rec = env.call(tokA, SyncRequest{Action: SyncActionSyncMedia, Media: []SyncMedia{drop},
		TagMediaIDs: map[string][]string{"学校": {drop.ID}}})
	expectSyncStatus(t, rec, http.StatusOK)
	out = decodeSync[SyncMediaResponse](t, rec)
	if !reflect.DeepEqual(out.Saved, []string{drop.ID}) || !reflect.DeepEqual(out.Untagged, []string{keep.ID}) {
		t.Errorf("save and untag = %+v", out)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media`); n != 4 {
		t.Errorf("media rows = %d, want 4 (rows are never deleted)", n)
	}
	for _, img := range append(slices.Clone(imgA), imgB...) {
		expectSyncStatus(t, env.get(UploadURLPrefix+img.Name), http.StatusOK)
	}
}

func TestSyncAPISyncMediaTagListValidation(t *testing.T) {
	env := newSyncEnv(t)
	tok := env.token("chief", auth.RoleEditor)
	tooMany := map[string][]string{}
	for i := range maxSyncMediaTags + 1 {
		tooMany["タグ"+strconv.Itoa(i)] = nil
	}
	cases := []struct {
		name string
		ids  map[string][]string
	}{
		{"tag with comma", map[string][]string{"学校,景色": {}}},
		{"blank tag", map[string][]string{" ": {}}},
		{"bad id", map[string][]string{"学校": {"../etc"}}},
		{"too many tags", tooMany},
		{"too many ids", map[string][]string{"学校": make([]string, maxSyncMediaTagIDs+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionSyncMedia, TagMediaIDs: tc.ids}), http.StatusUnprocessableEntity)
		})
	}
	// タグを外すことも公開サイトを変えるため、ライターのトークンでは断る。
	expectSyncStatus(t, env.call(env.token("yamada", auth.RoleWriter),
		SyncRequest{Action: SyncActionSyncMedia, TagMediaIDs: map[string][]string{"学校": {}}}), http.StatusForbidden)
}

// P2（送受信の結合）: このPCでタグを外した画像・削除した画像は、「公開サーバーへ反映する」で公開サーバーでもタグが外れ、
// 公開サイトのトップページのギャラリーに出なくなる（以前は UPSERT だけのため、外した写真がトップに残り続けた）。
// 公開中の記事の :::gallery のタグも同じ反映で送る。
func TestRemotePushRemovesUntaggedAndDeletedGalleryMedia(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("root", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	keep := adm.mustUpload("keep.png", "残す写真", "")
	untag := adm.mustUpload("untag.png", "タグを外す写真", "")
	gone := adm.mustUpload("gone.png", "削除する写真", "")
	event := adm.mustUpload("event.png", "行事の写真", "")
	expectStatus(t, bulkTags(t, adm, `{"media_ids":["`+keep.ID+`","`+untag.ID+`","`+gone.ID+`"],"add_tags":["学校"]}`), http.StatusOK)
	expectStatus(t, bulkTags(t, adm, `{"media_ids":["`+event.ID+`"],"add_tags":["行事"]}`), http.StatusOK)
	form := structureForm(nil, "")
	form.Set("top_gallery_tag", "学校")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")
	// 公開中の記事の :::gallery のタグ（行事）。記事の公開でも画像を送るため、その後に記録を消して設定の反映で送られるかを見る。
	article := articleFormFor("行事の記録", "event-gallery", "publish")
	article.Set("content_markdown", "当日の写真です。\n\n:::gallery tag=\"行事\"\n:::\n")
	expectRedirect(t, adm.postForm("/admin/articles/new", article), "/admin/articles/")
	if _, err := env.remote.db.Exec(`DELETE FROM media WHERE id = ?`, event.ID); err != nil {
		t.Fatal(err)
	}

	push := func(step string) {
		t.Helper()
		var out struct {
			Message string `json:"message"`
		}
		expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
		if strings.Contains(out.Message, "フォトギャラリー") {
			t.Errorf("%s: push message = %q", step, out.Message)
		}
		if dirty := env.local.server.remoteDirty(); len(dirty) != 0 {
			t.Errorf("%s: dirty after push = %v", step, dirty)
		}
	}
	push("first")
	front := env.remote.get("/").Body.String()
	expectContains(t, front, `alt="残す写真"`, `alt="タグを外す写真"`, `alt="削除する写真"`)
	if n := env.remote.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND ai_tags = '行事'`, event.ID); n != 1 {
		t.Errorf("article gallery media rows = %d, want 1 (sent by the settings push)", n)
	}

	// タグを外す・削除する（どちらも「フォトギャラリー／メディア」が未反映になる）。
	expectStatus(t, bulkTags(t, adm, `{"media_ids":["`+untag.ID+`"],"remove_tags":["学校"]}`), http.StatusOK)
	expectRedirect(t, adm.postForm("/admin/media/"+gone.ID+"/delete", nil), "/admin/media?flash=media_deleted")
	expectStatus(t, bulkTags(t, adm, `{"media_ids":["`+event.ID+`"],"remove_tags":["行事"]}`), http.StatusOK)
	expectMediaDirty(t, env.local, true, "after untag and delete")
	push("second")

	front = env.remote.get("/").Body.String()
	expectContains(t, front, `data-gallery-tag="学校"`, `alt="残す写真"`)
	expectNotContains(t, front, `alt="タグを外す写真"`, `alt="削除する写真"`)
	for id, want := range map[string]string{keep.ID: "学校", untag.ID: "", gone.ID: "", event.ID: ""} {
		var tags string
		if err := env.remote.db.Get(&tags, `SELECT ai_tags FROM media WHERE id = ?`, id); err != nil {
			t.Fatalf("remote row %s: %v (rows are kept)", id, err)
		}
		if tags != want {
			t.Errorf("remote tags of %s = %q, want %q", id, tags, want)
		}
	}
	expectNotContains(t, env.remote.get("/articles/event-gallery").Body.String(), `alt="行事の写真"`)
}

// P3: 表示枚数の選択肢の最大はサーバーの上限（maxTopGalleryLimit）と同じで、画面で選べる枚数はすべて保存できる。
func TestTopGalleryLimitOptionsMatchServerLimit(t *testing.T) {
	if !slices.IsSorted(topGalleryLimitOptions) || topGalleryLimitOptions[len(topGalleryLimitOptions)-1] != maxTopGalleryLimit {
		t.Fatalf("topGalleryLimitOptions = %v, want sorted and ending with %d", topGalleryLimitOptions, maxTopGalleryLimit)
	}
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	page := adm.get("/admin/settings/structure")
	for _, n := range topGalleryLimitOptions {
		expectContains(t, page.Body, `<option value="`+strconv.Itoa(n)+`"`)
	}
	form := structureForm(nil, "")
	form.Set("top_gallery_limit", strconv.Itoa(maxTopGalleryLimit))
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")
	if got := env.server.loadSiteStructure(t.Context()).TopGalleryLimit; got != maxTopGalleryLimit {
		t.Errorf("saved limit = %d, want %d", got, maxTopGalleryLimit)
	}
	expectContains(t, adm.get("/admin/settings/structure").Body, `<option value="24" selected>24</option>`)
	form.Set("top_gallery_limit", strconv.Itoa(maxTopGalleryLimit+1))
	expectStatus(t, adm.postForm("/admin/settings/structure", form), http.StatusUnprocessableEntity)

	// 選択肢に無い保存済みの枚数（同期などで保存したもの）は、選択肢に足して変えずに出す。
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{TopGalleryLimit: 10}); err != nil {
		t.Fatal(err)
	}
	expectContains(t, adm.get("/admin/settings/structure").Body, `<option value="10" selected>10</option>`)
}

// P3: サイト構成のフォトギャラリーの欄に、メディアで使っているタグの候補のボタンと、選んだタグの写真の枚数を出す。写真の
// 無いタグ（打ち間違いなど）を保存したら警告する。
func TestStructureGalleryTagSuggestionsAndWarning(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	// タグの付いた写真が無いときは、メディアライブラリでタグを付けるよう案内する。
	page := adm.get("/admin/settings/structure")
	expectContains(t, page.Body, `id="top-gallery-no-tags"`, "まだタグの付いた写真がありません", `id="top-gallery-media-link" href="/admin/media"`,
		"タグが空のため、トップページにフォトギャラリーは表示されません。")

	env.insertMedia("0000000000000000000000000000a001", "a.webp", "", "", "学校, 景色")
	env.insertMedia("0000000000000000000000000000a002", "b.webp", "", "", "学校")
	form := structureForm(nil, "")
	form.Set("top_gallery_tag", "学校")
	form.Set("top_gallery_limit", "8")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")
	page = adm.get("/admin/settings/structure")
	expectContains(t, page.Body, `id="top-gallery-chips"`,
		`data-gallery-tag-chip="学校" aria-pressed="true">学校</button>`, `data-gallery-tag-chip="景色" aria-pressed="false">景色</button>`,
		`data-count="2">このタグの写真：2枚（トップページには新しい順に最大8枚表示）</p>`,
		`id="top-gallery-media-link" href="/admin/media?tag=%e5%ad%a6%e6%a0%a1"`,
		`data-text-count="このタグの写真：{n}枚（トップページには新しい順に最大{m}枚表示）"`)
	expectNotContains(t, page.Body, `id="structure-warnings"`)

	form.Set("top_gallery_tag", "学校行事")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")
	page = adm.get("/admin/settings/structure")
	expectContains(t, page.Body, `id="structure-warnings"`,
		"トップページのフォトギャラリーのタグ「学校行事」が付いた写真がありません", "このタグの付いた写真はまだありません。")

	// 英語の表示でも案内と警告を英語で出す。
	adm.get("/admin/set-lang?lang=en&return=/admin")
	page = adm.get("/admin/settings/structure")
	expectContains(t, page.Body, `No photos have the tag &#34;学校行事&#34; chosen for the front page photo gallery`,
		"Tags used in the media library (click to choose)")
}

// メディアライブラリには、トップページのフォトギャラリーのタグの説明や「トップページ」の印を出さない（初心者が迷う
// ため、タグの絞り込みに一本化した）。トップページにタグを設定していても同じで、タグでの絞り込みはそのまま使える。
func TestMediaLibraryShowsTopGalleryTag(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	env.insertMedia("0000000000000000000000000000b001", "school.webp", "学校の写真", "", "学校")
	env.insertMedia("0000000000000000000000000000b002", "other.webp", "ほかの写真", "", "風景")

	form := structureForm(nil, "")
	form.Set("top_gallery_tag", "学校")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")
	page := adm.get("/admin/media")
	expectContains(t, page.Body, `id="media-tag-chips"`, `id="media-tag-cloud"`)
	expectNotContains(t, page.Body, `id="media-top-gallery-tag"`, "トップページのフォトギャラリーに使っているタグ",
		`id="media-tags-top-note"`, "トップページのフォトギャラリーに表示されます", `data-top-tag=`, `data-top-label=`, `data-media-top-tag=`)

	filtered := adm.get("/admin/media?tag=" + url.QueryEscape("学校"))
	expectContains(t, filtered.Body, `id="media-tag-filter"`, "タグ「学校」の写真", `alt="学校の写真"`, `<input type="hidden" name="tag" value="学校">`)
	expectNotContains(t, filtered.Body, `alt="ほかの写真"`)
}

// regexpMatch は s が pattern に一致するか。
func regexpMatch(pattern, s string) bool {
	return regexp.MustCompile(pattern).MatchString(s)
}

// decodeJSON は応答の本文 body を v に読む。
func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode: %v\nbody: %.2000s", err, body)
	}
}
