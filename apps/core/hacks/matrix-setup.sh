#!/usr/bin/env bash
# S4 Matrix 供给脚本（docs/plan/S4-协作底座.md §3.1）：幂等完成
#   1. 注册 bot 用户 @approval-bot / @audit-bot（synapse admin /register，shared secret）
#   2. 建 3 个房间 #ops-alerts / #approvals / #agent-log（alias + public 加入规则）
#   3. 每房间发一条测试消息（验证 token / alias 解析 / 发送链路）
#
# 用法（仓库根目录）：
#   ./hacks/matrix-setup.sh
#
# 说明：
#   - 全部 curl 经 `docker exec synapse` 在容器内执行（8008 不发布宿主端口）。
#   - shared secret 读 deploy/.env 的 SYNAPSE_REGISTRATION_SHARED_SECRET。
#   - 注册 MAC 公式（synapse 1.12+，rest/admin/users.py）：
#       HMAC-SHA1(secret, nonce \0 username \0 password \0 "admin"|"notadmin")
#     注意 bash printf 的 NUL 用 \0（octal）；\x00 在 zsh 下不展开。
#   - 已存在用户且密码未知时：@approval-bot 回退用根 .env 的 MATRIX_BOT_TOKEN；
#     @audit-bot 跳过（备用用户，非必需）。
#   - 全新环境（synapse-data 卷清空后）跑本脚本，把输出的 MATRIX_BOT_TOKEN 填回根 .env。
set -euo pipefail

cd "$(dirname "$0")/.."

HS="http://127.0.0.1:8008"
SS=$(grep '^SYNAPSE_REGISTRATION_SHARED_SECRET=' deploy/.env | cut -d= -f2 | tr -d '\r')
if [ -z "${SS:-}" ]; then
  echo "deploy/.env 缺少 SYNAPSE_REGISTRATION_SHARED_SECRET" >&2
  exit 1
fi
# 规范 server name（不带端口）：从 synapse 容器配置读取
SERVER_NAME=$(docker exec synapse python3 -c "import yaml; print(yaml.safe_load(open('/data/homeserver.yaml'))['server_name'])" 2>/dev/null) || true
if [ -z "${SERVER_NAME:-}" ]; then
  SERVER_NAME="dev-workflow.computeflux.ai"
fi
BOT_PASSWORD="matrix-bot-s4-2026" # 全新环境的固定密码（dev 用；真正凭据是 access token）

echo "== S4 Matrix 供给（server=$SERVER_NAME）=="

# register <user> <password> → access token（失败返回空）
register() {
  local user="$1" pass="$2" nonce mac
  nonce=$(docker exec synapse curl -sf -H "Authorization: Bearer $SS" "$HS/_synapse/admin/v1/register" \
    | sed -n 's/.*"nonce":"\([^"]*\)".*/\1/p') || return 0
  [ -n "$nonce" ] || return 0
  mac=$(printf "${nonce}\0${user}\0${pass}\0notadmin" | openssl dgst -sha1 -hmac "$SS" | awk '{print $NF}')
  docker exec synapse curl -s -X POST -H "Content-Type: application/json" \
    -d "{\"nonce\":\"$nonce\",\"mac\":\"$mac\",\"username\":\"$user\",\"password\":\"$pass\",\"admin\":false,\"displayname\":\"$user\"}" \
    "$HS/_synapse/admin/v1/register" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
}

# login <user> <password> → access token（失败返回空）
login() {
  docker exec synapse curl -s -X POST -H "Content-Type: application/json" \
    -d "{\"identifier\":{\"type\":\"m.id.user\",\"user\":\"$1\"},\"type\":\"m.login.password\",\"password\":\"$2\"}" \
    "$HS/_matrix/client/v3/login" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
}

# room_id <alias> <token> → room_id（不存在返回空）
room_id() {
  docker exec synapse curl -sf -H "Authorization: Bearer $2" \
    "$HS/_matrix/client/v3/directory/room/%23${1}:${SERVER_NAME}" \
    | sed -n 's/.*"room_id":"\([^"]*\)".*/\1/p' || true
}

# create_room <name> <alias> <token>（幂等：alias 已存在则跳过；preset=public_chat）
create_room() {
  local name="$1" alias="$2" token="$3" rid
  if rid=$(room_id "$alias" "$token") && [ -n "$rid" ]; then
    echo "  房间 #$alias 已存在（$rid），跳过"
    return 0
  fi
  rid=$(docker exec synapse curl -s -X POST -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
    -d "{\"name\":\"$name\",\"room_alias_name\":\"#$alias\",\"preset\":\"public_chat\"}" \
    "$HS/_matrix/client/v3/createRoom" | sed -n 's/.*"room_id":"\([^"]*\)".*/\1/p')
  if [ -z "$rid" ]; then
    echo "  房间 #$alias 创建失败" >&2
    return 1
  fi
  echo "  房间 #$alias 已建（$rid）"
}

# send_test <alias> <token>（alias 解析 + 发消息，验证通知链路）
send_test() {
  local alias="$1" token="$2" rid
  rid=$(room_id "$alias" "$token") || { echo "  #$alias 不存在，无法发测试消息" >&2; return 1; }
  local esc
  esc=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=''))" "$rid")
  docker exec synapse curl -sf -X PUT -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
    -d "{\"msgtype\":\"m.text\",\"body\":\"S4 供给脚本：#$alias 房间就绪（$(date -u +%FT%TZ)）\"}" \
    "$HS/_matrix/client/v3/rooms/$esc/send/m.room.message/setup-$(date +%s)" > /dev/null
  echo "  测试消息已发 → #$alias"
}

# ---- 1. bot 用户 ----
APPROVAL_TOKEN=""
EXISTING=$( { grep '^MATRIX_BOT_TOKEN=.' .env 2>/dev/null || true; } | head -1 | cut -d= -f2 | tr -d '\r' )
if [ -n "$EXISTING" ]; then
  APPROVAL_TOKEN="$EXISTING"
  echo "@approval-bot：复用根 .env 的 MATRIX_BOT_TOKEN"
else
  APPROVAL_TOKEN=$(register "approval-bot" "$BOT_PASSWORD")
  [ -n "$APPROVAL_TOKEN" ] || {
    echo "@approval-bot 注册失败（用户已存在但密码未知？请手工设置 .env 的 MATRIX_BOT_TOKEN）" >&2
    exit 1
  }
  echo "@approval-bot：已注册"
fi

AUDIT_TOKEN=$(register "audit-bot" "$BOT_PASSWORD")
if [ -n "$AUDIT_TOKEN" ]; then
  echo "@audit-bot：已注册"
else
  AUDIT_TOKEN=$(login "audit-bot" "$BOT_PASSWORD")
  if [ -n "$AUDIT_TOKEN" ]; then
    echo "@audit-bot：已存在（login 取 token）"
  else
    echo "@audit-bot：已存在但密码未知，跳过（备用用户，非必需）"
  fi
fi

# ---- 2. 房间 ----
create_room "Ops Alerts"  "ops-alerts" "$APPROVAL_TOKEN"
create_room "Approvals"   "approvals"  "$APPROVAL_TOKEN"
create_room "Agent Log"   "agent-log"  "$APPROVAL_TOKEN"

# ---- 3. 测试消息 ----
send_test "ops-alerts" "$APPROVAL_TOKEN"
send_test "approvals"  "$APPROVAL_TOKEN"
send_test "agent-log"  "$APPROVAL_TOKEN"

# ---- 汇总 ----
echo
echo "== 完成。若为全新环境，把以下 token 填入根 .env 的 MATRIX_BOT_TOKEN："
echo "MATRIX_BOT_TOKEN=$APPROVAL_TOKEN"
