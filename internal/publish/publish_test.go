package publish

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// writeSite は dir に files（相対パス → 内容）を書く。
func writeSite(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	m, err := BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// listFiles は dir の下のファイルの相対パス（/ 区切り）を並べて返す。
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestBuildManifestAndPlan(t *testing.T) {
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{
		"index.html": "top", "articles/a/index.html": "A", "articles/b/index.html": "B",
		"static/app.css": "css", "feed.xml": "feed",
	})
	prev := mustManifest(t, dir)
	if got := prev["articles/a/index.html"]; got.Size != 1 || len(got.SHA256) != 64 {
		t.Errorf("manifest entry = %+v", got)
	}

	// 初めての反映: すべて送り、何も消さない。HTML 以外 → HTML → トップページの順。
	first := NewPlan(nil, prev)
	want := []string{"feed.xml", "static/app.css", "articles/a/index.html", "articles/b/index.html", "index.html"}
	if !reflect.DeepEqual(first.Upload, want) || len(first.Delete) != 0 || first.Unchanged != 0 {
		t.Errorf("first plan = %+v, want upload %v", first, want)
	}

	// 記事 b を消し、トップページを変え、記事 c を足す。
	os.RemoveAll(filepath.Join(dir, "articles", "b"))
	writeSite(t, dir, map[string]string{"index.html": "top2", "articles/c/index.html": "C"})
	next := mustManifest(t, dir)
	p := NewPlan(prev, next)
	if want := []string{"articles/c/index.html", "index.html"}; !reflect.DeepEqual(p.Upload, want) {
		t.Errorf("upload = %v, want %v", p.Upload, want)
	}
	if want := []string{"articles/b/index.html"}; !reflect.DeepEqual(p.Delete, want) {
		t.Errorf("delete = %v, want %v", p.Delete, want)
	}
	if want := []string{"articles/b"}; !reflect.DeepEqual(p.RemoveDirs, want) {
		t.Errorf("remove dirs = %v, want %v (articles/ still has files)", p.RemoveDirs, want)
	}
	if p.Unchanged != 3 || p.Total() != 3 {
		t.Errorf("unchanged = %d total = %d, want 3 and 3", p.Unchanged, p.Total())
	}
}

func TestCleanRootRefusesWholeServer(t *testing.T) {
	for _, root := range []string{"", " ", "/", "//", ".", "./", "/./", "/..", "../x", "/a/../..", "a\\b", "a\nb"} {
		if _, err := CleanRoot(root); !errors.Is(err, ErrUnsafeRoot) {
			t.Errorf("CleanRoot(%q) = %v, want ErrUnsafeRoot", root, err)
		}
	}
	for root, want := range map[string]string{"/public_html": "/public_html", "public_html/": "public_html", "/home/u/www/": "/home/u/www"} {
		if got, err := CleanRoot(root); err != nil || got != want {
			t.Errorf("CleanRoot(%q) = %q, %v; want %q", root, got, err, want)
		}
	}
}

func TestCleanRelRefusesEscapes(t *testing.T) {
	for _, p := range []string{"", ".", "..", "../x", "a/../../x", "/etc/passwd", "a//b", "a\\b", "a/./b"} {
		if _, err := CleanRel(p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("CleanRel(%q) = %v, want ErrUnsafePath", p, err)
		}
	}
	if got, err := CleanRel("articles/a/index.html"); err != nil || got != "articles/a/index.html" {
		t.Errorf("CleanRel = %q, %v", got, err)
	}
}

// fakeRemote は送った・消したファイルを記録する Remote。failPut のパスへの Put は失敗する。
type fakeRemote struct {
	files   map[string]string
	failPut string
	puts    []string
	deletes []string
}

func (f *fakeRemote) Put(ctx context.Context, p string, r io.Reader, size int64) error {
	if p == f.failPut {
		return errors.New("disk full")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.files[p] = string(b)
	f.puts = append(f.puts, p)
	return nil
}
func (f *fakeRemote) Delete(ctx context.Context, p string) error {
	delete(f.files, p)
	f.deletes = append(f.deletes, p)
	return nil
}
func (f *fakeRemote) RemoveDir(ctx context.Context, p string) error { return nil }
func (f *fakeRemote) CountEntries(ctx context.Context) (int, error) {
	return len(f.files), nil
}
func (f *fakeRemote) Notes() []string { return nil }
func (f *fakeRemote) Close() error    { return nil }

func TestSyncStopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"a.css": "a", "b.css": "b", "index.html": "i"})
	r := &fakeRemote{files: map[string]string{"user.txt": "mine"}, failPut: "b.css"}
	var last Progress
	res, err := Sync(context.Background(), r, dir, nil, mustManifest(t, dir), func(p Progress) { last = p })
	if err == nil || !strings.Contains(err.Error(), "b.css") {
		t.Fatalf("Sync err = %v, want failure naming b.css", err)
	}
	if !reflect.DeepEqual(r.puts, []string{"a.css"}) || res.Uploaded != 1 {
		t.Errorf("puts = %v uploaded = %d, want only a.css before the failure", r.puts, res.Uploaded)
	}
	if !res.FirstTime || res.KeptExisting != 1 {
		t.Errorf("first time = %v kept = %d, want true and 1", res.FirstTime, res.KeptExisting)
	}
	if last.Path != "b.css" || last.Total != 3 {
		t.Errorf("last progress = %+v", last)
	}
}

func TestSyncRefusesUnsafeManifestPath(t *testing.T) {
	dir := t.TempDir()
	writeSite(t, dir, map[string]string{"index.html": "i"})
	r := &fakeRemote{files: map[string]string{}}
	// 保存してある前回の一覧が壊れていても、公開先のフォルダの外は消さない。
	prev := Manifest{"../outside.html": {SHA256: "x", Size: 1}}
	if _, err := Sync(context.Background(), r, dir, prev, mustManifest(t, dir), nil); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Sync = %v, want ErrUnsafePath", err)
	}
	if len(r.deletes) != 0 {
		t.Errorf("deleted %v", r.deletes)
	}
}

func TestFolderRemote(t *testing.T) {
	site := t.TempDir()
	writeSite(t, site, map[string]string{"index.html": "top", "articles/a/index.html": "A", "articles/b/index.html": "B"})
	out := filepath.Join(t.TempDir(), "site-export")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSite(t, out, map[string]string{"memo.txt": "利用者のファイル"})
	r, err := OpenFolder(out)
	if err != nil {
		t.Fatal(err)
	}
	prev := mustManifest(t, site)
	res, err := Sync(context.Background(), r, site, nil, prev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 3 || res.KeptExisting != 1 {
		t.Errorf("result = %+v", res)
	}
	os.RemoveAll(filepath.Join(site, "articles", "b"))
	if _, err := Sync(context.Background(), r, site, prev, mustManifest(t, site), nil); err != nil {
		t.Fatal(err)
	}
	if got, want := listFiles(t, out), []string{"articles/a/index.html", "index.html", "memo.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("folder = %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(out, "articles", "b")); !os.IsNotExist(err) {
		t.Errorf("empty folder articles/b was left: %v", err)
	}
	if _, err := OpenFolder("/"); !errors.Is(err, ErrUnsafeRoot) {
		t.Errorf("OpenFolder(/) = %v, want ErrUnsafeRoot", err)
	}
	if _, err := OpenFolder("relative/dir"); err == nil {
		t.Error("OpenFolder accepted a relative path")
	}
}
