package publish_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"sorus/internal/publish"
	"sorus/internal/publish/publishtest"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
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

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func manifestOf(t *testing.T, dir string) publish.Manifest {
	t.Helper()
	m, err := publish.BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// testSite は記事2件のサイト。
var testSite = map[string]string{
	"index.html":               "<a href=/articles/kept/>kept</a><a href=/articles/gone/>gone</a>",
	"articles/kept/index.html": "残す記事",
	"articles/gone/index.html": "非公開にする記事",
	"static/css/site.css":      "body{}",
}

// reflectTwice は remote への1回目の反映（すべて送る）と、記事 gone を消した2回目の反映を行い、
// 2回目の結果を返す。dial は反映のたびに接続を作る。
func reflectTwice(t *testing.T, dial func() publish.Remote) publish.Result {
	t.Helper()
	site := t.TempDir()
	writeFiles(t, site, testSite)
	ctx := context.Background()

	r := dial()
	m1 := manifestOf(t, site)
	res, err := publish.Sync(ctx, r, site, nil, m1, nil)
	r.Close()
	if err != nil {
		t.Fatalf("first reflect: %v", err)
	}
	if res.Uploaded != len(testSite) || res.Deleted != 0 || !res.FirstTime || res.KeptExisting != 1 {
		t.Errorf("first reflect = %+v, want all %d uploaded and 1 existing entry kept", res, len(testSite))
	}

	os.RemoveAll(filepath.Join(site, "articles", "gone"))
	writeFiles(t, site, map[string]string{"index.html": "<a href=/articles/kept/>kept</a>"})
	r = dial()
	res, err = publish.Sync(ctx, r, site, m1, manifestOf(t, site), nil)
	r.Close()
	if err != nil {
		t.Fatalf("second reflect: %v", err)
	}
	if res.Uploaded != 1 || res.Deleted != 1 || res.Unchanged != 2 || res.FirstTime {
		t.Errorf("second reflect = %+v, want 1 uploaded (index.html), 1 deleted, 2 unchanged", res)
	}
	return res
}

func TestFTPReflectUploadsAllThenDeletesOnlyRemovedFiles(t *testing.T) {
	root := t.TempDir()
	// 利用者がもともと置いていたファイル。前回の一覧に無いため、どの反映でも消さない。
	writeFiles(t, root, map[string]string{"public_html/.htaccess": "keep", "other/keep.txt": "keep"})
	srv := publishtest.NewFTPServer(t, root)
	dial := func() publish.Remote {
		r, err := publish.DialFTP(context.Background(), publish.FTPConfig{
			Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password, Root: "/public_html",
			AllowPlaintext: true,
		})
		if err != nil {
			t.Fatalf("DialFTP: %v", err)
		}
		return r
	}
	res := reflectTwice(t, dial)
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "暗号化せず") {
		t.Errorf("notes = %v, want a note about the unencrypted connection", res.Notes)
	}
	want := []string{"other/keep.txt", "public_html/.htaccess", "public_html/articles/kept/index.html",
		"public_html/index.html", "public_html/static/css/site.css"}
	if got := filesUnder(t, root); !reflect.DeepEqual(got, want) {
		t.Errorf("remote files = %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, "public_html", "articles", "gone")); !os.IsNotExist(err) {
		t.Errorf("empty folder of the removed article was left: %v", err)
	}
}

func TestFTPFailedUploadReturnsError(t *testing.T) {
	root := t.TempDir()
	srv := publishtest.NewFTPServer(t, root)
	srv.FailStor = func(p string) bool { return p == "/www/index.html" }
	site := t.TempDir()
	writeFiles(t, site, testSite)
	r, err := publish.DialFTP(context.Background(), publish.FTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password, Root: "/www",
		AllowPlaintext: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	res, err := publish.Sync(context.Background(), r, site, nil, manifestOf(t, site), nil)
	if err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("Sync = %v, want an upload error for index.html", err)
	}
	// トップページは最後に送るため、ほかのファイルは送れている。
	if res.Uploaded != len(testSite)-1 {
		t.Errorf("uploaded = %d, want %d before the failure", res.Uploaded, len(testSite)-1)
	}
}

func TestFTPRefusesWholeServerRoot(t *testing.T) {
	srv := publishtest.NewFTPServer(t, t.TempDir())
	for _, root := range []string{"/", "", "."} {
		_, err := publish.DialFTP(context.Background(), publish.FTPConfig{
			Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password, Root: root,
		})
		if !errors.Is(err, publish.ErrUnsafeRoot) {
			t.Errorf("DialFTP(root %q) = %v, want ErrUnsafeRoot", root, err)
		}
	}
	if n := len(srv.Commands()); n != 0 {
		t.Errorf("server received %d commands for a refused root, want 0 (refuse before connecting)", n)
	}
}

func TestFTPWrongPasswordIsAuthError(t *testing.T) {
	srv := publishtest.NewFTPServer(t, t.TempDir())
	_, err := publish.DialFTP(context.Background(), publish.FTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: "wrong", Root: "/www",
		AllowPlaintext: true,
	})
	if !errors.Is(err, publish.ErrAuth) {
		t.Errorf("DialFTP = %v, want ErrAuth", err)
	}
}

func TestFTPUsesExplicitTLSWhenAvailable(t *testing.T) {
	root := t.TempDir()
	srv := publishtest.NewFTPServer(t, root)
	cert, pool := selfSignedCert(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	site := t.TempDir()
	writeFiles(t, site, testSite)
	r, err := publish.DialFTP(context.Background(), publish.FTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password, Root: "/www",
		TLSConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("DialFTP over TLS: %v", err)
	}
	res, err := publish.Sync(context.Background(), r, site, nil, manifestOf(t, site), nil)
	r.Close()
	if err != nil {
		t.Fatalf("Sync over TLS: %v", err)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, want none for an encrypted connection", res.Notes)
	}
	cmds := strings.Join(srv.Commands(), " ")
	if !strings.HasPrefix(cmds, "AUTH USER PASS") || !strings.Contains(cmds, "PROT") {
		t.Errorf("commands = %s, want AUTH TLS before USER and PROT P for data", cmds)
	}
	if got := filesUnder(t, filepath.Join(root, "www")); len(got) != len(testSite) {
		t.Errorf("uploaded files = %v", got)
	}
}

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sorus test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestSFTPReflectUploadsAllThenDeletesOnlyRemovedFiles(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"www/keep.txt": "利用者のファイル", "outside.txt": "keep"})
	srv := publishtest.NewSFTPServer(t, publishtest.NewHostKey(t))
	known := ""
	dial := func() publish.Remote {
		r, err := publish.DialSFTP(context.Background(), publish.SFTPConfig{
			Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password,
			Root: filepath.ToSlash(filepath.Join(root, "www")), KnownHostKey: known,
		})
		if err != nil {
			t.Fatalf("DialSFTP: %v", err)
		}
		// 初めての接続で鍵を覚え、2回目からはその鍵と比べる。
		if known == "" {
			known = r.HostKey()
		}
		return r
	}
	reflectTwice(t, dial)
	if known != srv.Fingerprint() {
		t.Errorf("remembered host key = %q, want %q", known, srv.Fingerprint())
	}
	want := []string{"outside.txt", "www/articles/kept/index.html", "www/index.html", "www/keep.txt", "www/static/css/site.css"}
	if got := filesUnder(t, root); !reflect.DeepEqual(got, want) {
		t.Errorf("remote files = %v, want %v", got, want)
	}
}

func TestSFTPChangedHostKeyBlocksUpload(t *testing.T) {
	root := t.TempDir()
	srv := publishtest.NewSFTPServer(t, publishtest.NewHostKey(t))
	known := srv.Fingerprint()
	srv.SetHostKey(publishtest.NewHostKey(t))

	_, err := publish.DialSFTP(context.Background(), publish.SFTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password,
		Root: filepath.ToSlash(filepath.Join(root, "www")), KnownHostKey: known,
	})
	var changed *publish.HostKeyChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("DialSFTP = %v, want HostKeyChangedError", err)
	}
	if changed.Known != known || changed.Got != srv.Fingerprint() {
		t.Errorf("changed = %+v, want known %s got %s", changed, known, srv.Fingerprint())
	}
	if got := filesUnder(t, root); len(got) != 0 {
		t.Errorf("files were written despite the changed host key: %v", got)
	}
}

func TestSFTPRefusesWholeServerRootAndWrongPassword(t *testing.T) {
	srv := publishtest.NewSFTPServer(t, publishtest.NewHostKey(t))
	if _, err := publish.DialSFTP(context.Background(), publish.SFTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password, Root: "/",
	}); !errors.Is(err, publish.ErrUnsafeRoot) {
		t.Errorf("DialSFTP(root /) = %v, want ErrUnsafeRoot", err)
	}
	if _, err := publish.DialSFTP(context.Background(), publish.SFTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: "wrong", Root: "/tmp/www",
	}); !errors.Is(err, publish.ErrAuth) {
		t.Errorf("DialSFTP(wrong password) = %v, want ErrAuth", err)
	}
}

// 暗号化（AUTH TLS）を断るサーバーには、利用者が「暗号化しないで送る」を選んでいない限り、ユーザー名・パスワードを送らない。
func TestFTPWithoutTLSRefusesPlaintextLoginByDefault(t *testing.T) {
	srv := publishtest.NewFTPServer(t, t.TempDir())
	r, err := publish.DialFTP(context.Background(), publish.FTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password, Root: "/www",
	})
	if err == nil {
		r.Close()
		t.Fatal("DialFTP to a server without TLS succeeded, want an error unless plaintext is allowed")
	}
	if !errors.Is(err, publish.ErrFTPNoEncryption) {
		t.Errorf("DialFTP = %v, want ErrFTPNoEncryption", err)
	}
	for _, cmd := range srv.Commands() {
		if cmd == "USER" || cmd == "PASS" {
			t.Errorf("commands = %v, want no USER/PASS sent without encryption", srv.Commands())
			break
		}
	}
}

// 「暗号化しないで送る」を選んでいても、サーバーが対応していれば暗号化する。
func TestFTPAllowPlaintextStillPrefersTLS(t *testing.T) {
	srv := publishtest.NewFTPServer(t, t.TempDir())
	cert, pool := selfSignedCert(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	r, err := publish.DialFTP(context.Background(), publish.FTPConfig{
		Host: "127.0.0.1", Port: srv.Port(), User: srv.User, Password: srv.Password, Root: "/www",
		TLSConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}, AllowPlaintext: true,
	})
	if err != nil {
		t.Fatalf("DialFTP: %v", err)
	}
	defer r.Close()
	if notes := r.Notes(); len(notes) != 0 {
		t.Errorf("notes = %v, want none for an encrypted connection", notes)
	}
	if cmds := strings.Join(srv.Commands(), " "); !strings.HasPrefix(cmds, "AUTH USER PASS") {
		t.Errorf("commands = %s, want AUTH TLS before USER", cmds)
	}
}
