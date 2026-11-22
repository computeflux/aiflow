import { useCallback, useEffect, useState } from "react";
import { Loader2, Play, RefreshCw, Plus, X, Boxes } from "lucide-react";
import {
  fetchSkills,
  invokeSkill,
  registerSkill,
  setSkillTraffic,
  type SkillInfo,
} from "../api";

// 技能中心（P1 Skill 框架，docs/plan/transform-plan.md §6.9）
// 黑白极简：左侧 skill 列表，右侧详情（schema / 版本 / 执行记录 / 手动调用）

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
    ok: "text-white border-white",
    fail: "text-red-400 border-red-400",
    timeout: "text-yellow-400 border-yellow-400",
    rejected: "text-gray-500 border-gray-500",
    live: "text-white border-white",
    draft: "text-gray-500 border-gray-500",
    retired: "text-gray-500 border-gray-500",
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

// 根据 input_schema 生成一份带示例值的输入 JSON
function defaultInput(schema: Record<string, any> | null): string {
  const props: Record<string, any> = schema?.properties ?? {};
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(props)) {
    const t = v?.type;
    if (t === "integer" || t === "number") out[k] = v?.minimum ?? v?.default ?? 1;
    else if (t === "boolean") out[k] = true;
    else if (t === "array") out[k] = [];
    else if (t === "object") out[k] = {};
    else out[k] = "";
  }
  return JSON.stringify(out, null, 2);
}

export function Skills() {
  const [skills, setSkills] = useState<SkillInfo[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const [invokeText, setInvokeText] = useState("{}");
  const [invoking, setInvoking] = useState(false);
  const [invokeResult, setInvokeResult] = useState<string | null>(null);
  const [invokeError, setInvokeError] = useState("");

  const [registerOpen, setRegisterOpen] = useState(false);
  const [registerText, setRegisterText] = useState("");
  const [registerMsg, setRegisterMsg] = useState("");
  const [registering, setRegistering] = useState(false);

  const [trafficDrafts, setTrafficDrafts] = useState<Record<string, string>>({});

  const reload = useCallback(async (name?: string) => {
    setLoading(true);
    setError("");
    try {
      const list = await fetchSkills();
      setSkills(list);
      if (name) {
        setSelected(name);
      } else if (list.length > 0 && !list.some((s) => s.name === selected)) {
        setSelected(list[0].name);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected]);

  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const current = skills.find((s) => s.name === selected) ?? null;

  // 切换选中时重置调用面板
  useEffect(() => {
    if (!current) return;
    setInvokeText(defaultInput(current.input_schema));
    setInvokeResult(null);
    setInvokeError("");
  }, [selected]); // eslint-disable-line react-hooks/exhaustive-deps

  const onInvoke = async () => {
    if (!current) return;
    let input: Record<string, unknown>;
    try {
      input = JSON.parse(invokeText || "{}");
    } catch {
      setInvokeError("输入不是合法 JSON");
      return;
    }
    setInvoking(true);
    setInvokeResult(null);
    setInvokeError("");
    try {
      const out = await invokeSkill(current.name, input);
      setInvokeResult(fmtJSON(out));
    } catch (e) {
      const err = e as { message?: string; output?: unknown };
      let msg = err.message || String(e);
      if (err.output !== undefined) msg += "\n\nskill 输出:\n" + fmtJSON(err.output);
      setInvokeError(msg);
    } finally {
      setInvoking(false);
      reload(current.name);
    }
  };

  const onRegister = async () => {
    setRegistering(true);
    setRegisterMsg("");
    try {
      await registerSkill(registerText || "{}");
      setRegisterMsg("注册成功");
      setRegisterOpen(false);
      reload();
    } catch (e) {
      setRegisterMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setRegistering(false);
    }
  };

  const onTraffic = async (version: string, traffic: number) => {
    if (!current) return;
    try {
      await setSkillTraffic(current.name, version, traffic);
      reload(current.name);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="flex-1 flex flex-col min-w-0">
      <header className="flex items-center justify-between px-4 py-5 border-l border-(--border-subtle)">
        <div>
          <h1 className="text-xl font-bold tracking-tighter uppercase text-white">技能中心</h1>
          <p className="mt-1 text-sm text-(--text-muted)">
            Skill 注册表 · 版本路由 · 限流 · 调用记录（P1 框架 /api/skills）
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setRegisterOpen(true)}
            className="inline-flex items-center gap-2 border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white transition"
          >
            <Plus className="h-4 w-4" /> 注册 Skill
          </button>
          <button
            onClick={() => reload(selected ?? undefined)}
            className="inline-flex items-center gap-2 border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white transition"
          >
            <RefreshCw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} /> 刷新
          </button>
        </div>
      </header>

      {error && (
        <div className="mx-4 mt-3 border border-red-400/60 bg-black px-3 py-2 text-[11px] text-red-400">{error}</div>
      )}

      <div className="flex-1 flex min-h-0">
        {/* 左侧：skill 列表 */}
        <aside className="w-72 p-2 m-4 shrink-0 overflow-y-auto border border-(--border-subtle) shadow-md">
          {loading && skills.length === 0 ? (
            <div className="p-4 text-xs text-(--text-muted) inline-flex items-center gap-2">
              <Loader2 className="h-3.5 w-3.5 animate-spin" /> 加载中
            </div>
          ) : skills.length === 0 ? (
            <div className="p-4 text-xs text-(--text-muted)">
              暂无已注册 skill。<br />
              使用 skillctl register 或右上角「注册 Skill」。
            </div>
          ) : (
            skills.map((s) => (
              <button
                key={s.name}
                onClick={() => setSelected(s.name)}
                className={`flex w-full text-left px-4 py-3 border-b border-(--border-divider) border-l-2 border-l-transparent transition-colors ${
                  s.name === selected
                    ? "bg-(--bg-elevated) text-white border-l-white"
                    : "text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white"
                }`}
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="font-medium text-sm truncate flex items-center gap-2">
                    <Boxes className="h-3.5 w-3.5 shrink-0 opacity-60" /> {s.name}
                  </span>
                  <span className={`shrink-0 border px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-widest ${
                    s.live_version ? "text-white border-white/60" : "text-(--text-muted) border-(--text-muted)"
                  }`}>
                    {s.live_version ? "live" : "draft"}
                  </span>
                </div>
                <div className="mt-1 flex items-center gap-2 text-[10px] text-(--text-muted)">
                  <span className="px-1.5 py-0.5 border border-(--border-divider) rounded">{s.runtime}</span>
                  <span>v{s.live_version || "—"}</span>
                  <span className="ml-auto">{s.run_count} 次</span>
                </div>
              </button>
            ))
          )}
        </aside>

        {/* 右侧：详情 */}
        <main className="flex-1 overflow-y-auto p-4 space-y-4 min-w-0">
          {!current ? (
            <div className="text-xs text-(--text-muted)">选择一个 skill 查看详情</div>
          ) : (
            <>
              {/* 基本信息 */}
              <section className="grid grid-cols-2 md:grid-cols-4 gap-3">
                {[
                  ["Owner", current.owner],
                  ["Runtime", current.runtime],
                  ["Type", current.type],
                  ["Live Version", current.live_version || "—"],
                  ["Entrypoint", current.entrypoint || "—"],
                  ["Addr", current.addr || "—"],
                  ["Container", current.container || "—"],
                  ["Limits", `timeout ${current.limits?.timeout_sec ?? 60}s · rate ${current.limits?.rate_per_min ?? 0}/min · concurrency ${current.limits?.max_concurrency ?? 0}`],
                ].map(([k, v]) => (
                  <div key={k} className="border border-(--border-subtle) bg-(--bg-surface) px-3 py-2.5">
                    <div className="text-[9px] font-bold uppercase tracking-widest text-(--text-muted)">{k}</div>
                    <div className="mt-0.5 text-xs text-white truncate" title={String(v)}>{String(v)}</div>
                  </div>
                ))}
              </section>

              {/* 版本 */}
              <section>
                <h2 className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">版本</h2>
                <div className="border border-(--border-default) divide-y divide-(--border-divider)">
                  {current.versions.length === 0 && (
                    <div className="px-3 py-2 text-[11px] text-(--text-muted)">无版本记录</div>
                  )}
                  {current.versions.map((v) => {
                    const draft = trafficDrafts[`${v.version}`] ?? String(v.traffic_split);
                    return (
                      <div key={v.id} className="flex items-center gap-3 px-3 py-2">
                        <span className="text-xs font-bold text-white">v{v.version}</span>
                        <span className={statusClass(v.status)}>{v.status}</span>
                        <span className="text-[10px] text-(--text-muted) truncate flex-1">{v.image}</span>
                        <input
                          value={draft}
                          onChange={(e) => setTrafficDrafts((d) => ({ ...d, [v.version]: e.target.value }))}
                          className="w-16 border border-(--border-default) bg-black px-2 py-1 text-[11px] text-white"
                          type="number"
                          min={0}
                          max={100}
                        />
                        <button
                          onClick={() => onTraffic(v.version, Number(draft))}
                          className="border border-(--border-default) bg-(--bg-elevated) px-2 py-1 text-[10px] font-bold text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white transition"
                        >
                          应用
                        </button>
                      </div>
                    );
                  })}
                </div>
              </section>

              {/* Schema */}
              <section className="grid grid-cols-1 lg:grid-cols-2 gap-3">
                <Pre label="Input Schema" value={current.input_schema} />
                <Pre label="Output Schema" value={current.output_schema} />
              </section>

              {/* 手动调用 */}
              <section>
                <h2 className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">手动调用</h2>
                <textarea
                  value={invokeText}
                  onChange={(e) => setInvokeText(e.target.value)}
                  rows={6}
                  spellCheck={false}
                  className="w-full bg-black/40 border border-(--border-default) p-3 text-[11px] font-mono text-(--text-secondary) focus:outline-none focus:border-white/40"
                />
                <button
                  onClick={onInvoke}
                  disabled={invoking}
                  className="mt-2 inline-flex items-center gap-2 border border-white bg-white px-4 py-2 text-xs font-bold text-black hover:bg-(--text-secondary) hover:text-black transition disabled:opacity-50"
                >
                  {invoking ? <Loader2 className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}
                  {invoking ? "执行中（最长 5 分钟）…" : `调用 ${current.name}`}
                </button>
                {invokeError && (
                  <pre className="mt-2 border border-red-400/60 bg-black p-3 text-[11px] text-red-400 whitespace-pre-wrap break-all max-h-64 overflow-auto">{invokeError}</pre>
                )}
                {invokeResult && (
                  <div className="mt-2">
                    <Pre label="执行结果" value={JSON.parse(invokeResult)} />
                  </div>
                )}
              </section>

              {/* 最近执行 */}
              <section>
                <h2 className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">
                  最近执行（共 {current.run_count} 次）
                </h2>
                <div className="border border-(--border-default) overflow-x-auto">
                  <table className="w-full text-xs">
                    <thead>
                      <tr className="text-left text-[9px] uppercase tracking-widest text-(--text-muted) border-b border-(--border-default)">
                        <th className="px-3 py-2">状态</th>
                        <th className="px-3 py-2">版本</th>
                        <th className="px-3 py-2">耗时</th>
                        <th className="px-3 py-2">Trace ID</th>
                        <th className="px-3 py-2">时间</th>
                        <th className="px-3 py-2">错误</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(current.recent_runs ?? []).length === 0 && (
                        <tr>
                          <td colSpan={6} className="px-3 py-2 text-(--text-muted)">暂无执行记录</td>
                        </tr>
                      )}
                      {(current.recent_runs ?? []).map((r) => (
                        <tr key={r.id} className="border-b border-(--border-divider)">
                          <td className="px-3 py-1.5"><span className={statusClass(r.status)}>{r.status}</span></td>
                          <td className="px-3 py-1.5 text-white">v{r.version}</td>
                          <td className="px-3 py-1.5 text-(--text-secondary)">{r.latency_ms != null ? `${r.latency_ms}ms` : "—"}</td>
                          <td className="px-3 py-1.5 text-(--text-muted) font-mono truncate max-w-40" title={r.trace_id}>{r.trace_id}</td>
                          <td className="px-3 py-1.5 text-(--text-muted)">{r.created_at.replace("T", " ").slice(0, 19)}</td>
                          <td className="px-3 py-1.5 text-red-400 truncate max-w-56" title={r.error}>{r.error || ""}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </section>
            </>
          )}
        </main>
      </div>

      {/* 注册弹层 */}
      {registerOpen && (
        <div className="fixed inset-0 z-30 flex items-center justify-center bg-black/70">
          <div className="w-[560px] max-w-[90vw] border border-(--border-subtle) bg-(--bg-surface) shadow-2xl">
            <div className="flex items-center justify-between px-4 py-3 border-b border-(--border-default)">
              <h2 className="text-xs font-bold uppercase tracking-widest text-white">注册 Skill</h2>
              <button onClick={() => setRegisterOpen(false)} className="text-(--text-muted) hover:text-white transition">
                <X className="h-4 w-4" />
              </button>
            </div>
            <div className="p-4 space-y-3">
              <textarea
                value={registerText}
                onChange={(e) => setRegisterText(e.target.value)}
                rows={18}
                spellCheck={false}
                placeholder='粘贴 skill.json 内容（name/version/owner/runtime/type/schema/limits …）'
                className="w-full bg-black/40 border border-(--border-default) p-3 text-[11px] font-mono text-(--text-secondary) focus:outline-none focus:border-white/40"
              />
              {registerMsg && (
                <div className={`text-[11px] ${registerMsg === "注册成功" ? "text-white" : "text-red-400"}`}>{registerMsg}</div>
              )}
              <div className="flex justify-end">
                <button
                  onClick={onRegister}
                  disabled={registering}
                  className="inline-flex items-center gap-2 border border-white bg-white px-4 py-2 text-xs font-bold text-black hover:bg-(--text-secondary) transition disabled:opacity-50"
                >
                  {registering ? <Loader2 className="h-4 w-4 animate-spin" /> : <Plus className="h-4 w-4" />}
                  提交注册
                </button>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
