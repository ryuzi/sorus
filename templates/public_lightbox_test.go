package templates

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestPublicLightboxMarkup は、公開レイアウトの <dialog id="cms-lightbox"> が lightbox.js の使う部品
// （左右ボタン・読み上げ付きカウンター・閉じるボタン・画像の土台）を備えていることを確かめる（photo-gallery-ux.md §2-2）。
func TestPublicLightboxMarkup(t *testing.T) {
	data, err := FS.ReadFile("public/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	layout := string(data)
	start := strings.Index(layout, `<dialog id="cms-lightbox"`)
	end := strings.Index(layout, `</dialog>`)
	if start < 0 || end < start {
		t.Fatal(`layout.html has no <dialog id="cms-lightbox">...</dialog>`)
	}
	dialog := layout[start:end]
	for _, want := range []string{
		`aria-modal="true"`,
		`role="dialog"`,
		`class="cms-lightbox-counter" aria-live="polite"`,
		`class="cms-lightbox-close"`,
		`class="cms-lightbox-nav cms-lightbox-prev"`,
		`class="cms-lightbox-nav cms-lightbox-next"`,
		`class="cms-lightbox-stage"`,
		`class="cms-lightbox-image"`,
		`class="cms-lightbox-caption"`,
		`data-trigger-label=`,
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("lightbox dialog is missing %s", want)
		}
	}
	// 左右ボタンにも、日英それぞれの読み上げ名を付ける。
	for _, want := range []string{"Previous image", "前の画像", "Next image", "次の画像"} {
		if !strings.Contains(dialog, want) {
			t.Errorf("lightbox dialog is missing the label %q", want)
		}
	}
}

// TestLightboxScriptMatchesMarkup は、lightbox.js が探す .cms-lightbox-* の部品がすべてレイアウトにあり、
// public.css にも見た目が定義されていることを確かめる。部品の名前を片方だけ変えると、拡大表示が黙って動かなくなる。
func TestLightboxScriptMatchesMarkup(t *testing.T) {
	script, err := os.ReadFile("../static/js/lightbox.js")
	if err != nil {
		t.Fatal(err)
	}
	css, err := os.ReadFile("../static/css/public.css")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := FS.ReadFile("public/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	parts := regexp.MustCompile(`querySelector\("\.(cms-lightbox-[a-z-]+)"\)`).FindAllStringSubmatch(string(script), -1)
	if len(parts) < 7 {
		t.Fatalf("lightbox.js looks up only %d parts, want the image, stage, caption, counter, close, prev and next", len(parts))
	}
	for _, m := range parts {
		class := m[1]
		if !regexp.MustCompile(`class="[^"]*\b` + regexp.QuoteMeta(class) + `\b`).Match(layout) {
			t.Errorf("lightbox.js looks up .%s, but layout.html has no such element", class)
		}
		if !strings.Contains(string(css), "."+class) {
			t.Errorf("public.css has no style for .%s", class)
		}
	}
	// lightbox.js が切り替える状態クラスにも、見た目がある。
	for _, state := range []string{"is-ui-hidden", "is-zoomed", "is-dragging", "is-animating", "cms-lightbox-trigger"} {
		if !strings.Contains(string(css), "."+state) {
			t.Errorf("public.css has no style for the state .%s used by lightbox.js", state)
		}
	}
}
