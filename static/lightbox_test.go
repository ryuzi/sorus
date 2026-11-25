package static

import (
	"strings"
	"testing"
)

// TestLightboxScriptFeatures は、公開サイトの拡大表示が所定の操作を
// すべて受け付けることを、lightbox.js の中身から確かめる。動作そのものは実ブラウザで確認する。
func TestLightboxScriptFeatures(t *testing.T) {
	data, err := FS.ReadFile("js/lightbox.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, c := range []struct{ feature, code string }{
		{"左右キーでめくる", `"ArrowLeft"`},
		{"左右キーでめくる", `"ArrowRight"`},
		{"位置カウンター（「もっと見る」の続きも含めた総数）", `" / " + total`},
		{"位置カウンター（「もっと見る」の続きも含めた総数）", `Math.max(items.length, moreTotal())`},
		{"最後の写真から次へ進むと続きを読む", `sorusGalleryMore`},
		{"まだ見せていない写真は対象外", `.gallery-item[hidden]`},
		{"開くと履歴を積む", `history.pushState(`},
		{"「戻る」で閉じる", `"popstate"`},
		{"閉じたら履歴を戻す", `history.back()`},
		{"Esc で閉じる", `"cancel"`},
		{"タッチ操作（スワイプ・ピンチ）", `"pointermove"`},
		{"Ctrl+ホイールで拡大", `"wheel"`},
		{"フォーカストラップ", `e.key === "Tab"`},
		{"View Transitions", `startViewTransition`},
		{"表示切り替え（Hide-on-Tap）", `is-ui-hidden`},
		{"ギャラリー", `.gallery-item`},
		{"トップのギャラリー", `.cover-gallery`},
		{"本文の画像", `.prose`},
		{"任意のグループ", `data-lightbox-group`},
	} {
		if !strings.Contains(script, c.code) {
			t.Errorf("lightbox.js lacks %s (%s)", c.feature, c.code)
		}
	}
}

// TestLightboxSwipeLoadsMoreAtLastLoadedImage は、「もっと見る」の続きがあるとき、読み込み済みの最後の画像で
// 次へスワイプすると続きを読んでからめくり（go → loadMoreThenGo）、ドラッグ中もゴムのような減衰を掛けないことを確かめる。
func TestLightboxSwipeLoadsMoreAtLastLoadedImage(t *testing.T) {
	data, err := FS.ReadFile("js/lightbox.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	section := func(start, end string) string {
		t.Helper()
		i := strings.Index(script, start)
		if i < 0 {
			t.Fatalf("lightbox.js lacks %q", start)
		}
		rest := script[i:]
		j := strings.Index(rest, end)
		if j < 0 {
			t.Fatalf("lightbox.js lacks %q after %q", end, start)
		}
		return rest[:j]
	}

	move := section(`dialog.addEventListener("pointermove"`, `function finishPointer(`)
	if !strings.Contains(move, `var atEdge = (dx > 0 && index === 0) || (dx < 0 && index === items.length - 1 && !canLoadMore());`) {
		t.Errorf("pointermove damps the swipe on the last loaded image even when more photos can be loaded:\n%s", move)
	}

	swipe := section(`if (g.type === "swipe") {`, `} else if (g.type === "dismiss")`)
	if !strings.Contains(swipe, `target >= 0 && (target < items.length || (dir > 0 && canLoadMore()))`) {
		t.Errorf("finishPointer does not turn the page past the last loaded image when more photos can be loaded:\n%s", swipe)
	}
	if !strings.Contains(swipe, `go(dir, dx)`) {
		t.Errorf("finishPointer swipe no longer calls go(dir, dx):\n%s", swipe)
	}
	// go が最後の画像の先へ進むときに続きを読む（loadMoreThenGo）ことも合わせて確かめる。
	goFn := section(`function go(dir, fromX) {`, `index = next;`)
	if !strings.Contains(goFn, `next >= items.length && canLoadMore()`) || !strings.Contains(goFn, `loadMoreThenGo(dir, fromX)`) {
		t.Errorf("go does not load more photos before moving past the last loaded image:\n%s", goFn)
	}
}
