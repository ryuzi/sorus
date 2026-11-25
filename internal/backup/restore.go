package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"sorus/internal/database"
)

// 展開サイズの上限（Zip Bomb 対策）。圧縮率の高いエントリで展開先のディスクを埋められないよう、申告された
// 展開後のサイズを inspect で、実際に展開したバイト数を extractFile で調べる。テストで小さくできるよう変数にしている。
var (
	// maxRestoreEntrySize は1エントリの展開後の上限（4GB。管理画面から復元できるファイルの既定の上限と同じ）。
	maxRestoreEntrySize uint64 = 4 << 30
	// maxRestoreTotalSize は全エントリの展開後の合計の上限（8GB）。DB は圧縮がよく効くため、アップロードの上限
	// （4GB）より余裕を持たせる。
	maxRestoreTotalSize uint64 = 8 << 30
)

// archive は検査を終えたバックアップアーカイブ。
type archive struct {
	zr       *zip.ReadCloser
	manifest Manifest
	db       *zip.File
	// uploads は uploads/ 以下のエントリ。rel はそのエントリの uploads/ からの相対パス（スラッシュ区切り）。
	uploads []uploadEntry
	// attachments は attachments/ 以下のエントリ（形式のバージョン2から）。
	attachments []uploadEntry
}

type uploadEntry struct {
	file *zip.File
	rel  string
}

func (a *archive) Close() error { return a.zr.Close() }

// ReadManifest は zipPath のアーカイブを Restore と同じ基準で検査し、manifest.json の内容を返す。
// ファイルは展開しない。
func ReadManifest(zipPath string) (*Manifest, error) {
	a, err := openArchive(zipPath)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	m := a.manifest
	return &m, nil
}

// openArchive は zipPath を開き、すべてのエントリ名と manifest.json を検査する。
func openArchive(zipPath string) (*archive, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("backup: open %s: %w", zipPath, err)
		}
		return nil, fmt.Errorf("%w: cannot read zip: %v", ErrInvalidArchive, err)
	}
	a := &archive{zr: zr}
	if err := a.inspect(); err != nil {
		zr.Close()
		return nil, err
	}
	return a, nil
}

// inspect はエントリを分類し、manifest.json と実際の中身が一致することを確かめる。
func (a *archive) inspect() error {
	var manifest *zip.File
	seen := make(map[string]bool, len(a.zr.File))
	var uploadFiles, attachFiles int
	var uploadBytes, attachBytes int64
	var totalBytes uint64
	hasAttachments := false
	for _, f := range a.zr.File {
		name, isDir, err := checkEntry(f)
		if err != nil {
			return err
		}
		if f.UncompressedSize64 > maxRestoreEntrySize {
			return fmt.Errorf("%w: entry %q is too large (%d bytes, limit %d)", ErrInvalidArchive, f.Name,
				f.UncompressedSize64, maxRestoreEntrySize)
		}
		// 1エントリずつ上限を超えないか調べるので、加算はあふれない。
		totalBytes += f.UncompressedSize64
		if totalBytes > maxRestoreTotalSize {
			return fmt.Errorf("%w: total uncompressed size exceeds the limit of %d bytes", ErrInvalidArchive, maxRestoreTotalSize)
		}
		if seen[name] {
			return fmt.Errorf("%w: duplicate entry %q", ErrInvalidArchive, f.Name)
		}
		seen[name] = true
		switch {
		case name == ManifestName && !isDir:
			manifest = f
		case name == DatabaseName && !isDir:
			a.db = f
		case name == UploadsDir && isDir:
			// uploads/ 自体のエントリ。画像0件のバックアップでもディレクトリを表す。
		case strings.HasPrefix(name, UploadsDir+"/"):
			a.uploads = append(a.uploads, uploadEntry{file: f, rel: strings.TrimPrefix(name, UploadsDir+"/")})
			if !isDir {
				uploadFiles++
				uploadBytes += int64(f.UncompressedSize64)
			}
		case name == AttachmentsDir && isDir:
			hasAttachments = true
		case strings.HasPrefix(name, AttachmentsDir+"/"):
			hasAttachments = true
			a.attachments = append(a.attachments, uploadEntry{file: f, rel: strings.TrimPrefix(name, AttachmentsDir+"/")})
			if !isDir {
				attachFiles++
				attachBytes += int64(f.UncompressedSize64)
			}
		default:
			return fmt.Errorf("%w: unexpected entry %q", ErrInvalidArchive, f.Name)
		}
	}
	if manifest == nil {
		return fmt.Errorf("%w: %s not found", ErrInvalidArchive, ManifestName)
	}
	if a.db == nil {
		return fmt.Errorf("%w: %s not found", ErrInvalidArchive, DatabaseName)
	}
	if err := a.readManifest(manifest); err != nil {
		return err
	}

	m := a.manifest
	if m.Format != FormatName {
		return fmt.Errorf("%w: unknown format %q", ErrInvalidArchive, m.Format)
	}
	if m.FormatVersion < 1 {
		return fmt.Errorf("%w: invalid format_version %d", ErrInvalidArchive, m.FormatVersion)
	}
	if m.FormatVersion > FormatVersion {
		return fmt.Errorf("%w: format_version %d is newer than supported %d", ErrIncompatible, m.FormatVersion, FormatVersion)
	}
	if m.SchemaVersion < 1 {
		return fmt.Errorf("%w: invalid schema_version %d", ErrInvalidArchive, m.SchemaVersion)
	}
	if m.SchemaVersion > database.SchemaVersion() {
		return fmt.Errorf("%w: schema_version %d is newer than supported %d", ErrIncompatible, m.SchemaVersion, database.SchemaVersion())
	}
	if m.Database.Path != DatabaseName || m.Database.Size < 0 || uint64(m.Database.Size) != a.db.UncompressedSize64 ||
		len(m.Database.SHA256) != sha256.Size*2 {
		return fmt.Errorf("%w: database entry does not match %s", ErrInvalidArchive, ManifestName)
	}
	if m.Uploads.FileCount != uploadFiles || m.Uploads.TotalSize != uploadBytes {
		return fmt.Errorf("%w: uploads (%d files, %d bytes) do not match %s (%d files, %d bytes)", ErrInvalidArchive,
			uploadFiles, uploadBytes, ManifestName, m.Uploads.FileCount, m.Uploads.TotalSize)
	}
	// 添付資料は形式のバージョン2から。バージョン1のアーカイブ（添付資料を含まない）はそのまま受け付ける。
	switch {
	case m.Attachments == nil && hasAttachments:
		return fmt.Errorf("%w: attachments are not listed in %s", ErrInvalidArchive, ManifestName)
	case m.Attachments != nil && m.FormatVersion < 2:
		return fmt.Errorf("%w: attachments require format_version 2", ErrInvalidArchive)
	case m.Attachments != nil && (m.Attachments.FileCount != attachFiles || m.Attachments.TotalSize != attachBytes):
		return fmt.Errorf("%w: attachments (%d files, %d bytes) do not match %s (%d files, %d bytes)", ErrInvalidArchive,
			attachFiles, attachBytes, ManifestName, m.Attachments.FileCount, m.Attachments.TotalSize)
	}
	return nil
}

// RestoreTargets は復元先の場所。
type RestoreTargets struct {
	// DBPath は停止中の sorus の DB ファイル（Restore だけで使う。RestoreLive は稼働中の DB に書き写す）。
	DBPath string
	// UploadDir はアップロード画像のディレクトリ（必須）。
	UploadDir string
	// AttachmentDir は添付資料のディレクトリ。空、またはアーカイブに添付資料が無い（形式のバージョン1）ときは
	// 添付資料のディレクトリには触れない。
	AttachmentDir string
}

func (a *archive) readManifest(f *zip.File) error {
	if f.UncompressedSize64 > maxManifestSize {
		return fmt.Errorf("%w: %s is too large", ErrInvalidArchive, ManifestName)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("%w: open %s: %v", ErrInvalidArchive, ManifestName, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxManifestSize+1))
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrInvalidArchive, ManifestName, err)
	}
	if err := json.Unmarshal(data, &a.manifest); err != nil {
		return fmt.Errorf("%w: parse %s: %v", ErrInvalidArchive, ManifestName, err)
	}
	return nil
}

// checkEntry はエントリ名が展開先の中に収まる安全な相対パスかを検査し、末尾の "/" を除いた名前と、
// ディレクトリのエントリかを返す。
//
// Zip Slip を防ぐため、絶対パス、".." や "." を含むパス、空の要素（"a//b"）、正規化すると変わるパス、
// バックスラッシュ（Windows の区切り文字）、ドライブ指定や代替データストリームに使われるコロン、NUL 文字を拒否する。
// 通常のファイルとディレクトリ以外（シンボリックリンク・デバイスなど）のエントリも拒否する。
func checkEntry(f *zip.File) (string, bool, error) {
	name := f.Name
	isDir := strings.HasSuffix(name, "/")
	clean := strings.TrimSuffix(name, "/")
	unsafe := func(reason string) (string, bool, error) {
		return "", false, fmt.Errorf("%w: %q (%s)", ErrUnsafePath, name, reason)
	}
	if clean == "" || strings.ContainsAny(clean, "\\:\x00") {
		return unsafe("invalid characters")
	}
	if path.IsAbs(clean) || path.Clean(clean) != clean {
		return unsafe("not a clean relative path")
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return unsafe("path traversal")
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(clean)) {
		return unsafe("not a local path")
	}
	mode := f.Mode()
	if mode.IsDir() != isDir || (!mode.IsDir() && !mode.IsRegular()) {
		return unsafe("not a regular file or directory")
	}
	return clean, isDir, nil
}

// safeJoin は rel を base の中のパスに変換する。checkEntry を通った名前でも、結合後のパスが base の外を
// 指していないことを改めて確かめる。
func safeJoin(base, rel string) (string, error) {
	base = filepath.Clean(base)
	target := filepath.Join(base, filepath.FromSlash(rel))
	if !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q escapes the destination", ErrUnsafePath, rel)
	}
	return target, nil
}

// staged は検査済みのアーカイブを、復元先と同じディレクトリの一時領域に展開したもの。
type staged struct {
	db          string // 展開した DB ファイル
	uploads     string // 展開した uploads ディレクトリ
	attachments string // 展開した attachments ディレクトリ（添付資料を復元しないときは空）
}

// cleanup は差し替えに使わなかった一時領域を消す。差し替え済みのパスは存在しないので何もしない。
func (s *staged) cleanup() {
	if s.db != "" {
		os.Remove(s.db)
	}
	if s.uploads != "" {
		os.RemoveAll(s.uploads)
	}
	if s.attachments != "" {
		os.RemoveAll(s.attachments)
	}
}

// stage は DB を dbDir に、uploads を uploadPath と同じディレクトリに、attachments を attachPath と同じディレクトリに
// 一時的な名前で展開し、DB の SHA-256・整合性・スキーマを検査する。差し替えを rename で行えるよう、同じファイル
// システムに置く。attachPath が空、またはアーカイブに添付資料が無ければ添付資料は展開しない。
func (a *archive) stage(dbDir, uploadPath, attachPath string) (_ *staged, err error) {
	st := &staged{}
	defer func() {
		if err != nil {
			st.cleanup()
		}
	}()
	if err := os.MkdirAll(dbDir, dirMode); err != nil {
		return nil, fmt.Errorf("backup: restore: %w", err)
	}
	f, err := os.CreateTemp(dbDir, ".sorus-restore-*.db")
	if err != nil {
		return nil, fmt.Errorf("backup: restore: %w", err)
	}
	st.db = f.Name()
	sum, err := extractFile(a.db, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if sum != strings.ToLower(a.manifest.Database.SHA256) {
		return nil, fmt.Errorf("%w: %s checksum mismatch", ErrInvalidArchive, DatabaseName)
	}
	if err := os.Chmod(st.db, dbFileMode); err != nil {
		return nil, fmt.Errorf("backup: restore: %w", err)
	}
	if err := checkDatabase(st.db, a.manifest.SchemaVersion); err != nil {
		return nil, err
	}

	if st.uploads, err = stageTree(uploadPath, a.uploads, uploadDirMode, uploadFileMode); err != nil {
		return nil, err
	}
	if a.restoresAttachments(attachPath) {
		if st.attachments, err = stageTree(attachPath, a.attachments, attachDirMode, attachFileMode); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// restoresAttachments は、添付資料のディレクトリ attachPath を置き換えるかを返す。
func (a *archive) restoresAttachments(attachPath string) bool {
	return attachPath != "" && a.manifest.Attachments != nil
}

// stageTree は entries を target と同じディレクトリの一時的な名前のディレクトリに展開し、そのパスを返す。
// 失敗したときに作りかけのディレクトリを消すのは呼び出し側（staged.cleanup）なので、作ったパスは常に返す。
func stageTree(target string, entries []uploadEntry, dMode, fMode os.FileMode) (string, error) {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, dMode); err != nil {
		return "", fmt.Errorf("backup: restore: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "."+filepath.Base(target)+".restore-*")
	if err != nil {
		return "", fmt.Errorf("backup: restore: %w", err)
	}
	if err := os.Chmod(dir, dMode); err != nil {
		return dir, fmt.Errorf("backup: restore: %w", err)
	}
	for _, e := range entries {
		if err := extractUpload(dir, e, dMode, fMode); err != nil {
			return dir, err
		}
	}
	return dir, nil
}

// extractUpload は uploads/（または attachments/）以下のエントリ e を base の中に、ディレクトリは dMode、
// ファイルは fMode で展開する。既存のファイルは上書きしない（大文字・小文字だけが違う名前のエントリが、
// 大文字・小文字を区別しないファイルシステムで重なる場合を含む）。
func extractUpload(base string, e uploadEntry, dMode, fMode os.FileMode) error {
	target, err := safeJoin(base, e.rel)
	if err != nil {
		return err
	}
	if e.file.Mode().IsDir() {
		if err := os.MkdirAll(target, dMode); err != nil {
			return fmt.Errorf("backup: restore: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), dMode); err != nil {
		return fmt.Errorf("backup: restore: %w", err)
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: duplicate entry %q", ErrInvalidArchive, e.file.Name)
		}
		return fmt.Errorf("backup: restore: %w", err)
	}
	_, err = extractFile(e.file, out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if !e.file.Modified.IsZero() {
		_ = os.Chtimes(target, e.file.Modified, e.file.Modified)
	}
	return nil
}

// extractFile は f の中身を w に書き出し、SHA-256（16進）を返す。archive/zip は申告より長い中身や
// CRC-32 の不一致をエラーにするため、壊れたエントリはここで検出できる。それに頼らず、申告されたサイズ
// （かつ maxRestoreEntrySize）を1バイトでも超えて読めた時点で展開をやめる（Zip Bomb 対策）。
func extractFile(f *zip.File, w io.Writer) (string, error) {
	limit := min(f.UncompressedSize64, maxRestoreEntrySize)
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("%w: open %s: %v", ErrInvalidArchive, f.Name, err)
	}
	defer rc.Close()
	return copyLimited(f.Name, f.UncompressedSize64, limit, rc, w)
}

// copyLimited は r から最大 limit+1 バイトを w に書き出し、SHA-256（16進）を返す。limit を超えて読めたら
// 展開しすぎとしてエラーに、書き出したバイト数が申告サイズ declared と違えば切り詰めとしてエラーにする。
func copyLimited(name string, declared, limit uint64, r io.Reader, w io.Writer) (string, error) {
	if limit >= math.MaxInt64 {
		limit = math.MaxInt64 - 1
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hash), io.LimitReader(r, int64(limit)+1))
	if err != nil {
		if errors.Is(err, zip.ErrChecksum) || errors.Is(err, zip.ErrFormat) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", fmt.Errorf("%w: %s is corrupted: %v", ErrInvalidArchive, name, err)
		}
		return "", fmt.Errorf("backup: restore: extract %s: %w", name, err)
	}
	if uint64(n) > limit {
		return "", fmt.Errorf("%w: %s is larger than its declared size or the limit of %d bytes", ErrInvalidArchive, name, limit)
	}
	if uint64(n) != declared {
		return "", fmt.Errorf("%w: %s is truncated", ErrInvalidArchive, name)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// checkDatabase は展開した DB が壊れておらず、manifest.json のスキーマバージョンを持つ sorus の DB であることを確かめる。
func checkDatabase(dbPath string, schemaVersion int) error {
	db, err := sqlx.Open(database.DriverName, dbPath+"?_pragma=query_only(1)")
	if err != nil {
		return fmt.Errorf("backup: restore: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var result string
	if err := db.Get(&result, "PRAGMA integrity_check"); err != nil {
		return fmt.Errorf("%w: %s is not a valid database: %v", ErrInvalidArchive, DatabaseName, err)
	}
	if result != "ok" {
		return fmt.Errorf("%w: %s integrity check failed: %s", ErrInvalidArchive, DatabaseName, result)
	}
	var version, tables int
	if err := db.Get(&version, "PRAGMA user_version"); err != nil {
		return fmt.Errorf("%w: read schema version: %v", ErrInvalidArchive, err)
	}
	if version != schemaVersion {
		return fmt.Errorf("%w: database schema version %d does not match %s (%d)", ErrInvalidArchive, version, ManifestName, schemaVersion)
	}
	if err := db.Get(&tables, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('users', 'articles', 'settings')`); err != nil {
		return fmt.Errorf("%w: read schema: %v", ErrInvalidArchive, err)
	}
	if tables != 3 {
		return fmt.Errorf("%w: %s is not a sorus database", ErrInvalidArchive, DatabaseName)
	}
	return nil
}

// Restore は zipPath の完全バックアップを、停止中の sorus の DB ファイル targetDBPath と
// アップロード画像のディレクトリ targetUploadDir に復元する。
//
// アーカイブ全体を検査して一時領域に展開し、DB の整合性を確かめてから差し替えるので、途中で失敗しても
// 現在のデータは変わらない。差し替える前の DB（WAL などの付随ファイルを含む）とディレクトリは、
// 同じ場所に PreRestoreInfix と日時を付けた名前で残す。復元先の DB を他のプロセスが開いている
// （サーバーが稼働中など）場合は ErrDatabaseInUse を返す。稼働中のサーバーへは RestoreLive を使う。
// 添付資料は復元しない（RestoreWith を参照）。
func Restore(zipPath, targetDBPath, targetUploadDir string) error {
	return RestoreWith(zipPath, RestoreTargets{DBPath: targetDBPath, UploadDir: targetUploadDir})
}

// RestoreWith は Restore と同じ手順で、zipPath の完全バックアップを to の DB・アップロード画像・添付資料に復元する。
// 添付資料のディレクトリは、to.AttachmentDir を指定し、アーカイブに添付資料がある（形式のバージョン2）ときだけ
// 置き換え、置き換える前のディレクトリは画像と同じく元の場所に退避する。
func RestoreWith(zipPath string, to RestoreTargets) error {
	if zipPath == "" || to.DBPath == "" || to.UploadDir == "" {
		return errors.New("backup: restore: empty zipPath, targetDBPath or targetUploadDir")
	}
	dbPath, err := resolvePath(to.DBPath)
	if err != nil {
		return fmt.Errorf("backup: restore: %w", err)
	}
	uploadPath, attachPath, err := resolveTargets(to)
	if err != nil {
		return err
	}
	a, err := openArchive(zipPath)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := releaseDatabase(dbPath); err != nil {
		return err
	}
	st, err := a.stage(filepath.Dir(dbPath), uploadPath, attachPath)
	if err != nil {
		return err
	}
	defer st.cleanup()

	stamp := time.Now().UTC().Format(stampLayout)
	var sw swapper
	if exists(dbPath) {
		prev := freePath(dbPath + PreRestoreInfix + stamp)
		// SQLite は付随ファイルを "<DB のパス>-wal" の名前で探すため、DB と同じ名前に揃えて移す。
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if suffix != "" && !exists(dbPath+suffix) {
				continue
			}
			if err := sw.rename(dbPath+suffix, prev+suffix); err != nil {
				return sw.fail(err)
			}
		}
	}
	if err := sw.swapDirs(st, uploadPath, attachPath, stamp); err != nil {
		return sw.fail(err)
	}
	if err := sw.rename(st.db, dbPath); err != nil {
		return sw.fail(err)
	}
	return nil
}

// resolveTargets は to の画像・添付資料のディレクトリを実体の絶対パスにする。添付資料の指定が無ければ空を返す。
func resolveTargets(to RestoreTargets) (uploadPath, attachPath string, err error) {
	if uploadPath, err = resolvePath(to.UploadDir); err != nil {
		return "", "", fmt.Errorf("backup: restore: %w", err)
	}
	if to.AttachmentDir != "" {
		if attachPath, err = resolvePath(to.AttachmentDir); err != nil {
			return "", "", fmt.Errorf("backup: restore: %w", err)
		}
	}
	return uploadPath, attachPath, nil
}

// releaseDatabase は dbPath の DB を他のプロセスが開いていないことを確かめ、WAL の内容を DB ファイルへ
// 書き戻してロールバックジャーナル方式に切り替える。WAL からの切り替えには排他ロックが要るため、
// 他の接続が開いていれば切り替えられず、使用中と判定できる。DB ファイルが無ければ何もしない。
// 壊れた DB（SQLite として読めないファイル）は、バックアップで置き換える対象としてそのまま退避させる。
func releaseDatabase(dbPath string) error {
	if !exists(dbPath) {
		return nil
	}
	db, err := sqlx.Open(database.DriverName, dbPath+"?_pragma=busy_timeout(1000)")
	if err != nil {
		return fmt.Errorf("backup: restore: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var mode string
	err = db.Get(&mode, "PRAGMA journal_mode = DELETE")
	var se *sqlite.Error
	locked := errors.As(err, &se) && (se.Code()&0xff == sqlite3.SQLITE_BUSY || se.Code()&0xff == sqlite3.SQLITE_LOCKED)
	if locked || (err == nil && !strings.EqualFold(mode, "delete")) {
		return fmt.Errorf("%w: %s (stop the sorus server before restoring, or restore from the admin screen)", ErrDatabaseInUse, dbPath)
	}
	if err != nil && !(se != nil && (se.Code()&0xff == sqlite3.SQLITE_NOTADB || se.Code()&0xff == sqlite3.SQLITE_CORRUPT)) {
		return fmt.Errorf("backup: restore: open current database %s: %w", dbPath, err)
	}
	return nil
}

// RestoreLive は zipPath の完全バックアップを、稼働中のサーバーが使っている db とアップロード画像の
// ディレクトリ targetUploadDir に復元する。
//
// アーカイブ全体を検査して一時領域に展開し、DB の整合性を確かめてから差し替える。DB の中身は SQLite の
// オンラインバックアップ API で書き込みを一時的に排他しながら置き換えるため、db の接続はそのまま使い続けられる。
// 置き換える前の DB は VACUUM INTO で、ディレクトリはそのまま、元の場所に PreRestoreInfix と日時を付けた名前で
// 残す（インメモリ DB は退避しない）。古いスキーマのバックアップは置き換えたあとに database.Migrate で更新する。
// 途中で失敗した場合は、DB とディレクトリを元に戻してエラーを返す。添付資料は復元しない（RestoreLiveWith を参照）。
func RestoreLive(db *sqlx.DB, zipPath, targetUploadDir string) error {
	return RestoreLiveWith(db, zipPath, RestoreTargets{UploadDir: targetUploadDir})
}

// RestoreLiveWith は RestoreLive と同じ手順で、zipPath の完全バックアップを稼働中の db と to のアップロード画像・
// 添付資料のディレクトリに復元する（to.DBPath は使わない）。添付資料のディレクトリは、to.AttachmentDir を指定し、
// アーカイブに添付資料がある（形式のバージョン2）ときだけ置き換える。
func RestoreLiveWith(db *sqlx.DB, zipPath string, to RestoreTargets) error {
	if db == nil {
		return errors.New("backup: restore: nil db")
	}
	if zipPath == "" || to.UploadDir == "" {
		return errors.New("backup: restore: empty zipPath or targetUploadDir")
	}
	uploadPath, attachPath, err := resolveTargets(to)
	if err != nil {
		return err
	}
	livePath, err := mainDatabaseFile(db)
	if err != nil {
		return err
	}
	a, err := openArchive(zipPath)
	if err != nil {
		return err
	}
	defer a.Close()
	dbDir := os.TempDir()
	if livePath != "" {
		dbDir = filepath.Dir(livePath)
	}
	st, err := a.stage(dbDir, uploadPath, attachPath)
	if err != nil {
		return err
	}
	defer st.cleanup()

	stamp := time.Now().UTC().Format(stampLayout)
	prevDB := ""
	if livePath != "" {
		prevDB = freePath(livePath + PreRestoreInfix + stamp)
		if err := database.Backup(db, prevDB); err != nil {
			return fmt.Errorf("backup: restore: save current database: %w", err)
		}
	}
	// unchanged は DB を書き換える前に失敗したときの後始末。退避した DB は現在の DB と同じ内容なので消す。
	var sw swapper
	unchanged := func(err error) error {
		if prevDB != "" {
			os.Remove(prevDB)
		}
		return sw.fail(err)
	}
	if err := sw.swapDirs(st, uploadPath, attachPath, stamp); err != nil {
		return unchanged(err)
	}
	// オンラインバックアップ API の書き写しは1つのトランザクションで行われ、失敗すれば DB は元のまま残る。
	if err := copyInto(db, st.db); err != nil {
		return unchanged(fmt.Errorf("backup: restore: replace database: %w", err))
	}
	if err := database.Migrate(db); err != nil {
		err = fmt.Errorf("backup: restore: migrate restored database: %w", err)
		if prevDB == "" {
			return sw.fail(err)
		}
		if rerr := copyInto(db, prevDB); rerr != nil {
			// 元に戻せなかった DB は退避先から手作業で戻せるよう残す。
			return sw.fail(errors.Join(err, fmt.Errorf("backup: restore: roll back database from %s: %w", prevDB, rerr)))
		}
		return unchanged(err)
	}
	return nil
}

// mainDatabaseFile は db の main データベースのファイルパスを返す。インメモリ DB なら空を返す。
func mainDatabaseFile(db *sqlx.DB) (string, error) {
	var rows []struct {
		Seq  int    `db:"seq"`
		Name string `db:"name"`
		File string `db:"file"`
	}
	if err := db.Select(&rows, "PRAGMA database_list"); err != nil {
		return "", fmt.Errorf("backup: restore: read database list: %w", err)
	}
	for _, r := range rows {
		if r.Name == "main" {
			return r.File, nil
		}
	}
	return "", nil
}

// restorer は modernc.org/sqlite の接続が持つオンラインバックアップ（復元方向）の API。
type restorer interface {
	NewRestore(srcURI string) (*sqlite.Backup, error)
}

// copyInto は SQLite のオンラインバックアップ API で src の DB の中身を db にそのまま書き写す。
// 書き写す間は db への書き込みを排他し、完了すると db の他の接続からも新しい中身が見える。
func copyInto(db *sqlx.DB, src string) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		r, ok := dc.(restorer)
		if !ok {
			return fmt.Errorf("driver connection %T does not support online restore", dc)
		}
		b, err := r.NewRestore(src)
		if err != nil {
			return err
		}
		for {
			more, err := b.Step(-1)
			if err != nil {
				return errors.Join(err, b.Finish())
			}
			if !more {
				return b.Finish()
			}
		}
	})
}

// swapper はディレクトリ・ファイルの差し替えを記録し、失敗したときに逆順で元に戻す。
type swapper struct {
	done [][2]string
}

// swapDirs は展開した画像（と添付資料）のディレクトリを復元先へ移す。今のディレクトリは元の場所に
// PreRestoreInfix と日時 stamp を付けた名前で退避する。
func (s *swapper) swapDirs(st *staged, uploadPath, attachPath, stamp string) error {
	pairs := [][2]string{{st.uploads, uploadPath}}
	if st.attachments != "" {
		pairs = append(pairs, [2]string{st.attachments, attachPath})
	}
	for _, p := range pairs {
		if exists(p[1]) {
			if err := s.rename(p[1], freePath(p[1]+PreRestoreInfix+stamp)); err != nil {
				return err
			}
		}
		if err := s.rename(p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *swapper) rename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("backup: restore: %w", err)
	}
	s.done = append(s.done, [2]string{from, to})
	return nil
}

// fail は記録した差し替えを元に戻し、err（元に戻せなかった場合はその理由も）を返す。
func (s *swapper) fail(err error) error {
	for i := len(s.done) - 1; i >= 0; i-- {
		from, to := s.done[i][0], s.done[i][1]
		if rerr := os.Rename(to, from); rerr != nil {
			err = errors.Join(err, fmt.Errorf("backup: restore: roll back %s: %w", from, rerr))
		}
	}
	s.done = nil
	return err
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// freePath は p が空いていれば p を、使われていれば p-2, p-3, ... のうち空いている最初の名前を返す。
func freePath(p string) string {
	if !exists(p) {
		return p
	}
	for i := 2; ; i++ {
		if c := fmt.Sprintf("%s-%d", p, i); !exists(c) {
			return c
		}
	}
}
