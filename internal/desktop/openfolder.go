package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// OpenFolder は dir を OS のファイル管理の画面（Windows: エクスプローラー、macOS: Finder、Linux: 既定のファイル
// マネージャー）で開く。環境変数 SORUS_NO_BROWSER が 1 なら何もしない（テスト・自動化向け。OpenBrowser と同じ）。
// 開けなくても sorus は動き続けるため、呼び出し側は場所を画面で案内すればよい。
func OpenFolder(dir string) error {
	if os.Getenv(NoBrowserEnv) == "1" {
		return nil
	}
	name, args := folderCommand(runtime.GOOS, dir)
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("desktop: open folder: %w", err)
	}
	// エクスプローラーは開けても終了コード 1 を返すことがあるため、終了を待って片付けるだけにする。
	go cmd.Wait()
	return nil
}

// folderCommand は goos の OS で dir をファイル管理の画面で開くコマンドと引数を返す。
func folderCommand(goos, dir string) (string, []string) {
	switch goos {
	case "windows":
		return "explorer.exe", []string{dir}
	case "darwin":
		return "open", []string{dir}
	default:
		return "xdg-open", []string{dir}
	}
}
