//go:build !windows && !darwin

package desktop

import (
	"log"
	"os"
	"os/exec"
)

// openLogFile は記録のファイル path を既定のアプリで開く。試験で差し替える。
var openLogFile = func(path string) error {
	// OpenBrowser・OpenFolder と同じく、SORUS_NO_BROWSER が 1 なら開かない（試験・自動化向け）。
	if os.Getenv(NoBrowserEnv) == "1" {
		return nil
	}
	cmd := exec.Command("xdg-open", path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// showDialog は、ダイアログを出す標準の手段が無い OS では、記録（sorus.log）があれば開いて見せる。開けなくても
// （xdg-open が無いなど）、ShowError が標準エラー出力に書いているため何もしない。
func showDialog(title, msg string) {
	if p := recordedLog(); p != "" {
		_ = openLogFile(p)
	}
}

// askQuit は、確認画面を出す標準の手段が無い OS では尋ねずに、予約をすべて反映してから終了するものとして
// true を返し、ログに残す。
func askQuit(title, msg string) bool {
	log.Printf("sorus: %s（確認画面を出せないため、予約をすべて反映してから終了します）", msg)
	return true
}
