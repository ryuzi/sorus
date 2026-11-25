package server

// 記事の添付資料（PDF など）の送信（sync_attachments の送信側。受信側は handlers_sync_attachments.go）。記事を公開サーバーへ
// 公開したあとに、その記事の添付資料の記録をすべて送り、公開サーバーに無いと言われたファイルを1件ずつ送る。PC で削除した
// 添付資料は、公開サーバーからも消してもらう（PruneAttachments）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"sorus/internal/article"
)

// errRemoteAttachmentsUnsupported は公開サーバーの sorus が sync_attachments を知らない（古い）ことを示す。
var errRemoteAttachmentsUnsupported = errors.New("remote sync: sync_attachments is not supported by the public server")

// remoteAttachmentTimeout は添付資料1件を送る要求の制限時間。30MB の PDF を遅い回線で送っても間に合う長さにする。
const remoteAttachmentTimeout = 5 * time.Minute

// remoteAttachmentResult は記事の添付資料の送信の結果。
type remoteAttachmentResult struct {
	// Local はこの PC にある、その記事の添付資料の数。
	Local int
	// Saved は公開サーバーに保存した（または同じ内容で保存済みだった）記録の数。Files はそのうち中身を送ったファイルの数。
	Saved int
	Files int
	// Removed は PC で削除したため公開サーバーから消した添付資料の数。
	Removed int
}

// remoteLocalAttachment は送る添付資料1件と、この PC のファイルのパス（無ければ空）。
type remoteLocalAttachment struct {
	SyncAttachment
	local string
}

// localAttachmentsFor は記事 articleID の添付資料を、送る記録にして返す。ファイルが PC に無い資料は SHA-256 を空にする。
func (s *Server) localAttachmentsFor(ctx context.Context, articleID string) ([]remoteLocalAttachment, error) {
	var rows []attachmentView
	if err := s.db.SelectContext(ctx, &rows, `SELECT `+attachmentColumns+` FROM attachments
		WHERE article_id = ? ORDER BY created_at, id`, articleID); err != nil {
		return nil, fmt.Errorf("server: remote attachments: list: %w", err)
	}
	out := make([]remoteLocalAttachment, 0, len(rows))
	for _, r := range rows {
		if !syncAttachmentIDPattern.MatchString(r.ID) {
			continue
		}
		a := remoteLocalAttachment{SyncAttachment: SyncAttachment{ID: r.ID, Filename: r.Filename, Size: r.FileSize, CreatedAt: r.CreatedAt}}
		if p, ok := s.attachmentPath(r.FilePath); ok {
			if sum, size, err := fileSHA256(p); err == nil {
				a.SHA256, a.Size, a.local = sum, size, p
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("server: remote attachments: read %s: %w", r.ID, err)
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// pushRemoteAttachments は記事 a の添付資料を公開サーバーへ送る。記事は公開サーバーにすでにある（publish_article の後）こと。
// remoteCount は公開サーバーにあるその記事の添付資料の数（publish_article の応答。分からなければ nil）。この PC にも
// 公開サーバーにも添付資料が無ければ、消すものも送るものも無いため要求を送らない（一括同期の要求の数を減らす）。
// 公開サーバーの sorus が sync_attachments を知らないときは errRemoteAttachmentsUnsupported を返す（記事に添付資料が
// 無ければ、古い公開サーバーでも何もせずに成功とする）。
func (s *Server) pushRemoteAttachments(ctx context.Context, cfg remoteConfig, a *article.Article, remoteCount *int) (remoteAttachmentResult, error) {
	var result remoteAttachmentResult
	local, err := s.localAttachmentsFor(ctx, a.ID)
	if err != nil {
		return result, err
	}
	result.Local = len(local)
	if len(local) == 0 && remoteCount != nil && *remoteCount == 0 {
		return result, nil
	}
	client := remoteHTTPClient
	send := func(items []SyncAttachment, prune bool) (*SyncAttachmentsResponse, error) {
		var out SyncAttachmentsResponse
		err := callRemoteWith(ctx, client, cfg, SyncRequest{Action: SyncActionSyncAttachments, ArticleID: a.ID,
			Attachments: items, PruneAttachments: prune}, &out)
		var re *remoteError
		if errors.As(err, &re) && re.Status == http.StatusBadRequest && remoteUnknownAction(re) {
			return nil, errRemoteAttachmentsUnsupported
		}
		if err != nil {
			return nil, err
		}
		if out.Action != SyncActionSyncAttachments {
			return nil, &remoteError{Status: http.StatusOK, Message: "公開サーバーの応答を読み取れませんでした。公開サーバーの sorus を更新してください"}
		}
		return &out, nil
	}

	// 1. 記録だけを送る（PC で消した資料は公開サーバーからも消してもらう）。
	records := make([]SyncAttachment, len(local))
	for i, l := range local {
		records[i] = l.SyncAttachment
	}
	out, err := send(records, true)
	if errors.Is(err, errRemoteAttachmentsUnsupported) && len(local) == 0 {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Saved, result.Removed = len(out.Saved), len(out.Removed)
	if len(out.Missing) == 0 {
		return result, nil
	}

	// 2. 公開サーバーに無いファイルを、1件ずつ中身を付けて送る。
	client = remoteAttachmentHTTPClient()
	for _, id := range out.Missing {
		var item *remoteLocalAttachment
		for i := range local {
			if local[i].ID == id {
				item = &local[i]
				break
			}
		}
		if item == nil {
			continue
		}
		if item.local == "" {
			return result, &remoteError{Status: http.StatusOK,
				Message: fmt.Sprintf("添付資料「%s」のファイルがこの PC に見つからないため、公開サーバーへ送れませんでした。記事の編集画面で添付し直してください", item.Filename)}
		}
		data, err := os.ReadFile(item.local)
		if err != nil {
			return result, fmt.Errorf("server: remote attachments: read %s: %w", item.ID, err)
		}
		rec := item.SyncAttachment
		rec.Data, rec.Size, rec.SHA256 = data, int64(len(data)), sha256Hex(data)
		res, err := send([]SyncAttachment{rec}, false)
		if err != nil {
			var re *remoteError
			if errors.As(err, &re) && re.Status == http.StatusRequestEntityTooLarge {
				re.Message = fmt.Sprintf("添付資料「%s」が大きすぎて、公開サーバーが受け取れませんでした（公開サーバーの sorus を更新するか、前段のプロキシの要求の上限を 50MB 以上にしてください）", item.Filename)
			}
			return result, err
		}
		if len(res.Saved) != 1 {
			return result, &remoteError{Status: http.StatusOK, Message: fmt.Sprintf("公開サーバーに添付資料「%s」を保存できませんでした", item.Filename)}
		}
		result.Saved++
		result.Files++
	}
	return result, nil
}

// remoteUnknownAction は公開サーバーの 400 の応答が「未対応の action」（古い公開サーバー）か。受け取る側の検査の誤りは
// 422 で返すため、400 はほぼ未対応の action だが、メッセージでも確かめる。
func remoteUnknownAction(re *remoteError) bool {
	return re.Status == http.StatusBadRequest
}

// remoteAttachmentHTTPClient は添付資料のファイルを送る HTTP クライアント（制限時間を延ばす）。
func remoteAttachmentHTTPClient() *http.Client {
	c := *remoteHTTPClient
	c.Timeout = remoteAttachmentTimeout
	return &c
}
