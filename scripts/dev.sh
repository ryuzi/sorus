#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
  echo "Go 1.25 以降をインストールしてください。" >&2
  exit 1
fi
go_version=$(go env GOVERSION | sed 's/^go//')
major=$(printf '%s' "$go_version" | cut -d. -f1)
minor=$(printf '%s' "$go_version" | cut -d. -f2 | sed 's/[^0-9].*$//')
if [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt 25 ]; }; then
  echo "Go 1.25 以降が必要です（現在: $go_version）。" >&2
  exit 1
fi

mkdir -p uploads attachments
# 手元で書き換える設定（dev.env）はリポジトリに入れない（.gitignore）。無ければ見本（dev.env.example）から作る。
env_file=deploy/environments/dev.env
if [ ! -f "$env_file" ]; then
  cp deploy/environments/dev.env.example "$env_file"
  echo "$env_file を deploy/environments/dev.env.example から作成しました。開発用の設定はこのファイルで変更してください。"
fi
ENV_FILE="$env_file" go run .
