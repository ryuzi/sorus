package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/lock"
)

// MCP から記事を変える操作（保存・申請・公開・変更案）は、どれも次の順で確かめる。
//
//  1. 変更できる記事か（editCheck: 権限と状態）
//  2. PC の sorus で編集中（チェックアウト中）でないか
//  3. 他の人が管理画面で編集中でないか（記事を直接変える操作はロックを獲得し、変更案はロックを確かめるだけ）
//  4. ロックを確かめた後に PC で編集が始まっていないか（チェックアウトの印だけを読み直す）
//
// 記事を直接変える操作は guardWrite、変更案を預ける操作は guardPropose と stageProposal を使い、
// 断るときの文言と順序をすべての道具でそろえる。

// editCheck は user が記事 a を MCP から変更できる（権限と状態）かを確かめ、できなければ toolError を返す。
type editCheck func(user *auth.User, a *article.Article) error

// checkEditable は user が記事 a を MCP から変更できるかを確かめる。変更できるのは下書きだけで、管理画面と同じく
// 執筆チームの人は他の人の下書きも変更できる。公開中・予約中の記事は編集者でも変更できない（公開の取り下げにあたるため）。
func checkEditable(user *auth.User, a *article.Article) error {
	if !inTeam(user) {
		return errNoEditPermission()
	}
	if a.Status != article.StatusDraft {
		return toolErrorf("変更できるのは下書き（draft）の記事だけです（現在: %s）。校閲待ちの記事は差し戻されると下書きに戻ります",
			article.ViewStatus(a, time.Now()))
	}
	return nil
}

// checkSyncable は user が翻訳の記事 a の再翻訳を依頼できるかを確かめる editCheck を返す。下書きは checkEditable と同じく
// 執筆チームの誰でも、公開中の記事は編集者・管理者が force を付けたときだけ（反映は sorus の画面で公開の権限を持つ人が確かめる）。
func checkSyncable(force bool) editCheck {
	return func(user *auth.User, a *article.Article) error {
		if a.Status != article.StatusPublished {
			return checkEditable(user, a)
		}
		if !inTeam(user) {
			return errNoEditPermission()
		}
		if !canSeeAll(user) {
			return toolErrorf("公開中の記事を変更する権限がありません（編集者または管理者の force: true が必要です）")
		}
		if !force {
			return toolErrorf("公開中の記事を変更するには force: true を指定してください（sorus の画面で確認して保存するまで公開中の内容は変わりません）")
		}
		return nil
	}
}

// checkApprovable は記事 a が承認して公開できる（校閲待ちの）記事かを確かめる。承認の権限は道具の側で確かめる。
func checkApprovable(_ *auth.User, a *article.Article) error {
	if a.Status != article.StatusReview {
		return toolErrorf("校閲待ち（review）の記事だけを承認できます（現在: %s）", a.Status)
	}
	return nil
}

func errNoEditPermission() error {
	return toolErrorf("記事を変更する権限がありません")
}

// checkWritable は読み込んだ記事 a について、check（権限と状態）→ PC で編集中でないか、の順に確かめる。
func checkWritable(user *auth.User, a *article.Article, check editCheck) error {
	if err := check(user, a); err != nil {
		return err
	}
	return checkNotCheckedOut(a)
}

// guardWrite は記事 a を直接変える前の確認をまとめて行い、操作の後に呼ぶ release を返す。
// check（権限と状態）→ PC で編集中でないか → 編集ロックの獲得 → PC で編集中でないかの読み直し、の順に確かめる。
func (s *Server) guardWrite(ctx context.Context, user *auth.User, a *article.Article, check editCheck) (release func(always bool), err error) {
	if err := checkWritable(user, a, check); err != nil {
		return nil, err
	}
	return s.lockForWrite(ctx, user, a.ID)
}

// guardPropose は記事 a への変更案を作る（AI の翻訳を待つなど）前の確認をまとめて行う。
// check（権限と状態）→ PC で編集中でないか → 他の人が管理画面で編集中でないか、の順に確かめる（ロックは獲得しない）。
// 案を預ける直前の確かめ直しは stageProposal が行う。
func (s *Server) guardPropose(ctx context.Context, user *auth.User, a *article.Article, check editCheck) error {
	if err := checkWritable(user, a, check); err != nil {
		return err
	}
	return s.checkNotLockedByOther(ctx, user, a.ID)
}

// lockForWrite は記事 id の編集ロックを獲得し、獲得までに PC で編集が始まっていないかを読み直して確かめる。
// 編集中なら獲得したロックを戻して toolError を返す。release の意味は acquireLock と同じ。
func (s *Server) lockForWrite(ctx context.Context, user *auth.User, id string) (release func(always bool), err error) {
	release, err = s.acquireLock(ctx, user, id)
	if err != nil {
		return nil, err
	}
	if err := s.recheckNotCheckedOut(ctx, id); err != nil {
		release(false)
		return nil, err
	}
	return release, nil
}

// checkNotCheckedOut は記事 a が PC の sorus で編集中（有効なチェックアウトがある）なら toolError を返す。
// PC 側の変更と食い違わないよう、編集中は MCP からの保存・申請・公開・変更案を断る。
func checkNotCheckedOut(a *article.Article) error {
	if a.IsCheckedOut(time.Now()) {
		return errCheckedOut(a.CheckedOutBy)
	}
	return nil
}

func errCheckedOut(by string) error {
	return toolErrorf("%sさんが PC で編集中のため、そちらの作業が終わるまで変更できません", by)
}

// recheckNotCheckedOut は記事 id のチェックアウトの印だけを読み直し（本文は読まない）、PC の sorus で編集中なら toolError を返す。
func (s *Server) recheckNotCheckedOut(ctx context.Context, id string) error {
	by, _, at, err := s.articles.CheckoutState(ctx, id)
	if errors.Is(err, article.ErrNotFound) {
		return errArticleNotFound(id)
	}
	if err != nil {
		return fmt.Errorf("check checkout: %w", err)
	}
	return checkNotCheckedOut(&article.Article{CheckedOutBy: by, CheckedOutAt: at})
}

func errLockedByOther(name string) error {
	return toolErrorf("%sさんが管理画面で編集中のため変更できません。編集が終わってから再試行してください", name)
}

// acquireLock は記事の編集ロックを獲得し、操作の後に呼ぶ release を返す。他のユーザーが管理画面で編集中なら
// toolError を返す。release は、操作の前から自分のロックを持っていた（自分が管理画面で編集中の）場合は
// ロックを残し、それ以外は解放する。always が true なら持っていた場合も解放する。
func (s *Server) acquireLock(ctx context.Context, user *auth.User, id string) (release func(always bool), err error) {
	held := false
	if pre, err := s.locker.Check(ctx, id, user.ID); err == nil {
		held = pre.IsLocked && pre.IsOwnLock
	}
	info, err := s.locker.Acquire(ctx, id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return nil, errArticleNotFound(id)
	}
	if errors.Is(err, lock.ErrAlreadyLocked) {
		return nil, errLockedByOther(info.UserName)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return func(always bool) {
		if always || !held {
			// 要求が中断されてもロックを残さないよう、キャンセルされない context で解放する。
			_ = s.locker.Release(context.WithoutCancel(ctx), id, user.ID)
		}
	}, nil
}

// checkNotLockedByOther は、他のユーザーが記事 id を管理画面で編集中なら toolError を返す（ロックは獲得しない）。
func (s *Server) checkNotLockedByOther(ctx context.Context, user *auth.User, id string) error {
	info, err := s.locker.Check(ctx, id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return errArticleNotFound(id)
	}
	if err != nil {
		return fmt.Errorf("check lock: %w", err)
	}
	if info.IsLocked && !info.IsOwnLock {
		return errLockedByOther(info.UserName)
	}
	return nil
}
