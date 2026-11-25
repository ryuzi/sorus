package auth

const (
	// SessionCookieName は1つ目のサイトとサーバーモードで、セッショントークンを保持する Cookie の名前。
	SessionCookieName = "cms_session"
	// CSRFCookieName は1つ目のサイトとサーバーモードで、CSRF トークンを保持する Cookie の名前。
	CSRFCookieName = "cms_csrf"
	// LangCookieName は1つ目のサイトとサーバーモードで、管理画面の表示言語を保持する Cookie の名前。
	LangCookieName = "sorus_lang"
)

// CookieNames は1つのサイトが使う Cookie の名前。
type CookieNames struct {
	// Session はログイン（セッショントークン）の Cookie。
	Session string
	// CSRF は CSRF トークンの Cookie。
	CSRF string
	// Lang は管理画面の表示言語の Cookie。
	Lang string
}

// CookieNamesFor はサイトの鍵 siteKey（server.Config.SiteKey）から Cookie の名前を決める。Cookie の名前を決めるのは
// ここだけで、読む・書く・消すところはすべてここで決めた名前を使う。
//
// ブラウザは Cookie をポートで分けないため、同じ PC で同時に動くサイト（デスクトップモード）の名前が同じだと、
// 片方のログインがもう片方のログインを上書きして消す。siteKey が空（1つ目のサイトとサーバーモード）なら今までの
// 名前のまま（ログイン中の人をログアウトさせない）、空でなければそれぞれの末尾に _<siteKey> を付ける。
// siteKey は英小文字と数字だけにすること（Cookie の名前に使える文字）。
func CookieNamesFor(siteKey string) CookieNames {
	n := CookieNames{Session: SessionCookieName, CSRF: CSRFCookieName, Lang: LangCookieName}
	if siteKey != "" {
		suffix := "_" + siteKey
		n.Session += suffix
		n.CSRF += suffix
		n.Lang += suffix
	}
	return n
}
