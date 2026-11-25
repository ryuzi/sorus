package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/analytics"
	"sorus/internal/article"
	"sorus/internal/auth"
)

const (
	// dashboardTrendDays はダッシュボードの PV 推移グラフの日数。
	dashboardTrendDays = 30
	// dashboardPopularDays・dashboardPopularLimit は人気記事ランキングの集計日数と件数。
	dashboardPopularDays  = 7
	dashboardPopularLimit = 5

	// 推移グラフ（SVG）の座標系。左の余白に目盛りの数値を置く。
	chartWidth   = 720
	chartHeight  = 220
	chartLeft    = 44
	chartRight   = 8
	chartTop     = 12
	chartBottom  = 28
	chartBarGap  = 2
	chartBarEdge = 4
)

// analyticsDate は日次集計の日付の形式。日付はサイトのタイムゾーン（timeLocation）の暦日。
const analyticsDate = "2006-01-02"

// timeLocation は PV の日次集計の日付を区切るタイムゾーン。Config.Location を省略したら管理画面の表示と同じ JST。
func (s *Server) timeLocation() *time.Location {
	if s.cfg.Location != nil {
		return s.cfg.Location
	}
	return jst
}

// dashboardData は dashboard.html に渡すデータ。
type dashboardData struct {
	Today, Yesterday analytics.Stats
	// PeriodViews・PeriodUnique は直近 dashboardTrendDays 日の PV と、日ごとのユニーク訪問者の合計（延べ人数）。
	PeriodViews, PeriodUnique int64
	Trend                     trendChart
	Popular                   []popularRow
	PopularDays, TrendDays    int
	// 記事の状態ごとの件数。
	Drafts, Reviews, Scheduled, Published int
	CanPublish                            bool
	// TimeZone は日付の区切りに使うタイムゾーンの略称（例: JST）。表で見るの見出しに出す。
	TimeZone string
	// AnalyticsURL は外部の詳細なアクセス解析（Google Analytics・Cloudflare Web Analytics など）の画面の URL。
	// CanEditSettings は未設定のときに「サイト基本設定」への入口を出すか（管理者だけが設定できる）。
	AnalyticsURL    string
	CanEditSettings bool
	// RemoteSync は公開サーバーの校閲待ち記事の一覧を表示するか（公開サーバーの設定とロールから決める）。
	RemoteSync remoteSyncView
	// PublishAttention・BackupAttention は「サイトに反映」・自動バックアップに案内や警告が要るか（未設定・失敗）。
	// true ならファーストビュー上部に、false なら下段に1行の状態として出す（管理者だけ。バックアップはデスクトップ
	// モードだけ）。
	PublishAttention, BackupAttention bool
}

// trendChart は PV 推移の棒グラフ（サーバーで座標を計算した SVG）。
type trendChart struct {
	Width, Height int
	Bars          []trendBar
	Grid          []gridLine
	// BaselineY は棒の下端（0 の位置）。PlotLeft・PlotRight・PlotTop は描画領域の左右端と上端。
	BaselineY, PlotLeft, PlotRight, PlotTop float64
	// PlotHeight は描画領域の高さ（ホバー判定の列の高さ）。
	PlotHeight float64
	// FirstLabel・LastLabel は横軸の両端の日付。
	FirstLabel, LastLabel string
	// Empty は期間中の PV が 0 か。
	Empty bool
}

// trendBar は1日分の棒。Path は上端だけを角丸にした棒、HitX・HitWidth はホバー判定用の列。
type trendBar struct {
	Date, Label         string
	Views, Unique       int64
	Path                string
	HitX, HitWidth      float64
	TooltipX, TooltipY  float64
	IsToday, IsMaxValue bool
}

// gridLine は横の目盛り線。
type gridLine struct {
	Y     float64
	Label string
}

// popularRow は人気記事ランキングの1行。
type popularRow struct {
	Rank          int
	ID            string         `db:"id"`
	Title         string         `db:"title"`
	Slug          string         `db:"slug"`
	Status        article.Status `db:"status"`
	Kind          article.Kind   `db:"kind"`
	PublishedAt   *time.Time     `db:"published_at"`
	Views, Unique int64
	// PublicURL は読者に見えている記事・ページの公開 URL のパス（トップに出しているページは /）。
	PublicURL string
}

// handleDashboard は管理画面トップ（/admin）に、本日の PV・過去30日の推移・週間人気記事を表示する。
// 日付の区切りはサイトのタイムゾーン（timeLocation）の暦日。
func (s *Server) handleDashboard(c echo.Context) error {
	now := clockNow().In(s.timeLocation())
	user := currentUser(c)
	data := dashboardData{PopularDays: dashboardPopularDays, TrendDays: dashboardTrendDays, CanPublish: canPublish(user),
		RemoteSync: s.remoteSyncViewFor(user), TimeZone: now.Format("MST"),
		AnalyticsURL: s.analyticsDashboardURL(), CanEditSettings: user.HasRole(auth.RoleAdmin)}
	if user.HasRole(auth.RoleAdmin) {
		data.PublishAttention = s.publishNeedsAttention()
		if s.cfg.Desktop {
			data.BackupAttention = s.autoBackupView(false).needsAttention()
		}
	}

	stats, err := s.analytics.GetSiteStats(dashboardTrendDays)
	if err != nil {
		return err
	}
	byDate := make(map[string]analytics.Stats, len(stats))
	for _, st := range stats {
		byDate[st.Date] = st
		data.PeriodViews += st.Views
		data.PeriodUnique += st.UniqueVisitors
	}
	data.Today = byDate[now.Format(analyticsDate)]
	data.Yesterday = byDate[now.AddDate(0, 0, -1).Format(analyticsDate)]
	data.Trend = buildTrendChart(byDate, now, dashboardTrendDays)

	if data.Popular, err = s.popularArticles(c.Request().Context(), dashboardPopularLimit, dashboardPopularDays); err != nil {
		return err
	}
	// ダッシュボードの件数は記事・ページを合わせて数える（校閲待ちのページも見落とさない）。
	counts, err := s.countByStatus("", clockNow().UTC(), "", "")
	if err != nil {
		return err
	}
	data.Drafts, data.Reviews = counts[article.StatusDraft], counts[article.StatusReview]
	data.Scheduled, data.Published = counts[article.StatusScheduled], counts[article.StatusPublished]
	return s.render(c, http.StatusOK, "dashboard", pageTitle(c, "dashboard"), data)
}

// popularArticles は直近 days 日の PV 上位 limit 件の記事を、タイトルと状態を付けて返す。
// 集計後に削除（ゴミ箱を空に）された記事は外す。トップに出しているページの公開ページは / にする。
// 公開ページの URL は publicSiteOrigin があれば本番の URL、無ければこの PC の相対パス。
func (s *Server) popularArticles(ctx context.Context, limit, days int) ([]popularRow, error) {
	ranking, err := s.analytics.GetPopularArticles(limit, days)
	if err != nil || len(ranking) == 0 {
		return []popularRow{}, err
	}
	ids := make([]string, len(ranking))
	for i, r := range ranking {
		ids[i] = r.ArticleID
	}
	query, args, err := sqlx.In(`SELECT id, title, slug, status, kind, published_at FROM articles WHERE id IN (?)`, ids)
	if err != nil {
		return nil, err
	}
	var rows []popularRow
	if err := s.db.Select(&rows, query, args...); err != nil {
		return nil, err
	}
	front, err := s.frontPageSlug(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]popularRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]popularRow, 0, len(ranking))
	now := clockNow()
	origin := s.publicSiteOrigin()
	for _, r := range ranking {
		row, ok := byID[r.ArticleID]
		if !ok {
			continue
		}
		row.Rank, row.Views, row.Unique = len(out)+1, r.Views, r.UniqueVisitors
		if article.IsVisible(&article.Article{Status: row.Status, PublishedAt: row.PublishedAt}, now) {
			row.PublicURL = origin + publicURLPath(row.Kind, row.Slug, front)
		}
		out = append(out, row)
	}
	return out, nil
}

// publicSiteOrigin は公開ページの URL の起点（末尾の / なし）を返す。公開サイトの URL（base_url）がループバック
// 以外ならそれ、無ければ公開サーバーの URL（スキーム+ホスト）。どちらも無ければ空（この PC の相対パスで開く）。
func (s *Server) publicSiteOrigin() string {
	if base := strings.TrimRight(s.getSetting("base_url", s.getBaseURL()), "/"); base != "" {
		if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" && !isLoopbackHost(u.Hostname()) {
			return base
		}
	}
	if remote := s.loadRemoteConfig().URL; remote != "" {
		if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" && !isLoopbackHost(u.Hostname()) {
			return u.Scheme + "://" + u.Host
		}
	}
	return ""
}

// buildTrendChart は直近 days 日（今日を含む・欠けた日は 0）の PV の棒グラフを組み立てる。
// 日付は now のタイムゾーンの暦日で数える。
func buildTrendChart(byDate map[string]analytics.Stats, now time.Time, days int) trendChart {
	ch := trendChart{
		Width: chartWidth, Height: chartHeight,
		PlotLeft: chartLeft, PlotRight: chartWidth - chartRight, PlotTop: chartTop,
		BaselineY: chartHeight - chartBottom, PlotHeight: chartHeight - chartBottom - chartTop,
	}
	var max int64
	start := now.AddDate(0, 0, -(days - 1))
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i)
		st := byDate[d.Format(analyticsDate)]
		max = maxInt64(max, st.Views)
		ch.Bars = append(ch.Bars, trendBar{
			Date: d.Format(analyticsDate), Label: d.Format("1/2"),
			Views: st.Views, Unique: st.UniqueVisitors, IsToday: i == days-1,
		})
	}
	ch.Empty = max == 0
	top := niceCeil(max)
	plotHeight := ch.PlotHeight
	for _, v := range []int64{top / 2, top} {
		ch.Grid = append(ch.Grid, gridLine{Y: ch.BaselineY - plotHeight*float64(v)/float64(top), Label: formatCount(v)})
	}
	slot := (ch.PlotRight - ch.PlotLeft) / float64(days)
	for i := range ch.Bars {
		b := &ch.Bars[i]
		x := ch.PlotLeft + slot*float64(i)
		b.HitX, b.HitWidth = x, slot
		b.TooltipX = x + slot/2
		h := plotHeight * float64(b.Views) / float64(top)
		b.TooltipY = ch.BaselineY - h
		b.IsMaxValue = b.Views == max && max > 0
		if b.Views > 0 {
			b.Path = barPath(x+chartBarGap/2, ch.BaselineY-h, slot-chartBarGap, h)
		}
	}
	ch.FirstLabel, ch.LastLabel = ch.Bars[0].Label, ch.Bars[len(ch.Bars)-1].Label
	return ch
}

// barPath は下端が直角・上端だけ角丸の棒の SVG パス。低い棒は角丸の半径を高さに合わせて縮める。
func barPath(x, y, w, h float64) string {
	r := min(chartBarEdge, w/2, h)
	bottom := y + h
	return fmt.Sprintf("M%.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fZ",
		x, bottom, y+r, x, y, x+r, y, x+w-r, x+w, y, x+w, y+r, bottom)
}

// niceCeil は v 以上で、目盛りに使いやすい値（2・3・4・5・6・8・10 × 10^n。半分の目盛りも切りのよい数になる）を返す。
// 最小は 10。最大値との差を小さくし、グラフの上部に空きを作りすぎない。
func niceCeil(v int64) int64 {
	if v <= 10 {
		return 10
	}
	base := int64(1)
	for base*10 < v {
		base *= 10
	}
	for _, m := range []int64{2, 3, 4, 5, 6, 8, 10} {
		if base*m >= v {
			return base * m
		}
	}
	return base * 10
}

// formatCount は 1,234 のように3桁区切りで数を返す。
func formatCount(n int64) string {
	if n < 0 {
		return "-" + formatCount(-n)
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// recordView は公開記事の閲覧を PV として記録する（非同期・Cookie 不使用）。
// 先読み（prefetch）・HEAD・静的エクスポート（「サイトに反映」）の内部要求・ログイン中の編集部員の閲覧
// （Config.CountStaffViews が false のとき）は数えない。
// Bot の除外と、IP・User-Agent の日替わりソルト付きハッシュ化は analytics.Service が行う。
func (s *Server) recordView(c echo.Context, articleID string) {
	r := c.Request()
	if s.analytics == nil || r.Method != http.MethodGet || isPrefetch(r) || isStaticExport(c) {
		return
	}
	if !s.cfg.CountStaffViews && s.isStaff(c) {
		return
	}
	s.analytics.RecordView(articleID, c.RealIP(), r.UserAgent())
}

// isPrefetch はブラウザの先読み・事前レンダリングの要求かを返す。読者が実際に開いた閲覧ではない。
func isPrefetch(r *http.Request) bool {
	for _, h := range []string{"Sec-Purpose", "Purpose", "X-Purpose", "X-Moz"} {
		if v := strings.ToLower(r.Header.Get(h)); strings.Contains(v, "prefetch") || strings.Contains(v, "preview") {
			return true
		}
	}
	return false
}

// isStaff はログイン中の管理画面ユーザー（有効なセッション Cookie を持つ）からの要求かを返す。
// 一般の読者は Cookie を持たないため、DB を引くのはセッション Cookie がある要求だけ。
func (s *Server) isStaff(c echo.Context) bool {
	cookie, err := c.Cookie(s.cookies.Session)
	if err != nil || cookie.Value == "" {
		return false
	}
	_, err = auth.ValidateSession(s.db, cookie.Value)
	return err == nil
}
