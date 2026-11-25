# 環境設定

各 `.env.example` は設定項目の見本です。秘密情報を含む実設定は同じ `deploy/environments/` 配下に置き、Git に登録せず、所有者だけが読める権限（例: `chmod 600`）にしてください。`ENV_FILE` が指定されるとそのファイルを読み込み、未指定時はリポジトリ直下の `.env` を読み込みます。プロセス環境変数は設定ファイルの値より優先されます。

| 変数 | 用途 |
| --- | --- |
| `PORT` | HTTP 待受ポート。Compose 本番ではコンテナ内 8080 を使用します。 |
| `DB_PATH` | SQLite データベースファイル。永続領域に配置してください。 |
| `UPLOAD_DIR` | 画像などアップロードファイルの保存先。 |
| `SEARCH_INDEX_DIR` | 全文検索インデックスの保存先。 |
| `ATTACHMENT_DIR` | 記事添付ファイルの保存先。 |
| `BACKUP_DIR` | `sorus backup` の既定出力先。 |
| `SITE_NAME`, `SITE_DESCRIPTION` | サイト名と説明。 |
| `BASE_URL` | 公開 URL。OGP や canonical URL 生成に使います。 |
| `ADMIN_PASSWORD` | 初回管理者の初期パスワード。初回ログイン後に変更してください。 |
| `SECURE_COOKIE` | Cookie の Secure 属性。HTTPS 本番は `true` にします。 |
| `AUTOCERT_DOMAIN`, `AUTOCERT_EMAIL` | 設定すると Let's Encrypt 自動 HTTPS を有効にします。DNS と外部 80/443 到達性が必要です。 |
| `AUTOCERT_CACHE_DIR` | 証明書キャッシュ。永続領域に置いてください。 |
| `PUBLIC_ONLY` | `true` で公開専用モード（管理画面を無効にし、公開ページと同期 API だけを配信）。`--public-only` と同じです。 |
| `CAPABILITY_KEY_FILE` | 同期 API のトークン署名鍵（既定 `capability.key`）。永続領域に置き、バックアップしてください。同じディレクトリに自動生成される `sync_path.txt`（同期 API の秘匿パス）も一緒にバックアップしてください。 |
| `SYNC_API_PATH` | 同期 API のパス（例: `/api/sync-<ランダムな16進数>`）。`--sync-path` と同じです。未設定なら署名鍵と同じディレクトリの `sync_path.txt` に保存した推測できないパスを使い、無ければ生成します。固定の `/api/sync` は秘匿パスの使用時は 404 になります。 |
| `GEMINI_API_KEY` | 任意の AI 推敲機能用 API キー。 |

ステージングと本番には異なる DB、ファイル保存先、管理者パスワード、API キーを用意してください。サンプルのパスワードや `example.ac.jp` を実運用に流用しないでください。
