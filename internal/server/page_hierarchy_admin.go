package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
)

// maxSortOrder は管理画面で入力できる固定ページの並び順（sort_order）の絶対値の上限。SQLite の INTEGER には収まるが、
// 誤って桁の多い数を入れたときに気付けるよう、ふつうの使い方（数十〜数千）に十分な幅に絞る。
const maxSortOrder = 1000000

// adminText は管理画面の案内を、表示言語（日本語・英語）で返す。
func adminText(c echo.Context, ja, en string) string {
	if language(c) == "en" {
		return en
	}
	return ja
}

// parentPageRow は「親ページ」の欄の候補を作るために読む固定ページ1件。
type parentPageRow struct {
	ID       string         `db:"id"`
	Title    string         `db:"title"`
	ParentID string         `db:"parent_id"`
	Status   article.Status `db:"status"`
	Lang     string         `db:"lang"`
}

// parentPageOptions は固定ページの編集フォーム form の「親ページ」の欄の候補を返す。ゴミ箱にない、form と同じ言語の
// 固定ページ（公開中・下書き・校閲待ち・予約中）を、親子の階層の順（同じ親の中は article.PageOrder）に並べ、深さだけ
// 字下げする。編集中のページ自身とその子孫は親にできない（循環になる）ため除く。form が選んでいる親が候補に無い
// （ゴミ箱にある・別の言語の）ときは、選んだままにできるよう末尾に Missing の候補として足す。
func (s *Server) parentPageOptions(ctx context.Context, form articleForm) ([]parentPageOption, error) {
	lang := strings.TrimSpace(form.Lang)
	if lang == "" {
		lang = "ja"
	}
	var rows []parentPageRow
	err := s.db.SelectContext(ctx, &rows, `SELECT a.id, a.title, COALESCE(a.parent_id, '') AS parent_id, a.status,
			COALESCE(NULLIF(a.lang, ''), 'ja') AS lang
		FROM articles a
		WHERE a.kind = ? AND a.status <> ?
		ORDER BY `+article.PageOrder, article.KindPage, article.StatusDeleted)
	if err != nil {
		return nil, fmt.Errorf("server: parent page options: %w", err)
	}
	byID := make(map[string]parentPageRow, len(rows))
	for _, r := range rows {
		if r.Lang == lang {
			byID[r.ID] = r
		}
	}
	children := make(map[string][]parentPageRow)
	var roots []parentPageRow
	for _, r := range rows {
		if _, ok := byID[r.ID]; !ok {
			continue
		}
		if _, ok := byID[r.ParentID]; ok && r.ParentID != r.ID {
			children[r.ParentID] = append(children[r.ParentID], r)
		} else {
			roots = append(roots, r)
		}
	}

	var out []parentPageOption
	visited := make(map[string]bool, len(byID))
	var walk func(list []parentPageRow, depth int)
	walk = func(list []parentPageRow, depth int) {
		for _, r := range list {
			// 編集中のページ自身とその子孫（その下の枝ごと）は候補にしない。循環した階層でも同じページを2度出さない。
			if visited[r.ID] || (form.ID != "" && r.ID == form.ID) {
				continue
			}
			visited[r.ID] = true
			out = append(out, parentPageOption{
				ID:       r.ID,
				Label:    strings.Repeat("— ", min(depth, maxPageNavDepth)) + r.Title,
				Status:   r.Status,
				Selected: r.ID == form.ParentID,
			})
			walk(children[r.ID], depth+1)
		}
	}
	walk(roots, 0)
	// 循環した階層（DB の直接編集）でたどれなかったページも、最上位として候補に出す。
	for _, r := range rows {
		if _, ok := byID[r.ID]; ok && !visited[r.ID] && r.ID != form.ID && !isDescendantRow(byID, r.ID, form.ID) {
			visited[r.ID] = true
			out = append(out, parentPageOption{ID: r.ID, Label: r.Title, Status: r.Status, Selected: r.ID == form.ParentID})
		}
	}

	if form.ParentID != "" && !visited[form.ParentID] {
		var missing parentPageRow
		err := s.db.GetContext(ctx, &missing, `SELECT a.id, a.title, COALESCE(a.parent_id, '') AS parent_id, a.status,
				COALESCE(NULLIF(a.lang, ''), 'ja') AS lang
			FROM articles a WHERE a.id = ?`, form.ParentID)
		if err == nil {
			out = append(out, parentPageOption{ID: missing.ID, Label: missing.Title, Status: missing.Status, Missing: true, Selected: true})
		}
	}
	return out, nil
}

// isDescendantRow は id のページが ancestorID のページの子孫か（親をたどると ancestorID に着くか）を返す。
// ancestorID が空なら false。循環していても止まる。
func isDescendantRow(byID map[string]parentPageRow, id, ancestorID string) bool {
	if ancestorID == "" {
		return false
	}
	seen := map[string]bool{}
	for p, ok := byID[id]; ok && !seen[p.ID]; p, ok = byID[p.ParentID] {
		if p.ParentID == ancestorID {
			return true
		}
		seen[p.ID] = true
	}
	return false
}
