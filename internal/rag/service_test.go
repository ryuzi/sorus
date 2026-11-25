package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/database"
)

// vocab は fakeEmbedder の次元に対応する語。テキスト中の出現回数がその次元の値になる。
var vocab = []string{"量子", "計算", "物理", "サッカー", "試合", "優勝", "料理", "レシピ", "留学", "英語"}

// fakeEmbedder は語の出現回数をベクトルにする決定的な Embedder。呼び出しを記録する。
type fakeEmbedder struct {
	mu    sync.Mutex
	model string
	// actual は Embed の結果のモデル。空なら model。Model() の予告と異なるモデルが返る場合（Gemini の後継モデルへの切り替え）を模す。
	actual string
	err    error
	short  bool // true なら入力より1件少ないベクトルを返す
	calls  [][]string
	tasks  []TaskType
}

func newFakeEmbedder() *fakeEmbedder { return &fakeEmbedder{model: "fake-v1"} }

func (f *fakeEmbedder) Name() string { return "Fake (" + f.model + ")" }

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, task TaskType) (*Embeddings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), texts...))
	f.tasks = append(f.tasks, task)
	if f.err != nil {
		return nil, f.err
	}
	res := &Embeddings{Model: f.model}
	if f.actual != "" {
		res.Model = f.actual
	}
	for _, text := range texts {
		res.Vectors = append(res.Vectors, fakeVector(text))
	}
	if f.short && len(res.Vectors) > 0 {
		res.Vectors = res.Vectors[1:]
	}
	return res, nil
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeEmbedder) lastCall() ([]string, TaskType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1], f.tasks[len(f.tasks)-1]
}

// fakeVector は vocab の出現回数に、ゼロベクトルを避けるための小さな共通成分を足したベクトル。
func fakeVector(text string) []float32 {
	v := make([]float32, len(vocab)+1)
	for i, w := range vocab {
		v[i] = float32(strings.Count(text, w))
	}
	v[len(vocab)] = 0.1
	return v
}

func openTestDB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("Connect(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, role) VALUES ('u1', 'writer', 'h', 'writer')`)
	mustExec(t, db, `INSERT INTO categories (id, name, slug) VALUES (1, 'News', 'news')`)
	return db
}

func mustExec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// insertArticle は記事を入れる。公開の記事は1時間前に公開した（読者に見えている）記事にする。
func insertArticle(t *testing.T, db *sqlx.DB, id, title, status string) {
	t.Helper()
	var publishedAt *time.Time
	if status == "published" {
		at := time.Now().UTC().Add(-time.Hour)
		publishedAt = &at
	}
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, published_at) VALUES (?, ?, ?, 'u1', 1, ?, ?)`,
		id, title, "slug-"+id, status, publishedAt)
}

func mustIndex(t *testing.T, s *Service, articleID, markdown string) {
	t.Helper()
	if err := s.IndexArticle(context.Background(), articleID, markdown); err != nil {
		t.Fatalf("IndexArticle(%s): %v", articleID, err)
	}
}

type storedRow struct {
	ChunkIndex int    `db:"chunk_index"`
	Heading    string `db:"heading"`
	Text       string `db:"chunk_text"`
	Dimensions int    `db:"dimensions"`
	Model      string `db:"model"`
	Embedding  []byte `db:"embedding"`
}

func storedRows(t *testing.T, db *sqlx.DB, articleID string) []storedRow {
	t.Helper()
	var rows []storedRow
	if err := db.Select(&rows, `SELECT chunk_index, heading, chunk_text, dimensions, model, embedding
		FROM article_embeddings WHERE article_id = ? ORDER BY chunk_index`, articleID); err != nil {
		t.Fatal(err)
	}
	return rows
}

const labMarkdown = `量子計算の研究室を紹介します。

## 研究テーマ

量子計算と物理シミュレーションを研究しています。

## メンバー

留学生も在籍し、英語で議論しています。
`

func TestIndexArticleStoresChunks(t *testing.T) {
	db := openTestDB(t, ":memory:")
	insertArticle(t, db, "a1", "研究室紹介", "draft")
	f := newFakeEmbedder()
	s := NewService(db, f)
	if !s.Enabled() {
		t.Fatal("Enabled() = false with an embedder")
	}

	mustIndex(t, s, "a1", labMarkdown)

	chunks := ChunkMarkdown(labMarkdown)
	rows := storedRows(t, db, "a1")
	if len(rows) != len(chunks) || len(chunks) != 3 {
		t.Fatalf("stored %d chunks, chunker produced %d, want 3", len(rows), len(chunks))
	}
	texts, task := f.lastCall()
	if task != TaskDocument {
		t.Errorf("task = %v, want TaskDocument", task)
	}
	for i, r := range rows {
		c := chunks[i]
		if r.ChunkIndex != i || r.Heading != c.Heading || r.Text != c.Text || r.Model != "fake-v1" || r.Dimensions != len(vocab)+1 {
			t.Errorf("row %d = {%d %q %q %s %d}, want chunk %+v from fake-v1", i, r.ChunkIndex, r.Heading, r.Text, r.Model, r.Dimensions, c)
		}
		// 見出しの文脈を付けたテキストを埋め込む。
		if texts[i] != c.EmbeddingText() {
			t.Errorf("embedded text %d = %q, want %q", i, texts[i], c.EmbeddingText())
		}
		v, err := DecodeVector(r.Embedding)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(v) != fmt.Sprint(fakeVector(c.EmbeddingText())) {
			t.Errorf("row %d embedding = %v, want %v", i, v, fakeVector(c.EmbeddingText()))
		}
	}
	if rows[1].Heading != "研究テーマ" || !strings.HasPrefix(texts[1], "[見出し: 研究テーマ] ") {
		t.Errorf("heading context not kept: %q / %q", rows[1].Heading, texts[1])
	}
}

func TestRankRelatedTieBreaksByPublicationDate(t *testing.T) {
	old := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	newer := old.AddDate(1, 0, 0)
	got := rankRelated([]RelatedArticle{
		{ArticleID: "b", Score: 0.8, PublishedAt: old},
		{ArticleID: "z", Score: 0.8 + 0.5e-6, PublishedAt: newer},
		{ArticleID: "a", Score: 0.8, PublishedAt: newer},
		{ArticleID: "c", Score: 0.8 + 2e-6, PublishedAt: old},
	}, 10, time.Time{}) // 新しさ補正なし
	want := []string{"c", "a", "z", "b"}
	if len(got) != len(want) {
		t.Fatalf("rankRelated returned %d items, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ArticleID != id {
			t.Errorf("rankRelated[%d] = %q, want %q", i, got[i].ArticleID, id)
		}
	}
}

func TestRecencyBoost(t *testing.T) {
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	if got := recencyBoost(now, now); math.Abs(got-RelatedRecencyBoostMax) > 1e-12 {
		t.Errorf("boost at publish time = %v, want %v", got, RelatedRecencyBoostMax)
	}
	if got := recencyBoost(now.Add(24*time.Hour), now); math.Abs(got-RelatedRecencyBoostMax) > 1e-12 {
		t.Errorf("boost of a future date = %v, want the max %v", got, RelatedRecencyBoostMax)
	}
	halfLife := now.Add(-time.Duration(RelatedRecencyHalfLifeYears * 365.25 * 24 * float64(time.Hour)))
	if got := recencyBoost(halfLife, now); math.Abs(got-RelatedRecencyBoostMax/2) > 1e-9 {
		t.Errorf("boost after the half-life = %v, want %v", got, RelatedRecencyBoostMax/2)
	}
	if got := recencyBoost(time.Time{}, now); got != 0 {
		t.Errorf("boost of an unknown date = %v, want 0", got)
	}
	if got := recencyBoost(now, time.Time{}); got != 0 {
		t.Errorf("boost without a clock = %v, want 0", got)
	}
	// 新しいほど大きく、10 年前の記事は直近の記事より明らかに小さい。
	recent := recencyBoost(time.Date(2025, time.April, 1, 0, 0, 0, 0, time.UTC), now)
	decade := recencyBoost(time.Date(2015, time.April, 1, 0, 0, 0, 0, time.UTC), now)
	if !(recent > decade) || recent-decade < 0.03 || recent > RelatedRecencyBoostMax || decade <= 0 {
		t.Errorf("boost 2025 = %v, 2015 = %v, want 2025 > 2015 by >= 0.03 and both in (0, %v]", recent, decade, RelatedRecencyBoostMax)
	}
}

// 類似度が同程度なら、10 年前（2013〜2017 年）の記事より直近（2024〜2026 年）の記事が上位 3 件に入る。
// 類似度の差が補正の差より大きければ、古い記事でも上位のまま。返す Score は補正を含まない。
func TestRankRelatedRecencyBoost(t *testing.T) {
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	year := func(y int) time.Time { return time.Date(y, time.June, 1, 0, 0, 0, 0, time.UTC) }
	candidates := []RelatedArticle{
		{ArticleID: "old-2013", Score: 0.52, PublishedAt: year(2013)},
		{ArticleID: "old-2015", Score: 0.53, PublishedAt: year(2015)},
		{ArticleID: "old-2017", Score: 0.51, PublishedAt: year(2017)},
		{ArticleID: "new-2024", Score: 0.50, PublishedAt: year(2024)},
		{ArticleID: "new-2025", Score: 0.50, PublishedAt: year(2025)},
		{ArticleID: "new-2026", Score: 0.49, PublishedAt: year(2026)},
		{ArticleID: "strong-2014", Score: 0.70, PublishedAt: year(2014)},
		{ArticleID: "unrelated-2026", Score: 0, PublishedAt: year(2026)},
	}
	got := rankRelated(candidates, 4, now)
	want := []string{"strong-2014", "new-2026", "new-2025", "new-2024"}
	if ids := articleIDs(got, relatedArticle); fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("rankRelated with recency = %v, want %v", ids, want)
	}
	for _, r := range got {
		for _, c := range candidates {
			if c.ArticleID == r.ArticleID && c.Score != r.Score {
				t.Errorf("%s Score = %v, want the unboosted %v", r.ArticleID, r.Score, c.Score)
			}
		}
	}
	// 補正なしなら類似度の順のまま（古い記事が上位）。
	plain := rankRelated(candidates, 4, time.Time{})
	if ids := articleIDs(plain, relatedArticle); fmt.Sprint(ids) != "[strong-2014 old-2015 old-2013 old-2017]" {
		t.Errorf("rankRelated without recency = %v", ids)
	}
}

// FindRelatedArticlesWithOptions も新しさ補正で順位付けする。全文検索の一致が同程度なら新しい記事が上位 3 件に入る。
func TestFindRelatedArticlesPrefersRecentArticles(t *testing.T) {
	db := openTestDB(t, ":memory:")
	s := NewService(db, newFakeEmbedder())
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	insertArticle(t, db, "self", "基準の記事", "draft")
	keyword := map[string]float64{}
	for id, y := range map[string]int{"old-2013": 2013, "old-2015": 2015, "old-2017": 2017, "new-2024": 2024, "new-2025": 2025, "new-2026": 2026} {
		insertArticle(t, db, id, id, "published")
		mustExec(t, db, `UPDATE articles SET published_at = ? WHERE id = ?`, time.Date(y, time.March, 1, 0, 0, 0, 0, time.UTC), id)
		keyword[id] = 10
		if strings.HasPrefix(id, "old-") {
			keyword[id] = 10.5 // 語の一致は古い記事のほうがわずかに多い。
		}
	}
	related, err := s.FindRelatedArticlesWithOptions(context.Background(), "self", RelatedOptions{Limit: 3, KeywordScores: keyword})
	if err != nil {
		t.Fatal(err)
	}
	if ids := articleIDs(related, relatedArticle); fmt.Sprint(ids) != "[new-2026 new-2025 new-2024]" {
		t.Errorf("related = %v, want [new-2026 new-2025 new-2024]", ids)
	}
}

func TestIndexArticleReusesUnchangedChunks(t *testing.T) {
	db := openTestDB(t, ":memory:")
	insertArticle(t, db, "a1", "研究室紹介", "published")
	f := newFakeEmbedder()
	s := NewService(db, f)

	mustIndex(t, s, "a1", labMarkdown)
	if n := f.callCount(); n != 1 {
		t.Fatalf("embed calls = %d, want 1", n)
	}

	// 内容が同じなら API を呼ばない。
	mustIndex(t, s, "a1", labMarkdown)
	if n := f.callCount(); n != 1 {
		t.Errorf("embed calls after re-indexing the same content = %d, want 1", n)
	}
	if n := len(storedRows(t, db, "a1")); n != 3 {
		t.Errorf("stored %d chunks, want 3", n)
	}

	// 変わったチャンクだけを送る。
	changed := strings.Replace(labMarkdown, "英語で議論しています。", "英語で議論し、サッカーの試合も観ます。", 1)
	mustIndex(t, s, "a1", changed)
	texts, _ := f.lastCall()
	if f.callCount() != 2 || len(texts) != 1 || !strings.Contains(texts[0], "サッカー") {
		t.Errorf("embed calls = %d, last texts = %q, want only the changed chunk", f.callCount(), texts)
	}
	rows := storedRows(t, db, "a1")
	if len(rows) != 3 || !strings.Contains(rows[2].Text, "サッカー") {
		t.Fatalf("rows after change = %+v", rows)
	}
	v, _ := DecodeVector(rows[2].Embedding)
	if fmt.Sprint(v) != fmt.Sprint(fakeVector(texts[0])) {
		t.Errorf("changed chunk embedding = %v, want the new vector", v)
	}

	// チャンクが減っただけなら API を呼ばずに置き換える。
	shorter := strings.SplitN(changed, "## メンバー", 2)[0]
	mustIndex(t, s, "a1", shorter)
	if n := f.callCount(); n != 2 {
		t.Errorf("embed calls after removing a section = %d, want 2", n)
	}
	if rows := storedRows(t, db, "a1"); len(rows) != 2 {
		t.Errorf("stored %d chunks after removing a section, want 2", len(rows))
	}

	// Embedder のモデルが変わったら、内容が同じでも全チャンクを新しいモデルで生成し直す。
	f.model = "fake-v2"
	mustIndex(t, s, "a1", shorter)
	texts, _ = f.lastCall()
	if n := f.callCount(); n != 3 || len(texts) != 2 {
		t.Fatalf("embed calls after model change = %d with %d texts, want 3 with all 2 chunks", n, len(texts))
	}
	assertModel(t, db, "a1", "fake-v2")

	// 応答のモデルが予告と異なれば、変わったチャンクを生成した後、混在させないよう全チャンクを生成し直す。
	f.actual = "fake-v3"
	mustIndex(t, s, "a1", shorter+"\n\n料理も得意です。")
	texts, _ = f.lastCall()
	if n := f.callCount(); n != 5 || len(texts) != 2 {
		t.Fatalf("embed calls after unexpected model = %d with %d texts, want 5 (changed chunk, then all 2 chunks)", n, len(texts))
	}
	assertModel(t, db, "a1", "fake-v3")
}

func assertModel(t *testing.T, db *sqlx.DB, articleID, model string) {
	t.Helper()
	for _, r := range storedRows(t, db, articleID) {
		if r.Model != model {
			t.Errorf("chunk %d model = %s, want %s", r.ChunkIndex, r.Model, model)
		}
	}
}

func TestIndexArticleEmptyMarkdownRemovesChunks(t *testing.T) {
	db := openTestDB(t, ":memory:")
	insertArticle(t, db, "a1", "T", "published")
	f := newFakeEmbedder()
	s := NewService(db, f)
	mustIndex(t, s, "a1", labMarkdown)
	mustIndex(t, s, "a1", "  \n# 見出しだけ\n")
	if n := len(storedRows(t, db, "a1")); n != 0 {
		t.Errorf("stored %d chunks after emptying the article, want 0", n)
	}
	if n := f.callCount(); n != 1 {
		t.Errorf("embed calls = %d, want 1", n)
	}
}

func TestIndexArticleErrors(t *testing.T) {
	db := openTestDB(t, ":memory:")
	insertArticle(t, db, "a1", "T", "published")
	f := newFakeEmbedder()
	s := NewService(db, f)
	mustIndex(t, s, "a1", labMarkdown)
	before := storedRows(t, db, "a1")

	if err := NewService(db, nil).IndexArticle(context.Background(), "a1", "量子"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("IndexArticle without embedder: err = %v, want ErrNotConfigured", err)
	}
	if err := s.IndexArticle(context.Background(), "no-such-article", labMarkdown); !errors.Is(err, ErrArticleNotFound) {
		t.Errorf("IndexArticle unknown article: err = %v, want ErrArticleNotFound", err)
	}
	if n := f.callCount(); n != 1 {
		t.Errorf("embed calls = %d, want no call for a missing article", n)
	}

	// API が失敗・不正な応答をしたら、既存のチャンクを残す。
	apiErr := &APIError{Provider: "Fake", StatusCode: 429, Message: "quota exceeded"}
	f.err = apiErr
	err := s.IndexArticle(context.Background(), "a1", "新しい本文の量子")
	var gotAPIErr *APIError
	if !errors.As(err, &gotAPIErr) || gotAPIErr.StatusCode != 429 {
		t.Errorf("IndexArticle with API error: err = %v, want *APIError 429", err)
	}
	f.err = nil
	f.short = true
	if err := s.IndexArticle(context.Background(), "a1", "新しい本文の量子"); !errors.Is(err, ErrInvalidEmbeddings) {
		t.Errorf("IndexArticle with short response: err = %v, want ErrInvalidEmbeddings", err)
	}
	if after := storedRows(t, db, "a1"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("chunks changed by failed indexing")
	}
}

// searchFixture は公開・下書き・削除済みの記事を索引した Service を返す。
func searchFixture(t *testing.T) (*sqlx.DB, *fakeEmbedder, *Service) {
	t.Helper()
	db := openTestDB(t, ":memory:")
	f := newFakeEmbedder()
	s := NewService(db, f)
	articles := []struct{ id, title, status, markdown string }{
		{"quantum", "量子計算の最前線", "published", "## 概要\n\n量子計算と量子物理の研究です。\n\n## 応用\n\n量子計算で化学計算を高速化します。"},
		{"physics", "物理学科の一日", "published", "物理の実験と量子力学の講義があります。"},
		{"soccer", "サッカー部が優勝", "published", "サッカー部が試合に勝ち、リーグ優勝しました。"},
		{"cooking", "簡単レシピ", "published", "料理サークルのレシピを紹介します。"},
		{"draft", "量子の下書き", "draft", "量子計算の量子計算による量子計算。"},
		{"trash", "削除済みの量子", "deleted", "量子計算の量子計算による量子計算。"},
	}
	for _, a := range articles {
		insertArticle(t, db, a.id, a.title, a.status)
		mustIndex(t, s, a.id, a.markdown)
	}
	return db, f, s
}

func articleIDs[T any](items []T, id func(T) string) []string {
	var out []string
	for _, it := range items {
		out = append(out, id(it))
	}
	return out
}

func resultArticle(r SearchResult) string { return r.ArticleID }

func TestSearch(t *testing.T) {
	_, f, s := searchFixture(t)
	ctx := context.Background()

	results, err := s.Search(ctx, "量子計算について", 10, 0.5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, task := f.lastCall(); task != TaskQuery {
		t.Errorf("query task = %v, want TaskQuery", task)
	}
	ids := articleIDs(results, resultArticle)
	if len(results) == 0 || results[0].ArticleID != "quantum" {
		t.Fatalf("results = %v, want quantum first", ids)
	}
	for i, r := range results {
		if r.ArticleID == "draft" || r.ArticleID == "trash" {
			t.Errorf("unpublished article %s in results", r.ArticleID)
		}
		if r.ArticleID == "soccer" || r.ArticleID == "cooking" {
			t.Errorf("unrelated article %s scored above threshold: %v", r.ArticleID, r.Score)
		}
		if r.Score < 0.5 || r.Score > 1 || (i > 0 && r.Score > results[i-1].Score) {
			t.Errorf("result %d score %v is out of range or order", i, r.Score)
		}
	}
	top := results[0]
	if top.ArticleTitle != "量子計算の最前線" || top.ArticleSlug != "slug-quantum" || top.Heading == "" || !strings.Contains(top.Text, "量子計算") {
		t.Errorf("top result = %+v, want article title, slug, heading and chunk text", top)
	}

	if results, err := s.Search(ctx, "量子計算について", 1, 0); err != nil || len(results) != 1 {
		t.Errorf("Search with limit 1 = %d results, %v", len(results), err)
	}
	// limit 0 は既定の件数。
	if results, err := s.Search(ctx, "量子", 0, -1); err != nil || len(results) != DefaultSearchLimit {
		t.Errorf("Search with limit 0 = %d results, %v, want %d", len(results), err, DefaultSearchLimit)
	}

	calls := f.callCount()
	if results, err := s.Search(ctx, " \n ", 5, 0); err != nil || results != nil || f.callCount() != calls {
		t.Errorf("Search with blank query = %v, %v (calls %d -> %d), want nil without API call", results, err, calls, f.callCount())
	}
	if _, err := NewService(s.db, nil).Search(ctx, "量子", 5, 0); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Search without embedder: err = %v, want ErrNotConfigured", err)
	}
}

func TestSearchQueryIsTruncated(t *testing.T) {
	_, f, s := searchFixture(t)
	if _, err := s.Search(context.Background(), strings.Repeat("量", 5000), 5, 0); err != nil {
		t.Fatal(err)
	}
	texts, _ := f.lastCall()
	if n := len([]rune(texts[0])); n != maxQueryChars {
		t.Errorf("embedded query has %d chars, want %d", n, maxQueryChars)
	}
}

func TestSearchWithOptions(t *testing.T) {
	_, _, s := searchFixture(t)
	ctx := context.Background()

	// quantum は2チャンクとも上位に来るが、記事ごとに1件に絞れる。
	all, err := s.Search(ctx, "量子計算", 3, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if got := articleIDs(all, resultArticle); fmt.Sprint(got) != "[quantum quantum physics]" {
		t.Errorf("Search = %v, want [quantum quantum physics]", got)
	}
	distinct, err := s.SearchWithOptions(ctx, "量子計算", SearchOptions{Limit: 3, Threshold: 0.3, MaxPerArticle: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := articleIDs(distinct, resultArticle); fmt.Sprint(got) != "[quantum physics]" {
		t.Errorf("Search with MaxPerArticle 1 = %v, want [quantum physics]", got)
	}
	excluded, err := s.SearchWithOptions(ctx, "量子計算", SearchOptions{Limit: 3, Threshold: 0.3, ExcludeArticleIDs: []string{"quantum"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := articleIDs(excluded, resultArticle); fmt.Sprint(got) != "[physics]" {
		t.Errorf("Search excluding quantum = %v, want [physics]", got)
	}
}

func TestSearchFollowsArticleStatus(t *testing.T) {
	db, _, s := searchFixture(t)
	ctx := context.Background()
	// 公開すると、索引し直さなくても検索の対象になる。
	mustExec(t, db, `UPDATE articles SET status = 'published', published_at = ? WHERE id = 'draft'`, time.Now().UTC().Add(-time.Minute))
	mustExec(t, db, `UPDATE articles SET status = 'draft' WHERE id = 'quantum'`)
	results, err := s.Search(ctx, "量子計算", 10, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	ids := articleIDs(results, resultArticle)
	if len(ids) == 0 || ids[0] != "draft" || strings.Contains(fmt.Sprint(ids), "quantum") {
		t.Errorf("results = %v, want the newly published article first and no unpublished one", ids)
	}
}

func TestSearchIgnoresOtherModels(t *testing.T) {
	_, f, s := searchFixture(t)
	f.model = "fake-v2"
	results, err := s.Search(context.Background(), "量子計算", 10, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("results with a different query model = %v, want none", articleIDs(results, resultArticle))
	}
	// 新しいモデルで索引し直した記事だけが対象になる。
	mustIndex(t, s, "physics", "物理の実験と量子力学の講義があります。")
	results, err = s.Search(context.Background(), "量子計算", 10, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got := articleIDs(results, resultArticle); fmt.Sprint(got) != "[physics]" {
		t.Errorf("results = %v, want [physics]", got)
	}
}

func relatedArticle(r RelatedArticle) string { return r.ArticleID }

func TestFindRelatedArticles(t *testing.T) {
	db, f, s := searchFixture(t)
	ctx := context.Background()

	related, err := s.FindRelatedArticles(ctx, "quantum", 4)
	if err != nil {
		t.Fatalf("FindRelatedArticles: %v", err)
	}
	ids := articleIDs(related, relatedArticle)
	if len(related) == 0 || related[0].ArticleID != "physics" {
		t.Fatalf("related = %v, want physics first", ids)
	}
	if len(related) > 4 {
		t.Errorf("got %d related articles, want <= 4", len(related))
	}
	for i, r := range related {
		switch r.ArticleID {
		case "quantum", "draft", "trash":
			t.Errorf("related includes %s (itself or unpublished)", r.ArticleID)
		}
		if r.Score <= 0 || r.Score > 1 || (i > 0 && r.Score > related[i-1].Score) {
			t.Errorf("related %d score %v is out of range or order", i, r.Score)
		}
	}
	if related[0].Title != "物理学科の一日" || related[0].Slug != "slug-physics" {
		t.Errorf("related[0] = %+v, want title and slug of physics", related[0])
	}

	// 埋め込みは保存済みなので、API を呼ばず、Embedder が無くても求まる。
	calls := f.callCount()
	offline, err := NewService(db, nil).FindRelatedArticles(ctx, "quantum", 4)
	if err != nil || fmt.Sprint(offline) != fmt.Sprint(related) {
		t.Errorf("FindRelatedArticles without embedder = %v, %v, want %v", offline, err, related)
	}
	if f.callCount() != calls {
		t.Errorf("FindRelatedArticles called the embeddings API")
	}

	if top, err := s.FindRelatedArticles(ctx, "quantum", 1); err != nil || len(top) != 1 || top[0].ArticleID != "physics" {
		t.Errorf("FindRelatedArticles limit 1 = %v, %v", top, err)
	}
	if def, err := s.FindRelatedArticles(ctx, "quantum", 0); err != nil || len(def) != min(DefaultRelatedLimit, 3) {
		t.Errorf("FindRelatedArticles limit 0 = %d articles, %v, want the 3 other published articles", len(def), err)
	}
	// 下書きの記事でも、公開記事から関連記事を求められる。
	if fromDraft, err := s.FindRelatedArticles(ctx, "draft", 4); err != nil || len(fromDraft) == 0 || fromDraft[0].ArticleID != "quantum" {
		t.Errorf("FindRelatedArticles(draft) = %v, %v, want quantum first", articleIDs(fromDraft, relatedArticle), err)
	}
	if none, err := s.FindRelatedArticles(ctx, "not-indexed", 4); err != nil || none != nil {
		t.Errorf("FindRelatedArticles of an unindexed article = %v, %v, want nil", none, err)
	}
}

func TestFindRelatedArticlesHybrid(t *testing.T) {
	db, _, s := searchFixture(t)
	ctx := context.Background()
	// 埋め込みがまだ無い公開記事と下書き。全文検索で一致しても、公開中のものだけが候補になる。
	insertArticle(t, db, "no-embedding", "埋め込み未作成の記事", "published")
	insertArticle(t, db, "draft-no-embedding", "下書き", "draft")

	cosineOnly, err := s.FindRelatedArticles(ctx, "quantum", 10)
	if err != nil {
		t.Fatal(err)
	}
	cos := map[string]float64{}
	for _, r := range cosineOnly {
		cos[r.ArticleID] = r.Score
	}
	if ids := articleIDs(cosineOnly, relatedArticle); fmt.Sprint(ids) != "[physics cooking soccer]" {
		t.Fatalf("fixture: cosine-only related = %v", ids)
	}

	keyword := map[string]float64{
		"soccer": 8, "no-embedding": 4,
		"quantum": 100, "draft": 50, "draft-no-embedding": 50, "trash": 50, // 自身・非公開は除く（正規化の最大値にも使わない）。
		"cooking": -1,
	}
	related, err := s.FindRelatedArticlesWithOptions(ctx, "quantum", RelatedOptions{Limit: 10, KeywordScores: keyword})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"soccer":       RelatedCosineWeight*cos["soccer"] + RelatedKeywordWeight*1,
		"physics":      RelatedCosineWeight * cos["physics"],
		"no-embedding": RelatedKeywordWeight * 0.5,
		"cooking":      RelatedCosineWeight * cos["cooking"],
	}
	if len(related) != len(want) {
		t.Fatalf("related = %v, want %d articles", articleIDs(related, relatedArticle), len(want))
	}
	for i, r := range related {
		w, ok := want[r.ArticleID]
		if !ok || math.Abs(r.Score-w) > 1e-9 {
			t.Errorf("related %s score = %v, want %v (in %v)", r.ArticleID, r.Score, w, ok)
		}
		if i > 0 && r.Score > related[i-1].Score {
			t.Errorf("related is not sorted by score: %v", articleIDs(related, relatedArticle))
		}
	}
	// 語の重なりが大きいサッカーの記事が、コサイン類似度だけのときの1位（物理）を上回る。
	if ids := articleIDs(related, relatedArticle); fmt.Sprint(ids) != "[soccer physics no-embedding cooking]" {
		t.Errorf("related = %v, want [soccer physics no-embedding cooking]", ids)
	}
	for _, r := range related {
		if r.ArticleID == "no-embedding" && (r.Title != "埋め込み未作成の記事" || r.Slug != "slug-no-embedding") {
			t.Errorf("keyword-only candidate = %+v, want title and slug", r)
		}
	}

	// 埋め込みの無い記事でも、全文検索の一致だけで関連記事を求められる。
	fromKeyword, err := s.FindRelatedArticlesWithOptions(ctx, "not-indexed", RelatedOptions{KeywordScores: map[string]float64{"cooking": 3, "soccer": 6}})
	if err != nil || fmt.Sprint(articleIDs(fromKeyword, relatedArticle)) != "[soccer cooking]" ||
		math.Abs(fromKeyword[0].Score-RelatedKeywordWeight) > 1e-9 {
		t.Errorf("keyword-only related = %+v, %v", fromKeyword, err)
	}
	// 一致が無ければコサイン類似度だけの結果と同じ。
	if same, err := s.FindRelatedArticlesWithOptions(ctx, "quantum", RelatedOptions{Limit: 10, KeywordScores: map[string]float64{"quantum": 9}}); err != nil ||
		fmt.Sprint(same) != fmt.Sprint(cosineOnly) {
		t.Errorf("related with only self keyword match = %v, %v, want %v", same, err, cosineOnly)
	}
}

func TestFindRelatedArticlesUsesWholeArticle(t *testing.T) {
	db := openTestDB(t, ":memory:")
	s := NewService(db, newFakeEmbedder())
	// target はサッカーの話題が中心で、量子の話題は1段落だけ。
	// 記事全体の代表ベクトルで比べるため、量子の1段落と完全に一致する記事より、サッカーの記事が上位に来る。
	for _, a := range []struct{ id, markdown string }{
		{"target", "## 試合\n\nサッカーの試合で優勝。\n\n## 優勝\n\nサッカー優勝。\n\n## 余談\n\n量子計算。"},
		{"soccer", "サッカーの試合に勝ち優勝した。"},
		{"quantum", "量子計算。"},
	} {
		insertArticle(t, db, a.id, a.id, "published")
		mustIndex(t, s, a.id, a.markdown)
	}
	related, err := s.FindRelatedArticles(context.Background(), "target", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := articleIDs(related, relatedArticle); fmt.Sprint(got) != "[soccer quantum]" {
		t.Errorf("related = %v, want [soccer quantum]", got)
	}
}

func TestDeletingArticleRemovesEmbeddings(t *testing.T) {
	db, _, s := searchFixture(t)
	mustExec(t, db, `DELETE FROM articles WHERE id = 'physics'`)
	if n := len(storedRows(t, db, "physics")); n != 0 {
		t.Errorf("stored %d chunks after deleting the article, want 0", n)
	}
	related, err := s.FindRelatedArticles(context.Background(), "quantum", 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range related {
		if r.ArticleID == "physics" {
			t.Error("deleted article is still related")
		}
	}
}

func TestIndexArticleConcurrent(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "rag.db"))
	insertArticle(t, db, "a1", "T", "published")
	s := NewService(db, newFakeEmbedder())

	variants := make([]string, 8)
	for i := range variants {
		var b strings.Builder
		for j := 0; j <= i; j++ {
			fmt.Fprintf(&b, "## 節%d\n\n版%dの本文%d、量子計算。\n\n", j, i, j)
		}
		variants[i] = b.String()
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(variants)*3)
	for round := 0; round < 3; round++ {
		for _, md := range variants {
			wg.Add(1)
			go func(md string) {
				defer wg.Done()
				errs <- s.IndexArticle(context.Background(), "a1", md)
			}(md)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent IndexArticle: %v", err)
		}
	}

	// 最後に書き込んだ版のチャンクだけが、欠けも重複もなく残る。
	rows := storedRows(t, db, "a1")
	var texts []string
	for i, r := range rows {
		if r.ChunkIndex != i {
			t.Errorf("chunk_index %d at position %d", r.ChunkIndex, i)
		}
		texts = append(texts, r.Text)
	}
	matched := false
	for _, md := range variants {
		var want []string
		for _, c := range ChunkMarkdown(md) {
			want = append(want, c.Text)
		}
		if fmt.Sprint(want) == fmt.Sprint(texts) {
			matched = true
		}
	}
	if !matched {
		sort.Strings(texts)
		t.Errorf("stored chunks %q do not match any single version", texts)
	}
}

func TestFindRelatedArticlesExcludesPages(t *testing.T) {
	db, _, s := searchFixture(t)
	ctx := context.Background()
	// 物理の記事をページに変える。記事の関連記事に出ず、ページからは関連記事を推薦しない。
	mustExec(t, db, `UPDATE articles SET kind = 'page' WHERE id = 'physics'`)
	insertArticle(t, db, "page-no-embedding", "研究室の紹介", "published")
	mustExec(t, db, `UPDATE articles SET kind = 'page' WHERE id = 'page-no-embedding'`)

	related, err := s.FindRelatedArticlesWithOptions(ctx, "quantum", RelatedOptions{Limit: 10,
		KeywordScores: map[string]float64{"physics": 9, "page-no-embedding": 9, "soccer": 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range related {
		if r.ArticleID == "physics" || r.ArticleID == "page-no-embedding" {
			t.Errorf("related of a post includes page %s: %v", r.ArticleID, articleIDs(related, relatedArticle))
		}
	}
	if len(related) == 0 {
		t.Error("related of a post is empty, want the other posts")
	}
	fromPage, err := s.FindRelatedArticles(ctx, "physics", 4)
	if err != nil || fromPage != nil {
		t.Errorf("FindRelatedArticles(page) = %v, %v, want nil", fromPage, err)
	}
	// RAG の検索（AI の参考資料・管理画面の検索）にはページも出る。
	if res, err := s.Search(ctx, "量子", 10, 0); err != nil {
		t.Fatal(err)
	} else if !slices.Contains(articleIDs(res, resultArticle), "physics") {
		t.Errorf("Search results %v do not include the page", articleIDs(res, resultArticle))
	}
}

// 予約中の記事（公開日時が未来の公開記事）は、公開日時を迎えるまで AI の検索にも関連記事にも出さない。
func TestSearchAndRelatedHideScheduledArticlesUntilPublishTime(t *testing.T) {
	db, _, s := searchFixture(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(24 * time.Hour)
	mustExec(t, db, `UPDATE articles SET published_at = ? WHERE id = 'physics'`, at)
	// 埋め込みが無く、全文検索だけで一致する予約中の記事。
	insertArticle(t, db, "keyword-only", "予約中の量子の記事", "published")
	mustExec(t, db, `UPDATE articles SET published_at = ? WHERE id = 'keyword-only'`, at)

	check := func(label string, want bool) {
		t.Helper()
		results, err := s.Search(ctx, "量子計算", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		related, err := s.FindRelatedArticlesWithOptions(ctx, "quantum", RelatedOptions{
			Limit: 10, KeywordScores: map[string]float64{"physics": 1, "keyword-only": 1, "soccer": 0.5}})
		if err != nil {
			t.Fatal(err)
		}
		inSearch := slices.Contains(articleIDs(results, resultArticle), "physics")
		ids := articleIDs(related, relatedArticle)
		inRelated, keywordOnly := slices.Contains(ids, "physics"), slices.Contains(ids, "keyword-only")
		if inSearch != want || inRelated != want || keywordOnly != want {
			t.Errorf("%s: physics in search %v, in related %v, keyword-only in related %v, want %v",
				label, inSearch, inRelated, keywordOnly, want)
		}
	}
	check("before publish time", false)
	s.now = func() time.Time { return at.Add(time.Second) }
	check("after publish time", true)
}

// 関連記事は元の記事や既に選んだ記事とアイキャッチが同じ記事、公開日が近くタイトルが酷似した双子記事を飛ばし、
// 次点を繰り上げて DefaultRelatedLimit 件（3件）に揃える。
func TestFindRelatedArticlesDiversifies(t *testing.T) {
	db := openTestDB(t, ":memory:")
	s := NewService(db, nil)
	now := time.Now().UTC()
	articles := []struct {
		id, title, image string
		age              time.Duration
	}{
		{"src", "ACS Fall 2026@Chicago（USA）で研究成果発表を行いました。", "/uploads/2026/08/acs-poster-1024x768.jpg", time.Hour},
		{"src-twin", "ACS Fall 2026＠Chicago", "", 2 * time.Hour},
		{"src-image", "ポスターを展示しました", "https://example.com/wp-content/uploads/2026/08/ACS-Poster.JPG?ver=2", 48 * time.Hour},
		{"jsac", "日本分析化学会第73年会（＠名古屋大学）で研究成果を発表しました", "/uploads/abc123-original.webp", 10 * 24 * time.Hour},
		{"jsac-twin", "分析化学会第73年会 in名古屋", "/uploads/nagoya.jpg", 12 * 24 * time.Hour},
		{"jsac-image", "研究室の集合写真", "/uploads/abc123-800.webp", 40 * 24 * time.Hour},
		{"fridge", "20年間ありがとう研究室の冷蔵庫", "", 50 * 24 * time.Hour},
		{"jsac-later", "日本分析化学会第73年会の振り返り", "", 60 * 24 * time.Hour},
		{"extra", "第85回分析化学討論会", "", 70 * 24 * time.Hour},
	}
	keyword := map[string]float64{}
	for i, a := range articles {
		insertArticle(t, db, a.id, a.title, "published")
		mustExec(t, db, `UPDATE articles SET featured_image_url = ?, published_at = ? WHERE id = ?`, a.image, now.Add(-a.age), a.id)
		keyword[a.id] = float64(100 - i)
	}

	related, err := s.FindRelatedArticlesWithOptions(context.Background(), "src", RelatedOptions{KeywordScores: keyword})
	if err != nil {
		t.Fatal(err)
	}
	// src-twin（元記事の双子）・src-image（元記事と同じ写真）・jsac-twin（jsac の双子）・jsac-image（jsac と同じ写真の縮小版）を飛ばす。
	// jsac-later はタイトルが似ていても公開日が7日より離れているので別の記事として残す。
	if ids := articleIDs(related, relatedArticle); fmt.Sprint(ids) != "[jsac fridge jsac-later]" {
		t.Errorf("related = %v, want [jsac fridge jsac-later]", ids)
	}

	// 件数を増やしても重複は入らず、次点の候補で埋める。
	more, err := s.FindRelatedArticlesWithOptions(context.Background(), "src", RelatedOptions{Limit: 10, KeywordScores: keyword})
	if err != nil {
		t.Fatal(err)
	}
	if ids := articleIDs(more, relatedArticle); fmt.Sprint(ids) != "[jsac fridge jsac-later extra]" {
		t.Errorf("related limit 10 = %v, want [jsac fridge jsac-later extra]", ids)
	}
}

func TestDefaultRelatedLimitFillsOneRow(t *testing.T) {
	if DefaultRelatedLimit != 3 {
		t.Errorf("DefaultRelatedLimit = %d, want 3 (one row of the 3-column related grid)", DefaultRelatedLimit)
	}
	if relatedPoolSize(DefaultRelatedLimit) < 8 {
		t.Errorf("relatedPoolSize(%d) = %d, want at least 8 candidates before diversification", DefaultRelatedLimit, relatedPoolSize(DefaultRelatedLimit))
	}
}

func TestSimilarTitles(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"ACS Fall 2026@Chicago", "ACS Fall 2026@Chicago（USA）で研究成果発表を行いました。", true},
		{"日本分析化学会第73年会（＠名古屋大学）で研究成果を発表しました", "分析化学会第73年会 in名古屋", true},
		{"サンプル研究室ホームページを開設しました", "サンプル研究室ホームページを開設しました", true},
		{"第85回分析化学討論会（愛媛大学）で発表しました", "第85回分析化学討論会（愛媛大学）に参加", true},
		// 定型句（〜で研究成果を発表しました）や短い共通部分だけでは双子とみなさない。
		{"日本化学会春季年会で研究成果を発表しました", "電気化学会で研究成果を発表しました", false},
		{"2026年度卒業式", "2026年度入学式", false},
		{"サンプル研究室の新メンバー紹介", "サンプル研究室の大掃除", false},
		{"量子コンピュータの基礎", "量子コンピュータ研究会の発表", false},
		{"Sample Laboratory Open Campus Report", "Sample Laboratory Open Campus Photos", true},
		{"", "ACS Fall 2026", false},
	} {
		if got := similarTitles(normalizeRelatedTitle(tc.a), normalizeRelatedTitle(tc.b)); got != tc.want {
			t.Errorf("similarTitles(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRelatedImageKey(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"/uploads/abc-original.webp", "/uploads/abc-400.webp"},
		{"/uploads/2017/05/stage.jpg", "https://example.com/wp-content/uploads/2017/05/Stage-1024x768.JPG?v=2"},
		{"/uploads/Lab%20Photo-scaled.jpg", "/uploads/lab photo-300x200.jpg"},
	} {
		if ka, kb := relatedImageKey(tc.a), relatedImageKey(tc.b); ka == "" || ka != kb {
			t.Errorf("relatedImageKey(%q) = %q, relatedImageKey(%q) = %q, want the same key", tc.a, ka, tc.b, kb)
		}
	}
	for _, tc := range []struct{ a, b string }{
		{"/uploads/photo-1.jpg", "/uploads/photo-2.jpg"},
		{"/uploads/2024-05-01.jpg", "/uploads/2024-05-02.jpg"},
	} {
		if relatedImageKey(tc.a) == relatedImageKey(tc.b) {
			t.Errorf("relatedImageKey treats %q and %q as the same image", tc.a, tc.b)
		}
	}
	if relatedImageKey("  ") != "" || relatedImageKey("") != "" {
		t.Error("relatedImageKey of an empty URL is not empty")
	}
	if dup := duplicateRelated(relatedMeta{}, relatedMeta{}); dup {
		t.Error("articles without eyecatch or date are treated as duplicates")
	}
}
