package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Cloudflare Pages の Direct Upload（API でのアップロード）。Cloudflare の API リファレンス（Pages の assets と
// deployments）と wrangler の pages deploy と同じ手順で送る。
//
//  1. GET  /accounts/{account}/pages/projects/{project}/upload-token（API トークン）で、アップロード用の JWT を受け取る。
//  2. POST /pages/assets/check-missing（JWT）で、Cloudflare にまだ無いファイル（内容のハッシュ）を尋ねる。
//  3. POST /pages/assets/upload（JWT）で、無いファイルだけを Base64 の JSON にまとめて送る（まとまりごとに上限あり）。
//  4. POST /pages/assets/upsert-hashes（JWT）で、今回使うすべてのハッシュを登録する。
//  5. POST /accounts/{account}/pages/projects/{project}/deployments（API トークン）に、パスとハッシュの対応
//     （manifest）を multipart で送って新しい版を公開する。前の版にあって今回の manifest に無いファイルは公開されなくなる。
//
// そのため初めて反映するときは、先に GET /accounts/{account}/pages/projects/{project} で本番の版
// （canonical_deployment）があるかを確かめ、あれば利用者が置き換えを選ぶまで送らない（ErrLiveSite）。

// DefaultCloudflareAPI は Cloudflare API の起点。
const DefaultCloudflareAPI = "https://api.cloudflare.com/client/v4"

const (
	// cfMaxFileSize は Pages に置ける1ファイルの上限（25MiB）。
	cfMaxFileSize = 25 << 20
	// cfMaxFiles は1つの版に置けるファイル数の上限。
	cfMaxFiles = 20000
	// cfMaxBucketSize・cfMaxBucketFiles は1回の upload で送るファイルの合計バイト数と数の上限（wrangler と同じ）。
	cfMaxBucketSize  = 40 << 20
	cfMaxBucketFiles = 2000
	// cfMaxResponseBytes は API の応答として読む上限。
	cfMaxResponseBytes = 8 << 20
	// cfTimeout は1回の API の要求の制限時間。まとまった画像を送る upload に間に合う長さにする。
	cfTimeout = 5 * time.Minute
)

var (
	// ErrProjectNotFound は Cloudflare Pages のプロジェクトが無いこと。
	ErrProjectNotFound = errors.New("publish: cloudflare pages project not found")
	// ErrFileTooLarge は Cloudflare Pages に置けない大きさのファイルがあること。
	ErrFileTooLarge = errors.New("publish: file too large for cloudflare pages")
	// ErrTooManyFiles は Cloudflare Pages の1つの版に置けるファイル数を超えたこと。
	ErrTooManyFiles = errors.New("publish: too many files for cloudflare pages")
	// ErrLiveSite は、初めて反映するプロジェクトに、すでに公開中のサイト（本番の版）があること。Netlify のサイトと、
	// トップページ（index.html）がすでにあるフォルダ（CheckFolderTopPage）でも使う。
	ErrLiveSite = errors.New("publish: cloudflare pages project already has a live site")

	cfAccountIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	cfProjectPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,57}$`)
)

// CloudflareConfig は Cloudflare Pages の公開先の設定。
type CloudflareConfig struct {
	// AccountID は Cloudflare のアカウント ID（32文字の英数字）。
	AccountID string
	// APIToken は「Cloudflare Pages: 編集」の権限を持つ API トークン。
	APIToken string
	// Project は Pages のプロジェクト名（英小文字・数字・ハイフン）。公開 URL は https://<project>.pages.dev になる。
	Project string
	// API は API の起点。空なら DefaultCloudflareAPI（テストで差し替える）。
	API string
	// HTTPClient は API を呼ぶクライアント。nil ならリダイレクトを追わない既定のクライアント。
	HTTPClient *http.Client
	// ReplaceLiveSite は、初めての反映で公開中のサイトがあっても置き換えるか（利用者が「置き換える」を選んだとき）。
	ReplaceLiveSite bool
}

// ErrInvalidCloudflareConfig は Cloudflare Pages の設定の形が正しくないこと。
var ErrInvalidCloudflareConfig = errors.New("publish: invalid cloudflare pages config")

// Validate は AccountID・Project の形と APIToken の有無を確かめる。
func (c CloudflareConfig) Validate() error {
	switch {
	case !cfAccountIDPattern.MatchString(c.AccountID):
		return fmt.Errorf("%w: account id", ErrInvalidCloudflareConfig)
	case !cfProjectPattern.MatchString(c.Project):
		return fmt.Errorf("%w: project", ErrInvalidCloudflareConfig)
	case strings.TrimSpace(c.APIToken) == "":
		return fmt.Errorf("%w: api token", ErrInvalidCloudflareConfig)
	}
	return nil
}

// CloudflareProject は Pages のプロジェクト。
type CloudflareProject struct {
	Name string `json:"name"`
	// Subdomain は公開 URL のホスト名（例: my-site.pages.dev）。プロジェクト名が使われていると末尾に文字が付く。
	Subdomain        string `json:"subdomain"`
	ProductionBranch string `json:"production_branch"`
	// CanonicalDeployment は今の本番の版（公開中のサイト）。まだ版が無ければ nil。
	CanonicalDeployment *CloudflareDeployment `json:"canonical_deployment"`
}

// CloudflareDeployment は Pages のプロジェクトの1つの版。
type CloudflareDeployment struct {
	ID          string `json:"id"`
	Environment string `json:"environment"`
	// URL はこの版だけを見られる URL（例: https://abc123.my-site.pages.dev）。
	URL string `json:"url"`
}

// URL はプロジェクトの公開 URL（https://<subdomain>）。
func (p *CloudflareProject) URL() string {
	if p.Subdomain == "" {
		return "https://" + p.Name + ".pages.dev"
	}
	return "https://" + p.Subdomain
}

// CloudflareError は Cloudflare API が失敗を返したこと。
type CloudflareError struct {
	// Status は HTTP の状態コード。Code・Message は API が返した最初のエラー。
	Status  int
	Code    int
	Message string
}

func (e *CloudflareError) Error() string {
	return fmt.Sprintf("publish: cloudflare api: status %d: %d %s", e.Status, e.Code, e.Message)
}

// Is は認証の失敗（401・403）を ErrAuth として扱う。
func (e *CloudflareError) Is(target error) bool {
	return target == ErrAuth && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

// Cloudflare は Cloudflare Pages の API クライアント。
type Cloudflare struct {
	cfg  CloudflareConfig
	http *http.Client
}

// NewCloudflare は cfg の API クライアントを返す。
func NewCloudflare(cfg CloudflareConfig) *Cloudflare {
	if cfg.API == "" {
		cfg.API = DefaultCloudflareAPI
	}
	cfg.API = strings.TrimRight(cfg.API, "/")
	cfg.APIToken = strings.TrimSpace(cfg.APIToken)
	hc := cfg.HTTPClient
	if hc == nil {
		// リダイレクトを追うと、別のホストへトークンを送るおそれがあるため追わない。
		hc = &http.Client{Timeout: cfTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Cloudflare{cfg: cfg, http: hc}
}

func (c *Cloudflare) projectPath() string {
	return "/accounts/" + url.PathEscape(c.cfg.AccountID) + "/pages/projects/" + url.PathEscape(c.cfg.Project)
}

// Project はプロジェクトを返す。無ければ ErrProjectNotFound。
func (c *Cloudflare) Project(ctx context.Context) (*CloudflareProject, error) {
	var p CloudflareProject
	err := c.call(ctx, http.MethodGet, c.projectPath(), c.cfg.APIToken, "", nil, &p)
	var cerr *CloudflareError
	if errors.As(err, &cerr) && cerr.Status == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %v", ErrProjectNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateProject は Direct Upload で使うプロジェクトを作る（本番のブランチは main）。
func (c *Cloudflare) CreateProject(ctx context.Context) (*CloudflareProject, error) {
	body, err := json.Marshal(map[string]string{"name": c.cfg.Project, "production_branch": "main"})
	if err != nil {
		return nil, err
	}
	var p CloudflareProject
	path := "/accounts/" + url.PathEscape(c.cfg.AccountID) + "/pages/projects"
	if err := c.call(ctx, http.MethodPost, path, c.cfg.APIToken, "application/json", bytes.NewReader(body), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// cfAsset は1つの版に置くファイル。
type cfAsset struct {
	name string
	key  string
	size int64
}

// Deploy は dir の書き出しを新しい版として公開する。next は dir の Manifest、prev は前回の反映に成功したときの
// Manifest（件数の計算と、初めての反映かの判定に使う）。Cloudflare にまだ無いファイルだけを送る。
// 初めての反映（prev が nil）で、プロジェクトにすでに公開中のサイトがあれば、何も送らずに ErrLiveSite を返す
// （cfg.ReplaceLiveSite なら置き換える）。
func (c *Cloudflare) Deploy(ctx context.Context, dir string, prev, next Manifest, progress func(Progress)) (Result, error) {
	res := Result{FirstTime: prev == nil}
	if len(next) > cfMaxFiles {
		return res, fmt.Errorf("%w: %d files", ErrTooManyFiles, len(next))
	}
	assets := make([]cfAsset, 0, len(next))
	for name, f := range next {
		rel, err := CleanRel(name)
		if err != nil {
			return res, err
		}
		if f.Size > cfMaxFileSize {
			return res, fmt.Errorf("%w: %s", ErrFileTooLarge, rel)
		}
		assets = append(assets, cfAsset{name: rel, key: cfAssetKey(rel, f), size: f.Size})
	}
	slices.SortFunc(assets, func(a, b cfAsset) int { return strings.Compare(a.name, b.name) })
	// 初めての反映では、sorus が送ったことのないファイルも今回の manifest に無ければ公開されなくなる。Cloudflare の
	// API では版のファイルの一覧を読めないため、本番の版があれば公開中のサイトがあるとみなして尋ねる。
	if prev == nil && !c.cfg.ReplaceLiveSite {
		p, err := c.Project(ctx)
		if err != nil {
			return res, err
		}
		if p.CanonicalDeployment != nil {
			return res, fmt.Errorf("%w: production deployment %s", ErrLiveSite, p.CanonicalDeployment.ID)
		}
	}
	keys := make([]string, 0, len(assets))
	seen := map[string]bool{}
	for _, a := range assets {
		if !seen[a.key] {
			seen[a.key] = true
			keys = append(keys, a.key)
		}
	}

	jwt, err := c.uploadToken(ctx)
	if err != nil {
		return res, err
	}
	// withJWT は JWT の期限が切れて 401 になったとき、取り直して1回だけやり直す。
	withJWT := func(f func(jwt string) error) error {
		err := f(jwt)
		var cerr *CloudflareError
		if errors.As(err, &cerr) && cerr.Status == http.StatusUnauthorized {
			if jwt, err = c.uploadToken(ctx); err != nil {
				return err
			}
			err = f(jwt)
		}
		return err
	}

	var missing []string
	if err := withJWT(func(jwt string) error {
		missing = nil
		return c.postJSON(ctx, "/pages/assets/check-missing", jwt, map[string]any{"hashes": keys}, &missing)
	}); err != nil {
		return res, err
	}
	need := map[string]bool{}
	for _, k := range missing {
		need[k] = true
	}
	var upload []cfAsset
	for _, a := range assets {
		if need[a.key] {
			upload = append(upload, a)
			delete(need, a.key)
		}
	}

	total := len(upload)
	done := 0
	report := func(p string) {
		if progress != nil {
			progress(Progress{Done: done, Total: total, Path: p})
		}
	}
	for start := 0; start < len(upload); {
		end, size := start, int64(0)
		for end < len(upload) && end-start < cfMaxBucketFiles && (end == start || size+upload[end].size <= cfMaxBucketSize) {
			size += upload[end].size
			end++
		}
		bucket := upload[start:end]
		report(bucket[0].name)
		payload, err := cfUploadPayload(dir, bucket)
		if err != nil {
			return res, err
		}
		if err := withJWT(func(jwt string) error {
			return c.call(ctx, http.MethodPost, "/pages/assets/upload", jwt, "application/json", bytes.NewReader(payload), nil)
		}); err != nil {
			return res, err
		}
		done += len(bucket)
		start = end
	}
	if err := withJWT(func(jwt string) error {
		return c.postJSON(ctx, "/pages/assets/upsert-hashes", jwt, map[string]any{"hashes": keys}, nil)
	}); err != nil {
		// 次回の check-missing で送り直すだけで、今回の版の公開には影響しない（wrangler と同じ扱い）。
		res.Notes = append(res.Notes, "Cloudflare への記録の一部に失敗しました。次の反映では、変わっていないファイルも送り直すことがあります。")
	}

	manifest := make(map[string]string, len(assets))
	for _, a := range assets {
		manifest["/"+a.name] = a.key
	}
	depURL, err := c.createDeployment(ctx, manifest)
	if err != nil {
		return res, err
	}
	res.URL = depURL
	res.Uploaded = len(upload)
	res.Unchanged = len(next) - len(upload)
	for name := range prev {
		if _, ok := next[name]; !ok {
			res.Deleted++
		}
	}
	report("")
	return res, nil
}

// cfAssetKey は Pages の asset store でのファイルのキー（32文字の16進数）。同じ内容でも拡張子で Content-Type が
// 変わるため、拡張子も含めて決める。Content-Type を cfFixedTypes で決める拡張子は、その Content-Type もキーに含める
// （以前に違う Content-Type で送った同じ内容のファイルを、Cloudflare にあるものとみなさず送り直す）。
func cfAssetKey(name string, f File) string {
	seed := f.SHA256 + ":" + strings.TrimPrefix(path.Ext(name), ".")
	if t, ok := cfFixedTypes[strings.ToLower(path.Ext(name))]; ok {
		seed += ":" + t
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:32]
}

// cfUploadPayload は bucket のファイルを /pages/assets/upload の JSON（KV の一括書き込みと同じ形）にする。
func cfUploadPayload(dir string, bucket []cfAsset) ([]byte, error) {
	type metadata struct {
		ContentType string `json:"contentType"`
	}
	type item struct {
		Key      string   `json:"key"`
		Value    string   `json:"value"`
		Metadata metadata `json:"metadata"`
		Base64   bool     `json:"base64"`
	}
	items := make([]item, 0, len(bucket))
	for _, a := range bucket {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(a.name)))
		if err != nil {
			return nil, fmt.Errorf("publish: read %s: %w", a.name, err)
		}
		items = append(items, item{Key: a.key, Value: base64.StdEncoding.EncodeToString(data),
			Metadata: metadata{ContentType: contentType(a.name)}, Base64: true})
	}
	return json.Marshal(items)
}

// cfFixedTypes は、送るパソコンの MIME の設定（/etc/mime.types・Windows のレジストリ）によらずに決める Content-Type。
// Go の標準の表には .txt が無いため、設定の無い環境では書き出しの llms.txt・llms-full.txt・robots.txt が
// application/octet-stream で公開され、ブラウザではダウンロードになり、AI のクローラーや検索エンジンが文字として読めない。
var cfFixedTypes = map[string]string{
	".txt": "text/plain; charset=utf-8",
}

// contentType はファイル名の拡張子から決めた Content-Type。分からなければ application/octet-stream。
func contentType(name string) string {
	if t, ok := cfFixedTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (c *Cloudflare) uploadToken(ctx context.Context) (string, error) {
	var tok struct {
		JWT string `json:"jwt"`
	}
	if err := c.call(ctx, http.MethodGet, c.projectPath()+"/upload-token", c.cfg.APIToken, "", nil, &tok); err != nil {
		return "", err
	}
	if tok.JWT == "" {
		return "", errors.New("publish: cloudflare api: empty upload token")
	}
	return tok.JWT, nil
}

// createDeployment は manifest の版を作り、その版の URL を返す。
func (c *Cloudflare) createDeployment(ctx context.Context, manifest map[string]string) (string, error) {
	m, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("manifest", string(m)); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	var dep struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, http.MethodPost, c.projectPath()+"/deployments", c.cfg.APIToken, mw.FormDataContentType(), &body, &dep); err != nil {
		return "", err
	}
	return dep.URL, nil
}

func (c *Cloudflare) postJSON(ctx context.Context, path, bearer string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, path, bearer, "application/json", bytes.NewReader(body), out)
}

// call は API を呼び、応答の result を out に読み込む（out が nil なら読まない）。
func (c *Cloudflare) call(ctx context.Context, method, path, bearer, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("publish: cloudflare api %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, cfMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("publish: cloudflare api %s: read response: %w", path, err)
	}
	var env struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
	}
	jerr := json.Unmarshal(data, &env)
	if resp.StatusCode/100 != 2 || jerr != nil || !env.Success {
		e := &CloudflareError{Status: resp.StatusCode}
		if len(env.Errors) > 0 {
			e.Code, e.Message = env.Errors[0].Code, env.Errors[0].Message
		} else if jerr != nil {
			e.Message = "unexpected response"
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("publish: cloudflare api %s: decode result: %w", path, err)
	}
	return nil
}
