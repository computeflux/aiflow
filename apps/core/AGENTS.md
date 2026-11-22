# apps/core — 工作流 Go 核心

Go 主模块 `github.com/wetee/ai-workflow`（2026-10-10 由仓库根整体迁入本子项目）。Temporal 工作流编排 + REST API + CRAG 检索 + Qdrant + 审批/进化闭环（S1–S9）的全部 Go 代码。

## 目录结构

```
apps/core/
├── cmd/            # 入口（一服务一目录，见根 AGENTS.md 模块地图）
│   ├── api/        # Gin REST API（:8001）
│   ├── workflow/   # Temporal Go Worker
│   ├── audit/ evolve-driver/ sandbox-runner/  # S1 进化闭环
│   ├── flex-server/ watchdog/ matrix-bot/     # S2/S3/S4
│   ├── health-patrol/ inventory/ deployctl/   # S5
│   ├── ops-report/ knowledge-sync/ work-dispatch/  # S6/S7/S9
│   └── skillctl/ agentctl/ generate-skill/ evaluate-skill/ search-skill/  # 技能/运维 CLI
├── internal/       # Go 共享库（2026-11-18 结构治理后共 22 包，按域归并）：
│   │               #   核心链路 activities/ workflows/ crag/ llm/ qdrant/ mailer/
│   │               #   基础设施 db/ config/ authn/ otel/（含 error_logs 写入，原 errorsink）
│   │               #   S1-S9 域 approval/ circuitbreaker/ intervention/ skillgateway/ ops/ product/ project/ workitem/ marketing/ flex/
│   │               #   部署运维 deploy/（含原 asset 资产画像 + serviceops 运行服务） dockerapi/ docstore/ temporalscan/
│   │               #   已合并的小包：textutil→activities，sandboximg→cmd 两侧，generate/evaluate→cmd+activities 内联，
│   │               #   errorsink→otel，worktree/gateway→唯一消费者，asset/serviceops→deploy
├── agents/         # Agent manifest + adapter（pi/ocr/analyst=Go 二进制 /bin/agent-*，源码 internal/agentcli）
├── hacks/          # SQL DDL（bootstrap.sh + sql/*.sql）+ CI/开发脚本（ci-runner.sh 等）
├── hc/             # 最小健康检查探针（独立小 Go 模块，stdlib only，自带 go.mod）
├── go.mod / go.sum
└── .dockerignore   # Go 镜像构建上下文裁剪（context = 本目录）
```

## 常用命令

```bash
cd apps/core
go build ./...        # 全量编译（契约）
go test ./...         # 全量测试
go build -o bin/api ./cmd/api   # 本地单服务编译（bin/ 在仓库根 gitignore）
bash hacks/bootstrap.sh         # 数据库 DDL（顺序执行 hacks/sql/*.sql）
```

部署走统一 compose（`deploy/` 目录）：`docker compose up -d --build <svc>`；Go 服务构建上下文已是本目录（`deploy/Dockerfile.go` 多 target 一镜像一入口）。

## 脚本

`hacks/` 是开发/运维脚本，`agents/` 下是 Agent adapter 管理脚本。下表命令均在 `apps/core` 下执行（`cd apps/core`），路径相对本子项目根。

| 脚本 | 用途 | 用法 |
|------|------|------|
| `hacks/build.sh` | 编译主模块两个二进制到 `bin/`：`bin/api`（`cmd/api`）、`bin/go-worker`（`cmd/workflow`）。只覆盖 `apps/core`，其他子项目各有自己的构建 | `bash hacks/build.sh` |
| `hacks/bootstrap.sh` | ⚠️ 数据库 DDL：顺序执行 `hacks/sql/*.sql`（建表/补列/月分区/`error_logs` 补列），最后以 root 单独执行 `grants.sql` 做 `audit_logs` 权限收口。幂等（只有 `DROP PROCEDURE`，不删数据），但会真实改库结构与授权 | `bash hacks/bootstrap.sh` |
| `hacks/ci-runner.sh` | S1 全量 CI：`go build` + `go test ./cmd/... ./internal/...`，再对 `apps/*` 每个 Python 项目跑 `pytest`；JUnit XML 写到 `$CI_OUT_DIR`，stdout 末行输出 JSON 摘要，退出码即失败标记。面向 CI 容器，不建议在宿主直接跑 | `bash hacks/ci-runner.sh <task_no>`；容器内标准用法：`cd deploy && docker compose --profile tools run ci-runner <task_no>` |
| `hacks/doccheck.sh` | 导出符号 doc comment 检查（Google Go style「Comments」：导出 func/type/const/var 必须有以符号名开头的 doc comment；结构体字段用行内注释不计入）；退出码非 0 = 有缺失。2026-11-18 结构治理时全量补齐（cmd+internal 共 116 处） | `bash hacks/doccheck.sh [目录...]`（默认本项目的 cmd + internal） |
| `hacks/auto-update.sh` | ⚠️ 无人值守自动部署：查 GitHub 最新 commit → `git pull --ff-only origin main` → `docker-workers.sh rebuild`（重建镜像并重启 pycraper/pyreplay 容器）。会直接把新代码上线 | `bash hacks/auto-update.sh`（无参数；正常由 systemd timer 触发） |
| `hacks/docker-workers.sh` | ⚠️ 已退役（S0）：用 `docker run` 起/删 pycraper、pyreplay 容器。`start`/`rebuild` 会 `rm -f` 与统一 compose 同名的容器，直接和生产部署抢容器 | `bash hacks/docker-workers.sh {start\|stop\|rebuild\|status}` |
| `hacks/py-worker.sh` | 已退役脚本的兼容入口，参数原样转发给 `docker-workers.sh`（`restart`→`start`，`disable`→`stop`） | `bash hacks/py-worker.sh {start\|restart\|stop\|disable\|status\|rebuild}` |
| `hacks/go-worker.sh` | 编译 `bin/go-worker` 后操作 systemd 用户单元 `workflow-go-worker`。该单元已退役，当前宿主上不存在 | `bash hacks/go-worker.sh {start\|restart\|stop\|status\|disable}` |
| `hacks/api.sh` | 编译 `bin/api` 后操作 systemd 用户单元 `workflow-api`。该单元已退役，当前宿主上不存在 | `bash hacks/api.sh {start\|restart\|stop\|status\|disable}` |
| `hacks/log.sh` | 跟日志：py/go/api 走 `journalctl -fu <unit>`，board 走 `tail -f /tmp/workflow-board.log`，`all` 打印三者最近若干行 | `bash hacks/log.sh {py\|go\|api\|board\|all} [lines]`（默认 50 行） |
| `hacks/matrix-setup.sh` | S4 Matrix 供给（幂等）：注册 `@approval-bot` / `@audit-bot`、建 3 个房间 `#ops-alerts` `#approvals` `#agent-log`、每房间发一条测试消息，最后打印 `MATRIX_BOT_TOKEN=` 供回填根 `.env`。会在 Matrix 上真实建用户/房间/发消息 | `bash hacks/matrix-setup.sh`（无参数，工作目录任意） |
| `agents/pi/pi-web.sh` | 管理宿主运行的 fork 版 pi-web（`apps/pi-web`）：`install`=构建 + `npm install -g .` 覆盖全局 pi-web + 写默认工作目录 + `daemon-reload`；其余子命令操作 systemd 用户单元 `pi-web.service` | `bash agents/pi/pi-web.sh {install\|build\|env\|start\|stop\|restart\|reload\|enable\|disable\|status}` |

要点：

- **环境变量**：`bootstrap.sh` 读 `MYSQL_USER` / `MYSQL_PASSWORD` / `MYSQL_DATABASE` / `MYSQL_ROOT_PASSWORD`（环境变量优先，其次根 `.env`，缺省 `workflow` / `workflow123` / `workflow` / `workflow123`；`.env` 用 grep 取值，不 `source`）。`ci-runner.sh` 必传 `<task_no>`，另有 `CI_REPO_DIR`（默认 `/work`）、`CI_OUT_DIR`（默认 `/cache/ci`）。`pi-web.sh` 用 `PI_WEB_DEFAULT_CWD`（默认仓库根）。`matrix-setup.sh` 读 `deploy/.env` 的 `SYNAPSE_REGISTRATION_SHARED_SECRET`，根 `.env` 有 `MATRIX_BOT_TOKEN` 时直接复用。`auto-update.sh` 把上次 SHA 写 `/tmp/computeflux-workflow-last-sha`。
- **依赖/前置**：`bootstrap.sh` 要求 `deploy/` compose 的 `mysql` 容器在跑（`docker compose exec` + 容器内 `mysql` CLI）。`ci-runner.sh` 需要 `go`、`gotestsum`、`python3`；Python 项目需有 `.venv/python/bin/python`（evolve-driver 负责拷进 worktree），`/work` 只读故 `go build` 用 `-buildvcs=false`。`matrix-setup.sh` 需要 `synapse` 容器在跑（所有 curl 走 `docker exec synapse`，8008 不发布宿主）、`openssl`、`python3`。`docker-workers.sh` / `py-worker.sh` 需要镜像 `pycraper:latest` / `pyreplay:latest`、网络 `temporal-network` + `workflow_default`、`apps/pycraper/.env` 与 `apps/pyreplay/.env`。
- **常见坑（路径按重组前布局写死，脚本尚未修）**：`bootstrap.sh`、`auto-update.sh`、`docker-workers.sh` 都用 `hacks/../..` 当仓库根，实际解析到 `apps/`（少一级）。后果各不相同：`bootstrap.sh` 读不到根 `.env`（回退默认凭据）且 `cd apps/deploy` 找不到 compose 编排，`docker compose exec` 直接失败；`auto-update.sh` `git pull` 仍能成功（git 会向上找到仓库根），但接着调用的 `apps/hacks/docker-workers.sh` 不存在，重建容器失败；`docker-workers.sh` 的 `-v $REPO_DIR/apps/pycraper/config` 变成 `apps/apps/pycraper/config`，挂载路径不存在。`matrix-setup.sh` 用 `cd hacks/..` 落到 `apps/core/`，读 `deploy/.env` 报「缺少 SYNAPSE_REGISTRATION_SHARED_SECRET」后退出。`pi-web.sh` 的仓库根（上溯 4 级）是对的，但 `ENV_FILE` 拼成 `<repo>/agents/pi/config/env`，而 `pi-web.service` 实际 `EnvironmentFile` 是 `apps/core/agents/pi/config/env`——`env`/`install`/`restart` 写的 `PI_WEB_DEFAULT_CWD` 不会被单元读到。
- **systemd 已退役**：当前宿主只有 `pi-web.service`、`workflow-c2c.service`；`workflow-api` / `workflow-go-worker` / `workflow-py-worker` 都不存在，`api.sh` / `go-worker.sh` 的 `systemctl --user` 会失败，`log.sh` 的 `py|go|api` 分支无输出。重启服务用 `cd deploy && docker compose up -d --build <svc>`，看日志用 `docker logs`。
- **`build.sh` 只编 `api` 与 `go-worker`**，其他入口（audit / evolve-driver / sandbox-runner / 各 CLI）请用 `go build ./...` 或 compose 镜像。
- **`bootstrap.sh` 细节**：`grants.sql` 以 root 执行；容器内 `mysql` CLI 在 locale=C 时默认 latin1，脚本已显式 `--default-character-set=utf8mb4`（手工执行时别漏，否则中文双编码乱码）。另外 MySQL 9.x 下表级 `GRANT` 无法减权，`grants.sql` 只是意图声明，`audit_logs` 只增不删真正靠应用层无删除入口 + 只 DROP 过期分区。
- **`matrix-setup.sh` 细节**：bot 密码写死为 dev 密码 `matrix-bot-s4-2026`，真正凭据是 access token；全新环境（清空 `synapse-data` 卷）后重跑脚本，把打印的 `MATRIX_BOT_TOKEN` 回填根 `.env`。

## 约定

- 独立 Go 模块，**不随其他 apps 的 go.sum 同步**；模块名保持 `github.com/wetee/ai-workflow`（历史原因，不改）。
- 错误统一落 MySQL `error_logs`（`system=<服务名>`）。
- 启动自动加载根 `.env`（容器挂载 /app/.env；`internal/config` godotenv 读取）。
- **进化链红线**：本子目录（`apps/core/*`）在 coder 写范围之外——核心代码只走人工 PR（见根 AGENTS.md 进化链红线）。
- 改 `internal/*` 共享包后必须 `go build ./...` + 相关包测试，不要破坏跨模块的 MCP / Temporal activity 接口签名。
- 容器内仓库挂载点：evolve-driver /repo（可写）、ci-runner /work（只读 worktree）、ocr /work（只读）——adapter 的 workdir 由 JSON 协议传入，勿硬编码。pi-web 已改宿主运行（systemd 用户单元），不再容器化。
