package desktop

import (
	"log"
	"os/exec"
)

// showDialog は osascript でエラーのアイコン付きのダイアログを表示する。
func showDialog(title, msg string) {
	script := "display dialog " + appleScriptString(msg) + " with title " + appleScriptString(title) +
		` buttons {"OK"} default button "OK" with icon stop`
	_ = exec.Command("osascript", "-e", script).Run()
}

// askQuit は osascript で2つのボタンの確認画面を出す。出せなかったときは true を返す。
func askQuit(title, msg string) bool {
	out, err := exec.Command("osascript", "-e", quitDialogScript(title, msg)).Output()
	if err != nil {
		log.Printf("sorus: 終了の確認画面を出せないため、予約をすべて反映してから終了します: %v", err)
		return true
	}
	return quitDialogAnswer(string(out))
}
