package publish

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Cloudflare Workers の静的アセット（Static Assets）の Direct Upload。Cloudflare の「Direct Uploads」の手順と
// wrangler deploy（Worker のスクリプトを持たない、アセットだけの Worker）と同じ形で送る。Node.js も wrangler も使わない。
//
//  1. POST /accounts/{account}/workers/scripts/{script}/assets-upload-session（API トークン）に、パスごとの
//     ハッシュと大きさ（manifest）を送る。アップロード用の JWT と、Cloudflare にまだ無いハッシュのまとまり（buckets）が返る。
//     buckets が空なら、JWT がそのまま完了の JWT になる。
//  2. POST /accounts/{account}/workers/assets/upload?base64=true（アップロード用の JWT）に、まとまりごとに
//     multipart で送る（部分の名前とファイル名はハッシュ、中身は Base64、Content-Type は公開するときの Content-Type）。
//     最後のまとまりの応答に、完了の JWT が入る。
//  3. PUT /accounts/{account}/workers/scripts/{script}（API トークン）に、完了の JWT とアセットの配信の設定だけを
//     書いた metadata を multipart で送り、新しい版として公開する（スクリプトの部分は送らない）。
//     今回の manifest に無いファイルは公開されなくなる。
//  4. 初めての反映では POST .../workers/scripts/{script}/subdomain で workers.dev での公開を有効にし、
//     GET /accounts/{account}/workers/subdomain のアカウントのサブドメインから公開 URL
//     （https://<script>.<subdomain>.workers.dev）を決める。
//
// 初めて反映するときは、先に GET .../workers/scripts/{script}/settings で Worker がもうあるかを確かめ、あれば
// 利用者が置き換えを選ぶまで送らない（ErrLiveSite）。
//
// API の呼び出し（認証・リダイレクトを追わない・応答の読み取り・CloudflareError）、アカウント ID の形、アセットの
// キー（cfAssetKey）、Content-Type（contentType。.txt は text/plain に固定）、1ファイルの上限は Cloudflare Pages
// （cloudflare.go）と共通のものを使う。

const (
	// cfwMaxFiles は1つの版に置けるアセットの数の上限（有料プラン。無料プランは2万件で、超えると API が断る）。
	cfwMaxFiles = 100000
	// cfwCompatibilityDate は Worker の互換性の日付。スクリプトを持たないため、配信の動きには影響しない。
	cfwCompatibilityDate = "2025-09-01"
	// cfwHeadersFile・cfwRedirectsFile は書き出しのいちばん上に置く、配信の設定のファイル。アセットとしては送らず、
	// 中身を metadata の assets.config に入れる（wrangler と同じ扱い。送ると設定にならず、ファイルとして公開される）。
	cfwHeadersFile   = "_headers"
	cfwRedirectsFile = "_redirects"
	// cfwNotFoundPage があれば、無いページへのアクセスにそのページを 404 で返す。
	cfwNotFoundPage = "404.html"
)

var (
	// ErrInvalidCloudflareWorkersConfig は Cloudflare Workers の設定の形が正しくないこと。
	ErrInvalidCloudflareWorkersConfig = errors.New("publish: invalid cloudflare workers config")
	// ErrWorkersSubdomainNotFound は、Cloudflare のアカウントに workers.dev のサブドメインがまだ無いこと
	// （Cloudflare の画面で「Workers & Pages」を初めて開いたときに決める）。
	ErrWorkersSubdomainNotFound = errors.New("publish: cloudflare workers.dev subdomain not registered")

	// cfwScriptPattern は Worker の名前。workers.dev のホスト名の一部になるため、英小文字・数字・ハイフンで63文字まで、
	// ハイフンで始まらず終わらない。
	cfwScriptPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

// cfwNotFoundCode は、Cloudflare が「Worker が無い」「workers.dev のサブドメインが無い」ときに返すエラーコード。
const cfwNotFoundCode = 10007

// CloudflareWorkersConfig は Cloudflare Workers（Static Assets）の公開先の設定。
type CloudflareWorkersConfig struct {
	// AccountID は Cloudflare のアカウント ID（32文字の英数字）。
	AccountID string
	// APIToken は「Workers スクリプト: 編集」の権限を持つ API トークン。
	APIToken string
	// Script は Worker の名前（英小文字・数字・ハイフン）。公開 URL は https://<script>.<subdomain>.workers.dev になる。
	Script string
	// API は API の起点。空なら DefaultCloudflareAPI（テストで差し替える）。
	API string
	// HTTPClient は API を呼ぶクライアント。nil ならリダイレクトを追わない既定のクライアント。
	HTTPClient *http.Client
	// ReplaceLiveSite は、初めての反映で同じ名前の Worker がすでにあっても置き換えるか（利用者が「置き換える」を選んだとき）。
	ReplaceLiveSite bool
}

// Validate は AccountID・Script の形と APIToken の有無を確かめる。
func (c CloudflareWorkersConfig) Validate() error {
	switch {
	case !cfAccountIDPattern.MatchString(c.AccountID):
		return fmt.Errorf("%w: account id", ErrInvalidCloudflareWorkersConfig)
	case !cfwScriptPattern.MatchString(c.Script):
		return fmt.Errorf("%w: script", ErrInvalidCloudflareWorkersConfig)
	case strings.TrimSpace(c.APIToken) == "":
		return fmt.Errorf("%w: api token", ErrInvalidCloudflareWorkersConfig)
	}
	return nil
}

// CloudflareWorkers は Cloudflare Workers（Static Assets）の API クライアント。
type CloudflareWorkers struct {
	cfg CloudflareWorkersConfig
	// api は API を呼ぶ部分。Cloudflare Pages のクライアントと同じ呼び出し（call・postJSON）を使う。
	api *Cloudflare
}

// NewCloudflareWorkers は cfg の API クライアントを返す。
func NewCloudflareWorkers(cfg CloudflareWorkersConfig) *CloudflareWorkers {
	api := NewCloudflare(CloudflareConfig{AccountID: cfg.AccountID, APIToken: cfg.APIToken, API: cfg.API, HTTPClient: cfg.HTTPClient})
	cfg.API, cfg.APIToken = api.cfg.API, api.cfg.APIToken
	return &CloudflareWorkers{cfg: cfg, api: api}
}

func (w *CloudflareWorkers) accountPath() string {
	return "/accounts/" + url.PathEscape(w.cfg.AccountID)
}

func (w *CloudflareWorkers) scriptPath() string {
	return w.accountPath() + "/workers/scripts/" + url.PathEscape(w.cfg.Script)
}

// cfwNotFound は、API が「無い」と答えたか（404 またはエラーコード 10007）。
func cfwNotFound(err error) bool {
	var cerr *CloudflareError
	return errors.As(err, &cerr) && (cerr.Status == http.StatusNotFound || cerr.Code == cfwNotFoundCode)
}

// ScriptExists は同じ名前の Worker がもうあるかを返す。
func (w *CloudflareWorkers) ScriptExists(ctx context.Context) (bool, error) {
	err := w.api.call(ctx, http.MethodGet, w.scriptPath()+"/settings", w.cfg.APIToken, "", nil, nil)
	if cfwNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Subdomain はアカウントの workers.dev のサブドメイン（例: my-account）を返す。まだ無ければ ErrWorkersSubdomainNotFound。
func (w *CloudflareWorkers) Subdomain(ctx context.Context) (string, error) {
	var sub struct {
		Subdomain string `json:"subdomain"`
	}
	err := w.api.call(ctx, http.MethodGet, w.accountPath()+"/workers/subdomain", w.cfg.APIToken, "", nil, &sub)
	if cfwNotFound(err) {
		return "", fmt.Errorf("%w: %v", ErrWorkersSubdomainNotFound, err)
	}
	if err != nil {
		return "", err
	}
	if sub.Subdomain == "" {
		return "", ErrWorkersSubdomainNotFound
	}
	return sub.Subdomain, nil
}

// URL は Worker の workers.dev での公開 URL（https://<script>.<subdomain>.workers.dev）。
func (w *CloudflareWorkers) URL(ctx context.Context) (string, error) {
	sub, err := w.Subdomain(ctx)
	if err != nil {
		return "", err
	}
	return "https://" + w.cfg.Script + "." + sub + ".workers.dev", nil
}

// EnableWorkersDev は Worker を workers.dev で公開する（API で作った Worker は、そのままでは workers.dev で開けない）。
func (w *CloudflareWorkers) EnableWorkersDev(ctx context.Context) error {
	return w.api.postJSON(ctx, w.scriptPath()+"/subdomain", w.cfg.APIToken, map[string]bool{"enabled": true}, nil)
}

// cfwManifestEntry は assets-upload-session に送る1つのファイル。
type cfwManifestEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// cfwUploadSession は assets-upload-session の応答。
type cfwUploadSession struct {
	JWT     string     `json:"jwt"`
	Buckets [][]string `json:"buckets"`
}

// Deploy は dir の書き出しを Worker の新しい版として公開する。next は dir の Manifest、prev は前回の反映に成功したときの
// Manifest（件数の計算と、初めての反映かの判定に使う）。Cloudflare にまだ無いファイルだけを送る。
// 初めての反映（prev が nil）で、同じ名前の Worker がすでにあれば、何も送らずに ErrLiveSite を返す
// （cfg.ReplaceLiveSite なら置き換える）。
func (w *CloudflareWorkers) Deploy(ctx context.Context, dir string, prev, next Manifest, progress func(Progress)) (Result, error) {
	res := Result{FirstTime: prev == nil}
	assets := make([]cfAsset, 0, len(next))
	assetConfig := map[string]string{"html_handling": "auto-trailing-slash", "not_found_handling": "none"}
	for name, f := range next {
		rel, err := CleanRel(name)
		if err != nil {
			return res, err
		}
		if rel == cfwHeadersFile || rel == cfwRedirectsFile {
			data, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil {
				return res, fmt.Errorf("publish: read %s: %w", rel, err)
			}
			assetConfig[rel] = string(data)
			continue
		}
		if rel == cfwNotFoundPage {
			assetConfig["not_found_handling"] = "404-page"
		}
		if f.Size > cfMaxFileSize {
			return res, fmt.Errorf("%w: %s", ErrFileTooLarge, rel)
		}
		assets = append(assets, cfAsset{name: rel, key: cfAssetKey(rel, f), size: f.Size})
	}
	if len(assets) > cfwMaxFiles {
		return res, fmt.Errorf("%w: %d files", ErrTooManyFiles, len(assets))
	}
	slices.SortFunc(assets, func(a, b cfAsset) int { return strings.Compare(a.name, b.name) })
	if prev == nil && !w.cfg.ReplaceLiveSite {
		exists, err := w.ScriptExists(ctx)
		if err != nil {
			return res, err
		}
		if exists {
			return res, fmt.Errorf("%w: worker %s", ErrLiveSite, w.cfg.Script)
		}
	}

	manifest := make(map[string]cfwManifestEntry, len(assets))
	byKey := make(map[string]cfAsset, len(assets))
	for _, a := range assets {
		manifest["/"+a.name] = cfwManifestEntry{Hash: a.key, Size: a.size}
		if _, ok := byKey[a.key]; !ok {
			byKey[a.key] = a
		}
	}

	uploaded := map[string]bool{}
	var completion string
	// アップロード用の JWT（1時間有効）が途中で切れて 401 になったら、もう一度だけ manifest を送り直して続ける
	// （送り終えたファイルは buckets に入らない）。
	for attempt := 0; ; attempt++ {
		var err error
		completion, err = w.uploadAssets(ctx, dir, manifest, byKey, uploaded, progress)
		var cerr *CloudflareError
		if attempt == 0 && errors.As(err, &cerr) && cerr.Status == http.StatusUnauthorized && errors.Is(err, errUploadJWT) {
			continue
		}
		if err != nil {
			return res, err
		}
		break
	}

	if err := w.putScript(ctx, completion, assetConfig); err != nil {
		return res, err
	}
	if prev == nil {
		if err := w.EnableWorkersDev(ctx); err != nil {
			res.Notes = append(res.Notes, "Worker を workers.dev で公開する設定にできませんでした。Cloudflare の画面で、この Worker の「設定」→「ドメインとルート」から workers.dev を有効にしてください。")
		}
	}
	if u, err := w.URL(ctx); err == nil {
		res.URL = u
	} else if errors.Is(err, ErrWorkersSubdomainNotFound) {
		res.Notes = append(res.Notes, "Cloudflare のアカウントに workers.dev のサブドメインがまだ無いため、workers.dev の URL では開けません。Cloudflare の画面で「Workers & Pages」を開いてサブドメインを決めるか、独自ドメインを設定してください。")
	}
	res.Uploaded = len(uploaded)
	res.Unchanged = len(next) - len(uploaded)
	for name := range prev {
		if _, ok := next[name]; !ok {
			res.Deleted++
		}
	}
	if progress != nil {
		progress(Progress{Done: len(uploaded), Total: len(uploaded)})
	}
	return res, nil
}

// errUploadJWT は、アップロード用の JWT で送った要求の失敗に付ける印（API トークンの失敗と分けて、送り直すかを決める）。
var errUploadJWT = errors.New("publish: cloudflare workers asset upload")

// uploadAssets は manifest で upload session を始め、Cloudflare にまだ無いファイルを送って、完了の JWT を返す。
// 送ったファイルのキーを uploaded に記録する。アップロード用の JWT での失敗は errUploadJWT も付けて返す。
func (w *CloudflareWorkers) uploadAssets(ctx context.Context, dir string, manifest map[string]cfwManifestEntry,
	byKey map[string]cfAsset, uploaded map[string]bool, progress func(Progress)) (string, error) {
	var sess cfwUploadSession
	if err := w.api.postJSON(ctx, w.scriptPath()+"/assets-upload-session", w.cfg.APIToken, map[string]any{"manifest": manifest}, &sess); err != nil {
		return "", err
	}
	if sess.JWT == "" {
		return "", errors.New("publish: cloudflare api: empty assets upload token")
	}
	total := len(uploaded)
	for _, bucket := range sess.Buckets {
		total += len(bucket)
	}
	completion := ""
	if len(sess.Buckets) == 0 {
		// まだ無いファイルが1つも無ければ、受け取った JWT がそのまま完了の JWT になる。
		completion = sess.JWT
	}
	for _, bucket := range sess.Buckets {
		if len(bucket) == 0 {
			continue
		}
		first, ok := byKey[bucket[0]]
		if !ok {
			return "", fmt.Errorf("publish: cloudflare api: unknown asset hash %q", bucket[0])
		}
		if progress != nil {
			progress(Progress{Done: len(uploaded), Total: total, Path: first.name})
		}
		body, contentType, err := cfwUploadForm(dir, bucket, byKey)
		if err != nil {
			return "", err
		}
		var up struct {
			JWT string `json:"jwt"`
		}
		if err := w.api.call(ctx, http.MethodPost, w.accountPath()+"/workers/assets/upload?base64=true", sess.JWT, contentType, body, &up); err != nil {
			return "", fmt.Errorf("%w: %w", errUploadJWT, err)
		}
		for _, k := range bucket {
			uploaded[k] = true
		}
		if up.JWT != "" {
			completion = up.JWT
		}
	}
	if completion == "" {
		return "", errors.New("publish: cloudflare api: asset upload finished without a completion token")
	}
	return completion, nil
}

// cfwUploadForm は bucket のファイルを /workers/assets/upload?base64=true の multipart にする。部分の名前とファイル名は
// ハッシュ、中身は Base64、部分の Content-Type は公開するときの Content-Type（Cloudflare はそれを配信に使う）。
func cfwUploadForm(dir string, bucket []string, byKey map[string]cfAsset) (io.Reader, string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, key := range bucket {
		a, ok := byKey[key]
		if !ok {
			return nil, "", fmt.Errorf("publish: cloudflare api: unknown asset hash %q", key)
		}
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="`+key+`"; filename="`+key+`"`)
		h.Set("Content-Type", contentType(a.name))
		part, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if err := writeBase64File(part, filepath.Join(dir, filepath.FromSlash(a.name))); err != nil {
			return nil, "", fmt.Errorf("publish: read %s: %w", a.name, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &body, mw.FormDataContentType(), nil
}

func writeBase64File(dst io.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := base64.NewEncoder(base64.StdEncoding, dst)
	if _, err := io.Copy(enc, f); err != nil {
		return err
	}
	return enc.Close()
}

// putScript は完了の JWT のアセットを、スクリプトを持たない Worker の新しい版として公開する。
func (w *CloudflareWorkers) putScript(ctx context.Context, completion string, assetConfig map[string]string) error {
	meta, err := json.Marshal(map[string]any{
		"assets":             map[string]any{"jwt": completion, "config": assetConfig},
		"compatibility_date": cfwCompatibilityDate,
	})
	if err != nil {
		return err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(meta); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	return w.api.call(ctx, http.MethodPut, w.scriptPath(), w.cfg.APIToken, mw.FormDataContentType(), &body, nil)
}
