package publish

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// folderRemote は手元のフォルダを公開先にする。ほかの公開先と同じく、変わったファイルだけを書き、
// 書き出しから消えたファイルだけを消す（フォルダにもともとあったファイルは残す）。
type folderRemote struct {
	root string
}

// OpenFolder は絶対パスのフォルダ root を公開先として開く。root が無ければ最初に書くときに作る。
// ドライブ全体（/ や C:\）は断る。どのフォルダなら使ってよいかの細かな確認は呼び出し側で行う。
func OpenFolder(root string) (Remote, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("publish: folder %q is not absolute", root)
	}
	root = filepath.Clean(root)
	if filepath.Dir(root) == root {
		return nil, ErrUnsafeRoot
	}
	return &folderRemote{root: root}, nil
}

func (r *folderRemote) full(rel string) string { return filepath.Join(r.root, filepath.FromSlash(rel)) }

// Put は同じフォルダの一時ファイルに書いてから名前を変え、書きかけのファイルを見せない。
func (r *folderRemote) Put(ctx context.Context, rel string, body io.Reader, size int64) error {
	p := r.full(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".sorus-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (r *folderRemote) Delete(ctx context.Context, rel string) error {
	p := r.full(rel)
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// 前回書いたファイルの場所が、利用者の作ったフォルダに変わっていたら消さない。
	if info.IsDir() {
		return fmt.Errorf("publish: %s is a folder", rel)
	}
	return os.Remove(p)
}

func (r *folderRemote) RemoveDir(ctx context.Context, rel string) error {
	// os.Remove は空のフォルダしか消さない。
	return os.Remove(r.full(rel))
}

func (r *folderRemote) CountEntries(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(r.root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return len(entries), err
}

func (r *folderRemote) Notes() []string { return nil }

func (r *folderRemote) Close() error { return nil }

// ErrTopPageIsFolder は、書き出すフォルダのトップページ（index.html）の場所にフォルダがあり、書き出せないこと。
var ErrTopPageIsFolder = errors.New("書き出すフォルダの中に「index.html」という名前のフォルダがあるため、トップページを書き出せません")

// CheckFolderTopPage は、初めて書き出すフォルダ root に、すでにトップページ（index.html）があるかを確かめる。何も書かない。
// root や index.html が無ければ nil を返す。index.html があれば（ファイルでもショートカットでも）ErrLiveSite を返し、
// 呼び出し側は利用者が置き換えを選ぶまで書き出さない。index.html がフォルダなら置き換えられないため ErrTopPageIsFolder を返す。
func CheckFolderTopPage(root string) error {
	info, err := os.Lstat(filepath.Join(root, "index.html"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("publish: check folder top page: %w", err)
	case info.IsDir():
		return ErrTopPageIsFolder
	}
	return fmt.Errorf("%w: folder %s already has index.html", ErrLiveSite, root)
}

// WriteZip は dir の下のすべてのファイルを、dir からの相対パスで Zip にして w に書く。
func WriteZip(w io.Writer, dir string) error {
	zw := zip.NewWriter(w)
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name, h.Method = filepath.ToSlash(rel), zip.Deflate
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(fw, f)
		return err
	})
	if err != nil {
		return fmt.Errorf("publish: zip: %w", err)
	}
	return zw.Close()
}
