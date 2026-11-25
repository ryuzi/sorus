package server

import (
	"fmt"

	"sorus/internal/markdown"
)

// :::gallery の年別の絞り込み（year_filter="true"）。写真の AI タグの「YYYY年」を年とみなし、
// ピルに並べる年を集め（galleryYears）、続きの API の year=<YYYY年> でその年の写真だけを返す（galleryTagWhere）。

// galleryTagWhere はタグのギャラリーの写真を選ぶ media の WHERE 句と引数を返す。タグ query.Tag が付いた写真のうち、
// MinWidth が正なら元画像の幅がそれ以上のもの、Year が空でなければ AI タグにその年も付いたものに絞る。
func galleryTagWhere(query markdown.GalleryQuery) (string, []any) {
	where := mediaTagMatchSQL
	args := []any{mediaTagMatchArg(query.Tag)}
	if query.MinWidth > 0 {
		where += ` AND width >= ?`
		args = append(args, query.MinWidth)
	}
	if query.Year != "" {
		where += ` AND ` + mediaTagMatchSQL
		args = append(args, mediaTagMatchArg(query.Year))
	}
	return where, args
}

// galleryYears はタグ query.Tag（と MinWidth）の写真の AI タグの「YYYY年」を、重ねずに新しい順で返す
// （年別の絞り込みのピル。query.Year は見ない）。
func (s *Server) galleryYears(query markdown.GalleryQuery) ([]string, error) {
	query.Year = ""
	where, args := galleryTagWhere(query)
	// 同じタグの組み合わせの写真は多いため、組み合わせごとに 1 回だけ読む。
	var tags []string
	if err := s.db.Select(&tags, `SELECT DISTINCT ai_tags FROM media WHERE `+where+` AND ai_tags LIKE '%年%'`, args...); err != nil {
		return nil, fmt.Errorf("server: gallery years: %w", err)
	}
	seen := make(map[string]struct{})
	var years []string
	for _, t := range tags {
		for _, y := range markdown.GalleryYearTags(t) {
			if _, ok := seen[y]; !ok {
				seen[y] = struct{}{}
				years = append(years, y)
			}
		}
	}
	return markdown.SortGalleryYears(years), nil
}
