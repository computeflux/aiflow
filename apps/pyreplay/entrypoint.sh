#!/bin/sh
# pyreplay 容器入口：后台起 'like' skill server（9500）+ 后台跑 social 登录任务
# 消费者（M1-4，login_worker.py，消费 Redis social:jobs:queue 里的 login 任务），
# 前台跑 Temporal worker。
# skill server 崩溃后由守护循环 5s 自动重启（MySQL 瞬断 / 浏览器异常等不再导致
# 进程永久退出、端口 9500 失联）。
# login worker 同样守护循环拉起；它启动即探活 MySQL/Redis，配置缺失会立刻退出，
# 5s 重启循环下会在日志里持续可见（不静默）。
# 前台进程（main.py）退出即整体退出，让 docker --restart always 拉起整个容器。

SKILL_PID_FILE=/tmp/skill_server.pid
LOGIN_PID_FILE=/tmp/login_worker.pid

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

# 后台守护：social 登录任务消费者（board 账号页「触发登录」的执行端）
(
    while true; do
        python3 login_worker.py &
        echo "$!" > "$LOGIN_PID_FILE"
        wait
        echo "[entrypoint] login worker exited, restarting in 5s..."
        sleep 5
    done
) &
LOGIN_DAEMON_PID=$!

cleanup() {
    [ -f "$SKILL_PID_FILE" ] && kill "$(cat "$SKILL_PID_FILE")" 2>/dev/null
    [ -f "$LOGIN_PID_FILE" ] && kill "$(cat "$LOGIN_PID_FILE")" 2>/dev/null
    kill "$DAEMON_PID" 2>/dev/null
    kill "$LOGIN_DAEMON_PID" 2>/dev/null
    exit 0
}
trap cleanup INT TERM EXIT

python3 main.py
