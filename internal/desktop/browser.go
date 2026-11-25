package desktop

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// NoBrowserEnv を 1 にすると OpenBrowser・OpenWindow はブラウザを開かない（テスト・自動化向け）。
const NoBrowserEnv = "SORUS_NO_BROWSER"

// アプリモードで開くウィンドウの大きさ（デバイス非依存ピクセル。DIP）。
//
// 理想の大きさ（idealWindowW×idealWindowH）は、管理画面の中央の枠（max-w-7xl = 1280px）がちょうど収まり、記事の
// 編集画面が編集欄とプレビューの2列（lg = 1024px 以上）で開く大きさ。画面の作業領域（タスクバー・メニューバーを除いた
// 範囲）が分かれば、その 9 割に収まるよう縮める（windowSizeFor）。ただし縮めても下限（minWindowW×minWindowH）までに
// とどめ、作業領域が下限より狭い画面だけは作業領域いっぱいにする。
const (
	idealWindowW = 1280
	idealWindowH = 860
	minWindowW   = 960
	minWindowH   = 640
)

// windowSizeFor は作業領域の幅 workW・高さ workH（DIP）の画面で、アプリモードのウィンドウを開く大きさを返す。
// 作業領域が分からない（0 以下）ときは理想の大きさを返す。
func windowSizeFor(workW, workH int) (w, h int) {
	if workW <= 0 || workH <= 0 {
		return idealWindowW, idealWindowH
	}
	return clampWindowSide(idealWindowW, minWindowW, workW), clampWindowSide(idealWindowH, minWindowH, workH)
}

// clampWindowSide はウィンドウの1辺の長さを、作業領域 work の 9 割（切り捨て）と理想 ideal の小さいほうにする。
// それが下限 lower を下回れば lower にするが、作業領域 work を超えないようにする。
func clampWindowSide(ideal, lower, work int) int {
	n := min(ideal, work*9/10)
	if n < lower {
		n = min(lower, work)
	}
	return n
}

// appWindowSize はこの PC の画面の作業領域から決めた、アプリモードのウィンドウの大きさ。作業領域を調べられない OS・
// 調べられなかったときは理想の大きさ。
func appWindowSize() (w, h int) {
	return windowSizeFor(screenWorkArea())
}

// workAreaDIP は物理ピクセル（DPI が dpi の画面）の幅 w・高さ h を、96 DPI を基準にしたデバイス非依存ピクセル（DIP）に
// 直す。dpi が分からない（0 以下）ときは 96 DPI とみなし、そのまま返す。
func workAreaDIP(w, h, dpi int) (int, int) {
	if dpi <= 0 {
		dpi = 96
	}
	return w * 96 / dpi, h * 96 / dpi
}

// parseScreenSize は "幅,高さ"（区切りはカンマ。前後の空白と小数は許す）の形の文字列を、正の整数の幅・高さにする。
// 読めなければ 0, 0 を返す（windowSizeFor が理想の大きさにする）。macOS の osascript の出力を読むのに使う。
func parseScreenSize(s string) (w, h int) {
	ws, hs, ok := strings.Cut(strings.TrimSpace(s), ",")
	if !ok {
		return 0, 0
	}
	fw, err1 := strconv.ParseFloat(strings.TrimSpace(ws), 64)
	fh, err2 := strconv.ParseFloat(strings.TrimSpace(hs), 64)
	if err1 != nil || err2 != nil || fw < 1 || fh < 1 || fw > 100000 || fh > 100000 {
		return 0, 0
	}
	return int(fw), int(fh)
}

// OpenBrowser は url を OS の既定のブラウザで開く。環境変数 SORUS_NO_BROWSER が 1 なら何もしない。
// 開けなくても sorus は動き続けるため、呼び出し側はエラーをログに残して URL を案内するだけでよい。
func OpenBrowser(url string) error {
	if os.Getenv(NoBrowserEnv) == "1" {
		return nil
	}
	name, args := browserCommand(runtime.GOOS, url)
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("desktop: open browser: %w", err)
	}
	// 終了を待って子プロセスを片付ける（ブラウザ自体は別のプロセスとして動き続ける）。
	go cmd.Wait()
	return nil
}

// browserCommand は goos の OS で url を既定のブラウザで開くコマンドと引数を返す。
func browserCommand(goos, url string) (string, []string) {
	switch goos {
	case "windows":
		// start コマンドと違い、URL の & などをシェルに解釈されない。
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		return "open", []string{url}
	default:
		return "xdg-open", []string{url}
	}
}

// OpenWindow は url を Chromium 系ブラウザのアプリモード（--app。アドレスバーとタブの無い独立したウィンドウ）で
// 開く。ブラウザは Windows: Microsoft Edge、macOS: Google Chrome・Microsoft Edge・Chromium、Linux:
// google-chrome・chromium・microsoft-edge の順に探し、見つからないか起動できなければ OpenBrowser と同じく既定の
// ブラウザで開く。利用者のいつものプロファイルを使うため、初回の設定画面は出ない。環境変数 SORUS_NO_BROWSER が
// 1 なら何もしない。開けなくても sorus は動き続けるため、呼び出し側はエラーをログに残して URL を案内すればよい。
func OpenWindow(url string) error {
	if os.Getenv(NoBrowserEnv) == "1" {
		return nil
	}
	w, h := appWindowSize()
	return openWindow(runtime.GOOS, url, w, h, systemLookup, startProcess)
}

// openWindow は goos の OS で OpenWindow を行う。アプリモードのウィンドウは幅 w・高さ h（DIP）で開く。
// ブラウザの探し方 l と起動の仕方 start を試験で差し替える。
func openWindow(goos, url string, w, h int, l browserLookup, start func(name string, args []string) error) error {
	if browser := findAppBrowser(goos, l); browser != "" {
		name, args := appWindowCommand(goos, browser, url, w, h)
		err := start(name, args)
		if err == nil {
			return nil
		}
		log.Printf("sorus: %s をアプリモードで起動できないため、既定のブラウザで開きます: %v", browser, err)
	} else {
		log.Printf("sorus: アプリモードで開けるブラウザ（Chrome・Edge・Chromium）が見つからないため、既定のブラウザで開きます")
	}
	name, args := browserCommand(goos, url)
	if err := start(name, args); err != nil {
		return fmt.Errorf("desktop: open browser: %w", err)
	}
	return nil
}

// startProcess は name を args で起動し、終了を待たずに戻る（ブラウザは別のプロセスとして動き続ける）。
var startProcess = func(name string, args []string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// browserLookup はアプリモードに使うブラウザを探すための OS への問い合わせ。試験で差し替える。
type browserLookup struct {
	getenv   func(key string) string
	home     func() (string, error)
	appPath  func(exe string) string // Windows の App Paths に登録された exe の場所。無ければ空
	exists   func(path string) bool
	lookPath func(file string) (string, error)
}

// systemLookup は実際の OS に問い合わせる browserLookup。
var systemLookup = browserLookup{
	getenv:   os.Getenv,
	home:     os.UserHomeDir,
	appPath:  registeredAppPath,
	exists:   func(p string) bool { _, err := os.Stat(p); return err == nil },
	lookPath: exec.LookPath,
}

// macAppNames は macOS でアプリモードに使うブラウザの .app の名前。探す順に並べる。
var macAppNames = []string{"Google Chrome.app", "Microsoft Edge.app", "Chromium.app"}

// linuxBrowserCommands は Linux などでアプリモードに使うブラウザのコマンド名。PATH から探す順に並べる。
var linuxBrowserCommands = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"}

// appBrowserCandidates は goos の OS でアプリモードに使うブラウザの候補を、探す順に返す。Windows と macOS は
// 実行ファイル（macOS は .app）の絶対パス、ほかの OS は PATH から探すコマンド名を返す。
func appBrowserCandidates(goos string, l browserLookup) []string {
	switch goos {
	case "windows":
		var c []string
		if p := l.appPath("msedge.exe"); p != "" {
			c = append(c, p)
		}
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			if dir := l.getenv(env); dir != "" {
				c = append(c, strings.TrimRight(dir, `\`)+`\Microsoft\Edge\Application\msedge.exe`)
			}
		}
		return c
	case "darwin":
		dirs := []string{"/Applications"}
		if home, err := l.home(); err == nil && home != "" {
			dirs = append(dirs, path.Join(home, "Applications"))
		}
		var c []string
		for _, app := range macAppNames {
			for _, dir := range dirs {
				c = append(c, path.Join(dir, app))
			}
		}
		return c
	default:
		return linuxBrowserCommands
	}
}

// findAppBrowser は goos の OS でアプリモードに使うブラウザを探し、見つかった最初の候補の場所を返す。
// 見つからなければ空を返す。
func findAppBrowser(goos string, l browserLookup) string {
	for _, c := range appBrowserCandidates(goos, l) {
		switch goos {
		case "windows", "darwin":
			if l.exists(c) {
				return c
			}
		default:
			if p, err := l.lookPath(c); err == nil {
				return p
			}
		}
	}
	return ""
}

// appWindowCommand は goos の OS で、findAppBrowser が見つけた browser のアプリモードで url を、幅 w・高さ h（DIP）の
// ウィンドウで開くコマンドと引数を返す。macOS は open -na で .app を新しく起動する（すでに起動していても、引数が今の
// ブラウザに渡る）。
func appWindowCommand(goos, browser, url string, w, h int) (string, []string) {
	appArgs := []string{"--app=" + url, fmt.Sprintf("--window-size=%d,%d", w, h)}
	if goos == "darwin" {
		return "open", append([]string{"-na", browser, "--args"}, appArgs...)
	}
	return browser, appArgs
}
