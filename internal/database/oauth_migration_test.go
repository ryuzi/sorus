package database

import (
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
)

// TestOAuthTablesAndCascade は V12 の OAuth 2.1 のテーブル（oauth_clients・oauth_codes・oauth_tokens）が作られ、
// 制約とユーザー・クライアントの削除による CASCADE が働くことを確かめる。
func TestOAuthTablesAndCascade(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		mustExec(t, db, `INSERT INTO oauth_clients (client_id, client_name, redirect_uris) VALUES (?, ?, ?)`,
			"c1", "Claude", `["https://claude.ai/api/mcp/auth_callback"]`)
		mustExec(t, db, `INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, expires_at)
			VALUES ('h1', 'c1', 'u1', 'https://claude.ai/api/mcp/auth_callback', 'challenge', CURRENT_TIMESTAMP)`)
		mustExec(t, db, `INSERT INTO oauth_tokens (id, token_hash, refresh_token_hash, client_id, user_id, expires_at)
			VALUES ('t1', 'at1', 'rt1', 'c1', 'u1', CURRENT_TIMESTAMP)`)

		// PKCE は S256 だけを保存できる（plain は受け付けない）。
		if _, err := db.Exec(`INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, code_challenge_method, expires_at)
			VALUES ('h2', 'c1', 'u1', 'https://x', 'c', 'plain', CURRENT_TIMESTAMP)`); err == nil {
			t.Error("code_challenge_method 'plain' was accepted")
		}
		// アクセストークン・リフレッシュトークンのハッシュは重複できない。
		if _, err := db.Exec(`INSERT INTO oauth_tokens (id, token_hash, client_id, user_id, expires_at)
			VALUES ('t2', 'at1', 'c1', 'u1', CURRENT_TIMESTAMP)`); err == nil {
			t.Error("duplicate token_hash was accepted")
		}
		// 存在しないクライアント・ユーザーのトークンは保存できない。
		if _, err := db.Exec(`INSERT INTO oauth_tokens (id, token_hash, client_id, user_id, expires_at)
			VALUES ('t3', 'at3', 'missing', 'u1', CURRENT_TIMESTAMP)`); err == nil {
			t.Error("token for unknown client was accepted")
		}

		mustExec(t, db, `DELETE FROM users WHERE id = ?`, "u1")
		for _, table := range []string{"oauth_codes", "oauth_tokens"} {
			var n int
			if err := db.Get(&n, `SELECT COUNT(*) FROM `+table); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s rows after user deletion = %d, want 0", table, n)
			}
		}

		// クライアントを消すと、そのクライアントのコード・トークンも消える。
		insertUser(t, db, "u2", "bob")
		mustExec(t, db, `INSERT INTO oauth_tokens (id, token_hash, client_id, user_id, expires_at)
			VALUES ('t4', 'at4', 'c1', 'u2', CURRENT_TIMESTAMP)`)
		mustExec(t, db, `DELETE FROM oauth_clients WHERE client_id = ?`, "c1")
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM oauth_tokens`); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("oauth_tokens rows after client deletion = %d, want 0", n)
		}
	})
}

// TestMigrateV11ToV12AddsOAuthTables は V11 の DB に V12 を適用すると、既存のデータを保ったまま OAuth の
// テーブルが加わることを確かめる。
func TestMigrateV11ToV12AddsOAuthTables(t *testing.T) {
	db, err := Connect(filepath.Join(t.TempDir(), "v11.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := MigrateTo(db, 11); err != nil {
		t.Fatalf("MigrateTo(11): %v", err)
	}
	insertUser(t, db, "u1", "alice")
	mustExec(t, db, `INSERT INTO mcp_tokens (id, user_id, name, token_hash) VALUES ('m1', 'u1', 'API', 'hash')`)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() || version < 12 {
		t.Errorf("user_version = %d, want %d (>= 12)", version, SchemaVersion())
	}
	for _, table := range []string{"oauth_clients", "oauth_codes", "oauth_tokens"} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("table %s missing after migration", table)
		}
	}
	var tokens int
	if err := db.Get(&tokens, `SELECT COUNT(*) FROM mcp_tokens`); err != nil || tokens != 1 {
		t.Errorf("mcp_tokens rows = %d (%v), want 1", tokens, err)
	}
}
