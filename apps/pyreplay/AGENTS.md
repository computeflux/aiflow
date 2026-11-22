# pyreplay（执行 Worker）

Python MCP 执行 worker，系统标识 `system=pyreplay`，以 Docker 容器运行。是 **X/Twitter 点赞/回复 bot**，作为 Temporal worker 执行分发下来的任务（如 `FollowActivities.dispatch_likes`）。

## 技术栈
- **Python 3.12+**，用 `uv` 管理（`uv.lock`），虚拟环境 `.venv`
- `pyproject.toml` 声明依赖
- 关键依赖：`crawl4ai`、`temporalio`、`opentelemetry-*`、`pymysql`、`python-dotenv`
- 代码规范：ruff（`line-length = 120`）
- 类型检查：basedpyright（`basic`）

## 目录结构
- `main.py` — 主入口
- `entrypoint.sh` — 容器入口脚本
- `adapters/` — 各平台执行适配器（X 点赞/回复）
- `services/` — 业务服务
- `config/` — 配置
- `telemetry.py` — 遥测
- `skill_server.py` / `skill.json` — Skill 服务入口
- `tests/` — 测试

## 运行
```bash
cd apps/pyreplay
uv run python3 main.py            # 本地直跑
docker build -t pyreplay . && docker run -d --name pyreplay pyreplay
docker logs -f pyreplay           # 看容器日志
```

## 约定
- 遵循仓库根级约定（见上级 `AGENTS.md`）。
- 任务失败顺延 15~60 分钟重试，超过 5 次标记 `failed` 并写入 `error_logs`（系统 `system=pyreplay`）。
- 遵循 MCP 协议与 Temporal activity 接口（如 `dispatch_likes`）。
- 改动后跑 ruff 检查；新依赖写进 `pyproject.toml` 并重新 `uv lock`。

## 脚本
容器入口脚本，镜像 `CMD ["sh", "entrypoint.sh"]`，**容器启动时自动执行，不是手工调用的运维脚本**。

| 脚本 | 用途 | 用法 |
|------|------|------|
| `entrypoint.sh` | 后台起 `skill_server.py`（默认 :9500，`SKILL_LIKE_PORT` 可覆盖）并守护重启（崩溃后等 5s 再起）；前台运行 `main.py`（Temporal worker）。`main.py` 退出即容器退出，由容器 `restart` 策略拉起 | 无需手工调用（镜像 `CMD` 在容器启动时自动执行） |

要点：
- 需要 `python3` 与镜像内依赖（`Dockerfile` 已装 `temporalio` / `crawl4ai` / Playwright Chromium 等）；工作目录必须是 `/app`（`main.py`、`skill_server.py`、`adapters/` 同级）。
- 只跑 worker 可用 `uv run python3 main.py`，但**不会**启动 9500 的 skill server；需要 skill 服务时用 `entrypoint.sh` 或容器内单独 `python3 skill_server.py`。
- `skill_server.py` 单独崩溃不会导致容器退出（守护循环每 5s 重启）；`main.py` 退出会触发 `trap` 杀掉 skill server 并结束容器。
- 改 `SKILL_LIKE_PORT` 时同步 `skill.json` 的 `addr`（默认 `http://127.0.0.1:9500`），否则调用方仍按 9500 寻址。
