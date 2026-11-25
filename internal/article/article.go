// Package article は記事モデルと CRUD のビジネスロジックを提供する。
// 保存時に Markdown から安全な HTML を生成してキャッシュし、スラッグの一意性を保証する。
package article

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jmoiron/sqlx"
	"golang.org/x/text/unicode/norm"

	"sorus/internal/markdown"
)

// Status は記事の公開状態。articles.status の CHECK 制約と対応する。
type Status string

const (
	StatusDraft  Status = "draft"  // 下書き
	StatusReview Status = "review" // 校閲中（執筆者が申請し、編集者・管理者の確認待ち）
	// StatusScheduled は保存する状態ではなく、予約中の記事（IsScheduled: 公開で、公開日時が未来）を指す仮想の値。
	// 管理画面の一覧の絞り込み（?status=scheduled）と状態の表示（ViewStatus）、MCP の status の指定に使う。
	// articles.status には保存しない（Save は ErrInvalid を返す）。
	StatusScheduled Status = "scheduled"
	StatusPublished Status = "published" // 公開（公開日時を迎えるまでは予約中）
	StatusDeleted   Status = "deleted"   // ゴミ箱（論理削除）
)

// VisibleCondition は読者に見える記事（IsVisible）の WHERE 条件。articles の別名は a、プレースホルダは
// 状態（StatusPublished）・現在時刻の順。日時は保存形式の違いに左右されないよう julianday で比較する。
// 公開ページ・一覧・フィード・静的な書き出し・RAG など、読者に見せるものはすべてこの条件で絞る。
const VisibleCondition = `a.status = ? AND a.published_at IS NOT NULL AND julianday(a.published_at) <= julianday(?)`

// ScheduledCondition は予約中の記事（IsScheduled）の WHERE 条件。別名・プレースホルダは VisibleCondition と同じ。
const ScheduledCondition = `a.status = ? AND a.published_at IS NOT NULL AND julianday(a.published_at) > julianday(?)`

// IsScheduled は a が予約中か（公開で、公開日時が now より後か）を返す。予約中の記事は公開日時まで読者に見えず、
// 変更できるのは公開中の記事と同じく編集者・管理者だけ。SQL では ScheduledCondition。
func IsScheduled(a *Article, now time.Time) bool {
	return a != nil && a.Status == StatusPublished && a.PublishedAt != nil && a.PublishedAt.After(now)
}

// IsVisible は a が読者に見えるか（公開で、公開日時が now 以前か）を返す。SQL では VisibleCondition。
func IsVisible(a *Article, now time.Time) bool {
	return a != nil && a.Status == StatusPublished && a.PublishedAt != nil && !a.PublishedAt.After(now)
}

// ViewStatus は画面・API に出す a の状態を返す。予約中なら StatusScheduled、それ以外は保存した状態。
func ViewStatus(a *Article, now time.Time) Status {
	if IsScheduled(a, now) {
		return StatusScheduled
	}
	return a.Status
}

// StatusCondition は ViewStatus が st になる記事の WHERE 条件（articles の別名 a）と引数を返す。
// 予約投稿は ScheduledCondition、公開中は予約中を除いた公開記事、それ以外は保存した状態の一致。
func StatusCondition(st Status, now time.Time) (string, []any) {
	switch st {
	case StatusScheduled:
		return ScheduledCondition, []any{StatusPublished, now}
	case StatusPublished:
		return `a.status = ? AND NOT (` + ScheduledCondition + `)`, []any{StatusPublished, StatusPublished, now}
	}
	return `a.status = ?`, []any{st}
}

// Kind は記事の種類。articles.kind の CHECK 制約と対応する。
// ページは記事と同じく執筆・校閲・版の管理・予約公開を行い、公開サイトでの出し方だけが異なる
// （/<slug> で配信し、時系列の一覧・カテゴリ・RSS・関連記事には出さない）。
type Kind string

const (
	KindPost Kind = "post" // 記事（時系列の一覧に出す。/articles/<slug>）
	KindPage Kind = "page" // 固定ページ（研究紹介・メンバーなど。/<slug>）
)

// Valid は k が既知の種類かを返す。空文字は無効（保存時は KindPost とみなす）。
func (k Kind) Valid() bool {
	return k == KindPost || k == KindPage
}

// reservedPageSlugs はページのスラッグに使えない語。公開サイトの固定のパスの先頭部分と重なり、
// /<slug> で配信するとそのパスを隠してしまう。同期 API のパスの先頭部分のように設定で変わる語は
// WithReservedSlugs で Service に加え、Service.IsReservedPageSlug で両方を検査する。
var reservedPageSlugs = map[string]bool{
	"admin": true, "api": true, "articles": true, "categories": true, "search": true, "static": true,
	"uploads": true, "attachments": true, "og": true, "og-image": true, "feed.xml": true, "sitemap.xml": true,
	"robots.txt": true, "favicon.ico": true, "llms.txt": true, "llms-full.txt": true, "manifest.json": true,
	"sw.js": true, "healthz": true, "en": true,
	// デスクトップモードの開いている画面の接続（server.WindowPath）。
	"sorus-window": true,
	// 取り込んだカスタムテーマのファイル（server.CustomThemeURLPrefix）。
	"custom-themes": true,
	// MCP の OAuth 2.1 のエンドポイント（/oauth/authorize など）と、そのディスカバリー（/.well-known/oauth-*）。
	"oauth": true, ".well-known": true,
}

// IsReservedPageSlug は slug がページのスラッグに使えない固定の予約語か（大文字・小文字を区別しない）を返す。
// 設定で加わる語（WithReservedSlugs）は含まない。保存の可否は Service.IsReservedPageSlug で判定する。
func IsReservedPageSlug(slug string) bool {
	return reservedPageSlugs[strings.ToLower(strings.TrimSpace(slug))]
}

// PathReservedSlug は公開サイトのパス p（例: 同期 API の /hooks/sync）を隠さないよう、ページに使えない
// スラッグとして p の先頭部分（例: hooks）を返す。先頭部分が無ければ空文字を返す。
func PathReservedSlug(p string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(p), "/"), "/")
	return first
}

// idBytes は記事IDの乱数バイト長。hex で32文字になる。
const idBytes = 16

// fallbackSlug はタイトル・スラッグから有効な文字が得られないときのスラッグ。
const fallbackSlug = "article"

var (
	// ErrNotFound は記事が存在しないことを示す。
	ErrNotFound = errors.New("article: not found")
	// ErrInvalid は保存しようとした記事の内容が不正であることを示す。
	ErrInvalid = errors.New("article: invalid article")
	// ErrExists は Create で指定した ID の記事が既に存在することを示す。
	ErrExists = errors.New("article: already exists")
	// ErrReservedSlug はページのスラッグが予約語（Service.IsReservedPageSlug）であることを示す。
	// Save・Create は ErrInvalid とあわせて包んで返す。
	ErrReservedSlug = errors.New("article: reserved page slug")
	// ErrPublished は読者に見えたことのある記事（公開日時が現在以前）を予約にしようとしたことを示す。
	// 一度公開した記事を見えなくする事故を防ぐ。Save・Create は ErrInvalid とあわせて包んで返す。
	ErrPublished = errors.New("article: already published")
	// ErrNotInTrash はゴミ箱にない（状態が deleted でない）記事を復元・完全削除しようとしたことを示す。
	ErrNotInTrash = errors.New("article: not in trash")
	// ErrRestoreConflict はゴミ箱の記事を戻すと、同じ翻訳グループの同じ言語の記事が2つになることを示す
	// （idx_articles_group_lang はゴミ箱の記事を除いて一意）。
	ErrRestoreConflict = errors.New("article: another article already uses this translation")
)

// TrashRetention はゴミ箱に入れた記事を自動で完全削除するまでの期間（30日）。
const TrashRetention = 30 * 24 * time.Hour

// Article は articles テーブルの1行。
type Article struct {
	ID              string     `db:"id" json:"id"`
	Title           string     `db:"title" json:"title"`
	Slug            string     `db:"slug" json:"slug"`
	ContentMarkdown string     `db:"content_markdown" json:"content_markdown"`
	ContentHTML     string     `db:"content_html" json:"content_html"`
	Excerpt         string     `db:"excerpt" json:"excerpt"`
	FeaturedImage   string     `db:"featured_image_url" json:"featured_image_url"`
	AuthorID        string     `db:"author_id" json:"author_id"`
	CategoryID      int64      `db:"category_id" json:"category_id"`
	Status          Status     `db:"status" json:"status"`
	LockedBy        *string    `db:"locked_by" json:"locked_by"`
	LockedAt        *time.Time `db:"locked_at" json:"locked_at"`
	// PublishedAt は公開日時。公開の記事では、この日時から読者に見える（未来なら予約中）。
	PublishedAt *time.Time `db:"published_at" json:"published_at"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
	// Kind は記事の種類。空なら保存時に KindPost とする。
	Kind Kind `db:"kind" json:"kind"`
	// 以下は論文・学会発表などの学術メタデータ。該当しない記事では空文字。
	DOI            string `db:"doi" json:"doi"`
	JournalName    string `db:"journal_name" json:"journal_name"`
	ConferenceName string `db:"conference_name" json:"conference_name"`
	CoAuthors      string `db:"co_authors" json:"co_authors"`

	// 多言語化・翻訳クラスタ
	Lang               string         `db:"lang" json:"lang"`
	TranslationGroupID string         `db:"translation_group_id" json:"translation_group_id"`
	SourceArticleID    sql.NullString `db:"source_article_id" json:"source_article_id,omitempty"`
	IsOutdated         bool           `db:"is_outdated" json:"is_outdated"`

	// 公開サーバーとの同期状態。Save では書き換えず、SetRemoteUpdatedAt・SetCheckout・ClearCheckout で更新する。
	// RemoteUpdatedAt は公開サーバー側で最後に更新された日時。
	RemoteUpdatedAt *time.Time `db:"remote_updated_at" json:"remote_updated_at,omitempty"`
	// CheckedOutBy・CheckedOutAt は PC の sorus でこの記事を編集中の人（表示名）と、その開始日時。
	CheckedOutBy string     `db:"checked_out_by" json:"checked_out_by,omitempty"`
	CheckedOutAt *time.Time `db:"checked_out_at" json:"checked_out_at,omitempty"`
	// CheckedOutClient は編集中の印を付けた PC の sorus の識別子（remote_client_id）。同じ人・同じトークンを
	// 別の PC で使っていても、どの PC で編集中かを見分ける。識別子を送らない古い PC の印では空。
	CheckedOutClient string `db:"checked_out_client" json:"checked_out_client,omitempty"`
}

// CheckoutTTL はチェックアウト（PC の sorus での編集中の印）の有効期間。これを過ぎた印は無視する。
const CheckoutTTL = 24 * time.Hour

// IsCheckedOut は now の時点で記事が PC の sorus で編集中（有効なチェックアウトがある）かを返す。
// 印を付けてから CheckoutTTL 以上たったものは、付けたまま終わったとみなして編集中としない。
func (a *Article) IsCheckedOut(now time.Time) bool {
	return a.CheckedOutBy != "" && a.CheckedOutAt != nil && now.Sub(*a.CheckedOutAt) < CheckoutTTL
}

// URL は言語設定と記事種類に応じた公開 URL パスを返す。
// 英語記事（lang == "en"）の場合は /en プレフィックスが付与される。
func (a *Article) URL() string {
	prefix := ""
	if strings.ToLower(strings.TrimSpace(a.Lang)) == "en" {
		prefix = "/en"
	}
	if a.Kind == KindPage {
		return prefix + "/" + url.PathEscape(a.Slug)
	}
	return prefix + "/articles/" + url.PathEscape(a.Slug)
}

// columns は SELECT で取得する articles の列。Article のフィールドと対応する。
// articles.scheduled_at は使っていない（予約は公開日時が未来の公開記事で表す）。
const columns = `id, title, slug, content_markdown, content_html, excerpt, featured_image_url,
	author_id, category_id, status, locked_by, locked_at, published_at, created_at, updated_at,
	doi, journal_name, conference_name, co_authors, kind,
	lang, translation_group_id, source_article_id, is_outdated,
	remote_updated_at, checked_out_by, checked_out_at, checked_out_client`

// Service は記事の CRUD を行う。並行に使用してよい。
type Service struct {
	db *sqlx.DB
	// reserved は固定の予約語に加えてページのスラッグに使えない語（小文字）。
	reserved map[string]bool
}

// Option は NewService の設定。
type Option func(*Service)

// WithReservedSlugs は words を、固定の予約語に加えてページのスラッグに使えない語にする
// （大文字・小文字を区別しない。空文字は無視する）。同期 API のパスの先頭部分（PathReservedSlug）など、
// サーバーの設定で変わる語に使う。
func WithReservedSlugs(words ...string) Option {
	return func(s *Service) {
		for _, w := range words {
			if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
				if s.reserved == nil {
					s.reserved = map[string]bool{}
				}
				s.reserved[w] = true
			}
		}
	}
}

// NewService は db を使う Service を返す。db は database.Migrate 適用済みであること。
// opts を省略すると、ページのスラッグは固定の予約語（IsReservedPageSlug）だけを拒否する。
func NewService(db *sqlx.DB, opts ...Option) *Service {
	s := &Service{db: db}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// IsReservedPageSlug は slug がページのスラッグに使えないか（固定の予約語か、WithReservedSlugs で加えた語か。
// 大文字・小文字を区別しない）を返す。Save・Create と同じ規則で、保存前の検査や公開前の再検査に使う。
func (s *Service) IsReservedPageSlug(slug string) bool {
	return IsReservedPageSlug(slug) || s.reserved[strings.ToLower(strings.TrimSpace(slug))]
}

// Save は記事の新規作成または更新を行う。自動的に Markdown から HTML を生成してキャッシュする。
// スラッグが既存の記事と重複する場合は、自動で連番 (-2, -3) を付与して一意性を確保する。
//
// ID が空なら新しい ID を発行して作成し、ID の記事が存在すれば更新、存在しなければその ID で作成する。
// Slug が空ならタイトルから生成する。Kind が空なら post とし、ページのスラッグが予約語なら
// ErrReservedSlug（ErrInvalid を兼ねる）を返す。Status が空なら draft とし、published で PublishedAt が
// nil なら現在時刻を公開日時とする。予約は published と未来の PublishedAt で表し、StatusScheduled は保存できない。
// published 以外の記事の未来の PublishedAt は、まだ公開していない予定の日時のため消す（予約の取り消し）。
// 読者に見えたことのある記事（保存済みの公開日時が現在以前）を予約にしようとすると ErrPublished（ErrInvalid を兼ねる）を返す。
// 更新時の CreatedAt は保存済みの値を維持する。
// 成功すると a に保存後の値（ID、Slug、ContentHTML、日時など）を反映する。失敗時は a を変更しない。
func (s *Service) Save(a *Article) error {
	return s.save(a, false)
}

// Create は ID の記事が存在しない場合に限り、Save と同じく記事を作成する。ID の記事が既にあれば
// 更新せずに ErrExists を返す。存在を確かめてから保存するまでの間に、同じ ID の記事が並行して
// 作成された場合に上書きしないよう、存在の確認と作成を1つの書き込みトランザクションで行う。
func (s *Service) Create(a *Article) error {
	return s.save(a, true)
}

// save は Save・Create の本体。createOnly なら既存の記事を更新せずに ErrExists を返す。
func (s *Service) save(a *Article, createOnly bool) error {
	if a == nil {
		return fmt.Errorf("%w: nil article", ErrInvalid)
	}
	next := *a
	if strings.TrimSpace(next.Title) == "" {
		return fmt.Errorf("%w: empty title", ErrInvalid)
	}
	if next.Status == "" {
		next.Status = StatusDraft
	}
	if next.Status == StatusScheduled {
		return fmt.Errorf("%w: status %q is not stored (use published with a future published_at)", ErrInvalid, next.Status)
	}
	if !next.Status.valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalid, next.Status)
	}
	if next.Kind == "" {
		next.Kind = KindPost
	}
	if !next.Kind.Valid() {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, next.Kind)
	}
	lang := strings.ToLower(strings.TrimSpace(next.Lang))
	if lang == "" {
		lang = "ja"
	}
	if lang != "ja" && lang != "en" {
		return fmt.Errorf("%w: unsupported language %q (must be 'ja' or 'en')", ErrInvalid, next.Lang)
	}
	next.Lang = lang

	rendered, err := markdown.Render(next.ContentMarkdown)
	if err != nil {
		return fmt.Errorf("article: save: %w", err)
	}
	next.ContentHTML = rendered.HTML

	if next.ID == "" {
		if next.ID, err = newID(); err != nil {
			return fmt.Errorf("article: save: %w", err)
		}
	}
	if strings.TrimSpace(next.TranslationGroupID) == "" {
		next.TranslationGroupID = next.ID
	}
	requestedSlug := next.Slug
	base := requestedSlug
	if strings.TrimSpace(base) == "" {
		base = next.Title
	}
	base = normalizeSlug(base)
	if next.Kind == KindPage && s.IsReservedPageSlug(base) {
		return fmt.Errorf("%w: %w: %q", ErrInvalid, ErrReservedSlug, base)
	}

	now := time.Now().UTC()
	next.UpdatedAt = now
	if next.Status == StatusPublished && next.PublishedAt == nil {
		next.PublishedAt = &now
	}
	if next.Status != StatusPublished && next.PublishedAt != nil && next.PublishedAt.After(now) {
		next.PublishedAt = nil
	}
	next.PublishedAt = utcPtr(next.PublishedAt)
	next.LockedAt = utcPtr(next.LockedAt)

	if err := s.write(&next, base, requestedSlug, now, createOnly); err != nil {
		return fmt.Errorf("article: save: %w", err)
	}
	*a = next
	return nil
}

// write はスラッグの決定と INSERT/UPDATE を1つの書き込みトランザクションで行う。
// BEGIN IMMEDIATE で書き込みロックを先に取り、並行保存でのスラッグ競合を防ぐ。
// createOnly なら、記事が既にあれば書き込まずに ErrExists を返す。
func (s *Service) write(a *Article, baseSlug, requestedSlug string, now time.Time, createOnly bool) error {
	ctx := context.Background()
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var stored struct {
		CreatedAt   time.Time  `db:"created_at"`
		PublishedAt *time.Time `db:"published_at"`
		Slug        string     `db:"slug"`
	}
	err = conn.GetContext(ctx, &stored, `SELECT created_at, published_at, slug FROM articles WHERE id = ?`, a.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("find article: %w", err)
	}
	if exists && createOnly {
		return ErrExists
	}
	if exists && stored.PublishedAt != nil && !stored.PublishedAt.After(now) && IsScheduled(a, now) {
		return fmt.Errorf("%w: %w", ErrInvalid, ErrPublished)
	}
	if exists {
		a.CreatedAt = stored.CreatedAt
		if !createOnly && strings.TrimSpace(requestedSlug) != "" && requestedSlug == stored.Slug {
			baseSlug = stored.Slug
		}
	} else if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	} else {
		a.CreatedAt = a.CreatedAt.UTC()
	}
	if a.Kind == KindPage && s.IsReservedPageSlug(baseSlug) {
		return fmt.Errorf("%w: %w: %q", ErrInvalid, ErrReservedSlug, baseSlug)
	}

	if a.Slug, err = uniqueSlug(ctx, conn, baseSlug, a.ID); err != nil {
		return err
	}

	query := `INSERT INTO articles (id, title, slug, content_markdown, content_html, excerpt, featured_image_url,
			author_id, category_id, status, locked_by, locked_at, published_at, created_at, updated_at,
			doi, journal_name, conference_name, co_authors, kind,
			lang, translation_group_id, source_article_id, is_outdated)
		VALUES (:id, :title, :slug, :content_markdown, :content_html, :excerpt, :featured_image_url,
			:author_id, :category_id, :status, :locked_by, :locked_at, :published_at, :created_at, :updated_at,
			:doi, :journal_name, :conference_name, :co_authors, :kind,
			:lang, :translation_group_id, :source_article_id, :is_outdated)`
	if exists {
		query = `UPDATE articles SET title = :title, slug = :slug, content_markdown = :content_markdown,
			content_html = :content_html, excerpt = :excerpt, featured_image_url = :featured_image_url,
			author_id = :author_id, category_id = :category_id, status = :status, locked_by = :locked_by,
			locked_at = :locked_at, published_at = :published_at, updated_at = :updated_at,
			doi = :doi, journal_name = :journal_name,
			conference_name = :conference_name, co_authors = :co_authors, kind = :kind,
			lang = :lang, translation_group_id = :translation_group_id,
			source_article_id = :source_article_id, is_outdated = :is_outdated
		WHERE id = :id`
	}
	q, args, err := sqlx.Named(query, a)
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	if _, err := conn.ExecContext(ctx, conn.Rebind(q), args...); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// uniqueSlug は base が他の記事（論理削除済みを含む）と重複しなければそのまま、
// 重複すれば base-2, base-3, ... のうち最初の空きを返す。selfID の記事自身は重複とみなさない。
func uniqueSlug(ctx context.Context, conn *sqlx.Conn, base, selfID string) (string, error) {
	var taken []string
	err := conn.SelectContext(ctx, &taken, `SELECT slug FROM articles
		WHERE (slug = ? OR slug LIKE ? ESCAPE '\') AND id <> ?`, base, escapeLike(base)+"-%", selfID)
	if err != nil {
		return "", fmt.Errorf("find slugs: %w", err)
	}
	used := make(map[string]bool, len(taken))
	for _, t := range taken {
		used[t] = true
	}
	slug := base
	for n := 2; used[slug]; n++ {
		slug = base + "-" + strconv.Itoa(n)
	}
	return slug, nil
}

// GetByID は ID で記事を取得する。論理削除済みの記事も返す。存在しなければ ErrNotFound を返す。
func (s *Service) GetByID(id string) (*Article, error) {
	return s.getOne(`SELECT `+columns+` FROM articles WHERE id = ?`, id)
}

// GetBySlug はスラッグで記事を取得する。状態を問わず返すため、公開ページでは Status を確認すること。
// 存在しなければ ErrNotFound を返す。
func (s *Service) GetBySlug(slug string) (*Article, error) {
	return s.getOne(`SELECT `+columns+` FROM articles WHERE slug = ?`, slug)
}

func (s *Service) getOne(query string, arg string) (*Article, error) {
	var a Article
	err := s.db.Get(&a, query, arg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("article: get: %w", err)
	}
	return &a, nil
}

// SoftDelete は記事を論理削除 (status = 'deleted') する。予約中の記事の未来の公開日時は、Save と同じく消す。
// 削除済みの記事に対しても成功する。存在しなければ ErrNotFound を返す。
func (s *Service) SoftDelete(id string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE articles SET status = ?, updated_at = ?,
			published_at = CASE WHEN julianday(published_at) > julianday(?) THEN NULL ELSE published_at END
		WHERE id = ?`, StatusDeleted, now, now, id)
	if err != nil {
		return fmt.Errorf("article: soft delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("article: soft delete: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeResult は完全削除（HardDelete・EmptyTrash・PurgeExpiredTrash）の結果。
type PurgeResult struct {
	// IDs は完全削除した記事の ID。
	IDs []string
	// AttachmentFiles は削除した記事の添付資料の保存ファイル名（attachments.file_path）。DB の行は記事とともに
	// 消えるため、呼び出し側が添付資料の保存先からファイルを消す。
	AttachmentFiles []string
}

// RestoreFromTrash はゴミ箱の記事 id を下書きに戻す。公開日時は残すが、下書きのため読者には見えない。
// 存在しなければ ErrNotFound、ゴミ箱になければ ErrNotInTrash、同じ翻訳グループの同じ言語の記事が既にあれば
// ErrRestoreConflict を返す。
func (s *Service) RestoreFromTrash(id string) error {
	res, err := s.db.Exec(`UPDATE articles SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		StatusDraft, time.Now().UTC(), id, StatusDeleted)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("article: restore: %w", ErrRestoreConflict)
		}
		return fmt.Errorf("article: restore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("article: restore: %w", err)
	}
	if n == 0 {
		return s.trashState(id)
	}
	return nil
}

// trashState は記事 id がゴミ箱にない理由（ErrNotFound か ErrNotInTrash）を返す。
func (s *Service) trashState(id string) error {
	var st Status
	err := s.db.Get(&st, `SELECT status FROM articles WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("article: find status: %w", err)
	}
	if st != StatusDeleted {
		return ErrNotInTrash
	}
	return nil
}

// HardDelete はゴミ箱の記事 id を物理削除する。版の履歴・コメント・添付資料・埋め込み・閲覧数の行は外部キーの
// ON DELETE CASCADE で消え、この記事を翻訳元にしていた記事の source_article_id は NULL にする。
// 添付資料のファイルは消さないため、結果の AttachmentFiles を呼び出し側が消す。
// 存在しなければ ErrNotFound、ゴミ箱になければ ErrNotInTrash を返す（公開中の記事を誤って消さない）。
func (s *Service) HardDelete(id string) (PurgeResult, error) {
	res, err := s.purge(`id = ?`, id)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("article: hard delete: %w", err)
	}
	if len(res.IDs) == 0 {
		if err := s.trashState(id); err != nil {
			return PurgeResult{}, err
		}
		return PurgeResult{}, ErrNotFound
	}
	return res, nil
}

// EmptyTrash はゴミ箱のすべての記事を HardDelete と同じく物理削除する。
func (s *Service) EmptyTrash() (PurgeResult, error) {
	res, err := s.purge(`1 = 1`)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("article: empty trash: %w", err)
	}
	return res, nil
}

// PurgeExpiredTrash はゴミ箱に入れてから olderThan 以上たった記事を物理削除する。自動削除は TrashRetention を渡す。
func (s *Service) PurgeExpiredTrash(olderThan time.Duration) (PurgeResult, error) {
	return s.PurgeTrashBefore(time.Now().UTC().Add(-olderThan))
}

// PurgeTrashBefore は cutoff 以前にゴミ箱に入れた記事を物理削除する。ゴミ箱に入れた日時は、ゴミ箱の記事の
// 更新日時（SoftDelete が更新する。ゴミ箱の記事を保存すると下書きに戻るため、ゴミ箱にある間は変わらない）で表す。更新日時が後から進んでも、削除が
// 遅れるだけで早まることはない。
func (s *Service) PurgeTrashBefore(cutoff time.Time) (PurgeResult, error) {
	res, err := s.purge(`julianday(updated_at) <= julianday(?)`, cutoff.UTC())
	if err != nil {
		return PurgeResult{}, fmt.Errorf("article: purge expired trash: %w", err)
	}
	return res, nil
}

// purge は、ゴミ箱の記事のうち cond（articles の列の WHERE 条件）に当てはまるものを1つの書き込みトランザクションで
// 物理削除する。
func (s *Service) purge(cond string, args ...any) (PurgeResult, error) {
	ctx := context.Background()
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return PurgeResult{}, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var res PurgeResult
	where := `status = ? AND (` + cond + `)`
	params := append([]any{StatusDeleted}, args...)
	if err := conn.SelectContext(ctx, &res.IDs, `SELECT id FROM articles WHERE `+where+` ORDER BY id`, params...); err != nil {
		return PurgeResult{}, fmt.Errorf("find trash: %w", err)
	}
	if len(res.IDs) == 0 {
		return PurgeResult{}, nil
	}
	sub := `SELECT id FROM articles WHERE ` + where
	if err := conn.SelectContext(ctx, &res.AttachmentFiles,
		`SELECT file_path FROM attachments WHERE article_id IN (`+sub+`) ORDER BY file_path`, params...); err != nil {
		return PurgeResult{}, fmt.Errorf("find attachments: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE articles SET source_article_id = NULL
		WHERE source_article_id IN (`+sub+`)`, params...); err != nil {
		return PurgeResult{}, fmt.Errorf("detach translations: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM articles WHERE `+where, params...); err != nil {
		return PurgeResult{}, fmt.Errorf("delete: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return PurgeResult{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return res, nil
}

// ListPublished は公開日時を迎えた公開記事（kind=post）の一覧を取得する。ページは時系列の一覧に含めない。
// 公開日時の新しい順に、offset 件を飛ばして最大 limit 件を返す。該当がなければ空スライスを返す。
func (s *Service) ListPublished(limit, offset int) ([]*Article, error) {
	if limit <= 0 || offset < 0 {
		return nil, fmt.Errorf("article: list published: invalid limit %d or offset %d", limit, offset)
	}
	articles := []*Article{}
	err := s.db.Select(&articles, `SELECT `+columns+` FROM articles a
		WHERE a.kind = ? AND `+VisibleCondition+`
		ORDER BY julianday(a.published_at) DESC, a.id
		LIMIT ? OFFSET ?`, KindPost, StatusPublished, time.Now().UTC(), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("article: list published: %w", err)
	}
	return articles, nil
}

// Translations は同一 translation_group_id を持つすべての言語の記事一覧を取得する（言語コード昇順）。
// 該当する記事がない場合は空スライスを返す。
func (s *Service) Translations(ctx context.Context, groupID string) ([]Article, error) {
	if strings.TrimSpace(groupID) == "" {
		return []Article{}, nil
	}
	var articles []Article
	err := s.db.SelectContext(ctx, &articles, `SELECT `+columns+` FROM articles
		WHERE translation_group_id = ?
		ORDER BY lang ASC, created_at ASC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("article: list translations: %w", err)
	}
	if articles == nil {
		articles = []Article{}
	}
	return articles, nil
}

// Translation は特定の翻訳グループにおける指定言語の翻訳記事を1件取得する。
// 存在しない場合は ErrNotFound を返す。
func (s *Service) Translation(ctx context.Context, groupID string, lang string) (*Article, error) {
	if strings.TrimSpace(groupID) == "" || strings.TrimSpace(lang) == "" {
		return nil, ErrNotFound
	}
	var a Article
	err := s.db.GetContext(ctx, &a, `SELECT `+columns+` FROM articles
		WHERE translation_group_id = ? AND lang = ?
		LIMIT 1`, groupID, lang)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("article: get translation: %w", err)
	}
	return &a, nil
}

// MarkTranslationsOutdated は原文記事が更新された際に、同一グループ内の他の翻訳記事の is_outdated を 1 に更新する。
// exceptID で指定された記事（更新された原文記事自身）は対象外とする。
func (s *Service) MarkTranslationsOutdated(ctx context.Context, groupID string, exceptID string) error {
	if strings.TrimSpace(groupID) == "" {
		return nil
	}
	query := `UPDATE articles SET is_outdated = 1 WHERE translation_group_id = ? AND id <> ?`
	if _, err := s.db.ExecContext(ctx, query, groupID, exceptID); err != nil {
		return fmt.Errorf("article: mark translations outdated: %w", err)
	}
	return nil
}

// ClearOutdated は翻訳が再確認・更新された際に is_outdated を 0 にリセットする。
// 記事が存在しない場合は ErrNotFound を返す。
func (s *Service) ClearOutdated(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE articles SET is_outdated = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("article: clear outdated: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("article: clear outdated: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRemoteUpdatedAt は記事の公開サーバー側の更新日時 remote_updated_at を t にする。
// 記事が存在しない場合は ErrNotFound を返す。
func (s *Service) SetRemoteUpdatedAt(ctx context.Context, id string, t time.Time) error {
	return s.updateSyncState(ctx, "set remote updated at",
		`UPDATE articles SET remote_updated_at = ? WHERE id = ?`, t.UTC(), id)
}

// AdoptSlug は公開サーバーが付け直したスラッグ slug を記事に写す。updated_at は変えない（ローカルの編集ではないため）。
// 空のスラッグは ErrInvalid、記事が存在しない場合は ErrNotFound、ほかの記事と重なれば DB のエラーを返す。
func (s *Service) AdoptSlug(ctx context.Context, id, slug string) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return fmt.Errorf("article: adopt slug: %w: empty slug", ErrInvalid)
	}
	return s.updateSyncState(ctx, "adopt slug", `UPDATE articles SET slug = ? WHERE id = ?`, slug, id)
}

// SetCheckout は記事に PC の sorus で編集中の印（by が PC clientID の sorus で at から編集中）を付ける。
// clientID は空でもよい（識別子を送らない古い PC）。updated_at は変えない。記事が存在しない場合は ErrNotFound を返す。
func (s *Service) SetCheckout(ctx context.Context, id, by, clientID string, at time.Time) error {
	by = strings.TrimSpace(by)
	if by == "" {
		return fmt.Errorf("article: set checkout: %w: empty checked_out_by", ErrInvalid)
	}
	return s.updateSyncState(ctx, "set checkout",
		`UPDATE articles SET checked_out_by = ?, checked_out_client = ?, checked_out_at = ? WHERE id = ?`,
		by, strings.TrimSpace(clientID), at.UTC(), id)
}

// ClearCheckout は記事の編集中の印を外す。記事が存在しない場合は ErrNotFound を返す。
func (s *Service) ClearCheckout(ctx context.Context, id string) error {
	return s.updateSyncState(ctx, "clear checkout",
		`UPDATE articles SET checked_out_by = '', checked_out_client = '', checked_out_at = NULL WHERE id = ?`, id)
}

// CheckoutState は記事の編集中の印（編集者の名前・PC の識別子・開始日時）だけを読む。印が無ければ by・client は空で at は nil。
// 記事が存在しない場合は ErrNotFound を返す。
func (s *Service) CheckoutState(ctx context.Context, id string) (by, client string, at *time.Time, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT checked_out_by, checked_out_client, checked_out_at FROM articles WHERE id = ?`, id).Scan(&by, &client, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil, ErrNotFound
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("article: checkout state: %w", err)
	}
	return by, client, at, nil
}

// updateSyncState は同期状態の列を1件更新し、対象の記事が無ければ ErrNotFound を返す。
func (s *Service) updateSyncState(ctx context.Context, op, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("article: %s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("article: %s: %w", op, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// valid は st が articles.status に保存できる状態かを返す。仮想の値 StatusScheduled は含めない。
func (st Status) valid() bool {
	switch st {
	case StatusDraft, StatusReview, StatusPublished, StatusDeleted:
		return true
	}
	return false
}

// NormalizeSlug は normalizeSlug を公開したもの。インポーター等が記事・カテゴリのスラッグを
// 管理画面での保存と同じ規則で生成するために使う。
func NormalizeSlug(s string) string { return normalizeSlug(s) }

// normalizeSlug は s を URL 用のスラッグに変換する。
// 英字は小文字にし、日本語などの Unicode 文字・数字は残す。空白・"-"・"_" の連続は1つの "-" にし、
// その他の記号は除去する。有効な文字が残らなければ fallbackSlug を返す。
// NFD の結合文字（濁点など）が記号として落ちないよう、先に NFC へ正規化する。
func normalizeSlug(s string) string {
	var sb strings.Builder
	pendingDash := false
	for _, r := range strings.ToLower(norm.NFC.String(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			if pendingDash && sb.Len() > 0 {
				sb.WriteByte('-')
			}
			pendingDash = false
			sb.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '_':
			pendingDash = true
		}
	}
	if sb.Len() == 0 {
		return fallbackSlug
	}
	return sb.String()
}

// escapeLike は s を LIKE パターン中のリテラルとして扱えるようエスケープする（ESCAPE '\'）。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// newID は crypto/rand による16バイトの乱数を hex で返す。
func newID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate article id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
