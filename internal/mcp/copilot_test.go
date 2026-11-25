package mcp

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"sorus/internal/article"
)

// TestCopilotCLIIntegration は GitHub Copilot CLI から CMS 内蔵 MCP サーバーへの
// 実際の接続とツール呼び出し（cms_list_articles）を検証する統合テスト。
//
// 実行条件:
//   - 環境変数 COPILOT_INTEGRATION_TEST=1 が設定されていること
//   - または copilot CLI がインストールされており認証済みであること（CI等で未設定時は自動スキップ）
func TestCopilotCLIIntegration(t *testing.T) {
	if os.Getenv("COPILOT_INTEGRATION_TEST") != "1" {
		t.Skip("skipping Copilot CLI integration test; set COPILOT_INTEGRATION_TEST=1 to run")
	}

	copilotBin, err := exec.LookPath("copilot")
	if err != nil {
		copilotBin = "/home/ryuzi/.local/bin/copilot"
		if _, err := os.Stat(copilotBin); err != nil {
			t.Skip("GitHub Copilot CLI not found, skipping integration test")
		}
	}

	// copilot version を確認
	verCmd := exec.Command(copilotBin, "version")
	if out, err := verCmd.CombinedOutput(); err != nil {
		t.Skipf("GitHub Copilot CLI not functional (%v): %s", err, string(out))
	}

	env := newTestEnv(t, Config{})
	testTitle := "学園祭2026：実行委員会の挑戦と見どころ特集"
	env.seed(article.Article{
		AuthorID: writerA.ID,
		Title:    testTitle,
		Slug:     "gakusai-2026",
		Status:   article.StatusPublished,
	})

	serverName := "sorus-test-" + strings.ToLower(t.Name()) + "-" + strconv.Itoa(os.Getpid())
	mcpURL := env.http.URL + DefaultBasePath
	token := env.tokens[admin.ID]

	// サーバーを Copilot CLI に登録
	addCmd := exec.Command(copilotBin, "mcp", "add", "--transport", "http",
		"--header", "Authorization: Bearer "+token,
		serverName, mcpURL)
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to add MCP server to copilot: %v\nOutput: %s", err, string(out))
	}
	t.Cleanup(func() {
		_ = exec.Command(copilotBin, "mcp", "remove", serverName).Run()
	})

	// 登録確認
	getCmd := exec.Command(copilotBin, "mcp", "get", serverName)
	if out, err := getCmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "Enabled") {
		t.Fatalf("copilot mcp get %s failed: %v\nOutput: %s", serverName, err, string(out))
	}

	// Copilot CLI にツール呼び出しを実行させる
	// 作業ディレクトリを空の t.TempDir() にし、リポジトリ指示（AGENTS.md等）の読み込みと shell 実行を抑止
	prompt := "Call the tool cms_list_articles from " + serverName + " to list articles. Print the title of the article you find."
	runCmd := exec.Command(copilotBin,
		"--no-custom-instructions",
		"--deny-tool=shell(*)",
		"--allow-tool="+serverName+"(*)",
		"-p="+prompt,
	)
	runCmd.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	runCmd.Stdout = &stdout
	runCmd.Stderr = &stderr

	if err := runCmd.Run(); err != nil {
		t.Fatalf("copilot execution failed: %v\nStderr: %s\nStdout: %s", err, stderr.String(), stdout.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, testTitle) {
		t.Errorf("copilot response did not contain article title %q\nStdout:\n%s\nStderr:\n%s", testTitle, outStr, stderr.String())
	}
}
