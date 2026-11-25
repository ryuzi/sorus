#!/usr/bin/env bash
# sorus の公開サーバー（公開専用モード）を、まっさらな Ubuntu / Debian のサーバー（VPS）に1回で導入する。
#
# 使い方（SSH でサーバーに入り、次の1行を貼り付けて Enter）:
#   curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install-server.sh | sudo bash -s -- --domain media.example.jp --email you@example.jp
#
# VPS 業者の「スタートアップスクリプト」（cloud-init の user-data）欄に貼る場合は、次の2行を貼る（root で実行され、
# 途中で何も聞かない）。管理者用の招待コードは /root/sorus-admin-invite.txt にも保存する。
#   #!/bin/bash
#   curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install-server.sh | bash -s -- --domain media.example.jp --email you@example.jp
#
# サーバーから GitHub に接続できないときは、手元の PC でリリースの install-server.sh・
# sorus-server-<版>-linux-<arch>.tar.gz・SHA256SUMS をダウンロードして scp でサーバーに送り、
# --archive を付けて実行する:
#   sudo bash install-server.sh --archive sorus-server-v1.2.3-linux-amd64.tar.gz --domain media.example.jp --email you@example.jp
#
# 行うこと:
#   1. OS（Ubuntu / Debian）と、ドメインがこのサーバーを向いているか（DNS）を確かめる。
#   2. リリースから CPU に合うサーバー版をダウンロードし（--archive のときはそのファイルを使い）、
#      SHA256SUMS で確かめて /usr/local/bin/sorus に置く。
#   3. sorus ユーザー、/etc/sorus/sorus.env（公開専用モード・HTTPS の設定）、systemd のユニットを用意する。
#   4. ufw があれば、SSH のポートを先に許可してから 80・443 を開ける（SSH を閉め出さない）。
#   5. sorus を起動して応答を確かめ、管理者用の招待コードを発行して表示する。
#   もう一度実行すると、実行ファイルを更新して再起動する（招待コードは作り直さない。--new-admin-invite で作り直す）。
#
# オプション:
#   --domain D           公開サーバーのドメイン（必須。例: media.example.jp）
#   --email E            HTTPS の証明書（Let's Encrypt）の連絡先メールアドレス（必須）
#   --archive FILE       ダウンロードせず、サーバーに置いた sorus-server-<版>-linux-<arch>.tar.gz を使う
#                        （同じフォルダの SHA256SUMS、または --sha256sums のファイルで検証する）
#   --sha256sums FILE    --archive と一緒に使い、SHA256SUMS の場所を指定する
#   --skip-dns-check     ドメインがこのサーバーを向いているかの確認を飛ばす
#   --new-admin-invite   2回目以降の実行でも、管理者用の招待コードを新しく発行する
#   --admin-name NAME    管理者用の招待の名前（既定: サイト管理者）
#   -h, --help           この説明を表示する
#
# 環境変数（主にテスト・上級者向け）:
#   SORUS_RELEASE_BASE    ダウンロード元の URL（既定: https://github.com/ryuzi/sorus/releases/latest/download/）
#   SORUS_VERSION         版を固定する（例: v1.2.3）。SORUS_RELEASE_BASE が無いときだけ使う。
#   SORUS_PUBLIC_IP       このサーバーの公開 IP アドレス（指定すると外部のサービスに問い合わせない）
#   SORUS_INSTALL_SERVER_NO_MAIN=1  関数を定義するだけで何もしない（deploy/install_server_test.go が使う）
set -euo pipefail
shopt -s inherit_errexit 2>/dev/null || true

RELEASES_URL="https://github.com/ryuzi/sorus/releases"
BIN=/usr/local/bin/sorus
CONF_DIR=/etc/sorus
ENV_FILE=$CONF_DIR/sorus.env
STATE_DIR=/var/lib/sorus
UNIT_DIR=/etc/systemd/system
INVITE_MARKER=$CONF_DIR/admin-invite-issued
INVITE_SAVE=/root/sorus-admin-invite.txt
HEALTH_URL=http://127.0.0.1:8080/healthz

domain=""
email=""
skip_dns=0
new_invite=0
admin_name="サイト管理者"
archive_file=""
sha256sums_file=""
arch=""
https_ok=0
tmp=""

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
fail() {
  printf '\nエラー: %s\n' "$*" >&2
  exit 1
}
cleanup() {
  if [ -n "$tmp" ] && [ -d "$tmp" ]; then rm -rf "$tmp"; fi
}

usage() {
  sed -n '2,/^set -euo pipefail$/{/^set -euo pipefail$/d;s/^# \{0,1\}//;p}' "${BASH_SOURCE[0]:-$0}" 2>/dev/null ||
    say "使い方: install-server.sh --domain <ドメイン> --email <メールアドレス> [--archive <ファイル>] [--sha256sums <ファイル>] [--skip-dns-check] [--new-admin-invite]"
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --domain) [ $# -ge 2 ] || fail "--domain の後にドメインを書いてください。"; domain=$2; shift 2 ;;
      --domain=*) domain=${1#*=}; shift ;;
      --email) [ $# -ge 2 ] || fail "--email の後にメールアドレスを書いてください。"; email=$2; shift 2 ;;
      --email=*) email=${1#*=}; shift ;;
      --archive) [ $# -ge 2 ] && [ -n "$2" ] || fail "--archive の後にファイルの場所を書いてください。"; archive_file=$2; shift 2 ;;
      --archive=*) archive_file=${1#*=}; [ -n "$archive_file" ] || fail "--archive の後にファイルの場所を書いてください。"; shift ;;
      --sha256sums) [ $# -ge 2 ] && [ -n "$2" ] || fail "--sha256sums の後に SHA256SUMS の場所を書いてください。"; sha256sums_file=$2; shift 2 ;;
      --sha256sums=*) sha256sums_file=${1#*=}; [ -n "$sha256sums_file" ] || fail "--sha256sums の後に SHA256SUMS の場所を書いてください。"; shift ;;
      --admin-name) [ $# -ge 2 ] || fail "--admin-name の後に名前を書いてください。"; admin_name=$2; shift 2 ;;
      --admin-name=*) admin_name=${1#*=}; shift ;;
      --skip-dns-check) skip_dns=1; shift ;;
      --new-admin-invite) new_invite=1; shift ;;
      -h | --help) usage; exit 0 ;;
      *) fail "知らないオプションです: $1（--help で使い方を表示します）" ;;
    esac
  done
  domain=$(printf '%s' "$domain" | tr '[:upper:]' '[:lower:]')
  domain=${domain%.}
  [ -n "$domain" ] || fail "--domain でドメインを指定してください（例: --domain media.example.jp）。"
  [ -n "$email" ] || fail "--email で連絡先のメールアドレスを指定してください（HTTPS の証明書の期限切れなどの連絡が届きます）。"
  if ! printf '%s' "$domain" | grep -Eq '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$'; then
    fail "ドメインの形が正しくありません: $domain（https:// や / を付けずに、media.example.jp の形で書いてください）"
  fi
  if ! printf '%s' "$email" | grep -Eq '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'; then
    fail "メールアドレスの形が正しくありません: $email"
  fi
  if ! printf '%s' "$admin_name" | grep -Eq '^[^[:cntrl:]]{1,100}$'; then
    fail "--admin-name は改行を含まない100文字以内で指定してください。"
  fi
  if [ -n "$sha256sums_file" ] && [ -z "$archive_file" ]; then
    fail "--sha256sums は --archive と一緒に指定してください。"
  fi
}

check_system() {
  [ "$(id -u)" -eq 0 ] || fail "root で実行してください（1行の最後の部分を | sudo bash -s -- ... にします）。"
  [ -r /etc/os-release ] || fail "この OS には対応していません。Ubuntu または Debian のサーバーで実行してください。"
  # shellcheck disable=SC1091
  . /etc/os-release
  case " ${ID:-} ${ID_LIKE:-} " in
    *" ubuntu "* | *" debian "*) ;;
    *) fail "この OS（${PRETTY_NAME:-不明}）には対応していません。Ubuntu または Debian のサーバーで実行してください。" ;;
  esac
  command -v systemctl >/dev/null 2>&1 || fail "systemd が見つかりません。systemd で動く Ubuntu / Debian のサーバーで実行してください。"
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fail "この CPU（$(uname -m)）には対応していません。" ;;
  esac
  local missing=""
  for cmd in curl tar sha256sum getent runuser useradd; do
    command -v "$cmd" >/dev/null 2>&1 || missing="$missing $cmd"
  done
  if [ -n "$missing" ]; then
    step "必要な道具を入れています（${missing# }）"
    export DEBIAN_FRONTEND=noninteractive
    if ! { apt-get update -qq && apt-get install -y -qq curl ca-certificates tar coreutils libc-bin util-linux passwd >/dev/null; }; then
      fail "必要な道具（${missing# }）を入れられませんでした。インターネットにつながっているか確かめてください。"
    fi
  fi
}

# external_ips は外から見たこのサーバーの IP アドレスを書き出す（SORUS_PUBLIC_IP があればそれだけ）。
# クラウドの中にはサーバー自身が公開 IP を持たないもの（NAT）があるため、外部のサービスに問い合わせる。
external_ips() {
  if [ -n "${SORUS_PUBLIC_IP:-}" ]; then
    printf '%s\n' "$SORUS_PUBLIC_IP"
    return 0
  fi
  local url
  for url in https://api.ipify.org https://api64.ipify.org; do
    if curl -fsS --max-time 5 "$url" 2>/dev/null; then
      echo
    fi
  done
}

# interface_ips はこのサーバーのネットワークに付いている（グローバルな）IP アドレスを書き出す。
interface_ips() {
  if command -v ip >/dev/null 2>&1; then
    ip -o addr show scope global 2>/dev/null | awk '{sub(/\/.*/, "", $4); print $4}'
  fi
}

check_dns() {
  step "ドメイン $domain がこのサーバーを向いているか確かめています"
  local resolved external mine suggest ip
  resolved=$(getent ahosts "$domain" 2>/dev/null | awk '{print $1}' | sort -u || true)
  external=$(external_ips | grep -v '^$' | sort -u || true)
  mine=$(printf '%s\n%s\n' "$external" "$(interface_ips)" | grep -v '^$' | sort -u || true)
  # A レコードに入れる値として、外から見た IPv4 を優先して案内する。
  suggest=$(printf '%s\n' "$external" | grep -E '^[0-9]+\.' | head -n 1 || true)
  [ -n "$suggest" ] || suggest=$(interface_ips | grep -E '^[0-9]+\.' | head -n 1 || true)
  for ip in $resolved; do
    if printf '%s\n' "$mine" | grep -qxF "$ip"; then
      say "OK: $domain は このサーバー（$ip）を向いています。"
      return 0
    fi
  done
  if [ "$skip_dns" -eq 1 ]; then
    say "注意: $domain はまだこのサーバーを向いていませんが、--skip-dns-check が指定されたため続けます。"
    say "      向くまでは HTTPS の証明書を取れず、サイトを開けません。"
    return 0
  fi
  {
    say ""
    if [ -z "$resolved" ]; then
      say "ドメイン $domain の向き先（DNS）がまだ設定されていません。"
    else
      say "ドメイン $domain は別のサーバー（$(printf '%s' "$resolved" | paste -sd ' ' -)）を向いています。"
    fi
    say ""
    say "直し方:"
    say "  1. ドメインを買った会社（お名前.com・ムームードメイン・Cloudflare など）の管理画面を開きます。"
    say "  2. $domain の DNS の設定で、種類「A」のレコードを作り（または直し）、値に"
    say "     このサーバーの IP アドレス ${suggest:-（VPS の管理画面に書かれている IP アドレス）} を入れます。"
    say "  3. 数分から数時間で反映されます。反映されたら、同じ1行をもう一度実行してください。"
    say ""
    say "DNS の設定が正しいのにこの確認が通らない場合だけ、--skip-dns-check を付けて実行してください。"
  } >&2
  exit 1
}

verify_local_archive() {
  step "指定されたアーカイブ（$archive_file）を確かめています"
  [ -f "$archive_file" ] || fail "指定されたファイルが見つかりません: $archive_file"
  local sums_path name expected actual
  if [ -n "$sha256sums_file" ]; then
    sums_path=$sha256sums_file
  else
    sums_path="$(dirname "$archive_file")/SHA256SUMS"
  fi
  [ -f "$sums_path" ] || fail "ファイルの一覧（$sums_path）が見つかりません。アーカイブと同じフォルダに SHA256SUMS を置くか、--sha256sums で場所を指定してください。"
  name=$(basename "$archive_file")
  case "$name" in
    sorus-server-*-linux-"$arch".tar.gz) ;;
    sorus-server-*-linux-*.tar.gz)
      fail "$name はこのサーバーの CPU（$arch）向けではありません。sorus-server-<版>-linux-$arch.tar.gz を指定してください。"
      ;;
  esac
  expected=$(awk -v want="$name" '
    {
      n = $2
      sub(/^\*/, "", n)
      if (n == want) { print tolower($1); exit }
    }' "$sums_path")
  [ -n "$expected" ] || fail "SHA256SUMS に $name が載っていません。正しい SHA256SUMS か確かめてください。"
  case "$name" in
    sorus-server-*-linux-"$arch".tar.gz) ;;
    *)
      fail "$name はこのサーバーの CPU（$arch）向けのサーバー版（sorus-server-<版>-linux-$arch.tar.gz）ではありません。"
      ;;
  esac
  actual=$(sha256sum "$archive_file" | awk '{print tolower($1)}')
  if [ "$actual" != "$expected" ]; then
    fail "指定されたファイルが壊れているか、書き換えられている可能性があるため中止しました。いま動いている sorus はそのままです。"
  fi
  tmp=$(mktemp -d)
  mkdir "$tmp/x"
  tar -xzf "$archive_file" -C "$tmp/x"
  [ -f "$tmp/x/sorus" ] || fail "指定されたファイルの中に sorus が見つかりません。"
  say "OK: ファイルが正しいことを確かめました（SHA256）。"
}

download_release() {
  if [ -n "$archive_file" ]; then
    verify_local_archive
    return 0
  fi
  step "sorus（サーバー版）をダウンロードしています"
  local base
  if [ -n "${SORUS_RELEASE_BASE:-}" ]; then
    base=$SORUS_RELEASE_BASE
  elif [ -n "${SORUS_VERSION:-}" ]; then
    base="$RELEASES_URL/download/$SORUS_VERSION/"
  else
    base="$RELEASES_URL/latest/download/"
  fi
  case "$base" in */) ;; *) base="$base/" ;; esac

  tmp=$(mktemp -d)
  curl -fsSL --retry 2 -o "$tmp/SHA256SUMS" "${base}SHA256SUMS" ||
    fail "ファイルの一覧（SHA256SUMS）をダウンロードできませんでした。インターネットにつながっているか確かめてください。"
  local found expected archive actual
  found=$(awk -v suffix="-linux-$arch.tar.gz" '
    {
      name = $2
      sub(/^\*/, "", name)
      n = length(name); m = length(suffix)
      if (substr(name, 1, 13) == "sorus-server-" && n > m && substr(name, n - m + 1) == suffix) { print $1, name; exit }
    }' "$tmp/SHA256SUMS")
  [ -n "$found" ] || fail "このサーバーの CPU（$arch）向けのサーバー版がリリースにありません。"
  expected=${found%% *}
  archive=${found#* }
  say "$archive をダウンロードしています..."
  curl -fsSL --retry 2 -o "$tmp/$archive" "$base$archive" ||
    fail "$archive をダウンロードできませんでした。インターネットにつながっているか確かめてください。"
  actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
  if [ "$actual" != "$expected" ]; then
    fail "ダウンロードしたファイルが壊れているか、書き換えられている可能性があるため中止しました。いま動いている sorus はそのままです。"
  fi
  mkdir "$tmp/x"
  tar -xzf "$tmp/$archive" -C "$tmp/x"
  [ -f "$tmp/x/sorus" ] || fail "ダウンロードしたファイルの中に sorus が見つかりません。"
  say "OK: ファイルが正しいことを確かめました（SHA256）。"
}

install_binary() {
  step "sorus を $BIN に置いています"
  install -m 0755 "$tmp/x/sorus" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
}

ensure_user() {
  if ! id sorus >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$STATE_DIR" --shell /usr/sbin/nologin sorus
  fi
  install -d -m 0700 -o sorus -g sorus "$STATE_DIR"
}

# write_env_file は /etc/sorus/sorus.env の、このスクリプトが決める項目を書き換える。ほかの行（手で足した設定）は残す。
write_env_file() {
  step "設定ファイル $ENV_FILE を書いています"
  install -d -m 0750 -o root -g root "$CONF_DIR"
  local keep=""
  if [ -f "$ENV_FILE" ]; then
    # このスクリプトが書く項目と見出しの行（# --- で始まる）を除いた、手で足した行だけを残す。
    keep=$(grep -Ev '^(PUBLIC_ONLY|AUTOCERT_DOMAIN|AUTOCERT_EMAIL|BASE_URL|SECURE_COOKIE)=|^# --- ' "$ENV_FILE" || true)
  fi
  local next="$ENV_FILE.new"
  (
    umask 077
    {
      say "# --- sorus install-server.sh が書いた設定（もう一度実行すると書き直す） ---"
      say "PUBLIC_ONLY=true"
      say "AUTOCERT_DOMAIN=$domain"
      say "AUTOCERT_EMAIL=$email"
      say "BASE_URL=https://$domain"
      say "SECURE_COOKIE=true"
      if [ -n "$keep" ]; then
        say "# --- ここから下は手で足した設定（そのまま残す） ---"
        printf '%s\n' "$keep"
      fi
    } >"$next"
  )
  chown root:root "$next"
  chmod 0600 "$next"
  mv -f "$next" "$ENV_FILE"
}

# install_unit は標準入力の内容を systemd のユニット $1 として置く。内容は deploy/systemd/ のファイルと同じ
# （deploy/deploy_test.go が確かめる）。
install_unit() {
  local name=$1
  cat >"$UNIT_DIR/$name.new"
  chmod 0644 "$UNIT_DIR/$name.new"
  mv -f "$UNIT_DIR/$name.new" "$UNIT_DIR/$name"
}

install_units() {
  step "systemd のユニットを置いています"
  install_unit sorus.service <<'__SORUS_UNIT__'
# sorus の systemd ユニット。
#
# 導入例:
#   sudo useradd --system --home-dir /var/lib/sorus --shell /usr/sbin/nologin sorus
#   sudo install -m 0755 bin/sorus /usr/local/bin/sorus
#   sudo install -m 0644 deploy/systemd/sorus.service /etc/systemd/system/sorus.service
#   sudo install -d -m 0750 /etc/sorus && sudo editor /etc/sorus/sorus.env   # ADMIN_PASSWORD 等
#   sudo systemctl daemon-reload && sudo systemctl enable --now sorus
# バックアップ（稼働中でも可。cron や systemd timer から呼べる）:
#   sudo -u sorus env DB_PATH=/var/lib/sorus/database.db BACKUP_DIR=/var/lib/sorus/backups \
#     /usr/local/bin/sorus backup

[Unit]
Description=sorus
Documentation=file:///usr/local/share/doc/sorus/README.md
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=/usr/local/bin/sorus
Restart=on-failure
RestartSec=5s
# main.go は SIGTERM で 10 秒のグレースフルシャットダウンを行う。
KillSignal=SIGTERM
TimeoutStopSec=20s

# 永続データは /var/lib/sorus（StateDirectory）に置く。相対パスの既定値もここに解決される。
StateDirectory=sorus
StateDirectoryMode=0700
WorkingDirectory=/var/lib/sorus
Environment=DB_PATH=/var/lib/sorus/database.db
Environment=UPLOAD_DIR=/var/lib/sorus/uploads
Environment=SEARCH_INDEX_DIR=/var/lib/sorus/search_index
Environment=ATTACHMENT_DIR=/var/lib/sorus/attachments
Environment=AUTOCERT_CACHE_DIR=/var/lib/sorus/certs
Environment=BACKUP_DIR=/var/lib/sorus/backups
Environment=PORT=8080
# 秘密情報やサイト設定（ADMIN_PASSWORD, AUTOCERT_DOMAIN, AUTOCERT_EMAIL, SITE_NAME, BASE_URL,
# GEMINI_API_KEY など）は root のみ読めるファイルに置く。先頭の "-" はファイルが無くても起動する指定。
EnvironmentFile=-/etc/sorus/sorus.env

# --- 権限分離 ---
# 専用のシステムユーザーで実行し、80/443 の bind に必要な権限だけを与える。
User=sorus
Group=sorus
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
UMask=0077

# --- サンドボックス ---
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectHostname=yes
ProtectClock=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RemoveIPC=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
# 許可外のシステムコールはプロセスを kill せず EPERM で失敗させる。
SystemCallErrorNumber=EPERM

# --- リソース制限 ---
LimitNOFILE=65536
MemoryMax=1G
TasksMax=512

[Install]
WantedBy=multi-user.target
__SORUS_UNIT__
  install_unit sorus-backup.service <<'__SORUS_UNIT__'
[Unit]
Description=Online SQLite backup for sorus
After=network-online.target

[Service]
Type=oneshot
User=sorus
Group=sorus
StateDirectory=sorus
StateDirectoryMode=0700
WorkingDirectory=/var/lib/sorus
Environment=DB_PATH=/var/lib/sorus/database.db
Environment=BACKUP_DIR=/var/lib/sorus/backups
EnvironmentFile=-/etc/sorus/sorus.env
ExecStart=/usr/local/bin/sorus backup
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictSUIDSGID=yes
LockPersonality=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
__SORUS_UNIT__
  install_unit sorus-backup.timer <<'__SORUS_UNIT__'
[Unit]
Description=Daily 03:00 JST CMS database backup

[Timer]
OnCalendar=*-*-* 03:00:00 Asia/Tokyo
Persistent=true
RandomizedDelaySec=5m
Unit=sorus-backup.service

[Install]
WantedBy=timers.target
__SORUS_UNIT__
  systemctl daemon-reload
}

# ssh_ports は sshd が待ち受けているポートを1行に1つ書き出す。調べられなければ 22。
ssh_ports() {
  local ports=""
  if command -v sshd >/dev/null 2>&1; then
    ports=$(sshd -T 2>/dev/null | awk '$1 == "port" {print $2}' | sort -un || true)
  fi
  if [ -z "$ports" ] && command -v ss >/dev/null 2>&1; then
    ports=$(ss -Hltnp 2>/dev/null | awk '/"sshd"/ {n = split($4, a, ":"); print a[n]}' | sort -un || true)
  fi
  printf '%s\n' "${ports:-22}"
}

configure_firewall() {
  if ! command -v ufw >/dev/null 2>&1; then
    say "ufw が入っていないため、ファイアウォールは変更しません（VPS の管理画面で 80 と 443 を開けてください）。"
    return 0
  fi
  step "ファイアウォール（ufw）で SSH・80・443 を許可しています"
  local port
  # 有効にする前に SSH を許可し、いまの接続を閉め出さない。
  for port in $(ssh_ports); do
    ufw allow "$port/tcp" >/dev/null
    say "OK: SSH（$port）を許可しました。"
  done
  ufw allow 80/tcp >/dev/null
  ufw allow 443/tcp >/dev/null
  if ! ufw status 2>/dev/null | grep -q '^Status: active'; then
    ufw --force enable >/dev/null
  fi
  say "OK: 80（HTTP）と 443（HTTPS）を許可しました。"
}

start_services() {
  step "sorus を起動しています"
  systemctl enable sorus.service sorus-backup.timer >/dev/null 2>&1
  systemctl restart sorus.service
  systemctl start sorus-backup.timer
  local _
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 2 "$HEALTH_URL" >/dev/null 2>&1; then
      say "OK: sorus が動いています。"
      return 0
    fi
    sleep 1
  done
  journalctl -u sorus.service -n 30 --no-pager >&2 || true
  fail "sorus が起動しませんでした。上のログを確かめてください（journalctl -u sorus で全体を見られます）。"
}

# issue_admin_invite は sorus ユーザーとして sorus token create を実行し、管理者用の招待コードを書き出す。
# 公開サーバーと同じ署名鍵・同期 API のパス・DB を使い、発行は DB に記録される（画面から取り消せる）。
issue_admin_invite() {
  # 署名鍵・同期 API のパスを sorus.env で変えている場合は、公開サーバーと同じ値を使う。
  local key_file sync_path out
  key_file=$(sed -n 's/^CAPABILITY_KEY_FILE=//p' "$ENV_FILE" | tail -n 1)
  sync_path=$(sed -n 's/^SYNC_API_PATH=//p' "$ENV_FILE" | tail -n 1)
  out=$(cd "$STATE_DIR" && runuser -u sorus -- env DB_PATH="$STATE_DIR/database.db" \
    CAPABILITY_KEY_FILE="${key_file:-$STATE_DIR/capability.key}" SYNC_API_PATH="$sync_path" \
    "$BIN" token create --role admin --sub "$admin_name" --ttl 365d --base-url "https://$domain" 2>&1 >/dev/null) ||
    fail "管理者用の招待コードを発行できませんでした: $out"
  printf '%s\n' "$out" | sed -n 's/^  Invite code: \(sorus-invite:[^ ]*\)$/\1/p' | head -n 1
}

check_https() {
  # 最初の HTTPS の接続で証明書を取るため、ここで1回つないでおく（取れなくても導入は終わっている）。
  if curl -fsS --max-time 60 -o /dev/null "https://$domain/healthz" 2>/dev/null; then
    https_ok=1
  fi
}

print_box() {
  local code=$1
  local line="━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  say ""
  say "$line"
  say "  sorus の公開サーバーの準備ができました"
  say ""
  say "  サイトの URL:  https://$domain"
  if [ "${https_ok:-0}" -ne 1 ]; then
    say "  （HTTPS の証明書を取っているところです。数分たってからブラウザで開いてください）"
  fi
  say ""
  if [ -n "$code" ]; then
    say "  管理者用の招待コード（次の行を全部コピーします）:"
    say ""
    say "$code"
    say ""
    say "  次にすること:"
    say "    1. 自分の PC で sorus を開きます。"
    say "    2. sorus の「設定」→「公開先」で「公開サーバーに接続する」を開き、招待コードを貼り付けてください。"
    say "       「接続を確かめて参加する」を押すと参加できます。"
    say "    3. 参加できたら、同じ画面の「招待する」で、記事を書く人の招待コードを作って送ります。"
    say ""
    say "  この招待コードは、公開サーバーの管理者になれる大切な鍵です。他の人に見せないでください。"
    say "  同じ招待コードを $INVITE_SAVE にも保存しました（root だけが読めます）。"
  else
    say "  導入済みのため、実行ファイルを更新して再起動しました。"
    say "  管理者用の招待コードを作り直すときは、--new-admin-invite を付けてもう一度実行してください。"
  fi
  say "$line"
}

main() {
  trap cleanup EXIT
  parse_args "$@"
  check_system
  check_dns
  download_release
  install_binary
  ensure_user
  write_env_file
  install_units
  configure_firewall
  start_services
  check_https

  local code=""
  if [ ! -f "$INVITE_MARKER" ] || [ "$new_invite" -eq 1 ]; then
    step "管理者用の招待コードを発行しています"
    code=$(issue_admin_invite)
    [ -n "$code" ] || fail "管理者用の招待コードを読み取れませんでした。"
    (
      umask 077
      printf '%s\n' "$code" >"$INVITE_SAVE"
    )
    date -u +%Y-%m-%dT%H:%M:%SZ >"$INVITE_MARKER"
  fi
  print_box "$code"
}

# スクリプトを最後まで受け取ってから実行する（ダウンロードが途中で切れたときに、途中までを実行しない）。
if [ -z "${SORUS_INSTALL_SERVER_NO_MAIN:-}" ]; then
  main "$@"
fi
