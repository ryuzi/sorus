package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// mediaProxy は公開サーバーの同期 API の前に置くプロキシ。sync_media の要求を数え、old なら sync_media を知らない
// 古い公開サーバーとして 400（未対応の action）で断る。ほかの要求はそのまま公開サーバーに渡す。
type mediaProxy struct {
	srv        *httptest.Server
	mediaCalls atomic.Int32
}

// useMediaProxy はローカル CMS の接続先を、公開サーバーの前に置いた mediaProxy にする（トークンは sub・role で発行する）。
func (e *remoteEnv) useMediaProxy(sub string, role auth.Role, old bool) *mediaProxy {
	e.local.t.Helper()
	p := &mediaProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"action":"`+SyncActionSyncMedia+`"`)) {
			p.mediaCalls.Add(1)
			if old {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"未対応の action です: \"sync_media\""}`))
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		e.remote.server.Handler().ServeHTTP(w, r)
	}))
	e.local.t.Cleanup(p.srv.Close)
	cfg := remoteConfig{URL: p.srv.URL, SyncPath: e.remote.server.cfg.SyncAPIPath, Token: e.remote.token(sub, role)}
	if err := e.local.server.saveRemoteConfig(cfg); err != nil {
		e.local.t.Fatal(err)
	}
	return p
}

// tagMedia はローカルのメディアライブラリの画像 id にタグを付ける。
func (e *testEnv) tagMedia(id, tags string) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE media SET ai_tags = ? WHERE id = ?`, tags, id); err != nil {
		e.t.Fatal(err)
	}
}

// mediaCreatedAt は media の行の created_at を、保存した形の文字列で返す（PC と公開サーバーの比較用）。
func mediaCreatedAt(t *testing.T, e *testEnv, id string) string {
	t.Helper()
	var v string
	if err := e.db.Get(&v, `SELECT strftime('%Y-%m-%d %H:%M:%S', created_at) FROM media WHERE id = ?`, id); err != nil {
		t.Fatalf("created_at of %s: %v", id, err)
	}
	return v
}

func TestGalleryTagsIn(t *testing.T) {
	text := ":::gallery tag=\"学校\"\n:::\n\n" +
		"```\n:::gallery tag=\"コード\"\n:::\n```\n\n" +
		"本文の :::gallery tag=\"文中\" は記法ではない。\n\n" +
		":::gallery tag=\"行事\" limit=\"3\" sort=\"random\"\n:::\n\n" +
		":::gallery tag=\"学校\"\n:::\n\n" +
		":::gallery\n![写真](/uploads/a.webp)\n:::\n"
	if got, want := galleryTagsIn(text), []string{"学校", "行事"}; !reflect.DeepEqual(got, want) {
		t.Errorf("galleryTagsIn = %q, want %q", got, want)
	}
	if got := galleryTagsIn("ギャラリーの無い本文"); len(got) != 0 {
		t.Errorf("galleryTagsIn(no gallery) = %q", got)
	}
}

// 「公開サーバーへ反映する」は、トップページのフォトギャラリーのタグの画像の記録とファイルも送る。以前は記事の添付画像しか
// 送らなかったため、公開サーバーの media が空のままでギャラリーが出なかった（再現: 反映前の公開トップにギャラリーが無い）。
func TestRemoteSettingsPushSyncsTopGalleryMedia(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("root", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	gate := adm.mustUpload("gate.png", "正門", "春の正門")
	hall := adm.mustUpload("hall.png", "講堂", "")
	other := adm.mustUpload("other.png", "別の写真", "")
	env.local.tagMedia(gate.ID, "学校, 景色")
	env.local.tagMedia(hall.ID, "学校")
	env.local.tagMedia(other.ID, "風景")
	// 画像ファイルがローカルに無い記録（公開サーバーから取り込んだ記事の画像など）は送らない。
	env.local.insertMedia("0000000000000000000000000000beef", "lost.webp", "ファイルの無い画像", "", "学校")

	form := structureForm(nil, "")
	setGalleryFormField(form, "tag", "学校")
	setGalleryFormField(form, "limit", "6")
	setGalleryFormField(form, "title", "学校の写真")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")
	expectNotContains(t, env.remote.get("/").Body.String(), `data-gallery-tag="学校"`)

	var out struct {
		Message  string             `json:"message"`
		Settings SyncSettingsValues `json:"settings"`
	}
	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if frontSectionGalleryTag(out.Settings.FrontSections) != "学校" || strings.Contains(out.Message, "フォトギャラリー") {
		t.Fatalf("push = %+v", out)
	}
	for _, m := range []mediaResponse{gate, hall} {
		if n := env.remote.countRows(`SELECT COUNT(*) FROM media WHERE id = ?`, m.ID); n != 1 {
			t.Errorf("remote media %s rows = %d, want 1", m.ID, n)
		}
		if got, want := mediaCreatedAt(t, env.remote.testEnv, m.ID), mediaCreatedAt(t, env.local, m.ID); got != want {
			t.Errorf("remote created_at of %s = %q, want %q", m.ID, got, want)
		}
		for _, f := range env.remote.mediaFiles(m) {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("remote file: %v", err)
			}
		}
	}
	if n := env.remote.countRows(`SELECT COUNT(*) FROM media`); n != 2 {
		t.Errorf("remote media rows = %d, want 2 (untagged and file-less images are not sent)", n)
	}
	front := env.remote.get("/")
	expectSyncStatus(t, front, http.StatusOK)
	expectContains(t, front.Body.String(), `<h2 id="front-gallery-title" class="page-title">学校の写真</h2>`,
		`data-gallery-tag="学校"`, `alt="正門"`, `alt="講堂"`, `春の正門`, gate.LargeURL)
	expectNotContains(t, front.Body.String(), `alt="別の写真"`, `ファイルの無い画像`)
	expectSyncStatus(t, env.remote.get(gate.LargeURL), http.StatusOK)
	// 反映したらサイト構成は未反映でなくなる。
	if dirty := env.local.server.remoteDirty(); slices.Contains(dirty, remoteDirtyStructure) {
		t.Errorf("remote dirty = %v after a successful push", dirty)
	}

	// 2回目からは、公開サーバーにある画像ファイルを送らずに記録だけを更新する。
	if _, err := env.local.db.Exec(`UPDATE media SET alt = '新しい正門' WHERE id = ?`, gate.ID); err != nil {
		t.Fatal(err)
	}
	res, err := env.local.server.pushRemoteMedia(t.Context(), env.local.server.loadRemoteConfig(), []string{"学校"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 2 || res.Files != 0 {
		t.Errorf("second push = %+v, want 2 records and no files", res)
	}
	expectContains(t, env.remote.get("/").Body.String(), `alt="新しい正門"`)
}

// 画像が多いときは、1回の要求のファイル数（maxSyncImages）を超えないように分けて送る。
func TestRemotePushMediaInBatches(t *testing.T) {
	env := newRemoteEnv(t)
	proxy := env.useMediaProxy("root", auth.RoleAdmin, false)
	adm := env.local.loggedIn(admin)
	const count = 20 // 1枚4ファイルで80ファイル。maxSyncImages（64）を超える。
	for i := range count {
		m := adm.mustUpload(fmt.Sprintf("photo-%d.png", i), fmt.Sprintf("写真%d", i), "")
		env.local.tagMedia(m.ID, "行事")
	}
	res, err := env.local.server.pushRemoteMedia(t.Context(), env.local.server.loadRemoteConfig(), []string{"行事"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != count || res.Files != count*4 {
		t.Errorf("push = %+v, want %d records and %d files", res, count, count*4)
	}
	// 記録だけの1回と、ファイルを付けた2回以上。
	if calls := proxy.mediaCalls.Load(); calls < 3 {
		t.Errorf("sync_media calls = %d, want at least 3 (records, then files in batches)", calls)
	}
	if n := env.remote.countRows(`SELECT COUNT(*) FROM media WHERE ai_tags = '行事'`); n != count {
		t.Errorf("remote media rows = %d, want %d", n, count)
	}
	if files := env.remote.uploadedFiles(); len(files) != count*4 {
		t.Errorf("remote files = %d, want %d", len(files), count*4)
	}
}

// sync_media を知らない古い公開サーバーには、サイト基本設定とサイト構成だけを反映し、ギャラリーの画像を送れなかったことを
// 伝える。「フォトギャラリー／メディア」だけを未反映のまま残し、公開サーバーを更新した後にもう一度押せるようにする
// （サイト構成は反映できているため未反映にしない）。
func TestRemoteSettingsPushGalleryMediaOldServer(t *testing.T) {
	env := newRemoteEnv(t)
	proxy := env.useMediaProxy("root", auth.RoleAdmin, true)
	adm := env.local.loggedIn(admin)
	m := adm.mustUpload("gate.png", "正門", "")
	// メディアライブラリでタグを付けると「フォトギャラリー／メディア」が未反映になる。
	expectStatus(t, adm.postAPI("/admin/api/media/bulk-tags", "application/json",
		strings.NewReader(`{"media_ids":["`+m.ID+`"],"add_tags":["学校"]}`)), http.StatusOK)
	form := structureForm(nil, "")
	setGalleryFormField(form, "tag", "学校")
	expectRedirect(t, adm.postForm("/admin/settings/structure", form), "/admin/settings/structure")

	var out struct {
		Message string `json:"message"`
	}
	expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
	if !strings.Contains(out.Message, "公開サーバーの sorus が古いため、フォトギャラリーの画像は反映されていません") {
		t.Errorf("message = %q", out.Message)
	}
	if proxy.mediaCalls.Load() == 0 {
		t.Error("sync_media was not tried")
	}
	if got := frontSectionGalleryTag(env.remote.server.loadSiteStructure(t.Context()).FrontSections); got != "学校" {
		t.Errorf("remote top gallery tag = %q, want 学校", got)
	}
	if dirty := env.local.server.remoteDirty(); !reflect.DeepEqual(dirty, []string{remoteDirtyMedia}) {
		t.Errorf("remote dirty = %v, want only media kept", dirty)
	}
}

// 記事の直接公開では、本文の :::gallery のタグの画像を、記事より先に公開サーバーへ送る。古い公開サーバーには送らずに
// 記事だけを公開する。
func TestRemoteDirectPublishSyncsArticleGalleryMedia(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(fmt.Sprintf("old=%v", old), func(t *testing.T) {
			env := newRemoteEnv(t)
			env.useMediaProxy("editor", auth.RoleEditor, old)
			ed := env.local.loggedIn(chief)
			m := ed.mustUpload("festival.png", "文化祭", "模擬店の様子")
			env.local.tagMedia(m.ID, "文化祭")

			form := articleFormFor("文化祭の記録", "festival-gallery", "publish")
			form.Set("content_markdown", "当日の写真です。\n\n:::gallery tag=\"文化祭\"\n:::\n")
			loc := expectRedirect(t, ed.postForm("/admin/articles/new", form), "/admin/articles/")
			expectFlash(t, loc, "remote_published")
			id := editPathPattern.FindStringSubmatch(loc)[1]
			if a := env.remote.mustGetArticle(id); a.Status != article.StatusPublished {
				t.Fatalf("remote article = %+v", a)
			}

			page := env.remote.get("/articles/festival-gallery")
			expectSyncStatus(t, page, http.StatusOK)
			rows := env.remote.countRows(`SELECT COUNT(*) FROM media WHERE id = ?`, m.ID)
			if old {
				if rows != 0 {
					t.Errorf("old server: remote media rows = %d, want 0", rows)
				}
				return
			}
			if rows != 1 {
				t.Fatalf("remote media rows = %d, want 1", rows)
			}
			expectContains(t, page.Body.String(), `data-gallery-tag="文化祭"`, `alt="文化祭"`, m.LargeURL)
			if _, err := os.Stat(filepath.Join(env.remote.uploadDir, strings.TrimPrefix(m.LargeURL, UploadURLPrefix))); err != nil {
				t.Errorf("remote file: %v", err)
			}
		})
	}
}
