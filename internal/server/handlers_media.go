package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/image"
)

// mediaResponse は画像アップロード API の JSON レスポンス。URL はサイトルートからのパス。
type mediaResponse struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	OriginalURL string `json:"original_url"`
	LargeURL    string `json:"large_url"`
	MediumURL   string `json:"medium_url"`
	SmallURL    string `json:"small_url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	MimeType    string `json:"mime_type"`
	Alt         string `json:"alt"`
	Caption     string `json:"caption"`
	// Markdown はエディタのカーソル位置にそのまま挿入できる画像構文。
	Markdown string `json:"markdown"`
	// HTML は srcset 付きのレスポンシブな <img> タグ。
	HTML string `json:"html"`
}

// markdownAltReplacer は代替テキストから Markdown の画像構文を壊す文字を除く。
var markdownAltReplacer = strings.NewReplacer("[", "", "]", "", "\\", "", "\r", "", "\n", " ")

// handleMediaUpload は multipart の file フィールドの画像を検査・WebP 変換して保存し、
// エディタに挿入する Markdown とレスポンシブ画像タグを返す。任意の alt・caption はメディアライブラリの
// 代替テキスト・キャプションとして保存する。
func (s *Server) handleMediaUpload(c echo.Context) error {
	fh, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "画像ファイルを選択してください")
	}
	meta, msg := readMediaMeta(c)
	if msg != "" {
		return echo.NewHTTPError(http.StatusBadRequest, msg)
	}
	if fh.Size > s.cfg.MaxUploadSize {
		return uploadError(image.ErrFileTooLarge)
	}
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()

	img, err := image.Process(f, fh.Filename, image.ProcessOptions{
		MaxUploadSize: s.cfg.MaxUploadSize,
		OutputDir:     s.cfg.UploadDir,
	})
	if err != nil {
		return uploadError(err)
	}

	id, err := newMediaID()
	if err != nil {
		removeProcessed(img)
		return err
	}
	_, err = s.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, path_medium, path_small,
			width, height, mime_type, uploaded_by, alt, caption, taken_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, fh.Filename, img.OriginalPath, img.LargePath, img.MediumPath, img.SmallPath,
		img.Width, img.Height, img.MimeType, currentUser(c).ID, meta.Alt, meta.Caption, image.TakenAtValue(img.TakenAt, time.Now()))
	if err != nil {
		removeProcessed(img)
		return fmt.Errorf("server: save media: %w", err)
	}
	// アップロードした画像にはまだタグが無く、どのフォトギャラリーにも出ないため、公開サーバーへの未反映としては
	// 記録しない（タグを付けたときに記録する。handlers_dam.go の markMediaDirty）。

	alt := meta.Alt
	if alt == "" {
		alt = strings.TrimSpace(markdownAltReplacer.Replace(strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename))))
	}
	// 保存先ディレクトリに依らず、配信 URL（/uploads/<ファイル名>）を返す。
	public := &image.ProcessedImage{
		OriginalPath: uploadURL(img.OriginalPath),
		LargePath:    uploadURL(img.LargePath),
		MediumPath:   uploadURL(img.MediumPath),
		SmallPath:    uploadURL(img.SmallPath),
		Width:        img.Width,
		Height:       img.Height,
		MimeType:     img.MimeType,
	}
	return c.JSON(http.StatusOK, mediaResponse{
		ID:          id,
		URL:         public.LargePath,
		OriginalURL: public.OriginalPath,
		LargeURL:    public.LargePath,
		MediumURL:   public.MediumPath,
		SmallURL:    public.SmallPath,
		Width:       img.Width,
		Height:      img.Height,
		MimeType:    img.MimeType,
		Alt:         alt,
		Caption:     meta.Caption,
		Markdown:    mediaMarkdown(alt, fh.Filename, meta.Caption, public.LargePath),
		HTML:        image.RenderResponsiveImg(public, alt, ""),
	})
}

// uploadError は image.Process のエラーを利用者向けの HTTP エラーに変換する。
func uploadError(err error) error {
	switch {
	case errors.Is(err, image.ErrFileTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "画像のサイズが大きすぎます（15MBまで）")
	case errors.Is(err, image.ErrUnsupportedType):
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "JPEG・PNG・WebP の画像をアップロードしてください")
	case errors.Is(err, image.ErrMIMEMismatch):
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "ファイルの拡張子と中身の形式が一致しません")
	case errors.Is(err, image.ErrCorruptImage):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "画像が壊れているため読み込めません")
	case errors.Is(err, image.ErrTooManyPixels):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "画像の解像度が大きすぎます")
	}
	return err
}

// uploadURL は保存パスを UploadURLPrefix 配下の URL に変換する。
func uploadURL(p string) string {
	return UploadURLPrefix + path.Base(p)
}

func removeProcessed(img *image.ProcessedImage) {
	for _, p := range []string{img.OriginalPath, img.LargePath, img.MediumPath, img.SmallPath} {
		os.Remove(filepath.FromSlash(p))
	}
}

func newMediaID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("server: generate media id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// MediaUsedArticle は画像を使用している記事の情報。PublicURL は公開ページのパス（articlePublicPath。種類・言語に応じる）、
// Status は記事の状態。メディアライブラリは公開中の記事なら PublicURL を既定のブラウザで開き、それ以外はプレビューを開く。
type MediaUsedArticle struct {
	ID        string         `json:"id" db:"id"`
	Title     string         `json:"title" db:"title"`
	Slug      string         `json:"slug" db:"slug"`
	Status    article.Status `json:"status" db:"status"`
	Kind      article.Kind   `json:"kind" db:"kind"`
	Lang      string         `json:"lang" db:"lang"`
	PublicURL string         `json:"public_url" db:"-"`
	// Published は読者に見えている（公開中で、予約の日時を過ぎた）記事か。予約中の記事はプレビューで開く。
	Published   bool       `json:"published" db:"-"`
	PublishedAt *time.Time `json:"-" db:"published_at"`
	// AsFeatured はアイキャッチに、InBody は本文に、この画像のファイルを使っているか（両方のこともある）。
	AsFeatured bool `json:"as_featured" db:"as_featured"`
	InBody     bool `json:"in_body" db:"in_body"`
}

// MediaUsageResponse は画像詳細・使用中記事一覧 API のレスポンス。
type MediaUsageResponse struct {
	MediaID  string             `json:"media_id"`
	Count    int                `json:"count"`
	Articles []MediaUsedArticle `json:"articles"`
}

// mediaUsageArticles は m のファイルを本文またはアイキャッチで参照している記事の一覧を返す。
// ゴミ箱（status = 'deleted'）の記事は除外する。執筆チーム内では非公開の記事を共有するため、誰の記事でも並べる。
func (s *Server) mediaUsageArticles(m mediaRecord) ([]MediaUsedArticle, error) {
	names := m.fileNames()
	if len(names) == 0 {
		return []MediaUsedArticle{}, nil
	}
	// 本文・アイキャッチのどちらで使っているかを、ファイル名ごとの条件の OR で求める（mediaUsageCount と同じ条件）。
	featured, body, condArgs := mediaUsageCond(names)
	args := slices.Concat(condArgs, condArgs, condArgs, condArgs)
	var articles []MediaUsedArticle
	err := s.db.Select(&articles, `SELECT id, title, slug, status, kind, COALESCE(NULLIF(lang, ''), 'ja') AS lang, published_at,
		`+featured+` AS as_featured, `+body+` AS in_body
		FROM articles WHERE (`+featured+` OR `+body+`) AND status != 'deleted' ORDER BY updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	if articles == nil {
		articles = []MediaUsedArticle{}
	}
	// 読者に見えるか（予約中でないか）は、公開サイトと同じ時計（clockNow）で決める。
	now := clockNow()
	for i := range articles {
		a := &articles[i]
		a.PublicURL = articlePublicPath(a.Kind, a.Slug, a.Lang)
		a.Published = a.Status == article.StatusPublished && (a.PublishedAt == nil || !a.PublishedAt.After(now))
	}
	return articles, nil
}

// handleMediaArticlesAPI は画像を使用している記事の一覧（ID, Title, Slug）を JSON で返す。
func (s *Server) handleMediaArticlesAPI(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid media id")
	}
	m, err := s.getMedia(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "media not found")
		}
		return err
	}
	articles, err := s.mediaUsageArticles(m)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, MediaUsageResponse{
		MediaID:  id,
		Count:    len(articles),
		Articles: articles,
	})
}
