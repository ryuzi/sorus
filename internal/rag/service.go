package rag

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"golang.org/x/text/unicode/norm"

	"sorus/internal/article"
)

// 検索件数の既定値と上限。
const (
	// DefaultSearchLimit は Search の limit が 0 以下のときに返すチャンクの件数。
	DefaultSearchLimit = 5
	// MaxSearchLimit は Search が返すチャンクの件数の上限。
	MaxSearchLimit = 50
	// DefaultRelatedLimit は FindRelatedArticles の limit が 0 以下のときに返す記事の件数（公開記事末尾の Top-3。
	// デスクトップで3列×1行に揃う件数）。
	DefaultRelatedLimit = 3
)

// 関連記事のハイブリッド順位付けの重み。スコアは RelatedCosineWeight × コサイン類似度 + RelatedKeywordWeight × 全文検索スコア。
const (
	// RelatedCosineWeight は記事の代表ベクトルのコサイン類似度（意味の近さ）の重み。
	RelatedCosineWeight = 0.6
	// RelatedKeywordWeight は全文検索（Bleve）のスコアを 0〜1 に正規化した値（語の重なり）の重み。
	RelatedKeywordWeight = 0.4
)

// 関連記事の新しさ補正（Recency boost）。順位付けでは類似度のスコアに
// RelatedRecencyBoostMax × 2^(-経過年数 / RelatedRecencyHalfLifeYears) を加え、語や意味の近さが同程度なら
// 新しい記事を上に出す。補正は小さく、類似度の差が補正の差より大きければ順位は変わらない。
// 返す RelatedArticle.Score は補正を含まない類似度のまま。
const (
	// RelatedRecencyBoostMax は公開直後の記事に加える補正の最大値。
	RelatedRecencyBoostMax = 0.08
	// RelatedRecencyHalfLifeYears は補正が半分になるまでの経過年数。
	RelatedRecencyHalfLifeYears = 3.0
)

// maxQueryChars は検索クエリとして埋め込むテキストの上限（文字数）。
// 記事本文全体をクエリに渡されても Embeddings API の入力上限（Gemini は 2,048 トークン）を超えないよう、先頭だけを使う。
const maxQueryChars = 1500

// idBytes はチャンクIDの乱数バイト長。hex で32文字になる。
const idBytes = 16

// publishedStatus は検索・関連記事の対象とする記事の状態。対象は読者に見えている記事（article.VisibleCondition）に
// 限り、下書き・校閲中・予約中（公開日時が未来）・削除済みの記事は公開サイトや AI のコンテキストに出さない。
const publishedStatus = article.StatusPublished

// postKind は関連記事の対象とする記事の種類。固定ページ（page）は検索・RAG の対象だが、
// 関連記事には出さず、ページから関連記事も推薦しない。
const postKind = "post"

// pageKind は固定ページの種類。
const pageKind = "page"

// ErrArticleNotFound は埋め込みを保存する記事が存在しないことを表す。
var ErrArticleNotFound = errors.New("rag: article not found")

// SearchResult は Search が返す、クエリに意味的に近いチャンク。
type SearchResult struct {
	ArticleID    string  `json:"article_id"`
	ArticleTitle string  `json:"article_title"`
	ArticleSlug  string  `json:"article_slug"`
	ChunkIndex   int     `json:"chunk_index"`
	Heading      string  `json:"heading"`
	Text         string  `json:"text"`
	Score        float64 `json:"score"`
}

// RelatedArticle は FindRelatedArticles が返す、意味的に近い別の記事。
type RelatedArticle struct {
	ArticleID   string    `json:"article_id"`
	Title       string    `json:"title"`
	Slug        string    `json:"slug"`
	Score       float64   `json:"score"`
	PublishedAt time.Time `json:"published_at"`
}

// SearchOptions は SearchWithOptions の条件。
type SearchOptions struct {
	// Limit は返すチャンクの最大件数。0 以下なら DefaultSearchLimit、MaxSearchLimit を超えれば MaxSearchLimit。
	Limit int
	// Threshold はコサイン類似度の下限。これ未満のチャンクは返さない。
	Threshold float64
	// ExcludeArticleIDs は結果から除く記事（編集中の記事自身など）。
	ExcludeArticleIDs []string
	// MaxPerArticle は1記事から返すチャンクの最大件数。0 以下なら制限しない。
	// 「過去記事を3件参照する」のように別々の記事を集めたいときは 1 にする。
	MaxPerArticle int
}

// RelatedOptions は FindRelatedArticlesWithOptions の条件。
type RelatedOptions struct {
	// Limit は返す記事の最大件数。0 以下なら DefaultRelatedLimit。
	Limit int
	// KeywordScores は全文検索で求めた、記事 ID ごとの関連度（Bleve のスコア）。値の尺度は問わず、最大値で割って
	// 0〜1 に正規化してから重み付けする。空ならコサイン類似度だけで順位付けする。
	KeywordScores map[string]float64
}

// Service は記事チャンクの埋め込みの保存と、意味的な近傍検索・関連記事の算出を行う。並行に使用してよい。
//
// 検索と関連記事の対象は読者に見えている（公開で、公開日時を迎えた）記事に限る。外部の Embeddings API に下書きなどの
// 本文を送らないよう、呼び出し側は読者に見えている記事だけを IndexArticle に渡し、そうでなくなった記事の埋め込みは
// DeleteArticle で消す（server の indexRAGArticle）。
type Service struct {
	db       *sqlx.DB
	embedder Embedder
	chunker  Chunker
	// now は公開日時と比べる現在時刻を返す。テストで時計を差し替える。
	now func() time.Time
}

// NewService は db と embedder を使う Service を返す。db は database.Migrate 適用済みであること。
// embedder が nil（API キー未設定）でも FindRelatedArticles は保存済みの埋め込みで動き、
// IndexArticle と Search は ErrNotConfigured を返す。
func NewService(db *sqlx.DB, embedder Embedder) *Service {
	return &Service{db: db, embedder: embedder, now: time.Now}
}

// Enabled は Embeddings API を使えるか（IndexArticle と Search を呼べるか）を返す。
func (s *Service) Enabled() bool { return s.embedder != nil }

// storedChunk は article_embeddings から読んだ、埋め込みの再利用の判定に使う列。
type storedChunk struct {
	Heading    string `db:"heading"`
	Text       string `db:"chunk_text"`
	Model      string `db:"model"`
	Dimensions int    `db:"dimensions"`
	Embedding  []byte `db:"embedding"`
}

// IndexArticle は記事本文 markdown をチャンクに分割して埋め込みを生成し、記事の既存のチャンクと置き換える。
// 置き換えは単一のトランザクションで行い、途中で失敗すれば既存のチャンクを残す。
//
// 見出し・本文・モデルが既存のチャンクと同じものは保存済みのベクトルを再利用し、変わったチャンクだけ API に送る。
// 本文が空なら既存のチャンクを削除する。記事が存在しなければ ErrArticleNotFound、embedder が無ければ
// ErrNotConfigured を返す。
func (s *Service) IndexArticle(ctx context.Context, articleID, markdown string) error {
	if s.embedder == nil {
		return ErrNotConfigured
	}
	var exists bool
	if err := s.db.GetContext(ctx, &exists, `SELECT EXISTS (SELECT 1 FROM articles WHERE id = ?)`, articleID); err != nil {
		return fmt.Errorf("rag: index: find article: %w", err)
	}
	if !exists {
		return ErrArticleNotFound
	}

	chunks := s.chunker.Split(markdown)
	var stored []storedChunk
	if err := s.db.SelectContext(ctx, &stored, `SELECT heading, chunk_text, model, dimensions, embedding
		FROM article_embeddings WHERE article_id = ? ORDER BY chunk_index`, articleID); err != nil {
		return fmt.Errorf("rag: index: load chunks: %w", err)
	}
	model, vectors, err := s.embedChunks(ctx, chunks, stored)
	if err != nil {
		return fmt.Errorf("rag: index: %w", err)
	}

	return s.withWriteTx(ctx, func(conn *sqlx.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM article_embeddings WHERE article_id = ?`, articleID); err != nil {
			return fmt.Errorf("rag: index: delete chunks: %w", err)
		}
		for i, c := range chunks {
			id, err := newID()
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO article_embeddings
				(id, article_id, chunk_index, chunk_text, heading, embedding, dimensions, model)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				id, articleID, c.Index, c.Text, c.Heading, EncodeVector(vectors[i]), len(vectors[i]), model); err != nil {
				return fmt.Errorf("rag: index: insert chunk %d: %w", c.Index, err)
			}
		}
		return nil
	})
}

// DeleteArticle は記事の埋め込みをすべて消す。埋め込みが無くても成功する。Embeddings API は使わない。
func (s *Service) DeleteArticle(ctx context.Context, articleID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM article_embeddings WHERE article_id = ?`, articleID); err != nil {
		return fmt.Errorf("rag: delete chunks: %w", err)
	}
	return nil
}

// chunkKey は埋め込みの再利用の判定に使うチャンクの内容。EmbeddingText の材料と同じもの。
func chunkKey(heading, text string) string { return heading + "\x00" + text }

// embedChunks は chunks の埋め込みベクトルとモデル名を返す。stored が Embedder の現在のモデルで作られていれば、
// 見出し・本文が同じチャンクはそのベクトルを使い、残りだけを API に送る。新しいベクトルのモデルや次元数が
// 保存済みと異なるときは、混在させないようすべてのチャンクを生成し直す。
func (s *Service) embedChunks(ctx context.Context, chunks []Chunk, stored []storedChunk) (string, [][]float32, error) {
	if len(chunks) == 0 {
		return "", nil, nil
	}
	cached := map[string][]float32{}
	var cachedModel string
	var cachedDims int
	for i, sc := range stored {
		if i == 0 {
			cachedModel, cachedDims = sc.Model, sc.Dimensions
		}
		if sc.Model != cachedModel || sc.Dimensions != cachedDims || sc.Model != s.embedder.Model() {
			// モデルが混在している、または Embedder のモデルが変わったときは再利用しない。
			cached = map[string][]float32{}
			break
		}
		v, err := DecodeVector(sc.Embedding)
		if err != nil || len(v) != sc.Dimensions {
			continue
		}
		cached[chunkKey(sc.Heading, sc.Text)] = v
	}

	vectors := make([][]float32, len(chunks))
	var missing []int
	for i, c := range chunks {
		if v, ok := cached[chunkKey(c.Heading, c.Text)]; ok {
			vectors[i] = v
		} else {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return cachedModel, vectors, nil
	}

	res, err := s.embed(ctx, chunks, missing)
	if err != nil {
		return "", nil, err
	}
	if len(missing) < len(chunks) && (res.Model != cachedModel || len(res.Vectors[0]) != cachedDims) {
		missing = missing[:0]
		for i := range chunks {
			missing = append(missing, i)
		}
		if res, err = s.embed(ctx, chunks, missing); err != nil {
			return "", nil, err
		}
	}
	for j, i := range missing {
		vectors[i] = res.Vectors[j]
	}
	return res.Model, vectors, nil
}

// embed は chunks のうち indexes の位置のチャンクを文書として埋め込む。
func (s *Service) embed(ctx context.Context, chunks []Chunk, indexes []int) (*Embeddings, error) {
	texts := make([]string, len(indexes))
	for j, i := range indexes {
		texts[j] = chunks[i].EmbeddingText()
	}
	res, err := s.embedder.Embed(ctx, texts, TaskDocument)
	if err != nil {
		return nil, err
	}
	if err := checkEmbeddings(res, len(texts)); err != nil {
		return nil, err
	}
	return res, nil
}

// checkEmbeddings は Embedder の結果が n 件の入力に対応しているかを確かめる。
func checkEmbeddings(res *Embeddings, n int) error {
	if res == nil || res.Model == "" {
		return fmt.Errorf("%w: missing model", ErrInvalidEmbeddings)
	}
	if len(res.Vectors) != n {
		return fmt.Errorf("%w: got %d vectors for %d texts", ErrInvalidEmbeddings, len(res.Vectors), n)
	}
	return validateVectors(res.Vectors)
}

// Search はクエリに意味的に近い公開記事のチャンクを、類似度の高い順に最大 limit 件返す。
// コサイン類似度が threshold 未満のチャンクは返さない。SearchWithOptions を参照。
func (s *Service) Search(ctx context.Context, query string, limit int, threshold float64) ([]SearchResult, error) {
	return s.SearchWithOptions(ctx, query, SearchOptions{Limit: limit, Threshold: threshold})
}

// SearchWithOptions はクエリを埋め込み、同じモデル・次元数で保存された公開記事のチャンクとのコサイン類似度で
// 上位のチャンクを返す。クエリが空白だけなら nil を返す。embedder が無ければ ErrNotConfigured を返す。
func (s *Service) SearchWithOptions(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	if s.embedder == nil {
		return nil, ErrNotConfigured
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(query) > maxQueryChars {
		query = string([]rune(query)[:maxQueryChars])
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	limit = min(limit, MaxSearchLimit)

	res, err := s.embedder.Embed(ctx, []string{query}, TaskQuery)
	if err != nil {
		return nil, fmt.Errorf("rag: search: %w", err)
	}
	if err := checkEmbeddings(res, 1); err != nil {
		return nil, fmt.Errorf("rag: search: %w", err)
	}
	q := res.Vectors[0]

	var results []SearchResult
	err = s.withReadTx(ctx, func(tx *sqlx.Tx) error {
		// 本文は上位の候補だけを読み、全件の走査ではベクトルだけを読む。
		var rows []struct {
			ID        string `db:"id"`
			ArticleID string `db:"article_id"`
			Embedding []byte `db:"embedding"`
		}
		if err := tx.SelectContext(ctx, &rows, `SELECT e.id, e.article_id, e.embedding
			FROM article_embeddings e JOIN articles a ON a.id = e.article_id
			WHERE e.model = ? AND e.dimensions = ? AND `+article.VisibleCondition, res.Model, len(q), publishedStatus, s.now().UTC()); err != nil {
			return fmt.Errorf("rag: search: load vectors: %w", err)
		}
		exclude := map[string]bool{}
		for _, id := range opts.ExcludeArticleIDs {
			exclude[id] = true
		}
		var ids, articleIDs []string
		var vectors [][]float32
		for _, r := range rows {
			if exclude[r.ArticleID] {
				continue
			}
			v, err := DecodeVector(r.Embedding)
			if err != nil {
				return fmt.Errorf("rag: search: chunk %s: %w", r.ID, err)
			}
			ids = append(ids, r.ID)
			articleIDs = append(articleIDs, r.ArticleID)
			vectors = append(vectors, v)
		}

		k := limit
		if opts.MaxPerArticle > 0 {
			// 記事ごとの上限で間引くため、閾値以上の全件を順位付けしてから選ぶ。
			k = len(vectors)
		}
		var picked []Match
		perArticle := map[string]int{}
		for _, m := range TopK(q, vectors, k, opts.Threshold) {
			if opts.MaxPerArticle > 0 && perArticle[articleIDs[m.Index]] >= opts.MaxPerArticle {
				continue
			}
			perArticle[articleIDs[m.Index]]++
			picked = append(picked, m)
			if len(picked) == limit {
				break
			}
		}
		if len(picked) == 0 {
			return nil
		}

		pickedIDs := make([]string, len(picked))
		for i, m := range picked {
			pickedIDs[i] = ids[m.Index]
		}
		detailQuery, args, err := sqlx.In(`SELECT e.id, e.article_id, e.chunk_index, e.heading, e.chunk_text, a.title, a.slug
			FROM article_embeddings e JOIN articles a ON a.id = e.article_id WHERE e.id IN (?)`, pickedIDs)
		if err != nil {
			return fmt.Errorf("rag: search: build query: %w", err)
		}
		var details []struct {
			ID         string `db:"id"`
			ArticleID  string `db:"article_id"`
			ChunkIndex int    `db:"chunk_index"`
			Heading    string `db:"heading"`
			Text       string `db:"chunk_text"`
			Title      string `db:"title"`
			Slug       string `db:"slug"`
		}
		if err := tx.SelectContext(ctx, &details, tx.Rebind(detailQuery), args...); err != nil {
			return fmt.Errorf("rag: search: load chunks: %w", err)
		}
		byID := make(map[string]int, len(details))
		for i, d := range details {
			byID[d.ID] = i
		}
		for i, m := range picked {
			j, ok := byID[pickedIDs[i]]
			if !ok {
				continue
			}
			d := details[j]
			results = append(results, SearchResult{
				ArticleID: d.ArticleID, ArticleTitle: d.Title, ArticleSlug: d.Slug,
				ChunkIndex: d.ChunkIndex, Heading: d.Heading, Text: d.Text, Score: m.Score,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// FindRelatedArticles は articleID の記事に意味的に近い別の公開記事を、類似度の高い順に最大 limit 件返す。
// コサイン類似度だけで順位付けする。FindRelatedArticlesWithOptions を参照。
func (s *Service) FindRelatedArticles(ctx context.Context, articleID string, limit int) ([]RelatedArticle, error) {
	return s.FindRelatedArticlesWithOptions(ctx, articleID, RelatedOptions{Limit: limit})
}

// FindRelatedArticlesWithOptions は articleID の記事に近い別の公開記事を、スコアの高い順に最大 opts.Limit 件返す。
//
// 記事ごとにチャンクのベクトルを正規化して平均した代表ベクトルを作り、そのコサイン類似度で比べる。
// opts.KeywordScores があれば RelatedCosineWeight × コサイン類似度 + RelatedKeywordWeight × 全文検索スコア
// で順位付けし（負のコサイン類似度は 0 とみなし、全文検索スコアは候補の中の最大値で割って 0〜1 にする）、
// 埋め込みの無い公開記事も全文検索スコアだけで候補にする。
// 順位付けではこのスコアに新しさ補正（recencyBoost、最大 RelatedRecencyBoostMax）を加え、同程度なら新しい記事を上にする。
// 保存済みの埋め込みだけを使うため Embeddings API は呼ばず、embedder が無くても動く。
// 候補が無ければ（記事の埋め込みも全文検索の一致も無ければ）nil を返す。スコアが 0 以下の記事は返さない。
// 関連記事は記事（kind = post）どうしに限り、articleID がページなら nil を返す。
// 上位の候補は diversifyRelated で多様化し、元の記事や既に選んだ記事とアイキャッチが同じ記事・双子記事を飛ばして次点を繰り上げる。
func (s *Service) FindRelatedArticlesWithOptions(ctx context.Context, articleID string, opts RelatedOptions) ([]RelatedArticle, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultRelatedLimit
	}
	keyword := positiveScores(opts.KeywordScores, articleID)
	var related []RelatedArticle
	err := s.withReadTx(ctx, func(tx *sqlx.Tx) error {
		var isPage bool
		if err := tx.GetContext(ctx, &isPage, `SELECT EXISTS (SELECT 1 FROM articles WHERE id = ? AND kind = ?)`,
			articleID, pageKind); err != nil {
			return fmt.Errorf("rag: related: find article: %w", err)
		}
		if isPage {
			return nil
		}
		candidates, err := cosineCandidates(ctx, tx, articleID, s.now().UTC())
		if err != nil {
			return err
		}
		if len(keyword) == 0 {
			related, err = diversifyRelated(ctx, tx, articleID, rankRelated(candidates, relatedPoolSize(limit), s.now().UTC()), limit)
			return err
		}

		// 全文検索だけで一致した記事（埋め込みが未作成・別モデル）のタイトルとスラッグを読み、公開中のものだけを候補に加える。
		known := make(map[string]bool, len(candidates))
		for _, c := range candidates {
			known[c.ArticleID] = true
		}
		var missing []string
		for id := range keyword {
			if !known[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			q, args, err := sqlx.In(`SELECT a.id, a.title, a.slug, a.published_at FROM articles a WHERE `+article.VisibleCondition+` AND a.kind = ? AND a.id IN (?)`,
				publishedStatus, s.now().UTC(), postKind, missing)
			if err != nil {
				return fmt.Errorf("rag: related: build query: %w", err)
			}
			var rows []struct {
				ID          string    `db:"id"`
				Title       string    `db:"title"`
				Slug        string    `db:"slug"`
				PublishedAt time.Time `db:"published_at"`
			}
			if err := tx.SelectContext(ctx, &rows, tx.Rebind(q), args...); err != nil {
				return fmt.Errorf("rag: related: load keyword matches: %w", err)
			}
			for _, r := range rows {
				candidates = append(candidates, RelatedArticle{ArticleID: r.ID, Title: r.Title, Slug: r.Slug, PublishedAt: r.PublishedAt})
			}
		}
		// 全文検索スコアは、候補（公開中の記事）の中の最大値で割って 0〜1 にする。
		var top float64
		for _, c := range candidates {
			top = math.Max(top, keyword[c.ArticleID])
		}
		for i := range candidates {
			c := &candidates[i]
			c.Score = RelatedCosineWeight * math.Max(c.Score, 0)
			if top > 0 {
				c.Score += RelatedKeywordWeight * keyword[c.ArticleID] / top
			}
		}
		related, err = diversifyRelated(ctx, tx, articleID, rankRelated(candidates, relatedPoolSize(limit), s.now().UTC()), limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return related, nil
}

// cosineCandidates は articleID 以外の、now に読者に見えている記事（kind = post）を、articleID の記事の代表ベクトルとの
// コサイン類似度を Score にして返す。比べるのは articleID の記事と同じモデル・次元数で埋め込んだ記事だけ。
// articleID の記事の埋め込みが無ければ nil を返す。
func cosineCandidates(ctx context.Context, tx *sqlx.Tx, articleID string, now time.Time) ([]RelatedArticle, error) {
	var own []struct {
		Model      string `db:"model"`
		Dimensions int    `db:"dimensions"`
		Embedding  []byte `db:"embedding"`
	}
	if err := tx.SelectContext(ctx, &own, `SELECT model, dimensions, embedding FROM article_embeddings
		WHERE article_id = ? ORDER BY chunk_index`, articleID); err != nil {
		return nil, fmt.Errorf("rag: related: load chunks: %w", err)
	}
	if len(own) == 0 {
		return nil, nil
	}
	model, dims := own[0].Model, own[0].Dimensions
	var ownVectors [][]float32
	for _, r := range own {
		if r.Model != model || r.Dimensions != dims {
			continue
		}
		v, err := DecodeVector(r.Embedding)
		if err != nil {
			return nil, fmt.Errorf("rag: related: %w", err)
		}
		ownVectors = append(ownVectors, v)
	}
	query := centroid(ownVectors)
	if query == nil {
		return nil, nil
	}

	var rows []struct {
		ArticleID   string    `db:"article_id"`
		Title       string    `db:"title"`
		Slug        string    `db:"slug"`
		PublishedAt time.Time `db:"published_at"`
		Embedding   []byte    `db:"embedding"`
	}
	if err := tx.SelectContext(ctx, &rows, `SELECT e.article_id, a.title, a.slug, a.published_at, e.embedding
		FROM article_embeddings e JOIN articles a ON a.id = e.article_id
		WHERE e.model = ? AND e.dimensions = ? AND `+article.VisibleCondition+` AND a.kind = ? AND e.article_id <> ?
		ORDER BY e.article_id, e.chunk_index`, model, dims, publishedStatus, now, postKind, articleID); err != nil {
		return nil, fmt.Errorf("rag: related: load vectors: %w", err)
	}
	var candidates []RelatedArticle
	for start := 0; start < len(rows); {
		end := start
		var vectors [][]float32
		for ; end < len(rows) && rows[end].ArticleID == rows[start].ArticleID; end++ {
			v, err := DecodeVector(rows[end].Embedding)
			if err != nil {
				return nil, fmt.Errorf("rag: related: %w", err)
			}
			vectors = append(vectors, v)
		}
		if c := centroid(vectors); c != nil {
			score, err := CosineSimilarity(query, c)
			if err != nil {
				return nil, fmt.Errorf("rag: related: %w", err)
			}
			r := rows[start]
			candidates = append(candidates, RelatedArticle{ArticleID: r.ArticleID, Title: r.Title, Slug: r.Slug, Score: score, PublishedAt: r.PublishedAt})
		}
		start = end
	}
	return candidates, nil
}

// positiveScores は scores のうち正の有限値だけを返す。articleID 自身は除く。該当が無ければ nil を返す。
func positiveScores(scores map[string]float64, articleID string) map[string]float64 {
	var out map[string]float64
	for id, v := range scores {
		if id == articleID || !(v > 0) || math.IsInf(v, 0) {
			continue
		}
		if out == nil {
			out = make(map[string]float64, len(scores))
		}
		out[id] = v
	}
	return out
}

// recencyBoost は now の時点で publishedAt に公開した記事に加える新しさ補正を返す。
// 公開直後は RelatedRecencyBoostMax で、RelatedRecencyHalfLifeYears ごとに半分になる。
// 公開日時か now が不明（ゼロ値）なら 0。公開日時が now より後なら公開直後とみなす。
func recencyBoost(publishedAt, now time.Time) float64 {
	if publishedAt.IsZero() || now.IsZero() {
		return 0
	}
	years := now.Sub(publishedAt).Hours() / (24 * 365.25)
	if years < 0 {
		years = 0
	}
	return RelatedRecencyBoostMax * math.Exp2(-years/RelatedRecencyHalfLifeYears)
}

// rankRelated は Score が正の候補を、Score に新しさ補正（recencyBoost）を加えた値の降順で返す。
// 同程度なら公開日の新しい順、続いて記事 ID 順。now がゼロ値なら補正しない。
// 補正は Score が正の（関連のある）候補の順位付けにだけ使い、返す Score には含めない。
func rankRelated(candidates []RelatedArticle, limit int, now time.Time) []RelatedArticle {
	type ranked struct {
		RelatedArticle
		key float64
	}
	var list []ranked
	for _, c := range candidates {
		if c.Score > 0 {
			list = append(list, ranked{c, c.Score + recencyBoost(c.PublishedAt, now)})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if math.Abs(list[i].key-list[j].key) >= 1e-6 {
			return list[i].key > list[j].key
		}
		if !list[i].PublishedAt.Equal(list[j].PublishedAt) {
			return list[i].PublishedAt.After(list[j].PublishedAt)
		}
		return list[i].ArticleID < list[j].ArticleID
	})
	if len(list) > limit {
		list = list[:limit]
	}
	var out []RelatedArticle
	for _, r := range list {
		out = append(out, r.RelatedArticle)
	}
	return out
}

// 関連記事の多様化（diversifyRelated）の条件。
const (
	// relatedPoolMin は多様化の前に順位付けで残す候補の最小件数。重複を飛ばしても limit 件を埋められるよう多めに取る。
	relatedPoolMin = 10
	// relatedTwinWindow は双子記事（同じ出来事をニュースとブログなどで2回書いた記事）とみなす公開日の差の上限。
	relatedTwinWindow = 7 * 24 * time.Hour
	// relatedTwinContainRunes は、一方のタイトルが他方を含むときに双子とみなす短い方のタイトルの最小文字数。
	relatedTwinContainRunes = 6
	// relatedTwinPrefixRunes は、共通の書き出しで双子とみなす最小文字数（短い方のタイトルの8割以上も必要）。
	// 「量子コンピュータの基礎」と「量子コンピュータ研究会の発表」のような同じ分野の別の記事は双子とみなさない。
	relatedTwinPrefixRunes = 10
	// relatedTwinCommonRunes は、数字（回数・年）を含む共通部分で双子とみなす最小文字数（短い方のタイトルの半分以上も必要）。
	relatedTwinCommonRunes = 8
)

// relatedPoolSize は limit 件を選ぶために順位付けで残す候補の件数を返す。
func relatedPoolSize(limit int) int {
	return max(limit*3, relatedPoolMin)
}

// relatedMeta は多様化で比べる記事の情報（正規化したタイトル・アイキャッチの比較用の名前・公開日時）。
type relatedMeta struct {
	title       []rune
	image       string
	publishedAt time.Time
}

// diversifyRelated は順位の高い順に並んだ ranked から最大 limit 件を選ぶ。元の記事 articleID か既に選んだ記事と
// アイキャッチが同じ（サイズ違いを含む）記事や、公開日が近くタイトルが酷似した双子記事は飛ばし、次点を繰り上げる。
func diversifyRelated(ctx context.Context, tx *sqlx.Tx, articleID string, ranked []RelatedArticle, limit int) ([]RelatedArticle, error) {
	if len(ranked) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(ranked)+1)
	ids = append(ids, articleID)
	for _, r := range ranked {
		ids = append(ids, r.ArticleID)
	}
	q, args, err := sqlx.In(`SELECT id, title, featured_image_url, published_at FROM articles WHERE id IN (?)`, ids)
	if err != nil {
		return nil, fmt.Errorf("rag: related: build query: %w", err)
	}
	var rows []struct {
		ID            string     `db:"id"`
		Title         string     `db:"title"`
		FeaturedImage string     `db:"featured_image_url"`
		PublishedAt   *time.Time `db:"published_at"`
	}
	if err := tx.SelectContext(ctx, &rows, tx.Rebind(q), args...); err != nil {
		return nil, fmt.Errorf("rag: related: load images: %w", err)
	}
	metas := make(map[string]relatedMeta, len(rows))
	for _, r := range rows {
		m := relatedMeta{title: normalizeRelatedTitle(r.Title), image: relatedImageKey(r.FeaturedImage)}
		if r.PublishedAt != nil {
			m.publishedAt = *r.PublishedAt
		}
		metas[r.ID] = m
	}
	seen := []relatedMeta{metas[articleID]}
	var out []RelatedArticle
	for _, r := range ranked {
		if len(out) >= limit {
			break
		}
		m, ok := metas[r.ArticleID]
		if !ok {
			m = relatedMeta{title: normalizeRelatedTitle(r.Title), publishedAt: r.PublishedAt}
		}
		if slices.ContainsFunc(seen, func(o relatedMeta) bool { return duplicateRelated(m, o) }) {
			continue
		}
		seen = append(seen, m)
		out = append(out, r)
	}
	return out, nil
}

// duplicateRelated は a と b を関連記事の欄に並べると重複に見えるか（アイキャッチが同じか、双子記事か）を返す。
func duplicateRelated(a, b relatedMeta) bool {
	if a.image != "" && a.image == b.image {
		return true
	}
	if a.publishedAt.IsZero() || b.publishedAt.IsZero() {
		return false
	}
	if d := a.publishedAt.Sub(b.publishedAt); d > relatedTwinWindow || d < -relatedTwinWindow {
		return false
	}
	return similarTitles(a.title, b.title)
}

// normalizeRelatedTitle はタイトルを比較用に正規化する（NFKC で全角英数を半角に、小文字にし、文字と数字だけを残す）。
func normalizeRelatedTitle(title string) []rune {
	var out []rune
	for _, r := range norm.NFKC.String(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToLower(r))
		}
	}
	return out
}

// similarTitles は正規化したタイトル a・b が同じ出来事の記事に見えるほど似ているかを返す。
// 一方が他方を含む、書き出しの大半が同じ、または回数・年などの数字を含む長い共通部分があれば似ているとみなす。
// 「〜で研究成果を発表しました」のような定型句だけの一致は数字を含まないため似ているとはみなさない。
func similarTitles(a, b []rune) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) == 0 {
		return false
	}
	if len(a) >= relatedTwinContainRunes && strings.Contains(string(b), string(a)) {
		return true
	}
	prefix := 0
	for prefix < len(a) && a[prefix] == b[prefix] {
		prefix++
	}
	if prefix >= relatedTwinPrefixRunes && prefix*10 >= len(a)*8 {
		return true
	}
	common := longestCommonDigitRun(a, b)
	return common >= relatedTwinCommonRunes && common*2 >= len(a)
}

// longestCommonDigitRun は a と b の共通部分文字列のうち、数字を含む最長のものの長さを返す。
func longestCommonDigitRun(a, b []rune) int {
	best := 0
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] != b[j-1] {
				cur[j] = 0
				continue
			}
			cur[j] = prev[j-1] + 1
			if n := cur[j]; n > best && slices.ContainsFunc(a[i-n:i], unicode.IsDigit) {
				best = n
			}
		}
		prev, cur = cur, prev
	}
	return best
}

// relatedImageVariant はファイル名の末尾にある、同じ画像の別サイズを表す接尾辞に一致する。
// 本 CMS の縮小版（-original・-1200・-800・-400。image.WidthLarge などと同じ値）と、WordPress の縮小版（-300x200・-scaled）。
var relatedImageVariant = regexp.MustCompile(`(?:-(?:original|1200|800|400)|(?:-scaled)?(?:-\d+x\d+)?)$`)

// relatedImageKey はアイキャッチの URL から、サイズ違いを同じとみなす比較用の名前（ディレクトリ・拡張子・縮小版の
// 接尾辞を除いた小文字のファイル名）を返す。URL が空なら空文字を返す。
func relatedImageKey(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.IndexAny(ref, "?#"); i >= 0 {
		ref = ref[:i]
	}
	if ref == "" {
		return ""
	}
	if u, err := url.PathUnescape(ref); err == nil {
		ref = u
	}
	name := strings.ToLower(path.Base(ref))
	name = strings.TrimSuffix(name, path.Ext(name))
	if stem := relatedImageVariant.ReplaceAllString(name, ""); stem != "" {
		return stem
	}
	return name
}

// withReadTx は読み取りのトランザクション内で fn を実行する。複数のクエリを同じスナップショットで読み、
// 途中で IndexArticle がチャンクを置き換えても結果が食い違わないようにする。
func (s *Service) withReadTx(ctx context.Context, fn func(tx *sqlx.Tx) error) error {
	tx, err := s.db.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("rag: begin read: %w", err)
	}
	defer tx.Rollback()
	return fn(tx)
}

// withWriteTx は BEGIN IMMEDIATE の書き込みトランザクション内で fn を実行し、成功すればコミットする。
// 書き込みロックを先に取り、同じ記事の並行な IndexArticle が (article_id, chunk_index) で衝突しないようにする。
func (s *Service) withWriteTx(ctx context.Context, fn func(conn *sqlx.Conn) error) error {
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return fmt.Errorf("rag: acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("rag: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// ctx がキャンセル済みでも確実に巻き戻し、トランザクションを開いたまま接続をプールに返さない。
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("rag: commit: %w", err)
	}
	committed = true
	return nil
}

// newID は crypto/rand による16バイトの乱数を hex で返す。
func newID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rag: generate chunk id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
