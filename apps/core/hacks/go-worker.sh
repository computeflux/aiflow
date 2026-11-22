#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

case "${1:-}" in
  start|restart)
    echo "==> 编译 go-worker..."
    cd "$PROJECT_DIR" && go build -o bin/go-worker ./cmd/workflow/
    echo "  完成"
    systemctl --user "${1:-restart}" workflow-go-worker && echo "workflow-go-worker 已${1:-restart}"
    ;;
  stop)    systemctl --user stop workflow-go-worker && echo "workflow-go-worker 已停止" ;;
  status)  systemctl --user status workflow-go-worker --no-pager -l 2>&1 | head -30 ;;
  disable) systemctl --user disable workflow-go-worker && echo "workflow-go-worker 已禁用（关闭开机自启）" ;;
  *)
    echo "用法: $0 {start|stop|status|disable|restart}"
    exit 1
    ;;
esac
