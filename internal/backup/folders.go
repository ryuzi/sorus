package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// FolderName は同期フォルダの中に作る、自動バックアップの保存先フォルダの名前。
const FolderName = "sorus バックアップ"

// CloudFolder は見つかったクラウドの同期フォルダ。
type CloudFolder struct {
	// Service は利用者に見せるサービスの名前（OneDrive・Google ドライブ・Dropbox）。
	Service string
	// Path は同期フォルダの場所。
	Path string
	// Suggested は自動バックアップの保存先として勧めるフォルダ（Path の中の FolderName）。
	Suggested string
}

// CloudFolders はこのパソコンにある OneDrive・Google ドライブ・Dropbox の同期フォルダを探して返す。
// 自動バックアップの保存先を選ぶ画面で、ボタン1つで選べる候補として出す。
func CloudFolders() []CloudFolder {
	home, _ := os.UserHomeDir()
	return cloudFoldersFor(runtime.GOOS, home, os.Getenv, isDir)
}

// cloudFoldersFor は goos の OS で、ホームディレクトリ home・環境変数 getenv から同期フォルダを探す。
// dirExists は場所がフォルダとして存在するかを返す（テストで差し替える）。
func cloudFoldersFor(goos, home string, getenv func(string) string, dirExists func(string) bool) []CloudFolder {
	type candidate struct{ service, path string }
	var cands []candidate
	add := func(service string, paths ...string) {
		for _, p := range paths {
			if p != "" {
				cands = append(cands, candidate{service, p})
			}
		}
	}
	// macOS の新しい同期アプリは ~/Library/CloudStorage/<サービス>-<アカウント> に置く。
	cloudStorage := func(prefix string, sub ...string) []string {
		if home == "" {
			return nil
		}
		matches, _ := filepath.Glob(filepath.Join(home, "Library", "CloudStorage", prefix+"*"))
		var out []string
		for _, m := range matches {
			if len(sub) == 0 {
				out = append(out, m)
			}
			for _, s := range sub {
				out = append(out, filepath.Join(m, s))
			}
		}
		return out
	}
	homeJoin := func(name string) string {
		if home == "" {
			return ""
		}
		return filepath.Join(home, name)
	}

	add("OneDrive", getenv("OneDrive"), getenv("OneDriveConsumer"), getenv("OneDriveCommercial"), homeJoin("OneDrive"))
	add("OneDrive", cloudStorage("OneDrive")...)
	add("Google ドライブ", cloudStorage("GoogleDrive", "マイドライブ", "My Drive")...)
	if goos == "windows" {
		// Google ドライブ（パソコン版）は G: などのドライブとして見せる。
		for c := 'D'; c <= 'Z'; c++ {
			root := string(c) + `:\`
			add("Google ドライブ", root+"マイドライブ", root+"My Drive")
		}
	}
	add("Google ドライブ", homeJoin("Google Drive"), homeJoin("Google ドライブ"))
	add("Dropbox", homeJoin("Dropbox"))
	add("Dropbox", cloudStorage("Dropbox")...)

	var found []CloudFolder
	var seen []string
	for _, c := range cands {
		p := filepath.Clean(c.path)
		if slices.Contains(seen, p) || !dirExists(p) {
			continue
		}
		seen = append(seen, p)
		found = append(found, CloudFolder{Service: c.service, Path: p, Suggested: filepath.Join(p, FolderName)})
	}
	return found
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
