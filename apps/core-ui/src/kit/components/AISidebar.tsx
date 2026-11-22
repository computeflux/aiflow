import { useCallback, useEffect, useRef, useState } from "react";
import { AIPanel, subscribeAIPanelOpen } from "./AIPanel";
import { fetchBoard, fetchBoards, type Board } from "../api";

// ---------------------------------------------------------------------------
// AI 助手全局右侧边栏（2026-11-19）
// 目标形态：AI 助手不再是各页面内嵌/漂浮的面板，而是整个板块 Shell 右侧的固定
// 侧边栏（左侧导航 | 页面内容 | AI 侧栏）。每个板块 App 在宿主 flex 行末尾
// 挂载一次 <AISidebar />（.ai-sidebar-root，main.tsx 注入 display:contents），
// 作为 flex 子项常驻右侧；登录页等不挂载它的页面无侧栏，会话状态跨页保留。
//
// 页面通过 useAIPageContext 将自身上下文（看板 / 文档 / 思维导图）发布到侧栏：
// - BoardPage 发布选中看板；DocsPage 发布看板 + 当前文档；MindMapPage 发布导图数据。
// - 未发布上下文的页面（工作项中心 / 审批中心 / 项目等）：侧栏默认挂第一个看板，
//   作为通用 AI 助手。
// ---------------------------------------------------------------------------

export interface AIPageContext {
    board?: Board;
    onRefresh?: () => void;
    mindMapJson?: () => string;
    onUpdateMap?: (data: any) => void;
    mindMapId?: string;
    doc?: { path: string; content: string };
    targetListId?: number;
}

type AIPageContextEntry = AIPageContext & { _owner: object };

const AIPageContextStore = { current: null as AIPageContextEntry | null };

/** 页面发布自己的 AI 上下文；组件卸载后若上下文仍是自己发布的则清除。 */
export function useAIPageContext(ctx: AIPageContext) {
    const ownerRef = useRef<object>(undefined as unknown as object);
    if (!ownerRef.current) ownerRef.current = Symbol("ai-page") as unknown as object;
    const ctxRef = useRef(ctx);
    ctxRef.current = ctx;
    // 同步发布（render 期写入 store，订阅方同步重读）：
    // 不用 effect，避免与调用方页面（如 BoardPage）effect 间循环渲染。
    // entry 用内容签名复用同一对象引用，保证 store.getSnapshot 稳定（无循环渲染）。
    const sigRef = useRef<string>("");
    const entryRef = useRef<AIPageContextEntry | null>(null);
    const sig = `${ctx.board?.id ?? ""}|${ctx.mindMapId ?? ""}|${ctx.doc?.path ?? ""}|${ctx.targetListId ?? 0}`;
    if (sig !== sigRef.current || !entryRef.current) {
        sigRef.current = sig;
        entryRef.current = { ...ctx, _owner: ownerRef.current };
    }
    if (AIPageContextStore.current !== entryRef.current) {
        AIPageContextStore.current = entryRef.current;
        emitAIPageContext();
    }
    useEffect(() => {
        return () => {
            if (AIPageContextStore.current?._owner === ownerRef.current) {
                AIPageContextStore.current = null;
                emitAIPageContext();
            }
        };
    }, []);
}

// 上下文变更订阅：发布方 render 写入 store 后手动 emit（store 是可变对象，
// 无法自动感知）；侧栏侧用轮询兜底（见 AISidebar）。
type Listener = () => void;
const aiCtxListeners = new Set<Listener>();
export function subscribeAIPageContext(cb: Listener): () => void {
    aiCtxListeners.add(cb);
    return () => {
        aiCtxListeners.delete(cb);
    };
}
function emitAIPageContext() {
    for (const cb of aiCtxListeners) cb();
}

// 打开信号总线已下沉到 AIPanel（requestAIPanelOpen / subscribeAIPanelOpen，
// 2026-11-20：rail 渲染在 AIPanel 内部，信号定义在那侧，避免循环依赖）。

/** 侧栏内容：每个板块 App 在宿主 Shell flex 行末尾挂载一次（常驻右侧）。 */
export function AISidebar() {
    // 开合状态持久化（2026-11-20）：收起/展开落 localStorage，刷新后恢复
    // 上次形态（不再每次重新展开）。
    const [open, setOpen] = useState(() => {
        try {
            return localStorage.getItem("ai-sidebar-open") !== "0";
        } catch {
            return true;
        }
    });
    useEffect(() => {
        try {
            localStorage.setItem("ai-sidebar-open", open ? "1" : "0");
        } catch {
            /* localStorage 不可用 → 不持久化 */
        }
    }, [open]);
    const [sig, setSig] = useState(0);
    const [ctx, setCtx] = useState<AIPageContextEntry | null>(() => AIPageContextStore.current);

    // 打开信号（页面「AI 助手」按钮 / 收起态 rail 点击）
    useEffect(() => {
        const unsub = subscribeAIPanelOpen((seq) => {
            console.debug("[ai-sidebar] open signal", seq);
            setOpen(true);
            setSig(seq);
        });
        return unsub;
    }, []);

    // 页面上下文变化 → 同步快照（发布方 render 写入 store + emit；300ms 轮询兜底）
    useEffect(() => {
        const sync = () => setCtx(AIPageContextStore.current);
        const unsub = subscribeAIPageContext(sync);
        const iv = setInterval(sync, 300);
        return () => {
            unsub();
            clearInterval(iv);
        };
    }, []);

    // 默认上下文：当前页面未发布看板时，挂第一个看板，保证任何页面 AI 都可用
    const [fallbackBoard, setFallbackBoard] = useState<Board | null>(null);
    const refreshFallback = useCallback(() => {
        void fetchBoards()
            .then(({ boards }) => (boards[0] ? fetchBoard(boards[0].id) : null))
            .then((b) => b && setFallbackBoard(b))
            .catch(() => {});
    }, []);
    useEffect(() => {
        if (!ctx?.board) refreshFallback();
    }, [ctx?.board, refreshFallback]);

    const board = ctx?.board ?? fallbackBoard;
    const onRefresh = ctx?.onRefresh ?? refreshFallback;

    // .ai-sidebar-root：main.tsx 注入 display:contents，使本包装层不产生盒子，
    // AIPanel 直接作为宿主 flex 行末尾子项参与布局。
    return (
        <div className="ai-sidebar-root">
            <AIPanel
                board={board ?? undefined}
                onRefresh={onRefresh}
                mindMapJson={ctx?.mindMapJson}
                onUpdateMap={ctx?.onUpdateMap}
                mindMapId={ctx?.mindMapId}
                doc={ctx?.doc}
                targetListId={ctx?.targetListId}
                dockOpen={open}
                onDockClose={() => setOpen(false)}
                openSignal={sig}
            />
        </div>
    );
}
