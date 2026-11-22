import { useState } from "react";
import {
    Loader2, RefreshCw, Activity, CircleX,
} from "lucide-react";
import {
    fetchWorkItems, rejectWorkItem,
    type WorkItem,
} from "../api";
import { fmtTime } from "../utils";
import { useAsync } from "../hooks";

// S9：工作项中心（2026-10-19 由运维中心移入产品与任务看板）只显示"workflow 正在执行"的任务——
// 已被调度器领取、在 S1 进化链中（in_progress 处理中 / in_review 审核中 / approved 已批准待灰度）。
// 需求（看板卡）与 Bug 列表在工作看板；这里是"正在做什么"的执行看板。
const EXECUTING_STATUSES = ["in_progress", "in_review", "approved"];
const STAGE_COLORS: Record<string, string> = {
    in_progress: "text-(--warning)", in_review: "text-(--warning)", approved: "text-(--ok)",
};
const STAGE_LABELS: Record<string, string> = {
    in_progress: "处理中", in_review: "审核中", approved: "已批准",
};
const PRIORITY_COLORS: Record<string, string> = {
    P0: "text-(--danger)", P1: "text-(--warning)", P2: "text-(--info)", P3: "text-(--text-muted)",
};
const SOURCE_LABELS: Record<string, string> = {
    product: "产品情报", error_log: "错误日志", audit: "审计", manual: "人工",
};
const KIND_COLORS: Record<string, string> = { requirement: "text-(--info)", bug: "text-(--danger)" };
const KIND_LABELS: Record<string, string> = { requirement: "需求", bug: "Bug" };

function checklistText(json: string): string {
    try {
        const arr = JSON.parse(json || "[]") as { text: string; done: boolean }[];
        return arr.map((c) => (c.done ? "☑" : "☐") + " " + c.text).join("\n");
    } catch {
        return "";
    }
}

export function WorkItems() {
    const [msg, setMsg] = useState<string | null>(null);
    const [busyNo, setBusyNo] = useState<string | null>(null);
    const [expanded, setExpanded] = useState<string | null>(null);

    // 并行拉取三个执行阶段，合并为"正在执行"集合（按领取时间倒序，最近领取/进入链路的在前）
    const { data: items, loading, error, reload: load } = useAsync<WorkItem[]>(async () => {
        const results = await Promise.all(EXECUTING_STATUSES.map((s) => fetchWorkItems({ status: s })));
        const merged = results.flat();
        merged.sort((a, b) => (b.picked_at || b.created_at).localeCompare(a.picked_at || a.created_at));
        return merged;
    }, []);

    const act = async (no: string, fn: () => Promise<void>, okMsg: string) => {
        setBusyNo(no);
        setMsg(null);
        try {
            await fn();
            setMsg(okMsg);
            await load();
        } catch (e) {
            setMsg(e instanceof Error ? e.message : String(e));
        } finally {
            setBusyNo(null);
        }
    };

    const list = items ?? [];
    const counts = {
        in_progress: list.filter((i) => i.status === "in_progress").length,
        in_review: list.filter((i) => i.status === "in_review").length,
        approved: list.filter((i) => i.status === "approved").length,
    };

    return (
        <div className="flex-1 flex flex-col overflow-hidden">
            {/* 标题 + 操作 */}
            <div className="flex items-center gap-3 px-6 py-4 border-b border-(--border-default)">
                <Activity className="h-5 w-5 text-(--text-secondary)" />
                <h1 className="text-lg font-semibold">工作项中心 · 正在执行</h1>
                <span className="text-xs text-(--text-muted)">
                    调度器已领取、在 S1 进化链中：处理中 {counts.in_progress} · 审核中 {counts.in_review} · 已批准 {counts.approved}
                </span>
                <div className="flex-1" />
                {msg && <span className="text-xs text-(--text-secondary)">{msg}</span>}
                <button type="button" onClick={() => void load()} disabled={loading}
                    className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium rounded border border-(--border-default) hover:bg-(--bg-elevated) disabled:opacity-50">
                    {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5" />}
                    刷新
                </button>
            </div>

            {/* 列表 */}
            <div className="flex-1 overflow-y-auto px-6 py-4 space-y-2">
                {error && (
                    <div className="text-sm text-(--danger) flex items-center gap-2">
                        <CircleX className="h-4 w-4" /> {error}
                    </div>
                )}
                {!loading && !error && list.length === 0 && (
                    <div className="text-sm text-(--text-muted) py-8 text-center">
                        当前没有正在执行的工作项。看板卡「交给工作流」/ 产品拆解 / 审计 / Bug 分诊入队后，调度器会自动领取并在这里显示。
                    </div>
                )}
                {list.map((it) => {
                    const isExpanded = expanded === it.no;
                    return (
                        <div key={it.no}
                            className="rounded-lg border border-(--border-default) bg-(--bg-elevated) p-3 hover:border-(--border-hover) transition-colors">
                            <div className="flex items-start gap-3">
                                <button type="button" onClick={() => setExpanded(isExpanded ? null : it.no)}
                                    className="flex-1 text-left min-w-0">
                                    <div className="flex items-center gap-2 flex-wrap">
                                        <span className={`text-xs font-bold ${PRIORITY_COLORS[it.priority] ?? "text-(--text-secondary)"}`}>{it.priority}</span>
                                        <span className={`text-xs font-medium ${KIND_COLORS[it.kind] ?? "text-(--text-secondary)"}`}>{KIND_LABELS[it.kind] ?? it.kind}</span>
                                        <span className="text-[11px] text-(--text-muted)">{SOURCE_LABELS[it.source] ?? it.source}</span>
                                        <span className={`text-[11px] font-bold ${STAGE_COLORS[it.status] ?? "text-(--text-secondary)"}`}>{STAGE_LABELS[it.status] ?? it.status}</span>
                                    </div>
                                    <div className="text-sm font-medium text-(--text-primary) mt-1 truncate">{it.title}</div>
                                    <div className="text-[11px] text-(--text-muted) mt-0.5">
                                        分 {it.score}（严{it.severity} 急{it.urgency} 面{it.impact} 频{it.frequency}）
                                        {it.target_name && <> · 目标 {it.target_name}</>}
                                        {it.evolve_task_no && <> · 进化单 {it.evolve_task_no}</>}
                                    </div>
                                </button>
                                {/* 操作 */}
                                <div className="flex items-center gap-1 shrink-0">
                                    <ActionBtn title="驳回（终止该工作项 + 进化单）" busy={busyNo === it.no}
                                        onClick={() => act(it.no, () => rejectWorkItem(it.no, "人工终止"), `已驳回 ${it.no}`)}>
                                        <CircleX className="h-3.5 w-3.5 text-(--danger)" />
                                    </ActionBtn>
                                </div>
                            </div>
                            {/* 展开详情 */}
                            {isExpanded && (
                                <div className="mt-3 pt-3 border-t border-(--border-default) text-xs space-y-2">
                                    {it.description && (
                                        <div className="text-(--text-secondary) whitespace-pre-wrap break-words">{it.description}</div>
                                    )}
                                    {checklistText(it.checklist) && (
                                        <div className="text-(--text-secondary) whitespace-pre-wrap">{checklistText(it.checklist)}</div>
                                    )}
                                    <div className="text-(--text-muted) flex gap-4 flex-wrap">
                                        <span>no: {it.no}</span>
                                        {it.source_ref && <span>来源: {it.source_ref}</span>}
                                        {it.assignee && <span>负责人: {it.assignee}</span>}
                                        <span>创建: {fmtTime(it.created_at)}</span>
                                        {it.picked_at && <span>领取: {fmtTime(it.picked_at)}</span>}
                                    </div>
                                </div>
                            )}
                        </div>
                    );
                })}
            </div>
        </div>
    );
}

function ActionBtn({ title, busy, onClick, children }: {
    title: string; busy: boolean; onClick: () => void; children: React.ReactNode;
}) {
    return (
        <button type="button" title={title} onClick={onClick} disabled={busy}
            className="p-1.5 rounded border border-(--border-default) hover:bg-(--bg-hover) disabled:opacity-40 transition-colors">
            {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : children}
        </button>
    );
}
