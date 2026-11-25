package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"sorus/internal/theme"
)

// ErrPrivateAddress は、接続先がインターネットに公開されていないアドレス（ループバック・私有・リンクローカル・
// クラウドのメタデータ 169.254.169.254 など）だったため接続しなかったことを表す。Config.DenyPrivateNetwork が
// true のクライアント（サーバーモードで、管理画面から入力したベース URL に接続するもの）が返す。
var ErrPrivateAddress = errors.New("ai: connection to a non-public address is not allowed")

// maxPublicOnlyRedirects は DenyPrivateNetwork のクライアントがたどるリダイレクトの上限。
const maxPublicOnlyRedirects = 5

// checkPublicAddr は addr がインターネットに公開されたアドレスでなければ ErrPrivateAddress を包んだエラーを返す。
// 判定はテーマの取り込みや公開先のヘルスチェックと同じ theme.CheckPublicAddr を使う（IPv4 射影・NAT64 に埋め込んだ
// 非公開の IPv4・ゾーン付きの表記なども断る）。
func checkPublicAddr(addr netip.Addr) error {
	if err := theme.CheckPublicAddr(addr); err != nil {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, addr)
	}
	return nil
}

// CheckPublicHost は URL のホスト名（url.URL.Hostname() の値。ポートを含まない）が localhost（*.localhost を含む）か、
// インターネットに公開されていない IP アドレスなら、ErrPrivateAddress を包んだエラーを返す。ほかの名前はここでは
// 解決しない（DenyPrivateNetwork のクライアントが、接続のたびに解決したアドレスを調べる）。
func CheckPublicHost(host string) error {
	return checkHost(host, checkPublicAddr)
}

// checkHost は CheckPublicHost の本体。IP アドレスの判定 check を差し替えられる（試験用）。
func checkHost(host string, check func(netip.Addr) error) error {
	h := strings.TrimSuffix(strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]")), ".")
	if h == "" {
		return fmt.Errorf("%w: empty host", ErrPrivateAddress)
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, h)
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return check(addr)
	}
	return nil
}

// defaultHTTPClient は cfg.HTTPClient が無いときに使うクライアントを作る。タイムアウトは cfg.Timeout（既定は
// DefaultTimeout）。cfg.DenyPrivateNetwork なら、公開されていないアドレスへは接続しないクライアントにする。
func defaultHTTPClient(cfg Config) *http.Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if cfg.DenyPrivateNetwork {
		return newPublicOnlyClient(timeout, checkPublicAddr)
	}
	return &http.Client{Timeout: timeout}
}

// newPublicOnlyClient は、check を通ったアドレスにだけ接続する HTTP クライアントを返す（SSRF を防ぐ）。
//
//   - 接続は net.Dialer の ControlContext で、名前を解決した後、実際に接続するアドレスごとに check を通す。
//     確かめてから名前を解決し直さないため、解決のたびに向き先を変える DNS リバインディングでも私有アドレスへ
//     接続させない。
//   - 環境変数のプロキシ（HTTPS_PROXY など）は使わない（プロキシ越しでは接続先のアドレスを確かめられないため）。
//   - リダイレクトは maxPublicOnlyRedirects 回までに限り、リダイレクト先が localhost・公開されていない IP アドレスなら
//     接続する前に断る（名前のリダイレクト先も、接続するときに ControlContext で確かめる）。
func newPublicOnlyClient(timeout time.Duration, check func(netip.Addr) error) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: unexpected dial address %q", ErrPrivateAddress, address)
			}
			return check(ap.Addr())
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxPublicOnlyRedirects {
				return fmt.Errorf("ai: stopped after %d redirects", maxPublicOnlyRedirects)
			}
			return checkHost(req.URL.Hostname(), check)
		},
	}
}
