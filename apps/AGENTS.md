# apps — 子项目总览

`apps/` 是**独立运行的子项目集合**：每个目录都是独立项目（独立 `go.mod` / Python `uv` 项目 / Node 前端 / Docker 容器），**主项目不 import 它们**。跨项目改动前先查下表定位项目，再进入其目录（每个子项目自带 `AGENTS.md` / `README.md`，会被自动继承，含目录结构、端口、常用命令与约定）。

> **2026-11-12 git 拆分**：按依赖关系判定——**内部模块**（移除后主系统无法运行：`core` / `core-ui` / `auth` / `mailer` / `ai-bot` / `pycraper` / `pyreplay` / `metrics`）由主仓库 workflow 追踪；**外部可选项目**（`actives` / `c2c` / `game` / `minishop` / `social` / `dapp` / `tee-node` / `tee-node-monitor` / `web3auth` / `contract-deploy` / `pi-web`）各自是独立 git 仓库（remote = `https://github.com/computeflux/<name>.git`）。在主仓库内改动外部子项目目录不会被（也不应）被追踪——请在对应子目录内 `git add/commit`。

## 子项目速查表

| 目录 | 技术栈 | 功能 | 端口 / 运行方式 | 状态 |
|------|--------|------|-----------------|------|
| `actives` | Go（独立 go.mod） | 链上数据 / 积分（credit）/ Early Access 用户体系 GraphQL 服务（签到/任务/邀请/风控） | `cmd/server`（GraphQL server）；子命令 `import_data`/`send_credit`/`mint`/`snapshot` 等 | ✅ |
| `auth` | Go（独立 go.mod，Gin + MySQL） | **内部系统**统一身份（Authentication）+ RBAC 权限服务，自带 IdP（邮箱 OTP / Google / GitHub / X 社交登录），JWKS 本地验签；`/gql` 兼容 web3auth SDK 登录 mutations；管理前端 `GET /ui`（单文件 SPA 内嵌）；board 已接入（core API `internal/authn` 本地验签 + board 前端 SDK 登录）；对外 dApp（dapp/c2c/minishop）走专门链路（web3auth + tee-node），**不接入本服务** | `:8008`（`AUTH_PORT`） | ✅（P1+P2+P3；P4 board 已接入） |
| `core` | Go（独立 go.mod `github.com/wetee/ai-workflow`） | **工作流 Go 核心**（2026-10-10 由仓库根迁入）：Temporal 编排 + REST API（:8001）+ CRAG/Qdrant + 审批/进化闭环（S1–S9，见 `apps/core/AGENTS.md` 模块地图） | 各服务端口（compose 一服务一镜像） | ✅ |
| `core-ui` | React 19 + Vite 8 + TS | board 前端合并体（2026-10-10 由 board-ops + board-product + board-content + board-email + board-kit 五项目全部合并）：**单容器单端口、标准单入口 SPA**（一个 index.html + 一个 vite.config.ts），路径入口 `/ops/`（运维中心：S1 审批/进化 + S3 干预 + S6 运维 + 技能 + 用户）+ `/product/`（S7 产品情报 + 任务看板 + 思维导图）+ `/content/`（内容生产主链路）+ `/email/`（邮件），`/` 302 → `/ops/`，`React.lazy` 路由级代码分割；共享代码 `src/kit/`；SSO 登录枢纽 `:5181/ops/login` | `:5181` | ✅ |
| `c2c` | Go（独立 go.mod，Gin） | C2C 积分 ↔ 法币兑换平台：撮合 + 积分押注 + 收款核验 + 放点（不碰法币，资金走商家自有支付通道）；数据层本地 KV（pebble + TEE Sealer） | 代码默认 `:8006`（`C2C_PORT`；.env 覆盖为 `:8007`，TEE 模式仅 HTTPS）；体验台 `GET /c2c/ui` | ✅（M1–M6） |
| `contract-deploy` | Go（独立 go.mod） | 智能合约（EVM / ERC20-FLUX）部署与管理 Web 控制台 | `:8000`；`build.sh` / `deploy_contract.sh` / Docker | ✅ |
| `dapp` | React + Vite + TS | Dtoken 市场 DApp（WalletConnect / RainbowKit 连钱包，链上或后端 API 交互）；构建期 Puppeteer 预渲染 | `:5174`（Vite dev） | ✅ |
| `game` | React 19 + Vite + TS | AI 扑克（No-Limit Inference）：AI agent 打无限注德州扑克，内置 NLH 引擎 + WS transport | 前端 `:5177`；后端规划 `:8008` | ✅ 前端（后端未建） |
| `mailer` | Go（独立 go.mod，Gin + Temporal worker） | 邮件批量发送服务（SMTP 发信人 / 批次 / 模板 / 收件人） | 默认 `:8003`（`MAILER_PORT`）⚠️ 与 metrics 冲突 | ✅ |
| `metrics` | Go（独立 go.mod） | 项目指标监控（sslmon）：服务健康 / TLS 证书 / 部署控制 / Let's Encrypt DNS-01 续签；单二进制（内嵌前端） | 默认 `:8003`（`SSL_HTTP_PORT`）⚠️ 与 mailer 冲突 | ✅ |
| `minishop` | React + Vite + TS + **Bun** | 算力店铺 dApp：任何人开设算力小铺挂链上、按需消费、结算上链；应用级 SSR（Home/ShopDetail 服务端渲染 + SEO 端点 sitemap/robots/分享卡，CDN 回源） | 前端 `:5175`（dev）；SSR runner `:3000`（dev） | ✅（`share-server` 已被 app SSR 取代，保留作参考） |
| `pi-web` | Next.js + TS | pi coding agent 的本地浏览器 UI：会话工作区 / 分支 / 项目文件工具 / Git worktrees / Web 配置（多语言） | 默认 `:30141`（仅 `127.0.0.1`；`PORT` 可覆盖） | ✅（上游项目，本地使用） |
| `pycraper` | Python 3.12+（uv） | MCP 抓取 worker（`system=pycraper`）：从小红书 / X / 币安 / Reddit / GitHub 抓取内容写入 MySQL / Qdrant | Docker 容器；`main.py` 为 Temporal activity（`fetch`/`publish`/`social_generate`） | ✅ |
| `pyreplay` | Python 3.12+（uv） | MCP 执行 worker（`system=pyreplay`）：X/Twitter 点赞/回复 bot，执行分发任务（如 `dispatch_likes`） | Docker 容器；失败顺延 15~60min 重试，>5 次写 `error_logs` | ✅ |
| `tee-node` | Go（独立 go.mod） | tee-provider：TEE 去中心子网（CometBFT 共识 + DKG 阈值签名 + 代理重加密 + TEE 远程证明），提供 Web3Auth / API 网关 / 秘密管理 / 合约调用（`contractCall` / `contractDryRun`）GraphQL | GraphQL `/gql`（c2c 默认 `:8080`；minishop 生产 `:30118`）；可作任意公链子网 | ✅ |
| `tee-node-monitor` | Go（独立 go.mod） | 链上 TEE 节点数据监控（合约调用 / 数据收集 / 报告） | `:8002`；Docker + `compose.yaml` | ✅ |
| `web3auth` | TS 前端 SDK + Go 后端 | 自建 Web3Auth 替代：邮箱 OTP + Google/GitHub/X 社交登录 + 嵌入式以太坊钱包（EIP-1193 Provider） | 后端默认 `:8080`（`back/`）；前端 SDK 以 `file:` 本地依赖被 dapp / c2c / minishop 引用 | ✅（生产签名后端已迁 tee-node） |

## 跨项目关系图（改动前先看这里）

```
对外 dApp 侧（专门设计：web3auth + tee-node，不接入 auth，身份域独立）
 web3auth（前端 SDK，file: 本地依赖）
 ├── dapp / c2c / minishop 直接 import（登录 → 钱包直调合约）
 └── 登录/链上签名后端：tee-node /gql（原 web3auth/back 已迁至 tee-node）
 tee-node（/gql）
 ├── c2c：鉴权（checkSession）+ 读合约经济策略（汇率/佣金/限额）
 ├── minishop / dapp：contractDryRun 读链（SCALE 解码）
 ├── actives / tee-node-monitor：链上数据源
 └── 合约：由 contract-deploy 部署（ERC20-FLUX 等，ABI 变更需同步各消费方）

内部工作流侧（规划统一登录：auth :8008，JWKS 本地验签 + RBAC）
 apps/core（:8001，工作流 Go 核心）
 ├── board 前端（core-ui 单容器 :5181 单入口 SPA，路径入口 /ops + /product + /content + /email）：审批/任务/错误日志 UI（另对接 mailer :8003）；登录走 auth（web3auth SDK → :8008），core API 本地 JWKS 验签（internal/authn）
 └── pycraper / pyreplay：Temporal activity 抓取/执行（MCP 协议，勿破坏签名）
 auth（:8008）— 内部系统统一身份 + RBAC；已接入：board + metrics + contract-deploy + tee-node-monitor（P4 完成）；c2c 属对外 dApp 身份域不接入，upay 已删除
 metrics（:8003）— 已接入 auth（JWKS 本地验签，静态 token 兜底）
 contract-deploy（:8000）— 已接入 auth（写操作 requireAuth，admin 角色；AUTH_URL 空时直通）
 tee-node-monitor（:8002）— 已接入 auth（管理操作 JWKS 验签，admin 角色；静态密码兜底）
```

## 约定

- 进入某子目录即视为独立项目：以其自己的 `AGENTS.md` / `README.md` 为准，改完优先跑该模块自己的 lint / build / test。
- Go 子模块均为独立 `go.mod`（`core` 例外：模块名 `github.com/wetee/ai-workflow`），**不随彼此 `go.sum` 同步**。
- 错误统一落 MySQL `error_logs`（`system=<项目名>`）。
- ⚠️ **端口冲突**：`mailer` 与 `metrics` 默认都是 `:8003`（board 邮件 API 默认也指向 `:8003`）——部署时用 `MAILER_PORT` / `SSL_HTTP_PORT` 区分；`game` 后端规划与 `auth` 规划都占 `:8008`。
- ⚠️ **双身份域**：对外 dApp 侧（dapp/c2c/minishop）用 tee-node Web3Auth 身份，内部工作流侧用 auth 身份，两套独立、互不打通（专门设计，不要混用）。
- ⚠️ **kit 共享代码**：board 系列共享代码在 `core-ui/src/kit/`（原 board-kit）；改它影响 core-ui（唯一 board 前端项目，四板块），改后跑 `npm run build` 一次全量重建。
- 各服务启动自动加载根 `.env`。
