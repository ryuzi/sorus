package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sorus/internal/auth"
	"sorus/internal/database"
	"sorus/internal/server"
)

// newServerDB は公開サーバーの DB（マイグレーション済み）を作り、DB_PATH に設定してパスを返す。
func newServerDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "database.db")
	db, err := database.Connect(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Setenv("DB_PATH", dbPath)
	return dbPath
}

var inviteLinePattern = regexp.MustCompile(`(?m)^  Invite code: (sorus-invite:\S+)$`)

// --base-url を渡すと、標準エラー出力に招待コードの行を足す。標準出力はトークンだけのまま。
func TestRunTokenCreatePrintsInviteCode(t *testing.T) {
	t.Setenv("DB_PATH", "")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "capability.key")
	var stdout, stderr bytes.Buffer
	cfg := tokenConfig{role: "admin", sub: "管理者", ttl: 24 * time.Hour, keyFile: keyFile, baseURL: "https://media.example.jp"}
	if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
		t.Fatalf("runTokenCreate: %v", err)
	}
	token := strings.TrimSpace(stdout.String())
	if strings.Count(stdout.String(), "\n") != 1 || strings.Count(token, ".") != 2 {
		t.Fatalf("stdout must be only the token line, got %q", stdout.String())
	}
	m := inviteLinePattern.FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("stderr has no invite line:\n%s", stderr.String())
	}
	inv, err := server.DecodeInviteCode(m[1])
	if err != nil {
		t.Fatalf("DecodeInviteCode: %v", err)
	}
	syncPath, err := loadSyncAPIPath(syncPathFile(keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if inv.URL != "https://media.example.jp"+syncPath || inv.Token != token {
		t.Errorf("invite = %+v, want url %s and the printed token", inv, "https://media.example.jp"+syncPath)
	}
	// 既存の案内（同期 API の URL）も残す。DB_PATH が無ければ記録せずに知らせる。
	for _, want := range []string{"Sync URL:  https://media.example.jp" + syncPath, "DB_PATH is not set", server.InviteJoinHint} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}

	for name, base := range map[string]string{"no base url": "", "plain http": "http://media.example.jp"} {
		stdout.Reset()
		stderr.Reset()
		cfg.baseURL = base
		if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if inviteLinePattern.MatchString(stderr.String()) || !strings.Contains(stderr.String(), "  Invite code: ") {
			t.Errorf("%s: stderr = %s", name, stderr.String())
		}
	}
}

// 公開サーバーの DB があれば、発行したトークンを cli の発行として記録する。DB が無ければ作らない。
func TestRunTokenCreateRecordsToken(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "capability.key")
	dbPath := newServerDB(t)
	var stdout, stderr bytes.Buffer
	cfg := tokenConfig{role: "writer", sub: "山田", ttl: 48 * time.Hour, keyFile: keyFile}
	if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
		t.Fatalf("runTokenCreate: %v", err)
	}
	priv, err := loadCapabilityKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := auth.VerifyToken(priv.Public().(ed25519.PublicKey), strings.TrimSpace(stdout.String()))
	if err != nil || claims.ID == "" {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	db, err := database.Connect(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var row struct {
		Subject   string `db:"subject"`
		Role      string `db:"role"`
		IssuedVia string `db:"issued_via"`
	}
	if err := db.Get(&row, `SELECT subject, role, issued_via FROM sync_tokens WHERE jti = ?`, claims.ID); err != nil {
		t.Fatalf("token not recorded: %v\nstderr: %s", err, stderr.String())
	}
	if row.Subject != "山田" || row.Role != "writer" || row.IssuedVia != server.TokenIssuedViaCLI {
		t.Errorf("recorded = %+v", row)
	}
	if !strings.Contains(stderr.String(), "Recorded in: "+dbPath) {
		t.Errorf("stderr = %s", stderr.String())
	}

	t.Run("missing database is not created", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "none.db")
		t.Setenv("DB_PATH", missing)
		stdout.Reset()
		stderr.Reset()
		if err := runTokenCreate(cfg, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Errorf("token create created %s: %v", missing, err)
		}
		if !strings.Contains(stderr.String(), "was not found") || strings.TrimSpace(stdout.String()) == "" {
			t.Errorf("stderr = %s", stderr.String())
		}
	})
}

// 公開サーバーが書き込み中（ロックを持っている）でも、busy_timeout の間待って記録する。
func TestRunTokenCreateRecordsWhileDatabaseIsBusy(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "capability.key")
	dbPath := newServerDB(t)
	serverDB, err := database.Connect(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer serverDB.Close()
	conn, err := serverDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(300 * time.Millisecond)
		conn.ExecContext(context.Background(), `COMMIT`)
	}()

	var stdout, stderr bytes.Buffer
	if err := runTokenCreate(tokenConfig{role: "editor", sub: "佐藤", ttl: time.Hour, keyFile: keyFile}, &stdout, &stderr); err != nil {
		t.Fatalf("runTokenCreate: %v", err)
	}
	<-released
	var n int
	if err := serverDB.Get(&n, `SELECT COUNT(*) FROM sync_tokens WHERE subject = '佐藤' AND issued_via = 'cli'`); err != nil || n != 1 {
		t.Fatalf("recorded rows = %d, %v\nstderr: %s", n, err, stderr.String())
	}
}
