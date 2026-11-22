import { useMemo, useState } from "react";
import { RefreshCw, Check, X, Loader2, FolderOpen } from "lucide-react";
import {
  listApprovals,
  getApproval,
  decideApproval,
  type Approval,
} from "../api";
import { worktreeEditorUrl } from "../utils";
import { useAsync } from "../hooks";

// 审批中心（S1 进化闭环，docs/plan/S1-进化闭环.md §3.9）
// 黑白极简：pending 列表（kind/subject/proposer/payload 摘要）+ 批准/驳回（带 reason）+ 历史。
// 不存在"默认通过"：超时（默认 24h）无人处理 → expired + 升级通知。

const STATUSES = ["pending", "approved", "rejected", "expired"] as const;

function fmtTime(v: string | null | undefined): string {
  if (!v) return "—";
  return v.replace("T", " ").slice(0, 19);
}

function fmtJSON(v: unknown): string {
  if (v === null || v === undefined) return "null";
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

function statusClass(status: string): string {
  const map: Record<string, string> = {
    pending: "text-white border-white",
    approved: "text-gray-300 border-gray-300",
    rejected: "text-gray-500 border-gray-500",
    expired: "text-gray-500 border-gray-500 opacity-70",
  };
  return `border px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-widest ${map[status] || "text-gray-500 border-gray-500"}`;
}

function Pre({ label, value }: { label: string; value: unknown }) {
  return (
    <div>
      <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">
        {label}
      </div>
      <pre className="text-[11px] leading-relaxed bg-black/40 border border-(--border-default) p-3 overflow-auto max-h-64 whitespace-pre-wrap break-all text-(--text-secondary)">
        {fmtJSON(value)}
      </pre>
    </div>
  );
}

// payload 摘要：diff 截断 + 关键键
function payloadSummary(p: any): string {
  if (p === null || p === undefined) return "—";
  const parts: string[] = [];
  for (const k of ["skill_name", "version", "rollback_to", "evolve_task_no", "report"]) {
    if (p[k] !== undefined) parts.push(`${k}=${p[k]}`);
  }
  if (typeof p.diff === "string" && p.diff.length > 0) {
    parts.push(`diff ${p.diff.length} bytes`);
  }
  return parts.length ? parts.join(" · ") : fmtJSON(p).slice(0, 200);
}

export function Approvals() {
  const [status, setStatus] = useState<string>("pending");
  // 每次渲染取一次当前时间，供行内过期判断（useMemo 隔离 impure 调用，purity）
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const nowMs = useMemo(() => Date.now(), []);

  const [expandedId, setExpandedId] = useState<number | null>(null);
  const [detail, setDetail] = useState<Approval | null>(null);

  const [deciding, setDeciding] = useState<null | { id: number; decision: "approve" | "reject" }>(null);
  const [reason, setReason] = useState("");
  const [approver, setApprover] = useState("web");
  const [decideMsg, setDecideMsg] = useState("");
  const [decideError, setDecideError] = useState("");

  const { data, loading, error, reload } = useAsync<Approval[]>(() => listApprovals(status, 100), [status]);
  const rows = data ?? [];

  // 展开某单 → 拉全量 payload
  const onExpand = async (a: Approval) => {
    if (expandedId === a.id) {
      setExpandedId(null);
      setDetail(null);
      return;
    }
    setExpandedId(a.id);
    setDetail(a);
    try {
      setDetail(await getApproval(a.id));
    } catch {
      // 列表数据兜底
    }
  };

  const onDecide = async (decision: "approve" | "reject") => {
    if (deciding) return;
    setDeciding({ id: 0, decision });
    setDecideMsg("");
    setDecideError("");
    try {
      // 决策对象 = 展开的那单；未展开时提示先展开
      if (!detail || !expandedId) {
        setDecideError("先点击展开要操作的审批单");
        return;
      }
      await decideApproval(expandedId, decision, approver, reason);
      setDecideMsg(`已${decision === "approve" ? "批准" : "驳回"}（approve 后自动执行 CD：灰度流 / 回滚）`);
      setExpandedId(null);
      setDetail(null);
      setReason("");
      await reload();
    } catch (e) {
      setDecideError(e instanceof Error ? e.message : String(e));
    } finally {
      setDeciding(null);
    }
  };

  return (
    <div className="p-6 h-full w-full overflow-auto">
      <div className="flex items-center justify-between mb-4">
        <div>
          <h1 className="text-lg font-bold tracking-tight text-white">审批中心</h1>
          <p className="text-[11px] text-(--text-muted) mt-0.5">
            进化链 / 巡检审批单 · 超时自动失效（不存在"默认通过"）
          </p>
        </div>
        <button
          type="button"
          onClick={() => void reload()}
          className="flex items-center gap-2 px-3 py-1.5 text-xs text-(--text-secondary) border border-(--border-default) hover:text-white hover:border-white transition-colors cursor-pointer"
        >
          <RefreshCw className="h-3.5 w-3.5" /> 刷新
        </button>
      </div>

      {/* 状态筛选 */}
      <div className="flex gap-1 mb-4">
        {["", ...STATUSES].map((s) => (
          <button
            key={s || "all"}
            type="button"
            onClick={() => setStatus(s)}
            className={`px-3 py-1.5 text-[11px] font-bold uppercase tracking-widest border transition-colors cursor-pointer ${
              status === s
                ? "bg-white text-black border-white"
                : "text-(--text-secondary) border-(--border-default) hover:text-white hover:border-white"
            }`}
          >
            {s === "" ? "全部" : s}
          </button>
        ))}
      </div>

      {error && (
        <div className="text-xs text-red-400 border border-red-400/40 p-3 mb-4">{error}</div>
      )}
      {(decideMsg || decideError) && (
        <div className={`text-xs p-3 mb-4 border ${decideError ? "text-red-400 border-red-400/40" : "text-white border-white/40"}`}>
          {decideMsg || decideError}
        </div>
      )}

      {loading ? (
        <div className="flex items-center gap-2 text-xs text-(--text-muted) p-6">
          <Loader2 className="h-4 w-4 animate-spin" /> 加载中…
        </div>
      ) : rows.length === 0 ? (
        <div className="text-xs text-(--text-muted) p-6 border border-(--border-default)">
          无审批单
        </div>
      ) : (
        <div className="space-y-2">
          {rows.map((a) => {
            const expired = a.status === "pending" && a.expires_at !== null && new Date(a.expires_at).getTime() < nowMs;
            return (
              <div key={a.id} className="border border-(--border-default)">
                <button
                  type="button"
                  onClick={() => void onExpand(a)}
                  className="w-full flex items-center gap-3 px-4 py-3 text-left hover:bg-(--bg-elevated) transition-colors cursor-pointer"
                >
                  <span className={`shrink-0 ${statusClass(a.status)}`}>{a.status}</span>
                  <span className="text-[11px] font-mono text-(--text-muted) shrink-0">{a.approval_no}</span>
                  <span className="text-[11px] font-bold uppercase tracking-widest text-(--text-secondary) shrink-0">
                    {a.kind}
                  </span>
                  <span className="text-xs text-(--text-secondary) truncate flex-1" title={a.subject}>
                    {a.subject}
                  </span>
                  <span className="text-[10px] text-(--text-muted) shrink-0 hidden md:inline">
                    proposer: {a.proposer}
                  </span>
                  <span className={`text-[10px] shrink-0 ${expired ? "text-red-400" : "text-(--text-muted)"}`}>
                    {a.expires_at ? `过期 ${fmtTime(a.expires_at)}` : "—"}
                  </span>
                  <span className="text-[10px] text-(--text-muted) shrink-0">{fmtTime(a.created_at)}</span>
                </button>

                {expandedId === a.id && detail && (
                  <div className="px-4 pb-4 border-t border-(--border-divider) space-y-3">
                    <div className="flex flex-wrap gap-4 text-[11px] text-(--text-secondary) py-2">
                      <span>摘要：<span className="text-white">{payloadSummary(detail.payload)}</span></span>
                      {detail.approver && <span>操作人：{detail.approver}</span>}
                      {detail.reason && <span>reason：{detail.reason}</span>}
                      {detail.payload?.evolve_task_no && (
                        <a
                          href={worktreeEditorUrl(String(detail.payload.evolve_task_no))}
                          target="_blank"
                          rel="noreferrer"
                          className="inline-flex items-center gap-1 px-2 py-0.5 border border-(--border-default) text-(--accent) hover:border-white transition-colors font-mono"
                          title="在编辑器中打开该进化单的工作目录"
                        >
                          <FolderOpen className="h-3 w-3" /> 工作目录
                        </a>
                      )}
                    </div>
                    <Pre label="Payload（diff 已截断，完整见 GET /approvals/:id）" value={detail.payload} />

                    {/* 操作区：仅 pending */}
                    {detail.status === "pending" && (
                      <div className="flex items-end gap-2">
                        <div className="flex-1">
                          <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">
                            reason
                          </div>
                          <input
                            value={reason}
                            onChange={(e) => setReason(e.target.value)}
                            placeholder="批准/驳回理由"
                            className="w-full text-xs bg-black/40 border border-(--border-default) px-3 py-2 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white"
                          />
                        </div>
                        <div>
                          <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">
                            approver
                          </div>
                          <input
                            value={approver}
                            onChange={(e) => setApprover(e.target.value)}
                            className="w-28 text-xs bg-black/40 border border-(--border-default) px-3 py-2 text-white focus:outline-none focus:border-white"
                          />
                        </div>
                        <button
                          type="button"
                          disabled={deciding !== null}
                          onClick={() => void onDecide("approve")}
                          className="flex items-center gap-1.5 px-4 py-2 text-xs font-bold bg-white text-black hover:opacity-80 disabled:opacity-40 transition-opacity cursor-pointer"
                        >
                          {deciding?.decision === "approve" ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Check className="h-3.5 w-3.5" />}
                          批准
                        </button>
                        <button
                          type="button"
                          disabled={deciding !== null}
                          onClick={() => void onDecide("reject")}
                          className="flex items-center gap-1.5 px-4 py-2 text-xs font-bold border border-white text-white hover:bg-white hover:text-black disabled:opacity-40 transition-colors cursor-pointer"
                        >
                          {deciding?.decision === "reject" ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <X className="h-3.5 w-3.5" />}
                          驳回
                        </button>
                      </div>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
