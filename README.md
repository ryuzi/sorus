<p align="center">
  <img src="docs/images/sorus-logo.svg" alt="SORUS - Plant your ideas anywhere. Let them live everywhere." width="600">
</p>

# sorus

外部依存（Node.js・PostgreSQL・Redis 等）を一切排除した、シングルバイナリの Go 製 CMS & 静的サイトジェネレーターです。

## クイックスタート

### (a) アプリとして試す

自分の PC にインストールしてすぐ使えます。2回目以降も同じコマンドで最新版に更新できます。

**Windows（PowerShell）**

```powershell
irm https://github.com/ryuzi/sorus/releases/latest/download/install.ps1 | iex
```

**macOS / Linux（ターミナル）**

```sh
curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh
```

起動後、ブラウザに「はじめに」画面が表示されたら、ユーザー名・パスワード・サイト名を設定するだけですぐに書き始められます。

### (b) ソースから動かして試す

Go 1.25 以上が必要です。

```bash
git clone https://github.com/ryuzi/sorus.git
cd sorus
go mod download
make dev
```

起動後、ブラウザで以下の URL にアクセスします。

* 公開サイト: `http://localhost:8080`
* 管理画面: `http://localhost:8080/admin/login`
  * 開発用初期ユーザー: `admin`
  * 開発用初期パスワード: `dev-admin-pass`

配布版と同じデスクトップモード（「はじめに」画面・OS 標準のデータ保存先）で試す場合は `go run . --desktop` で起動してください。

## 主な特徴

* **シングルバイナリ・外部依存ゼロ**: Go（Pure Go SQLite `modernc.org/sqlite`）と埋め込みアセットのみで動作し、外部データベースや Node.js は不要です。
* **選べる公開形態**: 静的サイト生成（SSG: Cloudflare Pages・FTP/SFTP・ローカル出力）、Let's Encrypt 自動 HTTPS 対応のスタンドアロンサーバー、管理画面を遮断する公開専用サーバー（`--public-only`）に対応しています。
* **AI ネイティブ執筆・MCP 連携**: Gemini・OpenAI・Claude・Ollama による記事作成支援に加え、`llms.txt` / `llms-full.txt` の標準出力と Model Context Protocol（OAuth 2.1 / API トークン）を搭載しています。
* **メディア管理・日本語全文検索**: 画像アップロード時の EXIF 自動削除と WebP 変換、AI による Alt テキスト生成、外部 JS 不要の `:::gallery` 記法、Bleve v2（CJK バイグラム）による全文検索を備えています。
* **チーム運用と無停止バックアップ**: 4段階のロール（読者・ライター・編集者・管理者）、排他編集ロック、招待コードによる公開サーバー同期、稼働中 SQLite の Zip バックアップと復元を行えます。
* **テーマとサイト構成のカスタマイズ**: 5種類の標準テーマに加え、Git リポジトリや Zip から独自テーマを取り込み可能。固定ページやナビゲーションメニューも管理画面から編集できます。

詳細な仕様・CLI コマンド・環境変数については [ドキュメント一覧 (docs/README.md)](docs/README.md) を参照してください。

## ドキュメント

立場・目的に合わせて必要なガイドを参照できます。

| 立場・目的 | ドキュメント | 主な内容 |
| :--- | :--- | :--- |
| **導入者** | [インストールガイド](docs/install_guide.md) | 1行インストール、セキュリティ警告の対処、更新・削除、データ保存先 |
| **執筆者・編集者** | [執筆・編集ガイド](docs/writing_guide.md) | 記事作成、画像・ギャラリー、AI 執筆支援、校閲・承認フロー |
| **サイト管理者** | [サイト管理ガイド](docs/site_admin_guide.md) | ユーザー・権限管理、サイト基本設定、外観・テーマ、バックアップ |
| **サイト管理者（公開）** | [静的サイト公開ガイド](docs/static_site_guide.md) | レンタルサーバー・Cloudflare Pages・フォルダへの公開手順 |
| **サイト管理者（同期）** | [公開サーバー連携ガイド](docs/decoupled_sync_guide.md) | 管理画面のない公開サーバーの導入、招待コード同期、MCP 連携 |
| **サイト管理者（移行）** | [WordPress 移行ガイド](docs/wordpress_migration_guide.md) | WordPress（WXR XML）からの記事・画像・URL の移行手順 |
| **運用・インフラ担当** | [運用保守ガイド](docs/operations_guide.md) | デプロイ構成比較、Docker / systemd 運用、監視、環境変数一覧 |
| **テーマ制作者** | [テーマ開発仕様書](docs/theme_development_spec.md) | 独自テーマのディレクトリ構成、テンプレート仕様、配布方法 |
| **開発者** | [開発者ガイド](docs/developer_guide.md) | アーキテクチャ、パッケージ構成、ビルド・テスト、セキュリティ機構 |
| **全体目次** | [ドキュメントハブ](docs/README.md) | 全ガイド・設計仕様書の一覧と立場別ナビゲーション |

## ライセンス

[MIT License](LICENSE)
