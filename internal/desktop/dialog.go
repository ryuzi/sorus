package desktop

import (
	"fmt"
	"os"
	"strings"
)

// dialogTitle は起動失敗を知らせるダイアログの題名。
const dialogTitle = "sorus"

// 終了の確認（AskQuitWithPending）のボタンの文言。
const (
	finishFirstButton = "予約をすべて反映してから終了する"
	quitNowButton     = "今すぐ終了する"
)

// ShowError は起動の失敗などを利用者に知らせる。Windows の配布版にはコンソールが無いため、標準エラー出力に
// 加えて OS のダイアログ（Windows: MessageBoxW、macOS: osascript）で表示する。Linux など他の OS では
// 標準エラー出力に書き、動作の記録（OpenLog で開いた sorus.log）があれば xdg-open で開いて見せる（端末の無い
// ファイルマネージャーなどから起動したときにも気づけるように）。ダイアログを閉じるまで戻らない。
func ShowError(msg string) {
	fmt.Fprintln(os.Stderr, "sorus: "+msg)
	showDialog(dialogTitle, msg)
}

// AskQuitWithPending は、終了すると予約した記事の反映を逃すときに、msg を添えて「予約をすべて反映してから
// 終了する」か「今すぐ終了する」かを OS の確認画面（Windows: MessageBoxW のはい／いいえ、macOS: osascript）で
// 尋ね、前者なら true を返す。確認画面を出せない OS や、出せなかったときは、反映を逃さないよう true を返して
// ログに残す。答えるまで戻らない。
func AskQuitWithPending(msg string) (finishFirst bool) {
	return askQuit(dialogTitle, msg)
}

// quitYesNoMessage は、ボタンの文言を変えられない「はい／いいえ」の確認画面（Windows）に出す文面を返す。
// msg のあとに、どちらのボタンが何をするかを書き添える。
func quitYesNoMessage(msg string) string {
	return msg + "\n\n「はい」: " + finishFirstButton + "\n「いいえ」: " + quitNowButton
}

// quitDialogScript は、macOS の確認画面を出す AppleScript を返す。「予約をすべて反映してから終了する」を
// 既定のボタン（Return キーで押せる）にする。
func quitDialogScript(title, msg string) string {
	return "display dialog " + appleScriptString(msg) + " with title " + appleScriptString(title) +
		" buttons {" + appleScriptString(quitNowButton) + ", " + appleScriptString(finishFirstButton) + "}" +
		" default button " + appleScriptString(finishFirstButton) + " with icon caution"
}

// quitDialogAnswer は、quitDialogScript を実行した osascript の出力（"button returned:…"）から、
// 「今すぐ終了する」が押されたときだけ false を返す。
func quitDialogAnswer(out string) (finishFirst bool) {
	return strings.TrimSpace(out) != "button returned:"+quitNowButton
}

// appleScriptString は s を AppleScript の文字列リテラルにする。
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
