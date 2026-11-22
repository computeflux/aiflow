import React, { useState } from "react";
import { Loader2, RefreshCw, Eye, Clock, AlertCircle, CheckCircle, XCircle, History } from "lucide-react";
import { fetchWorkflows, fetchWorkflowDetail, type WorkflowExecution, type WorkflowDetail } from "../api";
import { useAsync } from "../hooks";

type WorkflowStatus = "all" | "running" | "completed" | "failed" | "terminated";

const STATUS_CONFIG: Record<string, { label: string; color: string; icon: React.ReactNode }> = {
  RUNNING: { label: "运行中", color: "text-(--info)", icon: <Clock className="h-3 w-3" /> },
  COMPLETED: { label: "已完成", color: "text-(--success)", icon: <CheckCircle className="h-3 w-3" /> },
  FAILED: { label: "失败", color: "text-(--danger)", icon: <XCircle className="h-3 w-3" /> },
  TERMINATED: { label: "已终止", color: "text-(--warning)", icon: <AlertCircle className="h-3 w-3" /> },
  CONTINUED_AS_NEW: { label: "续跑", color: "text-(--purple)", icon: <History className="h-3 w-3" /> },
  CANCELED: { label: "已取消", color: "text-(--warning)", icon: <AlertCircle className="h-3 w-3" /> },
  TIMED_OUT: { label: "超时", color: "text-(--danger)", icon: <XCircle className="h-3 w-3" /> },
  default: { label: "未知", color: "text-(--text-muted)", icon: <Clock className="h-3 w-3" /> },
};

const STATUS_FILTERS: { value: WorkflowStatus; label: string }[] = [
  { value: "all", label: "全部" }, { value: "running", label: "运行中" },
  { value: "completed", label: "已完成" }, { value: "failed", label: "失败" }, { value: "terminated", label: "已终止" },
];

export function Workflows() {
  const [selected, setSelected] = useState<WorkflowDetail | null>(null);
  const [status, setStatus] = useState<WorkflowStatus>("all");
  const [loadingDetail, setLoadingDetail] = useState(false);

  const { data: workflows, loading, error, reload: load, setError } = useAsync<WorkflowExecution[]>(
    async () => (await fetchWorkflows(status === "all" ? "" : status)).workflows,
    [status],
  );

  const handleDetail = async (wf: WorkflowExecution) => {
    setLoadingDetail(true);
    try { setSelected(await fetchWorkflowDetail(wf.workflow_id, wf.run_id)); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setLoadingDetail(false); }
  };

  const statusInfo = (s: string) => STATUS_CONFIG[s] || STATUS_CONFIG.default;

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase">Workflow 执行记录</h1>
          <p className="mt-1 text-sm text-(--text-muted)">查看所有工作流执行历史，支持按状态筛选</p>
        </div>
        <button onClick={() => void load()} disabled={loading} className="border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2">
          {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />} 刷新
        </button>
      </header>

      {error && <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">{error}</div>}

      <div className="mb-4 border border-(--border-subtle)  p-4">
        <div className="flex items-center gap-4 flex-wrap">
          <label className="text-xs font-bold uppercase text-(--text-muted)">状态:</label>
          {STATUS_FILTERS.map((f) => (
            <button key={f.value} onClick={() => setStatus(f.value)}
              className={`px-3 py-1 text-xs font-bold uppercase ${status === f.value ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>
              {f.label}
            </button>
          ))}
        </div>
      </div>

      <div className="border border-(--border-subtle) ">
        <table className="w-full text-left text-sm">
          <thead className="border-b border-(--border-subtle) text-(--text-muted)">
            <tr>
              <th className="px-4 py-3 font-bold uppercase w-48">Workflow ID</th>
              <th className="px-4 py-3 font-bold uppercase w-32">Run ID</th>
              <th className="px-4 py-3 font-bold uppercase w-40">类型</th>
              <th className="px-4 py-3 font-bold uppercase w-24">状态</th>
              <th className="px-4 py-3 font-bold uppercase w-44">开始时间</th>
              <th className="px-4 py-3 font-bold uppercase w-44">结束时间</th>
              <th className="px-4 py-3 font-bold uppercase w-16">操作</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-(--border-divider)">
            {(workflows ?? []).length === 0 ? (
              <tr><td colSpan={7} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">NO WORKFLOWS FOUND</td></tr>
            ) : (
              (workflows ?? []).map((wf) => {
                const s = statusInfo(wf.status);
                return (
                  <tr key={`${wf.workflow_id}-${wf.run_id}`} className="hover:bg-(--bg-elevated) transition-colors">
                    <td className="px-4 py-3 font-mono text-(--text-secondary) truncate max-w-45" title={wf.workflow_id}>{wf.workflow_id}</td>
                    <td className="px-4 py-3 font-mono text-(--text-muted) truncate max-w-45" title={wf.run_id}>{wf.run_id}</td>
                    <td className="px-4 py-3 uppercase font-bold text-xs text-(--text-primary)">{wf.type}</td>
                    <td className="px-4 py-3"><span className={`inline-flex items-center gap-1 ${s.color}`}>{s.icon} {s.label}</span></td>
                    <td className="px-4 py-3 text-(--text-muted) font-mono text-xs">{wf.start_time ? new Date(wf.start_time).toLocaleString() : "-"}</td>
                    <td className="px-4 py-3 text-(--text-muted) font-mono text-xs">{wf.close_time ? new Date(wf.close_time).toLocaleString() : "-"}</td>
                    <td className="px-4 py-3 text-right">
                      <button onClick={() => handleDetail(wf)} disabled={loadingDetail} className="inline-flex items-center gap-1 px-2 py-1 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:bg-(--bg-hover) text-xs transition">
                        <Eye className="h-3 w-3" />详情
                      </button>
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>

      {selected && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4" onClick={() => setSelected(null)}>
          <div className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-3xl max-h-[80vh] overflow-hidden flex flex-col" onClick={e => e.stopPropagation()}>
            <div className="flex items-center justify-between p-4 border-b border-(--border-subtle)">
              <h2 className="font-bold text-lg">Workflow 详情</h2>
              <button onClick={() => setSelected(null)} className="text-(--text-muted) hover:text-white">✕</button>
            </div>
            <div className="p-4 overflow-y-auto" style={{ maxHeight: "60vh" }}>
              <div className="grid grid-cols-2 gap-4 mb-4 text-sm">
                <div><span className="text-(--text-muted)">Workflow ID</span><p className="font-mono text-xs text-(--text-secondary) truncate">{selected.workflow_id}</p></div>
                <div><span className="text-(--text-muted)">Run ID</span><p className="font-mono text-xs text-(--text-secondary) truncate">{selected.run_id}</p></div>
                <div><span className="text-(--text-muted)">类型</span><p className="font-bold text-xs uppercase">{selected.type}</p></div>
                <div><span className="text-(--text-muted)">状态</span><p><span className={`inline-flex items-center gap-1 ${statusInfo(selected.status).color}`}>{statusInfo(selected.status).icon} {statusInfo(selected.status).label}</span></p></div>
                <div><span className="text-(--text-muted)">开始时间</span><p className="font-mono text-xs">{selected.start_time ? new Date(selected.start_time).toLocaleString() : "-"}</p></div>
                <div><span className="text-(--text-muted)">结束时间</span><p className="font-mono text-xs">{selected.close_time ? new Date(selected.close_time).toLocaleString() : "-"}</p></div>
                <div><span className="text-(--text-muted)">历史记录长度</span><p className="font-mono text-xs">{selected.history_length}</p></div>
              </div>
              {selected.memo && Object.keys(selected.memo).length > 0 && (
                <div className="mb-4"><span className="text-(--text-muted) text-xs font-bold uppercase mb-2 block">Memo</span>
                  <pre className="bg-(--bg-elevated) p-3 text-xs text-(--text-secondary) overflow-auto max-h-32">{JSON.stringify(selected.memo, null, 2)}</pre></div>
              )}
              {selected.search_attributes && Object.keys(selected.search_attributes).length > 0 && (
                <div><span className="text-(--text-muted) text-xs font-bold uppercase mb-2 block">Search Attributes</span>
                  <pre className="bg-(--bg-elevated) p-3 text-xs text-(--text-secondary) overflow-auto max-h-48">{JSON.stringify(selected.search_attributes, null, 2)}</pre></div>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
