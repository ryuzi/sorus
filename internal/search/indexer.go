// Package search は Bleve v2 と CJK バイグラムによる日本語全文検索を提供する。
// 辞書を使わないため CGO 不要で、未知語・造語も検索漏れしない。
// インデックスには公開記事だけを登録し、公開日時が未来の記事は検索時に除外する。
package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/jmoiron/sqlx"
	"golang.org/x/net/html"

	"sorus/internal/article"
	"sorus/internal/markdown"
)

const (
	// indexAnalyzer は索引用アナライザ。1文字検索のためバイグラムに加えてユニグラムも出力する。
	indexAnalyzer = "cms_cjk_index"
	// queryAnalyzer は検索語用アナライザ。2文字以上の CJK はバイグラムのみにし、
	// 1文字だけの CJK はバイグラムフィルタがユニグラムとして出力する。
	queryAnalyzer = "cms_cjk_query"
	// unigramFilter はユニグラムも出力する CJK バイグラムフィルタの登録名。
	unigramFilter = "cms_cjk_bigram_unigram"

	docType = "article"

	// currentMappingVersion はインデックスマッピングのバージョン。
	// lang フィールドの追加に伴い v2、scholarly フィールドの追加に伴い v3 へ更新。
	currentMappingVersion = "3"

	// syncBatchSize は SyncFromDB で1回のバッチにまとめる記事数。
	syncBatchSize = 200
)

// IndexedArticle はインデックスに登録する記事ドキュメント。
type IndexedArticle struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Slug        string    `json:"slug"`
	Content     string    `json:"content"` // HTML タグを除去したプレーンテキスト
	Excerpt     string    `json:"excerpt"`
	Category    string    `json:"category"`
	Author      string    `json:"author"`
	PublishedAt time.Time `json:"published_at"`
	// Kind は記事の種類（post / page）。検索結果のリンク先（/articles/<slug> か /<slug>）を決める。
	Kind string `json:"kind"`
	// Lang は記事の言語コード（ja, en など）。検索結果の言語フィルタとリンク先URL生成に使う。
	Lang string `json:"lang"`
	// Scholarly は論文・学会発表の記事か。公開ページは執筆者名を表示しない設定でも論文記事の著者を書誌情報として
	// 出すため、その設定での検索（Options.HideAuthor）でも論文記事だけは著者名で一致させる。
	Scholarly bool `json:"scholarly"`
}

// Index は記事の全文検索インデックス。並行に使用してよい。
type Index struct {
	idx bleve.Index
}

// OpenOrCreateIndex は dir のインデックスを開き、存在しなければ新規作成する。
// 空ディレクトリが既に存在する場合でも安全に新規インデックスを初期化する。
// マッピング定義が古い（lang フィールドが存在しない等）場合は安全に再初期化する。
func OpenOrCreateIndex(dir string) (*Index, error) {
	versionFile := filepath.Join(dir, ".sorus_mapping_version")
	idx, err := bleve.Open(dir)
	if err == nil {
		vBytes, vErr := os.ReadFile(versionFile)
		if vErr == nil && strings.TrimSpace(string(vBytes)) == currentMappingVersion {
			return &Index{idx: idx}, nil
		}
		// マッピングバージョン不一致（または旧バージョン）：インデックスを再作成する
		_ = idx.Close()
		_ = os.RemoveAll(dir)
	}

	isNotExist := errors.Is(err, bleve.ErrorIndexPathDoesNotExist) || errors.Is(err, os.ErrNotExist)
	isEmptyDir := false
	if entries, rerr := os.ReadDir(dir); rerr == nil && len(entries) == 0 {
		isEmptyDir = true
		_ = os.Remove(dir)
	}

	if isNotExist || isEmptyDir || err == nil {
		m, merr := newMapping()
		if merr != nil {
			return nil, fmt.Errorf("search: open %s: %w", dir, merr)
		}
		idx, err = bleve.New(dir, m)
		if err == nil {
			_ = os.WriteFile(versionFile, []byte(currentMappingVersion), 0644)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("search: open %s: %w", dir, err)
	}
	return &Index{idx: idx}, nil
}

// NewMemIndex はメモリ上のインデックスを作成する。テスト用。
func NewMemIndex() (*Index, error) {
	m, err := newMapping()
	if err != nil {
		return nil, fmt.Errorf("search: new mem index: %w", err)
	}
	idx, err := bleve.NewMemOnly(m)
	if err != nil {
		return nil, fmt.Errorf("search: new mem index: %w", err)
	}
	return &Index{idx: idx}, nil
}

// newMapping は記事ドキュメントのマッピングを構築する。
// 全文検索対象はタイトル・本文・抜粋・カテゴリ・著者で、ID・スラッグ・種類は表示用に保存のみ行う。
// 論文記事かどうか（scholarly）は検索条件にだけ使い、保存しない。
func newMapping() (*mapping.IndexMappingImpl, error) {
	m := bleve.NewIndexMapping()
	if err := m.AddCustomTokenFilter(unigramFilter, map[string]interface{}{
		"type":           cjk.BigramName,
		"output_unigram": true,
	}); err != nil {
		return nil, err
	}
	analyzers := map[string]string{indexAnalyzer: unigramFilter, queryAnalyzer: cjk.BigramName}
	for name, bigram := range analyzers {
		if err := m.AddCustomAnalyzer(name, map[string]interface{}{
			"type":          custom.Name,
			"tokenizer":     widthTokenizerName,
			"token_filters": []string{cjk.WidthName, lowercase.Name, bigram},
		}); err != nil {
			return nil, err
		}
	}
	m.DefaultAnalyzer = indexAnalyzer

	text := func() *mapping.FieldMapping {
		f := bleve.NewTextFieldMapping()
		f.Analyzer = indexAnalyzer
		f.Store = true
		f.IncludeTermVectors = true
		return f
	}
	stored := bleve.NewKeywordFieldMapping()
	stored.Index = false
	stored.IncludeInAll = false
	published := bleve.NewDateTimeFieldMapping()
	published.IncludeInAll = false
	scholarly := bleve.NewBooleanFieldMapping()
	scholarly.IncludeInAll = false

	doc := bleve.NewDocumentStaticMapping()
	doc.AddFieldMappingsAt("id", stored)
	doc.AddFieldMappingsAt("slug", stored)
	doc.AddFieldMappingsAt("kind", stored)
	langMapping := bleve.NewKeywordFieldMapping()
	langMapping.Store = true
	doc.AddFieldMappingsAt("lang", langMapping)
	for _, name := range searchFields {
		doc.AddFieldMappingsAt(name, text())
	}
	doc.AddFieldMappingsAt("published_at", published)
	doc.AddFieldMappingsAt("scholarly", scholarly)
	m.AddDocumentMapping(docType, doc)
	m.DefaultMapping = doc
	m.TypeField = "_type"
	return m, nil
}

// IndexArticle は記事をインデックスに登録・更新する。
// 公開状態でない記事（下書き・論理削除）はインデックスから削除する。
func (x *Index) IndexArticle(a *article.Article, authorName, categoryName string) error {
	if a == nil || a.ID == "" {
		return errors.New("search: index article: missing article id")
	}
	if !indexable(a) {
		return x.DeleteArticle(a.ID)
	}
	doc, err := toDoc(a, authorName, categoryName)
	if err != nil {
		return fmt.Errorf("search: index article %s: %w", a.ID, err)
	}
	if err := x.idx.Index(a.ID, doc); err != nil {
		return fmt.Errorf("search: index article %s: %w", a.ID, err)
	}
	return nil
}

// DeleteArticle は記事をインデックスから削除する。未登録の ID でも成功する。
func (x *Index) DeleteArticle(id string) error {
	if err := x.idx.Delete(id); err != nil {
		return fmt.Errorf("search: delete article %s: %w", id, err)
	}
	return nil
}

// syncRow は SyncFromDB で読む記事と著者名・カテゴリ名。
type syncRow struct {
	article.Article
	AuthorName   string `db:"author_name"`
	CategoryName string `db:"category_name"`
}

// SyncFromDB は DB の公開記事を全件インデックスし、公開記事でなくなったドキュメントを削除する。
// 起動時の初期化やインデックス破損時の再構築に使う。
func (x *Index) SyncFromDB(ctx context.Context, db *sqlx.DB) error {
	rows, err := db.QueryxContext(ctx, `SELECT a.id, a.title, a.slug, a.content_markdown, a.content_html,
			a.excerpt, a.featured_image_url, a.author_id, a.category_id, a.status, a.locked_by, a.locked_at,
			a.published_at, a.created_at, a.updated_at, a.kind,
			a.doi, a.journal_name, a.conference_name, a.co_authors,
			COALESCE(NULLIF(a.lang, ''), 'ja') AS lang,
			COALESCE(NULLIF(u.display_name, ''), u.username, '') AS author_name,
			COALESCE(c.name, '') AS category_name
		FROM articles a
		LEFT JOIN users u ON u.id = a.author_id
		LEFT JOIN categories c ON c.id = a.category_id
		WHERE a.status = ? AND a.published_at IS NOT NULL`, article.StatusPublished)
	if err != nil {
		return fmt.Errorf("search: sync: query: %w", err)
	}
	defer rows.Close()

	live := make(map[string]bool)
	batch := x.idx.NewBatch()
	for rows.Next() {
		var r syncRow
		if err := rows.StructScan(&r); err != nil {
			return fmt.Errorf("search: sync: scan: %w", err)
		}
		doc, err := toDoc(&r.Article, r.AuthorName, r.CategoryName)
		if err != nil {
			return fmt.Errorf("search: sync: article %s: %w", r.ID, err)
		}
		if err := batch.Index(r.ID, doc); err != nil {
			return fmt.Errorf("search: sync: article %s: %w", r.ID, err)
		}
		live[r.ID] = true
		if batch.Size() >= syncBatchSize {
			if err := x.idx.Batch(batch); err != nil {
				return fmt.Errorf("search: sync: batch: %w", err)
			}
			batch.Reset()
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("search: sync: rows: %w", err)
	}

	stale, err := x.staleIDs(ctx, live)
	if err != nil {
		return fmt.Errorf("search: sync: %w", err)
	}
	for _, id := range stale {
		batch.Delete(id)
	}
	if err := x.idx.Batch(batch); err != nil {
		return fmt.Errorf("search: sync: batch: %w", err)
	}
	return nil
}

// staleIDs はインデックス済みで live に含まれない ID を返す。
func (x *Index) staleIDs(ctx context.Context, live map[string]bool) ([]string, error) {
	n, err := x.idx.DocCount()
	if err != nil {
		return nil, fmt.Errorf("doc count: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	req := bleve.NewSearchRequestOptions(bleve.NewMatchAllQuery(), int(n), 0, false)
	res, err := x.idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list indexed: %w", err)
	}
	var stale []string
	for _, h := range res.Hits {
		if !live[h.ID] {
			stale = append(stale, h.ID)
		}
	}
	return stale, nil
}

// DocCount はインデックス済みの記事数を返す。ヘルスチェックでインデックスが読めることの確認にも使う。
func (x *Index) DocCount() (uint64, error) {
	n, err := x.idx.DocCount()
	if err != nil {
		return 0, fmt.Errorf("search: doc count: %w", err)
	}
	return n, nil
}

// Close はインデックスを閉じる。
func (x *Index) Close() error {
	if err := x.idx.Close(); err != nil {
		return fmt.Errorf("search: close: %w", err)
	}
	return nil
}

// indexable は記事が検索対象（公開状態で公開日時あり）かを返す。
func indexable(a *article.Article) bool {
	return a.Status == article.StatusPublished && a.PublishedAt != nil
}

// toDoc は記事をインデックスのドキュメントにする。ページは公開サイトでカテゴリ・著者を出さないため、
// それらの名前では検索に一致させない。
func toDoc(a *article.Article, authorName, categoryName string) (*IndexedArticle, error) {
	body := a.ContentHTML
	if body == "" && strings.TrimSpace(a.ContentMarkdown) != "" {
		rendered, err := markdown.Render(a.ContentMarkdown)
		if err != nil {
			return nil, err
		}
		body = rendered.HTML
	}
	kind := a.Kind
	if kind == "" {
		kind = article.KindPost
	}
	if kind == article.KindPage {
		authorName, categoryName = "", ""
	}
	lang := a.Lang
	if lang == "" {
		lang = "ja"
	}
	return &IndexedArticle{
		ID:          a.ID,
		Title:       a.Title,
		Slug:        a.Slug,
		Content:     htmlToText(body),
		Excerpt:     a.Excerpt,
		Category:    categoryName,
		Author:      authorName,
		PublishedAt: a.PublishedAt.UTC(),
		Kind:        string(kind),
		Lang:        lang,
		// 公開記事ページの publicArticle.IsScholarly と同じ条件。
		Scholarly: a.DOI != "" || a.JournalName != "" || a.ConferenceName != "" || a.CoAuthors != "",
	}, nil
}

// htmlToText は HTML からタグを除去したプレーンテキストを返す。
// ブロック要素の境界には空白を入れ、段落をまたいだ語がつながって誤ったバイグラムが生成されないようにする。
// インライン要素（strong, a など）は区切らないため、装飾をまたぐ語も検索できる。
func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var sb strings.Builder
	skip := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return strings.Join(strings.Fields(sb.String()), " ")
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == "script" || tag == "style" {
				switch tt {
				case html.StartTagToken:
					skip++
				case html.EndTagToken:
					skip = max(skip-1, 0)
				}
			}
			if blockTags[tag] {
				sb.WriteByte(' ')
			}
		case html.TextToken:
			if skip == 0 {
				sb.Write(z.Text())
			}
		}
	}
}

// blockTags は前後でテキストを区切る HTML 要素。
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "hr": true, "li": true, "ul": true, "ol": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "table": true, "tr": true, "td": true, "th": true,
	"figure": true, "figcaption": true, "img": true, "dt": true, "dd": true,
}
