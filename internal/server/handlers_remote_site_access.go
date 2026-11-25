package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
)

// 「公開サーバー」画面の「公開サーバーの閲覧制限」。公開サーバー自身の閲覧制限（Basic 認証）を、同期 API の
// get_site_access・set_site_access で確認・変更する（公開サーバー側は handlers_sync_site_access.go）。
//
// - この PC（サーバーモードなら「サイト設定」の閲覧制限がある。デスクトップモードには無い）とは別の設定で、この PC には
//   保存しない。画面を開くたびに公開サーバーに問い合わせる。
// - 公開サーバーに問い合わせられない・応答を読めないときは「分からない」と表示し、保護されているとは表示しない。
// - 公開サーバーの設定とは別に、公開サイトをパスワードなしで外から開いてみて、実際にパスワードを求められるかを確かめる
//   （probeSiteAccess）。開くのはトップページと、公開サーバーが挙げた代表のパス（フィード・最新の記事・画像）だけで、
//   CDN に残るほかのキャッシュや http:// など別のアドレスは確かめない。そのため、すべてパスワードを求めても「確かめた
//   範囲では求められた」（checked）とだけ示し、サイト全体の保護の完了とは言わない。パスワードなしに見えるとき、確かめ
//   られないとき（接続できない・転送された・公開されていないアドレスなど）も、保護されているとは示さない。
// - 公開サーバーが返したサイトの URL（site_url）は、接続先として設定した公開サーバー自身でなければ、インターネットに
//   公開されたアドレスにだけ接続する（publicDialer。この PC 内・社内のサービスへ接続させない）。
// - 閲覧用のパスワードは https の公開サーバーにだけ送り（この PC 内の http://localhost にも送らない）、画面・応答・ログに
//   出さない。公開サーバーが、この接続を TLS で直接受けていないと答えたとき（前段のプロキシで https を終える構成など。
//   SyncSiteAccessResponse.InsecureTransport）も送らない。どちらの場合も保存のフォームを使えなくし、理由を表示する
//   （remoteSiteAccessView.CanSave・SaveBlocked）。公開サーバーも、TLS で直接届いた接続でしか受け付けない。

const (
	// remoteSiteAccessTimeout は閲覧制限の確認・変更で、公開サーバーへの1回の要求の制限時間。
	remoteSiteAccessTimeout = 20 * time.Second
	// siteAccessProbeTimeout は公開サイトを外から開いて確かめる1回の要求の制限時間。
	siteAccessProbeTimeout = 10 * time.Second
	// maxSiteAccessProbeBody は確かめるときに読み捨てる応答の本文の上限。
	maxSiteAccessProbeBody = 64 << 10
)

// 総合の判定（remoteSiteAccessView.Status）。
const (
	// siteAccessStatusChecked は公開サーバーで有効で、外から開いた URL がすべてパスワードを求めた状態。確かめたのは
	// 一部の URL だけで、CDN に残るほかのキャッシュは確かめていないため、サイト全体が守られているとは言わない。
	siteAccessStatusChecked = "checked"
	// siteAccessStatusUnverified は公開サーバーで有効だが、外から確かめられなかった URL がある状態。
	siteAccessStatusUnverified = "unverified"
	// siteAccessStatusExposed は公開サーバーで有効なのに、外から開くとパスワードなしで表示される URL がある状態。
	siteAccessStatusExposed = "exposed"
	// siteAccessStatusInactive は有効に設定されているが、公開サーバーで働いていない状態。
	siteAccessStatusInactive = "inactive"
	// siteAccessStatusOff は閲覧制限が無効な状態。
	siteAccessStatusOff = "off"
)

// 外から開いた結果（remoteSiteAccessCheck.Result）。
const (
	siteAccessCheckProtected = "protected"
	siteAccessCheckOpen      = "open"
	siteAccessCheckUnknown   = "unknown"
)

// siteAccessProbeClient は、接続先として設定した公開サーバーの公開サイトを外から開いて確かめる HTTP クライアント。
// Cookie・資格情報は送らず、リダイレクトは追わない（転送先は確かめた結果にしない）。
var siteAccessProbeClient = &http.Client{
	Timeout:       siteAccessProbeTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// siteAccessPublicClient は、公開サーバーが返したサイトの URL を外から開いて確かめる HTTP クライアント。
// siteAccessProbeClient と同じく資格情報を送らずリダイレクトを追わず、加えて公開されたアドレスにだけ接続する
// （環境変数のプロキシは使わない）。
func (s *Server) siteAccessPublicClient() *http.Client {
	d := s.siteAccessDialer
	if d.timeout == 0 {
		d.timeout = siteAccessProbeTimeout
	}
	return &http.Client{
		Timeout:       siteAccessProbeTimeout,
		CheckRedirect: siteAccessProbeClient.CheckRedirect,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         d.DialContext,
			DisableKeepAlives:   true,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: siteAccessProbeTimeout,
		},
	}
}

// remoteSiteAccessCheck は公開サイトの URL を外から開いた結果1件。
type remoteSiteAccessCheck struct {
	URL    string `json:"url"`
	Result string `json:"result"`
	// Status は応答の HTTP ステータス（接続できなければ 0）。
	Status int `json:"status,omitempty"`
	// Label は結果の説明（表示言語）。
	Label string `json:"label"`
}

// remoteSiteAccessView は画面に出す公開サーバーの閲覧制限の状態。パスワードとハッシュは含まない。
type remoteSiteAccessView struct {
	Enabled     bool   `json:"enabled"`
	Active      bool   `json:"active"`
	User        string `json:"user"`
	PasswordSet bool   `json:"password_set"`
	Revision    string `json:"revision"`
	// Status・Tone・Summary は総合の判定と、その色（ok・warn・error・neutral）・説明。
	Status  string `json:"status"`
	Tone    string `json:"tone"`
	Summary string `json:"summary"`
	// StateLabel・PasswordLabel は公開サーバーの設定と、パスワードを設定済みかの表示。
	StateLabel    string                  `json:"state_label"`
	PasswordLabel string                  `json:"password_label"`
	Checks        []remoteSiteAccessCheck `json:"checks"`
	CheckedLabel  string                  `json:"checked_label"`
	// CanSave は、この PC から閲覧制限を変更できるか。false なら SaveBlocked に理由（表示言語）があり、画面は保存の
	// フォームを使えなくする（公開サーバーの URL が https でない、公開サーバーがこの接続を TLS で直接受けていない）。
	CanSave     bool   `json:"can_save"`
	SaveBlocked string `json:"save_blocked,omitempty"`
}

// remoteSiteAccessFailure は閲覧制限の確認・変更の失敗の応答。Status は不明であることを表す "unknown"。
type remoteSiteAccessFailure struct {
	Message      string `json:"message"`
	Status       string `json:"status"`
	RemoteStatus int    `json:"remote_status"`
	// OutcomeUnknown は変更を送った後に失敗し、反映されたかどうか分からないか。
	OutcomeUnknown bool `json:"outcome_unknown,omitempty"`
	// Conflict はほかの PC が先に変更していたか。State はそのときに読み直した最新の状態（読めなければ無い）。
	Conflict bool                  `json:"conflict,omitempty"`
	State    *remoteSiteAccessView `json:"state,omitempty"`
	// Blocked は、この接続では閲覧制限を変更できないため送らなかった（または公開サーバーが断った）か。何も変わって
	// おらず、入力を直しても保存できないため、画面は保存のフォームを使えなくする。
	Blocked bool `json:"blocked,omitempty"`
}

// handleRemoteSiteAccess は公開サーバーの閲覧制限の状態を、外から確かめた結果とともに返す。
// 確かめられなければ 502 と理由を返し、保護されているとは返さない。
func (s *Server) handleRemoteSiteAccess(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	cfg, err := s.requireRemote(auth.CanSyncSettings, "公開サーバーの閲覧制限の確認")
	if err != nil {
		return err
	}
	lang := language(c)
	st, err := fetchRemoteSiteAccess(c.Request().Context(), cfg)
	if err != nil {
		return siteAccessFailure(c, lang, err, siteAccessPhaseRead)
	}
	return c.JSON(http.StatusOK, s.siteAccessView(c.Request().Context(), lang, cfg, st))
}

// handleRemoteSiteAccessSave は公開サーバーの閲覧制限を変更する。フォームは enabled（"1" で有効）・user・
// password（空なら保存済みのパスワードを使い続ける）・revision（画面に読み込んだときの版）。保存できたら、外から
// 確かめた結果とともに新しい状態を返す。ほかの PC が先に変更していれば 409 と最新の状態を返し、何も変えない。
func (s *Server) handleRemoteSiteAccessSave(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	cfg, err := s.requireRemote(auth.CanSyncSettings, "公開サーバーの閲覧制限の変更")
	if err != nil {
		return err
	}
	lang := language(c)
	if !remoteEndpointSecure(cfg) {
		return c.JSON(http.StatusUnprocessableEntity, remoteSiteAccessFailure{
			Message: i18n.Translate(string(lang), "remote_access_insecure"), Status: siteAccessCheckUnknown, Blocked: true})
	}
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "フォームを読み取れませんでした")
	}
	if !form.Has("revision") {
		return c.JSON(http.StatusUnprocessableEntity, remoteSiteAccessFailure{
			Message: i18n.Translate(string(lang), "remote_access_revision_required"), Status: siteAccessCheckUnknown})
	}
	revision := strings.TrimSpace(form.Get("revision"))
	enabled := form.Get("enabled") == "1"
	user := strings.TrimSpace(form.Get("user"))
	pass := form.Get("password")
	// 形式の誤りは公開サーバーに送る前に断る（保存済みのパスワードの有無は公開サーバーが確かめる）。
	if err := validateBasicAuthInput(enabled, user, pass, true); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, remoteSiteAccessFailure{
			Message: i18n.Translate(string(lang), err.Error()), Status: siteAccessCheckUnknown})
	}

	ctx := c.Request().Context()
	// 閲覧用のパスワードを送る前に、公開サーバーがこの接続を TLS で直接受けているかを確かめる。受けていなければ
	// （前段のプロキシで https を終え、sorus までを http で中継する構成など）、送ると途中を平文で流れるため送らない。
	// 確かめられなければ送らない（何も変わっていないことは分かっている）。
	cur, err := fetchRemoteSiteAccess(ctx, cfg)
	if err != nil {
		return siteAccessFailure(c, lang, err, siteAccessPhaseBeforeSend)
	}
	if cur.InsecureTransport {
		return c.JSON(http.StatusUnprocessableEntity, remoteSiteAccessFailure{
			Message: i18n.Translate(string(lang), "remote_access_insecure_remote"), Status: siteAccessCheckUnknown, Blocked: true})
	}
	sendCtx, cancel := context.WithTimeout(ctx, remoteSiteAccessTimeout)
	defer cancel()
	var out SyncSiteAccessResponse
	err = callRemote(sendCtx, cfg, SyncRequest{Action: SyncActionSetSiteAccess, SiteAccess: &SyncSiteAccessUpdate{
		Enabled: enabled, User: user, Password: pass, ExpectedRevision: &revision,
	}}, &out)
	if err == nil && out.Action != SyncActionSetSiteAccess {
		err = &remoteError{Status: http.StatusOK, Message: i18n.Translate(string(lang), "remote_access_unreadable")}
	}
	var re *remoteError
	if errors.As(err, &re) && re.Code == siteAccessCodeConflict {
		// ほかの PC が先に変更していた。最新の状態を読み直して返し、内容を確かめてからもう一度保存してもらう。
		fail := remoteSiteAccessFailure{Message: i18n.Translate(string(lang), "remote_access_conflict"),
			Status: siteAccessCheckUnknown, RemoteStatus: re.Status, Conflict: true}
		if st, ferr := fetchRemoteSiteAccess(ctx, cfg); ferr == nil {
			view := s.siteAccessView(ctx, lang, cfg, st)
			fail.State = &view
		}
		return c.JSON(http.StatusConflict, fail)
	}
	if err != nil {
		return siteAccessFailure(c, lang, err, siteAccessPhaseSent)
	}
	view := s.siteAccessView(ctx, lang, cfg, out)
	return c.JSON(http.StatusOK, map[string]any{
		"message": i18n.Translate(string(lang), "remote_access_saved_"+view.Status),
		"changed": out.Changed,
		"state":   view,
	})
}

// fetchRemoteSiteAccess は公開サーバーの get_site_access を呼ぶ。応答が get_site_access のものでなければ
// （別のサーバーの応答など）、読み取れない応答としてエラーにする。
func fetchRemoteSiteAccess(ctx context.Context, cfg remoteConfig) (SyncSiteAccessResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteSiteAccessTimeout)
	defer cancel()
	var out SyncSiteAccessResponse
	if err := callRemote(ctx, cfg, SyncRequest{Action: SyncActionGetSiteAccess}, &out); err != nil {
		return out, err
	}
	if out.Action != SyncActionGetSiteAccess {
		return out, &remoteError{Status: http.StatusOK, Code: "unreadable"}
	}
	return out, nil
}

// siteAccessPhase は、閲覧制限の確認・変更が失敗した段階。
type siteAccessPhase int

const (
	// siteAccessPhaseRead は状態の確認（get_site_access）の失敗。
	siteAccessPhaseRead siteAccessPhase = iota
	// siteAccessPhaseBeforeSend は、変更を送る前の確認（get_site_access）の失敗。変更は送っていない。
	siteAccessPhaseBeforeSend
	// siteAccessPhaseSent は、変更（set_site_access）を送った後の失敗。応答が無い・読めないときは、反映されたかどうか
	// 分からない。
	siteAccessPhaseSent
)

// siteAccessFailure は閲覧制限の確認・変更の失敗を、状態が分からないことを表す応答にする。公開サーバーが入力の誤りを
// 示したとき、この接続では変更を受け付けないと断ったときは 422、それ以外の公開サーバーとの通信の失敗は 502 で返す。
// phase が変更を送った後なら、応答が無い・読めないときに反映されたかどうか分からないと伝え、送る前なら変更を送って
// いないと伝える。
func siteAccessFailure(c echo.Context, lang i18n.Lang, err error, phase siteAccessPhase) error {
	saving := phase == siteAccessPhaseSent
	var re *remoteError
	if !errors.As(err, &re) {
		return err
	}
	if re.cause != nil {
		c.Logger().Warnf("%v", re)
	}
	l := string(lang)
	fail := remoteSiteAccessFailure{Message: re.Message, Status: siteAccessCheckUnknown, RemoteStatus: re.Status}
	switch {
	case strings.HasPrefix(re.Code, "basic_auth_err_"):
		fail.Message = i18n.Translate(l, re.Code)
		return c.JSON(http.StatusUnprocessableEntity, fail)
	case re.Code == siteAccessCodeInsecure:
		// 公開サーバーが、この接続を TLS で直接受けていないため断った。何も変わっておらず、入力を直しても保存できない。
		fail.Message = i18n.Translate(l, "remote_access_insecure_remote")
		fail.Blocked = true
		return c.JSON(http.StatusUnprocessableEntity, fail)
	case re.Status == http.StatusBadRequest && strings.HasPrefix(re.Message, "未対応の action"):
		fail.Message = i18n.Translate(l, "remote_access_unsupported")
	case re.Code == "unreadable" || (re.Status == http.StatusOK && saving):
		fail.Message = i18n.Translate(l, "remote_access_unreadable")
		fail.OutcomeUnknown = saving
	case saving && (re.Status == 0 || re.Status >= http.StatusInternalServerError):
		fail.OutcomeUnknown = true
	}
	switch {
	case fail.OutcomeUnknown:
		fail.Message = strings.TrimSpace(fail.Message + " " + i18n.Translate(l, "remote_access_outcome_unknown"))
	case phase == siteAccessPhaseBeforeSend:
		fail.Message = strings.TrimSpace(fail.Message + " " + i18n.Translate(l, "remote_access_not_sent"))
	}
	return c.JSON(http.StatusBadGateway, fail)
}

// remoteEndpointSecure は、公開サーバーへの接続が TLS で暗号化されるか（https か）を返す。閲覧用のパスワードを送る前に
// 確かめる。接続先の保存（normalizeRemoteURL）は試験用にこの PC 内の http://localhost も許すが、閲覧用のパスワードは
// そこにも送らない（間にプロキシやトンネルがあれば平文で流れ、公開サーバーも平文の接続では受け付けない）。
func remoteEndpointSecure(cfg remoteConfig) bool {
	u, err := url.Parse(cfg.endpoint())
	return err == nil && u.Host != "" && strings.EqualFold(u.Scheme, "https")
}

// siteAccessSaveBlocked は、この PC から公開サーバーの閲覧制限を変更できない理由（表示言語）を返す。変更できるなら空。
// st は公開サーバーの get_site_access・set_site_access の応答。
func siteAccessSaveBlocked(lang i18n.Lang, cfg remoteConfig, st SyncSiteAccessResponse) string {
	switch {
	case !remoteEndpointSecure(cfg):
		return i18n.Translate(string(lang), "remote_access_insecure")
	case st.InsecureTransport:
		return i18n.Translate(string(lang), "remote_access_insecure_remote")
	}
	return ""
}

// siteAccessView は公開サーバーの状態 st を外から確かめ（probeSiteAccess）、画面に出す状態を作る。この PC から変更
// できるか（CanSave）と、できない理由（SaveBlocked）も入れる。
func (s *Server) siteAccessView(ctx context.Context, lang i18n.Lang, cfg remoteConfig, st SyncSiteAccessResponse) remoteSiteAccessView {
	v := buildSiteAccessView(lang, st, s.probeSiteAccess(ctx, lang, siteAccessTargets(cfg, st)))
	v.SaveBlocked = siteAccessSaveBlocked(lang, cfg, st)
	v.CanSave = v.SaveBlocked == ""
	return v
}

const (
	// maxSiteAccessCheckPaths は、公開サーバーが挙げたパスのうち、トップページに加えて開いてみる数の上限。
	maxSiteAccessCheckPaths = 3
	// maxSiteAccessCheckPathLen は、開いてみるパス1つの長さの上限。
	maxSiteAccessCheckPathLen = 512
)

// siteAccessTarget は外から開いて確かめる URL 1件。
type siteAccessTarget struct {
	URL string
	// Public は公開サーバーが返したサイトの URL（接続先として設定した公開サーバー自身ではない）か。公開された
	// アドレスにだけ接続する。
	Public bool
	// Invalid は URL の形式が正しくないため、開かずに「確かめていない」とするか。
	Invalid bool
}

// siteAccessTargets は外から開いて確かめる URL を返す。接続先として設定した公開サーバー（cfg.URL）と、公開サーバーに
// 設定されたサイトの URL（st.SiteURL。CDN を通した URL など）が別なら両方について、トップページと公開サーバーが
// 挙げたパス（st.CheckPaths）を並べる。サイトの URL は公開サーバーの応答のため、設定した公開サーバーと同じオリジン
// でなければ Public にする（http も開いてみる。資格情報は送らない）。形式が正しくなければ、確かめていない URL として残す。
func siteAccessTargets(cfg remoteConfig, st SyncSiteAccessResponse) []siteAccessTarget {
	paths := siteAccessCheckPathsFrom(st.CheckPaths)
	origin := strings.TrimRight(cfg.URL, "/")
	targets := make([]siteAccessTarget, 0, 2*len(paths))
	for _, p := range paths {
		targets = append(targets, siteAccessTarget{URL: origin + p})
	}
	raw := strings.TrimSpace(st.SiteURL)
	if raw == "" {
		return targets
	}
	u, err := url.Parse(raw)
	scheme := ""
	if err == nil {
		scheme = strings.ToLower(u.Scheme)
	}
	if err != nil || (scheme != "http" && scheme != "https") || u.Hostname() == "" || u.User != nil {
		shown := raw
		if err == nil && u.User != nil {
			u.User = nil // 画面に資格情報を出さない
			shown = u.String()
		}
		return append(targets, siteAccessTarget{URL: shown, Invalid: true})
	}
	site := scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	if strings.EqualFold(site, origin) {
		return targets
	}
	public := !sameOrigin(cfg.URL, u)
	for _, p := range paths {
		targets = append(targets, siteAccessTarget{URL: site + p, Public: public})
	}
	return targets
}

// siteAccessCheckPathsFrom は、トップページ（"/"）と、公開サーバーが挙げたパスのうち正しい形のもの（最大
// maxSiteAccessCheckPaths 件、重複なし）を返す。
func siteAccessCheckPathsFrom(raw []string) []string {
	out := []string{"/"}
	for _, p := range raw {
		if len(out) > maxSiteAccessCheckPaths {
			break
		}
		if validSiteAccessCheckPath(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// validSiteAccessCheckPath は p が、サイトの URL に続けられる "/" から始まるパスかを返す。ほかのホストを指す形
// （"//host"、スキーム付き）、クエリ、"..." などで別の場所を指す形、空白・制御文字・バックスラッシュは受け付けない。
func validSiteAccessCheckPath(p string) bool {
	if p == "" || len(p) > maxSiteAccessCheckPathLen || p[0] != '/' || strings.HasPrefix(p, "//") ||
		strings.ContainsAny(p, "\\?#") || strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil && path.Clean(u.Path) == u.Path
}

// sameOrigin は u が、接続先として設定した公開サーバーの URL（configured）と同じスキーム・ホスト・ポートかを返す。
func sameOrigin(configured string, u *url.URL) bool {
	c, err := url.Parse(configured)
	if err != nil {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(c.Scheme, u.Scheme) && strings.EqualFold(c.Hostname(), u.Hostname()) && port(c) == port(u)
}

// probeSiteAccess は targets をパスワードなしで同時に開き、パスワードを求められるかを確かめる。Basic 認証の
// 401 ならパスワードを求めている（protected）、2xx ならパスワードなしで表示された（open）、それ以外（接続できない・
// 転送・公開されていないアドレス・その他のエラー）は確かめられなかった（unknown）とする。
func (s *Server) probeSiteAccess(ctx context.Context, lang i18n.Lang, targets []siteAccessTarget) []remoteSiteAccessCheck {
	checks := make([]remoteSiteAccessCheck, len(targets))
	public := s.siteAccessPublicClient()
	var wg sync.WaitGroup
	for i, target := range targets {
		client := siteAccessProbeClient
		if target.Public {
			client = public
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			checks[i] = probeSiteAccessURL(ctx, lang, client, target)
		}()
	}
	wg.Wait()
	return checks
}

func probeSiteAccessURL(ctx context.Context, lang i18n.Lang, client *http.Client, target siteAccessTarget) remoteSiteAccessCheck {
	check := remoteSiteAccessCheck{URL: target.URL, Result: siteAccessCheckUnknown}
	l := string(lang)
	if target.Invalid {
		check.Label = i18n.Translate(l, "remote_access_check_invalid", target.URL)
		return check
	}
	ctx, cancel := context.WithTimeout(ctx, siteAccessProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "sorus-site-access-check")
		var res *http.Response
		if res, err = client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxSiteAccessProbeBody))
			res.Body.Close()
			check.Status = res.StatusCode
			switch {
			case res.StatusCode == http.StatusUnauthorized && isBasicChallenge(res.Header):
				check.Result = siteAccessCheckProtected
			case res.StatusCode >= 200 && res.StatusCode < 300:
				check.Result = siteAccessCheckOpen
			}
		}
	}
	switch {
	case check.Result == siteAccessCheckProtected:
		check.Label = i18n.Translate(l, "remote_access_check_protected", target.URL)
	case check.Result == siteAccessCheckOpen:
		check.Label = i18n.Translate(l, "remote_access_check_open", target.URL)
	case errors.Is(err, errNonPublicTarget):
		check.Label = i18n.Translate(l, "remote_access_check_private", target.URL)
	case check.Status >= 300 && check.Status < 400:
		check.Label = i18n.Translate(l, "remote_access_check_redirect", target.URL, check.Status)
	case check.Status != 0:
		check.Label = i18n.Translate(l, "remote_access_check_unknown_status", target.URL, check.Status)
	default:
		check.Label = i18n.Translate(l, "remote_access_check_unknown", target.URL)
	}
	return check
}

// buildSiteAccessView は公開サーバーの設定 st と外から確かめた結果 checks から、画面に出す状態を決める。
// 公開サーバーで有効でも、確かめたすべての URL でパスワードを求められなければ checked にしない。checked でも、
// 確かめていない URL（CDN に残るほかのキャッシュなど）があるため、成功の色（ok）にはせず、範囲を限って示す（info）。
func buildSiteAccessView(lang i18n.Lang, st SyncSiteAccessResponse, checks []remoteSiteAccessCheck) remoteSiteAccessView {
	l := string(lang)
	v := remoteSiteAccessView{
		Enabled: st.Enabled, Active: st.Active, User: st.User, PasswordSet: st.PasswordSet, Revision: st.Revision,
		Checks:       checks,
		CheckedLabel: i18n.Translate(l, "remote_access_checked_at", clockNow().In(jst).Format("2006/01/02 15:04:05")),
	}
	if v.Checks == nil {
		v.Checks = []remoteSiteAccessCheck{}
	}
	protected, open := 0, 0
	for _, ch := range checks {
		switch ch.Result {
		case siteAccessCheckProtected:
			protected++
		case siteAccessCheckOpen:
			open++
		}
	}
	switch {
	case !st.Enabled:
		v.Status, v.Tone, v.StateLabel = siteAccessStatusOff, "neutral", i18n.Translate(l, "remote_access_state_off")
	case !st.Active:
		v.Status, v.Tone, v.StateLabel = siteAccessStatusInactive, "error", i18n.Translate(l, "remote_access_state_inactive")
	case open > 0:
		v.Status, v.Tone, v.StateLabel = siteAccessStatusExposed, "error", i18n.Translate(l, "remote_access_state_on")
	case len(checks) > 0 && protected == len(checks):
		v.Status, v.Tone, v.StateLabel = siteAccessStatusChecked, "info", i18n.Translate(l, "remote_access_state_on")
	default:
		v.Status, v.Tone, v.StateLabel = siteAccessStatusUnverified, "warn", i18n.Translate(l, "remote_access_state_on")
	}
	v.Summary = i18n.Translate(l, "remote_access_summary_"+v.Status)
	if st.PasswordSet {
		v.PasswordLabel = i18n.Translate(l, "remote_access_password_set")
	} else {
		v.PasswordLabel = i18n.Translate(l, "remote_access_password_unset")
	}
	return v
}
