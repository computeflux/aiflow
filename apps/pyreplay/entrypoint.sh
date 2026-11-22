#!/bin/sh
# pyreplay 容器入口：后台起 'like' skill server（9500），前台跑 Temporal worker。
# skill server 崩溃后由守护循环 5s 自动重启（MySQL 瞬断 / 浏览器异常等不再导致
# 进程永久退出、端口 9500 失联）。
# 前台进程（main.py）退出即整体退出，让 docker --restart always 拉起整个容器。

SKILL_PID_FILE=/tmp/skill_server.pid

# 后台守护：skill server 退出后自动重启
(
    while true; do
        python3 skill_server.py &
        echo "$!" > "$SKILL_PID_FILE"
        wait
        echo "[entrypoint] skill server exited, restarting in 5s..."
        sleep 5
    done
) &
DAEMON_PID=$!

cleanup() {
    [ -f "$SKILL_PID_FILE" ] && kill "$(cat "$SKILL_PID_FILE")" 2>/dev/null
    kill "$DAEMON_PID" 2>/dev/null
    exit 0
}
trap cleanup INT TERM EXIT

python3 main.py
