import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Loader2, RefreshCw, Eye, AlertTriangle, CheckCircle2, Trash2, Search, ExternalLink, Bug } from "lucide-react";
import {
    fetchErrorLogs,
    fetchErrorStats,
    resolveErrorLog,
    resolveAllErrors,
    deleteErrorLog,
    type ErrorLog,
    type ErrorStats,
} from "../api";
import { fmtTime } from "../utils";
import { useAsync } from "../hooks";

const SYSTEM_LABELS: Record<string, string> = {
    api: "API",
    "go-worker": "Go Worker",
    pycraper: "抓取 Worker",
    pyreplay: "点赞 Worker",
    unknown: "未知",
};
const LEVEL_COLORS: Record<string, string> = {
    error: "text-(--danger)",
    warning: "text-(--warning)",
    fatal: "text-[#F43F5E]",
};
const LEVEL_LABELS: Record<string, string> = {
    error: "错误",
    warning: "警告",
    fatal: "致命",
};

const SYSTEM_FILTERS = ["", "api", "go-worker", "pycraper", "pyreplay"];
const LEVEL_FILTERS = ["", "error", "warning", "fatal"];
const RESOLVED_FILTERS = [
    { value: "", label: "全部" },
    { value: "false", label: "未处理" },
    { value: "true", label: "已处理" },
];

export function Errors() {
    const [page, setPage] = useState(1);
    const pageSize = 20;

    const [system, setSystem] = useState("");
    const [level, setLevel] = useState("");
    const [resolved, setResolved] = useState("false");
    const [keyword, setKeyword] = useState("");

    const [detail, setDetail] = useState<ErrorLog | null>(null);
    const [busy, setBusy] = useState<number | null>(null);
    const navigate = useNavigate();

    // 部分成功也要展示：两路 allSettled，子请求失败原因并入返回值
    interface ErrorsData {
        items: ErrorLog[];
        stats: ErrorStats | null;
        total: number;
        partialError: string | null;
    }
    const { data, loading, error, reload: load, setError } = useAsync<ErrorsData>(async () => {
        const [dataRes, statsRes] = await Promise.allSettled([
            fetchErrorLogs({ system, level, resolved, q: keyword, page, page_size: pageSize }),
            fetchErrorStats(),
        ]);
        const errors: string[] = [];
        for (const r of [dataRes, statsRes]) {
            if (r.status === "rejected") errors.push(r.reason instanceof Error ? r.reason.message : String(r.reason));
        }
        return {
            items: dataRes.status === "fulfilled" ? dataRes.value.items : [],
            total: dataRes.status === "fulfilled" ? dataRes.value.total : 0,
            stats: statsRes.status === "fulfilled" ? statsRes.value : null,
            partialError: errors.length > 0 ? errors.join("；") : null,
        };
    }, [system, level, resolved, keyword, page]);
    const items = data?.items ?? [];
    const stats = data?.stats ?? null;
    const total = data?.total ?? 0;
    // 部分成功（子请求失败）的提示：派生自最近一次取数结果，无需额外 effect
    const partialErr = data?.partialError ?? null;

    const applyFilters = () => {
        setPage(1);
        load();
    };

    const handleResolve = async (id: number) => {
        setBusy(id);
        try {
            await resolveErrorLog(id);
            setDetail((d) => (d && d.id === id ? { ...d, resolved: true } : d));
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setBusy(null);
        }
    };

    const handleResolveAll = async () => {
        if (!window.confirm("确定把当前所有未处理的错误标记为已处理？")) return;
        setBusy(-1);
        try {
            await resolveAllErrors();
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setBusy(null);
        }
    };

    const handleDelete = async (id: number) => {
        if (!window.confirm("确定删除这条错误记录？")) return;
        setBusy(id);
        try {
            await deleteErrorLog(id);
            if (detail && detail.id === id) setDetail(null);
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setBusy(null);
        }
    };

    const totalPages = Math.max(1, Math.ceil(total / pageSize));
    const sysLabel = (s: string) => SYSTEM_LABELS[s] || s;

    return (
        <div className="flex-1 p-4 overflow-y-auto">
            <header className="flex items-center justify-between mb-8">
                <div>
                    <h1 className="text-2xl font-bold tracking-tighter uppercase">错误日志</h1>
                    <p className="mt-1 text-sm text-(--text-muted)">各系统（API / Go Worker / Python Worker）的运行错误统一入库，可筛选、处理与删除</p>
                </div>
                <div className="flex items-center gap-2">
                    <button onClick={handleResolveAll} disabled={loading || busy === -1} className="border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2">
                        {busy === -1 ? <Loader2 className="h-4 w-4 animate-spin" /> : <CheckCircle2 className="h-4 w-4" />} 全部标记已处理
                    </button>
                    <button onClick={load} disabled={loading} className="border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2">
                        {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />} 刷新
                    </button>
                </div>
            </header>

            {/* 统计卡片 */}
            <div className="mb-6 grid grid-cols-1 gap-4 sm:grid-cols-4">
                {[
                    { title: "错误总数", value: stats?.total ?? "-" },
                    { title: "未处理", value: stats?.unresolved ?? "-" },
                    { title: "今日新增", value: stats?.today ?? "-" },
                    {
                        title: "按系统分布",
                        value: stats && (stats.by_system ?? []).length > 0
                            ? (stats.by_system ?? []).map((s) => `${sysLabel(s.key)} ${s.count}`).join(" · ")
                            : "-",
                    },
                ].map((stat, i) => (
                    <div key={i} className="border border-(--border-subtle) p-4">
                        <div className="flex justify-between items-center mb-2">
                            <span className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted)">{stat.title}</span>
                            {i === 1 && <AlertTriangle className="h-4 w-4 text-(--danger)" />}
                        </div>
                        <p className="text-xl font-black truncate" title={typeof stat.value === "string" ? stat.value : undefined}>{stat.value}</p>
                    </div>
                ))}
            </div>

            {/* 筛选栏 */}
            <div className="mb-4 border border-(--border-subtle) p-4">
                <div className="flex items-center gap-4 flex-wrap">
                    <label className="text-xs font-bold uppercase text-(--text-muted)">系统:</label>
                    {SYSTEM_FILTERS.map((s) => (
                        <button key={s || "all"} onClick={() => { setSystem(s); setPage(1); }}
                            className={`px-3 py-1 text-xs font-bold uppercase ${system === s ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
                            {s === "" ? "全部" : sysLabel(s)}
                        </button>
                    ))}
                    <label className="text-xs font-bold uppercase text-(--text-muted)">级别:</label>
                    {LEVEL_FILTERS.map((l) => (
                        <button key={l || "all"} onClick={() => { setLevel(l); setPage(1); }}
                            className={`px-3 py-1 text-xs font-bold uppercase ${level === l ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
                            {l === "" ? "全部" : LEVEL_LABELS[l]}
                        </button>
                    ))}
                    <label className="text-xs font-bold uppercase text-(--text-muted)">状态:</label>
                    {RESOLVED_FILTERS.map((r) => (
                        <button key={r.value} onClick={() => { setResolved(r.value); setPage(1); }}
                            className={`px-3 py-1 text-xs font-bold uppercase ${resolved === r.value ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
                            {r.label}
                        </button>
                    ))}
                    <div className="flex items-center gap-2 ml-auto">
                        <input
                            value={keyword}
                            onChange={(e) => setKeyword(e.target.value)}
                            onKeyDown={(e) => e.key === "Enter" && applyFilters()}
                            placeholder="搜索消息 / 来源 / 工作流"
                            className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-1.5 text-xs text-(--text-primary) w-64 outline-none focus:border-(--accent)"
                        />
                        <button onClick={applyFilters} className="border border-(--border-subtle) bg-(--bg-elevated) px-3 py-1.5 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) flex items-center gap-1">
                            <Search className="h-3 w-3" /> 搜索
                        </button>
                    </div>
                </div>
            </div>

            {(error || partialErr) && <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">{error ?? partialErr}</div>}

            {/* 错误列表 */}
            <div className="border border-(--border-subtle)">
                <table className="w-full text-left text-sm">
                    <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                        <tr>
                            <th className="px-4 py-3 font-bold uppercase w-40">时间</th>
                            <th className="px-4 py-3 font-bold uppercase w-28">系统</th>
                            <th className="px-4 py-3 font-bold uppercase w-20">级别</th>
                            <th className="px-4 py-3 font-bold uppercase w-36">来源</th>
                            <th className="px-4 py-3 font-bold uppercase">消息</th>
                            <th className="px-4 py-3 font-bold uppercase w-32">工作流</th>
                            <th className="px-4 py-3 font-bold uppercase w-20">状态</th>
                            <th className="px-4 py-3 font-bold uppercase w-32">工作项</th>
                            <th className="px-4 py-3 font-bold uppercase w-28">链路</th>
                            <th className="px-4 py-3 font-bold uppercase w-36">操作</th>
                        </tr>
                    </thead>
                    <tbody className="divide-y divide-(--border-divider)">
                        {items.length === 0 ? (
                            <tr><td colSpan={10} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">NO ERRORS FOUND</td></tr>
                        ) : (
                            items.map((item) => (
                                <tr key={item.id} className="hover:bg-(--bg-elevated) transition-colors">
                                    <td className="px-4 py-3 text-(--text-muted) font-mono text-xs">{fmtTime(item.created_at)}</td>
                                    <td className="px-4 py-3"><span className="inline-flex items-center gap-1 uppercase font-bold text-xs text-(--text-primary)">{sysLabel(item.system)}</span></td>
                                    <td className="px-4 py-3"><span className={`uppercase font-bold text-xs ${LEVEL_COLORS[item.level] || LEVEL_COLORS.error}`}>{LEVEL_LABELS[item.level] || item.level}</span></td>
                                    <td className="px-4 py-3 font-mono text-xs text-(--text-secondary) truncate max-w-36" title={item.source}>{item.source}</td>
                                    <td className="px-4 py-3 text-(--text-secondary) truncate max-w-100" title={item.message}>{item.message}</td>
                                    <td className="px-4 py-3 font-mono text-xs text-(--text-muted) truncate max-w-32" title={item.workflow_id}>{item.workflow_id || "-"}</td>
                                    <td className="px-4 py-3">
                                        {item.resolved
                                            ? <span className="text-(--success) text-xs font-bold">已处理</span>
                                            : <span className="text-(--danger) text-xs font-bold">未处理</span>}
                                    </td>
                                    <td className="px-4 py-3">
                                        {item.work_item_no ? (
                                            item.work_item_no.startsWith("card-") ? (
                                                // S9 §7.4 重设：分诊 Bug 卡关联（"card-<id>"）→ 跳 board-product 的 Bug 看板
                                                <a
                                                    href="/product/bugs" /* 同源：core-ui 单容器 /product 入口 */
                                                    target="_blank"
                                                    rel="noreferrer"
                                                    title={`Bug 卡 ${item.work_item_no}（board-product Bug 看板）`}
                                                    className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--accent) hover:bg-(--bg-hover) text-xs font-mono transition"
                                                >
                                                    <Bug className="h-3 w-3" />
                                                    卡 {item.work_item_no.slice(5)}
                                                </a>
                                            ) : (
                                                <button
                                                    onClick={() => navigate("/product/work-items")}
                                                    title={item.work_item_no}
                                                    className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--accent) hover:bg-(--bg-hover) text-xs font-mono transition"
                                                >
                                                    <Bug className="h-3 w-3" />
                                                    {item.work_item_no}
                                                </button>
                                            )
                                        ) : (
                                            <span className="text-(--text-muted) text-xs">-</span>
                                        )}
                                    </td>
                                    <td className="px-4 py-3">
                                        {item.trace_id ? (
                                            <a
                                                href={`/jaeger/trace/${item.trace_id}`}
                                                target="_blank"
                                                rel="noreferrer"
                                                title={item.trace_id}
                                                className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:bg-(--bg-hover) text-xs transition"
                                            >
                                                <ExternalLink className="h-3 w-3" />
                                                <span className="font-mono max-w-20 truncate">{item.trace_id.slice(0, 8)}</span>
                                            </a>
                                        ) : (
                                            <span className="text-(--text-muted) text-xs">-</span>
                                        )}
                                    </td>
                                    <td className="px-4 py-3">
                                        <div className="flex items-center gap-2">
                                            <button onClick={() => setDetail(item)} className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:bg-(--bg-hover) text-xs transition">
                                                <Eye className="h-3 w-3" />详情
                                            </button>
                                            {!item.resolved && (
                                                <button onClick={() => handleResolve(item.id)} disabled={busy === item.id} className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--success) hover:bg-(--bg-hover) text-xs transition">
                                                    <CheckCircle2 className="h-3 w-3" />处理
                                                </button>
                                            )}
                                            <button onClick={() => handleDelete(item.id)} disabled={busy === item.id} className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--danger) hover:bg-(--bg-hover) text-xs transition">
                                                <Trash2 className="h-3 w-3" />
                                            </button>
                                        </div>
                                    </td>
                                </tr>
                            ))
                        )}
                    </tbody>
                </table>
            </div>

            {/* 分页 */}
            <div className="mt-4 flex items-center justify-between text-sm text-(--text-muted)">
                <span className="font-mono text-xs">共 {total} 条 · 第 {page} / {totalPages} 页</span>
                <div className="flex items-center gap-2">
                    <button onClick={() => setPage((p) => Math.max(1, p - 1))} disabled={page <= 1} className="px-3 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-40">上一页</button>
                    <button onClick={() => setPage((p) => Math.min(totalPages, p + 1))} disabled={page >= totalPages} className="px-3 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-40">下一页</button>
                </div>
            </div>

            {/* 详情弹窗 */}
            {detail && (
                <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4" onClick={() => setDetail(null)}>
                    <div className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-3xl max-h-[80vh] overflow-hidden flex flex-col" onClick={(e) => e.stopPropagation()}>
                        <div className="flex items-center justify-between p-4 border-b border-(--border-subtle)">
                            <h2 className="font-bold text-lg">错误详情 #{detail.id}</h2>
                            <button onClick={() => setDetail(null)} className="text-(--text-muted) hover:text-white">✕</button>
                        </div>
                        <div className="p-4 overflow-y-auto" style={{ maxHeight: "60vh" }}>
                            <div className="grid grid-cols-2 gap-4 mb-4 text-sm">
                                <div><span className="text-(--text-muted)">系统</span><p className="font-bold text-xs uppercase">{sysLabel(detail.system)}</p></div>
                                <div><span className="text-(--text-muted)">级别</span><p className={`font-bold text-xs uppercase ${LEVEL_COLORS[detail.level] || LEVEL_COLORS.error}`}>{LEVEL_LABELS[detail.level] || detail.level}</p></div>
                                <div><span className="text-(--text-muted)">来源</span><p className="font-mono text-xs text-(--text-secondary)">{detail.source || "-"}</p></div>
                                <div><span className="text-(--text-muted)">时间</span><p className="font-mono text-xs text-(--text-secondary)">{fmtTime(detail.created_at)}</p></div>
                                <div><span className="text-(--text-muted)">Workflow ID</span><p className="font-mono text-xs text-(--text-secondary) truncate">{detail.workflow_id || "-"}</p></div>
                                <div><span className="text-(--text-muted)">Run ID</span><p className="font-mono text-xs text-(--text-secondary) truncate">{detail.run_id || "-"}</p></div>
                                <div className="col-span-2">
                                    <span className="text-(--text-muted)">Trace ID</span>
                                    {detail.trace_id ? (
                                        <p className="mt-1">
                                            <a href={`/jaeger/trace/${detail.trace_id}`} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:bg-(--bg-hover) text-xs transition">
                                                <ExternalLink className="h-3 w-3" />
                                                <span className="font-mono">{detail.trace_id}</span>
                                            </a>
                                        </p>
                                    ) : (
                                        <p className="font-mono text-xs text-(--text-muted)">-</p>
                                    )}
                                </div>
                            </div>
                            <div className="mb-4">
                                <span className="text-(--text-muted) text-xs font-bold uppercase mb-2 block">消息</span>
                                <pre className="bg-(--bg-elevated) p-3 text-xs text-red-400 overflow-auto max-h-48 whitespace-pre-wrap break-words">{detail.message}</pre>
                            </div>
                            {detail.context && detail.context !== "{}" && (
                                <div className="mb-4">
                                    <span className="text-(--text-muted) text-xs font-bold uppercase mb-2 block">上下文</span>
                                    <pre className="bg-(--bg-elevated) p-3 text-xs text-(--text-secondary) overflow-auto max-h-32 whitespace-pre-wrap break-words">{detail.context}</pre>
                                </div>
                            )}
                            {detail.stack && (
                                <div className="mb-4">
                                    <span className="text-(--text-muted) text-xs font-bold uppercase mb-2 block">堆栈</span>
                                    <pre className="bg-(--bg-elevated) p-3 text-xs text-(--text-secondary) overflow-auto max-h-48 whitespace-pre-wrap break-words">{detail.stack}</pre>
                                </div>
                            )}
                            {detail.resolved && (
                                <p className="text-xs text-(--success) font-bold">已于 {detail.resolved_at ? fmtTime(detail.resolved_at) : ""} 标记为已处理</p>
                            )}
                        </div>
                    </div>
                </div>
            )}
        </div>
    );
}
