package server

import "strings"

// docNav は公開サイトのドキュメント表示の部品（固定ページ・記事の左のサイドバー .doc-sidebar と、本文の後の
// 前後のページ送り .doc-pager）。サイト構成のメニュー（site_menu）で子項目を持つ最上位の項目を1つの「セクション」とみなし、
// 表示中のページがセクション（親の項目か、その子項目）にあるときだけ作る。メニューに無いページ（ふつうのブログの記事など）
// では作らず、今までどおりの1列（と目次）の見た目にする。templates/public/partials/doc_nav.html が描く。
type docNav struct {
	// Title はセクションの見出し（親の項目の表示名）。TitleURL は親の項目のリンク先で、見出しのみの親なら空。
	Title    string
	TitleURL string
	// TitleCurrent は表示中のページが親の項目そのものか（aria-current="page"）。
	TitleCurrent bool
	// Links はセクションの子項目（メニューの並び順。外部のサイトへのリンクも含む）。
	Links []docNavLink
	// Prev・Next は前後のページ。セクションの親（リンクのあるとき）と子項目をこの順に並べ、表示中のページの前後にある
	// サイト内のリンク（/ 始まり）を選ぶ。外部のサイトへのリンクと、表示中のページを指す別の項目は飛ばす。無ければ nil。
	Prev, Next *docNavLink
}

// docNavLink はサイドバー・ページ送りの1つのリンク。
type docNavLink struct {
	Label string
	URL   string
	// Current は表示中のページを指すリンクか（aria-current="page"）。
	Current bool
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
