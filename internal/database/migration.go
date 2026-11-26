package database

// schemaV2 は Phase 6（共同編集ワークフロー・予約投稿・リビジョン・資料添付・学術メタデータ）のスキーマ変更。
//
// articles.status の CHECK 制約に review（校閲中）と scheduled（予約投稿）を加える必要があるが、
// SQLite は既存の制約を ALTER TABLE で変更できない。そのため SQLite の推奨手順に従い、新しい定義の
// テーブルを作って全行をコピーし、旧テーブルと置き換える。予約日時・学術メタデータの列は
// ALTER TABLE ... ADD COLUMN と同じく末尾に追加し、既存の行では NULL または空文字になる。
// 既存の列の値と並び順は変わらない。DROP TABLE で消える articles のインデックスは作り直す。
// 既存の行に外部キー違反（DB を直接編集した場合など）があるとコピーが失敗し、Migrate は
// マイグレーション全体を巻き戻して v1 のまま残す。
//
// 外部キー制約が有効な接続で DROP TABLE すると参照元の行に ON DELETE 動作が連鎖するため、
// articles を参照するテーブル（article_revisions など）は必ず作り直しの後に作成する。
const schemaV2 = `
CREATE TABLE articles_v2 (
    id                 TEXT PRIMARY KEY,
    title              TEXT NOT NULL,
    slug               TEXT NOT NULL UNIQUE,
    content_markdown   TEXT NOT NULL DEFAULT '',
    content_html       TEXT NOT NULL DEFAULT '',
    excerpt            TEXT NOT NULL DEFAULT '',
    featured_image_url TEXT NOT NULL DEFAULT '',
    author_id          TEXT NOT NULL,
    category_id        INTEGER NOT NULL,
    status             TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'review', 'scheduled', 'published', 'deleted')),
    locked_by          TEXT,
    locked_at          DATETIME,
    published_at       DATETIME,
    created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    scheduled_at       DATETIME,
    doi                TEXT NOT NULL DEFAULT '',
    journal_name       TEXT NOT NULL DEFAULT '',
    conference_name    TEXT NOT NULL DEFAULT '',
    co_authors         TEXT NOT NULL DEFAULT '',
    FOREIGN KEY(author_id) REFERENCES users(id),
    FOREIGN KEY(category_id) REFERENCES categories(id),
    FOREIGN KEY(locked_by) REFERENCES users(id) ON DELETE SET NULL
);
INSERT INTO articles_v2 (id, title, slug, content_markdown, content_html, excerpt, featured_image_url,
        author_id, category_id, status, locked_by, locked_at, published_at, created_at, updated_at)
    SELECT id, title, slug, content_markdown, content_html, excerpt, featured_image_url,
        author_id, category_id, status, locked_by, locked_at, published_at, created_at, updated_at
    FROM articles;
DROP TABLE articles;
ALTER TABLE articles_v2 RENAME TO articles;
CREATE INDEX IF NOT EXISTS idx_articles_author_id ON articles(author_id);
CREATE INDEX IF NOT EXISTS idx_articles_category_id ON articles(category_id);
CREATE INDEX IF NOT EXISTS idx_articles_status_published_at ON articles(status, published_at);
CREATE INDEX IF NOT EXISTS idx_articles_status_scheduled_at ON articles(status, scheduled_at);

-- 記事のタイトル・本文のスナップショット。revision_num は記事ごとに 1 から振る連番。
-- UNIQUE 制約のインデックスが (article_id, revision_num) の検索と降順の走査を兼ねる。
CREATE TABLE IF NOT EXISTS article_revisions (
    id               TEXT PRIMARY KEY,
    article_id       TEXT NOT NULL,
    revision_num     INTEGER NOT NULL CHECK(revision_num > 0),
    title            TEXT NOT NULL,
    content_markdown TEXT NOT NULL,
    author_id        TEXT NOT NULL,
    comment          TEXT NOT NULL DEFAULT '',
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(article_id, revision_num),
    FOREIGN KEY(article_id) REFERENCES articles(id) ON DELETE CASCADE,
    FOREIGN KEY(author_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_revisions_author_id ON article_revisions(author_id);

-- 校閲・申し送りのための内部コメント。公開サイトには表示しない。
CREATE TABLE IF NOT EXISTS article_comments (
    id         TEXT PRIMARY KEY,
    article_id TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(article_id) REFERENCES articles(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_comments_article_id ON article_comments(article_id, created_at);
CREATE INDEX IF NOT EXISTS idx_article_comments_user_id ON article_comments(user_id);

-- 記事に添付する論文 PDF・配布資料。filename は利用者に見せる元のファイル名、file_path は保存先。
CREATE TABLE IF NOT EXISTS attachments (
    id             TEXT PRIMARY KEY,
    article_id     TEXT NOT NULL,
    filename       TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    file_size      INTEGER NOT NULL CHECK(file_size >= 0),
    mime_type      TEXT NOT NULL,
    download_count INTEGER NOT NULL DEFAULT 0 CHECK(download_count >= 0),
    uploaded_by    TEXT NOT NULL,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(article_id) REFERENCES articles(id) ON DELETE CASCADE,
    FOREIGN KEY(uploaded_by) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_attachments_article_id ON attachments(article_id, created_at);
CREATE INDEX IF NOT EXISTS idx_attachments_uploaded_by ON attachments(uploaded_by);
`

// schemaV3 は Phase 7（純Go RAG）のスキーマ変更。記事本文を見出し・段落単位で分割したチャンクと、
// その埋め込みベクトルを保存する。embedding は float32 のリトルエンディアン列で、長さは dimensions * 4 バイト。
// 異なるモデルのベクトルは比較できないため、検索時は model と dimensions で絞り込む。
//
// (article_id, chunk_index) の一意インデックスが記事ごとの削除・チャンク順の走査を兼ねる。
// 記事を物理削除するとチャンクも削除される。
const schemaV3 = `
CREATE TABLE IF NOT EXISTS article_embeddings (
    id          TEXT PRIMARY KEY,
    article_id  TEXT NOT NULL,
    chunk_index INTEGER NOT NULL CHECK(chunk_index >= 0),
    chunk_text  TEXT NOT NULL,
    heading     TEXT NOT NULL DEFAULT '',
    embedding   BLOB NOT NULL,
    dimensions  INTEGER NOT NULL DEFAULT 768 CHECK(dimensions > 0),
    model       TEXT NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK(length(embedding) = dimensions * 4),
    FOREIGN KEY(article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_article_embeddings_article_id ON article_embeddings(article_id, chunk_index);
`

// schemaV4 は Phase 8 の API トークンと日次ページビュー集計スキーマ。
const schemaV4 = `
CREATE TABLE IF NOT EXISTS mcp_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    last_used_at DATETIME,
    expires_at   DATETIME,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_mcp_tokens_user_id ON mcp_tokens(user_id);
CREATE TABLE IF NOT EXISTS article_views_daily (
    article_id       TEXT NOT NULL,
    date             TEXT NOT NULL,
    views            INTEGER NOT NULL DEFAULT 0 CHECK(views >= 0),
    unique_visitors  INTEGER NOT NULL DEFAULT 0 CHECK(unique_visitors >= 0),
    PRIMARY KEY(article_id, date),
    FOREIGN KEY(article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_views_daily_date_views ON article_views_daily(date, views DESC);
CREATE TABLE IF NOT EXISTS site_views_daily (
    date             TEXT PRIMARY KEY,
    views            INTEGER NOT NULL DEFAULT 0 CHECK(views >= 0),
    unique_visitors  INTEGER NOT NULL DEFAULT 0 CHECK(unique_visitors >= 0)
);
`

// schemaV5 stores site-wide settings, including the public theme selected by an administrator.
const schemaV5 = `
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO settings (key, value) VALUES ('theme', 'modern');
`

// schemaV6 はメディアライブラリ（DAM）用に、画像の代替テキスト・キャプション・AI タグを media に追加する。
// 新規インストールでも V1 から順に適用するため、V1 の media 定義は変えずにここで列を足す
// （V1 に列を加えると、この ALTER TABLE が重複列で失敗する）。
const schemaV6 = `
ALTER TABLE media ADD COLUMN alt TEXT NOT NULL DEFAULT '';
ALTER TABLE media ADD COLUMN caption TEXT NOT NULL DEFAULT '';
ALTER TABLE media ADD COLUMN ai_tags TEXT NOT NULL DEFAULT '';
`

// schemaV7 は廃止した公開テーマ（minimal・editorial・mono）の保存値を、見た目の近い残ったテーマに置き換える
// （theme system v2）。古いバックアップから復元した DB も、起動時にこの置き換えを受ける。
const schemaV7 = `
UPDATE settings SET value = CASE value
    WHEN 'minimal'   THEN 'modern'
    WHEN 'editorial' THEN 'news'
    WHEN 'mono'      THEN 'tech'
    ELSE value END
WHERE key = 'theme';
`

// schemaV8 は固定ページ（サイト構成）のため、記事に種類 kind（post: 記事 / page: ページ）を足す。
// 既存の記事はすべて post になる。公開サイトの時系列の一覧は kind と状態・公開日時で絞るため、索引を作る。
// WordPress から取り込んだ固定ページを page に変える処理は、転送先のパスのデコードが要るため
// convertImportedPages（V8 の適用直後に同じトランザクションで実行する）が行う。
const schemaV8 = `
ALTER TABLE articles ADD COLUMN kind TEXT NOT NULL DEFAULT 'post' CHECK(kind IN ('post','page'));
CREATE INDEX IF NOT EXISTS idx_articles_kind_status_published_at ON articles(kind, status, published_at);
`

// schemaV9 は公開サーバーが発行したケーパビリティ・トークン（招待）の記録。jti はトークン ID（32桁の16進数）で、
// トークン自体は保存しない。issued_via は発行の経路（cli: sorus token create、api: 同期 API の issue_invite、
// seen: 記録の無いトークンが初めて使われたときに記録したもの）、issued_by は api で発行した管理者のトークンの sub。
// revoked_at が NULL でなければ取り消し済みで、同期 API はそのトークンを拒否する。jti を持たない古い形式の
// トークンは記録できない（個別には取り消せない）。
const schemaV9 = `
CREATE TABLE IF NOT EXISTS sync_tokens (
    jti        TEXT PRIMARY KEY CHECK(length(jti) = 32),
    subject    TEXT NOT NULL,
    role       TEXT NOT NULL CHECK(role IN ('writer', 'editor', 'admin')),
    issued_at  DATETIME NOT NULL,
    expires_at DATETIME NOT NULL,
    issued_via TEXT NOT NULL CHECK(issued_via IN ('cli', 'api', 'seen')),
    issued_by  TEXT NOT NULL DEFAULT '',
    revoked_at DATETIME,
    revoked_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sync_tokens_issued_at ON sync_tokens(issued_at);
`

// schemaV10 は予約投稿の考え方をそろえる（schedule_unify_spec の D2）。予約中の記事は状態を公開（published）、
// 公開日時を未来の日時として表し、状態 scheduled は保存しない。既存の予約投稿は、公開日時を予約日時（scheduled_at）に
// した公開記事にする。予約日時の無い予約投稿（DB を直接編集した場合など）は公開も予約もできないため、校閲待ちに戻す。
// 古い版のバックアップを復元したときも、復元は Migrate を通るためこの変換を受ける。何度実行しても結果は変わらない。
//
// articles.scheduled_at はこれ以降使わない。idx_articles_status_scheduled_at の索引が参照しているため列は消さずに残し、
// 値だけを消す（索引も使わないが、V2 の索引の試験と同じ形で残す）。CHECK 制約の 'scheduled' も表を作り直さないと
// 外せないため残し、保存しないことは article.Service が保証する。
const schemaV10 = `
UPDATE articles SET status = 'published', published_at = scheduled_at
WHERE status = 'scheduled' AND scheduled_at IS NOT NULL;
UPDATE articles SET status = 'review' WHERE status = 'scheduled';
UPDATE articles SET scheduled_at = NULL WHERE scheduled_at IS NOT NULL;
`

// schemaV11 は多言語化・翻訳クラスタ（Translation Cluster）のスキーマ変更。
// articles テーブルに言語コード lang、翻訳グループID translation_group_id、
// 翻訳元の記事ID source_article_id、原文更新フラグ is_outdated を追加する。
// 既存の記事は translation_group_id を自身の id に初期化し、自らを起点とする日本語記事とする。
const schemaV11 = `
ALTER TABLE articles ADD COLUMN lang TEXT NOT NULL DEFAULT 'ja';
ALTER TABLE articles ADD COLUMN translation_group_id TEXT NOT NULL DEFAULT '';
ALTER TABLE articles ADD COLUMN source_article_id TEXT;
ALTER TABLE articles ADD COLUMN is_outdated INTEGER NOT NULL DEFAULT 0;
UPDATE articles SET translation_group_id = id WHERE translation_group_id = '' OR translation_group_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_articles_translation_group ON articles(translation_group_id);
CREATE INDEX IF NOT EXISTS idx_articles_lang ON articles(lang);
CREATE UNIQUE INDEX IF NOT EXISTS idx_articles_group_lang ON articles(translation_group_id, lang) WHERE status <> 'deleted' AND translation_group_id <> '';
`

// schemaV12 は MCP の OAuth 2.1（PKCE + Dynamic Client Registration）のスキーマ。
//
//   - oauth_clients: RFC 7591 で登録された公開クライアント（クライアントシークレットは持たない）。redirect_uris は
//     登録されたリダイレクト URI の JSON 配列で、認可要求の redirect_uri はこのどれかと完全に一致しなければならない。
//   - oauth_codes: 認可コード。コード自体は保存せず SHA-256 ハッシュ（code_hash）だけを持つ。PKCE の code_challenge
//     （S256）と、コードを使ったら記録する used_at で、使い捨てと横取り対策を行う。
//   - oauth_tokens: アクセストークンとリフレッシュトークンの組。どちらも SHA-256 ハッシュだけを保存する。
//     リフレッシュでローテーションした古い組や、管理画面で取り消した組は revoked_at を記録して無効にする。
//
// ユーザーを削除すると、そのユーザーのコードとトークンは外部キーの CASCADE で消える。
const schemaV12 = `
CREATE TABLE IF NOT EXISTS oauth_clients (
    client_id     TEXT PRIMARY KEY,
    client_name   TEXT NOT NULL DEFAULT '',
    redirect_uris TEXT NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_oauth_clients_created_at ON oauth_clients(created_at);

CREATE TABLE IF NOT EXISTS oauth_codes (
    code_hash             TEXT PRIMARY KEY,
    client_id             TEXT NOT NULL,
    user_id               TEXT NOT NULL,
    redirect_uri          TEXT NOT NULL,
    code_challenge        TEXT NOT NULL,
    code_challenge_method TEXT NOT NULL DEFAULT 'S256' CHECK(code_challenge_method IN ('S256')),
    scope                 TEXT NOT NULL DEFAULT '',
    resource              TEXT NOT NULL DEFAULT '',
    expires_at            DATETIME NOT NULL,
    used_at               DATETIME,
    FOREIGN KEY(client_id) REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_oauth_codes_client_id ON oauth_codes(client_id);
CREATE INDEX IF NOT EXISTS idx_oauth_codes_user_id ON oauth_codes(user_id);
CREATE INDEX IF NOT EXISTS idx_oauth_codes_expires_at ON oauth_codes(expires_at);

CREATE TABLE IF NOT EXISTS oauth_tokens (
    id                 TEXT PRIMARY KEY,
    token_hash         TEXT NOT NULL UNIQUE,
    refresh_token_hash TEXT UNIQUE,
    client_id          TEXT NOT NULL,
    user_id            TEXT NOT NULL,
    scope              TEXT NOT NULL DEFAULT '',
    resource           TEXT NOT NULL DEFAULT '',
    expires_at         DATETIME NOT NULL,
    refresh_expires_at DATETIME,
    revoked_at         DATETIME,
    created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at       DATETIME,
    FOREIGN KEY(client_id) REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_client_id ON oauth_tokens(client_id);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_user_id ON oauth_tokens(user_id);
`

// schemaV13 はメディアライブラリを撮影日（記事から取り込んだ画像は記事の日付）の順に並べるため、media に taken_at を足す。
// 手作業などで列が先に足されている DB でも失敗しないよう、列の追加・既存行の埋め戻し・索引は addMediaTakenAt
// （migrationHooks）で、列の有無を確かめてから行う。SQL の本体はない。
const schemaV13 = `SELECT 1;`

// schemaV14 は公開サーバーとの同期状態（remote_updated_at）と、PC の sorus での編集中の印
// （checked_out_by, checked_out_at, checked_out_client）を articles に足す。列の有無を確かめてから足すため、
// 列の追加は addArticleSyncState（migrationHooks）で行う。SQL の本体はない。
const schemaV14 = `SELECT 1;`

// schemaV15 は固定ページの階層（親ページ parent_id と並び順 sort_order）を articles に足す。列の有無を確かめてから
// 足すため、列の追加と索引 idx_articles_parent_sort は addPageHierarchy（migrationHooks）で行う。SQL の本体はない。
const schemaV15 = `SELECT 1;`
