package markdown

import (
	"bytes"
	htmltemplate "html/template"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"sorus/templates"
)

// assertNoEventHandlers は HTML をパースし、on で始まる属性（イベントハンドラ）が無いことを確かめる。
// 属性値の中にエスケープされた文字列として現れるだけのものは問題にしない。
func assertNoEventHandlers(t *testing.T, s string) {
	t.Helper()
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			for _, a := range tok.Attr {
				if strings.HasPrefix(strings.ToLower(a.Key), "on") {
					t.Errorf("event handler attribute %s=%q on <%s>: %s", a.Key, a.Val, tok.Data, s)
				}
			}
		}
	}
}

func TestRender_Gallery(t *testing.T) {
	src := ":::gallery\n" +
		"![春の図書館](/uploads/photo1.webp \"柔らかな陽光が差し込む閲覧スペース\")\n" +
		"![中庭の桜](/uploads/photo2.webp \"満開を迎えた中庭のしだれ桜\")\n" +
		":::\n"
	res := mustRender(t, src)
	want := `<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified">` + "\n" +
		`<div class="gallery-item" tabindex="0" role="button" aria-label="画像を拡大表示: 春の図書館">` +
		`<img src="/uploads/photo1.webp" data-full="/uploads/photo1.webp" alt="春の図書館" loading="lazy" decoding="async">` +
		`<figcaption>柔らかな陽光が差し込む閲覧スペース</figcaption></div>` + "\n" +
		`<div class="gallery-item" tabindex="0" role="button" aria-label="画像を拡大表示: 中庭の桜">` +
		`<img src="/uploads/photo2.webp" data-full="/uploads/photo2.webp" alt="中庭の桜" loading="lazy" decoding="async">` +
		`<figcaption>満開を迎えた中庭のしだれ桜</figcaption></div>` + "\n" +
		`</figure>`
	assertContains(t, res.HTML, want)
	if strings.Contains(res.HTML, "<p>") || strings.Contains(res.HTML, ":::") {
		t.Errorf("gallery markers or paragraphs leaked: %s", res.HTML)
	}
}

func TestRender_GalleryWithoutCaption(t *testing.T) {
	res := mustRender(t, ":::gallery\n![夕焼け](/uploads/a.webp)\n:::")
	assertContains(t, res.HTML,
		`aria-label="画像を拡大表示: 夕焼け"`,
		`<img src="/uploads/a.webp" data-full="/uploads/a.webp" alt="夕焼け" loading="lazy" decoding="async"></div>`)
	if strings.Contains(res.HTML, "<figcaption>") {
		t.Errorf("figcaption rendered without title: %s", res.HTML)
	}
}

func TestRender_GalleryAriaLabelFallback(t *testing.T) {
	res := mustRender(t, ":::gallery\n![](/a.webp \"キャプションだけ\")\n![](/b.webp)\n:::")
	assertContains(t, res.HTML,
		`aria-label="画像を拡大表示: キャプションだけ"`,
		`aria-label="画像を拡大表示"><img src="/b.webp"`)
}

func TestRender_GalleryAltTextIsPlain(t *testing.T) {
	res := mustRender(t, ":::gallery\n![**強調**と`コード`と\\[記号\\]](/a.webp \"A &amp; B\")\n:::")
	assertContains(t, res.HTML,
		`alt="強調とコードと[記号]"`,
		`<figcaption>A &amp; B</figcaption>`)
}

func TestRender_GalleryAltTextWithJapanesePunctuation(t *testing.T) {
	// 日本語の読点・句点・感嘆符・括弧などを含む代替テキストが bluemonday で削除されずに残ること。
	res := mustRender(t, ":::gallery\n![春の公園、満開の桜。美しい景色（撮影日：4月）](/a.webp)\n:::")
	assertContains(t, res.HTML, `alt="春の公園、満開の桜。美しい景色（撮影日：4月）"`)
}

func TestRender_GalleryImagesOnSameLineAndLinked(t *testing.T) {
	res := mustRender(t, ":::gallery\n![a](/a.webp) ![b](/b.webp)\n\n[![c](/c.webp)](https://example.com/)\n:::")
	if got := strings.Count(res.HTML, `class="gallery-item"`); got != 3 {
		t.Fatalf("gallery items = %d, want 3: %s", got, res.HTML)
	}
	if strings.Contains(res.HTML, "example.com") {
		t.Errorf("link around gallery image was rendered: %s", res.HTML)
	}
}

func TestRender_GalleryIgnoresNonImageContent(t *testing.T) {
	res := mustRender(t, ":::gallery\n説明文\n![a](/a.webp)\n:::\n\n本文")
	if strings.Contains(res.HTML, "説明文") {
		t.Errorf("non-image text inside gallery was rendered: %s", res.HTML)
	}
	assertContains(t, res.HTML, `alt="a"`, "<p>本文</p>")
}

func TestRender_GallerySurroundingContent(t *testing.T) {
	res := mustRender(t, "前の段落\n:::gallery\n![a](/a.webp)\n:::\n後の段落")
	assertContains(t, res.HTML, "<p>前の段落</p>", `<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified">`, "<p>後の段落</p>")
	if strings.Index(res.HTML, "前の段落") > strings.Index(res.HTML, "<figure") ||
		strings.Index(res.HTML, "<figure") > strings.Index(res.HTML, "後の段落") {
		t.Errorf("unexpected order: %s", res.HTML)
	}
}

func TestRender_MultipleGalleries(t *testing.T) {
	res := mustRender(t, ":::gallery\n![a](/a.webp)\n:::\n\n:::gallery\n![b](/b.webp)\n:::\n")
	if got := strings.Count(res.HTML, "<figure"); got != 2 {
		t.Fatalf("figures = %d, want 2: %s", got, res.HTML)
	}
}

func TestRender_GalleryMarkerVariants(t *testing.T) {
	// 行頭のインデント（3つまで）と行末の空白は許容する。
	res := mustRender(t, "  :::gallery  \n![a](/a.webp)\n  :::  \n")
	assertContains(t, res.HTML, `<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified">`)

	for _, src := range []string{
		":::gallery\n![a](/a.webp)\n",                  // 閉じが無い
		":::galleryx\n![a](/a.webp)\n:::\n",            // 記法名が違う
		":::gallery extra\n![a](/a.webp)\n:::\n",       // 余計な引数
		"    :::gallery\n    ![a](/a.webp)\n    :::\n", // インデントコード
		"```\n:::gallery\n![a](/a.webp)\n:::\n```\n",   // コードブロック内
		"`:::gallery`",
	} {
		res := mustRender(t, src)
		if strings.Contains(res.HTML, "<figure") {
			t.Errorf("Render(%q) produced gallery: %s", src, res.HTML)
		}
	}

	// 閉じが無い場合は後続の本文を飲み込まず、通常の Markdown として出力する。
	res = mustRender(t, ":::gallery\n![a](/a.webp)\n\n## 見出し\n")
	assertContains(t, res.HTML, `<img src="/a.webp" alt="a">`, "見出し</h2>")
}

func TestRender_EmptyGallery(t *testing.T) {
	for _, src := range []string{":::gallery\n:::\n", ":::gallery\n文章だけ\n:::\n"} {
		res := mustRender(t, src)
		if strings.TrimSpace(res.HTML) != "" {
			t.Errorf("Render(%q) = %q, want empty", src, res.HTML)
		}
	}
}

func TestRender_GalleryInsideList(t *testing.T) {
	res := mustRender(t, "- 写真\n\n  :::gallery\n  ![a](/a.webp)\n  :::\n")
	assertContains(t, res.HTML, `<li>`, `<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified">`)
}

func TestRender_GalleryXSS(t *testing.T) {
	src := ":::gallery\n" +
		"![x\" onerror=\"alert(1)](/a.webp \"<script>alert(2)</script>\")\n" +
		"![js](javascript:alert(3))\n" +
		"![data](data:text/html;base64,PHNjcmlwdD4= \"data\")\n" +
		"![q](/b.webp?a=\"onmouseover=alert(4))\n" +
		":::\n"
	res := mustRender(t, src)
	for _, bad := range []string{"<script", "javascript:", "data:text/html", `"onmouseover`} {
		if strings.Contains(res.HTML, bad) {
			t.Errorf("unsafe %q in output: %s", bad, res.HTML)
		}
	}
	assertNoEventHandlers(t, res.HTML)
	if got := strings.Count(res.HTML, `class="gallery-item"`); got != 2 {
		t.Errorf("gallery items = %d, want 2 (dangerous URLs dropped): %s", got, res.HTML)
	}
	assertContains(t, res.HTML, `&lt;script&gt;alert(2)&lt;/script&gt;`)
}

func TestRender_GalleryAttributesOnlyFromGallery(t *testing.T) {
	// 利用者が生 HTML で書いた属性は、ギャラリーと同じ形の安全な値だけを残す。
	// ギャラリーのクラスに別のクラスを混ぜた値は、汎用クラスとしても残さない（figure の汎用クラスは許可している）。
	res := mustRender(t, `<figure class="photo-gallery masonry-grid evil" onclick="x()"><div class="gallery-item other" tabindex="-1" role="link" `+
		`onkeydown="x()"><img src="/a.webp" data-full="javascript:alert(1)" loading="eager" data-x="1"></div></figure>`)
	for _, bad := range []string{"evil", "onclick", "other", "tabindex", "role", "onkeydown", "javascript:", "loading", "data-x"} {
		if strings.Contains(res.HTML, bad) {
			t.Errorf("unexpected %q kept: %s", bad, res.HTML)
		}
	}

	res = mustRender(t, `<img src="/a.webp" data-full="&#x6a;avascript:alert(1)">`+
		`<img src="/b.webp" data-full="https://example.com/b.webp">`+
		`<img src="/c.webp" data-full="/uploads/c-large.webp">`)
	if strings.Contains(res.HTML, "avascript") {
		t.Errorf("encoded javascript URL kept in data-full: %s", res.HTML)
	}
	assertContains(t, res.HTML, `data-full="https://example.com/b.webp"`, `data-full="/uploads/c-large.webp"`)
}

func TestRender_GalleryHeadingsStillCollected(t *testing.T) {
	// ギャラリーの前後の見出しは目次に入り、ギャラリーが見出し抽出を壊さないこと。
	res := mustRender(t, "## 前\n\n:::gallery\n![a](/a.webp)\n:::\n\n## 後\n")
	if len(res.Headings) != 2 {
		t.Fatalf("headings = %+v, want 2", res.Headings)
	}
}

func TestRender_TagGalleryUsesResolver(t *testing.T) {
	called := ""
	res, err := RenderWithGallery(":::gallery tag=\"landscape\" columns=\"3\"\n:::", func(tag string) ([]GalleryImage, error) {
		called = tag
		return []GalleryImage{{URL: "/uploads/a.webp", Alt: `A & B`, Caption: `<sunset>`}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called != "landscape" {
		t.Fatalf("resolver tag = %q", called)
	}
	assertContains(t, res.HTML, `<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified" data-gallery-tag="landscape">`,
		`src="/uploads/a.webp"`, `alt="A &amp; B"`, `<figcaption>&lt;sunset&gt;</figcaption>`)
}

func TestRender_TagGalleryOptions(t *testing.T) {
	var query GalleryQuery
	res, err := RenderWithGalleryQuery(`:::gallery tag="event" limit="6" sort="oldest"
:::`, func(q GalleryQuery) ([]GalleryImage, error) {
		query = q
		return []GalleryImage{{URL: "/event.webp"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if query != (GalleryQuery{Tag: "event", Limit: 6, Sort: "oldest"}) {
		t.Fatalf("query = %+v", query)
	}
	assertContains(t, res.HTML, `src="/event.webp"`)

	for _, tc := range []struct {
		attrs string
		limit int
		sort  string
	}{{`limit="0" sort="bogus"`, 0, "latest"}, {`limit="101" sort="random"`, 100, "random"}, {`limit="4"`, 4, "latest"}} {
		_, err := RenderWithGalleryQuery(":::gallery tag=\"event\" "+tc.attrs+"\n:::", func(q GalleryQuery) ([]GalleryImage, error) {
			if q.Limit != tc.limit || q.Sort != tc.sort {
				t.Errorf("query for %s = %+v, want limit=%d sort=%s", tc.attrs, q, tc.limit, tc.sort)
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRender_TagGalleryEscapesTagAndEmptyResolver(t *testing.T) {
	res, err := RenderWithGallery(":::gallery tag=\"x&amp;quot; onclick=&amp;quot;bad\"\n:::", func(string) ([]GalleryImage, error) {
		return []GalleryImage{{URL: "/a.webp"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.HTML, `" onclick=`) {
		t.Fatalf("unsafe tag attribute: %s", res.HTML)
	}

	res, err = RenderWithGallery(":::gallery tag=\"empty\"\n:::", func(string) ([]GalleryImage, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.HTML, "figure") {
		t.Fatalf("empty gallery rendered: %s", res.HTML)
	}
}

// :::gallery の layout で並べ方を選び、figure の class と data-gallery-layout に出す（photo-gallery-ux.md §2-3）。
// 指定が無い・空・知らない値は既定の justified。段組みには今までの masonry-grid も付ける。
func TestRender_GalleryLayouts(t *testing.T) {
	for _, tc := range []struct {
		attrs, class, layout string
	}{
		{``, "photo-gallery gallery-layout-justified", "justified"},
		{` layout="justified"`, "photo-gallery gallery-layout-justified", "justified"},
		{` layout="masonry"`, "photo-gallery gallery-layout-masonry masonry-grid", "masonry"},
		{` layout="cards"`, "photo-gallery gallery-layout-cards", "cards"},
		{` layout = " cards "`, "photo-gallery gallery-layout-cards", "cards"},
		{` layout=""`, "photo-gallery gallery-layout-justified", "justified"},
		{` layout="grid"`, "photo-gallery gallery-layout-justified", "justified"},
		{` layout="Cards"`, "photo-gallery gallery-layout-justified", "justified"},
		{` layout="cards\" onclick=\"x"`, "", ""}, // 属性の形でない開きの行はギャラリーにしない
	} {
		res := mustRender(t, ":::gallery"+tc.attrs+"\n![a](/a.webp)\n:::\n")
		if tc.class == "" {
			if strings.Contains(res.HTML, "<figure") {
				t.Errorf("attrs %q produced gallery: %s", tc.attrs, res.HTML)
			}
			continue
		}
		assertContains(t, res.HTML, `<figure class="`+tc.class+`" data-gallery-layout="`+tc.layout+`">`)
	}

	// タグのギャラリーでも同じ。tag・limit・sort と並べて書ける（順は問わない）。
	var query GalleryQuery
	res, err := RenderWithGalleryQuery(`:::gallery layout="cards" tag="event" sort="oldest" limit="3"
:::`, func(q GalleryQuery) ([]GalleryImage, error) {
		query = q
		return []GalleryImage{{URL: "/event.webp"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if query != (GalleryQuery{Tag: "event", Limit: 3, Sort: "oldest"}) {
		t.Fatalf("query = %+v", query)
	}
	assertContains(t, res.HTML, `<figure class="photo-gallery gallery-layout-cards" data-gallery-layout="cards" data-gallery-tag="event">`)
}

func TestNormalizeGalleryLayout(t *testing.T) {
	for in, want := range map[string]string{
		"": "justified", "justified": "justified", "masonry": "masonry", "cards": "cards", "grid": "justified", " cards": "justified",
	} {
		if got := NormalizeGalleryLayout(in); got != want {
			t.Errorf("NormalizeGalleryLayout(%q) = %q, want %q", in, got, want)
		}
	}
	if !reflect.DeepEqual(GalleryLayouts, []string{"justified", "masonry", "cards"}) || DefaultGalleryLayout != GalleryLayouts[0] {
		t.Errorf("GalleryLayouts = %v, default %q", GalleryLayouts, DefaultGalleryLayout)
	}
}

// タグのギャラリーは、一覧の src に軽いサムネイル（ThumbURL）、拡大の data-full に高解像度（FullURL）を使い、
// 元画像の寸法を width・height に出す。無い値は URL・属性なしに戻る。
func TestRender_TagGalleryThumbFullAndSize(t *testing.T) {
	res, err := RenderWithGalleryQuery(":::gallery tag=\"t\" layout=\"masonry\"\n:::", func(GalleryQuery) ([]GalleryImage, error) {
		return []GalleryImage{
			{URL: "/uploads/a-1200.webp", ThumbURL: "/uploads/a-800.webp", FullURL: "/uploads/a-1200.webp", Width: 3000, Height: 2000, Alt: "a"},
			{URL: "/uploads/b.webp", Alt: "b"},                                              // サムネイル・寸法が無い
			{URL: "/uploads/c.webp", ThumbURL: "/uploads/c-400.webp", Width: 640, Alt: "c"}, // 高さが不明
			{URL: "/uploads/d.webp", ThumbURL: "javascript:alert(1)", FullURL: "javascript:alert(2)", Alt: "d"},
			{URL: "javascript:alert(3)", Alt: "e"},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, res.HTML,
		`<img src="/uploads/a-800.webp" data-full="/uploads/a-1200.webp" width="3000" height="2000" alt="a" loading="lazy" decoding="async">`,
		`<img src="/uploads/b.webp" data-full="/uploads/b.webp" alt="b" loading="lazy" decoding="async">`,
		`<img src="/uploads/c-400.webp" data-full="/uploads/c.webp" alt="c" loading="lazy" decoding="async">`,
		`<img src="/uploads/d.webp" data-full="/uploads/d.webp" alt="d" loading="lazy" decoding="async">`)
	if strings.Contains(res.HTML, "javascript:") || strings.Contains(res.HTML, `alt="e"`) {
		t.Errorf("dangerous URL kept: %s", res.HTML)
	}
	if got := strings.Count(res.HTML, `class="gallery-item"`); got != 4 {
		t.Errorf("gallery items = %d, want 4: %s", got, res.HTML)
	}
}

// 生 HTML のギャラリーの属性は、gallery.go が出力する形の値だけを残す。
func TestRender_GalleryLayoutAttributesSanitized(t *testing.T) {
	res := mustRender(t, `<figure class="photo-gallery gallery-layout-cards" data-gallery-layout="cards">`+
		`<div class="gallery-item"><img src="/a.webp" width="1200" height="800" decoding="async"></div></figure>`)
	assertContains(t, res.HTML, `<figure class="photo-gallery gallery-layout-cards" data-gallery-layout="cards">`,
		`<img src="/a.webp" width="1200" height="800" decoding="async">`)

	res = mustRender(t, `<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified onclick">`+
		`<img src="/b.webp" width="javascript:x" height="80px;x" decoding="sync"></figure>`+
		`<div data-gallery-layout="cards">x</div>`)
	for _, bad := range []string{"data-gallery-layout", "javascript", "80px", "decoding"} {
		if strings.Contains(res.HTML, bad) {
			t.Errorf("unexpected %q kept: %s", bad, res.HTML)
		}
	}
	assertContains(t, res.HTML, `<figure class="photo-gallery gallery-layout-justified">`)
}

// :::gallery の出力は、公開テンプレートの {{template "gallery"}}（トップページ・テーマのギャラリー）と同じ HTML の形にする。
// CSS・justified_gallery.js・lightbox.js がどちらにも同じように効くように、要素・属性（順も）・文字を比べる。
func TestRender_TagGalleryMatchesPublicTemplate(t *testing.T) {
	src, err := templates.FS.ReadFile("public/partials/gallery.html")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := htmltemplate.New("").Funcs(htmltemplate.FuncMap{
		"galleryLayout": NormalizeGalleryLayout,
		"galleryImages": func(...any) any { return nil },
	}).Parse(string(src))
	if err != nil {
		t.Fatal(err)
	}
	images := []GalleryImage{
		{URL: "/uploads/a-1200.webp", ThumbURL: "/uploads/a-800.webp", FullURL: "/uploads/a-1200.webp", Width: 1600, Height: 900,
			Alt: `春の "図書館" & 中庭`, Caption: "<b>柔らかな</b>陽光"},
		{URL: "/uploads/b.webp", Caption: "キャプションだけ"},
		{URL: "/uploads/c.webp"},
	}
	for _, layout := range append([]string{"", "unknown"}, GalleryLayouts...) {
		var want bytes.Buffer
		if err := tmpl.ExecuteTemplate(&want, "gallery", struct {
			Tag, Layout, Lang string
			Images            []GalleryImage
		}{Tag: "イベント", Layout: layout, Lang: "ja", Images: images}); err != nil {
			t.Fatal(err)
		}
		md := ":::gallery tag=\"イベント\"\n:::"
		if layout != "" {
			md = ":::gallery tag=\"イベント\" layout=\"" + layout + "\"\n:::"
		}
		res, err := RenderWithGalleryQuery(md, func(GalleryQuery) ([]GalleryImage, error) { return images, nil })
		if err != nil {
			t.Fatal(err)
		}
		if got, want := htmlShape(t, res.HTML), htmlShape(t, want.String()); !reflect.DeepEqual(got, want) {
			t.Errorf("layout %q: :::gallery differs from the gallery template\ngot:\n%s\nwant:\n%s", layout, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// htmlShape は HTML を、開始・終了タグ（属性は書かれた順の 名前="値"）と空白以外の文字の並びにする。
func htmlShape(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.TextToken:
			if text := strings.TrimSpace(string(z.Text())); text != "" {
				out = append(out, "text:"+text)
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			line := "<" + tok.Data
			for _, a := range tok.Attr {
				line += " " + a.Key + "=" + `"` + a.Val + `"`
			}
			out = append(out, line+">")
		case html.EndTagToken:
			out = append(out, "</"+z.Token().Data+">")
		}
	}
}
