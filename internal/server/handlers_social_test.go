package server

import (
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"sorus/internal/ai"
	"sorus/internal/social"
)

// articleIDBySlug は slug の記事の ID を返す。
func (e *testEnv) articleIDBySlug(slug string) string {
	e.t.Helper()
	var id string
	if err := e.db.Get(&id, `SELECT id FROM articles WHERE slug = ?`, slug); err != nil {
		e.t.Fatalf("article %q: %v", slug, err)
	}
	return id
}

// head は HEAD リクエストを送る。
func (c *client) head(path string) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodHead, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(req)
}

func TestOGImageEndpoint(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()

	res := c.get("/og-image/keion-live.png")
	expectStatus(t, res, http.StatusOK)
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
	img, err := png.Decode(strings.NewReader(res.Body))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if b := img.Bounds(); b.Dx() != social.OGPWidth || b.Dy() != social.OGPHeight {
		t.Errorf("image size = %v, want 1200x630", b)
	}
	etag := res.Header.Get("ETag")
	if etag == "" || !strings.Contains(res.Header.Get("Cache-Control"), "max-age=") {
		t.Errorf("ETag = %q, Cache-Control = %q", etag, res.Header.Get("Cache-Control"))
	}
	if res.Header.Get("Content-Encoding") != "" {
		t.Errorf("PNG should not be gzip-compressed again: %q", res.Header.Get("Content-Encoding"))
	}

	// 2回目はキャッシュから同じ画像を返し、ETag の一致には 304 を返す。
	again := c.get("/og-image/keion-live.png")
	if again.Body != res.Body || again.Header.Get("ETag") != etag {
		t.Error("second request returned a different image")
	}
	if n := env.server.ogp.Len(); n != 1 {
		t.Errorf("ogp cache has %d images, want 1", n)
	}
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/og-image/keion-live.png", nil)
	req.Header.Set("If-None-Match", `"other", `+etag)
	expectStatus(t, c.do(req), http.StatusNotModified)
	expectStatus(t, c.head("/og-image/keion-live.png"), http.StatusOK)

	// 再検証は画像を描かずに ETag だけで 304 を返す（キャッシュに無い画像も描かない）。
	var published time.Time
	if err := env.db.Get(&published, `SELECT published_at FROM articles WHERE slug = 'library-room'`); err != nil {
		t.Fatal(err)
	}
	libraryETag := social.ETag(social.Card{Title: "図書館の新しい自習室", Category: "未分類", SiteName: "テスト大学新聞", Footer: published.In(jst).Format("2006.01.02")})
	req, _ = http.NewRequest(http.MethodGet, env.srv.URL+"/og-image/library-room.png", nil)
	req.Header.Set("If-None-Match", libraryETag)
	before := env.server.ogp.Len()
	if res := c.do(req); res.StatusCode != http.StatusNotModified || res.Header.Get("ETag") != libraryETag {
		t.Errorf("revalidation: status %d etag %q, want 304 %s", res.StatusCode, res.Header.Get("ETag"), libraryETag)
	}
	if env.server.ogp.Len() != before {
		t.Error("revalidation rendered an image")
	}
	if res := c.get("/og-image/library-room.png"); res.Header.Get("ETag") != libraryETag {
		t.Errorf("rendered etag %q, want %s", res.Header.Get("ETag"), libraryETag)
	}

	// 日本語スラッグ（パーセントエンコード）にも対応する。
	expectStatus(t, c.get("/og-image/"+url.PathEscape("学園祭レポート")+".png"), http.StatusOK)

	// タイトルを直すと別の画像（別の ETag）になる。
	if _, err := env.db.Exec(`UPDATE articles SET title = '軽音楽部の新歓ライブ（追記）' WHERE slug = 'keion-live'`); err != nil {
		t.Fatal(err)
	}
	if res := c.get("/og-image/keion-live.png"); res.Header.Get("ETag") == etag {
		t.Error("ETag did not change after the title changed")
	}

	// 公開中でない記事・存在しない記事・拡張子の違うパスは 404（HTML の 404 ページではない）。
	for _, path := range []string{"/og-image/secret-draft.png", "/og-image/scheduled.png", "/og-image/trashed.png",
		"/og-image/nothing.png", "/og-image/keion-live.jpg", "/og-image/.png"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusNotFound)
		if strings.Contains(res.Body, "<html") {
			t.Errorf("%s returned an HTML page", path)
		}
	}
}

func TestPublicArticleUsesGeneratedOGImage(t *testing.T) {
	env := newPublicEnv(t)
	c := env.newClient()

	res := c.get("/articles/keion-live")
	expectStatus(t, res, http.StatusOK)
	want := testBaseURL + "/og-image/keion-live.png"
	if got := metaContent(res.Body, "og:image"); got != want {
		t.Errorf("og:image = %q, want %q", got, want)
	}
	if metaContent(res.Body, "og:image:width") != "1200" || metaContent(res.Body, "og:image:height") != "630" ||
		metaContent(res.Body, "og:image:type") != "image/png" || metaContent(res.Body, "twitter:card") != "summary_large_image" {
		t.Errorf("OGP image metadata is missing:\n%.3000s", res.Body)
	}
	if metaContent(res.Body, "twitter:image") != want || metaContent(res.Body, "og:image:alt") != "軽音楽部の新歓ライブ" {
		t.Errorf("twitter:image = %q, og:image:alt = %q", metaContent(res.Body, "twitter:image"), metaContent(res.Body, "og:image:alt"))
	}

	res = c.get("/articles/" + url.PathEscape("学園祭レポート"))
	if got := metaContent(res.Body, "og:image"); got != testBaseURL+"/og-image/"+url.PathEscape("学園祭レポート")+".png" {
		t.Errorf("og:image for Japanese slug = %q", got)
	}
}

// socialTestAI は SNS 告知文を作れる AI の偽物。受け取った依頼を記録する。
type socialTestAI struct {
	fakeAI
	mu    sync.Mutex
	reqs  []ai.SocialPostRequest
	posts *ai.SocialPosts
	err   error
}

func (f *socialTestAI) GenerateSocialPosts(_ context.Context, req ai.SocialPostRequest) (*ai.SocialPosts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.posts, f.err
}

func (f *socialTestAI) lastRequest() ai.SocialPostRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func postSocial(c *client, values url.Values) (*response, socialPostResponse) {
	c.t.Helper()
	res := c.postAPI("/admin/ai/social-post", "application/x-www-form-urlencoded", strings.NewReader(values.Encode()))
	var out socialPostResponse
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
			c.t.Fatalf("decode: %v\n%s", err, res.Body)
		}
	}
	return res, out
}

func TestSocialPostTemplateWithoutAI(t *testing.T) {
	env := newPublicEnv(t)
	anon := env.newClient()
	expectStatus(t, anon.postAPI("/admin/ai/social-post", "application/x-www-form-urlencoded", strings.NewReader("title=x")), http.StatusUnauthorized)

	c := env.loggedIn(studentA)
	id := env.articleIDBySlug("keion-live")
	res, out := postSocial(c, url.Values{"article_id": {id}})
	expectStatus(t, res, http.StatusOK)
	if out.Source != "template" || out.Configured || out.Guide == "" {
		t.Errorf("source=%q configured=%v guide=%q", out.Source, out.Configured, out.Guide)
	}
	articleURL := testBaseURL + "/articles/keion-live"
	if out.URL != articleURL || out.OGImage != testBaseURL+"/og-image/keion-live.png" {
		t.Errorf("url=%q og_image=%q", out.URL, out.OGImage)
	}
	if len(out.XThread) != 2 || !strings.Contains(out.XThread[0], "【サークル】軽音楽部の新歓ライブ") ||
		!strings.Contains(out.XThread[0], "春の新歓ライブをレポート") || !strings.Contains(out.XThread[1], articleURL) {
		t.Errorf("x_thread = %q", out.XThread)
	}
	if len(out.XLengths) != len(out.XThread) || out.XLimit != social.XMaxLength {
		t.Errorf("x_lengths=%v x_limit=%d", out.XLengths, out.XLimit)
	}
	for i, n := range out.XLengths {
		if n != social.XLength(out.XThread[i]) || n > social.XMaxLength {
			t.Errorf("post %d length %d", i, n)
		}
	}
	if !strings.Contains(out.Instagram, "軽音楽部の新歓ライブ") || strings.Contains(out.Instagram, "https://") {
		t.Errorf("instagram = %q", out.Instagram)
	}
	if !strings.HasSuffix(out.LINE, articleURL) || !strings.Contains(strings.Join(out.Hashtags, " "), "#サークル") {
		t.Errorf("line = %q hashtags = %q", out.LINE, out.Hashtags)
	}

	// 抜粋の無い記事は本文の先頭（タグを除いたテキスト）を要約に使う。
	res, out = postSocial(c, url.Values{"article_id": {env.articleIDBySlug("library-room")}})
	expectStatus(t, res, http.StatusOK)
	if !strings.Contains(out.XThread[0], "図書館3階に24時間利用できる自習室がオープンした。") || strings.Contains(out.XThread[0], "<script") {
		t.Errorf("summary from body: %q", out.XThread[0])
	}

	// 下書きは URL・OGP 画像を付けない。エディタで編集中のタイトル・抜粋・カテゴリを優先する。
	draft := env.articleIDBySlug("secret-draft")
	res, out = postSocial(c, url.Values{"article_id": {draft}, "title": {"編集中のタイトル"}, "excerpt": {"編集中の抜粋"}, "category_id": {"2"}})
	expectStatus(t, res, http.StatusOK)
	if out.URL != "" || out.OGImage != "" || len(out.XThread) != 1 {
		t.Errorf("draft: url=%q og=%q thread=%q", out.URL, out.OGImage, out.XThread)
	}
	if !strings.Contains(out.XThread[0], "【サークル】編集中のタイトル") || !strings.Contains(out.XThread[0], "編集中の抜粋") {
		t.Errorf("draft post = %q", out.XThread[0])
	}

	// 執筆チーム内では非公開の記事を共有するため、他の執筆者の下書きの告知文も作れる（URL は付かない）。
	otherWriter := env.loggedIn(studentB)
	res, out = postSocial(otherWriter, url.Values{"article_id": {draft}})
	expectStatus(t, res, http.StatusOK)
	if out.URL != "" {
		t.Errorf("draft by another writer: url=%q", out.URL)
	}

	// 保存前の記事（article_id 無し）もタイトルがあれば作れる。タイトルが無ければ 400、記事が無ければ 404。
	res, out = postSocial(c, url.Values{"title": {"新しい記事"}, "text": {"# 見出し\n\n本文です。"}})
	expectStatus(t, res, http.StatusOK)
	if !strings.Contains(out.XThread[0], "本文です。") {
		t.Errorf("new article post = %q", out.XThread[0])
	}
	res, _ = postSocial(c, url.Values{"text": {"本文だけ"}})
	expectStatus(t, res, http.StatusBadRequest)
	res, _ = postSocial(c, url.Values{"article_id": {"no-such-article"}})
	expectStatus(t, res, http.StatusNotFound)
	// CSRF トークンの無い要求は拒否する。
	expectStatus(t, c.postRaw("/admin/ai/social-post", url.Values{"title": {"x"}}), http.StatusForbidden)
}

func TestSocialPostWithAI(t *testing.T) {
	fake := &socialTestAI{posts: &ai.SocialPosts{
		XThread:   []string{"新歓ライブの熱気をレポート！ #軽音", strings.Repeat("長い投稿", 100)},
		Instagram: "新歓ライブ https://example.com を見てね",
		LINE:      "新歓ライブのレポートです",
		Hashtags:  []string{"軽音", "#新歓", "#新歓"},
	}}
	env := newTestEnvWith(t, Config{AI: fake, BaseURL: testBaseURL, SiteName: "テスト大学新聞"}, nil)
	c := env.loggedIn(chief)
	id := c.createArticle("新歓ライブ", "満員の観客が盛り上がった。")
	expectRedirect(t, c.postForm("/admin/articles/"+id, url.Values{
		"title": {"新歓ライブ"}, "slug": {"shinkan-live"}, "category_id": {"1"},
		"content_markdown": {"満員の観客が盛り上がった。"}, "action": {"publish"},
	}), "/admin/articles/"+id+"/edit")

	res, out := postSocial(c, url.Values{"article_id": {id}})
	expectStatus(t, res, http.StatusOK)
	if out.Source != "ai" || !out.Configured || out.Provider != "Fake (test-model)" {
		t.Errorf("source=%q configured=%v provider=%q", out.Source, out.Configured, out.Provider)
	}
	req := fake.lastRequest()
	articleURL := testBaseURL + "/articles/shinkan-live"
	if req.Title != "新歓ライブ" || req.URL != articleURL || req.Category != "未分類" || req.SiteName != "テスト大学新聞" ||
		!strings.Contains(req.Text, "満員の観客") {
		t.Errorf("AI request = %+v", req)
	}
	// AI の出力は各 SNS の制約に合わせて整える（文字数・URL の追加・Instagram の URL 除去・タグの重複除去）。
	if len(out.XThread) != 3 || !strings.Contains(out.XThread[2], articleURL) {
		t.Errorf("x_thread = %q", out.XThread)
	}
	for i, n := range out.XLengths {
		if n > social.XMaxLength {
			t.Errorf("post %d has length %d", i, n)
		}
	}
	if strings.Contains(out.Instagram, "https://") || !strings.HasSuffix(out.LINE, articleURL) {
		t.Errorf("instagram=%q line=%q", out.Instagram, out.LINE)
	}
	if strings.Join(out.Hashtags, " ") != "#軽音 #新歓" {
		t.Errorf("hashtags = %q", out.Hashtags)
	}

	// AI の失敗は学生記者に分かる文言のエラーにする。
	fake.err = &ai.APIError{StatusCode: http.StatusTooManyRequests, Message: "quota"}
	res, _ = postSocial(c, url.Values{"article_id": {id}})
	expectStatus(t, res, http.StatusServiceUnavailable)
	expectContains(t, res.Body, "AIの利用上限")
	fake.err = errors.New("boom")
	res, _ = postSocial(c, url.Values{"article_id": {id}})
	expectStatus(t, res, http.StatusBadGateway)

	// エディタに SNS 告知文ボタンとモーダルが出る。
	res = c.get("/admin/articles/" + id + "/edit")
	expectContains(t, res.Body, `id="social-open-button"`, `data-article-id="`+id+`"`, `id="social-modal"`, `/static/js/social.js`)
}

func TestEtagMatches(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{`"abc"`, true},
		{`W/"abc"`, true},
		{`"x", "abc"`, true},
		{`*`, true},
		{`"abcd"`, false},
		{``, false},
	}
	for _, tt := range tests {
		if got := etagMatches(tt.header, `"abc"`); got != tt.want {
			t.Errorf("etagMatches(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestMarkdownSummary(t *testing.T) {
	got := markdownSummary("# 見出し\n\n**太字**と[リンク](https://example.com)。<script>x</script>", 100)
	if got != "見出し 太字とリンク。" {
		t.Errorf("summary = %q", got)
	}
	if got := markdownSummary(strings.Repeat("あ", 10), 5); got != "あああああ…" {
		t.Errorf("clipped = %q", got)
	}
	if markdownSummary("  ", 5) != "" {
		t.Error("blank markdown should give an empty summary")
	}
}
