package article

import (
	"errors"
	"testing"
	"time"
)

// SetCheckout は編集中の印に PC の識別子（clientID）を残し、ClearCheckout は名前・識別子・日時をすべて外す。
// どちらも updated_at は変えない。
func TestSetCheckoutStoresClientID(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("編集中の記事", "checkout-client", "本文"))
	updated := mustGet(t, s, a.ID).UpdatedAt
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)

	if err := s.SetCheckout(t.Context(), a.ID, " 佐藤 ", " pc-a ", at); err != nil {
		t.Fatalf("SetCheckout: %v", err)
	}
	got := mustGet(t, s, a.ID)
	if got.CheckedOutBy != "佐藤" || got.CheckedOutClient != "pc-a" || got.CheckedOutAt == nil || !got.CheckedOutAt.Equal(at) {
		t.Errorf("after SetCheckout: by %q client %q at %v, want 佐藤 pc-a %v", got.CheckedOutBy, got.CheckedOutClient, got.CheckedOutAt, at)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Errorf("SetCheckout changed updated_at: %v -> %v", updated, got.UpdatedAt)
	}

	// 同じ人が別の PC で付け直すと、識別子もその PC のものになる。識別子を送らない古い PC なら空になる。
	if err := s.SetCheckout(t.Context(), a.ID, "佐藤", "pc-b", at); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, a.ID); got.CheckedOutClient != "pc-b" {
		t.Errorf("client after re-checkout = %q, want pc-b", got.CheckedOutClient)
	}
	if err := s.SetCheckout(t.Context(), a.ID, "佐藤", "", at); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, a.ID); got.CheckedOutClient != "" || got.CheckedOutBy != "佐藤" {
		t.Errorf("legacy checkout = by %q client %q, want 佐藤 and empty client", got.CheckedOutBy, got.CheckedOutClient)
	}

	if err := s.SetCheckout(t.Context(), a.ID, "佐藤", "pc-a", at); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearCheckout(t.Context(), a.ID); err != nil {
		t.Fatalf("ClearCheckout: %v", err)
	}
	got = mustGet(t, s, a.ID)
	if got.CheckedOutBy != "" || got.CheckedOutClient != "" || got.CheckedOutAt != nil {
		t.Errorf("after ClearCheckout: by %q client %q at %v, want all empty", got.CheckedOutBy, got.CheckedOutClient, got.CheckedOutAt)
	}

	if err := s.SetCheckout(t.Context(), a.ID, "  ", "pc-a", at); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetCheckout with empty name = %v, want ErrInvalid", err)
	}
	if err := s.SetCheckout(t.Context(), "missing", "佐藤", "pc-a", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetCheckout missing article = %v, want ErrNotFound", err)
	}
}

// CheckoutState は編集中の印だけを読み、記事が無ければ ErrNotFound を返す。
func TestCheckoutState(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("状態", "checkout-state", "本文"))

	by, client, at, err := s.CheckoutState(t.Context(), a.ID)
	if err != nil || by != "" || client != "" || at != nil {
		t.Fatalf("initial = %q %q %v %v, want empty", by, client, at, err)
	}
	when := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	if err := s.SetCheckout(t.Context(), a.ID, "佐藤", "pc-a", when); err != nil {
		t.Fatal(err)
	}
	by, client, at, err = s.CheckoutState(t.Context(), a.ID)
	if err != nil || by != "佐藤" || client != "pc-a" || at == nil || !at.Equal(when) {
		t.Errorf("state = %q %q %v %v, want 佐藤 pc-a %v", by, client, at, err, when)
	}
	if _, _, _, err := s.CheckoutState(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v, want ErrNotFound", err)
	}
}

// Save は編集中の印と remote_updated_at を書き換えない。
func TestSaveKeepsSyncState(t *testing.T) {
	s, _ := newTestService(t)
	a := mustSave(t, s, newArticle("保存", "save-keeps-sync", "本文"))
	when := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	remote := when.Add(-time.Hour)
	if err := s.SetCheckout(t.Context(), a.ID, "佐藤", "pc-a", when); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRemoteUpdatedAt(t.Context(), a.ID, remote); err != nil {
		t.Fatal(err)
	}

	// 印を持たない古い値の記事で保存しても、DB 上の印は残る。
	stale := mustGet(t, s, a.ID)
	stale.CheckedOutBy, stale.CheckedOutClient, stale.CheckedOutAt, stale.RemoteUpdatedAt = "", "", nil, nil
	stale.Title = "保存後"
	if err := s.Save(stale); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := mustGet(t, s, a.ID)
	if got.Title != "保存後" {
		t.Errorf("title = %q, want 保存後", got.Title)
	}
	if got.CheckedOutBy != "佐藤" || got.CheckedOutClient != "pc-a" || got.CheckedOutAt == nil || !got.CheckedOutAt.Equal(when) {
		t.Errorf("checkout clobbered: %q %q %v", got.CheckedOutBy, got.CheckedOutClient, got.CheckedOutAt)
	}
	if got.RemoteUpdatedAt == nil || !got.RemoteUpdatedAt.Equal(remote) {
		t.Errorf("remote_updated_at = %v, want %v", got.RemoteUpdatedAt, remote)
	}
}
