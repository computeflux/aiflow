import { useCallback, useEffect, useState, Fragment } from "react";
import {
    Loader2, RefreshCw, Play, Power, PowerOff, Trash2, Plus, Edit3,
} from "lucide-react";
import {
    fetchIngestTasks, createIngestTask, updateIngestTask,
    deleteIngestTask, runIngestTask, spreadIngestTasks, restartCrons,
    type IngestTask,
} from "../api";
import { useSearchParams } from "react-router-dom";
import { ConfirmModal, Pagination } from "@kit";
import { fmtTime } from "../utils";

const PRESETS = [
    { label: "15分钟", cron: "*/15 * * * *" },
    { label: "30分钟", cron: "*/30 * * * *" },
    { label: "1小时", cron: "0 * * * *" },
    { label: "3小时", cron: "0 */3 * * *" },
    { label: "6小时", cron: "0 */6 * * *" },
    { label: "12小时", cron: "0 */12 * * *" },
    { label: "每天", cron: "0 0 * * *" },
];
function cronLabel(cron: string): string {
    const p = PRESETS.find((p) => p.cron === cron);
    return p ? p.label : cron;
}

const modeLabels: Record<string, string> = {
    keyword: "关键词",
    user: "用户推文",
};
function modeLabel(mode: string): string {
    return modeLabels[mode] || mode;
}

function getNextRunTime(schedule: string): Date | null {
    if (!schedule) return null;
    const parts = schedule.trim().split(/\s+/);
    if (parts.length !== 5) return null;
    const [minStr, hourStr, domStr, monthStr, dowStr] = parts;
    const now = new Date();

    const minEvery = minStr.startsWith("*/") ? parseInt(minStr.slice(2)) : null;
    const exactMin = /^\d+$/.test(minStr) ? parseInt(minStr) : null;
    const hourEvery = hourStr.startsWith("*/") ? parseInt(hourStr.slice(2)) : null;
    const exactHour = /^\d+$/.test(hourStr) ? parseInt(hourStr) : null;

    // Simple wildcard: * * * * *
    const wild = minStr === "*" && hourStr === "*" && domStr === "*" && monthStr === "*" && dowStr === "*";
    if (wild) return new Date(now.getTime() + 60000);

    // */N * * * * every N minutes
    if (minEvery && hourStr === "*" && domStr === "*" && monthStr === "*" && dowStr === "*") {
        const n = minEvery;
        const next = new Date(now);
        next.setMinutes(next.getMinutes() + n - (next.getMinutes() % n), 0, 0);
        if (next <= now) next.setMinutes(next.getMinutes() + n);
        return next;
    }

    // M */N * * * every N hours at minute M
    if (exactMin !== null && hourEvery && domStr === "*" && monthStr === "*" && dowStr === "*") {
        const n = hourEvery;
        const next = new Date(now);
        next.setMinutes(exactMin, 0, 0);
        const h = next.getHours();
        next.setHours(h + n - (h % n));
        if (next <= now) next.setHours(next.getHours() + n);
        return next;
    }

    // M H * * * daily at H:M
    if (exactMin !== null && exactHour !== null && domStr === "*" && monthStr === "*" && dowStr === "*") {
        const next = new Date(now);
        next.setHours(exactHour, exactMin, 0, 0);
        if (next <= now) next.setDate(next.getDate() + 1);
        return next;
    }

    // */N for minute, every hour
    if (minEvery && hourStr === "*" && domStr === "*" && monthStr === "*" && dowStr === "*") {
        const n = minEvery;
        const next = new Date(now);
        next.setMinutes(next.getMinutes() + n - (next.getMinutes() % n), 0, 0);
        if (next <= now) next.setMinutes(next.getMinutes() + n);
        return next;
    }

    // Default: try parsing as simple cron, give up
    return null;
}

function formatTime(d: Date | null): string {
    if (!d) return "-";
    const now = new Date();
    const pad = (n: number) => String(n).padStart(2, "0");
    const time = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    const tmr = new Date(now);
    tmr.setHours(23, 59, 59, 999);
    if (d > tmr) return `明天 ${time}`;
    return `今天 ${time}`;
}

export function IngestTasksPage() {
    const [tasks, setTasks] = useState<IngestTask[]>([]);
    const [tasksTotal, setTasksTotal] = useState(0);
    const [tasksPage, setTasksPage] = useState(1);
    const [taskSourceFilter, setTaskSourceFilter] = useState("all");
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [success, setSuccess] = useState<string | null>(null);
    const [deleteConfirm, setDeleteConfirm] = useState<IngestTask | null>(null);

    const [showForm, setShowForm] = useState(false);
    const [bulkMode, setBulkMode] = useState(false);
    const [newSource, setNewSource] = useState("x");
    const [newKeyword, setNewKeyword] = useState("");
    const [newLimit, setNewLimit] = useState("");
    const [newSchedule, setNewSchedule] = useState("");
    const [newMode, setNewMode] = useState("keyword");
    const [newParallelCount, setNewParallelCount] = useState("");
    const [bulkKeywords, setBulkKeywords] = useState("");

    const [editingTaskId, setEditingTaskId] = useState<number | null>(null);
    const [editSource, setEditSource] = useState("x");
    const [editKeyword, setEditKeyword] = useState("");
    const [editLimit, setEditLimit] = useState("");
    const [editMode, setEditMode] = useState("keyword");
    const [editParallelCount, setEditParallelCount] = useState("");
    const [editSchedule, setEditSchedule] = useState("");

    const load = useCallback(async () => {
        setLoading(true);
        try {
            const data = await fetchIngestTasks(tasksPage);
            setTasks(data.tasks || []);
            setTasksTotal(data.total || 0);
        } catch (err) {
            setError(String(err));
        } finally {
            setLoading(false);
        }
    }, [tasksPage]);

    useEffect(() => { load(); }, [load]);

    // 从邮件推广页跳转过来时（?new=1&source=github&keyword=xxx），自动打开新建表单并预填
    const [searchParams, setSearchParams] = useSearchParams();
    useEffect(() => {
        if (searchParams.get("new") !== "1") return;
        const src = searchParams.get("source") || "x";
        setNewSource(src);
        setNewKeyword(searchParams.get("keyword") || "");
        setBulkMode(false);
        setEditingTaskId(null);
        setShowForm(true);
        setSearchParams({}, { replace: true });
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    const resetForm = () => {
        setNewSource("x");
        setNewKeyword("");
        setNewLimit("");
        setNewSchedule("");
        setNewMode("keyword");
        setNewParallelCount("");
        setBulkKeywords("");
        setShowForm(false);
        setBulkMode(false);
    };

    const handleCreate = async (e: React.FormEvent) => {
        e.preventDefault();
        try {
            if (editingTaskId) {
                await updateIngestTask(editingTaskId, {
                    source: newSource,
                    keyword: newKeyword.trim(),
                    limit: newLimit ? Number(newLimit) : undefined,
                    mode: (newSource === "x" || newSource === "reddit") ? newMode.trim() || undefined : undefined,
                    schedule: newSchedule.trim() || undefined,
                });
            } else {
                await createIngestTask({
                    source: newSource,
                    keyword: newKeyword.trim(),
                    limit: newLimit ? Number(newLimit) : undefined,
                    mode: (newSource === "x" || newSource === "reddit") ? newMode.trim() || undefined : undefined,
                    schedule: newSchedule.trim() || undefined,
                    parallel_count: newParallelCount ? Number(newParallelCount) : undefined,
                });
            }
            resetForm();
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    const handleEdit = (task: IngestTask) => {
        setEditingTaskId(task.id);
        setEditSource(task.source);
        setEditKeyword(task.keyword);
        setEditLimit(task.limit ? String(task.limit) : "");
        setEditMode(task.mode || "keyword");
        setEditParallelCount(task.parallel_count ? String(task.parallel_count) : "");
        setEditSchedule(task.schedule || "");
    };

    const handleEditSave = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!editingTaskId) return;
        try {
            await updateIngestTask(editingTaskId, {
                source: editSource,
                keyword: editKeyword.trim(),
                limit: editLimit ? Number(editLimit) : undefined,
                mode: (editSource === "x" || editSource === "reddit") ? editMode : undefined,
                schedule: editSchedule.trim() || undefined,
                parallel_count: editParallelCount ? Number(editParallelCount) : undefined,
            });
            setEditingTaskId(null);
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    const handleBulkCreate = async (e: React.FormEvent) => {
        e.preventDefault();
        try {
            const lines = bulkKeywords
                .split(/[\n,]+/)
                .map((s) => s.trim())
                .filter(Boolean);
            let created = 0;
            for (let i = 0; i < lines.length; i++) {
                try {
                    await createIngestTask({
                        source: newSource,
                        keyword: lines[i],
                        limit: newLimit ? Number(newLimit) : undefined,
                        mode: (newSource === "x" || newSource === "reddit") ? newMode.trim() || undefined : undefined,
                        schedule: newSchedule.trim() || undefined,
                        parallel_count: newParallelCount ? Number(newParallelCount) : undefined,
                    });
                    created++;
                } catch (err) {
                    setError(`已创建 ${created} 个，第 ${i + 1} 个失败: ${err instanceof Error ? err.message : String(err)}`);
                    return;
                }
            }
            resetForm();
            await load();
        } catch (err) {
            setError(String(err));
        }
    };

    const handleToggle = async (task: IngestTask) => {
        try {
            await updateIngestTask(task.id, { enabled: !task.enabled });
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    const handleRun = async (task: IngestTask) => {
        try {
            await runIngestTask(task.id);
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    const handleDelete = async () => {
        if (!deleteConfirm) return;
        try {
            await deleteIngestTask(deleteConfirm.id);
            setDeleteConfirm(null);
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        }
    };

    const handleRestartCrons = async () => {
        try {
            const r = await restartCrons();
            setSuccess(`Cron 修复完成: ${r.success} 成功, ${r.failed} 失败`);
            await load();
        } catch (err) {
            setError(String(err));
        }
    };

    return (
        <div className="flex-1 p-4 overflow-y-auto">
            <header className="mb-6 flex items-center justify-between">
                <div>
                    <h1 className="text-2xl font-bold tracking-tighter uppercase">
                        定时抓取任务
                    </h1>
                    <p className="mt-1 text-sm text-(--text-muted)">
                        管理定时抓取数据源
                    </p>
                </div>
                <button
                    onClick={load}
                    disabled={loading}
                    className="border border-(--border-subtle) px-4 py-2 text-sm font-bold hover:bg-(--bg-elevated) transition disabled:opacity-50 flex items-center gap-2"
                >
                    {loading ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                    ) : (
                        <RefreshCw className="h-4 w-4" />
                    )}{" "}
                    REFRESH
                </button>
            </header>

            {error && (
                <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">
                    {error}
                </div>
            )}
            {success && (
                <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-(--success)">
                    {success}
                </div>
            )}

            <div className="border border-(--border-subtle) ">
                <div className="p-4 border-b border-(--border-subtle) flex items-center justify-between">
                    <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted)">
                        抓取任务列表
                    </h2>
                    <div className="flex items-center gap-1">
                        <button
                            onClick={async () => {
                                try {
                                    const r = await spreadIngestTasks(taskSourceFilter);
                                    setSuccess(`已打散 ${r.count} 个任务 (${taskSourceFilter})`);
                                    await load();
                                } catch (err) {
                                    setError(String(err));
                                }
                            }}
                            className="border border-(--border-subtle) px-2 py-1 text-[10px] text-(--text-secondary) hover:bg-(--bg-elevated)"
                        >
                            打散
                        </button>
                        <button
                            onClick={handleRestartCrons}
                            className="border border-(--warning) px-2 py-1 text-[10px] text-(--warning) hover:bg-(--warning) hover:text-black"
                        >
                            修复Cron
                        </button>
                        <button
                            onClick={() => {
                                setBulkMode(false);
                                setEditingTaskId(null);
                                setShowForm(true);
                            }}
                            className="flex items-center gap-1 border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
                        >
                            <Plus className="h-3 w-3" />
                            单个
                        </button>
                        <button
                            onClick={() => {
                                setBulkMode(true);
                                setEditingTaskId(null);
                                setShowForm(true);
                            }}
                            className="flex items-center gap-1 border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
                        >
                            <Plus className="h-3 w-3" />
                            批量
                        </button>
                    </div>
                </div>

                {/* 渠道筛选 */}
                <div className="flex flex-wrap gap-1 mb-3 mt-4 px-4">
                    {[
                        { v: "all", l: "全部" },
                        { v: "x", l: "X" },
                        { v: "github", l: "GitHub" },
                        { v: "reddit", l: "Reddit" },
                    ].map((s) => (
                        <button
                            key={s.v}
                            onClick={() => {
                                setTaskSourceFilter(s.v);
                                setTasksPage(1);
                            }}
                            className={`border mr-1 px-2 py-0.5 text-[11px] font-bold uppercase ${taskSourceFilter === s.v ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
                        >
                            {s.l}
                        </button>
                    ))}
                </div>

                {/* 新建表单 */}
                {showForm && !editingTaskId && (
                    <form
                        onSubmit={bulkMode ? handleBulkCreate : handleCreate}
                        className="p-4 border-b border-(--border-subtle) bg-(--bg-surface) space-y-3"
                    >
                        <div className="flex flex-wrap gap-3 items-end">
                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                                    来源
                                </label>
                                <select
                                    value={newSource}
                                    onChange={(e) => setNewSource(e.target.value)}
                                    className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                                >
                                    <option value="x">X (Twitter)</option>
                                    <option value="github">GitHub</option>
                                    <option value="reddit">Reddit</option>
                                </select>
                            </div>
                            {bulkMode ? (
                                <div className="flex-1 min-w-[300px]">
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                                        关键词（一行一个，或用逗号分隔）
                                    </label>
                                    <textarea
                                        value={bulkKeywords}
                                        onChange={(e) => setBulkKeywords(e.target.value)}
                                        placeholder="AI agent\nblockchain\nDeFi"
                                        rows={3}
                                        className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white resize-none font-mono"
                                    />
                                </div>
                            ) : (
                                <div className="flex-1 min-w-[200px]">
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                                        关键词
                                    </label>
                                    <input
                                        value={newKeyword}
                                        onChange={(e) => setNewKeyword(e.target.value)}
                                        placeholder={newSource === "reddit" ? "可选，用作标签" : "搜索关键词"}
                                        required={newSource !== "reddit"}
                                        className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                                    />
                                </div>
                            )}
                            {!bulkMode && (newSource === "x" || newSource === "reddit") && (
                                <div>
                                    <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                                        模式
                                    </label>
                                    <select
                                        value={newMode}
                                        onChange={(e) => setNewMode(e.target.value)}
                                        className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                                    >
                                        <option value="keyword">keyword - 关键词搜索</option>
                                        <option value="user">user - 用户推文</option>
                                    </select>
                                </div>
                            )}
                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                                    数量
                                </label>
                                <input
                                    value={newLimit}
                                    onChange={(e) => setNewLimit(e.target.value)}
                                    placeholder="默认"
                                    className="w-20 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                                />
                            </div>
                            <div>
                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                                    并行度
                                </label>
                                <input
                                    value={newParallelCount}
                                    onChange={(e) => setNewParallelCount(e.target.value)}
                                    placeholder="1"
                                    className="w-16 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                                />
                            </div>
                        </div>
                        <div>
                            <label className="block text-[10px] uppercase text-(--text-muted) mb-2">
                                执行频率
                            </label>
                            <div className="flex flex-wrap gap-2 mb-2">
                                {PRESETS.map((p) => (
                                    <button
                                        key={p.cron}
                                        type="button"
                                        onClick={() => setNewSchedule(p.cron)}
                                        className={`px-3 py-1 text-xs font-bold border transition ${newSchedule === p.cron ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
                                    >
                                        {p.label}
                                    </button>
                                ))}
                                <button
                                    type="button"
                                    onClick={() => setNewSchedule("")}
                                    className={`px-3 py-1 text-xs font-bold border transition ${!newSchedule ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-muted) hover:bg-(--bg-elevated)"}`}
                                >
                                    手动
                                </button>
                            </div>
                            <input
                                value={newSchedule}
                                onChange={(e) => setNewSchedule(e.target.value)}
                                placeholder="或输入自定义 cron..."
                                className="w-48 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm font-mono focus:outline-none focus:border-white"
                            />
                        </div>
                        <button
                            type="submit"
                            className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm hover:bg-(--text-primary)"
                        >
                            创建任务
                        </button>
                    </form>
                )}

                <table className="w-full text-left text-sm">
                    <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                        <tr>
                            <th className="px-4 py-3 font-bold uppercase w-12">ID</th>
                            <th className="px-4 py-3 font-bold uppercase">来源</th>
                            <th className="px-4 py-3 font-bold uppercase">关键词</th>
                            <th className="px-4 py-3 font-bold uppercase">模式</th>
                            <th className="px-4 py-3 font-bold uppercase">频率</th>
                            <th className="px-4 py-3 font-bold uppercase">状态</th>
                            <th className="px-4 py-3 font-bold uppercase">上次</th>
                            <th className="px-4 py-3 font-bold uppercase">下次执行</th>
                            <th className="px-4 py-3 text-right font-bold uppercase">操作</th>
                        </tr>
                    </thead>
                    <tbody className="divide-y divide-(--border-divider)">
                        {tasks.filter(
                            (t) => taskSourceFilter === "all" || t.source === taskSourceFilter,
                        ).length === 0 ? (
                            <tr>
                                <td colSpan={9} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
                                    NO INGEST TASKS
                                </td>
                            </tr>
                        ) : (
                            tasks
                                .filter((t) => taskSourceFilter === "all" || t.source === taskSourceFilter)
                                .map((task) => (
                                    <Fragment key={task.id}>
                                        <tr className="hover:bg-(--bg-elevated) transition-colors">
                                        <td className="px-4 py-3 font-mono text-(--text-secondary) text-xs">{task.id}</td>
                                        <td className="px-4 py-3 uppercase font-bold text-xs">
                                            <span className="px-1.5 py-0.5 border border-(--border-subtle) bg-(--bg-elevated)">{task.source}</span>
                                        </td>
                                        <td className="px-4 py-3 text-(--text-primary) font-medium text-sm">{task.keyword}</td>
                                        <td className="px-4 py-3">
                                            <span className="text-xs text-(--text-secondary) border border-(--border-subtle) px-1.5 py-0.5">
                                                {task.source === "x" ? modeLabel(task.mode || "keyword") : "-"}
                                            </span>
                                        </td>
                                        <td className="px-4 py-3">
                                            <span className="text-xs text-(--text-secondary) border border-(--border-subtle) px-1.5 py-0.5 font-mono">
                                                {cronLabel(task.schedule || "手动")}
                                            </span>
                                        </td>
                                        <td className="px-4 py-3">
                                            {task.schedule ? (
                                                <span className={`text-xs font-bold uppercase ${task.enabled ? "text-(--success)" : "text-(--text-muted)"}`}>
                                                    {task.enabled ? "启用" : "暂停"}
                                                </span>
                                            ) : (
                                                <span className="text-xs text-(--text-muted)">手动</span>
                                            )}
                                        </td>
                                        <td className="px-4 py-3 text-(--text-muted) text-xs">
                                            {fmtTime(task.last_run_at)}
                                        </td>
                                        <td className="px-4 py-3 text-(--text-muted) text-xs">
                                            {task.enabled && task.schedule ? formatTime(getNextRunTime(task.schedule)) : "-"}
                                        </td>
                                        <td className="px-4 py-3 text-right">
                                            <div className="flex items-center justify-end gap-1">
                                                <button onClick={() => handleRun(task)} title="立刻执行"
                                                    className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition">
                                                    <Play className="h-3 w-3" />
                                                </button>
                                                <button onClick={() => handleEdit(task)} title="编辑"
                                                    className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition">
                                                    <Edit3 className="h-3 w-3" />
                                                </button>
                                                {task.source === "x" && (
                                                    <button onClick={() => handleToggle(task)}
                                                        title={task.enabled ? "暂停" : "恢复"}
                                                        className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition">
                                                        {task.enabled ? <PowerOff className="h-3 w-3" /> : <Power className="h-3 w-3" />}
                                                    </button>
                                                )}
                                                <button onClick={() => setDeleteConfirm(task)} title="删除"
                                                    className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-red-500 hover:border-red-500 transition">
                                                    <Trash2 className="h-3 w-3" />
                                                </button>
                                            </div>
                                        </td>
                                    </tr>

                                    {/* 编辑表单 */}
                                    {editingTaskId === task.id && (
                                        <tr className="bg-(--bg-surface)">
                                            <td colSpan={9} className="p-4 border-b border-(--border-subtle)">
                                                <form onSubmit={handleEditSave} className="space-y-3">
                                                    <div className="flex flex-wrap gap-3 items-end">
                                                        <div>
                                                            <label className="block text-[10px] uppercase text-(--text-muted) mb-1">来源</label>
                                                            <select value={editSource} onChange={(e) => setEditSource(e.target.value)}
                                                                className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm">
                                                                <option value="x">X (Twitter)</option>
                                                                <option value="github">GitHub</option>
                                                                <option value="reddit">Reddit</option>
                                                            </select>
                                                        </div>
                                                        <div className="flex-1 min-w-[200px]">
                                                            <label className="block text-[10px] uppercase text-(--text-muted) mb-1">关键词</label>
                                                            <input value={editKeyword} onChange={(e) => setEditKeyword(e.target.value)} required
                                                                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm" />
                                                        </div>
                                                        {(editSource === "x" || editSource === "reddit") && (
                                                            <div>
                                                                <label className="block text-[10px] uppercase text-(--text-muted) mb-1">模式</label>
                                                                <select value={editMode} onChange={(e) => setEditMode(e.target.value)}
                                                                    className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm">
                                                                    <option value="keyword">keyword</option>
                                                                    <option value="user">user</option>
                                                                </select>
                                                            </div>
                                                        )}
                                                        <div>
                                                            <label className="block text-[10px] uppercase text-(--text-muted) mb-1">数量</label>
                                                            <input value={editLimit} onChange={(e) => setEditLimit(e.target.value)} placeholder="默认"
                                                                className="w-20 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm" />
                                                        </div>
                                                        <div>
                                                            <label className="block text-[10px] uppercase text-(--text-muted) mb-1">并行度</label>
                                                            <input value={editParallelCount} onChange={(e) => setEditParallelCount(e.target.value)} placeholder="1"
                                                                className="w-16 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm" />
                                                        </div>
                                                    </div>
                                                    <div>
                                                        <label className="block text-[10px] uppercase text-(--text-muted) mb-2">执行频率</label>
                                                        <div className="flex flex-wrap gap-2 mb-2">
                                                            {PRESETS.map((p) => (
                                                                <button key={p.cron} type="button" onClick={() => setEditSchedule(p.cron)}
                                                                    className={`px-3 py-1 text-xs font-bold border transition ${editSchedule === p.cron ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
                                                                    {p.label}
                                                                </button>
                                                            ))}
                                                            <button type="button" onClick={() => setEditSchedule("")}
                                                                className={`px-3 py-1 text-xs font-bold border transition ${!editSchedule ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-muted) hover:bg-(--bg-elevated)"}`}>
                                                                手动
                                                            </button>
                                                        </div>
                                                        <input value={editSchedule} onChange={(e) => setEditSchedule(e.target.value)}
                                                            placeholder="或输入自定义 cron..."
                                                            className="w-48 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm font-mono" />
                                                    </div>
                                                    <div className="flex items-center gap-2">
                                                        <button type="submit" className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm">保存修改</button>
                                                        <button type="button" onClick={() => setEditingTaskId(null)}
                                                            className="border border-(--border-subtle) text-(--text-secondary) px-4 py-1.5 text-sm font-bold">取消</button>
                                                    </div>
                                                </form>
                                            </td>
                                        </tr>
                                    )}
                                </Fragment>
                                ))
                        )}
                    </tbody>
                </table>
                <Pagination page={tasksPage} pageSize={50} total={tasksTotal} onPageChange={setTasksPage} />
            </div>

            {deleteConfirm && (
                <ConfirmModal
                    open={true}
                    title="删除任务"
                    message={`确认删除 "${deleteConfirm.keyword || ''}"？`}
                    onConfirm={handleDelete}
                    onCancel={() => setDeleteConfirm(null)}
                />
            )}
        </div>
    );
}
