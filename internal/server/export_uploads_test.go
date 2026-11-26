package server

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/publish"
)

// seedImage は画像 id の media の行と、保存先（UploadDir）の4つのサイズのファイルを用意する。ファイルの中身は
// content と各サイズの名前。タグ tags はギャラリーで選ぶときに使う。
func seedImage(t *testing.T, env *testEnv, id, content, tags string) {
	t.Helper()
	env.insertMedia(id, id+".png", id+"の説明", "", tags)
	for _, size := range []string{"original", "1200", "800", "400"} {
		writeTestFile(t, filepath.Join(env.uploadDir, id+"-"+size+".webp"), content+" "+size)
	}
}

// imageSizes は画像 id の、書き出しに入る予定のサイズのファイル名を返す。
func imageSizes(id string, sizes ...string) []string {
	var out []string
	for _, s := range sizes {
		out = append(out, id+"-"+s+".webp")
	}
	return out
}

// expectNoSecretImage は書き出し dir のどのファイルにも、公開していない画像の中身（"SECRET-IMAGE"）が無いことを確かめる。
func expectNoSecretImage(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "SECRET-IMAGE") {
			t.Errorf("%s contains a non-public image: %q", p, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// imageWarnings は書き出しの注意のうち、画像（uploads）についてのものを返す。
func imageWarnings(rep *ExportReport) []string {
	var out []string
	for _, w := range rep.Warnings {
		if strings.Contains(w, "画像") || strings.Contains(w, "uploads") {
			out = append(out, w)
		}
	}
	return out
}

// 静的な書き出しに入れるアップロード画像は、書き出した公開ページ（記事・固定ページの本文とアイキャッチと生成サイズ、
// トップのギャラリー、本文のタグのギャラリー、Favicon）が実際に使うものだけ。下書き・校閲中・ゴミ箱・予約中の記事だけが
// 使う画像、どこにも使われない画像、ほかのサイトの同じ名前の URL は書き出さない。元のファイルは保存先に残る。
func TestServerExport_OnlyPublicImages(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	setClock(t, now)
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL, SiteName: "画像の書き出し"}, nil)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	seedImage(t, env, "hero", "PUBLIC hero", "")
	seedImage(t, env, "body", "PUBLIC body", "")
	seedImage(t, env, "abs", "PUBLIC absolute", "")
	seedImage(t, env, "pagehero", "PUBLIC page hero", "")
	seedImage(t, env, "top", "PUBLIC top gallery", "トップ")
	seedImage(t, env, "inline", "PUBLIC body gallery", "記事の写真")
	seedImage(t, env, "draft", "SECRET-IMAGE draft", "")
	seedImage(t, env, "review", "SECRET-IMAGE review", "")
	seedImage(t, env, "trash", "SECRET-IMAGE trash", "")
	seedImage(t, env, "sched", "SECRET-IMAGE scheduled", "")
	seedImage(t, env, "unused", "SECRET-IMAGE unused", "")
	seedImage(t, env, "extern", "SECRET-IMAGE same name as another site's image", "")
	writeTestFile(t, filepath.Join(env.uploadDir, "my photo.png"), "PUBLIC encoded name")
	writeTestFile(t, filepath.Join(env.uploadDir, "legacy", "old.png"), "PUBLIC nested legacy")
	writeTestFile(t, filepath.Join(env.uploadDir, "legacy", "draft.png"), "SECRET-IMAGE nested draft")
	writeTestFile(t, filepath.Join(env.uploadDir, "favicon-0123456789abcdef.png"), "PUBLIC favicon")
	writeTestFile(t, filepath.Join(env.uploadDir, "favicon-fedcba9876543210.png"), "SECRET-IMAGE old favicon")
	writeTestFile(t, filepath.Join(env.uploadDir, ".upload-tmp"), "SECRET-IMAGE unfinished upload")
	env.setCardImageSetting("favicon_url", "/uploads/favicon-0123456789abcdef.png")
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontSections: []FrontSection{{Type: FrontSectionGallery, Tag: "トップ", Title: "写真", Limit: 4}}}); err != nil {
		t.Fatal(err)
	}

	seedArticle(t, env.db, article.Article{Title: "公開記事", Slug: "public-post", Status: article.StatusPublished, PublishedAt: &past,
		FeaturedImage: "/uploads/hero-1200.webp",
		ContentMarkdown: "本文の画像 ![本文](/uploads/body-800.webp)\n\n" +
			"![名前に空白](/uploads/my%20photo.png) ![古い場所](/uploads/legacy/old.png?v=2)\n\n" +
			"![ほかのサイト](https://other.example/uploads/extern-1200.webp)\n\n" +
			":::gallery tag=\"記事の写真\"\n:::\n"})
	seedPage(t, env.db, article.Article{Title: "公開ページ", Slug: "public-page", Status: article.StatusPublished, PublishedAt: &past,
		FeaturedImage: "/uploads/pagehero-800.webp", ContentMarkdown: "![絶対 URL](" + testBaseURL + "/uploads/abs-1200.webp)"})
	hidden := func(title, slug, img string, st article.Status, at *time.Time) string {
		return seedArticle(t, env.db, article.Article{Title: title, Slug: slug, Status: st, PublishedAt: at,
			FeaturedImage: "/uploads/" + img + "-1200.webp", ContentMarkdown: "![非公開](/uploads/" + img + "-800.webp) ![古い](/uploads/legacy/draft.png)"})
	}
	hidden("下書き", "draft-post", "draft", article.StatusDraft, nil)
	hidden("校閲中", "review-post", "review", article.StatusReview, nil)
	trashed := hidden("ゴミ箱", "trashed-post", "trash", article.StatusPublished, &past)
	if _, err := env.db.Exec(`UPDATE articles SET status = ? WHERE id = ?`, article.StatusDeleted, trashed); err != nil {
		t.Fatal(err)
	}
	hidden("予約", "scheduled-post", "sched", article.StatusPublished, &future)

	want := slices.Concat(
		imageSizes("hero", "1200", "800", "400"), // アイキャッチは生成サイズを srcset に並べる。og:image は 1200
		imageSizes("body", "800"),
		imageSizes("abs", "1200"),
		imageSizes("pagehero", "800"),       // 本文に画像があるページはアイキャッチを本文の上に出さず（ShowHero）、og:image だけが使う
		imageSizes("top", "800", "1200"),    // トップのフォトギャラリーは一覧に -800、拡大（data-full）に -1200 を使う
		imageSizes("inline", "800", "1200"), // 本文の :::gallery tag も同じく一覧に -800、拡大（data-full）に -1200 を使う
		[]string{"favicon-0123456789abcdef.png", "legacy/old.png", "my photo.png"},
	)
	slices.Sort(want)

	for _, tc := range []struct {
		name   string
		export func(t *testing.T) (string, *ExportReport)
	}{
		{"Export", func(t *testing.T) (string, *ExportReport) {
			out := filepath.Join(t.TempDir(), "dist")
			rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: out})
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			return out, rep
		}},
		{"ExportToTemp", func(t *testing.T) (string, *ExportReport) {
			dir, rep, err := env.server.ExportToTemp(context.Background(), ExportOptions{})
			if err != nil {
				t.Fatalf("ExportToTemp: %v", err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			return dir, rep
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, rep := tc.export(t)
			if got := filesIn(t, filepath.Join(dir, "uploads")); !slices.Equal(got, want) {
				t.Errorf("exported uploads = %v\nwant %v", got, want)
			}
			expectNoSecretImage(t, dir)
			if data, err := os.ReadFile(filepath.Join(dir, "uploads", "hero-1200.webp")); err != nil || string(data) != "PUBLIC hero 1200" {
				t.Errorf("uploads/hero-1200.webp = %q, %v", data, err)
			}
			if w := imageWarnings(rep); len(w) != 0 {
				t.Errorf("image warnings = %q, want none", w)
			}
		})
	}
	// 元のファイルは保存先に残る。
	for _, name := range []string{"draft-original.webp", "unused-400.webp", "favicon-fedcba9876543210.png", "legacy/draft.png"} {
		if _, err := os.Stat(filepath.Join(env.uploadDir, filepath.FromSlash(name))); err != nil {
			t.Errorf("source %s: %v", name, err)
		}
	}
}

// 書き出したファイルの中のアップロード画像の参照の見つけ方。属性・srcset・Markdown・JSON（\/ のエスケープ）・XML の
// 文字参照の中の参照、自分のサイトの絶対 URL（http・https・// の形）を拾い、ほかのサイトの URL・ほかのパスの一部
// （/static/uploads/ など）・保存先の外や隠しファイルを指す名前は拾わない。
func TestUploadRefsScan(t *testing.T) {
	refs := newUploadRefs(testBaseURL)
	refs.scan([]byte(`<img src="/uploads/a-800.webp" srcset="/uploads/a-400.webp 400w, /uploads/a-800.webp 800w,/uploads/a-1200.webp 1200w">` +
		`<meta property="og:image" content="` + testBaseURL + `/uploads/og-1200.webp">` +
		`<script type="application/ld+json">{"image":["https:\/\/media.example.ac.jp\/uploads\/ld-1200.webp"]}</script>` +
		`<description>&lt;img src=&#34;/uploads/feed.webp&#34;&gt;</description>` +
		`![alt](/uploads/md.webp "title") (/uploads/paren.webp) url('/uploads/css.webp')` +
		`<a href="http://MEDIA.example.ac.jp/uploads/http.webp">x</a> <img src="//media.example.ac.jp/uploads/proto.webp">` +
		`<img src="` + testBaseURL + `//uploads/slash.webp"> <img src="/uploads/q.webp?v=1#x"> <img src="/uploads/sub/dir/n.png"> <img src="/uploads/%E5%86%99%E7%9C%9F.png">` +
		`画像は/uploads/text.webp を使う。` +
		// 拾わないもの
		`<img src="https://other.example/uploads/other.webp"> <img src="https://media.example.ac.jp.evil.example/uploads/evil.webp">` +
		`<img src="/static/uploads/static.webp"> <img src="/admin/remote/uploads/remote.webp"> <img src="https://media.example.ac.jpx/uploads/x.webp">` +
		`<img src="/uploads/../secret.db"> <img src="/uploads/a/../../secret.db"> <img src="/uploads/.hidden"> <img src="/uploads/sub/.tmp/x.webp">` +
		`<img src="/uploads/%2e%2e/secret.db"> <img src="/uploads/a%2Fb.webp"> <img src="/uploads/"> <img src="/uploads/%zz.webp">`))
	got := slices.Sorted(func(yield func(string) bool) {
		for name := range refs.names {
			if !yield(name) {
				return
			}
		}
	})
	want := []string{"a-1200.webp", "a-400.webp", "a-800.webp", "css.webp", "feed.webp", "http.webp", "ld-1200.webp", "md.webp",
		"og-1200.webp", "paren.webp", "proto.webp", "q.webp", "slash.webp", "sub/dir/n.png", "text.webp", "写真.png"}
	if !slices.Equal(got, want) {
		t.Errorf("refs = %q\nwant %q", got, want)
	}
}

// 公開ページが使う画像のうち、sorus が管理する画像（メディアの行・Favicon の設定）のファイルが無いもの、保存先で
// 通常のファイルでないもの（シンボリックリンク・フォルダ）は書き出さず、書き出しは止めずに注意に出す。sorus が管理して
// いない名前で、ファイルも無いもの（本文の説明の中の例など）は注意に出さない。
func TestServerExport_MissingOrInvalidPublicImagesWarn(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, nil)
	seedImage(t, env, "gone", "PUBLIC gone", "")
	os.Remove(filepath.Join(env.uploadDir, "gone-1200.webp"))
	seedImage(t, env, "draftonly", "SECRET-IMAGE draft only", "")
	if err := os.Symlink(filepath.Join(env.uploadDir, "draftonly-original.webp"), filepath.Join(env.uploadDir, "link.webp")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(env.uploadDir, "folder.webp"), 0o755); err != nil {
		t.Fatal(err)
	}
	env.setCardImageSetting("favicon_url", "/uploads/favicon-0000000000000000.png")
	seedArticle(t, env.db, article.Article{Title: "公開記事", Slug: "public-post", Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		ContentMarkdown: "![無い](/uploads/gone-1200.webp) ![リンク](/uploads/link.webp) ![フォルダ](/uploads/folder.webp)\n\n" +
			"`/uploads/photo1.webp` のように書きます。"})

	out := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: out})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := filesIn(t, filepath.Join(out, "uploads")); len(got) != 0 {
		t.Errorf("exported uploads = %v, want none", got)
	}
	expectNoSecretImage(t, out)
	// 名前は参照の順（Favicon・folder.webp・gone-1200.webp・link.webp）の先頭3件と「など」。メディアの画像は元のファイル名で示す。
	w := strings.Join(imageWarnings(rep), "\n")
	for _, want := range []string{"4 件", "「Favicon」「folder.webp」「gone.png」 など"} {
		if !strings.Contains(w, want) {
			t.Errorf("image warnings = %q, want %q", w, want)
		}
	}
	if strings.Contains(w, "photo1") {
		t.Errorf("image warnings = %q, want no warning about an unmanaged name without a file", w)
	}
}

// 保存先の画像を調べてから開くまでに、ほかの画像（下書きの記事だけが使う画像）へのシンボリックリンクに差し替えられて
// も、その中身を書き出さず、注意に出す。
func TestServerExport_ImageSourceSwappedToSymlink(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, nil)
	seedImage(t, env, "pub", "PUBLIC image", "")
	seedImage(t, env, "secret", "SECRET-IMAGE draft", "")
	seedArticle(t, env.db, article.Article{Title: "公開記事", Slug: "public-post", Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		ContentMarkdown: "![公開](/uploads/pub-800.webp)"})
	target := filepath.Join(env.uploadDir, "pub-800.webp")
	orig := beforeUploadOpen
	t.Cleanup(func() { beforeUploadOpen = orig })
	beforeUploadOpen = func(name string) {
		if name != "pub-800.webp" {
			return
		}
		os.Remove(target)
		if err := os.Symlink("secret-800.webp", target); err != nil {
			t.Errorf("symlink: %v", err)
		}
	}
	out := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: out})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := filesIn(t, filepath.Join(out, "uploads")); len(got) != 0 {
		t.Errorf("exported uploads = %v, want none", got)
	}
	expectNoSecretImage(t, out)
	if w := strings.Join(imageWarnings(rep), "\n"); !strings.Contains(w, "1 件") {
		t.Errorf("image warnings = %q, want one about the swapped image", w)
	}
}

// 前回の書き出し先にもう一度（Clean でなく）書き出すとき、uploads/ に前からあるファイル（前回の書き出しで出した、今は
// 公開ページが使わない画像かもしれない）は消さずに残し、残っていることを注意に出す。Clean の書き出しなら残らない。
func TestServerExport_ReportsLeftoverImages(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, nil)
	seedImage(t, env, "pub", "PUBLIC image", "")
	seedArticle(t, env.db, article.Article{Title: "公開記事", Slug: "public-post", Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		ContentMarkdown: "![公開](/uploads/pub-800.webp)"})
	out := filepath.Join(t.TempDir(), "dist")
	writeTestFile(t, filepath.Join(out, "uploads", "draft-1200.webp"), "previously exported image")
	writeTestFile(t, filepath.Join(out, "uploads", "old", "note.txt"), "利用者のメモ")
	writeTestFile(t, filepath.Join(out, "uploads", "pub-800.webp"), "stale content")

	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: out})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := filesIn(t, filepath.Join(out, "uploads")); !slices.Equal(got, []string{"draft-1200.webp", "old/note.txt", "pub-800.webp"}) {
		t.Errorf("uploads after export = %v, want the leftovers kept", got)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "uploads", "pub-800.webp")); string(data) != "PUBLIC image 800" {
		t.Errorf("public image = %q, want the current file", data)
	}
	leftover := func(r *ExportReport) string {
		for _, w := range r.Warnings {
			if strings.Contains(w, "--clean") && strings.Contains(w, filepath.Join(out, "uploads")) {
				return w
			}
		}
		return ""
	}
	if w := leftover(rep); !strings.Contains(w, "2 件") {
		t.Errorf("Warnings = %q, want one about the 2 leftover files and --clean", rep.Warnings)
	}

	rep, err = env.server.Export(context.Background(), ExportOptions{OutDir: out, Clean: true})
	if err != nil {
		t.Fatalf("Export(Clean): %v", err)
	}
	if got := filesIn(t, filepath.Join(out, "uploads")); !slices.Equal(got, []string{"pub-800.webp"}) {
		t.Errorf("uploads after the clean export = %v", got)
	}
	if w := leftover(rep); w != "" {
		t.Errorf("clean export warned about leftovers: %q", w)
	}
}

// 公開ページが使う画像を書き出せなかったとき（書き出し先の uploads がファイル・シンボリックリンクなど）は、書き出しを
// 失敗にし、リンク先（書き出し先の外）を書き換えない。
func TestServerExport_ImageCopyErrorFailsExport(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, nil)
	seedImage(t, env, "pub", "PUBLIC image", "")
	seedArticle(t, env.db, article.Article{Title: "公開記事", Slug: "public-post", Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		ContentMarkdown: "![公開](/uploads/pub-800.webp) ![古い場所](/uploads/legacy/old.png)"})
	writeTestFile(t, filepath.Join(env.uploadDir, "legacy", "old.png"), "PUBLIC nested legacy")

	out := filepath.Join(t.TempDir(), "dist")
	writeTestFile(t, filepath.Join(out, "uploads"), "not a folder")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err == nil || !strings.Contains(err.Error(), "upload") {
		t.Errorf("Export error = %v, want an upload copy error", err)
	}

	outside := t.TempDir()
	for _, link := range []string{"uploads", "uploads/legacy"} {
		out := filepath.Join(t.TempDir(), "dist")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(out, link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(out, filepath.FromSlash(link))); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: out}); err == nil {
			t.Errorf("Export with %s as a symlink succeeded, want an error", link)
		}
		if got := filesIn(t, outside); len(got) != 0 {
			t.Errorf("files written outside the output dir through %s: %v", link, got)
		}
	}
}

// 「サイトに反映」（フォルダへの書き出し）でも、送る画像は公開ページが使うものだけ。前回の反映で送った画像（以前の版の
// sorus が保存先の画像をすべて送ったものを含む）は、公開ページが使わなくなった次の反映で公開先から消え、公開先に
// もともとある利用者のファイル（前回の一覧に無いもの）は消さない。
func TestPublishFolderReflectSendsOnlyPublicImages(t *testing.T) {
	env := newTestEnv(t)
	seedImage(t, env, "pub", "PUBLIC image", "")
	seedImage(t, env, "draft", "SECRET-IMAGE draft", "")
	pub := seedArticle(t, env.db, article.Article{Title: "画像つきの記事", Slug: "with-image", Status: article.StatusPublished, PublishedAt: ago(time.Hour),
		ContentMarkdown: "![公開](/uploads/pub-800.webp)"})
	seedArticle(t, env.db, article.Article{Title: "下書き", Slug: "draft-post", Status: article.StatusDraft, ContentMarkdown: "![下書き](/uploads/draft-800.webp)"})
	out := filepath.Join(t.TempDir(), "site")
	writeTestFile(t, filepath.Join(out, "uploads", "note.txt"), "利用者のメモ")
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, out)

	// 以前の版の sorus が保存先の画像をすべて送った反映の跡（公開先のファイルと、前回の一覧）。
	writeTestFile(t, filepath.Join(out, "uploads", "draft-800.webp"), "SECRET-IMAGE draft 800")
	writeTestFile(t, filepath.Join(out, "uploads", "draft-original.webp"), "SECRET-IMAGE draft original")
	prev := publish.Manifest{
		"uploads/draft-800.webp":      {SHA256: "old", Size: 1},
		"uploads/draft-original.webp": {SHA256: "old", Size: 1},
	}
	if err := env.server.savePublishManifest(env.server.loadPublishConfig().destinationID(), prev); err != nil {
		t.Fatal(err)
	}

	first := c.reflectAndWait()
	if !first.OK {
		t.Fatalf("first reflect = %+v", first)
	}
	if got := filesIn(t, filepath.Join(out, "uploads")); !slices.Equal(got, []string{"note.txt", "pub-800.webp"}) {
		t.Errorf("uploads after the first reflect = %v, want the public image and the user's file", got)
	}
	expectNoSecretImage(t, out)

	unpublish(t, env, pub)
	second := c.reflectAndWait()
	if !second.OK {
		t.Fatalf("second reflect = %+v", second)
	}
	if got := filesIn(t, filepath.Join(out, "uploads")); !slices.Equal(got, []string{"note.txt"}) {
		t.Errorf("uploads after unpublishing = %v, want only the user's file", got)
	}
	// 元のファイルは保存先に残る。
	if got := filesIn(t, env.uploadDir); len(got) != 8 {
		t.Errorf("source uploads = %v, want all 8 files kept", got)
	}
}

// 初めて反映する公開先（前回の一覧が無い）にもともとあるファイルは消さず、残したことと、以前に送った古い画像などが
// あれば公開先で消すことを知らせる。
func TestPublishFolderFirstReflectExplainsKeptFiles(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post")
	out := filepath.Join(t.TempDir(), "site")
	writeTestFile(t, filepath.Join(out, "uploads", "old-1200.webp"), "以前に別の方法で送った画像")
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, out)

	first := c.reflectAndWait()
	if !first.OK {
		t.Fatalf("first reflect = %+v", first)
	}
	if got := filesIn(t, filepath.Join(out, "uploads")); !slices.Equal(got, []string{"old-1200.webp"}) {
		t.Errorf("uploads after the first reflect = %v, want the existing file kept", got)
	}
	details := strings.Join(first.Details, "\n")
	if !strings.Contains(details, "もともとあった 1 件") || !strings.Contains(details, "公開先で削除") {
		t.Errorf("details = %q, want a note about the kept files and how to remove old ones", details)
	}
}
