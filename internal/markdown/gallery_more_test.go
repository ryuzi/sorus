package markdown

import (
	"fmt"
	"strings"
	"testing"
)

// manyImages は n 枚の写真（/p00.webp …）を返す。
func manyImages(n int) []GalleryImage {
	images := make([]GalleryImage, n)
	for i := range images {
		images[i] = GalleryImage{URL: fmt.Sprintf("/p%02d.webp", i), Width: 1200, Height: 800}
	}
	return images
}

// pagedResolver は総数 total のうち query の Offset・Limit の範囲を返す resolver（API のある公開サイトの代わり）。
func pagedResolver(total int, got *GalleryQuery) GalleryPageResolver {
	all := manyImages(total)
	return func(q GalleryQuery) (GalleryPage, error) {
		*got = q
		end := total
		if q.Limit > 0 {
			end = min(q.Offset+q.Limit, total)
		}
		return GalleryPage{Images: all[min(q.Offset, total):end], Total: total}, nil
	}
}

// min_width は resolver への問い合わせに渡り、数でない・1 未満は 0（絞らない）、上限を超える値は上限にする。
func TestRender_TagGalleryMinWidth(t *testing.T) {
	for _, tc := range []struct {
		attr string
		want int
	}{{`min_width="600"`, 600}, {`min_width="800px"`, 800}, {`min_width="abc"`, 0}, {`min_width="0"`, 0}, {`min_width="99999"`, MaxGalleryMinWidth}, {``, 0}} {
		var got GalleryQuery
		_, err := RenderWithGalleryQuery(":::gallery tag=\"album\" "+tc.attr+"\n:::", func(q GalleryQuery) ([]GalleryImage, error) {
			got = q
			return manyImages(1), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.MinWidth != tc.want {
			t.Errorf("%s: MinWidth = %d, want %d", tc.attr, got.MinWidth, tc.want)
		}
	}
}

// limit を書かないタグのギャラリーは、最初の DefaultGalleryPageSize 枚だけを出し、総数と続きの API・総枚数・
// 「もっと見る（残り N 件）」のボタンを出す。
func TestRender_TagGalleryDefaultLimitWithMore(t *testing.T) {
	var got GalleryQuery
	res, err := RenderWithGalleryOptions(":::gallery tag=\"研究室 行事\" min_width=\"600\" layout=\"masonry\"\n:::",
		GalleryOptions{Resolver: pagedResolver(50, &got), MoreURL: GalleryAPIPath})
	if err != nil {
		t.Fatal(err)
	}
	if got.Limit != DefaultGalleryPageSize || got.Offset != 0 || got.MinWidth != 600 || got.Seed != 0 {
		t.Fatalf("query = %+v", got)
	}
	if n := strings.Count(res.HTML, `class="gallery-item"`); n != DefaultGalleryPageSize {
		t.Errorf("items = %d, want %d", n, DefaultGalleryPageSize)
	}
	assertContains(t, res.HTML,
		`data-gallery-tag="研究室 行事" data-gallery-total="50" data-gallery-more="/api/gallery?limit=24&amp;min_width=600&amp;tag=%E7%A0%94%E7%A9%B6%E5%AE%A4+%E8%A1%8C%E4%BA%8B">`,
		`</figure>`+"\n"+`<p class="gallery-more"><span class="gallery-count">全 50 枚</span><button type="button" class="gallery-more-button">もっと見る（残り 26 件）</button></p>`)
	if strings.Contains(res.HTML, "hidden") || strings.Contains(res.HTML, "sorus-gallery-more") {
		t.Errorf("unexpected hidden items or marker: %s", res.HTML)
	}

	// 総数が最初の枚数以下なら、総数・ボタンは出さない。
	res, err = RenderWithGalleryOptions(":::gallery tag=\"album\"\n:::", GalleryOptions{Resolver: pagedResolver(DefaultGalleryPageSize, &got), MoreURL: GalleryAPIPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"data-gallery-total", "data-gallery-more", "gallery-more", "<button"} {
		if strings.Contains(res.HTML, bad) {
			t.Errorf("gallery of exactly %d images has %q: %s", DefaultGalleryPageSize, bad, res.HTML)
		}
	}
}

// 並び順 random の「もっと見る」は、種で決まる並べ替えにして続きの API にも同じ種を渡す（続きが重ならない）。
func TestRender_TagGalleryRandomMoreUsesSeed(t *testing.T) {
	var got GalleryQuery
	res, err := RenderWithGalleryOptions(":::gallery tag=\"album\" sort=\"random\"\n:::", GalleryOptions{Resolver: pagedResolver(30, &got), MoreURL: GalleryAPIPath})
	if err != nil {
		t.Fatal(err)
	}
	if !ValidGallerySeed(got.Seed) {
		t.Fatalf("seed = %d", got.Seed)
	}
	assertContains(t, res.HTML, fmt.Sprintf(`seed=%d&amp;sort=random&amp;tag=album`, got.Seed))
}

// limit="all" と明示した limit は今までどおり尊重し、「もっと見る」にしない（後方互換）。
func TestRender_TagGalleryExplicitLimitIsRespected(t *testing.T) {
	for _, tc := range []struct {
		attr      string
		wantLimit int
		wantItems int
	}{{`limit="all"`, 0, 50}, {`limit="ALL"`, 0, 50}, {`limit="6"`, 6, 6}, {`limit="30"`, 30, 30}, {`limit="500"`, MaxGalleryLimit, 50}} {
		var got GalleryQuery
		res, err := RenderWithGalleryOptions(":::gallery tag=\"album\" "+tc.attr+"\n:::", GalleryOptions{Resolver: pagedResolver(50, &got), MoreURL: GalleryAPIPath})
		if err != nil {
			t.Fatal(err)
		}
		if got.Limit != tc.wantLimit {
			t.Errorf("%s: Limit = %d, want %d", tc.attr, got.Limit, tc.wantLimit)
		}
		if n := strings.Count(res.HTML, `class="gallery-item"`); n != tc.wantItems {
			t.Errorf("%s: items = %d, want %d", tc.attr, n, tc.wantItems)
		}
		for _, bad := range []string{"data-gallery-total", "data-gallery-more", "gallery-more", "<button", "hidden"} {
			if strings.Contains(res.HTML, bad) {
				t.Errorf("%s: unexpected %q", tc.attr, bad)
			}
		}
	}
}

// 続きの API の無い描画（静的な書き出し・プレビュー。RenderWithGalleryQuery）では、写真をすべて出力して
// DefaultGalleryPageSize 枚より後ろを hidden にし、総枚数とボタンを出す（続きの API の URL は出さない）。
func TestRender_TagGalleryWithoutAPIHidesRest(t *testing.T) {
	var got GalleryQuery
	res, err := RenderWithGalleryQuery(":::gallery tag=\"album\"\n:::", func(q GalleryQuery) ([]GalleryImage, error) {
		got = q
		return manyImages(30), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Limit != 0 {
		t.Fatalf("limit = %d, want 0 (all)", got.Limit)
	}
	if n := strings.Count(res.HTML, `class="gallery-item"`); n != 30 {
		t.Errorf("items = %d, want 30", n)
	}
	if n := strings.Count(res.HTML, `" hidden=""><img`); n != 6 {
		t.Errorf("hidden items = %d, want 6\n%s", n, res.HTML)
	}
	// 25 枚目から hidden。
	if i, j := strings.Index(res.HTML, `/p23.webp`), strings.Index(res.HTML, `hidden=""`); i < 0 || j < i {
		t.Errorf("first hidden item comes before the 24th image")
	}
	assertContains(t, res.HTML, `data-gallery-total="30">`, `<span class="gallery-count">全 30 枚</span><button type="button" class="gallery-more-button">もっと見る（残り 6 件）</button>`)
	if strings.Contains(res.HTML, "data-gallery-more") {
		t.Errorf("gallery without API has data-gallery-more: %s", res.HTML)
	}
}

// 本文に書いた「もっと見る」の目印・ボタン・続きの API の属性は、生成物のふりをしても残らない。
func TestRender_GalleryMoreCannotBeForged(t *testing.T) {
	res := mustRender(t, `[[sorus-gallery-more:AAAA:5]]

<p class="gallery-more"><button type="button" class="gallery-more-button" onclick="x()">押す</button></p>
<figure class="photo-gallery gallery-layout-justified" data-gallery-more="https://evil.example/api/gallery?x" data-gallery-total="1e9">
<div class="gallery-item" hidden onclick="y()"><img src="/a.webp"></div></figure>`)
	for _, bad := range []string{"<button", "onclick", "evil.example", "data-gallery-more", "data-gallery-total"} {
		if strings.Contains(res.HTML, bad) {
			t.Errorf("forged %q kept: %s", bad, res.HTML)
		}
	}
	// 目印の文字はボタンにならず、文字のまま残る。
	assertContains(t, res.HTML, `[[sorus-gallery-more:AAAA:5]]`)
}

// GalleryItemsHTML（続きの API が返す写真）は、本文のタグのギャラリーと同じ形・同じサニタイズで出す。
func TestGalleryItemsHTML(t *testing.T) {
	got := GalleryItemsHTML([]GalleryImage{
		{URL: "/uploads/a-1200.webp", ThumbURL: "/uploads/a-800.webp", Width: 1600, Height: 900, Alt: `A "B"`, Caption: "<i>c</i>"},
		{URL: "javascript:alert(1)"},
	})
	assertContains(t, got, `<div class="gallery-item" tabindex="0" role="button" aria-label="画像を拡大表示: A &#34;B&#34;"><img src="/uploads/a-800.webp" data-full="/uploads/a-1200.webp" width="1600" height="900" alt="A &#34;B&#34;" loading="lazy" decoding="async"><figcaption>&lt;i&gt;c&lt;/i&gt;</figcaption></div>`)
	if strings.Contains(got, "javascript") || strings.Count(got, "gallery-item") != 1 {
		t.Errorf("unsafe item kept: %s", got)
	}
}

func TestGalleryMoreQuery(t *testing.T) {
	for _, tc := range []struct {
		q    GalleryQuery
		want string
	}{
		{GalleryQuery{Tag: "a&b", Sort: "latest", Limit: 24}, "limit=24&tag=a%26b"},
		{GalleryQuery{Tag: "x", Sort: "oldest", MinWidth: 600, Limit: 24}, "limit=24&min_width=600&sort=oldest&tag=x"},
		{GalleryQuery{Tag: "x", Sort: "random", Seed: 7, Limit: 24}, "limit=24&seed=7&sort=random&tag=x"},
	} {
		if got := GalleryMoreQuery(tc.q); got != tc.want {
			t.Errorf("GalleryMoreQuery(%+v) = %q, want %q", tc.q, got, tc.want)
		}
	}
}
