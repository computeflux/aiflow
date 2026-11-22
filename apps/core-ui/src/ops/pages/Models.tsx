import { useState } from "react";
import { RefreshCw, Loader2, Trash2, Check, Pencil, Plus } from "lucide-react";
import {
  listModelProfiles,
  createModelProfile,
  updateModelProfile,
  deleteModelProfile,
  applyModelProfile,
  syncAIFlowModels,
  type ModelProfile,
  type ModelProfileBody,
} from "../api";
import { useAsync } from "../hooks";

// 模型管理（S8）：模型档案（model_profiles 表）维护 + 应用（active）。
// 应用后的档案是全局生效模型：
//   - core（Go 核心 LLM：api/audit/ops-report…）：分钟级 ticker 拉取热切；
//   - aiflow（ai-flow 网关）：应用时推 model_list + /reload，全部 agent 立即切换；
//   - coder / reviewer（进化链 evolve-driver）：下次进化执行生效。
// S8.1：sync_agents（默认开）的档案在应用时同步进 agent 框架的模型目录：
// ai-flow model_list、pi-web ~/.pi/agent/models.json、ocr ~/.opencodereview/config.json
//（evolve-driver 另有 5 分钟兜底 ticker）。
// consumers 留空 = 应用到全部消费面。api_key 保存后不回显（编辑留空 = 不改）。

const CONSUMER_OPTIONS = ["core", "aiflow", "coder", "reviewer"] as const;

interface Draft {
  id: number | null; // null = 新建
  name: string;
  provider: string;
  model: string;
  base_url: string;
  api_key: string;
  embedding_model: string;
  consumers: string[];
  sync_agents: boolean;
  note: string;
}

const emptyDraft = (): Draft => ({
  id: null,
  name: "",
  provider: "openai",
  model: "",
  base_url: "",
  api_key: "",
  embedding_model: "",
  consumers: [],
  sync_agents: true,
  note: "",
});

const draftFromProfile = (p: ModelProfile): Draft => ({
  id: p.id,
  name: p.name,
  provider: p.provider || "openai",
  model: p.model,
  base_url: p.base_url,
  api_key: "",
  embedding_model: p.embedding_model,
  consumers: p.consumers ? p.consumers.split(",").filter(Boolean) : [],
  sync_agents: p.sync_agents == null ? true : p.sync_agents === 1,
  note: p.note,
});

function fmtTime(v: string | null | undefined): string {
  if (!v) return "—";
  return v.replace("T", " ").slice(0, 19);
}

export function Models() {
  const { data: models, loading, error, reload } = useAsync<ModelProfile[]>(
    () => listModelProfiles(),
    [],
  );
  const list = models ?? [];

  const [draft, setDraft] = useState<Draft | null>(null);
  const [saving, setSaving] = useState(false);
  const [applyingId, setApplyingId] = useState<number | null>(null);
  const [syncing, setSyncing] = useState(false);
  const [msg, setMsg] = useState("");
  const [errMsg, setErrMsg] = useState("");

  const notify = (okMsg: string) => {
    setMsg(okMsg);
    setErrMsg("");
  };
  const notifyErr = (e: unknown) => {
    setErrMsg(e instanceof Error ? e.message : String(e));
    setMsg("");
  };

  const onSave = async () => {
    if (!draft) return;
    if (!draft.name.trim() || !draft.model.trim()) {
      notifyErr("name 与 model 必填");
      return;
    }
    setSaving(true);
    try {
      const body: ModelProfileBody = {
        name: draft.name.trim(),
        provider: draft.provider.trim() || "openai",
        model: draft.model.trim(),
        base_url: draft.base_url.trim(),
        embedding_model: draft.embedding_model.trim(),
        consumers: draft.consumers.join(","),
        sync_agents: draft.sync_agents,
        note: draft.note.trim(),
      };
      if (draft.api_key) body.api_key = draft.api_key;
      if (draft.id == null) await createModelProfile(body);
      else await updateModelProfile(draft.id, body);
      setDraft(null);
      notify(`已保存模型档案「${draft.name}」`);
      await reload();
    } catch (e) {
      notifyErr(e);
    } finally {
      setSaving(false);
    }
  };

  const onApply = async (p: ModelProfile) => {
    setApplyingId(p.id);
    try {
      const r = await applyModelProfile(p.id);
      const parts = [`已应用「${p.name}」：core 侧分钟级热切生效`];
      if (typeof r.aiflow === "string") parts.push(`ai-flow：${r.aiflow}`);
      if (r.agents && typeof r.agents === "object") {
        for (const [k, v] of Object.entries(r.agents)) parts.push(`${k}：${v}`);
      }
      notify(parts.join("；"));
      await reload();
    } catch (e) {
      notifyErr(e);
    } finally {
      setApplyingId(null);
    }
  };

  const onDelete = async (p: ModelProfile) => {
    if (!window.confirm(`删除模型档案「${p.name}」？`)) return;
    try {
      await deleteModelProfile(p.id);
      notify(`已删除「${p.name}」`);
      await reload();
    } catch (e) {
      notifyErr(e);
    }
  };

  const onSyncAIFlow = async () => {
    setSyncing(true);
    try {
      const r = await syncAIFlowModels();
      notify(`ai-flow 同步完成：${r.aiflow}`);
    } catch (e) {
      notifyErr(e);
    } finally {
      setSyncing(false);
    }
  };

  const toggleConsumer = (c: string) => {
    if (!draft) return;
    setDraft({
      ...draft,
      consumers: draft.consumers.includes(c)
        ? draft.consumers.filter((x) => x !== c)
        : [...draft.consumers, c],
    });
  };

  const inputCls =
    "w-full bg-black/40 border border-(--border-default) px-2 py-1.5 text-xs text-white focus:border-white outline-none";

  return (
    <div className="p-6 h-full w-full overflow-auto">
      <div className="flex items-center justify-between mb-4">
        <div>
          <h1 className="text-lg font-bold tracking-tight text-white">模型管理</h1>
          <p className="text-[11px] text-(--text-muted) mt-0.5">
            模型档案（model_profiles）· 应用后全局生效：core 分钟级热切 · ai-flow 即时推送并热更全部 agent · coder/reviewer 下轮进化执行生效 · 勾选「agent 目录」的档案同步进 ai-flow/pi-web/ocr 的可选模型列表
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => void onSyncAIFlow()}
            disabled={syncing}
            className="flex items-center gap-2 px-3 py-1.5 text-xs text-(--text-secondary) border border-(--border-default) hover:text-white hover:border-white transition-colors cursor-pointer disabled:opacity-50"
          >
            {syncing ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5" />} 同步到 ai-flow
          </button>
          <button
            type="button"
            onClick={() => { setDraft(emptyDraft()); setMsg(""); setErrMsg(""); }}
            className="flex items-center gap-2 px-3 py-1.5 text-xs text-black bg-white border border-white hover:bg-(--bg-elevated) transition-colors cursor-pointer"
          >
            <Plus className="h-3.5 w-3.5" /> 新建模型
          </button>
        </div>
      </div>

      {error && <div className="text-xs text-red-400 border border-red-400/40 p-3 mb-4">{error}</div>}
      {(msg || errMsg) && (
        <div className={`text-xs p-3 mb-4 border ${errMsg ? "text-red-400 border-red-400/40" : "text-white border-white/40"}`}>
          {errMsg || msg}
        </div>
      )}

      {/* 编辑表单 */}
      {draft && (
        <div className="border border-white/40 p-4 mb-6 space-y-3">
          <div className="text-[10px] font-bold uppercase tracking-widest text-white">
            {draft.id == null ? "新建模型档案" : `编辑 #${draft.id}`}
          </div>
          <div className="grid grid-cols-2 gap-3">
            <label className="block">
              <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">名称（唯一，如 Qwen3.8）</span>
              <input className={inputCls} value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
            </label>
            <label className="block">
              <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">Model（上游模型标识）</span>
              <input className={inputCls} value={draft.model} onChange={(e) => setDraft({ ...draft, model: e.target.value })} />
            </label>
            <label className="block">
              <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">Provider（openai = OpenAI 兼容）</span>
              <input className={inputCls} value={draft.provider} onChange={(e) => setDraft({ ...draft, provider: e.target.value })} />
            </label>
            <label className="block">
              <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">Base URL（OpenAI 兼容端点）</span>
              <input className={inputCls} value={draft.base_url} onChange={(e) => setDraft({ ...draft, base_url: e.target.value })} placeholder="http://host:port/v1" />
            </label>
            <label className="block">
              <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">API Key{draft.id != null ? "（留空 = 保留原 key）" : ""}</span>
              <input className={inputCls} type="password" value={draft.api_key} onChange={(e) => setDraft({ ...draft, api_key: e.target.value })} />
            </label>
            <label className="block">
              <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">Embedding Model（core 向量检索用，可空）</span>
              <input className={inputCls} value={draft.embedding_model} onChange={(e) => setDraft({ ...draft, embedding_model: e.target.value })} />
            </label>
          </div>
          <div>
            <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">应用范围（留空 = 全部）</span>
            <div className="flex gap-2 mt-1">
              {CONSUMER_OPTIONS.map((c) => (
                <button
                  key={c}
                  type="button"
                  onClick={() => toggleConsumer(c)}
                  className={`px-2 py-1 text-[10px] font-bold uppercase tracking-widest border transition-colors cursor-pointer ${
                    draft.consumers.includes(c)
                      ? "text-black bg-white border-white"
                      : "text-(--text-muted) border-(--border-default) hover:text-white"
                  }`}
                >
                  {c}
                </button>
              ))}
            </div>
          </div>
          <label className="flex items-center gap-2 text-xs text-(--text-secondary) cursor-pointer select-none">
            <input
              type="checkbox"
              checked={draft.sync_agents}
              onChange={(e) => setDraft({ ...draft, sync_agents: e.target.checked })}
              className="accent-white"
            />
            纳入 agent 模型目录（同步到 ai-flow / pi-web / ocr 的可选模型列表）
          </label>
          <label className="block">
            <span className="text-[10px] uppercase tracking-widest text-(--text-muted)">备注</span>
            <input className={inputCls} value={draft.note} onChange={(e) => setDraft({ ...draft, note: e.target.value })} />
          </label>
          <div className="flex gap-2 pt-1">
            <button
              type="button"
              onClick={() => void onSave()}
              disabled={saving}
              className="flex items-center gap-2 px-3 py-1.5 text-xs text-black bg-white border border-white cursor-pointer disabled:opacity-50"
            >
              {saving ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Check className="h-3.5 w-3.5" />} 保存
            </button>
            <button
              type="button"
              onClick={() => setDraft(null)}
              className="px-3 py-1.5 text-xs text-(--text-secondary) border border-(--border-default) hover:text-white cursor-pointer"
            >
              取消
            </button>
          </div>
        </div>
      )}

      {/* 档案列表 */}
      {loading ? (
        <div className="flex items-center gap-2 text-xs text-(--text-muted) p-6">
          <Loader2 className="h-4 w-4 animate-spin" /> 加载中…
        </div>
      ) : (
        <table className="w-full text-xs">
          <thead>
            <tr className="text-[10px] uppercase tracking-widest text-(--text-muted) border-b border-(--border-default)">
              <th className="text-left py-2 pr-3">name</th>
              <th className="text-left py-2 pr-3">model</th>
              <th className="text-left py-2 pr-3">base_url</th>
              <th className="text-left py-2 pr-3">embedding</th>
              <th className="text-left py-2 pr-3">应用范围</th>
              <th className="text-left py-2 pr-3">agent 目录</th>
              <th className="text-left py-2 pr-3">key</th>
              <th className="text-left py-2 pr-3">更新</th>
              <th className="text-right py-2">操作</th>
            </tr>
          </thead>
          <tbody>
            {list.map((p) => (
              <tr key={p.id} className={`border-b border-(--border-divider) ${p.active ? "text-white" : "text-(--text-secondary)"}`}>
                <td className="py-2 pr-3 font-mono">
                  {p.active === 1 && (
                    <span className="mr-2 border border-white px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-widest">生效中</span>
                  )}
                  {p.name}
                </td>
                <td className="py-2 pr-3 font-mono">{p.model}</td>
                <td className="py-2 pr-3 font-mono text-(--text-muted)">{p.base_url || "—"}</td>
                <td className="py-2 pr-3 font-mono text-(--text-muted)">{p.embedding_model || "—"}</td>
                <td className="py-2 pr-3 font-mono text-(--text-muted)">{p.consumers || "全部"}</td>
                <td className="py-2 pr-3">{p.sync_agents === 0 ? "—" : "✓"}</td>
                <td className="py-2 pr-3">{p.has_api_key ? "✓" : "—"}</td>
                <td className="py-2 pr-3 text-(--text-muted)">{fmtTime(p.updated_at)}</td>
                <td className="py-2 text-right whitespace-nowrap">
                  {p.active !== 1 && (
                    <button
                      type="button"
                      onClick={() => void onApply(p)}
                      disabled={applyingId === p.id}
                      title="设为全局生效模型（按应用范围同步）"
                      className="mr-2 px-2 py-1 text-[10px] uppercase tracking-widest border border-white text-white hover:bg-white hover:text-black transition-colors cursor-pointer disabled:opacity-50"
                    >
                      {applyingId === p.id ? "应用中…" : "应用"}
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => { setDraft(draftFromProfile(p)); setMsg(""); setErrMsg(""); }}
                    title="编辑"
                    className="mr-2 p-1 text-(--text-muted) hover:text-white cursor-pointer"
                  >
                    <Pencil className="h-3.5 w-3.5" />
                  </button>
                  <button
                    type="button"
                    onClick={() => void onDelete(p)}
                    title="删除"
                    className="p-1 text-(--text-muted) hover:text-red-400 cursor-pointer"
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </button>
                </td>
              </tr>
            ))}
            {list.length === 0 && (
              <tr>
                <td colSpan={9} className="py-6 text-center text-xs text-(--text-muted)">
                  暂无模型档案。点击「新建模型」添加，保存后「应用」即可全局生效。
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}
    </div>
  );
}
