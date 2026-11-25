// Package publish は書き出した静的サイトを公開先（レンタルサーバーの FTP・SFTP、Cloudflare Pages、手元のフォルダ）へ
// 反映する。前回の反映で送ったファイルの一覧（Manifest）と比べて、新しいファイル・変わったファイルだけを送り、
// 書き出しから消えたファイルだけを公開先から消す。公開先にもともとあったファイル（前回の一覧に無いもの）は消さない。
package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// File は書き出したファイル1つの内容の要約。
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest は書き出したサイトのフォルダからの相対パス（/ 区切り）ごとのファイルの要約。
// 反映に成功したときの Manifest を保存しておき、次の反映で比べる。nil は「まだ一度も反映していない」。
type Manifest map[string]File

// ErrUnsafePath は、公開先のフォルダの外を指すおそれのある相対パスを断ったエラー。
var ErrUnsafePath = errors.New("publish: unsafe path")

// ErrUnsafeRoot は、アップロード先のフォルダとして空欄・「/」（サーバー全体）などを断ったエラー。
var ErrUnsafeRoot = errors.New("アップロード先のフォルダに、空欄や「/」だけは指定できません")

// BuildManifest は dir の下のすべてのファイルの要約を返す。フォルダとシンボリックリンクは含めない。
func BuildManifest(dir string) (Manifest, error) {
	m := Manifest{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		f, err := hashFile(p)
		if err != nil {
			return err
		}
		m[filepath.ToSlash(rel)] = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("publish: build manifest: %w", err)
	}
	return m, nil
}

func hashFile(p string) (File, error) {
	f, err := os.Open(p)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return File{}, err
	}
	return File{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// Plan は前回の反映（prev）から今回の書き出し（next）へ公開先を揃えるための作業。
type Plan struct {
	// Upload は新しいファイル・内容が変わったファイル。画像やスタイルなどを先に、HTML を後に、
	// トップページ（index.html）を最後に送る順に並べる（送っている途中でもページの参照先が欠けにくくする）。
	Upload []string
	// Delete は前回送ったが、今回の書き出しには無いファイル。
	Delete []string
	// Unchanged は内容が変わらず、送らないファイルの数。
	Unchanged int
	// RemoveDirs は Delete で空になるかもしれないフォルダ（深い順）。今回の書き出しにあるフォルダは含めない。
	RemoveDirs []string
}

// NewPlan は prev と next を比べた作業を返す。prev が nil（初めての反映）なら next のすべてを送り、何も消さない。
func NewPlan(prev, next Manifest) Plan {
	var p Plan
	for name, f := range next {
		if old, ok := prev[name]; ok && old == f {
			p.Unchanged++
			continue
		}
		p.Upload = append(p.Upload, name)
	}
	slices.SortFunc(p.Upload, func(a, b string) int {
		if ra, rb := uploadRank(a), uploadRank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	for name := range prev {
		if _, ok := next[name]; !ok {
			p.Delete = append(p.Delete, name)
		}
	}
	slices.Sort(p.Delete)

	keep := dirsOf(next)
	gone := map[string]bool{}
	for _, name := range p.Delete {
		for d := path.Dir(name); d != "." && !keep[d]; d = path.Dir(d) {
			gone[d] = true
		}
	}
	for d := range gone {
		p.RemoveDirs = append(p.RemoveDirs, d)
	}
	slices.SortFunc(p.RemoveDirs, func(a, b string) int {
		if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
			return db - da
		}
		return strings.Compare(a, b)
	})
	return p
}

// uploadRank は送る順番の組。HTML 以外 → HTML → トップページの順。
func uploadRank(name string) int {
	switch {
	case name == "index.html":
		return 2
	case strings.HasSuffix(name, ".html"):
		return 1
	}
	return 0
}

// dirsOf は m のファイルを含むすべてのフォルダ（相対パス）。
func dirsOf(m Manifest) map[string]bool {
	dirs := map[string]bool{}
	for name := range m {
		for d := path.Dir(name); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	return dirs
}

// Total は Plan の送る・消すファイルの合計。進み具合の分母にする。
func (p Plan) Total() int { return len(p.Upload) + len(p.Delete) }

// CleanRel は公開先のフォルダからの相対パス p を検査し、正規化して返す。空・「.」・絶対パス・「..」を含むパス・
// バックスラッシュや制御文字を含むパスは ErrUnsafePath で断る（公開先のフォルダの外には書かない・消さない）。
func CleanRel(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n") {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: %q", ErrUnsafePath, p)
		}
	}
	return p, nil
}

// CleanRoot はレンタルサーバー（FTP・SFTP）のアップロード先のフォルダを検査し、正規化して返す。
// 空欄・「/」・「.」など、サーバーのいちばん上やログインした場所そのものを指すものは ErrUnsafeRoot で断る。
// 「..」を含むものや制御文字を含むものも断る。相対パス（例: public_html）はログインした場所からの位置になる。
func CleanRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if strings.ContainsAny(root, "\\\x00\r\n") {
		return "", ErrUnsafeRoot
	}
	for _, seg := range strings.Split(root, "/") {
		if seg == ".." {
			return "", ErrUnsafeRoot
		}
	}
	clean := path.Clean(root)
	if root == "" || clean == "/" || clean == "." {
		return "", ErrUnsafeRoot
	}
	return clean, nil
}

// Progress は反映の進み具合。
type Progress struct {
	// Done は送り終えた・消し終えたファイルの数。Total は Plan.Total。
	Done, Total int
	// Path は今送っている・消しているファイル。
	Path string
}

// Result は反映の結果。
type Result struct {
	// Uploaded は送ったファイル、Deleted は公開先から消したファイル、Unchanged は変わらず送らなかったファイルの数。
	Uploaded, Deleted, Unchanged int
	// FirstTime は、この公開先へ初めて反映したか。
	FirstTime bool
	// KeptExisting は初めての反映で、アップロード先のフォルダにもともとあったファイル・フォルダの数。
	// sorus が送ったものではないため消さずに残す。
	KeptExisting int
	// URL は公開先が教えてくれた公開 URL（Cloudflare Pages の今回の版の URL など）。無ければ空。
	URL string
	// Notes は反映はできたが知らせておきたいこと（暗号化せずに接続したことなど）。画面にそのまま出せる日本語の文。
	Notes []string
}

// Remote はファイルを1つずつ書き換える公開先（FTP・SFTP・フォルダ）への接続。
// パスはすべてアップロード先のフォルダからの相対パス（CleanRel 済み）で渡す。
type Remote interface {
	// Put は p に r の内容（size バイト）を書く。親フォルダが無ければ作る。
	Put(ctx context.Context, p string, r io.Reader, size int64) error
	// Delete は p のファイルを消す。もう無ければ nil を返す。
	Delete(ctx context.Context, p string) error
	// RemoveDir は空のフォルダ p を消す。空でない（利用者のファイルがある）ときや無いときはエラーを返してよい。
	RemoveDir(ctx context.Context, p string) error
	// CountEntries はアップロード先のフォルダの直下にあるファイル・フォルダの数を返す。フォルダが無ければ 0。
	CountEntries(ctx context.Context) (int, error)
	// Notes は接続について知らせておきたいこと（暗号化していないなど）。
	Notes() []string
	Close() error
}

// Sync は dir の書き出しを r に反映する。prev は前回の反映に成功したときの Manifest（初めてなら nil）、
// next は dir の Manifest。新しい・変わったファイルを送ってから、消えたファイルを消す。
// 途中で失敗したらそこで止めてエラーを返す（呼び出し側は Manifest を保存せず、次の反映でやり直す）。
func Sync(ctx context.Context, r Remote, dir string, prev, next Manifest, progress func(Progress)) (Result, error) {
	plan := NewPlan(prev, next)
	res := Result{Unchanged: plan.Unchanged, FirstTime: prev == nil, Notes: r.Notes()}
	if res.FirstTime {
		n, err := r.CountEntries(ctx)
		if err != nil {
			return res, fmt.Errorf("publish: list remote folder: %w", err)
		}
		res.KeptExisting = n
	}
	total := plan.Total()
	report := func(done int, p string) {
		if progress != nil {
			progress(Progress{Done: done, Total: total, Path: p})
		}
	}
	done := 0
	for _, name := range plan.Upload {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rel, err := CleanRel(name)
		if err != nil {
			return res, err
		}
		report(done, rel)
		if err := putFile(ctx, r, dir, rel); err != nil {
			return res, fmt.Errorf("publish: upload %s: %w", rel, err)
		}
		res.Uploaded++
		done++
	}
	for _, name := range plan.Delete {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rel, err := CleanRel(name)
		if err != nil {
			return res, err
		}
		report(done, rel)
		if err := r.Delete(ctx, rel); err != nil {
			return res, fmt.Errorf("publish: delete %s: %w", rel, err)
		}
		res.Deleted++
		done++
	}
	// 空になったフォルダを消す。利用者のファイルが残っているフォルダは消えないため、失敗は気にしない。
	for _, d := range plan.RemoveDirs {
		if rel, err := CleanRel(d); err == nil {
			_ = r.RemoveDir(ctx, rel)
		}
	}
	report(done, "")
	return res, nil
}

func putFile(ctx context.Context, r Remote, dir, rel string) error {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return r.Put(ctx, rel, f, info.Size())
}
