# Agent 章程：角色、协议、替换与权限边界

依据 `docs/strategy/agent-architecture.md` v2.0 与 `docs/plan/transform-plan.md` §4。核心原则：**角色（职责接口）固定，实现可替换**。

## 1. 角色定义

| 角色 | 职责 | 说明 |
| --- | --- | --- |
| `coder` | 在 git worktree 内修改 `apps/*` 代码 | 实现：`pi`（pi-web 包内置 pi CLI） |
| `reviewer` | 对 diff 产出**行级**评论 | 实现：`ocr`（open-code-review） |
| `analyst` | 分析产品需求并拆解成任务卡（只读代码，不写文件） | 实现：`pi` 复用（pi-web，只读工具白名单） |

`analyst` 不是进化链的一环：它只服务看板 AI 的「产品需求分析」——按需调用（像 skill），
产出结构化卡片 JSON 交前端人工确认后落库（`create_cards` → `/ai-execute`）。

替换 Agent 时业务代码零改动：`evolve-driver`、`board`、`agentctl` 只依赖角色接口与统一 JSON 协议，不 import 任何具体 Agent。

## 2. 统一 JSON 协议（stdin → stdout）

**coder**：

```jsonc
// 输入
{"op":"fix","task":"修复 xxx","workdir":"/abs/path/to/worktree","files":["apps/pycraper/main.py"],
 "kind":"bug|requirement","feedback":"上一轮反馈","session_id":"..."}
// 输出
{"ok":true,"diff":"<git diff>","files_changed":["apps/pycraper/main.py"],"session_id":"..."}
{"ok":false,"reason":"..."}   // 失败，exit 1
```

**reviewer**：

```jsonc
// 输入
{"op":"review","diff":"<git diff>","workdir":"/abs/path","base":"main"}
// 输出
{"ok":true,"comments":[{"file":"apps/x.go","line":42,"level":"error|warning|info","message":"..."}]}
{"ok":false,"reason":"..."}   // 失败，exit 1
```

**analyst**（可选，看板 AI 用）：

```jsonc
// 输入
{"op":"analyze","requirement":"需求原文","board_context":"看板/项目/仓库摘要","list_id":21,"cwd":"/repo"}
// 输出
{"ok":true,"cards":[{"title":"...","description":"...","priority":"high","target_name":"core-ui",
  "checklist":["..."],"estimate_hours":2,"list_id":21,"depends_on_index":[1]}],"session_id":"..."}
{"ok":false,"reason":"..."}   // 失败，exit 1
```

> `analyst` 在 `workflow-api` 内是**进程内调用**（`agentcli.AnalyzeRequirement`），
> 不需要容器 exec；CLI 入口 `cmd/agent-analyst` 供注册表/其它调用方使用。

调用方（evolve-driver）会用 JSON Schema 校验输出，协议漂移立即失败（`coder_schema` / `reviewer_schema`）。

**adapter 形态**：原生可执行文件（Go 二进制，见 §3）或 `*.sh` 脚本（简单/测试替身）。
driver 的 `runAgentHost` 按扩展名分派：`.sh` → `bash <path>`，其余 → 直接 exec。

> 历史（2026-10-17 起）：pi/ocr 的 adapter 由 `adapter.sh` + 内联 python3 改为 **Go 二进制**
> （源码 `apps/core/internal/agentcli/`，入口 `cmd/agent-pi`、`cmd/agent-ocr`）。
> 原因：bash 流式读取脚本 → 运行中改脚本会让进程读到**错位内容**（无法安全热修）；
> 内联 JSON 解析在字段漂移时**静默丢内容**（ocr v1.13 正文键 `message`→`content`，
> 导致返工反馈全空、白烧一轮 coder）；超时/HTTP 重试/多仓库 diff 无法单测。
> 协议与入口路径语义不变，换实现只需换 driver 的 `*_ADAPTER_PATH`。

## 3. 目录结构

```
agents/
├── pi/                # coder 实现：pi（+ pi-web UI）
│   ├── agent.json     # manifest（角色/协议/入口/限额/权限）
│   └── config/        # 凭据与本地配置（gitignore，不提交）
├── ocr/               # reviewer 实现：open-code-review
│   ├── agent.json
│   └── config/
├── analyst/           # analyst 实现：复用 pi-web（只读工具 + JSON 拆卡）
│   └── agent.json
├── .gitignore
└── README.md          # 本文件

apps/core/
├── cmd/agent-pi/      # coder adapter 入口（Go）→ internal/agentcli.RunPi
├── cmd/agent-ocr/     # reviewer adapter 入口（Go）→ internal/agentcli.RunOCR
├── cmd/agent-analyst/ # analyst adapter 入口（Go）→ internal/agentcli.RunAnalyze
└── internal/agentcli/ # 协议 + 两个 adapter 实现 + 单测（含 ocr 输出映射）
```

## 4. Agent manifest（agent.json）

```jsonc
{
  "name": "pi",
  "role": "coder",                        // coder | reviewer
  "version": "1.0.0",
  "protocol": "cli",                      // cli | http
  "entrypoint": "/bin/agent-pi",          // adapter 可执行文件（容器内绝对路径）
  "container": "host",                    // Agent 运行环境（pi-web 宿主运行，systemd 用户单元）
  "workdir_prefix": "/home/wetee/work/cathome/workflow",  // 宿主仓库根路径
  "limits": { "timeout_sec": 7200, "max_files_per_turn": 50 },
  "permissions": { "write_scope": "worktree:apps/*" },
  "config": { "session_dir": "/home/wetee/.pi/agent" }
}
```

> adapter 由 **evolve-driver 容器内直接 exec**（宿主工具链经 `/host` 挂载：pi-web 走 HTTP，
> ocr CLI 在宿主 npm 全局装），因此 entrypoint 是 evolve-driver 镜像内的路径（`/bin/agent-*`）。
> manifest 落 `agents_registry` 表；**实际调用路径**取 env `CODER_ADAPTER_PATH` /
> `REVIEWER_ADAPTER_PATH`（默认 `/bin/agent-pi` / `/bin/agent-ocr`）。
>
> `limits.timeout_sec` 是 **exec 外层硬超时**（安全网），应略高于 adapter 自身内部预算：
> coder（pi）= 7200s，reviewer（ocr）= 4200s（ocr adapter 自身 `OCR_TIMEOUT_SEC` 默认 3600s）。
> 未配置时兜底 `defaultAgentTimeout` = 2h（旧值 25h 会让卡死任务阻塞近一天）。

## 5. 替换流程（不改任何业务代码）

1. 写新 `agent.json` + adapter（Go 二进制，或遵循 §2 协议的 `*.sh`）。
2. `agentctl register <path>` 注册。
3. `agentctl bind coder|reviewer <name>` 切换绑定。
4. 完成。运行时绑定存 `agent_bindings` 表，缺失时回退 env
   `AGENT_BINDING_CODER` / `AGENT_BINDING_REVIEWER`（默认 `pi` / `ocr`）。
   新 adapter 放进 evolve-driver 镜像（`deploy/Dockerfile.evolve`）后改 `*_ADAPTER_PATH` 即可。

## 6. 权限边界（与实现无关的硬约束）

- 无论绑定哪个 Agent，写权限一律限定在 **git worktree 内的 `apps/*`**。
- `internal/`、`cmd/`、`docs/` 的修改必须走人工 PR，Agent 无权直改（越界由 driver 的写范围校验打回）。
- 生产主仓库只读；进化流程在 `cache/evolve/<task_no>/` worktree 中进行。
- 所有 Agent 输出必须经 reviewer + 自动化测试 + 人工审批后方可上线（双环进化外环）。

## 7. 初始实现的安装与运行

```bash
# coder：pi（+ pi-web UI）——宿主运行（systemd 用户单元 pi-web.service）
# 安装：pi-web.sh install（npm 全局装 + 注册 systemd）；TLS 终结见 deploy/compose/core/nginx-pi.yml
# 访问：https://xiaobai.asyou.me:8443/（Basic Auth，用户名 pi）
apps/core/agents/pi/pi-web.sh install && apps/core/agents/pi/pi-web.sh start

# reviewer：open-code-review（宿主 npm 全局装，adapter 在 evolve-driver 容器内 exec）
# 配置：~/.opencodereview/config.json（provider）或 OCR_PROVIDER/OCR_API_KEY 环境变量
npm i -g @alibaba-group/open-code-review

# evolve-driver（含两个 adapter 二进制 /bin/agent-pi /bin/agent-ocr）
cd deploy && docker compose up -d --build evolve-driver

# headless 自测（adapter 在 evolve-driver 容器内跑；workdir 用容器路径 /repo/...）
( cd deploy && docker compose exec -T -e PI_WEB_URL=http://host.docker.internal:30141 -e PI_WEB_PASSWORD=$CODER_WEB_PASSWORD \
    -e PI_PROVIDER=local -e PI_MODEL=Qwen3.8-27B -e HOME=/host evolve-driver \
    /bin/agent-pi <<< '{"op":"fix","task":"给 apps/pycraper/main.py 加一行注释","workdir":"/repo","files":["apps/pycraper/main.py"]}' )
( cd deploy && docker compose exec -T -e OCR_BIN=/host/.npm-global/bin/ocr -e OCR_PROVIDER=computeflux -e HOME=/host \
    evolve-driver /bin/agent-ocr <<< '{"op":"review","workdir":"/repo","base":"main"}' )

# adapter 单测（离线，不调 LLM）
cd apps/core && go test ./internal/agentcli/
```

## 8. 替换候选

- coder：Claude Code、Codex、OpenCode 等任何可 headless 的编码 Agent。
- reviewer：自研 LLM reviewer 或其他评审工具。

换哪个都行——写新 manifest + adapter 即可，业务代码零改动。
