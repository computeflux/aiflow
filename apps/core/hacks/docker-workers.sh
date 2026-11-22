#!/usr/bin/env bash
set -euo pipefail

# pycraper / pyreplay 容器化运行脚本（替代原 systemd py worker 单元；
# ⚠️ 已退役（S0）：两 worker 已由统一 compose 管理（deploy/compose/core/pycraper.yml / pyreplay.yml），
# 本脚本仅作历史参考——勿在生产使用（docker run 会与 compose 容器同名冲突）。
#   hacks/docker-workers.sh start     # 启动两个 worker 容器（幂等）
#   hacks/docker-workers.sh stop      # 停止并删除容器
#   hacks/docker-workers.sh rebuild   # 重新构建镜像并重建容器（git pull 后执行）
#   hacks/docker-workers.sh status    # 查看状态与最近日志

# 2026-10-10 仓库重组：hacks 在 apps/core/hacks，monorepo 根在上一级的上一级
REPO_DIR="$(cd "$(dirname "$0")/../.." && pwd)"

# Temporal 与 MySQL 均为 docker compose 容器：跨网络用容器名直连
# （temporal 在 temporal-network，workflow-mysql 在 workflow_default）
_pycraper_run() {
  docker rm -f pycraper >/dev/null 2>&1 || true
  docker run -d --name pycraper --restart always \
    --network temporal-network \
    -v "$REPO_DIR/apps/pycraper/config:/app/config:ro" \
    --env-file "$REPO_DIR/apps/pycraper/.env" \
    -e TEMPORAL_HOST_PORT=temporal:7233 \
    -e MYSQL_HOST=workflow-mysql \
    -e OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 \
    -e OTEL_SERVICE_NAME=pycraper \
    pycraper:latest >/dev/null
  docker network connect workflow_default pycraper 2>/dev/null || true
}

_pyreplay_run() {
  docker rm -f pyreplay >/dev/null 2>&1 || true
  docker run -d --name pyreplay --restart always \
    --network temporal-network \
    -p 127.0.0.1:9500:9500 \
    -v "$REPO_DIR/apps/pyreplay/config:/app/config:ro" \
    --env-file "$REPO_DIR/apps/pyreplay/.env" \
    -e TEMPORAL_HOST_PORT=temporal:7233 \
    -e MYSQL_HOST=workflow-mysql \
    -e OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 \
    -e OTEL_SERVICE_NAME=pyreplay \
    pyreplay:latest >/dev/null
  docker network connect workflow_default pyreplay 2>/dev/null || true
}

case "${1:-}" in
  start)
    _pycraper_run
    _pyreplay_run
    echo "pycraper / pyreplay 容器已启动"
    ;;
  stop)
    docker rm -f pycraper pyreplay >/dev/null 2>&1 || true
    echo "pycraper / pyreplay 容器已停止"
    ;;
  rebuild)
    docker build -t pycraper:latest "$REPO_DIR/apps/pycraper"
    docker build -t pyreplay:latest "$REPO_DIR/apps/pyreplay"
    _pycraper_run
    _pyreplay_run
    echo "镜像已重建，容器已重启"
    ;;
  status)
    docker ps -a --filter name=pycraper --filter name=pyreplay \
      --format 'table {{.Names}}\t{{.Status}}\t{{.Networks}}'
    echo
    for c in pycraper pyreplay; do
      echo "--- $c（最近日志）---"
      docker logs "$c" 2>&1 | tail -5
      echo
    done
    ;;
  *)
    echo "用法: $0 {start|stop|rebuild|status}"
    exit 1
    ;;
esac
