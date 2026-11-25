package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/database"
)

// insertMCPToken は userID の API トークン（MCP トークン）の行を直接入れる。
func insertMCPToken(t *testing.T, db *sqlx.DB, id, userID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO mcp_tokens (id, user_id, name, token_hash) VALUES (?, ?, ?, ?)`,
		id, userID, "AIアプリ", "hash-"+id); err != nil {
		t.Fatalf("insert mcp token: %v", err)
	}
}

func countWhere(t *testing.T, db *sqlx.DB, table, userID string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// パスワードを変えると、その人のログイン（セッション）と AI アプリの接続（API トークン・OAuth トークン・認可コード）はすべて解除される。
// ほかの人のものは残る。
func TestUpdateUserPasswordRevokesSessionsAndTokens(t *testing.T) {
	db := setupTestDB(t)
	target, err := CreateUser(db, "member1", "メンバー", "oldpassword1", RoleWriter)
	if err != nil {
		t.Fatal(err)
	}
	other, err := CreateUser(db, "member2", "ほかの人", "otherpass12", RoleWriter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO oauth_clients (client_id, client_name, redirect_uris) VALUES ('c1', 'Claude', '["https://claude.ai/api/mcp/auth_callback"]')`); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*User{target, other} {
		if _, err := CreateSession(db, u.ID, time.Hour); err != nil {
			t.Fatal(err)
		}
		insertMCPToken(t, db, "tok-"+u.ID, u.ID)
		if _, err := db.Exec(`INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, expires_at) VALUES (?, 'c1', ?, 'https://claude.ai/api/mcp/auth_callback', 'ch', ?)`,
			"code-"+u.ID, u.ID, time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO oauth_tokens (id, token_hash, refresh_token_hash, client_id, user_id, expires_at) VALUES (?, ?, ?, 'c1', ?, ?)`,
			"ot-"+u.ID, "oth-"+u.ID, "rth-"+u.ID, u.ID, time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	if err := UpdateUserPassword(db, target.ID, "newpassword1"); err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}
	if n := countWhere(t, db, "sessions", target.ID); n != 0 {
		t.Errorf("sessions of the user = %d, want 0", n)
	}
	if n := countWhere(t, db, "mcp_tokens", target.ID); n != 0 {
		t.Errorf("API tokens of the user = %d, want 0", n)
	}
	if n := countWhere(t, db, "oauth_codes", target.ID); n != 0 {
		t.Errorf("oauth_codes of the user = %d, want 0", n)
	}
	var liveOAuth int
	if err := db.Get(&liveOAuth, `SELECT COUNT(*) FROM oauth_tokens WHERE user_id = ? AND revoked_at IS NULL`, target.ID); err != nil || liveOAuth != 0 {
		t.Errorf("live oauth_tokens of the user = %d (err %v), want 0", liveOAuth, err)
	}
	if countWhere(t, db, "sessions", other.ID) != 1 || countWhere(t, db, "mcp_tokens", other.ID) != 1 || countWhere(t, db, "oauth_codes", other.ID) != 1 {
		t.Error("another user's session, API token, or oauth_code was removed")
	}
	if err := db.Get(&liveOAuth, `SELECT COUNT(*) FROM oauth_tokens WHERE user_id = ? AND revoked_at IS NULL`, other.ID); err != nil || liveOAuth != 1 {
		t.Errorf("live oauth_tokens of another user = %d (err %v), want 1", liveOAuth, err)
	}
}

// パスワードの変更・セッションの削除・API トークンの削除のどれかが失敗したら、何も変えずにエラーを返す。
func TestUpdateUserPasswordIsAllOrNothing(t *testing.T) {
	for _, table := range []string{"sessions", "mcp_tokens"} {
		t.Run(table, func(t *testing.T) {
			db := setupTestDB(t)
			u, err := CreateUser(db, "member1", "メンバー", "oldpassword1", RoleWriter)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CreateSession(db, u.ID, time.Hour); err != nil {
				t.Fatal(err)
			}
			insertMCPToken(t, db, "tok-1", u.ID)
			if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_delete BEFORE DELETE ON %s
				BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, table)); err != nil {
				t.Fatal(err)
			}

			if err := UpdateUserPassword(db, u.ID, "newpassword1"); err == nil {
				t.Fatal("UpdateUserPassword succeeded although deleting from " + table + " failed")
			}
			got, err := GetUserByID(db, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !CheckPassword("oldpassword1", got.PasswordHash) {
				t.Error("password was changed although the update failed")
			}
			if countWhere(t, db, "sessions", u.ID) != 1 || countWhere(t, db, "mcp_tokens", u.ID) != 1 {
				t.Error("sessions or API tokens were removed although the update failed")
			}
		})
	}
}

// openFileDB は複数の接続から同時に使える、一時ディレクトリのファイルの DB を返す。
func openFileDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := database.Connect(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// 管理者が2人のとき、2人を同時に降格・削除しても、少なくとも1人は管理者のまま残る。
func TestLastAdminSurvivesConcurrentChanges(t *testing.T) {
	db := openFileDB(t)
	if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('op', 'operator', '', 'x', 'editor')`); err != nil {
		t.Fatal(err)
	}
	demote := func(id string) error { return UpdateUser(db, "op", id, "", RoleEditor) }
	remove := func(id string) error { return DeleteUser(db, "op", id) }
	cases := []struct {
		name   string
		first  func(id string) error
		second func(id string) error
	}{
		{"demote both", demote, demote},
		{"delete both", remove, remove},
		{"demote and delete", demote, remove},
	}
	const rounds = 30
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for round := range rounds {
				if _, err := db.Exec(`DELETE FROM users WHERE id IN ('a1', 'a2')`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role)
					VALUES ('a1', 'admin1', '', 'x', 'admin'), ('a2', 'admin2', '', 'x', 'admin')`); err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				errs := make([]error, 2)
				for i, op := range []func(string) error{tc.first, tc.second} {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						errs[i] = op(fmt.Sprintf("a%d", i+1))
					}()
				}
				close(start)
				wg.Wait()

				var admins int
				if err := db.Get(&admins, `SELECT COUNT(*) FROM users WHERE role = 'admin'`); err != nil {
					t.Fatal(err)
				}
				if admins != 1 {
					t.Fatalf("round %d: %d admin(s) left (errors %v), want exactly 1", round, admins, errs)
				}
				failed := 0
				for _, err := range errs {
					switch {
					case err == nil:
					case errors.Is(err, ErrCannotDeleteLastAdmin):
						failed++
					default:
						t.Fatalf("round %d: unexpected error %v", round, err)
					}
				}
				if failed != 1 {
					t.Fatalf("round %d: errors %v, want exactly one ErrCannotDeleteLastAdmin", round, errs)
				}
			}
		})
	}
}
