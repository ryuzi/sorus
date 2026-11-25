package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/database"
	"sorus/internal/image"
	"sorus/internal/markdown"
	"sorus/internal/search"
)

// mockWXR はテスト用の WXR。{{IMG}} は画像サーバーの URL に置き換える。
const mockWXR = `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0"
	xmlns:excerpt="http://wordpress.org/export/1.2/excerpt/"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:wfw="http://wellformedweb.org/CommentAPI/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:wp="http://wordpress.org/export/1.2/">
<channel>
	<title>学生メディア</title>
	<link>https://old.example.com</link>
	<wp:wxr_version>1.2</wp:wxr_version>
	<wp:base_site_url>https://old.example.com</wp:base_site_url>
	<wp:author>
		<wp:author_id>2</wp:author_id>
		<wp:author_login><![CDATA[hanako]]></wp:author_login>
		<wp:author_email><![CDATA[hanako@example.com]]></wp:author_email>
		<wp:author_display_name><![CDATA[山田花子]]></wp:author_display_name>
	</wp:author>
	<wp:category>
		<wp:term_id>3</wp:term_id>
		<wp:category_nicename><![CDATA[%e3%82%b5%e3%83%bc%e3%82%af%e3%83%ab]]></wp:category_nicename>
		<wp:cat_name><![CDATA[サークル]]></wp:cat_name>
	</wp:category>
	<wp:tag>
		<wp:tag_slug><![CDATA[live]]></wp:tag_slug>
		<wp:tag_name><![CDATA[ライブ]]></wp:tag_name>
	</wp:tag>
	<item>
		<title><![CDATA[軽音部ライブレポート]]></title>
		<link>https://old.example.com/2023/04/keion-live/</link>
		<dc:creator><![CDATA[hanako]]></dc:creator>
		<guid isPermaLink="false">https://old.example.com/?p=101</guid>
		<content:encoded><![CDATA[<!-- wp:paragraph -->
<p>春の<strong>定期ライブ</strong>が開催されました。</p>
<!-- /wp:paragraph -->

<!-- wp:heading {"level":2} -->
<h2>セットリスト</h2>
<!-- /wp:heading -->

<!-- wp:image -->
<figure class="wp-block-image"><img src="{{IMG}}/photo.jpg" alt="ステージ写真"/></figure>
<!-- /wp:image -->

<!-- wp:paragraph -->
<p>壊れた画像: <img src="{{IMG}}/missing.jpg" alt="欠損"></p>
<!-- /wp:paragraph -->]]></content:encoded>
		<excerpt:encoded><![CDATA[<p>春ライブの<em>速報</em></p>]]></excerpt:encoded>
		<wp:post_id>101</wp:post_id>
		<wp:post_date><![CDATA[2023-04-10 19:00:00]]></wp:post_date>
		<wp:post_date_gmt><![CDATA[2023-04-10 10:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[keion-live]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_parent>0</wp:post_parent>
		<wp:post_type><![CDATA[post]]></wp:post_type>
		<category domain="category" nicename="%e3%82%b5%e3%83%bc%e3%82%af%e3%83%ab"><![CDATA[サークル]]></category>
		<category domain="post_tag" nicename="live"><![CDATA[ライブ]]></category>
		<wp:postmeta>
			<wp:meta_key><![CDATA[_thumbnail_id]]></wp:meta_key>
			<wp:meta_value><![CDATA[200]]></wp:meta_value>
		</wp:postmeta>
	</item>
	<item>
		<title><![CDATA[下書きの企画書]]></title>
		<link>https://old.example.com/?p=102</link>
		<dc:creator><![CDATA[newbie]]></dc:creator>
		<content:encoded><![CDATA[段落ひとつめ
二行目

段落ふたつめ]]></content:encoded>
		<excerpt:encoded><![CDATA[]]></excerpt:encoded>
		<wp:post_id>102</wp:post_id>
		<wp:post_date><![CDATA[2023-05-01 09:00:00]]></wp:post_date>
		<wp:post_date_gmt><![CDATA[0000-00-00 00:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[]]></wp:post_name>
		<wp:status><![CDATA[draft]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[サークル紹介]]></title>
		<link>https://old.example.com/%e3%82%b5%e3%83%bc%e3%82%af%e3%83%ab%e7%b4%b9%e4%bb%8b/</link>
		<dc:creator><![CDATA[]]></dc:creator>
		<content:encoded><![CDATA[<p>固定ページです。</p>]]></content:encoded>
		<wp:post_id>103</wp:post_id>
		<wp:post_date_gmt><![CDATA[2022-01-01 00:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[%e3%82%b5%e3%83%bc%e3%82%af%e3%83%ab%e7%b4%b9%e4%bb%8b]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[page]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[消した記事]]></title>
		<link>https://old.example.com/?p=104</link>
		<content:encoded><![CDATA[<p>ゴミ箱</p>]]></content:encoded>
		<wp:post_id>104</wp:post_id>
		<wp:status><![CDATA[trash]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[photo-thumb]]></title>
		<link>https://old.example.com/photo-thumb/</link>
		<wp:post_id>200</wp:post_id>
		<wp:post_parent>101</wp:post_parent>
		<wp:status><![CDATA[inherit]]></wp:status>
		<wp:post_type><![CDATA[attachment]]></wp:post_type>
		<wp:attachment_url><![CDATA[{{IMG}}/thumb.png]]></wp:attachment_url>
	</item>
	<item>
		<title><![CDATA[メニュー]]></title>
		<wp:post_id>300</wp:post_id>
		<wp:post_type><![CDATA[nav_menu_item]]></wp:post_type>
	</item>
</channel>
</rss>`

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// imageServer は /photo.jpg と /thumb.png に PNG を返し、それ以外は 404 を返す。
// /photo.jpg は拡張子と実体が異なる（CDN の自動変換を模す）。
func imageServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	data := testPNG(t, 64, 48)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/photo.jpg", "/thumb.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func openDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role)
		VALUES ('admin-id', 'admin', '管理者', 'x', 'admin')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func newTestImporter(t *testing.T, db *sqlx.DB, idx *search.Index, opts Options) (*Importer, string) {
	t.Helper()
	uploadDir := filepath.Join(t.TempDir(), "uploads")
	opts.UploadDir = uploadDir
	opts.Logger = log.New(io.Discard, "", 0)
	return New(db, idx, opts), uploadDir
}

func mockXML(imgBase string) string { return strings.ReplaceAll(mockWXR, "{{IMG}}", imgBase) }

func TestParse(t *testing.T) {
	exp, err := Parse(strings.NewReader(mockXML("https://img.example.com")), ParseOptions{Location: time.FixedZone("JST", 9*3600)})
	if err != nil {
		t.Fatal(err)
	}
	if exp.SiteURL != "https://old.example.com" {
		t.Errorf("SiteURL = %q", exp.SiteURL)
	}
	if len(exp.Authors) != 1 || exp.Authors[0].Login != "hanako" || exp.Authors[0].DisplayName != "山田花子" {
		t.Errorf("Authors = %+v", exp.Authors)
	}
	if len(exp.Categories) != 1 || exp.Categories[0] != (Term{Name: "サークル", Slug: "サークル"}) {
		t.Errorf("Categories = %+v", exp.Categories)
	}
	if len(exp.Tags) != 1 || exp.Tags[0] != (Term{Name: "ライブ", Slug: "live"}) {
		t.Errorf("Tags = %+v", exp.Tags)
	}
	if len(exp.Posts) != 4 {
		t.Fatalf("len(Posts) = %d, want 4 (post, post, page, trash)", len(exp.Posts))
	}
	if len(exp.Attachments) != 1 || exp.Attachments[0].AttachmentURL != "https://img.example.com/thumb.png" || exp.Attachments[0].ParentID != 101 {
		t.Errorf("Attachments = %+v", exp.Attachments)
	}

	p := exp.Posts[0]
	if p.ID != 101 || p.Title != "軽音部ライブレポート" || p.Creator != "hanako" || p.Slug != "keion-live" ||
		p.Status != "publish" || p.Type != "post" || p.Link != "https://old.example.com/2023/04/keion-live/" {
		t.Errorf("post = %+v", p)
	}
	if want := time.Date(2023, 4, 10, 10, 0, 0, 0, time.UTC); !p.Date.Equal(want) {
		t.Errorf("Date = %v, want %v (GMT)", p.Date, want)
	}
	if !strings.Contains(p.ContentHTML, "<strong>定期ライブ</strong>") || !strings.Contains(p.ExcerptHTML, "速報") {
		t.Errorf("content/excerpt mixed up: content=%q excerpt=%q", p.ContentHTML, p.ExcerptHTML)
	}
	if len(p.Categories) != 1 || p.Categories[0].Name != "サークル" || len(p.Tags) != 1 || p.Tags[0].Name != "ライブ" {
		t.Errorf("terms = %+v / %+v", p.Categories, p.Tags)
	}
	if p.Meta["_thumbnail_id"] != "200" {
		t.Errorf("Meta = %+v", p.Meta)
	}

	// GMT が 0000-00-00 なら post_date を Location (JST) で解釈する。
	if want := time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC); !exp.Posts[1].Date.Equal(want) {
		t.Errorf("draft Date = %v, want %v", exp.Posts[1].Date, want)
	}
	if exp.Posts[2].Slug != "サークル紹介" || exp.Posts[2].Type != "page" {
		t.Errorf("page = %+v", exp.Posts[2])
	}
}

func TestParseOtherWXRVersion(t *testing.T) {
	src := strings.ReplaceAll(mockXML("https://img.example.com"), "export/1.2/", "export/1.1/")
	exp, err := Parse(strings.NewReader(src), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Posts) != 4 || exp.Posts[0].ExcerptHTML == "" || exp.Posts[0].ID != 101 {
		t.Errorf("WXR 1.1 not parsed: %+v", exp.Posts)
	}
}

func TestParseRejectsNonWXR(t *testing.T) {
	if _, err := Parse(strings.NewReader(`<html><body>x</body></html>`), ParseOptions{}); err != ErrNotWXR {
		t.Errorf("err = %v, want ErrNotWXR", err)
	}
	if _, err := Parse(strings.NewReader(`<rss><channel><item>`), ParseOptions{}); err == nil {
		t.Error("truncated XML: want error")
	}
}

func TestHTMLToMarkdown(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"gutenberg comments", "<!-- wp:paragraph -->\n<p>本文</p>\n<!-- /wp:paragraph -->", "本文\n"},
		{"headings", "<h1>大</h1><h2>中</h2><h6>小 <em>x</em></h6>", "# 大\n\n## 中\n\n###### 小 *x*\n"},
		{"paragraphs and emphasis", "<p>a <strong>太字</strong> <b>b</b> <em>斜体</em> <i>i</i> <del>消</del></p><p>次</p>",
			"a **太字** **b** *斜体* *i* ~~消~~\n\n次\n"},
		{"emphasis spaces move outside", "<p>a<strong> 太字 </strong>b</p>", "a **太字** b\n"},
		{"unordered nested list", "<ul><li>一<ul><li>入れ子</li></ul></li><li>二</li></ul>", "- 一\n  - 入れ子\n- 二\n"},
		{"ordered list with start", `<ol start="3"><li>三</li><li>四</li></ol>`, "3. 三\n4. 四\n"},
		{"blockquote", "<blockquote><p>引用1</p><p>引用2</p></blockquote>", "> 引用1\n>\n> 引用2\n"},
		{"code block with language", `<pre class="wp-block-code"><code class="language-go">fmt.Println("&lt;x&gt;")
</code></pre>`, "```go\nfmt.Println(\"<x>\")\n```\n"},
		{"code block containing fence", "<pre>```\nx\n```</pre>", "````\n```\nx\n```\n````\n"},
		{"inline code", "<p>run <code>go test</code> and <code>a`b</code></p>", "run `go test` and ``a`b``\n"},
		{"link", `<p><a href="https://example.com/a" title="t">リンク</a></p>`, "[リンク](https://example.com/a \"t\")\n"},
		{"link with spaces", `<p><a href="https://example.com/a b">x</a></p>`, "[x](<https://example.com/a b>)\n"},
		{"javascript link dropped", `<p><a href="javascript:alert(1)">x</a></p>`, "x\n"},
		{"image", `<p><img src="https://example.com/a.jpg" alt="写[真]"></p>`, "![写真](https://example.com/a.jpg)\n"},
		{"linked image", `<a href="https://example.com/big.jpg"><img src="https://example.com/a.jpg" alt="a"></a>`,
			"[![a](https://example.com/a.jpg)](https://example.com/big.jpg)\n"},
		{"table", "<table><thead><tr><th>名前</th><th>学年</th></tr></thead><tbody><tr><td>花子</td><td>2|3 <code>a||b</code></td></tr><tr><td>太郎</td></tr></tbody></table>",
			"| 名前 | 学年 |\n| --- | --- |\n| 花子 | 2\\|3 `a\\|\\|b` |\n| 太郎 |  |\n"},
		{"hr and br", "<p>一<br>二</p><hr><p>三</p>", "一\\\n二\n\n---\n\n三\n"},
		{"script and style dropped", "<p>a</p><script>alert(1)</script><style>p{}</style><p>b</p>", "a\n\nb\n"},
		{"escape markdown in text", "<p># 見出しじゃない *星* [括弧] 1. 番号</p>", "\\# 見出しじゃない \\*星\\* \\[括弧\\] 1. 番号\n"},
		{"escape ordered marker at line start", "<p>2023. 年</p>", "2023\\. 年\n"},
		{"exclamation before link", `<p>すごい!<a href="https://e.com">x</a></p>`, "すごい\\![x](https://e.com)\n"},
		{"caption shortcode", `[caption id="attachment_1" align="aligncenter" width="300"]<img src="https://e.com/a.jpg" alt="a" /> 説明文[/caption]`,
			"![a](https://e.com/a.jpg)\n\n*説明文*\n"},
		{"embed shortcode", "<p>動画</p>[embed]https://www.youtube.com/watch?v=abc[/embed]",
			"動画\n\n[https://www.youtube.com/watch?v=abc](https://www.youtube.com/watch?v=abc)\n"},
		{"gallery shortcode removed", `<p>前</p>[gallery ids="1,2,3"]<p>後</p>`, "前\n\n後\n"},
		{"plain bracket text kept", "<p>[PR] と [注1]</p>", "\\[PR\\] と \\[注1\\]\n"},
		{"classic editor autop", "一段落目\n改行\n\n二段落目", "一段落目\\\n改行\n\n二段落目\n"},
		{"iframe becomes link", `<iframe src="https://www.youtube.com/embed/abc"></iframe>`,
			"[https://www.youtube.com/embed/abc](https://www.youtube.com/embed/abc)\n"},
		{"empty", "<!-- wp:spacer --><div></div><!-- /wp:spacer -->", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTMLToMarkdown(tt.in); got != tt.want {
				t.Errorf("HTMLToMarkdown(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}
}

// 変換結果を CMS の Markdown レンダラに通し、意図した HTML 構造になることを確かめる。
func TestHTMLToMarkdownRendersBack(t *testing.T) {
	src := `<h2>見出し</h2><p>本文 <strong>強調</strong> <a href="https://e.com">リンク</a></p>
<ul><li>一<ul><li>入れ子</li></ul></li></ul>
<blockquote><p>引用</p></blockquote>
<pre><code class="language-go">x := 1</code></pre>
<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>
<p><img src="https://e.com/a.jpg" alt="画像"></p>
<p># ただの文字 *星*</p>`
	res, err := markdown.Render(HTMLToMarkdown(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<h2", "見出し</h2>", "<strong>強調</strong>", `<a href="https://e.com"`, "<li>一\n<ul>\n<li>入れ子</li>",
		"<blockquote>\n<p>引用</p>", "<pre><code>x := 1", "<th>A</th>", "<td>2</td>",
		`<img src="https://e.com/a.jpg" alt="画像"`, "# ただの文字 *星*",
	} {
		if !strings.Contains(res.HTML, want) {
			t.Errorf("rendered HTML missing %q:\n%s", want, res.HTML)
		}
	}
}

func TestExternalImageURLsAndReplace(t *testing.T) {
	md := "![a](https://e.com/a.jpg) ![b](<https://e.com/b c.png>) ![a again](https://e.com/a.jpg \"t\")\n" +
		"![local](/uploads/x.webp) [link](https://e.com/a.jpg?w=300) ![q](https://e.com/a.jpg?w=300)"
	got := ExternalImageURLs(md)
	want := []string{"https://e.com/a.jpg", "https://e.com/a.jpg?w=300"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ExternalImageURLs = %q, want %q", got, want)
	}
	out := ReplaceURLs(md, map[string]string{"https://e.com/a.jpg": "/uploads/A.webp", "https://e.com/a.jpg?w=300": "/uploads/Q.webp"})
	if strings.Contains(out, "https://e.com/a.jpg") {
		t.Errorf("not replaced: %s", out)
	}
	if !strings.Contains(out, "![q](/uploads/Q.webp)") || strings.Contains(out, "/uploads/A.webp?w=300") {
		t.Errorf("longer URL must be replaced first: %s", out)
	}
}

func TestMediaFetcher(t *testing.T) {
	data := testPNG(t, 1600, 20)
	var flaky atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Write(data)
		case "/flaky.png": // 1回目は 503、リトライで成功
			if flaky.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Write(data)
		case "/text.png":
			w.Write([]byte("not an image"))
		case "/big.png":
			w.Write(bytes.Repeat([]byte{0}, 2048))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	f := NewMediaFetcher(srv.Client(), dir, 1<<20)
	ctx := context.Background()

	img, cached, err := f.Fetch(ctx, srv.URL+"/ok.png")
	if err != nil || cached {
		t.Fatalf("Fetch ok: img=%v cached=%v err=%v", img, cached, err)
	}
	if !strings.HasPrefix(img.URL, UploadURLPrefix) || !strings.HasSuffix(img.URL, "-1200.webp") || img.Filename != "ok.png" {
		t.Errorf("img = %+v", img)
	}
	for _, p := range []string{img.Processed.OriginalPath, img.Processed.LargePath, img.Processed.MediumPath, img.Processed.SmallPath} {
		if _, err := os.Stat(filepath.FromSlash(p)); err != nil {
			t.Errorf("missing output %s: %v", p, err)
		}
	}
	if again, cached, err := f.Fetch(ctx, srv.URL+"/ok.png"); err != nil || !cached || again != img {
		t.Errorf("second Fetch should hit cache: cached=%v err=%v", cached, err)
	}

	if _, _, err := f.Fetch(ctx, srv.URL+"/flaky.png"); err != nil {
		t.Errorf("flaky: want success after retry, got %v", err)
	}
	if flaky.Load() != 2 {
		t.Errorf("flaky attempts = %d, want 2", flaky.Load())
	}
	for _, p := range []string{"/missing.png", "/text.png"} {
		if _, _, err := f.Fetch(ctx, srv.URL+p); err == nil {
			t.Errorf("%s: want error", p)
		}
	}
	small := NewMediaFetcher(srv.Client(), dir, 1024)
	if _, _, err := small.Fetch(ctx, srv.URL+"/big.png"); err == nil {
		t.Error("oversized download: want error")
	}
	if _, _, err := f.Fetch(ctx, "file:///etc/passwd"); err == nil {
		t.Error("non-http scheme: want error")
	}
}

func TestImportEndToEnd(t *testing.T) {
	db := openDB(t)
	idx, err := search.NewMemIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	imgSrv, _ := imageServer(t)

	// 既存記事とスラッグが衝突する場合は -2 を付ける。
	if _, err := db.Exec(`INSERT INTO categories (name, slug) VALUES ('既存', 'existing')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES ('old', '既存', 'keion-live', 'admin-id', 1)`); err != nil {
		t.Fatal(err)
	}

	im, uploadDir := newTestImporter(t, db, idx, Options{DownloadImages: true, HTTPClient: imgSrv.Client(),
		Location: time.FixedZone("JST", 9*3600)})
	sum, err := im.Import(context.Background(), strings.NewReader(mockXML(imgSrv.URL)))
	if err != nil {
		t.Fatal(err)
	}

	if sum.Articles != 3 || sum.Published != 2 || sum.Drafts != 1 || sum.Skipped != 1 {
		t.Errorf("articles: %+v", sum)
	}
	if sum.UsersCreated != 2 { // hanako, newbie（固定ページは admin に割り当て）
		t.Errorf("UsersCreated = %d, want 2", sum.UsersCreated)
	}
	if sum.CategoriesCreated != 2 { // サークル, 未分類
		t.Errorf("CategoriesCreated = %d, want 2", sum.CategoriesCreated)
	}
	if sum.ImagesSucceeded != 2 || sum.ImagesFailed != 1 {
		t.Errorf("images: ok=%d failed=%d, want 2/1", sum.ImagesSucceeded, sum.ImagesFailed)
	}
	if sum.Redirects != 6 || sum.Indexed != 2 {
		t.Errorf("redirects=%d indexed=%d, want 6/2", sum.Redirects, sum.Indexed)
	}
	var out bytes.Buffer
	sum.Write(&out)
	for _, want := range []string{"記事:         3 件（公開 2 / 下書き 1 / スキップ 1）", "変換画像:     成功 2 / 失敗 1", "301転送:      6 件", "処理時間"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, out.String())
		}
	}

	svc := article.NewService(db)
	post, err := svc.GetBySlug("keion-live-2")
	if err != nil {
		t.Fatalf("imported post: %v", err)
	}
	if post.Status != article.StatusPublished || post.PublishedAt == nil ||
		!post.PublishedAt.Equal(time.Date(2023, 4, 10, 10, 0, 0, 0, time.UTC)) || post.Excerpt != "春ライブの速報" {
		t.Errorf("post = %+v", post)
	}
	if strings.Contains(post.ContentMarkdown, "wp:") || strings.Contains(post.ContentMarkdown, imgSrv.URL+"/photo.jpg") {
		t.Errorf("markdown not cleaned / image not replaced:\n%s", post.ContentMarkdown)
	}
	if !strings.Contains(post.ContentMarkdown, "![ステージ写真]("+UploadURLPrefix) || !strings.Contains(post.ContentMarkdown, "## セットリスト") {
		t.Errorf("markdown = %s", post.ContentMarkdown)
	}
	// ダウンロードに失敗した画像は元の URL のまま残す。
	if !strings.Contains(post.ContentMarkdown, imgSrv.URL+"/missing.jpg") {
		t.Errorf("failed image URL must be kept:\n%s", post.ContentMarkdown)
	}
	if !strings.Contains(post.ContentHTML, "<strong>定期ライブ</strong>") {
		t.Errorf("content_html = %s", post.ContentHTML)
	}
	if !strings.HasPrefix(post.FeaturedImage, UploadURLPrefix) {
		t.Errorf("featured image = %q, want local WebP", post.FeaturedImage)
	}

	var author struct {
		Username string `db:"username"`
		Display  string `db:"display_name"`
		Role     string `db:"role"`
	}
	if err := db.Get(&author, `SELECT u.username, u.display_name, u.role FROM users u JOIN articles a ON a.author_id = u.id WHERE a.id = ?`, post.ID); err != nil {
		t.Fatal(err)
	}
	if author.Username != "hanako" || author.Display != "山田花子" || author.Role != "writer" {
		t.Errorf("author = %+v", author)
	}
	var catName, catSlug string
	if err := db.QueryRow(`SELECT name, slug FROM categories WHERE id = ?`, post.CategoryID).Scan(&catName, &catSlug); err != nil {
		t.Fatal(err)
	}
	if catName != "サークル" || catSlug != "サークル" {
		t.Errorf("category = %q/%q", catName, catSlug)
	}

	// 下書き: スラッグはタイトルから生成し、カテゴリは「未分類」。
	draft, err := svc.GetBySlug("下書きの企画書")
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if draft.Status != article.StatusDraft || draft.PublishedAt != nil || draft.ContentMarkdown != "段落ひとつめ\\\n二行目\n\n段落ふたつめ\n" {
		t.Errorf("draft = %+v", draft)
	}
	if err := db.QueryRow(`SELECT name FROM categories WHERE id = ?`, draft.CategoryID).Scan(&catName); err != nil || catName != "未分類" {
		t.Errorf("draft category = %q (%v)", catName, err)
	}
	page, err := svc.GetBySlug("サークル紹介")
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page.AuthorID != "admin-id" {
		t.Errorf("page author = %q, want default author admin", page.AuthorID)
	}
	if page.Kind != article.KindPage || post.Kind != article.KindPost || draft.Kind != article.KindPost {
		t.Errorf("kinds: page %q, post %q, draft %q", page.Kind, post.Kind, draft.Kind)
	}

	var mediaCount int
	if err := db.Get(&mediaCount, `SELECT COUNT(*) FROM media WHERE uploaded_by IS NOT NULL AND mime_type = 'image/png'`); err != nil || mediaCount != 2 {
		t.Errorf("media rows = %d (%v), want 2", mediaCount, err)
	}
	var nullTaken int
	if err := db.Get(&nullTaken, `SELECT COUNT(*) FROM media WHERE uploaded_by IS NOT NULL AND taken_at IS NULL`); err != nil || nullTaken != 0 {
		t.Errorf("media with NULL taken_at = %d (%v), want 0", nullTaken, err)
	}
	files, _ := os.ReadDir(uploadDir)
	if len(files) != 8 { // 2枚 × (原寸, 1200, 800, 400)
		t.Errorf("upload files = %d, want 8", len(files))
	}

	redirects := map[string]string{}
	rows, err := db.Queryx(`SELECT old_path, new_path FROM redirects WHERE status_code = 301`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var o, n string
		if err := rows.Scan(&o, &n); err != nil {
			t.Fatal(err)
		}
		redirects[o] = n
	}
	rows.Close()
	pageURL := "/articles/%E3%82%B5%E3%83%BC%E3%82%AF%E3%83%AB%E7%B4%B9%E4%BB%8B"
	wantRedirects := map[string]string{
		"/2023/04/keion-live/": "/articles/keion-live-2",
		"/?p=101":              "/articles/keion-live-2",
		"/?p=102":              "/articles/%E4%B8%8B%E6%9B%B8%E3%81%8D%E3%81%AE%E4%BC%81%E7%94%BB%E6%9B%B8",
		"/サークル紹介/":             pageURL,
		"/?p=103":              pageURL,
		"/?page_id=103":        pageURL,
	}
	if fmt.Sprint(redirects) != fmt.Sprint(wantRedirects) {
		t.Errorf("redirects =\n%v\nwant\n%v", redirects, wantRedirects)
	}

	res, err := idx.Search("定期ライブ", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Hits[0].ID != post.ID {
		t.Errorf("search hits = %+v", res.Hits)
	}
	if res, _ := idx.Search("企画書", 1, 10); res.Total != 0 {
		t.Errorf("draft must not be indexed: %+v", res.Hits)
	}

	// 公開サイトの旧 URL が新しい記事へ 301 転送されることを確かめる。
	handler, err := newServerHandler(db, uploadDir, idx)
	if err != nil {
		t.Fatal(err)
	}
	for old, want := range map[string]string{"/2023/04/keion-live/": "/articles/keion-live-2", "/?p=101": "/articles/keion-live-2"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, old, nil))
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != want {
			t.Errorf("GET %s = %d %q, want 301 %q", old, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	// 本文に写真がある記事はタイトル直下のアイキャッチ（.hero）を出さず、取り込んだ本文の写真をそのまま見せる。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/articles/keion-live-2", nil))
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "定期ライブ") ||
		!strings.Contains(body, `alt="ステージ写真"`) || strings.Contains(body, `<figure class="hero">`) {
		t.Errorf("article page = %d, body has stage photo: %v, hero: %v", rec.Code,
			strings.Contains(body, `alt="ステージ写真"`), strings.Contains(body, `<figure class="hero">`))
	}
	// 取り込んだアイキャッチは一覧のカードでサイズ違い（srcset）付きで表示する。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "-800.webp") {
		t.Errorf("index page = %d, card has responsive image: %v", rec.Code, strings.Contains(rec.Body.String(), "-800.webp"))
	}
}

func TestImportTwiceSkipsImported(t *testing.T) {
	db := openDB(t)
	imgSrv, hits := imageServer(t)
	im, _ := newTestImporter(t, db, nil, Options{DownloadImages: true, HTTPClient: imgSrv.Client()})
	if _, err := im.Import(context.Background(), strings.NewReader(mockXML(imgSrv.URL))); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	sum, err := im.Import(context.Background(), strings.NewReader(mockXML(imgSrv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Articles != 0 || sum.Skipped != 4 || sum.Redirects != 0 || sum.UsersCreated != 0 || sum.CategoriesCreated != 0 {
		t.Errorf("second import = %+v", sum)
	}
	if hits.Load() != before {
		t.Errorf("skipped articles must not download images again (%d -> %d)", before, hits.Load())
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM articles`); err != nil || n != 3 {
		t.Errorf("articles = %d, want 3", n)
	}
}

// Account を指定すると WordPress の著者（hanako・newbie・空の dc:creator）にかかわらずすべてその1人の記事にし、
// ユーザーを作らない。
func TestImportAccount(t *testing.T) {
	db := openDB(t)
	idx, err := search.NewMemIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	im, _ := newTestImporter(t, db, idx, Options{Account: "admin-id", DefaultAuthor: "someone"})
	sum, err := im.Import(context.Background(), strings.NewReader(mockXML("http://127.0.0.1:1")))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Articles != 3 || sum.UsersCreated != 0 || sum.AccountName != "管理者" {
		t.Errorf("summary = %+v, want 3 articles, no users created, AccountName 管理者", sum)
	}
	var users, others int
	if err := db.Get(&users, `SELECT COUNT(*) FROM users`); err != nil || users != 1 {
		t.Errorf("users = %d (%v), want only the seeded admin", users, err)
	}
	if err := db.Get(&others, `SELECT COUNT(*) FROM articles WHERE author_id <> 'admin-id'`); err != nil || others != 0 {
		t.Errorf("articles by others = %d (%v), want 0", others, err)
	}
	var out strings.Builder
	sum.Write(&out)
	if !strings.Contains(out.String(), "記事 3 件を「管理者」さんの記事として取り込みました") || strings.Contains(out.String(), "作成ユーザー") {
		t.Errorf("summary text:\n%s", out.String())
	}
}

// 存在しない Account を指定したら、ユーザーを作らずに取り込みを止める。
func TestImportAccountNotFound(t *testing.T) {
	db := openDB(t)
	im, _ := newTestImporter(t, db, nil, Options{Account: "missing"})
	if _, err := im.Import(context.Background(), strings.NewReader(mockXML("http://127.0.0.1:1"))); err == nil {
		t.Fatal("import with a missing account succeeded")
	}
	for _, table := range []string{"articles", "users"} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM `+table); err != nil {
			t.Fatal(err)
		}
		if want := map[string]int{"articles": 0, "users": 1}[table]; n != want {
			t.Errorf("%s rows = %d, want %d", table, n, want)
		}
	}
}

func TestImportDryRun(t *testing.T) {
	db := openDB(t)
	idx, err := search.NewMemIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	imgSrv, hits := imageServer(t)
	im, uploadDir := newTestImporter(t, db, idx, Options{DryRun: true, DownloadImages: true, HTTPClient: imgSrv.Client()})
	sum, err := im.Import(context.Background(), strings.NewReader(mockXML(imgSrv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if !sum.DryRun || sum.Articles != 3 || sum.Redirects != 6 || sum.Indexed != 0 {
		t.Errorf("dry run summary = %+v", sum)
	}
	for _, table := range []string{"articles", "redirects", "media", "categories"} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM `+table); err != nil || n != 0 {
			t.Errorf("%s rows = %d (%v), want 0", table, n, err)
		}
	}
	var users int
	if err := db.Get(&users, `SELECT COUNT(*) FROM users`); err != nil || users != 1 {
		t.Errorf("users = %d, want only the seeded admin", users)
	}
	if hits.Load() != 0 {
		t.Errorf("dry run downloaded %d images", hits.Load())
	}
	if _, err := os.Stat(uploadDir); !os.IsNotExist(err) {
		t.Errorf("dry run created upload dir: %v", err)
	}
}

func TestImportWithoutImagesAndSkipPages(t *testing.T) {
	db := openDB(t)
	imgSrv, hits := imageServer(t)
	im, _ := newTestImporter(t, db, nil, Options{SkipPages: true})
	sum, err := im.Import(context.Background(), strings.NewReader(mockXML(imgSrv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Articles != 2 || sum.Skipped != 2 || sum.ImagesSucceeded != 0 || hits.Load() != 0 {
		t.Errorf("summary = %+v, hits = %d", sum, hits.Load())
	}
	post, err := article.NewService(db).GetBySlug("keion-live")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(post.ContentMarkdown, imgSrv.URL+"/photo.jpg") || post.FeaturedImage != imgSrv.URL+"/thumb.png" {
		t.Errorf("images must keep original URLs: %q / %q", post.ContentMarkdown, post.FeaturedImage)
	}
}

// pagesWXR は固定ページ (post_type=page) を含む WXR。予約語と重なるスラッグのページと、同じスラッグの記事を持つ。
const pagesWXR = `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:wp="http://wordpress.org/export/1.2/">
<channel>
	<title>研究室</title>
	<link>https://old.example.com</link>
	<wp:wxr_version>1.2</wp:wxr_version>
	<item>
		<title><![CDATA[研究室について]]></title>
		<link>https://old.example.com/about/</link>
		<content:encoded><![CDATA[<p>研究室の紹介です。</p>]]></content:encoded>
		<wp:post_id>11</wp:post_id>
		<wp:post_date_gmt><![CDATA[2022-01-01 00:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[about]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[page]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[管理について]]></title>
		<link>https://old.example.com/admin/</link>
		<content:encoded><![CDATA[<p>運営の体制です。</p>]]></content:encoded>
		<wp:post_id>12</wp:post_id>
		<wp:post_date_gmt><![CDATA[2022-01-02 00:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[Admin]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[page]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[Search]]></title>
		<link>https://old.example.com/?page_id=13</link>
		<content:encoded><![CDATA[<p>検索の使い方。</p>]]></content:encoded>
		<wp:post_id>13</wp:post_id>
		<wp:post_name><![CDATA[]]></wp:post_name>
		<wp:status><![CDATA[draft]]></wp:status>
		<wp:post_type><![CDATA[page]]></wp:post_type>
	</item>
	<item>
		<title><![CDATA[管理画面を更新しました]]></title>
		<link>https://old.example.com/2023/admin/</link>
		<content:encoded><![CDATA[<p>記事です。</p>]]></content:encoded>
		<wp:post_id>14</wp:post_id>
		<wp:post_date_gmt><![CDATA[2023-01-01 00:00:00]]></wp:post_date_gmt>
		<wp:post_name><![CDATA[admin]]></wp:post_name>
		<wp:status><![CDATA[publish]]></wp:status>
		<wp:post_type><![CDATA[post]]></wp:post_type>
	</item>
</channel>
</rss>`

// 固定ページは kind = page として取り込み、予約語と重なるスラッグには決まった接尾辞を付けてサマリーで知らせる。
func TestImportPagesAsKindPage(t *testing.T) {
	db := openDB(t)
	im, uploadDir := newTestImporter(t, db, nil, Options{})
	sum, err := im.Import(context.Background(), strings.NewReader(pagesWXR))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Articles != 4 || sum.Pages != 3 || sum.PageSlugsRenamed != 2 {
		t.Errorf("summary = %+v", sum)
	}
	var out bytes.Buffer
	sum.Write(&out)
	if !strings.Contains(out.String(), "固定ページ:   3 件（予約語のためスラッグを変更 2 件）") {
		t.Errorf("summary output:\n%s", out.String())
	}
	for _, want := range []string{"「admin」はサイトの決まった URL と重なるため「admin-page」", "「search」はサイトの決まった URL と重なるため「search-page」"} {
		if !strings.Contains(strings.Join(sum.Warnings, "\n"), want) {
			t.Errorf("warnings = %q, want %q", sum.Warnings, want)
		}
	}

	svc := article.NewService(db)
	for slug, kind := range map[string]article.Kind{
		"about": article.KindPage, "admin-page": article.KindPage, "search-page": article.KindPage,
		// 記事は /articles/<slug> で配信するため、予約語のスラッグでもそのまま使う。
		"admin": article.KindPost,
	} {
		a, err := svc.GetBySlug(slug)
		if err != nil {
			t.Errorf("GetBySlug(%q): %v", slug, err)
			continue
		}
		if a.Kind != kind {
			t.Errorf("%s: kind = %q, want %q", slug, a.Kind, kind)
		}
	}

	// 同じファイルを取り込み直しても、取り込み済みとしてスキップする（スラッグは変わらない）。
	again, err := im.Import(context.Background(), strings.NewReader(pagesWXR))
	if err != nil {
		t.Fatal(err)
	}
	if again.Articles != 0 || again.Skipped != 4 || again.PageSlugsRenamed != 0 {
		t.Errorf("second import = %+v", again)
	}

	// 公開サイトでは /<slug> で配信し、旧 URL はページへ転送する（旧パーマリンク /admin/ は管理画面が受けるため転送しない）。
	handler, err := newServerHandler(db, uploadDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/admin-page"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "運営の体制です。") {
		t.Errorf("GET /admin-page = %d", rec.Code)
	}
	for old, want := range map[string]string{"/?page_id=12": "/admin-page", "/about/": "/about"} {
		if rec := get(old); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != want {
			t.Errorf("GET %s = %d %q, want 301 %q", old, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

// 同期 API のパスの先頭部分など、Options.ReservedPageSlug で加えた予約語のページも固定の予約語と同じく
// 接尾辞を付けて取り込む。指定しなければ固定の予約語だけを変える。
func TestImportPagesWithConfiguredReservedSlug(t *testing.T) {
	hooksWXR := strings.Replace(pagesWXR, "<wp:post_name><![CDATA[about]]>", "<wp:post_name><![CDATA[hooks]]>", 1)

	db := openDB(t)
	svc := article.NewService(db, article.WithReservedSlugs(article.PathReservedSlug("/hooks/sync")))
	im, _ := newTestImporter(t, db, nil, Options{ReservedPageSlug: svc.IsReservedPageSlug})
	sum, err := im.Import(context.Background(), strings.NewReader(hooksWXR))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pages != 3 || sum.PageSlugsRenamed != 3 {
		t.Errorf("summary = %+v", sum)
	}
	if want := "「hooks」はサイトの決まった URL と重なるため「hooks-page」"; !strings.Contains(strings.Join(sum.Warnings, "\n"), want) {
		t.Errorf("warnings = %q, want %q", sum.Warnings, want)
	}
	if _, err := svc.GetBySlug("hooks"); !errors.Is(err, article.ErrNotFound) {
		t.Errorf("GetBySlug(hooks) = %v, want ErrNotFound", err)
	}
	if a, err := svc.GetBySlug("hooks-page"); err != nil || a.Kind != article.KindPage || a.Status != article.StatusPublished {
		t.Errorf("GetBySlug(hooks-page) = %+v, %v; want a published page", a, err)
	}

	plain := openDB(t)
	im, _ = newTestImporter(t, plain, nil, Options{})
	if sum, err = im.Import(context.Background(), strings.NewReader(hooksWXR)); err != nil {
		t.Fatal(err)
	}
	if sum.PageSlugsRenamed != 2 {
		t.Errorf("default summary = %+v", sum)
	}
	if a, err := article.NewService(plain).GetBySlug("hooks"); err != nil || a.Kind != article.KindPage {
		t.Errorf("default: GetBySlug(hooks) = %+v, %v; want the page kept as hooks", a, err)
	}
}

// DB 登録に失敗したら全体をロールバックし、生成した WebP も削除する。
func TestImportRollsBackOnDBError(t *testing.T) {
	db := openDB(t)
	imgSrv, _ := imageServer(t)
	// 新規ユーザー作成を失敗させる（role の CHECK 制約に反するトリガで代用）。
	if _, err := db.Exec(`CREATE TRIGGER fail_users BEFORE INSERT ON users WHEN NEW.username = 'newbie'
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	im, uploadDir := newTestImporter(t, db, nil, Options{DownloadImages: true, HTTPClient: imgSrv.Client()})
	if _, err := im.Import(context.Background(), strings.NewReader(mockXML(imgSrv.URL))); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
	for _, table := range []string{"articles", "redirects", "media", "categories"} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM `+table); err != nil || n != 0 {
			t.Errorf("%s rows = %d (%v), want 0 after rollback", table, n, err)
		}
	}
	files, _ := os.ReadDir(uploadDir)
	if len(files) != 0 {
		t.Errorf("upload files left after rollback: %d", len(files))
	}
}

func TestMediaFetcher_SSRFProtection(t *testing.T) {
	tmpDir := t.TempDir()
	f := NewMediaFetcher(nil, tmpDir, 0)

	// プライベート / ループバック IP へのアクセスが遮断されること
	targets := []string{
		"http://127.0.0.1:8080/test.png",
		"http://localhost:8080/test.png",
		"http://10.0.0.1/test.png",
		"http://192.168.1.1/test.png",
	}
	for _, target := range targets {
		_, _, err := f.Fetch(context.Background(), target)
		if err == nil {
			t.Errorf("Fetch(%s) expected error due to SSRF protection, got nil", target)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.5", true},
		{"172.16.0.1", true},
		{"192.168.1.100", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"203.0.113.1", true}, // TEST-NET-3（文書用）も拒む
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if got := isPrivateIP(ip); got != tt.want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestMediaTakenAt(t *testing.T) {
	fb := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	exif := time.Date(2019, 6, 7, 8, 9, 10, 0, time.UTC)
	if got := image.TakenAtValue(exif, fb); got != "2019-06-07 08:09:10" {
		t.Errorf("exif: got %q", got)
	}
	if got := image.TakenAtValue(time.Time{}, fb); got != "2020-01-02 03:04:05" {
		t.Errorf("fallback: got %q", got)
	}
}
