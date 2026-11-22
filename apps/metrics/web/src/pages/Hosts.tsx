// 域名管理页: 增删监控域名(存于 MySQL monitor_config.hosts, 30s 内热加载生效)。
// 列表同时展示每个域名的证书状态(来自 /api/status)。
import { useCallback, useEffect, useState } from "react";
import {
  Globe,
  KeyRound,
  Loader2,
  Plus,
  RefreshCw,
  Trash2,
} from "lucide-react";
import {
  addHost,
  fetchStatus,
  removeHost,
  type StatusData,
} from "../api";
import { Badge, fmtDays, formatTime } from "../lib/status";

export function Hosts() {
  const [data, setData] = useState<StatusData | null>(null);
  const [loading, setLoading] = useState(false);
  const [token, setToken] = useState(() => localStorage.getItem("deployToken") ?? "");
  const [newHost, setNewHost] = useState("");
  const [busy, setBusy] = useState<string | null>(null); // "add" 或待删除的域名
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setData(await fetchStatus());
    } catch {
      // 状态加载失败不阻塞增删操作
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 30_000);
    return () => clearInterval(timer);
  }, [load]);

  const hosts = data?.hosts ?? [];

  const doAdd = async () => {
    const h = newHost.trim();
    if (!h) return;
    setBusy("add");
    setMsg(null);
    try {
      const r = await addHost(h, token);
      setNewHost("");
      setMsg({ ok: true, text: `已添加 ${r.host}, 后端 30s 内热加载生效` });
      load();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setBusy(null);
    }
  };

  const doRemove = async (host: string) => {
    if (!confirm(`确认删除监控域名 ${host}?`)) return;
    setBusy(host);
    setMsg(null);
    try {
      const r = await removeHost(host, token);
      setMsg({ ok: true, text: `已删除 ${host}, 剩余 ${r.hosts.length} 个域名` });
      load();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-8">
        <h1 className="text-2xl font-bold tracking-tighter uppercase">Domains</h1>
        <p className="mt-1 text-sm text-(--text-muted)">
          监控域名存于 MySQL <code className="font-mono">monitor_config</code> · 增删后 30s 内热加载生效, 无需重启容器
        </p>
      </header>

      {/* token */}
      <div className="mb-4 border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2.5 flex items-center gap-3">
        <KeyRound className="h-4 w-4 text-(--text-muted) shrink-0" />
        <span className="text-xs font-bold uppercase text-(--text-muted) shrink-0">部署 Token</span>
        <input
          type="password"
          value={token}
          onChange={(e) => {
            setToken(e.target.value);
            localStorage.setItem("deployToken", e.target.value);
          }}
          placeholder="MONITOR_DEPLOY_TOKEN"
          className="flex-1 max-w-72 bg-transparent border border-(--border-subtle) px-2 py-1 text-xs font-mono focus:outline-none"
        />
        <span className={`text-xs shrink-0 ${token ? "text-(--text-muted)" : "text-(--warning)"}`}>
          {token ? "已就绪" : "未填写(增删不可用)"}
        </span>
      </div>

      {/* 添加域名 */}
      <div className="mb-4 flex items-center gap-3">
        <div className="flex items-center gap-2 border border-(--border-default) bg-(--bg-elevated) px-3 flex-1 max-w-md">
          <Globe className="h-4 w-4 text-(--text-muted) shrink-0" />
          <input
            value={newHost}
            onChange={(e) => setNewHost(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && doAdd()}
            placeholder="new.example.com"
            spellCheck={false}
            className="flex-1 bg-transparent py-2 text-sm font-mono outline-none"
          />
        </div>
        <button
          onClick={doAdd}
          disabled={busy !== null || !token || !newHost.trim()}
          className="flex items-center gap-2 border border-(--border-default) bg-(--bg-elevated) px-4 py-2 text-xs font-bold uppercase transition hover:opacity-80 disabled:opacity-40"
        >
          {busy === "add" ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Plus className="h-3.5 w-3.5" />}
          添加域名
        </button>
        <button
          onClick={load}
          disabled={loading}
          className="flex items-center gap-2 border border-(--border-default) px-3 py-2 text-xs font-bold uppercase transition hover:bg-(--bg-elevated) disabled:opacity-40"
        >
          {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5" />}
          刷新
        </button>
      </div>

      {msg && (
        <div
          className={`mb-4 border px-3 py-2 text-sm ${
            msg.ok ? "border-(--success) text-(--success)" : "border-(--danger) text-(--danger)"
          }`}
        >
          {msg.text}
        </div>
      )}

      {/* 域名列表 */}
      <div className="border border-(--border-subtle)">
        <div className="px-4 py-3 border-b border-(--border-subtle) flex items-center gap-2 text-xs font-bold uppercase text-(--text-muted)">
          <Globe className="h-4 w-4" />
          监控域名 ({hosts.length})
        </div>
        {!data ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">LOADING...</div>
        ) : hosts.length === 0 ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
            未配置监控域名
          </div>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="border-b border-(--border-subtle) text-(--text-muted)">
              <tr>
                <th className="px-4 py-3 font-bold uppercase">域名</th>
                <th className="px-4 py-3 font-bold uppercase">证书状态</th>
                <th className="px-4 py-3 font-bold uppercase">剩余天数</th>
                <th className="px-4 py-3 font-bold uppercase">到期时间</th>
                <th className="px-4 py-3 font-bold uppercase">备注</th>
                <th className="px-4 py-3 font-bold uppercase">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-(--border-divider)">
              {hosts.map((r) => (
                <tr key={r.host} className="hover:bg-(--bg-elevated)">
                  <td className="px-4 py-3 font-bold font-mono text-xs">{r.host}</td>
                  <td className="px-4 py-3">
                    <Badge status={r.status} />
                  </td>
                  <td className="px-4 py-3 font-mono text-xs text-(--text-secondary)">{fmtDays(r)}</td>
                  <td className="px-4 py-3 font-mono text-xs text-(--text-secondary) whitespace-nowrap">
                    {formatTime(r.not_after)}
                  </td>
                  <td className="px-4 py-3 text-xs text-(--text-muted) truncate max-w-56">
                    {r.error || r.chain_error || "-"}
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap">
                    <button
                      onClick={() => doRemove(r.host)}
                      disabled={busy !== null}
                      title={`删除 ${r.host}`}
                      className="border border-(--border-subtle) px-2.5 py-1 text-xs font-bold text-(--danger) hover:bg-(--bg-elevated) transition disabled:opacity-40"
                    >
                      {busy === r.host ? (
                        <Loader2 className="h-3 w-3 animate-spin inline" />
                      ) : (
                        <Trash2 className="h-3 w-3 inline" />
                      )}
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <p className="mt-3 text-xs text-(--text-muted)">
        说明: 新域名需已解析到本机且 443 端口有 TLS 证书, 否则监控显示 ERROR; 自动续签(Let's Encrypt
        DNS-01)要求域名在阿里云 DNS 下且 <code className="font-mono">renewal.email</code> 非空。
      </p>
    </div>
  );
}
