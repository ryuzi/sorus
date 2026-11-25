package static

import (
	"regexp"
	"testing"
	"testing/fstest"
)

var versionedPattern = regexp.MustCompile(`^/static/css/public\.css\?v=[0-9a-f]{16}$`)

func TestURLAddsContentHash(t *testing.T) {
	got := URL("css/public.css")
	if !versionedPattern.MatchString(got) {
		t.Fatalf("URL(css/public.css) = %q, want /static/css/public.css?v=<16 hex>", got)
	}
	if again := URL("css/public.css"); again != got {
		t.Errorf("URL is not stable: %q then %q", got, again)
	}
	if a, b := URL("css/public.css"), URL("css/admin.css"); a[len(a)-16:] == b[len(b)-16:] {
		t.Errorf("different files share a version: %q, %q", a, b)
	}
}

func TestURLUnknownFileHasNoQuery(t *testing.T) {
	for _, name := range []string{"css/no-such-file.css", "themes/mono.css", ""} {
		if got, want := URL(name), "/static/"+name; got != want {
			t.Errorf("URL(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestVersionChangesWithContent(t *testing.T) {
	before := hashFiles(fstest.MapFS{
		"themes/news/theme.css": {Data: []byte(":root { --accent: #b91c1c; }")},
		"js/public.js":          {Data: []byte("console.log(1)")},
	})
	after := hashFiles(fstest.MapFS{
		"themes/news/theme.css": {Data: []byte(":root { --accent: #1d4ed8; }")},
		"js/public.js":          {Data: []byte("console.log(1)")},
	})
	b, a := versionedURL(before, "themes/news/theme.css"), versionedURL(after, "themes/news/theme.css")
	if b == a {
		t.Fatalf("changing the CSS kept the URL %q", b)
	}
	if len(before["themes/news/theme.css"]) != 16 {
		t.Errorf("version %q is not 16 hex characters", before["themes/news/theme.css"])
	}
	if versionedURL(before, "js/public.js") != versionedURL(after, "js/public.js") {
		t.Error("an unchanged file got a new URL")
	}
}

// TestServiceWorkerHasNoUnversionedAssets は、sw.js が版の付かない /static/ の URL を事前キャッシュしないことを確かめる。
// 版の付かない URL を保存すると、ページが要求する ?v= 付きの URL と一致せず、古い内容だけが残る。
func TestServiceWorkerHasNoUnversionedAssets(t *testing.T) {
	data, err := FS.ReadFile("sw.js")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`['"]/static/[^'"]+['"]`).Find(data); m != nil {
		t.Errorf("sw.js hardcodes the asset URL %s", m)
	}
}
