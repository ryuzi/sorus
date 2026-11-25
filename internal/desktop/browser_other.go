//go:build !windows

package desktop

// registeredAppPath は、App Paths の無い OS では常に空を返す。
func registeredAppPath(exe string) string { return "" }
