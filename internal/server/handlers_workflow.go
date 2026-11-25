package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/lock"
)

const (
	// maxCommentRunes は内部コメント・差し戻し理由の最大文字数。
	maxCommentRunes = 2000
	// datetimeLocalLayout は <input type="datetime-local"> の値の形式（JST として解釈する）。
	datetimeLocalLayout = "2006-01-02T15:04"
)

// commentView は内部コメントの1件。
type commentView struct {
	ID        string    `db:"id"`
	UserID    string    `db:"user_id"`
	Content   string    `db:"content"`
	CreatedAt time.Time `db:"created_at"`
	UserName  string    `db:"user_name"`
	UserRole  auth.Role `db:"user_role"`
	// IsMine は閲覧者自身のコメントか。
	IsMine bool `db:"-"`
}

// commentListData は partials/comment_list.html に渡すデータ。
type commentListData struct {
	ArticleID string
	Comments  []commentView
	// Lang は管理画面の表示言語。
	Lang string
}

// isAsync は fetch（JSON を受け付ける）または HTMX からの要求かを返す。フォーム送信と応答の形を分ける。
func isAsync(c echo.Context) bool {
	return c.Request().Header.Get("HX-Request") == "true" ||
		strings.Contains(c.Request().Header.Get(echo.HeaderAccept), echo.MIMEApplicationJSON)
}

// handleSubmitReview は記事を校閲待ちにする（下書き → 校閲待ち）。すべてのロールが行える。
//
// 編集画面のフォームから送られた場合（title を含む場合）は、入力中の内容を保存してから申請する。
// 他のユーザーが編集中なら 409 を返す。申請メモ（note）は申し送りコメントとして残し、
// 校閲者が開けるよう自分のロックを解放して校閲待ちの一覧へ戻る。
func (s *Server) handleSubmitReview(c echo.Context) (err error) {
	id := c.Param("id")
	token := c.FormValue("draft_token")
	user := currentUser(c)
	ctx := c.Request().Context()

	info, release, err := s.lockForAction(c, id)
	if errors.Is(err, lock.ErrAlreadyLocked) {
		return s.workflowError(c, id, http.StatusConflict, info.UserName+"さんが編集中のため校閲を申請できません")
	}
	if err != nil {
		return err
	}
	defer func() { release(err) }()
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.Status != article.StatusDraft {
		return s.workflowError(c, id, http.StatusConflict, "校閲を申請できるのは下書きの記事だけです（現在: "+statusLabel(a.Status)+"）")
	}

	var proposal proposalConfirmation
	if c.FormValue("title") != "" || c.FormValue("content_markdown") != "" {
		form := readArticleForm(c)
		form.ID = id
		// AI アプリの変更案を編集欄に反映して申請するときは、確かめた案と記事の内容が変わっていないかを確かめる。
		// AI 機能が OFF なら変更案は扱わない（handleArticleSave と同じ）。
		if s.aiEnabled() {
			var msg string
			if proposal, msg, err = s.confirmedProposal(c, a); err != nil {
				return err
			} else if msg != "" {
				return s.renderFormError(c, http.StatusConflict, editData{Form: withStoredState(form, a)}, msg)
			}
		}
		if msg, err := s.applyForm(c, a, form, actionReview, user); err != nil {
			return err
		} else if msg != "" {
			return s.renderFormError(c, http.StatusUnprocessableEntity, editData{Form: form}, msg)
		}
	} else {
		a.Status = article.StatusReview
	}
	note := strings.TrimSpace(normalizeNewlines(c.FormValue("note")))
	if utf8.RuneCountInString(note) > maxCommentRunes {
		return s.workflowError(c, id, http.StatusUnprocessableEntity, fmt.Sprintf("申請メモは%d文字以内で入力してください", maxCommentRunes))
	}
	// 翻訳の案を反映したときは、通常の保存と同じく、今の原文から作られた案なら要更新の印を外し、原文が変わる前に
	// 作られた（または原文を確かめられない）案なら要更新のままにする。
	if proposal.SourceCurrent {
		a.IsOutdated = false
	}
	if proposal.KeepOutdated {
		a.IsOutdated = true
	}
	if err := s.articles.Save(a); err != nil {
		if errors.Is(err, article.ErrInvalid) {
			return s.workflowError(c, id, http.StatusUnprocessableEntity, "入力内容を確認してください")
		}
		return err
	}
	s.reindexArticle(c, id)
	s.snapshot(c, a, proposalComment("校閲申請", proposal.ID))
	s.finishProposal(c, id, proposal.ID)
	s.addSystemComment(c, id, reviewRequestComment(note))
	// 校閲者がすぐに開けるよう、申請した人のロックは成功時に必ず解放する。
	_ = s.locker.Release(ctx, id, user.ID)

	if isAsync(c) {
		return c.JSON(http.StatusOK, map[string]any{"status": a.Status, "message": localizedMessage(c, "submitted")})
	}
	redirect := "/admin/articles?status=review" + kindQuery(a.Kind) + "&flash=submitted"
	if token != "" {
		redirect += "&saved_draft_token=" + url.QueryEscape(token)
	}
	return c.Redirect(http.StatusSeeOther, redirect)
}

// lockForAction は記事を変更する操作のために編集ロックを獲得する。他のユーザーが編集中なら
// lock.ErrAlreadyLocked と保持者の情報を、記事が無ければ 404 を返す。
//
// 戻り値の release は操作の終わりに操作のエラー（または応答）を渡して呼ぶ。操作が失敗し、かつ操作の前に
// 自分のロックを持っていなかった（編集画面を開いていない）場合だけロックを解放し、ロックを残さない。
func (s *Server) lockForAction(c echo.Context, id string) (*lock.LockInfo, func(error), error) {
	ctx := c.Request().Context()
	user := currentUser(c)
	held := false
	if pre, err := s.locker.Check(ctx, id, user.ID); err == nil {
		held = pre.IsLocked && pre.IsOwnLock
	}
	info, err := s.locker.Acquire(ctx, id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return nil, nil, echo.ErrNotFound
	}
	if err != nil {
		return info, nil, err
	}
	release := func(opErr error) {
		if !held && (opErr != nil || c.Response().Status >= http.StatusBadRequest) {
			_ = s.locker.Release(ctx, id, user.ID)
		}
	}
	return info, release, nil
}

// reviewRequestComment は校閲申請のときに残す申し送りコメントの本文。
func reviewRequestComment(note string) string {
	if note == "" {
		note = "校閲をお願いします。"
	}
	return "【校閲申請】" + note
}

// handleRejectReview は校閲待ちの記事を差し戻す（校閲待ち → 下書き）。編集者・管理者のみ。
// 差し戻し理由（reason・必須）は申し送りコメントとして自動で登録する。
// 他のユーザーが編集中の場合は 409 を返す。
func (s *Server) handleRejectReview(c echo.Context) (err error) {
	id := c.Param("id")
	reason := strings.TrimSpace(normalizeNewlines(c.FormValue("reason")))
	if reason == "" {
		return s.workflowError(c, id, http.StatusUnprocessableEntity, "差し戻しの理由を入力してください")
	}
	if utf8.RuneCountInString(reason) > maxCommentRunes {
		return s.workflowError(c, id, http.StatusUnprocessableEntity, fmt.Sprintf("差し戻しの理由は%d文字以内で入力してください", maxCommentRunes))
	}

	info, release, err := s.lockForAction(c, id)
	if errors.Is(err, lock.ErrAlreadyLocked) {
		return s.workflowError(c, id, http.StatusConflict, info.UserName+"さんが編集中のため差し戻しできません")
	}
	if err != nil {
		return err
	}
	user := currentUser(c)
	ctx := c.Request().Context()
	defer func() {
		release(err)
		if err == nil {
			_ = s.locker.Release(ctx, id, user.ID)
		}
	}()

	now := time.Now().UTC()
	ok, err := s.transition(c, id, `UPDATE articles SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		article.StatusDraft, now, id, article.StatusReview)
	if err != nil || !ok {
		return err
	}
	s.addSystemComment(c, id, "【差し戻し】"+reason)

	if isAsync(c) {
		return c.JSON(http.StatusOK, map[string]any{"status": article.StatusDraft, "message": localizedMessage(c, "rejected")})
	}
	return c.Redirect(http.StatusSeeOther, "/admin/articles/"+id+"/edit?flash=rejected")
}

// handleApprovePublish は校閲待ちの記事を承認して公開する。編集者・管理者のみ。
// publish_at（datetime-local、JST）が空なら即時公開（校閲待ち → 公開中）、未来の日時なら予約投稿にする。
// 予約投稿は状態を公開、公開日時をその日時として保存し、その日時から読者に見え始める（article.IsScheduled）。
// 一度読者に見えた記事（公開日時が過去の記事）は、予約にはできず（422）、即時公開では最初の公開日時を保つ。
// 任意のコメント（comment）は承認の記録とともに申し送りコメントに残す。
// 他のユーザーが編集中の場合は 409 を返す。
func (s *Server) handleApprovePublish(c echo.Context) (err error) {
	id := c.Param("id")
	comment := strings.TrimSpace(normalizeNewlines(c.FormValue("comment")))
	if utf8.RuneCountInString(comment) > maxCommentRunes {
		return s.workflowError(c, id, http.StatusUnprocessableEntity, fmt.Sprintf("コメントは%d文字以内で入力してください", maxCommentRunes))
	}
	now := clockNow().UTC()
	scheduledAt, msg := parsePublishAt(c.FormValue("publish_at"), now)
	if msg != "" {
		return s.workflowError(c, id, http.StatusUnprocessableEntity, msg)
	}

	info, release, err := s.lockForAction(c, id)
	if errors.Is(err, lock.ErrAlreadyLocked) {
		return s.workflowError(c, id, http.StatusConflict, info.UserName+"さんが編集中のため承認できません")
	}
	if err != nil {
		return err
	}
	user := currentUser(c)
	ctx := c.Request().Context()
	defer func() {
		release(err)
		if err == nil {
			_ = s.locker.Release(ctx, id, user.ID)
		}
	}()

	// 設定を変える前に保存したページは、公開してから同期 API などのパスを隠さないよう、状態を変える前に検査する。
	if a, err := s.articles.GetByID(id); err == nil && s.reservedPage(a) {
		return s.workflowError(c, id, http.StatusUnprocessableEntity, i18n.Translate(string(language(c)), "reserved_page_slug", a.Slug))
	} else if err == nil && refusesSchedule(a, scheduledAt, now) {
		// 一度公開した記事を予約にすると、公開日時まで読者から見えなくなる。断って即時公開での再承認を案内する。
		return s.workflowError(c, id, http.StatusUnprocessableEntity, "一度公開した記事は予約投稿にできません。公開日時を空欄にして承認してください（最初の公開日時のまま公開します）")
	} else if err != nil && !errors.Is(err, article.ErrNotFound) {
		return err
	}

	var ok bool
	if scheduledAt == nil {
		// 一度公開した記事を再承認した場合は、最初の公開日時を保つ（article.Service.Save と同じ扱い）。
		// 公開日時が未来のまま残っていれば（予約を取り消しきれていない記事）、今を公開日時にする。
		ok, err = s.transition(c, id, `UPDATE articles SET status = ?,
				published_at = CASE WHEN published_at IS NULL OR julianday(published_at) > julianday(?) THEN ? ELSE published_at END,
				updated_at = ? WHERE id = ? AND status = ?`,
			article.StatusPublished, now, now, now, id, article.StatusReview)
	} else {
		ok, err = s.transition(c, id, `UPDATE articles SET status = ?, published_at = ?, updated_at = ?
				WHERE id = ? AND status = ?`,
			article.StatusPublished, *scheduledAt, now, id, article.StatusReview)
	}
	if err != nil || !ok {
		return err
	}
	s.reindexArticle(c, id)
	s.trackScheduled(c, id)

	// 応答の status は画面に出す状態（予約なら仮想の scheduled）。fetch で承認する画面との取り決めを変えない。
	status, flash, snap, record := article.StatusPublished, "approved", "承認・公開", "【承認】公開しました。"
	if scheduledAt != nil {
		status, flash, snap = article.StatusScheduled, "scheduled", "承認・予約公開"
		record = "【承認】" + scheduledAt.In(jst).Format("2006/01/02 15:04") + " に予約公開します。"
	} else if key := s.reflectAfterPublish(); key != "" {
		// 今すぐ公開した記事は、記事公開時の自動反映の設定に従ってサイトに反映し、その旨を知らせる。
		flash = key
	}
	if a, err := s.articles.GetByID(id); err == nil {
		s.snapshot(c, a, snap)
	}
	if comment != "" {
		record += "\n" + comment
	}
	s.addSystemComment(c, id, record)

	if isAsync(c) {
		message := localizedMessage(c, flash)
		if scheduledAt != nil {
			message = scheduledMessage(c, *scheduledAt)
		}
		return c.JSON(http.StatusOK, map[string]any{"status": status, "message": message})
	}
	return c.Redirect(http.StatusSeeOther, "/admin/articles/"+id+"/edit?flash="+flash)
}

// transition は query（状態を条件にした UPDATE）で記事の状態を遷移させる。
// 記事が無ければ 404、状態が条件に合わず更新されなければ（他の校閲者が先に処理した場合など）409 を返す。
// 戻り値の ok が false のときは、利用者向けの応答を返し終えている。
func (s *Server) transition(c echo.Context, id, query string, args ...any) (ok bool, err error) {
	res, err := s.db.ExecContext(c.Request().Context(), query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return false, echo.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return false, s.workflowError(c, id, http.StatusConflict, "校閲待ちの記事ではありません（現在: "+statusLabel(a.Status)+"）")
}

// workflowError は状態・入力の不備を、fetch・HTMX には JSON のエラー、フォーム送信にはエラー通知付きの編集画面で返す。
func (s *Server) workflowError(c echo.Context, id string, code int, msg string) error {
	if isAsync(c) {
		return echo.NewHTTPError(code, msg)
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	data := editData{Form: formFromArticle(a)}
	if info, err := s.locker.Check(c.Request().Context(), id, currentUser(c).ID); err == nil && info.IsLocked && !info.IsOwnLock {
		data.Lock = info
	}
	return s.renderFormError(c, code, data, msg)
}

// parsePublishAt は予約日時の入力 v（datetime-local の値、日本時間）を、承認して公開・「公開する」の予約で共通の規則で
// 解釈する。空なら nil（今すぐ公開）を返す。形式の誤り・今以前の日時・今から MinScheduleLead より近い日時は、
// 利用者向けのメッセージ msg を返す。
func parsePublishAt(v string, now time.Time) (at *time.Time, msg string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, ""
	}
	t, err := parseDatetimeLocal(v)
	if err != nil {
		return nil, "予約日時の形式が正しくありません"
	}
	if !t.After(now) {
		return nil, "予約日時には現在より後の日時を指定してください"
	}
	if t.Before(now.Add(MinScheduleLead)) {
		return nil, fmt.Sprintf("予約日時には今から%d分以上先の日時を指定してください", int(MinScheduleLead/time.Minute))
	}
	return &t, ""
}

// refusesSchedule は記事 a を日時 at（nil なら今すぐ公開）で予約するのを断るか（D9）を返す。読者に見えたことのある
// 記事（公開日時が now 以前）を予約にすると、公開日時まで読者から見えなくなるため断る。
func refusesSchedule(a *article.Article, at *time.Time, now time.Time) bool {
	return at != nil && a.PublishedAt != nil && !a.PublishedAt.After(now)
}

// minScheduleAt は予約日時の入力欄に選べる最小の日時（datetime-local 形式、日本時間）を返す。入力欄は分単位のため、
// 分の途中なら次の分に切り上げ、選んだ日時が parsePublishAt の規則（今から MinScheduleLead 以上先）に合うようにする。
func minScheduleAt(now time.Time) string {
	at := now.Add(MinScheduleLead)
	if t := at.Truncate(time.Minute); !t.Equal(at) {
		at = t.Add(time.Minute)
	}
	return at.In(jst).Format(datetimeLocalLayout)
}

// parseDatetimeLocal は datetime-local の値（秒は省略可）を JST の日時として解釈し、UTC で返す。
func parseDatetimeLocal(v string) (time.Time, error) {
	for _, layout := range []string{datetimeLocalLayout, datetimeLocalLayout + ":05"} {
		if t, err := time.ParseInLocation(layout, v, jst); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid datetime %q", v)
}

// handleCommentList は記事の内部コメント（申し送りスレッド）のパーシャルを返す。エディタが定期的に再読み込みする。
func (s *Server) handleCommentList(c echo.Context) error {
	id := c.Param("id")
	if err := s.ensureArticle(id); err != nil {
		return err
	}
	data, err := s.commentList(c, id)
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "comment_list", data)
}

// handleCommentCreate は内部コメントを投稿する。HTMX・fetch にはスレッドのパーシャルを返し、
// フォーム送信なら編集画面のスレッドへ戻る。コメントは公開サイトには表示しない。
func (s *Server) handleCommentCreate(c echo.Context) error {
	id := c.Param("id")
	if err := s.ensureArticle(id); err != nil {
		return err
	}
	content := strings.TrimSpace(normalizeNewlines(c.FormValue("content")))
	if content == "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "コメントを入力してください")
	}
	if utf8.RuneCountInString(content) > maxCommentRunes {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("コメントは%d文字以内で入力してください", maxCommentRunes))
	}
	if err := s.insertComment(c, id, currentUser(c).ID, content); err != nil {
		return err
	}
	if !isAsync(c) {
		return c.Redirect(http.StatusSeeOther, "/admin/articles/"+id+"/edit#comments")
	}
	data, err := s.commentList(c, id)
	if err != nil {
		return err
	}
	return c.Render(http.StatusCreated, "comment_list", data)
}

// addSystemComment はワークフローの操作（申請・差し戻し・承認）を操作したユーザーのコメントとして記録する。
// 状態の変更は完了しているため、失敗してもログに残すだけにする。
func (s *Server) addSystemComment(c echo.Context, articleID, content string) {
	if err := s.insertComment(c, articleID, currentUser(c).ID, content); err != nil {
		c.Logger().Warnf("comment: article %s: %v", articleID, err)
	}
}

func (s *Server) insertComment(c echo.Context, articleID, userID, content string) error {
	id, err := newCommentID()
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(c.Request().Context(), `INSERT INTO article_comments (id, article_id, user_id, content, created_at)
		VALUES (?, ?, ?, ?, ?)`, id, articleID, userID, content, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("server: insert comment: %w", err)
	}
	return nil
}

// commentList は記事の内部コメントを古い順に返す。
// 同じミリ秒のコメントは書いた順（rowid）に並べる。ID はランダムなので順序に使えない。
func (s *Server) commentList(c echo.Context, articleID string) (commentListData, error) {
	data := commentListData{ArticleID: articleID, Comments: []commentView{}, Lang: string(language(c))}
	err := s.db.SelectContext(c.Request().Context(), &data.Comments, `SELECT cm.id, cm.user_id, cm.content, cm.created_at,
			COALESCE(NULLIF(u.display_name, ''), u.username, '') AS user_name, COALESCE(u.role, '') AS user_role
		FROM article_comments cm
		LEFT JOIN users u ON u.id = cm.user_id
		WHERE cm.article_id = ?
		ORDER BY julianday(cm.created_at), cm.rowid`, articleID)
	if err != nil {
		return data, fmt.Errorf("server: list comments: %w", err)
	}
	me := currentUser(c).ID
	for i := range data.Comments {
		data.Comments[i].IsMine = data.Comments[i].UserID == me
	}
	return data, nil
}

// ensureArticle は記事 id が存在しなければ 404 を返す。
func (s *Server) ensureArticle(id string) error {
	var n int
	if err := s.db.Get(&n, `SELECT COUNT(*) FROM articles WHERE id = ?`, id); err != nil {
		return err
	}
	if n == 0 {
		return echo.ErrNotFound
	}
	return nil
}

func newCommentID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("server: generate comment id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
