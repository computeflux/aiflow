import { useState } from "react";
import { RefreshCw, Loader2, GitBranch } from "lucide-react";
import {
  listAgents,
  bindAgent,
  setAgentModel,
  listEvolveTasks,
  getEvolveTask,
  listModelProfiles,
  type AgentInfo,
  type AgentBindingInfo,
  type EvolveTaskSummary,
  type EvolveTaskDetail,
  type ModelProfile,
} from "../api";
import { useAsync } from "../hooks";

// Agent 管理（S1 进化闭环，docs/plan/S1-进化闭环.md §3.9）
// 黑白极简：已注册 Agent + 角色绑定（一键切换）+ 进化单列表。
// 可插拔：绑定切到占位 coder（echo 脚本）→ evolve-driver 零改码可跑通 → 切回 pi。

const ROLES = ["coder", "reviewer"] as const;

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

type ReviewCommentView = { file?: string; line?: number; level?: string; message?: string };

// parseReviewValue 解析 reviewer 原始输出（DB review 列可能是对象或 JSON 字符串）。
// structured=false 表示旧数据/非协议输出：保持原文展示。
function parseReviewValue(v: unknown): { ok?: boolean; comments: ReviewCommentView[]; structured: boolean } {
  let data: unknown = v;
  if (typeof v === "string") {
    const t = v.trim();
    if (!t || t === "null") return { comments: [], structured: false };
    try {
      data = JSON.parse(t);
    } catch {
      return { comments: [], structured: false };
    }
  }
  if (!data || typeof data !== "object" || Array.isArray(data)) return { comments: [], structured: false };
  const obj = data as Record<string, unknown>;
  return {
    ok: typeof obj.ok === "boolean" ? obj.ok : undefined,
    comments: Array.isArray(obj.comments) ? (obj.comments as ReviewCommentView[]) : [],
    structured: typeof obj.ok === "boolean" || Array.isArray(obj.comments),
  };
}

function reviewLevelClass(level?: string): string {
  switch ((level || "").toLowerCase()) {
    case "error":
      return "text-(--danger)";
    case "warning":
    case "warn":
      return "text-(--warning)";
    case "info":
      return "text-(--info)";
    default:
      return "text-(--text-secondary)";
  }
}

// ReviewBlock reviewer 审核结果：协议 JSON 渲染为结论 + 行级评论，不再直接糊原始 JSON。
function ReviewBlock({ value }: { value: unknown }) {
  if (value === null || value === undefined) return null;
  const rv = parseReviewValue(value);
  if (!rv.structured) {
    return (
      <div>
        <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">review（原始输出）</div>
        <pre className="text-[10px] bg-black/40 border border-(--border-default) p-2 overflow-auto max-h-40 whitespace-pre-wrap break-all text-(--text-secondary)">{fmtJSON(value)}</pre>
      </div>
    );
  }
  const errors = rv.comments.filter((c) => (c.level || "").toLowerCase() === "error").length;
  const warns = rv.comments.filter((c) => ["warning", "warn"].includes((c.level || "").toLowerCase())).length;
  return (
    <div>
      <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">review（行级评论）</div>
      <div className="flex items-center gap-3 flex-wrap text-[10px] mb-1">
        <span>
          结论：
          {rv.ok === true ? (
            <span className="text-(--ok) font-bold">通过</span>
          ) : rv.ok === false ? (
            <span className="text-(--danger) font-bold">不通过</span>
          ) : (
            <span>未知</span>
          )}
        </span>
        <span>评论 {rv.comments.length} 条</span>
        {errors > 0 && <span className="text-(--danger)">error {errors}</span>}
        {warns > 0 && <span className="text-(--warning)">warning {warns}</span>}
      </div>
      {rv.comments.length > 0 ? (
        <div className="space-y-1.5">
          {rv.comments.map((c, i) => (
            <div key={i} className="border-l-2 border-(--border-default) pl-2">
              <div className="flex items-baseline gap-2 flex-wrap text-[10px]">
                <span className={`font-bold ${reviewLevelClass(c.level)}`}>{(c.level || "info").toUpperCase()}</span>
                {c.file && (
                  <span className="font-mono text-(--text-muted)">
                    {c.file}
                    {c.line ? `:${c.line}` : ""}
                  </span>
                )}
              </div>
              <div className="text-[11px] text-(--text-secondary) whitespace-pre-wrap mt-0.5">{c.message || ""}</div>
            </div>
          ))}
        </div>
      ) : (
        <div className={`text-[11px] ${rv.ok === false ? "text-(--danger)" : "text-(--ok)"}`}>
          {rv.ok === false ? "未通过，但 reviewer 未给出逐条意见" : "审核通过，无逐条意见"}
        </div>
      )}
    </div>
  );
}

function statusClass(status: string): string {
  const map: Record<string, string> = {
    open: "text-white border-white",
    coding: "text-gray-300 border-gray-300",
    reviewing: "text-gray-300 border-gray-300",
    sandboxing: "text-gray-300 border-gray-300",
    testing: "text-gray-300 border-gray-300",
    approving: "text-gray-300 border-gray-300",
    released: "text-gray-300 border-gray-300 opacity-80",
    rolled_back: "text-gray-500 border-gray-500",
    expired: "text-gray-500 border-gray-500 opacity-70",
    cancelled: "text-gray-500 border-gray-500",
  };
  return `border px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-widest ${map[status] || "text-gray-500 border-gray-500"}`;
}

export function Agents() {

  const [bindDrafts, setBindDrafts] = useState<Record<string, string>>({});
  const [bindingRole, setBindingRole] = useState<string | null>(null);
  const [bindMsg, setBindMsg] = useState("");
  const [bindError, setBindError] = useState("");

  const [taskFilter, setTaskFilter] = useState("");
  const [expandedTask, setExpandedTask] = useState<number | null>(null);
  const [taskDetail, setTaskDetail] = useState<EvolveTaskDetail | null>(null);

  const { data: agentData, loading, error, reload: reloadAgents } = useAsync<{ agents: AgentInfo[]; bindings: Record<string, AgentBindingInfo> }>(
    async () => {
      const d = await listAgents();
      return { agents: d.agents, bindings: d.bindings };
    },
    [],
  );
  const agents = agentData?.agents ?? [];
  const bindings = agentData?.bindings ?? {};

  // S8.2 agent 运行时模型：候选 = 纳入 agent 目录的模型档案（sync_agents=1）。
  const { data: profileData } = useAsync<ModelProfile[]>(() => listModelProfiles(), []);
  const agentProfiles = (profileData ?? []).filter((p) => p.sync_agents !== 0);

  const [modelDrafts, setModelDrafts] = useState<Record<string, string>>({});
  const [settingModel, setSettingModel] = useState<string | null>(null);

  const onSetModel = async (agentName: string) => {
    const profile = modelDrafts[agentName] ?? "";
    setSettingModel(agentName);
    setBindMsg("");
    setBindError("");
    try {
      await setAgentModel(agentName, profile);
      setBindMsg(`${agentName} 模型 → ${profile || "（已清除，回退全局生效模型/env）"}（下轮进化执行生效）`);
      await reloadAgents();
    } catch (e) {
      setBindError(e instanceof Error ? e.message : String(e));
    } finally {
      setSettingModel(null);
    }
  };

  const { data: taskData, reload: reloadTasks } = useAsync<EvolveTaskSummary[]>(() => listEvolveTasks(taskFilter, 50), [taskFilter]);
  const tasks = taskData ?? [];

  const onBind = async (role: string) => {
    const name = bindDrafts[role];
    if (!name) return;
    setBindingRole(role);
    setBindMsg("");
    setBindError("");
    try {
      await bindAgent(role as "coder" | "reviewer", name, "web");
      setBindMsg(`${role} → ${name}（运行时立即生效，evolve-driver 下轮轮询按新绑定执行）`);
      await reloadAgents();
    } catch (e) {
      setBindError(e instanceof Error ? e.message : String(e));
    } finally {
      setBindingRole(null);
    }
  };

  const onExpandTask = async (t: EvolveTaskSummary) => {
    if (expandedTask === t.id) {
      setExpandedTask(null);
      setTaskDetail(null);
      return;
    }
    setExpandedTask(t.id);
    setTaskDetail(null);
    try {
      setTaskDetail(await getEvolveTask(t.id));
    } catch {
      // 忽略：展开区显示加载失败
    }
  };

  return (
    <div className="p-6 h-full w-full overflow-auto">
      <div className="flex items-center justify-between mb-4">
        <div>
          <h1 className="text-lg font-bold tracking-tight text-white">Agent 管理</h1>
          <p className="text-[11px] text-(--text-muted) mt-0.5">
            角色固定（coder / reviewer），实现可替换 · 运行时绑定存 agent_bindings 表 · 每 agent 可指定运行时模型（模型管理建档 + 勾选 agent 目录后在此选择）
          </p>
        </div>
        <button
          type="button"
          onClick={() => { void reloadAgents(); void reloadTasks(); }}
          className="flex items-center gap-2 px-3 py-1.5 text-xs text-(--text-secondary) border border-(--border-default) hover:text-white hover:border-white transition-colors cursor-pointer"
        >
          <RefreshCw className="h-3.5 w-3.5" /> 刷新
        </button>
      </div>

      {error && <div className="text-xs text-red-400 border border-red-400/40 p-3 mb-4">{error}</div>}
      {(bindMsg || bindError) && (
        <div className={`text-xs p-3 mb-4 border ${bindError ? "text-red-400 border-red-400/40" : "text-white border-white/40"}`}>
          {bindMsg || bindError}
        </div>
      )}

      {loading ? (
        <div className="flex items-center gap-2 text-xs text-(--text-muted) p-6">
          <Loader2 className="h-4 w-4 animate-spin" /> 加载中…
        </div>
      ) : (
        <>
          {/* 已注册 Agent */}
          <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">已注册 Agent</div>
          {agents.length === 0 ? (
            <div className="text-xs text-(--text-muted) p-4 border border-(--border-default) mb-6">
              无已注册 Agent（agentctl register agents/pi/agent.json 后刷新）
            </div>
          ) : (
            <table className="w-full text-xs mb-6">
              <thead>
                <tr className="text-[10px] uppercase tracking-widest text-(--text-muted) border-b border-(--border-default)">
                  <th className="text-left py-2 pr-3">name</th>
                  <th className="text-left py-2 pr-3">role</th>
                  <th className="text-left py-2 pr-3">version</th>
                  <th className="text-left py-2 pr-3">container</th>
                  <th className="text-left py-2 pr-3">entrypoint</th>
                  <th className="text-right py-2">运行时模型</th>
                </tr>
              </thead>
              <tbody>
                {agents.map((a) => (
                  <tr key={a.name} className="border-b border-(--border-divider) text-(--text-secondary)">
                    <td className="py-2 pr-3 font-mono text-white">{a.name}</td>
                    <td className="py-2 pr-3 font-bold uppercase tracking-widest text-[10px]">{a.role}</td>
                    <td className="py-2 pr-3 font-mono">{a.version}</td>
                    <td className="py-2 pr-3 font-mono">{a.container}</td>
                    <td className="py-2 pr-3 font-mono text-(--text-muted)">{a.entrypoint}</td>
                    <td className="py-2 text-right whitespace-nowrap">
                      <select
                        value={modelDrafts[a.name] ?? a.model_profile ?? ""}
                        onChange={(e) => setModelDrafts((m) => ({ ...m, [a.name]: e.target.value }))}
                        title="该 agent 的运行时模型档案（需先在「模型管理」建档并勾选 agent 目录）"
                        className="text-xs bg-black/40 border border-(--border-default) px-2 py-1 text-white focus:outline-none focus:border-white"
                      >
                        <option value="">全局/env</option>
                        {agentProfiles.map((p) => (
                          <option key={p.id} value={p.name}>{p.name}（{p.model}）</option>
                        ))}
                      </select>
                      <button
                        type="button"
                        disabled={settingModel !== null || (modelDrafts[a.name] ?? a.model_profile ?? "") === (a.model_profile ?? "")}
                        onClick={() => void onSetModel(a.name)}
                        className="ml-2 px-2 py-1 text-[10px] font-bold uppercase tracking-widest border border-white text-white hover:bg-white hover:text-black disabled:opacity-40 transition-colors cursor-pointer"
                      >
                        {settingModel === a.name ? <Loader2 className="h-3 w-3 animate-spin" /> : "设置"}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          {/* 角色绑定 */}
          <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">角色绑定（一键切换）</div>
          <div className="space-y-2 mb-6">
            {ROLES.map((role) => {
              const b = bindings[role];
              const candidates = agents.filter((a) => a.role === role);
              return (
                <div key={role} className="flex items-center gap-3 border border-(--border-default) px-4 py-3">
                  <span className="w-20 text-[11px] font-bold uppercase tracking-widest text-white shrink-0">{role}</span>
                  <span className="text-xs text-(--text-secondary) shrink-0">
                    当前：
                    <span className="font-mono text-white">{b ? b.agent_name : "（未绑定，回退 env）"}</span>
                    {b && b.updated_by ? <span className="text-(--text-muted) ml-2 text-[10px]">by {b.updated_by} · {fmtTime(b.updated_at)}</span> : null}
                  </span>
                  <div className="flex-1" />
                  <select
                    value={bindDrafts[role] ?? (b?.agent_name ?? "")}
                    onChange={(e) => setBindDrafts((m) => ({ ...m, [role]: e.target.value }))}
                    className="text-xs bg-black/40 border border-(--border-default) px-2 py-1.5 text-white focus:outline-none focus:border-white"
                  >
                    <option value="">—</option>
                    {candidates.map((a) => (
                      <option key={a.name} value={a.name}>{a.name} (v{a.version})</option>
                    ))}
                  </select>
                  <button
                    type="button"
                    disabled={bindingRole !== null || !bindDrafts[role]}
                    onClick={() => void onBind(role)}
                    className="px-3 py-1.5 text-[11px] font-bold bg-white text-black hover:opacity-80 disabled:opacity-40 transition-opacity cursor-pointer"
                  >
                    {bindingRole === role ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : "切换绑定"}
                  </button>
                </div>
              );
            })}
          </div>

          {/* 进化单 */}
          <div className="flex items-center gap-3 mb-2">
            <span className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted)">进化单（evolve_tasks）</span>
            <GitBranch className="h-3 w-3 text-(--text-muted)" />
            <div className="flex gap-1">
              {["", "open", "approving", "released", "rolled_back", "cancelled"].map((s) => (
                <button
                  key={s || "all"}
                  type="button"
                  onClick={() => setTaskFilter(s)}
                  className={`px-2 py-0.5 text-[10px] font-bold uppercase tracking-widest border transition-colors cursor-pointer ${
                    taskFilter === s
                      ? "bg-white text-black border-white"
                      : "text-(--text-muted) border-(--border-default) hover:text-white"
                  }`}
                >
                  {s === "" ? "全部" : s}
                </button>
              ))}
            </div>
          </div>
          {tasks.length === 0 ? (
            <div className="text-xs text-(--text-muted) p-4 border border-(--border-default)">
              无进化单（巡检严重问题自动生成 source=patrol 单；也可手工 INSERT source=manual）
            </div>
          ) : (
            <div className="space-y-1.5">
              {tasks.map((t) => (
                <div key={t.id} className="border border-(--border-default)">
                  <button
                    type="button"
                    onClick={() => void onExpandTask(t)}
                    className="w-full flex items-center gap-3 px-3 py-2.5 text-left hover:bg-(--bg-elevated) transition-colors cursor-pointer"
                  >
                    <span className={`shrink-0 ${statusClass(t.status)}`}>{t.status}</span>
                    <span className="text-[11px] font-mono text-white shrink-0">{t.task_no}</span>
                    <span className="text-[10px] font-bold uppercase tracking-widest text-(--text-secondary) shrink-0">{t.source}</span>
                    <span className="text-[11px] text-(--text-secondary) shrink-0">{t.target_name}</span>
                    <div className="flex-1" />
                    <span className="text-[10px] text-(--text-muted) shrink-0">{fmtTime(t.created_at)}</span>
                  </button>
                  {expandedTask === t.id && (
                    <div className="px-3 pb-3 border-t border-(--border-divider) text-[11px] space-y-2">
                      {taskDetail ? (
                        <>
                          {taskDetail.evidence && (
                            <div>
                              <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">evidence</div>
                              <pre className="text-[10px] bg-black/40 border border-(--border-default) p-2 overflow-auto max-h-40 whitespace-pre-wrap break-all text-(--text-secondary)">{fmtJSON(taskDetail.evidence)}</pre>
                            </div>
                          )}
                          {taskDetail.diff && (
                            <div>
                              <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">diff（{taskDetail.diff.length} bytes）</div>
                              <pre className="text-[10px] bg-black/40 border border-(--border-default) p-2 overflow-auto max-h-56 whitespace-pre-wrap break-all text-(--text-secondary)">{taskDetail.diff}</pre>
                            </div>
                          )}
                          {taskDetail.review !== null && taskDetail.review !== undefined && (
                            <ReviewBlock value={taskDetail.review} />
                          )}
                          {taskDetail.test_report && (
                            <div>
                              <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">test_report</div>
                              <pre className="text-[10px] bg-black/40 border border-(--border-default) p-2 overflow-auto max-h-40 whitespace-pre-wrap break-all text-(--text-secondary)">{fmtJSON(taskDetail.test_report)}</pre>
                            </div>
                          )}
                        </>
                      ) : (
                        <div className="text-(--text-muted) p-2">加载详情中…</div>
                      )}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}
