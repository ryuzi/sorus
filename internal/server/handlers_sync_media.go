package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

// 公開サーバーが受け取るメディアライブラリの記録（sync_media）。公開サイトのフォトギャラリーは media の行を
// タグで引くため、PC はギャラリーに使う画像の記録とファイルをこの action で送る（handlers_remote_media.go）。

const (
	// maxSyncMediaItems は sync_media の1回の要求で受け付ける記録の数。画像ファイルは別に maxSyncImages まで。
	maxSyncMediaItems = 200
	// maxSyncMediaFilenameRunes は記録の元のファイル名の最大文字数。
	maxSyncMediaFilenameRunes = 255
	// maxSyncMediaMIMERunes は記録の MIME タイプの最大文字数。
	maxSyncMediaMIMERunes = 100
	// maxSyncMediaDimension は記録の幅・高さとして受け付ける最大値。
	maxSyncMediaDimension = 100000
	// maxSyncMediaTags は sync_media の1回の要求で、タグを外す対象として受け付けるタグの数（TagMediaIDs）。
	maxSyncMediaTags = 100
	// maxSyncMediaTagIDs は TagMediaIDs の1つのタグに並べられる画像 ID の数。これより多いタグは、PC がタグを外す
	// 対象にしない（送らない）。
	maxSyncMediaTagIDs = 5000
	// syncMediaTimeLayout は created_at を保存する形。CURRENT_TIMESTAMP と同じ形にし、公開サーバーで登録した行と
	// 文字列のまま正しく並べられるようにする（ギャラリーは created_at で並べる）。
	syncMediaTimeLayout = "2006-01-02 15:04:05"
)

// syncMedia は sync_media を処理する。記録と画像ファイルをすべて検査してから、画像ファイルを保存し、参照する画像ファイルが
// そろっている記録だけを同じ ID で保存（UPSERT）する。そろっていない記録は保存せず、無いファイル名を Missing で返す
// （記録だけを先に送り、公開サーバーに無いファイルだけを送り直してもらうため。ファイル名は画像ごとの乱数で決まり、
// 同じ名前のファイルの中身は変わらない）。
//
// TagMediaIDs があれば、同じトランザクションで、このトークンの同期ユーザーが保存した画像のうち、そのタグが付いているのに
// 一覧に無い画像からタグを外し、その ID を Untagged で返す（removeUnlistedSyncTags）。
func (s *Server) syncMedia(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	tagIDs, err := normalizeSyncTagMediaIDs(req.TagMediaIDs)
	if err != nil {
		return err
	}
	items, err := normalizeSyncMedia(req.Media, len(tagIDs) > 0)
	if err != nil {
		return err
	}
	if err := s.validateSyncImages(req.Images); err != nil {
		return err
	}
	// 画像ファイルは記録が参照するものだけを受け付ける（記録の伴わない画像の置き場所にさせない）。
	referenced := map[string]bool{}
	for _, m := range items {
		for _, name := range m.files() {
			referenced[name] = true
		}
	}
	for _, img := range req.Images {
		if !referenced[img.Name] {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "記録が参照していない画像は送れません: "+img.Name)
		}
	}

	ctx := c.Request().Context()
	userID, err := s.syncUser(ctx, claims)
	if err != nil {
		return err
	}
	created, _, err := s.saveSyncImages(req.Images)
	if err != nil {
		return err
	}

	res := SyncMediaResponse{Action: SyncActionSyncMedia, Saved: []string{}, Missing: []string{}, Untagged: []string{}}
	var ready []SyncMedia
	for _, m := range items {
		complete := true
		for _, name := range m.files() {
			info, err := os.Stat(filepath.Join(s.cfg.UploadDir, name))
			if err == nil && info.Mode().IsRegular() {
				continue
			}
			if err != nil && !os.IsNotExist(err) {
				removeFiles(created)
				return fmt.Errorf("server: sync media: stat %s: %w", name, err)
			}
			complete = false
			if !slices.Contains(res.Missing, name) {
				res.Missing = append(res.Missing, name)
			}
		}
		if complete {
			ready = append(ready, m)
		}
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		removeFiles(created)
		return err
	}
	defer tx.Rollback()
	for _, m := range ready {
		path := func(name string) string {
			if name == "" {
				return ""
			}
			return filepath.ToSlash(filepath.Join(s.cfg.UploadDir, name))
		}
		// uploaded_by は追加したときだけ同期のユーザーにする（更新では元の値を保つ）。
		if _, err := tx.ExecContext(ctx, `INSERT INTO media (id, filename, path_original, path_large, path_medium,
				path_small, width, height, mime_type, uploaded_by, alt, caption, ai_tags, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET filename = excluded.filename, path_original = excluded.path_original,
				path_large = excluded.path_large, path_medium = excluded.path_medium, path_small = excluded.path_small,
				width = excluded.width, height = excluded.height, mime_type = excluded.mime_type, alt = excluded.alt,
				caption = excluded.caption, ai_tags = excluded.ai_tags, created_at = excluded.created_at`,
			m.ID, m.Filename, path(m.PathOriginal), path(m.PathLarge), path(m.PathMedium), path(m.PathSmall),
			m.Width, m.Height, m.MimeType, userID, m.Alt, m.Caption, m.AITags,
			m.CreatedAt.UTC().Format(syncMediaTimeLayout)); err != nil {
			removeFiles(created)
			return fmt.Errorf("server: sync media: save %s: %w", m.ID, err)
		}
		res.Saved = append(res.Saved, m.ID)
	}
	if res.Untagged, err = removeUnlistedSyncTags(ctx, tx, userID, tagIDs); err != nil {
		removeFiles(created)
		return err
	}
	if err := tx.Commit(); err != nil {
		removeFiles(created)
		return fmt.Errorf("server: sync media: %w", err)
	}
	return c.JSON(http.StatusOK, res)
}

// removeUnlistedSyncTags は tagIDs のタグごとに、userID（この要求のトークンの同期ユーザー）が保存した画像のうち、
// そのタグが付いているのに一覧に無い画像からそのタグを外し、外した画像の ID を返す（同じ画像は1回だけ）。
// 画像の記録とファイルは消さない（ほかのタグのギャラリーや、記事の本文の画像として使われていることがあるため）。
// 公開サーバーで直接登録した画像と、ほかの招待（ほかの PC）が送った画像は uploaded_by が違うため触れない。
func removeUnlistedSyncTags(ctx context.Context, tx *sqlx.Tx, userID string, tagIDs map[string][]string) ([]string, error) {
	untagged := []string{}
	tags := make([]string, 0, len(tagIDs))
	for tag := range tagIDs {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	for _, tag := range tags {
		keep := make(map[string]bool, len(tagIDs[tag]))
		for _, id := range tagIDs[tag] {
			keep[id] = true
		}
		var rows []struct {
			ID     string `db:"id"`
			AITags string `db:"ai_tags"`
		}
		if err := tx.SelectContext(ctx, &rows, `SELECT id, ai_tags FROM media
			WHERE uploaded_by = ? AND `+mediaTagMatchSQL+`
			ORDER BY rowid`, userID, mediaTagMatchArg(tag)); err != nil {
			return nil, fmt.Errorf("server: sync media: list tag %q: %w", tag, err)
		}
		for _, row := range rows {
			if keep[row.ID] {
				continue
			}
			rest := slices.DeleteFunc(strings.Split(normalizeMediaTags(row.AITags), ", "), func(t string) bool { return t == tag })
			if _, err := tx.ExecContext(ctx, `UPDATE media SET ai_tags = ? WHERE id = ?`, strings.Join(rest, ", "), row.ID); err != nil {
				return nil, fmt.Errorf("server: sync media: untag %s: %w", row.ID, err)
			}
			if !slices.Contains(untagged, row.ID) {
				untagged = append(untagged, row.ID)
			}
		}
	}
	return untagged, nil
}

// validSyncMediaTag はタグを外す対象として受け付けるタグか（normalizeMediaTags で形が変わらない1つのタグで、
// トップページのギャラリーのタグと同じ長さの上限まで・制御文字なし）。
func validSyncMediaTag(tag string) bool {
	return tag != "" && normalizeMediaTags(tag) == tag && utf8.RuneCountInString(tag) <= maxGalleryTagRunes && !hasControl(tag)
}

// normalizeSyncTagMediaIDs は sync_media の TagMediaIDs を検査し、ID の重複を除いて返す。不正なタグ・ID があれば 422 を返す。
func normalizeSyncTagMediaIDs(in map[string][]string) (map[string][]string, error) {
	invalid := func(format string, args ...any) error {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf(format, args...))
	}
	if len(in) > maxSyncMediaTags {
		return nil, invalid("タグを外す対象のタグは1回に%d個まで送れます", maxSyncMediaTags)
	}
	out := make(map[string][]string, len(in))
	for tag, ids := range in {
		if !validSyncMediaTag(tag) {
			return nil, invalid("タグを外す対象のタグが正しくありません: %q", tag)
		}
		if len(ids) > maxSyncMediaTagIDs {
			return nil, invalid("タグ「%s」の画像 ID は%d件まで送れます", tag, maxSyncMediaTagIDs)
		}
		list := make([]string, 0, len(ids))
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if !syncArticleIDPattern.MatchString(id) {
				return nil, invalid("タグ「%s」の画像 ID の形式が正しくありません: %q", tag, id)
			}
			if !seen[id] {
				seen[id] = true
				list = append(list, id)
			}
		}
		out[tag] = list
	}
	return out, nil
}

// normalizeSyncMedia は sync_media の記録を検査し、前後の空白を除いた・タグを整えた記録を返す。
// 不正な記録が1件でもあれば、何も保存せずに 422 を返す（400 は古い公開サーバーの「未対応の action」と区別するため使わない）。
// allowEmpty なら記録が無くてもよい（タグを外すだけの要求）。
func normalizeSyncMedia(in []SyncMedia, allowEmpty bool) ([]SyncMedia, error) {
	invalid := func(format string, args ...any) error {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf(format, args...))
	}
	if len(in) == 0 {
		if allowEmpty {
			return nil, nil
		}
		return nil, invalid("media を指定してください")
	}
	if len(in) > maxSyncMediaItems {
		return nil, invalid("画像の記録は1回に%d件まで送れます", maxSyncMediaItems)
	}
	out := make([]SyncMedia, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		m.ID = strings.TrimSpace(m.ID)
		if !syncArticleIDPattern.MatchString(m.ID) {
			return nil, invalid("画像の記録の ID の形式が正しくありません: %q", m.ID)
		}
		if seen[m.ID] {
			return nil, invalid("同じ ID の画像の記録が重複しています: %s", m.ID)
		}
		seen[m.ID] = true
		for _, p := range []*string{&m.PathOriginal, &m.PathLarge, &m.PathMedium, &m.PathSmall} {
			*p = strings.TrimSpace(*p)
			if *p != "" && !syncImageNamePattern.MatchString(*p) {
				return nil, invalid("画像の記録 %s のファイル名が正しくありません: %q", m.ID, *p)
			}
		}
		if len(m.files()) == 0 {
			return nil, invalid("画像の記録 %s に画像のファイル名がありません", m.ID)
		}
		m.Filename = strings.TrimSpace(m.Filename)
		m.MimeType = strings.TrimSpace(m.MimeType)
		m.Alt = strings.TrimSpace(m.Alt)
		m.Caption = strings.TrimSpace(normalizeNewlines(m.Caption))
		m.AITags = normalizeMediaTags(m.AITags)
		switch {
		case utf8.RuneCountInString(m.Filename) > maxSyncMediaFilenameRunes || hasControl(m.Filename):
			return nil, invalid("画像の記録 %s の元のファイル名が正しくありません", m.ID)
		case !strings.HasPrefix(m.MimeType, "image/") || utf8.RuneCountInString(m.MimeType) > maxSyncMediaMIMERunes || hasControl(m.MimeType):
			return nil, invalid("画像の記録 %s の MIME タイプが正しくありません: %q", m.ID, m.MimeType)
		case m.Width < 0 || m.Height < 0 || m.Width > maxSyncMediaDimension || m.Height > maxSyncMediaDimension:
			return nil, invalid("画像の記録 %s の寸法が正しくありません", m.ID)
		case utf8.RuneCountInString(m.Alt) > maxMediaAltRunes:
			return nil, invalid("画像の記録 %s の代替テキストは%d文字以内にしてください", m.ID, maxMediaAltRunes)
		case utf8.RuneCountInString(m.Caption) > maxMediaCaptionRunes:
			return nil, invalid("画像の記録 %s のキャプションは%d文字以内にしてください", m.ID, maxMediaCaptionRunes)
		case utf8.RuneCountInString(m.AITags) > maxMediaTagsRunes:
			return nil, invalid("画像の記録 %s のタグは%d文字以内にしてください", m.ID, maxMediaTagsRunes)
		}
		if m.CreatedAt.IsZero() {
			m.CreatedAt = time.Now()
		}
		out = append(out, m)
	}
	return out, nil
}
