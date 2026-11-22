# MCP → RAG → 多平台发布工作流

技术栈：**Temporal** + **Go** 主服务 + **Python MCP Worker** + **Qdrant** 向量库。

## 架构

- **Go 服务**（`apps/core`）：工作流编排、CRAG 检索、REST API、Qdrant 写入/查询、MySQL 审批任务。
- **Python Worker**：通过 MCP 从小红书 / X / 币安抓取与发布。
- **Qdrant**：RAG 向量库。
- **Temporal**：跨语言任务调度、重试、状态机。
- 审批后台：board 前端（`apps/core-ui` 单容器单端口 :5181，单入口 SPA，路径入口 `/ops/` 运维中心 + `/product/` 产品与任务 + `/content/` 内容生产 + `/email/` 邮件，共享代码在 core-ui 的 `src/kit/`）

## 目录

> 2026-10-10 仓库重组：Go 工作流核心整体迁入 `apps/core`；board 五项目（board-ops / board-product / board-content / board-email / board-kit）合并为 `apps/core-ui`（唯一 board 前端项目，标准单入口 SPA）。根目录现为 monorepo hub（`.env` + `deploy/` + `docs/` + `apps/`）。

```
workflow/
├── apps/
│   ├── core/             # 工作流 Go 核心（独立 go.mod：github.com/wetee/ai-workflow）
│   │   ├── cmd/          # 入口（api :8001 / workflow / audit / evolve-driver / ...，见 apps/core/AGENTS.md）
│   │   ├── internal/     # Go 共享库（activities/workflows/qdrant/llm/db/approval/...）
│   │   ├── agents/       # Agent manifest + adapter（pi/ocr/fake）
│   │   ├── hacks/        # SQL DDL + CI/开发脚本
│   │   └── hc/           # 健康检查探针（独立小模块）
│   ├── core-ui/          # board 前端合并体（React+Vite+TS 单入口 SPA：/ops + /product + /content + /email 四板块，:5181 单端口；共享代码 src/kit/）
│   ├── pycraper/         # Python 抓取 worker（system=pycraper，docker 容器）
│   ├── pyreplay/         # Python 执行 worker（system=pyreplay，docker 容器）
│   ├── contract-deploy/  # 合约控制台（8000，独立 go.mod）
│   ├── tee-node-monitor/ # 链上监控（8002，独立 go.mod）
│   ├── actives/          # 链上数据 / 积分（独立 go.mod）
│   ├── mailer/           # 邮件发送（独立 go.mod，:8003）
│   ├── metrics/          # sslmon 指标监控（独立 go.mod，:8003）
│   ├── pi-web/           # pi 编码代理 UI（本地 fork，Node 22 + Next.js）
│   └── ...               # c2c / minishop / dapp / game / web3auth / tee-node / auth 等
├── deploy/           # 统一 docker-compose 部署入口（一服务一文件，用法见 deploy/README.md）
├── bin/              # Go 编译产物（api / go-worker，本地调试用）
└── docs/             # 方案文档（S0–S9 计划等）
```

## 本地启动（部署方式唯一：统一 docker-compose）

```bash
cd /home/wetee/work/cathome/workflow

# 0. 复制并填写环境变量（运行时配置全走根 .env）
cp .env.example .env

cd deploy
cp .env.example .env 2>/dev/null || true   # compose 级变量（首次）
docker compose up -d --build               # 全系统一条命令启动/重建（基础设施+核心+技能）
docker compose ps                          # 全部 healthy 即就绪
```

改代码后必须重建镜像：`docker compose up -d --build <svc>`（`restart` 不加载新代码）。
一次性工具：`docker compose --profile tools run skillctl list`。详见 `deploy/README.md`。

本地前台调试（可选）：

```bash
docker logs -f pycraper    # 抓取 worker 容器日志
cd apps/pycraper && .venv/bin/python3 main.py    # 或本地直跑（依赖见 pyproject.toml）
cd apps/core && bash hacks/build.sh && ./bin/api # 本地编译 + 直跑 REST API :8001
```

> 对外仅 board :5181（core-ui 单容器：`/ops/` 运维中心 + `/product/` 产品与任务，`/` 302 → `/ops/`）/ :5182（内容生产）/ :5183（邮件，各 nginx 同源代理 `/api/`、`/mailer/`、`/jaeger/`；SSO 登录枢纽 = :5181/ops/login）/ 8008（auth，浏览器 web3auth SDK 直连）/ 8443（pi-web，HTTPS：nginx-pi TLS 终结 + DigiCert 证书 xiaobai.asyou.me，Basic Auth 防护）；
> 8001 / 3306 / 7233 / 8233 / 6333 / 6334 / 4007 仅绑 127.0.0.1；技能 / otel / jaeger 端口仅内部网络。

合约控制台（:8000）与链上监控（:8002）为 docker 单服务部署，见 `apps/contract-deploy/` 与 `apps/tee-node-monitor/`。

## 环境变量

复制 `.env.example` 为 `.env` 并填写。Go 服务启动时会自动加载 `.env`。

- `OPENAI_API_KEY` / `OPENAI_BASE_URL`
- `EMBEDDING_MODEL`：默认 `bge-m3`（Ollama），向量维度 1024
- `LLM_MODEL`：默认 `llama3.2`（Ollama）
- `MCP_X_COMMAND` / `MCP_XHS_COMMAND` / `MCP_BINANCE_COMMAND`
- 各平台账号凭证（由 MCP server 读取）

**注意**：当前 `internal/qdrant/client.go` 的 `vectorSize` 为 `1024`，与 `bge-m3` 匹配。若改用 `text-embedding-3-small`（1536 维），需改回 `vectorSize` 并删除 Qdrant collection 重建。


## 验证

```bash
curl http://localhost:8001/health

curl -X POST http://localhost:8001/ingest \
  -H "Content-Type: application/json" \
  -d '{"source":"x","keyword":"AI","limit":5}'

curl "http://localhost:8001/search?q=AI&k=5&days=7"

curl -X POST http://localhost:8001/generate \
  -H "Content-Type: application/json" \
  -d '{"topic":"AI","platform":"x","keyword":"AI"}'

curl "http://localhost:8001/jobs?status=pending"

curl -X POST http://localhost:8001/jobs/1/approve
curl -X POST http://localhost:8001/publish \
  -H "Content-Type: application/json" \
  -d '{"job_id":1}'
```

访问 Temporal Web：`http://localhost:8233`  
访问 Qdrant UI：`http://localhost:6333/dashboard`

## 错误日志

各系统的运行错误统一写入 MySQL 的 `error_logs` 表（Go 侧 GORM AutoMigrate 自动建表，Python worker 有 `CREATE TABLE IF NOT EXISTS` 兜底）：

- **Go API**（`cmd/api/main.go`）：所有 >=500 的 HTTP 响应（含 panic）记录为 `system=api`，另覆盖 startCron / mailer 失败；
- **Go Worker**（`cmd/workflow/main.go`）：activity 拦截器把每个失败的 activity 尝试记录为 `system=go-worker`（含 WorkflowID/RunID/attempt）；
- **Python Worker**（`apps/pycraper/main.py`、`apps/pyreplay/main.py`）：activity 拦截器 + 抓取失败按平台记录为 `system=pycraper` / `system=pyreplay`（`source=fetch:<平台>`）。

core-ui 的「错误日志」页（`:5181/ops/errors`）可查看、按系统/级别/状态/关键词筛选、标记已处理、删除；「任务控制台」首页（`:5181/ops/`）也展示最近 5 条未处理错误。

相关 API：`GET /errors`、`GET /errors/stats`、`POST /errors/:id/resolve`、`POST /errors/resolve-all`、`DELETE /errors/:id`。

## X 订阅点赞分发

订阅目标（`follow_targets`）的新推文点赞改为“分发 + 各机器后台执行”模式，不再由单台机器实时扫赞：

1. **分发**：Go 侧 cron workflow `follow-like-dispatch`（默认每 15 分钟，可用环境变量 `FOLLOW_LIKE_DISPATCH_CRON` 调整）调用 pyreplay 的 `FollowActivities.dispatch_likes`（core-ui `/content/` 的订阅目标行也可点“立即分发”手动触发 `POST /follow-targets/:id/dispatch`）。分发器扫描所有启用订阅目标的推文，为**每台机器**（机器池来自 `x_accounts` 的去重 `machine_id`）生成一条 `like_jobs` 任务，`scheduled_at` 在 0~24 小时窗口内随机分布（`config/x.json` 的 `like_jobs_window_hours`，扫描条数 `profile_scan_limit`）。
2. **执行**：每台机器上的 pyreplay 主进程后台轮询（约 60 秒）本机到期的 `like_jobs`，随机选一个本机账号点赞——同一推文每台机器每个账号都会点一次（账号级去重，排除已点过的账号），每日不限量；两次点赞间随机间隔 30~180 秒。失败任务顺延 15~60 分钟重试，超过 5 次标记 `failed` 并写入 `error_logs`（core-ui `/ops/` 错误日志页可见）。

`like_jobs` 唯一键 `(tweet_id, machine_id)`：不同机器可重复点赞同一推文，同一机器同一推文只处理一次。

## 审批 / 运维 UI（board 前端）

board 按功能域拆分为前端板块：原 board-ops / board-product / board-content / board-email / board-kit 五项目已全部合并为 `apps/core-ui`（**单容器单端口 :5181，标准单入口 SPA**，路径入口 `/ops/`（原 :5181）+ `/product/`（原 :5184）+ `/content/`（原 :5182）+ `/email/`（原 :5183）；共享代码 `src/kit/`）；nginx 同源代理 `/api`/`/mailer`/`/jaeger`：

| 服务 | 地址 | 职责 |
|------|------|------|
| `apps/core-ui`（单容器单端口） | :5181 `/ops/` | 运维中心：任务控制台、技能、审批中心、干预舱、Agent、错误日志、运维情报、链路追踪、用户管理 |
| `apps/core-ui`（同容器） | :5181 `/product/` | 产品与任务：产品情报（AI 对话/文档/拆解）、工作看板、思维导图 |
| `apps/core-ui`（同容器） | :5181 `/content/` | 内容生产：社媒生成、X 点赞管理、定时抓取、素材库、向量库、工作流历史 |
| `apps/core-ui`（同容器） | :5181 `/email/` | 邮件：邮件推广、模板、发信服务商

```bash
# 开发（单 dev server :5181，四板块 /ops /product /content /email）
cd apps/core-ui && npm install && npm run dev
# 部署（单容器单端口）
cd deploy && docker compose up -d --build core-ui
```

支持：pending / approved / rejected / expired 筛选，通过 / 拒绝操作（全程 audit_logs 留痕）。

## 自进化 Workflow 使用手册

> 总纲：`docs/plan/self-evolving-workflow.md`；分阶段计划：`docs/plan/S0–S8-*.md`。
> 部署方式唯一：统一 docker-compose（`deploy/`）；以下命令均在 `deploy/` 目录执行。

### 识别（软件资产画像）

```bash
docker compose --profile tools run inventory          # 扫描仓库 → software_assets 表（纳管/不纳管清单）
```

### 部署（一键全量恢复 / 编排 / 回滚）

```bash
docker compose --profile tools run deployctl deploy all     # build（并行）→ up -d → 健康检查 → 失败自愈一次 → 仍败即停+干预+告警
docker compose --profile tools run deployctl status         # 逐资产健康总览
docker compose --profile tools run deployctl rollback <svc> # 镜像回滚 / 停服务
docker compose --profile tools run deployctl auto-update    # git pull + 重建
docker compose up -d --build <svc>                         # 改代码后必须重建镜像才生效
```

健康自愈由 `health-patrol`（10 分钟 ticker）自动执行，动作仅限 restart / rebuild+restart / skip；连续 3 轮失败升级 Matrix 告警。`docker compose stop health-patrol` 即退回人工运维。

### 审批（上线必须人工，24h 过期无默认通过）

- core-ui 审批中心：`http://<host>:5181/ops/approvals`（通过 / 拒绝，全程 audit_logs 留痕）
- Matrix 审批：审批单推送到 #ops-alerts，Bot 内 approve/reject
- 审批通过后自动启动灰度：5% → 50% → 100%（30 分钟/档），恶化自动回滚

### 版本容器与灰度（静态 compose 的唯一例外）

```bash
docker compose --profile tools run deployctl version up <skill> <ver> --env test   # 测试实例（195xx 端口，供人工测试）
docker compose --profile tools run deployctl version up <skill> <ver> --env gray   # 灰度实例（生产网，网关 addr 指向它，5% 起步）
docker compose --profile tools run deployctl version down <skill> <ver>            # 停实例（回滚 / retired）
```

### 干预舱（死循环 / 熔断 / 人工接管）

core-ui 干预舱页 `:5181/ops/interventions`：活跃 workflow 表（事件数/连续失败/最近错误）+ 熔断圆点 + 接管 / 注入 / 重试 / 回滚（全程留痕）。

### 运维情报与产品 Agent

- 运维：core-ui `:5181/ops/ops-reports`（日报/周报/升级建议，建议可一键转任务卡；high 风险只生成人工 PR 建议单）
- 产品：core-ui `:5181/product/`（对话生成产品文档/变更需求 → 自动拆解为任务卡；skill/app 类置 `ready_for_evolve` 进进化链，core 类只走人工 PR）

### 进化链（自动推进，人负责放行）

任务卡 `ready_for_evolve` → evolve-driver 领取（worktree 隔离）→ coder 改码（仅 `apps/*`，排除 `apps/board*` 与 `apps/core*`）→ reviewer 行级门禁 → 沙箱 → CI 契约 → 人工审批 → 灰度 5/50/100 → 上线；恶化自动回滚。红线：diff 绝不自动合入主分支；核心代码（`apps/core`）与 board 前端（`apps/core-ui` 等）只能人工 PR。
