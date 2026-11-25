package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/database"
	"sorus/internal/desktop"
	"sorus/internal/scheduler"
	"sorus/internal/server"

	"golang.org/x/net/websocket"
)

// fakeWatch は windowWatcher の差し替え（時計・画面の数・反映・予約・確認画面・終了）。確認画面は、テストが answer を
// 呼ぶまで答えない。
type fakeWatch struct {
	t        *testing.T
	w        *windowWatcher
	clock    time.Time
	open     int
	isBusy   bool
	count    int
	last     time.Time
	countErr error
	// asked は出した確認画面の文面。dialogs は答えを待っている確認画面（呼ぶと、その時の reply で答える）。
	asked   []string
	dialogs []func()
	reply   bool
	quits   int
	logs    []string
}

func newFakeWatch(t *testing.T) *fakeWatch {
	f := &fakeWatch{t: t, clock: time.Date(2026, 11, 1, 10, 0, 0, 0, desktopTimeZone)}
	f.w = &windowWatcher{
		now:     func() time.Time { return f.clock },
		windows: func() int { return f.open },
		busy:    func() bool { return f.isBusy },
		pending: func() (int, time.Time, error) { return f.count, f.last, f.countErr },
		ask: func(msg string) bool {
			f.asked = append(f.asked, msg)
			return f.reply
		},
		async:     func(fn func()) { f.dialogs = append(f.dialogs, fn) },
		quit:      func() { f.quits++ },
		logf:      func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
		grace:     10 * time.Second,
		firstWait: 3 * time.Minute,
	}
	f.w.start("http://127.0.0.1:8737/admin")
	return f
}

// tick は時計を d 進めてから step を1回呼ぶ。
func (f *fakeWatch) tick(d time.Duration) {
	f.clock = f.clock.Add(d)
	f.w.step()
}

// ticks は1秒ずつ n 回 tick する。
func (f *fakeWatch) ticks(n int) {
	for range n {
		f.tick(time.Second)
	}
}

// answer は答えを待っている最初の確認画面に reply で答える。
func (f *fakeWatch) answer(finishFirst bool) {
	f.t.Helper()
	if len(f.dialogs) == 0 {
		f.t.Fatal("no quit question is shown")
	}
	fn := f.dialogs[0]
	f.dialogs = f.dialogs[1:]
	f.reply = finishFirst
	fn()
}

// connectThenClose は最初の画面をつないでから閉じ、猶予時間を過ぎるまで進める（すべて閉じた判断の直後の状態にする）。
func (f *fakeWatch) connectThenClose() {
	f.t.Helper()
	f.open = 1
	f.tick(time.Second)
	f.open = 0
	f.ticks(11)
}

func (f *fakeWatch) wantQuits(n int) {
	f.t.Helper()
	if f.quits != n {
		f.t.Fatalf("quit called %d times, want %d (log: %q)", f.quits, n, f.logs)
	}
}

func (f *fakeWatch) wantPhase(p watchPhase) {
	f.t.Helper()
	if f.w.phase != p {
		f.t.Fatalf("phase = %d, want %d (log: %q)", f.w.phase, p, f.logs)
	}
}

func (f *fakeWatch) logged(substr string) bool {
	for _, l := range f.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// 最初の画面がつながるまでは、どれだけ待っても終了しない。3分たったら URL をログで1回だけ案内する。
func TestWindowWatcherFirstWindow(t *testing.T) {
	f := newFakeWatch(t)
	f.ticks(179)
	if f.logged("Open ") {
		t.Fatalf("guided before 3 minutes: %q", f.logs)
	}
	f.ticks(1)
	if !f.logged("Open http://127.0.0.1:8737/admin in your browser") {
		t.Fatalf("no guidance after 3 minutes: %q", f.logs)
	}
	f.ticks(600)
	f.wantQuits(0)
	f.wantPhase(phaseFirstWait)
	guides := 0
	for _, l := range f.logs {
		if strings.Contains(l, "Open http") {
			guides++
		}
	}
	if guides != 1 {
		t.Errorf("guidance logged %d times, want 1", guides)
	}
	// 遅れてつながった画面を閉じれば、そこから終了する。
	f.connectThenClose()
	f.wantQuits(1)
}

// 猶予時間の内につなぎ直せば終了しない。0 のまま猶予時間が続けば、予約がなければ尋ねずに終了する。
func TestWindowWatcherReconnectWithinGrace(t *testing.T) {
	f := newFakeWatch(t)
	f.open = 1
	f.ticks(5)
	f.open = 0
	f.ticks(9)
	f.open = 1
	f.ticks(1)
	f.open = 0
	f.ticks(9)
	f.wantQuits(0)
	f.wantPhase(phaseOpen)
	f.ticks(2)
	f.wantQuits(1)
	f.wantPhase(phaseQuitting)
	if len(f.asked) != 0 {
		t.Errorf("asked %q with nothing pending", f.asked)
	}
	f.ticks(20)
	f.wantQuits(1)
}

// 既定の猶予時間は 5 秒（admin_ux_phase5_spec.md 1.1。ウィンドウを閉じてから短い時間で終わる）。0 のまま 5 秒続けば
// 終了し、再読み込みやページの移動のように猶予時間の内につなぎ直せば終了しない。
func TestWindowWatcherDefaultGraceIsFiveSeconds(t *testing.T) {
	if desktopWindowGrace != 5*time.Second {
		t.Fatalf("desktopWindowGrace = %s, want 5s", desktopWindowGrace)
	}
	if w := newWindowWatcher(&desktopApp{}); w.grace != desktopWindowGrace {
		t.Fatalf("newWindowWatcher grace = %s, want %s", w.grace, desktopWindowGrace)
	}
	f := newFakeWatch(t)
	f.w.grace = desktopWindowGrace
	f.open = 1
	f.ticks(1)
	f.open = 0
	f.ticks(5)
	f.open = 1
	f.ticks(1)
	f.open = 0
	f.ticks(5)
	f.wantQuits(0)
	f.wantPhase(phaseOpen)
	f.ticks(1)
	f.wantQuits(1)
	f.wantPhase(phaseQuitting)
}

// 反映やバックアップが動いている間は、終了も確認もしない。終わったら判断する。その間に画面がつながれば見張りに戻る。
func TestWindowWatcherWaitsWhileBusy(t *testing.T) {
	f := newFakeWatch(t)
	f.isBusy = true
	f.connectThenClose()
	f.ticks(60)
	f.wantQuits(0)
	f.wantPhase(phaseOpen)
	f.open = 1
	f.ticks(1)
	f.open = 0
	f.isBusy = false
	f.ticks(5)
	f.wantQuits(0) // つながったので、猶予時間を数え直す
	f.ticks(6)
	f.wantQuits(1)
}

// 予約があれば確認画面で尋ね、「今すぐ終了する」なら終了する。文面には件数と最後の日時（日本時間）を出す。
func TestWindowWatcherPendingQuitNow(t *testing.T) {
	f := newFakeWatch(t)
	f.count, f.last = 3, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	f.connectThenClose()
	f.wantQuits(0)
	f.wantPhase(phaseAsking)
	if len(f.dialogs) != 1 {
		t.Fatalf("%d quit questions, want 1", len(f.dialogs))
	}
	f.ticks(30) // 答えるまで、同じ確認画面を出し直さない
	if len(f.dialogs) != 1 {
		t.Fatalf("%d quit questions while waiting for the answer, want 1", len(f.dialogs))
	}
	f.answer(false)
	f.wantQuits(1)
	want := "予約した記事が 3 件あります（最後は 11月3日 09:00）。sorus を終了すると、その時刻にサイトへ反映されません。"
	if len(f.asked) != 1 || f.asked[0] != want {
		t.Errorf("asked %q, want %q", f.asked, want)
	}
}

// 「予約をすべて反映してから終了する」なら、予約が残らず、反映も動いていなくなってから終了する。
func TestWindowWatcherPendingFinishFirst(t *testing.T) {
	f := newFakeWatch(t)
	f.count, f.last = 2, f.clock.Add(time.Hour)
	f.connectThenClose()
	f.answer(true)
	f.wantPhase(phaseFinishing)
	f.ticks(30)
	f.wantQuits(0)
	f.count = 1
	f.ticks(30)
	f.wantQuits(0)
	f.count, f.isBusy = 0, true // 最後の反映が動いている
	f.ticks(5)
	f.wantQuits(0)
	f.isBusy = false
	f.ticks(1)
	f.wantQuits(1)
	if !f.logged("1 scheduled article(s) left") || !f.logged("every scheduled article was reflected; quitting") {
		t.Errorf("state changes not logged: %q", f.logs)
	}
	if len(f.asked) != 1 {
		t.Errorf("asked %d times, want 1", len(f.asked))
	}
}

// 反映が失敗した予約は残るため、待ちの状態を続けて終了しない。予約を数えられないときも終了しない。
func TestWindowWatcherFailedReflectKeepsWaiting(t *testing.T) {
	f := newFakeWatch(t)
	f.count, f.last = 1, f.clock.Add(time.Minute)
	f.connectThenClose()
	f.answer(true)
	f.isBusy = true
	f.ticks(70) // 予約時刻を過ぎて反映が動き、失敗する
	f.isBusy = false
	f.ticks(600)
	f.wantQuits(0)
	f.wantPhase(phaseFinishing)
	f.count, f.countErr = 0, errors.New("database is locked")
	f.ticks(30)
	f.wantQuits(0)
	f.countErr = nil
	f.ticks(1)
	f.wantQuits(1)
}

// 確認画面を出している間に画面がつながったら、答えを捨てて動き続ける（step が先に気づく場合も、答えが先の場合も）。
func TestWindowWatcherConnectDuringQuestion(t *testing.T) {
	for _, stepFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("stepFirst=", stepFirst), func(t *testing.T) {
			f := newFakeWatch(t)
			f.count = 1
			f.connectThenClose()
			f.wantPhase(phaseAsking)
			f.open = 1
			if stepFirst {
				f.ticks(1)
				f.wantPhase(phaseOpen)
			}
			f.answer(false)
			f.wantQuits(0)
			f.wantPhase(phaseOpen)
			f.ticks(30)
			f.wantQuits(0)
			// また閉じれば、もう一度尋ねる。
			f.open = 0
			f.ticks(11)
			f.wantPhase(phaseAsking)
			if len(f.dialogs) != 1 {
				t.Errorf("%d quit questions shown again, want 1", len(f.dialogs))
			}
		})
	}
}

// 待ちの状態で画面がつながったら（2回目の起動など）、待ちをやめて画面を見張る。
func TestWindowWatcherConnectDuringWait(t *testing.T) {
	f := newFakeWatch(t)
	f.count = 1
	f.connectThenClose()
	f.answer(true)
	f.ticks(5)
	f.open = 1
	f.ticks(1)
	f.wantPhase(phaseOpen)
	f.count = 0 // 予約を反映し終えても、画面が開いていれば終了しない
	f.ticks(30)
	f.wantQuits(0)
	f.open = 0
	f.ticks(11)
	f.wantQuits(1)
}

// 「sorus を終了」で「予約をすべて反映してから終了する」を選ぶと、確認画面を出さずに待ちの状態に入る。押した画面が
// 閉じても尋ねず、予約を反映し終えたら終了する。
func TestWindowWatcherQuitButtonFinishFirst(t *testing.T) {
	f := newFakeWatch(t)
	f.count = 2
	f.open = 1
	f.ticks(3)
	f.w.quitButton(true)
	f.wantPhase(phaseFinishing)
	f.ticks(2)
	f.open = 0 // 押した画面が「反映してから終了します」の画面（数えない）に変わる
	f.ticks(60)
	f.wantPhase(phaseFinishing)
	if len(f.dialogs) != 0 {
		t.Fatalf("quit question shown after the quit button: %q", f.asked)
	}
	f.w.quitButton(true) // もう一度押しても同じ
	f.wantPhase(phaseFinishing)
	f.count = 0
	f.ticks(1)
	f.wantQuits(1)
}

// 「sorus を終了」で「今すぐ終了する」（予約がないときの「sorus を終了」も同じ）なら、すぐに終了する。確認画面の答えを
// 待っている間・待ちの状態でも同じ。
func TestWindowWatcherQuitButtonNow(t *testing.T) {
	f := newFakeWatch(t)
	f.open = 1
	f.ticks(1)
	f.w.quitButton(false)
	f.wantQuits(1)
	f.wantPhase(phaseQuitting)
	f.w.quitButton(false)
	f.ticks(30)
	f.wantQuits(1)

	f = newFakeWatch(t)
	f.count = 1
	f.connectThenClose()
	f.w.quitButton(true)
	f.answer(false) // 待ちの状態に入ったあとの確認画面の答えは使わない
	f.wantQuits(0)
	f.wantPhase(phaseFinishing)
	f.w.quitButton(false)
	f.wantQuits(1)
}

// 待ちの状態に入るときに、公開日時を迎えた予約の反映を1回だけ始めさせる（確認画面の答え・「sorus を終了」のどちらでも）。
// 待っている間や、画面がつながって待ちをやめたあとは始めさせない。
func TestWindowWatcherFinishFirstReflectsDueArticles(t *testing.T) {
	for _, viaButton := range []bool{false, true} {
		t.Run(fmt.Sprint("viaButton=", viaButton), func(t *testing.T) {
			f := newFakeWatch(t)
			calls := 0
			f.w.reflectDue = func() bool {
				calls++
				return true
			}
			f.count = 1
			if viaButton {
				f.open = 1
				f.ticks(1)
				f.w.quitButton(true)
				f.open = 0
			} else {
				f.connectThenClose()
				f.answer(true)
			}
			f.wantPhase(phaseFinishing)
			f.ticks(60)
			f.w.quitButton(true)
			if calls != 1 {
				t.Fatalf("reflectDue called %d times, want 1", calls)
			}
			if !f.logged("started reflecting the scheduled articles whose publish time has passed") {
				t.Errorf("start of the reflect not logged: %q", f.logs)
			}
			f.open = 1
			f.ticks(1)
			f.wantPhase(phaseOpen)
			if calls != 1 {
				t.Errorf("reflectDue called %d times after a window connected, want 1", calls)
			}
		})
	}
}

// overdueSite は、公開先がこの PC のフォルダで、アプリを閉じている間に予約時刻を過ぎた記事（スラッグ while-closed）が
// 1件あるデスクトップのサイト。起動直後の予約の確認で、その記事は「予約時刻を過ぎた記事」として数えられ、予約投稿による
// 自動の反映は止まっている。
type overdueSite struct {
	srv *server.Server
	out string
}

func newOverdueSite(t *testing.T) *overdueSite {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Connect(filepath.Join(dir, "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role) VALUES ('w', 'writer0', 'w', 'x', 'admin')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO categories (id, name, slug) VALUES (1, '未分類', 'uncategorized')`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	a := &article.Article{AuthorID: "w", CategoryID: 1, Title: "閉じている間の予約", Slug: "while-closed", ContentMarkdown: "本文",
		Status: article.StatusPublished, PublishedAt: &at}
	if err := article.NewService(db).Save(a); err != nil {
		t.Fatal(err)
	}
	// 予約した（公開日時より前の）時点に保存した記事にする。
	if _, err := db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, at.Add(-24*time.Hour), a.ID); err != nil {
		t.Fatal(err)
	}
	const siteURL = "https://media.example.ac.jp"
	out := filepath.Join(t.TempDir(), "site")
	folder, _ := json.Marshal(map[string]string{"path": out})
	for k, v := range map[string]string{"publish_kind": "folder", "publish_folder": string(folder), "base_url": siteURL} {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := server.New(server.Config{DB: db, UploadDir: filepath.Join(dir, "uploads"), AttachmentDir: filepath.Join(dir, "attachments"),
		Desktop: true, BaseURL: siteURL, SchedulerInterval: 20 * time.Millisecond, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	// 起動直後の予約の確認が終わるまで待つ。
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM settings WHERE key = ?`, scheduler.CheckpointSetting); err == nil && n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduler did not check the scheduled articles")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &overdueSite{srv: srv, out: out}
}

// watch は、画面の数・時計・確認画面だけを差し替え、反映・予約の数・反映の開始は site の本物を使う windowWatcher。
func (site *overdueSite) watch(t *testing.T) *fakeWatch {
	f := newFakeWatch(t)
	f.w.busy = site.srv.Busy
	f.w.pending = site.srv.PendingScheduledReflects
	f.w.reflectDue = site.srv.ReflectDueNow
	return f
}

// untilQuit は、終了するまで（最大 20 秒）、時計を1秒ずつ進めて step を呼ぶ。
func (f *fakeWatch) untilQuit() {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for f.quits == 0 {
		if time.Now().After(deadline) {
			f.t.Fatalf("sorus did not quit (log: %q)", f.logs)
		}
		f.tick(time.Second)
		time.Sleep(10 * time.Millisecond)
	}
}

// expectSent は、予約時刻を過ぎた記事が公開先に届き、まだ反映していない予約が残っていないことを確かめる。
func (site *overdueSite) expectSent(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(site.out, "articles", "while-closed", "index.html")); err != nil {
		t.Fatalf("the overdue article was not sent to the folder: %v", err)
	}
	if n, _, err := site.srv.PendingScheduledReflects(); n != 0 || err != nil {
		t.Fatalf("PendingScheduledReflects after quitting = %d (%v), want 0", n, err)
	}
}

// 起動する前に予約時刻を過ぎた記事があり、予約投稿による自動の反映が止まっているサイトでも、確認画面で「予約をすべて
// 反映してから終了する」を選べば（確認画面を出せない OS の既定も同じ答え）、手動の反映でその記事を公開先へ送ってから
// 終了する。
func TestFinishFirstReflectsOverdueArticleThenQuits(t *testing.T) {
	site := newOverdueSite(t)
	f := site.watch(t)
	f.connectThenClose()
	f.wantPhase(phaseAsking)
	f.answer(true)
	f.wantPhase(phaseFinishing)
	f.untilQuit()
	site.expectSent(t)
	if !f.logged("started reflecting the scheduled articles") || !f.logged("every scheduled article was reflected; quitting") {
		t.Errorf("log = %q", f.logs)
	}
}

// 「sorus を終了」で「予約をすべて反映してから終了する」を選んだとき（server.Config.OnQuit(true)）も同じ。
func TestQuitButtonFinishFirstReflectsOverdueArticleThenQuits(t *testing.T) {
	site := newOverdueSite(t)
	f := site.watch(t)
	f.open = 1
	f.ticks(2)
	f.w.quitButton(true)
	f.wantPhase(phaseFinishing)
	f.open = 0
	f.untilQuit()
	site.expectSent(t)
	if len(f.asked) != 0 {
		t.Errorf("quit question shown after the quit button: %q", f.asked)
	}
}

func TestPendingQuitMessage(t *testing.T) {
	got := pendingQuitMessage(1, time.Time{})
	if want := "予約した記事が 1 件あります。sorus を終了すると、その時刻にサイトへ反映されません。"; got != want {
		t.Errorf("pendingQuitMessage = %q, want %q", got, want)
	}
	for _, bad := range []string{"ポート", "インスタンス", "サーバー"} {
		if strings.Contains(pendingQuitMessage(3, time.Now()), bad) {
			t.Errorf("message contains %q", bad)
		}
	}
}

// runDesktop は、最初の画面がつながるまでは終了せず、つながった画面がすべて閉じて猶予時間が過ぎたら（予約がなければ）
// 尋ねずに終了する。
func TestRunDesktopQuitsWhenWindowsClosed(t *testing.T) {
	p := desktopTestPaths(t)
	prevGrace := desktopWindowGrace
	desktopWindowGrace = time.Second
	t.Cleanup(func() { desktopWindowGrace = prevGrace })

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runDesktop(p, nil, stop) }()
	finished := false
	t.Cleanup(func() {
		close(stop)
		if !finished {
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Error("runDesktop did not stop")
			}
		}
	})
	inst, ok := desktop.WaitRunning(context.Background(), p.Instance, 30*time.Second)
	if !ok {
		t.Fatal("runDesktop did not write a reachable instance.json")
	}
	expectDesktopTestPort(t, inst.Port)

	// まだ画面がつながっていないので、猶予時間を過ぎても止まらない。
	select {
	case err := <-done:
		finished = true
		t.Fatalf("runDesktop stopped before any window connected: %v", err)
	case <-time.After(3 * time.Second):
	}

	// ページと同じく、同じ Origin から、ページのスクリプトにある合言葉を付けて WebSocket でつなぐ。
	conn, err := websocket.Dial("ws"+strings.TrimPrefix(inst.BaseURL(), "http")+server.WindowPath+"?t="+
		windowTokenFromPage(t, inst.BaseURL()+"/admin/login"), "", inst.BaseURL())
	if err != nil {
		t.Fatalf("%s: %v", server.WindowPath, err)
	}
	// つながっている間は止まらない。
	select {
	case err := <-done:
		finished = true
		conn.Close()
		t.Fatalf("runDesktop stopped while a window was open: %v", err)
	case <-time.After(3 * time.Second):
	}
	conn.Close()

	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("runDesktop: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("sorus did not stop after every window was closed")
	}
	if _, err := os.Stat(p.Instance); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("instance.json after quit: %v, want removed", err)
	}
	logData, err := os.ReadFile(p.Log)
	if err != nil {
		t.Fatal(err)
	}
	// 画面が切れたこと（とその理由）も sorus.log に残る。
	for _, want := range []string{"window: connected (1 open)", "window: disconnected (0 open): closed by the page",
		"every window was closed; quitting", "sorus stopped."} {
		if !strings.Contains(string(logData), want) {
			t.Errorf("sorus.log lacks %q:\n%s", want, logData)
		}
	}
}

// windowTokenRe は、ページに埋め込まれた開いている画面の接続のスクリプトから、合言葉（&t=）を取り出す。
var windowTokenRe = regexp.MustCompile(`&t=([0-9a-f]{32})'`)

// windowTokenFromPage は、ブラウザと同じく pageURL のページを開き、接続のスクリプトに埋め込まれた合言葉を返す。
func windowTokenFromPage(t *testing.T, pageURL string) string {
	t.Helper()
	res, err := http.Get(pageURL)
	if err != nil {
		t.Fatalf("GET %s: %v", pageURL, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	m := windowTokenRe.FindSubmatch(body)
	if m == nil {
		t.Fatalf("%s has no window token (status %d)", pageURL, res.StatusCode)
	}
	return string(m[1])
}
