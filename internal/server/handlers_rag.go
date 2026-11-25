package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/rag"
	"sorus/internal/search"
)

const (
	// ragIndexTimeout は記事1件の埋め込みを更新する1回の試行の上限。Embeddings API は1回30秒で打ち切るが、
	// チャンクが多いと複数回に分けて呼ぶため余裕を持たせる。
	ragIndexTimeout = 2 * time.Minute
	// ragIndexAttempts は埋め込みの更新を試みる最大回数（初回を含む）。
	ragIndexAttempts = 3
	// defaultRAGRetryDelay は埋め込みの更新に失敗してから再試行するまでの待ち時間。試行ごとに倍にする。
	defaultRAGRetryDelay = 2 * time.Second

	// relatedCacheTTL は関連記事の推薦結果を使い回す時間。予約投稿の自動公開など、reindexArticle を通らない
	// 記事の変更もこの時間が経てば推薦に反映される。
	relatedCacheTTL = 10 * time.Minute
	// relatedCacheMaxEntries は関連記事のキャッシュに保持する記事数の上限。超えたら全体を捨てて作り直す。
	relatedCacheMaxEntries = 4096

	// maxRelatedKeywords は関連記事の全文検索に使う語の最大数（1語につき1回検索する）。
	maxRelatedKeywords = 8
	// maxKeywordRunes は漢字の連なりを1語のまま使う最大の長さ（文字数）。これより長い連なり（複合語）は
	// 2文字ずつに区切り、構成する語の一部だけを含む記事にも一致させる。
	maxKeywordRunes = 4
)

// handleRAGSearch は公開済み記事の類似チャンクを管理画面向けに検索する。
func (s *Server) handleRAGSearch(c echo.Context) error {
	if s.ragService == nil || !s.ragService.Enabled() {
		return c.JSON(http.StatusOK, map[string]any{"configured": false, "results": []rag.SearchResult{}})
	}
	query := strings.TrimSpace(c.QueryParam("q"))
	if query == "" {
		query = strings.TrimSpace(c.QueryParam("text"))
	}
	if query == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "検索語を入力してください")
	}
	limit := rag.DefaultSearchLimit
	if value := c.QueryParam("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > rag.MaxSearchLimit {
			return echo.NewHTTPError(http.StatusBadRequest, "検索件数が正しくありません")
		}
		limit = n
	}
	results, err := s.ragService.Search(c.Request().Context(), query, limit, 0)
	if err != nil {
		c.Logger().Warnf("rag: search: %v", err)
		return echo.NewHTTPError(http.StatusBadGateway, "類似記事を検索できませんでした")
	}
	return c.JSON(http.StatusOK, map[string]any{"configured": true, "results": results})
}

// ragIndexer は記事の埋め込みの更新を、保存リクエストから切り離してバックグラウンドで行う。
//
// 同じ記事の更新は1つずつ順に行い、実行中にまた保存されたら、終了後に最新の本文で1回だけやり直す。
// 保存が続いても API 呼び出しを積み上げず、古い本文の索引が新しい本文の索引を上書きしない。
type ragIndexer struct {
	ctx    context.Context
	cancel context.CancelFunc
	// retryDelay は1回目の再試行までの待ち時間。
	retryDelay time.Duration

	mu sync.Mutex
	// pending は更新中の記事 ID と、更新中にもう一度の更新を求められたか。
	pending map[string]bool
	closed  bool
	wg      sync.WaitGroup
}

func newRAGIndexer() *ragIndexer {
	ctx, cancel := context.WithCancel(context.Background())
	return &ragIndexer{ctx: ctx, cancel: cancel, retryDelay: defaultRAGRetryDelay, pending: map[string]bool{}}
}

// scheduleRAGIndex は記事 id の埋め込みを今の状態に合わせる処理（indexRAGArticle）をバックグラウンドで始め、すぐに返る。
// 失敗しても記事の保存は失敗させず、再試行してもだめならログに残す。RAG を使わない構成と、AI 機能を OFF にした
// サイト（aiEnabled）では何もしない（外部の Embeddings API に本文を送らず、保存済みの埋め込みも消さない）。
func (s *Server) scheduleRAGIndex(id string) {
	if s.ragService == nil || !s.aiEnabled() {
		return
	}
	q := s.ragIndexer
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if _, running := q.pending[id]; running {
		q.pending[id] = true
		return
	}
	q.pending[id] = false
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for {
			s.runRAGIndex(id)
			q.mu.Lock()
			again := q.pending[id] && !q.closed
			if !again {
				delete(q.pending, id)
				q.mu.Unlock()
				return
			}
			q.pending[id] = false
			q.mu.Unlock()
		}
	}()
}

// runRAGIndex は記事 id の埋め込みを更新し、失敗すれば待ち時間を倍にしながら ragIndexAttempts 回まで試す。
// 更新できたら関連記事のキャッシュを捨てる。
func (s *Server) runRAGIndex(id string) {
	q := s.ragIndexer
	logger := s.echo.Logger
	delay := q.retryDelay
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(q.ctx, ragIndexTimeout)
		err := s.indexRAGArticle(ctx, id)
		cancel()
		if err == nil {
			s.related.invalidate()
			return
		}
		if q.ctx.Err() != nil {
			logger.Warnf("rag: index article %s canceled by shutdown: %v", id, err)
			return
		}
		if attempt == ragIndexAttempts || errors.Is(err, rag.ErrNotConfigured) || errors.Is(err, rag.ErrArticleNotFound) {
			logger.Errorf("rag: index article %s failed after %d attempt(s): %v", id, attempt, err)
			return
		}
		logger.Warnf("rag: index article %s failed (will retry): %v", id, err)
		select {
		case <-q.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// indexRAGArticle は記事 id の埋め込みを今の状態に合わせる。埋め込みを作り、本文を外部の Embeddings API に送るのは
// 読者に見えている（公開で、公開日時を迎えた）記事だけ。下書き・校閲待ち・予約中・ゴミ箱の記事と、無くなった記事の
// 埋め込みは消す。予約投稿は公開日時を迎えたとき（onScheduledPublished）に作る。Embeddings API を使えない構成では、
// 公開中の記事の保存済みの埋め込みはそのまま残す（関連記事に使う）。
//
// 待っているあいだや再試行のあいだに AI 機能を OFF にしたときも、外部へ送らず保存済みの埋め込みも消さずに終える。
func (s *Server) indexRAGArticle(ctx context.Context, id string) error {
	if !s.aiEnabled() {
		return nil
	}
	a, err := s.articles.GetByID(id)
	if err != nil && !errors.Is(err, article.ErrNotFound) {
		return err
	}
	if !article.IsVisible(a, clockNow()) {
		return s.ragService.DeleteArticle(ctx, id)
	}
	if !s.ragService.Enabled() {
		return nil
	}
	return s.ragService.IndexArticle(ctx, a.ID, a.ContentMarkdown)
}

// waitRAGIndex はバックグラウンドの埋め込みの更新がすべて終わるまで待つ（テスト用）。
func (s *Server) waitRAGIndex() { s.ragIndexer.wg.Wait() }

// stopRAGIndex は新しい埋め込みの更新を受け付けないようにし、実行中の更新を打ち切って終了を待つ。
// 打ち切った記事は次の保存で作り直す。ctx が先に終われば ctx.Err() を返す。
func (s *Server) stopRAGIndex(ctx context.Context) error {
	q := s.ragIndexer
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cancel()
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// relatedCache は公開記事ごとの関連記事の推薦結果をメモリに保持し、記事表示のたびに全記事の埋め込みを
// 走査しないようにする。結果は relatedCacheTTL で失効し、記事の保存・公開・埋め込みの更新で全体を捨てる
// （ある記事の変更は他の記事の推薦にも影響するため）。
type relatedCache struct {
	ttl time.Duration

	mu sync.Mutex
	// gen は invalidate のたびに増える世代。計算中に捨てられた古い結果を put で保存しないために使う。
	gen     uint64
	entries map[string]relatedEntry
}

type relatedEntry struct {
	related []rag.RelatedArticle
	expires time.Time
}

func newRelatedCache(ttl time.Duration) *relatedCache {
	return &relatedCache{ttl: ttl, entries: map[string]relatedEntry{}}
}

// get は articleID の推薦結果を返す。無ければ ok = false と、計算した結果を put に渡すときの世代を返す。
func (rc *relatedCache) get(articleID string) (related []rag.RelatedArticle, gen uint64, ok bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, ok := rc.entries[articleID]
	if ok && time.Now().Before(e.expires) {
		return e.related, rc.gen, true
	}
	return nil, rc.gen, false
}

// put は gen の世代で計算した推薦結果を保存する。計算中に invalidate されていれば保存しない。
func (rc *relatedCache) put(articleID string, gen uint64, related []rag.RelatedArticle) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if gen != rc.gen {
		return
	}
	if len(rc.entries) >= relatedCacheMaxEntries {
		rc.entries = map[string]relatedEntry{}
	}
	rc.entries[articleID] = relatedEntry{related: related, expires: time.Now().Add(rc.ttl)}
}

// invalidate はすべての推薦結果を捨てる。
func (rc *relatedCache) invalidate() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.gen++
	clear(rc.entries)
}

// findRelated は記事 a の関連記事を、埋め込みのコサイン類似度と全文検索のスコアのハイブリッドで求める。
// 結果は relatedCache に保存し、次からは計算し直さない。
func (s *Server) findRelated(ctx context.Context, a publicArticle) ([]rag.RelatedArticle, error) {
	related, gen, ok := s.related.get(a.ID)
	if ok {
		return related, nil
	}
	related, err := s.ragService.FindRelatedArticlesWithOptions(ctx, a.ID, rag.RelatedOptions{
		Limit:         rag.DefaultRelatedLimit,
		KeywordScores: s.keywordScores(ctx, a),
	})
	if err != nil {
		return nil, err
	}
	s.related.put(a.ID, gen, related)
	return related, nil
}

// keywordScores は記事 a のタイトル・抜粋から取り出した語でそれぞれ全文検索し、記事 ID ごとに Bleve のスコアを
// 合計して返す（語の OR 検索に相当する）。a 自身も含むが、FindRelatedArticlesWithOptions が除く。
// 検索インデックスが無い、または検索に失敗した語は使わない。公開サイトの検索と同じく、執筆者名を表示しない設定では
// 著者名で一致させない（setShowAuthor で切り替えると推薦結果のキャッシュを捨てる）。
func (s *Server) keywordScores(ctx context.Context, a publicArticle) map[string]float64 {
	if s.search == nil {
		return nil
	}
	opts := search.Options{HideAuthor: !s.getShowAuthor()}
	scores := map[string]float64{}
	for _, kw := range relatedKeywords(a.Title, a.Excerpt) {
		res, err := s.search.SearchWithOptions(ctx, kw, 1, search.MaxPageSize, opts)
		if err != nil {
			s.echo.Logger.Warnf("related: keyword search %q: %v", kw, err)
			continue
		}
		for _, h := range res.Hits {
			scores[h.ID] += h.Score
		}
	}
	return scores
}

// keywordClass は relatedKeywords が語の区切りを判定する文字の種類。
type keywordClass int

const (
	classOther keywordClass = iota // ひらがな・記号・空白など（語の区切り）
	classHan
	classKatakana
	classAlnum
)

func classify(r rune) keywordClass {
	switch {
	case unicode.Is(unicode.Han, r):
		return classHan
	case unicode.Is(unicode.Katakana, r) || r == 'ー':
		return classKatakana
	case unicode.Is(unicode.Hiragana, r):
		return classOther
	case unicode.IsLetter(r) || unicode.IsDigit(r):
		return classAlnum
	}
	return classOther
}

// relatedKeywords は texts から関連記事の全文検索に使う語を、出現順に最大 maxRelatedKeywords 語返す。
//
// 辞書を使わずに内容語を取り出すため、漢字・カタカナ・英数字の連なりを1語とし、ひらがな（助詞など）や記号で区切る。
// 全角英数字は半角に畳み、英字は小文字にそろえる。2文字未満の語と数字だけの語（年号など）は除く。
// maxKeywordRunes 文字を超える漢字の連なりは2文字ずつに区切る。
func relatedKeywords(texts ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(word []rune) {
		if len(word) < 2 || len(out) == maxRelatedKeywords {
			return
		}
		w := strings.ToLower(string(word))
		if seen[w] || strings.IndexFunc(w, func(r rune) bool { return !unicode.IsDigit(r) }) < 0 {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, text := range texts {
		var word []rune
		var class keywordClass
		flush := func() {
			if class == classHan && len(word) > maxKeywordRunes {
				for len(word) >= 2 {
					add(word[:2])
					word = word[2:]
				}
			}
			add(word)
			word = word[:0]
		}
		for len(text) > 0 {
			r, size := utf8.DecodeRuneInString(text)
			text = text[size:]
			if r >= 0xFF01 && r <= 0xFF5E {
				r -= 0xFEE0 // 全角英数記号を半角に畳む。
			}
			c := classify(r)
			if c != class {
				flush()
				class = c
			}
			if c != classOther {
				word = append(word, r)
			}
		}
		flush()
	}
	return out
}
