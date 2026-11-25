package publish

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 初めて書き出すフォルダにトップページ（index.html）があるかの確認。無いフォルダ・空のフォルダ・ほかのファイルだけの
// フォルダは尋ねずに書き出し、index.html があれば（ファイル・ショートカットのどちらでも）ErrLiveSite で止める。
// index.html がフォルダなら置き換えられないため ErrTopPageIsFolder で止める。確かめるだけで、何も書かない。
func TestCheckFolderTopPage(t *testing.T) {
	mk := func(t *testing.T, files map[string]string) string {
		t.Helper()
		root := filepath.Join(t.TempDir(), "site")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		writeSite(t, root, files)
		return root
	}

	t.Run("missing folder", func(t *testing.T) {
		if err := CheckFolderTopPage(filepath.Join(t.TempDir(), "not-yet")); err != nil {
			t.Errorf("CheckFolderTopPage = %v, want nil", err)
		}
	})
	t.Run("empty folder", func(t *testing.T) {
		if err := CheckFolderTopPage(mk(t, nil)); err != nil {
			t.Errorf("CheckFolderTopPage = %v, want nil", err)
		}
	})
	t.Run("other files only", func(t *testing.T) {
		root := mk(t, map[string]string{"memo.txt": "メモ", "old/index.html": "下の階層のページ"})
		if err := CheckFolderTopPage(root); err != nil {
			t.Errorf("CheckFolderTopPage = %v, want nil (only the top-level index.html counts)", err)
		}
	})
	t.Run("top page file", func(t *testing.T) {
		root := mk(t, map[string]string{"index.html": "前のサイト"})
		if err := CheckFolderTopPage(root); !errors.Is(err, ErrLiveSite) {
			t.Errorf("CheckFolderTopPage = %v, want ErrLiveSite", err)
		}
		if got := listFiles(t, root); len(got) != 1 || got[0] != "index.html" {
			t.Errorf("folder = %v, want it untouched", got)
		}
	})
	t.Run("top page shortcut", func(t *testing.T) {
		root := mk(t, nil)
		target := filepath.Join(t.TempDir(), "elsewhere.html")
		if err := os.WriteFile(target, []byte("別の場所のページ"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "index.html")); err != nil {
			t.Fatal(err)
		}
		if err := CheckFolderTopPage(root); !errors.Is(err, ErrLiveSite) {
			t.Errorf("CheckFolderTopPage = %v, want ErrLiveSite", err)
		}
		// リンク先が無いショートカットも、今ある index.html として尋ねる。
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := CheckFolderTopPage(root); !errors.Is(err, ErrLiveSite) {
			t.Errorf("CheckFolderTopPage with a broken link = %v, want ErrLiveSite", err)
		}
	})
	t.Run("top page is a folder", func(t *testing.T) {
		root := mk(t, map[string]string{"index.html/inner.txt": "フォルダの中身"})
		err := CheckFolderTopPage(root)
		if !errors.Is(err, ErrTopPageIsFolder) || errors.Is(err, ErrLiveSite) {
			t.Errorf("CheckFolderTopPage = %v, want ErrTopPageIsFolder", err)
		}
	})
	t.Run("folder path is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "site")
		if err := os.WriteFile(file, []byte("ファイル"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := CheckFolderTopPage(file)
		if err == nil || errors.Is(err, ErrLiveSite) {
			t.Errorf("CheckFolderTopPage = %v, want an error that is not ErrLiveSite", err)
		}
	})
}
