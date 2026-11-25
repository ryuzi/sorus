package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

// newPublicMCPRemoteEnv は、公開専用モードで MCP を受け付ける公開サーバー（本番と同じ形）と、それにつなぐ PC。
func newPublicMCPRemoteEnv(t *testing.T) *remoteEnv {
	t.Helper()
	return &remoteEnv{local: newTestEnv(t), remote: newPublicMCPEnv(t)}
}

// 「AI・外部連携」で AI 機能を OFF にし「許可済みの連携も解除する」を選ぶと、この PC の MCP のトークンを取り消し、
// 「公開サーバーへ反映する」で公開サーバーの ai_enabled も OFF にして、公開サーバーの MCP・OAuth・同期 API の mcp_* を
// 404 にし、公開サーバーのトークンも取り消す。ON に戻せば公開サーバーでもまた使える。
func TestAIIntegrationsOffStopsPublicServerMCP(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newPublicMCPRemoteEnv(t)
	env.configure("管理者", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	issueMCPToken(adm, "AIアプリ", "30")

	writer := env.remote.token("ライターA", auth.RoleWriter)
	rec := env.remote.call(writer, SyncRequest{Action: SyncActionMCPIssueToken, MCP: &SyncMCPRequest{Name: "Claude Code", ExpiresDays: 30}})
	expectSyncStatus(t, rec, http.StatusOK)
	issued := decodeSync[SyncMCPIssueTokenResponse](t, rec)
	mcpPath := env.remote.server.cfg.MCPAPIPath
	if res, _ := env.remote.mcpCallAt(mcpPath, issued.Token, "tools/list", map[string]any{}); res.StatusCode != http.StatusOK {
		t.Fatalf("AI on: public MCP = %d, want 200", res.StatusCode)
	}
	if got := env.remote.get(oauthAuthServerMetadataPath).Code; got != http.StatusOK {
		t.Fatalf("AI on: public OAuth metadata = %d, want 200", got)
	}

	page := adm.get(aiIntegrationsPath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="ai-section-title" class="text-xl font-bold text-neutral-900">AI</h1>`, `name="ai_enabled" value="1" checked`, `id="ai-int-revoke-input"`,
		"この PC: 連携中の AI アプリ 0 件・API トークン 1 件", `data-remote-ai="unknown"`, `href="/admin/settings/mcp" id="ai-tab-mcp"`)
	expectNotContains(t, page.Body, `id="ai-int-links"`)
	nav := between(t, page.Body, `<nav class="admin-settings-sidebar`, `</nav>`)
	expectContains(t, nav, `href="/admin/settings/ai-integrations" aria-current="page"`)

	loc := expectRedirect(t, adm.postForm(aiIntegrationsPath, url.Values{"ai_enabled": {"0"}, "revoke_integrations": {"1"}}), aiIntegrationsPath)
	expectFlash(t, loc, "ai_integrations_revoked")
	if env.local.server.aiEnabled() {
		t.Fatal("AI is still on after turning it off")
	}
	if got := env.local.countRows(`SELECT COUNT(*) FROM mcp_tokens`); got != 0 {
		t.Errorf("local mcp_tokens = %d, want 0 (revoked)", got)
	}
	if !env.local.server.remoteRevokeMCPPending() {
		t.Error("the revoke request for the publish server was not recorded")
	}
	expectContains(t, adm.get(aiIntegrationsPath).Body, `data-remote-ai="pending"`, "まだ公開サーバーに反映されていません")
	// 公開サーバーはまだ ON のまま（反映するまでは変えない）。
	if res, _ := env.remote.mcpCallAt(mcpPath, issued.Token, "tools/list", map[string]any{}); res.StatusCode != http.StatusOK {
		t.Fatalf("before push: public MCP = %d, want 200", res.StatusCode)
	}

	msg := pushRemoteSettings(t, adm, http.StatusOK)
	for _, want := range []string{"公開サーバーで許可済みの AI アプリの連携（0 件）と API トークン（1 件）を取り消しました", "公開サーバーの外部 AI 連携（MCP）も止めました"} {
		if !strings.Contains(msg, want) {
			t.Errorf("push message = %q, want it to contain %q", msg, want)
		}
	}
	if got := env.remote.setting(settingAIEnabled); got != "0" {
		t.Errorf("remote ai_enabled = %q, want 0", got)
	}
	if got := env.remote.countRows(`SELECT COUNT(*) FROM mcp_tokens`); got != 0 {
		t.Errorf("remote mcp_tokens = %d, want 0 (revoked)", got)
	}
	if res, _ := env.remote.mcpCallAt(mcpPath, issued.Token, "tools/list", map[string]any{}); res.StatusCode != http.StatusNotFound {
		t.Errorf("AI off: public MCP = %d, want 404", res.StatusCode)
	}
	if got := env.remote.get(oauthAuthServerMetadataPath).Code; got != http.StatusNotFound {
		t.Errorf("AI off: public OAuth metadata = %d, want 404", got)
	}
	if rec := env.remote.call(writer, SyncRequest{Action: SyncActionMCPInfo}); rec.Code != http.StatusNotFound {
		t.Errorf("AI off: sync mcp_info = %d, want 404", rec.Code)
	}
	if env.local.server.remoteRevokeMCPPending() {
		t.Error("the revoke request is still pending after the publish server accepted it")
	}
	expectContains(t, adm.get(aiIntegrationsPath).Body, `data-remote-ai="synced"`, "公開サーバーでも AI 機能と外部の AI アプリとの連携（MCP）は止まっています")

	// ON に戻すと、公開サーバーでもまた使える（取り消したトークンは戻らない）。
	loc = expectRedirect(t, adm.postForm(aiIntegrationsPath, url.Values{"ai_enabled": {"0", "1"}}), aiIntegrationsPath)
	expectFlash(t, loc, "ai_integrations_saved_on")
	if msg := pushRemoteSettings(t, adm, http.StatusOK); strings.Contains(msg, "AI") {
		t.Errorf("push message after turning AI on = %q, want no AI note", msg)
	}
	if got := env.remote.setting(settingAIEnabled); got != "1" {
		t.Errorf("remote ai_enabled = %q, want 1", got)
	}
	rec = env.remote.call(writer, SyncRequest{Action: SyncActionMCPIssueToken, MCP: &SyncMCPRequest{Name: "Claude Code", ExpiresDays: 30}})
	expectSyncStatus(t, rec, http.StatusOK)
	again := decodeSync[SyncMCPIssueTokenResponse](t, rec)
	if res, _ := env.remote.mcpCallAt(mcpPath, again.Token, "tools/list", map[string]any{}); res.StatusCode != http.StatusOK {
		t.Errorf("AI back on: public MCP = %d, want 200", res.StatusCode)
	}
	if res, _ := env.remote.mcpCallAt(mcpPath, issued.Token, "tools/list", map[string]any{}); res.StatusCode == http.StatusOK {
		t.Error("a revoked token works again after turning AI back on")
	}
	expectContains(t, adm.get(aiIntegrationsPath).Body, `data-remote-ai="synced"`, "公開サーバーでも AI 機能と外部の AI アプリとの連携が使えます")
}

// 基本設定の AI 機能のスイッチを変えても、サイト基本設定が未反映になり、共通バナーの「公開サーバーへ反映」で公開サーバーへ届く。
func TestGeneralSettingsAIToggleMarksRemoteDirty(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newRemoteEnv(t)
	env.configure("管理者", auth.RoleAdmin)
	adm := env.local.loggedIn(admin)
	env.local.setAIEnabled(false)
	// 基本設定の保存と同じ比較（remoteGeneralSettings）で、AI 機能の ON/OFF の違いを未反映として拾う。
	before := env.local.server.remoteGeneralSettings()
	env.local.setAIEnabled(true)
	if after := env.local.server.remoteGeneralSettings(); after == before {
		t.Fatal("remoteGeneralSettings does not include the AI switch")
	}
	env.local.setAIEnabled(false)
	pushRemoteSettings(t, adm, http.StatusOK)
	if got := env.remote.setting(settingAIEnabled); got != "0" {
		t.Errorf("remote ai_enabled = %q, want 0", got)
	}
}

// 公開サーバーの sorus が古く AI 機能の ON/OFF を知らない（応答に ai_enabled が無い）ときは、反映できていないことと、
// 公開サーバーの外部 AI 連携が動いたままであることをはっきり知らせる。
func TestRemoteSettingsPushWarnsOldServerAI(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newRemoteEnv(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 応答を書き換えるため、圧縮しない応答を受け取る。
		r.Header.Del("Accept-Encoding")
		rec := httptest.NewRecorder()
		env.remote.server.Handler().ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if st, ok := m["settings"].(map[string]any); ok {
				delete(st, "ai_enabled")
				delete(st, "ai_forced_off")
				body, _ = json.Marshal(m)
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(proxy.Close)
	cfg := remoteConfig{URL: proxy.URL, SyncPath: env.remote.server.cfg.SyncAPIPath, Token: env.remote.token("管理者", auth.RoleAdmin)}
	if err := env.local.server.saveRemoteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	adm := env.local.loggedIn(admin)
	expectRedirect(t, adm.postForm(aiIntegrationsPath, url.Values{"ai_enabled": {"0"}}), aiIntegrationsPath)
	msg := pushRemoteSettings(t, adm, http.StatusOK)
	if !strings.Contains(msg, "公開サーバーの sorus が古いため、AI 機能の OFF は反映されていません（公開サーバーの外部 AI 連携は動いたままです）") {
		t.Errorf("push message = %q", msg)
	}
	if st := env.local.server.remoteAIStateSaved(); st == nil || !st.Old {
		t.Errorf("remote AI state = %+v, want old", st)
	}
	expectContains(t, adm.get(aiIntegrationsPath).Body, `data-remote-ai="old"`, "公開サーバーの sorus が古いため、AI 機能の ON/OFF を反映できません")
}

// 応答から足す文: 古い公開サーバー・公開サーバーの環境変数での強制 OFF・取り消した件数。
func TestRemoteAIResultNote(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
	on, off := true, false

	if note := s.remoteAIResultNote(c, SyncSettingsResponse{}, true, false); !strings.Contains(note, "AI 機能の ON/OFF は反映されていません") {
		t.Errorf("old server (AI on) note = %q", note)
	}
	if note := s.remoteAIResultNote(c, SyncSettingsResponse{}, false, false); !strings.Contains(note, "外部 AI 連携は動いたままです") {
		t.Errorf("old server (AI off) note = %q", note)
	}
	forced := SyncSettingsResponse{Settings: SyncSettingsValues{AIEnabled: &off, AIForcedOff: true}}
	if note := s.remoteAIResultNote(c, forced, true, false); !strings.Contains(note, "SORUS_AI=off") {
		t.Errorf("forced off note = %q", note)
	}
	if st := s.remoteAIStateSaved(); st == nil || st.Old || st.Enabled || !st.ForcedOff {
		t.Errorf("saved state = %+v, want forced off", st)
	}
	if note := s.remoteAIResultNote(c, SyncSettingsResponse{Settings: SyncSettingsValues{AIEnabled: &on}}, true, false); note != "" {
		t.Errorf("synced on note = %q, want empty", note)
	}
	if err := s.setRemoteRevokeMCPPending(true); err != nil {
		t.Fatal(err)
	}
	revoked := SyncSettingsResponse{Settings: SyncSettingsValues{AIEnabled: &off}, RevokedMCPTokens: 3, RevokedOAuthApps: 2}
	if note := s.remoteAIResultNote(c, revoked, false, true); !strings.Contains(note, "連携（2 件）と API トークン（3 件）を取り消しました") {
		t.Errorf("revoke note = %q", note)
	}
	if s.remoteRevokeMCPPending() {
		t.Error("revoke request still pending")
	}
}

// 公開サーバーに参加した直後に、この PC の AI 機能の ON/OFF を公開サーバーへ送る（AI を OFF にした PC が参加しても、
// 公開サーバーの外部 AI 連携が動いたままにならない）。設定を変えられない招待では送らない。古い公開サーバーなら知らせる。
func TestRemoteJoinSyncsAISwitch(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	t.Run("admin invite", func(t *testing.T) {
		env := newRemoteEnv(t)
		env.local.setAIEnabled(false)
		code, _ := env.inviteFor("編集長", auth.RoleAdmin)
		res := env.local.loggedIn(admin).postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}})
		expectRedirect(t, res, "/admin/settings/remote?flash=remote_joined_ai_off")
		if got := env.remote.setting(settingAIEnabled); got != "0" {
			t.Errorf("remote ai_enabled = %q, want 0", got)
		}
		if st := env.local.server.remoteAIStateSaved(); st == nil || st.Enabled || st.Old {
			t.Errorf("remote AI state = %+v, want off", st)
		}
	})
	t.Run("writer invite", func(t *testing.T) {
		env := newRemoteEnv(t)
		env.local.setAIEnabled(false)
		code, _ := env.inviteFor("ライター", auth.RoleWriter)
		res := env.local.loggedIn(admin).postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}})
		expectRedirect(t, res, "/admin/settings/remote?flash=remote_joined")
		if got := env.remote.countRows(`SELECT COUNT(*) FROM settings WHERE key = ?`, settingAIEnabled); got != 0 {
			t.Errorf("remote ai_enabled rows = %d, want unchanged (writer invites cannot sync settings)", got)
		}
	})
	t.Run("old server", func(t *testing.T) {
		env := newRemoteEnv(t)
		env.local.setAIEnabled(false)
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"action":"`+SyncActionSyncSettings+`"`) {
				// AI 機能の同期を知らない公開サーバーは ai_enabled を無視し、更新する設定が無いと断る。
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"更新する設定を指定してください"}`))
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			env.remote.server.Handler().ServeHTTP(w, r)
		}))
		t.Cleanup(proxy.Close)
		tok, _ := env.remote.recordedToken("編集長", auth.RoleAdmin)
		code, err := EncodeInviteCode(proxy.URL+env.remote.server.cfg.SyncAPIPath, tok)
		if err != nil {
			t.Fatal(err)
		}
		res := env.local.loggedIn(admin).postForm("/admin/settings/remote/join", url.Values{"invite_code": {code}})
		expectRedirect(t, res, "/admin/settings/remote?flash=remote_joined_ai_old")
		if st := env.local.server.remoteAIStateSaved(); st == nil || !st.Old {
			t.Errorf("remote AI state = %+v, want old", st)
		}
		page := env.local.loggedIn(admin).get("/admin/settings/remote?flash=remote_joined_ai_old")
		expectContains(t, page.Body, "公開サーバーの sorus が古いため、AI 機能の ON/OFF は反映されていません")
	})
}

// 「AI・外部連携」は AI 機能が OFF でも開け（ON に戻す入口）、設定の入口・サブメニューにも残る。環境変数で止めているときは
// 変えられない。管理者だけが開ける。英語でも訳して出す。
func TestAIIntegrationsPageAccess(t *testing.T) {
	t.Setenv(AIEnvVar, "")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	env.setAIEnabled(false)
	page := adm.get(aiIntegrationsPath)
	expectStatus(t, page, http.StatusOK)
	expectContains(t, page.Body, `id="ai-int-enabled" type="checkbox" name="ai_enabled" value="1" aria-describedby`, `data-remote-ai="none"`)
	expectNotContains(t, page.Body, `id="ai-int-links"`, " checked aria-describedby")
	// AI 機能が OFF でもタブ（使う AI・外部の AI アプリとつなぐ）は出さず、スイッチだけを出す。
	expectNotContains(t, page.Body, `id="ai-tabs"`)
	hub := adm.get("/admin/settings")
	expectContains(t, hub.Body, `href="/admin/settings/ai-integrations" id="hub-ai"`)
	expectNotContains(t, hub.Body, `id="hub-ai-member"`, `id="hub-mcp"`)
	// 基本設定には AI の ON/OFF を重ねて出さず、左メニューの「AI」から開く。
	general := adm.get("/admin/settings/general")
	expectNoGeneralAIToggle(t, general.Body)
	expectContains(t, general.Body, `href="/admin/settings/ai-integrations"`)

	ed := env.loggedIn(chief)
	if res := ed.get(aiIntegrationsPath); res.StatusCode != http.StatusForbidden {
		t.Errorf("editor GET = %d, want 403", res.StatusCode)
	}
	if res := ed.postForm(aiIntegrationsPath, url.Values{"ai_enabled": {"1"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("editor POST = %d, want 403", res.StatusCode)
	}

	adm.useEnglish()
	page = adm.get(aiIntegrationsPath)
	expectContains(t, page.Body, `id="ai-section-title" class="text-xl font-bold text-neutral-900">AI</h1>`, "Also revoke connected AI apps and API tokens (recommended)", "Not connected to a publish server.")
	expectNotContains(t, page.Body, "AI・外部連携", "公開サーバー")

	t.Setenv(AIEnvVar, "off")
	page = adm.get(aiIntegrationsPath)
	expectContains(t, page.Body, `id="ai-int-forced"`)
	expectNotContains(t, page.Body, `id="ai-int-save"`)
	loc := expectRedirect(t, adm.postForm(aiIntegrationsPath, url.Values{"ai_enabled": {"1"}}), aiIntegrationsPath)
	expectFlash(t, loc, "ai_integrations_forced")
	if env.server.aiEnabledSetting() {
		t.Error("the saved AI switch changed while SORUS_AI=off")
	}
}
