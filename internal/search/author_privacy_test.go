package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sorus/internal/article"
)

// searchHidden は執筆者名を表示しない公開サイトと同じ条件（HideAuthor）で検索する。
func searchHidden(t *testing.T, x *Index, q string, page, pageSize int) *SearchResult {
	t.Helper()
	res, err := x.SearchWithOptions(context.Background(), q, page, pageSize, Options{HideAuthor: true})
	if err != nil {
		t.Fatalf("SearchWithOptions(%q, HideAuthor): %v", q, err)
	}
	return res
}

func assertHiddenIDs(t *testing.T, x *Index, q string, want ...string) {
	t.Helper()
	res := searchHidden(t, x, q, 1, 50)
	got := hitIDs(res)
	wantSet := make(map[string]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	ok := len(got) == len(want) && res.Total == uint64(len(want))
	for _, id := range got {
		ok = ok && wantSet[id]
	}
	if !ok {
		t.Errorf("SearchWithOptions(%q, HideAuthor) = %v (Total %d), want %v (any order)", q, got, res.Total, want)
	}
}

// scholarly は論文の書誌情報（DOI）を持つ公開記事を返す。公開ページは著者を書誌情報として出す。
func scholarly(id, title, html string) *article.Article {
	a := published(id, title, html)
	a.DOI = "10.1234/" + id
	return a
}

// 執筆者名を表示しない公開サイトの検索では、通常の記事を著者名だけで一致させない。
// タイトル・本文・カテゴリの一致や、著者を公開している論文記事の著者名での一致は変わらない。
func TestSearchHideAuthorIgnoresAuthorOfNormalArticles(t *testing.T) {
	x := newTestIndex(t)
	// mustIndex は著者「山田太郎」、カテゴリ「サークル紹介」で登録する。
	mustIndex(t, x, published("normal", "新歓合宿のお知らせ", "<p>長野で合宿をします。</p>"))
	mustIndex(t, x, published("town", "山田町の夏祭り", "<p>屋台が並んだ。</p>"))
	mustIndex(t, x, scholarly("paper", "量子計算の研究", "<p>量子ビットの実験。</p>"))
	page := published("page", "アクセス", "<p>駅から徒歩5分。</p>")
	page.Kind = article.KindPage
	mustIndex(t, x, page)

	// 著者名を照合に使う既定の検索（管理画面・MCP など）は従来どおり。
	assertIDs(t, x, "山田", "normal", "town", "paper")
	assertIDs(t, x, "合宿 山田", "normal")

	assertHiddenIDs(t, x, "山田", "town", "paper")
	assertHiddenIDs(t, x, "山田太郎", "paper")
	assertHiddenIDs(t, x, "合宿 山田")
	// タイトル・本文・カテゴリでは一致する。
	assertHiddenIDs(t, x, "合宿", "normal")
	assertHiddenIDs(t, x, "屋台", "town")
	assertHiddenIDs(t, x, "紹介", "normal", "town", "paper")
	assertHiddenIDs(t, x, "量子", "paper")

	// 言語の絞り込みと組み合わせても著者名で一致させない。
	res, err := x.SearchWithOptions(context.Background(), "山田", 1, 50, Options{Lang: "ja", HideAuthor: true})
	if err != nil {
		t.Fatalf("SearchWithOptions(ja): %v", err)
	}
	for _, h := range res.Hits {
		if h.ID == "normal" {
			t.Errorf("ja search with HideAuthor matched normal article by author: %v", hitIDs(res))
		}
	}
	if res.Total != 2 {
		t.Errorf("ja search with HideAuthor Total = %d, want 2", res.Total)
	}
}

// 著者名だけで一致する記事は、件数・ページ数・各ページの結果のいずれにも影響しない。
func TestSearchHideAuthorPaginationIgnoresAuthorMatches(t *testing.T) {
	x := newTestIndex(t)
	for i := range 23 {
		a := published("author-only-"+string(rune('a'+i)), "部活の記録", "<p>練習の様子。</p>")
		at := time.Now().Add(-time.Duration(i+1) * time.Minute).UTC()
		a.PublishedAt = &at
		mustIndex(t, x, a)
	}
	var want []string
	for i := range 3 {
		id := "title-" + string(rune('a'+i))
		a := published(id, "山田町の記録", "<p>町の話。</p>")
		at := time.Now().Add(-time.Duration(i+1) * time.Hour).UTC()
		a.PublishedAt = &at
		mustIndex(t, x, a)
		want = append(want, id)
	}

	seen := map[string]bool{}
	for page, wantLen := range map[int]int{1: 2, 2: 1, 3: 0} {
		res := searchHidden(t, x, "山田", page, 2)
		if res.Total != 3 || res.TotalPages != 2 {
			t.Errorf("page %d: Total=%d TotalPages=%d, want 3 and 2", page, res.Total, res.TotalPages)
		}
		if len(res.Hits) != wantLen {
			t.Errorf("page %d: %d hits (%v), want %d", page, len(res.Hits), hitIDs(res), wantLen)
		}
		for _, h := range res.Hits {
			seen[h.ID] = true
		}
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("title match %s missing from pages; seen %v", id, seen)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("saw %d distinct hits (%v), want %d title matches only", len(seen), seen, len(want))
	}

	// 著者名を照合に使う既定の検索では全件が一致する。
	if res := mustSearch(t, x, "山田"); res.Total != 26 {
		t.Errorf("default search Total = %d, want 26", res.Total)
	}
}

// 起動時の同期でも論文記事かどうかを索引に入れ、論文記事だけを著者名で一致させる。
func TestSyncFromDBStoresScholarlyForHiddenAuthorSearch(t *testing.T) {
	db := openDB(t)
	svc := article.NewService(db)
	save := func(a *article.Article) *article.Article {
		t.Helper()
		a.AuthorID, a.CategoryID, a.Status = "u1", 1, article.StatusPublished
		if err := svc.Save(a); err != nil {
			t.Fatalf("Save(%s): %v", a.Title, err)
		}
		return a
	}
	normal := save(&article.Article{Title: "模擬店の出店", ContentMarkdown: "焼きそばを販売します"})
	journal := save(&article.Article{Title: "学園祭の経済効果", ContentMarkdown: "来場者の調査", JournalName: "学生経済学会誌"})
	conference := save(&article.Article{Title: "模擬店の動線分析", ContentMarkdown: "混雑の測定", ConferenceName: "第3回 学生研究発表会"})
	coAuthored := save(&article.Article{Title: "ステージ照明の研究", ContentMarkdown: "照明の配置", CoAuthors: "田中一郎"})

	x := newTestIndex(t)
	if err := x.SyncFromDB(context.Background(), db); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}
	assertIDs(t, x, "佐藤花子", normal.ID, journal.ID, conference.ID, coAuthored.ID)
	assertHiddenIDs(t, x, "佐藤花子", journal.ID, conference.ID, coAuthored.ID)
	assertHiddenIDs(t, x, "焼きそば", normal.ID)
}

// 論文記事の印を持たない旧バージョン（v2）のインデックスは、開くときに作り直す。
func TestOpenOrCreateIndexRebuildsV2Mapping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "v2_search.bleve")
	x, err := OpenOrCreateIndex(dir)
	if err != nil {
		t.Fatalf("OpenOrCreateIndex: %v", err)
	}
	mustIndex(t, x, published("old", "旧索引の記事", "<p>本文</p>"))
	if err := x.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sorus_mapping_version"), []byte("2"), 0o644); err != nil {
		t.Fatalf("write version: %v", err)
	}

	x2, err := OpenOrCreateIndex(dir)
	if err != nil {
		t.Fatalf("OpenOrCreateIndex(v2): %v", err)
	}
	defer x2.Close()
	if n, err := x2.DocCount(); err != nil || n != 0 {
		t.Errorf("DocCount after rebuild = %d, %v; want 0 (rebuilt and resynced from DB)", n, err)
	}
	v, err := os.ReadFile(filepath.Join(dir, ".sorus_mapping_version"))
	if err != nil || string(v) != currentMappingVersion || currentMappingVersion == "2" {
		t.Fatalf("version file = %q, err = %v, want %q (newer than 2)", string(v), err, currentMappingVersion)
	}
}
