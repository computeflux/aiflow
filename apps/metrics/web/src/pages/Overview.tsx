// 总览页: 项目指标监控大盘(服务健康 + 证书状态摘要 + 部署控制)。
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  KeyRound,
  Loader2,
  RefreshCw,
  Server,
  ShieldCheck,
  XCircle,
} from "lucide-react";
import {
  fetchStatus,
  fetchDeployConfig,
  sendDeploy,
  type DeployConfig,
  type StatusData,
} from "../api";
import { Badge, StatCard, fmtDays, formatTime, targetName } from "../lib/status";

const actionLabels: Record<string, string> = {
  restart: "重启",
  rebuild: "重编译",
  start: "启动",
  stop: "停止",
};

interface DeployRun {
  name: string;
  action: string;
  ok: boolean;
  detail: string;
  output?: string;
}

export function Overview() {
  const [data, setData] = useState<StatusData | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [autoRefresh, setAutoRefresh] = useState(true);

  const [deployCfg, setDeployCfg] = useState<DeployConfig | null>(null);
  const [token, setToken] = useState(() => localStorage.getItem("deployToken") ?? "");
  const [running, setRunning] = useState<string | null>(null); // "name:action"
  const [lastRun, setLastRun] = useState<DeployRun | null>(null);

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
    fetchDeployConfig().then(setDeployCfg).catch(() => setDeployCfg(null));
  }, [load]);

  // 自动刷新: 30 秒
  useEffect(() => {
    if (!autoRefresh) return;
    const timer = setInterval(load, 30_000);
    return () => clearInterval(timer);
  }, [autoRefresh, load]);

  const deploy = async (name: string, action: string) => {
    setRunning(`${name}:${action}`);
    setLastRun(null);
    try {
      const r = await sendDeploy(name, action, token);
      setLastRun({
        name,
        action,
        ok: r.ok,
        detail: r.ok
          ? `成功 · 耗时 ${(r.duration_ms / 1000).toFixed(1)}s`
          : `失败 (exit ${r.exit_code})`,
        output: r.output,
      });
      load(); // 部署后刷新服务状态
    } catch (err) {
      setLastRun({
        name,
        action,
        ok: false,
        detail: err instanceof Error ? err.message : String(err),
      });
    } finally {
      setRunning(null);
    }
  };

  const services = data?.services ?? [];
  const hosts = data?.hosts ?? [];
  const svcUp = services.filter((s) => s.status === "UP").length;
  const svcDown = services.length - svcUp;
  const certOk = hosts.filter((h) => h.status === "OK").length;
  const certBad = hosts.length - certOk;

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-8 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase">Project Metrics Monitor</h1>
          <p className="mt-1 text-sm text-(--text-muted)">
            监控 {services.length} 个服务 · {hosts.length} 个证书 · 采集于 {formatTime(data?.collected_at)}
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
          label="服务在线"
          value={data ? `${svcUp}/${services.length}` : "-"}
          tone="text-(--info)"
        />
        <StatCard
          icon={<XCircle className="h-5 w-5" />}
          label="服务离线"
          value={data ? String(svcDown) : "-"}
          tone={svcDown ? "text-(--danger)" : "text-(--text-secondary)"}
        />
        <StatCard
          icon={<CheckCircle2 className="h-5 w-5" />}
          label="证书正常"
          value={data ? String(certOk) : "-"}
          tone="text-(--success)"
        />
        <StatCard
          icon={<AlertTriangle className="h-5 w-5" />}
          label="证书异常"
          value={data ? String(certBad) : "-"}
          tone={certBad ? "text-(--warning)" : "text-(--text-secondary)"}
        />
      </div>

      {/* 部署 token */}
      {deployCfg && (
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
            placeholder={
              deployCfg.enabled ? "输入 MONITOR_DEPLOY_TOKEN" : "未启用: 请设置环境变量 MONITOR_DEPLOY_TOKEN"
            }
            className="flex-1 max-w-72 bg-transparent border border-(--border-subtle) px-2 py-1 text-xs font-mono focus:outline-none"
          />
          <span
            className={`text-xs shrink-0 ${
              deployCfg.enabled ? "text-(--text-muted)" : "text-(--warning)"
            }`}
          >
            {!deployCfg.enabled ? "部署已禁用" : token ? "已就绪" : "未填写"}
          </span>
        </div>
      )}

      {/* 服务健康 */}
      <div className="border border-(--border-subtle)">
        <div className="px-4 py-3 border-b border-(--border-subtle) flex items-center gap-2 text-xs font-bold uppercase text-(--text-muted)">
          <Activity className="h-4 w-4" />
          服务健康
        </div>
        {!data ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">LOADING...</div>
        ) : services.length === 0 ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
            未配置监控服务(在面板设置页添加 services)
          </div>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="border-b border-(--border-subtle) text-(--text-muted)">
              <tr>
                <th className="px-4 py-3 font-bold uppercase">服务</th>
                <th className="px-4 py-3 font-bold uppercase">状态</th>
                <th className="px-4 py-3 font-bold uppercase">延迟</th>
                <th className="px-4 py-3 font-bold uppercase">HTTP</th>
                <th className="px-4 py-3 font-bold uppercase">备注</th>
                <th className="px-4 py-3 font-bold uppercase">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-(--border-divider)">
              {services.map((s) => {
                const dep = deployCfg?.deployables.find((d) => d.name === s.name);
                const canDeploy = deployCfg?.enabled && !!dep;
                return (
                  <tr key={s.name || s.url} className="hover:bg-(--bg-elevated)">
                    <td className="px-4 py-3">
                      <div className="font-bold font-mono">{s.name}</div>
                      <div className="text-xs text-(--text-muted) font-mono truncate max-w-56">{s.url}</div>
                    </td>
                    <td className="px-4 py-3">
                      <span
                        className={`inline-flex items-center gap-1.5 text-xs font-bold uppercase ${
                          s.status === "UP" ? "text-(--success)" : "text-(--danger)"
                        }`}
                      >
                        <span
                          className={`h-1.5 w-1.5 rounded-full ${
                            s.status === "UP" ? "bg-(--success)" : "bg-(--danger)"
                          }`}
                        />
                        {s.status}
                      </span>
                    </td>
                    <td className="px-4 py-3 font-mono text-xs text-(--text-secondary)">
                      {s.status === "UP" && s.latency_ms != null ? `${s.latency_ms} ms` : "n/a"}
                    </td>
                    <td className="px-4 py-3 font-mono text-xs text-(--text-secondary)">
                      {s.http_code || "-"}
                    </td>
                    <td className="px-4 py-3 text-xs text-(--text-muted) truncate max-w-56">
                      {s.error || (s.http_code && s.http_code >= 400 ? `HTTP ${s.http_code}` : "-")}
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      {canDeploy ? (
                        dep!.actions.map((a) => (
                          <button
                            key={a}
                            disabled={running !== null}
                            onClick={() => deploy(s.name, a)}
                            title={`执行 ${dep!.script} ${a}`}
                            className="ml-1 border border-(--border-subtle) px-2.5 py-1 text-xs font-bold hover:bg-(--bg-elevated) transition disabled:opacity-40"
                          >
                            {running === `${s.name}:${a}` ? (
                              <Loader2 className="h-3 w-3 animate-spin inline" />
                            ) : (
                              actionLabels[a] ?? a
                            )}
                          </button>
                        ))
                      ) : (
                        <span className="text-xs text-(--text-muted)">-</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* 部署结果 */}
      {lastRun && (
        <div
          className={`mt-4 border p-3 ${
            lastRun.ok ? "border-(--border-subtle)" : "border-(--danger)"
          }`}
        >
          <div className="mb-2 flex items-center gap-2 text-xs font-bold">
            {lastRun.ok ? (
              <CheckCircle2 className="h-4 w-4 text-(--success)" />
            ) : (
              <XCircle className="h-4 w-4 text-(--danger)" />
            )}
            <span className="font-mono">
              {lastRun.name} · {actionLabels[lastRun.action] ?? lastRun.action}
            </span>
            <span className="text-(--text-muted)">{lastRun.detail}</span>
          </div>
          {lastRun.output && (
            <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all border border-(--border-subtle) bg-(--bg-surface) p-3 font-mono text-xs">
              {lastRun.output}
            </pre>
          )}
        </div>
      )}

      {/* 证书状态摘要 */}
      <div className="mt-8 border border-(--border-subtle)">
        <div className="flex items-center justify-between border-b border-(--border-subtle) px-4 py-3">
          <div className="flex items-center gap-2 text-xs font-bold uppercase text-(--text-muted)">
            <ShieldCheck className="h-4 w-4" />
            证书状态
          </div>
          <Link
            to="/certs"
            className="text-xs font-bold text-(--text-secondary) hover:text-white transition-colors"
          >
            查看详情 →
          </Link>
        </div>
        {!data ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">LOADING...</div>
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
              </tr>
            </thead>
            <tbody className="divide-y divide-(--border-divider)">
              {hosts.map((r) => (
                <tr key={targetName(r)} className="hover:bg-(--bg-elevated)">
                  <td className="px-4 py-3 font-bold font-mono text-xs">{targetName(r)}</td>
                  <td className="px-4 py-3">
                    <Badge status={r.status} />
                  </td>
                  <td className="px-4 py-3 font-mono text-xs text-(--text-secondary)">{fmtDays(r)}</td>
                  <td className="px-4 py-3 font-mono text-xs text-(--text-secondary) whitespace-nowrap">
                    {formatTime(r.not_after)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
