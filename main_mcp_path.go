package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"sorus/internal/server"
)

// mcpPathFileName は公開専用モードで自動生成した MCP サーバーの秘匿パスを保存するファイルの名前。署名鍵と同じ
// ディレクトリ（sync_path.txt の隣）に置く。
const mcpPathFileName = "mcp_path.txt"

// mcpPathFromEnv は環境変数 MCP_API_PATH を検査して正規化する。未設定（空白だけを含む）なら空を返す。
func mcpPathFromEnv() (string, error) {
	raw := os.Getenv("MCP_API_PATH")
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	p, err := server.NormalizeMCPAPIPath(raw)
	if err != nil {
		return "", fmt.Errorf("invalid MCP_API_PATH: %w", err)
	}
	return p, nil
}

// mcpAPIPath はサーバーモードの MCP サーバーのパスを返す。MCP_API_PATH があればそれを使う。無ければ、公開専用モードでは
// 署名鍵と同じディレクトリの mcp_path.txt から読み込み、無ければ推測できないパス（/api/mcp-<32桁の16進数>）を生成して
// 保存する。公開専用モードでなければ空（既定の /api/mcp）を返す。
func mcpAPIPath(cfg serveConfig) (string, error) {
	p, err := mcpPathFromEnv()
	if err != nil || p != "" || !cfg.publicOnly {
		return p, err
	}
	file := filepath.Join(filepath.Dir(cfg.keyFile), mcpPathFileName)
	p, created, err := loadOrCreateMCPAPIPath(file)
	if err != nil {
		return "", err
	}
	if created {
		log.Printf("Created a new secret MCP API path: %s (keep it with the signing key)", file)
	}
	return p, nil
}

// desktopMCPAPIPath はデスクトップモードの MCP サーバーのパスを返す。MCP_API_PATH が正しければそれを、無いか正しくなければ
// 空（既定の /api/mcp）を返す（正しくない値で起動できなくならないよう、ログに残して既定に戻す）。
func desktopMCPAPIPath() string {
	p, err := mcpPathFromEnv()
	if err != nil {
		log.Printf("Desktop mode: %v; using the default MCP API path", err)
		return ""
	}
	return p
}

// loadMCPAPIPath は path から MCP サーバーのパスを読み込む。ファイルが無ければ os.ErrNotExist を包んだエラーを返す。
// 同期 API のパスと同じく、所有者だけが読み書きできる権限かを確かめる（readPrivateFile）。
func loadMCPAPIPath(path string) (string, error) {
	data, err := readPrivateFile(path, "mcp path")
	if err != nil {
		return "", err
	}
	p, err := server.NormalizeMCPAPIPath(string(data))
	if err != nil {
		return "", fmt.Errorf("mcp path %s: %w (fix the file, or delete it to generate a new path and reconnect your AI apps)", path, err)
	}
	return p, nil
}

// loadOrCreateMCPAPIPath は path から MCP サーバーのパスを読み込む。ファイルが無ければ推測できないパスを生成し、所有者だけが
// 読み書きできる権限（0600）で保存する。壊れたファイルは作り直さずにエラーにする（作り直すと AI アプリに設定した URL が
// 使えなくなる）。created は新しく作成したか。
func loadOrCreateMCPAPIPath(path string) (p string, created bool, err error) {
	p, err = loadMCPAPIPath(path)
	if !errors.Is(err, os.ErrNotExist) {
		return p, false, err
	}
	if p, err = server.NewMCPAPIPath(); err != nil {
		return "", false, err
	}
	err = createPrivateFile(path, []byte(p+"\n"))
	if errors.Is(err, os.ErrExist) {
		p, err = loadMCPAPIPath(path)
		return p, false, err
	}
	if err != nil {
		return "", false, fmt.Errorf("mcp path: %w", err)
	}
	return p, true, nil
}
