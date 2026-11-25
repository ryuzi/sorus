package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

const (
	// StaticCacheControl は内容が変わるとファイル名（またはクエリ）が変わる静的アセット・アップロード画像の Cache-Control。
	// CDN とブラウザに永久キャッシュさせる。
	StaticCacheControl = "public, max-age=31536000, immutable"
	// PublicHTMLCacheControl は公開ページの Cache-Control。CDN エッジに最大1時間キャッシュさせ、
	// 期限切れ後も60秒間は古い内容を返しつつ背後で再検証させる。
	PublicHTMLCacheControl = "public, s-maxage=3600, stale-while-revalidate=60"
)

// isStaticAssetPath は不変キャッシュの対象（/static/ と /uploads/ 以下）へのパスかを返す。
func isStaticAssetPath(p string) bool {
	return strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, UploadURLPrefix)
}

// isCacheableMethod は CDN にキャッシュさせてよいメソッド（GET・HEAD）かを返す。
func isCacheableMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead
}

// StaticCacheMiddleware は /static/ と /uploads/ への GET・HEAD の成功応答（200・304）に StaticCacheControl を付ける。
// 404 などの失敗応答を永久キャッシュさせないよう、ステータスが決まった時点で付ける。
func StaticCacheMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			if !isCacheableMethod(r.Method) || !isStaticAssetPath(r.URL.Path) {
				return next(c)
			}
			res := c.Response()
			res.Before(func() {
				if res.Status == http.StatusOK || res.Status == http.StatusNotModified {
					res.Header().Set("Cache-Control", StaticCacheControl)
				}
			})
			return next(c)
		}
	}
}

// isPublicHTMLCachePath は公開ページのキャッシュ対象へのパスかを返す。
// 管理画面・API は利用者ごとに内容が変わるため、静的アセットは StaticCacheMiddleware が扱うため、
// ヘルスチェックは常に最新の状態を返すため、添付資料（最大 30MB）は本文をメモリに溜めないため対象外にする。
// excluded のパス（とその下）も対象外にする。/api の外に置いた同期 API の秘匿パスを渡す。
func isPublicHTMLCachePath(p string, excluded ...string) bool {
	if isAdminPath(p) || p == "/api" || strings.HasPrefix(p, "/api/") || isStaticAssetPath(p) || p == "/healthz" ||
		strings.HasPrefix(p, AttachmentURLPrefix) {
		return false
	}
	for _, x := range excluded {
		if x != "" && (p == x || strings.HasPrefix(p, x+"/")) {
			return false
		}
	}
	return true
}

// PublicHTMLCacheConfig は PublicHTMLCacheMiddlewareWithConfig の設定。
type PublicHTMLCacheConfig struct {
	// Excluded は対象外にするパス（とその下）。/api の外に置いた同期 API の秘匿パスなどを渡す。
	Excluded []string
	// VaryHost が true を返すあいだは、キャッシュさせる応答に Vary: Host を付ける。サイトの URL（BASE_URL）が無く、
	// canonical・OGP などの絶対 URL を Host ヘッダーから組み立てるときに、Host ごとにキャッシュを分けさせ、偽の Host で
	// 作らせたページをほかの訪問者へ返させない。nil なら付けない。
	VaryHost func() bool
}

// PublicHTMLCacheMiddleware は除外パスだけを指定した PublicHTMLCacheMiddlewareWithConfig。
func PublicHTMLCacheMiddleware(excluded ...string) echo.MiddlewareFunc {
	return PublicHTMLCacheMiddlewareWithConfig(PublicHTMLCacheConfig{Excluded: excluded})
}

// PublicHTMLCacheMiddlewareWithConfig は公開ページへの GET・HEAD の 200 応答に PublicHTMLCacheControl と本文の
// ハッシュから作った ETag を付け、If-None-Match が一致すれば本文を返さず 304 Not Modified を返す。
// ハンドラーが Cache-Control を自分で決めた応答は Cache-Control を上書きせず、Cookie を発行する応答は
// 利用者ごとの内容のため CDN にキャッシュさせない。Range 要求は部分応答のため対象外にする。
// 本文を一度メモリに溜めるため、SSE のような逐次送信の応答には使わない。cfg.Excluded のパス（同期 API など）は対象外にする。
func PublicHTMLCacheMiddlewareWithConfig(cfg PublicHTMLCacheConfig) echo.MiddlewareFunc {
	excluded := cfg.Excluded
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			r := c.Request()
			if !isCacheableMethod(r.Method) || !isPublicHTMLCachePath(r.URL.Path, excluded...) || r.Header.Get("Range") != "" {
				return next(c)
			}
			res := c.Response()
			orig := res.Writer
			bw := &bufferedResponseWriter{header: orig.Header()}
			res.Writer = bw
			err := next(c)
			res.Writer = orig
			if !bw.wroteHeader && bw.body.Len() == 0 {
				// ハンドラーが何も書かずにエラーを返した。エラーハンドラーが元の Writer へ書けるよう未確定に戻す。
				res.Committed = false
				return err
			}
			status := bw.status
			if status == 0 {
				status = http.StatusOK
			}
			h := orig.Header()
			if status == http.StatusOK && h.Get("Set-Cookie") == "" {
				// 304 にも同じ Vary を付ける（キャッシュが保存した応答と同じ選び方で再検証させる）。
				if cfg.VaryHost != nil && cfg.VaryHost() {
					addVary(h, "Host")
				}
				if h.Get("Cache-Control") == "" {
					h.Set("Cache-Control", PublicHTMLCacheControl)
				}
				if h.Get("ETag") == "" {
					h.Set("ETag", contentETag(bw.body.Bytes()))
				}
				if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, strings.TrimPrefix(h.Get("ETag"), "W/")) {
					h.Del("Content-Type")
					h.Del("Content-Length")
					orig.WriteHeader(http.StatusNotModified)
					res.Status = http.StatusNotModified
					res.Size = 0
					return err
				}
			}
			orig.WriteHeader(status)
			n, werr := orig.Write(bw.body.Bytes())
			res.Status = status
			res.Size = int64(n)
			if err == nil {
				err = werr
			}
			return err
		}
	}
}

// addVary は Vary ヘッダーに name が無ければ加える（ハンドラーが付けた HX-Request などは残す）。
func addVary(h http.Header, name string) {
	for _, v := range h.Values(echo.HeaderVary) {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f == "*" || strings.EqualFold(f, name) {
				return
			}
		}
	}
	h.Add(echo.HeaderVary, name)
}

// contentETag は本文の SHA-256 から弱い ETag を作る。Gzip ミドルウェアが本文を圧縮しても
// 意味的に同じ内容として扱えるよう弱い比較用（W/ 付き）にする。
func contentETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

// bufferedResponseWriter はステータスと本文をメモリに溜める http.ResponseWriter。ヘッダーは元の Writer と共有する。
type bufferedResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (w *bufferedResponseWriter) Header() http.Header { return w.header }

func (w *bufferedResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
}

func (w *bufferedResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(b)
}

// Flush は本文を溜めているため何もしない。echo.Response.Flush が未対応として panic しないように実装する。
func (w *bufferedResponseWriter) Flush() {}
