package desktop

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// HostGuard は、Host ヘッダーが 127.0.0.1:<port> か localhost:<port> の要求だけを next に渡し、それ以外を 403 で
// 拒否する。悪意のあるサイトが自分のドメインを 127.0.0.1 に向ける DNS リバインディングで、ブラウザから
// 手元の sorus を操作するのを防ぐ（その要求の Host は攻撃者のドメインになる）。
func HostGuard(port int, next http.Handler) http.Handler {
	p := strconv.Itoa(port)
	allowed := map[string]bool{
		net.JoinHostPort(ListenHost, p):  true,
		net.JoinHostPort("localhost", p): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "Forbidden: this sorus only accepts requests addressed to "+ListenHost+":"+p, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
