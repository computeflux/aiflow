#!/usr/bin/env bash
set -euo pipefail

# 兼容入口：py worker（pycraper / pyreplay）已容器化，
# 实际控制逻辑统一在 hacks/docker-workers.sh。
#   hacks/py-worker.sh start     # 启动/重建容器（幂等）
#   hacks/py-worker.sh restart   # 同上（start 本身会 rm -f 重建）
#   hacks/py-worker.sh stop      # 停止并删除容器
#   hacks/py-worker.sh disable   # 同上（删除容器即不再自启）
#   hacks/py-worker.sh status    # 查看状态与最近日志
#   hacks/py-worker.sh rebuild   # 重新构建镜像并重建容器（git pull 后）

case "${1:-}" in
  start|restart) exec bash "$(dirname "$0")/docker-workers.sh" start ;;
  stop|disable)  exec bash "$(dirname "$0")/docker-workers.sh" stop ;;
  status)        exec bash "$(dirname "$0")/docker-workers.sh" status ;;
  rebuild)       exec bash "$(dirname "$0")/docker-workers.sh" rebuild ;;
  *)
    echo "用法: $0 {start|stop|status|disable|restart|rebuild}"
    exit 1
    ;;
esac
