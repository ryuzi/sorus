package static

import (
	"strings"
	"testing"
)

// TestJustifiedGalleryRebalancesOrphanRow は、行揃えのフォトギャラリーが最後の行に取り残された写真
// （8枚目ぽつん）を配り直す仕組みを、justified_gallery.js の中身から
// 確かめる。並べ方そのものは実ブラウザで確認する。
func TestJustifiedGalleryRebalancesOrphanRow(t *testing.T) {
	data, err := FS.ReadFile("js/justified_gallery.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, c := range []struct{ feature, code string }{
		{"最後の行が幅の半分に届かないと配り直す", `var ORPHAN_ROW_RATIO = 0.5;`},
		{"配り直した行の高さの下限", `var MIN_ROW_SCALE = 0.6;`},
		{"行の高さの上限", `var MAX_ROW_SCALE = 1.6;`},
		{"取り残された行の判定", `rowWidth(last, ratios, gap) >= width * ORPHAN_ROW_RATIO`},
		{"行数を1つ減らして配り直す", `balancedRows(part, width, gap, target, m - 1)`},
		{"同じ行数で配り直す", `balancedRows(part, width, gap, target, m)`},
		{"全体で収まらなければ最後の数行だけ配り直す", `var tails = [n, 3, 2];`},
		{"配り直した行は幅いっぱい", `full: true`},
		{"行の分け方は目安の高さに近いもの", `Math.log(h / target)`},
		{"下限を割る分け方は打ち切る", `if (h < minHeight && i < j - 1) break;`},
		{"上限を超える分け方は使わない", `if (h > maxHeight || before === Infinity) continue;`},
		{"並べ直しの入口から配り直しも使える", `balancedRows: balancedRows`},
	} {
		if !strings.Contains(script, c.code) {
			t.Errorf("justified_gallery.js lacks %s (%s)", c.feature, c.code)
		}
	}
	// 配り直しは行を作る関数（rowsFor）の最後で必ず通す。
	if !strings.Contains(script, "return rebalance(rows, ratios, width, gap, target);") {
		t.Error("rowsFor does not rebalance its rows")
	}
}

// TestPublicHeaderScriptFeatures は、公開サイトのヘッダーのスクリプト（public.js）が
// ヘッダーの操作をすべて受け付けることを、中身から確かめる。
func TestPublicHeaderScriptFeatures(t *testing.T) {
	data, err := FS.ReadFile("js/public.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, c := range []struct{ feature, code string }{
		{"ヘッダーの高さの変数", `"--site-header-h"`},
		{"固定ヘッダーの高さの変数", `"--site-header-sticky-h"`},
		{"高さの変化を見張る", `new window.ResizeObserver(measure).observe(header)`},
		{"画面の大きさの変化でも測り直す", `window.addEventListener("resize", measure)`},
		{"aria-expanded で開閉を伝える", `setAttribute("aria-expanded"`},
		{"Esc で閉じる", `e.key === "Escape"`},
		{"矢印キーで子項目へ", `e.key === "ArrowDown"`},
		{"ホバー補助（マウスだけ）", `e.pointerType !== "mouse"`},
		{"外を押すと閉じる", `document.addEventListener("click"`},
		{"スマートフォンのメニューのボタン", `header.querySelector(".nav-toggle")`},
		{"スマートフォンのメニューの開閉", `"is-nav-open"`},
		{"PC と スマートフォンの境目", `"(min-width: 768px)"`},
		{"ドロップダウンの背景をメニューの帯に合わせる", `"--site-nav-bg"`},
	} {
		if !strings.Contains(script, c.code) {
			t.Errorf("public.js lacks %s (%s)", c.feature, c.code)
		}
	}
}

// TestPublicCSSUsesHeaderHeight は、目次の位置と見出しの scroll-margin-top が、public.js の測ったヘッダーの高さに
// 合わせて動くこと（スクリプトが動かないときは今までの値）を public.css の中身から確かめる。
func TestPublicCSSUsesHeaderHeight(t *testing.T) {
	data, err := FS.ReadFile("css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, want := range []string{
		`top: calc(var(--site-header-sticky-h, 4rem) + 1.5rem);`,
		`max-height: calc(100vh - var(--site-header-sticky-h, 4rem) - 3rem);`,
		`scroll-margin-top: calc(var(--site-header-sticky-h, 3.5rem) + 1.5rem);`,
		`.site-menu .submenu-toggle[aria-expanded="true"] + .site-submenu { display: flex; }`,
		`.site-header.is-nav-ready:not(.is-nav-open) .header-inner .search-form, .site-header.is-nav-ready:not(.is-nav-open) .category-nav { display: none; }`,
		`.footer-sitemap-inner {`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("public.css lacks %q", want)
		}
	}
	// 以前の固定の値は残さない。
	for _, old := range []string{"top: 5.5rem;", "scroll-margin-top: 5rem;"} {
		if strings.Contains(css, old) {
			t.Errorf("public.css still has %q", old)
		}
	}
}

// TestJustifiedGalleryLoadMore は :::gallery の「もっと見る」（limit を書かないタグのギャラリー）の仕組みを、
// justified_gallery.js の中身から確かめる。動きそのものは実ブラウザで確認する。
func TestJustifiedGalleryLoadMore(t *testing.T) {
	data, err := FS.ReadFile("js/justified_gallery.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, c := range []struct{ feature, code string }{
		{"まだ見せていない写真は並べない", `el.classList.contains('gallery-item') && !el.hidden`},
		{"続きの API を offset 付きで読む", `url + '&offset=' + shownCount(gallery)`},
		{"読んだ写真を末尾に足す", `insertAdjacentHTML('beforeend', data.html)`},
		{"静的な書き出しでは hidden の写真を順に見せる", `hidden.slice(0, PAGE_SIZE)`},
		{"足したら行揃えを並べ直す", `j.layout(gallery)`},
		{"すべて見せたらボタンを外す", `button.remove()`},
		{"残りの件数をボタンに出す", `もっと見る（残り `},
		{"拡大表示から続きを読む入口", `window.sorusGalleryMore = { load: load, hasMore: hasMore, total: totalOf }`},
	} {
		if !strings.Contains(script, c.code) {
			t.Errorf("justified_gallery.js lacks %s (%s)", c.feature, c.code)
		}
	}
}
