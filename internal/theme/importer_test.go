package theme

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sorus/static"
)

// zipFile は testZip に入れるエントリ。mode が 0 なら通常のファイル。
type zipFile struct {
	name string
	body string
	mode fs.FileMode
}

// testZip は files を順に入れた Zip を作る。
func testZip(t *testing.T, files ...zipFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		if f.mode != 0 {
			h.SetMode(f.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const validCSS = ":root { --bg: #fff; }\n.site-header { background: url(\"img/bg.png\"); }\n"

func validJSON(nameEN string) string {
	return fmt.Sprintf(`{"name": {"ja": "カスタム", "en": %q}, "description": {"ja": "説明"}, "color_scheme": "light dark", "order": 100}`, nameEN)
}

// validTheme は prefix（"" または "dir/"）の下に正しいテーマを置くエントリを返す。
func validTheme(prefix string) []zipFile {
	return []zipFile{
		{name: prefix + "theme.json", body: validJSON("Corporate Custom")},
		{name: prefix + "theme.css", body: validCSS},
		{name: prefix + "img/bg.png", body: "png"},
	}
}

func install(t *testing.T, data []byte, dest string) (*Theme, error) {
	t.Helper()
	return InstallFromZip(bytes.NewReader(data), int64(len(data)), dest)
}

// assertClean は dest に作業ディレクトリやテーマが残っていないことを確かめる（ロールバックの確認）。
func assertClean(t *testing.T, dest string) {
	t.Helper()
	entries, err := os.ReadDir(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s is not clean after a failed install: %v", dest, names)
	}
}

func TestInstallFromZipAndUninstall(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "themes")
	th, err := install(t, testZip(t, append(validTheme("corporate/"), zipFile{name: "corporate/", mode: fs.ModeDir | 0o755})...), dest)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if th.ID != "corporate" || !th.IsCustom || th.Dir != filepath.Join(dest, "corporate") || th.ColorScheme != "light dark" || th.Order != 100 {
		t.Errorf("installed theme = %+v", th)
	}
	if got := th.Stylesheet(); got != "custom-themes/corporate/theme.css" {
		t.Errorf("Stylesheet() = %q", got)
	}
	for _, f := range []string{"theme.json", "theme.css", "img/bg.png"} {
		if _, err := os.Stat(filepath.Join(dest, "corporate", f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	// 作業ディレクトリは残らない。
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Errorf("dest has %d entries, want only the theme", len(entries))
	}

	// 合成カタログに入る。
	c, err := LoadComposite(os.DirFS("testdata"), dest)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Get("corporate"); !ok || !got.IsCustom {
		t.Errorf("catalog does not contain the installed theme: %+v %v", got, ok)
	}

	if err := UninstallCustomTheme(dest, "corporate"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	assertClean(t, dest)
	if err := UninstallCustomTheme(dest, "corporate"); !errors.Is(err, ErrThemeNotFound) {
		t.Errorf("second uninstall = %v, want ErrThemeNotFound", err)
	}
}

func TestUninstallCustomThemeToleratesTrashRemoveFailure(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "themes")
	th, err := install(t, testZip(t, validTheme("locked/")...), dest)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	sub := filepath.Join(th.Dir, "locked_sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.Walk(dest, func(p string, info fs.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}

	err = UninstallCustomTheme(dest, "locked")
	if err != nil {
		t.Fatalf("UninstallCustomTheme returned unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "locked")); !os.IsNotExist(err) {
		t.Errorf("theme directory still exists: %v", err)
	}
}

func TestInstallFromZipRootAbsorption(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []zipFile
		want  string
	}{
		// GitHub の Zipball は repo-main/ で包まれている。-main は ID から除く。
		{"github zipball", append(validTheme("sorus-theme-corporate-main/"), zipFile{name: "__MACOSX/._theme.css", body: "x"}), "sorus-theme-corporate"},
		{"master branch", validTheme("Fancy.Theme-master/"), "fancy-theme"},
		// 包むディレクトリが無ければ name.en から作る。
		{"flat archive", append(validTheme(""), zipFile{name: ".DS_Store", body: "x"}), "corporate-custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			th, err := install(t, testZip(t, tc.files...), dest)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if th.ID != tc.want {
				t.Errorf("ID = %q, want %q", th.ID, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dest, tc.want, "img", "bg.png")); err != nil {
				t.Errorf("asset not at the theme root: %v", err)
			}
			for _, junk := range []string{"__MACOSX", ".DS_Store"} {
				if _, err := os.Stat(filepath.Join(dest, tc.want, junk)); err == nil {
					t.Errorf("%s was extracted", junk)
				}
			}
		})
	}
}

func TestInstallFromZipReplacesExistingTheme(t *testing.T) {
	dest := t.TempDir()
	if _, err := install(t, testZip(t, append(validTheme("corp/"), zipFile{name: "corp/old.txt", body: "old"})...), dest); err != nil {
		t.Fatal(err)
	}
	files := validTheme("corp/")
	files[0].body = validJSON("Updated")
	th, err := install(t, testZip(t, files...), dest)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if th.Label("en") != "Updated" {
		t.Errorf("Label(en) = %q, want the updated name", th.Label("en"))
	}
	if _, err := os.Stat(filepath.Join(dest, "corp", "old.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old file survived the update: %v", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Errorf("dest has %d entries, want only the theme", len(entries))
	}
}

func TestInstallFromZipRejectsZipSlip(t *testing.T) {
	for _, name := range []string{
		"../evil.txt",
		"corp/../../evil.txt",
		"/etc/evil.txt",
		"..\\evil.txt",
		"C:/evil.txt",
		"corp/..",
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "themes")
			_, err := install(t, testZip(t, append(validTheme("corp/"), zipFile{name: name, body: "pwned"})...), dest)
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("install = %v, want ErrUnsafeArchive", err)
			}
			if _, err := os.Stat(filepath.Join(parent, "evil.txt")); err == nil {
				t.Error("evil.txt was written outside the theme directory")
			}
			assertClean(t, dest)
		})
	}
}

func TestInstallFromZipRejectsSymlinks(t *testing.T) {
	dest := t.TempDir()
	_, err := install(t, testZip(t, append(validTheme("corp/"), zipFile{name: "corp/link", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777})...), dest)
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("install = %v, want ErrUnsafeArchive", err)
	}
	assertClean(t, dest)
}

func TestInstallFromZipRejectsBombs(t *testing.T) {
	t.Run("too many files", func(t *testing.T) {
		files := validTheme("corp/")
		for i := 0; len(files) <= MaxArchiveFiles; i++ {
			files = append(files, zipFile{name: fmt.Sprintf("corp/f%d.txt", i), body: "x"})
		}
		dest := t.TempDir()
		if _, err := install(t, testZip(t, files...), dest); !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("install = %v, want ErrArchiveTooLarge", err)
		}
		assertClean(t, dest)
	})
	t.Run("exactly the file limit", func(t *testing.T) {
		files := validTheme("corp/")
		for i := 0; len(files) < MaxArchiveFiles; i++ {
			files = append(files, zipFile{name: fmt.Sprintf("corp/f%d.txt", i), body: "x"})
		}
		if _, err := install(t, testZip(t, files...), t.TempDir()); err != nil {
			t.Fatalf("install of %d files: %v", MaxArchiveFiles, err)
		}
	})
	t.Run("extracted size", func(t *testing.T) {
		big := strings.Repeat("0", MaxExtractedSize+1)
		dest := t.TempDir()
		data := testZip(t, append(validTheme("corp/"), zipFile{name: "corp/big.bin", body: big})...)
		if len(data) > MaxArchiveSize {
			t.Fatalf("test archive is %d bytes; it must compress below the archive limit", len(data))
		}
		if _, err := install(t, data, dest); !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("install = %v, want ErrArchiveTooLarge", err)
		}
		assertClean(t, dest)
	})
	t.Run("forged uncompressed size", func(t *testing.T) {
		// ヘッダーでは 10 バイトと偽り、実際には上限を超えて展開されるエントリ。
		var comp bytes.Buffer
		fw, _ := flate.NewWriter(&comp, flate.BestCompression)
		zero := make([]byte, 1<<20)
		for i := 0; i <= MaxExtractedSize>>20; i++ {
			fw.Write(zero)
		}
		fw.Close()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, f := range validTheme("corp/") {
			w, _ := zw.Create(f.name)
			w.Write([]byte(f.body))
		}
		w, err := zw.CreateRaw(&zip.FileHeader{Name: "corp/bomb.bin", Method: zip.Deflate,
			CompressedSize64: uint64(comp.Len()), UncompressedSize64: 10})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(comp.Bytes())
		zw.Close()
		dest := t.TempDir()
		if _, err := install(t, buf.Bytes(), dest); err == nil {
			t.Fatal("install of a forged-size entry succeeded")
		}
		assertClean(t, dest)
	})
	t.Run("archive size", func(t *testing.T) {
		data := testZip(t, validTheme("corp/")...)
		if _, err := InstallFromZip(bytes.NewReader(data), MaxArchiveSize+1, t.TempDir()); !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("install = %v, want ErrArchiveTooLarge", err)
		}
	})
}

func TestInstallFromZipRejectsInvalidThemes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []zipFile
		want  error
		msg   string
	}{
		{"external css url", []zipFile{
			{name: "corp/theme.json", body: validJSON("Corp")},
			{name: "corp/theme.css", body: `@font-face { src: url("https://cdn.example.com/font.woff2"); }`},
		}, ErrInvalidTheme, "external URL"},
		{"css import", []zipFile{
			{name: "corp/theme.json", body: validJSON("Corp")},
			{name: "corp/theme.css", body: `@import "other.css";`},
		}, ErrInvalidTheme, "@import"},
		{"missing theme.css", []zipFile{{name: "corp/theme.json", body: validJSON("Corp")}}, ErrInvalidTheme, "theme.css"},
		{"missing theme.json", []zipFile{{name: "corp/theme.css", body: validCSS}}, ErrInvalidTheme, "theme.json"},
		{"two top-level dirs", append(validTheme("a/"), validTheme("b/")...), ErrInvalidTheme, "theme.json"},
		{"bad manifest", []zipFile{
			{name: "corp/theme.json", body: `{"name": {"ja": "x"}}`},
			{name: "corp/theme.css", body: validCSS},
		}, ErrInvalidTheme, "description.ja"},
		{"built-in id", validTheme("modern/"), ErrBuiltinTheme, "modern"},
		{"duplicate entry", append(validTheme("corp/"), zipFile{name: "corp/THEME.css", body: validCSS}), ErrUnsafeArchive, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			_, err := install(t, testZip(t, tc.files...), dest)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("install = %v, want %v mentioning %q", err, tc.want, tc.msg)
			}
			assertClean(t, dest)
		})
	}
	t.Run("not a zip", func(t *testing.T) {
		if _, err := install(t, []byte("not a zip"), t.TempDir()); !errors.Is(err, ErrInvalidTheme) {
			t.Fatalf("install = %v, want ErrInvalidTheme", err)
		}
	})
}

func TestInstallRecordsSourceURL(t *testing.T) {
	dest := t.TempDir()
	data := testZip(t, validTheme("repo-main/")...)
	th, err := installZip(bytes.NewReader(data), int64(len(data)), dest, "repo", "https://github.com/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	if th.SourceURL != "https://github.com/example/repo" {
		t.Errorf("SourceURL = %q", th.SourceURL)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "repo", "theme.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["source_url"] != "https://github.com/example/repo" || m["order"] != float64(100) {
		t.Errorf("theme.json = %s", raw)
	}
}

func TestUninstallCustomThemeRejectsUnsafeIDs(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "themes")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// customDir の外にあるディレクトリ。消されてはいけない。
	outside := filepath.Join(parent, "keep")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := UninstallCustomTheme(dest, "modern"); !errors.Is(err, ErrBuiltinTheme) {
		t.Errorf("uninstall built-in = %v, want ErrBuiltinTheme", err)
	}
	for _, id := range []string{"../keep", "..", "", "keep/..", "/tmp", "Upper"} {
		if err := UninstallCustomTheme(dest, id); !errors.Is(err, ErrThemeNotFound) {
			t.Errorf("uninstall %q = %v, want ErrThemeNotFound", id, err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dest, "link")); err == nil {
		if err := UninstallCustomTheme(dest, "link"); !errors.Is(err, ErrThemeNotFound) {
			t.Errorf("uninstall symlink = %v, want ErrThemeNotFound", err)
		}
	}
	if err := UninstallCustomTheme("", "corp"); !errors.Is(err, ErrThemeNotFound) {
		t.Errorf("uninstall without a dir = %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("directory outside customDir was removed: %v", err)
	}
}

func TestSlugID(t *testing.T) {
	for in, want := range map[string]string{
		"repo-main":                    "repo",
		"repo-master":                  "repo",
		"-main":                        "main",
		"My Theme!":                    "my-theme",
		"__x":                          "x",
		"日本語":                          "",
		"":                             "",
		strings.Repeat("a", 60):        strings.Repeat("a", 41),
		strings.Repeat("a", 40) + "-b": strings.Repeat("a", 40),
	} {
		if got := slugID(in); got != want {
			t.Errorf("slugID(%q) = %q, want %q", in, got, want)
		}
	}
}

// stageAs は data を StageZipAs で dest に id として展開・検証する。
func stageAs(t *testing.T, data []byte, dest, id string) (*StagedTheme, error) {
	t.Helper()
	st, err := StageZipAs(bytes.NewReader(data), int64(len(data)), dest, id)
	if st != nil {
		t.Cleanup(func() { st.Close() })
	}
	return st, err
}

// StageZipAs は Zip の中の名前（-main で終わるディレクトリや theme.json の name.en）に関係なく、指定した ID の
// まま展開・検証し、Commit まではカタログの読む場所に何も置かない。Commit すると同じ ID のテーマを置き換える。
func TestStageZipAsKeepsTheGivenID(t *testing.T) {
	dest := t.TempDir()
	for _, id := range []string{"blog-main", "x"} {
		st, err := stageAs(t, testZip(t, validTheme("other-master/")...), dest, id)
		if err != nil {
			t.Fatalf("StageZipAs(%q): %v", id, err)
		}
		if st.Theme.ID != id {
			t.Fatalf("staged ID = %q, want %q", st.Theme.ID, id)
		}
		if _, err := os.Stat(filepath.Join(dest, id)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("staging placed the theme before Commit: %v", err)
		}
		c, err := LoadComposite(static.FS, dest)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Get(id); ok {
			t.Fatalf("the catalog sees a staged theme %q", id)
		}
		if err := st.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if !st.Theme.IsCustom || st.Theme.Dir != filepath.Join(mustAbs(t, dest), id) {
			t.Errorf("committed theme = %+v", st.Theme)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dest, id, "img", "bg.png")); err != nil {
			t.Fatalf("committed theme files: %v", err)
		}
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 2 {
		t.Errorf("dest has %d entries, want the two themes only (no staging left)", len(entries))
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// Rollback は置き換えたテーマを元に戻し、新しい ID なら取り除く。
func TestStagedThemeRollback(t *testing.T) {
	dest := t.TempDir()
	if _, err := install(t, testZip(t, append(validTheme("corp/"), zipFile{name: "corp/old.txt", body: "old"})...), dest); err != nil {
		t.Fatal(err)
	}
	files := validTheme("")
	files[1].body = ":root { --bg: #000; }\n"
	st, err := stageAs(t, testZip(t, files...), dest, "corp")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(); err != nil {
		t.Fatal(err)
	}
	if css, _ := os.ReadFile(filepath.Join(dest, "corp", "theme.css")); !strings.Contains(string(css), "#000") {
		t.Fatalf("committed css = %q", css)
	}
	if err := st.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if css, _ := os.ReadFile(filepath.Join(dest, "corp", "theme.css")); string(css) != validCSS {
		t.Errorf("css after rollback = %q, want the previous theme", css)
	}
	if _, err := os.Stat(filepath.Join(dest, "corp", "old.txt")); err != nil {
		t.Errorf("previous theme file after rollback: %v", err)
	}
	if err := st.Rollback(); err != nil {
		t.Errorf("second Rollback = %v, want no-op", err)
	}
	st.Close()

	fresh, err := stageAs(t, testZip(t, validTheme("")...), dest, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Rollback(); err != nil {
		t.Fatal(err)
	}
	fresh.Close()
	if _, err := os.Stat(filepath.Join(dest, "fresh")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rolled back new theme is still installed: %v", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Errorf("dest has %d entries, want only the previous theme", len(entries))
	}
}

// StageZipAs も InstallFromZip と同じ検査を通し、拒んだときは何も残さない。ビルトインテーマの ID や ID の形式に合わない
// 名前は拒む。
func TestStageZipAsRejects(t *testing.T) {
	big := strings.Repeat("0", MaxExtractedSize+1)
	for _, tc := range []struct {
		name string
		id   string
		data func(t *testing.T) []byte
		want error
	}{
		{"built-in ID", Default, func(t *testing.T) []byte { return testZip(t, validTheme("")...) }, ErrBuiltinTheme},
		{"path in ID", "../evil", func(t *testing.T) []byte { return testZip(t, validTheme("")...) }, ErrInvalidTheme},
		{"upper-case ID", "Corp", func(t *testing.T) []byte { return testZip(t, validTheme("")...) }, ErrInvalidTheme},
		{"zip slip", "corp", func(t *testing.T) []byte {
			return testZip(t, append(validTheme(""), zipFile{name: "../evil.css", body: "x"})...)
		}, ErrUnsafeArchive},
		{"symlink", "corp", func(t *testing.T) []byte {
			return testZip(t, append(validTheme(""), zipFile{name: "link.css", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777})...)
		}, ErrUnsafeArchive},
		{"extracted size", "corp", func(t *testing.T) []byte {
			return testZip(t, append(validTheme(""), zipFile{name: "big.bin", body: big})...)
		}, ErrArchiveTooLarge},
		{"external CSS", "corp", func(t *testing.T) []byte {
			files := validTheme("")
			files[1].body = "@import url(https://evil.example/x.css);"
			return testZip(t, files...)
		}, ErrInvalidTheme},
		{"not a zip", "corp", func(t *testing.T) []byte { return []byte("not a zip") }, ErrInvalidTheme},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "themes")
			if _, err := stageAs(t, tc.data(t), dest, tc.id); !errors.Is(err, tc.want) {
				t.Fatalf("StageZipAs = %v, want %v", err, tc.want)
			}
			assertClean(t, dest)
			if _, err := os.Stat(filepath.Join(parent, "evil.css")); err == nil {
				t.Error("a file was written outside the themes directory")
			}
		})
	}
	data := testZip(t, validTheme("")...)
	if _, err := StageZipAs(bytes.NewReader(data), MaxArchiveSize+1, t.TempDir(), "corp"); !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("StageZipAs of an oversized archive = %v, want ErrArchiveTooLarge", err)
	}
}

// failRename は renameFunc を差し替え、fail が true を返す移動だけを失敗させる（テストの終わりに戻す）。
func failRename(t *testing.T, fail func(from, to string) bool) {
	t.Helper()
	orig := renameFunc
	renameFunc = func(from, to string) error {
		if fail(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("injected failure")}
		}
		return orig(from, to)
	}
	t.Cleanup(func() { renameFunc = orig })
}

// stageUpdate は dest に corp（validCSS）を取り込み、theme.css だけ違う新しい版を corp として展開・検証する。
func stageUpdate(t *testing.T, dest string) *StagedTheme {
	t.Helper()
	if _, err := install(t, testZip(t, validTheme("corp/")...), dest); err != nil {
		t.Fatal(err)
	}
	files := validTheme("")
	files[1].body = newCSS
	st, err := stageAs(t, testZip(t, files...), dest, "corp")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

const newCSS = ":root { --bg: #000; }\n"

// readCSS は dir/theme.css を返す（読めなければ ""）。
func readCSS(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "theme.css"))
	return string(b)
}

// 新しい版を置けなかった Commit は、前の版を元の場所に戻して配置の前のままにし、Close で作業ディレクトリを消す。
func TestCommitFailurePutsThePreviousVersionBack(t *testing.T) {
	dest := t.TempDir()
	st := stageUpdate(t, dest)
	failRename(t, func(from, to string) bool { return from == st.src })
	if err := st.Commit(); err == nil || errors.Is(err, ErrNotRestored) {
		t.Fatalf("Commit = %v, want a failure that left the previous version in place", err)
	}
	if got := readCSS(filepath.Join(dest, "corp")); got != validCSS {
		t.Errorf("css after the failed commit = %q, want the previous version", got)
	}
	st.Close()
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Errorf("dest has %d entries, want only the previous theme", len(entries))
	}
}

// 置き換えを元に戻せなかった Commit・Rollback は ErrNotRestored を返し、前の版を作業ディレクトリに残す（Close でも
// 消さない）。エラーには前の版を残した場所を書く。起動時の掃除も、作業ディレクトリが古くなっても前の版を消さず、
// 配置先へ戻しもしない（配置先は失敗した直後のまま。テーマが無いか、新しい版）。
func TestNotRestoredKeepsThePreviousVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		// commit は Commit してから Rollback で失敗させるか（false なら Commit で失敗させる）。
		commit bool
		fail   func(st *StagedTheme, from string) bool
		// live は失敗した後の配置先の theme.css（"" はテーマが無い）。起動時の掃除の後も変わらない。
		live string
	}{
		{"commit cannot place the new version or put the previous one back", false,
			func(st *StagedTheme, from string) bool { return from == st.src || from == st.backup }, ""},
		{"rollback cannot put the previous version back", true,
			func(st *StagedTheme, from string) bool { return from == st.backup }, ""},
		{"rollback cannot remove the new version", true,
			func(st *StagedTheme, from string) bool { return from == st.dest }, newCSS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			st := stageUpdate(t, dest)
			failing := !tc.commit
			failRename(t, func(from, to string) bool { return failing && tc.fail(st, from) })
			var err error
			if tc.commit {
				if err := st.Commit(); err != nil {
					t.Fatal(err)
				}
				failing = true
				err = st.Rollback()
			} else {
				err = st.Commit()
			}
			if !errors.Is(err, ErrNotRestored) {
				t.Fatalf("got %v, want ErrNotRestored", err)
			}
			if want := "the previous version is kept in " + st.backup; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not say %q", err, want)
			}
			if got := readCSS(filepath.Join(dest, "corp")); got != tc.live {
				t.Errorf("css right after the failure = %q, want %q", got, tc.live)
			}
			st.Close()

			failing = false
			makeStale(t, st.staging)
			CleanupStaleStaging(dest, 30*time.Minute)
			if got := readCSS(st.backup); got != validCSS {
				t.Fatalf("previous version after Close and the startup cleanup = %q, want it kept", got)
			}
			if got := readCSS(filepath.Join(dest, "corp")); got != tc.live {
				t.Errorf("css after the startup cleanup = %q, want %q (left as it was)", got, tc.live)
			}
		})
	}
}

// mkTheme は dir/theme.css に css を書く（中断した置き換えの跡を作る）。
func mkTheme(t *testing.T, dir, css string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "theme.css"), []byte(css), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeStale は dirs の更新時刻を2時間前にする。
func makeStale(t *testing.T, dirs ...string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	for _, d := range dirs {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// 起動時の掃除（CleanupStaleStaging）の方針: 置き換え前のテーマ（.old）を含む作業ディレクトリは、置き換えが
// 終わったかを推測せず、古くなっても消さず、配置先へ戻しもしない（前の版の唯一の写しかもしれず、別のプロセスが
// 作業中かもしれない）。残したことはログに書く。.old を含まない古い作業ディレクトリと削除のゴミ箱だけを消し、
// 新しい作業ディレクトリと配置済みのテーマには触れない。
func TestCleanupStaleStagingKeepsWorkDirsHoldingAPreviousVersion(t *testing.T) {
	dest := t.TempDir()
	// a: 前の版を退避した直後に止まった（配置先に無い）。
	mkTheme(t, filepath.Join(dest, ".install-a", "corp"), newCSS)
	mkTheme(t, filepath.Join(dest, ".install-a", oldDirName), validCSS)
	// b: 新しい版を配置した直後に止まった（配置先に新しい版、作業ディレクトリには前の版だけ）。
	mkTheme(t, filepath.Join(dest, "blog"), newCSS)
	mkTheme(t, filepath.Join(dest, ".install-b", oldDirName), validCSS)
	// c: 前の版だけが残り、配置先にも無い。
	mkTheme(t, filepath.Join(dest, ".install-c", oldDirName), validCSS)
	// d: 展開しただけで止まった（前の版を含まない）。e: 削除のゴミ箱。
	mkTheme(t, filepath.Join(dest, ".install-d", "shop"), newCSS)
	mkTheme(t, filepath.Join(dest, ".uninstall-e", "gone"), validCSS)
	makeStale(t, filepath.Join(dest, ".install-a"), filepath.Join(dest, ".install-b"), filepath.Join(dest, ".install-c"),
		filepath.Join(dest, ".install-d"), filepath.Join(dest, ".uninstall-e"))
	// f: 別のプロセスが置き換えの途中（rename 2回の間）の、新しい作業ディレクトリ。
	mkTheme(t, filepath.Join(dest, ".install-f", "news"), newCSS)
	mkTheme(t, filepath.Join(dest, ".install-f", oldDirName), validCSS)

	var logs bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(orig) })
	CleanupStaleStaging(dest, 30*time.Minute)

	for _, kept := range []string{".install-a/" + oldDirName, ".install-a/corp", ".install-b/" + oldDirName, ".install-c/" + oldDirName,
		".install-f/" + oldDirName, ".install-f/news", "blog"} {
		if readCSS(filepath.Join(dest, kept)) == "" {
			t.Errorf("%s was removed or moved by the cleanup", kept)
		}
	}
	for _, gone := range []string{"corp", "news", ".install-d", ".uninstall-e"} {
		if _, err := os.Stat(filepath.Join(dest, gone)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists after the cleanup: %v", gone, err)
		}
	}
	for _, name := range []string{".install-a", ".install-b", ".install-c"} {
		if !strings.Contains(logs.String(), filepath.Join(dest, name)) {
			t.Errorf("the log does not say that %s was kept: %q", name, logs.String())
		}
	}
	if strings.Contains(logs.String(), ".install-f") {
		t.Errorf("the cleanup reported a recent work directory: %q", logs.String())
	}
}
