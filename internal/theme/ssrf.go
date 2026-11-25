package theme

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedURL は取り込み元の URL・接続先がプライベートなネットワークなどを指すため拒んだときのエラー。
var ErrBlockedURL = errors.New("theme: blocked URL")

const (
	// fetchTimeout はダウンロード1回（リダイレクトを含む）の上限時間。
	fetchTimeout = 2 * time.Minute
	// maxRedirects はたどるリダイレクトの上限。
	maxRedirects = 5
)

// blockedPrefixes は接続を拒むアドレス範囲。IsPrivate などの判定に加えて、名前で判定できない範囲を並べる。
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // このネットワーク
		"10.0.0.0/8",      // RFC 1918
		"100.64.0.0/10",   // キャリアグレード NAT（RFC 6598）
		"127.0.0.0/8",     // ループバック
		"169.254.0.0/16",  // リンクローカル（クラウドのメタデータ 169.254.169.254 を含む）
		"172.16.0.0/12",   // RFC 1918
		"192.0.0.0/24",    // IETF プロトコル割り当て
		"192.0.2.0/24",    // TEST-NET-1
		"192.168.0.0/16",  // RFC 1918
		"198.18.0.0/15",   // ベンチマーク
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // マルチキャスト
		"240.0.0.0/4",     // 予約（255.255.255.255 を含む）
		"::/128",          // 未指定
		"::1/128",         // ループバック
		"100::/64",        // 破棄専用
		"64:ff9b:1::/48",  // ローカル NAT64（RFC 8215）
		"2001::/32",       // Teredo（RFC 4380）
		"2001:2::/48",     // ベンチマーク（RFC 5180）
		"2002::/16",       // 6to4（RFC 3056）
		"2001:db8::/32",   // 文書用
		"3fff::/20",       // 文書用（RFC 9637）
		"fc00::/7",        // ユニークローカル（RFC 4193）
		"fe80::/10",       // リンクローカル
		"fec0::/10",       // サイトローカル（廃止）
		"ff00::/8",        // マルチキャスト
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	// nat64Prefix は IPv4 アドレスを埋め込む NAT64 の既定の範囲。埋め込まれた IPv4 を調べる。
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// globalUnicast6 は IPv6 のグローバルユニキャストの範囲。IANA がインターネットに割り当てているのはこの中だけで、
	// 外側は IPv4 互換アドレス（::a.b.c.d、廃止）・未割り当て・予約などの範囲。
	globalUnicast6 = netip.MustParsePrefix("2000::/3")
)

// CheckPublicAddr は接続先のアドレスがインターネットに公開されたユニキャストアドレスでなければ、ErrBlockedURL を包んだ
// エラーを返す。外部から指定された URL へ接続するときの SSRF の判定で、テーマの取り込みのほか、公開先のヘルスチェックと、
// 公開サーバーが返したサイトの URL の確認（internal/server の publicDialer）も同じ判定を使う。
//
//   - IPv4 射影アドレス（::ffff:127.0.0.1）は IPv4 として調べる。NAT64 の既定の範囲（64:ff9b::/96）は埋め込まれた
//     IPv4 で調べ、DNS64 の環境で公開された IPv4 へは接続できるようにしつつ、私有・ループバックなどの IPv4 は断る。
//   - ゾーン付きの表記（fe80::1%eth0 など）は断る。netip.Prefix.Contains はゾーン付きのアドレスに常に false を返すため、
//     範囲の判定をすり抜けさせない（公開されたアドレスにゾーンは要らない）。
//   - IPv6 はグローバルユニキャストの範囲（2000::/3）だけを許し、その中でも blockedPrefixes の範囲（6to4・Teredo・
//     文書用など）は断る。
func CheckPublicAddr(addr netip.Addr) error {
	if addr.Zone() != "" {
		return fmt.Errorf("%w: address %s has a zone", ErrBlockedURL, addr)
	}
	addr = addr.Unmap() // ::ffff:127.0.0.1 などの IPv4 射影アドレスは IPv4 として調べる
	if addr.Is6() && nat64Prefix.Contains(addr) {
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("%w: address %s is not a public address", ErrBlockedURL, addr)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("%w: address %s is in the blocked range %s", ErrBlockedURL, addr, p)
		}
	}
	if addr.Is6() && !globalUnicast6.Contains(addr) {
		return fmt.Errorf("%w: address %s is outside the IPv6 global unicast range %s", ErrBlockedURL, addr, globalUnicast6)
	}
	return nil
}

// checkFetchURL は取り込み元の URL の形を調べる。https のみ、ユーザー情報なし、ホスト必須。
// ホストが IP アドレスならその場で ipCheck を通す（名前は接続時に解決したアドレスを調べる）。
func checkFetchURL(u *url.URL, ipCheck func(netip.Addr) error) error {
	if u == nil {
		return fmt.Errorf("%w: empty URL", ErrBlockedURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: only https URLs are allowed (got %q)", ErrBlockedURL, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%w: URLs with user information are not allowed", ErrBlockedURL)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: URL has no host", ErrBlockedURL)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return ipCheck(addr)
	}
	return nil
}

// newSafeClient は SSRF を防ぐ HTTP クライアントを返す。
//
//   - 接続は net.Dialer の ControlContext で、DNS 解決後に実際に接続するアドレスごとに ipCheck を通す。
//     解決と接続の間で名前の向き先を変える DNS リバインディングでも、接続の直前に調べるので防げる。
//   - 環境変数のプロキシは使わない（プロキシ越しでは接続先のアドレスを調べられないため）。
//   - リダイレクト先も checkFetchURL で調べ直し、maxRedirects 回までに限る。
func newSafeClient(ipCheck func(netip.Addr) error) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: unexpected dial address %q", ErrBlockedURL, address)
			}
			return ipCheck(ap.Addr())
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: too many redirects", ErrBlockedURL)
			}
			return checkFetchURL(req.URL, ipCheck)
		},
	}
}
