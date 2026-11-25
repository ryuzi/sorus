package server

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	glog "github.com/labstack/gommon/log"

	"sorus/internal/auth"
)

// expectProxiedImage は PC を通した画像の応答が want の WebP で、キャッシュに残さない見出しを持つことを確かめる。
func expectProxiedImage(t *testing.T, what string, res *response, want []byte) {
	t.Helper()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/webp" || res.Body != string(want) {
		t.Fatalf("%s = %d %q (%d bytes), want the image", what, res.StatusCode, res.Header.Get("Content-Type"), len(res.Body))
	}
	for k, v := range map[string]string{"Cache-Control": "private, no-store", "X-Content-Type-Options": "nosniff", "Content-Security-Policy": "default-src 'none'; sandbox"} {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", what, k, got, v)
		}
	}
}

// expectNoImage は応答が画像を返していないことを確かめる。
func expectNoImage(t *testing.T, what string, res *response, status int, data []byte) {
	t.Helper()
	if res.StatusCode != status || res.Header.Get("Content-Type") == "image/webp" || strings.Contains(res.Body, string(data)) {
		t.Errorf("%s = %d %q, want %d without the image", what, res.StatusCode, res.Header.Get("Content-Type"), status)
	}
}

// 公開サーバーの画像を PC を通して見られるのは、PC にログインした編集者が、校閲待ちを読めるトークン（editor・admin）を
// 設定しているときだけ。ログインしていない人・ライターには返さない。公開サーバーの閲覧制限の有無にかかわらず見られる。
func TestRemoteUploadProxyAuthorization(t *testing.T) {
	env := &remoteEnv{local: newTestEnv(t), remote: newPublicOnlyTLSEnv(t, nil)}
	images := localImages(t, 320, 200)
	if err := os.MkdirAll(env.remote.uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, img := range images {
		if err := os.WriteFile(env.remote.uploadDir+"/"+img.Name, img.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := images[1]
	path := remoteUploadProxyPrefix + img.Name
	editor := env.local.loggedIn(chief)

	expectNoImage(t, "not configured", editor.browserLoad(path), http.StatusConflict, img.Data)

	tok := env.configure("sato", auth.RoleEditor)
	expectProxiedImage(t, "restriction off", editor.browserLoad(path), img.Data)
	owner := env.remote.token("owner", auth.RoleAdmin)
	expectOK(t, env.remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))
	expectBasicAuthChallenge(t, env.remote.newClient().get("/uploads/"+img.Name))
	res := editor.browserLoad(path)
	expectProxiedImage(t, "restriction on", res, img.Data)
	// トークンはブラウザに渡さない。
	for k, vs := range res.Header {
		if strings.Contains(strings.Join(vs, " "), tok) {
			t.Errorf("token in response header %s", k)
		}
	}

	anon := env.local.newClient().browserLoad(path)
	if anon.StatusCode == http.StatusOK || anon.Header.Get("Content-Type") == "image/webp" || strings.Contains(anon.Body, string(img.Data)) {
		t.Errorf("anonymous = %d %q, want no image", anon.StatusCode, anon.Header.Get("Content-Type"))
	}
	expectNoImage(t, "writer", env.local.loggedIn(studentA).browserLoad(path), http.StatusForbidden, img.Data)
	// POST などは受け付けない（CSRF の確認より前に、取得のための経路として存在しない）。
	if res := editor.postJSON(path, nil, nil); res.StatusCode == http.StatusOK {
		t.Errorf("POST = %d, want an error", res.StatusCode)
	}

	env.configure("yamada", auth.RoleWriter)
	expectNoImage(t, "writer token", editor.browserLoad(path), http.StatusForbidden, img.Data)
	env.configure("owner", auth.RoleAdmin)
	expectProxiedImage(t, "admin token", editor.browserLoad(path), img.Data)
}

// imageUpstream は画像を返す公開サーバーの代わり。受け取った要求を記録する。
type imageUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []string
}

func newImageUpstream(t *testing.T, handle http.HandlerFunc) *imageUpstream {
	t.Helper()
	u := &imageUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits = append(u.hits, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		u.mu.Unlock()
		if handle != nil {
			handle(w, r)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *imageUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.hits...)
}

// configureUpstream はローカル CMS の接続先を u にし、role のトークンを保存して返す。
func configureUpstream(t *testing.T, local *testEnv, u *imageUpstream, role auth.Role) string {
	t.Helper()
	_, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.IssueToken(priv, "sato", string(role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.server.saveRemoteConfig(remoteConfig{URL: u.srv.URL, SyncPath: "/api/sync-fake", Token: tok}); err != nil {
		t.Fatal(err)
	}
	return tok
}

// 同期の画像の形式のファイル名だけを、設定した公開サーバーの /uploads/ から取得する。ほかのパス・名前では公開サーバーに
// 問い合わせない。
func TestRemoteUploadProxyRejectsOtherNames(t *testing.T) {
	local := newTestEnv(t)
	up := newImageUpstream(t, nil)
	configureUpstream(t, local, up, auth.RoleEditor)
	editor := local.loggedIn(chief)
	hash := strings.Repeat("ab", 16)
	for _, name := range []string{
		"photo.webp",
		strings.ToUpper(hash) + "-1200.webp",
		hash + "-1200.png",
		hash + "-1200.webp.png",
		hash + "-123456.webp",
		hash + "-large.webp",
		"..%2F..%2Fsettings",
		"..%2Fuploads%2F" + hash + "-1200.webp",
		hash + "-1200.webp%3Fx%3D1",
		"%2e%2e",
	} {
		res := editor.browserLoad(remoteUploadProxyPrefix + name)
		if res.StatusCode != http.StatusNotFound || res.Header.Get("Content-Type") == "image/webp" {
			t.Errorf("%s = %d %q, want 404", name, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	if got := up.requests(); len(got) != 0 {
		t.Errorf("the public server was asked: %q", got)
	}
}

// 公開サーバーの応答が画像でない・大きすぎる・転送（リダイレクト）・失敗のときは返さない。転送先には何も送らず、
// トークンは設定した公開サーバーに Authorization でだけ送る。応答・ログにトークンや公開サーバーの応答の本文を出さない。
func TestRemoteUploadProxyUpstreamResponses(t *testing.T) {
	const limit = 64 << 10
	local := newTestEnvWith(t, Config{MaxUploadSize: limit}, nil)
	logs := &lockedBuffer{}
	local.server.echo.Logger.SetOutput(logs)
	local.server.echo.Logger.SetLevel(glog.DEBUG)
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	images := localImages(t, 320, 200)
	if len(images[0].Data) >= limit {
		t.Fatalf("test image is %d bytes, want < %d", len(images[0].Data), limit)
	}
	name := func(i int) string { return fmt.Sprintf("%032x-1200.webp", i) }
	other := newImageUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(images[0].Data) })
	up := newImageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, UploadURLPrefix) {
		case name(1):
			w.Header().Set("Content-Type", "image/webp")
			_, _ = w.Write(images[0].Data)
		case name(3):
			// 転送の応答の本文が画像でも返さない。
			w.Header().Set("Location", other.srv.URL+r.URL.Path)
			w.WriteHeader(http.StatusFound)
			_, _ = w.Write(images[0].Data)
		case name(4):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<script>alert('upstream')</script>"))
		case name(5):
			_, _ = w.Write(append(append([]byte(nil), images[0].Data...), make([]byte, limit)...))
		case name(6):
			http.Error(w, "upstream internal detail", http.StatusInternalServerError)
		case name(7):
			w.Header().Set("WWW-Authenticate", `Basic realm="public", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	})
	tok := configureUpstream(t, local, up, auth.RoleEditor)
	editor := local.loggedIn(chief)
	load := func(i int) *response { return editor.browserLoad(remoteUploadProxyPrefix + name(i)) }

	expectProxiedImage(t, "image", load(1), images[0].Data)
	if got := up.requests(); len(got) != 1 || got[0] != "GET "+UploadURLPrefix+name(1)+" Bearer "+tok {
		t.Errorf("upstream requests = %q", got)
	}

	var bodies []string
	check := func(what string, res *response, status int) {
		t.Helper()
		expectNoImage(t, what, res, status, images[0].Data)
		if h := res.Header.Get("WWW-Authenticate"); h != "" {
			t.Errorf("%s: WWW-Authenticate = %q, want none (the browser must not ask for the public server's password)", what, h)
		}
		bodies = append(bodies, res.Body)
	}
	check("missing", load(2), http.StatusNotFound)
	check("redirect", load(3), http.StatusBadGateway)
	if got := other.requests(); len(got) != 0 {
		t.Errorf("the redirect target was asked: %q", got)
	}
	check("not an image", load(4), http.StatusBadGateway)
	check("too large", load(5), http.StatusBadGateway)
	check("server error", load(6), http.StatusBadGateway)
	check("unauthorized", load(7), http.StatusBadGateway)
	up.srv.Close()
	check("unreachable", load(1), http.StatusBadGateway)

	for i, b := range bodies {
		expectNotContainsAny(t, fmt.Sprintf("response %d", i), b, tok, up.srv.URL, "alert(", "upstream internal detail")
	}
	expectNotContainsAny(t, "logs", logs.String(), tok, "Bearer ")
}

// プレビューの HTML は、同期の画像の形式の参照だけを PC を通すパスにする。それ以外の /uploads/ は公開サーバーを指し、
// ほかのサイトの URL は変えない。
func TestRemotePreviewHTML(t *testing.T) {
	hash := strings.Repeat("0f", 16)
	cfg := remoteConfig{URL: "https://public.example"}
	in := `<p><img src="/uploads/` + hash + `-1200.webp" alt="a"></p>` +
		`<p><a href="/uploads/` + hash + `-original.webp" rel="nofollow">原寸</a></p>` +
		`<img src="/uploads/legacy.png">` +
		`<img src="https://other.example/uploads/` + hash + `-1200.webp">`
	want := `<p><img src="/admin/remote/uploads/` + hash + `-1200.webp" alt="a"></p>` +
		`<p><a href="/admin/remote/uploads/` + hash + `-original.webp" rel="nofollow">原寸</a></p>` +
		`<img src="https://public.example/uploads/legacy.png">` +
		`<img src="https://other.example/uploads/` + hash + `-1200.webp">`
	if got := remotePreviewHTML(in, cfg); got != want {
		t.Errorf("remotePreviewHTML =\n%s\nwant\n%s", got, want)
	}
}
