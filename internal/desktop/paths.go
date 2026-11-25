package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DataDirEnv はデータディレクトリを OS 標準の場所から差し替える環境変数（テスト・上級者向け）。
const DataDirEnv = "SORUS_DATA_DIR"

// dirName は OS 標準のデータの置き場所の下に作るディレクトリの名前。
const dirName = "sorus"

// AppPaths は、デスクトップモードのアプリ全体で1つだけのファイル・ディレクトリの場所。どれもデータディレクトリの
// 直下に置く。サイトごとのデータの場所は SitePaths。
type AppPaths struct {
	// Dir はデータディレクトリ（DataDir）。1つ目のサイトのデータもここに置く（SitePathsIn(Dir)）。
	Dir string
	// Log は動作の記録（sorus.log）。Windows の配布版にはコンソールが無いため、ログはここにだけ残る。
	Log string
	// Instance は起動中のインスタンスの情報（instance.json: pid と、最後に使っていたサイトの port）。2つ目の起動が
	// 既存のものを探すのに使う。
	Instance string
	// Lock は同じデータディレクトリを2つの sorus が同時に使わないようにするロックファイル（instance.lock）。
	Lock string
	// Sites はサイトの一覧（sites.json）。2つ目のサイトを作ったときに初めて作る（Registry）。
	Sites string
	// DeletedBackups は、自動バックアップのフォルダが決まっていないサイトを削除するとき、削除の前の完全バックアップを
	// 置くディレクトリ（削除したサイトのバックアップ/）。
	DeletedBackups string
}

// AppPathsIn はデータディレクトリ dir の中の、アプリ全体のファイル・ディレクトリの場所を返す。
func AppPathsIn(dir string) AppPaths {
	return AppPaths{
		Dir:            dir,
		Log:            filepath.Join(dir, "sorus.log"),
		Instance:       filepath.Join(dir, "instance.json"),
		Lock:           filepath.Join(dir, "instance.lock"),
		Sites:          filepath.Join(dir, sitesFileName),
		DeletedBackups: filepath.Join(dir, "削除したサイトのバックアップ"),
	}
}

// DefaultAppPaths は DataDir の中の、アプリ全体のファイル・ディレクトリの場所を返す。
func DefaultAppPaths() (AppPaths, error) {
	dir, err := DataDir()
	if err != nil {
		return AppPaths{}, err
	}
	return AppPathsIn(dir), nil
}

// Ensure はデータディレクトリを作る。他の利用者から読めないよう、所有者だけに権限を与える。
// 中のディレクトリ（uploads/ など）は、それぞれを使う処理が作る。
func (p AppPaths) Ensure() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("desktop: create data directory: %w", err)
	}
	return nil
}

// Site はサイト s のファイル・ディレクトリの場所を返す（s.Dir はデータディレクトリからの相対パス）。
func (p AppPaths) Site(s Site) SitePaths {
	return SitePathsIn(filepath.Join(p.Dir, filepath.FromSlash(s.Dir)))
}

// RecoveryFileName はパスワード再設定コードのファイルの名前。サイトのディレクトリに置く（server.RecoveryFileName と同じ）。
const RecoveryFileName = "パスワード再設定コード.txt"

// SitePaths は1つのサイトのファイル・ディレクトリの場所。書き出し・バックアップなど、サイトのデータを扱う機能は
// ここから場所を得る。1つ目のサイトのディレクトリはデータディレクトリそのもの、2つ目からは sites/<id>/。
type SitePaths struct {
	// Dir はサイトのディレクトリ。
	Dir string
	// DB は SQLite のデータベース（database.db）。
	DB string
	// Uploads はアップロード画像（uploads/）。
	Uploads string
	// Attachments は添付資料（attachments/）。
	Attachments string
	// SearchIndex は全文検索インデックス（search_index/）。
	SearchIndex string
	// Backups はコマンドの sorus backup が保存先を省略したときのバックアップの置き場所（backups/）。
	Backups string
	// Recovery はパスワード再設定コードのファイル（RecoveryFileName）。
	Recovery string
	// Themes は取り込んだカスタムテーマの置き場所（themes/<id>/）。
	Themes string
}

// SitePathsIn はサイトのディレクトリ dir の中の各ファイル・ディレクトリの場所を返す。
func SitePathsIn(dir string) SitePaths {
	return SitePaths{
		Dir:         dir,
		DB:          filepath.Join(dir, "database.db"),
		Uploads:     filepath.Join(dir, "uploads"),
		Attachments: filepath.Join(dir, "attachments"),
		SearchIndex: filepath.Join(dir, "search_index"),
		Backups:     filepath.Join(dir, "backups"),
		Recovery:    filepath.Join(dir, RecoveryFileName),
		Themes:      filepath.Join(dir, "themes"),
	}
}

// RemoveSite はサイト s のデータを消す。2つ目からのサイトは、そのディレクトリ（sites/<id>/）ごと消す。
// 1つ目のサイト（データディレクトリそのもの）は、データディレクトリとアプリのファイル（sites.json・sites/ など）には
// 触れず、サイトのデータとして決めたものだけを消す: DB とその -wal・-shm、uploads/・attachments/・search_index/、
// パスワード再設定コードのファイル。無いものは飛ばし、消せなかったものはまとめてエラーにする。
func (p AppPaths) RemoveSite(s Site) error {
	if s.Dir != "." {
		dir := p.Site(s).Dir
		// 壊れた sites.json がデータディレクトリの外や sites/ そのものを指していても、消さない。
		if filepath.Dir(dir) != filepath.Join(p.Dir, sitesDirName) {
			return fmt.Errorf("desktop: refusing to remove %s: not a site directory", dir)
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("desktop: remove site: %w", err)
		}
		return nil
	}
	sp := p.Site(s)
	var errs []error
	for _, f := range []string{sp.DB, sp.DB + "-wal", sp.DB + "-shm", sp.Recovery} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, d := range []string{sp.Uploads, sp.Attachments, sp.SearchIndex} {
		if err := os.RemoveAll(d); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("desktop: remove site: %w", err)
	}
	return nil
}

// PathsRemovedByDelete は RemoveSite が消すパスを返す。削除の前のバックアップを、この中には置かない。
// 2つ目からのサイトは、そのディレクトリ（sites/<id>/）ごと消す。1つ目のサイトは、DB とその -wal・-shm、
// uploads/・attachments/・search_index/、パスワード再設定コードのファイルだけを消す。
func (p AppPaths) PathsRemovedByDelete(s Site) []string {
	sp := p.Site(s)
	if !s.First() {
		return []string{sp.Dir}
	}
	return []string{sp.DB, sp.DB + "-wal", sp.DB + "-shm", sp.Recovery, sp.Uploads, sp.Attachments, sp.SearchIndex}
}

// DeleteBackupDir は、削除の前の完全バックアップを置くディレクトリを返す。configured が空か、removed の
// どれかの中（そのパス自身を含む）なら、消えてしまわないよう fallback を返す。比べるときは絶対パスにして
// 整え、実在する一番長い部分のシンボリックリンクを解決する。大文字小文字の違いと、実在するパスが同じ
// ファイルである場合も中とみなす（迷ったときは fallback に置く）。
func DeleteBackupDir(configured, fallback string, removed []string) string {
	if strings.TrimSpace(configured) == "" || pathInsideAny(configured, removed) {
		return fallback
	}
	return configured
}

// canonicalPath は path を絶対パスにして整える。実在する一番長い部分までシンボリックリンクを解決し、
// まだ無い部分はその後ろに付け直す。
func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	rest := ""
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return filepath.Clean(resolved)
			}
			return filepath.Clean(filepath.Join(resolved, rest))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
	}
}

// pathInsideAny は path が roots のどれかと同じか、その中にあるかを返す。
func pathInsideAny(path string, roots []string) bool {
	cleaned := canonicalPath(path)
	for _, root := range roots {
		if pathInside(cleaned, canonicalPath(root)) {
			return true
		}
	}
	return false
}

// pathInside は path と root が、どちらも canonicalPath で整えたあとのパスであるとして、path が root と同じか
// その中にあるかを返す。大文字小文字は区別しない（大文字小文字を区別しないファイルシステムで、削除の範囲を
// すり抜けないようにする。区別するファイルシステムでは、違うフォルダを中とみなして fallback に置く）。
// 文字列が違っても、実在する path かその親が root と同じファイルなら中とみなす。
func pathInside(path, root string) bool {
	if strings.EqualFold(path, root) {
		return true
	}
	if relInside(root, path) || relInside(strings.ToLower(root), strings.ToLower(path)) {
		return true
	}
	return sameFileInside(path, root)
}

// relInside は path が root と同じか、その中にあるかを返す。root と path は同じ大きさの文字で揃えておく。
func relInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sameFileInside は、path かその実在する親が root と同じファイルなら true を返す。
// まだ無い末尾は、実在する親が root と同じなら root の中とみなす。
func sameFileInside(path, root string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	cur := path
	for {
		if info, err := os.Stat(cur); err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// DataDir はデスクトップモードのデータディレクトリを返す。環境変数 SORUS_DATA_DIR があればその場所を使い、
// 無ければ OS 標準の利用者ごとの場所を使う。
//
//   - Windows: %LOCALAPPDATA%\sorus
//   - macOS: ~/Library/Application Support/sorus
//   - Linux など: $XDG_DATA_HOME/sorus（未設定なら ~/.local/share/sorus）
//
// カレントディレクトリや ENV_FILE には依存しない。
func DataDir() (string, error) {
	return dataDirFor(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// dataDirFor は goos の OS での DataDir を、環境変数 getenv とホームディレクトリ home から求める。
func dataDirFor(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if dir := getenv(DataDirEnv); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("desktop: %s: %w", DataDirEnv, err)
		}
		return abs, nil
	}
	switch goos {
	case "windows":
		if local := getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, dirName), nil
		}
		h, err := home()
		if err != nil {
			return "", errors.Join(errors.New("desktop: LOCALAPPDATA is not set"), err)
		}
		return filepath.Join(h, "AppData", "Local", dirName), nil
	case "darwin":
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("desktop: home directory: %w", err)
		}
		return filepath.Join(h, "Library", "Application Support", dirName), nil
	default:
		// XDG Base Directory の決まりどおり、相対パスの XDG_DATA_HOME は無視する。
		if xdg := getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
			return filepath.Join(xdg, dirName), nil
		}
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("desktop: home directory: %w", err)
		}
		return filepath.Join(h, ".local", "share", dirName), nil
	}
}
