package desktop

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// fakeLookup は、found に挙げた場所（Windows・macOS）とコマンド名（Linux）だけがある環境の browserLookup を返す。
func fakeLookup(found ...string) browserLookup {
	return browserLookup{
		getenv: func(key string) string {
			return map[string]string{
				"ProgramFiles(x86)": `C:\Program Files (x86)`,
				"ProgramFiles":      `C:\Program Files`,
				"LocalAppData":      `C:\Users\sorus\AppData\Local`,
			}[key]
		},
		home:    func() (string, error) { return "/Users/sorus", nil },
		appPath: func(string) string { return "" },
		exists:  func(p string) bool { return slices.Contains(found, p) },
		lookPath: func(f string) (string, error) {
			if slices.Contains(found, f) {
				return "/usr/bin/" + f, nil
			}
			return "", exec.ErrNotFound
		},
	}
}

// OS ごとに、アプリモードに使うブラウザの候補を決めた順に探す。
func TestAppBrowserCandidates(t *testing.T) {
	withAppPath := fakeLookup()
	withAppPath.appPath = func(exe string) string {
		if exe == "msedge.exe" {
			return `D:\Edge\msedge.exe`
		}
		return ""
	}
	noEnv := fakeLookup()
	noEnv.getenv = func(string) string { return "" }
	noEnv.home = func() (string, error) { return "", errors.New("no home") }
	tests := []struct {
		name string
		goos string
		l    browserLookup
		want []string
	}{
		{"windows", "windows", fakeLookup(), []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Users\sorus\AppData\Local\Microsoft\Edge\Application\msedge.exe`,
		}},
		{"windows App Paths first", "windows", withAppPath, []string{
			`D:\Edge\msedge.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Users\sorus\AppData\Local\Microsoft\Edge\Application\msedge.exe`,
		}},
		{"windows without env", "windows", noEnv, nil},
		{"darwin", "darwin", fakeLookup(), []string{
			"/Applications/Google Chrome.app", "/Users/sorus/Applications/Google Chrome.app",
			"/Applications/Microsoft Edge.app", "/Users/sorus/Applications/Microsoft Edge.app",
			"/Applications/Chromium.app", "/Users/sorus/Applications/Chromium.app",
		}},
		{"darwin without home", "darwin", noEnv, []string{
			"/Applications/Google Chrome.app", "/Applications/Microsoft Edge.app", "/Applications/Chromium.app",
		}},
		{"linux", "linux", fakeLookup(), []string{
			"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge",
		}},
	}
	for _, tt := range tests {
		if got := appBrowserCandidates(tt.goos, tt.l); !slices.Equal(got, tt.want) {
			t.Errorf("%s: candidates = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// 見つかった候補のうち、先に並ぶものを選ぶ。どれも無ければ空を返す。
func TestFindAppBrowser(t *testing.T) {
	tests := []struct {
		goos  string
		found []string
		want  string
	}{
		{"windows", []string{
			`C:\Users\sorus\AppData\Local\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		}, `C:\Program Files\Microsoft\Edge\Application\msedge.exe`},
		{"windows", nil, ""},
		{"darwin", []string{"/Applications/Chromium.app", "/Users/sorus/Applications/Microsoft Edge.app"},
			"/Users/sorus/Applications/Microsoft Edge.app"},
		{"darwin", nil, ""},
		{"linux", []string{"microsoft-edge", "chromium-browser"}, "/usr/bin/chromium-browser"},
		{"linux", []string{"google-chrome-stable", "google-chrome"}, "/usr/bin/google-chrome"},
		{"linux", nil, ""},
	}
	for _, tt := range tests {
		if got := findAppBrowser(tt.goos, fakeLookup(tt.found...)); got != tt.want {
			t.Errorf("%s with %q: %q, want %q", tt.goos, tt.found, got, tt.want)
		}
	}
}

// アプリモードの起動は --app と --window-size を渡す。macOS は open -na で .app に引数を渡す。
func TestAppWindowCommand(t *testing.T) {
	const u = "http://127.0.0.1:8780/admin?a=1&b=2"
	tests := []struct {
		goos, browser string
		name          string
		args          []string
	}{
		{"windows", `C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			[]string{"--app=" + u, "--window-size=1280,860"}},
		{"darwin", "/Applications/Google Chrome.app", "open",
			[]string{"-na", "/Applications/Google Chrome.app", "--args", "--app=" + u, "--window-size=1280,860"}},
		{"linux", "/usr/bin/google-chrome", "/usr/bin/google-chrome",
			[]string{"--app=" + u, "--window-size=1280,860"}},
	}
	for _, tt := range tests {
		name, args := appWindowCommand(tt.goos, tt.browser, u, 1280, 860)
		if name != tt.name || !slices.Equal(args, tt.args) {
			t.Errorf("%s: %s %q, want %s %q", tt.goos, name, args, tt.name, tt.args)
		}
		for _, a := range args {
			if strings.HasPrefix(a, "--user-data-dir") {
				t.Errorf("%s: passes %s; the normal profile must be used", tt.goos, a)
			}
		}
	}
}

// アプリモードのウィンドウの大きさは引数の幅・高さを --window-size に渡す（作業領域に合わせて縮めた値も同じ形）。
func TestAppWindowCommandUsesGivenSize(t *testing.T) {
	const u = "http://127.0.0.1:8780/admin"
	_, args := appWindowCommand("windows", `C:\msedge.exe`, u, 1229, 655)
	if want := []string{"--app=" + u, "--window-size=1229,655"}; !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

// ウィンドウの大きさは、理想の 1280×860 を作業領域の 9 割までに縮め、下限 960×640 は作業領域が許す範囲で守る。
// 作業領域は DIP（Windows の拡大率を反映した後の大きさ）で、タスクバー（約 40）・メニューバーを除いた値。
func TestWindowSizeFor(t *testing.T) {
	tests := []struct {
		name         string
		workW, workH int
		w, h         int
	}{
		{"unknown work area", 0, 0, 1280, 860},
		{"negative work area", -1, 768, 1280, 860},
		{"1920x1080 at 100%", 1920, 1040, 1280, 860},
		{"2560x1440 at 100%", 2560, 1400, 1280, 860},
		{"1920x1080 at 125%", 1536, 824, 1280, 741},
		{"1920x1080 at 150%", 1280, 680, 1152, 640},
		{"1440x900 at 100%", 1440, 860, 1280, 774},
		{"1366x768 at 100%", 1366, 728, 1229, 655},
		{"1366x768 at 125%", 1093, 582, 983, 582},
		{"1280x720 at 100%", 1280, 680, 1152, 640},
		{"1280x800 at 100%", 1280, 760, 1152, 684},
		{"MacBook Air 13 (1470x956 points)", 1470, 919, 1280, 827},
		{"1024x768", 1024, 728, 960, 655},
		{"800x600", 800, 560, 800, 560},
	}
	for _, tt := range tests {
		w, h := windowSizeFor(tt.workW, tt.workH)
		if w != tt.w || h != tt.h {
			t.Errorf("%s (%dx%d): %dx%d, want %dx%d", tt.name, tt.workW, tt.workH, w, h, tt.w, tt.h)
		}
		if tt.workW > 0 && tt.workH > 0 && (w > tt.workW || h > tt.workH) {
			t.Errorf("%s: %dx%d is larger than the work area %dx%d", tt.name, w, h, tt.workW, tt.workH)
		}
	}
}

// DPI 対応のプロセスとして物理ピクセルが返ったときは、システムの DPI で DIP に割り戻す。
func TestWorkAreaDIP(t *testing.T) {
	tests := []struct {
		w, h, dpi int
		wantW     int
		wantH     int
	}{
		{1920, 1040, 96, 1920, 1040},
		{1920, 1040, 144, 1280, 693},
		{1366, 728, 120, 1092, 582},
		{1366, 728, 0, 1366, 728},
	}
	for _, tt := range tests {
		if w, h := workAreaDIP(tt.w, tt.h, tt.dpi); w != tt.wantW || h != tt.wantH {
			t.Errorf("workAreaDIP(%d, %d, %d) = %d, %d, want %d, %d", tt.w, tt.h, tt.dpi, w, h, tt.wantW, tt.wantH)
		}
	}
}

// macOS の osascript が出す "幅,高さ" を読む。読めない出力は 0, 0（理想の大きさで開く）。
func TestParseScreenSize(t *testing.T) {
	tests := []struct {
		in   string
		w, h int
	}{
		{"1440,875\n", 1440, 875},
		{" 1512 , 944 ", 1512, 944},
		{"1470.5,919.25", 1470, 919},
		{"", 0, 0},
		{"1440", 0, 0},
		{"abc,def", 0, 0},
		{"0,900", 0, 0},
		{"-1440,900", 0, 0},
		{"execution error: not allowed (-1743)", 0, 0},
	}
	for _, tt := range tests {
		if w, h := parseScreenSize(tt.in); w != tt.w || h != tt.h {
			t.Errorf("parseScreenSize(%q) = %d, %d, want %d, %d", tt.in, w, h, tt.w, tt.h)
		}
	}
}

// startRecorder は起動したコマンドを覚える。fail に挙げたコマンドは起動に失敗する。
type startRecorder struct {
	started [][]string
	fail    []string
}

func (r *startRecorder) start(name string, args []string) error {
	r.started = append(r.started, append([]string{name}, args...))
	if slices.Contains(r.fail, name) {
		return errors.New("start failed")
	}
	return nil
}

// ブラウザが見つかればアプリモードで開き、見つからないか起動できなければ既定のブラウザで開く。
func TestOpenWindow(t *testing.T) {
	const u = "http://127.0.0.1:8780/admin"
	tests := []struct {
		name  string
		goos  string
		found []string
		fail  []string
		want  [][]string
		err   bool
	}{
		{"linux app mode", "linux", []string{"chromium"}, nil,
			[][]string{{"/usr/bin/chromium", "--app=" + u, "--window-size=1280,860"}}, false},
		{"linux none found", "linux", nil, nil, [][]string{{"xdg-open", u}}, false},
		{"linux app mode fails", "linux", []string{"chromium"}, []string{"/usr/bin/chromium"}, [][]string{
			{"/usr/bin/chromium", "--app=" + u, "--window-size=1280,860"}, {"xdg-open", u},
		}, false},
		{"linux nothing starts", "linux", nil, []string{"xdg-open"}, [][]string{{"xdg-open", u}}, true},
		{"windows none found", "windows", nil, nil, [][]string{{"rundll32", "url.dll,FileProtocolHandler", u}}, false},
		{"windows edge", "windows", []string{`C:\Program Files\Microsoft\Edge\Application\msedge.exe`}, nil,
			[][]string{{`C:\Program Files\Microsoft\Edge\Application\msedge.exe`, "--app=" + u, "--window-size=1280,860"}}, false},
		{"darwin none found", "darwin", nil, nil, [][]string{{"open", u}}, false},
		{"darwin chrome", "darwin", []string{"/Applications/Google Chrome.app"}, nil, [][]string{
			{"open", "-na", "/Applications/Google Chrome.app", "--args", "--app=" + u, "--window-size=1280,860"},
		}, false},
	}
	for _, tt := range tests {
		r := &startRecorder{fail: tt.fail}
		err := openWindow(tt.goos, u, 1280, 860, fakeLookup(tt.found...), r.start)
		if (err != nil) != tt.err {
			t.Errorf("%s: err = %v, want error %v", tt.name, err, tt.err)
		}
		if !slices.EqualFunc(r.started, tt.want, slices.Equal) {
			t.Errorf("%s: started %q, want %q", tt.name, r.started, tt.want)
		}
	}
}

// SORUS_NO_BROWSER が 1 なら、OpenWindow はブラウザを探しも起動もしない。
func TestOpenWindowNoBrowserEnv(t *testing.T) {
	prevStart, prevLookup := startProcess, systemLookup
	t.Cleanup(func() { startProcess, systemLookup = prevStart, prevLookup })
	r := &startRecorder{}
	startProcess = r.start
	// どの OS で試験しても、最初の候補が見つかるようにする。
	systemLookup = fakeLookup("google-chrome", "/Applications/Google Chrome.app",
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`)

	t.Setenv(NoBrowserEnv, "1")
	if err := OpenWindow("http://127.0.0.1:8780/admin"); err != nil {
		t.Errorf("OpenWindow with %s=1: %v", NoBrowserEnv, err)
	}
	if len(r.started) != 0 {
		t.Errorf("started %q with %s=1", r.started, NoBrowserEnv)
	}

	t.Setenv(NoBrowserEnv, "")
	if err := OpenWindow("http://127.0.0.1:8780/admin"); err != nil {
		t.Errorf("OpenWindow: %v", err)
	}
	if len(r.started) != 1 || !slices.Contains(r.started[0], "--app=http://127.0.0.1:8780/admin") {
		t.Errorf("started %q, want a browser in app mode", r.started)
	}
}
