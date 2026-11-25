## sorus {{VERSION}}

### インストール・更新のしかた（おすすめ）

次の1行をコピーして貼り付け、Enter を押すだけです。2回目以降は同じ1行で新しい版に更新できます。書いた記事などのデータは別の場所にあるので消えません。管理者のパスワードは要りません。

**Windows** — スタートボタンを右クリックして「ターミナル」または「Windows PowerShell」を開き、次の1行を貼り付けます。

```powershell
irm https://github.com/ryuzi/sorus/releases/latest/download/install.ps1 | iex
```

**Mac** — 「アプリケーション」→「ユーティリティ」の「ターミナル」を開き、次の1行を貼り付けます。

```sh
curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh
```

Linux も Mac と同じ1行で入ります。

この1行は、パソコンに合うファイルを選んでダウンロードし、壊れていないこと・書き換えられていないことを確かめてから入れ、sorus を起動します。ブラウザで sorus の画面が開けば完了です。次からはスタートメニュー（Windows）や、ホームの「アプリケーション」フォルダ（Mac）の sorus から起動できます。

### うまくいかないとき（予備の方法）

下の「Assets」から自分のパソコン用のファイルをダウンロードして使うこともできます。この方法では、まだ署名のないアプリのため警告が出ます。

* **Windows**: `sorus-{{VERSION}}-windows-amd64.zip`（ARM の PC は `windows-arm64`）を展開し、`sorus.exe` をダブルクリックします。「Windows によって PC が保護されました」と出たら、「詳細情報」を押してから「実行」を押します。
* **Mac**: `sorus-{{VERSION}}-darwin-arm64.zip`（Intel の Mac は `darwin-amd64`）を展開し、`sorus` をダブルクリックします。「開けません」と出たら、「システム設定」→「プライバシーとセキュリティ」を開き、「このまま開く」を押します。
* **Linux**: `sorus-{{VERSION}}-linux-amd64.tar.gz`（ARM は `linux-arm64`）を展開し、`sorus` を実行します。

Windows 11 の「スマート アプリ コントロール」がオンのパソコンでは、署名のないアプリはどの方法でも開けません。署名付きの版の公開をお待ちください。

### ファイルの確かめ方（上級者向け）

`SHA256SUMS` に全ファイルのハッシュがあります。サーバー版（Docker・systemd で動かす人向け）は `sorus-server-{{VERSION}}-linux-*.tar.gz` です。
