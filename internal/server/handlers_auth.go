package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
)

// loginData は login.html に渡すデータ。
type loginData struct {
	Username string
	// Desktop はデスクトップモードか。「パスワードを忘れたとき」をサイト管理者への問い合わせではなく、
	// この PC で再設定する画面（handlers_recovery.go）へのリンクにする。
	Desktop bool
	// Recovery は「パスワードを忘れたとき」の画面として表示するときの内容。ログイン画面なら nil。
	Recovery *recoveryView
}

// handleSetLanguage stores the selected interface language and returns to a local admin page.
func (s *Server) handleSetLanguage(c echo.Context) error {
	lang := i18n.Normalize(c.QueryParam("lang"))
	cookie := &http.Cookie{Name: s.cookies.Lang, Value: string(lang), Path: "/admin", MaxAge: 31536000, HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode}
	c.SetCookie(cookie)
	returnTo := c.QueryParam("return")
	if returnTo == "" || !strings.HasPrefix(returnTo, "/admin") || strings.HasPrefix(returnTo, "//") {
		returnTo = "/admin"
	}
	if _, err := url.ParseRequestURI(returnTo); err != nil {
		returnTo = "/admin"
	}
	return c.Redirect(http.StatusSeeOther, returnTo)
}

// dummyHash はユーザーが存在しないときにも bcrypt の比較を行い、
// 応答時間の差からユーザー名の有無を推測されないようにするためのハッシュ。
var dummyHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword("sorus-dummy-password")
	if err != nil {
		panic(err)
	}
	return h
})

// loginNextMaxAge はログイン後に戻る先（?next=）を覚えておく時間（秒）。
const loginNextMaxAge = 600

// loginNextCookieName はログイン後に戻る先を覚えておく Cookie の名前。ログインのフォーム（login.html）は action に
// クエリを付けずに送るため、ログイン画面を開いたときの ?next= をこの Cookie で POST まで持ち越す。
func (s *Server) loginNextCookieName() string { return s.cookies.Session + "_next" }

// safeLoginNext はログイン後に戻る先として next を受け付けるかを返す。別のサイトへ送る開いたリダイレクトにならないよう、
// このサイトの OAuth の同意画面（OAuthAuthorizePath）と管理画面（ログイン・ログアウト以外）のパスだけを受け付ける。
func safeLoginNext(next string) (string, bool) {
	if next == "" || len(next) > 4096 || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\r\n\t ") {
		return "", false
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	switch {
	case u.Path == OAuthAuthorizePath:
	case (u.Path == "/admin" || strings.HasPrefix(u.Path, "/admin/")) && path.Clean(u.Path) == strings.TrimSuffix(u.Path, "/") &&
		!strings.HasPrefix(u.Path, auth.DefaultLoginPath) && u.Path != "/admin/logout":
	default:
		return "", false
	}
	return next, true
}

// clearLoginNext はログイン後に戻る先の Cookie を消す。
func (s *Server) clearLoginNext(c echo.Context) {
	c.SetCookie(&http.Cookie{Name: s.loginNextCookieName(), Value: "", Path: auth.DefaultLoginPath, MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode})
}

// loginNext は POST /admin/login で、ログイン後に戻る先を返す（フォームの next、無ければログイン画面を開いたときの Cookie）。
func (s *Server) loginNext(c echo.Context) (string, bool) {
	if next, ok := safeLoginNext(c.FormValue("next")); ok {
		return next, true
	}
	if cookie, err := c.Cookie(s.loginNextCookieName()); err == nil {
		if raw, err := url.QueryUnescape(cookie.Value); err == nil {
			return safeLoginNext(raw)
		}
	}
	return "", false
}

// handleLoginPage はログイン画面を返す。有効なセッションがあれば記事一覧（?next= があればその画面）へ送る。
// ?next= はログインした後に戻る先（OAuth の同意画面など）で、POST まで Cookie で持ち越す。
func (s *Server) handleLoginPage(c echo.Context) error {
	next, hasNext := safeLoginNext(c.QueryParam("next"))
	if cookie, err := c.Cookie(s.cookies.Session); err == nil {
		if _, err := auth.ValidateSession(s.db, cookie.Value); err == nil {
			if hasNext {
				return c.Redirect(http.StatusSeeOther, next)
			}
			return c.Redirect(http.StatusSeeOther, "/admin/articles")
		}
	}
	if hasNext {
		c.SetCookie(&http.Cookie{Name: s.loginNextCookieName(), Value: url.QueryEscape(next), Path: auth.DefaultLoginPath,
			MaxAge: loginNextMaxAge, HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode})
	}
	// ?next= の無いログイン画面（表示言語の切り替えで開き直したときなど）では、覚えた戻り先を消さない。
	// 戻り先は loginNextMaxAge で切れ、ログインに使ったら消す。
	// ?username= はユーザー名の欄にあらかじめ入れておく値（「パスワードを忘れたとき」で変えた後の戻り先が付ける）。
	// ユーザー名に使える形式のときだけ受け付ける。
	username := normalizeUsername(c.QueryParam("username"))
	if !auth.ValidUsername(username) {
		username = ""
	}
	return s.render(c, http.StatusOK, "login", pageTitle(c, "login"), loginData{Username: username, Desktop: s.cfg.Desktop})
}

// normalizeUsername は、ログイン・パスワードの再設定で入力されたユーザー名の前後の空白を除き、全角の英数字・記号を
// 半角にする。ユーザー名は半角英数字・-・_ だけ（auth.ValidUsername）のため、日本語入力のまま打った全角の
// 「ｈａｎａｋｏ」も「hanako」として扱う（全角のままでは、どのユーザーにも一致せずログインできない）。
func normalizeUsername(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r >= '！' && r <= '～':
			return r - '！' + '!'
		case r == '　':
			return ' '
		}
		return r
	}, s))
}

// handleLogin はユーザー名とパスワードを検証し、セッション Cookie を発行する。
//
// 総当たりを抑えるため、パスワードの誤りを送り元ごと・ユーザー名ごとに数え（oauth_rate_limit.go）、上限に達したら
// 区切りが変わるまで 429 で断る。断るときはパスワードを照合しない（bcrypt の計算で CPU を使い尽くされない）。
// 正しいパスワードでログインできたら、そのユーザー名の数を数え直す。
func (s *Server) handleLogin(c echo.Context) error {
	username := normalizeUsername(c.FormValue("username"))
	password := c.FormValue("password")

	user, err := s.findUserByUsername(username)
	if err != nil {
		return err
	}
	limits := s.oauthLimits
	if retry, blocked := limits.adminLoginBlocked(c.Request(), username, user != nil); blocked {
		return s.renderLoginTooManyRequests(c, retry, username)
	}
	hash := dummyHash()
	if user != nil {
		hash = user.PasswordHash
	}
	if !auth.CheckPassword(password, hash) || user == nil {
		limits.adminLoginFailed(c.Request(), username, user != nil)
		return s.renderError(c, http.StatusUnauthorized, "login", pageTitle(c, "login"),
			i18n.Translate(string(language(c)), "login_error"), loginData{Username: username, Desktop: s.cfg.Desktop})
	}
	limits.adminLoginSucceeded(username)

	// 以前のセッションが残っていれば破棄し、ログインのたびに新しい ID を発行する。
	if cookie, err := c.Cookie(s.cookies.Session); err == nil {
		if err := auth.DeleteSession(s.db, cookie.Value); err != nil {
			return err
		}
	}
	session, err := auth.CreateSession(s.db, user.ID, s.cfg.SessionDuration)
	if err != nil {
		return err
	}
	auth.SetSessionCookie(c, s.cookies.Session, session, s.cfg.SecureCookie)
	if next, ok := s.loginNext(c); ok {
		s.clearLoginNext(c)
		return c.Redirect(http.StatusSeeOther, next)
	}
	return c.Redirect(http.StatusSeeOther, "/admin/articles")
}

// renderLoginTooManyRequests は、パスワードの誤りが続いて受け付けを止めているときのログイン画面を 429 で返す
// （Retry-After 付き）。待つ時間は分単位で切り上げて案内する。
func (s *Server) renderLoginTooManyRequests(c echo.Context, retry time.Duration, username string) error {
	setRetryAfter(c, retry)
	minutes := int((retry + time.Minute - 1) / time.Minute)
	msg := fmt.Sprintf("ログインの失敗が続いたため、しばらく受け付けを止めています。%d分ほど待ってから、もう一度お試しください。", minutes)
	if language(c) == "en" {
		msg = fmt.Sprintf("Too many failed sign-in attempts. Please wait about %d minute(s), then try again.", minutes)
	}
	return s.renderError(c, http.StatusTooManyRequests, "login", pageTitle(c, "login"), msg,
		loginData{Username: username, Desktop: s.cfg.Desktop})
}

// handleLogout はセッションを失効させてログイン画面へ戻す。
func (s *Server) handleLogout(c echo.Context) error {
	if cookie, err := c.Cookie(s.cookies.Session); err == nil {
		if err := auth.DeleteSession(s.db, cookie.Value); err != nil {
			return err
		}
	}
	auth.ClearSessionCookie(c, s.cookies.Session, s.cfg.SecureCookie)
	return c.Redirect(http.StatusSeeOther, "/admin/login?flash=logged_out")
}

// findUserByUsername はユーザーを取得する。存在しなければ nil を返す。
func (s *Server) findUserByUsername(username string) (*auth.User, error) {
	if username == "" {
		return nil, nil
	}
	var u auth.User
	err := s.db.Get(&u, `SELECT id, username, display_name, password_hash, role, created_at
		FROM users WHERE username = ?`, username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}
