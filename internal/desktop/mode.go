// Package desktop は sorus を自分の PC で1人で使うデスクトップモードの実行環境を提供する。
// OS 標準のデータディレクトリ、127.0.0.1 だけで待ち受ける空きポートの選択、ブラウザの起動、二重起動の検出、
// Host ヘッダーの検査（DNS リバインディング対策）、コンソールの無い環境での起動失敗の通知を扱う。
package desktop

import (
	"fmt"
	"strings"
)

// BuildModeDesktop は main.buildMode にこの値を埋め込む（-ldflags "-X main.buildMode=desktop"）と、
// フラグを付けずに起動してもデスクトップモードになる。配布用のデスクトップ版はこの値でビルドする。
const BuildModeDesktop = "desktop"

// ServerEnvVars は、設定されていればデスクトップモードを要求されてもサーバーモードで動かす環境変数。
// Docker・systemd の既存の運用はこれらのどれかを設定しているため、デスクトップ版の実行ファイルに差し替えても壊れない。
var ServerEnvVars = []string{"PUBLIC_ONLY", "ENV_FILE", "DB_PATH"}

// Mode は起動時に選んだ動作モード。
type Mode struct {
	// Desktop はデスクトップモードで動かすか。false ならサーバーモード（従来の動作）。
	Desktop bool
	// Args はコマンドライン引数から --desktop・--server を取り除いたもの。サブコマンドやサーバーの引数の解析に渡す。
	Args []string
	// Reason は、デスクトップモードを要求されたのにサーバーモードを選んだ理由（ログに1行出す）。それ以外は空。
	Reason string
}

// SelectMode はコマンドライン引数 args（プログラム名を除く）、ビルド時の既定値 buildMode、環境変数 getenv から
// 動作モードを選ぶ。
//
//   - --server（または --public-only）があればサーバーモード。
//   - --desktop があるか buildMode が BuildModeDesktop ならデスクトップモード。ただし ServerEnvVars のどれかが
//     設定されていればサーバーモードを優先し、その理由を Reason に入れる。
//   - どちらでもなければサーバーモード。
//
// --desktop・--server は引数のどこに置いてもよい（sorus --desktop backup と sorus backup --desktop は同じ）。
func SelectMode(args []string, buildMode string, getenv func(string) string) Mode {
	var m Mode
	desktopFlag, serverFlag, publicOnly := false, false, false
	for _, a := range args {
		switch modeFlag(a) {
		case "desktop":
			desktopFlag = true
			continue
		case "server":
			serverFlag = true
			continue
		case "public-only":
			publicOnly = true
		}
		m.Args = append(m.Args, a)
	}
	wantDesktop := desktopFlag || buildMode == BuildModeDesktop
	switch {
	case !wantDesktop:
	case serverFlag:
	case publicOnly:
		m.Reason = "Desktop mode: disabled because --public-only was given (running as a server)"
	default:
		for _, key := range ServerEnvVars {
			if strings.TrimSpace(getenv(key)) != "" {
				m.Reason = fmt.Sprintf("Desktop mode: disabled because %s is set (running as a server)", key)
				break
			}
		}
		m.Desktop = m.Reason == ""
	}
	return m
}

// modeFlag は引数 a が --desktop・--server・--public-only（- が1つの形と =true を付けた形を含む）なら
// その名前を返し、それ以外なら空を返す。--public-only は取り除かずにサーバーの引数として渡す。
func modeFlag(a string) string {
	name, ok := strings.CutPrefix(a, "--")
	if !ok {
		if name, ok = strings.CutPrefix(a, "-"); !ok {
			return ""
		}
	}
	name = strings.TrimSuffix(name, "=true")
	switch name {
	case "desktop", "server", "public-only":
		return name
	}
	return ""
}
