package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
)

// applySyncHierarchy は同期で届いた固定ページの親 parentID・並び順 sortOrder を、保存する前の記事 a に写す。
// 公開サーバーが PC からの提出・公開（syncSaveRemoteArticle）を受けるときと、PC が公開サーバーの記事を取り込むとき
// （importRemoteArticle）に使う。a.Kind は届いた種類を写した後の値であること。
//
//   - 記事（post）は親を持てないため、親を外す（固定ページを記事に変えた提出でも保存できるように）。
//   - nil は「変えない」（階層を知らない古い相手）。親の空文字は最上位にする。
//   - 親のページがこちらに無い・ゴミ箱にある・固定ページでない、または自分自身を指すときは、今の親のまま（新しいページは
//     最上位）にして保存を続ける。一括同期で子ページが親より先に届いても止めない。循環は article.Service.Save が断る。
//
// 親の ID の形式が正しくなければ 400 を返す。
func (s *Server) applySyncHierarchy(ctx context.Context, a *article.Article, parentID *string, sortOrder *int) error {
	if a.Kind != article.KindPage {
		a.ParentID = ""
		return nil
	}
	if sortOrder != nil {
		a.SortOrder = *sortOrder
	}
	if parentID == nil {
		return nil
	}
	pid := strings.TrimSpace(*parentID)
	if pid == "" {
		a.ParentID = ""
		return nil
	}
	if !syncArticleIDPattern.MatchString(pid) {
		return echo.NewHTTPError(http.StatusBadRequest, "parent_id の形式が正しくありません")
	}
	if pid == a.ID {
		return nil
	}
	ok, err := s.syncParentAvailable(ctx, pid)
	if err != nil {
		return err
	}
	if ok {
		a.ParentID = pid
	}
	return nil
}

// syncParentAvailable は id の記事が、親ページにできる（存在し、ゴミ箱になく、固定ページである）かを返す。
func (s *Server) syncParentAvailable(ctx context.Context, id string) (bool, error) {
	var parent struct {
		Kind   article.Kind   `db:"kind"`
		Status article.Status `db:"status"`
	}
	err := s.db.GetContext(ctx, &parent, `SELECT kind, status FROM articles WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("server: sync parent page: %w", err)
	}
	return parent.Kind == article.KindPage && parent.Status != article.StatusDeleted, nil
}

// syncHierarchyOf は記事 a の固定ページの階層を、同期 API で送る形（SyncArticle.ParentID・SortOrder）にする。
// 記事（post）では親を空文字（最上位）で送り、公開サーバーで固定ページから記事に変えたときに親を外させる。
func syncHierarchyOf(a *article.Article) (*string, *int) {
	parent := ""
	if a.Kind == article.KindPage {
		parent = a.ParentID
	}
	order := a.SortOrder
	return &parent, &order
}

// parentsFirst は記事の ID と親の ID の組 rows（送る順）を、親が同じ一覧にある子ページを親より後に送る順に並べ替える。
// それ以外の順は保つ。一括同期で子ページを親より先に送ると、公開サーバーで親が見つからず最上位に置かれるため。
// 循環した階層（DB の直接編集）でも止まる。
func parentsFirst(rows []idParent) []string {
	byID := make(map[string]idParent, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]string, 0, len(rows))
	done := make(map[string]bool, len(rows))
	visiting := make(map[string]bool)
	var visit func(r idParent)
	visit = func(r idParent) {
		if done[r.ID] || visiting[r.ID] {
			return
		}
		visiting[r.ID] = true
		if p, ok := byID[r.ParentID]; ok && r.ParentID != r.ID {
			visit(p)
		}
		visiting[r.ID] = false
		done[r.ID] = true
		out = append(out, r.ID)
	}
	for _, r := range rows {
		visit(r)
	}
	return out
}

// idParent は記事の ID と親の ID（無ければ空）。
type idParent struct {
	ID       string `db:"id"`
	ParentID string `db:"parent_id"`
}
