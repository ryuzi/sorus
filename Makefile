.PHONY: dev build test verify staging-up staging-down prod-up prod-down backup export release

dev:
	./scripts/dev.sh

build:
	go build -o bin/sorus .

test:
	go test -v ./...

verify:
	./.navigator/verify.sh

export:
	go run . export dist

# 配布用のリリース一式（デスクトップ版・サーバー版・SHA256SUMS）を dist-release/ に作る。
# 例: make release VERSION=v1.2.3
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z" >&2; exit 2; }
	./scripts/release.sh "$(VERSION)"

staging-up:
	docker compose -f deploy/docker-compose.staging.yml up -d --build

staging-down:
	docker compose -f deploy/docker-compose.staging.yml down

prod-up:
	docker compose -f deploy/docker-compose.yml up -d --build

prod-down:
	docker compose -f deploy/docker-compose.yml down

backup:
	go run . backup
