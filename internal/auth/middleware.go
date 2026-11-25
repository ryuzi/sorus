package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

const (
	// DefaultLoginPath は未認証のブラウザ画面遷移をリダイレクトする先。
	DefaultLoginPath = "/admin/login"
	// contextKeyUser は認証済みユーザーを echo.Context に格納するキー。
	contextKeyUser = "auth.user"
)

// MiddlewareConfig は AuthMiddlewareWithConfig の設定。
type MiddlewareConfig struct {
	// Secure は無効なセッション Cookie を消去する際の Secure 属性。
	// 発行時の SetSessionCookie と同じ値にすること。
	Secure bool
	// LoginPath が空でなければ、未認証のブラウザ画面遷移（HTML を要求する GET/HEAD）を
	// 303 でここへリダイレクトし、HTMX リクエストには HX-Redirect ヘッダーを付けて 401 を返す。
	// 空なら常に 401 を返す。
	LoginPath string
	// CookieName はセッション Cookie の名前（CookieNamesFor の Session）。空なら1つ目のサイトとサーバーモードの名前。
	CookieName string
}

// AuthMiddleware はセッション Cookie を検証し、有効ならユーザーを Context に注入する。
// 未認証時は API には 401、ブラウザの画面遷移には DefaultLoginPath へのリダイレクトを返す。
func AuthMiddleware(db *sqlx.DB) echo.MiddlewareFunc {
	return AuthMiddlewareWithConfig(db, MiddlewareConfig{Secure: true, LoginPath: DefaultLoginPath})
}

// AuthMiddlewareWithConfig は cfg に従う AuthMiddleware。
func AuthMiddlewareWithConfig(db *sqlx.DB, cfg MiddlewareConfig) echo.MiddlewareFunc {
	name := cfg.CookieName
	if name == "" {
		name = CookieNamesFor("").Session
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cookie, err := c.Cookie(name)
			if err != nil {
				return unauthorized(c, cfg)
			}
			user, err := ValidateSession(db, cookie.Value)
			if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrSessionExpired) {
				ClearSessionCookie(c, name, cfg.Secure)
				return unauthorized(c, cfg)
			}
			if err != nil {
				return err
			}
			SetUser(c, user)
			return next(c)
		}
	}
}

// RequireRole は Context のユーザーが roles のいずれかを（User.HasRole。管理者はすべての役割を持つ）持つ場合のみ通過させる。
// AuthMiddleware の後に適用すること。ユーザーが無ければ 401、ロールが不足なら 403 を返す。
func RequireRole(roles ...Role) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user, ok := UserFromContext(c)
			if !ok {
				return echo.ErrUnauthorized
			}
			if user.HasRole(roles...) {
				return next(c)
			}
			return echo.ErrForbidden
		}
	}
}

// SetUser は user を認証済みユーザーとして Context に格納する。
func SetUser(c echo.Context, user *User) {
	c.Set(contextKeyUser, user)
}

// UserFromContext は AuthMiddleware が格納したユーザーを返す。
func UserFromContext(c echo.Context) (*User, bool) {
	user, ok := c.Get(contextKeyUser).(*User)
	return user, ok && user != nil
}

// NewSessionCookie は s を保持する、名前が name（CookieNamesFor の Session）のセッション Cookie を作る。
// secure は HTTPS 配信時に true とする（c.Scheme() == "https" などで判定）。
func NewSessionCookie(name string, s *Session, secure bool) *http.Cookie {
	maxAge := int(time.Until(s.ExpiresAt).Seconds())
	if maxAge <= 0 {
		// MaxAge 0 は「属性なし」を意味するため、期限切れは即時削除の -1 にする。
		maxAge = -1
	}
	return &http.Cookie{
		Name:     name,
		Value:    s.ID,
		Path:     "/",
		Expires:  s.ExpiresAt,
		MaxAge:   maxAge,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// SetSessionCookie はレスポンスに、名前が name のセッション Cookie を設定する。
func SetSessionCookie(c echo.Context, name string, s *Session, secure bool) {
	c.SetCookie(NewSessionCookie(name, s, secure))
}

// ClearSessionCookie はブラウザに、名前が name のセッション Cookie を削除させる。
func ClearSessionCookie(c echo.Context, name string, secure bool) {
	c.SetCookie(&http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// unauthorized はリクエストの種類に応じた未認証レスポンスを返す。
func unauthorized(c echo.Context, cfg MiddlewareConfig) error {
	if cfg.LoginPath != "" {
		req := c.Request()
		if req.Header.Get("HX-Request") == "true" {
			c.Response().Header().Set("HX-Redirect", cfg.LoginPath)
			return echo.ErrUnauthorized
		}
		if wantsHTML(req) {
			return c.Redirect(http.StatusSeeOther, cfg.LoginPath)
		}
	}
	return echo.ErrUnauthorized
}

// wantsHTML はリクエストがブラウザによる HTML ページの取得かを判定する。
func wantsHTML(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return strings.Contains(req.Header.Get(echo.HeaderAccept), echo.MIMETextHTML)
}
