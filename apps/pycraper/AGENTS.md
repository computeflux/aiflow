# pycraper（抓取 Worker）

Python MCP 抓取 worker，系统标识 `system=pycraper`，以 Docker 容器运行。通过 MCP 从小红书 / X / 币安等平台**抓取**内容并写入 MySQL / Qdrant。

## 技术栈
- **Python 3.12+**，用 `uv` 管理（`uv.lock`），虚拟环境 `.venv`
- `pyproject.toml` 声明依赖
- 关键依赖：`crawl4ai`、`mcp`、`temporalio`、`opentelemetry-*`、`pymysql`、`pydantic`、`requests`、`python-dotenv`
- 代码规范：ruff（`line-length = 120`）
- 类型检查：basedpyright（`basic`）

## 目录结构
- `main.py` — 主入口，Temporal activity（`fetch`/`publish`/`social_generate`）+ 错误落库（`system=pycraper`）
- `adapters/` — 抓取适配器（见下）
- `agents/` — 抓取逻辑
- `config/` — 配置
- `telemetry.py` — 遥测（OpenTelemetry）
- `skill_entry.py` / `skill.json` — Skill 入口
- `tests/` — 测试

## 适配器（`adapters/`）
| 文件 | 类/说明 | source |
|------|---------|--------|
| `base.py` | `BaseAdapter(ABC)` 抽象基类 | — |
| `mcp_client.py` | `MCPClient` 封装 stdio MCP server，`split_command` 解析命令 | 通用 |
| `models.py` | `RawItem` 数据模型，校验并映射 MCP 工具结果（URL 去重键） | — |
| `x.py` | `XAdapter`：X 抓取/发布（MCP 或 Crawl4AI 兜底） | `x` |
| `x_scan.py` | X 直接扫描（浏览器/代理/cookie）抓取 | `x` |
| `xiaohongshu.py` | `XiaohongshuAdapter`：小红书 MCP（只读 `config/xiaohongshu.json`，字段 `mcp_command`/`fetch_tool`/`publish_tool`） | `xiaohongshu` |
| `reddit.py` | `RedditAdapter` | `reddit` |
| `reddit_api.py` | `RedditAPIAdapter` | `reddit` |
| `github.py` | `GitHubAdapter`（`source=github`） | `github` |
| `logging.py` | `get_logger` 日志 | — |

抓取经 MCP 或 Crawl4AI，MCP 配置**只读 JSON 文件、不读环境变量**：`XAdapter` 读 `config/x.json`（字段 `mcp_x_command`/`mcp_x_fetch_tool`/`mcp_x_publish_tool`），`XiaohongshuAdapter` 读 `config/xiaohongshu.json`（字段 `mcp_command`/`fetch_tool`/`publish_tool`）。**MCP 未配置时不能假装成功**：`mcp_command` 取值为 `echo` 是禁用哨兵（fetch 直接返回空、publish 显式抛 `RuntimeError`），避免 spawn `echo` 当 MCP server 卡住。

## 运行
```bash
cd apps/pycraper
uv run python3 main.py            # 本地直跑（依赖见 pyproject.toml）
docker build -t pycraper . && docker run -d --name pycraper pycraper
docker logs -f pycraper           # 看容器日志
```
启动时自动加载 `.env`（`python-dotenv`）。

## 脚本

本项目**没有独立脚本**：没有任何 `.sh`，也没有 `package.json` / npm scripts（Python 项目，依赖由 `pyproject.toml` + `uv.lock` 管理）。Python 入口是 `main.py`（Temporal worker）与 `skill_entry.py`（x-scan skill 的 CLI 模式）。运行方式见上方「运行」，实际可用命令：

| 命令 | 用途 |
|------|------|
| `uv sync` | 按 `uv.lock` 建/更新 `.venv`（README 的安装步骤；`dev` 依赖组含 `pytest`） |
| `uv run python3 main.py` | 本地直跑 Temporal worker（等价于 `source .venv/bin/activate` 后 `python main.py`） |
| `docker build -t pycraper .` | 构建镜像（`Dockerfile` 用 `pip` 装依赖 + Playwright Chromium，`CMD ["python3", "main.py"]`） |
| `docker run -d --name pycraper pycraper` | 以容器运行 worker（需注入 `.env`，见下） |
| `docker logs -f pycraper` | 跟容器日志 |
| `uv run python tests/test_x.py` | 手工抓取自测（X）；`tests/test_github.py` / `tests/test_reddit.py` / `tests/test_reddit_api.py` 同理 |
| `uv run python tests/test_xiaohongshu.py` | 小红书 MCP 联调自测（单次 search_notes 抓取；加 `--publish` 再单次 publish_note 发布并确认 note id；仅测试账号，需先配置 mcp_command / MCP_XHS_COMMAND） |
| `uv run python3 skill_entry.py` | skill_entry CLI 模式：stdin 读 JSON、stdout 出 JSON（当前部署走 service 型 HTTP 端点，不靠这条） |

要点：
- **没有测试脚本/ pytest 配置**：`tests/*.py` 是带 `__main__` 的独立脚本（不是 pytest 用例），会**真实请求对应平台**；只能按上表 `uv run python tests/<name>.py` 手工跑。（`pytest` 只在 `dev` 依赖组里，项目未提供 pytest 配置。）
- 启动前必须提供（`main.py` 的 `_validate_env()` 缺失即报错退出）：`MYSQL_HOST` / `MYSQL_PORT` / `MYSQL_USER` / `MYSQL_PASSWORD` / `MYSQL_DATABASE`、`TEMPORAL_HOST_PORT` / `TEMPORAL_NAMESPACE` / `TEMPORAL_PYTHON_TASK_QUEUE`。
- `.env` 从**本目录**加载（`load_dotenv(Path(__file__).parent / ".env")`）；容器内即 `/app/.env`，但 `.dockerignore` 已排除 `.env`，所以**镜像里没有 `.env`**，必须挂载或注入。
- worker 启动后同时跑 x-scan HTTP 端点 `POST :9504/invoke`（`XSCAN_HTTP_PORT` 可覆盖），供 Skill 网关调用。
- 统一部署走根 `deploy/`：compose 服务名 `pycraper`（`deploy/compose/core/pycraper.yml`，`env_file: apps/pycraper/.env`，并把 Temporal/MySQL 指向容器名）。
- `skill_entry.py` 的 CLI 用法（stdin 传 JSON）：
  ```bash
  echo '{"handle":"computefluxAI","limit":10}' | uv run python3 skill_entry.py
  ```
  输出 `{"ok":true,...}`；失败时 stdout 仍是 JSON（`ok:false`）且退出码 1。
- ruff（`line-length = 120`）与 basedpyright（`basic`）已在 `pyproject.toml` 配置，但**未列入依赖、也没有脚本**；当前仓库没有可直接跑的 lint 命令。

## 约定
- 遵循仓库根级约定（见上级 `AGENTS.md`）。
- 抓取失败按平台记录到 `error_logs`（`source=fetch:<平台>`），系统 `system=pycraper`；发布失败同样记录。
- 适配器新增平台时实现 `MCPClient` 调用或 `RawItem` 产出，保持与 `main.py` activity 签名一致。
- 遵循 MCP 协议与 Temporal activity 接口，不要破坏签名。
- 改动后跑 ruff 检查；新依赖写进 `pyproject.toml` 并重新 `uv lock`。
