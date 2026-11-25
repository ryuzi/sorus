package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/backup"
	"sorus/internal/i18n"
)

const (
	// autoBackupFolderSetting は自動バックアップの保存先フォルダを保存する settings のキー。空なら自動バックアップしない。
	autoBackupFolderSetting = "backup_auto_folder"
	// autoBackupLastSetting は最後の自動バックアップの結果（autoBackupStatus の JSON）を保存する settings のキー。
	autoBackupLastSetting = "backup_auto_last"

	// autoBackupPath・autoBackupRunPath・autoBackupOffPath は保存先の保存・今すぐ作る・やめるの送信先。
	autoBackupPath    = backupPagePath + "/auto"
	autoBackupRunPath = autoBackupPath + "/run"
	autoBackupOffPath = autoBackupPath + "/off"
	// backupStatusPath はダッシュボードに読み込む、自動バックアップの状態の部分。
	backupStatusPath = "/admin/backup/status"

	// autoBackupStartDelay は起動してから最初の自動バックアップを確かめるまでの待ち時間。起動直後の画面の表示を
	// バックアップの書き出しで遅くしない。
	autoBackupStartDelay = 10 * time.Second
	// localTimeLayout・localTimeLayoutEN は画面に出す日時の形（日本語・英語）。
	localTimeLayout   = "2006年1月2日 15:04"
	localTimeLayoutEN = "Jan 2, 2006 15:04"
)

func init() {
	// 通知の文言は辞書（internal/i18n）の同じ名前のキーから表示言語で引く。flashMessages には ?flash= で受け付ける
	// キーとして、既定（日本語）の文言を入れておく。
	for _, key := range []string{"backup_auto_saved", "backup_auto_done", "backup_auto_failed", "backup_auto_busy", "backup_auto_off"} {
		flashMessages[key] = i18n.Translate(string(i18n.JA), key)
		flashKeys[key] = key
	}
}

// formatLocalTime は t をこの PC の時刻で、表示言語 lang の形にする。
func formatLocalTime(t time.Time, lang string) string {
	if i18n.Normalize(lang) == i18n.EN {
		return t.Local().Format(localTimeLayoutEN)
	}
	return t.Local().Format(localTimeLayout)
}

// desktopCare はデスクトップモードで、利用者が何もしなくてもデータと sorus を守る仕組み（自動バックアップ・
// パスワードの再設定・更新のお知らせ）の、サーバーごとの状態。
type desktopCare struct {
	s       *Server
	backups *backup.Scheduler
	// recovery はパスワード再設定コード（handlers_recovery.go）。
	recovery *recoveryState
}

// registerDesktopCareRoutes はデスクトップモードだけの自動バックアップ・パスワードの再設定・更新のお知らせを登録し、
// 自動バックアップと新しい版の確認をバックグラウンドで始める（Shutdown で止める）。パスワードの再設定はログイン前に
// 使うため CSRF 保護済み・ログイン不要の admin グループに、それ以外はログイン済みの authed グループに登録する。
//
// サーバーモードでは何も登録しない。ただしパスワードの再設定のパスは、ログインしていなくてもログイン画面へ
// 送らず 404 にする（サーバーモードにこの仕組みがあるように見せない）。
func (s *Server) registerDesktopCareRoutes(admin, authed *echo.Group) {
	if !s.cfg.Desktop {
		for _, p := range []string{recoveryPath, recoveryCodePath} {
			s.echo.Any(p, func(echo.Context) error { return echo.ErrNotFound })
		}
		return
	}
	care := &desktopCare{s: s, recovery: newRecoveryState(s.dataDir())}
	care.backups = &backup.Scheduler{
		Site:          s.cfg.SiteKey,
		DB:            s.db,
		UploadDir:     s.cfg.UploadDir,
		AttachmentDir: s.cfg.AttachmentDir,
		WorkDir:       s.dataDir(),
		Folder:        func() string { return s.getSetting(autoBackupFolderSetting, "") },
		TryLock: func() (func(), bool) {
			if !s.backupMu.TryRLock() {
				return nil, false
			}
			return s.backupMu.RUnlock, true
		},
		OnResult: s.saveAutoBackupResult,
	}

	adminOnly := auth.RequireRole(auth.RoleAdmin)
	authed.POST("/settings/backup/auto", care.handleAutoBackupSave, adminOnly)
	authed.POST("/settings/backup/auto/run", care.handleAutoBackupRun, adminOnly)
	authed.POST("/settings/backup/auto/off", s.handleAutoBackupOff, adminOnly)
	authed.GET("/backup/status", s.handleBackupStatus)
	care.registerRecoveryRoutes(admin)
	s.registerUpdateRoutes(authed)

	s.startCare = func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.echo.Server.RegisterOnShutdown(cancel)
		s.stopCare, s.careDone = cancel, make(chan struct{})
		go func(done chan struct{}) { defer close(done); care.runAutoBackups(ctx) }(s.careDone)
		s.startUpdateChecks(ctx)
	}
	s.startCare()
}

// runAutoBackups は少し待ってから自動バックアップのスケジューラーを動かす。
func (care *desktopCare) runAutoBackups(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(autoBackupStartDelay):
	}
	care.backups.Run(ctx)
}

// autoBackupStatus は最後の自動バックアップの結果。
type autoBackupStatus struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
	// File は作成したバックアップのファイル名。
	File string `json:"file,omitempty"`
	// Message は失敗した理由。Warning は成功したが古いバックアップを消せなかったときの知らせ。どちらも辞書
	// （internal/i18n）のキーを保存し、画面に出すときに表示言語で引く（MessageFor・WarningFor）。以前の版が保存した
	// 文言（日本語）はキーではないので、そのまま出す。
	Message string `json:"message,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// localizeStored は、保存した値 v が辞書のキーなら lang で引き、そうでなければ（以前の版が保存した文言）そのまま返す。
func localizeStored(lang, v string) string {
	if v != "" && i18n.Has(v) {
		return i18n.Translate(lang, v)
	}
	return v
}

// MessageFor は失敗した理由を表示言語 lang の文言で返す。
func (st autoBackupStatus) MessageFor(lang string) string { return localizeStored(lang, st.Message) }

// WarningFor は古いバックアップを消せなかった知らせを表示言語 lang の文言で返す。
func (st autoBackupStatus) WarningFor(lang string) string { return localizeStored(lang, st.Warning) }

// saveAutoBackupResult は自動バックアップの結果をログに残し、画面に出すために保存する。
func (s *Server) saveAutoBackupResult(r backup.Result) {
	st := autoBackupStatus{At: r.At.UTC().Truncate(time.Second), OK: r.Err == nil}
	if r.Err != nil {
		s.echo.Logger.Errorf("backup: automatic backup failed: %v", r.Err)
		st.Message = autoBackupFailureKey(r.Err)
	} else {
		s.echo.Logger.Infof("backup: automatic backup created: %s", r.File)
		st.File = filepath.Base(r.File)
	}
	if r.PruneErr != nil {
		s.echo.Logger.Warnf("backup: remove old automatic backups: %v", r.PruneErr)
		st.Warning = "backup_auto_prune_warning"
	}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := s.putSetting(autoBackupLastSetting, string(b)); err != nil {
		s.echo.Logger.Errorf("backup: save automatic backup status: %v", err)
	}
}

// autoBackupFailureKey は自動バックアップに失敗した理由を、利用者が次に何をすればよいか分かる文言の辞書のキーにする。
func autoBackupFailureKey(err error) string {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "backup_auto_err_no_space"
	case errors.Is(err, fs.ErrNotExist):
		return "backup_auto_err_not_found"
	case errors.Is(err, fs.ErrPermission):
		return "backup_auto_err_permission"
	}
	return "backup_auto_err_generic"
}

// putSetting は settings の key に value を保存する。
func (s *Server) putSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, key, value)
	return err
}

// autoBackupView は自動バックアップの設定と状態を、画面（settings_backup.html とダッシュボードの部分）に渡す形にしたもの。
type autoBackupView struct {
	// Folder は保存先フォルダ。空なら自動バックアップは止まっている。
	Folder string
	// Last は最後の結果。まだ1回も作っていなければ nil。日時は LastAtFor で表示言語の形にする。
	Last *autoBackupStatus
	// Keep は残す世代の数。
	Keep int
	// Candidates はこの PC で見つかった同期フォルダ（設定画面で1つ選ぶだけで保存先にできる）。
	Candidates []backup.CloudFolder
	// Error は保存先の入力の誤り。Input はそのときの入力。
	Error string
	Input string
}

// LastAtFor は最後のバックアップの日時を、この PC の時刻で表示言語 lang の形にして返す（まだ無ければ空）。
func (v autoBackupView) LastAtFor(lang string) string {
	if v.Last == nil {
		return ""
	}
	return formatLocalTime(v.Last.At, lang)
}

// autoBackupView は今の自動バックアップの設定と状態を返す。withCandidates なら同期フォルダの候補も探す。
func (s *Server) autoBackupView(withCandidates bool) autoBackupView {
	v := autoBackupView{Folder: s.getSetting(autoBackupFolderSetting, ""), Keep: backup.DefaultKeep}
	if raw := s.getSetting(autoBackupLastSetting, ""); raw != "" {
		var st autoBackupStatus
		if json.Unmarshal([]byte(raw), &st) == nil && !st.At.IsZero() {
			v.Last = &st
		}
	}
	if withCandidates {
		v.Candidates = backup.CloudFolders()
	}
	return v
}

// handleAutoBackupSave は自動バックアップの保存先フォルダを決め、すぐに最初のバックアップを作る。
// フォルダが無ければ作り、書き込めることを確かめてから保存する。
func (care *desktopCare) handleAutoBackupSave(c echo.Context) error {
	s := care.s
	folder := strings.TrimSpace(c.FormValue("folder"))
	if key := prepareBackupFolder(folder); key != "" {
		msg := localizedMessage(c, key)
		d := s.backupSettingsData()
		d.Auto.Error, d.Auto.Input = msg, folder
		return s.renderError(c, http.StatusUnprocessableEntity, "settings_backup", pageTitle(c, "settings_nav_backup"), msg, d)
	}
	if err := s.putSetting(autoBackupFolderSetting, filepath.Clean(folder)); err != nil {
		return err
	}
	extendDeadlines(c)
	care.backups.RunNow()
	return c.Redirect(http.StatusSeeOther, backupPagePath+"?flash=backup_auto_saved#auto-backup")
}

// prepareBackupFolder は folder を自動バックアップの保存先にできるか確かめる（無ければ作る）。
// できなければ画面に出す文言の辞書（internal/i18n）のキーを返す。
func prepareBackupFolder(folder string) string {
	if folder == "" {
		return "backup_folder_err_empty"
	}
	if !filepath.IsAbs(folder) {
		return "backup_folder_err_relative"
	}
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "backup_folder_err_create"
	}
	f, err := os.CreateTemp(folder, ".sorus-write-check-*.tmp")
	if err != nil {
		return "backup_folder_err_write"
	}
	f.Close()
	os.Remove(f.Name())
	return ""
}

// handleAutoBackupRun は「今すぐバックアップする」。作り終えるまで待ち、結果を設定画面に出す。
func (care *desktopCare) handleAutoBackupRun(c echo.Context) error {
	extendDeadlines(c)
	res, err := care.backups.RunNow()
	flash := "backup_auto_done"
	switch {
	case errors.Is(err, backup.ErrBusy):
		flash = "backup_auto_busy"
	case err != nil || res.Err != nil:
		flash = "backup_auto_failed"
	}
	return c.Redirect(http.StatusSeeOther, backupPagePath+"?flash="+flash+"#auto-backup")
}

// handleAutoBackupOff は自動バックアップをやめる（作ったバックアップはそのまま残す）。
func (s *Server) handleAutoBackupOff(c echo.Context) error {
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key IN (?, ?)`, autoBackupFolderSetting, autoBackupLastSetting); err != nil {
		return err
	}
	return c.Redirect(http.StatusSeeOther, backupPagePath+"?flash=backup_auto_off#auto-backup")
}

// backupStatusData は自動バックアップの状態の部分（backupStatusTemplate・backupStatusCompactTemplate）に渡すデータ。
type backupStatusData struct {
	View autoBackupView
	Lang string
}

// T は key の文言を表示言語で返す（テンプレートから {{.T "key"}} で呼ぶ）。
func (d backupStatusData) T(key string, args ...any) string {
	return i18n.Translate(d.Lang, key, args...)
}

// backupStatusTemplate はダッシュボードに出す自動バックアップの状態。
var backupStatusTemplate = template.Must(template.New("backup_status").Parse(`{{$v := .View}}<section id="backup-status" class="rounded-xl border {{if and $v.Last (not $v.Last.OK)}}border-rose-300 bg-rose-50{{else}}border-neutral-200 bg-white{{end}} p-4" aria-labelledby="backup-status-title">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div class="min-w-0">
      <h2 id="backup-status-title" class="text-sm font-semibold text-neutral-800">{{.T "backup_auto_title"}}</h2>
      {{- if not $v.Folder}}
      <p class="mt-1 text-sm text-neutral-700">{{.T "backup_status_no_folder"}}</p>
      {{- else if not $v.Last}}
      <p class="mt-1 text-sm text-neutral-700">{{.T "backup_status_none_yet"}}</p>
      {{- else if $v.Last.OK}}
      <p class="mt-1 text-sm text-neutral-700" data-backup-result="ok">{{.T "backup_status_last"}}: <span class="font-medium tabular-nums">{{$v.LastAtFor .Lang}}</span></p>
      {{- with $v.Last.WarningFor .Lang}}<p class="mt-1 text-xs text-neutral-600">{{.}}</p>{{end}}
      {{- else}}
      <p class="mt-1 text-sm font-medium text-rose-900" data-backup-result="failed">{{.T "backup_status_failed_at" ($v.LastAtFor .Lang)}}</p>
      <p class="mt-1 text-sm text-rose-900">{{$v.Last.MessageFor .Lang}}</p>
      {{- end}}
      {{- with $v.Folder}}<p class="mt-1 break-all text-xs text-neutral-500">{{$.T "backup_status_folder" .}}</p>{{end}}
    </div>
    <a href="/admin/settings/backup#auto-backup" class="shrink-0 rounded-lg border border-neutral-300 bg-white px-3 py-1.5 text-sm text-neutral-800 hover:bg-neutral-100">{{if $v.Folder}}{{.T "backup_status_settings"}}{{else}}{{.T "backup_status_choose"}}{{end}}</a>
  </div>
</section>`))

// backupStatusCompactTemplate はダッシュボード下段に1行で出す自動バックアップの状態（保存先が決まっていて、
// 最後のバックアップが失敗していないとき）。
var backupStatusCompactTemplate = template.Must(template.New("backup_status_compact").Parse(`{{$v := .View}}<section id="backup-status" data-backup-compact class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 rounded-xl border border-neutral-200 bg-white px-4 py-2.5" aria-labelledby="backup-status-title">
  <div class="flex min-w-0 flex-1 flex-wrap items-baseline gap-x-3 gap-y-1">
    <h2 id="backup-status-title" class="text-sm font-semibold text-neutral-800">{{.T "backup_auto_title"}}</h2>
    {{- if $v.Last}}
    <p class="text-xs text-neutral-500" data-backup-result="ok">{{.T "backup_status_last"}}: <span class="tabular-nums">{{$v.LastAtFor .Lang}}</span></p>
    {{- with $v.Last.WarningFor .Lang}}<p class="text-xs text-neutral-600">{{.}}</p>{{end}}
    {{- else}}
    <p class="text-xs text-neutral-500">{{.T "backup_status_none_yet"}}</p>
    {{- end}}
    <p class="min-w-0 break-all text-xs text-neutral-500">{{.T "backup_status_folder" $v.Folder}}</p>
  </div>
  <a href="/admin/settings/backup#auto-backup" class="btn btn-secondary btn-sm shrink-0">{{.T "backup_status_settings"}}</a>
</section>`))

// needsAttention はダッシュボードのファーストビュー上部に自動バックアップの案内・警告を出すか。保存先が未設定か、
// 最後のバックアップが失敗したときだけ true（ふだんは下段に1行で出す）。
func (v autoBackupView) needsAttention() bool {
	return v.Folder == "" || (v.Last != nil && !v.Last.OK)
}

// handleBackupStatus はダッシュボードに読み込む、自動バックアップの状態の部分を返す。管理者でなければ空を返す。
// ?compact=1 は下段の1行の状態。案内・警告が要るとき（needsAttention）は compact でも普通の大きさで出す。
func (s *Server) handleBackupStatus(c echo.Context) error {
	var buf bytes.Buffer
	if currentUser(c).HasRole(auth.RoleAdmin) {
		d := backupStatusData{View: s.autoBackupView(false), Lang: string(language(c))}
		var err error
		if c.QueryParam("compact") != "" && !d.View.needsAttention() {
			err = backupStatusCompactTemplate.Execute(&buf, d)
		} else {
			err = backupStatusTemplate.Execute(&buf, d)
		}
		if err != nil {
			return err
		}
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.HTMLBlob(http.StatusOK, buf.Bytes())
}
