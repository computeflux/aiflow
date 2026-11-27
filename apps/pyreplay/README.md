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
│   ├── x_adapter.py       # XAdapter 门面（登录 / 登录态读写）
│   ├── x_login.py         # X/Twitter 登录链 + 按账号登录态存储 + 账号池回报
│   └── x_like.py          # X/Twitter 点赞
├── config/                # 平台配置 JSON
│   └── x.json
├── services/              # 业务服务
│   ├── account_manager.py # x_accounts 账号池（冷却 / 每日限额 / 最久未用随机）
│   ├── like_skill.py      # ★ skill 'like' 内核：schema + 账号轮换 + 落库（三条入口共用）
│   ├── mysql.py           # MySQL 连接（惰性 + 断线重连）
│   └── social_report.py   # M1-4 登录结果回报 social 账号池
├── skill.json             # skill 'like' 的契约（input/output schema + limits）
├── skill_server.py        # skill 'like' 的 HTTP 服务（:9500，skillgateway 调用）
├── main.py                # Temporal Worker 入口
└── pyproject.toml         # 项目元数据 & 依赖
```

## Activities

| Activity | 说明 |
|---|---|
| `LikeActivities.like` | 点赞单条推文（随机挑选账号） |
| `LikeActivities.like_batch` | 批量点赞：随机打乱顺序、逐条走 like 技能内核（账号在整批内去重轮换）、随机间隔。返回 `{total, success, failed, results}`，`results` 为逐条 `{tweet_id, tweet_url, success, error}`，供调用方（Go workflow）判断哪些推文真正点赞成功 |
| `FollowActivities.scan_and_like` | 订阅：扫描 `follow_targets` 中到期目标的用户主页，提取新推文（与 `like_records` 去重），用随机账号点赞。参数 `target_id` 可选，指定则只扫该目标 |

待点赞的推文列表由上层 Go workflow（`LikeSkillWorkflow`，见 `apps/core/internal/workflows/like_skill.go`）从 `raw_items` 表中查询后传入，本 worker 不直接查询业务库，只负责执行点赞动作并写自己的 `like_records` 日志表。订阅功能（`scan_and_like`）由 worker 自己读 `follow_targets` 表，去重依赖 `like_records`。

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

## like 技能内核（`services/like_skill.py`，M3-2）

`like` 是 pyreplay 暴露的第一个 skill（契约见 `skill.json`）。M3-2 把点赞能力的实现收口成
**一个**内核 `services.like_skill.LikeSkill`，三条入口全部复用它，行为逐字段等于 skill schema：

| 入口 | 调用方 | 说明 |
| --- | --- | --- |
| `skill_server.py` `POST /invoke` | skillgateway（board / agent / `skillctl invoke like`） | HTTP，输入输出即 schema |
| `main.py` `LikeActivities.like` / `like_batch` | Temporal（pyreplay 任务队列上的既有/手工投递调用） | activity 契约不变，内部改走内核 |
| `main.py` `_execute_like_job`（like_jobs 执行器） | 本机后台轮询 | 会话式批量；账号在会话内锁定、记账按会话计一次 |

内核契约（与 `skill.json` 一字不差；`results[]` 逐条给 `tweet_id` / `success` / `error`）：

```text
input  = {"post_url": "https://x.com/.../status/<id>", "like_count": 1..20}   # 缺省 1，越界钳到 [1,20]
output = {"ok": bool, "reason": str, "tweet_id": str, "attempts": int,
          "success_count": int, "results": [{"tweet_id", "success", "error"}]}
```

* `ok` = `success_count > 0`：网关按顶层 `ok` 记熔断计数；业务失败也回 HTTP 200 + `ok=false`
  （HTTP 状态只表达进程级错误），**不能**把「一条都没点动」写成 5xx —— 那会被 Temporal
  当成传输失败重试，一条坏推文能把整批拖成 N×重试次数的浏览器会话；
* 账号级去重：同一次调用内每个账号只用一次，跨调用由调用方传 `excluded_account_ids`；
  账号记账（冷却 / 每日限额）走注入的账号池接缝，策略在实现侧 —— worker 与 skill HTTP 进程
  逐条记，like_jobs 会话执行器整场会话只记一次；
* 逐条写 `like_records`，全失败写 `error_logs`；落库/记账异常一律吞成日志，绝不冒泡杀进程；
* 输出附带 `account_ids`（本次用过的账号；schema 允许附加属性），供调用方扩宽去重范围。

单测（不联网、不起浏览器、不连库：账号池 / 点赞执行器 / 落库全部鸭子注入）：

```bash
cd apps/pyreplay
uv run pytest tests/test_like_skill.py -q      # 有 pytest 时（dev 依赖组）
python3 tests/test_like_skill.py               # 无第三方依赖时的直跑契约（CI 兜底口径）
```

Go 侧的配套测试：`apps/core/internal/skillcontract`（读 `apps/pyreplay/skill.json`，逐字段比对
Go 契约常量与 schema 的字段名 / minimum / maximum，任一侧改名即红）和
`apps/core/internal/workflows/like_skill_test.go`（Temporal test environment 跑通「取数 →
InvokeSkill → 回写」整条编排：钉住走的是 Skill 网关、送给技能的输入符合 schema、只有
技能回报成功的推文才回写 `raw_items.liked`）。

## X 登录（`XAdapter.login`）

登录链在 `adapters/x_login.py`（流程 + 登录态存储 + 账号池回报），门面在 `adapters/x_adapter.py`；
skill HTTP 服务暴露为 `POST /login`（与 `POST /invoke` 点赞分锁串行）。

```bash
curl -s -X POST http://127.0.0.1:9500/login -H 'Content-Type: application/json' \
  -d '{"account":"alice@example.com","password":"***","headless":true}'
```

返回**脱敏**摘要（`auth_token` / `ct0` 只出掩码）：

```json
{"ok": false, "account": "alice@example.com", "username": "alice",
 "needs_manual": true, "intervention": "two_factor", "qrcode_path": "",
 "message": "account 'alice' requires a 2FA code (no handler installed)",
 "session_file": ".../sessions/alice_example_com.json", "profile_dir": ".../profiles/alice_example_com",
 "source": "", "account_manager": "updated", "auth_token": "a1b2****a1b2", "ct0": "0011****0123"}
```

要点：

1. **按账号隔离**：浏览器持久化 profile 与会话文件都按账号目录/文件名隔离
   （`X_PROFILE_DIR` / `X_SESSION_DIR`，会话文件 0600），多账号互不串登录态。
2. **人工介入占位接口**：2FA 与扫码是两条互斥路径，`two_factor_code` 直传优先；
   未装 handler 时不阻塞、不猜，直接回报 `needs_manual=true` + `intervention` +
   （扫码时）二维码落盘路径。接 Matrix / board 审批时传 `make_intervention(two_factor=..., qrcode=...)`，
   或继承 `ManualInterventionHandler`。⚠️ 扫码顺序固定为**先落盘二维码、再调 handler**
   （handler 常阻塞等人扫码，码必须先存在）；码取不到就返回空路径并判本次登录失败。
3. **介入必须闭环**：只有拿到有效登录态（`auth_token` + `ct0`）才算成功；
   handler 只声明「要人工」而没给码 → 登录失败（`needs_manual`），不会因页面出现登录元素就当成功。
   ⚠️ `needs_manual` 只表示「介入**没闭环**」，与 `intervention`（页面**要求过**哪种介入）是两回事：
   2FA 码提交成功后 `intervention="two_factor"` 而 `needs_manual=false`、`ok=true` ——
   两者混用会让 board 把成功登录当成「仍需人工」，反复带码重登直到撞风控。
4. **回报账号池**（`x_accounts`）：成功写回 `auth_token`/`ct0` 并置 `enabled=1`、清 `last_login_error`；
   失败只写 `last_login_at` + `last_login_error`，**绝不覆盖**账号原有登录态，也**绝不为
   「从没登录成功的账号」建 `enabled=1` 的空壳行**（`load_eligible` 不校验 token 是否为空，
   空壳行会被挑去点赞并必然失败）。未配 `MYSQL_HOST` 时回报为 `skipped: ...`，登录链照常落盘。

`config/x.json` 登录相关配置：

| 字段 | 说明 | 默认 |
|---|---|---|
| `login_2fa_code_max_len` | 2FA 码长度上限（形状防御，不做业务校验） | 16 |
| `login_intervention_probe_ms` | 探测「要 2FA 还是要扫码」的有界超时 | 1500 |
| `login_persist_session` | 登录成功后写会话文件 | true |
| `login_notify_account_manager` | 登录结果回报 `x_accounts` | true |
| `login_timeout_sec` | 登录整链超时（起浏览器 + 页面动作 + 介入） | 300 |

DDL：老库补登录回报列跑 `bash apps/core/hacks/bootstrap.sh`（含
`apps/core/hacks/sql/x_accounts_login_columns.sql`，幂等）；新库由 Go 侧 AutoMigrate 直接建出。

单测：`cd apps/pyreplay && uv run pytest tests/test_x_login.py -q`（无 pytest 时可直跑
`python3 tests/test_x_login.py`，同一套断言）。

登录整链超时：`login_timeout_sec`（默认 300 秒）覆盖「起浏览器 + 全部页面动作 + 人工介入」。
超时的实现是**取消**登录协程，而被取消的协程里 `await` 推进不了任何收尾（连 `asyncio.shield`
也不行：shield 只让内层任务在外层取消时存活，不会让外层多等一秒），所以收尾走
`_run_cleanup` / `_drain_cleanups` 这条「登记 → 驱动层补跑」通道。⚠️ 两个必须守住的细节：

- 登录协程必须用 `asyncio.ensure_future` **显式建 Task** 并把它交给 `_drain_cleanups`；
  `asyncio.wait_for(coro, ...)` 会自己新建 Task，登记方（登录协程里的 `current_task()`）与
  drain 方（外层 `current_task()`）就会是两个对象，通道静默空转，收尾任务在 `asyncio.run()`
  退出时被 `_cancel_all_tasks` 直接 cancel。
- 「有 page」与「无 page」两条收尾不能走错：句柄必须在 `_teardown` 清空它**之前**取出，
  否则会退化成「起一次性 driver 打开同一 user-data-dir 再关掉」，而那个目录正被没关的
  context 占着。漏掉收尾的代价是每次超时漏一个 persistent context + node driver 进程，并让
  账号的 user-data-dir 留着 `SingletonLock`，下次登录同一账号直接起不来。

改这块务必同步跑 `tests/test_x_login.py` 里的三个超时/释放用例（含不打桩真收尾那条）。
