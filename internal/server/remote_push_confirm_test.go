package server

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"sorus/internal/i18n"
	"sorus/static"
)

// pushConfirmPattern は「公開サーバーへ反映する」のボタンの確認文の属性。
var pushConfirmPattern = regexp.MustCompile(`id="remote-push-settings" data-confirm-message="([^"]*)"`)

// pushConfirm は path の画面を開き、「公開サーバーへ反映する」の確認文を返す。
func pushConfirm(t *testing.T, c *client, path string) string {
	t.Helper()
	page := c.get(path)
	expectStatus(t, page, http.StatusOK)
	m := pushConfirmPattern.FindStringSubmatch(page.Body)
	if m == nil {
		t.Fatalf("%s has no push button with a confirm message", path)
	}
	return html.UnescapeString(m[1])
}

// 「公開サーバーへ反映する」（共通バナー）の確認は、外観テーマ・サイト構成・公開サーバーの3画面とも、適用中が独自テーマなら
// そのファイルもこのPCの内容で上書きすることをテーマの名前とともに伝える（日本語・英語）。標準テーマなら従来の文のまま。
func TestRemotePushConfirmMentionsCustomTheme(t *testing.T) {
	env, pcThemes, _ := newRemoteThemeEnv(t, false)
	writeTestTheme(t, pcThemes, "paper", "ペーパー")
	if err := env.local.server.themeCatalog().Reload(static.FS); err != nil {
		t.Fatal(err)
	}
	// 確認文は共通バナーのボタンに付く。未反映の記録を書いてバナーを出す。
	env.local.markAllRemoteDirty()
	paths := []string{"/admin/settings/theme", "/admin/settings/structure", "/admin/settings/remote"}
	for _, lang := range []string{"ja", "en"} {
		adm := env.local.loggedIn(admin)
		if lang == "en" {
			expectStatus(t, adm.get("/admin/set-lang?lang=en"), http.StatusSeeOther)
		}

		env.local.setTheme("news")
		builtin := i18n.Translate(lang, "remote_push_confirm")
		for _, p := range paths {
			if got := pushConfirm(t, adm, p); got != builtin {
				t.Errorf("[%s] %s confirm with a built-in theme = %q, want %q", lang, p, got, builtin)
			}
		}

		env.local.setTheme("paper")
		label := map[string]string{"ja": "ペーパー", "en": "paper"}[lang]
		want := i18n.Translate(lang, "remote_push_confirm_custom", label)
		mention := map[string][]string{
			"ja": {"独自テーマ「ペーパー」", "このPCのテーマのファイルで上書き"},
			"en": {`custom theme "paper"`, "overwritten with the theme files on this PC"},
		}[lang]
		for _, p := range paths {
			got := pushConfirm(t, adm, p)
			if got != want {
				t.Errorf("[%s] %s confirm with a custom theme = %q, want %q", lang, p, got, want)
			}
			for _, m := range mention {
				if !strings.Contains(got, m) {
					t.Errorf("[%s] %s confirm %q does not mention %q", lang, p, got, m)
				}
			}
		}
	}
}
