package server

import (
	"cmp"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/mcp"
)

// OAuth 2.1 のエンドポイントの回数の制限。登録（/oauth/register）・トークン（/oauth/token）・同意画面
// （/oauth/authorize）は認証の前に誰でも呼べ、公開専用モードの同意画面は確認コードを総当たりで試されうるため、
// 送り元ごと（oauthClientKey）と全体の回数を、一定の時間の区切り（固定ウィンドウ）ごとに数えて制限する。
// 数はメモリ上だけに持ち、再起動で消える（制限は総当たりの速度を抑えるためのもので、確認コード自体の有効期限も短い）。
// 管理画面のログイン（POST /admin/login）のパスワードの誤りも、同じ仕組みで送り元ごと・ユーザー名ごとに数える。

const (
	// oauthRateWindow は回数を数える時間の区切り。
	oauthRateWindow = 10 * time.Minute
	// oauthRegisterPerClient・oauthRegisterGlobal はクライアント登録の、送り元ごと・全体の上限（oauthRateWindow あたり）。
	oauthRegisterPerClient = 20
	oauthRegisterGlobal    = 300
	// oauthTokenPerClient・oauthTokenGlobal はトークンの発行・更新の、送り元ごと・全体の上限。AI アプリはアクセス
	// トークンを1時間ごとに更新するため、全体の上限は多めにする。
	oauthTokenPerClient = 120
	oauthTokenGlobal    = 3000
	// oauthAuthorizePerClient・oauthAuthorizeGlobal は同意画面（GET・POST）の、送り元ごと・全体の上限。
	oauthAuthorizePerClient = 60
	oauthAuthorizeGlobal    = 600
	// oauthLoginFailPerClient・oauthLoginFailGlobal は確認コードの入力の誤りの、送り元ごと・全体の上限。超えたら
	// 区切りが変わるまで、正しいコードでも受け付けない。
	oauthLoginFailPerClient = 10
	oauthLoginFailGlobal    = 100
	// adminLoginFailPerClient・adminLoginFailPerUser は管理画面のログインのパスワードの誤りの、送り元ごと・
	// ユーザー名ごとの上限（oauthRateWindow あたり）。超えたら区切りが変わるまで、正しいパスワードでも受け付けない
	// （パスワードの照合もしないため、bcrypt の計算で CPU を使い尽くされない）。送り元ごとの上限は、学校や会社で
	// 同じアドレスを共有する人が打ち間違えても締め出されないよう、ユーザー名ごとより多めにする。
	adminLoginFailPerClient = 30
	adminLoginFailPerUser   = 10
	// maxRateLimitKeys は送り元ごとの記録の数の上限。超えたら区切りの過ぎた記録を消し、それでも多ければ数の少ない
	// （同じなら古い）記録から消す（大量の送り元からの要求でメモリを使い尽くさないようにする。上限に達した記録ほど
	// 残し、新しい送り元を大量に作って自分の記録を押し出すことはできない。全体の上限は別に数えるため、制限は効き続ける）。
	maxRateLimitKeys = 10000
	// oauthRateGlobalKey は全体の回数を数えるキー。
	oauthRateGlobalKey = "*"
)

// rateLimiter は固定ウィンドウでキーごとの回数を数える。並行に使用してよい。
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]*rateEntry
	now     func() time.Time
	// maxKeys は記録の数の上限（既定は maxRateLimitKeys。試験で小さくする）。
	maxKeys int
}

type rateEntry struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, entries: map[string]*rateEntry{}, now: time.Now, maxKeys: maxRateLimitKeys}
}

// setLimit は上限を変える（動いているサーバーの上限を試験で小さくするため。l.mu の中で変える）。
func (l *rateLimiter) setLimit(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = n
}

// setClock は時刻の取得元を変える（区切りが変わった後の動きを試験で確かめるため。l.mu の中で変える）。
func (l *rateLimiter) setClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// entry は key の今の区切りの記録を返す（無ければ作る）。l.mu を持って呼ぶ。
func (l *rateLimiter) entry(key string, now time.Time) *rateEntry {
	e := l.entries[key]
	if e != nil && now.Sub(e.start) < l.window {
		return e
	}
	if e == nil && len(l.entries) >= l.maxKeys {
		l.evict(now)
	}
	e = &rateEntry{start: now}
	l.entries[key] = e
	return e
}

// evict は記録の数が上限に達したときに、区切りの過ぎた記録を消す。それでも上限に達していれば、数の少ない（同じなら
// 区切りの始まりが古い）記録から、上限の1割を空けるまで消す。全部を消すと、上限に達して止めていた送り元の記録まで
// 消えて制限が外れるため、数の多い記録を残す。1割ずつ空けるのは、新しい送り元が続いても毎回並べ替えないため。
// l.mu を持って呼ぶ。
func (l *rateLimiter) evict(now time.Time) {
	for k, old := range l.entries {
		if now.Sub(old.start) >= l.window {
			delete(l.entries, k)
		}
	}
	if len(l.entries) < l.maxKeys {
		return
	}
	type keyed struct {
		key string
		e   *rateEntry
	}
	all := make([]keyed, 0, len(l.entries))
	for k, e := range l.entries {
		all = append(all, keyed{k, e})
	}
	slices.SortFunc(all, func(a, b keyed) int {
		return cmp.Or(cmp.Compare(a.e.count, b.e.count), a.e.start.Compare(b.e.start))
	})
	n := len(all) - l.maxKeys + max(l.maxKeys/10, 1)
	for _, x := range all[:min(n, len(all))] {
		delete(l.entries, x.key)
	}
}

// retryAfter は e の区切りが終わるまでの時間（1秒以上）。
func (l *rateLimiter) retryAfter(e *rateEntry, now time.Time) time.Duration {
	d := e.start.Add(l.window).Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return d
}

// hit は key の回数を1つ増やし、上限以内なら ok = true を返す。上限を超えたら、区切りが終わるまでの時間を返す。
func (l *rateLimiter) hit(key string) (retry time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entry(key, now)
	e.count++
	if e.count > l.limit {
		return l.retryAfter(e, now), false
	}
	return 0, true
}

// reset は key の記録を消す（正しいパスワードでログインできたユーザー名の誤りの数を数え直すため）。
func (l *rateLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// blocked は key がすでに上限に達しているかを、回数を増やさずに返す。
func (l *rateLimiter) blocked(key string) (retry time.Duration, blocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil || now.Sub(e.start) >= l.window || e.count < l.limit {
		return 0, false
	}
	return l.retryAfter(e, now), true
}

// oauthRateLimits は OAuth 2.1 のエンドポイントごとと、管理画面のログインの回数の制限。
type oauthRateLimits struct {
	register, registerGlobal   *rateLimiter
	token, tokenGlobal         *rateLimiter
	authorize, authorizeGlobal *rateLimiter
	loginFail, loginFailGlobal *rateLimiter
	// adminLoginClient は管理画面のログインの誤りの送り元ごと（adminLoginClientKey）の数。adminLoginUser は
	// 登録されているユーザー名ごとの数、adminLoginUnknown は登録されていないユーザー名ごとの数。登録されていない
	// ユーザー名を大量に試して、登録されているユーザー名の記録を押し出せないよう、別に数える（どちらも同じ上限で
	// 止めるため、止まり方からユーザー名の有無は分からない）。
	adminLoginClient, adminLoginUser, adminLoginUnknown *rateLimiter
	// proxies は送り元のアドレス（oauthClientKey）を決めるときに X-Forwarded-For を信じるリバースプロキシの一覧
	// （Server.proxies。Config.TrustedProxies を解析したもの）。
	proxies mcp.TrustedProxies
}

// newOAuthRateLimits は回数の制限を作る。proxies は Config.TrustedProxies を解析した一覧（Server.proxies）。
func newOAuthRateLimits(proxies mcp.TrustedProxies) *oauthRateLimits {
	return &oauthRateLimits{
		proxies: proxies,

		register:        newRateLimiter(oauthRegisterPerClient, oauthRateWindow),
		registerGlobal:  newRateLimiter(oauthRegisterGlobal, oauthRateWindow),
		token:           newRateLimiter(oauthTokenPerClient, oauthRateWindow),
		tokenGlobal:     newRateLimiter(oauthTokenGlobal, oauthRateWindow),
		authorize:       newRateLimiter(oauthAuthorizePerClient, oauthRateWindow),
		authorizeGlobal: newRateLimiter(oauthAuthorizeGlobal, oauthRateWindow),
		loginFail:       newRateLimiter(oauthLoginFailPerClient, oauthRateWindow),
		loginFailGlobal: newRateLimiter(oauthLoginFailGlobal, oauthRateWindow),

		adminLoginClient:  newRateLimiter(adminLoginFailPerClient, oauthRateWindow),
		adminLoginUser:    newRateLimiter(adminLoginFailPerUser, oauthRateWindow),
		adminLoginUnknown: newRateLimiter(adminLoginFailPerUser, oauthRateWindow),
	}
}

// hitBoth は送り元ごとと全体の両方を数え、どちらかが上限を超えたら ok = false を返す。
func (l *oauthRateLimits) hitBoth(perClient, global *rateLimiter, r *http.Request) (time.Duration, bool) {
	retry1, ok1 := perClient.hit(oauthClientKey(r, l.proxies))
	retry2, ok2 := global.hit(oauthRateGlobalKey)
	if ok1 && ok2 {
		return 0, true
	}
	return max(retry1, retry2), false
}

// allowRegister はクライアント登録を受け付けるかを返す。
func (l *oauthRateLimits) allowRegister(r *http.Request) (time.Duration, bool) {
	return l.hitBoth(l.register, l.registerGlobal, r)
}

// allowToken はトークンの発行・更新を受け付けるかを返す。
func (l *oauthRateLimits) allowToken(r *http.Request) (time.Duration, bool) {
	return l.hitBoth(l.token, l.tokenGlobal, r)
}

// allowAuthorize は同意画面への要求を受け付けるかを返す。
func (l *oauthRateLimits) allowAuthorize(r *http.Request) (time.Duration, bool) {
	return l.hitBoth(l.authorize, l.authorizeGlobal, r)
}

// loginBlocked は確認コードの入力の誤りが、送り元ごとか全体の上限に達しているかを返す。
func (l *oauthRateLimits) loginBlocked(r *http.Request) (time.Duration, bool) {
	retry1, b1 := l.loginFail.blocked(oauthClientKey(r, l.proxies))
	retry2, b2 := l.loginFailGlobal.blocked(oauthRateGlobalKey)
	return max(retry1, retry2), b1 || b2
}

// loginFailed は確認コードの入力の誤りを1回数える。
func (l *oauthRateLimits) loginFailed(r *http.Request) {
	l.loginFail.hit(oauthClientKey(r, l.proxies))
	l.loginFailGlobal.hit(oauthRateGlobalKey)
}

// adminLoginBlocked は管理画面のログインのパスワードの誤りが、送り元ごとかユーザー名ごとの上限に達しているかを返す。
// known はユーザー名が登録されているか。
func (l *oauthRateLimits) adminLoginBlocked(r *http.Request, username string, known bool) (time.Duration, bool) {
	retry1, b1 := l.adminLoginClient.blocked(adminLoginClientKey(r, l.proxies))
	retry2, b2 := l.adminLoginNames(known).blocked(adminLoginUserKey(username, known))
	return max(retry1, retry2), b1 || b2
}

// adminLoginFailed は管理画面のログインのパスワードの誤りを1回数える。known はユーザー名が登録されているか。
func (l *oauthRateLimits) adminLoginFailed(r *http.Request, username string, known bool) {
	l.adminLoginClient.hit(adminLoginClientKey(r, l.proxies))
	l.adminLoginNames(known).hit(adminLoginUserKey(username, known))
}

// adminLoginSucceeded は正しいパスワードでログインできた（登録されている）ユーザー名の、誤りの数を数え直す。
// 送り元ごとの数は消さない（自分のアカウントでログインし直すたびに数え直せると、同じ送り元から他のユーザー名を
// 試し続けられるため）。
func (l *oauthRateLimits) adminLoginSucceeded(username string) {
	l.adminLoginUser.reset(adminLoginUserKey(username, true))
}

// adminLoginNames はユーザー名ごとの数の記録（登録されているユーザー名か、されていないユーザー名か）を返す。
func (l *oauthRateLimits) adminLoginNames(known bool) *rateLimiter {
	if known {
		return l.adminLoginUser
	}
	return l.adminLoginUnknown
}

// adminLoginUserKey はユーザー名ごとの数を数えるキー。登録されているユーザー名はそのまま使う（数はユーザーの数までに
// 限られる）。登録されていないユーザー名のうち、ユーザー名に使えない形式（長すぎるものを含む）の入力は、記録を
// 大きくしないよう、すべて1つのキー（空）で数える（どのユーザーにも一致しないため、まとめて止めてよい）。
func adminLoginUserKey(username string, known bool) string {
	if !known && !auth.ValidUsername(username) {
		return ""
	}
	return username
}

// adminLoginClientKey は管理画面のログインで送り元ごとの数を数えるキー。oauthClientKey のアドレスのうち、IPv6 は
// /64 のネットワークにまとめる（1つの契約で /64 の中のアドレスを自由に使えるため、アドレスを変えて上限を逃れられない
// ようにする）。
func adminLoginClientKey(r *http.Request, proxies mcp.TrustedProxies) string {
	key := oauthClientKey(r, proxies)
	a, err := netip.ParseAddr(key)
	if err != nil || !a.Is6() {
		return key
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

// oauthClientKey は回数を数える送り元（IP アドレス）を返す。接続元が proxies（Config.TrustedProxies）のリバース
// プロキシなら、そのプロキシが付け足した X-Forwarded-For の値を使う（クライアントが自分で書いた前の方の値は偽れる
// ため使わない。mcp.TrustedProxies.ClientIP）。それ以外は、この PC 自身や私設のアドレスからの接続でも接続元の
// アドレス（TrustedProxies が未設定なら、X-Forwarded-For を書いて送り元を偽ることはできない）。環境変数
// TRUSTED_PROXIES は直接読まない（main.go が Config.TrustedProxies に渡す）。
func oauthClientKey(r *http.Request, proxies mcp.TrustedProxies) string {
	return proxies.ClientIP(r)
}

// setRetryAfter は Retry-After ヘッダー（秒）を付ける。
func setRetryAfter(c echo.Context, retry time.Duration) {
	secs := int((retry + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	c.Response().Header().Set("Retry-After", strconv.Itoa(secs))
}

// oauthTooManyRequests は登録・トークンのエンドポイントの 429 を OAuth のエラー（JSON）で返す。
func oauthTooManyRequests(c echo.Context, retry time.Duration) error {
	setRetryAfter(c, retry)
	return oauthError(c, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests; try again later")
}

// renderOAuthTooManyRequests は同意画面の 429 を、同意画面の見た目で返す。
func (s *Server) renderOAuthTooManyRequests(c echo.Context, retry time.Duration) error {
	setRetryAfter(c, retry)
	msg := "短い時間に何度も試されたため、受け付けを一時的に止めています。しばらく待ってから、AI アプリの画面から接続をやり直してください。"
	if language(c) == "en" {
		msg = "Too many attempts in a short time. Please wait a while, then start connecting again from the AI app."
	}
	return s.renderOAuthAuthorize(c, http.StatusTooManyRequests, oauthAuthorizeData{Error: msg, HideSiteName: s.oauthHidesSiteName()})
}
