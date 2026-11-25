package markdown

import (
	"slices"
	"strings"
	"testing"
)

func TestGalleryYearTags(t *testing.T) {
	got := GalleryYearTags("album, 2023年, 卒業式,2025年, 2023年, 2025年度, 25年, 20250年")
	if !slices.Equal(got, []string{"2025年", "2023年"}) {
		t.Fatalf("GalleryYearTags = %v", got)
	}
	if got := GalleryYearList("2024年,x, 2026年"); got != "2026年 2024年" {
		t.Errorf("GalleryYearList = %q", got)
	}
	if got := GalleryYearTags(""); len(got) != 0 {
		t.Errorf("empty tags = %v", got)
	}
	for in, want := range map[string]bool{"2025年": true, "1999年": true, "2025": false, "2025年度": false, "２０２５年": false, "": false} {
		if ValidGalleryYear(in) != want {
			t.Errorf("ValidGalleryYear(%q) = %v", in, !want)
		}
	}
}

func TestGalleryMoreQueryYear(t *testing.T) {
	q := GalleryMoreQuery(GalleryQuery{Tag: "album", Limit: 24, Year: "2025年"})
	if q != "limit=24&tag=album&year=2025%E5%B9%B4" {
		t.Fatalf("query = %q", q)
	}
	if !galleryMoreURLPattern.MatchString(GalleryAPIPath + "?" + q) {
		t.Error("the year query does not pass the data-gallery-more pattern")
	}
}

func yearImages() []GalleryImage {
	return []GalleryImage{
		{URL: "/a.webp", Alt: "a", Years: "2025年"},
		{URL: "/b.webp", Alt: "b", Years: "2023年 2025年 bogus"},
		{URL: "/c.webp", Alt: "c"},
	}
}

// year_filter="true" のタグのギャラリーは、写真の年を新しい順に集めて figure の直前に「すべて」と年のピルを出し、
// 写真ごとに data-gallery-years を付ける（年の無い写真は空）。
func TestRender_TagGalleryYearFilterPills(t *testing.T) {
	for _, attr := range []string{`year_filter="true"`, `year_pills="true"`, `year_filter="1"`} {
		res, err := RenderWithGalleryQuery(":::gallery tag=\"album\" "+attr+"\n:::", func(GalleryQuery) ([]GalleryImage, error) {
			return yearImages(), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		pills := `<div class="gallery-year-filter" role="group" aria-label="年で絞り込む">` +
			`<button type="button" class="gallery-year-pill is-active" data-gallery-year="" aria-pressed="true">すべて</button>` +
			`<button type="button" class="gallery-year-pill" data-gallery-year="2025年" aria-pressed="false">2025年</button>` +
			`<button type="button" class="gallery-year-pill" data-gallery-year="2023年" aria-pressed="false">2023年</button></div>` + "\n" +
			`<figure class="photo-gallery gallery-layout-justified" data-gallery-layout="justified" data-gallery-tag="album">`
		assertContains(t, res.HTML, pills,
			`aria-label="画像を拡大表示: a" data-gallery-years="2025年"><img src="/a.webp"`,
			`aria-label="画像を拡大表示: b" data-gallery-years="2025年 2023年"><img src="/b.webp"`,
			`aria-label="画像を拡大表示: c" data-gallery-years=""><img src="/c.webp"`)
		if strings.Contains(res.HTML, "[[sorus-gallery-years") {
			t.Errorf("%s: marker left in HTML", attr)
		}
	}
}

// year_filter が無い・false のギャラリーと、年の付いた写真が無いギャラリーには、ピルも data-gallery-years も出さない。
func TestRender_TagGalleryWithoutYearFilter(t *testing.T) {
	for _, tc := range []struct {
		src    string
		images []GalleryImage
	}{
		{":::gallery tag=\"album\"\n:::", yearImages()},
		{":::gallery tag=\"album\" year_filter=\"false\"\n:::", yearImages()},
		{":::gallery tag=\"album\" year_filter=\"true\"\n:::", []GalleryImage{{URL: "/x.webp"}}},
	} {
		res, err := RenderWithGalleryQuery(tc.src, func(GalleryQuery) ([]GalleryImage, error) { return tc.images, nil })
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.HTML, "gallery-year-filter") || strings.Contains(res.HTML, "gallery-year-pill") {
			t.Errorf("%q: unexpected pills: %s", tc.src, res.HTML)
		}
		if tc.images[0].Years == "" && !strings.Contains(res.HTML, `data-gallery-years=""`) {
			t.Errorf("%q: year filter gallery without years should still mark items", tc.src)
		}
		if tc.images[0].Years != "" && strings.Contains(res.HTML, "data-gallery-years") {
			t.Errorf("%q: data-gallery-years without year_filter", tc.src)
		}
	}
}

// 続きの API で読むギャラリー（本文に最初の一部しか無い）は、ピルの年を opts.Years（タグの写真すべて）から集める。
// 本文にすべての写真がある（静的な書き出し）ときは、出力する写真から集める。
func TestRender_TagGalleryYearFilterUsesYearsResolverWhenPaged(t *testing.T) {
	var asked GalleryQuery
	opts := GalleryOptions{
		MoreURL: GalleryAPIPath,
		Resolver: func(q GalleryQuery) (GalleryPage, error) {
			return GalleryPage{Images: yearImages(), Total: 50}, nil
		},
		Years: func(q GalleryQuery) ([]string, error) {
			asked = q
			return []string{"2019年", "2025年", "bogus", "2021年"}, nil
		},
	}
	res, err := RenderWithGalleryOptions(":::gallery tag=\"album\" min_width=\"800\" year_filter=\"true\"\n:::", opts)
	if err != nil {
		t.Fatal(err)
	}
	if asked != (GalleryQuery{Tag: "album", MinWidth: 800}) {
		t.Errorf("years query = %+v", asked)
	}
	assertContains(t, res.HTML,
		`data-gallery-year="2025年" aria-pressed="false">2025年</button><button type="button" class="gallery-year-pill" data-gallery-year="2021年" aria-pressed="false">2021年</button><button type="button" class="gallery-year-pill" data-gallery-year="2019年"`,
		`data-gallery-total="50" data-gallery-more="/api/gallery?limit=24&amp;min_width=800&amp;tag=album">`)
	if strings.Contains(res.HTML, "2023年</button>") || strings.Contains(res.HTML, "bogus") {
		t.Errorf("pills should come from the years resolver only: %s", res.HTML)
	}

	// 静的な書き出し（MoreURL が空）は写真をすべて出すので、写真の年から集める。
	opts.MoreURL = ""
	asked = GalleryQuery{}
	res, err = RenderWithGalleryOptions(":::gallery tag=\"album\" year_filter=\"true\"\n:::", opts)
	if err != nil {
		t.Fatal(err)
	}
	if asked != (GalleryQuery{}) {
		t.Errorf("static export asked the years resolver: %+v", asked)
	}
	assertContains(t, res.HTML, `data-gallery-year="2023年"`)
}

// 本文に同じ形の印を書いても、この描画の token でなければピルにならない。ピルの元になる年は数字だけに限る。
func TestRender_GalleryYearMarkerCannotBeForged(t *testing.T) {
	res, err := Render("[[sorus-gallery-years:AAAA:2025]]\n\n[[sorus-gallery-years:x:<b>]]")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.HTML, "<button") {
		t.Fatalf("forged marker became pills: %s", res.HTML)
	}
}

// data-gallery-years は年の形の値だけを残す（生 HTML で書いた不正な値は落とす）。
func TestRender_GalleryYearsAttributeSanitized(t *testing.T) {
	res, err := Render(`<div class="gallery-item" data-gallery-years="2025年 &quot;x">a</div><div class="gallery-item" data-gallery-years="2024年">b</div>`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.HTML, `data-gallery-years="2025年`) {
		t.Errorf("invalid data-gallery-years kept: %s", res.HTML)
	}
	assertContains(t, res.HTML, `data-gallery-years="2024年"`)
}
