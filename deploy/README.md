# 部署：统一 docker-compose（S0 交付）

工作流系统的全部服务（基础设施 + 核心 + 技能 + 工具）收敛到本目录的**一套 docker-compose**。
systemd 用户单元与零散 `docker run` 已退役（`hacks/systemd/`、`hacks/install.sh`、根 `docker-compose.yml`、`hacks/otel.sh` 均已删除，git 历史可恢复）。

**唯一例外**：测试环境 / 灰度版本容器由 `deployctl version up/down` 动态管理（S5），不进静态 compose。

## 目录结构（一服务一文件）

```
deploy/
├── docker-compose.yml          # 入口：include: compose/ 下所有文件 + 单一网络 workflow-net
├── .env                        # 本地 compose 级变量（gitignore）
├── .env.example                # compose 级变量示例
├── Dockerfile.go               # golang:1.26 → distroless；ARG target 选入口
│                               # （api / go-worker / 三技能 / 工具 共用；构建上下文 = apps/core）
├── Dockerfile.core-ui          # node:22 构建（vite）+ nginx 同源代理托管（构建上下文 = 仓库根；
│                               # 单入口 dist/ → 四板块 /ops /product /content /email）
├── Dockerfile.auth             # golang:1.26 → distroless（独立 go.mod 模块 apps/auth）
├── Dockerfile.deploy           # golang:1.26 → docker:27-cli（docker CLI + compose 插件 + shell）；
│                               # S5 deployctl / health-patrol 经挂载 docker.sock 驱动宿主 daemon
├── nginx/core-ui.conf          # core-ui 单容器（:5181）：/ 302 → /ops/，单入口 SPA 四板块 /ops /product /content /email +
│                               # 同源代理 /api/ /mailer/ /jaeger/
├── nginx/upstreams.conf        # board 上游 + zone + resolve 动态解析（Docker DNS 127.0.0.11）
├── config/
│   ├── otel/otel-collector.yaml
│   ├── temporal/               # docker.yaml + 幂等 bootstrap 脚本
│   └── nginx-pi/              # pi-web TLS 终结：nginx.conf（证书 bind mount token 节点 DigiCert）
└── compose/
    ├── infra/                  # temporal 栈（pg/server/ui）/ mysql / qdrant / postiz /
    │                           # otel-collector / jaeger / synapse
    ├── core/                   # auth / workflow-api / workflow-go-worker / core-ui(:5181 单容器单入口 SPA
    │                           # 四路径入口 /ops + /product + /content + /email) / mailer /
    │                           # nginx-pi（pi-web TLS 终结，upstream → 宿主 :30141）
    ├── skills/                 # generate-skill(:9501) / evaluate-skill(:9502) /
    │                           # search-skill(:9503)
    ├── evolve/                 # （S1–S7 逐步加入；S5 health-patrol 健康巡检 :8127 仅内部）
    └── tools/                  # skillctl / agentctl / inventory / deployctl（profiles: [tools]，默认不启动）
```

注意：`include` 进来的文件里，**相对路径以该文件所在目录为基准**（`../../../` = 仓库根，`../../../apps/core` = Go 核心模块，`../../` = `deploy/`）。

2026-10-10 仓库重组：Go 核心（cmd/internal/agents/hacks/hc）迁入 `apps/core`，board-ops/board-product/board-kit 合并为 `apps/core-ui`；Go 服务构建上下文改为 `apps/core`，hc 探针在 `apps/core/hc/`。

## 用法

所有命令在 `deploy/` 目录下执行：

```bash
cp .env.example .env            # 首次：填写 compose 级变量
docker compose up -d --build    # 全量启动 / 重建（一条命令恢复全系统）
docker compose ps               # 状态：全部 healthy 即就绪
docker compose down             # 全量停止

docker compose up -d --build <svc>        # 改代码后必须【重建镜像】才生效
docker compose --profile tools run skillctl list   # 一次性工具（默认不启动）
docker logs <svc>                               # 日志
```

⚠️ `docker compose restart <svc>` 只重启旧镜像，不加载新代码；改代码后一律 `up -d --build <svc>`。

⚠️ **改根 `.env` 后，已创建的容器不会生效**：compose 的 `env_file` 在**容器创建时**固化（`restart` 也不重读）。
必须 `docker compose up -d <svc>`（或全量 `docker compose up -d`）让 compose 按配置漂移自动重建；
2026-09-30 事故：`.env` 换了 LLM 网关后 audit/api 等仍是旧值（audit LLM 摘要 404 降级）。
可自查容器内现值（示例）：`docker inspect <svc> -f '{{range .Config.Env}}{{println .}}{{end}}' | grep '^LLM_MODEL='`。

⚠️ 上游换 IP 不再需要重启 board / nginx-pi：两者均用 `resolver 127.0.0.11`（Docker 内嵌 DNS）
加 `upstream` 带 `zone` + `resolve` 动态解析（见 `nginx/upstreams.conf`、`config/nginx-pi/nginx.conf`），
上游容器重建/重启换 IP 后 TTL（10s）内自动跟随。2026-09-30 前的老配置只在启动时解析一次，
上游重建即 502（当时靠 `docker compose restart board` 手动恢复）。
背景：upstream 的 `resolve` 参数需 nginx ≥ 1.27.3（board 镜像 stable-alpine=1.30.x、nginx-pi=1.27.x 均满足）。

## 配置

- **运行时配置（Go / Python / 前端服务）全部走仓库根 `.env`**（各 compose 文件 `env_file` 引用 + 容器内挂载），端点一律容器名 DNS：
  - `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317`
  - `TEMPORAL_HOST_PORT=temporal:7233`
  - `MYSQL_HOST=workflow-mysql` / `QDRANT_HOST=qdrant`
  - `SKILL_GATEWAY_URL=http://workflow-api:8001`（go-worker 的 InvokeSkill 经容器内网回 api）
  - `AUTH_URL=http://auth:8008`（auth 已容器化，容器 DNS）
  - `PICOCLAW_APPROVAL_SECRET=<openssl rand -hex 32>`（**必配**）：board AI 助手写操作两阶段确认
    （preview→confirm）的确认令牌 HMAC 密钥（`apps/ai-flow/pkg/workflow/approval.go`）。缺失时写工具
    fail-closed 全部拒绝（报错「两阶段确认未配置（缺少 PICOCLAW_APPROVAL_SECRET）」）。仅 ai-flow 一个进程
    内签/验，无需同步给其他服务；轮换只会让已签发未使用的 5 分钟令牌失效。
- **compose 级变量**（SYNAPSE_* 等）在 `deploy/.env`。
- **MySQL 字符集**：服务端 `--character-set-server=utf8mb4`（infra/mysql.yml command），客户端由 `config/mysql/utf8mb4.cnf`
  （挂到 `/etc/mysql/conf.d/`）默认 utf8mb4。
  ⚠️ 手工 SQL 必须走容器内客户端（默认已 utf8mb4）；如果自备客户端，务必 `--default-character-set=utf8mb4`——
  否则中文会**双编码损坏**（2026-09-30 `evolve_tasks` 曾中招；该损坏经 latin1 客户端读回来“看似正常”，极隐蔽）。
- 容器间通信一律容器名 DNS，不用 127.0.0.1、不用 /etc/hosts hack。

## 端口（最小化暴露，总表见 `docs/plan/evolve-plan.md` §3.1）

| 暴露 | 端口 | 说明 |
|------|------|------|
| 对外 | 5181 | **board 前端是唯一对外主入口**（core-ui 单容器 :5181 单入口 SPA：`/ops/` 运维中心 + `/product/` 产品与任务 + `/content/` 内容生产 + `/email/` 邮件，`/` 302 → `/ops/`，SSO 登录枢纽 `/ops/login`；nginx 同源代理 `/api/`→workflow-api:8001、`/mailer/`→mailer:8003、`/jaeger/`→jaeger:16686） |
| 对外 | 8008 | auth（浏览器 web3auth SDK 跨源直连，auth 侧已开 CORS） |
| 对外 | 8443 | pi-web（**HTTPS**，nginx-pi TLS 终结；证书 = token 节点 DigiCert DV `xiaobai.asyou.me`/`www.xiaobai.asyou.me`，bind mount 自 `/srv/token_node0/token_chain_data/ssl/`，到期 2026-10-26，续期替换文件即可；访问 `https://xiaobai.asyou.me:8443/`；PI_WEB_PASSWORD Basic Auth 纵深防御，用户名固定 `pi`；⚠️ nginx 的 Host 头必须用 `$http_host` 保留端口，否则 pi-web 同源自检 403；upstream → 宿主 pi-web :30141（systemd 用户单元 `pi-web.service`）；偏离 S0 端口最小化的例外） |
| 127.0.0.1 | 8001 / 8003 / 3306 / 7233 / 5432 | api / mailer / mysql / temporal / temporal-pg |
| 127.0.0.1 | 8233 / 6333 / 6334 / 4007 | temporal-ui / qdrant（UI + gRPC） / postiz（第三方，调试用） |
| 仅内部网络 | 4317 / 4318 / 16686 / 9501–9503 / 8008 | otel / jaeger / 三技能 / synapse（8008 容器内网；宿主 8008 已让给 auth 容器发布；S4 Matrix 审批 Bot `matrix-bot` :8126 健康探针同属内部网络） |
| 仅内部网络 | 8127 | S5 健康巡检 `health-patrol` 健康探针（不发布宿主端口） |

## 数据（named volume，两侧共享，切换部署方式不丢数据）

`workflow_mysql-data` / `workflow_qdrant-data` / `workflow_temporal-postgres-data` / `workflow_postiz-*` /
`workflow_jaeger-data` / `workflow_synapse-data`。

> pi-web 已改宿主运行（systemd 用户单元 `pi-web.service`，:30141），会话/模型凭据在宿主 `~/.pi/agent`，
> 不再使用 `workflow_pi-agent-data` volume。仓库根即宿主路径，`PI_WEB_DEFAULT_CWD=/home/wetee/work/cathome/workflow`。

## 链路追踪

otel-collector（OTLP :4317）→ jaeger；采样：错误 100% / 正常 10%（`OTEL_SAMPLING_RATIO` + ParentBased 配置），保留 7 天。
完整链路：board → `/api/` 代理 → workflow-api（Gin span）→ Temporal（StartWorkflow span 随 header 传递）→ go-worker（RunWorkflow/Activity span）→ skill 服务 → 回 api 的 skillgateway span，全部同一 trace。
board 的 Tracing 页（`/jaeger/`）可直接查完整链路。

> 踩坑记录：gin v1.12 起 `*gin.Context` 的 `Value()/Deadline()/...` 不再默认委托给请求 context，
> 需 `r.ContextWithFallback = true`（`apps/core/cmd/api/main.go`），否则请求里的 span 对 temporal client 不可见，链路断开。

## k3s/k8s 映射

一服务一文件 → 一文件一个 Deployment，逐文件平移；全配置走 .env → ConfigMap/Secret；
healthcheck → probe；`mem_limit`/`cpus` → resources；标签 `part-of: compute-flux-workflow` → label。
映射表见 `docs/plan/evolve-plan.md` §3.5。

## 不纳入本 compose（各自现有部署）

`contract-deploy`（:8000）/ `tee-node-monitor`（:8002）/ `minishop` / `c2c`（:8007，ego TEE 进程）/ `actives` 等业务 dApp；
`computeflux-auto-update`（S5 改为 `deployctl auto-update` 子命令，已停用不常驻）。

> 宿主机的 systemd 用户单元已清空：auth 已容器化（2026-09-29），过时的
> `contract-api.service` / `tee-node-monitor.service` 及其 target 已删除（dApp 各自 docker 部署）；
> 宿主机仅保留 `workflow-c2c.service`（ego TEE 进程，无法容器化）。

## 回滚

从 git 恢复 `hacks/systemd/` + `hacks/install.sh` + 根 `docker-compose.yml` + `hacks/otel.sh`，
重新 `systemctl --user enable --now workflow.target` + 根 `docker compose up -d`；
业务数据在 MySQL/Qdrant 的 named volume 中两侧共享，不受切换影响。
