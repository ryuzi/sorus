package importer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"sorus/internal/image"
	"sorus/internal/theme"
)

const (
	// UploadURLPrefix は生成した WebP を配信する URL パス。server.UploadURLPrefix と同じ値。
	UploadURLPrefix = "/uploads/"

	// downloadTimeout は画像1枚のダウンロードのタイムアウト。
	downloadTimeout = 10 * time.Second
	// downloadAttempts はダウンロードの試行回数（初回 + リトライ1回）。
	downloadAttempts = 2
	// retryDelay はリトライまでの待ち時間。
	retryDelay = 500 * time.Millisecond
)

// markdownImage は Markdown の画像構文 ![alt](url) / ![alt](<url>) / ![alt](url "title") の外部 URL。
var markdownImage = regexp.MustCompile(`!\[[^\]]*\]\(\s*<?(https?://[^\s<>)]+)>?(?:\s+"[^"]*")?\s*\)`)

// ExternalImageURLs は Markdown 本文の画像構文から外部画像 (http/https) の URL を出現順・重複なしで返す。
func ExternalImageURLs(md string) []string {
	var urls []string
	seen := map[string]bool{}
	for _, m := range markdownImage.FindAllStringSubmatch(md, -1) {
		if u := m[1]; !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls
}

// ReplaceURLs は本文中の旧 URL を新 URL に一括置換する。長い URL から置換し、
// 前方一致する短い URL（例: a.jpg と a.jpg?w=300）の置換が長い方を壊さないようにする。
func ReplaceURLs(md string, replacements map[string]string) string {
	if len(replacements) == 0 {
		return md
	}
	pairs := make([]string, 0, len(replacements)*2)
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		keys = append(keys, k)
	}
	// strings.Replacer は引数順に比較するため、長い順に並べる。
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		pairs = append(pairs, k, replacements[k])
	}
	return strings.NewReplacer(pairs...).Replace(md)
}

// FetchedImage はダウンロードして WebP 化した画像。
type FetchedImage struct {
	MediaID   string
	SourceURL string
	Filename  string // 元 URL のファイル名（media.filename に保存する）
	URL       string // 本文に埋め込む配信 URL（大サイズ WebP）
	Processed *image.ProcessedImage
}

// MediaFetcher は外部画像をダウンロードし、image.Process で EXIF 破棄・WebP 化して保存する。
// 同じ URL は1回だけ処理する。並行に使用してはならない。
type MediaFetcher struct {
	client    *http.Client
	uploadDir string
	maxSize   int64
	cache     map[string]fetchResult
	// checkURL は取得前に URL を調べる関数。SSRF 防御付きの既定のクライアントを使うときだけ checkImageURL を入れる
	// （呼び出し側が渡したクライアントの接続先の方針には立ち入らない）。
	checkURL func(*url.URL) error
}

type fetchResult struct {
	img *FetchedImage
	err error
}

// errBlockedURL は画像の URL・接続先がプライベートなネットワークなどを指すため拒んだときのエラー。
var errBlockedURL = errors.New("importer: blocked URL")

// maxRedirects はたどるリダイレクトの上限。
const maxRedirects = 5

// checkPublicIP は接続先のアドレスがインターネットに公開されたユニキャストアドレスでなければエラーを返す。
// テーマの取り込みと同じ判定（theme.CheckPublicAddr）を使い、0.0.0.0/8・CGNAT・NAT64 に埋め込まれた私有 IPv4・
// マルチキャスト・Teredo・6to4・サイトローカル・IPv4 射影アドレス・ゾーン付きアドレスなどもすべて拒む。
func checkPublicIP(addr netip.Addr) error {
	if err := theme.CheckPublicAddr(addr); err != nil {
		return fmt.Errorf("%w: %v", errBlockedURL, err)
	}
	return nil
}

// isPrivateIP は ip がインターネットに公開されたユニキャストアドレスでない（接続を拒む）かを返す。
func isPrivateIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return checkPublicIP(addr) != nil
}

// checkImageURL は画像の URL の形を調べる。http・https のみ、ユーザー情報なし、ホスト必須。
// ホストが IP アドレスならその場で checkPublicIP を通す（名前は接続時に解決したアドレスを調べる）。
func checkImageURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: empty URL", errBlockedURL)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: invalid scheme %q", errBlockedURL, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%w: URLs with user information are not allowed", errBlockedURL)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: URL has no host", errBlockedURL)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return checkPublicIP(addr)
	}
	return nil
}

// defaultSecureTransport はプライベート IP や内部ネットワークへの接続（SSRF）を遮断する Transport を返す。
//
//   - 接続は net.Dialer の ControlContext で、DNS 解決後に実際に接続するアドレスごとに checkPublicIP を通す。
//     解決と接続の間で名前の向き先を変える DNS リバインディングでも、接続の直前に調べるので防げる。
//   - 環境変数のプロキシは使わない（プロキシ越しでは接続先のアドレスを調べられないため）。
func defaultSecureTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   downloadTimeout,
		KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: unexpected dial address %q", errBlockedURL, address)
			}
			return checkPublicIP(ap.Addr())
		},
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// checkRedirect はリダイレクト先も checkImageURL で調べ直し、maxRedirects 回までに限る。
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("importer: too many redirects")
	}
	return checkImageURL(req.URL)
}

// NewMediaFetcher は uploadDir に WebP を保存する MediaFetcher を返す。
// client が nil ならタイムアウト 10 秒・SSRF 防御付きの http.Client を使う。maxSize が 0 以下なら image.DefaultMaxUploadSize。
func NewMediaFetcher(client *http.Client, uploadDir string, maxSize int64) *MediaFetcher {
	var checkURL func(*url.URL) error
	if client == nil {
		checkURL = checkImageURL
		client = &http.Client{
			Timeout:       downloadTimeout,
			Transport:     defaultSecureTransport(),
			CheckRedirect: checkRedirect,
		}
	}
	if maxSize <= 0 {
		maxSize = image.DefaultMaxUploadSize
	}
	return &MediaFetcher{client: client, uploadDir: uploadDir, maxSize: maxSize, cache: map[string]fetchResult{}, checkURL: checkURL}
}

// Fetch は rawURL の画像を取得・変換する。失敗時はエラーを返し、同じ URL の再試行もしない。
// cached は2回目以降の呼び出し（統計を重複して数えないため）かを示す。
func (f *MediaFetcher) Fetch(ctx context.Context, rawURL string) (img *FetchedImage, cached bool, err error) {
	if r, ok := f.cache[rawURL]; ok {
		return r.img, true, r.err
	}
	img, err = f.fetch(ctx, rawURL)
	f.cache[rawURL] = fetchResult{img: img, err: err}
	return img, false, err
}

func (f *MediaFetcher) fetch(ctx context.Context, rawURL string) (*FetchedImage, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("importer: invalid image url %q", rawURL)
	}
	if f.checkURL != nil {
		if err := f.checkURL(u); err != nil {
			return nil, fmt.Errorf("importer: image url %q: %w", rawURL, err)
		}
	}
	data, err := f.download(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	// CDN が拡張子と異なる形式（.jpg で WebP など）を返すことがあるため、拡張子は渡さず実体で判定させる。
	filename := path.Base(u.Path)
	if filename == "." || filename == "/" {
		filename = "image"
	}
	stem := strings.TrimSuffix(filename, path.Ext(filename))
	processed, err := image.Process(bytes.NewReader(data), stem, image.ProcessOptions{
		MaxUploadSize: f.maxSize,
		OutputDir:     f.uploadDir,
	})
	if err != nil {
		return nil, fmt.Errorf("importer: process image %s: %w", rawURL, err)
	}
	id, err := newHexID()
	if err != nil {
		RemoveImageFiles(processed)
		return nil, err
	}
	return &FetchedImage{
		MediaID:   id,
		SourceURL: rawURL,
		Filename:  filename,
		URL:       UploadURLPrefix + path.Base(processed.LargePath),
		Processed: processed,
	}, nil
}

// download は GET を最大2回試行する。ネットワークエラーと 5xx・429 だけを再試行する。
func (f *MediaFetcher) download(ctx context.Context, rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		data, retry, err := f.get(ctx, rawURL)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (f *MediaFetcher) get(ctx context.Context, rawURL string) (data []byte, retry bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("importer: download %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", "sorus-importer/1.0")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded), fmt.Errorf("importer: download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, retry, fmt.Errorf("importer: download %s: status %d", rawURL, resp.StatusCode)
	}
	if resp.ContentLength > f.maxSize {
		return nil, false, fmt.Errorf("importer: download %s: %w", rawURL, image.ErrFileTooLarge)
	}
	// 上限 +1 バイトまで読み、超過を検知する。
	data, err = io.ReadAll(io.LimitReader(resp.Body, f.maxSize+1))
	if err != nil {
		return nil, true, fmt.Errorf("importer: download %s: %w", rawURL, err)
	}
	if int64(len(data)) > f.maxSize {
		return nil, false, fmt.Errorf("importer: download %s: %w", rawURL, image.ErrFileTooLarge)
	}
	return data, false, nil
}

// RemoveImageFiles は生成した WebP を削除する。DB 登録に失敗したときの後始末に使う。
func RemoveImageFiles(img *image.ProcessedImage) {
	if img == nil {
		return
	}
	for _, p := range []string{img.OriginalPath, img.LargePath, img.MediumPath, img.SmallPath} {
		if p != "" {
			os.Remove(filepath.FromSlash(p))
		}
	}
}

func newHexID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("importer: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
