package server

import (
	"context"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
)

// seedAttachmentRow は記事 articleID の添付資料の行を直接入れる。content が nil でなければ、保存先（AttachmentDir）の
// file にその中身のファイルを置く。
func seedAttachmentRow(t *testing.T, env *testEnv, articleID, id, file string, content []byte) {
	t.Helper()
	seedAttachmentRowMIME(t, env, articleID, id, file, "application/pdf", content)
}

// seedAttachmentRowMIME は seedAttachmentRow と同じく資料を入れる。形式（mime_type）を mime にする。
func seedAttachmentRowMIME(t *testing.T, env *testEnv, articleID, id, file, mime string, content []byte) {
	t.Helper()
	if _, err := env.db.Exec(`INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, articleID, id+path.Ext(file), file, len(content), mime, studentA.ID); err != nil {
		t.Fatalf("insert attachment %s: %v", id, err)
	}
	if content != nil {
		writeTestFile(t, filepath.Join(env.attachmentDir, file), string(content))
	}
}

// seedAttachmentRowWithoutArticle は記事が無い添付資料の行（外部キーの検査が無かった頃の DB を想定）を入れ、
// 保存先にファイルを置く。
func seedAttachmentRowWithoutArticle(t *testing.T, env *testEnv, id, file string, content []byte) {
	t.Helper()
	ctx := context.Background()
	conn, err := env.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, "no-such-article", id+".pdf", file, len(content), "application/pdf", studentA.ID)
	if _, ferr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); ferr != nil {
		t.Fatal(ferr)
	}
	if err != nil {
		t.Fatalf("insert attachment %s: %v", id, err)
	}
	writeTestFile(t, filepath.Join(env.attachmentDir, file), string(content))
}

// expectExportedAttachments は、書き出し dir の attachments/ にあるファイルがちょうど want（名前→中身）で、
// dir のどのファイルにも非公開の資料の中身（"SECRET"）が入っていないことを確かめる。
func expectExportedAttachments(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := filesIn(t, filepath.Join(dir, "attachments"))
	var names []string
	for name := range want {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(got, names) {
		t.Errorf("exported attachments = %v, want %v", got, names)
	}
	for name, content := range want {
		data, err := os.ReadFile(filepath.Join(dir, "attachments", name))
		if err != nil || string(data) != content {
			t.Errorf("attachments/%s = %q, %v; want %q", name, data, err, content)
		}
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "SECRET") {
			t.Errorf("%s contains the content of a non-public file: %q", p, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 静的な書き出し（Export・ExportToTemp）に入れる添付資料は、公開サーバーがダウンロードさせるもの
// （handleAttachmentDownload と同じ条件・同じ時刻）だけ。下書き・校閲中・ゴミ箱・予約中の記事の資料、記事が無い資料、
// どの記事の資料でもないファイルは書き出さない。
func TestServerExport_OnlyPublicArticleAttachments(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	setClock(t, now)
	env := newTestEnv(t)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	post := func(title, slug string, st article.Status, at *time.Time) string {
		return seedArticle(t, env.db, article.Article{Title: title, Slug: slug, ContentMarkdown: title + "の本文", Status: st, PublishedAt: at})
	}
	pubPost := post("公開記事", "public-post", article.StatusPublished, &past)
	pubPage := seedPage(t, env.db, article.Article{Title: "公開ページ", Slug: "public-page", ContentMarkdown: "ページの本文",
		Status: article.StatusPublished, PublishedAt: &past})
	draft := post("下書き", "draft-post", article.StatusDraft, nil)
	review := post("校閲中", "review-post", article.StatusReview, nil)
	trashed := post("ゴミ箱", "trashed-post", article.StatusPublished, &past)
	if _, err := env.db.Exec(`UPDATE articles SET status = ? WHERE id = ?`, article.StatusDeleted, trashed); err != nil {
		t.Fatal(err)
	}
	scheduled := post("予約", "scheduled-post", article.StatusPublished, &future)

	files := map[string]string{} // 資料の ID → 保存ファイル名
	add := func(articleID, id, content string) {
		files[id] = id + ".pdf"
		seedAttachmentRow(t, env, articleID, id, id+".pdf", []byte(content))
	}
	add(pubPost, "att-public-post", "PUBLIC post attachment")
	add(pubPage, "att-public-page", "PUBLIC page attachment")
	add(draft, "att-draft", "SECRET draft attachment")
	add(review, "att-review", "SECRET review attachment")
	add(trashed, "att-trashed", "SECRET trashed attachment")
	add(scheduled, "att-scheduled", "SECRET scheduled attachment")
	files["att-no-article"] = "att-no-article.pdf"
	seedAttachmentRowWithoutArticle(t, env, "att-no-article", "att-no-article.pdf", []byte("SECRET attachment of a missing article"))
	writeTestFile(t, filepath.Join(env.attachmentDir, "orphan.pdf"), "SECRET orphan file")
	writeTestFile(t, filepath.Join(env.attachmentDir, "nested", "orphan.pdf"), "SECRET nested orphan file")
	writeTestFile(t, filepath.Join(env.attachmentDir, ".upload-123"), "SECRET unfinished upload")

	want := map[string]string{"att-public-post.pdf": "PUBLIC post attachment", "att-public-page.pdf": "PUBLIC page attachment"}
	outDir := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	expectExportedAttachments(t, outDir, want)
	tmp, tmpRep, err := env.server.ExportToTemp(context.Background(), ExportOptions{})
	if err != nil {
		t.Fatalf("ExportToTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	expectExportedAttachments(t, tmp, want)

	// 書き出したファイルの数（FilesCount はページ以外のファイル）は、書き出し先にあるファイルの数と合う。
	for dir, r := range map[string]*ExportReport{outDir: rep, tmp: tmpRep} {
		if n := len(filesIn(t, dir)); n != r.FilesCount+r.PagesCount {
			t.Errorf("%s: %d files, report says %d files + %d pages", dir, n, r.FilesCount, r.PagesCount)
		}
		for _, w := range r.Warnings {
			if strings.Contains(w, "添付資料") {
				t.Errorf("%s: unexpected attachment warning %q", dir, w)
			}
		}
	}
	// 公開記事の資料へのリンクは、書き出したファイルを指す。
	page, err := os.ReadFile(filepath.Join(outDir, "articles", "public-post", "index.html"))
	if err != nil || !strings.Contains(string(page), AttachmentURLPrefix+"att-public-post.pdf") {
		t.Errorf("article page does not link the exported attachment: %v", err)
	}

	// 書き出したかどうかは、公開サーバーがダウンロードさせるかどうかと同じ。
	reader := env.newClient()
	for id, file := range files {
		served := reader.get(AttachmentURLPrefix+id+"/download").StatusCode == http.StatusOK
		_, statErr := os.Stat(filepath.Join(outDir, "attachments", file))
		if served != (statErr == nil) {
			t.Errorf("%s: served by the public server = %v, exported = %v", id, served, statErr == nil)
		}
	}
}

// 保存ファイル名が保存先の外を指す資料（DB の改ざんを想定）、通常のファイルでないもの（シンボリックリンク・フォルダ）、
// ファイルが無いものは書き出さず、書き出しは止めずに件数を注意に出す。書き出し先の外には何も書かない。
func TestServerExport_AttachmentPathSafety(t *testing.T) {
	env := newTestEnv(t)
	id := publishedArticle(t, env, "改ざんされた記事", "tampered")
	secret := filepath.Join(filepath.Dir(env.attachmentDir), "secret.txt")
	writeTestFile(t, secret, "SECRET outside the attachment dir")
	writeTestFile(t, filepath.Join(env.attachmentDir, "nested", "inner.pdf"), "SECRET nested file")
	seedAttachmentRow(t, env, id, "att-ok", "att-ok.pdf", []byte("PUBLIC ok"))
	skipped := 0
	for _, p := range []string{"../secret.txt", "..", ".", "", secret, `..\secret.txt`, "sub/../../secret.txt", "nested/inner.pdf", "missing.pdf"} {
		seedAttachmentRow(t, env, id, fmt.Sprintf("att-bad-%d", skipped), p, nil)
		skipped++
	}
	if err := os.Mkdir(filepath.Join(env.attachmentDir, "dir.pdf"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedAttachmentRow(t, env, id, "att-dir", "dir.pdf", nil)
	skipped++
	if err := os.Symlink(secret, filepath.Join(env.attachmentDir, "link.pdf")); err == nil {
		seedAttachmentRow(t, env, id, "att-link", "link.pdf", nil)
		skipped++
	} else {
		t.Logf("symlink not available: %v", err)
	}

	parent := t.TempDir()
	outDir := filepath.Join(parent, "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	expectExportedAttachments(t, outDir, map[string]string{"att-ok.pdf": "PUBLIC ok"})
	if got := dirNames(t, parent); !slices.Equal(got, []string{"dist"}) {
		t.Errorf("export wrote next to the output dir: %v", got)
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "SECRET outside the attachment dir" {
		t.Errorf("secret file changed: %q, %v", data, err)
	}
	warned := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "添付資料") && strings.Contains(w, fmt.Sprintf("%d 件", skipped)) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("Warnings = %q, want a warning about %d attachments that were not exported", rep.Warnings, skipped)
	}
}

// 前回の書き出し先にもう一度（Clean でなく）書き出すとき、attachments/ に前からあるファイル（その後に非公開にした
// 記事の資料かもしれない）は消さずに残し、残っていることを注意に出す。Clean の書き出しなら残らない。
func TestServerExport_ReportsLeftoverAttachments(t *testing.T) {
	env := newTestEnv(t)
	id := publishedArticle(t, env, "公開記事", "public-post")
	seedAttachmentRow(t, env, id, "att-public", "att-public.pdf", []byte("PUBLIC current"))
	outDir := filepath.Join(t.TempDir(), "dist")
	writeTestFile(t, filepath.Join(outDir, "attachments", "att-old.pdf"), "previously exported attachment")
	writeTestFile(t, filepath.Join(outDir, "attachments", "note.txt"), "利用者のメモ")
	writeTestFile(t, filepath.Join(outDir, "attachments", "att-public.pdf"), "stale content")

	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := filesIn(t, filepath.Join(outDir, "attachments")); !slices.Equal(got, []string{"att-old.pdf", "att-public.pdf", "note.txt"}) {
		t.Errorf("attachments after export = %v, want the leftovers kept", got)
	}
	if data, _ := os.ReadFile(filepath.Join(outDir, "attachments", "att-public.pdf")); string(data) != "PUBLIC current" {
		t.Errorf("public attachment = %q, want the current file", data)
	}
	leftoverWarning := func(r *ExportReport) string {
		for _, w := range r.Warnings {
			if strings.Contains(w, "--clean") {
				return w
			}
		}
		return ""
	}
	if w := leftoverWarning(rep); !strings.Contains(w, "2 件") || !strings.Contains(w, filepath.Join(outDir, "attachments")) {
		t.Errorf("Warnings = %q, want one about the 2 leftover files and --clean", rep.Warnings)
	}

	rep, err = env.server.Export(context.Background(), ExportOptions{OutDir: outDir, Clean: true})
	if err != nil {
		t.Fatalf("Export(Clean): %v", err)
	}
	expectExportedAttachments(t, outDir, map[string]string{"att-public.pdf": "PUBLIC current"})
	if w := leftoverWarning(rep); w != "" {
		t.Errorf("clean export warned about leftovers: %q", w)
	}
}

// 公開記事の資料を書き出せなかったとき（書き出し先の attachments がファイルなど）は、書き出しを失敗にする。
func TestServerExport_AttachmentCopyErrorFailsExport(t *testing.T) {
	env := newTestEnv(t)
	id := publishedArticle(t, env, "公開記事", "public-post")
	seedAttachmentRow(t, env, id, "att-public", "att-public.pdf", []byte("PUBLIC current"))
	outDir := filepath.Join(t.TempDir(), "dist")
	writeTestFile(t, filepath.Join(outDir, "attachments"), "not a folder")
	if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err == nil || !strings.Contains(err.Error(), "attachment") {
		t.Errorf("Export error = %v, want an attachment copy error", err)
	}
}

// 「サイトに反映」（フォルダへの書き出し）でも、送る添付資料は公開記事のものだけ。前回送った資料は記事を非公開にした
// 次の反映で消え、公開先にもともとある利用者のファイルは消さない。
func TestPublishFolderReflectSendsOnlyPublicAttachments(t *testing.T) {
	env := newTestEnv(t)
	pub := publishedArticle(t, env, "資料つきの記事", "with-attachment")
	draft := seedArticle(t, env.db, article.Article{Title: "下書き", Slug: "draft-post", ContentMarkdown: "下書き", Status: article.StatusDraft})
	seedAttachmentRow(t, env, pub, "att-public", "att-public.pdf", []byte("PUBLIC attachment"))
	seedAttachmentRow(t, env, draft, "att-draft", "att-draft.pdf", []byte("SECRET draft attachment"))
	writeTestFile(t, filepath.Join(env.attachmentDir, "orphan.pdf"), "SECRET orphan file")
	out := filepath.Join(t.TempDir(), "site")
	writeTestFile(t, filepath.Join(out, "attachments", "note.txt"), "利用者のメモ")
	c := env.loggedIn(admin)
	saveFolderDestination(t, c, out)

	first := c.reflectAndWait()
	if !first.OK {
		t.Fatalf("first reflect = %+v", first)
	}
	if got := filesIn(t, filepath.Join(out, "attachments")); !slices.Equal(got, []string{"att-public.pdf", "note.txt"}) {
		t.Errorf("attachments after the first reflect = %v", got)
	}

	unpublish(t, env, pub)
	second := c.reflectAndWait()
	if !second.OK {
		t.Fatalf("second reflect = %+v", second)
	}
	if got := filesIn(t, filepath.Join(out, "attachments")); !slices.Equal(got, []string{"note.txt"}) {
		t.Errorf("attachments after unpublishing = %v, want only the user's file", got)
	}
}

// staticAttachmentRefRe は書き出したファイルの中の、静的な書き出しの添付資料へのリンク（/attachments/<名前>.<拡張子>）。
var staticAttachmentRefRe = regexp.MustCompile(`/attachments/([A-Za-z0-9_-]+\.[a-z]+)`)

// 書き出したサイトの添付資料へのリンク（資料欄・本文の Markdown のリンク・JSON-LD・Google Scholar 向けメタタグ・
// llms-full.txt・RSS）は、ファイル名どおりに配信するだけの静的ホスティングで開ける（書き出したファイルを指し、拡張子で
// 形式が決まる）。同じ保存ファイルを指す資料は1つのファイルを共有し、公開していない資料・形式や名前が正しくない資料の
// リンクは書き換えない。
func TestServerExport_AttachmentLinksResolveOnStaticHost(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL, SiteName: "書き出しテスト"}, nil)
	draft := seedArticle(t, env.db, article.Article{Title: "下書き", Slug: "draft-post", ContentMarkdown: "下書き", Status: article.StatusDraft})
	body := "資料: [論文](/attachments/att-paper/download)、[絶対 URL](" + testBaseURL + "/attachments/att-paper/download)、" +
		"[共有](/attachments/att-shared-b/download)、[下書きの資料](/attachments/att-draft/download)、" +
		"[下書きと共有](/attachments/att-draft-shared/download)、[HTML](/attachments/att-html/download)"
	paper := seedArticle(t, env.db, article.Article{Title: "論文の紹介", Slug: "paper", ContentMarkdown: body,
		Status: article.StatusPublished, PublishedAt: ago(time.Hour), DOI: "10.1234/static.1"})
	seedAttachmentRow(t, env, paper, "att-paper", "att-paper.pdf", []byte("PUBLIC paper pdf"))
	seedAttachmentRowMIME(t, env, paper, "att-shared-a", "shared.csv", "text/csv", []byte("PUBLIC,shared\n"))
	seedAttachmentRowMIME(t, env, paper, "att-shared-b", "shared.csv", "text/csv", nil)
	seedAttachmentRow(t, env, draft, "att-draft", "att-draft.pdf", []byte("SECRET draft attachment"))
	seedAttachmentRow(t, env, draft, "att-draft-shared", "att-paper.pdf", nil)
	// 公開記事の資料でも、静的ホスティングでページとして開かれる拡張子・形式の食い違い・安全でない ID は書き出さない。
	seedAttachmentRowMIME(t, env, paper, "att-html", "att-html.html", "text/html", []byte("<p>PUBLIC html</p>"))
	seedAttachmentRowMIME(t, env, paper, "att-mismatch", "att-mismatch.pdf", "text/html", []byte("PUBLIC mismatch"))
	seedAttachmentRow(t, env, paper, "bad id", "badid.pdf", []byte("PUBLIC bad id"))

	outDir := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	want := map[string]string{"att-paper.pdf": "PUBLIC paper pdf", "shared.csv": "PUBLIC,shared\n"}
	expectExportedAttachments(t, outDir, want)
	if n := len(filesIn(t, outDir)); n != rep.FilesCount+rep.PagesCount {
		t.Errorf("%d files, report says %d files + %d pages", n, rep.FilesCount, rep.PagesCount)
	}
	// 拡張子で決まる形式（静的ホスティングが返す Content-Type）が資料の形式と同じ。
	for name, mimeType := range map[string]string{"att-paper.pdf": "application/pdf", "shared.csv": "text/csv"} {
		if got := mime.TypeByExtension(path.Ext(name)); !strings.HasPrefix(got, mimeType) {
			t.Errorf("%s is served as %q, want %s", name, got, mimeType)
		}
	}

	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	page := read("articles/paper/index.html")
	expectContains(t, page, `href="/attachments/att-paper.pdf"`, `href="`+testBaseURL+`/attachments/att-paper.pdf"`,
		`href="/attachments/shared.csv"`, `href="/attachments/att-draft/download"`, `href="/attachments/att-draft-shared/download"`,
		`href="/attachments/att-html/download"`)
	if got := metaContent(page, "citation_pdf_url"); got != testBaseURL+"/attachments/att-paper.pdf" {
		t.Errorf("citation_pdf_url = %q", got)
	}
	ld := jsonLD(t, page)["ScholarlyArticle"]
	media, _ := ld["associatedMedia"].([]any)
	paperURL := ""
	for _, m := range media {
		if m := m.(map[string]any); m["name"] == "att-paper.pdf" {
			paperURL, _ = m["contentUrl"].(string)
		}
	}
	if paperURL != testBaseURL+"/attachments/att-paper.pdf" {
		t.Errorf("associatedMedia = %v", ld["associatedMedia"])
	}
	expectContains(t, read("llms-full.txt"), "[論文](/attachments/att-paper.pdf)", "[共有](/attachments/shared.csv)")
	expectContains(t, read("feed.xml"), `/attachments/att-paper.pdf`)

	// どのファイルにも、公開した資料の公開サーバー向けの URL は残らず、静的な書き出しへのリンクはすべて書き出したファイルを指す。
	err = filepath.WalkDir(outDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || strings.HasPrefix(p, filepath.Join(outDir, "attachments")) {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, id := range []string{"att-paper", "att-shared-a", "att-shared-b"} {
			if strings.Contains(string(data), "/attachments/"+id+"/download") {
				t.Errorf("%s still links /attachments/%s/download", p, id)
			}
		}
		for _, m := range staticAttachmentRefRe.FindAllStringSubmatch(string(data), -1) {
			if _, ok := want[m[1]]; !ok {
				t.Errorf("%s links %s, which was not exported", p, m[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, w := range rep.Warnings {
		warned = warned || (strings.Contains(w, "添付資料") && strings.Contains(w, "3 件"))
	}
	if !warned {
		t.Errorf("Warnings = %q, want one about the 3 attachments that were not exported", rep.Warnings)
	}
}

// 書き出し先（前回の書き出し先）の attachments や資料のファイルがシンボリックリンクなら書き出しを失敗にし、リンク先
// （書き出し先の外）を書き換えない。ハードリンクのファイルは、リンク先を書き換えずに新しいファイルに置き換える。
func TestServerExport_AttachmentDestinationLinksDoNotWriteOutside(t *testing.T) {
	setup := func(t *testing.T) (*testEnv, string, string) {
		env := newTestEnv(t)
		id := publishedArticle(t, env, "公開記事", "public-post")
		seedAttachmentRow(t, env, id, "att-public", "att-public.pdf", []byte("PUBLIC current"))
		outside := filepath.Join(t.TempDir(), "outside")
		writeTestFile(t, filepath.Join(outside, "att-public.pdf"), "ORIGINAL outside")
		outDir := filepath.Join(t.TempDir(), "dist")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		return env, outside, outDir
	}
	expectOutsideUnchanged := func(t *testing.T, outside string) {
		t.Helper()
		if got := dirNames(t, outside); !slices.Equal(got, []string{"att-public.pdf"}) {
			t.Errorf("outside folder = %v", got)
		}
		if data, err := os.ReadFile(filepath.Join(outside, "att-public.pdf")); err != nil || string(data) != "ORIGINAL outside" {
			t.Errorf("outside file = %q, %v; want it unchanged", data, err)
		}
	}

	t.Run("attachments folder is a symlink", func(t *testing.T) {
		env, outside, outDir := setup(t)
		if err := os.Symlink(outside, filepath.Join(outDir, "attachments")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err == nil {
			t.Error("Export succeeded through a symlinked attachments folder")
		}
		expectOutsideUnchanged(t, outside)
	})
	t.Run("attachment file is a symlink", func(t *testing.T) {
		env, outside, outDir := setup(t)
		writeTestFile(t, filepath.Join(outDir, "attachments", "note.txt"), "利用者のメモ")
		if err := os.Symlink(filepath.Join(outside, "att-public.pdf"), filepath.Join(outDir, "attachments", "att-public.pdf")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err == nil {
			t.Error("Export succeeded through a symlinked attachment file")
		}
		expectOutsideUnchanged(t, outside)
		if data, err := os.ReadFile(filepath.Join(outDir, "attachments", "note.txt")); err != nil || string(data) != "利用者のメモ" {
			t.Errorf("user's file = %q, %v", data, err)
		}
	})
	t.Run("attachment file is a hard link", func(t *testing.T) {
		env, outside, outDir := setup(t)
		if err := os.MkdirAll(filepath.Join(outDir, "attachments"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(outside, "att-public.pdf"), filepath.Join(outDir, "attachments", "att-public.pdf")); err != nil {
			t.Skipf("hard link: %v", err)
		}
		if _, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir}); err != nil {
			t.Fatalf("Export: %v", err)
		}
		expectOutsideUnchanged(t, outside)
		if data, _ := os.ReadFile(filepath.Join(outDir, "attachments", "att-public.pdf")); string(data) != "PUBLIC current" {
			t.Errorf("exported attachment = %q", data)
		}
	})
}

// 保存先の資料を調べてから開くまでに、同じ保存先のほかのファイル（下書きの記事の資料）へのシンボリックリンクに
// 差し替えられても、その中身を書き出さない。
func TestServerExport_AttachmentSourceSwappedToSymlink(t *testing.T) {
	env := newTestEnv(t)
	id := publishedArticle(t, env, "公開記事", "public-post")
	draft := seedArticle(t, env.db, article.Article{Title: "下書き", Slug: "draft-post", ContentMarkdown: "下書き", Status: article.StatusDraft})
	seedAttachmentRow(t, env, id, "att-public", "att-public.pdf", []byte("PUBLIC current"))
	seedAttachmentRow(t, env, draft, "att-draft", "att-draft.pdf", []byte("SECRET draft attachment"))
	src := filepath.Join(env.attachmentDir, "att-public.pdf")
	prev := beforeAttachmentOpen
	t.Cleanup(func() { beforeAttachmentOpen = prev })
	beforeAttachmentOpen = func(name string) {
		if name != "att-public.pdf" {
			return
		}
		if err := os.Remove(src); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("att-draft.pdf", src); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}
	outDir := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	expectExportedAttachments(t, outDir, map[string]string{})
	warned := false
	for _, w := range rep.Warnings {
		warned = warned || (strings.Contains(w, "添付資料") && strings.Contains(w, "1 件"))
	}
	if !warned {
		t.Errorf("Warnings = %q, want one about the attachment that was not exported", rep.Warnings)
	}
}

// 書き出しの途中（ページを書き出したあと、資料をコピーする前）で公開をやめた記事の資料はコピーせず、もう一度
// 書き出すよう注意に出す。
func TestServerExport_AttachmentOfArticleUnpublishedDuringExport(t *testing.T) {
	env := newTestEnv(t)
	id := publishedArticle(t, env, "途中で取り下げる記事", "withdrawn")
	seedAttachmentRow(t, env, id, "att-withdrawn", "att-withdrawn.pdf", []byte("SECRET withdrawn attachment"))
	prev := beforeAttachmentCopy
	t.Cleanup(func() { beforeAttachmentCopy = prev })
	beforeAttachmentCopy = func() { unpublish(t, env, id) }
	outDir := filepath.Join(t.TempDir(), "dist")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := filesIn(t, filepath.Join(outDir, "attachments")); len(got) != 0 {
		t.Errorf("attachments = %v, want none", got)
	}
	warned := false
	for _, w := range rep.Warnings {
		warned = warned || (strings.Contains(w, "途中") && strings.Contains(w, "1 件"))
	}
	if !warned {
		t.Errorf("Warnings = %q, want one about the attachment of the article withdrawn during the export", rep.Warnings)
	}
}

// 公開記事の資料のファイルが無いとき、前回の書き出しに残っている古いコピーへはリンクせず（消しもしない）、
// ファイルが無いことと、前回の残りがあることの両方を注意に出す。
func TestServerExport_MissingAttachmentDoesNotLinkOldCopy(t *testing.T) {
	env := newTestEnv(t)
	id := seedArticle(t, env.db, article.Article{Title: "資料が消えた記事", Slug: "gone", ContentMarkdown: "[資料](/attachments/att-gone/download)",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	seedAttachmentRow(t, env, id, "att-gone", "att-gone.pdf", nil)
	outDir := filepath.Join(t.TempDir(), "dist")
	writeTestFile(t, filepath.Join(outDir, "attachments", "att-gone.pdf"), "previously exported copy")
	rep, err := env.server.Export(context.Background(), ExportOptions{OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(outDir, "articles", "gone", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "/attachments/att-gone.pdf") || !strings.Contains(string(page), "/attachments/att-gone/download") {
		t.Errorf("article page links the old copy instead of leaving the link unresolved")
	}
	if data, _ := os.ReadFile(filepath.Join(outDir, "attachments", "att-gone.pdf")); string(data) != "previously exported copy" {
		t.Errorf("old copy = %q, want it kept", data)
	}
	var missing, leftover bool
	for _, w := range rep.Warnings {
		missing = missing || (strings.Contains(w, "1 件") && strings.Contains(w, "att-gone.pdf") && strings.Contains(w, "見つからない"))
		leftover = leftover || (strings.Contains(w, "1 件") && strings.Contains(w, "--clean"))
	}
	if !missing || !leftover {
		t.Errorf("Warnings = %q, want both the missing-file and the leftover warnings", rep.Warnings)
	}
}
