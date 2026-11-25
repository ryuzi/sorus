package server

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
)

// 公開サーバーを TLS で起動する試験（newPublicOnlyTLSEnv・newSyncEnvTLS）の準備。本番の公開サーバーは
// AUTOCERT_DOMAIN で sorus 自身が TLS を受け、PC は https で接続する。閲覧制限の変更（set_site_access）は TLS で
// 直接届いた接続でしか受け付けないため、試験でも同じ経路を通す。
//
// httptest の TLS サーバーはすべて同じ試験用の証明書（127.0.0.1・::1・example.com）を使う。PC が公開サーバーを呼ぶ
// クライアント（同期 API・画像の取得の remoteHTTPClient と、公開サイトを外から確かめる siteAccessProbeClient）に、
// その証明書を信頼させる。どの試験よりも前（init）に設定し、試験の途中で書き換えない（-race で競合しない）。
func init() {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	srv.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	remoteHTTPClient.Transport = transport
	siteAccessProbeClient.Transport = transport
}
