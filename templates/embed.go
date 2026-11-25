// Package templates は html/template の SSR テンプレートをバイナリに埋め込む。
package templates

import "embed"

// FS は管理画面テンプレート（admin/ 以下）と公開サイトのテンプレート（public/ 以下）を保持する。
//
//go:embed admin public
var FS embed.FS
