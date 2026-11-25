// Package auth はパスワードハッシュ、DB駆動のセッション管理、
// Echo 用の Cookie 認証・ロールベース認可ミドルウェアを提供する。
package auth

import (
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Role はユーザーの権限。users.role の CHECK 制約と一致させること。
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	RoleWriter Role = "writer"
	RoleReader Role = "reader"
)

// User は users テーブルの1行。
type User struct {
	ID           string    `db:"id"`
	Username     string    `db:"username"`
	DisplayName  string    `db:"display_name"`
	PasswordHash string    `db:"password_hash"`
	Role         Role      `db:"role"`
	CreatedAt    time.Time `db:"created_at"`
}

// HasRole は u が roles のいずれかの役割を持つかを返す。役割による判定（RequireRole・画面の表示条件・
// ハンドラーの権限確認）はすべてこれを使う。管理者（admin）は編集者・ライターのすべての役割を持つため、
// admin・editor・writer のどれを問われても true を返す。編集者・ライターは保存上の役割（Role）だけで判定する。
// u が nil なら false。
func (u *User) HasRole(roles ...Role) bool {
	if u == nil {
		return false
	}
	for _, r := range roles {
		if u.Role == r || (u.Role == RoleAdmin && isValidRole(r)) {
			return true
		}
	}
	return false
}

// HashPassword は password を bcrypt（DefaultCost）でハッシュ化する。
// bcrypt の上限である72バイトを超える password はエラーになる。
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword は password が hash と一致するかを判定する。
// hash が不正な形式の場合も false を返す。
func CheckPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
