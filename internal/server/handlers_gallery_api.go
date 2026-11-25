package server

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/markdown"
)

// :::gallery の「もっと見る」で続きの写真を返す公開の API（markdown.GalleryAPIPath = /api/gallery）。
// limit を書かないタグのギャラリーは最初の markdown.DefaultGalleryPageSize 枚だけを本文に出し、justified_gallery.js が
// ボタンを押されたらここから続きを読んで figure の末尾に足す（lightbox.js は足した写真も前後送りに含める）。
//
//	GET /api/gallery?tag=<タグ>&sort=latest|oldest|random&min_width=<px>&seed=<種>&offset=<飛ばす枚数>&limit=<枚数>&year=<YYYY年>
//
// year は年別の絞り込み（year_filter="true" のピル）で、AI タグにその年（「2025年」の形）も付いた写真だけにする。
//
// 応答は JSON（html: 足す div.gallery-item の並び。本文と同じ形・同じサニタイズ、count: 返した枚数、total: 条件に合う総数、
// next_offset: 次に読む offset、remaining: 残りの枚数）。
// 公開中の記事・ページの :::gallery が使っているタグだけを返す（下書きだけで使うタグや、ギャラリーにしていないタグの
// 画像を一覧させない）。それ以外のタグ・不正な値は 404・400。

// galleryAPIResponse は続きの API の応答。
type galleryAPIResponse struct {
	HTML       string `json:"html"`
	Count      int    `json:"count"`
	Total      int    `json:"total"`
	NextOffset int    `json:"next_offset"`
	Remaining  int    `json:"remaining"`
}

// maxGalleryAPIOffset は offset に受け付ける上限（メディアライブラリの写真の数としては十分に大きい値）。
const maxGalleryAPIOffset = 1_000_000

// galleryAPIPath は続きの API のパス（server.go が登録する）。
const galleryAPIPath = markdown.GalleryAPIPath

func (s *Server) handlePublicGalleryAPI(c echo.Context) error {
	query, ok := parseGalleryAPIQuery(c)
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid gallery query")
	}
	used, err := s.publishedGalleryTags(c)
	if err != nil {
		return err
	}
	if !slices.Contains(used, query.Tag) {
		return echo.ErrNotFound
	}
	page, err := s.resolveGalleryPage(query, true)
	if err != nil {
		return err
	}
	next := query.Offset + len(page.Images)
	res := galleryAPIResponse{
		HTML:       markdown.GalleryItemsHTML(page.Images),
		Count:      len(page.Images),
		Total:      page.Total,
		NextOffset: next,
		Remaining:  max(page.Total-next, 0),
	}
	// 写真の追加・タグの変更をすぐに反映させるため、共有のキャッシュには置かせない。
	c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
	return c.JSON(http.StatusOK, res)
}

// parseGalleryAPIQuery は続きの API の問い合わせを検査して GalleryQuery にする。tag は必須。limit は省略すると
// markdown.DefaultGalleryPageSize、1〜MaxGalleryLimit。sort は latest・oldest・random、seed は random のときだけ使う。
func parseGalleryAPIQuery(c echo.Context) (markdown.GalleryQuery, bool) {
	q := markdown.GalleryQuery{Tag: strings.TrimSpace(c.QueryParam("tag")), Sort: "latest", Limit: markdown.DefaultGalleryPageSize}
	if q.Tag == "" || len(q.Tag) > 200 {
		return q, false
	}
	switch sort := c.QueryParam("sort"); sort {
	case "", "latest":
	case "oldest", "random":
		q.Sort = sort
	default:
		return q, false
	}
	intParam := func(name string, lo, hi int, dst *int) bool {
		v := c.QueryParam(name)
		if v == "" {
			return true
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return false
		}
		*dst = n
		return true
	}
	if year := strings.TrimSpace(c.QueryParam("year")); year != "" {
		if !markdown.ValidGalleryYear(year) {
			return q, false
		}
		q.Year = year
	}
	if !intParam("limit", 1, markdown.MaxGalleryLimit, &q.Limit) ||
		!intParam("offset", 0, maxGalleryAPIOffset, &q.Offset) ||
		!intParam("min_width", 0, markdown.MaxGalleryMinWidth, &q.MinWidth) {
		return q, false
	}
	if v := c.QueryParam("seed"); v != "" {
		seed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || !markdown.ValidGallerySeed(seed) {
			return q, false
		}
		if q.Sort == "random" {
			q.Seed = seed
		}
	}
	// 種の無い random は毎回ばらばらになり、続きが重なる。続きの API では必ず種のある並べ替えにする。
	if q.Sort == "random" && q.Seed == 0 {
		return q, false
	}
	return q, true
}

// publishedGalleryTags は公開中の記事・ページの本文の :::gallery が使うタグを返す。
func (s *Server) publishedGalleryTags(c echo.Context) ([]string, error) {
	var bodies []string
	if err := s.db.SelectContext(c.Request().Context(), &bodies, `SELECT a.content_markdown FROM articles a
		WHERE `+publishedCondition+` AND a.content_markdown LIKE '%:::gallery%'`, article.StatusPublished, clockNow().UTC()); err != nil {
		return nil, err
	}
	var tags []string
	for _, body := range bodies {
		for _, tag := range galleryTagsIn(body) {
			if !slices.Contains(tags, tag) {
				tags = append(tags, tag)
			}
		}
	}
	return tags, nil
}

// galleryRenderOptions は公開ページの本文の :::gallery の描画のしかたを返す。静的な書き出しには続きの API が無いため、
// 写真をすべて出力して「もっと見る」で順に見せる形にする。
func (s *Server) galleryRenderOptions(c echo.Context) markdown.GalleryOptions {
	opts := markdown.GalleryOptions{Resolver: s.galleryPageResolver(), Years: s.galleryYears}
	if c == nil || !isStaticExport(c) {
		opts.MoreURL = markdown.GalleryAPIPath
	}
	return opts
}
