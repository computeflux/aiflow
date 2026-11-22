import { useState, useRef, useEffect, useCallback } from "react";
import {
    Bot,
    Send,
    Check,
    Loader2,
    Trash2,
    Plus,
    Edit3,
    ArrowLeftRight,
    ChevronsRight,
    ChevronsLeft,
} from "lucide-react";
import { authFetch, getApiBase, type Board } from "../api";
import { streamEndpoint } from "../ai-backend";
import { redirectToLogin, readStoredSession } from "../sso";

// 401 未登录标记：authFetch 已派发 auth:required（SsoGate 会跳登录），
// 这里用友好文案替代后端原始 "missing bearer token"，并提供「去登录」按钮。
const AUTH_ERROR = "__AUTH_REQUIRED__";
// 未登录提示文案：按工作项要求把「配置/登录过程」直接显示给用户
//（wi-1793584973-037f：输入消息后显示 missing bearer token，需告知如何配置）。
const AUTH_ERROR_MSG =
    "未登录或登录已过期。配置方法：点击右侧「去登录」按钮 → 在登录页（/ops/login）完成 web3auth 登录 → 自动返回本页，即可正常使用 AI 助手。";

// ---------------------------------------------------------------------------
// types
// ---------------------------------------------------------------------------

interface ToolCall {
    id: string;
    name: string;
    args: Record<string, any>;
    description: string;
    status: "pending" | "executed";
}

interface ThinkingStep {
    id: string;
    text: string;
    status: "running" | "done" | "error";
}

interface ChatMessage {
    role: "user" | "assistant";
    content: string;
    toolCalls?: ToolCall[];
    // 思考过程：只读工具循环（tool/tool_result）的实时状态，流式中渲染、完成后随消息持久化。
    thinking?: ThinkingStep[];
}

interface ChatSession {
    id: string;
    title: string;
    messages: ChatMessage[];
    createdAt: number;
    updatedAt: number;
}

// ---------------------------------------------------------------------------
// storage
// ---------------------------------------------------------------------------

function loadSessions(pageKey: string): ChatSession[] {
    try {
        const raw = localStorage.getItem(`ai-chats-${pageKey}`);
        return raw ? JSON.parse(raw) : [];
    } catch {
        return [];
    }
}

function saveSessions(pageKey: string, sessions: ChatSession[]) {
    localStorage.setItem(`ai-chats-${pageKey}`, JSON.stringify(sessions));
}

// ---------------------------------------------------------------------------
// board helpers
// ---------------------------------------------------------------------------

// buildBoardContext 已移除（2026-10-19）：看板上下文由后端注入（现为 ai-flow
// board 通道：workflow 工具集按需查询看板/卡片/仓库，客户端不送上下文）。
// （带预算的摘要 + 项目表 + AGENTS.md + docs 索引），客户端再送一份会让 prompt 翻倍。

// parseToolCalls 把后端返回的 tool_calls 转成前端结构（含人类可读描述）。
// 流式/非流式共用。
function parseToolCalls(rawCalls: any[] | undefined, board: Board | undefined): ToolCall[] {
    const out: ToolCall[] = [];
    for (const tc of rawCalls || []) {
        try {
            const args =
                typeof tc.function?.arguments === "string"
                    ? JSON.parse(tc.function.arguments)
                    : tc.function?.arguments;
            const t: ToolCall = {
                id: tc.id || String(Math.random()),
                name: tc.function?.name || "",
                args,
                description: "",
                status: "pending",
            };
            t.description = describeBoardToolCall(t, board);
            out.push(t);
        } catch {
            /* skip malformed */
        }
    }
    return out;
}

// describeReadToolCall 只读仓库工具的人类可读描述（流式过程中的状态提示）。
function describeReadToolCall(name: string, args: any): string {
    const path = args?.path || "";
    if (name === "read_file") return `读取 ${path}`;
    if (name === "list_dir") return `浏览 ${path || "仓库根目录"}`;
    if (name === "search_code")
        return `搜索 ${args?.pattern || ""}${path ? `（${path}）` : ""}`;
    // analyst Agent（pi-web）需求分析：可能跑几分钟，给一条明确的状态提示。
    if (name === "analyze_requirement")
        return "analyst（pi）正在分析需求并拆解任务卡…";
    return `${name} ${JSON.stringify(args ?? {})}`.slice(0, 80);
}

// parseCardsArg 解析 create_cards 的批量入参：cards_json 字符串 / cards 字符串 /
// cards 数组都认；解析失败返回空数组。
function parseCardsArg(args: any): any[] {
    for (const key of ["cards_json", "cards"]) {
        const v = args?.[key];
        if (v == null) continue;
        if (Array.isArray(v)) return v;
        if (typeof v === "string") {
            const s = v.trim().replace(/^```(?:json)?/, "").replace(/```$/, "");
            if (!s) continue;
            try {
                const parsed = JSON.parse(s);
                if (Array.isArray(parsed)) return parsed;
            } catch {
                /* 试下一个 key */
            }
        }
    }
    return [];
}

// parseIndexList 解析 depends_on_index（批次内 1-based 序号）。
function parseIndexList(v: any): number[] {
    const out: number[] = [];
    const push = (n: any) => {
        const x = Number(n);
        if (Number.isFinite(x) && x > 0) out.push(Math.trunc(x));
    };
    if (Array.isArray(v)) v.forEach(push);
    else if (typeof v === "number") push(v);
    else if (typeof v === "string") {
        const s = v.trim().replace(/^\[/, "").replace(/\]$/, "");
        if (s) s.split(/[,\s、;]+/).forEach((p) => push(p.replace(/^#/, "")));
    }
    return out;
}

function describeBoardToolCall(tc: ToolCall, board: Board | undefined): string {
    if (!board) return `${tc.name}: ${JSON.stringify(tc.args)}`;
    const listName = (id: number) =>
        board.lists?.find((l) => l.id === id)?.name || `list#${id}`;
    const cardTitle = (id: number) => {
        for (const l of board.lists || [])
            for (const c of l.cards || []) if (c.id === id) return c.title;
        return `card#${id}`;
    };
    switch (tc.name) {
        case "create_card":
            return `在「${listName(tc.args.list_id)}」中创建卡片「${tc.args.title}」`;
        case "create_cards": {
            // 批量建卡（2026-11-01）：参数是 cards_json 字符串（嵌套对象数组在本地模型上不可靠）
            const items = parseCardsArg(tc.args);
            const defaultList = tc.args?.list_id;
            if (items.length === 0) return "批量创建任务卡（未解析出卡片）";
            const lines = items
                .map((c: any, i: number) => {
                    const lid = c?.list_id || defaultList;
                    const h = c?.estimate_hours ? ` · ${c.estimate_hours}h` : "";
                    // 同批依赖：depends_on_index 是 1-based 序号（建卡后由后端回填成卡 ID）
                    const deps = parseIndexList(c?.depends_on_index);
                    const depNote = deps.length
                        ? ` · 需第 ${deps.join("、")} 张先完成`
                        : "";
                    return `  ${i + 1}. ${c?.title ?? "(无标题)"}${h}${depNote}${
                        lid ? ` → ${listName(lid)}` : ""
                    }`;
                })
                .join("\n");
            return `批量创建 ${items.length} 张任务卡：\n${lines}`;
        }
        case "update_card": {
            const fields = Object.entries(tc.args)
                .filter(([k]) => k !== "card_id")
                .map(([k, v]) => `  ${k}: ${v}`)
                .join("\n");
            return `修改「${cardTitle(tc.args.card_id)}」:\n${fields}`;
        }
        case "move_card":
            return `移动「${cardTitle(tc.args.card_id)}」到「${listName(tc.args.target_list_id)}」`;
        case "delete_card":
            return `删除「${cardTitle(tc.args.card_id)}」${tc.args.reason ? `(${tc.args.reason})` : ""}`;
        case "create_list":
            return `创建新列表「${tc.args.name}」`;
        default:
            return `${tc.name}: ${JSON.stringify(tc.args)}`;
    }
}

// ---------------------------------------------------------------------------
// 打开信号总线（2026-11-20）：页面工具栏「AI 助手」按钮与收起态 rail 都通过它
// 请求展开侧栏。定义在 AIPanel（被 AISidebar 包装层消费），避免 kit 内部循环
// 依赖；对外经 kit/index.ts 再导出，调用方从 @kit 导入。
// ---------------------------------------------------------------------------

/** 触发打开侧栏 AI 面板（页面工具栏「AI 助手」按钮 / 收起态 rail 用）。 */
export function requestAIPanelOpen() {
    openSeq.value += 1;
    for (const cb of openListeners) cb(openSeq.value);
}

const openSeq = { value: 0 };
const openListeners: Array<(seq: number) => void> = [];

/** 订阅打开信号（侧栏容器 AISidebar 用），返回退订函数。 */
export function subscribeAIPanelOpen(cb: (seq: number) => void): () => void {
    openListeners.push(cb);
    return () => {
        const i = openListeners.indexOf(cb);
        if (i >= 0) openListeners.splice(i, 1);
    };
}

// ---------------------------------------------------------------------------
// component
// ---------------------------------------------------------------------------

interface AIPanelProps {
    board?: Board;
    onRefresh?: () => void;
    mindMapJson?: () => string;
    onUpdateMap?: (data: any) => void;
    mindMapId?: string;
    layout?: string;
    theme?: string;
    // ---- 文档拆卡模式（2026-11-01）：复用看板流式管线，只是多带一份文档正文 ----
    // 2026-11-20：旧 board_ai 的 analyze/doc 字段已删除；ai-flow 链路下文档正文
    // 由 sendMessage 内联进 content（见「【文档拆卡 …】」前缀）。
    doc?: { path: string; content: string };
    // 默认落卡列（create_card.list_id）；不传则靠模型从看板摘要里选
    targetListId?: number;
    // 外部打开信号：每 +1 一次就打开面板（DocsPage 工具栏按钮用）
    openSignal?: number;
    // ---- 全局右侧 AI 侧栏（2026-11-19）：开合状态提升到侧栏容器 ----
    // dockOpen：受控展开/收起；onDockClose：面板头部 × 收起时回调
    dockOpen?: boolean;
    onDockClose?: () => void;
}

export function AIPanel({
    board,
    onRefresh,
    mindMapJson,
    onUpdateMap,
    mindMapId,
    doc,
    targetListId,
    openSignal,
    dockOpen,
    onDockClose,
}: AIPanelProps) {
    const mode = board ? ("board" as const) : ("mindmap" as const);
    // 会话按页面隔离；带文档时再按文档路径隔离，否则换一篇文档会串到上一个对话里
    const pageKey = board
        ? `board-${board.id}${doc ? `-doc-${doc.path}` : ""}`
        : `mindmap-${mindMapId || "default"}`;

    // 右侧常驻 dock：面板始终占位显示，collapsed 时收成右缘细栏（原「漂浮」FAB 形态已废弃）
    // 受控模式（全局侧栏 dockOpen）优先于内部 state；受控时 onDockClose
    // 负责收起（rail 展开由侧栏容器决定），非受控时回退内部 state
    const [openInner, setOpenInner] = useState(true);
    // 受控模式（全局侧栏 dockOpen）优先于内部 state；受控时 onDockClose
    // 负责收起（rail 展开由侧栏容器决定），非受控时回退内部 state
    const open = dockOpen ?? openInner;
    const setOpen = (v: boolean) => {
        if (dockOpen === undefined) setOpenInner(v);
        if (!v) onDockClose?.();
    };
    // 收起把手拖拽调宽（2026-11-20）：pointerdown 记录起始 ×/宽度，pointermove
    // 实时改宽（只收不扩：上限 = 展开宽 w-96 = 384px），pointerup 落定：
    // 拖到 ≤56px 收起成 rail，否则保持拖拽宽度（widthOverride）。
    const panelRef = useRef<HTMLDivElement | null>(null);
    const widthOverrideRef = useRef<number | null>(null);
    const [panelWidth, setPanelWidth] = useState<number | null>(null);
    const draggingRef = useRef(false);

    // 拖拽宽度持久化（2026-11-20）：刷新 / 切页后恢复上次的侧栏宽度；
    // 开合状态（open）的持久化在侧栏容器 AISidebar（受控方）里做。
    useEffect(() => {
        try {
            const w = Number(localStorage.getItem("ai-sidebar-width"));
            if (Number.isFinite(w) && w >= 56 && w <= 384) {
                widthOverrideRef.current = w;
                setPanelWidth(w);
            }
        } catch {
            /* localStorage 不可用 → 默认宽度 */
        }
    }, []);

    const onHandlePointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
        e.preventDefault();
        const startX = e.clientX;
        const startW = panelRef.current?.offsetWidth ?? 384;
        draggingRef.current = true;
        const onMove = (ev: PointerEvent) => {
            if (!draggingRef.current) return;
            const w = Math.max(48, Math.min(384, startW + (startX - ev.clientX)));
            widthOverrideRef.current = w;
            setPanelWidth(w);
        };
        const onUp = () => {
            draggingRef.current = false;
            window.removeEventListener("pointermove", onMove);
            window.removeEventListener("pointerup", onUp);
            const w = widthOverrideRef.current;
            if (w != null && w <= 56) {
                widthOverrideRef.current = null;
                setPanelWidth(null);
                setOpen(false);
            } else if (w != null) {
                try {
                    localStorage.setItem("ai-sidebar-width", String(w));
                } catch {
                    /* ignore */
                }
            }
        };
        window.addEventListener("pointermove", onMove);
        window.addEventListener("pointerup", onUp);
    };
    const [sessions, setSessions] = useState<ChatSession[]>([]);
    const [activeId, setActiveId] = useState<string | null>(null);
    const [input, setInput] = useState("");
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [isAuthError, setIsAuthError] = useState(false);
    const [executing, setExecuting] = useState(false);
    // 流式输出：逐 token 累加的临时文本（不落库，结束后再写入会话）
    const [streamText, setStreamText] = useState("");
    // 思考过程：只读工具循环的实时状态（tool/tool_result），流式中渲染、结束后随消息持久化。
    // 取代原来只保留单条、每次被覆盖的 streamNote，让用户能看到 AI 的中间对话。
    const [thinking, setThinking] = useState<ThinkingStep[]>([]);
    const bottomRef = useRef<HTMLDivElement>(null);
    // 进行中的请求：组件卸载 / 切页时 abort，避免后端继续生成、也避免
    // 卸载后 updateMessages 落到过期状态（切回来看不到结果）。
    const abortRef = useRef<AbortController | null>(null);

    // Load sessions on open; reset activeId when switching boards/mindmaps
    // so a session from the previous page doesn't leak into the new one.
    useEffect(() => {
        if (!open) return;
        const all = loadSessions(pageKey);
        setSessions(all);
        setActiveId((prev) =>
            all.length === 0
                ? null
                : all.some((s) => s.id === prev)
                  ? prev
                  : all[0].id,
        );
    }, [open, pageKey]);

    const active = sessions.find((s) => s.id === activeId) || null;
    const messages = active?.messages || [];

    // 外部打开信号（DocsPage 工具栏按钮 / 收起态 rail）：每 +1 一次就打开面板。
    // 用 ref 记住上一次的值，避免挂载时就自动打开。（原 openPanel 预填输入
    // 包装层已移除：initialInput 无任何调用方使用，且包装层会干扰受控开合。）
    const lastOpenSignal = useRef(openSignal ?? 0);
    useEffect(() => {
        const sig = openSignal ?? 0;
        if (sig > lastOpenSignal.current) setOpen(true);
        lastOpenSignal.current = sig;
        // setOpen 每次 render 都是新函数，不放进依赖（行为只跟 openSignal 变）
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [openSignal]);

    // 消息变化 → 滚到底部。用内容签名做依赖（原实现依赖派生数组 messages，每次 render 新引用，
    // 语义即“render 后滚动”；签名等价且只读）。scrollIntoView 只读 DOM（无 setState）。
    const messagesKey = messages.map((m) => `${m.role}:${m.content.length}:${m.content}`).join("|");
    useEffect(() => {
        bottomRef.current?.scrollIntoView({ behavior: "smooth" });
    }, [messagesKey]);

    // 组件卸载（切页 / 切换看板）时取消进行中的请求：
    // 否则后端继续生成、且卸载后 updateMessages 落到过期状态，切回来看不到结果。
    useEffect(() => () => {
        abortRef.current?.abort();
        abortRef.current = null;
    }, []);

    const persist = useCallback(
        (updated: ChatSession[]) => {
            setSessions(updated);
            saveSessions(pageKey, updated);
        },
        [pageKey],
    );

    const newChat = () => {
        const s: ChatSession = {
            id: Date.now().toString(),
            title: "新对话",
            messages: [],
            createdAt: Date.now(),
            updatedAt: Date.now(),
        };
        const updated = [s, ...sessions];
        persist(updated);
        setActiveId(s.id);
        // setShowList(false);
    };

    const deleteChat = (id: string) => {
        const updated = sessions.filter((s) => s.id !== id);
        persist(updated);
        if (activeId === id) setActiveId(updated[0]?.id || null);
    };

    const switchChat = (id: string) => {
        setActiveId(id);
    };

    const updateMessages = (msgs: ChatMessage[], title?: string, sessionId?: string) => {
        const sid = sessionId || activeId;
        if (!sid) return;
        const now = Date.now();
        const updated = sessions.map((s) => {
            if (s.id !== sid) return s;
            return {
                ...s,
                messages: msgs,
                updatedAt: now,
                title: title || s.title,
            };
        });
        if (!updated.some((s) => s.id === sid)) {
            // session was just created in this tick (not yet in `sessions`) — append it
            updated.unshift({
                id: sid,
                title: title || "新对话",
                messages: msgs,
                createdAt: now,
                updatedAt: now,
            });
        }
        persist(updated);
    };

    const sendMessage = async (retryText?: string) => {
        const text = (retryText || input).trim();
        if (!text) return;
        // 前置检查：本地无会话（从未登录 / 已登出）时不发请求，直接显示配置指引，
        // 避免用户看到后端原始 "missing bearer token"（wi-1793584973-037f）。
        if (!readStoredSession()) {
            setIsAuthError(true);
            setError(AUTH_ERROR_MSG);
            return;
        }
        let sessionId = activeId;
        if (!sessionId) {
            // Create a new session, then continue sending in it
            sessionId = Date.now().toString();
            const s: ChatSession = {
                id: sessionId,
                title: "新对话",
                messages: [],
                createdAt: Date.now(),
                updatedAt: Date.now(),
            };
            persist([s, ...sessions]);
            setActiveId(sessionId);
        }

        const userMsg: ChatMessage = { role: "user", content: text };
        const newMsgs = [...messages, userMsg];
        const autoTitle = messages.length === 0 ? text.slice(0, 30) : undefined;
        updateMessages(newMsgs, autoTitle, sessionId);
        setInput("");
        setLoading(true);
        setError(null);
        setIsAuthError(false);

        // 每个请求用独立 controller；取消上一个（切页 / 重发时）。
        const ac = new AbortController();
        abortRef.current = ac;

        // 2026-11-20：board 对话统一走 ai-flow 网关（/api/ai-flow/chat SSE），旧
        // workflow-api board_ai 链路（/boards/:id/ai-chat/stream）已删除，无回退。
        const run = async (): Promise<void> => {
        try {
            const apiBase = getApiBase();

            if (mode === "board") {
                // ai-flow board 通道：请求体 {board_id, session, user_id, content}；
                // session 用前端会话 id（board 级多轮上下文由 ai-flow 按 session_key
                // 维护，历史不随请求发送）。文档拆卡（带 doc 上下文）时把正文内联进 content。
                // Authorization 头携带用户 JWT，ai-flow 侧透传给 workflow 工具/落卡。
                const session = sessionId || Date.now().toString();
                const content = doc
                    ? `【文档拆卡 ${doc.path}｜目标列 ${targetListId ?? 0}】\n${doc.content}\n\n${text}`
                    : text;
                const res = await authFetch(
                    streamEndpoint(),
                    {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        signal: ac.signal,
                        body: JSON.stringify({
                            board_id: board!.id,
                            session,
                            user_id: readStoredSession()?.address ?? "0",
                            content,
                        }),
                    },
                );
                if (res.status === 401) {
                    // 未登录 / 登录过期：authFetch 已派发 auth:required（SsoGate 跳登录），
                    // 这里用友好文案替代后端原始 "missing bearer token"。
                    throw new Error(AUTH_ERROR);
                }
                if (!res.ok) {
                    // ai-flow / nginx 对失败给非流式响应（502/504 + {"error":"…"}）——展示 error 字段
                    const raw = await res.text();
                    let msg = raw || `AI chat failed: ${res.status}`;
                    try {
                        const parsed = JSON.parse(raw);
                        if (parsed?.error) msg = String(parsed.error);
                    } catch {
                        /* 非 JSON（网关 HTML 等）→ 原样展示 */
                    }
                    throw new Error(msg);
                }
                if (!res.body) throw new Error("当前浏览器不支持流式响应");

                setStreamText("");
                setThinking([]);
                let live = "";
                let finalContent: string | null = null;
                let finalCalls: ToolCall[] = [];
                let streamErr: string | null = null;

                // 思考过程累积：把每次 tool/tool_result 追加成一条带状态的历史步骤，
                // 而不是只保留单条（原 streamNote 每次被覆盖，AI 的中间对话一闪而过）。
                // 结束后随消息一起落库，用户能在对话里回看 AI 的推理过程。
                let thinking: ThinkingStep[] = [];
                const pushStep = (text: string, status: ThinkingStep["status"]) => {
                    const step: ThinkingStep = { id: String(Date.now() + Math.random()), text, status };
                    thinking = [...thinking, step];
                    setThinking(thinking);
                };

                const handleEvent = (ev: any) => {
                    switch (ev?.type) {
                        case "token":
                            live += ev.text || "";
                            setStreamText(live);
                            break;
                        case "tool":
                            pushStep(describeReadToolCall(ev.name, ev.args), "running");
                            break;
                        case "tool_result":
                            // 把最近一条 running 步骤标记为 done（或 error）
                            thinking = thinking.map((s, i) =>
                                i === thinking.length - 1 && s.status === "running"
                                    ? { ...s, status: ev.error ? "error" : "done" }
                                    : s,
                            );
                            setThinking(thinking);
                            break;
                        case "result":
                            finalContent = ev.content ?? live;
                            finalCalls = parseToolCalls(ev.tool_calls, board);
                            break;
                        case "error":
                            streamErr = String(ev.error || "AI 调用失败");
                            break;
                    }
                };

                // SSE 帧解析：以空行（\n\n）分隔，每帧取 data: 行
                const reader = res.body.getReader();
                const decoder = new TextDecoder();
                let buf = "";
                for (;;) {
                    const { done, value } = await reader.read();
                    if (done) break;
                    // 已取消（切页 / 卸载）：立即退出，不再处理后续帧
                    if (ac.signal.aborted) break;
                    buf += decoder.decode(value, { stream: true });
                    let idx = buf.indexOf("\n\n");
                    while (idx >= 0) {
                        const frame = buf.slice(0, idx);
                        buf = buf.slice(idx + 2);
                        for (const line of frame.split("\n")) {
                            if (!line.startsWith("data: ")) continue;
                            try {
                                handleEvent(JSON.parse(line.slice(6)));
                            } catch {
                                /* 跳过坏帧 */
                            }
                        }
                        idx = buf.indexOf("\n\n");
                    }
                }
                setStreamText("");
                if (streamErr) throw new Error(streamErr);

                // 已取消则不写库（切页后组件已卸载，写也是落到过期状态）。
                if (ac.signal.aborted) return;

                // 兜底：模型什么都没说（没文本也没工具调用）时，不要存一条空白消息
                // （2026-10-30 实测：面板上一片空白，用户以为“没反应”）。
                const finalText = (finalContent ?? live).trim();
                if (!finalText && finalCalls.length === 0) {
                    throw new Error(
                        "AI 没有返回内容。请换个问法，或指定具体模块/文件后重试。",
                    );
                }

                updateMessages([
                    ...newMsgs,
                    {
                        role: "assistant",
                        content: finalText,
                        toolCalls: finalCalls,
                        thinking,
                    },
                ], undefined, sessionId);
            } else {
                const mmJson = mindMapJson?.() || "{}";
                const res = await authFetch(`${apiBase}/mindmap-chat`, {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    signal: ac.signal,
                    body: JSON.stringify({
                        mindmap_json: mmJson,
                        message: text,
                    }),
                });
                if (res.status === 401) {
                    throw new Error(AUTH_ERROR);
                }
                if (!res.ok)
                    throw new Error(
                        (await res.text()) || `AI chat failed: ${res.status}`,
                    );
                const data = await res.json();
                if (data.mindmap_json && onUpdateMap) {
                    try {
                        onUpdateMap(JSON.parse(data.mindmap_json));
                    } catch {
                        /* ignore */
                    }
                }
                updateMessages([
                    ...newMsgs,
                    {
                        role: "assistant",
                        content: data.reply || "已更新思维导图",
                    },
                ], undefined, sessionId);
            }
        } catch (err) {
            // 主动取消（切页 / 卸载时 abort）：reader.read() 会抛 "Error in input stream"，
            // 这是预期内的干净退出，不算错误——不显示、不 rollback、不重填输入框。
            if (ac.signal.aborted) return;
            const msg = err instanceof Error ? err.message : String(err);
            const authErr = msg === AUTH_ERROR;
            setIsAuthError(authErr);
            setError(
                authErr ? AUTH_ERROR_MSG : msg,
            );
            setInput(text);
            updateMessages(messages, undefined, sessionId); // rollback
        } finally {
            setLoading(false);
            setThinking([]);
            if (abortRef.current === ac) abortRef.current = null;
        }
        };

        await run();
    };

    const executeAll = async () => {
        if (!active) return;
        const pending = (
            active.messages[active.messages.length - 1]?.toolCalls || []
        ).filter((t) => t.status === "pending");
        if (pending.length === 0) return;
        // 前置检查：本地无会话时不发请求，直接显示配置指引（与 sendMessage 一致）
        if (!readStoredSession()) {
            setIsAuthError(true);
            setError(AUTH_ERROR_MSG);
            return;
        }
        setExecuting(true);
        setError(null);
        setIsAuthError(false);
        try {
            const apiBase = getApiBase();
            const res = await authFetch(
                `${apiBase}/boards/${board!.id}/ai-execute`,
                {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({
                        tool_calls: pending.map((t) => ({
                            name: t.name,
                            arguments: t.args,
                        })),
                    }),
                },
            );
            if (res.status === 401) throw new Error(AUTH_ERROR);
            if (!res.ok) throw new Error(await res.text());
            onRefresh?.();
            const msgs = [...active.messages];
            const lastMsg = { ...msgs[msgs.length - 1] };
            lastMsg.toolCalls = lastMsg.toolCalls?.map((t) => ({
                ...t,
                status: "executed" as const,
            }));
            msgs[msgs.length - 1] = lastMsg;
            updateMessages(msgs);
        } catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            const authErr = msg === AUTH_ERROR;
            setIsAuthError(authErr);
            setError(authErr ? AUTH_ERROR_MSG : msg);
        } finally {
            setExecuting(false);
        }
    };

    const allExecuted = (tc: ToolCall[]) =>
        tc.every((t) => t.status === "executed");
    const hasPending = (tc: ToolCall[]) =>
        tc.some((t) => t.status === "pending");

    const toolIcon = (name: string) => {
        switch (name) {
            case "create_card":
                return <Plus className="h-3 w-3 text-(--success)" />;
            case "create_cards":
                return <Plus className="h-3 w-3 text-(--success)" />;
            case "update_card":
                return <Edit3 className="h-3 w-3 text-(--info)" />;
            case "move_card":
                return <ArrowLeftRight className="h-3 w-3 text-(--warning)" />;
            case "delete_card":
                return <Trash2 className="h-3 w-3 text-(--danger)" />;
            case "create_list":
                return <Plus className="h-3 w-3 text-(--purple)" />;
            default:
                return null;
        }
    };

    return (
        <>
            {/* 收起态 rail（2026-11-20）：脱离 .ai-sidebar-root（display:contents）
                包装层，直接 fixed 贴右缘（z-40）。包装层在 flex 容器下对子盒子的
                支持不稳（rail 渲染不出、点不动），fixed 定位不依赖父布局；
                侧栏收起后宿主右缘即视口右缘，rail 正好补位。 */}
            {!open && (
                <button
                    onClick={() => {
                        // 受控模式下 AIPanel 内部 setOpen(true) 是空操作（开合状态
                        // 在侧栏容器 AISidebar），必须走「打开信号」总线，由
                        // AISidebar 统一 setOpen(true)（2026-11-20 修复：rail 点击
                        // 无反应的根因）。
                        requestAIPanelOpen();
                    }}
                    title="展开 AI 助手"
                    className="fixed right-0 top-1/2 z-40 flex w-8 -translate-y-1/2 flex-col items-center gap-1 rounded-l border border-r-0 border-(--border-default) bg-(--bg-surface) py-3 text-(--text-muted) hover:text-white hover:bg-(--bg-elevated)"
                >
                    <ChevronsLeft className="h-4 w-4" />
                    <span
                        className="text-[10px] font-bold tracking-widest"
                        style={{ writingMode: "vertical-rl" }}
                    >
                        AI助手
                    </span>
                </button>
            )}

            {open && (
                <div
                    ref={panelRef}
                    className="relative shrink-0 self-stretch flex flex-col border-l border-(--border-default) bg-(--bg-surface)"
                    style={
                        panelWidth != null ? { width: panelWidth } : undefined
                    }
                >
                    {/* 左缘拖拽把手：向左拖收起到右缘（≤56px 落定收起），向右回扩 */}
                    <div
                        onPointerDown={onHandlePointerDown}
                        title="拖拽收起 / 展开 AI 侧栏"
                        className="absolute left-0 top-1/2 z-20 -translate-x-1/2 -translate-y-1/2 flex h-16 w-2 cursor-col-resize items-center justify-center rounded bg-(--border-default) hover:bg-(--accent)"
                    >
                        <ChevronsRight className="pointer-events-none h-3 w-3 text-(--text-muted)" />
                    </div>
                    {/* Header */}
                    <div className="p-3 border-b border-(--border-default) flex items-center justify-between shrink-0">
                        <div className="flex items-center gap-2">
                            <Bot className="h-4 w-4 text-white" />
                            <span className="text-xs font-bold uppercase">
                                AI 助手
                            </span>
                        </div>
                        <div className="flex items-center gap-1">
                            <button
                                onClick={newChat}
                                className="p-1 text-(--text-muted) hover:text-white"
                                title="新对话"
                            >
                                <Plus className="h-3.5 w-3.5" />
                            </button>
                            <button
                                onClick={() => setOpen(false)}
                                className="p-1 text-(--text-muted) hover:text-white"
                                title="收起到右侧"
                            >
                                <ChevronsRight className="h-4 w-4" />
                            </button>
                        </div>
                    </div>

                    {/* 文档拆卡模式：告诉用户 AI 正在看哪篇文档（否则聊天里提到「这篇」会很模糊） */}
                    {doc && (
                        <div className="px-3 py-1.5 border-b border-(--border-default) shrink-0 bg-(--bg-elevated)">
                            <div className="text-[10px] text-(--text-muted) truncate">
                                已附加文档：
                                <span className="font-mono text-(--text-secondary)">
                                    {doc.path}
                                </span>
                            </div>
                        </div>
                    )}

                    {/* Tabs */}
                    {sessions.length > 1 && (
                        <div className="flex overflow-x-auto shrink-0 border-b border-(--border-default)">
                            {sessions.map((s) => (
                                <button
                                    key={s.id}
                                    onClick={() => switchChat(s.id)}
                                    className={`group shrink-0 px-3 py-1.5 text-[10px] max-w-30 truncate border-r border-(--border-default) transition flex items-center gap-1 ${s.id === activeId ? "bg-(--bg-elevated) text-white" : "text-(--text-muted) hover:bg-(--bg-elevated) hover:text-(--text-secondary)"}`}
                                >
                                    <span className="truncate">{s.title}</span>
                                    <span
                                        onClick={(e) => {
                                            e.stopPropagation();
                                            deleteChat(s.id);
                                        }}
                                        className="opacity-0 group-hover:opacity-100 text-(--text-muted) hover:text-red-400 shrink-0"
                                    >
                                        &times;
                                    </span>
                                </button>
                            ))}
                        </div>
                    )}

                    {/* Messages */}
                    <div className="flex-1 overflow-y-auto p-3 space-y-3">
                        {!active && (
                            <div className="text-center py-8 text-(--text-muted) text-xs">
                                <Bot className="h-8 w-8 mx-auto mb-2 opacity-30" />
                                <p>
                                    点击 <Plus className="h-3 w-3 inline" />{" "}
                                    新建对话
                                </p>
                            </div>
                        )}
                        {active && messages.length === 0 && (
                            <div className="text-center py-8 text-(--text-muted) text-xs">
                                <Bot className="h-8 w-8 mx-auto mb-2 opacity-30" />
                                <p>
                                    我是你的
                                    {mode === "board" ? "看板" : "思维导图"}助手
                                </p>
                            </div>
                        )}
                        {messages.map((msg, mi) => (
                            <div key={mi}>
                                <div
                                    className={`text-xs max-w-[90%] ${msg.role === "user" ? "ml-auto bg-(--accent) text-(--accent-foreground) px-3 py-2" : "bg-(--bg-elevated) text-(--text-primary) px-3 py-2 border border-(--border-default)"}`}
                                >
                                    <div className="whitespace-pre-wrap">
                                        {msg.content}
                                    </div>
                                </div>
                                {msg.thinking && msg.thinking.length > 0 && (
                                    <div className="mt-2 ml-2 border-l-2 border-(--border-default) pl-2 space-y-1">
                                        {msg.thinking.map((step) => (
                                            <div
                                                key={step.id}
                                                className="flex items-start gap-1.5 text-[10px] text-(--text-secondary)"
                                            >
                                                <span className="shrink-0 mt-px">
                                                    {step.status === "running" ? (
                                                        <Loader2 className="h-3 w-3 animate-spin text-(--info)" />
                                                    ) : step.status === "error" ? (
                                                        "⚠"
                                                    ) : (
                                                        "✓"
                                                    )}
                                                </span>
                                                <span className="whitespace-pre-wrap">
                                                    {step.text}
                                                </span>
                                            </div>
                                        ))}
                                    </div>
                                )}
                                {msg.toolCalls && msg.toolCalls.length > 0 && (
                                    <div className="mt-2 ml-2 space-y-1">
                                        {msg.toolCalls.map((tc) => (
                                            <div
                                                key={tc.id}
                                                className={`border text-xs p-2 flex items-center gap-2 ${tc.status === "executed" ? "border-(--success) bg-(--success)/5" : "border-(--border-default) bg-(--bg-elevated)"}`}
                                            >
                                                <span className="shrink-0">
                                                    {toolIcon(tc.name)}
                                                </span>
                                                <div className="flex-1 min-w-0">
                                                    <div className="flex items-center gap-1.5">
                                                        <span className="font-bold text-(--text-secondary) text-[10px]">
                                                            {tc.name}
                                                        </span>
                                                        {tc.status ===
                                                            "executed" && (
                                                            <span className="text-(--success) text-[10px]">
                                                                ✓
                                                            </span>
                                                        )}
                                                    </div>
                                                    <div className="text-(--text-secondary) mt-0.5 whitespace-pre-wrap">
                                                        {tc.description}
                                                    </div>
                                                </div>
                                            </div>
                                        ))}
                                        {hasPending(msg.toolCalls) && (
                                            <button
                                                onClick={executeAll}
                                                disabled={executing}
                                                className="w-full mt-1 border border-white/20 text-white text-xs py-1.5 font-bold hover:bg-white hover:text-black transition flex items-center justify-center gap-1"
                                            >
                                                {executing ? (
                                                    <Loader2 className="h-3 w-3 animate-spin" />
                                                ) : (
                                                    <Check className="h-3 w-3" />
                                                )}
                                                执行全部操作
                                            </button>
                                        )}
                                        {allExecuted(msg.toolCalls) && (
                                            <div className="text-[10px] text-(--success) text-center">
                                                ✓ 全部已执行
                                            </div>
                                        )}
                                    </div>
                                )}
                            </div>
                        ))}
                        {streamText && (
                            <div className="text-xs max-w-[90%] bg-(--bg-elevated) text-(--text-primary) px-3 py-2 border border-(--border-default)">
                                <div className="whitespace-pre-wrap">
                                    {streamText}
                                    <span className="animate-pulse">▍</span>
                                </div>
                            </div>
                        )}
                        {thinking.length > 0 && (
                            <div className="ml-2 border-l-2 border-(--border-default) pl-2 space-y-1">
                                {thinking.map((step) => (
                                    <div
                                        key={step.id}
                                        className="flex items-start gap-1.5 text-[10px] text-(--text-secondary)"
                                    >
                                        <span className="shrink-0 mt-px">
                                            {step.status === "running" ? (
                                                <Loader2 className="h-3 w-3 animate-spin text-(--info)" />
                                            ) : step.status === "error" ? (
                                                "⚠"
                                            ) : (
                                                "✓"
                                            )}
                                        </span>
                                        <span className="whitespace-pre-wrap">
                                            {step.text}
                                        </span>
                                    </div>
                                ))}
                            </div>
                        )}
                        {loading &&
                            thinking.length === 0 && (
                                <div className="flex items-center gap-2 text-(--text-muted) text-xs px-3">
                                    <Loader2 className="h-3.5 w-3.5 animate-spin" />{" "}
                                    AI 正在思考...
                                </div>
                            )}
                        {error && (
                            <div className="text-xs text-(--danger) px-3 py-2 border border-(--danger)/20 bg-(--danger)/5 flex items-center justify-between gap-2">
                                <span className="flex-1">{error}</span>
                                {isAuthError ? (
                                    <button
                                        onClick={() => redirectToLogin()}
                                        className="text-white bg-(--danger) hover:opacity-90 border border-(--danger)/30 px-2 py-0.5 text-[10px] font-bold shrink-0"
                                    >
                                        去登录
                                    </button>
                                ) : (
                                    <button
                                        onClick={() => sendMessage(input)}
                                        className="text-(--danger) hover:text-white border border-(--danger)/30 px-2 py-0.5 text-[10px] font-bold shrink-0"
                                    >
                                        重试
                                    </button>
                                )}
                            </div>
                        )}
                        <div ref={bottomRef} />
                    </div>

                    {/* Input */}
                    {active && (
                        <div className="p-3 border-t border-(--border-default) shrink-0">
                            <div className="flex gap-2 items-end">
                                <textarea
                                    value={input}
                                    onChange={(e) => {
                                        setInput(e.target.value);
                                        e.target.style.height = "auto";
                                        e.target.style.height =
                                            Math.min(
                                                e.target.scrollHeight,
                                                120,
                                            ) + "px";
                                    }}
                                    onKeyDown={(e) => {
                                        if (e.key === "Enter" && !e.shiftKey) {
                                            e.preventDefault();
                                            sendMessage();
                                        }
                                    }}
                                    placeholder="输入你的需求..."
                                    disabled={loading}
                                    rows={1}
                                    className="flex-1 bg-(--bg-elevated) border border-(--border-default) px-3 py-1.5 text-sm focus:outline-none focus:border-white disabled:opacity-50 resize-none"
                                />
                                <button
                                    onClick={() => sendMessage()}
                                    disabled={loading || !input.trim()}
                                    className="bg-(--accent) text-(--accent-foreground) px-3 text-sm font-bold hover:bg-(--text-primary) disabled:opacity-30 self-stretch"
                                >
                                    <Send className="h-4 w-4" />
                                </button>
                            </div>
                        </div>
                    )}
                </div>
            )}
        </>
    );
}
