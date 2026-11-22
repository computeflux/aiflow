# PyReplay

X/Twitter like/reply bot — Temporal worker.  
专门用于点赞推文的 Temporal worker，类似 pycraper 的架构。

## 快速开始

### 前置依赖

- [uv](https://docs.astral.sh/uv/) >= 0.4.0
- Python 3.12

### 安装

```bash
cd pyreplay
uv sync
source .venv/bin/activate
```

### 环境变量

创建 `.env` 文件：

| 变量 | 说明 |
|---|---|
| `MYSQL_HOST` / `MYSQL_PORT` / `MYSQL_USER` / `MYSQL_PASSWORD` / `MYSQL_DATABASE` | MySQL 连接 |
| `TEMPORAL_HOST_PORT` / `TEMPORAL_NAMESPACE` / `TEMPORAL_PYTHON_TASK_QUEUE` | Temporal 配置 |
| `X_AUTH_TOKEN` / `X_CT0` | X (Twitter) 登录 Cookie（从浏览器中提取） |
| `MACHINE_ID` | 机器标识（如 `bot-01`）。每台机器人设不同值，worker 只操作 `x_accounts` / `follow_targets` 中 `machine_id` 匹配本值的账号与订阅目标；留空 = `default` 分组 |

### 运行

**本地直跑**（`.env` 里是容器内地址，本地需覆盖为宿主机地址）：

```bash
cd apps/pyreplay
MYSQL_HOST=127.0.0.1 TEMPORAL_HOST_PORT=127.0.0.1:7233 uv run python3 main.py
```

**Docker 部署**（生产方式；容器在 `temporal-network` + `workflow_default` 两个网络，
`skill_server.py` 直接读 `os.environ` 不加载 `.env`，所以所有变量必须用 `-e` 显式传入）：

```bash
cd apps/pyreplay
docker build -t pyreplay .
docker rm -f pyreplay 2>/dev/null
docker run -d --name pyreplay --restart always \
  --network temporal-network \
  -e MYSQL_HOST=workflow-mysql -e MYSQL_PORT=3306 \
  -e MYSQL_USER=workflow -e MYSQL_PASSWORD=workflow123 -e MYSQL_DATABASE=workflow \
  -e TEMPORAL_HOST_PORT=temporal:7233 -e TEMPORAL_NAMESPACE=default \
  -e TEMPORAL_PYTHON_TASK_QUEUE=pyreplay-task-queue \
  -e X_AUTH_TOKEN=<从 .env 取> -e X_CT0=<从 .env 取> \
  -e MACHINE_ID=replay-0 \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 -e OTEL_SERVICE_NAME=pyreplay \
  pyreplay
docker network connect workflow_default pyreplay
docker logs -f pyreplay
```

> ⚠️ 改代码后必须**重建镜像**（`docker build` + 重新 `docker run`），`docker restart` 只重启旧镜像、不加载新代码。

## 项目结构

```
pyreplay/
├── adapters/              # 平台适配器
│   ├── models.py          # 数据模型 (Pydantic)
│   └── x_like.py          # X/Twitter 点赞
├── config/                # 平台配置 JSON
│   └── x.json
├── main.py                # Temporal Worker 入口
└── pyproject.toml         # 项目元数据 & 依赖
```

## Activities

| Activity | 说明 |
|---|---|
| `LikeActivities.like` | 点赞单条推文（随机挑选账号） |
| `LikeActivities.like_batch` | 批量点赞：随机打乱顺序、随机切换账号、随机间隔。返回 `{total, success, failed, results}`，`results` 为逐条 `{tweet_id, tweet_url, success, error}`，供调用方（Go workflow）判断哪些推文真正点赞成功 |
| `FollowActivities.scan_and_like` | 订阅：扫描 `follow_targets` 中到期目标的用户主页，提取新推文（与 `like_records` 去重），用随机账号点赞。参数 `target_id` 可选，指定则只扫该目标 |

待点赞的推文列表由上层 Go workflow（`LikeWorkflow`，见 `internal/workflows/like.go`）从 `raw_items` 表中查询后传入，本 worker 不直接查询业务库，只负责执行点赞动作并写自己的 `like_records` 日志表。订阅功能（`scan_and_like`）由 worker 自己读 `follow_targets` 表，去重依赖 `like_records`。

## 多账号 & 随机化（反封号）

1. **多机器隔离**：每台机器人部署时在 `.env` 设置不同的 `MACHINE_ID`（如 `bot-01`），管理后台添加账号/订阅目标时填相同的机器 ID，worker 只操作属于自己机器的数据。留空 = `default` 分组（单机部署零配置）。
2. **多账号**：账号存在 MySQL `x_accounts` 表（每行一个账号的 `auth_token`/`ct0`、每日限额、冷却时间、所属机器）。`AccountManager` 每次操作随机挑一个**可用**账号（属于本机、启用、未达每日限额、冷却期已过），且避免连续两次用同一账号；挑选时按"最久未用"排序、只从前 1/3 随机选，兼顾分散使用与随机性。
3. **随机间隔**：批量/订阅点赞之间随机等待（默认 30~180 秒），由 `config/x.json` 的 `min_delay_between_likes` / `max_delay_between_likes` 控制；批量任务的顺序也会随机打乱。
4. **无账号池时的回退**：本机账号池为空时，回退使用 `.env` 中的 `X_AUTH_TOKEN` / `X_CT0`（单账号模式，保持向后兼容）。

## 数据库

pyreplay 不负责建表。表结构说明如下：

- `x_accounts`（点赞账号池）与 `follow_targets`（订阅目标）：由 Go API（`cmd/api`）的 GORM AutoMigrate 自动创建，并通过管理后台（board 的「X 点赞管理」页面）维护。两表都有 `machine_id` 字段，用于多机器人部署时按机器隔离数据。
- `like_records`（点赞结果日志）：pyreplay worker 自己写入的表，需要在 MySQL 中手动创建：

```sql
CREATE TABLE like_records (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    tweet_id VARCHAR(64) NOT NULL DEFAULT '',
    tweet_url VARCHAR(512) NOT NULL DEFAULT '',
    success TINYINT(1) NOT NULL DEFAULT 0,
    error VARCHAR(1024) NOT NULL DEFAULT '',
    liked_at DATETIME NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_tweet_id (tweet_id),
    INDEX idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

`config/x.json` 新增配置：

| 字段 | 说明 | 默认 |
|---|---|---|
| `profile_scroll_steps` | 扫描用户主页时的滚动次数 | 5 |
| `min_delay_between_likes` / `max_delay_between_likes` | 两次点赞之间的随机间隔（秒） | 30 / 180 |
| `default_daily_like_limit` / `default_cooldown_minutes` | 新账号默认每日限额 / 冷却分钟数 | 50 / 10 |
