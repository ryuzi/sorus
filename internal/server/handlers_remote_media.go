package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"sorus/internal/article"
	"sorus/internal/markdown"
)

// フォトギャラリーの画像の反映（sync_media の送信側。受信側は handlers_sync_media.go）。公開サイトのギャラリー（トップページの
// ギャラリー・本文の :::gallery）はタグで media の行を引くため、「公開サーバーへ反映する」と記事の直接公開のときに、
// そのタグの付いたメディアライブラリの画像の記録と画像ファイルを公開サーバーへ送る。

const (
	// maxRemoteMediaItems は1回の反映で送る画像の記録の数。タグごとに新しい順に数え、超えた古い画像は送らない
	// （ギャラリーは1つで最大100枚、トップページは最大 maxTopGalleryLimit 枚を新しい順に出す）。
	maxRemoteMediaItems = 300
	// remoteMediaPushDeadline はギャラリーの画像を送る反映の、ブラウザへの応答の書き込みの期限。画像を何回かに分けて
	// 送るため、サーバーの WriteTimeout（60秒）より延ばす。
	remoteMediaPushDeadline = 5 * time.Minute
)

// errRemoteMediaUnsupported は公開サーバーの sorus が sync_media を知らない（古い）ことを示す。
var errRemoteMediaUnsupported = errors.New("remote sync: sync_media is not supported by the public server")

// remoteMediaResult はギャラリーの画像の反映の結果。
type remoteMediaResult struct {
	// Records は公開サーバーに保存した（または同じ内容で保存済みだった）記録の数。
	Records int
	// Files は公開サーバーに無かったため送った画像ファイルの数。
	Files int
	// Untagged は公開サーバーで、このPCでタグを外した（または削除した）ためにタグを外した画像の数。
	Untagged int
	// UntagUnsupported は公開サーバーの sorus が古く、タグを外したことを反映できなかったか（応答に untagged が無い）。
	UntagUnsupported bool
}

// remoteGalleryTags は「公開サーバーへ反映する」で画像を送るギャラリーのタグを返す。トップページのフォトギャラリーの
// タグ top を先頭に、公開中の記事の本文の :::gallery のタグを続ける（重複なし・maxSyncMediaTags 個まで）。
// 記事の :::gallery も公開サーバーの media をタグで引くため、メディアライブラリでタグを変えたことを、記事を公開し直さずに
// 反映できるようにする。
func (s *Server) remoteGalleryTags(ctx context.Context, top string) ([]string, error) {
	var tags []string
	if top = strings.TrimSpace(top); top != "" {
		tags = append(tags, top)
	}
	var bodies []string
	if err := s.db.SelectContext(ctx, &bodies, `SELECT a.content_markdown FROM articles a WHERE `+publishedCondition+`
			AND instr(a.content_markdown, ':::gallery') > 0 ORDER BY a.published_at DESC, a.id`,
		article.StatusPublished, clockNow().UTC()); err != nil {
		return nil, fmt.Errorf("server: remote media: list gallery articles: %w", err)
	}
	for _, body := range bodies {
		for _, tag := range galleryTagsIn(body) {
			if len(tags) >= maxSyncMediaTags {
				return tags, nil
			}
			if !slices.Contains(tags, tag) {
				tags = append(tags, tag)
			}
		}
	}
	return tags, nil
}

// remoteTagMediaIDs は tags のタグごとに、いまそのタグが付いているメディアライブラリの画像の ID をすべて返す
// （sync_media の TagMediaIDs）。公開サーバーはこの一覧に無い画像からタグを外すため、送る記録の上限（maxRemoteMediaItems）
// や画像ファイルの有無にかかわらず全部を並べる。公開サーバーが受け取れない形のタグ、画像が maxSyncMediaTagIDs 枚を
// 超えるタグは、タグを外す対象にしない（一覧に含めない）。
func (s *Server) remoteTagMediaIDs(ctx context.Context, tags []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, tag := range tags {
		if !validSyncMediaTag(tag) || len(out) >= maxSyncMediaTags {
			continue
		}
		if _, done := out[tag]; done {
			continue
		}
		var ids []string
		if err := s.db.SelectContext(ctx, &ids, `SELECT id FROM media
			WHERE `+mediaTagMatchSQL+`
			ORDER BY created_at DESC, rowid DESC LIMIT ?`, mediaTagMatchArg(tag), maxSyncMediaTagIDs+1); err != nil {
			return nil, fmt.Errorf("server: remote media: ids of %q: %w", tag, err)
		}
		if len(ids) > maxSyncMediaTagIDs {
			continue
		}
		// 公開サーバーが受け取れない形の ID の画像は、公開サーバーにも無い（normalizeSyncMedia が断る）。
		out[tag] = slices.DeleteFunc(ids, func(id string) bool { return !syncArticleIDPattern.MatchString(id) })
	}
	return out, nil
}

// galleryTagsIn は Markdown の本文が :::gallery で使うタグを、出現順・重複なしで返す。公開サイトと同じパーサーで
// 読むため、コードブロックの中の記法などは含まない。
func galleryTagsIn(text string) []string {
	var tags []string
	_, _ = markdown.RenderWithGalleryQuery(text, func(q markdown.GalleryQuery) ([]markdown.GalleryImage, error) {
		if tag := strings.TrimSpace(q.Tag); tag != "" && !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
		return nil, nil
	})
	return tags
}

// remoteMediaFor は tags のどれかが付いたメディアライブラリの画像を、公開サーバーへ送る記録にして返す。タグごとに新しい順で、
// 合わせて maxRemoteMediaItems 件まで。files は記録ごとの画像ファイルのローカルのパス（送るファイル名をキーにする）。
// 公開サーバーが受け取れない名前のファイルと、ローカルに無いファイルは記録から外し、画像ファイルが1つも残らない記録は送らない。
func (s *Server) remoteMediaFor(ctx context.Context, tags []string) (records []SyncMedia, files map[string]string, err error) {
	files = map[string]string{}
	seen := map[string]bool{}
	for _, tag := range tags {
		if len(records) >= maxRemoteMediaItems {
			break
		}
		var rows []mediaRecord
		if err := s.db.SelectContext(ctx, &rows, `SELECT id, filename, path_original, path_large, path_medium, path_small,
				width, height, mime_type, alt, caption, ai_tags, created_at FROM media
			WHERE `+mediaTagMatchSQL+`
			ORDER BY created_at DESC, rowid DESC LIMIT ?`, mediaTagMatchArg(tag), maxRemoteMediaItems); err != nil {
			return nil, nil, fmt.Errorf("server: remote media: list %q: %w", tag, err)
		}
		for _, row := range rows {
			if seen[row.ID] || len(records) >= maxRemoteMediaItems {
				continue
			}
			seen[row.ID] = true
			m := SyncMedia{ID: row.ID, Filename: row.Filename, Width: row.Width, Height: row.Height, MimeType: row.MimeType,
				Alt: row.Alt, Caption: row.Caption, AITags: row.AITags, CreatedAt: row.CreatedAt}
			paths := map[string]string{}
			for _, f := range []struct {
				dst  *string
				path string
			}{{&m.PathOriginal, row.PathOriginal}, {&m.PathLarge, row.PathLarge}, {&m.PathMedium, row.PathMedium}, {&m.PathSmall, row.PathSmall}} {
				name, ok := safeUploadName(f.path)
				if !ok || !syncImageNamePattern.MatchString(name) {
					continue
				}
				local := filepath.Join(s.cfg.UploadDir, name)
				if info, err := os.Stat(local); err != nil || !info.Mode().IsRegular() {
					continue
				}
				*f.dst = name
				paths[name] = local
			}
			if len(paths) == 0 {
				continue
			}
			records = append(records, m)
			for name, local := range paths {
				files[name] = local
			}
		}
	}
	return records, files, nil
}

// pushRemoteMedia は tags のどれかが付いた画像の記録と画像ファイルを公開サーバーへ sync_media で送る。まず記録だけを送り、
// 公開サーバーに無いと言われた画像ファイルを、その記録とともに何回かに分けて送り直す（2回目からの反映では、変わった
// 記録だけを送る程度の通信で済む）。最初の要求には、タグごとのいまの画像の一覧（TagMediaIDs）も付け、このPCでタグを
// 外した画像・削除した画像から、公開サーバーでもそのタグを外してもらう（タグの付いた画像が1枚も無くなったタグも送る）。
// 公開サーバーの sorus が sync_media を知らないときは errRemoteMediaUnsupported を返す。
func (s *Server) pushRemoteMedia(ctx context.Context, cfg remoteConfig, tags []string) (remoteMediaResult, error) {
	var result remoteMediaResult
	records, files, err := s.remoteMediaFor(ctx, tags)
	if err != nil {
		return result, err
	}
	tagIDs, err := s.remoteTagMediaIDs(ctx, tags)
	if err != nil || (len(records) == 0 && len(tagIDs) == 0) {
		return result, err
	}
	send := func(media []SyncMedia, images []SyncImage, tagIDs map[string][]string) (*SyncMediaResponse, error) {
		var out SyncMediaResponse
		err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionSyncMedia, Media: media, Images: images, TagMediaIDs: tagIDs}, &out)
		var re *remoteError
		if errors.As(err, &re) && re.Status == http.StatusBadRequest {
			// sync_media を知らない古い公開サーバーは「未対応の action」として 400 で断る（受け取る側は検査の誤りを 422 で返す）。
			return nil, errRemoteMediaUnsupported
		}
		if err != nil {
			return nil, err
		}
		if out.Action != SyncActionSyncMedia {
			return nil, &remoteError{Status: http.StatusOK, Message: "公開サーバーの応答を読み取れませんでした。公開サーバーの sorus を更新してください"}
		}
		return &out, nil
	}

	// 1. 記録だけを送る。画像ファイルがそろっている記録はこの時点で保存される。タグを外す一覧は最初の要求にだけ付ける
	// （送る記録が無くても、タグを外すために1回は送る）。
	missing := map[string]bool{}
	for start := 0; start == 0 || start < len(records); start += maxSyncMediaItems {
		var ids map[string][]string
		if start == 0 && len(tagIDs) > 0 {
			ids = tagIDs
		}
		out, err := send(records[start:min(start+maxSyncMediaItems, len(records))], nil, ids)
		if err != nil {
			return result, err
		}
		result.Records += len(out.Saved)
		for _, name := range out.Missing {
			missing[name] = true
		}
		if ids != nil {
			result.Untagged = len(out.Untagged)
			result.UntagUnsupported = out.Untagged == nil
		}
	}
	if len(missing) == 0 {
		return result, nil
	}

	// 2. 画像ファイルが足りない記録を、無いファイルとともに送り直す。1回の要求は maxSyncImages ファイル・
	// maxRemoteImageBytes バイトまでにし、1つの記録のファイルは同じ要求にまとめる。
	tooLarge := &remoteError{Status: http.StatusRequestEntityTooLarge,
		Message: fmt.Sprintf("フォトギャラリーの画像が大きすぎるため送れません（1枚の画像は合計%dMBまで）", maxRemoteImageBytes>>20)}
	var batch []SyncMedia
	var images []SyncImage
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out, err := send(batch, images, nil)
		if err != nil {
			return err
		}
		if len(out.Missing) > 0 {
			return &remoteError{Status: http.StatusOK, Message: "公開サーバーにフォトギャラリーの画像を保存できませんでした: " + strings.Join(out.Missing, ", ")}
		}
		result.Records += len(out.Saved)
		result.Files += len(images)
		batch, images, size = nil, nil, 0
		return nil
	}
	sent := map[string]bool{}
	for _, m := range records {
		resend := false
		var need []SyncImage
		needSize := 0
		for _, name := range m.files() {
			if !missing[name] {
				continue
			}
			resend = true
			// 同じ画像ファイルを複数の記録が参照するときは一度だけ送る（前の要求で送ったファイルは公開サーバーにある）。
			if sent[name] {
				continue
			}
			data, err := os.ReadFile(files[name])
			if err != nil {
				return result, fmt.Errorf("server: remote media: read %s: %w", name, err)
			}
			need = append(need, SyncImage{Name: name, Data: data})
			needSize += len(data)
		}
		if !resend {
			continue
		}
		if needSize > maxRemoteImageBytes {
			return result, tooLarge
		}
		if len(images)+len(need) > maxSyncImages || size+needSize > maxRemoteImageBytes || len(batch) >= maxSyncMediaItems {
			if err := flush(); err != nil {
				return result, err
			}
		}
		for _, img := range need {
			images = append(images, img)
			size += len(img.Data)
			sent[img.Name] = true
		}
		batch = append(batch, m)
	}
	if err := flush(); err != nil {
		return result, err
	}
	return result, nil
}
