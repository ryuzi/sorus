package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jmoiron/sqlx"
)

// backupDirMode はバックアップ先ディレクトリを新規作成するときのパーミッション。
// バックアップにはパスワードハッシュやセッションIDが含まれるため所有者のみに限る。
const backupDirMode os.FileMode = 0o700

// backupNameLayout は BackupFileName が使う time.Format のレイアウト。
const backupNameLayout = "backup_20060102_150405.db"

// backupNamePattern は RotateBackups が整理対象とするファイル名（BackupFileName の形式）。
var backupNamePattern = regexp.MustCompile(`^backup_\d{8}_\d{6}\.db$`)

// BackupFileName は t 時点の定期バックアップのファイル名（backup_YYYYMMDD_HHMMSS.db）を返す。
// 名前の辞書順が作成順と一致するよう、時刻は UTC で表す。
func BackupFileName(t time.Time) string {
	return t.UTC().Format(backupNameLayout)
}

// Backup は稼働中の db を VACUUM INTO で destPath に複製する。
//
// VACUUM INTO は読み取りトランザクションのスナップショットを書き出すため、WAL モードでは
// 他の接続の読み書きを止めずに、整合性の取れた単一ファイルのコピーを作れる。
// 書き出しは同じディレクトリの一時ファイルに行い、完了後に destPath へリネームするので、
// 途中で失敗しても destPath に壊れたファイルは残らない。バックアップは所有者のみ読み書きできる
// パーミッション（0600）で作成する。親ディレクトリが無ければ作成し、destPath が既に存在すれば
// 上書きせずエラーを返す。
func Backup(db *sqlx.DB, destPath string) error {
	if db == nil {
		return errors.New("database: backup: nil db")
	}
	if destPath == "" {
		return errors.New("database: backup: empty destPath")
	}
	// "file:" で始まる相対パスが URI として解釈されないよう絶対パスにする。
	dest, err := filepath.Abs(destPath)
	if err != nil {
		return fmt.Errorf("database: backup: %w", err)
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, backupDirMode); err != nil {
		return fmt.Errorf("database: backup: create dir: %w", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("database: backup: %s already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("database: backup: %w", err)
	}

	// VACUUM INTO は空の既存ファイルにも書き込めるため、0600 で作った一時ファイルを渡す。
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return fmt.Errorf("database: backup: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("database: backup: %w", err)
	}

	if _, err := db.Exec("VACUUM INTO ?", tmpPath); err != nil {
		return fmt.Errorf("database: backup: vacuum into %s: %w", tmpPath, err)
	}
	if err := syncFile(tmpPath); err != nil {
		return fmt.Errorf("database: backup: %w", err)
	}
	// 既存宛先への上書きを防ぐため、Link（ハードリンク作成による原子的確保）を試みる。
	// 同一ディレクトリ内に作られた一時ファイルなので同一FS内であり、dest が既に存在する場合は
	// os.ErrExist で原子的に失敗する。
	if err := os.Link(tmpPath, dest); err == nil {
		_ = os.Remove(tmpPath)
		renamed = true
		return nil
	} else if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("database: backup: %s already exists", dest)
	}

	// Link がサポートされない環境（例: 特定の仮想ファイルシステム）向けのフォールバック。
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("database: backup: %s already exists", dest)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("database: backup: %w", err)
	}
	renamed = true
	return nil
}

// syncFile は path の内容をストレージへ確実に書き出す。
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RotateBackups は backupDir の定期バックアップ（BackupFileName 形式の名前のファイル）のうち、
// 新しい keepCount 世代を残して古いものを削除する。それ以外の名前のファイル（手動バックアップや
// 書き込み途中の一時ファイルなど）には触れない。backupDir が存在しなければ何もしない。
// keepCount が 1 未満ならエラーを返す。
func RotateBackups(backupDir string, keepCount int) error {
	if keepCount < 1 {
		return fmt.Errorf("database: rotate backups: keepCount must be at least 1, got %d", keepCount)
	}
	entries, err := os.ReadDir(backupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("database: rotate backups: %w", err)
	}

	// os.ReadDir はファイル名順に返し、BackupFileName の名前は辞書順が作成順と一致する。
	var backups []string
	for _, e := range entries {
		if e.Type().IsRegular() && backupNamePattern.MatchString(e.Name()) {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) <= keepCount {
		return nil
	}
	var errs []error
	for _, name := range backups[:len(backups)-keepCount] {
		if err := os.Remove(filepath.Join(backupDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("database: rotate backups: %w", err)
	}
	return nil
}
