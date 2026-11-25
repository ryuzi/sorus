package auth

import (
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"sorus/internal/database"
)

func setupTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUserManagementCRUD(t *testing.T) {
	db := setupTestDB(t)

	// 1. 初期管理者作成
	admin, err := CreateUser(db, "admin1", "管理者", "adminpass123", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}
	if admin.Username != "admin1" || admin.Role != RoleAdmin {
		t.Errorf("admin = %+v", admin)
	}

	// 2. バリデーションエラー検証
	_, err = CreateUser(db, "admin1", "重複", "password123", RoleWriter)
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken, got %v", err)
	}
	_, err = CreateUser(db, "a", "短すぎ", "password123", RoleWriter)
	if !errors.Is(err, ErrInvalidUsername) {
		t.Errorf("expected ErrInvalidUsername, got %v", err)
	}
	_, err = CreateUser(db, "writer1", "ライター", "short", RoleWriter)
	if !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got %v", err)
	}
	_, err = CreateUser(db, "writer1", "ライター", "password123", Role("unknown"))
	if !errors.Is(err, ErrInvalidRole) {
		t.Errorf("expected ErrInvalidRole, got %v", err)
	}

	// 3. ライター作成
	writer, err := CreateUser(db, "writer1", "山田記者", "writerpass123", RoleWriter)
	if err != nil {
		t.Fatalf("CreateUser writer: %v", err)
	}

	// 4. ListUsers
	users, err := ListUsers(db)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("len(users) = %d, want 2", len(users))
	}

	// 5. UpdateUser: ロール変更と最後の管理者保護
	err = UpdateUser(db, admin.ID, admin.ID, "新管理者", RoleWriter)
	if !errors.Is(err, ErrCannotDeleteLastAdmin) {
		t.Errorf("expected ErrCannotDeleteLastAdmin, got %v", err)
	}

	// 管理者をもう1人追加
	admin2, err := CreateUser(db, "admin2", "副管理者", "admin2pass123", RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser admin2: %v", err)
	}

	// 2人いれば降格可能
	if err := UpdateUser(db, admin.ID, admin2.ID, "副管理者（編集へ）", RoleEditor); err != nil {
		t.Fatalf("UpdateUser admin2 to editor: %v", err)
	}

	// 6. パスワード更新
	if err := UpdateUserPassword(db, writer.ID, "newpassword123"); err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}
	updatedWriter, _ := GetUserByID(db, writer.ID)
	if !CheckPassword("newpassword123", updatedWriter.PasswordHash) {
		t.Errorf("new password verification failed")
	}

	// 7. DeleteUser: 自身削除防止、最後の管理者保護、正常削除
	if err := DeleteUser(db, admin.ID, admin.ID); !errors.Is(err, ErrCannotDeleteSelf) {
		t.Errorf("expected ErrCannotDeleteSelf, got %v", err)
	}
	if err := DeleteUser(db, writer.ID, admin.ID); !errors.Is(err, ErrCannotDeleteLastAdmin) {
		t.Errorf("expected ErrCannotDeleteLastAdmin, got %v", err)
	}

	// 記事があるユーザーの削除防止
	_, err = db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id, status) VALUES ('art1', '記事', 'art1', ?, 1, 'draft')`, writer.ID)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	if err := DeleteUser(db, admin.ID, writer.ID); !errors.Is(err, ErrUserHasArticles) {
		t.Errorf("expected ErrUserHasArticles, got %v", err)
	}

	// 記事削除後はユーザー削除可能
	_, _ = db.Exec(`DELETE FROM articles WHERE id = 'art1'`)
	if err := DeleteUser(db, admin.ID, writer.ID); err != nil {
		t.Fatalf("DeleteUser writer: %v", err)
	}
	_, err = GetUserByID(db, writer.ID)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound after deletion, got %v", err)
	}
}
