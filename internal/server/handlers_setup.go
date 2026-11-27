package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/markdown"
)

const (
	// SetupPath はデスクトップモードの「はじめに」画面（最初のユーザーの登録）の URL パス。
	SetupPath = "/admin/setup"
	// setupRestorePath は「はじめに」画面の「バックアップから始める」の送信先。
	setupRestorePath = SetupPath + "/restore"

	// maxSetupDisplayName・maxSetupSiteName は表示名・サイト名の上限の文字数。
	maxSetupDisplayName = 50
	maxSetupSiteName    = 100
	// maxPasswordBytes は bcrypt が扱えるパスワードの上限のバイト数。
	maxPasswordBytes = 72
)

func init() {
	if !slices.Contains(pageNames, "setup") {
		pageNames = append(pageNames, "setup")
	}
	flashMessages["setup_restored"] = "バックアップから復元しました。バックアップを作ったときのユーザー名とパスワードでログインしてください"
}

// setupData は setup.html に渡すデータ。パスワードは入力し直してもらうため持たない。
type setupData struct {
	DisplayName string
	Username    string
	SiteName    string
	Starter     string
	// Errors は入力の誤り（画面の上にまとめて出す）。
	Errors []string
	// Done は、すでに管理者がいるため登録できないことを伝える表示か。
	Done bool
	// CanRestore は「バックアップから始める」を出すか（ユーザーが1人もいないときだけ）。
	CanRestore bool
	// MaxRestoreSize・MaxRestoreBytes は「バックアップから始める」で選べるファイルの上限。
	MaxRestoreSize  string
	MaxRestoreBytes int64
}

// registerSetupRoutes は「はじめに」画面を、CSRF 保護済み・ログイン不要の /admin グループに登録する（デスクトップモードだけ）。
func (s *Server) registerSetupRoutes(admin *echo.Group) {
	admin.GET("/setup", s.handleSetupPage)
	admin.POST("/setup", s.handleSetup)
	admin.POST("/setup/restore", s.handleSetupRestore)
}

// setupDone は「はじめに」が済んでいる（管理者を登録済み）かを返す。古い版の取り込みで作られた著者のユーザーしか
// いなければ済んでいない（誰もログインできないため「はじめに」を出す）。sorus import と同じ auth.SetupDone で判定する。
func (s *Server) setupDone() (bool, error) {
	return auth.SetupDone(s.db)
}

// hasUsers はユーザーが1人でもいるかを返す。「バックアップから始める」はユーザーが1人もいない間だけ受け付ける
// （取り込みで作られたユーザーや記事があれば、確かめないまま置き換えない）。
func (s *Server) hasUsers() (bool, error) {
	var exists bool
	err := s.db.Get(&exists, `SELECT EXISTS (SELECT 1 FROM users)`)
	return exists, err
}

// requireSetup は、「はじめに」が済んでいなければ「はじめに」画面（「バックアップから始める」を含む）以外の /admin への
// 要求をすべて SetupPath へ送るミドルウェア。HTMX の要求には HX-Redirect ヘッダーを付けて 401 を返す。
func (s *Server) requireSetup(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if p := c.Request().URL.Path; p == SetupPath || p == setupRestorePath {
			return next(c)
		}
		done, err := s.setupDone()
		if err != nil {
			return err
		}
		if done {
			return next(c)
		}
		if c.Request().Header.Get("HX-Request") == "true" {
			c.Response().Header().Set("HX-Redirect", SetupPath)
			return echo.ErrUnauthorized
		}
		return c.Redirect(http.StatusSeeOther, SetupPath)
	}
}

// handleSetupPage は「はじめに」画面を返す。登録済みなら管理画面へ送る。
func (s *Server) handleSetupPage(c echo.Context) error {
	done, err := s.setupDone()
	if err != nil {
		return err
	}
	if done {
		return c.Redirect(http.StatusSeeOther, "/admin")
	}
	var d setupData
	if err := s.setRestoreOption(&d); err != nil {
		return err
	}
	if auth.ValidUsername(s.cfg.SuggestedUsername) {
		d.Username = strings.TrimSpace(s.cfg.SuggestedUsername)
	}
	// 環境変数などで決めたサイト名があれば初期値にする（既定の名前は入れず、自分のサイト名を考えてもらう）。
	if siteName := s.getSiteName(); siteName != DefaultSiteName {
		d.SiteName = siteName
	}
	return s.render(c, http.StatusOK, "setup", pageTitle(c, "setup_title"), d)
}

// handleSetup は最初のユーザーを管理者として登録し、サイト名を保存してログインさせる。
// 管理者が1人でもいれば何も変えずに 403 を返す（あとから「はじめに」をやり直して管理者を乗っ取れない）。
// 古い版の取り込みで作られた著者のユーザーしかいなければ登録でき、そのユーザーと同じユーザー名は入力の誤りとして断る。
func (s *Server) handleSetup(c echo.Context) error {
	d := setupData{
		DisplayName: strings.TrimSpace(c.FormValue("display_name")),
		Username:    strings.TrimSpace(c.FormValue("username")),
		SiteName:    strings.TrimSpace(c.FormValue("site_name")),
		Starter:     c.FormValue("site_starter"),
	}
	if d.Starter == "" {
		d.Starter = "blog"
	}
	if !slices.Contains([]string{"blog", "docs", "site", "empty"}, d.Starter) {
		d.Starter = "blog"
	}
	password := c.FormValue("password")
	if done, err := s.setupDone(); err != nil {
		return err
	} else if done {
		return s.renderSetupDone(c)
	}
	if d.Errors = validateSetup(d, password, c.FormValue("password_confirm")); len(d.Errors) > 0 {
		return s.renderSetupErrors(c, d)
	}

	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	user, err := auth.CreateFirstUser(tx, d.Username, d.DisplayName, password, auth.RoleAdmin)
	if errors.Is(err, auth.ErrSetupDone) {
		return s.renderSetupDone(c)
	}
	if errors.Is(err, auth.ErrUsernameTaken) {
		// 画面を返す前にトランザクションを終える（返す画面のために同じ DB を読む）。
		tx.Rollback()
		d.Errors = []string{"このユーザー名はすでに使われています。別のユーザー名にしてください"}
		return s.renderSetupErrors(c, d)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('site_name', ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, d.SiteName); err != nil {
		return err
	}
	if err := applySetupStarter(tx, user.ID, d.Starter); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.setSiteName(d.SiteName)

	session, err := auth.CreateSession(s.db, user.ID, s.cfg.SessionDuration)
	if err != nil {
		return err
	}
	auth.SetSessionCookie(c, s.cookies.Session, session, s.cfg.SecureCookie)
	return c.Redirect(http.StatusSeeOther, "/admin")
}

// applySetupStarter stores the selected starter's initial public structure and sample pages.
func applySetupStarter(tx *sqlx.Tx, userID, starter string) error {
	sections := []FrontSection{{Type: FrontSectionArticleList}}
	menu := []MenuItem{}
	theme := "modern"
	var pages []struct{ title, slug, body, parent string }
	switch starter {
	case "docs":
		theme = "tech"
		pages = append(pages, struct{ title, slug, body, parent string }{"はじめに", "guide", "# はじめに\n\nこのサイトの使い方をまとめます。", ""}, struct{ title, slug, body, parent string }{"インストール", "installation", "# インストール\n\n導入手順をここに記載します。", "guide"})
		sections = []FrontSection{{Type: FrontSectionHero, Pages: []string{"guide"}}, {Type: FrontSectionCards, Pages: []string{"guide", "installation"}}}
		menu = []MenuItem{{Label: "ガイド", Type: MenuItemGroup, Children: []MenuItem{{Label: "はじめに", Type: MenuItemPage, Target: "guide"}, {Label: "インストール", Type: MenuItemPage, Target: "installation"}}}}
	case "site":
		theme = "portal"
		pages = append(pages, struct{ title, slug, body, parent string }{"紹介", "about", "# このサイトについて\n\n活動や作品をご紹介します。", ""}, struct{ title, slug, body, parent string }{"お問い合わせ", "contact", "# お問い合わせ\n\n連絡先をご案内します。", ""})
		sections = []FrontSection{{Type: FrontSectionHero, Pages: []string{"about"}}, {Type: FrontSectionCards, Pages: []string{"about", "contact"}}, {Type: FrontSectionArticleList}}
		menu = []MenuItem{{Label: "紹介", Type: MenuItemPage, Target: "about"}, {Label: "お問い合わせ", Type: MenuItemPage, Target: "contact"}}
	case "empty":
		sections = []FrontSection{}
	}
	pageIDs := map[string]string{}
	for _, p := range pages {
		idBytes := make([]byte, 16)
		if _, err := rand.Read(idBytes); err != nil {
			return err
		}
		id := hex.EncodeToString(idBytes)
		pageIDs[p.slug] = id
		rendered, err := markdown.Render(p.body)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO articles (id,title,slug,content_markdown,content_html,author_id,category_id,status,kind,parent_id,published_at)
			VALUES (?,?,?,?,?,?,1,'published','page',NULLIF(?,''),CURRENT_TIMESTAMP)`, id, p.title, p.slug, p.body, rendered.HTML, userID, pageIDs[p.parent]); err != nil {
			return err
		}
	}
	menuJSON, _ := json.Marshal(menu)
	sectionsJSON, _ := json.Marshal(sections)
	settings := map[string]string{settingSiteMenu: string(menuJSON), settingFrontSections: string(sectionsJSON)}
	if starter == "docs" || starter == "site" {
		settings["theme"] = theme
		settings[settingFrontPage] = map[string]string{"docs": "guide", "site": "about"}[starter]
	}
	for key, value := range settings {
		if _, err := tx.Exec(`INSERT INTO settings(key,value,updated_at) VALUES(?,?,CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=CURRENT_TIMESTAMP`, key, value); err != nil {
			return err
		}
	}
	return nil
}

// handleSetupRestore は「はじめに」画面の「バックアップから始める」。別の PC で作ったバックアップ（Zip）を、
// アカウントを作る前のこの PC に復元し、ログイン画面へ送る（復元した DB にその人のアカウントがある）。
// ユーザーが1人もいない間だけ受け付け、いれば何も変えずに 403 を返す（あとから他人のデータで置き換えさせない。
// 古い版の取り込みで作られたユーザーと記事も、確かめないまま置き換えない）。
// フォームは設定画面の復元と同じく fetch（XMLHttpRequest）で X-CSRF-Token ヘッダーを付けて送り、検査と上限も同じ。
func (s *Server) handleSetupRestore(c echo.Context) error {
	if !s.backupMu.TryLock() {
		return errBackupBusy
	}
	defer s.backupMu.Unlock()
	if err := s.setupRestoreRefused(
		"はじめの設定はすでに済んでいるため、ここからは復元できません。ログインしてから「バックアップと復元」の画面で復元してください"); err != nil {
		return err
	}
	extendDeadlines(c)
	zipPath, err := s.receiveBackupUpload(c)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)
	// 受け取っている間に「はじめに」が済んでいたら、作ったばかりのアカウントを置き換えない。
	if err := s.setupRestoreRefused("はじめの設定がすでに済んでいるため、復元しませんでした"); err != nil {
		return err
	}
	if err := s.restoreUploadedBackup(c, zipPath); err != nil {
		return err
	}
	c.Logger().Infof("backup: restored from upload on the setup screen")
	return c.JSON(http.StatusOK, backupRestoreResponse{
		Message:  "バックアップから復元しました。バックアップを作ったときのユーザー名とパスワードでログインしてください",
		Redirect: auth.DefaultLoginPath + "?flash=setup_restored",
	})
}

// setupRestoreRefused は、ユーザーがいて「バックアップから始める」を受け付けないとき、その理由を伝える 403 を返す。
// ユーザーが1人もいなければ nil を返す。「はじめに」が済んでいれば doneMessage を、済んでいない（古い版の取り込みで
// 作られたユーザーしかいない）なら、先に「はじめに」を済ませてから設定画面で復元するよう伝える。
func (s *Server) setupRestoreRefused(doneMessage string) error {
	if has, err := s.hasUsers(); err != nil || !has {
		return err
	}
	done, err := s.setupDone()
	if err != nil {
		return err
	}
	if done {
		return echo.NewHTTPError(http.StatusForbidden, doneMessage)
	}
	return echo.NewHTTPError(http.StatusForbidden,
		"この PC の sorus にはすでにデータがあるため、ここからは復元できません。先に「はじめに」を済ませてログインし、「バックアップと復元」の画面で復元してください")
}

// setRestoreOption は「バックアップから始める」を出すか（ユーザーが1人もいないときだけ）と、選べるファイルの上限を d に入れる。
func (s *Server) setRestoreOption(d *setupData) error {
	has, err := s.hasUsers()
	if err != nil {
		return err
	}
	d.CanRestore = !has
	d.MaxRestoreSize, d.MaxRestoreBytes = formatByteSize(s.cfg.MaxRestoreSize), s.cfg.MaxRestoreSize
	return nil
}

// renderSetupErrors は入力の誤り（d.Errors）を出して「はじめに」画面を 422 で返し直す。
func (s *Server) renderSetupErrors(c echo.Context, d setupData) error {
	if err := s.setRestoreOption(&d); err != nil {
		return err
	}
	return s.render(c, http.StatusUnprocessableEntity, "setup", pageTitle(c, "setup_title"), d)
}

// renderSetupDone は、すでに登録済みのため「はじめに」を受け付けないことを 403 で伝える。
func (s *Server) renderSetupDone(c echo.Context) error {
	return s.render(c, http.StatusForbidden, "setup", pageTitle(c, "setup_title"), setupData{Done: true})
}

// validateSetup は「はじめに」画面の入力を検査し、誤りを画面に出す文言で返す。
func validateSetup(d setupData, password, confirm string) []string {
	var errs []string
	switch n := utf8.RuneCountInString(d.DisplayName); {
	case n == 0:
		errs = append(errs, "表示名（あなたの名前）を入力してください")
	case n > maxSetupDisplayName:
		errs = append(errs, "表示名は50文字以内にしてください")
	}
	if !auth.ValidUsername(d.Username) {
		errs = append(errs, "ユーザー名は半角の英字・数字と「-」「_」を使って、3〜32文字にしてください")
	}
	switch {
	case len(password) < 8:
		errs = append(errs, "パスワードは8文字以上にしてください")
	case len(password) > maxPasswordBytes:
		errs = append(errs, "パスワードが長すぎます。半角72文字以内にしてください")
	case password != confirm:
		errs = append(errs, "確認のために入力したパスワードが一致しません")
	}
	switch n := utf8.RuneCountInString(d.SiteName); {
	case n == 0:
		errs = append(errs, "サイト名を入力してください")
	case n > maxSetupSiteName:
		errs = append(errs, "サイト名は100文字以内にしてください")
	}
	return errs
}
