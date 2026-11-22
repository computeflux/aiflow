# apps/core-ui — board 前端合并体（唯一 board 前端项目）

React 19 + Vite 8 + TypeScript **标准单入口**前端（2026-10-10 由 `board-ops` + `board-product` + `board-content` + `board-email` + `board-kit` 全部合并；**单容器单端口 :5181**）。一个 `index.html` + 一个 `vite.config.ts`，四个板块由前端路由区分，`React.lazy` 按路由做代码分割（重依赖互不拖累）。

## 路由与地址

| 路由 | 地址 | 职责 |
|------|------|------|
| /ops | :5181 `/ops/` | 运维中心：任务控制台、技能、审批中心、干预舱、Agent、错误日志、运维情报、链路追踪、用户管理（全系统唯一登录框宿主 `/ops/login`） |
| /product | :5181 `/product/` | 产品与任务：**默认页 = 工作看板**（`/product/` 直接进看板）、思维导图、工作项中心、审批中心、文档（仓库 `docs/` 树形浏览/编辑/预览 + AI 生成 + 拆解为任务）、项目 |
| /content | :5181 `/content/` | 内容生产（MCP→RAG→发布主链路，原 board-content :5182）：社媒生成、X 点赞管理、定时抓取、素材库、向量库、工作流历史 |
| /email | :5181 `/email/` | 邮件（原 board-email :5183）：邮件推广、模板、发信服务商 |

`/` 302 → `/ops/`。入口 `src/main.tsx`：SSO boot + 顶层 `BrowserRouter` + 四板块懒加载；各板块 `src/<板块>/App.tsx` 只含 provider + 内部路由（内部路径相对板块前缀，如 `/ops/errors`、`/email/email-templates`）。nginx SPA 回退统一指向 `/index.html`（见 `deploy/nginx/core-ui.conf`），同源代理 `/api/` `/mailer/` `/jaeger/`。

## 目录结构

```
apps/core-ui/
├── index.html            # 唯一 HTML 入口
├── vite.config.ts        # 唯一构建配置（dedupe + react/react-dom alias 双实例红线，见文件内注释）
├── public/               # 静态资源（favicon/urls.json/overview.html；urls.json 供外部服务发现服务列表）
├── src/
│   ├── main.tsx          # 入口：SSO boot + 顶层路由 + 四板块懒加载
│   ├── index.css         # Tailwind v4 主题（四板块共用）
│   ├── kit/              # 共享代码（原 board-kit：api/auth/Sidebar/组件；入口 index.ts）
│   ├── ops/              # ops 板块（原 board-ops/src）
│   ├── product/          # product 板块（原 board-product/src）
│   ├── content/          # content 板块（原 board-content/src）
│   └── email/            # email 板块（原 board-email/src）
└── package.json
```

## 共享代码（kit）

- kit 代码在 `src/kit/`，四个板块内 import 走 **`@kit`** alias（vite alias + tsconfig paths 指向 `src/kit/index.ts`）。
- 原 board-content/board-email 的 `@wetee/board-kit` 导入已统一改写为 `@kit`。
- Tailwind v4 自动扫描项目根（`src/` 全覆盖含 kit），无需 `@source`。
- ⚠️ 改 `src/kit/` 影响全部四个板块：`npm run build` 一次全量重建即可。
- ⚠️ 双实例红线：`@wetee/web3auth` 是 file: 符号链接依赖，`vite.config.ts` 里 react/react-dom 的 alias 钉死本项目 node_modules，不可删。

## 常用命令

```bash
cd apps/core-ui
npm install
npm run dev          # 开发 :5181（四板块 /ops /product /content /email 同 dev server）
npm run build        # tsc -b + vite build → dist/
npm run lint         # oxlint
```

## 脚本

npm scripts（定义见 `package.json`）：

| 命令 | 用途 |
|------|------|
| `npm run dev` | `vite` 开发服务器 :5181（`vite.config.ts` 里 `server.port` 写死），四板块共用同一端口 |
| `npm run build` | `tsc -b` + `vite build` → `dist/`（一次构建全部板块，含 `src/kit/`） |
| `npm run preview` | `vite preview` 预览 `dist/` |
| `npm run lint` | `oxlint`（不是 eslint；没有 lint 自动修复脚本） |

要点：
- **没有 `test` 脚本**，也没有单板块构建/发布脚本——四板块把同一份 `dist/` 部署，构建粒度就是整包。
- `build` 先 `tsc -b` 再 `vite build`，类型错误会中断构建；改 `src/kit/` 或任一板块后跑一次 `npm run build` 即可覆盖四板块。
- `preview` 只是静态预览：不带 nginx 的 `/api` `/mailer` `/jaeger` 同源代理，接口请求不会被转发，会落到前端路由或 404；联调请用 `npm run dev` 或部署后的 :5181。
- 部署（重建镜像）见下节，`preview` 不能替代。

## 部署

单容器单端口（compose 服务 `core-ui`，镜像 `computeflux/core-ui`；`deploy/Dockerfile.core-ui` 构建单一 `dist/` 直接拷入 nginx 根，配置 `deploy/nginx/core-ui.conf`）：

```bash
cd deploy && docker compose up -d --build core-ui
```

⚠️ 仅镜像更新时 `up -d` 不重建运行中容器，需加 `--force-recreate`。

## 后端 API / SSO

主 API `:8001`（`/api` 同源代理，见 `deploy/nginx/core-ui.conf`）；请求统一走 kit 的 `api.ts`，不要散落 `fetch`。登录走 auth（web3auth SDK → :8008；**`/ops/login` 是全系统唯一登录框宿主**，其他服务（pi-web/dapp/minishop 等）的 SSO 副本（各自 sso.ts）都指向 `:5181/ops/login`，改登录路径需同步改全部副本）。遵循仓库根级约定（见上级 `AGENTS.md`）。
