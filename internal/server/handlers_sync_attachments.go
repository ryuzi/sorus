package server

// 記事の添付資料（PDF など）の受け取り（sync_attachments の受信側。送信側は handlers_remote_attachments.go）。
// 公開サイトの記事は添付資料を attachments の行と AttachmentDir のファイルから出し、本文のリンクは
// /attachments/<ID>/download を指すため、PC と同じ ID で行とファイルを保存する。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// syncAttachmentIDPattern は添付資料の ID（newAttachmentID が作る32桁の16進数）。
var syncAttachmentIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// syncAttachmentSHA256Pattern は添付資料の SHA-256（小文字の16進数）。
var syncAttachmentSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// syncAttachmentFile は検査済みの添付資料1件と、公開サーバーに保存するファイル名（ID + 拡張子）。
type syncAttachmentFile struct {
	SyncAttachment
	stored   string
	filename string
	mime     string
}

// bytesFile は bytes.Reader を multipart.File（Close を持つ）として inspectAttachment に渡すための型。
type bytesFile struct{ *bytes.Reader }

func (bytesFile) Close() error { return nil }

// syncAttachments は sync_attachments に応える。ArticleID の記事の添付資料の記録を同じ ID で保存（追加・更新）し、
// ファイルが無い（または中身が違う）記録は Missing として返す。ファイルの中身（Data）が付いた記録は、管理画面での
// 添付と同じ検査（拡張子・中身の形式・実行ファイルやマクロの拒否・上限）をしてから保存する。PruneAttachments なら、
// 一覧に無いその記事の添付資料を消す。
func (s *Server) syncAttachments(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	id, err := syncArticleIDFrom(req)
	if err != nil {
		return err
	}
	if len(req.Attachments) > maxAttachmentsPerArticle {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("添付資料は1つの記事に%d件までです", maxAttachmentsPerArticle))
	}
	files := make([]syncAttachmentFile, 0, len(req.Attachments))
	seen := map[string]bool{}
	for _, in := range req.Attachments {
		f, err := s.checkSyncAttachment(in)
		if err != nil {
			return err
		}
		if seen[f.ID] {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "同じ添付資料が重複しています: "+f.ID)
		}
		seen[f.ID] = true
		files = append(files, f)
	}

	ctx := c.Request().Context()
	if _, err := s.articles.GetByID(id); errors.Is(err, article.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "公開サーバーにこの記事がありません。先に記事を公開サーバーへ送ってください")
	} else if err != nil {
		return err
	}
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}

	out := SyncAttachmentsResponse{Action: SyncActionSyncAttachments, Saved: []string{}, Missing: []string{}, Removed: []string{}}
	for _, f := range files {
		ok, err := s.storeSyncAttachmentFile(f)
		if err != nil {
			return err
		}
		if !ok {
			out.Missing = append(out.Missing, f.ID)
			continue
		}
		if err := s.saveSyncAttachmentRow(ctx, id, userID, f); err != nil {
			return err
		}
		out.Saved = append(out.Saved, f.ID)
	}
	if req.PruneAttachments {
		removed, err := s.pruneSyncAttachments(ctx, id, files)
		if err != nil {
			return err
		}
		out.Removed = append(out.Removed, removed...)
	}
	return c.JSON(http.StatusOK, out)
}

// checkSyncAttachment は送られた記録の形を検査し、保存するファイル名と MIME タイプを決める（中身の検査は保存の直前）。
func (s *Server) checkSyncAttachment(in SyncAttachment) (syncAttachmentFile, error) {
	f := syncAttachmentFile{SyncAttachment: in}
	f.ID = strings.TrimSpace(in.ID)
	if !syncAttachmentIDPattern.MatchString(f.ID) {
		return f, echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("添付資料の ID が正しくありません: %q", in.ID))
	}
	f.filename = sanitizeFilename(in.Filename)
	ext := strings.ToLower(path.Ext(f.filename))
	typ, ok := attachmentTypes[ext]
	if f.filename == "" || !ok {
		return f, echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("添付できない形式のファイルです: %q", in.Filename))
	}
	f.stored, f.mime = f.ID+ext, typ.MIME
	if in.Size < 0 || in.Size > s.cfg.MaxAttachmentSize {
		return f, echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("添付資料「%s」が大きすぎます（公開サーバーの上限は1件%dMBです）", f.filename, s.cfg.MaxAttachmentSize>>20))
	}
	f.SHA256 = strings.ToLower(strings.TrimSpace(in.SHA256))
	if f.SHA256 != "" && !syncAttachmentSHA256Pattern.MatchString(f.SHA256) {
		return f, echo.NewHTTPError(http.StatusUnprocessableEntity, "添付資料の SHA-256 の形式が正しくありません: "+f.ID)
	}
	if in.Data != nil {
		if int64(len(in.Data)) != in.Size {
			return f, echo.NewHTTPError(http.StatusUnprocessableEntity, "添付資料の大きさが記録と違います: "+f.ID)
		}
		if f.SHA256 == "" || sha256Hex(in.Data) != f.SHA256 {
			return f, echo.NewHTTPError(http.StatusUnprocessableEntity, "添付資料の中身が途中で変わりました。もう一度送ってください: "+f.ID)
		}
		if err := inspectAttachment(bytesFile{bytes.NewReader(in.Data)}, in.Size, typ); err != nil {
			return f, attachmentError(err, s.cfg.MaxAttachmentSize)
		}
	}
	return f, nil
}

// storeSyncAttachmentFile は記録のファイルを用意する。中身が付いていれば保存し、付いていなければ公開サーバーにある
// ファイルが同じ中身か（PC にファイルが無く SHA-256 が空なら、ファイルがあるか）を確かめる。ファイルが用意できたかを返す。
func (s *Server) storeSyncAttachmentFile(f syncAttachmentFile) (bool, error) {
	dst, ok := s.attachmentPath(f.stored)
	if !ok {
		return false, echo.NewHTTPError(http.StatusUnprocessableEntity, "添付資料のファイル名が正しくありません: "+f.ID)
	}
	if f.Data != nil {
		if _, err := writeAttachmentFile(dst, bytes.NewReader(f.Data), s.cfg.MaxAttachmentSize); err != nil {
			return false, err
		}
		return true, nil
	}
	file, err := os.Open(dst)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("server: sync attachment: %w", err)
	}
	defer file.Close()
	if f.SHA256 == "" {
		return true, nil
	}
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("server: sync attachment: %w", err)
	}
	if info.Size() != f.Size {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return false, fmt.Errorf("server: sync attachment: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)) == f.SHA256, nil
}

// saveSyncAttachmentRow は添付資料の記録を同じ ID で保存する。同じ ID の記録があれば、ダウンロード数を残して更新する。
func (s *Server) saveSyncAttachmentRow(ctx context.Context, articleID, userID string, f syncAttachmentFile) error {
	created := f.CreatedAt.UTC()
	if f.CreatedAt.IsZero() {
		created = time.Now().UTC()
	}
	size := f.Size
	if f.Data == nil && f.SHA256 == "" {
		// PC にファイルが無いときは、公開サーバーにあるファイルの大きさを記録する。
		if p, ok := s.attachmentPath(f.stored); ok {
			if info, err := os.Stat(p); err == nil {
				size = info.Size()
			}
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO attachments (id, article_id, filename, file_path, file_size, mime_type, uploaded_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET article_id = excluded.article_id, filename = excluded.filename, file_path = excluded.file_path,
			file_size = excluded.file_size, mime_type = excluded.mime_type, created_at = excluded.created_at`,
		f.ID, articleID, f.filename, f.stored, size, f.mime, userID, created)
	if err != nil {
		return fmt.Errorf("server: sync attachment: save: %w", err)
	}
	return nil
}

// pruneSyncAttachments は articleID の添付資料のうち、keep に無いものの記録とファイルを消し、消した ID を返す。
func (s *Server) pruneSyncAttachments(ctx context.Context, articleID string, keep []syncAttachmentFile) ([]string, error) {
	var rows []attachmentView
	if err := s.db.SelectContext(ctx, &rows, `SELECT `+attachmentColumns+` FROM attachments WHERE article_id = ?`, articleID); err != nil {
		return nil, fmt.Errorf("server: sync attachment: list: %w", err)
	}
	var removed []string
	for _, r := range rows {
		if slices.ContainsFunc(keep, func(f syncAttachmentFile) bool { return f.ID == r.ID }) {
			continue
		}
		var file string
		err := s.db.GetContext(ctx, &file, `DELETE FROM attachments WHERE id = ? AND article_id = ? RETURNING file_path`, r.ID, articleID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("server: sync attachment: remove: %w", err)
		}
		if p, ok := s.attachmentPath(file); ok {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, fmt.Errorf("server: sync attachment: remove file: %w", err)
			}
		}
		removed = append(removed, r.ID)
	}
	return removed, nil
}

// sha256Hex は data の SHA-256 を小文字の16進数で返す。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fileSHA256 はファイル p の SHA-256（小文字の16進数）と大きさを返す。
func fileSHA256(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
