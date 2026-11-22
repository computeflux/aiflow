import { useState } from "react";
import { useAsync } from "../hooks";
import { Link, useParams } from "react-router-dom";
import {
    Loader2,
    RefreshCw,
    ArrowLeft,
    CheckCircle2,
    Circle,
    LoaderCircle,
    FileDiff,
    FileJson,
    ListTree,
    Copy,
    Download,
    AlertTriangle,
    ScanSearch,
    FolderOpen,
} from "lucide-react";
import {
    fetchOcrReviewDetail,
    type OcrReviewDetail as OcrReviewDetailData,
    type OcrReviewStep,
    type OcrReviewComment,
} from "../api";
import { fmtTime, worktreeEditorUrl } from "../utils";

// OCR review-web（卡片③）：单条审核过程详情页。
// 数据源 GET /ocr/reviews/:task_no（wi-1793175196-bd90）：
// 任务元数据 + 解析后的审核评论 + 步骤时间线 + 审计事件流 + 原始产物（diff / review JSON）。
// 从列表页（/ops/ocr-reviews）点「详情」进入。

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

const STEP_LABELS: Record<string, string> = {
    created: "创建",
    coding: "编码",
    reviewing: "审核",
    sandboxing: "沙箱",
    testing: "测试",
    approving: "审批",
    released: "发布",
};

const LEVEL_LABELS: Record<string, string> = {
    error: "错误",
    warning: "警告",
    info: "提示",
};
const LEVEL_CLASSES: Record<string, string> = {
    error: "text-(--danger)",
    warning: "text-(--warning)",
    info: "text-(--text-muted)",
};

type ArtifactTab = "diff" | "review" | "events";

function StepIcon({ step }: { step: OcrReviewStep }) {
    if (step.status === "done") return <CheckCircle2 className="h-4 w-4 text-(--ok) shrink-0" />;
    if (step.status === "running") return <LoaderCircle className="h-4 w-4 text-(--warning) animate-spin shrink-0" />;
    return <Circle className="h-4 w-4 text-(--text-muted) shrink-0" />;
}

function StepDetail({ detail }: { detail: any }) {
    if (detail === null || detail === undefined) return null;
    const text = typeof detail === "string" ? detail : JSON.stringify(detail, null, 2);
    if (!text || text === "null") return null;
    return (
        <pre className="mt-1 text-[10px] bg-black/40 border border-(--border-default) p-2 overflow-auto max-h-32 whitespace-pre-wrap break-all text-(--text-secondary)">{text}</pre>
    );
}

function downloadText(filename: string, text: string) {
    const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    a.click();
    URL.revokeObjectURL(url);
}

export function OcrReviewDetail() {
    const { taskNo = "" } = useParams<{ taskNo: string }>();
    const { data, loading, error, reload: load } = useAsync<OcrReviewDetailData>(
        () => fetchOcrReviewDetail(taskNo),
        [taskNo],
    );
    const [tab, setTab] = useState<ArtifactTab>("diff");
    const [copied, setCopied] = useState(false);

    const copyText = async (text: string) => {
        try {
            await navigator.clipboard.writeText(text);
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
        } catch {
            // 剪贴板不可用（非 https / 权限）时静默失败
        }
    };

    if (loading && !data) {
        return (
            <div className="flex-1 flex items-center justify-center text-(--text-muted)">
                <Loader2 className="h-5 w-5 animate-spin mr-2" /> 加载审核详情…
            </div>
        );
    }

    if (error && !data) {
        return (
            <div className="flex-1 p-4 overflow-y-auto">
                <Link to="/ops/ocr-reviews" className="inline-flex items-center gap-1 text-xs text-(--text-secondary) hover:text-white mb-4 transition">
                    <ArrowLeft className="h-3 w-3" /> 返回审核记录列表
                </Link>
                <div className="border border-(--border-subtle) p-4 text-sm font-bold bg-(--bg-elevated) text-red-500 inline-flex items-center gap-2">
                    <AlertTriangle className="h-4 w-4" /> {error}
                </div>
            </div>
        );
    }

    if (!data) return null;

    const { task, review, steps, events, artifacts } = data;
    const activeTabText = tab === "diff" ? artifacts.diff : tab === "review" ? artifacts.review_json : events.map((e) => `${e.at}\t${e.action}\t${e.actor}\t${e.note}\t${e.detail}`).join("\n");

    return (
        <div className="flex-1 p-4 overflow-y-auto">
            {/* 头部：返回 + 元数据 */}
            <header className="mb-6">
                <div className="flex items-center justify-between mb-3">
                    <Link to="/ops/ocr-reviews" className="inline-flex items-center gap-1 text-xs text-(--text-secondary) hover:text-white transition">
                        <ArrowLeft className="h-3 w-3" /> 返回审核记录列表
                    </Link>
                    <button onClick={load} disabled={loading} className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-1.5 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2">
                        {loading ? <Loader2 className="h-3 w-3 animate-spin" /> : <RefreshCw className="h-3 w-3" />} 刷新
                    </button>
                </div>
                <div className="flex items-center gap-3 flex-wrap">
                    <h1 className="text-2xl font-bold tracking-tighter uppercase font-mono">{task.task_no}</h1>
                    <span className={`uppercase font-bold text-xs ${STATUS_CLASSES[task.status] || "text-(--text-muted)"}`}>
                        {STATUS_LABELS[task.status] || task.status}
                    </span>
                    {task.pr_url && (
                        <a href={task.pr_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--accent) hover:bg-(--bg-hover) text-xs font-mono transition">
                            PR
                        </a>
                    )}
                    <a href={worktreeEditorUrl(task.task_no)} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--accent) hover:bg-(--bg-hover) text-xs font-mono transition" title="在编辑器中打开该进化单的工作目录">
                        <FolderOpen className="h-3 w-3" /> 工作目录
                    </a>
                </div>
                <div className="mt-3 grid grid-cols-2 gap-x-8 gap-y-2 text-sm sm:grid-cols-3 lg:grid-cols-6">
                    <div><span className="text-(--text-muted) text-xs font-bold uppercase block">目标模块</span><p className="font-mono text-xs text-(--text-secondary) truncate" title={task.target_name}>{task.target_name}</p></div>
                    <div><span className="text-(--text-muted) text-xs font-bold uppercase block">来源</span><p className="font-mono text-xs text-(--text-secondary)">{task.source || "-"}</p></div>
                    <div><span className="text-(--text-muted) text-xs font-bold uppercase block">Reviewer</span><p className="font-mono text-xs text-(--text-secondary)">{review?.agent || "-"}</p></div>
                    <div><span className="text-(--text-muted) text-xs font-bold uppercase block">创建时间</span><p className="font-mono text-xs text-(--text-secondary)">{fmtTime(task.created_at)}</p></div>
                    <div><span className="text-(--text-muted) text-xs font-bold uppercase block">更新时间</span><p className="font-mono text-xs text-(--text-secondary)">{fmtTime(task.updated_at)}</p></div>
                    <div><span className="text-(--text-muted) text-xs font-bold uppercase block">审核重试</span><p className="font-mono text-xs text-(--text-secondary)">{review?.reviewer_retries ?? 0} 次</p></div>
                </div>
            </header>

            {error && <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">{error}</div>}

            <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
                {/* 左列：过程时间线（步骤状态 + 时间戳） */}
                <section className="border border-(--border-subtle)">
                    <div className="flex items-center gap-2 px-4 py-3 border-b border-(--border-subtle)">
                        <ListTree className="h-4 w-4 text-(--text-muted)" />
                        <h2 className="font-bold text-sm uppercase tracking-widest">过程时间线</h2>
                    </div>
                    <ol className="p-4">
                        {steps.map((step, i) => (
                            <li key={step.name} className="relative flex gap-3 pb-5 last:pb-0">
                                {/* 连接线 */}
                                {i < steps.length - 1 && (
                                    <span className={`absolute left-[7px] top-5 bottom-0 w-px ${step.status === "done" ? "bg-(--ok)/40" : "bg-(--border-subtle)"}`} />
                                )}
                                <div className="pt-0.5"><StepIcon step={step} /></div>
                                <div className="flex-1 min-w-0">
                                    <div className="flex items-center gap-2 flex-wrap">
                                        <span className={`text-sm font-bold ${step.status === "pending" ? "text-(--text-muted)" : "text-(--text-primary)"}`}>
                                            {STEP_LABELS[step.name] || step.name}
                                        </span>
                                        <span className="font-mono text-[10px] uppercase text-(--text-muted)">{step.name}</span>
                                        {step.status === "running" && <span className="text-[10px] font-bold uppercase text-(--warning)">进行中</span>}
                                        <span className="ml-auto font-mono text-[10px] text-(--text-muted)">{step.at ? fmtTime(step.at) : "-"}</span>
                                    </div>
                                    <StepDetail detail={step.detail} />
                                </div>
                            </li>
                        ))}
                    </ol>
                </section>

                {/* 右列：审核评论 */}
                <section className="border border-(--border-subtle)">
                    <div className="flex items-center gap-2 px-4 py-3 border-b border-(--border-subtle)">
                        <ScanSearch className="h-4 w-4 text-(--text-muted)" />
                        <h2 className="font-bold text-sm uppercase tracking-widest">审核评论</h2>
                        {review && (
                            <span className="ml-auto inline-flex items-center gap-2 font-mono text-xs">
                                <span className="text-(--text-secondary)">{review.summary.total} 条</span>
                                <span className="text-(--danger)">{review.summary.error}E</span>
                                <span className="text-(--warning)">{review.summary.warning}W</span>
                                <span className="text-(--text-muted)">{review.summary.info}I</span>
                            </span>
                        )}
                    </div>
                    <div className="p-4 max-h-[480px] overflow-y-auto">
                        {!review ? (
                            <p className="text-xs text-(--text-muted) font-mono">无有效审核数据（review 列为空 / 解析失败 / ok=false）</p>
                        ) : review.comments.length === 0 ? (
                            <p className="text-xs text-(--text-muted) font-mono">审核通过，无评论</p>
                        ) : (
                            <ul className="space-y-2">
                                {review.comments.map((c: OcrReviewComment, i: number) => (
                                    <li key={i} className="border border-(--border-subtle) bg-(--bg-elevated) p-2">
                                        <div className="flex items-center gap-2 mb-1">
                                            <span className={`text-[10px] font-bold uppercase ${LEVEL_CLASSES[c.level] || LEVEL_CLASSES.info}`}>
                                                {LEVEL_LABELS[c.level] || c.level}
                                            </span>
                                            <span className="font-mono text-[10px] text-(--text-secondary) truncate" title={c.file}>
                                                {c.file}{c.line > 0 ? `:${c.line}` : ""}
                                            </span>
                                        </div>
                                        <p className="text-xs text-(--text-secondary) whitespace-pre-wrap break-words">{c.message}</p>
                                    </li>
                                ))}
                            </ul>
                        )}
                    </div>
                </section>
            </div>

            {/* 关键产物：diff / review JSON / 审计事件流 */}
            <section className="mt-4 border border-(--border-subtle)">
                <div className="flex items-center gap-2 px-4 py-3 border-b border-(--border-subtle) flex-wrap">
                    <FileDiff className="h-4 w-4 text-(--text-muted)" />
                    <h2 className="font-bold text-sm uppercase tracking-widest">关键产物</h2>
                    <div className="flex items-center gap-1 ml-4">
                        {([
                            { key: "diff", label: "Diff", icon: FileDiff },
                            { key: "review", label: "Review JSON", icon: FileJson },
                            { key: "events", label: `事件流（${events.length}）`, icon: ListTree },
                        ] as { key: ArtifactTab; label: string; icon: typeof FileDiff }[]).map((t) => (
                            <button key={t.key} onClick={() => setTab(t.key)}
                                className={`inline-flex items-center gap-1 px-3 py-1 text-xs font-bold ${tab === t.key ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
                                <t.icon className="h-3 w-3" /> {t.label}
                            </button>
                        ))}
                    </div>
                    <div className="ml-auto flex items-center gap-2">
                        <button onClick={() => void copyText(activeTabText)} disabled={!activeTabText}
                            className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:bg-(--bg-hover) text-xs transition disabled:opacity-40">
                            <Copy className="h-3 w-3" /> {copied ? "已复制" : "复制"}
                        </button>
                        <button onClick={() => downloadText(`${task.task_no}-${tab === "events" ? "events.log" : tab === "review" ? "review.json" : "diff"}`, activeTabText)} disabled={!activeTabText}
                            className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:bg-(--bg-hover) text-xs transition disabled:opacity-40">
                            <Download className="h-3 w-3" /> 下载
                        </button>
                    </div>
                </div>
                <div className="p-4">
                    {tab === "diff" ? (
                        artifacts.diff ? (
                            <pre className="bg-black/40 border border-(--border-default) p-3 text-[11px] overflow-auto max-h-[480px] whitespace-pre text-(--text-secondary)">{artifacts.diff}</pre>
                        ) : (
                            <p className="text-xs text-(--text-muted) font-mono">无 diff 产物</p>
                        )
                    ) : tab === "review" ? (
                        artifacts.review_json ? (
                            <pre className="bg-black/40 border border-(--border-default) p-3 text-[11px] overflow-auto max-h-[480px] whitespace-pre-wrap break-all text-(--text-secondary)">{artifacts.review_json}</pre>
                        ) : (
                            <p className="text-xs text-(--text-muted) font-mono">无 review JSON 产物</p>
                        )
                    ) : events.length === 0 ? (
                        <p className="text-xs text-(--text-muted) font-mono">无审计事件</p>
                    ) : (
                        <table className="w-full text-left text-xs">
                            <thead className="text-(--text-muted)">
                                <tr>
                                    <th className="py-2 pr-4 font-bold uppercase w-40">时间</th>
                                    <th className="py-2 pr-4 font-bold uppercase w-40">事件</th>
                                    <th className="py-2 pr-4 font-bold uppercase w-28">执行者</th>
                                    <th className="py-2 font-bold uppercase">备注 / 详情</th>
                                </tr>
                            </thead>
                            <tbody className="divide-y divide-(--border-divider)">
                                {events.map((e, i) => (
                                    <tr key={i} className="align-top">
                                        <td className="py-2 pr-4 font-mono text-(--text-muted)">{fmtTime(e.at)}</td>
                                        <td className="py-2 pr-4 font-mono text-(--text-primary)">{e.action}</td>
                                        <td className="py-2 pr-4 font-mono text-(--text-secondary)">{e.actor || "-"}</td>
                                        <td className="py-2 text-(--text-secondary)">
                                            {e.note && <span className="block">{e.note}</span>}
                                            {e.detail && e.detail !== "null" && (
                                                <pre className="mt-1 text-[10px] bg-black/40 border border-(--border-default) p-2 overflow-auto max-h-24 whitespace-pre-wrap break-all text-(--text-muted)">{e.detail}</pre>
                                            )}
                                        </td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    )}
                </div>
            </section>
        </div>
    );
}
