package server

import (
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
)

// settingsHubPath は設定の入口の画面。上部メニューの「設定」から開き、各設定の画面をカードで並べる。
const settingsHubPath = "/admin/settings"

func init() {
	if !slices.Contains(pageNames, "settings_hub") {
		pageNames = append(pageNames, "settings_hub")
	}
	templateFuncs["adminNavSection"] = adminNavSection
}

// registerSettingsHubRoutes は設定の入口を登録する。並べる項目は表示する人が使えるものだけ（settingsHubFor）
// なので、入口そのものはログイン中の全員が開ける。
func (s *Server) registerSettingsHubRoutes(authed *echo.Group) {
	authed.GET("/settings", s.handleSettingsHub)
	// 「AI・外部連携」（handlers_ai_integrations.go）は AI 機能が OFF でも開ける（ON に戻す入口）。管理者だけ。
	adminOnly := auth.RequireRole(auth.RoleAdmin)
	authed.GET(aiIntegrationsPath[len("/admin"):], s.handleAIIntegrations, adminOnly)
	authed.POST(aiIntegrationsPath[len("/admin"):], s.handleAIIntegrationsSave, adminOnly)
}

// settingsHubCard は設定の入口に並べる1項目。Desc は1行の説明。
type settingsHubCard struct {
	Href, ID, Label, Desc string
}

// settingsHubData は設定の入口に表示するカテゴリと追加項目。
type settingsHubData struct {
	Cards, Advanced []settingsHubCard
}

// settingsHubFor は u が使える設定の画面を、入口に並べる順で返す。役割の条件は各画面のルート
// （server.go の routes・registerPublishRoutes など）と同じにし、auth.User.HasRole で判定する。
// 表示名は左のメニュー（settings_subnav.html）と同じキーを使い、並びは管理画面の情報設計（SPEC §2-5）に合わせる。
func settingsHubFor(u *auth.User, hasSites bool, lang string) settingsHubData {
	card := func(href, id, label, desc string) settingsHubCard {
		return settingsHubCard{Href: href, ID: id, Label: translateOr(lang, label), Desc: translateOr(lang, desc)}
	}
	admin := u.HasRole(auth.RoleAdmin)
	var d settingsHubData
	if admin {
		d.Cards = append(d.Cards,
			card("/admin/settings/general", "hub-general", "settings_nav_general", "hub_general_desc"),
			card("/admin/settings/theme", "hub-appearance", "settings_nav_appearance", "hub_appearance_desc"),
			// 公開方法は「公開サーバー」か「静的サイト」のどちらか1つなので、入口も「公開先」1枚にまとめる。
			// 公開サーバーの画面（/admin/settings/remote）へは「公開先」から進む（handlePublishPage）。
			card("/admin/publish", "hub-publish", "settings_nav_publish", "hub_publish_section_desc"),
			card(operationsPagePath, "hub-operations", "settings_nav_operations", "hub_operations_desc"),
			// AI 機能の ON/OFF・使う AI・外部の AI アプリとの連携は「AI」1枚にまとめる（画面の中はタブ。AI 機能が OFF でも出す）。
			card(aiIntegrationsPath, "hub-ai", "settings_nav_ai_section", "hub_ai_section_desc"))
	} else {
		// 管理者以外は文章スタイルと外部の AI アプリとの連携（AI 機能が OFF なら出さない）。
		d.Cards = append(d.Cards, card(aiSettingsPath, "hub-ai-member", "settings_nav_ai_section", "hub_ai_section_member_desc"))
	}
	if admin || u.HasRole(auth.RoleEditor) {
		d.Cards = append(d.Cards, card(categorySettingsPath, "hub-categories", "categories", "hub_categories_desc"))
	}
	if admin {
		d.Cards = append(d.Cards,
			card("/admin/settings/backup", "hub-backup", "settings_nav_backup", "hub_backup_desc"),
			card(migrationPagePath, "hub-migration", "settings_nav_migration", "hub_import_desc"),
			card("/admin/users", "hub-users", "users", "hub_users_desc"))
		if hasSites {
			d.Cards = append(d.Cards, card(sitesPagePath, "hub-sites", "sites", "hub_sites_desc"))
		}
	}
	return d
}

// migrationPagePath は「データの取り込み」の画面（取り込み元を選ぶ。handlers_import.go の handleImportPage）。
const migrationPagePath = "/admin/settings/migration"

// translateOr は key を lang に翻訳する。辞書に無いキーは、キー名そのものが画面に出ないよう空文字にする
// （入口のカードの文言がそろっていることは TestSettingsHubCardsAreTranslated が確かめる）。
func translateOr(lang, key string) string {
	if !i18n.Has(key) {
		return ""
	}
	return i18n.Translate(lang, key)
}

func (s *Server) handleSettingsHub(c echo.Context) error {
	lang := string(language(c))
	u, _ := auth.UserFromContext(c)
	d := settingsHubFor(u, s.cfg.Sites != nil, lang)
	if !s.aiEnabled() {
		d = d.withoutAI()
	}
	return s.render(c, http.StatusOK, "settings_hub", i18n.Translate(lang, "settings"), d)
}

// aiHubCardIDs は AI 機能が OFF のサイトで設定の入口に出さない項目（管理者以外の「AI」。管理者の「AI」は ON に戻す入口として残す）。
var aiHubCardIDs = map[string]bool{"hub-ai-member": true}

// withoutAI は d から AI が関わる項目（aiHubCardIDs）を除いたものを返す。
func (d settingsHubData) withoutAI() settingsHubData {
	keep := func(cards []settingsHubCard) []settingsHubCard {
		var out []settingsHubCard
		for _, c := range cards {
			if !aiHubCardIDs[c.ID] {
				out = append(out, c)
			}
		}
		return out
	}
	return settingsHubData{Cards: keep(d.Cards), Advanced: keep(d.Advanced)}
}

// adminNavSection は表示中の画面の URL（クエリ付きでもよい）が、上部メニューのどの項目に属するかを返す
// （home・articles・manuscripts・media・settings。どれでもなければ空）。メニューの aria-current に使う。
// 公開先（公開サーバーの画面を含む）・ユーザー管理など、設定の入口から開く画面は「設定」に属する。
func adminNavSection(requestURI string) string {
	p, _, _ := strings.Cut(requestURI, "?")
	under := func(prefixes ...string) bool {
		for _, prefix := range prefixes {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return true
			}
		}
		return false
	}
	switch {
	case p == "/admin" || p == "/admin/":
		return "home"
	case under("/admin/articles"):
		return "articles"
	case under("/admin/manuscripts"):
		return "manuscripts"
	case under("/admin/media"):
		return "media"
	case under(settingsHubPath, "/admin/publish", "/admin/users", importPagePath):
		return "settings"
	}
	return ""
}
