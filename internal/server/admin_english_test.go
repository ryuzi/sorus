package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// 管理画面を英語で表示したときに、Go から返す部分（更新のお知らせ・自動バックアップの状態）や
// エラーの文言に日本語が残らないことを確かめるテスト。

// japaneseIn は s に含まれる日本語（ひらがな・カタカナ・漢字・全角記号）を返す。無ければ空。
func japaneseIn(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Han, r) ||
			(r >= 0x3000 && r <= 0x303f) || (r >= 0xff01 && r <= 0xff60) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func expectNoJapanese(t *testing.T, what, body string) {
	t.Helper()
	if ja := japaneseIn(body); ja != "" {
		t.Errorf("%s contains Japanese %q:\n%s", what, ja, body)
	}
}

func TestUpdateBannerFollowsDisplayLanguage(t *testing.T) {
	withRunningVersion(t, "v1.0.0")
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	if err := env.server.putSetting(updateLatestSetting, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	expectContains(t, c.get(updateBannerPath).Body, "新しいバージョン（v1.1.0）があります", "「sorus を終了」", "ターミナルを開き",
		`data-copied-label="コピーしました"`, ">コピー</button>", ">閉じる</button>")

	c.useEnglish()
	banner := c.get(updateBannerPath).Body
	expectContains(t, banner, "A new version (v1.1.0) is available", "Quit sorus", "then open Terminal,",
		`data-copied-label="Copied"`, ">Copy</button>", ">Close</button>")
	expectNoJapanese(t, "English update banner", banner)
}

func TestUpdateBannerUsesPowerShellOnWindows(t *testing.T) {
	d := newUpdateBannerData("en", "v2.0.0", "windows")
	if !strings.Contains(d.Body, "open PowerShell,") {
		t.Errorf("windows body = %q", d.Body)
	}
	if d := newUpdateBannerData("ja", "v2.0.0", "windows"); !strings.Contains(d.Body, "PowerShellを開き") {
		t.Errorf("windows JA body = %q", d.Body)
	}
}

func TestBackupStatusFollowsDisplayLanguage(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	c.useEnglish()

	// 保存先が未設定。
	status := c.get(backupStatusPath).Body
	expectContains(t, status, "Automatic backup", "No backup folder has been chosen yet", "Choose a backup folder")
	expectNoJapanese(t, "English backup status (no folder)", status)

	// 保存先の入力の誤り（相対パス）。
	res := c.postForm(autoBackupPath, url.Values{"folder": {"backups"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "Enter the full folder location")

	// 最後のバックアップが失敗（辞書のキーで保存した理由を、表示言語で出す）。
	folder := filepath.Join(t.TempDir(), "backups")
	at := time.Date(2026, 11, 3, 9, 30, 0, 0, time.Local)
	saveStatus := func(st autoBackupStatus) {
		t.Helper()
		raw, _ := json.Marshal(st)
		if err := env.server.putSetting(autoBackupFolderSetting, folder); err != nil {
			t.Fatal(err)
		}
		if err := env.server.putSetting(autoBackupLastSetting, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	saveStatus(autoBackupStatus{At: at.UTC(), OK: false, Message: "backup_auto_err_not_found"})
	failed := c.get(backupStatusPath).Body
	expectContains(t, failed, `data-backup-result="failed"`, "The backup at Nov 3, 2026 09:30 failed",
		"The backup folder was not found", "Folder: "+folder, "Backup settings")
	expectNoJapanese(t, "English backup status (failed)", failed)
	page := c.get(backupPagePath).Body
	expectContains(t, page, "The backup at Nov 3, 2026 09:30 failed", "The backup folder was not found")

	// 成功したが古いバックアップを消せなかった（下段の1行の表示）。
	saveStatus(autoBackupStatus{At: at.UTC(), OK: true, File: "sorus_backup_x.zip", Warning: "backup_auto_prune_warning"})
	compact := c.get(backupStatusPath + "?compact=1").Body
	expectContains(t, compact, "data-backup-compact", "Last backup: <span class=\"tabular-nums\">Nov 3, 2026 09:30</span></p>",
		"Could not remove old backups")
	expectNotContains(t, compact, "(succeeded)")
	expectNoJapanese(t, "English backup status (compact)", compact)

	// 通知（?flash=）も表示言語で出す。
	expectContains(t, c.get(backupPagePath+"?flash=backup_auto_off").Body, "Automatic backup turned off")

	// 日本語では従来どおりの文言と日時の形。
	c.useJapanese()
	ja := c.get(backupStatusPath + "?compact=1").Body
	expectContains(t, ja, "最後のバックアップ: <span class=\"tabular-nums\">2026年11月3日 09:30</span></p>",
		"古いバックアップを消せませんでした")
	expectNotContains(t, ja, "うまくいきました")
	expectContains(t, c.get(backupPagePath+"?flash=backup_auto_off").Body, "自動バックアップをやめました")
}

// useJapanese は管理画面の表示言語を日本語にする。
func (c *client) useJapanese() {
	c.t.Helper()
	expectStatus(c.t, c.get("/admin/set-lang?lang=ja&return=/admin"), http.StatusSeeOther)
}

func TestAutoBackupStatusKeepsLegacyMessages(t *testing.T) {
	st := autoBackupStatus{Message: "保存先に書き込めませんでした", Warning: "古い警告"}
	if got := st.MessageFor("en"); got != st.Message {
		t.Errorf("legacy message = %q", got)
	}
	if got := st.WarningFor("en"); got != st.Warning {
		t.Errorf("legacy warning = %q", got)
	}
	st = autoBackupStatus{Message: autoBackupFailureKey(nil)}
	if got := st.MessageFor("ja"); !strings.Contains(got, "sorus.log") || japaneseIn(got) == "" {
		t.Errorf("JA generic message = %q", got)
	}
	if got := st.MessageFor("en"); !strings.Contains(got, "sorus.log") || japaneseIn(got) != "" {
		t.Errorf("EN generic message = %q", got)
	}
}

func TestGeneralSettingsErrorsFollowDisplayLanguage(t *testing.T) {
	env := newTestEnv(t)
	c := env.loggedIn(admin)
	c.useEnglish()
	for _, tc := range []struct {
		field, value, want string
	}{
		{"site_name", strings.Repeat("a", 101), "The site name must be 100 characters or fewer"},
		{"base_url", "example.jp", "Enter the public site URL in the form https://example.jp"},
		{"analytics_dashboard_url", "javascript:alert(1)", "Enter a detailed analytics URL that starts with https://"},
	} {
		form := generalSettingsForm()
		form.Set(tc.field, tc.value)
		res := c.postForm("/admin/settings/general", form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, tc.want)
	}
}

func TestSettingsHubCardsAreTranslated(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleEditor, auth.RoleWriter} {
		u := &auth.User{Role: role}
		for _, lang := range []string{"ja", "en"} {
			d := settingsHubFor(u, true, lang)
			for _, card := range append(d.Cards, d.Advanced...) {
				if card.Label == "" || card.Desc == "" {
					t.Errorf("%s/%s: card %s has an empty label or description: %+v", role, lang, card.ID, card)
				}
				if lang == "en" && japaneseIn(card.Label+card.Desc) != "" {
					t.Errorf("%s/en: card %s is not translated: %+v", role, card.ID, card)
				}
			}
		}
	}
	if got := translateOr("en", "hub_import_desc"); strings.Contains(got, "moving away from WordPress") || !strings.Contains(got, "WordPress") {
		t.Errorf("hub_import_desc (en) = %q, want a description for any external service", got)
	}
	if got := translateOr("en", "no_such_key"); got != "" {
		t.Errorf("unknown key = %q, want empty", got)
	}
}

// 多言語対応が OFF のときは、記事一覧に言語のバッジ（JA・EN）と「+ EN」を出さない。
func TestArticleRowsHideLanguageBadgesWhenMultilingualOff(t *testing.T) {
	env := newTestEnvWith(t, Config{BaseURL: testBaseURL}, func(db *sqlx.DB) {
		seedBilingualPairInto(t, db)
		seedArticle(t, db, article.Article{ID: "ja-only", Title: "翻訳のない記事", Slug: "ja-only", ContentMarkdown: "本文",
			Status: article.StatusDraft, Lang: "ja"})
	})
	adm := env.loggedIn(admin)
	badges := []string{`source_article_id=ja-only`, `font-semibold text-neutral-700">JA</span>`, ">+ EN</a>"}

	saveMultilingual(t, adm, true)
	expectContains(t, adm.get("/admin/articles").Body, badges...)

	saveMultilingual(t, adm, false)
	list := adm.get("/admin/articles").Body
	expectContains(t, list, `data-article-id="ja-only"`, "翻訳のない記事")
	expectNotContains(t, list, badges...)
}
