package server

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
)

// redirectCacheTTL は redirects テーブルを読み直すまでの時間。
const redirectCacheTTL = time.Minute

// redirectTarget は転送先とステータスコード。
type redirectTarget struct {
	To   string
	Code int
	// HasQuery は旧パスがクエリ文字列まで含めて登録されているか（WordPress の /?p=123 など）。
	HasQuery bool
}

// redirectCache は redirects テーブルをメモリに保持し、ttl ごとに読み直す。
// 読み込みに失敗したときは古い内容を使い続け、公開サイトを止めない。
type redirectCache struct {
	db  *sqlx.DB
	ttl time.Duration

	mu       sync.Mutex
	entries  map[string]redirectTarget
	loadedAt time.Time
}

func newRedirectCache(db *sqlx.DB, ttl time.Duration) *redirectCache {
	return &redirectCache{db: db, ttl: ttl}
}

// lookup は requestPath（デコード済み）と rawQuery に一致する転送先を返す。
// クエリ付きで登録された旧パスを優先し、無ければパスだけで照合する。
func (rc *redirectCache) lookup(c echo.Context, requestPath, rawQuery string) (redirectTarget, bool) {
	entries := rc.load(c)
	key := normalizeRedirectPath(requestPath)
	if rawQuery != "" {
		if t, ok := entries[key+"?"+rawQuery]; ok {
			return t, true
		}
	}
	t, ok := entries[key]
	return t, ok
}

// reset はキャッシュを捨て、次の照合で DB から読み直させる。
func (rc *redirectCache) reset() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.entries = nil
}

func (rc *redirectCache) load(c echo.Context) map[string]redirectTarget {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.entries != nil && time.Since(rc.loadedAt) < rc.ttl {
		return rc.entries
	}
	var rows []struct {
		OldPath    string `db:"old_path"`
		NewPath    string `db:"new_path"`
		StatusCode int    `db:"status_code"`
	}
	if err := rc.db.Select(&rows, `SELECT old_path, new_path, status_code FROM redirects`); err != nil {
		c.Logger().Errorf("redirect: load redirects: %v", err)
		// 失敗が続いても毎リクエスト DB を叩かないよう、次の読み直しは ttl 後にする。
		rc.loadedAt = time.Now()
		if rc.entries == nil {
			rc.entries = map[string]redirectTarget{}
		}
		return rc.entries
	}
	entries := make(map[string]redirectTarget, len(rows))
	for _, r := range rows {
		oldPath, query, hasQuery := strings.Cut(strings.TrimSpace(r.OldPath), "?")
		key := normalizeRedirectPath(oldPath)
		if hasQuery {
			key += "?" + query
		}
		to := strings.TrimSpace(r.NewPath)
		toPath, toQuery, toHasQuery := strings.Cut(to, "?")
		normTo := normalizeRedirectPath(toPath)
		if toHasQuery {
			normTo += "?" + toQuery
		}
		if to == "" || normTo == key {
			continue // 自分自身への転送はループになるため登録しない（クエリ付きを含む）。
		}
		entries[key] = redirectTarget{To: to, Code: r.StatusCode, HasQuery: hasQuery}
	}
	rc.entries = entries
	rc.loadedAt = time.Now()
	return entries
}

// normalizeRedirectPath はパーセントエンコードの有無と末尾の / の違いを吸収した照合キーを返す。
func normalizeRedirectPath(p string) string {
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}
	return p
}

// redirectMiddleware は redirects テーブルに登録された旧パスへの GET/HEAD を転送先へ転送する。
// ルーティングより前（Echo#Pre）で動かし、存在しないパスでも転送できるようにする。
// 管理画面と静的アセットは対象外。パスだけで一致した場合、元のクエリ文字列は転送先へ引き継ぐ。
//
// 公開中の固定ページ自身の URL（/<slug>）は転送しない。WordPress から取り込んだ固定ページは旧パーマリンク
// （/<slug>/）から /articles/<slug> への転送を持つが、/articles/<slug> はページを /<slug> へ戻すため循環する。
// 転送先が公開中のページの /articles/<slug> なら、301 を重ねないよう /<slug> へ直接転送する。
func (s *Server) redirectMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			if req.Method != http.MethodGet && req.Method != http.MethodHead {
				return next(c)
			}
			p := req.URL.Path
			if p == "/admin" || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, UploadURLPrefix) {
				return next(c)
			}
			// 英語トップ（/en, /en/）は予約パスであり、リダイレクトテーブルで横取りしない
			if p == "/en" || p == "/en/" {
				return next(c)
			}
			t, ok := s.redirects.lookup(c, p, req.URL.RawQuery)
			if !ok {
				return next(c)
			}
			// ページのルートが受けるのは末尾に / の無い /<slug> だけなので、/<slug>/ は転送する。
			if slug, ok := singleSegment(p); ok && s.isPublishedPage(c, slug) {
				return next(c)
			}
			to := t.To
			if toPath, toQuery, hasQuery := strings.Cut(to, "?"); strings.HasPrefix(toPath, "/articles/") {
				if slug, ok := singleSegment(strings.TrimPrefix(normalizeRedirectPath(toPath), "/articles")); ok && s.isPublishedPage(c, slug) {
					to = pagePath(slug)
					if hasQuery {
						to += "?" + toQuery
					}
				}
			}
			if !t.HasQuery && req.URL.RawQuery != "" && !strings.Contains(to, "?") {
				to += "?" + req.URL.RawQuery
			}
			return c.Redirect(t.Code, to)
		}
	}
}

// singleSegment は "/<x>"（x は空でなく / を含まない）の x を返す。p はデコード済みのパス。
func singleSegment(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, "/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// isPublishedPage は slug が公開中で公開日時を迎えた固定ページのスラッグかを返す。読めなければ false（転送を優先する）。
func (s *Server) isPublishedPage(c echo.Context, slug string) bool {
	var exists bool
	err := s.db.GetContext(c.Request().Context(), &exists, `SELECT EXISTS (SELECT 1 FROM articles a
		WHERE a.slug = ? AND a.kind = ? AND `+publishedCondition+`)`, slug, article.KindPage, article.StatusPublished, clockNow().UTC())
	return err == nil && exists
}
