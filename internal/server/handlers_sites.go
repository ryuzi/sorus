package server

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/labstack/echo/v4"

	"sorus/internal/desktop"
	"sorus/internal/i18n"
)

const (
	// sitesPagePath はデスクトップモードの「設定」→「サイト」の画面と、「新しいサイトを作る」の送信先。
	sitesPagePath = "/admin/settings/sites"
	// sitesDeletePath は現在のサイトを削除する確認画面と送信先。
	sitesDeletePath = "/admin/settings/sites/delete"
)

func init() {
	for _, name := range []string{"settings_sites", "settings_sites_delete"} {
		if !slices.Contains(pageNames, name) {
			pageNames = append(pageNames, name)
		}
	}
	flashMessages["site_deleted"] = "サイトを削除しました"
	flashKeys["site_deleted"] = "site_deleted"
	flashMessages["site_forgotten"] = "一覧から外しました。フォルダとその中のデータは、そのまま残しています。"
	flashKeys["site_forgotten"] = "site_forgotten_short"
}

// registerSitesRoutes はデスクトップモードで Config.Sites が渡されているときだけ、「サイト」画面のルートを登録する。
// Config.Sites が nil（サーバーモード、または複数サイトを使わないテスト）のときは何も登録せず 404 にする。
func (s *Server) registerSitesRoutes(authed *echo.Group, adminOnly echo.MiddlewareFunc) {
	if s.cfg.Sites == nil {
		return
	}
	authed.GET("/settings/sites", s.handleSitesPage, adminOnly)
	authed.POST("/settings/sites", s.handleSiteCreate, adminOnly)
	authed.GET("/settings/sites/delete", s.handleSiteDeletePage, adminOnly)
	authed.POST("/settings/sites/delete", s.handleSiteDelete, adminOnly)
	authed.POST("/settings/sites/:id/delete", s.handleUnnamedSiteDelete, adminOnly)
	authed.POST("/settings/sites/:id/forget", s.handleSiteForget, adminOnly)
}

// siteMenuItem は管理画面のヘッダーのサイト切り替えメニューに並べる1つのサイト。
type siteMenuItem struct {
	Name     string
	AdminURL string
	Current  bool
}

// layoutSites は管理画面のヘッダーのサイト切り替えメニューに出す一覧を返す。
// サイトが2つ以上あるときだけ開けたサイトを並べ、1つ以下のときは nil を返す（M3）。
func (s *Server) layoutSites(lang string) []siteMenuItem {
	list := s.cfg.Sites.List()
	if len(list) < 2 {
		return nil
	}
	items := make([]siteMenuItem, 0, len(list))
	for _, si := range list {
		if si.OpenError != "" {
			continue
		}
		url := si.AdminURL
		if url == "" {
			url = "/admin"
		}
		items = append(items, siteMenuItem{
			Name:     siteDisplayName(lang, si),
			AdminURL: url,
			Current:  si.Current,
		})
	}
	return items
}

// siteDisplayName は画面に出すサイト名を返す。「はじめに」が済んでおらず Name が空なら「名前の無いサイト（作成日）」を返す。
// サイト名を読めなかったときは、済んでいないとは別の「サイトの情報を読めませんでした」を返す。
func siteDisplayName(lang string, si SiteInfo) string {
	if si.Name != "" {
		return si.Name
	}
	if si.NameError != "" {
		return i18n.Translate(lang, "site_name_unreadable")
	}
	dateLayout := "2006年1月2日"
	if i18n.Normalize(lang) == i18n.EN {
		dateLayout = "2006-01-02"
	}
	return i18n.Translate(lang, "site_unnamed", si.Created.Local().Format(dateLayout))
}

// appPaths はデスクトップモードのアプリ全体のパス（sorus.log や「削除したサイトのバックアップ」の場所）を返す。
// 2つ目以降のサイト（sites/<id>/）から呼ばれたときは、親のデータディレクトリを指す。Config.LogPath があれば優先する。
func (s *Server) appPaths() desktop.AppPaths {
	dir := s.dataDir()
	if filepath.Base(filepath.Dir(dir)) == "sites" {
		dir = filepath.Dir(filepath.Dir(dir))
	}
	p := desktop.AppPathsIn(dir)
	if s.cfg.LogPath != "" {
		p.Log = s.cfg.LogPath
	}
	return p
}

// siteRowView は「サイト」画面の一覧に並べる1つのサイト。
type siteRowView struct {
	ID               string
	Name             string
	Unnamed          bool
	AdminURL         string
	Current          bool
	OpenError        string
	OpenSummary      string
	Dir              string
	NameUnreadable   bool
	NameError        string
	CanDeleteUnnamed bool
	CanForget        bool
}

// sitesPageData は「サイト」画面（settings_sites.html）に渡すデータ。
type sitesPageData struct {
	Sites            []siteRowView
	LogPath          string
	CanDeleteCurrent bool
	Error            string
	// Notice は一覧から外したあとの案内。クエリの文字列は使わない。
	Notice string
}

func (s *Server) sitesPageData(lang, errMsg string) sitesPageData {
	list := s.cfg.Sites.List()
	openCount := 0
	for _, si := range list {
		if si.OpenError == "" {
			openCount++
		}
	}
	rows := make([]siteRowView, len(list))
	for i, si := range list {
		unnamed := si.OpenError == "" && si.NameError == "" && si.Name == ""
		kind := si.OpenKind
		if si.OpenError != "" && kind == "" {
			kind = ClassifyOpenError(errors.New(si.OpenError))
		}
		summary := ""
		if si.OpenError != "" {
			summary = openFailureSummary(lang, kind)
		}
		rows[i] = siteRowView{
			ID:               si.ID,
			Name:             siteDisplayName(lang, si),
			Unnamed:          unnamed,
			AdminURL:         si.AdminURL,
			Current:          si.Current,
			OpenError:        si.OpenError,
			OpenSummary:      summary,
			Dir:              si.Dir,
			NameUnreadable:   si.NameError != "",
			NameError:        si.NameError,
			CanDeleteUnnamed: !si.Current && unnamed && si.ID != "" && openCount >= 2,
			CanForget:        si.OpenError != "" && si.ID != "" && len(list) >= 2 && openCount >= 1,
		}
	}
	return sitesPageData{
		Sites:            rows,
		LogPath:          s.appPaths().Log,
		CanDeleteCurrent: openCount >= 2,
		Error:            errMsg,
	}
}

// openFailureSummary は開けなかった種類 kind の、画面に出す分かりやすい説明を返す。
func openFailureSummary(lang, kind string) string {
	key := map[string]string{
		OpenFailureDamaged:    "site_open_failed_damaged",
		OpenFailureMissing:    "site_open_failed_missing",
		OpenFailurePermission: "site_open_failed_permission",
		OpenFailureAddress:    "site_open_failed_address",
	}[kind]
	if key == "" {
		key = "site_open_failed"
	}
	return i18n.Translate(lang, key)
}

func (s *Server) renderSitesError(c echo.Context, code int, msg string) error {
	lang := string(language(c))
	d := s.sitesPageData(lang, msg)
	return s.renderError(c, code, "settings_sites", i18n.Translate(lang, "sites_title"), msg, d)
}

// handleSitesPage は「設定」→「サイト」の画面を返す。
func (s *Server) handleSitesPage(c echo.Context) error {
	lang := string(language(c))
	data := s.sitesPageData(lang, "")
	if c.QueryParam("flash") == "site_forgotten" {
		data.Notice = i18n.Translate(lang, "site_forgotten")
	}
	return s.render(c, http.StatusOK, "settings_sites", i18n.Translate(lang, "sites_title"), data)
}

// handleSiteCreate は「新しいサイトを作る」。新しいサイトを作って動かし始め、そのサイトの管理画面（はじめに）へ 303 で送る。
func (s *Server) handleSiteCreate(c echo.Context) error {
	info, err := s.cfg.Sites.Create()
	if err != nil {
		return s.renderSitesError(c, http.StatusUnprocessableEntity, err.Error())
	}
	return c.Redirect(http.StatusSeeOther, info.AdminURL)
}

// siteDeletePageData は現在のサイトの削除確認画面（settings_sites_delete.html）に渡すデータ。
type siteDeletePageData struct {
	SiteName     string
	BackupFolder string
	TypedName    string
	Error        string
}

func (s *Server) siteDeletePageData(siteName, typedName, errMsg string) siteDeletePageData {
	folder := s.deleteBackupFolder("")
	return siteDeletePageData{
		SiteName:     siteName,
		BackupFolder: folder,
		TypedName:    typedName,
		Error:        errMsg,
	}
}

// currentSiteState は現在のサイトの ID・サイト名・削除できるか（開いているサイトが2つ以上あるか）を返す。
func (s *Server) currentSiteState() (id, name string, canDelete bool) {
	id = s.cfg.SiteKey
	openCount := 0
	for _, si := range s.cfg.Sites.List() {
		if si.OpenError == "" {
			openCount++
		}
		if si.Current {
			if si.ID != "" {
				id = si.ID
			}
			name = si.Name
		}
	}
	if name == "" {
		name = s.getSetting("site_name", s.getSiteName())
	}
	if name == "" {
		name = DefaultSiteName
	}
	return id, name, openCount >= 2
}

// handleSiteDeletePage は「このサイトを削除」の確認画面（GET）を返す。
func (s *Server) handleSiteDeletePage(c echo.Context) error {
	lang := string(language(c))
	_, name, canDelete := s.currentSiteState()
	if !canDelete {
		return s.renderSitesError(c, http.StatusUnprocessableEntity, i18n.Translate(lang, "site_delete_last_err"))
	}
	return s.render(c, http.StatusOK, "settings_sites_delete", i18n.Translate(lang, "site_delete_title"),
		s.siteDeletePageData(name, "", ""))
}

// handleSiteDelete は現在のサイトを削除する（POST）。入力されたサイト名が一致しなければ削除せず確認画面を返す。
// 削除に成功したら、外部の CSS やスクリプトを読み込まない完了画面を返す。
func (s *Server) handleSiteDelete(c echo.Context) error {
	lang := string(language(c))
	curID, expectedName, canDelete := s.currentSiteState()
	if otherID := c.FormValue("id"); otherID != "" && otherID != curID && c.FormValue("site_name") == "" {
		return s.deleteUnnamedSite(c, otherID)
	}
	if !canDelete {
		return s.renderSitesError(c, http.StatusUnprocessableEntity, i18n.Translate(lang, "site_delete_last_err"))
	}
	typed := c.FormValue("site_name")
	if typed == "" {
		typed = c.FormValue("name")
	}
	if typed != expectedName {
		msg := i18n.Translate(lang, "site_delete_name_mismatch", expectedName)
		d := s.siteDeletePageData(expectedName, typed, msg)
		return s.renderError(c, http.StatusUnprocessableEntity, "settings_sites_delete", i18n.Translate(lang, "site_delete_title"), msg, d)
	}
	extendDeadlines(c)
	backupFile, err := s.cfg.Sites.Delete(curID)
	if err != nil {
		msg := err.Error()
		d := s.siteDeletePageData(expectedName, typed, msg)
		return s.renderError(c, http.StatusUnprocessableEntity, "settings_sites_delete", i18n.Translate(lang, "site_delete_title"), msg, d)
	}
	nextURL, nextName := s.nextRemainingSite(lang, curID)
	var buf bytes.Buffer
	err = siteDeletedTemplate.Execute(&buf, siteDeletedView{
		Lang:        lang,
		JA:          i18n.Normalize(lang) == i18n.JA,
		DeletedName: expectedName,
		BackupFile:  backupFile,
		NextURL:     nextURL,
		NextName:    nextName,
	})
	if err != nil {
		return err
	}
	h := c.Response().Header()
	h.Set("Cache-Control", "no-store")
	// 削除したサイトの待ち受けはすぐ閉じるため、同じ接続で次の要求を送らせない。
	h.Set("Connection", "close")
	return c.HTMLBlob(http.StatusOK, buf.Bytes())
}

// nextRemainingSite はサイト deletedID を削除したあとに開く、残った先頭のサイトの管理画面 URL と表示名を返す。
func (s *Server) nextRemainingSite(lang, deletedID string) (adminURL, name string) {
	list := s.cfg.Sites.List()
	for _, si := range list {
		if si.OpenError != "" || si.Current || (deletedID != "" && si.ID == deletedID) {
			continue
		}
		return si.AdminURL, siteDisplayName(lang, si)
	}
	for _, si := range list {
		if si.OpenError == "" {
			return si.AdminURL, siteDisplayName(lang, si)
		}
	}
	return "/admin", DefaultSiteName
}

// handleUnnamedSiteDelete は、まだ「はじめに」が済んでいない（Name が空の）別のサイトを削除する。
func (s *Server) handleUnnamedSiteDelete(c echo.Context) error {
	return s.deleteUnnamedSite(c, c.Param("id"))
}

func (s *Server) deleteUnnamedSite(c echo.Context, id string) error {
	lang := string(language(c))
	_, _, canDelete := s.currentSiteState()
	if !canDelete {
		return s.renderSitesError(c, http.StatusUnprocessableEntity, i18n.Translate(lang, "site_delete_last_err"))
	}
	var target *SiteInfo
	for _, si := range s.cfg.Sites.List() {
		if si.ID == id && !si.Current {
			cp := si
			target = &cp
			break
		}
	}
	if target == nil || target.OpenError != "" {
		return echo.ErrNotFound
	}
	// サイト名を読めなかったことは、「はじめに」が済んでいないことではない。入力なしの削除はしない。
	if target.NameError != "" {
		return s.renderSitesError(c, http.StatusUnprocessableEntity, i18n.Translate(lang, "site_name_unreadable"))
	}
	if target.Name != "" {
		return s.renderSitesError(c, http.StatusForbidden, i18n.Translate(lang, "site_delete_other_named_err"))
	}
	extendDeadlines(c)
	if _, err := s.cfg.Sites.Delete(target.ID); err != nil {
		return s.renderSitesError(c, http.StatusUnprocessableEntity, err.Error())
	}
	return c.Redirect(http.StatusSeeOther, sitesPagePath+"?flash=site_deleted")
}

// handleSiteForget は、開けなかったサイトを一覧（sites.json）から外す。フォルダは消さない。
// 開いているサイトや、開けたサイトへの送信は、ボタンが無くても受け付けない。
func (s *Server) handleSiteForget(c echo.Context) error {
	id := c.Param("id")
	var target *SiteInfo
	for _, si := range s.cfg.Sites.List() {
		if si.ID == id && !si.Current && si.OpenError != "" {
			cp := si
			target = &cp
			break
		}
	}
	if target == nil {
		return echo.ErrNotFound
	}
	if err := s.cfg.Sites.Forget(id); err != nil {
		return s.renderSitesError(c, http.StatusUnprocessableEntity, err.Error())
	}
	return c.Redirect(http.StatusSeeOther, sitesPagePath+"?flash=site_forgotten")
}

type siteDeletedView struct {
	Lang        string
	JA          bool
	DeletedName string
	BackupFile  string
	NextURL     string
	NextName    string
}

// siteDeletedTemplate はサイトを削除した直後に返す完了画面。そのすぐあとに待ち受けが閉じるため、
// desktopQuitPage と同じく /static などの外部ファイルを読み込まない自己完結した HTML にする。
var siteDeletedTemplate = template.Must(template.New("site_deleted").Parse(`<!doctype html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="3;url={{.NextURL}}">
<title>{{if .JA}}サイトを削除しました{{else}}Site deleted{{end}}</title>
<style>
body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
  background: #fafafa; color: #262626; font-family: system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 32rem; margin: 1rem; padding: 2rem; border: 1px solid #e5e5e5; border-radius: 1rem; background: #fff; }
h1 { margin: 0 0 .75rem; font-size: 1.25rem; }
p { margin: .5rem 0 0; line-height: 1.7; color: #525252; font-size: .9375rem; }
code { display: block; margin-top: .5rem; padding: .5rem .75rem; border: 1px solid #e5e5e5; border-radius: .5rem;
  background: #f5f5f5; color: #171717; font-size: .8125rem; word-break: break-all; }
.action { margin-top: 1.25rem; }
a { display: inline-block; padding: .5rem 1rem; border-radius: .5rem; background: #262626; color: #fff;
  text-decoration: none; font-size: .875rem; font-weight: 600; }
a:hover { background: #171717; }
</style>
</head>
<body>
<main>
<h1>{{if .JA}}「{{.DeletedName}}」を削除しました{{else}}Deleted "{{.DeletedName}}"{{end}}</h1>
<p>{{if .JA}}削除の前の完全バックアップを次の場所に保存しました。{{else}}A complete backup was saved before deletion:{{end}}</p>
<code>{{.BackupFile}}</code>
<p>{{if .JA}}新しいサイトを作るときに「バックアップから始める」を選ぶと、このバックアップから元に戻せます。{{else}}You can restore it by choosing "Start from a backup" when you create a new site.{{end}}</p>
<p class="action"><a id="site-deleted-next" href="{{.NextURL}}">{{if .JA}}{{.NextName}} を開く{{else}}Open {{.NextName}}{{end}}</a></p>
</main>
<script>
setTimeout(function () {
  var a = document.getElementById('site-deleted-next');
  if (a && a.href) window.location.href = a.href;
}, 3000);
</script>
</body>
</html>
`))
