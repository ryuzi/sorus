package database

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/jmoiron/sqlx"
)

// migrations はスキーマの変更履歴。index+1 がスキーマバージョンとなり、
// 適用済みバージョンは PRAGMA user_version に記録する。
// 既存の要素は書き換えず、変更は末尾に追加すること。
var migrations = []string{
	schemaV1,
	schemaV2,
	schemaV3,
	schemaV4,
	schemaV5,
	schemaV6,
	schemaV7,
	schemaV8,
	schemaV9,
	schemaV10,
	schemaV11,
	schemaV12,
	schemaV13,
	schemaV14,
	schemaV15,
}

// migrationHooks は SQL だけでは書けないデータの移行。キーのバージョンの SQL を適用した直後に、
// 同じトランザクションの中で実行する。
var migrationHooks = map[int]func(ctx context.Context, conn *sqlx.Conn) error{
	8:  convertImportedPages,
	11: protectEnPageSlug,
	13: addMediaTakenAt,
	14: addArticleSyncState,
	15: addPageHierarchy,
}

// addMediaTakenAt は media に撮影日時 taken_at が無ければ足し、未設定の行を登録日時（created_at）で埋める。
// 既存の画像は撮影日が分からないため登録日時を使う。taken_at の無い行（取り込み・同期で足した画像など）も
// 並べられるよう、一覧は COALESCE(taken_at, created_at) で並べ、その式に索引を張る。
func addMediaTakenAt(ctx context.Context, conn *sqlx.Conn) error {
	var n int
	if err := conn.GetContext(ctx, &n, `SELECT COUNT(*) FROM pragma_table_info('media') WHERE name = 'taken_at'`); err != nil {
		return fmt.Errorf("check media.taken_at: %w", err)
	}
	if n == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE media ADD COLUMN taken_at DATETIME`); err != nil {
			return fmt.Errorf("add media.taken_at: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE media SET taken_at = created_at WHERE taken_at IS NULL`); err != nil {
		return fmt.Errorf("backfill media.taken_at: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_media_taken_at ON media(COALESCE(taken_at, created_at))`); err != nil {
		return fmt.Errorf("index media.taken_at: %w", err)
	}
	return nil
}

// columnDef は ADD COLUMN で足す列の名前と定義。
type columnDef struct{ name, def string }

// articleSyncColumns は addArticleSyncState が articles に足す列と、その定義。
var articleSyncColumns = []columnDef{
	{"remote_updated_at", "DATETIME"},
	{"checked_out_by", "TEXT NOT NULL DEFAULT ''"},
	{"checked_out_at", "DATETIME"},
	{"checked_out_client", "TEXT NOT NULL DEFAULT ''"},
}

// addArticleSyncState は articles に公開サーバーとの同期状態の列が無ければ足す。
//   - remote_updated_at: 公開サーバー側で最後に更新された日時（同期で取り込んだ時点の値）
//   - checked_out_by / checked_out_at: PC の sorus で編集中（チェックアウト中）の人とその開始日時
//   - checked_out_client: 編集中の印を付けた PC の sorus の識別子（同じ人・トークンの別の PC と見分ける）
//
// 列が先に足されている DB でも失敗しないよう、pragma_table_info で有無を確かめてから足す。
func addArticleSyncState(ctx context.Context, conn *sqlx.Conn) error {
	return addMissingArticleColumns(ctx, conn, articleSyncColumns)
}

// addMissingArticleColumns は cols のうち articles に無い列を ALTER TABLE ... ADD COLUMN で足す。
// 既にある列は定義が違っても変えない。
func addMissingArticleColumns(ctx context.Context, conn *sqlx.Conn, cols []columnDef) error {
	for _, col := range cols {
		var n int
		if err := conn.GetContext(ctx, &n, `SELECT COUNT(*) FROM pragma_table_info('articles') WHERE name = ?`, col.name); err != nil {
			return fmt.Errorf("check articles.%s: %w", col.name, err)
		}
		if n > 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx, "ALTER TABLE articles ADD COLUMN "+col.name+" "+col.def); err != nil {
			return fmt.Errorf("add articles.%s: %w", col.name, err)
		}
	}
	return nil
}

// pageHierarchyColumns は addPageHierarchy が articles に足す列と、その定義。
var pageHierarchyColumns = []columnDef{
	{"parent_id", "TEXT"},
	{"sort_order", "INTEGER NOT NULL DEFAULT 0"},
}

// addPageHierarchy は固定ページの階層のため、articles に親ページ parent_id と並び順 sort_order が無ければ足し、
// 子ページの一覧（parent_id で絞って sort_order の順）に使う索引を張る。
//   - parent_id: 親の固定ページの ID。最上位のページと記事（post）は NULL。外部キーは張らない（同期・取り込みで
//     親より先に子が届いても保存できるようにする）。親を完全に削除したときは article.Service が NULL に戻す。
//   - sort_order: 同じ親の中での並び順（小さいほど先）。既存の行は 0 になり、並びはタイトル・作成日時の順のまま。
//
// ADD COLUMN は既存の行を書き換えずに済む（NULL・定数の既定値）ため、表の作り直しは要らない。
// 列が先に足されている DB でも失敗しないよう、pragma_table_info で有無を確かめてから足す。
func addPageHierarchy(ctx context.Context, conn *sqlx.Conn) error {
	if err := addMissingArticleColumns(ctx, conn, pageHierarchyColumns); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_articles_parent_sort ON articles(parent_id, sort_order)`); err != nil {
		return fmt.Errorf("index articles.parent_id: %w", err)
	}
	return nil
}

// importedPagePrefix は WordPress の固定ページの旧 URL（/?page_id=<ID>）。
const importedPagePrefix = "/?page_id="

// convertImportedPages は WordPress から取り込んだ固定ページ（redirects に /?page_id= からの転送がある記事）を
// kind = 'page' にする。取り込みは転送先を /articles/<url.PathEscape(slug)> で登録するため、
// 日本語のスラッグも照合できるようパスをデコードしてから比べる。転送そのものは変えない
// （/articles/<slug> から /<slug> への 301 は公開サイトが返す）。
func convertImportedPages(ctx context.Context, conn *sqlx.Conn) error {
	var targets []string
	if err := conn.SelectContext(ctx, &targets, `SELECT new_path FROM redirects WHERE substr(old_path, 1, ?) = ?`,
		len(importedPagePrefix), importedPagePrefix); err != nil {
		return fmt.Errorf("find imported pages: %w", err)
	}
	for _, target := range targets {
		escaped, ok := strings.CutPrefix(strings.TrimRight(strings.TrimSpace(target), "/"), "/articles/")
		if !ok || escaped == "" || strings.Contains(escaped, "/") {
			continue
		}
		slug, err := url.PathUnescape(escaped)
		if err != nil {
			continue
		}
		if _, err := conn.ExecContext(ctx, `UPDATE articles SET kind = 'page' WHERE slug = ?`, slug); err != nil {
			return fmt.Errorf("convert imported page %q: %w", slug, err)
		}
	}
	return nil
}

// protectEnPageSlug は V11 マイグレーション時に、スラッグが en の固定ページが存在する場合に
// スラッグを en-page に改名する（/en は英語トップページの予約パスであるため、リダイレクトは登録しない）。
// スラッグ en-page が既に存在する場合、en-page-2, en-page-3 ... と空いているスラッグを探してリネームする。
func protectEnPageSlug(ctx context.Context, conn *sqlx.Conn) error {
	var count int
	if err := conn.GetContext(ctx, &count, `SELECT COUNT(*) FROM articles WHERE kind = 'page' AND slug = 'en' AND status <> 'deleted'`); err != nil {
		return fmt.Errorf("check en page: %w", err)
	}
	if count > 0 {
		newSlug := "en-page"
		suffix := 2
		for {
			var exists int
			if err := conn.GetContext(ctx, &exists, `SELECT COUNT(*) FROM articles WHERE slug = ?`, newSlug); err != nil {
				return fmt.Errorf("check slug conflict %q: %w", newSlug, err)
			}
			if exists == 0 {
				break
			}
			newSlug = fmt.Sprintf("en-page-%d", suffix)
			suffix++
		}
		if _, err := conn.ExecContext(ctx, `UPDATE articles SET slug = ? WHERE kind = 'page' AND slug = 'en' AND status <> 'deleted'`, newSlug); err != nil {
			return fmt.Errorf("rename en page to %q: %w", newSlug, err)
		}
	}
	// 既に /en や /en/ のリダイレクトが登録されている場合も削除して英語トップを保護する
	if _, err := conn.ExecContext(ctx, `DELETE FROM redirects WHERE old_path IN ('/en', '/en/')`); err != nil {
		return fmt.Errorf("cleanup en redirects: %w", err)
	}
	return nil
}

// schemaV1 は Phase 1 の初期スキーマ。
// 日時列は DATETIME とし、アプリ側からは UTC の time.Time で読み書きする。
const schemaV1 = `
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK(role IN ('admin', 'editor', 'writer')),
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    expires_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS categories (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    slug        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS articles (
    id                 TEXT PRIMARY KEY,
    title              TEXT NOT NULL,
    slug               TEXT NOT NULL UNIQUE,
    content_markdown   TEXT NOT NULL DEFAULT '',
    content_html       TEXT NOT NULL DEFAULT '',
    excerpt            TEXT NOT NULL DEFAULT '',
    featured_image_url TEXT NOT NULL DEFAULT '',
    author_id          TEXT NOT NULL,
    category_id        INTEGER NOT NULL,
    status             TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'published', 'deleted')),
    locked_by          TEXT,
    locked_at          DATETIME,
    published_at       DATETIME,
    created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(author_id) REFERENCES users(id),
    FOREIGN KEY(category_id) REFERENCES categories(id),
    FOREIGN KEY(locked_by) REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_articles_author_id ON articles(author_id);
CREATE INDEX IF NOT EXISTS idx_articles_category_id ON articles(category_id);
CREATE INDEX IF NOT EXISTS idx_articles_status_published_at ON articles(status, published_at);

CREATE TABLE IF NOT EXISTS media (
    id            TEXT PRIMARY KEY,
    filename      TEXT NOT NULL,
    path_original TEXT NOT NULL,
    path_large    TEXT NOT NULL DEFAULT '',
    path_medium   TEXT NOT NULL DEFAULT '',
    path_small    TEXT NOT NULL DEFAULT '',
    width         INTEGER NOT NULL DEFAULT 0,
    height        INTEGER NOT NULL DEFAULT 0,
    mime_type     TEXT NOT NULL,
    uploaded_by   TEXT,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(uploaded_by) REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_media_uploaded_by ON media(uploaded_by);
CREATE INDEX IF NOT EXISTS idx_media_created_at ON media(created_at);

CREATE TABLE IF NOT EXISTS redirects (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    old_path    TEXT NOT NULL UNIQUE,
    new_path    TEXT NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 301 CHECK(status_code IN (301, 302, 307, 308)),
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// SchemaVersion は Migrate 適用後のスキーマバージョン。
func SchemaVersion() int { return len(migrations) }

// Migrate は最新のスキーマ（users, sessions, categories, articles, media, redirects,
// article_revisions, article_comments, attachments, article_embeddings, mcp_tokens, article_views_daily, site_views_daily, settings,
// oauth_clients, oauth_codes, oauth_tokens）を自動適用する。
// SQL の後に migrationHooks のデータ移行があるバージョンは、その移行も同じトランザクションで行う。
//
// 未適用のマイグレーションだけを BEGIN IMMEDIATE のトランザクション内で実行するため、
// 複数回・複数プロセスから呼んでも安全（冪等）。
func Migrate(db *sqlx.DB) error {
	return MigrateTo(db, len(migrations))
}

// MigrateTo は指定したバージョン（1..len(migrations)）までのスキーマを適用する。
// 過去のバージョンのバックアップ復元テストなどで使用する。
func MigrateTo(db *sqlx.DB, target int) error {
	if target < 0 || target > len(migrations) {
		return fmt.Errorf("database: migrate: target version %d out of range (0..%d)", target, len(migrations))
	}
	ctx := context.Background()

	conn, err := db.Connx(ctx)
	if err != nil {
		return fmt.Errorf("database: migrate: acquire conn: %w", err)
	}
	defer conn.Close()

	// 書き込みロックを先に取り、並行マイグレーションでのバージョン競合を防ぐ。
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("database: migrate: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var current int
	if err := conn.GetContext(ctx, &current, "PRAGMA user_version"); err != nil {
		return fmt.Errorf("database: migrate: read user_version: %w", err)
	}
	if current > target {
		return fmt.Errorf("database: migrate: schema version %d is newer than supported %d", current, target)
	}

	for v := current; v < target; v++ {
		if _, err := conn.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("database: migrate: apply v%d: %w", v+1, err)
		}
		if hook, ok := migrationHooks[v+1]; ok {
			if err := hook(ctx, conn); err != nil {
				return fmt.Errorf("database: migrate: apply v%d: %w", v+1, err)
			}
		}
	}
	// PRAGMA はプレースホルダを受け付けないため整数を直接埋め込む。
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
		return fmt.Errorf("database: migrate: set user_version: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("database: migrate: commit: %w", err)
	}
	committed = true
	return nil
}
