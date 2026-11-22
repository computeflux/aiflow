import { useEffect, useState } from "react";
import {
    Loader2, RefreshCw, FileText, Plus, Sparkles,
    AlertTriangle, CheckCircle2, ClipboardList,
} from "lucide-react";
import {
    fetchOpsReports, suggestOpsReport, createOpsSuggestionCard, fetchBoards,
    type OpsReport, type Board,
} from "../api";
import { useAsync } from "../hooks";
import { fmtTime } from "../utils";

const TABS = [
    { key: "daily", label: "日报" },
    { key: "weekly", label: "周报" },
    { key: "upgrade_suggestion", label: "升级建议" },
];

const RISK_COLORS: Record<string, string> = {
    low: "text-(--ok)",
    medium: "text-(--warning)",
    high: "text-(--danger)",
};
const RISK_LABELS: Record<string, string> = {
    low: "低风险", medium: "中风险", high: "高风险",
};

interface SuggestionView {
    title: string; reason: string; suggested_change: string;
    risk_level: string; manual_pr_only: boolean;
    suggested_card?: { title: string; description: string; priority: string; checklist: string[] };
}

function suggestionsOf(rep: OpsReport | null): SuggestionView[] {
    if (!rep || rep.type !== "upgrade_suggestion") return [];
    return rep.metrics?.suggestions ?? [];
}

export function OpsReports() {
    const [tab, setTab] = useState("daily");
    const [selectedRep, setSelected] = useState<OpsReport | null>(null);
    const [suggesting, setSuggesting] = useState(false);
    const [boards, setBoards] = useState<Board[]>([]);
    const [listId, setListId] = useState<number>(0);
    const [cardMsg, setCardMsg] = useState<string | null>(null);
    const [cardBusy, setCardBusy] = useState<number | null>(null);
    const [showRaw, setShowRaw] = useState(false);

    const { data, loading, error, reload: load } = useAsync<OpsReport[]>(async () => (await fetchOpsReports(tab)).items, [tab]);
    const items = data ?? [];

    // 自动选中第一项：首项派生自取数结果（无需 effect）；切 tab 时 onClick 已 setSelected(null)
    const selected = selectedRep ?? data?.[0] ?? null;
    useEffect(() => {
        fetchBoards().then((d) => {
            setBoards(d.boards);
            const firstList = d.boards.find((b) => b.lists?.length)?.lists?.[0];
            if (firstList) setListId(firstList.id);
        }).catch(() => {});
    }, []);

    const runSuggest = async () => {
        setSuggesting(true);
        setCardMsg(null);
        try {
            const rep = await suggestOpsReport();
            setTab("upgrade_suggestion");
            await load();
            setSelected(rep);
            setCardMsg("升级建议已生成");
        } catch (e) {
            setCardMsg(e instanceof Error ? e.message : String(e));
        } finally {
            setSuggesting(false);
        }
    };

    const makeCard = async (rep: OpsReport, idx: number) => {
        if (!listId) { setCardMsg("请先选择一个看板列"); return; }
        setCardBusy(idx);
        setCardMsg(null);
        try {
            await createOpsSuggestionCard(rep.id, idx, listId);
            setCardMsg(`已生成任务卡（建议 #${idx + 1}，source=ops_suggestion）`);
        } catch (e) {
            setCardMsg(e instanceof Error ? e.message : String(e));
        } finally {
            setCardBusy(null);
        }
    };

    const suggestions = suggestionsOf(selected);

    return (
        <div className="flex-1 flex flex-col overflow-hidden">
            {/* 标题 + 操作 */}
            <div className="flex items-center gap-3 px-6 py-4 border-b border-(--border-default)">
                <FileText className="h-5 w-5 text-(--text-secondary)" />
                <h1 className="text-lg font-semibold">运维情报</h1>
                <span className="text-xs text-(--text-muted)">每日 08:00 日报 · 每周一 08:00 周报/升级建议</span>
                <div className="flex-1" />
                {tab === "upgrade_suggestion" && (
                    <button
                        type="button"
                        onClick={() => void runSuggest()}
                        disabled={suggesting}
                        className="flex items-center gap-2 px-3 py-1.5 text-sm font-medium bg-(--accent) text-white rounded-none hover:opacity-90 disabled:opacity-50 cursor-pointer"
                    >
                        {suggesting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Sparkles className="h-4 w-4" />}
                        {suggesting ? "生成中…" : "生成建议"}
                    </button>
                )}
                <button
                    type="button"
                    onClick={() => void load()}
                    className="flex items-center gap-2 px-3 py-1.5 text-sm font-medium border border-(--border-default) rounded-none hover:bg-(--bg-elevated) cursor-pointer"
                >
                    <RefreshCw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} /> 刷新
                </button>
            </div>

            {/* 选项卡 */}
            <div className="flex gap-1 px-6 pt-3">
                {TABS.map((t) => (
                    <button
                        key={t.key}
                        type="button"
                        onClick={() => { setTab(t.key); setSelected(null); }}
                        className={`px-4 py-1.5 text-sm font-medium rounded-none transition-colors cursor-pointer ${tab === t.key ? "bg-(--bg-elevated) text-white" : "text-(--text-secondary) hover:bg-(--bg-elevated) hover:text-white"}`}
                    >
                        {t.label}
                    </button>
                ))}
            </div>

            {cardMsg && (
                <div className="mx-6 mt-3 px-3 py-2 text-sm bg-(--bg-elevated) border border-(--border-default) rounded-none">
                    {cardMsg}
                </div>
            )}

            <div className="flex-1 flex overflow-hidden">
                {/* 左：报告列表 */}
                <div className="w-72 border-r border-(--border-default) overflow-y-auto">
                    {loading && items.length === 0 ? (
                        <div className="flex items-center justify-center h-32 text-(--text-muted)"><Loader2 className="h-5 w-5 animate-spin" /></div>
                    ) : items.length === 0 ? (
                        <div className="p-6 text-sm text-(--text-muted)">暂无{labelOf(tab)}。可点击「生成建议」或等待每日 08:00 自动生成。</div>
                    ) : (
                        items.map((r) => (
                            <button
                                key={r.id}
                                type="button"
                                onClick={() => setSelected(r)}
                                className={`w-full text-left px-4 py-3 border-b border-(--border-divider) transition-colors cursor-pointer ${selected?.id === r.id ? "bg-(--bg-elevated)" : "hover:bg-(--bg-elevated)"}`}
                            >
                                <div className="text-sm font-medium">{r.report_date?.slice(0, 10)}</div>
                                <div className="text-xs text-(--text-muted)">{labelOf(r.type)} · {fmtTime(r.created_at)}</div>
                            </button>
                        ))
                    )}
                </div>

                {/* 右：详情 */}
                <div className="flex-1 overflow-y-auto p-6">
                    {error ? (
                        <div className="text-(--danger) text-sm">{error}</div>
                    ) : !selected ? (
                        <div className="text-sm text-(--text-muted)">从左侧选择一份报告查看。</div>
                    ) : (
                        <div>
                            <div className="flex items-center gap-2 mb-3">
                                <span className="text-base font-semibold">{labelOf(selected.type)}</span>
                                <span className="text-xs text-(--text-muted)">{selected.report_date?.slice(0, 10)}</span>
                                {selected.type === "upgrade_suggestion" && (
                                    <div className="flex-1" />
                                )}
                            </div>

                            {/* Markdown 内容 */}
                            <pre className="whitespace-pre-wrap text-sm leading-relaxed bg-(--bg-elevated) border border-(--border-default) rounded-none p-4 mb-4">
                                {selected.content}
                            </pre>

                            {/* 升级建议：逐条 + 一键转任务卡 */}
                            {suggestions.length > 0 && (
                                <div className="mb-6">
                                    <div className="text-sm font-semibold mb-2 flex items-center gap-2">
                                        <ClipboardList className="h-4 w-4" /> 建议（{suggestions.length}）
                                    </div>
                                    <div className="space-y-3">
                                        {suggestions.map((s, i) => (
                                            <div key={i} className="border border-(--border-default) rounded-none p-4">
                                                <div className="flex items-center gap-2 mb-1">
                                                    <span className="text-sm font-medium">{i + 1}. {s.title}</span>
                                                    <span className={`text-xs font-semibold ${RISK_COLORS[s.risk_level] ?? "text-(--text-secondary)"}`}>
                                                        {RISK_LABELS[s.risk_level] ?? s.risk_level}
                                                    </span>
                                                    {s.manual_pr_only && (
                                                        <span className="text-xs text-(--danger) flex items-center gap-1">
                                                            <AlertTriangle className="h-3 w-3" /> 仅人工 PR 建议单
                                                        </span>
                                                    )}
                                                </div>
                                                <div className="text-xs text-(--text-secondary) mb-1">理由：{s.reason}</div>
                                                <div className="text-xs text-(--text-secondary) mb-2">变更：{s.suggested_change}</div>
                                                {s.manual_pr_only ? (
                                                    <div className="text-xs text-(--text-muted)">红线：涉及核心代码，不生成自动任务卡（请走人工 PR）。</div>
                                                ) : (
                                                    s.suggested_card && (
                                                        <div className="flex items-center gap-3">
                                                            <select
                                                                value={listId}
                                                                onChange={(e) => setListId(Number(e.target.value))}
                                                                className="text-xs bg-(--bg-sidebar) border border-(--border-default) rounded-none px-2 py-1"
                                                            >
                                                                {boards.flatMap((b) => b.lists ?? []).map((l) => (
                                                                    <option key={l.id} value={l.id}>#{l.name}</option>
                                                                ))}
                                                            </select>
                                                            <button
                                                                type="button"
                                                                onClick={() => void makeCard(selected, i)}
                                                                disabled={cardBusy === i}
                                                                className="flex items-center gap-1.5 px-3 py-1 text-xs font-medium bg-(--accent) text-white rounded-none hover:opacity-90 disabled:opacity-50 cursor-pointer"
                                                            >
                                                                {cardBusy === i ? <Loader2 className="h-3 w-3 animate-spin" /> : <Plus className="h-3 w-3" />}
                                                                生成任务
                                                            </button>
                                                        </div>
                                                    )
                                                )}
                                            </div>
                                        ))}
                                    </div>
                                </div>
                            )}

                            {/* 原始数据 */}
                            <div className="flex items-center gap-2">
                                <button
                                    type="button"
                                    onClick={() => setShowRaw((v) => !v)}
                                    className="flex items-center gap-1.5 text-xs text-(--text-secondary) hover:text-white cursor-pointer"
                                >
                                    {showRaw ? "收起原始数据" : "展开原始数据（metrics JSON）"}
                                </button>
                                {selected.metrics && Object.keys(selected.metrics).length > 0 && (
                                    <span className="flex items-center gap-1 text-xs text-(--ok)"><CheckCircle2 className="h-3 w-3" /> 含原始数据</span>
                                )}
                            </div>
                            {showRaw && (
                                <pre className="whitespace-pre-wrap text-xs mt-2 bg-(--bg-elevated) border border-(--border-default) rounded-none p-3 max-h-96 overflow-y-auto">
                                    {JSON.stringify(selected.metrics, null, 2)}
                                </pre>
                            )}
                        </div>
                    )}
                </div>
            </div>
        </div>
    );
}

function labelOf(type: string): string {
    switch (type) {
        case "daily": return "日报";
        case "weekly": return "周报";
        case "upgrade_suggestion": return "升级建议";
        default: return type;
    }
}
