package server

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"golang.org/x/text/unicode/norm"

	"sorus/internal/article"
	"sorus/internal/lock"
)

const (
	// AttachmentURLPrefix は添付資料を公開サイトで配信する URL パス（/attachments/:id/download）。
	AttachmentURLPrefix = "/attachments/"
	// maxAttachmentsPerArticle は1記事に添付できる資料の数。
	maxAttachmentsPerArticle = 20
	// maxFilenameRunes は表示用のファイル名の最大文字数（拡張子を含む）。
	maxFilenameRunes = 120
	// sniffBytes は形式の判定に読む先頭のバイト数。
	sniffBytes = 512
)

var (
	errAttachmentTooLarge   = errors.New("attachment: file too large")
	errAttachmentType       = errors.New("attachment: unsupported file type")
	errAttachmentExecutable = errors.New("attachment: executable or script")
	errAttachmentMismatch   = errors.New("attachment: content does not match extension")
	errAttachmentMacro      = errors.New("attachment: office file with macros")
	errTooManyAttachments   = errors.New("attachment: too many attachments")
)

// attachmentType は添付を許可する形式。check はファイルの中身（先頭 head と全体 r）が拡張子どおりかを確かめる。
type attachmentType struct {
	MIME  string
	Label string
	check func(head []byte, r io.ReaderAt, size int64) error
}

// attachmentTypes は許可する拡張子（小文字）。これ以外の拡張子は中身を問わず拒否する。
var attachmentTypes = map[string]attachmentType{
	".pdf":  {"application/pdf", "PDF", checkPDF},
	".zip":  {"application/zip", "ZIP", checkZip},
	".txt":  {"text/plain", "TXT", checkText},
	".csv":  {"text/csv", "CSV", checkText},
	".docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Word", checkOOXML("word/")},
	".xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "Excel", checkOOXML("xl/")},
	".pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation", "PowerPoint", checkOOXML("ppt/")},
}

// executableMagics は実行ファイル・スクリプトの先頭のバイト列。拡張子を偽装した実行ファイルを拒否する。
var executableMagics = [][]byte{
	[]byte("MZ"),             // Windows PE (.exe, .dll)
	[]byte("\x7fELF"),        // Linux ELF
	[]byte("#!"),             // シェバン付きスクリプト (.sh, .py など)
	{0xfe, 0xed, 0xfa, 0xce}, // Mach-O 32bit
	{0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64bit
	{0xce, 0xfa, 0xed, 0xfe}, // Mach-O 32bit (LE)
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64bit (LE)
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O Universal / Java class
	[]byte("\x00asm"),        // WebAssembly
	[]byte("\xef\xbb\xbf#!"), // BOM 付きシェバン
}

// attachmentView は添付資料の1件。FilePath は AttachmentDir 内の保存ファイル名。
type attachmentView struct {
	ID            string    `db:"id"`
	ArticleID     string    `db:"article_id"`
	Filename      string    `db:"filename"`
	FilePath      string    `db:"file_path"`
	FileSize      int64     `db:"file_size"`
	MimeType      string    `db:"mime_type"`
	DownloadCount int64     `db:"download_count"`
	CreatedAt     time.Time `db:"created_at"`
}

// URL は公開サイトのダウンロード URL。
func (a attachmentView) URL() string { return AttachmentURLPrefix + a.ID + "/download" }

// AdminURL は管理画面のダウンロード URL（公開前でも取得でき、ダウンロード数を数えない）。
func (a attachmentView) AdminURL() string { return "/admin/attachments/" + a.ID + "/download" }

// Label は形式の表示名（PDF・Word など）。
func (a attachmentView) Label() string {
	if t, ok := attachmentTypes[strings.ToLower(path.Ext(a.FilePath))]; ok {
		return t.Label
	}
	return "ファイル"
}

// IsPDF は論文 PDF か。構造化データ・Google Scholar 向けメタデータの citation_pdf_url に使う。
func (a attachmentView) IsPDF() bool { return a.MimeType == "application/pdf" }

// attachmentListData は partials/attachment_list.html に渡すデータ。
type attachmentListData struct {
	ArticleID string
	Items     []attachmentView
	// CanEdit は資料を追加・削除できるか。
	CanEdit   bool
	CSRFToken string
	// MaxMB はアップロードの上限（MB）、Accept は file input の accept 属性。
	MaxMB  int64
	Accept string
	// Lang は管理画面の表示言語。
	Lang string
}

// attachmentAccept は file input の accept 属性に使う、許可する拡張子の一覧。
const attachmentAccept = ".pdf,.zip,.txt,.csv,.docx,.xlsx,.pptx"

// attachmentColumns は attachmentView を読む SELECT 句。
const attachmentColumns = `id, article_id, filename, file_path, file_size, mime_type, download_count, created_at`

// handleAttachmentUpload は multipart の file フィールドの資料を検査して保存する。
//
// 許可した拡張子で、かつ中身がその形式（マジックナンバー・ZIP 構造）と一致するものだけを受け付け、
// 実行ファイル・スクリプト・マクロ付き Office ファイルは拒否する。保存先は静的配信しないディレクトリで、
// ファイル名は乱数にして実行権限を付けない。他のユーザーが編集中なら 409、公開中・予約中の記事に
// 執筆者が添付しようとした場合は 403 を返す。
func (s *Server) handleAttachmentUpload(c echo.Context) error {
	id := c.Param("id")
	if err := s.checkAttachmentEdit(c, id); err != nil {
		return err
	}
	fh, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "添付するファイルを選択してください")
	}
	var n int
	if err := s.db.Get(&n, `SELECT COUNT(*) FROM attachments WHERE article_id = ?`, id); err != nil {
		return err
	}
	if n >= maxAttachmentsPerArticle {
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("1つの記事に添付できる資料は%d件までです", maxAttachmentsPerArticle))
	}

	att, err := s.saveAttachment(fh, id, currentUser(c).ID)
	if err != nil {
		return attachmentError(err, s.cfg.MaxAttachmentSize)
	}
	switch {
	case c.Request().Header.Get("HX-Request") == "true":
		data, err := s.attachmentList(c, id)
		if err != nil {
			return err
		}
		data.CanEdit = true
		return c.Render(http.StatusCreated, "attachment_list", data)
	case isAsync(c):
		return c.JSON(http.StatusCreated, map[string]any{
			"id": att.ID, "filename": att.Filename, "file_size": att.FileSize, "mime_type": att.MimeType,
			"url": att.URL(), "markdown": fmt.Sprintf("[%s](%s)", markdownLinkText(att.Filename), att.URL()),
		})
	}
	return c.Redirect(http.StatusSeeOther, "/admin/articles/"+id+"/edit#attachments")
}

// handleAttachmentDelete は添付資料を削除する。HTMX には更新後の一覧のパーシャルを返す。
func (s *Server) handleAttachmentDelete(c echo.Context) error {
	id := c.Param("id")
	if err := s.checkAttachmentEdit(c, id); err != nil {
		return err
	}
	var file string
	err := s.db.Get(&file, `DELETE FROM attachments WHERE id = ? AND article_id = ? RETURNING file_path`, c.Param("aid"), id)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if p, ok := s.attachmentPath(file); ok {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.Logger().Warnf("attachment: remove %s: %v", p, err)
		}
	}
	if c.Request().Header.Get("HX-Request") == "true" {
		data, err := s.attachmentList(c, id)
		if err != nil {
			return err
		}
		data.CanEdit = true
		return c.Render(http.StatusOK, "attachment_list", data)
	}
	return c.NoContent(http.StatusNoContent)
}

// checkAttachmentEdit は記事 id の添付資料を現在のユーザーが変更できるかを確かめる。
func (s *Server) checkAttachmentEdit(c echo.Context, id string) error {
	user := currentUser(c)
	info, err := s.locker.Check(c.Request().Context(), id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if info.IsLocked && !info.IsOwnLock {
		return echo.NewHTTPError(http.StatusConflict, info.UserName+"さんが編集中のため添付資料を変更できません")
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if requiresPublisher(a.Status) && !canPublish(user) {
		return echo.NewHTTPError(http.StatusForbidden, "公開中・予約中の記事の添付資料を変更できるのは編集長・管理者だけです")
	}
	return nil
}

// handleAttachmentDownload は公開中の記事の添付資料をダウンロードさせ、ダウンロード数を数える。
// 非公開の記事の資料は存在しないものとして 404 を返す。
func (s *Server) handleAttachmentDownload(c echo.Context) error {
	var att attachmentView
	err := s.db.Get(&att, `SELECT at.id, at.article_id, at.filename, at.file_path, at.file_size, at.mime_type,
			at.download_count, at.created_at
		FROM attachments at JOIN articles a ON a.id = at.article_id
		WHERE at.id = ? AND `+publishedCondition, c.Param("id"), article.StatusPublished, clockNow().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.serveAttachment(c, att, true)
}

// handleAdminAttachmentDownload は管理画面から添付資料を確認用にダウンロードさせる（公開前でもよく、数えない）。
func (s *Server) handleAdminAttachmentDownload(c echo.Context) error {
	var att attachmentView
	err := s.db.Get(&att, `SELECT `+attachmentColumns+` FROM attachments WHERE id = ?`, c.Param("aid"))
	if errors.Is(err, sql.ErrNoRows) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.serveAttachment(c, att, false)
}

// serveAttachment は添付資料をダウンロード用のヘッダー付きで返す。Range 要求にも応じ、
// count なら先頭からの取得のときだけダウンロード数を1増やす。
func (s *Server) serveAttachment(c echo.Context, att attachmentView, count bool) error {
	p, ok := s.attachmentPath(att.FilePath)
	if !ok {
		c.Logger().Errorf("attachment %s: invalid file path %q", att.ID, att.FilePath)
		return echo.ErrNotFound
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}

	if count && isFirstRange(c.Request()) {
		if _, err := s.db.Exec(`UPDATE attachments SET download_count = download_count + 1 WHERE id = ?`, att.ID); err != nil {
			c.Logger().Warnf("attachment %s: count download: %v", att.ID, err)
		}
	}
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, att.MimeType)
	h.Set(echo.HeaderXContentTypeOptions, "nosniff")
	h.Set(echo.HeaderContentDisposition, contentDisposition(att.Filename))
	// 万一ブラウザが表示しても、スクリプトの実行や外部への通信をさせない。
	h.Set(echo.HeaderContentSecurityPolicy, "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-cache")
	http.ServeContent(c.Response(), c.Request(), "", st.ModTime(), f)
	return nil
}

// isFirstRange は要求がファイルの先頭から取得するものか（Range なし、または bytes=0- から）を返す。
// 分割ダウンロードの続きの要求でダウンロード数を重複して数えないために使う。
func isFirstRange(r *http.Request) bool {
	rng := r.Header.Get("Range")
	return rng == "" || strings.HasPrefix(strings.ReplaceAll(rng, " ", ""), "bytes=0-")
}

// attachmentPath は保存ファイル名を AttachmentDir 内のパスに変換する。
// 区切り文字や ".." を含む名前（DB の改ざんなど）はディレクトリトラバーサルを防ぐため拒否する。
func (s *Server) attachmentPath(name string) (string, bool) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || filepath.Base(name) != name {
		return "", false
	}
	dir := filepath.Clean(s.cfg.AttachmentDir)
	p := filepath.Join(dir, name)
	if rel, err := filepath.Rel(dir, p); err != nil || rel != name {
		return "", false
	}
	return p, true
}

// saveAttachment は fh を検査して AttachmentDir に保存し、attachments に登録する。
func (s *Server) saveAttachment(fh *multipart.FileHeader, articleID, userID string) (*attachmentView, error) {
	if fh.Size > s.cfg.MaxAttachmentSize {
		return nil, errAttachmentTooLarge
	}
	filename := sanitizeFilename(fh.Filename)
	ext := strings.ToLower(path.Ext(filename))
	typ, ok := attachmentTypes[ext]
	if !ok {
		return nil, errAttachmentType
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := inspectAttachment(f, fh.Size, typ); err != nil {
		return nil, err
	}

	id, err := newAttachmentID()
	if err != nil {
		return nil, err
	}
	stored := id + ext
	dst := filepath.Join(s.cfg.AttachmentDir, stored)
	size, err := writeAttachmentFile(dst, f, s.cfg.MaxAttachmentSize)
	if err != nil {
		return nil, err
	}
	att := &attachmentView{
		ID: id, ArticleID: articleID, Filename: filename, FilePath: stored,
		FileSize: size, MimeType: typ.MIME, CreatedAt: time.Now().UTC(),
	}

	tx, err := s.db.Beginx()
	if err != nil {
		os.Remove(dst)
		return nil, fmt.Errorf("server: save attachment: %w", err)
	}
	defer tx.Rollback()

	var count int
	if err := tx.Get(&count, `SELECT COUNT(*) FROM attachments WHERE article_id = ?`, articleID); err != nil {
		os.Remove(dst)
		return nil, fmt.Errorf("server: save attachment: %w", err)
	}
	if count >= maxAttachmentsPerArticle {
		os.Remove(dst)
		return nil, errTooManyAttachments
	}

	_, err = tx.Exec(`INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, att.ID, att.ArticleID, att.Filename, att.FilePath, att.FileSize, att.MimeType, userID, att.CreatedAt)
	if err != nil {
		os.Remove(dst)
		return nil, fmt.Errorf("server: save attachment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		os.Remove(dst)
		return nil, fmt.Errorf("server: save attachment: %w", err)
	}
	return att, nil
}

// writeAttachmentFile は r を一時ファイルに書いてから dst に置き換える。書き込みが max を超えたら中断する。
// 実行権限を付けず、所有者とグループだけが読めるようにする。
func writeAttachmentFile(dst string, r io.ReadSeeker, max int64) (int64, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return 0, fmt.Errorf("server: save attachment: %w", err)
	}
	defer os.Remove(tmp.Name()) // Rename 後は存在しないため何もしない。
	n, err := io.Copy(tmp, io.LimitReader(r, max+1))
	if err == nil && n > max {
		err = errAttachmentTooLarge
	}
	if err == nil {
		err = tmp.Chmod(0o640)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		if errors.Is(err, errAttachmentTooLarge) {
			return 0, err
		}
		return 0, fmt.Errorf("server: save attachment: %w", err)
	}
	return n, nil
}

// inspectAttachment はファイルの中身が実行ファイル・スクリプトでなく、拡張子の形式と一致するかを確かめる。
func inspectAttachment(f multipart.File, size int64, typ attachmentType) error {
	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	head = head[:n]
	for _, magic := range executableMagics {
		if bytes.HasPrefix(head, magic) {
			return errAttachmentExecutable
		}
	}
	return typ.check(head, f, size)
}

func checkPDF(head []byte, _ io.ReaderAt, _ int64) error {
	if !bytes.HasPrefix(head, []byte("%PDF-")) {
		return errAttachmentMismatch
	}
	return nil
}

func checkZip(head []byte, r io.ReaderAt, size int64) error {
	if !bytes.HasPrefix(head, []byte("PK\x03\x04")) && !bytes.HasPrefix(head, []byte("PK\x05\x06")) {
		return errAttachmentMismatch
	}
	if _, err := zip.NewReader(r, size); err != nil {
		return errAttachmentMismatch
	}
	return nil
}

// checkOOXML は Office Open XML（ZIP の中に [Content_Types].xml と dir 配下の本体を持つ）かを確かめる。
// マクロ（vbaProject.bin）を含むファイルは、拡張子を .docx などに変えたものも拒否する。
func checkOOXML(dir string) func([]byte, io.ReaderAt, int64) error {
	return func(head []byte, r io.ReaderAt, size int64) error {
		if !bytes.HasPrefix(head, []byte("PK\x03\x04")) {
			return errAttachmentMismatch
		}
		zr, err := zip.NewReader(r, size)
		if err != nil {
			return errAttachmentMismatch
		}
		var contentTypes, body bool
		for _, f := range zr.File {
			name := strings.ToLower(f.Name)
			switch {
			case name == "[content_types].xml":
				contentTypes = true
			case strings.HasSuffix(name, "vbaproject.bin"):
				return errAttachmentMacro
			case strings.HasPrefix(name, dir):
				body = true
			}
		}
		if !contentTypes || !body {
			return errAttachmentMismatch
		}
		return nil
	}
}

// checkText はテキストファイル（TXT・CSV）かを確かめる。バイナリや HTML・PHP などのマークアップは拒否する。
// Shift_JIS の CSV（Excel の既定）も受け付けるため、文字コードは問わない。
func checkText(head []byte, _ io.ReaderAt, _ int64) error {
	if !strings.HasPrefix(http.DetectContentType(head), "text/plain") {
		return errAttachmentMismatch
	}
	trimmed := bytes.TrimLeft(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(trimmed) > 1 && trimmed[0] == '<' && (trimmed[1] == '?' || trimmed[1] == '!' || isASCIILetter(trimmed[1])) {
		return errAttachmentExecutable
	}
	return nil
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// attachmentError は添付の検査エラーを利用者向けの HTTP エラーに変換する。
func attachmentError(err error, max int64) error {
	switch {
	case errors.Is(err, errTooManyAttachments):
		return echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("1つの記事に添付できる資料は%d件までです", maxAttachmentsPerArticle))
	case errors.Is(err, errAttachmentTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("ファイルのサイズが大きすぎます（%dMBまで）", max>>20))
	case errors.Is(err, errAttachmentType):
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "添付できるのは PDF・ZIP・TXT・CSV・Word・Excel・PowerPoint のファイルです")
	case errors.Is(err, errAttachmentExecutable):
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "実行ファイルやスクリプトは添付できません")
	case errors.Is(err, errAttachmentMacro):
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "マクロを含む Office ファイルは添付できません")
	case errors.Is(err, errAttachmentMismatch):
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "ファイルの拡張子と中身の形式が一致しません")
	}
	return err
}

// attachmentList は記事の添付資料を古い順に返す。CanEdit は呼び出し側で設定する。
func (s *Server) attachmentList(c echo.Context, articleID string) (attachmentListData, error) {
	data := attachmentListData{
		ArticleID: articleID,
		Items:     []attachmentView{},
		CSRFToken: csrfToken(c),
		MaxMB:     s.cfg.MaxAttachmentSize >> 20,
		Accept:    attachmentAccept,
		Lang:      string(language(c)),
	}
	err := s.db.SelectContext(c.Request().Context(), &data.Items, `SELECT `+attachmentColumns+` FROM attachments
		WHERE article_id = ? ORDER BY julianday(created_at), id`, articleID)
	if err != nil {
		return data, fmt.Errorf("server: list attachments: %w", err)
	}
	return data, nil
}

// sanitizeFilename は利用者に見せる元のファイル名を整える。パスの部分・制御文字・書式文字
// （右から左への上書きで拡張子を偽装する U+202E など）を除き、NFC に正規化して長さを制限する。
func sanitizeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, norm.NFC.String(name))
	name = strings.Trim(name, " .")
	if name == "" || name == "/" {
		return "file"
	}
	if r := []rune(name); len(r) > maxFilenameRunes {
		ext := []rune(path.Ext(name))
		if len(ext) >= maxFilenameRunes {
			ext = nil
		}
		name = string(r[:maxFilenameRunes-len(ext)]) + string(ext)
	}
	return name
}

// contentDisposition はダウンロード用の Content-Disposition を返す。
// 古いブラウザ向けに ASCII だけの filename と、日本語名を RFC 5987 で符号化した filename* を併記する。
func contentDisposition(name string) string {
	var fallback strings.Builder
	for _, r := range name {
		if r >= 0x20 && r < 0x7f && r != '"' && r != '\\' {
			fallback.WriteRune(r)
		} else {
			fallback.WriteByte('_')
		}
	}
	return `attachment; filename="` + fallback.String() + `"; filename*=UTF-8''` + rfc5987Escape(name)
}

// rfc5987Escape は RFC 5987 の attr-char 以外をパーセント符号化する。
func rfc5987Escape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isASCIILetter(c) || (c >= '0' && c <= '9') || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// markdownLinkText はファイル名から Markdown のリンク構文を壊す文字を除く。
func markdownLinkText(s string) string { return markdownAltReplacer.Replace(s) }

// formatFileSize はバイト数を「1.5 MB」のような表示にする。
func formatFileSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func newAttachmentID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("server: generate attachment id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
