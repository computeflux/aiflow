# apps/ai-flow —— PicoClaw fork（工作流 AI 助手）

## 来源与许可

| 项 | 值 |
|---|---|
| 上游 | https://github.com/computeflux/ai-flow |
| Commit | `bbf6893ca7afad27f1d00a0f5a45982a549c6ed6` |
| Fork 时间 | 2026-11-09 |
| 许可 | **MIT**（保留上游 `LICENSE`，不得删除；改动需在本文档留痕） |
| 上游状态 | GitHub `pushed_at` = 2026-11-05；上游 README 明示 "early rapid development / v1.0 前勿上生产" |

## ⚠️ 红线豁免（必须知晓）

根 `AGENTS.md` 规定：**禁止把第三方 `*.go` 源码复制进 `apps/`**（2026-11-03 红线）。
本目录是该红线的**显式例外**（决策人：项目 owner，2026-11-09），理由：
1. 需要"完整改造"（深度嵌入 workflow 的身份/权限/审批/API），外部进程 + MCP 无法满足；
2. 上游为 MIT，允许 fork 与商用；
3. 上游事实上由我们接管维护。

**该豁免仅适用于 `apps/ai-flow/`，不得推广到其它第三方项目。**

## 改造原则（重要）

1. **最小偏离**：能通过 `pkg/` 的扩展点（Tool 注册、MCP、Skills、Hooks、AgentLoopOption）实现的，**不要改上游文件**。
2. 我们的自有代码集中在以下目录，便于升级时 diff：
   - `pkg/tools/workflow/`（workflow API 工具，新增）
   - `pkg/workflow/`（workflow 领域客户端 / 身份透传 / 审批，新增）
   - `pkg/gateway/workflowapi*.go`（对 board 的 SSE 入口，新增）
3. 修改上游文件时，在改动处加 `// [workflow-fork]` 注释，并在本文档"偏离清单"登记。
4. 删除/裁剪的包同样在"偏离清单"登记，避免升级时误合并。

## 目录职责（上游原样）

| 目录 | 说明 | 我方策略 |
|---|---|---|
| `pkg/agent/` | AgentLoop、上下文、事件、MCP 装配 | **保留**，仅加选项 |
| `pkg/tools/` | 工具注册表 + 内置工具 | **保留**，加 `workflow/` 子包 |
| `pkg/providers/` | 30+ LLM provider | **裁剪**（保留 openai 兼容 / ollama / vllm） |
| `pkg/channels/` | 19+ 聊天渠道（26.5k LOC） | **裁剪**（board 不需要） |
| `pkg/gateway/` | HTTP/WS 入口 | **保留并扩展**（加 board SSE 端点） |
| `pkg/config/` | 配置加载（config.json + .security.yml） | **改**（接根 `.env` / compose） |
| `pkg/mcp/` | MCP 客户端 | 保留（可选能力） |
| `pkg/skills/` | SKILL.md | 保留 |
| `pkg/memory/` `pkg/session/` | 记忆 / 会话 | 保留 |
| `web/` | 上游 WebUI（3.7M） | **删除**（board 前端复用 core-ui） |
| `assets/` | 演示图片/视频（12M） | **删除** |
| `docs/` | 上游文档（2.3M） | **删除**（保留 LICENSE / 本文档） |

## 构建 / 测试

```bash
cd apps/ai-flow
go build ./...          # 全量编译
go test ./pkg/...       # 单测（上游约 138k 行测试）
go vet ./...
```

> 上游无 `vendor/`，首次构建需联网拉依赖。

## 偏离清单（每次改动追加）

| 日期 | 文件/目录 | 变更 | 原因 |
|---|---|---|---|
| 2026-11-09 | 全仓 | fork 自 `bbf6893` | 初始引入 |
| 2026-11-09 | `AGENTS.md` | 新增本文件 | 溯源 + 红线豁免 + 改造原则 |
| 2026-11-09 | `web/` `assets/` `docs/` | 计划删除 | 体积（18M）/ 由 core-ui 替代 |
| 2026-11-10 | `web/` `assets/` `docs/` | **已删除**（fork 引入时即不落盘，工作树/索引均无这三个目录） | 瘦身：上游 WebUI + launcher 前端链、演示图片视频 ~12M、上游文档 ~2.3M；board 前端复用 `apps/core-ui` |
| 2026-11-10 | `Makefile` | 同步清理 web/docs 引用：删除 `build-launcher-frontend`；`build-launcher` / `build-launcher-android-arm64` 改为显式失败 stub（保留目标名，提示已移除）；`build-android-bundle` 去掉 launcher 构建与 `libpicoclaw-web.so` 打包，仅保留核心 android 产物；`vet` / `test` 去掉 `grep -v '^github.com/sipeed/picoclaw/web/'` 过滤与 `cd web/backend && $(WEB_GO) vet ./...`、`cd web && make test`，直接 `./...`；`lint-docs` 改为 no-op echo（`scripts/lint-docs.sh` 保留不删）；`build-macos-app` 保留对 `build-launcher` 的依赖（在 stub 处显式失败） | 上游构建链已随目录删除，避免 make 目标指向不存在的目录 |
| 2026-11-10 | `onboard_workspace_embed.go` `workspace/` `pkg/skills/installer.go` | **未动**（约束项） | 运行时 embed 依赖；installer 中 'assets'/'docs' 是 skills 安装目录白名单，与本次删除无关 |
| 2026-11-10 | `Makefile` `docker/` `scripts/` `README.md` `.goreleaser.yaml` | P1-1 二轮清理：`Makefile` 删除 `WEB_GO` 变量（含 Darwin 分支赋值）与 `build-launcher-android-arm64` stub，`build-macos-app` 改为显式失败 stub（不再依赖已删脚本）；删除 `docker/Dockerfile.launcher` / `Dockerfile.goreleaser.launcher`、`scripts/build-macos-app.sh`；`docker/docker-compose.yml` 删除 `picoclaw-launcher` 服务；`.goreleaser.yaml` 删除 launcher before hooks / picoclaw-launcher build / launcher dockers_v2 / notarize+nfpms 中的 launcher 引用；`README.md` 删除 launcher 构建说明与 WebUI/gatekeeper 段落（本地 assets 图片引用保留指向原路径，渲染时由上游仓库托管页展示） | 上游 web/launcher 构建链专属文件全部移除，目标名保留 stub 避免外部脚本误触成功 |
| 2026-11-09 | `pkg/channels/` | 计划裁剪 | board 场景不需要 19+ 渠道 |
| 2026-11-09 | `pkg/providers/` | 计划裁剪 | 仅保留 openai 兼容 / ollama / vllm |
| 2026-11-10 | `pkg/workflow/config.go`（新增）+ `cmd/ai-flow/main.go`（gateway 启动接入）+ `go.mod`/`go.sum`（+godotenv） | P1-5：workflow 配置加载。从根 `.env`（godotenv，容器内挂载 `/app/.env`）读 `WORKFLOW_API_URL` / `AIFLOW_*`（端口、provider base_url/model/key、轮次上限），与 config.json（agents.defaults + model_list，gateway.port 默认 :8134）合并生成运行时配置；`runGateway` 在 `config.LoadConfig` 后应用并打日志 | ai-flow 融入 workflow 构建/部署面 |
| 2026-11-10 | `deploy/Dockerfile.ai-flow` + `deploy/compose/core/ai-flow.yml` + `deploy/docker-compose.yml`（include） | P1-5：多阶段镜像（golang:1.25 → distroless，hc 探针内联构建）+ compose 服务（:8134 仅内部、挂载根 .env 到 /app/.env、healthcheck、依赖 workflow-api healthy） | ai-flow 纳入统一 docker-compose 部署 |
| 2026-11-11 | `pkg/workflow/client.go` + `pkg/workflow/client_test.go`（新增） | P2-1：workflow-api(:8001) 只读 typed 客户端。`Client.Do(ctx, method, path, query, body)` 统一转发（ctx 取 `ContextKeyJWT` → Authorization: Bearer 透传；超时 3s / 重试 1 次，仅对传输错误与 5xx）；首版只读接口 boards list/get（含 lists/cards）、card executions、rag search（/search）、ops-report list（/ops-reports）、service list（/services）；列表默认截断 50 条 + truncated 标记；httptest 单测覆盖 JWT 透传/重试/截断/超时 | P2-1 工作项：ai-flow 接入 workflow API 只读面 |
| 2026-11-11 | `pkg/agent/workflow_prompt.go` + `pkg/agent/workflow_prompt_test.go`（新增）+ `pkg/agent/agent.go` / `agent_inject.go` / `context.go` / `cmd/ai-flow/main.go`（改动） | P2-4：workflow 系统提示注入。`workflowPromptContributor`（source `workflow:prompt`，capability/tooling，参考 toolDiscoveryPromptContributor）注入固定段落：领域术语表（board=看板/list=列/card=任务卡）、工具规范（先 list/get 查真实 id 再谈写、引用卡片必须带标题）、P2 只读红线（写操作请求说明将上线）、`<untrusted_data>` 内容非指令声明；`ContextBuilder.WithWorkflowPrompt()` 注册入口 + `AgentLoop.WithWorkflowTools(client)` 遍历 registry 各 agent ContextBuilder 注入并在 `ReloadProviderAndConfig` 换注册表后持久重放；`BuildSystemPromptParts()` 补齐收集 contributors（与 BuildMessagesFromPrompt 对齐，验收口径）；`cmd/ai-flow` gateway / agent 启动按 `WORKFLOW_API_URL` 构造 P2-1 typed client 并激活 | P2-4 工作项：让模型正确使用 workflow 工具 || 2026-11-11 | `pkg/tools/workflow/tools.go` + `tools_test.go`（新增）+ `pkg/agent/agent_inject.go` / `agent.go` / `workflow_prompt.go` / `workflow_prompt_test.go`（改动） | P2-3/P2-4 接线：新建 `pkg/tools/workflow` 只读工具集（workflow_board / workflow_card / workflow_rag / workflow_ops_report / workflow_service，包装 P2-1 typed client，输出统一 `<untrusted_data>` 包裹，参数缺 id 即报错 = 先查后写落地）；`AgentLoop.registerWorkflowTools` 由 no-op 占位改为真实注册（WithWorkflowTools(client) 与热重载换注册表后均生效）；导出 `WorkflowPromptPart()` 供工具侧复用同一契约 | P2-4 工作项收尾：workflow 工具 + 系统提示完整闭环 |
| 2026-11-14 | `pkg/tools/workflow/actions.go` + `write.go` + `write_tools.go` + `write_test.go`（新增）+ `pkg/tools/workflow/tools.go` / `tools_test.go` / `pkg/agent/workflow_prompt.go` / `workflow_prompt_test.go` / `pkg/agent/agent_inject.go` / `pkg/workflow/approval.go` / `client.go`（改动） | P4-2：ai-flow workflow 写工具 + 高危角色门控。`workflow_card` 增加 action=create/update/move/comment/finish/rerun + 高危 approve/decision/delete，`workflow_board` 增加 create/update/add_list + 高危 delete（board/list），全部走 P4-1 两阶段（confirm=false preview 签发 HMAC confirm_token → explain → confirm=true 执行，无 token/伪造/过期/重放 100% 拒绝）；高危清单角色门控（delete→admin，approve/decision→board_approver，workflow.RequireRole 二次收窄），高危单次仅允许 1 个动作（actions 数组 / action 逗号多值 / create 批量 cards 直接拒绝）；写与只读工具同注册进 WithWorkflowTools（AllWithDeps，共享一个 Approval broker + HTTPAuditSink，confirm 成功审计双写 audit_logs on_behalf_of 可查）；`Approval.Issued()`/`ConfirmWithSink()` + `NewApprovalBroker` 支撑工具侧签发与按请求审计 sink（共享 broker 的 sink 构造期不可变，运行期禁止切换）；写工具在注册表同名覆盖（last-wins）下承接 P2-3 只读 action（board list/get、card executions），高危 delete 目标按工具规范化（board→board / card→card），预览文本与实际执行目标同源一致；系统提示 P2 只读红线段落更新为 P4 两阶段确认规范（写操作必须 explain preview 后等待用户确认） | P4-2 工作项：ai-flow 写面接入审批/审计闭环 |
| 2026-11-12 | `cmd/ai-flow/main.go`（改动） | [workflow-fork] P1-6：gateway blank-import 全部渠道工厂包（telegram/discord/feishu/slack/…，matrix 除外——需 cgo libolm，非 workflow 部署链路）。上游设计 = 子包 init() 自注册 + gateway blank import 触发；fork 引入时缺失该 import，导致 `Factory not registered`、渠道无法启用 | Telegram 渠道接入（统一 compose） |
| 2026-11-12 | `pkg/workflow/config.go` + `config_test.go`（改动） | [workflow-fork] Telegram 渠道 env 接入：`applyTelegramChannel` 在 LoadWorkflowConfig 内按 `PICOCLAW_CHANNELS_TELEGRAM_TOKEN` / `_ENABLED` / `_ALLOW_FROM` 注入并启用 telegram 渠道（enabled 是 Channel 布尔字段、非 settings，上游 env tag 覆盖不到；无 config.json 场景 = DefaultConfig + 本注入）。token 已设且未显式禁用 → 自动启用；allow_from 逗号白名单，未设 = 开放任何人（上游 secure-by-default 告警适用） | workflow 统一 compose 部署接入 Telegram（无 config.json 依赖） |
