package desktop

import (
	"log"

	"golang.org/x/sys/windows"
)

// idNo は MessageBoxW が「いいえ」を押されたときに返す値（IDNO）。
const idNo = 7

// showDialog はエラーのアイコン付きのメッセージボックス（MessageBoxW）を表示する。
func showDialog(title, msg string) {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	m, err := windows.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND)
}

// askQuit は「はい／いいえ」のメッセージボックス（MessageBoxW）で尋ね、「はい」（既定のボタン）なら true を
// 返す。ボタンの文言は変えられないため、どちらが何をするかを文面に書き添える。出せなかったときは true を返す。
func askQuit(title, msg string) bool {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		log.Printf("sorus: 終了の確認画面を出せないため、予約をすべて反映してから終了します: %v", err)
		return true
	}
	m, err := windows.UTF16PtrFromString(quitYesNoMessage(msg))
	if err != nil {
		log.Printf("sorus: 終了の確認画面を出せないため、予約をすべて反映してから終了します: %v", err)
		return true
	}
	ret, err := windows.MessageBox(0, m, t,
		windows.MB_YESNO|windows.MB_ICONQUESTION|windows.MB_DEFBUTTON1|windows.MB_SETFOREGROUND)
	if ret == 0 {
		log.Printf("sorus: 終了の確認画面を出せないため、予約をすべて反映してから終了します: %v", err)
		return true
	}
	return ret != idNo
}
