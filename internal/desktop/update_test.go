package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.4", "v1.2.3", true},
		{"v1.10.0", "v1.9.9", true},
		{"v2.0.0", "v1.99.99", true},
		{"1.2.4", "v1.2.3", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.2", "v1.2.3", false},
		{"v1.2.3", "v1.2.3-rc.1", true},
		{"v1.2.3-rc.2", "v1.2.3-rc.1", true},
		{"v1.2.3-rc.10", "v1.2.3-rc.9", true},
		{"v1.2.3-rc.1", "v1.2.3", false},
		{"v1.2.3-beta", "v1.2.3-1", true},
		{"v1.2.3+build.5", "v1.2.3", false},
		{"v1.2.4", "dev", false},
		{"latest", "v1.2.3", false},
		{"v1.2", "v1.2.3", false},
		{"v01.2.3", "v1.2.2", false},
		{"v1.2.3-", "v1.2.2", false},
	}
	for _, tt := range tests {
		if got := Newer(tt.latest, tt.current); got != tt.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestUpdateCheckEnabled(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if UpdateCheckEnabled("dev", env(nil)) {
		t.Error("enabled for dev")
	}
	if UpdateCheckEnabled("", env(nil)) {
		t.Error("enabled for empty version")
	}
	if UpdateCheckEnabled("v1.0.0", env(map[string]string{NoUpdateCheckEnv: "1"})) {
		t.Error("enabled with SORUS_NO_UPDATE_CHECK=1")
	}
	if !UpdateCheckEnabled("v1.0.0", env(nil)) {
		t.Error("disabled for a release")
	}
	if got := UpdateURL(env(nil)); got != DefaultUpdateURL {
		t.Errorf("UpdateURL = %q", got)
	}
	if got := UpdateURL(env(map[string]string{UpdateURLEnv: "http://127.0.0.1:1/latest"})); got != "http://127.0.0.1:1/latest" {
		t.Errorf("UpdateURL with env = %q", got)
	}
}

func TestLatestRelease(t *testing.T) {
	var gotUA string
	body := `{"tag_name":"v1.4.0","html_url":"https://github.com/ryuzi/sorus/releases/tag/v1.4.0","draft":false,"prerelease":false}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	rel, err := LatestRelease(context.Background(), srv.Client(), srv.URL, "v1.3.0")
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Version != "v1.4.0" || rel.PageURL == "" || gotUA != "sorus/v1.3.0" {
		t.Fatalf("release = %+v, UA %q", rel, gotUA)
	}
	if !Newer(rel.Version, "v1.3.0") || Newer(rel.Version, "v1.4.0") {
		t.Error("compare with the fetched release")
	}

	for name, b := range map[string]string{
		"prerelease": `{"tag_name":"v2.0.0-rc.1","prerelease":true}`,
		"not semver": `{"tag_name":"nightly"}`,
		"broken":     `{`,
	} {
		body = b
		if _, err := LatestRelease(context.Background(), srv.Client(), srv.URL, "v1.3.0"); err == nil {
			t.Errorf("%s: LatestRelease succeeded", name)
		}
	}
	body, status = `{"message":"rate limited"}`, http.StatusForbidden
	if _, err := LatestRelease(context.Background(), srv.Client(), srv.URL, "v1.3.0"); err == nil {
		t.Error("403: LatestRelease succeeded")
	}
}

func TestInstallCommandAndFolderCommand(t *testing.T) {
	if got := InstallCommand("windows"); got != "irm https://github.com/ryuzi/sorus/releases/latest/download/install.ps1 | iex" {
		t.Errorf("windows = %q", got)
	}
	for _, goos := range []string{"darwin", "linux"} {
		if got := InstallCommand(goos); got != "curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh" {
			t.Errorf("%s = %q", goos, got)
		}
	}
	for goos, want := range map[string][]string{
		"windows": {"explorer.exe", `C:\data`},
		"darwin":  {"open", `C:\data`},
		"linux":   {"xdg-open", `C:\data`},
	} {
		name, args := folderCommand(goos, `C:\data`)
		if got := append([]string{name}, args...); !slices.Equal(got, want) {
			t.Errorf("folderCommand(%s) = %v, want %v", goos, got, want)
		}
	}
	t.Setenv(NoBrowserEnv, "1")
	if err := OpenFolder(t.TempDir()); err != nil {
		t.Errorf("OpenFolder with SORUS_NO_BROWSER=1: %v", err)
	}
}
