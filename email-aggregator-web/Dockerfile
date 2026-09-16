# 邮箱聚合平台 · 前端（React+TS+Vite 构建产物）
# 多阶段构建：构建阶段 -> nginx 静态服务 + /api、/ws 反向代理
FROM node:22-alpine AS builder
LABEL org.opencontainers.image.version="MALLV0.0.0"
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --silent
COPY . .
RUN npm run build

FROM nginx:alpine
COPY --from=builder /app/dist /usr/share/nginx/html
# 用配置模板替换默认站点：监听 5173，并把 /api 与 /ws 反代到 Go 后端。
# nginx:alpine 入口脚本会以 envsubst 渲染模板（占位符 ${API_UPSTREAM} 来自环境变量）。
RUN rm -f /etc/nginx/conf.d/default.conf
COPY nginx.conf.template /etc/nginx/templates/default.conf.template
ENV API_UPSTREAM=http://host.docker.internal:8090
EXPOSE 5173
CMD ["nginx", "-g", "daemon off;"]
