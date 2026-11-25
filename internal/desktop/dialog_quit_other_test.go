//go:build !windows && !darwin

package desktop

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// 確認画面を出せない OS では、尋ねずに「予約をすべて反映してから終了する」を選んだものとし、ログに残す。
func TestAskQuitWithPendingWithoutDialog(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	if !AskQuitWithPending("予約した記事が 1 件あります。") {
		t.Error("AskQuitWithPending = false, want true (finish first)")
	}
	if !strings.Contains(buf.String(), "予約した記事が 1 件あります。") {
		t.Errorf("log = %q, want the message", buf.String())
	}
}
