// Package backup は、データベースとアップロード画像・添付資料をまとめた完全バックアップ（Zip アーカイブ）の
// 作成と復元、決めたフォルダへの定期的な自動バックアップ（Scheduler）を提供する。
//
// アーカイブには次の4種類のエントリだけを入れる。
//
//	manifest.json   形式・作成日時・スキーマバージョン・DB のサイズと SHA-256・画像と添付資料の件数を記録したメタデータ
//	database.db     稼働中の DB を VACUUM INTO で書き出した、整合性の取れた単一ファイルのコピー
//	                （ログインセッションは除く）
//	uploads/...     アップロード画像のディレクトリ（UPLOAD_DIR）の中身
//	attachments/... 添付資料のディレクトリ（ATTACHMENT_DIR）の中身。形式のバージョン2から。添付資料を含めずに
//	                作ったアーカイブ（Create）は、以前の sorus でも復元できるようバージョン1のまま作る
//
// 復元では、展開の前にすべてのエントリ名を検査し、展開先のディレクトリの外へ書き出させるエントリ
// （Zip Slip）、シンボリックリンク、想定外のエントリを含むアーカイブを拒否する。
package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/database"
)

const (
	// FormatName は manifest.json の format に記録する、sorus の完全バックアップであることを示す名前。
	FormatName = "sorus-backup"
	// FormatVersion はこのパッケージが作成するアーカイブ形式の最新のバージョン（添付資料を含む）。
	// 復元はこれ以下のバージョンを受け付ける。
	FormatVersion = 2
	// formatVersionNoAttachments は添付資料を含めないアーカイブの形式のバージョン。
	formatVersionNoAttachments = 1

	// ManifestName・DatabaseName・UploadsDir・AttachmentsDir はアーカイブ内のエントリ名。
	ManifestName   = "manifest.json"
	DatabaseName   = "database.db"
	UploadsDir     = "uploads"
	AttachmentsDir = "attachments"

	// PreRestoreInfix は、復元で置き換える前のデータを退避するときに元の名前の後ろに付ける文字列。
	// 退避先は <元のパス>.pre-restore-YYYYMMDD_HHMMSS（UTC）になる。
	PreRestoreInfix = ".pre-restore-"

	// fileNameLayout は FileName が使う time.Format のレイアウト。
	fileNameLayout = "sorus_backup_20060102_150405.zip"
	// stampLayout は退避先の名前に付ける日時のレイアウト。
	stampLayout = "20060102_150405"
	// maxManifestSize は manifest.json として読み込む上限バイト数。
	maxManifestSize = 1 << 20
	// dirMode・dbFileMode・uploadFileMode は作成するディレクトリ・DB ファイル・画像ファイルのパーミッション。
	// DB にはパスワードハッシュやセッション ID が含まれるため所有者のみに限る。画像は公開配信するため
	// image.Process が保存するファイルと同じにする。
	dirMode        os.FileMode = 0o700
	dbFileMode     os.FileMode = 0o600
	uploadDirMode  os.FileMode = 0o755
	uploadFileMode os.FileMode = 0o644
	// attachDirMode・attachFileMode は添付資料のディレクトリ・ファイルのパーミッション。添付資料は静的配信しないため、
	// サーバーが保存するときと同じく所有者とグループだけに権限を与える。
	attachDirMode  os.FileMode = 0o750
	attachFileMode os.FileMode = 0o640
)

var (
	// ErrInvalidArchive は、アーカイブが壊れている・Zip 形式でない・manifest.json と中身が一致しないなど、
	// sorus の完全バックアップとして読めないことを表す。
	ErrInvalidArchive = errors.New("backup: invalid backup archive")
	// ErrUnsafePath は、展開先の外を指すエントリ名（Zip Slip）やシンボリックリンクを含むことを表す。
	ErrUnsafePath = errors.New("backup: unsafe path in backup archive")
	// ErrIncompatible は、この sorus より新しい形式・スキーマのバックアップであることを表す。
	ErrIncompatible = errors.New("backup: backup archive is not compatible with this version")
	// ErrDatabaseInUse は、復元先の DB を他のプロセス（稼働中のサーバーなど）が開いていることを表す。
	ErrDatabaseInUse = errors.New("backup: database is in use by another process")
)

// Manifest はアーカイブの manifest.json の内容。
type Manifest struct {
	Format        string       `json:"format"`
	FormatVersion int          `json:"format_version"`
	CreatedAt     time.Time    `json:"created_at"`
	AppVersion    string       `json:"app_version"`
	SchemaVersion int          `json:"schema_version"`
	Database      DatabaseInfo `json:"database"`
	Uploads       UploadsInfo  `json:"uploads"`
	// Attachments は添付資料の情報。形式のバージョン1のアーカイブには無い。
	Attachments *UploadsInfo `json:"attachments,omitempty"`
}

// DatabaseInfo はアーカイブ内の DB ファイルの情報。
type DatabaseInfo struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// UploadsInfo はアーカイブ内のアップロード画像（または添付資料）の情報。
type UploadsInfo struct {
	FileCount int   `json:"file_count"`
	TotalSize int64 `json:"total_size"`
}

// FileName は t 時点の完全バックアップのファイル名（sorus_backup_YYYYMMDD_HHMMSS.zip）を返す。
// 名前の辞書順が作成順と一致するよう、時刻は UTC で表す。
func FileName(t time.Time) string {
	return t.UTC().Format(fileNameLayout)
}

// CreateOptions は CreateWith に渡す、アーカイブに含めるディレクトリと作業場所。
type CreateOptions struct {
	// UploadDir はアップロード画像のディレクトリ（必須）。
	UploadDir string
	// AttachmentDir は添付資料のディレクトリ。空なら添付資料を含めず、形式のバージョン1のアーカイブを作る。
	AttachmentDir string
	// WorkDir は DB の一時的な複製を置くディレクトリ。空なら destZipPath と同じディレクトリを使う。
	// クラウドの同期フォルダに保存するときは、ログインセッションを消す前の複製を同期させないよう、
	// 同期されない場所（OS の一時ディレクトリなど）を指定する。
	WorkDir string
}

// Create は稼働中の db を VACUUM INTO で複製し、uploadDir の中身と manifest.json を加えた Zip アーカイブを
// destZipPath に作成する。添付資料は含めない（CreateWith を参照）。
func Create(db *sqlx.DB, uploadDir, destZipPath string) error {
	return CreateWith(db, destZipPath, CreateOptions{UploadDir: uploadDir})
}

// CreateWith は稼働中の db を VACUUM INTO で複製し、opts のアップロード画像・添付資料のディレクトリの中身と
// manifest.json を加えた Zip アーカイブを destZipPath に作成する。
//
// 書き出しは同じディレクトリの一時ファイルに行い、完了後に destZipPath へ移すので、途中で失敗しても
// destZipPath に壊れたファイルは残らない（クラウドの同期アプリにも書きかけのファイルを見せない）。
// アーカイブは所有者のみ読み書きできるパーミッション（0600）で作る。
// 親ディレクトリが無ければ作成し、destZipPath が既に存在すれば上書きせずエラーを返す。
// 画像・添付資料のディレクトリが存在しなければ0件として扱う。中のシンボリックリンクは辿らず、アーカイブに含めない。
// セッション ID はそのままログインに使えるため、複製した DB からログインセッションを消してからアーカイブする
// （復元後は全員がログインし直す）。
func CreateWith(db *sqlx.DB, destZipPath string, opts CreateOptions) error {
	if db == nil {
		return errors.New("backup: create: nil db")
	}
	if opts.UploadDir == "" || destZipPath == "" {
		return errors.New("backup: create: empty uploadDir or destZipPath")
	}
	dest, err := filepath.Abs(destZipPath)
	if err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("backup: create: create dir: %w", err)
	}
	if err := ensureAbsent(dest); err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	// UPLOAD_DIR 自体がシンボリックリンク（別ディスクへのリンクなど）でも中身を辿れるよう実体のパスにする。
	uploads, err := resolvePath(opts.UploadDir)
	if err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	attachments := ""
	if opts.AttachmentDir != "" {
		if attachments, err = resolvePath(opts.AttachmentDir); err != nil {
			return fmt.Errorf("backup: create: %w", err)
		}
	}
	workDir := dir
	if opts.WorkDir != "" {
		workDir = opts.WorkDir
	}

	work, err := os.MkdirTemp(workDir, ".sorus-backup-*")
	if err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	defer os.RemoveAll(work)
	snapshot := filepath.Join(work, DatabaseName)
	if err := database.Backup(db, snapshot); err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	if err := removeSessions(snapshot); err != nil {
		return fmt.Errorf("backup: create: remove sessions: %w", err)
	}
	var schemaVersion int
	if err := db.Get(&schemaVersion, "PRAGMA user_version"); err != nil {
		return fmt.Errorf("backup: create: read schema version: %w", err)
	}

	// 一時ファイルの名前は "." で始めて ".tmp" で終える（同期アプリの多くが一時ファイルとして扱う）。
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	tmpPath := tmp.Name()
	published := false
	defer func() {
		if !published {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()
	if err := writeArchive(tmp, snapshot, uploads, attachments, schemaVersion); err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	if beforePublish != nil {
		beforePublish(tmpPath, dest)
	}
	if err := publish(tmpPath, dest); err != nil {
		return fmt.Errorf("backup: create: %w", err)
	}
	published = true
	return nil
}

// beforePublish はテスト用。書き終えた一時ファイルを dest へ移す直前に呼ぶ。
var beforePublish func(tmpPath, dest string)

// removeSessions は DB のスナップショット path からログインセッションを消す。secure_delete を有効にして、
// 消した行の内容（セッション ID）を空き領域にも残さない。
func removeSessions(path string) error {
	db, err := sqlx.Open(database.DriverName, path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`); err != nil || n == 0 {
		return err
	}
	if _, err := db.Exec("PRAGMA secure_delete = ON"); err != nil {
		return err
	}
	_, err = db.Exec("DELETE FROM sessions")
	return err
}

// writeArchive は DB のスナップショット snapshot と uploadDir の中身、manifest.json を Zip として w に書く。
// attachmentDir が空でなければ、その中身も attachments/ として加え、形式のバージョン2にする。
func writeArchive(w io.Writer, snapshot, uploadDir, attachmentDir string, schemaVersion int) error {
	zw := zip.NewWriter(w)
	m := Manifest{
		Format:        FormatName,
		FormatVersion: formatVersionNoAttachments,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
		AppVersion:    appVersion(),
		SchemaVersion: schemaVersion,
	}

	sum, size, err := addFile(zw, DatabaseName, snapshot, m.CreatedAt, dbFileMode)
	if err != nil {
		return err
	}
	m.Database = DatabaseInfo{Path: DatabaseName, Size: size, SHA256: sum}

	if m.Uploads, err = addTree(zw, UploadsDir, uploadDir, m.CreatedAt, uploadDirMode, uploadFileMode); err != nil {
		return fmt.Errorf("archive uploads: %w", err)
	}
	if attachmentDir != "" {
		info, err := addTree(zw, AttachmentsDir, attachmentDir, m.CreatedAt, attachDirMode, attachFileMode)
		if err != nil {
			return fmt.Errorf("archive attachments: %w", err)
		}
		m.FormatVersion = FormatVersion
		m.Attachments = &info
	}

	mw, err := zw.CreateHeader(&zip.FileHeader{Name: ManifestName, Method: zip.Deflate, Modified: m.CreatedAt})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	return zw.Close()
}

// addTree はディレクトリ src の中身を prefix/ 以下のエントリとしてアーカイブに追加し、ファイルの件数と合計サイズを返す。
// src が存在しなければ prefix/ のエントリだけを追加する。シンボリックリンク等は辿らない（src の外のファイルを含めない）。
func addTree(zw *zip.Writer, prefix, src string, created time.Time, dMode, fMode os.FileMode) (UploadsInfo, error) {
	var info UploadsInfo
	if err := addDir(zw, prefix+"/", created, dMode); err != nil {
		return info, err
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == src && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if p == src {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		name := prefix + "/" + filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return addDir(zw, name+"/", fi.ModTime(), dMode)
		case d.Type().IsRegular():
			_, n, err := addFile(zw, name, p, fi.ModTime(), fMode)
			if err != nil {
				return err
			}
			info.FileCount++
			info.TotalSize += n
		}
		return nil
	})
	return info, err
}

// addFile は src をアーカイブに name として追加し、内容の SHA-256（16進）とバイト数を返す。
func addFile(zw *zip.Writer, name, src string, modified time.Time, mode os.FileMode) (string, int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := &zip.FileHeader{Name: name, Method: compressionMethod(name), Modified: modified}
	h.SetMode(mode)
	w, err := zw.CreateHeader(h)
	if err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hash), f)
	if err != nil {
		return "", 0, fmt.Errorf("archive %s: %w", name, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

// addDir はディレクトリのエントリ name（末尾は "/"）をアーカイブに追加する。
func addDir(zw *zip.Writer, name string, modified time.Time, mode os.FileMode) error {
	h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: modified}
	h.SetMode(fs.ModeDir | mode)
	_, err := zw.CreateHeader(h)
	return err
}

// compressionMethod は圧縮済みの画像形式を再圧縮せずに格納し、それ以外を Deflate で圧縮する。
func compressionMethod(name string) uint16 {
	switch strings.ToLower(path.Ext(name)) {
	case ".webp", ".jpg", ".jpeg", ".png", ".gif", ".avif", ".zip", ".gz":
		return zip.Store
	}
	return zip.Deflate
}

// appVersion はビルド情報からこの sorus のバージョン（取れなければ "devel"）を返す。
func appVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	v := bi.Main.Version
	if v == "" || v == "(devel)" {
		v = "devel"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			v += "+" + s.Value[:12]
		}
	}
	return v
}

// ensureAbsent は path に何も無いことを確かめる。
func ensureAbsent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// publish は書き出しを終えた一時ファイル tmpPath を dest に移す。dest が既に存在すれば上書きしない。
func publish(tmpPath, dest string) error {
	// ハードリンクの作成は dest が既にあれば os.ErrExist で失敗するため、上書きせずに原子的に確保できる。
	if err := os.Link(tmpPath, dest); err == nil {
		return os.Remove(tmpPath)
	} else if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists", dest)
	}
	// ハードリンクを作れないファイルシステム向けのフォールバック。
	if err := ensureAbsent(dest); err != nil {
		return err
	}
	return os.Rename(tmpPath, dest)
}

// resolvePath は p を絶対パスにし、存在すればシンボリックリンクを解決した実体のパスを返す。
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return abs, nil
	}
	return real, err
}
