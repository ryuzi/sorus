# syntax=docker/dockerfile:1
# sorus の本番用イメージ（静的リンクした単一バイナリ + curl healthcheck）。
#
# ビルド（リポジトリのルートで実行）:
#   docker build -f deploy/Dockerfile -t sorus .
# 実行（HTTP :8080）:
#   docker run -d -p 8080:8080 -v sorus-data:/data -e ADMIN_PASSWORD=... sorus
# 実行（autocert 自動 HTTPS。80/443 を公開し、DNS をこのホストに向けておく）:
#   docker run -d -p 80:80 -p 443:443 -v sorus-data:/data \
#     -e AUTOCERT_DOMAIN=sorus.example.jp -e AUTOCERT_EMAIL=ops@example.jp sorus
# バックアップ（稼働中でも可）:
#   docker exec <container> /sorus backup /data/backups/manual.db

# go.mod が go 1.25 を要求し、公式イメージは GOTOOLCHAIN=local のため 1.25 以上を使う。
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# nodynamic: gen2brain/webp が purego で libwebp を dlopen しないようにする（WASM 版のみを使う）。
# このタグが無いと CGO_ENABLED=0 でも動的リンクの実行ファイルになり、scratch では起動できない。
RUN CGO_ENABLED=0 go build -tags nodynamic -trimpath -ldflags="-s -w" -o /out/sorus . \
 && mkdir -p /out/data /out/tmp \
 && chmod 1777 /out/tmp

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata curl \
 && mkdir -p /data /tmp \
 && chmod 1777 /tmp \
 && chown 65532:65532 /data
COPY --from=build --chown=65532:65532 /out/sorus /sorus

# 永続データ（DB・アップロード・検索インデックス・添付・証明書・バックアップ）はすべて /data に置く。
ENV DB_PATH=/data/database.db \
    UPLOAD_DIR=/data/uploads \
    SEARCH_INDEX_DIR=/data/search_index \
    ATTACHMENT_DIR=/data/attachments \
    AUTOCERT_CACHE_DIR=/data/certs \
    BACKUP_DIR=/data/backups \
    PORT=8080 \
    TZ=Asia/Tokyo
WORKDIR /data
VOLUME ["/data"]

# 非 root（nonroot: 65532）で実行する。Docker 20.10 以降はコンテナ内で 1024 未満のポートも
# 非 root で bind できる（net.ipv4.ip_unprivileged_port_start=0）。
USER 65532:65532
EXPOSE 80 443 8080
ENTRYPOINT ["/sorus"]
