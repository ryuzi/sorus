package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
)

// 「公開サーバー」画面の招待コードでの参加と、サイト管理者向けの招待の発行・一覧・取り消し。
// ブラウザはローカルの /admin/settings/remote/join と /admin/remote/* を呼び、ローカル CMS が公開サーバーの
// 同期 API（issue_invite・list_tokens・revoke_token）へ中継する。トークンと招待コードは、発行した直後の
// 応答で1回だけブラウザに渡し、ローカル CMS には保存しない。

// remoteJoinTimeout は参加するときの接続の確認の制限時間。返事を待つ人を長く待たせない。
const remoteJoinTimeout = 20 * time.Second

func init() {
	flashMessages["remote_joined"] = "公開サーバーに参加しました"
}

// remoteRole はロールを、参加する人に見せる言葉にしたもの。
type remoteRole struct {
	Label       string
	Description string
}

// remoteRoleWords はロールごとの言葉。
var remoteRoleWords = map[auth.Role]remoteRole{
	auth.RoleWriter: {"ライター", "記事を書いて公開サーバーへ提出できます"},
	auth.RoleEditor: {"編集者", "記事の提出に加えて、届いた記事の校閲と公開ができます"},
	auth.RoleAdmin:  {"サイト管理者", "記事の校閲と公開、サイト設定の反映、招待の発行と取り消しができます"},
}

// remoteRoleWordsEN は remoteRoleWords の英語版（管理画面を英語で表示するとき）。
var remoteRoleWordsEN = map[auth.Role]remoteRole{
	auth.RoleWriter: {"Writer", "Can write articles and submit them to the publish server"},
	auth.RoleEditor: {"Editor", "Can submit articles and also review and publish received articles"},
	auth.RoleAdmin:  {"Site administrator", "Can review and publish articles, push site settings, and issue and revoke invites"},
}

// roleWordsFor は lang（ja・en）でのロールの言葉を返す。
func roleWordsFor(role auth.Role, lang string) remoteRole {
	if i18n.Normalize(lang) == i18n.EN {
		if w, ok := remoteRoleWordsEN[role]; ok {
			return w
		}
	}
	return roleWords(role)
}

// formatDateFor は日付を lang（ja・en）の書き方にする。英語は 2006-01-02。
func formatDateFor(t time.Time, lang string) string {
	if i18n.Normalize(lang) == i18n.EN {
		return t.In(jst).Format("2006-01-02")
	}
	return formatJapaneseDate(t)
}

// expiryTextFor は expiryText を lang（ja・en）の文にする。
func expiryTextFor(exp, now time.Time, lang string) string {
	if i18n.Normalize(lang) != i18n.EN {
		return expiryText(exp, now)
	}
	if !now.Before(exp) {
		return "Expired (was valid until " + formatDateFor(exp, lang) + ")"
	}
	e, n := exp.In(jst), now.In(jst)
	days := int(time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, time.UTC).
		Sub(time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
	if days == 0 {
		return "Until " + e.Format("15:04") + " today"
	}
	return fmt.Sprintf("Until %s (%d days left)", formatDateFor(exp, lang), days)
}

func roleWords(role auth.Role) remoteRole {
	if w, ok := remoteRoleWords[role]; ok {
		return w
	}
	return remoteRole{Label: string(role)}
}

// formatJapaneseDate は t を「2026年10月28日」の形で返す。
func formatJapaneseDate(t time.Time) string {
	t = t.In(jst)
	return fmt.Sprintf("%d年%d月%d日", t.Year(), int(t.Month()), t.Day())
}

// expiryText は有効期限 exp を「2027年10月28日まで（あと365日）」の形で返す。日数は日本時間の暦の日付で数える。
// 今日のうちに切れるなら「今日の 14:00 まで」、期限切れなら「期限切れ（…）」。
func expiryText(exp, now time.Time) string {
	if !now.Before(exp) {
		return "期限切れ（" + formatJapaneseDate(exp) + "まで）"
	}
	e, n := exp.In(jst), now.In(jst)
	days := int(time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, time.UTC).
		Sub(time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
	if days == 0 {
		return "今日の " + e.Format("15:04") + " まで"
	}
	return fmt.Sprintf("%sまで（あと%d日）", formatJapaneseDate(exp), days)
}

// pingRemote は公開サーバーに存在しない action を送り、トークンが受け入れられるかを確かめる。公開サーバーは
// トークンを検証してから action を判定するため、400（未対応の action）が返れば接続できたと分かる。
// 接続できたときは公開サーバーの HTTP ステータスと nil、できなければ remoteError を返す。
func pingRemote(ctx context.Context, cfg remoteConfig) (int, error) {
	err := callRemote(ctx, cfg, SyncRequest{Action: remotePingAction}, nil)
	var re *remoteError
	switch {
	case err == nil:
		return http.StatusOK, nil
	case errors.As(err, &re) && re.Status == http.StatusBadRequest:
		return re.Status, nil
	}
	return 0, err
}

// handleRemoteJoin は招待コードを読み、公開サーバーに接続できることを確かめてから、同期 API の URL とトークンを
// 保存する（接続テストと保存を1回の操作で行う）。接続できなければ何も保存せず、貼り付けた招待コードを残して
// 画面を表示し直す。
func (s *Server) handleRemoteJoin(c echo.Context) error {
	raw := c.FormValue("invite_code")
	fail := func(msg string) error {
		d := s.remoteSettingsData(c, s.loadRemoteConfig())
		d.JoinCode = strings.TrimSpace(raw)
		return s.renderError(c, http.StatusUnprocessableEntity, "settings_remote", pageTitle(c, "settings_nav_publish"), msg, d)
	}
	if strings.TrimSpace(raw) == "" {
		return fail("サイト管理者から受け取った招待コードを貼り付けてください")
	}
	inv, err := DecodeInviteCode(raw)
	if err != nil {
		return fail(err.Error())
	}
	base, syncPath, err := normalizeRemoteURL(inv.URL)
	if err != nil {
		return fail(err.Error())
	}
	cfg := remoteConfig{URL: base, SyncPath: syncPath, Token: normalizeRemoteToken(inv.Token)}
	info, err := parseRemoteToken(cfg.Token, time.Now())
	if err != nil {
		return fail(err.Error())
	}
	if info.Expired {
		return fail("この招待コードは有効期限が切れています。サイト管理者に新しい招待コードを発行してもらってください")
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), remoteJoinTimeout)
	defer cancel()
	if _, err := pingRemote(ctx, cfg); err != nil {
		var re *remoteError
		if !errors.As(err, &re) {
			return err
		}
		if re.cause != nil {
			c.Logger().Warnf("%v", re)
		}
		return fail("公開サーバーに接続できなかったため、まだ参加していません。" + re.Message)
	}
	if err := s.saveRemoteConfig(cfg); err != nil {
		return err
	}
	// 参加した直後に、この PC の AI 機能の ON/OFF を公開サーバーへ送る（handlers_ai_integrations.go）。
	flash := s.syncRemoteAIOnJoin(c, cfg, info.Role)
	return c.Redirect(http.StatusSeeOther, "/admin/settings/remote?flash="+flash)
}

// requireRemoteAdmin は招待の管理に使う保存済みの設定を返す（トークンが admin で期限内であること）。
func (s *Server) requireRemoteAdmin() (remoteConfig, *remoteTokenInfo, error) {
	cfg, err := s.requireRemote(auth.CanManageInvites, "招待の管理")
	if err != nil {
		return cfg, nil, err
	}
	// requireRemote でトークンを読めることは確かめてある。
	info, err := parseRemoteToken(cfg.Token, time.Now())
	if err != nil {
		return cfg, nil, err
	}
	return cfg, info, nil
}

// remoteTokenItem は「発行した招待」の表の1行。日時と状態は画面にそのまま出せる言葉にする。
type remoteTokenItem struct {
	JTI         string    `json:"jti"`
	Subject     string    `json:"sub"`
	Role        auth.Role `json:"role"`
	RoleLabel   string    `json:"role_label"`
	IssuedAt    string    `json:"issued_at"`
	ExpiresAt   string    `json:"expires_at"`
	Status      string    `json:"status"` // active・expired・revoked
	StatusLabel string    `json:"status_label"`
	IssuedBy    string    `json:"issued_by"`
	// Current はこのアプリが使っている招待か。
	Current bool `json:"current"`
}

func newRemoteTokenItem(r SyncTokenRecord, now time.Time) remoteTokenItem {
	item := remoteTokenItem{
		JTI:       r.JTI,
		Subject:   r.Subject,
		Role:      r.Role,
		RoleLabel: roleWords(r.Role).Label,
		IssuedAt:  formatJapaneseDate(r.IssuedAt),
		ExpiresAt: formatJapaneseDate(r.ExpiresAt),
		Current:   r.Current,
	}
	switch {
	case r.RevokedAt != nil:
		item.Status, item.StatusLabel = "revoked", "取り消し済み（"+formatJapaneseDate(*r.RevokedAt)+"）"
	case !now.Before(r.ExpiresAt):
		item.Status, item.StatusLabel = "expired", "期限切れ"
	default:
		item.Status, item.StatusLabel = "active", "使えます"
	}
	switch r.IssuedVia {
	case TokenIssuedViaAPI:
		item.IssuedBy = r.IssuedBy
	case TokenIssuedViaCLI:
		item.IssuedBy = "サーバーで発行"
	default:
		item.IssuedBy = "不明（使われたときに記録）"
	}
	return item
}

// newRemoteTokenItemFor は newRemoteTokenItem の表示用の文言を lang（ja・en）にする。
func newRemoteTokenItemFor(r SyncTokenRecord, now time.Time, lang string) remoteTokenItem {
	item := newRemoteTokenItem(r, now)
	if i18n.Normalize(lang) != i18n.EN {
		return item
	}
	item.RoleLabel = roleWordsFor(r.Role, lang).Label
	item.IssuedAt, item.ExpiresAt = formatDateFor(r.IssuedAt, lang), formatDateFor(r.ExpiresAt, lang)
	switch item.Status {
	case "revoked":
		item.StatusLabel = "Revoked (" + formatDateFor(*r.RevokedAt, lang) + ")"
	case "expired":
		item.StatusLabel = "Expired"
	default:
		item.StatusLabel = "Active"
	}
	switch r.IssuedVia {
	case TokenIssuedViaAPI:
	case TokenIssuedViaCLI:
		item.IssuedBy = "Issued on the server"
	default:
		item.IssuedBy = "Unknown (recorded when used)"
	}
	return item
}

// handleRemoteTokens は公開サーバーが記録している発行済みの招待を一覧で返す。
func (s *Server) handleRemoteTokens(c echo.Context) error {
	cfg, _, err := s.requireRemoteAdmin()
	if err != nil {
		return err
	}
	var out SyncListTokensResponse
	if err := callRemote(c.Request().Context(), cfg, SyncRequest{Action: SyncActionListTokens}, &out); err != nil {
		return remoteFailure(c, err)
	}
	now := time.Now()
	items := make([]remoteTokenItem, 0, len(out.Tokens))
	for _, r := range out.Tokens {
		items = append(items, newRemoteTokenItemFor(r, now, string(language(c))))
	}
	return c.JSON(http.StatusOK, map[string]any{
		"tokens": items,
	})
}

// handleRemoteInvite は名前・立場・有効日数を受け取り、公開サーバーで招待を発行して招待コードを返す。
// 招待コードはこの応答でしか受け取れない。
func (s *Server) handleRemoteInvite(c echo.Context) error {
	cfg, _, err := s.requireRemoteAdmin()
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if name == "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "招待する人の名前を入力してください")
	}
	role := auth.Role(c.FormValue("role"))
	if _, ok := remoteRoleWords[role]; !ok {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "招待する人の立場を選んでください")
	}
	days, err := strconv.Atoi(strings.TrimSpace(c.FormValue("ttl_days")))
	if err != nil || days < 1 || days > maxInviteTTLDays {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("有効期間は1日から%d日までで選んでください", maxInviteTTLDays))
	}
	var out SyncIssueInviteResponse
	req := SyncRequest{Action: SyncActionIssueInvite, Role: string(role), Subject: name, TTLDays: days}
	if err := callRemote(c.Request().Context(), cfg, req, &out); err != nil {
		return remoteFailure(c, err)
	}
	// 古い公開サーバーなど、招待コードとして読めない応答は渡さない。
	if _, err := DecodeInviteCode(out.InviteCode); err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"message": "公開サーバーから正しい招待コードを受け取れませんでした。公開サーバーの sorus を更新してください"})
	}
	lang := string(language(c))
	msg := fmt.Sprintf("%sさん（%s）の招待コードを作りました", out.Subject, roleWords(out.Role).Label)
	if i18n.Normalize(lang) == i18n.EN {
		msg = fmt.Sprintf("Created an invite code for %s (%s)", out.Subject, roleWordsFor(out.Role, lang).Label)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message":     msg,
		"invite_code": out.InviteCode,
		"jti":         out.JTI,
		"sub":         out.Subject,
		"role_label":  roleWordsFor(out.Role, lang).Label,
		"expires_at":  formatDateFor(out.ExpiresAt, lang),
	})
}

// handleRemoteRevoke は発行済みの招待を公開サーバーで取り消す。このアプリが使っている招待を取り消すには
// confirm_self が要る（公開サーバーも同じ確認をする）。
func (s *Server) handleRemoteRevoke(c echo.Context) error {
	jti := c.Param("jti")
	if !auth.ValidTokenID(jti) {
		return echo.NewHTTPError(http.StatusBadRequest, "取り消す招待の指定が正しくありません")
	}
	cfg, info, err := s.requireRemoteAdmin()
	if err != nil {
		return err
	}
	self := jti == info.ID
	confirmSelf := c.FormValue("confirm_self") != ""
	if self && !confirmSelf {
		return echo.NewHTTPError(http.StatusConflict, "このアプリが使っている招待です。取り消すと公開サーバーに接続できなくなります")
	}
	var out SyncRevokeTokenResponse
	req := SyncRequest{Action: SyncActionRevokeToken, JTI: jti, ConfirmSelf: self && confirmSelf}
	if err := callRemote(c.Request().Context(), cfg, req, &out); err != nil {
		return remoteFailure(c, err)
	}
	lang := string(language(c))
	msg := fmt.Sprintf("%sさんの招待を取り消しました。この招待ではもう公開サーバーに接続できません", out.Token.Subject)
	if self {
		msg = "このアプリが使っていた招待を取り消しました。公開サーバーに接続し直すには、新しい招待コードを用意して、" + InviteJoinHint
	}
	if i18n.Normalize(lang) == i18n.EN {
		msg = fmt.Sprintf("Revoked the invite for %s. It can no longer be used to connect to the publish server.", out.Token.Subject)
		if self {
			msg = "Revoked the invite this app was using. To reconnect to the publish server, get a new invite code and join again under Settings → Publishing → Connect to a publish server."
		}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message": msg,
		"token":   newRemoteTokenItemFor(out.Token, time.Now(), lang),
	})
}

// remoteServerHost は公開サーバーの URL から、画面に見せるホスト名を取り出す。
func remoteServerHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}
