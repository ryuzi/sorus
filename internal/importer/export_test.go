package importer

import (
	"net/http"

	"github.com/jmoiron/sqlx"

	"sorus/internal/search"
)

// NewServerHandler は、取り込んだ記事を公開サイトで確かめるための HTTP ハンドラー（server.New の Handler）を作る。
// server パッケージは管理画面の取り込み（handlers_import.go）のために importer を使うため、この内部のテストからは
// server を import できない（循環になる）。外部テストパッケージの server_bridge_test.go が init で設定する。
// idx が nil なら、サーバーはメモリ上の検索インデックスを作る。
var NewServerHandler func(db *sqlx.DB, uploadDir string, idx *search.Index) (http.Handler, error)

func newServerHandler(db *sqlx.DB, uploadDir string, idx *search.Index) (http.Handler, error) {
	return NewServerHandler(db, uploadDir, idx)
}
