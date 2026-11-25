package desktop

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// sites.json が無ければ、データディレクトリそのものを唯一のサイトとし、ファイルは作らない。
func TestLoadRegistryAbsent(t *testing.T) {
	p := AppPathsIn(t.TempDir())
	reg, exists, err := LoadRegistry(p)
	if err != nil || exists {
		t.Fatalf("LoadRegistry = exists %v, err %v; want absent without error", exists, err)
	}
	if len(reg.Sites) != 1 || reg.Sites[0].ID != "" || !reg.Sites[0].First() || reg.Sites[0].Key() != "" {
		t.Fatalf("sites = %+v, want only the data directory with no id", reg.Sites)
	}
	if got := p.Site(reg.Sites[0]); got != SitePathsIn(p.Dir) {
		t.Errorf("first site paths = %+v, want the data directory", got)
	}
	if reg.LastSite() != reg.Sites[0] {
		t.Errorf("LastSite = %+v, want the only site", reg.LastSite())
	}
	if _, err := os.Stat(p.Sites); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LoadRegistry created sites.json: %v", err)
	}
}

// 2つ目のサイトを加えると、1つ目のサイトにも id が付き、ディレクトリは "." のまま。書いて読み直すと同じ一覧になる。
func TestRegistryAddSecondSite(t *testing.T) {
	p := AppPathsIn(t.TempDir())
	reg, _, err := LoadRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	reg.Sites[0].Port = PreferredPort
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	second, err := reg.AddSite(p, now)
	if err != nil {
		t.Fatal(err)
	}
	first := reg.Sites[0]
	if !ValidSiteID(first.ID) || first.Dir != "." || first.Key() != "" {
		t.Errorf("first site = %+v, want an id, dir \".\" and an empty key", first)
	}
	if !ValidSiteID(second.ID) || second.ID == first.ID || second.Dir != "sites/"+second.ID ||
		second.Key() != second.ID || !second.Created.Equal(now) {
		t.Errorf("second site = %+v", second)
	}
	if got, want := p.Site(second).Dir, filepath.Join(p.Dir, "sites", second.ID); got != want {
		t.Errorf("second site dir = %s, want %s", got, want)
	}
	reg.Last = second.ID
	if err := SaveRegistry(p, reg); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := LoadRegistry(p)
	if err != nil || !exists {
		t.Fatalf("LoadRegistry after save: exists %v, err %v", exists, err)
	}
	if !slices.EqualFunc(loaded.Sites, reg.Sites, func(a, b Site) bool {
		return a.ID == b.ID && a.Dir == b.Dir && a.Port == b.Port && a.Created.Equal(b.Created)
	}) || loaded.Last != second.ID || loaded.LastSite().ID != second.ID {
		t.Errorf("loaded = %+v, want %+v", loaded, reg)
	}
	var raw map[string]any
	data, _ := os.ReadFile(p.Sites)
	if err := json.Unmarshal(data, &raw); err != nil || raw["last"] != second.ID {
		t.Errorf("sites.json = %s (err %v)", data, err)
	}
}

// 書き込みは一時ファイルに書いてから置き換えるため、一時ファイルを残さず、書けなければ前の内容がそのまま残る。
func TestSaveRegistryAtomic(t *testing.T) {
	p := AppPathsIn(t.TempDir())
	reg, _, _ := LoadRegistry(p)
	if _, err := reg.AddSite(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(p, reg); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p.Sites)

	// 正しくない一覧は書かない。
	bad := reg.Clone()
	bad.Sites[1].Dir = "../elsewhere"
	if err := SaveRegistry(p, bad); err == nil {
		t.Error("SaveRegistry wrote a site outside sites/")
	}
	// 置き換えられない（sites.json の場所がディレクトリ）ときも、一時ファイルを残さない。
	dirPaths := AppPathsIn(t.TempDir())
	if err := os.Mkdir(dirPaths.Sites, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirPaths.Sites, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(dirPaths, reg); err == nil {
		t.Error("SaveRegistry replaced a directory")
	}
	for _, dir := range []string{p.Dir, dirPaths.Dir} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".sites-") {
				t.Errorf("temporary file left in %s: %s", dir, e.Name())
			}
		}
	}
	if after, _ := os.ReadFile(p.Sites); string(after) != string(before) {
		t.Errorf("sites.json changed by a failed write:\n%s\nwant\n%s", after, before)
	}
}

// 壊れた・書き換えられた sites.json は読まずにエラーにする（サイトを黙って失わない・データディレクトリの外を消さない）。
func TestLoadRegistryRejectsInvalid(t *testing.T) {
	for name, content := range map[string]string{
		"not json":       `{`,
		"no sites":       `{"sites":[]}`,
		"no id":          `{"sites":[{"id":"","dir":"."}]}`,
		"bad id":         `{"sites":[{"id":"1abc","dir":"."}]}`,
		"duplicate id":   `{"sites":[{"id":"abc","dir":"."},{"id":"abc","dir":"sites/abc"}]}`,
		"outside":        `{"sites":[{"id":"abc","dir":"."},{"id":"xyz","dir":"../xyz"}]}`,
		"wrong dir":      `{"sites":[{"id":"abc","dir":"."},{"id":"xyz","dir":"sites/abc"}]}`,
		"two data dirs":  `{"sites":[{"id":"abc","dir":"."},{"id":"xyz","dir":"."}]}`,
		"invalid port":   `{"sites":[{"id":"abc","dir":".","port":70000}]}`,
		"sites dir self": `{"sites":[{"id":"abc","dir":"sites"}]}`,
	} {
		p := AppPathsIn(t.TempDir())
		if err := os.WriteFile(p.Sites, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadRegistry(p); err == nil {
			t.Errorf("%s: LoadRegistry accepted %s", name, content)
		}
	}
}

// id は英小文字で始まる英小文字と数字の短い乱数で、ほかのサイトとも sites/ に残ったディレクトリとも重ならない。
func TestSiteIDs(t *testing.T) {
	p := AppPathsIn(t.TempDir())
	if err := os.MkdirAll(filepath.Join(p.Dir, "sites"), 0o700); err != nil {
		t.Fatal(err)
	}
	reg, _, _ := LoadRegistry(p)
	format := regexp.MustCompile(`^[a-z][a-z0-9]{5}$`)
	seen := map[string]bool{}
	for range 200 {
		s, err := reg.AddSite(p, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !format.MatchString(s.ID) || !ValidSiteID(s.ID) {
			t.Fatalf("id %q has the wrong format", s.ID)
		}
		if seen[s.ID] {
			t.Fatalf("id %q was chosen twice", s.ID)
		}
		seen[s.ID] = true
	}
	if !format.MatchString(reg.Sites[0].ID) || seen[reg.Sites[0].ID] {
		t.Errorf("first site id = %q", reg.Sites[0].ID)
	}

	// 残っているディレクトリと同じ id は選ばない。
	taken := Registry{Sites: []Site{{ID: "abc", Dir: "."}}}
	for range 50 {
		id, err := taken.newSiteID(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(p.Dir, "sites", id), 0o700); err != nil {
			t.Fatalf("newSiteID chose %q, which already has a directory: %v", id, err)
		}
	}
	for _, id := range []string{"", "1abc", "Abc", "ab-c", "ab_c", "あ"} {
		if ValidSiteID(id) {
			t.Errorf("ValidSiteID(%q) = true", id)
		}
	}
}

// 記録したポートが空いていればそれを使い、使われていれば別のポートを選ぶ。ほかのサイトのポートは選ばない。
func TestListenSite(t *testing.T) {
	probe, err := net.Listen("tcp", net.JoinHostPort(ListenHost, "0"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	ln, err := ListenSite(recorded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ln.Addr().(*net.TCPAddr).Port; got != recorded {
		t.Errorf("free recorded port: listening on %d, want %d", got, recorded)
	}
	// 記録したポートが使われていれば、別のポートで待ち受ける。
	ln2, err := ListenSite(recorded, nil)
	if err != nil {
		t.Fatal(err)
	}
	busyReplaced := ln2.Addr().(*net.TCPAddr).Port
	if busyReplaced == recorded {
		t.Error("ListenSite reused a busy port")
	}
	ln.Close()
	ln2.Close()

	// 新しいサイト（記録なし）は、ほかのサイトのポートを避けて選ぶ。
	var avoid []int
	for p := PreferredPort + 1; p <= PreferredPort+siteSearchPorts-1; p++ {
		avoid = append(avoid, p)
	}
	ln3, err := ListenSite(0, avoid)
	if err != nil {
		t.Fatal(err)
	}
	defer ln3.Close()
	got := ln3.Addr().(*net.TCPAddr).Port
	if slices.Contains(avoid, got) || got == PreferredPort {
		t.Errorf("new site port = %d, which another site or the first site uses", got)
	}
	if host, _, _ := net.SplitHostPort(ln3.Addr().String()); host != ListenHost {
		t.Errorf("listening on %s, want host %s", ln3.Addr(), ListenHost)
	}
}

// 2つ目からのサイトはディレクトリごと消し、1つ目のサイトはサイトのデータとして決めたものだけを消す。
func TestRemoveSite(t *testing.T) {
	p := AppPathsIn(t.TempDir())
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := SitePathsIn(p.Dir)
	other := Site{ID: "xyz", Dir: "sites/xyz"}
	otherPaths := p.Site(other)
	remove := []string{first.DB, first.DB + "-wal", first.DB + "-shm", first.Recovery,
		filepath.Join(first.Uploads, "2026", "a.webp"), filepath.Join(first.Attachments, "b.pdf"),
		filepath.Join(first.SearchIndex, "store", "c")}
	keep := []string{p.Log, p.Instance, p.Lock, p.Sites, filepath.Join(p.DeletedBackups, "d.zip"),
		filepath.Join(first.Backups, "e.zip"), filepath.Join(p.Dir, "memo.txt"), otherPaths.DB,
		filepath.Join(otherPaths.Uploads, "f.webp")}
	for _, f := range append(slices.Clone(remove), keep...) {
		write(f)
	}

	if err := p.RemoveSite(Site{ID: "abc", Dir: "."}); err != nil {
		t.Fatalf("RemoveSite(first): %v", err)
	}
	for _, f := range remove {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was not removed: %v", f, err)
		}
	}
	for _, d := range []string{first.Uploads, first.Attachments, first.SearchIndex} {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was not removed: %v", d, err)
		}
	}
	for _, f := range keep {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed with the first site: %v", f, err)
		}
	}

	if err := p.RemoveSite(other); err != nil {
		t.Fatalf("RemoveSite(other): %v", err)
	}
	if _, err := os.Stat(otherPaths.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("site directory %s was not removed: %v", otherPaths.Dir, err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "sites")); err != nil {
		t.Errorf("sites/ itself was removed: %v", err)
	}
	if err := p.RemoveSite(Site{ID: "abc", Dir: "sites"}); err == nil {
		t.Error("RemoveSite removed sites/ itself")
	}
	if err := p.RemoveSite(Site{ID: "abc", Dir: "../abc"}); err == nil {
		t.Error("RemoveSite removed a directory outside sites/")
	}
}

// 削除で消える範囲の中に自動バックアップのフォルダがあるときは、消えない場所へ置く。
func TestDeleteBackupDir(t *testing.T) {
	root := t.TempDir()
	p := AppPathsIn(root)
	fallback := p.DeletedBackups
	later := p.PathsRemovedByDelete(Site{ID: "abc", Dir: "sites/abc"})
	siteDir := filepath.Join(root, "sites", "abc")
	if len(later) != 1 || later[0] != siteDir {
		t.Fatalf("later site removed paths = %v, want %s", later, siteDir)
	}
	inside := filepath.Join(siteDir, "backups")
	if got := DeleteBackupDir(inside, fallback, later); got != fallback {
		t.Errorf("inside = %s, want %s", got, fallback)
	}
	if got := DeleteBackupDir(siteDir, fallback, later); got != fallback {
		t.Errorf("equal to the removed directory = %s, want %s", got, fallback)
	}
	outside := filepath.Join(t.TempDir(), "safe")
	if got := DeleteBackupDir(outside, fallback, later); got != outside {
		t.Errorf("outside = %s, want the configured folder", got)
	}
	if got := DeleteBackupDir("  ", fallback, later); got != fallback {
		t.Errorf("blank configured = %s, want %s", got, fallback)
	}
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "backup-link")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	if got := DeleteBackupDir(link, fallback, later); got != fallback {
		t.Errorf("symlink into the site = %s, want %s", got, fallback)
	}

	first := p.PathsRemovedByDelete(Site{Dir: "."})
	uploads := SitePathsIn(root).Uploads
	if got := DeleteBackupDir(uploads, fallback, first); got != fallback {
		t.Errorf("first site uploads = %s, want %s", got, fallback)
	}
	backups := SitePathsIn(root).Backups
	if got := DeleteBackupDir(backups, fallback, first); got != backups {
		t.Errorf("first site backups = %s, want the configured folder (it is not removed)", got)
	}
	for _, kept := range []string{p.Dir, p.Sites, p.Log, backups, fallback} {
		for _, removed := range first {
			if removed == kept {
				t.Errorf("first site removal includes %s", kept)
			}
		}
	}

	// 大文字小文字・相対パス・末尾の区切りが違っても、消える範囲の中とみなす。
	// 名前が似ているだけの隣のフォルダは、消える範囲ではない。
	t.Chdir(root)
	sibling := SitePathsIn(root).Uploads + "-extra"
	if got := DeleteBackupDir(sibling, fallback, first); got != sibling {
		t.Errorf("sibling of a removed folder = %s, want the configured folder", got)
	}
	for _, configured := range []string{
		filepath.Join("Sites", "ABC", "Backups"),
		filepath.Join("sites", "abc", "backups") + string(filepath.Separator),
		filepath.Join(root, "SITES", "abc", "backups"),
	} {
		if got := DeleteBackupDir(configured, fallback, later); got != fallback {
			t.Errorf("later site %q = %s, want %s", configured, got, fallback)
		}
	}
	for _, item := range first {
		if got := DeleteBackupDir(item, fallback, first); got != fallback {
			t.Errorf("first site removed item %s = %s, want %s", item, got, fallback)
		}
		rel, err := filepath.Rel(root, item)
		if err != nil {
			t.Fatal(err)
		}
		for _, configured := range []string{
			item + string(filepath.Separator),
			rel,
			filepath.Join(filepath.Dir(rel), swapASCIICase(filepath.Base(rel))),
			filepath.Join(filepath.Dir(item), swapASCIICase(filepath.Base(item))),
		} {
			if got := DeleteBackupDir(configured, fallback, first); got != fallback {
				t.Errorf("first site %q = %s, want %s", configured, got, fallback)
			}
		}
	}
}

// swapASCIICase は s の英字の大文字と小文字を入れ替える。
func swapASCIICase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case 'A' <= r && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// 削除の前のバックアップの名前はサイトの id を含み、自動バックアップの名前の形（sorus_backup_…）に当たらない。
func TestDeletedSiteBackupName(t *testing.T) {
	name := DeletedSiteBackupName("abc123", time.Date(2026, 10, 29, 12, 0, 5, 0, time.FixedZone("JST", 9*3600)))
	if name != "sorus_deleted_site_abc123_20261029_030005.zip" {
		t.Errorf("name = %q", name)
	}
	// 自動バックアップの名前: 1つ目のサイトは sorus_backup_YYYYMMDD_HHMMSS.zip、ほかは sorus_backup_<key>_YYYYMMDD_HHMMSS.zip。
	auto := regexp.MustCompile(`^sorus_backup_`)
	if auto.MatchString(name) {
		t.Errorf("%q looks like an automatic backup and could be pruned", name)
	}
}
