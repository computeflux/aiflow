#!/bin/bash
# 数据库 bootstrap：顺序执行 hacks/sql/ 下全部 DDL（S1 起，docs/plan/S1-进化闭环.md §3.1）。
# 旧版（systemd 单元安装 + docker-workers.sh）已随 S0 退役——服务一律走统一 compose：
#   cd deploy && docker compose up -d --build
#
# 用法（仓库根目录）：
#   bash apps/core/hacks/bootstrap.sh
set -euo pipefail

HACKS_DIR="$(cd "$(dirname "$0")" && pwd)"
# 2026-10-10 仓库重组：hacks 在 apps/core/hacks，monorepo 根在上一级的上一级
REPO_ROOT="$(cd "$HACKS_DIR/../.." && pwd)"

# 读取根 .env（mysql 凭据）；只取需要的键——.env 含 cron 表达式（`*/2`）等
# 非 shell 安全的值，不能直接 source。
env_val() {
  [ -f "$REPO_ROOT/.env" ] && \
    grep -E "^[[:space:]]*$1=" "$REPO_ROOT/.env" | tail -n1 | cut -d= -f2- || true
}
MYSQL_USER="${MYSQL_USER:-$(env_val MYSQL_USER)}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-$(env_val MYSQL_PASSWORD)}"
MYSQL_DATABASE="${MYSQL_DATABASE:-$(env_val MYSQL_DATABASE)}"
MYSQL_USER="${MYSQL_USER:-workflow}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-workflow123}"
MYSQL_DATABASE="${MYSQL_DATABASE:-workflow}"

echo "=== ComputeFlux DB Bootstrap ==="
echo "  db: $MYSQL_USER@$MYSQL_DATABASE"

# 库默认 collation 对齐 utf8mb4_0900_ai_ci（2026-10-30）：
# 手写 DDL 用 `DEFAULT CHARSET=utf8mb4` 会解析成字符集默认 0900；GORM AutoMigrate 建的表
# 则继承库默认。若库默认是旧的 unicode_ci，两者拼不起来（JOIN 报 ERROR 1267）。
# 老库靠这条改默认，新库由 compose 的 --collation-server 直接建对。
# ALTER DATABASE 不支持 prepared statement，只能带字面库名 → 放在这里用 $MYSQL_DATABASE。
echo ">>> aligning database collation to utf8mb4_0900_ai_ci"
(cd "$REPO_ROOT/deploy" && docker compose exec -T mysql \
  mysql --default-character-set=utf8mb4 -u"$MYSQL_USER" -p"$MYSQL_PASSWORD" \
  -e "ALTER DATABASE \`$MYSQL_DATABASE\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")

for f in "$HACKS_DIR"/sql/*.sql; do
  [ "$(basename "$f")" = "grants.sql" ] && continue   # 权限收口需 root，见下
  echo ">>> applying $(basename "$f")"
  # 显式 utf8mb4：容器内 mysql CLI 在 locale=C 时默认 latin1，会把中文写成双编码乱码
  (cd "$REPO_ROOT/deploy" && docker compose exec -T mysql \
    mysql --default-character-set=utf8mb4 -u"$MYSQL_USER" -p"$MYSQL_PASSWORD" "$MYSQL_DATABASE" < "$f")
done

# 权限收口：应用账号无 GRANT 权限，以 root 执行
if [ -f "$HACKS_DIR/sql/grants.sql" ]; then
  MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD:-$(env_val MYSQL_ROOT_PASSWORD)}"
  MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD:-workflow123}"
  echo ">>> applying grants.sql (as root)"
  (cd "$REPO_ROOT/deploy" && docker compose exec -T mysql \
    mysql --default-character-set=utf8mb4 -uroot -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE" < "$HACKS_DIR/sql/grants.sql")
fi

echo "=== Done ==="
