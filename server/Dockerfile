# ---- build stage ----
# 多阶段构建：把二进制压到最小，适合 NAS 上长期运行。
#
# 基础镜像的 Go 版本必须 >= go.mod 里声明的版本，否则 go mod download 直接失败：
#   go: go.mod requires go >= 1.26.1 (running go 1.24.13; GOTOOLCHAIN=local)
# 这里刻意不带 GOTOOLCHAIN=auto 来"自动下载工具链"——容器构建应完全
# 由镜像版本决定，避免构建时再联网拉一套 Go，那样既慢又不可复现。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

# 先只拷依赖描述，最大化利用层缓存
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# TARGETOS/TARGETARCH 由 buildx 注入（amd64/arm64）
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

# 纯 Go 构建（modernc.org/sqlite 不需要 CGO），因此可以静态链接。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/venera-nas ./cmd/venera-nas

# ---- runtime stage ----
# alpine 而非 scratch：便于用户 exec 进去排查（sh + 基本工具）。
# 时区数据用于日志可读性。
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -u 1000 venera

COPY --from=builder /out/venera-nas /usr/local/bin/venera-nas

# /data   : SQLite 索引、源脚本、同步包
# /library: 漫画文件（建议挂载到大容量卷）
RUN mkdir -p /data /library && chown -R venera:venera /data /library

USER venera
WORKDIR /home/venera

ENV VENERA_LISTEN=:8080 \
    VENERA_DATA_DIR=/data \
    VENERA_LIBRARY_DIR=/library

EXPOSE 8080

# 注意：这里用 127.0.0.1 探测，而 VENERA_LISTEN 是 :8080（全部网卡）。
# 容器内自检走 loopback 不需要额外放开监听地址。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/venera-nas"]
