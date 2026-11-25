package server

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/desktop"
)

const (
	// recoveryPath はデスクトップモードの「パスワードを忘れたとき」の画面と、新しいパスワードの送信先。
	// recoveryCodePath は再設定コードを作る送信先。サーバーモードでは登録せず 404 になる。
	recoveryPath     = "/admin/login/recovery"
	recoveryCodePath = recoveryPath + "/code"
	// RecoveryFileName は再設定コードを書き出すファイルの名前。データディレクトリに置く。
	RecoveryFileName = "パスワード再設定コード.txt"
	// recoveryCodeTTL は再設定コードの有効期間。
	recoveryCodeTTL = 15 * time.Minute
	// recoveryMaxAttempts は続けて間違えてよい回数。超えるとコードを捨て、recoveryLockout のあいだ作り直させない。
	// 作り直しても数え直さない（作り直しながら試し続けることを防ぐ）。
	recoveryMaxAttempts = 5
	// recoveryCodeDigits は再設定コードの桁数（4桁ずつ区切って見せる）。
	recoveryCodeDigits = 8
	// recoveryLockout は recoveryMaxAttempts 回間違えたあと、新しいコードを作らない時間。
	recoveryLockout = 15 * time.Minute
)

// recoveryClock は newRecoveryState が作る状態の現在時刻（テストで差し替える）。
var recoveryClock = time.Now

func init() {
	flashMessages["password_reset"] = "パスワードを変更しました。新しいパスワードでログインしてください。AIアプリはトークンの発行し直しが必要です"
}

var (
	errRecoveryNoCode  = errors.New("recovery: no active code")
	errRecoveryExpired = errors.New("recovery: code expired")
	errRecoveryLocked  = errors.New("recovery: too many wrong attempts")
	errRecoveryWrong   = errors.New("recovery: wrong code")
)

// recoveryCooldownError は、再設定コードを間違えすぎたため、Wait のあいだ新しいコードを作らないこと。
type recoveryCooldownError struct {
	Wait time.Duration
}

func (e *recoveryCooldownError) Error() string {
	return fmt.Sprintf("recovery: too many wrong attempts, retry in %s", e.Wait)
}

// recoveryState はデスクトップモードのパスワード再設定コード。コードはメモリにだけ持ち、利用者にはデータディレクトリの
// ファイルで渡す。そのファイルを読めること（この PC のこの利用者であること）を本人の確認とする。
type recoveryState struct {
	// file は再設定コードを書き出すファイル。
	file string
	// now は現在時刻を返す（テストで差し替える）。
	now func() time.Time

	mu      sync.Mutex
	code    string // 数字8桁。無効なら空。
	expires time.Time
	timer   *time.Timer
	// misses は続けて間違えた回数、lastMiss は最後に間違えた時刻。コードを作り直しても戻さない。正しいコードを
	// 入れたとき、recoveryLockout のあいだ間違えなかったとき、間違えすぎて lockedUntil を決めたときに戻す。
	misses   int
	lastMiss time.Time
	// lockedUntil は間違えすぎたため、新しいコードを作らない期限。
	lockedUntil time.Time
}

// newRecoveryState はデータディレクトリ dataDir に再設定コードを書き出す状態を作る。前回の起動で残った
// コードのファイルは、もう使えないため消す。
func newRecoveryState(dataDir string) *recoveryState {
	r := &recoveryState{file: filepath.Join(dataDir, RecoveryFileName), now: recoveryClock}
	os.Remove(r.file)
	return r
}

// issue は新しい再設定コードを作ってファイルに書き出す。前のコードは使えなくなる。間違えすぎたあと
// recoveryLockout のあいだは作らず、*recoveryCooldownError を返す。
func (r *recoveryState) issue() error {
	n, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		return err
	}
	code := fmt.Sprintf("%0*d", recoveryCodeDigits, n)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if wait := r.lockedUntil.Sub(now); wait > 0 {
		return &recoveryCooldownError{Wait: wait}
	}
	r.clearLocked()
	expires := now.Add(recoveryCodeTTL)
	nl := "\n"
	if runtime.GOOS == "windows" {
		nl = "\r\n"
	}
	text := strings.Join([]string{
		"sorus のパスワード再設定コード",
		"",
		"    " + groupCode(code),
		"",
		"このコードを sorus の「パスワードを忘れたとき」の画面に入力してください。",
		"使えるのは " + expires.Local().Format("15:04") + " まで（15分間）、1回だけです。",
		"使い終わると、このファイルは自動で消えます。",
		"",
		"心当たりがないときは、このファイルを消してください。",
	}, nl) + nl
	if err := os.WriteFile(r.file, []byte(text), 0o600); err != nil {
		return err
	}
	r.code, r.expires = code, expires
	// 期限が来たら、使われなかったコードのファイルも消す。
	r.timer = time.AfterFunc(recoveryCodeTTL, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.code == code {
			r.clearLocked()
		}
	})
	return nil
}

// active は使える再設定コードがあるかを返す。
func (r *recoveryState) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code != "" && r.now().Before(r.expires)
}

// verify は input が再設定コードと一致するかを確かめる。一致すればコードを捨て（1回だけ使える）、nil を返す。
// 期限切れなら errRecoveryExpired を返してコードを捨てる。続けて recoveryMaxAttempts 回間違えたら errRecoveryLocked を
// 返してコードを捨て、recoveryLockout のあいだ新しいコードを作らない。それ以外の不一致は errRecoveryWrong と、
// あと何回試せるかを返す。
func (r *recoveryState) verify(input string) (remaining int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == "" {
		return 0, errRecoveryNoCode
	}
	now := r.now()
	if !now.Before(r.expires) {
		r.clearLocked()
		return 0, errRecoveryExpired
	}
	if subtle.ConstantTimeCompare([]byte(normalizeRecoveryCode(input)), []byte(r.code)) == 1 {
		r.clearLocked()
		r.misses = 0
		return 0, nil
	}
	if now.Sub(r.lastMiss) >= recoveryLockout {
		r.misses = 0
	}
	r.misses++
	r.lastMiss = now
	if r.misses >= recoveryMaxAttempts {
		r.clearLocked()
		r.misses, r.lockedUntil = 0, now.Add(recoveryLockout)
		return 0, errRecoveryLocked
	}
	return recoveryMaxAttempts - r.misses, errRecoveryWrong
}

// clearLocked はコードを捨て、ファイルを消す。r.mu を持って呼ぶ。
func (r *recoveryState) clearLocked() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.code = ""
	os.Remove(r.file)
}

// recoveryWaitMessage は、新しいコードを作れるまで wait 待ってもらう文。分は切り上げる。
func recoveryWaitMessage(wait time.Duration) string {
	return fmt.Sprintf("しばらく待ってから、もう一度お試しください（あと %d 分）", (wait+time.Minute-1)/time.Minute)
}

// normalizeRecoveryCode は入力から数字だけを取り出す（区切りの「-」や空白を無視し、全角の数字は半角にする）。
func normalizeRecoveryCode(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			b.WriteRune(c)
		case c >= '０' && c <= '９':
			b.WriteRune('0' + (c - '０'))
		}
	}
	return b.String()
}

// groupCode は8桁のコードを 1234-5678 の形にする。
func groupCode(code string) string {
	return code[:4] + "-" + code[4:]
}

// recoveryView はログイン画面（login.html）の「パスワードを忘れたとき」の表示に渡すデータ。
type recoveryView struct {
	// CodeSent は再設定コードを作ったあと（コードの入力欄を出す）か。
	CodeSent bool
	// Folder・File は再設定コードを書き出したフォルダとファイルの名前（フォルダが開かなかったときの案内）。
	Folder string
	File   string
	// AskUsername はユーザーが2人以上いるため、再設定するユーザー名を入れてもらうか。
	AskUsername bool
	// Username はユーザーが1人のとき、再設定するその人のユーザー名。ログインに使うユーザー名を取り違えないよう
	// 画面に出し、パスワードの保存機能が新しいパスワードを正しいユーザー名で覚え直せるようフォームにも入れる。
	Username string
	// Errors は入力の誤り。
	Errors []string
}

// registerRecoveryRoutes は「パスワードを忘れたとき」を、CSRF 保護済み・ログイン不要の /admin グループに登録する
// （デスクトップモードだけ）。
func (care *desktopCare) registerRecoveryRoutes(admin *echo.Group) {
	admin.GET("/login/recovery", care.handleRecoveryPage)
	admin.POST("/login/recovery/code", care.handleRecoveryCode)
	admin.POST("/login/recovery", care.handleRecoveryReset)
}

func (care *desktopCare) recoveryView() (recoveryView, error) {
	v := recoveryView{CodeSent: care.recovery.active(), Folder: filepath.Dir(care.recovery.file), File: RecoveryFileName}
	var names []string
	if err := care.s.db.Select(&names, `SELECT username FROM users LIMIT 2`); err != nil {
		return v, err
	}
	v.AskUsername = len(names) > 1
	if len(names) == 1 {
		v.Username = names[0]
	}
	return v, nil
}

func (care *desktopCare) renderRecovery(c echo.Context, code int, v recoveryView, flashErr string) error {
	d := loginData{Recovery: &v}
	if flashErr != "" {
		return care.s.renderError(c, code, "login", pageTitle(c, "forgot_password_title"), flashErr, d)
	}
	return care.s.render(c, code, "login", pageTitle(c, "forgot_password_title"), d)
}

// handleRecoveryPage は「パスワードを忘れたとき」の画面を返す。コードを作ったあとなら、コードと新しいパスワードの入力欄を出す。
func (care *desktopCare) handleRecoveryPage(c echo.Context) error {
	v, err := care.recoveryView()
	if err != nil {
		return err
	}
	return care.renderRecovery(c, http.StatusOK, v, "")
}

// handleRecoveryCode は再設定コードを作ってデータディレクトリのファイルに書き出し、そのフォルダを開く。
// 間違えすぎたあとしばらくは作らず、あと何分待てばよいかを伝える。
func (care *desktopCare) handleRecoveryCode(c echo.Context) error {
	var cooldown *recoveryCooldownError
	if err := care.recovery.issue(); errors.As(err, &cooldown) {
		c.Logger().Warnf("recovery: refused to issue a code during the lockout (%s left)", cooldown.Wait.Round(time.Second))
		v, verr := care.recoveryView()
		if verr != nil {
			return verr
		}
		v.Errors = []string{recoveryWaitMessage(cooldown.Wait)}
		return care.renderRecovery(c, http.StatusTooManyRequests, v, "")
	} else if err != nil {
		c.Logger().Errorf("recovery: write code: %v", err)
		v, verr := care.recoveryView()
		if verr != nil {
			return verr
		}
		return care.renderRecovery(c, http.StatusInternalServerError, v, "再設定コードを作れませんでした。もう一度試してください")
	}
	c.Logger().Infof("recovery: issued a password reset code in %s", care.recovery.file)
	if err := desktop.OpenFolder(filepath.Dir(care.recovery.file)); err != nil {
		c.Logger().Warnf("recovery: %v", err)
	}
	return c.Redirect(http.StatusSeeOther, recoveryPath)
}

// handleRecoveryReset は再設定コードを確かめ、新しいパスワードを設定する。コードは1回だけ使え、使うとファイルを消す。
// 入力の誤り（パスワードが短いなど）ではコードを使わず、間違えた回数にも数えない。パスワードを変えると、その人の
// ログインと AI アプリの接続（API トークン）はすべて解除される（auth.UpdateUserPassword）。
func (care *desktopCare) handleRecoveryReset(c echo.Context) error {
	v, err := care.recoveryView()
	if err != nil {
		return err
	}
	v.CodeSent = true
	password := c.FormValue("password")
	switch {
	case len(password) < 8:
		v.Errors = append(v.Errors, "新しいパスワードは8文字以上にしてください")
	case len(password) > maxPasswordBytes:
		v.Errors = append(v.Errors, "新しいパスワードが長すぎます。半角72文字以内にしてください")
	case password != c.FormValue("password_confirm"):
		v.Errors = append(v.Errors, "確認のために入力したパスワードが一致しません")
	}
	user, err := care.recoveryUser(normalizeUsername(c.FormValue("username")))
	if err != nil {
		return err
	}
	if user == nil {
		v.Errors = append(v.Errors, "そのユーザー名のアカウントがありません")
	}
	if len(v.Errors) > 0 {
		return care.renderRecovery(c, http.StatusUnprocessableEntity, v, "")
	}

	remaining, err := care.recovery.verify(c.FormValue("code"))
	switch {
	case errors.Is(err, errRecoveryWrong):
		v.Errors = []string{fmt.Sprintf("再設定コードが違います。ファイルに書かれた8桁の数字を確かめてください（あと %d 回まで試せます）", remaining)}
		return care.renderRecovery(c, http.StatusUnprocessableEntity, v, "")
	case err != nil:
		v.CodeSent = false
		msg := "再設定コードがありません。もう一度「再設定コードを作る」を押してください"
		if errors.Is(err, errRecoveryExpired) {
			msg = "再設定コードの期限（15分）が過ぎました。もう一度「再設定コードを作る」を押してください"
		} else if errors.Is(err, errRecoveryLocked) {
			msg = "再設定コードを5回間違えたため、このコードは使えなくなりました。" + recoveryWaitMessage(recoveryLockout)
		}
		v.Errors = []string{msg}
		return care.renderRecovery(c, http.StatusUnprocessableEntity, v, "")
	}
	if err := auth.UpdateUserPassword(care.s.db, user.ID, password); err != nil {
		// 何も変えていない（パスワード・ログイン・AI アプリの接続はそのまま）。コードは使い終えたため作り直してもらう。
		c.Logger().Errorf("recovery: reset password of %s: %v", user.Username, err)
		v.CodeSent = false
		v.Errors = []string{"パスワードを変えられませんでした。パスワードは前のままです。もう一度「再設定コードを作る」を押してやり直してください"}
		return care.renderRecovery(c, http.StatusInternalServerError, v, "")
	}
	c.Logger().Infof("recovery: password of %s was reset with a recovery code", user.Username)
	// ログイン画面には、パスワードを変えたユーザー名を入れた状態で戻す（ユーザー名の取り違えでログインできないことを防ぐ）。
	return c.Redirect(http.StatusSeeOther, auth.DefaultLoginPath+"?flash=password_reset&username="+url.QueryEscape(user.Username))
}

// recoveryUser はパスワードを再設定するユーザーを返す。ユーザーが1人なら username を問わずその人、2人以上なら
// username の人（いなければ nil）。
func (care *desktopCare) recoveryUser(username string) (*auth.User, error) {
	users, err := auth.ListUsers(care.s.db)
	if err != nil {
		return nil, err
	}
	if len(users) == 1 {
		return &users[0], nil
	}
	return care.s.findUserByUsername(username)
}

// dataDir はデータディレクトリ（DB ファイルのあるディレクトリ）を返す。インメモリ DB なら desktop.DataDir。
func (s *Server) dataDir() string {
	var rows []struct {
		Seq  int    `db:"seq"`
		Name string `db:"name"`
		File string `db:"file"`
	}
	if err := s.db.Select(&rows, "PRAGMA database_list"); err == nil {
		for _, r := range rows {
			if r.Name == "main" && r.File != "" {
				return filepath.Dir(r.File)
			}
		}
	}
	dir, err := desktop.DataDir()
	if err != nil {
		return os.TempDir()
	}
	return dir
}
