package importer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// 既定のクライアントは theme の SSRF 判定と同じ範囲（CGNAT・NAT64・マルチキャスト・Teredo・6to4 など）を拒む。
func TestCheckPublicIPBlocksSpecialRanges(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "0.1.2.3", // 0.0.0.0/8
		"100.64.0.1", "100.127.255.254", // CGNAT
		"64:ff9b::7f00:1", "64:ff9b::a00:1", // NAT64 に埋め込まれたループバック・私有 IPv4
		"224.0.0.1", "239.255.255.250", // IPv4 マルチキャスト
		"ff02::1", "ff0e::1", // IPv6 マルチキャスト
		"2001::1",                 // Teredo
		"2002:7f00:1::1",          // 6to4
		"fec0::1",                 // サイトローカル
		"fc00::1", "fd12:3456::1", // ユニークローカル
		"fe80::1",                      // リンクローカル
		"::ffff:127.0.0.1",             // IPv4 射影のループバック
		"::ffff:169.254.169.254",       // IPv4 射影のメタデータ
		"::127.0.0.1",                  // IPv4 互換（廃止）
		"240.0.0.1", "255.255.255.255", // 予約・ブロードキャスト
		"198.18.0.1", "192.0.0.8", // ベンチマーク・IETF
	}
	for _, s := range blocked {
		if err := checkPublicIP(netip.MustParseAddr(s)); !errors.Is(err, errBlockedURL) {
			t.Errorf("checkPublicIP(%s) = %v, want errBlockedURL", s, err)
		}
		if !isPrivateIP(net.ParseIP(s)) {
			t.Errorf("isPrivateIP(%s) = false, want true", s)
		}
	}
	if err := checkPublicIP(netip.MustParseAddr("fe80::1%eth0")); err == nil {
		t.Error("zoned address must be blocked")
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::808:808"} {
		if err := checkPublicIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("checkPublicIP(%s) = %v, want nil", s, err)
		}
	}
	if !isPrivateIP(nil) {
		t.Error("isPrivateIP(nil) must be true")
	}
}

func TestCheckImageURL(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"https://example.com/a.png", true},
		{"http://example.com/a.png", true},
		{"https://8.8.8.8/a.png", true},
		{"ftp://example.com/a.png", false},
		{"file:///etc/passwd", false},
		{"gopher://example.com/", false},
		{"https://user:pass@example.com/a.png", false},
		{"https://user@example.com/a.png", false},
		{"http://127.0.0.1/a.png", false},
		{"http://[::1]/a.png", false},
		{"http://100.64.0.1/a.png", false},
		{"http://[64:ff9b::a9fe:a9fe]/latest/meta-data", false}, // NAT64 経由の 169.254.169.254
		{"http://0.0.0.0:8080/", false},
		{"http:///a.png", false},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if err := checkImageURL(u); (err == nil) != tc.ok {
			t.Errorf("checkImageURL(%q) = %v, want ok=%v", tc.raw, err, tc.ok)
		}
	}
	if err := checkImageURL(nil); err == nil {
		t.Error("checkImageURL(nil) must fail")
	}
}

// リダイレクト先もスキーム・ユーザー情報・非公開 IP リテラル・回数を調べる。
func TestCheckRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	via := []*http.Request{req("https://example.com/start")}
	for _, raw := range []string{
		"http://127.0.0.1/x",
		"http://169.254.169.254/latest/meta-data",
		"http://[fd00::1]/x",
		"https://user:pass@example.com/x",
		"ftp://example.com/x",
		"http://100.100.100.200/x",
	} {
		if err := checkRedirect(req(raw), via); err == nil {
			t.Errorf("checkRedirect(%s) = nil, want error", raw)
		}
	}
	if err := checkRedirect(req("https://cdn.example.com/x.png"), via); err != nil {
		t.Errorf("redirect to public host rejected: %v", err)
	}
	many := make([]*http.Request, maxRedirects)
	for i := range many {
		many[i] = req("https://example.com/")
	}
	if err := checkRedirect(req("https://example.com/x"), many); err == nil {
		t.Error("too many redirects must be rejected")
	}
}

// 既定の Transport は環境変数のプロキシを使わず、接続の直前にアドレスを調べる。
func TestDefaultSecureTransportNoProxyAndDialCheck(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tr := defaultSecureTransport()
	if tr.Proxy != nil {
		t.Fatal("Proxy must be nil")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("loopback server must not be reached")
	}))
	defer srv.Close()
	// 名前（localhost）で指定しても、解決後のアドレスで拒む。
	u, _ := url.Parse(srv.URL)
	target := "http://localhost:" + u.Port() + "/a.png"
	_, err := (&http.Client{Transport: tr}).Get(target)
	if err == nil || !errors.Is(err, errBlockedURL) {
		t.Fatalf("Get(%s) = %v, want errBlockedURL", target, err)
	}
}

// 既定のクライアントは、ユーザー情報付き・非公開 IP リテラルの URL を接続前に拒み、リダイレクトも調べる。
func TestMediaFetcherDefaultClientRejectsBlockedURL(t *testing.T) {
	f := NewMediaFetcher(nil, t.TempDir(), 0)
	for _, raw := range []string{
		"https://user:pass@example.com/a.png",
		"http://[::ffff:127.0.0.1]/a.png",
		"http://100.64.1.1/a.png",
	} {
		_, _, err := f.Fetch(context.Background(), raw)
		if err == nil || !strings.Contains(err.Error(), "blocked URL") {
			t.Errorf("Fetch(%s) = %v, want blocked URL error", raw, err)
		}
	}
	if f.client.CheckRedirect == nil {
		t.Fatal("default client must check redirects")
	}
}
