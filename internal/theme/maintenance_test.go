package theme

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// テーマに同梱する maintenance.html（任意）の検証と読み込み。

const testMaintenanceHTML = `<!doctype html><html lang="ja"><head><meta charset="utf-8"><title>工事中</title></head><body><p>theme-maint</p></body></html>`

// 同梱した maintenance.html は検証して読み込み、BOM と前後の空白を除いた本文を返す。無ければ同梱していない扱い。
func TestValidateThemeDirReadsMaintenanceHTML(t *testing.T) {
	fsys := fixtureFS(t)
	if th, err := ValidateThemeDir(fsys, "themes/sample"); err != nil {
		t.Fatal(err)
	} else if html, ok := th.MaintenanceHTML(); ok || html != "" {
		t.Fatalf("theme without maintenance.html: MaintenanceHTML() = %q, %v", html, ok)
	}
	fsys["themes/sample/maintenance.html"] = &fstest.MapFile{Data: append(append([]byte{}, utf8BOM...), []byte("\n  "+testMaintenanceHTML+"\n")...)}
	th, err := ValidateThemeDir(fsys, "themes/sample")
	if err != nil {
		t.Fatalf("valid maintenance.html: %v", err)
	}
	html, ok := th.MaintenanceHTML()
	if !ok || html != testMaintenanceHTML {
		t.Fatalf("MaintenanceHTML() = %q, %v; want the trimmed body", html, ok)
	}
	// サブディレクトリの maintenance.html は同梱とみなさない（テーマのルート直下だけ）。
	delete(fsys, "themes/sample/maintenance.html")
	fsys["themes/sample/sub/maintenance.html"] = &fstest.MapFile{Data: []byte("\xff")}
	if th, err := ValidateThemeDir(fsys, "themes/sample"); err != nil {
		t.Fatalf("maintenance.html in a sub directory: %v", err)
	} else if _, ok := th.MaintenanceHTML(); ok {
		t.Fatal("maintenance.html in a sub directory was used")
	}
}

// 1 MiB を超える・UTF-8 でない・NUL を含む・空・HTML でない・ディレクトリの maintenance.html は、テーマごと検証を通さない。
func TestValidateThemeDirRejectsInvalidMaintenanceHTML(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *fstest.MapFile
		want error
	}{
		{"too large", &fstest.MapFile{Data: []byte("<p>" + strings.Repeat("a", MaxMaintenanceSize))}, ErrMaintenanceTooLarge},
		{"not utf-8", &fstest.MapFile{Data: []byte{'<', 'p', '>', 0xff, 0xfe}}, ErrMaintenanceNotUTF8},
		{"nul", &fstest.MapFile{Data: []byte("<html>\x00</html>")}, ErrMaintenanceBinary},
		{"empty", &fstest.MapFile{Data: []byte(" \n\t")}, ErrMaintenanceEmpty},
		{"bom only", &fstest.MapFile{Data: utf8BOM}, ErrMaintenanceEmpty},
		{"plain text", &fstest.MapFile{Data: []byte("ただいまメンテナンス中です")}, ErrMaintenanceNotHTML},
		{"directory", &fstest.MapFile{Mode: 0o755 | 1<<31}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fixtureFS(t)
			if tc.name == "directory" {
				fsys["themes/sample/maintenance.html/index.html"] = &fstest.MapFile{Data: []byte(testMaintenanceHTML)}
			} else {
				fsys["themes/sample/maintenance.html"] = tc.file
			}
			_, err := ValidateThemeDir(fsys, "themes/sample")
			if !errors.Is(err, ErrInvalidTheme) || !strings.Contains(err.Error(), MaintenanceFile) {
				t.Fatalf("err = %v, want ErrInvalidTheme mentioning %s", err, MaintenanceFile)
			}
			if tc.want != nil && !errors.Is(err, tc.want) && !strings.Contains(err.Error(), tc.want.Error()) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// ちょうど上限の大きさは受け付ける。
	fsys := fixtureFS(t)
	body := "<p>" + strings.Repeat("a", MaxMaintenanceSize-len("<p>"))
	fsys["themes/sample/maintenance.html"] = &fstest.MapFile{Data: []byte(body)}
	if _, err := ValidateThemeDir(fsys, "themes/sample"); err != nil {
		t.Fatalf("maintenance.html at the limit: %v", err)
	}
}

func TestNormalizeMaintenanceHTML(t *testing.T) {
	got, err := NormalizeMaintenanceHTML(append(append([]byte{}, utf8BOM...), []byte(" <p>x</p> ")...))
	if err != nil || got != "<p>x</p>" {
		t.Fatalf("NormalizeMaintenanceHTML = %q, %v", got, err)
	}
	for _, tc := range []struct {
		data []byte
		want error
	}{
		{nil, ErrMaintenanceEmpty},
		{[]byte{0xff}, ErrMaintenanceNotUTF8},
		{[]byte("a\x00"), ErrMaintenanceBinary},
		{[]byte(strings.Repeat("a", MaxMaintenanceSize+1)), ErrMaintenanceTooLarge},
	} {
		if _, err := NormalizeMaintenanceHTML(tc.data); !errors.Is(err, tc.want) {
			t.Errorf("NormalizeMaintenanceHTML(%.10q) err = %v, want %v", tc.data, err, tc.want)
		}
	}
}

// Zip から取り込んだテーマの maintenance.html はテーマのディレクトリに残り、カタログからも読める。不正なものは取り込まない。
func TestInstallFromZipKeepsMaintenanceHTML(t *testing.T) {
	dest := t.TempDir()
	files := append(validTheme("corp/"), zipFile{name: "corp/maintenance.html", body: testMaintenanceHTML})
	th, err := install(t, testZip(t, files...), dest)
	if err != nil {
		t.Fatal(err)
	}
	if html, ok := th.MaintenanceHTML(); !ok || html != testMaintenanceHTML {
		t.Fatalf("installed theme MaintenanceHTML() = %q, %v", html, ok)
	}
	if _, err := os.Stat(filepath.Join(dest, th.ID, MaintenanceFile)); err != nil {
		t.Fatalf("maintenance.html was not installed: %v", err)
	}
	c, err := LoadComposite(fixtureFS(t), dest)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get(th.ID)
	if !ok {
		t.Fatalf("custom theme %s not in the catalog (skipped %v)", th.ID, c.Skipped())
	}
	if html, ok := got.MaintenanceHTML(); !ok || html != testMaintenanceHTML {
		t.Fatalf("catalog MaintenanceHTML() = %q, %v", html, ok)
	}

	bad := t.TempDir()
	files = append(validTheme("broken/"), zipFile{name: "broken/maintenance.html", body: "\xff\xfe"})
	if _, err := install(t, testZip(t, files...), bad); !errors.Is(err, ErrInvalidTheme) {
		t.Fatalf("invalid maintenance.html: err = %v, want ErrInvalidTheme", err)
	}
	assertClean(t, bad)
}
