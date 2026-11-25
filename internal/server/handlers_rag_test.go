package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/rag"
)

func TestPublicArticleShowsRelatedRAGCards(t *testing.T) {
	env := newTestEnv(t)
	env.server.ragService = rag.NewService(env.db, nil)
	targetID := seedArticle(t, env.db, article.Article{Title: "研究の対象", Slug: "rag-target", ContentMarkdown: "対象の本文。",
		Status: article.StatusPublished, PublishedAt: ago(2 * time.Hour)})
	relatedID := seedArticle(t, env.db, article.Article{Title: "関連する研究記事", Slug: "rag-related", Excerpt: "研究の紹介です。",
		ContentMarkdown: "近いテーマの本文。", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	for i, id := range []string{targetID, relatedID} {
		if _, err := env.db.Exec(`INSERT INTO article_embeddings
			(id, article_id, chunk_index, chunk_text, embedding, dimensions, model)
			VALUES (?, ?, 0, ?, ?, 2, 'test-model')`,
			"rag-embedding-"+id, id, "研究テーマ", rag.EncodeVector([]float32{1, 0})); err != nil {
			t.Fatalf("insert embedding %d: %v", i, err)
		}
	}
	c := env.newClient()
	res := c.get("/articles/rag-target")
	expectStatus(t, res, 200)
	expectContains(t, res.Body, "関連記事", "関連する研究記事", "研究の紹介です。")
	if targetID == relatedID {
		t.Fatal("target and related article IDs must differ")
	}
}

// setEmbedding は記事 id の埋め込みを1チャンクの v に置き換える。
func setEmbedding(t *testing.T, db *sqlx.DB, id string, v []float32) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM article_embeddings WHERE article_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO article_embeddings (id, article_id, chunk_index, chunk_text, embedding, dimensions, model)
		VALUES (?, ?, 0, '本文', ?, ?, 'test-model')`, "emb-"+id, id, rag.EncodeVector(v), len(v)); err != nil {
		t.Fatal(err)
	}
}

// relatedTitles は公開記事ページの関連記事欄に並ぶ記事のタイトルを、表示順に返す。
func relatedTitles(t *testing.T, body string) []string {
	t.Helper()
	_, section, ok := strings.Cut(body, `class="related-articles"`)
	if !ok {
		return nil
	}
	section, _, _ = strings.Cut(section, "</section>")
	var titles []string
	for {
		var title string
		if _, section, ok = strings.Cut(section, "<h3>"); !ok {
			return titles
		}
		title, section, _ = strings.Cut(section, "</h3>")
		titles = append(titles, title)
	}
}

// echoContext は reindexArticle などに渡す、リクエストを持たない echo.Context を返す。
func (e *testEnv) echoContext() echo.Context {
	return e.server.echo.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
}

func TestPublicRelatedArticlesAreHybridAndCached(t *testing.T) {
	var targetID, semanticID, keywordID string
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		// 起動時の検索インデックスの同期に含めるため、サーバーの起動前に保存する。
		cat := seedCategory(t, db, "研究", "research", "")
		targetID = seedArticle(t, db, article.Article{Title: "量子コンピュータ研究会の発表", Slug: "hybrid-target", CategoryID: cat,
			ContentMarkdown: "研究会で発表しました。", Status: article.StatusPublished, PublishedAt: ago(3 * time.Hour)})
		semanticID = seedArticle(t, db, article.Article{Title: "サッカー部の試合結果", Slug: "hybrid-semantic", CategoryID: cat,
			ContentMarkdown: "試合に勝ちました。", Status: article.StatusPublished, PublishedAt: ago(2 * time.Hour)})
		keywordID = seedArticle(t, db, article.Article{Title: "量子コンピュータの基礎", Slug: "hybrid-keyword", CategoryID: cat,
			ContentMarkdown: "量子コンピュータの仕組みを解説します。", Status: article.StatusPublished, PublishedAt: ago(time.Hour)})
	})
	env.server.ragService = rag.NewService(env.db, nil)
	setEmbedding(t, env.db, targetID, []float32{1, 0})
	setEmbedding(t, env.db, semanticID, []float32{0.6, 0.8}) // コサイン類似度 0.6、語の重なりなし
	setEmbedding(t, env.db, keywordID, []float32{0, 1})      // コサイン類似度 0、タイトルの語が重なる

	// 0.6 × 0 + 0.4 × 1（全文検索の1位）= 0.4 が、0.6 × 0.6 + 0.4 × 0 = 0.36 を上回る。
	// コサイン類似度だけなら、語が重なる記事は候補にも入らない。
	c := env.newClient()
	res := c.get("/articles/hybrid-target")
	expectStatus(t, res, http.StatusOK)
	if got := relatedTitles(t, res.Body); fmt.Sprint(got) != "[量子コンピュータの基礎 サッカー部の試合結果]" {
		t.Fatalf("related = %q, want the keyword match first", got)
	}

	// 推薦はキャッシュし、記事を表示するたびに全記事の埋め込みを読み直さない。
	setEmbedding(t, env.db, semanticID, []float32{1, 0})
	if got := relatedTitles(t, c.get("/articles/hybrid-target").Body); fmt.Sprint(got) != "[量子コンピュータの基礎 サッカー部の試合結果]" {
		t.Errorf("related after changing embeddings = %q, want the cached order", got)
	}
	// 記事の保存・公開（reindexArticle）でキャッシュを捨て、新しい埋め込みで推薦し直す。
	env.server.reindexArticle(env.echoContext(), semanticID)
	if got := relatedTitles(t, c.get("/articles/hybrid-target").Body); fmt.Sprint(got) != "[サッカー部の試合結果 量子コンピュータの基礎]" {
		t.Errorf("related after reindex = %q, want the semantic match first", got)
	}
}

func TestRelatedCache(t *testing.T) {
	rc := newRelatedCache(time.Hour)
	if _, _, ok := rc.get("a"); ok {
		t.Fatal("empty cache returned an entry")
	}
	_, gen, _ := rc.get("a")
	rc.put("a", gen, []rag.RelatedArticle{{ArticleID: "b"}})
	if got, _, ok := rc.get("a"); !ok || len(got) != 1 || got[0].ArticleID != "b" {
		t.Errorf("get(a) = %v, %v", got, ok)
	}
	// 関連記事が無いという結果もキャッシュする。
	_, gen, _ = rc.get("lonely")
	rc.put("lonely", gen, nil)
	if _, _, ok := rc.get("lonely"); !ok {
		t.Error("empty result is not cached")
	}

	// 計算中に無効化されたら、古いデータで計算した結果は保存しない。
	_, gen, _ = rc.get("c")
	rc.invalidate()
	rc.put("c", gen, []rag.RelatedArticle{{ArticleID: "stale"}})
	for _, id := range []string{"a", "lonely", "c"} {
		if got, _, ok := rc.get(id); ok {
			t.Errorf("get(%s) after invalidate = %v", id, got)
		}
	}

	expired := newRelatedCache(-time.Second)
	_, gen, _ = expired.get("a")
	expired.put("a", gen, []rag.RelatedArticle{{ArticleID: "b"}})
	if _, _, ok := expired.get("a"); ok {
		t.Error("expired entry is returned")
	}
}

func TestRelatedKeywords(t *testing.T) {
	for _, tc := range []struct {
		texts []string
		want  string
	}{
		{[]string{"量子計算サークルの歩み"}, "[量子計算 サークル]"},
		// 数字だけの語と1文字の語は除き、4文字を超える漢字の連なりは2文字ずつに区切る。
		{[]string{"2025年度 AI 研究会：量子情報処理研究"}, "[年度 ai 研究会 量子 情報 処理 研究]"},
		// 英数字は半角・小文字にそろえて重複を除く。
		{[]string{"ＡＩと私", "ai の話と AI"}, "[ai]"},
		{[]string{"は、を。"}, "[]"},
	} {
		if got := fmt.Sprint(relatedKeywords(tc.texts...)); got != tc.want {
			t.Errorf("relatedKeywords(%q) = %s, want %s", tc.texts, got, tc.want)
		}
	}
	many := strings.Repeat("語彙 ", 3) + "一番 二番 三番 四番 五番 六番 七番 八番 九番 十番"
	if n := len(relatedKeywords(many)); n != maxRelatedKeywords {
		t.Errorf("relatedKeywords returned %d words, want %d", n, maxRelatedKeywords)
	}
}

func TestReindexArticleWithRAGOnly(t *testing.T) {
	// 検索インデックスが無く RAG だけを設定した構成でも、記事が見つからない経路で panic しない。
	env := newTestEnv(t)
	idx := env.server.search
	env.server.search = nil
	t.Cleanup(func() { env.server.search = idx }) // Shutdown の前に戻す（New が作ったインデックスを閉じる）。
	env.server.ragService = rag.NewService(env.db, nil)
	id := seedArticle(t, env.db, article.Article{Title: "RAG だけの構成", Slug: "rag-only", ContentMarkdown: "本文。",
		Status: article.StatusPublished, PublishedAt: ago(time.Hour)})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reindexArticle panicked: %v", r)
		}
	}()
	env.server.reindexArticle(env.echoContext(), "missing-article")
	env.server.reindexArticle(env.echoContext(), id)
	env.server.waitRAGIndex()
}

// fakeEmbedder は release が閉じられるまで Embed を止め、先頭の failures 回は失敗する Embedder。呼び出しを記録する。
type fakeEmbedder struct {
	release  chan struct{} // nil なら止めない
	started  chan struct{} // Embed が呼ばれるたびに（空きがあれば）送る
	failures int

	mu    sync.Mutex
	calls [][]string
}

func (e *fakeEmbedder) Name() string  { return "Fake" }
func (e *fakeEmbedder) Model() string { return "test-model" }

func (e *fakeEmbedder) Embed(ctx context.Context, texts []string, _ rag.TaskType) (*rag.Embeddings, error) {
	e.mu.Lock()
	e.calls = append(e.calls, texts)
	fail := len(e.calls) <= e.failures
	e.mu.Unlock()
	select {
	case e.started <- struct{}{}:
	default:
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return nil, errors.New("embeddings API is unavailable")
	}
	res := &rag.Embeddings{Model: e.Model()}
	for range texts {
		res.Vectors = append(res.Vectors, []float32{1, 0})
	}
	return res, nil
}

func (e *fakeEmbedder) recorded() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.calls...)
}

func TestArticleSaveDoesNotWaitForRAGIndexing(t *testing.T) {
	env := newTestEnv(t)
	emb := &fakeEmbedder{release: make(chan struct{}), started: make(chan struct{}, 10)}
	env.server.ragService = rag.NewService(env.db, emb)
	// 埋め込みを作るのは公開中の記事だけなので、公開できる編集者で保存する。
	ed := env.loggedIn(chief)
	save := func(id, action, body string) *response {
		form := url.Values{"title": {"非同期の記事"}, "category_id": {"1"}, "content_markdown": {body}, "action": {action}}
		return ed.postForm("/admin/articles/"+id, form)
	}

	// Embeddings API が応答しない間も、保存はすぐに完了する。
	quick := func(name string, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			fn()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			close(emb.release)
			<-done
			t.Fatalf("%s waited for the embeddings API", name)
		}
	}
	var id string
	quick("create", func() { id = ed.createArticle("非同期の記事", "下書きの本文。") })
	quick("publish", func() { expectRedirect(t, save(id, "publish", "最初の本文。"), "/admin/articles/"+id+"/edit") })
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("RAG indexing did not start")
	}
	// 索引の更新中に2回保存しても、終わってから最新の本文で1回だけ更新し直す。
	for _, body := range []string{"二回目の本文。", "最新の本文。"} {
		quick("save", func() { expectRedirect(t, save(id, "save", body), "/admin/articles/"+id+"/edit") })
	}
	close(emb.release)
	env.server.waitRAGIndex()

	calls := emb.recorded()
	if len(calls) != 2 || !strings.Contains(calls[0][0], "最初の本文") || !strings.Contains(calls[1][0], "最新の本文") {
		t.Errorf("embedded texts = %q, want the first body and then only the latest one", calls)
	}
	var stored []string
	if err := env.db.Select(&stored, `SELECT chunk_text FROM article_embeddings WHERE article_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stored) != "[最新の本文。]" {
		t.Errorf("stored chunks = %q, want the latest body", stored)
	}
}

func TestRAGIndexingRetriesInBackground(t *testing.T) {
	env := newTestEnv(t)
	emb := &fakeEmbedder{failures: 2, started: make(chan struct{}, 10)}
	env.server.ragService = rag.NewService(env.db, emb)
	env.server.ragIndexer.retryDelay = time.Millisecond
	// 埋め込みを作るのは公開中の記事だけなので、公開できる編集者で保存する。
	ed := env.loggedIn(chief)
	save := func(id, action, body string) {
		t.Helper()
		form := url.Values{"title": {"再試行の記事"}, "category_id": {"1"}, "content_markdown": {body}, "action": {action}}
		expectRedirect(t, ed.postForm("/admin/articles/"+id, form), "/admin/articles/"+id+"/edit")
	}

	id := ed.createArticle("再試行の記事", "下書きの本文。")
	save(id, "publish", "再試行の本文。")
	env.server.waitRAGIndex()
	if n := len(emb.recorded()); n != ragIndexAttempts {
		t.Errorf("Embed calls = %d, want %d", n, ragIndexAttempts)
	}
	if n := env.countRows(`SELECT COUNT(*) FROM article_embeddings WHERE article_id = ?`, id); n != 1 {
		t.Errorf("stored %d chunks after retrying, want 1", n)
	}

	// 試行回数を使い切ったら諦め、記事の保存には影響しない。
	emb.mu.Lock()
	emb.failures = len(emb.calls) + ragIndexAttempts
	emb.mu.Unlock()
	save(id, "save", "書き換えた本文。")
	env.server.waitRAGIndex()
	if n := len(emb.recorded()); n != 2*ragIndexAttempts {
		t.Errorf("Embed calls = %d, want %d", n, 2*ragIndexAttempts)
	}
	if got := env.mustGetArticle(id).ContentMarkdown; got != "書き換えた本文。" {
		t.Errorf("saved body = %q", got)
	}
}
