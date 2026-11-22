import { useState } from "react";
import { FolderGit2, Loader2, RefreshCw, Save, Trash2, Power } from "lucide-react";
import {
    fetchProjects, scanProjects, updateProject, deleteProject,
    type Project,
} from "../api";
import { useAsync } from "../hooks";

// S9 §7.8 项目注册：把 apps/ 下的目录注册为工作流项目（1 项目 = 1 目录），
// 项目名 = 工作项 target_name = 卡片 labels / Bug 分诊的目标；启用且镜像在白名单内才会自动执行。
const RUNTIME_COLORS: Record<string, string> = {
    go: "text-(--info)", python: "text-(--ok)", node: "text-(--warning)",
};
const RUNTIME_LABELS: Record<string, string> = { go: "Go", python: "Python", node: "Node" };

interface EditState { image: string; test_cmd: string; description: string }

export function Projects() {
    const [scanning, setScanning] = useState(false);
    const [msg, setMsg] = useState<string | null>(null);
    const [editing, setEditing] = useState<number | null>(null);
    const [edit, setEdit] = useState<EditState>({ image: "", test_cmd: "", description: "" });
    const [busyId, setBusyId] = useState<number | null>(null);

    const { data: projects, loading, error, reload: load } = useAsync<Project[]>(() => fetchProjects(), []);

    const runScan = async () => {
        setScanning(true);
        setMsg(null);
        try {
            const res = await scanProjects();
            setMsg(`扫描完成：发现 ${res.scanned} 个目录，新注册 ${res.created}，刷新 ${res.updated}`);
            await load();
        } catch (e) {
            setMsg(e instanceof Error ? e.message : String(e));
        } finally {
            setScanning(false);
        }
    };

    const startEdit = (p: Project) => {
        setEditing(p.id);
        setEdit({ image: p.image || "", test_cmd: p.test_cmd || "", description: p.description || "" });
    };

    const save = async (p: Project) => {
        setBusyId(p.id);
        setMsg(null);
        try {
            await updateProject(p.id, { image: edit.image, test_cmd: edit.test_cmd, description: edit.description });
            setMsg(`项目 ${p.name} 已保存`);
            setEditing(null);
            await load();
        } catch (e) {
            setMsg(e instanceof Error ? e.message : String(e));
        } finally {
            setBusyId(null);
        }
    };

    const toggleActive = async (p: Project) => {
        setBusyId(p.id);
        setMsg(null);
        try {
            await updateProject(p.id, { active: !p.active });
            await load();
        } catch (e) {
            setMsg(e instanceof Error ? e.message : String(e));
        } finally {
            setBusyId(null);
        }
    };

    const remove = async (p: Project) => {
        if (!window.confirm(`删除项目注册「${p.name}」？（只删注册表记录，不动 apps/${p.name} 目录）`)) return;
        setBusyId(p.id);
        setMsg(null);
        try {
            await deleteProject(p.id);
            setMsg(`已删除项目 ${p.name}`);
            await load();
        } catch (e) {
            setMsg(e instanceof Error ? e.message : String(e));
        } finally {
            setBusyId(null);
        }
    };

    return (
        <div className="flex-1 flex flex-col overflow-hidden">
            <div className="flex items-center gap-3 px-6 py-4 border-b border-(--border-default)">
                <FolderGit2 className="h-5 w-5 text-(--text-secondary)" />
                <h1 className="text-lg font-semibold">项目</h1>
                <span className="text-xs text-(--text-muted)">
                    apps/ 目录 ↔ 项目（1:1）· 项目名 = 工作项目标 · 启用且镜像在白名单内才会自动执行
                </span>
                <div className="flex-1" />
                <button
                    onClick={runScan}
                    disabled={scanning}
                    className="flex items-center gap-1.5 px-3 py-1.5 text-xs bg-(--accent) text-black font-medium disabled:opacity-50"
                >
                    {scanning ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5" />}
                    扫描 apps 目录
                </button>
                <button
                    onClick={() => void load()}
                    className="flex items-center gap-1.5 px-3 py-1.5 text-xs border border-(--border-subtle) text-(--text-secondary) hover:text-white"
                >
                    <RefreshCw className="h-3.5 w-3.5" /> 刷新
                </button>
            </div>

            {(error || msg) && (
                <div className={`mx-6 mt-3 border p-2 text-xs ${error ? "border-(--danger)/50 text-(--danger)" : "border-(--border-subtle) text-(--text-secondary)"}`}>
                    {error ?? msg}
                </div>
            )}

            <div className="flex-1 overflow-auto px-6 py-4">
                {loading ? (
                    <div className="flex items-center gap-2 text-sm text-(--text-muted)">
                        <Loader2 className="h-4 w-4 animate-spin" /> 加载中…
                    </div>
                ) : (projects ?? []).length === 0 ? (
                    <div className="text-sm text-(--text-muted)">
                        还没有注册项目——点右上角「扫描 apps 目录」，把 apps/ 下的目录注册为项目。
                    </div>
                ) : (
                    <div className="border border-(--border-subtle)">
                        <table className="w-full text-xs">
                            <thead>
                                <tr className="border-b border-(--border-subtle) text-left text-(--text-muted)">
                                    <th className="px-3 py-2 font-medium">项目</th>
                                    <th className="px-3 py-2 font-medium">目录</th>
                                    <th className="px-3 py-2 font-medium">runtime</th>
                                    <th className="px-3 py-2 font-medium">沙箱镜像</th>
                                    <th className="px-3 py-2 font-medium">测试命令（空=默认）</th>
                                    <th className="px-3 py-2 font-medium">状态</th>
                                    <th className="px-3 py-2 font-medium w-28">操作</th>
                                </tr>
                            </thead>
                            <tbody>
                                {(projects ?? []).map((p) => (
                                    <tr key={p.id} className="border-b border-(--border-subtle)/50 align-top">
                                        <td className="px-3 py-2">
                                            <div className="font-medium text-(--text-primary)">{p.name}</div>
                                            {p.description && <div className="text-(--text-muted) mt-0.5 max-w-xs truncate" title={p.description}>{p.description}</div>}
                                        </td>
                                        <td className="px-3 py-2 font-mono text-(--text-secondary)">{p.dir}</td>
                                        <td className={`px-3 py-2 ${RUNTIME_COLORS[p.runtime] ?? ""}`}>{RUNTIME_LABELS[p.runtime] ?? p.runtime ?? "-"}</td>
                                        <td className="px-3 py-2">
                                            {editing === p.id ? (
                                                <input
                                                    value={edit.image}
                                                    onChange={(e) => setEdit((s) => ({ ...s, image: e.target.value }))}
                                                    className="w-40 bg-transparent border border-(--border-subtle) px-1.5 py-0.5 font-mono"
                                                    placeholder="如 pycraper:latest"
                                                />
                                            ) : (
                                                <span className="font-mono text-(--text-secondary)">{p.image || "（未配置）"}</span>
                                            )}
                                        </td>
                                        <td className="px-3 py-2">
                                            {editing === p.id ? (
                                                <input
                                                    value={edit.test_cmd}
                                                    onChange={(e) => setEdit((s) => ({ ...s, test_cmd: e.target.value }))}
                                                    className="w-56 bg-transparent border border-(--border-subtle) px-1.5 py-0.5 font-mono"
                                                    placeholder="默认按 runtime"
                                                />
                                            ) : (
                                                <span className="font-mono text-(--text-muted) max-w-xs inline-block truncate" title={p.test_cmd}>
                                                    {p.test_cmd || "（按 runtime 默认）"}
                                                </span>
                                            )}
                                        </td>
                                        <td className="px-3 py-2">
                                            <span className={p.active ? "text-(--ok)" : "text-(--text-muted)"}>
                                                {p.active ? "启用（可执行目标）" : "停用"}
                                            </span>
                                        </td>
                                        <td className="px-3 py-2">
                                            <div className="flex items-center gap-2">
                                                {editing === p.id ? (
                                                    <button
                                                        onClick={() => void save(p)}
                                                        disabled={busyId === p.id}
                                                        className="flex items-center gap-1 text-(--info) hover:text-white"
                                                    >
                                                        <Save className="h-3.5 w-3.5" /> 保存
                                                    </button>
                                                ) : (
                                                    <button onClick={() => startEdit(p)} className="text-(--text-muted) hover:text-white">编辑</button>
                                                )}
                                                <button
                                                    onClick={() => void toggleActive(p)}
                                                    disabled={busyId === p.id}
                                                    title={p.active ? "停用（不再作为自动执行目标）" : "启用"}
                                                    className="flex items-center gap-1 text-(--text-muted) hover:text-white"
                                                >
                                                    <Power className="h-3.5 w-3.5" /> {p.active ? "停用" : "启用"}
                                                </button>
                                                <button
                                                    onClick={() => void remove(p)}
                                                    disabled={busyId === p.id}
                                                    className="flex items-center gap-1 text-(--danger)/80 hover:text-(--danger)"
                                                >
                                                    <Trash2 className="h-3.5 w-3.5" />
                                                </button>
                                            </div>
                                        </td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                )}
                <div className="mt-3 text-[11px] text-(--text-muted)">
                    提示：项目镜像必须在 <span className="font-mono">SANDBOX_ALLOWED_IMAGES</span> 白名单内（当前 pycraper:latest / pyreplay:latest），
                    且项目的 .venv（python）或工具链需可用，S1 沙箱才能跑测试；board* 前端服务默认停用（人工维护红线）。
                </div>
            </div>
        </div>
    );
}
