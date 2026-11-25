package server

// 公開方式の排他（公開サーバー／静的サイト）。
//
// 公開サーバーの接続（remote_*）と静的サイトの公開先（publish_*）は別々に保存され、両方が残ることがある。両方を設定した
// まま記事を公開すると、静的サイトへの自動反映と公開サーバーへの送信が両方走ってしまう（二重公開）。そこで「いま使う方式」を
// settings の publish_active_method に1つだけ持ち、記事の公開・予約投稿・ダッシュボードの「サイトに反映」はその方式にだけ
// 送る。もう一方の設定は消さずに「待機中（自動反映停止中）」として残し、1クリックで切り替えられるようにする。

import (
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

const (
	// settingPublishActiveMethod は、いま使う公開方式（publishMethodRemote・publishMethodStatic）を保存する settings のキー。
	// 未保存・空なら、今までの画面と同じ規則で決める（activePublishMethod）。
	settingPublishActiveMethod = "publish_active_method"
	// publishMethodRemote は公開サーバー（sorus の公開専用サーバー）へ記事を送る方式。
	publishMethodRemote = "remote"
	// publishMethodStatic は静的サイト（HTML を書き出して外部の公開先へ送る）方式。
	publishMethodStatic = "static"
)

func init() {
	flashMessages["publish_method_switched_remote"] = "公開方式を「公開サーバー」に切り替えました。静的サイトへの自動反映は止まります（設定は残っています）"
	flashMessages["publish_method_switched_static"] = "公開方式を「静的サイト」に切り替えました。公開サーバーへは記事を送りません（接続は残っています）"
	flashMessages["publish_method_cleared_remote"] = "公開サーバーとの接続を解除しました"
	flashMessages["publish_method_cleared_static"] = "静的サイトの公開先の設定を削除しました"
	flashMessages["publish_method_unavailable"] = "その公開方式はまだ設定されていません。先に設定してください"
	flashMessages["publish_static_standby"] = "静的サイトは待機中のため反映していません。使う方式を「静的サイト」に切り替えてから反映してください"
	for _, key := range []string{"publish_method_switched_remote", "publish_method_switched_static", "publish_method_cleared_remote",
		"publish_method_cleared_static", "publish_method_unavailable", "publish_static_standby"} {
		flashKeys[key] = key
	}
}

// storedPublishMethod は保存した公開方式（publishMethodRemote・publishMethodStatic）を返す。未保存・不明な値なら空。
func (s *Server) storedPublishMethod() string {
	switch v := s.getSetting(settingPublishActiveMethod, ""); v {
	case publishMethodRemote, publishMethodStatic:
		return v
	}
	return ""
}

// resolvePublishMethod は保存した方式 stored と、公開サーバーに接続しているか・静的サイトの公開先が決まっているかから、
// いま使う方式を決める。保存した方式が設定済みならそれを使い、そうでなければ（未保存・その方式を解除した）今までの
// 画面と同じく、公開サーバーに接続していれば公開サーバー、そうでなく静的サイトが決まっていれば静的サイトにする。
// どちらも設定していなければ空。
func resolvePublishMethod(stored string, remoteConnected, staticConfigured bool) string {
	switch {
	case stored == publishMethodRemote && remoteConnected:
		return publishMethodRemote
	case stored == publishMethodStatic && staticConfigured:
		return publishMethodStatic
	case remoteConnected:
		return publishMethodRemote
	case staticConfigured:
		return publishMethodStatic
	}
	return ""
}

// activePublishMethod は、いま使う公開方式（publishMethodRemote・publishMethodStatic、どちらも未設定なら空）を返す。
// 公開専用モードのサーバー（公開サーバー自身）は、どちらにも送らないため空。
func (s *Server) activePublishMethod() string {
	if s.cfg.PublicOnly {
		return ""
	}
	return resolvePublishMethod(s.storedPublishMethod(), s.loadRemoteConfig().connected(), s.loadPublishConfig().configured())
}

// staticPublishActive は静的サイトへの反映（記事公開時の自動反映・予約投稿・「サイトに反映」）をしてよいか。
func (s *Server) staticPublishActive() bool { return s.activePublishMethod() == publishMethodStatic }

// remotePublishActive は記事の公開を公開サーバーへ送ってよいか（いまの方式が公開サーバー）。
func (s *Server) remotePublishActive() bool { return s.activePublishMethod() == publishMethodRemote }

// canRemotePublish は user が記事を公開したとき、公開サーバーにも公開するか（直接公開できる招待で、いまの方式が公開サーバー）。
func (s *Server) canRemotePublish(user *auth.User) bool {
	return s.remoteSyncViewFor(user).CanPublish && s.remotePublishActive()
}

// publishMethodView は「公開先」の2つの画面（/admin/publish・/admin/settings/remote）の先頭に出す、方式の切り替えの
// タブと、両方を設定済みのときの1行の状態（partials/publish_method_banner.html）のデータ。
type publishMethodView struct {
	// Active はいま使う方式（publishMethodRemote・publishMethodStatic・空）。Tab は表示中の画面の方式。
	Active string
	Tab    string
	// RemoteConfigured・StaticConfigured はそれぞれの方式を設定済みか。
	RemoteConfigured bool
	StaticConfigured bool
}

// Both は両方の方式を設定済みか（待機中の方式があるか）。
func (v publishMethodView) Both() bool { return v.RemoteConfigured && v.StaticConfigured }

// Standby は両方を設定済みのとき、待機中の方式（いま使っていない方）を返す。両方を設定していなければ空。
func (v publishMethodView) Standby() string {
	switch {
	case !v.Both():
		return ""
	case v.Active == publishMethodRemote:
		return publishMethodStatic
	default:
		return publishMethodRemote
	}
}

// TabActive は表示中の画面の方式が、いま使う方式か。
func (v publishMethodView) TabActive() bool { return v.Tab == v.Active }

// TabConfigured は表示中の画面の方式を設定済みか（接続の解除・設定の削除を出すか）。
func (v publishMethodView) TabConfigured() bool {
	if v.Tab == publishMethodRemote {
		return v.RemoteConfigured
	}
	return v.StaticConfigured
}

// publishMethodViewFor は tab の画面に出す方式のタブのデータを作る。
func (s *Server) publishMethodViewFor(tab string) *publishMethodView {
	v := &publishMethodView{Tab: tab, RemoteConfigured: s.loadRemoteConfig().connected(), StaticConfigured: s.loadPublishConfig().configured()}
	v.Active = resolvePublishMethod(s.storedPublishMethod(), v.RemoteConfigured, v.StaticConfigured)
	return v
}

// layoutPublishMethod は「公開先」の画面（管理者だけが開ける）に方式のタブのデータを入れる。
func (s *Server) layoutPublishMethod(p *page) {
	if p.User == nil || !p.User.HasRole(auth.RoleAdmin) || s.cfg.PublicOnly {
		return
	}
	switch {
	case p.AtPath("/admin/publish"):
		p.PublishMethod = s.publishMethodViewFor(publishMethodStatic)
	case p.AtPath("/admin/settings/remote"):
		p.PublishMethod = s.publishMethodViewFor(publishMethodRemote)
	}
}

// publishMethodReturnURL は方式の切り替え・解除のあとに戻る画面（戻り先の方式 tab の画面）。
func publishMethodReturnURL(tab, flash string) string {
	q := url.Values{"flash": {flash}}
	if tab == publishMethodRemote {
		return "/admin/settings/remote?" + q.Encode()
	}
	q.Set("view", "static")
	return "/admin/publish?" + q.Encode()
}

// handlePublishMethodSwitch は、いま使う公開方式を切り替える（POST /admin/publish/method、method=remote|static）。
// 切り替える先の方式が設定済みでなければ何も変えない。もう一方の設定は消さない（待機中として残す）。
func (s *Server) handlePublishMethodSwitch(c echo.Context) error {
	method := c.FormValue("method")
	tab := c.FormValue("return")
	if tab != publishMethodRemote {
		tab = publishMethodStatic
	}
	var ok bool
	switch method {
	case publishMethodRemote:
		ok = s.loadRemoteConfig().connected()
	case publishMethodStatic:
		ok = s.loadPublishConfig().configured()
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "method には remote か static を指定してください")
	}
	if !ok {
		return c.Redirect(http.StatusSeeOther, publishMethodReturnURL(tab, "publish_method_unavailable"))
	}
	if err := upsertSetting(s.db, settingPublishActiveMethod, method); err != nil {
		return err
	}
	if method == publishMethodRemote {
		// 公開サーバーへ切り替えたら、まだ始まっていない静的サイトへの自動反映も取りやめる（切り替えた後に送らない）。
		s.cancelScheduledReflect()
	}
	return c.Redirect(http.StatusSeeOther, publishMethodReturnURL(tab, "publish_method_switched_"+method))
}

// handlePublishMethodClear は公開方式の設定を解除する（POST /admin/publish/method/clear、method=remote|static）。
// 公開サーバーは接続先とトークンを消し（未反映の記録も消える）、静的サイトは公開先の種類と接続の設定（パスワード・
// トークンを含む）を消す。いま使う方式を解除したら、残っている方式（無ければ未設定）に切り替える。
func (s *Server) handlePublishMethodClear(c echo.Context) error {
	method := c.FormValue("method")
	switch method {
	case publishMethodRemote:
		if err := s.saveRemoteConfig(remoteConfig{}); err != nil {
			return err
		}
	case publishMethodStatic:
		s.cancelScheduledReflect()
		if err := s.clearPublishConfig(); err != nil {
			return err
		}
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "method には remote か static を指定してください")
	}
	// 保存した方式が解除した方式なら、残っている方式を保存し直す（どちらも無ければ消す）。
	if s.storedPublishMethod() == method {
		next := resolvePublishMethod("", s.loadRemoteConfig().connected(), s.loadPublishConfig().configured())
		var err error
		if next == "" {
			_, err = s.db.Exec(`DELETE FROM settings WHERE key = ?`, settingPublishActiveMethod)
		} else {
			err = upsertSetting(s.db, settingPublishActiveMethod, next)
		}
		if err != nil {
			return err
		}
	}
	tab := publishMethodStatic
	if method == publishMethodStatic && s.loadRemoteConfig().connected() {
		tab = publishMethodRemote
	}
	return c.Redirect(http.StatusSeeOther, publishMethodReturnURL(tab, "publish_method_cleared_"+method))
}

// clearPublishConfig は静的サイトの公開先の種類と、公開先ごとの接続の設定（パスワード・トークン・鍵を含む）を消す。
// 公開サイトの URL・自動反映の設定・反映の記録（Manifest）は残す（同じ公開先を設定し直したときに使う）。
func (s *Server) clearPublishConfig() error {
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range []string{publishSettingKind, publishSettingServer, publishSettingCloudflare, publishSettingGitHub,
		publishSettingNetlify, publishSettingS3, publishSettingFolder} {
		if _, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// cancelScheduledReflect は、待っているあいだの静的サイトへの自動反映（scheduleReflect）を取りやめる。始まった反映は止めない。
func (s *Server) cancelScheduledReflect() {
	st := s.publishState()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
}
