package article

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func newPage(title, slug string) *Article {
	a := newArticle(title, slug, "本文")
	a.Kind = KindPage
	return a
}

func newChildPage(title, slug, parentID string, sortOrder int) *Article {
	a := newPage(title, slug)
	a.ParentID = parentID
	a.SortOrder = sortOrder
	return a
}

func publishedPage(title, slug, parentID string, sortOrder int, publishedAt time.Time) *Article {
	a := newChildPage(title, slug, parentID, sortOrder)
	a.Status = StatusPublished
	a.PublishedAt = &publishedAt
	return a
}

// 親の検証で拒否されたことを確かめる（ErrInvalid と ErrInvalidParent の両方を兼ねる）。
func wantInvalidParent(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, ErrInvalidParent) {
		t.Fatalf("err = %v, want ErrInvalid and ErrInvalidParent", err)
	}
}

func TestSave_PageHierarchyRoundTrip(t *testing.T) {
	s, db := newTestService(t)
	parent := mustSave(t, s, newPage("研究紹介", "research"))
	child := mustSave(t, s, newChildPage("メンバー", "members", "  "+parent.ID+" ", 3))

	if child.ParentID != parent.ID || child.SortOrder != 3 {
		t.Errorf("saved child ParentID = %q, SortOrder = %d, want %q and 3", child.ParentID, child.SortOrder, parent.ID)
	}
	got := mustGet(t, s, child.ID)
	if got.ParentID != parent.ID || got.SortOrder != 3 {
		t.Errorf("GetByID ParentID = %q, SortOrder = %d, want %q and 3", got.ParentID, got.SortOrder, parent.ID)
	}
	bySlug, err := s.GetBySlug("members")
	if err != nil || bySlug.ParentID != parent.ID {
		t.Fatalf("GetBySlug = %+v, %v, want ParentID %q", bySlug, err, parent.ID)
	}

	// 親の無いページ・記事は NULL で保存し、空文字として読む（既存の行と同じ形）。
	p := mustGet(t, s, parent.ID)
	if p.ParentID != "" || p.SortOrder != 0 {
		t.Errorf("top-level page ParentID = %q, SortOrder = %d, want empty and 0", p.ParentID, p.SortOrder)
	}
	var isNull bool
	if err := db.Get(&isNull, `SELECT parent_id IS NULL FROM articles WHERE id = ?`, parent.ID); err != nil || !isNull {
		t.Errorf("top-level parent_id IS NULL = %v (%v), want true", isNull, err)
	}
	post := mustSave(t, s, newArticle("お知らせ", "news-1", "x"))
	if err := db.Get(&isNull, `SELECT parent_id IS NULL FROM articles WHERE id = ?`, post.ID); err != nil || !isNull {
		t.Errorf("post parent_id IS NULL = %v (%v), want true", isNull, err)
	}

	// 親・並び順を指定せずに保存し直しても（読み直した記事を変えて保存する編集画面と同じ）、階層は保たれる。
	got.Title = "メンバー紹介"
	mustSave(t, s, got)
	again := mustGet(t, s, child.ID)
	if again.ParentID != parent.ID || again.SortOrder != 3 {
		t.Errorf("after re-save ParentID = %q, SortOrder = %d, want %q and 3", again.ParentID, again.SortOrder, parent.ID)
	}

	// 親を外すと最上位に戻り、NULL になる。
	again.ParentID = ""
	mustSave(t, s, again)
	if err := db.Get(&isNull, `SELECT parent_id IS NULL FROM articles WHERE id = ?`, child.ID); err != nil || !isNull {
		t.Errorf("detached parent_id IS NULL = %v (%v), want true", isNull, err)
	}
}

// 既存の行（parent_id が NULL のまま）を読み書きしても壊れないこと。
func TestSave_LegacyRowsWithoutHierarchy(t *testing.T) {
	s, db := newTestService(t)
	if _, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id, status, kind)
		VALUES ('legacy-page', '旧ページ', 'legacy', ?, ?, 'draft', 'page')`, testAuthorID, testCategoryID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	a := mustGet(t, s, "legacy-page")
	if a.ParentID != "" || a.SortOrder != 0 {
		t.Fatalf("legacy row ParentID = %q, SortOrder = %d, want empty and 0", a.ParentID, a.SortOrder)
	}
	a.Title = "旧ページ（改）"
	mustSave(t, s, a)
	child := mustSave(t, s, newChildPage("子", "child", "legacy-page", 0))
	if child.ParentID != "legacy-page" {
		t.Errorf("child ParentID = %q, want legacy-page", child.ParentID)
	}
}

func TestSave_PostCannotHaveParent(t *testing.T) {
	s, _ := newTestService(t)
	parent := mustSave(t, s, newPage("親", "parent"))

	post := newArticle("記事", "post", "x")
	post.ParentID = parent.ID
	wantInvalidParent(t, s.Save(post))
	if post.ID != "" {
		t.Errorf("failed Save changed the article: ID = %q", post.ID)
	}

	// 種類を省略すると記事（post）とみなすため、やはり親は持てない。
	post.Kind = ""
	wantInvalidParent(t, s.Save(post))

	// ページを記事に変えるときも、親を外さなければ保存できない。
	child := mustSave(t, s, newChildPage("子", "child", parent.ID, 0))
	child.Kind = KindPost
	wantInvalidParent(t, s.Save(child))
	child.ParentID = ""
	mustSave(t, s, child)
}

func TestSave_ParentMustBeExistingPage(t *testing.T) {
	s, _ := newTestService(t)
	post := mustSave(t, s, newArticle("記事", "post", "x"))
	trashed := mustSave(t, s, newPage("ゴミ箱のページ", "trashed"))
	if err := s.SoftDelete(trashed.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	for name, parentID := range map[string]string{
		"missing": "no-such-article",
		"post":    post.ID,
		"trashed": trashed.ID,
	} {
		t.Run(name, func(t *testing.T) {
			wantInvalidParent(t, s.Save(newChildPage("子", "child-"+name, parentID, 0)))
			if _, err := s.GetBySlug("child-" + name); !errors.Is(err, ErrNotFound) {
				t.Errorf("rejected child was stored: %v", err)
			}
		})
	}

	// Create も同じ検証を行う。
	c := newChildPage("子", "child-create", post.ID, 0)
	c.ID = "child-create-id"
	wantInvalidParent(t, s.Create(c))
}

// 下書き・校閲待ちのページも親にできる（公開の順序は問わない）。
func TestSave_ParentMayBeUnpublishedPage(t *testing.T) {
	s, _ := newTestService(t)
	draft := mustSave(t, s, newPage("下書きの親", "draft-parent"))
	review := newPage("校閲中の親", "review-parent")
	review.Status = StatusReview
	mustSave(t, s, review)
	mustSave(t, s, newChildPage("子1", "c1", draft.ID, 0))
	mustSave(t, s, newChildPage("子2", "c2", review.ID, 0))
}

func TestSave_RejectsSelfParent(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newPage("自分", "self"))
	a.ParentID = a.ID
	wantInvalidParent(t, s.Save(a))
	if got := mustGet(t, s, a.ID); got.ParentID != "" {
		t.Errorf("stored ParentID = %q, want empty", got.ParentID)
	}

	// 呼び出し側が ID を決めて作るときも、自分自身は親にできない。
	b := newChildPage("自分2", "self-2", "fixed-id", 0)
	b.ID = "fixed-id"
	wantInvalidParent(t, s.Save(b))
	wantInvalidParent(t, s.Create(b))
}

func TestSave_RejectsCycles(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newPage("A", "a"))
	b := mustSave(t, s, newChildPage("B", "b", a.ID, 0))
	c := mustSave(t, s, newChildPage("C", "c", b.ID, 0))

	// A→B→A（2段）と A→C→B→A（3段）の循環。
	for _, parent := range []*Article{b, c} {
		cur := mustGet(t, s, a.ID)
		cur.ParentID = parent.ID
		wantInvalidParent(t, s.Save(cur))
	}
	if got := mustGet(t, s, a.ID); got.ParentID != "" {
		t.Errorf("A ParentID = %q after rejected saves, want empty", got.ParentID)
	}

	// 循環にならない付け替え（C を A の直下へ、B を最上位へ）はできる。
	c = mustGet(t, s, c.ID)
	c.ParentID = a.ID
	mustSave(t, s, c)
	b = mustGet(t, s, b.ID)
	b.ParentID = ""
	mustSave(t, s, b)
	// B は A の子孫ではなくなったため、A を B の下に置ける。
	a = mustGet(t, s, a.ID)
	a.ParentID = b.ID
	mustSave(t, s, a)
}

// 親を変えない保存では親を検証し直さない。親をゴミ箱に入れても、子ページの編集は続けられる。
func TestSave_UnchangedParentIsNotRevalidated(t *testing.T) {
	s, _ := newTestService(t)
	parent := mustSave(t, s, newPage("親", "parent"))
	child := mustSave(t, s, newChildPage("子", "child", parent.ID, 1))
	if err := s.SoftDelete(parent.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	child = mustGet(t, s, child.ID)
	child.ContentMarkdown = "更新"
	child.SortOrder = 5
	mustSave(t, s, child)
	if got := mustGet(t, s, child.ID); got.ParentID != parent.ID || got.SortOrder != 5 {
		t.Errorf("ParentID = %q, SortOrder = %d, want %q and 5", got.ParentID, got.SortOrder, parent.ID)
	}

	// ゴミ箱のページへ新たに付け替えることはできない。
	other := mustSave(t, s, newPage("別のページ", "other"))
	other.ParentID = parent.ID
	wantInvalidParent(t, s.Save(other))
}

func TestSave_PageWithChildrenCannotBecomePost(t *testing.T) {
	s, _ := newTestService(t)
	parent := mustSave(t, s, newPage("親", "parent"))
	child := mustSave(t, s, newChildPage("子", "child", parent.ID, 0))

	parent.Kind = KindPost
	err := s.Save(parent)
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, ErrHasChildPages) {
		t.Fatalf("Save(page with children -> post) = %v, want ErrInvalid and ErrHasChildPages", err)
	}
	if got := mustGet(t, s, parent.ID); got.Kind != KindPage {
		t.Errorf("Kind = %q, want page", got.Kind)
	}

	// 子がゴミ箱にしか無ければ記事にでき、ゴミ箱の子は親から外す（戻しても記事の下に入らない）。
	if err := s.SoftDelete(child.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	parent = mustGet(t, s, parent.ID)
	parent.Kind = KindPost
	mustSave(t, s, parent)
	if got := mustGet(t, s, child.ID); got.ParentID != "" {
		t.Errorf("trashed child ParentID = %q, want empty", got.ParentID)
	}
}

func TestSave_SortOrderDefaultsToZero(t *testing.T) {
	s, _ := newTestService(t)
	for _, a := range []*Article{newArticle("記事", "p", "x"), newPage("ページ", "pg")} {
		mustSave(t, s, a)
		if got := mustGet(t, s, a.ID); got.SortOrder != 0 {
			t.Errorf("%s SortOrder = %d, want 0", a.Kind, got.SortOrder)
		}
	}
	neg := newPage("先頭", "first")
	neg.SortOrder = -1
	mustSave(t, s, neg)
	if got := mustGet(t, s, neg.ID); got.SortOrder != -1 {
		t.Errorf("SortOrder = %d, want -1", got.SortOrder)
	}
}

func TestListChildPages(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)

	root := mustSave(t, s, publishedPage("研究室", "lab", "", 0, past))
	other := mustSave(t, s, publishedPage("別の親", "other", "", 1, past))

	// 並び順: sort_order、タイトル、作成日時の順。
	b := publishedPage("B", "b", root.ID, 2, past)
	z := publishedPage("Z", "z", root.ID, 1, past)
	a1 := publishedPage("A", "a1", root.ID, 2, past)
	a1.CreatedAt = past.Add(time.Hour)
	a2 := publishedPage("A", "a2", root.ID, 2, past)
	a2.CreatedAt = past.Add(2 * time.Hour)
	for _, p := range []*Article{b, a2, z, a1} {
		mustSave(t, s, p)
	}

	// 読者に見えない子（下書き・予約中・ゴミ箱）と、別の親の子は含めない。
	mustSave(t, s, newChildPage("下書き", "draft-child", root.ID, 0))
	mustSave(t, s, publishedPage("予約", "scheduled-child", root.ID, 0, now.Add(time.Hour)))
	gone := mustSave(t, s, publishedPage("削除", "deleted-child", root.ID, 0, past))
	if err := s.SoftDelete(gone.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	mustSave(t, s, publishedPage("よその子", "other-child", other.ID, 0, past))

	children, err := s.ListChildPages(ctx, root.ID, now)
	if err != nil {
		t.Fatalf("ListChildPages: %v", err)
	}
	if got, want := ids(children), []string{z.ID, a1.ID, a2.ID, b.ID}; !slices.Equal(got, want) {
		t.Errorf("children = %v, want %v (Z, A(older), A(newer), B)", got, want)
	}
	if len(children) > 0 && (children[0].ContentHTML == "" || children[0].ParentID != root.ID) {
		t.Errorf("child row is incomplete: %+v", children[0])
	}

	// 空の parentID は最上位の公開ページ（記事は含めない）。
	pub := newArticle("公開記事", "post", "x")
	pub.Status, pub.PublishedAt = StatusPublished, &past
	mustSave(t, s, pub)
	top, err := s.ListChildPages(ctx, "", now)
	if err != nil {
		t.Fatalf("ListChildPages(top): %v", err)
	}
	if got, want := ids(top), []string{root.ID, other.ID}; !slices.Equal(got, want) {
		t.Errorf("top-level pages = %v, want %v", got, want)
	}

	// 子の無いページ・存在しないページは空スライス（nil ではない）。
	none, err := s.ListChildPages(ctx, "no-such-page", now)
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("ListChildPages(unknown) = %v, %v, want empty slice", none, err)
	}
}

func TestListVisiblePages(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	root := mustSave(t, s, publishedPage("研究室", "lab", "", 1, past))
	first := mustSave(t, s, publishedPage("トップ", "top", "", 0, past))
	child := mustSave(t, s, publishedPage("メンバー", "members", root.ID, 0, past))
	mustSave(t, s, newPage("下書き", "draft"))
	mustSave(t, s, publishedPage("予約", "later", "", 0, now.Add(time.Hour)))
	post := newArticle("公開記事", "post", "x")
	post.Status, post.PublishedAt = StatusPublished, &past
	mustSave(t, s, post)

	pages, err := s.ListVisiblePages(ctx, now)
	if err != nil {
		t.Fatalf("ListVisiblePages: %v", err)
	}
	// 親子を問わず sort_order・タイトル・作成日時の順。木は呼び出し側が ParentID で組み立てる。
	// sort_order 0 の「トップ」「メンバー」はタイトル（UTF-8 のバイト順）で並ぶ。
	if got, want := ids(pages), []string{first.ID, child.ID, root.ID}; !slices.Equal(got, want) {
		t.Errorf("pages = %v, want %v", got, want)
	}
	for _, p := range pages {
		if p.ID == child.ID && p.ParentID != root.ID {
			t.Errorf("child ParentID = %q, want %q", p.ParentID, root.ID)
		}
	}
}

func TestHardDelete_DetachesChildPages(t *testing.T) {
	s, _ := newTestService(t)
	parent := mustSave(t, s, newPage("親", "parent"))
	child := mustSave(t, s, newChildPage("子", "child", parent.ID, 2))
	if err := s.SoftDelete(parent.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := s.HardDelete(parent.ID); err != nil {
		t.Fatalf("HardDelete: %v", err)
	}
	got := mustGet(t, s, child.ID)
	if got.ParentID != "" || got.SortOrder != 2 {
		t.Errorf("orphan ParentID = %q, SortOrder = %d, want empty and 2", got.ParentID, got.SortOrder)
	}
	// 親が消えた子も、そのまま保存し直せる。
	got.Title = "子（改）"
	mustSave(t, s, got)
}
