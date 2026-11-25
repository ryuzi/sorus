package article

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/database"
)

const (
	testAuthorID   = "user-1"
	testCategoryID = int64(1)
)

// openDB はユーザーとカテゴリを1件ずつ登録した、マイグレーション済みのDBを返す。
func openDB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(path)
	if err != nil {
		t.Fatalf("Connect(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role) VALUES (?, 'writer', 'hash', 'writer')`, testAuthorID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO categories (id, name, slug) VALUES (?, 'News', 'news')`, testCategoryID); err != nil {
		t.Fatalf("insert category: %v", err)
	}
	return db
}

func newTestService(t *testing.T) (*Service, *sqlx.DB) {
	t.Helper()
	db := openDB(t, ":memory:")
	return NewService(db), db
}

func newArticle(title, slug, md string) *Article {
	return &Article{
		Title:           title,
		Slug:            slug,
		ContentMarkdown: md,
		AuthorID:        testAuthorID,
		CategoryID:      testCategoryID,
	}
}

func mustSave(t *testing.T, s *Service, a *Article) *Article {
	t.Helper()
	if err := s.Save(a); err != nil {
		t.Fatalf("Save(%q): %v", a.Title, err)
	}
	return a
}

func mustGet(t *testing.T, s *Service, id string) *Article {
	t.Helper()
	a, err := s.GetByID(id)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", id, err)
	}
	return a
}

func TestSave_CreateRendersAndCachesHTML(t *testing.T) {
	s, db := newTestService(t)
	a := mustSave(t, s, newArticle("Hello", "", "## 見出し\n\n**bold** text\n\n<script>alert(1)</script>"))

	if a.ID == "" {
		t.Fatal("ID was not generated")
	}
	if a.Status != StatusDraft {
		t.Errorf("Status = %q, want %q", a.Status, StatusDraft)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: created=%v updated=%v", a.CreatedAt, a.UpdatedAt)
	}
	if a.PublishedAt != nil {
		t.Errorf("draft PublishedAt = %v, want nil", a.PublishedAt)
	}

	// DB に保存された content_html を直接確認する。
	var html string
	if err := db.Get(&html, `SELECT content_html FROM articles WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<h2 id="見出し">見出し</h2>`, "<strong>bold</strong>"} {
		if !strings.Contains(html, want) {
			t.Errorf("content_html = %q, want to contain %q", html, want)
		}
	}
	if strings.Contains(html, "<script") {
		t.Errorf("content_html = %q, must not contain <script>", html)
	}
	if a.ContentHTML != html {
		t.Errorf("a.ContentHTML = %q, want stored %q", a.ContentHTML, html)
	}

	got := mustGet(t, s, a.ID)
	if got.ContentHTML != html || got.Title != "Hello" || got.Slug != "hello" || got.Status != StatusDraft {
		t.Errorf("GetByID = %+v", got)
	}
	if got.AuthorID != testAuthorID || got.CategoryID != testCategoryID {
		t.Errorf("author/category = %q/%d", got.AuthorID, got.CategoryID)
	}
}

func TestSave_IgnoresCallerSuppliedHTML(t *testing.T) {
	s, _ := newTestService(t)
	a := newArticle("Hello", "", "plain")
	a.ContentHTML = `<img src=x onerror=alert(1)>`
	mustSave(t, s, a)
	if got := mustGet(t, s, a.ID).ContentHTML; strings.Contains(got, "onerror") || !strings.Contains(got, "<p>plain</p>") {
		t.Errorf("content_html = %q, want rendered from markdown only", got)
	}
}

func TestSave_UpdateRegeneratesHTML(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("Hello", "hello", "first"))
	created := a.CreatedAt
	firstUpdated := a.UpdatedAt

	time.Sleep(5 * time.Millisecond)
	a.ContentMarkdown = "## second"
	a.Title = "Hello 2"
	a.CreatedAt = time.Time{} // 呼び出し側の値に関わらず保存済みの作成日時を維持する
	mustSave(t, s, a)

	got := mustGet(t, s, a.ID)
	if !strings.Contains(got.ContentHTML, `<h2 id="second">second</h2>`) || strings.Contains(got.ContentHTML, "first") {
		t.Errorf("content_html = %q, want regenerated", got.ContentHTML)
	}
	if got.Title != "Hello 2" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Slug != "hello" {
		t.Errorf("Slug = %q, want unchanged %q", got.Slug, "hello")
	}
	if !got.CreatedAt.Equal(created) || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v (stored %v), want %v", a.CreatedAt, got.CreatedAt, created)
	}
	if !got.UpdatedAt.After(firstUpdated) {
		t.Errorf("UpdatedAt = %v, want after %v", got.UpdatedAt, firstUpdated)
	}

	var n int
	if err := s.db.Get(&n, `SELECT COUNT(*) FROM articles`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("article count = %d, want 1", n)
	}
}

func TestSave_CreatesWithCallerSuppliedID(t *testing.T) {
	s, _ := newTestService(t)
	a := newArticle("Imported", "", "x")
	a.ID = "wp-123"
	mustSave(t, s, a)
	if got := mustGet(t, s, "wp-123"); got.Title != "Imported" {
		t.Errorf("GetByID = %+v", got)
	}
}

func TestCreate_NewArticle(t *testing.T) {
	s, _ := newTestService(t)
	a := newArticle("Submitted", "submitted", "**x**")
	a.ID = "sync-123"
	if err := s.Create(a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := mustGet(t, s, "sync-123"); got.Title != "Submitted" || !strings.Contains(got.ContentHTML, "<strong>") {
		t.Errorf("GetByID = %+v", got)
	}

	// ID が空なら Save と同じく新しい ID を発行する。
	b := newArticle("Generated", "", "y")
	if err := s.Create(b); err != nil || len(b.ID) != idBytes*2 {
		t.Fatalf("Create without id: id=%q err=%v", b.ID, err)
	}
}

// Create は同じ ID の記事を更新せず ErrExists を返し、呼び出し側の a も変更しない。
func TestCreate_ExistingIDIsNotOverwritten(t *testing.T) {
	s, _ := newTestService(t)
	orig := newArticle("Original", "original", "first")
	orig.ID = "same-id"
	mustSave(t, s, orig)

	a := newArticle("Intruder", "intruder", "second")
	a.ID = "same-id"
	before := *a
	if err := s.Create(a); !errors.Is(err, ErrExists) {
		t.Fatalf("Create(existing id) err = %v, want ErrExists", err)
	}
	if *a != before {
		t.Errorf("article modified on failure: %+v", a)
	}
	if got := mustGet(t, s, "same-id"); got.Title != "Original" || got.ContentMarkdown != "first" || got.Slug != "original" {
		t.Errorf("existing article overwritten: %+v", got)
	}
}

// 同じ ID で並行に Create しても、作成できるのは1件だけで、残りは ErrExists になる。
func TestCreate_ConcurrentSameIDCreatesOnce(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "test.db"))
	s := NewService(db)

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := newArticle(fmt.Sprintf("A%d", i), "", "x")
			a.ID = "contended"
			errs[i] = s.Create(a)
		}(i)
	}
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner >= 0:
			t.Errorf("Create %d and %d both succeeded", winner, i)
		case err == nil:
			winner = i
		case !errors.Is(err, ErrExists):
			t.Errorf("Create %d: %v, want ErrExists", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no Create succeeded")
	}
	if got := mustGet(t, s, "contended"); got.Title != fmt.Sprintf("A%d", winner) {
		t.Errorf("title = %q, want A%d (the successful Create)", got.Title, winner)
	}
}

func TestSave_DuplicateSlugGetsSuffix(t *testing.T) {
	s, _ := newTestService(t)
	want := []string{"hello", "hello-2", "hello-3"}
	for i, w := range want {
		a := mustSave(t, s, newArticle(fmt.Sprintf("Article %d", i), "hello", "x"))
		if a.Slug != w {
			t.Errorf("article %d: Slug = %q, want %q", i, a.Slug, w)
		}
		if got := mustGet(t, s, a.ID).Slug; got != w {
			t.Errorf("article %d: stored slug = %q, want %q", i, got, w)
		}
	}
}

func TestSave_DuplicateSlugFillsFirstGap(t *testing.T) {
	s, _ := newTestService(t)
	mustSave(t, s, newArticle("A", "hello", "x"))
	mustSave(t, s, newArticle("B", "hello-3", "x"))
	if a := mustSave(t, s, newArticle("C", "hello", "x")); a.Slug != "hello-2" {
		t.Errorf("Slug = %q, want %q", a.Slug, "hello-2")
	}
	if a := mustSave(t, s, newArticle("D", "hello", "x")); a.Slug != "hello-4" {
		t.Errorf("Slug = %q, want %q", a.Slug, "hello-4")
	}
}

func TestSave_SlugPrefixIsNotDuplicate(t *testing.T) {
	s, _ := newTestService(t)
	mustSave(t, s, newArticle("A", "hello-world", "x"))
	mustSave(t, s, newArticle("B", "hello-2nd", "x"))
	if a := mustSave(t, s, newArticle("C", "hello", "x")); a.Slug != "hello" {
		t.Errorf("Slug = %q, want %q", a.Slug, "hello")
	}
}

func TestSave_SlugFromTitle(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("Go 言語 入門!", "", "x"))
	if a.Slug != "go-言語-入門" {
		t.Errorf("Slug = %q, want %q", a.Slug, "go-言語-入門")
	}
	b := mustSave(t, s, newArticle("Go 言語 入門!", "", "x"))
	if b.Slug != "go-言語-入門-2" {
		t.Errorf("Slug = %q, want %q", b.Slug, "go-言語-入門-2")
	}
}

func TestSave_NFDSlugIsNormalizedToNFC(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("学生", "がくせい", "x"))
	// 見た目が同じ NFD のスラッグは NFC と同一視され、重複として連番が付く。
	b := mustSave(t, s, newArticle("学生", "\u304b\u3099\u304f\u305b\u3044", "x"))
	if a.Slug != "がくせい" || b.Slug != "がくせい-2" {
		t.Errorf("Slugs = %q, %q, want %q, %q", a.Slug, b.Slug, "がくせい", "がくせい-2")
	}
	c := mustSave(t, s, newArticle("\u304b\u3099\u304f\u305b\u3044 入門", "", "x"))
	if c.Slug != "がくせい-入門" {
		t.Errorf("Slug from NFD title = %q, want %q", c.Slug, "がくせい-入門")
	}
}

func TestSave_UpdateToTakenSlugGetsSuffix(t *testing.T) {
	s, _ := newTestService(t)
	mustSave(t, s, newArticle("A", "taken", "x"))
	b := mustSave(t, s, newArticle("B", "other", "x"))

	b.Slug = "taken"
	mustSave(t, s, b)
	if got := mustGet(t, s, b.ID).Slug; got != "taken-2" {
		t.Errorf("Slug = %q, want %q", got, "taken-2")
	}
	// 自分自身のスラッグとは重複しない。
	mustSave(t, s, b)
	if got := mustGet(t, s, b.ID).Slug; got != "taken-2" {
		t.Errorf("re-save Slug = %q, want %q", got, "taken-2")
	}
}

func TestSave_PreservesExistingUnderscoreSlugWhenUnchanged(t *testing.T) {
	s, db := newTestService(t)
	a := mustSave(t, s, newArticle("Front page", "front-page", "本文"))
	if _, err := db.Exec(`UPDATE articles SET slug = 'front_page' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	a = mustGet(t, s, a.ID)
	a.Title = "Updated front page"
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if a.Slug != "front_page" {
		t.Fatalf("slug after unchanged update = %q, want front_page", a.Slug)
	}
	if got := mustGet(t, s, a.ID).Slug; got != "front_page" {
		t.Errorf("stored slug after unchanged update = %q, want front_page", got)
	}
}

func TestSave_NormalizesChangedSlug(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("Front page", "front_page", "本文"))
	a.Slug = "new_page"
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if a.Slug != "new-page" {
		t.Fatalf("slug after changed update = %q, want new-page", a.Slug)
	}
}

func TestSave_DeletedArticleSlugStaysReserved(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("A", "hello", "x"))
	if err := s.SoftDelete(a.ID); err != nil {
		t.Fatal(err)
	}
	if b := mustSave(t, s, newArticle("B", "hello", "x")); b.Slug != "hello-2" {
		t.Errorf("Slug = %q, want %q", b.Slug, "hello-2")
	}
}

func TestSave_LikeWildcardsInSlugAreLiteral(t *testing.T) {
	s, db := newTestService(t)
	// normalizeSlug を通らない既存データにワイルドカード文字があっても重複判定を誤らない。
	if _, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES ('x', 'X', 'a%b-2', ?, ?)`,
		testAuthorID, testCategoryID); err != nil {
		t.Fatal(err)
	}
	if a := mustSave(t, s, newArticle("A", "ab", "x")); a.Slug != "ab" {
		t.Errorf("Slug = %q, want %q", a.Slug, "ab")
	}
}

func TestSave_ConcurrentSameSlugAreUnique(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "test.db"))
	s := NewService(db)

	const n = 8
	slugs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := newArticle(fmt.Sprintf("A%d", i), "same", "x")
			errs[i] = s.Save(a)
			slugs[i] = a.Slug
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Save %d: %v", i, errs[i])
		}
		if seen[slugs[i]] {
			t.Errorf("duplicate slug %q", slugs[i])
		}
		seen[slugs[i]] = true
	}
	for i := 1; i <= n; i++ {
		w := "same"
		if i > 1 {
			w = fmt.Sprintf("same-%d", i)
		}
		if !seen[w] {
			t.Errorf("missing slug %q in %v", w, slugs)
		}
	}
}

func TestSave_PublishedSetsPublishedAt(t *testing.T) {
	s, _ := newTestService(t)
	before := time.Now().UTC().Add(-time.Second)
	a := newArticle("A", "", "x")
	a.Status = StatusPublished
	mustSave(t, s, a)
	if a.PublishedAt == nil || a.PublishedAt.Before(before) {
		t.Fatalf("PublishedAt = %v, want around now", a.PublishedAt)
	}
	got := mustGet(t, s, a.ID)
	if got.PublishedAt == nil || !got.PublishedAt.Equal(*a.PublishedAt) {
		t.Errorf("stored PublishedAt = %v, want %v", got.PublishedAt, a.PublishedAt)
	}

	// 再保存しても公開日時は変わらない。
	first := *got.PublishedAt
	time.Sleep(5 * time.Millisecond)
	mustSave(t, s, got)
	if again := mustGet(t, s, a.ID); !again.PublishedAt.Equal(first) {
		t.Errorf("PublishedAt changed on re-save: %v -> %v", first, again.PublishedAt)
	}
}

func TestSave_Validation(t *testing.T) {
	s, _ := newTestService(t)
	cases := map[string]*Article{
		"nil":         nil,
		"empty title": newArticle("  ", "x", "x"),
		"bad status":  func() *Article { a := newArticle("A", "", "x"); a.Status = "archived"; return a }(),
		// 予約は公開と未来の公開日時で表し、仮想の状態 scheduled は保存しない。
		"scheduled is not stored": func() *Article {
			a := newArticle("A", "", "x")
			a.Status = StatusScheduled
			at := time.Now().Add(time.Hour)
			a.PublishedAt = &at
			return a
		}(),
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.Save(a); !errors.Is(err, ErrInvalid) {
				t.Errorf("Save = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestSave_ReviewAndScheduledStatuses(t *testing.T) {
	s, _ := newTestService(t)

	review := newArticle("校閲中", "", "x")
	review.Status = StatusReview
	mustSave(t, s, review)
	if got := mustGet(t, s, review.ID); got.Status != StatusReview || got.PublishedAt != nil {
		t.Errorf("review article = status %q published_at %v, want review and nil", got.Status, got.PublishedAt)
	}

	// 予約投稿は、状態が公開で公開日時が未来の記事として保存する。
	jst := time.FixedZone("JST", 9*60*60)
	at := time.Now().Add(24 * time.Hour).Truncate(time.Second).In(jst)
	scheduled := newArticle("予約投稿", "", "x")
	scheduled.Status = StatusPublished
	scheduled.PublishedAt = &at
	mustSave(t, s, scheduled)
	if scheduled.PublishedAt.Location() != time.UTC || !scheduled.PublishedAt.Equal(at) {
		t.Errorf("saved PublishedAt = %v, want %v in UTC", scheduled.PublishedAt, at)
	}
	got := mustGet(t, s, scheduled.ID)
	if got.Status != StatusPublished || got.PublishedAt == nil || !got.PublishedAt.Equal(at) {
		t.Errorf("scheduled article = status %q published_at %v, want published and %v", got.Status, got.PublishedAt, at)
	}
	if now := time.Now(); !IsScheduled(got, now) || IsVisible(got, now) || ViewStatus(got, now) != StatusScheduled {
		t.Errorf("scheduled article: IsScheduled %v IsVisible %v ViewStatus %q", IsScheduled(got, now), IsVisible(got, now), ViewStatus(got, now))
	}

	// 予約を取り消して下書きに戻すと、まだ公開していない予定の日時は残さない。
	got.Status = StatusDraft
	mustSave(t, s, got)
	if again := mustGet(t, s, scheduled.ID); again.Status != StatusDraft || again.PublishedAt != nil {
		t.Errorf("after unscheduling = status %q published_at %v", again.Status, again.PublishedAt)
	}

	list, err := s.ListPublished(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("ListPublished = %v, want review/scheduled articles excluded", ids(list))
	}
}

func TestSave_AcademicMetadataRoundTrip(t *testing.T) {
	s, _ := newTestService(t)
	a := newArticle("深層学習による古文書の翻刻", "", "本文")
	a.DOI = "10.1234/jdh.2026.001"
	a.JournalName = "日本デジタル人文学会誌"
	a.ConferenceName = "人文科学とコンピュータシンポジウム 2026"
	a.CoAuthors = "山田太郎, 佐藤花子"
	mustSave(t, s, a)

	got := mustGet(t, s, a.ID)
	if got.DOI != a.DOI || got.JournalName != a.JournalName || got.ConferenceName != a.ConferenceName || got.CoAuthors != a.CoAuthors {
		t.Errorf("stored metadata = %q %q %q %q", got.DOI, got.JournalName, got.ConferenceName, got.CoAuthors)
	}

	got.DOI = ""
	got.CoAuthors = "山田太郎, 佐藤花子, 鈴木一郎"
	mustSave(t, s, got)
	bySlug, err := s.GetBySlug(a.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if bySlug.DOI != "" || bySlug.CoAuthors != "山田太郎, 佐藤花子, 鈴木一郎" || bySlug.JournalName != a.JournalName {
		t.Errorf("updated metadata = DOI %q co-authors %q journal %q", bySlug.DOI, bySlug.CoAuthors, bySlug.JournalName)
	}
}

func TestSave_FailureLeavesArticleUnchanged(t *testing.T) {
	s, _ := newTestService(t)
	a := newArticle("A", "", "**x**")
	a.CategoryID = 999 // 外部キー違反
	orig := *a
	if err := s.Save(a); err == nil {
		t.Fatal("Save with unknown category succeeded")
	}
	if *a != orig {
		t.Errorf("article modified on failure: %+v", a)
	}
	var n int
	if err := s.db.Get(&n, `SELECT COUNT(*) FROM articles`); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("article count = %d, want 0", n)
	}
}

func TestGet_NotFound(t *testing.T) {
	s, _ := newTestService(t)
	if _, err := s.GetByID("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID = %v, want ErrNotFound", err)
	}
	if _, err := s.GetBySlug("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBySlug = %v, want ErrNotFound", err)
	}
}

func TestGetBySlug(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("A", "my-post", "x"))
	mustSave(t, s, newArticle("B", "my-post", "y"))
	got, err := s.GetBySlug("my-post")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID {
		t.Errorf("GetBySlug ID = %q, want %q", got.ID, a.ID)
	}
}

func TestSoftDelete(t *testing.T) {
	s, _ := newTestService(t)
	a := newArticle("A", "", "x")
	a.Status = StatusPublished
	mustSave(t, s, a)
	b := newArticle("B", "", "y")
	b.Status = StatusPublished
	mustSave(t, s, b)

	if err := s.SoftDelete(a.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	got := mustGet(t, s, a.ID)
	if got.Status != StatusDeleted {
		t.Errorf("Status = %q, want %q", got.Status, StatusDeleted)
	}
	if !got.UpdatedAt.After(a.UpdatedAt) && !got.UpdatedAt.Equal(a.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want >= %v", got.UpdatedAt, a.UpdatedAt)
	}

	list, err := s.ListPublished(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != b.ID {
		t.Errorf("ListPublished = %v, want only %q", ids(list), b.ID)
	}

	// 削除済みに対しても成功する。
	if err := s.SoftDelete(a.ID); err != nil {
		t.Errorf("second SoftDelete: %v", err)
	}
	if err := s.SoftDelete("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SoftDelete(missing) = %v, want ErrNotFound", err)
	}
}

func TestListPublished(t *testing.T) {
	s, _ := newTestService(t)
	now := time.Now().UTC()
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	save := func(title string, status Status, publishedAt *time.Time) *Article {
		a := newArticle(title, "", "x")
		a.Status = status
		a.PublishedAt = publishedAt
		return mustSave(t, s, a)
	}
	old := save("old", StatusPublished, at(-3*time.Hour))
	newest := save("newest", StatusPublished, at(-1*time.Hour))
	mid := save("mid", StatusPublished, at(-2*time.Hour))
	save("draft", StatusDraft, at(-30*time.Minute))
	save("scheduled", StatusPublished, at(time.Hour))
	save("deleted", StatusDeleted, at(-30*time.Minute))
	// 他のタイムゾーンで渡されても UTC に揃えて比較・並び替えされる。
	jst := time.FixedZone("JST", 9*60*60)
	tz := save("tz", StatusPublished, func() *time.Time { t := now.Add(-90 * time.Minute).In(jst); return &t }())

	list, err := s.ListPublished(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{newest.ID, tz.ID, mid.ID, old.ID}
	if got := ids(list); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ListPublished = %v, want %v", got, want)
	}
	if list[0].ContentHTML == "" {
		t.Error("listed article has empty ContentHTML")
	}

	page, err := s.ListPublished(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page); strings.Join(got, ",") != strings.Join(want[1:3], ",") {
		t.Errorf("ListPublished(2, 1) = %v, want %v", got, want[1:3])
	}

	empty, err := s.ListPublished(10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("ListPublished past end = %#v, want empty non-nil slice", empty)
	}
}

func TestListPublished_InvalidArgs(t *testing.T) {
	s, _ := newTestService(t)
	for _, c := range [][2]int{{0, 0}, {-1, 0}, {10, -1}} {
		if _, err := s.ListPublished(c[0], c[1]); err == nil {
			t.Errorf("ListPublished(%d, %d) succeeded, want error", c[0], c[1])
		}
	}
}

func TestNormalizeSlug(t *testing.T) {
	cases := map[string]string{
		"Hello World":        "hello-world",
		"  Hello   World  ":  "hello-world",
		"already-a-slug":     "already-a-slug",
		"snake_case__name":   "snake-case-name",
		"Go 言語 入門!":          "go-言語-入門",
		"日本語タイトル":            "日本語タイトル",
		"What's new? (2024)": "whats-new-2024",
		"--trim--":           "trim",
		"!!!":                fallbackSlug,
		"":                   fallbackSlug,
		"a/b?c#d":            "abcd",
		"<script>x</script>": "scriptxscript",
		// NFD の結合濁点は NFC に合成してから判定する（結合文字を落とさない）。
		"\u304b\u3099\u304f\u305b\u3044": "がくせい",
		"\u30cf\u309a\u30fc\u30c6\u30a3": "パーティ",
	}
	for in, want := range cases {
		if got := normalizeSlug(in); got != want {
			t.Errorf("normalizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func ids(list []*Article) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.ID
	}
	return out
}

func TestKindValid(t *testing.T) {
	for k, want := range map[Kind]bool{KindPost: true, KindPage: true, "": false, "Page": false, "posts": false} {
		if got := k.Valid(); got != want {
			t.Errorf("Kind(%q).Valid() = %v, want %v", k, got, want)
		}
	}
}

func TestSave_KindDefaultsToPostAndRoundTrips(t *testing.T) {
	s, _ := newTestService(t)
	post := mustSave(t, s, newArticle("記事", "news", "本文"))
	if post.Kind != KindPost || mustGet(t, s, post.ID).Kind != KindPost {
		t.Errorf("default kind = %q, want post", post.Kind)
	}
	page := newArticle("研究紹介", "research", "本文")
	page.Kind = KindPage
	mustSave(t, s, page)
	if got := mustGet(t, s, page.ID); got.Kind != KindPage {
		t.Errorf("saved kind = %q, want page", got.Kind)
	}
	// 種類を変えて保存できる。
	page.Kind = KindPost
	mustSave(t, s, page)
	if got := mustGet(t, s, page.ID); got.Kind != KindPost {
		t.Errorf("kind after change = %q, want post", got.Kind)
	}

	bad := newArticle("不明", "unknown", "")
	bad.Kind = "widget"
	if err := s.Save(bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("Save(kind=widget) = %v, want ErrInvalid", err)
	}
}

// reservedWords は仕様（サイト構成 3.2）の予約語。
var reservedWords = []string{"admin", "api", "articles", "categories", "search", "static", "uploads", "attachments",
	"og", "feed.xml", "sitemap.xml", "robots.txt", "favicon.ico", "llms.txt", "llms-full.txt", "manifest.json",
	"sw.js", "healthz"}

func TestIsReservedPageSlug(t *testing.T) {
	for _, w := range reservedWords {
		if !IsReservedPageSlug(w) || !IsReservedPageSlug(strings.ToUpper(w)) {
			t.Errorf("IsReservedPageSlug(%q) = false, want true (case-insensitive)", w)
		}
	}
	for _, ok := range []string{"research", "members", "about", "article", "admins", "研究", "searching", ""} {
		if IsReservedPageSlug(ok) {
			t.Errorf("IsReservedPageSlug(%q) = true, want false", ok)
		}
	}
}

func TestSave_PageRejectsReservedSlug(t *testing.T) {
	s, db := newTestService(t)
	for _, w := range reservedWords {
		// 記号は正規化で落ちるため、正規化後も予約語のままの語だけを保存で確かめる。
		if NormalizeSlug(w) != w {
			continue
		}
		for _, slug := range []string{w, strings.ToUpper(w)} {
			a := newArticle("ページ", slug, "本文")
			a.Kind = KindPage
			err := s.Save(a)
			if !errors.Is(err, ErrReservedSlug) || !errors.Is(err, ErrInvalid) {
				t.Errorf("Save(page slug %q) = %v, want ErrReservedSlug and ErrInvalid", slug, err)
			}
			if a.ID != "" {
				t.Errorf("Save(page slug %q) changed the article: %+v", slug, a)
			}
		}
	}
	// スラッグが空ならタイトルから作ったスラッグを検査する。
	titled := newArticle("Search", "", "")
	titled.Kind = KindPage
	if err := s.Save(titled); !errors.Is(err, ErrReservedSlug) {
		t.Errorf("Save(page titled Search) = %v, want ErrReservedSlug", err)
	}
	// 記事（post）は /articles/<slug> で配信するため予約語でもよい。
	mustSave(t, s, newArticle("記事", "search", ""))
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM articles WHERE kind = 'page'`); err != nil || n != 0 {
		t.Errorf("pages saved = %d (%v), want 0", n, err)
	}
}

func TestPathReservedSlug(t *testing.T) {
	for p, want := range map[string]string{"/hooks/sync": "hooks", "/api/sync": "api", "/api/sync-ab12": "api",
		"hooks": "hooks", " /Hooks/sync ": "Hooks", "/": "", "": ""} {
		if got := PathReservedSlug(p); got != want {
			t.Errorf("PathReservedSlug(%q) = %q, want %q", p, got, want)
		}
	}
}

// WithReservedSlugs で加えた語は、固定の予約語と同じくページのスラッグに使えない（記事には使える）。
func TestSave_PageRejectsConfiguredReservedSlug(t *testing.T) {
	_, db := newTestService(t)
	s := NewService(db, WithReservedSlugs("Hooks", " ", ""))
	for _, slug := range []string{"hooks", "HOOKS", "admin"} {
		if !s.IsReservedPageSlug(slug) {
			t.Errorf("IsReservedPageSlug(%q) = false, want true", slug)
		}
		a := newArticle("ページ", slug, "")
		a.Kind = KindPage
		if err := s.Create(a); !errors.Is(err, ErrReservedSlug) || !errors.Is(err, ErrInvalid) {
			t.Errorf("Create(page slug %q) = %v, want ErrReservedSlug and ErrInvalid", slug, err)
		}
	}
	if s.IsReservedPageSlug("") || s.IsReservedPageSlug("research") {
		t.Error("IsReservedPageSlug accepted an empty or ordinary slug as reserved")
	}
	titled := newArticle("Hooks", "", "")
	titled.Kind = KindPage
	if err := s.Save(titled); !errors.Is(err, ErrReservedSlug) {
		t.Errorf("Save(page titled Hooks) = %v, want ErrReservedSlug", err)
	}
	mustSave(t, s, newArticle("連携の記事", "hooks", ""))

	// 指定しない Service は固定の予約語だけを拒む。
	plain := NewService(db)
	if plain.IsReservedPageSlug("hooks") {
		t.Error("default service treats hooks as reserved")
	}
	page := newArticle("連携", "hooks-page", "")
	page.Kind = KindPage
	mustSave(t, plain, page)
}

func TestListPublished_ExcludesPages(t *testing.T) {
	s, _ := newTestService(t)
	past := time.Now().UTC().Add(-time.Hour)
	post := newArticle("記事", "news", "")
	post.Status, post.PublishedAt = StatusPublished, &past
	mustSave(t, s, post)
	page := newArticle("研究", "research", "")
	page.Status, page.PublishedAt, page.Kind = StatusPublished, &past, KindPage
	mustSave(t, s, page)
	got, err := s.ListPublished(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != post.ID {
		t.Errorf("ListPublished = %v, want only the post", got)
	}
}

func TestIsScheduledAndIsVisible(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	cases := []struct {
		name      string
		a         *Article
		scheduled bool
		visible   bool
		view      Status
	}{
		{"future published", &Article{Status: StatusPublished, PublishedAt: at(time.Minute)}, true, false, StatusScheduled},
		{"published at now", &Article{Status: StatusPublished, PublishedAt: at(0)}, false, true, StatusPublished},
		{"past published", &Article{Status: StatusPublished, PublishedAt: at(-time.Minute)}, false, true, StatusPublished},
		{"published without time", &Article{Status: StatusPublished}, false, false, StatusPublished},
		{"draft with future time", &Article{Status: StatusDraft, PublishedAt: at(time.Hour)}, false, false, StatusDraft},
		{"review", &Article{Status: StatusReview}, false, false, StatusReview},
		{"deleted with past time", &Article{Status: StatusDeleted, PublishedAt: at(-time.Hour)}, false, false, StatusDeleted},
	}
	for _, c := range cases {
		if got := IsScheduled(c.a, now); got != c.scheduled {
			t.Errorf("%s: IsScheduled = %v, want %v", c.name, got, c.scheduled)
		}
		if got := IsVisible(c.a, now); got != c.visible {
			t.Errorf("%s: IsVisible = %v, want %v", c.name, got, c.visible)
		}
		if got := ViewStatus(c.a, now); got != c.view {
			t.Errorf("%s: ViewStatus = %q, want %q", c.name, got, c.view)
		}
	}
	if IsScheduled(nil, now) || IsVisible(nil, now) {
		t.Error("nil article is scheduled or visible")
	}
	// 他のタイムゾーンの公開日時も同じ時刻として比べる。
	jst := time.FixedZone("JST", 9*60*60)
	if a := (&Article{Status: StatusPublished, PublishedAt: func() *time.Time { t := now.In(jst); return &t }()}); !IsVisible(a, now) {
		t.Error("published_at in JST equal to now is not visible")
	}
}

// SQL の条件（VisibleCondition・ScheduledCondition・StatusCondition）は、IsVisible・IsScheduled・ViewStatus と同じ記事を選ぶ。
func TestConditionsMatchGoRules(t *testing.T) {
	s, db := newTestService(t)
	base := time.Now().UTC()
	at := func(d time.Duration) *time.Time { t := base.Add(d); return &t }
	saved := map[string]*Article{}
	for name, a := range map[string]*Article{
		"past":      {Status: StatusPublished, PublishedAt: at(-time.Hour)},
		"future":    {Status: StatusPublished, PublishedAt: at(time.Hour)},
		"draft":     {Status: StatusDraft},
		"review":    {Status: StatusReview},
		"deleted":   {Status: StatusDeleted, PublishedAt: at(-time.Hour)},
		"published": {Status: StatusPublished},
	} {
		a.Title, a.AuthorID, a.CategoryID = name, testAuthorID, testCategoryID
		saved[name] = mustSave(t, s, a)
	}
	// 公開日時を省いた記事は保存した時刻が公開日時になるため、どの記事の公開日時とも重ならない時刻で比べる。
	now := base.Add(time.Minute)
	query := func(cond string, args ...any) []string {
		var got []string
		if err := db.Select(&got, `SELECT a.title FROM articles a WHERE `+cond+` ORDER BY a.title`, args...); err != nil {
			t.Fatalf("select %s: %v", cond, err)
		}
		return got
	}
	want := func(keep func(*Article) bool) []string {
		var out []string
		for name, a := range saved {
			if keep(a) {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return out
	}
	check := func(label string, got, want []string) {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", label, got, want)
		}
	}
	check("VisibleCondition", query(VisibleCondition, StatusPublished, now), want(func(a *Article) bool { return IsVisible(a, now) }))
	check("ScheduledCondition", query(ScheduledCondition, StatusPublished, now), want(func(a *Article) bool { return IsScheduled(a, now) }))
	for _, st := range []Status{StatusDraft, StatusReview, StatusScheduled, StatusPublished, StatusDeleted} {
		cond, args := StatusCondition(st, now)
		check("StatusCondition("+string(st)+")", query(cond, args...), want(func(a *Article) bool { return ViewStatus(a, now) == st }))
	}
}

// 一度読者に見えた記事は、予約（未来の公開日時）に戻せない。予約中の記事の日時は変えられる。
func TestSave_RefusesSchedulingVisibleArticle(t *testing.T) {
	s, _ := newTestService(t)
	future := time.Now().Add(2 * time.Hour)

	visible := newArticle("公開中", "", "x")
	visible.Status = StatusPublished
	mustSave(t, s, visible)
	first := *visible.PublishedAt
	visible.PublishedAt = &future
	if err := s.Save(visible); !errors.Is(err, ErrPublished) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("Save(visible -> scheduled) = %v, want ErrPublished and ErrInvalid", err)
	}
	if got := mustGet(t, s, visible.ID); !got.PublishedAt.Equal(first) {
		t.Errorf("published_at = %v, want kept %v", got.PublishedAt, first)
	}

	// 公開したあと下書きに戻した記事も、最初の公開日時を持つため予約にできない。
	back := mustGet(t, s, visible.ID)
	back.Status = StatusDraft
	mustSave(t, s, back)
	back.Status, back.PublishedAt = StatusPublished, &future
	if err := s.Save(back); !errors.Is(err, ErrPublished) {
		t.Fatalf("Save(unpublished -> scheduled) = %v, want ErrPublished", err)
	}

	scheduled := newArticle("予約", "", "x")
	scheduled.Status, scheduled.PublishedAt = StatusPublished, &future
	mustSave(t, s, scheduled)
	later := future.Add(time.Hour)
	scheduled.PublishedAt = &later
	mustSave(t, s, scheduled)
	if got := mustGet(t, s, scheduled.ID); !got.PublishedAt.Equal(later) {
		t.Errorf("rescheduled published_at = %v, want %v", got.PublishedAt, later)
	}
}

// ゴミ箱に移した予約中の記事は予定の日時を残さず、公開したことのある記事は最初の公開日時を残す。
func TestSoftDelete_ClearsOnlyFuturePublishedAt(t *testing.T) {
	s, _ := newTestService(t)
	future := time.Now().Add(time.Hour)
	scheduled := newArticle("予約", "", "x")
	scheduled.Status, scheduled.PublishedAt = StatusPublished, &future
	mustSave(t, s, scheduled)
	visible := newArticle("公開中", "", "x")
	visible.Status = StatusPublished
	mustSave(t, s, visible)

	for _, id := range []string{scheduled.ID, visible.ID} {
		if err := s.SoftDelete(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustGet(t, s, scheduled.ID); got.Status != StatusDeleted || got.PublishedAt != nil {
		t.Errorf("deleted scheduled article = status %q published_at %v, want deleted and nil", got.Status, got.PublishedAt)
	}
	if got := mustGet(t, s, visible.ID); got.Status != StatusDeleted || got.PublishedAt == nil || !got.PublishedAt.Equal(*visible.PublishedAt) {
		t.Errorf("deleted visible article = status %q published_at %v, want deleted and kept", got.Status, got.PublishedAt)
	}
}

func TestSave_MultilingualDefaultsAndRoundTrip(t *testing.T) {
	s, _ := newTestService(t)

	// デフォルト値の確認 (Lang = "ja", TranslationGroupID = a.ID)
	defaultArt := newArticle("日本語デフォルト", "default-ja", "本文")
	mustSave(t, s, defaultArt)

	gotDefault := mustGet(t, s, defaultArt.ID)
	if gotDefault.Lang != "ja" {
		t.Errorf("default lang = %q, want 'ja'", gotDefault.Lang)
	}
	if gotDefault.TranslationGroupID != gotDefault.ID {
		t.Errorf("default translation_group_id = %q, want %q", gotDefault.TranslationGroupID, gotDefault.ID)
	}
	if gotDefault.SourceArticleID.Valid {
		t.Errorf("default source_article_id is valid (%v), want null", gotDefault.SourceArticleID.String)
	}
	if gotDefault.IsOutdated {
		t.Errorf("default is_outdated = %v, want false", gotDefault.IsOutdated)
	}

	// 明示的な値のラウンドトリップ確認
	enArt := newArticle("English Article", "english-art", "English content")
	enArt.Lang = "en"
	enArt.TranslationGroupID = defaultArt.ID
	enArt.SourceArticleID = sql.NullString{String: defaultArt.ID, Valid: true}
	enArt.IsOutdated = true
	mustSave(t, s, enArt)

	gotEN := mustGet(t, s, enArt.ID)
	if gotEN.Lang != "en" {
		t.Errorf("saved lang = %q, want 'en'", gotEN.Lang)
	}
	if gotEN.TranslationGroupID != defaultArt.ID {
		t.Errorf("saved translation_group_id = %q, want %q", gotEN.TranslationGroupID, defaultArt.ID)
	}
	if !gotEN.SourceArticleID.Valid || gotEN.SourceArticleID.String != defaultArt.ID {
		t.Errorf("saved source_article_id = %+v, want valid %q", gotEN.SourceArticleID, defaultArt.ID)
	}
	if !gotEN.IsOutdated {
		t.Errorf("saved is_outdated = %v, want true", gotEN.IsOutdated)
	}

	// UPDATE でも維持・更新されること
	gotEN.Title = "English Article Updated"
	gotEN.IsOutdated = false
	mustSave(t, s, gotEN)

	gotENUpdated := mustGet(t, s, enArt.ID)
	if gotENUpdated.Title != "English Article Updated" {
		t.Errorf("updated title = %q, want 'English Article Updated'", gotENUpdated.Title)
	}
	if gotENUpdated.Lang != "en" {
		t.Errorf("updated lang = %q, want 'en'", gotENUpdated.Lang)
	}
	if gotENUpdated.IsOutdated {
		t.Errorf("updated is_outdated = %v, want false", gotENUpdated.IsOutdated)
	}
}

func TestTranslations_ListAndGet(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)

	// 原文記事（日本語）
	jaArt := newArticle("日本語記事", "ja-art", "日本語")
	mustSave(t, s, jaArt)
	groupID := jaArt.ID

	// 翻訳記事（英語）
	enArt := newArticle("English Article", "en-art", "English")
	enArt.Lang = "en"
	enArt.TranslationGroupID = groupID
	enArt.SourceArticleID = sql.NullString{String: jaArt.ID, Valid: true}
	mustSave(t, s, enArt)

	// Translations: 言語コード順（en, ja）で取得されること
	translations, err := s.Translations(ctx, groupID)
	if err != nil {
		t.Fatalf("Translations(%q): %v", groupID, err)
	}
	if len(translations) != 2 {
		t.Fatalf("Translations length = %d, want 2", len(translations))
	}
	wantLangs := []string{"en", "ja"}
	for i, want := range wantLangs {
		if translations[i].Lang != want {
			t.Errorf("translations[%d].Lang = %q, want %q", i, translations[i].Lang, want)
		}
	}

	// Translation: 個別言語の取得
	gotEN, err := s.Translation(ctx, groupID, "en")
	if err != nil {
		t.Fatalf("Translation(en): %v", err)
	}
	if gotEN.ID != enArt.ID {
		t.Errorf("Translation(en).ID = %q, want %q", gotEN.ID, enArt.ID)
	}

	gotJA, err := s.Translation(ctx, groupID, "ja")
	if err != nil {
		t.Fatalf("Translation(ja): %v", err)
	}
	if gotJA.ID != jaArt.ID {
		t.Errorf("Translation(ja).ID = %q, want %q", gotJA.ID, jaArt.ID)
	}

	// 存在しない言語
	if _, err := s.Translation(ctx, groupID, "fr"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Translation(fr) err = %v, want ErrNotFound", err)
	}

	// 存在しないグループ
	emptyTranslations, err := s.Translations(ctx, "nonexistent-group")
	if err != nil {
		t.Fatalf("Translations(nonexistent): %v", err)
	}
	if len(emptyTranslations) != 0 {
		t.Errorf("Translations(nonexistent) length = %d, want 0", len(emptyTranslations))
	}

	if _, err := s.Translation(ctx, "nonexistent-group", "ja"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Translation(nonexistent) err = %v, want ErrNotFound", err)
	}
}

func TestMarkTranslationsOutdated_AndClearOutdated(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)

	// 原文記事
	jaArt := newArticle("元記事", "source-art", "本文")
	mustSave(t, s, jaArt)
	groupID := jaArt.ID

	// 翻訳記事 (en)
	enArt := newArticle("English translation", "en-trans", "Body")
	enArt.Lang = "en"
	enArt.TranslationGroupID = groupID
	enArt.SourceArticleID = sql.NullString{String: jaArt.ID, Valid: true}
	mustSave(t, s, enArt)

	// 初期状態の確認
	for _, id := range []string{jaArt.ID, enArt.ID} {
		if a := mustGet(t, s, id); a.IsOutdated {
			t.Errorf("article %s initially outdated = true, want false", id)
		}
	}

	// 原文更新に伴い、同一グループ内の他言語記事を outdated にマーク
	if err := s.MarkTranslationsOutdated(ctx, groupID, jaArt.ID); err != nil {
		t.Fatalf("MarkTranslationsOutdated: %v", err)
	}

	if a := mustGet(t, s, jaArt.ID); a.IsOutdated {
		t.Errorf("exceptID article %s is outdated = true, want false", jaArt.ID)
	}
	if a := mustGet(t, s, enArt.ID); !a.IsOutdated {
		t.Errorf("en article %s is outdated = false, want true", enArt.ID)
	}

	// en の outdated を解除
	if err := s.ClearOutdated(ctx, enArt.ID); err != nil {
		t.Fatalf("ClearOutdated(en): %v", err)
	}

	if a := mustGet(t, s, enArt.ID); a.IsOutdated {
		t.Errorf("en article %s after clear is outdated = true, want false", enArt.ID)
	}

	// 存在しない記事の ClearOutdated
	if err := s.ClearOutdated(ctx, "nonexistent-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClearOutdated(nonexistent) err = %v, want ErrNotFound", err)
	}
}

func TestSave_LangValidation(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)

	// 空文字 -> "ja" に正規化
	art1 := newArticle("空言語", "empty-lang", "本文")
	art1.Lang = ""
	mustSave(t, s, art1)
	if art1.Lang != "ja" {
		t.Errorf("art1.Lang = %q, want 'ja'", art1.Lang)
	}

	// 大文字 "EN" -> 小文字 "en" に正規化
	art2 := newArticle("大文字英語", "upper-en", "Body")
	art2.Lang = "EN"
	mustSave(t, s, art2)
	if art2.Lang != "en" {
		t.Errorf("art2.Lang = %q, want 'en'", art2.Lang)
	}

	// 前後空白つき " ja " -> "ja"
	art3 := newArticle("空白日本語", "space-ja", "本文")
	art3.Lang = "  ja  "
	mustSave(t, s, art3)
	if art3.Lang != "ja" {
		t.Errorf("art3.Lang = %q, want 'ja'", art3.Lang)
	}

	// 未対応言語コード（例: zh, fr, de）は ErrInvalid
	for _, badLang := range []string{"zh", "fr", "de", "es", "invalid"} {
		artBad := newArticle("未対応言語", "bad-lang-"+badLang, "Text")
		artBad.Lang = badLang
		err := s.Save(artBad)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Save with lang %q err = %v, want ErrInvalid", badLang, err)
		}
	}
}

func TestArticle_URL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		art  Article
		want string
	}{
		{
			name: "日本語 post",
			art:  Article{Kind: KindPost, Slug: "hello-world", Lang: "ja"},
			want: "/articles/hello-world",
		},
		{
			name: "日本語 page",
			art:  Article{Kind: KindPage, Slug: "about", Lang: "ja"},
			want: "/about",
		},
		{
			name: "言語未設定 post (既定 ja)",
			art:  Article{Kind: KindPost, Slug: "intro", Lang: ""},
			want: "/articles/intro",
		},
		{
			name: "英語 post",
			art:  Article{Kind: KindPost, Slug: "hello-world", Lang: "en"},
			want: "/en/articles/hello-world",
		},
		{
			name: "英語 page",
			art:  Article{Kind: KindPage, Slug: "about", Lang: "en"},
			want: "/en/about",
		},
		{
			name: "英語 post 大文字 EN",
			art:  Article{Kind: KindPost, Slug: "sample", Lang: "EN"},
			want: "/en/articles/sample",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.art.URL()
			if got != tc.want {
				t.Errorf("URL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// trashArticle は記事を保存してゴミ箱に入れ、その記事を返す。
func trashArticle(t *testing.T, svc *Service, title, slug string) *Article {
	t.Helper()
	a := newArticle(title, slug, "本文")
	if err := svc.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := svc.SoftDelete(a.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	return a
}

// countRows は table の article_id = id の行数を返す。
func countRows(t *testing.T, db *sqlx.DB, table, id string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM `+table+` WHERE article_id = ?`, id); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestRestoreFromTrash(t *testing.T) {
	svc, _ := newTestService(t)
	a := trashArticle(t, svc, "戻す記事", "restore-me")
	if err := svc.RestoreFromTrash(a.ID); err != nil {
		t.Fatalf("RestoreFromTrash: %v", err)
	}
	got, err := svc.GetByID(a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != StatusDraft {
		t.Errorf("status = %q, want draft", got.Status)
	}
	// ゴミ箱にない記事・存在しない記事は戻さない。
	if err := svc.RestoreFromTrash(a.ID); !errors.Is(err, ErrNotInTrash) {
		t.Errorf("restore a draft: err = %v, want ErrNotInTrash", err)
	}
	if err := svc.RestoreFromTrash("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore a missing article: err = %v, want ErrNotFound", err)
	}
}

// 同じ翻訳グループの同じ言語の記事が既にあれば、ゴミ箱から戻すと一意でなくなるため ErrRestoreConflict を返す。
func TestRestoreFromTrashConflict(t *testing.T) {
	svc, _ := newTestService(t)
	old := trashArticle(t, svc, "古い記事", "old")
	b := newArticle("新しい記事", "new", "本文")
	b.TranslationGroupID = old.TranslationGroupID
	if err := svc.Save(b); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := svc.RestoreFromTrash(old.ID); !errors.Is(err, ErrRestoreConflict) {
		t.Fatalf("err = %v, want ErrRestoreConflict", err)
	}
	got, _ := svc.GetByID(old.ID)
	if got.Status != StatusDeleted {
		t.Errorf("status = %q, want deleted", got.Status)
	}
}

func TestHardDelete(t *testing.T) {
	svc, db := newTestService(t)
	a := trashArticle(t, svc, "消す記事", "purge-me")
	tr := newArticle("翻訳", "purge-me-en", "body")
	tr.Lang = "en"
	tr.TranslationGroupID = a.TranslationGroupID
	tr.SourceArticleID = sql.NullString{String: a.ID, Valid: true}
	if err := svc.Save(tr); err != nil {
		t.Fatalf("Save translation: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO article_revisions (id, article_id, revision_num, title, content_markdown, author_id) VALUES ('r1', ?, 1, 't', 'b', 'user-1')`,
		`INSERT INTO article_comments (id, article_id, user_id, content) VALUES ('c1', ?, 'user-1', 'memo')`,
		`INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by) VALUES ('f1', ?, 'a.pdf', 'stored.pdf', 1, 'application/pdf', 'user-1')`,
	} {
		if _, err := db.Exec(q, a.ID); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// ゴミ箱にない記事は消さない。
	if _, err := svc.HardDelete(tr.ID); !errors.Is(err, ErrNotInTrash) {
		t.Errorf("hard delete a draft: err = %v, want ErrNotInTrash", err)
	}
	res, err := svc.HardDelete(a.ID)
	if err != nil {
		t.Fatalf("HardDelete: %v", err)
	}
	if !slices.Equal(res.IDs, []string{a.ID}) || !slices.Equal(res.AttachmentFiles, []string{"stored.pdf"}) {
		t.Errorf("result = %+v", res)
	}
	if _, err := svc.GetByID(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID after hard delete: err = %v, want ErrNotFound", err)
	}
	for _, table := range []string{"article_revisions", "article_comments", "attachments"} {
		if n := countRows(t, db, table, a.ID); n != 0 {
			t.Errorf("%s rows = %d, want 0", table, n)
		}
	}
	got, err := svc.GetByID(tr.ID)
	if err != nil {
		t.Fatalf("translation was removed: %v", err)
	}
	if got.SourceArticleID.Valid {
		t.Errorf("source_article_id = %q, want NULL", got.SourceArticleID.String)
	}
	if _, err := svc.HardDelete(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second hard delete: err = %v, want ErrNotFound", err)
	}
}

func TestEmptyTrash(t *testing.T) {
	svc, _ := newTestService(t)
	a := trashArticle(t, svc, "ゴミ1", "trash-1")
	b := trashArticle(t, svc, "ゴミ2", "trash-2")
	keep := newArticle("残す", "keep", "本文")
	if err := svc.Save(keep); err != nil {
		t.Fatalf("Save: %v", err)
	}
	res, err := svc.EmptyTrash()
	if err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	if !slices.Equal(res.IDs, want) {
		t.Errorf("IDs = %v, want %v", res.IDs, want)
	}
	if _, err := svc.GetByID(keep.ID); err != nil {
		t.Errorf("article outside the trash was removed: %v", err)
	}
	if res, err := svc.EmptyTrash(); err != nil || len(res.IDs) != 0 {
		t.Errorf("empty trash again = %+v, %v", res, err)
	}
}

// ゴミ箱に入れてから30日たった記事だけを消し、新しいゴミ箱の記事とゴミ箱の外の記事は残す。
func TestPurgeExpiredTrash(t *testing.T) {
	svc, db := newTestService(t)
	old := trashArticle(t, svc, "古いゴミ", "old-trash")
	fresh := trashArticle(t, svc, "新しいゴミ", "fresh-trash")
	oldDraft := newArticle("古い下書き", "old-draft", "本文")
	if err := svc.Save(oldDraft); err != nil {
		t.Fatalf("Save: %v", err)
	}
	past := time.Now().UTC().Add(-TrashRetention - time.Hour)
	for _, id := range []string{old.ID, oldDraft.ID} {
		if _, err := db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, past, id); err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.PurgeExpiredTrash(TrashRetention)
	if err != nil {
		t.Fatalf("PurgeExpiredTrash: %v", err)
	}
	if !slices.Equal(res.IDs, []string{old.ID}) {
		t.Errorf("IDs = %v, want [%s]", res.IDs, old.ID)
	}
	for _, id := range []string{fresh.ID, oldDraft.ID} {
		if _, err := svc.GetByID(id); err != nil {
			t.Errorf("article %s was removed: %v", id, err)
		}
	}
}
