package desktop

import (
	"context"
	"os/exec"
	"time"
)

// screenWorkAreaTimeout は画面の大きさを osascript で調べるのを待つ上限。超えたら理想の大きさで開く。
const screenWorkAreaTimeout = 3 * time.Second

// screenWorkAreaScript は主ディスプレイの、メニューバーと Dock を除いた範囲（NSScreen の visibleFrame）の幅・高さを
// ポイント（Retina でも DIP と同じ単位）で "幅,高さ" と出す JavaScript for Automation。Finder を操作しないため、
// 「sorus が Finder を制御しようとしています」というアクセス許可の確認は出ない。
const screenWorkAreaScript = `ObjC.import("AppKit"); var f = $.NSScreen.mainScreen.visibleFrame; ` +
	`Math.round(f.size.width) + "," + Math.round(f.size.height)`

// screenWorkArea は主ディスプレイの作業領域の幅・高さ（ポイント = DIP）を osascript で調べる。調べられなければ 0, 0 を
// 返す（windowSizeFor が理想の大きさにする）。
func screenWorkArea() (w, h int) {
	ctx, cancel := context.WithTimeout(context.Background(), screenWorkAreaTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", screenWorkAreaScript).Output()
	if err != nil {
		return 0, 0
	}
	return parseScreenSize(string(out))
}
