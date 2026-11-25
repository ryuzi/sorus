package server

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"strings"
	"syscall"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// OpenFailureDamaged は、サイトのデータが壊れていて開けない。
	OpenFailureDamaged = "damaged"
	// OpenFailureMissing は、サイトのフォルダまたはファイルが無い。
	OpenFailureMissing = "missing"
	// OpenFailurePermission は、サイトのフォルダを読み書きできない。
	OpenFailurePermission = "permission"
	// OpenFailureAddress は、サイトのアドレス（待ち受け）を使えない。
	OpenFailureAddress = "address"
	// OpenFailureUnknown は、上のどれにも当てはまらない理由で開けない。
	OpenFailureUnknown = "unknown"
)

// ClassifyOpenError はサイトを開けなかった err を、画面で説明する種類に分ける。
// nil は空を返す。データベースが返したコードと、あとから文字列だけになったエラー（OpenError に残した文）の
// どちらでも同じ結果になる。先にアドレス、次に権限、データの破損、ファイルの不足の順で見て、どれでもなければ unknown。
func ClassifyOpenError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case openErrorAddress(err):
		return OpenFailureAddress
	case openErrorPermission(err):
		return OpenFailurePermission
	case openErrorDamaged(err):
		return OpenFailureDamaged
	case openErrorMissing(err):
		return OpenFailureMissing
	default:
		return OpenFailureUnknown
	}
}

func openErrorAddress(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && (op.Op == "listen" || op.Op == "bind") {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "address already in use") || strings.Contains(text, "cannot assign requested address")
}

func openErrorPermission(err error) bool {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == syscall.EACCES || errno == syscall.EPERM) {
		return true
	}
	switch sqlitePrimaryCode(err) {
	case sqlite3.SQLITE_PERM, sqlite3.SQLITE_READONLY, sqlite3.SQLITE_AUTH:
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "permission denied") || strings.Contains(text, "readonly database") ||
		strings.Contains(text, "read-only file system") || strings.Contains(text, "authorization denied")
}

func openErrorDamaged(err error) bool {
	switch sqlitePrimaryCode(err) {
	case sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_CORRUPT:
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "file is not a database") || strings.Contains(text, "disk image is malformed")
}

func openErrorMissing(err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	// SQLITE_CANTOPEN は「ファイルが無い」だけではない。無いパスは database.Connect が SQLite に
	// 届く前に os.ErrNotExist を返す。CANTOPEN は、フォルダもファイルもあるのに開けないとき
	// （ディレクトリをデータベースとして開く、パスが長すぎる、など）にも返るので、ここでは見ない。
	return strings.Contains(strings.ToLower(err.Error()), "no such file")
}

// sqlitePrimaryCode は err の連鎖にある SQLite の基本コード（拡張コードの下位 8 ビット）を返す。無ければ 0。
func sqlitePrimaryCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() & 0xff
	}
	return 0
}
