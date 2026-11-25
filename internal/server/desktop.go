package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/desktop"
	"sorus/internal/i18n"
	"sorus/internal/scheduler"
)

// DesktopQuitPath は デスクトップモードの「sorus を終了」の送信先。サーバーモードでは登録せず 404 になる。
// GET は、まだサイトに反映していない予約があるときに、終わらせ方を選ぶ確認の画面（desktop_app_window_spec.md D5）。
const DesktopQuitPath = "/admin/desktop/quit"

// DesktopOpenExternalPath は デスクトップモードで、管理画面の外部リンク・新しいタブで開くリンクを OS の既定のブラウザで
// 開かせる送信先（static/js/admin.js が POST する）。sorus の画面はアプリ風の独立したウィンドウ（desktop.OpenWindow）で
// 開くため、そのまま新しいタブで開くと、そのブラウザ（Edge など）のタブになる。サーバーモードでは登録せず 404 になる。
const DesktopOpenExternalPath = "/admin/desktop/open-external"

// desktopOpenBrowser は URL を OS の既定のブラウザで開く。テストで差し替える。
var desktopOpenBrowser = desktop.OpenBrowser

// PreviewTokenPath は デスクトップモードで、下書きを OS の既定のブラウザ（Chrome など）で公開デザインのまま確かめる
// 使い捨てのプレビュー（GET PreviewTokenPath + "/<token>"）の置き場所。既定のブラウザにはアプリのウィンドウの
// ログインの Cookie が無いため、ログインの代わりに、管理画面で発行した1回だけ・短い時間だけ使えるトークンで開く
// （handleDesktopPreviewOpen・handleTokenThemePreview）。サーバーモードでは登録せず 404 になる。
const PreviewTokenPath = "/_preview"

// previewTokenTTL はプレビューのトークンを発行してから使えなくなるまでの時間。既定のブラウザが起動して開くまでの間だけ使える。
const previewTokenTTL = 60 * time.Second

// previewTokenEntry はプレビューのトークン1つで開ける記事と、使えなくなる時刻。
type previewTokenEntry struct {
	articleID string
	expiresAt time.Time
}

// previewTokenStore は使い捨てのプレビューのトークンのメモリ上の置き場所。トークンは1回使うか期限が切れると消し、
// 発行・使用のたびに期限の切れたものを掃除する。sorus を終了すると発行済みのトークンはすべて使えなくなる。零値で使う。
type previewTokenStore struct {
	mu     sync.Mutex
	tokens map[string]previewTokenEntry
}

// issue は記事 articleID を開くトークン（128 ビットの乱数の 32 文字の16進数）を発行し、now から previewTokenTTL の
// 間だけ使えるようにして返す。
func (st *previewTokenStore) issue(articleID string, now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("server: generate preview token: %w", err)
	}
	token := hex.EncodeToString(b[:])
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked(now)
	if st.tokens == nil {
		st.tokens = make(map[string]previewTokenEntry)
	}
	st.tokens[token] = previewTokenEntry{articleID: articleID, expiresAt: now.Add(previewTokenTTL)}
	return token, nil
}

// consume はトークンを使い、開ける記事の ID を返す。トークンは見つかれば期限にかかわらず消すため、2回目からは
// 使えない。知らないトークン・期限の切れたトークンなら ok は false。
func (st *previewTokenStore) consume(token string, now time.Time) (articleID string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, found := st.tokens[token]
	delete(st.tokens, token)
	st.sweepLocked(now)
	if !found || !now.Before(e.expiresAt) {
		return "", false
	}
	return e.articleID, true
}

// revoke はまだ使っていないトークンを消す（既定のブラウザを開けなかったとき）。
func (st *previewTokenStore) revoke(token string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.tokens, token)
}

// sweepLocked は期限の切れたトークンを消す。st.mu を持って呼ぶ。
func (st *previewTokenStore) sweepLocked(now time.Time) {
	for token, e := range st.tokens {
		if !now.Before(e.expiresAt) {
			delete(st.tokens, token)
		}
	}
}

// publishSettingReflectStarted は、最後に成功した「サイトに反映」を始めた時刻（RFC 3339、UTC）を保存する settings のキー。
// その時刻より後に公開日時を迎えた予約投稿は、まだ公開先へ送っていない（PendingScheduledReflects）。
const publishSettingReflectStarted = "publish_last_success_started_at"

// desktopQuitPage は終了したことを伝える画面。sorus が止まった後に表示されたままになるため、管理画面の
// レイアウト（ナビの各画面へのリンクや外部の CSS）は使わず、それだけで表示できる HTML にする。
const desktopQuitPage = `<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>sorus を終了しました</title>
<style>
body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
  background: #fafafa; color: #262626; font-family: system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 28rem; margin: 1rem; padding: 2rem; border: 1px solid #e5e5e5; border-radius: 1rem; background: #fff; text-align: center; }
h1 { margin: 0 0 .75rem; font-size: 1.25rem; }
p { margin: .5rem 0 0; line-height: 1.7; color: #525252; font-size: .9375rem; }
</style>
</head>
<body>
<main>
<h1>終了しました。このタブは閉じてかまいません。</h1>
<p>もう一度使うときは、sorus を起動してください。書いた記事はこのパソコンに保存されています。</p>
</main>
</body>
</html>
`

// desktopQuitStyle は、確認の画面と「反映してから終了する」を選んだあとの画面に共通の見た目。desktopQuitPage と同じく、
// それだけで表示できるようにする。
const desktopQuitStyle = `body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
  background: #fafafa; color: #262626; font-family: system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 30rem; margin: 1rem; padding: 2rem; border: 1px solid #e5e5e5; border-radius: 1rem; background: #fff; text-align: center; }
h1 { margin: 0 0 .75rem; font-size: 1.25rem; }
p { margin: .5rem 0 0; line-height: 1.7; color: #525252; font-size: .9375rem; }
form { margin: 1.5rem 0 0; display: flex; flex-direction: column; gap: .5rem; }
button, a.cancel { display: block; padding: .625rem 1rem; border-radius: .5rem; font-size: .9375rem; font-family: inherit; cursor: pointer; text-decoration: none; }
button { border: 1px solid #262626; background: #262626; color: #fff; }
button.secondary { background: #fff; color: #262626; border-color: #d4d4d4; }
a.cancel { color: #525252; }
`

// desktopQuitConfirmTmpl は、まだサイトに反映していない予約があるときの「sorus を終了」の確認の画面。
// 終了を選んだら sorus が止まるため、管理画面のレイアウトは使わない。ここで迷っている間も画面として数える（Window）。
var desktopQuitConfirmTmpl = template.Must(template.New("quit-confirm").Parse(`<!doctype html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>{{.Style}}</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{with .Body}}<p id="quit-pending">{{.}}</p>{{end}}
<form method="post" action="` + DesktopQuitPath + `">
<input type="hidden" name="_csrf" value="{{.CSRFToken}}">
{{if .Pending}}<button type="submit" name="finish" value="1" id="quit-finish-first">{{.FinishFirst}}</button>
<button type="submit" name="finish" value="0" id="quit-now" class="secondary">{{.QuitNow}}</button>
{{else}}<button type="submit" name="finish" value="0" id="quit-now">{{.QuitNow}}</button>
{{end}}<a href="/admin" class="cancel" id="quit-cancel" onclick="if (history.length > 1) { history.back(); return false; }">{{.Cancel}}</a>
</form>
</main>
{{.Window}}
</body>
</html>
`))

// desktopQuitLaterTmpl は「予約をすべて反映してから終了する」を選んだあとの画面。sorus は画面を出さずに動き続け、
// 最後の予約を反映したら自分で終了する。この画面は数えない（数えると、閉じるまで終了の待ちを始められない）。
var desktopQuitLaterTmpl = template.Must(template.New("quit-later").Parse(`<!doctype html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>{{.Style}}</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<p>{{.Body}}</p>
</main>
</body>
</html>
`))

// registerDesktopRoutes はデスクトップモードだけの操作を、ログイン済み・CSRF 保護済みの /admin グループに登録する。
func (s *Server) registerDesktopRoutes(authed *echo.Group) {
	authed.GET("/desktop/quit", s.handleDesktopQuitConfirm)
	authed.POST("/desktop/quit", s.handleDesktopQuit)
	authed.POST("/desktop/open-external", s.handleDesktopOpenExternal)
}

// handleDesktopOpenExternal は JSON（{"url": "..."}）かフォーム（url）で受け取った URL を OS の既定のブラウザで開き、
// 204 を返す。URL は externalURL で確かめ、開けない URL は 400、ブラウザを起動できなければ 500 を返す。
func (s *Server) handleDesktopOpenExternal(c echo.Context) error {
	raw := ""
	if strings.HasPrefix(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationJSON) {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON")
		}
		raw = body.URL
	} else {
		raw = c.FormValue("url")
	}
	target, err := externalURL(raw, c.Scheme(), c.Request().Host)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := desktopOpenBrowser(target); err != nil {
		c.Logger().Errorf("desktop: open external link: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "could not open the browser")
	}
	return c.NoContent(http.StatusNoContent)
}

// handleDesktopPreviewOpen は記事 :id の下書きプレビュー（公開デザインでの確認）を OS の既定のブラウザで開き、204 を返す
// （POST /admin/articles/:id/preview-open。デスクトップモードだけ。ログイン・CSRF・ライター以上の役割を確かめた後に呼ばれる）。
// 使い捨てのトークンを発行し、いま届いた要求と同じこの PC のオリジン（http://127.0.0.1:<port>）の
// PreviewTokenPath/<token> を開かせる。ゴミ箱・存在しない記事は 404、ブラウザを起動できなければ 500 を返す。
func (s *Server) handleDesktopPreviewOpen(c echo.Context) error {
	a, err := s.articles.GetByID(c.Param("id"))
	if errors.Is(err, article.ErrNotFound) {
		return echo.ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.Status == article.StatusDeleted {
		return echo.ErrNotFound
	}
	origin, err := desktopPreviewOrigin(c.Scheme(), c.Request().Host)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	token, err := s.previewTokens.issue(a.ID, clockNow())
	if err != nil {
		return err
	}
	if err := desktopOpenBrowser(origin + PreviewTokenPath + "/" + token); err != nil {
		// 開けなかったトークンは使わせない（エラーの文に URL が含まれていても、ログからは開けない）。
		s.previewTokens.revoke(token)
		c.Logger().Errorf("desktop: open draft preview of article %s: %v", a.ID, err)
		return echo.NewHTTPError(http.StatusInternalServerError, "could not open the browser")
	}
	return c.NoContent(http.StatusNoContent)
}

// desktopPreviewOrigin は既定のブラウザで開くプレビューのオリジン（http://127.0.0.1:<port>）を、いま届いた要求の
// スキームとホストから作る。デスクトップモードの待ち受けは HostGuard でこの PC 自身のホストしか受け付けないが、
// 念のため、この PC 自身（127.0.0.1・localhost・::1）ではないホスト・余計な部分のあるホストならエラーにする。
func desktopPreviewOrigin(scheme, host string) (string, error) {
	if scheme != "https" {
		scheme = "http"
	}
	u, err := url.Parse(scheme + "://" + host)
	if err != nil || u.Host == "" || u.Host != host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		!isLoopbackHost(u.Hostname()) {
		return "", errors.New("the preview can only be opened on this PC")
	}
	return u.Scheme + "://" + u.Host, nil
}

// externalURL は既定のブラウザで開いてよい絶対 URL を返す。"/" で始まる（"//" ではない）パスは scheme://host の
// ページとして解決する。それ以外は http・https でホストのある絶対 URL だけを許し、javascript:・file:・data: などや、
// 空の URL はエラーにする。
func externalURL(raw, scheme, host string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, "/\\") {
		if scheme != "https" {
			scheme = "http"
		}
		raw = scheme + "://" + host + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("only http and https URLs can be opened")
	}
	return u.String(), nil
}

// handleDesktopQuitConfirm は「sorus を終了」の確認の画面。まだサイトに反映していない予約があれば、件数と最後の日時を
// 出し、「予約をすべて反映してから終了する」「今すぐ終了する」「やめる」を選ばせる。予約が無ければ（確認の画面を開いた
// 後に反映し終えたときなど）、「今すぐ終了する」と「やめる」だけを出す。
func (s *Server) handleDesktopQuitConfirm(c echo.Context) error {
	lang := string(language(c))
	n, last, err := s.PendingScheduledReflects()
	if err != nil {
		c.Logger().Warnf("desktop: count pending scheduled reflects: %v", err)
	}
	title := i18n.Translate(lang, "desktop_quit_confirm")
	if s.cfg.Sites != nil && len(s.layoutSites(lang)) > 0 {
		title = i18n.Translate(lang, "desktop_quit_all_confirm")
	}
	d := map[string]any{
		"Lang": lang, "Title": title, "Style": template.CSS(desktopQuitStyle), "CSRFToken": csrfToken(c),
		"Pending": n > 0, "FinishFirst": i18n.Translate(lang, "desktop_quit_finish_first"),
		"QuitNow": i18n.Translate(lang, "desktop_quit_now"), "Cancel": i18n.Translate(lang, "desktop_quit_cancel"),
		"Window": s.windowScriptFor(c),
	}
	if n > 0 {
		d["Body"] = i18n.Translate(lang, "desktop_quit_pending", n, formatQuitTime(lang, last))
	}
	var b strings.Builder
	if err := desktopQuitConfirmTmpl.Execute(&b, d); err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.HTML(http.StatusOK, b.String())
}

// formatQuitTime は確認の画面に出す最後の予約の日時（日本時間）。
func formatQuitTime(lang string, t time.Time) string {
	t = t.In(jst)
	if i18n.Normalize(lang) == i18n.EN {
		return t.Format("Jan 2 15:04")
	}
	return t.Format("1月2日 15:04")
}

// handleDesktopQuit は「sorus を終了」。Config.OnQuit で終了を始めさせ、終わり方を伝える画面を返す。
// finish=1（予約をすべて反映してから終了する）なら、sorus は予約を反映し終えてから終了するため、その旨とウィンドウを
// 閉じてよいことを伝える。それ以外は今すぐ終了し、終了したことを伝える画面を返す。
// OnQuit の呼び出し側が行う Shutdown は処理中の要求を待つため、この画面を返し終えてからプロセスが終わる。
func (s *Server) handleDesktopQuit(c echo.Context) error {
	finishFirst := c.FormValue("finish") == "1"
	if s.cfg.OnQuit != nil {
		s.cfg.OnQuit(finishFirst)
	}
	h := c.Response().Header()
	h.Set("Cache-Control", "no-store")
	// 終了後に同じ接続で次の要求を送らせない。
	h.Set("Connection", "close")
	if !finishFirst {
		return c.HTML(http.StatusOK, desktopQuitPage)
	}
	lang := string(language(c))
	var b strings.Builder
	err := desktopQuitLaterTmpl.Execute(&b, map[string]any{
		"Lang": lang, "Style": template.CSS(desktopQuitStyle),
		"Title": i18n.Translate(lang, "desktop_quit_later_title"), "Body": i18n.Translate(lang, "desktop_quit_later_body"),
	})
	if err != nil {
		return err
	}
	return c.HTML(http.StatusOK, b.String())
}

// Busy は「サイトに反映」かバックアップ（作成・復元。反映の書き出しも含む）が動いているかを返す。アプリは、これが
// false になるまで待ってから終了する。
func (s *Server) Busy() bool {
	st := s.publishState()
	st.mu.Lock()
	running := st.running
	st.mu.Unlock()
	if running {
		return true
	}
	// バックアップの作成は backupMu を読み取りで、復元は書き込みで持つ。どちらかが持っていれば書き込みは取れない。
	if !s.backupMu.TryLock() {
		return true
	}
	s.backupMu.Unlock()
	return false
}

// PendingScheduledReflects は、公開先（パターンAの反映先）へまだ送っていない予約投稿の数 n と、そのうち最も遅い
// 公開日時 last を返す（desktop_app_window_spec.md D3、schedule_unify_spec.md D5）。公開先が決まっていなければ 0 と
// ゼロ時刻を返す。数え方は unreflectedScheduled。
func (s *Server) PendingScheduledReflects() (n int, last time.Time, err error) {
	if !s.loadPublishConfig().configured() {
		return 0, time.Time{}, nil
	}
	list, err := s.unreflectedScheduled(context.Background())
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("server: count pending scheduled reflects: %w", err)
	}
	for _, r := range list {
		if r.PublishedAt.After(last) {
			last = r.PublishedAt
		}
	}
	return len(list), last, nil
}

// scheduledReflect は、まだ公開先へ送っていない予約投稿の1件。
type scheduledReflect struct {
	ID          string    `db:"id"`
	PublishedAt time.Time `db:"published_at"`
}

// unreflectedScheduled は、公開先が決まっているかにかかわらず、まだ公開先へ送っていない予約投稿を返す。状態が公開の
// 記事のうち、次のどちらかに当たる記事。
//   - 予約中（公開日時が未来）の記事。
//   - 公開日時を迎えたが、その時刻が最後に成功した反映を始めた時刻より後の、予約だった記事。反映が失敗したか、
//     まだ動いていなければ数えたままにする。予約だった記事は、予約だった記事の集合（scheduler.Scheduled）に今と同じ
//     公開日時で入っている記事とする。公開日時を迎えたあとに保存し直しても、起動し直しても集合に残る。すぐに公開した
//     記事は集合に入らず、予約による自動の反映を待たないため含めない。
//
// 最後に成功した反映を始めた時刻と集合は settings に保存するため、起動し直しても同じ答えになる。
func (s *Server) unreflectedScheduled(ctx context.Context) ([]scheduledReflect, error) {
	set, err := scheduler.Scheduled(ctx, s.db)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	now, since := clockNow().UTC(), s.lastReflectStarted()
	var rows []scheduledReflect
	err = s.db.SelectContext(ctx, &rows, `SELECT a.id, a.published_at FROM articles a
		WHERE a.status = ? AND a.published_at IS NOT NULL AND (
			julianday(a.published_at) > julianday(?) OR
			(julianday(a.published_at) > julianday(?) AND a.id IN (SELECT value FROM json_each(?))))`,
		article.StatusPublished, now, since.UTC(), string(list))
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if at, ok := set[r.ID]; r.PublishedAt.After(now) || (ok && at.Equal(r.PublishedAt)) {
			out = append(out, r)
		}
	}
	return out, nil
}

// lastReflectStarted は最後に成功した反映を始めた時刻を返す。保存する前の版で反映していたときは、最後の反映が成功して
// いればその終わった時刻を使う。一度も成功していなければゼロ時刻（公開日時を迎えた予約だった記事をすべて数える）。
func (s *Server) lastReflectStarted() time.Time {
	if raw := s.getSetting(publishSettingReflectStarted, ""); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t
		}
	}
	if out := s.loadLastOutcome(); out != nil && out.OK {
		return out.FinishedAt
	}
	return time.Time{}
}

// saveReflectSucceeded は、started に始めた反映が成功したことを保存する（PendingScheduledReflects）。
func (s *Server) saveReflectSucceeded(started time.Time) error {
	return upsertSetting(s.db, publishSettingReflectStarted, started.UTC().Format(time.RFC3339Nano))
}

// quitPending は管理画面の「sorus を終了」に、確認の画面（GET DesktopQuitPath）を使うかを決める予約の数。
func (s *Server) quitPending(c echo.Context) int {
	n, _, err := s.PendingScheduledReflects()
	if err != nil {
		c.Logger().Warnf("desktop: count pending scheduled reflects: %v", err)
	}
	return n
}
