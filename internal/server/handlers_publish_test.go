package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"sorus/internal/article"
	"sorus/internal/database"
	"sorus/internal/publish/publishtest"
)

// postJSONAccept は fetch と同じく、Accept: application/json と CSRF ヘッダーを付けて path に POST する。
func (c *client) postJSONAccept(path string) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(CSRFHeaderName, c.csrfToken())
	return c.do(req)
}

// reflectAndWait は「サイトに反映」を押し、反映が終わるまで待って結果を返す。
func (c *client) reflectAndWait() publishOutcome {
	c.t.Helper()
	res := c.postJSONAccept("/admin/publish/reflect")
	expectStatus(c.t, res, http.StatusAccepted)
	return waitReflectDone(c.t, c.env.server)
}

// waitReflectDone は反映が終わるまで待ち、最後の結果を返す。
func waitReflectDone(t *testing.T, s *Server) publishOutcome {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := s.publishState()
		st.mu.Lock()
		running, last, pending := st.running, st.last, st.timer != nil
		st.mu.Unlock()
		if !running && !pending && last != nil {
			return *last
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the reflect to finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func publishedArticle(t *testing.T, env *testEnv, title, slug string) string {
	t.Helper()
	return seedArticle(t, env.db, article.Article{Title: title, Slug: slug, ContentMarkdown: title + "の本文",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
}

func unpublish(t *testing.T, env *testEnv, id string) {
	t.Helper()
	if _, err := env.db.Exec(`UPDATE articles SET status = 'draft' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

func saveFolderDestination(t *testing.T, c *client, dir string) {
	t.Helper()
	expectRedirect(t, c.postForm("/admin/publish", url.Values{
		"kind": {"folder"}, "folder_path": {dir}, "folder_site_url": {testBaseURL},
	}), "/admin/publish?flash=publish_saved")
}

func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestPublishFolderReflect(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "残る記事", "keep-me")
	gone := publishedArticle(t, env, "非公開にする記事", "unpublish-me")
	c := env.loggedIn(admin)
	out := filepath.Join(t.TempDir(), "site-export")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "memo.txt"), []byte("利用者のメモ"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveFolderDestination(t, c, out)
	if got := env.server.getSetting("base_url", ""); got != testBaseURL {
		t.Errorf("base_url = %q, want %q saved from 公開サイトのURL", got, testBaseURL)
	}

	first := c.reflectAndWait()
	if !first.OK || first.Uploaded == 0 || first.Deleted != 0 || first.SiteURL != testBaseURL {
		t.Fatalf("first reflect = %+v", first)
	}
	if !strings.Contains(strings.Join(first.Details, "\n"), "もともとあった 1 件") {
		t.Errorf("details = %q, want a note that the existing file was kept", first.Details)
	}
	for _, f := range []string{"index.html", "articles/keep-me/index.html", "articles/unpublish-me/index.html", "memo.txt"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s missing after the first reflect: %v", f, err)
		}
	}
	files1 := filesIn(t, out)

	unpublish(t, env, gone)
	second := c.reflectAndWait()
	if !second.OK || second.Deleted == 0 {
		t.Fatalf("second reflect = %+v", second)
	}
	files2 := filesIn(t, out)
	var removed []string
	for _, f := range files1 {
		if !slices.Contains(files2, f) {
			removed = append(removed, f)
		}
	}
	// 消えたのは非公開にした記事のファイルだけで、利用者のファイルとほかの記事は残る。
	if len(removed) != second.Deleted {
		t.Errorf("removed %v but reported %d deleted", removed, second.Deleted)
	}
	for _, f := range removed {
		if !strings.HasPrefix(f, "articles/unpublish-me/") {
			t.Errorf("removed unrelated file %s", f)
		}
	}
	for _, f := range []string{"memo.txt", "articles/keep-me/index.html"} {
		if !slices.Contains(files2, f) {
			t.Errorf("%s was removed", f)
		}
	}

	// 状態の API と画面に結果が出る。
	res := c.get("/admin/publish/status")
	expectStatus(t, res, http.StatusOK)
	var st publishStatus
	if err := json.Unmarshal([]byte(res.Body), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Configured || st.Running || st.Last == nil || !st.Last.OK {
		t.Errorf("status = %+v", st)
	}
	expectContains(t, c.get("/admin/publish").Body, "フォルダに書き出しました", "公開サイトを開く", "送ったファイル")
}

func TestPublishFTPReflectDeletesOnlyUnpublishedArticle(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "残る記事", "keep-me")
	gone := publishedArticle(t, env, "非公開にする記事", "unpublish-me")
	root := t.TempDir()
	// アップロード先にもともとある、利用者のファイル。
	if err := os.MkdirAll(filepath.Join(root, "public_html"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "public_html", "old.html"), []byte("前からあるページ"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := publishtest.NewFTPServer(t, root)
	c := env.loggedIn(admin)
	expectRedirect(t, c.postForm("/admin/publish", url.Values{
		"kind": {"server"}, "protocol": {"ftp"}, "allow_plain_ftp": {"1"}, "host": {"127.0.0.1"}, "port": {strings.TrimPrefix(srv.Addr, "127.0.0.1:")},
		"user": {srv.User}, "password": {srv.Password}, "root": {"/public_html"}, "server_site_url": {testBaseURL},
	}), "/admin/publish?flash=publish_saved")

	first := c.reflectAndWait()
	if !first.OK {
		t.Fatalf("first reflect = %+v", first)
	}
	id := env.server.loadPublishConfig().destinationID()
	m1, err := env.server.loadPublishManifest(id)
	if err != nil || len(m1) != first.Uploaded {
		t.Fatalf("manifest after the first reflect = %d entries, %v; uploaded %d", len(m1), err, first.Uploaded)
	}
	remote := filepath.Join(root, "public_html")
	if got := filesIn(t, remote); len(got) != len(m1)+1 {
		t.Errorf("remote has %d files, want all %d plus old.html", len(got), len(m1))
	}

	unpublish(t, env, gone)
	second := c.reflectAndWait()
	if !second.OK {
		t.Fatalf("second reflect = %+v", second)
	}
	m2, _ := env.server.loadPublishManifest(id)
	var wantGone []string
	for p := range m1 {
		if _, ok := m2[p]; !ok {
			wantGone = append(wantGone, p)
		}
	}
	if len(wantGone) == 0 || second.Deleted != len(wantGone) {
		t.Fatalf("deleted %d, want %d (%v)", second.Deleted, len(wantGone), wantGone)
	}
	for _, p := range wantGone {
		if !strings.HasPrefix(p, "articles/unpublish-me/") {
			t.Errorf("manifest dropped unrelated file %s", p)
		}
		if _, err := os.Stat(filepath.Join(remote, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Errorf("%s still on the remote: %v", p, err)
		}
	}
	if got := filesIn(t, remote); len(got) != len(m2)+1 || !slices.Contains(got, "old.html") || !slices.Contains(got, "articles/keep-me/index.html") {
		t.Errorf("remote files = %v", got)
	}
}

func TestPublishFailedUploadKeepsManifest(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	root := t.TempDir()
	srv := publishtest.NewFTPServer(t, root)
	fail := true
	srv.FailStor = func(p string) bool { return fail && p == "/www/index.html" }
	c := env.loggedIn(admin)
	expectRedirect(t, c.postForm("/admin/publish", url.Values{
		"kind": {"server"}, "protocol": {"ftp"}, "allow_plain_ftp": {"1"}, "host": {"127.0.0.1"}, "port": {strings.TrimPrefix(srv.Addr, "127.0.0.1:")},
		"user": {srv.User}, "password": {srv.Password}, "root": {"/www"}, "server_site_url": {testBaseURL},
	}), "/admin/publish")

	out := c.reflectAndWait()
	if out.OK || !strings.Contains(out.Message, "反映できませんでした") || out.Error == "" {
		t.Fatalf("failed reflect = %+v", out)
	}
	id := env.server.loadPublishConfig().destinationID()
	if m, _ := env.server.loadPublishManifest(id); m != nil {
		t.Fatalf("manifest saved after a failed upload (%d entries)", len(m))
	}

	fail = false
	out = c.reflectAndWait()
	if !out.OK {
		t.Fatalf("retry = %+v", out)
	}
	if m, _ := env.server.loadPublishManifest(id); len(m) == 0 {
		t.Error("manifest not saved after a successful reflect")
	}
}

func TestPublishSFTPChangedHostKeyBlocksReflect(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	root := t.TempDir()
	srv := publishtest.NewSFTPServer(t, publishtest.NewHostKey(t))
	c := env.loggedIn(admin)
	remote := filepath.ToSlash(filepath.Join(root, "www"))
	expectRedirect(t, c.postForm("/admin/publish", url.Values{
		"kind": {"server"}, "protocol": {"sftp"}, "host": {"127.0.0.1"}, "port": {strings.TrimPrefix(srv.Addr, "127.0.0.1:")},
		"user": {srv.User}, "password": {srv.Password}, "root": {remote}, "server_site_url": {testBaseURL},
	}), "/admin/publish")
	if out := c.reflectAndWait(); !out.OK {
		t.Fatalf("first reflect = %+v", out)
	}
	if got := env.server.knownHostKeys()["127.0.0.1:"+strings.TrimPrefix(srv.Addr, "127.0.0.1:")]; got != srv.Fingerprint() {
		t.Fatalf("remembered host key = %q, want %q", got, srv.Fingerprint())
	}

	srv.SetHostKey(publishtest.NewHostKey(t))
	publishedArticle(t, env, "新しい記事", "post-2")
	out := c.reflectAndWait()
	if out.OK || !out.HostKeyChanged {
		t.Fatalf("reflect with a changed host key = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(root, "www", "articles", "post-2")); !os.IsNotExist(err) {
		t.Errorf("uploaded despite the changed host key: %v", err)
	}
	page := c.get("/admin/publish").Body
	// 指紋の「+」は画面では &#43; と書かれるため、テンプレートと同じく書き出した形で探す。
	var fp strings.Builder
	if err := template.Must(template.New("fp").Parse("{{.}}")).Execute(&fp, srv.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	expectContains(t, page, "接続先のサーバーの鍵が、前に接続したときと違います", fp.String())

	// 違う指紋では信頼しない。
	expectStatus(t, c.postForm("/admin/publish/trust-host-key", url.Values{"fingerprint": {"SHA256:other"}}), http.StatusConflict)
	expectRedirect(t, c.postForm("/admin/publish/trust-host-key", url.Values{"fingerprint": {srv.Fingerprint()}}),
		"/admin/publish?flash=publish_trusted")
	if out := c.reflectAndWait(); !out.OK {
		t.Fatalf("reflect after trusting = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(root, "www", "articles", "post-2", "index.html")); err != nil {
		t.Errorf("new article not uploaded after trusting the key: %v", err)
	}
}

func TestPublishSaveValidatesAndHidesSecrets(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	base := url.Values{"kind": {"server"}, "protocol": {"sftp"}, "host": {"sv1.example.jp"}, "user": {"u"},
		"password": {"pass-s3cret-123"}, "server_site_url": {testBaseURL}}

	for _, root := range []string{"/", "", "."} {
		form := url.Values{}
		for k, v := range base {
			form[k] = v
		}
		form.Set("root", root)
		res := c.postForm("/admin/publish", form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "空欄や「/」だけは指定できません")
		if strings.Contains(res.Body, "pass-s3cret-123") {
			t.Error("password rendered back on a validation error")
		}
	}
	if env.server.loadPublishConfig().configured() {
		t.Fatal("refused config was saved")
	}

	base.Set("root", "/public_html")
	expectRedirect(t, c.postForm("/admin/publish", base), "/admin/publish?flash=publish_saved")
	cfToken := "cf-token-abcdefghijklmnopqrstuvwxyz"
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"cloudflare"},
		"cf_account_id": {"0123456789abcdef0123456789abcdef"}, "cf_api_token": {cfToken}, "cf_project": {"my-media"}}),
		"/admin/publish")
	page := c.get("/admin/publish").Body
	for _, secret := range []string{"pass-s3cret-123", cfToken} {
		if strings.Contains(page, secret) {
			t.Errorf("publish page renders the secret %q", secret)
		}
	}
	expectContains(t, page, "保存済み", "my-media")
	cfg := env.server.loadPublishConfig()
	if cfg.Kind != publishKindCloudflare || cfg.Cloudflare.APIToken != cfToken || cfg.Server.Password != "pass-s3cret-123" {
		t.Errorf("saved config = %+v", cfg)
	}
	// 公開サイトURLは、レンタルサーバーの設定で入れた値のまま（未設定・localhost のときだけ Pages の URL にする）。
	if got := env.server.getSetting("base_url", ""); got != testBaseURL {
		t.Errorf("base_url = %q, want the URL entered for the rental server", got)
	}

	// 秘密の欄を空のまま保存し直しても、保存済みの値を残す。
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"cloudflare"},
		"cf_account_id": {"0123456789abcdef0123456789abcdef"}, "cf_project": {"my-media"}}), "/admin/publish")
	if got := env.server.loadPublishConfig().Cloudflare.APIToken; got != cfToken {
		t.Errorf("token after saving with an empty field = %q", got)
	}
}

func TestPublishS3ValidatesRegionAndTestsWriteDelete(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	var methods []string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method != http.MethodPut && r.Method != http.MethodDelete {
			t.Errorf("unexpected S3 connection test method %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, ".sorus-connection-test-") {
			t.Errorf("connection test path = %q", r.URL.Path)
		}
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			if string(body) != "sorus" {
				t.Errorf("probe body = %q", body)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer remote.Close()
	form := url.Values{"kind": {"s3"}, "s3_endpoint": {remote.URL}, "s3_bucket": {"site"}, "s3_region": {"bad region"},
		"s3_access_key_id": {"access"}, "s3_secret_access_key": {"secret"}}
	res := c.postForm("/admin/publish", form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "リージョンは半角英小文字・数字・ハイフン")
	if env.server.loadPublishConfig().configured() {
		t.Fatal("invalid S3 region was saved")
	}

	form.Set("s3_region", "auto")
	res = c.postForm("/admin/publish/test", form)
	expectStatus(t, res, http.StatusOK)
	var result publishTestResult
	if err := json.Unmarshal([]byte(res.Body), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !strings.Contains(strings.Join(result.Details, " "), "書き込み、削除") {
		t.Fatalf("S3 connection test = %+v", result)
	}
	if strings.Join(methods, ",") != "PUT,DELETE" {
		t.Fatalf("S3 probe methods = %v", methods)
	}
	if env.server.loadPublishConfig().configured() {
		t.Fatal("S3 connection test saved the config")
	}
}

func TestPublishS3SettingsSecretAndValidation(t *testing.T) {
	env := newTestEnv(t)
	if err := env.server.saveBaseURL("https://old.example.test"); err != nil {
		t.Fatal(err)
	}
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"s3"}, "s3_endpoint": {"https://storage.example.test"}, "s3_bucket": {"sorus-site"},
		"s3_region": {"auto"}, "s3_access_key_id": {"access-1"}, "s3_secret_access_key": {"secret-original"}, "s3_site_url": {testBaseURL}}
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	if cfg := env.server.loadPublishConfig(); cfg.S3.Endpoint != form.Get("s3_endpoint") || cfg.S3.Bucket != "sorus-site" || cfg.S3.Region != "auto" || cfg.S3.AccessKeyID != "access-1" || cfg.S3.SecretAccessKey != "secret-original" {
		t.Fatalf("saved S3 config = %+v", cfg.S3)
	}
	if got := env.server.getSetting("base_url", ""); got != testBaseURL {
		t.Fatalf("base_url = %q, want the S3 public site URL", got)
	}
	page := c.get("/admin/publish").Body
	if strings.Contains(page, "secret-original") || strings.Contains(page, `value="secret-original"`) {
		t.Fatal("saved S3 secret was echoed into the page")
	}
	expectContains(t, page, "保存済み", `name="s3_secret_access_key"`)

	// Leaving the password blank for the same destination retains the saved secret.
	form.Del("s3_secret_access_key")
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	if got := env.server.loadPublishConfig().S3.SecretAccessKey; got != "secret-original" {
		t.Fatalf("secret after blank same-destination save = %q", got)
	}

	// A destination identity change cannot silently reuse the previous destination's key.
	for _, tc := range []struct{ field, value string }{{"s3_endpoint", "https://other.example.test"}, {"s3_bucket", "other-bucket"}, {"s3_access_key_id", "access-2"}} {
		changed := url.Values{}
		for k, values := range form {
			changed[k] = append([]string(nil), values...)
		}
		changed.Set(tc.field, tc.value)
		res := c.postForm("/admin/publish", changed)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, tc.value)
		if strings.Contains(res.Body, "secret-original") {
			t.Errorf("%s validation response echoed the stored secret", tc.field)
		}
		if got := env.server.loadPublishConfig().S3.SecretAccessKey; got != "secret-original" {
			t.Errorf("%s failed save changed saved secret to %q", tc.field, got)
		}
	}

	// An invalid region re-renders the non-secret fields the user entered.
	badRegion := url.Values{}
	for k, values := range form {
		badRegion[k] = append([]string(nil), values...)
	}
	badRegion.Set("s3_region", "bad region")
	res := c.postForm("/admin/publish", badRegion)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, `value="https://storage.example.test"`, `value="sorus-site"`, `value="bad region"`, `value="access-1"`, `value="`+testBaseURL+`"`)
	if strings.Contains(res.Body, "secret-original") {
		t.Fatal("S3 validation response echoed the stored secret")
	}
}

func TestOpenPublishRemoteS3(t *testing.T) {
	var gotQuery string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer remote.Close()
	env := newTestEnv(t)
	r, err := env.server.openPublishRemote(context.Background(), publishConfig{Kind: publishKindS3, S3: publishS3Config{
		Endpoint: remote.URL, Bucket: "site", Region: "auto", AccessKeyID: "access", SecretAccessKey: "secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.CountEntries(context.Background()); err != nil {
		t.Fatalf("CountEntries via openPublishRemote: %v", err)
	}
	if !strings.Contains(gotQuery, "delimiter=%2F") || !strings.Contains(gotQuery, "list-type=2") {
		t.Fatalf("S3 list query = %q", gotQuery)
	}
}

func TestPublishS3ReflectSavesManifestAndPurgesCDN(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "S3 公開", "s3-post")
	var methods []string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
		case http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected S3 reflect method %s", r.Method)
		}
	}))
	defer remote.Close()
	c := env.loggedIn(admin)
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"s3"}, "s3_endpoint": {remote.URL}, "s3_bucket": {"site"},
		"s3_region": {"auto"}, "s3_access_key_id": {"access"}, "s3_secret_access_key": {"secret"}, "s3_site_url": {testBaseURL}}),
		"/admin/publish?flash=publish_saved")
	for key, value := range map[string]string{"cdn_purge_enabled": "true", "cdn_cf_zone_id": "zone-123", "cdn_cf_api_token": "token-secret"} {
		if err := upsertSetting(env.db, key, value); err != nil {
			t.Fatal(err)
		}
	}
	oldPurge := purgeCDNCache
	t.Cleanup(func() { purgeCDNCache = oldPurge })
	purgeCalls := 0
	purgeCDNCache = func(_ context.Context, _ *http.Client, token, zone string, _ []string) error {
		purgeCalls++
		if token != "token-secret" || zone != "zone-123" {
			t.Errorf("purge credentials = %q / %q", token, zone)
		}
		return nil
	}
	first := c.reflectAndWait()
	if !first.OK || first.Uploaded == 0 || purgeCalls != 1 {
		t.Fatalf("first S3 reflect = %+v, CDN purge calls = %d", first, purgeCalls)
	}
	id := env.server.loadPublishConfig().destinationID()
	manifest, err := env.server.loadPublishManifest(id)
	if err != nil || len(manifest) != first.Uploaded {
		t.Fatalf("S3 manifest has %d entries, err=%v; uploaded=%d", len(manifest), err, first.Uploaded)
	}
	if !slices.Contains(methods, http.MethodPut) {
		t.Fatalf("S3 reflect methods = %v, want uploads", methods)
	}
	second := c.reflectAndWait()
	if !second.OK || second.Uploaded != 0 || second.Deleted != 0 || purgeCalls != 1 {
		t.Fatalf("unchanged S3 reflect = %+v, CDN purge calls = %d", second, purgeCalls)
	}
}

func TestS3MissingSiteURLLinksToS3Field(t *testing.T) {
	env := newTestEnv(t)
	out := env.server.siteURLMissing(publishOutcome{}, publishConfig{Kind: publishKindS3})
	if out.FixSiteURL != "/admin/publish#publish-s3-site-url" {
		t.Fatalf("fix site URL = %q", out.FixSiteURL)
	}
}

func TestPublishCloudflareFillsBaseURL(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: "http://localhost:8080"}, nil)
	c := env.loggedIn(admin)
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"cloudflare"},
		"cf_account_id": {"0123456789abcdef0123456789abcdef"}, "cf_api_token": {"tok"}, "cf_project": {"my-media"}}), "/admin/publish")
	if got := env.server.getSetting("base_url", ""); got != "https://my-media.pages.dev" {
		t.Errorf("base_url = %q, want https://my-media.pages.dev", got)
	}
	if env.server.cfg.BaseURL != "https://my-media.pages.dev" {
		t.Errorf("cfg.BaseURL = %q", env.server.cfg.BaseURL)
	}
}

func TestPublishAdminOnly(t *testing.T) {
	env := newTestEnv(t)
	for _, u := range []testUser{studentA, chief} {
		c := env.loggedIn(u)
		for _, p := range []string{"/admin/publish", "/admin/publish/status", "/admin/publish/panel", "/admin/publish/zip"} {
			if res := c.get(p); res.StatusCode != http.StatusForbidden {
				t.Errorf("%s GET %s = %d, want 403", u.Username, p, res.StatusCode)
			}
		}
		if res := c.postJSONAccept("/admin/publish/reflect"); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s POST reflect = %d, want 403", u.Username, res.StatusCode)
		}
	}
	// 公開先が無いうちは「サイトに反映」を始めない。ダッシュボードの部分も出さない（204）。
	c := env.loggedIn(admin)
	expectStatus(t, c.postJSONAccept("/admin/publish/reflect"), http.StatusConflict)
	expectStatus(t, c.get("/admin/publish/panel"), http.StatusNoContent)
	// 公開先が決まれば下段に出せるよう、ダッシュボードには下段の枠（中身は空で隠れる）とスクリプトを置く。
	expectContains(t, c.get("/admin").Body, `data-publish-panel="/admin/publish/panel?compact=1"`, `src="/static/js/reflect.js?v=`)
	expectContains(t, c.get("/admin/settings").Body, `href="/admin/publish"`)
	if js := c.get("/static/js/reflect.js"); js.StatusCode != http.StatusOK || !strings.Contains(js.Body, "/admin/publish/status") {
		t.Errorf("reflect.js = %d", js.StatusCode)
	}
	// スクリプトは static/js/reflect.js から配信し、Go の手作りの配信口は無い。
	expectStatus(t, c.get("/admin/publish/reflect.js"), http.StatusNotFound)
}

func TestPublishZipDownload(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	res := c.get("/admin/publish/zip")
	expectStatus(t, res, http.StatusOK)
	zr, err := zip.NewReader(bytes.NewReader([]byte(res.Body)), int64(len(res.Body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	for _, want := range []string{"index.html", "articles/post-1/index.html"} {
		if !slices.Contains(names, want) {
			t.Errorf("zip lacks %s", want)
		}
	}
}

func TestPublishTestConnection(t *testing.T) {
	env := newTestEnv(t)
	root := t.TempDir()
	srv := publishtest.NewFTPServer(t, root)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"server"}, "protocol": {"ftp"}, "allow_plain_ftp": {"1"}, "host": {"127.0.0.1"}, "port": {strings.TrimPrefix(srv.Addr, "127.0.0.1:")},
		"user": {srv.User}, "password": {srv.Password}, "root": {"/www"}}
	res := c.postForm("/admin/publish/test", form)
	expectStatus(t, res, http.StatusOK)
	var tr publishTestResult
	if err := json.Unmarshal([]byte(res.Body), &tr); err != nil {
		t.Fatal(err)
	}
	if !tr.OK || !strings.Contains(tr.Message, "書き込めます") {
		t.Errorf("test result = %+v", tr)
	}
	if got := filesIn(t, root); len(got) != 0 {
		t.Errorf("test left files on the remote: %v", got)
	}
	if env.server.loadPublishConfig().configured() {
		t.Error("connection test saved the config")
	}
	form.Set("password", "wrong")
	res = c.postForm("/admin/publish/test", form)
	if err := json.Unmarshal([]byte(res.Body), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.OK || !strings.Contains(tr.Message, "受け付けてもらえませんでした") {
		t.Errorf("test with a wrong password = %+v", tr)
	}
}

func TestScheduledPublishTriggersOneDebouncedReflect(t *testing.T) {
	env := newTestEnvWith(t, Config{SchedulerInterval: 20 * time.Millisecond}, nil)
	st := env.server.publishState()
	st.mu.Lock()
	st.debounce = 300 * time.Millisecond
	st.mu.Unlock()
	c := env.loggedIn(admin)
	out := filepath.Join(t.TempDir(), "site-export")
	saveFolderDestination(t, c, out)

	// 続けて予定日時を迎える3件。別々の周期で公開されても、反映は1回にまとめる。
	now := time.Now()
	for i, slug := range []string{"sched-1", "sched-2", "sched-3"} {
		at := now.Add(time.Duration(50+40*i) * time.Millisecond)
		seedArticle(t, env.db, article.Article{Title: "予約" + slug, Slug: slug, ContentMarkdown: "予約投稿",
			Status: article.StatusPublished, PublishedAt: &at})
	}
	got := waitReflectDone(t, env.server)
	if !got.OK || got.Trigger != publishTriggerSchedule {
		t.Fatalf("scheduled reflect = %+v", got)
	}
	for _, slug := range []string{"sched-1", "sched-2", "sched-3"} {
		if _, err := os.Stat(filepath.Join(out, "articles", slug, "index.html")); err != nil {
			t.Errorf("%s not reflected: %v", slug, err)
		}
	}
	// 待ち時間を過ぎても、2回目の反映は起きない。
	time.Sleep(2 * st.debounce)
	st.mu.Lock()
	reflects := st.reflects
	st.mu.Unlock()
	if reflects != 1 {
		t.Errorf("reflects = %d, want exactly 1 for a burst of scheduled publishes", reflects)
	}
}

func TestScheduledPublishWithoutDestinationDoesNotReflect(t *testing.T) {
	env := newTestEnv(t)
	st := env.server.publishState()
	env.server.onScheduledPublished([]string{"a", "b"})
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.timer != nil || st.reflects != 0 {
		t.Errorf("timer = %v reflects = %d, want nothing without a destination", st.timer, st.reflects)
	}
}

// newDesktopEnvWith はデスクトップモードのサーバーを、seed で DB を用意してから起動する。
func newDesktopEnvWith(t *testing.T, seed func(env *testEnv)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Connect(filepath.Join(dir, "desktop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	env := &testEnv{t: t, db: db, uploadDir: filepath.Join(dir, "uploads"), attachmentDir: filepath.Join(dir, "attachments")}
	seed(env)
	s, err := New(Config{DB: db, UploadDir: env.uploadDir, AttachmentDir: env.attachmentDir, Desktop: true,
		SchedulerInterval: 20 * time.Millisecond, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	env.server = s
	env.srv = httptest.NewServer(s.Handler())
	t.Cleanup(env.srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return env
}

// seedOverdueDesktop は、管理者 w（writer0）・フォルダの公開先 out・公開サイトURLと、アプリを閉じている間に予約時刻を
// 過ぎた記事を用意し、その記事の ID を返す。
func seedOverdueDesktop(t *testing.T, env *testEnv, out string) string {
	t.Helper()
	if _, err := env.db.Exec(`INSERT INTO categories (id, name, slug) VALUES (1, '未分類', 'uncategorized')`); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w', 'writer0', 'w', ?, 'admin')`, string(hash)); err != nil {
		t.Fatal(err)
	}
	id := seedScheduled(t, env.db, article.Article{AuthorID: "w", Title: "閉じている間の予約", Slug: "while-closed",
		ContentMarkdown: "本文"}, *ago(time.Hour))
	cfg := publishConfig{Kind: publishKindFolder, Folder: publishFolderConfig{Path: out}}
	b, _ := json.Marshal(cfg.Folder)
	for k, v := range map[string]string{publishSettingKind: cfg.Kind, publishSettingFolder: string(b), "base_url": testBaseURL} {
		if err := upsertSetting(env.db, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestDesktopOverdueScheduledArticlesAskOnce(t *testing.T) {
	out := filepath.Join(t.TempDir(), "site-export")
	var overdueID string
	env := newDesktopEnvWith(t, func(env *testEnv) { overdueID = seedOverdueDesktop(t, env, out) })
	waitForCond(t, "overdue article published on start", func() bool {
		return article.IsVisible(env.mustGetArticle(overdueID), time.Now())
	})
	st := env.server.publishState()
	waitForCond(t, "overdue article counted", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.overdue == 1
	})

	c := env.loggedIn(testUser{ID: "w", Username: "writer0"})
	panel := c.get("/admin/publish/panel")
	expectStatus(t, panel, http.StatusOK)
	expectContains(t, panel.Body, "予約時刻を過ぎた記事が 1 件あります。サイトに反映しますか？")
	if again := c.get("/admin/publish/panel").Body; strings.Contains(again, "予約時刻を過ぎた記事") {
		t.Error("overdue question shown twice")
	}
	st.mu.Lock()
	reflects := st.reflects
	st.mu.Unlock()
	if reflects != 0 {
		t.Errorf("reflects = %d, want 0: overdue articles are not sent without asking", reflects)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("export folder created without asking: %v", err)
	}
}

// 公開サイトURLが未設定、またはこのパソコンの中だけのアドレスのままなら、書き出さずに断り、入力欄へ案内する。
func TestReflectRequiresPublicSiteURL(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	c := env.loggedIn(admin)
	out := filepath.Join(t.TempDir(), "site")
	expectRedirect(t, c.postForm("/admin/publish", url.Values{"kind": {"folder"}, "folder_path": {out}}), "/admin/publish?flash=publish_saved")
	for _, base := range []string{"", "http://localhost:8080"} {
		if err := env.server.saveBaseURL(base); err != nil {
			t.Fatal(err)
		}
		got := c.reflectAndWait()
		if got.OK {
			t.Fatalf("reflect with base URL %q = %+v, want a refusal", base, got)
		}
		expectContains(t, got.Message, "公開サイトのURL", "反映していません")
		if got.FixSiteURL != "/admin/publish#publish-folder-site-url" || got.Folder != "" {
			t.Errorf("fix link = %q, folder = %q", got.FixSiteURL, got.Folder)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("base URL %q: export folder written: %v", base, err)
		}
	}
	expectContains(t, c.get("/admin/publish").Body, `href="/admin/publish#publish-folder-site-url"`, "「公開サイトのURL」を入力する")
	expectContains(t, c.get("/admin/publish/panel").Body, `href="/admin/publish#publish-folder-site-url"`)

	saveFolderDestination(t, c, out)
	if got := c.reflectAndWait(); !got.OK || got.FixSiteURL != "" {
		t.Fatalf("reflect with a public site URL = %+v", got)
	}
}

// 尋ねたあと「反映」を押さないうちに別の予約投稿の時刻が来ても、自動では送らない。押して反映できたら、また自動で送る。
func TestDesktopOverdueBlocksLaterScheduledReflect(t *testing.T) {
	out := filepath.Join(t.TempDir(), "site-export")
	var overdueID, laterID string
	env := newDesktopEnvWith(t, func(env *testEnv) {
		overdueID = seedOverdueDesktop(t, env, out)
		later := time.Now().UTC().Add(1500 * time.Millisecond)
		laterID = seedScheduled(t, env.db, article.Article{AuthorID: "w", Title: "起動したあとの予約", Slug: "after-start",
			ContentMarkdown: "本文"}, later)
	})
	st := env.server.publishState()
	st.mu.Lock()
	st.debounce = 20 * time.Millisecond
	st.mu.Unlock()
	waitForCond(t, "overdue article counted", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.overdue == 1
	})
	c := env.loggedIn(testUser{ID: "w", Username: "writer0"})
	expectContains(t, c.get("/admin/publish/panel").Body, "予約時刻を過ぎた記事が 1 件あります。サイトに反映しますか？")

	waitForCond(t, "later article published", func() bool {
		return article.IsVisible(env.mustGetArticle(laterID), time.Now())
	})
	time.Sleep(300 * time.Millisecond)
	st.mu.Lock()
	reflects, pending := st.reflects, st.timer != nil
	st.mu.Unlock()
	if reflects != 0 || pending {
		t.Errorf("reflects = %d (timer pending %v), want none until the user reflects", reflects, pending)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("export folder created before the user reflected: %v", err)
	}

	if got := c.reflectAndWait(); !got.OK {
		t.Fatalf("manual reflect = %+v", got)
	}
	st.mu.Lock()
	overdue := st.overdue
	st.mu.Unlock()
	if overdue != 0 {
		t.Errorf("overdue after manual reflect = %d, want 0", overdue)
	}
	if !article.IsVisible(env.mustGetArticle(overdueID), time.Now()) {
		t.Error("overdue article is not published")
	}
	if _, err := os.Stat(filepath.Join(out, "index.html")); err != nil {
		t.Errorf("manual reflect did not write the site: %v", err)
	}
}

// ダッシュボードの「サイトに反映」は1通り。公開先が未設定なら何も出さず（204）、設定済みなら目立つ「サイトに反映」を
// 出す。以前の版の「はじめに」で保存した立場（desktop_intent）は読まない。
func TestDesktopPublishPanelIgnoresStoredIntent(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	if err := upsertSetting(env.db, "desktop_intent", "writer"); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, c.get("/admin/publish/panel"), http.StatusNoContent)
	saveFolderDestination(t, c, filepath.Join(t.TempDir(), "out"))
	expectContains(t, c.get("/admin/publish/panel").Body, `id="publish-dashboard-reflect"`, "サイトに反映", `class="btn btn-primary btn-md"`)
}

// 公開先が未設定なら、ダッシュボードの「サイトに反映」の部分は、上部・下段（compact）・英語の表示・最後の反映が
// 失敗していたときのどれでも、何も返さない（204・本文なし）。「サイトを公開するには」の案内は出さない。公開先を
// 決めたら、今までどおり部分を返す。
func TestPublishPanelEmptyWithoutDestination(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	expectEmpty := func(step string) {
		t.Helper()
		for _, path := range []string{"/admin/publish/panel", "/admin/publish/panel?compact=1"} {
			res := c.get(path)
			expectStatus(t, res, http.StatusNoContent)
			if res.Body != "" {
				t.Errorf("%s: %s body = %q, want empty", step, path, res.Body)
			}
			if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
				t.Errorf("%s: %s Cache-Control = %q, want no-store", step, path, cc)
			}
		}
	}
	expectEmpty("no destination")
	expectNotContains(t, c.get("/admin").Body, "サイトを公開するには", "To publish your site", `id="publish-dashboard-setup"`)

	// 最後の反映が失敗していても、公開先が無ければ出さない。
	st := env.server.publishState()
	st.mu.Lock()
	st.last = &publishOutcome{OK: false, FinishedText: "11/18 10:00", Message: "送れませんでした"}
	st.mu.Unlock()
	expectEmpty("failed reflect, no destination")

	c.useEnglish()
	expectEmpty("english")
	c.useJapanese()

	saveFolderDestination(t, c, filepath.Join(t.TempDir(), "out"))
	panel := c.get("/admin/publish/panel")
	expectStatus(t, panel, http.StatusOK)
	expectContains(t, panel.Body, `id="publish-dashboard"`, `id="publish-dashboard-reflect"`, "できませんでした")
	expectNotContains(t, panel.Body, "サイトを公開するには")
}

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 暗号化に対応していない FTP のサーバーには、「暗号化しないで送る」を選ぶまで送らない。選んだら、反映の欄に注意を出し続ける。
func TestPublishFTPWithoutEncryptionNeedsOptIn(t *testing.T) {
	env := newTestEnv(t)
	publishedArticle(t, env, "記事", "post-1")
	root := t.TempDir()
	srv := publishtest.NewFTPServer(t, root)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"server"}, "protocol": {"ftp"}, "host": {"127.0.0.1"}, "port": {strings.TrimPrefix(srv.Addr, "127.0.0.1:")},
		"user": {srv.User}, "password": {srv.Password}, "root": {"/www"}, "server_site_url": {testBaseURL}}
	refused := []string{"暗号化して送る方法に対応していません", "SFTP", "「暗号化しないで送る」"}

	// 接続テストは、ユーザー名・パスワードを送らずに分かりやすく断る。
	res := c.postForm("/admin/publish/test", form)
	var tr publishTestResult
	if err := json.Unmarshal([]byte(res.Body), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.OK {
		t.Fatalf("test without opt-in = %+v, want a refusal", tr)
	}
	expectContains(t, tr.Message, refused...)
	if cmds := srv.Commands(); slices.Contains(cmds, "USER") || slices.Contains(cmds, "PASS") {
		t.Errorf("commands = %v, want no login without encryption", cmds)
	}

	// 保存はできるが、反映は同じ理由で断り、何も送らない。
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	if env.server.loadPublishConfig().Server.AllowPlainFTP {
		t.Error("AllowPlainFTP saved as true without the checkbox")
	}
	page := c.get("/admin/publish").Body
	expectContains(t, page, `name="allow_plain_ftp"`, "暗号化なし（非推奨）")
	expectNotContains(t, page, "publish-plain-ftp-warning")
	out := c.reflectAndWait()
	if out.OK {
		t.Fatalf("reflect without opt-in = %+v, want a refusal", out)
	}
	expectContains(t, out.Message, refused...)
	if got := filesIn(t, root); len(got) != 0 {
		t.Errorf("files sent without encryption: %v", got)
	}

	// 選んだら送れる。画面には注意を出し続け、反映の結果でも知らせる。
	form.Set("allow_plain_ftp", "1")
	res = c.postForm("/admin/publish/test", form)
	if err := json.Unmarshal([]byte(res.Body), &tr); err != nil {
		t.Fatal(err)
	}
	if !tr.OK || !strings.Contains(strings.Join(tr.Details, " "), "暗号化せずに送りました") {
		t.Errorf("test with opt-in = %+v", tr)
	}
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	out = c.reflectAndWait()
	if !out.OK || !strings.Contains(strings.Join(out.Warnings, " "), "暗号化せずに送りました") {
		t.Errorf("reflect with opt-in = %+v", out)
	}
	expectContains(t, c.get("/admin/publish").Body, "publish-plain-ftp-warning", `id="publish-allow-plain-ftp" name="allow_plain_ftp" value="1" class="mt-1" checked`)

	// チェックを外して保存すれば、また送らない。
	form.Del("allow_plain_ftp")
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	expectNotContains(t, c.get("/admin/publish").Body, "publish-plain-ftp-warning")
}

// この設定が無かったころに保存した FTP の公開先は、暗号化しないで送らない。
func TestPublishSavedFTPWithoutOptInFieldStaysEncryptedOnly(t *testing.T) {
	env := newTestEnv(t)
	if err := upsertSetting(env.db, publishSettingServer,
		`{"protocol":"ftp","host":"sv1.example.jp","user":"u","password":"p","root":"/www"}`); err != nil {
		t.Fatal(err)
	}
	if env.server.loadPublishConfig().Server.AllowPlainFTP {
		t.Error("old saved FTP destination allows plaintext")
	}
}

// 「記事公開時に自動でサイトに反映する」は既定でチェックが入り、公開先と一緒に保存・読み込みできる。欄の無い送信では
// 保存済みの値を変えず、入力の誤りで表示し直すときは、送った値を残して保存しない。
func TestPublishAutoReflectSetting(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	checked := regexp.MustCompile(`<input type="checkbox" id="publish-auto-reflect"[^>]*\bchecked\b`)
	page := func() string {
		t.Helper()
		res := c.get("/admin/publish")
		expectStatus(t, res, http.StatusOK)
		return res.Body
	}
	body := page()
	expectContains(t, body, "記事公開時に自動でサイトに反映する", `<input type="hidden" name="auto_reflect_on_publish" value="0">`)
	if !checked.MatchString(body) || !env.server.autoReflectOnPublish() {
		t.Fatal("automatic reflect on publish is not on by default")
	}

	out := filepath.Join(t.TempDir(), "site")
	save := func(values ...string) {
		t.Helper()
		form := url.Values{"kind": {"folder"}, "folder_path": {out}, "folder_site_url": {testBaseURL}}
		if values != nil {
			form["auto_reflect_on_publish"] = values
		}
		expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	}
	// チェックを外すと、隠し欄の "0" だけが送られる。
	save("0")
	if got := env.server.getSetting(publishSettingAutoReflect, ""); got != "0" {
		t.Errorf("saved %q after unchecking, want \"0\"", got)
	}
	if env.server.autoReflectOnPublish() || checked.MatchString(page()) {
		t.Error("automatic reflect still on after unchecking")
	}
	// 欄の無い送信（古い画面・ほかの経路）では、保存済みの値を変えない。
	save()
	if env.server.autoReflectOnPublish() {
		t.Error("a form without the field turned the automatic reflect back on")
	}
	// チェックを入れると、"0" と "1" が送られる。
	save("0", "1")
	if got := env.server.getSetting(publishSettingAutoReflect, ""); got != "1" {
		t.Errorf("saved %q after checking, want \"1\"", got)
	}
	if !checked.MatchString(page()) {
		t.Error("checkbox not checked after saving it on")
	}

	// 入力の誤りで表示し直すときは、外して送った値を残し、保存はしない。
	res := c.postForm("/admin/publish", url.Values{"kind": {"folder"}, "folder_path": {"relative/path"}, "auto_reflect_on_publish": {"0"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if checked.MatchString(res.Body) {
		t.Error("re-shown form lost the unchecked value")
	}
	if !env.server.autoReflectOnPublish() {
		t.Error("an invalid form saved the automatic reflect setting")
	}
}

func TestPublishCDNPurgeSettingsAndTestAPIValidation(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	form := url.Values{"kind": {"cloudflare"}, "cf_account_id": {"0123456789abcdef0123456789abcdef"}, "cf_api_token": {"pages-token"}, "cf_project": {"my-media"},
		"cdn_purge_enabled": {"auto"}, "cdn_cf_zone_id": {"zone-123"}, "cdn_cf_api_token": {"token-secret"}}
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	if got := env.server.getSetting("cdn_purge_enabled", ""); got != "true" {
		t.Errorf("cdn_purge_enabled = %q", got)
	}
	if got := env.server.getSetting("cdn_cf_zone_id", ""); got != "zone-123" {
		t.Errorf("zone ID = %q", got)
	}
	if got := env.server.getSetting("cdn_cf_api_token", ""); got != "token-secret" {
		t.Errorf("token = %q", got)
	}
	page := c.get("/admin/publish").Body
	expectContains(t, page, "CDNキャッシュ自動クリア", "zone-123", "保存済み（変更するときだけ入力）")
	expectNotContains(t, page, "token-secret")
	invalid := c.postForm("/admin/api/publish/test-cdn-purge", url.Values{"cdn_cf_zone_id": {"bad zone"}})
	expectStatus(t, invalid, http.StatusOK)
	var result publishTestResult
	if err := json.Unmarshal([]byte(invalid.Body), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Fatal("invalid CDN settings unexpectedly accepted")
	}
	oldPurge := purgeCDNCache
	t.Cleanup(func() { purgeCDNCache = oldPurge })
	called := false
	purgeCDNCache = func(_ context.Context, _ *http.Client, token, zone string, files []string) error {
		called = true
		if token != "token-secret" || zone != "zone-123" || len(files) != 0 {
			t.Errorf("purge args token=%q zone=%q files=%v", token, zone, files)
		}
		return nil
	}
	success := c.postForm("/admin/api/publish/test-cdn-purge", url.Values{})
	if err := json.Unmarshal([]byte(success.Body), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !called {
		t.Fatalf("test API result = %+v, called = %v", result, called)
	}
	form.Set("cdn_cf_zone_id", "")
	expectRedirect(t, c.postForm("/admin/publish", form), "/admin/publish?flash=publish_saved")
	if got := env.server.getSetting("cdn_purge_enabled", ""); got != "false" {
		t.Errorf("CDN purge setting with empty Zone ID = %q, want false", got)
	}
	// A control character in a token is rejected by the save handler before any setting changes.
	form.Set("cdn_cf_api_token", "bad\ntoken")
	res := c.postForm("/admin/publish", form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	if got := env.server.getSetting("cdn_cf_api_token", ""); got != "token-secret" {
		t.Errorf("token after invalid save = %q", got)
	}
}

func TestPublishValidationPreservesInputsAndCDNSettings(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	for key, value := range map[string]string{"cdn_purge_enabled": "true", "cdn_cf_zone_id": "saved-zone", "cdn_cf_api_token": "saved-token"} {
		if err := upsertSetting(env.db, key, value); err != nil {
			t.Fatal(err)
		}
	}
	form := url.Values{
		"kind": {"folder"}, "folder_path": {filepath.Join(t.TempDir(), "submitted-site")}, "folder_site_url": {"not-a-url"},
		"cdn_purge_enabled": {"auto"}, "cdn_cf_zone_id": {"bad zone"},
	}
	res := c.postForm("/admin/publish", form)
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, `value="folder" checked`, form.Get("folder_path"), `value="not-a-url"`, `value="bad zone"`)
	expectNotContains(t, res.Body, "cdn_cf_api_token\" value=\"saved-token")
	if got := env.server.getSetting("cdn_purge_enabled", ""); got != "true" {
		t.Errorf("CDN enabled after invalid save = %q", got)
	}
	if got := env.server.getSetting("cdn_cf_zone_id", ""); got != "saved-zone" {
		t.Errorf("CDN zone after invalid save = %q", got)
	}
	if got := env.server.getSetting("cdn_cf_api_token", ""); got != "saved-token" {
		t.Errorf("CDN token after invalid save = %q", got)
	}

	// A partial form without the CDN fields must leave the existing settings untouched.
	partial := url.Values{"kind": {"folder"}, "folder_path": {filepath.Join(t.TempDir(), "partial-site")}, "folder_site_url": {testBaseURL}}
	expectRedirect(t, c.postForm("/admin/publish", partial), "/admin/publish?flash=publish_saved")
	if got := env.server.getSetting("cdn_purge_enabled", ""); got != "true" {
		t.Errorf("CDN enabled after partial save = %q", got)
	}
	if got := env.server.getSetting("cdn_cf_zone_id", ""); got != "saved-zone" {
		t.Errorf("CDN zone after partial save = %q", got)
	}
	if got := env.server.getSetting("cdn_cf_api_token", ""); got != "saved-token" {
		t.Errorf("CDN token after partial save = %q", got)
	}
}

func TestPublishReflectPurgesCDNAfterSuccessfulExport(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	outDir := filepath.Join(t.TempDir(), "site")
	saveFolderDestination(t, c, outDir)
	for key, value := range map[string]string{"cdn_purge_enabled": "true", "cdn_cf_zone_id": "zone-123", "cdn_cf_api_token": "token-secret"} {
		if err := upsertSetting(env.db, key, value); err != nil {
			t.Fatal(err)
		}
	}
	oldPurge := purgeCDNCache
	t.Cleanup(func() { purgeCDNCache = oldPurge })
	called := false
	purgeCDNCache = func(_ context.Context, _ *http.Client, token, zone string, files []string) error {
		called = true
		if token != "token-secret" || zone != "zone-123" || len(files) != 0 {
			t.Errorf("purge args token=%q zone=%q files=%v", token, zone, files)
		}
		return nil
	}
	result := c.reflectAndWait()
	if !result.OK || !called {
		t.Fatalf("reflect result = %+v, purge called = %v", result, called)
	}
	if !strings.Contains(strings.Join(result.Details, " "), "Cloudflare CDN キャッシュをクリアしました") {
		t.Errorf("details = %v", result.Details)
	}
}

func TestPublishReflectSkipsCDNPurgeWhenNothingChanged(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	outDir := filepath.Join(t.TempDir(), "site")
	saveFolderDestination(t, c, outDir)
	for key, value := range map[string]string{"cdn_purge_enabled": "true", "cdn_cf_zone_id": "zone-123", "cdn_cf_api_token": "token-secret"} {
		if err := upsertSetting(env.db, key, value); err != nil {
			t.Fatal(err)
		}
	}
	oldPurge := purgeCDNCache
	t.Cleanup(func() { purgeCDNCache = oldPurge })
	called := 0
	purgeCDNCache = func(_ context.Context, _ *http.Client, _, _ string, _ []string) error {
		called++
		return nil
	}
	if result := c.reflectAndWait(); !result.OK || called != 1 {
		t.Fatalf("first reflect = %+v, purge calls = %d; want successful reflect and one purge", result, called)
	}
	result := c.reflectAndWait()
	if !result.OK || result.Uploaded != 0 || result.Deleted != 0 || called != 1 {
		t.Fatalf("unchanged reflect = %+v, purge calls = %d; want no second purge", result, called)
	}
}
