package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/scheduler"
)

// lifecycleTools は記事を下書きに戻す・ゴミ箱へ移すツールを返す。どちらも物理削除はせず、管理画面と同じく
// 公開中・予約中の記事に触れるのは編集者・管理者だけにする。
func (s *Server) lifecycleTools() []*tool {
	note := stringProp(fmt.Sprintf("理由などのメモ（%d文字以内。内部コメントに残ります）", maxNoteRunes))
	return []*tool{
		{
			info: toolInfo{
				Name:  "cms_unpublish_article",
				Title: "記事を下書きに戻す",
				Description: "公開中（published）・予約中（scheduled）・校閲待ち（review）の記事・固定ページを下書き（draft）に戻します。" +
					"公開中の記事は公開を取り下げ（初回の公開日時は残ります）、予約中の記事は予約を取り消します。公開先（静的サイト・公開サーバー）も管理画面と同じく更新します。" +
					"公開中・予約中の記事は編集者・管理者だけ、校閲待ちの記事は編集者・管理者と記事の作成者が戻せます。" +
					"PC の sorus で編集中・他の人が管理画面で編集中の記事は戻せません。note は内部コメントとして残ります。",
				InputSchema: objectSchema(map[string]any{
					"article_id": stringProp("下書きに戻す記事の ID"),
					"note":       note,
				}, "article_id"),
				Annotations: &toolAnnotations{Title: "記事を下書きに戻す", DestructiveHint: true, IdempotentHint: true},
			},
			run: s.toolUnpublishArticle,
		},
		{
			info: toolInfo{
				Name:  "cms_delete_article",
				Title: "記事をゴミ箱へ移す",
				Description: "下書き（draft）・校閲待ち（review）の記事・固定ページをゴミ箱（deleted）へ移します。物理削除はしません（ゴミ箱の記事は30日後に自動で完全に削除されます）。" +
					"公開中・予約中の記事は移せないため、先に cms_unpublish_article で下書きに戻してください。" +
					"編集者・管理者はどの下書き・校閲待ちの記事も、執筆者は自分が作成した下書きだけを移せます。" +
					"PC の sorus で編集中・他の人が管理画面で編集中の記事は移せません。note は内部コメントとして残ります。",
				InputSchema: objectSchema(map[string]any{
					"article_id": stringProp("ゴミ箱へ移す記事の ID"),
					"note":       note,
				}, "article_id"),
				Annotations: &toolAnnotations{Title: "記事をゴミ箱へ移す", DestructiveHint: true, IdempotentHint: true},
			},
			run: s.toolDeleteArticle,
		},
	}
}

// checkUnpublishable は user が記事 a を下書きに戻せるかを確かめる。公開中・予約中の記事は編集者・管理者だけ
// （公開の取り下げにあたるため。管理画面の requiresPublisher と同じ）、校閲待ちの記事は編集者・管理者と作成者。
func checkUnpublishable(user *auth.User, a *article.Article) error {
	if !inTeam(user) {
		return errNoEditPermission()
	}
	switch article.ViewStatus(a, time.Now()) {
	case article.StatusPublished, article.StatusScheduled:
		if !canSeeAll(user) {
			return toolErrorf("公開中・予約中の記事を下書きに戻せるのは編集者・管理者だけです")
		}
	case article.StatusReview:
		if !canSeeAll(user) && a.AuthorID != user.ID {
			return toolErrorf("他の人の校閲待ちの記事を下書きに戻せるのは編集者・管理者だけです")
		}
	case article.StatusDraft:
		return toolErrorf("この記事はすでに下書き（draft）です")
	default:
		return toolErrorf("ゴミ箱の記事は下書きに戻せません（現在: %s）", a.Status)
	}
	return nil
}

// checkTrashable は user が記事 a をゴミ箱へ移せるかを確かめる。公開中・予約中の記事は誤って取り下げないよう、
// 先に cms_unpublish_article で下書きに戻してもらう。校閲待ちは編集者・管理者、下書きは編集者・管理者と作成者。
func checkTrashable(user *auth.User, a *article.Article) error {
	if !inTeam(user) {
		return errNoEditPermission()
	}
	switch article.ViewStatus(a, time.Now()) {
	case article.StatusPublished, article.StatusScheduled:
		return toolErrorf("公開中・予約中の記事はゴミ箱へ移せません。先に cms_unpublish_article で下書きに戻してください（編集者・管理者のみ）")
	case article.StatusReview:
		if !canSeeAll(user) {
			return toolErrorf("校閲待ちの記事をゴミ箱へ移せるのは編集者・管理者だけです")
		}
	case article.StatusDraft:
		if !canSeeAll(user) && a.AuthorID != user.ID {
			return toolErrorf("他の人の下書きをゴミ箱へ移せるのは編集者・管理者だけです")
		}
	default:
		return toolErrorf("この記事はすでにゴミ箱にあります")
	}
	return nil
}

type articleStateArgs struct {
	ArticleID string `json:"article_id"`
	Note      string `json:"note"`
}

// articleStateResult は cms_unpublish_article・cms_delete_article の結果。
type articleStateResult struct {
	Article        articleRow     `json:"article"`
	PreviousStatus article.Status `json:"previous_status"`
	Message        string         `json:"message"`
	// Warning は記事の変更は済んだが、後続の処理（公開サーバーからの取り下げなど）に失敗したときの案内。
	Warning string `json:"warning,omitempty"`
}

// readStateArgs は article_id と note を検査して返す。
func readStateArgs(raw json.RawMessage) (id, note string, err error) {
	var args articleStateArgs
	if err := decodeArgs(raw, &args); err != nil {
		return "", "", err
	}
	id = strings.TrimSpace(args.ArticleID)
	if id == "" {
		return "", "", toolErrorf("article_id を指定してください")
	}
	note = strings.TrimSpace(normalizeNewlines(args.Note))
	if utf8.RuneCountInString(note) > maxNoteRunes {
		return "", "", toolErrorf("note は%d文字以内で指定してください", maxNoteRunes)
	}
	return id, note, nil
}

// lockedArticle は記事 id に対して guardWrite（check → PC で編集中でないか → ロックの獲得）を行い、ロックの獲得後に
// 読み直した記事をもう一度確かめて返す。ロックを待つ間に状態が変わっていれば断る。
func (s *Server) lockedArticle(ctx context.Context, user *auth.User, id string, check editCheck) (*article.Article, func(bool), error) {
	a, err := s.loadArticle(user, id, "")
	if err != nil {
		return nil, nil, err
	}
	release, err := s.guardWrite(ctx, user, a, check)
	if err != nil {
		return nil, nil, err
	}
	if a, err = s.loadArticle(user, id, ""); err == nil {
		err = checkWritable(user, a, check)
	}
	if err != nil {
		release(false)
		return nil, nil, err
	}
	return a, release, nil
}

func (s *Server) toolUnpublishArticle(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	id, note, err := readStateArgs(raw)
	if err != nil {
		return nil, err
	}
	a, release, err := s.lockedArticle(ctx, user, id, checkUnpublishable)
	if err != nil {
		return nil, err
	}
	defer release(false)

	now := time.Now().UTC()
	prev := article.ViewStatus(a, now)
	// 同期 API の下書きに戻す操作（syncUnpublishArticle）と同じく、過去の公開日時は初回公開日時として残し、
	// 未来の公開日時（予約）は消す。ロックの獲得後に読んだ状態のままのときだけ変える。
	res, err := s.db.ExecContext(ctx, `UPDATE articles SET status = ?,
			published_at = CASE WHEN published_at IS NOT NULL AND julianday(published_at) > julianday(?) THEN NULL ELSE published_at END,
			scheduled_at = NULL, updated_at = ?
		WHERE id = ? AND status = ?`, article.StatusDraft, now, now, id, a.Status)
	if err != nil {
		return nil, fmt.Errorf("unpublish article: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("unpublish article: %w", err)
	} else if n == 0 {
		return nil, toolErrorf("記事の状態が変わったため下書きに戻せませんでした。cms_get_article で現在の状態を確認してください")
	}
	if updated, err := s.articles.GetByID(id); err == nil {
		s.snapshot(ctx, updated, user, "下書きに戻す（MCP）")
	}
	if note == "" {
		note = "下書きに戻しました（MCP）"
	}
	if err := s.insertComment(ctx, id, user.ID, "【下書きに戻す】"+note); err != nil {
		s.logger.Printf("mcp: comment: article %s: %v", id, err)
	}
	s.changed(id)
	if err := scheduler.Track(ctx, s.db, now, id); err != nil {
		s.logger.Printf("mcp: track scheduled article %s: %v", id, err)
	}

	out := articleStateResult{PreviousStatus: prev}
	switch prev {
	case article.StatusPublished:
		out.Message = "公開を取り下げ、下書きに戻しました"
	case article.StatusScheduled:
		out.Message = "予約を取り消し、下書きに戻しました"
	default:
		out.Message = "校閲申請を取り下げ、下書きに戻しました"
	}
	// 読者に見えていた（見える予定だった）記事は、管理画面と同じく公開先からも取り下げる。
	if (prev == article.StatusPublished || prev == article.StatusScheduled) && s.cfg.OnArticleUnpublished != nil {
		if err := s.cfg.OnArticleUnpublished(ctx, user, id); err != nil {
			var te *toolError
			if !errors.As(err, &te) {
				s.logger.Printf("mcp: unpublish article %s from destinations: %v", id, err)
				err = toolErrorf("公開サーバーから記事を取り下げられませんでした。PC の sorus の管理画面で公開先の状態を確認してください")
			}
			out.Warning = err.Error()
		}
	}
	if out.Article, err = s.articleRowByID(ctx, id); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) toolDeleteArticle(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	id, note, err := readStateArgs(raw)
	if err != nil {
		return nil, err
	}
	a, release, err := s.lockedArticle(ctx, user, id, checkTrashable)
	if err != nil {
		return nil, err
	}
	// ゴミ箱の記事は編集しないため、管理画面の削除と同じく自分のロックも残さない。
	defer release(true)

	prev := article.ViewStatus(a, time.Now())
	if err := s.articles.SoftDelete(id); err != nil {
		if errors.Is(err, article.ErrNotFound) {
			return nil, errArticleNotFound(id)
		}
		return nil, fmt.Errorf("move article to trash: %w", err)
	}
	if note == "" {
		note = "ゴミ箱へ移しました（MCP）"
	}
	if err := s.insertComment(ctx, id, user.ID, "【ゴミ箱へ移動】"+note); err != nil {
		s.logger.Printf("mcp: comment: article %s: %v", id, err)
	}
	s.changed(id)

	msg := "記事をゴミ箱へ移しました。ゴミ箱の記事は30日後に自動で完全に削除されます"
	if !s.cfg.PublicOnly {
		msg += "（それまでは sorus の管理画面のゴミ箱から元に戻せます）"
	}
	out := articleStateResult{PreviousStatus: prev, Message: msg}
	if out.Article, err = s.articleRowByID(ctx, id); err != nil {
		return nil, err
	}
	return out, nil
}
