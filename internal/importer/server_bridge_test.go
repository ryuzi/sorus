package importer_test

import (
	"net/http"

	"github.com/jmoiron/sqlx"

	"sorus/internal/importer"
	"sorus/internal/search"
	"sorus/internal/server"
)

// 内部のテスト（importer_test.go）が取り込んだ記事を公開サイトで確かめられるよう、server.New のハンドラーを渡す
// （export_test.go の NewServerHandler）。外部テストパッケージは、importer を使う server を import できる。
func init() {
	importer.NewServerHandler = func(db *sqlx.DB, uploadDir string, idx *search.Index) (http.Handler, error) {
		cfg := server.Config{DB: db, UploadDir: uploadDir}
		if idx != nil {
			cfg.Search = idx
		}
		srv, err := server.New(cfg)
		if err != nil {
			return nil, err
		}
		return srv.Handler(), nil
	}
}
