package search

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"

	"sorus/internal/article"
)

const (
	// DefaultPageSize は pageSize に 0 以下を指定したときの1ページの件数。
	DefaultPageSize = 10
	// MaxPageSize は1ページの最大件数。
	MaxPageSize = 100

	// snippetRunes はスニペットの最大文字数。
	snippetRunes = 120
	// snippetLead はスニペットで最初の一致箇所より前に含める文字数。
	snippetLead = 30
	// titleBoost はタイトル一致のスコア倍率。
	titleBoost = 3.0
)

// searchFields は全文検索の対象フィールド。
var searchFields = []string{"title", "content", "excerpt", "category", "author"}

// SearchResult は1ページ分の検索結果。
type SearchResult struct {
	Query      string
	Total      uint64 // 条件に一致した全件数
	Page       int    // 1始まり
	PageSize   int
	TotalPages int
	Hits       []Hit
	Took       time.Duration
}

// Hit は検索結果の1件。
type Hit struct {
	ID          string
	Slug        string
	Title       string
	TitleHTML   template.HTML // 一致箇所を <mark> で囲んだエスケープ済みタイトル
	Excerpt     string
	Category    string
	Author      string
	PublishedAt time.Time // 秒単位に丸めた公開日時（UTC）
	// Kind は記事の種類。種類を保存していない古いインデックスのドキュメントは KindPost とみなす
	// （ページへのリンク /articles/<slug> は公開サイトが /<slug> へ 301 で転送する）。
	Kind     article.Kind
	Lang     string
	Score    float64
	Snippets []Snippet
}

// Snippet は一致箇所を含む本文などの抜粋。
type Snippet struct {
	Field    string        // 抜粋元のフィールド名（content, excerpt など）
	Fragment template.HTML // 一致箇所を <mark> で囲んだエスケープ済み HTML
}

// Options は SearchWithOptions の絞り込み条件。零値なら全言語の記事を、著者名も照合して検索する。
type Options struct {
	// Lang を指定した場合は、その言語（"ja", "en" 等）の記事に絞り込む（"ja" の場合は言語未設定の既存記事も含む）。
	Lang string
	// HideAuthor は、公開サイトが執筆者名を表示しない設定のときに true にする。著者名だけで一致する記事を
	// 件数・ページ・スコアのいずれにも含めない。ただし論文・学会発表の記事は公開ページが著者を書誌情報として
	// 出すため、引き続き著者名でも一致させる。
	HideAuthor bool
}

// Search は SearchContext を context.Background で呼ぶ。
func (x *Index) Search(q string, page, pageSize int, lang ...string) (*SearchResult, error) {
	return x.SearchContext(context.Background(), q, page, pageSize, lang...)
}

// SearchContext は q で公開記事を検索し、page ページ目（1始まり）の結果を返す。
//
// q は空白（全角空白を含む）で区切った語の AND 検索になる。各語はタイトル・本文・抜粋・カテゴリ・著者の
// いずれかに一致すればよく、2文字以上の日本語はバイグラムの全てを含む記事に一致する。
// 全角英数・半角カナは索引と同じく正規化して照合する。公開日時が未来の記事は含めない。
// 結果はスコアの高い順、同点なら公開日時の新しい順。q が空なら0件を返す。
// lang を指定した場合は、その言語（"ja", "en" 等）の記事に絞り込む（"ja" の場合は言語未設定の既存記事も含む）。
func (x *Index) SearchContext(ctx context.Context, q string, page, pageSize int, lang ...string) (*SearchResult, error) {
	var opts Options
	if len(lang) > 0 {
		opts.Lang = lang[0]
	}
	return x.SearchWithOptions(ctx, q, page, pageSize, opts)
}

// SearchWithOptions は SearchContext と同じく q で公開記事を検索し、opts の条件で絞り込む。
// 条件は検索クエリに組み込むため、件数・ページ数・各ページの結果はすべて条件を満たす記事だけで決まる。
func (x *Index) SearchWithOptions(ctx context.Context, q string, page, pageSize int, opts Options) (*SearchResult, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	pageSize = min(pageSize, MaxPageSize)
	res := &SearchResult{Query: q, Page: page, PageSize: pageSize, Hits: []Hit{}}

	terms := strings.Fields(q)
	if len(terms) == 0 {
		return res, nil
	}

	req := bleve.NewSearchRequestOptions(buildQuery(terms, time.Now(), opts), pageSize, (page-1)*pageSize, false)
	req.Fields = []string{"id", "slug", "title", "content", "excerpt", "category", "author", "published_at", "kind", "lang"}
	req.IncludeLocations = true
	req.SortBy([]string{"-_score", "-published_at", "_id"})
	sr, err := x.idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search: query %q: %w", q, err)
	}

	res.Total = sr.Total
	res.TotalPages = int((sr.Total + uint64(pageSize) - 1) / uint64(pageSize))
	res.Took = sr.Took
	for _, h := range sr.Hits {
		res.Hits = append(res.Hits, toHit(h))
	}
	return res, nil
}

// buildQuery は各語の AND に、公開日時が now 以前という条件（および指定があれば言語条件）を加えたクエリを返す。
func buildQuery(terms []string, now time.Time, opts Options) query.Query {
	conj := bleve.NewConjunctionQuery()
	for _, t := range terms {
		disj := bleve.NewDisjunctionQuery()
		for _, f := range searchFields {
			m := bleve.NewMatchQuery(t)
			m.SetField(f)
			m.Analyzer = queryAnalyzer
			m.SetOperator(query.MatchQueryOperatorAnd)
			if f == "title" {
				m.SetBoost(titleBoost)
			}
			if f == "author" && opts.HideAuthor {
				// 執筆者名を表示しない設定では、著者を公開している論文記事だけを著者名で一致させる。
				scholarly := bleve.NewBoolFieldQuery(true)
				scholarly.SetField("scholarly")
				scholarly.SetBoost(0)
				disj.AddQuery(bleve.NewConjunctionQuery(m, scholarly))
				continue
			}
			disj.AddQuery(m)
		}
		conj.AddQuery(disj)
	}
	inclusive := true
	published := bleve.NewDateRangeInclusiveQuery(time.Time{}, now, nil, &inclusive)
	published.SetField("published_at")
	published.SetBoost(0)
	conj.AddQuery(published)

	if targetLang := strings.ToLower(strings.TrimSpace(opts.Lang)); targetLang != "" {
		if targetLang == "ja" {
			// 旧インデックスや lang 未設定の既存記事（日本語）との後方互換性のため、
			// "ja" は「en 等の他言語以外」のドキュメント（旧日本語記事を含む）を一致させる。
			boolQ := bleve.NewBooleanQuery()
			boolQ.AddMust(bleve.NewMatchAllQuery())
			notOther := bleve.NewTermQuery("en")
			notOther.SetField("lang")
			boolQ.AddMustNot(notOther)
			boolQ.SetBoost(0)
			conj.AddQuery(boolQ)
		} else {
			tq := bleve.NewTermQuery(targetLang)
			tq.SetField("lang")
			tq.SetBoost(0)
			conj.AddQuery(tq)
		}
	}
	return conj
}

func toHit(h *search.DocumentMatch) Hit {
	str := func(f string) string {
		s, _ := h.Fields[f].(string)
		return s
	}
	hit := Hit{
		ID:       h.ID,
		Slug:     str("slug"),
		Title:    str("title"),
		Excerpt:  str("excerpt"),
		Category: str("category"),
		Author:   str("author"),
		Kind:     article.Kind(str("kind")),
		Lang:     str("lang"),
		Score:    h.Score,
	}
	if hit.Lang == "" {
		hit.Lang = "ja"
	}
	if !hit.Kind.Valid() {
		hit.Kind = article.KindPost
	}
	if t, err := time.Parse(time.RFC3339Nano, str("published_at")); err == nil {
		hit.PublishedAt = t.UTC()
	}
	hit.TitleHTML = highlight(hit.Title, spans(h.Locations["title"]), 0)
	for _, f := range []string{"content", "excerpt"} {
		if sp := spans(h.Locations[f]); len(sp) > 0 {
			hit.Snippets = append(hit.Snippets, Snippet{Field: f, Fragment: highlight(str(f), sp, snippetRunes)})
		}
	}
	// タイトルなどにだけ一致した場合も、本文の冒頭を抜粋として返す。
	if len(hit.Snippets) == 0 && str("content") != "" {
		hit.Snippets = append(hit.Snippets, Snippet{Field: "content", Fragment: highlight(str("content"), nil, snippetRunes)})
	}
	return hit
}

// span は一致箇所のバイト範囲 [start, end)。
type span struct{ start, end int }

// spans はフィールド内の一致位置を開始順に並べ、重なり・隣接する範囲を結合して返す。
// バイグラムは隣同士が1文字重なるため、結合しないと語の一部だけが強調される。
func spans(tlm search.TermLocationMap) []span {
	var sp []span
	for _, locs := range tlm {
		for _, l := range locs {
			sp = append(sp, span{int(l.Start), int(l.End)})
		}
	}
	sort.Slice(sp, func(i, j int) bool { return sp[i].start < sp[j].start })
	var merged []span
	for _, s := range sp {
		if n := len(merged); n > 0 && s.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, s.end)
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// highlight は text をエスケープし、sp の範囲を <mark> で囲んだ HTML を返す。
// maxRunes > 0 なら最初の一致箇所の少し前から maxRunes 文字に切り詰め、省略箇所に「…」を付ける。
func highlight(text string, sp []span, maxRunes int) template.HTML {
	from, to := 0, len(text)
	if maxRunes > 0 && utf8.RuneCountInString(text) > maxRunes {
		if len(sp) > 0 {
			from = backRunes(text, sp[0].start, snippetLead)
		}
		to = forwardRunes(text, from, maxRunes)
	}

	var sb strings.Builder
	if from > 0 {
		sb.WriteString("…")
	}
	cur := from
	for _, s := range sp {
		if s.start < cur || s.end > to || s.end > len(text) {
			continue
		}
		sb.WriteString(html.EscapeString(text[cur:s.start]))
		sb.WriteString("<mark>")
		sb.WriteString(html.EscapeString(text[s.start:s.end]))
		sb.WriteString("</mark>")
		cur = s.end
	}
	sb.WriteString(html.EscapeString(text[cur:to]))
	if to < len(text) {
		sb.WriteString("…")
	}
	return template.HTML(sb.String()) //nolint:gosec // 全テキストを EscapeString 済み
}

// backRunes は text の byte 位置 pos から n 文字戻った byte 位置を返す。
func backRunes(text string, pos, n int) int {
	for ; n > 0 && pos > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(text[:pos])
		pos -= size
	}
	return pos
}

// forwardRunes は text の byte 位置 pos から n 文字進んだ byte 位置を返す。
func forwardRunes(text string, pos, n int) int {
	for ; n > 0 && pos < len(text); n-- {
		_, size := utf8.DecodeRuneInString(text[pos:])
		pos += size
	}
	return pos
}
