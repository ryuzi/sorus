package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/lock"
	"sorus/internal/mcp"
)

// AI アプリ（MCP）から既存の記事の内容を置き換えようとすると、記事は変えずに変更案（mcp.Proposal）として預かる。
// 編集画面では、その案と今の保存内容との差分（誰がいつ保存した内容を、誰の案でどう変えるか）を見せ、
// 「編集欄に反映」で編集欄に入れたあと、いつもの保存ボタンで保存したときにだけ記事に入る。保存のときは、
// 確かめた案がまだ同じ案か（proposal_id）、確かめたときから記事が変わっていないか（proposal_base）を確かめ、
// どちらかが違えば保存せずにもう一度確かめてもらう（新しい案は消さない）。
//
// 翻訳の案は、訳した原文の印（mcp.Proposal.SourceBase）を今の原文と比べる。案を作った後に原文が変わっていたら、
// または確かめられなければ（記録が無い・原文が見つからない・読めない）、反映する前に警告し、反映するかは利用者に任せる。
// 反映して保存しても要更新の印は外さない（古い原文の翻訳を最新として扱わない）。差分を出したときから原文が変わったら
// （proposal_source）、保存せずにもう一度確かめてもらう。

const (
	// proposalIDField・proposalBaseField は、編集欄に反映した変更案と、そのときの記事の内容の印を保存のフォームで送る欄。
	proposalIDField   = "proposal_id"
	proposalBaseField = "proposal_base"
	// proposalSourceField は、差分を出したときの今の原文の印（翻訳の案だけ。原文を確かめられなければ空）を送る欄。
	proposalSourceField = "proposal_source"
	// unknownSaver は保存した人が分からないときに出す名前（推測しない）。
	unknownSaver = "不明"
)

func init() {
	flashMessages["proposal_discarded"] = "AIアプリからの変更案を破棄しました（記事は変わっていません）"
}

// 翻訳の案が訳した原文と、今の原文との関係（proposalSource.Status）。
const (
	sourceCurrent = "current" // 案を作ったときから原文は変わっていない
	sourceChanged = "changed" // 案を作った後に原文が変わった（または別の原文から作られた）
	sourceUnknown = "unknown" // どの原文から作られたかを確かめられない
)

// proposalSource は翻訳の案が訳した原文と、今の原文との関係。
type proposalSource struct {
	Status string
	// Note は変わった・確かめられない理由（利用者に見せる）。
	Note      string
	Title     string
	UpdatedAt time.Time
	// Seen は今の原文の印（mcp.SourceFingerprint）。原文を読めなければ空。
	Seen string
}

// checkProposalSource は翻訳の案 p が訳した原文と、記事 cur の今の原文とを比べる。記録が無い・原文が見つからない・
// 読めないときは確かめられない（sourceUnknown）とし、変わっていないとは扱わない。翻訳の案でなければ nil を返す。
func (s *Server) checkProposalSource(c echo.Context, p *mcp.Proposal, cur *article.Article) *proposalSource {
	if !p.IsTranslation() {
		return nil
	}
	if p.SourceArticleID == "" {
		return &proposalSource{Status: sourceUnknown, Note: "この案がどの原文から作られたかの記録がありません"}
	}
	src, err := s.articles.GetByID(p.SourceArticleID)
	if errors.Is(err, article.ErrNotFound) {
		return &proposalSource{Status: sourceUnknown, Note: "原文の記事が見つかりません"}
	}
	if err != nil {
		c.Logger().Warnf("proposal: load source of article %s: %v", cur.ID, err)
		return &proposalSource{Status: sourceUnknown, Note: "原文の記事を読み込めませんでした"}
	}
	ps := &proposalSource{Status: sourceCurrent, Title: src.Title, UpdatedAt: src.UpdatedAt, Seen: mcp.SourceFingerprint(src)}
	switch {
	case cur.SourceArticleID.Valid && cur.SourceArticleID.String != "" && cur.SourceArticleID.String != src.ID:
		ps.Status, ps.Note = sourceChanged, "この案は、今この翻訳の原文になっている記事とは別の記事「"+src.Title+"」から作られています"
	case p.SourceBase == "":
		ps.Status, ps.Note = sourceUnknown, "案を作ったときの原文の版の記録がありません"
	case p.SourceLang == "":
		// 言語を含めずに印を作っていた古い案。言語が変わったかを確かめられない。
		ps.Status, ps.Note = sourceUnknown, "案を作ったときの原文の言語の記録がありません"
	case p.SourceLang != src.Lang:
		ps.Status, ps.Note = sourceChanged, "この案を作った後に、原文「"+src.Title+"」の言語が変更されています（"+p.SourceLang+" → "+src.Lang+"）"
	case p.SourceBase != ps.Seen:
		ps.Status, ps.Note = sourceChanged, "この案を作った後に、原文「"+src.Title+"」が変更されています"
	}
	return ps
}

// proposalFieldChange は変更案で変わるタイトル・本文・抜粋以外の項目（スラッグなど）。
type proposalFieldChange struct {
	Label, From, To string
}

// proposalView は編集画面に出す変更案（templates/admin/article_edit.html の「AIアプリからの変更案」）。
type proposalView struct {
	ID         string
	KindLabel  string
	ProposedBy string
	CreatedAt  time.Time
	// SourceTitle は翻訳の案で使った原文のタイトル（分からなければ空）。
	SourceTitle string
	// SourceStatus・SourceNote・SourceUpdatedAt は翻訳の案が訳した原文と今の原文との関係（翻訳の案でなければ空）。
	SourceStatus    string
	SourceNote      string
	SourceUpdatedAt time.Time
	// SourceSeen は差分を作ったときの今の原文の印。反映した後の保存で照らし合わせる。
	SourceSeen string
	// Base は差分を作ったときの記事の内容の印（articleFingerprint）。反映した後の保存で照らし合わせる。
	Base string
	// SavedBy・SavedAt は今のタイトル・本文を保存した人（分からなければ「不明」）と記事の最終更新日時。
	SavedBy string
	SavedAt time.Time

	HasTitle, HasExcerpt, HasBody    bool
	TitleDiff, ExcerptDiff, BodyDiff template.HTML
	Others                           []proposalFieldChange

	// 以下は「編集欄に反映」で編集欄に入れる値。Has* が false の欄は変えない。
	Title, Excerpt, Content string
	HasSlug                 bool
	Slug                    string
	CategoryID              int64
	Kind                    string
	Lang                    string
	TranslationGroupID      string
	SourceArticleID         string
}

// proposalKindLabel は変更案の種類を利用者に見せる名前にする。
func proposalKindLabel(kind string) string {
	switch kind {
	case mcp.ProposalTranslation:
		return "AIアプリからの翻訳案"
	case mcp.ProposalTranslationSync:
		return "AIアプリからの再翻訳案（原文の更新に合わせた翻訳）"
	}
	return "AIアプリからの変更案"
}

// articleFingerprint は記事の内容（変更案が置き換える項目と状態）の印を返す。秒単位の更新日時では
// 同じ秒の変更を見分けられないため、内容そのものから作る。
func articleFingerprint(a *article.Article) string {
	h := sha256.New()
	published := ""
	if a.PublishedAt != nil {
		published = a.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	for _, v := range []string{
		a.Title, a.ContentMarkdown, a.Excerpt, a.Slug, strconv.FormatInt(a.CategoryID, 10), string(a.Kind),
		string(a.Status), published, a.Lang, a.TranslationGroupID, a.SourceArticleID.String,
	} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// savedByName は記事 a の今のタイトル・本文を保存した人の名前を返す。最新の版（リビジョン）が今の内容と同じなら
// その版を記録した人、違えば（版を残さずに変わった記事など）推測せず「不明」とする。
func (s *Server) savedByName(c echo.Context, a *article.Article) (string, error) {
	revs, err := s.revisions.ListRevisions(c.Request().Context(), a.ID)
	if err != nil {
		return "", err
	}
	if len(revs) == 0 || revs[0].Title != a.Title || revs[0].ContentMarkdown != a.ContentMarkdown {
		return unknownSaver, nil
	}
	names, err := s.userNames(c, []string{revs[0].AuthorID})
	if err != nil {
		return "", err
	}
	if name := names[revs[0].AuthorID]; name != "" {
		return name, nil
	}
	return unknownSaver, nil
}

// proposalViewFor は記事 id の変更案を、保存済みの記事との差分にして返す。案が無ければ nil。
func (s *Server) proposalViewFor(c echo.Context, id string) (*proposalView, error) {
	ctx := c.Request().Context()
	p, err := mcp.LoadProposal(ctx, s.db, id)
	if err != nil || p == nil {
		return nil, err
	}
	cur, err := s.articles.GetByID(id)
	if err != nil {
		return nil, err
	}
	next := p.Changes.Apply(*cur)
	names, err := s.userNames(c, []string{p.ProposedBy})
	if err != nil {
		return nil, err
	}
	v := &proposalView{
		ID: p.ID, KindLabel: proposalKindLabel(p.Kind), ProposedBy: names[p.ProposedBy], CreatedAt: p.CreatedAt,
		Base: articleFingerprint(cur), SavedAt: cur.UpdatedAt,
		HasTitle: p.Changes.Title != nil, HasExcerpt: p.Changes.Excerpt != nil, HasBody: p.Changes.ContentMarkdown != nil,
		TitleDiff:   template.HTML(ai.RenderDiffHTML(ai.Diff(cur.Title, next.Title))),
		ExcerptDiff: template.HTML(ai.RenderDiffHTML(ai.Diff(cur.Excerpt, next.Excerpt))),
		BodyDiff:    template.HTML(ai.RenderDiffHTML(ai.Diff(cur.ContentMarkdown, next.ContentMarkdown))),
		Title:       next.Title, Excerpt: next.Excerpt, Content: next.ContentMarkdown,
	}
	if v.ProposedBy == "" {
		v.ProposedBy = unknownSaver
	}
	if v.SavedBy, err = s.savedByName(c, cur); err != nil {
		return nil, err
	}
	if ps := s.checkProposalSource(c, p, cur); ps != nil {
		v.SourceStatus, v.SourceNote, v.SourceTitle, v.SourceUpdatedAt, v.SourceSeen = ps.Status, ps.Note, ps.Title, ps.UpdatedAt, ps.Seen
	}
	ch := p.Changes
	if ch.Slug != nil {
		v.HasSlug, v.Slug = true, next.Slug
		v.Others = append(v.Others, proposalFieldChange{"スラッグ", cur.Slug, orEmpty(next.Slug, "（タイトルから作り直す）")})
	}
	if ch.CategoryID != nil {
		v.CategoryID = next.CategoryID
		v.Others = append(v.Others, proposalFieldChange{"カテゴリ", s.categoryName(cur.CategoryID), s.categoryName(next.CategoryID)})
	}
	if ch.Kind != nil {
		v.Kind = string(next.Kind)
		v.Others = append(v.Others, proposalFieldChange{"種類", kindName(cur.Kind), kindName(next.Kind)})
	}
	if ch.Lang != nil {
		v.Lang = next.Lang
		v.Others = append(v.Others, proposalFieldChange{"言語", cur.Lang, next.Lang})
	}
	if ch.TranslationGroupID != nil {
		v.TranslationGroupID = next.TranslationGroupID
		v.Others = append(v.Others, proposalFieldChange{"翻訳グループ", cur.TranslationGroupID, next.TranslationGroupID})
	}
	if ch.SourceArticleID != nil {
		v.SourceArticleID = next.SourceArticleID.String
		v.Others = append(v.Others, proposalFieldChange{"翻訳元の記事", cur.SourceArticleID.String, next.SourceArticleID.String})
	}
	return v, nil
}

func orEmpty(v, empty string) string {
	if strings.TrimSpace(v) == "" {
		return empty
	}
	return v
}

func kindName(k article.Kind) string {
	if k == article.KindPage {
		return "固定ページ"
	}
	return "記事"
}

// categoryName はカテゴリ id の名前を返す。無ければ ID を返す。
func (s *Server) categoryName(id int64) string {
	var name string
	if err := s.db.Get(&name, `SELECT name FROM categories WHERE id = ?`, id); err != nil || name == "" {
		return strconv.FormatInt(id, 10)
	}
	return name
}

// proposalConfirmation は保存のフォームが編集欄に反映した変更案。ID が空ならふだんの保存。
type proposalConfirmation struct {
	ID string
	// KeepOutdated は、翻訳の案を作った後に原文が変わっていた（または確かめられなかった）ため、保存しても
	// 要更新の印を外さない（立てる）か。
	KeepOutdated bool
	// SourceCurrent は、今の原文から作られた翻訳の案か（反映すれば要更新の印を外せる）。
	SourceCurrent bool
}

// confirmedProposal は、保存のフォームが編集欄に反映した変更案（proposal_id）を送ってきたとき、その案が今も同じ案で、
// 確かめたときから記事 a（ロックを獲得して読み直した記事）と、翻訳の案なら原文が変わっていないかを確かめる。違えば
// 保存せずに見せる文を msg に返す。案を送っていなければ空の ID を返す（ふだんの保存）。
func (s *Server) confirmedProposal(c echo.Context, a *article.Article) (conf proposalConfirmation, msg string, err error) {
	id := strings.TrimSpace(c.FormValue(proposalIDField))
	if id == "" {
		return conf, "", nil
	}
	p, err := mcp.LoadProposal(c.Request().Context(), s.db, a.ID)
	if err != nil {
		return conf, "", err
	}
	if p == nil || p.ID != id {
		return conf, "確かめた変更案は、その後に届いた新しい案に置き換わったか、破棄されました。保存していません。今の案の差分を確かめてから、もう一度反映してください", nil
	}
	if c.FormValue(proposalBaseField) != articleFingerprint(a) {
		return conf, "差分を確かめた後に、記事が変更されました。保存していません。もう一度差分を確かめてから反映してください", nil
	}
	if ps := s.checkProposalSource(c, p, a); ps != nil {
		if c.FormValue(proposalSourceField) != ps.Seen {
			if ps.Seen == "" {
				return conf, "差分を確かめた後に、原文の記事を確かめられなくなりました。保存していません。もう一度差分と原文の状態を確かめてから、反映するか判断してください", nil
			}
			return conf, "差分を確かめた後に、原文の記事が変更されました。保存していません。原文の変更を確かめてから、もう一度反映するか判断してください", nil
		}
		conf.SourceCurrent = ps.Status == sourceCurrent
		conf.KeepOutdated = !conf.SourceCurrent
	}
	conf.ID = id
	return conf, "", nil
}

// finishProposal は変更案を反映した保存のあと、その案を消す。保存は終わっているため、失敗してもログに残すだけにする。
// 確かめた後に新しい案が届いていれば消さない（confirmedProposal で弾くが、念のため案の ID で消す）。
func (s *Server) finishProposal(c echo.Context, articleID, proposalID string) {
	if proposalID == "" {
		return
	}
	if _, err := mcp.DeleteProposal(c.Request().Context(), s.db, articleID, proposalID); err != nil {
		c.Logger().Warnf("proposal: delete for article %s: %v", articleID, err)
	}
}

// proposalComment は変更案を反映して保存したときの版のメモ。
func proposalComment(comment, proposalID string) string {
	if proposalID == "" {
		return comment
	}
	return comment + "（AIアプリの変更案を反映）"
}

// handleProposalDiscard は記事の変更案を破棄する。記事は変えない。他の人が編集中なら 409、公開中・予約中の記事の案を
// 公開の権限の無い人が破棄しようとしたら 403。確かめた案が新しい案に置き換わっていれば、新しい案を消さずに 409 を返す。
func (s *Server) handleProposalDiscard(c echo.Context) (err error) {
	id := c.Param("id")
	user := currentUser(c)
	info, release, err := s.lockForAction(c, id)
	if errors.Is(err, lock.ErrAlreadyLocked) {
		return s.workflowError(c, id, http.StatusConflict, info.UserName+"さんが編集中のため、変更案を破棄できません")
	}
	if err != nil {
		return err
	}
	defer func() { release(err) }()
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if requiresPublisher(a.Status) && !canPublish(user) {
		return echo.NewHTTPError(http.StatusForbidden, "公開中・予約中の記事を変更できるのは編集長・管理者だけです")
	}
	ok, err := mcp.DeleteProposal(c.Request().Context(), s.db, id, strings.TrimSpace(c.FormValue(proposalIDField)))
	if err != nil {
		return fmt.Errorf("server: discard proposal: %w", err)
	}
	if !ok {
		return s.workflowError(c, id, http.StatusConflict, "破棄しようとした変更案は、新しい案に置き換わったか、すでに破棄されています。今の案を確かめてください")
	}
	return c.Redirect(http.StatusSeeOther, "/admin/articles/"+id+"/edit?flash=proposal_discarded")
}

// loadProposalState は編集画面に出す、変更案と今の保存内容を保存した人を data に入れる。
func (s *Server) loadProposalState(c echo.Context, data *editData) error {
	a, err := s.articles.GetByID(data.Form.ID)
	if errors.Is(err, article.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if data.SavedBy, err = s.savedByName(c, a); err != nil {
		return err
	}
	if data.ReadOnly() || data.Locked {
		p, err := mcp.LoadProposal(c.Request().Context(), s.db, a.ID)
		if err != nil {
			return err
		}
		data.ProposalWaiting = p != nil
		return nil
	}
	data.Proposal, err = s.proposalViewFor(c, a.ID)
	return err
}
