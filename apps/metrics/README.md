# sslmon — 项目指标监控

监控项目整体指标，支持：

- **服务健康**：对配置中的项目服务做 HTTP 探测（存活状态 + 延迟 + HTTP 状态码）
- **TLS 证书**：监控域名证书有效期与证书链状态
- **部署控制**：在面板上重启/重编译其他应用（调用各应用现有部署脚本，需 token 鉴权）
- **证书续签**：证书剩余 ≤10 天时自动通过 Let's Encrypt（DNS-01）签发新证书，面板可下载
- **CLI 模式**：一次性检查 / 循环监控 / 邮件告警（原功能）
- **HTTP 服务模式**：Web UI + REST API（内嵌前端，单二进制部署）
- **配置数据库**：所有数据（监控主机/服务/可部署应用/续签/邮件告警）统一由 MySQL 维护（单行 JSON），面板“设置”页可读写，30s 热加载

## 快速开始

### CLI（原有用法）

```sh
go build -o sslmon .
./sslmon                        # 一次性检查，异常时发邮件
./sslmon -loop                  # 循环监控（默认 1h 间隔）
./sslmon -mail-test             # 测试邮件
./sslmon -hosts a.com,b.com -json  # 显式主机（最高优先级），JSON 输出
```

配置数据取自 MySQL `monitor_config` 表（邮件告警配置也在其中）；数据库不可达时不阻塞检查，仅降级为无邮件告警（内置默认主机）。

退出码：`0` 全部正常，`1` 有 WARN，`2` 有 EXPIRED/ERROR。

### HTTP 服务模式（Web UI）

```sh
# 先构建前端（生成 web/dist）
cd web && npm install && npm run build && cd ..

go build -o sslmon .
./sslmon -http                        # 默认监听 :8003
./sslmon -http -http-port 9000        # 指定端口
SSL_HTTP=1 SSL_HTTP_PORT=8003 ./sslmon  # 或用环境变量
```

- **UI**：`http://localhost:8003/` — 总览大盘（服务健康 + 证书摘要）/ 证书详情页（`/certs`，主机列表、状态、剩余天数、详情展开）/ 设置页（`/config`，数据库配置 JSON 编辑）
- **API**：`GET /api/status` — 项目指标 JSON：`hosts`(证书) + `services`(服务健康)，60s 缓存
- **健康检查**：`GET /healthz`

服务健康检查的主机占位符：配置中 `services[].url` 的 `xxxx` 会替换为 `$MONITOR_HOST`
（容器部署默认 `host.docker.internal`，经 host-gateway 探测宿主机服务）；不含 `xxxx` 的地址原样使用。

### 部署控制（面板重启/重编译其他应用）

服务表的“操作”列可执行各应用**现有的部署脚本**（`deployables`，数据库配置，见下文）：

- `GET /api/deploy/config` — 可部署应用与允许的子命令（前端据此渲染按钮）
- `POST /api/deploy` — `{"name": "workflow", "action": "restart"}` → `{ok, exit_code, duration_ms, output}`

实现原理：容器挂载 `/var/run/docker.sock`，通过
`docker run --rm --privileged --pid=host <helper> nsenter … setpriv … bash -lc '<脚本> <子命令>'`
进入宿主机命名空间、以部署用户（含完整补充组）执行脚本，宿主机的 docker / node /
`systemctl --user` 均可直接使用，**各应用无需任何修改**。

相关环境变量（见 compose.yaml）：

| 变量 | 说明 |
| --- | --- |
| `MONITOR_DEPLOY_TOKEN` | **必填**：部署 token（面板输入，存 localStorage）。未设置时部署/续签接口返回 503 |
| `MONITOR_REPO` | 部署脚本所在仓库根目录（默认 /home/wetee/work/cathome/workflow） |
| `MONITOR_DEPLOY_UID` / `MONITOR_DEPLOY_HOME` | 宿主机执行用户（默认 1001 / /home/wetee） |
| `DEPLOY_HELPER_IMAGE` | helper 容器镜像（默认 alpine:3.21，需含 nsenter） |
| `ALICLOUD_ACCESS_KEY` / `ALICLOUD_SECRET_KEY` | 阿里云 AccessKey（证书续签 DNS-01，需 AliyunDNSFullAccess） |

### 证书续签（Let's Encrypt）

配置 `renewal`（数据库配置，见下文）后启用：

- **自动续签**：每 30 分钟检查一次，监控证书剩余天数 ≤ `renewal.days`（默认 10）时自动签发
- **手动续签**：面板证书页"续签"按钮 → `POST /api/renew {"host": ...}`（需 token，仅限监控主机）
- **下载**：`GET /api/cert/{host}/{cert|fullchain|privkey}`（私钥需 token）；面板证书页有"全链/私钥"按钮

验证方式为 **DNS-01**（域名在阿里云 DNS，无需域名指向本机）：签发时自动创建/删除
`_acme-challenge.<host>` TXT 记录。签发产物存于卷 `metrics_certs`（`/app/certs/<host>/`），
ACME 账号私钥同卷持久化。本地新证书仍有效时自动续签会跳过，避免触发 LE 速率限制。

> **凭证**：DNS-01 需要阿里云 DNS 的 AccessKey（建议 RAM 用户，仅授权 DNS 记录读写；
> 可选 `ALICLOUD_REGION_ID`，默认 `cn-hangzhou`）。写入 `apps/metrics/.env`：
> `ALICLOUD_ACCESS_KEY=...` / `ALICLOUD_SECRET_KEY=...`（compose.yaml 已透传，
> `.env` 在仓库根 `.gitignore` 中已忽略），`./hacks/docker.sh start` 重建容器后生效。

> 注意：签发的新证书需要**下载后自行部署**到对应服务（本工具不自动替换线上证书）。
> `renewal.staging: true` 使用 LE staging 环境（测试用，证书不可用于生产）。

> ⚠️ **安全警告**：该能力等价于宿主机 root 权限（任意容器操作 + 任意用户执行）。
> 务必设置强随机 `MONITOR_DEPLOY_TOKEN`，并确保面板端口不对不可信网络暴露。

配置来源优先级：`-hosts` 参数 > `SSL_HOSTS` 环境变量 > 配置文件 `hosts` > 内置默认主机。
Web UI 的服务切换列表读取 workflow board 的 `urls.json`（默认 `http://<host>:5181/urls.json`，board-ops 枢纽；可用 `VITE_URLS_JSON` 覆盖）。

### 配置数据库（所有数据均由 MySQL 维护）

所有配置数据（监控主机/服务/可部署应用/续签/邮件告警）以单行 JSON 存于 MySQL `monitor_config` 表，不再有 YAML 配置文件：

- **读取**：`GET /api/config` → `{config, updated_at}`（需 token）
- **写入**：`PUT /api/config` → `{ok, updated_at}`（需 token，校验 `hosts` 非空）；面板“设置”页（`/config`）可直接编辑 JSON
- **热加载**：后台 30s 轮询，保存后无需重启容器即生效（服务健康/证书监控/续签阈值/部署列表均实时读取）
- **播种**：首次启动表为空时，自动用内置默认配置写入（请在面板设置页完善）
- **无回退**：数据库不可达时 HTTP 服务启动失败（容器按 restart 策略重试），避免配置不一致的静默降级；首次启动带有限重试（约 40s），容忍数据库稍后启动/恢复；CLI 模式仅降级为无邮件告警

配置库连接由环境变量决定（连接信息本身不进配置，不存在自指）：

| 变量 | 说明 |
| --- | --- |
| `MONITOR_DB_DSN` | 配置库完整 DSN；留空则用内置默认 `workflow:workflow123@tcp(dev-workflow.computeflux.ai:3306)/workflow` |

> 注意：数据库中的 `hosts` 同时是证书监控列表与续签白名单；`renewal.email` 非空才启用自动续签。
> `host:port` 条目参与监控与续签白名单，但 Let's Encrypt 只对域名（DNS-01）签发，与端口无关。
> 主机优先级：`-hosts` 参数 > `SSL_HOSTS` 环境变量（临时覆盖，同样支持 `host:port`）> 数据库配置 > 内置默认。

### Docker 部署

```sh
# 构建镜像并后台启动(推荐)
./hacks/docker.sh start

# 管理命令: stop / restart / rebuild / status / logs / down
# 全量重新部署(拉最新代码 → 重建镜像 → 启动 → 健康检查):
./hacks/docker.sh redeploy

# 自定义配置(环境变量, 见 compose.yaml):
#   SSL_HTTP_PORT  面板端口(宿主机), 默认 19003
#   SSL_HOSTS      证书监控主机(逗号分隔), 默认取数据库配置
#   MONITOR_HOST   服务健康检查主机(替换 xxxx 占位符), 默认 host.docker.internal
SSL_HTTP_PORT=19003 SSL_HOSTS="a.com,b.com" ./hacks/docker.sh start
```

## 配置数据（MySQL monitor_config 表）

面板“设置”页（`/config`）直接编辑该行 JSON：

| 字段 | 说明 |
| --- | --- |
| `hosts` | 要监控的域名列表; 条目可带端口(`host:port`, 如 `xiaobai.asyou.me:8443`), 同一域名可监控多个端口; 不带端口用全局 `port` |
| `port` | 全局默认 TLS 端口（默认 443，仅对不带端口的条目生效） |
| `warn_days` | 到期不足 N 天告警（默认 30） |
| `services` | 项目服务健康检查列表（`name` + `url`，`xxxx` 替换为 `$MONITOR_HOST`） |
| `deployables` | 可部署应用列表（`name` + `script` + `actions` 白名单，面板部署控制） |
| `renewal` | Let's Encrypt 续签：`enabled`/`days`/`email`/`dns`/`staging` |
| `interval` | 循环模式检查间隔（默认 1h） |
| `alert_to` / `smtp` | 告警邮件收件人与 SMTP 配置 |

## 目录结构

```
apps/metrics/
├── main.go         # 入口：CLI + HTTP 服务模式
├── server.go       # HTTP 服务（/api/status、/healthz、静态资源）
├── dbconfig.go     # 配置数据库（MySQL 单行 JSON, 30s 热加载, 无回退）
├── embedded.go     # go:embed web/dist
├── web/            # 前端（React + Vite + Tailwind, 参考 tee-node-monitor/web）
├── hacks/docker.sh # Docker 运行/部署脚本（同 tee-node-monitor）
├── Dockerfile      # 多阶段构建（node → golang → alpine）
└── compose.yaml    # docker compose 部署
```