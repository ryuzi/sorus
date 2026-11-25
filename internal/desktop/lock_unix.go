//go:build linux || darwin || freebsd

package desktop

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile は f に排他ロック（flock）をかける。別のプロセスがロック中なら ErrLocked を返す。
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	if err != nil {
		return fmt.Errorf("desktop: lock %s: %w", f.Name(), err)
	}
	return nil
}

// unlockFile は lockFile のロックを解放する。
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
