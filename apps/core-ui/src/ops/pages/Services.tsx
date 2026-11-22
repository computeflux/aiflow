import { useRef, useState } from "react";
import {
    RefreshCw, Loader2, Server, RotateCcw, Square, Play, Undo2,
    Settings2, X, AlertTriangle, Lock,
} from "lucide-react";
import {
    fetchServices, fetchServiceDetail, restartService, stopService,
    startService, rollbackService, updateServiceParams, DEFAULT_LOG_TAIL,
    type ServiceView, type ServiceDetail,
} from "../api";
import { useAuth, useAsync, useInterval } from "../hooks";

// 运维中心「运行服务总览」（wi-1792321102）：
// 实时展示 workflow 正在运行的程序/软件（compose 服务 + docker 实时状态），
// 支持操作：重启 / 停止 / 启动 / 回滚 / 详情 / 改参。
// 红线：不纳管（external）服务只读展示，不可执行任何写操作；
// 所有写操作有确认弹窗，危险操作（停止/回滚）二次确认；后端写 audit_logs 留痕。

const REFRESH_INTERVAL_MS = 15_000; // 自动刷新间隔

// 状态 → 展示（运行中 / 停止 / 异常）。
// 异常判定统一由 abnormalReason 派生，避免与头部「异常 N」统计口径分叉。
function stateBadge(v: ServiceView) {
    if (v.state === "running") {
        if (v.health === "unhealthy") return { label: "异常", cls: "text-(--danger) border-(--danger)" };
        if (v.health === "starting") return { label: "启动中", cls: "text-(--warning) border-(--warning)" };
        return { label: "运行中", cls: "text-(--ok) border-(--ok)" };
    }
    if (v.state === "exited") {
        if (v.one_shot && v.exit_code === 0) return { label: "已完成", cls: "text-(--text-secondary) border-(--border-default)" };
        if (v.exit_code === 0) return { label: "已停止", cls: "text-(--text-secondary) border-(--border-default)" };
        return { label: "异常退出", cls: "text-(--danger) border-(--danger)" };
    }
    if (v.state === "restarting") return { label: "重启中", cls: "text-(--warning) border-(--warning)" };
    if (v.state === "paused") return { label: "已暂停", cls: "text-(--warning) border-(--warning)" };
    if (v.state === "not_created") return { label: "未创建", cls: "text-(--text-muted) border-(--border-default)" };
    if (v.state === "dead") return { label: "异常", cls: "text-(--danger) border-(--danger)" };
    if (v.state === "created") return { label: "已创建", cls: "text-(--text-muted) border-(--border-default)" };
    if (v.state === "removing") return { label: "删除中", cls: "text-(--warning) border-(--warning)" };
    return { label: "停止", cls: "text-(--text-secondary) border-(--border-default)" };
}

// 异常判定（唯一口径）：返回异常原因（非空 = 异常），stateBadge 与头部「异常 N」统计都基于它派生，
// 避免两处各自维护导致分叉。运行中不健康 / 非零退出（含非一次性）/ dead 容器。
// 注意：exit_code === 0 的 exited 视为主动停止（如用户点「停止」），不算异常。
function abnormalReason(v: ServiceView): string | null {
    if (v.state === "running" && v.health === "unhealthy") return "运行中不健康";
    if (v.state === "exited" && v.exit_code !== 0) return "异常退出";
    if (v.state === "dead") return "dead 容器";
    return null;
}

function isAbnormal(v: ServiceView): boolean {
    return abnormalReason(v) !== null;
}

function fmtTime(v: string | null | undefined): string {
    // Docker 零值时间（容器已创建但从未启动）为 0001-01-01T00:00:00Z（真值），归一为「—」
    if (!v || v.startsWith("0001-01-01")) return "—";
    return v.replace("T", " ").slice(0, 19);
}

// 统一脱敏工具（键名判定 + 值形态判定），复用于环境变量渲染与日志展示，避免多处正则失配。
// 服务端已对 env / logs 做脱敏（GetServiceDetail），此处为展示层二次防御（非唯一安全边界）。
// 敏感词子串匹配（命中 API_KEY/DB_PASSWORD 等复合键）；仅 AUTH 需独立成词（按非字母数字分词后精确匹配），
// 避免误伤 OAUTH_*/AUTHORITY。不用环视（lookbehind）以兼容旧浏览器。
const SENSITIVE_SUBSTR_RE = /PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIAL|DSN/;
const CRED_VALUE_RE = /:\/\/[^:@\s/]+:[^@\s/]+@/;
function isSensitiveKey(k: string): boolean {
    const upper = k.toUpperCase();
    if (SENSITIVE_SUBSTR_RE.test(upper)) return true;
    // AUTH 独立成词：按非字母数字分词后精确匹配（OAUTH_CLIENT_ID → [OAUTH, CLIENT, ID] 不命中）
    return upper.split(/[^A-Z0-9]+/).includes("AUTH");
}

// 日志脱敏（展示层二次防御）：对 `KEY=...`（敏感键，允许行首裸键）与 `://user:pass@` 片段替换。
// 注：日志正则只覆盖敏感子串词（password/token/secret/dsn 等，日志凭据泄漏的主形态）；
// AUTH 因需独立成词判定（防 OAUTH 误伤）不纳入日志正则，属可接受的取舍（与服务端 redactLogs 一致）。
const LOG_KEY_VAL_RE = /\b([A-Za-z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIAL|DSN)[A-Za-z0-9_]*)\s*[=:]\s*\S+/gi;
function redactLog(logs: string): string {
    if (!logs) return logs;
    return logs
        .replace(LOG_KEY_VAL_RE, (_m, key: string) => `${key}=<redacted>`)
        .replace(CRED_VALUE_RE, "://<redacted>@");
}

type PendingAction =
    | { kind: "restart"; name: string }
    | { kind: "stop"; name: string }
    | { kind: "start"; name: string }
    | { kind: "rollback"; name: string; image: string }
    | { kind: "params"; name: string; env: Record<string, string>; ports: string[]; reason: string };

export function Services() {
    const { user } = useAuth();
    const isAdmin = !!user?.roles.includes("admin");

    const [msg, setMsg] = useState<string | null>(null);
    const [busy, setBusy] = useState<string | null>(null); // 正在执行操作的服务名
    const [filter, setFilter] = useState("");
    const [autoRefresh, setAutoRefresh] = useState(true);

    // 确认弹窗（危险操作二次确认）
    const [confirm, setConfirm] = useState<PendingAction | null>(null);
    const [confirmStage, setConfirmStage] = useState(1); // 1 = 第一次确认，2 = 危险操作二次确认
    const [confirmErr, setConfirmErr] = useState<string | null>(null);

    // 详情抽屉
    const [detail, setDetail] = useState<ServiceDetail | null>(null);
    const [detailLoading, setDetailLoading] = useState(false);
    const [detailError, setDetailError] = useState<string | null>(null); // 详情请求失败单独存放（不被列表轮询清除）

    // 改参弹窗
    const [paramModal, setParamModal] = useState<ServiceView | null>(null);
    const [envText, setEnvText] = useState("");
    const [portsText, setPortsText] = useState("");
    const [reasonText, setReasonText] = useState("");
    const [paramErr, setParamErr] = useState<string | null>(null);

    // 静默轮询由 useInterval 驱动；成功（含静默）清 error，失败仅在非静默时报错
    const { data, loading, error, reload: load, reloadSilently } = useAsync<ServiceView[]>(() => fetchServices(), []);
    const rows = data ?? [];

    // 自动刷新（实时状态，静默：不闪 loading / 不清 error）
    useInterval(() => void reloadSilently(), autoRefresh ? REFRESH_INTERVAL_MS : null);

    // 详情请求时序守卫（不复用 loadSeqRef：自动刷新会自增它，会误取消待返回的详情请求）
    const detailSeqRef = useRef(0);
    // 关闭详情抽屉：自增 detailSeqRef 使在途请求失效，防止已关闭的抽屉被在途响应重新弹出
    const closeDetail = () => {
        detailSeqRef.current++;
        setDetail(null);
        setDetailLoading(false);
        setDetailError(null);
    };
    const openDetail = async (name: string) => {
        const seq = ++detailSeqRef.current;
        setDetailLoading(true);
        setDetail(null);
        setDetailError(null);
        try {
            const d = await fetchServiceDetail(name);
            if (seq !== detailSeqRef.current) return;
            setDetail(d);
        } catch (e) {
            if (seq !== detailSeqRef.current) return;
            setDetailError(e instanceof Error ? e.message : String(e));
        } finally {
            if (seq === detailSeqRef.current) setDetailLoading(false);
        }
    };

    const openParams = (v: ServiceView) => {
        setParamModal(v);
        // 端口 / env 均不预填：留空 = 不改（与占位提示一致）；预填会让「仅改 env」也回传端口触发重建
        setPortsText("");
        setEnvText("");
        setReasonText("");
        setParamErr(null);
    };

    // 统一打开确认弹窗：重置 stage / error，避免新增动作时遗漏状态重置
    const openConfirm = (action: PendingAction) => {
        setConfirm(action);
        setConfirmStage(1);
        setConfirmErr(null);
    };

    // 执行确认弹窗中的动作
    const runConfirm = async () => {
        if (!confirm) return;
        setBusy(confirm.name);
        setConfirmErr(null);
        try {
            switch (confirm.kind) {
                case "restart":
                    await restartService(confirm.name);
                    setMsg(`已重启 ${confirm.name}`);
                    break;
                case "stop":
                    await stopService(confirm.name);
                    setMsg(`已停止 ${confirm.name}`);
                    break;
                case "start":
                    await startService(confirm.name);
                    setMsg(`已启动 ${confirm.name}`);
                    break;
                case "rollback":
                    // 发送前 trim：复制镜像名常带尾随空格，后端直接用原值建容器会 invalid reference
                    await rollbackService(confirm.name, confirm.image.trim());
                    setMsg(`已回滚 ${confirm.name} → ${confirm.image.trim()}`);
                    break;
                case "params":
                    await updateServiceParams(confirm.name, {
                        env: Object.keys(confirm.env).length > 0 ? confirm.env : undefined,
                        ports: confirm.ports.length > 0 ? confirm.ports : undefined,
                        reason: confirm.reason,
                    });
                    setMsg(`已修改 ${confirm.name} 启动参数（重建容器生效）`);
                    break;
            }
            setConfirm(null);
            setConfirmStage(1);
            await load();
        } catch (e) {
            setConfirmErr(e instanceof Error ? e.message : String(e));
        } finally {
            setBusy(null);
        }
    };

    // 确认弹窗「确认」按钮：危险操作（stop / rollback）需二次确认
    const onConfirmClick = () => {
        if (!confirm) return;
        if ((confirm.kind === "stop" || confirm.kind === "rollback") && confirmStage === 1) {
            setConfirmStage(2);
            return;
        }
        void runConfirm();
    };

    // 改参弹窗提交
    const submitParams = () => {
        if (!paramModal) return;
        setParamErr(null);
        const env: Record<string, string> = {};
        for (const line of envText.split("\n")) {
            const t = line.trim();
            if (!t || t.startsWith("#")) continue;
            const idx = t.indexOf("=");
            if (idx <= 0) { setParamErr(`env 行格式错误（需 KEY=VALUE）: ${t}`); return; }
            env[t.slice(0, idx).trim()] = t.slice(idx + 1).trim();
        }
        const ports = portsText.split("\n").map((l) => l.trim()).filter(Boolean);
        // 端口格式校验（与后端 parsePortBindings 对齐：host:container / ip:host:container / container）
        for (const p of ports) {
            const parts = p.split(":");
            if (parts.length > 3 || parts.some((s) => s === "")) {
                setParamErr(`端口格式错误（需 host:container / ip:host:container / container）: ${p}`);
                return;
            }
        }
        if (Object.keys(env).length === 0 && ports.length === 0) {
            setParamErr("env 和端口至少改一项");
            return;
        }
        setParamModal(null);
        openConfirm({ kind: "params", name: paramModal.name, env, ports, reason: reasonText.trim() });
    };

    const visible = rows.filter((r) => !filter || r.name.toLowerCase().includes(filter.toLowerCase()));
    const running = rows.filter((r) => r.state === "running").length;
    const abnormal = rows.filter(isAbnormal).length;

    return (
        <div className="p-6 h-full w-full overflow-auto">
            <div className="flex items-center justify-between mb-4">
                <div>
                    <h1 className="text-lg font-bold tracking-tight text-white flex items-center gap-2">
                        <Server className="h-5 w-5" /> 运行服务总览
                    </h1>
                    <p className="text-[11px] text-(--text-muted) mt-0.5">
                        纳管 {rows.filter((r) => r.managed).length} / 共 {rows.length} 服务 · 运行中 {running} · 异常 {abnormal}
                        （数据 = software_assets + docker 实时状态，{autoRefresh ? `每 ${REFRESH_INTERVAL_MS / 1000}s 自动刷新` : "自动刷新已关"}）
                    </p>
                </div>
                <div className="flex items-center gap-2">
                    <input
                        value={filter}
                        onChange={(e) => setFilter(e.target.value)}
                        placeholder="过滤服务名…"
                        className="text-xs bg-black/40 border border-(--border-default) px-3 py-1.5 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white w-40"
                    />
                    <label className="flex items-center gap-1.5 text-[11px] text-(--text-secondary) cursor-pointer select-none">
                        <input type="checkbox" checked={autoRefresh} onChange={(e) => setAutoRefresh(e.target.checked)} className="accent-white" />
                        自动刷新
                    </label>
                    <button
                        type="button"
                        onClick={() => void load()}
                        className="flex items-center gap-2 px-3 py-1.5 text-xs text-(--text-secondary) border border-(--border-default) hover:text-white hover:border-white transition-colors cursor-pointer"
                    >
                        <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} /> 刷新
                    </button>
                </div>
            </div>

            {error && <div className="text-xs text-red-400 border border-red-400/40 p-3 mb-4">{error}</div>}
            {msg && (
                <div className="text-xs p-3 mb-4 border text-white border-white/40 flex items-center justify-between">
                    <span>{msg}</span>
                    <button type="button" onClick={() => setMsg(null)} className="text-(--text-muted) hover:text-white cursor-pointer">✕</button>
                </div>
            )}

            {!isAdmin && (
                <div className="text-xs text-(--warning) border border-(--warning)/40 p-3 mb-4 flex items-center gap-2">
                    <Lock className="h-3.5 w-3.5 shrink-0" /> 当前角色无运维写权限（需 admin）：仅可查看，操作按钮已禁用。
                </div>
            )}

            {loading && rows.length === 0 ? (
                <div className="flex items-center gap-2 text-xs text-(--text-muted) p-8">
                    <Loader2 className="h-4 w-4 animate-spin" /> 加载服务列表…
                </div>
            ) : (
                <div className="border border-(--border-default) overflow-auto">
                    <table className="w-full text-left text-[11px]">
                        <thead>
                            <tr className="text-(--text-muted) border-b border-(--border-divider)">
                                <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">服务</th>
                                <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">镜像</th>
                                <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">状态</th>
                                <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">端口</th>
                                <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">启动时间</th>
                                <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px]">纳管</th>
                                <th className="px-3 py-2 font-bold uppercase tracking-widest text-[10px] text-right">操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            {visible.map((v) => {
                                const badge = stateBadge(v);
                                // 写操作统一禁用：非 admin / 不纳管 / 操作中 / 容器未创建（后端必然 404/500）
                                const disabled = !isAdmin || !v.managed || busy !== null || v.state === "not_created";
                                // 停止：running / restarting / paused 可停；启动：仅 exited / created 可启
                                //（paused 需 unpause 语义、restarting/removing/dead 启停均无意义 → 禁用）
                                const canStop = v.state === "running" || v.state === "restarting" || v.state === "paused";
                                const canStart = v.state === "exited" || v.state === "created";
                                return (
                                    <tr key={v.name} className="border-b border-(--border-divider) last:border-b-0 hover:bg-(--bg-elevated)">
                                        <td className="px-3 py-2 font-mono text-white">{v.name}</td>
                                        <td className="px-3 py-2 text-(--text-muted) max-w-[220px] truncate" title={v.image}>{v.image || "—"}</td>
                                        <td className="px-3 py-2">
                                            <span className={`border px-1.5 py-0.5 text-[10px] font-bold ${badge.cls}`}>{badge.label}</span>
                                            {v.status && <span className="ml-1.5 text-(--text-muted)">{v.status}</span>}
                                        </td>
                                        <td className="px-3 py-2 font-mono text-(--text-secondary)">
                                            {v.ports.length > 0 ? v.ports.join(", ") : "—"}
                                        </td>
                                        <td className="px-3 py-2 text-(--text-muted)">{fmtTime(v.started_at)}</td>
                                        <td className="px-3 py-2">
                                            {v.managed ? (
                                                <span className="text-(--ok)">纳管</span>
                                            ) : (
                                                <span className="text-(--text-muted) flex items-center gap-1" title={`start_method=${v.start_method}：只读展示，不可操作`}>
                                                    <Lock className="h-3 w-3" /> 不纳管
                                                </span>
                                            )}
                                        </td>
                                        <td className="px-3 py-2">
                                            <div className="flex items-center justify-end gap-1 shrink-0">
                                                <ActionBtn title="查看详情" disabled={busy !== null} onClick={() => void openDetail(v.name)}>
                                                    <Settings2 className="h-3.5 w-3.5" /> 详情
                                                </ActionBtn>
                                                <ActionBtn title="重启" disabled={disabled} onClick={() => openConfirm({ kind: "restart", name: v.name })}>
                                                    <RotateCcw className="h-3.5 w-3.5" /> 重启
                                                </ActionBtn>
                                                {canStop ? (
                                                    <ActionBtn title="停止（危险操作，二次确认）" disabled={disabled} danger onClick={() => openConfirm({ kind: "stop", name: v.name })}>
                                                        <Square className="h-3.5 w-3.5" /> 停止
                                                    </ActionBtn>
                                                ) : canStart ? (
                                                    <ActionBtn title="启动" disabled={disabled} onClick={() => openConfirm({ kind: "start", name: v.name })}>
                                                        <Play className="h-3.5 w-3.5" /> 启动
                                                    </ActionBtn>
                                                ) : (
                                                    <ActionBtn title={v.state === "not_created" ? "容器未创建，无法启动（需先部署）" : `当前状态（${v.state}）不可启停`} disabled>
                                                        <Play className="h-3.5 w-3.5" /> 启动
                                                    </ActionBtn>
                                                )}
                                                <ActionBtn title="回滚到指定版本（危险操作，二次确认）" disabled={disabled} danger onClick={() => openConfirm({ kind: "rollback", name: v.name, image: "" })}>
                                                    <Undo2 className="h-3.5 w-3.5" /> 回滚
                                                </ActionBtn>
                                                <ActionBtn title="修改启动参数（env/端口，重建容器生效）" disabled={disabled} onClick={() => openParams(v)}>
                                                    改参
                                                </ActionBtn>
                                            </div>
                                        </td>
                                    </tr>
                                );
                            })}
                            {visible.length === 0 && (
                                <tr><td colSpan={7} className="px-3 py-6 text-center text-(--text-muted)">无匹配服务</td></tr>
                            )}
                        </tbody>
                    </table>
                </div>
            )}

            {/* 确认弹窗（危险操作二次确认） */}
            {confirm && (
                <div className="fixed inset-0 z-50 bg-black/70 flex items-center justify-center p-6" onClick={() => { if (busy === null) { setConfirm(null); setConfirmStage(1); } }}>
                    <div className="w-full max-w-md bg-(--bg-sidebar) border border-(--border-default) p-5" onClick={(e) => e.stopPropagation()}>
                        <div className="flex items-center justify-between mb-3">
                            <h2 className="text-sm font-bold text-white flex items-center gap-2">
                                {(confirm.kind === "stop" || confirm.kind === "rollback") && (
                                    <AlertTriangle className="h-4 w-4 text-(--danger)" />
                                )}
                                {confirmTitle(confirm)}
                                <span className="text-[11px] font-mono text-(--text-muted)">{confirm.name}</span>
                            </h2>
                            <button type="button" disabled={busy !== null} onClick={() => { setConfirm(null); setConfirmStage(1); }} className="text-(--text-muted) hover:text-white cursor-pointer">✕</button>
                        </div>

                        <p className="text-xs text-(--text-secondary) mb-3">
                            {confirmMessage(confirm)}
                            {(confirm.kind === "stop" || confirm.kind === "rollback") && confirmStage === 2 && (
                                <span className="block mt-2 text-(--danger) font-bold">⚠️ 危险操作二次确认：此操作会影响线上服务，确认继续？</span>
                            )}
                        </p>

                        {confirm.kind === "rollback" && (
                            <div className="mb-3">
                                <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">回滚目标镜像（image）</div>
                                <input
                                    value={confirm.image}
                                    onChange={(e) => setConfirm({ ...confirm, image: e.target.value })}
                                    placeholder="如 computeflux/workflow-api:1.2.0"
                                    className="w-full text-xs font-mono bg-black/40 border border-(--border-default) px-3 py-2 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white"
                                />
                                <p className="text-[10px] text-(--text-muted) mt-1">
                                    版本历史可在「详情」中查看（skill_versions）；留空不可提交。
                                </p>
                            </div>
                        )}

                        {confirmErr && <div className="text-xs text-red-400 border border-red-400/40 p-2 mb-3">{confirmErr}</div>}

                        <div className="flex justify-end gap-2">
                            <button
                                type="button"
                                disabled={busy !== null}
                                onClick={() => { setConfirm(null); setConfirmStage(1); }}
                                className="px-4 py-2 text-xs font-bold border border-(--border-default) text-(--text-secondary) hover:text-white hover:border-white transition-colors cursor-pointer"
                            >
                                取消
                            </button>
                            <button
                                type="button"
                                disabled={busy !== null || (confirm.kind === "rollback" && !confirm.image.trim())}
                                onClick={onConfirmClick}
                                className={`flex items-center gap-1.5 px-4 py-2 text-xs font-bold disabled:opacity-40 transition-opacity cursor-pointer ${
                                    (confirm.kind === "stop" || confirm.kind === "rollback") ? "bg-(--danger) text-white hover:opacity-80" : "bg-white text-black hover:opacity-80"
                                }`}
                            >
                                {busy !== null ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : null}
                                {(confirm.kind === "stop" || confirm.kind === "rollback") && confirmStage === 1 ? "确认（危险）" : "执行"}
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* 改参弹窗 */}
            {paramModal && (
                <div className="fixed inset-0 z-50 bg-black/70 flex items-center justify-center p-6" onClick={() => setParamModal(null)}>
                    <div className="w-full max-w-lg bg-(--bg-sidebar) border border-(--border-default) p-5" onClick={(e) => e.stopPropagation()}>
                        <div className="flex items-center justify-between mb-3">
                            <h2 className="text-sm font-bold text-white">修改启动参数 <span className="text-[11px] font-mono text-(--text-muted)">{paramModal.name}</span></h2>
                            <button type="button" onClick={() => setParamModal(null)} className="text-(--text-muted) hover:text-white cursor-pointer">✕</button>
                        </div>
                        <p className="text-[11px] text-(--text-muted) mb-3">
                            修改后重建容器生效（需再次确认）。⚠️ API 侧为运行时覆盖，持久化需同步 compose 文件 / 根 .env（deployctl 或人工），避免与 auto-update 冲突。
                        </p>
                        <div className="space-y-3">
                            <div>
                                <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">环境变量（每行 KEY=VALUE，留空 = 不改）</div>
                                <textarea
                                    value={envText}
                                    onChange={(e) => setEnvText(e.target.value)}
                                    rows={4}
                                    placeholder={"APP_PORT=9000\nLOG_LEVEL=debug"}
                                    className="w-full text-[11px] font-mono bg-black/40 border border-(--border-default) p-3 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white resize-y"
                                />
                            </div>
                            <div>
                                <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">端口（每行 host:container，留空 = 不改）</div>
                                <textarea
                                    value={portsText}
                                    onChange={(e) => setPortsText(e.target.value)}
                                    rows={3}
                                    placeholder={"8001:8001\n127.0.0.1:5181:80"}
                                    className="w-full text-[11px] font-mono bg-black/40 border border-(--border-default) p-3 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white resize-y"
                                />
                            </div>
                            <div>
                                <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-1">修改原因（审计留痕）</div>
                                <input
                                    value={reasonText}
                                    onChange={(e) => setReasonText(e.target.value)}
                                    placeholder="如：临时调大日志级别排查问题"
                                    className="w-full text-xs bg-black/40 border border-(--border-default) px-3 py-2 text-white placeholder:text-(--text-muted) focus:outline-none focus:border-white"
                                />
                            </div>
                        </div>
                        {paramErr && <div className="text-xs text-red-400 border border-red-400/40 p-2 mt-3">{paramErr}</div>}
                        <div className="flex justify-end gap-2 mt-4">
                            <button type="button" onClick={() => setParamModal(null)} className="px-4 py-2 text-xs font-bold border border-(--border-default) text-(--text-secondary) hover:text-white hover:border-white transition-colors cursor-pointer">取消</button>
                            <button type="button" onClick={submitParams} className="px-4 py-2 text-xs font-bold bg-white text-black hover:opacity-80 transition-opacity cursor-pointer">下一步（确认）</button>
                        </div>
                    </div>
                </div>
            )}

            {/* 详情抽屉（detailLoading 期间也渲染，显示加载态） */}
            {(detailLoading || detail || detailError) && (
                <div className="fixed inset-0 z-40">
                    <div className="absolute inset-0 bg-black/60" onClick={closeDetail} />
                    <div className="absolute right-0 top-0 h-full w-full max-w-2xl bg-(--bg-app) border-l border-(--border-default) overflow-y-auto">
                        <div className="sticky top-0 bg-(--bg-app) border-b border-(--border-default) px-5 py-3 flex items-center justify-between z-10">
                            <h2 className="text-sm font-bold text-white flex items-center gap-2">
                                <Server className="h-4 w-4" /> {detail?.name ?? "…"}
                                {detail && <span className="text-[10px] font-mono text-(--text-muted)">{detail.container}</span>}
                            </h2>
                            <button type="button" onClick={closeDetail} className="text-(--text-muted) hover:text-white cursor-pointer"><X className="h-4 w-4" /></button>
                        </div>
                        {detailLoading && !detail ? (
                            <div className="flex items-center gap-2 text-xs text-(--text-muted) p-8">
                                <Loader2 className="h-4 w-4 animate-spin" /> 加载详情…
                            </div>
                        ) : detailError ? (
                            <div className="text-xs text-red-400 border border-red-400/40 p-4 m-5">{detailError}</div>
                        ) : detail && (
                        <div className="p-5 space-y-5">
                            {/* 概览 */}
                            <section>
                                <SectionTitle>概览</SectionTitle>
                                <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 text-[11px]">
                                    <KV k="状态" v={detail.status || detail.state} />
                                    <KV k="健康" v={detail.health || "无 healthcheck"} />
                                    <KV k="镜像" v={detail.image || "—"} mono />
                                    <KV k="启动时间" v={fmtTime(detail.started_at)} />
                                    <KV k="端口" v={(detail.ports ?? []).join(", ") || "—"} mono />
                                    <KV k="纳管" v={detail.managed ? "纳管" : `不纳管（${detail.start_method}，只读）`} />
                                    <KV k="compose 文件" v={detail.compose_file} mono />
                                    <KV k="restart" v={detail.restart || "—"} mono />
                                </div>
                            </section>

                            {/* 版本历史 */}
                            <section>
                                <SectionTitle>版本历史（{(detail.versions ?? []).length}）</SectionTitle>
                                {(detail.versions ?? []).length === 0 ? (
                                    <div className="text-[11px] text-(--text-muted)">无版本记录（skill_versions 无该服务条目）</div>
                                ) : (
                                    <table className="w-full text-left text-[11px]">
                                        <thead>
                                            <tr className="text-(--text-muted) border-b border-(--border-divider)">
                                                <th className="py-1.5 pr-3 font-bold text-[10px] uppercase tracking-widest">版本</th>
                                                <th className="py-1.5 pr-3 font-bold text-[10px] uppercase tracking-widest">镜像</th>
                                                <th className="py-1.5 pr-3 font-bold text-[10px] uppercase tracking-widest">状态</th>
                                                <th className="py-1.5 font-bold text-[10px] uppercase tracking-widest">创建时间</th>
                                            </tr>
                                        </thead>
                                        <tbody>
                                            {(detail.versions ?? []).map((v) => (
                                                <tr key={v.version} className="border-b border-(--border-divider) last:border-b-0">
                                                    <td className="py-1.5 pr-3 font-mono text-white">{v.version}</td>
                                                    <td className="py-1.5 pr-3 font-mono text-(--text-muted) max-w-[240px] truncate" title={v.image}>{v.image}</td>
                                                    <td className="py-1.5 pr-3 text-(--text-secondary)">{v.status}</td>
                                                    <td className="py-1.5 text-(--text-muted)">{fmtTime(v.created_at)}</td>
                                                </tr>
                                            ))}
                                        </tbody>
                                    </table>
                                )}
                            </section>

                            {/* 环境变量（敏感键脱敏） */}
                            <section>
                                <SectionTitle>环境变量（compose environment，{Object.keys(detail.environment ?? {}).length}）</SectionTitle>
                                {Object.keys(detail.environment ?? {}).length === 0 ? (
                                    <div className="text-[11px] text-(--text-muted)">无（env 来自 env_file，见 compose 文件）</div>
                                ) : (
                                    <pre className="text-[11px] font-mono bg-black/40 border border-(--border-default) p-3 whitespace-pre-wrap break-all text-(--text-secondary) max-h-64 overflow-y-auto">
                                        {Object.entries(detail.environment ?? {}).map(([k, v]) => `${k}=${isSensitiveKey(k) || /:\/\/[^:@/]+:[^@/]+@/.test(v) ? "<redacted>" : v}`).join("\n")}
                                    </pre>
                                )}
                            </section>

                            {/* compose 配置 */}
                            <section>
                                <SectionTitle>compose 配置</SectionTitle>
                                <div className="text-[11px] text-(--text-muted) mb-1.5">
                                    文件：<span className="font-mono text-(--text-secondary)">{detail.compose_file}</span>
                                    {(detail.volumes ?? []).length > 0 && <> · 卷：{(detail.volumes ?? []).join(", ")}</>}
                                    {(detail.networks ?? []).length > 0 && <> · 网络：{(detail.networks ?? []).join(", ")}</>}
                                </div>
                                {detail.health_check && (
                                    <div className="text-[11px] text-(--text-muted)">
                                        healthcheck：{detail.health_check.test.join(" ")}（interval {detail.health_check.interval} / timeout {detail.health_check.timeout} / retries {detail.health_check.retries}）
                                    </div>
                                )}
                            </section>

                            {/* 日志 */}
                            <section>
                                <SectionTitle>最近日志（tail {DEFAULT_LOG_TAIL}，已脱敏）</SectionTitle>
                                {detail.logs ? (
                                    <pre className="text-[10px] font-mono bg-black/60 border border-(--border-default) p-3 whitespace-pre-wrap break-all text-(--text-secondary) max-h-96 overflow-y-auto">{redactLog(detail.logs)}</pre>
                                ) : (
                                    <div className="text-[11px] text-(--text-muted)">无日志（容器未创建或日志为空）</div>
                                )}
                            </section>
                        </div>
                        )}
                    </div>
                </div>
            )}
        </div>
    );
}

function ActionBtn({ children, onClick, disabled, title, danger }: {
    children: React.ReactNode;
    onClick?: () => void;
    disabled?: boolean;
    title?: string;
    danger?: boolean;
}) {
    return (
        <button
            type="button"
            title={title}
            disabled={disabled}
            onClick={onClick}
            className={`flex items-center gap-1 px-2 py-1 text-[10px] font-bold border whitespace-nowrap shrink-0 transition-colors cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed ${
                danger
                    ? "border-(--danger)/60 text-(--danger) hover:bg-(--danger) hover:text-white"
                    : "border-(--border-default) text-(--text-secondary) hover:text-white hover:border-white"
            }`}
        >
            {children}
        </button>
    );
}

function SectionTitle({ children }: { children: React.ReactNode }) {
    return <div className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted) mb-2">{children}</div>;
}

function KV({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
    return (
        <div className="flex gap-2">
            <span className="text-(--text-muted) shrink-0">{k}</span>
            <span className={`text-(--text-secondary) break-all ${mono ? "font-mono" : ""}`}>{v}</span>
        </div>
    );
}

function confirmTitle(c: PendingAction): string {
    switch (c.kind) {
        case "restart": return "重启服务";
        case "stop": return "停止服务";
        case "start": return "启动服务";
        case "rollback": return "回滚服务";
        case "params": return "修改启动参数";
    }
}

function confirmMessage(c: PendingAction): string {
    switch (c.kind) {
        case "restart": return "将执行 docker restart（不重建镜像）。确认重启？";
        case "stop": return "将执行 docker stop（镜像与数据保留，可再启动）。停止后该服务不可用。";
        case "start": return "将执行 docker start 启动已停止的容器。";
        case "rollback": return "将以指定镜像重建容器（停 → 删 → 创建 → 启动）。回滚期间服务短暂不可用。";
        case "params": return `将重建容器应用新参数（env ${Object.keys(c.env).length} 项 / 端口 ${c.ports.length} 项${c.reason ? `，原因：${c.reason}` : ""}）。重建期间服务短暂不可用。`;
    }
}
