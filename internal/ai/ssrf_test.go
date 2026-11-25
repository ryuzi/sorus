package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCheckPublicHost は、ベース URL のホストが localhost・公開されていない IP アドレスなら断り、公開された
// IP アドレスと名前（接続時に調べる）は通すことを確かめる。
func TestCheckPublicHost(t *testing.T) {
	blocked := []string{
		"localhost", "LOCALHOST", "localhost.", "ollama.localhost",
		"127.0.0.1", "127.8.9.10", "::1", "[::1]", "0.0.0.0", "::",
		"169.254.169.254", "fe80::1", "fe80::1%eth0",
		"10.0.0.5", "172.16.0.2", "192.168.1.50", "100.64.0.1", "fd00::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe",
		"",
	}
	for _, h := range blocked {
		if err := CheckPublicHost(h); !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("CheckPublicHost(%q) = %v, want ErrPrivateAddress", h, err)
		}
	}
	allowed := []string{"api.openai.com", "api.groq.com", "8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "metadata.example"}
	for _, h := range allowed {
		if err := CheckPublicHost(h); err != nil {
			t.Errorf("CheckPublicHost(%q) = %v, want nil", h, err)
		}
	}
}

// countingServer は OpenAI 互換の応答を返し、受け付けた要求の数を数える試験用のサーバー。
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"地域の図書館で読書会が始まりました\n子ども向け読書会のお知らせ"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// サーバーモード（DenyPrivateNetwork）のカスタム AI は、ループバックのアドレスにも、ループバックに解決される名前
// （localhost。接続時に解決したアドレスを調べる）にも接続しない。DenyPrivateNetwork でなければ（デスクトップ
// モードのローカル LLM）従来どおり接続する。
func TestDenyPrivateNetworkBlocksLoopbackBaseURL(t *testing.T) {
	srv, hits := countingServer(t)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	for _, base := range []string{srv.URL + "/v1", "http://localhost:" + port + "/v1"} {
		for name, newProvider := range map[string]func(Config) (Provider, error){
			"NewProvider": NewProvider,
			"NewCustom":   func(c Config) (Provider, error) { return NewCustom(c), nil },
		} {
			p, err := newProvider(Config{Kind: KindCustom, BaseURL: base, Model: "llama3.1", DenyPrivateNetwork: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.SuggestTitles(t.Context(), "本文")
			if !errors.Is(err, ErrPrivateAddress) {
				t.Errorf("%s %s: err = %v, want ErrPrivateAddress", name, base, err)
			}
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server received %d requests, want 0", n)
	}

	p, err := NewProvider(Config{Kind: KindCustom, BaseURL: srv.URL + "/v1", Model: "llama3.1"})
	if err != nil {
		t.Fatal(err)
	}
	if titles, err := p.SuggestTitles(t.Context(), "本文"); err != nil || len(titles) == 0 {
		t.Fatalf("without DenyPrivateNetwork: titles = %v, err = %v", titles, err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("server received %d requests, want 1", n)
	}
}

// DenyPrivateNetwork のクライアントは環境変数のプロキシを使わない（プロキシ越しでは接続先を確かめられない）。
func TestDenyPrivateNetworkClientIgnoresProxy(t *testing.T) {
	c := defaultHTTPClient(Config{DenyPrivateNetwork: true, Timeout: 3 * time.Second})
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.DialContext == nil {
		t.Fatalf("transport = %#v, want a direct transport with a guarded dialer", c.Transport)
	}
	if c.Timeout != 3*time.Second || c.CheckRedirect == nil {
		t.Fatalf("client timeout = %v, CheckRedirect set = %v", c.Timeout, c.CheckRedirect != nil)
	}
	if plain := defaultHTTPClient(Config{}); plain.Transport != nil || plain.Timeout != DefaultTimeout {
		t.Fatalf("plain client = %#v, want the default transport with DefaultTimeout", plain)
	}
}

// 公開されたサーバーが、クラウドのメタデータ・localhost など公開されていない送り先へリダイレクトしても、
// 接続する前に断る。公開された送り先へのリダイレクトはたどり、回数には上限がある。
// 試験では 127.0.0.1 だけを「公開されたアドレス」とみなす判定に差し替える。
func TestPublicOnlyClientRejectsPrivateRedirects(t *testing.T) {
	loopbackOnly := func(addr netip.Addr) error {
		if addr == netip.MustParseAddr("127.0.0.1") {
			return nil
		}
		return checkPublicAddr(addr)
	}
	var final atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		final.Add(1)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(target.Close)
	port := target.URL[strings.LastIndex(target.URL, ":")+1:]

	var redirectTo atomic.Value
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loop" {
			http.Redirect(w, r, "/loop", http.StatusFound)
			return
		}
		http.Redirect(w, r, redirectTo.Load().(string), http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	client := newPublicOnlyClient(5*time.Second, loopbackOnly)
	get := func(u string) error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
		}
		return err
	}

	for _, to := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:" + port + "/",
		"http://localhost:" + port + "/",
		"http://10.0.0.1/",
	} {
		redirectTo.Store(to)
		start := time.Now()
		err := get(origin.URL + "/start")
		if !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("redirect to %s: err = %v, want ErrPrivateAddress", to, err)
		}
		// 断るのは接続の前（169.254.169.254 などへの接続を試みて時間切れを待つのではない）。
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("redirect to %s took %v; it must be refused before dialing", to, d)
		}
	}
	if n := final.Load(); n != 0 {
		t.Fatalf("redirect target received %d requests, want 0", n)
	}

	redirectTo.Store(target.URL + "/")
	if err := get(origin.URL + "/start"); err != nil || final.Load() != 1 {
		t.Fatalf("redirect to an allowed address: err = %v, hits = %d", err, final.Load())
	}
	if err := get(origin.URL + "/loop"); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("redirect loop: err = %v, want the redirect limit", err)
	}
}

// 接続時の判定は、名前を解決した後の実際の接続先アドレスで行う（ControlContext）。判定を通らないアドレスには
// TCP の接続そのものを始めない。
func TestPublicOnlyClientChecksResolvedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	var (
		mu   sync.Mutex
		seen []netip.Addr
	)
	client := newPublicOnlyClient(3*time.Second, func(addr netip.Addr) error {
		mu.Lock()
		seen = append(seen, addr)
		mu.Unlock()
		return checkPublicAddr(addr)
	})
	res, err := client.Get("http://localhost:" + port + "/")
	if err == nil {
		res.Body.Close()
	}
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("err = %v, want ErrPrivateAddress", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the dial-time check never ran")
	}
	for _, a := range seen {
		if !a.IsLoopback() {
			t.Errorf("checked address %s, want the resolved loopback address", a)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("listener accepted %d connections, want 0", n)
	}
}
