// install.sh を一時 HOME と、httptest で配信する疑似リリースに対して実行して確かめる。
// ネットワークには出ない。sh が無い環境（Windows など）では install.sh のテストを飛ばす。
//
// 確かめないこと:
//   - macOS の実機での動作（SORUS_FORCE_OS=Darwin で .app の組み立てだけを確かめる。
//     open による起動・codesign・Rosetta 判定は実機でしか確かめられない）。
//   - 起動（SORUS_NO_LAUNCH=1 で常に省略する）。
//   - install.ps1 の実行（pwsh があれば構文解析だけ行う。Windows 実機では未検証）。
package install_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fakeBinary は疑似リリースに入れる「sorus」。中身で版と OS・CPU を見分ける。
func fakeBinary(version, goos, goarch string) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\necho sorus %s %s-%s\n", version, goos, goarch))
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.SetMode(0o755)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 疑似リリースのアプリアイコン（scripts/release.sh が darwin の zip に sorus.icns、linux の tar.gz に sorus.png を入れる）。
// v1.0.0 はアイコンの入っていない古い版として、アイコンを入れない。
var (
	fakeICNS = []byte("icns\x00\x00\x00\x08")
	fakePNG  = []byte("\x89PNG\r\n\x1a\nfake-icon")
)

func releaseHasIcons(version string) bool { return version != "v1.0.0" }

// writeRelease は dir に scripts/release.sh と同じ名前のアーカイブと SHA256SUMS を作る。
// corrupt が true なら、SHA256SUMS を書いたあとでアーカイブの中身を差し替える。
func writeRelease(t *testing.T, dir string, versions []string, corrupt bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sums strings.Builder
	add := func(name string, data []byte) {
		sum := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		if corrupt {
			data = append(append([]byte{}, data...), "tampered"...)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range versions {
		// サーバー版は名前の末尾がデスクトップ版と同じなので、先に並べて誤選択しないことを確かめる。
		for _, arch := range []string{"amd64", "arm64"} {
			add(fmt.Sprintf("sorus-server-%s-linux-%s.tar.gz", v, arch), tarGz(t, map[string][]byte{"sorus": []byte("server")}))
		}
		for _, arch := range []string{"amd64", "arm64"} {
			linux := map[string][]byte{"sorus": fakeBinary(v, "linux", arch), "README.txt": []byte("readme")}
			darwin := map[string][]byte{"sorus": fakeBinary(v, "darwin", arch), "README.txt": []byte("readme")}
			if releaseHasIcons(v) {
				linux["sorus.png"] = fakePNG
				darwin["sorus.icns"] = fakeICNS
			}
			add(fmt.Sprintf("sorus-%s-linux-%s.tar.gz", v, arch), tarGz(t, linux))
			add(fmt.Sprintf("sorus-%s-darwin-%s.zip", v, arch), zipArchive(t, darwin))
			add(fmt.Sprintf("sorus-%s-windows-%s.zip", v, arch), zipArchive(t, map[string][]byte{
				"sorus.exe": []byte("MZ"), "README.txt": []byte("readme"),
			}))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeReleases は v1・v2・改ざん版・2つの版が混ざった（版の固定用）リリースを配信する。
func fakeReleases(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	writeRelease(t, filepath.Join(root, "v1"), []string{"v1.0.0"}, false)
	writeRelease(t, filepath.Join(root, "v2"), []string{"v2.0.0"}, false)
	writeRelease(t, filepath.Join(root, "bad"), []string{"v3.0.0"}, true)
	writeRelease(t, filepath.Join(root, "mixed"), []string{"v2.0.0", "v1.0.0"}, false)
	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	return srv
}

func requireSh(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh は macOS / Linux 用")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh が見つからない")
	}
	return sh
}

// hostArch は uname -m が amd64 / arm64 以外を返す環境でもテストできるよう、必要なら CPU を固定する。
func hostArch() (env []string, arch string) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return nil, runtime.GOARCH
	}
	return []string{"SORUS_FORCE_ARCH=x86_64"}, "amd64"
}

type result struct {
	err    error
	stdout string
	stderr string
}

// runInstall は `curl ... | sh` と同じく、install.sh を標準入力から sh に渡して実行する。
func runInstall(t *testing.T, sh, home, base string, extra ...string) result {
	t.Helper()
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-s")
	cmd.Stdin = bytes.NewReader(script)
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if key == "HOME" || strings.HasPrefix(key, "SORUS_") || strings.HasPrefix(key, "XDG_") || strings.HasSuffix(upper, "_PROXY") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "SORUS_RELEASE_BASE="+base, "SORUS_NO_LAUNCH=1")
	cmd.Env = append(env, extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return result{err: err, stdout: stdout.String(), stderr: stderr.String()}
}

func mustSucceed(t *testing.T, r result) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("install.sh failed: %v\nstdout:\n%s\nstderr:\n%s", r.err, r.stdout, r.stderr)
	}
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

// assertNoLeftovers は入れ替え用の一時ファイル（.sorus.*）が残っていないことを確かめる。
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sorus") {
			t.Errorf("leftover %s in %s", e.Name(), dir)
		}
	}
}

func TestInstallShLinux(t *testing.T) {
	sh := requireSh(t)
	srv := fakeReleases(t)
	home := t.TempDir()
	env, arch := hostArch()
	if runtime.GOOS != "linux" {
		env = append(env, "SORUS_FORCE_OS=Linux")
	}
	bin := filepath.Join(home, ".local", "bin", "sorus")
	desktop := filepath.Join(home, ".local", "share", "applications", "sorus.desktop")
	icon := filepath.Join(home, ".local", "share", "icons", "hicolor", "256x256", "apps", "sorus.png")

	r := runInstall(t, sh, home, srv.URL+"/v1", env...)
	mustSucceed(t, r)
	assertFile(t, bin, fakeBinary("v1.0.0", "linux", arch))
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable: %v", bin, info.Mode())
	}
	entry, err := os.ReadFile(desktop)
	if err != nil {
		t.Fatalf("read .desktop: %v", err)
	}
	for _, want := range []string{"[Desktop Entry]", "Name=sorus", "Type=Application", `Exec="` + bin + `"`} {
		if !strings.Contains(string(entry), want) {
			t.Errorf(".desktop missing %q:\n%s", want, entry)
		}
	}
	if !strings.Contains(r.stdout, "v1.0.0") {
		t.Errorf("progress output does not mention the version:\n%s", r.stdout)
	}
	// v1.0.0 のアーカイブにはアイコンが無い。Icon= を書かず、インストールは続ける。
	if strings.Contains(string(entry), "Icon=") {
		t.Errorf(".desktop has Icon= without an icon:\n%s", entry)
	}
	if _, err := os.Stat(icon); !os.IsNotExist(err) {
		t.Errorf("icon exists without an icon in the archive: %v", err)
	}

	t.Run("hash mismatch keeps previous install", func(t *testing.T) {
		r := runInstall(t, sh, home, srv.URL+"/bad", env...)
		if r.err == nil {
			t.Fatalf("install.sh succeeded with a tampered archive\nstdout:\n%s", r.stdout)
		}
		if !strings.Contains(r.stderr, "中止") {
			t.Errorf("stderr does not explain the abort:\n%s", r.stderr)
		}
		assertFile(t, bin, fakeBinary("v1.0.0", "linux", arch))
		assertNoLeftovers(t, filepath.Dir(bin))
	})

	t.Run("rerun updates in place", func(t *testing.T) {
		mustSucceed(t, runInstall(t, sh, home, srv.URL+"/v2/", env...))
		assertFile(t, bin, fakeBinary("v2.0.0", "linux", arch))
		assertNoLeftovers(t, filepath.Dir(bin))
		assertNoLeftovers(t, filepath.Dir(desktop))
		// v2.0.0 のアーカイブのアイコンを置き、.desktop の Icon= に絶対パスで書く。
		assertFile(t, icon, fakePNG)
		assertNoLeftovers(t, filepath.Dir(icon))
		entry, err := os.ReadFile(desktop)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(entry), "\nIcon="+icon+"\n") {
			t.Errorf(".desktop missing Icon=%s:\n%s", icon, entry)
		}
	})

	t.Run("SORUS_VERSION pins a version", func(t *testing.T) {
		mustSucceed(t, runInstall(t, sh, home, srv.URL+"/mixed", append(env, "SORUS_VERSION=v1.0.0")...))
		assertFile(t, bin, fakeBinary("v1.0.0", "linux", arch))
		// アイコンの無い古い版を入れ直しても、前に置いたアイコンを使い続ける。
		assertFile(t, icon, fakePNG)
		entry, err := os.ReadFile(desktop)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(entry), "\nIcon="+icon+"\n") {
			t.Errorf(".desktop lost Icon= after installing a release without an icon:\n%s", entry)
		}
	})

	t.Run("aarch64 maps to arm64", func(t *testing.T) {
		mustSucceed(t, runInstall(t, sh, home, srv.URL+"/v2", "SORUS_FORCE_OS=Linux", "SORUS_FORCE_ARCH=aarch64"))
		assertFile(t, bin, fakeBinary("v2.0.0", "linux", "arm64"))
	})

	t.Run("unsupported OS", func(t *testing.T) {
		r := runInstall(t, sh, t.TempDir(), srv.URL+"/v2", "SORUS_FORCE_OS=FreeBSD")
		if r.err == nil || !strings.Contains(r.stderr, "対応していません") {
			t.Fatalf("want unsupported-OS error, got err=%v stderr=%s", r.err, r.stderr)
		}
	})
}

// plistDict は Info.plist の <dict> を key -> 値（true/false は "true"/"false"）にする。
func plistDict(t *testing.T, data []byte) map[string]string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	out := map[string]string{}
	var key, elem string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			elem = tk.Name.Local
			if (elem == "true" || elem == "false") && key != "" {
				out[key] = elem
				key = ""
			}
		case xml.CharData:
			text := strings.TrimSpace(string(tk))
			if text == "" {
				continue
			}
			if elem == "key" {
				key = text
			} else if elem == "string" && key != "" {
				out[key] = text
				key = ""
			}
		}
	}
	return out
}

func TestInstallShMacAppAssembly(t *testing.T) {
	sh := requireSh(t)
	_, errUnzip := exec.LookPath("unzip")
	_, errDitto := exec.LookPath("ditto")
	if errUnzip != nil && errDitto != nil {
		t.Skip("unzip も ditto も無いため macOS 用の zip を展開できない")
	}
	srv := fakeReleases(t)
	home := t.TempDir()
	env := []string{"SORUS_FORCE_OS=Darwin", "SORUS_FORCE_ARCH=arm64"}
	apps := filepath.Join(home, "Applications")
	app := filepath.Join(apps, "sorus.app")
	exe := filepath.Join(app, "Contents", "MacOS", "sorus")

	mustSucceed(t, runInstall(t, sh, home, srv.URL+"/v1", env...))
	assertFile(t, exe, fakeBinary("v1.0.0", "darwin", "arm64"))
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable: %v", exe, info.Mode())
	}
	plist, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	got := plistDict(t, plist)
	for key, want := range map[string]string{
		"CFBundleIdentifier":         "jp.sorus.desktop",
		"CFBundleName":               "sorus",
		"CFBundleExecutable":         "sorus",
		"CFBundlePackageType":        "APPL",
		"CFBundleShortVersionString": "1.0.0",
		"LSUIElement":                "true",
	} {
		if got[key] != want {
			t.Errorf("Info.plist %s = %q, want %q\n%s", key, got[key], want, plist)
		}
	}

	t.Run("hash mismatch keeps previous app", func(t *testing.T) {
		r := runInstall(t, sh, home, srv.URL+"/bad", env...)
		if r.err == nil {
			t.Fatalf("install.sh succeeded with a tampered archive\nstdout:\n%s", r.stdout)
		}
		assertFile(t, exe, fakeBinary("v1.0.0", "darwin", "arm64"))
		assertNoLeftovers(t, apps)
	})

	// v1.0.0 のアーカイブにはアイコンが無い。CFBundleIconFile も Resources も作らない。
	if _, ok := got["CFBundleIconFile"]; ok {
		t.Errorf("Info.plist has CFBundleIconFile without an icon:\n%s", plist)
	}
	icns := filepath.Join(app, "Contents", "Resources", "sorus.icns")
	if _, err := os.Stat(icns); !os.IsNotExist(err) {
		t.Errorf("sorus.icns exists without an icon in the archive: %v", err)
	}
	checkIcon := func(t *testing.T) {
		t.Helper()
		assertFile(t, icns, fakeICNS)
		plist, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
		if err != nil {
			t.Fatal(err)
		}
		got := plistDict(t, plist)
		if got["CFBundleIconFile"] != "sorus.icns" || got["CFBundleExecutable"] != "sorus" || got["LSUIElement"] != "true" {
			t.Errorf("Info.plist = %v\n%s", got, plist)
		}
	}

	t.Run("rerun replaces the app", func(t *testing.T) {
		mustSucceed(t, runInstall(t, sh, home, srv.URL+"/v2", env...))
		assertFile(t, exe, fakeBinary("v2.0.0", "darwin", "arm64"))
		assertNoLeftovers(t, apps)
		checkIcon(t)
	})

	t.Run("release without an icon keeps the previous icon", func(t *testing.T) {
		mustSucceed(t, runInstall(t, sh, home, srv.URL+"/mixed", append(env, "SORUS_VERSION=v1.0.0")...))
		assertFile(t, exe, fakeBinary("v1.0.0", "darwin", "arm64"))
		assertNoLeftovers(t, apps)
		checkIcon(t)
	})
}

// install.sh は macOS の /bin/sh（bash の POSIX モード）と dash の両方で動く必要がある。
func TestInstallShSyntax(t *testing.T) {
	checked := 0
	for _, shell := range [][]string{{"dash", "-n"}, {"bash", "--posix", "-n"}, {"sh", "-n"}} {
		path, err := exec.LookPath(shell[0])
		if err != nil {
			continue
		}
		checked++
		out, err := exec.Command(path, append(shell[1:], "install.sh")...).CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", strings.Join(shell, " "), err, out)
		}
	}
	if checked == 0 {
		t.Skip("sh が見つからない")
	}
}

func TestInstallPs1(t *testing.T) {
	data, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	// irm | iex（ISO-8859-1 として読まれる）でも -File（ANSI コードページで読まれる）でも
	// 日本語が化けないよう、ファイルは ASCII のみで書き、日本語は \uXXXX で書く。
	for i, b := range data {
		if b >= 0x80 {
			line := bytes.Count(data[:i], []byte("\n")) + 1
			t.Fatalf("install.ps1 must be ASCII only: byte 0x%02x on line %d", b, line)
		}
	}
	src := string(data)
	for _, want := range []string{
		"Invoke-WebRequest -UseBasicParsing",
		"Get-FileHash -Algorithm SHA256",
		"SHA256SUMS",
		"$env:LOCALAPPDATA 'Programs\\sorus'",
		"WScript.Shell",
		"GetFolderPath('Programs')",
		"GetFolderPath('Desktop')",
		"$env:SORUS_NO_LAUNCH",
		"$env:SORUS_RELEASE_BASE",
		"$env:SORUS_VERSION",
		"PROCESSOR_ARCHITECTURE",
		"https://github.com/ryuzi/sorus/releases",
		`$lnk.IconLocation = "$exe,0"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("install.ps1 does not contain %q", want)
		}
	}
	// install.ps1 は実行して確かめられないため、SHA256SUMS から版を取り出す文字数が接頭辞「sorus-」の長さと
	// 合っていることを文字列で確かめる（ずれると版がずれる・デスクトップ版が見つからない）。
	const prefix = "sorus-"
	for _, want := range []string{
		"$name.StartsWith('" + prefix + "')",
		"$name.StartsWith('sorus-server-')",
		fmt.Sprintf("$name.Length -gt $suffix.Length + %d", len(prefix)),
		fmt.Sprintf("$archive.Substring(%d, $archive.Length - %d - $suffix.Length)", len(prefix), len(prefix)),
	} {
		if !strings.Contains(src, want) {
			t.Errorf("install.ps1 does not contain %q (prefix length must match %q)", want, prefix)
		}
	}
	// irm | iex で exit すると利用者の PowerShell ごと閉じるため、ファイル実行時だけに限る。
	for _, line := range strings.Split(src, "\n") {
		code, _, _ := strings.Cut(line, "#")
		if regexp.MustCompile(`\bexit\b`).MatchString(code) && !strings.Contains(code, "if ($PSCommandPath)") {
			t.Errorf("unguarded exit in install.ps1: %s", strings.TrimSpace(line))
		}
	}
	// J に渡す文字列は [regex]::Unescape で戻すので、\u 以外のバックスラッシュを含めない。
	for _, m := range regexp.MustCompile(`J '([^']*)'`).FindAllStringSubmatch(src, -1) {
		rest := regexp.MustCompile(`\\u[0-9a-fA-F]{4}`).ReplaceAllString(m[1], "")
		if strings.Contains(rest, `\`) {
			t.Errorf("J string has a backslash other than \\uXXXX: %s", m[1])
		}
	}

	t.Run("pwsh parse", func(t *testing.T) {
		pwsh, err := exec.LookPath("pwsh")
		if err != nil {
			t.Skip("pwsh が無いため install.ps1 の構文解析は未検証")
		}
		abs, err := filepath.Abs("install.ps1")
		if err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf(`$e = $null; $t = $null; [void][System.Management.Automation.Language.Parser]::ParseFile('%s', [ref]$t, [ref]$e); if ($e.Count -gt 0) { $e | ForEach-Object { $_.ToString() }; exit 1 }`, strings.ReplaceAll(abs, "'", "''"))
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
		if err != nil {
			t.Fatalf("install.ps1 parse errors: %v\n%s", err, out)
		}
	})
}

func TestReleaseNotesTemplate(t *testing.T) {
	data, err := os.ReadFile("RELEASE_NOTES_TEMPLATE.md")
	if err != nil {
		t.Fatal(err)
	}
	notes := string(data)
	for _, want := range []string{
		"irm https://github.com/ryuzi/sorus/releases/latest/download/install.ps1 | iex",
		"curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh",
		"{{VERSION}}",
		"予備",
		"詳細情報",
		"このまま開く",
		"SHA256SUMS",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("release notes template does not contain %q", want)
		}
	}
	if strings.Contains(notes, "attestation") {
		t.Error("release notes template still mentions attestation")
	}
}

func TestReleaseWorkflow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(data)
	if strings.Contains(raw, "attest-build-provenance") {
		t.Error("release.yml must not use attest-build-provenance")
	}
	// ソースと配布を ryuzi/sorus 1つで扱うので、別リポジトリ用の PAT（RELEASES_TOKEN）は使わない。
	if strings.Contains(raw, "RELEASES_TOKEN") {
		t.Error("release.yml must not require RELEASES_TOKEN (releases go to ryuzi/sorus itself with GITHUB_TOKEN)")
	}
	var wf struct {
		On struct {
			Push struct {
				Tags []string `yaml:"tags"`
			} `yaml:"push"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("release.yml does not parse: %v", err)
	}
	if len(wf.On.Push.Tags) != 1 || wf.On.Push.Tags[0] != "v*" {
		t.Errorf("trigger tags = %v, want [v*]", wf.On.Push.Tags)
	}
	// GITHUB_TOKEN で同じリポジトリにリリースを作るため、contents: write が要る。
	if wf.Permissions["contents"] != "write" {
		t.Errorf("permissions.contents = %q, want write", wf.Permissions["contents"])
	}
	for _, forbidden := range []string{"id-token", "attestations"} {
		if _, exists := wf.Permissions[forbidden]; exists {
			t.Errorf("permissions.%s must not be set", forbidden)
		}
	}
	job, ok := wf.Jobs["release"]
	if !ok {
		t.Fatal("job release not found")
	}
	var order []string
	for _, s := range job.Steps {
		switch {
		case strings.HasPrefix(s.Uses, "actions/setup-go@"):
			if s.With["go-version-file"] != "go.mod" {
				t.Errorf("setup-go go-version-file = %q, want go.mod", s.With["go-version-file"])
			}
			order = append(order, "setup-go")
		case strings.Contains(s.Run, "go test ./..."):
			order = append(order, "test")
		case strings.Contains(s.Run, "./scripts/release.sh"):
			order = append(order, "build")
		case strings.Contains(s.Run, "install/RELEASE_NOTES_TEMPLATE.md"):
			order = append(order, "notes")
		case strings.HasPrefix(s.Uses, "softprops/action-gh-release@v"):
			// repository を省略すると、ワークフローを動かしているリポジトリ（ryuzi/sorus）に出す。
			if repo := s.With["repository"]; repo != "" && repo != "ryuzi/sorus" {
				t.Errorf("gh-release repository = %q, want empty or ryuzi/sorus", repo)
			}
			if !strings.Contains(s.With["token"], "secrets.GITHUB_TOKEN") {
				t.Errorf("gh-release token = %q, want secrets.GITHUB_TOKEN", s.With["token"])
			}
			if !strings.Contains(s.With["tag_name"], "github.ref_name") {
				t.Errorf("gh-release tag_name = %q, want github.ref_name", s.With["tag_name"])
			}
			if !strings.Contains(s.With["files"], "dist-release/*") || !strings.Contains(s.With["body_path"], "release-notes.md") {
				t.Errorf("gh-release with = %v", s.With)
			}
			order = append(order, "upload")
		}
	}
	if got, want := strings.Join(order, ","), "setup-go,test,build,notes,upload"; got != want {
		t.Errorf("step order = %s, want %s", got, want)
	}
	if !strings.Contains(raw, "# - name: Sign Windows executables") || !strings.Contains(raw, "notarize") {
		t.Error("release.yml lacks the commented-out signing / notarization placeholder")
	}
}

// release.sh は install.sh・install.ps1・install-server.sh を dist-release/ に入れ、SHA256SUMS とリリースの添付に含める。
func TestReleaseScript(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "scripts", "release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		`sorus-$VERSION-$goos-$goarch`,
		`sorus-server-$VERSION-$goos-$goarch.tar.gz`,
		"-X main.buildMode=desktop",
		"-X sorus/internal/version.Version=$VERSION",
		"-H windowsgui",
		"-tags nodynamic",
		"-trimpath",
		"CGO_ENABLED=0",
		`install/install.sh" "$ROOT/install/install.ps1" "$ROOT/deploy/install-server.sh"`,
		"install.sh install.ps1 install-server.sh",
		"SHA256SUMS",
		"RELEASE_TARGETS",
		"https://github.com/ryuzi/sorus/releases/latest/download/install.ps1",
		"https://github.com/ryuzi/sorus/releases/latest/download/install.sh",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("release.sh does not contain %q", want)
		}
	}
	if regexp.MustCompile(`(?mi)^[^#]*\bupx\b`).MatchString(src) {
		t.Error("release.sh must not compress binaries with UPX")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		return
	}
	if out, err := exec.Command(bash, "-n", filepath.Join("..", "scripts", "release.sh")).CombinedOutput(); err != nil {
		t.Errorf("bash -n release.sh: %v\n%s", err, out)
	}

	// ARM 向けの gen2brain/webp（WASM から変換した巨大な libwebp.go）は最適化するとコンパイルに
	// 6GB 超を使い、GitHub の標準ランナーが止まる。ARM 向けのときだけ webp の最適化を止める。
	t.Run("webp optimization off only for arm64", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("release.sh execution test runs on unix")
		}
		fakeRoot := t.TempDir()
		for _, rel := range []string{
			filepath.Join("scripts", "release.sh"),
			filepath.Join("install", "install.sh"),
			filepath.Join("install", "install.ps1"),
			filepath.Join("deploy", "install-server.sh"),
		} {
			b, err := os.ReadFile(filepath.Join("..", rel))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(fakeRoot, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		binDir := t.TempDir()
		logPath := filepath.Join(t.TempDir(), "go-calls.log")
		goScript := "#!/bin/sh\necho \"$GOOS/$GOARCH $*\" >>\"$FAKE_GO_LOG\"\nout=''\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-o\" ]; then out=\"$2\"; shift 2; else shift; fi\ndone\nprintf '#!/bin/sh\\necho fake-sorus\\n' >\"$out\"\nchmod 755 \"$out\"\n"
		if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goScript), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, filepath.Join(fakeRoot, "scripts", "release.sh"), "v1.2.3")
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"RELEASE_TARGETS=linux/amd64 linux/arm64 darwin/arm64",
			"FAKE_GO_LOG="+logPath,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("release.sh failed: %v\n%s", err, out)
		}
		logData, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		const flag = "-gcflags=github.com/gen2brain/webp=-N"
		builds := 0
		for _, line := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
			if !strings.Contains(line, " build ") || !strings.Contains(line, "-tags nodynamic") {
				continue // zipper のビルドなど
			}
			builds++
			arm := strings.HasSuffix(strings.Fields(line)[0], "/arm64")
			if got := strings.Contains(line, flag); got != arm {
				t.Errorf("%s: has %s = %v, want %v", line, flag, got, arm)
			}
		}
		// デスクトップ版 3つと、サーバー版 2つ（linux/amd64・linux/arm64）。
		if builds != 5 {
			t.Errorf("sorus builds = %d, want 5:\n%s", builds, logData)
		}
	})

	// 疑似の go コマンドで release.sh を実際に走らせ、dist-release/ と SHA256SUMS の出力を確かめる。
	t.Run("outputs and SHA256SUMS", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("release.sh execution test runs on unix")
		}
		fakeRoot := t.TempDir()
		for _, rel := range []string{
			filepath.Join("scripts", "release.sh"),
			filepath.Join("install", "install.sh"),
			filepath.Join("install", "install.ps1"),
			filepath.Join("deploy", "install-server.sh"),
		} {
			b, err := os.ReadFile(filepath.Join("..", rel))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(fakeRoot, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		binDir := t.TempDir()
		fakeGo := filepath.Join(binDir, "go")
		goScript := "#!/bin/sh\nout=''\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-o\" ]; then out=\"$2\"; shift 2; else shift; fi\ndone\nprintf '#!/bin/sh\\necho fake-sorus\\n' >\"$out\"\nchmod 755 \"$out\"\n"
		if err := os.WriteFile(fakeGo, []byte(goScript), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, filepath.Join(fakeRoot, "scripts", "release.sh"), "v1.2.3")
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"RELEASE_TARGETS=linux/amd64",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("release.sh failed: %v\n%s", err, out)
		}
		distDir := filepath.Join(fakeRoot, "dist-release")
		sumsBytes, err := os.ReadFile(filepath.Join(distDir, "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		sumsText := string(sumsBytes)
		for _, wantFile := range []string{
			"sorus-v1.2.3-linux-amd64.tar.gz",
			"sorus-server-v1.2.3-linux-amd64.tar.gz",
			"install.sh",
			"install.ps1",
			"install-server.sh",
		} {
			if _, err := os.Stat(filepath.Join(distDir, wantFile)); err != nil {
				t.Errorf("dist-release missing %s: %v", wantFile, err)
			}
			if !strings.Contains(sumsText, "  "+wantFile+"\n") {
				t.Errorf("SHA256SUMS missing %s:\n%s", wantFile, sumsText)
			}
		}
	})
}

// release.sh はアプリアイコン（build/icons/）を組み込む: Windows はビルドの間だけルートに *_windows_<arch>.syso を
// 置いて sorus.exe にリンクさせ、終わったら消す（作業ツリーを汚さない）。macOS の zip には sorus.icns、Linux の
// tar.gz には sorus.png を入れる。資材が無ければ警告してアイコンなしで作る。
func TestReleaseScriptAppIcons(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release.sh execution test runs on unix")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	src, err := os.ReadFile(filepath.Join("..", "scripts", "release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`rsrc_windows_$1.syso`, `zz_release_icon_windows_$1.syso`, "trap cleanup EXIT", "sorus.icns", "sorus-256.png"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("release.sh does not contain %q", want)
		}
	}
	// コミットしてあるアイコンの資材（go run ./build/icons/gen で作る）。
	for _, name := range []string{"sorus.png", "sorus.ico", "sorus.icns", "sorus-256.png", "rsrc_windows_amd64.syso", "rsrc_windows_arm64.syso"} {
		if _, err := os.Stat(filepath.Join("..", "build", "icons", name)); err != nil {
			t.Errorf("build/icons/%s: %v", name, err)
		}
	}

	setup := func(t *testing.T, withIcons bool) (root, logPath string) {
		t.Helper()
		root = t.TempDir()
		rels := []string{
			filepath.Join("scripts", "release.sh"),
			filepath.Join("install", "install.sh"),
			filepath.Join("install", "install.ps1"),
			filepath.Join("deploy", "install-server.sh"),
		}
		if withIcons {
			for _, name := range []string{"sorus.icns", "sorus-256.png", "rsrc_windows_amd64.syso", "rsrc_windows_arm64.syso"} {
				rels = append(rels, filepath.Join("build", "icons", name))
			}
		}
		for _, rel := range rels {
			b, err := os.ReadFile(filepath.Join("..", rel))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		binDir := t.TempDir()
		logPath = filepath.Join(t.TempDir(), "go-calls.log")
		// 疑似の go: sorus のビルド（-tags nodynamic）では、ビルドしたときにカレント（main パッケージ）にある .syso を記録する。
		// zip を作る zipper のビルドは本物の go に任せる。
		realGo, err := exec.LookPath("go")
		if err != nil {
			t.Skip("go command not found (needed to build the zipper)")
		}
		goScript := "#!/bin/sh\ncase \"$*\" in *nodynamic*) ;; *) exec \"$REAL_GO\" \"$@\" ;; esac\nsyso=$(ls *.syso 2>/dev/null | tr '\\n' ' ')\necho \"$GOOS/$GOARCH syso=[$syso] $*\" >>\"$FAKE_GO_LOG\"\nout=''\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-o\" ]; then out=\"$2\"; shift 2; else shift; fi\ndone\nprintf '#!/bin/sh\\necho fake-sorus\\n' >\"$out\"\nchmod 755 \"$out\"\n"
		if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goScript), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, filepath.Join(root, "scripts", "release.sh"), "v1.2.3")
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"RELEASE_TARGETS=windows/amd64 windows/arm64 darwin/arm64 linux/amd64",
			"FAKE_GO_LOG="+logPath,
			"REAL_GO="+realGo,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("release.sh failed: %v\n%s", err, out)
		}
		if withIcons == strings.Contains(string(out), "warning:") {
			t.Errorf("release.sh warnings (with icons = %v):\n%s", withIcons, out)
		}
		return root, logPath
	}
	sysoLeft := func(t *testing.T, root string) {
		t.Helper()
		left, _ := filepath.Glob(filepath.Join(root, "*.syso"))
		if len(left) > 0 {
			t.Errorf("release.sh left %v in the work tree", left)
		}
	}
	builds := func(t *testing.T, logPath string) map[string]string {
		t.Helper()
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if !strings.Contains(line, " build ") || !strings.Contains(line, "-tags nodynamic") || !strings.Contains(line, "main.buildMode=desktop") {
				continue
			}
			target, rest, _ := strings.Cut(line, " ")
			syso, _, _ := strings.Cut(strings.TrimPrefix(rest, "syso=["), "]")
			out[target] = strings.TrimSpace(syso)
		}
		return out
	}
	zipNames := func(t *testing.T, path string) []string {
		t.Helper()
		zr, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		return names
	}
	tarNames := func(t *testing.T, path string) []string {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		var names []string
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			names = append(names, h.Name)
		}
		return names
	}
	contains := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}

	t.Run("with icons", func(t *testing.T) {
		root, logPath := setup(t, true)
		got := builds(t, logPath)
		for target, want := range map[string]string{
			"windows/amd64": "zz_release_icon_windows_amd64.syso",
			"windows/arm64": "zz_release_icon_windows_arm64.syso",
			"darwin/arm64":  "",
			"linux/amd64":   "",
		} {
			if s, ok := got[target]; !ok || s != want {
				t.Errorf("%s desktop build saw syso [%s] (built %v), want [%s]", target, s, ok, want)
			}
		}
		sysoLeft(t, root)
		dist := filepath.Join(root, "dist-release")
		if names := zipNames(t, filepath.Join(dist, "sorus-v1.2.3-darwin-arm64.zip")); !contains(names, "sorus.icns") || !contains(names, "sorus") {
			t.Errorf("darwin zip = %v, want sorus and sorus.icns", names)
		}
		if names := zipNames(t, filepath.Join(dist, "sorus-v1.2.3-windows-amd64.zip")); len(names) != 2 || !contains(names, "sorus.exe") {
			t.Errorf("windows zip = %v, want sorus.exe and README.txt only", names)
		}
		if names := tarNames(t, filepath.Join(dist, "sorus-v1.2.3-linux-amd64.tar.gz")); !contains(names, "sorus.png") || !contains(names, "sorus") {
			t.Errorf("linux tar.gz = %v, want sorus and sorus.png", names)
		}
		if names := tarNames(t, filepath.Join(dist, "sorus-server-v1.2.3-linux-amd64.tar.gz")); contains(names, "sorus.png") {
			t.Errorf("server tar.gz = %v, want no icon", names)
		}
	})

	t.Run("without icons", func(t *testing.T) {
		root, logPath := setup(t, false)
		for target, s := range builds(t, logPath) {
			if s != "" {
				t.Errorf("%s build saw syso [%s] without icon assets", target, s)
			}
		}
		sysoLeft(t, root)
		dist := filepath.Join(root, "dist-release")
		if names := zipNames(t, filepath.Join(dist, "sorus-v1.2.3-darwin-arm64.zip")); contains(names, "sorus.icns") {
			t.Errorf("darwin zip = %v", names)
		}
	})

	t.Run("build failure still removes the syso", func(t *testing.T) {
		root := t.TempDir()
		for _, rel := range []string{filepath.Join("scripts", "release.sh"), filepath.Join("build", "icons", "rsrc_windows_amd64.syso")} {
			b, err := os.ReadFile(filepath.Join("..", rel))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		binDir := t.TempDir()
		// zipper のビルドは通し、sorus のビルドで失敗する。
		goScript := "#!/bin/sh\ncase \"$*\" in *nodynamic*) exit 1 ;; esac\nout=''\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-o\" ]; then out=\"$2\"; shift 2; else shift; fi\ndone\nprintf '#!/bin/sh\\n' >\"$out\"\nchmod 755 \"$out\"\n"
		if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goScript), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, filepath.Join(root, "scripts", "release.sh"), "v1.2.3")
		cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "RELEASE_TARGETS=windows/amd64")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("release.sh succeeded with a failing build:\n%s", out)
		}
		sysoLeft(t, root)
	})
}

// legacyRepo は改名前の非公開ソースリポジトリ。いまはソースも配布も公開リポジトリ ryuzi/sorus 1つで扱うので、
// これを指す URL は利用者には開けない。このファイル自身が検査に引っかからないよう、文字列は分けて組み立てる。
var legacyRepo = "ryuzi/" + "aito"

// legacyReleaseRepos は以前の配布専用リポジトリ（旧名と、改名時に一時的に使った名前）。
// 配布も ryuzi/sorus に統一したので、どの形で書かれていても検査で報告する。文字列は分けて組み立てる。
var legacyReleaseRepos = []string{"ryuzi/" + "aito" + "-releases", "ryuzi/" + "sorus" + "-releases"}

// scanLegacyRepoURLs は、改名前の非公開ソースリポジトリの Releases / API / raw URL と、以前の配布専用リポジトリ
// （legacyReleaseRepos）への参照が、追跡しているファイル（配布物・コード・ドキュメント。.navigator と docs/specs は除く）
// に残っていないかを調べる。
// ryuzi/sorus の URL（https://github.com/ryuzi/sorus/releases など）は正しい配布先なので対象外。
func scanLegacyRepoURLs(root string) ([]string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git is unavailable (required to list tracked files): %w", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = absRoot
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(absRoot))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files -z in %s failed (not a git work tree?): %w: %s", root, err, strings.TrimSpace(stderr.String()))
	}

	forbidden := []string{
		"github.com/" + legacyRepo + "/releases",
		"api.github.com/repos/" + legacyRepo + "/",
		"raw.githubusercontent.com/" + legacyRepo + "/",
	}
	forbidden = append(forbidden, legacyReleaseRepos...)
	var findings []string
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		rel := filepath.ToSlash(string(entry))
		if rel == ".navigator" || strings.HasPrefix(rel, ".navigator/") ||
			rel == "docs/specs" || strings.HasPrefix(rel, "docs/specs/") {
			continue
		}
		path := filepath.Join(absRoot, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		content := string(data)
		for _, pat := range forbidden {
			if strings.Contains(content, pat) {
				findings = append(findings, fmt.Sprintf("%s contains forbidden URL %q", rel, pat))
			}
		}
	}
	return findings, nil
}

func TestNoLegacyRepoURLs(t *testing.T) {
	findings, err := scanLegacyRepoURLs("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}

	t.Run("ignores untracked files in ignored paths and reports tracked files", func(t *testing.T) {
		repo := t.TempDir()
		runGit := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
		writeFile := func(rel, content string) {
			t.Helper()
			full := filepath.Join(repo, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		runGit("init")
		forbiddenURL := "https://github.com/" + legacyRepo + "/releases/download/v1.0.0/app"

		writeFile(".gitignore", "dist/\n")
		// 正しい配布先（ryuzi/sorus）の URL は報告しない。
		writeFile("README.md", "https://github.com/ryuzi/sorus/releases/latest/download/install.sh\n")
		writeFile(".navigator/config.yaml", forbiddenURL+"\n")
		writeFile("docs/specs/spec.md", forbiddenURL+"\n")
		runGit("add", ".gitignore", "README.md", ".navigator/config.yaml", "docs/specs/spec.md")

		// Ignored untracked build output containing forbidden URL must be ignored.
		writeFile("dist/sorus.exe", "binary-with-"+forbiddenURL+"\n")

		got, err := scanLegacyRepoURLs(repo)
		if err != nil {
			t.Fatalf("scanLegacyRepoURLs failed: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no findings for untracked/excluded files, got %v", got)
		}

		// Tracked file containing forbidden URL must be reported.
		writeFile("install/install.sh", forbiddenURL+"\n")
		runGit("add", "install/install.sh")

		got, err = scanLegacyRepoURLs(repo)
		if err != nil {
			t.Fatalf("scanLegacyRepoURLs failed: %v", err)
		}
		if len(got) != 1 || !strings.Contains(got[0], "install/install.sh") {
			t.Fatalf("expected tracked file install/install.sh to be reported, got %v", got)
		}
	})

	t.Run("reports legacy release repositories", func(t *testing.T) {
		for _, legacy := range legacyReleaseRepos {
			t.Run(legacy, func(t *testing.T) {
				repo := t.TempDir()
				runGit := func(args ...string) {
					t.Helper()
					cmd := exec.Command("git", args...)
					cmd.Dir = repo
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
					}
				}
				writeFile := func(rel, content string) {
					t.Helper()
					full := filepath.Join(repo, filepath.FromSlash(rel))
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}

				runGit("init")
				writeFile("README.md", "https://github.com/ryuzi/sorus/releases/latest/download/install.sh\n")
				writeFile("internal/desktop/update.go", "https://api.github.com/repos/"+legacy+"/releases/latest\n")
				writeFile("docs/install_guide.md", "https://github.com/"+legacy+"/releases/latest/download/install.ps1\n")
				runGit("add", "README.md", "internal/desktop/update.go", "docs/install_guide.md")

				got, err := scanLegacyRepoURLs(repo)
				if err != nil {
					t.Fatalf("scanLegacyRepoURLs failed: %v", err)
				}
				if len(got) != 2 {
					t.Fatalf("expected 2 findings (update.go and install_guide.md), got %v", got)
				}
				for _, f := range got {
					if strings.Contains(f, "README.md") {
						t.Errorf("ryuzi/sorus URL must not be reported: %s", f)
					}
				}
			})
		}
	})

	t.Run("fails on non-git directory", func(t *testing.T) {
		nonGit := t.TempDir()
		if _, err := scanLegacyRepoURLs(nonGit); err == nil {
			t.Fatal("expected error for non-git directory, got nil")
		}
	})

	t.Run("fails when git is unavailable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := scanLegacyRepoURLs(".."); err == nil {
			t.Fatal("expected error when git is unavailable, got nil")
		}
	})
}

// TestInstallShRealRelease は scripts/release.sh で作った本物のアーカイブを install.sh で入れる。
// ビルドに時間とメモリがかかるため、SORUS_TEST_RELEASE_DIR に dist-release/ の場所を渡したときだけ行う。
//
//	RELEASE_TARGETS=linux/amd64 make release VERSION=v0.0.0-test
//	SORUS_TEST_RELEASE_DIR=$PWD/dist-release go test ./install -run RealRelease
func TestInstallShRealRelease(t *testing.T) {
	dir := os.Getenv("SORUS_TEST_RELEASE_DIR")
	if dir == "" {
		t.Skip("SORUS_TEST_RELEASE_DIR が未設定（本物のアーカイブでの確認は手動で行う）")
	}
	sh := requireSh(t)
	if runtime.GOOS != "linux" {
		t.Skip("本物のアーカイブでの確認は Linux のみ")
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	suffix := "-linux-" + runtime.GOARCH + ".tar.gz"
	var archive string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasPrefix(fields[1], "sorus-") && !strings.HasPrefix(fields[1], "sorus-server-") && strings.HasSuffix(fields[1], suffix) {
			archive = fields[1]
		}
	}
	if archive == "" {
		t.Fatalf("no desktop archive for linux/%s in %s", runtime.GOARCH, dir)
	}
	// 期待する中身は、アーカイブの中の sorus そのもの。
	f, err := os.Open(filepath.Join(dir, archive))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var want []byte
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == "sorus" {
			var buf bytes.Buffer
			if _, err := buf.ReadFrom(tr); err != nil {
				t.Fatal(err)
			}
			want = buf.Bytes()
		}
	}
	if want == nil {
		t.Fatalf("%s has no sorus", archive)
	}

	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	mustSucceed(t, runInstall(t, sh, home, srv.URL))
	assertFile(t, filepath.Join(home, ".local", "bin", "sorus"), want)
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "applications", "sorus.desktop")); err != nil {
		t.Fatal(err)
	}
}

// CI はスクリプトを ./scripts/release.sh のように直接実行するため、Git に実行権限付き（100755）で
// 登録されていなければならない。core.fileMode=false の環境（WSL の /mnt 下など）では権限が記録されない。
func TestShellScriptsAreExecutableInGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is unavailable (required to list tracked files): %v", err)
	}
	cmd := exec.Command("git", "ls-files", "-s", "-z", "--", "*.sh")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files -s: %v", err)
	}
	var checked int
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, path, _ := strings.Cut(string(entry), "\t")
		checked++
		if mode, _, _ := strings.Cut(meta, " "); mode != "100755" {
			t.Errorf("%s is %s in git; run: git update-index --chmod=+x %s", path, mode, path)
		}
	}
	if checked == 0 {
		t.Fatal("no tracked *.sh files found")
	}
}
