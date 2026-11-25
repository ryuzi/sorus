package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

type userListData struct {
	Users       []auth.User
	CurrentUser *auth.User
	Error       string
}

func (s *Server) handleUsersList(c echo.Context) error {
	user, _ := auth.UserFromContext(c)
	users, err := auth.ListUsers(s.db)
	if err != nil {
		return err
	}
	errMsg := c.QueryParam("error")
	return s.render(c, http.StatusOK, "users", pageTitle(c, "users"), userListData{
		Users:       users,
		CurrentUser: user,
		Error:       errMsg,
	})
}

func (s *Server) handleUserCreate(c echo.Context) error {
	username := c.FormValue("username")
	displayName := c.FormValue("display_name")
	password := c.FormValue("password")
	role := auth.Role(c.FormValue("role"))

	_, err := auth.CreateUser(s.db, username, displayName, password, role)
	if err != nil {
		var msg string
		switch {
		case errors.Is(err, auth.ErrUsernameTaken):
			msg = "このユーザー名は既に使用されています"
		case errors.Is(err, auth.ErrInvalidUsername):
			msg = "ユーザー名は3〜32文字の半角英数字、ハイフン、アンダースコアで入力してください"
		case errors.Is(err, auth.ErrInvalidPassword):
			msg = "パスワードは8文字以上で入力してください"
		case errors.Is(err, auth.ErrInvalidRole):
			msg = "権限（ロール）が無効です"
		default:
			msg = "ユーザーの作成に失敗しました"
		}
		return c.Redirect(http.StatusSeeOther, "/admin/users?error="+url.QueryEscape(msg))
	}

	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=user_created")
}

func (s *Server) handleUserEdit(c echo.Context) error {
	currentUser, _ := auth.UserFromContext(c)
	targetID := c.Param("id")
	displayName := c.FormValue("display_name")
	role := auth.Role(c.FormValue("role"))

	err := auth.UpdateUser(s.db, currentUser.ID, targetID, displayName, role)
	if err != nil {
		var msg string
		switch {
		case errors.Is(err, auth.ErrCannotDeleteLastAdmin):
			msg = "最後の管理者を降格することはできません"
		case errors.Is(err, auth.ErrInvalidRole):
			msg = "権限が無効です"
		default:
			msg = "ユーザー情報の更新に失敗しました"
		}
		return c.Redirect(http.StatusSeeOther, "/admin/users?error="+url.QueryEscape(msg))
	}

	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=user_updated")
}

// handleUserPassword はユーザーのパスワードを変更し、そのユーザーの全セッション・MCP トークン・
// OAuth 2.1 の認可コードおよびトークン（oauth_tokens）をすべて失効させる（auth.UpdateUserPassword）。
func (s *Server) handleUserPassword(c echo.Context) error {
	targetID := c.Param("id")
	newPassword := c.FormValue("password")

	err := auth.UpdateUserPassword(s.db, targetID, newPassword)
	if err != nil {
		var msg string
		if errors.Is(err, auth.ErrInvalidPassword) {
			msg = "パスワードは8文字以上で入力してください"
		} else {
			msg = "パスワードの更新に失敗しました"
		}
		return c.Redirect(http.StatusSeeOther, "/admin/users?error="+url.QueryEscape(msg))
	}

	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=user_password_updated")
}

func (s *Server) handleUserDelete(c echo.Context) error {
	currentUser, _ := auth.UserFromContext(c)
	targetID := c.Param("id")

	err := auth.DeleteUser(s.db, currentUser.ID, targetID)
	if err != nil {
		var msg string
		switch {
		case errors.Is(err, auth.ErrCannotDeleteSelf):
			msg = "自分自身のアカウントは削除できません"
		case errors.Is(err, auth.ErrCannotDeleteLastAdmin):
			msg = "最後の管理者を削除することはできません"
		case errors.Is(err, auth.ErrUserHasArticles):
			msg = "執筆した記事が存在するユーザーは削除できません（先に記事の作成者を変更するか削除してください）"
		default:
			msg = "ユーザーの削除に失敗しました"
		}
		return c.Redirect(http.StatusSeeOther, "/admin/users?error="+url.QueryEscape(msg))
	}

	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=user_deleted")
}
