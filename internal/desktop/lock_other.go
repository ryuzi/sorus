//go:build !linux && !darwin && !freebsd && !windows

package desktop

import "os"

// lockFile はファイルロックの無い OS では何もしない（二重起動は instance.json とヘルスチェックだけで検出する）。
func lockFile(*os.File) error { return nil }

// unlockFile は lockFile に対応して何もしない。
func unlockFile(*os.File) error { return nil }
