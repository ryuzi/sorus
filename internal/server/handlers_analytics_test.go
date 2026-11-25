package server

import (
	"context"
	"html"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"sorus/internal/analytics"
)

// flushAnalytics はメモリ上の PV 集計を DB に書き出す（サーバーが作った集計サービスを閉じる。Shutdown で再度閉じても安全）。
func (e *testEnv) flushAnalytics() {
	e.t.Helper()
	if err := e.server.analytics.Close(); err != nil {
		e.t.Fatalf("flush analytics: %v", err)
	}
}

// getAs は User-Agent と追加のヘッダーを付けて GET する。
func (c *client) getAs(path, userAgent string, header map[string]string) *response {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.env.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", userAgent)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return c.do(req)
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36"

func TestPublicArticleRecordsPageViews(t *testing.T) {
	env := newPublicEnv(t)
	reader := env.newClient()
	expectStatus(t, reader.getAs("/articles/keion-live", browserUA, nil), http.StatusOK)
	expectStatus(t, reader.getAs("/articles/keion-live", browserUA, nil), http.StatusOK)
	expectStatus(t, reader.getAs("/articles/library-room", browserUA, map[string]string{"X-Forwarded-For": "198.51.100.7"}), http.StatusOK)

	// Bot・先読み・ログイン中の編集部員の閲覧は数えない。
	expectStatus(t, reader.getAs("/articles/keion-live", "Mozilla/5.0 (compatible; Googlebot/2.1)", nil), http.StatusOK)
	expectStatus(t, reader.getAs("/articles/keion-live", browserUA, map[string]string{"Sec-Purpose": "prefetch;prerender"}), http.StatusOK)
	staff := env.loggedIn(studentA)
	expectStatus(t, staff.getAs("/articles/keion-live", browserUA, nil), http.StatusOK)
	// 存在しない記事・下書きは数えない。
	expectStatus(t, reader.getAs("/articles/secret-draft", browserUA, nil), http.StatusNotFound)

	env.flushAnalytics()
	views := func(slug string) (v, u int64) {
		t.Helper()
		var row struct {
			Views  int64 `db:"views"`
			Unique int64 `db:"unique_visitors"`
		}
		err := env.db.Get(&row, `SELECT COALESCE(SUM(views), 0) AS views, COALESCE(SUM(unique_visitors), 0) AS unique_visitors
			FROM article_views_daily WHERE article_id = ?`, env.articleIDBySlug(slug))
		if err != nil {
			t.Fatal(err)
		}
		return row.Views, row.Unique
	}
	if v, u := views("keion-live"); v != 2 || u != 1 {
		t.Errorf("keion-live views = %d / unique %d, want 2 / 1", v, u)
	}
	if v, u := views("library-room"); v != 1 || u != 1 {
		t.Errorf("library-room views = %d / unique %d, want 1 / 1", v, u)
	}
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM site_views_daily`); n != 3 {
		t.Errorf("site views = %d, want 3", n)
	}
	// IP アドレス・User-Agent は保存しない（日次の集計だけ）。
	for _, table := range []string{"article_views_daily", "site_views_daily"} {
		var cols []struct {
			Name string `db:"name"`
		}
		if err := env.db.Select(&cols, `SELECT name FROM pragma_table_info(?)`, table); err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			if strings.Contains(c.Name, "ip") || strings.Contains(c.Name, "agent") || strings.Contains(c.Name, "hash") {
				t.Errorf("%s stores %s", table, c.Name)
			}
		}
	}
}

func TestCountStaffViews(t *testing.T) {
	env := newTestEnvWith(t, Config{CountStaffViews: true}, nil)
	c := env.loggedIn(chief)
	id := c.createArticle("編集部の記事", "本文")
	if _, err := env.db.Exec(`UPDATE articles SET status = 'published', published_at = ?, slug = 'staff' WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute), id); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, c.getAs("/articles/staff", browserUA, nil), http.StatusOK)
	env.flushAnalytics()
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily WHERE article_id = ?`, id); n != 1 {
		t.Errorf("staff view counted %d times, want 1 with CountStaffViews", n)
	}
}

// seedViews は日次の PV 集計を直接入れる。daysAgo はサイトのタイムゾーン（JST）の今日から何日前か。
func seedViews(t *testing.T, env *testEnv, articleID string, daysAgo int, views, unique int64) {
	t.Helper()
	seedViewsOn(t, env, articleID, clockNow().In(env.server.timeLocation()).AddDate(0, 0, -daysAgo).Format(analyticsDate), views, unique)
}

// seedViewsOn は date（YYYY-MM-DD）の日次の PV 集計を直接入れる。
func seedViewsOn(t *testing.T, env *testEnv, articleID, date string, views, unique int64) {
	t.Helper()
	if articleID != "" {
		if _, err := env.db.Exec(`INSERT INTO article_views_daily (article_id, date, views, unique_visitors) VALUES (?, ?, ?, ?)
			ON CONFLICT(article_id, date) DO UPDATE SET views = views + excluded.views, unique_visitors = unique_visitors + excluded.unique_visitors`,
			articleID, date, views, unique); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.db.Exec(`INSERT INTO site_views_daily (date, views, unique_visitors) VALUES (?, ?, ?)
		ON CONFLICT(date) DO UPDATE SET views = views + excluded.views, unique_visitors = unique_visitors + excluded.unique_visitors`,
		date, views, unique); err != nil {
		t.Fatal(err)
	}
}

func TestDashboard(t *testing.T) {
	env := newPublicEnv(t)
	c := env.loggedIn(studentA)

	// データが無くても表示できる。
	res := c.get("/admin")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "本日のPV", "過去30日のPV推移", "週間人気記事 トップ5", "まだ閲覧データがありません",
		"この7日間に読まれた記事はまだありません", `href="/admin/settings"`, "/static/js/dashboard.js")

	keion, library, jp := env.articleIDBySlug("keion-live"), env.articleIDBySlug("library-room"), env.articleIDBySlug("学園祭レポート")
	seedViews(t, env, keion, 0, 1200, 800)
	seedViews(t, env, library, 0, 34, 30)
	seedViews(t, env, keion, 1, 300, 250)
	seedViews(t, env, jp, 3, 2500, 2000)
	seedViews(t, env, library, 10, 90, 80) // 週間ランキングの集計外
	seedViews(t, env, "", 40, 99999, 1)    // 推移グラフの期間外

	res = c.get("/admin/")
	expectStatus(t, res, http.StatusOK)
	tile := func(id string) string {
		t.Helper()
		i := strings.Index(res.Body, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("tile %s not found", id)
		}
		end := strings.Index(res.Body[i:], "</div>")
		return res.Body[i : i+end]
	}
	expectContains(t, tile("stat-today-views"), ">1,234<", "昨日 300")
	expectContains(t, tile("stat-today-visitors"), ">830<", "昨日 250")
	expectContains(t, tile("stat-period-views"), ">4,124<", "1日平均 137")
	expectContains(t, tile("stat-period-visitors"), ">3,160<")
	if strings.Contains(res.Body, "まだ閲覧データがありません") || strings.Contains(res.Body, "99,999") {
		t.Error("dashboard shows the empty state or out-of-range data")
	}

	// 推移グラフ: 30日分の棒（PV のある日だけ棒を描く）と、表で見る代替。最大値だけを直接ラベルにする。
	if n := strings.Count(res.Body, `class="chart-bar`); n != dashboardTrendDays {
		t.Errorf("chart has %d bars, want %d", n, dashboardTrendDays)
	}
	if n := strings.Count(res.Body, `class="chart-mark"`); n != 4 {
		t.Errorf("chart has %d drawn bars, want 4", n)
	}
	if n := strings.Count(res.Body, `class="chart-value"`); n != 1 {
		t.Errorf("chart has %d value labels, want 1", n)
	}
	expectContains(t, res.Body, `class="chart-bar is-today"`, `data-views="1,234"`, "表で見る", `role="img"`)

	// 週間人気記事: 過去7日の PV 順。公開ページ（公開サイトの URL を起点にした本番の URL）へのリンク付き。
	pop := res.Body[strings.Index(res.Body, `id="popular-articles"`):]
	iJP, iKeion, iLib := strings.Index(pop, "日本語スラッグの記事"), strings.Index(pop, "軽音楽部の新歓ライブ"), strings.Index(pop, "図書館の新しい自習室")
	if iJP < 0 || iKeion < 0 || iLib < 0 || !(iJP < iKeion && iKeion < iLib) {
		t.Errorf("popular ranking order is wrong (jp=%d keion=%d library=%d)", iJP, iKeion, iLib)
	}
	expectContains(t, pop, "2,500</span> PV", "1,500</span> PV", `href="`+testBaseURL+`/articles/keion-live"`, `href="/admin/articles/`+keion+`/edit"`, `aria-label="1位"`)
	adminRes := env.loggedIn(admin).get("/admin")
	expectStatus(t, adminRes, http.StatusOK)
	// 管理者も記事を編集できるため、編集画面へのリンクにする。
	adminPop := adminRes.Body[strings.Index(adminRes.Body, `id="popular-articles"`):]
	expectContains(t, adminPop, `href="/admin/articles/`+keion+`/edit"`)

	// 削除した記事は、集計が残っていてもランキングに出さない（集計は外部キーで消える）。
	if _, err := env.db.Exec(`DELETE FROM articles WHERE id = ?`, jp); err != nil {
		t.Fatal(err)
	}
	res = c.get("/admin")
	if strings.Contains(res.Body, "日本語スラッグの記事") {
		t.Error("deleted article is still ranked")
	}

	// 記事の状況（状態ごとの件数）から一覧へ移動できる。
	expectContains(t, res.Body, `href="/admin/articles?status=draft"`, `href="/admin/articles?status=published"`)
}

// ダッシュボードの「本日」・推移の日付は UTC ではなくサイトのタイムゾーン（JST）の暦日で区切る。
// 2026-10-20 01:30 JST は UTC ではまだ 10/19 なので、UTC で区切ると「本日」が 1 日ずれる。
func TestDashboardUsesLocalDates(t *testing.T) {
	env := newPublicEnv(t)
	setClock(t, time.Date(2026, 10, 19, 16, 30, 0, 0, time.UTC))
	seedViewsOn(t, env, "", "2026-10-20", 50, 40)   // JST の今日
	seedViewsOn(t, env, "", "2026-10-19", 7, 6)     // JST の昨日（UTC では今日）
	seedViewsOn(t, env, "", "2026-09-21", 3, 3)     // 過去30日の初日
	seedViewsOn(t, env, "", "2026-09-20", 9000, 1)  // 期間外
	seedViewsOn(t, env, "", "2026-10-21", 70000, 1) // 未来の日付（期間外）

	res := env.loggedIn(studentA).get("/admin")
	expectStatus(t, res, http.StatusOK)
	tile := func(id string) string {
		t.Helper()
		i := strings.Index(res.Body, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("tile %s not found", id)
		}
		return res.Body[i : i+strings.Index(res.Body[i:], "</div>")]
	}
	expectContains(t, tile("stat-today-views"), ">50<", "昨日 7")
	expectContains(t, tile("stat-period-views"), ">60<")
	expectContains(t, res.Body, `class="chart-bar is-today" tabindex="0" data-date="2026-10-20"`, `data-date="2026-09-21"`,
		"今日（10/20）", "日付（JST）")
	expectNotContains(t, res.Body, `data-date="2026-09-20"`, `data-date="2026-10-21"`, "9,000", "70,000", "日付（UTC）")
}

// 外部の詳細なアクセス解析へのリンク: 設定してあればダッシュボードに出し、未設定なら管理者に設定への入口を出す。
func TestDashboardExternalAnalyticsLink(t *testing.T) {
	env := newPublicEnv(t)
	adm, writer := env.loggedIn(admin), env.loggedIn(studentA)

	res := adm.get("/admin")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `id="analytics-external-setup" href="/admin/settings/general#analytics_dashboard_url"`, "詳細な解析ツールのURLを設定する")
	expectNotContains(t, res.Body, `id="pv-trend-note"`, "analytics_local_note")
	expectNotContains(t, res.Body, `id="analytics-external-link"`)
	// 設定できない編集部員には入口を出さない（開いても権限が無い）。
	res = writer.get("/admin")
	expectNotContains(t, res.Body, `id="pv-trend-note"`, `analytics_local_note`, `id="analytics-external-setup"`, `id="analytics-external-link"`)

	const dash = "https://dash.cloudflare.com/abc123/web-analytics/overview?siteTag~in=xyz"
	form := generalSettingsForm()
	form.Set("analytics_dashboard_url", "  "+dash+"  ")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	if got := env.setting("analytics_dashboard_url"); got != dash {
		t.Fatalf("analytics_dashboard_url = %q, want %q", got, dash)
	}
	link := `id="analytics-external-link" href="https://dash.cloudflare.com/abc123/web-analytics/overview?siteTag~in=xyz" target="_blank" rel="noopener noreferrer"`
	for _, c := range []*client{adm, writer} {
		res = c.get("/admin")
		expectContains(t, res.Body, link, "詳細なアクセス解析を開く ↗")
		expectNotContains(t, res.Body, `id="analytics-external-setup"`)
	}
	expectContains(t, adm.get("/admin/settings/general").Body, `name="analytics_dashboard_url"`, `value="`+html.EscapeString(dash)+`"`)

	// 空欄にすると未設定に戻る。
	form.Set("analytics_dashboard_url", "")
	expectRedirect(t, adm.postForm("/admin/settings/general", form), "/admin/settings/general?flash=settings_saved")
	expectNotContains(t, adm.get("/admin").Body, `id="analytics-external-link"`)
}

// 外部解析の URL は http(s) の URL だけを受け付ける。不正な値は何も保存せず、入力を保ったままフォームを出し直す。
func TestGeneralSettingsRejectsInvalidAnalyticsDashboardURL(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	for _, bad := range []string{"javascript:alert(1)", "dash.cloudflare.com/abc", "ftp://example.com/x", "https://"} {
		form := generalSettingsForm()
		form.Set("site_name", "保存されない名前")
		form.Set("analytics_dashboard_url", bad)
		res := adm.postForm("/admin/settings/general", form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, "https:// で始まる URL を入力してください", `value="保存されない名前"`, `value="`+html.EscapeString(bad)+`"`)
		if n := env.countRows(`SELECT COUNT(*) FROM settings WHERE key IN ('analytics_dashboard_url', 'site_name')`); n != 0 {
			t.Errorf("%q: %d settings saved, want 0", bad, n)
		}
	}
	for raw, want := range map[string]string{"": "", "https://analytics.google.com/analytics/web/#/p123/reports": "https://analytics.google.com/analytics/web/#/p123/reports",
		"http://localhost:3000/stats": "http://localhost:3000/stats"} {
		if got, err := normalizeAnalyticsDashboardURL(raw); err != nil || got != want {
			t.Errorf("normalizeAnalyticsDashboardURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestArticleListShowsTotalViews(t *testing.T) {
	env := newPublicEnv(t)
	keion := env.articleIDBySlug("keion-live")
	seedViews(t, env, keion, 0, 1000, 10)
	seedViews(t, env, keion, 20, 234, 5)

	c := env.loggedIn(admin)
	res := c.get("/admin/articles?status=published")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "総PV", `data-social-open data-article-id="`+keion+`"`, `id="social-modal"`, "/static/js/social.js")
	row := res.Body[strings.Index(res.Body, `data-article-id="`+keion+`"`):]
	row = row[:strings.Index(row, "</tr>")]
	expectContains(t, row, `<td class="article-views whitespace-nowrap px-4 py-3 text-right tabular-nums text-neutral-600">1,234</td>`)

	// 下書きの一覧には告知文ボタンを出さない（公開 URL が無い）。
	res = c.get("/admin/articles?status=draft")
	expectContains(t, res.Body, `class="article-views`)
	rows := strings.SplitN(res.Body, `<tbody id="article-rows"`, 2)
	if len(rows) != 2 {
		t.Fatal("tbody #article-rows not found")
	}
	tbody := strings.SplitN(rows[1], `</tbody>`, 2)[0]
	if strings.Contains(tbody, `data-social-open`) {
		t.Error("draft rows show the social post button")
	}
}

// 推移グラフの日付は now のタイムゾーンの暦日で数える。
func TestBuildTrendChartUsesLocation(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 30, 0, 0, time.UTC).In(jst) // JST 10/20 01:30
	ch := buildTrendChart(map[string]analytics.Stats{"2026-10-20": {Views: 5}}, now, 7)
	if last := ch.Bars[len(ch.Bars)-1]; last.Date != "2026-10-20" || !last.IsToday || last.Views != 5 || ch.Bars[0].Date != "2026-10-14" {
		t.Errorf("bars = first %s, last %+v", ch.Bars[0].Date, last)
	}
}

func TestBuildTrendChart(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	byDate := map[string]analytics.Stats{
		"2026-10-20": {Date: "2026-10-20", Views: 15, UniqueVisitors: 9},
		"2026-10-18": {Date: "2026-10-18", Views: 30, UniqueVisitors: 20},
		"2026-09-21": {Date: "2026-09-21", Views: 3, UniqueVisitors: 3},
		"2026-09-20": {Date: "2026-09-20", Views: 999, UniqueVisitors: 1}, // 期間外
	}
	ch := buildTrendChart(byDate, now, 30)
	if len(ch.Bars) != 30 || ch.Bars[0].Date != "2026-09-21" || ch.Bars[29].Date != "2026-10-20" || !ch.Bars[29].IsToday {
		t.Fatalf("bars = %d, first %s, last %s", len(ch.Bars), ch.Bars[0].Date, ch.Bars[29].Date)
	}
	if ch.FirstLabel != "9/21" || ch.LastLabel != "10/20" || ch.Empty {
		t.Errorf("labels %q-%q empty=%v", ch.FirstLabel, ch.LastLabel, ch.Empty)
	}
	if ch.Grid[len(ch.Grid)-1].Label != "30" || ch.Grid[0].Label != "15" {
		t.Errorf("grid = %+v, want 15 / 30", ch.Grid)
	}
	maxBar := ch.Bars[27]
	if !maxBar.IsMaxValue || maxBar.Path == "" || ch.Bars[28].Path != "" {
		t.Errorf("max bar = %+v, zero bar path = %q", maxBar, ch.Bars[28].Path)
	}
	// 棒の高さは目盛りの最大値に対する比率。下端はベースラインに揃う。
	if want := ch.BaselineY - ch.PlotHeight; maxBar.TooltipY != want {
		t.Errorf("max bar top = %v, want %v", maxBar.TooltipY, want)
	}
	if !strings.HasPrefix(maxBar.Path, "M") || !strings.Contains(maxBar.Path, "Q") {
		t.Errorf("bar path = %q", maxBar.Path)
	}
	if empty := buildTrendChart(nil, now, 30); !empty.Empty || empty.Grid[len(empty.Grid)-1].Label != "10" {
		t.Errorf("empty chart = %+v", empty.Grid)
	}
}

func TestNiceCeilAndFormatCount(t *testing.T) {
	for v, want := range map[int64]int64{0: 10, 7: 10, 11: 20, 30: 30, 35: 40, 45: 50, 55: 60, 70: 80, 99: 100, 101: 200, 210: 300, 1234: 2000, 4500: 5000} {
		if got := niceCeil(v); got != want {
			t.Errorf("niceCeil(%d) = %d, want %d", v, got, want)
		}
	}
	for v, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := formatCount(v); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", v, got, want)
		}
	}
}

func TestIsPrefetch(t *testing.T) {
	for _, h := range [][2]string{{"Sec-Purpose", "prefetch"}, {"Purpose", "prefetch"}, {"X-Moz", "prefetch"}, {"Sec-Purpose", "prefetch;prerender"}} {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(h[0], h[1])
		if !isPrefetch(r) {
			t.Errorf("%s: %s is not detected as prefetch", h[0], h[1])
		}
	}
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	if isPrefetch(r) {
		t.Error("plain request detected as prefetch")
	}
}

// 静的な書き出し（「サイトに反映」）で全ページを描画しても、読者の閲覧として数えない。
func TestStaticExportDoesNotRecordPageViews(t *testing.T) {
	env := newPublicEnv(t)
	dir, _, err := env.server.ExportToTemp(context.Background(), ExportOptions{})
	if err != nil {
		t.Fatalf("ExportToTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	env.flushAnalytics()
	today := clockNow().In(env.server.timeLocation()).Format(analyticsDate)
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM site_views_daily WHERE date = ?`, today); n != 0 {
		t.Errorf("site views today after an export = %d, want 0", n)
	}
	if n := env.countRows(`SELECT COALESCE(SUM(views), 0) FROM article_views_daily`); n != 0 {
		t.Errorf("article views after an export = %d, want 0", n)
	}
}
