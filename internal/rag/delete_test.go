package rag

import (
	"context"
	"testing"
)

// DeleteArticle はその記事の埋め込みだけを消し、Embeddings API は呼ばない。埋め込みが無い記事でも成功する。
func TestDeleteArticleRemovesOnlyThatArticle(t *testing.T) {
	db, f, s := searchFixture(t)
	calls := f.callCount()
	if err := s.DeleteArticle(context.Background(), "physics"); err != nil {
		t.Fatalf("DeleteArticle: %v", err)
	}
	if n := len(storedRows(t, db, "physics")); n != 0 {
		t.Errorf("stored %d chunks after DeleteArticle, want 0", n)
	}
	if n := len(storedRows(t, db, "quantum")); n == 0 {
		t.Error("another article's chunks were removed")
	}
	if err := s.DeleteArticle(context.Background(), "physics"); err != nil {
		t.Errorf("DeleteArticle without chunks: %v", err)
	}
	if err := NewService(db, nil).DeleteArticle(context.Background(), "quantum"); err != nil {
		t.Errorf("DeleteArticle without an embedder: %v", err)
	}
	if f.callCount() != calls {
		t.Errorf("Embed calls = %d, want %d (DeleteArticle must not call the API)", f.callCount(), calls)
	}
}
