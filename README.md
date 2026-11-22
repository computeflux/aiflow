# AI Workflow

MCP → RAG → 多平台发布工作流 + 自进化（S0–S9）平台。

技术栈：**Temporal** + **Go 主服务**（`apps/core`）+ **Python MCP Worker** + **Qdrant** 向量库 + **React/Vite** 前端（`apps/core-ui`）+ **统一 docker-compose** 部署（`deploy/`）。

> 目录约定见 `AGENTS.md`（根）与各自 `apps/*/AGENTS.md`。2026-11 起：board 前端五项目已合并为唯一前端 `apps/core-ui`；`apps/ai-bot` 已重命名为 `apps/ai-flow`；`board_ai` 旧实现与 `/review/` 路由已删除。

## 架构

- **Go 主服务**（`apps/core`）：工作流编排、CRAG 检索、REST API、Qdrant 读写、审批/灰度/审计、进化执行链、运维情报、资产画像。
- **ai-flow**（`apps/ai-flow`，PicoClaw fork）：board 右侧「AI 助手」侧边栏的后端 Agent（工具调用 + 系统上下文，可读运维/产品/skill 信息）。
- **Python Worker**（`apps/pycraper` / `apps/pyreplay`）：通过 MCP 从小红书 / X / 币安抓取与发布。
- **Temporal**：跨语言任务调度、重试、状态机（Web :8233）。
- **Qdrant**：RAG 向量库（UI :6333）。
- **core-ui**（`apps/core-ui`）：唯一 board 前端，单容器单端口 :5181，四板块路径入口 `/ops` `/product` `/content` `/email`。
- **基础设施**：Temporal + PostgreSQL、MySQL、Qdrant、Postiz、Synapse/Matrix、OTel + Jaeger。

## 目录

```
workflow/
├── apps/
│   ├── core/              # 工作流 Go 核心（独立 go.mod：github.com/wetee/ai-workflow）
│   │   ├── cmd/           # api :8001 / workflow / audit / evolve-driver / sandbox-runner /
│   │   │                  # flex-server / watchdog / health-patrol / ops-report / knowledge-sync /
│   │   │                  # deployctl / inventory / skillctl / agentctl / agent-pi / agent-ocr /
│   │   │                  # search-skill / generate-skill / evaluate-skill / matrix-bot / work-dispatch / mk-judge
│   │   ├── internal/      # Go 共享库（activities/workflows/approval/crag/qdrant/llm/deploy/asset/ops/product/…）
│   │   ├── agents/        # Agent manifest + adapter（pi=coder、ocr=reviewer、analyst）
│   │   ├── hacks/         # SQL DDL（bootstrap.sh 顺序执行 sql/*.sql）+ CI/开发脚本
│   │   └── hc/            # 健康检查探针（独立小 Go 模块）
│   ├── core-ui/           # 唯一 board 前端（React+Vite+TS 单入口 SPA，:5181；共享代码 src/kit/）
│   ├── ai-flow/           # AI 助手 Agent 运行时（独立 go.mod：github.com/computeflux/ai-flow；PicoClaw fork）
│   ├── auth/              # SSO / JWT 认证（容器化，对外 :8008）
│   ├── mailer/            # 邮件发送（独立 go.mod，:8003 / MAILER_PORT）
│   ├── metrics/           # sslmon 指标监控（独立 go.mod，SSL_HTTP_PORT）
│   ├── pycraper/          # Python 抓取 worker（system=pycraper，docker 容器）
│   ├── pyreplay/          # Python 执行 worker（system=pyreplay，docker 容器）
│   ├── social/            # 社媒发布（xhs 等，外部能力以依赖/服务方式接入，不纳管）
│   ├── pi-web/            # pi 编码代理 UI（本地 fork，Node 22 + Next.js，systemd 用户单元 :30141 / HTTPS :8443）
│   └── ...                # contract-deploy :8000 / tee-node-monitor :8002 / actives / c2c :8007 / minishop / dapp / game / web3auth / tee-node
├── deploy/                # 统一 docker-compose 部署入口（一服务一文件 + Dockerfile；用法见 deploy/README.md）
│   ├── compose/{infra,core,evolve,skills,marketing,tools}/
│   ├── config/            # mysql / temporal / otel / nginx-pi 配置
│   └── nginx/             # core-ui 同源代理配置
├── docs/                  # docs/README.md 总索引 + strategy/ + plan/ + marketing/ + ppt/ + standards/
└── bin/                   # Go 编译产物（本地调试用）
```

> Postiz（开源社媒发布平台）作为第三方服务在 `deploy/compose/infra/postiz.yml` 中编排（postiz + postiz-postgres + postiz-redis），仓库根不再有 `postiz/` 目录。

`apps/*` 中内部模块（`core` / `core-ui` / `auth` / `mailer` / `ai-flow` / `pycraper` / `pyreplay` / `metrics`）由主仓库追踪；外部可选项目（`actives` / `c2c` / `game` / `minishop` / `social` / `dapp` / `tee-node*` / `web3auth` / `contract-deploy` / `pi-web`）各自是独立 git 仓库（remote = `github.com/computeflux/<name>.git`），在主仓库不追踪。

## 本地启动（部署方式唯一：统一 docker-compose）

```bash
cd /home/wetee/work/cathome/workflow

# 0. 复制并填写环境变量（运行时配置全走根 .env）
cp .env.example .env

cd deploy
docker compose up -d --build               # 全系统一条命令启动/重建（基础设施+核心+技能）
docker compose ps                          # 全部 healthy 即就绪
```

改代码后必须**重建镜像**：`docker compose up -d --build <svc>`（`restart` 不加载新代码；仅镜像更新时还需 `--force-recreate`）。

```bash
# 常用
cd deploy && docker compose restart <svc>                                  # 重启旧镜像
cd deploy && docker compose up -d --build core-ui                          # 改前端后
cd apps/core && go build ./... && go test ./...                            # 本地 Go 构建/测试
cd apps/core-ui && npm run lint && npm run build                           # 本地前端校验
cd apps/core-ui && npm run dev                                             # 前端开发（:5181，四板块）
bash apps/core/hacks/bootstrap.sh                                          # 数据库 DDL（sql/*.sql）
docker compose --profile tools run --rm skillctl list                       # 一次性工具（tools profile）
```

本地前台调试（可选）：

```bash
docker logs -f pycraper                          # 抓取 worker 容器日志
cd apps/pycraper && .venv/bin/python3 main.py    # 本地直跑（依赖见 pyproject.toml）
cd apps/core && bash hacks/build.sh && ./bin/api # 本地编译 + 直跑 REST API :8001
```

> 对外仅 board **:5181**（core-ui 单容器：`/ops/` + `/product/` + `/content/` + `/email/`，`/` 302 → `/ops/`；SSO 登录枢纽 = `/ops/login`）/ **8008**（auth，浏览器 web3auth SDK 直连）/ **8443**（pi-web HTTPS：nginx-pi TLS 终结 + DigiCert 证书 xiaobai.asyou.me，Basic Auth）；
> 8001 / 8003 / 3306 / 7233 / 8233 / 6333 / 6334 / 4007 仅绑 127.0.0.1；技能与进化链服务（sandbox-runner :8123、audit :8120、evolve-driver :8121、flex-server :8124、watchdog :8125、health-patrol :8127、ops-report :8128、knowledge-sync :8129、ai-flow :8134、dind :2375、synapse / otel / jaeger）仅内部网络。

## 环境变量

根 `.env` 是唯一运行时配置来源（Go 服务启动自动加载，容器内挂载到 `/app/.env`）。关键项见 `.env.example`：

- `OPENAI_API_KEY` / `OPENAI_BASE_URL`；`EMBEDDING_MODEL`（默认 `bge-m3`，1024 维）、`LLM_MODEL`
- `MCP_X_COMMAND` / `MCP_XHS_COMMAND` / `MCP_BINANCE_COMMAND` + 各平台账号凭证（由 MCP server 读取）
- `AI_FLOW_BASE_URL`（core-ui nginx → ai-flow）、`AIFLOW_*`（ai-flow 运行时覆盖：provider / base_url / model_name / max_tool_iterations）、`PICOCLAW_HOME`
- `MAILER_PORT` / `SSL_HTTP_PORT`（mailer 与 metrics 默认都用 8003，部署时用这两个变量区分）
- `FOLLOW_LIKE_DISPATCH_CRON`、`EVOLVE_SCOPE_ALLOW_FILES` / `EVOLVE_SCOPE_ALLOW_LINES`（进化范围校验阈值，默认 2 文件 / 50 行）

**注意**：`apps/core/internal/qdrant/client.go` 的 `vectorSize` 为 `1024`（与 `bge-m3` 匹配）。若改用 `text-embedding-3-small`（1536 维），需改回 `vectorSize` 并删除 Qdrant collection 重建。

## 模型管理

- **模型列表页**：core-ui `/ops/models`（`model_profiles` 表，含 `consumers` 与 `sync_agents`）。
- **同步到 Agent**：标记 `sync_agents=1` 的模型可一键同步给 coder / reviewer / ai-flow（路由 `/model-config/sync-aiflow`）。
- **单 Agent 模型**：`GET/PUT /api/agents/:name/model` 写 `agents_registry.config.model_profile`（evolve-driver 消费）；`/ops/agents` 选择。
- **Reviewer（ocr）**：`/ops/ocr-reviews` 头部「Reviewer 模型」下拉直接切换。
- **ai-flow**：模型来自 `.env` 的 `AIFLOW_*`（与 `apps/ai-flow/config/config.json` 合并）。

## board 前端（core-ui）页面

单容器单端口 :5181，nginx 同源代理 `/api`、`/mailer`、`/jaeger`；右侧「AI 助手」侧边栏（可收起、状态持久化）由 ai-flow 驱动。

| 板块 | 入口 | 页面 |
|------|------|------|
| 运维中心 | `/ops/` | 任务控制台、服务总览、技能、审批中心、干预舱、Agent、模型管理、OCR 评审、错误日志、运维情报、链路追踪、用户 |
| 产品与任务 | `/product/` | 工作看板、工作项中心、审批中心、文档、思维导图、项目、原始条目 |
| 内容生产 | `/content/` | 社媒生成、X 订阅点赞管理、定时抓取、素材库、向量库、工作流历史、社媒发布 |
| 邮件 | `/email/` | 邮件推广、模板、发信服务商 |

```bash
cd apps/core-ui && npm run dev            # 开发（单 dev server :5181）
cd deploy && docker compose up -d --build core-ui
```

## API 验证

```bash
curl http://localhost:8001/health

curl -X POST http://localhost:8001/ingest  -H "Content-Type: application/json" -d '{"source":"x","keyword":"AI","limit":5}'
curl "http://localhost:8001/search?q=AI&k=5&days=7"
curl -X POST http://localhost:8001/generate -H "Content-Type: application/json" -d '{"topic":"AI","platform":"x","keyword":"AI"}'

curl "http://localhost:8001/jobs?status=pending"
curl -X POST http://localhost:8001/jobs/1/approve
curl -X POST http://localhost:8001/publish -H "Content-Type: application/json" -d '{"job_id":1}'

# AI 助手（经 core-ui 同源代理；SSE 流式返回）
curl -N -X POST http://localhost:5181/api/ai-flow/chat \
  -H "Content-Type: application/json" \
  -d '{"board_id":1,"session":"smoke","user_id":1,"content":"你好"}'
```

Temporal Web `http://localhost:8233` · Qdrant UI `http://localhost:6333/dashboard` · Jaeger（经 core-ui `/jaeger/`）

## 错误日志

运行错误统一写入 MySQL `error_logs`（Go 侧 GORM AutoMigrate 建表，Python worker 有 `CREATE TABLE IF NOT EXISTS` 兜底）：

- **Go API**（`apps/core/cmd/api`）：所有 >=500 的 HTTP 响应（含 panic）记为 `system=api`，另覆盖 startCron / mailer 失败；
- **Go Worker**（`apps/core/cmd/workflow`）：activity 拦截器把每个失败的 activity 尝试记为 `system=go-worker`（含 WorkflowID/RunID/attempt）；
- **Python Worker**（`apps/pycraper`、`apps/pyreplay`）：activity 拦截器 + 抓取失败按平台记为 `system=pycraper` / `system=pyreplay`（`source=fetch:<平台>`）。

core-ui「错误日志」页（`/ops/errors`）可筛选（系统/级别/状态/关键词）、标记已处理、删除；「任务控制台」（`/ops/`）展示最近 5 条未处理错误。

相关 API：`GET /errors`、`GET /errors/stats`、`POST /errors/:id/resolve`、`POST /errors/resolve-all`、`DELETE /errors/:id`。

## X 订阅点赞分发

订阅目标（`follow_targets`）的新推文点赞采用「分发 + 各机器后台执行」模式：

1. **分发**：Go cron workflow `follow-like-dispatch`（默认每 15 分钟，`FOLLOW_LIKE_DISPATCH_CRON` 可调）调用 pyreplay 的 `FollowActivities.dispatch_likes`（core-ui `/content/` 订阅目标行可点「立即分发」触发 `POST /follow-targets/:id/dispatch`）。扫描所有启用目标的推文，为每台机器（机器池 = `x_accounts` 去重 `machine_id`）生成 `like_jobs`，`scheduled_at` 在 0~24h 窗口随机（`config/x.json` 的 `like_jobs_window_hours` / `profile_scan_limit`）。
2. **执行**：每台机器的 pyreplay 主进程约 60 秒轮询本机到期任务并随机选账号点赞（账号级去重，两次点赞间隔随机 30~180 秒）。失败顺延 15~60 分钟重试，超 5 次标 `failed` 并写 `error_logs`。

`like_jobs` 唯一键 `(tweet_id, machine_id)`：不同机器可重复点赞同一推文，同一机器同一推文只处理一次。

## 自进化 Workflow 使用手册

> 总纲：`docs/plan/self-evolving-workflow.md`；分阶段计划：`docs/plan/S0–S9-*.md`；设计与 k8s 映射：`docs/plan/evolve-plan.md`。以下命令均在 `deploy/` 执行。

### 识别（软件资产画像）

```bash
docker compose --profile tools run inventory            # 扫描仓库 → software_assets 表（纳管/不纳管清单）
```

### 部署（一键全量恢复 / 编排 / 回滚）

```bash
docker compose --profile tools run deployctl deploy all       # build（并行）→ up -d → 健康检查 → 失败自愈一次 → 仍败即停+干预+告警
docker compose --profile tools run deployctl status           # 逐资产健康总览
docker compose --profile tools run deployctl rollback <svc>   # 镜像回滚 / 停服务
docker compose --profile tools run deployctl auto-update      # git pull + 重建
```

健康自愈由 `health-patrol`（10 分钟 ticker）执行，动作仅限 restart / rebuild+restart / skip；连续 3 轮失败升级 Matrix 告警。

### 审批与灰度（上线必须人工，24h 过期无默认通过）

- 审批中心：`:5181/ops/approvals`（通过 / 拒绝，全程 `audit_logs` 留痕）
- Matrix 审批：审批单推送 #ops-alerts，Bot 内 approve/reject
- 审批通过后自动灰度：5% → 50% → 100%（30 分钟/档），恶化自动回滚
- 版本容器（静态 compose 的唯一例外）：

```bash
docker compose --profile tools run deployctl version up <skill> <ver> --env test   # 测试实例（195xx 端口）
docker compose --profile tools run deployctl version up <skill> <ver> --env gray   # 灰度实例（网关 addr 指向它）
docker compose --profile tools run deployctl version down <skill> <ver>            # 停实例（回滚 / retired）
```

### 进化链（自动推进，人负责放行）

```bash
docker compose --profile tools run --rm -T agentctl register /repo/apps/core/agents/pi/agent.json
docker compose --profile tools run --rm -T agentctl bind coder pi
docker compose --profile tools run --rm -T agentctl bind reviewer ocr
docker compose --profile tools run --rm -T ci-runner <task_no>     # 手工跑 CI
```

任务卡 `ready_for_evolve` → evolve-driver 领取（worktree 隔离）→ coder 改码 → 范围校验 → reviewer 行级门禁 → 沙箱 → CI 契约 → 人工审批 → 灰度 5/50/100 → 上线；恶化自动回滚。

红线：diff **绝不**自动 push/merge 主分支（审批通过后自动推 `evolve/<task_no>` feature 分支 + 建 GitHub PR，由人工合并）；coder 写范围默认仅限 `apps/*`，未显式选中时保护 `apps/board*` / `apps/core*`，小范围越界允许（≤ `EVOLVE_SCOPE_ALLOW_FILES` 文件 / `EVOLVE_SCOPE_ALLOW_LINES` 行，默认 2/50），超阈值打回人工 PR。

### 干预舱与运维情报

- 干预舱 `:5181/ops/interventions`：活跃 workflow（事件数 / 连续失败 / 最近错误）+ skill 熔断圆点 + 接管 / 注入 / 重试 / 回滚（全程留痕）。`watchdog`（1 分钟 ticker）检测死循环。
- 运维情报 `:5181/ops/ops-reports`：日报/周报/升级建议（建议可一键转任务卡；high 风险只生成人工 PR 建议单）。
- 产品 Agent `:5181/product/`：对话生成产品文档/变更需求 → 自动拆解任务卡；`knowledge-sync`（daily 02:00）把内部文档增量向量化入 Qdrant（secret 脱敏红线）。

## 不纳入统一 compose（不纳管清单）

业务 dApp 维持各自部署：`contract-deploy`（:8000，`cd apps/contract-deploy && ./docker.sh rebuild`）、`tee-node-monitor`（:8002，`cd apps/tee-node-monitor && docker compose up -d --build`）、minishop / c2c（:8007，ego TEE 进程）/ actives 等。宿主仅保留 systemd 用户单元 `pi-web.service`（:30141）与 `workflow-c2c.service`（TEE 进程）。
