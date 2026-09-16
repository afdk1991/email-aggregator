# 邮箱聚合平台 · 容器化单二进制镜像（完整功能版）
#
# 形态：Go 主干 email-aggregator-go + 内嵌前端 SPA（-tags webui），
#       单进程同端口提供 REST + WebSocket + 静态页面，保留 30s 定时演示推送与 AI 网关。
# 区别：与 EdgeOne Makers 版（Serverless、无 WebSocket、内存态）相比，
#       容器版可长驻运行，支持 WebSocket 实时推送；接真实中间件时再加 -tags integration。
#
# 构建上下文必须是仓库根目录（需要同时访问 email-aggregator-web 与 email-aggregator-go）：
#   docker build -f deploy/container/Dockerfile -t email-aggregator:latest .
# 运行：
#   docker run --rm -p 8080:8080 -e HTTP_PORT=8080 email-aggregator:latest

# ---------- 阶段 1：构建前端 ----------
FROM node:22-alpine AS web
WORKDIR /web
COPY email-aggregator-web/package.json email-aggregator-web/package-lock.json ./
RUN npm ci --silent
COPY email-aggregator-web/ ./
RUN npm run build

# ---------- 阶段 2：构建 Go 主干（内嵌前端） ----------
FROM golang:1.26-alpine AS be
WORKDIR /src
# Go 模块代理：默认走国内可达镜像（proxy.golang.org 在部分网络被拒）；
# 海外 CI/Render 可用 --build-arg GOPROXY=https://proxy.golang.org,direct 覆盖。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
# 先复制依赖清单，利用 Docker 层缓存
COPY email-aggregator-go/go.mod email-aggregator-go/go.sum ./
RUN go mod download
COPY email-aggregator-go/ ./
# 前端产物写入 webroot/dist，供 -tags webui 内嵌
COPY --from=web /web/dist ./webroot/dist
# CGO_ENABLED=0 产出静态二进制；-tags webui 内嵌 SPA（不加则为纯 API 服务）
RUN CGO_ENABLED=0 GOOS=linux go build -tags webui -trimpath -ldflags="-s -w" -o /out/emailagg ./cmd/server

# ---------- 阶段 3：运行镜像 ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 appuser
COPY --from=be /out/emailagg /usr/local/bin/emailagg
USER appuser
# 默认 8080；平台注入 PORT 时（Render / Cloud Run / Koyeb）以 PORT 为准
ENV HTTP_PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:${HTTP_PORT}/api/health || exit 1
# 用 sh 做一层端口映射：优先采用平台注入的 PORT，回退 HTTP_PORT/8080
CMD ["sh", "-c", "HTTP_PORT=${PORT:-${HTTP_PORT:-8080}} exec /usr/local/bin/emailagg"]
