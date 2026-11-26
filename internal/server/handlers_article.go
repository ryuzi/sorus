package server

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/lock"
	"sorus/internal/markdown"
	"sorus/internal/scheduler"
)

// articlesPerPage は記事一覧の1ページあたりの件数。
const articlesPerPage = 20

// listTabs は記事一覧のタブの並び。予約投稿（article.StatusScheduled）は保存する状態ではなく、公開日時が未来の
// 公開記事のタブ（article.StatusCondition）。
var listTabs = []article.Status{article.StatusDraft, article.StatusReview, article.StatusScheduled,
	article.StatusPublished, article.StatusDeleted}

// listKinds は記事一覧の「記事 / ページ」の切り替えの並び。
var listKinds = []article.Kind{article.KindPost, article.KindPage}

// 記事フォームの action（押されたボタン）。
const (
	// actionDraft は下書きとして保存する（公開中・予約中の記事は下書きに戻す）。
	actionDraft = "draft"
	// actionSave は状態を変えずに保存する（校閲待ち・予約投稿のまま本文を直す）。
	actionSave = "save"
	// actionReview は保存して校閲を申請する。
	actionReview = "review"
	// actionPublish は保存して公開する。編集者・管理者のみ。
	actionPublish = "publish"
)

// doiPattern は DOI（10.<登録者コード>/<接尾辞>）。
var doiPattern = regexp.MustCompile(`^10\.\d{4,9}/\S+$`)

// maxMetaRunes は学術メタデータの各項目の最大文字数。
const maxMetaRunes = 500

type category struct {
	ID   int64  `db:"id"`
	Name string `db:"name"`
}

// articleRow は記事一覧の1行。
type articleRow struct {
	ID    string `db:"id"`
	Title string `db:"title"`
	Slug  string `db:"slug"`
	// Status は画面に出す状態（article.ViewStatus）。予約中の記事は scheduled。
	Status     article.Status `db:"status"`
	AuthorName string         `db:"author_name"`
	UpdatedAt  time.Time      `db:"updated_at"`
	// PublishedAt は公開日時。予約投稿では公開予定日時。
	PublishedAt *time.Time `db:"published_at"`
	// LockedBy は有効なロックの保持者の表示名。ロックが無ければ空。
	LockedBy string `db:"locked_by_name"`
	// TotalViews は公開ページの累計 PV（日次集計の合計。直近10秒以内の閲覧はまだ含まない）。
	TotalViews int64 `db:"total_views"`
	// Kind は記事の種類。公開ページの確認リンク（/articles/<slug> か /<slug>）に使う。
	Kind article.Kind `db:"kind"`
	// 多言語ステータス
	Lang                  string `db:"lang"`
	TranslationGroupID    string `db:"translation_group_id"`
	SourceArticleID       string `db:"source_article_id"`
	IsOutdated            bool   `db:"is_outdated"`
	HasEnTranslation      bool   `db:"-"`
	EnArticleID           string `db:"-"`
	EnTranslationOutdated bool   `db:"-"`
	// DatePrimary・DateNote は「日付」の列に出す文（setRowDates）。公開中は公開日時と、公開後に日をまたいで直したときの
	// 更新日（DateNote）、予約中は公開予定日時、下書き・校閲待ち・ゴミ箱は更新日時。
	DatePrimary string `db:"-"`
	DateNote    string `db:"-"`
}

type articleViewData struct {
	Article     *article.Article
	AuthorName  string
	Status      article.Status
	ContentHTML template.HTML
	CanEdit     bool
	PublicURL   string
}

type listTab struct {
	Status article.Status
	Label  string
	Count  int
	Active bool
}

// kindTab は記事一覧の「記事 / ページ」の切り替えの1つ。Count は表示中の状態のタブの件数。
type kindTab struct {
	Kind   article.Kind
	Count  int
	Active bool
}

// listData は article_list.html に渡すデータ。
type listData struct {
	// Kind は表示中の種類（記事 / ページ）。
	Kind     article.Kind
	KindTabs []kindTab
	Status   article.Status
	Tabs     []listTab
	Rows     []articleRow
	Page     int
	PrevPage int
	NextPage int
	// CanPublish は公開中・予約中の記事もゴミ箱へ移せる（編集者・管理者である）か。
	CanPublish bool
	// Lang は言語フィルター（"" = すべて, "ja", "en"）。
	Lang string
	// Query は検索クエリ（タイトル・スラッグの絞り込み）。
	Query string
	// MultilingualEnabled は多言語対応を有効にしているか。OFF なら言語の絞り込み・言語バッジを出さない。
	MultilingualEnabled bool
	// DeletedTitle は「ゴミ箱へ」で移したばかりの記事のタイトル（?deleted=<記事 ID>）。空ならお知らせを出さない。
	// クエリの任意の文字列を画面に出さないよう、ID で引いてゴミ箱にある記事のときだけ入れる。
	DeletedTitle string
	// TrashTotal はゴミ箱の記事とページの合計（「ゴミ箱を空にする」の対象）。ゴミ箱のタブでだけ数える。
	TrashTotal int
	// TrashNotice はゴミ箱の操作の結果を伝える文（?trash=restored|purged|emptied）。無ければ空。
	TrashNotice string
	// Sort は並び順（listSorts のどれか）。空ならタブごとの既定の並び順。
	Sort string
	// Sorts は並び順の選択肢。
	Sorts []sortOption
}

// sortOption は記事一覧の並び順の選択肢の1つ。
type sortOption struct {
	Value  string
	Label  string
	Active bool
}

// sortQuery は記事一覧の URL に付ける並び順のクエリ（既定の並び順なら空）。テンプレートのリンクは html/template が
// クエリをエスケープするため、"{{if .Data.Sort}}&sort=…{{end}}" と書く。
func sortQuery(sort string) string {
	if sort == "" {
		return ""
	}
	return "&sort=" + sort
}

// listSorts は記事一覧で選べる並び順（?sort=）。"" はタブごとの既定（公開中は公開日の新しい順、予約中は公開予定の近い順、
// ほかは更新日時の新しい順）。
var listSorts = []string{"", "published_desc", "updated_desc", "updated_asc", "title_asc"}

// validSort は sort が選べる並び順ならそのまま、そうでなければ既定（""）を返す。
func validSort(sort string) string {
	if slices.Contains(listSorts, sort) {
		return sort
	}
	return ""
}

// listOrder は記事一覧の ORDER BY 句。既定の公開中の並びは公開サイトの記事一覧（公開日の新しい順）と同じにする。
func listOrder(status article.Status, sort string) string {
	switch sort {
	case "published_desc":
		return `julianday(COALESCE(a.published_at, a.updated_at)) DESC, a.id`
	case "updated_desc":
		return `julianday(a.updated_at) DESC, a.id`
	case "updated_asc":
		return `julianday(a.updated_at) ASC, a.id`
	case "title_asc":
		return `a.title COLLATE NOCASE ASC, a.id`
	}
	switch status {
	case article.StatusPublished:
		return `julianday(a.published_at) DESC, a.id`
	case article.StatusScheduled:
		return `julianday(a.published_at) ASC, a.id`
	}
	return `julianday(a.updated_at) DESC, a.id`
}

// setRowDates は各行の「日付」の列の文を、画面の言語 lang（"en" なら英語）で入れる。日時は JST で出す。
func setRowDates(rows []articleRow, lang string) {
	en := lang == "en"
	label := func(ja, enLabel string) string {
		if en {
			return enLabel
		}
		return ja
	}
	for i := range rows {
		r := &rows[i]
		r.DatePrimary, r.DateNote = "", ""
		switch r.Status {
		case article.StatusPublished, article.StatusScheduled:
			if r.PublishedAt == nil || r.PublishedAt.IsZero() {
				r.DatePrimary = label("更新 ", "Updated ") + r.UpdatedAt.In(jst).Format(displayTimeLayout)
				continue
			}
			if r.Status == article.StatusScheduled {
				r.DatePrimary = label("公開予定 ", "Scheduled ") + r.PublishedAt.In(jst).Format(displayTimeLayout)
				continue
			}
			r.DatePrimary = label("公開 ", "Published ") + r.PublishedAt.In(jst).Format(displayTimeLayout)
			if r.UpdatedAt.Sub(*r.PublishedAt) > 24*time.Hour {
				r.DateNote = label("更新 ", "Updated ") + r.UpdatedAt.In(jst).Format("2006/01/02")
			}
		default:
			if !r.UpdatedAt.IsZero() {
				r.DatePrimary = label("更新 ", "Updated ") + r.UpdatedAt.In(jst).Format(displayTimeLayout)
			}
		}
	}
}

// IsTrash はゴミ箱のタブを表示しているかを返す。
func (d listData) IsTrash() bool { return d.Status == article.StatusDeleted }

// trashNotices はゴミ箱の操作の後に一覧で伝える文の翻訳キー。クエリの任意文字列を画面に出さないよう、
// ?trash=<key> のキーだけを受け付ける。
var trashNotices = map[string]string{
	"restored": "trash_restored",
	"purged":   "trash_purged",
	"emptied":  "trash_emptied",
}

// kindQuery は記事一覧の URL に付ける種類のクエリ（記事なら空）。既存の記事一覧の URL は変えない。
func kindQuery(kind article.Kind) string {
	if kind == article.KindPage {
		return "&kind=page"
	}
	return ""
}

// articleForm は編集フォームの入力値。
type articleForm struct {
	ID              string
	Title           string
	Slug            string
	Kind            article.Kind // 種類の欄が無い送信では空で、保存済みの種類を変えない
	CategoryID      int64
	Excerpt         string
	ContentMarkdown string
	// Status は画面に出す状態（article.ViewStatus）。予約中の記事は scheduled。
	Status    article.Status
	UpdatedAt time.Time
	// PublishedAt は公開日時（予約投稿では公開予定日時）。WasPublished は読者に見えたことがある（公開日時が過去の）記事か。
	// 一度公開した記事は予約にできないため、承認の欄に予約日時を出さない。
	PublishedAt  *time.Time
	WasPublished bool
	// 学術メタデータ（論文・学会発表）。
	DOI, JournalName, ConferenceName, CoAuthors string
	// FeaturedImage はアイキャッチ画像の URL。HasFeaturedImage はフォームに入力欄があったか
	// （欄の無い送信では既存のアイキャッチを消さない）。
	FeaturedImage    string
	HasFeaturedImage bool
	// PublishAt は「公開する」の「日時を決めて予約」で入力した予約日時（datetime-local の値、日本時間）。空なら今すぐ公開する。
	// 入力の不備で編集画面を出し直すときは、入力欄を開いたまま値を残す。
	PublishAt string

	// 多言語・翻訳フィールド
	Lang               string
	TranslationGroupID string
	SourceArticleID    string
	IsOutdated         bool

	// 固定ページの階層。ParentID は親ページの ID（空なら最上位）、SortOrder は同じ親の中の並び順（入力のままの文字列。
	// applyForm が整数として確かめる）。HasHierarchy はフォームに親ページの欄があったか（欄の無い送信・記事の編集では
	// 保存済みの親・並び順を変えない）。
	ParentID     string
	SortOrder    string
	HasHierarchy bool
}

// HasAcademicMeta は学術メタデータが1つでも入力されているか。編集画面で入力欄を開いておくために使う。
func (f articleForm) HasAcademicMeta() bool {
	return f.DOI != "" || f.JournalName != "" || f.ConferenceName != "" || f.CoAuthors != ""
}

// IsPage はページ（kind = page）の編集か。ページではカテゴリの欄を隠す。
func (f articleForm) IsPage() bool { return f.Kind == article.KindPage }

// editData は article_edit.html に渡すデータ。
type editData struct {
	IsNew           bool
	Form            articleForm
	SavedDraftToken string
	Categories      []category
	// Lock は他のユーザーが保持している有効なロック。nil なら自分が編集できる。
	Lock *lock.LockInfo
	// CanForceUnlock は Lock を強制解除できる（管理者である）か。
	CanForceUnlock bool
	Preview        previewData
	// HeartbeatSeconds はロックのハートビート間隔（秒）。
	HeartbeatSeconds int
	// AIConfigured は AI 執筆アシスタントが使えるか。false ならツールバーのボタンは設定ガイドを表示する。
	AIConfigured bool
	// AIProvider は AI アシスタントのプロバイダー名。AIGuide は未設定のときの設定ガイド。
	AIProvider, AIGuide string
	// AIConfiguredProviders は AI モーダルで切り替えられる、使える AI（先頭がメインのプロバイダー）。選んだものの Value を
	// /admin/ai/* の provider に送る。AIProviderKind はメインのプロバイダーの種類（AIConfiguredProviders の先頭の Value）。
	AIConfiguredProviders []aiProviderChoice
	AIProviderKind        string

	// CanPublish は公開・承認・差し戻し・予約ができる（編集者・管理者である）か。
	CanPublish bool
	// Locked は公開中・予約中（article.IsScheduled）の記事で、執筆者には保存できないか。
	Locked bool
	// Attachments は記事の添付資料、Comments は内部の申し送りコメント。
	Attachments attachmentListData
	Comments    commentListData
	// MinScheduleAt は予約日時の入力欄の最小値（datetime-local 形式、JST）。
	MinScheduleAt string
	// RemoteSync は公開サーバー連携で表示・実行できる操作。
	RemoteSync remoteSyncView

	// 多言語編集・左右スプリットビュー
	SourceArticle *article.Article
	ShowSplitView bool
	IsOutdated    bool

	// Proposal は AI アプリ（MCP）から届いた、確認待ちの変更案（handlers_proposal.go）。編集できるときだけ出す。
	// ProposalWaiting は案があるが、他の人が編集中・公開の権限が無いなどでこの画面からは確かめられないとき。
	Proposal        *proposalView
	ProposalWaiting bool
	// SavedBy は今のタイトル・本文を保存した人（分からなければ「不明」）。AI翻訳で編集欄を置き換える前の確認に出す。
	SavedBy string
	// Lang は管理画面の表示言語。editData を受け取るパーシャル（版履歴・AI・SNS のモーダル）の T に使う。
	Lang string
	// MultilingualEnabled は多言語対応を有効にしているか。OFF なら翻訳ボタン・英語版の作成・言語バッジを出さない。
	MultilingualEnabled bool
	// ParentPages は固定ページの編集で「親ページ」の欄に出す候補（ゴミ箱にない固定ページを階層の順に。自分自身と
	// 自分の子孫は除く）。固定ページの編集のときだけ renderEditPage が埋める。
	ParentPages []parentPageOption
}

// parentPageOption は「親ページ」の欄の選択肢1つ。
type parentPageOption struct {
	ID string
	// Label は階層の深さだけ字下げしたタイトル。
	Label string
	// Status は保存した状態（下書き・校閲待ちなどを見分けて添える）。Missing は選択中の親が候補に無い（ゴミ箱にある・
	// 別の言語の）ページであることを示す。
	Status   article.Status
	Missing  bool
	Selected bool
}

// ReadOnly は他のユーザーが編集中で、閲覧専用で表示するか。
func (d editData) ReadOnly() bool { return d.Lock != nil }

// T は表示言語 Lang の文言を返す（page.T と同じ）。
func (d editData) T(key string, args ...any) string { return i18n.Translate(d.Lang, key, args...) }

// remoteSubmitButton は partials/remote_sync_modal.html の "remote_sync_submit_button" に渡すデータ。
// ArticleID が空（保存前の新しい記事）のときはボタンを出さない。
type remoteSubmitButton struct {
	ArticleID string
	Lang      string
}

// RemoteSubmitButton は公開サーバーへの提出ボタンに渡すデータを返す。
func (d editData) RemoteSubmitButton() remoteSubmitButton {
	return remoteSubmitButton{ArticleID: d.Form.ID, Lang: d.Lang}
}

// previewData は partials/preview.html に渡すデータ。HTML は markdown.Render でサニタイズ済み。
type previewData struct {
	HTML     template.HTML
	Headings []markdown.Heading
	// Lang は管理画面の表示言語（「目次」などの見出しに使う）。
	Lang string
}

// handleArticleList は種類（?kind=post|page、既定は記事）と状態ごとのタブで記事一覧を返す。
func (s *Server) handleArticleList(c echo.Context) error {
	kind := article.Kind(c.QueryParam("kind"))
	if !kind.Valid() {
		kind = article.KindPost
	}
	rawStatus := c.QueryParam("status")
	status := article.Status(rawStatus)
	switch status {
	case article.StatusReview, article.StatusScheduled, article.StatusPublished, article.StatusDeleted:
	default:
		if rawStatus == "" && kind == article.KindPage {
			status = article.StatusPublished
		} else {
			status = article.StatusDraft
		}
	}
	pageNum, err := strconv.Atoi(c.QueryParam("page"))
	if err != nil || pageNum < 1 {
		pageNum = 1
	}

	currentLang := strings.TrimSpace(c.QueryParam("lang"))
	if currentLang != "ja" && currentLang != "en" {
		currentLang = ""
	}

	searchQuery := strings.TrimSpace(c.QueryParam("q"))
	sort := validSort(c.QueryParam("sort"))
	uiLang := string(language(c))

	now := clockNow().UTC()
	counts, err := s.countByStatus(kind, now, currentLang, searchQuery)
	if err != nil {
		return err
	}
	kindCounts, err := s.countByKind(now, currentLang, searchQuery)
	if err != nil {
		return err
	}
	rows, err := s.listArticles(kind, status, now, articlesPerPage+1, (pageNum-1)*articlesPerPage, currentLang, searchQuery, sort)
	if err != nil {
		return err
	}
	setRowDates(rows, uiLang)

	data := listData{
		Kind:       kind,
		Status:     status,
		Rows:       rows,
		Page:       pageNum,
		CanPublish: canPublish(currentUser(c)),
		Lang:       currentLang,
		Query:      searchQuery,
		Sort:       sort,

		MultilingualEnabled: s.isMultilingualEnabled(),
	}
	for _, v := range listSorts {
		key := "sort_default"
		if v != "" {
			key = "sort_" + v
		}
		data.Sorts = append(data.Sorts, sortOption{Value: v, Label: i18n.Translate(uiLang, key), Active: v == sort})
	}
	for _, k := range listKinds {
		data.KindTabs = append(data.KindTabs, kindTab{Kind: k, Count: kindCounts[k], Active: k == kind})
	}
	if len(rows) > articlesPerPage {
		data.Rows = rows[:articlesPerPage]
		data.NextPage = pageNum + 1
	}
	if pageNum > 1 {
		data.PrevPage = pageNum - 1
	}
	for _, st := range listTabs {
		data.Tabs = append(data.Tabs, listTab{Status: st, Label: statusLabel(st), Count: counts[st], Active: st == status})
	}
	if status == article.StatusDeleted {
		if err := s.db.Get(&data.TrashTotal, `SELECT COUNT(*) FROM articles WHERE status = ?`, article.StatusDeleted); err != nil {
			return err
		}
		if key, ok := trashNotices[c.QueryParam("trash")]; ok {
			data.TrashNotice = i18n.Translate(uiLang, key)
		}
	}
	if id := c.QueryParam("deleted"); id != "" {
		if a, err := s.articles.GetByID(id); err == nil && a.Status == article.StatusDeleted {
			data.DeletedTitle = a.Title
		}
	}
	if data.DeletedTitle != "" && c.QueryParam("flash") == "deleted" {
		// 一覧の上のお知らせが同じことを伝えるので、トースト通知は重ねて出さない（公開先からの取り下げの結果は出す）。
		return s.renderPage(c, http.StatusOK, "article_list", pageTitle(c, "articles"), data, "", false)
	}
	return s.render(c, http.StatusOK, "article_list", pageTitle(c, "articles"), data)
}

// handleArticleNew は新規記事の編集画面を返す。?kind=page ならページとして書き始める。保存前の記事にはロックを取らない。
func (s *Server) handleArticleNew(c echo.Context) error {
	cats, err := s.listCategories()
	if err != nil {
		return err
	}
	data := editData{IsNew: true, Form: articleForm{Status: article.StatusDraft, Kind: article.KindPost, Lang: "ja"}}
	if len(cats) > 0 {
		data.Form.CategoryID = cats[0].ID
	}
	if article.Kind(c.QueryParam("kind")) == article.KindPage {
		data.Form.Kind = article.KindPage
		// ページはカテゴリを表示しないため、既定のカテゴリ（未分類）に入れる。
		if data.Form.CategoryID, err = s.defaultCategoryID(); err != nil {
			return err
		}
	}

	sourceID := strings.TrimSpace(c.QueryParam("source_article_id"))
	targetLang := strings.TrimSpace(c.QueryParam("lang"))
	if sourceID != "" {
		src, err := s.articles.GetByID(sourceID)
		if err == nil && src != nil {
			if targetLang == "" {
				targetLang = "en"
			}
			data.Form.Lang = targetLang
			data.Form.SourceArticleID = src.ID
			groupID := src.TranslationGroupID
			if groupID == "" {
				groupID = src.ID
			}
			data.Form.TranslationGroupID = groupID
			data.Form.CategoryID = src.CategoryID
			data.Form.FeaturedImage = src.FeaturedImage
			data.Form.HasFeaturedImage = src.FeaturedImage != ""
			data.Form.Kind = src.Kind
			data.SourceArticle = src
			data.ShowSplitView = true
		}
	}

	return s.renderEdit(c, http.StatusOK, data)
}

// handleArticleView は記事を読み取り専用で表示する。主に記事を編集できない管理者が中身を確かめるために使う。
// 執筆チーム内では非公開の記事を共有するため、ログインしている人なら誰の記事でも開ける。
func (s *Server) handleArticleView(c echo.Context) error {
	a, err := s.articles.GetByID(c.Param("id"))
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	canEdit := currentUser(c).HasRole(auth.RoleEditor, auth.RoleWriter)
	var authorName string
	if err := s.db.Get(&authorName, `SELECT COALESCE(NULLIF(display_name, ''), username, '') FROM users WHERE id = ?`, a.AuthorID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	status := article.ViewStatus(a, clockNow())
	publicURL := ""
	if status == article.StatusPublished {
		publicURL = a.URL()
	}
	data := articleViewData{
		Article: a, AuthorName: authorName, Status: status, ContentHTML: template.HTML(a.ContentHTML),
		CanEdit: canEdit, PublicURL: publicURL,
	}
	return s.render(c, http.StatusOK, "article_view", a.Title, data)
}

// handleArticleCreate は新規記事を作成し、その編集画面へリダイレクトする。
func (s *Server) handleArticleCreate(c echo.Context) error {
	form := readArticleForm(c)
	token := c.FormValue("draft_token")
	user := currentUser(c)
	a := &article.Article{AuthorID: user.ID}
	action := c.FormValue("action")
	if msg, err := s.applyForm(c, a, form, action, user); err != nil {
		return err
	} else if msg != "" {
		return s.renderFormError(c, http.StatusUnprocessableEntity, editData{IsNew: true, Form: form}, msg)
	}
	if err := s.articles.Save(a); err != nil {
		return s.saveError(c, editData{IsNew: true, Form: form}, err)
	}

	if a.Lang == "en" {
		_ = s.articles.ClearOutdated(c.Request().Context(), a.ID)
	} else if a.Lang == "ja" && a.TranslationGroupID != "" && a.TranslationGroupID != a.ID {
		_ = s.articles.MarkTranslationsOutdated(c.Request().Context(), a.TranslationGroupID, a.ID)
	}

	s.reindexArticle(c, a.ID)
	s.trackScheduled(c, a.ID)
	createComment := "作成"
	if action == actionReview || action == actionPublish {
		createComment = revisionComment(a, action, createComment)
	}
	s.snapshot(c, a, createComment)
	if a.Status == article.StatusReview {
		s.addSystemComment(c, a.ID, reviewRequestComment(strings.TrimSpace(normalizeNewlines(c.FormValue("note")))))
		redirect := "/admin/articles?status=review" + kindQuery(a.Kind) + "&flash=submitted"
		if token != "" {
			redirect += "&saved_draft_token=" + url.QueryEscape(token)
		}
		return c.Redirect(http.StatusSeeOther, redirect)
	}
	if action == actionPublish && article.IsScheduled(a, clockNow()) {
		return c.Redirect(http.StatusSeeOther, articleEditRedirect(a.ID, "scheduled", token))
	}
	flash := "created"
	if article.ViewStatus(a, clockNow()) == article.StatusPublished {
		// 今すぐ公開した記事は、記事公開時の自動反映の設定に従ってサイトに反映し、その旨を知らせる。
		if key := s.reflectAfterPublish(); key != "" {
			flash = key
		}
		// いまの公開方式が公開サーバーのときだけ送る（静的サイトへの自動反映と両方へ送らない。publish_method.go）。
		if s.canRemotePublish(user) {
			flash = s.remotePublishFlash(c, a)
		}
	}
	return c.Redirect(http.StatusSeeOther, articleEditRedirect(a.ID, flash, token))
}

// handleArticleEdit は記事の編集画面を返す。ロックを獲得できれば編集可能、
// 他のユーザーが編集中なら警告バナー付きの閲覧専用モードで表示する。
func (s *Server) handleArticleEdit(c echo.Context) error {
	id := c.Param("id")
	user := currentUser(c)

	info, err := s.locker.Acquire(c.Request().Context(), id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return echo.ErrNotFound
	}
	if err != nil && !errors.Is(err, lock.ErrAlreadyLocked) {
		return err
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}

	data := editData{Form: formFromArticle(a), SavedDraftToken: c.QueryParam("saved_draft_token")}
	if !info.IsOwnLock {
		data.Lock = info
	}

	// 多言語編集（原文・スプリットビュー・要更新）の解決
	if a.Lang == "en" || a.IsOutdated || (a.SourceArticleID.Valid && a.SourceArticleID.String != "") {
		var src *article.Article
		if a.SourceArticleID.Valid && a.SourceArticleID.String != "" {
			src, _ = s.articles.GetByID(a.SourceArticleID.String)
		}
		if src == nil && a.TranslationGroupID != "" {
			src, _ = s.articles.Translation(c.Request().Context(), a.TranslationGroupID, "ja")
		}
		if src != nil {
			data.SourceArticle = src
			data.ShowSplitView = true
		}
		data.IsOutdated = a.IsOutdated
	}

	return s.renderEdit(c, http.StatusOK, data)
}

// handleArticleSave は記事を保存する。action=publish なら公開、それ以外は下書きとして保存する。
// 保存前にロックを獲得（自分のロックなら延長）し、他のユーザーが編集中なら 409 で拒否する。
func (s *Server) handleArticleSave(c echo.Context) error {
	id := c.Param("id")
	token := c.FormValue("draft_token")
	user := currentUser(c)
	form := readArticleForm(c)
	form.ID = id

	info, err := s.locker.Acquire(c.Request().Context(), id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return echo.ErrNotFound
	}
	if errors.Is(err, lock.ErrAlreadyLocked) {
		// 入力内容を失わないよう、送信された値を閲覧専用で表示し直す。
		return s.renderFormError(c, http.StatusConflict, editData{Form: form, Lock: info},
			info.UserName+"さんが編集中のため保存できませんでした")
	}
	if err != nil {
		return err
	}

	// ロック獲得後に読み直し、Save が保存する locked_by / locked_at を最新にする。
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if base, parseErr := strconv.ParseInt(c.FormValue("base_updated_at"), 10, 64); parseErr == nil && base != 0 && base != a.UpdatedAt.Unix() {
		return s.renderFormError(c, http.StatusConflict, editData{Form: withStoredState(form, a)}, "他の端末またはユーザーによって記事が更新されています")
	}
	// AI アプリの変更案を編集欄に反映して保存するときは、確かめた案と記事の内容が変わっていないかを確かめる。
	// AI 機能が OFF なら変更案は扱わず、ふだんの保存にする（案は確定も削除もしない）。
	var proposal proposalConfirmation
	if s.aiEnabled() {
		var msg string
		if proposal, msg, err = s.confirmedProposal(c, a); err != nil {
			return err
		}
		if msg != "" {
			return s.renderFormError(c, http.StatusConflict, editData{Form: withStoredState(form, a)}, msg)
		}
	}
	wasPublished := article.ViewStatus(a, clockNow()) == article.StatusPublished
	action := c.FormValue("action")
	oldTitle := a.Title
	oldContent := a.ContentMarkdown
	if msg, err := s.applyForm(c, a, form, action, user); err != nil {
		return err
	} else if msg != "" {
		// applyForm は不備を見つけると a を変えずに返すため、a は保存済みの記事のまま。
		return s.renderFormError(c, http.StatusUnprocessableEntity, editData{Form: withStoredState(form, a)}, msg)
	}

	// 保存直前にもロック状態を再確認（処理中の強制解除・他者奪取による上書き事故を防止）
	if check, err := s.locker.Check(c.Request().Context(), id, user.ID); err == nil && (!check.IsOwnLock || !check.IsLocked) {
		return s.renderFormError(c, http.StatusConflict, editData{Form: form, Lock: check},
			"編集ロックが失効または解除されたため保存できませんでした")
	}

	// 原文が変わる前に作られた（または原文を確かめられない）翻訳の案を反映したときは、要更新のままにする。
	if proposal.KeepOutdated {
		a.IsOutdated = true
	}
	if err := s.articles.Save(a); err != nil {
		return s.saveError(c, editData{Form: form}, err)
	}

	contentChanged := (oldTitle != a.Title || oldContent != a.ContentMarkdown)
	if a.Lang == "ja" && a.TranslationGroupID != "" && contentChanged {
		_ = s.articles.MarkTranslationsOutdated(c.Request().Context(), a.TranslationGroupID, a.ID)
	}
	if (a.Lang == "en" || a.IsOutdated) && !proposal.KeepOutdated {
		_ = s.articles.ClearOutdated(c.Request().Context(), a.ID)
	}

	s.reindexArticle(c, a.ID)
	s.trackScheduled(c, a.ID)
	s.snapshot(c, a, proposalComment(revisionComment(a, action, "保存"), proposal.ID))
	if s.aiEnabled() {
		s.finishProposal(c, a.ID, proposal.ID)
	}
	remoteUnpublishFlash := ""
	if wasPublished && article.ViewStatus(a, clockNow()) != article.StatusPublished {
		// 公開中の記事を下書きなどへ戻したら、公開先も最新の公開状態に同期する。
		s.reflectAfterPublish()
		if s.remoteSyncViewFor(user).CanPublish {
			if err := s.unpublishRemoteArticle(c, a); err != nil {
				c.Logger().Warnf("remote unpublish article %s: %v", a.ID, err)
				remoteUnpublishFlash = "remote_unpublish_failed"
			} else {
				remoteUnpublishFlash = "remote_unpublished"
			}
		}
	}

	flash := "saved"
	switch article.ViewStatus(a, clockNow()) {
	case article.StatusPublished:
		// 下書き・校閲待ちから公開した記事も、公開中のまま直した記事も、記事公開時の自動反映の設定に従ってサイトに反映する。
		flash = "published"
		if key := s.reflectAfterPublish(); key != "" {
			flash = key
		}
		// いまの公開方式が公開サーバーのときだけ送る（静的サイトへの自動反映と両方へ送らない。publish_method.go）。
		if s.canRemotePublish(user) {
			flash = s.remotePublishFlash(c, a)
		}
	case article.StatusScheduled:
		// 「日時を決めて予約」は予約した日時を知らせる。予約を保ったままの保存は「保存しました」のまま。
		if action == actionPublish {
			flash = "scheduled"
		}
	case article.StatusReview:
		if action == actionReview {
			s.addSystemComment(c, a.ID, reviewRequestComment(strings.TrimSpace(normalizeNewlines(c.FormValue("note")))))
			flash = "submitted"
		}
	}
	if remoteUnpublishFlash != "" {
		// 校閲申請をした場合も、公開先への取り下げ結果を優先して伝える。
		flash = remoteUnpublishFlash
	}
	return c.Redirect(http.StatusSeeOther, articleEditRedirect(a.ID, flash, token))
}

func articleEditRedirect(id, flash, token string) string {
	query := url.Values{"flash": {flash}}
	if token != "" {
		query.Set("saved_draft_token", token)
	}
	return "/admin/articles/" + id + "/edit?" + query.Encode()
}

// trackScheduled は記事 ids の今の状態を、予約だった記事の集合（scheduler.Track）に写す。予約にした記事を覚え、
// 下書き・校閲待ちに戻した・ゴミ箱に移した・「今すぐ公開」にした記事は忘れる。公開日時を迎えたあとに保存し直した
// 予約は覚えたままにする（反映に成功するまで PendingScheduledReflects が数える）。記事の保存は終わっているため、
// 失敗してもログに残すだけにする（予約中の記事は公開日時で数え、集合は scheduler が確かめるたびに合わせ直す）。
func (s *Server) trackScheduled(c echo.Context, ids ...string) {
	ctx := context.WithoutCancel(c.Request().Context())
	if err := scheduler.Track(ctx, s.db, clockNow(), ids...); err != nil {
		c.Logger().Warnf("scheduler: track scheduled articles %v: %v", ids, err)
	}
}

// revisionComment は保存した記事 a のリビジョンに付けるメモを決める。「公開する」で予約にした記事は「予約公開」、
// それ以外は snapshotComment で action から決める。
func revisionComment(a *article.Article, action, fallback string) string {
	if action == actionPublish && article.IsScheduled(a, clockNow()) {
		return "予約公開"
	}
	return snapshotComment(action, fallback)
}

// snapshotComment はリビジョンに付けるメモを action から決める。
func snapshotComment(action, fallback string) string {
	switch action {
	case actionPublish:
		return "公開"
	case actionReview:
		return "校閲申請"
	case actionDraft:
		return "下書き保存"
	}
	return fallback
}

// snapshot は保存した記事のリビジョンを記録する。保存自体は成功しているため、失敗してもログに残すだけにする。
func (s *Server) snapshot(c echo.Context, a *article.Article, comment string) {
	if _, _, err := s.revisions.SaveSnapshot(c.Request().Context(), a, currentUser(c).ID, comment); err != nil {
		c.Logger().Warnf("revision: snapshot article %s: %v", a.ID, err)
	}
}

// handleArticleDelete は記事を論理削除（ゴミ箱へ移動）する。他のユーザーが編集中なら 409 で拒否する。
func (s *Server) handleArticleDelete(c echo.Context) error {
	id := c.Param("id")
	user := currentUser(c)
	ctx := c.Request().Context()

	// 削除前に確実にロックを獲得（他者が編集中なら 409 で拒否）
	info, err := s.locker.Acquire(ctx, id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return echo.ErrNotFound
	}
	if errors.Is(err, lock.ErrAlreadyLocked) {
		return echo.NewHTTPError(http.StatusConflict, info.UserName+"さんが編集中のため削除できません")
	}
	if err != nil {
		return err
	}
	defer func() {
		_ = s.locker.Release(ctx, id, user.ID)
	}()

	// 公開中・予約中の記事をゴミ箱へ移すことは公開の取り下げにあたるため、編集者・管理者に限る。
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if requiresPublisher(a.Status) && !canPublish(user) {
		return echo.NewHTTPError(http.StatusForbidden, "公開中・予約中の記事をゴミ箱に移せるのは編集長・管理者だけです")
	}

	if err := s.articles.SoftDelete(id); err != nil {
		if errors.Is(err, article.ErrNotFound) {
			return echo.ErrNotFound
		}
		return err
	}
	s.reindexArticle(c, id)
	s.trackScheduled(c, id)
	deletedPublic := article.ViewStatus(a, clockNow()) == article.StatusPublished || article.IsScheduled(a, clockNow())
	flash := "deleted"
	if deletedPublic {
		// 公開・予約中の記事をゴミ箱へ移したら、公開先からも取り下げる。
		s.reflectAfterPublish()
		if s.remoteSyncViewFor(user).CanPublish {
			if err := s.unpublishRemoteArticle(c, a); err != nil {
				c.Logger().Warnf("remote unpublish deleted article %s: %v", a.ID, err)
				flash = "remote_unpublish_failed"
			} else {
				flash = "remote_unpublished"
			}
		}
	}
	return c.Redirect(http.StatusSeeOther, articleDeleteReturnURL(c, a.Kind)+"&flash="+flash+"&deleted="+url.QueryEscape(a.ID))
}

// articleDeleteReturnURL は「ゴミ箱へ」のあとに戻る記事一覧の URL（末尾にクエリを足せる形）を返す。
// 一覧の削除フォームは表示中のタブ（return_status）・言語（return_lang）・検索語（return_q）を送るので、その一覧へ戻す。
// 送らない画面（編集画面など）からは、移した先が分かるようゴミ箱の一覧を開く。
func articleDeleteReturnURL(c echo.Context, kind article.Kind) string {
	st := article.Status(c.FormValue("return_status"))
	if !slices.Contains(listTabs, st) {
		st = article.StatusDeleted
	}
	// 一覧のほかのリンクと同じく status・kind の順に並べる（url.Values.Encode はキーの順に並べ替えるため使わない）。
	u := "/admin/articles?status=" + string(st) + kindQuery(kind)
	if lang := c.FormValue("return_lang"); lang == "ja" || lang == "en" {
		u += "&lang=" + lang
	}
	if q := strings.TrimSpace(c.FormValue("return_q")); q != "" {
		u += "&q=" + url.QueryEscape(q)
	}
	return u + sortQuery(validSort(c.FormValue("return_sort")))
}

// trashListURL はゴミ箱のタブの URL。notice はゴミ箱の操作の結果（trashNotices のキー）。
func trashListURL(kind article.Kind, notice string) string {
	return "/admin/articles?status=deleted" + kindQuery(kind) + "&trash=" + notice
}

// handleArticleRestore はゴミ箱の記事を下書きに戻す。下書きのため読者には見えず、公開し直すには編集画面から公開する。
func (s *Server) handleArticleRestore(c echo.Context) error {
	id := c.Param("id")
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	switch err := s.articles.RestoreFromTrash(id); {
	case errors.Is(err, article.ErrNotFound):
		return echo.ErrNotFound
	case errors.Is(err, article.ErrNotInTrash):
		return echo.NewHTTPError(http.StatusConflict, i18n.Translate(string(language(c)), "trash_not_in_trash"))
	case errors.Is(err, article.ErrRestoreConflict):
		return echo.NewHTTPError(http.StatusConflict, i18n.Translate(string(language(c)), "trash_restore_conflict"))
	case err != nil:
		return err
	}
	s.reindexArticle(c, id)
	return c.Redirect(http.StatusSeeOther, trashListURL(a.Kind, "restored"))
}

// handleArticlePurge はゴミ箱の記事を完全に削除する（版の履歴・コメント・添付資料も消え、元に戻せない）。
// 他のユーザーが編集画面で開いている記事は消さない。
func (s *Server) handleArticlePurge(c echo.Context) error {
	id := c.Param("id")
	user := currentUser(c)
	info, err := s.locker.Check(c.Request().Context(), id, user.ID)
	if errors.Is(err, lock.ErrArticleNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if info.IsLocked && !info.IsOwnLock {
		return echo.NewHTTPError(http.StatusConflict, info.UserName+"さんが編集中のため削除できません")
	}
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	res, err := s.articles.HardDelete(id)
	switch {
	case errors.Is(err, article.ErrNotFound):
		return echo.ErrNotFound
	case errors.Is(err, article.ErrNotInTrash):
		return echo.NewHTTPError(http.StatusConflict, i18n.Translate(string(language(c)), "trash_not_in_trash"))
	case err != nil:
		return err
	}
	s.afterPurge(c.Logger(), res)
	return c.Redirect(http.StatusSeeOther, trashListURL(a.Kind, "purged"))
}

// handleTrashEmpty はゴミ箱のすべての記事とページを完全に削除する。
func (s *Server) handleTrashEmpty(c echo.Context) error {
	res, err := s.articles.EmptyTrash()
	if err != nil {
		return err
	}
	s.afterPurge(c.Logger(), res)
	return c.Redirect(http.StatusSeeOther, trashListURL(article.Kind(c.FormValue("kind")), "emptied"))
}

// afterPurge は完全に削除した記事の、DB の外の後片付けをする。添付資料のファイルを消し、検索インデックスから外す。
// 管理画面からの完全削除と、スケジューラによる30日後の自動削除で共通。
func (s *Server) afterPurge(logger echo.Logger, res article.PurgeResult) {
	for _, name := range res.AttachmentFiles {
		p, ok := s.attachmentPath(name)
		if !ok {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warnf("attachment: remove %s: %v", p, err)
		}
	}
	for _, id := range res.IDs {
		s.reindex(logger, id)
	}
}

// handlePreview は本文 Markdown をサニタイズ済み HTML と目次に変換したパーシャルを返す。
func (s *Server) handlePreview(c echo.Context) error {
	p, err := s.renderPreview(c.FormValue("content_markdown"))
	if err != nil {
		return err
	}
	p.Lang = string(language(c))
	return c.Render(http.StatusOK, "preview", p)
}

func renderPreview(src string) (previewData, error) {
	res, err := markdown.Render(src)
	if err != nil {
		return previewData{}, err
	}
	// markdown.Render の HTML は Bluemonday でサニタイズ済みのため、エスケープせずに埋め込める。
	return previewData{HTML: template.HTML(res.HTML), Headings: res.Headings}, nil
}

func (s *Server) renderPreview(src string) (previewData, error) {
	res, err := markdown.RenderWithGalleryQuery(src, s.galleryQueryResolver())
	if err != nil {
		return previewData{}, err
	}
	return previewData{HTML: template.HTML(res.HTML), Headings: res.Headings}, nil
}

// renderEdit は data に不足している共通の値（カテゴリ・プレビュー等）を埋めて編集画面を返す。
func (s *Server) renderEdit(c echo.Context, code int, data editData) error {
	return s.renderEditPage(c, code, data, "")
}

// renderFormError は errMsg をエラー通知として表示した編集画面を返す。
func (s *Server) renderFormError(c echo.Context, code int, data editData, errMsg string) error {
	return s.renderEditPage(c, code, data, errMsg)
}

func (s *Server) renderEditPage(c echo.Context, code int, data editData, errMsg string) error {
	cats, err := s.listCategories()
	if err != nil {
		return err
	}
	data.Categories = cats
	if data.Preview, err = s.renderPreview(data.Form.ContentMarkdown); err != nil {
		return err
	}
	data.Preview.Lang = data.Lang
	data.HeartbeatSeconds = int(HeartbeatInterval / time.Second)
	data.Lang = string(language(c))
	user := currentUser(c)
	data.CanForceUnlock = data.Lock != nil && user.HasRole(auth.RoleEditor, auth.RoleAdmin)
	data.CanPublish = canPublish(user)
	data.MultilingualEnabled = s.isMultilingualEnabled()
	data.Locked = !data.CanPublish && requiresPublisher(data.Form.Status)
	data.MinScheduleAt = minScheduleAt(clockNow())
	data.RemoteSync = s.remoteSyncViewFor(user)
	if data.Form.IsPage() {
		if data.ParentPages, err = s.parentPageOptions(c.Request().Context(), data.Form); err != nil {
			return err
		}
	}
	if !data.IsNew {
		if data.Attachments, err = s.attachmentList(c, data.Form.ID); err != nil {
			return err
		}
		data.Attachments.CanEdit = !data.ReadOnly() && !data.Locked
		if data.Comments, err = s.commentList(c, data.Form.ID); err != nil {
			return err
		}
		if s.aiEnabled() {
			if err := s.loadProposalState(c, &data); err != nil {
				return err
			}
		}
	}
	provider := s.aiProvider()
	data.AIProvider = provider.Name()
	data.AIConfigured = ai.IsConfigured(provider)
	if !data.AIConfigured {
		data.AIGuide = s.aiSetupGuide()
	}
	data.AIConfiguredProviders = s.aiConfiguredProviders()
	saved, hasSaved := s.loadAIConfig()
	data.AIProviderKind = mainAIKind(saved, hasSaved)

	title := pageTitle(c, "article_edit_title")
	switch {
	case data.IsNew && data.Form.IsPage():
		title = i18n.Translate(string(language(c)), "page_new_title")
	case data.IsNew:
		title = pageTitle(c, "new_article")
	case data.Form.IsPage():
		title = i18n.Translate(string(language(c)), "page_edit_title")
	}
	if errMsg != "" {
		return s.renderError(c, code, "article_edit", title, errMsg, data)
	}
	// 予約した直後（?flash=scheduled）は、予約中の記事の公開予定日時を通知に入れる。開くまでに予約でなくなっていれば
	// 日時を付けない既定の通知を出す。
	if c.QueryParam("flash") == "scheduled" && data.Form.Status == article.StatusScheduled && data.Form.PublishedAt != nil {
		return s.renderPage(c, code, "article_edit", title, data, scheduledMessage(c, *data.Form.PublishedAt), false)
	}
	return s.render(c, code, "article_edit", title, data)
}

// saveError は Save の失敗を、入力の不備なら 422 の編集画面、それ以外は内部エラーとして返す。
func (s *Server) saveError(c echo.Context, data editData, err error) error {
	if errors.Is(err, article.ErrReservedSlug) {
		return s.renderFormError(c, http.StatusUnprocessableEntity, data, reservedSlugMessage(c, data.Form))
	}
	if errors.Is(err, article.ErrInvalidParent) {
		return s.renderFormError(c, http.StatusUnprocessableEntity, data, adminText(c,
			"この親ページは選べません。自分自身・自分の子ページ・ゴミ箱のページは親ページにできません",
			"This parent page cannot be used. A page cannot be placed under itself, its own child pages, or a page in the trash"))
	}
	if errors.Is(err, article.ErrHasChildPages) {
		return s.renderFormError(c, http.StatusUnprocessableEntity, data, adminText(c,
			"子ページのある固定ページは記事に変えられません。先に子ページの親ページを変えてください",
			"A page with child pages cannot become a post. Move its child pages to another parent first"))
	}
	if errors.Is(err, article.ErrInvalid) {
		return s.renderFormError(c, http.StatusUnprocessableEntity, data, "入力内容を確認してください")
	}
	return err
}

// applyForm はフォームの値と action（押されたボタン）による状態の変更を a に反映する。
// 入力に不備があれば、a を変えずに利用者向けのメッセージを返す。action=publish に予約日時（form.PublishAt）があれば、
// 承認して公開と同じ規則（parsePublishAt・refusesSchedule）で検査し、その日時を公開日時とする予約投稿にする。権限の無い操作（執筆者による公開・公開中の記事の変更）は
// 403 の HTTP エラーを返す。種類（記事 / ページ）の変更も他の変更と同じ権限の検査に従う
// （公開中・予約中の記事の種類を変えられるのは公開の権限を持つ人だけ）。
func (s *Server) applyForm(c echo.Context, a *article.Article, form articleForm, action string, user *auth.User) (string, error) {
	next, err := nextStatus(a.Status, action, user)
	if err != nil {
		return "", err
	}
	now := clockNow().UTC()
	var scheduledAt *time.Time
	if action == actionPublish {
		var msg string
		if scheduledAt, msg = parsePublishAt(form.PublishAt, now); msg != "" {
			return msg, nil
		}
		if refusesSchedule(a, scheduledAt, now) {
			return "一度公開した記事は予約投稿にできません。日時を決めずに公開してください（最初の公開日時のまま公開します）", nil
		}
	}
	if strings.TrimSpace(form.Title) == "" {
		return "タイトルを入力してください", nil
	}
	kind := a.Kind
	if form.Kind != "" {
		if !form.Kind.Valid() {
			return i18n.Translate(string(language(c)), "kind_invalid"), nil
		}
		kind = form.Kind
	}
	if kind == "" {
		kind = article.KindPost
	}
	if kind == article.KindPage && s.articles.IsReservedPageSlug(pageSlugOf(form)) {
		return reservedSlugMessage(c, form), nil
	}
	if msg := validateAcademicMeta(&form); msg != "" {
		return msg, nil
	}
	// 変更していない既存の値（取り込んだ記事など）は検査しない。
	if form.HasFeaturedImage && form.FeaturedImage != a.FeaturedImage && !validFeaturedImageURL(form.FeaturedImage) {
		return "アイキャッチ画像の URL が正しくありません（メディアライブラリから選択してください）", nil
	}
	sortOrder := a.SortOrder
	if kind == article.KindPage && form.HasHierarchy {
		if v := strings.TrimSpace(form.SortOrder); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < -maxSortOrder || n > maxSortOrder {
				return adminText(c, "並び順は -"+strconv.Itoa(maxSortOrder)+" から "+strconv.Itoa(maxSortOrder)+" までの整数で入力してください",
					"Enter the order as a whole number from -"+strconv.Itoa(maxSortOrder)+" to "+strconv.Itoa(maxSortOrder)), nil
			}
			sortOrder = n
		} else {
			sortOrder = 0
		}
		if form.ParentID != "" && form.ParentID == a.ID {
			return adminText(c, "自分自身を親ページにはできません", "A page cannot be its own parent"), nil
		}
	}
	ok, err := s.categoryExists(form.CategoryID)
	if err != nil {
		return "", err
	}
	if !ok && kind == article.KindPage {
		// ページはカテゴリを表示しないため、欄の無い送信では既定のカテゴリ（未分類）に入れる。
		if form.CategoryID, err = s.defaultCategoryID(); err != nil {
			return "", err
		}
		ok = form.CategoryID != 0
	}
	if !ok {
		return "カテゴリを選択してください", nil
	}
	a.Kind = kind
	// 親を持てるのは固定ページだけ。記事に変えるときは親から外す（子ページを持つページは article.Service.Save が断る）。
	switch {
	case kind != article.KindPage:
		a.ParentID = ""
	case form.HasHierarchy:
		a.ParentID = form.ParentID
		a.SortOrder = sortOrder
	}
	a.Title = form.Title
	a.Slug = form.Slug
	a.CategoryID = form.CategoryID
	a.Excerpt = form.Excerpt
	a.ContentMarkdown = form.ContentMarkdown
	a.DOI = form.DOI
	a.JournalName = form.JournalName
	a.ConferenceName = form.ConferenceName
	a.CoAuthors = form.CoAuthors
	if form.HasFeaturedImage {
		a.FeaturedImage = form.FeaturedImage
	}
	if form.Lang != "" {
		a.Lang = form.Lang
	} else if a.Lang == "" {
		a.Lang = "ja"
	}
	if form.TranslationGroupID != "" {
		a.TranslationGroupID = form.TranslationGroupID
	}
	if form.SourceArticleID != "" {
		a.SourceArticleID = sql.NullString{String: form.SourceArticleID, Valid: true}
	}
	a.Status = next
	switch {
	case scheduledAt != nil:
		// 「日時を決めて予約」: 状態は公開のまま、公開日時を予約した日時にする（予約投稿）。
		a.PublishedAt = scheduledAt
	case action == actionPublish && a.PublishedAt != nil && a.PublishedAt.After(now):
		// 「今すぐ公開」: 予約中の記事を公開するときは、公開予定日時ではなく今を公開日時にする。予約を取り消した記事
		// （下書き・校閲待ちに戻した記事）の未来の公開日時は article.Service.Save が消す。
		a.PublishedAt = &now
	}
	return "", nil
}

// pageSlugOf はフォームの値から保存されるスラッグ（空ならタイトルから作る）を返す。article.Service.Save と同じ規則。
func pageSlugOf(form articleForm) string {
	if strings.TrimSpace(form.Slug) != "" {
		return article.NormalizeSlug(form.Slug)
	}
	return article.NormalizeSlug(form.Title)
}

// reservedPage は a が使えないスラッグ（article.Service.IsReservedPageSlug）のページかを返す。
// 予約語の設定を変える前に保存したページを、公開する前に再検査するために使う。
func (s *Server) reservedPage(a *article.Article) bool {
	return a.Kind == article.KindPage && s.articles.IsReservedPageSlug(a.Slug)
}

// reservedSlugMessage はページに使えないスラッグを保存しようとしたときの、利用者向けのエラー文。
func reservedSlugMessage(c echo.Context, form articleForm) string {
	return i18n.Translate(string(language(c)), "reserved_page_slug", pageSlugOf(form))
}

// nextStatus は現在の状態 cur の記事を action で保存したときの状態を返す。
//
// 権限マトリクス: 公開（publish）と、公開中・予約中の記事の変更は編集者・管理者だけが行える。
// 執筆者は下書きの保存と校閲申請ができ、校閲待ちの記事は「保存」で校閲待ちのまま直すか、
// 「下書き保存」で申請を取り下げられる。ゴミ箱の記事を保存すると下書きに戻る。
func nextStatus(cur article.Status, action string, user *auth.User) (article.Status, error) {
	if action == actionPublish || requiresPublisher(cur) {
		if !canPublish(user) {
			if action == actionPublish {
				return "", echo.NewHTTPError(http.StatusForbidden, "公開できるのは編集長・管理者だけです。「校閲を申請」から依頼してください")
			}
			return "", echo.NewHTTPError(http.StatusForbidden, "公開中・予約中の記事を変更できるのは編集長・管理者だけです")
		}
	}
	switch action {
	case actionPublish:
		return article.StatusPublished, nil
	case actionReview:
		return article.StatusReview, nil
	case actionSave:
		if cur == "" || cur == article.StatusDeleted {
			return article.StatusDraft, nil
		}
		return cur, nil
	}
	return article.StatusDraft, nil
}

// canPublish は user が公開・承認・差し戻しをできる（編集者である）かを返す。
// 管理者は編集者の役割も持つため公開できる（auth.User.HasRole）。ライターは公開できない。
func canPublish(user *auth.User) bool {
	return user.HasRole(auth.RoleEditor)
}

// requiresPublisher は st の記事の変更に編集者・管理者の権限が要るか（読者に見える・見える予定か）を返す。
// st は保存した状態（予約中の記事は published）でも、画面に出す状態（article.ViewStatus の scheduled）でもよい。
func requiresPublisher(st article.Status) bool {
	return st == article.StatusPublished || st == article.StatusScheduled
}

// validateAcademicMeta は学術メタデータを検証・正規化する。DOI は https://doi.org/ などの前置きを除いて保存する。
func validateAcademicMeta(f *articleForm) string {
	for _, v := range []string{f.DOI, f.JournalName, f.ConferenceName, f.CoAuthors} {
		if len([]rune(v)) > maxMetaRunes {
			return "学術情報の各項目は" + strconv.Itoa(maxMetaRunes) + "文字以内で入力してください"
		}
	}
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi:"} {
		if len(f.DOI) >= len(prefix) && strings.EqualFold(f.DOI[:len(prefix)], prefix) {
			f.DOI = strings.TrimSpace(f.DOI[len(prefix):])
			break
		}
	}
	if f.DOI != "" && !doiPattern.MatchString(f.DOI) {
		return "DOI は「10.1234/abcd」の形式で入力してください"
	}
	return ""
}

func readArticleForm(c echo.Context) articleForm {
	catID, _ := strconv.ParseInt(c.FormValue("category_id"), 10, 64)
	params, _ := c.FormParams()
	_, hasFeatured := params["featured_image_url"]
	_, hasHierarchy := params["parent_id"]
	return articleForm{
		ParentID:           strings.TrimSpace(c.FormValue("parent_id")),
		SortOrder:          strings.TrimSpace(c.FormValue("sort_order")),
		HasHierarchy:       hasHierarchy,
		FeaturedImage:      strings.TrimSpace(c.FormValue("featured_image_url")),
		HasFeaturedImage:   hasFeatured,
		Title:              strings.TrimSpace(c.FormValue("title")),
		Slug:               strings.TrimSpace(c.FormValue("slug")),
		Kind:               article.Kind(strings.TrimSpace(c.FormValue("kind"))),
		CategoryID:         catID,
		Excerpt:            strings.TrimSpace(c.FormValue("excerpt")),
		ContentMarkdown:    normalizeNewlines(c.FormValue("content_markdown")),
		Status:             article.StatusDraft,
		DOI:                strings.TrimSpace(c.FormValue("doi")),
		JournalName:        strings.TrimSpace(c.FormValue("journal_name")),
		ConferenceName:     strings.TrimSpace(c.FormValue("conference_name")),
		CoAuthors:          strings.TrimSpace(normalizeNewlines(c.FormValue("co_authors"))),
		PublishAt:          strings.TrimSpace(c.FormValue("publish_at")),
		Lang:               strings.TrimSpace(c.FormValue("lang")),
		TranslationGroupID: strings.TrimSpace(c.FormValue("translation_group_id")),
		SourceArticleID:    strings.TrimSpace(c.FormValue("source_article_id")),
	}
}

// withStoredState は入力 form に、保存済みの記事 a の状態（状態・公開日時・最終更新）を写す。入力の不備で編集画面を
// 出し直すとき、ボタンと予約の欄を保存済みの記事に合わせる（一度公開した記事に予約の欄を出さない）。
func withStoredState(form articleForm, a *article.Article) articleForm {
	stored := formFromArticle(a)
	form.Status, form.UpdatedAt, form.PublishedAt, form.WasPublished = stored.Status, stored.UpdatedAt, stored.PublishedAt, stored.WasPublished
	form.Lang, form.TranslationGroupID, form.SourceArticleID, form.IsOutdated = stored.Lang, stored.TranslationGroupID, stored.SourceArticleID, stored.IsOutdated
	// 種類の欄・親ページの欄が無い（閲覧専用・公開中で無効にした）送信では、保存済みの種類・階層で出し直す。
	if form.Kind == "" {
		form.Kind = stored.Kind
	}
	if !form.HasHierarchy {
		form.ParentID, form.SortOrder = stored.ParentID, stored.SortOrder
	}
	return form
}

func formFromArticle(a *article.Article) articleForm {
	now := clockNow()
	return articleForm{
		ID:                 a.ID,
		Title:              a.Title,
		Slug:               a.Slug,
		Kind:               a.Kind,
		CategoryID:         a.CategoryID,
		Excerpt:            a.Excerpt,
		ContentMarkdown:    a.ContentMarkdown,
		Status:             article.ViewStatus(a, now),
		UpdatedAt:          a.UpdatedAt,
		PublishedAt:        a.PublishedAt,
		WasPublished:       a.PublishedAt != nil && !a.PublishedAt.After(now),
		DOI:                a.DOI,
		JournalName:        a.JournalName,
		ConferenceName:     a.ConferenceName,
		CoAuthors:          a.CoAuthors,
		FeaturedImage:      a.FeaturedImage,
		Lang:               a.Lang,
		TranslationGroupID: a.TranslationGroupID,
		SourceArticleID:    a.SourceArticleID.String,
		IsOutdated:         a.IsOutdated,
		ParentID:           a.ParentID,
		SortOrder:          strconv.Itoa(a.SortOrder),
	}
}

// validFeaturedImageURL はアイキャッチ画像の URL として、空・サイト内の絶対パス・http(s) の URL だけを受け付ける。
func validFeaturedImageURL(u string) bool {
	if u == "" {
		return true
	}
	if strings.ContainsAny(u, " \t\r\n\"'<>`") {
		return false
	}
	if strings.HasPrefix(u, "/") {
		return !strings.HasPrefix(u, "//") && !strings.HasPrefix(u, "/\\")
	}
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// normalizeNewlines はブラウザが textarea から送る CRLF を LF に揃える。
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func (s *Server) listCategories() ([]category, error) {
	cats := []category{}
	if err := s.db.Select(&cats, `SELECT id, name FROM categories ORDER BY id`); err != nil {
		return nil, err
	}
	return cats, nil
}

// defaultCategoryID はページに入れる既定のカテゴリ（未分類。無ければ最初のカテゴリ）の ID を返す。カテゴリが無ければ 0。
func (s *Server) defaultCategoryID() (int64, error) {
	var ids []int64
	if err := s.db.Select(&ids, `SELECT id FROM categories ORDER BY slug = 'uncategorized' DESC, id LIMIT 1`); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return ids[0], nil
}

func (s *Server) categoryExists(id int64) (bool, error) {
	var n int
	if err := s.db.Get(&n, `SELECT COUNT(*) FROM categories WHERE id = ?`, id); err != nil {
		return false, err
	}
	return n > 0, nil
}

// countByStatus は種類 kind の記事の、画面に出す状態（article.ViewStatus）ごとの件数を返す。kind が空なら記事・ページを
// 合わせて数える。予約中の記事は公開中ではなく予約投稿（article.StatusScheduled）に数える。
func (s *Server) countByStatus(kind article.Kind, now time.Time, lang string, query string) (map[article.Status]int, error) {
	var rows []struct {
		Status article.Status `db:"status"`
		Count  int            `db:"n"`
	}
	langCond := ""
	if lang == "" {
		langCond = ` AND NOT (a.lang = 'en' AND a.translation_group_id != '' AND EXISTS (
			SELECT 1 FROM articles ja WHERE ja.translation_group_id = a.translation_group_id
			AND (ja.lang = 'ja' OR ja.lang = '' OR ja.lang IS NULL) AND ja.status != 'deleted'))`
	}
	if lang == "en" {
		langCond = " AND a.lang = 'en'"
	} else if lang == "ja" {
		langCond = " AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL)"
	}
	qCond, qArgs := articleSearchCond(query)
	args := []any{article.StatusPublished, now, article.StatusScheduled, kind, kind}
	args = append(args, qArgs...)
	if err := s.db.Select(&rows, `SELECT CASE WHEN `+article.ScheduledCondition+` THEN ? ELSE a.status END AS status, COUNT(*) AS n
		FROM articles a WHERE (? = '' OR a.kind = ?)`+langCond+qCond+` GROUP BY 1`,
		args...); err != nil {
		return nil, err
	}
	counts := make(map[article.Status]int, len(rows))
	for _, r := range rows {
		counts[r.Status] = r.Count
	}
	return counts, nil
}

// articleSearchCond は記事一覧の検索語 query で、タイトルかスラッグに query を含む記事（articles a）に絞る
// 「 AND ...」の条件と引数を返す。query が空なら条件は空。
func articleSearchCond(query string) (string, []any) {
	if query == "" {
		return "", nil
	}
	like := "%" + query + "%"
	return " AND (a.title LIKE ? OR a.slug LIKE ?)", []any{like, like}
}

// countByKind は記事の種類ごとの総件数を返す（ゴミ箱以外の総件数）。
func (s *Server) countByKind(now time.Time, lang string, query string) (map[article.Kind]int, error) {
	var rows []struct {
		Kind  article.Kind `db:"kind"`
		Count int          `db:"n"`
	}
	langCond := ""
	if lang == "" {
		langCond = ` AND NOT (a.lang = 'en' AND a.translation_group_id != '' AND EXISTS (
			SELECT 1 FROM articles ja WHERE ja.translation_group_id = a.translation_group_id
			AND (ja.lang = 'ja' OR ja.lang = '' OR ja.lang IS NULL) AND ja.status != 'deleted'))`
	}
	if lang == "en" {
		langCond = " AND a.lang = 'en'"
	} else if lang == "ja" {
		langCond = " AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL)"
	}
	qCond, qArgs := articleSearchCond(query)
	args := append([]any{article.StatusDeleted}, qArgs...)
	if err := s.db.Select(&rows, `SELECT a.kind, COUNT(*) AS n FROM articles a WHERE a.status != ?`+langCond+qCond+` GROUP BY a.kind`, args...); err != nil {
		return nil, err
	}
	counts := make(map[article.Kind]int, len(rows))
	for _, r := range rows {
		counts[r.Kind] = r.Count
	}
	return counts, nil
}

// listArticles は種類 kind・画面に出す状態 status の記事を並び順 sort（listOrder）で返す。作成者名と、有効なロックの保持者名を含む。
func (s *Server) listArticles(kind article.Kind, status article.Status, now time.Time, limit, offset int, lang string, query string, sort string) ([]articleRow, error) {
	lockedSince := time.Now().UTC().Add(-s.cfg.LockTTL)
	rows := []articleRow{}
	order := listOrder(status, sort)
	cond, condArgs := article.StatusCondition(status, now)
	langCond := ""
	if lang == "" {
		langCond = ` AND NOT (a.lang = 'en' AND a.translation_group_id != '' AND EXISTS (
			SELECT 1 FROM articles ja WHERE ja.translation_group_id = a.translation_group_id
			AND (ja.lang = 'ja' OR ja.lang = '' OR ja.lang IS NULL) AND ja.status != 'deleted'))`
	}
	if lang == "en" {
		langCond = " AND a.lang = 'en'"
	} else if lang == "ja" {
		langCond = " AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL)"
	}
	qCond, qArgs := articleSearchCond(query)
	args := append([]any{lockedSince}, condArgs...)
	args = append(args, kind)
	args = append(args, qArgs...)
	args = append(args, limit, offset)
	err := s.db.Select(&rows, `SELECT a.id, a.title, a.slug, a.status, a.kind, a.updated_at, a.published_at,
			COALESCE(NULLIF(au.display_name, ''), au.username, '') AS author_name,
			CASE WHEN a.locked_at IS NOT NULL AND julianday(a.locked_at) > julianday(?)
				THEN COALESCE(NULLIF(lu.display_name, ''), lu.username, '') ELSE '' END AS locked_by_name,
			COALESCE((SELECT SUM(v.views) FROM article_views_daily v WHERE v.article_id = a.id), 0) AS total_views,
			COALESCE(NULLIF(a.lang, ''), 'ja') AS lang,
			COALESCE(NULLIF(a.translation_group_id, ''), a.id) AS translation_group_id,
			COALESCE(a.source_article_id, '') AS source_article_id,
			a.is_outdated
		FROM articles a
		LEFT JOIN users au ON au.id = a.author_id
		LEFT JOIN users lu ON lu.id = a.locked_by
		WHERE `+cond+` AND a.kind = ?`+langCond+qCond+`
		ORDER BY `+order+`
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Status = article.ViewStatus(&article.Article{Status: rows[i].Status, PublishedAt: rows[i].PublishedAt}, now)
	}

	if len(rows) > 0 {
		groupIDs := make([]string, 0, len(rows))
		for _, r := range rows {
			if r.TranslationGroupID != "" {
				groupIDs = append(groupIDs, r.TranslationGroupID)
			}
		}
		if len(groupIDs) > 0 {
			query, qargs, err := sqlx.In(`SELECT id, lang, translation_group_id, is_outdated FROM articles WHERE translation_group_id IN (?) AND status != ?`, groupIDs, article.StatusDeleted)
			if err == nil {
				var peers []struct {
					ID                 string `db:"id"`
					Lang               string `db:"lang"`
					TranslationGroupID string `db:"translation_group_id"`
					IsOutdated         bool   `db:"is_outdated"`
				}
				if err := s.db.Select(&peers, query, qargs...); err == nil {
					type peerInfo struct {
						ID       string
						Outdated bool
					}
					peerMap := make(map[string]map[string]peerInfo)
					for _, p := range peers {
						pl := p.Lang
						if pl == "" {
							pl = "ja"
						}
						if peerMap[p.TranslationGroupID] == nil {
							peerMap[p.TranslationGroupID] = make(map[string]peerInfo)
						}
						peerMap[p.TranslationGroupID][pl] = peerInfo{ID: p.ID, Outdated: p.IsOutdated}
					}
					for i := range rows {
						gid := rows[i].TranslationGroupID
						if rows[i].Lang == "ja" {
							if en, ok := peerMap[gid]["en"]; ok {
								rows[i].HasEnTranslation = true
								rows[i].EnArticleID = en.ID
								rows[i].EnTranslationOutdated = en.Outdated
							}
						}
					}
				}
			}
		}
	}

	return rows, nil
}

// handleArticleTranslate は原文記事の一括翻訳または選択テキストの意図指定型部分翻訳・推敲を行い、結果を返す。
func (s *Server) handleArticleTranslate(c echo.Context) error {
	var req struct {
		SourceArticleID string  `json:"source_article_id" form:"source_article_id"`
		TargetLang      string  `json:"target_lang" form:"target_lang"`
		SourceLang      string  `json:"source_lang" form:"source_lang"`
		Provider        string  `json:"provider" form:"provider"`
		Text            *string `json:"text" form:"text"`
		Style           string  `json:"style" form:"style"`
		Instruction     string  `json:"instruction" form:"instruction"`
		BeforeContext   string  `json:"before_context" form:"before_context"`
		AfterContext    string  `json:"after_context" form:"after_context"`
		// Current* は記事全体の翻訳で、編集欄に今入っている内容。送られたら、翻訳で置き換える前に見せる差分を返す。
		CurrentTitle    *string `json:"current_title" form:"current_title"`
		CurrentExcerpt  *string `json:"current_excerpt" form:"current_excerpt"`
		CurrentMarkdown *string `json:"current_markdown" form:"current_markdown"`
	}
	if err := c.Bind(&req); err != nil {
		req.SourceArticleID = c.FormValue("source_article_id")
		req.TargetLang = c.FormValue("target_lang")
		req.SourceLang = c.FormValue("source_lang")
		req.Provider = c.FormValue("provider")
		if c.Request().Form.Has("text") {
			tVal := c.FormValue("text")
			req.Text = &tVal
		}
		req.Style = c.FormValue("style")
		req.Instruction = c.FormValue("instruction")
		req.BeforeContext = c.FormValue("before_context")
		req.AfterContext = c.FormValue("after_context")
	} else if req.Text == nil && c.Request().Form.Has("text") {
		tVal := c.FormValue("text")
		req.Text = &tVal
	}

	if req.TargetLang == "" {
		req.TargetLang = "en"
	}
	if req.Provider != "" && !isTranslationKind(req.Provider) {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "無効な翻訳プロバイダーです: " + req.Provider})
	}

	// 1. 部分テキストの意図指定型翻訳・推敲
	if req.Text != nil {
		trimmedText := strings.TrimSpace(*req.Text)
		if trimmedText == "" {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": "翻訳するテキストを入力してください",
			})
		}

		providerName := req.Provider
		saved, _ := s.loadAIConfig()
		var warningMsg string

		// DeepL が指定されていて自由指示（Instruction）がある場合、LLM プロバイダーが利用可能ならそちらにルーティング
		if strings.TrimSpace(req.Instruction) != "" {
			reqKind := strings.TrimSpace(req.Provider)
			if reqKind == "" {
				reqKind = saved.TranslationProvider
			}
			if reqKind == ai.KindDeepL {
				routed := false
				if saved.Provider != "" && saved.Provider != ai.KindDeepL {
					candidate, err := s.buildTranslationProvider(saved.Provider)
					if err == nil && ai.IsConfigured(candidate) {
						providerName = saved.Provider
						routed = true
					}
				}
				if !routed {
					providerName = ai.KindDeepL
					warningMsg = "DeepLでは追加指示は反映されません"
				}
			}
		}

		p, err := s.buildTranslationProvider(providerName)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "error": "翻訳プロバイダーの初期化に失敗しました: " + err.Error()})
		}
		if !ai.IsConfigured(p) {
			return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "翻訳AIのAPIキーが未設定です。「設定」→「AI」でAPIキーを設定してください"})
		}

		// サイトコンテキストの自動生成
		siteName, siteDesc := s.getSiteInfo()
		siteContext := ""
		if siteName != "" || siteDesc != "" {
			parts := make([]string, 0, 2)
			if siteName != "" {
				parts = append(parts, siteName)
			}
			if siteDesc != "" {
				parts = append(parts, siteDesc)
			}
			siteContext = strings.Join(parts, " - ")
		}

		opts := ai.TranslateOptions{
			Glossary:      parseGlossary(saved.Glossary),
			WritingStyle:  saved.WritingStyle,
			Style:         strings.TrimSpace(req.Style),
			Instruction:   strings.TrimSpace(req.Instruction),
			SiteContext:   siteContext,
			BeforeContext: strings.TrimSpace(req.BeforeContext),
			AfterContext:  strings.TrimSpace(req.AfterContext),
		}

		ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Minute)
		defer cancel()

		srcLang := strings.TrimSpace(req.SourceLang)
		if srcLang == "" {
			srcLang = "ja"
		}

		translated, err := p.Translate(ctx, trimmedText, srcLang, req.TargetLang, opts)
		if err != nil {
			c.Logger().Warnf("partial translate error: %v", err)
			_, message := aiErrorStatus(err)
			return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": "翻訳処理に失敗しました: " + message})
		}

		res := map[string]any{
			"ok":              true,
			"translated_text": translated,
		}
		if warningMsg != "" {
			res["warning"] = warningMsg
		}
		return c.JSON(http.StatusOK, res)
	}

	// 2. 記事全体の一括翻訳（従来フロー・後方互換）
	if req.SourceArticleID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "原文記事のID（source_article_id）または翻訳テキスト（text）が指定されていません"})
	}

	src, err := s.articles.GetByID(req.SourceArticleID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]any{"ok": false, "error": "原文記事が見つかりませんでした"})
	}

	p, err := s.buildTranslationProvider(req.Provider)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "error": "翻訳プロバイダーの初期化に失敗しました: " + err.Error()})
	}
	if !ai.IsConfigured(p) {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "翻訳AIのAPIキーが未設定です。「設定」→「AI」でAPIキーを設定してください"})
	}

	saved, _ := s.loadAIConfig()
	siteName, siteDesc := s.getSiteInfo()
	siteContext := ""
	if siteName != "" || siteDesc != "" {
		parts := make([]string, 0, 2)
		if siteName != "" {
			parts = append(parts, siteName)
		}
		if siteDesc != "" {
			parts = append(parts, siteDesc)
		}
		siteContext = strings.Join(parts, " - ")
	}

	opts := ai.TranslateOptions{
		Glossary:     parseGlossary(saved.Glossary),
		WritingStyle: saved.WritingStyle,
		Style:        strings.TrimSpace(req.Style),
		Instruction:  strings.TrimSpace(req.Instruction),
		SiteContext:  siteContext,
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Minute)
	defer cancel()

	srcLang := src.Lang
	if srcLang == "" {
		srcLang = "ja"
	}

	titleTrans, err := p.Translate(ctx, src.Title, srcLang, req.TargetLang, opts)
	if err != nil {
		c.Logger().Warnf("translate: title: %v", err)
		_, message := aiErrorStatus(err)
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": "タイトルの翻訳に失敗しました: " + message})
	}

	var excerptTrans string
	if src.Excerpt != "" {
		excerptTrans, err = p.Translate(ctx, src.Excerpt, srcLang, req.TargetLang, opts)
		if err != nil {
			c.Logger().Warnf("translate: excerpt: %v", err)
			_, message := aiErrorStatus(err)
			return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": "抜粋の翻訳に失敗しました: " + message})
		}
	}

	var markdownTrans string
	if src.ContentMarkdown != "" {
		markdownTrans, err = p.Translate(ctx, src.ContentMarkdown, srcLang, req.TargetLang, opts)
		if err != nil {
			c.Logger().Warnf("translate: markdown: %v", err)
			_, message := aiErrorStatus(err)
			return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": "本文の翻訳に失敗しました: " + message})
		}
	}

	res := map[string]any{
		"ok":       true,
		"title":    titleTrans,
		"excerpt":  excerptTrans,
		"markdown": markdownTrans,
	}
	// 編集欄の内容が送られたら、AI翻訳で置き換える前に本人に見せる差分を付ける（置き換えは画面で確かめてから）。
	if req.CurrentTitle != nil || req.CurrentExcerpt != nil || req.CurrentMarkdown != nil {
		res["diff"] = replaceDiff(deref(req.CurrentTitle), titleTrans, deref(req.CurrentExcerpt), excerptTrans,
			normalizeNewlines(deref(req.CurrentMarkdown)), normalizeNewlines(markdownTrans))
	}
	return c.JSON(http.StatusOK, res)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// replaceDiff は編集欄の内容（旧）を AI翻訳（新）で置き換えるときの差分を返す。HTML は ai.RenderDiffHTML でエスケープ済み。
func replaceDiff(oldTitle, newTitle, oldExcerpt, newExcerpt, oldBody, newBody string) map[string]any {
	title, excerpt, body := ai.Diff(oldTitle, newTitle), ai.Diff(oldExcerpt, newExcerpt), ai.Diff(oldBody, newBody)
	return map[string]any{
		"title_html":   ai.RenderDiffHTML(title),
		"excerpt_html": ai.RenderDiffHTML(excerpt),
		"body_html":    ai.RenderDiffHTML(body),
		"changed":      ai.HasChanges(title) || ai.HasChanges(excerpt) || ai.HasChanges(body),
	}
}
