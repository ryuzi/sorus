package server

import (
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/version"
	"sorus/static"
)

// pageNames は layout.html に埋め込んで描画するページテンプレート（templates/admin/<name>.html）。
var pageNames = []string{"login", "article_list", "article_edit", "article_view", "dashboard", "mcp_tokens", "settings_theme", "settings_structure", "users", "settings_general"}

// jst は管理画面で日時を表示するタイムゾーン。tzdata に依存しないよう固定オフセットで持つ。
var jst = time.FixedZone("JST", 9*60*60)

// displayTimeLayout は管理画面に日時を出す書式（JST）。テンプレートの formatTime・formatTimePtr と同じ。
const displayTimeLayout = "2006/01/02 15:04"

// flashMessages はリダイレクト先で表示するトースト通知。クエリの任意文字列を画面に出さないよう、
// ?flash=<key> のキーだけを受け付ける。
var flashMessages = map[string]string{
	"saved":                   "保存しました",
	"published":               "公開しました",
	"remote_published":        "記事を公開し、公開サーバーへ反映しました",
	"remote_publish_failed":   "ローカルで公開しましたが、公開サーバーへの反映は行えませんでした",
	"remote_unpublished":      "記事を非公開にし、公開サーバーからも取り下げました",
	"remote_unpublish_failed": "ローカルでは非公開にしましたが、公開サーバーからの取り下げは行えませんでした",
	"created":                 "記事を作成しました",
	"deleted":                 "ゴミ箱に移動しました",
	"unlocked":                "ロックを強制解除しました",
	"logged_out":              "ログアウトしました",
	"submitted":               "校閲を申請しました。編集長・管理者の確認をお待ちください",
	"rejected":                "差し戻しました",
	"approved":                "承認して公開しました",
	"scheduled":               "公開するよう予約しました",
	"restored":                "過去のバージョンに復元しました",
	"token_revoked":           "APIトークンを失効させました",
	"user_created":            "ユーザーを作成しました",
	"user_updated":            "ユーザー情報を更新しました",
	"user_password_updated":   "パスワードを変えました。その人はログアウトされ、AIアプリはトークンの発行し直しが必要です",
	"user_deleted":            "ユーザーを削除しました",
	"settings_saved":          "設定を保存しました",
	"backup_restored":         "バックアップから復元しました",
	"backup_relogin":          "バックアップから復元しました。復元したデータのアカウントでもう一度ログインしてください",
	"media_saved":             "画像の情報を保存しました",
	"media_deleted":           "画像を削除しました",
	"media_uploaded":          "画像をアップロードしました",
	"theme_saved":             "テーマを適用しました。CDN を使っている場合、公開サイトへの反映には最大1時間かかります",
	"structure_saved":         "サイト構成を保存しました。CDN を使っている場合、公開サイトへの反映には最大1時間かかります",
	"card_images_saved":       "公開カードのアイキャッチ画像表示を保存しました",
}

var templateFuncs = template.FuncMap{
	// asset は静的アセットの URL を、内容のハッシュ付き（/static/<name>?v=<hash>）で返す。
	"asset": static.URL,
	"formatTime": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(jst).Format(displayTimeLayout)
	},
	"formatTimePtr": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return ""
		}
		return t.In(jst).Format(displayTimeLayout)
	},
	"statusLabel": statusLabel,
	"statusLabelFor": func(st article.Status, lang string) string {
		key := "status_draft"
		switch st {
		case article.StatusPublished:
			key = "status_published"
		case article.StatusDeleted:
			key = "status_deleted"
		case article.StatusReview:
			key = "status_review"
		case article.StatusScheduled:
			key = "status_scheduled"
		}
		return i18n.Translate(lang, key)
	},
	"statusKey": func(st article.Status) string {
		switch st {
		case article.StatusPublished:
			return "status_published"
		case article.StatusDeleted:
			return "status_deleted"
		case article.StatusReview:
			return "status_review"
		case article.StatusScheduled:
			return "status_scheduled"
		}
		return "status_draft"
	},
	"roleKey": func(r auth.Role) string {
		switch r {
		case auth.RoleAdmin:
			return "role_admin"
		case auth.RoleEditor:
			return "role_editor"
		}
		return "role_writer"
	},
	"statusBadge": func(st article.Status) string {
		switch st {
		case article.StatusPublished:
			return "bg-neutral-100 text-neutral-800"
		case article.StatusDeleted:
			return "bg-neutral-200 text-neutral-600"
		case article.StatusReview:
			return "status-review"
		case article.StatusScheduled:
			return "status-scheduled"
		}
		return "bg-neutral-100 text-neutral-700"
	},
	"roleLabel": func(r auth.Role) string {
		switch r {
		case auth.RoleAdmin:
			return "管理者"
		case auth.RoleEditor:
			return "編集長"
		}
		return "執筆者"
	},
	"fileSize": formatFileSize,
	// contentPath は記事（/articles/<slug>）・ページ（/<slug>）の公開 URL のパス。
	"contentPath": contentPath,
	// formatCount は PV などの数を 1,234 の形で返す。
	"formatCount": formatCount,
	// avgPerDay は期間の合計を日数で割った1日平均（四捨五入）。
	"avgPerDay": func(total int64, days int) int64 {
		if days <= 0 {
			return 0
		}
		return (total + int64(days)/2) / int64(days)
	},
	// commentKind はワークフローが自動で残したコメント（【差し戻し】など）を色分けするクラス。
	"commentKind": func(content string) string {
		switch {
		case strings.HasPrefix(content, "【差し戻し】"):
			return "comment-reject"
		case strings.HasPrefix(content, "【承認】"):
			return "comment-approve"
		case strings.HasPrefix(content, "【校閲申請】"):
			return "comment-request"
		}
		return ""
	},
	// signed は文字数の増減を「+12」「-3」「±0」の形で返す。
	"signed": func(n int) string {
		switch {
		case n > 0:
			return fmt.Sprintf("+%d", n)
		case n < 0:
			return fmt.Sprintf("%d", n)
		}
		return "±0"
	},
	"userName": func(u *auth.User) string {
		if u == nil {
			return ""
		}
		if u.DisplayName != "" {
			return u.DisplayName
		}
		return u.Username
	},
}

func statusLabel(st article.Status) string {
	switch st {
	case article.StatusPublished:
		return "公開中"
	case article.StatusDeleted:
		return "ゴミ箱"
	case article.StatusReview:
		return "校閲待ち"
	case article.StatusScheduled:
		return "予約投稿"
	}
	return "下書き"
}

// renderer は echo.Renderer を実装する html/template のラッパー。
// ページは各レイアウトの "layout" として描画し、それ以外の名前はパーシャル（partials/ の define 名）として描画する。
// 公開サイトのページ・パーシャルは "public/<name>" の名前で描画する。
type renderer struct {
	partials       *template.Template
	pages          map[string]*template.Template
	publicPartials *template.Template
}

func newRenderer(fsys fs.FS) (*renderer, error) {
	partials, pages, err := parseTemplateSet(fsys, "admin", templateFuncs, pageNames)
	if err != nil {
		return nil, err
	}
	publicPartials, publicPages, err := parseTemplateSet(fsys, "public", publicTemplateFuncs, publicPageNames)
	if err != nil {
		return nil, err
	}
	for name, t := range publicPages {
		pages["public/"+name] = t
	}
	return &renderer{partials: partials, pages: pages, publicPartials: publicPartials}, nil
}

// parseTemplateSet は dir/layout.html と dir/partials/*.html を土台に、names のページをそれぞれ複製して読み込む。
// 戻り値の1つ目はパーシャル描画用の土台。
func parseTemplateSet(fsys fs.FS, dir string, funcs template.FuncMap, names []string) (*template.Template, map[string]*template.Template, error) {
	base, err := template.New(dir).Funcs(funcs).
		ParseFS(fsys, dir+"/layout.html", dir+"/partials/*.html")
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s layout: %w", dir, err)
	}
	pages := make(map[string]*template.Template, len(names))
	for _, name := range names {
		t, err := base.Clone()
		if err != nil {
			return nil, nil, fmt.Errorf("clone %s layout for %s: %w", dir, name, err)
		}
		if _, err := t.ParseFS(fsys, dir+"/"+name+".html"); err != nil {
			return nil, nil, fmt.Errorf("parse %s/%s: %w", dir, name, err)
		}
		pages[name] = t
	}
	// Clone は実行前にしか呼べないため、全ページを複製し終えてからパーシャル用に使う。
	return base, pages, nil
}

func (r *renderer) Render(w io.Writer, name string, data any, c echo.Context) error {
	if t, ok := r.pages[name]; ok {
		return t.ExecuteTemplate(w, "layout", data)
	}
	if partial, ok := strings.CutPrefix(name, "public/"); ok {
		return r.publicPartials.ExecuteTemplate(w, partial, data)
	}
	return r.partials.ExecuteTemplate(w, name, data)
}

// page はすべてのページテンプレートに渡すデータ。ページ固有の値は Data に入れる。
type page struct {
	SiteName  string
	Version   string
	Title     string
	User      *auth.User
	CSRFToken string
	Flash     string
	FlashKey  string
	// FlashError は Flash をエラー（赤）として表示するか。
	FlashError  bool
	Data        any
	Lang        string
	CurrentPath string
	// FaviconURL・FaviconType は <link rel="icon"> に出す Favicon の URL と MIME タイプ。
	FaviconURL  string
	FaviconType string
	// Sites はデスクトップモードでサイトが2つ以上あるとき、ヘッダーのサイト切り替えメニューに並べる開いているサイト。
	// サイトが1つだけのとき、またはログインしていないときは空。
	Sites []siteMenuItem
	// WindowScript はデスクトップモードで、この画面を開いている画面として数えてもらう接続のスクリプト（WindowPath）。
	// デスクトップモードでなければ空。
	WindowScript template.HTML
	// Desktop はデスクトップモードか。更新のお知らせ・自動バックアップの状態・「sorus を終了」を出す。
	Desktop bool
	// QuitPending はデスクトップモードで、まだサイトに反映していない予約の数（PendingScheduledReflects）。
	// 0 でなければ、「sorus を終了」は終わらせ方を選ぶ確認の画面（GET DesktopQuitPath）を開く。
	QuitPending int
	// RemoteDirty は公開サーバーへ未反映の設定の種類（"general"・"theme"・"structure"・"media" の順）。公開サーバーに
	// 接続していないとき・未反映が無いときは空。RemoteCanPushSettings は「公開サーバーへ反映する」を押せるか
	// （ローカルの管理者で、トークンが設定を反映できる）。RemotePushConfirm はそのボタンの確認文。
	// RemoteBannerStandby は未反映が無くてもバナーを隠して置く画面か（メディアライブラリ。タグを変えたら JS が出す）。
	// 共通バナー（layout.html）が使う（handlers_remote.go の layoutRemoteState）。
	RemoteDirty           []string
	RemoteCanPushSettings bool
	RemotePushConfirm     string
	RemoteBannerStandby   bool
	// MultilingualEnabled は公開コンテンツの多言語対応を有効にしているか（isMultilingualEnabled）。OFF なら管理画面の
	// 翻訳・英語版の作成・言語の絞り込みなどを出さない（テンプレートは $.MultilingualEnabled で参照する）。
	MultilingualEnabled bool
	// AIEnabled は AI 機能を使えるか（aiEnabled。マスタースイッチ）。false なら AI のボタン・メニュー・AI 設定・MCP 連携を
	// DOM ごと出さない（テンプレートは $.AIEnabled で参照する）。AIForcedOffByEnv は環境変数 SORUS_AI で強制的に OFF に
	// しているか（基本設定の「AI機能を使う」を変えられないことを示す）。
	AIEnabled        bool
	AIForcedOffByEnv bool
	// PublishMethod は「公開先」の画面（/admin/publish・/admin/settings/remote）の先頭に出す方式の切り替えのタブと
	// 共通の方式バナー（partials/publish_method_banner.html）のデータ。ほかの画面では nil（publish_method.go）。
	PublishMethod *publishMethodView
	// AISwitch は AI の画面（AI・外部連携・使う AI・外部の AI アプリとつなぐ）の先頭に出す「AI機能を使う」のスイッチと
	// 公開サーバーの状態のデータ（partials/ai_tabs.html）。管理者がその3画面を開いたときだけ入れ、ほかでは nil
	// （handlers_ai_integrations.go の layoutAISwitch）。
	AISwitch *aiIntegrationsData
}

func (p page) T(key string, args ...any) string { return i18n.Translate(p.Lang, key, args...) }

// pageTitle は管理画面のページの題名（<title>）を、表示言語の辞書から引く。
func pageTitle(c echo.Context, key string) string { return i18n.Translate(string(language(c)), key) }

// AtPath は表示中の画面の URL のパス（クエリを除く）が paths のどれかか。ナビ・タブの aria-current に使う。
func (p page) AtPath(paths ...string) bool {
	current, _, _ := strings.Cut(p.CurrentPath, "?")
	for _, path := range paths {
		if current == path {
			return true
		}
	}
	return false
}

// render はページ name を layout に埋め込んで code で返す。
// ログイン中のユーザー・CSRF トークン・?flash= のトースト通知は自動で設定する。
func (s *Server) render(c echo.Context, code int, name, title string, data any) error {
	flash := c.QueryParam("flash")
	if _, ok := flashMessages[flash]; !ok {
		flash = ""
	}
	return s.renderPage(c, code, name, title, data, flash, false)
}

// renderError は msg をエラーのトースト通知として表示するページを返す。
func (s *Server) renderError(c echo.Context, code int, name, title, msg string, data any) error {
	return s.renderPage(c, code, name, title, data, msg, true)
}

func (s *Server) renderPage(c echo.Context, code int, name, title string, data any, flash string, flashError bool) error {
	lang := language(c)
	p := page{
		SiteName:    s.getSiteName(),
		Version:     version.Version,
		Title:       title,
		CSRFToken:   csrfToken(c),
		Flash:       flash,
		FlashKey:    flash,
		FlashError:  flashError || flashErrorKeys[flash],
		Data:        data,
		Lang:        string(lang),
		CurrentPath: c.Request().URL.RequestURI(),
		Desktop:     s.cfg.Desktop,
	}
	p.AIForcedOffByEnv = s.aiEnvForcedOff()
	p.AIEnabled = !p.AIForcedOffByEnv && s.aiEnabledSetting()
	// AI 機能が OFF のサイトでは、AI アプリ（MCP）に触れないパスワード変更の通知にする。
	if flash == "user_password_updated" && !p.AIEnabled {
		flash = flashPasswordUpdatedNoAI
	}
	// 閲覧制限中の公開専用モードの OAuth の同意画面（本人を確かめる前）は、タイトルにもサイト名を出さない（oauthHidesSiteName）。
	if hide, _ := c.Get(hideSiteNameContextKey).(bool); hide {
		p.SiteName = ""
	}
	// ログインしていない画面（ログイン・OAuth の同意画面）では使わないため、ログイン中だけ読む。
	if user, ok := auth.UserFromContext(c); ok && user != nil {
		p.MultilingualEnabled = s.isMultilingualEnabled()
	}
	if key, ok := flashKeys[flash]; ok {
		p.Flash = i18n.Translate(string(lang), key)
	} else if msg, ok := flashMessages[flash]; ok {
		// 翻訳キーが未登録の通知は、キー名ではなく既定（日本語）の文言を表示する。
		p.Flash = msg
	}
	p.FaviconURL, p.FaviconType = s.layoutFavicon()
	p.User, _ = auth.UserFromContext(c)
	if s.cfg.Sites != nil && p.User != nil {
		p.Sites = s.layoutSites(string(lang))
	}
	p.WindowScript = s.windowScriptFor(c)
	s.layoutRemoteState(c, &p)
	s.layoutPublishMethod(&p)
	s.layoutAISwitch(c, &p)
	if s.cfg.Desktop && p.User != nil {
		p.QuitPending = s.quitPending(c)
	}
	return c.Render(code, name, p)
}

// layoutFavicon は管理画面・公開サイトのレイアウトの <link rel="icon"> に出す Favicon の URL と MIME タイプを返す。
// 既定の Favicon は /static/ の不変キャッシュで配信するため、ほかの静的アセットと同じく内容のハッシュを付ける。
func (s *Server) layoutFavicon() (url, mimeType string) {
	url, mimeType = s.favicon()
	if url == DefaultFaviconURL {
		url = static.URL(strings.TrimPrefix(DefaultFaviconURL, "/static/"))
	}
	return url, mimeType
}

// flashErrorKeys はエラー（赤）として表示する通知のキー。
var flashErrorKeys = map[string]bool{
	"remote_publish_failed": true, "remote_unpublish_failed": true, "remote_published_attachments_failed": true,
	"publish_method_unavailable": true,
}

var flashKeys = map[string]string{
	// Identifier keys
	"saved": "saved", "published": "published", "created": "created", "deleted": "deleted",
	"unlocked": "unlocked", "logged_out": "logged_out", "submitted": "submitted",
	"rejected": "rejected", "approved": "approved", "scheduled": "scheduled",
	"restored": "restored", "token_revoked": "token_revoked", "token_created": "token_created",
	"login_error": "login_error", "backup_restored": "backup_restored", "backup_relogin": "backup_relogin",
	"media_saved": "media_saved", "media_deleted": "media_deleted", "media_uploaded": "media_uploaded",
	"theme_saved": "theme_saved", "structure_saved": "structure_saved", "card_images_saved": "card_images_saved",
}

// langCookieContextKey は、要求を受けたサイトの表示言語の Cookie の名前（auth.CookieNames の Lang）を
// echo.Context に入れるキー。Server.routes が要求ごとに入れる。
const langCookieContextKey = "server.langCookie"

// langCookieName は、要求を受けたサイトの表示言語の Cookie の名前を返す。
func langCookieName(c echo.Context) string {
	if name, ok := c.Get(langCookieContextKey).(string); ok {
		return name
	}
	return auth.CookieNamesFor("").Lang
}

func language(c echo.Context) i18n.Lang {
	// 管理画面の ?lang= は記事の言語（一覧の絞り込み・翻訳先）なので、表示言語には使わない（Cookie に従う）。
	if q := c.QueryParam("lang"); q != "" && !isAdminPath(c.Request().URL.Path) {
		return i18n.Normalize(q)
	}
	if cookie, err := c.Cookie(langCookieName(c)); err == nil && cookie.Value != "" {
		return i18n.Normalize(cookie.Value)
	}
	if al := c.Request().Header.Get("Accept-Language"); al != "" {
		alLower := strings.ToLower(al)
		enIdx := strings.Index(alLower, "en")
		jaIdx := strings.Index(alLower, "ja")
		if enIdx >= 0 && (jaIdx == -1 || enIdx < jaIdx) {
			return i18n.EN
		}
	}
	return i18n.JA
}

func localizedMessage(c echo.Context, key string) string {
	return i18n.Translate(string(language(c)), key)
}

// scheduledMessage は予約したあとの通知（「2026/11/03 09:00 に公開するよう予約しました」）。日時は予約のバナーと
// 同じ書式で出す。?flash=scheduled の編集画面と、承認の fetch の応答で使う。
func scheduledMessage(c echo.Context, at time.Time) string {
	return i18n.Translate(string(language(c)), "scheduled_at", at.In(jst).Format(displayTimeLayout))
}
