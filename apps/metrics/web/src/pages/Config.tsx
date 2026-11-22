// 设置页: 读取/修改存于 MySQL 的完整配置(与总览/证书页共享 token)。
// 保存后后端 30s 内热加载生效, 无需重启容器。
import { useCallback, useEffect, useState } from "react";
import { Database, KeyRound, Loader2, Save, ScrollText } from "lucide-react";
import { fetchConfig, saveConfig, type ConfigData } from "../api";
import { formatTime } from "../lib/status";

export function Config() {
  const [token, setToken] = useState(() => localStorage.getItem("deployToken") ?? "");
  const [data, setData] = useState<ConfigData | null>(null);
  const [text, setText] = useState("");
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const keepToken = (v: string) => {
    setToken(v);
    localStorage.setItem("deployToken", v);
  };

  const load = useCallback(async (tk: string) => {
    setLoading(true);
    setMsg(null);
    try {
      const d = await fetchConfig(tk);
      setData(d);
      setText(JSON.stringify(d.config, null, 2));
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (token) load(token);
  }, [token, load]);

  const format = () => {
    try {
      setText(JSON.stringify(JSON.parse(text), null, 2));
      setMsg({ ok: true, text: "JSON 格式正确" });
    } catch (err) {
      setMsg({ ok: false, text: "JSON 无效: " + (err instanceof Error ? err.message : String(err)) });
    }
  };

  const save = async () => {
    let cfg: unknown;
    try {
      cfg = JSON.parse(text);
    } catch (err) {
      setMsg({ ok: false, text: "JSON 无效, 未保存: " + (err instanceof Error ? err.message : String(err)) });
      return;
    }
    setSaving(true);
    setMsg(null);
    try {
      const r = await saveConfig(cfg as never, token);
      setData((d) => (d ? { ...d, updated_at: r.updated_at } : d));
      setMsg({ ok: true, text: `已保存(生效时间 ${formatTime(r.updated_at)}), 后端 30s 内热加载` });
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-8">
        <h1 className="text-2xl font-bold tracking-tighter uppercase">Settings</h1>
        <p className="mt-1 text-sm text-(--text-muted)">
          完整配置存于 MySQL <code className="font-mono">monitor_config</code> 表 · 保存后 30s 内热加载生效, 无需重启容器
        </p>
      </header>

      <div className="mb-4 flex flex-wrap items-end gap-3">
        <div className="flex flex-col gap-1">
          <label className="flex items-center gap-1.5 text-xs font-bold uppercase text-(--text-muted)">
            <KeyRound className="h-3.5 w-3.5" />
            部署 Token
          </label>
          <input
            type="password"
            value={token}
            onChange={(e) => keepToken(e.target.value)}
            placeholder="MONITOR_DEPLOY_TOKEN"
            className="border border-(--border-default) bg-(--bg-elevated) px-3 py-2 text-sm w-72 outline-none focus:border-(--accent, #6366f1)"
          />
        </div>
        <button
          onClick={() => load(token)}
          disabled={loading || !token}
          className="flex items-center gap-2 border border-(--border-default) px-3 py-2 text-xs font-bold uppercase transition hover:bg-(--bg-elevated) disabled:opacity-40"
        >
          {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <ScrollText className="h-3.5 w-3.5" />}
          重新加载
        </button>
        <button
          onClick={format}
          className="border border-(--border-default) px-3 py-2 text-xs font-bold uppercase transition hover:bg-(--bg-elevated)"
        >
          格式化
        </button>
        <button
          onClick={save}
          disabled={saving || !token}
          className="flex items-center gap-2 border border-(--border-default) bg-(--bg-elevated) px-3 py-2 text-xs font-bold uppercase transition hover:opacity-80 disabled:opacity-40"
        >
          {saving ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Save className="h-3.5 w-3.5" />}
          保存配置
        </button>
        {data && (
          <span className="flex items-center gap-1.5 text-xs text-(--text-muted)">
            <Database className="h-3.5 w-3.5" />
            存储: MySQL <code className="font-mono">monitor_config</code> · 更新于 {formatTime(data.updated_at)}
          </span>
        )}
      </div>

      {msg && (
        <div
          className={`mb-4 border px-3 py-2 text-sm ${
            msg.ok
              ? "border-(--success) text-(--success)"
              : "border-(--danger) text-(--danger)"
          }`}
        >
          {msg.text}
        </div>
      )}

      <textarea
        value={text}
        onChange={(e) => setText(e.target.value)}
        spellCheck={false}
        placeholder="输入 token 后自动加载当前配置(JSON)"
        className="w-full h-[calc(100vh-260px)] min-h-[400px] resize-y border border-(--border-default) bg-(--bg-elevated) p-4 font-mono text-xs leading-relaxed text-(--text-primary) outline-none focus:border-(--accent, #6366f1)"
      />

      <p className="mt-3 text-xs text-(--text-muted)">
        说明: 该 JSON 是完整配置(hosts / services / deployables / renewal / smtp 等), 所有数据均由
        数据库维护。修改 hosts 会同时影响证书监控与续签白名单;{" "}
        <code className="font-mono">renewal.email</code> 非空才启用自动续签。配置库 DSN 由环境变量
        <code className="font-mono"> MONITOR_DB_DSN</code> 指定(默认宿主机 workflow 库), 数据库不可达时服务不会启动。
      </p>
    </div>
  );
}
