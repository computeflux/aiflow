# ComputeFlux AI Workflow（根约定）

MCP → RAG → 多平台发布工作流。技术栈：**Temporal + Go 主服务 + Python MCP Worker + Qdrant**。

> **2026-10-10 仓库重组**：Go 工作流核心整体迁入 `apps/core`（独立子项目）；board 前端五项目（board-ops / board-product / board-content / board-email / board-kit）全部合并为 `apps/core-ui`（**唯一 board 前端项目**，单容器单端口 :5181，路径入口 /ops /product /content /email）。根目录现在是 monorepo hub：`.env` + `deploy/`（统一编排）+ `docs/` + `apps/`。各 `apps/*` 有自己的 `AGENTS.md`，会被自动继承并拼接到这里之后。进入某子目录即视为一个独立项目。

## 目录定位
- **`apps/core`**：工作流编排核心（Go 主模块 `github.com/wetee/ai-workflow`：`cmd/`、`internal/`、`agents/`、`hacks/`、`hc/`；`internal/crag|evaluate|generate` 共享包）。
- **`apps/core-ui`**：board 前端（原 board-ops / board-product / board-content / board-email / board-kit 全部合并；React+Vite+TS **标准单入口**项目：一个 `index.html` + 一个 `vite.config.ts`，四板块路由区分 + `React.lazy` 路由级代码分割；**单容器单端口 :5181，路径入口 `/ops/` + `/product/` + `/content/` + `/email/`**；共享代码在 `src/kit/`）。
- **`apps/`**：独立运行的子项目集合（各有独立 `go.mod` / 是 Python uv 项目 / Docker 容器）。**2026-11-12 git 拆分**：内部模块（`core` / `core-ui` / `auth` / `mailer` / `ai-flow` / `pycraper` / `pyreplay` / `metrics`，移除后主系统无法运行）由主仓库追踪；外部可选项目（`actives` / `c2c` / `game` / `minishop` / `social` / `dapp` / `tee-node` / `tee-node-monitor` / `web3auth` / `contract-deploy` / `pi-web`，移除后核心照常运行）各自是独立 git 仓库（remote = `github.com/computeflux/<name>.git`），主仓库不纳管、不追踪；子项目改动在各自仓库提交。
- **`deploy/`**：统一 docker-compose 部署入口（一服务一文件 + Dockerfile；Go 服务构建上下文指向 `apps/core`，前端/agent 服务用仓库根上下文）。
- **`postiz`**：docker-compose 内的开源社媒发布平台，独立子项目（暂不纳入 `apps/` 管理）。

## 架构速览
- **Go 服务**（`apps/core`）：工作流编排、CRAG 检索、REST API、Qdrant 写入/查询、审批任务（MySQL）。
- **Python Worker**：通过 MCP 从小红书 / X / 币安抓取与发布。
- **Temporal**：跨语言任务调度、重试、状态机（Web :8233）。
- **Qdrant**：RAG 向量库（UI :6333）。
- **基础设施**：`docker-compose.yml`（Temporal + Qdrant + MySQL + postiz）。
- **进化闭环（S1）**：audit 巡检 → evolve-driver（worktree + coder/reviewer Agent + 沙箱 + CI）→ 人工审批（board 审批中心）→ 灰度自动推进（5%→50%→100%）/ 恶化自动回滚；全程 audit_logs 留痕。

## 模块地图

### 工作流核心（`apps/core`，Go 主模块）
| 目录 | 说明 | 入口/端口 |
|------|------|-----------|
| `apps/core/cmd/api` | Gin REST API（`bin/api` :8001；含 S1 审批/Agent/进化单路由 + 灰度/过期 ticker） | :8001 |
| `apps/core/cmd/workflow` | Temporal Go Worker（`bin/go-worker`） | — |
| `apps/core/cmd/audit` | S1 审计巡检（30 分钟 ticker，报告 JSON + 审批单/进化单；不自动改流量） | :8120 仅内部 |
| `apps/core/cmd/evolve-driver` | S1 进化执行链（worktree→coder→范围校验→reviewer→沙箱→CI→审批单；串行） | :8121 仅内部 |
| `apps/core/cmd/sandbox-runner` | S1 沙箱执行器（安全参数写死代码；白名单镜像） | :8123 仅内部 |
| `apps/core/cmd/flex-server` | S2 柔性分支图引擎（Go 自研；节点≤20/token≤50k/超时 10min 写死） | :8124 仅内部 |
| `apps/core/cmd/watchdog` | S3 死循环检测（1 分钟 ticker，扫两 namespace 运行中 workflow） | :8125 仅内部 |
| `apps/core/cmd/skillctl`·`agentctl` | Skill / Agent 运维 CLI（tools profile，经 API 落库） | — |
| `apps/core/cmd/agent-pi`·`agent-ocr` | Agent adapter（统一 JSON 协议；coder/reviewer 的 Go 实现，源码 `internal/agentcli`；由 evolve-driver 容器内 exec） | — |
| `apps/core/cmd/inventory` | S5 软件资产画像（tools profile，扫描仓库 → `software_assets` 表，含不纳管 external 标注） | — |
| `apps/core/cmd/deployctl` | S5 部署编排器（tools profile；build/up/健康检查/自愈/回滚 + 版本容器 + auto-update） | — |
| `apps/core/cmd/health-patrol` | S5 健康巡检与故障自愈（10 分钟 ticker；动作仅 restart/rebuild+restart/skip，3 连败升级） | :8127 仅内部 |
| `apps/core/cmd/ops-report` | S6 运维情报（daily/weekly ticker；聚合 error_logs/skill_runs/skill_circuit/interventions/Temporal→LLM 日报/升级建议→`ops_reports` 表；不碰运行链路） | :8128 仅内部 |
| `apps/core/cmd/knowledge-sync` | S7 产品信息汇聚（daily 02:00 ticker；扫 `KNOWLEDGE_INTERNAL_DIR` →hash diff 增量入库 Qdrant + `knowledge_docs` 去重基线；不碰运行链路） | :8129 仅内部 |
| `apps/core/internal/*` | Go 共享库（activities/workflows/qdrant/llm/db/mailer/errorsink/skillgateway/ops/product） | — |
| `apps/core/internal/approval` | S1 审批/灰度/审计核心（六表访问 + 状态机 + 月分区清理） | — |
| `apps/core/internal/notify` | S1 告警/审批通知接口（no-op 默认；S4 接 Matrix） | — |
| `apps/core/internal/dockerapi` | S1 极简 docker Engine API 客户端（exec + 一次性容器，stdlib only） | — |
| `apps/core/internal/circuitbreaker` | S3 skill 级熔断器（5 连败→open→60s 半开→2 连成→close；持久化 skill_circuit） | — |
| `apps/core/internal/intervention` | S3 干预舱 API（takeover/inject/retry/rollback/resolve + 活跃 workflow/熔断查询） | — |
| `apps/core/internal/ops` | S6 运维情报核心（聚合 + 日报/周报/升级建议 + git 热点/TODO 扫描；LLM/Temporal 降级） | — |
| `apps/core/internal/product` | S7 产品情报核心（两表 + 入库清洗/分块/向量化 + 产品 Agent 对话/拆解校验/建卡分流；secret 脱敏红线） | — |
| `apps/core/internal/temporalscan` | S3 运行中 workflow history 扫描（事件数 + 连续失败计数） | — |
| `apps/core/internal/deploy` | S5 部署编排核心（统一 compose 解析 + 拓扑序 + docker CLI 执行 + 健康检查 + 版本容器） | — |
| `apps/core/internal/asset` | S5 资产画像（仓库扫描 → `software_assets` 表 + 纳管/不纳管红线） | — |
| `apps/core/internal/crag`·`evaluate`·`generate` | CRAG / 评估 / 生成（核心共享包，非独立子项目） | — |
| `apps/core/agents/` | Agent manifest + adapter（pi=coder 宿主 pi-web；ocr=reviewer；stdin/stdout JSON 协议；adapter 为 Go 二进制 `/bin/agent-pi`·`/bin/agent-ocr`（源码 `internal/agentcli`）） | — |
| `apps/core/hacks/` | SQL DDL（bootstrap.sh 顺序执行 sql/*.sql）+ CI/开发脚本（ci-runner.sh 等） | — |
| `apps/core/hc/` | 最小健康检查探针（独立小 Go 模块，stdlib only；Dockerfile 多阶段构建用） | — |

### 子项目（`apps/`）
| 目录 | 模块名 | 说明 | 端口 |
|------|--------|------|------|
| `apps/core` | `github.com/wetee/ai-workflow` | 工作流 Go 核心（见上表） | 各服务端口 |
| `apps/core-ui` | — (React + Vite + TS) | board 前端合并体（唯一 board 前端项目）：**单容器单端口**，路径入口 `/ops/`（运维中心：S1 审批/进化 + S3 干预 + S6 运维 + 技能 + 用户）+ `/product/`（产品与任务：默认工作看板 + 工作项中心 + 审批中心 + 文档 + 思维导图 + 项目）+ `/content/`（内容生产：MCP→RAG→发布主链路）+ `/email/`（邮件）；共享代码 `src/kit/`；nginx 同源代理 /api | :5181（`/` 302 → `/ops/`） |
| `apps/pycraper` | — (Python uv) | 抓取 worker（system=pycraper） | docker |
| `apps/pyreplay` | — (Python uv) | 执行 worker（system=pyreplay） | docker |
| `apps/contract-deploy` | `github.com/computeflux/contract-deploy` | 合约控制台 | docker :8000 |
| `apps/tee-node-monitor` | `github.com/computeflux/tee-node-monitor` | 链上监控 | docker :8002 |
| `apps/actives` | `github.com/computeflux/actives` | 链上数据 / 积分 | — |
| `apps/c2c` | `github.com/computeflux/c2c` | C2C 积分兑换平台（撮合/押注/核验/放点，不碰法币） | :8007（C2C_PORT；代码默认 8006，.env 覆盖为 8007） |
| `apps/mailer` | `github.com/computeflux/mailer` | 邮件发送 | :8003（MAILER_PORT） |
| `apps/metrics` | `github.com/computeflux/metrics`（sslmon） | 指标监控 | :8003（SSL_HTTP_PORT） |

| `apps/pi-web` | — (Node 22 + Next.js 16) | pi 编码代理 UI（本地 fork，含 web3auth 登录等定制；**宿主运行**，systemd 用户单元 `pi-web.service`，npm 全局安装） | :30141（宿主）；:8443（HTTPS，nginx-pi TLS 终结） |
| `apps/minishop` | — (React + Vite + TS) | 算力店铺 dApp（单店私域）；子服务 `services/share-server` 为 SEO / 社交分享 SSR（Bun） | :5175（app）；:3000（share-server） |

### 其他
| 目录 | 说明 |
|------|------|
| `deploy/` | 统一 docker-compose 部署入口（一服务一文件，含 Dockerfile 与 tools profile；k8s-ready） | `deploy/docker-compose.yml` |
| `docs/` | 文档（2026-10-19 归类：`docs/README.md` 总索引 + `strategy/`（业务/产品/Agent/OS/营销 Bible）+ `plan/`（S0–S9 + evolve-plan/transform-plan/自进化设计）+ `marketing/`（营销方案）+ `ppt/`（演示 deck）；产品看板「文档」页可树形浏览/编辑） | — |
| `postiz` | 统一 compose 内的开源社媒发布平台（第三方，不纳管） | — |

## 全局约定
- **`apps/` = 独立子项目**：每个都有独立边界（独立 `go.mod` / Python uv 项目 / Docker 容器），彼此不 import。`apps/core` 是 Go 主模块（`github.com/wetee/ai-workflow`），`internal/crag|evaluate|generate` 是核心共享包（被 `internal/activities`、`cmd/api` 引用）。
- **`apps/core-ui`** 是**唯一 board 前端项目**（React/TS **标准单入口**：一个 `index.html` + `src/main.tsx` 顶层路由，四板块 `/ops` `/product` `/content` `/email` 路由区分，`React.lazy` 路由级代码分割；单容器单端口 :5181；共享代码 `src/kit/` 原 board-kit；原 board-content/board-email 已并入 src/content、src/email）。改 kit 或页面后跑 `npm run lint` + `npm run build`，部署 `docker compose up -d --build core-ui`。
- `apps/*` 下的 Go 模块命名已统一为 `github.com/computeflux/<name>`（`apps/core` 例外：`github.com/wetee/ai-workflow`），不随彼此 `go.sum` 同步。
- ⚠️ **禁止拷贝第三方源码**（2026-11-03 立的红线）：需要某个开源项目的能力时，**只允许**这三种方式：
  1. **作为依赖引入**（Go `require` / npm `dependencies` / Python `pyproject`），进程内 import 调用；
  2. 作为**独立服务/镜像**部署（docker compose 引一个 image），通过 HTTP/MCP/RPC 调用；
  3. 只用协议（如 MCP client），自己实现客户端。
  **绝不允许**把上游仓库的 `*.go` / `*.py` / `*.ts` 源码复制进 `apps/`（无法升级、无法审查、还会把上游示例与登录态文件一起带进来）。
  反例：`apps/social` 曾把 `xpzouying/xiaohongshu-mcp` 全仓 13k 行拷进来（已清除，改为 `require ... v1.2.9` + import 子包）。
- `apps/pycraper`、`apps/pyreplay` 是 Python（uv）；`apps/pi-web` 是 Node/Next.js 本地 fork（依赖 `apps/web3auth` 的 file: 包，改它跑 `npm run lint` / `npm test` / `npm run build`，勿在 dev 期间 `next build`）。
- ⚠️ **端口冲突**：`apps/mailer` 与 `apps/metrics` 默认都用 `8003`；board 的邮件 API 默认也指向 `8003`。部署时用 `MAILER_PORT` / `SSL_HTTP_PORT` 区分。
- 错误统一写入 MySQL `error_logs` 表（Go 侧 GORM AutoMigrate，Python worker 有 `CREATE TABLE IF NOT EXISTS` 兜底）。
- 各服务启动自动加载根 `.env`（容器内挂载到 /app/.env，godotenv 读取）。
- 改任何模块后，优先跑该模块自己的 lint / build / test，不要破坏跨模块的 MCP / Temporal activity 接口签名。Go 核心：`cd apps/core && go build ./... && go test ./...`。
- ⚠️ **audit_logs 只增不删**（S1）：月分区 + 每日只 DROP 过期分区（保留 180 天）；**无任何删除 API**，MySQL 应用账号（`workflow`）对其仅 SELECT + INSERT。
- ⚠️ **进化链红线**（S1）：diff 绝不 push/merge 主分支（审批通过后自动推 `evolve/<task_no>` **feature 分支** + 建 GitHub PR，由人工合并）；coder 写范围校验：默认仅限 `apps/*`，**未显式选中时保护 `apps/board*` 与 `apps/core*`**（避免误改核心框架；任务显式选中 `core`/`core-ui` 为 target 时照走自动链——core = Go 核心、core-ui = board 前端，不再有“只走人工 PR”的默认红线，人工在审批 + 合并把关），**小范围越界允许**（越界文件 ≤ `EVOLVE_SCOPE_ALLOW_FILES` 且改动行 ≤ `EVOLVE_SCOPE_ALLOW_LINES`，默认 2 文件/50 行，audit `scope_allow` 留痕），超阈值打回人工 PR；上线必须人工审批（24h 超时 expired，**无默认通过**）。
- ⚠️ **`apps/minishop/services/share-server`** 是独立的 **Bun** 服务（SEO / 社交分享 SSR，非主项目 import）。读链走 TEE `contractDryRun`，产出 `/share/{id}.html`（SEO+meta+JSON-LD）、`/share/{id}.svg`（分享卡）、`/sitemap.xml`。CDN 回源到它，前端 app（:5175）独立托管。改它只跑 `bun run test:*` + `bun build --target bun`；ABI 变更需重拷 `public/contract/gateway.json` 到其 `data/` 再重建镜像。详见 `apps/minishop/AGENTS.md` 与 `apps/minishop/services/share-server/DEPLOYMENT.md`。

## 本地启动（完整流程见根 README）
```bash
cd /home/wetee/work/cathome/workflow
cp .env.example .env            # 先填环境变量（运行时配置全走根 .env）
cd deploy
docker compose up -d --build    # 全系统（基础设施+核心+技能，见下）
docker compose ps               # 全部 healthy 即就绪
```

## 服务重启 / 部署方式

**部署方式唯一：统一 docker-compose**（`deploy/docker-compose.yml`，一服务一文件，用法见 `deploy/README.md`；设计与 k8s 映射见 `docs/plan/evolve-plan.md` §3.5）。systemd 用户单元与零散 `docker run` 已退役（S0）。**唯一例外：测试环境 / 灰度版本容器**由 `deployctl version up/down` 动态管理，不进静态 compose。

### 常用命令
| 场景 | 命令 |
|------|------|
| 全量启动 / 恢复 | `cd deploy && docker compose up -d --build` |
| 全量停止 | `cd deploy && docker compose down` |
| 重启某服务 | `cd deploy && docker compose restart <svc>` |
| 改 Go 核心代码后 | `cd deploy && docker compose up -d --build <svc>`（必须**重建镜像**才生效，`restart` 不加载新代码；构建上下文已是 `apps/core`） |
| 改 core-ui 前端代码后 | `cd deploy && docker compose up -d --build core-ui`（唯一 board 前端服务，一次重建覆盖全部四板块。⚠️ 仅镜像更新时 `up -d` 不重建运行中容器，需加 `--force-recreate`） |
| 本地 Go 构建/测试 | `cd apps/core && go build ./... && go test ./...` |
| 本地前端开发 | `cd apps/core-ui && npm run dev`（:5181，单 dev server 四板块 /ops /product /content /email） |
| 一次性工具 | `cd deploy && docker compose --profile tools run <tool> ...`（deployctl / skillctl / agentctl / inventory / ci-runner） |
| S1 进化链常用 | `--profile tools run --rm -T agentctl register /repo/apps/core/agents/pi/agent.json` → `bind coder pi` / `bind reviewer ocr`；`--profile tools run --rm -T ci-runner <task_no>` 手工跑 CI |
| 数据库 DDL | `bash apps/core/hacks/bootstrap.sh`（顺序执行 `apps/core/hacks/sql/*.sql`，含 S1 审批/审计/Agent/进化表） |
| 测试环境 / 灰度版本容器 | `deployctl version up <skill> <ver> --env test\|gray` / `deployctl version down <skill> <ver>`（S5 前灰度回滚停版本容器为人工操作） |
| 查状态 | `docker compose ps` / `deployctl status` / `docker logs <svc>` |

### 不纳入统一 compose（不纳管清单）
业务 dApp 维持各自现有部署：`contract-deploy`（:8000，`cd apps/contract-deploy && ./docker.sh rebuild`）、`tee-node-monitor`（:8002，`cd apps/tee-node-monitor && docker compose up -d --build`）、minishop / c2c（:8007，ego TEE 进程）/ actives 等。`auth` 已容器化（`deploy/compose/core/auth.yml`，:8008 对外）；宿主仅保留 `workflow-c2c.service`（TEE 进程）。

### ⚠️ 易踩坑
- 改代码后必须**重建镜像**：`docker compose up -d --build <svc>`；`docker compose restart` 只重启旧镜像。
- 宿主 systemd 用户单元：`pi-web.service`（pi-web 编码代理 UI，:30141，`apps/core/agents/pi/pi-web.sh` 管理）；`workflow-c2c.service`（TEE 进程）。其余工作流服务全部容器化。
- 端口最小化暴露：对外仅 board :5181（core-ui 单容器四路径入口 /ops + /product + /content + /email，nginx 同源代理 `/api`、`/mailer`、`/jaeger`；SSO 登录枢纽 = :5181/ops/login）/ 8008（auth，浏览器 web3auth SDK 直连）/ 8443（pi-web，HTTPS：nginx-pi TLS 终结 + DigiCert 证书 xiaobai.asyou.me，远程使用 + Basic Auth）；8001 / 8003 / 3306 / 7233 / 8233 / 6333 / 6334 / 4007 仅绑 127.0.0.1；技能 / otel / jaeger / synapse / S1 进化服务（sandbox-runner :8123 / audit :8120 / evolve-driver :8121 / dind 2375）/ S2 flex-server :8124 / S3 watchdog :8125 / S5 health-patrol :8127 端口仅内部网络。
- k3s/k8s 接入预留：一服务一文件、全配置走 .env、healthcheck + 限额 + 标签，映射表见 `docs/plan/evolve-plan.md` §3.5。


# Tool Calling Rules
- NEVER output `<｜DSML｜ calls>`, `<｜DSML｜ invoke>`, XML tags, or any tool call syntax inside your `<think>` reasoning block.
- Complete your entire reasoning process and close the `</think>` tag BEFORE invoking any tools.
- Use ONLY standard structured tool calls provided by the system schema.
