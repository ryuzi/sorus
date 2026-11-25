package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
	"sorus/internal/i18n"
	"sorus/internal/publish"
	"sorus/templates"
)

// 「公開先」画面。公開する人が公開先を1つ選び（レンタルサーバーの FTP・SFTP、Cloudflare Pages、フォルダに書き出すだけ）、
// 接続を試してから保存する。設定は settings テーブルに保存し、パスワード・API トークン・秘密鍵はブラウザに返さない。
// 「サイトに反映」はこの画面とダッシュボードに置き、反映は handlers_publish_run.go がバックグラウンドで行う。

const (
	// publishSettingKind は選んだ公開先の種類（publishKind*）。空なら未設定。
	publishSettingKind = "publish_kind"
	// publishSettingServer・publishSettingCloudflare・publishSettingFolder は公開先ごとの設定（JSON）。
	// 種類を切り替えても、ほかの公開先の設定は残す。
	publishSettingServer     = "publish_server"
	publishSettingCloudflare = "publish_cloudflare"
	publishSettingGitHub     = "publish_github"
	publishSettingNetlify    = "publish_netlify"
	publishSettingS3         = "publish_s3"
	publishSettingFolder     = "publish_folder"
	// publishSettingKnownHosts は SFTP の接続先（host:port）ごとに、初めて接続したときに覚えた鍵の指紋（JSON）。
	publishSettingKnownHosts = "publish_sftp_known_hosts"
	// publishSettingManifestPrefix に公開先の識別子を付けたキーに、その公開先へ最後に反映できた書き出しの
	// Manifest（JSON）を保存する。
	publishSettingManifestPrefix = "publish_manifest:"
	// publishSettingLast は最後に終わった反映の結果（JSON）。再起動しても画面に出す。
	publishSettingLast = "publish_last_result"
	// publishSettingAutoReflect は記事を今すぐ公開したとき（「公開する」「承認して公開」）に、自動でサイトに反映するか
	// （「記事公開時に自動でサイトに反映する」）。"1" なら反映し、"0" なら「サイトに反映」を押すまで反映しない。
	// 未保存なら反映する（autoReflectOnPublish）。
	publishSettingAutoReflect = "auto_reflect_on_publish"

	publishKindServer     = "server"
	publishKindCloudflare = "cloudflare"
	publishKindGitHub     = "github"
	publishKindNetlify    = "netlify"
	publishKindS3         = "s3"
	publishKindFolder     = "folder"

	publishProtocolFTP  = "ftp"
	publishProtocolSFTP = "sftp"

	// publishFolderName は「フォルダに書き出すだけ」の既定のフォルダの名前。データの置き場所の隣に作る。
	publishFolderName = "site-export"
	// maxPublishSecretLength はパスワード・トークン・秘密鍵として受け付ける長さ。
	maxPublishSecretLength = 16 << 10
)

// purgeCDNCache is the standard-library Cloudflare purge implementation. Tests replace it to avoid external requests.
var purgeCDNCache = publish.PurgeCloudflare

// errPublishFolderInData は、書き出し先のフォルダがアップロード画像・添付資料の保存先の中にあること。
var errPublishFolderInData = errors.New("publish folder is inside the data folder")

func init() {
	if !slices.Contains(pageNames, "publish") {
		pageNames = append(pageNames, "publish")
	}
	flashMessages["publish_saved"] = "公開先を保存しました"
	flashMessages["publish_trusted"] = "接続先の新しい鍵を信頼しました。もう一度「サイトに反映」を押してください"
	// 記事を今すぐ公開したあとの通知（reflectAfterPublish）。文言は i18n のカタログで言語ごとに出す。
	flashMessages[flashPublishedReflecting] = "記事を公開しました。静的サイトへの反映を開始しました"
	flashMessages[flashPublishedNoTarget] = "記事を公開しました。（公開先が未設定のため、公開先設定画面から設定してください）"
	flashMessages[flashPublishedManual] = "記事を公開しました。静的サイトへ反映するには『サイトに反映』を実行してください"
	flashMessages["restored_reflecting"] = "過去のバージョンに復元しました。静的サイトへの反映を開始しました"
	flashMessages["restored_manual"] = "過去のバージョンに復元しました。静的サイトへ反映するには『サイトに反映』を実行してください"
	flashMessages["restored_no_target"] = "過去のバージョンに復元しました。公開先設定画面で公開先を設定してください"
	flashMessages["settings_saved_manual"] = "設定を保存しました"
	flashMessages["settings_saved_reflecting"] = "設定を保存しました。公開サイトへ自動反映中..."
	flashMessages["structure_saved_manual"] = "サイト構成を保存しました"
	flashMessages["structure_saved_reflecting"] = "設定を保存しました。公開サイトへ自動反映中..."
	for _, key := range []string{flashPublishedReflecting, flashPublishedNoTarget, flashPublishedManual} {
		flashKeys[key] = key
	}
	for _, key := range []string{"restored_reflecting", "restored_manual", "restored_no_target"} {
		flashKeys[key] = key
	}
}

// publishPanelTemplate はダッシュボードに読み込む「サイトに反映」の部分（templates/admin/publish_panel.html）。
var publishPanelTemplate = template.Must(template.New("publish_panel.html").Funcs(templateFuncs).
	ParseFS(templates.FS, "admin/publish_panel.html"))

// registerPublishRoutes は「公開先」画面と「サイトに反映」の API を、ログイン済み・CSRF 保護済みの /admin グループに
// 管理者専用で登録する。
func (s *Server) registerPublishRoutes(authed *echo.Group) {
	g := authed.Group("/publish", auth.RequireRole(auth.RoleAdmin))
	g.GET("", s.handlePublishPage)
	g.POST("", s.handlePublishSave)
	g.POST("/test", s.handlePublishTest)
	authed.POST("/api/publish/test-cdn-purge", s.handleCDNPurgeTest, auth.RequireRole(auth.RoleAdmin))
	authed.POST("/api/publish/health-check", s.handlePublishHealthCheck, auth.RequireRole(auth.RoleAdmin))
	g.POST("/reflect", s.handlePublishReflect)
	g.POST("/run", s.handlePublishReflect)
	g.GET("/status", s.handlePublishStatus)
	g.GET("/panel", s.handlePublishPanel)
	g.GET("/zip", s.handlePublishZip)
	g.POST("/trust-host-key", s.handlePublishTrustHostKey)
	g.POST("/replace-site", s.handlePublishReplaceSite)
	// 公開方式（公開サーバー・静的サイト）の切り替えと解除（publish_method.go）。
	g.POST("/method", s.handlePublishMethodSwitch)
	g.POST("/method/clear", s.handlePublishMethodClear)
}

// --- 設定 ---

// publishServerConfig はレンタルサーバー（FTP・SFTP）の設定。
type publishServerConfig struct {
	Protocol   string `json:"protocol"`
	Host       string `json:"host"`
	Port       int    `json:"port,omitempty"`
	User       string `json:"user"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Root       string `json:"root"`
	// AllowPlainFTP は FTP のサーバーが暗号化に対応していないときに、暗号化しないで送るか（「暗号化しないで送る」）。
	// 既定は送らない。この項目が無い保存済みの設定も送らない。
	AllowPlainFTP bool `json:"allow_plain_ftp,omitempty"`
}

// plainFTP は FTP で、暗号化しないで送ることを選んでいるか。
func (c publishServerConfig) plainFTP() bool {
	return c.Protocol == publishProtocolFTP && c.AllowPlainFTP
}

// addr は host:port（ポートが未指定なら方式の既定値）。
func (c publishServerConfig) addr() string {
	port := c.Port
	if port == 0 {
		port = 21
		if c.Protocol == publishProtocolSFTP {
			port = 22
		}
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

// publishCloudflareConfig は Cloudflare Pages の設定。
type publishCloudflareConfig struct {
	AccountID string `json:"account_id"`
	Project   string `json:"project"`
	APIToken  string `json:"api_token,omitempty"`
}

type publishS3Config struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SiteURL         string `json:"site_url,omitempty"`
}

type publishNetlifyConfig struct {
	SiteID string `json:"site_id"`
	Token  string `json:"token,omitempty"`
}

func (c publishNetlifyConfig) client() publish.NetlifyConfig {
	return publish.NetlifyConfig{SiteID: c.SiteID, Token: c.Token, API: publishNetlifyAPI}
}

var publishNetlifyAPI = publish.DefaultNetlifyAPI

func (c publishS3Config) client() publish.S3Config {
	return publish.S3Config{Endpoint: c.Endpoint, Bucket: c.Bucket, Region: c.Region, AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SiteURL: c.SiteURL}
}

// publishCloudflareAPI は Cloudflare API の起点。試験で偽物の API に差し替える。
var publishCloudflareAPI = publish.DefaultCloudflareAPI

func (c publishCloudflareConfig) client() publish.CloudflareConfig {
	return publish.CloudflareConfig{AccountID: c.AccountID, APIToken: c.APIToken, Project: c.Project, API: publishCloudflareAPI}
}

// publishFolderConfig は「フォルダに書き出すだけ」の設定。
type publishFolderConfig struct {
	Path string `json:"path"`
}

// publishConfig は保存済みの公開先の設定。
type publishConfig struct {
	Kind       string
	Server     publishServerConfig
	Cloudflare publishCloudflareConfig
	GitHub     publishGitHubConfig
	Netlify    publishNetlifyConfig
	S3         publishS3Config
	Folder     publishFolderConfig
}

func (s *Server) loadPublishConfig() publishConfig {
	cfg := publishConfig{Kind: s.getSetting(publishSettingKind, "")}
	_ = json.Unmarshal([]byte(s.getSetting(publishSettingServer, "{}")), &cfg.Server)
	_ = json.Unmarshal([]byte(s.getSetting(publishSettingCloudflare, "{}")), &cfg.Cloudflare)
	_ = json.Unmarshal([]byte(s.getSetting(publishSettingGitHub, "{}")), &cfg.GitHub)
	_ = json.Unmarshal([]byte(s.getSetting(publishSettingNetlify, "{}")), &cfg.Netlify)
	_ = json.Unmarshal([]byte(s.getSetting(publishSettingS3, "{}")), &cfg.S3)
	_ = json.Unmarshal([]byte(s.getSetting(publishSettingFolder, "{}")), &cfg.Folder)
	return cfg
}

func (s *Server) savePublishConfig(cfg publishConfig) error {
	values := map[string]any{publishSettingServer: cfg.Server, publishSettingCloudflare: cfg.Cloudflare, publishSettingGitHub: cfg.GitHub, publishSettingNetlify: cfg.Netlify, publishSettingS3: cfg.S3, publishSettingFolder: cfg.Folder}
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertSetting(tx, publishSettingKind, cfg.Kind); err != nil {
		return err
	}
	for k, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := upsertSetting(tx, k, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// settingExecer は settings を書き換える DB（*sqlx.DB）またはトランザクション（*sqlx.Tx）。
type settingExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// upsertSetting は settings の key に value を保存する。
func upsertSetting(db settingExecer, key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, key, value)
	return err
}

// configured は公開先が選ばれていて、反映に必要な値がそろっているか。
func (c publishConfig) configured() bool {
	switch c.Kind {
	case publishKindServer:
		return c.Server.Host != "" && c.Server.User != "" && c.Server.Root != "" &&
			(c.Server.Password != "" || c.Server.PrivateKey != "")
	case publishKindCloudflare:
		return c.Cloudflare.client().Validate() == nil
	case publishKindGitHub:
		return c.GitHub.client().Validate() == nil
	case publishKindNetlify:
		return c.Netlify.client().Validate() == nil
	case publishKindS3:
		return c.S3.client().Validate() == nil
	case publishKindFolder:
		return c.Folder.Path != ""
	}
	return false
}

// label は公開先を1行で表した文（例: Cloudflare Pages（my-site））。
func (c publishConfig) label() string { return c.labelIn("ja") }

// labelIn は label を表示言語 lang で返す（英語では種類の名前と括弧を英語にする）。
func (c publishConfig) labelIn(lang string) string {
	if i18n.Normalize(lang) == i18n.EN {
		switch c.Kind {
		case publishKindServer:
			return "Rental server (" + strings.ToUpper(c.Server.Protocol) + ": " + c.Server.Root + " on " + c.Server.Host + ")"
		case publishKindCloudflare:
			return "Cloudflare Pages (" + c.Cloudflare.Project + ")"
		case publishKindGitHub:
			return "GitHub Pages (" + c.GitHub.Owner + "/" + c.GitHub.Repository + ")"
		case publishKindNetlify:
			return "Netlify (" + c.Netlify.SiteID + ")"
		case publishKindS3:
			return "S3-compatible storage (" + c.S3.Bucket + ")"
		case publishKindFolder:
			return "Folder (" + c.Folder.Path + ")"
		}
		return ""
	}
	switch c.Kind {
	case publishKindServer:
		return "レンタルサーバー（" + strings.ToUpper(c.Server.Protocol) + ": " + c.Server.Host + " の " + c.Server.Root + "）"
	case publishKindCloudflare:
		return "Cloudflare Pages（" + c.Cloudflare.Project + "）"
	case publishKindGitHub:
		return "GitHub Pages（" + c.GitHub.Owner + "/" + c.GitHub.Repository + "）"
	case publishKindNetlify:
		return "Netlify（" + c.Netlify.SiteID + "）"
	case publishKindS3:
		return "S3 互換ストレージ（" + c.S3.Bucket + "）"
	case publishKindFolder:
		return "フォルダ（" + c.Folder.Path + "）"
	}
	return ""
}

// destinationID は Manifest を分けて保存するための公開先の識別子。接続先やフォルダが変われば別の公開先として扱い、
// 前回の一覧を使わない（初めての反映と同じく、もとからあるファイルを消さない）。
func (c publishConfig) destinationID() string {
	var key string
	switch c.Kind {
	case publishKindServer:
		key = strings.Join([]string{c.Kind, c.Server.Protocol, strings.ToLower(c.Server.addr()), c.Server.User, c.Server.Root}, "\x00")
	case publishKindCloudflare:
		key = strings.Join([]string{c.Kind, c.Cloudflare.AccountID, c.Cloudflare.Project}, "\x00")
	case publishKindGitHub:
		key = strings.Join([]string{c.Kind, c.GitHub.Owner, c.GitHub.Repository, c.GitHub.Branch, c.GitHub.Directory}, "\x00")
	case publishKindNetlify:
		key = strings.Join([]string{c.Kind, c.Netlify.SiteID}, "\x00")
	case publishKindS3:
		key = strings.Join([]string{c.Kind, strings.TrimRight(c.S3.Endpoint, "/"), c.S3.Bucket}, "\x00")
	case publishKindFolder:
		key = strings.Join([]string{c.Kind, c.Folder.Path}, "\x00")
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func (s *Server) loadPublishManifest(id string) (publish.Manifest, error) {
	raw := s.getSetting(publishSettingManifestPrefix+id, "")
	if raw == "" {
		return nil, nil
	}
	var m publish.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m == nil {
		m = publish.Manifest{}
	}
	return m, nil
}

func (s *Server) savePublishManifest(id string, m publish.Manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return upsertSetting(s.db, publishSettingManifestPrefix+id, string(b))
}

func (s *Server) saveLastOutcome(out publishOutcome) error {
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return upsertSetting(s.db, publishSettingLast, string(b))
}

func (s *Server) loadLastOutcome() *publishOutcome {
	raw := s.getSetting(publishSettingLast, "")
	if raw == "" {
		return nil
	}
	var out publishOutcome
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return &out
}

// knownHostKeys は SFTP の接続先ごとに覚えている鍵の指紋。
func (s *Server) knownHostKeys() map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s.getSetting(publishSettingKnownHosts, "{}")), &m)
	return m
}

func (s *Server) rememberHostKey(addr, fingerprint string) error {
	m := s.knownHostKeys()
	m[strings.ToLower(addr)] = fingerprint
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return upsertSetting(s.db, publishSettingKnownHosts, string(b))
}

// defaultPublishFolder は「フォルダに書き出すだけ」の既定のフォルダ（データの置き場所の中の site-export）。
// デスクトップモードではアップロード画像の保存先がデータの置き場所の直下にあるため、その隣になる。
func (s *Server) defaultPublishFolder() string {
	up, err := filepath.Abs(s.cfg.UploadDir)
	if err != nil {
		return publishFolderName
	}
	return filepath.Join(filepath.Dir(up), publishFolderName)
}

// checkPublishFolder は書き出し先のフォルダ p を確かめ、絶対パスを返す。危ない場所（ホームフォルダ全体・作業中の
// フォルダ・.git やデータベースを含むフォルダなど: checkCleanExportDir）と、画像・添付資料の保存先の中は断る。
func (s *Server) checkPublishFolder(p string) (string, error) {
	abs, err := checkCleanExportDir(p, s.cfg.UploadDir, s.cfg.AttachmentDir)
	if err != nil {
		return "", err
	}
	for _, d := range []string{s.cfg.UploadDir, s.cfg.AttachmentDir} {
		if da, err := filepath.Abs(d); err == nil && isWithin(realPath(abs), realPath(da)) {
			return "", errPublishFolderInData
		}
	}
	return abs, nil
}

// openPublishRemote は FTP・SFTP・フォルダの公開先に接続する。SFTP では初めての接続先の鍵を覚え、覚えた鍵と
// 違えば確認待ちにして publish.HostKeyChangedError を返す。
func (s *Server) openPublishRemote(ctx context.Context, cfg publishConfig) (publish.Remote, error) {
	switch cfg.Kind {
	case publishKindS3:
		return publish.NewS3(cfg.S3.client())
	case publishKindFolder:
		dir, err := s.checkPublishFolder(cfg.Folder.Path)
		if err != nil {
			return nil, err
		}
		return publish.OpenFolder(dir)
	case publishKindServer:
		sc := cfg.Server
		if sc.Protocol == publishProtocolFTP {
			return publish.DialFTP(ctx, publish.FTPConfig{Host: sc.Host, Port: sc.Port, User: sc.User, Password: sc.Password, Root: sc.Root,
				AllowPlaintext: sc.AllowPlainFTP})
		}
		addr := strings.ToLower(sc.addr())
		known := s.knownHostKeys()[addr]
		r, err := publish.DialSFTP(ctx, publish.SFTPConfig{Host: sc.Host, Port: sc.Port, User: sc.User, Password: sc.Password,
			PrivateKey: sc.PrivateKey, Root: sc.Root, KnownHostKey: known})
		var hk *publish.HostKeyChangedError
		if errors.As(err, &hk) {
			st := s.publishState()
			st.mu.Lock()
			st.hostKey = &pendingHostKey{Addr: addr, Known: hk.Known, Got: hk.Got}
			st.mu.Unlock()
		}
		if err != nil {
			return nil, err
		}
		if known == "" {
			if err := s.rememberHostKey(addr, r.HostKey()); err != nil {
				r.Close()
				return nil, err
			}
		}
		return r, nil
	}
	return nil, errors.New("publish: no destination")
}

// fillPagesBaseURL は公開サイトURLが未設定・この PC 内（localhost）・自動で入れた Pages の URL のままなら、
// Cloudflare Pages の公開 URL にする。
func (s *Server) fillPagesBaseURL(project, pagesURL string) error {
	cur := s.getSetting("base_url", s.getBaseURL())
	if cur != "" && len(baseURLWarnings(cur)) == 0 && cur != "https://"+project+".pages.dev" {
		return nil
	}
	if cur == pagesURL {
		return nil
	}
	return s.saveBaseURL(pagesURL)
}

// saveBaseURL は公開サイトURL（サイト基本設定の base_url）を保存し、サーバーメモリ上の値も更新する。
func (s *Server) saveBaseURL(u string) error {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if u != "" {
		valid, err := normalizeSiteURL(u)
		if err != nil {
			return fmt.Errorf("save base url: %w", err)
		}
		u = valid
	}
	if err := upsertSetting(s.db, "base_url", u); err != nil {
		return err
	}
	s.setBaseURL(u)
	return nil
}

// normalizeSiteURL は「公開サイトのURL」の入力を検査し、末尾の / を除いて返す。
func normalizeSiteURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("「公開サイトのURL」は https://example.jp の形で入力してください")
	}
	return raw, nil
}

// --- 画面 ---

// publishPageData は publish.html に渡すデータ。パスワード・API トークン・秘密鍵そのものは入れない。
type publishPageData struct {
	CSRFToken  string
	Kind       string
	Configured bool
	Label      string
	Server     publishServerView
	Cloudflare publishCloudflareView
	GitHub     publishGitHubView
	Netlify    publishNetlifyView
	S3         publishS3View
	// FolderPath は書き出し先のフォルダ（未設定なら既定のフォルダ）。
	FolderPath string
	// SiteURL は公開サイトのURL（サイト基本設定の base_url）。
	SiteURL string
	// PlainFTP は保存済みの公開先が、暗号化しないで送る FTP か（反映の欄に注意を出し続ける）。
	PlainFTP bool
	// AutoReflect は「記事公開時に自動でサイトに反映する」にチェックを入れるか（publishSettingAutoReflect）。
	AutoReflect bool
	CDNZoneID   string
	CDNHasToken bool
	Status      publishStatus
	Health      *publishHealthStatus
	HostKey     *pendingHostKey
	Errors      []string
	// RemoteConnected は公開サーバー（sorus 公開サーバー同期）に接続済みか。接続済みなら公開方式は公開サーバーで、
	// この画面は ?view=static で静的サイトの設定を開いたときだけ表示する（handlePublishPage）。RemoteHost はその接続先。
	RemoteConnected bool
	RemoteHost      string
	// StaticActive はいまの公開方式が静的サイトか（待機中なら「サイトに反映」を押せない。publish_method.go）。
	StaticActive bool
}

type publishServerView struct {
	Protocol, Host, Port, User, Root string
	HasPassword, HasKey              bool
	AllowPlainFTP                    bool
}

type publishCloudflareView struct {
	AccountID, Project string
	HasToken           bool
	TokenPreview       string
}

type publishS3View struct {
	Endpoint, Bucket, Region, AccessKeyID string
	HasSecret                             bool
}

type publishNetlifyView struct {
	SiteID   string
	HasToken bool
}

func (s *Server) publishPageData(c echo.Context, cfg publishConfig) publishPageData {
	d := publishPageData{
		CSRFToken: csrfToken(c), Kind: cfg.Kind, Configured: cfg.configured(), Label: cfg.label(),
		Server: publishServerView{Protocol: cfg.Server.Protocol, Host: cfg.Server.Host, User: cfg.Server.User, Root: cfg.Server.Root,
			HasPassword: cfg.Server.Password != "", HasKey: cfg.Server.PrivateKey != "", AllowPlainFTP: cfg.Server.AllowPlainFTP},
		Cloudflare: publishCloudflareView{AccountID: cfg.Cloudflare.AccountID, Project: cfg.Cloudflare.Project,
			HasToken: cfg.Cloudflare.APIToken != ""},
		GitHub:      publishGitHubView{Owner: cfg.GitHub.Owner, Repository: cfg.GitHub.Repository, Branch: cfg.GitHub.Branch, Directory: cfg.GitHub.Directory, HasToken: cfg.GitHub.Token != ""},
		Netlify:     publishNetlifyView{SiteID: cfg.Netlify.SiteID, HasToken: cfg.Netlify.Token != ""},
		S3:          publishS3View{Endpoint: cfg.S3.Endpoint, Bucket: cfg.S3.Bucket, Region: cfg.S3.Region, AccessKeyID: cfg.S3.AccessKeyID, HasSecret: cfg.S3.SecretAccessKey != ""},
		FolderPath:  cfg.Folder.Path,
		SiteURL:     s.getSetting("base_url", s.getBaseURL()),
		PlainFTP:    cfg.Kind == publishKindServer && cfg.Server.plainFTP(),
		AutoReflect: s.autoReflectOnPublish(),
		CDNZoneID:   s.getSetting("cdn_cf_zone_id", ""),
		CDNHasToken: s.getSetting("cdn_cf_api_token", "") != "",
		Status:      s.currentPublishStatus(),
		Health:      s.loadPublishHealth(),
	}
	if d.Server.Protocol == "" {
		d.Server.Protocol = publishProtocolSFTP
	}
	if cfg.Server.Port != 0 {
		d.Server.Port = strconv.Itoa(cfg.Server.Port)
	}
	if d.Cloudflare.HasToken {
		d.Cloudflare.TokenPreview = maskToken(cfg.Cloudflare.APIToken)
	}
	if d.FolderPath == "" {
		d.FolderPath = s.defaultPublishFolder()
	}
	st := s.publishState()
	st.mu.Lock()
	d.HostKey = st.hostKey
	st.mu.Unlock()
	if remote := s.loadRemoteConfig(); remote.connected() {
		d.RemoteConnected = true
		d.RemoteHost = remoteServerHost(remote.URL)
	}
	d.StaticActive = resolvePublishMethod(s.storedPublishMethod(), d.RemoteConnected, d.Configured) == publishMethodStatic
	return d
}

// handlePublishPage は左メニューの「公開先」の画面。公開方法は公開サーバーか静的サイトのどちらか1つなので、
// 公開サーバーに接続済みならその画面（/admin/settings/remote）へ送り、どちらも未設定なら公開方法を選ぶ2択を出す。
// ?view=static は、公開サーバーに接続済みでも静的サイトの設定を開くときに使う。
func (s *Server) handlePublishPage(c echo.Context) error {
	if c.QueryParam("view") != "static" && s.remotePublishActive() {
		return c.Redirect(http.StatusSeeOther, "/admin/settings/remote")
	}
	return s.render(c, http.StatusOK, "publish", pageTitle(c, "settings_nav_publish"), s.publishPageData(c, s.loadPublishConfig()))
}

// publishPageURL は静的サイトの設定の操作（保存・鍵の信頼・反映など）のあとに戻る /admin/publish の URL。
// 公開サーバーに接続中は ?view=static を付けないと /admin/settings/remote へ送られてしまい、保存完了の
// フラッシュも消えるので、静的サイトの設定画面に留まるよう付ける。flash が空ならフラッシュを付けない。
func (s *Server) publishPageURL(flash string) string {
	var params []string
	if s.remotePublishActive() {
		params = append(params, "view=static")
	}
	if flash != "" {
		params = append(params, "flash="+url.QueryEscape(flash))
	}
	if len(params) == 0 {
		return "/admin/publish"
	}
	return "/admin/publish?" + strings.Join(params, "&")
}

// publishForm はフォームの入力から公開先の設定と公開サイトのURLを組み立てる。パスワード・トークン・鍵の欄が空なら
// 保存済みの値を使う。入力の誤りは利用者に見せる文で返す。
func (s *Server) publishForm(c echo.Context, saved publishConfig) (cfg publishConfig, siteURL string, errs []string) {
	cfg = saved
	cfg.Kind = c.FormValue("kind")
	secret := func(name, keep string) string {
		if v := strings.TrimSpace(c.FormValue(name)); v != "" {
			if len(v) > maxPublishSecretLength {
				errs = append(errs, "入力が長すぎます")
				return keep
			}
			return v
		}
		return keep
	}
	switch cfg.Kind {
	case publishKindServer:
		sc := publishServerConfig{
			Protocol: c.FormValue("protocol"),
			Host:     strings.TrimSpace(c.FormValue("host")),
			User:     strings.TrimSpace(c.FormValue("user")),
			Root:     strings.TrimSpace(c.FormValue("root")),
			// チェックを外して送られたフォームには値が無いため、無ければ送らない（既定）にする。
			AllowPlainFTP: c.FormValue("allow_plain_ftp") != "",
		}
		// 接続先が変わったら、別のサーバーのパスワード・鍵を送らない。
		sameServer := sc.Host == saved.Server.Host && sc.User == saved.Server.User
		keepPassword, keepKey := "", ""
		if sameServer {
			keepPassword, keepKey = saved.Server.Password, saved.Server.PrivateKey
		}
		sc.Password = secret("password", keepPassword)
		sc.PrivateKey = secret("private_key", keepKey)
		if c.FormValue("clear_private_key") != "" {
			sc.PrivateKey = ""
		}
		if sc.Protocol != publishProtocolFTP && sc.Protocol != publishProtocolSFTP {
			errs = append(errs, "接続のしかた（SFTP か FTP）を選んでください")
		}
		if h := strings.ToLower(sc.Host); strings.Contains(h, "://") {
			// 「ftp://example.jp」のように貼り付けられたら、ホスト名だけにする。
			if u, err := url.Parse(sc.Host); err == nil && u.Hostname() != "" {
				sc.Host = u.Hostname()
			}
		}
		if sc.Host == "" || strings.ContainsAny(sc.Host, " /\\@") {
			errs = append(errs, "ホスト名を入力してください（例: sv1234.example.jp）")
		}
		if p := strings.TrimSpace(c.FormValue("port")); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				errs = append(errs, "ポート番号は 1〜65535 の数字で入力してください（空欄なら SFTP は 22、FTP は 21）")
			}
			sc.Port = n
		}
		if sc.User == "" {
			errs = append(errs, "ユーザー名を入力してください")
		}
		if sc.Password == "" && sc.PrivateKey == "" {
			errs = append(errs, "パスワードを入力してください")
		}
		if root, err := publish.CleanRoot(sc.Root); err != nil {
			errs = append(errs, "アップロード先のフォルダを入力してください（例: /public_html）。ほかのファイルを消さないよう、空欄や「/」だけは指定できません")
		} else {
			sc.Root = root
		}
		cfg.Server = sc
		u, err := normalizeSiteURL(c.FormValue("server_site_url"))
		if err != nil {
			errs = append(errs, err.Error())
		}
		siteURL = u
	case publishKindCloudflare:
		cc := publishCloudflareConfig{
			AccountID: strings.ToLower(strings.TrimSpace(c.FormValue("cf_account_id"))),
			Project:   strings.ToLower(strings.TrimSpace(c.FormValue("cf_project"))),
		}
		keep := ""
		if cc.AccountID == saved.Cloudflare.AccountID {
			keep = saved.Cloudflare.APIToken
		}
		cc.APIToken = secret("cf_api_token", keep)
		cfg.Cloudflare = cc
		if err := cc.client().Validate(); err != nil {
			switch {
			case strings.HasSuffix(err.Error(), "account id"):
				errs = append(errs, "アカウント ID は、Cloudflare の画面に出ている32文字の英数字を貼り付けてください")
			case strings.HasSuffix(err.Error(), "project"):
				errs = append(errs, "プロジェクト名は、半角の英小文字・数字・ハイフンで入力してください（例: my-site）")
			default:
				errs = append(errs, "API トークンを貼り付けてください")
			}
		}
	case publishKindGitHub:
		gc := publishGitHubConfig{Owner: strings.TrimSpace(c.FormValue("github_owner")), Repository: strings.TrimSpace(c.FormValue("github_repository")), Branch: strings.TrimSpace(c.FormValue("github_branch")), Directory: strings.Trim(strings.TrimSpace(c.FormValue("github_directory")), "/")}
		if gc.Branch == "" {
			gc.Branch = "main"
		}
		keep := ""
		if gc.Owner == saved.GitHub.Owner && gc.Repository == saved.GitHub.Repository {
			keep = saved.GitHub.Token
		}
		gc.Token = secret("github_token", keep)
		cfg.GitHub = gc
		if err := gc.client().Validate(); err != nil {
			switch {
			case strings.HasSuffix(err.Error(), "owner"):
				errs = append(errs, "GitHub のユーザー名または組織名を入力してください")
			case strings.HasSuffix(err.Error(), "repository"):
				errs = append(errs, "GitHub のリポジトリ名を入力してください")
			case strings.HasSuffix(err.Error(), "branch"):
				errs = append(errs, "公開元のブランチ名を入力してください")
			case strings.HasSuffix(err.Error(), "directory"):
				errs = append(errs, "リポジトリ内のフォルダを確認してください")
			default:
				errs = append(errs, "GitHub の Personal Access Token を入力してください")
			}
		}
		u := "https://" + strings.ToLower(gc.Owner) + ".github.io"
		if gc.Repository != "" && !strings.EqualFold(gc.Repository, gc.Owner+".github.io") {
			u += "/" + gc.Repository
		}
		siteURL = u
	case publishKindNetlify:
		nc := publishNetlifyConfig{SiteID: strings.TrimSpace(c.FormValue("netlify_site_id"))}
		keep := ""
		if nc.SiteID == saved.Netlify.SiteID {
			keep = saved.Netlify.Token
		}
		nc.Token = secret("netlify_token", keep)
		cfg.Netlify = nc
		if err := nc.client().Validate(); err != nil {
			switch {
			case strings.HasSuffix(err.Error(), "site id"):
				errs = append(errs, "Netlify の Site ID を入力してください")
			default:
				errs = append(errs, "Netlify の Personal Access Token を入力してください")
			}
		}
		siteURL = ""
	case publishKindS3:
		sc := publishS3Config{Endpoint: strings.TrimSpace(c.FormValue("s3_endpoint")), Bucket: strings.TrimSpace(c.FormValue("s3_bucket")), Region: strings.TrimSpace(c.FormValue("s3_region")), AccessKeyID: strings.TrimSpace(c.FormValue("s3_access_key_id"))}
		if sc.Region == "" {
			sc.Region = "auto"
		}
		keep := ""
		if sc.Endpoint == saved.S3.Endpoint && sc.Bucket == saved.S3.Bucket && sc.AccessKeyID == saved.S3.AccessKeyID {
			keep = saved.S3.SecretAccessKey
		}
		sc.SecretAccessKey = secret("s3_secret_access_key", keep)
		cfg.S3 = sc
		if err := sc.client().Validate(); err != nil {
			switch {
			case strings.HasSuffix(err.Error(), "endpoint"):
				errs = append(errs, "エンドポイント URL は https://example.com の形で入力してください")
			case strings.HasSuffix(err.Error(), "bucket"):
				errs = append(errs, "バケット名を入力してください")
			case strings.HasSuffix(err.Error(), "region"):
				errs = append(errs, "リージョンは半角英小文字・数字・ハイフンで入力してください（例: auto、ap-northeast-1）")
			default:
				errs = append(errs, "アクセスキー ID とシークレットアクセスキーを入力してください")
			}
		}
		u, err := normalizeSiteURL(c.FormValue("s3_site_url"))
		if err != nil {
			errs = append(errs, err.Error())
		}
		siteURL = u
	case publishKindFolder:
		p := strings.TrimSpace(c.FormValue("folder_path"))
		if p == "" || !filepath.IsAbs(p) {
			errs = append(errs, "書き出すフォルダの場所を、ドライブやフォルダの名前から入力してください（例: "+s.defaultPublishFolder()+"）")
		} else if abs, err := s.checkPublishFolder(p); err != nil {
			errs = append(errs, describePublishError(err))
		} else {
			cfg.Folder.Path = abs
		}
		if raw := strings.TrimSpace(c.FormValue("folder_site_url")); raw != "" {
			u, err := normalizeSiteURL(raw)
			if err != nil {
				errs = append(errs, err.Error())
			}
			siteURL = u
		}
	default:
		errs = append(errs, "公開先を選んでください")
	}
	return cfg, siteURL, errs
}

// autoReflectForm は公開先のフォームの「記事公開時に自動でサイトに反映する」の値を返す。フォームは隠し欄の "0" と
// チェックボックスの "1" を送るため、"1" があればチェックあり。欄そのものが無い送信（ok が false）では、保存済みの
// 値を変えない。
func autoReflectForm(c echo.Context) (on, ok bool) {
	form, err := c.FormParams()
	if err != nil {
		return false, false
	}
	values, ok := form[publishSettingAutoReflect]
	return slices.Contains(values, "1"), ok
}

// handlePublishSave は公開先を保存する。入力に誤りがあれば、入力を残して画面を表示し直す（秘密の値は表示しない）。
// Cloudflare Pages では、公開サイトURLが未設定・この PC 内のままなら https://<project>.pages.dev にする。
// 「記事公開時に自動でサイトに反映する」も一緒に保存する。
func (s *Server) handlePublishSave(c echo.Context) error {
	saved := s.loadPublishConfig()
	cfg, siteURL, errs := s.publishForm(c, saved)
	autoReflect, hasAutoReflect := autoReflectForm(c)
	form, _ := c.FormParams()
	_, hasCDNPurgeSettings := form["cdn_purge_enabled"]
	cdnEnabled := s.getSetting("cdn_purge_enabled", "false") == "true"
	zoneID := s.getSetting("cdn_cf_zone_id", "")
	cdnToken := s.getSetting("cdn_cf_api_token", "")
	if hasCDNPurgeSettings {
		zoneID = strings.TrimSpace(c.FormValue("cdn_cf_zone_id"))
		if token := strings.TrimSpace(c.FormValue("cdn_cf_api_token")); token != "" {
			cdnToken = token
		}
		if len(zoneID) > 256 || hasControl(zoneID) || (zoneID != "" && !validCloudflareID(zoneID)) {
			errs = append(errs, "Cloudflare Zone ID は半角の英数字とハイフンで入力してください")
		}
		if len(cdnToken) > maxPublishSecretLength || hasControl(cdnToken) {
			errs = append(errs, "Cloudflare API Token の長さまたは文字を確かめてください")
		}
		cdnEnabled = zoneID != "" && cdnToken != ""
	}
	if len(errs) > 0 {
		d := s.publishPageData(c, cfg)
		d.Kind, d.Configured, d.Label, d.Errors = cfg.Kind, cfg.configured(), cfg.label(), errs
		d.PlainFTP = cfg.Kind == publishKindServer && cfg.Server.plainFTP()
		d.CDNZoneID = zoneID
		if hasCDNPurgeSettings {
			d.CDNHasToken = cdnToken != ""
		}
		switch cfg.Kind {
		case publishKindServer:
			d.Server.Host, d.Server.User, d.Server.Root = c.FormValue("host"), c.FormValue("user"), c.FormValue("root")
			d.Server.Port = c.FormValue("port")
			d.SiteURL = c.FormValue("server_site_url")
		case publishKindCloudflare:
			d.Cloudflare.AccountID, d.Cloudflare.Project = c.FormValue("cf_account_id"), c.FormValue("cf_project")
		case publishKindGitHub:
			d.GitHub.Owner, d.GitHub.Repository, d.GitHub.Branch, d.GitHub.Directory = c.FormValue("github_owner"), c.FormValue("github_repository"), c.FormValue("github_branch"), c.FormValue("github_directory")
		case publishKindNetlify:
			d.Netlify.SiteID = c.FormValue("netlify_site_id")
		case publishKindS3:
			d.S3.Endpoint, d.S3.Bucket, d.S3.Region, d.S3.AccessKeyID = c.FormValue("s3_endpoint"), c.FormValue("s3_bucket"), c.FormValue("s3_region"), c.FormValue("s3_access_key_id")
			d.SiteURL = c.FormValue("s3_site_url")
		case publishKindFolder:
			d.FolderPath = c.FormValue("folder_path")
			d.SiteURL = c.FormValue("folder_site_url")
		}
		if hasAutoReflect {
			d.AutoReflect = autoReflect
		}
		return s.renderError(c, http.StatusUnprocessableEntity, "publish", pageTitle(c, "settings_nav_publish"), "入力を確かめてください", d)
	}
	if err := s.savePublishConfig(cfg); err != nil {
		return err
	}
	if hasCDNPurgeSettings {
		if err := upsertSetting(s.db, "cdn_purge_enabled", strconv.FormatBool(cdnEnabled)); err != nil {
			return err
		}
		if err := upsertSetting(s.db, "cdn_cf_zone_id", zoneID); err != nil {
			return err
		}
		if err := upsertSetting(s.db, "cdn_cf_api_token", cdnToken); err != nil {
			return err
		}
	}
	if hasAutoReflect {
		value := "0"
		if autoReflect {
			value = "1"
		}
		if err := upsertSetting(s.db, publishSettingAutoReflect, value); err != nil {
			return err
		}
	}
	switch {
	case cfg.Kind == publishKindCloudflare:
		if err := s.fillPagesBaseURL(cfg.Cloudflare.Project, "https://"+cfg.Cloudflare.Project+".pages.dev"); err != nil {
			return err
		}
	case cfg.Kind == publishKindGitHub:
		if siteURL != "" {
			cur := s.getSetting("base_url", s.getBaseURL())
			if isCustomDomain(cur) {
				break
			}
			if err := s.saveBaseURL(siteURL); err != nil {
				return err
			}
		}
	case cfg.Kind == publishKindNetlify:
		// The Netlify site endpoint provides its canonical public URL; reflection
		// resolves it before export when no custom URL has been configured.
	case siteURL != "":
		if err := s.saveBaseURL(siteURL); err != nil {
			return err
		}
	}
	return c.Redirect(http.StatusSeeOther, s.publishPageURL("publish_saved"))
}

func isCustomDomain(urlStr string) bool {
	if len(baseURLWarnings(urlStr)) > 0 {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(urlStr))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || host == "localhost" || host == "127.0.0.1" || strings.HasSuffix(host, ".github.io") || strings.HasSuffix(host, ".pages.dev") || strings.HasSuffix(host, ".netlify.app") {
		return false
	}
	return true
}

func validCloudflareID(value string) bool {
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (s *Server) handleCDNPurgeTest(c echo.Context) error {
	zoneID := strings.TrimSpace(c.FormValue("cdn_cf_zone_id"))
	token := strings.TrimSpace(c.FormValue("cdn_cf_api_token"))
	if token == "" {
		token = s.getSetting("cdn_cf_api_token", "")
	}
	if zoneID == "" {
		zoneID = s.getSetting("cdn_cf_zone_id", "")
	}
	if len(zoneID) > 256 || !validCloudflareID(zoneID) || len(token) > maxPublishSecretLength || token == "" || hasControl(token) {
		return c.JSON(http.StatusOK, publishTestResult{Message: "Zone ID と API Token を確かめてください"})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()
	if err := purgeCDNCache(ctx, nil, token, zoneID, nil); err != nil {
		return c.JSON(http.StatusOK, publishTestResult{Message: "CDN キャッシュをパージできませんでした。" + err.Error(), Error: err.Error()})
	}
	return c.JSON(http.StatusOK, publishTestResult{OK: true, Message: "接続できました。サイト全体の CDN キャッシュをパージしました。"})
}

// publishTestResult は接続テストの結果。
type publishTestResult struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
	Error   string   `json:"error,omitempty"`
	// HostKeyChanged は SFTP の接続先の鍵が変わっていたか（画面を読み直して確認の欄を出す）。
	HostKeyChanged bool `json:"host_key_changed,omitempty"`
}

// handlePublishTest はフォームに入力した公開先（空の秘密の欄は保存済みの値）に接続できるかを確かめる。保存はしない。
// レンタルサーバーとフォルダでは、アップロード先に小さなファイルを書いて消し、書き込めることも確かめる。結果は常に 200。
func (s *Server) handlePublishTest(c echo.Context) error {
	cfg, _, errs := s.publishForm(c, s.loadPublishConfig())
	// 接続テストには公開サイトのURLは要らない。
	errs = slices.DeleteFunc(errs, func(e string) bool { return strings.Contains(e, "公開サイトのURL") })
	if len(errs) > 0 {
		return c.JSON(http.StatusOK, publishTestResult{Message: strings.Join(errs, "。")})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*publish.DefaultTimeout)
	defer cancel()
	fail := func(err error) error {
		var hk *publish.HostKeyChangedError
		return c.JSON(http.StatusOK, publishTestResult{Message: "接続できませんでした。" + describePublishError(err),
			Error: err.Error(), HostKeyChanged: errors.As(err, &hk)})
	}

	if cfg.Kind == publishKindCloudflare {
		pages := publish.NewCloudflare(cfg.Cloudflare.client())
		p, err := pages.Project(ctx)
		if errors.Is(err, publish.ErrProjectNotFound) {
			return c.JSON(http.StatusOK, publishTestResult{OK: true, Message: "接続できました。",
				Details: []string{"プロジェクト「" + cfg.Cloudflare.Project + "」はまだありません。最初に「サイトに反映」したときに作ります。"}})
		}
		if err != nil {
			return fail(err)
		}
		return c.JSON(http.StatusOK, publishTestResult{OK: true, Message: "接続できました。",
			Details: []string{"公開サイトの URL: " + p.URL()}})
	}
	if cfg.Kind == publishKindGitHub {
		return s.handleGitHubPublishTest(c, cfg.GitHub, ctx, fail)
	}
	if cfg.Kind == publishKindNetlify {
		return s.handleNetlifyPublishTest(c, cfg.Netlify, ctx, fail)
	}
	if cfg.Kind == publishKindS3 {
		client, err := publish.NewS3(cfg.S3.client())
		if err != nil {
			return fail(err)
		}
		if err := client.TestConnection(ctx); err != nil {
			return fail(err)
		}
		return c.JSON(http.StatusOK, publishTestResult{OK: true, Message: "接続できました。", Details: []string{i18n.Translate(string(language(c)), "publish_s3_connection_ok", cfg.S3.Bucket)}})
	}

	r, err := s.openPublishRemote(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	defer r.Close()
	res := publishTestResult{OK: true, Message: "接続できました。アップロード先のフォルダに書き込めます。"}
	n, err := r.CountEntries(ctx)
	if err != nil {
		return fail(err)
	}
	const probe = ".sorus-connection-test"
	if err := r.Put(ctx, probe, strings.NewReader("sorus"), 4); err != nil {
		return fail(fmt.Errorf("write test file: %w", err))
	}
	if err := r.Delete(ctx, probe); err != nil {
		return fail(fmt.Errorf("delete test file: %w", err))
	}
	if n > 0 {
		res.Details = append(res.Details, fmt.Sprintf(
			"アップロード先のフォルダには、すでに %d 件のファイル・フォルダがあります。sorus が送ったもの以外は消しません。", n))
	}
	res.Details = append(res.Details, r.Notes()...)
	return c.JSON(http.StatusOK, res)
}

// wantsJSON は fetch からの要求（応答を JSON で受け取る）かを返す。フォームの送信なら画面へリダイレクトする。
func wantsJSON(c echo.Context) bool {
	return strings.Contains(c.Request().Header.Get(echo.HeaderAccept), echo.MIMEApplicationJSON)
}

// handlePublishReflect は「サイトに反映」。反映をバックグラウンドで始め、すぐに状態を返す（反映中なら始めない）。
func (s *Server) handlePublishReflect(c echo.Context) error {
	if !s.loadPublishConfig().configured() {
		if !wantsJSON(c) {
			return c.Redirect(http.StatusSeeOther, s.publishPageURL(""))
		}
		return c.JSON(http.StatusConflict, map[string]string{"message": "公開先がまだ決まっていません。「公開先」の画面で公開先を選んでください。"})
	}
	// 公開サーバーを使っているあいだ（静的サイトは待機中）は、静的サイトへ送らない（二重公開を防ぐ。publish_method.go）。
	if !s.staticPublishActive() {
		if !wantsJSON(c) {
			return c.Redirect(http.StatusSeeOther, s.publishPageURL("publish_static_standby"))
		}
		return c.JSON(http.StatusConflict, map[string]string{"message": i18n.Translate(string(language(c)), "publish_static_standby")})
	}
	st := s.publishState()
	st.mu.Lock()
	st.overdueAsked = true
	st.mu.Unlock()
	s.startReflect(publishTriggerManual)
	if !wantsJSON(c) {
		return c.Redirect(http.StatusSeeOther, s.publishPageURL(""))
	}
	return c.JSON(http.StatusAccepted, s.currentPublishStatus())
}

func (s *Server) handlePublishStatus(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, s.currentPublishStatus())
}

func (s *Server) handlePublishHealthCheck(c echo.Context) error {
	if !s.loadPublishConfig().configured() {
		if !wantsJSON(c) {
			return c.Redirect(http.StatusSeeOther, s.publishPageURL(""))
		}
		return c.JSON(http.StatusConflict, map[string]string{"message": "公開先を設定してから確認してください。"})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()
	status := s.checkPublishHealth(ctx)
	if wantsJSON(c) {
		return c.JSON(http.StatusOK, status)
	}
	return c.Redirect(http.StatusSeeOther, s.publishPageURL(""))
}

// publishPanelData は publish_panel.html（ダッシュボードの「サイトに反映」）に渡すデータ。
type publishPanelData struct {
	CSRFToken string
	// Configured は公開先が決まっているか。handlePublishPanel は決まっているときだけ描くため、常に true。
	Configured bool
	Label      string
	// Overdue はデスクトップモードで起動する前に予約時刻を過ぎていた記事の数。1回だけ尋ねる。
	Overdue int
	Status  publishStatus
	// Lang は管理画面の表示言語。
	Lang string
}

// handlePublishPanel はダッシュボードに読み込む「サイトに反映」の部分を返す。公開先が未設定なら何も出さない
// （204。ダッシュボードの枠は空のまま隠れる）。公開先は「設定 ＞ 公開先」から決める
// （desktop_quit_and_editor_ux_spec.md 1.2）。
func (s *Server) handlePublishPanel(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	cfg := s.loadPublishConfig()
	// 公開先が未設定のときと、静的サイトが待機中（公開サーバーを使っている）のときは出さない。
	if !cfg.configured() || !s.staticPublishActive() {
		return c.NoContent(http.StatusNoContent)
	}
	d := publishPanelData{
		CSRFToken: csrfToken(c), Configured: true, Label: cfg.label(),
		Status: s.currentPublishStatus(), Lang: string(language(c)),
	}
	st := s.publishState()
	st.mu.Lock()
	if st.overdue > 0 && !st.overdueAsked && !st.running {
		d.Overdue, st.overdueAsked = st.overdue, true
	}
	st.mu.Unlock()
	// ?compact=1 はダッシュボード下段の1行の状態（正常に動いているとき）。確認が要るときは compact でも
	// 普通の大きさで出す（予約時刻を過ぎた記事の確認は1回だけ尋ねるため、取りこぼさない）。
	name := "publish_panel"
	if c.QueryParam("compact") != "" && d.Overdue == 0 && !publishFailed(d.Status) {
		name = "publish_panel_compact"
	}
	var buf bytes.Buffer
	if err := publishPanelTemplate.ExecuteTemplate(&buf, name, d); err != nil {
		return err
	}
	return c.HTMLBlob(http.StatusOK, buf.Bytes())
}

// publishFailed は最後の反映がうまくいかず（置き換えの確認・公開サイトURLの入力待ちを含む）、今は反映していないか。
func publishFailed(st publishStatus) bool {
	return !st.Running && st.Last != nil && !st.Last.OK
}

// publishNeedsAttention はダッシュボードのファーストビュー上部に「サイトに反映」の警告を出すか。最後の反映が
// 失敗、または予約時刻を過ぎた記事をまだ尋ねていないときだけ true（ふだんは下段に出す）。公開先が未設定なだけでは
// 上部に出さない（設定は公開の設定画面に委ね、ダッシュボードには何も出さない。handlePublishPanel）。
// 予約時刻を過ぎた記事は覗くだけで、尋ねた印（overdueAsked）は handlePublishPanel が付ける。
func (s *Server) publishNeedsAttention() bool {
	if publishFailed(s.currentPublishStatus()) {
		return true
	}
	st := s.publishState()
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.overdue > 0 && !st.overdueAsked && !st.running
}

// handlePublishZip は今のサイトを書き出して Zip でダウンロードさせる（公開先の設定は使わない）。
func (s *Server) handlePublishZip(c echo.Context) error {
	extendDeadlines(c)
	s.backupMu.RLock()
	dir, _, err := s.ExportToTemp(c.Request().Context(), ExportOptions{})
	s.backupMu.RUnlock()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	name := "site-" + time.Now().In(jst).Format("20060102-1504") + ".zip"
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, "application/zip")
	h.Set(echo.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	h.Set("Cache-Control", "no-store")
	c.Response().WriteHeader(http.StatusOK)
	return publish.WriteZip(c.Response(), dir)
}

// handlePublishTrustHostKey は確認待ちの SFTP の接続先の新しい鍵を信頼し、以後はその鍵と比べる。
func (s *Server) handlePublishTrustHostKey(c echo.Context) error {
	st := s.publishState()
	st.mu.Lock()
	pending := st.hostKey
	st.mu.Unlock()
	// 画面に出した鍵と違う鍵を信頼しないよう、フォームの指紋と照らし合わせる。
	if pending == nil || c.FormValue("fingerprint") != pending.Got {
		return echo.NewHTTPError(http.StatusConflict, "確認を待っている接続先の鍵がありません")
	}
	if err := s.rememberHostKey(pending.Addr, pending.Got); err != nil {
		return err
	}
	st.mu.Lock()
	if st.hostKey == pending {
		st.hostKey = nil
	}
	st.mu.Unlock()
	return c.Redirect(http.StatusSeeOther, s.publishPageURL("publish_trusted"))
}

// handlePublishReplaceSite は「置き換える」。Cloudflare Pages・Netlify の公開中のサイト（フォルダでは、すでにある
// トップページ）を sorus のサイトで置き換えることを、尋ねていた公開先への次の1回の反映だけで認め、反映を始める。
// 尋ねていなければ何もしない。
func (s *Server) handlePublishReplaceSite(c echo.Context) error {
	id := s.loadPublishConfig().destinationID()
	st := s.publishState()
	st.mu.Lock()
	asked := st.liveSite != "" && st.liveSite == id
	if asked {
		st.liveSite, st.replaceLive = "", id
	}
	st.mu.Unlock()
	if !asked {
		const msg = "置き換えるかを尋ねている公開先がありません。もう一度「サイトに反映」を押してください。"
		if !wantsJSON(c) {
			return echo.NewHTTPError(http.StatusConflict, msg)
		}
		return c.JSON(http.StatusConflict, map[string]string{"message": msg})
	}
	s.startReflect(publishTriggerManual)
	if !wantsJSON(c) {
		return c.Redirect(http.StatusSeeOther, s.publishPageURL(""))
	}
	return c.JSON(http.StatusAccepted, s.currentPublishStatus())
}
