package server

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// manuscriptRemoteTimeout は「届いた原稿」の画面を開くときに、公開サーバーの校閲待ちを待つ時間の上限。
const manuscriptRemoteTimeout = 15 * time.Second

func init() {
	if !slices.Contains(pageNames, "manuscripts") {
		pageNames = append(pageNames, "manuscripts")
	}
}

// registerManuscriptRoutes は「届いた原稿」（公開サーバーの校閲待ちの原稿を確かめて、この PC に受け取る画面）を、
// ログイン済み・CSRF 保護済みの /admin グループに登録する。受け取りは編集者が行う。中身の確認と取り込みは
// 公開サーバーの画面と同じ /admin/remote/pending/:id/preview・/import を使う（handlers_remote.go）。
func (s *Server) registerManuscriptRoutes(authed *echo.Group) {
	g := authed.Group("/manuscripts")
	g.GET("", s.handleManuscriptInbox, auth.RequireRole(auth.RoleEditor))
}

// manuscriptPreview は「届いた原稿」で受け取る前に見せる、公開サーバーに届いた原稿の内容（handleRemotePreview が返す）。
type manuscriptPreview struct {
	Title      string       `json:"title"`
	Kind       article.Kind `json:"kind"`
	Category   string       `json:"category"`
	AuthorName string       `json:"author_name"`
	// AuthorNote は誰の記事として受け取るかの説明。
	AuthorNote string `json:"author_note"`
	// CreatedAt は公開サーバーに提出（更新）された日時。
	CreatedAt string `json:"created_at"`
	Images    int    `json:"images"`
	// HTML は本文のプレビュー（markdown.Render でサニタイズ済み）。
	HTML     string                  `json:"html"`
	Existing *manuscriptExistingView `json:"existing,omitempty"`
}

// manuscriptExistingView は同じ ID のローカルの記事と、届いた原稿との差分。
type manuscriptExistingView struct {
	Title       string    `json:"title"`
	StatusLabel string    `json:"status_label"`
	UpdatedAt   string    `json:"updated_at"`
	EditURL     string    `json:"edit_url"`
	NeedConfirm bool      `json:"need_confirm"`
	Reason      string    `json:"reason"`
	TitleRow    diffRow   `json:"title_row"`
	Rows        []diffRow `json:"rows"`
	Changed     bool      `json:"changed"`
}

// existingView は同じ ID のローカルの記事 a と、届いた原稿（タイトル title・本文 content）との差分を返す。
// needConfirm・reason は、上書きの前に「この内容で上書きする」の確認が要るかとその理由。
func existingView(a *article.Article, title, content string, needConfirm bool, reason string) *manuscriptExistingView {
	rows := lineDiff(a.ContentMarkdown, content)
	changed := a.Title != title
	for _, r := range rows {
		changed = changed || r.Op != "same"
	}
	titleOp := "same"
	if a.Title != title {
		titleOp = "change"
	}
	return &manuscriptExistingView{
		Title:       a.Title,
		StatusLabel: statusLabel(article.ViewStatus(a, clockNow())),
		UpdatedAt:   a.UpdatedAt.In(jst).Format("2006/01/02 15:04"),
		EditURL:     "/admin/articles/" + url.PathEscape(a.ID) + "/edit",
		NeedConfirm: needConfirm,
		Reason:      reason,
		TitleRow:    diffRow{Op: titleOp, Left: a.Title, Right: title},
		Rows:        rows,
		Changed:     changed,
	}
}

// --- 届いた原稿 ---

// inboxItem は「届いた原稿」の1件（公開サーバーの校閲待ちの記事か、作成中の下書き）。
type inboxItem struct {
	ID         string
	Title      string
	AuthorName string
	// ArrivedAt は公開サーバーに提出（更新）された日時。
	ArrivedAt time.Time
	// EditURL はローカルで校閲する記事の編集画面。まだ受け取っていなければ空。
	EditURL string
	// LocalStatus はローカルに同じ記事があればその状態。
	LocalStatus article.Status
	// IsDraft は公開サーバーで作成中の下書きか（校閲待ちでないか）。
	IsDraft bool
	// CheckedOutBy は PC の sorus でこの記事を編集中の人（有効な印があるときだけ）。
	// CheckedOutHere はその印がこの PC（remote_client_id）のものか。CheckedOutElsewhere はほかの PC の印か（ほかの人、
	// または同じ人・同じ招待で別の PC。「引き継ぐ」が要る）。CheckedOutBySelf は印の名前がこの PC の招待の名前と同じか
	// （別の PC で自分が編集中）。
	CheckedOutBy        string
	CheckedOutHere      bool
	CheckedOutElsewhere bool
	CheckedOutBySelf    bool
	// Proposal は AI アプリから届いている、確認待ちの変更案（無ければ nil）。
	Proposal *inboxProposal
}

// inboxProposal は「差分を見て反映」で見せる変更案。Lines は本文の差分（離れた同じ行はまとめる）。
type inboxProposal struct {
	ID         string
	ProposedBy string
	TitleRow   *diffRow
	ExcerptRow *diffRow
	Lines      []inboxDiffLine
}

// inboxDiffLine は変更案の差分の1行。Skipped が 0 より大きい行は、同じ行がその数だけ続くことを表す（diffRow は空）。
type inboxDiffLine struct {
	diffRow
	Skipped int
}

// proposalDiffContext は変更案の差分で、変わった行の前後に残す同じ行の数。
const proposalDiffContext = 2

// inboxProposalFor は公開サーバーの記事 p に届いている変更案を、今の記事との差分にする。案が無ければ nil。
func inboxProposalFor(p SyncPendingArticle) *inboxProposal {
	pp := p.PendingProposal
	if pp == nil {
		return nil
	}
	out := &inboxProposal{ID: pp.ID, ProposedBy: pp.ProposedBy}
	if pp.HasTitle && pp.Title != p.Title {
		out.TitleRow = &diffRow{Op: "change", Left: p.Title, Right: pp.Title}
	}
	if pp.HasExcerpt && pp.Excerpt != p.Excerpt {
		out.ExcerptRow = &diffRow{Op: "change", Left: p.Excerpt, Right: pp.Excerpt}
	}
	if pp.HasContent {
		out.Lines = compactDiff(lineDiff(p.ContentMarkdown, pp.ContentMarkdown), proposalDiffContext)
	}
	return out
}

// compactDiff は差分 rows のうち、変わった行とその前後 context 行だけを残し、離れた同じ行は数にまとめる。
// 変わった行が無ければ空を返す。
func compactDiff(rows []diffRow, context int) []inboxDiffLine {
	near := make([]bool, len(rows))
	changed := false
	for i, r := range rows {
		if r.Op == "same" {
			continue
		}
		changed = true
		for k := max(0, i-context); k <= min(len(rows)-1, i+context); k++ {
			near[k] = true
		}
	}
	if !changed {
		return nil
	}
	var out []inboxDiffLine
	skipped := 0
	for i, r := range rows {
		if !near[i] {
			skipped++
			continue
		}
		if skipped > 0 {
			out = append(out, inboxDiffLine{Skipped: skipped})
			skipped = 0
		}
		out = append(out, inboxDiffLine{diffRow: r})
	}
	if skipped > 0 {
		out = append(out, inboxDiffLine{Skipped: skipped})
	}
	return out
}

// inboxData は manuscripts.html に渡すデータ。
type inboxData struct {
	Items []inboxItem
	// Drafts は公開サーバーで作成中の下書き（下書きを返さない古い公開サーバーでは空）。
	Drafts []inboxItem
	// RemoteConnected は公開サーバーに（校閲待ちを読める招待で）参加しているか。参加していなければ、画面は参加の
	// 案内だけを出す。RemoteError は校閲待ちを取得できなかった理由。
	RemoteConnected bool
	RemoteError     string
	// CanRemotePublish は公開サーバーの記事をこの画面から公開できるか（トークンに公開の権限がある）。
	CanRemotePublish bool
	// Scheduled は公開サーバーで予約中の記事。Schedule は「日時を決めて予約」の選択肢を出すかと入力欄の値。
	Scheduled []remoteScheduledItem
	Schedule  remoteScheduleView
}

// handleManuscriptInbox は「届いた原稿」の画面。公開サーバーの校閲を待っている原稿の一覧を、「公開サーバー」の画面と
// 同じ取得処理（fetchRemoteQueue・localStatuses）で読んで出す。公開サーバーに参加していなければ、参加の案内を出す。
func (s *Server) handleManuscriptInbox(c echo.Context) error {
	ctx := c.Request().Context()
	data := inboxData{}
	if s.remoteSyncViewFor(currentUser(c)).CanReview {
		data.RemoteConnected = true
		if info, err := parseRemoteToken(s.loadRemoteConfig().Token, time.Now()); err == nil {
			data.CanRemotePublish = auth.CanPublish(info.Role)
		}
		queue, err := s.remoteInboxItems(ctx)
		if err != nil {
			c.Logger().Warnf("manuscript inbox: remote pending: %v", err)
			data.RemoteError = localizedMessage(c, "manuscript_remote_error")
		} else {
			lang := language(c)
			data.Items = queue.items
			data.Drafts = queue.drafts
			data.Scheduled = remoteScheduledItems(lang, queue.scheduled)
			data.Schedule = remoteScheduleViewFor(ctx, lang, s.loadRemoteConfig())
		}
		newestFirst := func(a, b inboxItem) int { return b.ArrivedAt.Compare(a.ArrivedAt) }
		slices.SortStableFunc(data.Items, newestFirst)
		slices.SortStableFunc(data.Drafts, newestFirst)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return s.render(c, http.StatusOK, "manuscripts", pageTitle(c, "nav_manuscripts"), data)
}

// remoteInbox は「届いた原稿」に並べる公開サーバーの記事。
type remoteInbox struct {
	items     []inboxItem
	drafts    []inboxItem
	scheduled []SyncPendingArticle
}

// remoteInboxItems は公開サーバーの校閲待ちの記事・作成中の下書きと、予約中の記事（予約を知らない古い公開サーバーでは nil）を返す。
func (s *Server) remoteInboxItems(ctx context.Context) (remoteInbox, error) {
	ctx, cancel := context.WithTimeout(ctx, manuscriptRemoteTimeout)
	defer cancel()
	cfg := s.loadRemoteConfig()
	queue, err := fetchRemoteQueue(ctx, cfg)
	if err != nil {
		return remoteInbox{}, err
	}
	local, err := s.localStatuses(ctx, append(slices.Clip(queue.Articles), queue.Drafts...))
	if err != nil {
		return remoteInbox{}, err
	}
	me := ""
	if info, err := parseRemoteToken(cfg.Token, time.Now()); err == nil {
		me = strings.TrimSpace(info.Subject)
	}
	mine := s.remoteClientID(ctx)
	toItems := func(list []SyncPendingArticle) []inboxItem {
		items := make([]inboxItem, 0, len(list))
		for _, p := range list {
			it := inboxItem{ID: p.ID, Title: p.Title, AuthorName: p.AuthorName, ArrivedAt: p.UpdatedAt, LocalStatus: local[p.ID],
				IsDraft: remotePendingIsDraft(&p), CheckedOutBy: p.CheckedOutBy, Proposal: inboxProposalFor(p)}
			if it.CheckedOutBy != "" {
				it.CheckedOutHere = checkedOutHere(p, me, mine)
				it.CheckedOutElsewhere = !it.CheckedOutHere
				it.CheckedOutBySelf = it.CheckedOutBy == me
			}
			if it.LocalStatus != "" {
				it.EditURL = "/admin/articles/" + url.PathEscape(p.ID) + "/edit"
			}
			items = append(items, it)
		}
		return items
	}
	return remoteInbox{items: toItems(queue.Articles), drafts: toItems(queue.Drafts), scheduled: queue.Scheduled}, nil
}

// checkedOutHere は、公開サーバーの記事 p の編集中の印がこの PC（識別子 mine、招待の名前 me）のものかを返す。
// 印に PC の識別子があればそれで見分ける（同じ人・同じ招待の別の PC は「ここ」ではない）。識別子を返さない古い
// 公開サーバーの印は、従来どおり名前で見分ける。
func checkedOutHere(p SyncPendingArticle, me, mine string) bool {
	if p.CheckedOutClient != "" {
		return mine != "" && p.CheckedOutClient == mine
	}
	return me != "" && p.CheckedOutBy == me
}

// manuscriptRow は manuscripts.html の1件（manuscript_inbox_item）に渡す、記事・ページ・画面のデータの組。
type manuscriptRow struct {
	// Lang はページの表示言語（部品の中で {{$ja := ne .Lang "en"}} として使う）。
	Lang string
	Item inboxItem
	Page page
	Data inboxData
}

// ManuscriptRow は「届いた原稿」の1件 it を、テンプレートの部品に渡す形にする。
func (p page) ManuscriptRow(it inboxItem) manuscriptRow {
	d, _ := p.Data.(inboxData)
	return manuscriptRow{Lang: p.Lang, Item: it, Page: p, Data: d}
}
