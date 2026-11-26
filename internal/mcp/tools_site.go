package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"sorus/internal/auth"
)

// maxMediaTagRunes は cms_list_media の tag の最大文字数（メディアライブラリのタグの上限と同じ）。
const maxMediaTagRunes = 100

// SiteMenuItem はナビゲーションメニュー（site_menu）・フッターのサイトマップ（site_footer_menu）の1項目。
// 管理画面の「サイト構成」が保存する JSON と同じ形。参照先はスラッグで持つ。
type SiteMenuItem struct {
	Label   string `json:"label"`
	LabelEn string `json:"label_en,omitempty"`
	// Type は page（固定ページ）・category（カテゴリの記事一覧）・articles（記事の一覧）・url（URL）・group（見出しのみ）。
	Type   string `json:"type"`
	Target string `json:"target,omitempty"`
	// Children は子項目（1階層まで）。深い入れ子は保存時の検査で断る。
	Children []SiteMenuItem `json:"children,omitempty"`
}

// SiteStructure は MCP で読み書きできるサイト構成（メニュー・フッターのサイトマップ・トップのカード）。
// 値の検査と保存は管理画面の「サイト構成」と同じ処理（Config.UpdateSiteStructure）で行う。
type SiteStructure struct {
	SiteMenu       []SiteMenuItem `json:"site_menu"`
	SiteFooterMenu []SiteMenuItem `json:"site_footer_menu"`
	FrontSections  []FrontSection `json:"front_sections"`
}

type FrontSection struct {
	Type       string   `json:"type"`
	Pages      []string `json:"pages,omitempty"`
	Tag        string   `json:"tag,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Title      string   `json:"title,omitempty"`
	Layout     string   `json:"layout,omitempty"`
	LinkURL    string   `json:"link_url,omitempty"`
	LinkLabel  string   `json:"link_label,omitempty"`
	ArticleTag string   `json:"article_tag,omitempty"`
}

// SiteStructureUpdate はサイト構成の部分的な更新。nil の項目は今の値を保つ。
type SiteStructureUpdate struct {
	SiteMenu       *[]SiteMenuItem
	SiteFooterMenu *[]SiteMenuItem
	FrontSections  *[]FrontSection
}

// MediaQuery は cms_list_media の検索条件。
type MediaQuery struct {
	// Query は空白区切りの検索語（すべてを含む画像。ファイル名・代替テキスト・キャプション・タグを探す）。
	Query string
	// Tag はタグの完全一致で絞り込む（:::gallery tag="…" で選ばれる画像）。
	Tag    string
	Limit  int
	Offset int
}

// MediaItem はメディアライブラリの画像1件。URL はサイトのルートからのパスで、本文の Markdown にそのまま書ける。
type MediaItem struct {
	ID           string    `json:"id"`
	Filename     string    `json:"filename"`
	URL          string    `json:"url"`
	ThumbnailURL string    `json:"thumbnail_url,omitempty"`
	Alt          string    `json:"alt"`
	Caption      string    `json:"caption,omitempty"`
	Tags         []string  `json:"tags"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	MimeType     string    `json:"mime_type"`
	CreatedAt    time.Time `json:"created_at"`
	// Markdown は本文に貼れる画像の構文（![alt](url "caption")）。
	Markdown string `json:"markdown"`
}

// MediaList は cms_list_media の1ページ分の画像と、条件に合う画像の総数。
type MediaList struct {
	Items []MediaItem
	Total int
}

// siteTools はサイト構成とメディアライブラリのツールを返す。
func (s *Server) siteTools() []*tool {
	menuDesc := "type は page（target: 固定ページのスラッグ）・category（target: カテゴリのスラッグ）・articles（記事の一覧。target なし）・" +
		"url（target: https:// の URL か / 始まりのサイト内パス。label 必須）・group（見出しのみ。target なし・label 必須・最上位だけに置き、children が1つ以上要る）"
	return []*tool{
		{
			info: toolInfo{
				Name:  "cms_update_site_structure",
				Title: "サイト構成の変更",
				Description: "公開サイトのナビゲーションメニュー（site_menu）・フッターのサイトマップ（site_footer_menu）・トップページのセクション（front_sections）を保存します。" +
					"指定した項目だけを置き換え、省略した項目は今の値を保ちます（空配列 [] で空にできます）。今の値は cms_get_site_context の site_structure で確認してください。" +
					"件数（メニュー最大8項目・フッター最大6列・子項目は1項目に最大10・カード最大6）・階層（子項目は1階層まで）・種類・URL・表示名は管理画面の「サイト構成」と同じ規則で検査し、" +
					"不正なら何も保存せずに理由を返します。参照先のページが無い・未公開の項目は保存したうえで warnings で知らせます（公開サイトには表示されません）。" +
					"固定ページのスラッグは cms_list_articles（kind: page, status: published）、カテゴリのスラッグは cms_get_site_context で確認できます。" +
					"編集者・管理者のみ実行できます。公開サーバー（管理画面の無いサーバー）では使えません。",
				InputSchema: objectSchema(map[string]any{
					"site_menu":        menuArraySchema("ナビゲーションメニュー（最上位の項目の配列）。"+menuDesc, true),
					"site_footer_menu": menuArraySchema("フッターのサイトマップ。最上位の項目が列（見出し）で、その children が列のリンク。"+menuDesc, true),
					"front_sections":   map[string]any{"type": "array", "description": "トップページの表示順セクション。type は hero/cards/gallery/article_list", "items": objectSchema(map[string]any{"type": enumProp("種類", "hero", "cards", "gallery", "article_list"), "pages": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "tag": stringProp("画像タグ"), "limit": integerProp("表示件数", 0, 24), "title": stringProp("見出し"), "layout": enumProp("並べ方", "justified", "masonry", "cards"), "link_url": stringProp("リンク先"), "link_label": stringProp("リンク文言"), "article_tag": stringProp("記事タグ")}, "type")},
				}),
				Annotations: &toolAnnotations{Title: "サイト構成の変更", IdempotentHint: true},
			},
			run: s.toolUpdateSiteStructure,
		},
		{
			info: toolInfo{
				Name:  "cms_list_media",
				Title: "メディアライブラリの一覧",
				Description: "メディアライブラリに登録済みの画像（ID・ファイル名・URL・代替テキスト・キャプション・タグ・幅/高さ・登録日時）を、撮影日の新しい順に一覧します。" +
					"q で検索（空白区切りの AND。ファイル名・代替テキスト・キャプション・タグを探す）、tag でタグの完全一致に絞り込めます。" +
					"既存の画像を本文に使うときは、返された markdown（![alt](url)）か url をそのまま書いてください。" +
					"タグの付いた画像は :::gallery tag=\"タグ\" と ::: で囲んで、フォトギャラリーとして並べられます。画像の登録・変更はできません。",
				InputSchema: objectSchema(map[string]any{
					"q":      stringProp(fmt.Sprintf("検索語（%d文字以内）", maxQueryRunes)),
					"tag":    stringProp(fmt.Sprintf("タグ（完全一致、%d文字以内）", maxMediaTagRunes)),
					"limit":  integerProp(fmt.Sprintf("返す件数（既定 %d、最大 %d）", defaultListLimit, maxListLimit), 1, maxListLimit),
					"offset": integerProp("先頭から飛ばす件数（ページング用）", 0, 0),
				}),
				Annotations: &toolAnnotations{Title: "メディアライブラリの一覧", ReadOnlyHint: true, IdempotentHint: true},
			},
			run: s.toolListMedia,
		},
	}
}

// menuArraySchema はメニュー項目の配列の JSON Schema を返す。withChildren なら項目に children（子項目の配列）を持たせる。
func menuArraySchema(desc string, withChildren bool) map[string]any {
	props := map[string]any{
		"label":    stringProp("表示名（空なら参照先のページのタイトル・カテゴリ名。url・group では必須）"),
		"label_en": stringProp("英語表示の表示名（任意）"),
		"type":     enumProp("項目の種類", "page", "category", "articles", "url", "group"),
		"target":   stringProp("参照先（page・category はスラッグ、url は URL。articles・group では省略）"),
	}
	if withChildren {
		props["children"] = menuArraySchema("子項目（1階層まで。子項目は children を持てず、group にできない）", false)
	}
	return map[string]any{"type": "array", "description": desc, "items": objectSchema(props, "type")}
}

type updateSiteStructureArgs struct {
	SiteMenu       *[]SiteMenuItem `json:"site_menu"`
	SiteFooterMenu *[]SiteMenuItem `json:"site_footer_menu"`
	FrontSections  *[]FrontSection `json:"front_sections"`
}

type updateSiteStructureResult struct {
	SiteStructure SiteStructure `json:"site_structure"`
	// Updated は置き換えた項目の名前（site_menu・site_footer_menu・front_sections）。
	Updated  []string `json:"updated"`
	Warnings []string `json:"warnings,omitempty"`
	Message  string   `json:"message"`
}

func (s *Server) toolUpdateSiteStructure(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	if err := publisherOnly(user, "サイト構成を変更する権限がありません（編集者・管理者のみ）"); err != nil {
		return nil, err
	}
	if s.cfg.PublicOnly {
		// 公開サーバーのサイト構成は PC の sorus から反映する（同期 API の設定の反映で上書きされる）。
		return nil, toolErrorf("公開サーバーではサイト構成を変更できません。PC の sorus の「サイト構成」で変更し、公開サーバーへ反映してください")
	}
	if s.cfg.UpdateSiteStructure == nil {
		return nil, toolErrorf("サイト構成の変更を利用できません")
	}
	var args updateSiteStructureArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	upd := SiteStructureUpdate{SiteMenu: args.SiteMenu, SiteFooterMenu: args.SiteFooterMenu, FrontSections: args.FrontSections}
	var updated []string
	if upd.SiteMenu != nil {
		updated = append(updated, "site_menu")
	}
	if upd.SiteFooterMenu != nil {
		updated = append(updated, "site_footer_menu")
	}
	if upd.FrontSections != nil {
		updated = append(updated, "front_sections")
	}
	if len(updated) == 0 {
		return nil, toolErrorf("site_menu・site_footer_menu・front_sections のうち、変更する項目を指定してください")
	}
	saved, warnings, err := s.cfg.UpdateSiteStructure(ctx, upd)
	if err != nil {
		return nil, err
	}
	return updateSiteStructureResult{
		SiteStructure: saved, Updated: updated, Warnings: warnings,
		Message: "サイト構成を保存しました（" + strings.Join(updated, "・") + "）",
	}, nil
}

type listMediaArgs struct {
	Q      string `json:"q"`
	Tag    string `json:"tag"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

type listMediaResult struct {
	Items      []MediaItem `json:"items"`
	Total      int         `json:"total"`
	Limit      int         `json:"limit"`
	Offset     int         `json:"offset"`
	NextOffset *int        `json:"next_offset,omitempty"`
}

func (s *Server) toolListMedia(ctx context.Context, _ *auth.User, raw json.RawMessage) (any, error) {
	var args listMediaArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Limit == 0 {
		args.Limit = defaultListLimit
	}
	if args.Limit < 1 || args.Limit > maxListLimit || args.Offset < 0 {
		return nil, toolErrorf("limit は 1〜%d、offset は 0 以上で指定してください", maxListLimit)
	}
	q, tag := strings.TrimSpace(args.Q), strings.TrimSpace(args.Tag)
	if utf8.RuneCountInString(q) > maxQueryRunes {
		return nil, toolErrorf("q は%d文字以内で指定してください", maxQueryRunes)
	}
	if utf8.RuneCountInString(tag) > maxMediaTagRunes {
		return nil, toolErrorf("tag は%d文字以内で指定してください", maxMediaTagRunes)
	}
	if s.cfg.ListMedia == nil {
		return nil, toolErrorf("メディアライブラリを利用できません")
	}
	list, err := s.cfg.ListMedia(ctx, MediaQuery{Query: q, Tag: tag, Limit: args.Limit, Offset: args.Offset})
	if err != nil {
		return nil, err
	}
	res := listMediaResult{Items: list.Items, Total: list.Total, Limit: args.Limit, Offset: args.Offset}
	if res.Items == nil {
		res.Items = []MediaItem{}
	}
	for i := range res.Items {
		if res.Items[i].Tags == nil {
			res.Items[i].Tags = []string{}
		}
	}
	if next := args.Offset + len(res.Items); next < res.Total {
		res.NextOffset = &next
	}
	return res, nil
}

// siteStructure は cms_get_site_context に載せる今のサイト構成を返す。Config.GetSiteStructure が無ければ nil。
func (s *Server) siteStructure(ctx context.Context) (*SiteStructure, error) {
	if s.cfg.GetSiteStructure == nil {
		return nil, nil
	}
	st, err := s.cfg.GetSiteStructure(ctx)
	if err != nil {
		return nil, fmt.Errorf("load site structure: %w", err)
	}
	if st.SiteMenu == nil {
		st.SiteMenu = []SiteMenuItem{}
	}
	if st.SiteFooterMenu == nil {
		st.SiteFooterMenu = []SiteMenuItem{}
	}
	if st.FrontSections == nil {
		st.FrontSections = []FrontSection{}
	}
	return &st, nil
}
