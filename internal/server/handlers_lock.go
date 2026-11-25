package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/lock"
)

// lockResponse はハートビート API の JSON レスポンス。
type lockResponse struct {
	Locked    bool      `json:"locked"`
	OwnLock   bool      `json:"own_lock"`
	LockedBy  string    `json:"locked_by,omitempty"`
	LockedAt  time.Time `json:"locked_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// handleLockHeartbeat は編集画面からのハートビートでロックを延長する。
//
// lock.Locker.Refresh ではなく Acquire を使い、ページ離脱の sendBeacon が再読み込み後に届いた場合や
// 通信断で TTL を過ぎた場合でも、他のユーザーが取っていなければロックを取り直す。
// 他のユーザーがロックを保持していれば 409 を返し、エディタは保存を無効化する。
func (s *Server) handleLockHeartbeat(c echo.Context) error {
	info, err := s.locker.Acquire(c.Request().Context(), c.Param("id"), currentUser(c).ID)
	switch {
	case errors.Is(err, lock.ErrArticleNotFound):
		return echo.ErrNotFound
	case errors.Is(err, lock.ErrAlreadyLocked):
		resp := s.lockResponse(info)
		resp.Message = info.UserName + "さんが編集を始めたため、閲覧専用になりました"
		return c.JSON(http.StatusConflict, resp)
	case err != nil:
		return err
	}
	return c.JSON(http.StatusOK, s.lockResponse(info))
}

// handleUnlock はページ離脱時（navigator.sendBeacon）に自分のロックを解放する。
// ロックが無ければ何もしない。他のユーザーのロックは解放せず 409 を返す。
func (s *Server) handleUnlock(c echo.Context) error {
	err := s.locker.Release(c.Request().Context(), c.Param("id"), currentUser(c).ID)
	switch {
	case errors.Is(err, lock.ErrArticleNotFound):
		return echo.ErrNotFound
	case errors.Is(err, lock.ErrNotLockHolder):
		return echo.NewHTTPError(http.StatusConflict, "lock is held by another user")
	case err != nil:
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// handleForceUnlock は管理者または編集者が他のユーザーのロックを強制解除し、編集画面へリダイレクトする。
func (s *Server) handleForceUnlock(c echo.Context) error {
	id := c.Param("id")
	err := s.locker.ForceUnlock(c.Request().Context(), id)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	return c.Redirect(http.StatusSeeOther, "/admin/articles/"+id+"/edit?flash=unlocked")
}

func (s *Server) lockResponse(info *lock.LockInfo) lockResponse {
	if !info.IsLocked {
		return lockResponse{}
	}
	return lockResponse{
		Locked:    true,
		OwnLock:   info.IsOwnLock,
		LockedBy:  info.UserName,
		LockedAt:  info.LockedAt,
		ExpiresAt: info.LockedAt.Add(s.cfg.LockTTL),
	}
}

func isAdmin(c echo.Context) bool {
	user, ok := auth.UserFromContext(c)
	return ok && user.HasRole(auth.RoleAdmin)
}
