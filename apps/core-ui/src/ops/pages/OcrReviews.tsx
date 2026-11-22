import { useState } from "react";
import { useNavigate } from "react-router-dom";
import {
    Loader2,
    RefreshCw,
    Eye,
    Search,
    ScanSearch,
} from "lucide-react";
import {
    fetchOcrReviews,
    listAgents,
    setAgentModel,
    listModelProfiles,
    type OcrReviewListItem,
} from "../api";
import { fmtTime } from "../utils";
import { useAsync } from "../hooks";

// OCR review-web（卡片②）：审核记录列表。
// 数据源 GET /ocr/reviews（wi-1793175196-bd90）：只列有审核数据（review 列非空）的进化单。
// 点「详情」进入单条审核过程详情页（/ops/ocr-reviews/:taskNo，卡片③）。
// 页内可直接切换 reviewer 运行时模型（S8.2：按 reviewer 绑定解析 agent_bindings →
// 实际执行 agent，写入 agents_registry.config.model_profile；下轮进化执行生效）。

const STATUS_LABELS: Record<string, string> = {
    open: "待执行",
    coding: "编码中",
    reviewing: "审核中",
    sandboxing: "沙箱中",
    testing: "测试中",
    approving: "审批中",
    released: "已发布",
    cancelled: "已取消",
    rolled_back: "已回滚",
};
const STATUS_CLASSES: Record<string, string> = {
    open: "text-(--text-muted)",
    coding: "text-(--info)",
    reviewing: "text-(--warning)",
    sandboxing: "text-(--purple)",
    testing: "text-(--purple)",
    approving: "text-(--warning)",
    released: "text-(--ok)",
    cancelled: "text-(--text-muted)",
    rolled_back: "text-(--danger)",
};

const PAGE_SIZE = 20;

export function OcrReviews() {
    const [page, setPage] = useState(1);

    const [taskNo, setTaskNo] = useState("");
    const [targetName, setTargetName] = useState("");
    const [status, setStatus] = useState("");

    const navigate = useNavigate();

    const { data, loading, error, reload: load } = useAsync<{ items: OcrReviewListItem[]; total: number }>(
        async () => {
            const d = await fetchOcrReviews({
                page,
                page_size: PAGE_SIZE,
                task_no: taskNo,
                target_name: targetName,
                status,
            });
            return { items: d.items, total: d.total };
        },
        [page, taskNo, targetName, status],
    );
    const items = data?.items ?? [];
    const total = data?.total ?? 0;

    // reviewer 模型选择器：候选 = 纳入 agent 目录的档案；当前值 = reviewer 绑定 agent 的 config.model_profile。
    const { data: reviewerModel, reload: reloadReviewerModel } = useAsync<{ agent: string; model_profile: string }>(
        async () => {
            const d = await listAgents();
            const name = d.bindings.reviewer?.agent_name ?? Object.values(d.bindings).find((b) => b.agent_name)?.agent_name ?? "";
            const agent = d.agents.find((a) => a.name === name);
            return { agent: agent?.name ?? "", model_profile: agent?.model_profile ?? "" };
        },
        [],
    );
    const { data: profileData } = useAsync(() => listModelProfiles(), []);
    const agentProfiles = (profileData ?? []).filter((p) => p.sync_agents !== 0);

    const [modelDraft, setModelDraft] = useState<string | null>(null);
    const [modelMsg, setModelMsg] = useState("");
    const [modelSaving, setModelSaving] = useState(false);

    const currentModel = modelDraft ?? reviewerModel?.model_profile ?? "";

    const onSaveModel = async () => {
        if (!reviewerModel?.agent) return;
        setModelSaving(true);
        setModelMsg("");
        try {
            await setAgentModel(reviewerModel.agent, modelDraft ?? "");
            setModelMsg(`reviewer（${reviewerModel.agent}）模型 → ${modelDraft || "全局生效模型"}，下轮进化执行生效`);
            setModelDraft(null);
            await reloadReviewerModel();
        } catch (e) {
            setModelMsg(`设置失败：${e instanceof Error ? e.message : String(e)}`);
        } finally {
            setModelSaving(false);
        }
    };

    const applyFilters = () => {
        setPage(1);
    };

    const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

    return (
        <div className="flex-1 p-4 overflow-y-auto">
            <header className="flex items-center justify-between mb-8">
                <div>
                    <h1 className="text-2xl font-bold tracking-tighter uppercase">OCR 审核记录</h1>
                    <p className="mt-1 text-sm text-(--text-muted)">
                        单次 OCR 审核（reviewer）记录列表 · 点「详情」查看完整审核过程（步骤时间线 / 评论 / 产物）
                    </p>
                </div>
                <div className="flex items-center gap-2">
                    {/* reviewer 模型选择（S8.2）：档案需先在「模型管理」建档并勾选 agent 目录 */}
                    <label className="text-xs font-bold uppercase text-(--text-muted)" htmlFor="ocr-reviewer-model">Reviewer 模型</label>
                    <select
                        id="ocr-reviewer-model"
                        value={currentModel}
                        onChange={(e) => setModelDraft(e.target.value)}
                        className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-2 text-xs text-(--text-primary) outline-none focus:border-(--accent)"
                    >
                        <option value="">全局生效模型</option>
                        {agentProfiles.map((p) => (
                            <option key={p.id} value={p.name}>{p.name}（{p.model}）</option>
                        ))}
                    </select>
                    <button
                        onClick={() => void onSaveModel()}
                        disabled={modelSaving || reviewerModel == null || modelDraft === null || modelDraft === (reviewerModel.model_profile ?? "")}
                        title="设置 reviewer（OCR）审核使用的模型；下轮进化执行生效"
                        className="border border-(--border-subtle) bg-(--accent) px-4 py-2 text-xs font-bold text-(--accent-foreground) hover:bg-(--bg-hover) transition disabled:opacity-40 flex items-center gap-2"
                    >
                        {modelSaving ? <Loader2 className="h-4 w-4 animate-spin" /> : null} 保存
                    </button>
                    <button onClick={load} disabled={loading} className="border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2">
                        {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />} 刷新
                    </button>
                </div>
            </header>

            {modelMsg && <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-(--text-secondary)">{modelMsg}</div>}

            {/* 筛选栏 */}
            <div className="mb-4 border border-(--border-subtle) p-4">
                <div className="flex items-center gap-4 flex-wrap">
                    <label className="text-xs font-bold uppercase text-(--text-muted)">状态:</label>
                    <div className="flex items-center gap-1 flex-wrap">
                        <button onClick={() => { setStatus(""); setPage(1); }}
                            className={`px-3 py-1 text-xs font-bold uppercase ${status === "" ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
                            全部
                        </button>
                        {Object.entries(STATUS_LABELS).map(([k, label]) => (
                            <button key={k} onClick={() => { setStatus(k); setPage(1); }}
                                className={`px-3 py-1 text-xs font-bold uppercase ${status === k ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
                                {label}
                            </button>
                        ))}
                    </div>
                    <div className="flex items-center gap-2 ml-auto">
                        <input
                            value={taskNo}
                            onChange={(e) => setTaskNo(e.target.value)}
                            onKeyDown={(e) => e.key === "Enter" && applyFilters()}
                            placeholder="进化单号（模糊）"
                            className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-1.5 text-xs text-(--text-primary) w-48 outline-none focus:border-(--accent)"
                        />
                        <input
                            value={targetName}
                            onChange={(e) => setTargetName(e.target.value)}
                            onKeyDown={(e) => e.key === "Enter" && applyFilters()}
                            placeholder="目标模块（精确）"
                            className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-1.5 text-xs text-(--text-primary) w-40 outline-none focus:border-(--accent)"
                        />
                        <button onClick={applyFilters} className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-1.5 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) flex items-center gap-1">
                            <Search className="h-3 w-3" /> 搜索
                        </button>
                    </div>
                </div>
            </div>

            {error && <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">{error}</div>}

            {/* 记录列表 */}
            <div className="border border-(--border-subtle)">
                <table className="w-full text-left text-sm">
                    <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                        <tr>
                            <th className="px-4 py-3 font-bold uppercase w-44">进化单号</th>
                            <th className="px-4 py-3 font-bold uppercase w-36">目标模块</th>
                            <th className="px-4 py-3 font-bold uppercase w-24">状态</th>
                            <th className="px-4 py-3 font-bold uppercase w-24">来源</th>
                            <th className="px-4 py-3 font-bold uppercase w-40">评论（E/W/I）</th>
                            <th className="px-4 py-3 font-bold uppercase w-40">创建时间</th>
                            <th className="px-4 py-3 font-bold uppercase w-40">更新时间</th>
                            <th className="px-4 py-3 font-bold uppercase w-24">操作</th>
                        </tr>
                    </thead>
                    <tbody className="divide-y divide-(--border-divider)">
                        {items.length === 0 ? (
                            <tr>
                                <td colSpan={8} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
                                    {loading ? "LOADING…" : "NO OCR REVIEWS FOUND"}
                                </td>
                            </tr>
                        ) : (
                            items.map((item) => (
                                <tr key={item.task_no} className="hover:bg-(--bg-elevated) transition-colors">
                                    <td className="px-4 py-3 font-mono text-xs text-(--text-primary)">{item.task_no}</td>
                                    <td className="px-4 py-3 font-mono text-xs text-(--text-secondary) truncate max-w-36" title={item.target_name}>{item.target_name}</td>
                                    <td className="px-4 py-3">
                                        <span className={`uppercase font-bold text-xs ${STATUS_CLASSES[item.status] || "text-(--text-muted)"}`}>
                                            {STATUS_LABELS[item.status] || item.status}
                                        </span>
                                    </td>
                                    <td className="px-4 py-3 font-mono text-xs text-(--text-muted)">{item.source || "-"}</td>
                                    <td className="px-4 py-3">
                                        {item.comment_count > 0 ? (
                                            <span className="inline-flex items-center gap-2 font-mono text-xs">
                                                <span className="text-(--text-secondary)">{item.comment_count}</span>
                                                <span className="text-(--danger)">{item.error_count}E</span>
                                                <span className="text-(--warning)">{item.warning_count}W</span>
                                                <span className="text-(--text-muted)">{item.info_count}I</span>
                                            </span>
                                        ) : (
                                            <span className="text-(--text-muted) text-xs">-</span>
                                        )}
                                    </td>
                                    <td className="px-4 py-3 text-(--text-muted) font-mono text-xs">{fmtTime(item.created_at)}</td>
                                    <td className="px-4 py-3 text-(--text-muted) font-mono text-xs">{fmtTime(item.updated_at)}</td>
                                    <td className="px-4 py-3">
                                        <button
                                            onClick={() => navigate(`/ops/ocr-reviews/${encodeURIComponent(item.task_no)}`)}
                                            className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--accent) hover:bg-(--bg-hover) text-xs transition"
                                        >
                                            <Eye className="h-3 w-3" /> 详情
                                        </button>
                                    </td>
                                </tr>
                            ))
                        )}
                    </tbody>
                </table>
            </div>

            {/* 分页 */}
            <div className="mt-4 flex items-center justify-between text-sm text-(--text-muted)">
                <span className="font-mono text-xs inline-flex items-center gap-1">
                    <ScanSearch className="h-3 w-3" /> 共 {total} 条 · 第 {page} / {totalPages} 页
                </span>
                <div className="flex items-center gap-2">
                    <button onClick={() => setPage((p) => Math.max(1, p - 1))} disabled={page <= 1} className="px-3 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-40">上一页</button>
                    <button onClick={() => setPage((p) => Math.min(totalPages, p + 1))} disabled={page >= totalPages} className="px-3 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-40">下一页</button>
                </div>
            </div>
        </div>
    );
}
