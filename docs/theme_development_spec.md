# テーマ開発仕様書（AI エージェント向け）

この文書は、sorus の公開サイトで使える独自テーマ（カスタムテーマ）を作るための仕様です。Cursor・Claude Code・GitHub Copilot・Windsurf などの AI エージェントにこの文書を渡すと、sorus がそのまま取り込めるテーマを作れるように書いています。人が読んで手で作る場合も、同じ仕様に従ってください。

* 取り込みと適用の操作は [サイト管理者ガイド「独自テーマの追加と利用」](site_admin_guide.md#独自テーマの追加と利用) を参照してください。
* sorus 側の実装（検証・配信・SSRF 防御など）は [開発者ガイド「独自テーマ（カスタムテーマ）」](developer_guide.md#独自テーマカスタムテーマ) を参照してください。
* この文書の内容は、`internal/theme/theme.go`（検証）・`internal/theme/importer.go`（取り込み）・`internal/server/handlers_theme_custom.go`（配信）・`static/css/public.css`（既定のトークン）・`templates/public/`（HTML の構造）・`internal/markdown/markdown.go`（本文の HTML のサニタイズ）に合わせています。食い違いがあればコードが正です。

---

## 目次

1. [AI エージェント向けワンショットプロンプト](#1-ai-エージェント向けワンショットプロンプト)
2. [テーマの最小構成とディレクトリ構造](#2-テーマの最小構成とディレクトリ構造)
3. [theme.json の完全スキーマ](#3-themejson-の完全スキーマ)
4. [CSS 変数（デザイントークン）とデザインシステム](#4-css-変数デザイントークンとデザインシステム)
5. [HTML 構造と対象セレクタ](#5-html-構造と対象セレクタ)
6. [セキュリティ制約とバリデーションルール](#6-セキュリティ制約とバリデーションルール)
7. [GitHub リポジトリでの公開と配布](#7-github-リポジトリでの公開と配布)
8. [提出前のチェックリスト](#8-提出前のチェックリスト)

---

## 1. AI エージェント向けワンショットプロンプト

次のブロックを、AI エージェントのプロンプト欄にそのまま貼り付けます。先頭の「作りたいテーマ」の3行だけを書き換えてください。エージェントがこの文書を読める場合は、あわせて `docs/theme_development_spec.md` を開くよう指示すると、より正確になります。

```text
あなたは sorus（Go 製 CMS）の公開サイト用テーマを作ります。次の仕様を厳密に守り、テーマのリポジトリ一式を作ってください。

# 作りたいテーマ
- テーマ名（フォルダ名・リポジトリ名）: my-theme
- 雰囲気: （例: 和紙のような落ち着いた配色、明朝体の見出し、角丸は小さめ）
- 配色モード: light dark（明るい画面と暗い画面の両方に対応）

# 作るファイル（これ以外は作らない。README.md と LICENSE は作ってよい）
my-theme/
  theme.json   必須
  theme.css    必須
  assets/      任意（画像・フォント。CSS から相対パスで参照するものだけ）

# theme.json の規則
- JSON オブジェクト1つだけ。書ける項目は name, description, color_scheme, order, layout の5つだけ。
  それ以外の項目（version, author, license など）を1つでも書くと取り込みが拒否される。
- source_url は sorus が取り込み時に自動で記録する項目なので、作者は書かない。
- name: {"ja": "日本語の名前", "en": "English Name"}。ja は必須、en も必ず書く。
- description: {"ja": "日本語の説明", "en": "English description"}。ja は必須、en も必ず書く。
- color_scheme: "light"・"dark"・"light dark" のどれか。
- order: 整数（任意）。管理画面での並び順。100 以上を使う（標準テーマは 10〜40）。
- layout: オブジェクト（任意）。ヘッダー・トップのヒーロー・記事一覧の骨格を選ぶ。省略した項目は既定値になる。
  - header_style: "inline"（既定）・"centered"・"minimal" のどれか。
  - hero_layout: "stacked"（既定）・"centered"・"split" のどれか。
  - article_list: "grid"（既定）・"list"・"magazine" のどれか。

# theme.css の規則
- 純粋な CSS だけ。@import を書かない。@layer で囲まない。
- http:・https: を CSS のどこにも書かない（コメント以外は文字列の中でも拒否される）。// で始まる URL も書かない。
- 画像・フォントは assets/ に入れ、url("assets/ファイル名") の相対パスで参照する。
- 使える拡張子: .css .png .jpg .jpeg .gif .webp .avif .svg .ico .woff .woff2 .ttf .otf。これ以外のファイルは配信されない。
- JavaScript・HTML は使えない（テーマは CSS だけ）。
- :root で次の19個のトークンをすべて定義する:
  色: --bg --surface --fg --muted --border --accent --accent-fg --accent-soft --mark --code-bg --code-fg
  書体: --font-body --font-heading --font-mono
  形・幅: --radius --radius-lg --content-width --prose-width --shadow
- color_scheme に dark を含むなら、最上位に @media (prefers-color-scheme: dark) { :root { ... } } を置き、色の11個を暗い配色で定義し直す。
- CSS の color-scheme プロパティは書かない（theme.json の color_scheme が決める）。
- 部品の見た目は、次の公開クラスとその子孫だけで変える。h1, h2, h3, a, code, pre, blockquote, body を単独のセレクタで選ばない（.prose h2 のように公開クラスの下に限る）。
  .site-header .site-header--centered .site-header--minimal .header-inner .site-title .search-form .category-nav .site-menu .site-footer
  .page-title .lead .empty .meta .badge .breadcrumb
  .card-grid .card-grid--list .card-grid--magazine .card .card-media .card-body .card-title .card-excerpt .pagination .page-status
  .article .article-header .article-title .hero .toc .toc-title .prose .scholarly-meta
  .attachments .attachments-title .attachment-cards .attachment-card
  .related-articles .related-article-grid .related-article-card .related-article-media .related-article-body
  .page .front-hero .front-hero--centered .front-hero--split .front-cards .front-card .front-card-media .front-card-body .front-card-title .front-card-excerpt
  .front-news .front-news-more .front-gallery .photo-gallery .gallery-item
  .search-hits .search-hit .snippet .result-count .not-found
- 色・角丸・幅は、部品の規則の中でもできるだけ var(--accent) などのトークンで書く。
- 絵文字を使わない（theme.json・theme.css・README のすべて）。
- 文字コードは UTF-8。

# 仕上げ
- 文字と背景のコントラストは WCAG AA（本文 4.5:1 以上）を満たす。明るい配色と暗い配色の両方で確かめる。
- フォントを同梱する場合は @font-face で url("assets/xxx.woff2") を指し、ライセンス上再配布できるものだけを使う。
  同梱しない場合は OS のフォントを並べる（例: system-ui, "Hiragino Sans", "Noto Sans JP", Meiryo, sans-serif）。
- README.md に、テーマの説明と「sorus の管理画面の 設定 > 外観 > テーマ > 独自テーマの追加 にこのリポジトリの URL を貼る」取り込み方を書く。
- 全ファイルの合計は 10MB 未満（Zip にしたとき）にする。
```

---

## 2. テーマの最小構成とディレクトリ構造

```mermaid
flowchart LR
  n0["my-theme/"]
  n0 --> n1["theme.json<br/>必須: テーマのメタデータ"]
  n0 --> n2["theme.css<br/>必須: テーマのスタイルシート"]
  n0 --> n3["README.md<br/>任意: 説明。配信はされない"]
  n0 --> n4["assets/<br/>任意: 画像、フォントなどの静的アセット"]
  n4 --> n5["font.woff2"]
  n4 --> n6["bg.png"]
```

* `theme.json` と `theme.css` は、テーマのフォルダの**直下**に置きます。`assets/` の名前と階層は自由ですが、CSS からの相対パスと一致させます。
* Zip で配る場合は、次のどちらかの形にします。
  * Zip の直下に `theme.json` がある（フォルダで包まない）。
  * Zip の直下にフォルダが**1つだけ**あり、その直下に `theme.json` がある（GitHub・GitLab が作る Zip はこの形です）。
* `__MACOSX/`・`.DS_Store`・`Thumbs.db` は取り込み時に無視されます。名前が `.` で始まるファイル・フォルダは配信されません。

### テーマ ID の決まり方

テーマ ID は、sorus の中でテーマを区別する名前で、配信 URL（`/custom-themes/<id>/`）にも使われます。取り込み時に次の順で決まります。

1. Zip を包んでいる1つのフォルダの名前（`my-theme-main` のように末尾が `-main`・`-master` なら、その部分を除く）
2. 取り込み元の名前（GitHub・GitLab ならリポジトリ名、Zip の URL ならファイル名から `.zip` を除いたもの）
3. `theme.json` の `name.en`

それぞれ小文字にし、使えない文字は `-` に置き換えたうえで、`^[a-z0-9][a-z0-9_-]{0,40}$`（英小文字・数字で始まり、英小文字・数字・`-`・`_` の42文字以内）に合うものを使います。フォルダ名・リポジトリ名を最初からこの形にしておくのが確実です。

* 標準テーマと同じ ID（`modern`・`tech`・`news`・`portal`）は取り込めません。
* すでに同じ ID の独自テーマがあると、新しい内容で置き換えます（テーマの更新）。
* 公開サーバー連携で「公開サーバーへ反映する」を押すと、PC で決まった ID のまま公開サーバーへ送られます。送るのは `theme.json` と、公開サイトで使うファイル（`theme.css` と、そこから相対 `url()` で読む画像・フォント）だけです。README などは送りません。
* 画像・フォントの URL には版が付かないため、sorus はブラウザ・CDN に毎回確かめさせて配信します。テーマを更新するときにファイル名を変える必要はありません。

---

## 3. theme.json の完全スキーマ

```json
{
  "name":        { "ja": "和紙", "en": "Washi" },
  "description": { "ja": "和紙のような落ち着いた配色と明朝体の見出し", "en": "Calm paper-like colors with serif headings" },
  "color_scheme": "light dark",
  "order": 100,
  "layout": {
    "header_style": "centered",
    "hero_layout": "split",
    "article_list": "magazine"
  }
}
```

| 項目 | 型 | 必須 | 内容 |
| :--- | :--- | :--- | :--- |
| `name` | `{"ja": 文字列, "en": 文字列}` | `ja` は必須。`en` は推奨 | 管理画面に出す名前。英語表示で `en` が無いときは `ja` を使う |
| `description` | `{"ja": 文字列, "en": 文字列}` | `ja` は必須。`en` は推奨 | 管理画面のカードに出す説明文。英語表示で `en` が無いときは `ja` を使う |
| `color_scheme` | 文字列 | 必須 | `"light"`・`"dark"`・`"light dark"` のいずれか。公開ページの `<meta name="color-scheme">` になる |
| `order` | 整数 | 任意 | 管理画面での並び順（小さい順。同じなら ID の順）。標準テーマは 10〜40 を使っているので、100 以上を勧める |
| `layout` | オブジェクト | 任意 | 公開ページの宣言的レイアウト設定。下表の3項目を任意に組み合わせられる（省略時はすべて既定値） |
| `source_url` | 文字列 | 書かない | 取り込み元の URL。URL から取り込んだとき sorus が自動で書き込む。作者が書く必要はない |

### `layout` オブジェクトの項目（すべて任意）

| 項目 | 許可値 | 既定値 | 公開 HTML に付く修飾クラスと挙動 |
| :--- | :--- | :--- | :--- |
| `header_style` | `"inline"`・`"centered"`・`"minimal"` | `"inline"` | `"centered"` は `.site-header--centered`（サイト名とメニューを中央揃え）、`"minimal"` は `.site-header--minimal`（境界線と余白を抑えた簡潔なヘッダー）を `.site-header` に付ける |
| `hero_layout` | `"stacked"`・`"centered"`・`"split"` | `"stacked"` | `"centered"` は `.front-hero--centered`（中央揃え）、`"split"` は `.front-hero--split`（アイキャッチ画像があるときはPCで2カラム、無いときは中央寄せ）をトップ固定ページの `.front-hero` に付ける |
| `article_list` | `"grid"`・`"list"`・`"magazine"` | `"grid"` | `"list"` は `.card-grid--list`（1列の横長カード一覧）、`"magazine"` は `.card-grid--magazine`（先頭記事カードを大きく表示するマガジン配置）を `.card-grid` に付ける |

* 既定値（`"inline"`・`"stacked"`・`"grid"`）のときは修飾クラスを付けません。
* 修飾クラスのレスポンシブな骨格は本体の `public.css`（`@layer sorus-base`）に入っているため、`theme.json` の `layout` を書くだけで配置が切り替わります。`theme.css` から修飾クラスを上書きして細部を調整することもできます。

検証の規則（`ValidateThemeDir`。標準テーマ・独自テーマ・取り込みのすべてが同じ検証を通ります）:

* **上の表にない項目は拒否されます**（`DisallowUnknownFields`）。`version`・`author`・`license`・`homepage` なども書けません。`layout` の中も `header_style`・`hero_layout`・`article_list` 以外の項目があると拒否されます。版や作者は README.md に書いてください。
* JSON オブジェクトの後ろに余分なデータがあると拒否されます。
* `name.ja`・`description.ja` が空（空白だけを含む）だと拒否されます。
* `color_scheme` が3つの値のどれでもないと拒否されます（`"auto"`・`"Dark"` も不可）。
* `layout` の各項目が空文字でも許可値のどれでもないと拒否されます。
* 文字コードは UTF-8 にします。絵文字は使いません。

---

## 4. CSS 変数（デザイントークン）とデザインシステム

### カスケードの仕組み

公開ページは次の順に CSS を読み込みます。

```html
<link rel="stylesheet" href="/static/css/public.css?v=...">   <!-- 本体。全体が @layer sorus-base の中 -->
<link rel="stylesheet" href="/custom-themes/<id>/theme.css?v=...">  <!-- テーマ -->
```

* 本体の `public.css` は全体を `@layer sorus-base { ... }` で囲んでいます。レイヤーに入っていない `theme.css` の指定は、詳細度に関係なく本体より優先されます。セレクタを強くする必要はありません。
* `theme.css` を `@layer` で囲まないでください。優先順位が逆転し、上書きが効かなくなります。
* `<html data-theme="<id>">` が出力されるので、`[data-theme="my-theme"]` で自分のテーマだけに効く指定も書けます。

### トークンの完全一覧

`public.css` の `:root` に全トークンの既定値があり、テーマは `theme.css` の `:root` でこれを上書きします。次の19個は**すべて定義してください**。省略しても取り込みは拒否されませんが、本体の既定値（藍色のアクセント）が混ざり、意図しない見た目になります。標準テーマは自動テストでこの19個の定義を強制しています。

| 分類 | トークン | 意味 | 本体の既定値（明るい） |
| :--- | :--- | :--- | :--- |
| 色 | `--bg` | ページの背景色 | `#ffffff` |
| 色 | `--surface` | 目次・検索欄・コードなどの面の色 | `#f8fafc` |
| 色 | `--fg` | 本文の文字色 | `#1e293b` |
| 色 | `--muted` | 補助文字の色（日付、リード文、メニュー） | `#64748b` |
| 色 | `--border` | 境界線の色 | `#e2e8f0` |
| 色 | `--accent` | リンク・ボタン・バッジの強調色 | `#4338ca` |
| 色 | `--accent-fg` | アクセント色の上に載せる文字の色 | `#ffffff` |
| 色 | `--accent-soft` | 控えめな強調の背景色（バッジ、メニューのホバー） | `#eef2ff` |
| 色 | `--mark` | 検索語・マーカー（`mark`）の背景色 | `#fef08a` |
| 色 | `--code-bg` | 記事のコードブロックの背景色 | `#0f172a` |
| 色 | `--code-fg` | 記事のコードブロックの文字色 | `#e2e8f0` |
| 書体 | `--font-body` | 本文の書体 | `system-ui, -apple-system, "Hiragino Sans", ...` |
| 書体 | `--font-heading` | 見出し（`.page-title`・`.article-title`・`.prose h2` など）の書体 | `var(--font-body)` |
| 書体 | `--font-mono` | 等幅の書体（コード） | `ui-monospace, "SFMono-Regular", Menlo, ...` |
| 形・幅 | `--radius` | 入力欄・ボタン・コードなど小さい要素の角丸 | `8px` |
| 形・幅 | `--radius-lg` | カード・目次・ヒーロー画像などの角丸 | `12px` |
| 形・幅 | `--content-width` | ページ全体の最大幅 | `1080px` |
| 形・幅 | `--prose-width` | 記事本文の最大幅 | `760px` |
| 形・幅 | `--shadow` | カードなどの影（不要なら `none`） | `0 1px 3px rgb(15 23 42 / .08)` |

テーマだけで使う色が必要なときは、`--my-theme-header-bg` のように**テーマ ID を接頭辞にした**独自のトークンを `:root` に足してかまいません（標準テーマ `portal` の `--portal-header-bg` などが例です）。

### ライトモード / ダークモード

`theme.json` の `color_scheme` で、テーマが対応する配色を宣言します。

| `color_scheme` | 意味 | CSS で書くこと |
| :--- | :--- | :--- |
| `"light"` | 明るい配色だけ | `:root` に19個 |
| `"dark"` | 暗い配色だけ | `:root` に19個（暗い色で）。加えて下の `@media` に色の11個を書くことを勧める |
| `"light dark"` | 閲覧者の OS の設定に合わせて切り替え | `:root` に19個（明るい色）と、下の `@media` に色の11個（暗い色） |

```css
:root {
  --bg: #fbf8f1; --surface: #ffffff; --fg: #2b2a27; --muted: #6b655d; --border: #e4ded2;
  --accent: #8a4b2a; --accent-fg: #ffffff; --accent-soft: #f5e9dc; --mark: #f6e27a;
  --code-bg: #2b2a27; --code-fg: #f3efe6;
  --font-body: "Hiragino Sans", "Noto Sans JP", Meiryo, sans-serif;
  --font-heading: "Hiragino Mincho ProN", "Noto Serif JP", serif;
  --font-mono: ui-monospace, Menlo, Consolas, monospace;
  --radius: 4px; --radius-lg: 8px; --content-width: 1080px; --prose-width: 720px;
  --shadow: none;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #1b1a17; --surface: #252420; --fg: #ece8df; --muted: #a39d93; --border: #3a372f;
    --accent: #e0a27f; --accent-fg: #1b1a17; --accent-soft: #3a2a20; --mark: #6b5a16;
    --code-bg: #111010; --code-fg: #ece8df;
  }
}
```

* ダークモードの `@media (prefers-color-scheme: dark)` は、**最上位**に書きます（ほかの `@media` や規則の中に入れない）。
* `color-scheme` プロパティを CSS に書かないでください。`<meta name="color-scheme">`（`theme.json` の値）より優先されてしまい、宣言と食い違います。
* 書体・形・幅のトークンは、明るい `:root` だけに書けば足ります。
* 文字と背景のコントラストは WCAG AA（本文 4.5:1、大きな文字 3:1）以上にします。`--accent` と `--accent-fg`、`--code-bg` と `--code-fg` の組み合わせも確かめてください。

### フォントと画像の同梱

```css
@font-face {
  font-family: "Washi Serif";
  src: url("assets/washi-serif.woff2") format("woff2");
  font-display: swap;
}
:root { --font-heading: "Washi Serif", "Hiragino Mincho ProN", serif; }
.site-header { background: var(--bg) url("assets/header.webp") center / cover no-repeat; }
```

* `url()` は `theme.css` からの相対パスで書きます。`theme.css` は `/custom-themes/<id>/theme.css` で配信されるため、`url("assets/x.woff2")` は `/custom-themes/<id>/assets/x.woff2` になります。サイトの静的書き出しでも同じパスに書き出されます。
* 日本語フォントは1ファイルが数 MB になります。Zip の上限（10MB）に収まるよう、サブセット化した `woff2` を使うか、OS のフォントを並べてください。
* 再配布が許されたフォント・画像だけを入れます。

---

## 5. HTML 構造と対象セレクタ

公開ページの HTML は全テーマ共通です（`templates/public/`）。テーマは HTML を変えられず、CSS だけで見た目を変えます。

### ページの骨組み

```html
<html lang="ja" data-theme="<id>">
<body>
  <a class="skip-link" href="#main">本文へスキップ</a>
  <header class="site-header">
    <div class="container header-inner">
      <a href="/" class="site-title">サイト名</a>
      <form class="search-form" role="search"><input type="search"><button>検索</button></form>
    </div>
    <nav class="category-nav">                      <!-- メニューかカテゴリの一覧 -->
      <ul class="container site-menu">              <!-- サイト構成のメニューのとき site-menu が付く -->
        <li><a href="..." aria-current="page">...</a></li>
      </ul>
    </nav>
  </header>
  <main id="main" class="container"> ...各ページの中身... </main>
  <footer class="site-footer"><div class="container"><p>&copy; サイト名</p><p><a href="/feed.xml">RSS</a></p></div></footer>
</body>
```

### 記事一覧（トップ・記事の一覧・カテゴリ）

```html
<h1 class="page-title">新着記事</h1>
<p class="lead">カテゴリの説明</p>                  <!-- カテゴリのページだけ -->
<div class="card-grid">
  <article class="card">
    <a class="card-media"><img></a>
    <div class="card-body">
      <a class="badge">カテゴリ名</a>
      <h2 class="card-title"><a>記事タイトル</a></h2>
      <p class="card-excerpt">抜粋</p>
      <p class="meta"><time>2026-11-02</time></p>
    </div>
  </article>
</div>
<nav class="pagination"><a rel="prev">前へ</a><span class="page-status">1 / 3</span><span class="disabled">次へ</span></nav>
```

### 記事詳細・固定ページ

```html
<nav class="breadcrumb"><ol><li><a href="/">ホーム</a></li><li aria-current="page">タイトル</li></ol></nav>
<article class="article">                         <!-- 固定ページは class="page" -->
  <header class="article-header">
    <a class="badge">カテゴリ名</a>
    <h1 class="article-title">記事タイトル</h1>
    <p class="meta"><time>...</time><span>・文: <span class="author">著者</span></span></p>
  </header>
  <figure class="hero"><img></figure>
  <nav class="toc"><p class="toc-title">目次</p><ul><li class="toc-level-2"><a>見出し</a></li></ul></nav>
  <div class="prose"> Markdown から変換した本文（h2〜h6, p, ul, ol, blockquote, pre, code, table, img, mark） </div>
  <section class="attachments"><h2 class="attachments-title">添付ファイル</h2><ul class="attachment-cards">...</ul></section>
  <section class="related-articles"><h2>関連記事</h2><div class="related-article-grid">...</div></section>
</article>
```

### セレクタ対応表（公開クラス）

| 部位 | セレクタ | 備考 |
| :--- | :--- | :--- |
| ヘッダー | `.site-header` `.site-header--centered` `.site-header--minimal` `.header-inner` `.site-title` | `.site-title` はサイト名のリンク。`--centered`・`--minimal` は `layout.header_style` で付く |
| 検索欄 | `.search-form` `.search-form input` `.search-form button` `.search-form-large` | `-large` は検索ページの大きい欄 |
| ナビゲーション | `.category-nav` `.category-nav a` `.site-menu` `.category-nav a[aria-current="page"]` | 表示中の項目は `aria-current="page"` |
| ページ見出し | `.page-title` `.lead` `.empty` | `.empty` は「記事がありません」 |
| パンくず | `.breadcrumb` `.breadcrumb ol` `.breadcrumb a` | |
| 記事一覧（カード） | `.card-grid` `.card-grid--list` `.card-grid--magazine` `.card` `.card-media` `.card-body` `.card-title` `.card-excerpt` `.meta` `.badge` | `.card-grid` は CSS Grid。`--list`・`--magazine` は `layout.article_list` で付く |
| ページネーション | `.pagination` `.pagination a` `.pagination .disabled` `.page-status` | |
| 記事詳細 | `.article` `.article-header` `.article-title` `.hero` `.meta` `.author` `.scholarly-meta` | `.scholarly-meta` は論文の書誌情報 |
| 目次 | `.toc` `.toc-title` `.toc ul` `.toc li` `.toc-level-2`〜`.toc-level-6` | |
| 本文 | `.prose` `.prose h2` `.prose h3` `.prose h4` `.prose p` `.prose a` `.prose ul` `.prose ol` `.prose blockquote` `.prose pre` `.prose code` `.prose :not(pre) > code` `.prose table` `.prose th` `.prose td` `.prose img` | 本文の要素は必ず `.prose` の下で選ぶ |
| 添付ファイル | `.attachments` `.attachments-title` `.attachment-cards` `.attachment-card` `.attachment-type` `.attachment-name` `.attachment-meta` | |
| 関連記事 | `.related-articles` `.related-article-grid` `.related-article-card` `.related-article-media` `.related-article-body` | |
| 固定ページ | `.page` | |
| ドキュメント表示 | `.doc-layout` `.doc-main` `.doc-sidebar` `.doc-sidebar-collapsible` `.doc-sidebar-summary` `.doc-sidebar-nav` `.doc-sidebar-title` `.doc-sidebar-links` `.doc-pager` `.doc-pager-link` `.doc-pager-prev` `.doc-pager-next` `.doc-pager-label` `.doc-pager-title` | ドキュメントとして並べたページだけに出る。表示中のページは `aria-current="page"`。`.doc-sidebar-collapsible` は `<details>` |
| トップ（サイト構成） | `.front-hero` `.front-hero--centered` `.front-hero--split` `.front-cards` `.front-card` `.front-card-media` `.front-card-body` `.front-card-title` `.front-card-excerpt` `.front-news` `.front-news-more` | 固定ページをトップにしたときに出る。`--centered`・`--split` は `layout.hero_layout` で付く |
| フォトギャラリー | `.front-gallery` `.photo-gallery` `.masonry-grid` `.gallery-item` `.gallery-item img` `.gallery-item figcaption` | |
| 本文の注意書き | `.callout` `.callout-note` `.callout-tip` `.callout-important` `.callout-warning` `.callout-caution` `.callout-title` `.callout-body` | `> [!NOTE]` などから出る（`div.callout.callout-note > p.callout-title + div.callout-body`） |
| 本文のタブ | `.doc-tabs` `.doc-tabs.is-ready` `.doc-tab-list` `.doc-tab` `.doc-tab[aria-selected="true"]` `.doc-tab-panel` `.doc-tab-title` | `:::tabs` から出る。JavaScript が無いときはすべての中身が名前（`.doc-tab-title`）付きで縦に並ぶ。切り替えに使う `hidden` 属性の要素を `display` で表示し直さないでください |
| 本文の図・数式 | `.mermaid` `.mermaid-diagram` `.math-inline` `.math-display` | `pre.mermaid` は図の定義で、描いた図は直後の `.mermaid-diagram` に入る |
| 本文のカード | `.doc-cards` `.doc-card` | `:::cards` から出る（`div.doc-cards > div.doc-card`）。カードの中は通常の本文（`h3`・`p`・`a` など）。`.doc-cards` を CSS Grid にする想定 |
| 本文の行動の呼びかけ | `.doc-cta` `.doc-cta-body` `.doc-cta-actions` `.doc-cta-actions a` | `:::cta` から出る（`div.doc-cta > div.doc-cta-body + div.doc-cta-actions`）。どちらかの欄が空なら、その `div` は出ない。リンクは `.doc-cta-actions` の下の `p` か `ul` の中にある |
| 本文のよくある質問 | `.doc-faq` `.doc-faq-item` `.doc-faq-item[open]` `.doc-faq-q` `.doc-faq-a` | `:::faq` から出る（`div.doc-faq > details.doc-faq-item > summary.doc-faq-q + div.doc-faq-a`）。JavaScript を使わずに開閉する |
| 検索結果 | `.result-count` `.search-hits` `.search-hit` `.snippet` `mark` | `mark` は `.snippet mark` のように範囲を絞る |
| 404 | `.not-found` | |
| フッター | `.site-footer` `.site-footer p` `.site-footer a` | |
| 共通 | `.container` | 幅は `--content-width` で変える |

上書きしないもの:

* `h1`・`h2`・`h3`・`a`・`code`・`pre`・`blockquote`・`body` を、公開クラスの外で単独に選ばないでください（`.prose h2` は可、`h2` は不可）。プレビューのバーなど、テーマの外の部品まで変わってしまいます。
* `.theme-preview-*`（管理画面のプレビューのバー）、`.cms-lightbox*`（画像の拡大表示）、`.skip-link`・`.visually-hidden`（読み上げ用）は変えないでください。
* `display: none` で本文・メニュー・検索欄を隠さないでください。

### 本文の生 HTML のクラス

記事の本文（Markdown）に書いた生 HTML のうち、次の要素の `class` は公開ページに残ります。テーマは `.prose` の下でこれらのクラスを選べます（例: `.prose .profile-card`、`.prose figure.article-figure img`）。クラス名は記事の書き手とテーマの作者が取り決めるもので、sorus 側の決まった一覧はありません。

| 項目 | 規則 |
| :--- | :--- |
| 対象の要素 | `div` `p` `figure` `figcaption` `img` `ul` `ol` `li` `table` `blockquote` `section` `span` |
| クラス名 | 英数字・`_`・`-` だけ。空白区切りで複数可（例: `class="profile-card wide"`） |
| 予約名 | `math-inline` `math-display` `mermaid` `photo-gallery` `masonry-grid` `gallery-item` `gallery-more` `gallery-count` `gallery-more-button` `gallery-layout-*` `doc-tabs` `doc-tab-list` `doc-tab` `doc-tab-panel` `doc-tab-title` は数式・Mermaid の図・フォトギャラリー・タブ用（スクリプトが目印に使う）。sorus が出力したときだけ残り、生 HTML のクラスに混ぜると `class` 属性ごと消えます |
| 予約しない部品のクラス | 注意書き（`callout` など）・カード（`doc-cards` `doc-card`）・行動の呼びかけ（`doc-cta` など）・よくある質問（`doc-faq` など）はスクリプトを使わないため、生 HTML の `div` にも書けます。`details` には `doc-faq-item`、`summary` には `doc-faq-q` だけが残ります |

* 規則に合わない値（`md:flex` のような記号入り、日本語、前後の空白、1つでも合わないクラス名を含むもの）は、`class` 属性ごと消えます。要素と中身は残ります。
* 表にない要素（`a`・`h2`・`td`・`pre`・`code` など）の `class` は消えます。
* `style`・イベント属性（`onclick` など）・`javascript:` の URL・`youtube-nocookie.com` の埋め込み以外の `iframe` は、クラスの有無にかかわらず消えます。
* クラスはテーマではなく記事に保存されます。テーマを替えると効かなくなるため、クラスのスタイルが無くても読める HTML にしてください。

---

## 6. セキュリティ制約とバリデーションルール

sorus は取り込むテーマを次の規則で検査し、1つでも違反があれば取り込みを拒否します（または配信しません）。オフラインで動くこと・閲覧者のプライバシーを守ること・管理画面と同じオリジンで任意のコードを動かさないことが目的です。

### theme.css の検査（取り込み時に拒否）

検査はコメント（`/* */`）と属性セレクタの値（`a[href^="https://"]`）を除き、CSS のエスケープ（`\40 import` など）を戻してから行います。

| 規則 | 拒否される例 | 代わりに |
| :--- | :--- | :--- |
| `@import` は使えない | `@import "base.css";` `@import url(fonts.css);` | 1つの `theme.css` にまとめる |
| `http:`・`https:` を含めない（`url()` の外の文字列も対象） | `url("https://fonts.example/x.woff2")` `content: "https://..."` | `assets/` に同梱して `url("assets/x.woff2")` |
| プロトコル相対 URL は使えない | `url(//cdn.example/bg.png)` `src: url("//...")` | 同上 |
| `theme.css` が無い | | テーマのフォルダの直下に置く |

`data:` URL（`url("data:image/svg+xml,...")`）は検査では拒否されませんが、ファイルが大きくなり読みにくくなるため、`assets/` のファイルを勧めます。

### 配信の制限（取り込み後）

* **JavaScript は使えません**。テーマは CSS だけです。`.js`・`.html` などのファイルを入れても配信されません。CSS の `expression()` や `behavior` のような古いブラウザ向けの仕組みも使いません。
* 配信されるのは、次の拡張子の通常のファイルだけです（`/custom-themes/<id>/...`）。

  `.css` `.png` `.jpg` `.jpeg` `.gif` `.webp` `.avif` `.svg` `.ico` `.woff` `.woff2` `.ttf` `.otf`

* `theme.json`・`README.md`・名前が `.` で始まるファイル・テーマのフォルダの外を指すシンボリックリンクは配信されません。
* 配信するファイルには `Content-Security-Policy`（`default-src 'none'; ... sandbox`）と `X-Content-Type-Options: nosniff` が付きます。SVG の中にスクリプトを書いても実行されません。SVG は画像としてだけ使ってください。

### Zip の制限（取り込み時に拒否）

| 規則 | 上限・内容 |
| :--- | :--- |
| Zip の大きさ | 10MB まで（URL からのダウンロードも同じ） |
| 展開後の合計 | 50MB まで |
| ファイル数 | 500 個まで（フォルダは 1000 個まで） |
| 危険なパス | `..` を含むもの・絶対パス・ドライブ名（`C:`）・バックスラッシュを含むものは拒否 |
| 特殊なファイル | シンボリックリンク・デバイスファイルは拒否 |
| 重複 | 大文字小文字だけが違う同名のファイル（`Theme.css` と `theme.css`）は拒否 |

### URL からの取り込みの制限

* `https://` の URL だけを受け付けます。`http://`・ユーザー名やパスワードを含む URL（`https://user:pass@...`）は拒否します。
* 取り込める URL は次の3種類です。
  * `https://github.com/{owner}/{repo}`（`.git` 付きも可。`/tree/...` などのサブパスは不可）
  * `https://gitlab.com/{group}/{repo}`（サブグループも可）
  * `.zip` で終わる `https://` の URL
* GitHub・GitLab は `main` ブランチ、無ければ `master` ブランチの Zip を取ります。ほかのブランチ・タグは、その Zip の URL を直接指定します（例: `https://github.com/owner/my-theme/archive/refs/tags/v1.0.0.zip`）。
* GitHub が自動で作るタグの Zip（`archive/refs/tags/v1.0.0.zip`）は、中身が `my-theme-1.0.0` のようなフォルダで包まれています。末尾が `-main`・`-master` ではないため、テーマ ID は `my-theme-1-0-0`（`<repo>-1-0-0`）になり、リポジトリ URL から取り込んだ `my-theme` とは別のテーマとして追加されます。
* 誰でも見られる（パブリックな）リポジトリ・URL だけが取り込めます。ログインが必要なものは取れません。
* プライベート IP・ループバック・リンクローカル（クラウドのメタデータの `169.254.169.254` を含む）などへの接続は拒否します。リダイレクトは5回まで、ダウンロードは管理画面から45秒までです。

---

## 7. GitHub リポジトリでの公開と配布

### 公開の手順

1. GitHub で新しいリポジトリを作ります。名前はテーマ ID にしたい名前（例: `my-theme`）にし、**Public** を選びます。既定のブランチは `main` のままにします。
2. リポジトリの**直下**に `theme.json`・`theme.css`（と `assets/`、`README.md`）を置きます。`my-theme/theme.json` のように、さらにフォルダで包まないでください。
3. コミットして `main` に push します。

   ```sh
   git init -b main
   git add theme.json theme.css assets README.md
   git commit -m "Add my-theme"
   git remote add origin https://github.com/owner/my-theme.git
   git push -u origin main
   ```

4. ブラウザで `https://github.com/owner/my-theme` を開き、直下に `theme.json` があることを確かめます。

GitLab でも同じです（`https://gitlab.com/owner/my-theme`）。

### sorus での取り込み

1. sorus の管理画面で「設定」→「外観」→「テーマ」を開きます。
2. 「独自テーマの追加」の「リポジトリ URL から追加」に `https://github.com/owner/my-theme` を貼り付け、「URL から取り込む」を押します。
3. テーマの一覧に「独自テーマ」の印が付いたカードが増えます。「プレビュー」で確かめ、「このテーマを選ぶ」を選んで「このテーマを適用」を押します。

AI エージェントから操作する場合は、MCP ツール `cms_install_theme`（`url` に同じ URL を渡す）と `cms_activate_theme` を使います（[開発者ガイド](developer_guide.md#独自テーマカスタムテーマ)）。

### 更新と版

* テーマを直したら `main` に push し、sorus で同じ URL をもう一度取り込みます。同じ ID のテーマが新しい内容に置き換わります。適用中のテーマを置き換えた場合も、ブラウザは新しい CSS を読み込みます（URL に更新時刻の版が付くため）。
* 同じ ID のまま置き換えて更新するには、リポジトリ URL（`https://github.com/owner/my-theme`。常に `main`、無ければ `master` を取り込む）を指定します。
* 版を固定して配りたい場合は、タグ（例: `v1.0.0`）を打ち、タグの Zip の URL を案内します。ただしタグの Zip から取り込むと、テーマ ID は `my-theme-1-0-0`（`<repo>-1-0-0`）のように版付きになり、既存の `my-theme` を置き換えずに別のテーマとして追加されます。版を固定しつつ同じ ID で置き換えたい場合は、中身をテーマ ID 名のフォルダ（`my-theme/theme.json` など）で包んだ Zip を自分で作って配ります。`theme.json` に `version` は書けないので、版は README.md とタグで表します。
* GitHub の「Releases」に Zip を添付して配ることもできます。その場合も Zip の中身は「2. テーマの最小構成」の形にします。

---

## 8. 提出前のチェックリスト

* [ ] `theme.json` の項目は `name`・`description`・`color_scheme`・`order`・`layout`（任意）だけで、`name.ja`・`description.ja` がある
* [ ] `layout` を書く場合、`header_style`・`hero_layout`・`article_list` が許可値だけで、未知の項目がない
* [ ] `color_scheme` が `"light"`・`"dark"`・`"light dark"` のどれか
* [ ] `theme.css` の `:root` に19個のトークンがすべてある
* [ ] `color_scheme` に `dark` を含むなら、最上位の `@media (prefers-color-scheme: dark)` の `:root` に色の11個がある
* [ ] `@import`・`@layer`・`color-scheme` プロパティを書いていない
* [ ] `http:`・`https:`・`//` で始まる URL がコメント以外のどこにもない
* [ ] `url()` はすべて `assets/...` の相対パスで、そのファイルが実際にある
* [ ] `h1`・`h2`・`a`・`code` などを公開クラスの外で選んでいない
* [ ] アセットの拡張子が配信できるもの（6章）だけ
* [ ] 絵文字を使っていない
* [ ] Zip にして 10MB 未満、ファイルは 500 個以下
* [ ] フォルダ名・リポジトリ名が `^[a-z0-9][a-z0-9_-]{0,40}$` に合い、`modern`・`tech`・`news`・`portal` ではない
* [ ] 明るい配色・暗い配色の両方で、トップ・記事一覧・記事詳細（目次・コード・表・引用を含む）を sorus のプレビューで確かめた
