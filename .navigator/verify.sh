#!/usr/bin/env bash
# エージェントの「完了」を機械的に判定する検証スクリプト。
# exit 0 = 完了してよい。Stop hook がこれを実行し、失敗中はターンを終わらせない。
# 速いチェックから順に並べる。目安5分以内。遅いE2Eはここに入れない。
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

step() { printf '\n==> %s\n' "$*"; }

if ! command -v go >/dev/null 2>&1; then
  echo "verify.sh: go command not found. Go 1.25+ is required." >&2
  exit 1
fi

if [ ! -f go.mod ]; then
  echo "verify.sh: go.mod not found. Run go mod init to set up project." >&2
  exit 1
fi

step "go vet"
go vet ./...

step "go test"
go test ./...

step "go build"
go build -v -o /dev/null .
