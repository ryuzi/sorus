package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/image"
)

// syncEnv は同期 API を登録したテスト用サーバーと、ケーパビリティ・トークンの署名鍵。
type syncEnv struct {
	*testEnv
	priv ed25519.PrivateKey
}

// newSyncEnv は同期 API を登録したサーバーを用意する。要求はテストの goroutine で Handler に直接渡し、
// ルートの登録と処理を同じ goroutine に揃える。
func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	return registerSyncEnv(t, newTestEnv(t))
}

// newSyncEnvTLS は newSyncEnv と同じサーバーを、sorus が TLS を直接受ける形で起動する（startTestEnv）。
// 閲覧制限の変更（set_site_access）を HTTP で送る試験で使う。
func newSyncEnvTLS(t *testing.T) *syncEnv {
	t.Helper()
	return registerSyncEnv(t, startTestEnv(t, Config{}, nil, true))
}

// registerSyncEnv は env のサーバーに、新しい署名鍵の公開鍵で同期 API を登録する。
func registerSyncEnv(t *testing.T, env *testEnv) *syncEnv {
	t.Helper()
	pub, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := env.server.RegisterSyncAPI(pub); err != nil {
		t.Fatalf("RegisterSyncAPI: %v", err)
	}
	return &syncEnv{testEnv: env, priv: priv}
}

// newSyncEnvWith は cfg（PublicOnly・SyncAPIPath など）に新しい署名鍵の公開鍵を渡し、同期 API を New で登録した
// サーバーを起動する。seed はサーバー起動前に呼ぶ。
func newSyncEnvWith(t *testing.T, cfg Config, seed func(db *sqlx.DB)) *syncEnv {
	t.Helper()
	return startSyncEnv(t, cfg, seed, false)
}

// startSyncEnv は newSyncEnvWith と同じサーバーを起動する。useTLS なら sorus が TLS を直接受ける（startTestEnv）。
func startSyncEnv(t *testing.T, cfg Config, seed func(db *sqlx.DB), useTLS bool) *syncEnv {
	t.Helper()
	pub, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	cfg.SyncPublicKey = pub
	return &syncEnv{testEnv: startTestEnv(t, cfg, seed, useTLS), priv: priv}
}

// newSecretSyncAPIPath は NewSyncAPIPath で推測できない同期 API のパスを生成する。
func newSecretSyncAPIPath(t *testing.T) string {
	t.Helper()
	p, err := NewSyncAPIPath()
	if err != nil {
		t.Fatalf("NewSyncAPIPath: %v", err)
	}
	return p
}

// token は sub・role の有効なトークンを発行する。
func (e *syncEnv) token(sub string, role auth.Role) string {
	e.t.Helper()
	tok, err := auth.IssueToken(e.priv, sub, string(role), time.Hour)
	if err != nil {
		e.t.Fatalf("IssueToken: %v", err)
	}
	return tok
}

// call は body を JSON にして token で同期 API に送る。
func (e *syncEnv) call(token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal: %v", err)
	}
	return e.callRaw("Bearer "+token, echo.MIMEApplicationJSON, b)
}

// callRaw は Authorization・Content-Type・ボディをそのまま指定して同期 API（サーバーに設定したパス）に送る。
// 空のヘッダーは付けない。
func (e *syncEnv) callRaw(authorization, contentType string, body []byte) *httptest.ResponseRecorder {
	return e.callPath(e.server.cfg.SyncAPIPath, authorization, contentType, body)
}

// callPath は callRaw と同じ要求を path に送る（設定していないパスが同期 API として応じないことの確認用）。
func (e *syncEnv) callPath(path, authorization, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if authorization != "" {
		req.Header.Set(echo.HeaderAuthorization, authorization)
	}
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	return e.serve(req)
}

// get は path を GET する（公開ページ・アップロード画像の確認用）。
func (e *syncEnv) get(path string) *httptest.ResponseRecorder {
	return e.serve(httptest.NewRequest(http.MethodGet, path, nil))
}

func (e *syncEnv) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	return rec
}

func expectSyncStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d\nbody: %.2000s", rec.Code, want, rec.Body.String())
	}
}

func decodeSync[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v\nbody: %.2000s", v, err, rec.Body.String())
	}
	return v
}

// syncErrorMessage は Echo のエラー応答 {"message": ...} の本文を返す。
func syncErrorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decodeSync[struct {
		Message string `json:"message"`
	}](t, rec).Message
}

func submitRequest(a SyncArticle, images ...SyncImage) SyncRequest {
	return SyncRequest{Action: SyncActionSubmitDraft, Article: &a, Images: images}
}

func publishRequest(id string) SyncRequest {
	return SyncRequest{Action: SyncActionPublishArticle, ArticleID: id}
}

func strPtr(s string) *string { return &s }

func TestSyncUnpublishArticle(t *testing.T) {
	e := newSyncEnv(t)
	id := seedArticle(t, e.db, article.Article{Title: "取り下げ", Slug: "unpublish-sync", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	request := SyncRequest{Action: SyncActionUnpublishArticle, ArticleID: id}
	expectSyncStatus(t, e.call(e.token("writer", auth.RoleWriter), request), http.StatusForbidden)
	for _, role := range []auth.Role{auth.RoleEditor, auth.RoleAdmin} {
		rec := e.call(e.token(string(role), role), request)
		expectSyncStatus(t, rec, http.StatusOK)
		out := decodeSync[SyncPublishResponse](t, rec)
		if out.Action != SyncActionUnpublishArticle || out.Article.Status != article.StatusDraft {
			t.Fatalf("response = %+v", out)
		}
		a := e.mustGetArticle(id)
		if a.Status != article.StatusDraft || a.PublishedAt == nil {
			t.Fatalf("unpublished article = %+v", a)
		}
	}
	// Draft repeat is idempotent.
	expectSyncStatus(t, e.call(e.token("editor2", auth.RoleEditor), request), http.StatusOK)
	for _, id := range []string{"", "bad/id"} {
		req := request
		req.ArticleID = id
		expectSyncStatus(t, e.call(e.token("editor3", auth.RoleEditor), req), http.StatusBadRequest)
	}
	expectSyncStatus(t, e.call(e.token("editor4", auth.RoleEditor), SyncRequest{Action: SyncActionUnpublishArticle, ArticleID: "missing-id"}), http.StatusNotFound)
	expectSyncStatus(t, e.get("/articles/unpublish-sync"), http.StatusNotFound)

	t.Run("locked by another user", func(t *testing.T) {
		lockedID := seedArticle(t, e.db, article.Article{Title: "編集中の公開記事", Slug: "unpublish-locked", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
		if _, err := e.server.locker.Acquire(context.Background(), lockedID, studentB.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.server.locker.Release(context.Background(), lockedID, studentB.ID) })
		rec := e.call(e.token("editor5", auth.RoleEditor), SyncRequest{Action: SyncActionUnpublishArticle, ArticleID: lockedID})
		expectSyncStatus(t, rec, http.StatusConflict)
		if st := e.mustGetArticle(lockedID).Status; st != article.StatusPublished {
			t.Errorf("locked article status = %s, want published", st)
		}
		expectSyncStatus(t, e.get("/articles/unpublish-locked"), http.StatusOK)
	})
}

func TestSyncUnpublishClearsScheduledPublishedAt(t *testing.T) {
	e := newSyncEnv(t)
	future := time.Now().UTC().Add(time.Hour)
	id := seedArticle(t, e.db, article.Article{Title: "予約取り下げ", Slug: "unpublish-scheduled", Status: article.StatusPublished, PublishedAt: &future})
	expectSyncStatus(t, e.call(e.token("editor", auth.RoleEditor), SyncRequest{Action: SyncActionUnpublishArticle, ArticleID: id}), http.StatusOK)
	if a := e.mustGetArticle(id); a.Status != article.StatusDraft || a.PublishedAt != nil {
		t.Fatalf("scheduled article after unpublish = %+v", a)
	}
}

// localImages はローカル CMS と同じく image.Process で PNG を WebP に変換し、転送する画像を返す。
func localImages(t *testing.T, w, h int) []SyncImage {
	t.Helper()
	dir := t.TempDir()
	img, err := image.Process(bytes.NewReader(pngBytes(t, w, h)), "photo.png", image.ProcessOptions{OutputDir: dir})
	if err != nil {
		t.Fatalf("image.Process: %v", err)
	}
	var images []SyncImage
	for _, p := range []string{img.OriginalPath, img.LargePath, img.MediumPath, img.SmallPath} {
		data, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			t.Fatal(err)
		}
		images = append(images, SyncImage{Name: filepath.Base(p), Data: data})
	}
	return images
}

// signedToken は任意の header・payload に正しい署名を付けたトークンを作る（期限切れのトークンの再現用）。
func signedToken(priv ed25519.PrivateKey, header, payload string) string {
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString([]byte(payload))
	return signingInput + "." + enc.EncodeToString(ed25519.Sign(priv, []byte(signingInput)))
}

// seedReview は校閲待ちの記事を直接保存し、ID を返す。
func (e *syncEnv) seedReview(title, slug string) string {
	e.t.Helper()
	return seedArticle(e.t, e.db, article.Article{Title: title, Slug: slug, ContentMarkdown: title + "の本文", Status: article.StatusReview})
}

func (e *syncEnv) setting(key string) string {
	e.t.Helper()
	var v string
	if err := e.db.Get(&v, `SELECT value FROM settings WHERE key = ?`, key); err != nil {
		e.t.Fatalf("setting %s: %v", key, err)
	}
	return v
}

func (e *syncEnv) uploadedFiles() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.uploadDir)
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, ent := range entries {
		names = append(names, ent.Name())
	}
	return names
}

func TestNewSyncAPIPath(t *testing.T) {
	pattern := regexp.MustCompile(`^/api/sync-[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for range 20 {
		p := newSecretSyncAPIPath(t)
		if !pattern.MatchString(p) {
			t.Fatalf("NewSyncAPIPath() = %q, want /api/sync-<32 hex digits>", p)
		}
		if seen[p] {
			t.Fatalf("NewSyncAPIPath() returned %q twice", p)
		}
		seen[p] = true
		if got, err := NormalizeSyncAPIPath(p); err != nil || got != p {
			t.Errorf("NormalizeSyncAPIPath(%q) = %q, %v; want it unchanged", p, got, err)
		}
	}
}

func TestNormalizeSyncAPIPath(t *testing.T) {
	for in, want := range map[string]string{
		"/api/sync":                            "/api/sync",
		"/api/sync-0123456789abcdef":           "/api/sync-0123456789abcdef",
		"  api/sync-abc/  ":                    "/api/sync-abc",
		"/hidden/Sync_Endpoint.v2~x":           "/hidden/Sync_Endpoint.v2~x",
		"/api/sync-abc//":                      "/api/sync-abc",
		"/api/mcpx":                            "/api/mcpx",
		"/administration-sync":                 "/administration-sync",
		"/api/sync-" + strings.Repeat("a", 32): "/api/sync-" + strings.Repeat("a", 32),
	} {
		if got, err := NormalizeSyncAPIPath(in); err != nil || got != want {
			t.Errorf("NormalizeSyncAPIPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", " ", "/", "//", "/api//sync", "/api/./sync", "/api/../sync", "/api/sync/..", "/api/:id", "/api/sync*",
		"/api/sync?x=1", "/api/sync#x", "/api/sync x", "/api/同期", "https://example.com/api/sync", "/api/sync%2F",
		"/admin", "/admin/sync", "/static/sync", "/uploads/sync", "/attachments/sync", "/og-image/sync", "/api/mcp",
		"/api/mcp/sync", "/" + strings.Repeat("a", maxSyncAPIPathLength),
	} {
		got, err := NormalizeSyncAPIPath(in)
		if err == nil {
			t.Errorf("NormalizeSyncAPIPath(%q) = %q, want error", in, got)
			continue
		}
		if !errors.Is(err, ErrInvalidSyncAPIPath) {
			t.Errorf("NormalizeSyncAPIPath(%q) err = %v, want ErrInvalidSyncAPIPath", in, err)
		}
	}
}

func TestNewValidatesSyncAPIPath(t *testing.T) {
	env := newTestEnv(t)
	newServer := func(p string) (*Server, error) {
		pub, _, err := auth.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		return New(Config{DB: env.db, UploadDir: t.TempDir(), AttachmentDir: t.TempDir(), DisableScheduler: true,
			SyncPublicKey: pub, SyncAPIPath: p})
	}
	for in, want := range map[string]string{"": DefaultSyncAPIPath, " api/sync-abc/ ": "/api/sync-abc"} {
		srv, err := newServer(in)
		if err != nil {
			t.Fatalf("New(SyncAPIPath=%q): %v", in, err)
		}
		if srv.cfg.SyncAPIPath != want {
			t.Errorf("New(SyncAPIPath=%q) path = %q, want %q", in, srv.cfg.SyncAPIPath, want)
		}
		srv.Shutdown(context.Background())
	}
	for _, in := range []string{"/admin/sync", "/api/:id", "/api/../sync"} {
		if _, err := newServer(in); !errors.Is(err, ErrInvalidSyncAPIPath) {
			t.Errorf("New(SyncAPIPath=%q) err = %v, want ErrInvalidSyncAPIPath", in, err)
		}
	}
}

// 秘匿パスを設定すると、同期 API はそのパスだけで応じ、固定の /api/sync は存在しないパスと同じ 404 になる。
// 正しいトークンを付けても固定のパスでは受け付けず、トークンが無くても 401 ではなく 404 を返して API の存在を悟らせない。
func TestSyncAPISecretPath(t *testing.T) {
	for name, publicOnly := range map[string]bool{"normal": false, "public-only": true} {
		t.Run(name, func(t *testing.T) {
			secret := newSecretSyncAPIPath(t)
			env := newSyncEnvWith(t, Config{PublicOnly: publicOnly, SyncAPIPath: secret}, nil)
			editor := env.token("sato", auth.RoleEditor)
			body, err := json.Marshal(SyncRequest{Action: SyncActionFetchPending})
			if err != nil {
				t.Fatal(err)
			}

			ok := env.call(editor, SyncRequest{Action: SyncActionFetchPending})
			expectSyncStatus(t, ok, http.StatusOK)
			if cc := ok.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("secret path Cache-Control = %q, want no-store", cc)
			}
			expectSyncStatus(t, env.callRaw("", echo.MIMEApplicationJSON, body), http.StatusUnauthorized)

			unknown := env.callPath("/no-such-api", "", echo.MIMEApplicationJSON, body)
			for _, p := range []string{DefaultSyncAPIPath, DefaultSyncAPIPath + "/", secret + "/", secret[:len(secret)-1], strings.ToUpper(secret)} {
				for label, authz := range map[string]string{"valid token": "Bearer " + editor, "no token": ""} {
					rec := env.callPath(p, authz, echo.MIMEApplicationJSON, body)
					expectSyncStatus(t, rec, http.StatusNotFound)
					if rec.Body.String() != unknown.Body.String() {
						t.Errorf("POST %s (%s) body = %q, want the same as an unknown path %q", p, label, rec.Body.String(), unknown.Body.String())
					}
					if h := rec.Header().Get(echo.HeaderWWWAuthenticate); h != "" {
						t.Errorf("POST %s (%s) WWW-Authenticate = %q, want none", p, label, h)
					}
				}
			}
			// 読者向けの GET も、存在しないページと同じ 404 ページになる。
			page := env.get(DefaultSyncAPIPath)
			expectSyncStatus(t, page, http.StatusNotFound)
			if !strings.Contains(page.Body.String(), "ページが見つかりません") {
				t.Errorf("GET %s body is not the reader 404 page: %.500s", DefaultSyncAPIPath, page.Body.String())
			}
		})
	}
}

// 同期 API のパスは公開ページ用のキャッシュ（Cache-Control: s-maxage・ETag）の対象にしない。
// /api の外に置いた秘匿パスでも、CDN に応答を残させない。
func TestSyncAPIPathIsNotCached(t *testing.T) {
	const secret = "/hidden-sync-0123456789abcdef"
	for _, p := range []string{secret, secret + "/x", DefaultSyncAPIPath, "/api/sync-0123"} {
		if isPublicHTMLCachePath(p, secret) {
			t.Errorf("isPublicHTMLCachePath(%q, %q) = true, want false", p, secret)
		}
	}
	for _, p := range []string{"/", "/articles/a", secret + "x", "/hidden-sync"} {
		if !isPublicHTMLCachePath(p, secret) {
			t.Errorf("isPublicHTMLCachePath(%q, %q) = false, want true", p, secret)
		}
	}
	if !isPublicHTMLCachePath(secret) || !isPublicHTMLCachePath(secret, "") {
		t.Errorf("isPublicHTMLCachePath(%q) without exclusion = false, want true", secret)
	}

	e := newCacheTestEcho(PublicHTMLCacheMiddleware(secret))
	e.GET(secret, func(c echo.Context) error { return c.HTML(http.StatusOK, "sync") })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, secret, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "" || rec.Header().Get("ETag") != "" {
		t.Errorf("GET %s: status %d Cache-Control %q ETag %q, want 200 without cache headers",
			secret, rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("ETag"))
	}
}

func TestSyncAPIRegisterRejectsInvalidKey(t *testing.T) {
	env := newTestEnv(t)
	if err := env.server.RegisterSyncAPI(ed25519.PublicKey("short")); err == nil {
		t.Fatal("RegisterSyncAPI accepted a malformed public key")
	}
}

func TestSyncAPIAuthentication(t *testing.T) {
	env := newSyncEnv(t)
	_, otherPriv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := auth.IssueToken(otherPriv, "intruder", "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	valid := env.token("writer-1", auth.RoleWriter)
	parts := strings.Split(valid, ".")
	// 署名はそのままに payload だけを admin へ書き換えたトークン。
	escalated := parts[0] + "." + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"sub":"writer-1","role":"admin","exp":4102444800,"iat":1700000000}`)) + "." + parts[2]
	now := time.Now().Unix()
	expired := signedToken(env.priv, `{"alg":"EdDSA","typ":"sorus-cap"}`,
		fmt.Sprintf(`{"sub":"writer-1","role":"writer","exp":%d,"iat":%d}`, now-60, now-3600))

	body, err := json.Marshal(submitRequest(SyncArticle{Title: "認証されない提出", ContentMarkdown: "本文"}, localImages(t, 32, 24)...))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		authorization string
		wantChallenge string
		wantMessage   string
	}{
		{"missing header", "", "Bearer", "Bearer トークン"},
		{"basic scheme", "Basic d3JpdGVyOnB3", `Bearer error="invalid_request"`, "Bearer トークン"},
		{"empty bearer", "Bearer ", `Bearer error="invalid_request"`, "Bearer トークン"},
		{"garbage token", "Bearer not-a-token", `Bearer error="invalid_token"`, "無効"},
		{"signed by another key", "Bearer " + foreign, `Bearer error="invalid_token"`, "無効"},
		{"tampered role", "Bearer " + escalated, `Bearer error="invalid_token"`, "無効"},
		{"expired", "Bearer " + expired, `Bearer error="invalid_token"`, "有効期限"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := env.callRaw(tt.authorization, echo.MIMEApplicationJSON, body)
			expectSyncStatus(t, rec, http.StatusUnauthorized)
			if got := rec.Header().Get(echo.HeaderWWWAuthenticate); got != tt.wantChallenge {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tt.wantChallenge)
			}
			if msg := syncErrorMessage(t, rec); !strings.Contains(msg, tt.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", msg, tt.wantMessage)
			}
		})
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles`); n != 0 {
		t.Errorf("rejected requests saved %d articles", n)
	}
	if files := env.uploadedFiles(); len(files) != 0 {
		t.Errorf("rejected requests saved images: %v", files)
	}

	// 同じボディでも、正しいトークンなら受け付ける（上の拒否がボディのせいではないことの確認）。
	expectSyncStatus(t, env.callRaw("bearer "+valid, echo.MIMEApplicationJSON, body), http.StatusOK)
}

func TestSyncAPIPermissions(t *testing.T) {
	env := newSyncEnv(t)
	allowed := map[auth.Role]map[string]bool{
		auth.RoleWriter: {SyncActionSubmitDraft: true},
		auth.RoleEditor: {SyncActionSubmitDraft: true, SyncActionFetchPending: true, SyncActionPublishArticle: true},
		auth.RoleAdmin: {SyncActionSubmitDraft: true, SyncActionFetchPending: true, SyncActionPublishArticle: true,
			SyncActionSyncSettings: true},
	}
	actions := []string{SyncActionSubmitDraft, SyncActionFetchPending, SyncActionPublishArticle, SyncActionSyncSettings}

	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleEditor, auth.RoleAdmin} {
		for _, action := range actions {
			t.Run(string(role)+"/"+action, func(t *testing.T) {
				reviewID := env.seedReview("権限確認 "+string(role)+" "+action, "")
				siteName := env.server.cfg.SiteName
				var req SyncRequest
				switch action {
				case SyncActionSubmitDraft:
					req = submitRequest(SyncArticle{Title: "提出 " + string(role), ContentMarkdown: "本文"})
				case SyncActionFetchPending:
					req = SyncRequest{Action: action}
				case SyncActionPublishArticle:
					req = publishRequest(reviewID)
				case SyncActionSyncSettings:
					req = SyncRequest{Action: action, Settings: &SyncSettings{SiteName: strPtr("変更後 " + string(role))}}
				}
				rec := env.call(env.token("member-"+string(role), role), req)

				if allowed[role][action] {
					expectSyncStatus(t, rec, http.StatusOK)
					return
				}
				expectSyncStatus(t, rec, http.StatusForbidden)
				if msg := syncErrorMessage(t, rec); !strings.Contains(msg, action) {
					t.Errorf("message = %q, want it to name the action", msg)
				}
				// 拒否した操作は何も変更しない。
				if st := env.mustGetArticle(reviewID).Status; st != article.StatusReview {
					t.Errorf("forbidden %s changed article status to %s", action, st)
				}
				if env.server.cfg.SiteName != siteName {
					t.Errorf("forbidden %s changed site name to %q", action, env.server.cfg.SiteName)
				}
			})
		}
	}
}

func TestSyncAPIRequestValidation(t *testing.T) {
	env := newSyncEnv(t)
	admin := env.token("chief-admin", auth.RoleAdmin)
	draftID := seedArticle(t, env.db, article.Article{Title: "下書き", Status: article.StatusDraft})

	t.Run("content type", func(t *testing.T) {
		rec := env.callRaw("Bearer "+admin, "text/plain", []byte(`{"action":"fetch_pending"}`))
		expectSyncStatus(t, rec, http.StatusUnsupportedMediaType)
		// charset 付きの application/json は受け付ける。
		rec = env.callRaw("Bearer "+admin, "application/json; charset=utf-8", []byte(`{"action":"fetch_pending"}`))
		expectSyncStatus(t, rec, http.StatusOK)
	})

	raw := []struct {
		name string
		body string
		want int
	}{
		{"empty body", ``, http.StatusBadRequest},
		{"invalid json", `{"action":`, http.StatusBadRequest},
		{"trailing data", `{"action":"fetch_pending"} {}`, http.StatusBadRequest},
		{"invalid base64 image", `{"action":"submit_draft","article":{"title":"t"},"images":[{"name":"a","data":"%%%"}]}`, http.StatusBadRequest},
	}
	for _, tt := range raw {
		t.Run(tt.name, func(t *testing.T) {
			expectSyncStatus(t, env.callRaw("Bearer "+admin, echo.MIMEApplicationJSON, []byte(tt.body)), tt.want)
		})
	}

	tests := []struct {
		name string
		req  SyncRequest
		want int
	}{
		{"missing action", SyncRequest{}, http.StatusBadRequest},
		{"unknown action", SyncRequest{Action: "delete_everything"}, http.StatusBadRequest},
		{"submit without article", SyncRequest{Action: SyncActionSubmitDraft}, http.StatusBadRequest},
		{"submit with blank title", submitRequest(SyncArticle{Title: "  ", ContentMarkdown: "本文"}), http.StatusUnprocessableEntity},
		{"submit with malformed id", submitRequest(SyncArticle{ID: "../../etc", Title: "t"}), http.StatusBadRequest},
		{"publish without id", SyncRequest{Action: SyncActionPublishArticle}, http.StatusBadRequest},
		{"publish malformed id", publishRequest("a b"), http.StatusBadRequest},
		{"publish unknown id", publishRequest("0123456789abcdef0123456789abcdef"), http.StatusNotFound},
		{"publish draft", publishRequest(draftID), http.StatusConflict},
		{"settings missing", SyncRequest{Action: SyncActionSyncSettings}, http.StatusBadRequest},
		{"settings empty", SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{}}, http.StatusBadRequest},
		{"settings base url scheme", SyncRequest{Action: SyncActionSyncSettings,
			Settings: &SyncSettings{BaseURL: strPtr("javascript:alert(1)")}}, http.StatusUnprocessableEntity},
		{"settings base url query", SyncRequest{Action: SyncActionSyncSettings,
			Settings: &SyncSettings{BaseURL: strPtr("https://example.com/?a=b")}}, http.StatusUnprocessableEntity},
		{"settings unknown theme", SyncRequest{Action: SyncActionSyncSettings,
			Settings: &SyncSettings{Theme: strPtr("no-such-theme")}}, http.StatusUnprocessableEntity},
		{"settings too long", SyncRequest{Action: SyncActionSyncSettings,
			Settings: &SyncSettings{SiteName: strPtr(strings.Repeat("長", maxSettingRunes+1))}}, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectSyncStatus(t, env.call(admin, tt.req), tt.want)
		})
	}
	if st := env.mustGetArticle(draftID).Status; st != article.StatusDraft {
		t.Errorf("draft status = %s after rejected publish", st)
	}
}

func TestSyncAPISubmitDraft(t *testing.T) {
	env := newSyncEnv(t)
	catID := seedCategory(t, env.db, "研究", "research", "")
	writer := env.token("tanaka", auth.RoleWriter)
	const id = "0123456789abcdef0123456789abcdef"

	rec := env.call(writer, submitRequest(SyncArticle{
		ID: id, Title: "  写真部の秋季展示会 ", Slug: "photo-exhibition", Category: "research",
		ContentMarkdown: "# 展示会\r\n\r\n体育館で秋季展示会が開かれた。", Excerpt: " 写真部の展示 ",
	}))
	expectSyncStatus(t, rec, http.StatusOK)
	if cc := rec.Header().Get(echo.HeaderCacheControl); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	res := decodeSync[SyncSubmitResponse](t, rec)
	if res.Action != SyncActionSubmitDraft || !res.Created {
		t.Errorf("response = %+v, want created submit_draft", res)
	}
	want := SyncArticleRef{ID: id, Slug: "photo-exhibition", Status: article.StatusReview, Category: "research",
		URL: "/articles/photo-exhibition"}
	if got := res.Article; got.ID != want.ID || got.Slug != want.Slug || got.Status != want.Status ||
		got.Category != want.Category || got.URL != want.URL || got.PublishedAt != nil {
		t.Errorf("article = %+v, want %+v", got, want)
	}
	if res.Images == nil || len(res.Images) != 0 {
		t.Errorf("images = %#v, want empty list", res.Images)
	}

	a := env.mustGetArticle(id)
	if a.Status != article.StatusReview || a.Title != "写真部の秋季展示会" || a.Excerpt != "写真部の展示" ||
		a.CategoryID != catID || strings.Contains(a.ContentMarkdown, "\r") || !strings.Contains(a.ContentHTML, "<h1") {
		t.Errorf("saved article = %+v", a)
	}

	// トークンの sub に対応するユーザーがいなければ、ログインできない同期用のユーザーを作成する。
	var author auth.User
	if err := env.db.Get(&author, `SELECT id, username, display_name, password_hash, role, created_at FROM users WHERE id = ?`,
		a.AuthorID); err != nil {
		t.Fatalf("author: %v", err)
	}
	if !strings.HasPrefix(author.ID, syncUserIDPrefix) || author.DisplayName != "tanaka" || author.Role != auth.RoleWriter {
		t.Errorf("author = %+v", author)
	}
	if auth.CheckPassword("", author.PasswordHash) || auth.CheckPassword("tanaka", author.PasswordHash) {
		t.Error("sync user must not be able to log in")
	}

	// 校閲待ちの記事は公開ページに出さない。
	expectSyncStatus(t, env.get("/articles/photo-exhibition"), http.StatusNotFound)

	t.Run("resubmit updates own article and keeps slug", func(t *testing.T) {
		rec := env.call(writer, submitRequest(SyncArticle{ID: id, Title: "写真部の秋季展示会（改訂）", ContentMarkdown: "改訂した本文"}))
		expectSyncStatus(t, rec, http.StatusOK)
		res := decodeSync[SyncSubmitResponse](t, rec)
		if res.Created || res.Article.Slug != "photo-exhibition" || res.Article.Category != "research" {
			t.Errorf("response = %+v, want update keeping slug and category", res)
		}
		a := env.mustGetArticle(id)
		if a.Title != "写真部の秋季展示会（改訂）" || a.ContentMarkdown != "改訂した本文" || a.AuthorID != author.ID {
			t.Errorf("updated article = %+v", a)
		}
		// 更新の間だけ獲得した編集ロックは、保存後に解放する。
		if a.LockedBy != nil {
			t.Errorf("lock left after resubmission: locked_by = %q", *a.LockedBy)
		}
		if n := env.countRows(`SELECT COUNT(*) FROM users WHERE id LIKE 'sync-%'`); n != 1 {
			t.Errorf("sync users = %d, want 1 (reused)", n)
		}
		if n := env.countRows(`SELECT COUNT(*) FROM article_revisions WHERE article_id = ?`, id); n != 2 {
			t.Errorf("revisions = %d, want 2", n)
		}
	})

	// 執筆チーム内では非公開の記事を共有するため、他の執筆者も下書き・校閲待ちの記事を更新できる。執筆者は変えない。
	t.Run("another writer can update", func(t *testing.T) {
		rec := env.call(env.token("someone-else", auth.RoleWriter), submitRequest(SyncArticle{ID: id, Title: "共同で直した見出し", ContentMarkdown: "本文"}))
		expectSyncStatus(t, rec, http.StatusOK)
		if a := env.mustGetArticle(id); a.Title != "共同で直した見出し" || a.AuthorID != author.ID {
			t.Errorf("article after another writer's update = %+v, want title changed and author kept", a)
		}
	})

	t.Run("editor can update", func(t *testing.T) {
		rec := env.call(env.token("desk", auth.RoleEditor), submitRequest(SyncArticle{ID: id, Title: "編集者が整えた見出し", ContentMarkdown: "本文"}))
		expectSyncStatus(t, rec, http.StatusOK)
		if a := env.mustGetArticle(id); a.Title != "編集者が整えた見出し" || a.AuthorID != author.ID {
			t.Errorf("article after editor update = %+v, want title changed and author kept", a)
		}
	})

	t.Run("locked by another user", func(t *testing.T) {
		if _, err := env.server.locker.Acquire(context.Background(), id, chief.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = env.server.locker.Release(context.Background(), id, chief.ID) })
		rec := env.call(writer, submitRequest(SyncArticle{ID: id, Title: "編集中に提出"}))
		expectSyncStatus(t, rec, http.StatusConflict)
		if !strings.Contains(syncErrorMessage(t, rec), chief.DisplayName) {
			t.Errorf("message does not name the lock holder: %s", rec.Body.String())
		}
	})

	t.Run("published article cannot be resubmitted", func(t *testing.T) {
		pubID := seedArticle(t, env.db, article.Article{Title: "公開済み", Slug: "already-live", Status: article.StatusPublished,
			AuthorID: author.ID})
		rec := env.call(writer, submitRequest(SyncArticle{ID: pubID, Title: "公開済みを差し替え"}))
		expectSyncStatus(t, rec, http.StatusConflict)
		if a := env.mustGetArticle(pubID); a.Status != article.StatusPublished || a.Title != "公開済み" {
			t.Errorf("published article changed: %+v", a)
		}
		expectSyncStatus(t, env.get("/articles/already-live"), http.StatusOK)
	})

	t.Run("token isolation prevents impersonating existing users", func(t *testing.T) {
		// 既存ユーザーと同じ sub であっても、jti ベースの独立した同期ユーザーになりすましを防ぐ
		for sub, existingID := range map[string]string{studentA.ID: studentA.ID, studentB.Username: studentB.ID} {
			rec := env.call(env.token(sub, auth.RoleWriter), submitRequest(SyncArticle{Title: "提出 " + sub, Category: "no-such-category"}))
			expectSyncStatus(t, rec, http.StatusOK)
			res := decodeSync[SyncSubmitResponse](t, rec)
			a := env.mustGetArticle(res.Article.ID)
			if a.AuthorID == existingID {
				t.Errorf("sub %q must not impersonate existing user %q", sub, existingID)
			}
			if !strings.HasPrefix(a.AuthorID, syncUserIDPrefix+"tok-") {
				t.Errorf("author ID = %q, want sync-tok- prefix", a.AuthorID)
			}
			// 公開サーバーに無いカテゴリは既定のカテゴリにする。
			if a.CategoryID != 1 || res.Article.Category != "uncategorized" {
				t.Errorf("sub %q: category = %d (%q), want default", sub, a.CategoryID, res.Article.Category)
			}
			if len(res.Article.ID) != 32 {
				t.Errorf("generated id = %q", res.Article.ID)
			}
		}
	})
}

// 複数の執筆者が同じ新しい記事 ID で同時に提出しても、作成できるのは最初の1件だけで、
// 後から来た提出が存在確認と保存の間に作成された記事を作成で上書きしない（作成済みの記事への提出は更新になる）。
func TestSyncAPISubmitDraftConcurrentSameNewID(t *testing.T) {
	env := newSyncEnv(t)
	const id = "fedcba9876543210fedcba9876543210"
	const writers = 8
	tokens := make([]string, writers)
	for i := range tokens {
		tokens[i] = env.token(fmt.Sprintf("writer-%d", i), auth.RoleWriter)
	}
	// 全員が同じ画像を添える。作成に負けた提出が、勝った提出の参照する画像を削除しないことも確かめる。
	images := localImages(t, 1300, 700)

	recs := make([]*httptest.ResponseRecorder, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = env.call(tokens[i], submitRequest(SyncArticle{ID: id, Title: fmt.Sprintf("同時提出 %d", i)}, images...))
		}(i)
	}
	close(start)
	wg.Wait()

	winner := -1
	saved := map[string]bool{}
	for i, rec := range recs {
		switch rec.Code {
		case http.StatusOK:
			saved[fmt.Sprintf("同時提出 %d", i)] = true
			if res := decodeSync[SyncSubmitResponse](t, rec); res.Created {
				if winner >= 0 {
					t.Errorf("writers %d and %d both created article %s", winner, i, id)
				}
				winner = i
			}
		case http.StatusConflict:
		default:
			t.Errorf("writer %d: status %d, want 200 or 409\nbody: %s", i, rec.Code, rec.Body.String())
		}
	}
	if winner < 0 {
		t.Fatal("no submission created the article")
	}
	a := env.mustGetArticle(id)
	if !saved[a.Title] {
		t.Errorf("title = %q, want one of the saved submissions %v", a.Title, saved)
	}
	var author string
	if err := env.db.Get(&author, `SELECT display_name FROM users WHERE id = ?`, a.AuthorID); err != nil || author != fmt.Sprintf("writer-%d", winner) {
		t.Errorf("author = %q (%v), want writer-%d", author, err, winner)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM article_revisions WHERE article_id = ?`, id); n != len(saved) {
		t.Errorf("revisions = %d, want %d (one per saved submission)", n, len(saved))
	}
	for _, img := range images {
		if got, err := os.ReadFile(filepath.Join(env.uploadDir, img.Name)); err != nil || !bytes.Equal(got, img.Data) {
			t.Errorf("image %s of the saved submission: %v", img.Name, err)
		}
	}
}

func TestSyncAPISubmitDraftImages(t *testing.T) {
	env := newSyncEnv(t)
	writer := env.token("tanaka", auth.RoleWriter)
	images := localImages(t, 64, 48)
	large := images[1].Name
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := submitRequest(SyncArticle{ID: id, Title: "画像付きの記事", ContentMarkdown: "![写真](/uploads/" + large + ")"}, images...)

	rec := env.call(writer, req)
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncSubmitResponse](t, rec)
	if len(res.Images) != len(images) {
		t.Fatalf("images = %v, want %d", res.Images, len(images))
	}
	for i, img := range images {
		if res.Images[i] != UploadURLPrefix+img.Name {
			t.Errorf("images[%d] = %q, want %q", i, res.Images[i], UploadURLPrefix+img.Name)
		}
		got, err := os.ReadFile(filepath.Join(env.uploadDir, img.Name))
		if err != nil {
			t.Fatalf("saved image %s: %v", img.Name, err)
		}
		if !bytes.Equal(got, img.Data) {
			t.Errorf("saved image %s differs from the submitted data", img.Name)
		}
	}
	if files := env.uploadedFiles(); len(files) != len(images) {
		t.Errorf("upload dir = %v, want only the %d images (no temp files)", files, len(images))
	}
	served := env.get(UploadURLPrefix + large)
	expectSyncStatus(t, served, http.StatusOK)
	if !bytes.Equal(served.Body.Bytes(), images[1].Data) {
		t.Error("served image differs from the submitted data")
	}

	t.Run("identical resubmission is accepted", func(t *testing.T) {
		expectSyncStatus(t, env.call(writer, req), http.StatusOK)
		if files := env.uploadedFiles(); len(files) != len(images) {
			t.Errorf("upload dir = %v after resubmission", files)
		}
	})

	t.Run("different content under an existing name is rejected", func(t *testing.T) {
		other := localImages(t, 40, 30)
		fresh := SyncImage{Name: strings.Repeat("b", 32) + "-1200.webp", Data: other[1].Data}
		clash := SyncImage{Name: large, Data: other[1].Data}
		rec := env.call(writer, submitRequest(SyncArticle{ID: id, Title: "差し替えの試み"}, fresh, clash))
		expectSyncStatus(t, rec, http.StatusConflict)
		got, err := os.ReadFile(filepath.Join(env.uploadDir, large))
		if err != nil || !bytes.Equal(got, images[1].Data) {
			t.Errorf("existing image was overwritten (err=%v)", err)
		}
		// 同じ要求で先に保存した画像も消し、記事も更新しない。
		if _, err := os.Stat(filepath.Join(env.uploadDir, fresh.Name)); !os.IsNotExist(err) {
			t.Errorf("image saved before the conflict was not removed: %v", err)
		}
		if a := env.mustGetArticle(id); a.Title != "画像付きの記事" {
			t.Errorf("article updated despite image conflict: %q", a.Title)
		}
	})

	pngData := pngBytes(t, 8, 8)
	corrupt := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0xff}, 32)...)
	bad := []struct {
		name  string
		image SyncImage
		want  int
	}{
		{"path traversal", SyncImage{Name: "../" + large, Data: images[1].Data}, http.StatusUnprocessableEntity},
		{"nested path", SyncImage{Name: "sub/" + large, Data: images[1].Data}, http.StatusUnprocessableEntity},
		{"unexpected name", SyncImage{Name: "evil.webp", Data: images[1].Data}, http.StatusUnprocessableEntity},
		{"other extension", SyncImage{Name: strings.Repeat("c", 32) + "-1200.html", Data: images[1].Data}, http.StatusUnprocessableEntity},
		{"png disguised as webp", SyncImage{Name: strings.Repeat("d", 32) + "-1200.webp", Data: pngData}, http.StatusUnsupportedMediaType},
		{"corrupt webp", SyncImage{Name: strings.Repeat("e", 32) + "-1200.webp", Data: corrupt}, http.StatusUnprocessableEntity},
		{"empty data", SyncImage{Name: strings.Repeat("f", 32) + "-1200.webp"}, http.StatusUnsupportedMediaType},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			before := env.uploadedFiles()
			rec := env.call(writer, submitRequest(SyncArticle{Title: "不正な画像 " + tt.name}, tt.image))
			expectSyncStatus(t, rec, tt.want)
			if after := env.uploadedFiles(); len(after) != len(before) {
				t.Errorf("upload dir changed: %v -> %v", before, after)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(env.uploadDir), large)); !os.IsNotExist(err) {
				t.Errorf("image escaped the upload dir: %v", err)
			}
		})
	}
	if n := env.countRows(`SELECT COUNT(*) FROM articles WHERE title LIKE '不正な画像%'`); n != 0 {
		t.Errorf("articles with rejected images were saved: %d", n)
	}

	t.Run("duplicate and too many images", func(t *testing.T) {
		rec := env.call(writer, submitRequest(SyncArticle{Title: "重複"}, images[0], images[0]))
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
		many := make([]SyncImage, maxSyncImages+1)
		for i := range many {
			many[i] = SyncImage{Name: fmt.Sprintf("%032x-400.webp", i), Data: images[3].Data}
		}
		rec = env.call(writer, submitRequest(SyncArticle{Title: "多すぎる画像"}, many...))
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
		if files := env.uploadedFiles(); len(files) != len(images) {
			t.Errorf("upload dir = %d files, want %d", len(files), len(images))
		}
	})
}

func TestSyncAPIFetchPending(t *testing.T) {
	env := newSyncEnv(t)
	editor := env.token("desk", auth.RoleEditor)

	rec := env.call(editor, SyncRequest{Action: SyncActionFetchPending})
	expectSyncStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"articles":[]`) {
		t.Errorf("empty queue must be an empty array: %s", rec.Body.String())
	}

	catID := seedCategory(t, env.db, "研究", "research", "")
	seedArticle(t, env.db, article.Article{Title: "下書きの記事", Status: article.StatusDraft})
	seedArticle(t, env.db, article.Article{Title: "公開中の記事", Status: article.StatusPublished})
	first := seedArticle(t, env.db, article.Article{Title: "先に提出した記事", Slug: "first", ContentMarkdown: "一つ目の本文",
		Excerpt: "抜粋", Status: article.StatusReview, CategoryID: catID, AuthorID: studentA.ID})
	if _, err := env.db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Hour), first); err != nil {
		t.Fatal(err)
	}
	submitted := decodeSync[SyncSubmitResponse](t, env.call(env.token("tanaka", auth.RoleWriter),
		submitRequest(SyncArticle{Title: "後から提出した記事", ContentMarkdown: "二つ目の本文"})))

	for _, role := range []auth.Role{auth.RoleEditor, auth.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			rec := env.call(env.token("reviewer", role), SyncRequest{Action: SyncActionFetchPending})
			expectSyncStatus(t, rec, http.StatusOK)
			res := decodeSync[SyncPendingResponse](t, rec)
			if res.Action != SyncActionFetchPending || len(res.Articles) != 2 {
				t.Fatalf("response = %+v, want 2 pending articles", res)
			}
			got := res.Articles[0]
			if got.ID != first || got.Title != "先に提出した記事" || got.Slug != "first" || got.ContentMarkdown != "一つ目の本文" ||
				got.Excerpt != "抜粋" || got.Status != article.StatusReview || got.Category != "research" ||
				got.CategoryName != "研究" || got.AuthorID != studentA.ID || got.AuthorName != studentA.DisplayName {
				t.Errorf("articles[0] = %+v", got)
			}
			if second := res.Articles[1]; second.ID != submitted.Article.ID || second.ContentMarkdown != "二つ目の本文" ||
				second.AuthorName != "tanaka" {
				t.Errorf("articles[1] = %+v", second)
			}
		})
	}
}

func TestSyncAPIPublishArticle(t *testing.T) {
	env := newSyncEnv(t)
	searchCount := func(q string) string {
		t.Helper()
		rec := env.get("/search?q=" + url.QueryEscape(q))
		expectSyncStatus(t, rec, http.StatusOK)
		m := regexp.MustCompile(`の検索結果: (\d+)件`).FindStringSubmatch(rec.Body.String())
		if m == nil {
			t.Fatalf("result count not found: %.1000s", rec.Body.String())
		}
		return m[1]
	}

	// ライターが提出し、編集者が承認して公開する（仕様書の検証手順 3）。
	submitted := decodeSync[SyncSubmitResponse](t, env.call(env.token("tanaka", auth.RoleWriter), submitRequest(SyncArticle{
		Title: "吹奏楽部の定期演奏会", Slug: "brass-concert", ContentMarkdown: "ホールで定期演奏会が開かれた。",
	})))
	id := submitted.Article.ID
	expectSyncStatus(t, env.get("/articles/brass-concert"), http.StatusNotFound)
	if n := searchCount("定期演奏会"); n != "0" {
		t.Fatalf("pending article is searchable: %s hits", n)
	}

	rec := env.call(env.token("tanaka", auth.RoleWriter), publishRequest(id))
	expectSyncStatus(t, rec, http.StatusForbidden)

	before := time.Now().UTC().Add(-time.Second)
	rec = env.call(env.token("desk", auth.RoleEditor), publishRequest(id))
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncPublishResponse](t, rec)
	if res.Action != SyncActionPublishArticle || res.Article.ID != id || res.Article.Status != article.StatusPublished ||
		res.Article.URL != "/articles/brass-concert" || res.Article.PublishedAt == nil || res.Article.PublishedAt.Before(before) {
		t.Errorf("response = %+v", res)
	}
	a := env.mustGetArticle(id)
	if a.Status != article.StatusPublished || a.PublishedAt == nil || a.LockedBy != nil {
		t.Errorf("published article = %+v (lock must be released)", a)
	}
	page := env.get("/articles/brass-concert")
	expectSyncStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), "吹奏楽部の定期演奏会") {
		t.Error("public page does not show the published article")
	}
	if n := searchCount("定期演奏会"); n != "1" {
		t.Errorf("published article hits = %s, want 1", n)
	}

	t.Run("already published", func(t *testing.T) {
		rec := env.call(env.token("desk", auth.RoleEditor), publishRequest(id))
		expectSyncStatus(t, rec, http.StatusConflict)
		if !strings.Contains(syncErrorMessage(t, rec), "公開中") {
			t.Errorf("message does not show the current status: %s", rec.Body.String())
		}
	})

	t.Run("locked by another user", func(t *testing.T) {
		reviewID := env.seedReview("編集中の記事", "being-edited")
		if _, err := env.server.locker.Acquire(context.Background(), reviewID, studentB.ID); err != nil {
			t.Fatal(err)
		}
		rec := env.call(env.token("chief-admin", auth.RoleAdmin), publishRequest(reviewID))
		expectSyncStatus(t, rec, http.StatusConflict)
		if st := env.mustGetArticle(reviewID).Status; st != article.StatusReview {
			t.Errorf("locked article status = %s", st)
		}
	})
}

func TestSyncAPISyncSettings(t *testing.T) {
	env := newSyncEnv(t)
	adminToken := env.token("chief-admin", auth.RoleAdmin)

	rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{
		SiteName:        strPtr("  同期したメディア "),
		SiteDescription: strPtr("学生が運営するメディア"),
		BaseURL:         strPtr("https://media.example.ac.jp/"),
		Copyright:       strPtr("(c) 2026 Example University"),
		AnalyticsTag:    strPtr(`<script async src="https://analytics.example.com/a.js"></script>`),
		Theme:           strPtr("news"),
	}})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncSettingsResponse](t, rec)
	want := SyncSettingsValues{
		SiteName:        "同期したメディア",
		SiteDescription: "学生が運営するメディア",
		BaseURL:         "https://media.example.ac.jp",
		Copyright:       "(c) 2026 Example University",
		AnalyticsTag:    `<script async src="https://analytics.example.com/a.js"></script>`,
		Theme:           "news",
		SiteMenu:        []MenuItem{},
		// フッターのサイトマップは、知っていることを伝えるため空でも空の配列で返す。
		SiteFooterMenu: &[]MenuItem{},
		FrontSections:  []FrontSection{{Type: FrontSectionArticleList}},
		// 多言語対応は送っていないため、英語の記事の無い新しいサイトの既定（OFF）のまま返す。
		MultilingualEnabled: boolPtr(false),
		ShowAuthor:          boolPtr(false),
		// AI 機能の ON/OFF は送っていないため、公開サーバーの既定（ON）のまま返す（受け取れる公開サーバーであることも伝わる）。
		AIEnabled: boolPtr(true),
	}
	if res.Action != SyncActionSyncSettings || !reflect.DeepEqual(res.Settings, want) {
		t.Errorf("response = %+v, want %+v", res, want)
	}
	for key, value := range map[string]string{"site_name": want.SiteName, "site_description": want.SiteDescription,
		"base_url": want.BaseURL, "copyright": want.Copyright, "analytics_tag": want.AnalyticsTag, "theme": want.Theme} {
		if got := env.setting(key); got != value {
			t.Errorf("settings[%s] = %q, want %q", key, got, value)
		}
	}
	if cfg := env.server.cfg; cfg.SiteName != want.SiteName || cfg.SiteDescription != want.SiteDescription || cfg.BaseURL != want.BaseURL {
		t.Errorf("in-memory config not updated: %q %q %q", cfg.SiteName, cfg.SiteDescription, cfg.BaseURL)
	}
	home := env.get("/")
	expectSyncStatus(t, home, http.StatusOK)
	expectContains(t, home.Body.String(), "同期したメディア")

	t.Run("partial update keeps other settings", func(t *testing.T) {
		rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{Copyright: strPtr("(c) 2027")}})
		expectSyncStatus(t, rec, http.StatusOK)
		res := decodeSync[SyncSettingsResponse](t, rec)
		if res.Settings.Copyright != "(c) 2027" || res.Settings.SiteName != want.SiteName || res.Settings.Theme != want.Theme {
			t.Errorf("settings = %+v", res.Settings)
		}
	})

	t.Run("theme outside the catalog is rejected", func(t *testing.T) {
		// 廃止した mono も、管理画面・エクスポートと同じく 422 で拒否し、公開サーバーの更新を案内する。
		for _, bad := range []string{"mono", "minimal", "editorial", "../news", "NEWS"} {
			rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{Theme: strPtr(bad)}})
			expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
			if body := rec.Body.String(); !strings.Contains(body, "公開サーバーの sorus を更新してください") {
				t.Errorf("theme %q error body = %s", bad, body)
			}
		}
		if got := env.setting("theme"); got != want.Theme {
			t.Errorf("theme after rejected sync = %q, want %q", got, want.Theme)
		}
	})

	t.Run("empty site name falls back to default", func(t *testing.T) {
		rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{SiteName: strPtr(" ")}})
		expectSyncStatus(t, rec, http.StatusOK)
		if got := env.setting("site_name"); got != DefaultSiteName {
			t.Errorf("site_name = %q, want %q", got, DefaultSiteName)
		}
	})

	t.Run("invalid value changes nothing", func(t *testing.T) {
		rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{
			Copyright: strPtr("(c) 2030"), BaseURL: strPtr("ftp://media.example.ac.jp"),
		}})
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
		if got := env.setting("copyright"); got != "(c) 2027" {
			t.Errorf("copyright = %q after rejected update", got)
		}
	})
}

// sync_settings の show_author（記事や一覧に執筆者名を表示する）は、管理画面のサイト基本設定と同じ形で保存し、
// 公開サイトの表示（メモリ上の設定）にもすぐ反映する。送らなければ変えない。
func TestSyncAPISyncSettingsShowAuthor(t *testing.T) {
	env := newSyncEnv(t)
	adminToken := env.token("chief-admin", auth.RoleAdmin)
	show := true
	rec := env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{ShowAuthor: &show}})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncSettingsResponse](t, rec)
	if res.Settings.ShowAuthor == nil || !*res.Settings.ShowAuthor {
		t.Errorf("response show_author = %v, want true", res.Settings.ShowAuthor)
	}
	if got := env.setting(settingShowAuthor); got != "1" {
		t.Errorf("settings[show_author] = %q, want \"1\"", got)
	}
	if !env.server.getShowAuthor() {
		t.Error("in-memory ShowAuthor was not updated")
	}

	// ほかの項目だけを送っても変わらない。
	rec = env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{Copyright: strPtr("(c) 2027")}})
	expectSyncStatus(t, rec, http.StatusOK)
	if !env.server.getShowAuthor() || env.setting(settingShowAuthor) != "1" {
		t.Error("show_author changed by an update without show_author")
	}

	hide := false
	rec = env.call(adminToken, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{ShowAuthor: &hide}})
	expectSyncStatus(t, rec, http.StatusOK)
	if got := env.setting(settingShowAuthor); got != "0" || env.server.getShowAuthor() {
		t.Errorf("after hiding: settings = %q, in-memory = %v", got, env.server.getShowAuthor())
	}
}

// 同期 API は、1つの IP アドレスからの認証の失敗が続くと、期間が明けるまで正しいトークンの要求も 429 で断る。
// ほかの IP アドレスからの要求は断らない。
func TestSyncAPIRateLimitsAuthFailures(t *testing.T) {
	env := newSyncEnv(t)
	good := env.token("chief-admin", auth.RoleAdmin)
	body := []byte(`{"action":"server_info"}`)
	send := func(ip, authorization string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, env.server.cfg.SyncAPIPath, bytes.NewReader(body))
		req.RemoteAddr = ip + ":40000"
		req.Header.Set(echo.HeaderAuthorization, authorization)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		// 偽装した X-Forwarded-For では制限を逃れられない。
		req.Header.Set(echo.HeaderXForwardedFor, fmt.Sprintf("198.51.100.%d", len(authorization)%250))
		return env.serve(req)
	}
	for i := 0; i < syncAuthFailureLimit; i++ {
		expectSyncStatus(t, send("203.0.113.7", fmt.Sprintf("Bearer bad-token-%d", i)), http.StatusUnauthorized)
	}
	rec := send("203.0.113.7", "Bearer "+good)
	expectSyncStatus(t, rec, http.StatusTooManyRequests)
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("429 without Retry-After")
	}
	expectSyncStatus(t, send("203.0.113.8", "Bearer "+good), http.StatusOK)
}

// syncRateLimiter は期間の中の回数だけを数え、期間が明けたら数え直す。
func TestSyncRateLimiterWindow(t *testing.T) {
	l := newSyncRateLimiter(2, time.Minute)
	start := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	if l.hit("a", start) != 0 || l.hit("a", start.Add(time.Second)) != 0 {
		t.Fatal("hits within the limit were refused")
	}
	if wait := l.hit("a", start.Add(2*time.Second)); wait != 58*time.Second {
		t.Errorf("wait = %v, want 58s", wait)
	}
	if l.blocked("a", start.Add(30*time.Second)) == 0 {
		t.Error("not blocked within the window")
	}
	if l.blocked("b", start) != 0 || l.hit("b", start) != 0 {
		t.Error("another client was limited")
	}
	if l.blocked("a", start.Add(time.Minute)) != 0 || l.hit("a", start.Add(time.Minute)) != 0 {
		t.Error("still limited after the window")
	}
}

// PC で「記事や一覧に執筆者名を表示する」を変えて「公開サーバーへ反映する」を押すと、公開サーバーにも保存され、
// 公開サーバーの表示（メモリ上の設定）も切り替わる。
func TestRemoteSettingsPushShowAuthor(t *testing.T) {
	env := newRemoteEnv(t)
	env.configure("root", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	for _, show := range []bool{true, false} {
		form := url.Values{"site_name": {"新聞部ウェブ"}, "site_description": {""}, "base_url": {""}, "copyright": {""}, "analytics_tag": {""}}
		if show {
			form.Set("show_author", "1")
		}
		expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general")
		var out struct {
			Message  string             `json:"message"`
			Settings SyncSettingsValues `json:"settings"`
		}
		expectStatus(t, adm.postJSON("/admin/remote/settings/push", nil, &out), http.StatusOK)
		if out.Settings.ShowAuthor == nil || *out.Settings.ShowAuthor != show {
			t.Errorf("show=%v: pushed show_author = %v", show, out.Settings.ShowAuthor)
		}
		if strings.Contains(out.Message, "執筆者名") {
			t.Errorf("show=%v: message mentions an unsupported server: %q", show, out.Message)
		}
		if got := env.remote.server.getShowAuthor(); got != show {
			t.Errorf("show=%v: public server ShowAuthor = %v", show, got)
		}
		if got, want := env.remote.setting(settingShowAuthor), formatShowAuthor(show); got != want {
			t.Errorf("show=%v: public server settings[show_author] = %q, want %q", show, got, want)
		}
	}
}

// 記事・固定ページのアイキャッチ画像の URL を公開サーバーに保存する。項目が無い（古いローカル CMS）更新では今の値を保ち、
// 空文字なら外す。/ で始まるパスか http(s) の URL 以外は断る。
func TestSyncAPIFeaturedImageURL(t *testing.T) {
	env := newSyncEnv(t)
	editor := env.token("desk", auth.RoleEditor)
	const (
		postID = "0123456789abcdef0123456789abcd01"
		pageID = "0123456789abcdef0123456789abcd02"
		cover  = "/uploads/0123456789abcdef0123456789abcdef-1200.webp"
		other  = "https://cdn.example.jp/cover.webp"
	)
	featured := func(id string) string {
		t.Helper()
		return env.mustGetArticle(id).FeaturedImage
	}
	publishWith := func(a SyncArticle) *httptest.ResponseRecorder {
		t.Helper()
		return env.call(editor, SyncRequest{Action: SyncActionPublishArticle, Article: &a})
	}

	for _, tc := range []struct {
		name string
		id   string
		kind article.Kind
		slug string
	}{
		{"post", postID, article.KindPost, "featured-post"},
		{"page", pageID, article.KindPage, "featured-page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := SyncArticle{ID: tc.id, Title: "アイキャッチの同期", Slug: tc.slug, Kind: tc.kind, ContentMarkdown: "本文"}

			// 提出（submit_draft）で新しく作るときに保存する。
			withCover := base
			withCover.FeaturedImageURL = strPtr("  " + cover + " ")
			expectSyncStatus(t, env.call(editor, submitRequest(withCover)), http.StatusOK)
			if got := featured(tc.id); got != cover {
				t.Fatalf("after submit: featured_image_url = %q, want %q", got, cover)
			}

			// 直接公開（publish_article）で別の画像に替える。
			withOther := base
			withOther.FeaturedImageURL = strPtr(other)
			expectSyncStatus(t, publishWith(withOther), http.StatusOK)
			if got := featured(tc.id); got != other {
				t.Fatalf("after publish: featured_image_url = %q, want %q", got, other)
			}

			// 項目の無い更新（古いローカル CMS）は今の値を保つ。JSON に featured_image_url が出ないことも確かめる。
			if b, _ := json.Marshal(base); strings.Contains(string(b), "featured_image_url") {
				t.Fatalf("nil FeaturedImageURL must be omitted: %s", b)
			}
			expectSyncStatus(t, publishWith(base), http.StatusOK)
			if got := featured(tc.id); got != other {
				t.Fatalf("update without the field: featured_image_url = %q, want %q (kept)", got, other)
			}

			// 空文字は外す。
			cleared := base
			cleared.FeaturedImageURL = strPtr("")
			expectSyncStatus(t, publishWith(cleared), http.StatusOK)
			if got := featured(tc.id); got != "" {
				t.Fatalf("after clear: featured_image_url = %q, want empty", got)
			}
		})
	}

	t.Run("invalid url is rejected", func(t *testing.T) {
		for _, bad := range []string{"javascript:alert(1)", "//evil.example/x.webp", "data:image/png;base64,AAAA", "/uploads/a b.webp"} {
			a := SyncArticle{ID: postID, Title: "アイキャッチの同期", Slug: "featured-post", ContentMarkdown: "本文", FeaturedImageURL: strPtr(bad)}
			rec := publishWith(a)
			expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
			if !strings.Contains(syncErrorMessage(t, rec), "featured_image_url") {
				t.Errorf("%q: message = %s", bad, rec.Body.String())
			}
		}
		if got := featured(postID); got != "" {
			t.Errorf("rejected request changed featured_image_url to %q", got)
		}
	})
}
