# =============================================================================
# email-aggregator-go 多入口 Dockerfile（Phase 2 / 蓝图 §11 服务拆分）
# -----------------------------------------------------------------------------
# 支持 4 个独立入口，通过 BUILD_TAGS + BIN_NAME 构建参数选择：
#   server_integration : -tags integration
#   sync_worker        : -tags integration,sync_worker
#   ingest_worker      : -tags integration,ingest_worker
#   search_service     : -tags integration,search_service
#
# 用法：
#   docker build -t email-aggregator:sync --build-arg BUILD_TAGS=integration,sync_worker --build-arg BIN_NAME=sync_worker .
#   docker run --env-file deploy/.env email-aggregator:sync
# =============================================================================

# ── Build stage ──
FROM golang:1.22-alpine AS builder

# build-arg 注入构建标签与二进制名
ARG BUILD_TAGS=integration
ARG BIN_NAME=server_integration

WORKDIR /app
# 依赖缓存层（go.mod/go.sum 不变时跳过 go mod download）
COPY go.mod go.sum ./
RUN go mod download

# 源码
COPY . .

# 编译指定入口（cgo-enabled=0 纯静态；alpine 无 libc 需求）
ENV CGO_ENABLED=0
RUN go build -tags ${BUILD_TAGS} -o /out/${BIN_NAME} ./cmd/

# ── Runtime stage ──
FROM alpine:3.19

# ca-certificates：HTTPS 调用 OpenSearch/MinIO 等自签/公网证书
RUN apk add --no-cache ca-certificates

ARG BIN_NAME=server_integration
WORKDIR /app
COPY --from=builder /out/${BIN_NAME} ./app

# 默认入口；docker-compose 可用 command 覆盖
ENTRYPOINT ["./app"]
