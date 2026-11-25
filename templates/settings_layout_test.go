package templates

import (
	"errors"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sorus/internal/i18n"
)

// TestSettingsLayout ensures that all admin settings pages adhere to the unified 2-pane layout:
// 1. Container with max-w-7xl
// 2. 2-column grid layout with md:grid-cols-[15rem_minmax(0,1fr)]
// 3. Inclusion of settings_subnav template
func TestSettingsLayout(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("admin", "settings_*.html"))
	if err != nil {
		t.Fatalf("failed to glob settings templates: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no settings templates found matching admin/settings_*.html")
	}

	// 例外は設けない。サイトの一覧・削除（settings_sites*.html）も左メニュー付きの2ペインにする（admin_ux_polish_and_mcp_spec.md §2-3）。
	checked := map[string]bool{}
	checkedCount := 0
	for _, f := range files {
		checked[filepath.Base(f)] = true

		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("failed to read %s: %v", f, err)
		}
		content := string(data)

		if !strings.Contains(content, `max-w-7xl`) {
			t.Errorf("%s: missing 'max-w-7xl' container for unified layout", f)
		}
		if !strings.Contains(content, `settings_subnav`) {
			t.Errorf("%s: missing 'settings_subnav' inclusion", f)
		}
		if !strings.Contains(content, `md:grid-cols-[15rem_minmax(0,1fr)]`) {
			t.Errorf("%s: missing 2-pane grid specification md:grid-cols-[15rem_minmax(0,1fr)]", f)
		}
		checkedCount++
	}

	if checkedCount < 10 {
		t.Errorf("expected at least 10 settings templates to be verified, got %d", checkedCount)
	}
	for _, name := range []string{"settings_sites.html", "settings_sites_delete.html"} {
		if !checked[name] {
			t.Errorf("%s was not verified for the 2-pane layout", name)
		}
	}

	// Also verify publish.html which is part of the settings section
	publishFile := filepath.Join("admin", "publish.html")
	if data, err := os.ReadFile(publishFile); err == nil {
		content := string(data)
		if !strings.Contains(content, `max-w-7xl`) {
			t.Errorf("%s: missing 'max-w-7xl' container", publishFile)
		}
		if !strings.Contains(content, `settings_subnav`) {
			t.Errorf("%s: missing 'settings_subnav' inclusion", publishFile)
		}
	}
}

// TestSettingsSubnavStructure verifies that the settings subnav is organized into
// three distinct groups ("基本設定", "コンテンツ", "管理") and enforces role-based visibility.
func TestSettingsSubnavStructure(t *testing.T) {
	subnavPath := filepath.Join("admin", "partials", "settings_subnav.html")
	data, err := os.ReadFile(subnavPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", subnavPath, err)
	}
	content := string(data)

	// Verify group headings
	requiredGroups := []string{"基本設定", "コンテンツ", "管理"}
	for _, grp := range requiredGroups {
		if !strings.Contains(content, grp) {
			t.Errorf("subnav missing required group heading %q", grp)
		}
	}

	// Verify required links
	requiredLinks := []string{
		"/admin/settings/general",
		"/admin/settings/theme",
		"/admin/publish",
		"/admin/settings/ai",
		"/admin/settings/backup",
		"/admin/settings/migration",
		"/admin/users",
		"/admin/settings/ai-integrations",
		"/admin/settings/categories",
		"/admin/settings/operations",
	}
	for _, link := range requiredLinks {
		if !strings.Contains(content, `href="`+link+`"`) {
			t.Errorf("subnav missing required link %s", link)
		}
	}
	// 公開サーバーと静的サイトは「公開先」1項目にまとめ、公開サーバーの画面（/admin/settings/remote）でも選択中にする。
	if strings.Contains(content, `href="/admin/settings/remote"`) || strings.Contains(content, `"settings_nav_remote"`) {
		t.Errorf("subnav must not have a separate publish server item")
	}
	if got := strings.Count(content, `.AtPath "/admin/publish" "/admin/settings/remote"`); got != 2 {
		t.Errorf("publishing item must be current on both /admin/publish and /admin/settings/remote (found %d conditions, want 2)", got)
	}

	// AI の3画面（AI・外部連携・使う AI・外部の AI アプリとつなぐ）は「AI」1項目にまとめ、どの画面でも選択中にする。
	// 個別の「AI設定」「MCP連携」の項目は出さない（画面の中のタブで切り替える）。
	if got := strings.Count(content, `.AtPath "/admin/settings/ai-integrations" "/admin/settings/ai" "/admin/settings/mcp"`); got != 2 {
		t.Errorf("AI item must be current on all three AI pages (found %d conditions, want 2)", got)
	}
	if strings.Contains(content, `href="/admin/settings/mcp"`) || strings.Contains(content, `.T "mcp"`) || strings.Contains(content, `.T "settings_nav_ai"`) {
		t.Errorf("subnav must not have separate AI settings / MCP items")
	}

	// 「スマートフォンで書く」はメニューから外した（admin_ux_redesign_spec.md §2-8）。
	if strings.Contains(content, "/admin/settings/mobile") || strings.Contains(content, "mobile_connect") {
		t.Errorf("subnav must not link to the removed mobile writing page")
	}

	// 左メニューの項目名は SPEC §2-5 の表どおりにする。設定の入口（handlers_settings_hub.go）と同じ辞書のキーで引き、
	// 入口と左メニューの名前がずれないようにする。
	navLabels := map[string]string{
		"settings_nav_publish": "公開先", "settings_nav_operations": "サイト運用", "settings_nav_ai_section": "AI",
		"settings_nav_backup": "バックアップと復元", "settings_nav_migration": "データの取り込み",
		"users": "", "sites_title": "",
	}
	for key, ja := range navLabels {
		if !strings.Contains(content, `.T "`+key+`"`) {
			t.Errorf("subnav missing menu label key %q", key)
		}
		if got := i18n.Translate("ja", key); ja != "" && got != ja {
			t.Errorf("menu label %q = %q, want %q", key, got, ja)
		}
	}

	// Verify role check guards
	if !strings.Contains(content, `.User.HasRole "admin"`) {
		t.Errorf("subnav missing admin role check")
	}
	if !strings.Contains(content, `.User.HasRole "admin" "editor"`) {
		t.Errorf("subnav missing editor/admin role check for categories")
	}
}

func TestThemeBadgesDoNotWrap(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("admin", "settings_theme.html"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, class := range []string{
		`class="theme-applied-badge shrink-0 whitespace-nowrap`,
		`class="theme-custom-badge shrink-0 whitespace-nowrap`,
		`class="flex flex-wrap items-center justify-between gap-2"`,
		`class="min-w-0 break-words text-base text-neutral-900"`,
	} {
		if !strings.Contains(content, class) {
			t.Errorf("theme settings missing %q", class)
		}
	}
}

func readTemplate(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func expectAll(t *testing.T, name, content string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(content, w) {
			t.Errorf("%s missing %q", name, w)
		}
	}
}

func expectNone(t *testing.T, name, content string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(content, u) {
			t.Errorf("%s must not contain %q", name, u)
		}
	}
}

// 「スマートフォンで書く」の画面は削除した（admin_ux_redesign_spec.md §2-8）。
func TestMobileSettingsPageRemoved(t *testing.T) {
	if _, err := os.Stat(filepath.Join("admin", "settings_mobile.html")); !os.IsNotExist(err) {
		t.Errorf("admin/settings_mobile.html should be removed, stat err = %v", err)
	}
}

// 管理画面の共通ボタン（.btn と、バリアント・サイズ）が admin.css に定義されている（§2-2）。
func TestAdminButtonClassesDefined(t *testing.T) {
	css := readTemplate(t, "..", "static", "css", "admin.css")
	for _, sel := range []string{".btn ", ".btn-primary", ".btn-secondary", ".btn-danger", ".btn-ghost", ".btn-sm", ".btn-md", "summary.btn", ".btn[hidden]"} {
		if !strings.Contains(css, sel) {
			t.Errorf("admin.css missing button selector %q", sel)
		}
	}
}

// 「日時を決めて予約」は、行の中では隣のボタンと同じ .btn-sm、モーダルのフッターでは .btn-md の高さにそろえる（§2-2）。
func TestSchedulePickerMatchesNeighbourButtons(t *testing.T) {
	modal := readTemplate(t, "admin", "partials", "remote_sync_modal.html")
	expectAll(t, "remote_sync_modal.html", modal,
		`<summary class="btn btn-secondary btn-sm">`,
		`data-remote-pending-refresh`+"\n"+`            class="btn btn-secondary btn-sm"`,
		`<div id="remote-sync-modal-schedule" class="btn-group-md" hidden>`,
		`id="remote-sync-modal-publish" class="btn btn-primary btn-md"`,
	)
	css := readTemplate(t, "..", "static", "css", "admin.css")
	expectAll(t, "admin.css", css, ".btn-group-md summary.btn-sm")
	js := readTemplate(t, "..", "static", "js", "remote_sync.js")
	expectAll(t, "remote_sync.js", js, "el('button', 'btn btn-primary btn-sm'", "el('button', 'btn btn-secondary btn-sm'")
	expectNone(t, "remote_sync.js", js, "px-2.5 py-1 text-xs font-medium hover:bg-neutral-50")
}

// 管理画面のテンプレートに、共通クラスを使わない従来の塗りボタン（bg-zinc-800 など）を残さない（§2-2）。
func TestAdminTemplatesUseButtonClasses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("admin", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	partials, err := filepath.Glob(filepath.Join("admin", "partials", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	// aria-selected: などの状態つきの指定（タブ）は対象外にするため、クラス名の直前が空白か引用符のものだけを見る。
	legacy := regexp.MustCompile(`<(button|a|summary)\b[^>]*class="(?:[^"]*\s)?rounded-(?:lg|md)\s(?:[^"]*\s)?bg-(?:zinc|neutral)-[6-9]00\s(?:[^"]*\s)?text-white[\s"]`)
	for _, f := range append(files, partials...) {
		if m := legacy.FindString(readTemplate(t, f)); m != "" {
			t.Errorf("%s still has a button without the shared classes: %s", f, m)
		}
	}
}

// 「公開サーバーへ反映する」はレイアウトの共通バナーに1つだけ置き、各設定画面からは撤去する（§2-3）。
func TestRemotePushBannerInLayoutOnly(t *testing.T) {
	layout := readTemplate(t, "admin", "layout.html")
	expectAll(t, "layout.html", layout,
		`{{if and .RemoteCanPushSettings (gt (len .RemoteDirty) 0)}}`,
		`id="remote-dirty-banner"`,
		`id="remote-push-settings" data-confirm-message="{{.RemotePushConfirm}}"`,
		`id="remote-push-result"`,
		`eq $k "general"`, `eq $k "theme"`, `eq $k "structure"`,
		`js/remote_sync.js`,
	)
	if n := strings.Count(layout, `id="remote-push-settings"`); n != 1 {
		t.Errorf("layout.html has %d push buttons, want 1", n)
	}
	for _, name := range []string{"settings_remote.html", "settings_theme.html", "settings_structure.html"} {
		expectNone(t, name, readTemplate(t, "admin", name), `id="remote-push-settings"`, `id="remote-push-result"`)
	}
	js := readTemplate(t, "..", "static", "js", "remote_sync.js")
	expectAll(t, "remote_sync.js", js, "window.__sorusRemoteSyncLoaded", "/admin/remote/settings/push", "sorus.remoteDirty.clear()")
	common := readTemplate(t, "..", "static", "js", "sorus_common.js")
	expectAll(t, "sorus_common.js", common, "remote-dirty-items", "remote-dirty-banner", "data-remote-dirty-label-")
}

// 管理画面の共通部品（static/js/sorus_common.js の window.sorus）は、レイアウトが admin.js の後・各画面のスクリプトより前に
// 読み込み、差分の表・日時・要求・未反映のバナーは各スクリプトで作り直さず共通部品を使う。
func TestAdminScriptsUseSharedCommon(t *testing.T) {
	layout := readTemplate(t, "admin", "layout.html")
	admin, common, scripts := strings.Index(layout, `{{asset "js/admin.js"}}`), strings.Index(layout, `{{asset "js/sorus_common.js"}}`), strings.Index(layout, `{{block "scripts" .}}`)
	if admin < 0 || common < 0 || scripts < 0 || !(admin < common && common < scripts) {
		t.Errorf("layout.html must load sorus_common.js after admin.js and before the page scripts (admin=%d common=%d scripts=%d)", admin, common, scripts)
	}
	shared := readTemplate(t, "..", "static", "js", "sorus_common.js")
	expectAll(t, "sorus_common.js", shared, "window.sorus = {", "csrf: csrf", "el: el", "toast: toast", "requestJSON: requestJSON",
		"postJSON: postJSON", "fmtDate: fmtDate", "diffTable: diffTable", "confirmTakeover: confirmTakeover", "remoteDirty: remoteDirty")

	ms := readTemplate(t, "..", "static", "js", "manuscript.js")
	expectAll(t, "manuscript.js", ms, "sorus.diffTable(", "sorus.postJSON", "sorus.confirmTakeover(", "'btn btn-primary btn-md'", "'btn btn-secondary btn-md'")
	expectNone(t, "manuscript.js", ms, "bg-zinc-900", "rounded-lg border border-neutral-300 px-4", "function csrf(", "fetch(", "var opClass")
	expectAll(t, "manuscripts.html", readTemplate(t, "admin", "manuscripts.html"), `{{asset "js/manuscript.js"}}`)
	expectNone(t, "manuscripts.html", readTemplate(t, "admin", "manuscripts.html"), "/admin/manuscripts/manuscript.js")

	rs := readTemplate(t, "..", "static", "js", "remote_sync.js")
	expectAll(t, "remote_sync.js", rs, "sorus.diffTable(", "sorus.fmtDate", "sorus.postJSON(", "sorus.confirmTakeover(", "sorus.toast", "sorus.el")
	expectNone(t, "remote_sync.js", rs, "fetch(", "function fmtDate", "conflictClass", "Intl.DateTimeFormat")

	media := readTemplate(t, "..", "static", "js", "media.js")
	expectAll(t, "media.js", media, "window.sorus.remoteDirty.add('media')", "function articleRow(", "function loadArticles(")
	page := readTemplate(t, "admin", "media.html")
	expectNone(t, "media.html", page, "<script>", "function articleRow(")
}

// 公開サーバーの画面は接続・参加・招待・詳細設定に絞り、閲覧制限・メンテナンス表示は「サイト運用」へ移す（§2-6）。
func TestRemoteAndOperationsSeparated(t *testing.T) {
	remote := readTemplate(t, "admin", "settings_remote.html")
	expectAll(t, "settings_remote.html", remote, `id="remote-join-form"`, `data-remote-invites`, `id="remote-advanced"`, `href="/admin/settings/operations"`)
	expectNone(t, "settings_remote.html", remote, `id="remote-site-access"`, `id="remote-maintenance"`, `remote_pending_panel`, `site_access.js`, `site-access.js`, `remote_maintenance.js`)

	ops := readTemplate(t, "admin", "settings_operations.html")
	if got := i18n.Translate("ja", "operations_title"); got != "サイト運用（メンテナンス・閲覧制限）" {
		t.Errorf("operations_title = %q", got)
	}
	expectAll(t, "settings_operations.html", ops,
		`{{$.T "operations_title"}}`,
		`id="remote-site-access"`, `data-remote-site-access`, `id="remote-site-access-form"`, `id="remote_access_enabled"`,
		`id="remote-maintenance"`, `data-remote-maintenance`, `id="remote-maintenance-toggle"`, `id="remote-maintenance-local-form"`,
		`{{asset "js/remote_site_access.js"}}`, `js/remote_maintenance.js`,
		`静的サイト公開`, `href="/admin/settings/remote"`,
	)
}

// 執筆者名の表示の説明に、特定分野の言葉（研究室）や存在しない「組織名」を出さない（§2-4）。
func TestShowAuthorDescription(t *testing.T) {
	general := readTemplate(t, "admin", "settings_general.html")
	expectNone(t, "settings_general.html", general, "研究室", "組織名")
	expectAll(t, "settings_general.html", general, "サイト名による公式発信", "構造化データの著者にもサイト名")
}

// AI設定はプロバイダーごとにキーを保存でき、メインAI・翻訳AIを選べる。記事エディタでも使うAIを切り替えられる（§2-7）。
func TestAISettingsPerProvider(t *testing.T) {
	ai := readTemplate(t, "admin", "settings_ai.html")
	expectAll(t, "settings_ai.html", ai,
		`range $d.ProviderConfigs`,
		`name="api_key_{{$key}}"`, `name="model_{{$key}}"`, `name="base_url_{{$key}}"`, `name="clear_key_{{$key}}"`,
		`name="provider"`, `name="translation_provider"`, `data-ai-test="{{$key}}"`, `test_provider`,
	)
	expectNone(t, "settings_ai.html", ai, `name="api_key"`, `name="deepl_api_key"`, `HasSavedDeepLKey`)

	modal := readTemplate(t, "admin", "partials", "ai_modal.html")
	expectAll(t, "ai_modal.html", modal, `.AIConfiguredProviders`, `id="ai-provider-select"`)
	editor := readTemplate(t, "..", "static", "js", "editor.js")
	expectAll(t, "editor.js", editor, `getElementById('ai-provider-select')`, `body.set('provider'`)
}

// MCP連携は、上段に一般向けの案内（接続URL・公式サイトへのリンク・2つの接続方法）、中段に連携中のアプリと
// APIトークン、下段の折りたたみに「高度な設定」を置く（admin_ux_polish_and_mcp_spec.md §2-6）。
func TestMCPPageLayers(t *testing.T) {
	mcp := readTemplate(t, "admin", "mcp_tokens.html")
	expectAll(t, "mcp_tokens.html", mcp,
		`id="mcp-intro"`, `id="mcp-connect-oauth"`, `id="mcp-connect-local"`, `<details id="mcp-developer"`,
		"高度な設定（接続URL・クライアント設定例）", "OAuth 2.1", "127.0.0.1", "Bearer",
		"ChatGPT / Claude / Gemini / Perplexity",
		`href="https://chatgpt.com/" target="_blank" rel="noopener noreferrer"`,
		`href="https://claude.ai/" target="_blank" rel="noopener noreferrer"`,
		`id="oauth-apps"`, `{{range $d.OAuthApps}}`, `{{.ClientName}}`, `action="/admin/settings/mcp/oauth/{{.ID}}/revoke"`,
		`class="help-tip"`, `data-copy-target="#mcp-endpoint"`,
	)
	expectNone(t, "mcp_tokens.html", mcp, "開発者・エンジニア向け設定", "Settings for developers and engineers", "Skywork")
	// 2つの接続URL（ローカル・公開サーバー）を用途で対比し、高度な設定の例はどちらの URL にするかを切り替えられる。
	// 公開サーバーが無いときは独自ドメインへの読み替えを添える（admin_ux_and_desktop_polish_spec.md §1-4）。
	expectAll(t, "mcp_tokens.html", mcp,
		"この PC 専用の接続URL（ローカル）", "URL for this PC only (local)",
		"どこからでも使える接続URL（公開サーバー）", "URL that works from anywhere (public server)",
		`{{if $d.EndpointLocal}}`, `{{with $d.Remote}}{{$remoteURL = .Endpoint}}{{end}}`, `id="mcp-endpoint-remote"`,
		`data-mcp-url-switch data-local-url="{{$d.Endpoint}}" data-remote-url="{{$remoteURL}}"`,
		`name="mcp-snippet-target" value="local" checked`, `name="mcp-snippet-target" value="remote"`,
		`<span data-mcp-url>{{$d.Endpoint}}</span>`, `<span data-mcp-auth>`,
		`id="mcp-snippet-domain-note"`, "https://あなたのドメイン{{$d.EndpointPath}}", "https://your-domain{{$d.EndpointPath}}",
	)
	if n := strings.Count(mcp, `<span data-mcp-url>`); n != 4 {
		t.Errorf("mcp_tokens.html: every settings example (4) must use the switchable URL, got %d", n)
	}
	if n := strings.Count(mcp, `<span data-mcp-auth>`); n != 4 {
		t.Errorf("mcp_tokens.html: every settings example (4) must use the switchable token, got %d", n)
	}
	intro, oauth := strings.Index(mcp, `id="mcp-intro"`), strings.Index(mcp, `id="oauth-apps"`)
	dev, setup := strings.Index(mcp, `<details id="mcp-developer"`), strings.Index(mcp, `id="mcp-setup"`)
	if !(intro < oauth && oauth < dev && dev < setup) {
		t.Errorf("mcp_tokens.html: want intro (%d) < connected apps (%d) < advanced details (%d) wrapping the setup (%d)", intro, oauth, dev, setup)
	}
}

// 統一ヘルプポップオーバー（.help-tip）の見た目と動き（クリックで開閉・1つだけ開く・Esc と外側クリックで閉じる・
// 画面の端からはみ出さない）が admin.css・admin.js にある（admin_ux_polish_and_mcp_spec.md §2-1）。
func TestHelpTipComponent(t *testing.T) {
	css := readTemplate(t, "..", "static", "css", "admin.css")
	expectAll(t, "admin.css", css, ".help-tip {", ".help-tip-btn {", ".help-tip-btn[aria-expanded=\"true\"]", ".help-tip-pop {", ".help-tip-pop[hidden]")
	js := readTemplate(t, "..", "static", "js", "admin.js")
	expectAll(t, "admin.js", js,
		"closest('.help-tip-btn')", "setAttribute('aria-expanded', 'true')", "setAttribute('aria-expanded', 'false')",
		"aria-controls", "e.key === 'Escape'", "closeTip(false)", "function placeTip", "vw - w - TIP_MARGIN",
		"addEventListener('resize'",
	)
}

// helpTipBtn は「?」ボタンの決まった書き方。閉じた状態（aria-expanded="false"）で始め、読み上げ用の名前を付ける。
var (
	helpTipBtn = regexp.MustCompile(`<button type="button" class="help-tip-btn" aria-expanded="false" aria-label="[^"]+">\?</button>`)
	helpTipPop = regexp.MustCompile(`<span class="help-tip-pop"[^>]*>`)
)

// 各設定画面の長い説明は「?」のポップオーバーに入れ、画面にはラベルと最小限の文だけを出す（§2-1）。
func TestSettingsPagesUseHelpTips(t *testing.T) {
	pages := []string{
		"settings_general.html", "settings_ai.html", "settings_theme.html", "settings_structure.html", "settings_remote.html",
		"settings_operations.html", "publish.html", "settings_backup.html", "settings_import.html", "settings_categories.html",
		"users.html", "mcp_tokens.html", "settings_sites.html",
	}
	for _, name := range pages {
		content := readTemplate(t, "admin", name)
		tips := strings.Count(content, `<span class="help-tip">`)
		if tips == 0 {
			t.Errorf("%s has no help tips", name)
			continue
		}
		if n := len(helpTipBtn.FindAllString(content, -1)); n != tips {
			t.Errorf("%s: %d help tips but %d well-formed ? buttons", name, tips, n)
		}
		pops := helpTipPop.FindAllString(content, -1)
		if len(pops) != tips {
			t.Errorf("%s: %d help tips but %d popovers", name, tips, len(pops))
		}
		for _, pop := range pops {
			if !strings.Contains(pop, " hidden>") {
				t.Errorf("%s: popover must start hidden: %s", name, pop)
			}
		}
		// 「?」の名前（$help）を、その画面で表示言語に合わせて用意している。
		expectAll(t, name, content, `{{$help := "Show help"}}{{if $ja}}{{$help = "説明を表示"}}{{end}}`)
	}

	// 項目の下に並んでいた長い説明の段落は残さない。
	for name, unwanted := range map[string][]string{
		"settings_general.html":    {`<p class="text-xs text-neutral-500">`, `<p class="mt-1 text-xs text-neutral-500">`},
		"settings_ai.html":         {`<p class="mt-0.5 text-xs text-neutral-500">`, `<p class="text-xs text-neutral-500 mb-1.5">`},
		"publish.html":             {`<p class="mt-1 text-xs text-neutral-500">{{if $ja}}`, `<p class="mt-1 pl-6 text-xs text-neutral-500">`},
		"settings_remote.html":     {"<p class=\"text-xs text-neutral-500\">\n"},
		"settings_operations.html": {`id="remote-site-access-cache-note"`, `id="remote-maintenance-cache-note"`},
		"settings_import.html":     {`id="import-how-title"`},
		"mcp_tokens.html":          {`border-amber-200 bg-amber-50`},
	} {
		expectNone(t, name, readTemplate(t, "admin", name), unwanted...)
	}
}

// 多言語対応を無効にしたときは、英語のサイト名・言語フィルター・翻訳の設定と操作を出さない（§2-5）。
// 値はページのデータ（render.go の page.MultilingualEnabled）から引く。
func TestMultilingualToggleUI(t *testing.T) {
	general := readTemplate(t, "admin", "settings_general.html")
	expectAll(t, "settings_general.html", general,
		`name="multilingual_enabled" value="1" {{if $.MultilingualEnabled}}checked{{end}}`,
		`<div id="site-name-en-field"{{if not $.MultilingualEnabled}} hidden{{end}}>`,
		"enField.hidden = !multi.checked",
		// 公開サイトの多言語対応と管理画面の表示言語は別の設定であることを、ラベルと「?」で伝える（admin_ux_and_desktop_polish_spec.md §1-3）。
		"公開サイトの多言語対応（英語ページ /en/ を使う）", "Multilingual public site (use English pages at /en/)",
		"右上のアカウントメニュー", "account menu in the top right",
	)
	expectNone(t, "settings_general.html", general, "多言語対応（英語ページ・翻訳機能）を有効にする")
	if i, j := strings.Index(general, `id="site-name-en-field"`), strings.Index(general, `name="site_name_en"`); i < 0 || j < i {
		t.Errorf("settings_general.html: the English site name must be inside #site-name-en-field")
	}

	list := readTemplate(t, "admin", "article_list.html")
	if i, j := strings.Index(list, `{{if $.MultilingualEnabled}}`), strings.Index(list, `id="lang-filter"`); i < 0 || j < i {
		t.Errorf("article_list.html: the language filter must be wrapped in {{if $.MultilingualEnabled}}")
	}

	ai := readTemplate(t, "admin", "settings_ai.html")
	expectAll(t, "settings_ai.html", ai,
		`{{if or (ne $key "deepl") $.MultilingualEnabled}}`,
		`<input type="hidden" name="translation_provider" value="{{$d.Form.TranslationProvider}}">`,
		`<input type="hidden" name="glossary" value="{{$d.Form.Glossary}}">`,
	)
	if i, j := strings.Index(ai, "{{if $.MultilingualEnabled}}\n      <div class=\"mb-4\">"), strings.Index(ai, `id="ai-trans-provider"`); i < 0 || j < i {
		t.Errorf("settings_ai.html: the translation engine must be wrapped in {{if $.MultilingualEnabled}}")
	}

	edit := readTemplate(t, "admin", "article_edit.html")
	expectAll(t, "article_edit.html", edit,
		`{{$ml := $.MultilingualEnabled}}`,
		`{{if and $d.IsOutdated $ml}}`,
		"{{if and $d.SourceArticle $ml}}\n    <div id=\"source-article-banner\"",
		"{{if and $d.SourceArticle $ml}}\n      <section id=\"split-source-pane\"",
		`{{if and $d.ShowSplitView $ml}}hidden{{else}}max-lg:hidden{{end}}`,
	)
}

// ユーザー管理は一覧を全幅にし、新規登録はボタンから開くモーダルで行う（§2-10）。
func TestUsersPageModal(t *testing.T) {
	users := readTemplate(t, "admin", "users.html")
	expectAll(t, "users.html", users, `id="add-user-open"`, `<dialog id="add-user-modal"`, `action="/admin/users" id="add-user-form"`, `whitespace-nowrap`, `w-full rounded-xl`)
	expectNone(t, "users.html", users, `lg:grid-cols-[minmax(0,1fr)_22rem]`)
}

// 管理画面のテンプレートは、サーバーと同じ組み立て（layout.html とパーシャルに各ページを重ねる）で構文エラーなく読める。
// テンプレート関数はサーバー側（internal/server）で登録するため、ここでは名前だけの代わりの関数で読む。
func TestAdminTemplatesParse(t *testing.T) {
	funcs := template.FuncMap{}
	undefined := regexp.MustCompile(`function "([^"]+)" not defined`)
	// parse は未登録の関数に出会うたびに、その名前の代わりの関数を足して読み直す。
	parse := func(build func() (*template.Template, error)) (*template.Template, error) {
		for range 100 {
			tmpl, err := build()
			if err == nil {
				return tmpl, nil
			}
			m := undefined.FindStringSubmatch(err.Error())
			if m == nil {
				return nil, err
			}
			funcs[m[1]] = func(...any) any { return nil }
		}
		return nil, errors.New("too many undefined functions")
	}
	base, err := parse(func() (*template.Template, error) {
		return template.New("admin").Funcs(funcs).ParseFS(FS, "admin/layout.html", "admin/partials/*.html")
	})
	if err != nil {
		t.Fatalf("parse layout and partials: %v", err)
	}
	pages, err := fs.Glob(FS, "admin/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range pages {
		if page == "admin/layout.html" {
			continue
		}
		if _, err := parse(func() (*template.Template, error) {
			clone, err := base.Clone()
			if err != nil {
				return nil, err
			}
			return clone.Funcs(funcs).ParseFS(FS, page)
		}); err != nil {
			t.Errorf("parse %s: %v", page, err)
		}
	}
}
