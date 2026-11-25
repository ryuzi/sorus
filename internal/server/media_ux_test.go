package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sorus/internal/ai"
)

// 画像の解析に対応した偽の AI が、解析のインターフェースを満たし続けているか（満たさないと 501 になり、テストが
// 意図と違う理由で失敗する）。
var _ ai.MultimodalAnalyzer = (*fakeVisionAI)(nil)

// hrefAttr は html/template が href に書き出す形（& を &amp; にする）の URL を返す。
func hrefAttr(u string) string { return `href="` + strings.ReplaceAll(u, "&", "&amp;") + `"` }

// ?untagged=1 はタグの無い画像（空白だけのタグも含む）だけに絞る。?tag= と同時に指定したら untagged を優先し、
// 検索語（q）とは組み合わせられる。一覧の画面とメディアピッカー API で同じ条件を使う。
func TestMediaUntaggedFilter(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	env.insertMedia("untagged-empty", "empty.webp", "空のタグ", "", "")
	env.insertMedia("untagged-space", "space.webp", "空白のタグ", "", "   ")
	env.insertMedia("tagged-art", "art.webp", "アートの写真", "", "art")
	env.insertMedia("tagged-cart", "cart.webp", "カートの写真", "", "cart, 春")

	listIDs := func(path string) []string {
		t.Helper()
		res := c.get(path)
		expectStatus(t, res, http.StatusOK)
		var listed mediaListResponse
		decodeJSON(t, res.Body, &listed)
		var ids []string
		for _, it := range listed.Items {
			ids = append(ids, it.ID)
		}
		slices.Sort(ids)
		if listed.Total != len(ids) {
			t.Errorf("%s: total = %d, items = %v", path, listed.Total, ids)
		}
		return ids
	}
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/admin/api/media?untagged=1", []string{"untagged-empty", "untagged-space"}},
		{"/admin/api/media?untagged=1&tag=art", []string{"untagged-empty", "untagged-space"}},
		{"/admin/api/media?untagged=1&q=" + url.QueryEscape("空白"), []string{"untagged-space"}},
		{"/admin/api/media?tag=art", []string{"tagged-art"}},
		{"/admin/api/media?untagged=0", []string{"tagged-art", "tagged-cart", "untagged-empty", "untagged-space"}},
	} {
		if got := listIDs(tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.path, got, tc.want)
		}
	}

	// 一覧の画面: 絞り込みの説明を出し、検索しても絞り込みを残す（hidden の untagged）。タグの無い写真には「タグなし」を出す。
	page := c.get("/admin/media?untagged=1&tag=art")
	expectStatus(t, page, http.StatusOK)
	if n := countCards(page.Body); n != 2 {
		t.Errorf("untagged cards = %d, want 2", n)
	}
	expectContains(t, page.Body, `id="media-untagged-filter"`, "タグ未設定の写真", `<input type="hidden" name="untagged" value="1">`,
		`id="media-tag-untagged" class="media-cloud-chip is-untagged" aria-current="page"`, `media-tags-preview is-empty`, ">タグなし</span>")
	expectNotContains(t, page.Body, `alt="アートの写真"`, `<input type="hidden" name="tag"`, `id="media-tag-filter"`)

	// タグの無い写真が無ければ、そのことを伝える。
	env2 := newTestEnv(t)
	env2.insertMedia("only-tagged", "a.webp", "", "", "art")
	empty := env2.loggedIn(studentA).get("/admin/media?untagged=1")
	expectContains(t, empty.Body, `id="media-empty"`, "タグ未設定の写真はありません。")
}

// ページ送りの URL は、タグ未設定の絞り込みと検索語を引き継ぐ。
func TestMediaUntaggedPagination(t *testing.T) {
	if got, want := mediaPageURL("桜", "art", true, 2), "/admin/media?page=2&q=%E6%A1%9C&untagged=1"; got != want {
		t.Errorf("mediaPageURL(untagged) = %q, want %q", got, want)
	}
	if got, want := mediaPageURL("", "art", false, 1), "/admin/media?tag=art"; got != want {
		t.Errorf("mediaPageURL(tag) = %q, want %q", got, want)
	}
	if got := mediaPageURL("", "", false, 1); got != "/admin/media" {
		t.Errorf("mediaPageURL() = %q", got)
	}

	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	for i := 0; i < mediaPerPage+2; i++ {
		env.insertMedia(fmt.Sprintf("u%03d", i), fmt.Sprintf("u%03d.webp", i), "", "", "")
	}
	env.insertMedia("tagged", "tagged.webp", "", "", "art")
	first := c.get("/admin/media?untagged=1")
	expectContains(t, first.Body, `id="media-next"`, hrefAttr(mediaPageURL("", "", true, 2)))
	second := c.get("/admin/media?untagged=1&page=2")
	if n := countCards(second.Body); n != 2 {
		t.Errorf("second page cards = %d, want 2", n)
	}
	expectContains(t, second.Body, `id="media-prev"`, hrefAttr(mediaPageURL("", "", true, 1)))
	// 空のページからは、絞り込みを残したまま 1 ページ目に戻れる。
	beyond := c.get("/admin/media?untagged=1&page=9")
	expectContains(t, beyond.Body, `id="media-empty"`, hrefAttr(mediaPageURL("", "", true, 1)))
}

// GET /admin/api/media/tags は以前からの tags（タグ名の順）に加え、同じ順のタグごとの枚数（counts）と、タグの無い
// 画像の数（untagged）を返す。1 枚に同じタグが重なっていても 1 枚と数える。
func TestMediaTagsAPICountsAndUntagged(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	env.insertMedia("m1", "1.webp", "", "", "桜, 春")
	env.insertMedia("m2", "2.webp", "", "", "桜")
	env.insertMedia("m3", "3.webp", "", "", "春,春")
	env.insertMedia("m4", "4.webp", "", "", "")
	env.insertMedia("m5", "5.webp", "", "", " ")

	res := c.get("/admin/api/media/tags")
	expectStatus(t, res, http.StatusOK)
	var got mediaTagsResponse
	decodeJSON(t, res.Body, &got)
	want := mediaTagsResponse{
		Tags:     []string{"春", "桜"},
		Counts:   []mediaTagCount{{Tag: "春", Count: 2}, {Tag: "桜", Count: 2}},
		Popular:  []mediaTagCount{{Tag: "春", Count: 2}, {Tag: "桜", Count: 2}},
		Untagged: 2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tags response = %+v, want %+v", got, want)
	}

	// 画像が無ければ空の配列（null ではない）と 0。
	empty := newTestEnv(t).loggedIn(studentA).get("/admin/api/media/tags")
	expectContains(t, empty.Body, `"tags":[]`, `"counts":[]`, `"popular":[]`, `"untagged":0`)
}

// 一覧の上にタグの絞り込み（タグクラウド）を出す。「すべて」「タグ未設定 (N)」と、枚数の多い順のタグ（枚数・3段階の
// 強さ付き）を並べ、選んでいるものに aria-current を付け、どのリンクも検索語を残す。一覧の上には「このページを
// すべて選択」を置く。
func TestMediaLibraryTagCloud(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	for i := 0; i < 5; i++ {
		env.insertMedia(fmt.Sprintf("sakura%d", i), fmt.Sprintf("s%d.webp", i), "", "", "sakura, spring")
	}
	env.insertMedia("spring-only", "sp.webp", "", "", "spring")
	env.insertMedia("rare", "r.webp", "", "", "rare")
	env.insertMedia("none", "n.webp", "", "", "")

	page := c.get("/admin/media")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="media-tag-cloud"`,
		`id="media-tag-all" class="media-cloud-chip" aria-current="page"`,
		`id="media-tag-untagged" class="media-cloud-chip is-untagged"`, `タグ未設定 <span class="media-cloud-count">(1)</span>`,
		hrefAttr("/admin/media?untagged=1"),
		`class="media-cloud-chip media-cloud-level-3" data-cloud-tag="spring">spring <span class="media-cloud-count">(6)</span>`,
		`class="media-cloud-chip media-cloud-level-3" data-cloud-tag="sakura">sakura <span class="media-cloud-count">(5)</span>`,
		`class="media-cloud-chip media-cloud-level-1" data-cloud-tag="rare">rare <span class="media-cloud-count">(1)</span>`,
		hrefAttr("/admin/media?tag=spring"),
		`id="media-select-all"`, "このページをすべて選択", `id="media-bulk-bar"`)
	// 枚数の多い順に並べる。
	if s, k, r := strings.Index(page.Body, `data-cloud-tag="spring"`), strings.Index(page.Body, `data-cloud-tag="sakura"`),
		strings.Index(page.Body, `data-cloud-tag="rare"`); !(s < k && k < r) {
		t.Errorf("tag order: spring=%d sakura=%d rare=%d", s, k, r)
	}
	expectNotContains(t, page.Body, `id="media-tag-cloud-more"`)

	// タグを選ぶと、そのタグに aria-current を付け、検索語を残したリンクにする。
	q := url.QueryEscape("s")
	filtered := c.get("/admin/media?q=" + q + "&tag=sakura")
	expectContains(t, filtered.Body, `data-cloud-tag="sakura" aria-current="page"`,
		hrefAttr(mediaPageURL("s", "", false, 1)), hrefAttr(mediaPageURL("s", "", true, 1)), hrefAttr(mediaPageURL("s", "spring", false, 1)))
	expectNotContains(t, filtered.Body, `id="media-tag-all" class="media-cloud-chip" aria-current="page"`, `data-cloud-tag="spring" aria-current`)
	if n := countCards(filtered.Body); n != 5 {
		t.Errorf("filtered cards = %d, want 5", n)
	}
}

// タグが多いときは mediaTagCloudLimit 個までを並べ、残りは「ほかのタグ」に畳む。選んでいるタグが畳んだ中にあれば最初から開く。
func TestMediaLibraryTagCloudMore(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(studentA)
	for i := 0; i < mediaTagCloudLimit+2; i++ {
		env.insertMedia(fmt.Sprintf("m%02d", i), fmt.Sprintf("%02d.webp", i), "", "", fmt.Sprintf("t%02d", i))
	}
	page := c.get("/admin/media")
	expectContains(t, page.Body, `id="media-tag-cloud-more" class="mt-2">`, "ほかのタグ（2個）")
	more := page.Body[strings.Index(page.Body, `id="media-tag-cloud-more"`):]
	expectContains(t, more, `data-cloud-tag="t30"`, `data-cloud-tag="t31"`)
	expectNotContains(t, more, `data-cloud-tag="t29"`)

	opened := c.get("/admin/media?tag=t31")
	expectContains(t, opened.Body, `id="media-tag-cloud-more" class="mt-2" open>`, `data-cloud-tag="t31" aria-current="page"`)
}

// 画像の数に応じた表示の強さ: いちばん少ないタグは 1、いちばん多いタグは 3、どのタグも同じ枚数なら 2。
func TestMediaTagChipLevels(t *testing.T) {
	chips := mediaTagChips([]mediaTagCount{{"a", 1}, {"b", 5}, {"c", 10}, {"d", 2}}, mediaQuery{Tag: "b"})
	got := map[string]int{}
	var order []string
	for _, ch := range chips {
		got[ch.Tag] = ch.Level
		order = append(order, ch.Tag)
		if ch.Current != (ch.Tag == "b") {
			t.Errorf("chip %s current = %v", ch.Tag, ch.Current)
		}
	}
	if want := map[string]int{"a": 1, "d": 1, "b": 2, "c": 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("levels = %v, want %v", got, want)
	}
	if want := []string{"c", "b", "d", "a"}; !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	for _, ch := range mediaTagChips([]mediaTagCount{{"x", 3}, {"y", 3}}, mediaQuery{Untagged: true}) {
		if ch.Level != 2 || ch.Current {
			t.Errorf("equal counts chip = %+v, want level 2 and not current", ch)
		}
	}
	if chips := mediaTagChips(nil, mediaQuery{}); chips != nil {
		t.Errorf("no tags = %v", chips)
	}
}

// AI の自動生成は今のタグ（トップページ用など）を消さず、AI のタグのうち新しいものだけを後ろに足す。AI には
// サイトでよく使うタグを、よく使う順に渡す。
func TestMediaAnalyzeKeepsExistingTags(t *testing.T) {
	fake := &fakeVisionAI{result: &ai.ImageAnalysis{Alt: "実験をする学生", Caption: "", Tags: []string{"実験", "トップ", "白衣"}}}
	env := newTestEnvWith(t, Config{AI: fake}, nil)
	c := env.loggedIn(studentA)
	m := c.mustUpload("lab.png", "", "")
	var saved mediaItem
	expectStatus(t, c.postJSON("/admin/media/"+m.ID, url.Values{"ai_tags": {"トップ, 実験風景"}}, &saved), http.StatusOK)
	env.insertMedia("other", "other.webp", "", "", "実験風景, 桜")

	res := c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusOK)
	var item mediaItem
	if err := json.Unmarshal([]byte(res.Body), &item); err != nil {
		t.Fatal(err)
	}
	if want := "トップ, 実験風景, 実験, 白衣"; item.AITags != want {
		t.Errorf("tags after analyze = %q, want %q", item.AITags, want)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM media WHERE id = ? AND ai_tags = 'トップ, 実験風景, 実験, 白衣' AND alt = '実験をする学生'`, m.ID); n != 1 {
		t.Error("merged tags were not saved")
	}
	if len(fake.hints) != 1 {
		t.Fatalf("analyzer calls = %d, want 1", len(fake.hints))
	}
	if want := []string{"実験風景", "トップ", "桜"}; !slices.Equal(fake.hints[0], want) {
		t.Errorf("hint tags = %v, want %v", fake.hints[0], want)
	}

	// 二度目の自動生成でも、タグは増えも消えもしない（AI のタグはもう付いている）。
	res = c.postAPI(analyzePath(m.ID), "", nil)
	expectStatus(t, res, http.StatusOK)
	decodeJSON(t, res.Body, &item)
	if item.AITags != "トップ, 実験風景, 実験, 白衣" {
		t.Errorf("tags after second analyze = %q", item.AITags)
	}
}

func TestMergeMediaTags(t *testing.T) {
	long := strings.Repeat("長", maxMediaTagsRunes)
	for _, tc := range []struct{ existing, added, want string }{
		{"", "桜, 春", "桜, 春"},
		{"トップ, 実験風景", "", "トップ, 実験風景"},
		{"トップ, 実験風景", "実験, トップ, 実験", "トップ, 実験風景, 実験"},
		{"a,b，c、 a", "d", "a, b, c, d"},
		// 上限を超えるときは足すタグを捨て、今のタグは長くても削らない。
		{long, "新しい", long},
		{strings.Repeat("あ", maxMediaTagsRunes-4), "いいい, う", strings.Repeat("あ", maxMediaTagsRunes-4) + ", う"},
	} {
		if got := mergeMediaTags(tc.existing, tc.added); got != tc.want {
			t.Errorf("mergeMediaTags(%q, %q) = %q, want %q", tc.existing, tc.added, got, tc.want)
		}
	}
}
