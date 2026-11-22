import { useState, useEffect, useRef } from "react";
import { Loader2, Sparkles, ExternalLink, Copy, Check, Clock, History, Plus, Trash2, Brain, ChevronDown, ChevronUp } from "lucide-react";
import { generateSocialContent, apiGet, request, getApiBase } from "../api";
import { fmtTime } from "../utils";
import { useAsync } from "../hooks";

const SOURCES = [
  { v: "x", l: "X (Twitter)" },
  { v: "reddit", l: "Reddit" },
  { v: "github", l: "GitHub" },
];

const SCHEDULE_TIMES: string[] = [];
for (let h = 0; h < 24; h++) {
  SCHEDULE_TIMES.push(`${String(h).padStart(2,"0")}:00`, `${String(h).padStart(2,"0")}:30`);
}

interface Schedule {
  id: number; hours: number; count: number; sources: string;
  schedule: string; enabled: boolean; last_run_at?: string;
}

interface HistoryItem {
  id: number; schedule_id: number; content: string;
  source_url: string; keyword: string; score: number;
  reasoning: string; created_at: string;
}

type Tab = "history" | "schedules";

const PRESET_PICKER = ({ hours, setHours, count, setCount, sources, setSources, onGenerate, loading }: any) => (
  <div className="flex flex-wrap items-end gap-4">
    <div>
      <label className="block text-[10px] uppercase text-(--text-muted) mb-1">时间范围</label>
      <div className="flex gap-1">
        {[6,12,24,48].map(h => (<button key={h} onClick={() => setHours(h)} className={`px-3 py-1.5 text-xs font-bold border transition ${hours===h?"border-white bg-(--accent) text-(--accent-foreground)":"border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>{h}h</button>))}
      </div>
    </div>
    <div>
      <label className="block text-[10px] uppercase text-(--text-muted) mb-1">数量</label>
      <div className="flex gap-1">
        {[3,5,10].map(n => (<button key={n} onClick={() => setCount(n)} className={`px-3 py-1.5 text-xs font-bold border transition ${count===n?"border-white bg-(--accent) text-(--accent-foreground)":"border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>{n}</button>))}
      </div>
    </div>
    <div>
      <label className="block text-[10px] uppercase text-(--text-muted) mb-1">来源</label>
      <div className="flex gap-1">
        {SOURCES.map(s => (<button key={s.v} onClick={() => setSources((prev: string[]) => prev.includes(s.v) ? prev.filter(x=>x!==s.v) : [...prev,s.v])} className={`px-2 py-1.5 text-xs font-bold border transition ${sources.includes(s.v)?"border-white bg-(--accent) text-(--accent-foreground)":"border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}>{s.l}</button>))}
      </div>
    </div>
    <button onClick={onGenerate} disabled={loading || sources.length===0} className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm hover:bg-(--text-primary) disabled:opacity-30 flex items-center gap-2">
      {loading ? <><Loader2 className="h-4 w-4 animate-spin" /> 分析中...</> : <><Sparkles className="h-4 w-4" /> 立即生成</>}
    </button>
  </div>
);

export function SocialPage() {
  const [tab, setTab] = useState<Tab>("history");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [hours, setHours] = useState(24);
  const [count, setCount] = useState(5);
  const [sources, setSources] = useState<string[]>(["x"]);
  const [copied, setCopied] = useState<number | null>(null);
  const [expandedId, setExpandedId] = useState<number | null>(null);

  const [showSchedForm, setShowSchedForm] = useState(false);
  const [schedHours, setSchedHours] = useState(24);
  const [schedCount, setSchedCount] = useState(5);
  const [schedSources, setSchedSources] = useState<string[]>(["x"]);
  const [schedTime, setSchedTime] = useState("09:00");

  const { data: schedData, reload: loadSchedules } = useAsync<Schedule[]>(
    async () => (await apiGet("/social-schedules") as Schedule[] | null) ?? [],
    [],
    { immediate: false },
  );
  const { data: histData, reload: loadHistory } = useAsync<HistoryItem[]>(
    async () => (await apiGet("/social-tweets") as HistoryItem[] | null) ?? [],
    [],
  );
  const schedules = schedData ?? [];
  const history = histData ?? [];

  // 切到 schedules 页签时拉取排程列表（首帧由 useAsync immediate 负责 history）
  const prevTabRef = useRef<Tab | null>(null);
  useEffect(() => {
    const first = prevTabRef.current === null;
    const was = prevTabRef.current;
    prevTabRef.current = tab;
    if (tab === "schedules" && (!first || was !== "schedules")) void loadSchedules();
  }, [tab, loadSchedules]);

  const handleGenerate = async () => {
    setLoading(true); setError(null);
    try {
      await generateSocialContent(hours, count, sources.join(","));
      await loadHistory();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally { setLoading(false); }
  };

  const copyTweet = async (text: string, index: number) => {
    await navigator.clipboard.writeText(text);
    setCopied(index);
    setTimeout(() => setCopied(null), 2000);
  };

  const createSchedule = async () => {
    // Build cron from time: "09:30" → "30 9 * * *"
    const [hh, mm] = schedTime.split(":");
    const cron = `${mm} ${parseInt(hh)} * * *`;
    const r = await request(`${getApiBase()}/social-schedules`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ hours: schedHours, count: schedCount, sources: schedSources.join(","), schedule: cron }),
    });
    if (!r.ok) { setError("创建失败"); return; }
    setShowSchedForm(false);
    loadSchedules();
  };

  const deleteSchedule = async (id: number) => {
    try {
      const r = await request(`${getApiBase()}/social-schedules/${id}`, { method: "DELETE" });
      if (!r.ok) throw new Error(`删除调度失败: ${r.status}`);
      loadSchedules();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-6">
        <h1 className="text-2xl font-bold tracking-tighter uppercase flex items-center gap-2">
          <Sparkles className="h-6 w-6" /> 社媒内容生成
        </h1>
      </header>

      <div className="mb-6 flex items-center gap-2 px-2 border border-(--border-subtle)">
        {[
          { k: "history" as Tab, l: "生成历史", i: History },
          { k: "schedules" as Tab, l: "定时调度", i: Clock },
        ].map(t => (
          <button key={t.k} onClick={() => setTab(t.k)}
            className={`px-4 py-2 text-xs font-bold uppercase transition ${tab === t.k ? "border-b-2 border-white text-white" : "text-(--text-muted) hover:text-(--text-secondary)"}`}>
            <t.i className="h-3.5 w-3.5 inline mr-1" />{t.l}
          </button>
        ))}
      </div>

      {error && <div className="mb-4 border border-(--danger)/30 bg-(--danger)/5 p-3 text-sm text-(--danger)">{error}</div>}

      {/* ===== HISTORY TAB ===== */}
      {tab === "history" && (
        <div>
          <div className="mb-6 p-4 border border-(--border-subtle)">
            <PRESET_PICKER hours={hours} setHours={setHours} count={count} setCount={setCount} sources={sources} setSources={setSources} onGenerate={handleGenerate} loading={loading} />
          </div>

          {history.length === 0 ? (
            <div className="text-center py-16 border border-(--border-subtle) ">
              <History className="h-12 w-12 text-(--border-divider) mx-auto mb-4" />
              <p className="text-sm text-(--text-muted)">暂无记录。设置参数后点击「立即生成」</p>
            </div>
          ) : (
            <div className="grid gap-6">
              {(() => {
                // Group tweets by batch (same second)
                const batches: Map<string, HistoryItem[]> = new Map()
                for (const h of history) {
                  const ts = (h.created_at || "").slice(0, 19)  // truncate to seconds
                  const items = batches.get(ts) || []
                  items.push(h)
                  batches.set(ts, items)
                }
                return Array.from(batches.entries()).map(([ts, tweets]) => (
                  <div key={ts} className="border-3 border-(--border-subtle)">
                    <div className="p-3 border-b border-(--border-subtle) flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <span className="text-[10px] text-(--text-muted)">{fmtTime(tweets[0].created_at)}</span>
                        <span className="text-[10px] text-(--text-secondary) font-bold">{tweets.length} 条推文</span>
                      </div>
                    </div>
                    {tweets.map((h, i) => (
                      <div key={h.id} className="p-4 border-b border-(--border-subtle) last:border-b-0 group">
                        <div className="flex items-center justify-between mb-2">
                          <span className="text-[10px] text-(--text-muted)">#{i + 1}</span>
                          <div className="flex items-center gap-2 opacity-0 group-hover:opacity-100 transition">
                            <button onClick={() => copyTweet(h.content, h.id)} className="text-[10px] text-(--text-muted) hover:text-white flex items-center gap-1">
                              {copied===h.id ? <Check className="h-3 w-3 text-(--success)" /> : <Copy className="h-3 w-3" />}
                            </button>
                          </div>
                        </div>
                        <p className="text-sm text-(--text-primary) whitespace-pre-wrap leading-relaxed">{h.content}</p>
                        {h.source_url && (
                          <div className="mt-2">
                            <a href={h.source_url} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 text-[11px] text-(--info) hover:underline">
                              <ExternalLink className="h-3 w-3" />
                              转发地址: {h.source_url}
                            </a>
                          </div>
                        )}
                        {h.reasoning && (
                          <div className="mt-2">
                            <button
                              onClick={() => setExpandedId(expandedId === h.id ? null : h.id)}
                              className="flex items-center gap-1 text-[10px] text-(--purple) hover:text-white transition">
                              <Brain className="h-3 w-3" />
                              思考过程
                              {expandedId === h.id ? <ChevronUp className="h-3 w-3" /> : <ChevronDown className="h-3 w-3" />}
                            </button>
                            {expandedId === h.id && (
                              <pre className="mt-2 p-3 bg-(--bg-elevated) border border-(--border-subtle) text-[11px] text-(--text-secondary) whitespace-pre-wrap leading-relaxed max-h-64 overflow-y-auto">{h.reasoning}</pre>
                            )}
                          </div>
                        )}
                      </div>
                    ))}
                  </div>
                ))
              })()}
            </div>
          )}
        </div>
      )}

      {/* ===== SCHEDULES TAB ===== */}
      {tab === "schedules" && (
        <div>
          <div className="flex items-center justify-between mb-4">
            <p className="text-xs text-(--text-muted)">定时自动生成推文，结果自动保存到生成历史</p>
            <button onClick={() => setShowSchedForm(!showSchedForm)} className="border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) flex items-center gap-1">
              <Plus className="h-3 w-3" />{showSchedForm ? "取消" : "新建调度"}
            </button>
          </div>

          {showSchedForm && (
            <div className="mb-6 p-4 border border-(--border-subtle)">
              <div className="flex flex-wrap gap-4 items-end">
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">执行时间</label>
                  <select value={schedTime} onChange={e => setSchedTime(e.target.value)} className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm">
                    {SCHEDULE_TIMES.map(t => (<option key={t} value={t}>{t}</option>))}
                  </select>
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">数据范围</label>
                  <select value={schedHours} onChange={e => setSchedHours(Number(e.target.value))} className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1 text-sm">
                    {[12,24,48].map(h => (<option key={h} value={h}>最近 {h}h</option>))}
                  </select>
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">数量</label>
                  <select value={schedCount} onChange={e => setSchedCount(Number(e.target.value))} className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1 text-sm">
                    {[3,5,10].map(n => (<option key={n} value={n}>{n} 条</option>))}
                  </select>
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">来源</label>
                  <div className="flex gap-1">
                    {SOURCES.map(s => (<button key={s.v} onClick={() => setSchedSources(prev => prev.includes(s.v) ? prev.filter(x=>x!==s.v) : [...prev,s.v])} className={`px-2 py-1.5 text-xs font-bold border transition ${schedSources.includes(s.v)?"border-white bg-(--accent) text-(--accent-foreground)":"border-(--border-subtle) text-(--text-secondary)"}`}>{s.l}</button>))}
                  </div>
                </div>
                <button onClick={createSchedule} className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm">创建</button>
              </div>
            </div>
          )}

          <div className="border border-(--border-subtle) ">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                <tr><th className="px-4 py-2 font-bold uppercase text-xs">执行时间</th><th className="px-4 py-2 font-bold uppercase text-xs">数据范围</th><th className="px-4 py-2 font-bold uppercase text-xs">数量</th><th className="px-4 py-2 font-bold uppercase text-xs">来源</th><th className="px-4 py-2 text-right font-bold uppercase text-xs">操作</th></tr>
              </thead>
              <tbody className="divide-y divide-(--border-divider)">
                {schedules.length === 0 ? (
                  <tr><td colSpan={5} className="px-4 py-8 text-center text-(--text-muted) text-xs">暂无定时调度</td></tr>
                ) : (
                  schedules.map(s => {
                    const cronParts = (s.schedule || "").split(" ");
                    const cronLabel = cronParts.length >= 2 ? `每天 ${cronParts[1].padStart(2,"0")}:${cronParts[0].padStart(2,"0")}` : s.schedule;
                    return (
                      <tr key={s.id} className="hover:bg-(--bg-elevated)">
                        <td className="px-4 py-2 text-xs">{cronLabel}</td>
                        <td className="px-4 py-2 text-xs">{s.hours}h</td>
                        <td className="px-4 py-2 text-xs">{s.count}</td>
                        <td className="px-4 py-2 text-xs text-(--text-secondary)">{s.sources}</td>
                        <td className="px-4 py-2 text-right">
                          <button onClick={() => deleteSchedule(s.id)} className="p-1 text-(--text-muted) hover:text-red-500"><Trash2 className="h-3 w-3" /></button>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
