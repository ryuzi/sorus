package version_test

import (
	"os/exec"
	"strings"
	"testing"

	"sorus/internal/version"
)

func TestDefaultVersionIsDev(t *testing.T) {
	if version.Version != "dev" {
		t.Fatalf("Version = %q, want dev", version.Version)
	}
}

// scripts/release.sh と同じ -X の指定で版が埋め込まれることを確かめる。
func TestLdflagsStampsVersion(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go コマンドが見つからない")
	}
	const want = "v9.9.9-test"
	cmd := exec.Command(gobin, "run", "-ldflags", "-X sorus/internal/version.Version="+want, "./testdata/printversion")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("stamped version = %q, want %q", got, want)
	}
}
