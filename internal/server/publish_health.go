package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"sorus/internal/mcp"
)

const publishHealthSetting = "publish_health_status"

func (s *Server) mcpCheckPublishHealth(ctx context.Context) mcp.PublishHealthResult {
	return mapPublishHealthResult(s.checkPublishHealth(ctx))
}

func mapPublishHealthResult(status publishHealthStatus) mcp.PublishHealthResult {
	checked := status.CheckedAt
	if checked.IsZero() {
		checked = time.Now()
	}
	issues := []string{}
	if status.Message != "" && status.State != "ok" {
		issues = append(issues, status.Message)
	}
	for _, key := range status.AdditionalMessageKeys {
		if strings.Contains(key, "sitemap") {
			issues = append(issues, "サイトマップを確認できませんでした。")
		} else {
			issues = append(issues, key)
		}
	}
	sitemapFound := status.State != "error" && !strings.Contains(status.Message, "サイトマップ")
	for _, key := range status.AdditionalMessageKeys {
		if strings.Contains(key, "sitemap") {
			sitemapFound = false
		}
	}
	tlsDays := 0
	if !status.TLSExpiresAt.IsZero() {
		tlsDays = int(time.Until(status.TLSExpiresAt).Hours() / 24)
	}
	return mcp.PublishHealthResult{Healthy: status.State == "ok", StatusCode: status.HTTPStatus,
		ResponseTimeMS: status.LatencyMS, TLSDaysRemaining: tlsDays, SitemapFound: sitemapFound,
		Issues: issues, CheckedAt: checked.UTC()}
}

type publishHealthStatus struct {
	State                 string    `json:"state"`
	CheckedAt             time.Time `json:"checked_at"`
	CheckedText           string    `json:"checked_text"`
	HTTPStatus            int       `json:"http_status,omitempty"`
	LatencyMS             int64     `json:"latency_ms,omitempty"`
	Message               string    `json:"message"`
	MessageKey            string    `json:"message_key"`
	AdditionalMessageKeys []string  `json:"additional_message_keys,omitempty"`
	TLSExpiresAt          time.Time `json:"tls_expires_at,omitempty"`
	TLSExpiresText        string    `json:"tls_expires_text,omitempty"`
}

func (s *Server) loadPublishHealth() *publishHealthStatus {
	raw := s.getSetting(publishHealthSetting, "")
	if raw == "" {
		return nil
	}
	var status publishHealthStatus
	if json.Unmarshal([]byte(raw), &status) != nil {
		return nil
	}
	return &status
}

func (s *Server) checkPublishHealth(ctx context.Context) publishHealthStatus {
	base := strings.TrimSpace(s.getSetting("base_url", s.getBaseURL()))
	status := runPublishHealthCheck(ctx, base, nil)
	status.CheckedAt = time.Now()
	status.CheckedText = status.CheckedAt.In(jst).Format("2006/01/02 15:04")
	b, err := json.Marshal(status)
	if err == nil {
		if err := upsertSetting(s.db, publishHealthSetting, string(b)); err != nil {
			s.echo.Logger.Warnf("publish health: save result: %v", err)
		}
	}
	return status
}

func (s *Server) runAutoPublishHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.bgMu.Lock()
	if s.shutDown || s.bgStopped {
		s.bgMu.Unlock()
		return
	}
	s.publishHealthWG.Add(1)
	s.bgMu.Unlock()
	defer s.publishHealthWG.Done()
	s.checkPublishHealth(ctx)
}

func runPublishHealthCheck(ctx context.Context, base string, client *http.Client) publishHealthStatus {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := publishHealthStatus{State: "error"}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		result.Message = "公開サイトのURLを設定してください。"
		result.MessageKey = "publish_health_unconfigured"
		return result
	}
	if client == nil {
		if err := publicHealthTarget(ctx, u); errors.Is(err, errNonPublicTarget) {
			result.Message = "公開サイトのURLは、インターネットから見られるアドレスを設定してください。"
			result.MessageKey = "publish_health_private_target"
			return result
		} else if err != nil {
			result.Message = describeHealthError(err)
			result.MessageKey = healthErrorKey(err)
			return result
		}
		// 接続のたびに名前を1回だけ解決し、公開されたアドレスにだけ接続する（DNS リバインディングを防ぐ）。
		transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: publicDialer{timeout: 10 * time.Second}.DialContext}
		client = &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: healthCheckRedirect}
	}
	started := time.Now()
	page, err := requestHealthURL(ctx, client, u.String())
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Message = describeHealthError(err)
		result.MessageKey = healthErrorKey(err)
		return result
	}
	defer page.Close()
	result.HTTPStatus = page.StatusCode
	if page.TLS != nil && len(page.TLS.PeerCertificates) > 0 {
		result.TLSExpiresAt = page.TLS.PeerCertificates[0].NotAfter
		result.TLSExpiresText = result.TLSExpiresAt.In(jst).Format("2006/01/02")
	}
	if page.StatusCode == http.StatusForbidden {
		result.Message = "公開サイトがアクセスを拒否しています。公開設定やバケットポリシー、Webサーバーのアクセス制限を確認してください。"
		result.MessageKey = "publish_health_forbidden"
		return result
	}
	if page.StatusCode == http.StatusNotFound {
		result.Message = "トップページが見つかりません。公開フォルダの場所や、index.html への振り分け設定を確認してください。"
		result.MessageKey = "publish_health_not_found"
		return result
	}
	if page.StatusCode < 200 || page.StatusCode >= 300 {
		result.Message = fmt.Sprintf("公開サイトが HTTP %d を返しました。公開先の設定を確認してください。", page.StatusCode)
		result.MessageKey = "publish_health_http_error"
		return result
	}
	if !strings.Contains(strings.ToLower(page.ContentType), "text/html") {
		result.Message = "公開サイトのトップページが HTML を返していません。公開先のルートフォルダを確認してください。"
		result.MessageKey = "publish_health_not_html"
		return result
	}
	if page.TLS != nil && len(page.TLS.PeerCertificates) > 0 && time.Until(page.TLS.PeerCertificates[0].NotAfter) < 30*24*time.Hour {
		result.State = "warning"
		result.Message = "サイトは表示できますが、SSL 証明書の期限が30日以内です。公開先で証明書を更新してください。"
		result.MessageKey = "publish_health_tls_expiring"
	} else {
		result.State = "ok"
		result.Message = "トップページは正常に表示されています。"
		result.MessageKey = "publish_health_ok"
	}
	page.Close()
	page, err = requestHealthURL(ctx, client, strings.TrimRight(u.String(), "/")+"/sitemap.xml")
	if err != nil || page.StatusCode < 200 || page.StatusCode >= 300 {
		if page != nil {
			page.Close()
		}
		if result.State == "ok" {
			result.State = "warning"
		}
		result.Message += " サイトマップを確認できませんでした。公開先に sitemap.xml があるか確認してください。"
		if result.MessageKey == "publish_health_tls_expiring" {
			result.AdditionalMessageKeys = append(result.AdditionalMessageKeys, "publish_health_sitemap_warning")
		} else {
			result.MessageKey = "publish_health_sitemap_warning"
		}
	} else {
		page.Close()
	}
	return result
}

func healthCheckRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("redirect limit exceeded")
	}
	return nil
}

type publishHealthResponse struct {
	StatusCode  int
	ContentType string
	TLS         *tls.ConnectionState
	Close       func()
}

func requestHealthURL(ctx context.Context, client *http.Client, raw string) (*publishHealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "sorus-publish-health/1.0")
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	tlsState := resp.TLS
	return &publishHealthResponse{StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), TLS: tlsState, Close: func() { _ = resp.Body.Close() }}, nil
}

// publicHealthTarget は公開サイトの URL のホストが、インターネットに公開されたアドレスだけに解決されるかを確かめる
// （接続の前に、私有アドレスなどを publish_health_private_target として案内するため）。判定は接続のときと同じ
// publicDialer の resolve で、接続のときにも解決したアドレスを確かめ直す（DNS リバインディング対策）。
func publicHealthTarget(ctx context.Context, u *url.URL) error {
	_, err := publicDialer{}.resolve(ctx, u.Hostname())
	return err
}

func waitPublishHealth(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func healthErrorKey(err error) string {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "publish_health_timeout"
	case strings.Contains(strings.ToLower(err.Error()), "certificate"), strings.Contains(strings.ToLower(err.Error()), "tls"):
		return "publish_health_tls_error"
	case strings.Contains(strings.ToLower(err.Error()), "no such host"), strings.Contains(strings.ToLower(err.Error()), "lookup"), strings.Contains(strings.ToLower(err.Error()), "no addresses"):
		return "publish_health_dns_error"
	default:
		return "publish_health_connect_error"
	}
}

func describeHealthError(err error) string {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "公開サイトへの応答が10秒以内にありませんでした。公開先の稼働状況を確認してください。"
	case strings.Contains(strings.ToLower(err.Error()), "certificate"), strings.Contains(strings.ToLower(err.Error()), "tls"):
		return "SSL 証明書を確認できません。ドメインの SSL 設定と証明書の期限を確認してください。"
	case strings.Contains(strings.ToLower(err.Error()), "no such host"), strings.Contains(strings.ToLower(err.Error()), "lookup"), strings.Contains(strings.ToLower(err.Error()), "no addresses"):
		return "ドメインが見つかりません。ドメインの取得状況と DNS 設定を確認してください。"
	default:
		return "公開サイトに接続できません。ドメインの DNS 設定と公開先の稼働状況を確認してください。"
	}
}
