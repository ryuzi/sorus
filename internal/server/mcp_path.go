package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"sorus/internal/mcp"
)

// MCP サーバーのパス（Config.MCPAPIPath）。
//
//   - デスクトップモード・サーバーモード（社内 LAN の --server）: 既定は mcp.DefaultBasePath（/api/mcp）。環境変数
//     MCP_API_PATH があれば、そのパスに置き換える（/api/mcp は 404 になる）。
//   - 公開専用モード（--public-only）: インターネットに出るため、推測できない秘匿パス（/api/mcp-<32桁の16進数>）だけに
//     置く。main.go が MCP_API_PATH、無ければ署名鍵と同じディレクトリの mcp_path.txt から読み込み、どちらも無ければ
//     NewMCPAPIPath で生成して保存する。固定の /api/mcp への無差別なスキャンは、存在しないページと同じ 404 になる。
//
// 秘匿パスで動かすときは、OAuth 2.1 の保護されたリソースのメタデータも、そのパス専用のもの
// （mcp.ProtectedResourceMetadataPathFor）だけを置き、共通の /.well-known/oauth-protected-resource には秘匿パスを
// 載せない。

const (
	// secretMCPAPIPathPrefix は NewMCPAPIPath が生成するパスの接頭辞。
	secretMCPAPIPathPrefix = mcp.DefaultBasePath + "-"
	// secretMCPAPIPathBytes は NewMCPAPIPath が生成するパスの乱数部分のバイト数（16進数で32桁）。
	secretMCPAPIPathBytes = 16
	// mcpAPIPathRequiredPrefix は MCP サーバーのパスの先頭。/api の下に置き、公開ページのキャッシュ・CSP
	// （isPublicHTMLCachePath）やページのスラッグと重ならないようにする。
	mcpAPIPathRequiredPrefix = "/api/"
)

// ErrInvalidMCPAPIPath は MCP サーバーのパスの形式が正しくないことを示す。
var ErrInvalidMCPAPIPath = errors.New("invalid mcp api path")

// NewMCPAPIPath は第三者に推測できない MCP サーバーのパス（/api/mcp-<32桁の16進数>）を生成する。
func NewMCPAPIPath() (string, error) {
	buf := make([]byte, secretMCPAPIPathBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("server: generate mcp api path: %w", err)
	}
	return secretMCPAPIPathPrefix + hex.EncodeToString(buf), nil
}

// NormalizeMCPAPIPath は MCP サーバーのパスを検査し、前後の空白と末尾の / を除き、先頭の / を補った形にする。
// /api の下の、URL の非予約文字だけの区間を / でつないだパスだけを受け付ける。正しくなければ ErrInvalidMCPAPIPath を
// 包んだエラーを返す。同期 API のパスとの重なりは New が確かめる。
func NormalizeMCPAPIPath(raw string) (string, error) {
	p := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > maxSyncAPIPathLength || !syncAPIPathPattern.MatchString(p) || path.Clean(p) != p ||
		!strings.HasPrefix(p, mcpAPIPathRequiredPrefix) {
		return "", fmt.Errorf("%w %q: use a path such as %s<random hex> (under /api/, letters, digits and . _ ~ - only)",
			ErrInvalidMCPAPIPath, raw, secretMCPAPIPathPrefix)
	}
	return p, nil
}

// pathsOverlap は a と b が同じパスか、一方がもう一方の下にあるかを返す。
func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// resolveMCPAPIPath は New に渡された MCP サーバーのパスを検査して返す。省略したときは、公開専用モードなら
// 推測できないパスをこの起動のあいだだけ生成し（main.go は保存したパスを渡す）、それ以外は mcp.DefaultBasePath。
// 公開専用モードで推測できる /api/mcp を指定したとき、同期 API のパスと重なるときはエラーにする。
func resolveMCPAPIPath(raw string, publicOnly bool, syncPath string) (string, error) {
	var p string
	if strings.TrimSpace(raw) == "" {
		if !publicOnly {
			return mcp.DefaultBasePath, nil
		}
		var err error
		if p, err = NewMCPAPIPath(); err != nil {
			return "", err
		}
	} else {
		var err error
		if p, err = NormalizeMCPAPIPath(raw); err != nil {
			return "", err
		}
	}
	if publicOnly && p == mcp.DefaultBasePath {
		return "", fmt.Errorf("%w %q: the public-only server must use a secret path such as %s<random hex>",
			ErrInvalidMCPAPIPath, raw, secretMCPAPIPathPrefix)
	}
	if pathsOverlap(p, syncPath) {
		return "", fmt.Errorf("%w %q: it overlaps the sync api path", ErrInvalidMCPAPIPath, raw)
	}
	return p, nil
}

// mcpAPIPath は MCP サーバーのパスを返す。New を通さずに作った Server で空のままなら、公開専用モード以外は
// 既定の mcp.DefaultBasePath を返す。
func (s *Server) mcpAPIPath() string {
	if s.cfg.MCPAPIPath != "" {
		return s.cfg.MCPAPIPath
	}
	if s.cfg.PublicOnly {
		return ""
	}
	return mcp.DefaultBasePath
}

// mcpSecretPath は MCP サーバーを秘匿したパス（既定の /api/mcp 以外）で動かしているかを返す。
func (s *Server) mcpSecretPath() bool {
	return s.cfg.PublicOnly || (s.cfg.MCPAPIPath != "" && s.cfg.MCPAPIPath != mcp.DefaultBasePath)
}

// isMCPPath は MCP サーバー（Config.MCPAPIPath 以下）へのパスかを返す。
func (s *Server) isMCPPath(p string) bool {
	base := s.mcpAPIPath()
	return base != "" && (p == base || strings.HasPrefix(p, base+"/"))
}
