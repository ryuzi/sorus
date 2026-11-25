// Package version はアプリの版（リリースのタグ名）を保持する。
// 配布用ビルドでは scripts/release.sh が
// -ldflags "-X sorus/internal/version.Version=<タグ名>" で埋め込む。
package version

// Version はビルドした版。go run や通常の go build では "dev" のまま。
var Version = "dev"
