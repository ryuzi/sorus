//go:build !windows && !darwin

package desktop

// screenWorkArea は、作業領域を調べる手段を持たない OS（Linux など）では常に 0, 0 を返す（windowSizeFor が理想の
// 大きさにする）。
func screenWorkArea() (w, h int) { return 0, 0 }
