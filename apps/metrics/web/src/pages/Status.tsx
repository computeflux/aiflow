import { Fragment, useCallback, useEffect, useMemo, useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  Download,
  KeyRound,
  Loader2,
  RefreshCw,
  Server,
  ShieldCheck,
  XCircle,
} from "lucide-react";
import {
  certDownloadURL,
  fetchStatus,
  triggerRenew,
  type CertFile,
  type StatusData,
} from "../api";
import { Badge, StatCard, fmtDays, formatTime, targetName } from "../lib/status";

// 证书状态详情页(总览页 / 有摘要表格, 此处展示完整信息)。

export function Status() {
  const [data, setData] = useState<StatusData | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);

  // 证书续签(与总览页共享同一个 token, 存 localStorage)
  const [token, setToken] = useState(() => localStorage.getItem("deployToken") ?? "");
  const [renewing, setRenewing] = useState<string | null>(null);
  const [renewMsg, setRenewMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setData(await fetchStatus());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 自动刷新: 30 秒
  useEffect(() => {
    if (!autoRefresh) return;
    const timer = setInterval(load, 30_000);
    return () => clearInterval(timer);
  }, [autoRefresh, load]);

  const hosts = data?.hosts ?? [];
  const apiErr = data?.error;
  const renewMap = useMemo(
    () => new Map((data?.renewals ?? []).map((x) => [targetName(x), x])),
    [data],
  );

  const doRenew = async (host: string) => {
    // host 是监控条目(可含 :port); 续签只对域名部分签发(后端处理)
    setRenewing(host);
    setRenewMsg(null);
    try {
      const r = await triggerRenew(host, token);
      setRenewMsg({ ok: true, text: `续签成功: 新证书有效期至 ${formatTime(r.not_after)}` });
      load();
    } catch (err) {
      setRenewMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setRenewing(null);
    }
  };

  const downloadCert = async (host: string, file: CertFile) => {
    try {
      const res = await fetch(certDownloadURL(host, file), {
        headers: { "X-Deploy-Token": token },
      });
      if (!res.ok) {
        setRenewMsg({ ok: false, text: `下载失败: HTTP ${res.status}` });
        return;
      }
      const blob = await res.blob();
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = `${host}-${file}.pem`;
      a.click();
      URL.revokeObjectURL(a.href);
    } catch (err) {
      setRenewMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };

  const counts = useMemo(() => {
    const c = { total: hosts.length, ok: 0, warn: 0, bad: 0 };
    for (const h of hosts) {
      if (h.status === "OK") c.ok++;
      else if (h.status === "WARN") c.warn++;
      else c.bad++;
    }
    return c;
  }, [hosts]);

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-8 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase">
            SSL Certificate Monitor
          </h1>
          <p className="mt-1 text-sm text-(--text-muted)">
            监控 {counts.total} 个主机 · 采集于 {formatTime(data?.collected_at)}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setAutoRefresh(!autoRefresh)}
            className={`border px-3 py-2 text-xs font-bold uppercase transition ${
              autoRefresh
                ? "border-white bg-(--accent) text-(--accent-foreground)"
                : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"
            }`}
          >
            自动刷新 {autoRefresh ? "ON" : "OFF"}
          </button>
          <button
            onClick={load}
            disabled={loading}
            className="border border-(--border-subtle) px-4 py-2 text-sm font-bold hover:bg-(--bg-elevated) transition disabled:opacity-50 flex items-center gap-2"
          >
            {loading ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <RefreshCw className="h-4 w-4" />
            )}{" "}
            REFRESH
          </button>
        </div>
      </header>

      {error && (
        <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">
          {error}
        </div>
      )}

      {/* 统计卡片 */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-8">
        <StatCard
          icon={<Server className="h-5 w-5" />}
          label="主机总数"
          value={data ? String(counts.total) : "-"}
          tone="text-(--info)"
        />
        <StatCard
          icon={<CheckCircle2 className="h-5 w-5" />}
          label="正常"
          value={data ? String(counts.ok) : "-"}
          tone="text-(--success)"
        />
        <StatCard
          icon={<AlertTriangle className="h-5 w-5" />}
          label="即将到期"
          value={data ? String(counts.warn) : "-"}
          tone={counts.warn ? "text-(--warning)" : "text-(--text-secondary)"}
        />
        <StatCard
          icon={<XCircle className="h-5 w-5" />}
          label="过期/异常"
          value={data ? String(counts.bad) : "-"}
          tone={counts.bad ? "text-(--danger)" : "text-(--text-secondary)"}
        />
      </div>

      {/* 续签 token(与总览页部署 token 共用) */}
      {data?.renewals && data.renewals.length > 0 && (
        <div className="mb-4 border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2.5 flex items-center gap-3">
          <KeyRound className="h-4 w-4 text-(--text-muted) shrink-0" />
          <span className="text-xs font-bold uppercase text-(--text-muted) shrink-0">续签/下载 Token</span>
          <input
            type="password"
            value={token}
            onChange={(e) => {
              setToken(e.target.value);
              localStorage.setItem("deployToken", e.target.value);
            }}
            placeholder="输入 MONITOR_DEPLOY_TOKEN(手动续签与下载私钥需要)"
            className="flex-1 max-w-72 bg-transparent border border-(--border-subtle) px-2 py-1 text-xs font-mono focus:outline-none"
          />
          <span className={`text-xs shrink-0 ${token ? "text-(--text-muted)" : "text-(--warning)"}`}>
            {token ? "已就绪" : "未填写"}
          </span>
        </div>
      )}

      {renewMsg && (
        <div
          className={`mb-4 border p-3 text-sm font-bold ${
            renewMsg.ok ? "border-(--border-subtle)" : "border-(--danger)"
          }`}
        >
          <span className={renewMsg.ok ? "text-(--success)" : "text-red-500"}>{renewMsg.text}</span>
        </div>
      )}

      {/* 证书状态明细 */}
      <div className="border border-(--border-subtle)">
        <div className="px-4 py-3 border-b border-(--border-subtle) flex items-center gap-2 text-xs font-bold uppercase text-(--text-muted)">
          <ShieldCheck className="h-4 w-4" />
          证书状态
        </div>
        {!data || apiErr ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
            {apiErr ? `状态查询失败: ${apiErr}` : "LOADING..."}
          </div>
        ) : hosts.length === 0 ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
            未配置监控主机
          </div>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="border-b border-(--border-subtle) text-(--text-muted)">
              <tr>
                <th className="px-4 py-3 font-bold uppercase">主机</th>
                <th className="px-4 py-3 font-bold uppercase">状态</th>
                <th className="px-4 py-3 font-bold uppercase">剩余天数</th>
                <th className="px-4 py-3 font-bold uppercase">到期时间</th>
                <th className="px-4 py-3 font-bold uppercase">签发者</th>
                <th className="px-4 py-3 font-bold uppercase">续签</th>
                <th className="px-4 py-3" />
              </tr>
            </thead>
            <tbody className="divide-y divide-(--border-divider)">
              {hosts.map((r) => {
                const name = targetName(r);
                const isOpen = expanded === name;
                return (
                  <Fragment key={name}>
                    <tr
                      className="hover:bg-(--bg-elevated) cursor-pointer"
                      onClick={() => setExpanded(isOpen ? null : name)}
                    >
                      <td className="px-4 py-3">
                        <div className="font-bold font-mono">{name}</div>
                        {(r.subject || r.error) && (
                          <div className="text-xs text-(--text-muted) font-mono truncate">
                            {r.error ? `error: ${r.error}` : r.subject}
                          </div>
                        )}
                      </td>
                      <td className="px-4 py-3">
                        <Badge status={r.status} />
                      </td>
                      <td
                        className={`px-4 py-3 font-mono text-xs ${
                          r.status === "EXPIRED"
                            ? "text-(--danger)"
                            : r.status === "WARN"
                              ? "text-(--warning)"
                              : "text-(--text-primary)"
                        }`}
                      >
                        {fmtDays(r)}
                      </td>
                      <td className="px-4 py-3 text-xs text-(--text-secondary) whitespace-nowrap">
                        {formatTime(r.not_after)}
                      </td>
                      <td className="px-4 py-3 text-xs text-(--text-secondary) truncate max-w-56">
                        {r.issuer || "-"}
                      </td>
                      <td
                        className="px-4 py-3 whitespace-nowrap"
                        onClick={(e) => e.stopPropagation()}
                      >
                        {(() => {
                          const ri = renewMap.get(name);
                          if (!ri) return <span className="text-xs text-(--text-muted)">-</span>;
                          const btnCls =
                            "border border-(--border-subtle) px-2 py-1 text-xs font-bold hover:bg-(--bg-elevated) transition disabled:opacity-40";
                          return (
                            <div className="flex items-center gap-1.5">
                              {ri.due && !ri.cert_ready && (
                                <span className="text-xs font-bold text-(--warning)">即将到期</span>
                              )}
                              {ri.cert_ready && (
                                <span className="text-xs font-bold text-(--success)">新证书就绪</span>
                              )}
                              {renewing === name ? (
                                <button disabled className={btnCls}>
                                  <Loader2 className="h-3 w-3 animate-spin inline" /> 签发中…
                                </button>
                              ) : (
                                <button onClick={() => doRenew(name)} className={btnCls}>
                                  <RefreshCw className="h-3 w-3 inline" /> 续签
                                </button>
                              )}
                              {ri.cert_ready && (
                                <>
                                  <button onClick={() => downloadCert(r.host, "fullchain")} className={btnCls}>
                                    <Download className="h-3 w-3 inline" /> 全链
                                  </button>
                                  <button onClick={() => downloadCert(r.host, "privkey")} className={btnCls}>
                                    <Download className="h-3 w-3 inline" /> 私钥
                                  </button>
                                </>
                              )}
                            </div>
                          );
                        })()}
                      </td>
                      <td className="px-4 py-3 text-right">
                        <ChevronDown
                          className={`h-4 w-4 inline text-(--text-muted) transition-transform ${
                            isOpen ? "rotate-180" : ""
                          }`}
                        />
                      </td>
                    </tr>
                    {isOpen && (
                      <tr className="bg-(--bg-surface)">
                        <td colSpan={7} className="px-4 py-4">
                          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
                            <div className="space-y-1.5 text-xs">
                              <div className="text-[10px] font-bold uppercase text-(--text-muted)">
                                有效期
                              </div>
                              <div className="font-mono text-(--text-secondary)">
                                {formatTime(r.not_before)} ~ {formatTime(r.not_after)}
                              </div>
                              <div className="text-[10px] font-bold uppercase text-(--text-muted) pt-2">
                                SANs（{r.sans?.length ?? 0}）
                              </div>
                              {r.sans && r.sans.length > 0 ? (
                                <div className="flex flex-wrap gap-1.5 pt-1">
                                  {r.sans.map((san) => (
                                    <span
                                      key={san}
                                      className="border border-(--border-subtle) bg-(--bg-elevated) px-2 py-0.5 font-mono text-xs text-(--text-secondary)"
                                    >
                                      {san}
                                    </span>
                                  ))}
                                </div>
                              ) : (
                                <div className="font-mono text-(--text-muted)">-</div>
                              )}
                              {(() => {
                                const ri = renewMap.get(name);
                                if (!ri || (!ri.last_try && !ri.cert_ready)) return null;
                                return (
                                  <div className="pt-2">
                                    <div className="text-[10px] font-bold uppercase text-(--text-muted)">
                                      续签记录
                                    </div>
                                    {ri.cert_ready && (
                                      <div className="font-mono text-(--success) pt-1">
                                        新证书: 有效期至 {formatTime(ri.cert_not_after)}
                                      </div>
                                    )}
                                    {ri.last_try && (
                                      <div className="font-mono text-(--text-secondary)">
                                        上次尝试: {formatTime(ri.last_try)}
                                      </div>
                                    )}
                                    {ri.last_error && (
                                      <div className="font-mono text-(--danger) break-all">
                                        错误: {ri.last_error}
                                      </div>
                                    )}
                                  </div>
                                );
                              })()}
                            </div>
                            {(r.error || r.chain_error) && (
                              <div className="space-y-1.5 text-xs">
                                {r.error && (
                                  <>
                                    <div className="text-[10px] font-bold uppercase text-(--danger)">
                                      连接错误
                                    </div>
                                    <div className="font-mono text-(--danger) break-all">
                                      {r.error}
                                    </div>
                                  </>
                                )}
                                {r.chain_error && (
                                  <>
                                    <div className="text-[10px] font-bold uppercase text-(--warning) pt-2">
                                      证书链错误
                                    </div>
                                    <div className="font-mono text-(--warning) break-all">
                                      {r.chain_error}
                                    </div>
                                  </>
                                )}
                              </div>
                            )}
                            {!r.error && !r.chain_error && (
                              <div className="text-xs text-(--text-muted) font-mono">
                                证书链验证通过，无错误。
                              </div>
                            )}
                          </div>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}