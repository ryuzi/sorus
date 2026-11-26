package server

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/markdown"
)

const (
	// mediaPerPage はメディアライブラリの 1 ページに並べる画像の数。
	mediaPerPage = 24
	// mediaAPIMaxPerPage はメディアピッカー API が 1 回に返す画像の上限。
	mediaAPIMaxPerPage = 60
	// maxMediaQueryTerms は検索語として使う語の上限。長い入力で LIKE の条件が膨らまないようにする。
	maxMediaQueryTerms = 8

	maxMediaAltRunes     = 300
	maxMediaCaptionRunes = 1000
	maxMediaTagsRunes    = 500

	// maxAnalyzeImageBytes は AI に送る画像ファイルの上限。Gemini のリクエスト全体の上限（20MB）に収める。
	maxAnalyzeImageBytes = 15 << 20

	// mediaTagCloudLimit はメディアライブラリのタグの絞り込みにそのまま並べるタグの数。残りは「ほかのタグ」に畳む。
	mediaTagCloudLimit = 30

	// yearTagSuffix は年のタグ（例: 2026年）の書き方。タグはデータなので表示言語によらない（internal/markdown/gallery.go の
	// 年のタグと同じ）。タグの編集欄は「年」のグループに分けて並べる（templates/admin/media.html・static/js/media.js）。
	yearTagSuffix = "年"
)

func init() {
	if !slices.Contains(pageNames, "media") {
		pageNames = append(pageNames, "media")
	}
	templateFuncs["yearTagSuffix"] = func() string { return yearTagSuffix }
}

// registerMediaRoutes はメディアライブラリ（DAM）の画面・メタデータ編集・削除・ピッカー API を、
// ログイン済み・CSRF 保護済みの /admin グループに登録する。削除は管理者だけが行える。
func (s *Server) registerMediaRoutes(authed *echo.Group, adminOnly echo.MiddlewareFunc) {
	authed.GET("/media", s.handleMediaLibrary)
	authed.GET("/api/media", s.handleMediaAPI)
	authed.GET("/api/media/tags", s.handleMediaTagsAPI)
	authed.GET("/api/media/:id/articles", s.handleMediaArticlesAPI)
	authed.POST("/media/:id", s.handleMediaUpdate)
	authed.POST("/media/:id/delete", s.handleMediaDelete, adminOnly)
	// 画像の AI 自動生成は、AI 機能を OFF にしたサイト（aiEnabled）では存在しないページと同じ 404 にする。
	authed.POST("/api/media/:id/analyze", s.handleMediaAnalyze, s.requireAIEnabled)
	authed.POST("/api/media/bulk-tags", s.handleMediaBulkTags)
}

type mediaBulkTagsRequest struct {
	MediaIDs   []string `json:"media_ids"`
	AddTags    []string `json:"add_tags"`
	RemoveTags []string `json:"remove_tags"`
}

// handleMediaBulkTags applies tag changes to a bounded set of media rows atomically.
func (s *Server) handleMediaBulkTags(c echo.Context) error {
	if !currentUser(c).HasRole(auth.RoleWriter, auth.RoleEditor, auth.RoleAdmin) {
		return echo.NewHTTPError(http.StatusForbidden, "forbidden")
	}
	var req mediaBulkTagsRequest
	if err := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 32<<10)).Decode(&req); err != nil {
		return mediaBulkTagsError(c, http.StatusBadRequest, "media_bulk_error_request")
	}
	if len(req.MediaIDs) < 1 || len(req.MediaIDs) > 100 || len(req.AddTags)+len(req.RemoveTags) == 0 {
		return mediaBulkTagsError(c, http.StatusBadRequest, "media_bulk_error_selection")
	}
	for _, tags := range [][]string{req.AddTags, req.RemoveTags} {
		for i, tag := range tags {
			if !validMediaBulkTag(tag) {
				return mediaBulkTagsError(c, http.StatusBadRequest, "media_bulk_error_tag")
			}
			tags[i] = strings.TrimSpace(tag)
		}
	}
	for _, tag := range append(slices.Clone(req.AddTags), req.RemoveTags...) {
		if tag == "" {
			return mediaBulkTagsError(c, http.StatusBadRequest, "media_bulk_error_tag")
		}
	}
	ids := make([]string, 0, len(req.MediaIDs))
	for _, id := range req.MediaIDs {
		if strings.TrimSpace(id) == "" || len(id) > 200 {
			return mediaBulkTagsError(c, http.StatusBadRequest, "media_bulk_error_selection")
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return fmt.Errorf("server: begin media bulk tags: %w", err)
	}
	defer tx.Rollback()
	updated := 0
	changed := false
	for _, id := range ids {
		var current string
		err := tx.Get(&current, `SELECT ai_tags FROM media WHERE id = ?`, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("server: read media tags: %w", err)
		}
		before := normalizeMediaTags(current)
		tags := strings.Split(before, ", ")
		if len(tags) == 1 && tags[0] == "" {
			tags = nil
		}
		for _, remove := range req.RemoveTags {
			tags = slices.DeleteFunc(tags, func(t string) bool { return t == remove })
		}
		for _, add := range req.AddTags {
			if !slices.Contains(tags, add) {
				tags = append(tags, add)
			}
		}
		if after := strings.Join(tags, ", "); after != before {
			changed = true
		}
		if _, err := tx.Exec(`UPDATE media SET ai_tags = ? WHERE id = ?`, strings.Join(tags, ", "), id); err != nil {
			return fmt.Errorf("server: update media tags: %w", err)
		}
		updated++
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("server: commit media bulk tags: %w", err)
	}
	// タグが変わった画像があれば、公開サイトのフォトギャラリーへ未反映として記録する。
	dirty := changed && s.markMediaDirty(c, append(slices.Clone(req.AddTags), req.RemoveTags...)...)
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "updated_count": updated, "remote_dirty": dirty})
}

// markMediaDirty は画像の変更が公開サイトのフォトギャラリーに関わるとき（変更の前後の tags のどれかが空でない、
// つまりギャラリーにタグで選ばれうる画像のとき）、公開サーバーへ未反映（remoteDirtyMedia）として記録し、記録したかを返す。
// 公開サーバーに接続していなければ記録しない。新しくアップロードした画像はまだタグが無く、どのギャラリーにも出ないため
// 記録しない（記事の編集画面で画像を貼るたびにバナーが出ないようにする）。
func (s *Server) markMediaDirty(c echo.Context, tags ...string) bool {
	if !slices.ContainsFunc(tags, func(t string) bool { return strings.TrimSpace(t) != "" }) || !s.loadRemoteConfig().connected() {
		return false
	}
	s.markRemoteDirty(c, remoteDirtyMedia)
	return true
}

func validMediaBulkTag(tag string) bool {
	for _, r := range tag {
		if unicode.IsControl(r) || r == '"' || r == ',' || r == '，' || r == '、' {
			return false
		}
	}
	tag = strings.TrimSpace(tag)
	return tag != "" && utf8.RuneCountInString(tag) <= 100
}

func mediaBulkTagsError(c echo.Context, status int, key string) error {
	return c.JSON(status, map[string]string{"message": i18n.Translate(string(language(c)), key)})
}

// mediaRecord は media テーブルの 1 行。保存パスはアップロード先ディレクトリを含む。
type mediaRecord struct {
	ID           string    `db:"id"`
	Filename     string    `db:"filename"`
	PathOriginal string    `db:"path_original"`
	PathLarge    string    `db:"path_large"`
	PathMedium   string    `db:"path_medium"`
	PathSmall    string    `db:"path_small"`
	Width        int       `db:"width"`
	Height       int       `db:"height"`
	MimeType     string    `db:"mime_type"`
	Alt          string    `db:"alt"`
	Caption      string    `db:"caption"`
	AITags       string    `db:"ai_tags"`
	UploaderName string    `db:"uploader_name"`
	CreatedAt    time.Time `db:"created_at"`
}

// fileNames は記録に紐づく保存ファイルの名前（重複・空を除く）を返す。
func (m mediaRecord) fileNames() []string {
	var names []string
	for _, p := range []string{m.PathOriginal, m.PathLarge, m.PathMedium, m.PathSmall} {
		if name, ok := safeUploadName(p); ok && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// mediaItem は管理画面とメディアピッカー API に渡す画像 1 件。URL はサイトルートからのパス。
type mediaItem struct {
	ID           string    `json:"id"`
	Filename     string    `json:"filename"`
	URL          string    `json:"url"`
	ThumbnailURL string    `json:"thumbnail_url"`
	OriginalURL  string    `json:"original_url"`
	LargeURL     string    `json:"large_url"`
	MediumURL    string    `json:"medium_url"`
	SmallURL     string    `json:"small_url"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	MimeType     string    `json:"mime_type"`
	Alt          string    `json:"alt"`
	Caption      string    `json:"caption"`
	AITags       string    `json:"ai_tags"`
	Markdown     string    `json:"markdown"`
	UploaderName string    `json:"uploader_name"`
	CreatedAt    time.Time `json:"created_at"`
	// UsageCount は画像を本文・アイキャッチに使っている記事の数（管理画面の一覧でだけ数える）。
	UsageCount int `json:"usage_count"`
}

func newMediaItem(m mediaRecord) mediaItem {
	original, large, medium, small := mediaURL(m.PathOriginal), mediaURL(m.PathLarge), mediaURL(m.PathMedium), mediaURL(m.PathSmall)
	main := firstNonEmpty(large, original, medium, small)
	return mediaItem{
		ID:           m.ID,
		Filename:     m.Filename,
		URL:          main,
		ThumbnailURL: firstNonEmpty(small, medium, large, original),
		OriginalURL:  original,
		LargeURL:     large,
		MediumURL:    medium,
		SmallURL:     small,
		Width:        m.Width,
		Height:       m.Height,
		MimeType:     m.MimeType,
		Alt:          m.Alt,
		Caption:      m.Caption,
		AITags:       m.AITags,
		Markdown:     mediaMarkdown(m.Alt, m.Filename, m.Caption, main),
		UploaderName: m.UploaderName,
		CreatedAt:    m.CreatedAt,
	}
}

// mediaURL は保存パスを配信 URL に変換する。パスが空ならファイルが無いため空を返す。
func mediaURL(p string) string {
	if _, ok := safeUploadName(p); !ok {
		return ""
	}
	return uploadURL(baseName(p))
}

// safeUploadName は保存パスからアップロード先直下のファイル名を取り出す。
// 空・"."・".." など、アップロード先のファイルを指さない値は ok=false を返す。
func safeUploadName(p string) (string, bool) {
	if strings.TrimSpace(p) == "" {
		return "", false
	}
	name := baseName(p)
	if name == "" || name == "." || name == ".." || name == "/" || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return name, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// markdownTitleReplacer はキャプションから Markdown の画像タイトル（"..."）を壊す文字を除く。
var markdownTitleReplacer = strings.NewReplacer(`"`, "", "\\", "", "\r", "", "\n", " ")

// mediaMarkdown はエディタに挿入する画像構文を返す。代替テキストが空ならファイル名（拡張子なし）を使い、
// キャプションは画像のタイトルとして添える。
func mediaMarkdown(alt, filename, caption, src string) string {
	if src == "" {
		return ""
	}
	alt = strings.TrimSpace(markdownAltReplacer.Replace(alt))
	if alt == "" {
		alt = strings.TrimSpace(markdownAltReplacer.Replace(strings.TrimSuffix(filename, filepath.Ext(filename))))
	}
	if title := strings.TrimSpace(markdownTitleReplacer.Replace(caption)); title != "" {
		return fmt.Sprintf("![%s](%s %q)", alt, src, title)
	}
	return fmt.Sprintf("![%s](%s)", alt, src)
}

// mediaMeta は利用者が編集できる画像のメタデータ。
type mediaMeta struct {
	Alt, Caption, AITags string
}

// readMediaMeta はフォームの alt・caption・ai_tags を整えて返す。長すぎる値は利用者向けのメッセージを返す。
// 代替テキストは Markdown の画像構文に入れるため、構文を壊す文字と改行を除く。
func readMediaMeta(c echo.Context) (mediaMeta, string) {
	m := mediaMeta{
		Alt:     strings.TrimSpace(markdownAltReplacer.Replace(c.FormValue("alt"))),
		Caption: strings.TrimSpace(normalizeNewlines(c.FormValue("caption"))),
		AITags:  normalizeMediaTags(c.FormValue("ai_tags")),
	}
	switch {
	case utf8.RuneCountInString(m.Alt) > maxMediaAltRunes:
		return m, fmt.Sprintf("代替テキストは%d文字以内で入力してください", maxMediaAltRunes)
	case utf8.RuneCountInString(m.Caption) > maxMediaCaptionRunes:
		return m, fmt.Sprintf("キャプションは%d文字以内で入力してください", maxMediaCaptionRunes)
	case utf8.RuneCountInString(m.AITags) > maxMediaTagsRunes:
		return m, fmt.Sprintf("タグは合計%d文字以内で入力してください", maxMediaTagsRunes)
	}
	return m, ""
}

// normalizeMediaTags はカンマ（全角・読点を含む）区切りのタグを、空と重複を除いて ", " で連結する。
func normalizeMediaTags(s string) string {
	s = strings.NewReplacer("，", ",", "、", ",", "\r", ",", "\n", ",").Replace(s)
	var tags []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	return strings.Join(tags, ", ")
}

// mediaQuery は一覧・ピッカー共通の検索条件。
type mediaQuery struct {
	Q   string
	Tag string
	// Untagged はタグの無い画像だけに絞るか（?untagged=1）。Tag とは同時に使わず、Untagged を優先する。
	Untagged bool
	// Sort は並び順（?sort=。mediaSortOrders のキー）。既定の撮影日の新しい順（taken_desc）は空にする。
	Sort    string
	Page    int
	PerPage int
}

// 一覧の並び順。撮影日は撮影日時 taken_at（無ければ登録日時）で、記事から取り込んだ画像は記事の日付が入る。
const (
	mediaSortTakenDesc   = "taken_desc"
	mediaSortTakenAsc    = "taken_asc"
	mediaSortCreatedDesc = "created_desc"
)

// mediaSortOrders は並び順ごとの ORDER BY。クエリの値をそのまま SQL に入れないよう、ここにあるものだけを使う。
// 同じ日時の画像も登録順（の逆）に並ぶよう rowid で並べる。
var mediaSortOrders = map[string]string{
	mediaSortTakenDesc:   "COALESCE(m.taken_at, m.created_at) DESC, m.rowid DESC",
	mediaSortTakenAsc:    "COALESCE(m.taken_at, m.created_at) ASC, m.rowid ASC",
	mediaSortCreatedDesc: "m.created_at DESC, m.rowid DESC",
}

// normalizeMediaSort は ?sort= の値を mediaQuery.Sort の値にする。既定（taken_desc）と知らない値は空。
func normalizeMediaSort(v string) string {
	v = strings.TrimSpace(v)
	if _, ok := mediaSortOrders[v]; !ok || v == mediaSortTakenDesc {
		return ""
	}
	return v
}

// orderBy は並び順の ORDER BY 句の中身を返す。
func (mq mediaQuery) orderBy() string {
	if order, ok := mediaSortOrders[mq.Sort]; ok {
		return order
	}
	return mediaSortOrders[mediaSortTakenDesc]
}

func readMediaQuery(c echo.Context, perPage int) mediaQuery {
	q := mediaQuery{Q: strings.TrimSpace(c.QueryParam("q")), Tag: strings.TrimSpace(c.QueryParam("tag")),
		Sort: normalizeMediaSort(c.QueryParam("sort")), PerPage: perPage}
	if c.QueryParam("untagged") == "1" {
		q.Untagged, q.Tag = true, ""
	}
	if n, err := strconv.Atoi(c.QueryParam("page")); err == nil && n > 0 {
		q.Page = n
	} else {
		q.Page = 1
	}
	return q
}

const mediaSelectColumns = `m.id, m.filename, m.path_original, m.path_large, m.path_medium, m.path_small,
	m.width, m.height, m.mime_type, m.alt, m.caption, m.ai_tags, m.created_at,
	COALESCE(NULLIF(u.display_name, ''), u.username, '') AS uploader_name`

// mediaWhere は検索語（空白区切り）をすべて含む画像に絞る条件を返す。各語はファイル名・代替テキスト・
// キャプション・AI タグのいずれかに含まれればよい。tag が空でなければ、そのタグ（完全一致）の付いた画像に絞る。
func mediaWhere(q, tag string) (string, []any) {
	return mediaQuery{Q: q, Tag: tag}.where()
}

// where は q の検索語・タグ・タグの有無に合う画像に絞る WHERE 句（条件が無ければ空）を返す。
func (mq mediaQuery) where() (string, []any) {
	q, tag := mq.Q, mq.Tag
	terms := strings.Fields(q)
	if len(terms) > maxMediaQueryTerms {
		terms = terms[:maxMediaQueryTerms]
	}
	var conds []string
	var args []any
	for _, t := range terms {
		like := "%" + escapeLike(t) + "%"
		conds = append(conds, `(m.filename LIKE ? ESCAPE '\' OR m.alt LIKE ? ESCAPE '\'
			OR m.caption LIKE ? ESCAPE '\' OR m.ai_tags LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	switch {
	case mq.Untagged:
		conds = append(conds, `TRIM(m.ai_tags) = ''`)
	case tag != "":
		conds = append(conds, mediaTagMatchSQL)
		args = append(args, mediaTagMatchArg(tag))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// mediaTagMatchSQL は media の AI タグに 1 つのタグ（完全一致）が付いているかの条件。引数は mediaTagMatchArg(tag) を 1 つ渡す。
// ai_tags は「, 」区切りに正規化してあり、両端に区切りを足して比べるため、"art" が "cart" に一致しない。
// ai_tags の列は media にしかないため、users などと結合しても修飾は要らない。
const mediaTagMatchSQL = `(',' || replace(ai_tags, ', ', ',') || ',') LIKE ? ESCAPE '\'`

// mediaTagMatchArg は mediaTagMatchSQL に渡す tag の LIKE パターンを返す。
func mediaTagMatchArg(tag string) string {
	return "%," + escapeLike(tag) + ",%"
}

// searchMedia は q の並び順（既定は撮影日の新しい順）で q に合う画像の 1 ページ分と、条件に合う総数を返す。
func (s *Server) searchMedia(q mediaQuery) ([]mediaRecord, int, error) {
	return s.searchMediaRange(context.Background(), q, q.PerPage, (q.Page-1)*q.PerPage)
}

// searchMediaRange は searchMedia の本体。ページ番号ではなく件数 limit と先頭から飛ばす件数 offset で範囲を決める
// （MCP の cms_list_media）。q.Page・q.PerPage は使わない。
func (s *Server) searchMediaRange(ctx context.Context, q mediaQuery, limit, offset int) ([]mediaRecord, int, error) {
	where, args := q.where()
	var total int
	if err := s.db.GetContext(ctx, &total, `SELECT COUNT(*) FROM media m`+where, args...); err != nil {
		return nil, 0, fmt.Errorf("server: count media: %w", err)
	}
	rows := []mediaRecord{}
	err := s.db.SelectContext(ctx, &rows, `SELECT `+mediaSelectColumns+` FROM media m LEFT JOIN users u ON u.id = m.uploaded_by`+
		where+` ORDER BY `+q.orderBy()+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("server: list media: %w", err)
	}
	return rows, total, nil
}

// getMedia は id の画像を返す。無ければ sql.ErrNoRows。
func (s *Server) getMedia(id string) (mediaRecord, error) {
	var m mediaRecord
	err := s.db.Get(&m, `SELECT `+mediaSelectColumns+` FROM media m LEFT JOIN users u ON u.id = m.uploaded_by WHERE m.id = ?`, id)
	return m, err
}

// mediaUsageCond は names のどれかのファイルをアイキャッチで参照している条件（featuredCond）と、本文で参照している条件
// （bodyCond）を返す。args は各条件に 1 回ずつ渡す引数（どちらの条件も同じ並び）。NULL の列は空文字として比べる。
// ファイル名はランダムな ID を含むため、部分一致で参照を見つけられる。
func mediaUsageCond(names []string) (featuredCond, bodyCond string, args []any) {
	featured := make([]string, 0, len(names))
	body := make([]string, 0, len(names))
	args = make([]any, 0, len(names))
	for _, n := range names {
		featured = append(featured, "instr(COALESCE(featured_image_url, ''), ?) > 0")
		body = append(body, "instr(COALESCE(content_markdown, ''), ?) > 0")
		args = append(args, n)
	}
	return "(" + strings.Join(featured, " OR ") + ")", "(" + strings.Join(body, " OR ") + ")", args
}

// mediaUsageCount は m のファイルを本文またはアイキャッチで参照している記事の数を返す。
// 詳細の「使用中の記事」（mediaUsageArticles）と数が合うよう、ゴミ箱（status = 'deleted'）の記事は数えない。
func (s *Server) mediaUsageCount(m mediaRecord) (int, error) {
	names := m.fileNames()
	if len(names) == 0 {
		return 0, nil
	}
	featured, body, args := mediaUsageCond(names)
	var n int
	err := s.db.Get(&n, `SELECT COUNT(*) FROM articles WHERE (`+featured+` OR `+body+`) AND status != 'deleted'`,
		append(slices.Clone(args), args...)...)
	return n, err
}

// mediaUsageCounts は records の画像ごとに、本文またはアイキャッチで参照している記事の数を ID をキーにして返す
// （mediaUsageCount と同じ数え方）。一覧の 1 ページ分を、画像ごとに記事を検索し直さず、ゴミ箱以外の記事を 1 回だけ読んで数える。
func (s *Server) mediaUsageCounts(records []mediaRecord) (map[string]int, error) {
	counts := make(map[string]int, len(records))
	names := make([][]string, len(records))
	hasNames := false
	for i, r := range records {
		counts[r.ID] = 0
		names[i] = r.fileNames()
		hasNames = hasNames || len(names[i]) > 0
	}
	if !hasNames {
		return counts, nil
	}
	rows, err := s.db.Query(`SELECT COALESCE(featured_image_url, ''), COALESCE(content_markdown, '')
		FROM articles WHERE status != 'deleted'`)
	if err != nil {
		return nil, fmt.Errorf("server: media usage counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var featured, body string
		if err := rows.Scan(&featured, &body); err != nil {
			return nil, fmt.Errorf("server: media usage counts: %w", err)
		}
		for i, r := range records {
			if slices.ContainsFunc(names[i], func(n string) bool {
				return strings.Contains(featured, n) || strings.Contains(body, n)
			}) {
				counts[r.ID]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("server: media usage counts: %w", err)
	}
	return counts, nil
}

// mediaLibraryData は media.html に渡すデータ。
type mediaLibraryData struct {
	Items      []mediaItem
	Query      string
	Total      int
	Page       int
	TotalPages int
	PrevURL    string
	NextURL    string
	// CanUpload はアップロードできるか（執筆者・編集者）。CanDelete は削除できるか（管理者）。
	CanUpload bool
	CanDelete bool
	// AIAnalyze は画像の解析に対応した AI が設定済みか。false なら自動生成ボタンの横に設定の案内を出す。
	AIAnalyze bool
	// Error は直前の操作の失敗を伝えるメッセージ。
	Error string
	// Tag はタグで絞り込んでいるときのタグ（?tag=。完全一致）。Untagged はタグの無い画像に絞っているか（?untagged=1）。
	Tag      string
	Untagged bool
	// Sort は今の並び順（mediaSortOrders のキー。既定は taken_desc）。
	Sort string
	// FirstURL は今の条件のまま 1 ページ目に戻る URL。
	FirstURL string
	// タグの絞り込み（タグクラウド）。TagCounts は枚数の多い順に mediaTagCloudLimit 個まで、残りは TagCountsMore に入れ、
	// 選んでいるタグが TagCountsMore にあれば TagCountsMoreOpen で最初から開く。AllURL は絞り込みを外す URL、
	// UntaggedURL はタグの無い画像に絞る URL、UntaggedCount はタグの無い画像の数。どの URL も検索語は残す。
	TagCounts         []mediaTagChip
	TagCountsMore     []mediaTagChip
	TagCountsMoreOpen bool
	AllURL            string
	UntaggedURL       string
	UntaggedCount     int
}

// mediaTagCount はタグ 1 つと、そのタグの付いた画像の数。
type mediaTagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// mediaTagChip はメディアライブラリのタグの絞り込みに並べるタグのリンク 1 つ。
type mediaTagChip struct {
	Tag   string
	Count int
	// Level は 1〜3 の表示の強さ。画像の数が多いタグほど大きく・濃く表示する。
	Level int
	// URL はこのタグで絞り込む一覧の URL（検索語は残す）。Current は今このタグで絞り込んでいるか。
	URL     string
	Current bool
}

// mediaTagStats は全画像のタグごとの枚数（タグ名の順）と、タグの無い画像の数を返す。
func (s *Server) mediaTagStats() ([]mediaTagCount, int, error) {
	var values []string
	if err := s.db.Select(&values, `SELECT ai_tags FROM media WHERE TRIM(ai_tags) <> ''`); err != nil {
		return nil, 0, fmt.Errorf("server: list media tags: %w", err)
	}
	counts := make(map[string]int)
	for _, value := range values {
		var seen []string // 1 枚の画像に同じタグが重なっていても 1 枚と数える。
		for _, tag := range strings.Split(value, ",") {
			if tag = strings.TrimSpace(tag); tag != "" && !slices.Contains(seen, tag) {
				seen = append(seen, tag)
				counts[tag]++
			}
		}
	}
	list := make([]mediaTagCount, 0, len(counts))
	for tag, n := range counts {
		list = append(list, mediaTagCount{Tag: tag, Count: n})
	}
	slices.SortFunc(list, func(a, b mediaTagCount) int { return strings.Compare(a.Tag, b.Tag) })
	var untagged int
	if err := s.db.Get(&untagged, `SELECT COUNT(*) FROM media WHERE TRIM(ai_tags) = ''`); err != nil {
		return nil, 0, fmt.Errorf("server: count untagged media: %w", err)
	}
	return list, untagged, nil
}

// sortMediaTagsByUse は counts を枚数の多い順（同じ枚数ならタグ名の順）に並べ替えた複製を返す。
func sortMediaTagsByUse(counts []mediaTagCount) []mediaTagCount {
	sorted := slices.Clone(counts)
	slices.SortStableFunc(sorted, func(a, b mediaTagCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.Tag, b.Tag))
	})
	return sorted
}

// mediaTagChips はタグの絞り込みに並べるリンクを、枚数の多い順に作る。表示の強さ（Level）は、いちばん少ないタグから
// いちばん多いタグまでを 3 段階に分けて決める（どのタグも同じ枚数なら中間の 2）。
func mediaTagChips(counts []mediaTagCount, q mediaQuery) []mediaTagChip {
	sorted := sortMediaTagsByUse(counts)
	if len(sorted) == 0 {
		return nil
	}
	most, least := sorted[0].Count, sorted[len(sorted)-1].Count
	chips := make([]mediaTagChip, 0, len(sorted))
	for _, tc := range sorted {
		level := 2
		if most > least {
			level = 1 + min(2, (tc.Count-least)*3/(most-least))
		}
		chips = append(chips, mediaTagChip{Tag: tc.Tag, Count: tc.Count, Level: level,
			URL: q.listURL(tc.Tag, false, 1), Current: !q.Untagged && q.Tag == tc.Tag})
	}
	return chips
}

// handleMediaLibrary はメディアライブラリの一覧（新しい順・検索・ページ送り）を返す。
func (s *Server) handleMediaLibrary(c echo.Context) error {
	q := readMediaQuery(c, mediaPerPage)
	rows, total, err := s.searchMedia(q)
	if err != nil {
		return err
	}
	tagCounts, untaggedCount, err := s.mediaTagStats()
	if err != nil {
		return err
	}
	user := currentUser(c)
	data := mediaLibraryData{
		Items:         make([]mediaItem, 0, len(rows)),
		Query:         q.Q,
		Total:         total,
		Page:          q.Page,
		TotalPages:    (total + mediaPerPage - 1) / mediaPerPage,
		CanUpload:     user.HasRole(auth.RoleEditor, auth.RoleWriter),
		CanDelete:     user.HasRole(auth.RoleAdmin),
		AIAnalyze:     s.canAnalyzeMedia(),
		Error:         mediaErrorMessages[c.QueryParam("error")],
		Tag:           q.Tag,
		Untagged:      q.Untagged,
		Sort:          cmp.Or(q.Sort, mediaSortTakenDesc),
		FirstURL:      q.listURL(q.Tag, q.Untagged, 1),
		AllURL:        q.listURL("", false, 1),
		UntaggedURL:   q.listURL("", true, 1),
		UntaggedCount: untaggedCount,
	}
	chips := mediaTagChips(tagCounts, q)
	if len(chips) > mediaTagCloudLimit {
		data.TagCountsMore = chips[mediaTagCloudLimit:]
		chips = chips[:mediaTagCloudLimit]
		data.TagCountsMoreOpen = slices.ContainsFunc(data.TagCountsMore, func(c mediaTagChip) bool { return c.Current })
	}
	data.TagCounts = chips
	usage, err := s.mediaUsageCounts(rows)
	if err != nil {
		return err
	}
	for _, r := range rows {
		item := newMediaItem(r)
		item.UsageCount = usage[r.ID]
		data.Items = append(data.Items, item)
	}
	if q.Page > 1 {
		data.PrevURL = q.listURL(q.Tag, q.Untagged, q.Page-1)
	}
	if q.Page*mediaPerPage < total {
		data.NextURL = q.listURL(q.Tag, q.Untagged, q.Page+1)
	}
	return s.render(c, http.StatusOK, "media", pageTitle(c, "media_library_title"), data)
}

// mediaErrorMessages はリダイレクト先の一覧で表示するエラー。クエリの任意文字列を画面に出さないよう、キーだけを受け付ける。
var mediaErrorMessages = map[string]string{
	"not_found": "画像が見つかりません。すでに削除された可能性があります",
	"invalid":   "入力内容を確認してください（代替テキスト・キャプション・タグが長すぎます）",
}

// mediaPageURL はメディアライブラリの一覧の URL を返す。untagged ならタグの無い画像に絞り、tag は使わない。
func mediaPageURL(q, tag string, untagged bool, page int) string {
	return mediaQuery{Q: q}.listURL(tag, untagged, page)
}

// listURL は mq の検索語と並び順を残したまま、tag・untagged で絞った page ページ目の一覧の URL を返す。
// untagged ならタグの無い画像に絞り、tag は使わない。
func (mq mediaQuery) listURL(tag string, untagged bool, page int) string {
	v := url.Values{}
	if mq.Q != "" {
		v.Set("q", mq.Q)
	}
	if mq.Sort != "" {
		v.Set("sort", mq.Sort)
	}
	if untagged {
		v.Set("untagged", "1")
	} else if tag != "" {
		v.Set("tag", tag)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return "/admin/media"
	}
	return "/admin/media?" + v.Encode()
}

// mediaListResponse はメディアピッカー API の JSON レスポンス。
type mediaListResponse struct {
	Items   []mediaItem `json:"items"`
	Page    int         `json:"page"`
	PerPage int         `json:"per_page"`
	Total   int         `json:"total"`
	HasNext bool        `json:"has_next"`
}

// handleMediaAPI はメディアピッカー用に、検索語に合う画像を新しい順に JSON で返す。
// per_page で 1 回の件数（既定 24、最大 60）を変えられる。
func (s *Server) handleMediaAPI(c echo.Context) error {
	perPage := mediaPerPage
	if n, err := strconv.Atoi(c.QueryParam("per_page")); err == nil && n > 0 {
		perPage = min(n, mediaAPIMaxPerPage)
	}
	q := readMediaQuery(c, perPage)
	rows, total, err := s.searchMedia(q)
	if err != nil {
		return err
	}
	res := mediaListResponse{Items: make([]mediaItem, 0, len(rows)), Page: q.Page, PerPage: perPage, Total: total,
		HasNext: q.Page*perPage < total}
	for _, r := range rows {
		res.Items = append(res.Items, newMediaItem(r))
	}
	return c.JSON(http.StatusOK, res)
}

// mediaTagsResponse はタグ一覧 API の応答。Tags はタグ名の順の一覧（以前からの形）、Counts は同じ順のタグごとの
// 画像の数、Popular は同じタグを画像の多い順（同じ数ならタグ名の順）に並べたもの、Untagged はタグの無い画像の数。
// Popular は画像の詳細の「よく使うタグ」に使う。以前からの Tags・Counts は変えない（古い画面・スクリプトのため）。
type mediaTagsResponse struct {
	Tags     []string        `json:"tags"`
	Counts   []mediaTagCount `json:"counts"`
	Popular  []mediaTagCount `json:"popular"`
	Untagged int             `json:"untagged"`
}

// handleMediaTagsAPI は全画像で使っているタグ（重複なし・タグ名の順）と、タグごとの画像の数（タグ名の順・多い順）・
// タグの無い画像の数を返す。
func (s *Server) handleMediaTagsAPI(c echo.Context) error {
	counts, untagged, err := s.mediaTagStats()
	if err != nil {
		return err
	}
	tags := make([]string, 0, len(counts))
	for _, tc := range counts {
		tags = append(tags, tc.Tag)
	}
	popular := sortMediaTagsByUse(counts)
	if popular == nil {
		popular = []mediaTagCount{}
	}
	if counts == nil {
		counts = []mediaTagCount{}
	}
	return c.JSON(http.StatusOK, mediaTagsResponse{Tags: tags, Counts: counts, Popular: popular, Untagged: untagged})
}

// mediaHintTags は AI の画像解析に渡す既存のタグ（よく使う順に ai.MaxImageHintTags 個まで）を返す。
func (s *Server) mediaHintTags() ([]string, error) {
	counts, _, err := s.mediaTagStats()
	if err != nil {
		return nil, err
	}
	sorted := sortMediaTagsByUse(counts)
	tags := make([]string, 0, min(len(sorted), ai.MaxImageHintTags))
	for _, tc := range sorted[:min(len(sorted), ai.MaxImageHintTags)] {
		tags = append(tags, tc.Tag)
	}
	return tags, nil
}

// galleryResolver looks up current image metadata by an exact normalized tag.
func (s *Server) galleryResolver() markdown.GalleryResolver {
	return func(tag string) ([]markdown.GalleryImage, error) {
		return s.resolveGallery(markdown.GalleryQuery{Tag: tag, Sort: "latest"})
	}
}

func (s *Server) galleryQueryResolver() markdown.GalleryQueryResolver {
	return s.resolveGallery
}

func (s *Server) resolveGallery(query markdown.GalleryQuery) ([]markdown.GalleryImage, error) {
	page, err := s.resolveGalleryPage(query, false)
	return page.Images, err
}

// galleryPageResolver は総数付きでタグのギャラリーの写真の一部を返す（:::gallery の「もっと見る」と続きの API）。
func (s *Server) galleryPageResolver() markdown.GalleryPageResolver {
	return func(query markdown.GalleryQuery) (markdown.GalleryPage, error) {
		return s.resolveGalleryPage(query, true)
	}
}

// resolveGalleryPage はタグ query.Tag の写真を、並び順 query.Sort で Offset 枚目から Limit 枚（0 ならすべて。MaxGalleryLimit
// を超える数は上限）返す。MinWidth が正なら元画像の幅がそれ以上の写真だけ、Year が空でなければその年の写真だけにする。withTotal なら条件に合う写真の総数も数える。
// 並び順 random は Seed が正ならそれで決まる並べ替え（続きを読んでも重ならない）、0 なら毎回ばらばら。
func (s *Server) resolveGalleryPage(query markdown.GalleryQuery, withTotal bool) (markdown.GalleryPage, error) {
	query.Limit = min(query.Limit, markdown.MaxGalleryLimit)
	where, args := galleryTagWhere(query)
	var page markdown.GalleryPage
	if withTotal {
		if err := s.db.Get(&page.Total, `SELECT COUNT(*) FROM media WHERE `+where, args...); err != nil {
			return page, fmt.Errorf("server: count gallery tag: %w", err)
		}
	}
	order := "created_at DESC, rowid DESC"
	switch query.Sort {
	case "oldest":
		order = "created_at ASC, rowid ASC"
	case "random":
		order = "RANDOM()"
		if markdown.ValidGallerySeed(query.Seed) {
			order = markdown.GallerySeedOrder("rowid", query.Seed) + ", rowid"
		}
	}
	stmt := `SELECT id, filename, path_original, path_large, path_medium, path_small,
			width, height, mime_type, alt, caption, ai_tags, created_at FROM media
			WHERE ` + where + ` ORDER BY ` + order
	if query.Limit > 0 {
		stmt += " LIMIT ?"
		args = append(args, query.Limit)
		if query.Offset > 0 {
			stmt += " OFFSET ?"
			args = append(args, query.Offset)
		}
	} else if query.Offset > 0 {
		stmt += " LIMIT -1 OFFSET ?"
		args = append(args, query.Offset)
	}
	var rows []mediaRecord
	if err := s.db.Select(&rows, stmt, args...); err != nil {
		return page, fmt.Errorf("server: resolve gallery tag: %w", err)
	}
	page.Images = make([]markdown.GalleryImage, 0, len(rows))
	for _, row := range rows {
		if img, ok := newGalleryImage(row); ok {
			page.Images = append(page.Images, img)
		}
	}
	if !withTotal {
		page.Total = len(page.Images)
	}
	return page, nil
}

// newGalleryImage はメディアの記録をフォトギャラリーの写真1枚にする。一覧には軽い -800（無ければ -400）を、
// 拡大表示には -1200（無ければ原寸）を使い、どちらも無いサイズは URL（今までの表示の画像）で埋める。
// 配信できる画像のファイルが1つも無ければ ok=false。
func newGalleryImage(row mediaRecord) (markdown.GalleryImage, bool) {
	main := mediaURL(firstNonEmpty(row.PathLarge, row.PathOriginal, row.PathMedium, row.PathSmall))
	if main == "" {
		return markdown.GalleryImage{}, false
	}
	img := markdown.GalleryImage{
		URL:      main,
		Alt:      row.Alt,
		Caption:  row.Caption,
		ThumbURL: cmp.Or(mediaURL(row.PathMedium), mediaURL(row.PathSmall), main),
		FullURL:  cmp.Or(mediaURL(row.PathLarge), mediaURL(row.PathOriginal), main),
		Years:    markdown.GalleryYearList(row.AITags),
	}
	if row.Width > 0 && row.Height > 0 {
		img.Width, img.Height = row.Width, row.Height
	}
	return img, true
}

// acceptsJSON は fetch からの要求（Accept: application/json）かを返す。フォーム送信ならリダイレクトで応える。
func acceptsJSON(c echo.Context) bool {
	return strings.Contains(c.Request().Header.Get(echo.HeaderAccept), echo.MIMEApplicationJSON)
}

// handleMediaUpdate は画像の代替テキスト・キャプション・AI タグを保存する。
func (s *Server) handleMediaUpdate(c echo.Context) error {
	id := c.Param("id")
	meta, msg := readMediaMeta(c)
	if msg != "" {
		if acceptsJSON(c) {
			return echo.NewHTTPError(http.StatusBadRequest, msg)
		}
		return c.Redirect(http.StatusSeeOther, "/admin/media?error=invalid")
	}
	// 変更の前の値は、フォトギャラリーに関わる変更か（未反映として記録するか）を決めるために読む。
	var before mediaMeta
	err := s.db.QueryRow(`SELECT alt, caption, ai_tags FROM media WHERE id = ?`, id).Scan(&before.Alt, &before.Caption, &before.AITags)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("server: read media: %w", err)
	}
	res, err := s.db.Exec(`UPDATE media SET alt = ?, caption = ?, ai_tags = ? WHERE id = ?`, meta.Alt, meta.Caption, meta.AITags, id)
	if err != nil {
		return fmt.Errorf("server: update media: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if acceptsJSON(c) {
			return echo.NewHTTPError(http.StatusNotFound, mediaErrorMessages["not_found"])
		}
		return c.Redirect(http.StatusSeeOther, "/admin/media?error=not_found")
	}
	dirty := before != meta && s.markMediaDirty(c, before.AITags, meta.AITags)
	if !acceptsJSON(c) {
		return c.Redirect(http.StatusSeeOther, "/admin/media?flash=media_saved")
	}
	m, err := s.getMedia(id)
	if err != nil {
		return err
	}
	item := newMediaItem(m)
	if item.UsageCount, err = s.mediaUsageCount(m); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, struct {
		mediaItem
		RemoteDirty bool `json:"remote_dirty,omitempty"`
	}{mediaItem: item, RemoteDirty: dirty})
}

// handleMediaDelete は画像の記録を消し、アップロード先の画像ファイル（原寸・各サイズ）を削除する。
// 消すファイルは記録のファイル名をアップロード先直下に限って解決するため、記録の値でほかの場所は消さない。
// 記事で使用中の画像も削除できる（画面の確認ダイアログで使用中の記事数を警告する）。
func (s *Server) handleMediaDelete(c echo.Context) error {
	id := c.Param("id")
	m, err := s.getMedia(id)
	if errors.Is(err, sql.ErrNoRows) {
		if acceptsJSON(c) {
			return echo.NewHTTPError(http.StatusNotFound, mediaErrorMessages["not_found"])
		}
		return c.Redirect(http.StatusSeeOther, "/admin/media?error=not_found")
	} else if err != nil {
		return err
	}
	// 記録を先に消し、以後は公開ページが消えたファイルを参照しないようにする。
	if _, err := s.db.Exec(`DELETE FROM media WHERE id = ?`, id); err != nil {
		return fmt.Errorf("server: delete media: %w", err)
	}
	for _, name := range m.fileNames() {
		if s.mediaFileShared(name) {
			continue
		}
		if err := os.Remove(filepath.Join(s.cfg.UploadDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.Logger().Errorf("server: remove media file %s: %v", name, err)
		}
	}
	// タグの付いた画像を消したら、公開サイトのギャラリーからも外す必要がある（反映でそのタグを外す）。
	dirty := s.markMediaDirty(c, m.AITags)
	if acceptsJSON(c) {
		return c.JSON(http.StatusOK, map[string]any{"deleted": id, "remote_dirty": dirty})
	}
	return c.Redirect(http.StatusSeeOther, "/admin/media?flash=media_deleted")
}

// mediaFileShared はほかの画像の記録も name のファイルを使っているかを返す（取り込みで同じファイルを
// 共有した場合に、残る記録の画像を壊さない）。判定できないときは安全側に倒して使用中とみなす。
func (s *Server) mediaFileShared(name string) bool {
	like := "%" + escapeLike(name)
	var rows []mediaRecord
	if err := s.db.Select(&rows, `SELECT id, path_original, path_large, path_medium, path_small FROM media
		WHERE path_original LIKE ? ESCAPE '\' OR path_large LIKE ? ESCAPE '\'
			OR path_medium LIKE ? ESCAPE '\' OR path_small LIKE ? ESCAPE '\'`, like, like, like, like); err != nil {
		return true
	}
	for _, r := range rows {
		if slices.Contains(r.fileNames(), name) {
			return true
		}
	}
	return false
}

// handleMediaAnalyze は画像を AI に見せて代替テキスト・キャプション・タグを作る。空欄は保存し、入力済みの説明は
// AI 案を応答に含めるだけにして、利用者が選んで保存できるようにする。タグは今のタグ（トップページのギャラリー用など）を
// すべて残し、AI のタグのうち新しいものだけを後ろに足す（mergeMediaTags）。AI にはサイトでよく使うタグを渡し、
// 当てはまるものはその表記で使わせる（表記ゆれでタグが散らばらないようにする）。
// AI に送るのは大きいサイズ（無ければ中・原寸・小）の保存ファイルで、記録のファイル名をアップロード先直下に限って読む。
func (s *Server) handleMediaAnalyze(c echo.Context) error {
	provider := s.aiProvider()
	analyzer, ok := provider.(ai.MultimodalAnalyzer)
	switch {
	case !ai.IsConfigured(provider):
		return echo.NewHTTPError(http.StatusServiceUnavailable, s.mediaAISetupMessage())
	case !ok:
		return echo.NewHTTPError(http.StatusNotImplemented,
			"現在のAI（"+provider.Name()+"）は画像の解析に対応していません。画像の自動生成には Gemini を設定してください")
	}
	id := c.Param("id")
	m, err := s.getMedia(id)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, mediaErrorMessages["not_found"])
	} else if err != nil {
		return err
	}
	data, err := s.readMediaForAnalysis(m)
	if err != nil {
		c.Logger().Warnf("server: read media %s for analysis: %v", id, err)
		return echo.NewHTTPError(http.StatusNotFound, "画像ファイルが見つからないため自動生成できません")
	}

	hints, err := s.mediaHintTags()
	if err != nil {
		// 候補が読めなくても解析はできる（候補なしで AI にタグを作らせる）。
		c.Logger().Warnf("server: read media tags for analysis: %v", err)
	}
	res, err := analyzer.AnalyzeImage(c.Request().Context(), data, http.DetectContentType(data), hints)
	switch {
	case errors.Is(err, ai.ErrNotConfigured):
		return echo.NewHTTPError(http.StatusServiceUnavailable, s.mediaAISetupMessage())
	case errors.Is(err, ai.ErrInvalidImage):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "この画像はAIで解析できない形式です")
	case err != nil:
		return s.aiError(c, err)
	}

	meta := analysisMeta(res)
	// AI の応答を待つ間に利用者が入力を保存していた場合も、その値を残す。
	m, err = s.getMedia(id)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, mediaErrorMessages["not_found"])
	} else if err != nil {
		return err
	}
	suggestedAlt, suggestedCaption := "", ""
	alt, caption := meta.Alt, meta.Caption
	if strings.TrimSpace(m.Alt) != "" {
		alt = m.Alt
		if meta.Alt != m.Alt {
			suggestedAlt = meta.Alt
		}
	}
	if strings.TrimSpace(m.Caption) != "" {
		caption = m.Caption
		if meta.Caption != m.Caption {
			suggestedCaption = meta.Caption
		}
	}
	tags := mergeMediaTags(m.AITags, meta.AITags)
	if _, err := s.db.Exec(`UPDATE media SET alt = ?, caption = ?, ai_tags = ? WHERE id = ?`, alt, caption, tags, id); err != nil {
		return fmt.Errorf("server: update media: %w", err)
	}
	before := mediaMeta{Alt: m.Alt, Caption: m.Caption, AITags: m.AITags}
	dirty := before != (mediaMeta{Alt: alt, Caption: caption, AITags: tags}) && s.markMediaDirty(c, m.AITags, tags)
	if m, err = s.getMedia(id); errors.Is(err, sql.ErrNoRows) {
		// AI の応答を待つ間に削除された。
		return echo.NewHTTPError(http.StatusNotFound, mediaErrorMessages["not_found"])
	} else if err != nil {
		return err
	}
	item := newMediaItem(m)
	if item.UsageCount, err = s.mediaUsageCount(m); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, struct {
		mediaItem
		SuggestedAlt     string `json:"suggested_alt,omitempty"`
		SuggestedCaption string `json:"suggested_caption,omitempty"`
		RemoteDirty      bool   `json:"remote_dirty,omitempty"`
	}{mediaItem: item, SuggestedAlt: suggestedAlt, SuggestedCaption: suggestedCaption, RemoteDirty: dirty})
}

// canAnalyzeMedia は設定中の AI で画像を解析できるかを返す。
func (s *Server) canAnalyzeMedia() bool {
	provider := s.aiProvider()
	_, ok := provider.(ai.MultimodalAnalyzer)
	return ok && ai.IsConfigured(provider)
}

// mediaAISetupMessage は AI が未設定のときにメディアライブラリのトーストで出す案内（Markdown の記号は除く）。
func (s *Server) mediaAISetupMessage() string {
	return "AIが設定されていないため自動生成できません。" + strings.ReplaceAll(s.aiSetupGuide(), "`", "")
}

// readMediaForAnalysis は m の保存ファイルのうち、AI に送る 1 枚を読み込む。上限を超えるファイルは次の候補に回す。
func (s *Server) readMediaForAnalysis(m mediaRecord) ([]byte, error) {
	err := os.ErrNotExist
	for _, p := range []string{m.PathLarge, m.PathMedium, m.PathOriginal, m.PathSmall} {
		name, ok := safeUploadName(p)
		if !ok {
			continue
		}
		path := filepath.Join(s.cfg.UploadDir, name)
		var info os.FileInfo
		if info, err = os.Stat(path); err != nil {
			continue
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxAnalyzeImageBytes {
			err = fmt.Errorf("%s: unusable file (%d bytes)", name, info.Size())
			continue
		}
		var data []byte
		if data, err = os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return nil, err
}

// analysisMeta は AI の解析結果を、手入力と同じ規則（readMediaMeta）で保存できる値に整える。長すぎる値は切り詰める。
func analysisMeta(res *ai.ImageAnalysis) mediaMeta {
	return mediaMeta{
		Alt:     truncateMediaRunes(strings.TrimSpace(markdownAltReplacer.Replace(res.Alt)), maxMediaAltRunes),
		Caption: truncateMediaRunes(strings.TrimSpace(normalizeNewlines(res.Caption)), maxMediaCaptionRunes),
		AITags:  joinMediaTags(res.Tags, maxMediaTagsRunes),
	}
}

func truncateMediaRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:max]))
}

// mergeMediaTags は今のタグ（existing）を順番どおりすべて残し、added のタグのうち付いていないものを後ろに足して、
// normalizeMediaTags の形で返す。足すと合計が maxMediaTagsRunes 文字を超えるタグは捨てる（今のタグは削らない）。
func mergeMediaTags(existing, added string) string {
	kept := splitMediaTags(normalizeMediaTags(existing))
	for _, t := range splitMediaTags(normalizeMediaTags(added)) {
		if slices.Contains(kept, t) || utf8.RuneCountInString(strings.Join(append(slices.Clip(kept), t), ", ")) > maxMediaTagsRunes {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, ", ")
}

// splitMediaTags は normalizeMediaTags の形（", " 区切り）のタグを分ける。空なら nil。
func splitMediaTags(normalized string) []string {
	if normalized == "" {
		return nil
	}
	return strings.Split(normalized, ", ")
}

// joinMediaTags はタグを normalizeMediaTags の形で連結する。合計が max 文字を超えるタグは途中で切らずに捨てる。
func joinMediaTags(tags []string, max int) string {
	var kept []string
	for _, t := range strings.Split(normalizeMediaTags(strings.Join(tags, ",")), ", ") {
		if t == "" || utf8.RuneCountInString(strings.Join(append(kept, t), ", ")) > max {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, ", ")
}
