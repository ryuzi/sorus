package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/i18n"
)

// MenuItemType はナビゲーションメニューの項目の種類。
type MenuItemType string

const (
	// MenuItemPage は固定ページ（Target はページのスラッグ）。
	MenuItemPage MenuItemType = "page"
	// MenuItemCategory はカテゴリの記事一覧（Target はカテゴリのスラッグ）。
	MenuItemCategory MenuItemType = "category"
	// MenuItemArticles は記事の一覧（/articles）。Target は持たない。
	MenuItemArticles MenuItemType = "articles"
	// MenuItemURL は外部 URL（https://）かサイト内のパス（/ 始まり）。Label は必須。
	MenuItemURL MenuItemType = "url"
	// MenuItemGroup はリンクを持たない見出し（Target は持たず、Label は必須）。子項目（Children）をまとめるためだけに
	// 使い、公開サイトのメニューではドロップダウンを開くボタン、フッターのサイトマップでは列の見出しになる。
	// 最上位だけに置け、子項目が1つ以上要る。
	MenuItemGroup MenuItemType = "group"
)

// menuItemTypes は管理画面の選択肢に出す順の、既知の種類。
var menuItemTypes = []MenuItemType{MenuItemPage, MenuItemCategory, MenuItemArticles, MenuItemURL, MenuItemGroup}

// MenuItem はナビゲーションメニューの1項目。参照先はスラッグで持ち、ローカルの CMS と公開サーバーで ID が違っても通じる。
// Label が空なら参照先のページのタイトル・カテゴリ名を使う。
type MenuItem struct {
	Label   string       `json:"label"`
	LabelEn string       `json:"label_en,omitempty"`
	Type    MenuItemType `json:"type"`
	Target  string       `json:"target,omitempty"`
	// Children はこの項目の下にまとめる子項目（公開サイトのメニューではドロップダウン、フッターのサイトマップでは列の
	// リンク）。1階層だけで、子項目は Children を持たない（validateMenuTree）。子項目の無い項目は JSON に出さず、
	// 子項目を知らない古いメニュー（フラットな JSON）もそのまま読める。
	Children []MenuItem `json:"children,omitempty"`
}

// FrontSectionType はトップページに表示するセクションの種類。
type FrontSectionType string

const (
	FrontSectionHero        FrontSectionType = "hero"
	FrontSectionCards       FrontSectionType = "cards"
	FrontSectionGallery     FrontSectionType = "gallery"
	FrontSectionArticleList FrontSectionType = "article_list"
	maxFrontSections                         = 12
)

// FrontSection はトップページの1つのセクション。
type FrontSection struct {
	Type           FrontSectionType `json:"type"`
	Pages          []string         `json:"pages,omitempty"`
	Tag            string           `json:"tag,omitempty"`
	Limit          int              `json:"limit,omitempty"`
	Title          string           `json:"title,omitempty"`
	Layout         string           `json:"layout,omitempty"`
	LinkURL        string           `json:"link_url,omitempty"`
	LinkLabel      string           `json:"link_label,omitempty"`
	SecondaryURL   string           `json:"secondary_url,omitempty"`
	SecondaryLabel string           `json:"secondary_label,omitempty"`
	ArticleTag     string           `json:"article_tag,omitempty"`
}

// SiteStructure はサイト構成（メニュー・フッターのサイトマップ・トップページ）。
type SiteStructure struct {
	Menu []MenuItem
	// FooterMenu はフッターのサイトマップ。最上位の項目が列（見出し）で、その子項目が列のリンク。空ならサイトマップを
	// 出さない。読み込んだ値は、空なら nil。
	FooterMenu    []MenuItem
	FrontPage     string
	FrontSections []FrontSection
}

// フォトギャラリーの並べ方（トップページの gallery セクションと partials/gallery.html の Layout）。
const (
	// galleryLayoutJustified は写真の縦横比を保ったまま行の高さをそろえる行揃え（既定・おすすめ）。
	galleryLayoutJustified = "justified"
	// galleryLayoutMasonry は縦横比を保ったまま段に詰める段組み。
	galleryLayoutMasonry = "masonry"
	// galleryLayoutCards は余白のある枠の下にキャプションを添えるカード。
	galleryLayoutCards = "cards"
)

// galleryLayouts は選べる並べ方（画面に出す順）。
var galleryLayouts = []string{galleryLayoutJustified, galleryLayoutMasonry, galleryLayoutCards}

// normalizeGalleryLayout は並べ方を選べる値にする。空・知らない値は既定の行揃え。
func normalizeGalleryLayout(layout string) string {
	layout = strings.TrimSpace(layout)
	if slices.Contains(galleryLayouts, layout) {
		return layout
	}
	return galleryLayoutJustified
}

// clearUnusedFrontSectionFields はセクションの種別で使わないフィールドをゼロ値にする。
func clearUnusedFrontSectionFields(section FrontSection) FrontSection {
	switch section.Type {
	case FrontSectionHero:
		return FrontSection{
			Type:           FrontSectionHero,
			LinkURL:        section.LinkURL,
			LinkLabel:      section.LinkLabel,
			SecondaryURL:   section.SecondaryURL,
			SecondaryLabel: section.SecondaryLabel,
		}
	case FrontSectionCards:
		return FrontSection{
			Type:  FrontSectionCards,
			Pages: section.Pages,
		}
	case FrontSectionArticleList:
		return FrontSection{
			Type:       FrontSectionArticleList,
			Title:      section.Title,
			ArticleTag: section.ArticleTag,
		}
	case FrontSectionGallery:
		return FrontSection{
			Type:      FrontSectionGallery,
			Tag:       section.Tag,
			Title:     section.Title,
			Limit:     section.Limit,
			Layout:    section.Layout,
			LinkURL:   section.LinkURL,
			LinkLabel: section.LinkLabel,
		}
	default:
		return section
	}
}

const (
	// maxMenuItems はメニューの最上位の最大項目数。スマートフォンでも迷わない数にとどめる。
	maxMenuItems = 8
	// maxMenuChildren は1つの項目にまとめる子項目（ドロップダウン・フッターの列のリンク）の最大数。
	maxMenuChildren = 10
	// maxFooterColumns はフッターのサイトマップの最大の列数。
	maxFooterColumns = 6
	// maxFrontCards はトップに並べるページのカードの最大数。
	maxFrontCards        = 6
	maxGalleryTagRunes   = 100
	maxGalleryTitleRunes = 100
	// maxGalleryLinkLabelRunes はトップページのフォトギャラリーの「すべて見る」リンクの文言の最大文字数。
	maxGalleryLinkLabelRunes = 40
	// maxTopGalleryLimit はトップページのフォトギャラリーに出す最大の枚数。画面の選択肢（topGalleryLimitOptions）の
	// 最大と同じにする。
	maxTopGalleryLimit = 24
	// maxMenuLabelRunes はメニューの表示名の最大文字数。
	maxMenuLabelRunes = 40
	// maxStructureSlugRunes は参照するスラッグの最大文字数。
	maxStructureSlugRunes = 200
	// maxMenuURLBytes はメニューの URL の最大バイト数。
	maxMenuURLBytes = 2000
	// frontNewsCount はトップページの新着記事の件数。
	frontNewsCount = 6

	settingSiteMenu = "site_menu"
	// settingSiteFooterMenu はフッターのサイトマップ（MenuItem の JSON の配列）。
	settingSiteFooterMenu = "site_footer_menu"
	settingFrontPage      = "front_page"
	settingFrontSections  = "front_sections"
)

// topGalleryLimitOptions はサイト構成の画面で選べる、トップページのフォトギャラリーの枚数（小さい順）。最後は
// サーバーの上限 maxTopGalleryLimit と同じにし、画面で選べる枚数と保存できる枚数をそろえる。
var topGalleryLimitOptions = []int{4, 6, 8, 12, 16, maxTopGalleryLimit}

func frontSectionGalleryTag(sections []FrontSection) string {
	for _, section := range sections {
		if section.Type == FrontSectionGallery {
			return section.Tag
		}
	}
	return ""
}

// siteStructureError はサイト構成の検証エラー。管理画面は Key を表示言語で翻訳して出し、Error は日本語の文言を返す。
type siteStructureError struct {
	// Row はエラーのあるメニューの行（最上位の項目の1始まりの番号）。メニュー以外のエラーは 0。
	Row int
	// Child はエラーのある子項目の1始まりの番号（Row の項目の何番目の子項目か）。最上位の項目のエラーは 0。
	Child int
	// Footer はフッターのサイトマップ（FooterMenu）のエラーか。false ならメニュー（Menu）。
	Footer bool
	Key    string
	Args   []any
}

func (e *siteStructureError) Error() string { return e.localize(string(i18n.JA)) }

// localize は表示言語 lang の文言を返す。
func (e *siteStructureError) localize(lang string) string {
	msg := i18n.Translate(lang, e.Key, e.Args...)
	switch {
	case e.Row > 0 && e.Child > 0 && e.Footer:
		return i18n.Translate(lang, "structure_err_footer_child_row", e.Row, e.Child, msg)
	case e.Row > 0 && e.Footer:
		return i18n.Translate(lang, "structure_err_footer_row", e.Row, msg)
	case e.Row > 0 && e.Child > 0:
		return i18n.Translate(lang, "structure_err_menu_child_row", e.Row, e.Child, msg)
	case e.Row > 0:
		return i18n.Translate(lang, "structure_err_menu_row", e.Row, msg)
	}
	return msg
}

func structureErr(key string, args ...any) error { return &siteStructureError{Key: key, Args: args} }

// validateSiteStructure はサイト構成の形を検査する（件数・既知の種類・URL・表示名・制御文字）。
// 参照先のページ・カテゴリがあるかは描画時に確かめ、無ければ公開側に出さない（ここではエラーにしない）。
func validateSiteStructure(st SiteStructure) error {
	if len(st.FrontSections) > maxFrontSections {
		return structureErr("structure_err_sections_count", maxFrontSections)
	}
	for _, section := range st.FrontSections {
		section = clearUnusedFrontSectionFields(section)
		switch section.Type {
		case FrontSectionHero, FrontSectionCards, FrontSectionGallery, FrontSectionArticleList:
		default:
			return structureErr("structure_err_section_type")
		}
		if len(section.Pages) > maxFrontCards {
			return structureErr("structure_err_cards_count", maxFrontCards)
		}
		for _, slug := range section.Pages {
			if slug == "" {
				return structureErr("structure_err_target_required")
			}
			if err := validateStructureSlug(slug); err != nil {
				return err
			}
		}
		if section.Tag != "" && (utf8.RuneCountInString(section.Tag) > maxGalleryTagRunes || hasControl(section.Tag)) {
			return structureErr("structure_err_gallery_tag")
		}
		if section.Type == FrontSectionGallery && (section.Limit < 0 || section.Limit > maxTopGalleryLimit) {
			return structureErr("structure_err_gallery_limit", maxTopGalleryLimit)
		}
		if utf8.RuneCountInString(section.Title) > maxGalleryTitleRunes || hasControl(section.Title) {
			return structureErr("structure_err_gallery_title", maxGalleryTitleRunes)
		}
		if section.Layout != "" && !slices.Contains(galleryLayouts, section.Layout) {
			return structureErr("structure_err_gallery_layout")
		}
		if section.LinkURL != "" && !validGalleryLinkURL(section.LinkURL) {
			return structureErr("structure_err_gallery_link_url")
		}
		if utf8.RuneCountInString(section.LinkLabel) > maxGalleryLinkLabelRunes || hasControl(section.LinkLabel) {
			return structureErr("structure_err_gallery_link_label", maxGalleryLinkLabelRunes)
		}
		if section.SecondaryURL != "" && !validGalleryLinkURL(section.SecondaryURL) {
			return structureErr("structure_err_gallery_link_url")
		}
		if utf8.RuneCountInString(section.SecondaryLabel) > maxGalleryLinkLabelRunes || hasControl(section.SecondaryLabel) {
			return structureErr("structure_err_gallery_link_label", maxGalleryLinkLabelRunes)
		}
		if section.ArticleTag != "" && (utf8.RuneCountInString(section.ArticleTag) > maxGalleryTagRunes || hasControl(section.ArticleTag)) {
			return structureErr("structure_err_gallery_tag")
		}
	}
	if err := validateMenuTree(st.Menu, false); err != nil {
		return err
	}
	if err := validateMenuTree(st.FooterMenu, true); err != nil {
		return err
	}
	if st.FrontPage != "" {
		if err := validateStructureSlug(st.FrontPage); err != nil {
			return err
		}
	}
	return nil
}

// validateMenuTree はメニュー（footer ならフッターのサイトマップ）の項目と子項目の形を検査する。子項目は1階層だけ
// （子項目は Children を持たない）で、見出しのみの項目は最上位に置き、子項目を1つ以上持つ。エラーには何番目の項目・
// 子項目かを付ける。
func validateMenuTree(items []MenuItem, footer bool) error {
	maxTop, countKey := maxMenuItems, "structure_err_menu_count"
	if footer {
		maxTop, countKey = maxFooterColumns, "structure_err_footer_count"
	}
	if len(items) > maxTop {
		return &siteStructureError{Key: countKey, Args: []any{maxTop}, Footer: footer}
	}
	at := func(err error, row, child int) error {
		var se *siteStructureError
		if errors.As(err, &se) {
			se.Row, se.Child, se.Footer = row, child, footer
		}
		return err
	}
	for i, it := range items {
		if err := validateMenuItem(it); err != nil {
			return at(err, i+1, 0)
		}
		if it.Type == MenuItemGroup && len(it.Children) == 0 {
			return at(structureErr("structure_err_group_empty"), i+1, 0)
		}
		if len(it.Children) > maxMenuChildren {
			return at(structureErr("structure_err_children_count", maxMenuChildren), i+1, 0)
		}
		for j, ch := range it.Children {
			if err := validateMenuChild(ch); err != nil {
				return at(err, i+1, j+1)
			}
		}
	}
	return nil
}

// validateMenuChild は子項目の形を検査する。validateMenuItem に加えて、孫項目（2階層目の Children）と見出しのみの
// 項目を断る。公開側の描画でも、保存値が改ざんされていた子項目を出さないために使う。
func validateMenuChild(it MenuItem) error {
	if len(it.Children) > 0 {
		return structureErr("structure_err_menu_depth")
	}
	if it.Type == MenuItemGroup {
		return structureErr("structure_err_group_child")
	}
	return validateMenuItem(it)
}

// validateMenuItem はメニューの1項目の形を検査する（子項目は見ない。validateMenuTree）。公開側の描画でも、保存値が
// 改ざんされていた項目を出さないために使う。
func validateMenuItem(it MenuItem) error {
	if utf8.RuneCountInString(it.Label) > maxMenuLabelRunes {
		return structureErr("structure_err_label_long", maxMenuLabelRunes)
	}
	if hasControl(it.Label) {
		return structureErr("structure_err_control")
	}
	if utf8.RuneCountInString(it.LabelEn) > maxMenuLabelRunes {
		return structureErr("structure_err_label_long", maxMenuLabelRunes)
	}
	if hasControl(it.LabelEn) {
		return structureErr("structure_err_control")
	}
	switch it.Type {
	case MenuItemPage, MenuItemCategory:
		if it.Target == "" {
			return structureErr("structure_err_target_required")
		}
		return validateStructureSlug(it.Target)
	case MenuItemArticles:
		if it.Target != "" {
			return structureErr("structure_err_articles_target")
		}
		return nil
	case MenuItemURL:
		if strings.TrimSpace(it.Label) == "" {
			return structureErr("structure_err_label_required")
		}
		if !validMenuURL(it.Target) {
			return structureErr("structure_err_url")
		}
		return nil
	case MenuItemGroup:
		if strings.TrimSpace(it.Label) == "" {
			return structureErr("structure_err_group_label")
		}
		if it.Target != "" {
			return structureErr("structure_err_group_target")
		}
		return nil
	}
	return structureErr("structure_err_type", string(it.Type))
}

// validateStructureSlug は参照するスラッグの形（長さ・制御文字・区切り文字）を検査する。
func validateStructureSlug(slug string) error {
	if utf8.RuneCountInString(slug) > maxStructureSlugRunes {
		return structureErr("structure_err_slug_long", maxStructureSlugRunes)
	}
	if hasControl(slug) {
		return structureErr("structure_err_control")
	}
	if strings.ContainsAny(slug, "/\\ ") {
		return structureErr("structure_err_slug", slug)
	}
	return nil
}

// validMenuURL は u がメニューに出してよい URL か返す。https:// の絶対 URL か、"/" 始まりのサイト内パス
// （"//host" のように別ホストと解釈されるものは除く）だけを受け付ける。javascript: などのスキームは通さない。
func validMenuURL(u string) bool {
	if u == "" || len(u) > maxMenuURLBytes || hasControl(u) || strings.ContainsAny(u, " \\\"'<>`") {
		return false
	}
	if strings.HasPrefix(u, "/") {
		return isLocalPath(u)
	}
	if !strings.HasPrefix(u, "https://") {
		return false
	}
	p, err := url.Parse(u)
	return err == nil && p.Scheme == "https" && p.Host != "" && p.User == nil
}

// validGalleryLinkURL は u がトップページのフォトギャラリーの「すべて見る」リンクに出してよい URL か返す。メニューの
// URL（validMenuURL）と同じく "/" 始まりのサイト内パスか https:// の絶対 URL を受け付け、加えて http:// の絶対 URL も
// 受け付ける（既存の外部ギャラリーへのリンク用）。javascript: などのスキーム・"//host" は通さない。
func validGalleryLinkURL(u string) bool {
	if rest, ok := strings.CutPrefix(u, "http://"); ok {
		return validMenuURL("https://" + rest)
	}
	return validMenuURL(u)
}

// hasControl は s が制御文字（改行・タブを含む）を含むか返す。
func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// loadSiteStructure は保存済みのサイト構成を返す。壊れた JSON は空として読む（公開サイトを止めない）。
func (s *Server) loadSiteStructure(ctx context.Context) SiteStructure {
	var rows []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	var st SiteStructure
	if err := s.db.SelectContext(ctx, &rows, `SELECT key, value FROM settings WHERE key IN (?, ?, ?, ?)`, settingSiteMenu, settingSiteFooterMenu, settingFrontPage, settingFrontSections); err != nil {
		return st
	}
	for _, r := range rows {
		switch r.Key {
		case settingSiteMenu:
			if json.Unmarshal([]byte(r.Value), &st.Menu) != nil {
				st.Menu = nil
			}
		case settingSiteFooterMenu:
			if json.Unmarshal([]byte(r.Value), &st.FooterMenu) != nil || len(st.FooterMenu) == 0 {
				st.FooterMenu = nil
			}
		case settingFrontPage:
			st.FrontPage = strings.TrimSpace(r.Value)
		case settingFrontSections:
			if json.Unmarshal([]byte(r.Value), &st.FrontSections) != nil {
				st.FrontSections = nil
			} else {
				for i := range st.FrontSections {
					section := &st.FrontSections[i]
					*section = clearUnusedFrontSectionFields(*section)
					if section.Type == FrontSectionGallery && (section.Layout == "" || !slices.Contains(galleryLayouts, section.Layout)) {
						section.Layout = galleryLayoutJustified
					}
					if section.LinkURL != "" && !validGalleryLinkURL(section.LinkURL) {
						section.LinkURL = ""
					}
					if hasControl(section.LinkLabel) || utf8.RuneCountInString(section.LinkLabel) > maxGalleryLinkLabelRunes {
						section.LinkLabel = ""
					}
					if section.SecondaryURL != "" && !validGalleryLinkURL(section.SecondaryURL) {
						section.SecondaryURL = ""
					}
					if hasControl(section.SecondaryLabel) || utf8.RuneCountInString(section.SecondaryLabel) > maxGalleryLinkLabelRunes {
						section.SecondaryLabel = ""
					}
				}
			}
		}
	}
	if st.FrontSections == nil {
		if st.FrontPage != "" {
			st.FrontSections = []FrontSection{{Type: FrontSectionHero}, {Type: FrontSectionArticleList}}
		} else {
			st.FrontSections = []FrontSection{{Type: FrontSectionArticleList}}
		}
	}
	return st
}

// remoteNestedMenuRejectedNote は、公開サーバーがサイト構成を断ったときに、メニューの子項目・見出しのみの項目・
// フッターのサイトマップを使っていれば添える文（古い公開サーバーは見出しのみの項目を未知の種類として断る）。
const remoteNestedMenuRejectedNote = "公開サーバーの sorus が古いと、メニューのドロップダウン（子項目）・見出しのみの項目・フッターのサイトマップを受け取れません。公開サーバーの sorus を更新してから、もう一度反映してください。"

// usesNestedMenus はサイト構成が、子項目・見出しのみの項目・フッターのサイトマップ（古い公開サーバーが知らない
// 機能）を使っているか返す。
func (st SiteStructure) usesNestedMenus() bool {
	if len(st.FooterMenu) > 0 {
		return true
	}
	return slices.ContainsFunc(st.Menu, func(it MenuItem) bool { return len(it.Children) > 0 || it.Type == MenuItemGroup })
}

// saveSiteStructureTx はサイト構成を検査し、指定されたトランザクション内でキーを保存する。
func (s *Server) saveSiteStructureTx(ctx context.Context, tx *sqlx.Tx, st SiteStructure) error {
	if st.FrontSections == nil {
		if st.FrontPage != "" {
			st.FrontSections = []FrontSection{{Type: FrontSectionHero}, {Type: FrontSectionArticleList}}
		} else {
			st.FrontSections = []FrontSection{{Type: FrontSectionArticleList}}
		}
	}
	for i := range st.FrontSections {
		st.FrontSections[i] = clearUnusedFrontSectionFields(st.FrontSections[i])
	}
	if err := validateSiteStructure(st); err != nil {
		return err
	}
	for i := range st.FrontSections {
		section := &st.FrontSections[i]
		section.Tag = strings.TrimSpace(section.Tag)
		section.Title = strings.TrimSpace(section.Title)
		if section.Type == FrontSectionGallery {
			section.Layout = normalizeGalleryLayout(section.Layout)
		}
		section.LinkURL = strings.TrimSpace(section.LinkURL)
		section.LinkLabel = strings.TrimSpace(section.LinkLabel)
		section.SecondaryURL = strings.TrimSpace(section.SecondaryURL)
		section.SecondaryLabel = strings.TrimSpace(section.SecondaryLabel)
		section.ArticleTag = strings.TrimSpace(section.ArticleTag)
		*section = clearUnusedFrontSectionFields(*section)
	}
	menu, footer := st.Menu, st.FooterMenu
	if menu == nil {
		menu = []MenuItem{}
	}
	if footer == nil {
		footer = []MenuItem{}
	}
	sections := st.FrontSections
	if sections == nil {
		sections = []FrontSection{}
	}
	mj, err := json.Marshal(menu)
	if err != nil {
		return err
	}
	fj, err := json.Marshal(footer)
	if err != nil {
		return err
	}
	sj, err := json.Marshal(sections)
	if err != nil {
		return err
	}
	for _, kv := range [][2]string{{settingSiteMenu, string(mj)}, {settingSiteFooterMenu, string(fj)}, {settingFrontPage, st.FrontPage}, {settingFrontSections, string(sj)}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// saveSiteStructure はサイト構成を検査し、3つのキーを1つのトランザクションで保存する。
func (s *Server) saveSiteStructure(ctx context.Context, st SiteStructure) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.saveSiteStructureTx(ctx, tx, st); err != nil {
		return err
	}
	return tx.Commit()
}

// structurePageRef は固定ページのスラッグとタイトル。
type structurePageRef struct {
	Slug  string
	Title string
}

// structureRefs はサイト構成が参照する公開中のページ（スラッグ → タイトル）と、あるカテゴリ（スラッグ → 名前）を返す。
// EnPages は翻訳クラスタで紐づく英語固定ページ（スラッグ → 英語スラッグ・英語タイトル）。
type structureRefs struct {
	Pages      map[string]string
	Categories map[string]string
	EnPages    map[string]structurePageRef
}

// resolveStructureRefs は st が参照するスラッグのうち、公開中のページと、あるカテゴリを読む。
func (s *Server) resolveStructureRefs(ctx context.Context, st SiteStructure) (structureRefs, error) {
	refs := structureRefs{
		Pages:      map[string]string{},
		Categories: map[string]string{},
		EnPages:    map[string]structurePageRef{},
	}
	var pageSlugs, catSlugs []string
	collect := func(items []MenuItem) {
		for _, it := range items {
			switch it.Type {
			case MenuItemPage:
				pageSlugs = append(pageSlugs, it.Target)
			case MenuItemCategory:
				catSlugs = append(catSlugs, it.Target)
			}
			// 子項目は1階層だけ（validateMenuTree）。改ざんされた保存値の深い入れ子は、公開側では出さないため読まない。
			for _, ch := range it.Children {
				switch ch.Type {
				case MenuItemPage:
					pageSlugs = append(pageSlugs, ch.Target)
				case MenuItemCategory:
					catSlugs = append(catSlugs, ch.Target)
				}
			}
		}
	}
	collect(st.Menu)
	collect(st.FooterMenu)
	if st.FrontPage != "" {
		pageSlugs = append(pageSlugs, st.FrontPage)
	}
	for _, section := range st.FrontSections {
		pageSlugs = append(pageSlugs, section.Pages...)
	}

	if len(pageSlugs) > 0 {
		type pageRow struct {
			Slug               string `db:"slug"`
			Title              string `db:"title"`
			Lang               string `db:"lang"`
			TranslationGroupID string `db:"translation_group_id"`
		}
		q, args, err := sqlx.In(`SELECT a.slug, a.title, a.lang, a.translation_group_id FROM articles a WHERE a.slug IN (?) AND a.kind = 'page' AND `+
			publishedCondition, pageSlugs, article.StatusPublished, clockNow().UTC())
		if err != nil {
			return refs, err
		}
		var rows []pageRow
		if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
			return refs, err
		}
		var clusterGroupIDs []string
		slugToGroup := make(map[string]string, len(rows))
		for _, r := range rows {
			refs.Pages[r.Slug] = r.Title
			if r.Lang == "en" {
				refs.EnPages[r.Slug] = structurePageRef{Slug: r.Slug, Title: r.Title}
			} else if r.TranslationGroupID != "" {
				clusterGroupIDs = append(clusterGroupIDs, r.TranslationGroupID)
				slugToGroup[r.Slug] = r.TranslationGroupID
			}
		}
		if len(clusterGroupIDs) > 0 {
			qEn, argsEn, err := sqlx.In(`SELECT a.slug, a.title, a.translation_group_id FROM articles a WHERE a.translation_group_id IN (?) AND a.lang = 'en' AND a.kind = 'page' AND `+
				publishedCondition, clusterGroupIDs, article.StatusPublished, clockNow().UTC())
			if err == nil {
				var enRows []struct {
					Slug               string `db:"slug"`
					Title              string `db:"title"`
					TranslationGroupID string `db:"translation_group_id"`
				}
				if err := s.db.SelectContext(ctx, &enRows, qEn, argsEn...); err == nil {
					groupByEn := make(map[string]structurePageRef, len(enRows))
					for _, er := range enRows {
						groupByEn[er.TranslationGroupID] = structurePageRef{Slug: er.Slug, Title: er.Title}
					}
					for slug, grpID := range slugToGroup {
						if enRef, ok := groupByEn[grpID]; ok {
							refs.EnPages[slug] = enRef
						}
					}
				}
			}
		}
	}
	if len(catSlugs) > 0 {
		type row struct {
			Slug string `db:"slug"`
			Name string `db:"name"`
		}
		q, args, err := sqlx.In(`SELECT slug, name FROM categories WHERE slug IN (?)`, catSlugs)
		if err != nil {
			return refs, err
		}
		var rows []row
		if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
			return refs, err
		}
		for _, r := range rows {
			refs.Categories[r.Slug] = r.Name
		}
	}
	return refs, nil
}

// categoryEnglishName はカテゴリの英語名を解決する。既知の標準カテゴリの英語翻訳、
// または ASCII 名、またはスラッグをベースとした英語表記を返す。
func categoryEnglishName(slug, name string) string {
	switch strings.ToLower(slug) {
	case "uncategorized", "default":
		return "Uncategorized"
	case "news":
		return "News"
	case "notice", "notices":
		return "Notice"
	case "research":
		return "Research"
	case "publication", "publications":
		return "Publications"
	case "member", "members":
		return "Members"
	case "event", "events":
		return "Events"
	case "column", "columns":
		return "Column"
	case "blog", "blogs":
		return "Blog"
	case "press":
		return "Press"
	case "release", "releases":
		return "Release"
	case "circle", "circles":
		return "Club Activity"
	}
	if isASCIIOnly(name) {
		return name
	}
	if isASCIIOnly(slug) && slug != "" {
		return strings.ToUpper(slug[:1]) + slug[1:]
	}
	return name
}

func isASCIIOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return len(s) > 0
}

// isInternalContentPath は英語表示時に /en プレフィックスを補完すべき既知の内部コンテンツパスか判定する。
// 静的ファイル（/uploads/, /static/）、フィード（/feed.xml）、サイトマップ等のシステムパスや外部URLは書き換えない。
func isInternalContentPath(p string) bool {
	if p == "/" {
		return true
	}
	if p == "/articles" || strings.HasPrefix(p, "/articles/") {
		return true
	}
	if p == "/categories" || strings.HasPrefix(p, "/categories/") {
		return true
	}
	if p == "/page" || strings.HasPrefix(p, "/page/") {
		return true
	}
	if p == "/search" || strings.HasPrefix(p, "/search/") {
		return true
	}
	return false
}

func matchPath(match, current string) bool {
	if match == "" || current == "" {
		return false
	}
	if match == current {
		return true
	}
	mTrim := strings.TrimRight(match, "/")
	cTrim := strings.TrimRight(current, "/")
	if mTrim != "" && mTrim == cTrim {
		return true
	}
	if (match == "/" || match == "") && (current == "/" || current == "") {
		return true
	}
	if (match == "/en/" || match == "/en") && (current == "/en/" || current == "/en") {
		return true
	}
	return false
}

// publicMenuItem は公開サイトのナビゲーション・フッターのサイトマップに出す1項目（参照先を解決済み）。
type publicMenuItem struct {
	Label string
	// URL はリンク先。空なら見出しのみ（リンクなし）の項目で、子項目をまとめる見出しとして出す。
	URL string
	// Current は表示中のページを指す項目か（aria-current="page"）。
	Current bool
	// Children は子項目（メニューのドロップダウン・フッターの列のリンク）のうち、参照先があり形の正しいもの。
	Children []publicMenuItem
	// HasCurrent は子項目のどれかが表示中のページを指すか（ドロップダウンの親の項目を強調する）。
	HasCurrent bool
	// ID は子項目の一覧に付ける id（ドロップダウンを開くボタンの aria-controls）。子項目が無ければ空。
	ID string
}

// publicMenu は保存済みのメニューを、表示中のパス currentPath（デコード済み）に合わせて解決する。
// 表示言語が "en" の場合は英語ラベル（LabelEn）や英語ページ・英語カテゴリ名を使用し、URL も /en/... を指すように解決する。
// 参照先が無い・未公開の項目と、形の正しくない項目は出さない。
func (s *Server) publicMenu(ctx context.Context, st SiteStructure, currentPath string, lang ...string) ([]publicMenuItem, error) {
	menu, _, err := s.publicNav(ctx, st, currentPath, lang...)
	return menu, err
}

// publicNav は保存済みのメニューとフッターのサイトマップを、表示中のパス currentPath に合わせて解決する（参照先は
// 1回で読む）。解決の決まりは publicMenu と同じ。
func (s *Server) publicNav(ctx context.Context, st SiteStructure, currentPath string, lang ...string) (menu, footer []publicMenuItem, err error) {
	if len(st.Menu) == 0 && len(st.FooterMenu) == 0 {
		return nil, nil, nil
	}
	header := st.Menu
	if len(header) > maxMenuItems {
		header = header[:maxMenuItems]
	}
	foot := st.FooterMenu
	if len(foot) > maxFooterColumns {
		foot = foot[:maxFooterColumns]
	}
	refs, err := s.resolveStructureRefs(ctx, SiteStructure{Menu: header, FooterMenu: foot, FrontPage: st.FrontPage})
	if err != nil {
		return nil, nil, err
	}
	r := menuResolver{
		refs:    refs,
		front:   resolvedFrontPage(st, refs),
		current: currentPath,
		isEn:    len(lang) > 0 && strings.ToLower(strings.TrimSpace(lang[0])) == "en",
	}
	return r.tree(header, "site-submenu"), r.tree(foot, ""), nil
}

// menuResolver はメニューの項目を、参照先（refs）と表示中のパス・表示言語に合わせて公開用の項目にする。
type menuResolver struct {
	refs structureRefs
	// front はトップに出している公開中のページのスラッグ（無ければ空）。
	front   string
	current string
	isEn    bool
}

// tree は最上位の項目とその子項目を解決する。参照先の無い親でも、表示名があり子項目が残るなら見出しとして出し、
// 子項目がすべて出せない見出しのみの項目は出さない。idPrefix が空でなければ、子項目のある項目に
// "<idPrefix>-<番号>" の ID を付ける。
func (r menuResolver) tree(items []MenuItem, idPrefix string) []publicMenuItem {
	var out []publicMenuItem
	for i, it := range items {
		if validateMenuItem(it) != nil {
			continue
		}
		var children []publicMenuItem
		hasCurrent := false
		for j, ch := range it.Children {
			if j >= maxMenuChildren {
				break
			}
			if validateMenuChild(ch) != nil {
				continue
			}
			if c, ok := r.item(ch); ok {
				children = append(children, c)
				hasCurrent = hasCurrent || c.Current
			}
		}
		p, ok := r.item(it)
		if !ok {
			label := r.ownLabel(it)
			if label == "" || len(children) == 0 {
				continue
			}
			p = publicMenuItem{Label: label}
		}
		if p.URL == "" && len(children) == 0 {
			continue
		}
		if len(children) > 0 {
			p.Children, p.HasCurrent = children, hasCurrent
			if idPrefix != "" {
				p.ID = fmt.Sprintf("%s-%d", idPrefix, i+1)
			}
		}
		out = append(out, p)
	}
	return out
}

// ownLabel は項目に入力された表示名（英語表示では英語の表示名、空なら日本語の表示名）を返す。
func (r menuResolver) ownLabel(it MenuItem) string {
	if r.isEn {
		if l := strings.TrimSpace(it.LabelEn); l != "" {
			return l
		}
	}
	return strings.TrimSpace(it.Label)
}

// item は1項目の参照先を解決する（子項目は見ない）。参照先が無い・未公開の項目は ok が false。見出しのみの項目は
// URL が空。
func (r menuResolver) item(it MenuItem) (publicMenuItem, bool) {
	var (
		p     publicMenuItem
		match string // 表示中のパスがこれと同じなら現在の項目
	)
	refs, front, isEn := r.refs, r.front, r.isEn
	switch it.Type {
	case MenuItemPage:
		title, ok := refs.Pages[it.Target]
		if !ok {
			return p, false
		}
		resolvedSlug := it.Target
		resolvedTitle := title
		if isEn {
			if enRef, ok := refs.EnPages[it.Target]; ok {
				resolvedSlug = enRef.Slug
				resolvedTitle = enRef.Title
			}
			p = publicMenuItem{Label: resolvedTitle, URL: "/en/" + url.PathEscape(resolvedSlug)}
			match = "/en/" + resolvedSlug
			if it.Target == front {
				p.URL, match = "/en/", "/en/"
			}
		} else {
			p = publicMenuItem{Label: title, URL: pagePath(it.Target)}
			match = "/" + it.Target
			// トップに出しているページは / が唯一の URL（/<slug> は / へ転送する）。
			if it.Target == front {
				p.URL, match = "/", "/"
			}
		}
	case MenuItemCategory:
		name, ok := refs.Categories[it.Target]
		if !ok {
			return p, false
		}
		if isEn {
			p = publicMenuItem{Label: categoryEnglishName(it.Target, name), URL: "/en" + categoryPath(it.Target)}
			match = "/en" + categoryPath(it.Target)
		} else {
			p = publicMenuItem{Label: name, URL: categoryPath(it.Target)}
			match = categoryPath(it.Target)
		}
	case MenuItemArticles:
		if isEn {
			p = publicMenuItem{Label: "Articles", URL: "/en" + articlesListPath}
			match = "/en" + articlesListPath
		} else {
			p = publicMenuItem{Label: "記事一覧", URL: articlesListPath}
			match = articlesListPath
		}
	case MenuItemURL:
		targetURL := it.Target
		if isEn && strings.HasPrefix(targetURL, "/") && !strings.HasPrefix(targetURL, "/en/") && targetURL != "/en" {
			parsedPath := targetURL
			if u, err := url.Parse(targetURL); err == nil && u.Path != "" {
				parsedPath = u.Path
			}
			if isInternalContentPath(parsedPath) {
				if targetURL == "/" {
					targetURL = "/en/"
				} else {
					targetURL = "/en" + targetURL
				}
			}
		}
		p = publicMenuItem{URL: targetURL}
		if strings.HasPrefix(targetURL, "/") {
			if u, err := url.Parse(targetURL); err == nil {
				match = u.Path
			}
		}
	case MenuItemGroup:
		// 見出しのみ: リンクを持たず、現在の項目にもならない。表示名は下で入れる。
	default:
		return p, false
	}

	if isEn {
		if l := strings.TrimSpace(it.LabelEn); l != "" {
			p.Label = l
		} else if l := strings.TrimSpace(it.Label); l != "" && (it.Type == MenuItemURL || it.Type == MenuItemGroup) {
			p.Label = l
		}
	} else {
		if l := strings.TrimSpace(it.Label); l != "" {
			p.Label = l
		}
	}

	p.Current = matchPath(match, r.current)
	return p, true
}

// articlesLabelSet は公開サイトで記事の一覧（/articles）を指す文言（articlesLabels）。
type articlesLabelSet struct {
	// Label は一覧の呼び名（見出し・パンくず・タイトル）。例: 「ニュース」・"News"。
	Label string
	// Back は記事ページの戻るリンク（例: 「ニュース一覧へ戻る」・"Back to News"）。
	Back string
	// All はトップの新着記事の下のリンク（例: 「ニュース一覧へ」・"All News"）。
	All string
}

// articlesLabels は st のメニュー（site_menu）の articles 項目の表示名から、記事の一覧を指す文言を作る。
// 日本語は Label、英語は LabelEn を使い、項目が無い・表示名が空なら「記事一覧」・"Articles"。
// 日本語の呼び名が「一覧」で終わるときは「一覧」を重ねない（「記事一覧へ戻る」であって「記事一覧一覧へ戻る」ではない）。
func articlesLabels(st SiteStructure, lang string) articlesLabelSet {
	isEn := lang == "en"
	label := ""
	if it, ok := findArticlesMenuItem(st.Menu); ok {
		if isEn {
			label = strings.TrimSpace(it.LabelEn)
		} else {
			label = strings.TrimSpace(it.Label)
		}
	}
	if isEn {
		if label == "" {
			label = "Articles"
		}
		return articlesLabelSet{Label: label, Back: "Back to " + label, All: "All " + label}
	}
	if label == "" {
		label = "記事一覧"
	}
	list := label + "一覧"
	if strings.HasSuffix(label, "一覧") {
		list = label
	}
	return articlesLabelSet{Label: label, Back: list + "へ戻る", All: list + "へ"}
}

// findArticlesMenuItem はメニューで最初の articles 項目を返す。公開メニュー（menuResolver.tree）と同じく、
// 検証を通る最上位の項目と、その子項目（1階層・最大 maxMenuChildren 件）だけを見て、改ざんされた孫項目などの
// 表示名は使わない。
func findArticlesMenuItem(items []MenuItem) (MenuItem, bool) {
	for i, it := range items {
		if i >= maxMenuItems {
			break
		}
		if validateMenuItem(it) != nil {
			continue
		}
		if it.Type == MenuItemArticles {
			return it, true
		}
		for j, ch := range it.Children {
			if j >= maxMenuChildren {
				break
			}
			if validateMenuChild(ch) == nil && ch.Type == MenuItemArticles {
				return ch, true
			}
		}
	}
	return MenuItem{}, false
}

// frontData は front.html（トップに固定ページを出すときのトップページ）に渡すデータ。
// 埋め込んだ articleData はトップに出すページ（アイキャッチ・タイトル・本文）で、content_head もこれを読む。
type frontData struct {
	articleData
	Sections []renderedFrontSection
	// Cards は cards セクションで参照された固定ページ（公開中のものだけ、設定順）。
	Cards []publicArticle
	// News は新着記事（frontNewsCount 件）。
	News []publicArticle
	// ShowNews は「新着記事」の欄を出すか（showFrontNews）。
	ShowNews bool
}

type renderedFrontSection struct {
	Type           FrontSectionType
	Page           publicPage
	Hero           articleData
	Cards          []publicArticle
	News           []publicArticle
	ShowNews       bool
	Tag            string
	Limit          int
	Title          string
	Layout         string
	LinkURL        string
	LinkLabel      string
	SecondaryURL   string
	SecondaryLabel string
	Index          bool
	ID             string
	IndexArticles  []publicArticle
	Pagination     pagination
}

func sectionID(kind FrontSectionType, occurrence int) string {
	name := string(kind)
	if kind == FrontSectionArticleList {
		name = "news"
	}
	base := "front-" + name + "-title"
	if occurrence <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, occurrence)
}

// showFrontNews はトップに固定ページを出すトップページに「新着記事」の欄を出すかを返す。新着記事が1件以上あるか、
// メニューに記事の一覧（articles）の項目があるサイトでは出す。記事を書かないサイト（ドキュメントや固定ページ中心の
// サイト）で、新着記事が無く、メニューからも記事の一覧へ行けないときは、「まだ公開された記事はありません。」だけの欄を出さない。
func showFrontNews(news []publicArticle, menu []MenuItem) bool {
	if len(news) > 0 {
		return true
	}
	_, ok := findArticlesMenuItem(menu)
	return ok
}

// resolvedFrontPage は st の front_page が公開中のページ（refs で解決済み）ならそのスラッグを、未設定・参照切れなら空を返す。
func resolvedFrontPage(st SiteStructure, refs structureRefs) string {
	if st.FrontPage == "" || validateStructureSlug(st.FrontPage) != nil {
		return ""
	}
	if _, ok := refs.Pages[st.FrontPage]; !ok {
		return ""
	}
	return st.FrontPage
}

// frontPageSlug はトップに公開中のページを出しているとき、そのページのスラッグを返す。未設定・参照切れなら空。
// サイトマップ・llms.txt は、このページの URL を / として載せる（/<slug> は / へ転送するため）。
func (s *Server) frontPageSlug(ctx context.Context) (string, error) {
	st := s.loadSiteStructure(ctx)
	refs, err := s.resolveStructureRefs(ctx, SiteStructure{FrontPage: st.FrontPage})
	if err != nil {
		return "", err
	}
	return resolvedFrontPage(st, refs), nil
}

// publicURLPath は記事・ページの公開 URL のパス。トップに出しているページ（front）は / または /en/ にする。
func publicURLPath(kind article.Kind, slug, front string, lang ...string) string {
	l := ""
	if len(lang) > 0 {
		l = lang[0]
	}
	if kind == article.KindPage && front != "" && slug == front {
		if strings.ToLower(strings.TrimSpace(l)) == "en" {
			return "/en/"
		}
		return "/"
	}
	return articlePublicPath(kind, slug, l)
}

// frontPage は保存済みの front_page が公開中のページなら、そのページを返す。未設定・参照切れなら ok=false。
func (s *Server) frontPage(st SiteStructure) (publicArticle, bool, error) {
	if st.FrontPage == "" || validateStructureSlug(st.FrontPage) != nil {
		return publicArticle{}, false, nil
	}
	a, err := s.publishedBySlug(st.FrontPage)
	if errors.Is(err, sql.ErrNoRows) {
		return publicArticle{}, false, nil
	}
	if err != nil {
		return publicArticle{}, false, err
	}
	return a, a.Kind == article.KindPage, nil
}

// renderFront はトップに固定ページ front を出すトップページを描画する。<title>・description・JSON-LD はサイトのトップのまま。
func (s *Server) renderFront(c echo.Context, p publicPage, front publicArticle, st SiteStructure) error {
	ctx := c.Request().Context()
	p.GalleryQueryResolver = s.galleryQueryResolver()
	var scratch publicPage
	body, err := s.loadArticleBody(c, front, &scratch, s.baseURL(c))
	if err != nil {
		return err
	}
	// og:image はアイキャッチがあるときだけ使う（ページのタイトルを描いた動的 OGP 画像はトップには合わない）。
	if front.FeaturedImage != "" {
		p.OGImage = scratch.OGImage
	}
	body.GeneratedOGImage = false
	data := frontData{articleData: body}

	refs, err := s.resolveStructureRefs(ctx, SiteStructure{FrontSections: st.FrontSections})
	if err != nil {
		return err
	}
	var cardSlugs []string
	for _, section := range st.FrontSections {
		if section.Type != FrontSectionCards {
			continue
		}
		for _, slug := range section.Pages {
			if _, ok := refs.Pages[slug]; ok && slug != front.Slug && !slices.Contains(cardSlugs, slug) {
				cardSlugs = append(cardSlugs, slug)
			}
		}
	}
	var lookupSlugs []string
	for _, slug := range cardSlugs {
		targetSlug := slug
		if p.Lang == "en" {
			if enRef, ok := refs.EnPages[slug]; ok && enRef.Slug != "" {
				targetSlug = enRef.Slug
			}
		}
		lookupSlugs = append(lookupSlugs, targetSlug)
	}
	if len(lookupSlugs) > 0 {
		q, args, err := sqlx.In(`SELECT `+publicArticleColumns+publicArticleFrom+`WHERE a.slug IN (?) AND a.kind = 'page' AND `+
			publishedCondition, lookupSlugs, article.StatusPublished, clockNow().UTC())
		if err != nil {
			return err
		}
		var rows []publicArticle
		if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
			return err
		}
		bySlug := make(map[string]publicArticle, len(rows))
		for _, r := range rows {
			r.ImageHTML = s.responsiveImage(r.FeaturedImage, r.Title, "front-card-image")
			r.Excerpt = articleDescription(r, r.ContentHTML)
			bySlug[r.Slug] = r
		}
		for _, targetSlug := range lookupSlugs {
			if r, ok := bySlug[targetSlug]; ok {
				data.Cards = append(data.Cards, r)
			}
		}
	}

	data.News = []publicArticle{}
	if err := s.db.SelectContext(ctx, &data.News, `SELECT `+publicArticleColumns+publicArticleFrom+`WHERE `+postKindCondition+
		` AND `+publishedCondition+` ORDER BY julianday(a.published_at) DESC, a.id LIMIT ?`,
		article.StatusPublished, clockNow().UTC(), frontNewsCount); err != nil {
		return err
	}
	for i := range data.News {
		data.News[i].ImageHTML = s.responsiveImage(data.News[i].FeaturedImage, data.News[i].Title, "card-image")
	}
	data.ShowNews = showFrontNews(data.News, st.Menu)
	sectionOccurrences := map[FrontSectionType]int{}
	for _, section := range st.FrontSections {
		sectionOccurrences[section.Type]++
		layout := section.Layout
		if section.Type == FrontSectionGallery {
			layout = normalizeGalleryLayout(layout)
		}
		limit := section.Limit
		if section.Type == FrontSectionGallery && limit <= 0 {
			limit = 6
		}
		view := renderedFrontSection{Type: section.Type, Page: p, Hero: body, Tag: section.Tag, Limit: limit, Title: section.Title, Layout: layout, LinkURL: section.LinkURL, LinkLabel: section.LinkLabel, SecondaryURL: section.SecondaryURL, SecondaryLabel: section.SecondaryLabel, ID: sectionID(section.Type, sectionOccurrences[section.Type])}
		switch section.Type {
		case FrontSectionCards:
			for _, slug := range section.Pages {
				if slug == front.Slug {
					continue
				}
				targetSlug := slug
				if p.Lang == "en" {
					if ref, ok := refs.EnPages[slug]; ok && ref.Slug != "" {
						targetSlug = ref.Slug
					}
				}
				for _, card := range data.Cards {
					if card.Slug == targetSlug {
						view.Cards = append(view.Cards, card)
						break
					}
				}
			}
		case FrontSectionArticleList:
			view.News = data.News
			if section.ArticleTag != "" {
				categoryID, err := s.articleTagCategoryID(section.ArticleTag)
				if err != nil {
					return err
				}
				view.News = []publicArticle{}
				langCondition := ` AND (a.lang = 'ja' OR a.lang = '' OR a.lang IS NULL)`
				if p.Lang == "en" {
					langCondition = ` AND a.lang = 'en'`
				}
				if err := s.db.SelectContext(ctx, &view.News, `SELECT `+publicArticleColumns+publicArticleFrom+`WHERE `+postKindCondition+langCondition+` AND a.category_id = ? AND `+publishedCondition+` ORDER BY julianday(a.published_at) DESC, a.id LIMIT ?`, categoryID, article.StatusPublished, clockNow().UTC(), frontNewsCount); err != nil {
					return err
				}
				for i := range view.News {
					view.News[i].ImageHTML = s.responsiveImage(view.News[i].FeaturedImage, view.News[i].Title, "card-image")
				}
			}
			view.ShowNews = showFrontNews(view.News, st.Menu)
		}
		data.Sections = append(data.Sections, view)
	}
	p.Data = data
	return s.renderPublic(c, http.StatusOK, "front", p)
}

// --- 管理画面「サイト構成」 ---

// structureOption は選択肢に出すページ・カテゴリ。
type structureOption struct {
	Slug  string `db:"slug"`
	Title string `db:"title"`
}

// structureRow は管理画面のメニュー（またはフッターのサイトマップ）の1行。Missing は参照先が無い・未公開のときの
// 参照先のスラッグで、選択肢に残して保存し直しても失われないようにする。
type structureRow struct {
	MenuItem
	Missing string
	// Index は行の位置（最上位の項目、その子項目、次の最上位の項目…の順に平らに並べた0始まりの番号。行の操作の
	// ボタンの値と欄の id）、Number は最上位の項目の1始まりの番号、Child は子項目の1始まりの番号（最上位の行は 0）。
	Index, Number, Child int
	// First・Last は兄弟（最上位の項目どうし・同じ親の子項目どうし）の最初・最後か（「上へ」「下へ」を押せなくする）。
	First, Last bool
	// Rows は子項目の行（最上位の行だけ）。
	Rows []structureRow
	// CanAddChild は子項目を足せるか（最上位の行）、CanIndent は直前の最上位の項目の子項目にできるか、CanOutdent は
	// 子項目を最上位の項目に出せるか。
	CanAddChild, CanIndent, CanOutdent bool
	// E は行の属する編集欄（欄の名前の接頭辞・選択肢・翻訳）。
	E *structureMenuEditor
}

// IsChild は子項目の行か。
func (r structureRow) IsChild() bool { return r.Child > 0 }

// Lang は行を描く表示言語（テンプレートの {{$ja := ne .Lang "en"}}）。
func (r structureRow) Lang() string { return r.E.Lang }

// structureMenuEditor は管理画面のメニュー・フッターのサイトマップの編集欄（settings_structure.html の
// structure_menu_editor）。2つの欄は同じ部品で描き、欄の名前と操作の値の接頭辞だけを変える。
type structureMenuEditor struct {
	Lang string
	// Prefix はフォームの欄の名前（<Prefix>_type など）と id（<Prefix>-type-0 など）の接頭辞（menu・footer）。
	// OpPrefix は行の操作（op）の値の接頭辞（メニューは空、フッターは "footer-"）。
	Prefix, OpPrefix string
	Footer           bool
	Rows             []structureRow
	// CanAdd は最上位の項目（フッターでは列）を足せるか。
	CanAdd                             bool
	MaxTop, MaxChildren, MaxLabelRunes int
	Pages, Categories                  []structureOption
	Types                              []MenuItemType
}

// T は編集欄の表示言語で key を翻訳する。
func (e *structureMenuEditor) T(key string, args ...any) string {
	return i18n.Translate(e.Lang, key, args...)
}

// ChildTypes は子項目の行で選べる種類（見出しのみは最上位だけ）。
func (e *structureMenuEditor) ChildTypes() []MenuItemType {
	return slices.DeleteFunc(slices.Clone(e.Types), func(t MenuItemType) bool { return t == MenuItemGroup })
}

// structureSelect はトップページ・カードの選択欄。
type structureSelect struct {
	Value   string
	Missing string
	// Number は画面に出す番号（カードの1始まりの番号）。
	Number int
}

type structureSettingsData struct {
	Sections          []frontSectionForm
	FrontSectionTypes []FrontSectionType
	Rows              []structureRow
	FrontPage         structureSelect
	FrontCards        []structureSelect
	Pages             []structureOption
	Categories        []structureOption
	GalleryTags       []string
	TopGalleryTag     string
	TopGalleryLimit   int
	TopGalleryTitle   string
	// TopGalleryLimitOptions は表示枚数の選択肢（topGalleryLimitOptions。保存済みの枚数が選択肢に無ければ足す）。
	TopGalleryLimitOptions []int
	// TopGalleryCount は TopGalleryTag の付いた写真の枚数。TopGalleryStatus はその案内の文（表示言語に翻訳済み。
	// JavaScript なしでも出し、JavaScript があれば入力に合わせて書き換える）。
	TopGalleryCount  int
	TopGalleryStatus string
	// TopGalleryLayout は選んでいる並べ方（空なら既定の行揃え）、TopGalleryLayouts は選べる並べ方（画面に出す順）。
	TopGalleryLayout    string
	TopGalleryLayouts   []string
	TopGalleryLinkURL   string
	TopGalleryLinkLabel string
	// MaxGalleryLinkLabelRunes は「すべて見る」リンクの文言の最大文字数（入力欄の maxlength）。
	MaxGalleryLinkLabelRunes int
	Types                    []MenuItemType
	// Menu・Footer はメニューとフッターのサイトマップの編集欄。
	Menu, Footer *structureMenuEditor
	// Warnings は参照先が無い・未公開の項目の警告（表示言語に翻訳済み）。
	Warnings []string
	// Unsaved は行の追加・削除・移動の後で、まだ保存していない表示か。
	Unsaved          bool
	CanAddRow        bool
	MaxMenuItems     int
	MaxMenuChildren  int
	MaxFooterColumns int
	MaxFrontCards    int
	MaxFrontSections int
	MaxLabelRunes    int
	CanPushSettings  bool
	PushConfirm      string
	JustSaved        bool
}

type frontSectionForm struct {
	FrontSection
	Index             int
	Number            int
	First             bool
	Last              bool
	PagesText         string
	EffectiveLayout   string
	MissingArticleTag string
}

func (s *Server) handleStructureSettings(c echo.Context) error {
	st := s.loadSiteStructure(c.Request().Context())
	return s.renderStructure(c, http.StatusOK, st, false, "")
}

// handleStructureSettingsSave はサイト構成のフォームを受ける。op が add・delete:<i>・up:<i>・down:<i> なら
// JavaScript なしでも行を編集できるよう、保存せずに編集後のフォームを表示し直す。それ以外（save）は保存する。
func (s *Server) handleStructureSettingsSave(c echo.Context) error {
	st, err := structureFromForm(c)
	if err != nil {
		return err
	}
	op := c.FormValue("op")
	if op != "" && op != "save" {
		if strings.HasPrefix(op, "section-") {
			st.FrontSections = applyFrontSectionOp(st.FrontSections, op)
		} else if rest, ok := strings.CutPrefix(op, footerOpPrefix); ok {
			st.FooterMenu = applyRowOp(st.FooterMenu, rest, maxFooterColumns)
		} else {
			st.Menu = applyRowOp(st.Menu, op, maxMenuItems)
		}
		return s.renderStructure(c, http.StatusOK, st, true, "")
	}
	st.Menu = dropBlankRows(st.Menu)
	st.FooterMenu = dropBlankRows(st.FooterMenu)
	if err := s.saveSiteStructure(c.Request().Context(), st); err != nil {
		var se *siteStructureError
		if errors.As(err, &se) {
			return s.renderStructure(c, http.StatusUnprocessableEntity, st, true, se.localize(string(language(c))))
		}
		return err
	}
	s.markRemoteDirty(c, remoteDirtyStructure)
	flash := "structure_saved"
	switch s.reflectAfterPublish() {
	case flashPublishedManual:
		flash = "structure_saved_manual"
	case flashPublishedReflecting:
		flash = "structure_saved_reflecting"
	}
	return c.Redirect(http.StatusSeeOther, "/admin/settings/structure?flash="+flash)
}

func applyFrontSectionOp(sections []FrontSection, op string) []FrontSection {
	if op == "section-add" {
		if len(sections) < maxFrontSections {
			sections = append(sections, FrontSection{Type: FrontSectionArticleList})
		}
		return sections
	}
	name, raw, ok := strings.Cut(op, "section-")
	if !ok {
		return sections
	}
	name, raw, ok = strings.Cut(raw, ":")
	if !ok {
		return sections
	}
	i, err := strconv.Atoi(raw)
	if err != nil || i < 0 || i >= len(sections) {
		return sections
	}
	switch name {
	case "delete":
		return slices.Delete(sections, i, i+1)
	case "up":
		if i > 0 {
			sections[i-1], sections[i] = sections[i], sections[i-1]
		}
	case "down":
		if i < len(sections)-1 {
			sections[i], sections[i+1] = sections[i+1], sections[i]
		}
	}
	return sections
}

// footerOpPrefix はフッターのサイトマップの行の操作（op）の値の接頭辞（footer-add・footer-delete:<i> など）。
const footerOpPrefix = "footer-"

// structureFromForm はフォームのメニューの行（menu_type・menu_page・menu_category・menu_url・menu_label・
// menu_label_en・menu_depth の並び）と、フッターのサイトマップの行（footer_ で始まる同じ並び）、トップの設定を読む。
func structureFromForm(c echo.Context) (SiteStructure, error) {
	form, err := c.FormParams()
	if err != nil {
		return SiteStructure{}, echo.NewHTTPError(http.StatusBadRequest, "invalid form")
	}
	var st SiteStructure
	st.Menu = menuFromForm(form, "menu", maxMenuItems)
	st.FooterMenu = menuFromForm(form, "footer", maxFooterColumns)
	st.FrontPage = strings.TrimSpace(form.Get("front_page"))
	types := form["section_type"]
	for i, rawType := range types {
		section := FrontSection{Type: FrontSectionType(strings.TrimSpace(rawType))}
		at := func(key string) string {
			if i < len(form["section_"+key]) {
				return strings.TrimSpace(form["section_"+key][i])
			}
			return ""
		}
		section.Tag, section.Title, section.Layout, section.LinkURL, section.LinkLabel, section.SecondaryURL, section.SecondaryLabel, section.ArticleTag = at("tag"), at("title"), at("layout"), at("link_url"), at("link_label"), at("secondary_url"), at("secondary_label"), at("article_tag")
		if raw := at("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil {
				return SiteStructure{}, echo.NewHTTPError(http.StatusBadRequest, "invalid form")
			}
			section.Limit = limit
		}
		if i < len(form["section_pages"]) {
			for _, slug := range strings.Split(form["section_pages"][i], ",") {
				if slug = strings.TrimSpace(slug); slug != "" && !slices.Contains(section.Pages, slug) {
					section.Pages = append(section.Pages, slug)
				}
			}
		}
		st.FrontSections = append(st.FrontSections, clearUnusedFrontSectionFields(section))
	}
	return st, nil
}

// menuFromForm はフォームの接頭辞 prefix（menu・footer）の行を読む。行の種類に応じて、対応する欄の値を Target に
// する。<prefix>_depth が "1" の行は直前の最上位の行の子項目にする（最上位の行より前の子の行は最上位として読む。
// depth の無い古いフォームはすべて最上位）。
func menuFromForm(form url.Values, prefix string, maxTop int) []MenuItem {
	types := form[prefix+"_type"]
	// 不正に大きなフォームで描画を重くしないよう、上限を少し超える行までしか読まない（超過は保存時に検査で拒否する）。
	if limit := maxTop*(maxMenuChildren+1) + 1; len(types) > limit {
		types = types[:limit]
	}
	at := func(name string, i int) string {
		if v := form[prefix+"_"+name]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	var menu []MenuItem
	for i, t := range types {
		it := MenuItem{
			Type:    MenuItemType(t),
			Label:   at("label", i),
			LabelEn: at("label_en", i),
		}
		switch it.Type {
		case MenuItemPage:
			it.Target = at("page", i)
		case MenuItemCategory:
			it.Target = at("category", i)
		case MenuItemURL:
			it.Target = at("url", i)
		}
		if at("depth", i) == "1" && len(menu) > 0 {
			parent := &menu[len(menu)-1]
			parent.Children = append(parent.Children, it)
			continue
		}
		menu = append(menu, it)
	}
	return menu
}

// menuPos は平らに並べた行の位置 i（最上位の項目、その子項目、次の最上位の項目…の順）を、最上位の項目の位置 top と
// 子項目の位置 child（最上位の行なら -1）にする。範囲外なら ok が false。
func menuPos(menu []MenuItem, i int) (top, child int, ok bool) {
	n := 0
	for t, it := range menu {
		if i == n {
			return t, -1, true
		}
		n++
		if i < n+len(it.Children) {
			return t, i - n, true
		}
		n += len(it.Children)
	}
	return 0, 0, false
}

// applyRowOp はメニューの行に op を適用する。add は最上位の項目を足し（maxTop まで）、<name>:<i> は平らに並べた i 番目の
// 行に対して delete（最上位なら子項目ごと）・up・down（兄弟の中で入れ替え）・child（子項目を足す）・indent（直前の
// 最上位の項目の最後の子項目にする）・outdent（子項目を親の直後の最上位の項目にする）を行う。できない操作は何もしない。
func applyRowOp(menu []MenuItem, op string, maxTop int) []MenuItem {
	if op == "add" {
		if len(menu) < maxTop {
			menu = append(menu, MenuItem{Type: MenuItemPage})
		}
		return menu
	}
	name, idx, ok := strings.Cut(op, ":")
	i, err := strconv.Atoi(idx)
	if !ok || err != nil || i < 0 {
		return menu
	}
	top, child, ok := menuPos(menu, i)
	if !ok {
		return menu
	}
	if child < 0 {
		switch name {
		case "delete":
			return slices.Delete(menu, top, top+1)
		case "up":
			if top > 0 {
				menu[top-1], menu[top] = menu[top], menu[top-1]
			}
		case "down":
			if top < len(menu)-1 {
				menu[top], menu[top+1] = menu[top+1], menu[top]
			}
		case "child":
			if len(menu[top].Children) < maxMenuChildren {
				menu[top].Children = append(menu[top].Children, MenuItem{Type: MenuItemPage})
			}
		case "indent":
			if canIndent(menu, top) {
				menu[top-1].Children = append(menu[top-1].Children, menu[top])
				return slices.Delete(menu, top, top+1)
			}
		}
		return menu
	}
	children := menu[top].Children
	switch name {
	case "delete":
		menu[top].Children = nilIfEmpty(slices.Delete(children, child, child+1))
	case "up":
		if child > 0 {
			children[child-1], children[child] = children[child], children[child-1]
		}
	case "down":
		if child < len(children)-1 {
			children[child], children[child+1] = children[child+1], children[child]
		}
	case "outdent":
		if len(menu) < maxTop {
			it := children[child]
			menu[top].Children = nilIfEmpty(slices.Delete(children, child, child+1))
			menu = slices.Insert(menu, top+1, it)
		}
	}
	return menu
}

// canIndent は最上位の top 番目の項目を、直前の最上位の項目の子項目にできるか返す（子項目を持つ項目・見出しのみの
// 項目は子項目にできない）。
func canIndent(menu []MenuItem, top int) bool {
	return top > 0 && top < len(menu) && len(menu[top].Children) == 0 && menu[top].Type != MenuItemGroup &&
		len(menu[top-1].Children) < maxMenuChildren
}

// nilIfEmpty は空の子項目を nil にする（保存する JSON に "children" を出さず、読み直した値と同じにする）。
func nilIfEmpty(items []MenuItem) []MenuItem {
	if len(items) == 0 {
		return nil
	}
	return items
}

// dropBlankRows は何も入力していない行（参照先・表示名とも空の、記事の一覧以外の行）を除く。
// 「行を追加」したまま保存しても、空の行のためにエラーにならないようにする。子項目の残る最上位の行は残す。
func dropBlankRows(menu []MenuItem) []MenuItem {
	blank := func(it MenuItem) bool {
		return it.Type != MenuItemArticles && it.Target == "" && it.Label == "" && it.LabelEn == ""
	}
	for i := range menu {
		menu[i].Children = nilIfEmpty(slices.DeleteFunc(menu[i].Children, blank))
	}
	return slices.DeleteFunc(menu, func(it MenuItem) bool { return blank(it) && len(it.Children) == 0 })
}

// structureRows は編集欄 e の行を、参照先 refs を確かめながら作る（参照先が無い・未公開の行は Missing に入れる）。
func structureRows(e *structureMenuEditor, items []MenuItem, refs structureRefs) []structureRow {
	var rows []structureRow
	index := 0
	for i, it := range items {
		row := structureRow{MenuItem: it, Index: index, Number: i + 1, First: i == 0, Last: i == len(items)-1, E: e,
			CanAddChild: len(it.Children) < maxMenuChildren, CanIndent: canIndent(items, i)}
		row.Missing = missingTarget(it, refs)
		index++
		for j, ch := range it.Children {
			cr := structureRow{MenuItem: ch, Index: index, Number: i + 1, Child: j + 1, First: j == 0, Last: j == len(it.Children)-1, E: e,
				CanOutdent: len(items) < e.MaxTop}
			cr.Missing = missingTarget(ch, refs)
			row.Rows = append(row.Rows, cr)
			index++
		}
		rows = append(rows, row)
	}
	return rows
}

// missingTarget は項目の参照先（ページ・カテゴリ）が無い・未公開ならそのスラッグを、あれば空を返す。
func missingTarget(it MenuItem, refs structureRefs) string {
	if it.Target == "" {
		return ""
	}
	switch it.Type {
	case MenuItemPage:
		if _, ok := refs.Pages[it.Target]; !ok {
			return it.Target
		}
	case MenuItemCategory:
		if _, ok := refs.Categories[it.Target]; !ok {
			return it.Target
		}
	}
	return ""
}

// structureRowWarnings は参照先が無い・未公開の行の警告（表示言語 lang）を返す。表示名が空なら行の番号
// （子項目は「2-3」の形）で示す。
func structureRowWarnings(lang string, rows []structureRow, footer bool) []string {
	pageKey, catKey := "structure_warn_menu_page", "structure_warn_menu_category"
	if footer {
		pageKey, catKey = "structure_warn_footer_page", "structure_warn_footer_category"
	}
	var out []string
	add := func(r structureRow) {
		if r.Missing == "" {
			return
		}
		label := r.Label
		if label == "" {
			label = strconv.Itoa(r.Number)
			if r.Child > 0 {
				label = fmt.Sprintf("%d-%d", r.Number, r.Child)
			}
		}
		key := pageKey
		if r.Type == MenuItemCategory {
			key = catKey
		}
		out = append(out, i18n.Translate(lang, key, label, r.Missing))
	}
	for _, r := range rows {
		add(r)
		for _, cr := range r.Rows {
			add(cr)
		}
	}
	return out
}

// renderStructure はサイト構成の画面を描画する。errMsg が空でなければエラーのトーストとして出す。
func (s *Server) renderStructure(c echo.Context, code int, st SiteStructure, unsaved bool, errMsg string) error {
	ctx := c.Request().Context()
	lang := string(language(c))
	remote := s.remoteSettingsData(c, s.loadRemoteConfig())
	gallery := FrontSection{Type: FrontSectionGallery, Limit: 6, Layout: galleryLayoutJustified}
	for _, section := range st.FrontSections {
		if section.Type == FrontSectionGallery {
			gallery = section
			break
		}
	}
	d := structureSettingsData{
		FrontSectionTypes: []FrontSectionType{FrontSectionHero, FrontSectionCards, FrontSectionGallery, FrontSectionArticleList},
		Types:             menuItemTypes,
		Unsaved:           unsaved,
		CanAddRow:         len(st.Menu) < maxMenuItems,
		MaxMenuItems:      maxMenuItems,
		MaxMenuChildren:   maxMenuChildren,
		MaxFooterColumns:  maxFooterColumns,
		MaxFrontCards:     maxFrontCards,
		MaxFrontSections:  maxFrontSections,
		MaxLabelRunes:     maxMenuLabelRunes,
		CanPushSettings:   remote.CanPushSettings,
		PushConfirm:       remote.PushConfirm,
		JustSaved:         c.QueryParam("flash") == "structure_saved",
		TopGalleryTag:     gallery.Tag,
		TopGalleryLimit:   gallery.Limit,
		TopGalleryTitle:   gallery.Title,
		// 検証で戻した画面では入力したままの値を出す（知らない並べ方は既定の行揃えを選んだ表示にする）。
		TopGalleryLayout:         normalizeGalleryLayout(gallery.Layout),
		TopGalleryLayouts:        galleryLayouts,
		TopGalleryLinkURL:        gallery.LinkURL,
		TopGalleryLinkLabel:      gallery.LinkLabel,
		MaxGalleryLinkLabelRunes: maxGalleryLinkLabelRunes,
	}
	if d.TopGalleryLimit == 0 {
		d.TopGalleryLimit = 6
	}
	d.TopGalleryLimitOptions = slices.Clone(topGalleryLimitOptions)
	if !slices.Contains(d.TopGalleryLimitOptions, d.TopGalleryLimit) {
		d.TopGalleryLimitOptions = append(d.TopGalleryLimitOptions, d.TopGalleryLimit)
		slices.Sort(d.TopGalleryLimitOptions)
	}
	if gallery.Tag != "" {
		where, args := mediaWhere("", gallery.Tag)
		if err := s.db.GetContext(ctx, &d.TopGalleryCount, `SELECT COUNT(*) FROM media m`+where, args...); err != nil {
			return err
		}
	}
	d.TopGalleryStatus = topGalleryStatus(lang, gallery.Tag, d.TopGalleryCount, d.TopGalleryLimit)
	if err := s.db.SelectContext(ctx, &d.Pages, `SELECT a.slug, a.title FROM articles a WHERE a.kind = 'page' AND `+
		publishedCondition+` ORDER BY a.title, a.slug`, article.StatusPublished, clockNow().UTC()); err != nil {
		return err
	}
	if err := s.db.SelectContext(ctx, &d.Categories, `SELECT slug, name AS title FROM categories ORDER BY id`); err != nil {
		return err
	}
	var tagValues []string
	if err := s.db.SelectContext(ctx, &tagValues, `SELECT ai_tags FROM media WHERE ai_tags <> ''`); err != nil {
		return err
	}
	seenTags := make(map[string]struct{})
	for _, value := range tagValues {
		for _, tag := range strings.Split(value, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			if _, exists := seenTags[tag]; exists {
				continue
			}
			seenTags[tag] = struct{}{}
			d.GalleryTags = append(d.GalleryTags, tag)
		}
	}
	slices.Sort(d.GalleryTags)
	refs, err := s.resolveStructureRefs(ctx, st)
	if err != nil {
		return err
	}
	editor := func(prefix, opPrefix string, footer bool, maxTop int, items []MenuItem) *structureMenuEditor {
		e := &structureMenuEditor{Lang: lang, Prefix: prefix, OpPrefix: opPrefix, Footer: footer, CanAdd: len(items) < maxTop,
			MaxTop: maxTop, MaxChildren: maxMenuChildren, MaxLabelRunes: maxMenuLabelRunes,
			Pages: d.Pages, Categories: d.Categories, Types: menuItemTypes}
		e.Rows = structureRows(e, items, refs)
		d.Warnings = append(d.Warnings, structureRowWarnings(lang, e.Rows, footer)...)
		return e
	}
	d.Menu = editor("menu", "", false, maxMenuItems, st.Menu)
	d.Rows = d.Menu.Rows
	d.Footer = editor("footer", footerOpPrefix, true, maxFooterColumns, st.FooterMenu)
	d.FrontPage = structureSelect{Value: st.FrontPage}
	if _, ok := refs.Pages[st.FrontPage]; !ok && st.FrontPage != "" {
		d.FrontPage.Missing = st.FrontPage
		d.Warnings = append(d.Warnings, i18n.Translate(lang, "structure_warn_front_page", st.FrontPage))
	}
	if gallery.Tag != "" && d.TopGalleryCount == 0 {
		// 打ち間違い・まだ写真にタグを付けていないタグでは、トップページにギャラリーが出ない。
		d.Warnings = append(d.Warnings, i18n.Translate(lang, "structure_warn_gallery_empty", gallery.Tag))
	}
	knownCategories := make(map[string]bool, len(d.Categories))
	for _, cat := range d.Categories {
		knownCategories[cat.Slug] = true
	}
	for i, section := range st.FrontSections {
		section = clearUnusedFrontSectionFields(section)
		missingArticleTag := ""
		if section.ArticleTag != "" && !knownCategories[section.ArticleTag] {
			missingArticleTag = section.ArticleTag
		}
		effectiveLayout := ""
		if section.Type == FrontSectionGallery {
			effectiveLayout = normalizeGalleryLayout(section.Layout)
		}
		d.Sections = append(d.Sections, frontSectionForm{
			FrontSection:      section,
			Index:             i,
			Number:            i + 1,
			First:             i == 0,
			Last:              i == len(st.FrontSections)-1,
			PagesText:         strings.Join(section.Pages, ", "),
			EffectiveLayout:   effectiveLayout,
			MissingArticleTag: missingArticleTag,
		})
		for _, slug := range section.Pages {
			if _, ok := refs.Pages[slug]; !ok {
				d.Warnings = append(d.Warnings, i18n.Translate(lang, "structure_warn_front_card", slug))
			}
		}
	}
	title := i18n.Translate(lang, "structure_title")
	if errMsg != "" {
		return s.renderError(c, code, "settings_structure", title, errMsg, d)
	}
	return s.render(c, code, "settings_structure", title, d)
}

// topGalleryStatus はサイト構成の画面で、トップページのフォトギャラリーのタグの下に出す案内の文を返す。文は
// static の JavaScript（settings_structure.html）と同じ辞書の文で、{n}（枚数）・{m}（表示する最大の枚数）を埋める。
func topGalleryStatus(lang, tag string, count, limit int) string {
	key := "structure_gallery_count"
	switch {
	case tag == "":
		key = "structure_gallery_off"
	case count == 0:
		key = "structure_gallery_empty"
	}
	return strings.NewReplacer("{n}", strconv.Itoa(count), "{m}", strconv.Itoa(limit)).Replace(i18n.Translate(lang, key))
}

// LabelKey は管理画面に出す種類の名前の翻訳キー。
func (t MenuItemType) LabelKey() string { return fmt.Sprintf("structure_type_%s", t) }

// LabelKey は管理画面に出すセクションの種類の名前の翻訳キー。
func (t FrontSectionType) LabelKey() string { return fmt.Sprintf("structure_section_type_%s", t) }
