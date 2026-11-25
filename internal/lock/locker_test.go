package lock

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/database"
)

const (
	testArticleID = "article-1"
	userA         = "user-a"
	userB         = "user-b"
	userC         = "user-c"
	adminID       = "admin-1"
	testTTL       = 2 * time.Minute
)

// openDB はユーザー（A は表示名あり、B は表示名なし）・カテゴリ・記事を登録した、
// マイグレーション済みのDBを返す。
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
	stmts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, 'yamada', '山田', 'hash', 'writer')`, []any{userA}},
		{`INSERT INTO users (id, username, password_hash, role) VALUES (?, 'suzuki', 'hash', 'writer')`, []any{userB}},
		{`INSERT INTO users (id, username, password_hash, role) VALUES (?, 'tanaka', 'hash', 'editor')`, []any{userC}},
		{`INSERT INTO users (id, username, password_hash, role) VALUES (?, 'admin', 'hash', 'admin')`, []any{adminID}},
		{`INSERT INTO categories (id, name, slug) VALUES (1, 'News', 'news')`, nil},
		{`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, 'T', 't', ?, 1)`, []any{testArticleID, userA}},
	}
	for _, s := range stmts {
		if _, err := db.Exec(s.query, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.query, err)
		}
	}
	return db
}

// fakeClock はテストから進められる時計。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestLocker はインメモリDBと固定時計を使う Locker を返す。
func newTestLocker(t *testing.T) (*Locker, *fakeClock, *sqlx.DB) {
	t.Helper()
	db := openDB(t, ":memory:")
	clock := &fakeClock{t: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)}
	l := NewLocker(db, testTTL)
	l.now = clock.Now
	return l, clock, db
}

func mustAcquire(t *testing.T, l *Locker, articleID, userID string) *LockInfo {
	t.Helper()
	info, err := l.Acquire(context.Background(), articleID, userID)
	if err != nil {
		t.Fatalf("Acquire(%s, %s): %v", articleID, userID, err)
	}
	return info
}

// assertHeldBy は記事が holder にロックされていることを、DB の列と Check の両方で確認する。
func assertHeldBy(t *testing.T, l *Locker, db *sqlx.DB, holder string) {
	t.Helper()
	var lockedBy *string
	if err := db.Get(&lockedBy, `SELECT locked_by FROM articles WHERE id = ?`, testArticleID); err != nil {
		t.Fatalf("select locked_by: %v", err)
	}
	if lockedBy == nil || *lockedBy != holder {
		t.Fatalf("locked_by = %v, want %q", lockedBy, holder)
	}
	info, err := l.Check(context.Background(), testArticleID, holder)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.IsLocked || !info.IsOwnLock || info.UserID != holder {
		t.Fatalf("Check = %+v, want locked by %q", info, holder)
	}
}

// assertUnlocked は記事のロック列が NULL であることを確認する。
func assertUnlocked(t *testing.T, db *sqlx.DB) {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM articles WHERE id = ? AND locked_by IS NULL AND locked_at IS NULL`, testArticleID); err != nil {
		t.Fatalf("select lock: %v", err)
	}
	if n != 1 {
		t.Fatal("article is still locked in DB")
	}
}

func TestNewLocker_DefaultTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		if got := NewLocker(nil, ttl).ttl; got != DefaultTTL {
			t.Errorf("NewLocker(ttl=%v).ttl = %v, want %v", ttl, got, DefaultTTL)
		}
	}
	if DefaultTTL != 2*time.Minute {
		t.Errorf("DefaultTTL = %v, want 2m", DefaultTTL)
	}
}

func TestAcquire_FreeArticle(t *testing.T) {
	l, clock, db := newTestLocker(t)
	info := mustAcquire(t, l, testArticleID, userA)
	want := LockInfo{
		ArticleID: testArticleID, UserID: userA, UserName: "山田",
		LockedAt: clock.Now(), IsLocked: true, IsOwnLock: true,
	}
	if *info != want {
		t.Fatalf("Acquire = %+v, want %+v", *info, want)
	}
	assertHeldBy(t, l, db, userA)

	var lockedAt time.Time
	if err := db.Get(&lockedAt, `SELECT locked_at FROM articles WHERE id = ?`, testArticleID); err != nil {
		t.Fatalf("select locked_at: %v", err)
	}
	if !lockedAt.Equal(clock.Now()) {
		t.Fatalf("locked_at = %v, want %v", lockedAt, clock.Now())
	}
}

func TestAcquire_OtherUserGetsAlreadyLockedWithHolderInfo(t *testing.T) {
	l, clock, db := newTestLocker(t)
	lockedAt := clock.Now()
	mustAcquire(t, l, testArticleID, userA)

	clock.Advance(testTTL - time.Millisecond)
	info, err := l.Acquire(context.Background(), testArticleID, userB)
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("Acquire by B err = %v, want ErrAlreadyLocked", err)
	}
	want := LockInfo{
		ArticleID: testArticleID, UserID: userA, UserName: "山田",
		LockedAt: lockedAt, IsLocked: true, IsOwnLock: false,
	}
	if info == nil || *info != want {
		t.Fatalf("Acquire by B info = %+v, want %+v", info, want)
	}
	assertHeldBy(t, l, db, userA)
}

func TestAcquire_SameUserRefreshesLock(t *testing.T) {
	l, clock, _ := newTestLocker(t)
	mustAcquire(t, l, testArticleID, userA)

	clock.Advance(time.Minute)
	info := mustAcquire(t, l, testArticleID, userA)
	if !info.LockedAt.Equal(clock.Now()) {
		t.Fatalf("re-Acquire LockedAt = %v, want %v", info.LockedAt, clock.Now())
	}

	// 再獲得から TTL 未満なので、最初の獲得から TTL を過ぎても B は獲得できない。
	clock.Advance(testTTL - time.Millisecond)
	if _, err := l.Acquire(context.Background(), testArticleID, userB); !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("Acquire by B err = %v, want ErrAlreadyLocked", err)
	}
}

func TestAcquire_UserNameFallsBackToUsername(t *testing.T) {
	l, _, _ := newTestLocker(t)
	info := mustAcquire(t, l, testArticleID, userB)
	if info.UserName != "suzuki" {
		t.Fatalf("UserName = %q, want username %q", info.UserName, "suzuki")
	}
	_, err := l.Acquire(context.Background(), testArticleID, userA)
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("Acquire by A err = %v, want ErrAlreadyLocked", err)
	}
	checked, err := l.Check(context.Background(), testArticleID, userA)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if checked.UserName != "suzuki" {
		t.Fatalf("Check UserName = %q, want %q", checked.UserName, "suzuki")
	}
}

func TestAcquire_ExpiredLockIsTakenOver(t *testing.T) {
	l, clock, db := newTestLocker(t)
	mustAcquire(t, l, testArticleID, userA)

	clock.Advance(testTTL)
	info := mustAcquire(t, l, testArticleID, userB)
	if info.UserID != userB || !info.IsOwnLock {
		t.Fatalf("Acquire by B = %+v, want own lock", info)
	}
	assertHeldBy(t, l, db, userB)
}

func TestAcquire_Errors(t *testing.T) {
	l, _, db := newTestLocker(t)
	if _, err := l.Acquire(context.Background(), "missing", userA); !errors.Is(err, ErrArticleNotFound) {
		t.Errorf("Acquire(missing article) err = %v, want ErrArticleNotFound", err)
	}
	if _, err := l.Acquire(context.Background(), testArticleID, "ghost"); err == nil {
		t.Error("Acquire(unknown user) succeeded, want error")
	}
	assertUnlocked(t, db)
}

func TestRefresh_ExtendsExpiry(t *testing.T) {
	l, clock, db := newTestLocker(t)
	mustAcquire(t, l, testArticleID, userA)

	clock.Advance(testTTL - time.Second)
	if err := l.Refresh(context.Background(), testArticleID, userA); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	refreshedAt := clock.Now()

	// 獲得からは TTL を過ぎたが、ハートビートからは TTL 未満なのでロックは有効。
	clock.Advance(testTTL - time.Second)
	info, err := l.Acquire(context.Background(), testArticleID, userB)
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("Acquire by B after refresh err = %v, want ErrAlreadyLocked", err)
	}
	if !info.LockedAt.Equal(refreshedAt) {
		t.Fatalf("LockedAt = %v, want refreshed time %v", info.LockedAt, refreshedAt)
	}
	assertHeldBy(t, l, db, userA)

	// ハートビートが途絶えて TTL を過ぎれば失効する。
	clock.Advance(time.Second)
	mustAcquire(t, l, testArticleID, userB)
}

func TestRefresh_NotHolder(t *testing.T) {
	l, clock, db := newTestLocker(t)
	ctx := context.Background()

	if err := l.Refresh(ctx, testArticleID, userA); !errors.Is(err, ErrNotLockHolder) {
		t.Errorf("Refresh(unlocked) err = %v, want ErrNotLockHolder", err)
	}
	assertUnlocked(t, db)

	mustAcquire(t, l, testArticleID, userA)
	if err := l.Refresh(ctx, testArticleID, userB); !errors.Is(err, ErrNotLockHolder) {
		t.Errorf("Refresh by B err = %v, want ErrNotLockHolder", err)
	}

	// 失効したロックはハートビートでは延長できない（Acquire で獲得し直す）。
	clock.Advance(testTTL)
	if err := l.Refresh(ctx, testArticleID, userA); !errors.Is(err, ErrNotLockHolder) {
		t.Errorf("Refresh(expired) err = %v, want ErrNotLockHolder", err)
	}
	mustAcquire(t, l, testArticleID, userB)

	if err := l.Refresh(ctx, "missing", userA); !errors.Is(err, ErrArticleNotFound) {
		t.Errorf("Refresh(missing) err = %v, want ErrArticleNotFound", err)
	}
}

func TestRelease_LetsOtherUserAcquireImmediately(t *testing.T) {
	l, _, db := newTestLocker(t)
	mustAcquire(t, l, testArticleID, userA)

	if err := l.Release(context.Background(), testArticleID, userA); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertUnlocked(t, db)
	info, err := l.Check(context.Background(), testArticleID, userB)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.IsLocked {
		t.Fatalf("Check after release = %+v, want unlocked", info)
	}
	mustAcquire(t, l, testArticleID, userB)
	assertHeldBy(t, l, db, userB)
}

func TestRelease_NotHolder(t *testing.T) {
	l, clock, db := newTestLocker(t)
	ctx := context.Background()
	mustAcquire(t, l, testArticleID, userA)

	if err := l.Release(ctx, testArticleID, userB); !errors.Is(err, ErrNotLockHolder) {
		t.Fatalf("Release by B err = %v, want ErrNotLockHolder", err)
	}
	assertHeldBy(t, l, db, userA)

	// 失効していても他人のロックは解放できない。
	clock.Advance(testTTL)
	if err := l.Release(ctx, testArticleID, userB); !errors.Is(err, ErrNotLockHolder) {
		t.Fatalf("Release(expired) by B err = %v, want ErrNotLockHolder", err)
	}

	if err := l.Release(ctx, "missing", userA); !errors.Is(err, ErrArticleNotFound) {
		t.Errorf("Release(missing) err = %v, want ErrArticleNotFound", err)
	}
}

func TestRelease_ExpiredOwnLockAndIdempotent(t *testing.T) {
	l, clock, db := newTestLocker(t)
	ctx := context.Background()
	mustAcquire(t, l, testArticleID, userA)

	clock.Advance(testTTL)
	if err := l.Release(ctx, testArticleID, userA); err != nil {
		t.Fatalf("Release(expired own lock): %v", err)
	}
	assertUnlocked(t, db)
	// sendBeacon の重複送信など、ロックが無い状態での解放は成功する。
	if err := l.Release(ctx, testArticleID, userA); err != nil {
		t.Fatalf("Release(unlocked): %v", err)
	}
}

func TestForceUnlock(t *testing.T) {
	l, _, db := newTestLocker(t)
	ctx := context.Background()
	mustAcquire(t, l, testArticleID, userA)

	if err := l.ForceUnlock(ctx, testArticleID); err != nil {
		t.Fatalf("ForceUnlock: %v", err)
	}
	assertUnlocked(t, db)
	mustAcquire(t, l, testArticleID, adminID)
	assertHeldBy(t, l, db, adminID)

	// 強制解除されたユーザーのハートビートは失敗する。
	if err := l.Refresh(ctx, testArticleID, userA); !errors.Is(err, ErrNotLockHolder) {
		t.Fatalf("Refresh by A after force unlock err = %v, want ErrNotLockHolder", err)
	}

	if err := l.ForceUnlock(ctx, testArticleID); err != nil {
		t.Fatalf("ForceUnlock(again): %v", err)
	}
	if err := l.ForceUnlock(ctx, testArticleID); err != nil {
		t.Fatalf("ForceUnlock(unlocked): %v", err)
	}
	if err := l.ForceUnlock(ctx, "missing"); !errors.Is(err, ErrArticleNotFound) {
		t.Fatalf("ForceUnlock(missing) err = %v, want ErrArticleNotFound", err)
	}
}

func TestCheck(t *testing.T) {
	l, clock, _ := newTestLocker(t)
	ctx := context.Background()

	info, err := l.Check(ctx, testArticleID, userA)
	if err != nil {
		t.Fatalf("Check(unlocked): %v", err)
	}
	if *info != (LockInfo{ArticleID: testArticleID}) {
		t.Fatalf("Check(unlocked) = %+v, want unlocked", info)
	}

	lockedAt := clock.Now()
	mustAcquire(t, l, testArticleID, userA)
	clock.Advance(time.Minute)

	own, err := l.Check(ctx, testArticleID, userA)
	if err != nil {
		t.Fatalf("Check(own): %v", err)
	}
	wantOwn := LockInfo{
		ArticleID: testArticleID, UserID: userA, UserName: "山田",
		LockedAt: lockedAt, IsLocked: true, IsOwnLock: true,
	}
	if *own != wantOwn {
		t.Fatalf("Check(own) = %+v, want %+v", *own, wantOwn)
	}

	other, err := l.Check(ctx, testArticleID, userB)
	if err != nil {
		t.Fatalf("Check(other): %v", err)
	}
	wantOther := wantOwn
	wantOther.IsOwnLock = false
	if *other != wantOther {
		t.Fatalf("Check(other) = %+v, want %+v", *other, wantOther)
	}

	// TTL を過ぎたロックはロック無しとして扱う。
	clock.Advance(time.Minute)
	expired, err := l.Check(ctx, testArticleID, userA)
	if err != nil {
		t.Fatalf("Check(expired): %v", err)
	}
	if *expired != (LockInfo{ArticleID: testArticleID}) {
		t.Fatalf("Check(expired) = %+v, want unlocked", expired)
	}

	if _, err := l.Check(ctx, "missing", userA); !errors.Is(err, ErrArticleNotFound) {
		t.Fatalf("Check(missing) err = %v, want ErrArticleNotFound", err)
	}
}

func TestCheck_LockHolderDeleted(t *testing.T) {
	l, _, db := newTestLocker(t)
	mustAcquire(t, l, testArticleID, userB)
	// locked_by は ON DELETE SET NULL のため、保持者の削除でロックは外れる。
	if _, err := db.Exec(`DELETE FROM users WHERE id = ?`, userB); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	info, err := l.Check(context.Background(), testArticleID, userA)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.IsLocked {
		t.Fatalf("Check = %+v, want unlocked", info)
	}
	mustAcquire(t, l, testArticleID, userA)
}

// TestRealClock_ZombieLockExpires は実時間の短い TTL で、ハートビートが途絶えた
// ロックが自動失効することを確認する。
func TestRealClock_ZombieLockExpires(t *testing.T) {
	const ttl = 100 * time.Millisecond
	db := openDB(t, filepath.Join(t.TempDir(), "lock.db"))
	l := NewLocker(db, ttl)
	ctx := context.Background()

	start := time.Now()
	mustAcquire(t, l, testArticleID, userA)
	_, err := l.Acquire(ctx, testArticleID, userB)
	if time.Since(start) < ttl && !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("Acquire by B within TTL err = %v, want ErrAlreadyLocked", err)
	}

	time.Sleep(ttl + 50*time.Millisecond)
	info := mustAcquire(t, l, testArticleID, userB)
	if info.UserID != userB {
		t.Fatalf("Acquire by B after TTL = %+v", info)
	}
	if err := l.Refresh(ctx, testArticleID, userA); !errors.Is(err, ErrNotLockHolder) {
		t.Fatalf("Refresh by zombie A err = %v, want ErrNotLockHolder", err)
	}
}

// TestAcquire_ConcurrentOnlyOneWins は複数接続から同時に獲得しても、
// 有効なロックを得るのが1人だけであることを確認する。
func TestAcquire_ConcurrentOnlyOneWins(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "lock.db"))
	const n = 8
	users := make([]string, n)
	for i := range users {
		users[i] = fmt.Sprintf("racer-%d", i)
		if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role) VALUES (?, ?, 'hash', 'writer')`,
			users[i], users[i]); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	l := NewLocker(db, time.Hour)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		start   = make(chan struct{})
	)
	for _, u := range users {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			<-start
			_, err := l.Acquire(context.Background(), testArticleID, u)
			switch {
			case err == nil:
				mu.Lock()
				winners = append(winners, u)
				mu.Unlock()
			case !errors.Is(err, ErrAlreadyLocked):
				t.Errorf("Acquire(%s): %v", u, err)
			}
		}(u)
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly one", winners)
	}
	assertHeldBy(t, l, db, winners[0])
}

func TestCanceledContextLeavesNoOpenTransaction(t *testing.T) {
	l, _, db := newTestLocker(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx, testArticleID, userA); err == nil {
		t.Fatal("Acquire with canceled ctx succeeded, want error")
	}
	// インメモリDBは単一接続のため、トランザクションが残っていれば次の書き込みが失敗する。
	mustAcquire(t, l, testArticleID, userA)
	assertHeldBy(t, l, db, userA)
}
