package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sorus/internal/mcp"
)

// 送り元のアドレスは、Config.TrustedProxies に含まれる接続元から届いたときだけ X-Forwarded-For を使う。未設定なら、
// この PC 自身や LAN のアドレスからの接続でも X-Forwarded-For を無視し、書き換えて回数の制限を逃れられない。
// 環境変数 TRUSTED_PROXIES は直接読まない（どの接続元も信頼する値にしても、結果は変わらない）。
func TestOAuthClientKey(t *testing.T) {
	t.Setenv(mcp.TrustedProxiesEnv, "0.0.0.0/0,::/0")
	for _, tc := range []struct {
		name    string
		trusted string
		remote  string
		xff     []string
		want    string
	}{
		{"direct", "", "203.0.113.5:4000", nil, "203.0.113.5"},
		{"direct spoofed xff", "", "203.0.113.5:4000", []string{"198.51.100.1"}, "203.0.113.5"},
		// 既定ではループバック・私設のアドレスからの接続でも X-Forwarded-For を信頼しない。
		{"loopback untrusted", "", "127.0.0.1:5000", []string{"10.9.9.9, 198.51.100.7"}, "127.0.0.1"},
		{"private untrusted", "", "192.168.1.10:5000", []string{"1.1.1.1", "198.51.100.8"}, "192.168.1.10"},
		{"ipv6 loopback", "", "[::1]:5000", []string{"198.51.100.9"}, "::1"},
		// 信頼するプロキシからなら、後ろから見て信頼するプロキシでない最初のアドレス（クライアントが書いた前の値は使わない）。
		{"trusted proxy", "127.0.0.1", "127.0.0.1:5000", []string{"10.9.9.9, 198.51.100.7"}, "198.51.100.7"},
		{"trusted cidr, multiple headers", "192.168.0.0/16", "192.168.1.10:5000", []string{"1.1.1.1", "198.51.100.8"}, "198.51.100.8"},
		{"chain of trusted proxies", "127.0.0.1,10.0.0.0/8", "127.0.0.1:5000", []string{"6.6.6.6, 198.51.100.7, 10.1.2.3"}, "198.51.100.7"},
		{"all hops trusted", "127.0.0.1,10.0.0.0/8", "127.0.0.1:5000", []string{"10.0.0.2, 10.0.0.3"}, "10.0.0.2"},
		{"trusted but no xff", "127.0.0.1", "127.0.0.1:5000", nil, "127.0.0.1"},
		{"trusted but invalid xff", "127.0.0.1", "127.0.0.1:5000", []string{"not-an-ip"}, "127.0.0.1"},
		{"trusted, invalid hop in chain", "127.0.0.1", "127.0.0.1:5000", []string{"198.51.100.7, bogus"}, "127.0.0.1"},
		{"ipv4-mapped remote", "127.0.0.1", "[::ffff:127.0.0.1]:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		// 信頼するプロキシ以外の接続元は、同じ LAN でも X-Forwarded-For を使わない。
		{"other lan host", "192.168.1.1", "192.168.1.10:5000", []string{"198.51.100.8"}, "192.168.1.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/oauth/token", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := oauthClientKey(r, testProxies(t, tc.trusted)); got != tc.want {
				t.Errorf("oauthClientKey(%s, %v) with trusted proxies %q = %q, want %q", tc.remote, tc.xff, tc.trusted, got, tc.want)
			}
		})
	}
}

// 管理画面のログインの送り元ごとの数は、IPv6 を /64 にまとめる（アドレスを変えて上限を逃れられない）。
func TestAdminLoginClientKey(t *testing.T) {
	for remote, want := range map[string]string{
		"203.0.113.5:4000":            "203.0.113.5",
		"[2001:db8:1:2:3:4:5:6]:4000": "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::1]:4000": "2001:db8:1:2::/64",
		"[::ffff:203.0.113.5]:4000":   "203.0.113.5",
	} {
		r := httptest.NewRequest(http.MethodPost, "/admin/login", nil)
		r.RemoteAddr = remote
		if got := adminLoginClientKey(r, mcp.TrustedProxies{}); got != want {
			t.Errorf("adminLoginClientKey(%s) = %q, want %q", remote, got, want)
		}
	}
}

// 記録の数が上限に達したら、区切りの過ぎた記録を先に消し、全部は消さない。
func TestRateLimiterEvictsExpiredFirst(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	l := newRateLimiter(1, time.Minute)
	l.maxKeys = 4
	l.setClock(func() time.Time { return now })
	l.hit("old1")
	l.hit("old2")
	now = now.Add(2 * time.Minute)
	l.hit("blocked")
	if _, ok := l.hit("blocked"); ok {
		t.Fatal("second hit of blocked was accepted")
	}
	l.hit("live")
	l.hit("new") // 上限（4）に達している。区切りの過ぎた old1・old2 だけを消す。
	if _, ok := l.entries["old1"]; ok {
		t.Error("expired entry old1 was kept")
	}
	if _, ok := l.entries["old2"]; ok {
		t.Error("expired entry old2 was kept")
	}
	for _, k := range []string{"blocked", "live", "new"} {
		if _, ok := l.entries[k]; !ok {
			t.Errorf("live entry %q was evicted", k)
		}
	}
	if _, blocked := l.blocked("blocked"); !blocked {
		t.Error("blocked key is no longer blocked after eviction")
	}
}

// 区切りの過ぎた記録が無くても上限に達したら、数の少ない（同じなら古い）記録から消し、上限に達した記録は残す。
// 新しい送り元を大量に作っても、止められている送り元の記録は押し出せない。
func TestRateLimiterEvictsLowestCountFirst(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	l := newRateLimiter(3, 10*time.Minute)
	l.maxKeys = 20
	l.setClock(func() time.Time { return now })
	for range 4 {
		l.hit("attacker")
	}
	if _, blocked := l.blocked("attacker"); !blocked {
		t.Fatal("attacker is not blocked")
	}
	l.hit("busy")
	l.hit("busy")
	for i := range 1000 {
		now = now.Add(time.Millisecond)
		l.hit(fmt.Sprintf("spray-%d", i))
		if len(l.entries) > l.maxKeys {
			t.Fatalf("entries = %d, want <= %d", len(l.entries), l.maxKeys)
		}
	}
	if _, blocked := l.blocked("attacker"); !blocked {
		t.Error("attacker was evicted by spraying new keys")
	}
	if e := l.entries["busy"]; e == nil || e.count != 2 {
		t.Errorf("busy entry = %+v, want count 2 kept", e)
	}
	// 最も新しい送り元は残る（古いものから消す）。
	if _, ok := l.entries["spray-999"]; !ok {
		t.Error("the newest entry was evicted")
	}
	if _, ok := l.entries["spray-0"]; ok {
		t.Error("the oldest single-hit entry was kept")
	}
}

func TestRateLimiterReset(t *testing.T) {
	l := newRateLimiter(1, time.Minute)
	l.hit("a")
	l.hit("a")
	if _, blocked := l.blocked("a"); !blocked {
		t.Fatal("a is not blocked")
	}
	l.reset("a")
	if _, blocked := l.blocked("a"); blocked {
		t.Error("a is still blocked after reset")
	}
}

// 同意画面は送り元ごとに加えて全体の回数も制限する（多数の送り元からの総当たりを抑える）。
func TestAllowAuthorizeGlobalLimit(t *testing.T) {
	l := newOAuthRateLimits(mcp.TrustedProxies{})
	l.authorizeGlobal.setLimit(2)
	req := func(remote string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/oauth/authorize", nil)
		r.RemoteAddr = remote
		return r
	}
	for i, remote := range []string{"203.0.113.1:1", "203.0.113.2:1"} {
		if _, ok := l.allowAuthorize(req(remote)); !ok {
			t.Fatalf("request %d refused", i+1)
		}
	}
	retry, ok := l.allowAuthorize(req("203.0.113.3:1"))
	if ok || retry <= 0 {
		t.Errorf("third request from a new address = %v, %v; want refused by the global limit", retry, ok)
	}
	if oauthAuthorizeGlobal <= oauthAuthorizePerClient {
		t.Errorf("global limit %d must be larger than the per-client limit %d", oauthAuthorizeGlobal, oauthAuthorizePerClient)
	}
}

// testProxies は TRUSTED_PROXIES の書式（カンマ区切り）の list を、New と同じく ParseTrustedProxies で解析する。
func testProxies(t *testing.T, list string) mcp.TrustedProxies {
	t.Helper()
	prefixes, err := ParseTrustedProxies(strings.Split(list, ","))
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", list, err)
	}
	return mcp.NewTrustedProxies(prefixes...)
}
