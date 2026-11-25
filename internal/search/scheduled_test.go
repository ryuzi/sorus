package search

import (
	"context"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"

	"sorus/internal/article"
)

// searchAt は q の各語で、現在時刻を now として（公開日時が now 以前の記事だけを）検索し、見つかった ID を返す。
func searchAt(t *testing.T, x *Index, now time.Time, terms ...string) []string {
	t.Helper()
	sr, err := x.idx.Search(bleve.NewSearchRequest(buildQuery(terms, now, Options{})))
	if err != nil {
		t.Fatalf("search %v at %v: %v", terms, now, err)
	}
	ids := make([]string, 0, len(sr.Hits))
	for _, h := range sr.Hits {
		ids = append(ids, h.ID)
	}
	return ids
}

// 予約投稿（公開日時が未来の公開記事）も索引に入れておき、検索するときに公開日時で絞る。公開日時を迎えたら、
// 索引を作り直さなくても検索に出る。
func TestScheduledArticleIsIndexedAndFilteredAtQueryTime(t *testing.T) {
	db := openDB(t)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	scheduled := &article.Article{Title: "予約した特集", ContentMarkdown: "焼きそばの特集", AuthorID: "u1", CategoryID: 1,
		Status: article.StatusPublished, PublishedAt: &at}
	if err := article.NewService(db).Save(scheduled); err != nil {
		t.Fatalf("Save: %v", err)
	}

	for name, index := range map[string]func(x *Index) error{
		"SyncFromDB":   func(x *Index) error { return x.SyncFromDB(context.Background(), db) },
		"IndexArticle": func(x *Index) error { return x.IndexArticle(scheduled, "佐藤花子", "学園祭") },
	} {
		t.Run(name, func(t *testing.T) {
			x := newTestIndex(t)
			if err := index(x); err != nil {
				t.Fatal(err)
			}
			if n, err := x.DocCount(); err != nil || n != 1 {
				t.Fatalf("DocCount() = %d, %v; want 1 (scheduled article is indexed)", n, err)
			}
			// 公開日時の前は出さず、公開日時ちょうどから出す。
			if got := searchAt(t, x, at.Add(-time.Second), "焼きそば"); len(got) != 0 {
				t.Errorf("before publish time: %v", got)
			}
			if got := searchAt(t, x, at, "焼きそば"); len(got) != 1 || got[0] != scheduled.ID {
				t.Errorf("at publish time: %v, want [%s]", got, scheduled.ID)
			}
			// 実際の検索は今の時刻で絞るため、まだ出ない。
			assertIDs(t, x, "焼きそば")
		})
	}
}
