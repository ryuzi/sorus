package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// PC の sorus と公開サーバーの記事の食い違い（docs: 同期の衝突）を扱う部品。
//
//   - 提出・承認・予約では、PC が最後に取得したときの公開サーバーの更新日時（article.Article.RemoteUpdatedAt）を
//     base_updated_at として送り、その後に公開サーバーで変わっていれば公開サーバーが 409（SyncConflictCode）を返す。
//     この PC はそれを、差分（lineDiff）付きの 409 にしてブラウザへ返す（remoteSyncFailure）。
//   - 利用者が差分を見て「この PC の内容で上書き提出」を選ぶと、ブラウザは応答の server_updated_at を base_updated_at に
//     付けて送り直す（remoteBaseUpdatedAt）。
//   - ほかの人が PC で編集中（SyncCheckedOutCode）の 409 も、「引き継ぐ」（force=1）を出せるよう 409 のまま返す。

// remoteConflictView は公開サーバーの記事が PC で取得した後に変わっていたときに、ブラウザへ返す 409 の応答。
// Rows・TitleRow は左がこの PC、右が公開サーバーの今の内容の差分。
type remoteConflictView struct {
	Code                  string    `json:"code"`
	Message               string    `json:"message"`
	ArticleID             string    `json:"article_id"`
	ServerTitle           string    `json:"server_title"`
	ServerContentMarkdown string    `json:"server_content_markdown"`
	ServerUpdatedAt       time.Time `json:"server_updated_at"`
	ServerAuthorName      string    `json:"server_author_name"`
	LocalTitle            string    `json:"local_title"`
	TitleRow              diffRow   `json:"title_row"`
	Rows                  []diffRow `json:"rows"`
	Changed               bool      `json:"changed"`
}

// remoteParams はブラウザから届いた操作の値（フォームか JSON）を返す。JSON の真偽値・数値は文字列にする。
func remoteParams(c echo.Context) map[string]string {
	if v, ok := c.Get("remoteParams").(map[string]string); ok {
		return v
	}
	out := map[string]string{}
	if strings.HasPrefix(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationJSON) {
		var raw map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10)).Decode(&raw); err == nil {
			for k, v := range raw {
				switch t := v.(type) {
				case string:
					out[k] = t
				case bool:
					if t {
						out[k] = "1"
					}
				case float64:
					out[k] = fmt.Sprint(t)
				}
			}
		}
	} else {
		for _, k := range []string{"overwrite", "force", "base_updated_at", "proposal_action", "proposal_id", "publish_at"} {
			if v := c.FormValue(k); v != "" {
				out[k] = v
			}
		}
	}
	c.Set("remoteParams", out)
	return out
}

// remoteFlag は操作の値 name が「はい」（1・true）かを返す。
func remoteFlag(c echo.Context, name string) bool {
	switch strings.ToLower(strings.TrimSpace(remoteParams(c)[name])) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// remoteBaseUpdatedAt は公開サーバーへ送る base_updated_at を返す。利用者が差分を見て上書きを選んだときは、ブラウザが
// 送った base_updated_at（公開サーバーの今の更新日時）を使い、無ければこの PC の記事 local の RemoteUpdatedAt を使う。
// どちらも無ければ nil（確かめない）。形式が正しくなければ 400 を返す。
func remoteBaseUpdatedAt(c echo.Context, local *article.Article) (*time.Time, error) {
	if v := strings.TrimSpace(remoteParams(c)["base_updated_at"]); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, "base_updated_at の形式が正しくありません")
		}
		t = t.UTC()
		return &t, nil
	}
	if local != nil && local.RemoteUpdatedAt != nil && !local.RemoteUpdatedAt.IsZero() {
		t := local.RemoteUpdatedAt.UTC()
		return &t, nil
	}
	return nil, nil
}

// localArticle は id のこの PC の記事を返す。無ければ nil。
func (s *Server) localArticle(id string) (*article.Article, error) {
	a, err := s.articles.GetByID(id)
	if errors.Is(err, article.ErrNotFound) {
		return nil, nil
	}
	return a, err
}

// remoteSyncFailure は公開サーバーとの通信の失敗 err をブラウザへの応答にする。食い違い（SyncConflictCode）は
// この PC の記事 local（nil 可）との差分を付けた 409、ほかの人の編集中（SyncCheckedOutCode）は need_force 付きの 409、
// それ以外は remoteFailure（502）にする。
func remoteSyncFailure(c echo.Context, err error, local *article.Article) error {
	var re *remoteError
	if !errors.As(err, &re) || re.Status != http.StatusConflict {
		return remoteFailure(c, err)
	}
	switch re.Code {
	case SyncConflictCode:
		var sc SyncConflictResponse
		if jerr := json.Unmarshal(re.body, &sc); jerr != nil {
			return remoteFailure(c, err)
		}
		return c.JSON(http.StatusConflict, conflictView(sc, local))
	case SyncCheckedOutCode:
		var co struct {
			CheckedOutBy string `json:"checked_out_by"`
		}
		_ = json.Unmarshal(re.body, &co)
		return c.JSON(http.StatusConflict, map[string]any{
			"code":           SyncCheckedOutCode,
			"message":        re.Message,
			"checked_out_by": co.CheckedOutBy,
			"need_force":     true,
		})
	}
	return remoteFailure(c, err)
}

// conflictView は公開サーバーの食い違いの応答 sc を、この PC の記事 local（nil なら空として比べる）との差分付きにする。
func conflictView(sc SyncConflictResponse, local *article.Article) remoteConflictView {
	var localTitle, localContent string
	if local != nil {
		localTitle, localContent = local.Title, local.ContentMarkdown
	}
	rows := lineDiff(localContent, sc.ServerContentMarkdown)
	changed := localTitle != sc.ServerTitle
	for _, r := range rows {
		changed = changed || r.Op != "same"
	}
	titleOp := "same"
	if localTitle != sc.ServerTitle {
		titleOp = "change"
	}
	return remoteConflictView{
		Code:                  SyncConflictCode,
		Message:               "公開サーバー側が新しく更新されています",
		ArticleID:             sc.ArticleID,
		ServerTitle:           sc.ServerTitle,
		ServerContentMarkdown: sc.ServerContentMarkdown,
		ServerUpdatedAt:       sc.ServerUpdatedAt,
		ServerAuthorName:      sc.ServerAuthorName,
		LocalTitle:            localTitle,
		TitleRow:              diffRow{Op: titleOp, Left: localTitle, Right: sc.ServerTitle},
		Rows:                  rows,
		Changed:               changed,
	}
}

// adoptRemoteRef は公開サーバーで保存できた記事 ref を、この PC の同じ記事に写す。公開サーバーがスラッグを付け直して
// いれば（同じスラッグの記事があったなど）それに合わせ、公開サーバーの更新日時を次の base_updated_at として残す。
// この PC に記事が無ければ何もしない。操作は終わっているため、失敗してもログに残すだけにする。スラッグを写したかを返す。
func (s *Server) adoptRemoteRef(c echo.Context, ref SyncArticleRef) bool {
	ctx := c.Request().Context()
	a, err := s.localArticle(ref.ID)
	if err != nil || a == nil || ref.ID == "" {
		return false
	}
	adopted := false
	if ref.Slug != "" && ref.Slug != a.Slug {
		if err := s.articles.AdoptSlug(ctx, a.ID, ref.Slug); err != nil {
			c.Logger().Warnf("remote sync: adopt slug %q for article %s: %v", ref.Slug, a.ID, err)
		} else {
			adopted = true
			s.reindexArticle(c, a.ID)
		}
	}
	if !ref.UpdatedAt.IsZero() {
		if err := s.articles.SetRemoteUpdatedAt(ctx, a.ID, ref.UpdatedAt); err != nil {
			c.Logger().Warnf("remote sync: remember remote updated_at of article %s: %v", a.ID, err)
		}
	}
	return adopted
}

// checkoutRemote は公開サーバーの記事 id に、この PC で編集中の印を付ける（force ならほかの人の印を引き継ぐ）。
// 印を知らない古い公開サーバー（未対応の action として 400）では印を付けずに成功とする。
func (s *Server) checkoutRemote(ctx context.Context, cfg remoteConfig, id string, force bool) error {
	var out SyncCheckoutResponse
	err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionCheckout, ArticleID: id, Force: force, ClientID: s.remoteClientID(ctx)}, &out)
	var re *remoteError
	if errors.As(err, &re) && re.Status == http.StatusBadRequest {
		return nil
	}
	return err
}

// releaseRemoteCheckout は checkoutRemote で付けた印を外す（取り込みに失敗したとき）。失敗してもログに残すだけにする。
func (s *Server) releaseRemoteCheckout(c echo.Context, cfg remoteConfig, id string) {
	ctx := c.Request().Context()
	if err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionReleaseCheckout, ArticleID: id, ClientID: s.remoteClientID(ctx)}, nil); err != nil {
		c.Logger().Warnf("remote sync: release checkout of article %s: %v", id, err)
	}
}

// handleRemoteResolveProposal は公開サーバーの記事に届いている AI アプリからの変更案を、proposal_action（accept・discard）に
// 従って取り込むか破棄する（resolve_proposal）。proposal_id を付けると、確かめた案が新しい案に置き換わっていたときに断る。
// 取り込んだときは、この PC に同じ記事があり（下書き・校閲待ちで、ほかの人が編集中でなければ）その記事にも写す
// （写す前の内容は履歴に残る）。
func (s *Server) handleRemoteResolveProposal(c echo.Context) error {
	id, err := remotePendingID(c)
	if err != nil {
		return err
	}
	cfg, err := s.requireRemote(auth.CanFetchPending, "変更案の確認")
	if err != nil {
		return err
	}
	action := strings.TrimSpace(remoteParams(c)["proposal_action"])
	if action != SyncProposalAccept && action != SyncProposalDiscard {
		return echo.NewHTTPError(http.StatusBadRequest, "proposal_action には accept か discard を指定してください")
	}
	ctx := c.Request().Context()
	var out SyncResolveProposalResponse
	req := SyncRequest{Action: SyncActionResolveProposal, ArticleID: id, ProposalAction: action,
		ProposalID: strings.TrimSpace(remoteParams(c)["proposal_id"]), Force: remoteFlag(c, "force"), ClientID: s.remoteClientID(ctx)}
	if err := callRemote(ctx, cfg, req, &out); err != nil {
		return remoteSyncFailure(c, err, nil)
	}
	localUpdated := false
	if action == SyncProposalAccept {
		if localUpdated, err = s.applyRemoteToLocal(c, out.Article); err != nil {
			return err
		}
	}
	msg := "変更案を破棄しました"
	if action == SyncProposalAccept {
		msg = "変更案を反映しました"
		if localUpdated {
			msg += "（この PC の記事にも反映しました）"
		}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message":       msg,
		"action":        action,
		"local_updated": localUpdated,
		"article":       out.Article,
	})
}

// applyRemoteToLocal は公開サーバーの記事 p のタイトル・抜粋・本文を、この PC の同じ記事に写し、公開サーバーの更新日時を
// 残す。この PC に記事が無い・下書きか校閲待ちでない・ほかの人が編集中なら写さずに false を返す。
func (s *Server) applyRemoteToLocal(c echo.Context, p SyncPendingArticle) (bool, error) {
	ctx := c.Request().Context()
	a, err := s.localArticle(p.ID)
	if err != nil || a == nil {
		return false, err
	}
	if checkLocalImportable(a) != nil {
		return false, nil
	}
	user := currentUser(c)
	info, err := s.locker.Check(ctx, a.ID, user.ID)
	if err != nil {
		return false, err
	}
	if info.IsLocked && !info.IsOwnLock {
		return false, nil
	}
	// ほかの人がローカルで編集を始めていた（ロックを取れない）ときは、公開サーバーへの反映は済んでいるため写さずに続ける。
	release, err := s.lockForSync(ctx, a.ID, user.ID)
	if err != nil {
		return false, nil
	}
	defer release()
	if a, err = s.articles.GetByID(p.ID); err != nil {
		return false, err
	}
	// ロックを取るまでの間に公開・予約などで状態が変わっていれば写さない。
	if checkLocalImportable(a) != nil {
		return false, nil
	}
	s.syncSnapshot(c, a, user.ID, "変更案を反映する前の版")
	a.Title, a.Excerpt, a.ContentMarkdown = p.Title, p.Excerpt, p.ContentMarkdown
	if err := s.saveImportedArticle(c, a, s.articles.Save, p.UpdatedAt, "変更案を反映"); err != nil {
		return false, fmt.Errorf("server: remote resolve proposal: %w", err)
	}
	return true, nil
}
