package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

var (
	ErrUserNotFound          = errors.New("user not found")
	ErrUsernameTaken         = errors.New("username is already taken")
	ErrCannotDeleteSelf      = errors.New("cannot delete own account")
	ErrCannotDeleteLastAdmin = errors.New("cannot delete or demote the last admin")
	ErrUserHasArticles       = errors.New("cannot delete user with existing articles")
	ErrInvalidUsername       = errors.New("invalid username")
	ErrInvalidPassword       = errors.New("password too short")
	ErrInvalidRole           = errors.New("invalid role")
	// ErrSetupDone は、すでに管理者がいるため最初の管理者を作れない（「はじめに」は済んでいる）ことを示す。
	ErrSetupDone = errors.New("an admin already exists")
)

var usernameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

const minPasswordLength = 8

// ListUsers は全ユーザーを作成日時昇順で返す。
func ListUsers(db *sqlx.DB) ([]User, error) {
	var users []User
	err := db.Select(&users, `SELECT id, username, display_name, password_hash, role, created_at FROM users ORDER BY created_at ASC, username ASC`)
	return users, err
}

// GetUserByID は指定 ID のユーザーを取得する。
func GetUserByID(db *sqlx.DB, id string) (*User, error) {
	var u User
	err := db.Get(&u, `SELECT id, username, display_name, password_hash, role, created_at FROM users WHERE id = ?`, id)
	if err != nil {
		return nil, ErrUserNotFound
	}
	return &u, nil
}

// ValidUsername は username（前後の空白を除いたもの）がユーザー名に使える形式（半角英数字・-・_ の3〜32文字）かを返す。
func ValidUsername(username string) bool {
	return usernameRegex.MatchString(strings.TrimSpace(username))
}

// validateNewUser は新規ユーザーの入力を検査し、前後の空白を除いたユーザー名と表示名を返す。
func validateNewUser(username, displayName, password string, role Role) (string, string, error) {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if !usernameRegex.MatchString(username) {
		return "", "", ErrInvalidUsername
	}
	if len(password) < minPasswordLength {
		return "", "", ErrInvalidPassword
	}
	if role != RoleAdmin && role != RoleEditor && role != RoleWriter {
		return "", "", ErrInvalidRole
	}
	return username, displayName, nil
}

// newUserID は新規ユーザーの ID（16バイトの乱数の hex）を返す。
func newUserID() (string, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("generate user id: %w", err)
	}
	return hex.EncodeToString(idBytes), nil
}

// CreateUser は新規ユーザーを登録する。
func CreateUser(db *sqlx.DB, username, displayName, password string, role Role) (*User, error) {
	username, displayName, err := validateNewUser(username, displayName, password, role)
	if err != nil {
		return nil, err
	}

	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM users WHERE username = ?`, username); err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrUsernameTaken
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	id, err := newUserID()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	_, err = db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, username, displayName, hash, role, now)
	if err != nil {
		return nil, err
	}

	return &User{
		ID:           id,
		Username:     username,
		DisplayName:  displayName,
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    now,
	}, nil
}

// adminExistsQuery は管理者（admin）のユーザーがいるかを調べる副問い合わせ。デスクトップモードの「はじめに」が
// 済んでいるかの判定（SetupDone）と、最初の管理者の登録（CreateFirstUser）で同じ条件を使う。
const adminExistsQuery = `SELECT 1 FROM users WHERE role = 'admin'`

// SetupDone はデスクトップモードの「はじめに」が済んでいる（管理者のユーザーが1人でもいる）かを返す。
// 古い版の取り込みで作られた著者のユーザー（admin でない）しかいなければ、まだ済んでいない。
func SetupDone(db sqlx.Queryer) (bool, error) {
	var done bool
	err := sqlx.Get(db, &done, `SELECT EXISTS (`+adminExistsQuery+`)`)
	return done, err
}

// CreateFirstUser は、管理者が1人もいないときに限り最初のユーザーを登録する（デスクトップモードの「はじめに」画面）。
// すでに管理者がいれば何も変えずに ErrSetupDone を、古い版の取り込みで作られたユーザーと同じユーザー名なら
// ErrUsernameTaken を返す。確認と登録を1つの文で行うため、同時に送られた登録のうち1つだけが成功し、あとから
// 別の人が管理者を乗っ取ることはできない。db にはトランザクションも渡せる。
func CreateFirstUser(db sqlx.Ext, username, displayName, password string, role Role) (*User, error) {
	username, displayName, err := validateNewUser(username, displayName, password, role)
	if err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	id, err := newUserID()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	res, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role, created_at)
		SELECT ?, ?, ?, ?, ?, ? WHERE NOT EXISTS (`+adminExistsQuery+`) AND NOT EXISTS (SELECT 1 FROM users WHERE username = ?)`,
		id, username, displayName, hash, role, now, username)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		// 登録しなかった理由を確かめる。管理者がいれば、ユーザー名が重なっていても「はじめに」は済んでいる。
		done, err := SetupDone(db)
		if err != nil {
			return nil, err
		}
		if done {
			return nil, ErrSetupDone
		}
		return nil, ErrUsernameTaken
	}

	return &User{
		ID:           id,
		Username:     username,
		DisplayName:  displayName,
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    now,
	}, nil
}

// keepsAdminCondition は、対象のユーザーが管理者でないか、ほかにも管理者がいる（対象が管理者でなくなっても
// 管理者が1人以上残る）という WHERE 条件。件数の確認と降格・削除を1つの文で行うため、同時に別の降格・削除が
// あっても管理者は0人にならない（SQLite は書き込む文を1つずつ実行する）。
const keepsAdminCondition = `(role != 'admin' OR (SELECT COUNT(*) FROM users WHERE role = 'admin') > 1)`

// lastAdminOrNotFound は、降格・削除の文が1行も変えなかった理由として、ユーザーがいなければ ErrUserNotFound、
// いれば（最後の管理者だったため）ErrCannotDeleteLastAdmin を返す。
func lastAdminOrNotFound(db sqlx.Queryer, targetID string) error {
	var exists bool
	if err := sqlx.Get(db, &exists, `SELECT EXISTS (SELECT 1 FROM users WHERE id = ?)`, targetID); err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}
	return ErrCannotDeleteLastAdmin
}

// UpdateUser はユーザーの表示名とロールを更新する。最後の1人の管理者は降格しない（ErrCannotDeleteLastAdmin）。
func UpdateUser(db *sqlx.DB, currentUserID, targetID, displayName string, newRole Role) error {
	displayName = strings.TrimSpace(displayName)
	if newRole != RoleAdmin && newRole != RoleEditor && newRole != RoleWriter {
		return ErrInvalidRole
	}

	res, err := db.Exec(`UPDATE users SET display_name = ?, role = ? WHERE id = ? AND (? = 'admin' OR `+keepsAdminCondition+`)`,
		displayName, newRole, targetID, newRole)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return lastAdminOrNotFound(db, targetID)
	}
	return nil
}

// UpdateUserPassword はユーザーのパスワードを変更し、その人のログイン（全セッション）と AI アプリの接続
// （全 API トークンと、OAuth 2.1 で許可した連携のアクセストークン・リフレッシュトークン）を解除する。
// すべて1つのトランザクションで行い、どれかが失敗したら何も変えずにエラーを返す。
func UpdateUserPassword(db *sqlx.DB, targetID, newPassword string) error {
	if len(newPassword) < minPasswordLength {
		return ErrInvalidPassword
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, targetID)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err != nil {
		return err
	} else if rows == 0 {
		return ErrUserNotFound
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mcp_tokens WHERE user_id = ?`, targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM oauth_codes WHERE user_id = ?`, targetID); err != nil {
		return err
	}
	// OAuth 2.1 の連携は取り消した記録を残す（リフレッシュトークンの再利用を見分けるため、行は消さない）。
	if _, err := tx.Exec(`UPDATE oauth_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		time.Now().UTC(), targetID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUser はユーザーを、そのセッション・API トークン・OAuth の認可コードおよびトークンとともに削除する。最後の1人の管理者は削除しない
// （ErrCannotDeleteLastAdmin）。
func DeleteUser(db *sqlx.DB, currentUserID, targetID string) error {
	if currentUserID == targetID {
		return ErrCannotDeleteSelf
	}

	if _, err := GetUserByID(db, targetID); err != nil {
		return err
	}

	// 記事の著作者になっているか確認
	var articleCount int
	if err := db.Get(&articleCount, `SELECT COUNT(*) FROM articles WHERE author_id = ?`, targetID); err != nil {
		return err
	}
	if articleCount > 0 {
		return ErrUserHasArticles
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// セッション、MCPトークン、OAuth認可コード・トークンの削除
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mcp_tokens WHERE user_id = ?`, targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM oauth_codes WHERE user_id = ?`, targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM oauth_tokens WHERE user_id = ?`, targetID); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM users WHERE id = ? AND `+keepsAdminCondition, targetID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return lastAdminOrNotFound(tx, targetID)
	}

	return tx.Commit()
}
