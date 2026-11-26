package server

// 公開中の記事・添付資料・フォトギャラリーの写真を、公開サーバーへまとめて送る「一括同期」（「公開先」の公開サーバーの画面）。
//
// 記事はふだん、公開したときに1件ずつ公開サーバーへ送られる（publishRemoteArticle）。データの取り込みや、この PC で
// まとめて直した記事・後から付けた添付資料は、記事を1件ずつ保存し直さないと届かない。一括同期はこの PC で公開中の記事を
// すべて（画像・添付資料とともに）送り、フォトギャラリーの写真も送る。送信はサーバーの中で続け、画面は進み具合を読むだけに
// する（画面を閉じても止まらない）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// remoteBulkDeadline は一括同期の全体の制限時間。
const remoteBulkDeadline = 3 * time.Hour

// remoteBulkFailure は一括同期で送れなかったもの1件。Kind は gallery（フォトギャラリーの写真）・article（記事）・
// attachments（記事は送れたが添付資料を送れなかった）。Message は公開サーバー（またはこの PC）が返した理由。
type remoteBulkFailure struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message"`
}

// 一括同期の結果（remoteBulkStatus.Outcome）。画面の文は表示言語の辞書から組み立てる（js_bulk_result_*）。
const (
	remoteBulkOK      = "ok"
	remoteBulkPartial = "partial"
	remoteBulkStopped = "stopped"
	remoteBulkTimeout = "timeout"
)

// 一括同期で知らせておくこと（remoteBulkStatus.Notes。表示言語の辞書のキー）。
const (
	remoteBulkNoteMediaOld       = "js_bulk_note_media_old"
	remoteBulkNoteAttachmentsOld = "js_bulk_note_attachments_old"
)

// remoteBulkStatus は一括同期の状態（/admin/remote/bulk-sync の応答）。
type remoteBulkStatus struct {
	Running bool `json:"running"`
	// Started は一度でも一括同期を始めたか（この sorus を起動してから）。
	Started bool `json:"started"`
	// Total・Done は送る記事の数と、送り終えた（失敗を含む）数。Percent はその割合。Current は送っている記事の題名。
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Percent int    `json:"percent"`
	Current string `json:"current,omitempty"`
	// Waiting は公開サーバーの受付上限（429）のため、送り直すまで待っている秒数（待っていなければ 0）。
	Waiting int `json:"waiting,omitempty"`
	// Articles は公開サーバーへ公開できた記事、Images は送った画像ファイル、Attachments は公開サーバーに保存した添付資料、
	// Media は送ったフォトギャラリーの写真の記録の数。
	Articles    int `json:"articles"`
	Images      int `json:"images"`
	Attachments int `json:"attachments"`
	Media       int `json:"media"`
	// Failures は送れなかったもの、Notes は知らせておくこと（公開サーバーが古い など。辞書のキー）。
	Failures []remoteBulkFailure `json:"failures"`
	Notes    []string            `json:"notes"`
	// Outcome は終わったときの結果（ok・partial・stopped・timeout。送っている間は空）。StopReason は途中でやめた理由
	// （公開サーバーが招待を受け付けなかった など）、FinishedText は終わった日時。
	Outcome      string `json:"outcome,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	FinishedText string `json:"finished_text,omitempty"`
}

// remoteBulkState は一括同期の状態を持つ（Server.remoteBulk）。
type remoteBulkState struct {
	mu sync.Mutex
	st remoteBulkStatus
}

func (b *remoteBulkState) snapshot() remoteBulkStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.st
	out.Failures = append([]remoteBulkFailure{}, b.st.Failures...)
	out.Notes = append([]string{}, b.st.Notes...)
	if out.Total > 0 {
		out.Percent = out.Done * 100 / out.Total
	}
	return out
}

func (b *remoteBulkState) update(f func(st *remoteBulkStatus)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(&b.st)
}

// remoteBulkPlan は一括同期で送るものの数（画面に出す）。
type remoteBulkPlan struct {
	Articles    int
	Attachments int
}

// remoteBulkArticleIDs はこの PC で公開中（読者に見えている）の記事・固定ページの ID を、公開日時の古い順に返す。
// ただし親ページも送る子ページは親の後にする（parentsFirst。公開サーバーで子ページを親の下に置けるように）。
func (s *Server) remoteBulkArticleIDs(ctx context.Context) ([]string, error) {
	var rows []idParent
	err := s.db.SelectContext(ctx, &rows, `SELECT a.id, COALESCE(a.parent_id, '') AS parent_id FROM articles a WHERE `+publishedCondition+`
		ORDER BY julianday(a.published_at), a.id`, article.StatusPublished, clockNow().UTC())
	if err != nil {
		return nil, fmt.Errorf("server: remote bulk sync: list articles: %w", err)
	}
	return parentsFirst(rows), nil
}

// remoteBulkPlanFor は一括同期で送る記事と添付資料の数を数える。
func (s *Server) remoteBulkPlanFor(ctx context.Context) remoteBulkPlan {
	var p remoteBulkPlan
	_ = s.db.GetContext(ctx, &p.Articles, `SELECT COUNT(*) FROM articles a WHERE `+publishedCondition, article.StatusPublished, clockNow().UTC())
	_ = s.db.GetContext(ctx, &p.Attachments, `SELECT COUNT(*) FROM attachments at JOIN articles a ON a.id = at.article_id
		WHERE `+publishedCondition, article.StatusPublished, clockNow().UTC())
	return p
}

// handleRemoteBulkStatus は一括同期の状態を返す（GET /admin/remote/bulk-sync）。
func (s *Server) handleRemoteBulkStatus(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, s.remoteBulk.snapshot())
}

// handleRemoteBulkStart は一括同期を始める（POST /admin/remote/bulk-sync）。いまの公開方式が公開サーバーで、招待が
// 直接公開できる（editor・admin）ときだけ始める。すでに送っている最中なら 409。
func (s *Server) handleRemoteBulkStart(c echo.Context) error {
	cfg, err := s.requireRemote(auth.CanPublish, "記事の一括同期")
	if err != nil {
		return err
	}
	lang := string(language(c))
	if !s.remotePublishActive() {
		return c.JSON(http.StatusConflict, map[string]string{"message": translateOr(lang, "remote_bulk_standby")})
	}
	ids, err := s.remoteBulkArticleIDs(c.Request().Context())
	if err != nil {
		return err
	}
	started := false
	s.remoteBulk.update(func(st *remoteBulkStatus) {
		if st.Running {
			return
		}
		*st = remoteBulkStatus{Running: true, Started: true, Total: len(ids), Failures: []remoteBulkFailure{}, Notes: []string{}}
		started = true
	})
	if !started {
		return c.JSON(http.StatusConflict, map[string]string{"message": translateOr(lang, "remote_bulk_running")})
	}
	go s.runRemoteBulkSync(cfg, ids)
	return c.JSON(http.StatusAccepted, s.remoteBulk.snapshot())
}

// remoteFailureText は公開サーバーとの通信・この PC の読み込みの失敗を、利用者に見せる文にする。
func remoteFailureText(err error) string {
	var re *remoteError
	var he *echo.HTTPError
	switch {
	case errors.As(err, &re):
		return re.Message
	case errors.As(err, &he):
		if msg, ok := he.Message.(string); ok {
			return msg
		}
	}
	return "この PC のデータを読み込めませんでした"
}

// remoteAuthFailure は、公開サーバーが招待（トークン）を受け付けなかった（取り消し・期限切れ・権限不足）ための失敗か。
// その場合は、残りの記事を送っても同じく断られるため一括同期をやめる。
func remoteAuthFailure(err error) bool {
	var re *remoteError
	return errors.As(err, &re) && (re.Status == http.StatusUnauthorized || re.Status == http.StatusForbidden)
}

// runRemoteBulkSync は一括同期を行う（handleRemoteBulkStart が別の goroutine で呼ぶ）。先にフォトギャラリーの写真を送り
// （公開した記事のギャラリーが最初からそろうように）、公開中の記事を古い順に、画像・添付資料とともに1件ずつ送る。
// 1件の失敗では止めずに次の記事へ進み、送れなかったものを最後に並べる。
func (s *Server) runRemoteBulkSync(cfg remoteConfig, ids []string) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteBulkDeadline)
	defer cancel()
	// 公開サーバーの受付上限（1分あたりの要求の数）に当たったら、期間が明けるまで待って送り直し、最後まで送る。
	// 待っているあいだは、進み具合の画面に待ち時間を出す。
	ctx = withRemotePatience(ctx, func(wait time.Duration) {
		secs := int((wait + time.Second - 1) / time.Second)
		s.remoteBulk.update(func(st *remoteBulkStatus) { st.Waiting = secs })
	})
	logger := s.echo.Logger
	var notes []string
	addNote := func(note string) {
		for _, n := range notes {
			if n == note {
				return
			}
		}
		notes = append(notes, note)
	}

	// 1. フォトギャラリーの写真（トップページのギャラリーと、公開中の記事の :::gallery のタグ）。
	tags, err := s.remoteGalleryTags(ctx, s.loadSiteStructure(ctx).TopGalleryTag)
	if err == nil && len(tags) > 0 {
		s.remoteBulk.update(func(st *remoteBulkStatus) { st.Current = "" })
		var res remoteMediaResult
		res, err = s.pushRemoteMedia(ctx, cfg, tags)
		switch {
		case errors.Is(err, errRemoteMediaUnsupported):
			addNote(remoteBulkNoteMediaOld)
			err = nil
		case err == nil:
			s.remoteBulk.update(func(st *remoteBulkStatus) { st.Media = res.Records })
		}
	}
	if err != nil {
		logger.Warnf("remote bulk sync: gallery media: %v", err)
		s.remoteBulk.update(func(st *remoteBulkStatus) {
			st.Failures = append(st.Failures, remoteBulkFailure{Kind: "gallery", Message: remoteFailureText(err)})
		})
		if remoteAuthFailure(err) {
			s.finishRemoteBulkSync(notes, remoteBulkStopped, remoteFailureText(err))
			return
		}
	}

	// 2. 公開中の記事（画像・添付資料とともに）。
	for _, id := range ids {
		if ctx.Err() != nil {
			s.finishRemoteBulkSync(notes, remoteBulkTimeout, "")
			return
		}
		a, err := s.articles.GetByID(id)
		if err != nil || article.ViewStatus(a, clockNow()) != article.StatusPublished {
			// 送る間に非公開にした・削除した記事は送らない。
			s.remoteBulk.update(func(st *remoteBulkStatus) { st.Done++ })
			continue
		}
		s.remoteBulk.update(func(st *remoteBulkStatus) { st.Current = a.Title })
		out, err := s.sendRemoteArticle(ctx, cfg, a, remoteSendOptions{SkipGallery: true}, logger.Warnf)
		if err != nil {
			logger.Warnf("remote bulk sync: article %s: %v", a.ID, err)
		}
		s.remoteBulk.update(func(st *remoteBulkStatus) {
			st.Done++
			if err != nil {
				st.Failures = append(st.Failures, remoteBulkFailure{Kind: "article", ID: a.ID, Title: a.Title, Message: remoteFailureText(err)})
				return
			}
			st.Articles++
			st.Images += out.Images
			st.Attachments += out.Attachments.Saved
			if out.AttachmentsErr != nil {
				st.Failures = append(st.Failures, remoteBulkFailure{Kind: "attachments", ID: a.ID, Title: a.Title,
					Message: remoteFailureText(out.AttachmentsErr)})
			}
		})
		if out.AttachmentsUnsupported {
			addNote(remoteBulkNoteAttachmentsOld)
		}
		if err != nil && remoteAuthFailure(err) {
			s.finishRemoteBulkSync(notes, remoteBulkStopped, remoteFailureText(err))
			return
		}
	}
	s.finishRemoteBulkSync(notes, "", "")
}

// finishRemoteBulkSync は一括同期を終え、結果を決める。stopped は途中でやめたときの結果（remoteBulkStopped・
// remoteBulkTimeout。最後まで送ったなら空）、reason はその理由。
func (s *Server) finishRemoteBulkSync(notes []string, stopped, reason string) {
	s.remoteBulk.update(func(st *remoteBulkStatus) {
		st.Running = false
		st.Current = ""
		st.Waiting = 0
		st.Notes = append(st.Notes, notes...)
		st.FinishedText = clockNow().In(jst).Format(displayTimeLayout)
		switch {
		case stopped != "":
			st.Outcome, st.StopReason = stopped, reason
		case len(st.Failures) > 0:
			st.Outcome = remoteBulkPartial
		default:
			st.Outcome = remoteBulkOK
		}
	})
}
