// Package lock は記事の排他編集ロックを提供する。
// articles.locked_by / locked_at を使い、ハートビートで延長される TTL 付きのロックで
// 複数の人が同じ記事を同時に編集して上書きする事故を防ぐ。
package lock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// DefaultTTL はハートビートが途絶えてからロックが自動失効するまでの既定時間。
const DefaultTTL = 2 * time.Minute

var (
	// ErrAlreadyLocked は他のユーザーが有効期間内のロックを保持していることを示す。
	ErrAlreadyLocked = errors.New("lock: article is currently locked by another user")
	// ErrNotLockHolder はユーザーが記事の有効なロックを保持していないことを示す。
	ErrNotLockHolder = errors.New("lock: user does not hold lock for this article")
	// ErrArticleNotFound は記事が存在しないことを示す。
	ErrArticleNotFound = errors.New("lock: article not found")
)

// LockInfo は記事のロック状況。IsLocked が false のとき UserID・UserName・LockedAt は空。
type LockInfo struct {
	ArticleID string
	UserID    string
	UserName  string
	LockedAt  time.Time
	IsLocked  bool
	IsOwnLock bool
}

// Locker は記事のロックを獲得・延長・解放する。並行に使用してよい。
type Locker struct {
	db  *sqlx.DB
	ttl time.Duration
	// now は現在時刻を返す。テストで時刻を固定するために差し替える。
	now func() time.Time
}

// NewLocker は db を使う Locker を返す。db は database.Migrate 適用済みであること。
// ttl が 0 以下なら DefaultTTL を使う。
func NewLocker(db *sqlx.DB, ttl time.Duration) *Locker {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Locker{db: db, ttl: ttl, now: time.Now}
}

// lockRow は記事のロック列と、ロック保持者の表示名。
type lockRow struct {
	LockedBy sql.NullString `db:"locked_by"`
	LockedAt sql.NullTime   `db:"locked_at"`
	UserName sql.NullString `db:"user_name"`
}

// selectLock は記事のロック列を保持者の表示名（display_name、空なら username）とともに取得する。
const selectLock = `SELECT a.locked_by, a.locked_at,
		COALESCE(NULLIF(u.display_name, ''), u.username) AS user_name
	FROM articles a LEFT JOIN users u ON u.id = a.locked_by
	WHERE a.id = ?`

// querier は *sqlx.DB と *sqlx.Conn に共通の読み取り操作。
type querier interface {
	GetContext(ctx context.Context, dest any, query string, args ...any) error
}

// Acquire は記事のロックを userID で獲得する。
//
// ロックが無いか TTL を過ぎていれば獲得し、userID が既に保持していれば locked_at を更新する。
// 他のユーザーが有効期間内に保持している場合は、保持者の情報と ErrAlreadyLocked を返す。
func (l *Locker) Acquire(ctx context.Context, articleID, userID string) (*LockInfo, error) {
	var info *LockInfo
	err := l.withWriteTx(ctx, func(conn *sqlx.Conn, now time.Time) error {
		cur, err := l.load(ctx, conn, articleID, userID, now)
		if err != nil {
			return err
		}
		if cur.IsLocked && !cur.IsOwnLock {
			info = cur
			return ErrAlreadyLocked
		}

		var name string
		err = conn.GetContext(ctx, &name,
			`SELECT COALESCE(NULLIF(display_name, ''), username) FROM users WHERE id = ?`, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unknown user %q", userID)
		}
		if err != nil {
			return fmt.Errorf("find user: %w", err)
		}

		if _, err := conn.ExecContext(ctx,
			`UPDATE articles SET locked_by = ?, locked_at = ? WHERE id = ?`, userID, now, articleID); err != nil {
			return fmt.Errorf("update lock: %w", err)
		}
		info = &LockInfo{
			ArticleID: articleID,
			UserID:    userID,
			UserName:  name,
			LockedAt:  now,
			IsLocked:  true,
			IsOwnLock: true,
		}
		return nil
	})
	if errors.Is(err, ErrAlreadyLocked) {
		return info, ErrAlreadyLocked
	}
	if err != nil {
		return nil, wrap("acquire", err)
	}
	return info, nil
}

// Refresh はハートビートとして、userID が保持する有効なロックの locked_at を現在時刻に更新する。
// ロックが無い・他のユーザーが保持している・TTL を過ぎている場合は ErrNotLockHolder を返す。
// 失効後に編集を続けるには Acquire で獲得し直す。
func (l *Locker) Refresh(ctx context.Context, articleID, userID string) error {
	err := l.withWriteTx(ctx, func(conn *sqlx.Conn, now time.Time) error {
		cur, err := l.load(ctx, conn, articleID, userID, now)
		if err != nil {
			return err
		}
		if !cur.IsOwnLock {
			return ErrNotLockHolder
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE articles SET locked_at = ? WHERE id = ?`, now, articleID); err != nil {
			return fmt.Errorf("update lock: %w", err)
		}
		return nil
	})
	return wrap("refresh", err)
}

// Release は userID が保持するロックを解放する。TTL を過ぎた自身のロックも解放する。
// ロックが無ければ何もせず成功する（ページ離脱時の重複送信に備える）。
// 他のユーザーが保持している場合は ErrNotLockHolder を返す。
func (l *Locker) Release(ctx context.Context, articleID, userID string) error {
	err := l.withWriteTx(ctx, func(conn *sqlx.Conn, now time.Time) error {
		var row lockRow
		if err := getLockRow(ctx, conn, articleID, &row); err != nil {
			return err
		}
		if !row.LockedBy.Valid {
			return nil
		}
		if row.LockedBy.String != userID {
			return ErrNotLockHolder
		}
		return clearLock(ctx, conn, articleID)
	})
	return wrap("release", err)
}

// ForceUnlock は保持者に関わらず記事のロックを解除する。
// 管理者（role = 'admin'）であることの確認は呼び出し側で行うこと。
func (l *Locker) ForceUnlock(ctx context.Context, articleID string) error {
	err := l.withWriteTx(ctx, func(conn *sqlx.Conn, now time.Time) error {
		var row lockRow
		if err := getLockRow(ctx, conn, articleID, &row); err != nil {
			return err
		}
		return clearLock(ctx, conn, articleID)
	})
	return wrap("force unlock", err)
}

// Check は記事の現在のロック状況を返す。TTL を過ぎたロックはロック無しとして扱う。
// 単一の SELECT は一貫したスナップショットを読むため、書き込みトランザクションは使わない。
func (l *Locker) Check(ctx context.Context, articleID, currentUserID string) (*LockInfo, error) {
	info, err := l.load(ctx, l.db, articleID, currentUserID, l.now().UTC())
	if err != nil {
		return nil, wrap("check", err)
	}
	return info, nil
}

// load は記事のロック行を読み、時刻 now における有効なロック状況に変換する。
func (l *Locker) load(ctx context.Context, q querier, articleID, currentUserID string, now time.Time) (*LockInfo, error) {
	var row lockRow
	if err := getLockRow(ctx, q, articleID, &row); err != nil {
		return nil, err
	}
	info := &LockInfo{ArticleID: articleID}
	if !row.LockedBy.Valid || !row.LockedAt.Valid || l.expired(row.LockedAt.Time, now) {
		return info, nil
	}
	info.UserID = row.LockedBy.String
	info.UserName = row.UserName.String
	info.LockedAt = row.LockedAt.Time.UTC()
	info.IsLocked = true
	info.IsOwnLock = info.UserID == currentUserID
	return info, nil
}

// expired は lockedAt の最終ハートビートから now までに TTL が経過したかを判定する。
func (l *Locker) expired(lockedAt, now time.Time) bool {
	return !now.Before(lockedAt.Add(l.ttl))
}

func getLockRow(ctx context.Context, q querier, articleID string, row *lockRow) error {
	err := q.GetContext(ctx, row, selectLock, articleID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrArticleNotFound
	}
	if err != nil {
		return fmt.Errorf("find article: %w", err)
	}
	return nil
}

func clearLock(ctx context.Context, conn *sqlx.Conn, articleID string) error {
	if _, err := conn.ExecContext(ctx,
		`UPDATE articles SET locked_by = NULL, locked_at = NULL WHERE id = ?`, articleID); err != nil {
		return fmt.Errorf("clear lock: %w", err)
	}
	return nil
}

// withWriteTx は fn を BEGIN IMMEDIATE のトランザクション内で実行し、fn が成功すればコミットする。
// 書き込みロックを先に取るため、読み取りから更新までの間に他の Locker 操作が割り込まない。
// fn には書き込みロック獲得後の現在時刻（UTC）を渡す。
func (l *Locker) withWriteTx(ctx context.Context, fn func(conn *sqlx.Conn, now time.Time) error) error {
	conn, err := l.db.Connx(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// ctx がキャンセル済みでも確実に巻き戻し、トランザクションを開いたまま接続をプールに返さない。
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := fn(conn, l.now().UTC()); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// wrap は内部エラーに操作名を付ける。公開エラー（Err*）は errors.Is で判定できるようそのまま返す。
func wrap(op string, err error) error {
	if err == nil || errors.Is(err, ErrNotLockHolder) || errors.Is(err, ErrArticleNotFound) || errors.Is(err, ErrAlreadyLocked) {
		return err
	}
	return fmt.Errorf("lock: %s: %w", op, err)
}
