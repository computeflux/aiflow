import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
    Loader2,
    Plus,
    ChevronLeft,
    ChevronRight,
    Columns,
    ListChecks,
    Trash2,
    Zap,
    History,
    GitPullRequest,
    CheckCircle2,
    XCircle,
    Play,
    RotateCcw,
    FolderOpen,
} from "lucide-react";
import {
    fetchBoards,
    createBoard,
    fetchBoard,
    deleteBoard,
    renameBoard,
    createCard,
    deleteList,
    updateCard,
    finishCard,
    deleteCard,
    fetchCardComments,
    fetchCardExecutions,
    triggerWorkTriage,
    fetchProjects,
    approveCardApproval,
    rejectCardApproval,
    rerunCard,
    type Board,
    type BoardCard,
    type BoardComment,
    type CardExecution,
    type ExecutionEvent,
} from "../api";
import { fmtTime, worktreeEditorUrl } from "../utils";
import { ConfirmModal, useAIPageContext } from "@kit";

// pi-web（coder 编码代理 UI）：执行记录里点「查看 coder 会话」→ 打开对应 session 的修改过程。
// 域名走 HTTPS（nginx-pi TLS 终结 + Basic Auth），与迁移前宿主 nginx 行为一致。
const PI_WEB_BASE = "https://xiaobai.asyou.me:8443";

// S9：已关联工作项的卡，其状态是流水线状态的实时镜像（work-dispatch 每 5min 同步）
const PIPELINE_BADGE: Record<string, { label: string; cls: string }> = {
    todo: { label: "候选", cls: "bg-zinc-500/20 text-zinc-300" },
    ready_for_evolve: { label: "排队中", cls: "bg-blue-500/20 text-blue-300" },
    in_progress: { label: "执行中", cls: "bg-violet-500/20 text-violet-300" },
    in_review: { label: "审核中", cls: "bg-amber-500/20 text-amber-300" },
    done: { label: "已完成", cls: "bg-green-500/20 text-green-300" },
};

// S9 执行记录（卡片弹窗）：把 audit_logs 阶段事件渲染为可读时间线（成功失败都可直观查看）
const EXEC_EVENT_LABELS: Record<string, string> = {
    work_item_feedback: "状态迁移",
    coder_start: "编码开始",
    coder_done: "编码完成",
    reviewer_done: "审核完成",
    sandbox_done: "沙箱测试",
    approval_created: "建审批单",
    task_cancelled: "终止",
};
function execEventLabel(action: string): string {
    if (EXEC_EVENT_LABELS[action]) return EXEC_EVENT_LABELS[action];
    if (action.startsWith("task_status_")) return "阶段 " + action.slice("task_status_".length);
    return action;
}
function parseDetail(detail: string): Record<string, any> | null {
    if (!detail) return null;
    try {
        const v = JSON.parse(detail);
        return v && typeof v === "object" ? v : null;
    } catch {
        return null;
    }
}
function execEventSummary(e: ExecutionEvent): string {
    const d = parseDetail(e.detail);
    if (!d) return e.note || "";
    switch (e.action) {
        case "work_item_feedback":
            return `${d.from ?? ""} → ${d.to ?? ""}${d.task_status ? `（进化单 ${d.task_status}）` : ""}`;
        case "coder_done":
            return `agent=${d.agent ?? "-"} 改动 ${Array.isArray(d.files) ? d.files.length : 0} 个文件`;
        case "reviewer_done":
            return `评论 ${d.comments ?? 0} 条`;
        case "sandbox_done":
            return `exit=${d.exit_code ?? "?"} 耗时 ${d.duration_ms ?? "?"}ms 镜像 ${d.image ?? ""}`;
        case "approval_created":
            return `版本 v${d.version ?? "?"} 待审批`;
        case "task_cancelled":
            return `${d.reason ?? ""}`;
    }
    const keys = Object.keys(d);
    return keys.length ? JSON.stringify(d).slice(0, 140) : e.note || "";
}
function execOutcome(ex: CardExecution): { label: string; cls: string } {
    if (ex.status === "shipped") return { label: "✓ 已上线", cls: "text-(--ok)" };
    if (["rejected", "expired"].includes(ex.status)) return { label: "✗ 失败", cls: "text-(--danger)" };
    // 已上线（released）但还没合入 main：阻塞态（看板上不能显示成完成），直到合入成功
    if (ex.task_status === "released" && !ex.merge_commit) {
        if (ex.task_merge_state === "paused_base_dirty") {
            return { label: "🚧 阻塞：等待合入（相关仓库有未提交改动）", cls: "text-(--danger)" };
        }
        return { label: "🚧 等待合入 main", cls: "text-(--warning)" };
    }
    return { label: "⏳ 进行中", cls: "text-(--warning)" };
}

// 空迁移（from == to，如 in_review→in_review）不展示：修复前的回流会每轮刷一条（历史数据仍在 audit_logs）
function isNoopTransition(e: ExecutionEvent): boolean {
    if (e.action !== "work_item_feedback") return false;
    const d = parseDetail(e.detail);
    return !!d && d.from === d.to;
}

// PR / diff 查看器：分支只在服务端本地（进化链绝不 push），团队成员点击后自行推分支建 PR
interface PRView {
    title: string;
    workItemNo: string;
    branch: string;
    pushCmd: string;
    compareUrl: string;
    diff: string;
}
function diffLineClass(l: string): string {
    if (l.startsWith("diff --git") || l.startsWith("index ") || l.startsWith("+++") || l.startsWith("---"))
        return "text-(--text-muted) font-bold";
    if (l.startsWith("@@")) return "text-(--info)";
    if (l.startsWith("+")) return "text-(--ok)";
    if (l.startsWith("-")) return "text-(--danger)";
    return "text-(--text-secondary)";
}

// reviewer 审核记录查看器（2026-10-29）：执行记录里点「审核记录」→ 查看该轮 reviewer 的完整评论。
// 后端 /cards/:id/executions 已返回 task_review（reviewer 原始 JSON），此前前端没有入口，
// 只能在事件时间线里看到「审核完成」四个字，看不到到底审了什么。
interface ReviewComment {
    file?: string;
    line?: number;
    level?: string;
    message?: string;
}
interface ReviewView {
    title: string;
    workItemNo: string;
    taskNo: string;
    /** 该轮审核完成时间（事件时间）；用于弹窗标题区分是第几轮 */
    at?: string;
    ok?: boolean;
    comments: ReviewComment[];
    raw: string;
    /** 输出符合 reviewer 协议（能读到 ok/comments）：false 时弹窗回退展示原文（旧数据/异常输出） */
    structured: boolean;
    /** 本轮 ocr 会话 ID（reviewer_done detail.sessions，2026-11-11 起）：审查过程页深链用 */
    sessions?: string[];
}
// parseReview 解析 reviewer 输出（{"ok":bool,"comments":[{file,line,level,message}],"sessions":[...]}）。
// structured=false（非 JSON / 非对象 / 无协议字段）→ 由调用方回退展示原文。
function parseReview(raw: string): { ok?: boolean; comments: ReviewComment[]; sessions?: string[]; structured: boolean } {
    if (!raw) return { comments: [], structured: false };
    try {
        const v = JSON.parse(raw);
        if (v && typeof v === "object" && !Array.isArray(v)) {
            return {
                ok: typeof v.ok === "boolean" ? v.ok : undefined,
                comments: Array.isArray(v.comments) ? v.comments : [],
                sessions: Array.isArray(v.sessions) ? (v.sessions as string[]) : undefined,
                structured: typeof v.ok === "boolean" || Array.isArray(v.comments),
            };
        }
    } catch {
        /* 非 JSON → 原文展示 */
    }
    return { comments: [], structured: false };
}
// hasReview 是否真有 reviewer 记录：MySQL JSON 列的空值会读成字面量 "null"，
// 不能只看非空串，否则每条执行记录都会多出一个点开只有 null 的按钮。
function hasReview(raw: string): boolean {
    const t = (raw || "").trim();
    return t !== "" && t !== "null" && t !== "{}" && t !== "[]";
}
// reviewFromEvent 从「审核完成」事件里取出**该轮** reviewer 的完整记录。
// driver 在 detail.review 存原始 JSON（2026-10-29 起）。
// 旧数据事件里只有 comments 条数、没存内容 → 最后一轮可回退到 ex.task_review：
// driver 每轮覆盖写 task.Review，最后一轮的值恰好等于最后一条 reviewer_done 的那轮。
function reviewFromEvent(e: ExecutionEvent, ex: CardExecution, isLastRound: boolean): ReviewView | null {
    if (e.action !== "reviewer_done") return null;
    const d = parseDetail(e.detail);
    let raw = typeof d?.review === "string" ? d.review : "";
    if (!hasReview(raw) && isLastRound) raw = ex.task_review;
    if (!hasReview(raw)) return null;
    const parsed = parseReview(raw);
    return {
        title: ex.title,
        workItemNo: ex.work_item_no,
        taskNo: ex.evolve_task_no,
        at: e.at,
        ok: parsed.ok,
        comments: parsed.comments,
        raw,
        structured: parsed.structured,
        sessions: Array.isArray(d?.sessions) ? (d!.sessions as string[]) : parsed.sessions,
    };
}
function reviewLevelCls(level?: string): string {
    switch ((level || "").toLowerCase()) {
        case "error":
            return "text-(--danger)";
        case "warning":
        case "warn":
            return "text-(--warning)";
        case "info":
            return "text-(--info)";
        default:
            return "text-(--text-secondary)";
    }
}

function fmtDate(d: Date): string {
    return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
}
function startOfWeek(d: Date): Date {
    const day = d.getDay();
    const diff = d.getDate() - day + (day === 0 ? -6 : 1);
    const m = new Date(d);
    m.setDate(diff);
    m.setHours(0, 0, 0, 0);
    return m;
}
function addDays(d: Date, n: number): Date {
    const r = new Date(d);
    r.setDate(r.getDate() + n);
    return r;
}
function isSameDay(a: Date, b: Date): boolean {
    return (
        a.getFullYear() === b.getFullYear() &&
        a.getMonth() === b.getMonth() &&
        a.getDate() === b.getDate()
    );
}

export function BoardPage({ initialBoardName }: { initialBoardName?: string } = {}) {
    const [boards, setBoards] = useState<Board[]>([]);
    const [selected, setSelected] = useState<Board | null>(null);
    // 轮询会每 5s 替换 selected 对象：用 ref 读取“当前看板”，避免 loadBoards 依赖 selected
    // 导致列表接口/loading 被反复触发（首次自动选中只需 initialPickedRef 做幂等）。
    const selectedRef = useRef<Board | null>(null);
    useEffect(() => {
        selectedRef.current = selected;
    }, [selected]);
    const [loading, setLoading] = useState(false);
    // S9 §7.4 重设：Bug 看板的手工分诊按钮（仅 Bug 看板显示）
    const [triaging, setTriaging] = useState(false);
    const [triageMsg, setTriageMsg] = useState<string | null>(null);
    const initialBoardRef = useRef(initialBoardName ?? null);
    const initialPickedRef = useRef(false); // 初始看板只自动选中一次（防 loadBoards 重建后重复选中）
  const [error, setError] = useState<string | null>(null);
    const [deleteConfirm, setDeleteConfirm] = useState<{type:string,name:string,id:number} | null>(null);
    const [deleteListConfirm, setDeleteListConfirm] = useState<{id:number,name:string} | null>(null);
    const [newName, setNewName] = useState("");
    const [creating, setCreating] = useState(false);
    const [editingCard, setEditingCard] = useState<BoardCard | null>(null);
    const [creatingCardListId, setCreatingCardListId] = useState<number | null>(
        null,
    );
    const [editTitle, setEditTitle] = useState("");
    const [editDesc, setEditDesc] = useState("");
    const [editLabels, setEditLabels] = useState("");
    const [editTargetName, setEditTargetName] = useState("");
    // 显式前置卡（逗号分隔的卡 id）：未合入主分支前本卡不可开始
    const [editDependsOn, setEditDependsOn] = useState("");
    const [editAssignee, setEditAssignee] = useState("");
    const [editDueDate, setEditDueDate] = useState("");
    const [editPriority, setEditPriority] = useState("medium");
    const [editCoverColor, setEditCoverColor] = useState("");
    const [editChecklist, setEditChecklist] = useState<
        { text: string; done: boolean }[]
    >([]);
    // S9.8 §7.10：目标项目选项（只按项目识别：core 系置顶 + 启用项目，支持多选；不再识别技能）
    const [targetOptions, setTargetOptions] = useState<{ name: string }[]>([]);
    // S9.8 §7.10：目标多选切换（target_name 存逗号分隔串；主目标 = 第一个）
    const toggleTargetName = (name: string) => {
        const cur = editTargetName
            .split(",")
            .map((x) => x.trim())
            .filter((x) => x !== "");
        setEditTargetName(
            cur.includes(name)
                ? cur.filter((x) => x !== name).join(",")
                : [...cur, name].join(","),
        );
    };
    const [newCheckItem, setNewCheckItem] = useState("");
    const [_cardComments, setCardComments] = useState<BoardComment[]>([]);
    const [_newComment, setNewComment] = useState("");
    const [_commentAuthor, _setCommentAuthor] = useState("");
    // S9 执行记录（卡片弹窗内展示：每次尝试 = 一个工作项 = 一轮 S1 链路）
    const [executions, setExecutions] = useState<CardExecution[] | null>(null);
    const [execLoading, setExecLoading] = useState(false);
    // S9 PR：diff 查看器（卡面 PR 小徽与执行记录内按钮共用）
    const [prView, setPrView] = useState<PRView | null>(null);
    const [prCopied, setPrCopied] = useState(false);
    // S9 reviewer：审核记录查看器（执行记录内「审核记录」按钮）
    const [reviewView, setReviewView] = useState<ReviewView | null>(null);

    // Context menu
    const [ctxMenu, setCtxMenu] = useState<{
        x: number;
        y: number;
        board: Board;
    } | null>(null);
    const [renamingBoard, setRenamingBoard] = useState<Board | null>(null);
    const [renameName, setRenameName] = useState("");

    // Time filter state
    const [timeMode, setTimeMode] = useState<"all" | "day" | "week" | "month">(
        "all",
    );
    const [timeOffset, setTimeOffset] = useState(0);
    // 惰性初始化：避免 render 期调用 new Date()（purity）
    const [monthYear, setMonthYear] = useState(() => ({
        y: new Date().getFullYear(),
        m: new Date().getMonth(),
    }));
    const [dayDate, setDayDate] = useState(() => new Date());

    // 全局右侧 AI 侧栏：发布选中看板作为侧栏上下文（会话按看板隔离）
    useAIPageContext({
        board: selected ?? undefined,
        onRefresh: () => {
            if (selectedRef.current) void loadBoard(selectedRef.current.id);
        },
    });

    const loadBoard = useCallback(async (id: number) => {
        setLoading(true);
        try {
            setSelected(await fetchBoard(id));
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setLoading(false);
        }
    }, []);

    // 静默刷新（轮询用）：不置 loading、不弹错误（网络抖动下一轮再试），避免打断交互
    const refreshBoardSilently = useCallback(async (id: number) => {
        try {
            setSelected(await fetchBoard(id));
        } catch {
            /* 静默：下一轮再试 */
        }
    }, []);

    // Auto-select first board on load（在取数回调内完成，避免 effect 里 setState）
    const loadBoards = useCallback(async () => {
        setLoading(true);
        try {
            const all = (await fetchBoards()).boards;
            setBoards(all);
            // 首次加载且无初始看板指定 → 自动选中第一个
            if (!selectedRef.current && !initialPickedRef.current && all.length > 0) {
                initialPickedRef.current = true;
                void loadBoard(all[0].id);
            }
            // 指定初始看板（如 /bugs → Bug 看板）：首次加载后自动选中（仅一次）
            if (initialBoardRef.current && !initialPickedRef.current && !selectedRef.current) {
                const target = all.find((b) => b.name.toLowerCase() === initialBoardRef.current!.toLowerCase());
                if (target) {
                    initialPickedRef.current = true;
                    void loadBoard(target.id);
                }
            }
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setLoading(false);
        }
    }, [loadBoard]);

    // S9 §7.4 重设：手动跑一轮错误日志分诊（仅 Bug 看板）：分诊产出 Bug 卡（Backlog 列）。
    const isBugBoard = selected?.name?.toLowerCase() === "bug";
    const runTriage = useCallback(async () => {
        setTriaging(true);
        setTriageMsg(null);
        try {
            const res = await triggerWorkTriage();
            setTriageMsg(`分诊完成：扫描 ${res?.scanned ?? 0}，建卡 ${res?.created ?? 0}，归并 ${res?.merged ?? 0}`);
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setTriageMsg(err instanceof Error ? err.message : String(err));
        } finally {
            setTriaging(false);
        }
    }, [selected, loadBoard]);

    // S9 看板为核心：拖到 **Todo** = 交给工作流（建/重开工作项入队，等调度器领取）；
    // 拖到 **In Progress** = 立即执行（入队 + 立即领取）。
    const handleHandOff = useCallback(async (card: BoardCard, targetListName = "Todo") => {
        const dest = selected?.lists?.find((l) => l.name === targetListName);
        if (!dest || !selected) return;
        const maxPos = Math.max(0, ...(dest.cards || []).map((c) => c.position));
        try {
            // 拖到 In Progress = 立即执行：后端会调 LLM 检查前置依赖（未完成 → 409 阻止），给足超时
            await updateCard(
                card.id,
                { list_id: dest.id, position: maxPos + 1 },
                targetListName === "In Progress" ? 120_000 : 10_000,
            );
            await loadBoard(selected.id);
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    }, [selected, loadBoard]);

    // 首帧取数：走 useAsync 的 immediate reload（不在 effect 里直接 setState）
    const boardsInitRef = useRef(false);
    useEffect(() => {
        if (boardsInitRef.current) return;
        boardsInitRef.current = true;
        void loadBoards();
    }, [loadBoards]);

    // S9.8 §7.10：加载目标项目下拉选项（只按项目识别，技能不参与）
    useEffect(() => {
        let cancelled = false;
        (async () => {
            try {
                const projects = await fetchProjects();
                if (cancelled) return;
                // 2026-10-18 起不再特判 core 系：项目一律默认启用，active 是纯启停开关
                // （自动执行的把关在审批 + 合并 PR，不在“默认停用”）。
                const opts: { name: string }[] = projects
                    .filter((p) => p.active)
                    .map((p) => ({ name: p.name }));
                setTargetOptions(opts);
            } catch {
                // 加载失败不阻断（下拉为空，仍可手填）
            }
        })();
        return () => {
            cancelled = true;
        };
    }, []);

    // Filter cards by time range
    // 「本周」基准取当前时间：render 期取时是展示派生（周视图语义），非副作用
    // eslint-disable-next-line react-hooks/purity
    const weekStart = useMemo(() => addDays(startOfWeek(new Date()), timeOffset * 7), [timeOffset]);
    const cardsInRange = (cards: BoardCard[]) => {
        if (timeMode === "all") return cards;
        if (timeMode === "day") {
            return cards.filter(
                (c) => c.due_date && isSameDay(new Date(c.due_date), dayDate),
            );
        }
        if (timeMode === "week") {
            const ws = weekStart;
            const we = addDays(ws, 6);
            return cards.filter((c) => {
                if (!c.due_date) return false;
                const d = new Date(c.due_date);
                return d >= ws && d <= we;
            });
        }
        if (timeMode === "month") {
            return cards.filter((c) => {
                if (!c.due_date) return false;
                const d = new Date(c.due_date);
                return (
                    d.getFullYear() === monthYear.y &&
                    d.getMonth() === monthYear.m
                );
            });
        }
        return cards;
    };

    const handleCreate = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!newName.trim()) return;
        setCreating(true);
        try {
            await createBoard(newName.trim(), "week");
            setNewName("");
            await loadBoards();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setCreating(false);
        }
    };

    // S9 看板直接审批（Review 列）：审批通过 / 驳回 → 走 S1 审批链（卡随工作项状态自动移到 Done / Backlog）。
    const [decidingCard, setDecidingCard] = useState<number | null>(null);
    const decideCard = async (card: BoardCard, decision: "approve" | "reject") => {
        if (decidingCard !== null) return;
        setDecidingCard(card.id);
        try {
            if (decision === "approve") {
                // 后端：有待审批单 → 走审批链；没有（「完成执行」推进 Review 的卡）→ 置 released 等合入。
                // 红线：代码必须合并到 main 之后才会进 Done，所以这里可能只返回「已放行」。
                const res = await approveCardApproval(card.id);
                if (res?.note) setRerunMsg({ kind: "ok", text: res.note });
            } else {
                await rejectCardApproval(card.id);
            }
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setDecidingCard(null);
        }
    };

    // S9 失败重跑（2026-10-18，看板按钮）：续接执行 = 复用上一轮进化单（同 worktree + 同 coder 会话
    // + 已积累的代码改动）；重新执行 = 新工作项从零开始（干净 worktree + 全新会话）。
    const [rerunActing, setRerunActing] = useState<null | "resume" | "restart" | "review">(null);
    const [rerunMsg, setRerunMsg] = useState<{ kind: "ok" | "err"; text: string } | null>(null);
    const rerunCurrentCard = async (mode: "resume" | "restart") => {
        const card = editingCard;
        if (!card?.id || rerunActing) return;
        setRerunActing(mode);
        setRerunMsg(null);
        try {
            const res = await rerunCard(card.id, mode);
            setRerunMsg({
                kind: "ok",
                text:
                    res?.note ||
                    (mode === "resume" ? "已续接上一轮执行" : "已重新入队（从零开始）"),
            });
            // 刷新执行记录（resume 复用旧工作项，restart 会多出一条新工作项）
            setExecLoading(true);
            try {
                setExecutions(await fetchCardExecutions(card.id));
            } catch {
                /* 保留旧记录 */
            } finally {
                setExecLoading(false);
            }
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setRerunMsg({
                kind: "err",
                text: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setRerunActing(null);
        }
    };

    // S9 看板拖拽 = 触发操作框（不再"拖一下就改状态"）：拖到某列 → 弹该阶段迁移的**特定操作框**，
    // 用户完成操作（交给工作流/审批/驳回/验收/收回）后卡才真正移动；同列拖动 / 无对应操作 → 不弹。
    const [dragCard, setDragCard] = useState<{ card: BoardCard; fromList: string } | null>(null);
    const [moveDialog, setMoveDialog] = useState<{ card: BoardCard; fromList: string; toList: string } | null>(null);
    const [moveActing, setMoveActing] = useState(false);

    // 看板状态实时刷新：5s 静默轮询（页面可见、且未在拖拽/弹窗/编辑时才刷）。
    // 后端 GET /boards/:id 会按 evolve_task 实时投影卡状态与列，所以轮询能秒级看到
    // 「执行中 → 审核中 → 完成」，不用等 work-dispatch 的 5min 镜像轮。
    const selectedBoardId = selected?.id;
    useEffect(() => {
        if (!selectedBoardId) return;
        const timer = window.setInterval(() => {
            if (document.visibilityState !== "visible") return;
            if (dragCard || moveDialog || editingCard || creatingCardListId) return;
            void refreshBoardSilently(selectedBoardId);
        }, 5000);
        return () => window.clearInterval(timer);
    }, [selectedBoardId, dragCard, moveDialog, editingCard, creatingCardListId, refreshBoardSilently]);

    const handleDragStart = (card: BoardCard, listName: string) => {
        setDragCard({ card, fromList: listName });
    };

    const handleDragOver = (e: React.DragEvent) => {
        e.preventDefault(); // 允许放置
    };

    const handleDrop = (e: React.DragEvent, listName: string) => {
        e.preventDefault();
        const dc = dragCard;
        setDragCard(null);
        if (!dc || dc.fromList === listName) return; // 同列拖动 → 不操作
        if (!getMoveAction(dc.card, dc.fromList, listName)) return; // 无对应操作 → 不弹
        setMoveDialog({ card: dc.card, fromList: dc.fromList, toList: listName });
    };

    // moveCardToList：拖拽 = 对工作流下的命令（后端按目标列名翻译为工作项状态机迁移）。
    const moveCardToList = async (card: BoardCard, listName: string) => {
        const dest = selected?.lists?.find((l) => l.name === listName);
        if (!dest) return;
        const maxPos = Math.max(0, ...(dest.cards || []).map((c) => c.position));
        try {
            await updateCard(card.id, { list_id: dest.id, position: maxPos + 1 });
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    // 人工结束在跑任务（拖 In Progress → Done/Backlog 走这里：状态机不允许 in_progress 直接跳
    // shipped/open，必须由后端按合法阶梯收口并停掉进化单）
    const finishRunningCard = async (card: BoardCard, outcome: "done" | "reject") => {
        try {
            await finishCard(card.id, outcome);
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    // 「跳过审核」按钮已移除（2026-11-03）：与「完成执行」重复 —— 两者都是把卡交给人看 diff，
    // 但「完成执行」更轻（不重跑沙箱/CI）、适用面更广（含已驳回的卡）。
    // 后端 skip_review 能力保留（供以后按 diff 体积自动跳过 reviewer 用）。

    // 续接执行：复用同一个 worktree + coder 会话 + 已积累 diff（环境性失败/审核打回适用）。
    const resumeRunningCard = async (card: BoardCard) => {
        try {
            await rerunCard(card.id, "resume");
            setRerunMsg({ kind: "ok", text: "已续接上一轮：复用工作目录与对话，driver 下一轮继续" });
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setRerunMsg({ kind: "err", text: err instanceof Error ? err.message : String(err) });
        }
    };

    // 完成执行（2026-11-03）：停掉当前任务，工作项直接推成 in_review → 卡进 Review 列，
    // 由人工看 diff。用于卡住/失败但代码可看的情况（不必等流水线自己跑到 approving）。
    const finishToReview = async (card: BoardCard) => {
        setRerunActing("review");
        try {
            await finishCard(card.id, "review");
            setRerunMsg({ kind: "ok", text: "已停在 Review 列：任务已停止，请检查改动后审批或驳回" });
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setRerunMsg({ kind: "err", text: err instanceof Error ? err.message : String(err) });
        } finally {
            setRerunActing(null);
        }
    };

    // 阶段迁移 → 特定操作（拖到某列弹什么操作框）。列语义（2026-10-28）：
    //   Backlog     = 候选任务（未入队 / 失败回收）
    //   Todo        = 待执行（已入队，等调度器领取）
    //   In Progress = 正在执行
    //   Review      = 正在审核（流水线自动进入）
    //   Done        = 完成
    const getMoveAction = (card: BoardCard, fromList: string, toList: string) => {
        if (toList === "Todo") {
            if (fromList === "Backlog") {
                return {
                    title: "交给工作流",
                    desc: "创建/重开工作项并入队（queued），卡移到 Todo 等调度器领取",
                    confirm: "交给工作流",
                    run: () => handleHandOff(card, "Todo"),
                };
            }
            return null; // 执行中/审核中不能回待执行
        }
        if (toList === "In Progress") {
            if (fromList === "Backlog" || fromList === "Todo") {
                return {
                    title: "立即执行",
                    desc: "入队并立即领取（并发满时留在 Todo，由调度器按优先级领取）",
                    confirm: "立即执行",
                    run: () => handleHandOff(card, "In Progress"),
                };
            }
            return null;
        }
        if (toList === "Done") {
            if (fromList === "Review") {
                return {
                    title: "审批通过",
                    desc: "走 S1 审批链（触发灰度发布），卡移到 Done",
                    confirm: "审批通过",
                    run: () => decideCard(card, "approve"),
                };
            }
            if (fromList === "In Progress") {
                return {
                    title: "结束任务（验收完成）",
                    desc: "停止在跑任务，工作项按合法阶梯 in_progress → in_review → approved → shipped，卡移到 Done",
                    confirm: "结束任务",
                    run: () => finishRunningCard(card, "done"),
                };
            }
            return null; // 未执行的任务不能直接标完成（请先交给工作流）
        }
        if (toList === "Backlog") {
            if (fromList === "Review") {
                return {
                    title: "驳回",
                    desc: "进化单取消，工作项 → rejected，卡回到 Backlog",
                    confirm: "驳回",
                    run: () => decideCard(card, "reject"),
                };
            }
            if (fromList === "In Progress") {
                return {
                    title: "终止任务",
                    desc: "停止在跑任务，工作项 → rejected，卡回到 Backlog（可重新 hand-off 重跑）",
                    confirm: "终止任务",
                    run: () => finishRunningCard(card, "reject"),
                };
            }
            if (fromList === "Todo") {
                return {
                    title: "收回候选",
                    desc: "把已入队的任务收回 Backlog（工作项 → open，可再次交给工作流）",
                    confirm: "收回",
                    run: () => moveCardToList(card, "Backlog"),
                };
            }
            return null;
        }
        return null; // Review：流水线自动进入，不提供手动操作
    };

    // 操作框确认：执行对应操作后关闭并刷新。
    const confirmMove = async () => {
        if (!moveDialog || moveActing) return;
        const action = getMoveAction(moveDialog.card, moveDialog.fromList, moveDialog.toList);
        if (!action) {
            setMoveDialog(null);
            return;
        }
        setMoveActing(true);
        try {
            await action.run();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setMoveActing(false);
            setMoveDialog(null);
        }
    };

    const openCreateCard = (listId: number) => {
        setCreatingCardListId(listId);
        setEditingCard(null);
        setEditTitle("");
        setEditDesc("");
        setEditLabels("");
	        setEditTargetName("");
	        setEditDependsOn("");
        setEditAssignee("");
        setEditDueDate("");
        setEditPriority("medium");
        setEditCoverColor("");
        setEditChecklist([]);
        setNewCheckItem("");
        setNewComment("");
        setCardComments([]);
    };

    const openEditCard = async (card: BoardCard) => {
        setRerunMsg(null);
        setEditingCard(card);
        setEditTitle(card.title);
        setEditDesc(card.description || "");
        setEditLabels(card.labels || "");
	        setEditTargetName(card.target_name || "");
	        setEditDependsOn(card.depends_on || "");
        setEditAssignee(card.assignee || "");
        setEditDueDate(
            card.due_date && !isNaN(new Date(card.due_date).getTime())
                ? new Date(card.due_date).toISOString().slice(0, 16)
                : "",
        );
        setEditPriority(card.priority || "medium");
        setEditCoverColor(card.cover_color || "");
        try {
            setEditChecklist(card.checklist ? JSON.parse(card.checklist) : []);
        } catch {
            setEditChecklist([]);
        }
        setNewCheckItem("");
        setNewComment("");
	        try {
	            setCardComments((await fetchCardComments(card.id)).comments);
	        } catch {
	            setCardComments([]);
	        }
	        // S9 执行记录：加载该卡所有尝试（工作项）+ S1 链路阶段事件
	        setExecutions(null);
	        setExecLoading(true);
	        try {
	            setExecutions(await fetchCardExecutions(card.id));
	        } catch {
	            setExecutions([]);
	        } finally {
	            setExecLoading(false);
	        }
	    };

    const handleSaveCard = async () => {
        if (!editTitle.trim()) return;
        try {
            const dueDate =
                editDueDate && !isNaN(new Date(editDueDate).getTime())
                    ? new Date(editDueDate).toISOString()
                    : null;
            if (creatingCardListId) {
                await createCard(creatingCardListId, editTitle.trim(), {
                    description: editDesc,
                    labels: editLabels,
                    target_name: editTargetName,
                    depends_on: editDependsOn,
                    assignee: editAssignee,
                    due_date: dueDate,
                    priority: editPriority,
                    cover_color: editCoverColor,
                    checklist: JSON.stringify(editChecklist),
                });
            } else if (editingCard) {
                await updateCard(editingCard.id, {
                    title: editTitle.trim(),
                    description: editDesc,
                    labels: editLabels,
                    target_name: editTargetName,
                    depends_on: editDependsOn,
                    assignee: editAssignee,
                    due_date: dueDate,
                    priority: editPriority,
                    cover_color: editCoverColor,
                    checklist: JSON.stringify(editChecklist),
                });
            }
            setEditingCard(null);
            setCreatingCardListId(null);
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    const handleDeleteConfirm = async () => {
        if (!deleteConfirm) return;
        if (deleteConfirm.type === "board") {
            await deleteBoard(deleteConfirm.id);
            setSelected(null);
            setBoards(prev => prev.filter(x => x.id !== deleteConfirm.id));
        }
        setDeleteConfirm(null);
    };

    const handleDeleteCard = async (id: number) => {
        try {
            await deleteCard(id);
            if (selected) await loadBoard(selected.id);
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    const toggleCheckItem = (idx: number) => {
        setEditChecklist((prev) =>
            prev.map((item, i) =>
                i === idx ? { ...item, done: !item.done } : item,
            ),
        );
    };

    const addCheckItem = () => {
        if (!newCheckItem.trim()) return;
        setEditChecklist((prev) => [
            ...prev,
            { text: newCheckItem.trim(), done: false },
        ]);
        setNewCheckItem("");
    };

    const removeCheckItem = (idx: number) => {
        setEditChecklist((prev) => prev.filter((_, i) => i !== idx));
    };

    const titleText = () => {
        if (timeMode === "all") return "全部";
        if (timeMode === "day") return fmtDate(dayDate);
        if (timeMode === "week") {
            const ws = weekStart;
            return `${fmtDate(ws)} - ${fmtDate(addDays(ws, 6))}`;
        }
        return `${monthYear.y}年${monthYear.m + 1}月`;
    };

    // Empty state
    if (loading) {
        return (
            <div className="flex-1 flex items-center justify-center">
                <Loader2 className="h-6 w-6 animate-spin text-(--text-muted)" />
            </div>
        );
    }
    if (boards.length === 0) {
        return (
            <div className="flex-1 p-4 overflow-y-auto">
                <header className="mb-8">
                    <h1 className="text-2xl font-bold tracking-tighter uppercase flex items-center gap-2">
                        <Columns className="h-6 w-6" /> Board
                    </h1>
                    <p className="mt-1 text-sm text-(--text-muted)">
                        按状态列管理任务，支持日/周/月时间筛选
                    </p>
                </header>
                {error && (
                    <div className="mb-4 border border-(--danger)/50 bg-(--danger)/10 p-3 text-sm text-(--danger)">
                        {error}
                    </div>
                )}
                <form
                    onSubmit={handleCreate}
                    className="border border-[#000000] p-4"
                >
                    <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted) mb-3">
                        新建 Board
                    </h2>
                    <div className="flex flex-wrap gap-3 items-end">
                        <div className="flex-1 min-w-50">
                            <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                                名称
                            </label>
                            <input
                                value={newName}
                                onChange={(e) => setNewName(e.target.value)}
                                placeholder="Board 名称"
                                required
                                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-4 py-2 text-sm focus:outline-none focus:border-white"
                            />
                        </div>
                        <button
                            type="submit"
                            disabled={creating}
                            className="bg-(--accent) text-(--accent-foreground) font-bold px-6 py-2 text-sm hover:bg-(--text-primary) transition disabled:opacity-50 flex items-center gap-2"
                        >
                            {creating ? (
                                <Loader2 className="h-4 w-4 animate-spin" />
                            ) : (
                                <Plus className="h-4 w-4" />
                            )}
                            创建
                        </button>
                    </div>
                </form>
            </div>
        );
    }
    if (!selected)
        return (
            <div className="flex-1 flex items-center justify-center">
                <Loader2 className="h-6 w-6 animate-spin text-(--text-muted)" />
            </div>
        );

    // Board detail
    return (
        <div className="flex-1 relative p-4 overflow-hidden flex flex-col">
            <header className="flex px-2 items-center justify-between mb-4 shrink-0 border border-(--border-subtle)">
                <div className="flex items-center gap-1">
                    {boards.map((b) => (
                        <div key={b.id} className="relative group">
                            <button
                                onClick={() => loadBoard(b.id)}
                                onContextMenu={(e) => {
                                    e.preventDefault();
                                    setCtxMenu({
                                        x: e.clientX,
                                        y: e.clientY,
                                        board: b,
                                    });
                                }}
                                className={`px-4 py-3 text-xs font-bold uppercase transition border-b-2 -mb-0.5 ${selected?.id === b.id ? "border-white text-white" : "border-transparent text-(--text-muted) hover:text-(--text-secondary)"}`}
                            >
                                {b.name}
                            </button>
                        </div>
                    ))}
                    <form
                        onSubmit={(e) => {
                            e.preventDefault();
                            if (!newName.trim()) return;
                            createBoard(newName.trim(), "week")
                                .then(() => {
                                    setNewName("");
                                    loadBoards();
                                })
                                .catch((err) =>
                                    setError(
                                        err instanceof Error
                                            ? err.message
                                            : String(err),
                                    ),
                                );
                        }}
                        className="flex items-center"
                    >
                        <input
                            value={newName}
                            onChange={(e) => setNewName(e.target.value)}
                            placeholder="+ 新建"
                            className="bg-transparent border-0 px-2 py-2 text-xs text-(--text-muted) placeholder:text-(--text-disabled) focus:outline-none w-20"
                        />
                    </form>
                </div>
                <div className="flex items-center gap-2">
                    {triageMsg && <span className="text-[11px] text-(--text-muted) max-w-72 truncate" title={triageMsg}>{triageMsg}</span>}
                    {isBugBoard && (
                        <button
                            type="button"
                            onClick={() => void runTriage()}
                            disabled={triaging}
                            className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium rounded border border-(--border-subtle) hover:bg-(--bg-elevated) disabled:opacity-50"
                            title="手动跑一轮错误日志分诊（默认 15min 自动一轮）：未归并 error_logs → 聚簇 → AI 分诊 → Bug 卡入 Backlog"
                        >
                            {triaging ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Zap className="h-3.5 w-3.5" />}
                            手动分诊
                        </button>
                    )}
                </div>
            </header>

            {/* Time filter */}
            <div className="flex items-center justify-between mb-4 shrink-0">
                <div className="flex items-center gap-2">
                    {(["all", "day", "week", "month"] as const).map((m) => (
                        <button
                            key={m}
                            onClick={() => {
                                setTimeMode(m);
                                setTimeOffset(0);
                                setMonthYear({
                                    y: new Date().getFullYear(),
                                    m: new Date().getMonth(),
                                });
                                setDayDate(new Date());
                            }}
                            className={`px-3 py-1 text-xs font-bold uppercase transition ${timeMode === m ? "border-white bg-(--accent) text-(--accent-foreground)" : "border border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
                        >
                            {m === "all"
                                ? "全部"
                                : m === "day"
                                  ? "日"
                                  : m === "week"
                                    ? "周"
                                    : "月"}
                        </button>
                    ))}
                </div>
                {timeMode !== "all" && (
                    <div className="flex items-center gap-2">
                        <button
                            onClick={() => {
                                if (timeMode === "week")
                                    setTimeOffset((o) => o - 1);
                                else if (timeMode === "month")
                                    setMonthYear((p) => ({
                                        y: p.m === 0 ? p.y - 1 : p.y,
                                        m: p.m === 0 ? 11 : p.m - 1,
                                    }));
                                else setDayDate((d) => addDays(d, -1));
                            }}
                            className="p-1 border border-(--border-subtle) text-(--text-muted) hover:text-white"
                        >
                            <ChevronLeft className="h-4 w-4" />
                        </button>
                        <span className="text-sm font-mono text-(--text-secondary)">
                            {titleText()}
                        </span>
                        <button
                            onClick={() => {
                                if (timeMode === "week")
                                    setTimeOffset((o) => o + 1);
                                else if (timeMode === "month")
                                    setMonthYear((p) => ({
                                        y: p.m === 11 ? p.y + 1 : p.y,
                                        m: p.m === 11 ? 0 : p.m + 1,
                                    }));
                                else setDayDate((d) => addDays(d, 1));
                            }}
                            className="p-1 border border-(--border-subtle) text-(--text-muted) hover:text-white"
                        >
                            <ChevronRight className="h-4 w-4" />
                        </button>
                    </div>
                )}
            </div>

            {error && (
                <div className="mb-4 border border-(--danger)/50 bg-(--danger)/10 p-3 text-sm text-(--danger) shrink-0">
                    {error}
                </div>
            )}

            {/* Lists as columns */}
            <div className="flex gap-3 overflow-x-auto flex-1 pb-4">
                {selected.lists?.map((list) => {
                    const filtered = cardsInRange(list.cards || []);
                    return (
                        <div
                            key={list.id}
                            className="bg-(--bg-surface) border border-(--border-subtle) shrink-0 flex flex-col min-h-0"
                            onDragOver={handleDragOver}
                            onDrop={(e) => handleDrop(e, list.name)}
                            style={{ width: "calc(20% - 10px)", minWidth:"200px", maxWidth: "300px" }}
                        >
                            <div className="p-3 border-b border-(--border-subtle) flex items-center justify-between">
                                <h3 className="text-xs font-bold uppercase text-(--text-secondary)">
                                    {list.name}
                                </h3>
                                <div className="flex items-center gap-2">
                                    {!["Backlog","Todo","In Progress","Review","Done"].includes(list.name) && (
                                        <button onClick={() => setDeleteListConfirm({id: list.id, name: list.name})} className="text-(--text-muted) hover:text-red-400"><Trash2 className="h-3 w-3" /></button>
                                    )}
                                    <span className="text-[10px] text-(--text-muted)">{filtered.length}</span>
                                </div>
                            </div>
                            <div className="p-2 space-y-2 overflow-y-auto flex-1">
                                {filtered.map((card) => (
                                    <div
                                        key={card.id}
                                        className="bg-(--bg-elevated)  p-2.5 group cursor-pointer transition-colors"
                                        draggable
                                        onDragStart={() => handleDragStart(card, list.name)}
                                        onClick={() => openEditCard(card)}
                                        style={
                                            card.cover_color
                                                ? {
                                                      borderLeftColor: card.cover_color,
                                                      borderLeftWidth: 8,
                                                  }
                                                : {
                                                      borderLeftColor: "#000",
                                                      borderLeftWidth: 8,
                                                  }
                                        }
                                    >
                                        <div className="flex items-start justify-between gap-1">
                                            <p className="text-xs text-(--text-primary) leading-relaxed flex-1">
                                                {card.title}
                                            </p>
                                            <div className="flex gap-0.5 opacity-0 group-hover:opacity-100 transition-opacity shrink-0">
                                                {(!card.work_item_no || card.work_item_no.startsWith("card-")) && ["Backlog", "Todo"].includes(list.name) && (
                                                    <button
                                                        onClick={(e) => {
                                                            e.stopPropagation();
                                                            handleHandOff(card);
                                                        }}
                                                        className="p-0.5 text-(--text-muted) hover:text-white"
                                                        title="交给工作流（移到 Todo 列入队，等调度器领取；拖到 In Progress 则立即执行）"
                                                    >
                                                        <Zap className="h-3 w-3" />
                                                    </button>
                                                )}
                                            </div>
                                        </div>
                                        {card.target_name && (
                                            <div className="flex gap-1 mt-1.5 flex-wrap">
                                                {card.target_name
                                                    .split(",")
                                                    .map((x) => x.trim())
                                                    .filter((x) => x !== "")
                                                    .map((n) => (
                                                        <span key={n} className="px-1.5 py-0.5 bg-emerald-500/20 text-emerald-300 text-[10px] font-bold">
                                                            目标 {n}
                                                        </span>
                                                    ))}
                                            </div>
                                        )}
                                        {card.labels && (
                                            <div className="flex gap-1 mt-1.5 flex-wrap">
                                                {card.labels
                                                    .split(",")
                                                    .filter(Boolean)
                                                    .map((l, i) => (
                                                        <span
                                                            key={i}
                                                            className="px-1.5 py-0.5 bg-white/10 text-[10px] text-(--text-secondary)"
                                                        >
                                                            {l.trim()}
                                                        </span>
                                                    ))}
                                            </div>
                                        )}
                                        {card.description && (
                                            <p className="text-[11px] text-(--text-secondary) mt-1 line-clamp-2">
                                                {card.description}
                                            </p>
                                        )}
                                        <div className="flex items-center gap-2 mt-2 flex-wrap">
                                            {card.priority &&
                                                card.priority !== "medium" && (
                                                    <span
                                                        className={`text-[9px] font-bold uppercase px-1 py-0.5 ${card.priority === "urgent" ? "bg-red-500/20 text-red-400" : card.priority === "high" ? "bg-amber-500/20 text-amber-400" : "bg-blue-500/20 text-blue-400"}`}
                                                    >
                                                        {card.priority}
                                                    </span>
                                                )}
                                            {card.assignee && (
                                                <span className="text-[10px] text-(--text-muted)">
                                                    👤 {card.assignee}
                                                </span>
                                            )}
                                            {card.work_item_no && !card.work_item_no.startsWith("card-") ? (
                                                <span
                                                    className={`text-[9px] font-bold uppercase px-1 py-0.5 ${PIPELINE_BADGE[card.status || "todo"]?.cls ?? "bg-violet-500/20 text-violet-300"}`}
                                                    title={`工作项 ${card.work_item_no}（状态随流水线实时同步）`}
                                                >
                                                    {PIPELINE_BADGE[card.status || "todo"]?.label ?? card.status}
                                                </span>
                                            ) : card.work_item_no?.startsWith("card-") ? (
                                                // S9 §7.4：分诊 Bug 卡（未交给工作流，work_item_no 是卡关联 "card-<id>"）
                                                <span className="text-[9px] font-bold uppercase px-1 py-0.5 bg-red-500/20 text-red-300" title={`分诊 Bug 卡（${card.work_item_no}）：拖到 Todo = 交给工作流`}>Bug</span>
                                            ) : card.status === "ready_for_evolve" ? (
                                                <span className="text-[9px] font-bold uppercase px-1 py-0.5 bg-blue-500/20 text-blue-300" title="已交给工作流，等待调度器建工作项入队">排队中</span>
                                            ) : card.status === "done" ? (
                                                <span className="text-[9px] font-bold uppercase px-1 py-0.5 bg-green-500/20 text-green-300">已完成</span>
                                            ) : null}
                                            {card.blocked_by && card.blocked_by.length > 0 && (
                                                <span
                                                    className="text-[9px] font-bold px-1 py-0.5 bg-amber-500/20 text-amber-300"
                                                    title={`需 ${card.blocked_by.map((b) => `#${b.card_id} ${b.title}`).join("、")} 的改动合入主分支后才能开始（在卡片里改「前置卡片」可调整）`}
                                                >
                                                    ⏳ 需 {card.blocked_by.map((b) => `#${b.card_id}`).join("、")} 完成后才开始
                                                </span>
                                            )}
                                            {/* 失败态（evolve_status=failed）：排在 In Progress 等人工处理，不打回 Backlog */}
                                            {card.evolve_status === "failed" && (
                                                <span
                                                    className="text-[9px] font-bold px-1 py-0.5 bg-rose-500/20 text-rose-300 truncate max-w-[220px]"
                                                    title={card.fail_reason || "任务失败（可点「续接执行」或「完成执行」）"}
                                                >
                                                    ✕ 已失败
                                                    {card.fail_reason
                                                        ? `：${card.fail_reason.slice(0, 60)}${card.fail_reason.length > 60 ? "…" : ""}`
                                                        : ""}
                                                </span>
                                            )}
                                            {/* 合入阻塞提示（2026-11-03）：代码分支有未提交改动 / 冲突 → 无法完成合并，卡停在 Review */}
                                            {card.merge_state === "paused_base_dirty" && (
                                                <span
                                                    className="text-[9px] font-bold px-1 py-0.5 bg-amber-500/20 text-amber-300"
                                                    title="代码分支有修改（相关仓库工作区有未提交改动），无法完成合并。提交/清理后驱动会自动补合（每 5 分钟重试），合入成功才进 Done。"
                                                >
                                                    ⏸ 分支有未提交改动，无法合并
                                                </span>
                                            )}
                                            {card.merge_state === "conflict" && (
                                                <span
                                                    className="text-[9px] font-bold px-1 py-0.5 bg-amber-500/20 text-amber-300"
                                                    title="与 main 合并冲突，正在自动重试解决；未合入前会一直停在 Review。"
                                                >
                                                    ⏸ 合并冲突，重试中
                                                </span>
                                            )}
                                            {(card.frequency ?? 0) > 0 && (
                                                <span className="text-[9px] font-bold px-1 py-0.5 bg-orange-500/20 text-orange-300" title="近 24h 出现次数（分诊归并累加，影响调度优先级）">
                                                    ⚡ {card.frequency}次
                                                </span>
                                            )}
                                            {card.work_item_no && !card.work_item_no.startsWith("card-") && card.status === "in_review" && (
                                                <div className="flex items-center gap-0.5">
                                                    <button
                                                        onClick={(e) => { e.stopPropagation(); void decideCard(card, "approve"); }}
                                                        disabled={decidingCard === card.id}
                                                        className="flex items-center gap-0.5 text-[9px] font-bold uppercase px-1 py-0.5 bg-green-500/20 text-green-300 hover:bg-green-500/40 disabled:opacity-50"
                                                        title="直接审批通过（走 S1 审批链：触发灰度发布，卡自动移到 Done）"
                                                    >
                                                        <CheckCircle2 className="h-2.5 w-2.5" /> 审批
                                                    </button>
                                                    <button
                                                        onClick={(e) => { e.stopPropagation(); void decideCard(card, "reject"); }}
                                                        disabled={decidingCard === card.id}
                                                        className="flex items-center gap-0.5 text-[9px] font-bold uppercase px-1 py-0.5 bg-rose-500/20 text-rose-300 hover:bg-rose-500/40 disabled:opacity-50"
                                                        title="驳回（进化单取消，卡自动回到 Backlog）"
                                                    >
                                                        <XCircle className="h-2.5 w-2.5" /> 驳回
                                                    </button>
                                                </div>
                                            )}
                                            {card.work_item_no && !card.work_item_no.startsWith("card-") && card.status === "in_progress" && (
                                                <div className="flex items-center gap-0.5">
                                                    <button
                                                        onClick={(e) => { e.stopPropagation(); void resumeRunningCard(card); }}
                                                        className="flex items-center gap-0.5 text-[9px] font-bold uppercase px-1 py-0.5 bg-sky-500/20 text-sky-300 hover:bg-sky-500/40"
                                                        title="续接执行：复用同一个工作目录 + coder 会话 + 已积累改动，从上次中断处继续"
                                                    >
                                                        <Play className="h-2.5 w-2.5" /> 续接执行
                                                    </button>
                                                    <button
                                                        onClick={(e) => { e.stopPropagation(); setMoveDialog({ card, fromList: "In Progress", toList: "Done" }); }}
                                                        disabled={decidingCard === card.id}
                                                        className="flex items-center gap-0.5 text-[9px] font-bold uppercase px-1 py-0.5 bg-green-500/20 text-green-300 hover:bg-green-500/40 disabled:opacity-50"
                                                        title="结束任务（验收完成）：停止在跑任务，工作项逐级到 shipped，卡移到 Done"
                                                    >
                                                        <CheckCircle2 className="h-2.5 w-2.5" /> 结束
                                                    </button>
                                                    <button
                                                        onClick={(e) => { e.stopPropagation(); setMoveDialog({ card, fromList: "In Progress", toList: "Backlog" }); }}
                                                        disabled={decidingCard === card.id}
                                                        className="flex items-center gap-0.5 text-[9px] font-bold uppercase px-1 py-0.5 bg-rose-500/20 text-rose-300 hover:bg-rose-500/40 disabled:opacity-50"
                                                        title="终止任务：停止在跑任务，工作项 → rejected，卡回到 Backlog（可重新 hand-off）"
                                                    >
                                                        <XCircle className="h-2.5 w-2.5" /> 终止
                                                    </button>
                                                </div>
                                            )}
                                        </div>
                                        <div className="flex items-center gap-2 mt-1 text-[10px] text-(--text-muted)">
                                            {card.due_date && (
                                                <span>
                                                    📅{" "}
                                                    {new Date(
                                                        card.due_date,
                                                    ).toLocaleDateString()}
                                                </span>
                                            )}
                                            {card.checklist &&
                                                (() => {
                                                    try {
                                                        const cl = JSON.parse(
                                                            card.checklist,
                                                        );
                                                        const done = cl.filter(
                                                            (i: any) => i.done,
                                                        ).length;
                                                        return (
                                                            <span className="inline-flex items-center gap-1">
                                                                <ListChecks className="h-3 w-3" /> {done}/{cl.length}
                                                            </span>
                                                        );
                                                    } catch {
                                                        return null;
                                                    }
                                                })()}
                                        </div>
                                    </div>
                                ))}
                            </div>
                            <div className="p-2 border-t border-(--border-subtle)">
                                <button
                                    onClick={() => openCreateCard(list.id)}
                                    className="w-full text-left px-2 py-2 text-xs text-(--text-secondary) hover:text-white hover:bg-(--bg-elevated) transition-colors"
                                >
                                    + 添加卡片
                                </button>
                            </div>
                        </div>
                    );
                })}
            </div>

            {/* S9 拖拽操作框：拖到某列 → 弹该阶段迁移的特定操作，用户完成后卡才真正移动 */}
            {moveDialog && (() => {
                const action = getMoveAction(moveDialog.card, moveDialog.fromList, moveDialog.toList);
                if (!action) return null;
                return (
                    <div
                        className="fixed inset-0 z-[55] flex items-center justify-center bg-black/60 p-4"
                        onClick={() => setMoveDialog(null)}
                    >
                        <div
                            className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-md"
                            onClick={(e) => e.stopPropagation()}
                        >
                            <div className="p-5 border-b border-(--border-subtle)">
                                <h2 className="font-bold text-sm text-(--text-secondary)">
                                    {action.title}
                                </h2>
                                <p className="text-[10px] text-(--text-muted) mt-1">
                                    {moveDialog.fromList} → {moveDialog.toList} · {moveDialog.card.title}
                                </p>
                            </div>
                            <div className="p-5">
                                <p className="text-xs text-(--text-secondary) leading-relaxed">
                                    {action.desc}
                                </p>
                            </div>
                            <div className="p-4 border-t border-(--border-subtle) flex gap-2 justify-end">
                                <button
                                    onClick={() => setMoveDialog(null)}
                                    disabled={moveActing}
                                    className="px-4 py-2 text-xs border border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated) disabled:opacity-50"
                                >
                                    取消
                                </button>
                                <button
                                    onClick={() => void confirmMove()}
                                    disabled={moveActing}
                                    className="px-4 py-2 text-xs font-bold bg-white/90 text-black hover:bg-white disabled:opacity-50"
                                >
                                    {moveActing ? "处理中…" : action.confirm}
                                </button>
                            </div>
                        </div>
                    </div>
                );
            })()}

            {/* Edit modal */}
            {(editingCard || creatingCardListId) && (
                <div
                    className="fixed inset-0 z-50 flex items-start justify-center bg-black/60 p-4 overflow-y-auto"
                    onClick={() => {
                        setEditingCard(null);
                        setCreatingCardListId(null);
                    }}
                >
                    <div
                        className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-2xl lg:max-w-5xl my-8"
                        onClick={(e) => e.stopPropagation()}
                    >
                        {/* Header */}
                        <div className="p-5 border-b border-(--border-subtle) flex items-center justify-between">
                            <div className="flex items-center gap-2">
                                {editCoverColor && (
                                    <div
                                        className="w-8 h-8"
                                        style={{
                                            backgroundColor: editCoverColor,
                                        }}
                                    />
                                )}
                                <div>
                                    <h2 className="font-bold text-sm text-(--text-secondary)">
                                        {editingCard?.id
                                            ? `卡片 #${editingCard.id}`
                                            : "新建卡片"}
                                    </h2>
                                    {editingCard?.id && (
                                        <p className="text-[10px] text-(--text-muted)">
                                            创建于{" "}
                                            {fmtTime(editingCard.created_at)}
                                            {editingCard.source === "workflow"
                                                ? ` · 工作流 ${editingCard.workflow_id}`
                                                : ""}
                                        </p>
                                    )}
                                </div>
                            </div>
                            <button
                                onClick={() => {
                                    setEditingCard(null);
                                    setCreatingCardListId(null);
                                }}
                                className="text-(--text-muted) hover:text-white text-lg"
                            >
                                &times;
                            </button>
                        </div>

                        {/* Body：宽屏（lg+）左右结构——左=表单，右=执行记录；窄屏上下堆叠（执行记录在底部） */}
                        <div className="p-5 max-h-[70vh] overflow-y-auto">
                            <div className="flex flex-col lg:flex-row lg:gap-6">
                                <div className="flex-1 min-w-0 space-y-4">
                            {/* Title */}
                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                    标题
                                </label>
                                <input
                                    value={editTitle}
                                    onChange={(e) =>
                                        setEditTitle(e.target.value)
                                    }
                                    className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white font-bold"
                                />
                            </div>

                            {/* Grid row */}
                            <div className="grid grid-cols-3 gap-3">
                                <div>
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                        指派人
                                    </label>
                                    <input
                                        value={editAssignee}
                                        onChange={(e) =>
                                            setEditAssignee(e.target.value)
                                        }
                                        placeholder="名字"
                                        className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white"
                                    />
                                </div>
                                <div>
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                        截止时间
                                    </label>
                                    <input
                                        type="datetime-local"
                                        value={editDueDate}
                                        onChange={(e) =>
                                            setEditDueDate(e.target.value)
                                        }
                                        className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white"
                                    />
                                </div>
                                <div>
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                        优先级
                                    </label>
                                    <select
                                        value={editPriority}
                                        onChange={(e) =>
                                            setEditPriority(e.target.value)
                                        }
                                        className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white"
                                    >
                                        <option value="low">Low</option>
                                        <option value="medium">Medium</option>
                                        <option value="high">High</option>
                                        <option value="urgent">Urgent</option>
                                    </select>
                                </div>
                            </div>

                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                    目标项目（可多选：S1 可修改的 apps/&lt;dir&gt;；含 core 系 = 核心框架）
                                </label>
                                <div className="w-full max-h-44 overflow-y-auto bg-(--bg-elevated) border border-(--border-subtle) rounded">
                                    {targetOptions.map((o) => {
                                        const cur = editTargetName
                                            .split(",")
                                            .map((x) => x.trim())
                                            .filter((x) => x !== "");
                                        return (
                                            <label
                                                key={o.name}
                                                className="flex items-center gap-2 px-3 py-1.5 text-sm cursor-pointer hover:bg-(--bg-elevated) transition-colors"
                                            >
                                                <input
                                                    type="checkbox"
                                                    checked={cur.includes(o.name)}
                                                    onChange={() => toggleTargetName(o.name)}
                                                    className="accent-white"
                                                />
                                                <span className="text-(--text-primary)">{o.name}</span>
                                                <span className="text-[10px] text-(--text-muted) ml-auto">
                                                    {o.name.startsWith("core") ? "核心框架" : "项目"}
                                                </span>
                                            </label>
                                        );
                                    })}
                                    {targetOptions.length === 0 && (
                                        <div className="px-3 py-2 text-xs text-(--text-muted)">
                                            （未指定 → 无法执行，打回 Backlog）
                                        </div>
                                    )}
                                </div>
                            </div>

                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                    前置卡片（卡 id，逗号分隔：需它们合入主分支后本卡才能开始）
                                </label>
                                <input
                                    value={editDependsOn}
                                    onChange={(e) => setEditDependsOn(e.target.value)}
                                    placeholder="例如 137 或 137,138"
                                    className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white"
                                />
                                {editingCard?.blocked_by && editingCard.blocked_by.length > 0 && (
                                    <div className="mt-1 text-[10px] text-amber-300">
                                        ⏳ 当前被阻塞：
                                        {editingCard.blocked_by
                                            .map((b) => `#${b.card_id} ${b.title}`)
                                            .join("、")}
                                        （未合入主分支）
                                    </div>
                                )}
                            </div>

                            <div className="grid grid-cols-2 gap-3">
                                <div>
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                        标签 (纯展示，逗号分隔)
                                    </label>
                                    <input
                                        value={editLabels}
                                        onChange={(e) =>
                                            setEditLabels(e.target.value)
                                        }
                                        placeholder="frontend, urgent"
                                        className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white"
                                    />
                                </div>
                                <div>
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                        封面颜色
                                    </label>
                                    <div className="flex gap-1">
                                        {[
                                            "",
                                            "#EF444430",
                                            "#F59E0B30",
                                            "#10B98130",
                                            "#3B82F630",
                                            "#8B5CF630",
                                            "#EC489930",
                                        ].map((c) => (
                                            <button
                                                key={c}
                                                onClick={() =>
                                                    setEditCoverColor(c)
                                                }
                                                className={`w-9 h-9 border ${editCoverColor === c ? "border-white" : "border-(--border-subtle)"}`}
                                                style={{
                                                    backgroundColor:
                                                        c || "transparent",
                                                }}
                                                title={c || "无"}
                                            />
                                        ))}
                                    </div>
                                </div>
                            </div>

                            {/* Description */}
                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                                    描述
                                </label>
                                <textarea
                                    value={editDesc}
                                    onChange={(e) =>
                                        setEditDesc(e.target.value)
                                    }
                                    rows={5}
                                    placeholder="详细描述..."
                                    className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white resize-y font-mono"
                                />
                            </div>

                            {/* Checklist */}
                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-2 font-bold">
                                    清单 (
                                    {editChecklist.filter((i) => i.done).length}
                                    /{editChecklist.length})
                                </label>
                                <div className="space-y-1.5 mb-2">
                                    {editChecklist.map((item, i) => (
                                        <div
                                            key={i}
                                            className="flex items-center gap-2 group"
                                        >
                                            <input
                                                type="checkbox"
                                                checked={item.done}
                                                onChange={() =>
                                                    toggleCheckItem(i)
                                                }
                                                className="accent-white"
                                            />
                                            <span
                                                className={`text-xs flex-1 ${item.done ? "line-through text-(--text-muted)" : "text-(--text-primary)"}`}
                                            >
                                                {item.text}
                                            </span>
                                            <button
                                                onClick={() =>
                                                    removeCheckItem(i)
                                                }
                                                className="opacity-0 group-hover:opacity-100 text-(--text-muted) hover:text-red-400 text-xs"
                                            >
                                                &times;
                                            </button>
                                        </div>
                                    ))}
                                </div>
                                <div className="flex gap-1">
                                    <input
                                        value={newCheckItem}
                                        onChange={(e) =>
                                            setNewCheckItem(e.target.value)
                                        }
                                        onKeyDown={(e) =>
                                            e.key === "Enter" && addCheckItem()
                                        }
                                        placeholder="添加清单项"
                                        className="flex-1 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-xs focus:outline-none focus:border-white"
                                    />
                                    <button
                                        onClick={addCheckItem}
                                        className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-1.5 text-xs text-(--text-secondary) hover:bg-(--bg-hover)"
                                    >
                                        添加
                                    </button>
                                </div>
                            </div>
                                </div>

                                {/* 右列（宽屏 lg+）：执行记录；窄屏在底部堆叠 */}
                                {editingCard?.id && (
                                <div className="lg:w-80 xl:w-96 shrink-0 lg:border-l lg:border-(--border-subtle) lg:pl-6 mt-5 lg:mt-0">
                                <div className="border border-(--border-subtle) bg-(--bg-surface) p-3">
                                    <div className="flex items-center gap-2 mb-2">
                                        <History className="h-3.5 w-3.5 text-(--text-muted)" />
                                        <span className="text-[10px] uppercase font-bold text-(--text-muted)">
                                            执行记录
                                        </span>
                                        {execLoading && (
                                            <Loader2 className="h-3 w-3 animate-spin text-(--text-muted)" />
                                        )}
                                    </div>
                                    {/* 失败重跑：续接执行（同目录/同会话/保留已改代码）或重新执行（从零） */}
                                    <div className="flex items-center gap-2 mb-2">
                                        <button
                                            onClick={() => rerunCurrentCard("resume")}
                                            disabled={rerunActing !== null}
                                            title="续接执行：复用上一轮的进化单——同一工作目录（已改代码保留）+ 同一 coder 会话，从中断处继续"
                                            className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-[10px] font-bold text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-50"
                                        >
                                            {rerunActing === "resume" ? (
                                                <Loader2 className="h-3 w-3 animate-spin" />
                                            ) : (
                                                <Play className="h-3 w-3" />
                                            )}
                                            续接执行
                                        </button>
                                        <button
                                            onClick={() => rerunCurrentCard("restart")}
                                            disabled={rerunActing !== null}
                                            title="重新执行：新工作项 + 干净工作目录 + 全新会话，从零开始"
                                            className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-[10px] font-bold text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-50"
                                        >
                                            {rerunActing === "restart" ? (
                                                <Loader2 className="h-3 w-3 animate-spin" />
                                            ) : (
                                                <RotateCcw className="h-3 w-3" />
                                            )}
                                            重新执行
                                        </button>
                                        {/* 完成执行：停任务 + 直接进 Review 列（不跑完沙箱/CI/审批） */}
                                        <button
                                            onClick={() => editingCard && finishToReview(editingCard)}
                                            disabled={rerunActing !== null}
                                            title="完成执行：停掉当前任务，直接把工作项推到 in_review，卡进 Review 列由你检查改动"
                                            className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-[10px] font-bold text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-50"
                                        >
                                            {rerunActing === "review" ? (
                                                <Loader2 className="h-3 w-3 animate-spin" />
                                            ) : (
                                                <CheckCircle2 className="h-3 w-3" />
                                            )}
                                            完成执行
                                        </button>
                                    </div>
                                    {rerunMsg && (
                                        <div
                                            className={`mb-2 border p-2 text-[11px] ${
                                                rerunMsg.kind === "ok"
                                                    ? "border-(--ok)/50 text-(--ok)"
                                                    : "border-(--danger)/50 text-(--danger)"
                                            }`}
                                        >
                                            {rerunMsg.text}
                                        </div>
                                    )}
                                    {!execLoading && executions && executions.length === 0 && (
                                        <div className="text-xs text-(--text-muted)">
                                            暂无执行记录——把卡拖进 Todo 交给工作流后，这里显示每次尝试的完整链路结果。
                                        </div>
                                    )}
                                    <div className="space-y-3">
                                        {executions?.map((ex) => (
                                            <div
                                                key={ex.work_item_no}
                                                className="border-l-2 border-(--border-subtle) pl-3"
                                            >
                                                <div className="flex items-center gap-2 flex-wrap text-xs">
                                                    <span className={`font-bold ${execOutcome(ex).cls}`}>
                                                        {execOutcome(ex).label}
                                                    </span>
                                                    <span className="font-mono text-(--text-muted)">
                                                        {ex.work_item_no}
                                                    </span>
                                                    <span className="text-(--text-muted)">
                                                        目标 {ex.target_name || "-"}
                                                    </span>
                                                    <span className="text-(--text-muted)">
                                                        {fmtTime(ex.created_at)}
                                                    </span>
                                                    {ex.merge_commit && (
                                                        <span
                                                            className="text-[10px] font-bold text-(--ok)"
                                                            title={`已快进合并到本地 main（${ex.merge_commit}）`}
                                                        >
                                                            ✓ 已合并 main@{ex.merge_commit.slice(0, 8)}
                                                        </span>
                                                    )}
                                                    													{ex.task_merge_state === "paused_base_dirty" && !ex.merge_commit && (
                                                    														<span
                                                    															className="text-[10px] font-bold text-(--danger)"
                                                    															title="主仓库或本次改动的子仓库（如 apps/contract-deploy）工作区有未提交改动 → 合入暂停（整体不合并）；driver 每 5 分钟重试检查，全部干净后自动合入；期间不拾取新任务"
                                                    														>
                                                    															⏸ 合入暂停：相关仓库有未提交改动（每 5 分钟重试）
                                                    														</span>
                                                    													)}
                                                    													{/* 合并/PR 永久失败（pr_error 非空 = 不再重试，需人工）；完整原因在 tooltip */}
                                                    													{ex.pr_error && (
                                                    														<span
                                                    															className="text-[10px] font-bold text-(--danger) cursor-help"
                                                    															title={ex.pr_error}
                                                    														>
                                                    															✕ 合并/PR 失败：{ex.pr_error.slice(0, 80)}
                                                    															{ex.pr_error.length > 80 ? "…" : ""}
                                                    														</span>
                                                    													)}
                                                    												{ex.pr_url && (
                                                        <a
                                                            href={ex.pr_url}
                                                            target="_blank"
                                                            rel="noreferrer"
                                                            className="text-[10px] font-bold text-(--info) hover:text-white"
                                                        >
                                                            PR ↗
                                                        </a>
                                                    )}
                                                    {ex.task_diff && (
                                                        <button
                                                            onClick={() => {
                                                                setPrCopied(false);
                                                                setPrView({
                                                                    title: ex.title, workItemNo: ex.work_item_no,
                                                                    branch: ex.branch, pushCmd: ex.push_cmd,
                                                                    compareUrl: ex.compare_url, diff: ex.task_diff,
                                                                });
                                                            }}
                                                            className="flex items-center gap-0.5 text-[10px] text-(--warning) hover:text-white"
                                                            title="查看本轮完整改动"
                                                        >
                                                            <GitPullRequest className="h-3 w-3" /> PR / 查看改动
                                                        </button>
                                                    )}
                                                    {ex.coder_session && (
                                                        <a
                                                            href={`${PI_WEB_BASE}/?conv=${encodeURIComponent(ex.coder_session)}`}
                                                            target="_blank"
                                                            rel="noreferrer"
                                                            className="flex items-center gap-0.5 text-[10px] text-(--info) hover:text-white"
                                                            title="在 pi-web 查看 coder 的完整修改过程（会话记录）"
                                                        >
                                                            <Zap className="h-3 w-3" /> coder 会话 ↗
                                                        </a>
                                                    )}
                                                    {ex.evolve_task_no && (
                                                        <a
                                                            href={worktreeEditorUrl(ex.evolve_task_no)}
                                                            target="_blank"
                                                            rel="noreferrer"
                                                            className="flex items-center gap-0.5 text-[10px] text-(--purple) hover:text-white"
                                                            title="在 code-server 打开本轮工作目录（真人查看/审代码改动）"
                                                        >
                                                            <FolderOpen className="h-3 w-3" /> 查看代码
                                                        </a>
                                                    )}
                                                </div>
                                                {ex.events.length > 0 && (
                                                    <div className="mt-1 space-y-0.5">
                                                        {ex.events.filter((e) => !isNoopTransition(e)).map((e, i, arr) => {
                                                            // 最后一条 reviewer_done：旧数据无内容时可回退到 task_review
                                                            const isLastRound =
                                                                e.action === "reviewer_done" &&
                                                                !arr.slice(i + 1).some((x) => x.action === "reviewer_done");
                                                            const rv = reviewFromEvent(e, ex, isLastRound);
                                                            return (
                                                            <div
                                                                key={i}
                                                                className="flex items-baseline gap-2 text-[11px]"
                                                            >
                                                                <span className="text-(--text-muted) shrink-0 font-mono">
                                                                    {fmtTime(e.at)}
                                                                </span>
                                                                <span className="text-(--text-secondary) shrink-0">
                                                                    {execEventLabel(e.action)}
                                                                </span>
                                                                <span
                                                                    className="text-(--text-muted) truncate"
                                                                    title={execEventSummary(e)}
                                                                >
                                                                    {execEventSummary(e)}
                                                                </span>
                                                                {rv && (
                                                                    <button
                                                                        onClick={() => setReviewView(rv)}
                                                                        className="shrink-0 flex items-center gap-0.5 text-[10px] text-(--info) hover:text-white"
                                                                        title="查看这一轮 reviewer 的完整审核记录"
                                                                    >
                                                                        <ListChecks className="h-3 w-3" /> 查看
                                                                        {rv.comments.filter((c) => (c.level || "").toLowerCase() === "error").length > 0 && (
                                                                            <span className="font-bold text-(--danger)">
                                                                                error {rv.comments.filter((c) => (c.level || "").toLowerCase() === "error").length}
                                                                            </span>
                                                                        )}
                                                                    </button>
                                                                )}
                                                            </div>
                                                            );
                                                        })}
                                                    </div>
                                                )}
                                            </div>
                                        ))}
                                    </div>
                                </div>
                                </div>
                            )}
                            </div>

                        </div>

                        {/* Footer */}
                        <div className="p-4 border-t border-(--border-subtle) flex gap-2 justify-between">
                            {editingCard?.id && (
                                <button
                                    onClick={() => {
                                        handleDeleteCard(editingCard.id);
                                        setEditingCard(null);
                                        setCreatingCardListId(null);
                                    }}
                                    className="border border-(--danger) text-(--danger) px-3 py-1.5 text-xs font-bold hover:bg-(--danger) hover:text-black"
                                >
                                    删除卡片
                                </button>
                            )}
                            <div className="flex-1"></div>
                            <div className="flex gap-2">
                                <button
                                    onClick={() => {
                                        setEditingCard(null);
                                        setCreatingCardListId(null);
                                    }}
                                    className="border border-(--border-subtle) text-(--text-secondary) px-4 py-1.5 text-sm"
                                >
                                    取消
                                </button>
                                <button
                                    onClick={handleSaveCard}
                                    className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm"
                                >
                                    {editingCard?.id ? "保存" : "创建"}
                                </button>
                            </div>
                        </div>
                    </div>
                </div>
            )}

            {/* Context menu */}
            {ctxMenu && (
                <>
                    <div
                        className="fixed inset-0 z-40"
                        onClick={() => setCtxMenu(null)}
                    />
                    <div
                        className="fixed z-50 bg-(--bg-elevated) border border-(--border-subtle) shadow-lg py-1 min-w-35"
                        style={{ left: ctxMenu.x, top: ctxMenu.y }}
                    >
                        <button
                            onClick={() => {
                                setRenamingBoard(ctxMenu.board);
                                setRenameName(ctxMenu.board.name);
                                setCtxMenu(null);
                            }}
                            className="w-full text-left px-4 py-2 text-xs text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white transition-colors"
                        >
                            重命名
                        </button>
                        <button
                            onClick={() => {
                                {
                                    setDeleteConfirm({
                                        id: ctxMenu.board.id,
                                        type: "board",
                                        name: ctxMenu.board.name,
                                    });
                                    setCtxMenu(null); return;
                                }
                            }}
                            className="w-full text-left px-4 py-2 text-xs text-red-400 hover:bg-(--bg-hover) transition-colors"
                        >
                            删除
                        </button>
                    </div>
                </>
            )}

            {/* Rename modal */}
            {renamingBoard && (
                <div
                    className="fixed inset-0 z-50 flex items-center justify-center bg-black/60"
                    onClick={() => setRenamingBoard(null)}
                >
                    <div
                        className="bg-(--bg-app) border border-(--border-subtle) p-6 w-80 space-y-3"
                        onClick={(e) => e.stopPropagation()}
                    >
                        <h3 className="font-bold text-sm">重命名 Board</h3>
                        <input
                            value={renameName}
                            onChange={(e) => setRenameName(e.target.value)}
                            className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white"
                            autoFocus
                        />
                        <div className="flex gap-2 justify-end">
                            <button
                                onClick={() => setRenamingBoard(null)}
                                className="border border-(--border-subtle) text-(--text-secondary) px-3 py-1.5 text-xs"
                            >
                                取消
                            </button>
                            <button
                                onClick={() => {
                                    if (renameName.trim()) {
                                        renameBoard(
                                            renamingBoard.id,
                                            renameName.trim(),
                                        )
                                            .then(() => {
                                                setRenamingBoard(null);
                                                loadBoards().then(() => {
                                                    if (selected)
                                                        loadBoard(selected.id);
                                                });
                                            })
                                            .catch((err) =>
                                                setError(
                                                    err instanceof Error
                                                        ? err.message
                                                        : String(err),
                                                ),
                                            );
                                    }
                                }}
                                className="bg-(--accent) text-(--accent-foreground) font-bold px-3 py-1.5 text-xs"
                            >
                                保存
                            </button>
                        </div>
                    </div>
                </div>
            )}
      {/* S9 PR / diff 查看器：分支在服务端本地（进化链绝不 push 主分支），推分支后在 GitHub 建 PR */}
      {prView && (
        <div
          className="fixed inset-0 z-[60] flex items-start justify-center bg-black/70 p-4 overflow-y-auto"
          onClick={() => setPrView(null)}
        >
          <div
            className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-4xl my-8"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="p-4 border-b border-(--border-subtle) flex items-center gap-2 flex-wrap">
              <GitPullRequest className="h-4 w-4 text-(--warning)" />
              <span className="font-bold text-sm">PR</span>
              <span className="font-mono text-xs text-(--text-muted)">{prView.branch}</span>
              <span className="text-[10px] text-(--text-muted)">
                {prView.workItemNo} · {prView.title}
              </span>
              <div className="flex-1" />
              <a
                href={prView.compareUrl}
                target="_blank"
                rel="noreferrer"
                className="px-3 py-1.5 text-xs bg-(--accent) text-black font-medium"
                title="推送分支后在 GitHub 一键建 PR（compare 页面）"
              >
                在 GitHub 创建 PR ↗
              </a>
              <button
                onClick={async () => {
                  try {
                    await navigator.clipboard.writeText(prView.pushCmd);
                    setPrCopied(true);
                  } catch (err) {
                    setError(err instanceof Error ? err.message : String(err));
                  }
                }}
                className="px-3 py-1.5 text-xs border border-(--border-subtle) text-(--text-secondary) hover:text-white"
              >
                {prCopied ? "已复制 ✓" : "复制推送命令"}
              </button>
              <button
                onClick={() => setPrView(null)}
                className="text-(--text-muted) hover:text-white text-lg"
              >
                &times;
              </button>
            </div>
            <div className="px-4 py-2 text-[11px] text-(--text-muted) border-b border-(--border-subtle)">
              进化链绝不 push——分支只在服务端仓库本地。流程：在仓库根执行{" "}
              <code className="font-mono text-(--text-secondary)">{prView.pushCmd}</code>…
              推送后点「在 GitHub 创建 PR」；下方为完整改动预览。
            </div>
            <div className="p-4 max-h-[65vh] overflow-auto text-[11px] font-mono whitespace-pre leading-5">
              {prView.diff.split("\n").map((l, i) => (
                <div key={i} className={diffLineClass(l)}>
                  {l === "" ? " " : l}
                </div>
              ))}
            </div>
          </div>
        </div>
      )}

      {reviewView && (
        <div
          className="fixed inset-0 z-[60] flex items-start justify-center bg-black/70 p-4 overflow-y-auto"
          onClick={() => setReviewView(null)}
        >
          <div
            className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-3xl my-8"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="p-4 border-b border-(--border-subtle) flex items-center gap-2 flex-wrap">
              <ListChecks className="h-4 w-4 text-(--info)" />
              <span className="font-bold text-sm">reviewer 审核记录</span>
              {reviewView.taskNo && (
                <span className="font-mono text-xs text-(--text-muted)">{reviewView.taskNo}</span>
              )}
              <span className="text-[10px] text-(--text-muted)">
                {reviewView.workItemNo} · {reviewView.title}
              </span>
              {reviewView.at && (
                <span className="text-[10px] text-(--text-muted)">
                  该轮完成于 {fmtTime(reviewView.at)}
                </span>
              )}
              {(reviewView.sessions?.length ?? 0) > 0 && (
                <a
                  href={`/ops/ocr-reviews/${encodeURIComponent(reviewView.taskNo)}`}
                  className="text-[10px] text-(--info) hover:text-white font-bold"
                  title="在运维中心查看本轮 reviewer 的完整审核过程（会话步骤）"
                >
                  审查过程 ↗
                </a>
              )}
              {reviewView.taskNo && (
                <a
                  href={worktreeEditorUrl(reviewView.taskNo)}
                  target="_blank"
                  rel="noreferrer"
                  className="text-[10px] text-(--purple) hover:text-white font-bold"
                  title="在 code-server 打开该进化单的工作目录（真人查看代码改动）"
                >
                  查看代码 ↗
                </a>
              )}
              <div className="flex-1" />
              <button
                onClick={() => setReviewView(null)}
                className="text-(--text-muted) hover:text-white text-lg"
              >
                &times;
              </button>
            </div>
            <div className="px-4 py-2 text-[11px] text-(--text-muted) border-b border-(--border-subtle) flex items-center gap-3 flex-wrap">
              <span>
                结论：
                {reviewView.ok === true ? (
                  <span className="text-(--ok) font-bold">通过</span>
                ) : reviewView.ok === false ? (
                  <span className="text-(--danger) font-bold">不通过</span>
                ) : (
                  <span>未知</span>
                )}
              </span>
              <span>评论 {reviewView.comments.length} 条</span>
              {reviewView.comments.filter((c) => (c.level || "").toLowerCase() === "error").length > 0 && (
                <span className="text-(--danger)">
                  error {reviewView.comments.filter((c) => (c.level || "").toLowerCase() === "error").length}
                </span>
              )}
              {reviewView.comments.filter((c) => (c.level || "").toLowerCase() === "warning").length > 0 && (
                <span className="text-(--warning)">
                  warning {reviewView.comments.filter((c) => (c.level || "").toLowerCase() === "warning").length}
                </span>
              )}
            </div>
            {reviewView.comments.length > 0 ? (
              <div className="p-4 max-h-[65vh] overflow-auto space-y-3">
                {reviewView.comments.map((c, i) => (
                  <div key={i} className="border-l-2 border-(--border-subtle) pl-3">
                    <div className="flex items-baseline gap-2 flex-wrap text-[11px]">
                      <span className={`font-bold ${reviewLevelCls(c.level)}`}>
                        {(c.level || "info").toUpperCase()}
                      </span>
                      {c.file && (
                        <span className="font-mono text-(--text-muted)">
                          {c.file}
                          {c.line ? `:${c.line}` : ""}
                        </span>
                      )}
                    </div>
                    <div className="text-[12px] text-(--text-secondary) whitespace-pre-wrap mt-0.5">
                      {c.message || ""}
                    </div>
                  </div>
                ))}
              </div>
            ) : reviewView.structured ? (
              <div className="p-6 text-center space-y-2">
                <p
                  className={`text-sm font-bold ${reviewView.ok === false ? "text-(--danger)" : reviewView.ok === true ? "text-(--ok)" : "text-(--text-secondary)"}`}
                >
                  {reviewView.ok === true
                    ? "本轮审核通过，无逐条意见"
                    : reviewView.ok === false
                      ? "本轮审核未通过，reviewer 未给出逐条意见"
                      : "本轮审核无逐条意见"}
                </p>
                <p className="text-[11px] text-(--text-muted)">
                  {reviewView.ok === false
                    ? "可展开下方原始输出排查，或点右上「审查过程」回看会话记录。"
                    : (reviewView.sessions?.length ?? 0) > 0
                      ? `已记录 ${reviewView.sessions!.length} 个审查会话，点右上「审查过程」可回看本轮完整过程。`
                      : "reviewer 未产生行级评论。"}
                </p>
                <details className="text-left">
                  <summary className="cursor-pointer text-[10px] text-(--text-muted) hover:text-white select-none">
                    原始输出（JSON）
                  </summary>
                  <pre className="mt-1 p-2 bg-black/30 border border-(--border-subtle) text-[10px] font-mono whitespace-pre-wrap break-all text-(--text-secondary) max-h-64 overflow-auto">
                    {reviewView.raw}
                  </pre>
                </details>
              </div>
            ) : (
              <div className="p-4 max-h-[65vh] overflow-auto text-[11px] font-mono whitespace-pre-wrap text-(--text-secondary)">
                {reviewView.raw}
              </div>
            )}
          </div>
        </div>
      )}

      {deleteConfirm && (
        <ConfirmModal
          open={true}
          title="确认删除"
          message={'确认删除 "' + (deleteConfirm.name || '') + '"？'}
          onConfirm={handleDeleteConfirm}
          onCancel={() => setDeleteConfirm(null)}
        />
      )}
      {deleteListConfirm && (
        <ConfirmModal
          open={true}
          title="删除列表"
          message={'确认删除列表 "' + deleteListConfirm.name + '"？列表内的卡片也会被删除。'}
          onConfirm={async () => {
            try {
              await deleteList(deleteListConfirm.id);
              setDeleteListConfirm(null);
              if (selected) await loadBoard(selected.id);
            } catch (err) {
              setError(err instanceof Error ? err.message : String(err));
            }
          }}
          onCancel={() => setDeleteListConfirm(null)}
        />
      )}
</div>
    );
}
