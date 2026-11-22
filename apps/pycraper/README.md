# PyCraper

MCP scraper worker for ComputeFlux — fetches content from social platforms (X/Twitter, Reddit, GitHub) and publishes via Temporal workflows.

## 快速开始

### 前置依赖

- [uv](https://docs.astral.sh/uv/) >= 0.4.0
- Python 3.12

### 安装

```bash
cd pycraper
uv sync
source .venv/bin/activate
```

### 环境变量

复制或创建 `.env` 文件：

| 变量 | 说明 |
|---|---|
| `MYSQL_HOST` / `MYSQL_PORT` / `MYSQL_USER` / `MYSQL_PASSWORD` / `MYSQL_DATABASE` | MySQL 连接 |
| `TEMPORAL_HOST_PORT` / `TEMPORAL_NAMESPACE` / `TEMPORAL_PYTHON_TASK_QUEUE` | Temporal 配置 |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` / `LLM_MODEL` | LLM 配置（推文生成） |
| `REDDIT_CLIENT_ID` / `REDDIT_CLIENT_SECRET` | Reddit API 凭证（可选，无则回退浏览器抓取） |
| `GITHUB_TOKEN` | GitHub API token（可选，提高限流） |
| `X_AUTH_TOKEN` / `X_CT0` | X (Twitter) 登录凭证（从浏览器 Cookie 中提取，可选） |
| `MCP_XHS_COMMAND` | 小红书 MCP server 命令（覆盖 `config/xiaohongshu.json` 的 `mcp_command`，可选） |

### 运行

```bash
python main.py
```

## 部署（systemd + 自动更新）

```bash
git clone https://github.com/computeflux/workflow.git
cd workflow
# 配置 .env 文件
./hacks/bootstrap.sh
```

`bootstrap.sh` 会：
1. 安装 systemd service
2. `uv sync` 安装依赖
3. 启动 worker（`workflow-py-worker`）
4. 启动自动更新定时器（每 5 分钟检查 GitHub 新 commit → `git pull` → 重启 worker）

查看状态：

```bash
systemctl status workflow-py-worker
systemctl status computeflux-auto-update.timer
journalctl -u workflow-py-worker -f   # 实时日志
```

## 项目结构

```
pycraper/
├── adapters/              # 平台适配器
│   ├── base.py            # 基类 & 公共配置加载
│   ├── github.py          # GitHub
│   ├── reddit.py          # Reddit（浏览器抓取，无 API key）
│   ├── reddit_api.py      # Reddit（官方 API，有 API key）
│   ├── reddit_scan.py     # Reddit 浏览器爬虫实现
│   ├── x.py               # X/Twitter
│   ├── x_scan.py          # X 爬虫实现
│   ├── xiaohongshu.py     # 小红书
│   ├── mcp_client.py      # MCP 协议客户端
│   ├── models.py          # 数据模型 (Pydantic)
│   └── logging.py         # 日志
├── agents/                # 业务服务
│   ├── ranker.py          # 推文评分/排序
│   └── social_generator.py # AI 推文生成
├── config/                # 平台配置 JSON
├── tests/                 # 测试脚本
│   ├── test_github.py
│   ├── test_reddit.py
│   ├── test_reddit_api.py
│   ├── test_x.py
│   └── test_xiaohongshu.py
├── scripts/               # 数据迁移脚本
├── main.py                # Temporal Worker 入口
├── pyproject.toml         # 项目元数据 & 依赖
└── uv.lock                # 依赖锁定文件
```

## 多 Agent 并行

部署多个 worker 实例到不同机器，Temporal 自动分发任务：

```bash
# 每台机器上
git clone ... && ./hacks/bootstrap.sh
```

如需同一条抓取任务由多个 agent 并行执行（如 X 关键词搜索），在 Console 页面创建任务时设置 `parallel_count > 1`。
