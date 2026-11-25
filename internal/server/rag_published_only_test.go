package server

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"sorus/internal/rag"
)

// useFakeEmbedder は RAG の埋め込み API を、呼び出しを記録する偽物（fakeEmbedder）にする。
func (e *testEnv) useFakeEmbedder() *fakeEmbedder {
	emb := &fakeEmbedder{}
	e.server.ragService = rag.NewService(e.db, emb)
	return emb
}

func (e *testEnv) embeddingChunks(id string) int {
	e.t.Helper()
	return e.countRows(`SELECT COUNT(*) FROM article_embeddings WHERE article_id = ?`, id)
}

// AI 検索（RAG）の埋め込みは公開中の記事だけに作る。下書き・校閲待ちの本文は外部の埋め込み API に送らず、
// 公開したときに作り、下書きに戻す・ゴミ箱に移すと消す。
func TestRAGEmbedsOnlyPublishedArticles(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir()}, nil)
	emb := env.useFakeEmbedder()
	ed := env.loggedIn(chief)
	expectCalls := func(step string, want int) {
		t.Helper()
		env.server.waitRAGIndex()
		if calls := emb.recorded(); len(calls) != want {
			t.Fatalf("%s: embedded texts = %q, want %d call(s)", step, calls, want)
		}
	}

	id := ed.createArticle("RAGの記事", "下書きの本文。")
	edit := "/admin/articles/" + id
	save := func(action, body string) {
		t.Helper()
		form := url.Values{"title": {"RAGの記事"}, "category_id": {"1"}, "content_markdown": {body}, "action": {action}}
		expectRedirect(t, ed.postForm(edit, form), edit+"/edit")
	}
	save("review", "校閲待ちの本文。")
	expectCalls("draft and review", 0)
	if n := env.embeddingChunks(id); n != 0 {
		t.Fatalf("stored %d chunk(s) for an unpublished article", n)
	}

	save("publish", "公開した本文。")
	expectCalls("publish", 1)
	if got := fmt.Sprint(emb.recorded()[0]); !strings.Contains(got, "公開した本文") {
		t.Errorf("embedded text = %q, want the published body", got)
	}
	if n := env.embeddingChunks(id); n != 1 {
		t.Fatalf("stored %d chunk(s) after publishing, want 1", n)
	}

	save("draft", "下書きに戻した本文。")
	expectCalls("back to draft", 1)
	if n := env.embeddingChunks(id); n != 0 {
		t.Fatalf("stored %d chunk(s) after going back to draft, want 0", n)
	}

	save("publish", "もう一度公開した本文。")
	expectCalls("publish again", 2)
	expectRedirect(t, ed.postForm(edit+"/delete", nil), "/admin/articles")
	expectCalls("delete", 2)
	if n := env.embeddingChunks(id); n != 0 {
		t.Fatalf("stored %d chunk(s) after moving to the trash, want 0", n)
	}
}

// 予約投稿は、予約した日時が来て公開されたときに埋め込みを作る。それまでは本文を送らない。
func TestRAGEmbedsScheduledArticleWhenPublished(t *testing.T) {
	env := newTestEnvWith(t, Config{ThemesDir: t.TempDir(), SchedulerInterval: 20 * time.Millisecond}, nil)
	emb := env.useFakeEmbedder()
	ed := env.loggedIn(chief)
	id := ed.createArticle("予約のRAG", "初稿")
	edit := "/admin/articles/" + id
	expectRedirect(t, ed.postForm(edit, scheduleForm("予約のRAG", "予約した本文。", futureLocal(2*time.Hour))), edit+"/edit")
	env.server.waitRAGIndex()
	if calls := emb.recorded(); len(calls) != 0 {
		t.Fatalf("embedded texts before the publish time = %q, want none", calls)
	}

	env.arriveScheduled(id)
	deadline := time.Now().Add(5 * time.Second)
	for env.embeddingChunks(id) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no embeddings were made after the scheduled article was published")
		}
		time.Sleep(20 * time.Millisecond)
	}
	env.server.waitRAGIndex()
	if got := fmt.Sprint(emb.recorded()); !strings.Contains(got, "予約した本文") {
		t.Errorf("embedded texts = %q, want the scheduled body", got)
	}
}
