package server

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/lock"
	"sorus/internal/revision"
)

// revisionRow はリビジョン一覧の1行。
type revisionRow struct {
	Num        int
	CreatedAt  time.Time
	AuthorName string
	Comment    string
	Title      string
	// Chars は本文の文字数、CharDelta は1つ前の版からの文字数の増減。最古の版では 0。
	Chars, CharDelta int
	// IsCurrent は現在の記事とタイトル・本文が同じ版か。
	IsCurrent bool
}

// revisionListData は partials/revision_modal.html の "revision_list" に渡すデータ。
type revisionListData struct {
	ArticleID string
	Revisions []revisionRow
	// Lang は管理画面の表示言語。
	Lang string
}

// revisionDiffData は partials/revision_modal.html の "revision_diff" に渡すデータ。
// 差分は第 Num 版（旧）から現在の記事（新）への変化で、HTML は ai.RenderDiffHTML でエスケープ済み。
type revisionDiffData struct {
	// Lang は管理画面の表示言語。
	Lang       string
	ArticleID  string
	Num        int
	CreatedAt  time.Time
	AuthorName string
	Comment    string
	TitleDiff  template.HTML
	BodyDiff   template.HTML
	Changed    bool
	// CanRestore はこの版に復元できるか（他のユーザーが編集中でなく、権限があり、現在と内容が異なる）。
	CanRestore bool
	CSRFToken  string
}

// handleRevisionList は記事のリビジョン履歴（新しい順）のパーシャルを返す。
func (s *Server) handleRevisionList(c echo.Context) error {
	id := c.Param("id")
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	revs, err := s.revisions.ListRevisions(c.Request().Context(), id)
	if err != nil {
		return err
	}
	names, err := s.userNames(c, revisionAuthors(revs))
	if err != nil {
		return err
	}

	data := revisionListData{ArticleID: id, Revisions: make([]revisionRow, len(revs)), Lang: string(language(c))}
	for i, r := range revs {
		row := revisionRow{
			Num:        r.RevisionNum,
			CreatedAt:  r.CreatedAt,
			AuthorName: names[r.AuthorID],
			Comment:    r.Comment,
			Title:      r.Title,
			Chars:      utf8.RuneCountInString(r.ContentMarkdown),
			IsCurrent:  r.Title == a.Title && r.ContentMarkdown == a.ContentMarkdown,
		}
		if i+1 < len(revs) {
			row.CharDelta = row.Chars - utf8.RuneCountInString(revs[i+1].ContentMarkdown)
		}
		data.Revisions[i] = row
	}
	return c.Render(http.StatusOK, "revision_list", data)
}

// handleRevisionDiff は第 :num 版から現在の記事への差分（追加は緑、削除は赤）のパーシャルを返す。
func (s *Server) handleRevisionDiff(c echo.Context) error {
	id := c.Param("id")
	a, rev, err := s.articleRevision(c)
	if err != nil {
		return err
	}
	names, err := s.userNames(c, []string{rev.AuthorID})
	if err != nil {
		return err
	}
	body := ai.Diff(rev.ContentMarkdown, a.ContentMarkdown)
	title := ai.Diff(rev.Title, a.Title)
	data := revisionDiffData{
		Lang:       string(language(c)),
		ArticleID:  id,
		Num:        rev.RevisionNum,
		CreatedAt:  rev.CreatedAt,
		AuthorName: names[rev.AuthorID],
		Comment:    rev.Comment,
		// RenderDiffHTML はテキストをすべてエスケープし、<ins>/<del> だけを出力する。
		TitleDiff: template.HTML(ai.RenderDiffHTML(title)),
		BodyDiff:  template.HTML(ai.RenderDiffHTML(body)),
		Changed:   ai.HasChanges(body) || ai.HasChanges(title),
		CSRFToken: csrfToken(c),
	}
	if data.Changed && (canPublish(currentUser(c)) || !requiresPublisher(a.Status)) {
		info, err := s.locker.Check(c.Request().Context(), id, currentUser(c).ID)
		data.CanRestore = err == nil && (!info.IsLocked || info.IsOwnLock)
	}
	return c.Render(http.StatusOK, "revision_diff", data)
}

// handleRevisionRestore は記事のタイトルと本文を第 :num 版の内容に戻す。状態（公開中など）は変えない。
//
// 復元の前に現在の内容をリビジョンとして残すため、復元自体もいつでも元に戻せる。
// 他のユーザーが編集中なら 409、公開中・予約中の記事を執筆者が復元しようとした場合は 403 を返す。
func (s *Server) handleRevisionRestore(c echo.Context) (err error) {
	id := c.Param("id")
	user := currentUser(c)

	info, release, err := s.lockForAction(c, id)
	if errors.Is(err, lock.ErrAlreadyLocked) {
		return s.workflowError(c, id, http.StatusConflict, info.UserName+"さんが編集中のため復元できません")
	}
	if err != nil {
		return err
	}
	defer func() { release(err) }()
	a, rev, err := s.articleRevision(c)
	if err != nil {
		return err
	}
	if requiresPublisher(a.Status) && !canPublish(user) {
		return echo.NewHTTPError(http.StatusForbidden, "公開中・予約中の記事を変更できるのは編集長・管理者だけです")
	}

	contentChanged := a.Title != rev.Title || a.ContentMarkdown != rev.ContentMarkdown
	wasPublished := article.ViewStatus(a, clockNow()) == article.StatusPublished
	s.snapshot(c, a, "復元前の自動保存")
	a.Title = rev.Title
	a.ContentMarkdown = rev.ContentMarkdown
	if err := s.articles.Save(a); err != nil {
		if errors.Is(err, article.ErrInvalid) {
			return s.workflowError(c, id, http.StatusUnprocessableEntity, "この版は復元できません")
		}
		return err
	}
	s.reindexArticle(c, id)
	s.snapshot(c, a, fmt.Sprintf("第%d版から復元", rev.RevisionNum))
	flash := "restored"
	if wasPublished && contentChanged {
		flash = revisionRestoreFlash(s.reflectAfterPublish())
	}

	if isAsync(c) {
		return c.JSON(http.StatusOK, map[string]any{"revision_num": rev.RevisionNum, "message": localizedMessage(c, flash)})
	}
	return c.Redirect(http.StatusSeeOther, "/admin/articles/"+id+"/edit?flash="+flash)
}

func revisionRestoreFlash(result string) string {
	switch result {
	case flashPublishedReflecting:
		return "restored_reflecting"
	case flashPublishedManual:
		return "restored_manual"
	case flashPublishedNoTarget:
		return "restored_no_target"
	default:
		return "restored"
	}
}

// articleRevision はパスの :id の記事と :num 番目のリビジョンを返す。どちらかが無ければ 404 を返す。
func (s *Server) articleRevision(c echo.Context) (*article.Article, *revision.Revision, error) {
	id := c.Param("id")
	num, err := strconv.Atoi(c.Param("num"))
	if err != nil || num < 1 {
		return nil, nil, echo.ErrNotFound
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return nil, nil, echo.ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	rev, err := s.revisions.GetRevision(c.Request().Context(), id, num)
	if errors.Is(err, revision.ErrNotFound) {
		return nil, nil, echo.ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return a, rev, nil
}

func revisionAuthors(revs []*revision.Revision) []string {
	ids := make([]string, 0, len(revs))
	for _, r := range revs {
		ids = append(ids, r.AuthorID)
	}
	return ids
}

// userNames は ids のユーザーの表示名（無ければユーザー名）を ID ごとに返す。
func (s *Server) userNames(c echo.Context, ids []string) (map[string]string, error) {
	names := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	q, args, err := sqlx.In(`SELECT id, COALESCE(NULLIF(display_name, ''), username) AS name FROM users WHERE id IN (?)`, ids)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID   string `db:"id"`
		Name string `db:"name"`
	}
	if err := s.db.SelectContext(c.Request().Context(), &rows, s.db.Rebind(q), args...); err != nil {
		return nil, fmt.Errorf("server: user names: %w", err)
	}
	for _, r := range rows {
		names[r.ID] = r.Name
	}
	return names, nil
}
