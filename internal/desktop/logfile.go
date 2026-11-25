package desktop

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
)

// maxLogSize は sorus.log の上限の目安。起動時にこれを超えていれば sorus.log.1 に移して新しく書き始める。
const maxLogSize = 5 << 20

// openedLog は OpenLog が最後に開いた記録のパス。起動に失敗したとき ShowError が利用者に見せる。
var openedLog atomic.Value // string

// recordedLog は OpenLog で開いた記録のパスを返す。まだ開いていなければ空。
func recordedLog() string {
	p, _ := openedLog.Load().(string)
	return p
}

// OpenLog は動作の記録 path（sorus.log）を追記用に開く。前回までの記録が maxLogSize を超えていれば
// path.1 に移し（前の path.1 は消える）、記録が際限なく大きくならないようにする。開いた path は覚えておき、
// 起動に失敗したときに ShowError が開いて見せる（Linux）。
func OpenLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, fmt.Errorf("desktop: rotate log: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("desktop: log: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("desktop: open log: %w", err)
	}
	openedLog.Store(path)
	return f, nil
}
