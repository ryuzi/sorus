package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"sorus/internal/theme"
)

// errNonPublicTarget は接続先の名前が、インターネットに公開されていないアドレス（ループバック・私有・リンクローカル
// など。publicIP を参照）に解決されたため、接続しなかったときのエラー。
var errNonPublicTarget = errors.New("non-public address")

// publicIP は ip がインターネットに公開されたアドレス（外部から指定された URL の接続先として許すアドレス）かを返す。
// 判定はテーマの取り込みの SSRF の判定（theme.CheckPublicAddr）と同じで、ループバック・私有・リンクローカル・CGNAT・
// 文書用・予約の範囲に加え、NAT64 に埋め込んだ非公開の IPv4、6to4・Teredo、IPv6 のグローバルユニキャスト（2000::/3）の
// 外の範囲（IPv4 互換アドレスなど）も断る。
func publicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	return ok && theme.CheckPublicAddr(addr) == nil
}

// publicDialer は、インターネットに公開されたアドレス（publicIP）にだけ接続する。外部から指定された URL
// （公開先のヘルスチェック、公開サーバーが返したサイトの URL）に接続するときに使い、この PC 内や社内のサービスへ
// 接続させない（SSRF を防ぐ）。
//
// 名前の解決は接続ごとに1回だけ行い、解決したアドレスがすべて公開されたアドレスかを確かめてから、そのアドレスに
// 直接接続する。確かめた後に名前を解決し直さないため、DNS リバインディングで私有アドレスへ向け直されない。
// http.Transport の DialContext に渡し、Transport の Proxy は nil にする（プロキシ越しでは接続先を確かめられない）。
// TLS の検証は Transport が URL のホスト名で行う。
type publicDialer struct {
	// timeout は1回の接続の制限時間（0 なら制限しない。dial を差し替えたときは使わない）。
	timeout time.Duration
	// lookup・dial・allow は試験で差し替える。nil なら net.DefaultResolver・net.Dialer・publicIP を使う。
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
	allow  func(net.IP) bool
}

// resolve は host を1回だけ解決し、解決したアドレスがすべて公開されたアドレスなら返す。どれか1つでも公開されて
// いなければ errNonPublicTarget を返す（混ざった答えは全部断る）。名前の解決の失敗はそのまま返す。
func (d publicDialer) resolve(ctx context.Context, host string) ([]net.IPAddr, error) {
	lookup := d.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	resolved, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, errors.New("no addresses returned by DNS lookup")
	}
	allow := d.allow
	if allow == nil {
		allow = publicIP
	}
	for _, addr := range resolved {
		if !allow(addr.IP) {
			return nil, errNonPublicTarget
		}
	}
	return resolved, nil
}

// DialContext は address（ホスト:ポート）のホストを解決し、公開されたアドレスだけに接続する。解決したアドレスの
// どれか1つでも公開されていなければ、どこにも接続せずに errNonPublicTarget を返す。
func (d publicDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("public dial: %w", err)
	}
	resolved, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dial := d.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: d.timeout}).DialContext
	}
	var last error
	for _, addr := range resolved {
		conn, err := dial(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
