package desktop

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	// sitesFileName はサイトの一覧のファイルの名前。
	sitesFileName = "sites.json"
	// sitesDirName は2つ目からのサイトのディレクトリを置くディレクトリの名前。
	sitesDirName = "sites"
	// siteIDLength はサイトの id の長さ。
	siteIDLength = 6
)

// Site は sites.json に記録する1つのサイト。サイト名は記録しない（そのサイトの DB から読み、二重に持たない）。
type Site struct {
	// ID はサイトの id（英小文字と数字の短い乱数。先頭は英字）。sites.json がまだ無いときの1つ目のサイトは空。
	ID string `json:"id"`
	// Dir はサイトのディレクトリ。データディレクトリからの相対パスを / で区切って書く。1つ目のサイトは "."、
	// 2つ目からは "sites/<id>"。
	Dir string `json:"dir"`
	// Port は待ち受けるポート。次に起動したときも同じポートを使う（ブックマークがそのまま使える）。0 なら未記録。
	Port int `json:"port"`
	// Created はサイトを作った日時。
	Created time.Time `json:"created"`
}

// First は s が1つ目のサイト（データディレクトリそのもの）かを返す。
func (s Site) First() bool { return s.Dir == "." }

// Key はサイトの鍵（server.Config.SiteKey）を返す。Cookie の名前と自動バックアップの名前に使う。1つ目のサイトは空
// （今までの名前のまま）、2つ目からは id。
func (s Site) Key() string {
	if s.First() {
		return ""
	}
	return s.ID
}

// Registry はサイトの一覧（sites.json の内容）。
//
//	{
//	  "sites": [
//	    {"id": "k3x9mq", "dir": ".", "port": 8780, "created": "2026-10-29T10:00:00+09:00"},
//	    {"id": "p7d2wa", "dir": "sites/p7d2wa", "port": 8781, "created": "2026-10-29T10:05:00+09:00"}
//	  ],
//	  "last": "p7d2wa"
//	}
type Registry struct {
	// Sites はサイトを作った順に並べる。
	Sites []Site `json:"sites"`
	// Last は最後に使ったサイトの id（アプリ全体で1つ）。
	Last string `json:"last,omitempty"`
}

// LoadRegistry は p.Sites（sites.json）を読む。ファイルが無ければ、データディレクトリそのものを唯一のサイト
// （id は空）とする一覧と exists=false を返し、ファイルは作らない。中身が正しくなければエラーを返す（読めない
// サイトを黙って一覧から落とし、次の書き込みで失わないため）。
func LoadRegistry(p AppPaths) (reg Registry, exists bool, err error) {
	data, err := os.ReadFile(p.Sites)
	if errors.Is(err, os.ErrNotExist) {
		return Registry{Sites: []Site{{Dir: ".", Created: firstSiteCreated(p)}}}, false, nil
	}
	if err != nil {
		return reg, false, fmt.Errorf("desktop: read sites: %w", err)
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return reg, true, fmt.Errorf("desktop: %s: %w", p.Sites, err)
	}
	if err := reg.validate(); err != nil {
		return reg, true, fmt.Errorf("desktop: %s: %w", p.Sites, err)
	}
	return reg, true, nil
}

// firstSiteCreated は、まだ sites.json に記録していない1つ目のサイトの作成日時として、DB のファイルの日時を返す。
// DB が無ければ今の日時を返す。
func firstSiteCreated(p AppPaths) time.Time {
	if st, err := os.Stat(SitePathsIn(p.Dir).DB); err == nil {
		return st.ModTime().Truncate(time.Second)
	}
	return time.Now().Truncate(time.Second)
}

// validate は reg が sites.json として正しいかを確かめる。サイトが1つ以上あり、id が正しい形で重ならず、
// ディレクトリが "."（1つまで）か "sites/<id>" であること。
func (r Registry) validate() error {
	if len(r.Sites) == 0 {
		return errors.New("no sites")
	}
	seen := map[string]bool{}
	first := 0
	for _, s := range r.Sites {
		if !ValidSiteID(s.ID) {
			return fmt.Errorf("invalid site id %q", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate site id %q", s.ID)
		}
		seen[s.ID] = true
		switch s.Dir {
		case ".":
			if first++; first > 1 {
				return errors.New(`more than one site uses the data directory "."`)
			}
		case sitesDirName + "/" + s.ID:
		default:
			return fmt.Errorf("site %s: invalid dir %q", s.ID, s.Dir)
		}
		if s.Port < 0 || s.Port > 65535 {
			return fmt.Errorf("site %s: invalid port %d", s.ID, s.Port)
		}
	}
	return nil
}

// SaveRegistry は reg を p.Sites（sites.json）に書く。読み手が書きかけのファイルを読まないよう、同じディレクトリの
// 一時ファイルに書いてから置き換える。sites.json を書くのは動いているアプリだけで、コマンド（sorus backup など）は
// 読むだけにする。
func SaveRegistry(p AppPaths, reg Registry) error {
	if err := reg.validate(); err != nil {
		return fmt.Errorf("desktop: write sites: %w", err)
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.Sites), ".sites-*.json")
	if err != nil {
		return fmt.Errorf("desktop: write sites: %w", err)
	}
	_, werr := tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), p.Sites)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("desktop: write sites: %w", werr)
	}
	return nil
}

// Clone は r の複製を返す（書き込みに失敗したときに元の一覧を残すため）。
func (r Registry) Clone() Registry {
	r.Sites = slices.Clone(r.Sites)
	return r
}

// Index は id のサイトの、一覧での位置を返す。無ければ -1。
func (r Registry) Index(id string) int {
	return slices.IndexFunc(r.Sites, func(s Site) bool { return s.ID == id })
}

// LastSite は最後に使ったサイトを返す。記録が無いか、そのサイトが一覧に無ければ先頭のサイトを返す。
func (r Registry) LastSite() Site {
	if i := r.Index(r.Last); r.Last != "" && i >= 0 {
		return r.Sites[i]
	}
	return r.Sites[0]
}

// IDs はすべてのサイトの id を一覧の順に返す。
func (r Registry) IDs() []string {
	ids := make([]string, len(r.Sites))
	for i, s := range r.Sites {
		ids[i] = s.ID
	}
	return ids
}

// AddSite は新しいサイトを一覧の最後に加えて返す（ディレクトリは作らない）。1つ目のサイトにまだ id が無ければ
// （sites.json がまだ無かったとき）、それにも id を付ける。id はどのサイトとも、p の sites/ にあるディレクトリとも
// 重ならない。
func (r *Registry) AddSite(p AppPaths, now time.Time) (Site, error) {
	for i := range r.Sites {
		if r.Sites[i].ID == "" {
			id, err := r.newSiteID(p)
			if err != nil {
				return Site{}, err
			}
			r.Sites[i].ID = id
		}
	}
	id, err := r.newSiteID(p)
	if err != nil {
		return Site{}, err
	}
	s := Site{ID: id, Dir: sitesDirName + "/" + id, Created: now.Truncate(time.Second)}
	r.Sites = append(r.Sites, s)
	return s, nil
}

// newSiteID は r のどのサイトとも、p の sites/ にあるディレクトリとも重ならない新しい id を返す。
func (r Registry) newSiteID(p AppPaths) (string, error) {
	for range 100 {
		id, err := randomSiteID()
		if err != nil {
			return "", err
		}
		if r.Index(id) >= 0 {
			continue
		}
		if _, err := os.Lstat(filepath.Join(p.Dir, sitesDirName, id)); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		return id, nil
	}
	return "", errors.New("desktop: could not choose a new site id")
}

// siteIDLetters・siteIDChars は id の先頭と2文字目からに使う文字。
const (
	siteIDLetters = "abcdefghijklmnopqrstuvwxyz"
	siteIDChars   = siteIDLetters + "0123456789"
)

// randomSiteID は siteIDLength 文字の英小文字と数字の乱数（先頭は英字）を返す。
func randomSiteID() (string, error) {
	id := make([]byte, siteIDLength)
	for i := range id {
		chars := siteIDChars
		if i == 0 {
			chars = siteIDLetters
		}
		c, err := randomChar(chars)
		if err != nil {
			return "", fmt.Errorf("desktop: site id: %w", err)
		}
		id[i] = c
	}
	return string(id), nil
}

// randomChar は chars から1文字を偏りなく選ぶ。
func randomChar(chars string) (byte, error) {
	limit := 256 - 256%len(chars)
	var b [1]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if int(b[0]) < limit {
			return chars[int(b[0])%len(chars)], nil
		}
	}
}

// ValidSiteID は id がサイトの id の形（英小文字で始まり、英小文字と数字だけ）かを返す。
func ValidSiteID(id string) bool {
	if id == "" || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for i := 1; i < len(id); i++ {
		if c := id[i]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// DeletedSiteBackupName は、サイト id を削除する前に t 時点で作る完全バックアップの名前
// （sorus_deleted_site_<id>_YYYYMMDD_HHMMSS.zip）を返す。自動バックアップの名前（sorus_backup_ で始まる）とは
// 形を変え、同じフォルダに置いても古い世代の整理で消されないようにする。時刻は UTC で表す。
func DeletedSiteBackupName(id string, t time.Time) string {
	return "sorus_deleted_site_" + id + "_" + t.UTC().Format("20060102_150405") + ".zip"
}
