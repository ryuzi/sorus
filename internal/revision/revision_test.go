package revision

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/database"
)

const (
	writerID = "writer-1"
	editorID = "editor-1"
)

// setup はユーザー2人とカテゴリを登録したDBに、記事を1件保存して返す。
func setup(t *testing.T, path string) (*Service, *article.Service, *article.Article) {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("Connect(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, role) VALUES (?, 'writer', 'h', 'writer')`, writerID)
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, role) VALUES (?, 'editor', 'h', 'editor')`, editorID)
	mustExec(t, db, `INSERT INTO categories (id, name, slug) VALUES (1, 'News', 'news')`)

	articles := article.NewService(db)
	a := &article.Article{Title: "研究室紹介", ContentMarkdown: "初稿", AuthorID: writerID, CategoryID: 1}
	if err := articles.Save(a); err != nil {
		t.Fatalf("save article: %v", err)
	}
	return NewService(db), articles, a
}

func mustExec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func mustSnapshot(t *testing.T, s *Service, a *article.Article, authorID, comment string) (*Revision, bool) {
	t.Helper()
	rev, created, err := s.SaveSnapshot(context.Background(), a, authorID, comment)
	if err != nil {
		t.Fatalf("SaveSnapshot(%q): %v", a.ContentMarkdown, err)
	}
	return rev, created
}

func mustList(t *testing.T, s *Service, articleID string) []*Revision {
	t.Helper()
	revs, err := s.ListRevisions(context.Background(), articleID)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	return revs
}

func TestSaveSnapshot_RecordsSequentialRevisions(t *testing.T) {
	s, _, a := setup(t, ":memory:")
	fixed := time.Date(2026, 10, 20, 9, 0, 0, 0, time.FixedZone("JST", 9*60*60))
	s.now = func() time.Time { return fixed }

	first, created := mustSnapshot(t, s, a, writerID, "手動保存")
	if !created {
		t.Fatal("first snapshot was not created")
	}
	if first.ID == "" || first.ArticleID != a.ID || first.RevisionNum != 1 || first.Title != "研究室紹介" ||
		first.ContentMarkdown != "初稿" || first.AuthorID != writerID || first.Comment != "手動保存" {
		t.Errorf("first revision = %+v", first)
	}
	if !first.CreatedAt.Equal(fixed) || first.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want %v in UTC", first.CreatedAt, fixed)
	}

	a.ContentMarkdown = "校閲で修正した本文"
	second, created := mustSnapshot(t, s, a, editorID, "")
	if !created || second.RevisionNum != 2 || second.AuthorID != editorID || second.ID == first.ID {
		t.Errorf("second revision = %+v (created=%v), want revision 2 by editor", second, created)
	}

	got, err := s.GetRevision(context.Background(), a.ID, 1)
	if err != nil {
		t.Fatalf("GetRevision(1): %v", err)
	}
	if got.ContentMarkdown != "初稿" || got.Comment != "手動保存" || !got.CreatedAt.Equal(fixed) {
		t.Errorf("stored revision 1 = %+v", got)
	}
}

func TestSaveSnapshot_SkipsWhenUnchanged(t *testing.T) {
	s, _, a := setup(t, ":memory:")
	first, _ := mustSnapshot(t, s, a, writerID, "手動保存")

	// タイトル・本文が最新リビジョンと同じなら、保存者やコメントが違っても記録しない。
	again, created := mustSnapshot(t, s, a, editorID, "公開")
	if created {
		t.Error("unchanged snapshot was recorded")
	}
	if again.ID != first.ID || again.RevisionNum != 1 || again.Comment != "手動保存" {
		t.Errorf("skipped snapshot returned %+v, want latest revision %+v", again, first)
	}

	// タイトルだけの変更も新しい版として記録する。
	a.Title = "研究室紹介（改訂）"
	if rev, created := mustSnapshot(t, s, a, writerID, ""); !created || rev.RevisionNum != 2 {
		t.Errorf("title change: revision %d created=%v, want revision 2", rev.RevisionNum, created)
	}

	// 最新以外の版と同じ内容に戻した場合は、新しい版として記録する。
	a.Title = "研究室紹介"
	if rev, created := mustSnapshot(t, s, a, writerID, "差し戻し"); !created || rev.RevisionNum != 3 {
		t.Errorf("revert to older content: revision %d created=%v, want revision 3", rev.RevisionNum, created)
	}
	if n := len(mustList(t, s, a.ID)); n != 3 {
		t.Errorf("revisions = %d, want 3", n)
	}
}

func TestSaveSnapshot_PrunesBeyondMax(t *testing.T) {
	s, _, a := setup(t, ":memory:")
	total := MaxRevisions + 5
	for i := 1; i <= total; i++ {
		a.ContentMarkdown = fmt.Sprintf("版 %d", i)
		if rev, created := mustSnapshot(t, s, a, writerID, ""); !created || rev.RevisionNum != i {
			t.Fatalf("snapshot %d: revision %d created=%v", i, rev.RevisionNum, created)
		}
	}

	revs := mustList(t, s, a.ID)
	if len(revs) != MaxRevisions {
		t.Fatalf("revisions kept = %d, want %d", len(revs), MaxRevisions)
	}
	if revs[0].RevisionNum != total || revs[len(revs)-1].RevisionNum != total-MaxRevisions+1 {
		t.Errorf("kept revisions %d..%d, want %d..%d",
			revs[len(revs)-1].RevisionNum, revs[0].RevisionNum, total-MaxRevisions+1, total)
	}
	if _, err := s.GetRevision(context.Background(), a.ID, total-MaxRevisions); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRevision(pruned) err = %v, want ErrNotFound", err)
	}
}

func TestSaveSnapshot_InvalidInput(t *testing.T) {
	s, _, a := setup(t, ":memory:")
	ctx := context.Background()

	cases := []struct {
		name     string
		a        *article.Article
		authorID string
		want     error
	}{
		{"nil article", nil, writerID, ErrInvalid},
		{"empty article id", &article.Article{Title: "T"}, writerID, ErrInvalid},
		{"empty author", a, "", ErrInvalid},
		{"unknown article", &article.Article{ID: "no-such-article", Title: "T"}, writerID, ErrArticleNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rev, created, err := s.SaveSnapshot(ctx, tc.a, tc.authorID, "")
			if !errors.Is(err, tc.want) || rev != nil || created {
				t.Errorf("SaveSnapshot = (%v, %v, %v), want error %v", rev, created, err, tc.want)
			}
		})
	}

	if _, _, err := s.SaveSnapshot(ctx, a, "no-such-user", ""); err == nil {
		t.Error("SaveSnapshot with unknown author succeeded, want error")
	}
	if n := len(mustList(t, s, a.ID)); n != 0 {
		t.Errorf("revisions after failed snapshots = %d, want 0", n)
	}
}

func TestListRevisions_NewestFirstPerArticle(t *testing.T) {
	s, articles, a := setup(t, ":memory:")
	other := &article.Article{Title: "別の記事", ContentMarkdown: "x", AuthorID: writerID, CategoryID: 1}
	if err := articles.Save(other); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		a.ContentMarkdown = fmt.Sprintf("版 %d", i)
		mustSnapshot(t, s, a, writerID, "")
	}
	mustSnapshot(t, s, other, editorID, "")

	revs := mustList(t, s, a.ID)
	if len(revs) != 3 {
		t.Fatalf("revisions = %d, want 3", len(revs))
	}
	for i, rev := range revs {
		if want := 3 - i; rev.RevisionNum != want || rev.ArticleID != a.ID || rev.ContentMarkdown != fmt.Sprintf("版 %d", want) {
			t.Errorf("revs[%d] = revision %d %q of %s, want revision %d", i, rev.RevisionNum, rev.ContentMarkdown, rev.ArticleID, want)
		}
	}
	if o := mustList(t, s, other.ID); len(o) != 1 || o[0].RevisionNum != 1 {
		t.Errorf("other article revisions = %+v, want one revision numbered 1", o)
	}

	empty := mustList(t, s, "no-such-article")
	if empty == nil || len(empty) != 0 {
		t.Errorf("ListRevisions(unknown) = %#v, want empty non-nil slice", empty)
	}
}

func TestGetRevision_NotFound(t *testing.T) {
	s, _, a := setup(t, ":memory:")
	mustSnapshot(t, s, a, writerID, "")
	ctx := context.Background()
	for _, tc := range []struct {
		articleID string
		num       int
	}{{a.ID, 2}, {a.ID, 0}, {"no-such-article", 1}} {
		if _, err := s.GetRevision(ctx, tc.articleID, tc.num); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetRevision(%q, %d) err = %v, want ErrNotFound", tc.articleID, tc.num, err)
		}
	}
}

func TestSaveSnapshot_ConcurrentNumbersAreUnique(t *testing.T) {
	s, _, a := setup(t, filepath.Join(t.TempDir(), "rev.db"))
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snap := *a
			snap.ContentMarkdown = fmt.Sprintf("並行保存 %d", i)
			_, _, err := s.SaveSnapshot(context.Background(), &snap, writerID, "")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent SaveSnapshot: %v", err)
		}
	}

	revs := mustList(t, s, a.ID)
	if len(revs) != n {
		t.Fatalf("revisions = %d, want %d", len(revs), n)
	}
	for i, rev := range revs {
		if rev.RevisionNum != n-i {
			t.Errorf("revs[%d].RevisionNum = %d, want %d", i, rev.RevisionNum, n-i)
		}
	}
}

func TestSaveSnapshot_CanceledContext(t *testing.T) {
	s, _, a := setup(t, ":memory:")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.SaveSnapshot(ctx, a, writerID, ""); err == nil {
		t.Fatal("SaveSnapshot with canceled context succeeded, want error")
	}
	// 失敗後もトランザクションが残らず、続けて保存できること。
	if _, created := mustSnapshot(t, s, a, writerID, ""); !created {
		t.Error("snapshot after canceled attempt was not created")
	}
}
