package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"sorus/internal/database"
)

func TestClassifyOpenErrorNil(t *testing.T) {
	if got := ClassifyOpenError(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
}

func TestClassifyOpenErrorDamaged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database.db")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := database.Connect(path)
	if err == nil {
		t.Fatal("Connect opened a file that is not a database")
	}
	wrapped := fmt.Errorf("connect database: %w", err)
	if got := ClassifyOpenError(wrapped); got != OpenFailureDamaged {
		t.Errorf("real connect error %v classified as %q, want %s", wrapped, got, OpenFailureDamaged)
	}
	stored := errors.New(`connect database: database: connect "/tmp/sites/l1yvl3/database.db": file is not a database (26)`)
	if got := ClassifyOpenError(stored); got != OpenFailureDamaged {
		t.Errorf("stored not-a-database error classified as %q", got)
	}
	malformed := errors.New("database disk image is malformed")
	if got := ClassifyOpenError(malformed); got != OpenFailureDamaged {
		t.Errorf("malformed image classified as %q", got)
	}
}

func TestClassifyOpenErrorMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "database.db")
	_, err := database.Connect(missing)
	if err == nil {
		t.Fatal("Connect opened a missing path")
	}
	if got := ClassifyOpenError(fmt.Errorf("connect database: %w", err)); got != OpenFailureMissing {
		t.Errorf("missing file %v classified as %q, want %s", err, got, OpenFailureMissing)
	}
	stored := errors.New(`open "/tmp/sites/abc/database.db": no such file or directory`)
	if got := ClassifyOpenError(stored); got != OpenFailureMissing {
		t.Errorf("stored missing-file error classified as %q", got)
	}
	if got := ClassifyOpenError(fmt.Errorf("open search index: %w", os.ErrNotExist)); got != OpenFailureMissing {
		t.Errorf("ErrNotExist classified as %q", got)
	}
}

func TestClassifyOpenErrorPermission(t *testing.T) {
	err := fmt.Errorf("connect database: %w", os.ErrPermission)
	if got := ClassifyOpenError(err); got != OpenFailurePermission {
		t.Errorf("ErrPermission classified as %q", got)
	}
	stored := errors.New(`open "/tmp/sites/abc/database.db": permission denied`)
	if got := ClassifyOpenError(stored); got != OpenFailurePermission {
		t.Errorf("stored permission error classified as %q", got)
	}
	readonly := errors.New("attempt to write a readonly database")
	if got := ClassifyOpenError(readonly); got != OpenFailurePermission {
		t.Errorf("readonly database classified as %q", got)
	}
}

func TestClassifyOpenErrorAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = net.Listen("tcp", ln.Addr().String())
	if err == nil {
		t.Fatal("second listen succeeded")
	}
	wrapped := fmt.Errorf("listen: %w", err)
	if got := ClassifyOpenError(wrapped); got != OpenFailureAddress {
		t.Errorf("real bind error %v classified as %q, want %s", wrapped, got, OpenFailureAddress)
	}
	stored := errors.New("listen tcp 127.0.0.1:8780: bind: address already in use")
	if got := ClassifyOpenError(stored); got != OpenFailureAddress {
		t.Errorf("stored address error classified as %q", got)
	}
}

func TestClassifyOpenErrorUnknown(t *testing.T) {
	err := errors.New("initialize server: boom")
	if got := ClassifyOpenError(err); got != OpenFailureUnknown {
		t.Errorf("unknown error classified as %q", got)
	}
}

// SQLITE_CANTOPEN は、フォルダもファイルも無いときだけ起きるわけではない。
// 無いパスは Connect が SQLite に届く前に os.ErrNotExist を返す。ディレクトリをデータベースとして
// 開くと、実在するのに SQLITE_CANTOPEN (14) になる。それを「見つからない」にしてはいけない。
func TestClassifyOpenErrorCantOpenIsUnknown(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open(database.DriverName, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	err = db.Ping()
	if err == nil {
		t.Fatal("Ping opened a directory as a database")
	}
	var se *sqlite.Error
	if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_CANTOPEN {
		t.Fatalf("Ping error = %v, want SQLITE_CANTOPEN", err)
	}
	wrapped := fmt.Errorf("connect database: %w", err)
	if got := ClassifyOpenError(wrapped); got != OpenFailureUnknown {
		t.Errorf("SQLITE_CANTOPEN (%v) classified as %q, want %s", err, got, OpenFailureUnknown)
	}
	stored := errors.New(wrapped.Error())
	if got := ClassifyOpenError(stored); got != OpenFailureUnknown {
		t.Errorf("stored error %q classified as %q, want %s", stored, got, OpenFailureUnknown)
	}
}
