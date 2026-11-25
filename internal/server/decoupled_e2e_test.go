package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// 公開サーバーのエンドツーエンド検証。New に PublicOnly・SyncPublicKey・
// 秘匿の SyncAPIPath を渡した公開サーバーを実際の HTTP で起動し、公開専用モード・秘匿パス・トークン検証・
// 提出から承認公開までの流れ・キャッシュヘッダーを確かめる。

// newPublicOnlyEnv は公開専用モードで、推測できないパス（NewSyncAPIPath）に同期 API を置いた公開サーバーを起動する。
// seed はサーバー起動前に呼ぶ（公開済みの記事など、起動時の検索インデックス同期の対象にしたいデータを入れる）。
func newPublicOnlyEnv(t *testing.T, seed func(db *sqlx.DB)) *syncEnv {
	t.Helper()
	return newSyncEnvWith(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t)}, seed)
}

// newPublicOnlyTLSEnv は newPublicOnlyEnv と同じ公開サーバーを、本番（AUTOCERT_DOMAIN）と同じく sorus が TLS を直接
// 受ける形で起動する。閲覧制限の変更（set_site_access）は TLS で直接届いた接続でしか受け付けないため、閲覧制限を
// 変更する試験で使う。PC が公開サーバーを呼ぶクライアントは試験用の証明書を信頼する（tls_env_test.go）。
func newPublicOnlyTLSEnv(t *testing.T, seed func(db *sqlx.DB)) *syncEnv {
	t.Helper()
	return startSyncEnv(t, Config{PublicOnly: true, SyncAPIPath: newSecretSyncAPIPath(t)}, seed, true)
}

// syncHTTP は authorization（空なら付けない）で、サーバーに設定した同期 API のパスに body を JSON で POST する。
func (c *client) syncHTTP(authorization string, body any) *response {
	c.t.Helper()
	return c.syncHTTPPath(c.env.server.cfg.SyncAPIPath, authorization, body)
}

// syncHTTPPath は syncHTTP と同じ要求を path に送る。
func (c *client) syncHTTPPath(path, authorization string, body any) *response {
	c.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.env.srv.URL+path, bytes.NewReader(b))
	if err != nil {
		c.t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set(echo.HeaderAuthorization, authorization)
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return c.do(req)
}

// send は method で path を送る。request と違い CSRF トークンを付けないため、管理画面の無い公開専用モードでも使える。
func (c *client) send(method, path string, header map[string]string) *response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return c.do(req)
}

func expectNotContains(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("body contains %q\nbody: %.3000s", u, body)
		}
	}
}

func decodeResponse[T any](t *testing.T, res *response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(res.Body), &v); err != nil {
		t.Fatalf("decode %T: %v\nbody: %.2000s", v, err, res.Body)
	}
	return v
}

// tamperRole は署名を変えずに payload の role を role に書き換えたトークンを返す。
func tamperRole(t *testing.T, token string, role auth.Role) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	claims["role"] = string(role)
	if payload, err = json.Marshal(claims); err != nil {
		t.Fatal(err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	return strings.Join(parts, ".")
}

// tamperSignature は署名の1ビットを反転したトークンを返す。
func tamperSignature(t *testing.T, token string) string {
	t.Helper()
	i := strings.LastIndexByte(token, '.')
	sig, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	sig[len(sig)/2] ^= 0x01
	return token[:i+1] + base64.RawURLEncoding.EncodeToString(sig)
}

// §6.1 公開専用モード: /admin 以下は存在しないページと同じ 404 になり、公開ページは通常どおり配信される。
func TestE2EPublicOnlyModeHidesAdmin(t *testing.T) {
	env := newPublicOnlyEnv(t, func(db *sqlx.DB) {
		seedArticle(t, db, article.Article{Title: "公開済みの記事", Slug: "hello", ContentMarkdown: "公開中の本文",
			CategoryID: seedCategory(t, db, "ニュース", "news", ""), Status: article.StatusPublished, PublishedAt: ago(0)})
	})
	c := env.newClient()

	unknown := c.get("/no-such-page")
	expectStatus(t, unknown, http.StatusNotFound)
	expectContains(t, unknown.Body, "ページが見つかりません")

	for _, p := range []string{
		"/admin", "/admin/", "/admin/login", "/admin/articles", "/admin/articles/new", "/admin/settings/remote",
		"/admin/remote/pending", "/admin/users", "//admin/login", "/static/../admin/login",
	} {
		t.Run("GET "+p, func(t *testing.T) {
			res := c.get(p)
			expectStatus(t, res, http.StatusNotFound)
			// 管理画面の存在を見せないよう、存在しないページと同じ読者向けの 404 ページを返す。
			expectContains(t, res.Body, "ページが見つかりません")
			expectNotContains(t, res.Body, `name="password"`, `name="_csrf"`, "/admin/login")
			if sc := res.Header.Values("Set-Cookie"); len(sc) > 0 {
				t.Errorf("Set-Cookie = %q, want none (no CSRF/session cookie in public-only mode)", sc)
			}
		})
	}
	t.Run("POST /admin/login", func(t *testing.T) {
		res := c.postRaw("/admin/login", url.Values{"username": {"admin"}, "password": {"x"}})
		expectStatus(t, res, http.StatusNotFound)
	})
	t.Run("MCP is not served", func(t *testing.T) {
		expectStatus(t, c.send(http.MethodGet, "/api/mcp", nil), http.StatusNotFound)
		expectStatus(t, c.send(http.MethodPost, "/api/mcp", nil), http.StatusNotFound)
	})

	for _, p := range []string{"/", "/articles/hello", "/feed.xml", "/sitemap.xml", "/static/icon.svg", "/healthz"} {
		res := c.get(p)
		expectStatus(t, res, http.StatusOK)
		if p == "/" || p == "/articles/hello" {
			expectContains(t, res.Body, "公開済みの記事")
		}
	}
}

// 公開専用モードでなければ、管理画面は従来どおり使え、同期 API は鍵を渡したときだけ登録される。
func TestE2ENormalModeKeepsAdmin(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	expectStatus(t, c.get("/admin/login"), http.StatusOK)
	// SyncPublicKey を渡していないため同期 API は無い。
	expectStatus(t, c.syncHTTP("Bearer x", SyncRequest{Action: SyncActionFetchPending}), http.StatusNotFound)

	pub, _, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	both := newTestEnvWith(t, Config{SyncPublicKey: pub}, nil)
	bc := both.newClient()
	expectStatus(t, bc.get("/admin/login"), http.StatusOK)
	expectStatus(t, bc.syncHTTP("", SyncRequest{Action: SyncActionFetchPending}), http.StatusUnauthorized)
}

func TestNewRejectsInvalidSyncPublicKey(t *testing.T) {
	env := newTestEnv(t)
	if _, err := New(Config{DB: env.db, UploadDir: t.TempDir(), AttachmentDir: t.TempDir(), SyncPublicKey: []byte("short")}); err == nil {
		t.Fatal("New accepted a malformed sync public key")
	}
}

// 秘匿同期エンドポイント: 公開サーバーの同期 API は推測できないパスだけで応じる。固定の /api/sync への要求は
// 正しいトークンを付けても、存在しないパスと同じ 404 で門前払いし、API の存在自体を見せない。
func TestE2EPublicOnlySecretSyncPath(t *testing.T) {
	env := newPublicOnlyEnv(t, nil)
	c := env.newClient()
	secret := env.server.cfg.SyncAPIPath
	if !strings.HasPrefix(secret, "/api/sync-") || len(secret) != len("/api/sync-")+32 {
		t.Fatalf("public server sync path = %q, want /api/sync-<32 hex digits>", secret)
	}
	editor := env.token("sato", auth.RoleEditor)
	fetch := SyncRequest{Action: SyncActionFetchPending}

	unknown := c.syncHTTPPath("/api/no-such-endpoint", "Bearer "+editor, fetch)
	expectStatus(t, unknown, http.StatusNotFound)
	for _, p := range []string{DefaultSyncAPIPath, DefaultSyncAPIPath + "/", "/api/sync-", secret[:len(secret)-1] + "x"} {
		for label, authz := range map[string]string{"valid token": "Bearer " + editor, "no token": ""} {
			res := c.syncHTTPPath(p, authz, fetch)
			expectStatus(t, res, http.StatusNotFound)
			if res.Body != unknown.Body || res.Header.Get(echo.HeaderWWWAuthenticate) != "" {
				t.Errorf("POST %s (%s) = %q (WWW-Authenticate %q), want the same 404 as an unknown path %q",
					p, label, res.Body, res.Header.Get(echo.HeaderWWWAuthenticate), unknown.Body)
			}
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		res := c.send(method, DefaultSyncAPIPath, nil)
		expectStatus(t, res, http.StatusNotFound)
		if method == http.MethodGet {
			expectContains(t, res.Body, "ページが見つかりません")
		}
	}

	// 秘匿パスでは同期できる。トークンの検証も従来どおり行う。
	expectStatus(t, c.syncHTTP("", fetch), http.StatusUnauthorized)
	res := c.syncHTTP("Bearer "+editor, fetch)
	expectStatus(t, res, http.StatusOK)
	if got := decodeResponse[SyncPendingResponse](t, res); got.Action != SyncActionFetchPending {
		t.Errorf("fetch_pending response = %+v", got)
	}
	// 秘匿パスへの GET は同期 API として応じず、公開ページのキャッシュヘッダーも付けない。
	get := c.send(http.MethodGet, secret, nil)
	if get.StatusCode == http.StatusOK || get.Header.Get("ETag") != "" || strings.Contains(get.Header.Get("Cache-Control"), "s-maxage") {
		t.Errorf("GET %s: status %d Cache-Control %q ETag %q, want no cacheable success",
			secret, get.StatusCode, get.Header.Get("Cache-Control"), get.Header.Get("ETag"))
	}
}

// §6.2・§6.3 トークン検証と、ライターの提出から編集者の承認公開までの流れ。
func TestE2EPublicOnlySyncFlow(t *testing.T) {
	env := newPublicOnlyEnv(t, nil)
	c := env.newClient()
	writer := env.token("yamada", auth.RoleWriter)
	editor := env.token("sato", auth.RoleEditor)

	t.Run("rejects missing and tampered tokens", func(t *testing.T) {
		_, otherPriv, err := auth.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		foreign, err := auth.IssueToken(otherPriv, "mallory", string(auth.RoleAdmin), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for name, authz := range map[string]string{
			"missing":            "",
			"not bearer":         "Basic " + writer,
			"role escalated":     "Bearer " + tamperRole(t, writer, auth.RoleAdmin),
			"signature modified": "Bearer " + tamperSignature(t, writer),
			"other key":          "Bearer " + foreign,
		} {
			res := c.syncHTTP(authz, SyncRequest{Action: SyncActionFetchPending})
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401\nbody: %s", name, res.StatusCode, res.Body)
			}
		}
	})

	images := localImages(t, 64, 48)
	res := c.syncHTTP("Bearer "+writer, submitRequest(SyncArticle{
		Title: "E2Eで提出した記事", Slug: "e2e-article", ContentMarkdown: "提出された本文\n\n![写真](/uploads/" + images[1].Name + ")",
	}, images...))
	expectStatus(t, res, http.StatusOK)
	submitted := decodeResponse[SyncSubmitResponse](t, res)
	if submitted.Article.Status != article.StatusReview || submitted.Article.ID == "" {
		t.Fatalf("submitted article = %+v, want review", submitted.Article)
	}
	id := submitted.Article.ID

	// 校閲待ちの記事は公開サイトに出ない。
	expectStatus(t, c.get("/articles/e2e-article"), http.StatusNotFound)
	expectNotContains(t, c.get("/").Body, "E2Eで提出した記事")

	// ライターは公開できない。
	expectStatus(t, c.syncHTTP("Bearer "+writer, publishRequest(id)), http.StatusForbidden)
	expectStatus(t, c.get("/articles/e2e-article"), http.StatusNotFound)

	res = c.syncHTTP("Bearer "+editor, SyncRequest{Action: SyncActionFetchPending})
	expectStatus(t, res, http.StatusOK)
	pending := decodeResponse[SyncPendingResponse](t, res)
	if len(pending.Articles) != 1 || pending.Articles[0].ID != id || pending.Articles[0].Title != "E2Eで提出した記事" {
		t.Fatalf("pending = %+v", pending.Articles)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("sync API Cache-Control = %q, want no-store", cc)
	}

	res = c.syncHTTP("Bearer "+editor, publishRequest(id))
	expectStatus(t, res, http.StatusOK)
	if got := decodeResponse[SyncPublishResponse](t, res).Article.Status; got != article.StatusPublished {
		t.Errorf("published status = %q", got)
	}

	page := c.get("/articles/e2e-article")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "E2Eで提出した記事", "提出された本文")
	expectContains(t, c.get("/").Body, "E2Eで提出した記事")

	// 転送された画像は同じ名前で配信され、不変キャッシュのヘッダーが付く。
	img := c.send(http.MethodGet, "/uploads/"+images[1].Name, nil)
	expectStatus(t, img, http.StatusOK)
	if cc := img.Header.Get("Cache-Control"); cc != StaticCacheControl {
		t.Errorf("uploaded image Cache-Control = %q, want %q", cc, StaticCacheControl)
	}
}

// §6.4 キャッシュヘッダー: curl -sI（HEAD）でも静的アセットに immutable、公開ページに s-maxage と ETag が付く。
func TestE2EPublicOnlyCacheHeaders(t *testing.T) {
	env := newPublicOnlyEnv(t, func(db *sqlx.DB) {
		seedArticle(t, db, article.Article{Title: "キャッシュの記事", Slug: "cached", ContentMarkdown: "本文",
			CategoryID: seedCategory(t, db, "ニュース", "news", ""), Status: article.StatusPublished, PublishedAt: ago(0)})
	})
	c := env.newClient()

	ua := map[string]string{"User-Agent": browserUA}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			st := c.send(method, "/static/icon.svg", ua)
			expectStatus(t, st, http.StatusOK)
			if cc := st.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") || cc != StaticCacheControl {
				t.Errorf("%s /static/icon.svg Cache-Control = %q, want %q", method, cc, StaticCacheControl)
			}
			for _, p := range []string{"/", "/articles/cached"} {
				res := c.send(method, p, ua)
				expectStatus(t, res, http.StatusOK)
				if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "s-maxage=3600") {
					t.Errorf("%s %s Cache-Control = %q, want s-maxage", method, p, cc)
				}
				if res.Header.Get("ETag") == "" {
					t.Errorf("%s %s: ETag missing", method, p)
				}
				if method == http.MethodHead && res.Body != "" {
					t.Errorf("HEAD %s returned a body of %d bytes", p, len(res.Body))
				}
			}
		})
	}

	first := c.send(http.MethodGet, "/", nil)
	etag := first.Header.Get("ETag")
	nm := c.send(http.MethodGet, "/", map[string]string{"If-None-Match": etag})
	expectStatus(t, nm, http.StatusNotModified)
	if nm.Body != "" {
		t.Errorf("304 body = %q", nm.Body)
	}
	// HEAD は PV に数えない（CDN・監視の確認で閲覧数が増えない）。
	env.flushAnalytics()
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily`); n != 1 {
		t.Errorf("recorded views = %d, want 1 (only the GET of /articles/cached)", n)
	}
	// 未知の静的ファイルは 404 で、永久キャッシュさせない。
	missing := c.send(http.MethodHead, "/static/no-such-file.css", nil)
	expectStatus(t, missing, http.StatusNotFound)
	if cc := missing.Header.Get("Cache-Control"); cc == StaticCacheControl {
		t.Errorf("404 static got immutable Cache-Control")
	}
}

// §6.3 をローカル CMS の管理画面から行う: ライターが「公開サーバーへ提出」し、編集者がダッシュボードの
// 校閲待ち一覧から「承認して本番公開」する。公開サーバーは公開専用モードで、秘匿パスに同期 API を置く。
func TestE2ELocalAdminToPublicOnlyServer(t *testing.T) {
	env := &remoteEnv{local: newTestEnv(t), remote: newPublicOnlyEnv(t, nil)}
	id := seedArticle(t, env.local.db, article.Article{Title: "ローカルで書いた記事", Slug: "local-post",
		ContentMarkdown: "ローカルの本文", AuthorID: studentA.ID, Status: article.StatusDraft})
	reader := env.remote.newClient()
	writer := env.local.loggedIn(studentA)
	adm := env.local.loggedIn(admin)

	// ローカルの管理者は sorus token create が案内する同期 API の完全な URL とトークンを、設定画面にそのまま貼り付ける。
	// サイトで1つの接続先のため、ライターは設定できない。
	syncURL := env.remote.srv.URL + env.remote.server.cfg.SyncAPIPath
	writerToken := env.remote.token("yamada", auth.RoleWriter)
	expectStatus(t, writer.postJSON("/admin/remote/test", url.Values{"remote_url": {syncURL}, "remote_token": {writerToken}}, nil), http.StatusForbidden)
	// 公開サーバーの URL だけでは秘匿パスに届かず、接続テストが同期 API を見つけられない。
	var test remoteTestResult
	expectStatus(t, adm.postJSON("/admin/remote/test", url.Values{"remote_url": {env.remote.srv.URL}, "remote_token": {writerToken}}, &test), http.StatusOK)
	if test.OK || test.RemoteStatus != http.StatusNotFound || !strings.Contains(test.Message, "/api/sync-") {
		t.Errorf("connection test without the sync path: %+v", test)
	}
	test = remoteTestResult{}
	expectStatus(t, adm.postJSON("/admin/remote/test", url.Values{"remote_url": {syncURL}, "remote_token": {writerToken}}, &test), http.StatusOK)
	if !test.OK || test.Role != auth.RoleWriter {
		t.Errorf("connection test with the sync URL: %+v", test)
	}
	expectRedirect(t, adm.postForm("/admin/settings/remote", url.Values{"remote_url": {syncURL}, "remote_token": {writerToken}}),
		"/admin/settings/remote?flash=settings_saved")
	if cfg := env.local.server.loadRemoteConfig(); cfg.URL != env.remote.srv.URL || cfg.SyncPath != env.remote.server.cfg.SyncAPIPath {
		t.Fatalf("saved remote config = %+v, want URL %s and the secret sync path", cfg, env.remote.srv.URL)
	}
	expectContains(t, adm.get("/admin/settings/remote").Body, `value="`+syncURL+`"`)

	edit := writer.get("/admin/articles/" + id + "/edit")
	expectStatus(t, edit, http.StatusOK)
	expectContains(t, edit.Body, `data-remote-submit="`+id+`"`, "原稿を下書きとして提出", "/static/js/remote_sync.js")

	var submitted struct {
		Created bool           `json:"created"`
		Article SyncArticleRef `json:"article"`
	}
	expectStatus(t, writer.postJSON("/admin/remote/articles/"+id+"/submit", nil, &submitted), http.StatusOK)
	if !submitted.Created || submitted.Article.ID != id || submitted.Article.Status != article.StatusReview {
		t.Fatalf("submit = %+v", submitted)
	}
	expectStatus(t, reader.get("/articles/local-post"), http.StatusNotFound)
	// ライターのトークンでは公開できない（ローカルのルートは編集者専用のため、まず権限で拒否される）。
	expectStatus(t, writer.postJSON("/admin/remote/pending/"+id+"/publish", nil, nil), http.StatusForbidden)

	env.configure("sato", auth.RoleEditor)
	editor := env.local.loggedIn(chief)
	dash := editor.get("/admin")
	expectStatus(t, dash, http.StatusOK)
	expectContains(t, dash.Body, "data-remote-pending", "校閲待ち・公開待ち（公開サーバー", `id="remote-sync-modal"`, "/static/js/remote_sync.js")

	var pending struct {
		Count    int                 `json:"count"`
		Articles []remotePendingItem `json:"articles"`
	}
	expectStatus(t, editor.getJSON("/admin/remote/pending", &pending), http.StatusOK)
	if pending.Count != 1 || pending.Articles[0].ID != id || pending.Articles[0].Title != "ローカルで書いた記事" {
		t.Fatalf("pending = %+v", pending)
	}

	var published struct {
		URL string `json:"url"`
	}
	expectStatus(t, editor.postJSON("/admin/remote/pending/"+id+"/publish", nil, &published), http.StatusOK)
	if !strings.HasSuffix(published.URL, "/articles/local-post") {
		t.Errorf("published url = %q", published.URL)
	}
	page := reader.get("/articles/local-post")
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, "ローカルで書いた記事", "ローカルの本文")
}

// 管理画面のテンプレート配線: 連携画面へのナビゲーションは管理者にだけ出し、提出ボタンと校閲待ちの一覧は
// 設定済みのトークンとローカルのロールで許される人にだけ出す。
func TestAdminTemplatesRemoteSyncWiring(t *testing.T) {
	env := newRemoteEnv(t)
	id := seedArticle(t, env.local.db, article.Article{Title: "配線確認の記事", Slug: "wiring", ContentMarkdown: "本文",
		AuthorID: studentA.ID, Status: article.StatusDraft})
	writer := env.local.loggedIn(studentA)
	editor := env.local.loggedIn(chief)
	adm := env.local.loggedIn(admin)

	for name, c := range map[string]*client{"writer": writer, "editor": editor, "admin": adm} {
		res := c.get("/admin/settings")
		expectStatus(t, res, http.StatusOK)
		if got := strings.Contains(res.Body, `href="/admin/publish"`); got != (name == "admin") {
			t.Errorf("%s: settings hub links to publishing = %v, want %v", name, got, name == "admin")
		}
	}

	// 未設定なら提出ボタン・校閲待ちの一覧・スクリプトを出さない。
	expectNotContains(t, writer.get("/admin/articles/"+id+"/edit").Body, "data-remote-submit", "remote_sync.js")
	expectNotContains(t, editor.get("/admin").Body, "data-remote-pending", "remote_sync.js")

	env.configure("yamada", auth.RoleWriter)
	expectContains(t, writer.get("/admin/articles/"+id+"/edit").Body, `data-remote-submit="`+id+`"`, "remote_sync.js")
	// 保存前の新しい記事には提出する ID が無い。
	expectNotContains(t, writer.get("/admin/articles/new").Body, "data-remote-submit")
	// writer のトークンでは校閲待ちを取得できないため、編集者にも一覧を出さない。
	expectNotContains(t, editor.get("/admin").Body, "data-remote-pending", "remote_sync.js")
	expectNotContains(t, writer.get("/admin").Body, "data-remote-pending")

	env.configure("sato", auth.RoleEditor)
	expectContains(t, editor.get("/admin").Body, "data-remote-pending", "remote_sync.js")
	// 校閲は編集者の役割のため、ライターのダッシュボードには出さない。管理者は編集者の役割も持つため出す。
	expectNotContains(t, writer.get("/admin").Body, "data-remote-pending")
	expectContains(t, adm.get("/admin").Body, "data-remote-pending", "remote_sync.js")

	// 他の人が編集中（閲覧専用）なら提出ボタンを出さない。編集画面を開くと編集ロックを獲得するため、
	// ライターのロックを解放してから編集者に開かせる。
	if res := writer.postAPI("/admin/articles/"+id+"/unlock", "", nil); res.StatusCode >= 300 {
		t.Fatalf("unlock: status %d", res.StatusCode)
	}
	expectStatus(t, editor.get("/admin/articles/"+id+"/edit"), http.StatusOK)
	readOnly := writer.get("/admin/articles/" + id + "/edit")
	expectContains(t, readOnly.Body, "readonly")
	expectNotContains(t, readOnly.Body, "data-remote-submit")
}
