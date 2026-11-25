//go:build !windows && !darwin

package desktop

import (
	"errors"
	"path/filepath"
	"testing"
)

// Linux では、ShowError は記録を開いていればそのファイルを開く。開けなくても止まらない。
func TestShowErrorOpensRecordedLog(t *testing.T) {
	prevOpen, prevLog := openLogFile, recordedLog()
	t.Cleanup(func() { openLogFile = prevOpen; openedLog.Store(prevLog) })
	var opened []string
	openLogFile = func(path string) error {
		opened = append(opened, path)
		return errors.New("xdg-open: not found")
	}

	openedLog.Store("")
	ShowError("記録を開く前の失敗")
	if len(opened) != 0 {
		t.Fatalf("opened %v before a log was recorded", opened)
	}

	path := filepath.Join(t.TempDir(), "sorus.log")
	f, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	ShowError("起動できませんでした")
	if len(opened) != 1 || opened[0] != path {
		t.Errorf("opened = %v, want [%s]", opened, path)
	}
}
