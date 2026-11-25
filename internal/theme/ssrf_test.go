package theme

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestCheckPublicAddr(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.255.255.254", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "0.0.0.0", "255.255.255.255", "224.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "::ffff:127.0.0.1", "::ffff:192.168.1.1",
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "ff02::1", "2001::1", "2002:c0a8:101::1", "64:ff9b:1::1",
	} {
		if err := CheckPublicAddr(netip.MustParseAddr(s)); !errors.Is(err, ErrBlockedURL) {
			t.Errorf("CheckPublicAddr(%s) = %v, want ErrBlockedURL", s, err)
		}
	}
	for _, s := range []string{"8.8.8.8", "140.82.112.3", "172.32.0.1", "100.128.0.1", "2606:4700::1111", "64:ff9b::808:808"} {
		if err := CheckPublicAddr(netip.MustParseAddr(s)); err != nil {
			t.Errorf("CheckPublicAddr(%s) = %v, want nil", s, err)
		}
	}
}

// IPv4 を埋め込む古い形式（IPv4 互換 ::a.b.c.d、SIIT の ::ffff:0:a.b.c.d）、IPv6 のグローバルユニキャスト（2000::/3）の
// 外の予約された範囲、文書用・ベンチマーク用の範囲は、公開されたアドレスとして扱わない。ゾーン（%eth0）付きの表記は
// netip.Prefix.Contains が常に false を返すため、範囲の判定をすり抜けないよう、公開されたアドレスでも断る。
func TestCheckPublicAddrRejectsEmbeddedReservedAndZonedForms(t *testing.T) {
	for _, s := range []string{
		// IPv4 互換アドレス（廃止）に埋め込んだループバック・メタデータ・私有の IPv4、SIIT の IPv4 変換アドレス。
		"::7f00:1", "::a9fe:a9fe", "::a00:1", "::ffff:0:7f00:1",
		// 2000::/3 の外（未割り当て・SRv6 の SID など）、文書用（RFC 9637）、ベンチマーク用（RFC 5180）。
		"4000::1", "5f00::1", "e000::1", "3fff::1", "2001:2::1",
		// ゾーン付きの表記。
		"64:ff9b::7f00:1%eth0", "2002:7f00:1::%eth0", "fd00::1%eth0", "2606:4700::1111%eth0",
	} {
		if err := CheckPublicAddr(netip.MustParseAddr(s)); !errors.Is(err, ErrBlockedURL) {
			t.Errorf("CheckPublicAddr(%s) = %v, want ErrBlockedURL", s, err)
		}
	}
	u, err := url.Parse("https://[64:ff9b::7f00:1%25eth0]/theme.zip")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkFetchURL(u, CheckPublicAddr); !errors.Is(err, ErrBlockedURL) {
		t.Errorf("checkFetchURL(%s) = %v, want ErrBlockedURL", u, err)
	}
}

func TestResolveArchiveURLs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		hint string
	}{
		{"https://github.com/example/sorus-theme", []string{
			"https://github.com/example/sorus-theme/archive/refs/heads/main.zip",
			"https://github.com/example/sorus-theme/archive/refs/heads/master.zip",
		}, "sorus-theme"},
		{"https://github.com/example/sorus-theme.git", []string{
			"https://github.com/example/sorus-theme/archive/refs/heads/main.zip",
			"https://github.com/example/sorus-theme/archive/refs/heads/master.zip",
		}, "sorus-theme"},
		{" https://GitHub.com/example/sorus-theme/ ", []string{
			"https://github.com/example/sorus-theme/archive/refs/heads/main.zip",
			"https://github.com/example/sorus-theme/archive/refs/heads/master.zip",
		}, "sorus-theme"},
		{"https://gitlab.com/group/sub/my-theme", []string{
			"https://gitlab.com/group/sub/my-theme/-/archive/main/my-theme-main.zip",
			"https://gitlab.com/group/sub/my-theme/-/archive/master/my-theme-master.zip",
		}, "my-theme"},
		{"https://example.com/dl/corp-theme.zip?token=1", []string{"https://example.com/dl/corp-theme.zip?token=1"}, "corp-theme"},
	} {
		got, hint, err := resolveArchiveURLs(tc.in)
		if err != nil {
			t.Errorf("resolveArchiveURLs(%q): %v", tc.in, err)
			continue
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") || hint != tc.hint {
			t.Errorf("resolveArchiveURLs(%q) = %v, %q; want %v, %q", tc.in, got, hint, tc.want, tc.hint)
		}
	}
	for _, in := range []string{
		"http://github.com/example/repo",       // https のみ
		"ftp://example.com/theme.zip",          // https のみ
		"https://user:pass@example.com/t.zip",  // ユーザー情報
		"https://github.com/example",           // リポジトリ名が無い
		"https://github.com/example/repo/tree", // 余分なパス
		"https://github.com/example/..",        // パストラバーサル
		"https://example.com/example/repo",     // 知らないホスト
		"not a url at all",
		"",
	} {
		if _, _, err := resolveArchiveURLs(in); err == nil {
			t.Errorf("resolveArchiveURLs(%q) succeeded, want error", in)
		}
	}
}

func TestInstallFromURLRejectsPrivateTargets(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the blocked server received a request: %s", r.URL)
	}))
	defer srv.Close()
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
	ctx := context.Background()
	for _, raw := range []string{
		srv.URL + "/theme.zip",                     // https://127.0.0.1:port（IP の事前検査）
		"https://localhost:" + port + "/theme.zip", // 名前解決後の接続時の検査
		"https://192.168.1.1/theme.zip",            // プライベート
		"https://169.254.169.254/latest/theme.zip", // メタデータ
		"http://example.com/theme.zip",             // http は不可
	} {
		dest := t.TempDir()
		_, err := InstallFromURL(ctx, raw, dest)
		if !errors.Is(err, ErrBlockedURL) {
			t.Errorf("InstallFromURL(%s) = %v, want ErrBlockedURL", raw, err)
		}
		assertClean(t, dest)
	}
}

func TestSafeClientBlocksAtDial(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the blocked server received a request: %s", r.URL)
	}))
	defer srv.Close()
	// URL の事前検査を通さずに、接続時の検査だけで拒むことを確かめる（DNS リバインディング対策の要）。
	client := newSafeClient(CheckPublicAddr)
	u, _ := url.Parse(srv.URL)
	for _, host := range []string{u.Host, "localhost:" + u.Port()} {
		resp, err := client.Get("https://" + host + "/")
		if err == nil {
			resp.Body.Close()
			t.Errorf("GET %s succeeded", host)
			continue
		}
		if !errors.Is(err, ErrBlockedURL) {
			t.Errorf("GET %s = %v, want ErrBlockedURL", host, err)
		}
	}
}

// loopbackOnly は試験用の接続先検査。httptest のサーバー（ループバック）だけを許し、ほかは本番と同じ検査にかける。
func loopbackOnly(a netip.Addr) error {
	if a.Unmap().IsLoopback() {
		return nil
	}
	return CheckPublicAddr(a)
}

// testClient は httptest の TLS サーバーを信頼し、ループバックへの接続だけを許す安全なクライアントを返す。
func testClient(srv *httptest.Server) *http.Client {
	c := newSafeClient(loopbackOnly)
	c.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return c
}

func TestInstallFromURLDownloadsAndInstalls(t *testing.T) {
	archive := testZip(t, validTheme("corp-theme-main/")...)
	mux := http.NewServeMux()
	mux.HandleFunc("/theme.zip", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/redirect.zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/theme.zip", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	for _, p := range []string{"/theme.zip", "/redirect.zip"} {
		dest := t.TempDir()
		raw := srv.URL + p
		th, err := installFromURL(context.Background(), testClient(srv), loopbackOnly, raw, dest)
		if err != nil {
			t.Fatalf("install %s: %v", p, err)
		}
		if th.ID != "corp-theme" || th.SourceURL != raw || !th.IsCustom {
			t.Errorf("installed theme = %+v", th)
		}
		reloaded, err := ValidateThemeDir(os.DirFS(dest), "corp-theme")
		if err != nil || reloaded.SourceURL != raw {
			t.Errorf("theme.json does not record source_url: %+v, %v", reloaded, err)
		}
	}
}

func TestInstallFromURLRejectsUnsafeRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/to-http.zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/theme.zip", http.StatusFound)
	})
	mux.HandleFunc("/to-private.zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://192.168.1.1/theme.zip", http.StatusFound)
	})
	mux.HandleFunc("/to-metadata.zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/theme.zip", http.StatusFound)
	})
	mux.HandleFunc("/loop.zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop.zip", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	for _, p := range []string{"/to-http.zip", "/to-private.zip", "/to-metadata.zip", "/loop.zip"} {
		dest := t.TempDir()
		_, err := installFromURL(context.Background(), testClient(srv), loopbackOnly, srv.URL+p, dest)
		if !errors.Is(err, ErrBlockedURL) {
			t.Errorf("install %s = %v, want ErrBlockedURL", p, err)
		}
		assertClean(t, dest)
	}
}

func TestDownloadLimitsAndFallback(t *testing.T) {
	big := bytes.Repeat([]byte("x"), MaxArchiveSize+1)
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.zip", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("zip")) })
	mux.HandleFunc("/big-length.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		w.Write(big)
	})
	mux.HandleFunc("/big-chunked.zip", func(w http.ResponseWriter, r *http.Request) {
		// Content-Length を付けずに送り、読みながら上限で打ち切ることを確かめる。
		for i := 0; i < len(big); i += 1 << 20 {
			w.Write(big[i:min(i+1<<20, len(big))])
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/error.zip", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	client, ctx := testClient(srv), context.Background()

	for _, p := range []string{"/big-length.zip", "/big-chunked.zip"} {
		if _, _, err := download(ctx, client, loopbackOnly, srv.URL+p); !errors.Is(err, ErrArchiveTooLarge) {
			t.Errorf("download %s = %v, want ErrArchiveTooLarge", p, err)
		}
	}
	// main が 404 なら master を試す。
	data, err := downloadFirst(ctx, client, loopbackOnly, []string{srv.URL + "/missing.zip", srv.URL + "/ok.zip"})
	if err != nil || string(data) != "zip" {
		t.Errorf("fallback = %q, %v", data, err)
	}
	// 404 以外の失敗では次の候補へ進まない。
	if _, err := downloadFirst(ctx, client, loopbackOnly, []string{srv.URL + "/error.zip", srv.URL + "/ok.zip"}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("non-404 failure = %v, want the 500 error", err)
	}
	if _, err := downloadFirst(ctx, client, loopbackOnly, []string{srv.URL + "/missing.zip"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("all missing = %v, want the 404 error", err)
	}
	// 中断されたコンテキストではダウンロードしない。
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := download(cctx, client, loopbackOnly, srv.URL+"/ok.zip"); err == nil {
		t.Error("download with a cancelled context succeeded")
	}
}
