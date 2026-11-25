#!/usr/bin/env bash
# 公開サーバー同期と CDN キャッシュのエンドツーエンド検証を、ビルドした実行ファイルと curl で行う。
# 一時ディレクトリで公開専用モード（PUBLIC_ONLY=true）の公開サーバーを起動し、自動生成された秘匿の同期パス
# （sync_path.txt）と sorus token create で発行したトークンで、提出・承認公開の流れとキャッシュヘッダーを確かめる。
# 固定の /api/sync が 404 で門前払いされることも確かめる。リポジトリの DB・鍵・アップロードには触れない。
# 続けて招待の流れ（管理者の招待コードで管理者のアプリが参加 →
# アプリの画面からライターを招待 → ライターのアプリが参加・提出 → 取り消すと拒否される）を、ローカルの sorus を
# 2つ（管理者・ライター）起動して確かめる。
# 使い方: scripts/e2e_decoupled_sync.sh
#   （ポートは E2E_PORT・E2E_ADMIN_APP_PORT・E2E_WRITER_APP_PORT で変更できる。既定 18080・18081・18082）
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

port="${E2E_PORT:-18080}"
base="http://127.0.0.1:${port}"
admin_app_port="${E2E_ADMIN_APP_PORT:-18081}"
writer_app_port="${E2E_WRITER_APP_PORT:-18082}"
work="$(mktemp -d)"
server_pid=""
app_pids=()
sync_path=""
failures=0

cleanup() {
  local pid
  for pid in "$server_pid" "${app_pids[@]}"; do
    [ -n "$pid" ] || continue
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$work"
}
trap cleanup EXIT

pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }

# expect_status は "<method> <path>" の応答ステータスが want かを確かめる。追加の curl 引数を渡せる。
expect_status() {
  local want="$1" method="$2" path="$3"
  shift 3
  local got
  got="$(curl -s -o /dev/null -w '%{http_code}' -X "$method" "$@" "${base}${path}")"
  if [ "$got" = "$want" ]; then pass "$method $path -> $got"; else fail "$method $path -> $got (want $want)"; fi
}

# sync は token で同期 API（既定は秘匿の同期パス）に JSON を送り、"<ステータス>\n<本文>" を出力する。
sync() {
  local token="$1" body="$2" path="${3:-$sync_path}"
  curl -s -w '\n%{http_code}' -X POST -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
    --data "$body" "${base}${path}"
}

# expect_sync は同期 API の応答ステータスが want かを確かめる。5番目の引数で送り先のパスを変えられる。
expect_sync() {
  local label="$1" want="$2" token="$3" body="$4" path="${5:-$sync_path}" out status
  out="$(sync "$token" "$body" "$path")"
  status="${out##*$'\n'}"
  if [ "$status" = "$want" ]; then pass "$label -> $status"; else fail "$label -> $status (want $want): ${out%$'\n'*}"; fi
  SYNC_BODY="${out%$'\n'*}"
}

# expect_header は HEAD（curl -sI）の応答ヘッダーに pattern（大文字小文字を区別しない拡張正規表現）があるかを確かめる。
expect_header() {
  local path="$1" pattern="$2"
  if curl -sI "${base}${path}" | grep -qiE "$pattern"; then pass "HEAD $path has /$pattern/"; else fail "HEAD $path lacks /$pattern/"; fi
}

echo "==> build"
go build -o "$work/sorus" .

# 公開サーバーの環境。ENV_FILE は存在しないファイルを指し、リポジトリの .env を読ませない。
export ENV_FILE="$work/none.env" PORT="$port" DB_PATH="$work/public.db" UPLOAD_DIR="$work/uploads" \
  SEARCH_INDEX_DIR="$work/search_index" ATTACHMENT_DIR="$work/attachments" CAPABILITY_KEY_FILE="$work/capability.key"
unset AUTOCERT_DOMAIN BASE_URL SYNC_API_PATH

echo "==> start public-only server on ${base}"
(cd "$work" && PUBLIC_ONLY=true exec ./sorus >"$work/server.log" 2>&1) &
server_pid=$!
for _ in $(seq 1 100); do
  curl -sf "${base}/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -sf "${base}/healthz" >/dev/null || { cat "$work/server.log"; echo "server did not start" >&2; exit 1; }

echo "==> 1. public-only mode"
expect_status 404 GET /admin
expect_status 404 GET /admin/login
expect_status 404 POST /admin/login --data 'username=admin&password=x'
expect_status 404 GET /admin/settings/remote
expect_status 404 GET /api/mcp
expect_status 200 GET /
expect_status 200 GET /feed.xml
if grep -q 'Initial admin user created' "$work/server.log"; then fail "public-only server created a login admin"; else pass "no initial admin in public-only mode"; fi

echo "==> 2. secret sync path"
sync_path="$(cat "$work/sync_path.txt" 2>/dev/null || true)"
if [[ "$sync_path" =~ ^/api/sync-[0-9a-f]{32}$ ]]; then pass "generated secret sync path $sync_path"; else fail "sync_path.txt: '$sync_path'"; fi
path_perm="$(stat -c '%a' "$work/sync_path.txt" 2>/dev/null || true)"
if [ "$path_perm" = "600" ]; then pass "sync path file permission 600"; else fail "sync path file permission $path_perm"; fi
if grep -qF "Sync API: enabled at POST ${sync_path} " "$work/server.log"; then pass "server log shows the secret sync path"; else fail "server log lacks the secret sync path"; fi

echo "==> 3. token issue and verification"
writer="$("$work/sorus" token create --role writer --sub yamada --ttl 1d 2>"$work/token.err")"
editor="$("$work/sorus" token create --role editor --sub sato --ttl 1d --base-url "$base" 2>"$work/token_url.err")"
if [ -n "$writer" ] && [ -n "$editor" ]; then pass "sorus token create issued writer and editor tokens"; else fail "token create"; fi
key_perm="$(stat -c '%a' "$work/capability.key")"
if [ "$key_perm" = "600" ]; then pass "signing key permission 600"; else fail "signing key permission $key_perm"; fi
if grep -qF "Sync path: ${sync_path}" "$work/token.err"; then pass "token create shows the sync path"; else fail "token create stderr: $(cat "$work/token.err")"; fi
if grep -qF "Sync URL:  ${base}${sync_path}" "$work/token_url.err"; then pass "token create --base-url shows the sync URL"; else fail "token create --base-url stderr: $(cat "$work/token_url.err")"; fi
# 固定の /api/sync は、正しいトークンでもトークンが無くても、存在しないパスと同じ 404 で門前払いする。
expect_sync "fixed /api/sync with a valid token" 404 "$editor" '{"action":"fetch_pending"}' /api/sync
expect_sync "fixed /api/sync without a token" 404 "" '{"action":"fetch_pending"}' /api/sync
expect_status 404 GET /api/sync
expect_sync "secret sync path" 200 "$editor" '{"action":"fetch_pending"}'
# 署名の末尾を書き換えたトークン・role を書き換えたトークンは拒否される。
last="${writer: -1}"
if [ "$last" = "A" ]; then repl="B"; else repl="A"; fi
expect_sync "tampered signature" 401 "${writer%?}${repl}" '{"action":"fetch_pending"}'
IFS=. read -r h _ s <<<"$writer"
forged_payload="$(printf '{"sub":"yamada","role":"admin","exp":9999999999,"iat":1700000000}' | base64 -w0 | tr '+/' '-_' | tr -d '=')"
expect_sync "forged role" 401 "${h}.${forged_payload}.${s}" '{"action":"fetch_pending"}'
expect_sync "missing token" 401 "" '{"action":"fetch_pending"}'

echo "==> 4. writer submit and editor publish"
article='{"action":"submit_draft","article":{"id":"e2e-article-1","title":"E2E Script Article","slug":"e2e-script","content_markdown":"submitted from the e2e script"}}'
expect_sync "writer submit_draft" 200 "$writer" "$article"
expect_status 404 GET /articles/e2e-script
if curl -s "${base}/" | grep -q 'E2E Script Article'; then fail "review article is listed on /"; else pass "review article is not listed on /"; fi
expect_sync "writer publish_article" 403 "$writer" '{"action":"publish_article","article_id":"e2e-article-1"}'
expect_sync "editor fetch_pending" 200 "$editor" '{"action":"fetch_pending"}'
case "$SYNC_BODY" in *'"id":"e2e-article-1"'*) pass "fetch_pending returns the submitted article" ;; *) fail "fetch_pending body: $SYNC_BODY" ;; esac
expect_sync "editor publish_article" 200 "$editor" '{"action":"publish_article","article_id":"e2e-article-1"}'
expect_status 200 GET /articles/e2e-script
if curl -s "${base}/" | grep -q 'E2E Script Article'; then pass "published article is listed on /"; else fail "published article is not listed on /"; fi

echo "==> 5. cache headers"
expect_header /static/icon.svg 'cache-control: .*immutable'
expect_header / 'cache-control: .*s-maxage=3600'
expect_header / '^etag: '
expect_header /articles/e2e-script 'cache-control: .*s-maxage=3600'
etag="$(curl -sI "${base}/" | tr -d '\r' | sed -n 's/^[Ee][Tt][Aa][Gg]: //p')"
expect_status 304 GET / -H "If-None-Match: ${etag}"

echo "==> 6. invite codes (spec 9-5)"
app_password="e2e-app-password"

# start_app は公開専用でないローカルの sorus（管理画面あり）を name の作業ディレクトリと app_port で起動する。
start_app() {
  local name="$1" app_port="$2" dir="$work/$1"
  mkdir -p "$dir"
  (cd "$dir" && ENV_FILE="$dir/none.env" PORT="$app_port" DB_PATH="$dir/app.db" UPLOAD_DIR="$dir/uploads" \
    SEARCH_INDEX_DIR="$dir/search_index" ATTACHMENT_DIR="$dir/attachments" CAPABILITY_KEY_FILE="$dir/capability.key" \
    ADMIN_PASSWORD="$app_password" exec "$work/sorus" >"$dir/app.log" 2>&1) &
  app_pids+=("$!")
  local app="http://127.0.0.1:${app_port}"
  for _ in $(seq 1 100); do
    curl -sf "${app}/healthz" >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -sf "${app}/healthz" >/dev/null || { cat "$dir/app.log"; echo "$name app did not start" >&2; exit 1; }
}

# login は app の管理画面に初期管理者でログインし、Cookie を jar に保存する。CSRF トークンを APP_CSRF に入れる。
login() {
  local app="$1" jar="$2" status
  curl -s -o /dev/null -c "$jar" -b "$jar" "${app}/admin/login"
  APP_CSRF="$(awk '$6 == "cms_csrf" {print $7}' "$jar")"
  status="$(curl -s -o /dev/null -w '%{http_code}' -c "$jar" -b "$jar" --data-urlencode "_csrf=${APP_CSRF}" \
    --data-urlencode username=admin --data-urlencode "password=${app_password}" "${app}/admin/login")"
  APP_CSRF="$(awk '$6 == "cms_csrf" {print $7}' "$jar")"
  if [ "$status" = "303" ] && [ -n "$APP_CSRF" ]; then pass "login to $app"; else fail "login to $app -> $status"; fi
}

# app_post は画面の fetch と同じく X-CSRF-Token 付きでフォームを POST し、"<本文>\n<ステータス>" を出力する。
app_post() {
  local app="$1" jar="$2" path="$3"
  shift 3
  curl -s -w '\n%{http_code}' -X POST -b "$jar" -c "$jar" -H "X-CSRF-Token: ${APP_CSRF}" -H 'Accept: application/json' "$@" "${app}${path}"
}

# join は app の「公開サーバー」の画面で招待コードを貼って送り、ステータスを出力する。
join() {
  local app="$1" jar="$2" code="$3"
  curl -s -o "$work/join.html" -w '%{http_code}' -b "$jar" -c "$jar" --data-urlencode "_csrf=${APP_CSRF}" \
    --data-urlencode "invite_code=${code}" "${app}/admin/settings/remote/join"
}

# invite_token は招待コードからトークンを取り出す（sorus-invite: + Base64URL の JSON）。
invite_token() {
  local body="${1#sorus-invite:}"
  while [ $(( ${#body} % 4 )) -ne 0 ]; do body="${body}="; done
  printf '%s' "$body" | tr '_-' '/+' | base64 -d | sed -n 's/.*"token":"\([^"]*\)".*/\1/p'
}

json_field() { sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p'; }

# 公開サーバーで、管理者用の招待コードを sorus token create で発行する（発行は公開サーバーの DB に記録される）。
"$work/sorus" token create --role admin --sub "e2e-admin" --ttl 1d --base-url "$base" >/dev/null 2>"$work/admin_token.err"
admin_invite="$(sed -n 's/^  Invite code: \(sorus-invite:[^ ]*\)$/\1/p' "$work/admin_token.err")"
if [[ "$admin_invite" == sorus-invite:* ]]; then pass "token create prints an invite code"; else fail "token create stderr: $(cat "$work/admin_token.err")"; fi
if grep -qF "Recorded in: ${DB_PATH}" "$work/admin_token.err"; then pass "token create records the token in the server database"; else fail "token create did not record: $(cat "$work/admin_token.err")"; fi

admin_app="http://127.0.0.1:${admin_app_port}"
writer_app="http://127.0.0.1:${writer_app_port}"
start_app admin "$admin_app_port"
start_app writer "$writer_app_port"
admin_jar="$work/admin.jar"
writer_jar="$work/writer.jar"

login "$admin_app" "$admin_jar"
admin_csrf="$APP_CSRF"
status="$(join "$admin_app" "$admin_jar" "sorus-invite:broken")"
if [ "$status" = "422" ] && grep -q '招待コードの形式が正しくありません' "$work/join.html"; then pass "broken invite code is refused"; else fail "broken invite -> $status"; fi
status="$(join "$admin_app" "$admin_jar" "$admin_invite")"
if [ "$status" = "303" ]; then pass "admin app joins with the admin invite code"; else fail "admin join -> $status: $(grep -o 'まだ参加していません[^<]*' "$work/join.html" | head -1)"; fi
page="$(curl -s -b "$admin_jar" "${admin_app}/admin/settings/remote")"
case "$page" in *'サイト管理者'*'data-remote-invites'*) pass "admin app shows the role and the invite form" ;; *) fail "admin app settings page lacks the invite form" ;; esac

APP_CSRF="$admin_csrf"
out="$(app_post "$admin_app" "$admin_jar" /admin/remote/invites --data-urlencode name=e2e-writer --data-urlencode role=writer --data-urlencode ttl_days=7)"
status="${out##*$'\n'}"
writer_invite="$(printf '%s' "${out%$'\n'*}" | json_field invite_code)"
writer_jti="$(printf '%s' "${out%$'\n'*}" | json_field jti)"
if [ "$status" = "200" ] && [[ "$writer_invite" == sorus-invite:* ]]; then pass "admin app issues a writer invite"; else fail "issue invite -> $status: ${out%$'\n'*}"; fi

login "$writer_app" "$writer_jar"
status="$(join "$writer_app" "$writer_jar" "$writer_invite")"
if [ "$status" = "303" ]; then pass "writer app joins with the writer invite code"; else fail "writer join -> $status"; fi
writer_token="$(invite_token "$writer_invite")"
expect_sync "invited writer submit_draft" 200 "$writer_token" '{"action":"submit_draft","article":{"id":"e2e-invite-1","title":"Invited Writer","content_markdown":"from an invite"}}'
expect_sync "invited writer issue_invite" 403 "$writer_token" '{"action":"issue_invite","role":"writer","sub":"x","ttl_days":1}'

APP_CSRF="$admin_csrf"
out="$(curl -s -w '\n%{http_code}' -b "$admin_jar" -H 'Accept: application/json' "${admin_app}/admin/remote/tokens")"
writer_item="$(printf '%s' "${out%$'\n'*}" | grep -o "\"jti\":\"${writer_jti}\"[^}]*" || true)"
case "$writer_item" in *'"status":"active"'*) pass "admin app lists the writer invite" ;; *) fail "list tokens -> ${out##*$'\n'}: ${out%$'\n'*}" ;; esac
out="$(app_post "$admin_app" "$admin_jar" "/admin/remote/tokens/${writer_jti}/revoke")"
if [ "${out##*$'\n'}" = "200" ]; then pass "admin app revokes the writer invite"; else fail "revoke -> ${out##*$'\n'}: ${out%$'\n'*}"; fi
expect_sync "revoked writer submit_draft" 401 "$writer_token" '{"action":"submit_draft","article":{"id":"e2e-invite-2","title":"After Revoke","content_markdown":"x"}}'
case "$SYNC_BODY" in *'取り消されています'*) pass "revoked token gets a clear message" ;; *) fail "revoked message: $SYNC_BODY" ;; esac
login "$writer_app" "$writer_jar"
status="$(join "$writer_app" "$writer_jar" "$writer_invite")"
if [ "$status" = "422" ] && grep -q '取り消されています' "$work/join.html"; then pass "revoked invite cannot join again"; else fail "rejoin with a revoked invite -> $status"; fi

# 管理者は、最後のサイト管理者の招待（自分の招待）を取り消せない。
APP_CSRF="$admin_csrf"
admin_jti="$(curl -s -b "$admin_jar" -H 'Accept: application/json' "${admin_app}/admin/remote/tokens" | grep -o '"jti":"[0-9a-f]*","sub":"e2e-admin"' | json_field jti)"
out="$(app_post "$admin_app" "$admin_jar" "/admin/remote/tokens/${admin_jti}/revoke" --data-urlencode confirm_self=1)"
case "$out" in *'最後のサイト管理者'*) pass "last admin invite cannot be revoked" ;; *) fail "self revoke: $out" ;; esac

echo
if [ "$failures" -ne 0 ]; then
  echo "E2E: ${failures} check(s) failed. Server log:"
  cat "$work/server.log"
  for app_log in "$work"/*/app.log; do
    [ -f "$app_log" ] && { echo "--- $app_log"; cat "$app_log"; }
  done
  exit 1
fi
echo "E2E: all checks passed"
