package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

// 公開サーバーの画像を、PC の管理画面を通して見せる（校閲待ちの一覧・プレビュー）。
// 公開サーバーが閲覧制限（Basic 認証）を有効にしていると、ブラウザは公開サーバーの /uploads/ を直接読めない
// （閲覧用のパスワードを持たない）。PC が同期 API のトークンで取得し（取り込みと同じ fetchRemoteImage）、
// PC にログインした編集者にだけ返す。トークンはブラウザに渡さない。

// remoteUploadProxyPrefix は公開サーバーの画像を PC を通して返すパス（handleRemoteUpload）。
const remoteUploadProxyPrefix = "/admin/remote/uploads/"

var (
	// remotePreviewImagePattern はプレビューの HTML の属性値のうち、同期の画像の形式（syncImageNamePattern）の
	// /uploads/ の参照。
	remotePreviewImagePattern = regexp.MustCompile(`"/uploads/([0-9a-f]{32}-(?:original|[0-9]{1,5})\.webp)"`)
	// errRemoteImageNotFound は公開サーバーに画像が無い（404）こと。
	errRemoteImageNotFound = errors.New("remote image not found")
)

// remotePreviewHTML は公開サーバーの記事を描画した html の画像の参照を、ブラウザが読める URL に書き換える。
// 同期の画像の形式なら PC を通すパス（remoteUploadProxyPrefix）にし、それ以外の /uploads/ は従来どおり公開サーバーを指す。
func remotePreviewHTML(html string, cfg remoteConfig) string {
	html = remotePreviewImagePattern.ReplaceAllString(html, `"`+remoteUploadProxyPrefix+`$1"`)
	return strings.ReplaceAll(html, `"`+UploadURLPrefix, `"`+cfg.URL+UploadURLPrefix)
}

// fetchRemoteImage は公開サーバーの /uploads/name を同期 API のトークンで取得し、公開サーバーへの提出と同じ検査
// （ファイル名・WebP であること・サイズ・画素数）を通ったものだけを返す。公開サーバーが閲覧制限を有効にしていても、
// 閲覧用のパスワードの代わりにトークンで取得できる（syncTokenReadsUpload）。リダイレクトは追わないため
// （remoteHTTPClient）、トークンは設定した公開サーバーにだけ送る。エラーには URL・トークンを入れない。
func (s *Server) fetchRemoteImage(ctx context.Context, cfg remoteConfig, name string) ([]byte, error) {
	if !syncImageNamePattern.MatchString(name) {
		return nil, fmt.Errorf("remote image: invalid name: %w", errRemoteImageNotFound)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL+UploadURLPrefix+name, nil)
	if err != nil {
		return nil, errors.New("remote image: invalid server URL")
	}
	req.Header.Set("User-Agent", "sorus-remote-sync")
	if cfg.Token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+cfg.Token)
	}
	res, err := remoteHTTPClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("remote image %s: %w", name, err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("remote image %s: %w", name, errRemoteImageNotFound)
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("remote image %s: status %d", name, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, s.cfg.MaxUploadSize+1))
	if err != nil {
		return nil, fmt.Errorf("remote image %s: read: %w", name, err)
	}
	if err := s.validateSyncImages([]SyncImage{{Name: name, Data: data}}); err != nil {
		return nil, fmt.Errorf("remote image %s: %w", name, err)
	}
	return data, nil
}

// handleRemoteUpload は公開サーバーの画像を PC を通して返す。校閲待ちの一覧・プレビューの <img> が読む。
// 校閲待ちの記事の取得と同じく、PC の編集者が、校閲待ちを読めるトークン（editor・admin）を設定しているときだけ返す。
// 公開サーバーの任意の URL は取得せず、同期の画像の形式のファイル名だけを設定した公開サーバーの /uploads/ から取得する。
func (s *Server) handleRemoteUpload(c echo.Context) error {
	name := c.Param("name")
	if !syncImageNamePattern.MatchString(name) {
		return echo.NewHTTPError(http.StatusNotFound, "画像が見つかりません")
	}
	cfg, err := s.requireRemote(auth.CanFetchPending, "校閲待ちの記事の取得")
	if err != nil {
		return err
	}
	data, err := s.fetchRemoteImage(c.Request().Context(), cfg, name)
	if errors.Is(err, errRemoteImageNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "公開サーバーにこの画像がありません")
	}
	if err != nil {
		c.Logger().Warnf("server: %v", err)
		return echo.NewHTTPError(http.StatusBadGateway, "公開サーバーから画像を取得できませんでした")
	}
	h := c.Response().Header()
	// 管理画面のログインで見せる画像のため、共有のキャッシュにも、ブラウザのキャッシュにも残さない。
	h.Set(echo.HeaderCacheControl, "private, no-store")
	h.Set(echo.HeaderXContentTypeOptions, "nosniff")
	h.Set(echo.HeaderContentSecurityPolicy, "default-src 'none'; sandbox")
	return c.Blob(http.StatusOK, "image/webp", data)
}
