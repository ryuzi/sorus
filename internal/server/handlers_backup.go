package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/backup"
)

const (
	// DefaultMaxRestoreSize は復元のためにアップロードできるバックアップファイルの既定の上限バイト数（4GB）。
	DefaultMaxRestoreSize = 4 << 30

	// backupPagePath・backupDownloadPath・backupRestorePath は完全バックアップの画面・作成・復元のパス。
	backupPagePath     = "/admin/settings/backup"
	backupDownloadPath = backupPagePath + "/download"
	backupRestorePath  = backupPagePath + "/restore"
	// backupFormField は復元フォームでバックアップファイルを送るフィールド名。
	backupFormField = "backup"
	// backupTransferTimeout は大きなバックアップの作成・ダウンロード・アップロードに許す時間。
	// サーバー全体の読み書きのタイムアウト（数十秒）を、管理者の認証を通った要求に限って延ばす。
	backupTransferTimeout = 2 * time.Hour
)

var (
	errBackupBusy = echo.NewHTTPError(http.StatusConflict,
		"別のバックアップの作成または復元を実行中です。終わるまで待ってからやり直してください")
	errBackupMissingFile = echo.NewHTTPError(http.StatusBadRequest,
		"復元するバックアップファイル（.zip）を選択してください")
)

func init() {
	if !slices.Contains(pageNames, "settings_backup") {
		pageNames = append(pageNames, "settings_backup")
	}
}

// registerBackupRoutes は完全バックアップの画面・作成・復元を、ログイン済み・CSRF 保護済みの /admin グループに
// 管理者専用として登録する。
func (s *Server) registerBackupRoutes(authed *echo.Group, adminOnly echo.MiddlewareFunc) {
	authed.GET("/settings/backup", s.handleBackupSettings, adminOnly)
	authed.POST("/settings/backup/download", s.handleBackupDownload, adminOnly)
	authed.POST("/settings/backup/restore", s.handleBackupRestore, adminOnly)
}

// isBackupRestore は復元（POST /admin/settings/backup/restore と、デスクトップモードの「はじめに」画面の
// 「バックアップから始める」の POST /admin/setup/restore）への要求かを返す。
func isBackupRestore(c echo.Context) bool {
	r := c.Request()
	return r.Method == http.MethodPost && (r.URL.Path == backupRestorePath || r.URL.Path == setupRestorePath)
}

// isBackupTransferPath は Zip を送受信する（Gzip で圧縮し直さない）パスかを返す。
func isBackupTransferPath(p string) bool {
	return strings.HasPrefix(p, backupPagePath+"/")
}

// requireRestoreCSRFHeader は、復元の要求が CSRF トークンを X-CSRF-Token ヘッダーで送り、それが CSRF Cookie と
// 一致することを、本文を読む前に確かめる。
//
// CSRF ミドルウェアはヘッダーにトークンが無ければフォームの本文を解析して探すため、そのままでは未ログインの
// 要求でも数 GB の本文を一時ファイルへ受け取ってしまう。ヘッダーでの照合を必須にして、本文はログインと
// 管理者権限を確かめたあとのハンドラーだけが読むようにする。CSRF ミドルウェア自体の検証もこのあと通常どおり行う。
func (s *Server) requireRestoreCSRFHeader(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !isBackupRestore(c) {
			return next(c)
		}
		header := c.Request().Header.Get(CSRFHeaderName)
		cookie, err := c.Cookie(s.cookies.CSRF)
		if header == "" || err != nil || subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
			return echo.NewHTTPError(http.StatusForbidden, "invalid or missing csrf token")
		}
		return next(c)
	}
}

// extendDeadlines はこの要求の読み書きのタイムアウトを backupTransferTimeout 後まで延ばす。
func extendDeadlines(c echo.Context) {
	rc := http.NewResponseController(c.Response())
	deadline := time.Now().Add(backupTransferTimeout)
	if err := rc.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.Logger().Warnf("backup: extend read deadline: %v", err)
	}
	if err := rc.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.Logger().Warnf("backup: extend write deadline: %v", err)
	}
}

// backupSettingsData は settings_backup.html に渡すデータ。
type backupSettingsData struct {
	// MaxRestoreSize は復元でアップロードできるファイルの上限（表示用）。
	MaxRestoreSize  string
	MaxRestoreBytes int64
	// Auto はデスクトップモードの自動バックアップの設定と状態（handlers_backup_auto.go）。サーバーモードでは nil。
	Auto *autoBackupView
}

func (s *Server) backupSettingsData() backupSettingsData {
	d := backupSettingsData{MaxRestoreSize: formatByteSize(s.cfg.MaxRestoreSize), MaxRestoreBytes: s.cfg.MaxRestoreSize}
	if s.cfg.Desktop {
		v := s.autoBackupView(true)
		d.Auto = &v
	}
	return d
}

func (s *Server) handleBackupSettings(c echo.Context) error {
	return s.render(c, http.StatusOK, "settings_backup", pageTitle(c, "settings_nav_backup"), s.backupSettingsData())
}

// handleBackupDownload は DB とアップロード画像・添付資料の完全バックアップ（Zip）を作成し、そのままダウンロードさせる。
// 作成中にエラーになっても壊れた Zip を送らないよう、一時ファイルに作り終えてから送る。
func (s *Server) handleBackupDownload(c echo.Context) error {
	if !s.backupMu.TryRLock() {
		return s.renderError(c, http.StatusConflict, "settings_backup", pageTitle(c, "settings_nav_backup"), errBackupBusy.Message.(string), s.backupSettingsData())
	}
	defer s.backupMu.RUnlock()
	extendDeadlines(c)

	dir, err := os.MkdirTemp("", "sorus-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	name := backup.FileName(time.Now())
	zipPath := filepath.Join(dir, name)
	if err := backup.CreateWith(s.db, zipPath, backup.CreateOptions{UploadDir: s.cfg.UploadDir, AttachmentDir: s.cfg.AttachmentDir}); err != nil {
		c.Logger().Errorf("backup: create: %v", err)
		return s.renderError(c, http.StatusInternalServerError, "settings_backup", pageTitle(c, "settings_nav_backup"),
			"バックアップを作成できませんでした。サーバーの空き容量とログを確認してください", s.backupSettingsData())
	}
	f, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, "application/zip")
	h.Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", name))
	// バックアップにはパスワードハッシュやセッションが含まれるため、ブラウザや中継サーバーに残させない。
	h.Set("Cache-Control", "no-store")
	http.ServeContent(c.Response(), c.Request(), name, info.ModTime(), f)
	return nil
}

// backupRestoreResponse は復元 API の JSON レスポンス。
type backupRestoreResponse struct {
	Message string `json:"message"`
	// Redirect は復元後に開くページ。復元した DB に今のログインセッションが無ければログイン画面になる。
	Redirect string `json:"redirect"`
}

// handleBackupRestore はアップロードされた完全バックアップを検査し、稼働中の DB とアップロード画像・添付資料に復元する。
// 添付資料を含まない以前のバックアップでは、添付資料はそのまま残す。
// フォームは fetch（XMLHttpRequest）で X-CSRF-Token ヘッダーを付けて multipart/form-data で送る。
func (s *Server) handleBackupRestore(c echo.Context) error {
	if !s.backupMu.TryLock() {
		return errBackupBusy
	}
	defer s.backupMu.Unlock()
	extendDeadlines(c)

	zipPath, err := s.receiveBackupUpload(c)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)

	if err := s.restoreUploadedBackup(c, zipPath); err != nil {
		return err
	}
	c.Logger().Infof("backup: restored from upload by %s", currentUser(c).Username)

	res := backupRestoreResponse{
		Message:  "バックアップから復元しました",
		Redirect: backupPagePath + "?flash=backup_restored",
	}
	if cookie, err := c.Cookie(s.cookies.Session); err != nil || !s.sessionValid(cookie.Value) {
		res.Message = "バックアップから復元しました。復元したデータのアカウントでもう一度ログインしてください"
		res.Redirect = auth.DefaultLoginPath + "?flash=backup_relogin"
	}
	return c.JSON(http.StatusOK, res)
}

// restoreUploadedBackup は受け取ったバックアップ zipPath を稼働中の DB・画像・添付資料に復元し、サーバーが
// メモリに持つ設定や検索インデックスを読み直す。失敗したら画面に出す文言の HTTP エラーを返す。
// 呼び出し側は backupMu を書き込みロックで持っていること。
func (s *Server) restoreUploadedBackup(c echo.Context, zipPath string) error {
	err := backup.RestoreLiveWith(s.db, zipPath, backup.RestoreTargets{UploadDir: s.cfg.UploadDir, AttachmentDir: s.cfg.AttachmentDir})
	if err != nil {
		switch {
		case errors.Is(err, backup.ErrUnsafePath):
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				"このファイルには保存先の外へ書き込もうとする項目が含まれているため、復元できません。現在のデータは変更していません")
		case errors.Is(err, backup.ErrIncompatible):
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				"このバックアップは新しいバージョンの sorus で作成されています。sorus を更新してから復元してください。現在のデータは変更していません")
		case errors.Is(err, backup.ErrInvalidArchive):
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				"sorus のバックアップファイルとして読み取れませんでした。ファイルが壊れていないか、正しいファイルを選んだか確認してください。現在のデータは変更していません")
		}
		c.Logger().Errorf("backup: restore: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "復元に失敗しました。サーバーのログを確認してください")
	}
	if err := s.reloadAfterRestore(c.Request().Context()); err != nil {
		c.Logger().Errorf("backup: reload after restore: %v", err)
	}
	return nil
}

// receiveBackupUpload は multipart の backup フィールドを一時ファイルに保存し、そのパスを返す。
// 数 GB になりうるため、フォーム全体をメモリや一時ファイルへ解析させず、パートを順に読んで直接書き出す。
func (s *Server) receiveBackupUpload(c echo.Context) (string, error) {
	mr, err := c.Request().MultipartReader()
	if err != nil {
		return "", errBackupMissingFile
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return "", errBackupMissingFile
		}
		if err != nil {
			return "", uploadReadError(err)
		}
		if part.FormName() != backupFormField || part.FileName() == "" {
			part.Close()
			continue
		}
		f, err := os.CreateTemp("", "sorus-restore-*.zip")
		if err != nil {
			return "", err
		}
		n, err := io.Copy(f, io.LimitReader(part, s.cfg.MaxRestoreSize+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		part.Close()
		switch {
		case err != nil:
			os.Remove(f.Name())
			return "", uploadReadError(err)
		case n > s.cfg.MaxRestoreSize:
			os.Remove(f.Name())
			return "", restoreTooLarge(s.cfg.MaxRestoreSize)
		case n == 0:
			os.Remove(f.Name())
			return "", errBackupMissingFile
		}
		return f.Name(), nil
	}
}

func restoreTooLarge(limit int64) error {
	return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
		fmt.Sprintf("バックアップファイルが大きすぎます（上限 %s）", formatByteSize(limit)))
}

// uploadReadError はアップロードの読み取りエラーを応答に変える。本文の上限を超えた場合は BodyLimit の 413 を返す。
func uploadReadError(err error) error {
	var he *echo.HTTPError
	if errors.As(err, &he) {
		return he
	}
	return echo.NewHTTPError(http.StatusBadRequest, "バックアップファイルを受け取れませんでした。もう一度やり直してください")
}

// sessionValid は id のログインセッションが（復元後の）DB で有効かを返す。
func (s *Server) sessionValid(id string) bool {
	_, err := auth.ValidateSession(s.db, id)
	return err == nil
}

// reloadAfterRestore は、復元で DB が入れ替わったあとに、サーバーがメモリに持つ設定・転送ルール・関連記事を捨てて
// 読み直し、検索インデックスを復元した記事に揃える。
func (s *Server) reloadAfterRestore(ctx context.Context) error {
	applySavedSiteSettings(&s.cfg)
	// 閲覧制限（Basic 認証）の設定も復元したデータに揃える。読めなければ今の設定のまま守り続ける。デスクトップモードでは
	// 読み直しても働かない（basicAuthApplies）。
	var err error
	if berr := s.reloadBasicAuth(); berr != nil {
		err = fmt.Errorf("reload basic auth settings: %w", berr)
	}
	// メンテナンス表示の設定も復元したデータに揃える。読めなければ今の状態のまま。
	if merr := s.reloadMaintenance(); merr != nil {
		err = errors.Join(err, fmt.Errorf("reload maintenance setting: %w", merr))
	}
	s.redirects.reset()
	s.related.invalidate()
	if s.search == nil {
		return err
	}
	return errors.Join(err, s.search.SyncFromDB(ctx, s.db))
}

// formatByteSize は n バイトを 4 GB・512 MB の形で返す。
func formatByteSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	}
	return formatFileSize(n)
}
