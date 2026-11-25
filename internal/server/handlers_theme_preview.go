package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/i18n"
	"sorus/internal/theme"
)

// themeOverride は公開ページのテーマを、保存済みの設定より優先して使わせる文脈の値。
// エクスポートの --theme と管理画面のプレビューが、内部リクエスト（serveInternal）の文脈に入れて渡す。
// 訪問者の要求から設定する方法は無い（公開側の ?theme= は無視する）。
type themeOverride struct {
	// ID はカタログで確かめ済みのテーマ ID。
	ID string
	// Preview は管理画面のプレビューとして描画するときの情報。エクスポートでは nil。
	Preview *themePreview
}

type themeOverrideKey struct{}

// themeOverrideFrom は要求の文脈に入っているテーマの上書きを返す。
func themeOverrideFrom(c echo.Context) (themeOverride, bool) {
	ov, ok := c.Request().Context().Value(themeOverrideKey{}).(themeOverride)
	return ov, ok
}

// serveInternal は req をネットワークを通さずにサーバー自身のハンドラーへ流し、応答を記録して返す。
// ov が nil でなければ文脈に入れ、公開ページのテーマを上書きさせる。エクスポートとプレビューが使う。
func (s *Server) serveInternal(req *http.Request, ov *themeOverride) *httptest.ResponseRecorder {
	if ov != nil {
		req = req.WithContext(context.WithValue(req.Context(), themeOverrideKey{}, *ov))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// themePreviewPath はテーマ設定のプレビューのパス。
const themePreviewPath = "/admin/settings/theme/preview"

// プレビューで表示できるページ（?page= の値）。
const (
	previewPageHome    = "home"
	previewPageArticle = "article"
)

// themePreview は公開レイアウトのプレビューバーに渡す情報。
type themePreview struct {
	Theme theme.Theme
	// Lang は管理画面の言語。バーの文言とテーマ名に使う。
	Lang string
	// Page は表示中のページ（previewPageHome か previewPageArticle）。
	Page string
	// HasArticle は「記事」に切り替えられる公開記事があるか。
	HasArticle bool
	// Frame は設定画面のカードの縮小表示（?frame=1）か。バーを出さない。
	Frame bool
	// Applied はこのテーマが保存済み（適用中）か。
	Applied bool
	// CSRFToken はバーの「このテーマを適用」フォームに載せる管理画面の CSRF トークン。
	CSRFToken string
}

// T は管理画面の言語で key の文言を返す。
func (p *themePreview) T(key string, args ...any) string { return i18n.Translate(p.Lang, key, args...) }

// Label は管理画面の言語でのテーマ名を返す。
func (p *themePreview) Label() string { return p.Theme.Label(p.Lang) }

// URL は同じテーマで page を表示するプレビューの URL を返す。
func (p *themePreview) URL(page string) string { return themePreviewURL(p.Theme.ID, page, false) }

// themePreviewURL はテーマ id の page をプレビューする URL を返す。frame なら設定画面のカード用（バーなし）。
func themePreviewURL(id, page string, frame bool) string {
	v := url.Values{"theme": {id}}
	if page != "" && page != previewPageHome {
		v.Set("page", page)
	}
	if frame {
		v.Set("frame", "1")
	}
	return themePreviewPath + "?" + v.Encode()
}

// handleThemePreview は公開ページ（トップ、または最新の公開記事）を、?theme= のテーマで描画して返す（管理者専用）。
// 公開ハンドラーへ内部リクエストを流して本物の HTML を作り、テーマは文脈で上書きする。PV は数えず、
// 検索エンジンにもキャッシュにも残させない。iframe での表示は同じオリジンに限る。
func (s *Server) handleThemePreview(c echo.Context) error {
	t, ok := s.themeCatalog().Get(c.QueryParam("theme"))
	if !ok {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid theme")
	}
	slug, err := s.latestPublicSlug(c.Request().Context())
	if err != nil {
		return err
	}
	applied, ok := s.themeCatalog().Get(s.getSetting("theme", theme.Default))
	pv := &themePreview{
		Theme:      t,
		Lang:       string(language(c)),
		Page:       previewPageHome,
		HasArticle: slug != "",
		Frame:      c.QueryParam("frame") == "1",
		Applied:    ok && applied.ID == t.ID,
		CSRFToken:  csrfToken(c),
	}
	target := "/"
	if c.QueryParam("page") == previewPageArticle && slug != "" {
		pv.Page, target = previewPageArticle, articlePath(slug)
	}

	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(c.Request().Context())
	// 絶対 URL（canonical など）を、管理者が開いているのと同じホストで組み立てさせる。
	req.Host = c.Request().Host
	rec := s.serveInternal(req, &themeOverride{ID: t.ID, Preview: pv})

	h := c.Response().Header()
	if ct := rec.Header().Get(echo.HeaderContentType); ct != "" {
		h.Set(echo.HeaderContentType, ct)
	}
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set(echo.HeaderXFrameOptions, "SAMEORIGIN")
	c.Response().WriteHeader(rec.Code)
	_, err = c.Response().Write(rec.Body.Bytes())
	return err
}

// latestPublicSlug は公開日時が最も新しい公開記事（ページを除く）のスラッグを返す。公開記事が無ければ空文字を返す。
func (s *Server) latestPublicSlug(ctx context.Context) (string, error) {
	var slugs []string
	err := s.db.SelectContext(ctx, &slugs, `SELECT a.slug FROM articles a WHERE `+postKindCondition+` AND `+publishedCondition+
		` ORDER BY julianday(a.published_at) DESC, a.id LIMIT 1`, article.StatusPublished, clockNow().UTC())
	if err != nil || len(slugs) == 0 {
		return "", err
	}
	return slugs[0], nil
}
