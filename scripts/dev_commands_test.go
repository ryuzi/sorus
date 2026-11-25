package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// 開発用の起動コマンド（scripts/dev.sh・Makefile）と利用者向け文書のコマンド例が、main.go 単体ではなく
// main パッケージ全体（go run .）を実行することを確かめる。go run main.go だと同じパッケージの
// main_desktop_window.go などが外れ、openDesktopWindow や windowWatcher が undefined になってコンパイルできない。

// fakeGoScript は PATH の先頭に置く偽の go。go env GOVERSION には版を返し、それ以外の呼び出しでは
// 引数（1行1個）・作業ディレクトリ・ENV_FILE を $FAKE_GO_LOG のディレクトリに記録する。
const fakeGoScript = `#!/bin/sh
if [ "$1" = env ]; then
  echo go1.25.0
  exit 0
fi
: > "$FAKE_GO_LOG/args"
for a in "$@"; do
  printf '%s\n' "$a" >> "$FAKE_GO_LOG/args"
done
pwd -P > "$FAKE_GO_LOG/pwd"
printf '%s\n' "${ENV_FILE-<unset>}" > "$FAKE_GO_LOG/env_file"
`

// devEnvFile は scripts/dev.sh が読み込む手元の開発用の設定（.gitignore で除外）。devEnvExample はその見本で、
// dev.env が無ければ dev.sh がこれを写して作る。
const (
	devEnvFile    = "deploy/environments/dev.env"
	devEnvExample = "deploy/environments/dev.env.example"
)

type goCall struct {
	args    []string
	dir     string
	envFile string
}

func requireShellTools(t *testing.T, tools ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("開発用のシェルスクリプトと Makefile は macOS / Linux 用")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
}

// devWorkspace は scripts/dev.sh・Makefile・開発用の設定の見本（deploy/environments/dev.env.example）だけを写した
// 一時のリポジトリを作る。実際の DB や uploads/、手元の dev.env には触れない。
func devWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"scripts", filepath.Join("deploy", "environments")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "dev.sh", filepath.Join(root, "scripts", "dev.sh"), 0o755)
	copyFile(t, filepath.Join("..", "Makefile"), filepath.Join(root, "Makefile"), 0o644)
	copyFile(t, filepath.Join("..", devEnvExample), filepath.Join(root, devEnvExample), 0o644)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatal(err)
	}
}

// runWithFakeGo は偽の go を PATH の先頭に置いて name args を dir で実行し、go の呼び出しを返す。
func runWithFakeGo(t *testing.T, dir, name string, args ...string) goCall {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGoScript), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ENV_FILE=") || strings.HasPrefix(kv, "PATH=") ||
			strings.HasPrefix(kv, "MAKEFLAGS=") || strings.HasPrefix(kv, "MFLAGS=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_GO_LOG="+logDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatalf("go was not run: %v", err)
		}
		return string(data)
	}
	return goCall{
		args:    strings.Split(strings.TrimSuffix(read("args"), "\n"), "\n"),
		dir:     strings.TrimSpace(read("pwd")),
		envFile: strings.TrimSpace(read("env_file")),
	}
}

// main パッケージは main.go 以外のファイルにも分かれているので、go run main.go では足りない。
func TestMainPackageSpansMultipleFiles(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{join .GoFiles \"\\n\"}}", "..").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	files := strings.Fields(string(out))
	if !slices.Contains(files, "main.go") || len(files) < 2 {
		t.Fatalf("main package files = %v, want main.go and at least one more", files)
	}
}

func TestDevScriptRunsMainPackage(t *testing.T) {
	requireShellTools(t, "sh")
	root := devWorkspace(t)
	// 別のディレクトリから呼んでも、リポジトリの直下で起動する。
	call := runWithFakeGo(t, t.TempDir(), "sh", filepath.Join(root, "scripts", "dev.sh"))
	if want := []string{"run", "."}; !slices.Equal(call.args, want) {
		t.Errorf("dev.sh ran go %q, want go %q", call.args, want)
	}
	if call.dir != root {
		t.Errorf("dev.sh ran go in %s, want %s", call.dir, root)
	}
	if call.envFile != devEnvFile {
		t.Errorf("dev.sh ENV_FILE = %q, want %q", call.envFile, devEnvFile)
	}
	for _, d := range []string{"uploads", "attachments"} {
		if fi, err := os.Stat(filepath.Join(root, d)); err != nil || !fi.IsDir() {
			t.Errorf("dev.sh did not create %s/: %v", d, err)
		}
	}
}

// dev.sh は deploy/environments/dev.env が無ければ dev.env.example を写して作り、それを読み込む（見本を直接読まない）。
// 一度作った後に書き換えた dev.env は、次に起動するときも上書きしない。
func TestDevScriptCreatesDevEnvFromExample(t *testing.T) {
	requireShellTools(t, "sh")
	root := devWorkspace(t)
	example, err := os.ReadFile(filepath.Join(root, devEnvExample))
	if err != nil {
		t.Fatal(err)
	}
	devEnv := filepath.Join(root, devEnvFile)
	if _, err := os.Stat(devEnv); !os.IsNotExist(err) {
		t.Fatalf("the workspace already has dev.env: %v", err)
	}

	if call := runWithFakeGo(t, root, "sh", filepath.Join(root, "scripts", "dev.sh")); call.envFile != devEnvFile {
		t.Errorf("first run: ENV_FILE = %q, want %q", call.envFile, devEnvFile)
	}
	if got, err := os.ReadFile(devEnv); err != nil || string(got) != string(example) {
		t.Fatalf("dev.env = %q, %v; want a copy of dev.env.example", got, err)
	}

	edited := "PORT=9999\n"
	if err := os.WriteFile(devEnv, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if call := runWithFakeGo(t, root, "sh", filepath.Join(root, "scripts", "dev.sh")); call.envFile != devEnvFile {
		t.Errorf("second run: ENV_FILE = %q, want %q", call.envFile, devEnvFile)
	}
	if got, _ := os.ReadFile(devEnv); string(got) != edited {
		t.Errorf("dev.env was overwritten: %q, want %q", got, edited)
	}
	if got, _ := os.ReadFile(filepath.Join(root, devEnvExample)); string(got) != string(example) {
		t.Errorf("dev.env.example was changed: %q", got)
	}
}

func TestMakefileTargetsRunMainPackage(t *testing.T) {
	requireShellTools(t, "sh", "make")
	tests := []struct {
		target  string
		args    []string
		envFile string
	}{
		{"dev", []string{"run", "."}, devEnvFile},
		{"export", []string{"run", ".", "export", "dist"}, "<unset>"},
		{"backup", []string{"run", ".", "backup"}, "<unset>"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			root := devWorkspace(t)
			call := runWithFakeGo(t, root, "make", "--no-print-directory", tt.target)
			if !slices.Equal(call.args, tt.args) {
				t.Errorf("make %s ran go %q, want go %q", tt.target, call.args, tt.args)
			}
			if call.dir != root {
				t.Errorf("make %s ran go in %s, want %s", tt.target, call.dir, root)
			}
			if call.envFile != tt.envFile {
				t.Errorf("make %s ENV_FILE = %q, want %q", tt.target, call.envFile, tt.envFile)
			}
		})
	}
}

// 利用者が読む現行の文書は、main パッケージ全体を実行するコマンド例（go run .）を載せる。
// development_spec.md は保護された仕様書、roadmap.md は当時の記録なので対象外。
func TestCurrentDocsDoNotRunSingleMainFile(t *testing.T) {
	docs, err := filepath.Glob(filepath.Join("..", "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	docs = append(docs, filepath.Join("..", "README.md"))
	historical := map[string]bool{"development_spec.md": true, "roadmap.md": true}
	bad := regexp.MustCompile(`go run main\.go`)
	checked := 0
	for _, path := range docs {
		if historical[filepath.Base(path)] {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d uses `go run main.go`; use `go run .` so the whole main package is built:\n%s", path, i+1, line)
			}
		}
	}
	if checked < 2 {
		t.Fatalf("checked %d documents, want README.md and docs/*.md", checked)
	}
}
