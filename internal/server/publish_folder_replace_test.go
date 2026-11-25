package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sorus/internal/i18n"
	"strings"
	"testing"

	"sorus/internal/publish/publishtest"
)

const (
	folderReplaceQuestion = "このフォルダには、すでにトップページ（index.html）があります。sorus のサイトで置き換えますか？"
	oldFolderTopPage      = "<html><body>前のサイトのトップページ</body></html>"
)

// writeFolderFiles は dir の下に files（相対パス → 内容）を作る。
func writeFolderFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// existingFolderSite は、ほかの方法で作った前のサイト（トップページ index.html と、利用者のファイル）がある
// フォルダを作って返す。
func existingFolderSite(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "public")
	writeFolderFiles(t, dir, map[string]string{
		"index.html":    oldFolderTopPage,
		"memo.txt":      "利用者のメモ",
		"old/page.html": "前のサイトのページ",
	})
	return dir
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// expectFolderUntouched は、dir のファイルが before のままで、トップページも前のサイトのままで、反映の一覧（Manifest）を
// 保存していないことを確かめる。
func expectFolderUntouched(t *testing.T, env *testEnv, dir string, before []string, when string) {
	t.Helper()
	if got := filesIn(t, dir); !slices.Equal(got, before) {
		t.Errorf("%s: folder files = %v, want unchanged %v", when, got, before)
	}
	if got := readText(t, filepath.Join(dir, "index.html")); got != oldFolderTopPage {
		t.Errorf("%s: index.html = %q, want the old top page", when, got)
	}
	id := env.server.loadPublishConfig().destinationID()
	if m, err := env.server.loadPublishManifest(id); err != nil || m != nil {
		t.Errorf("%s: manifest = %v, %v, want none saved", when, m, err)
	}
}

// 初めて書き出すフォルダにトップページ（index.html）があれば、何も書き換えずに置き換えるかを尋ね、「置き換える」を
// 押したときだけ1回反映する。2回目からは尋ねずに、いつもどおり上書きし、sorus が前に書いたファイルだけを消す。
func TestPublishFolderAsksBeforeReplacingExistingTopPage(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	dir := existingFolderSite(t)
	before := filesIn(t, dir)
	saveFolderDestination(t, c, dir)
	id := env.server.loadPublishConfig().destinationID()

	// 尋ねる前は、置き換えを受け付けない。
	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusConflict)
	expectFolderUntouched(t, env, dir, before, "replace before asking")

	out := c.reflectAndWait()
	if out.OK || !out.ReplaceSite || out.Message != folderReplaceQuestion {
		t.Fatalf("first reflect to a folder with index.html = %+v", out)
	}
	expectFolderUntouched(t, env, dir, before, "asking")
	page := c.get("/admin/publish").Body
	expectContains(t, page, folderReplaceQuestion, "まだ何も書き換えていません", `action="/admin/publish/replace-site"`,
		">置き換える</button>", `data-publish-result="confirm"`)
	expectContains(t, c.get("/admin/publish/panel").Body, "置き換えるかの確認を待っています")

	// 「サイトに反映」をもう一度押しても、書き換えずに尋ねる。
	if out := c.reflectAndWait(); out.OK || !out.ReplaceSite {
		t.Fatalf("second reflect without confirmation = %+v", out)
	}
	expectFolderUntouched(t, env, dir, before, "asking again")

	// 「置き換える」を押すと反映する。同じ名前のファイルだけを上書きし、ほかのファイルは残す。
	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusAccepted)
	done := waitReflectDone(t, env.server)
	if !done.OK || done.ReplaceSite || done.Uploaded == 0 {
		t.Fatalf("reflect after confirming = %+v", done)
	}
	if got := readText(t, filepath.Join(dir, "index.html")); got == oldFolderTopPage {
		t.Error("index.html was not replaced after confirming")
	}
	for _, f := range []string{"memo.txt", "old/page.html", "articles/post-1/index.html"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s missing after replacing: %v", f, err)
		}
	}
	if m, err := env.server.loadPublishManifest(id); err != nil || len(m) != done.Uploaded {
		t.Errorf("manifest after replacing = %d entries, %v; uploaded %d", len(m), err, done.Uploaded)
	}
	expectContains(t, strings.Join(done.Details, "\n"), "トップページ（index.html）など")
	// 確認は使い切り。
	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusConflict)
	if strings.Contains(c.get("/admin/publish").Body, `action="/admin/publish/replace-site"`) {
		t.Error("the replace button is still shown after replacing")
	}

	// 2回目からは尋ねずに、いつもどおり反映する。
	gone := publishedArticle(t, env, "あとで下書きに戻す記事", "post-2")
	if out := c.reflectAndWait(); !out.OK || out.ReplaceSite || out.Uploaded == 0 {
		t.Fatalf("next reflect = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "articles", "post-2", "index.html")); err != nil {
		t.Errorf("new article missing: %v", err)
	}
	unpublish(t, env, gone)
	if out := c.reflectAndWait(); !out.OK || out.ReplaceSite || out.Deleted == 0 {
		t.Fatalf("reflect after unpublishing = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "articles", "post-2")); !os.IsNotExist(err) {
		t.Errorf("unpublished article is still in the folder: %v", err)
	}
	for _, f := range []string{"memo.txt", "old/page.html", "articles/post-1/index.html"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s was removed by a later reflect: %v", f, err)
		}
	}
}

const folderReplacedNote = "トップページ（index.html）など、sorus のサイトと同じ名前のファイルは上書きし、それ以外のファイルは消さずに残しました。以前のサイトの古いページや画像が残っていないか、フォルダを確かめてください。"

// 「置き換える」で反映したあとは、上書きしたトップページを「消さずに残した」と数えないよう、もとからあったファイルの件数は
// 出さずに、同じ名前のファイルは上書きし、それ以外は残したことを案内する。ほかのファイルはこれまでどおり残す。
// トップページの無いフォルダへの初めての反映では、これまでどおり件数を出す。
func TestPublishFolderReplaceNoteWithoutKeptCount(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"top page only": {"index.html": oldFolderTopPage},
		"top page and other files": {
			"index.html": oldFolderTopPage, "memo.txt": "利用者のメモ", "old/page.html": "前のサイトのページ",
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			publishedArticle(t, env, "記事", "post-1")
			c := env.loggedIn(admin)
			dir := filepath.Join(t.TempDir(), "public")
			writeFolderFiles(t, dir, files)
			saveFolderDestination(t, c, dir)
			if out := c.reflectAndWait(); !out.ReplaceSite {
				t.Fatalf("first reflect = %+v, want the replace question", out)
			}
			expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusAccepted)
			out := waitReflectDone(t, env.server)
			if !out.OK {
				t.Fatalf("reflect after confirming = %+v", out)
			}
			details := strings.Join(out.Details, "\n")
			expectContains(t, details, folderReplacedNote)
			if strings.Contains(details, "もともとあった") {
				t.Errorf("details = %q, want no count of kept files after replacing", details)
			}
			for f := range files {
				if f == "index.html" {
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
					t.Errorf("%s was removed: %v", f, err)
				}
			}
		})
	}

	t.Run("no top page", func(t *testing.T) {
		env := newTestEnv(t)
		publishedArticle(t, env, "記事", "post-1")
		c := env.loggedIn(admin)
		dir := filepath.Join(t.TempDir(), "public")
		writeFolderFiles(t, dir, map[string]string{"memo.txt": "利用者のメモ", "old/page.html": "前のサイトのページ"})
		saveFolderDestination(t, c, dir)
		out := c.reflectAndWait()
		if !out.OK || out.ReplaceSite {
			t.Fatalf("first reflect = %+v", out)
		}
		details := strings.Join(out.Details, "\n")
		expectContains(t, details, "もともとあった 2 件")
		if strings.Contains(details, folderReplacedNote) {
			t.Errorf("details = %q, want the usual kept count without the replace note", details)
		}
	})
}

// 置き換えるかを尋ねている結果（まだ何も書き出していない）にも、状態の API はフォルダの場所を付ける。画面のスクリプトは、
// 反映できたときだけ「書き出したフォルダ」と出し、確認待ちや失敗では「書き出すフォルダ」と出す（書き出し済みと誤解させない）。
func TestPublishFolderConfirmDoesNotSayWritten(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	dir := existingFolderSite(t)
	saveFolderDestination(t, c, dir)
	if out := c.reflectAndWait(); !out.ReplaceSite || out.Folder == "" {
		t.Fatalf("first reflect = %+v, want the replace question with the folder", out)
	}
	var st publishStatus
	expectStatus(t, c.getJSON("/admin/publish/status", &st), http.StatusOK)
	if st.Last == nil || !st.Last.ReplaceSite || st.Last.OK || st.Last.Folder == "" {
		t.Fatalf("status = %+v, want an unfinished replace question with the folder", st.Last)
	}

	js := c.get("/static/js/reflect.js").Body
	var folderLine string
	for _, line := range strings.Split(js, "\n") {
		if strings.Contains(line, "o.folder") {
			folderLine = line
			break
		}
	}
	if folderLine == "" {
		t.Fatal("reflect.js does not render the folder")
	}
	// 文言は辞書（js_pub_folder_done＝書き出したフォルダ、js_pub_folder_pending＝書き出すフォルダ）から引く。
	if !strings.Contains(folderLine, "o.ok ? T('js_pub_folder_done', o.folder) : T('js_pub_folder_pending', o.folder)") {
		t.Errorf("reflect.js folder line = %q, want 書き出したフォルダ only for a finished reflect", folderLine)
	}
	if n := strings.Count(js, "js_pub_folder_done"); n != 1 {
		t.Errorf("reflect.js says 書き出したフォルダ %d times, want only the finished case", n)
	}
	if got := i18n.Translate("ja", "js_pub_folder_done", "x"); got != "書き出したフォルダ: x" {
		t.Errorf("js_pub_folder_done = %q", got)
	}
}

// 「置き換える」は、尋ねたフォルダへの次の1回の反映だけに使う。公開先を変えたあとは、前の確認では書き換えない。
func TestPublishFolderReplaceApprovalOnlyForAskedFolder(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	a, b := existingFolderSite(t), existingFolderSite(t)
	beforeA, beforeB := filesIn(t, a), filesIn(t, b)

	saveFolderDestination(t, c, a)
	if out := c.reflectAndWait(); !out.ReplaceSite {
		t.Fatalf("reflect to folder a = %+v, want the replace question", out)
	}
	// 尋ねたあとで公開先を変えたら、前の確認の「置き換える」は出さず、受け付けない。
	saveFolderDestination(t, c, b)
	if strings.Contains(c.get("/admin/publish").Body, `action="/admin/publish/replace-site"`) {
		t.Error("the replace button is shown for a folder that was not asked about")
	}
	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusConflict)
	if out := c.reflectAndWait(); !out.ReplaceSite || out.Message != folderReplaceQuestion {
		t.Fatalf("reflect to folder b = %+v, want the replace question", out)
	}
	idB := env.server.loadPublishConfig().destinationID()
	expectFolderUntouched(t, env, b, beforeB, "folder b after switching")

	// b の「置き換える」が押された直後（反映が始まる前）に a へ変えても、b への確認では a を書き換えない。
	st := env.server.publishState()
	st.mu.Lock()
	st.liveSite, st.replaceLive = "", idB
	st.mu.Unlock()
	saveFolderDestination(t, c, a)
	if out := c.reflectAndWait(); !out.ReplaceSite {
		t.Fatalf("reflect to folder a with b's confirmation = %+v, want the replace question", out)
	}
	expectFolderUntouched(t, env, a, beforeA, "folder a with b's confirmation")
	// 使わなかった確認も残らない。b に戻しても、また尋ねる。
	saveFolderDestination(t, c, b)
	if out := c.reflectAndWait(); !out.ReplaceSite {
		t.Fatalf("reflect to folder b after its confirmation was dropped = %+v", out)
	}
	expectFolderUntouched(t, env, b, beforeB, "folder b after its confirmation was dropped")
}

// トップページの無いフォルダ（まだ無いフォルダ・空のフォルダ）には、尋ねずに書き出す。2回目も尋ねない。
func TestPublishFolderWithoutTopPageDoesNotAsk(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T) string{
		"missing": func(t *testing.T) string { return filepath.Join(t.TempDir(), "new-site") },
		"empty": func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), "empty-site")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return dir
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			publishedArticle(t, env, "記事", "post-1")
			c := env.loggedIn(admin)
			dir := mk(t)
			saveFolderDestination(t, c, dir)
			first := c.reflectAndWait()
			if !first.OK || first.ReplaceSite || first.Uploaded == 0 {
				t.Fatalf("first reflect = %+v", first)
			}
			if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
				t.Fatalf("index.html missing: %v", err)
			}
			publishedArticle(t, env, "次の記事", "post-2")
			if out := c.reflectAndWait(); !out.OK || out.ReplaceSite || out.Uploaded == 0 {
				t.Fatalf("second reflect = %+v", out)
			}
		})
	}
}

// index.html という名前のフォルダがあるときは、置き換えられないため、何も書かずに止めて理由を出す。
func TestPublishFolderTopPageFolderStopsBeforeWriting(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	dir := filepath.Join(t.TempDir(), "public")
	writeFolderFiles(t, dir, map[string]string{"index.html/inner.txt": "フォルダの中身", "memo.txt": "メモ"})
	before := filesIn(t, dir)
	saveFolderDestination(t, c, dir)

	out := c.reflectAndWait()
	if out.OK || out.ReplaceSite || !strings.Contains(out.Message, "「index.html」という名前のフォルダ") {
		t.Fatalf("reflect to a folder whose index.html is a folder = %+v", out)
	}
	if got := filesIn(t, dir); !slices.Equal(got, before) {
		t.Errorf("folder files = %v, want unchanged %v", got, before)
	}
	expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusConflict)
}

// 初めての反映が途中で失敗したら一覧を保存しない。トップページがあったフォルダでは、「置き換える」は使い切りのため
// やり直しでもう一度尋ね、トップページの無いフォルダでは、やり直しでも尋ねない。
func TestPublishFolderFirstReflectFailureAndRetry(t *testing.T) {
	t.Run("with top page", func(t *testing.T) {
		env := newTestEnv(t)
		publishedArticle(t, env, "記事", "post-1")
		c := env.loggedIn(admin)
		dir := existingFolderSite(t)
		// articles という名前のファイルがあると、記事のページを書けずに失敗する（トップページは最後に書く）。
		writeFolderFiles(t, dir, map[string]string{"articles": "ふさいでいるファイル"})
		saveFolderDestination(t, c, dir)
		id := env.server.loadPublishConfig().destinationID()

		if out := c.reflectAndWait(); !out.ReplaceSite {
			t.Fatalf("first reflect = %+v, want the replace question", out)
		}
		expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusAccepted)
		if out := waitReflectDone(t, env.server); out.OK || out.ReplaceSite {
			t.Fatalf("confirmed reflect into a blocked folder = %+v, want a failure", out)
		}
		if got := readText(t, filepath.Join(dir, "index.html")); got != oldFolderTopPage {
			t.Errorf("index.html = %q after the failure, want the old top page", got)
		}
		if m, err := env.server.loadPublishManifest(id); err != nil || m != nil {
			t.Errorf("manifest after the failure = %v, %v, want none", m, err)
		}
		// やり直しでは、もう一度尋ねる（前の「置き換える」は使い切った）。
		if out := c.reflectAndWait(); !out.ReplaceSite {
			t.Fatalf("retry after the failure = %+v, want the replace question", out)
		}
		if err := os.Remove(filepath.Join(dir, "articles")); err != nil {
			t.Fatal(err)
		}
		expectStatus(t, c.postJSONAccept("/admin/publish/replace-site"), http.StatusAccepted)
		if out := waitReflectDone(t, env.server); !out.OK {
			t.Fatalf("confirmed retry = %+v", out)
		}
		if got := readText(t, filepath.Join(dir, "index.html")); got == oldFolderTopPage {
			t.Error("index.html was not replaced by the confirmed retry")
		}
	})
	t.Run("without top page", func(t *testing.T) {
		env := newTestEnv(t)
		publishedArticle(t, env, "記事", "post-1")
		c := env.loggedIn(admin)
		dir := filepath.Join(t.TempDir(), "public")
		writeFolderFiles(t, dir, map[string]string{"articles": "ふさいでいるファイル"})
		saveFolderDestination(t, c, dir)
		if out := c.reflectAndWait(); out.OK || out.ReplaceSite {
			t.Fatalf("first reflect into a blocked folder = %+v, want a failure", out)
		}
		if err := os.Remove(filepath.Join(dir, "articles")); err != nil {
			t.Fatal(err)
		}
		if out := c.reflectAndWait(); !out.OK || out.ReplaceSite {
			t.Fatalf("retry = %+v, want success without asking", out)
		}
	})
}

// フォルダ以外の公開先（レンタルサーバー）の初めての反映は、これまでどおり尋ねずに送る。
func TestPublishFTPFirstReflectDoesNotAskAboutTopPage(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	root := t.TempDir()
	writeFolderFiles(t, root, map[string]string{"public_html/index.html": oldFolderTopPage, "public_html/old.html": "前からあるページ"})
	srv := publishtest.NewFTPServer(t, root)
	c := env.loggedIn(admin)
	expectRedirect(t, c.postForm("/admin/publish", url.Values{
		"kind": {"server"}, "protocol": {"ftp"}, "allow_plain_ftp": {"1"}, "host": {"127.0.0.1"}, "port": {strings.TrimPrefix(srv.Addr, "127.0.0.1:")},
		"user": {srv.User}, "password": {srv.Password}, "root": {"/public_html"}, "server_site_url": {testBaseURL},
	}), "/admin/publish?flash=publish_saved")

	if out := c.reflectAndWait(); !out.OK || out.ReplaceSite {
		t.Fatalf("first FTP reflect = %+v, want success without asking", out)
	}
	if got := readText(t, filepath.Join(root, "public_html", "index.html")); got == oldFolderTopPage {
		t.Error("FTP index.html was not uploaded")
	}
	if _, err := os.Stat(filepath.Join(root, "public_html", "old.html")); err != nil {
		t.Errorf("old.html was removed: %v", err)
	}
}
