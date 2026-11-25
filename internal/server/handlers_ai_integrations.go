package server

// 「AI・外部連携」の画面（/admin/settings/ai-integrations）と、AI 機能の ON/OFF を公開サーバーへ反映する処理。
//
// AI 機能のマスタースイッチ（handlers_ai_switch.go）は、この PC だけでなく接続している公開サーバーにも効かせる。公開サーバー
// には管理画面が無いため、PC の ai_enabled を「公開サーバーへ反映する」（sync_settings の AIEnabled）で送り、公開サーバーの
// 外部 AI 連携（MCP・OAuth・同期 API の mcp_*）も止める。OFF にするときは、許可済みの AI アプリの連携と API トークンを
// この PC と公開サーバーの両方で取り消すこともできる（AI 機能を OFF にすると MCP 連携の画面が無くなり、後から取り消す
// 入口が無くなるため、OFF にする時点で選べるようにする）。
//
// この画面は AI 機能が OFF でも開ける（ON に戻す・公開サーバーの状態を確かめる入口のため。requireAIEnabled を付けない）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

// aiIntegrationsPath は「AI・外部連携」の画面。
const aiIntegrationsPath = "/admin/settings/ai-integrations"

const (
	// remoteSettingRevokeMCP は、公開サーバーの外部 AI 連携の取り消しを次の「公開サーバーへ反映する」で頼むかを保存する
	// settings のキー（"1"）。公開サーバーが取り消しを受け付けたら消す。
	remoteSettingRevokeMCP = "remote_revoke_mcp_pending"
	// remoteSettingAIState は、最後に公開サーバーへ反映したときの公開サーバーの AI 機能の状態（JSON。remoteAIState）。
	remoteSettingAIState = "remote_ai_state"
)

func init() {
	if !slices.Contains(pageNames, "settings_ai_integrations") {
		pageNames = append(pageNames, "settings_ai_integrations")
	}
	for key, msg := range map[string]string{
		"ai_integrations_saved_on":  "AI機能を使う設定にしました",
		"ai_integrations_saved_off": "AI機能を止めました",
		"ai_integrations_revoked":   "AI機能を止め、許可済みのAIアプリの連携とAPIトークンを解除しました",
		"ai_integrations_forced":    "AI機能はサーバーの環境変数で止められているため、変えられません",
		"remote_joined_ai_off":      "公開サーバーに参加しました。AI機能がOFFのため、公開サーバーの外部AI連携も止めました",
		"remote_joined_ai_old":      "公開サーバーに参加しました",
		"remote_joined_ai_failed":   "公開サーバーに参加しました",
	} {
		flashMessages[key] = msg
		flashKeys[key] = key
	}
}

// remoteAIState は最後に公開サーバーへ反映したときの、公開サーバーの AI 機能の状態。
type remoteAIState struct {
	// Old は公開サーバーの sorus が古く、AI 機能の ON/OFF を受け取れなかったか。
	Old bool `json:"old,omitempty"`
	// Enabled は公開サーバーで AI 機能（外部 AI 連携を含む）が使えるか。ForcedOff は公開サーバーの環境変数で OFF に固定されているか。
	Enabled   bool      `json:"enabled"`
	ForcedOff bool      `json:"forced_off,omitempty"`
	At        time.Time `json:"at"`
}

// remoteAIStateSaved は最後に記録した公開サーバーの AI 機能の状態を返す。記録が無ければ nil。
func (s *Server) remoteAIStateSaved() *remoteAIState {
	raw := s.getSetting(remoteSettingAIState, "")
	if raw == "" {
		return nil
	}
	var st remoteAIState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil
	}
	return &st
}

// saveRemoteAIState は公開サーバーの AI 機能の状態を記録する（失敗はログだけ）。
func (s *Server) saveRemoteAIState(c echo.Context, st remoteAIState) {
	st.At = clockNow().UTC()
	b, err := json.Marshal(st)
	if err == nil {
		err = upsertSetting(s.db, remoteSettingAIState, string(b))
	}
	if err != nil {
		c.Logger().Warnf("remote: record the AI state of the public server: %v", err)
	}
}

// remoteRevokeMCPPending は、次の反映で公開サーバーの外部 AI 連携の取り消しを頼むか。
func (s *Server) remoteRevokeMCPPending() bool {
	return s.getSetting(remoteSettingRevokeMCP, "") == "1"
}

// setRemoteRevokeMCPPending は公開サーバーの外部 AI 連携の取り消しを頼む印を付ける・外す。
func (s *Server) setRemoteRevokeMCPPending(on bool) error {
	if on {
		return upsertSetting(s.db, remoteSettingRevokeMCP, "1")
	}
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, remoteSettingRevokeMCP)
	return err
}

// remoteAIResultNote は「公開サーバーへ反映する」の応答 out から、AI 機能の ON/OFF の反映の結果を記録し、結果の文に足す文を
// 返す（足すことが無ければ空）。aiOn はこの PC が送った値、revoke は外部 AI 連携の取り消しを頼んだか。
func (s *Server) remoteAIResultNote(c echo.Context, out SyncSettingsResponse, aiOn, revoke bool) string {
	got := out.Settings.AIEnabled
	if got == nil {
		s.saveRemoteAIState(c, remoteAIState{Old: true})
		if aiOn {
			return "。公開サーバーの sorus が古いため、AI 機能の ON/OFF は反映されていません。公開サーバーの sorus を更新してください"
		}
		return "。公開サーバーの sorus が古いため、AI 機能の OFF は反映されていません（公開サーバーの外部 AI 連携は動いたままです）。公開サーバーの sorus を更新してください"
	}
	s.saveRemoteAIState(c, remoteAIState{Enabled: *got, ForcedOff: out.Settings.AIForcedOff})
	note := ""
	if revoke {
		if err := s.setRemoteRevokeMCPPending(false); err != nil {
			c.Logger().Warnf("remote: clear the MCP revoke request: %v", err)
		}
		note += fmt.Sprintf("。公開サーバーで許可済みの AI アプリの連携（%d 件）と API トークン（%d 件）を取り消しました", out.RevokedOAuthApps, out.RevokedMCPTokens)
	}
	switch {
	case aiOn && !*got && out.Settings.AIForcedOff:
		note += "。公開サーバーは環境変数 SORUS_AI=off で AI 機能が止められているため、公開サーバーでは OFF のままです"
	case !aiOn && !*got:
		note += "。公開サーバーの外部 AI 連携（MCP）も止めました"
	}
	return note
}

// syncRemoteAIOnJoin は公開サーバーに参加した直後に、この PC の AI 機能の ON/OFF だけを公開サーバーへ送る
// （AI 機能を OFF にした PC が参加したのに、公開サーバーの外部 AI 連携が動いたままにならないように）。サイト基本設定の
// 反映の権限が無い招待（writer・editor）では何もしない。参加後に出す通知のキーを返す。
func (s *Server) syncRemoteAIOnJoin(c echo.Context, cfg remoteConfig, role auth.Role) string {
	if !auth.CanSyncSettings(role) {
		return "remote_joined"
	}
	// 前の接続先に頼むつもりだった取り消しは、新しい接続先には頼まない。
	if err := s.setRemoteRevokeMCPPending(false); err != nil {
		c.Logger().Warnf("remote join: clear the MCP revoke request: %v", err)
	}
	aiOn := s.aiEnabled()
	ctx, cancel := context.WithTimeout(c.Request().Context(), remoteJoinTimeout)
	defer cancel()
	var out SyncSettingsResponse
	err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionSyncSettings, Settings: &SyncSettings{AIEnabled: &aiOn}}, &out)
	var re *remoteError
	switch {
	case errors.As(err, &re) && re.Status == http.StatusBadRequest, err == nil && out.Settings.AIEnabled == nil:
		// AI 機能の同期を知らない古い公開サーバーは、ai_enabled を無視して「更新する設定が無い」と断る。
		s.saveRemoteAIState(c, remoteAIState{Old: true})
		return "remote_joined_ai_old"
	case err != nil:
		c.Logger().Warnf("remote join: sync the AI switch: %v", err)
		// 送れなかった AI 機能の ON/OFF は、共通バナーの「公開サーバーへ反映する」で送れるよう未反映として残す。
		s.markRemoteDirty(c, remoteDirtyGeneral)
		return "remote_joined_ai_failed"
	}
	s.saveRemoteAIState(c, remoteAIState{Enabled: *out.Settings.AIEnabled, ForcedOff: out.Settings.AIForcedOff})
	if !aiOn {
		return "remote_joined_ai_off"
	}
	return "remote_joined"
}

// aiIntegrationsData は settings_ai_integrations.html に渡すデータ。
type aiIntegrationsData struct {
	// AIEnabledSetting は基本設定で保存した AI 機能の ON/OFF（環境変数の強制 OFF を含めない）。
	AIEnabledSetting bool
	// LocalTokens・LocalApps はこの PC で発行済みの MCP の API トークンと、許可中の AI アプリの連携の数。
	LocalTokens int
	LocalApps   int
	// RemoteConnected は公開サーバーに接続しているか。RemoteHost はその接続先、RemoteCanPush は「公開サーバーへ反映する」を
	// 押せるか（招待が admin）。RemoteDirty は AI 機能の ON/OFF を含むサイト基本設定が未反映か。
	RemoteConnected bool
	RemoteHost      string
	RemoteCanPush   bool
	RemoteDirty     bool
	// RemoteState は最後に反映したときの公開サーバーの AI 機能の状態（無ければ nil）。RevokePending は公開サーバーの
	// 外部 AI 連携の取り消しを、まだ頼めていないか。
	RemoteState   *remoteAIState
	RevokePending bool
}

// RemoteMatches は公開サーバーの AI 機能の状態が、この PC の設定と同じか（反映済みか）。
func (d aiIntegrationsData) RemoteMatches() bool {
	return d.RemoteState != nil && !d.RemoteState.Old && !d.RemoteDirty && !d.RevokePending &&
		(d.RemoteState.Enabled == d.AIEnabledSetting || (d.AIEnabledSetting && d.RemoteState.ForcedOff))
}

func (s *Server) aiIntegrationsData(c echo.Context) aiIntegrationsData {
	ctx := c.Request().Context()
	d := aiIntegrationsData{AIEnabledSetting: s.aiEnabledSetting(), RevokePending: s.remoteRevokeMCPPending()}
	_ = s.db.GetContext(ctx, &d.LocalTokens, `SELECT COUNT(*) FROM mcp_tokens`)
	_ = s.db.GetContext(ctx, &d.LocalApps, `SELECT COUNT(*) FROM oauth_tokens WHERE revoked_at IS NULL
		AND julianday(COALESCE(refresh_expires_at, expires_at)) > julianday(?)`, clockNow().UTC())
	cfg := s.loadRemoteConfig()
	if cfg.connected() {
		d.RemoteConnected = true
		d.RemoteHost = remoteServerHost(cfg.URL)
		d.RemoteDirty = slices.Contains(s.remoteDirty(), remoteDirtyGeneral)
		d.RemoteState = s.remoteAIStateSaved()
		if info, err := parseRemoteToken(cfg.Token, time.Now()); err == nil && !info.Expired && auth.CanSyncSettings(info.Role) {
			d.RemoteCanPush = true
		}
	}
	return d
}

// aiSectionPaths は左メニューの「AI」1項目にまとめた画面。どれでも先頭に「AI機能を使う」のスイッチとタブ（使う AI・
// 外部の AI アプリとつなぐ）を出す（partials/ai_tabs.html）。URL は以前のまま3本を使う。
var aiSectionPaths = []string{aiIntegrationsPath, aiSettingsPath, "/admin/settings/mcp"}

// layoutAISwitch は管理者が AI の画面（aiSectionPaths）を開いたとき、先頭のスイッチのデータを p.AISwitch に入れる。
func (s *Server) layoutAISwitch(c echo.Context, p *page) {
	if p.User == nil || !p.User.HasRole(auth.RoleAdmin) || s.cfg.PublicOnly || !p.AtPath(aiSectionPaths...) {
		return
	}
	d := s.aiIntegrationsData(c)
	p.AISwitch = &d
}

// handleAIIntegrations は「AI・外部連携」の画面を返す。
func (s *Server) handleAIIntegrations(c echo.Context) error {
	return s.render(c, http.StatusOK, "settings_ai_integrations", pageTitle(c, "settings_nav_ai_integrations"), s.aiIntegrationsData(c))
}

// handleAIIntegrationsSave は AI 機能の ON/OFF を保存する。OFF にするときに revoke_integrations が付いていれば、この PC の
// MCP の API トークンと AI アプリの連携をすべて取り消し、公開サーバーに接続していれば次の「公開サーバーへ反映する」で
// 公開サーバーの連携も取り消してもらう。ON/OFF が変わったら、公開サーバーへ未反映として記録する（共通バナーから反映できる）。
func (s *Server) handleAIIntegrationsSave(c echo.Context) error {
	if s.aiEnvForcedOff() {
		return c.Redirect(http.StatusSeeOther, aiIntegrationsPath+"?flash=ai_integrations_forced")
	}
	before := s.aiEnabledSetting()
	on := formChecked(c, settingAIEnabled)
	revoke := !on && c.FormValue("revoke_integrations") == "1"
	if err := upsertSetting(s.db, settingAIEnabled, formatAIEnabled(on)); err != nil {
		return err
	}
	flash := "ai_integrations_saved_on"
	if !on {
		flash = "ai_integrations_saved_off"
	}
	if revoke {
		tokens, apps, err := revokeAllMCPAccess(c.Request().Context(), s.db)
		if err != nil {
			return err
		}
		c.Logger().Infof("ai integrations: revoked %d MCP tokens and %d OAuth app connections", tokens, apps)
		flash = "ai_integrations_revoked"
		if s.loadRemoteConfig().connected() {
			if err := s.setRemoteRevokeMCPPending(true); err != nil {
				return err
			}
		}
	}
	if before != on || revoke {
		s.markRemoteDirty(c, remoteDirtyGeneral)
	}
	return c.Redirect(http.StatusSeeOther, aiIntegrationsPath+"?flash="+flash)
}
