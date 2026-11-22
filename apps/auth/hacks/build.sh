#!/usr/bin/env bash
# auth 编译 + 运行（前台，Ctrl-C 停止）。
# 用法:
#   bash hacks/build.sh          # 编译 + 运行
#   bash hacks/build.sh -b       # 只编译
#   AUTH_PORT=8008 bash hacks/build.sh
set -eu

SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
APP_DIR=$(CDPATH= cd -- "$SELF_DIR/.." && pwd)
ROOT=$(CDPATH= cd -- "$APP_DIR/../.." && pwd)

cd "$APP_DIR"
export PATH="$PATH:$HOME/Soft/go/bin"

go build -o bin/auth .
echo "[auth] built bin/auth"

if [ "${1:-}" = "-b" ]; then
  exit 0
fi

# 运行前自动停掉旧实例
pkill -f "bin/auth" 2>/dev/null || true
sleep 0.5

# 根 .env 提供 MYSQL_* / SMTP_*（config.Load 依次加载 .env / ../.env / ../../.env）
exec ./bin/auth
