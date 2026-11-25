// Package database は SQLite（modernc.org/sqlite, CGO 不要）への接続と
// スキーマの自動マイグレーションを提供する。
package database

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"modernc.org/sqlite" // ドライバ "sqlite" の登録と sqlite.Error
	sqlite3 "modernc.org/sqlite/lib"
)

// DriverName は modernc.org/sqlite が登録するドライバ名。
const DriverName = "sqlite"

// dbFileMode はDBファイル（と WAL 等の付随ファイル）のパーミッション。
// 他ユーザーからパスワードハッシュやセッションIDを読まれないよう所有者のみに限る。
const dbFileMode os.FileMode = 0o600

// busyTimeout は SQLITE_BUSY 時にロック解放を待つ最大時間。
const busyTimeout = 5000 * time.Millisecond

// connPragmas は接続ごとに適用する PRAGMA。modernc.org/sqlite は DSN の
// _pragma を新しい接続を開くたびに実行するため、プール内の全接続に効く。
// journal_mode=WAL はDBファイルに永続化されるため、Connect で一度だけ設定する。
var connPragmas = []string{
	fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()),
	"foreign_keys(ON)",
}

// Connect はSQLiteに接続し、WALモード等のPRAGMAを設定して接続プールを初期化する。
//
// dbPath にはファイルパス、":memory:"、または "file:" URI を渡せる。
// time.Time は SQLite の日時関数と互換な形式で書き込まれる（_time_format=sqlite）。
func Connect(dbPath string) (*sqlx.DB, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("database: empty dbPath")
	}

	if err := secureDBFile(dbPath); err != nil {
		return nil, fmt.Errorf("database: secure %q: %w", dbPath, err)
	}

	db, err := sqlx.Open(DriverName, buildDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("database: open %q: %w", dbPath, err)
	}

	if isMemory(dbPath) {
		// インメモリDBは接続ごとに別のDBになるため、単一接続を使い回す。
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		db.SetConnMaxIdleTime(0)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: connect %q: %w", dbPath, err)
	}

	if err := enableWAL(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: connect %q: %w", dbPath, err)
	}

	var fk int
	if err := db.Get(&fk, "PRAGMA foreign_keys"); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: read foreign_keys: %w", err)
	}
	if fk != 1 {
		db.Close()
		return nil, fmt.Errorf("database: foreign_keys is not enabled")
	}

	return db, nil
}

// enableWAL は journal_mode を WAL に切り替える。
// 新規ファイルを複数プロセスが同時に開くと、切り替えは busy_timeout を待たずに
// SQLITE_BUSY を返すことがあるため、busyTimeout の範囲で再試行する。
// インメモリDBは WAL 非対応のため "memory" のままとなり、これはエラーにしない。
func enableWAL(db *sqlx.DB) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		var mode string
		err := db.Get(&mode, "PRAGMA journal_mode = WAL")
		if err == nil {
			if !strings.EqualFold(mode, "wal") && !strings.EqualFold(mode, "memory") {
				return fmt.Errorf("set journal_mode=WAL: got %q", mode)
			}
			return nil
		}
		if !isBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("set journal_mode=WAL: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isBusy は err が SQLITE_BUSY（拡張コードを含む）かを判定する。
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// buildDSN は dbPath に PRAGMA と時刻フォーマットのクエリパラメータを付与する。
func buildDSN(dbPath string) string {
	q := url.Values{}
	for _, p := range connPragmas {
		q.Add("_pragma", p)
	}
	q.Set("_time_format", "sqlite")

	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	return dbPath + sep + q.Encode()
}

// secureDBFile はファイルベースのDBを dbFileMode で作成し、既存ファイルも
// dbFileMode に制限する。SQLite は WAL・共有メモリ・ジャーナルを新規作成する際に
// DBファイルと同じパーミッションを使うため、本体を先に作っておけば付随ファイルも揃う。
// 既に存在する付随ファイルは個別に制限する。インメモリDBでは何もしない。
func secureDBFile(dbPath string) error {
	if isMemory(dbPath) {
		return nil
	}
	path, err := dbFilePath(dbPath)
	if err != nil {
		return err
	}
	if path == "" {
		// 空のファイル名は SQLite が一時DBとして扱うため、作成するファイルはない。
		return nil
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, dbFileMode)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		err := os.Chmod(path+suffix, dbFileMode)
		if err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	return nil
}

// dbFilePath は dbPath からDBファイルのパスを取り出す。
// "file:" URI はパス部分をデコードし、それ以外はドライバと同様に "?" 以降を除く。
func dbFilePath(dbPath string) (string, error) {
	if !strings.HasPrefix(dbPath, "file:") {
		path, _, _ := strings.Cut(dbPath, "?")
		return path, nil
	}
	u, err := url.Parse(dbPath)
	if err != nil {
		return "", fmt.Errorf("parse URI: %w", err)
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("unsupported URI authority %q", u.Host)
	}
	if u.Opaque != "" {
		// "file:data.db" のような相対パス形式。
		return url.PathUnescape(u.Opaque)
	}
	return u.Path, nil
}

// isMemory は dbPath がインメモリDBを指すかを判定する。
func isMemory(dbPath string) bool {
	return strings.Contains(dbPath, ":memory:") || strings.Contains(dbPath, "mode=memory")
}
