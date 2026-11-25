package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"sorus/internal/auth"
)

// publicDialer は名前を接続ごとに1回だけ解決し、確かめたアドレスにだけ接続する。解決し直した結果が私有アドレスに
// 変わっても（DNS リバインディング）、そこへは接続しない。
func TestPublicDialerPinsResolvedAddress(t *testing.T) {
	var lookups atomic.Int32
	var mu sync.Mutex
	var dialed []string
	d := publicDialer{
		lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host != "rebind.example" {
				t.Errorf("lookup(%q)", host)
			}
			// 1回目は公開されたアドレス、2回目からはループバックを返す。
			if lookups.Add(1) == 1 {
				return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		dial: func(_ context.Context, _, address string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, address)
			mu.Unlock()
			return nil, errors.New("test: no connection")
		},
	}
	ctx := context.Background()
	if _, err := d.DialContext(ctx, "tcp", "rebind.example:443"); err == nil || errors.Is(err, errNonPublicTarget) {
		t.Fatalf("first dial: err = %v, want the test dial error", err)
	}
	if n := lookups.Load(); n != 1 || !reflect.DeepEqual(dialed, []string{"93.184.216.34:443"}) {
		t.Fatalf("first dial: %d lookups, dialed %v; want 1 lookup and only the checked address", n, dialed)
	}
	// 次の接続で名前がループバックに変わっていれば、どこにも接続しない。
	if _, err := d.DialContext(ctx, "tcp", "rebind.example:443"); !errors.Is(err, errNonPublicTarget) {
		t.Fatalf("rebound dial: err = %v, want errNonPublicTarget", err)
	}
	if len(dialed) != 1 {
		t.Fatalf("rebound dial connected to %v", dialed[1:])
	}

	// 公開されたアドレスと私有アドレスが混ざった答えも断る。
	mixed := d
	mixed.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("10.0.0.5")}}, nil
	}
	if _, err := mixed.DialContext(ctx, "tcp", "mixed.example:80"); !errors.Is(err, errNonPublicTarget) || len(dialed) != 1 {
		t.Fatalf("mixed answer: err = %v, dialed %v", err, dialed)
	}

	// IP アドレスそのもの（名前の解決は行われない）。
	literal := publicDialer{dial: d.dial}
	for _, host := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.10", "172.16.0.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1"} {
		if _, err := literal.DialContext(ctx, "tcp", net.JoinHostPort(host, "80")); !errors.Is(err, errNonPublicTarget) {
			t.Errorf("%s: err = %v, want errNonPublicTarget", host, err)
		}
	}
	if len(dialed) != 1 {
		t.Fatalf("non-public literals were dialed: %v", dialed[1:])
	}
}

// 公開サーバーが返したサイトの URL が公開されたアドレスでも、そこからの転送（リダイレクト）は追わない。転送先が
// この PC 内でも接続せず、確かめられなかったと示す。
func TestSiteAccessProbeDoesNotFollowRedirects(t *testing.T) {
	env, adm := newRemoteSiteAccessEnv(t)
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
		_, _ = io.WriteString(w, "internal")
	}))
	t.Cleanup(internal.Close)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
	}))
	t.Cleanup(site.Close)
	siteAddr := strings.TrimPrefix(site.URL, "http://")

	// www.example.test は公開されたアドレスに解決されたことにし、実際には試験のサーバーへつなぐ。
	var lookups atomic.Int32
	env.local.server.siteAccessDialer = publicDialer{
		lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			lookups.Add(1)
			if host != "www.example.test" {
				return nil, errors.New("unexpected lookup " + host)
			}
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "93.184.216.34:80" {
				return nil, errors.New("unexpected dial " + address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, siteAddr)
		},
	}
	if _, err := env.remote.db.Exec(`INSERT INTO settings (key, value) VALUES ('base_url', 'http://www.example.test') ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}
	owner := env.remote.token("check", auth.RoleAdmin)
	expectOK(t, env.remote.setSiteAccess(owner, SyncSiteAccessUpdate{Enabled: true, User: remoteAccessUser, Password: remoteAccessPass, ExpectedRevision: revPtr("")}))

	st := decodeSiteAccessReply(t, expectOK(t, adm.getJSON("/admin/remote/site-access", nil)))
	if n := internalHits.Load(); n != 0 {
		t.Fatalf("the redirect was followed to an internal address (%d hits)", n)
	}
	if lookups.Load() == 0 {
		t.Fatal("the site URL was not opened through the public-only dialer")
	}
	redirected := 0
	for _, c := range st.Checks {
		if !strings.HasPrefix(c.URL, "http://www.example.test/") {
			continue
		}
		redirected++
		if c.Result != siteAccessCheckUnknown || c.Status != http.StatusFound || !strings.Contains(c.Label, "転送") {
			t.Errorf("redirected check %+v, want unknown (redirect not followed)", c)
		}
	}
	if redirected == 0 || st.Status != siteAccessStatusUnverified || st.Tone != "warn" {
		t.Fatalf("redirecting site: status %q tone %q checks %+v, want unverified", st.Status, st.Tone, st.Checks)
	}
}

// 確かめる URL は、接続先として設定した公開サーバーと、公開サーバーが返したサイトの URL のトップページと代表のパス。
// サイトの URL は設定した公開サーバーと同じオリジンでなければ公開されたアドレスだけに接続し、ほかのホストを指す
// パスや形式の正しくない URL は開かない。
func TestSiteAccessTargets(t *testing.T) {
	cfg := remoteConfig{URL: "https://media.example.jp"}
	paths := []string{"/feed.xml", "//evil.example/x", "http://evil.example/", "/a/../b", "/x?y=1", "/sp ace", `/back\slash`,
		"/articles/%E3%81%82", "/feed.xml", "/uploads/0123456789abcdef0123456789abcdef-800.webp", "/fourth"}
	got := siteAccessTargets(cfg, SyncSiteAccessResponse{SiteURL: "https://WWW.example.jp/blog/", CheckPaths: paths})
	want := []siteAccessTarget{
		{URL: "https://media.example.jp/"},
		{URL: "https://media.example.jp/feed.xml"},
		{URL: "https://media.example.jp/articles/%E3%81%82"},
		{URL: "https://media.example.jp/uploads/0123456789abcdef0123456789abcdef-800.webp"},
		{URL: "https://www.example.jp/blog/", Public: true},
		{URL: "https://www.example.jp/blog/feed.xml", Public: true},
		{URL: "https://www.example.jp/blog/articles/%E3%81%82", Public: true},
		{URL: "https://www.example.jp/blog/uploads/0123456789abcdef0123456789abcdef-800.webp", Public: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets =\n%+v\nwant\n%+v", got, want)
	}

	// 設定した公開サーバーと同じオリジン（パスだけ違う）なら、設定した接続先として扱う。同じ URL なら重ねない。
	for site, wantLen := range map[string]int{"https://MEDIA.example.jp:443/news": 2, "https://media.example.jp/": 1, "https://media.example.jp": 1} {
		got := siteAccessTargets(cfg, SyncSiteAccessResponse{SiteURL: site})
		if len(got) != wantLen {
			t.Errorf("site %s: targets %+v, want %d", site, got, wantLen)
		}
		for _, tg := range got {
			if tg.Public || tg.Invalid {
				t.Errorf("site %s: target %+v must be the configured server", site, tg)
			}
		}
	}
	// http やポート違いは別のオリジン（公開されたアドレスだけ）。
	for _, site := range []string{"http://media.example.jp", "https://media.example.jp:8443"} {
		got := siteAccessTargets(cfg, SyncSiteAccessResponse{SiteURL: site})
		if len(got) != 2 || !got[1].Public {
			t.Errorf("site %s: targets %+v, want a public-only target", site, got)
		}
	}
	// 形式の正しくない URL は開かず、確かめていない URL として示す（資格情報は画面に出さない）。
	for site, shown := range map[string]string{
		"ftp://files.example.jp/":        "ftp://files.example.jp/",
		"javascript:alert(1)":            "javascript:alert(1)",
		"https://user:pw@www.example.jp": "https://www.example.jp",
		"https:///no-host":               "https:///no-host",
	} {
		got := siteAccessTargets(cfg, SyncSiteAccessResponse{SiteURL: site})
		if len(got) != 2 || !got[1].Invalid || got[1].URL != shown {
			t.Errorf("site %s: targets %+v, want one invalid target shown as %q", site, got, shown)
		}
	}
	check := probeSiteAccessURL(context.Background(), "ja", siteAccessProbeClient, siteAccessTarget{URL: "javascript:alert(1)", Invalid: true})
	if check.Result != siteAccessCheckUnknown || !strings.Contains(check.Label, "確かめていません") {
		t.Errorf("invalid target check = %+v", check)
	}
}
