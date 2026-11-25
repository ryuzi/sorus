package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// sessionIDBytes はセッショントークンの乱数バイト長。hex で64文字になる。
const sessionIDBytes = 32

var (
	// ErrSessionNotFound はセッションが存在しない（失効済み・不正なIDを含む）ことを示す。
	ErrSessionNotFound = errors.New("auth: session not found")
	// ErrSessionExpired はセッションの有効期限切れを示す。期限切れの行は検出時に削除される。
	ErrSessionExpired = errors.New("auth: session expired")
)

// Session は sessions テーブルの1行。ID がそのままセッション Cookie の値になる。
type Session struct {
	ID        string    `db:"id"`
	UserID    string    `db:"user_id"`
	ExpiresAt time.Time `db:"expires_at"`
	CreatedAt time.Time `db:"created_at"`
}

// CreateSession は userID のセッションを発行し、duration 後に失効するよう保存する。
func CreateSession(db *sqlx.DB, userID string, duration time.Duration) (*Session, error) {
	if duration <= 0 {
		return nil, fmt.Errorf("auth: create session: non-positive duration %v", duration)
	}
	id, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("auth: create session: %w", err)
	}
	now := time.Now().UTC()
	s := &Session{
		ID:        id,
		UserID:    userID,
		ExpiresAt: now.Add(duration),
		CreatedAt: now,
	}
	_, err = db.NamedExec(`INSERT INTO sessions (id, user_id, expires_at, created_at)
		VALUES (:id, :user_id, :expires_at, :created_at)`, s)
	if err != nil {
		return nil, fmt.Errorf("auth: create session: %w", err)
	}
	return s, nil
}

// ValidateSession は sessionID が有効であればそのユーザーを返す。
// 存在しなければ ErrSessionNotFound、期限切れなら行を削除して ErrSessionExpired を返す。
func ValidateSession(db *sqlx.DB, sessionID string) (*User, error) {
	if !validSessionID(sessionID) {
		return nil, ErrSessionNotFound
	}
	var row struct {
		User
		SessionExpiresAt time.Time `db:"session_expires_at"`
	}
	err := db.Get(&row, `SELECT u.id, u.username, u.display_name, u.password_hash, u.role, u.created_at,
			s.expires_at AS session_expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ?`, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: validate session: %w", err)
	}
	if !time.Now().Before(row.SessionExpiresAt) {
		if err := DeleteSession(db, sessionID); err != nil {
			return nil, err
		}
		return nil, ErrSessionExpired
	}
	return &row.User, nil
}

// DeleteSession は sessionID のセッションを失効させる。存在しない場合もエラーにしない。
func DeleteSession(db *sqlx.DB, sessionID string) error {
	if _, err := db.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions は期限切れのセッションをすべて削除し、削除件数を返す。
// 定期的なクリーンアップ用。
func DeleteExpiredSessions(db *sqlx.DB) (int64, error) {
	res, err := db.Exec(`DELETE FROM sessions WHERE julianday(expires_at) <= julianday(?)`, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("auth: delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("auth: delete expired sessions: %w", err)
	}
	return n, nil
}

// newSessionID は crypto/rand による32バイトの乱数を hex で返す。
func newSessionID() (string, error) {
	b := make([]byte, sessionIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validSessionID は id が newSessionID の形式（小文字hex 64文字）かを判定する。
// 形式外の値は DB に問い合わせずに拒否する。
func validSessionID(id string) bool {
	if len(id) != hex.EncodedLen(sessionIDBytes) {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
