# auth（统一权限认证服务）

独立 Go 模块（`github.com/computeflux/auth`），全系统统一身份 + RBAC 权限服务，**自带 IdP 后端**（邮箱 OTP / Google / GitHub / X 社交登录，自研，不依赖 tee-node）。前端复用 `apps/web3auth` 的登录 SDK，其 `backendUrl` 指向本服务。

> 当前状态：**P1+P2+P3+P4 已实现**（登录/验签/RBAC/审计 + 管理前端 `GET /ui` + 各应用接入）。P4 接入对象为 board + metrics + contract-deploy + tee-node-monitor（均已完成）；c2c 属对外 dApp 身份域（web3auth + tee-node，不接入本服务），upay 已删除。设计见 `README.md` + `docs/`（01 模块设计 / 02 API / 03 交互图 / 04 线框图 / 05 安全 / 06 路线与开放问题）。

## 已实现
- **Go + Gin + MySQL**（复用 workflow 库，表前缀 `auth_`，GORM AutoMigrate）
- 端口 `:8008`（`AUTH_PORT` 可覆盖）；`bash hacks/build.sh` 编译+运行
- 邮箱 OTP（6 位/5min/SHA256/5 次锁 30min/60s 限频，SMTP 直发）+ 社交登录（Google/GitHub/X OAuth code 交换，X 带 PKCE）+ **钱包登录**（外部钱包 MetaMask/WalletConnect，EIP-191 personal_sign 验签，迁移自 tee-node `loginWithWallet`；`requestWalletNonce`+`loginWithWallet` 两个 mutation，web3auth SDK 无需改动）
- 自包含 JWT（RS256, 15min, kid 轮换）+ 不透明 refresh（30 天，轮换 + 重放检测全吊销）+ 黑名单
- JWKS 端点（下游本地验签）+ `/blacklist/{jti}`
- RBAC（`app:resource:action` 权限点）+ `/authorize` 实时判定 + 用户禁用/会话管理 + 审计（异步批量）
- **`/gql` 兼容层**：web3auth 前端 SDK 的 requestOTP/verifyOTP/socialLogin mutations 直接映射到本服务（SDK 无需改动，`backendUrl` 指向 :8008 即可）
- 管理前端（P3）：`web/index.html` 单文件 SPA（零依赖，go:embed 进二进制，`GET /ui`），深色等宽风格对齐 board；四页：用户（列表/详情/角色/直授/会话）/ 角色权限 / 审计 / 设置；登录走邮箱 OTP（需 admin 角色）或 X-Admin-Token
- **P4 board 接入**：
  - 后端（主项目）：`internal/authn`（JWKS 本地验签中间件，kid 缓存 TTL 1h）；`cmd/api` 敏感路由接入——审批/发布 `requireRole(admin, board_approver)`，工作流 cancel/看板/卡片/定时任务 `requireAuth()`，terminate/发信商/X 账号/技能等 `requireRole(admin)`；`AUTH_URL` 为空时直通（兼容未部署 auth 的环境）
  - 前端（board）：引入 `@wetee/web3auth`（file: 依赖）+ `src/auth.tsx`（AuthProvider，backendUrl → :8008，登录后拉 /me 暴露 roles）；`api.ts` 的 request 自动带 Bearer + 401 静默续期重放；Sidebar 底部登录/登出；`VITE_AUTH_API_URL` 可覆盖
- **用户管理**：`POST /users`（手动添加用户：email 必填 + name + roles，如预创建管理员；创建后该邮箱首次 OTP 登录自动合并到本用户）；`GET /users` 列表含 roles；`DELETE /users/:id` 禁止删除自己（自删保护）。board 前端 `pages/Users.tsx`（`/users` 路由，仅 admin 可见）：查看/添加/删除管理员

## 约定
- 遵循仓库根级约定（见上级 `AGENTS.md`）。
- 独立模块，不随主仓库 go.sum 同步。
- 实现阶段：P1 登录+验签 → P2 RBAC → P3 管理前端 → P4 各应用接入（见 `docs/06-roadmap.md`）。

## 脚本
以下命令均在项目根目录 `apps/auth` 下执行；表中路径相对项目根。

| 脚本 | 用途 | 用法 |
|------|------|------|
| `hacks/build.sh` | 编译 `bin/auth` 并前台运行（默认）。⚠️ 运行前 `pkill -f "bin/auth"` 停掉旧进程，匹配到该字符串的进程都会被终止 | `bash hacks/build.sh`<br>`bash hacks/build.sh -b`（只编译）<br>`AUTH_PORT=8008 bash hacks/build.sh` |

要点：
- 需要 Go；脚本会把 `$HOME/Soft/go/bin` 追加到 `PATH`。
- 运行配置由 `config.Load` 依次读当前目录 `.env`、`../.env`、`../../.env`（仓库根 `.env`）；`AUTH_PORT` 默认 `8008`，MySQL 走 `AUTH_DB_DSN` 或 `MYSQL_*`。
- 只识别第一个参数 `-b`；传其它参数会按「编译 + 运行」处理。
- `8008` 是仓库对外暴露的登录枢纽，本地起服务注意端口占用；正式部署走 `deploy/` 容器（`deploy/compose/core/auth.yml`）。
