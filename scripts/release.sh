#!/usr/bin/env bash
# 配布用のリリース一式を dist-release/ に作る。
#
# 使い方（リポジトリのどこからでも可）:
#   ./scripts/release.sh v1.2.3        # または make release VERSION=v1.2.3
#   RELEASE_TARGETS="linux/amd64" ./scripts/release.sh v1.2.3   # 一部の OS/CPU だけ作る
#
# RELEASE_TARGETS は作る対象の os/arch を空白区切りで並べる（省略時は下の全対象）。
# サーバー版は、そのうち linux のものだけを作る。Windows・macOS 向けのクロスビルドは
# メモリを多く使う（7GB 超）ため、手元では linux/amd64 だけにし、全対象は GitHub Actions で作る。
#
# 作るもの:
#   sorus-<版>-<os>-<arch>.zip|tar.gz   デスクトップ版（windows/darwin は zip、linux は tar.gz）
#   sorus-server-<版>-linux-<arch>.tar.gz  サーバー版（Docker・systemd 用と同じビルド）
#   install.sh / install.ps1 / install-server.sh  1行インストール用のスクリプト
#   SHA256SUMS                         上記すべてのハッシュ（sha256sum -c で検証できる）
#
# アプリアイコン（build/icons/。作り方と差し替え方は docs/developer_guide.md）:
#   Windows   build/icons/rsrc_windows_<arch>.syso を、ビルドの間だけリポジトリのルートに置き、sorus.exe に
#             埋め込む（Go が *_windows_<arch>.syso を自動でリンクする）。ビルドの後・失敗時・中断時に消す。
#   macOS     build/icons/sorus.icns を zip に入れる（install.sh が sorus.app に入れる）。
#   Linux     build/icons/sorus-256.png を sorus.png として tar.gz に入れる（install.sh が .desktop に設定する）。
#   資材が無いときは警告を出し、アイコンなしで作る。
#
# UPX 等の圧縮・難読化はしない（ウイルス対策ソフトの誤検知の原因になるため）。
# 使う外部コマンドは bash・go・tar・sha256sum（なければ shasum -a 256）だけ。
# zip は環境に無いことがあるため、Go の標準ライブラリで作る。
set -euo pipefail

VERSION="${1:-${VERSION:-}}"
if [ -z "$VERSION" ]; then
  echo "usage: $0 <version>  (例: $0 v1.2.3)" >&2
  exit 2
fi
case "$VERSION" in
  v[0-9]*) ;;
  *) echo "release.sh: version must look like vX.Y.Z (got: $VERSION)" >&2; exit 2 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/dist-release"

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$@"; }
else
  echo "release.sh: sha256sum or shasum is required" >&2
  exit 1
fi

WORK="$(mktemp -d)"
ICONS="$ROOT/build/icons"
# ICON_SYSO は Windows 向けのビルドの間だけルートに置いた .syso（作業ツリーを汚さないよう、終わったら必ず消す）。
ICON_SYSO=""
cleanup() {
  if [ -n "$ICON_SYSO" ]; then rm -f "$ICON_SYSO"; fi
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

# nodynamic: gen2brain/webp が purego で libwebp を dlopen しないようにする（deploy/Dockerfile と同じ）。
# デスクトップ版にも付け、Linux でも動的ローダーに依存しない静的な実行ファイルにする。
# 利用者の PC に libwebp は通常無く、どのみち WASM 版で動くため失うものはない。
VERSION_FLAG="-X sorus/internal/version.Version=$VERSION"
ALL_DESKTOP_TARGETS="windows/amd64 windows/arm64 darwin/arm64 darwin/amd64 linux/amd64 linux/arm64"
ALL_SERVER_TARGETS="linux/amd64 linux/arm64"
DESKTOP_TARGETS="${RELEASE_TARGETS:-$ALL_DESKTOP_TARGETS}"
# shellcheck disable=SC2086
if [ -z "$(echo $DESKTOP_TARGETS)" ]; then
  echo "release.sh: RELEASE_TARGETS is empty" >&2
  exit 2
fi
SERVER_TARGETS=""
for target in $DESKTOP_TARGETS; do
  case " $ALL_DESKTOP_TARGETS " in
    *" $target "*) ;;
    *) echo "release.sh: unknown target in RELEASE_TARGETS: $target (choose from: $ALL_DESKTOP_TARGETS)" >&2; exit 2 ;;
  esac
  case " $ALL_SERVER_TARGETS " in
    *" $target "*) SERVER_TARGETS="$SERVER_TARGETS $target" ;;
  esac
done

# zip ファイルを作る小さな Go プログラム。実行ビットを保ったまま、フォルダ構造なしで格納する。
mkdir -p "$WORK/zipper"
cat >"$WORK/zipper/go.mod" <<'EOF'
module zipper

go 1.21
EOF
cat >"$WORK/zipper/main.go" <<'EOF'
package main

import (
	"archive/zip"
	"io"
	"log"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: zipper out.zip file...")
	}
	out, err := os.Create(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for _, name := range os.Args[2:] {
		info, err := os.Stat(name)
		if err != nil {
			log.Fatal(err)
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			log.Fatal(err)
		}
		hdr.Name = filepath.Base(name)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			log.Fatal(err)
		}
		f, err := os.Open(name)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := io.Copy(w, f); err != nil {
			log.Fatal(err)
		}
		f.Close()
	}
	if err := zw.Close(); err != nil {
		log.Fatal(err)
	}
	if err := out.Close(); err != nil {
		log.Fatal(err)
	}
}
EOF
(cd "$WORK/zipper" && GOWORK=off GOFLAGS= CGO_ENABLED=0 go build -o "$WORK/zipper-bin" .)

# README.txt の本文。$1 = os（windows/darwin/linux）。
desktop_readme() {
  case "$1" in
    windows)
      cat <<'EOF'
sorus（デスクトップ版）

かんたんな入れ方（おすすめ）
  PowerShell を開き、次の1行を貼り付けて Enter を押してください。
  irm https://github.com/ryuzi/sorus/releases/latest/download/install.ps1 | iex

この Zip から使う場合
  1. Zip を右クリックして「すべて展開」を選びます。
  2. 展開したフォルダの sorus.exe をダブルクリックします。
  3. 「Windows によって PC が保護されました」と出たら、「詳細情報」を押してから「実行」を押します。
  ブラウザで sorus の画面が開きます。終わるときは画面の「sorus を終了」を押してください。

書いた記事などのデータは %LOCALAPPDATA%\sorus に保存されます。
EOF
      ;;
    darwin)
      cat <<'EOF'
sorus（デスクトップ版）

かんたんな入れ方（おすすめ）
  ターミナルを開き、次の1行を貼り付けて Enter を押してください。
  curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh

この Zip から使う場合
  1. 展開してできた sorus をダブルクリックします。
  2. 「開けません」と出たら、「システム設定」→「プライバシーとセキュリティ」を開き、
     下のほうにある「このまま開く」を押します。
  ブラウザで sorus の画面が開きます。終わるときは画面の「sorus を終了」を押してください。

書いた記事などのデータは ~/Library/Application Support/sorus に保存されます。
EOF
      ;;
    linux)
      cat <<'EOF'
sorus（デスクトップ版）

かんたんな入れ方（おすすめ）
  端末を開き、次の1行を貼り付けて Enter を押してください。
  curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh

この tar.gz から使う場合
  展開してできた sorus を実行すると、ブラウザで sorus の画面が開きます。
  終わるときは画面の「sorus を終了」を押してください。
EOF
      ;;
  esac
}

server_readme() {
  cat <<'EOF'
sorus（サーバー版）

公開サーバーで動かすための実行ファイルです。
設定と起動の手順は、リポジトリの docs/operations_guide.md を見てください。
自分の PC で記事を書くだけなら、サーバー版ではなくデスクトップ版を使ってください。
EOF
}

# ARM 向けでは gen2brain/webp だけ最適化を止める（-N）。WASM から変換した巨大な libwebp.go は、
# ARM 向けに最適化するとコンパイル 1つで 6GB を超え、GitHub の標準ランナー（メモリ約 7.7GB）が
# スワップを足しても止まる（2026-10-30）。-N なら約 1GB で作れるが、ARM 版の WebP 変換は約 3.5 倍遅い。
# メモリの多い有料ランナーに移したら、この指定を外して最適化したまま作る。
WEBP_ARM_GCFLAGS="-gcflags=github.com/gen2brain/webp=-N"

# $1 = GOOS, $2 = GOARCH, $3 = 出力先, 残り = 追加の go build 引数
build() {
  local goos="$1" goarch="$2" out="$3"
  shift 3
  if [ "$goarch" = arm64 ]; then
    set -- "$WEBP_ARM_GCFLAGS" "$@"
  fi
  echo "==> build $goos/$goarch -> $(basename "$(dirname "$out")")"
  (cd "$ROOT" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -tags nodynamic "$@" -o "$out" .)
}

# Windows のアイコンのリソースを、ビルドの間だけ main パッケージ（ルート）に置く。$1 = GOARCH。
place_windows_icon() {
  local src="$ICONS/rsrc_windows_$1.syso"
  if [ ! -f "$src" ]; then
    echo "release.sh: warning: $src not found; building sorus.exe without the app icon" >&2
    return 0
  fi
  ICON_SYSO="$ROOT/zz_release_icon_windows_$1.syso"
  cp "$src" "$ICON_SYSO"
}

remove_windows_icon() {
  if [ -n "$ICON_SYSO" ]; then
    rm -f "$ICON_SYSO"
    ICON_SYSO=""
  fi
}

# デスクトップ版のアーカイブに入れるアイコン（$1 = os、$2 = 置き場所）。入れたファイル名を出力する。
stage_icon() {
  local src="" name=""
  case "$1" in
    darwin) src="$ICONS/sorus.icns" name="sorus.icns" ;;
    linux) src="$ICONS/sorus-256.png" name="sorus.png" ;;
    *) return 0 ;;
  esac
  if [ ! -f "$src" ]; then
    echo "release.sh: warning: $src not found; the $1 archive has no app icon" >&2
    return 0
  fi
  cp "$src" "$2/$name"
  echo "$name"
}

rm -rf "$OUT"
mkdir -p "$OUT"

for target in $DESKTOP_TARGETS; do
  goos="${target%/*}"
  goarch="${target#*/}"
  stage="$WORK/desktop-$goos-$goarch"
  mkdir -p "$stage"
  ldflags="-s -w -X main.buildMode=desktop $VERSION_FLAG"
  bin="sorus"
  if [ "$goos" = windows ]; then
    ldflags="$ldflags -H windowsgui"
    bin="sorus.exe"
  fi
  if [ "$goos" = windows ]; then
    place_windows_icon "$goarch"
  fi
  build "$goos" "$goarch" "$stage/$bin" -ldflags "$ldflags"
  remove_windows_icon
  icon="$(stage_icon "$goos" "$stage")"
  if [ "$goos" = windows ]; then
    # メモ帳などで文字化けしないよう、Windows 向けは BOM 付き UTF-8 にする。
    { printf '\357\273\277'; desktop_readme "$goos"; } >"$stage/README.txt"
  else
    desktop_readme "$goos" >"$stage/README.txt"
  fi
  name="sorus-$VERSION-$goos-$goarch"
  if [ "$goos" = linux ]; then
    tar -C "$stage" -czf "$OUT/$name.tar.gz" "$bin" README.txt ${icon:+"$icon"}
  else
    "$WORK/zipper-bin" "$OUT/$name.zip" "$stage/$bin" "$stage/README.txt" ${icon:+"$stage/$icon"}
  fi
done

for target in $SERVER_TARGETS; do
  goos="${target%/*}"
  goarch="${target#*/}"
  stage="$WORK/server-$goos-$goarch"
  mkdir -p "$stage"
  build "$goos" "$goarch" "$stage/sorus" -ldflags "-s -w $VERSION_FLAG"
  server_readme >"$stage/README.txt"
  tar -C "$stage" -czf "$OUT/sorus-server-$VERSION-$goos-$goarch.tar.gz" sorus README.txt
done

cp "$ROOT/install/install.sh" "$ROOT/install/install.ps1" "$ROOT/deploy/install-server.sh" "$OUT/"

# RELEASE_TARGETS で zip が1つも無いときに *.zip がそのまま残らないよう nullglob にする。
shopt -s nullglob
(cd "$OUT" && sha256 -- *.zip *.tar.gz install.sh install.ps1 install-server.sh >"$WORK/SHA256SUMS" && mv "$WORK/SHA256SUMS" SHA256SUMS)

echo "==> done: $OUT"
ls -l "$OUT"
