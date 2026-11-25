package desktop

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// appPathsKey は、実行ファイルの場所を登録しておくレジストリのキー（App Paths）。
const appPathsKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`

// registeredAppPath は App Paths に登録された exe の場所を返す。シェルと同じく利用者ごと（HKCU）、
// コンピューター全体（HKLM）の順に見る。登録が無ければ空を返す。
func registeredAppPath(exe string) string {
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		k, err := registry.OpenKey(root, appPathsKey+exe, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, typ, err := k.GetStringValue("")
		k.Close()
		if err != nil || v == "" {
			continue
		}
		if typ == registry.EXPAND_SZ {
			if e, err := registry.ExpandString(v); err == nil {
				v = e
			}
		}
		return strings.Trim(v, `"`)
	}
	return ""
}

// 画面の作業領域を調べる Win32 API（user32.dll。cgo を使わずに呼ぶ）。
var (
	user32                    = windows.NewLazySystemDLL("user32.dll")
	procSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
	procGetDpiForSystem       = user32.NewProc("GetDpiForSystem")
)

// spiGetWorkArea は SystemParametersInfoW で、主ディスプレイの作業領域（タスクバーを除いた範囲）を得る指定。
const spiGetWorkArea = 0x0030

// screenWorkArea は主ディスプレイの作業領域の幅・高さを、ブラウザの --window-size と同じデバイス非依存ピクセル
// （DIP）で返す。sorus は DPI 対応を宣言していないため、ふつうは Windows が 96 DPI 換算の値を返し、GetDpiForSystem も
// 96 を返す。DPI 対応のプロセスとして動いたときも、システムの DPI で割り戻して DIP にそろえる（workAreaDIP）。
// 調べられなければ 0, 0 を返す（windowSizeFor が理想の大きさにする）。
func screenWorkArea() (w, h int) {
	if procSystemParametersInfoW.Find() != nil {
		return 0, 0
	}
	var r windows.Rect
	if ret, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0); ret == 0 {
		return 0, 0
	}
	return workAreaDIP(int(r.Right-r.Left), int(r.Bottom-r.Top), systemDPI())
}

// systemDPI はシステムの DPI を返す。GetDpiForSystem の無い古い Windows（Windows 10 1607 より前）では 96。
func systemDPI() int {
	if procGetDpiForSystem.Find() != nil {
		return 96
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	return int(dpi)
}
