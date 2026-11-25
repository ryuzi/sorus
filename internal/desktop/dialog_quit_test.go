package desktop

import (
	"strings"
	"testing"
)

// 「はい／いいえ」の確認画面（Windows）では、どちらのボタンが何をするかを文面に書き添える。
func TestQuitYesNoMessage(t *testing.T) {
	const msg = "予約した記事が 3 件あります（最後は 11月3日 9:00）。"
	got := quitYesNoMessage(msg)
	want := msg + "\n\n「はい」: 予約をすべて反映してから終了する\n「いいえ」: 今すぐ終了する"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// macOS の確認画面は2つのボタンを出し、「予約をすべて反映してから終了する」を既定にする。文面は引用符を逃がす。
func TestQuitDialogScript(t *testing.T) {
	got := quitDialogScript("sorus", `記事「"題名"\」の予約`)
	want := `display dialog "記事「\"題名\"\\」の予約" with title "sorus"` +
		` buttons {"今すぐ終了する", "予約をすべて反映してから終了する"}` +
		` default button "予約をすべて反映してから終了する" with icon caution`
	if got != want {
		t.Errorf("script =\n%s\nwant\n%s", got, want)
	}
}

// osascript の出力から、「今すぐ終了する」が押されたときだけ false を返す。
func TestQuitDialogAnswer(t *testing.T) {
	tests := []struct {
		out  string
		want bool
	}{
		{"button returned:今すぐ終了する\n", false},
		{"button returned:予約をすべて反映してから終了する\n", true},
		{"", true},
	}
	for _, tt := range tests {
		if got := quitDialogAnswer(tt.out); got != tt.want {
			t.Errorf("answer(%q) = %v, want %v", strings.TrimSpace(tt.out), got, tt.want)
		}
	}
}
