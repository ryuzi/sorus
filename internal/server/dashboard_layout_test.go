package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/backup"
)

// indexOf は body の中で s が最初に現れる位置を返す。無ければテストを止める。
func indexOf(t *testing.T, body, s string) int {
	t.Helper()
	i := strings.Index(body, s)
	if i < 0 {
		t.Fatalf("body does not contain %q", s)
	}
	return i
}

// 上部ナビの入口とダッシュボードの見出しは同じ名前（JA=ダッシュボード / EN=Dashboard）。
func TestDashboardNavMatchesHeading(t *testing.T) {
	env := newPagesEnv(t, Config{})
	c := env.loggedIn(admin)
	body := c.get("/admin").Body
	expectContains(t, body, `aria-current="page">ダッシュボード</a>`, `<h1 class="text-xl font-bold text-neutral-900">ダッシュボード</h1>`)
	expectNotContains(t, body, `>ホーム</a>`)
	c.useEnglish()
	body = c.get("/admin").Body
	expectContains(t, body, `aria-current="page">Dashboard</a>`, `<h1 class="text-xl font-bold text-neutral-900">Dashboard</h1>`)
	expectNotContains(t, body, `>Home</a>`)
}

// 週間人気記事: 公開中の記事は公開ページ（新しいタブ。公開サイトの URL があれば本番の URL）、未公開の記事は編集画面へ。各行の右に「編集」を置く
// （記事を編集できるライター・編集者・管理者だけ）。
func TestDashboardPopularLinks(t *testing.T) {
	env := newPublicEnv(t)
	keion := env.articleIDBySlug("keion-live")
	draft := seedArticle(t, env.db, article.Article{Title: "まだ下書きの記事", Slug: "still-draft", ContentMarkdown: "本文",
		AuthorID: studentA.ID, Status: article.StatusDraft})
	seedViews(t, env, keion, 0, 50, 40)
	seedViews(t, env, draft, 0, 20, 10)

	popular := func(c *client) string {
		t.Helper()
		res := c.get("/admin")
		expectStatus(t, res, http.StatusOK)
		return res.Body[indexOf(t, res.Body, `id="popular-articles"`):]
	}
	for _, u := range []testUser{studentA, chief, admin} {
		pop := popular(env.loggedIn(u))
		expectContains(t, pop,
			`<a href="`+testBaseURL+`/articles/keion-live" target="_blank" rel="noopener" class="popular-title`,
			`<a href="/admin/articles/`+draft+`/edit" class="popular-title`, "未公開",
			`<a href="/admin/articles/`+keion+`/edit" class="popular-edit btn btn-secondary btn-sm shrink-0"`,
			`<a href="/admin/articles/`+draft+`/edit" class="popular-edit btn btn-secondary btn-sm shrink-0"`)
		// 公開中の記事のタイトルは編集画面へ行かない（編集は右の「編集」から）。
		expectNotContains(t, pop, `<a href="/admin/articles/`+keion+`/edit" class="popular-title`)
	}

	c := env.loggedIn(studentA)
	c.useEnglish()
	expectContains(t, popular(c), "Not public", ">Edit</a>", "opens the public page in a new tab")
}

// 「サイトに反映」: 最後の反映が失敗・予約時刻を過ぎた記事の確認が要るときだけファーストビュー上部に出す。
// 公開先が未設定なら何も出さない（「サイトを公開するには」の案内も出さない）。正常に動いているときは下段に1行（compact）で出す。
func TestDashboardPublishPanelPlacement(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	const top, bottom = `data-publish-panel="/admin/publish/panel"`, `data-publish-panel="/admin/publish/panel?compact=1"`

	// 未設定: ファーストビュー上部には枠を置かない。下段の枠の中身は空（204）で、枠は隠れたまま。
	body := c.get("/admin").Body
	expectNotContains(t, body, top, "サイトを公開するには", `id="publish-dashboard-setup"`)
	for _, path := range []string{"/admin/publish/panel?compact=1", "/admin/publish/panel"} {
		res := c.get(path)
		expectStatus(t, res, http.StatusNoContent)
		if res.Body != "" {
			t.Errorf("%s without a destination: body %q, want empty", path, res.Body)
		}
	}

	// 設定済み・正常: 上部には出さず、下段に1行で出す。
	saveFolderDestination(t, c, filepath.Join(t.TempDir(), "out"))
	body = c.get("/admin").Body
	expectContains(t, body, bottom, `id="dashboard-status"`)
	expectNotContains(t, body, top)
	if indexOf(t, body, bottom) < indexOf(t, body, `id="popular-articles"`) {
		t.Error("compact publish status is not below the popular articles")
	}
	compact := c.get("/admin/publish/panel?compact=1").Body
	expectContains(t, compact, `data-publish-compact`, `id="publish-dashboard-reflect"`, `class="btn btn-primary btn-sm"`,
		"data-publish-status", `href="/admin/publish"`)
	expectNotContains(t, compact, "サイトを公開するには")

	// 最後の反映が失敗: 上部に戻し、compact でも普通の大きさで失敗を知らせる。
	st := env.server.publishState()
	st.mu.Lock()
	st.last = &publishOutcome{OK: false, FinishedText: "11/18 10:00", Message: "送れませんでした"}
	st.mu.Unlock()
	body = c.get("/admin").Body
	expectContains(t, body, top)
	expectNotContains(t, body, bottom)
	failed := c.get("/admin/publish/panel?compact=1").Body
	expectContains(t, failed, "できませんでした")
	expectNotContains(t, failed, "data-publish-compact")

	// 予約時刻を過ぎた記事の確認がまだなら上部。尋ねたあとは下段に戻る。
	st.mu.Lock()
	st.last = &publishOutcome{OK: true, FinishedText: "11/18 10:05", Message: "反映しました"}
	st.addOverdue("overdue-1")
	st.overdueAsked = false
	st.mu.Unlock()
	expectContains(t, c.get("/admin").Body, top)
	expectContains(t, c.get("/admin/publish/panel").Body, "予約時刻を過ぎた記事が 1 件あります。")
	body = c.get("/admin").Body
	expectContains(t, body, bottom)
	expectNotContains(t, body, top)

	// 編集者には出さない。
	expectNotContains(t, newPagesEnv(t, Config{}).loggedIn(chief).get("/admin").Body, "data-publish-panel", `id="dashboard-status"`)
}

// 自動バックアップ（デスクトップモード）: 保存先が未設定・最後のバックアップが失敗したときだけファーストビュー上部に
// 出し、正常に動いているときは下段に1行（compact）で出す。
func TestDashboardBackupStatusPlacement(t *testing.T) {
	env, _ := newDesktopEnv(t)
	c := env.completeSetup()
	const top, bottom = `data-desktop-part="/admin/backup/status"`, `data-desktop-part="/admin/backup/status?compact=1"`

	body := c.get("/admin").Body
	expectContains(t, body, top, `src="/static/js/desktop_care.js?v=`)
	expectNotContains(t, body, bottom)
	if indexOf(t, body, top) > indexOf(t, body, `id="stat-today-views"`) {
		t.Error("backup guide is not above the page view summary")
	}
	expectContains(t, c.get(backupStatusPath+"?compact=1").Body, "バックアップ先がまだ決まっていません")

	folder := filepath.Join(t.TempDir(), "OneDrive", backup.FolderName)
	expectRedirect(t, c.postForm(autoBackupPath, url.Values{"folder": {folder}}), backupPagePath+"?flash=backup_auto_saved")
	body = c.get("/admin").Body
	expectContains(t, body, bottom, `src="/static/js/desktop_care.js?v=`)
	expectNotContains(t, body, top)
	if indexOf(t, body, bottom) < indexOf(t, body, `id="popular-articles"`) {
		t.Error("compact backup status is not below the popular articles")
	}
	compact := c.get(backupStatusPath + "?compact=1").Body
	expectContains(t, compact, "data-backup-compact", `data-backup-result="ok"`, "最後のバックアップ: ", folder, `href="/admin/settings/backup#auto-backup"`)
	expectNotContains(t, compact, "うまくいきました")
	c.useEnglish()
	en := c.get(backupStatusPath + "?compact=1").Body
	expectContains(t, en, "Automatic backup", "Last backup:", "Backup settings")
	expectNotContains(t, en, "(succeeded)")

	raw, _ := json.Marshal(autoBackupStatus{At: time.Now().UTC(), OK: false, Message: "保存先に書き込めませんでした"})
	if err := upsertSetting(env.db, autoBackupLastSetting, string(raw)); err != nil {
		t.Fatal(err)
	}
	body = c.get("/admin").Body
	expectContains(t, body, top)
	expectNotContains(t, body, bottom)
	failed := c.get(backupStatusPath + "?compact=1").Body
	expectContains(t, failed, `data-backup-result="failed"`, "保存先に書き込めませんでした")
	expectNotContains(t, failed, "data-backup-compact")
}
