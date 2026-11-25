package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const installServerScript = "install-server.sh"

// install-server.sh に埋め込んだユニットは、deploy/systemd/ のファイルと1文字も違わない。
func TestInstallServerEmbedsSystemdUnits(t *testing.T) {
	data, err := os.ReadFile(installServerScript)
	if err != nil {
		t.Fatal(err)
	}
	embedded := regexp.MustCompile(`(?ms)^  install_unit (\S+) <<'__SORUS_UNIT__'\n(.*?)^__SORUS_UNIT__$`).FindAllStringSubmatch(string(data), -1)
	got := map[string]string{}
	for _, m := range embedded {
		got[m[1]] = m[2]
	}
	for _, name := range []string{"sorus.service", "sorus-backup.service", "sorus-backup.timer"} {
		want, err := os.ReadFile(filepath.Join("systemd", name))
		if err != nil {
			t.Fatal(err)
		}
		content, ok := got[name]
		if !ok {
			t.Errorf("%s does not embed %s", installServerScript, name)
			continue
		}
		if content != string(want) {
			t.Errorf("embedded %s differs from systemd/%s; copy the file into %s again", name, name, installServerScript)
		}
	}
	if len(got) != 3 {
		t.Errorf("embedded units = %d, want 3", len(got))
	}
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
}

func TestInstallServerScriptSyntax(t *testing.T) {
	requireTools(t, "bash")
	if out, err := exec.Command("bash", "-n", installServerScript).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	if _, err := exec.LookPath("shellcheck"); err == nil {
		if out, err := exec.Command("shellcheck", installServerScript).CombinedOutput(); err != nil {
			t.Errorf("shellcheck: %v\n%s", err, out)
		}
	} else {
		t.Log("shellcheck is not installed; only bash -n was run")
	}
}

// runInstallFunc は install-server.sh の関数を定義だけ読み込み（SORUS_INSTALL_SERVER_NO_MAIN）、snippet を実行する。
func runInstallFunc(t *testing.T, env []string, snippet string) (string, error) {
	t.Helper()
	script, err := filepath.Abs(installServerScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; `+snippet, "bash", script)
	cmd.Env = append(append(os.Environ(), "SORUS_INSTALL_SERVER_NO_MAIN=1"), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallServerArguments(t *testing.T) {
	requireTools(t, "bash")
	out, err := exec.Command("bash", installServerScript, "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--domain") || !strings.Contains(string(out), "--new-admin-invite") || !strings.Contains(string(out), "--archive") || !strings.Contains(string(out), "--sha256sums") {
		t.Errorf("--help: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "--domain でドメインを指定してください"},
		{[]string{"--domain", "media.example.jp"}, "--email で連絡先"},
		{[]string{"--domain", "https://media.example.jp/", "--email", "a@example.jp"}, "ドメインの形が正しくありません"},
		{[]string{"--domain", "media.example.jp", "--email", "not-an-email"}, "メールアドレスの形が正しくありません"},
		{[]string{"--domain", "media.example.jp", "--email", "a@example.jp", "--archive"}, "--archive の後にファイルの場所を書いてください"},
		{[]string{"--domain", "media.example.jp", "--email", "a@example.jp", "--sha256sums", "/tmp/sums"}, "--sha256sums は --archive と一緒に指定してください"},
		{[]string{"--domain", "media.example.jp", "--email", "a@example.jp", "--unknown"}, "知らないオプション"},
	} {
		out, err := exec.Command("bash", append([]string{installServerScript}, tc.args...)...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), tc.want) {
			t.Errorf("args %q: err=%v, want %q in\n%s", tc.args, err, tc.want, out)
		}
	}
	// 大文字や末尾の . は直して受け付ける。
	parsed, err := runInstallFunc(t, nil, `parse_args --domain Media.Example.JP. --email a@example.jp; echo "domain=$domain"`)
	if err != nil || !strings.Contains(parsed, "domain=media.example.jp") {
		t.Errorf("parse_args: %v\n%s", err, parsed)
	}
}

// writeRelease は sorus-server-<版>-linux-amd64.tar.gz（中身は sorus）と SHA256SUMS を dir に作る。
// corrupt なら SHA256SUMS に別の値を書く。
func writeRelease(t *testing.T, dir string, corrupt bool) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho sorus\n")
	for name, data := range map[string][]byte{"sorus": body, "README.txt": []byte("readme\n")} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
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
	const archive = "sorus-server-v9.9.9-linux-amd64.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, archive), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if corrupt {
		sum[0] ^= 0xff
	}
	// デスクトップ版の行は選ばない。
	sums := strings.Repeat("0", 64) + "  sorus-v9.9.9-linux-amd64.tar.gz\n" + hex.EncodeToString(sum[:]) + "  " + archive + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
}

// サーバー版を SHA256SUMS で確かめ、一致しなければ中止する（ダウンロード元は file:// の疑似リリース）。
func TestInstallServerDownloadVerifiesChecksum(t *testing.T) {
	requireTools(t, "bash", "curl", "tar", "sha256sum")
	snippet := `arch=amd64; download_release; trap - EXIT; cat "$tmp/x/sorus"; rm -rf "$tmp"`

	good := t.TempDir()
	writeRelease(t, good, false)
	out, err := runInstallFunc(t, []string{"SORUS_RELEASE_BASE=file://" + good}, snippet)
	if err != nil || !strings.Contains(out, "sorus-server-v9.9.9-linux-amd64.tar.gz") || !strings.Contains(out, "echo sorus") {
		t.Fatalf("download_release: %v\n%s", err, out)
	}

	bad := t.TempDir()
	writeRelease(t, bad, true)
	out, err = runInstallFunc(t, []string{"SORUS_RELEASE_BASE=file://" + bad + "/"}, snippet)
	if err == nil || !strings.Contains(out, "中止しました") || strings.Contains(out, "echo sorus") {
		t.Fatalf("download_release with a wrong checksum: err=%v\n%s", err, out)
	}

	out, err = runInstallFunc(t, []string{"SORUS_RELEASE_BASE=file://" + good}, `arch=arm64; download_release`)
	if err == nil || !strings.Contains(out, "arm64") {
		t.Fatalf("download_release for a missing arch: err=%v\n%s", err, out)
	}
}

// --archive はローカルのサーバー版アーカイブを SHA256SUMS で検証して BIN に置き、検証できなければ何も入れない。
func TestInstallServerLocalArchive(t *testing.T) {
	requireTools(t, "bash", "tar", "sha256sum")
	const archiveName = "sorus-server-v9.9.9-linux-amd64.tar.gz"

	runWithArchive := func(t *testing.T, arch, binPath string, extraArgs ...string) (string, error) {
		t.Helper()
		env := []string{
			"SORUS_RELEASE_BASE=http://127.0.0.1:1/never-called",
			"TEST_ARCH=" + arch,
			"TEST_BIN=" + binPath,
		}
		var quoted []string
		for _, a := range extraArgs {
			quoted = append(quoted, `"`+a+`"`)
		}
		snippet := `trap cleanup EXIT; parse_args --domain media.example.jp --email a@example.jp ` + strings.Join(quoted, " ") + `; arch="$TEST_ARCH"; BIN="$TEST_BIN"; download_release; install_binary`
		return runInstallFunc(t, env, snippet)
	}

	t.Run("matching hash installs", func(t *testing.T) {
		dir := t.TempDir()
		writeRelease(t, dir, false)
		bin := filepath.Join(t.TempDir(), "sorus")
		out, err := runWithArchive(t, "amd64", bin, "--archive", filepath.Join(dir, archiveName))
		if err != nil {
			t.Fatalf("matching archive failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(bin)
		if err != nil || !strings.Contains(string(got), "echo sorus") {
			t.Fatalf("installed binary = %q, err = %v", got, err)
		}
	})

	t.Run("mismatched hash fails and installs nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeRelease(t, dir, true)
		bin := filepath.Join(t.TempDir(), "sorus")
		out, err := runWithArchive(t, "amd64", bin, "--archive", filepath.Join(dir, archiveName))
		if err == nil || !strings.Contains(out, "中止しました") {
			t.Fatalf("mismatched hash: err=%v\n%s", err, out)
		}
		if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
			t.Fatalf("binary must not be installed on hash mismatch: stat err = %v", statErr)
		}
	})

	t.Run("missing SHA256SUMS fails", func(t *testing.T) {
		dir := t.TempDir()
		writeRelease(t, dir, false)
		if err := os.Remove(filepath.Join(dir, "SHA256SUMS")); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(t.TempDir(), "sorus")
		out, err := runWithArchive(t, "amd64", bin, "--archive", filepath.Join(dir, archiveName))
		if err == nil || !strings.Contains(out, "SHA256SUMS") {
			t.Fatalf("missing SHA256SUMS: err=%v\n%s", err, out)
		}
		if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
			t.Fatalf("binary must not be installed when SHA256SUMS is missing: stat err = %v", statErr)
		}
	})

	t.Run("file not listed fails", func(t *testing.T) {
		dir := t.TempDir()
		writeRelease(t, dir, false)
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("a", 64)+"  sorus-server-v0.0.1-linux-amd64.tar.gz\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(t.TempDir(), "sorus")
		out, err := runWithArchive(t, "amd64", bin, "--archive", filepath.Join(dir, archiveName))
		if err == nil || !strings.Contains(out, "SHA256SUMS に "+archiveName+" が載っていません") {
			t.Fatalf("unlisted file: err=%v\n%s", err, out)
		}
		if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
			t.Fatalf("binary must not be installed when file is unlisted: stat err = %v", statErr)
		}
	})

	t.Run("wrong arch fails", func(t *testing.T) {
		dir := t.TempDir()
		writeRelease(t, dir, false)
		bin := filepath.Join(t.TempDir(), "sorus")
		out, err := runWithArchive(t, "arm64", bin, "--archive", filepath.Join(dir, archiveName))
		if err == nil || !strings.Contains(out, "CPU（arm64）向けではありません") {
			t.Fatalf("wrong arch: err=%v\n%s", err, out)
		}
		if _, statErr := os.Stat(bin); !os.IsNotExist(statErr) {
			t.Fatalf("binary must not be installed on wrong arch: stat err = %v", statErr)
		}
	})

	t.Run("sha256sums flag overrides location", func(t *testing.T) {
		dir := t.TempDir()
		writeRelease(t, dir, false)
		sumsDir := t.TempDir()
		customSums := filepath.Join(sumsDir, "custom.sums")
		sumsData, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(customSums, sumsData, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "SHA256SUMS")); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(t.TempDir(), "sorus")
		out, err := runWithArchive(t, "amd64", bin, "--archive="+filepath.Join(dir, archiveName), "--sha256sums="+customSums)
		if err != nil {
			t.Fatalf("--sha256sums override failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(bin)
		if err != nil || !strings.Contains(string(got), "echo sorus") {
			t.Fatalf("installed binary = %q, err = %v", got, err)
		}
	})
}

// ドメインがこのサーバーを向いていなければ、直し方を説明して止まる（--skip-dns-check なら続ける）。
func TestInstallServerDNSCheck(t *testing.T) {
	requireTools(t, "bash", "getent")
	if out, err := exec.Command("getent", "ahosts", "localhost").Output(); err != nil || !strings.Contains(string(out), "127.0.0.1") {
		t.Skipf("localhost does not resolve to 127.0.0.1 here: %v", err)
	}
	out, err := runInstallFunc(t, []string{"SORUS_PUBLIC_IP=127.0.0.1"}, `domain=localhost; check_dns`)
	if err != nil || !strings.Contains(out, "OK: localhost は このサーバー") {
		t.Errorf("matching DNS: %v\n%s", err, out)
	}
	out, err = runInstallFunc(t, []string{"SORUS_PUBLIC_IP=203.0.113.9"}, `domain=localhost; check_dns`)
	if err == nil || !strings.Contains(out, "別のサーバー") || !strings.Contains(out, "203.0.113.9") || !strings.Contains(out, "A」のレコード") {
		t.Errorf("DNS pointing elsewhere: err=%v\n%s", err, out)
	}
	out, err = runInstallFunc(t, []string{"SORUS_PUBLIC_IP=203.0.113.9"}, `domain=localhost; skip_dns=1; check_dns; echo continued`)
	if err != nil || !strings.Contains(out, "continued") {
		t.Errorf("--skip-dns-check: %v\n%s", err, out)
	}
	out, err = runInstallFunc(t, []string{"SORUS_PUBLIC_IP=203.0.113.9"}, `domain=no-such-host.invalid; check_dns`)
	if err == nil || !strings.Contains(out, "まだ設定されていません") {
		t.Errorf("unresolved domain: err=%v\n%s", err, out)
	}
}

// 招待コードの貼り付け先は、sorus の「設定」→「公開先」の「公開サーバーに接続する」と案内する（server.InviteJoinHint と同じ言い方）。
// 「公開サーバー」は画面上部のメニューには無い。
func TestInstallServerInviteHint(t *testing.T) {
	data, err := os.ReadFile(installServerScript)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, "sorus の「設定」→「公開先」で「公開サーバーに接続する」を開き、招待コードを貼り付けてください") {
		t.Error("install-server.sh does not tell where to paste the invite code")
	}
	if strings.Contains(script, "画面上部") {
		t.Error("install-server.sh still places 公開サーバー in the header")
	}
}
