# syntax=docker/dockerfile:1

# ============================================================
# SSL 证书监控（sslmon）— 多阶段构建
#   1. node 构建前端 web/ → web/dist
#   2. golang 构建 sslmon 二进制（go:embed web/dist）
#   3. alpine 运行
# 所有配置数据由 MySQL monitor_config 表维护(首次启动播种内置默认, 面板可读写, 30s 热加载;
# 配置库连接由 MONITOR_DB_DSN 环境变量决定, 见 dbconfig.go)
# ============================================================

# ---- 阶段 1：构建前端 ----
FROM node:22-alpine AS ui
WORKDIR /build/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- 阶段 2：构建 Go 二进制 ----
FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
COPY *.go ./
# go:embed web/dist 需要 dist 存在
COPY --from=ui /build/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/sslmon .

# ---- 阶段 3：运行 ----
FROM alpine:3.21
# docker-cli: 面板部署控制用(docker run 宿主机 helper 容器, 见 deploy.go)
RUN apk add --no-cache ca-certificates docker-cli
WORKDIR /app
COPY --from=builder /out/sslmon /app/sslmon
EXPOSE 8003
# HTTP 服务模式: SSL_HTTP=1, 端口 SSL_HTTP_PORT(默认 8003)
# 所有配置存 MySQL monitor_config 表(表为空时自动播种内置默认, 请在面板设置页完善)
CMD ["/app/sslmon", "-http"]
