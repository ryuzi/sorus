package database

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

var expectedTables = []string{"users", "sessions", "categories", "articles", "media", "redirects",
	"article_revisions", "article_comments", "attachments"}

var expectedIndexes = map[string]string{
	"idx_sessions_user_id":                  "sessions",
	"idx_sessions_expires_at":               "sessions",
	"idx_articles_author_id":                "articles",
	"idx_articles_category_id":              "articles",
	"idx_articles_status_published_at":      "articles",
	"idx_articles_status_scheduled_at":      "articles",
	"idx_articles_kind_status_published_at": "articles",
	"idx_articles_translation_group":        "articles",
	"idx_articles_lang":                     "articles",
	"idx_articles_group_lang":               "articles",
	"idx_articles_parent_sort":              "articles",
	"idx_media_uploaded_by":                 "media",
	"idx_media_created_at":                  "media",
	"idx_article_revisions_author_id":       "article_revisions",
	"idx_article_comments_article_id":       "article_comments",
	"idx_article_comments_user_id":          "article_comments",
	"idx_attachments_article_id":            "attachments",
	"idx_attachments_uploaded_by":           "attachments",
}

// openDB は種類ごとに接続・マイグレーション済みのDBを返す。
func openDB(t *testing.T, kind string) *sqlx.DB {
	t.Helper()
	var path string
	switch kind {
	case "memory":
		path = ":memory:"
	case "file":
		path = filepath.Join(t.TempDir(), "test.db")
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	db, err := Connect(path)
	if err != nil {
		t.Fatalf("Connect(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func forEachKind(t *testing.T, fn func(t *testing.T, db *sqlx.DB)) {
	for _, kind := range []string{"memory", "file"} {
		t.Run(kind, func(t *testing.T) {
			fn(t, openDB(t, kind))
		})
	}
}

func TestConnectRejectsEmptyPath(t *testing.T) {
	if _, err := Connect(""); err == nil {
		t.Fatal("Connect(\"\") succeeded, want error")
	}
}

func TestConnectCreatesOwnerOnlyFile(t *testing.T) {
	skipIfNoUnixPerms(t)
	path := filepath.Join(t.TempDir(), "perm.db")
	db, err := Connect(path)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	insertUser(t, db, "u1", "alice")

	// 接続中は WAL と共有メモリのファイルも存在し、同じく 0600 であること。
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		assertFileMode(t, p, 0o600)
	}
}

func TestConnectRestrictsExistingFile(t *testing.T) {
	skipIfNoUnixPerms(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.db")

	// 0644 で作られた既存DB（と残存した付随ファイル）を用意する。
	db, err := Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(path+"-journal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-journal"} {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err = Connect(path)
	if err != nil {
		t.Fatalf("Connect existing: %v", err)
	}
	defer db.Close()
	assertFileMode(t, path, 0o600)
	assertFileMode(t, path+"-journal", 0o600)
}

func TestConnectURIPathIsOwnerOnly(t *testing.T) {
	skipIfNoUnixPerms(t)
	path := filepath.Join(t.TempDir(), "uri dir", "uri.db")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String()
	db, err := Connect(uri)
	if err != nil {
		t.Fatalf("Connect(%q): %v", uri, err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, path, 0o600)
}

func TestConnectMemoryCreatesNoFile(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	for _, path := range []string{":memory:", "file::memory:?cache=shared", "file:memdb1?mode=memory&cache=shared"} {
		db, err := Connect(path)
		if err != nil {
			t.Fatalf("Connect(%q): %v", path, err)
		}
		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate(%q): %v", path, err)
		}
		db.Close()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("in-memory Connect created file %q", e.Name())
	}
}

func TestConnectPragmas(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		db := openDB(t, "file")
		assertPragma(t, db, "foreign_keys", "1")
		assertPragma(t, db, "journal_mode", "wal")
		assertPragma(t, db, "busy_timeout", "5000")
	})
	t.Run("memory", func(t *testing.T) {
		db := openDB(t, "memory")
		assertPragma(t, db, "foreign_keys", "1")
		assertPragma(t, db, "busy_timeout", "5000")
		// インメモリDBは WAL をサポートせず "memory" のままになる。
		assertPragma(t, db, "journal_mode", "memory")
	})
}

func TestConnectPragmasAppliedToEveryPooledConn(t *testing.T) {
	db := openDB(t, "file")
	db.SetMaxOpenConns(4)
	ctx := context.Background()

	// 複数の接続を同時に保持し、それぞれで PRAGMA を確認する。
	var conns []*sqlx.Conn
	for i := 0; i < 4; i++ {
		c, err := db.Connx(ctx)
		if err != nil {
			t.Fatalf("Connx: %v", err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		var fk, timeout int
		if err := c.GetContext(ctx, &fk, "PRAGMA foreign_keys"); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i, err)
		}
		if err := c.GetContext(ctx, &timeout, "PRAGMA busy_timeout"); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if fk != 1 || timeout != 5000 {
			t.Errorf("conn %d: foreign_keys=%d busy_timeout=%d, want 1 and 5000", i, fk, timeout)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestMigrateCreatesTables(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		for _, table := range expectedTables {
			var name string
			err := db.Get(&name, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table)
			if err != nil {
				t.Errorf("table %s not found: %v", table, err)
				continue
			}
			var n int
			if err := db.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
				t.Errorf("query %s: %v", table, err)
			}
		}
	})
}

func TestMigrateCreatesColumns(t *testing.T) {
	want := map[string][]string{
		"users":      {"id", "username", "display_name", "password_hash", "role", "created_at"},
		"sessions":   {"id", "user_id", "expires_at", "created_at"},
		"categories": {"id", "name", "slug", "description"},
		"articles": {"id", "title", "slug", "content_markdown", "content_html", "excerpt",
			"featured_image_url", "author_id", "category_id", "status", "locked_by", "locked_at",
			"published_at", "created_at", "updated_at",
			"scheduled_at", "doi", "journal_name", "conference_name", "co_authors", "kind",
			"lang", "translation_group_id", "source_article_id", "is_outdated",
			"remote_updated_at", "checked_out_by", "checked_out_at", "checked_out_client",
			"parent_id", "sort_order"},
		"media": {"id", "filename", "path_original", "path_large", "path_medium", "path_small",
			"width", "height", "mime_type", "uploaded_by", "created_at",
			"alt", "caption", "ai_tags", "taken_at"},
		"redirects": {"id", "old_path", "new_path", "status_code", "created_at"},
		"article_revisions": {"id", "article_id", "revision_num", "title", "content_markdown",
			"author_id", "comment", "created_at"},
		"article_comments": {"id", "article_id", "user_id", "content", "created_at"},
		"attachments": {"id", "article_id", "filename", "file_path", "file_size", "mime_type",
			"download_count", "uploaded_by", "created_at"},
	}
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		for table, cols := range want {
			var got []string
			if err := db.Select(&got, "SELECT name FROM pragma_table_info(?) ORDER BY cid", table); err != nil {
				t.Fatalf("table_info(%s): %v", table, err)
			}
			if strings.Join(got, ",") != strings.Join(cols, ",") {
				t.Errorf("%s columns = %v, want %v", table, got, cols)
			}
		}
	})
}

func TestMigrateCreatesIndexes(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		for index, table := range expectedIndexes {
			var tbl string
			err := db.Get(&tbl, "SELECT tbl_name FROM sqlite_master WHERE type = 'index' AND name = ?", index)
			if err != nil {
				t.Errorf("index %s not found: %v", index, err)
				continue
			}
			if tbl != table {
				t.Errorf("index %s is on %s, want %s", index, tbl, table)
			}
		}

		// クエリプランでインデックスが実際に使われることを確認する。
		var plan []struct {
			ID      int    `db:"id"`
			Parent  int    `db:"parent"`
			NotUsed int    `db:"notused"`
			Detail  string `db:"detail"`
		}
		if err := db.Select(&plan, "EXPLAIN QUERY PLAN SELECT id FROM sessions WHERE user_id = ?", "u1"); err != nil {
			t.Fatalf("explain: %v", err)
		}
		if len(plan) == 0 || !strings.Contains(plan[0].Detail, "idx_sessions_user_id") {
			t.Errorf("query plan %+v does not use idx_sessions_user_id", plan)
		}
	})
}

func TestMigrateIsIdempotent(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		for i := 0; i < 3; i++ {
			if err := Migrate(db); err != nil {
				t.Fatalf("Migrate #%d: %v", i+2, err)
			}
		}
		var version int
		if err := db.Get(&version, "PRAGMA user_version"); err != nil {
			t.Fatal(err)
		}
		if version != SchemaVersion() {
			t.Errorf("user_version = %d, want %d", version, SchemaVersion())
		}
		var n int
		if err := db.Get(&n, "SELECT COUNT(*) FROM users"); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("users count after re-migrate = %d, want 1", n)
		}
	})
}

func TestMigratePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	db, err := Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	insertUser(t, db, "u1", "alice")
	db.Close()

	db, err = Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on reopen: %v", err)
	}
	var name string
	if err := db.Get(&name, "SELECT username FROM users WHERE id = ?", "u1"); err != nil {
		t.Fatalf("select after reopen: %v", err)
	}
	if name != "alice" {
		t.Errorf("username = %q, want alice", name)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Connect(path)
			if err != nil {
				errs <- err
				return
			}
			defer db.Close()
			errs <- Migrate(db)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Connect/Migrate: %v", err)
		}
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	db := openDB(t, "memory")
	if _, err := db.Exec("PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err == nil {
		t.Fatal("Migrate succeeded on newer schema, want error")
	}
}

func TestForeignKeyConstraints(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		catID := insertCategory(t, db, "News", "news")

		t.Run("article with unknown author", func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
				"a1", "Title", "title", "no-such-user", catID)
			assertFKError(t, err)
		})
		t.Run("article with unknown category", func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
				"a1", "Title", "title", "u1", 9999)
			assertFKError(t, err)
		})
		t.Run("session with unknown user", func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`,
				"s1", "no-such-user", time.Now().UTC().Add(time.Hour))
			assertFKError(t, err)
		})
		t.Run("media with unknown uploader", func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO media (id, filename, path_original, mime_type, uploaded_by) VALUES (?, ?, ?, ?, ?)`,
				"m1", "a.jpg", "/media/a.jpg", "image/jpeg", "no-such-user")
			assertFKError(t, err)
		})
		t.Run("valid article", func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
				"a1", "Title", "title", "u1", catID)
			if err != nil {
				t.Fatalf("insert valid article: %v", err)
			}
			var status string
			if err := db.Get(&status, "SELECT status FROM articles WHERE id = ?", "a1"); err != nil {
				t.Fatal(err)
			}
			if status != "draft" {
				t.Errorf("default status = %q, want draft", status)
			}
		})
		t.Run("delete referenced author", func(t *testing.T) {
			_, err := db.Exec(`DELETE FROM users WHERE id = ?`, "u1")
			assertFKError(t, err)
		})
	})
}

func TestForeignKeyDeleteActions(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		insertUser(t, db, "u2", "bob")
		catID := insertCategory(t, db, "News", "news")

		mustExec(t, db, `INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`,
			"s1", "u2", time.Now().UTC().Add(time.Hour))
		mustExec(t, db, `INSERT INTO media (id, filename, path_original, mime_type, uploaded_by) VALUES (?, ?, ?, ?, ?)`,
			"m1", "a.jpg", "/media/a.jpg", "image/jpeg", "u2")
		mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, locked_by, locked_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"a1", "Title", "title", "u1", catID, "u2", time.Now().UTC())

		mustExec(t, db, `DELETE FROM users WHERE id = ?`, "u2")

		var sessions int
		if err := db.Get(&sessions, "SELECT COUNT(*) FROM sessions WHERE user_id = ?", "u2"); err != nil {
			t.Fatal(err)
		}
		if sessions != 0 {
			t.Errorf("sessions of deleted user = %d, want 0 (ON DELETE CASCADE)", sessions)
		}
		var uploadedBy, lockedBy *string
		if err := db.Get(&uploadedBy, "SELECT uploaded_by FROM media WHERE id = ?", "m1"); err != nil {
			t.Fatal(err)
		}
		if uploadedBy != nil {
			t.Errorf("media.uploaded_by = %q, want NULL", *uploadedBy)
		}
		if err := db.Get(&lockedBy, "SELECT locked_by FROM articles WHERE id = ?", "a1"); err != nil {
			t.Fatal(err)
		}
		if lockedBy != nil {
			t.Errorf("articles.locked_by = %q, want NULL", *lockedBy)
		}
	})
}

func TestCheckAndUniqueConstraints(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		catID := insertCategory(t, db, "News", "news")

		cases := []struct {
			name  string
			query string
			args  []any
		}{
			{"invalid role", `INSERT INTO users (id, username, password_hash, role) VALUES (?, ?, ?, ?)`,
				[]any{"u9", "mallory", "h", "root"}},
			{"duplicate username", `INSERT INTO users (id, username, password_hash, role) VALUES (?, ?, ?, ?)`,
				[]any{"u9", "alice", "h", "writer"}},
			{"duplicate category slug", `INSERT INTO categories (name, slug) VALUES (?, ?)`,
				[]any{"Other", "news"}},
			{"invalid article status", `INSERT INTO articles (id, title, slug, author_id, category_id, status) VALUES (?, ?, ?, ?, ?, ?)`,
				[]any{"a9", "T", "t", "u1", catID, "archived"}},
			{"invalid redirect status", `INSERT INTO redirects (old_path, new_path, status_code) VALUES (?, ?, ?)`,
				[]any{"/old", "/new", 200}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := db.Exec(tc.query, tc.args...); err == nil {
					t.Fatal("insert succeeded, want constraint error")
				}
			})
		}

		mustExec(t, db, `INSERT INTO redirects (old_path, new_path) VALUES (?, ?)`, "/2020/01/old-post", "/articles/new-post")
		var code int
		if err := db.Get(&code, "SELECT status_code FROM redirects WHERE old_path = ?", "/2020/01/old-post"); err != nil {
			t.Fatal(err)
		}
		if code != 301 {
			t.Errorf("default status_code = %d, want 301", code)
		}
		if _, err := db.Exec(`INSERT INTO redirects (old_path, new_path) VALUES (?, ?)`, "/2020/01/old-post", "/x"); err == nil {
			t.Error("duplicate redirect old_path accepted, want error")
		}
	})
}

func TestTimeRoundTrip(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		expires := time.Date(2026, 10, 19, 22, 0, 0, 123456000, time.UTC)
		mustExec(t, db, `INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`, "s1", "u1", expires)

		var row struct {
			ExpiresAt time.Time `db:"expires_at"`
			CreatedAt time.Time `db:"created_at"`
		}
		if err := db.Get(&row, "SELECT expires_at, created_at FROM sessions WHERE id = ?", "s1"); err != nil {
			t.Fatalf("scan times: %v", err)
		}
		if !row.ExpiresAt.Equal(expires) {
			t.Errorf("expires_at = %v, want %v", row.ExpiresAt, expires)
		}
		if row.CreatedAt.IsZero() {
			t.Error("created_at default not set")
		}

		// SQLite の日時関数と比較できる形式で保存されていること。
		var n int
		if err := db.Get(&n, "SELECT COUNT(*) FROM sessions WHERE datetime(expires_at) = datetime('2026-10-19 22:00:00')"); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("datetime() comparison matched %d rows, want 1", n)
		}
	})
}

func insertUser(t *testing.T, db *sqlx.DB, id, username string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, ?, ?)`,
		id, username, username, "hash", "writer")
}

func insertCategory(t *testing.T, db *sqlx.DB, name, slug string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO categories (name, slug) VALUES (?, ?)`, name, slug)
	if err != nil {
		t.Fatalf("insert category: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustExec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func assertPragma(t *testing.T, db *sqlx.DB, name, want string) {
	t.Helper()
	var got string
	if err := db.Get(&got, "PRAGMA "+name); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	if !strings.EqualFold(got, want) {
		t.Errorf("PRAGMA %s = %q, want %q", name, got, want)
	}
}

// skipIfNoUnixPerms は Unix のパーミッションビットを持たない OS でテストを飛ばす。
func skipIfNoUnixPerms(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions are not supported on Windows")
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %#o, want %#o", filepath.Base(path), got, want)
	}
}

func assertFKError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("statement succeeded, want FOREIGN KEY constraint error")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("error = %v, want FOREIGN KEY constraint error", err)
	}
}
