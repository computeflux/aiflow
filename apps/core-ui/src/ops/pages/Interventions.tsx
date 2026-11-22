import { useState } from "react";
import { RefreshCw, Loader2, CircleDot, Circle, Hand, Zap, RotateCcw, Undo2, Check } from "lucide-react";
import {
  fetchInterventions,
  fetchActiveWorkflows,
  fetchCircuitStates,
  takeoverIntervention,
  injectIntervention,
  retryIntervention,
  rollbackIntervention,
  resolveIntervention,
  type Intervention,
  type ActiveWorkflow,
  type CircuitState,
} from "../api";

import { useAsync } from "../hooks";
// 干预舱（S3，docs/plan/S3-干预舱.md §3.4）：
// 顶部活跃 workflow 表（含断路器圆点）+ open 干预记录（接管/注入/重试/回滚）+ 解决。
// 黑白极简；所有动作写 audit_logs 留痕，回滚走 S1 审批。

const WORKFLOW_TYPES = ["weekly_content", "fetch", "generic_skill"] as const;

function fmtTime(v: string | null | undefined): string {
  if (!v) return "—";
  return v.replace("T", " ").slice(0, 19);
}

// 断路器圆点（黑白主题：实心=open，空心=close，半=half）
function CircuitDot({ state }: { state: string }) {
  const base = "inline-block h-2.5 w-2.5 rounded-full align-middle";
  if (state === "open") return <span className={`${base} bg-white`} title="circuit open" />;
  if (state === "half") return <span className={`${base} bg-white/50`} title="circuit half-open" />;
  return <span className={`${base} border border-(--text-secondary)`} title="circuit close" />;
}

type ModalAction = "takeover" | "inject" | "retry" | "rollback";

function defaultBody(action: ModalAction, item: Intervention): string {
  switch (action) {
    case "inject":
      return JSON.stringify({ name: "inject", payload: {} }, null, 2);
    case "rollback":
      return "";
    case "takeover":
    case "retry":
    default:
      return JSON.stringify({ workflow: "weekly_content", input: { week: item.workflow_id } }, null, 2);
  }
}

export function Interventions() {

  const [status, setStatus] = useState<string>("open");
  const [expandedId, setExpandedId] = useState<number | null>(null);
  const [note, setNote] = useState("");

  const [modal, setModal] = useState<{ id: number; action: ModalAction } | null>(null);
  const [bodyText, setBodyText] = useState("");
  const [rbSkill, setRbSkill] = useState("");
  const [rbTo, setRbTo] = useState("");
  const [acting, setActing] = useState(false);
  const [msg, setMsg] = useState("");
  const [err, setErr] = useState("");

  const { data, loading, error, reload } = useAsync<{ rows: Intervention[]; workflows: ActiveWorkflow[]; circuits: CircuitState[] }>(
    async () => {
      const [ints, wfs, cks] = await Promise.all([
        fetchInterventions(status),
        fetchActiveWorkflows(),
        fetchCircuitStates(),
      ]);
      return { rows: ints, workflows: wfs, circuits: cks };
    },
    [status],
  );
  const rows = data?.rows ?? [];
  const workflows = data?.workflows ?? [];
  const circuits = data?.circuits ?? [];

  const openModal = (item: Intervention, action: ModalAction) => {
    setModal({ id: item.id, action });
    setBodyText(defaultBody(action, item));
    setRbSkill("");
    setRbTo("");
    setMsg("");
    setErr("");
  };

  const runAction = async () => {
    if (!modal || acting) return;
    setActing(true);
    setMsg("");
    setErr("");
    try {
      if (modal.action === "rollback") {
        if (!rbSkill.trim() || !rbTo.trim()) throw new Error("skill_name + rollback_to 必填");
        const r = await rollbackIntervention(modal.id, { skill_name: rbSkill.trim(), rollback_to: rbTo.trim() });
        setMsg(`回滚审批单已创建：${r.approval_no}（走 S1 审批，非直接执行）`);
      } else {
        let body: Record<string, any>;
        try {
          body = JSON.parse(bodyText || "{}");
        } catch {
          throw new Error("参数不是合法 JSON");
        }
        if (modal.action === "takeover") await takeoverIntervention(modal.id, body as any);
        else if (modal.action === "inject") await injectIntervention(modal.id, body as any);
        else await retryIntervention(modal.id, body as any);
        setMsg(`${modal.action} 成功（${body.workflow ? `new workflow: ${body.workflow}` : "ok"}）`);
      }
      setModal(null);
      setExpandedId(null);
      await reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setActing(false);
    }
  };

  const onResolve = async (item: Intervention) => {
    if (acting) return;
    setActing(true);
    setMsg("");
    setErr("");
    try {
      await resolveIntervention(item.id, note.trim());
      setMsg(`intervention #${item.id} 已标记 resolved`);
      setNote("");
      setExpandedId(null);
      await reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setActing(false);
    }
  };

  return (
    <div className="p-6 h-full w-full overflow-auto">
      <div className="flex items-center justify-between mb-4">
        <div>
          <h1 className="text-lg font-bold tracking-tight text-white">干预舱</h1>
          <p className="text-[11px] text-(--text-muted) mt-0.5">
            熔断器 · 死循环检测 · 人工接管/注入/重试/回滚（全程 audit_logs 留痕）
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

      {error && <div className="text-xs text-red-400 border border-red-400/40 p-3 mb-4">{error}</div>}
      {(msg || err) && (
        <div className={`text-xs p-3 mb-4 border ${err ? "text-red-400 border-red-400/40" : "text-white border-white/40"}`}>
          {msg || err}
        </div>
      )}

      {/* 活跃 workflow 表 */}
      <section className="mb-6">
        <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">
          运行中 Workflow（{workflows.length}）
        </div>
        {loading ? (
          <div className="flex items-center gap-2 text-xs text-(--text-muted) p-4">
            <Loader2 className="h-4 w-4 animate-spin" /> 扫描中…
          </div>
        ) : workflows.length === 0 ? (
          <div className="text-xs text-(--text-muted) p-4 border border-(--border-default)">无运行中 workflow</div>
        ) : (
          <div className="border border-(--border-default) overflow-auto">
            <table className="w-full text-left text-[11px]">
              <thead>
                <tr className="text-(--text-muted) border-b border-(--border-divider)">
                  <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">workflow_id</th>
                  <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">namespace</th>
                  <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">开始时间</th>
                  <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">事件数</th>
                  <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">连续失败</th>
                  <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">最近错误</th>
                </tr>
              </thead>
              <tbody>
                {workflows.map((w) => (
                  <tr key={`${w.namespace}/${w.workflow_id}`} className="border-b border-(--border-divider) last:border-b-0 hover:bg-(--bg-elevated)">
                    <td className="px-3 py-2 font-mono text-(--text-secondary)">{w.workflow_id}</td>
                    <td className="px-3 py-2 text-(--text-muted)">{w.namespace}</td>
                    <td className="px-3 py-2 text-(--text-muted)">{fmtTime(w.start_time)}</td>
                    <td className="px-3 py-2 text-(--text-secondary)">{w.event_count}</td>
                    <td className="px-3 py-2">
                      <span className={w.max_consecutive_activity_failures >= 10 ? "text-white font-bold" : "text-(--text-muted)"}>
                        {w.max_consecutive_activity_failures}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-(--text-muted) max-w-[240px] truncate" title={w.last_error}>{w.last_error || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {/* 断路器状态 */}
      <section className="mb-6">
        <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">
          技能熔断器（{circuits.length}）
        </div>
        {circuits.length === 0 ? (
          <div className="text-xs text-(--text-muted) p-4 border border-(--border-default)">
            暂无已知熔断器（skill 首次被调用后出现；<Circle className="inline h-2.5 w-2.5 align-middle" /> close / <CircleDot className="inline h-2.5 w-2.5 align-middle" /> open）
          </div>
        ) : (
          <div className="flex flex-wrap gap-2">
            {circuits.map((c) => (
              <span key={c.skill_name} className="flex items-center gap-1.5 px-2.5 py-1.5 text-[11px] border border-(--border-default) text-(--text-secondary)">
                <CircuitDot state={c.state} />
                <span className="font-mono">{c.skill_name}</span>
                <span className="text-(--text-muted) uppercase tracking-widest text-[10px]">{c.state}</span>
              </span>
            ))}
          </div>
        )}
      </section>

      {/* 干预记录 */}
      <section>
        <div className="flex items-center justify-between mb-2">
          <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted)">
            干预记录（{rows.length}）
          </div>
          <div className="flex gap-1">
            {["open", "resolved", ""].map((s) => (
              <button
                key={s || "all"}
                type="button"
                onClick={() => setStatus(s)}
                className={`px-2.5 py-1 text-[10px] font-bold uppercase tracking-widest border transition-colors cursor-pointer ${
                  status === s ? "bg-white text-black border-white" : "text-(--text-secondary) border-(--border-default) hover:text-white hover:border-white"
                }`}
              >
                {s === "" ? "全部" : s}
              </button>
            ))}
          </div>
        </div>

        {rows.length === 0 ? (
          <div className="text-xs text-(--text-muted) p-6 border border-(--border-default)">无干预记录</div>
        ) : (
          <div className="space-y-2">
            {rows.map((item) => (
              <div key={item.id} className="border border-(--border-default)">
                <button
                  type="button"
                  onClick={() => { setExpandedId(expandedId === item.id ? null : item.id); setNote(""); }}
                  className="w-full flex items-center gap-3 px-4 py-3 text-left hover:bg-(--bg-elevated) transition-colors cursor-pointer"
                >
                  <span className={`shrink-0 border px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-widest ${item.status === "open" ? "text-white border-white" : "text-gray-500 border-gray-500"}`}>{item.status}</span>
                  <span className="text-[11px] font-bold uppercase tracking-widest text-(--text-secondary) shrink-0">{item.kind}</span>
                  <span className="text-[11px] font-mono text-(--text-muted) truncate flex-1" title={item.workflow_id}>{item.workflow_id}</span>
                  <span className="text-[10px] text-(--text-muted) shrink-0">{fmtTime(item.created_at)}</span>
                </button>

                {expandedId === item.id && (
                  <div className="px-4 pb-4 border-t border-(--border-divider) space-y-3">
                    {item.note && (
                      <pre className="text-[11px] leading-relaxed bg-black/40 border border-(--border-default) p-3 whitespace-pre-wrap break-all text-(--text-secondary)">{item.note}</pre>
                    )}

                    {item.status === "open" && (
                      <div className="flex flex-wrap items-center gap-2">
                        <button type="button" disabled={acting} onClick={() => openModal(item, "takeover")} className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-bold border border-white text-white hover:bg-white hover:text-black disabled:opacity-40 transition-colors cursor-pointer"><Hand className="h-3.5 w-3.5" /> 接管</button>
                        <button type="button" disabled={acting} onClick={() => openModal(item, "inject")} className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-bold border border-white text-white hover:bg-white hover:text-black disabled:opacity-40 transition-colors cursor-pointer"><Zap className="h-3.5 w-3.5" /> 注入</button>
                        <button type="button" disabled={acting} onClick={() => openModal(item, "retry")} className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-bold border border-white text-white hover:bg-white hover:text-black disabled:opacity-40 transition-colors cursor-pointer"><RotateCcw className="h-3.5 w-3.5" /> 重试</button>
                        <button type="button" disabled={acting} onClick={() => openModal(item, "rollback")} className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-bold border border-white text-white hover:bg-white hover:text-black disabled:opacity-40 transition-colors cursor-pointer"><Undo2 className="h-3.5 w-3.5" /> 回滚</button>
                      </div>
                    )}

                    <div className="flex items-end gap-2">
                      <div className="flex-1">
                        <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">resolve note</div>
                        <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="标记 resolved 的备注（可选）" className="w-full text-xs bg-black/40 border border-(--border-default) px-3 py-2 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white" />
                      </div>
                      <button type="button" disabled={acting} onClick={() => void onResolve(item)} className="flex items-center gap-1.5 px-4 py-2 text-xs font-bold bg-white text-black hover:opacity-80 disabled:opacity-40 transition-opacity cursor-pointer"><Check className="h-3.5 w-3.5" /> 解决</button>
                    </div>
                  </div>
                )}
              </div>
            ))}
          </div>
        )}
      </section>

      {/* 动作弹窗 */}
      {modal && (
        <div className="fixed inset-0 z-30 bg-black/70 flex items-center justify-center p-6" onClick={() => !acting && setModal(null)}>
          <div className="w-full max-w-lg bg-(--bg-sidebar) border border-(--border-default) p-5" onClick={(e) => e.stopPropagation()}>
            <div className="flex items-center justify-between mb-3">
              <h2 className="text-sm font-bold text-white">
                {modal.action === "takeover" ? "接管" : modal.action === "inject" ? "注入" : modal.action === "retry" ? "重试" : "回滚"}
                <span className="ml-2 text-[11px] font-mono text-(--text-muted)">#{modal.id}</span>
              </h2>
              <button type="button" disabled={acting} onClick={() => setModal(null)} className="text-(--text-muted) hover:text-white cursor-pointer">✕</button>
            </div>

            {modal.action === "rollback" ? (
              <div className="space-y-3">
                <div>
                  <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">skill_name</div>
                  <input value={rbSkill} onChange={(e) => setRbSkill(e.target.value)} placeholder="技能名" className="w-full text-xs bg-black/40 border border-(--border-default) px-3 py-2 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white" />
                </div>
                <div>
                  <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">rollback_to（目标版本）</div>
                  <input value={rbTo} onChange={(e) => setRbTo(e.target.value)} placeholder="如 v1.2.0" className="w-full text-xs bg-black/40 border border-(--border-default) px-3 py-2 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white" />
                </div>
                <p className="text-[11px] text-(--text-muted)">回滚走 S1 审批（生成 pending 审批单，非直接执行）。</p>
              </div>
            ) : (
              <div>
                <div className="flex items-center justify-between mb-1">
                  <span className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted)">
                    {modal.action === "inject" ? "参数 JSON：{ name, payload }" : "参数 JSON：{ workflow, input[, skill] }"}
                  </span>
                  {modal.action !== "inject" && (
                    <select value={JSON.parse(bodyText || "{}").workflow ?? ""} onChange={(e) => { const b = JSON.parse(bodyText || "{}"); b.workflow = e.target.value; setBodyText(JSON.stringify(b, null, 2)); }} className="text-[11px] bg-black/40 border border-(--border-default) px-2 py-1 text-white focus:outline-none focus:border-white">
                      {WORKFLOW_TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
                    </select>
                  )}
                </div>
                <textarea value={bodyText} onChange={(e) => setBodyText(e.target.value)} rows={8} className="w-full text-[11px] font-mono bg-black/40 border border-(--border-default) p-3 text-white focus:outline-none focus:border-white resize-y" />
              </div>
            )}

            {err && <div className="text-xs text-red-400 border border-red-400/40 p-2 mt-3">{err}</div>}

            <div className="flex justify-end gap-2 mt-4">
              <button type="button" disabled={acting} onClick={() => setModal(null)} className="px-4 py-2 text-xs font-bold border border-(--border-default) text-(--text-secondary) hover:text-white hover:border-white transition-colors cursor-pointer">取消</button>
              <button type="button" disabled={acting} onClick={() => void runAction()} className="flex items-center gap-1.5 px-4 py-2 text-xs font-bold bg-white text-black hover:opacity-80 disabled:opacity-40 transition-opacity cursor-pointer">
                {acting ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Check className="h-3.5 w-3.5" />} 执行
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
