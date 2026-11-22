# metrics（sslmon — 项目指标监控）

独立 Go 模块（`github.com/computeflux/metrics`），项目级指标监控服务，代号 `sslmon`。监控服务健康、TLS 证书、部署控制、证书续签。

## 技术栈
- **Go**（独立 `go.mod`，模块名 `github.com/computeflux/metrics`；原为 `github.com/computeflux/tee-provider/ext/metrics`）
- **单二进制部署**：内嵌前端（`web/dist`）+ REST API
- **MySQL** 配置库（`monitor_config` 表，单行 JSON，30s 热加载）
- Let's Encrypt（DNS-01）证书自动续签
- HTTP 服务：Web UI + REST API

## 目录结构
- `main.go` — 入口（CLI 模式 + HTTP 模式）
- `server.go` — HTTP 服务
- `metrics/` — 监控逻辑
- `hacks/` — 运维脚本
- `web/` — 前端（`npm install && npm run build` 生成 `web/dist`）
- `compose.yaml` / `Dockerfile` — 编排/镜像

## 运行模式
```bash
cd apps/metrics
go build -o sslmon .
./sslmon                    # CLI：一次性检查，异常发邮件
./sslmon -loop              # 循环监控（默认 1h）
./sslmon -http              # HTTP 服务模式（默认 :8003）
SSL_HTTP_PORT=9000 ./sslmon # 指定端口
```
CLI 退出码：`0` 全正常 / `1` 有 WARN / `2` 有 EXPIRED 或 ERROR。

## 端口
- **默认 :8003**（`SSL_HTTP_PORT` 或 `-http-port` 可覆盖）。
- ⚠️ **与 `apps/mailer` 默认端口冲突**（两者默认都是 8003）。部署时务必用环境变量区分。

## 约定
- 遵循仓库根级约定（见上级 `AGENTS.md`）。
- 独立模块，不随主仓库 go.sum 同步。
- 改前端需重新 `cd web && npm install && npm run build`。
- 改动后跑 `go build ./...`。

## 脚本
以下命令均在项目根目录 `apps/metrics` 下执行；表中路径相对项目根。

| 脚本 | 用途 | 用法 |
|------|------|------|
| `hacks/docker.sh` | 用 `compose.yaml` 管理 sslmon。`start` 构建并后台启动；`stop` 停止容器（保留容器/卷）；`restart` 重启；`redeploy` ⚠️ 先 `git pull --ff-only`（仅当本目录是独立 git 仓库）、`docker rm -f sslmon`、重建镜像、最多等 60s 健康检查；`rebuild` ⚠️ `--force-recreate` 强制重建；`status` 看状态；`logs` 跟随日志；`down` ⚠️ 删除容器（默认保留 `metrics_certs` 卷） | `bash hacks/docker.sh start`<br>`bash hacks/docker.sh stop`<br>`bash hacks/docker.sh restart`<br>`bash hacks/docker.sh redeploy`<br>`bash hacks/docker.sh rebuild`<br>`bash hacks/docker.sh status`<br>`bash hacks/docker.sh logs`<br>`bash hacks/docker.sh down` |

要点：
- 无参数会打印用法并以 1 退出。需要 Docker + `docker compose` v2。
- `compose.yaml` 里 `workflow-net` 是 `external: true`，必须已存在（统一 compose 启动时会创建），否则 `up` 失败。
- 端口：宿主机 `${SSL_HTTP_PORT:-19003}` → 容器固定 `8003`；与 `apps/mailer` 默认端口冲突，注意区分。
- 常用环境变量（写 `apps/metrics/.env` 或当前 shell）：`SSL_HOSTS`、`SSL_HTTP_PORT`、`MONITOR_HOST`、`MONITOR_DEPLOY_TOKEN`（等价宿主机 root，务必设置强随机值）、`MONITOR_DB_DSN`、`ALICLOUD_ACCESS_KEY` / `ALICLOUD_SECRET_KEY`。
- 配置库（MySQL `monitor_config`）不可达时服务不启动（见 `compose.yaml` 注释）；容器挂载 `/var/run/docker.sock`，面板的部署控制等价宿主机 root。
- 本仓库是 monorepo，`apps/metrics` 下没有 `.git`，`redeploy` 会跳过 `git pull`，直接用当前工作区代码重建。
