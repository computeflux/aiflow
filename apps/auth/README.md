# auth — 统一权限认证服务

> 状态：已实现（P1 登录+验签 + P2 RBAC + P3 管理前端 `GET /ui` + P4 各应用接入：board + metrics + contract-deploy + tee-node-monitor 已完成；c2c 属独立身份域不接入，upay 已删除）
> 本地启动：`bash hacks/build.sh`（编译 + 前台运行，:8008）；配置见 `.env.example`（复用根 .env 的 MYSQL_* / SMTP_*）。
> web3auth 前端 SDK 兼容：`POST /gql` 映射 SDK 的 requestOTP / verifyOTP / socialLogin mutations（SDK 无需改动，`backendUrl` 指向本服务即可）。
> 定位：整个 workflow 的统一**身份（Authentication）+ 权限（Authorization）**服务，**自带 IdP 后端**（邮箱 OTP / Google / GitHub / X 社交登录，自研，不依赖 tee-node）。
> 前端（内部侧）：board / metrics 等复用 `apps/web3auth` 的登录 SDK（LoginModal / OTP / 社交登录），其 `backendUrl` 直接指向本服务。
> ⚠️ 对外 dApp 侧（dapp/c2c/minishop）是**专门设计**：web3auth SDK 的登录 + 链上签名均指向 tee-node，**不接入本服务**，其身份域与内部系统独立。
> 技术栈：Go（独立模块 `github.com/computeflux/auth`）+ Gin + MySQL（复用 workflow 库）+ 可选管理前端。
> 端口：`:8008`（`AUTH_PORT` 可覆盖）。

## 文档索引

| 文档 | 内容 |
|------|------|
| [docs/01-module-design.md](docs/01-module-design.md) | 模块设计：总体架构、核心决策、包结构、数据模型、JWT claims、前端 SDK |
| [docs/02-api-design.md](docs/02-api-design.md) | API 设计：全部端点、请求/响应示例、鉴权方式 |
| [docs/03-interaction-diagrams.md](docs/03-interaction-diagrams.md) | 交互图：登录/续期/验签/角色变更/登出/密钥轮换/状态机（mermaid） |
| [docs/04-wireframes.md](docs/04-wireframes.md) | 线框图：管理前端各页面 + 业务应用接入示意（ASCII） |
| [docs/05-security.md](docs/05-security.md) | 安全设计要点 |
| [docs/06-roadmap.md](docs/06-roadmap.md) | 演进路线 + 实现前开放问题 |

## 实现说明（与设计的差异）

- **`/gql` 兼容层**：web3auth SDK 的登录走 GraphQL（`requestOTP`/`verifyOTP`/`socialLogin`），本服务核心是 REST，`handler/gql.go` 把 SDK 的固定 mutation 映射到同一套 service；响应字段对齐 SDK（`token`/`refreshToken`/`address`/`userInfo`）。`address` 无钱包地址时回退 user_id（SDK 要求非空才能持久化会话）。
- **OAuth state**：SDK 在回调页自行比对 state（sessionStorage），不传给后端；后端只验 code(+PKCE)。
- **admin 引导**：`AUTH_ADMIN_EMAIL` 首次登录自动授 admin；`X-Admin-Token` 供服务间管理操作。
- **默认角色**：首次登录自动授内置 `user` 角色（无权限点，管理员手工授权）。

## 背景与目标

### 现状问题
- 各服务无统一登录：主 API（:8001）、board（:5173）裸奔；metrics 用静态 `MONITOR_DEPLOY_TOKEN`。
- 没有"操作人"概念：board 审批、c2c 撮合都无法追溯是谁做的。
- `apps/web3auth` 只有前端 SDK，其登录/签名后端目前指向 tee-node 的 /gql；不能直接复用 tee-node 代码，需要自研后端。
- 登录能力（OTP/社交）与身份主数据、权限模型分散，无法统一追溯操作人。

### 目标
1. **统一身份**：全系统一个 `user_id` 主键，跨应用身份打通（board 和 c2c 是同一个用户）。
2. **统一认证**：各业务服务本地验签（JWKS），无逐请求网络调用、无强单点。
3. **集中权限**：RBAC 角色/权限一处定义、一处维护；支持 token 吊销。
4. **可扩展登录方式**：本期实现邮箱 OTP + 社交登录，将来可加邮箱密码、SSO、API Key，业务服务无感。
5. **审计**：登录、授权变更、关键操作可追溯。

### 非目标（本期不做）
- 不服务对外 dApp 侧（dapp/c2c/minishop）：它们走专门链路（web3auth SDK + tee-node 登录/链上签名），不接入本服务（见上方「前端」说明）。
- 不做交易密码/challenge-response（那是 web3auth 侧的钱包安全，见 `apps/web3auth/docs/security-redesign-v2.md`）。
- 不做 OAuth2 Provider 对外（不服务外部第三方应用）。
- 不做网关/流量收敛（Traefik 是独立决策，见 [docs/06-roadmap.md](docs/06-roadmap.md)）。
