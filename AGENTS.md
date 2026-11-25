# sorus (Full-Stack Go CMS)

IT専門知識のないユーザーでも迷わず扱える、Node.js等の外部依存を一切排除したシングルバイナリのFull-Stack Go CMS。

## コマンド

- セットアップ: `go mod download`
- 検証（完了前に必須）: `.navigator/verify.sh`
- 単体テスト: `go test -v ./...`
- 起動: `make dev` または `go run .`

## 構成

- `docs/`: 要件定義書・設計書（`development_spec.md` 等）
- `internal/`: 各種業務ロジック（server, database, auth, article, desktop, publish, backup, importer, search, markdown, image ほか。一覧は `docs/developer_guide.md`）
- `templates/`: `html/template` によるSSRテンプレート
- `static/`: 静的アセット（埋め込み用）
- `install/`・`scripts/`・`deploy/`: 1行インストール、リリース、公開サーバーの構築

## 規約

- Go 1.25以上（`go.mod`）。標準ライブラリ（`net/http`, `embed`, `crypto` 等）を最大限活用。
- 外部の重厚なミドルウェア（Node.js, Redis, PostgreSQL等）への依存を排除し、シングルバイナリで動作すること。
- SQLite は Pure Go の `modernc.org/sqlite` を使い、ポータビリティを確保（cgo なし）。
- darwin/arm64 のビルドは CI だけで行う。WSL で Windows・macOS 向けを作るのは、WSL に 10GB 以上を割り当て、コンパイルにメモリ上限を付けたときだけ（それ以外は WSL ごと落ちる。手順は `docs/developer_guide.md` §4）。

## 完了の定義

- `.navigator/verify.sh` が exit 0
- 変更に対応する単体テスト・結合テストがある（バグ修正は再現テストを先に書く）
- UI 変更は内蔵ブラウザ／スクリーンショットで確認済み

## 禁止事項

- `.navigator/protected` に列挙したパスを編集しない
- テストの skip 化・閾値緩和で検証を通さない