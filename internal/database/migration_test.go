package database

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// v1ArticleColumns は Phase 1 の articles の列。アップグレード前後で値が変わらないことを確認する。
const v1ArticleColumns = `id, title, slug, content_markdown, content_html, excerpt, featured_image_url,
	author_id, category_id, status, locked_by, locked_at, published_at, created_at, updated_at`

// openV1DB は Phase 1 のスキーマ（user_version = 1）だけを適用したファイルDBを返す。
func openV1DB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db, err := Connect(path)
	if err != nil {
		t.Fatalf("Connect(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, schemaV1)
	mustExec(t, db, "PRAGMA user_version = 1")
	return db
}

// articleRows は v1 の列で全記事を ID 順に読む。
func articleRows(t *testing.T, db *sqlx.DB) []map[string]any {
	t.Helper()
	rows, err := db.Queryx(`SELECT ` + v1ArticleColumns + ` FROM articles ORDER BY id`)
	if err != nil {
		t.Fatalf("select articles: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		m := map[string]any{}
		if err := rows.MapScan(m); err != nil {
			t.Fatalf("scan article: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrateUpgradesV1DatabaseWithoutDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db := openV1DB(t, path)

	insertUser(t, db, "u1", "alice")
	insertUser(t, db, "u2", "bob")
	catID := insertCategory(t, db, "News", "news")
	published := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	locked := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO articles (id, title, slug, content_markdown, content_html, excerpt,
			featured_image_url, author_id, category_id, status, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"a1", "入学式のお知らせ", "nyugakushiki", "# 本文", "<h1>本文</h1>", "抜粋", "/uploads/a.webp",
		"u1", catID, "published", published)
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, locked_by, locked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a2", "下書き", "draft", "u1", catID, "u2", locked)
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status)
		VALUES (?, ?, ?, ?, ?, ?)`, "a3", "削除済み", "trash", "u1", catID, "deleted")
	mustExec(t, db, `INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`,
		"s1", "u1", time.Now().UTC().Add(time.Hour))
	mustExec(t, db, `INSERT INTO media (id, filename, path_original, mime_type, uploaded_by) VALUES (?, ?, ?, ?, ?)`,
		"m1", "a.jpg", "/media/a.jpg", "image/jpeg", "u1")
	mustExec(t, db, `INSERT INTO redirects (old_path, new_path) VALUES (?, ?)`, "/?p=1", "/articles/nyugakushiki")

	before := articleRows(t, db)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v1 -> v%d: %v", SchemaVersion(), err)
	}

	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion())
	}
	var defaultTheme string
	if err := db.Get(&defaultTheme, `SELECT value FROM settings WHERE key = 'theme'`); err != nil {
		t.Fatalf("read default theme: %v", err)
	}
	if defaultTheme != "modern" {
		t.Errorf("default theme = %q, want modern", defaultTheme)
	}

	if after := articleRows(t, db); !reflect.DeepEqual(before, after) {
		t.Errorf("articles changed by migration:\nbefore %v\nafter  %v", before, after)
	}

	// 新しい列は既存の行で NULL または空文字になる。
	var added []struct {
		ScheduledAt    *time.Time `db:"scheduled_at"`
		DOI            string     `db:"doi"`
		JournalName    string     `db:"journal_name"`
		ConferenceName string     `db:"conference_name"`
		CoAuthors      string     `db:"co_authors"`
	}
	if err := db.Select(&added, `SELECT scheduled_at, doi, journal_name, conference_name, co_authors FROM articles`); err != nil {
		t.Fatalf("select new columns: %v", err)
	}
	if len(added) != 3 {
		t.Fatalf("articles count = %d, want 3", len(added))
	}
	for _, r := range added {
		if r.ScheduledAt != nil || r.DOI != "" || r.JournalName != "" || r.ConferenceName != "" || r.CoAuthors != "" {
			t.Errorf("new columns of existing row = %+v, want NULL and empty strings", r)
		}
	}

	for table, want := range map[string]int{"users": 2, "categories": 1, "sessions": 1, "media": 1, "redirects": 1} {
		var n int
		if err := db.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s count = %d, want %d", table, n, want)
		}
	}

	// 作り直しの一時テーブルが残らず、インデックスと整合性が保たれていること。
	var leftovers int
	if err := db.Get(&leftovers, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'articles_v2'`); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Error("temporary table articles_v2 remains after migration")
	}
	for index, table := range expectedIndexes {
		var tbl string
		if err := db.Get(&tbl, "SELECT tbl_name FROM sqlite_master WHERE type = 'index' AND name = ?", index); err != nil {
			t.Errorf("index %s not found after upgrade: %v", index, err)
		} else if tbl != table {
			t.Errorf("index %s is on %s, want %s", index, tbl, table)
		}
	}
	assertPragma(t, db, "integrity_check", "ok")
	var fkViolations int
	if err := db.Get(&fkViolations, "SELECT COUNT(*) FROM pragma_foreign_key_check"); err != nil {
		t.Fatal(err)
	}
	if fkViolations != 0 {
		t.Errorf("foreign_key_check reported %d violations", fkViolations)
	}

	// 作り直した articles でも外部キー制約と ON DELETE SET NULL が働くこと。
	_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
		"a9", "T", "t", "no-such-user", catID)
	assertFKError(t, err)
	mustExec(t, db, `DELETE FROM users WHERE id = ?`, "u2")
	var lockedBy *string
	if err := db.Get(&lockedBy, "SELECT locked_by FROM articles WHERE id = ?", "a2"); err != nil {
		t.Fatal(err)
	}
	if lockedBy != nil {
		t.Errorf("articles.locked_by = %q after deleting locker, want NULL", *lockedBy)
	}

	// 再オープンして再度 Migrate しても何も変わらないこと。
	db.Close()
	db, err = Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after upgrade: %v", err)
	}
	if n := len(articleRows(t, db)); n != 3 {
		t.Errorf("articles count after re-migrate = %d, want 3", n)
	}
}

func TestMigrateV2FailureRollsBackToV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	db := openV1DB(t, path)
	insertUser(t, db, "u1", "alice")
	catID := insertCategory(t, db, "News", "news")
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
		"a1", "Title", "title", "u1", catID)

	// 外部キー制約を無効にした接続で、存在しない著者を参照する行を作る（DB の直接編集を模す）。
	conn, err := db.Connx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"PRAGMA foreign_keys = OFF",
		`INSERT INTO articles (id, title, slug, author_id, category_id) VALUES ('a2', 'Orphan', 'orphan', 'ghost', 1)`,
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	conn.Close()
	before := articleRows(t, db)

	if err := Migrate(db); err == nil {
		t.Fatal("Migrate succeeded with a foreign key violation in articles, want error")
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("user_version after failed migration = %d, want 1", version)
	}
	if after := articleRows(t, db); !reflect.DeepEqual(before, after) {
		t.Errorf("articles changed by failed migration:\nbefore %v\nafter  %v", before, after)
	}
	var tables []string
	if err := db.Select(&tables, `SELECT name FROM sqlite_master WHERE type = 'table' AND name IN
		('articles_v2', 'article_revisions', 'article_comments', 'attachments', 'article_embeddings')`); err != nil {
		t.Fatal(err)
	}
	if len(tables) != 0 {
		t.Errorf("tables %v remain after failed migration", tables)
	}
	var cols []string
	if err := db.Select(&cols, "SELECT name FROM pragma_table_info('articles') WHERE name = 'scheduled_at'"); err != nil {
		t.Fatal(err)
	}
	if len(cols) != 0 {
		t.Error("articles.scheduled_at exists after failed migration")
	}
}

func TestMigrateArticleStatuses(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		catID := insertCategory(t, db, "News", "news")
		for i, status := range []string{"draft", "review", "scheduled", "published", "deleted"} {
			_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id, status) VALUES (?, ?, ?, ?, ?, ?)`,
				status, "T", "t"+string(rune('a'+i)), "u1", catID, status)
			if err != nil {
				t.Errorf("insert status %q: %v", status, err)
			}
		}
		_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id, status) VALUES (?, ?, ?, ?, ?, ?)`,
			"x", "T", "x", "u1", catID, "archived")
		if err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("insert status archived: err = %v, want CHECK constraint error", err)
		}
	})
}

func TestPhase6TablesConstraints(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		insertUser(t, db, "u2", "bob")
		catID := insertCategory(t, db, "News", "news")
		mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
			"a1", "Title", "title", "u1", catID)

		insertRevision := func(id string, num int) error {
			_, err := db.Exec(`INSERT INTO article_revisions (id, article_id, revision_num, title, content_markdown, author_id)
				VALUES (?, ?, ?, ?, ?, ?)`, id, "a1", num, "Title", "body", "u2")
			return err
		}
		if err := insertRevision("r1", 1); err != nil {
			t.Fatalf("insert revision: %v", err)
		}
		if err := insertRevision("r1-dup", 1); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			t.Errorf("duplicate revision_num: err = %v, want UNIQUE constraint error", err)
		}
		if err := insertRevision("r0", 0); err == nil {
			t.Error("revision_num 0 accepted, want CHECK constraint error")
		}
		mustExec(t, db, `INSERT INTO article_comments (id, article_id, user_id, content) VALUES (?, ?, ?, ?)`,
			"c1", "a1", "u2", "表記揺れを直してください")
		mustExec(t, db, `INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "f1", "a1", "paper.pdf", "attachments/f1.pdf", 1024, "application/pdf", "u1")

		var downloads int
		if err := db.Get(&downloads, "SELECT download_count FROM attachments WHERE id = ?", "f1"); err != nil {
			t.Fatal(err)
		}
		if downloads != 0 {
			t.Errorf("default download_count = %d, want 0", downloads)
		}
		if _, err := db.Exec(`INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "f2", "a1", "x.pdf", "x", -1, "application/pdf", "u1"); err == nil {
			t.Error("negative file_size accepted, want CHECK constraint error")
		}

		t.Run("unknown article", func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO article_comments (id, article_id, user_id, content) VALUES (?, ?, ?, ?)`,
				"c9", "no-such-article", "u1", "x")
			assertFKError(t, err)
			_, err = db.Exec(`INSERT INTO article_revisions (id, article_id, revision_num, title, content_markdown, author_id)
				VALUES (?, ?, ?, ?, ?, ?)`, "r9", "no-such-article", 1, "T", "", "u1")
			assertFKError(t, err)
			_, err = db.Exec(`INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, "f9", "no-such-article", "a.pdf", "a", 1, "application/pdf", "u1")
			assertFKError(t, err)
		})

		// 記事を物理削除すると、リビジョン・コメント・添付も削除される。
		mustExec(t, db, `DELETE FROM articles WHERE id = ?`, "a1")
		for _, table := range []string{"article_revisions", "article_comments", "attachments"} {
			var n int
			if err := db.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s rows after deleting article = %d, want 0 (ON DELETE CASCADE)", table, n)
			}
		}
	})
}

func TestPhase6QueriesUseIndexes(t *testing.T) {
	db := openDB(t, "memory")
	cases := []struct {
		query string
		args  []any
		index string
	}{
		// UNIQUE(article_id, revision_num) の自動インデックス（_1 は主キー）。
		{`SELECT id FROM article_revisions WHERE article_id = ? ORDER BY revision_num DESC LIMIT 1`,
			[]any{"a1"}, "sqlite_autoindex_article_revisions_2"},
		{`SELECT id FROM article_comments WHERE article_id = ? ORDER BY created_at`,
			[]any{"a1"}, "idx_article_comments_article_id"},
		{`SELECT id FROM attachments WHERE article_id = ? ORDER BY created_at`,
			[]any{"a1"}, "idx_attachments_article_id"},
		{`SELECT id FROM articles WHERE status = ? AND scheduled_at IS NOT NULL`,
			[]any{"scheduled"}, "idx_articles_status_scheduled_at"},
	}
	for _, tc := range cases {
		var plan []struct {
			ID      int    `db:"id"`
			Parent  int    `db:"parent"`
			NotUsed int    `db:"notused"`
			Detail  string `db:"detail"`
		}
		if err := db.Select(&plan, "EXPLAIN QUERY PLAN "+tc.query, tc.args...); err != nil {
			t.Fatalf("explain %q: %v", tc.query, err)
		}
		var details []string
		for _, p := range plan {
			details = append(details, p.Detail)
		}
		got := strings.Join(details, "; ")
		if !strings.Contains(got, tc.index) || strings.Contains(got, "TEMP B-TREE") {
			t.Errorf("plan for %q = %q, want %s without a temp b-tree", tc.query, got, tc.index)
		}
	}
}

// openV2DB は Phase 6 までのスキーマ（user_version = 2）を適用したファイルDBを返す。
func openV2DB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db := openV1DB(t, path)
	mustExec(t, db, schemaV2)
	mustExec(t, db, "PRAGMA user_version = 2")
	return db
}

func TestMigrateUpgradesV2DatabaseToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db := openV2DB(t, path)
	insertUser(t, db, "u1", "alice")
	catID := insertCategory(t, db, "News", "news")
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, doi) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"a1", "研究紹介", "lab", "u1", catID, "review", "10.1234/abc")
	mustExec(t, db, `INSERT INTO article_revisions (id, article_id, revision_num, title, content_markdown, author_id)
		VALUES (?, ?, ?, ?, ?, ?)`, "r1", "a1", 1, "研究紹介", "本文", "u1")
	before := articleRows(t, db)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v2 -> v%d: %v", SchemaVersion(), err)
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion())
	}
	if after := articleRows(t, db); !reflect.DeepEqual(before, after) {
		t.Errorf("articles changed by migration:\nbefore %v\nafter  %v", before, after)
	}
	var doi string
	if err := db.Get(&doi, "SELECT doi FROM articles WHERE id = ?", "a1"); err != nil {
		t.Fatal(err)
	}
	if doi != "10.1234/abc" {
		t.Errorf("doi = %q after migration, want 10.1234/abc", doi)
	}
	var revisions int
	if err := db.Get(&revisions, "SELECT COUNT(*) FROM article_revisions"); err != nil {
		t.Fatal(err)
	}
	if revisions != 1 {
		t.Errorf("article_revisions count = %d, want 1", revisions)
	}

	var cols []string
	if err := db.Select(&cols, "SELECT name FROM pragma_table_info('article_embeddings') ORDER BY cid"); err != nil {
		t.Fatal(err)
	}
	want := "id,article_id,chunk_index,chunk_text,heading,embedding,dimensions,model,created_at"
	if got := strings.Join(cols, ","); got != want {
		t.Errorf("article_embeddings columns = %s, want %s", got, want)
	}
	var tbl string
	if err := db.Get(&tbl, "SELECT tbl_name FROM sqlite_master WHERE type = 'index' AND name = ?",
		"idx_article_embeddings_article_id"); err != nil {
		t.Errorf("index idx_article_embeddings_article_id not found: %v", err)
	} else if tbl != "article_embeddings" {
		t.Errorf("index idx_article_embeddings_article_id is on %s, want article_embeddings", tbl)
	}
	assertPragma(t, db, "integrity_check", "ok")

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after upgrade: %v", err)
	}
}

// embedding は dims 次元分（4バイト/次元）のゼロ埋めの BLOB を返す。
func embedding(dims int) []byte { return make([]byte, dims*4) }

func TestArticleEmbeddingsConstraints(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		catID := insertCategory(t, db, "News", "news")
		for _, id := range []string{"a1", "a2"} {
			mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`,
				id, "Title "+id, "slug-"+id, "u1", catID)
		}

		insert := func(id, articleID string, chunk int, blob []byte, dims any) error {
			if dims == nil {
				_, err := db.Exec(`INSERT INTO article_embeddings (id, article_id, chunk_index, chunk_text, embedding, model)
					VALUES (?, ?, ?, ?, ?, ?)`, id, articleID, chunk, "本文", blob, "text-embedding-004")
				return err
			}
			_, err := db.Exec(`INSERT INTO article_embeddings (id, article_id, chunk_index, chunk_text, embedding, dimensions, model)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, id, articleID, chunk, "本文", blob, dims, "text-embedding-004")
			return err
		}

		// dimensions の既定は 768 で、heading の既定は空文字。
		if err := insert("e1", "a1", 0, embedding(768), nil); err != nil {
			t.Fatalf("insert default-dimension embedding: %v", err)
		}
		var row struct {
			Dimensions int    `db:"dimensions"`
			Heading    string `db:"heading"`
		}
		if err := db.Get(&row, "SELECT dimensions, heading FROM article_embeddings WHERE id = ?", "e1"); err != nil {
			t.Fatal(err)
		}
		if row.Dimensions != 768 || row.Heading != "" {
			t.Errorf("defaults = %+v, want dimensions 768 and empty heading", row)
		}
		if err := insert("e2", "a1", 1, embedding(3), 3); err != nil {
			t.Fatalf("insert 3-dimension embedding: %v", err)
		}
		if err := insert("e3", "a2", 0, embedding(3), 3); err != nil {
			t.Fatalf("insert embedding of another article: %v", err)
		}

		cases := []struct {
			name    string
			err     error
			wantMsg string
		}{
			{"duplicate chunk index", insert("e9", "a1", 1, embedding(3), 3), "UNIQUE"},
			{"negative chunk index", insert("e9", "a1", -1, embedding(3), 3), "CHECK"},
			{"blob shorter than dimensions", insert("e9", "a1", 2, embedding(2), 3), "CHECK"},
			{"default dimensions with short blob", insert("e9", "a1", 2, embedding(3), nil), "CHECK"},
			{"zero dimensions", insert("e9", "a1", 2, []byte{}, 0), "CHECK"},
		}
		for _, tc := range cases {
			if tc.err == nil || !strings.Contains(tc.err.Error(), tc.wantMsg) {
				t.Errorf("%s: err = %v, want %s constraint error", tc.name, tc.err, tc.wantMsg)
			}
		}
		assertFKError(t, insert("e9", "no-such-article", 0, embedding(3), 3))

		// 記事を物理削除すると、その記事のチャンクだけが削除される。
		mustExec(t, db, `DELETE FROM articles WHERE id = ?`, "a1")
		var ids []string
		if err := db.Select(&ids, "SELECT id FROM article_embeddings ORDER BY id"); err != nil {
			t.Fatal(err)
		}
		if strings.Join(ids, ",") != "e3" {
			t.Errorf("article_embeddings after deleting a1 = %v, want [e3] (ON DELETE CASCADE)", ids)
		}
	})
}

func TestArticleEmbeddingsQueriesUseIndex(t *testing.T) {
	db := openDB(t, "memory")
	for _, query := range []string{
		`SELECT id FROM article_embeddings WHERE article_id = ? ORDER BY chunk_index`,
		`DELETE FROM article_embeddings WHERE article_id = ?`,
	} {
		var plan []struct {
			ID      int    `db:"id"`
			Parent  int    `db:"parent"`
			NotUsed int    `db:"notused"`
			Detail  string `db:"detail"`
		}
		if err := db.Select(&plan, "EXPLAIN QUERY PLAN "+query, "a1"); err != nil {
			t.Fatalf("explain %q: %v", query, err)
		}
		var details []string
		for _, p := range plan {
			details = append(details, p.Detail)
		}
		got := strings.Join(details, "; ")
		if !strings.Contains(got, "idx_article_embeddings_article_id") || strings.Contains(got, "TEMP B-TREE") {
			t.Errorf("plan for %q = %q, want idx_article_embeddings_article_id without a temp b-tree", query, got)
		}
	}
}

func TestPhase8TablesAndCascade(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		catID := insertCategory(t, db, "News", "news")
		mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id) VALUES (?, ?, ?, ?, ?)`, "a1", "Title", "title", "u1", catID)
		mustExec(t, db, `INSERT INTO mcp_tokens (id, user_id, name, token_hash) VALUES (?, ?, ?, ?)`, "t1", "u1", "API", "hash")
		mustExec(t, db, `INSERT INTO article_views_daily (article_id, date, views, unique_visitors) VALUES (?, ?, ?, ?)`, "a1", "2026-10-20", 3, 2)
		mustExec(t, db, `INSERT INTO site_views_daily (date, views, unique_visitors) VALUES (?, ?, ?)`, "2026-10-20", 3, 2)
		mustExec(t, db, `DELETE FROM articles WHERE id = ?`, "a1")
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM article_views_daily`); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("article_views_daily rows after article deletion = %d, want 0", n)
		}
		mustExec(t, db, `DELETE FROM users WHERE id = ?`, "u1")
		if err := db.Get(&n, `SELECT COUNT(*) FROM mcp_tokens`); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("mcp_tokens rows after user deletion = %d, want 0", n)
		}
	})
}

// openV5DB は Phase 8 のサイト設定までのスキーマ（user_version = 5）を適用したファイルDBを返す。
func openV5DB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db := openV2DB(t, path)
	for _, q := range []string{schemaV3, schemaV4, schemaV5} {
		mustExec(t, db, q)
	}
	mustExec(t, db, "PRAGMA user_version = 5")
	return db
}

// mediaColumns は media の列名を定義順に返す。
func mediaColumns(t *testing.T, db *sqlx.DB) []string {
	t.Helper()
	var cols []string
	if err := db.Select(&cols, "SELECT name FROM pragma_table_info('media') ORDER BY cid"); err != nil {
		t.Fatal(err)
	}
	return cols
}

func TestMigrateUpgradesV5DatabaseToV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	db := openV5DB(t, path)
	insertUser(t, db, "u1", "alice")
	mustExec(t, db, `INSERT INTO media (id, filename, path_original, path_large, path_medium, path_small, width, height, mime_type, uploaded_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "m1", "campus.png", "uploads/a-original.webp", "uploads/a-1200.webp",
		"uploads/a-800.webp", "uploads/a-400.webp", 1600, 900, "image/png", "u1")
	if got := mediaColumns(t, db); slices.Contains(got, "alt") {
		t.Fatalf("v5 media already has alt: %v", got)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v5 -> v%d: %v", SchemaVersion(), err)
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion())
	}
	want := "id,filename,path_original,path_large,path_medium,path_small,width,height,mime_type,uploaded_by,created_at,alt,caption,ai_tags,taken_at"
	if got := strings.Join(mediaColumns(t, db), ","); got != want {
		t.Errorf("media columns = %s, want %s", got, want)
	}

	// 既存の行は残り、追加した列は空文字で埋まる。
	var m struct {
		Filename string `db:"filename"`
		Width    int    `db:"width"`
		Alt      string `db:"alt"`
		Caption  string `db:"caption"`
		AITags   string `db:"ai_tags"`
	}
	if err := db.Get(&m, "SELECT filename, width, alt, caption, ai_tags FROM media WHERE id = ?", "m1"); err != nil {
		t.Fatal(err)
	}
	if m.Filename != "campus.png" || m.Width != 1600 || m.Alt != "" || m.Caption != "" || m.AITags != "" {
		t.Errorf("media row after migration = %+v", m)
	}
	// 新しい列は NOT NULL で、値を指定しない挿入でも空文字になる。
	if _, err := db.Exec("UPDATE media SET alt = NULL WHERE id = ?", "m1"); err == nil {
		t.Error("media.alt accepted NULL, want NOT NULL constraint")
	}
	mustExec(t, db, "UPDATE media SET alt = ?, caption = ?, ai_tags = ? WHERE id = ?", "校舎", "春の校舎", "キャンパス, 春", "m1")
	assertPragma(t, db, "integrity_check", "ok")

	// 適用済みの DB に再実行しても変わらない（ALTER TABLE を二度流さない）。
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after upgrade: %v", err)
	}
	if err := db.Get(&m, "SELECT filename, width, alt, caption, ai_tags FROM media WHERE id = ?", "m1"); err != nil {
		t.Fatal(err)
	}
	if m.Alt != "校舎" || m.Caption != "春の校舎" || m.AITags != "キャンパス, 春" {
		t.Errorf("media row after re-migrate = %+v", m)
	}
}

func TestMigrateV6FailureRollsBackToV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken-v5.db")
	db := openV5DB(t, path)
	// caption 列だけを手作業で足した DB を模す。V6 は alt を足したあと caption で失敗する。
	mustExec(t, db, "ALTER TABLE media ADD COLUMN caption TEXT NOT NULL DEFAULT ''")
	before := mediaColumns(t, db)

	if err := Migrate(db); err == nil {
		t.Fatal("Migrate succeeded with a duplicate media.caption column, want error")
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != 5 {
		t.Errorf("user_version after failed migration = %d, want 5", version)
	}
	// 失敗した V6 で先に足した alt も取り消される。
	if after := mediaColumns(t, db); !reflect.DeepEqual(before, after) {
		t.Errorf("media columns changed by failed migration:\nbefore %v\nafter  %v", before, after)
	}
	assertPragma(t, db, "integrity_check", "ok")
}

func TestFreshInstallHasMediaMetadataColumns(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		cols := mediaColumns(t, db)
		for _, c := range []string{"alt", "caption", "ai_tags"} {
			if !slices.Contains(cols, c) {
				t.Errorf("media columns %v do not include %s", cols, c)
			}
		}
		insertUser(t, db, "u1", "alice")
		mustExec(t, db, `INSERT INTO media (id, filename, path_original, mime_type) VALUES (?, ?, ?, ?)`,
			"m1", "a.png", "uploads/a.webp", "image/webp")
		var alt string
		if err := db.Get(&alt, "SELECT alt FROM media WHERE id = ?", "m1"); err != nil || alt != "" {
			t.Errorf("alt = %q (%v), want empty", alt, err)
		}
	})
}

// openV6DB はメディアの説明列までのスキーマ（user_version = 6）を適用したファイルDBを返す。
func openV6DB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db := openV5DB(t, path)
	mustExec(t, db, schemaV6)
	mustExec(t, db, "PRAGMA user_version = 6")
	return db
}

func TestMigrateV7ReplacesLegacyThemes(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"mono", "tech"},
		{"minimal", "modern"},
		{"editorial", "news"},
		{"news", "news"},
		{"tech", "tech"},
		{"my-custom", "my-custom"},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			db := openV6DB(t, filepath.Join(t.TempDir(), "v6.db"))
			mustExec(t, db, "UPDATE settings SET value = ? WHERE key = 'theme'", tc.stored)
			// テーマ以外の設定に同じ文字列があっても書き換えない。
			mustExec(t, db, "INSERT INTO settings (key, value) VALUES ('site_name', ?)", tc.stored)

			if err := Migrate(db); err != nil {
				t.Fatalf("Migrate v6 -> v%d: %v", SchemaVersion(), err)
			}
			var version int
			if err := db.Get(&version, "PRAGMA user_version"); err != nil {
				t.Fatal(err)
			}
			if version != SchemaVersion() || SchemaVersion() < 7 {
				t.Errorf("user_version = %d, SchemaVersion = %d, want >= 7", version, SchemaVersion())
			}
			var theme, siteName string
			if err := db.Get(&theme, "SELECT value FROM settings WHERE key = 'theme'"); err != nil {
				t.Fatal(err)
			}
			if err := db.Get(&siteName, "SELECT value FROM settings WHERE key = 'site_name'"); err != nil {
				t.Fatal(err)
			}
			if theme != tc.want || siteName != tc.stored {
				t.Errorf("theme = %q, site_name = %q, want %q, %q", theme, siteName, tc.want, tc.stored)
			}
		})
	}
}

func TestFreshInstallStoresDefaultTheme(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		var theme string
		if err := db.Get(&theme, "SELECT value FROM settings WHERE key = 'theme'"); err != nil || theme != "modern" {
			t.Errorf("theme = %q (%v), want modern", theme, err)
		}
	})
}

// openV7DB は固定ページ（kind）より前のスキーマ（user_version = 7）を適用したファイルDBを返す。
func openV7DB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db := openV6DB(t, path)
	mustExec(t, db, schemaV7)
	mustExec(t, db, "PRAGMA user_version = 7")
	return db
}

func TestMigrateV8AddsArticleKind(t *testing.T) {
	db := openV7DB(t, filepath.Join(t.TempDir(), "v7.db"))
	insertUser(t, db, "u1", "alice")
	cat := insertCategory(t, db, "未分類", "uncategorized")
	insert := func(id, slug string) {
		mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status) VALUES (?, ?, ?, 'u1', ?, 'published')`,
			id, slug, slug, cat)
	}
	insert("a-post", "news-1")
	insert("a-about", "about")
	insert("a-members", "メンバー")
	insert("a-p-only", "post-with-p")
	insert("a-other", "other-redirect")
	// WordPress から取り込んだ固定ページは /?page_id= から /articles/<エスケープしたスラッグ> へ転送している。
	mustExec(t, db, `INSERT INTO redirects (old_path, new_path) VALUES
		('/?page_id=2', '/articles/about'),
		('/?page_id=7', '/articles/%E3%83%A1%E3%83%B3%E3%83%90%E3%83%BC'),
		('/?p=9', '/articles/post-with-p'),
		('/old/other', '/articles/other-redirect'),
		('/?page_id=8', '/articles/missing'),
		('/?page_id=10', 'https://example.com/elsewhere')`)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v7 -> v%d: %v", SchemaVersion(), err)
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion() || SchemaVersion() < 8 {
		t.Errorf("user_version = %d, SchemaVersion = %d, want >= 8", version, SchemaVersion())
	}
	kinds := map[string]string{}
	var rows []struct{ ID, Kind string }
	if err := db.Select(&rows, "SELECT id, kind FROM articles"); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		kinds[r.ID] = r.Kind
	}
	want := map[string]string{"a-post": "post", "a-about": "page", "a-members": "page", "a-p-only": "post", "a-other": "post"}
	for id, k := range want {
		if kinds[id] != k {
			t.Errorf("kind of %s = %q, want %q", id, kinds[id], k)
		}
	}
	// 転送は変えない（/articles/<slug> から /<slug> への 301 は公開サイトが返す）。
	var redirects int
	if err := db.Get(&redirects, "SELECT COUNT(*) FROM redirects WHERE new_path = '/articles/about'"); err != nil || redirects != 1 {
		t.Errorf("redirect to /articles/about = %d (%v), want unchanged", redirects, err)
	}
	assertPragma(t, db, "integrity_check", "ok")

	// 再実行しても変わらない。
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after upgrade: %v", err)
	}
}

func TestArticleKindColumn(t *testing.T) {
	forEachKind(t, func(t *testing.T, db *sqlx.DB) {
		insertUser(t, db, "u1", "alice")
		cat := insertCategory(t, db, "未分類", "uncategorized")
		// 指定しなければ post。
		mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id) VALUES ('a1', 'A', 'a', 'u1', ?)`, cat)
		var kind string
		if err := db.Get(&kind, "SELECT kind FROM articles WHERE id = 'a1'"); err != nil || kind != "post" {
			t.Errorf("default kind = %q (%v), want post", kind, err)
		}
		mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, kind) VALUES ('a2', 'B', 'b', 'u1', ?, 'page')`, cat)
		for _, bad := range []string{"Page", "posts", "", "category"} {
			if _, err := db.Exec(`UPDATE articles SET kind = ? WHERE id = 'a1'`, bad); err == nil {
				t.Errorf("kind %q was accepted, want CHECK constraint error", bad)
			}
		}
		if _, err := db.Exec(`UPDATE articles SET kind = NULL WHERE id = 'a1'`); err == nil {
			t.Error("kind NULL was accepted, want NOT NULL constraint error")
		}
	})
}

// openV9DB は予約投稿をそろえる前のスキーマ（user_version = 9）を適用したファイルDBを返す。
func openV9DB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db := openV7DB(t, path)
	mustExec(t, db, schemaV8)
	mustExec(t, db, schemaV9)
	mustExec(t, db, "PRAGMA user_version = 9")
	return db
}

// scheduleRow は予約投稿の移行で変わりうる列。
type scheduleRow struct {
	ID          string     `db:"id"`
	Status      string     `db:"status"`
	PublishedAt *time.Time `db:"published_at"`
	ScheduledAt *time.Time `db:"scheduled_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

func scheduleRows(t *testing.T, db *sqlx.DB) map[string]scheduleRow {
	t.Helper()
	var rows []scheduleRow
	if err := db.Select(&rows, `SELECT id, status, published_at, scheduled_at, updated_at FROM articles ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]scheduleRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func TestMigrateV10UnifiesScheduledArticles(t *testing.T) {
	db := openV9DB(t, filepath.Join(t.TempDir(), "v9.db"))
	insertUser(t, db, "u1", "alice")
	cat := insertCategory(t, db, "未分類", "uncategorized")
	approved := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	future := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	past := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	firstPublished := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	insert := func(id, status string, publishedAt, scheduledAt *time.Time) {
		mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, published_at, scheduled_at, updated_at)
			VALUES (?, ?, ?, 'u1', ?, ?, ?, ?, ?)`, id, id, id, cat, status, publishedAt, scheduledAt, approved)
	}
	insert("future", "scheduled", nil, &future)
	insert("overdue", "scheduled", nil, &past)
	insert("no-time", "scheduled", nil, nil)
	// 一度公開した記事を予約し直したもの。公開日時は予約日時に置き換わる（旧スケジューラーと同じ）。
	insert("republish", "scheduled", &firstPublished, &future)
	// 旧スケジューラーが公開した記事は予約日時を残していた。
	insert("flipped", "published", &past, &past)
	insert("draft", "draft", nil, nil)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v9 -> v%d: %v", SchemaVersion(), err)
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil || version != SchemaVersion() || SchemaVersion() < 10 {
		t.Fatalf("user_version = %d (%v), SchemaVersion = %d, want >= 10", version, err, SchemaVersion())
	}
	got := scheduleRows(t, db)
	want := map[string]struct {
		status      string
		publishedAt *time.Time
	}{
		"future": {"published", &future}, "overdue": {"published", &past}, "no-time": {"review", nil},
		"republish": {"published", &future}, "flipped": {"published", &past}, "draft": {"draft", nil},
	}
	for id, w := range want {
		r := got[id]
		if r.Status != w.status || (r.PublishedAt == nil) != (w.publishedAt == nil) ||
			(w.publishedAt != nil && !r.PublishedAt.Equal(*w.publishedAt)) {
			t.Errorf("%s = status %q published_at %v, want %q %v", id, r.Status, r.PublishedAt, w.status, w.publishedAt)
		}
		if r.ScheduledAt != nil {
			t.Errorf("%s scheduled_at = %v, want NULL (unused)", id, r.ScheduledAt)
		}
		// 更新日時は承認したときのまま（スケジューラーが予約の記事と見分けるのに使う）。
		if !r.UpdatedAt.Equal(approved) {
			t.Errorf("%s updated_at = %v, want unchanged %v", id, r.UpdatedAt, approved)
		}
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM articles WHERE status = 'scheduled'`); err != nil || n != 0 {
		t.Errorf("scheduled rows after migration = %d (%v), want 0", n, err)
	}
	assertPragma(t, db, "integrity_check", "ok")

	// 移行の SQL をもう一度実行しても、Migrate を再実行しても変わらない。
	mustExec(t, db, schemaV10)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after upgrade: %v", err)
	}
	if again := scheduleRows(t, db); !reflect.DeepEqual(again, got) {
		t.Errorf("rows changed on second run:\n got %+v\nwant %+v", again, got)
	}
}

// openV10DB は多言語化前のスキーマ（user_version = 10）を適用したファイルDBを返す。
func openV10DB(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db := openV9DB(t, path)
	mustExec(t, db, schemaV10)
	mustExec(t, db, "PRAGMA user_version = 10")
	return db
}

type multilingualRow struct {
	ID                 string  `db:"id"`
	Lang               string  `db:"lang"`
	TranslationGroupID string  `db:"translation_group_id"`
	SourceArticleID    *string `db:"source_article_id"`
	IsOutdated         int     `db:"is_outdated"`
}

func TestMigrateV11MultilingualArticles(t *testing.T) {
	db := openV10DB(t, filepath.Join(t.TempDir(), "v10.db"))
	insertUser(t, db, "u1", "alice")
	cat := insertCategory(t, db, "ニュース", "news")

	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status)
		VALUES ('art-1', '日本語記事1', 'slug-1', 'u1', ?, 'published')`, cat)
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status)
		VALUES ('art-2', '日本語記事2', 'slug-2', 'u1', ?, 'draft')`, cat)

	// V11 マイグレーション前の slug="en" の固定ページ
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, kind)
		VALUES ('page-en', '英語ページ旧', 'en', 'u1', ?, 'published', 'page')`, cat)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v10 -> v%d: %v", SchemaVersion(), err)
	}

	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil || version != SchemaVersion() || SchemaVersion() < 11 {
		t.Fatalf("user_version = %d (%v), SchemaVersion = %d, want >= 11", version, err, SchemaVersion())
	}

	// slug="en" の固定ページが "en-page" に改名され、英語トップ保護のため redirects には登録されないこと
	var enPageSlug string
	if err := db.Get(&enPageSlug, `SELECT slug FROM articles WHERE id = 'page-en'`); err != nil || enPageSlug != "en-page" {
		t.Errorf("page-en slug = %q (%v), want 'en-page'", enPageSlug, err)
	}
	var redirCount int
	if err := db.Get(&redirCount, `SELECT COUNT(*) FROM redirects WHERE old_path IN ('/en', '/en/')`); err != nil || redirCount != 0 {
		t.Errorf("redirect for /en should not exist (redirCount=%d, err=%v)", redirCount, err)
	}

	// idx_articles_group_lang 部分ユニークインデックスの確認:
	// 同一 translation_group_id かつ同一 lang の非deleted記事の重複挿入はエラーになること
	_, err := db.Exec(`INSERT INTO articles (id, title, slug, author_id, category_id, status, lang, translation_group_id)
		VALUES ('art-dup', '重複', 'slug-dup', 'u1', ?, 'published', 'ja', 'art-1')`, cat)
	if err == nil {
		t.Error("duplicate (translation_group_id, lang) should fail unique index constraint")
	}

	var rows []multilingualRow
	if err := db.Select(&rows, `SELECT id, lang, translation_group_id, source_article_id, is_outdated FROM articles ORDER BY id`); err != nil {
		t.Fatalf("select multilingual articles: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("articles count = %d, want 3", len(rows))
	}

	for _, r := range rows {
		if r.Lang != "ja" {
			t.Errorf("article %s lang = %q, want 'ja'", r.ID, r.Lang)
		}
		if r.TranslationGroupID != r.ID {
			t.Errorf("article %s translation_group_id = %q, want %q", r.ID, r.TranslationGroupID, r.ID)
		}
		if r.SourceArticleID != nil {
			t.Errorf("article %s source_article_id = %v, want nil", r.ID, *r.SourceArticleID)
		}
		if r.IsOutdated != 0 {
			t.Errorf("article %s is_outdated = %d, want 0", r.ID, r.IsOutdated)
		}
	}

	assertPragma(t, db, "integrity_check", "ok")

	// 冪等性の確認: 再度 Migrate を実行しても問題ないこと
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate second run: %v", err)
	}

	var rowsAfter []multilingualRow
	if err := db.Select(&rowsAfter, `SELECT id, lang, translation_group_id, source_article_id, is_outdated FROM articles ORDER BY id`); err != nil {
		t.Fatalf("select multilingual articles second run: %v", err)
	}
	if !reflect.DeepEqual(rows, rowsAfter) {
		t.Errorf("rows changed after second migration:\n got %+v\nwant %+v", rowsAfter, rows)
	}
}

func TestMigrateV11EnPageSlugConflict(t *testing.T) {
	db := openV10DB(t, filepath.Join(t.TempDir(), "v10-conflict.db"))
	insertUser(t, db, "u1", "alice")
	cat := insertCategory(t, db, "ニュース", "news")

	// 既に en-page および en-page-2 が存在している状態を作る
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, kind)
		VALUES ('p-1', '既存ページ1', 'en-page', 'u1', ?, 'published', 'page')`, cat)
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, kind)
		VALUES ('p-2', '既存ページ2', 'en-page-2', 'u1', ?, 'published', 'page')`, cat)
	// スラッグ en の固定ページ
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status, kind)
		VALUES ('p-en', '英語旧', 'en', 'u1', ?, 'published', 'page')`, cat)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v10 -> v%d: %v", SchemaVersion(), err)
	}

	// en-page, en-page-2 が埋まっているため、en-page-3 にリネームされること
	var slug string
	if err := db.Get(&slug, `SELECT slug FROM articles WHERE id = 'p-en'`); err != nil || slug != "en-page-3" {
		t.Errorf("p-en slug = %q (%v), want 'en-page-3'", slug, err)
	}
}

func TestMigration_MultilingualSettings(t *testing.T) {
	db, err := Connect(filepath.Join(t.TempDir(), "multilingual.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	// settings テーブルで site_name_en が正しく保存・取得できること
	mustExec(t, db, `INSERT INTO settings (key, value) VALUES ('site_name_en', 'Sorus English Site')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`)

	var siteNameEn string
	if err := db.Get(&siteNameEn, `SELECT value FROM settings WHERE key = 'site_name_en'`); err != nil {
		t.Fatalf("get site_name_en: %v", err)
	}
	if siteNameEn != "Sorus English Site" {
		t.Errorf("site_name_en = %q, want 'Sorus English Site'", siteNameEn)
	}

	// サイト構成メニューの多言語 JSON が正しく保存・取得できること
	menuJSON := `[{"label":"ホーム","label_en":"Home","type":"url","target":"/"},{"label":"ニュース","label_en":"News","type":"category","target":"news"}]`
	mustExec(t, db, `INSERT INTO settings (key, value) VALUES ('site_menu', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, menuJSON)

	var storedMenu string
	if err := db.Get(&storedMenu, `SELECT value FROM settings WHERE key = 'site_menu'`); err != nil {
		t.Fatalf("get site_menu: %v", err)
	}
	if storedMenu != menuJSON {
		t.Errorf("site_menu = %q, want %q", storedMenu, menuJSON)
	}
}

func TestMigrateV14AddsArticleSyncState(t *testing.T) {
	db, err := Connect(filepath.Join(t.TempDir(), "v13.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := MigrateTo(db, 13); err != nil {
		t.Fatalf("MigrateTo 13: %v", err)
	}
	insertUser(t, db, "u1", "alice")
	cat := insertCategory(t, db, "ニュース", "news")
	mustExec(t, db, `INSERT INTO articles (id, title, slug, author_id, category_id, status)
		VALUES ('a-1', '既存記事', 'old', 'u1', ?, 'published')`, cat)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v13 -> v%d: %v", SchemaVersion(), err)
	}
	var version int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil || version != SchemaVersion() || SchemaVersion() < 14 {
		t.Fatalf("user_version = %d (%v), SchemaVersion = %d, want >= 14", version, err, SchemaVersion())
	}
	var row struct {
		RemoteUpdatedAt  *string `db:"remote_updated_at"`
		CheckedOutBy     string  `db:"checked_out_by"`
		CheckedOutAt     *string `db:"checked_out_at"`
		CheckedOutClient string  `db:"checked_out_client"`
	}
	if err := db.Get(&row, `SELECT remote_updated_at, checked_out_by, checked_out_at, checked_out_client FROM articles WHERE id = 'a-1'`); err != nil {
		t.Fatalf("select sync columns: %v", err)
	}
	if row.RemoteUpdatedAt != nil || row.CheckedOutBy != "" || row.CheckedOutAt != nil || row.CheckedOutClient != "" {
		t.Errorf("existing row sync state = %+v, want empty", row)
	}
}

// 列が先に足されている DB でも v14 の移行が失敗しないこと。
func TestMigrateV14ToleratesExistingColumns(t *testing.T) {
	db, err := Connect(filepath.Join(t.TempDir(), "v13-cols.db"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := MigrateTo(db, 13); err != nil {
		t.Fatalf("MigrateTo 13: %v", err)
	}
	mustExec(t, db, `ALTER TABLE articles ADD COLUMN checked_out_by TEXT NOT NULL DEFAULT ''`)
	mustExec(t, db, `ALTER TABLE articles ADD COLUMN checked_out_client TEXT NOT NULL DEFAULT ''`)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate with pre-existing column: %v", err)
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('articles')
		WHERE name IN ('remote_updated_at', 'checked_out_by', 'checked_out_at', 'checked_out_client')`); err != nil || n != 4 {
		t.Errorf("sync columns = %d (%v), want 4", n, err)
	}
}
