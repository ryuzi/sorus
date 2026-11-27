package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"sorus/internal/article"
)

// docNav は公開サイトのドキュメント表示の部品（固定ページ・記事の左のサイドバー .doc-sidebar と、本文の後の
// 前後のページ送り .doc-pager）。作り方は2つある。
//
//   - 固定ページの親子階層（articles.parent_id）: 表示中の固定ページが親ページを持つか、公開中の子ページを持つときは、
//     最上位の祖先（ルートのページ）とその子孫から作る（buildPageHierarchyNav）。サイト構成のメニューに子ページを
//     並べなくてよい。メニューより優先する。
//   - サイト構成のメニュー（site_menu）: 子項目を持つ最上位の項目を1つの「セクション」とみなし、表示中のページが
//     セクション（親の項目か、その子項目）にあるときだけ作る（buildDocNav）。
//
// どちらにも当たらないページ（ふつうのブログの記事など）では作らず、今までどおりの1列（と目次）の見た目にする。
// templates/public/partials/doc_nav.html が描く。
type docNav struct {
	// Title はセクションの見出し（親の項目の表示名）。TitleURL は親の項目のリンク先で、見出しのみの親なら空。
	Title    string
	TitleURL string
	// TitleCurrent は表示中のページが親の項目そのものか（aria-current="page"）。
	TitleCurrent bool
	// Grouped は layout.front_sidebar の全体目次サイドバー（章グループを <details class="doc-sidebar-group"> で開閉する表示）か。
	Grouped bool
	// Links はセクションの子項目（メニューの並び順。外部のサイトへのリンクも含む）。
	Links []docNavLink
	// Prev・Next は前後のページ。セクションの親（リンクのあるとき）と子項目をこの順に並べ、表示中のページの前後にある
	// サイト内のリンク（/ 始まり）を選ぶ。外部のサイトへのリンクと、表示中のページを指す別の項目は飛ばす。無ければ nil。
	Prev, Next *docNavLink
}

// docNavLink はサイドバー・ページ送り・パンくずリストの1つのリンク。
type docNavLink struct {
	Label string
	URL   string
	// Current は表示中のページを指すリンクか（aria-current="page"）。
	Current bool
	// Open は子ページを持つ親項目（<details class="doc-sidebar-group">）を開いた状態で出すか（layout.front_sidebar の全体目次）。
	Open bool
	// Children はサイドバーで入れ子にして出す子ページ（固定ページの階層だけ）。従来のセクション別サイドバーでは表示中の
	// ページとその祖先の子ページだけを開き、layout.front_sidebar の全体目次ではすべての公開固定ページの階層を入れる。
	// メニューから作るサイドバー・ページ送り・パンくずリストでは常に空。
	Children []docNavLink
}

// buildDocNav は解決済みのメニュー menu（publicNav）から、表示中のページを含む最初のセクションのサイドバーと
// ページ送りを作る。子項目の無い項目と、表示中のページを含まないセクションは見ない。該当するセクションが無ければ nil。
func buildDocNav(menu []publicMenuItem) *docNav {
	for _, sec := range menu {
		if len(sec.Children) == 0 || !(sec.Current || sec.HasCurrent) {
			continue
		}
		nav := &docNav{Title: sec.Label, TitleURL: sec.URL, TitleCurrent: sec.Current}
		var seq []docNavLink
		if sec.URL != "" {
			seq = append(seq, docNavLink{Label: sec.Label, URL: sec.URL, Current: sec.Current})
		}
		for _, ch := range sec.Children {
			l := docNavLink{Label: ch.Label, URL: ch.URL, Current: ch.Current}
			nav.Links = append(nav.Links, l)
			seq = append(seq, l)
		}
		cur := -1
		for i, l := range seq {
			if l.Current {
				cur = i
				break
			}
		}
		if cur < 0 {
			return nil
		}
		nav.Prev = pagerNeighbor(seq, cur, -1)
		nav.Next = pagerNeighbor(seq, cur, 1)
		return nav
	}
	return nil
}

// pagerNeighbor は seq の cur から step（-1・1）の向きにたどり、最初のサイト内のリンクを返す。表示中のページを指す
// 項目（同じページをセクションに2回置いたとき）と外部のサイトへのリンクは飛ばす。無ければ nil。
func pagerNeighbor(seq []docNavLink, cur, step int) *docNavLink {
	for i := cur + step; i >= 0 && i < len(seq); i += step {
		l := seq[i]
		if l.Current || !isSiteLocalURL(l.URL) {
			continue
		}
		return &l
	}
	return nil
}

// isSiteLocalURL は u がサイト内のパス（/ 始まり。// 始まりの別のホストは除く）か返す。
func isSiteLocalURL(u string) bool {
	return strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//")
}

// maxPageNavDepth は固定ページの階層ナビゲーションでたどる深さの上限。DB を直接編集して循環した階層や、極端に深い
// 階層でも、サイドバー・ページ送りを作る処理が止まるようにする（循環は訪れたページの記録でも止める）。
const maxPageNavDepth = 16

// pageNavNode は固定ページの階層ナビゲーションに使う固定ページ1件（本文を含まない軽い列）。
type pageNavNode struct {
	ID                 string `db:"id"`
	Title              string `db:"title"`
	Slug               string `db:"slug"`
	Lang               string `db:"lang"`
	TranslationGroupID string `db:"translation_group_id"`
	ParentID           string `db:"parent_id"`
}

// pageHierarchy は固定ページの階層から作った表示の部品。Nav はサイドバーとページ送り、Ancestors はパンくずリストに
// 出す祖先（ルートが先頭、親が末尾。表示中のページは含まない）。
type pageHierarchy struct {
	Nav       *docNav
	Ancestors []docNavLink
}

// buildPageHierarchyNav は固定ページの一覧 pages（同じ言語の、読者に見えるページ。article.PageOrder の順）から、
// 表示中のページ curID の階層ナビゲーションを作る。urlOf はページの公開 URL を返す。
//
// 祖先は pages の中で親をたどれる限りたどり、いちばん上の祖先（ルート）をセクションの見出しにする。親が pages に
// 無い（下書き・ゴミ箱・別の言語の）ページは、そこを最上位とみなす。サイドバーの一覧はルートの子ページ（並び順）で、
// 表示中のページとその祖先の子ページだけを入れ子で開く。ページ送りはルートから子孫を深さ優先でたどった順
// （ルート → 子1 → 子1の子 … → 子2 …）で、表示中のページの前後を選ぶ。
// 表示中のページが pages に無い、またはルートに子ページが無い（階層に入っていない）ときは空の結果を返す。
func buildPageHierarchyNav(pages []pageNavNode, curID string, urlOf func(pageNavNode) string) pageHierarchy {
	byID := make(map[string]pageNavNode, len(pages))
	for _, p := range pages {
		byID[p.ID] = p
	}
	cur, ok := byID[curID]
	if !ok {
		return pageHierarchy{}
	}
	children := make(map[string][]pageNavNode)
	for _, p := range pages {
		if p.ParentID == "" || p.ParentID == p.ID {
			continue
		}
		if _, ok := byID[p.ParentID]; ok {
			children[p.ParentID] = append(children[p.ParentID], p)
		}
	}

	// 祖先（親、親の親…）。循環していても同じページを2度たどらない。
	var chain []pageNavNode
	seen := map[string]bool{cur.ID: true}
	for p, ok := byID[cur.ParentID]; ok && !seen[p.ID] && len(chain) < maxPageNavDepth; p, ok = byID[p.ParentID] {
		seen[p.ID] = true
		chain = append(chain, p)
	}
	root := cur
	if len(chain) > 0 {
		root = chain[len(chain)-1]
	}
	if len(children[root.ID]) == 0 {
		return pageHierarchy{}
	}
	// open は子ページを開いて見せるページ（表示中のページとその祖先）。
	open := map[string]bool{cur.ID: true}
	for _, p := range chain {
		open[p.ID] = true
	}

	link := func(p pageNavNode) docNavLink {
		return docNavLink{Label: p.Title, URL: urlOf(p), Current: p.ID == cur.ID}
	}
	nav := &docNav{Title: root.Title, TitleURL: urlOf(root), TitleCurrent: root.ID == cur.ID}
	var (
		seq     = []docNavLink{link(root)}
		visited = map[string]bool{root.ID: true}
	)
	// walk は p の子ページを深さ優先でたどり、ページ送りの順 seq に足し、サイドバーの一覧（開いている枝だけ入れ子）を返す。
	var walk func(p pageNavNode, depth int) []docNavLink
	walk = func(p pageNavNode, depth int) []docNavLink {
		if depth >= maxPageNavDepth {
			return nil
		}
		var out []docNavLink
		for _, ch := range children[p.ID] {
			if visited[ch.ID] {
				continue
			}
			visited[ch.ID] = true
			l := link(ch)
			seq = append(seq, l)
			sub := walk(ch, depth+1)
			if open[ch.ID] {
				l.Children = sub
			}
			out = append(out, l)
		}
		return out
	}
	nav.Links = walk(root, 0)

	for i, l := range seq {
		if l.Current {
			nav.Prev = pagerNeighbor(seq, i, -1)
			nav.Next = pagerNeighbor(seq, i, 1)
			break
		}
	}
	h := pageHierarchy{Nav: nav}
	for i := len(chain) - 1; i >= 0; i-- {
		h.Ancestors = append(h.Ancestors, docNavLink{Label: chain[i].Title, URL: urlOf(chain[i])})
	}
	return h
}

// pageNavLangCondition は固定ページの言語を lang（"en" か、それ以外は日本語）にそろえる WHERE 条件（articles の別名 a）。
func pageNavLangCondition(lang string) string {
	if lang == "en" {
		return `a.lang = 'en'`
	}
	return `(a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL)`
}

// pageHierarchyFor は表示中の固定ページ cur の階層ナビゲーションを作る。cur が親ページを持つか、読者に見える子ページを
// 持つときだけ作り、どちらも無ければ（今までのページ・記事）DB を1回だけ確かめて空を返す。cur と同じ言語の、読者に
// 見える固定ページだけを使う。cur 自身は読者に見えなくても（下書きのプレビュー）一覧に入れる。
// トップに出しているページ（front_page）へのリンクは / （英語は /en/）にする。
func (s *Server) pageHierarchyFor(ctx context.Context, cur publicArticle, st SiteStructure) (pageHierarchy, error) {
	if cur.Kind != article.KindPage {
		return pageHierarchy{}, nil
	}
	lang := cur.Lang
	now := clockNow().UTC()
	langCond := pageNavLangCondition(lang)
	if cur.ParentID == "" {
		var hasChildren bool
		err := s.db.GetContext(ctx, &hasChildren, `SELECT EXISTS (SELECT 1 FROM articles a
			WHERE a.kind = ? AND a.parent_id = ? AND `+langCond+` AND `+publishedCondition+`)`,
			article.KindPage, cur.ID, article.StatusPublished, now)
		if err != nil {
			return pageHierarchy{}, fmt.Errorf("server: page hierarchy: children: %w", err)
		}
		if !hasChildren {
			return pageHierarchy{}, nil
		}
	}
	var pages []pageNavNode
	err := s.db.SelectContext(ctx, &pages, `SELECT a.id, a.title, a.slug,
			COALESCE(NULLIF(a.lang, ''), 'ja') AS lang,
			COALESCE(NULLIF(a.translation_group_id, ''), a.id) AS translation_group_id,
			COALESCE(a.parent_id, '') AS parent_id
		FROM articles a
		WHERE a.kind = ? AND `+langCond+` AND (`+publishedCondition+` OR a.id = ?)
		ORDER BY `+article.PageOrder, article.KindPage, article.StatusPublished, now, cur.ID)
	if err != nil {
		return pageHierarchy{}, fmt.Errorf("server: page hierarchy: pages: %w", err)
	}
	front, hasFront, err := s.frontPage(st)
	if err != nil {
		return pageHierarchy{}, err
	}
	urlOf := func(p pageNavNode) string {
		if isFrontPageOf(st, front, hasFront, p.Slug, p.TranslationGroupID) {
			if p.Lang == "en" {
				return "/en/"
			}
			return "/"
		}
		return contentPath(article.KindPage, p.Slug, p.Lang)
	}
	return buildPageHierarchyNav(pages, cur.ID, urlOf), nil
}

// isFrontPageOf はスラッグ slug・翻訳グループ groupID のページがトップに出しているページ（またはその翻訳）かを返す。
// handlePublicPage が /<slug> を / へ転送する決まりと同じ。
func isFrontPageOf(st SiteStructure, front publicArticle, hasFront bool, slug, groupID string) bool {
	if st.FrontPage != "" && st.FrontPage == slug {
		return true
	}
	return hasFront && front.TranslationGroupID != "" && front.TranslationGroupID == groupID
}

// buildFrontPageHierarchyNav は固定ページの一覧 pages（同じ言語の、読者に見えるページ。article.PageOrder の順）から、
// トップページ（frontID）の左に出す全体の階層目次サイドバーを作る。frontID 自身（および isFront が true のページ）は
// 一覧から除き、トップレベルの各ページとその子孫を .Children としてツリー化する。
// 表示できるページが1件も無ければ nil を返す（空のサイドバーを出さない）。
func buildFrontPageHierarchyNav(pages []pageNavNode, frontID, title, titleURL string, isFront func(pageNavNode) bool, urlOf func(pageNavNode) string) *docNav {
	return buildFullDocsHierarchyNav(pages, frontID, frontID, title, titleURL, isFront, urlOf)
}

// buildFullDocsHierarchyNav は固定ページの一覧 pages（同じ言語の、読者に見えるページ。article.PageOrder の順）から、
// トップページと配下の固定ページで共通の全体階層目次サイドバーを作る。frontID 自身（および isFront が true のページ）は
// 一覧から除き、トップレベルの各ページとその子孫を .Children としてツリー化する。
// curID に一致するページに Current: true を付け、curID を含む親グループ（およびその祖先、または curID 自身が子を持つ場合）に
// Open: true を付ける。トップページ（curID == frontID）表示時は TitleCurrent: true となり、各グループは閉じた状態になる。
// 表示できるページが1件も無ければ nil を返す（空のサイドバーを出さない）。
func buildFullDocsHierarchyNav(pages []pageNavNode, frontID, curID, title, titleURL string, isFront func(pageNavNode) bool, urlOf func(pageNavNode) string) *docNav {
	byID := make(map[string]pageNavNode, len(pages))
	for _, p := range pages {
		if (frontID != "" && p.ID == frontID) || (isFront != nil && isFront(p)) {
			continue
		}
		byID[p.ID] = p
	}
	if len(byID) == 0 {
		return nil
	}
	var roots []pageNavNode
	children := make(map[string][]pageNavNode)
	for _, p := range pages {
		if _, ok := byID[p.ID]; !ok {
			continue
		}
		if p.ParentID != "" && p.ParentID != p.ID {
			if _, ok := byID[p.ParentID]; ok {
				children[p.ParentID] = append(children[p.ParentID], p)
				continue
			}
		}
		roots = append(roots, p)
	}
	visited := make(map[string]bool, len(byID))
	var walk func(list []pageNavNode, depth int) ([]docNavLink, bool)
	walk = func(list []pageNavNode, depth int) ([]docNavLink, bool) {
		if depth >= maxPageNavDepth {
			return nil, false
		}
		var out []docNavLink
		anyActive := false
		for _, p := range list {
			if visited[p.ID] {
				continue
			}
			visited[p.ID] = true
			sub, subActive := walk(children[p.ID], depth+1)
			cur := curID != "" && p.ID == curID
			open := len(sub) > 0 && (cur || subActive)
			if cur || subActive {
				anyActive = true
			}
			out = append(out, docNavLink{
				Label:    p.Title,
				URL:      urlOf(p),
				Current:  cur,
				Open:     open,
				Children: sub,
			})
		}
		return out, anyActive
	}
	links, _ := walk(roots, 0)
	if len(links) == 0 {
		return nil
	}
	return &docNav{
		Title:        title,
		TitleURL:     titleURL,
		TitleCurrent: frontID != "" && curID == frontID,
		Grouped:      true,
		Links:        links,
	}
}

// frontPageHierarchyNav はトップに固定ページ front を出しているときの、公開トップページ向けの全体階層目次サイドバーを作る。
// 表示言語 lang の公開済み固定ページだけを article.PageOrder（sort_order / id）順で取得し、front 自身を除いてツリー化する。
// ほかの公開固定ページが存在しない場合は nil を返す。
func (s *Server) frontPageHierarchyNav(ctx context.Context, front publicArticle, lang string, st SiteStructure) (*docNav, error) {
	return s.fullDocsHierarchyNavWithFront(ctx, front, true, front.ID, lang, st)
}

// fullDocsHierarchyNav は layout.front_sidebar が有効なテーマで、トップページと配下の固定ページの両方に共通の
// 全体階層目次サイドバーを作る。curID は現在表示中のページの ID（トップなら front.ID、配下ページなら a.ID）。
func (s *Server) fullDocsHierarchyNav(ctx context.Context, curID, lang string, st SiteStructure) (*docNav, error) {
	front, hasFront, err := s.frontPage(st)
	if err != nil {
		return nil, err
	}
	if hasFront && lang == "en" && front.Lang != "en" {
		var enFront publicArticle
		err := s.db.GetContext(ctx, &enFront, `SELECT `+publicArticleColumns+publicArticleFrom+
			`WHERE a.translation_group_id = ? AND a.lang = 'en' AND `+publishedCondition,
			front.TranslationGroupID, article.StatusPublished, clockNow().UTC())
		switch {
		case err == nil:
			front = enFront
		case errors.Is(err, sql.ErrNoRows):
			front = publicArticle{}
			hasFront = false
		default:
			return nil, fmt.Errorf("server: front page hierarchy: en front: %w", err)
		}
	} else if hasFront && lang != "en" && front.Lang == "en" {
		front = publicArticle{}
		hasFront = false
	}
	return s.fullDocsHierarchyNavWithFront(ctx, front, hasFront, curID, lang, st)
}

func (s *Server) fullDocsHierarchyNavWithFront(ctx context.Context, front publicArticle, hasFront bool, curID, lang string, st SiteStructure) (*docNav, error) {
	now := clockNow().UTC()
	langCond := pageNavLangCondition(lang)
	var pages []pageNavNode
	err := s.db.SelectContext(ctx, &pages, `SELECT a.id, a.title, a.slug,
			COALESCE(NULLIF(a.lang, ''), 'ja') AS lang,
			COALESCE(NULLIF(a.translation_group_id, ''), a.id) AS translation_group_id,
			COALESCE(a.parent_id, '') AS parent_id
		FROM articles a
		WHERE a.kind = ? AND `+langCond+` AND (`+publishedCondition+` OR a.id = ?)
		ORDER BY `+article.PageOrder, article.KindPage, article.StatusPublished, now, curID)
	if err != nil {
		return nil, fmt.Errorf("server: front page hierarchy: pages: %w", err)
	}
	title := strings.TrimSpace(front.Title)
	if !hasFront && title == "" {
		title = strings.TrimSpace(s.siteNameFor(lang))
	}
	titleURL := "/"
	if lang == "en" {
		titleURL = "/en/"
		if title == "" {
			title = "Documentation"
		}
	} else if title == "" {
		title = "ドキュメント"
	}
	frontID := ""
	if hasFront {
		frontID = front.ID
	}
	isFront := func(p pageNavNode) bool {
		return isFrontPageOf(st, front, hasFront, p.Slug, p.TranslationGroupID)
	}
	urlOf := func(p pageNavNode) string {
		return contentPath(article.KindPage, p.Slug, p.Lang)
	}
	return buildFullDocsHierarchyNav(pages, frontID, curID, title, titleURL, isFront, urlOf), nil
}
