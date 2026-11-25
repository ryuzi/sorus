# 開発者ガイド (Developer Guide)

本ドキュメントは、Full-Stack Go CMS のアーキテクチャ、設計原則、コードベース構成、開発・テスト手法を解説する開発者向けドキュメントです。

---

## 1. 設計思想とコア原則

* **外部ミドルウェアゼロのシングルバイナリ**:
  * Node.js, Python, Redis, PostgreSQL などの外部ランタイムに一切依存せず、Go の標準ツールチェーンのみでビルド・実行可能。
  * `embed.FS` により HTML テンプレートや CSS/JS などの静的アセットをバイナリ内に完全内包。
* **純Go SQLite による可搬性**:
  * CGO 不要の Pure Go SQLite ドライバー（`modernc.org/sqlite`）を採用し、クロスコンパイルや軽量コンテナ（Alpine / scratch）への配置を容易化。
* **ハイブリッド配信（SSR & SSG）**:
  * **SSR (Server-Side Rendering)**: Echo + `html/template` による動的配信・管理画面のリアルタイム編集。
  * **SSG (Static Site Generation)**: `sorus export` による完全静的 HTML/XML/アセット一括出力。

---

## 2. ディレクトリ構成

```mermaid
flowchart LR
  r0["（リポジトリ直下）"]
  r0 --> n0["main.go<br/>エントリーポイント・動作モードの選択（サーバー / デスクトップ）・複数サイトの管理（desktopApp / desktopSite）・CLIサブコマンド（export, backup, restore, import, sites, token create）、--version、help"]
  r0 --> n1["static/<br/>埋め込み用静的アセット（css, js, favicon）"]
  r0 --> n2["templates/<br/>埋め込み用 html/template"]
  n2 --> n3["admin/<br/>管理画面用テンプレート（ダッシュボード、記事編集、メディアDAM、設定等）"]
  n2 --> n4["public/<br/>公開Webサイト用テンプレート（トップ、記事詳細、ギャラリー、カテゴリ等）"]
  r0 --> n5["internal/"]
  n5 --> n6["ai/<br/>LLM連携（Gemini, OpenAI, Claude, Ollama）、推敲、Grounding、画像解析"]
  n5 --> n7["analytics/<br/>アクセス集計・プライバシー保護型ダッシュボードメトリクス"]
  n5 --> n8["article/<br/>記事エンティティ・リポジトリ"]
  n5 --> n9["auth/<br/>セッション管理、サイトごとのCookie名（CookieNamesFor）、パスワードハッシュ（bcrypt）、Ed25519ケーパビリティトークン"]
  n5 --> n10["backup/<br/>Zip完全バックアップ（形式 v2: DB・画像・添付資料）・リストア、Zip Slip防止、サイト別自動バックアップのスケジューラーと世代整理、同期フォルダ（OneDrive 等）の検出"]
  n5 --> n11["desktop/<br/>デスクトップモードの実行環境（モード選択、AppPaths / SitePaths、sites.json、127.0.0.1の空きポート、アプリモードのウィンドウ起動（OpenWindow）、二重起動検出、Host検査、エラーダイアログと終了の確認画面、更新確認）"]
  n5 --> n12["database/<br/>DB接続、マイグレーション、VACUUM INTOオンラインバックアップ"]
  n5 --> n13["i18n/<br/>多言語カタログ（日本語・英語）と翻訳関数"]
  n5 --> n14["image/<br/>EXIF除去・WebP変換・レスポンシブ画像生成・メディアDAM"]
  n5 --> n15["importer/<br/>WordPress WXRインポーター（SSRF防御・画像ローカライズ）"]
  n5 --> n16["lock/<br/>記事の排他的リアルタイム編集ロック（ハートビート）"]
  n5 --> n17["markdown/<br/>goldmark + bluemonday サニタイズドMarkdown変換・ギャラリー拡張"]
  n5 --> n18["mcp/<br/>Model Context Protocol サーバー（外部AIツール連携）"]
  n5 --> n19["publish/<br/>公開先への差分反映（FTP/FTPS・SFTP・Cloudflare Pages・S3互換・GitHub Pages（`github.go`）・フォルダ）、CDNパージとマニフェスト"]
  n5 --> n20["qrcode/<br/>純Go の QR コードエンコーダー（バイトモード・誤り訂正 M・型番1〜10、SVG 出力。現在の画面からは使っていない）"]
  n5 --> n21["rag/<br/>純Goベクトル演算・コサイン類似度・Markdownチャンキング"]
  n5 --> n22["revision/<br/>記事リビジョン履歴・Myers Diff差分管理"]
  n5 --> n23["scheduler/<br/>予約投稿の公開日時が来たことを知らせるワーカー（Goroutine。状態は書き換えない）"]
  n5 --> n24["search/<br/>Bleve v2 + CJKバイグラムによる全文検索"]
  n5 --> n25["server/<br/>Echo ルーター、HTTPハンドラ、同期API・招待、静的エクスポート、公開先・届いた原稿・設定の入口の画面"]
  n5 --> n26["social/<br/>純Go 1200x630 動的OGP画像生成（フォント埋め込み）"]
  n5 --> n27["theme/<br/>公開テーマのカタログ（ビルトイン static/themes/#lt;id#gt;/ と独自テーマの合成・検証・既定テーマ）、独自テーマの Zip / URL 取り込み（Zip Slip・Zip ボム・SSRF 防御）"]
  n5 --> n28["version/<br/>版（リリース時に -ldflags で埋め込む。既定は dev）"]
  r0 --> n29["install/<br/>1行インストール用の install.sh / install.ps1 とリリースノートの雛形"]
  r0 --> n30["scripts/<br/>dev.sh（開発サーバー）、release.sh（リリース一式）、e2e_decoupled_sync.sh"]
  r0 --> n31[".github/workflows/<br/>release.yml（タグ push でリリースを作る）"]
  r0 --> n32["deploy/<br/>Dockerfile, Docker Compose, systemd ユニット, 環境変数見本, install-server.sh（公開サーバーの導入）"]
  r0 --> n33["docs/<br/>開発・運用・利用者向け各種ドキュメント"]
  r0 --> n34["build/icons/<br/>アプリアイコンの資材（元画像 sorus.png、.ico・.icns・Linux 用 PNG・Windows 用 .syso）と生成ツール gen/"]
```

---

## 3. ローカル開発環境のセットアップ

### 前提要件
* Linux / macOS / Windows WSL2
* Go 1.25 以上

### 起動手順
```bash
# 依存パッケージの取得
go mod download

# 開発用サーバーの起動 (scripts/dev.sh または make dev)
make dev
# -> http://localhost:8080 で起動（サーバーモード。初期管理者 admin / dev-admin-pass）

# 配布版と同じデスクトップモードで起動（データは OS 標準の場所。SORUS_DATA_DIR で差し替え可）
go run . --desktop

# 使い方（起動の仕方とすべてのサブコマンド）を表示
go run . help
```

* `sorus help`・`sorus -h`・`sorus --help`（`--desktop`・`--server` を付けても同じ）は、使い方の全体を標準出力に書いて 0 で終わります（`main.go` の `isHelpArgs`・`writeUsage`）。環境変数に設定した同期 API のパスは表示しません。`SYNC_API_PATH` はフラグの既定値にせず、`--sync-path` を指定しなかったときに解析のあとで補う（`syncPathOrEnv`）ため、`sorus --public-only -h`・`sorus token create -h`・引数の誤りで出す使い方にも出ません。
* 各サブコマンドの `-h`（例: `sorus import -h`）は、そのサブコマンドの使い方を表示して 0 で終わります。引数の誤りは 2 で終わります（`exitOnParseError`）。

---

## 4. ビルドと検証

品質管理・CI/CD のため、`.navigator/verify.sh` による自動検証が標準化されています。

```bash
# 静的解析・全単体テスト・コンパイルの実行
make verify
# または
./.navigator/verify.sh

# 単体テストのみ実行
make test
```

### 検証ゲートのルール
`.navigator/verify.sh` は次を順に実行し、すべて成功したら exit 0 になります。
* `go vet ./...` がエラーなしであること
* `go test ./...` が全件 PASS すること
* `go build -v -o /dev/null .` が正常終了すること

依存を変えたときは、`go mod tidy -diff` に差分がないことも手で確かめてください（verify.sh には含まれません）。

### リリースビルドとクロスビルド
* リリースはタグ（`v*`）の push で `.github/workflows/release.yml` が作ります。ソースと配布ファイルは同じリポジトリ `ryuzi/sorus` にあり、配布ファイルは同じリポジトリの Releases に出します（作成には自動で受け取る `GITHUB_TOKEN` を使うため、追加のシークレットは要りません）。`go test ./...` の後に `scripts/release.sh` を実行し、デスクトップ版（windows/darwin は zip、linux は tar.gz）、サーバー版（`sorus-server-<版>-linux-<arch>.tar.gz`）、`install.sh` / `install.ps1` / `install-server.sh`、`SHA256SUMS` をリリースに添付します。
* 手元では `make release VERSION=vX.Y.Z`（= `scripts/release.sh`）で同じ一式を `dist-release/` に作れます。版は `-X sorus/internal/version.Version=<版>`、デスクトップ版は `-X main.buildMode=desktop` で埋め込みます。`-tags nodynamic` を付け、UPX などの圧縮はしません。
* **クロスビルドとメモリ**: 画像変換のライブラリ `gen2brain/webp` の `libwebp.go`（WASM から変換した 2.7MB のコード）は、ARM 向け（`GOARCH=arm64`）に最適化してコンパイルすると、1つで 6GB を超えます（amd64 向けは約 0.9GB、SQLite は arm64 向けでも約 0.7GB）。GitHub の標準ランナー（メモリ約 7.7GB）では、スワップを 18GB 足しても止まりました（2026-10-30）。
  * そのため `scripts/release.sh` は、arm64 向けのときだけ `-gcflags=github.com/gen2brain/webp=-N` で webp の最適化を止めます。sorus 全体の windows/arm64・darwin/arm64 のビルドが約 0.85GB・35 秒で済み、標準ランナーで作れます。
  * 代わりに ARM 版の WebP 変換は約 3.5 倍遅くなります（amd64 での計測で 2000×1500 の画像が 0.49 秒→1.72 秒）。amd64 版は最適化したままです。
  * メモリの多い有料ランナーに移したら、`release.sh` の `WEBP_ARM_GCFLAGS` を外して最適化したまま作ります。
  * 手元で arm64 向けを作るときも、`release.sh` を使うか、同じ `-gcflags` を付けます。付けないと WSL ごと落ちます。
* WSL の割り当てが 8GB 以下（`.wslconfig` を設定していない既定の状態など）のときは、Windows・macOS 向けを WSL で作らないでください。WSL ごと落ち、ほかの作業も止まります。`RELEASE_TARGETS="linux/amd64" ./scripts/release.sh vX.Y.Z` のように linux/amd64 だけを作り、OS ごとの分岐は `GOOS=windows go vet ./internal/desktop/` のように `internal/desktop` だけで確かめます。
* コード署名・公証はまだ行っていません。差し込み位置は `release.yml` にコメントで用意しています。

### アプリアイコン（既定のアイコンと差し替え）
配布版のアプリアイコンは `build/icons/` に置いた資材から組み込みます。既定のアイコンは `static/icon.svg`（管理画面・PWA のアイコン）と同じデザインです（角丸の濃紺の正方形に白い太字の「a」。文字は Go に同梱の Go Bold で描きます）。

| ファイル | 使う場所 |
| --- | --- |
| `sorus.png` | 元画像（1024x1024）。ほかの資材はすべてこれから作る |
| `rsrc_windows_amd64.syso`・`rsrc_windows_arm64.syso` | Windows の `sorus.exe` に埋め込むアイコンのリソース（16〜256px）。`scripts/release.sh` が Windows 向けのビルドの間だけリポジトリのルートに `zz_release_icon_windows_<arch>.syso` として置き（Go は main パッケージの `*_windows_<arch>.syso` を自動でリンクする）、ビルドの後・失敗時・中断時に消す。`install.ps1` のショートカットは `IconLocation` で `sorus.exe` のアイコンを使う |
| `sorus.icns` | macOS 用。`release.sh` が darwin の zip に入れ、`install.sh` が `sorus.app/Contents/Resources/sorus.icns` に置いて `Info.plist` の `CFBundleIconFile` に設定する |
| `sorus-256.png` | Linux 用。`release.sh` が `sorus.png` として linux の tar.gz に入れ、`install.sh` が `~/.local/share/icons/hicolor/256x256/apps/sorus.png` に置いて `sorus.desktop` の `Icon=` に絶対パスで書く |
| `sorus.ico` | Windows 用のアイコンファイル（手作業でショートカットなどに使うとき用） |

* 資材は外部のツール（ImageMagick・rsrc・windres など）を使わず、Go と `golang.org/x/image` だけで作ります（`build/icons/gen`）。CI・Linux でも同じバイト列になり、`release.sh` はコミット済みの資材をそのまま使います。資材が無いときは警告を出してアイコンなしで作ります。
* アイコンの入っていない古い版のアーカイブを `install.sh` で入れたときは、前に入れたアイコンがあればそれを使い、無ければ設定しません（OS の標準のアイコンになるだけで、インストールは続けます）。
* `go build` や `make dev` で手元で作った実行ファイルにはアイコンは入りません（`.syso` を置くのは `release.sh` だけです）。
* 試験: `build/icons/gen/gen_test.go`（コミット済みの資材が元画像と一致すること、`.ico`・`.icns` の形式、`.syso` の構造、Windows 向けの小さなプログラムをクロスビルドしてリソースが入ること）、`install/install_test.go`（`TestReleaseScriptAppIcons`・`TestInstallShLinux`・`TestInstallShMacAppAssembly`・`TestInstallPs1`）。

**独自のアイコンに差し替える手順**

1. 正方形の PNG を用意します（1024x1024 を推奨。256x256 以上。角を透明にすると角丸などの形になります）。SVG しか無いときは、ブラウザやデザインツールで PNG に書き出します。
2. リポジトリのルートで生成ツールを実行します。元画像 `build/icons/sorus.png` を置き換え、ほかの資材をすべて作り直します。
   ```bash
   go run ./build/icons/gen -src path/to/my-icon.png
   ```
3. 小さい大きさ（16px・32px）でも見分けられるかを確かめます（`build/icons/sorus-256.png` や、Windows のエクスプローラーで `sorus.ico` を開いて見ます）。細い線や小さい文字はつぶれます。
4. `go test ./build/...` が通ることを確かめ、`build/icons/` の変更をすべてコミットします。次のリリース（`scripts/release.sh`）から新しいアイコンになります。

* 元画像だけを手で差し替えたときは、`go run ./build/icons/gen` で残りを作り直します（作り直さないと `TestCommittedIconsMatchMaster` が失敗します）。
* 既定のデザインに戻すときは `go run ./build/icons/gen -render` を実行します。`static/icon.svg` のデザインを変えたときは、`build/icons/gen/render.go` の値も合わせて変えてから実行します（`TestRenderDefaultFollowsTheSVGDesign` が食い違いを知らせます）。
* 管理画面・公開サイトのファビコンと PWA のアイコンは別です（`static/icon.svg` と、サイト設定でアップロードする画像）。

### 自分の PC でビルドして試す（はじめての人向け）
配布版と同じデスクトップ版を、自分の PC で作って動かす手順です。利用者として使うだけなら、ビルドは要りません（[インストールガイド](install_guide.md)の1行インストールを使います）。

**Windows（PowerShell）**

1. Go（1.25 以上）を入れます。入れたら PowerShell を開き直します。
   ```powershell
   winget install GoLang.Go
   go version
   ```
2. リポジトリのフォルダでビルドします。1回目は部品のダウンロードとコンパイルで数分かかり、メモリを 8GB 近く使います。ほかの大きなアプリは閉じておきます。
   ```powershell
   cd D:\Projects\cms-go   # 自分のリポジトリの場所
   $env:CGO_ENABLED = "0"
   go build -trimpath -tags nodynamic -ldflags "-s -w -X main.buildMode=desktop -X sorus/internal/version.Version=v0.0.0-local -H windowsgui" -o dist\sorus.exe .
   ```
   起動に失敗したときに理由を黒い画面で見たいときは、`-H windowsgui` を外してビルドします。
3. 普段のデータと分けて試すため、試し用のフォルダを指定して起動します。ブラウザに「はじめに」が開きます。
   ```powershell
   $env:SORUS_DATA_DIR = "$env:TEMP\sorus-try"
   .\dist\sorus.exe
   ```
   自分でビルドしたファイルには「インターネットから来た」印が付かないため、SmartScreen の警告は出ません。終えるときは、管理画面の右上の名前 ▼ から「sorus を終了」を押します。試し用のフォルダ（`%TEMP%\sorus-try`）は消してかまいません。

**WSL（メモリを増やしてあるとき）**

`.wslconfig` で WSL に 10GB 以上を割り当て、`wsl --shutdown` で再起動したあとなら、WSL でも Windows 向けを作れます。万一メモリが足りなくなっても WSL 全体が落ちないよう、コンパイルの上限を決めて並列数を1にします。
```bash
free -g   # Mem の total が 10 以上あることを確かめる
systemd-run --user --scope -q -p MemoryMax=8G -p MemorySwapMax=4G \
  env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 GOFLAGS=-p=1 \
  go build -trimpath -tags nodynamic \
  -ldflags "-s -w -X main.buildMode=desktop -X sorus/internal/version.Version=v0.0.0-local -H windowsgui" \
  -o dist/sorus.exe .
```
上限で止まると `Terminated`（終了コード 143）と出て、ビルドだけが止まります。Intel の Mac 向けは `GOOS=darwin GOARCH=amd64` で作れます（`-H windowsgui` は外します）。Apple Silicon の Mac 向け（`darwin/arm64`）は、`-gcflags=github.com/gen2brain/webp=-N` を付けたときだけ作れます（付けないと 8GB を超えます）。

---

## 5. 静的エクスポート（SSG）のアーキテクチャ

`sorus export [--clean] [--theme <id>] [out_dir]` と、管理画面の「サイトに反映」（`internal/server/handlers_publish*.go`）は、同じ書き出し処理（`internal/server/export.go`）を使います。Echo のルーティングと Go 標準の `net/http/httptest` を組み合わせて動作します。

1. **コンテンツ走査**:
   * SQLite から公開中の全記事（`status='published'` かつ `published_at <= NOW()`）およびカテゴリを取得。
2. **内部リクエスト実行**:
   * サーバー起動時と同一のレンダリングパイプラインを通り、`httptest.ResponseRecorder` 経由で完全な HTML/XML を取得。
   * トップページ、各記事（`articles/<slug>/index.html`）、固定ページ（`<slug>/index.html`）、カテゴリ一覧、`feed.xml`、`sitemap.xml`、`robots.txt`、`llms.txt`、PWA 用の `manifest.json` と `sw.js` を出力。
   * 記事一覧・カテゴリ・トップの新着は、2ページ目以降も `<一覧のパス>/page/N/index.html` に書き出す。
   * サイト内検索は、検索用インデックス `search-index.json`（本文は先頭 2000 文字まで）をブラウザで絞り込む検索ページ（`/search/`、`static/js/static-search.js`）にする。
   * 静的出力では閲覧数を記録しない。
   * サイト基本設定（サイト名・説明・公開 URL・著作権表記・アクセス解析タグ）は DB の `settings` テーブルから読むため、管理画面で保存した値がそのまま静的出力にも反映される。
3. **静的アセット配置**:
   * 埋め込み `static.FS` から CSS/JS を `<out_dir>/static/` へ展開。
   * アップロード画像は、書き出したページ（`saveURL` で書くすべてのページ・フィード・`llms-full.txt` など）が参照するものだけを `<out_dir>/uploads/<保存先からの相対パス>` にコピーする。本文・アイキャッチとその生成サイズの `srcset`・カード・トップと本文のギャラリー・Favicon・`og:image`・JSON-LD は、どれも描画したページの中に URL として現れるため、描画と同じ判定を別に持たず、書き出すページから集める（`uploadRefs`）。参照とみなすのは、`/uploads/` の直前が URL の区切り（引用符・括弧・空白・`=`・`,`・`;` など）か、公開サイト URL（`https://`・`http://`・`//` の形）のもの。JSON の `\/` は `/` として読み、名前は `?`・`#` や区切りの前までで URL のエスケープを戻す。ほかのサイトの URL・ほかのパスの一部（`/static/uploads/` など）・保存先の外や隠しファイルを指す名前（`..`・`.` 始まり・`%2F`）は除く。下書き・校閲中・ゴミ箱・予約中の記事だけが使う画像、どこにも使われない画像は出さない（元のファイルは保存先に残る）。独自テーマのファイルは `exportCustomTheme` が記事とは別に書き出し、ここでは扱わない。コピーはページを書き出したあと（`copyReferencedUploads`）で、添付資料と同じく `os.Root` で調べてから開き、保存先で通常のファイルでないもの（途中のフォルダを含めシンボリックリンクなど・調べたあとに差し替えられたもの）と、sorus が管理する画像（メディアの行・Favicon の設定）でファイルが無いものは出さずに注意を出す。sorus が管理していない名前でファイルも無いもの（本文の説明の中の例など）は黙って除く。書き出し先の `uploads` やその中のフォルダがシンボリックリンクなどなら失敗にし、`--clean` なしで前回の書き出し先に書き出すと `uploads/` に前からあるほかのファイルは消さずに残し、残っている件数を注意に出す。
   * 添付資料は、公開サーバーがダウンロードさせるもの（`handleAttachmentDownload` と同じく `publishedCondition` を書き出しの時刻で満たす記事の資料）だけを `<out_dir>/attachments/<保存ファイル名>` にコピーする。下書き・校閲中・ゴミ箱・予約中の記事の資料、記事が無い資料、どの記事の資料でもない保存先のファイルは出さない。ページを書き出す前に対象を決め（`planAttachmentExport`）、書き出すページ・フィード・`llms-full.txt` などの中の対象の資料の URL（`/attachments/<id>/download`。資料欄・本文のリンク・JSON-LD・`citation_pdf_url`）を `/attachments/<保存ファイル名>` に書き換え、静的ホスティングでも拡張子どおりの形式で開けるようにする。拡張子が添付を許可した形式でその形式が `mime_type` と同じもの、ID と保存ファイル名が英数字・`-`・`_` だけのものに限り、それ以外・保存先の外を指す名前・無いファイル・通常のファイルでないもの（シンボリックリンクなど）は出さず、リンクも書き換えずに注意を出す。コピーはページを書き出したあと（`copyPlannedAttachments`）で、直前に記事が今も読者に見えるかを確かめ直し、途中で公開をやめた記事の資料は出さずに書き出し直すよう注意を出す。書き出し先へは `os.Root` で一時ファイルに書いてから名前を変えて置き、`attachments` や資料のファイルがシンボリックリンクなどなら書き出しを失敗にする（リンク先を書き換えない）。`--clean` なしで前回の書き出し先に書き出すと、`attachments/` に前からあるほかのファイルは消さずに残し、残っている件数を注意に出す（「サイトに反映」は前回送ったファイルの一覧との差分で消す）。
4. **入れ替えと反映**:
   * `--clean` は隣の一時フォルダに書き出してから `out_dir` と入れ替え、前回の残り（下書きに戻した記事など）を消す（`exportClean`）。
   * 一時フォルダと前回の書き出しの退避先は、`out_dir` の隣の決まった名前 `.<name>.export-new`・`.<name>.export-old`（`<name>` は `out_dir` のフォルダ名。`cleanExportSiblings`）。入れ替えは「`out_dir` → `-old`」「`-new` → `out_dir`」の2回の名前の変更で、書き出しや2回目の名前の変更に失敗したときは前回の書き出しを `out_dir` に戻す。
   * 2回の名前の変更の間でプロセスが止まると `out_dir` が無くなるが、前回の書き出しは `-old` に残る。次に同じ `out_dir` へ `--clean` で書き出すと、最初に `recoverCleanExport` が片付ける: 残った `-new` を消し、`out_dir` が無いか空のフォルダなら（空の `out_dir` を消して）`-old` を `out_dir` に戻し、`out_dir` に中身があれば入れ替えは済んでいるので `-old` を消す。触れるのはこの2つの名前のフォルダ（シンボリックリンクでないもの）と空の `out_dir` だけ。
   * 名前が `out_dir` から決まるため、同じ `out_dir` への `--clean` の書き出しを同時に2つ動かすことはサポートしない（後から始めた方が先の一時フォルダを消す）。
   * 「サイトに反映」は毎回一時フォルダに書き出し、`internal/publish` が前回のマニフェストとの差分だけを公開先へ送る（「20. 公開先と「サイトに反映」」を参照）。

---

## 6. セキュリティ設計

* **CSRF 保護**: 管理画面の全 POST/PUT/DELETE リクエストにトークン検証を強制。
* **セッション管理**: `HttpOnly`, `SameSite=Strict`, `Secure` クッキーによるセッションハイジャック防止。
* **EXIF 自動除去**: アップロードされた JPEG/PNG 画像から位置情報やカメラ情報を破棄し、WebP へ再エンコード。
* **HTML サニタイズ**: 記事本文の HTML レンダリング時、Bluemonday によりスクリプトタグ等の危険要素を無害化。
* **静的エクスポートのパストラバーサル防御**: `sorus export` 実行時、slug やカテゴリ名に `..` や不正なセグメントが含まれていないかを検証し、出力ルート外への不正なファイル書き込みを確実に遮断。また、シンボリックリンクの追従を抑止して不正な外部参照の持ち出しを防止。
* **アタックサーフェス最小化**: 公開サーバーに静的ファイル（SSG 出力）のみを配置することで、公開側の脆弱性を根本排除。
* **役割による権限**: 管理画面のルートごとに `auth.RequireRole` で許可する役割を限定（詳細は「9. 役割と権限モデル」）。

インターネット・社内 LAN に置くサーバー（`--server`・`--public-only`）向けの対策は以下のとおりです。運用で設定する環境変数は [運用保守ガイドの環境変数一覧](operations_guide.md#5-技術者向け付録-環境変数一覧) と [リバースプロキシの後ろに置く](operations_guide.md#f-リバースプロキシの後ろに置く) を参照してください。

* **待ち受けアドレス（`main.go`）**: `listenAddrFromEnv` が `LISTEN_ADDR`（無ければ `BIND_ADDR`）を `normalizeListenAddr` で `host:port` にする（ポートを省けば `PORT`。未設定ならすべてのアドレスの `:PORT`）。`AUTOCERT_DOMAIN` のときの平文ポート（`/healthz` 以外は HTTPS へ転送）は `autocertPlainAddr` が `AUTOCERT_PLAIN_ADDR`（無ければ上の待ち受けアドレス）で決める。読めない値は DB を開く前に `log.Fatalf` で起動を止める。
* **信頼するプロキシ（`TRUSTED_PROXIES`）**: `main.go` が `splitEnvList("TRUSTED_PROXIES")` を `server.Config.TrustedProxies` に渡し、`server.New` が1度だけ `ParseTrustedProxies` で解析して（不正な値はエラー）、`c.RealIP()`（`newIPExtractor`）・公開 URL のスキーム・OAuth/招待のオリジン（`advertisedOrigin`）・回数制限（`oauthClientKey`）・MCP（`mcp.Config.TrustedProxies`）で共有する。空なら転送ヘッダーを一切信頼せず、`e.IPExtractor` は `echo.ExtractIPDirect()`、スキームは TLS の有無だけで決める。接続元が一覧のプロキシのときだけ、そのプロキシが付け足した `X-Forwarded-For` の値（クライアントが書いた前の方の値は使わない。`mcp.TrustedProxies.ClientIP`）と `X-Forwarded-Proto` を使う。`internal/mcp` が環境変数を直接読む（`EnvTrustedProxies`）のは `mcp.Config.TrustedProxies` が nil のときだけ。
* **`Host` の検証（`ALLOWED_HOSTS`）**: `server.Config.AllowedHosts`（`normalizeAllowedHosts`。ポートを書いた値はポートまで、書かない値はホスト名だけで比べる）。未設定なら従来どおり `Host` を使う。設定すると、`BaseURL` が無いときの公開サイトの絶対 URL は一覧に無い `Host` を使わず先頭の値で組み立て、OAuth のメタデータ・招待コード・同期 API の MCP 案内のオリジン（`mcp.TrustedProxies.PreferredOrigin`）は一覧に無い `Host` に対して `BaseURL`（ホストが一覧にあるとき）か一覧の先頭を返し、サーバーモードの MCP は一覧に無い `Host` を 403 で断る（`mcp.Server.hostAllowed`。デスクトップモードはループバックと `MCPAllowedOrigins` のホストだけ）。`BaseURL` が無いあいだは、公開ページの HTML キャッシュに `Vary: Host` を付ける（`PublicHTMLCacheConfig.VaryHost`）。
* **ログインの回数制限（`internal/server/handlers_auth.go`, `oauth_rate_limit.go`）**: `POST /admin/login` のパスワードの誤りを、10 分の固定ウィンドウ（`oauthRateWindow`）で送り元ごと（`adminLoginFailPerClient` 30 回。IPv6 は /64 にまとめる `adminLoginClientKey`）とユーザー名ごと（`adminLoginFailPerUser` 10 回。登録されていない・形式の正しくないユーザー名は別の記録・1つのキーにまとめる）に数える。上限に達したら区切りが変わるまで、パスワードを照合せずに（bcrypt の計算をさせずに）429 と `Retry-After` で断る。正しくログインできたら、そのユーザー名の数だけ数え直す（送り元ごとの数は消さない）。
* **OAuth の回数制限（`oauth_rate_limit.go`）**: `/oauth/register`（送り元ごと 20・全体 300）、`/oauth/token`（120・3,000）、`/oauth/authorize`（`allowAuthorize`。60・600）、公開専用モードの確認コードの誤り（10・100）を 10 分ごとに数える。記録は `maxRateLimitKeys`（10,000）までで、上限に達したら全消去ではなく、区切りの過ぎた記録を消し、それでも多ければ数の少ない（同じなら古い）記録から上限の1割を空ける（`evict`。上限に達した送り元の記録は残るため、新しい送り元を大量に作って自分の記録を押し出せない）。
* **`/healthz`（`handlers_health.go`）**: 認証なしで呼べるため `{"status":"ok"}`・`{"status":"error"}`（DB に接続できなければ 503）だけを返し、`Cache-Control: no-store`。検索インデックス・予約投稿ワーカーの異常はログにだけ残す。
* **HTTP セキュリティヘッダー（`server.go`）**: `securityHeadersConfig` が echo の既定（`X-XSS-Protection`・`X-Content-Type-Options: nosniff`・`X-Frame-Options: SAMEORIGIN`）に `Referrer-Policy: strict-origin-when-cross-origin` を加え、`Config.SecureCookie` なら HSTS（`max-age=31536000`、`includeSubDomains` なし。echo は TLS か `X-Forwarded-Proto: https` の応答にだけ付ける）。CSP は3種類: 管理画面・はじめに・OAuth の同意画面（`adminCSP`。`default-src 'self'`・`object-src 'none'`・`base-uri 'self'`・`frame-ancestors 'self'`・`connect-src 'self'`。`form-action` は OAuth なら https とこの PC の http、デスクトップモードならこの PC の http も許す）、公開ページ（`publicCSP`。`base-uri 'self'; form-action 'self'; object-src 'none'`、`img-src` は `'self' data: https:` で `http:` を許さない。アクセス解析タグの配信元は `buildPublicCSP` が足す）、`/uploads/` のファイル（`uploadCSP`。`default-src 'none'` とサンドボックス。SVG を直接開いてもスクリプト・外部の読み込みが動かない）。
* **WordPress インポータの SSRF 防御（`internal/importer/media.go`）**: 画像のダウンロードは、テーマの取り込みと同じ `theme.CheckPublicAddr`（`0.0.0.0/8`・CGNAT・NAT64 に埋め込まれた私有 IPv4・マルチキャストなどを拒否）を、`net.Dialer` の `ControlContext` で DNS 解決後に実際に接続するアドレスごとに通す。`Proxy: nil`（環境変数のプロキシを経由しない）、リダイレクト先も同じく検査する。
* **バックアップ復元の Zip Bomb 対策（`internal/backup/restore.go`）**: 展開後のサイズの上限を、1エントリ `maxRestoreEntrySize`（4GB）・合計 `maxRestoreTotalSize`（8GB）とし、`inspect()` で宣言サイズを、`extractFile()`（`copyLimited` の `io.LimitReader`）で実際に読んだ量を確かめる（宣言より1バイトでも多く読めたら中止）。
* **ファビコン SVG・AI 用語集**: ファビコンの SVG は `<style>` 要素と `style` 属性を拒否する（CSS の `url()`・`@import`・エスケープで外部を読み込みうるため。`handlers_setting.go` の `safeSVGAttr` ほか）。AI 設定の用語集（`glossary`）は `maxAIGlossaryRunes`（5,000 ルーン）まで（`handlers_settings_ai.go`）。
---

## 7. AIネイティブアーキテクチャと外部LLM連携

sorus は AI ネイティブな CMS として、内部アシスタント機能と外部 LLM / エージェントとの相互運用性を備えています。

### 1. 下書き生成と推敲パイプライン (`internal/ai`)

* **プロバイダ抽象化**: `DraftGenerator`、`Proofreader`、`Suggester`、`Summarizer` などのインターフェースを通じて、Gemini（無料枠対応）、OpenAI、Claude、Ollama / ローカルLLM（OpenAI互換）を透過的に利用可能。
* **対話メモからの下書き生成**:
  * エンドポイント: `POST /admin/ai/draft`
  * パラメータ: `topic`（必須・主題）, `notes`（対話メモや要点箇条書き）, `context`（トーンや読者設定などの補足）
  * 出力: `DraftResult`（タイトル、英数字スラッグ、抜粋、Markdown本文）
  * ユーザー体験: ChatGPT や Claude との壁打ちログを貼り付けるだけで、記事の骨子と本文を一括生成し、人間が最終確認を行って公開するフローを実現。
* **管理画面からの AI 設定（`internal/server/handlers_settings_ai.go`）**:
  * `GET/POST /admin/settings/ai`（管理者のみ）で、プロバイダー（`gemini`・`openai`・`claude`・`custom`）・API キー・モデル・ベース URL（`custom` のときだけ保存）を `settings` テーブルの `ai_config`（JSON）に保存する。`POST /admin/settings/ai/test` は入力中の設定でタイトル提案を1回実行し、結果と応答時間（`elapsed_ms`）を JSON で返す。`POST /admin/settings/ai/reset` は保存した設定を消し、起動時に環境変数から作ったプロバイダー（`server.Config.AI`）に戻す。
  * キーの優先順位は「画面で保存したキー > 環境変数のキー > 未設定（`ai.Dummy`）」（`resolveAIConfig`）。キー欄が空なら、同じプロバイダー（`custom` は同じベース URL）の保存済みキーを引き継ぎ、無ければ環境変数のキーを使う。別のプロバイダー・別のベース URL には保存済みのキーも、別の送り先向けの環境変数のキーも送らない。どこにもキーが無い設定は保存しない（AI が使えなくなる事故を防ぐ）。
  * 使うプロバイダーは `Server.aiCur`（`atomic.Pointer`）に持ち、`aiProvider()` で読む。要求ごとに1度だけ読み、保存で差し替えても処理中の要求と競合しない。MCP の `cms_proofread` も `mcp.Config.AISource` 経由で同じプロバイダーを使う。
* **エラーの扱い**: Google の API は無効・未認可のキー（`AQ.` で始まる Auth キーの紐付け待ちを含む）に HTTP 400（`API_KEY_INVALID`・`FAILED_PRECONDITION` など）を返すため、`ai.IsAuthError` が 401・403 に加えてこれらもキーの問題と判定し、画面には「AIのAPIキーが無効または未認可です」とだけ表示する。API の生のエラー（`APIError` の status・reason・message）は Echo のログ（レベル INFO。デスクトップモードでは `sorus.log`）にだけ残す。
* **Gemini の既定モデル**: `gemini-3.8-flash`。404 のときだけ `gemini-3.5-flash-lite` で再試行する。キーは形式で弾かず `X-Goog-Api-Key` ヘッダーで送る（URL のクエリには載せない）。
* **MCP の接続先の表示**: 「MCP連携」の画面の URL は、公開サイトの URL（`BASE_URL`）ではなく、要求が届いたオリジン（`mcpEndpointURL`）から組み立てる。公開先（静的配信・公開専用モード）には `/api/mcp` が無く、デスクトップモードの複数サイトはサイトごとにポートが違うため。

### 2. llms.txt / llms-full.txt サポート

AI クローラーや外部 LLM がサイト全体のコンテンツ構造を効率的に把握できるよう、`llms.txt` 規格を標準サポートしています。

* **エンドポイント**:
  * `GET /llms.txt`: サイトの基本情報と、公開記事のタイトル・URL・要約・`llms-full.txt` へのリンクを含むインデックス。
  * `GET /llms-full.txt`: 公開中の全記事の完全な Markdown 本文、公開日時、カテゴリ、URL を含むストリーミングテキスト。
* **SSG 静的エクスポート連動**:
  * `sorus export` 実行時、`dist/llms.txt` および `dist/llms-full.txt` が自動出力されます。静的ホスティング環境でも外部 LLM によるサイト全体の正確な参照・引用が可能です。

### 3. Model Context Protocol (MCP) サーバー (`internal/mcp`)

sorus は HTTP ベースの Model Context Protocol (MCP) エンドポイント（`/api/mcp` および `/api/mcp/sse`）を内蔵しています。Claude Desktop や Cursor などの MCP クライアントから、API トークン（Bearer 認証）を用いて対話しながら安全に CMS 操作を行えます。

* **パス（`internal/server/mcp_path.go`, `main_mcp_path.go`）**: デスクトップモード・サーバーモードは既定 `/api/mcp`（`mcp.DefaultBasePath`）。`MCP_API_PATH` を指定するとそのパスにし、既定の `/api/mcp` は 404。公開専用モード（`--public-only`）は固定の `/api/mcp` を使えず（`ErrInvalidMCPAPIPath` で起動を拒否）、`MCP_API_PATH` が無ければ署名鍵と同じディレクトリの `mcp_path.txt`（`0600`）から読み、無ければ `NewMCPAPIPath()` で `/api/mcp-<32桁の16進数>`（128 bit の乱数）を作って保存する。同期 API のパスとの重複・包含はエラー。秘匿パスのときは共通の `/.well-known/oauth-protected-resource`（と `/api/mcp` 付き）を 404 にし、`/.well-known/oauth-protected-resource/api/mcp-<hex>` だけを登録して、未認証の要求への 401 の `WWW-Authenticate` からだけ案内する。
* **認証**: 静的 API トークン（`cmsmcp_...`）と OAuth 2.1 のアクセストークン（`soruso_...`）を、どちらも `Authorization: Bearer` で受け付ける。OAuth 2.1（`internal/server/handlers_oauth*.go`）は RFC 9728・RFC 8414 のメタデータ、Dynamic Client Registration（`POST /oauth/register`、公開クライアント。戻り先は https かループバックの http だけ）、PKCE（`S256` 必須）の認可コード（5分・1回限り）、リフレッシュトークンのローテーションと再利用の検知。トークンは SHA-256 だけを `oauth_tokens` に保存する。公開専用モードにはログイン画面が無いため、同意画面は接続した PC の「MCP連携」（同期 API の `mcp_issue_login_code`）で発行した8文字の確認コード（10分・1回限り）で本人を確かめる（`handlers_oauth_public.go`）。PC の管理画面からは同期 API の `mcp_info`・`mcp_issue_token`・`mcp_revoke_token`・`mcp_revoke_oauth_app` で公開サーバーのトークン・連携アプリを扱う（`handlers_sync_mcp.go`・`handlers_remote_mcp.go`）。
* **DNS リバインディング・Host**: デスクトップモードは `Host` をループバックと `MCPAllowedOrigins` のホストに限る。サーバーモードは `ALLOWED_HOSTS` が無ければ書式だけを確かめ、あれば一覧外を 403。`Origin` 付きの要求は許可したオリジンからだけ受け付ける。

* **提供ツール一覧**:
  * `cms_get_site_context`: サイト名、説明、カテゴリ一覧、直近公開記事一覧、`llms.txt` の場所を返し、AI にサイト全体の文脈を即座に把握させます。
  * `cms_list_articles`: 記事一覧の取得（ステータスやカテゴリで絞り込み可能）。
  * `cms_get_article`: 指定記事（ID またはスラッグ）の本文 Markdown と詳細メタデータの取得。
  * `cms_save_draft`: 記事下書きの新規作成・更新。保存完了時には編集画面の確認用 URL（`edit_url`）を返し、AI が対話上で人間に確認・公開を促すことができます。既存の文章を書き換える更新は変更案になります（下記）。
  * `cms_submit_for_review`: 編集長・デスクへの校閲申請。
  * `cms_proofread`: 記事の推敲。
  * `cms_search`: CJK バイグラム解析による記事の全文検索。
  * `cms_list_themes`: ビルトインと独自テーマの一覧と、適用中のテーマ ID（`active_theme_id`）。
  * `cms_install_theme`: GitHub・GitLab のリポジトリ URL・https の `.zip` の URL（`url`）、または Base64 の Zip（`zip_base64`）から独自テーマを取り込む（管理者のみ。適用はしない）。
  * `cms_activate_theme`: 公開サイトに適用するテーマを切り替える（`theme_id`。管理者）。
  * `cms_uninstall_theme`: 独自テーマを削除する（`theme_id`。管理者のみ。ビルトインと適用中のテーマは削除できない）。
  * テーマのツールの詳細は「8. 公開サイトのテーマと国際化（i18n）」の「独自テーマ（カスタムテーマ）」を参照。
* **記事を見られる範囲**: 管理画面と同じく、執筆チーム（ライター・編集者・管理者）は他の人の非公開の記事も一覧・取得でき、下書きなら更新・校閲申請できる（`inTeam`・`checkEditable`）。公開中・予約中・校閲待ちの記事の保護は維持し、同期 API・原稿の書き出し・画像の使用箇所はそれぞれの権限仕様に従う。
* **既存の内容を置き換える操作は変更案にする**（`internal/mcp/proposal.go`、`internal/server/handlers_proposal.go`）:
  * 既存の記事のタイトル・本文・抜粋を変える `cms_save_draft`、既存の翻訳がある `cms_create_translation`、`cms_sync_translation` は記事を変えず、settings の `article_proposal:<記事ID>` に変更案（変える項目だけ）を預けて、`needs_confirmation` と確認用の `confirm_url`（編集画面の `#article-proposal`）を返す。新しい下書きの作成と、設定（スラッグ・カテゴリなど）だけの変更はそのまま保存する。
  * 同じ記事に新しい案が届いたら前の案を置き換える。他の人が編集ロックを持っている間は案を預からない（AI を呼ぶ前に断る）。MCP には案を反映する手段を置かない。AI エージェントの承認は人の確認の代わりにならないため。
  * 編集画面は案の作成者・今の内容を保存した人（最新の版が今の内容と一致するときだけ。分からなければ「不明」）・差分を出す。「編集欄に反映」は編集欄を埋めるだけで、反映は通常の保存（ロック・公開の権限・`base_updated_at`）で行う。保存時は `proposal_id`（案の ID）と `proposal_base`（差分を出したときの記事の SHA-256 の印）を照らし、案が置き換わった・破棄された・記事が変わった（同じ秒の変更も）ときは 409 で保存せず、もう一度確かめてもらう。反映した保存の後に案を消す（ID が一致するときだけ）。
  * 翻訳の案は、訳した原文の記事 ID・言語・印（`mcp.SourceFingerprint`: 記事 ID・言語・タイトル・本文の SHA-256）を `source_article_id`・`source_lang`・`source_base` に残す（`source_lang` の無い古い案は確かめられない＝unknown）。編集画面では今の原文と比べ、案を作った後に原文が変わった・別の原文から作られた（changed）、記録が無い・原文が見つからない・読めない（unknown）ときは反映する前に警告し、反映するかは利用者に任せる。changed・unknown の案を反映して保存（校閲申請も）すると要更新（`is_outdated`）の印を外さずに立て、今の原文から作られた案（current）なら外す。差分を出したときの原文の印を `proposal_source` で送り、保存までに原文が変わったら 409 で保存しない。
  * 変更案には公開前の本文が入るため、公開サイト・静的書き出し・ログには出さない（バックアップには settings ごと入る）。
* **MCP クライアント設定例（HTTP / SSE トランスポート）**:
  ```json
  {
    "mcpServers": {
      "sorus": {
        "url": "http://localhost:8080/api/mcp",
        "headers": {
          "Authorization": "Bearer cmsmcp_xxxxxxxxxxxxxxxxxxxxxxxx"
        }
      }
    }
  }
  ```

---

## 8. 公開サイトのテーマと国際化（i18n）

### 公開サイトのテーマ

公開ページのテーマは、`static/themes/<id>/` の1つのディレクトリで表します。テーマを1つ足すのに触るのは、このディレクトリだけです。

```mermaid
flowchart LR
  n0["static/themes/#lt;id#gt;/"]
  n0 --> n1["theme.json<br/>名前・説明・配色モード・並び順"]
  n0 --> n2["theme.css<br/>デザイントークンの上書きと、公開クラスへの部品の上書き"]
```

テーマの一覧は、埋め込みの `static.FS`（`static/embed.go` の `go:embed themes`）からカタログ（`internal/theme` の `Builtin`）が起動時に作ります。ディレクトリを足して `go build` すれば、管理画面の選択肢・`sorus export --theme`・同期 API のすべてに現れます。`internal/theme/theme.go` や管理画面のテンプレートに ID を書き足す必要はありません。

### 新しいテーマを追加する手順

1. `static/themes/<id>/` を作ります。`<id>` は `^[a-z0-9][a-z0-9_-]{0,40}$` の形式にします（例: `paper`）。ID は画面には出ません。
2. `theme.json` を書きます（次の「theme.json」を参照）。
3. `theme.css` を書きます（「トークン」「部品の上書き」「禁止事項」を参照）。
4. `go test ./internal/theme` を実行します。約束を満たさないテーマは、違反の内容を示して失敗します。
5. `go run .` で起動し、管理画面の「外観」の「テーマ」タブで確かめます（「管理画面でのプレビュー」を参照）。
6. 最後に `.navigator/verify.sh` を実行して exit 0 を確認します。

### theme.json

```json
{
  "name":        { "ja": "ペーパー", "en": "Paper" },
  "description": { "ja": "白い紙面のような落ち着いたデザイン", "en": "A calm, paper-like design" },
  "color_scheme": "light dark",
  "order": 40
}
```

| 項目 | 型 | 必須 | 内容 |
| --- | --- | --- | --- |
| `name` | `{"ja": ..., "en": ...}` | `ja` は必須 | 管理画面に出す名前。`en` が無い場合は `ja` を使う |
| `description` | `{"ja": ..., "en": ...}` | `ja` は必須 | 管理画面のカードに出す説明文。`en` が無い場合は `ja` を使う |
| `color_scheme` | 文字列 | 必須 | `"light"`・`"dark"`・`"light dark"` のいずれか。`<meta name="color-scheme">` と CSS の `color-scheme` に使う |
| `order` | 整数 | 任意 | 管理画面での並び順。小さい順に並ぶ。同じ値なら ID の順 |

* 知らない項目があるとエラーになります（`DisallowUnknownFields`）。JSON の後ろに余分なデータがある場合もエラーです。
* `theme.css` が無いテーマもエラーになります。
* `modern`（`Default`）は必ずカタログに含まれている必要があります。`modern` を消すと起動時に panic します。
* `theme.json` にも `theme.css` にも絵文字は使いません（`templates/emoji_test.go` の検査対象）。
* テーマごとのテンプレート（`templates/public/` の別ファイル）や、テーマ ID による分岐は作りません。HTML の構造は全テーマ共通で、テーマが変えられるのはトークンと公開クラスだけです（「18. 固定ページとサイト構成」の「4つの層」を参照）。

### トークン

テーマが変えるのはデザイントークン（CSS 変数）です。`static/css/public.css` の `:root` に全トークンの既定値があり、テーマはそのうち必要なものを `theme.css` の `:root` で上書きします。次の19個は**必須**で、`theme.css` の `:root` に必ず書きます。省略すると `missing --accent-fg in :root` のように失敗します。

| 分類 | トークン | 意味 |
| --- | --- | --- |
| 色 | `--bg` | ページの背景色 |
| 色 | `--surface` | 目次・検索欄などの面の色 |
| 色 | `--fg` | 本文の文字色 |
| 色 | `--muted` | 補助文字の色（カテゴリ、リード文） |
| 色 | `--border` | 境界線の色 |
| 色 | `--accent` | リンク・ボタン・バッジの強調色 |
| 色 | `--accent-fg` | アクセント色の上に載せる文字の色 |
| 色 | `--accent-soft` | 控えめな強調の背景色（バッジ、カテゴリのホバー） |
| 色 | `--mark` | 本文中のマーカー（`mark`）の色 |
| 色 | `--code-bg` | 記事のコードブロックの背景色 |
| 色 | `--code-fg` | 記事のコードブロックの文字色 |
| 書体 | `--font-body` | 本文の書体 |
| 書体 | `--font-heading` | 見出しの書体（既定値は `--font-body`） |
| 書体 | `--font-mono` | 等幅の書体（インラインコードなど） |
| 形・幅 | `--radius` | 入力欄・ボタン・コードなど小さい要素の角丸 |
| 形・幅 | `--radius-lg` | カード・目次・ヒーロー画像などの角丸 |
| 形・幅 | `--content-width` | ページ全体の最大幅 |
| 形・幅 | `--prose-width` | 記事本文の最大幅 |
| 形・幅 | `--shadow` | カードなどの影 |

```css
:root {
  --bg: #fbf8f1; --surface: #ffffff; --fg: #2b2a27; --muted: #77716a; --border: #e4ded2;
  --accent: #8a4b2a; --accent-fg: #ffffff; --accent-soft: #f5e9dc; --mark: #f6e27a;
  --code-bg: #2b2a27; --code-fg: #f3efe6;
  --font-body: "Hiragino Mincho ProN", "Noto Serif JP", serif;
  --font-heading: var(--font-body);
  --font-mono: ui-monospace, Menlo, Consolas, monospace;
  --radius: 4px; --radius-lg: 8px; --content-width: 1080px; --prose-width: 720px;
  --shadow: none;
}
```

テーマで書く値は、テーマの見た目に合わせて自由に決められます。本体は、トークンの値が何であってもレイアウトが崩れないように作っています。

### ダークモード

`color_scheme` が `"dark"` または `"light dark"` のテーマは、**トップレベル**の `@media (prefers-color-scheme: dark)` の中の `:root` で、色のトークン（上の表の「色」の11個）も定義します。書体・形・幅は明るいときの `:root` だけで足ります。

```css
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #1b1a17; --surface: #252420; --fg: #ece8df; --muted: #a39d93; --border: #3a372f;
    --accent: #e0a27f; --accent-fg: #1b1a17; --accent-soft: #3a2a20; --mark: #6b5a16;
    --code-bg: #111010; --code-fg: #ece8df;
  }
}
```

`color_scheme` が `"light"` のテーマには、ダークモードの定義は要りません。

### 部品の上書き（公開クラス）

テンプレートの部品を `theme.css` で変えてよいのは、公開テンプレートで使っている**公開クラス**に限ります。現在の公開クラスは次のとおりです。

`.site-header` `.site-title` `.page-title` `.article-title` `.card-grid` `.card-title` `.card-body` `.prose` `.toc` `.badge` `.category-nav` `.hero` `.lead` `.related-article-card` `.page` `.site-menu` `.front-hero` `.front-cards` `.front-card` `.front-news`

固定ページ（`.page`）・サイト構成（`.site-menu`・`.front-*`）の部品は、`public.css` の `@layer sorus-base` がトークンだけで既定の見た目を持つため、上書きのないテーマでも崩れません。

* `h1`・`h2`・`h3`・`code`・`pre`・`blockquote`・`body`・`a` のような要素セレクタで始まる規則は書けません。ページ全体に効いてしまい、テンプレートの構造に依存するためです。`.prose h2`・`.prose pre`・`.prose a` のように公開クラスの下に限ってください。
* テストは、これらの要素セレクタから始まる規則を検出します（`selector "h1" starts with a bare element`）。
* 部品の上書きで新しい色や寸法を書くと、トークンの約束が崩れます。色・角丸・幅はできるだけトークンで表してください。

```css
.prose h2 { font-size: 1.25rem; border-left: 4px solid var(--accent); padding-left: .6em; }
.site-title::before { content: "> "; color: var(--accent); }
```

`<html data-theme="<id>">` が出力されるため、テーマ CSS の中で `[data-theme="<id>"]` を使うこともできます。

### カスケード（レイヤー）

`public.css` は全体を `@layer sorus-base { ... }` で囲んでいます。レイヤーに入っていない `theme.css` の指定は、レイヤー内の指定より常に優先されます。そのためテーマでは、セレクタの詳細度を上げる必要がありません。`theme.css` を `@layer` で囲むと、この優先順位の前提が崩れるため書かないでください。

### 禁止事項

* `@import` は使いません（テストで検出）。
* 外部のリソースを読む `url()`（`http:`・`https:`・`//` で始まるもの）は使いません（テストで検出）。シングルバイナリでオフラインでも動かすためです。フォントは OS のフォントを使ってください。
* 絵文字は使いません。

### テストによる検査

| テスト | 検査の内容 |
| --- | --- |
| `TestBuiltinThemesHonorContract`（`contract_test.go`） | 埋め込まれた全テーマの `theme.css` が、必須トークン・ダークモードの定義・禁止事項・公開クラスの約束を満たすか。新しいテーマも自動で対象になる |
| `TestBaseStylesheetDefinesTokens`（`contract_test.go`） | `public.css` が `@layer sorus-base` に入り、全トークンの既定値を持つか |
| `TestContractCheckerRejectsBrokenThemes`（`contract_test.go`） | 検査器そのものが、わざと壊した CSS（`testdata/contract/`）を確かに見つけるか |
| `theme_test.go` | `theme.json` の必須項目・未知の項目・`color_scheme`・`Default` の存在・`theme.css` の有無 |

テストを skip にしたり、検査を緩めてテストを通すことはしないでください（AGENTS.md の禁止事項）。

### 管理画面でのプレビュー

「外観」の「テーマ」タブ（`/admin/settings/theme`）では、適用前に実際の公開ページで確かめられます。

* 各カードには、トップページを縮小した見本が出ます（`/admin/settings/theme/preview?theme=<id>&frame=1`）。見本は本物の HTML をそのまま縮めたものなので、CSS を変えるとすぐ反映されます。
* 「プレビュー」は新しいタブで `/admin/settings/theme/preview?theme=<id>` を開きます。`page=article` を付けると最新の公開記事を表示します（公開記事が無い場合はトップ）。ページ上部のバーから「このテーマを適用」もできます。
* プレビューは `adminOnly` です（編集者・ライターは 403）。`Cache-Control: no-store` と `X-Robots-Tag: noindex` を付け、閲覧数は数えません。`X-Frame-Options: SAMEORIGIN` を付けるため、同じオリジンの iframe でしか表示できません。
* 公開側の `?theme=` は無視されます。訪問者がテーマを変えることはできません。

### 版付きのアセット URL

テーマの CSS は、テンプレート関数 `asset`（`static.URL`）で参照します。

```
/static/themes/<id>/theme.css?v=0123456789abcdef
```

* `?v=` の値は、埋め込みファイルの内容の SHA-256 の先頭8バイト（16桁）です。初回の呼び出しで1回だけ計算します。
* `/static/` は `max-age=31536000, immutable` でキャッシュします。内容が変われば URL も変わるため、古い CSS が残りません。以前は URL が変わらないまま1年間キャッシュされ、テーマを直しても訪問者に古い CSS が届いていました。
* 公開 HTML は `s-maxage=3600, stale-while-revalidate=60` でキャッシュします。CDN を使う場合、テーマを変えてから最大1時間は古い HTML が届きます。
* Service Worker（`static/sw.js`）は URL（クエリを含む）をキーに `/static/` を保存します。`CACHE_NAME` を変えると、古いキャッシュを削除します。
* 静的エクスポートも同じ `?v=` 付きの URL を出し、`dist/static/` に同じファイルを書き出します。`?v=` は埋め込みの内容から決まるため、`dist` の CSS を手で書き換えても URL は変わりません。テーマを変えるときは再ビルドしてください。

### 保存・出力・同期での扱い

* 優先順位は `publicTheme` の1か所で決めます。文脈の上書き（`sorus export --theme`・管理画面のプレビュー）→ 保存済みの設定（`settings.theme`）→ `Default`（`modern`）の順です。
* 管理画面の保存（`/admin/settings/theme`）、同期 API（`sync_settings`）、`sorus export --theme` は、すべて `Catalog.Get` で判定します。カタログに無い ID は、どの入口でも拒否します。
* 同期 API では、公開サーバーの sorus にそのテーマが無い場合に 422 を返します。エラー文は「公開サーバーの sorus にテーマ「<名前>」がありません。公開サーバーの sorus を更新してください」です。独自テーマは `theme_package` でファイルごと送るため、この 422 にはなりません（「独自テーマ（カスタムテーマ）」の「公開サーバーへの反映」）。
* 保存済みの ID がカタログに無い場合（テーマを削除した後など）は `modern` で表示し、「外観」の「テーマ」タブに警告を出します。
* カタログから消したテーマの保存値を別のテーマに置き換えたい場合は、マイグレーションを追加します。既存の例は `internal/database/migration.go` の `schemaV7` を参照してください。

標準テーマは `modern`（既定）、`tech`、`news`、`portal`、`portfolio` の5つです。静的出力は管理画面で保存した設定を使います。一回限りの上書きには `sorus export --theme tech dist` を使います。上書きは出力だけに効き、データベースは変わりません。

### 独自テーマ（カスタムテーマ）

利用者は、ビルトインテーマのほかに、GitHub・GitLab のリポジトリや Zip から独自テーマを取り込めます。独自テーマの作り方（`theme.json`・`theme.css`・アセットの仕様と AI エージェント向けのプロンプト）は [テーマ開発仕様書](theme_development_spec.md) を参照してください。

#### 構成

| パッケージ・ファイル | 役割 |
| --- | --- |
| `internal/theme/theme.go` | カタログ。`LoadComposite(static.FS, customDir)` がビルトイン（`static/themes/<id>/`）と独自テーマ（`<customDir>/<id>/`）を1つのカタログに合成する。`ValidateThemeDir` が検証の SSOT（ビルトイン・独自・取り込みのすべてが通る） |
| `internal/theme/importer.go` | `InstallFromZip`・`InstallFromURL`・`UninstallCustomTheme`。Zip の展開・ID の決定・検証・入れ替え |
| `internal/theme/ssrf.go` | URL 取り込み用の HTTP クライアント（`newSafeClient`）と接続先の検査（`CheckPublicAddr`・`checkFetchURL`）。`CheckPublicAddr` は公開先のヘルスチェックと閲覧制限の外からの確認（`internal/server` の `publicDialer`）も使う、インターネットに公開されたアドレスの判定の SSOT |
| `internal/server/handlers_theme_custom.go` | `/custom-themes/<id>/*` の配信、管理画面の取り込み・削除（`/admin/settings/theme/install-url`・`install-zip`・`uninstall`、管理者のみ）、結果の通知 |
| `internal/server/export.go` | 静的書き出しで、使うテーマが独自テーマならそのファイルを `dist/custom-themes/<id>/` に書き出す（`exportCustomTheme`） |
| `internal/mcp/tools_theme.go` | MCP のテーマのツール（`cms_list_themes`・`cms_install_theme`・`cms_activate_theme`・`cms_uninstall_theme`） |

* 独自テーマの置き場所は `Config.ThemesDir` です。サーバーモードは環境変数 `THEMES_DIR`（既定は作業ディレクトリの `themes`）、デスクトップモードはサイトのディレクトリの `themes/`（`desktop.SitePaths.Themes`）です。
* 壊れた独自テーマはカタログに入れずに飛ばし、理由を `Catalog.Skipped()` で返します（1つの壊れたテーマでサイト全体を止めない）。ビルトインが壊れている場合だけ起動に失敗します。名前が `.` で始まるディレクトリ（取り込み中の作業ディレクトリ）は読みません。
* ビルトインと同じ ID は取り込めません（`ErrBuiltinTheme`）。同じ ID の独自テーマを取り込むと置き換えます。
* 取り込み・削除の後は `Catalog.Reload(static.FS)` でカタログを入れ替えます。取り込み・削除・MCP からの適用は `Server.themeMu` で1つずつ行います。カタログの読み出しは `Catalog` 自身の `RWMutex` で守られ、並行に読めます。
* 適用中のテーマは、管理画面からも MCP からも削除できません。保存済みの ID がカタログから消えた場合（`THEMES_DIR` から手で消したときなど）は、ビルトインと同じく `modern` で表示し、テーマの画面に警告を出します。
* 独自テーマにはビルトインの約束のテスト（`contract_test.go`。必須トークン・公開クラス）は掛かりません。取り込み時に検査するのは `ValidateThemeDir` の内容（`theme.json` の形式と、`theme.css` が `@import`・外部の絶対 URL を使わないこと、任意の `maintenance.html` が 1 MiB 以下の UTF-8 の HTML であること）だけです。トークンと公開クラスの約束は、テーマ開発仕様書で作者に求めます。
* テーマのルート直下に任意で置く `maintenance.html`（`theme.MaintenanceFile`）は、公開サーバーのメンテナンス表示の画面になります（優先順位は「公開サーバーのメンテナンス表示」の節）。`ValidateThemeDir` が `theme.NormalizeMaintenanceHTML`（サイト運用で選ぶ独自 HTML と同じ検査。空・1 MiB 超・NUL・UTF-8 でないものを拒む）に加えて HTML のタグを含むことを確かめ、本文をカタログ（`Theme.MaintenanceHTML`）に読み込みます。不正なら、`theme.css` が不正なときと同じくテーマごと検証を通しません。HTML なので `/custom-themes/<id>/` からは配信せず（`customThemeAssetTypes` に `.html` は無い）、静的エクスポートにも書き出しません。

#### 取り込みの流れ（`installZip`）

1. Zip の大きさを確かめ、全エントリを `planArchive` で調べます。1バイトも書く前に、危険なパス・シンボリックリンク・上限超えを拒否します。
2. `theme.json` の場所（Zip の直下、または唯一のトップレベルディレクトリの直下）からテーマのルートを決めます。
3. `chooseID` が ID を決めます（Zip を包むディレクトリ名 → リポジトリ名・Zip のファイル名 → `name.en`。`-main`・`-master` を除いて `^[a-z0-9][a-z0-9_-]{0,40}$` に直す）。
4. `<ThemesDir>/.install-XXXX/<id>/` に展開し、`ValidateThemeDir` で検証します。URL から取り込んだ場合は `theme.json` に `source_url` を書き足して検証し直します。
5. `StagedTheme.Commit` で `<ThemesDir>/<id>/` に入れ替えます（既存のものは作業ディレクトリの `.old` へ退避してから rename し、失敗したら戻す）。作業ディレクトリは `Close` で消します。

#### 置き換えに失敗・中断したとき

テーマの置き換えは rename 2回（前の版を `.old` へ退避 → 新しい版を配置）です。作業記録・ロック・自動の復旧は持ちません。守るのは「前の版（`.install-*/.old`）を自動では消さない」ことだけで、配置先へ戻すのは同じプロセスの中の `Commit`・`Rollback` に限ります。

* `Commit` で新しい版を置けなかったときは、同じプロセスで前の版を元の場所に戻し、配置の前のままにします。前の版も戻せなかったとき（`Commit`）と、`Rollback` が途中で失敗したときは `ErrNotRestored` を返し、`Close` は前の版を含む作業ディレクトリを消さずに残します（配置先はテーマが無いか、新しい版のまま）。エラーの文には前の版を残した場所（`the previous version is kept in <ThemesDir>/.install-XXXX/.old`）を書きます。
* 起動時の `CleanupStaleStaging`（`server.New`。カタログを読む前）は、更新から30分過ぎた `.install-*`・`.uninstall-*` を消します。ただし `.old` を含む `.install-*` は、配置先の有無にかかわらず消さず、配置先へ戻しもしません（置き換えが終わったかを推測しない。前の版の唯一の写しかもしれない）。残したことは `theme: kept <dir>: it holds the previous version of a theme (.old) ...` とログに出します。30分以内の作業ディレクトリは、別のプロセスの作業中かもしれないため中を見ません。
* プロセスが rename 2回の間で強制終了したときと、`Commit` が `ErrNotRestored` を返したときは、そのテーマは配置先に無いままです（カタログに入らず、適用中なら `modern` で表示）。PC では同じテーマをもう一度取り込めば使えるようになり、公開サーバーでは PC から「公開サーバーへ反映する」をもう一度押して揃えます（配置先が無ければ前の版の退避なしで置く）。前の版に戻したいときは、運用者が sorus を止めて `.old` を手で戻します（運用保守ガイド）。
* 公開サーバーの受け取り（`sync_settings`）では、配置したテーマと設定の確定が揃っていることを自動では保証しません。`ErrNotRestored` のときと、配置から設定の確定までの間に強制終了したときは、テーマのファイルだけが無いか新しい版になることがあります。テーマの元のデータは PC にあるため、PC から「公開サーバーへ反映する」をもう一度押して揃えます（何度押しても PC の内容で上書きするだけです）。
* 同じ `<ThemesDir>` を使う別のプロセスとの排他はありません（プロセス内は `themeMu` で1つずつ行います）。起動時の掃除はファイルを動かさないため、別のプロセスの置き換えの途中を書き換えません。
* テスト: `importer_test.go`（rename の失敗を `renameFunc` で注入し、`Commit`・`Rollback` が前の版を戻す・残すこと、エラーが前の版の場所を書くこと、`CleanupStaleStaging` が `.old` を含む作業ディレクトリを古くても消さず・戻さず、新しい作業ディレクトリに触れないこと）。

#### SSRF 防御（`ssrf.go`）

URL からの取り込みは、管理者が入力した URL をサーバーが取りに行くため、内部ネットワークへの踏み台にされないようにしています。

* URL の形: `https` のみ、ユーザー情報（`user:pass@`）なし、ホスト必須（`checkFetchURL`）。GitHub・GitLab はポートの指定やリポジトリ以外のパスを拒否し、Zip の URL（`refs/heads/main.zip`、無ければ `master.zip`）に変換します（`resolveArchiveURLs`）。
* **ダイアル時の IP 検査**: `net.Dialer.ControlContext` で、DNS 解決の後に実際に接続するアドレスごとに `CheckPublicAddr` を通します。名前の検査と接続が同じ時点で行われるため、解決と接続の間で向き先を変える **DNS リバインディング**でも内部のアドレスには接続できません。
* **遮断するアドレス**（`CheckPublicAddr`）: ループバック・プライベート（RFC 1918、`fc00::/7`）・リンクローカル（`169.254.0.0/16`。クラウドのメタデータ `169.254.169.254` を含む）・CGNAT（`100.64.0.0/10`）・このネットワーク（`0.0.0.0/8`）・予約（`240.0.0.0/4`。ブロードキャストを含む）・マルチキャスト・未指定・文書用（`2001:db8::/32`・`3fff::/20` を含む）・ベンチマーク用（`2001:2::/48` を含む）・6to4（`2002::/16`）・Teredo（`2001::/32`）・ローカル NAT64（`64:ff9b:1::/48`）などの範囲（`blockedPrefixes`）。IPv4 射影アドレス（`::ffff:127.0.0.1`）と NAT64（`64:ff9b::/96`）は埋め込まれた IPv4 を調べます。IPv6 はグローバルユニキャスト（`2000::/3`）の外をすべて断ります（IPv4 互換 `::7f00:1` などの埋め込みの形や、未割り当ての範囲を含む）。ゾーン付きのアドレス（`fe80::1%eth0` など）も断ります（`netip.Prefix.Contains` がゾーン付きのアドレスに常に false を返し、範囲の検査をすり抜けるため）。
* 環境変数のプロキシは使いません（`Transport.Proxy = nil`。プロキシ越しでは接続先を検査できないため）。
* リダイレクトは5回までで、リダイレクト先も `checkFetchURL` で検査し直します（接続時の IP 検査も掛かります）。
* 時間の上限: ダウンロード全体で2分（`fetchTimeout`）、管理画面と MCP からの取り込みは 45 秒（`themeInstallTimeout`。応答の `WriteTimeout` より短くする）。
* 本文は `MaxArchiveSize`（10MB）+1 バイトまでしか読まず、`Content-Length` が上限を超えていればその場で打ち切ります。

#### Zip Slip・Zip ボム・DoS 防御

| 対策 | 内容 |
| --- | --- |
| Zip Slip | `safeEntryName` が `..`・絶対パス・ドライブ名（`:`）・バックスラッシュ・NUL を含むエントリ名を拒否する。展開時（`extract`）にも、書き込み先が展開先の配下にあることを `filepath.Rel` で確かめ直す（二重の防御） |
| リンク・特殊ファイル | シンボリックリンクと、通常のファイル・ディレクトリ以外のエントリを拒否する。書き込みは `O_CREATE|O_EXCL` で、既存のファイルをたどらない |
| 大文字小文字の衝突 | 大文字小文字だけが違う同名のエントリを拒否する（大文字小文字を区別しないファイルシステムでの上書きを防ぐ） |
| Zip ボム | 宣言された展開後のサイズの合計を `MaxExtractedSize`（50MB）で先に判定し、展開中も実際に書いたバイト数で同じ上限を守る（宣言を偽った Zip でも止まる） |
| 数の上限 | Zip は `MaxArchiveSize`（10MB）、ファイルは `MaxArchiveFiles`（500）、ディレクトリは 1000 まで。アップロードは `fh.Size` で、MCP の `zip_base64` はデコード前の長さで先に判定する |
| 後始末 | 失敗したら作業ディレクトリごと消す（前の版を戻せなかったときは残す。起動時の掃除も `.old` を含む作業ディレクトリは消さず、戻しもしない）。削除（`UninstallCustomTheme`）も作業用の名前へ rename してから消すため、半端なテーマがカタログに残らない |

#### 配信と静的エクスポート（`/custom-themes/<id>/`）

* 公開ページは独自テーマの CSS を `/custom-themes/<id>/theme.css?v=<theme.css の更新時刻>` で読み込みます（`themeStylesheetURL`）。取り込み直すと URL が変わるため、`Cache-Control: public, max-age=3600` でも古い CSS は使われません。
* `theme.css` から相対 `url()` で読む画像・フォントの URL には版を付けられないため、`Cache-Control: public, no-cache`（`customThemeAssetCacheControl`）で返し、使う前に毎回 `Last-Modified`・`ETag` で確かめさせます（変わっていなければ 304）。同じ ID のテーマを取り込み直した・公開サーバーへ反映し直したときに、新しい CSS と古い画像が混ざりません。見つからない応答は `no-store` にし、置き換えの一瞬に届いた要求の 404 を CDN に残させません。閲覧制限で守った応答は、従来どおり `private, no-cache` になります。
* `handleCustomThemeAsset`（認証不要）は、カタログにある独自テーマのディレクトリの中の、`customThemeAssetTypes` の拡張子（`.css` `.png` `.jpg` `.jpeg` `.gif` `.webp` `.avif` `.svg` `.ico` `.woff` `.woff2` `.ttf` `.otf`）の通常のファイルだけを返します。`theme.json`・隠しファイル・`..`・シンボリックリンクを解決するとディレクトリの外になるものは 404 です。
* 応答には `Content-Security-Policy: default-src 'none'; ...; sandbox` と `nosniff` を付けます。SVG を管理画面と同じオリジンで文書として開かれても、スクリプトは動きません。
* 静的エクスポート（`sorus export`・「サイトに反映」）は、使うテーマ（`--theme` の上書き → 保存済みの設定 → `modern`）が独自テーマなら、配信と同じ判定（`customThemeFile`）を通ったファイルだけを `dist/custom-themes/<id>/` に書き出します。HTML が参照する URL と書き出す場所が一致するため、`theme.css` からの相対 `url()` もそのまま解決されます。`sorus export` は `THEMES_DIR` の独自テーマも `--theme` に指定できます。
* 公開サーバー（公開サーバー連携、`PublicOnly`）には管理画面が無いため、独自テーマは PC で取り込み、「公開サーバーへ反映する」でファイルごと送ります（次の節）。公開サーバーの `THEMES_DIR` に手で置いて再起動する必要はありません。

#### 公開サーバーへの反映（`sync_settings` の `theme_package`）

PC で適用中のテーマが独自テーマなら、`handleRemoteSettingsPush`（「外観」のテーマ・サイト構成と「公開サーバー」の画面のボタンが共通で呼ぶ `POST /admin/remote/settings/push`、管理者のみ）は、テーマのファイルを Zip にして `sync_settings` の `settings.theme_package`（`SyncThemePackage`: `id`・Base64 の `archive`・`sha256`）として送ります。公開サーバーが外部の URL へテーマを取りに行くことはありません。

| 段階 | 実装 | 内容 |
| --- | --- | --- |
| PC: 詰める | `remoteThemePackage`・`packCustomTheme`（`handlers_remote_theme.go`） | `themeMu` の中で、`ValidateThemeDir` を通し直してから、`theme.json`・ルート直下の `maintenance.html`（あれば。公開サーバーのメンテナンス表示で使う）と配信するファイル（`customThemeFile` を通る通常のファイル。静的エクスポートと同じ判定）だけを Zip にする。隠しファイル・シンボリックリンク・README は入れない。パスの順に並べ、更新時刻を固定するため、同じ内容なら同じ Zip（同じ SHA-256）になる。取り込みと同じ上限（`MaxArchiveFiles`・`MaxExtractedSize`・`MaxArchiveSize`）を超えたら送らない |
| PC: 確かめる | `checkRemoteThemeSupport` | 先に `server_info` を呼び、`custom_themes: true`（と `max_theme_package_bytes`）を確かめる。`theme_package` に対応していない公開サーバーはこの項目を無視して設定だけを変えてしまうため、対応していないと分かったら `sync_settings` を送らない |
| PC: 送る | `callRemoteWith`（`remoteThemeHTTPClient`） | 制限時間は `remoteThemePushTimeout`（3分）。PC の応答の書き込み期限も延ばし、ブラウザに結果を返せるようにする |
| 公開サーバー: 検査 | `stageSyncTheme`（`handlers_sync_theme.go`） | `id` と `theme` の一致、空・上限、`sha256` の一致を確かめ、`theme.StageZipAs` で `THEMES_DIR` の作業ディレクトリ（`.install-*`）に展開・検証する。ID は `id` に固定する（Zip の中の名前から決めない）。取り込みと同じく Zip Slip・シンボリックリンク・Zip ボム・ビルトインの ID・不正な `theme.css` を拒む。ほかの設定・サイト構成の検査が先で、どれかが不正なら展開しない。断るときは何も置かずに 422 |
| 公開サーバー: 確定 | `commitSyncTheme`・`rollbackSyncTheme` | `themeMu` の中で、設定のトランザクションに書き込んだ後、`StagedTheme.Commit` で配置（同じ ID は置き換え、前の版は退避）し、`Catalog.Reload` で読み直して、カタログに入ったことを確かめてからトランザクションを確定する。配置・読み直し・確定のどれかに失敗したら前の版に戻して読み直し、「設定とテーマは変更していません」と理由を付けた 500 を返す。前の版を戻せなかったとき（`ErrNotRestored`）は、設定は変えていないこと・このテーマのファイルが無いか新しい版になっていること・もう一度反映すれば揃うことを返す（`syncThemeNotRestoredMessage`。「変更していません」とも、自動で戻すとも言わない） |
| 公開サーバー: 応答 | `SyncSettingsResponse.theme_package` | 配置したテーマの `id` と `sha256` を返す。PC は送ったものと一致するかを確かめ、無い・違うときは「設定は反映したが、テーマの受け取りを確かめられない」と伝える |

* 同期 API は、有効なトークンの要求に限り、本文の読み込みと応答の書き込みの期限を `syncTransferTimeout`（3分）まで延ばします（`extendSyncDeadlines`）。認証前の低速な接続の占有を防ぐ `ReadTimeout`・`WriteTimeout` は変えません。
* 要求の大きさは、Zip（最大 10MB）を Base64 にした約 14MB と設定で、公開サーバーのリクエストボディの上限（`bodyLimit` = 20MB）に収まります。公開サーバーの前にプロキシを置く構成では、プロキシの上限（nginx の `client_max_body_size` など）も 20MB 以上にしてください。プロキシが 413 を返したときは、PC はその旨を案内します。
* PC の失敗の応答（`settingsPushResult`）は、`not_sent`（送っていない・公開サーバーが 4xx で断った。何も変わっていない）、`outcome_unknown`（送った後に応答が無い・読めない・理由の無い 5xx。反映されたかどうか分からない）、`partial`（設定は反映したが、テーマの受け取りを確かめられない）のどれかを立て、文言の最後に公開サーバーの状態とすることを書きます。接続・名前解決・証明書の確認の失敗は送る前の失敗として扱います（`remoteNotDelivered`）。やり直しは同じ内容で上書きし直すだけなので安全です。
* 標準テーマを適用しているときは従来どおり ID だけを送り、`server_info` も呼びません。公開サーバーの閲覧制限（Basic 認証）は `sync_settings` では変わりません。

既知の部分失敗（自動の復旧はしない。PC から反映し直して揃える）:

* 公開サーバーのプロセスが、前の版を退避してから新しい版を置くまでの間（rename 2回の間）に強制終了すると、次に起動するまでそのテーマのディレクトリが無くなります（公開ページは `modern` で表示）。前の版は `.install-*/.old` に残り、自動では戻さず、消しもしません。PC からもう一度反映すれば揃います（前の節の「置き換えに失敗・中断したとき」）。
* 配置してから設定のトランザクションを確定するまでの間に強制終了すると、テーマのファイルだけが新しい版になり、設定は前のままのことがあります。PC には応答が届かないため「反映されたかどうか分かりません」（`outcome_unknown`）と出て、もう一度反映すれば揃います。
* 置き換えの一瞬に届いたテーマのファイルへの要求は 404 になることがあります（`no-store` のため CDN には残りません）。
* 公開ページの HTML は CDN に最大1時間残るため（`PublicHTMLCacheControl`）、CDN を前に置いた構成では、古い HTML が古い `?v=` の CSS を読むことがあります。標準テーマの切り替えと同じ扱いです。

#### MCP のテーマのツール

AI エージェントは、MCP からテーマの取り込みから適用までを自律的に行えます。

| ツール | 権限 | 引数 | 内容 |
| --- | --- | --- | --- |
| `cms_list_themes` | すべて | なし | `active_theme_id` と、各テーマの `id`・`name`・`name_en`・`description`・`description_en`・`is_custom`・`source_url`・`color_scheme` |
| `cms_install_theme` | 管理者 | `url` または `zip_base64`（どちらか一方） | 取り込んだテーマの情報を返す。適用はしない |
| `cms_activate_theme` | 管理者 | `theme_id` | 設定の `theme` を保存する。公開先への反映は別途行う |
| `cms_uninstall_theme` | 管理者 | `theme_id` | ビルトイン・適用中のテーマは削除できない |

MCP サーバーは管理画面と同じカタログ（`mcp.Config.ThemeCatalog`）を共有するため、MCP で取り込んだテーマはすぐ管理画面にも現れます。エラーは管理画面と同じ種類（`ErrBlockedURL`・`ErrArchiveTooLarge`・`ErrUnsafeArchive`・`ErrBuiltinTheme`・`ErrInvalidTheme`・`ErrUnsupportedURL`）を、AI が次の行動を決められる日本語の文に変えて返します。

#### テスト

| テスト | 検査の内容 |
| --- | --- |
| `internal/theme/theme_test.go` | 合成カタログ、壊れた独自テーマの読み飛ばし、`theme.css` の `@import`・外部 URL の検出（エスケープ・コメント・属性セレクタを含む） |
| `internal/theme/importer_test.go` | Zip の構成・ID の決定・Zip Slip・シンボリックリンク・上限・置き換え・削除、`StageZipAs` の ID の固定・配置・元に戻す、rename の失敗（`ErrNotRestored` で前の版を残し、その場所をエラーに書く）、起動時の掃除が `.old` を含む作業ディレクトリを消さず・戻さないこと |
| `internal/theme/ssrf_test.go` | 遮断するアドレス範囲、URL の形、リダイレクト、ダイアル時の検査 |
| `internal/theme/maintenance_test.go` | テーマ同梱の `maintenance.html` の読み込み（BOM・前後の空白の除去、ルート直下だけ）、不正なもの（1 MiB 超・UTF-8 でない・NUL・空・HTML でない・ディレクトリ）の拒否、Zip からの取り込みでの保持 |
| `internal/server/handlers_theme_custom_test.go` | 配信の可否と応答ヘッダー、管理画面からの取り込み・削除と通知 |
| `internal/server/export_test.go` | 独自テーマの静的書き出し |
| `internal/server/remote_custom_theme_test.go` | 公開サーバーへの反映（Zip・URL から取り込んだテーマの転送、同じ ID の更新、標準テーマの従来動作、権限、壊れた・上限超え・危険なアーカイブ、同時の反映、独自テーマに対応していない公開サーバー、通信の失敗、閲覧制限が有効な公開サーバー） |
| `internal/server/theme_replace_failure_test.go` | 中断のあとの起動で前の版を残し自動では戻さないこと、公開サーバーでテーマが無くなったあと PC から反映し直すと揃い、起動し直しても残った前の版と反映したテーマがそのままであること、`ErrNotRestored` の文言（公開サーバー・管理画面。再起動で戻るとは言わない） |
| `internal/server/remote_push_confirm_test.go` | 3画面の確認ダイアログの文言（独自テーマ・標準テーマ、日本語・英語） |
| `internal/mcp/tools_theme_test.go` | MCP のテーマのツールと権限 |

### 国際化（i18n）

管理画面の文言は `internal/i18n` の日本語・英語カタログで管理します。テンプレートでは `{{$.T "catalog_key"}}` を使い、切替設定は `sorus_lang` Cookie に保存されます。リクエストパラメータ `?lang=en` や `Accept-Language` ヘッダーによる言語検出にも対応しています。

---

## 9. 役割と権限モデル

### 4つの役割

| 役割 | DB 上の値 | 責務 |
| --- | --- | --- |
| 読者 (Reader) | なし（未ログイン） | 公開サイトの閲覧のみ。アカウントを持たず、`users` テーブルにも登録しない。 |
| ライター (Writer) | `writer` | 記事の作成、未公開（下書き・校閲待ち）記事の編集、校閲申請。 |
| 編集者 (Editor) | `editor` | 記事の全操作、公開承認・差し戻し、予約投稿、ロック強制解除。 |
| 管理者 (Admin) | `admin` | 編集者・ライターのすべての操作に加えて、ユーザー管理、サイト基本設定、外観（テーマ・サイト構成）、公開先とサイトへの反映、公開サーバーの接続、バックアップ。 |

役割は `internal/auth` の `Role` 型で表し、セッション検証（`auth.ValidateSession`）のたびに `users` テーブルから読み直す。役割の変更は再ログインなしで次のリクエストから有効になる。

`User.HasRole` は、管理者（`admin`）ならどの役割（`admin`・`editor`・`writer`）を問われても true を返し、編集者・ライターは保存上の役割だけで判定する。サーバーモードでもデスクトップモードでも同じで、デスクトップモードの「はじめに」で作るアカウントは管理者になる。権限の判定は必ず `HasRole` を通し、`Role` を直接比べないこと。アプリ版かどうかで変える表示（更新のお知らせ・「sorus を終了」など）は、役割ではなく `Config.Desktop`（テンプレートでは `.Desktop`）で判定する。

### ルートへの適用

`internal/server/server.go` の `routes()` で、ルートごとに次の3種類のミドルウェアを付ける。新しい管理画面のルートを追加するときは、どの役割の職務かを決めてから必ずいずれかを付けること。管理者はどのミドルウェアも通る（`User.HasRole`）。

| ミドルウェア | 許可する役割 | 主な対象 |
| --- | --- | --- |
| `authorOnly` | `editor`, `writer`（と `admin`） | 記事の作成・編集・削除、プレビュー、画像・添付資料、版の復元、AI 執筆支援、校閲申請 |
| `editorOnly` | `editor`（と `admin`） | 公開承認（`approve-publish`）、差し戻し（`reject-review`） |
| `adminOnly` | `admin` | `/admin/users`、`/admin/settings/general`・`theme`・`structure`・`backup`・`remote`、`/admin/publish` 以下、招待の発行・取り消し（`/admin/remote/invites`・`/admin/remote/tokens`）、サイト設定の反映（`/admin/remote/settings/push`） |

「届いた原稿」（`/admin/manuscripts`）と公開サーバーの校閲待ち（`/admin/remote/pending`）は `editorOnly` です。設定の入口（`/admin/settings`、`settingsHubFor`）は、各画面と同じ役割の条件でカードを出し分けます。

ロックの強制解除（`force-unlock`）は `admin` と `editor` に許可し、解除後は編集画面へリダイレクトする。

ルート単位の制御に加えて、ハンドラー内でも記事の状態に応じた検査を行う。公開中・予約中の記事の変更・削除・添付資料の変更・版の復元は `canPublish`（編集者と管理者）で判定する。テンプレートはこれらの判定結果に合わせてボタンやリンクを出し分けるが、権限の根拠は常にサーバー側の検査であり、表示の出し分けだけに頼らないこと。

### ユーザー管理の不変条件

`internal/auth/user.go` が次の条件をモデル層で保証する（HTTP ハンドラーを経由しない呼び出しでも破れない）。

* 自分自身のアカウントは削除できない（`ErrCannotDeleteSelf`）。
* 管理者が1人しかいないとき、その管理者の削除・降格はできない（`ErrCannotDeleteLastAdmin`）。件数の確認と操作を条件付きの1つの SQL 文で行うため、同時に操作しても管理者は0人にならない。
* 記事の `author_id` として参照されているユーザーは削除できない（`ErrUserHasArticles`）。
* パスワード変更時・ユーザー削除時は、そのユーザーのセッションと MCP トークンを1つのトランザクションで破棄する（どれかが失敗したら何も変えない）。

### テスト

権限の結合テストは `internal/server` にある。役割ごとの 403 を確認するテストを、機能追加のたびに併せて追加すること。

* `server_test.go`: 管理者が記事の作成・編集・削除・承認・差し戻しをできないこと、編集者が記事を管理でき、サイト設定・ユーザー管理はできないこと。
* `handlers_user_test.go`: ユーザー管理の一覧・作成・役割変更・パスワード変更と、自己削除防止・最後の管理者保護・記事所持ユーザーの削除保護、管理者以外の 403。
* `handlers_setting_test.go`: サイト基本設定の保存、公開ページと静的エクスポートへの反映、再起動後の引き継ぎ、管理者以外の 403。

---

## 10. サイト基本設定の保存と反映

`/admin/settings/general` で保存した値は `settings` テーブル（キー・値）に格納する。

| キー | 反映先 | 読み出し |
| --- | --- | --- |
| `site_name`, `site_description`, `base_url` | `<title>`、OGP、フィード、canonical URL | 起動時に `New` が `Config` に上書きし、保存時にも更新する。環境変数（`SITE_NAME` 等）より DB の値を優先する。 |
| `copyright` | 公開ページのフッター | 公開ページの描画ごとに DB から読む。 |
| `analytics_tag` | 公開ページの `<head>` | 公開ページの描画ごとに DB から読み、エスケープせずに出力する（管理者だけが設定できる信頼済みの HTML として扱う）。 |
| `multilingual_enabled` | 英語のページ（`/en` 以下）・言語切り替え・hreflang、管理画面の翻訳機能 | `isMultilingualEnabled`。保存していなければ英語の記事（ゴミ箱以外）があるかで決める。OFF なら `/en` 以下は 404（`requireMultilingual`）、静的な書き出しも英語のページを出さない。 |

公開サーバーへの反映（「公開サーバーへ反映する」、同期 API の `sync_settings`）では、`remoteGeneralSettings()` の値（サイト名・説明文・著作権表示・アクセス解析タグ・公開サイト URL（この PC の中を指すなら送らない）・`MultilingualEnabled`）を送る。サイト基本設定の保存の前後で `remoteGeneralSettings()` が変わったときだけ「公開サーバーへ未反映」として案内する（閲覧制限・Favicon など送らない設定だけの変更では出さない）。`SyncSettings.MultilingualEnabled` は `*bool` で、この項目に対応していない公開サーバーは無視する（応答の `Settings.MultilingualEnabled` が nil）。

静的エクスポートは同じ描画パイプラインを通るため、これらはすべて `sorus export` の出力にも反映される。

---

## 11. VPS 1台でのスタンドアロン運用（Nginx 不要）

sorus はリバースプロキシを前提としない。1つのバイナリが次をすべて担うため、VPS 1台にバイナリと環境変数ファイルを置くだけで本番運用できる。

* **HTTPS 終端**: `AUTOCERT_DOMAIN` を設定すると `golang.org/x/crypto/acme/autocert` で Let's Encrypt の証明書を取得・自動更新し、`AUTOCERT_HTTPS_ADDR`（既定 `:443`）で TLS 配信する。`AUTOCERT_HTTP_ADDR`（既定 `:80`）は HTTP-01 チャレンジへの応答と HTTPS へのリダイレクトだけを行う。証明書は `AUTOCERT_CACHE_DIR` に保存する。
* **アプリケーション**: 管理画面・公開サイト・MCP・検索・予約投稿ワーカーを同一プロセスで提供する。
* **データ**: SQLite（`modernc.org/sqlite`、CGO 不要）1ファイルとアップロード・添付資料のディレクトリ。`sorus backup` で稼働中にバックアップできる。
* **常駐化**: `deploy/systemd/sorus.service` は専用ユーザーで実行し、80/443 の bind に必要な `CAP_NET_BIND_SERVICE` だけを付与する。

`CGO_ENABLED=0 go build` で作ったバイナリは libc にも依存しないため、Linux の VPS へコピーするだけで動作する。

リバースプロキシの後ろに置くときは、`TRUSTED_PROXIES`・`ALLOWED_HOSTS`・`LISTEN_ADDR`・`BASE_URL` を必ず設定する。ログの場所（`journalctl -u sorus`、本体のアクセスログ、リバースプロキシのログ）とあわせて、[運用保守ガイドの 3章 F・G](operations_guide.md#f-リバースプロキシの後ろに置く) にまとめている。

---

## 12. Web App Manifest と Service Worker

`/manifest.json`（Web App Manifest）と `/sw.js`（Service Worker）を `internal/server/handlers_pwa.go` が `static/` の埋め込みファイルから配信する。管理画面と公開ページのレイアウトがこれらを読み込み、Service Worker は `/static/` のアセットをキャッシュする（「版付きのアセット URL」）。どちらも認証不要のパスで、静的エクスポートにも出力する。sorus は PC のアプリ（デスクトップ版）と公開サイトの構成で使うもので、スマートフォン向けのアプリとしては案内しない。

---

## 13. 完全バックアップと安全なリストア (`internal/backup`, `internal/database`)

データベースだけでなく、アップロード画像・添付資料を含めたシステム全体の資産を完全かつ無停止で保全・復元します。

### アーキテクチャ
* **単一 Zip アーカイブ形式**: `sorus_backup_YYYYMMDD_HHMMSS.zip`（形式のバージョン 2。`backup.FormatVersion`）
  * `database.db`: SQLite `VACUUM INTO` による無停止オンラインスナップショット。ログインセッションなどの一時データは除去。
  * `uploads/`: アップロードされた画像ファイル群（WebP、元画像、Favicon等）。
  * `attachments/`: 添付資料（`ATTACHMENT_DIR`）。形式のバージョン 2 から。添付資料を含めずに作るアーカイブは、以前の sorus でも復元できるようバージョン 1 のまま作る。
  * `manifest.json`: 形式、作成日時、スキーマバージョン、DB のサイズと SHA-256、画像・添付資料の件数を記録。
  * 独自テーマ（`THEMES_DIR`）は入れない。公開サーバーのテーマは PC から「公開サーバーへ反映する」で送り直し、PC の独自テーマは取り込み元から取り込み直す（復元しても `THEMES_DIR` には触れない）。
  * 復元はバージョン 2 以下を受け付ける。添付資料を含まないバージョン 1 から復元したときは、`ATTACHMENT_DIR` をそのまま残す。
* **CLI & 管理画面 UI**:
  * コマンドライン: `sorus backup [--db-only] [--site <id>] [dest]` / `sorus restore [--yes] [--site <id>] <archive.zip>`（`--site <id>` はデスクトップモード専用。サイトが2つ以上あるときは必須）
  * 管理画面: `/admin/settings/backup`（管理者のみ）。ワンクリックでの Zip 作成・ダウンロード、ドラッグ＆ドロップによる復元。
  * デスクトップモードの「はじめに」の画面（`/admin/setup/restore`）からも復元できる（新しい PC への引っ越しや、削除したサイトの復元用）。ユーザーが1人もいないときだけ出して受け付け、古い版の取り込みで作られたユーザーがいるときは出さずに 403 で断る（そのデータを置き換えない。「はじめに」のあと `/admin/settings/backup` で復元する）。
* **自動バックアップ（デスクトップモードのみ）**: `backup.Scheduler` が、設定した保存先フォルダ（settings の `backup_auto_folder`）へ起動時と1日1回完全バックアップを作り、対象サイト（`Scheduler.Site` = `s.cfg.SiteKey`）の自動バックアップの名前のファイルだけを数えて新しい 14 世代（`DefaultKeep`）を残す（1つ目のサイトは `sorus_backup_YYYYMMDD_HHMMSS.zip`、2つ目以降は `sorus_backup_<id>_YYYYMMDD_HHMMSS.zip`）。DB の一時的な複製はサイトのディレクトリに置き、同期フォルダには載せない。保存先の候補として OneDrive・Google ドライブ・Dropbox などのフォルダを検出する（`internal/backup/folders.go`）。
* **セキュリティ（Zip Slip・Zip Bomb 防止）**:
  * 解凍前にアーカイブ内の全エントリを検査し、相対パス（`..`）やシンボリックリンクを含む危険なアーカイブの展開を厳格に遮断。
  * 展開後のサイズを1エントリ 4GB（`maxRestoreEntrySize`）・合計 8GB（`maxRestoreTotalSize`）までに限り、`inspect()` の宣言サイズと `extractFile()` の実際に読んだ量（`io.LimitReader`）の両方で確かめる。
  * 復元実行時、既存の DB と画像ディレクトリを自動で `.pre-restore-YYYYMMDD_HHMMSS` に退避（安全なロールバック担保）。

---

## 14. メディア管理 (DAM) と AI マルチモーダル解析 (`internal/image`, `internal/ai`)

アップロード済み画像を組織の資産として一元管理し、アクセシビリティ（WCAG）と検索性を最大化します。

### メディアライブラリ UI (`/admin/media`)
* **表示切り替え**: グリッドビュー（サムネイル重視）とリストビュー（詳細メタデータ重視）をワンクリック切り替え。
* **詳細モーダル**: フルサイズプレビュー、生成された各サイズ（Large 1200px / Medium 800px / Small 400px）の URL 確認、Alt テキスト・キャプションのインライン編集・保存。
* **記事エディタ連携（メディアピッカー）**: 記事執筆画面からモーダルでメディアライブラリを開き、過去の画像をワンクリックでエディタのカーソル位置へ挿入。

### AI マルチモーダル解析パイプライン
* 画像アップロード時、Gemini Vision API 等が有効であれば、画像を自動解析。
* **Alt テキスト**: スクリーンリーダーおよび SEO に最適な客観的描写を自動生成。
* **キャプション**: 記事でそのまま使える要約文を自動生成。
* **AI タグ**: 被写体・情景に応じた検索用タグを抽出し、SQLite に保存（メディアライブラリでのキーワード・タグ横断絞り込み検索を可能化）。

---

## 15. 標準ネイティブ・フォトギャラリー (`internal/markdown`, `templates/public`)

外部の重厚な JavaScript ライブラリを一切排除し、モダンブラウザ標準技術のみでフォトギャラリーを提供します。

* **Markdown 拡張記法**:
  ```markdown
  :::gallery
  ![秋のキャンパス](/uploads/photo1.webp "木々が色づくメインストリート")
  ![図書館ホール](/uploads/photo2.webp "静寂が広がる閲覧エリア")
  :::
  ```
* **HTML レンダリング**:
  * CSS Columns または CSS Grid によるレスポンシブな Masonry 配置。
  * HTML native `<dialog>` 要素 + View Transitions API による外部依存ゼロの滑らかなモーフィング拡大モーダル（ライトボックス）。
  * キーボードナビゲーション（Escape によるクローズ、左右矢印キーによる前後移動）を完全サポート。

### タグで指定する動的ギャラリー（`:::gallery tag="..."`）

* `:::gallery tag="イベント"` の次の行に `:::` を書くと、描画のたびに `media.ai_tags` にそのタグを持つ写真（`created_at` の新しい順）を展開します。出力は `<figure class="photo-gallery masonry-grid" data-gallery-tag="イベント">` で、各写真は固定のギャラリーと同じ `.gallery-item` です。該当する写真が無ければ何も出力しません。
* 動的ギャラリーは `limit="N|all"`・`sort="latest|oldest|random"`・`min_width="px"` を受け付けます。`limit` は1〜100（超える値は100）か `all`（全件）。`min_width` は元画像の幅（`media.width`）がその値以上の写真だけにします（幅の分からない写真も除く）。
* `limit` が未指定・不正値のときは最初の `markdown.DefaultGalleryPageSize`（24）枚だけを出力し、総数がそれを超えれば figure に `data-gallery-total`（総数）と `data-gallery-more`（続きの API `/api/gallery?tag=...&sort=...&min_width=...&seed=...&limit=24`）を付け、直後に `<p class="gallery-more"><span class="gallery-count">全 N 枚</span><button class="gallery-more-button">もっと見る（残り N 件）</button></p>` を出します。ボタンは Bluemonday で残せない要素のため、サニタイズの後に描画ごとの推測できない印から差し込みます。`static/js/justified_gallery.js` が `offset`（表示済みの枚数）を足して続きを読み、figure の末尾に追加して行揃えを並べ直します。`lightbox.js` は最後の写真から次へ進むと `window.sorusGalleryMore.load` で続きを読みます。
* 続きの API `GET /api/gallery`（`handlers_gallery_api.go`）は JSON（`html`・`count`・`total`・`next_offset`・`remaining`）を返します。公開中の記事・ページの `:::gallery` が使うタグだけを返し（それ以外は 404）、不正な値は 400 です。`sort=random` は種（`seed`）で決まる並べ替え（`(rowid * seed) % (2^31-1)`）にして続きが重ならないようにし、種の無い random は 400 です。
* 静的な書き出し（続きの API が無い）とプレビューでは全件を出力し、25枚目以降を `hidden` にしてボタンで24枚ずつ見せます（隠した写真の画像も書き出しに含まれます）。並び順の既定値と不正値は `latest`（新しい順）で、`oldest` は古い順、`random` はランダムです。上限を適用した結果が0件なら何も出力しません。
* タグは完全一致です。`ai_tags` は `normalizeMediaTags` で `", "` 区切りに整えて保存しているため、`(',' || replace(ai_tags, ', ', ',') || ',') LIKE '%,<tag>,%'` で区切りごとに比べます（「art」が「cart」に一致しない）。
* 写真の解決は `markdown.GalleryResolver`（`(*Server).galleryResolver`、`internal/server/handlers_dam.go`）が行います。`markdown.RenderWithGallery` に渡すのは、公開ページの本文（`loadArticleBody`）、管理画面のプレビュー（`renderPreview`）です。静的な書き出しも同じ描画を通るため、写真を足したあとに「サイトに反映」（`sorus export`）すると新しい写真が加わります。解決器を渡さない `markdown.Render` では、タグのギャラリーは中の画像だけを描画します（通常は空）。
* 管理画面の API: `GET /admin/api/media?tag=<タグ>`（`q`・`page`・`per_page` と併用可）はそのタグの写真だけを返し、`GET /admin/api/media/tags` は使われているタグの一覧 `{"tags": [...]}`（重複なし・昇順）を返します。

### 記事編集画面のギャラリー作成モーダル（`templates/admin/partials/gallery_modal.html`, `static/js/gallery_modal.js`）

* 編集画面の `data-gallery-modal-open` のボタンで `<dialog id="gallery-modal">` を開きます。閲覧専用（`ReadOnly`）の画面には出しません。
* 「写真を選んで作成（個別指定）」は `/admin/api/media` の一覧から写真を選び、並べ替え・代替テキスト・キャプションを直して、固定の `:::gallery` を本文のカーソル位置に入れます。モーダルに落としたファイルは `/admin/media/upload` へ1枚ずつ送り、選択に加えます。
* 「タグで指定（動的自動更新）」は `/admin/api/media/tags` のタグを並べ、選んだタグの写真を `/admin/api/media?tag=...` で表示します。挿入は動的（`:::gallery tag="..."`）か、いまの写真をすべて読んで（`per_page=60` で全ページ）書き出す固定のギャラリーです。`"` を含むタグは記法に書けないため、動的には挿入できません。
* 挿入後は本文の `textarea` に `input` を発火し、プレビュー（`/admin/preview`）を更新させます。プレビューの段組みは `static/css/admin.css` の `.prose-preview .photo-gallery.masonry-grid`（プレビュー欄の幅で2列・3列）です。
* 文言は `internal/i18n` の `gallery_*` のキーです。スクリプトが使う文言は `data-msg-*` 属性で渡し、`{n}` を数に置き換えます。

### メディアタグ一括変更 API (`POST /admin/api/media/bulk-tags`)

ログイン済みライター・編集者・管理者が使う、CSRF 保護された API です。リクエストは JSON で、`media_ids` は1〜100件、`add_tags` と `remove_tags` はタグ文字列の配列です。少なくとも一方のタグ配列が空でない必要があります。例:

```json
{
  "media_ids": ["media-id-1", "media-id-2"],
  "add_tags": ["campus", "春の行事"],
  "remove_tags": ["old-event"]
}
```

`media_ids` の重複は除かれ、存在しない ID は無視します。追加済みタグの重複追加は行わず、削除は完全一致するタグを外します。タグは1〜100文字で、空白を含む文字を受け付けます。制御文字、引用符、カンマ（半角・全角・読点）は使えません。処理は1つのトランザクションで行います。

成功時は `200` と `{"ok":true,"updated_count":N}` を返します。JSON 不正、件数不正、タグ不正、ID 不正は `400`、権限不足は `403` です。本文の上限は32 KiBです。CSRF トークンは他の管理画面 API と同じく `X-CSRF-Token` ヘッダーで渡します。

### テーマ・レイアウトにギャラリーを組み込む（`galleryImages`、`templates/public/partials/gallery.html`）

記事の本文の外（サイドバー・フッター・トップページなど）にタグのギャラリーを置くときは、公開テンプレートの関数 `galleryImages` と部品 `gallery` を使います。

```html
{{/* templates/public/layout.html のフッターの例。$ は公開ページのデータ（publicPage）。 */}}
<footer class="site-footer">
  <div class="container">
    {{template "gallery" (galleryImages $ "フッター")}}
    ...
  </div>
</footer>
```

* `galleryImages <公開ページ> "<タグ>" [表示上限] [並び順]` は `{Tag, Images}`（`galleryTemplateData`）を返します。1つ目の引数は公開ページのデータ（`publicPage`）です。表示上限の範囲は1〜100で、並び順は `latest`・`oldest`・`random` です。省略時は全件・新しい順です。`{{range}}` や `{{with}}` の中では `.` が変わるため、`$` を渡してください。
* `{{template "gallery" ...}}` は、写真があるときだけ本文のギャラリーと同じ `<figure class="photo-gallery masonry-grid" data-gallery-tag="...">` を出力します。写真が無ければ何も出さないので、`{{if}}` で囲む必要はありません。見出しなどを写真があるときだけ出したいときは、`{{with (galleryImages $ "フッター")}}{{if .Images}}<h2>...</h2>{{template "gallery" .}}{{end}}{{end}}` のように書きます。
* 拡大表示（`static/js/lightbox.js`）はページ内のすべての `.gallery-item` に効くため、追加の JavaScript は要りません。見た目は `static/css/public.css` の `.photo-gallery.masonry-grid`・`.gallery-item` に従い、テーマの `theme.css` でトークンと公開クラスを上書きして変えます。
* 写真の検索に失敗すると、テンプレートの実行がエラーになり、ページは表示されません（500）。
* テンプレートは全テーマ共通です（「8. 公開サイトのテーマと国際化（i18n）」を参照）。テーマ ID による分岐は作らず、どのテーマでも同じ場所に出します。

---

## 16. 分散型ローカルファースト同期と要塞化 (`internal/auth`, `internal/server`)

「中央集権管理の完全排除」「ローカルファーストの自律作業」「公開サーバーの要塞化」を実現する同期アーキテクチャです。

### 公開専用モード (`--public-only` / `PUBLIC_ONLY=true`)
* 公開サーバー上では `/admin` 以下の全ルートを無効化（404 Not Found を返却）。インターネット上に管理画面やログインフォームを一切露出させません。
* MCP サーバーは推測できない秘匿パス `/api/mcp-<32桁の16進数>`（`mcp_path.txt`）にだけ置き、OAuth 2.1 の同意は PC で発行した確認コードで行う（「7. AIネイティブアーキテクチャと外部LLM連携」の「3. Model Context Protocol (MCP) サーバー」）。

### 秘匿同期エンドポイント (Stealth Sync Endpoint)
* 同期 API は固定の `/api/sync` ではなく、推測不可能なランダムハッシュパス（例: `/api/sync-<32文字hex>`）を `sync_path.txt` に永続化して稼働。
* 固定パスへの外部からのスキャンや攻撃は 404 で門前払いされ、API の存在自体を隠蔽。

### 暗号ケーパビリティトークン (`internal/auth/capability.go`)
* 中央セッションではなく、Go 標準 `crypto/ed25519` による電子署名付きトークン（Base64URL 形式）を採用。
* CLI コマンド `sorus token create --role <writer|editor|admin> --sub <名前>` により安全に発行。`--base-url` に `https://` の URL を渡すと招待コードも標準エラー出力に表示する。発行したトークンは公開サーバーの DB に記録し、取り消せるようにする。

### 招待コードと招待の発行・取り消し (`internal/server/handlers_remote*.go`)
* 招待コードは `sorus-invite:` ＋ Base64URL（パディングなし）の JSON `{"v":1,"url":"<同期 URL>","token":"<トークン>"}`。ローカル CMS の「設定」→「公開先」（`/admin/settings/remote`）に貼り付けて「接続を確かめて参加する」（`/admin/settings/remote/join`）で参加する。URL とトークンを別々に入れる「詳細設定」も残している。
* サイト管理者（admin のトークン）は、画面から招待の発行（同期 API の `issue_invite`）・一覧・取り消しができる。取り消したトークンは公開サーバーの失効リストで拒否する。最後の管理者の招待は取り消せない。
* `issue_invite` による発行は1時間に30件（`maxInvitesPerHour`）まで。上限の確認と `sync_tokens` への記録は、条件付きの1つの `INSERT ... SELECT ... WHERE (SELECT COUNT(*) ...) < ?` で行う（`internal/server/handlers_sync_tokens.go` の `recordInvite`）。SQLite の書き込みの文は読む前に書き込みのロックを取るため、同時に多数の発行が来ても（別のプロセスからでも）上限を超えて記録しない。記録されなかった（上限に達した）ときは招待を渡さず 429 を返す。
* 公開サーバーの接続設定は1つのサイト全体の設定なので、サーバーモードでは管理者だけが変更できる（`adminOnly`）。デスクトップモードでは実効的な役割で通る。
* 公開サーバーの導入は `deploy/install-server.sh`（DNS の確認、サーバー版のダウンロードと SHA256 検証、`/etc/sorus/sorus.env`、systemd、ufw、管理者用の招待コードの発行）。

### 公開サーバー自身の閲覧制限 (`internal/server/handlers_sync_site_access.go`, `handlers_remote_site_access*.go`)
* 同期 API の `get_site_access`・`set_site_access`（admin のトークンのみ）で、公開サーバー自身の Basic 認証を確認・変更する。PC の「設定」→「サイト運用」（`/admin/remote/site-access`）がこれを呼ぶ。変更は `expected_revision` で競合を検出する。接続した側の「サイト設定」の閲覧制限とは別の設定。
* **動作モードの区別は `Config.Desktop` だけで行う**（`internal/server/basic_auth.go` の `basicAuthApplies`）。
  * デスクトップモード以外（サーバーモード＝管理画面つき。同期 API の有無を問わず、手元の `make dev`・`go run .` も含む。公開専用モード）: `basic_auth_*` の settings で公開サイトを守る。サーバーモードの「サイト設定」（`generalSettings`・`handleGeneralSettingsSave`）は閲覧制限の表示・有効化・変更・解除と、`basic_auth_revision` による版の食い違いの検出（409）を v0.2.8 と同じく行う。同期 API を有効にしたサーバーでは `set_site_access` と同じキー・同じ版を共有する。
  * デスクトップモード: 閲覧制限は無い。`basicAuthMiddleware`・`basicAuthActive` は働かせず、「サイト設定」は項目を出さず（`generalSettingsData.BasicAuth` が false）、保存は `basic_auth_*` のフォーム値と settings のキーを読み書きしない（古い画面の送信は無視し、409・422 で止めない）。v0.2.8 の「サイト設定」で保存した `basic_auth_*` は消さずに残すが働かない。
  * 画面の案内もモードで切り替える。「サイト設定」の導入文は `general_settings_intro`（デスクトップ）・`general_settings_intro_access`（サーバーモード）。「公開サーバー」画面の閲覧制限の案内は `remoteSettingsData.LocalBasicAuth` で `remote_access_separate_note`（サーバーモード: この sorus の「サイト設定」の閲覧制限と互いに独立）と `remote_access_separate_note_desktop`（デスクトップ: 公開サーバーに保存される設定で、存在しない「サイト設定」の閲覧制限は案内しない）を選ぶ。日英とも `TestSiteSettingsIntroMatchesMode`・`TestRemoteSiteAccessNoteMatchesMode`・`TestServerSiteSettingsExplainsIndependentRemoteAccess` で確かめる。
  * 試験: `basic_auth_test.go` の `TestDesktop*`（デスクトップ）、`TestServerModeKeepsSavedBasicAuthAfterUpdate`（更新前の設定を持つサーバーモード）、`TestPublicServerKeepsSavedBasicAuthAfterUpdate`・`TestPublicOnlyServerBasicAuthProtectsSite`（公開サーバー）。更新前の DB は `seedBasicAuth` で作る。
* **閲覧用のパスワードは、sorus が TLS を直接受けた接続でだけ受け付ける**（`secureSyncRequest`: `r.TLS != nil && r.TLS.HandshakeComplete`）。それ以外は 403 `site_access_insecure` で、何も変えない。`X-Forwarded-Proto`・`Forwarded`・Host は送り手が決められるため使わない。この PC 内（ループバック）からの平文の接続も受け付けない。同じマシンのリバースプロキシ（nginx の `proxy_pass http://127.0.0.1:PORT;` は Host を上流のアドレスにし、転送ヘッダーを付けない）が中継した接続と見分けられないため（sorus を `127.0.0.1` だけで待ち受けても同じ）。本番は `AUTOCERT_DOMAIN` で sorus 自身が TLS を受ける（`deploy/install-server.sh` の構成。平文の `PORT` は `/healthz` のほかを https へ転送する。`newPlainHealthCheckHandler`）。
* 応答の `insecure_transport` は、その接続を sorus が TLS で直接受けていないことを示す。PC は保存の前に `get_site_access` で確かめ、`insecure_transport` なら `set_site_access` を送らない。公開サーバーの URL が `https://` でないとき（`http://localhost` を含む）も送らない。どちらの場合も、画面の状態に `can_save: false` と理由（`save_blocked`）を入れ、画面は保存のフォームを使えなくする。保存を断ったときの応答は `blocked: true` の 422。
* 失敗の案内は段階（`siteAccessPhase`）で分ける。送る前の確認で失敗したら「送っていません」と伝え、送った後に応答が無い・読めないときは「反映されたかどうか分かりません」（`outcome_unknown`）と伝える。
* **閲覧制限・メンテナンス表示の対象外は共通の `siteGateExempt`（`basic_auth.go`）だけ**: 同期 API（`Config.SyncAPIPath` への POST）、管理画面と MCP サーバー（`/admin`・`/api/mcp`。**管理画面のあるサーバーモードだけ**）、`/static/`（GET/HEAD）・`/manifest.json`・`/sw.js`（GET/HEAD）・`/healthz`（GET）、静的エクスポートとテーマのプレビューの内部要求。対象外にするのはルーティングで実際にそれらの処理に届く要求だけ（`siteGateExemptPath`）: ルートが 1 つのパスは登録したメソッドで、正規化しないパスがそのまま一致するときだけ。`/static/`・`/admin`・`/api/mcp` は、正規化しないパス（ルーティングが見る）と `path.Clean` したパス（処理が見る）のどちらもその下のときだけ。`Path` と `RawPath`（ルーティングは `RawPath` があればそれで行う）のどちらも対象外のときだけ対象外。`/healthz/`・`//healthz`・`/./healthz`・`//static/...`・`/api` の下の存在しないパス・同期 API のパスへの GET や `/` をつけた POST・公開専用モードの `/admin`（`PublicGuardMiddleware` が公開サイトの 404 ページにする）・デスクトップモードにしか無い `WindowPath` のようにルートが合わない要求は、`/:slug` などに届いて公開サイトの 404 ページ（テーマ・サイト名・メニュー）になるため、パスワードを求める（秘匿パスの有無も見せない）。管理画面にログイン中の編集部員（`isStaff`）は今までどおり通す。試験は `TestSiteGateExemptPath`・`TestBasicAuthExemptsOnlyRoutedPaths`・`TestMaintenanceExemptsOnlyRoutedPaths`。
* 閲覧制限中の `/uploads/` は、同期 API と同じ署名付きトークン（`Authorization: Bearer`）でも読める（PC のプレビューの画像の取得。`syncTokenReadsUpload`）。画像のルートに届く GET・HEAD（正規化しないパスと正規化したパスのどちらも `/uploads/` の下）だけで、`//uploads/...` などは公開サイトの 404 ページになるため認めない。PC の同期 API の呼び出しが閲覧制限の 401（`WWW-Authenticate: Basic`）を受けたら、同期 API に届いていない（URL のパスの誤り）として「同期 API が見つかりません」と案内する（`callRemoteWith`。`TestRemoteTestWithoutSyncPathBehindBasicAuth`）。初めて使われたトークンは同期 API と同じく `sync_tokens` に `issued_via = 'seen'` で記録する（`syncTokenRevoked`）。取り消したトークンと、記録できなかったトークン（DB の失敗）では読ませない。
* 外からの確認（`probeSiteAccess`）は、公開サーバーが返したサイトの URL が接続先の公開サーバー自身でなければ、インターネットに公開されたアドレスにだけ接続する（`publicDialer`。判定は `theme.CheckPublicAddr`）。
* 試験: `set_site_access` を受ける公開サーバーの試験は TLS で起動する（`newPublicOnlyTLSEnv`・`newSyncEnvTLS`。`startTestEnv` の `useTLS`）。httptest の TLS サーバーはすべて同じ試験用の証明書を使うため、`tls_env_test.go` の `init` で PC 側の `remoteHTTPClient`・`siteAccessProbeClient` にその証明書を信頼させる。平文の環境（`newPublicOnlyEnv`）は、平文の接続を断ることの試験に使う。

### 公開サーバーのメンテナンス表示 (`internal/server/maintenance.go`, `handlers_remote_maintenance.go`)
* 同期 API の `get_maintenance`・`set_maintenance`（admin のトークンのみ。`auth.CanSyncSettings`）で、公開サーバーのメンテナンス表示を確認し、手動で始める・終える。PC の「設定」→「サイト運用」（`templates/admin/settings_operations.html`。API は `/admin/remote/maintenance`。GET が状態、POST がフォーム `enabled=1|0`。管理者だけ・CSRF あり。画面の部品は `static/js/remote_maintenance.js`）がこれを呼ぶ。独自 HTML の選択はこの PC のこのサイトへ POST `/admin/remote/maintenance/page`（multipart の `file`、または `clear=1`）。表示中の画面の更新は POST `/admin/remote/maintenance/apply`（ON/OFF は変えない）。表示の権限は閲覧制限と同じ（`remoteSettingsData.CanManageMaintenance`）。
* **画面の優先順位**（`resolveMaintenanceHTML`）: ① サイト運用で個別に選んだ独自 HTML（公開サーバーでは `maintenance_page`、PC では `maintenance_page_local`）→ ② 適用中のテーマ（サイトの設定の `theme`。プレビューでは変えない）に同梱した `maintenance.html`（`theme.Theme.MaintenanceHTML`。独自テーマは「公開サーバーへ反映する」でテーマといっしょに公開サーバーへ送られる）→ ③ sorus の標準画面。`set_maintenance` の `page.kind` が `default` のとき、公開サーバーは②か③を出す。同期 API の形式は変えていない（`page` は「独自 HTML か、それ以外か」だけを表す）。
* **ダウンロード → 編集 → アップロード**: GET `/admin/remote/maintenance/page/download`（管理者だけ・権限は上と同じ）は、PC でいま有効な画面（上の優先順位）を `maintenance.html` として返す（`Content-Disposition: attachment`・`Content-Security-Policy: sandbox`・`no-store`。出どころを `X-Sorus-Maintenance-Source: custom|theme|default` で返す）。公開サーバーには問い合わせない。サイト運用の画面には「現在のメンテナンス画面をダウンロード」と手順を出し、編集したファイルを「この PC に保存」で選び直す。適用中のテーマが `maintenance.html` を同梱しているときは、この PC の画面の表示（`maintenancePageView` の `source`・`theme_name`・`source_note`）で「テーマ「名前」の画面」「独自 HTML を優先している」ことを示し、「標準の画面に戻す」は「テーマの画面に戻す」になる。
* `set_maintenance` は `{"maintenance":{"enabled":true|false}}` で**状態を指定する**（反転ではない。冪等なので版は持たない）。オフからオンにするときは `maintenance.page`（`kind`: `custom` または `default`。custom なら `name` と UTF-8 の `html`。上限 1 MiB）をいっしょに送り、画面と ON を一体で反映する。`page` が無ければ標準画面。すでにオンのときに `enabled: true` を送っても画面は変えない。画面だけを送る（`enabled` なし）と、表示中なら画面を更新し、表示中でなければ 409 `maintenance_page_not_active`（ON/OFF は変えない）。`enabled` も `page` も無ければ 400 `maintenance_missing`。不正な画面は 400 `maintenance_page_invalid`。応答 `SyncMaintenanceResponse` は `enabled`・`changed_at`・`changed`（ON/OFF が変わったか）に加え、必ず `page`（`kind`・`name`・`sha256`。HTML 本文は返さない）と `page_changed` を付ける。PC は応答に `page` が無ければ読み取れない（結果不明）として扱う。v0.2.8 など未対応の action は 400（何も変わらない）。
* 保存先は settings の `maintenance_enabled`（"1"/"0"）と `maintenance_page`（訪問者に出す画面の JSON。name・sha256・html。空なら標準）。この PC で選んでいる画面は `maintenance_page_local`（同じ JSON）に別保存し、選んだだけでは `maintenance_page` も `Server.maintenancePage` も変えない。`New` で公開側を読み（ON/OFF が読めなければ起動しない）、`Server.maintenance`（`atomic.Bool`）と `Server.maintenancePage` に持つ。書き込みは `Server.maintenanceMu` の中で DB に保存してから、公開側だけメモリに反映する。復元後は `reloadAfterRestore` が公開側を読み直す（選択は DB から都度読む）。`sync_settings`（独自テーマの `ThemePackage` を含む）と `set_site_access` は触らない。`SyncSettings` に maintenance の項目を加えないこと（`TestSyncSettingsAndSiteAccessDoNotChangeMaintenance`）。バックアップの形式は変えていない（settings に含まれる。PC 側の選択は `maintenance_page_local`、公開サーバー側の ON/OFF・画面は `maintenance_enabled` と `maintenance_page`）。
* 判定は `maintenanceMiddleware`（`e.Pre`）。順序は `basicAuthMiddleware` → `maintenanceMiddleware` → `PublicGuardMiddleware`（公開専用モードの `/admin` は 404）→ `redirectMiddleware`。閲覧制限が先なので、公開サイトのページ・画像などを開いた、パスワードを知らない人には 401 だけを返し、メンテナンス中であることも見せない。転送ルールより前なので、リダイレクトも返さない。`PublicGuardMiddleware` を後ろに置くのは、公開専用モードの `/admin` もメンテナンス画面にするため（公開専用モードの `/admin` は閲覧制限の対象なので、閲覧制限を有効にしていればその前に 401 になる）。
  * 対象外は閲覧制限と同じ `siteGateExempt`（上の「公開サーバー自身の閲覧制限」）。同期 API のパスへの POST 以外のメソッド・ルートが合わない表記は、ほかのパスと同じ 503 にし、公開サイトの 404 ページも秘匿パスの有無も見せない。
  * 管理画面にログイン中の編集部員（`isStaff`。サーバーモード）と、同期 API のトークンで `/uploads/` を読む PC（`syncTokenReadsUpload`）は通す。どちらも `basicAuthGuardedKey` を記録し、`basicAuthPrivateCache` が `Cache-Control: private, no-cache` にする（CDN に残させない）。
  * それ以外はメソッド・パスを問わず `serveMaintenancePage`: 上の優先順位で決めた HTML（独自 HTML → テーマ同梱の `maintenance.html` → 標準の HTML）をそのまま返す。503、`Cache-Control: no-store`、`X-Content-Type-Options: nosniff`。標準画面は `noindex`。サイト名などサイトの内容は標準画面には出さない。`e.Pre` で返すため、`e.Use` のキャッシュ・ETag・Gzip・CSP のミドルウェアは通らない。管理画面での HTML プレビューは設けない（同一オリジンでスクリプトを動かさない）。
  * 働くのはデスクトップモード以外（`maintenanceApplies`）。デスクトップモードには同期 API が無く、DB に設定が残っていても止めない。独自 HTML の選択はデスクトップでも PC 側に保存でき、公開サーバーへ送れる。
* PC 側は ON/OFF を保存せず、公開サーバーの応答だけを表示する。独自 HTML の選択だけをこのサイトの DB（`maintenance_page_local`）に保存する（選択・保存では公開サーバーも、このインスタンスの訪問者向け画面も変えない）。失敗はすべて 502 と `status: "unknown"` で返し、成功とは見せない（`remoteMaintenanceFail`）。未対応の action（v0.2.8）は「更新してください」、送った後の 0・5xx・読めない応答（必要な `page` が無い応答を含む）は `outcome_unknown`（「反映されたかどうか分かりません」）、送った後の 4xx は「切り替わっていません」。公開サーバーが指定と違う `enabled` を返したときは、その状態（`state`）つきの 502。始めたときに独自 HTML を送ったのに返った画面が一致しなければ 502（成功とは見せない）。PC の選択と公開側の一致は種類と SHA-256 で判定する（ファイル名は見ない）。画面の反映の失敗も同様に、独自画面の成功とは見せない。自動のやり直し・定期的な問い合わせはしない（画面の「状態を確かめる」で問い合わせ直す）。
* 試験: `maintenance_test.go`・`maintenance_page_test.go`。テーマ同梱の画面（`TestMaintenanceUsesThemeBundledPage`: 反映で公開サーバーへ送られること・独自 HTML の優先・標準テーマに戻したときの標準画面・`/custom-themes/` から配信しないこと）とダウンロード（`TestRemoteMaintenancePageDownload`・`TestRemoteMaintenancePageDownloadPermissions`）。公開サーバー側（ルート・メソッド・対象外のパスに似せた要求（末尾の `/`・`//`・`.`・符号化・登録していないメソッド。閲覧制限あり・なし・サーバーモード）・503/キャッシュ・同期 API・閲覧制限との順序・トークンの画像・再起動・`sync_settings`・独自テーマの反映・サーバーモード・デスクトップモード・復元・独自 HTML の開始・更新・標準への戻し・検証・画面だけの更新がモードを変えないこと）と PC 側（画面・権限・CSRF・入力・接続失敗・メンテナンス表示に対応していない公開サーバー・結果不明・食い違い・デスクトップ版の PC・取り込み・PC への保存が公開サーバーを変えないこと・開始で画面を送ること・反映・複数 PC・選択と公開の分離・同内容の別名・apply の切断/5xx/読めない応答）。

---

## 17. カスタムファビコン管理 (`internal/server`, `templates/admin/`)

組織やメディアごとのブランドアイデンティティを確立するため、直感的なファビコン管理を提供します。

* **対応フォーマット**: SVG, PNG, WebP, ICO。
* **管理画面 UI (`/admin/settings/general`)**: 「サイト基本設定」画面内の専用セクションからドラッグ＆ドロップによるアップロード、リアルタイムプレビュー表示、現在のファビコンの削除・初期化。
* **自動展開**: アップロードされた画像から各種デバイス向けアイコンを自動処理し、公開サイトおよび管理画面の `<head>` に即時反映。
* **SVG のサニタイズ**: スクリプト・イベントハンドラ（`on*`）・外部参照に加え、`<style>` 要素と `style` 属性も拒否する（CSS の `url()`・`@import`・エスケープで外部の URL を読み込みうるため）。

---

## 18. 固定ページとサイト構成 (`internal/article`, `internal/server/site_structure.go`, `internal/database`)

ページ（固定の URL の文書）とメニュー・トップページの設定を扱います。

### 4つの層

内容・構成・構造・見た目を分けて扱います（仕様 2.5）。

| 層 | 持つもの | 置き場所 | 変える人 |
| --- | --- | --- | --- |
| 内容 | 記事・ページの本文 | `articles`（`kind`） | ライター・編集者 |
| 構成 | どのページをメニュー・トップに出すか | `settings`（`site_menu`・`front_page`・`front_cards`） | 管理者（「外観」→「サイト構成」） |
| 構造 | HTML の骨組みと公開クラス | `templates/public/**`（全テーマ共通） | 開発者 |
| 見た目 | 色・書体・形・部品の上書き | `static/themes/<id>/`（テーマ） | テーマの作者・管理者の選択 |

テーマが変えられるのは見た目だけです。構成や構造をテーマごとに分けないでください。

### 記事の種類 `articles.kind`

* 新しいテーブルは作らず、`articles` に `kind`（`post` / `page`）を足します。型は `article.Kind`、検査は `Kind.Valid()` です。
* マイグレーション V8（`internal/database/migration.go` の `schemaV8`）は、`kind TEXT NOT NULL DEFAULT 'post' CHECK(kind IN ('post','page'))` の列と、`(kind, status, published_at)` の索引を足します。既存の記事はすべて `post` になります。
* `convertImportedPages`（`internal/database/schema.go`、V8 と同じトランザクション）は、`redirects` に `/?page_id=` からの転送がある記事を `page` に変えます。
* 公開 URL は `contentPath(kind, slug)` で決めます。ページは `/<slug>`、記事は `/articles/<slug>` です。
* 保存時、`kind` が空なら `post` です。`page` のスラッグが予約語なら `article.ErrReservedSlug` を返します（`internal/article/article.go`）。
* ページはカテゴリを表示しないため、フォームに欄が無い送信では既定のカテゴリ（未分類）を入れます（`handlers_article.go`）。

### 時系列の一覧は `postKindCondition` を通す

公開側で日時順に並べるクエリには、`postKindCondition`（`a.kind = 'post'`）を必ず入れます。対象は、トップの新着、`/articles`、カテゴリ、RSS、関連記事、RAG の関連記事、テーマのプレビュー、トップ用の新着6件です（`handlers_public.go`、`handlers_feed.go`、`handlers_theme_preview.go`、`site_structure.go`）。新しい一覧を足すときも同じです。入れ忘れると、ページが記事の一覧に混ざります。

ページ（`kind = page`）は、検索インデックス（`internal/search`）・サイトマップ・llms.txt に出します。検索インデックスのドキュメントには `kind` を入れ、結果のリンク先を決めます。`kind` を持たない古いドキュメントは記事として扱います（`searcher.go`）。

### 公開ルート

| パス | ハンドラ | 内容 |
| --- | --- | --- |
| `/` | `handlePublicIndex` | 記事の一覧。`front_page` が公開中のページなら、そのページとカード・新着6件 |
| `/articles` | `handlePublicArticles` | 記事の一覧（ページ送り）。`/articles?page=N` |
| `/articles/:slug` | `handlePublicArticle` | 記事。ページなら `/<slug>` へ 301 |
| `/categories/:slug` | `handlePublicCategory` | カテゴリの記事一覧 |
| `/:slug` | `handlePublicPage` | ページ。記事なら `/articles/<slug>` へ 301。`front_page` のページなら `/` へ 302 |

* `/:slug` は、固定のパスをすべて登録した後に置きます。Echo は固定のパスを優先するため、予約語と同じ名前のパスは既存の機能が受けます。
* `/:slug` は全メソッドで受けますが、GET と HEAD 以外は 404 を返します（405 にしない）。
* `handlePublicIndex` は、`front_page` が設定されているとき `/?page=N` を `/articles?page=N` へ 302 で転送します。

### 予約語と経路の検査

* ページのスラッグの予約語は `internal/article/article.go` の `reservedPageSlugs` です。`IsReservedPageSlug` は大文字・小文字を区別しません。
* 同期 API のパスの先頭部分は設定で変わるため、`article.WithReservedSlugs` で `Service` に渡し、`Service.IsReservedPageSlug` が固定の予約語とあわせて検査します。管理画面・同期・MCP・WordPress インポート・公開承認のどの経路も、この1つの規則を使います。
* `internal/server/reserved_routes_test.go` の `TestRegisteredRoutesAreReservedPageSlugs` は、`routes()` が登録する固定パスの先頭部分が予約語にあるかを検査します。**トップレベルのルートを足したら、その先頭部分を `reservedPageSlugs` に必ず足してください。** 足し忘れるとこのテストが失敗します。同期 API のパスは検査から除かれます。
* ユーザーガイドの予約語の一覧は、この `reservedPageSlugs` と同じにしてください。

### サイト構成の設定

* 設定のキーは `site_menu`（JSON 配列、最大8項目）、`front_page`（スラッグ）、`front_cards`（JSON 配列、最大6件）、`top_gallery_tag`、`top_gallery_limit`、`top_gallery_title` です。対応する `SiteStructure` のフィールドは `TopGalleryTag`（最大100文字、写真タグとの完全一致）、`TopGalleryLimit`（0〜24、0 は既定値6枚）、`TopGalleryTitle`（最大100文字、空なら「フォトギャラリー」）です。タグに一致する写真はトップページに新しい順で表示し、写真がなければギャラリー全体を出しません。保存は `saveSiteStructure`、読み込みは `loadSiteStructure` です。読み込みは壊れた JSON を空として扱い、公開サイトを止めません。
* 静的サイト公開の静的書き出しはローカル DB のメディアタグを解決して写真を含む HTML を生成します。公開サーバー連携の同期で転送するのはメディアファイルで、メディアのタグ情報は同期対象ではありません。そのため、動的なタグギャラリーを使うサイトには静的サイト公開の静的ホスティングを推奨します。
* メニューの種類は `page`（スラッグ）・`category`（スラッグ）・`articles`（対象なし）・`url`（`https://` の絶対 URL か `/` で始まるサイト内のパス）です（`MenuItemType`）。表示名が空なら、ページのタイトルかカテゴリ名を使います。
* 検証（件数・種類・URL・表示名・制御文字）は `validateSiteStructure` が行い、保存と同期の両方で使います。参照先（ページ・カテゴリ）の存在は検証せず、描画時に `resolveStructureRefs` で解決します。参照切れの項目は `publicMenu` が公開側に出しません。管理画面は警告を出します。
* 管理画面は `GET/POST /admin/settings/structure`（`adminOnly`）です。テンプレートは `templates/admin/settings_structure.html`、タブは `templates/admin/partials/appearance_tabs.html` です。
* 同期 API の `sync_settings` は `site_menu`・`front_page`・`front_cards` と `top_gallery_tag`・`top_gallery_limit`・`top_gallery_title` を扱います（`sync_types.go` の `SyncSettings` / `SyncSettingsValues`）。
* 公開側の HTML は `<nav class="category-nav">` の中に `.site-menu` を出します。メニューが無いときはカテゴリの一覧を出します。

### テーマのギャラリースタイル

本文・トップページ・テーマから呼び出したギャラリーは共通 HTML の `<figure class="photo-gallery masonry-grid">`、各画像は `.gallery-item` です。基本のレスポンシブ配置と余白は `static/css/public.css` の `@layer sorus-base` が持つため、テーマは `static/themes/<id>/theme.css` でトークンや `.photo-gallery` / `.masonry-grid` / `.gallery-item` を上書きして見た目を整えます。特定テーマだけで動く DOM を追加せず、色・列幅・間隔・角丸などのテーマ差分として実装してください。トップページのギャラリーを囲むセクションには `.front-gallery` が付きます。

---

## 19. デスクトップモード (`main.go`, `internal/desktop`, `internal/version`)

PC 上で使う配布版の動作モードです。管理画面は `127.0.0.1` だけで待ち受けます。サーバーモード（従来の動作）とは同じバイナリで、起動時に `desktop.SelectMode` が決めます。

* **モードの選択**: `--desktop`、またはビルド時の `-ldflags "-X main.buildMode=desktop"`（配布版）でデスクトップモード。`--server`・`--public-only` があるか、`PUBLIC_ONLY`・`ENV_FILE`・`DB_PATH` のどれかが設定されていればサーバーモードを優先する（Docker・systemd の既存の運用を壊さない）。`--desktop`・`--server` は引数のどこに置いてもよい。
* **データディレクトリ**: `.env` や `ENV_FILE` を読まず、OS 標準の場所（Windows `%LOCALAPPDATA%\sorus`、macOS `~/Library/Application Support/sorus`、Linux `$XDG_DATA_HOME/sorus` または `~/.local/share/sorus`）を使う。`SORUS_DATA_DIR` で差し替えられる。パスはアプリ全体の `desktop.AppPaths`（`Dir`・`Log`（`sorus.log`）・`Instance`（`instance.json`）・`Lock`（`instance.lock`）・`Sites`（`sites.json`）・`DeletedBackups`（`削除したサイトのバックアップ/`））と、サイトごとの `desktop.SitePaths`（`Dir`・`DB`（`database.db`）・`Uploads`（`uploads/`）・`Attachments`（`attachments/`）・`SearchIndex`（`search_index/`）・`Backups`（`backups/`）・`Recovery`（`パスワード再設定コード.txt`））に分かれる。1つ目のサイトはデータディレクトリ直下（`dir: "."`）、2つ目以降は `sites/<id>/` に置く。サブコマンド（`export`・`backup`・`restore`・`import`）も同じ場所を使い、サイトが2つ以上あるときは `--site <id>` で対象サイトを指定する（一覧は `sorus sites`）。
* **待ち受け**: `127.0.0.1` だけ。サイトごとに別のポートで待ち受ける。1つ目のサイトは優先ポート 8780（`desktop.PreferredPort`、使用中なら空きポート: `desktop.Listen`）、2つ目以降は `sites.json` に記録したポート → `8781`〜`8800`（`PreferredPort+1`〜`PreferredPort+20` のうち他サイトが使用・記録していないもの）→ OS の空きポートの順に選ぶ（`desktop.ListenSite`）。ポートが変わったときは `sites.json` を更新し、`Desktop mode: site <id>: port N is not available; using port M from now on` をログに出す。`Host` ヘッダーが `127.0.0.1:<port>` / `localhost:<port>` 以外の要求はサイトのポートごとに 403 にする（`desktop.HostGuard`、DNS リバインディング対策）。Cookie に `Secure` は付けない。
* **起動と終了**: 起動すると登録されているすべてのサイトを同時に開いて動かし、最後に使っていたサイトの `/admin` を sorus のウィンドウ（`desktop.OpenWindow`、下の「アプリのウィンドウと、閉じたら終了」）で開く（`SORUS_NO_BROWSER=1` で開かない）。あるサイトが開けなくても `openErr` に理由を記録して他のサイトは起動し、1つも開けないときだけエラーを返す。2つ目の起動は `instance.json`（最後に使っていたサイトのポートを記録）の既存インスタンスの `/healthz` を確かめて同じく `OpenWindow` で開き、終了する。終了は、画面がすべて閉じたとき（`windowWatcher`）か、管理画面の「sorus を終了」（`/admin/desktop/quit`）で、すべてのサイトを閉じる（サイトが2つ以上あるときは確認文が `desktop_quit_all_confirm` になる）。ログは `sorus.log` に残し、起動失敗は OS のダイアログで知らせる（Windows `MessageBoxW`、macOS `osascript`、Linux は標準エラーと `xdg-open` でのログ表示）。
* **アカウント**: 初期管理者 `admin` は作らない。サイトごとに管理者（`admin`）のユーザーがいなければ「はじめに」の画面（`/admin/setup`）で利用者のアカウントを管理者として作る（管理者は編集者・ライターのすべての役割を持つ。「9. 役割と権限モデル」）。「はじめに」で決めるのは名前・パスワードとサイト名だけ。「はじめに」が済んでいるかは `auth.SetupDone`（管理者がいるか）で判定し、古い版の `sorus import` が作った著者のユーザー（ライター）しかいない PC でも「はじめに」を出す。その著者と同じユーザー名は入力の誤りとして断り、管理者がいれば「はじめに」からは作れない（`auth.CreateFirstUser` が確認と登録を1つの文で行う）。管理者はアプリ版でも「設定」→「ユーザー管理」からアカウントを管理できる。
* **データを守る仕組み**（デスクトップモードだけで登録する。`internal/server/handlers_backup_auto.go`）:
  * 自動バックアップ（「13. 完全バックアップと安全なリストア」および下の「複数サイトのアーキテクチャ」）。
  * 「パスワードを忘れたとき」（`/admin/login/recovery`）: そのサイトのフォルダ（`s.dataDir()`）に `パスワード再設定コード.txt`（8桁、15分有効）を書き出し、そのフォルダを開く。5回続けて間違えるとコードを捨て、作り直しても数え直さず、15分は新しいコードを作らない。サーバーモードではこのパスは 404。
  * 更新のお知らせ: 24時間に1回 GitHub の releases/latest（`SORUS_UPDATE_URL` で差し替え）を調べ、新しい版があればホームに1行インストールのコマンドを出す。版が `dev` のときと `SORUS_NO_UPDATE_CHECK=1` のときは調べない。自動更新はしない。
* **WordPress の取り込み**: 管理画面では、すべての記事をログイン中の人にまとめるか、WordPress の著者ごとにユーザーを作るかを選べる。CLI はどのモードでも `current` が既定で、`--account-mode=authors` を指定すると WordPress の著者ごとにユーザーを作る。`current` は管理者アカウントにまとめ、`authors` は `importer.Options.DefaultAuthor` に従って記事を割り当てる。「はじめに」を済ませる前は「はじめに」を済ませてから取り込むよう日本語で伝えて止まる（`checkDesktopSetupDone`）。
* **版**: `internal/version.Version`（既定 `dev`）。`sorus --version` で表示する。

### アプリのウィンドウと、閉じたら終了 (`internal/desktop/browser.go`, `internal/desktop/dialog*.go`, `internal/server/handlers_window.go`, `internal/server/desktop.go`, `main_desktop_window.go`)

デスクトップ版の画面を、アドレスバーとタブの無いウィンドウで開き、画面がすべて閉じたら終了します。

* **`desktop.OpenWindow(url)`**: Chromium 系ブラウザのアプリモード（`--app=<URL> --window-size=1280,860`）で開く。利用者のいつものプロファイルを使う。候補は `appBrowserCandidates` の表で、Windows は App Paths に登録された `msedge.exe` → `%ProgramFiles(x86)%`・`%ProgramFiles%`・`%LocalAppData%` の `Microsoft\Edge\Application\msedge.exe`、macOS は `/Applications` と `~/Applications` の `Google Chrome.app` → `Microsoft Edge.app` → `Chromium.app`（`open -na <app> --args ...` で起動）、Linux などは PATH の `google-chrome` → `google-chrome-stable` → `chromium` → `chromium-browser` → `microsoft-edge`。見つからないか起動に失敗したら、`OpenBrowser` と同じ既定のブラウザ（Windows `rundll32 url.dll,FileProtocolHandler`、macOS `open`、Linux `xdg-open`）のタブで開く。`SORUS_NO_BROWSER=1` なら何もしない。
* **画面の数え方（`/sorus-window`）**: デスクトップモードのときだけ `server.WindowPath`（`/sorus-window`）を登録する（サーバーモード・公開専用モードでは 404）。数えるのはアプリの画面だけ（`isAppWindowPage`）で、管理画面・ログイン・はじめに・終了の確認（`/admin`・`/setup`・`/oauth` とその下）と OAuth の同意画面、管理画面のテーマのプレビュー（`/admin/settings/theme/preview`。公開ページを内部の要求で描くため、パスではなく文脈 `isThemePreview` で判定）。記事・トップ・カテゴリ・検索などの公開ページは数えない（公開ページを Chrome などで開いたままアプリのウィンドウを閉じても sorus が終わるように）。これらのレイアウトは `WindowScript`（`windowScriptFor`。デスクトップモードでない、静的な書き出し、アプリの画面でないページでは空）で WebSocket（`golang.org/x/net/websocket`）を1本保ち、つながっている間を「開いている画面」として数える（`(*Server).OpenWindows()`）。iframe の中では数えない。SSE（`EventSource`）にしないのは、ブラウザが同じホストへの HTTP/1.1 の接続を 6 本までしか開かず、画面ごとに SSE を保つと 6 枚目の次のページが読み込めなくなるため（WebSocket はこの上限に数えない）。タイマーで知らせる方式は、最小化した画面でタイマーが間引かれて閉じたと見誤るため使わない。
  * **合言葉（ブートトークン）**: `Server` ごとに起動のたびに `crypto/rand` の 16 バイトを 16 進にした合言葉（`windowState.token`、`newWindowToken`）を作り、スクリプトが接続 URL の `&t=` に付ける。サーバーは `subtle.ConstantTimeCompare` で照らし、合わなければ 403 で断って数えない（前に起動した sorus が描いた開いたままのタブや、`Origin` を偽ったほかのプログラムが終了を妨げないように）。合言葉はページ内のスクリプトにだけ置き、URL・Cookie には出さない。加えて接続は `Origin` のホストが `Host` と同じときだけ受け付ける（`sameOriginWindow`。ほかは 403）。
  * **生存確認**: サーバーの読み書きのタイムアウトでは切れない（`SetDeadline(time.Time{})`）。サーバーは 10 秒ごと（`windowKeepalive`）に ping を送り（書き込みの上限 `windowWriteWait` 10 秒）、ブラウザが自動で返す pong を含むフレームが届くたびに読み取りの期限を 25 秒先（`windowReadWait`、ping 間隔の 2.5 倍）へ延ばす（`readWindow`。`Conn.Read` は pong を読み捨てて戻らないため、フレーム単位で読む）。25 秒なにも届かなければ相手はもう応えないとみなして切る。Shutdown が始まると開いている接続を閉じる。
  * **つなぎ直し**: 切れたら 2 秒後（`windowRetry`）につなぎ直す。つながる前に切れる（断られた・sorus がいない）ことが続くときは待ち時間を倍にしていき（上限 30 秒、`windowRetryMax`）、一度つながれば 2 秒に戻す。sorus が 1008（規約違反）で閉じたときはつなぎ直さない。
  * **ページを離れるとき**: `pagehide`・`beforeunload` で自分から `close(1000)` を送り、sorus が応答の途絶えを待たずにすぐ数から外せるようにする。戻る・進むでページがそのまま戻ったとき（`pageshow` の `persisted`）はつなぎ直し、ダウンロードのリンクなどで `beforeunload` の後もページが残ったときは 2 秒後につなぎ直す。
  * ウィンドウを閉じてから終了するまでは、`close` が届けば猶予時間の 5 秒、届かず応答だけが途絶えても `windowReadWait`（25 秒）＋猶予時間でおよそ 30 秒（1 分以内）。`sorus-window` は `reservedPageSlugs` に入れてある。数の変化と切れた理由は `sorus.log` に `window: connected (N open) /admin` のように残る。
* **閉じたときの判断（`windowWatcher`）**: `desktopApp` はすべてのサイトの `OpenWindows()` の合計を 1 秒ごとに見る。
  * 最初の画面がつながるまでは終了しない（`phaseFirstWait`）。3 分つながらなければ URL をログで案内し、動き続ける。
  * 合計が 0 のまま猶予時間（`desktopWindowGrace`、5 秒）続いたら「すべて閉じた」とみなす。ページの移動や再読み込みでは終了しない。
  * どれかのサイトで反映かバックアップが動いていれば（`(*Server).Busy()`）、終わるまで待つ。
  * まだ反映していない予約（`(*Server).PendingScheduledReflects()` の合計）が 0 なら、そのまま終了する。1 件以上なら `desktop.AskQuitWithPending(msg)` で OS の確認画面を出す（Windows `MessageBoxW` のはい／いいえ、macOS `osascript` の `display dialog`）。確認画面を出せない OS（Linux など）や、出せなかったときは「予約をすべて反映してから終了する」を選んだものとして扱い、ログに残す。
  * 確認画面の間に画面がつながったら、答えを捨てて見張りに戻る（`reopen`）。
* **反映してから終了（headless の待ち、`phaseFinishing`）**: 画面を出さずに動き続け、`Busy()` が false で `PendingScheduledReflects()` が 0 になったら終了する。反映が失敗した予約は数えたまま残るため、待ち続ける。数えられなかったサイトがあるときも待ち続ける。待っている間に画面がつながったら（2回目の起動で `OpenWindow` が開いたときなど）、待ちをやめて見張りに戻る。
* **「sorus を終了」（`internal/server/desktop.go`）**: まだ反映していない予約が無ければ、ナビは今までどおり `POST /admin/desktop/quit` のフォーム（確認文つき）。予約があれば（`quitPending`）、`GET /admin/desktop/quit` の確認の画面（件数と最後の日時、「予約をすべて反映してから終了する」「今すぐ終了する」「やめる」）へのリンクになる。`finish=1` で送ると `Config.OnQuit(true)` → `windowWatcher.quitButton` が待ちの状態に入り、「予約をすべて反映したら、sorus を終了します」の画面（この画面は数えない）を返す。`finish=0` は今すぐ終了する。
* **`PendingScheduledReflects()`**: 公開先（静的サイト公開）が設定されていなければ 0。設定されていれば、状態が公開の記事のうち、公開日時が未来の記事と、公開日時が最後に成功した反映を始めた時刻（settings の `publish_last_success_started_at`。無ければ最後の成功の終了時刻）より後で、予約だった記事の集合（settings の `scheduler_scheduled_articles`）に今と同じ公開日時で入っている記事を数え、最も遅い公開日時を返す。集合は更新日時を見ないため、公開日時を迎えたあとに保存し直しても、起動し直しても数えたままになる。すぐに公開した記事は集合に入らない。反映の開始時刻は `startReflect` が覚え、成功したら `saveReflectSucceeded` が保存し、`scheduler.ForgetSent` が送った記事を集合から外す。
* **`ReflectDueNow()`**: 「予約をすべて反映してから終了する」（確認画面の答え・「sorus を終了」・確認画面を出せない OS の既定）で `windowWatcher` が待ちの状態に入るとき、サイトごとに呼ぶ。公開日時を迎えたのにまだ送っていない予約があれば、手動の反映を1回始める（反映中なら、終わってから手動の反映をもう1回行う）。起動する前に予約時刻を過ぎた記事について尋ねているあいだは予約投稿による反映が断られるため、手動の反映にする。
* **予約時刻を過ぎた記事の確認**（`loadOverdue`）: デスクトップモードで起動したあと最初に要るときに、まだ送っていない予約のうち、settings の `publish_overdue_checked_until`（前に数えた起動の時刻）より後で起動より前に公開日時を迎えた記事を数え、その時刻を今回の起動まで進める。前の起動で反映が失敗したまま終了した予約も次の起動で1回だけ尋ね、起動し直すたびには尋ね直さない。
* **`Busy()`**: `publishState.running`（反映中）か、`backupMu` の書き込みロックを取れない（バックアップの作成・復元中）ときに true。
* **外部リンクを既定のブラウザで開く（`POST /admin/desktop/open-external`）**: アプリのウィンドウで新しいタブを開くと、そのブラウザ（Edge など）のタブになるため、デスクトップモードの管理画面（`body` の `data-open-external`）では `static/js/admin.js` がクリックを拾い、外部リンクと `target="_blank"` のリンクの URL を JSON（`{"url": "..."}`。フォームの `url` も可）で `DesktopOpenExternalPath` へ POST する（ログイン済み・CSRF 保護済み。サーバーモードでは登録せず 404）。サーバーは `externalURL` で確かめ（`/` で始まるパスは同じホストのページとして解決し、それ以外は http・https でホストのある絶対 URL だけ。`javascript:`・`file:`・`data:` などは 400）、`desktop.OpenBrowser` で OS の既定のブラウザを開いて 204 を返す（起動できなければ 500）。204 以外（ログインが切れてログイン画面へ送られた 200 など）のときだけ `window.open` の新しいタブで開く。修飾キー付き・中クリック・`download`・`#` で始まるページ内リンク・`javascript:` はブラウザに任せる。左上のサイトの切り替え（`#site-menu` の `data-no-external-intercept`）と、同じホスト名（別のポートを含む、この PC のほかのサイト）の `/admin` 配下のページは既定のブラウザに逃がさず、アプリのウィンドウの中で開く（既定のブラウザではログインが要るうえ、そのブラウザが開いている画面の接続を保ち続けて sorus が終わらなくなるため）。
* **記事の公開テーマプレビュー（`GET /admin/articles/:id/preview`）**: 記事を状態（下書き・校閲待ち・予約中・公開中）を問わず、いま適用中の公開テーマで公開ページと同じに描画する（`handleArticleThemePreview`。ライター・編集者・管理者。ゴミ箱の記事は 404）。公開日時の無い記事は更新日時で代える。読者の閲覧ではないため閲覧数を数えず、検索エンジン・キャッシュにも残させない（`draftPreviewContextKey`・`isDraftPreview`）。
* **複数サイト**: サイトの切り替えは同じウィンドウの中で行う。「新しいサイトを作る」のフォームは `target="_blank"` を使わず、同じウィンドウで「はじめに」を開く。公開ページのプレビューなど読者として見るページは新しいタブのまま（`/admin` の外の公開ページは、上のとおり既定のブラウザで開く）。
* **macOS の `sorus.app`**: `LSUIElement` を保つ（sorus は Cocoa のイベントループを持たないため）。Dock にはアプリモードを開いたブラウザのアイコンが出る。

### 複数サイトのアーキテクチャ (`main.go`, `internal/desktop/sites.go`, `internal/server/sites.go`, `internal/server/handlers_sites.go`, `internal/auth/cookie.go`, `internal/backup/auto.go`)

1サイト＝1フォルダ＝1つの独立した `server.Server` として分離し、アプリがそれらを束ねて同時に動かします。

* **アプリとサイトの実行構造（`desktopApp` / `desktopSite`）**:
  * `desktopApp`（プロセスに1つ）がデータディレクトリの排他ロック（`instance.lock`）、`instance.json`、`sorus.log`、終了通知（`quit` チャネル）、共有の更新確認（`*server.UpdateCheck`）、サイトの一覧（`desktop.Registry` / `sites.json`）、サイト削除の後片付けを待つ `deletions`（`sync.WaitGroup`）を持つ。
  * `desktopSite`（サイトの数だけ）がサイトごとの `net.Listener`、SQLite 接続（`database.db`）、Bleve 全文検索インデックス（`search_index/`）、AI / RAG サービス、および `*server.Server` を持つ。各サイトの待ち受けには `desktop.HostGuard(port, ...)` と、管理画面の HTML ページ取得（`GET /admin...`、非 HTMX、`Accept: text/html`）で `sites.json` の `last` と `instance.json` を更新する `trackUse` ミドルウェアを挟む。
* **`server.Config` の複数サイト関連フィールド**:
  * `Sites`（`server.Sites` インターフェース: `List() []SiteInfo`、`Create() (SiteInfo, error)`、`Delete(id string) (backupFile string, err error)`、`Forget(id string) error`）: `main.go` がサイトごとの `desktopSiteView{app, site}` を渡す。「設定」→「サイト」（`/admin/settings/sites`、`handlers_sites.go`）とヘッダーのサイト切り替えメニュー（`layoutSites`）はこれを通してだけサイトを扱う。サーバーモードでは `nil` で、ルート（`registerSitesRoutes`）も画面・メニューも出さない。
  * `SiteKey`: サイトを見分ける鍵（英小文字と数字のみ。`validSiteKey` で検査）。1つ目のサイト（`dir == "."`）とサーバーモードは空文字列 `""`、2つ目以降のサイトはそのサイトの `id`。Cookie 名と自動バックアップのファイル名に使われる。
  * `UpdateCheck`（`*server.UpdateCheck`）: `server.NewUpdateCheck()` でアプリにつき1つ作り、すべてのサイトの `Config.UpdateCheck` に渡して `go app.update.Run(updateCtx)` をアプリ全体で1回だけ動かす。サイトは `startCare` で加わり、`StopBackground`・`Shutdown` で外れる。確認日時（`update_checked_at`）と最新版（`update_latest_version`）は参加サイト間で最も新しい値を共有して全サイトの DB に保存し、GitHub への問い合わせをアプリ全体で1日1回までに保つ（「閉じる」の `update_dismissed_version` はサイトごと）。`UpdateCheck` が `nil` のときはサーバー単独の `runUpdateChecks` で動く。
  * `LogPath`: 動作記録（`sorus.log`）のパス。「サイト」画面で開けなかったサイト（`OpenError != ""`）の案内に出す。省略時はデータディレクトリの `sorus.log`（`appPaths()` は `sites/<id>/` から呼ばれても親のデータディレクトリを解決する）。
* **サイトごとの Cookie 名（`auth.CookieNamesFor`）**:
  * ブラウザは Cookie をポート番号で分けないため、`auth.CookieNamesFor(siteKey)` でサイトごとに Cookie 名を導出する。
  * `siteKey == ""`（1つ目のサイトとサーバーモード）では従来の `cms_session`・`cms_csrf`・`sorus_lang` を保ち、既存のログインを切らない。
  * `siteKey != ""`（2つ目以降のサイト）では末尾に `_<siteKey>` を付けた `cms_session_<id>`・`cms_csrf_<id>`・`sorus_lang_<id>` を使う。`Server` は `s.cookies` に保持し、セッション・CSRF・表示言語の Cookie を読む・書く・消すすべての箇所で同じ名前を使う（`language(c)` は `e.Pre` ミドルウェアがコンテキストに入れた言語 Cookie 名を読む）。
* **サイトごとの自動バックアップ名と削除前バックアップ（`internal/backup/auto.go`）**:
  * `backup.Scheduler.Site` に `s.cfg.SiteKey` を渡す。`Site == ""`（1つ目のサイト）は `sorus_backup_YYYYMMDD_HHMMSS.zip`、`Site != ""`（2つ目以降のサイト）は `sorus_backup_<Site>_YYYYMMDD_HHMMSS.zip`（同じ秒に重なったときは末尾に `-2`, `-3`, ...）を作る。
  * `Latest` と `Prune` は正規表現 `autoNamePattern`（`^sorus_backup_(?:([a-z][a-z0-9]*)_)?(\d{8}_\d{6})(?:-(\d+))?\.zip$`）で `Site` が一致するファイルだけを数えて消すため、複数サイトが同じ保存先フォルダを共有しても他のサイトのバックアップや利用者のファイルに触れない。
  * サイト削除直前に `Server.BackupBeforeDelete` が作る完全バックアップは、`desktop.DeletedSiteBackupName(id, t)` により `sorus_deleted_site_<id>_YYYYMMDD_HHMMSS.zip`（UTC）という名前にする。`sorus_backup_` で始まらないため、自動バックアップと同じフォルダ（設定されていなければ `<データディレクトリ>/削除したサイトのバックアップ/`）に置いても世代整理で消されない。自動バックアップのフォルダが削除で消える範囲の中（2つ目以降は `sites/<id>/` の中、1つ目は `uploads/` など消すもの）にあるときは、書く前に「削除したサイトのバックアップ」へ置く（`desktop.DeleteBackupDir`。確認画面も同じ場所を示す）。
* **`Server.StopBackground` / `ResumeBackground` とサイト削除の手順（`internal/server/sites.go`, `main.go`）**:
  * `Sites.Delete(id)` は、開けなかったサイト（`srv == nil`）の削除と、開いているサイトが1つしか残っていないときの削除を拒否する（開けなかったサイトは残数に数えない）。
  * 手順2で `Server.StopBackground(ctx)` を呼び、予約投稿ワーカー（進行中の1回が終わるのを待つ）、予約投稿による自動反映のデバウンスタイマーと再実行フラグ（取り消して `reflectPending` に記憶。すでに動いている反映は止めない）、自動バックアップのループ（進行中の自動バックアップが終わるのを待つ）、共有 `UpdateCheck` への登録を止める。画面と API はそのまま応答し続ける。
  * 手順3で `Server.BackupBeforeDelete` により完全バックアップを作る。バックアップに失敗したときは何も消さず、アプリが終了中でなければ `Server.ResumeBackground()` で予約投稿・自動バックアップ・`UpdateCheck`・取り消した自動反映タイマーを元どおり動かし直す（アプリが終了中なら再開せず、`cancelDelete` が `Shutdown` して閉じる）。
  * バックアップに成功したら一覧から隠し（`gone = true`）、ハンドラーは `/static` などを読み込まない自己完結 HTML の完了画面（`siteDeletedTemplate`、残った先頭サイトへのリンクと3秒後の自動遷移付き）を `Connection: close` で返す（「はじめに」が未完了の別サイトを `POST /admin/settings/sites/:id/delete` で消したときは `/admin/settings/sites?flash=site_deleted` へ 303 リダイレクト）。
  * 別の goroutine（`finishDelete`）で `Server.Shutdown`（完了画面の送信完了を待つ。`a.mu` は持たない）→ DB・検索インデックス・リスナーのクローズ → `sites.json` の保存（外したあとの一覧。保存に失敗したらフォルダは消さず、一覧からも外さない。止めて閉じたサイトは開けなかったサイトとして一覧に残し、`instance.json` は開いているサイトを指す）→ 保存が成功してから `AppPaths.RemoveSite`（2つ目以降は `sites/<id>/` ごと削除。1つ目のサイト `dir == "."` は `database.db` と `-wal`/`-shm`、`uploads/`、`attachments/`、`search_index/`、`パスワード再設定コード.txt` だけを消し、データディレクトリと `sites.json`・`sites/` などは残す）し、`instance.json` は開いているサイトを指すようにする。
  * 開けなかったサイトは `Sites.Forget` で一覧（`sites.json`）から外せる。フォルダは消さないし、中身も変えない。開いているサイト、無い id、一覧が空になる操作は断る。1つ目のサイトは、ほかに開いているサイトがあるときだけ外せる。最後に使っていたサイトなら、残った先頭を最後に使ったサイトにする。`instance.json` がそのサイトの待ち受けポートか記録したポートを指していれば、開いているサイトを指すようにする。画面では「一覧から外す」（`POST /admin/settings/sites/:id/forget`）。確認と結果では、フォルダと中のデータをそのまま残すことと、バックアップの Zip があれば新しいサイトの「バックアップから始める」で戻せることを伝える。結果の文はクエリの文字列を表示しない。開けなかった理由は `ClassifyOpenError` で damaged・missing・permission・address・unknown に分け、技術的な文は「詳しい内容」に畳む。`SQLITE_CANTOPEN` は missing にしない（無いパスは SQLite に届く前に `os.ErrNotExist` になる）。
* **`sites.json` の形式（`internal/desktop/sites.go`）**:
  * `<データディレクトリ>/sites.json` は、2つ目のサイトを初めて作ったときに作成される（そのとき1つ目のサイトにも `id` を振る）。サイトが1つだけの間はファイルを作らず、`LoadRegistry` は `dir: "."`（`id: ""`、作成日時は DB ファイルの更新日時）の1サイトを返す。
  * JSON 形式:
    ```json
    {
      "sites": [
        {"id": "k3x9mq", "dir": ".", "port": 8780, "created": "2026-10-29T10:00:00+09:00"},
        {"id": "p7d2wa", "dir": "sites/p7d2wa", "port": 8781, "created": "2026-10-29T10:05:00+09:00"}
      ],
      "last": "p7d2wa"
    }
    ```
  * `id`: 英小文字で始まる6文字の英小文字・数字の乱数（`ValidSiteID`）。既存エントリおよび `sites/` 配下のディレクトリと重ならない。
  * `dir`: `"."`（高々1サイト）または `"sites/<id>"`。それ以外はパストラバーサル防止のため `validate` が拒否する。
  * `port`: 待ち受けポート（`0` は未記録）。
  * `last`: 最後に管理画面を開いたサイトの `id`。
  * サイト名は二重管理を避けるため `sites.json` には持たず、各サイトの DB から `server.SiteNameOf` で読む（「はじめに」が済んでいなければ空文字列で、画面と `sorus sites` では「名前の無いサイト（作成日）」と表示する）。読み取りに失敗したときは未設定とは別（`SiteInfo.NameError`）で、名前の入力なしの削除はしない。
  * `SaveRegistry` は同じディレクトリの一時ファイル（`.sites-*.json`）に書いてから `os.Rename` で置き換える。書き込むのは動いているアプリだけで、CLI は読むだけにする。`sites.json` が壊れているときは、サイトを黙って失わないよう起動をエラーで止める。

---

## 20. 公開先と「サイトに反映」 (`internal/publish`, `internal/server/handlers_publish*.go`)

「設定」→「公開先」（`/admin/publish`、管理者のみ）で公開先を1つ保存し、「サイトに反映」（`POST /admin/publish/reflect`）で静的出力を送ります。反映はバックグラウンドで行い、画面は `/admin/publish/status` で進み具合を取得します。

公開方法は「公開サーバー」（sorus 公開サーバー同期、`/admin/settings/remote`）か「静的サイト」（`/admin/publish`）のどちらか1つなので、左メニュー・設定の入口は「公開先」1項目にまとめ、両方の画面で選択中にします（`settings_subnav.html` の `.AtPath "/admin/publish" "/admin/settings/remote"`）。公開サーバーに接続済みなら `/admin/publish` は `/admin/settings/remote` へ送り（`?view=static` で静的サイトの設定を開ける）、どちらも未設定なら `/admin/publish` に「公開方法を選ぶ」2択（`#publish-method-choice`）を出します。両方の画面の上には「現在の公開方式」（`#publish-current-method`・`#remote-current-method` の `data-publish-method`）を出し、もう一方の方式へは主ボタンではなく控えめなリンクだけを置きます。回帰テストは `publish_guides_test.go`。

| 公開先 | 実装 | 主な扱い |
| :--- | :--- | :--- |
| SFTP | `pkg/sftp` + `golang.org/x/crypto/ssh` | パスワードまたは秘密鍵。初回にホスト鍵を記憶し、変わったら何も送らずに止める（確認後に `trust-host-key`） |
| FTP | `jlaffaye/ftp` | `AUTH TLS`（FTPS）で暗号化する。平文で送るのは「暗号化しないで送る」を明示したときだけ |
| Cloudflare Pages | Direct Upload API（標準ライブラリ） | プロジェクトが無ければ作る。反映のたびにサイト全体が新しい版になる |
| S3 互換ストレージ | `internal/publish/s3.go`（Go 標準ライブラリ） | AWS Signature Version 4 と path-style URL で S3、R2、MinIO 等に差分反映 |
| GitHub Pages | `internal/publish/github.go`（Go 標準ライブラリ） | Git Data API でサイト全体を1コミットにまとめ、公開元ブランチを原子的に進める。ルートまたは `docs` フォルダ |
| フォルダ | ローカルファイル | 書き出すだけ。Zip でのダウンロード（`/admin/publish/zip`）もできる |

* **差分**: 前回送ったファイルのマニフェストと比べ、変わったファイルだけを送る。下書きに戻した記事など、要らなくなったファイルは sorus が前に送ったものだけを消す。アップロード先のフォルダの外には書き込まない。
* **公開サイトの URL の確認**: 公開サイトの URL が未設定、またはこの PC の中のアドレス（`localhost` など）のときは、書き出さずに止め、「公開サイトのURL」の入力欄へのリンクを返す（CLI の `sorus export` は警告だけ）。
* **Cloudflare Pages の初回反映**: 保存済みのマニフェストが無いときは、先に本番の版を調べ、公開中のサイトがあれば `publish.ErrLiveSite` で止めて「置き換える」かを尋ねる（`POST /admin/publish/replace-site` で次の1回だけ許可する）。空のプロジェクトはそのまま反映する。
* **フォルダの初回反映**: 保存済みのマニフェストが無いときは、書き込む前に `publish.CheckFolderTopPage` でフォルダ直下の `index.html` を `Lstat` で調べ、あれば（ファイル・シンボリックリンク）`publish.ErrLiveSite` で止めて、Cloudflare Pages と同じ「置き換える」の確認（公開先の `destinationID` ごと、次の1回だけ）を使う。フォルダが無い・空・`index.html` が無いときは尋ねない。`index.html` がフォルダなら `publish.ErrTopPageIsFolder` で何も書かずに止める。2回目からは確かめず、いつもどおりマニフェストで上書き・削除する。
* **S3 互換ストレージ**: `internal/publish/s3.go` が S3 REST API を path-style URL で呼び出し、リクエストごとに Signature Version 4 を計算する。接続テストはバケット内に一時オブジェクトを PUT し、DELETE できることを確認する。リージョン未指定時は `auto` を使う。静的サイト配信や独自ドメイン、ディレクトリ URL から `index.html` への振り分けは公開先側で構成する。
* **GitHub Pages**（`internal/publish/github.go`、画面と接続テストは `internal/server/handlers_publish_github.go`）: GitHub SDK・go-git・git CLI を使わず、`net/http` で REST API（`https://api.github.com`、テストでは `publishGitHubAPI` を差し替える）を呼ぶ。設定は settings の `publish_github`（JSON）に保存し、トークンは画面に出さない（`publishGitHubView.HasToken` だけを渡す）。リダイレクトは追わない（`CheckRedirect` で `http.ErrUseLastResponse`）。
  * **Git Data API による原子的反映**（`Deploy`）: (1) **Ref** `GET /git/ref/heads/<branch>` で先頭のコミットを得る。(2) **Commit** `GET /git/commits/<sha>` でその Tree を得る。(3) **Blob** 差分（`NewPlan(prev, next)`）のうちアップロードするファイルを読み、100KB 未満かつ UTF-8 として正しいものは Tree のエントリに `content` としてインラインで載せ、それ以外は `POST /git/blobs`（base64）で Blob を作って `sha` を載せる。100MB を超えるファイルは `ErrGitHubFileTooLarge` で止める。(4) **Tree** `POST /git/trees` に `base_tree`（前のコミットの Tree）と、アップロード・削除（`sha: null`）・`.nojekyll` のエントリを渡す。`base_tree` を使うので、`CNAME` などマニフェストに無いファイルはそのまま残る。(5) **Commit** `POST /git/commits` で親を1つにしたコミットを作る。(6) **Ref** `PATCH /git/refs/heads/<branch>` を `force: false` で呼び、fast-forward でブランチを進める。ブランチを進めるのは最後の1回だけなので、途中で失敗しても公開中のサイトは変わらず、作りかけの Blob・Tree・Commit はどの参照からもたどれない。同時に別のコミットが入って fast-forward できないときは GitHub が 422 を返し、反映を失敗として扱う。
  * **インライン登録の理由**: 小さなテキスト（HTML・CSS・XML など）を Blob にせず Tree に載せることで、ファイル数に比例する API コールを画像などのバイナリだけに減らし、GitHub の二次レート制限（内容を作るリクエストは毎分80件程度が目安）に当たりにくくする。
  * **二段構えの初期化**: ブランチ参照の取得が失敗したとき、ステータスで処理を分ける。
    * **404（リポジトリはあるがブランチが無い）**: `createBranch` がリポジトリの存在を `GET /repos/<owner>/<repo>` で確かめたあと、Git Data API で `.nojekyll` だけの Tree と親なしコミット（`parents: []`）を作り、`POST /git/refs` でブランチを作る。既定ブランチの履歴を引き継がない、公開専用の独立したブランチになる。
    * **409（空のリポジトリ。コミットが0件）**: GitHub の Git Data API は空のリポジトリでは使えない（409 Git Repository is empty）ため、`seedEmptyRepo` が Contents API の `PUT /contents/<dir>/.nojekyll` で最初のコミットを作り、そのコミットを土台に通常の反映を続ける。
    * 以前あった「Tree の作成が 409 になったら base_tree なしで作り直す」逃げ道は、既存のファイルを消してしまうおそれがあるため設けない。
  * **`.nojekyll`**: アップロードか削除が1件でもあるときは、毎回 Tree に空の `.nojekyll`（`docs` 指定時は `docs/.nojekyll`）を載せ、Jekyll の変換を止めて `_` で始まるファイルを守る。初期化のコミットにも含める。
  * **接続テスト**（`TestConnection`）: ブランチを読み取り（404・409 ならブランチ未作成または空のリポジトリとして続ける）、さらに `POST /git/blobs` でプローブ用の Blob（`sorus connection test`）を作り、`contents:write` があることを確かめる。Blob はどの Tree・参照にも入らないので、サイトには影響しない。
  * **設定の検証**（`GitHubPagesConfig.Validate`）: 持ち主・リポジトリ名は GitHub の命名規則の正規表現、ブランチは `git check-ref-format` 相当（`..`、`@{`、`.lock`、`.` で始まる要素などを拒否）、フォルダは空か `docs` のみ。API のエラーは `GitHubError.Code`（`auth`・`rate_limited`・`permission`・`not_found`・`conflict`・`validation`）に分類する。
  * **公開サイトの URL**: 保存時に `https://<owner>.github.io`（`<owner>.github.io` 以外のリポジトリなら `/<repo>` を付ける）を入れる。`base_url` がすでに独自ドメインなら上書きしない。
* **Cloudflare CDN Cache Purge**: Zone ID と API Token を設定し、Cache Purge API を呼び出す。接続テストはサイト全体をパージする。自動パージは、反映でアップロードまたは削除が行われた後に実行する。失敗は反映結果の警告に記録する。
* **公開サイトのヘルスチェック**: 反映成功後にバックグラウンドで実行し、設定した公開 URL のトップページ、HTTP ステータス、HTML Content-Type、TLS 証明書の残存期間、`sitemap.xml` を確認する。HTTP リクエストと DNS 解決を含め最大10秒で打ち切り、結果を `settings.publish_health_status` に保存して公開先画面に表示する。SSRF 対策として、検査先は公開 IP アドレスに限定し（`publicHealthTarget`。判定はテーマの取り込みと同じ `theme.CheckPublicAddr`）、リダイレクト先も検査する。名前は1度だけ解決して、全部の答えが公開アドレスであることを確かめ、確かめたアドレスに固定して接続する（`publicDialer`。DNS リバインディング対策。答えに1つでも公開されていないアドレスがあれば接続しない）。これは閲覧可能性の診断であり、認証が必要なサイトの検査には対応しない。
* **予約投稿**: `internal/scheduler` が予約の公開日時が来たことを知らせたら（`SetOnPublished` → `onScheduledPublished`）、公開先が設定されていれば少し待ってまとめて自動で反映する。sorus が止まっている間に過ぎた予約は、ホームで反映するかを尋ねる。ホームの確認は1回だけ出し（`overdueAsked`）、「あとで」で閉じても、手動の反映が成功するまでは予約の時刻による自動の反映を止めたままにする（`st.overdue` は手動の成功でだけ戻る）。成功した反映を始めた時刻は `publish_last_success_started_at` に保存し、終了の判断（`PendingScheduledReflects`）に使う。予約の考え方は「23. 予約投稿の考え方」。
* パスワードと API トークンは DB の設定に保存し、画面には表示しない。

---

## 21. 「届いた原稿」と複数人で書く方法 (`internal/server/handlers_manuscript*.go`)

複数人で書く方法は、公開サーバーとつなぐ（招待コード・同期 API）、同じネットワークのサーバー版を一緒に使う（`sorus --server` に各自がブラウザでログイン）、AI アプリ（MCP）の3つ。原稿ファイル（`.sorus`）のやり取りは v0.3.0 の後に廃止した（書き出し・読み込みのルートも形式のコードも無い）。

* **「届いた原稿」**（`/admin/manuscripts`、`editorOnly`）: 公開サーバーの校閲待ちを `fetchRemoteQueue`・`localStatuses` で読んで並べ、公開サーバーで予約中の記事も出す。公開サーバーに（校閲待ちを読める招待で）参加していないとき（`remoteSyncViewFor(user).CanReview` が偽）は、公開サーバーへの参加の案内だけを出す。
* **受け取り**: 「この PC に受け取る」は `/admin/remote/pending/:id/preview` で中身と既存の記事との差分（`manuscriptPreview`・`existingView`・`lineDiff`）を見せ、`/admin/remote/pending/:id/import` で校閲待ちとして取り込む。既存の記事を上書きするときは `overwrite=1` を求め、上書き前の内容はリビジョン履歴に残す。
* **ダッシュボード**: `/admin/manuscripts/panel` は、公開サーバーの原稿を受け取れる編集者（と管理者）にだけ「届いた原稿」への入口を返し、それ以外は空を返す。

---

## 22. 管理画面のナビゲーションと設定の入口 (`templates/admin/layout.html`, `internal/server/handlers_settings_hub.go`)

* ヘッダーは1行で、左端のサイト名（デスクトップモードでサイトが2つ以上あるときだけ `layoutSites` により `<details id="site-menu">` の切り替えメニューになる）、続いて「ホーム」「記事」「届いた原稿」（編集者のみ）「メディア」「設定」と「＋ 新しい記事」、アカウントのメニュー（表示言語・ログアウト・デスクトップモードの「sorus を終了」）を並べる。
* 「設定」（`/admin/settings`）は設定画面の入口で、公開先・公開サーバー・サイト設定・外観・サイト（デスクトップモードで `Config.Sites != nil` のときのみ）・バックアップ・ユーザー管理（サーバーモードのみ）をカードで並べ、「上級者向け」に MCP連携を置く。カードは各画面のルートと同じ役割の条件で出し分ける。
* 現在の画面に対応するナビの項目は `adminNavSection` が決める（`/admin/publish`・`/admin/users` は「設定」に含める）。新しい設定画面を足すときは、ルートの役割と `settingsHubFor` の条件を揃えること。

---

## 23. 予約投稿の考え方 (`internal/article`, `internal/scheduler`, `internal/database`, `internal/server/handlers_sync.go`, `internal/server/handlers_remote.go`)

予約投稿は「公開してよい」という判断と「読者に見せ始める時刻」の組み合わせで、時刻を守るのは読者に記事を配っている場所です。

* **状態は公開だけ**: 予約中の記事は、状態が `published` で公開日時（`published_at`）が未来の記事。`article.IsScheduled(a, now)`（公開で、公開日時が `now` より後）と `article.IsVisible(a, now)`（公開で、公開日時が `now` 以前）で判定する。`article.StatusScheduled`（`scheduled`）は保存しない仮想の値で、一覧の絞り込み（`?status=scheduled`）、画面の状態（`article.ViewStatus`）、MCP の `statuses` に使う。`Service.Save` に渡すと `ErrInvalid` になる。
* **読者に見える条件は1つ**: SQL では `article.VisibleCondition`（`a.status = ? AND a.published_at IS NOT NULL AND julianday(a.published_at) <= julianday(?)`）、予約中は `article.ScheduledCondition`。公開ページ・一覧・カテゴリ・RSS・サイトマップ・`llms.txt`・関連記事・添付資料・静的な書き出し・SNS 向けの情報・RAG・MCP の閲覧は、どれもこの条件で絞る。検索の索引には未来の公開記事も入れ、検索するときに時刻で絞る。状態ごとの件数・絞り込みは `article.StatusCondition`（`published` は予約中を除く）。
* **予約にできない記事**: 読者に見えたことのある記事（保存済みの公開日時が今以前）を予約にすると `article.ErrPublished`（`ErrInvalid` を兼ねる）。承認（`handleApprovePublish`）でも 422 で断り、即時の再承認では最初の公開日時を保つ。公開以外の状態に戻すと、未来の公開日時は消す（予約の取り消し。`SoftDelete` も同じ）。
* **移行 `schemaV10`**（`internal/database/migration.go`）: `scheduled` の記事を `published`、公開日時を `scheduled_at` の値にする。`scheduled_at` の無い `scheduled` は `review` に戻す。最後に `scheduled_at` の値をすべて消す。列と CHECK 制約の `'scheduled'` は索引・表の作り直しを避けるため残し、使わない。何度実行しても同じ結果になり、古い版のバックアップを復元したときも `Migrate` を通るので移行される。
* **scheduler のチェックポイント**（`internal/scheduler`）: 状態は書き換えず、公開日時を迎えた予約だった記事を知らせて検索の索引に反映し直す。「どこまで確かめたか」を settings の `scheduler_checked_until`（RFC 3339、UTC）に、探した記事と同じ `BEGIN IMMEDIATE` のトランザクションで保存する。sorus を閉じている間に迎えた記事は次の起動で1回だけ見つかり、時計が戻っても確かめた時刻は戻さない。予約だった記事は、settings の `scheduler_scheduled_articles`（記事 ID と予約した公開日時の JSON）に今と同じ公開日時で入っている記事とする。集合には、記事を保存・承認した画面と同期 API の `schedule_article` が予約を入れ（`scheduler.Track`）、削除・下書きなどへの戻し・`unschedule_article`・「今すぐ公開」で外す。scheduler も確かめるたびに予約中の記事を入れ、予約でなくなった記事を外す。集合のキーが無ければ空の集合から始める。チェックポイントのキーが無ければ時刻の制限なく探す。
* **静的サイト公開の反映**: 知らせを受けた `onScheduledPublished` が反映を始める（「20. 公開先と『サイトに反映』」）。成功した反映を始めた時刻 `publish_last_success_started_at` より後に公開日時を迎えた予約は、まだ公開先へ送っていないものとして `PendingScheduledReflects` が数える（「19. デスクトップモード」の「アプリのウィンドウと、閉じたら終了」）。
* **公開サーバー連携の予約公開（同期 API）**（`internal/server/sync_types.go`・`handlers_sync.go`）:
  * `schedule_article`（editor・admin）: 校閲待ちの記事を、`publish_at`（RFC 3339。今から `MinScheduleLead` = 1 分以上先）を公開日時にした公開記事にする。公開サーバーは公開日時を保存するだけで、時刻のための常駐処理は無い。公開中の記事・一度公開した記事は 422、校閲待ちでなければ 409。
  * `unschedule_article`（editor・admin）: 予約中の記事を校閲待ちに戻し、公開日時を消す。公開日時を過ぎた記事は戻さない（409）。
  * `server_info`（どのロールでも）: `{"schedule": true}` を返す。
  * `fetch_pending` の応答に、予約中の記事を公開日時の早い順に `scheduled` として足す（校閲待ちの `articles` には入れない。予約を知らない古いローカル CMS に公開の操作を出さないため）。
* **能力の確かめ方（capability probe）**（`handlers_remote.go` の `remoteCanSchedule`）: ローカル CMS は、予約の選択肢を出す前に `server_info` を送る。200 で `schedule` が true なら対応、400（予約に対応していない公開サーバーは知らない action を 400 で断る）なら非対応。結果は接続先（同期 URL とトークンのハッシュ）ごとに覚え、非対応は 5 分で確かめ直す。非対応なら選択肢の代わりに「公開サーバーを更新すると、予約して公開できます」を出し、`/admin/remote/pending/:id/schedule` も 409 を返す。どの場合も即時公開（`publish_article`）には切り替えない。接続できないなど判断できないときは、何も出さない。
* **画面**: 「届いた原稿」と「公開サーバーの校閲待ち記事」（ホーム・「設定」→「公開先」）に「日時を決めて予約」（`datetime-local`、日本時間、初期値は 5 分以上先の次の正時）と、「公開サーバーで予約中の記事」（「予約: 11月3日 9:00」と「予約を取り消す」）を出す。
* 公開サーバーの前に配信サービス（CDN）がある場合、予約の時刻を過ぎてから見え始めるまで最大 1 時間ほど遅れることがある。キャッシュの消去はしない。
