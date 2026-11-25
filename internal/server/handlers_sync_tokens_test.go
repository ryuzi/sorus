package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/database"
	"sorus/internal/mcp"
)

// newInviteSyncEnv は署名鍵も渡して（招待を発行できる）同期 API を有効にした公開サーバーを起動する。
func newInviteSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	return newInviteSyncEnvWith(t, Config{})
}

// newInviteSyncEnvWith は cfg（TrustedProxies など）に署名鍵を加えて newInviteSyncEnv と同じサーバーを起動する。
func newInviteSyncEnvWith(t *testing.T, cfg Config) *syncEnv {
	t.Helper()
	pub, priv, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	cfg.SyncPublicKey, cfg.SyncSigningKey, cfg.ThemesDir = pub, priv, t.TempDir()
	return &syncEnv{testEnv: newTestEnvWith(t, cfg, nil), priv: priv}
}

// recordedToken は sub・role のトークンを発行し、sorus token create と同じく sync_tokens に記録する。
func (e *syncEnv) recordedToken(sub string, role auth.Role) (string, *auth.Claims) {
	e.t.Helper()
	tok, claims, err := auth.IssueTokenClaims(e.priv, sub, string(role), time.Hour)
	if err != nil {
		e.t.Fatalf("IssueTokenClaims: %v", err)
	}
	if err := RecordIssuedToken(context.Background(), e.db, claims, TokenIssuedViaCLI, ""); err != nil {
		e.t.Fatalf("RecordIssuedToken: %v", err)
	}
	return tok, claims
}

// syncAt は Host と X-Forwarded-Proto を指定して同期 API に送る（招待コードの URL の組み立ての確認用）。
func (e *syncEnv) syncAt(host, proto, token string, body SyncRequest) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, e.server.cfg.SyncAPIPath, strings.NewReader(mustJSON(e.t, body)))
	req.Host = host
	if proto != "" {
		req.Header.Set(echo.HeaderXForwardedProto, proto)
	}
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return e.serve(req)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func issueInviteRequest(role auth.Role, sub string, days int) SyncRequest {
	return SyncRequest{Action: SyncActionIssueInvite, Role: string(role), Subject: sub, TTLDays: days}
}

func revokeRequest(jti string, confirmSelf bool) SyncRequest {
	return SyncRequest{Action: SyncActionRevokeToken, JTI: jti, ConfirmSelf: confirmSelf}
}

func (e *syncEnv) tokenRow(jti string) SyncTokenRecord {
	e.t.Helper()
	var rec SyncTokenRecord
	if err := e.db.Get(&rec, `SELECT `+syncTokenColumns+` FROM sync_tokens WHERE jti = ?`, jti); err != nil {
		e.t.Fatalf("sync_tokens %s: %v", jti, err)
	}
	return rec
}

func TestNewValidatesSyncSigningKey(t *testing.T) {
	pub, _, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Connect(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]Config{
		"mismatched pair": {SyncPublicKey: pub, SyncSigningKey: other},
		"short key":       {SyncPublicKey: pub, SyncSigningKey: ed25519.PrivateKey(make([]byte, 10))},
		"no public key":   {SyncSigningKey: other},
	} {
		cfg.DB, cfg.UploadDir, cfg.AttachmentDir = db, t.TempDir(), t.TempDir()
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want error", name)
		}
	}
}

// 招待の発行・一覧・取り消しは admin のトークンだけが使える。
func TestSyncInvitePermissions(t *testing.T) {
	env := newInviteSyncEnv(t)
	_, claims := env.recordedToken("old-writer", auth.RoleWriter)
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleEditor} {
		tok := env.token("someone", role)
		for _, body := range []SyncRequest{
			issueInviteRequest(auth.RoleWriter, "new", 30),
			{Action: SyncActionListTokens},
			revokeRequest(claims.ID, false),
		} {
			rec := env.call(tok, body)
			expectSyncStatus(t, rec, http.StatusForbidden)
		}
	}
	if rec := env.tokenRow(claims.ID); rec.RevokedAt != nil {
		t.Error("a non-admin token revoked a token")
	}
}

func TestSyncIssueInvite(t *testing.T) {
	env := newInviteSyncEnv(t)
	adminTok, adminClaims := env.recordedToken("サイト管理者", auth.RoleAdmin)
	c := env.newClient()

	res := c.syncHTTP("Bearer "+adminTok, issueInviteRequest(auth.RoleWriter, "  山田 花子 ", 30))
	expectStatus(t, res, http.StatusOK)
	out := decodeResponse[SyncIssueInviteResponse](t, res)
	if out.Action != SyncActionIssueInvite || out.Subject != "山田 花子" || out.Role != auth.RoleWriter || !auth.ValidTokenID(out.JTI) {
		t.Fatalf("response = %+v", out)
	}
	if d := time.Until(out.ExpiresAt); d < 29*24*time.Hour || d > 30*24*time.Hour {
		t.Errorf("expires_at = %v, want about 30 days later", out.ExpiresAt)
	}
	inv, err := DecodeInviteCode(out.InviteCode)
	if err != nil {
		t.Fatalf("DecodeInviteCode(%q): %v", out.InviteCode, err)
	}
	if want := env.srv.URL + env.server.cfg.SyncAPIPath; inv.URL != want {
		t.Errorf("invite url = %q, want %q", inv.URL, want)
	}
	claims, err := auth.VerifyToken(env.priv.Public().(ed25519.PublicKey), inv.Token)
	if err != nil || claims.ID != out.JTI || claims.Subject != "山田 花子" || claims.Role != auth.RoleWriter {
		t.Fatalf("invite token claims = %+v, %v", claims, err)
	}
	rec := env.tokenRow(out.JTI)
	if rec.IssuedVia != TokenIssuedViaAPI || rec.IssuedBy != "サイト管理者" || rec.Role != auth.RoleWriter || rec.RevokedAt != nil {
		t.Errorf("recorded = %+v", rec)
	}

	// 招待のトークンでそのまま記事を提出できる。
	sub := c.syncHTTP("Bearer "+inv.Token, submitRequest(SyncArticle{Title: "招待で提出", ContentMarkdown: "本文"}))
	expectStatus(t, sub, http.StatusOK)

	t.Run("list tokens", func(t *testing.T) {
		res := c.syncHTTP("Bearer "+adminTok, SyncRequest{Action: SyncActionListTokens})
		expectStatus(t, res, http.StatusOK)
		list := decodeResponse[SyncListTokensResponse](t, res)
		if list.CurrentJTI != adminClaims.ID || len(list.Tokens) != 2 {
			t.Fatalf("list = %+v", list)
		}
		byID := map[string]SyncTokenRecord{}
		for _, r := range list.Tokens {
			byID[r.JTI] = r
		}
		if a := byID[adminClaims.ID]; !a.Current || a.IssuedVia != TokenIssuedViaCLI || a.Role != auth.RoleAdmin {
			t.Errorf("admin record = %+v", a)
		}
		if w := byID[out.JTI]; w.Current || w.Subject != "山田 花子" || w.IssuedBy != "サイト管理者" || w.ExpiresAt.Unix() != out.ExpiresAt.Unix() {
			t.Errorf("writer record = %+v", w)
		}
		// 新しく発行したものが先に来る。
		if list.Tokens[0].JTI != out.JTI && !list.Tokens[0].IssuedAt.Equal(list.Tokens[1].IssuedAt) {
			t.Errorf("order = %s, %s", list.Tokens[0].Subject, list.Tokens[1].Subject)
		}
		if strings.Contains(res.Body, inv.Token) || strings.Contains(res.Body, adminTok) {
			t.Error("list_tokens must not include tokens")
		}
	})

	t.Run("default ttl is 90 days", func(t *testing.T) {
		res := c.syncHTTP("Bearer "+adminTok, issueInviteRequest(auth.RoleEditor, "佐藤", 0))
		expectStatus(t, res, http.StatusOK)
		out := decodeResponse[SyncIssueInviteResponse](t, res)
		if d := time.Until(out.ExpiresAt); d < 89*24*time.Hour || d > 90*24*time.Hour {
			t.Errorf("expires_at = %v, want about 90 days later", out.ExpiresAt)
		}
	})

	t.Run("validation", func(t *testing.T) {
		for name, body := range map[string]SyncRequest{
			"unknown role":     issueInviteRequest("owner", "x", 30),
			"empty role":       issueInviteRequest("", "x", 30),
			"empty name":       issueInviteRequest(auth.RoleWriter, "  ", 30),
			"name with break":  issueInviteRequest(auth.RoleWriter, "a\nb", 30),
			"long name":        issueInviteRequest(auth.RoleWriter, strings.Repeat("あ", maxSyncDisplayNameRunes+1), 30),
			"too long ttl":     issueInviteRequest(auth.RoleWriter, "x", maxInviteTTLDays+1),
			"negative ttl":     issueInviteRequest(auth.RoleWriter, "x", -1),
			"admin case typo":  issueInviteRequest("Admin", "x", 30),
			"role with spaces": issueInviteRequest("writer editor", "x", 30),
		} {
			res := c.syncHTTP("Bearer "+adminTok, body)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("%s: status %d, want 422: %s", name, res.StatusCode, res.Body)
			}
		}
		res := c.syncHTTP("Bearer "+adminTok, issueInviteRequest(auth.RoleWriter, "x", maxInviteTTLDays))
		expectStatus(t, res, http.StatusOK)
	})

	t.Run("invite url follows the https request", func(t *testing.T) {
		// Config.TrustedProxies が未設定なら、クライアントが書いた X-Forwarded-Proto で平文の接続を https と偽れない
		// （環境変数 TRUSTED_PROXIES が接続元を信頼していても、サーバーは Config だけを使う）。
		t.Setenv(mcp.TrustedProxiesEnv, "192.0.2.1")
		rec := env.syncAt("media.example.jp", "https", adminTok, issueInviteRequest(auth.RoleWriter, "鈴木", 7))
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)

		// 信頼するリバースプロキシ（httptest.NewRequest の接続元 192.0.2.1）が付けた X-Forwarded-Proto なら https。
		t.Setenv(mcp.TrustedProxiesEnv, "")
		env := newInviteSyncEnvWith(t, Config{TrustedProxies: []string{"192.0.2.1"}})
		adminTok, _ := env.recordedToken("admin", auth.RoleAdmin)
		rec = env.syncAt("media.example.jp", "https", adminTok, issueInviteRequest(auth.RoleWriter, "鈴木", 7))
		expectSyncStatus(t, rec, http.StatusOK)
		inv, err := DecodeInviteCode(decodeSync[SyncIssueInviteResponse](t, rec).InviteCode)
		if err != nil || inv.URL != "https://media.example.jp"+env.server.cfg.SyncAPIPath {
			t.Fatalf("invite = %+v, %v", inv, err)
		}
		// 暗号化されない http の公開サーバーでは、参加できない招待コードを作らない。
		rec = env.syncAt("media.example.jp", "", adminTok, issueInviteRequest(auth.RoleWriter, "鈴木", 7))
		expectSyncStatus(t, rec, http.StatusUnprocessableEntity)
		if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "https") {
			t.Errorf("message = %q", msg)
		}
	})
}

// 署名鍵を渡されていない公開サーバーは、招待の発行だけを分かる言葉で断る。
func TestSyncIssueInviteWithoutSigningKey(t *testing.T) {
	env := newSyncEnv(t)
	tok := env.token("admin", auth.RoleAdmin)
	rec := env.call(tok, issueInviteRequest(auth.RoleWriter, "x", 30))
	expectSyncStatus(t, rec, http.StatusNotImplemented)
	if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "招待を発行できません") {
		t.Errorf("message = %q", msg)
	}
	// 一覧と取り消しは署名鍵が無くても使える。
	expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionListTokens}), http.StatusOK)
}

func TestSyncIssueInviteRateLimit(t *testing.T) {
	env := newInviteSyncEnv(t)
	adminTok, _ := env.recordedToken("admin", auth.RoleAdmin)
	now := time.Now().UTC()
	insert := func(i int, at time.Time) {
		jti := fmt.Sprintf("%032x", i+1)
		if _, err := env.db.Exec(`INSERT INTO sync_tokens (jti, subject, role, issued_at, expires_at, issued_via, issued_by)
			VALUES (?, 'x', 'writer', ?, ?, 'api', 'admin')`, jti, at, at.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// 1時間より前の発行は数えない。
	for i := range 40 {
		insert(i, now.Add(-2*time.Hour))
	}
	for i := 40; i < 40+maxInvitesPerHour-1; i++ {
		insert(i, now.Add(-30*time.Minute))
	}
	c := env.newClient()
	expectStatus(t, c.syncHTTP("Bearer "+adminTok, issueInviteRequest(auth.RoleWriter, "ok", 1)), http.StatusOK)
	res := c.syncHTTP("Bearer "+adminTok, issueInviteRequest(auth.RoleWriter, "too many", 1))
	expectStatus(t, res, http.StatusTooManyRequests)
	if msg := jsonMessage(t, res); !strings.Contains(msg, "1時間") {
		t.Errorf("message = %q", msg)
	}
}

// 同時に多数の招待を発行しても、1時間の上限を超えて発行されない（数えてから記録するまでの間に他の発行が入らない）。
func TestSyncIssueInviteRateLimitConcurrent(t *testing.T) {
	// 平文の httptest の要求を https として扱わせるため、接続元（192.0.2.1）を信頼するリバースプロキシにする。
	env := newInviteSyncEnvWith(t, Config{TrustedProxies: []string{"192.0.2.0/24"}})
	adminTok, _ := env.recordedToken("admin", auth.RoleAdmin)
	const n = 2 * maxInvitesPerHour
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = env.syncAt("cms.example.com", "https", adminTok, issueInviteRequest(auth.RoleWriter, fmt.Sprint("並行", i), 1)).Code
		}()
	}
	close(start)
	wg.Wait()

	got := map[int]int{}
	for _, code := range codes {
		got[code]++
	}
	if got[http.StatusOK] != maxInvitesPerHour || got[http.StatusTooManyRequests] != n-maxInvitesPerHour {
		t.Errorf("statuses = %v, want %d x 200 and %d x 429", got, maxInvitesPerHour, n-maxInvitesPerHour)
	}
	var issued int
	if err := env.db.Get(&issued, `SELECT COUNT(*) FROM sync_tokens WHERE issued_via = ?`, TokenIssuedViaAPI); err != nil {
		t.Fatal(err)
	}
	if issued != maxInvitesPerHour {
		t.Errorf("recorded invites = %d, want %d", issued, maxInvitesPerHour)
	}
}

func TestSyncRevokeToken(t *testing.T) {
	env := newInviteSyncEnv(t)
	adminTok, adminClaims := env.recordedToken("管理者A", auth.RoleAdmin)
	writerTok, writerClaims := env.recordedToken("山田", auth.RoleWriter)
	submit := submitRequest(SyncArticle{Title: "取り消し前", ContentMarkdown: "本文"})
	expectSyncStatus(t, env.call(writerTok, submit), http.StatusOK)

	rec := env.call(adminTok, revokeRequest(writerClaims.ID, false))
	expectSyncStatus(t, rec, http.StatusOK)
	out := decodeSync[SyncRevokeTokenResponse](t, rec)
	if out.Token.JTI != writerClaims.ID || out.Token.RevokedAt == nil || out.Token.Current {
		t.Fatalf("revoke response = %+v", out)
	}
	var revokedBy string
	if err := env.db.Get(&revokedBy, `SELECT revoked_by FROM sync_tokens WHERE jti = ?`, writerClaims.ID); err != nil || revokedBy != "管理者A" {
		t.Errorf("revoked_by = %q, %v", revokedBy, err)
	}

	// 取り消したトークンは、署名と期限が正しくても分かる言葉で拒否される。
	rec = env.call(writerTok, submit)
	expectSyncStatus(t, rec, http.StatusUnauthorized)
	if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "取り消されています") {
		t.Errorf("message = %q", msg)
	}
	if h := rec.Header().Get(echo.HeaderWWWAuthenticate); !strings.Contains(h, `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q", h)
	}

	// 取り消し済みのものをもう一度取り消しても、そのまま記録を返す。
	rec = env.call(adminTok, revokeRequest(writerClaims.ID, false))
	expectSyncStatus(t, rec, http.StatusOK)
	if again := decodeSync[SyncRevokeTokenResponse](t, rec); again.Token.RevokedAt == nil || !again.Token.RevokedAt.Equal(*out.Token.RevokedAt) {
		t.Errorf("second revoke = %+v", again)
	}

	t.Run("unknown or malformed id", func(t *testing.T) {
		expectSyncStatus(t, env.call(adminTok, revokeRequest(strings.Repeat("0", 32), false)), http.StatusNotFound)
		for _, jti := range []string{"", "abc", strings.Repeat("Z", 32), "../" + strings.Repeat("0", 29)} {
			expectSyncStatus(t, env.call(adminTok, revokeRequest(jti, false)), http.StatusBadRequest)
		}
	})

	t.Run("lockout protection", func(t *testing.T) {
		// 自分のトークンは確認が無ければ取り消せない。
		rec := env.call(adminTok, revokeRequest(adminClaims.ID, false))
		expectSyncStatus(t, rec, http.StatusConflict)
		if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "いま使っている招待") {
			t.Errorf("message = %q", msg)
		}
		// 確認があっても、使えるサイト管理者のトークンが他に無ければ取り消せない（期限切れの admin は数えない）。
		expired := &auth.Claims{Subject: "期限切れの管理者", Role: auth.RoleAdmin, IssuedAt: time.Now().Add(-48 * time.Hour).Unix(),
			ExpiresAt: time.Now().Add(-24 * time.Hour).Unix(), ID: strings.Repeat("e", 32)}
		if err := RecordIssuedToken(context.Background(), env.db, expired, TokenIssuedViaCLI, ""); err != nil {
			t.Fatal(err)
		}
		rec = env.call(adminTok, revokeRequest(adminClaims.ID, true))
		expectSyncStatus(t, rec, http.StatusConflict)
		if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "最後のサイト管理者") {
			t.Errorf("message = %q", msg)
		}
		if env.tokenRow(adminClaims.ID).RevokedAt != nil {
			t.Fatal("the last admin token was revoked")
		}
		expectSyncStatus(t, env.call(adminTok, SyncRequest{Action: SyncActionListTokens}), http.StatusOK)

		// 別のサイト管理者がいれば、確認のうえで自分のトークンを取り消せる。
		adminB, claimsB := env.recordedToken("管理者B", auth.RoleAdmin)
		rec = env.call(adminTok, revokeRequest(adminClaims.ID, true))
		expectSyncStatus(t, rec, http.StatusOK)
		if out := decodeSync[SyncRevokeTokenResponse](t, rec); !out.Token.Current || out.Token.RevokedAt == nil {
			t.Errorf("self revoke = %+v", out)
		}
		expectSyncStatus(t, env.call(adminTok, SyncRequest{Action: SyncActionListTokens}), http.StatusUnauthorized)
		// 残った最後のサイト管理者 B は、取り消せない。
		expectSyncStatus(t, env.call(adminB, revokeRequest(claimsB.ID, true)), http.StatusConflict)
	})
}

// 同時に2人のサイト管理者が互いを取り消しても、サイト管理者が1人もいなくなることはない。
func TestSyncRevokeTokenConcurrentAdmins(t *testing.T) {
	env := newInviteSyncEnv(t)
	tokA, claimsA := env.recordedToken("A", auth.RoleAdmin)
	tokB, claimsB := env.recordedToken("B", auth.RoleAdmin)
	c := env.newClient()
	var wg sync.WaitGroup
	for _, p := range []struct{ tok, target string }{{tokA, claimsB.ID}, {tokB, claimsA.ID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.syncHTTP("Bearer "+p.tok, revokeRequest(p.target, false))
		}()
	}
	wg.Wait()
	var active int
	if err := env.db.Get(&active, `SELECT COUNT(*) FROM sync_tokens WHERE role = 'admin' AND revoked_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if active < 1 {
		t.Fatalf("active admin tokens = %d, want at least 1", active)
	}
}

// ID の無い招待トークンは、署名が正しくても公開サーバーで拒否する。
func TestSyncTokenWithoutIDRejected(t *testing.T) {
	env := newInviteSyncEnv(t)
	exp := time.Now().Add(time.Hour).Unix()
	legacy := func(sub string, role auth.Role) string {
		return signedToken(env.priv, `{"alg":"EdDSA","typ":"sorus-cap"}`,
			fmt.Sprintf(`{"sub":%q,"role":%q,"exp":%d,"iat":%d}`, sub, role, exp, time.Now().Unix()))
	}
	rec := env.call(legacy("昔のライター", auth.RoleWriter), submitRequest(SyncArticle{Title: "古い形式", ContentMarkdown: "本文"}))
	expectSyncStatus(t, rec, http.StatusUnauthorized)
	if !strings.Contains(rec.Body.String(), "新しい招待コード") || !strings.Contains(rec.Body.String(), "もう一度参加") {
		t.Errorf("response = %s, want understandable guidance to get a new invite", rec.Body.String())
	}
}

// 記録の無いトークン（公開サーバーの DB の無い場所で発行したものなど）は、初めて使われたときに記録され、取り消せる。
func TestSyncTokenRecordedOnFirstUse(t *testing.T) {
	env := newInviteSyncEnv(t)
	adminTok, _ := env.recordedToken("admin", auth.RoleAdmin)
	tok, claims, err := auth.IssueTokenClaims(env.priv, "どこかで発行", string(auth.RoleEditor), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionFetchPending}), http.StatusOK)
	rec := env.tokenRow(claims.ID)
	if rec.IssuedVia != TokenIssuedViaSeen || rec.Subject != "どこかで発行" || rec.Role != auth.RoleEditor || rec.IssuedAt.Unix() != claims.IssuedAt {
		t.Fatalf("recorded = %+v", rec)
	}
	// 2回目の利用で記録は変わらない。
	expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionFetchPending}), http.StatusOK)
	expectSyncStatus(t, env.call(adminTok, revokeRequest(claims.ID, false)), http.StatusOK)
	expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionFetchPending}), http.StatusUnauthorized)
}

// sorus token create は公開サーバーの稼働中に別の接続から記録する。同期 API の要求と並行しても書き込め、一覧に出る。
func TestRecordIssuedTokenWhileServerRunning(t *testing.T) {
	env := newInviteSyncEnv(t)
	adminTok, _ := env.recordedToken("admin", auth.RoleAdmin)
	var dbFile string
	if err := env.db.Get(&dbFile, `SELECT file FROM pragma_database_list WHERE name = 'main'`); err != nil || dbFile == "" {
		t.Fatalf("database file = %q, %v", dbFile, err)
	}
	cli, err := database.Connect(dbFile)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cli.Close()

	c := env.newClient()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// サーバー側も書き込む（招待の発行）ことで、書き込みの競合を起こす。
			c.syncHTTP("Bearer "+adminTok, issueInviteRequest(auth.RoleWriter, fmt.Sprint("並行", i), 1))
		}
	}()
	var ids []string
	for i := range 10 {
		_, claims, err := auth.IssueTokenClaims(env.priv, fmt.Sprint("cli", i), string(auth.RoleWriter), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := RecordIssuedToken(context.Background(), cli, claims, TokenIssuedViaCLI, ""); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("RecordIssuedToken while the server is running: %v", err)
		}
		ids = append(ids, claims.ID)
	}
	close(stop)
	wg.Wait()

	res := c.syncHTTP("Bearer "+adminTok, SyncRequest{Action: SyncActionListTokens})
	expectStatus(t, res, http.StatusOK)
	got := map[string]string{}
	for _, r := range decodeResponse[SyncListTokensResponse](t, res).Tokens {
		got[r.JTI] = r.IssuedVia
	}
	for _, id := range ids {
		if got[id] != TokenIssuedViaCLI {
			t.Errorf("token %s: issued_via = %q, want cli", id, got[id])
		}
	}
}
