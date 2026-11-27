package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/importer"
)

// WordPress の書き出しファイル（WXR）を管理画面から取り込む画面（「設定」→「WordPress から取り込む」）。
// Windows 版はコンソールを持たない GUI アプリのため、sorus import コマンドの代わりにこの画面を使う。
//
// 流れ: ファイルとオプションを選ぶ（GET importPagePath）→ 確認（POST importPreviewPath。DB を変えずに取り込み
// 予定の件数と警告を出し、ファイルを預かる）→ 取り込む（POST importRunPath。預かったファイルの合言葉 token、
// または「先に確認する」を外して送ったファイルを取り込む）→ 結果。
const (
	importPagePath    = "/admin/import"
	importPreviewPath = importPagePath + "/preview"
	importRunPath     = importPagePath + "/run"
	// importFileField は WXR ファイルを送るフィールド名。importTokenField は確認したファイルをそのまま取り込むときに
	// 送る、預かったファイルの合言葉のフィールド名。
	importFileField  = "wxr"
	importTokenField = "token"
	// maxImportSize は取り込める WXR ファイルの上限バイト数（100MB）。
	maxImportSize = 100 << 20
	// importStageTTL は確認したファイルを、「この内容で取り込む」が押されるまで預かっておく時間。
	importStageTTL = time.Hour
	// importMaxWarnings は結果の画面に並べる警告の上限。残りは件数だけを出す。
	importMaxWarnings = 200

	// importAccountCurrent・importAccountAuthors は著者の扱い（account_mode）。current はログイン中の人の記事に
	// まとめ、authors は WordPress の著者ごとに sorus のユーザーを作る（同じユーザー名の人がいればその人の記事にする）。
	importAccountCurrent = "current"
	importAccountAuthors = "authors"
)

func init() {
	for _, name := range []string{"settings_import", "settings_import_result"} {
		if !slices.Contains(pageNames, name) {
			pageNames = append(pageNames, name)
		}
	}
}

// importError は取り込みの画面に出すエラー。Key は i18n のキー、Status は応答の HTTP ステータス。
type importError struct {
	Status int
	Key    string
}

func (e *importError) Error() string { return "import: " + e.Key }

var (
	errImportMissingFile     = &importError{http.StatusBadRequest, "import_err_missing_file"}
	errImportSourceMismatch  = &importError{http.StatusBadRequest, "import_err_source_mismatch"}
	errImportTooLarge        = &importError{http.StatusRequestEntityTooLarge, "import_err_too_large"}
	errImportNotWXR          = &importError{http.StatusBadRequest, "import_err_not_wxr"}
	errImportInvalidXML      = &importError{http.StatusBadRequest, "import_err_invalid_xml"}
	errImportInvalidMarkdown = &importError{http.StatusBadRequest, "import_err_invalid_markdown"}
	errImportInvalidZip      = &importError{http.StatusBadRequest, "import_err_invalid_zip"}
	errImportZipTooLarge     = &importError{http.StatusRequestEntityTooLarge, "import_err_zip_too_large"}
	errImportZipNoMarkdown   = &importError{http.StatusBadRequest, "import_err_zip_no_markdown"}
	errImportZipUnsafePath   = &importError{http.StatusBadRequest, "import_err_zip_unsafe_path"}
	errImportNoPosts         = &importError{http.StatusBadRequest, "import_err_no_posts"}
	errImportExpired         = &importError{http.StatusBadRequest, "import_err_expired"}
	errImportBusy            = &importError{http.StatusConflict, "import_err_busy"}
	errImportFailed          = &importError{http.StatusInternalServerError, "import_err_failed"}
)

// importForm は取り込みのオプション。フォームのフィールド名は download_images・skip_pages・account_mode・dry_run。
type importForm struct {
	Source         string
	DownloadImages bool
	SkipPages      bool
	AccountMode    string
	DryRun         bool
}

// defaultImportForm は画面を開いたときのオプション（画像を取り込み、固定ページも取り込み、自分の記事にまとめ、先に確認する）。
func defaultImportForm() importForm {
	return importForm{Source: "wordpress", DownloadImages: true, AccountMode: importAccountCurrent, DryRun: true}
}

// readImportForm は送られたオプションを読む。チェックボックスは外すと送られないため、無ければ false。
func readImportForm(c echo.Context) importForm {
	f := importForm{
		Source:         c.FormValue("source"),
		DownloadImages: c.FormValue("download_images") != "",
		SkipPages:      c.FormValue("skip_pages") != "",
		AccountMode:    c.FormValue("account_mode"),
		DryRun:         c.FormValue("dry_run") != "",
	}
	if f.Source != "" && f.Source != "wordpress" && f.Source != "markdown" {
		f.Source = "wordpress"
	}
	if f.AccountMode != importAccountAuthors {
		f.AccountMode = importAccountCurrent
	}
	return f
}

// importState は WordPress の取り込みの状態。run は取り込み（確認を含む）を同時に1つだけにし、staged は確認した
// ファイルを合言葉ごとに預かる。
type importState struct {
	run    sync.Mutex
	mu     sync.Mutex
	staged map[string]*importStage
}

// importStage は確認したあと「この内容で取り込む」を待っているファイル。確認した人（UserID）だけが取り込める。
type importStage struct {
	Path, Filename, UserID string
	Form                   importForm
	Expires                time.Time
}

// put は stage を預かり、その合言葉を返す。期限の切れたファイルと、同じ人が前に確認したファイルはここで消す
// （確認を何度やり直しても、1人につき最新の1つだけを預かる）。
func (st *importState) put(stage *importStage) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("import: token: %w", err)
	}
	token := hex.EncodeToString(b)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dropExpiredLocked(time.Now())
	for old, s := range st.staged {
		if s.UserID == stage.UserID {
			os.Remove(s.Path)
			delete(st.staged, old)
		}
	}
	if st.staged == nil {
		st.staged = map[string]*importStage{}
	}
	st.staged[token] = stage
	return token, nil
}

// get は userID が預けた、期限内の token のファイルを返す。
func (st *importState) get(token, userID string) (*importStage, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dropExpiredLocked(time.Now())
	stage, ok := st.staged[token]
	if !ok || stage.UserID != userID {
		return nil, false
	}
	return stage, true
}

// remove は token のファイルを消す。
func (st *importState) remove(token string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if stage, ok := st.staged[token]; ok {
		os.Remove(stage.Path)
		delete(st.staged, token)
	}
}

// clear は預かっているファイルをすべて消す（Shutdown で呼ぶ）。
func (st *importState) clear() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for token, stage := range st.staged {
		os.Remove(stage.Path)
		delete(st.staged, token)
	}
}

func (st *importState) dropExpiredLocked(now time.Time) {
	for token, stage := range st.staged {
		if !now.Before(stage.Expires) {
			os.Remove(stage.Path)
			delete(st.staged, token)
		}
	}
}

// registerImportRoutes は取り込みの画面・確認・実行を、ログイン済み・CSRF 保護済みの /admin グループに管理者専用として登録する。
func (s *Server) registerImportRoutes(authed *echo.Group, adminOnly echo.MiddlewareFunc) {
	authed.GET("/import", s.handleImportPage, adminOnly)
	authed.GET("/settings/migration", s.handleImportPage, adminOnly)
	authed.POST("/import/preview", s.handleImportPreview, adminOnly)
	authed.POST("/import/run", s.handleImportRun, adminOnly)
}

// isImportUpload は WXR ファイルを受け取る要求（確認・取り込みの POST）かを返す。
func isImportUpload(c echo.Context) bool {
	r := c.Request()
	return r.Method == http.MethodPost && (r.URL.Path == importPreviewPath || r.URL.Path == importRunPath)
}

// requireImportAdmin は、取り込みの要求の本文（最大 maxImportSize）を CSRF ミドルウェアが読む前に、管理者として
// ログインしていることを確かめ、大きなファイルの送受信に間に合うよう読み書きのタイムアウトを延ばす。
// ログインしていない人や管理者でない人に、大きな本文を受け取らせないためのもので、権限の判定はこのあとルートの
// 認証・権限ミドルウェアでも通常どおり行う。
func (s *Server) requireImportAdmin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !isImportUpload(c) {
			return next(c)
		}
		cookie, err := c.Cookie(s.cookies.Session)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "管理者としてログインしてから、もう一度やり直してください")
		}
		u, err := auth.ValidateSession(s.db, cookie.Value)
		if err != nil || !u.HasRole(auth.RoleAdmin) {
			return echo.NewHTTPError(http.StatusForbidden, "管理者としてログインしてから、もう一度やり直してください")
		}
		extendDeadlines(c)
		return next(c)
	}
}

// importPageData は settings_import.html に渡すデータ。
type importPageData struct {
	Form     importForm
	MaxSize  string
	MaxBytes int64
	Error    string
}

// importResultData は settings_import_result.html に渡すデータ。
type importResultData struct {
	// Preview は確認（ドライラン）の結果か。Token はそのとき「この内容で取り込む」が送る合言葉。
	Preview  bool
	Token    string
	Filename string
	SiteURL  string
	Form     importForm
	Summary  *importer.Summary
	// Warnings は画面に並べる警告（最大 importMaxWarnings 件）。MoreWarnings は並べきれなかった件数。
	Warnings     []string
	MoreWarnings int
	// IndexFailed は、取り込みは終わったが検索への登録に失敗した記事があるか。
	IndexFailed bool
	Duration    string
}

func (s *Server) importPageData(form importForm) importPageData {
	return importPageData{Form: form, MaxSize: formatByteSize(maxImportSize), MaxBytes: maxImportSize}
}

func importTitle(c echo.Context) string { return i18n.Translate(string(language(c)), "import_title") }

func (s *Server) handleImportPage(c echo.Context) error {
	return s.render(c, http.StatusOK, "settings_import", importTitle(c), s.importPageData(defaultImportForm()))
}

// renderImportError は err（importError）を、オプションを選び直せるよう入力画面に出す。それ以外のエラーはそのまま返す。
func (s *Server) renderImportError(c echo.Context, form importForm, err error) error {
	var ie *importError
	if !errors.As(err, &ie) {
		return err
	}
	d := s.importPageData(form)
	d.Error = i18n.Translate(string(language(c)), ie.Key)
	return s.render(c, ie.Status, "settings_import", importTitle(c), d)
}

// handleImportPreview は送られた WXR を DB を変えずに取り込んでみて（ドライラン）、取り込み予定の件数と警告を出す。
// ファイルは「この内容で取り込む」のために importStageTTL のあいだ預かる。
func (s *Server) handleImportPreview(c echo.Context) error {
	form := readImportForm(c)
	form.DryRun = true
	path, filename, err := receiveImportUpload(c)
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	staged := false
	defer func() {
		if !staged {
			os.Remove(path)
		}
	}()
	form.Source = importSourceForFile(form.Source, filename)
	form = normalizeImportFormForSource(form)
	if err := validateImportSource(form.Source, filename); err != nil {
		return s.renderImportError(c, form, err)
	}

	release, err := s.lockImport()
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	defer release()
	exp, err := parseImportFile(path, filename, s.timeLocation())
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	sum, indexFailed, err := s.runImport(c, exp, form, true)
	if err != nil {
		return s.renderImportError(c, form, err)
	}

	u := currentUser(c)
	token, err := s.imports.put(&importStage{Path: path, Filename: filename, UserID: u.ID, Form: form, Expires: time.Now().Add(importStageTTL)})
	if err != nil {
		return err
	}
	staged = true
	d := s.importResult(exp, sum, form, filename, indexFailed)
	d.Preview, d.Token = true, token
	return s.render(c, http.StatusOK, "settings_import_result", importTitle(c), d)
}

// handleImportRun は WXR を取り込む。確認の画面からは預かったファイルの合言葉（token）を、入力画面で「先に確認する」を
// 外したときはファイルそのものを受け取る。
func (s *Server) handleImportRun(c echo.Context) error {
	if token := c.FormValue(importTokenField); token != "" {
		return s.runStagedImport(c, token)
	}
	form := readImportForm(c)
	form.DryRun = false
	path, filename, err := receiveImportUpload(c)
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	defer os.Remove(path)
	form.Source = importSourceForFile(form.Source, filename)
	form = normalizeImportFormForSource(form)
	if err := validateImportSource(form.Source, filename); err != nil {
		return s.renderImportError(c, form, err)
	}
	release, err := s.lockImport()
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	defer release()
	exp, err := parseImportFile(path, filename, s.timeLocation())
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	sum, indexFailed, err := s.runImport(c, exp, form, false)
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	return s.render(c, http.StatusOK, "settings_import_result", importTitle(c), s.importResult(exp, sum, form, filename, indexFailed))
}

// validateImportSource rejects a selected source that does not match the uploaded file.
// An omitted source is treated as WordPress for compatibility with existing callers.
func validateImportSource(source, filename string) error {
	ext := strings.ToLower(filepath.Ext(filename))
	if source == "markdown" && (ext == ".md" || ext == ".zip") {
		return nil
	}
	if source == "wordpress" && ext == ".xml" {
		return nil
	}
	if source == "markdown" || source == "wordpress" {
		return errImportSourceMismatch
	}
	return errImportSourceMismatch
}

func importSourceForFile(source, filename string) string {
	if source != "" {
		return source
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".md", ".zip":
		return "markdown"
	default:
		return "wordpress"
	}
}

func normalizeImportFormForSource(form importForm) importForm {
	if form.Source == "markdown" {
		form.SkipPages = false
		form.AccountMode = importAccountCurrent
	}
	return form
}

// runStagedImport は確認したときに預かったファイルを、確認したときのオプションで取り込む。取り込めたら預かったファイルを消す。
// 失敗したときは、もう一度押せるよう預かったままにする。
func (s *Server) runStagedImport(c echo.Context, token string) error {
	stage, ok := s.imports.get(token, currentUser(c).ID)
	if !ok {
		return s.renderImportError(c, defaultImportForm(), errImportExpired)
	}
	form := stage.Form
	form.DryRun = false
	release, err := s.lockImport()
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	defer release()
	exp, err := parseImportFile(stage.Path, stage.Filename, s.timeLocation())
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	sum, indexFailed, err := s.runImport(c, exp, form, false)
	if err != nil {
		return s.renderImportError(c, form, err)
	}
	s.imports.remove(token)
	return s.render(c, http.StatusOK, "settings_import_result", importTitle(c), s.importResult(exp, sum, form, stage.Filename, indexFailed))
}

// lockImport は取り込みを始めてよいかを確かめ、終わったら呼ぶ解放の関数を返す。取り込みは同時に1つだけにし、
// バックアップからの復元とも重ならないようにする（作成中のバックアップとは重なってよい）。
func (s *Server) lockImport() (func(), error) {
	if !s.imports.run.TryLock() {
		return nil, errImportBusy
	}
	if !s.backupMu.TryRLock() {
		s.imports.run.Unlock()
		return nil, errImportBusy
	}
	return func() {
		s.backupMu.RUnlock()
		s.imports.run.Unlock()
	}, nil
}

// receiveImportUpload は送られた WXR ファイルを一時ファイルに書き出し、そのパスと元のファイル名を返す。
func receiveImportUpload(c echo.Context) (path, filename string, err error) {
	fh, err := c.FormFile(importFileField)
	if err != nil || fh.Filename == "" {
		return "", "", errImportMissingFile
	}
	if fh.Size > maxImportSize {
		return "", "", errImportTooLarge
	}
	src, err := fh.Open()
	if err != nil {
		return "", "", fmt.Errorf("import: open upload: %w", err)
	}
	defer src.Close()
	dst, err := os.CreateTemp("", "sorus-import-*")
	if err != nil {
		return "", "", fmt.Errorf("import: create temp file: %w", err)
	}
	n, err := io.Copy(dst, io.LimitReader(src, maxImportSize+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		os.Remove(dst.Name())
		return "", "", fmt.Errorf("import: save upload: %w", err)
	case n > maxImportSize:
		os.Remove(dst.Name())
		return "", "", errImportTooLarge
	case n == 0:
		os.Remove(dst.Name())
		return "", "", errImportMissingFile
	}
	return dst.Name(), filepath.Base(filepath.Clean("/" + fh.Filename)), nil
}

// parseImportFile は path の WXR を解析する。WXR でない・XML として読めない・記事が1件も無いときは画面に出すエラーを返す。
func parseImportFile(path, filename string, loc *time.Location) (*importer.Export, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("import: open %s: %w", path, err)
	}
	defer f.Close()
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".md":
		exp, err := importer.ParseMarkdown(f, filename)
		switch {
		case errors.Is(err, importer.ErrZipTooLarge):
			return nil, errImportTooLarge
		case err != nil:
			return nil, errImportInvalidMarkdown
		}
		return exp, nil
	case ".zip":
		st, err := f.Stat()
		if err != nil {
			return nil, errImportInvalidZip
		}
		exp, err := importer.ParseMarkdownZip(f, st.Size())
		switch {
		case errors.Is(err, importer.ErrZipTooLarge):
			return nil, errImportZipTooLarge
		case errors.Is(err, importer.ErrNoMarkdownFiles):
			return nil, errImportZipNoMarkdown
		case errors.Is(err, importer.ErrUnsafeZipPath):
			return nil, errImportZipUnsafePath
		case errors.Is(err, importer.ErrInvalidMarkdown):
			return nil, errImportInvalidMarkdown
		case err != nil:
			return nil, errImportInvalidZip
		}
		return exp, nil
	case ".xml":
	default:
		return nil, errImportNotWXR
	}
	exp, err := importer.Parse(f, importer.ParseOptions{Location: loc})
	switch {
	case errors.Is(err, importer.ErrNotWXR):
		return nil, errImportNotWXR
	case err != nil:
		return nil, errImportInvalidXML
	case len(exp.Posts) == 0:
		return nil, errImportNoPosts
	}
	return exp, nil
}

// runImport は exp を form のオプションで取り込む（dryRun なら DB を変えずに件数だけを数える）。
// 取り込みは終わったが検索への登録に失敗した記事があるときは、indexFailed を true にして結果を返す。
func (s *Server) runImport(c echo.Context, exp *importer.Export, form importForm, dryRun bool) (sum *importer.Summary, indexFailed bool, err error) {
	im := importer.New(s.db, s.search, s.importOptions(currentUser(c), form, dryRun))
	sum, err = im.ImportExport(c.Request().Context(), exp)
	if !dryRun && sum != nil {
		// 旧 URL からの転送を登録したため、転送のキャッシュを読み直させる。記事が増えたため関連記事も作り直させる。
		s.redirects.reset()
		s.related.invalidate()
	}
	if err != nil {
		if sum != nil {
			c.Logger().Warnf("import: %v", err)
			return sum, true, nil
		}
		c.Logger().Errorf("import: %v", err)
		return nil, false, errImportFailed
	}
	if !dryRun {
		c.Logger().Infof("import: %d articles imported from WordPress by %s", sum.Articles, currentUser(c).Username)
	}
	return sum, false, nil
}

// importOptions は form のオプションを importer.Options にする。「自分の記事にまとめる」ならすべての記事を u の記事にし、
// そうでなければ WordPress の著者ごとにユーザーを作る（著者の無い記事は u の記事にする）。
func (s *Server) importOptions(u *auth.User, form importForm, dryRun bool) importer.Options {
	opts := importer.Options{
		DownloadImages: form.DownloadImages,
		DryRun:         dryRun,
		SkipPages:      form.SkipPages,
		UploadDir:      s.cfg.UploadDir,
		MaxImageSize:   s.cfg.MaxUploadSize,
		Location:       s.timeLocation(),
		// 取り込んだ固定ページの /<slug> が、管理画面や同期 API など決まった URL を隠さないようにする。
		ReservedPageSlug: s.articles.IsReservedPageSlug,
	}
	if form.AccountMode != importAccountAuthors {
		opts.Account = u.ID
	} else {
		opts.DefaultAuthor = u.Username
	}
	return opts
}

func (s *Server) importResult(exp *importer.Export, sum *importer.Summary, form importForm, filename string, indexFailed bool) importResultData {
	d := importResultData{
		Filename:    filename,
		SiteURL:     exp.SiteURL,
		Form:        form,
		Summary:     sum,
		Warnings:    sum.Warnings,
		IndexFailed: indexFailed,
		Duration:    sum.Duration.Round(time.Millisecond).String(),
	}
	if len(d.Warnings) > importMaxWarnings {
		d.MoreWarnings = len(d.Warnings) - importMaxWarnings
		d.Warnings = d.Warnings[:importMaxWarnings]
	}
	return d
}
