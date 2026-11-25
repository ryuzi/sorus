package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"sorus/internal/desktop"
	"sorus/internal/server"
)

// 画面を閉じたら終了する（desktop_app_window_spec.md D2〜D6、3-1 のアプリの側・3-2・3-3）。desktopApp は、すべての
// サイトの開いている画面（server.Server.OpenWindows）の合計を windowWatcher で見張り、すべて閉じたら、予約した記事の
// 反映を逃すときだけ尋ねてから終了する。

// desktopWindowPoll は画面の数を確かめる間隔。
const desktopWindowPoll = time.Second

// desktopWindowGrace は「すべて閉じた」とみなすまで、画面の数が 0 のまま続く時間（ページの移動や再読み込みで一瞬だけ
// 0 になるのを除くため。ページは離れるときに close を送り、次のページは読み込んですぐつなぎ直す）。ウィンドウを閉じて
// から終わるまでを短くするため 5 秒にする（admin_ux_phase5_spec.md 1.1。server の windowReadWait と合わせ、close が
// 届かないときでも 1 分以内に終わる）。desktopFirstWindowWait は、最初の画面がつながらないまま、URL をログで案内する
// までの時間。どちらもテストが短くする。
var (
	desktopWindowGrace     = 5 * time.Second
	desktopFirstWindowWait = 3 * time.Minute
)

// desktopTimeZone は確認画面に出す日時のタイムゾーン。管理画面と同じく日本時間（固定のオフセット）で出す。
var desktopTimeZone = time.FixedZone("JST", 9*60*60)

// watchPhase は windowWatcher の状態。
type watchPhase int

const (
	// phaseFirstWait は最初の画面がつながるのを待っている状態。この間は終了しない。
	phaseFirstWait watchPhase = iota
	// phaseOpen は画面を見張っている状態。数が 0 のまま猶予時間が過ぎたら、終了するか尋ねる。
	phaseOpen
	// phaseAsking は確認画面（AskQuitWithPending）の答えを待っている状態。
	phaseAsking
	// phaseFinishing は「予約をすべて反映してから終了する」の待ちの状態。画面は出さずに動き続ける。
	phaseFinishing
	// phaseQuitting は終了を頼んだあと。もう何もしない。
	phaseQuitting
)

// windowWatcher は、画面がすべて閉じたとき・「sorus を終了」が押されたときに、終了するか、尋ねるか、予約を反映して
// から終了するかを決める。サーバー・確認画面・時計・終了は関数で受け取り、テストで差し替えられるようにする。
// step を desktopWindowPoll ごとに呼ぶ（run）。step・answer・quitButton は別の goroutine から呼んでよい。
type windowWatcher struct {
	// now は今の時刻。windows はすべてのサイトの開いている画面の合計。busy はどれかのサイトで反映かバックアップが
	// 動いているか。pending はまだ反映していない予約（パターンA）の合計と、そのうち最も遅い公開日時。数えられない
	// サイトがあれば、数えられた分とエラーを返す。
	now     func() time.Time
	windows func() int
	busy    func() bool
	pending func() (int, time.Time, error)
	// reflectDue は、公開日時を迎えたのにまだ反映していない予約があるサイトで、手動の反映を始める（すでに反映中なら、
	// 終わってからもう1回行う）。始めたサイトがあれば true を返す。待ちの状態に入るときに1回呼ぶ。
	reflectDue func() bool
	// ask は確認画面を出し、「予約をすべて反映してから終了する」なら true を返す（答えるまで戻らない）。async は
	// ask を別の goroutine で動かす（テストは、答える時を自分で決める）。quit は終了を始めさせる（すぐに戻る）。
	ask   func(msg string) (finishFirst bool)
	async func(func())
	quit  func()
	logf  func(format string, args ...any)
	// grace は「すべて閉じた」とみなすまでの時間。firstWait は最初の画面を待って URL を案内するまでの時間。
	grace, firstWait time.Duration

	mu    sync.Mutex
	phase watchPhase
	// started は見張りを始めた時刻。adminURL は最初の画面がつながらないときに案内する URL。warned は案内したか。
	started  time.Time
	adminURL string
	warned   bool
	// closedSince は画面の数が 0 になった時刻（0 でなければゼロ値）。busyLogged は反映などの終わりを待つことを
	// ログに残したか。
	closedSince time.Time
	busyLogged  bool
	// dialog は確認画面の番号。確認画面を出すたびと、その答えを使わなくなったときに増やす。
	dialog int
	// waitWindows は待ちの状態に入ってからの画面の数の最小。これより増えたら、画面がつながったとみなす。
	// lastPending・lastErr は待ちの状態で最後にログに残した予約の数とエラー（変わったときだけ残す）。
	waitWindows int
	lastPending int
	lastErr     string
}

// newWindowWatcher は、本物の時計・確認画面・ログを使い、app のサイトを見張る windowWatcher を返す。
func newWindowWatcher(app *desktopApp) *windowWatcher {
	return &windowWatcher{
		now:        time.Now,
		windows:    app.openWindows,
		busy:       app.busy,
		pending:    app.pendingReflects,
		reflectDue: app.reflectDueNow,
		ask:        desktop.AskQuitWithPending,
		async:      func(f func()) { go f() },
		quit:       app.requestQuit,
		logf:       log.Printf,
		grace:      desktopWindowGrace,
		firstWait:  desktopFirstWindowWait,
	}
}

// start は見張りを始めた時刻と、最初の画面がつながらないときに案内する adminURL を記録する。
func (w *windowWatcher) start(adminURL string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started, w.adminURL = w.now(), adminURL
}

// run は ctx が終わるまで desktopWindowPoll ごとに step を呼ぶ。
func (w *windowWatcher) run(ctx context.Context) {
	t := time.NewTicker(desktopWindowPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.step()
		}
	}
}

// step は今の画面の数を見て、状態を1つ進める。
func (w *windowWatcher) step() {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	n := w.windows()
	switch w.phase {
	case phaseFirstWait:
		if n > 0 {
			w.phase = phaseOpen
			w.logf("Desktop mode: a window connected; sorus quits when every window has been closed for %s", w.grace)
			return
		}
		if !w.warned && now.Sub(w.started) >= w.firstWait {
			w.warned = true
			w.logf("Desktop mode: no window has connected for %s; sorus keeps running. Open %s in your browser.", w.firstWait, w.adminURL)
		}
	case phaseOpen:
		w.watchClosed(now, n)
	case phaseAsking:
		if n > 0 {
			w.reopen("a window connected while the quit question was shown; sorus keeps running")
		}
	case phaseFinishing:
		w.waitForReflects(n)
	}
}

// watchClosed は、画面の数 n が 0 のまま猶予時間が過ぎたら、反映などの終わりを待ってから、終了するか尋ねる。
// w.mu を持って呼ぶ。
func (w *windowWatcher) watchClosed(now time.Time, n int) {
	if n > 0 {
		w.closedSince, w.busyLogged = time.Time{}, false
		return
	}
	if w.closedSince.IsZero() {
		w.closedSince = now
		return
	}
	if now.Sub(w.closedSince) < w.grace {
		return
	}
	if w.busy() {
		if !w.busyLogged {
			w.busyLogged = true
			w.logf("Desktop mode: every window was closed; waiting for the running reflect or backup to finish")
		}
		return
	}
	p, last, err := w.pending()
	if err != nil {
		// 数えられなかったサイトの予約は、次に起動したときの「予約時刻を過ぎた記事」の確認で知らせる。
		w.logf("Desktop mode: could not count the scheduled articles: %v", err)
	}
	if p == 0 {
		w.stop("every window was closed")
		return
	}
	w.phase = phaseAsking
	w.dialog++
	id, msg := w.dialog, pendingQuitMessage(p, last)
	w.logf("Desktop mode: every window was closed with %d scheduled article(s) not yet reflected; asking whether to quit", p)
	w.async(func() { w.answer(id, w.ask(msg)) })
}

// answer は確認画面 id の答えを受け取る。その間に画面がつながった（使わなくなった）答えは捨てる。
func (w *windowWatcher) answer(id int, finishFirst bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.phase != phaseAsking || id != w.dialog {
		w.logf("Desktop mode: the answer to the quit question was ignored (a window connected or sorus was asked to quit meanwhile)")
		return
	}
	if w.windows() > 0 {
		w.reopen("a window connected while the quit question was shown; the answer was ignored and sorus keeps running")
		return
	}
	if !finishFirst {
		w.stop("quit now was chosen")
		return
	}
	w.finishFirst("finish first was chosen", 0)
}

// quitButton は管理画面の「sorus を終了」（server.Config.OnQuit）。finishFirst が false なら今すぐ終了し、true なら
// 確認画面を出さずに待ちの状態に入る。
func (w *windowWatcher) quitButton(finishFirst bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.phase == phaseQuitting {
		return
	}
	if !finishFirst {
		w.stop("quit was requested from the admin page")
		return
	}
	if w.phase == phaseFinishing {
		return
	}
	w.finishFirst("quit after the scheduled reflects was requested from the admin page", w.windows())
}

// finishFirst は待ちの状態に入る。n は今の画面の数（「sorus を終了」を押した画面は、まだつながっていることがある）。
// 出ている確認画面の答えは使わない。公開日時を迎えたのにまだ反映していない予約（起動する前に時刻を過ぎた記事や、
// 反映が失敗した記事）は、予約投稿による自動の反映を待たず、ここで反映を始める（reflectDue）。w.mu を持って呼ぶ。
func (w *windowWatcher) finishFirst(reason string, n int) {
	w.phase = phaseFinishing
	w.dialog++
	w.waitWindows, w.lastPending, w.lastErr = n, -1, ""
	w.logf("Desktop mode: %s; sorus keeps running without a window and quits after every scheduled article is reflected", reason)
	if w.reflectDue != nil && w.reflectDue() {
		w.logf("Desktop mode: started reflecting the scheduled articles whose publish time has passed")
	}
}

// waitForReflects は待ちの状態の1回分。画面がつながったら待ちをやめ、予約が残らず反映や
// バックアップも動いていなければ終了する。反映が失敗した予約は残るため、待ち続ける。w.mu を持って呼ぶ。
func (w *windowWatcher) waitForReflects(n int) {
	if n > w.waitWindows {
		w.reopen("a window connected; sorus no longer quits by itself after the scheduled reflects")
		return
	}
	w.waitWindows = n
	if w.busy() {
		return
	}
	p, _, err := w.pending()
	if err != nil {
		if msg := err.Error(); msg != w.lastErr {
			w.lastErr = msg
			w.logf("Desktop mode: could not count the scheduled articles (sorus keeps waiting): %v", err)
		}
		return
	}
	w.lastErr = ""
	if p == 0 {
		w.stop("every scheduled article was reflected")
		return
	}
	if p != w.lastPending {
		w.lastPending = p
		w.logf("Desktop mode: %d scheduled article(s) left to reflect; sorus quits after the last one", p)
	}
}

// reopen は画面がつながったので、終了の判断や待ちをやめて、また画面を見張る。出ている確認画面の答えは使わない。
// w.mu を持って呼ぶ。
func (w *windowWatcher) reopen(reason string) {
	w.phase = phaseOpen
	w.dialog++
	w.closedSince, w.busyLogged = time.Time{}, false
	w.logf("Desktop mode: %s", reason)
}

// stop は終了を始めさせる。w.mu を持って呼ぶ。
func (w *windowWatcher) stop(reason string) {
	w.phase = phaseQuitting
	w.logf("Desktop mode: %s; quitting", reason)
	w.quit()
}

// pendingQuitMessage は、終了すると反映を逃す予約が n 件（最後は last）あるときの確認画面の文面。
func pendingQuitMessage(n int, last time.Time) string {
	when := ""
	if !last.IsZero() {
		when = "（最後は " + last.In(desktopTimeZone).Format("1月2日 15:04") + "）"
	}
	return fmt.Sprintf("予約した記事が %d 件あります%s。sorus を終了すると、その時刻にサイトへ反映されません。", n, when)
}

// servers は動いているサイトのサーバーを返す。withDeleting が false なら削除の途中のサイトを除く。
func (a *desktopApp) servers(withDeleting bool) []*server.Server {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*server.Server
	for _, s := range a.sites {
		if s.srv != nil && (withDeleting || !s.deleting) {
			out = append(out, s.srv)
		}
	}
	return out
}

// openWindows はすべてのサイトの開いている画面の合計を返す（削除の途中のサイトの画面も数える）。
func (a *desktopApp) openWindows() int {
	n := 0
	for _, srv := range a.servers(true) {
		n += srv.OpenWindows()
	}
	return n
}

// busy はどれかのサイトで反映かバックアップが動いているかを返す。
func (a *desktopApp) busy() bool {
	for _, srv := range a.servers(false) {
		if srv.Busy() {
			return true
		}
	}
	return false
}

// pendingReflects は、すべてのサイトのまだ反映していない予約の合計と、そのうち最も遅い公開日時を返す。数えられない
// サイトがあれば、数えられた分と、それらのエラーをまとめて返す。
func (a *desktopApp) pendingReflects() (int, time.Time, error) {
	total, latest := 0, time.Time{}
	var errs []error
	for _, srv := range a.servers(false) {
		n, last, err := srv.PendingScheduledReflects()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		total += n
		if last.After(latest) {
			latest = last
		}
	}
	return total, latest, errors.Join(errs...)
}

// reflectDueNow は、どのサイトでも、公開日時を迎えたのにまだ反映していない予約があれば手動の反映を始め
// （server.Server.ReflectDueNow）、始めたサイトがあれば true を返す。
func (a *desktopApp) reflectDueNow() bool {
	started := false
	for _, srv := range a.servers(false) {
		if srv.ReflectDueNow() {
			started = true
		}
	}
	return started
}

// openDesktopWindow は url を sorus のウィンドウ（アプリモード。無ければ既定のブラウザ）で開く。管理画面の外部リンク・
// 新しいタブで開くリンクは、static/js/admin.js が server.DesktopOpenExternalPath を通して OS の既定のブラウザで開く。
// 開けなければ、利用者が自分で開けるよう URL をログに残す。
func openDesktopWindow(url string) {
	if err := desktop.OpenWindow(url); err != nil {
		log.Printf("Could not open the browser (%v). Open %s in your browser.", err, url)
	}
}
