package server

import (
	"context"
	"errors"
	"slices"
	"strings"

	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/mcp"
)

// MCP のサイト構成・メディアライブラリ・記事の取り下げのツール（internal/mcp の tools_site.go・tools_lifecycle.go）が、
// 管理画面と同じ検査・保存・後処理を使うためのつなぎ。mcp パッケージは server を参照できないため、mcp.Config の関数として渡す。

// mcpSiteStructure は今のサイト構成を MCP の形で返す（cms_get_site_context の site_structure）。
func (s *Server) mcpSiteStructure(ctx context.Context) (mcp.SiteStructure, error) {
	return toMCPSiteStructure(s.loadSiteStructure(ctx)), nil
}

// mcpUpdateSiteStructure はサイト構成の指定された項目を置き換え、管理画面・同期 API と同じ検査（validateSiteStructure）を
// 通して保存する。保存後は管理画面の保存と同じく、公開サーバーへ未反映として記録し、静的サイトへの反映の設定に従って反映する。
// 参照先の無い・未公開の項目は保存したうえで、管理画面と同じ警告の文を返す。
func (s *Server) mcpUpdateSiteStructure(ctx context.Context, upd mcp.SiteStructureUpdate) (mcp.SiteStructure, []string, error) {
	st := s.loadSiteStructure(ctx)
	if upd.SiteMenu != nil {
		st.Menu = fromMCPMenu(*upd.SiteMenu)
	}
	if upd.SiteFooterMenu != nil {
		st.FooterMenu = fromMCPMenu(*upd.SiteFooterMenu)
	}
	if upd.FrontCards != nil {
		// 管理画面のフォーム（structureFromForm）と同じく前後の空白を除き、重なったスラッグは1つにする。空のスラッグは
		// 検査で断る（黙って捨てない）。
		cards := []string{}
		for _, slug := range *upd.FrontCards {
			if slug = strings.TrimSpace(slug); slug == "" || !slices.Contains(cards, slug) {
				cards = append(cards, slug)
			}
		}
		st.FrontCards = cards
	}
	if err := s.saveSiteStructure(ctx, st); err != nil {
		var se *siteStructureError
		if errors.As(err, &se) {
			return mcp.SiteStructure{}, nil, mcp.NewToolError(se.Error())
		}
		return mcp.SiteStructure{}, nil, err
	}
	s.recordRemoteDirty(s.echo.Logger, remoteDirtyStructure)
	s.reflectAfterPublish()

	saved := s.loadSiteStructure(ctx)
	warnings, err := s.siteStructureWarnings(ctx, saved)
	if err != nil {
		// 保存は済んでいるため、警告を作れなくても成功として返す。
		s.echo.Logger.Warnf("mcp: site structure warnings: %v", err)
		warnings = nil
	}
	return toMCPSiteStructure(saved), warnings, nil
}

// siteStructureWarnings は、管理画面の「サイト構成」と同じく、参照先のページが無い・未公開の項目、無いカテゴリの項目、
// 公開中でないページのカードの警告（日本語）を返す。
func (s *Server) siteStructureWarnings(ctx context.Context, st SiteStructure) ([]string, error) {
	refs, err := s.resolveStructureRefs(ctx, st)
	if err != nil {
		return nil, err
	}
	lang := string(i18n.JA)
	var out []string
	for _, ed := range []struct {
		items  []MenuItem
		footer bool
		maxTop int
	}{{st.Menu, false, maxMenuItems}, {st.FooterMenu, true, maxFooterColumns}} {
		e := &structureMenuEditor{Lang: lang, Footer: ed.footer, MaxTop: ed.maxTop}
		out = append(out, structureRowWarnings(lang, structureRows(e, ed.items, refs), ed.footer)...)
	}
	for _, slug := range st.FrontCards {
		if _, ok := refs.Pages[slug]; !ok {
			out = append(out, i18n.Translate(lang, "structure_warn_front_card", slug))
		}
	}
	return out, nil
}

// toMCPSiteStructure はサイト構成のうち MCP で扱う項目を、空でも空の配列にして返す。
func toMCPSiteStructure(st SiteStructure) mcp.SiteStructure {
	cards := slices.Clone(st.FrontCards)
	if cards == nil {
		cards = []string{}
	}
	return mcp.SiteStructure{SiteMenu: toMCPMenu(st.Menu), SiteFooterMenu: toMCPMenu(st.FooterMenu), FrontCards: cards}
}

func toMCPMenu(items []MenuItem) []mcp.SiteMenuItem {
	out := make([]mcp.SiteMenuItem, 0, len(items))
	for _, it := range items {
		m := mcp.SiteMenuItem{Label: it.Label, LabelEn: it.LabelEn, Type: string(it.Type), Target: it.Target}
		if len(it.Children) > 0 {
			m.Children = toMCPMenu(it.Children)
		}
		out = append(out, m)
	}
	return out
}

// fromMCPMenu は MCP のメニューを保存する形にする。管理画面のフォーム（menuFromForm）と同じく各欄の前後の空白を除く。
// 子項目の深さ・種類などの検査は保存時の validateSiteStructure が行うため、ここでは形を変えない（深い入れ子も渡して断らせる）。
func fromMCPMenu(items []mcp.SiteMenuItem) []MenuItem {
	out := make([]MenuItem, 0, len(items))
	for _, it := range items {
		m := MenuItem{
			Label:   strings.TrimSpace(it.Label),
			LabelEn: strings.TrimSpace(it.LabelEn),
			Type:    MenuItemType(strings.TrimSpace(it.Type)),
			Target:  strings.TrimSpace(it.Target),
		}
		if len(it.Children) > 0 {
			m.Children = fromMCPMenu(it.Children)
		}
		out = append(out, m)
	}
	return out
}

// mcpListMedia はメディアライブラリの画像を、管理画面の一覧と同じ検索条件（mediaQuery.where）・並び順で返す。
// URL と貼り付け用の Markdown も管理画面のメディアピッカーと同じもの（newMediaItem）を使う。
func (s *Server) mcpListMedia(ctx context.Context, q mcp.MediaQuery) (mcp.MediaList, error) {
	rows, total, err := s.searchMediaRange(ctx, mediaQuery{Q: q.Query, Tag: q.Tag}, q.Limit, q.Offset)
	if err != nil {
		return mcp.MediaList{}, err
	}
	items := make([]mcp.MediaItem, 0, len(rows))
	for _, r := range rows {
		it := newMediaItem(r)
		tags := splitMediaTags(normalizeMediaTags(r.AITags))
		if tags == nil {
			tags = []string{}
		}
		items = append(items, mcp.MediaItem{
			ID: it.ID, Filename: it.Filename, URL: it.URL, ThumbnailURL: it.ThumbnailURL,
			Alt: it.Alt, Caption: it.Caption, Tags: tags, Width: it.Width, Height: it.Height,
			MimeType: it.MimeType, CreatedAt: it.CreatedAt, Markdown: it.Markdown,
		})
	}
	return mcp.MediaList{Items: items, Total: total}, nil
}

// mcpArticleUnpublished は、公開中・予約中の記事を MCP で下書きに戻した後に、管理画面で下書きに戻したとき
// （handleArticleSave）と同じく静的サイトへ反映し、公開サーバーに公開できるなら公開サーバーからも取り下げる。
func (s *Server) mcpArticleUnpublished(ctx context.Context, user *auth.User, articleID string) error {
	s.reflectAfterPublish()
	if !s.remoteSyncViewFor(user).CanPublish {
		return nil
	}
	a, err := s.articles.GetByID(articleID)
	if err != nil {
		return err
	}
	return s.unpublishRemoteArticleCtx(ctx, a)
}
