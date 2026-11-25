package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"
)

// 開いている画面の数え方（desktop_app_window_spec.md 3-1 のサイトの側）。デスクトップモードでは、sorus のアプリの画面
// （管理画面・ログイン・はじめに・OAuth の同意画面。isAppWindowPage）が WindowPath への WebSocket の接続を1本保ち、
// つながっている間を「開いている画面」として数える。公開ページ（記事・トップ・カテゴリ・検索など）は数えない。公開
// ページを別のブラウザ（Chrome など）で開いたままアプリのウィンドウを閉じても、sorus が終わるように
// （desktop_quit_and_editor_ux_spec.md 1.1）。アプリ（main.go の desktopApp）は、すべてのサイトの OpenWindows の
// 合計で、画面がすべて閉じたかを判断する。
//
// SSE（EventSource）ではなく WebSocket にするのは、ブラウザが同じホストへの HTTP/1.1 の接続を 6 本までしか開かない
// ため。画面ごとに SSE を1本保つと、6 枚開いたところで次のページが読み込めなくなる（v0.1.6）。WebSocket の接続はこの
// 上限に数えない。タイマーで定期的に知らせる方式にしないのは、最小化・非表示のページではブラウザがタイマーを
// 間引き、開いている画面を閉じたと見誤って sorus が終わってしまうため。

// WindowPath は開いている画面が保つ接続（WebSocket）のパス。デスクトップモードでだけ登録し、サーバーモード・公開専用
// モードではほかの無いページと同じ 404 になる。先頭部分（sorus-window）は article の予約語にしてあり、固定ページの
// スラッグに使えない（_ で始まるパスにしないのは、スラッグを整えると _ が消え、予約語として断れないため）。
const WindowPath = "/sorus-window"

// windowKeepalive は、相手がいなくなった接続に気づくために送る ping の間隔。windowWriteWait は ping を書き込むのを
// 待つ上限。windowReadWait は、ブラウザから何も届かない（送った ping に pong が返らない）まま待つ上限で、過ぎたら
// 相手はもう応えないとみなして切る（ping の書き込みが通るだけでは、相手が生きているとは限らない）。pong の遅れ1回で
// 切らないよう、ping の間隔の 2.5 倍にする。windowRetry は接続が切れてからブラウザがつなぎ直すまでの待ち時間（ミリ秒）。
// 再読み込みやページの移動でつなぎ直すまでの間を、アプリの猶予時間（既定 5 秒、main の desktopWindowGrace）より短くする。
//
// ウィンドウを閉じたら 1 分以内に確実に終わるようにする（admin_ux_phase5_spec.md 1.1）。ページが close を送れたときは
// 猶予時間の 5 秒で、close が届かず応答だけが途絶えたときでも windowReadWait（25 秒）＋猶予時間でおよそ 30 秒で終わる。
var (
	windowKeepalive = 10 * time.Second
	windowWriteWait = 10 * time.Second
	windowReadWait  = 25 * time.Second
	windowRetry     = 2000
)

// windowRetryMax は、つながらないまま切れる（sorus に断られた・sorus がいない）ことが続いたときに、つなぎ直すまで待つ
// 上限（ミリ秒）。
const windowRetryMax = 30000

// windowScriptWith は WindowPath へ接続するスクリプトを返す。外部のファイルを読まず、ページに直接埋め込む（管理画面の
// レイアウトの WindowScript）。WebSocket は切れても自分ではつなぎ直さないため、閉じたら windowRetry ミリ秒後に
// つなぎ直す。プレビューなどの iframe の中では数えない（画面の数はウィンドウ・タブの数）。WebSocket は参照が無いと
// 回収されて切れることがあるため、window に持たせる。どのページの接続かを動作の記録に残せるよう、パスを ?page= に付ける。
//
// token は、この sorus（の Server）が起動したときに作った合言葉（windowState.token）。接続の URL の &t= に付け、sorus は
// 合わなければ断る。前に起動した sorus が描いたページ（開いたままのタブ）は古い合言葉を持ったままつなぎ直してくるが、
// 今の sorus はそれを数えない（数えると、閉じ忘れたタブで sorus が終わらなくなる）。合言葉はページの中のスクリプトにだけ
// 置き、ページの URL・Cookie には出さない。
//
// つながる前に切れた（断られた・sorus がいない）ことが続くときは、待ち時間を倍にしていき（上限 windowRetryMax）、
// 無駄につなぎ直し続けない。sorus が規約違反（1008）で閉じたときは、つなぎ直さない。一度つながれば、待ち時間は
// windowRetry に戻る。
//
// ページを離れる・閉じるとき（pagehide・beforeunload）は、closing を立てて自分から閉じ（正常終了の 1000 を送り、sorus が
// 応答の途絶えを待たずにすぐ数から外せるようにする）、つなぎ直さない。戻る・進むで
// ページがそのまま戻ったとき（pageshow の persisted）は、つなぎ直す。ダウンロードのリンクなど、beforeunload の後も
// ページが残ったときは、windowRetry ミリ秒後につなぎ直す（ページを離れたら、この時計は動かない）。古い接続の onclose・
// 前に予約したつなぎ直しが、新しい接続と重ならないよう、今の接続（w.socket）のときだけつなぎ直し、つながっている
// 間はつなぎ直さない。
func windowScriptWith(token string) template.HTML {
	// token は newWindowToken の 16 進の文字列で、JS の文字列にそのまま埋め込める。
	retry := strconv.Itoa(windowRetry)
	return template.HTML(`<script>
(function() {
  if (window.top !== window || !window.WebSocket || window.sorusWindow) return;
  var url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '` + WindowPath + `' +
    '?page=' + encodeURIComponent(location.pathname) + '&t=` + token + `';
  var w = window.sorusWindow = {};
  var closing = false;
  var fails = 0;
  function connect() {
    if (closing || (w.socket && w.socket.readyState <= 1)) return;
    var s = w.socket = new WebSocket(url);
    var opened = false;
    s.onopen = function() { opened = true; fails = 0; };
    s.onclose = function(e) {
      if (closing) return;
      if (w.socket !== s) return;
      if (e && e.code === 1008) return;
      if (!opened) fails++;
      var wait = fails > 1 ? Math.min(` + retry + ` * Math.pow(2, fails - 1), ` + strconv.Itoa(windowRetryMax) + `) : ` + retry + `;
      setTimeout(connect, wait);
    };
  }
  function suspend() {
    closing = true;
    if (w.socket) { try { w.socket.close(1000); } catch (_) {} }
  }
  function resume() {
    if (!closing) return;
    closing = false;
    connect();
  }
  window.addEventListener('pagehide', suspend);
  window.addEventListener('beforeunload', function() {
    suspend();
    setTimeout(resume, ` + retry + `);
  });
  window.addEventListener('pageshow', function(e) { if (e.persisted) resume(); });
  connect();
})();
</script>`)
}

// windowState は Server ごとの開いている画面の状態。publishState と同じく、Server の構造体にフィールドを足さずに
// 弱い参照のキーで持つ。
type windowState struct {
	open atomic.Int64
	// token は、この Server の接続だけを受け付けるための合言葉（16 バイトの乱数の 16 進）。Server ごとに起動のたびに
	// 作り直す（windowScriptWith）。
	token string
	// done は Shutdown が始まると閉じる。開いたままの接続を終わらせ、Shutdown が待ち続けないようにする。
	done     chan struct{}
	stopOnce sync.Once
}

var windowStates sync.Map // weak.Pointer[Server] -> *windowState

func (s *Server) windowState() *windowState {
	key := weak.Make(s)
	if v, ok := windowStates.Load(key); ok {
		return v.(*windowState)
	}
	v, loaded := windowStates.LoadOrStore(key, &windowState{token: newWindowToken(), done: make(chan struct{})})
	if !loaded {
		runtime.AddCleanup(s, func(k weak.Pointer[Server]) { windowStates.Delete(k) }, key)
	}
	return v.(*windowState)
}

// newWindowToken は windowState.token にする 16 バイトの乱数を 16 進で返す。
func newWindowToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // Go 1.24 以降の crypto/rand.Read は失敗しない（失敗すればプロセスが止まる）。
	return hex.EncodeToString(b)
}

func (ws *windowState) stop() { ws.stopOnce.Do(func() { close(ws.done) }) }

// OpenWindows は、このサイトの画面のうち、今つながっている数を返す（デスクトップモードでなければ常に 0）。
func (s *Server) OpenWindows() int {
	return int(s.windowState().open.Load())
}

// registerWindowRoute は WindowPath を登録する。デスクトップモードのときだけ呼ぶ。
func (s *Server) registerWindowRoute() {
	ws := s.windowState()
	s.echo.Server.RegisterOnShutdown(ws.stop)
	s.echo.GET(WindowPath, s.handleWindow)
}

// windowScriptFor は、デスクトップモードでレイアウトに埋め込む接続のスクリプトを返す。デスクトップモードでないとき、
// 静的な書き出し（公開先へ送るファイル）、アプリの画面ではないページ（公開ページ）では空を返す。
func (s *Server) windowScriptFor(c echo.Context) template.HTML {
	if !s.cfg.Desktop || isStaticExport(c) || !isAppWindowPage(c) {
		return ""
	}
	return windowScriptWith(s.windowState().token)
}

// appWindowPrefixes は、開いている画面として数えるページのパスの先頭。管理画面（ログイン・はじめに・終了の確認を含む）
// と、MCP 連携の OAuth の同意画面。/setup は今は無いが、はじめにの画面として数える側に入れておく。
var appWindowPrefixes = []string{"/admin", "/setup", "/oauth"}

// isAppWindowPage は c のページを開いている画面として数えるか（アプリの画面か）を返す。管理画面のテーマのプレビュー
// （/admin/settings/theme/preview）は、公開ページを内部の要求（/ や記事のパス）で描くため、パスではなく文脈で見る。
func isAppWindowPage(c echo.Context) bool {
	if isThemePreview(c) {
		return true
	}
	p := c.Request().URL.Path
	for _, prefix := range appWindowPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// handleWindow は開いている画面の接続（WebSocket）。同じサイトのページ（Origin のホストが要求の Host と同じ）からの
// 接続だけを受け付け、それ以外は断る（403）。ほかのサイトのページが接続を保って sorus を動かし続けられないようにする。
// 合言葉（?t=）がこの Server の windowState.token と合わない接続も断り（403）、数えない。前に起動した sorus が描いた
// ページ（開いたままのタブ）や、Origin を偽ったほかのプログラムが、sorus が終わるのを妨げられないようにする。
func (s *Server) handleWindow(c echo.Context) error {
	ws := s.windowState()
	select {
	case <-ws.done:
		return echo.NewHTTPError(http.StatusServiceUnavailable)
	default:
	}
	if subtle.ConstantTimeCompare([]byte(c.QueryParam("t")), []byte(ws.token)) != 1 {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	page := windowPage(c.Request())
	websocket.Server{
		Handshake: sameOriginWindow,
		Handler:   func(conn *websocket.Conn) { s.serveWindow(ws, conn, page) },
	}.ServeHTTP(c.Response(), c.Request())
	return nil
}

// windowPageMax は動作の記録に残すページのパスの長さの上限。
const windowPageMax = 200

// windowPage は動作の記録に残す、接続したページのパス（windowScript が ?page= に付ける）。/ で始まらなければ残さない。
// 記録の行を乱さないよう、表示できる ASCII 以外の文字は ? に置き換え、長すぎれば切る。
func windowPage(r *http.Request) string {
	p := r.URL.Query().Get("page")
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	if len(p) > windowPageMax {
		p = p[:windowPageMax]
	}
	b := []byte(p)
	for i, ch := range b {
		if ch <= ' ' || ch > '~' {
			b[i] = '?'
		}
	}
	return string(b)
}

// sameOriginWindow は、接続を求めたページの Origin のホストが要求の Host と同じかを確かめる。Origin の無い要求
// （ブラウザのページからではない）も断る。
func sameOriginWindow(cfg *websocket.Config, req *http.Request) error {
	origin, err := websocket.Origin(cfg, req)
	if err != nil {
		return err
	}
	if origin == nil || !strings.EqualFold(origin.Host, req.Host) {
		return errors.New("window: cross-origin connection refused")
	}
	cfg.Origin = origin
	return nil
}

// serveWindow は、つながっている間を1つの画面として数え、切れたら（ページを閉じた・移動した）数から1回だけ外す。
// ブラウザから届くのは ping への pong と、閉じるときの close だけ。読み取りがエラーを返す（close が届いた・接続が
// 切れた・windowReadWait の間なにも届かない）まで読み続けて、切れたことに気づく（readWindow）。Shutdown が始まったら
// 接続を閉じて終わらせる。page は接続したページのパス（動作の記録に残す。分からなければ空）。
func (s *Server) serveWindow(ws *windowState, conn *websocket.Conn, page string) {
	// サーバー全体の読み書きのタイムアウトは、この接続には使わない（つながっている間ずっと開いたままにする）。
	// net/http（Go 1.25）は接続を乗っ取るときに期限を外すが、http.Hijacker の文書は期限が残りうるとしているため、
	// ここでも外す。
	_ = conn.SetDeadline(time.Time{})
	// データは送らない。書き込むのは ping だけ（pingWindow）。
	conn.PayloadType = websocket.PingFrame
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }

	// 数の変化は動作の記録（sorus.log）に残す（画面を閉じたのに終わらない・勝手に終わったときに確かめられるように）。
	// Echo のログは既定でエラーだけを出すため、Config.LogOutput の出力先へレベルなしで書く。
	// 切れた理由も残す（閉じたのに数えたまま・勝手に外れたときに、どちらの側が切ったか確かめられるように）。
	logger := log.New(s.echo.Logger.Output(), "", log.LstdFlags)
	where := ""
	if page != "" {
		where = " " + page
	}
	n := ws.open.Add(1)
	logger.Printf("window: connected (%d open)%s", n, where)
	reason := "unknown"
	defer func() {
		n := ws.open.Add(-1)
		logger.Printf("window: disconnected (%d open)%s: %s", n, where, reason)
	}()

	// Shutdown を待つ・ping を送るのは別の goroutine で行う。読み取りが終わったら止め、終わるのを待ってから閉じる。
	// sorus の側から閉じたときは、その理由を closedBy に入れてから閉じる（読むのは goroutine が終わった後）。
	var closedBy string
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(windowKeepalive)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ws.done:
				closedBy = "sorus is shutting down"
				closeConn()
				return
			case <-ticker.C:
				if !pingWindow(conn) {
					closedBy = "could not send a ping"
					closeConn()
					return
				}
			}
		}
	}()
	reason = readWindow(conn)
	close(stop)
	<-stopped
	if closedBy != "" {
		reason = closedBy
	}
	closeConn()
}

// readWindow は、接続が終わるまでブラウザから届くフレームを読み、終わった理由を返す。フレームが届くたび（ブラウザが
// ping に自動で返す pong を含む）に、読み取りの期限を windowReadWait 先へ延ばす。Conn.Read は pong を中で読み捨てて
// 戻らず、期限を延ばせないため、フレームの単位で読む。ping への応答と close の扱いは x/net/websocket の
// HandleFrame に任せ、データのフレーム（送られないはず）は読み捨てる。
func readWindow(conn *websocket.Conn) string {
	for {
		if err := conn.SetReadDeadline(time.Now().Add(windowReadWait)); err != nil {
			return "read error: " + err.Error()
		}
		frame, err := conn.NewFrameReader()
		if err != nil {
			return windowReadReason(err)
		}
		data, err := conn.HandleFrame(frame)
		if errors.Is(err, io.EOF) {
			// close のフレームが届いた（ページを閉じた・移動した）。
			return "closed by the page"
		}
		if err != nil {
			return windowReadReason(err)
		}
		if data == nil {
			continue // ping・pong
		}
		if _, err := io.Copy(io.Discard, data); err != nil {
			return windowReadReason(err)
		}
		if trailer := data.TrailerReader(); trailer != nil {
			if _, err := io.Copy(io.Discard, trailer); err != nil {
				return windowReadReason(err)
			}
		}
	}
}

// windowReadReason は、フレームを読めなかったエラーを、動作の記録に残す切れた理由にする。
func windowReadReason(err error) string {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "no response from the page for " + windowReadWait.String()
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection was lost"
	default:
		return "read error: " + err.Error()
	}
}

// pingWindow は ping を1つ送る（ブラウザが自動で応える）。書き込めなければ相手はもういないとみなして false を返す。
func pingWindow(conn *websocket.Conn) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(windowWriteWait)); err != nil {
		return false
	}
	if _, err := conn.Write(nil); err != nil {
		return false
	}
	return conn.SetWriteDeadline(time.Time{}) == nil
}
