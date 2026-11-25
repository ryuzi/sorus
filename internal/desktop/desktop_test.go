package desktop

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSelectMode(t *testing.T) {
	noEnv := func(string) string { return "" }
	envWith := func(key string) func(string) string {
		return func(k string) string {
			if k == key {
				return "x"
			}
			return ""
		}
	}
	tests := []struct {
		name        string
		args        []string
		buildMode   string
		getenv      func(string) string
		wantDesktop bool
		wantArgs    []string
		wantReason  bool
	}{
		{name: "default is server", args: nil, getenv: noEnv},
		{name: "desktop flag", args: []string{"--desktop"}, getenv: noEnv, wantDesktop: true},
		{name: "single dash flag", args: []string{"-desktop"}, getenv: noEnv, wantDesktop: true},
		{name: "build default", buildMode: BuildModeDesktop, getenv: noEnv, wantDesktop: true},
		{name: "other build mode", buildMode: "server", getenv: noEnv},
		{name: "server flag beats build default", args: []string{"--server"}, buildMode: BuildModeDesktop, getenv: noEnv},
		{name: "server flag beats desktop flag", args: []string{"--desktop", "--server"}, getenv: noEnv},
		{name: "public-only beats build default", args: []string{"--public-only"}, buildMode: BuildModeDesktop, getenv: noEnv,
			wantArgs: []string{"--public-only"}, wantReason: true},
		{name: "PUBLIC_ONLY wins", args: []string{"--desktop"}, getenv: envWith("PUBLIC_ONLY"), wantReason: true},
		{name: "ENV_FILE wins", buildMode: BuildModeDesktop, getenv: envWith("ENV_FILE"), wantReason: true},
		{name: "DB_PATH wins", args: []string{"--desktop"}, getenv: envWith("DB_PATH"), wantReason: true},
		{name: "unrelated env", args: []string{"--desktop"}, getenv: envWith("PORT"), wantDesktop: true},
		{name: "env alone does not log", getenv: envWith("DB_PATH")},
		{name: "flag after subcommand", args: []string{"backup", "--desktop", "out.zip"}, getenv: noEnv,
			wantDesktop: true, wantArgs: []string{"backup", "out.zip"}},
		{name: "flag before subcommand", args: []string{"--desktop", "export", "dist"}, getenv: noEnv,
			wantDesktop: true, wantArgs: []string{"export", "dist"}},
		{name: "server flag is removed", args: []string{"--server", "--sync-path", "/api/sync-x"}, buildMode: BuildModeDesktop, getenv: noEnv,
			wantArgs: []string{"--sync-path", "/api/sync-x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := SelectMode(tt.args, tt.buildMode, tt.getenv)
			if m.Desktop != tt.wantDesktop {
				t.Errorf("Desktop = %v, want %v", m.Desktop, tt.wantDesktop)
			}
			if !slices.Equal(m.Args, tt.wantArgs) {
				t.Errorf("Args = %q, want %q", m.Args, tt.wantArgs)
			}
			if (m.Reason != "") != tt.wantReason {
				t.Errorf("Reason = %q, want reason: %v", m.Reason, tt.wantReason)
			}
		})
	}
}

func TestDataDirFor(t *testing.T) {
	home := func() (string, error) { return filepath.Join("/", "home", "taro"), nil }
	noHome := func() (string, error) { return "", errors.New("no home") }
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	override := t.TempDir()

	tests := []struct {
		name    string
		goos    string
		env     map[string]string
		home    func() (string, error)
		want    string
		wantErr bool
	}{
		{name: "override", goos: "linux", env: map[string]string{DataDirEnv: override}, home: noHome, want: override},
		{name: "windows", goos: "windows", env: map[string]string{"LOCALAPPDATA": filepath.Join("C:", "Users", "taro", "AppData", "Local")}, home: noHome,
			want: filepath.Join("C:", "Users", "taro", "AppData", "Local", "sorus")},
		{name: "windows without LOCALAPPDATA", goos: "windows", home: home, want: filepath.Join("/", "home", "taro", "AppData", "Local", "sorus")},
		{name: "darwin", goos: "darwin", env: map[string]string{"XDG_DATA_HOME": "/xdg"}, home: home,
			want: filepath.Join("/", "home", "taro", "Library", "Application Support", "sorus")},
		{name: "linux xdg", goos: "linux", env: map[string]string{"XDG_DATA_HOME": "/xdg"}, home: noHome, want: filepath.Join("/xdg", "sorus")},
		{name: "linux relative xdg is ignored", goos: "linux", env: map[string]string{"XDG_DATA_HOME": "rel"}, home: home,
			want: filepath.Join("/", "home", "taro", ".local", "share", "sorus")},
		{name: "linux default", goos: "linux", home: home, want: filepath.Join("/", "home", "taro", ".local", "share", "sorus")},
		{name: "linux without home", goos: "linux", home: noHome, wantErr: true},
		{name: "darwin without home", goos: "darwin", home: noHome, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dataDirFor(tt.goos, env(tt.env), tt.home)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("dataDirFor = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPathsIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DataDirEnv, dir)
	p, err := DefaultAppPaths()
	if err != nil {
		t.Fatal(err)
	}
	if p != AppPathsIn(dir) {
		t.Errorf("DefaultAppPaths = %+v, want AppPathsIn(%q)", p, dir)
	}
	site := SitePathsIn(dir)
	for got, want := range map[string]string{
		site.DB: "database.db", site.Uploads: "uploads", site.Attachments: "attachments", site.SearchIndex: "search_index",
		p.Log: "sorus.log", p.Instance: "instance.json",
	} {
		if got != filepath.Join(dir, want) {
			t.Errorf("path %q, want %q in %q", got, want, dir)
		}
	}
	sub := AppPathsIn(filepath.Join(dir, "new", "sorus"))
	if err := sub.Ensure(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(sub.Dir); err != nil || !st.IsDir() {
		t.Errorf("Ensure did not create %s: %v", sub.Dir, err)
	}
}

func TestHostGuard(t *testing.T) {
	const port = 8780
	h := HostGuard(port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	tests := []struct {
		host string
		want int
	}{
		{"127.0.0.1:8780", http.StatusOK},
		{"localhost:8780", http.StatusOK},
		{"LOCALHOST:8780", http.StatusOK},
		{"evil.example", http.StatusForbidden},
		{"evil.example:8780", http.StatusForbidden},
		{"127.0.0.1", http.StatusForbidden},
		{"127.0.0.1:8781", http.StatusForbidden},
		{"localhost", http.StatusForbidden},
		{"[::1]:8780", http.StatusForbidden},
		{"", http.StatusForbidden},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		req.Host = tt.host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("Host %q: status %d, want %d", tt.host, rec.Code, tt.want)
		}
	}
}

// healthServer は sorus の /healthz に似た応答を返すテスト用のサーバーを 127.0.0.1 で起動し、そのポートを返す。
func healthServer(t *testing.T, code int, body string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// FindRunning は Host を 127.0.0.1:<port> にして確かめる（HostGuard を通る）。
		if r.URL.Path != healthPath || !strings.HasPrefix(r.Host, ListenHost+":") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return portOf(t, srv.Listener.Addr())
}

func portOf(t *testing.T, addr net.Addr) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestFindRunning(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.json")

	if _, ok := FindRunning(ctx, path); ok {
		t.Error("found an instance without instance.json")
	}

	running := healthServer(t, http.StatusOK, `{"status":"healthy"}`)
	if err := WriteInstance(path, Instance{PID: 123, Port: running}); err != nil {
		t.Fatal(err)
	}
	inst, ok := FindRunning(ctx, path)
	if !ok || inst.Port != running || inst.PID != 123 {
		t.Fatalf("FindRunning = %+v, %v; want port %d", inst, ok, running)
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(running) + "/admin"; inst.AdminURL() != want {
		t.Errorf("AdminURL = %q, want %q", inst.AdminURL(), want)
	}

	// データベースに接続できない（503 unhealthy）インスタンスも起動中として扱う。
	unhealthy := healthServer(t, http.StatusServiceUnavailable, `{"status":"unhealthy"}`)
	if err := WriteInstance(path, Instance{PID: 1, Port: unhealthy}); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindRunning(ctx, path); !ok {
		t.Error("unhealthy instance was not found")
	}

	// sorus ではないサーバーが同じポートを使っていれば起動中とみなさない。
	other := healthServer(t, http.StatusOK, `<html>hello</html>`)
	if err := WriteInstance(path, Instance{PID: 1, Port: other}); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindRunning(ctx, path); ok {
		t.Error("a non-sorus server was treated as a running instance")
	}

	// 前回の異常終了で残った instance.json（誰も待ち受けていないポート）。
	closed := httptest.NewServer(http.NotFoundHandler())
	closedPort := portOf(t, closed.Listener.Addr())
	closed.Close()
	if err := WriteInstance(path, Instance{PID: 1, Port: closedPort}); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindRunning(ctx, path); ok {
		t.Error("stale instance.json was treated as running")
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindRunning(ctx, path); ok {
		t.Error("broken instance.json was treated as running")
	}

	if err := RemoveInstance(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveInstance(path); err != nil {
		t.Errorf("RemoveInstance of a missing file: %v", err)
	}
}

func TestWaitRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.json")
	port := healthServer(t, http.StatusOK, `{"status":"healthy"}`)
	go func() {
		time.Sleep(300 * time.Millisecond)
		WriteInstance(path, Instance{PID: 1, Port: port})
	}()
	if inst, ok := WaitRunning(context.Background(), path, 5*time.Second); !ok || inst.Port != port {
		t.Fatalf("WaitRunning = %+v, %v", inst, ok)
	}
	if _, ok := WaitRunning(context.Background(), filepath.Join(t.TempDir(), "none.json"), 300*time.Millisecond); ok {
		t.Error("WaitRunning found an instance without instance.json")
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock: err = %v, want ErrLocked", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock2, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	unlock2()
}

func TestListenPrefersPort(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 優先ポートを使えたとき・使えなかったときのどちらも 127.0.0.1 で待ち受ける。2つ目は必ず別の空きポートになる。
	ln2, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	for _, l := range []net.Listener{ln, ln2} {
		if host, _, _ := net.SplitHostPort(l.Addr().String()); host != ListenHost {
			t.Errorf("listening on %s, want host %s", l.Addr(), ListenHost)
		}
	}
	if ln.Addr().String() == ln2.Addr().String() {
		t.Error("two listeners on the same address")
	}
}

func TestBrowserCommand(t *testing.T) {
	const u = "http://127.0.0.1:8780/admin?a=1&b=2"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", u}},
		{"darwin", "open", []string{u}},
		{"linux", "xdg-open", []string{u}},
	}
	for _, tt := range tests {
		name, args := browserCommand(tt.goos, u)
		if name != tt.name || !slices.Equal(args, tt.args) {
			t.Errorf("%s: %s %q, want %s %q", tt.goos, name, args, tt.name, tt.args)
		}
	}
	t.Setenv(NoBrowserEnv, "1")
	t.Setenv("PATH", "")
	if err := OpenBrowser(u); err != nil {
		t.Errorf("OpenBrowser with %s=1: %v", NoBrowserEnv, err)
	}
}

func TestOpenLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sorus.log")
	big := make([]byte, maxLogSize+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("new\n")
	f.Close()
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != int64(len(big)) {
		t.Errorf("old log was not kept as .1: %v", err)
	}
	f, err = OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("more\n")
	f.Close()
	if b, _ := os.ReadFile(path); string(b) != "new\nmore\n" {
		t.Errorf("log = %q, want appended lines", b)
	}
}
