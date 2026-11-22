#!/usr/bin/env bash
# 管理 fork 版的 pi-web（apps/pi-web）：安装到全局 npm、设置默认工作目录、管理 systemd 用户服务。
#
# 默认工作目录 = 仓库根（$DIR），可用环境变量 PI_WEB_DEFAULT_CWD 覆盖。
# 用法: agents/pi/pi-web.sh {install|build|env|start|stop|restart|reload|enable|disable|status}
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"  # monorepo root（apps/core/agents/pi -> ../../../..）
PI_WEB_DIR="$DIR/apps/pi-web"            # fork 源码
ENV_FILE="$DIR/agents/pi/config/env"     # systemd 读取的 pi-web 环境变量
UNIT="pi-web.service"
# 默认工作目录 = 仓库根；可用 PI_WEB_DEFAULT_CWD 覆盖
DEFAULT_CWD="${PI_WEB_DEFAULT_CWD:-$DIR}"

# 幂等地确保 ENV_FILE 里 PI_WEB_DEFAULT_CWD 指向目标目录。
ensure_default_cwd() {
  mkdir -p "$(dirname "$ENV_FILE")"
  if grep -q '^PI_WEB_DEFAULT_CWD=' "$ENV_FILE" 2>/dev/null; then
    sed -i "s|^PI_WEB_DEFAULT_CWD=.*|PI_WEB_DEFAULT_CWD=$DEFAULT_CWD|" "$ENV_FILE"
  else
    echo "PI_WEB_DEFAULT_CWD=$DEFAULT_CWD" >> "$ENV_FILE"
  fi
}

case "${1:-}" in
  install)
    echo "正在构建 $PI_WEB_DIR ..."
    cd "$PI_WEB_DIR"
    npm run build
    echo "正在把 fork 版 pi-web 装到全局 npm（覆盖 upstream）..."
    npm install -g .
    ensure_default_cwd
    systemctl --user daemon-reload
    echo "完成：默认工作目录=$DEFAULT_CWD"
    ;;
  build)
    cd "$PI_WEB_DIR"
    npm run build
    ;;
  env)
    ensure_default_cwd
    echo "PI_WEB_DEFAULT_CWD=$DEFAULT_CWD (写入 $ENV_FILE)"
    ;;
  start)
    systemctl --user start "$UNIT" && echo "$UNIT 已启动" ;;
  stop)
    systemctl --user stop "$UNIT" && echo "$UNIT 已停止" ;;
  restart)
    ensure_default_cwd
    systemctl --user restart "$UNIT" && echo "$UNIT 已重启"
    ;;
  reload)
    systemctl --user daemon-reload && echo "已 daemon-reload" ;;
  enable)
    systemctl --user enable "$UNIT" && echo "$UNIT 已开机自启" ;;
  disable)
    systemctl --user disable "$UNIT" && echo "$UNIT 已禁用开机自启" ;;
  status)
    systemctl --user status "$UNIT" --no-pager -l 2>&1 | head -n 20 ;;
  *)
    echo "用法: $0 {install|build|env|start|stop|restart|reload|enable|disable|status}"
    exit 1
    ;;
esac
