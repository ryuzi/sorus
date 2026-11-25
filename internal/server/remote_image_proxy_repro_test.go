package server

import (
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"sorus/internal/auth"
)

// imgSourcePattern はプレビューの HTML の <img> の src。
var imgSourcePattern = regexp.MustCompile(`<img[^>]*\ssrc="([^"]*)"`)

// imgSources は html の <img> の src を出てくる順に返す。
func imgSources(html string) []string {
	var out []string
	for _, m := range imgSourcePattern.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// browserLoad は、管理画面に表示した <img src> をブラウザと同じく読み込む。相対 URL は PC の管理画面を起点にし、
// ログインのクッキーは持つが、公開サーバーの閲覧用のパスワード（Basic 認証）は持たない。
func (c *client) browserLoad(src string) *response {
	c.t.Helper()
	base, err := url.Parse(c.env.srv.URL + "/admin/manuscripts")
	if err != nil {
		c.t.Fatal(err)
	}
	ref, err := url.Parse(src)
	if err != nil {
		c.t.Fatalf("img src %q: %v", src, err)
	}
	req, err := http.NewRequest(http.MethodGet, base.ResolveReference(ref).String(), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/*,*/*;q=0.8")
	return c.do(req)
}

// 公開サーバーの閲覧制限（Basic 認証）が有効でも、PC の編集者は校閲待ちの一覧とプレビューで本文の画像を見られる。
// ブラウザは公開サーバーの閲覧用のパスワードを持たないため、<img> は PC の管理画面のログインで読めなければならない。
func TestReproRemotePreviewImagesUnderSiteAccess(t *testing.T) {
	env := &remoteEnv{local: newTestEnv(t), remote: newPublicOnlyTLSEnv(t, nil)}
	images := localImages(t, 1300, 700)
	submitted := decodeSync[SyncSubmitResponse](t, env.remote.call(env.remote.token("tanaka", auth.RoleWriter), submitRequest(SyncArticle{
		Title: "陸上部の記録会", Slug: "track-meet", ContentMarkdown: "自己ベストが続出した。\n\n![記録会](/uploads/" + images[1].Name + ")",
	}, images...)))
	id := submitted.Article.ID
	owner := env.remote.token("owner", auth.RoleAdmin)
	expectOK(t, env.remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))
	expectBasicAuthChallenge(t, env.remote.newClient().get("/uploads/"+images[1].Name))

	env.configure("sato", auth.RoleEditor)
	editor := env.local.loggedIn(chief)
	var preview manuscriptPreview
	expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/preview", nil, &preview), http.StatusOK)
	var list struct {
		Articles []remotePendingItem `json:"articles"`
	}
	expectStatus(t, editor.getJSON("/admin/remote/pending", &list), http.StatusOK)
	if len(list.Articles) != 1 {
		t.Fatalf("pending = %+v", list.Articles)
	}

	for what, html := range map[string]string{"preview": preview.HTML, "pending list": list.Articles[0].HTML} {
		srcs := imgSources(html)
		if len(srcs) != 1 {
			t.Fatalf("%s: img sources = %q in %s", what, srcs, html)
		}
		res := editor.browserLoad(srcs[0])
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/webp" || res.Body != string(images[1].Data) {
			t.Errorf("%s: <img src=%q> = %d %q, want the image", what, srcs[0], res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
}
