#!/bin/sh
# sorus のインストール・更新スクリプト（macOS / Linux）。
#
# 使い方（ターミナルに貼り付けて Enter）:
#   curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh
#
# 行うこと:
#   1. OS と CPU に合うファイルを選んでダウンロードする。
#   2. 同じリリースの SHA256SUMS でハッシュを確かめ、一致しなければ中止する（入っている sorus はそのまま）。
#   3. 管理者権限なしで置く。macOS: ~/Applications/sorus.app をその場で組み立てる
#      （curl で取得したので quarantine 属性は付かない）。Linux: ~/.local/bin/sorus と .desktop。
#      アプリアイコン（scripts/release.sh がアーカイブに入れる sorus.icns・sorus.png）があれば設定する。
#      macOS: sorus.app/Contents/Resources/sorus.icns と Info.plist の CFBundleIconFile。
#      Linux: ~/.local/share/icons/hicolor/256x256/apps/sorus.png と .desktop の Icon=。
#      アイコンの入っていない古い版のアーカイブでは、前に入れたアイコンがあればそれを使い、無ければ設定しない
#      （OS の標準のアイコンになるだけで、インストールは続ける）。
#   4. sorus を起動する。2回目以降の実行は「更新」になる（データは別の場所なので消えない）。
#
# 環境変数（主にテスト・上級者向け）:
#   SORUS_VERSION       版を固定する（例: v1.2.3）。省略時は最新版。
#   SORUS_RELEASE_BASE  ダウンロード元の URL（末尾の / は省略可）。
#   SORUS_NO_LAUNCH=1   インストール後に起動しない（空でなければ何でもよい）。
#   SORUS_FORCE_OS / SORUS_FORCE_ARCH  uname -s / uname -m の代わりに使う値（テスト用）。
set -eu

RELEASES_URL="https://github.com/ryuzi/sorus/releases"

say() { printf '%s\n' "$*"; }
fail() {
  printf 'エラー: %s\n' "$*" >&2
  exit 1
}

tmp=""
stage=""
cleanup() {
  if [ -n "$stage" ] && [ -e "$stage" ]; then rm -rf "$stage"; fi
  if [ -n "$tmp" ] && [ -e "$tmp" ]; then rm -rf "$tmp"; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

[ -n "${HOME:-}" ] || fail "HOME が設定されていないため、インストール先を決められません。"

# --- OS と CPU を調べる ---
real_os=$(uname -s)
os_name=${SORUS_FORCE_OS:-$real_os}
case "$os_name" in
  Darwin) os=darwin ext=zip ;;
  Linux) os=linux ext=tar.gz ;;
  *) fail "この OS（$os_name）には対応していません。Windows では PowerShell 用の1行を使ってください。" ;;
esac

machine=${SORUS_FORCE_ARCH:-$(uname -m)}
# Apple シリコンの Mac で Rosetta 経由のターミナルだと x86_64 と出るため、本来の CPU を確かめる。
if [ -z "${SORUS_FORCE_ARCH:-}" ] && [ "$real_os" = Darwin ] && [ "$machine" = x86_64 ]; then
  if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
    machine=arm64
  fi
fi
case "$machine" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "この CPU（$machine）には対応していません。" ;;
esac

# --- ダウンロード元 ---
if [ -n "${SORUS_RELEASE_BASE:-}" ]; then
  base=$SORUS_RELEASE_BASE
elif [ -n "${SORUS_VERSION:-}" ]; then
  base="$RELEASES_URL/download/$SORUS_VERSION/"
else
  base="$RELEASES_URL/latest/download/"
fi
case "$base" in
  */) ;;
  *) base="$base/" ;;
esac

download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 2 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1"
  else
    fail "curl も wget も見つからないため、ダウンロードできません。"
  fi
}

if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  fail "sha256sum も shasum も見つからないため、ファイルが正しいか確かめられません。"
fi

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t sorus-install)

say "sorus のインストールを始めます。"
say "ファイルの一覧を取得しています..."
download "${base}SHA256SUMS" "$tmp/SHA256SUMS" ||
  fail "ダウンロードできませんでした。インターネットにつながっているか確かめて、もう一度お試しください。"

# SHA256SUMS から、この OS と CPU 向けのデスクトップ版を探す（サーバー版は除く）。
suffix="-$os-$arch.$ext"
found=$(awk -v suffix="$suffix" -v want="${SORUS_VERSION:+sorus-$SORUS_VERSION$suffix}" '
  {
    name = $2
    sub(/^\*/, "", name)
    if (want != "") {
      if (name == want) { print $1, name; exit }
      next
    }
    if (substr(name, 1, 6) != "sorus-" || substr(name, 1, 13) == "sorus-server-") next
    n = length(name); m = length(suffix)
    if (n > m + 6 && substr(name, n - m + 1) == suffix) { print $1, name; exit }
  }' "$tmp/SHA256SUMS")
[ -n "$found" ] || fail "この OS と CPU（$os / $arch）向けのファイルがリリースにありません。"
expected=${found%% *}
archive=${found#* }
version=${archive#sorus-}
version=${version%"$suffix"}

say "sorus $version をダウンロードしています..."
download "$base$archive" "$tmp/$archive" ||
  fail "ダウンロードできませんでした。インターネットにつながっているか確かめて、もう一度お試しください。"

say "ファイルが正しいか確かめています..."
actual=$(sha256_of "$tmp/$archive")
if [ "$actual" != "$expected" ]; then
  fail "ダウンロードしたファイルが壊れているか、書き換えられている可能性があるため、インストールを中止しました。時間をおいてもう一度お試しください。すでに入っている sorus はそのまま使えます。"
fi

mkdir "$tmp/x"
if [ "$ext" = zip ]; then
  if command -v unzip >/dev/null 2>&1; then
    unzip -q "$tmp/$archive" -d "$tmp/x"
  elif command -v ditto >/dev/null 2>&1; then
    ditto -x -k "$tmp/$archive" "$tmp/x"
  else
    fail "unzip が見つからないため、ファイルを展開できません。"
  fi
else
  tar -xzf "$tmp/$archive" -C "$tmp/x"
fi
[ -f "$tmp/x/sorus" ] || fail "ダウンロードしたファイルの中に sorus が見つかりません。"
chmod 755 "$tmp/x/sorus"

busy_msg="sorus を置き換えられませんでした。sorus が動いている場合は、画面の「sorus を終了」で終了してから、もう一度この1行を実行してください。"

say "インストールしています..."
if [ "$os" = darwin ]; then
  apps="$HOME/Applications"
  app="$apps/sorus.app"
  mkdir -p "$apps"
  # 新しい .app を隣で組み立ててから入れ替える。途中で失敗しても元の sorus.app は残る。
  stage="$apps/.sorus.app.new.$$"
  rm -rf "$stage"
  mkdir -p "$stage/Contents/MacOS"
  cp "$tmp/x/sorus" "$stage/Contents/MacOS/sorus"
  chmod 755 "$stage/Contents/MacOS/sorus"
  # アイコン: ダウンロードしたアーカイブのもの、無ければ今の sorus.app のもの。どちらも無ければ付けない。
  icon_src=""
  if [ -f "$tmp/x/sorus.icns" ]; then
    icon_src="$tmp/x/sorus.icns"
  elif [ -f "$app/Contents/Resources/sorus.icns" ]; then
    icon_src="$app/Contents/Resources/sorus.icns"
  fi
  icon_plist=""
  if [ -n "$icon_src" ] && mkdir -p "$stage/Contents/Resources" && cp "$icon_src" "$stage/Contents/Resources/sorus.icns"; then
    icon_plist="	<key>CFBundleIconFile</key>
	<string>sorus.icns</string>"
  fi
  cat >"$stage/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>jp.sorus.desktop</string>
	<key>CFBundleName</key>
	<string>sorus</string>
	<key>CFBundleDisplayName</key>
	<string>sorus</string>
	<key>CFBundleExecutable</key>
	<string>sorus</string>
${icon_plist}
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleShortVersionString</key>
	<string>${version#v}</string>
	<key>CFBundleVersion</key>
	<string>${version#v}</string>
	<key>LSUIElement</key>
	<true/>
</dict>
</plist>
EOF
  # 中身を変えた .app として整合させるため、その場で署名し直す（ad-hoc。できなくても起動はできる）。
  if [ "$real_os" = Darwin ] && command -v codesign >/dev/null 2>&1; then
    codesign --force --sign - "$stage" >/dev/null 2>&1 || true
  fi
  old=""
  if [ -e "$app" ]; then
    old="$apps/.sorus.app.old.$$"
    mv "$app" "$old" || fail "$busy_msg"
  fi
  if ! mv "$stage" "$app"; then
    if [ -n "$old" ]; then mv "$old" "$app" || true; fi
    fail "$busy_msg"
  fi
  stage=""
  if [ -n "$old" ]; then rm -rf "$old"; fi
  say "インストールしました: $app"
else
  bindir="$HOME/.local/bin"
  appsdir="$HOME/.local/share/applications"
  mkdir -p "$bindir" "$appsdir"
  # 同じフォルダに置いてから名前を変えて入れ替える（途中で失敗しても元の sorus は残る）。
  stage="$bindir/.sorus.new.$$"
  cp "$tmp/x/sorus" "$stage"
  chmod 755 "$stage"
  mv -f "$stage" "$bindir/sorus" || fail "$busy_msg"
  # アイコン: アーカイブにあれば置き換え、無ければ前に置いたものを使う。どちらも無ければ Icon= を書かない。
  # Icon= は絶対パスにする（アイコンのキャッシュの更新を待たずに表示される）。
  iconsdir="$HOME/.local/share/icons/hicolor/256x256/apps"
  if [ -f "$tmp/x/sorus.png" ] && mkdir -p "$iconsdir"; then
    stage="$iconsdir/.sorus.png.new.$$"
    if cp "$tmp/x/sorus.png" "$stage"; then
      mv -f "$stage" "$iconsdir/sorus.png" || rm -f "$stage"
    else
      rm -f "$stage"
    fi
    stage=""
  fi
  stage="$appsdir/.sorus.desktop.new.$$"
  cat >"$stage" <<EOF
[Desktop Entry]
Type=Application
Name=sorus
Comment=記事を書いてサイトに公開するアプリ
Exec="$bindir/sorus"
Terminal=false
Categories=Office;
EOF
  if [ -f "$iconsdir/sorus.png" ]; then
    printf 'Icon=%s\n' "$iconsdir/sorus.png" >>"$stage"
  fi
  mv -f "$stage" "$appsdir/sorus.desktop"
  stage=""
  say "インストールしました: $bindir/sorus"
fi

if command -v pgrep >/dev/null 2>&1 && pgrep -x sorus >/dev/null 2>&1; then
  say "前の版の sorus が動いています。画面の「sorus を終了」で終了してから起動し直すと、新しい版になります。"
fi

if [ -n "${SORUS_NO_LAUNCH:-}" ]; then
  say "完了しました（起動は省略しました）。"
  exit 0
fi

say "sorus を起動しています。ブラウザで画面が開きます..."
if [ "$os" = darwin ]; then
  open "$app"
  say "完了しました。次からは「アプリケーション」フォルダ（ホームの中）の sorus から起動できます。"
else
  nohup "$bindir/sorus" >/dev/null 2>&1 &
  say "完了しました。次からはアプリの一覧の sorus から起動できます。"
fi
