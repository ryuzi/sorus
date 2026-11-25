package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestLLMSTxtEndpoints(t *testing.T) {
	env := newPublicEnv(t)
	anon := env.newClient()

	// 1. GET /llms.txt
	res := anon.get("/llms.txt")
	expectStatus(t, res, http.StatusOK)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected text/plain, got %q", ct)
	}
	// サイト名、公開記事タイトル、llms-full.txt への案内が含まれていること
	expectContains(t, res.Body,
		"# テスト大学新聞",
		"> 学生が運営するメディア",
		"## Articles",
		"- [図書館の新しい自習室]",
		"- [軽音楽部の新歓ライブ]",
		"## Optional",
		"/llms-full.txt",
	)
	// 下書きや未公開が含まれていないこと
	if strings.Contains(res.Body, "下書き記事") {
		t.Errorf("draft article found in llms.txt: %s", res.Body)
	}
	if strings.Contains(res.Body, "未来の予約記事") {
		t.Errorf("scheduled article found in llms.txt: %s", res.Body)
	}

	// 2. GET /llms-full.txt
	resFull := anon.get("/llms-full.txt")
	expectStatus(t, resFull, http.StatusOK)
	if ct := resFull.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected text/plain, got %q", ct)
	}
	// 本文 Markdown やカテゴリ、URL が含まれていること
	expectContains(t, resFull.Body,
		"# 軽音楽部の新歓ライブ",
		"Published:",
		"Category: サークル",
		"## ライブの様子",
		"満員の観客がスタンディングで盛り上がった。",
		"# 図書館の新しい自習室",
		"---",
	)
	if strings.Contains(resFull.Body, "下書き記事") {
		t.Errorf("draft article found in llms-full.txt: %s", resFull.Body)
	}
}

func TestLLMSTxtEmpty(t *testing.T) {
	env := newTestEnvWith(t, Config{SiteName: "Empty Site"}, nil)
	anon := env.newClient()

	res := anon.get("/llms.txt")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, "# Empty Site", "## Articles", "## Optional")

	resFull := anon.get("/llms-full.txt")
	expectStatus(t, resFull, http.StatusOK)
}
