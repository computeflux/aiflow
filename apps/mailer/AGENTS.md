# mailer（邮件发送）

独立 Go 模块（`github.com/computeflux/mailer`），邮件批量发送服务。作为 Temporal worker 执行邮件工作流，同时提供 REST API（gin）。

## 技术栈
- **Go**（独立 `go.mod`，模块名 `github.com/computeflux/mailer`）
- **Gin** REST API + **Temporal** SDK worker
- **GORM** 模型（MySQL）
- 错误落库：`errorsink`（写入主项目 `error_logs` 表，`system=mailer`）

## 目录结构
- `main.go` — 入口（SMTP 默认发信人自动初始化 + gin API + Temporal client）
- `config/` — 配置（`MAILER_PORT`、`MYSQL_PORT`、`TEMPORAL_HOST_PORT`、`SMTP_PORT` 等环境变量）
- `mailer/` — 邮件发送逻辑（`mailer.go`）
- `models/` — 数据模型（发信人 / 邮件批次 / 收件人 / 模板）
- `errorsink/` — 统一错误落库
- `Dockerfile` / `docker.sh` — 容器（镜像名 `workflow-mailer:latest`，容器名 `workflow-mailer`）
- `run.sh` — 运行脚本

## 端口
- **默认 :8003**（环境变量 `MAILER_PORT`，默认 `8003`）。
- ⚠️ **与 `apps/metrics`（sslmon）默认端口冲突**（两者默认都是 8003）。部署时务必用 `MAILER_PORT` 或 metrics 的 `SSL_HTTP_PORT` / `-http-port` 区分。board 的 `getMailerApiBase()` 默认指向 `:8003`（邮件 API）。

## 常用命令
```bash
cd apps/mailer
go build ./...          # 编译
go run .                # 本地运行（默认 :8003）
bash docker.sh          # 构建/运行容器
```

## 脚本
以下命令均在项目根目录 `apps/mailer` 下执行；表中路径相对项目根。

| 脚本 | 用途 | 用法 |
|------|------|------|
| `docker.sh` | 构建 `workflow-mailer:latest`，删除同名旧容器后以 `--network workflow-net`、`--env-file ../../.env` 后台启动，宿主机仅绑 `127.0.0.1:${MAILER_PORT:-8003}`。⚠️ 会 `docker rm -f workflow-mailer` 删除旧容器（业务数据在 MySQL，不在容器内） | `bash docker.sh`<br>`MAILER_PORT=8013 bash docker.sh` |
| `run.sh` | 本地 `go build -o bin/mailer .` 后前台运行（默认 :8003） | `bash run.sh`<br>`MAILER_PORT=8013 bash run.sh` |

要点：
- `docker.sh` 需要 Docker 权限与仓库根 `.env`；`workflow-net` 不存在时会自动创建。容器内端口由 `--env-file ../../.env` 中的 `MAILER_PORT` 决定（未设置则默认 8003），宿主机映射用脚本读到的 `MAILER_PORT`；只在 shell 里 `export MAILER_PORT=8013` 而根 `.env` 未改，会出现「映射 8013→8013、容器仍监听 8003」的错位。
- `run.sh` 需要 `go` 在 `PATH`；配置由 `config.Load` 依次读 `.env`、`../.env`、`../../.env`（仓库根 `.env`），并依赖 MySQL、Temporal（`TEMPORAL_HOST_PORT`）与 SMTP 配置。
- 两者都不经 `deploy/` 统一 compose；正式部署以 `deploy/docker-compose.yml` 为准，`docker.sh` 是项目内单独跑容器的方式。
- ⚠️ 默认端口 `8003` 与 `apps/metrics` 冲突，部署时用 `MAILER_PORT` / `SSL_HTTP_PORT` 区分。

## 约定
- 遵循仓库根级约定（见上级 `AGENTS.md`）。
- 独立模块，不随主仓库 go.sum 同步。
- 改动后跑 `go vet` / `go build ./...`。
