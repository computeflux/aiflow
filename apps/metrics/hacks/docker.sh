#!/usr/bin/env bash
set -euo pipefail

# docker.sh — 通过 docker compose 构建镜像并启动/管理 sslmon
#
# 用法: ./hacks/docker.sh {start|stop|restart|redeploy|rebuild|status|logs|down}
#   start     首次启动/后台启动(必要时自动构建镜像)
#   stop      停止容器(保留容器与数据卷)
#   restart   重启容器
#   redeploy  全量重新部署: git pull 拉最新代码 → 清理旧容器 → 重建镜像 → 启动 → 健康检查
#   rebuild   仅强制重建镜像并重启容器(不拉代码)
#   status    查看容器状态
#   logs      跟踪日志
#
# 可配置环境变量(见 compose.yaml, 也可写入 apps/metrics/.env):
#   SSL_HOSTS            证书监控主机列表(逗号分隔), 默认取数据库配置(首次播种自 config.yaml)
#   SSL_HTTP_PORT        面板端口(宿主机),      默认 19003
#   MONITOR_HOST         服务健康检查主机(替换 xxxx 占位符), 默认 host.docker.internal
#   MONITOR_DEPLOY_TOKEN 部署控制面板 token(面板"操作"列; 等价宿主机 root, 务必设置)
#
# 示例:
#   ./hacks/docker.sh start
#   SSL_HTTP_PORT=19003 ./hacks/docker.sh start
#   SSL_HOSTS="a.com,b.com" ./hacks/docker.sh start

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="$PROJECT_DIR/compose.yaml"

COMPOSE=(docker compose -f "$COMPOSE_FILE")

case "${1:-}" in
  start)
    "${COMPOSE[@]}" up -d --build
    echo ""
    echo "▶ sslmon 已启动"
    echo "  面板: http://<host>:${SSL_HTTP_PORT:-19003}"
    echo "  日志: ./hacks/docker.sh logs"
    ;;
  stop)
    "${COMPOSE[@]}" stop
    echo "▶ 已停止(容器保留)"
    ;;
  restart)
    "${COMPOSE[@]}" restart
    echo "▶ 已重启"
    ;;
  redeploy)
    # 全量重新部署: 拉最新代码(有 git 远端时) → 清理旧容器 → 重建镜像 → 启动 → 健康检查
    if [ -d "$PROJECT_DIR/.git" ] && [ -n "$(git -C "$PROJECT_DIR" remote)" ]; then
      echo "▶ 拉取最新代码 (git pull --ff-only) ..."
      git -C "$PROJECT_DIR" pull --ff-only 2>&1 \
        || echo "  ⚠ git pull 失败(本地有未提交改动?), 使用当前代码继续"
    fi
    # 清理早期 docker run 部署的旧容器(名称冲突会阻塞 compose 重建)
    docker rm -f sslmon >/dev/null 2>&1 || true
    "${COMPOSE[@]}" up -d --build
    # 等待健康检查(最多 60 秒)
    echo "▶ 等待健康检查 ..."
    ok=0
    for i in $(seq 1 30); do
      status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' sslmon 2>/dev/null || echo none)
      if [ "$status" = "healthy" ]; then ok=1; break; fi
      sleep 2
    done
    if [ "$ok" = "1" ]; then
      echo "▶ redeploy 完成, 面板: http://<host>:${SSL_HTTP_PORT:-19003}"
    else
      echo "⚠ 健康检查未通过(状态: $status), 查看日志: ./hacks/docker.sh logs"
    fi
    ;;
  rebuild)
    "${COMPOSE[@]}" up -d --build --force-recreate
    echo "▶ 已强制重建并重启"
    ;;
  status)
    "${COMPOSE[@]}" ps
    ;;
  logs)
    "${COMPOSE[@]}" logs -f --tail=100
    ;;
  down)
    "${COMPOSE[@]}" down
    echo "▶ 容器已删除"
    ;;
  *)
    echo "用法: $0 {start|stop|restart|redeploy|rebuild|status|logs|down}"
    exit 1
    ;;
esac
