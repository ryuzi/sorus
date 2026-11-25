// Package revision は記事のリビジョン（版）履歴を提供する。
// 保存・校閲申請・公開のたびにタイトルと本文のスナップショットを記録し、
// 過去の版の一覧表示・差分表示・復元に使えるようにする。
package revision

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
)

// MaxRevisions は記事ごとに保持するリビジョンの上限。超えた分は古い順に削除する。
const MaxRevisions = 50

// idBytes はリビジョンIDの乱数バイト長。hex で32文字になる。
const idBytes = 16

var (
	// ErrNotFound はリビジョンが存在しないことを示す。
	ErrNotFound = errors.New("revision: not found")
	// ErrArticleNotFound はスナップショット対象の記事が存在しないことを示す。
	ErrArticleNotFound = errors.New("revision: article not found")
	// ErrInvalid はスナップショットの入力が不正であることを示す。
	ErrInvalid = errors.New("revision: invalid snapshot")
)

// Revision は article_revisions テーブルの1行。RevisionNum は記事ごとに 1 から振る連番。
type Revision struct {
	ID              string    `db:"id" json:"id"`
	ArticleID       string    `db:"article_id" json:"article_id"`
	RevisionNum     int       `db:"revision_num" json:"revision_num"`
	Title           string    `db:"title" json:"title"`
	ContentMarkdown string    `db:"content_markdown" json:"content_markdown"`
	AuthorID        string    `db:"author_id" json:"author_id"`
	Comment         string    `db:"comment" json:"comment"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

// columns は SELECT で取得する article_revisions の列。Revision のフィールドと対応する。
const columns = `id, article_id, revision_num, title, content_markdown, author_id, comment, created_at`

// Service はリビジョンの記録と取得を行う。並行に使用してよい。
type Service struct {
	db *sqlx.DB
	// now は現在時刻を返す。テストで時刻を固定するために差し替える。
	now func() time.Time
}

// NewService は db を使う Service を返す。db は database.Migrate 適用済みであること。
func NewService(db *sqlx.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// SaveSnapshot は記事 a のタイトルと本文を新しいリビジョンとして記録する。
// authorID はこの版を保存したユーザー、comment は版に付けるメモ（「校閲申請」など、空でもよい）。
//
// 最新リビジョンとタイトル・本文が同じなら記録せず、最新リビジョンと created=false を返す。
// 記録すると created=true を返し、記事のリビジョンが MaxRevisions を超えた分を古い順に削除する。
// a.ID の記事が存在しなければ ErrArticleNotFound を返す。
func (s *Service) SaveSnapshot(ctx context.Context, a *article.Article, authorID, comment string) (rev *Revision, created bool, err error) {
	if a == nil || a.ID == "" {
		return nil, false, fmt.Errorf("%w: missing article id", ErrInvalid)
	}
	if authorID == "" {
		return nil, false, fmt.Errorf("%w: missing author id", ErrInvalid)
	}

	err = s.withWriteTx(ctx, func(conn *sqlx.Conn, now time.Time) error {
		var exists bool
		if err := conn.GetContext(ctx, &exists, `SELECT EXISTS (SELECT 1 FROM articles WHERE id = ?)`, a.ID); err != nil {
			return fmt.Errorf("find article: %w", err)
		}
		if !exists {
			return ErrArticleNotFound
		}

		var latest Revision
		err := conn.GetContext(ctx, &latest, `SELECT `+columns+` FROM article_revisions
			WHERE article_id = ? ORDER BY revision_num DESC LIMIT 1`, a.ID)
		hasLatest := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find latest revision: %w", err)
		}
		if hasLatest && latest.Title == a.Title && latest.ContentMarkdown == a.ContentMarkdown {
			rev = &latest
			return nil
		}

		id, err := newID()
		if err != nil {
			return err
		}
		next := &Revision{
			ID:              id,
			ArticleID:       a.ID,
			RevisionNum:     latest.RevisionNum + 1,
			Title:           a.Title,
			ContentMarkdown: a.ContentMarkdown,
			AuthorID:        authorID,
			Comment:         comment,
			CreatedAt:       now,
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO article_revisions (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			next.ID, next.ArticleID, next.RevisionNum, next.Title, next.ContentMarkdown, next.AuthorID, next.Comment, next.CreatedAt); err != nil {
			return fmt.Errorf("insert revision: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM article_revisions WHERE article_id = ? AND revision_num <= ?`,
			a.ID, next.RevisionNum-MaxRevisions); err != nil {
			return fmt.Errorf("prune revisions: %w", err)
		}
		rev, created = next, true
		return nil
	})
	if errors.Is(err, ErrArticleNotFound) {
		return nil, false, ErrArticleNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("revision: save snapshot: %w", err)
	}
	return rev, created, nil
}

// ListRevisions は記事のリビジョンを新しい順に返す。該当がなければ空スライスを返す。
func (s *Service) ListRevisions(ctx context.Context, articleID string) ([]*Revision, error) {
	revs := []*Revision{}
	err := s.db.SelectContext(ctx, &revs, `SELECT `+columns+` FROM article_revisions
		WHERE article_id = ? ORDER BY revision_num DESC`, articleID)
	if err != nil {
		return nil, fmt.Errorf("revision: list: %w", err)
	}
	return revs, nil
}

// GetRevision は記事の revisionNum 番目のリビジョンを返す。存在しなければ ErrNotFound を返す。
func (s *Service) GetRevision(ctx context.Context, articleID string, revisionNum int) (*Revision, error) {
	var rev Revision
	err := s.db.GetContext(ctx, &rev, `SELECT `+columns+` FROM article_revisions
		WHERE article_id = ? AND revision_num = ?`, articleID, revisionNum)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("revision: get: %w", err)
	}
	return &rev, nil
}

// withWriteTx は BEGIN IMMEDIATE の書き込みトランザクション内で fn を実行し、成功すればコミットする。
// 書き込みロックを先に取り、並行保存での revision_num の競合を防ぐ。
func (s *Service) withWriteTx(ctx context.Context, fn func(conn *sqlx.Conn, now time.Time) error) error {
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
			// ctx がキャンセル済みでも確実に巻き戻し、トランザクションを開いたまま接続をプールに返さない。
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := fn(conn, s.now().UTC()); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// newID は crypto/rand による16バイトの乱数を hex で返す。
func newID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate revision id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
