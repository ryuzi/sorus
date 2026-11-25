package server

import (
	"bytes"
	"context"
	"html/template"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/desktop"
	"sorus/internal/i18n"
	"sorus/internal/version"
)

const (
	// updateCheckedSetting・updateLatestSetting・updateDismissedSetting は、新しい版を最後に調べた日時（RFC 3339）・
	// そのとき見つかった最新の版・利用者が「閉じる」を押した版を保存する settings のキー。
	updateCheckedSetting   = "update_checked_at"
	updateLatestSetting    = "update_latest_version"
	updateDismissedSetting = "update_dismissed_version"

	// updateBannerPath・updateDismissPath はダッシュボードに読み込む更新のお知らせと、その「閉じる」の送信先。
	updateBannerPath  = "/admin/update/banner"
	updateDismissPath = "/admin/update/dismiss"

	// updateFirstCheckDelay は起動してから最初に新しい版を調べるまでの待ち時間。
	updateFirstCheckDelay = 30 * time.Second
	// updatePollInterval は前回の確認から desktop.UpdateCheckInterval たったかを確かめる間隔。
	updatePollInterval = time.Hour
)

// runningVersion は動いている sorus の版を返す（テストで差し替える）。
var runningVersion = func() string { return version.Version }

// registerUpdateRoutes は更新のお知らせを、ログイン済み・CSRF 保護済みの /admin グループに登録する（デスクトップモードだけ）。
func (s *Server) registerUpdateRoutes(authed *echo.Group) {
	authed.GET("/update/banner", s.handleUpdateBanner)
	authed.POST("/update/dismiss", s.handleUpdateDismiss)
}

// runUpdateChecks は、1日に1回まで、版 current より新しいリリースがあるかを調べて保存する。開発中の版（"dev"）と、
// 環境変数 SORUS_NO_UPDATE_CHECK=1 のときは調べずにすぐ戻る。失敗はログに残すだけにする。ctx が終わるまで戻らない。
func (s *Server) runUpdateChecks(ctx context.Context, current string) {
	if !desktop.UpdateCheckEnabled(current, os.Getenv) {
		return
	}
	url := desktop.UpdateURL(os.Getenv)
	wait := updateFirstCheckDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := s.checkUpdateIfDue(ctx, nil, url, current, time.Now()); err != nil {
			s.echo.Logger.Warnf("update: check for a new version: %v", err)
		}
		wait = updatePollInterval
	}
}

// startUpdateChecks は新しい版の確認を始める。Config.UpdateCheck があれば、確認はアプリ全体で1つの
// UpdateCheck.Run に任せ、このサーバーをその結果の保存先に加える（Shutdown・StopBackground で外す。ctx の終わりでは
// 外さない: 止めたときの ctx の後始末が、ResumeBackground で加わり直したあとに動くことがあるため）。
// 無ければ、このサーバーだけで確かめる（runUpdateChecks。ctx が終わると止まる）。
func (s *Server) startUpdateChecks(ctx context.Context) {
	u := s.cfg.UpdateCheck
	if u == nil {
		go s.runUpdateChecks(ctx, runningVersion())
		return
	}
	u.add(s)
}

// leaveUpdateCheck は、このサーバーを共有の UpdateCheck の保存先から外す。
func (s *Server) leaveUpdateCheck() {
	if u := s.cfg.UpdateCheck; u != nil {
		u.remove(s)
	}
}

// UpdateCheck は新しい版の確認を、デスクトップモードのアプリ全体で1つにまとめる。アプリは NewUpdateCheck で1つ作って
// すべてのサイトの Config.UpdateCheck に渡し、Run を1回だけ動かす。サイトがいくつあっても問い合わせは1日に1回まで
// で、見つかった版はどのサイトの画面にもお知らせとして出る（「閉じる」はサイトごと）。
//
// 確かめた日時と見つかった版は、今までどおり各サイトの DB に保存する（起動し直しても1日に1回までを守り、1つ目の
// サイトに前の版が保存した日時もそのまま使う）。サイトが加わると、それまでの結果をそのサイトにも保存する。
type UpdateCheck struct {
	mu sync.Mutex
	// servers は結果を保存するサーバー（動いているデスクトップモードのサイト）。
	servers map[*Server]struct{}
	// checked・latest は、加わったサイトと確認で分かったうち最も新しい、確かめた日時と見つかった版。
	checked time.Time
	latest  string
}

// NewUpdateCheck は、サイトがまだ1つも加わっていない UpdateCheck を返す。
func NewUpdateCheck() *UpdateCheck {
	return &UpdateCheck{servers: map[*Server]struct{}{}}
}

// add は s を結果の保存先に加える。s の DB に保存された結果がこれまでより新しければそれを使い、それまでに加わった
// サイトにも保存する。古ければ、これまでの結果を s に保存する。
func (u *UpdateCheck) add(s *Server) {
	u.mu.Lock()
	defer u.mu.Unlock()
	checked, _ := time.Parse(time.RFC3339, s.getSetting(updateCheckedSetting, ""))
	switch {
	case checked.After(u.checked):
		u.checked, u.latest = checked, s.getSetting(updateLatestSetting, "")
		u.saveAll()
	case u.checked.After(checked):
		if err := u.save(s); err != nil {
			s.echo.Logger.Warnf("update: save the last check: %v", err)
		}
	}
	u.servers[s] = struct{}{}
}

// remove は s を結果の保存先から外す。
func (u *UpdateCheck) remove(s *Server) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.servers, s)
}

// save は今の結果を s の DB に保存する。u.mu を持って呼ぶ。
func (u *UpdateCheck) save(s *Server) error {
	if err := s.putSetting(updateCheckedSetting, u.checked.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if u.latest == "" {
		return nil
	}
	return s.putSetting(updateLatestSetting, u.latest)
}

// saveAll は今の結果を、加わっているすべてのサーバーに保存する。u.mu を持って呼ぶ。
func (u *UpdateCheck) saveAll() {
	for s := range u.servers {
		if err := u.save(s); err != nil {
			s.echo.Logger.Warnf("update: save the last check: %v", err)
		}
	}
}

// Run は Server.runUpdateChecks と同じく、1日に1回まで、動いている sorus より新しいリリースがあるかを調べ、結果を
// 加わっているすべてのサイトに保存する。開発中の版と SORUS_NO_UPDATE_CHECK=1 のときはすぐ戻る。失敗はログに残す
// だけにする。ctx が終わるまで戻らない。アプリ全体で1回だけ呼ぶ。
func (u *UpdateCheck) Run(ctx context.Context) {
	current := runningVersion()
	if !desktop.UpdateCheckEnabled(current, os.Getenv) {
		return
	}
	url := desktop.UpdateURL(os.Getenv)
	wait := updateFirstCheckDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := u.checkIfDue(ctx, nil, url, current, time.Now()); err != nil {
			log.Printf("update: check for a new version: %v", err)
		}
		wait = updatePollInterval
	}
}

// checkIfDue は Server.checkUpdateIfDue と同じ判断で、前回の確認から desktop.UpdateCheckInterval たっていれば url から
// 最新のリリースを調べ、加わっているすべてのサイトに保存する。サイトが1つも加わっていなければ何もしない。
func (u *UpdateCheck) checkIfDue(ctx context.Context, client *http.Client, url, current string, now time.Time) error {
	u.mu.Lock()
	if len(u.servers) == 0 || !u.checked.IsZero() && now.Sub(u.checked) < desktop.UpdateCheckInterval && !u.checked.After(now) {
		u.mu.Unlock()
		return nil
	}
	// 失敗しても確認した日時は保存し、次の確認は翌日にする（問い合わせを繰り返さない）。
	u.checked = now
	u.saveAll()
	u.mu.Unlock()

	rel, err := desktop.LatestRelease(ctx, client, url, current)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.latest = rel.Version
	u.saveAll()
	return nil
}

// checkUpdateIfDue は、前回の確認から desktop.UpdateCheckInterval たっていれば url から最新のリリースを調べ、
// その版を保存する。失敗しても確認した日時は保存し、次の確認は翌日にする（問い合わせを繰り返さない）。
func (s *Server) checkUpdateIfDue(ctx context.Context, client *http.Client, url, current string, now time.Time) error {
	if last, err := time.Parse(time.RFC3339, s.getSetting(updateCheckedSetting, "")); err == nil &&
		now.Sub(last) < desktop.UpdateCheckInterval && !last.After(now) {
		return nil
	}
	if err := s.putSetting(updateCheckedSetting, now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	rel, err := desktop.LatestRelease(ctx, client, url, current)
	if err != nil {
		return err
	}
	return s.putSetting(updateLatestSetting, rel.Version)
}

// updateNotice は、版 current より新しい版が見つかっていて、利用者がまだそのお知らせを閉じていなければ、その版を返す。
func (s *Server) updateNotice(current string) (string, bool) {
	latest := s.getSetting(updateLatestSetting, "")
	if !desktop.Newer(latest, current) || s.getSetting(updateDismissedSetting, "") == latest {
		return "", false
	}
	return latest, true
}

// updateBannerData は更新のお知らせに渡すデータ。文言は表示言語の辞書（internal/i18n）から引いて渡す。
type updateBannerData struct {
	// Command はこの OS で最新版を入れ直す1行。
	Command string
	Dismiss string
	// Title・Body・Close・Copy・Copied は表示言語の文言（Body には貼る先の PowerShell・ターミナルを入れてある）。
	Title, Body, Close, Copy, Copied string
}

// newUpdateBannerData は、版 v の更新のお知らせを lang の文言で作る。goos はこの PC の OS（runtime.GOOS）。
func newUpdateBannerData(lang, v, goos string) updateBannerData {
	terminal := i18n.Translate(lang, "update_terminal")
	if goos == "windows" {
		terminal = "PowerShell"
	}
	return updateBannerData{
		Command: desktop.InstallCommand(goos),
		Dismiss: updateDismissPath,
		Title:   i18n.Translate(lang, "update_banner_title", v),
		Body:    i18n.Translate(lang, "update_banner_body", terminal),
		Close:   i18n.Translate(lang, "close"),
		Copy:    i18n.Translate(lang, "copy"),
		Copied:  i18n.Translate(lang, "copied"),
	}
}

// updateBannerTemplate はダッシュボードに出す更新のお知らせ。
var updateBannerTemplate = template.Must(template.New("update_banner").Parse(`<section class="rounded-xl border border-sky-300 bg-sky-50 p-4 text-sky-950" aria-labelledby="update-banner-title">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <h2 id="update-banner-title" class="text-sm font-semibold">{{.Title}}</h2>
    <button type="button" data-dismiss-update="{{.Dismiss}}" class="rounded-md px-2 py-1 text-sm text-sky-900 hover:bg-sky-100">{{.Close}}</button>
  </div>
  <p class="mt-1 text-sm">{{.Body}}</p>
  <div class="mt-3 flex flex-wrap items-center gap-2">
    <code id="update-command" class="min-w-0 flex-1 break-all rounded-lg border border-sky-200 bg-white px-3 py-2 font-mono text-xs text-neutral-900">{{.Command}}</code>
    <button type="button" data-copy-target="update-command" data-copied-label="{{.Copied}}" class="shrink-0 rounded-lg bg-sky-900 px-4 py-2 text-sm font-semibold text-white hover:bg-sky-800">{{.Copy}}</button>
  </div>
</section>`))

// handleUpdateBanner はダッシュボードに読み込む更新のお知らせを返す。新しい版が無ければ空を返す。
func (s *Server) handleUpdateBanner(c echo.Context) error {
	var buf bytes.Buffer
	if v, ok := s.updateNotice(runningVersion()); ok {
		d := newUpdateBannerData(string(language(c)), v, runtime.GOOS)
		if err := updateBannerTemplate.Execute(&buf, d); err != nil {
			return err
		}
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.HTMLBlob(http.StatusOK, buf.Bytes())
}

// handleUpdateDismiss は更新のお知らせの「閉じる」。今の最新版のお知らせを出さないようにする（さらに新しい版が出たらまた出す）。
func (s *Server) handleUpdateDismiss(c echo.Context) error {
	if latest := s.getSetting(updateLatestSetting, ""); latest != "" {
		if err := s.putSetting(updateDismissedSetting, latest); err != nil {
			return err
		}
	}
	return c.NoContent(http.StatusNoContent)
}
