import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Loader2, RefreshCw, Plus, Trash2, LogIn } from "lucide-react";
import {
  createSocialAccount,
  deleteSocialAccount,
  fetchSocialAccounts,
  fetchSocialRegistryChannels,
  triggerSocialAccountLogin,
  type SocialAccountView,
} from "../api";
import { useAsync } from "../hooks";
import {
  BADGE_LABELS,
  accountBadge,
  lastActiveAt,
  listChannels,
  quotaExhausted,
  quotaLabel,
  type AccountBadge,
} from "../accounts";

// M1-4 账号管理页（social 渠道账号池）：
//   - 渠道 tab + ?channel= 双向同步（tab 是 UI 状态，URL 是可分享/可书签的过滤条件）；
//   - 每行状态徽标（活跃 / 需登录 / 封号 / 冷却，冷却到期自动回落活跃，见 accounts.ts）；
//   - 今日配额用量（跨天按 0 计）与最近活动时间（max(使用, 登录)）；
//   - 新增账号 / 删除账号 / 触发登录（建 login 任务，登录动作在 worker 侧执行）。
// 黑白极简，风格与 XManage / Users 一致。

const BADGE_CLASS: Record<AccountBadge, string> = {
    active: "text-white border-white",
    need_login: "text-yellow-400 border-yellow-400",
    banned: "text-red-400 border-red-400",
    cooldown: "text-gray-400 border-gray-400",
};

function fmtTime(d: Date | null): string {
    return d && !Number.isNaN(d.getTime()) ? d.toLocaleString("zh-CN", { hour12: false }) : "-";
}

function Badge({ badge }: { badge: AccountBadge }) {
    return (
        <span className={`border px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-widest ${BADGE_CLASS[badge]}`}>
            {BADGE_LABELS[badge]}
        </span>
    );
}

export function AccountsPage() {
    const [searchParams, setSearchParams] = useSearchParams();
    const channel = searchParams.get("channel") ?? "";
    const [accounts, setAccounts] = useState<SocialAccountView[]>([]);
    const [channels, setChannels] = useState<string[]>([]);
    const [submitting, setSubmitting] = useState(false);
    const [busy, setBusy] = useState<string | null>(null); // `${channel}/${account_id}`
    const [notice, setNotice] = useState("");

    // 新增表单
    const [showForm, setShowForm] = useState(false);
    // 渠道默认值跟随当前 tab：从「xhs」tab 点新增，表单就预填 xhs（省一次选择，
    // 也保证新增的账号一定出现在当前 tab 里，不会出现「保存后列表里找不到」）。
    const [fChannel, setFChannel] = useState(() => channel);
    const [fAccountId, setFAccountId] = useState("");
    const [fLoginName, setFLoginName] = useState("");
    const [fMachineId, setFMachineId] = useState("");
    const [fSessionRef, setFSessionRef] = useState("");
    const [fDailyLimit, setFDailyLimit] = useState(0);

    const { data: state, loading, error, reload, setError } = useAsync<{ accounts: SocialAccountView[]; channels: string[] }>(
        async () => {
            // 渠道 tab 以注册表为准；注册表挂了（social 未部署）退回账号表里出现过的渠道。
            const [list, regs] = await Promise.all([
                fetchSocialAccounts(channel ? { channel } : {}),
                fetchSocialRegistryChannels().catch(() => [] as string[]),
            ]);
            return { accounts: list, channels: listChannels(regs, list) };
        },
        [channel],
    );

    // 取数结果同步进本地列表（reload 后立刻反映新增/删除/登录回报后的状态）。
    useEffect(() => {
        setAccounts(state?.accounts ?? []);
        setChannels(state?.channels ?? []);
    }, [state]);

    const setChannelTab = (c: string) => {
        setNotice("");
        const next = new URLSearchParams(searchParams);
        if (c) next.set("channel", c);
        else next.delete("channel");
        setSearchParams(next, { replace: true });
    };

    const handleCreate = async () => {
        if (submitting) return;
        if (!fChannel || !fAccountId) {
            setError("渠道 / 账号 ID 必填");
            return;
        }
        setSubmitting(true);
        try {
            await createSocialAccount({
                channel: fChannel,
                account_id: fAccountId,
                login_name: fLoginName,
                machine_id: fMachineId,
                // 留空 = 不带登录态建号：social 侧落 need_login（空壳 active 账号会被
                // 调度挑走并必然失败），运维随后点「触发登录」把状态推回 active。
                ...(fSessionRef ? { session_ref: fSessionRef, status: "active" } : { status: "need_login" }),
                daily_limit: fDailyLimit,
            });
            setShowForm(false);
            setFAccountId("");
            setFLoginName("");
            setFSessionRef("");
            setNotice(`账号 ${fChannel}/${fAccountId} 已保存`);
            await reload();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setSubmitting(false);
        }
    };

    const handleDelete = async (a: SocialAccountView) => {
        if (!window.confirm(`删除账号 ${a.channel}/${a.account_id}？登录态密文一并销毁。`)) return;
        const key = `${a.channel}/${a.account_id}`;
        setBusy(key);
        try {
            await deleteSocialAccount(a.channel, a.account_id);
            setNotice(`已删除 ${key}`);
            await reload();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setBusy(null);
        }
    };

    const handleLogin = async (a: SocialAccountView) => {
        const key = `${a.channel}/${a.account_id}`;
        setBusy(key);
        try {
            const r = await triggerSocialAccountLogin(a.channel, a.account_id);
            setNotice(
                r.queue
                    ? `登录任务 ${r.job_no} 已入队（worker 执行后自动回报账号状态）`
                    : `登录任务 ${r.job_no} 已建单（Redis 未就绪：未入队，需人工消费）`,
            );
            await reload();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setBusy(null);
        }
    };

    const now = useMemo(() => new Date(), [accounts]);

    return (
        <main className="flex-1 overflow-auto p-6">
            <header className="flex items-center justify-between mb-4">
                <h1 className="text-lg font-bold tracking-tight">账号管理</h1>
                <button
                    onClick={() => void reload()}
                    className="flex items-center gap-1 border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
                >
                    {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />} REFRESH
                </button>
            </header>

            {/* 渠道 tab（含「全部」；?channel= 与 tab 双向同步） */}
            <div className="flex flex-wrap items-center gap-2 mb-4">
                <button
                    onClick={() => setChannelTab("")}
                    className={`border px-4 py-1.5 text-xs font-bold uppercase transition ${channel === "" ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
                >
                    全部
                </button>
                {channels.map((c) => (
                    <button
                        key={c}
                        onClick={() => setChannelTab(c)}
                        className={`border px-4 py-1.5 text-xs font-bold uppercase transition ${channel === c ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
                    >
                        {c}
                    </button>
                ))}
                <button
                    onClick={() => {
                        setFChannel(channel || channels[0] || "");
                        setShowForm((v) => !v);
                    }}
                    className="ml-auto flex items-center gap-1 border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
                >
                    <Plus className="h-3 w-3" /> 新增账号
                </button>
            </div>

            {error && (
                <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">{error}</div>
            )}
            {notice && !error && (
                <div className="mb-4 border border-(--border-subtle) p-3 text-xs bg-(--bg-elevated) text-(--text-secondary)">{notice}</div>
            )}

            {showForm && (
                <div className="mb-4 border border-(--border-subtle) p-4 bg-(--bg-surface) space-y-3">
                    <div className="grid grid-cols-2 gap-3">
                        <label className="block text-[10px] uppercase text-(--text-muted)">
                            渠道（必填）
                            <input
                                list="social-channels"
                                value={fChannel}
                                onChange={(e) => setFChannel(e.target.value.trim())}
                                placeholder="xhs / x / bilibili / github…"
                                className="mt-1 w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-sm font-mono focus:outline-none focus:border-white"
                            />
                        </label>
                        <label className="block text-[10px] uppercase text-(--text-muted)">
                            账号 ID（必填，渠道内稳定标识）
                            <input
                                value={fAccountId}
                                onChange={(e) => setFAccountId(e.target.value.trim())}
                                placeholder="xhs-1"
                                className="mt-1 w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-sm font-mono focus:outline-none focus:border-white"
                            />
                        </label>
                        <label className="block text-[10px] uppercase text-(--text-muted)">
                            登录名 / 昵称（人读）
                            <input
                                value={fLoginName}
                                onChange={(e) => setFLoginName(e.target.value)}
                                className="mt-1 w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-sm focus:outline-none focus:border-white"
                            />
                        </label>
                        <label className="block text-[10px] uppercase text-(--text-muted)">
                            机器 ID（空 = 全局共享）
                            <input
                                value={fMachineId}
                                onChange={(e) => setFMachineId(e.target.value.trim())}
                                placeholder="bot-a"
                                className="mt-1 w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-sm font-mono focus:outline-none focus:border-white"
                            />
                        </label>
                        <label className="block text-[10px] uppercase text-(--text-muted)">
                            登录态（cookies/token；留空 = 建号后点「触发登录」获取）
                            <textarea
                                value={fSessionRef}
                                onChange={(e) => setFSessionRef(e.target.value)}
                                rows={2}
                                className="mt-1 w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-xs font-mono focus:outline-none focus:border-white"
                            />
                        </label>
                        <label className="block text-[10px] uppercase text-(--text-muted)">
                            每日限额（0 = 不限）
                            <input
                                type="number"
                                min={0}
                                value={fDailyLimit}
                                onChange={(e) => setFDailyLimit(Math.max(0, Number(e.target.value) || 0))}
                                className="mt-1 w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-sm focus:outline-none focus:border-white"
                            />
                        </label>
                    </div>
                    <div className="flex items-center gap-2">
                        <button
                            onClick={() => void handleCreate()}
                            disabled={submitting}
                            className="flex items-center gap-1 border border-white bg-(--accent) px-4 py-1.5 text-xs font-bold text-(--accent-foreground) hover:opacity-90 transition disabled:opacity-50"
                        >
                            {submitting ? <Loader2 className="h-3 w-3 animate-spin" /> : <Plus className="h-3 w-3" />} 保存账号
                        </button>
                        <button
                            onClick={() => setShowForm(false)}
                            className="border border-(--border-subtle) px-4 py-1.5 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
                        >
                            取消
                        </button>
                        <span className="text-[10px] text-(--text-muted)">登录态加密入库（AES-256-GCM），接口永不回显明文。</span>
                    </div>
                </div>
            )}

            <datalist id="social-channels">
                {channels.map((c) => <option key={c} value={c} />)}
            </datalist>

            <div className="border border-(--border-subtle)">
                <div className="p-4 border-b border-(--border-subtle) flex items-center justify-between">
                    <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted)">
                        账号列表（{accounts.length}）
                    </h2>
                    <span className="text-[10px] text-(--text-muted)">徽标 = 状态机 + 冷却到期回落；配额跨天自动清零</span>
                </div>

                {accounts.length === 0 ? (
                    <div className="p-8 text-center text-xs text-(--text-muted)">
                        {loading ? "加载中…" : channel ? `渠道 ${channel} 暂无账号` : "暂无账号：点右上「新增账号」"}
                    </div>
                ) : (
                    <table className="w-full text-xs">
                        <thead>
                            <tr className="text-left text-[10px] uppercase tracking-widest text-(--text-muted) border-b border-(--border-subtle)">
                                <th className="p-3">渠道 / 账号</th>
                                <th className="p-3">状态</th>
                                <th className="p-3">今日配额</th>
                                <th className="p-3">最近活动</th>
                                <th className="p-3">最近登录失败</th>
                                <th className="p-3 text-right">操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            {accounts.map((a) => {
                                const key = `${a.channel}/${a.account_id}`;
                                const badge = accountBadge(a, now);
                                return (
                                    <tr key={key} className="border-b border-(--border-subtle) last:border-b-0 hover:bg-(--bg-elevated) transition">
                                        <td className="p-3">
                                            <div className="font-mono font-bold">{a.account_id}</div>
                                            <div className="text-[10px] text-(--text-muted)">
                                                {a.channel}
                                                {a.machine_id ? ` · ${a.machine_id}` : ""}
                                                {a.login_name ? ` · ${a.login_name}` : ""}
                                            </div>
                                        </td>
                                        <td className="p-3">
                                            <Badge badge={badge} />
                                        </td>
                                        <td className={`p-3 font-mono ${quotaExhausted(a, now) ? "text-red-400" : ""}`}>
                                            {quotaLabel(a, now)}
                                        </td>
                                        <td className="p-3 font-mono text-(--text-secondary)">{fmtTime(lastActiveAt(a))}</td>
                                        <td className="p-3 max-w-64 truncate text-(--text-muted)" title={a.last_login_error || a.last_error || ""}>
                                            {a.last_login_error || a.last_error || "-"}
                                        </td>
                                        <td className="p-3">
                                            <div className="flex items-center justify-end gap-2">
                                                <button
                                                    onClick={() => void handleLogin(a)}
                                                    disabled={busy === key}
                                                    title="投递登录任务（worker 执行登录并回报账号池）"
                                                    className="flex items-center gap-1 border border-(--border-subtle) px-2 py-1 text-[10px] font-bold text-(--text-secondary) hover:bg-(--bg-surface) transition disabled:opacity-50"
                                                >
                                                    {busy === key ? <Loader2 className="h-3 w-3 animate-spin" /> : <LogIn className="h-3 w-3" />} 触发登录
                                                </button>
                                                <button
                                                    onClick={() => void handleDelete(a)}
                                                    disabled={busy === key}
                                                    className="flex items-center gap-1 border border-red-900 px-2 py-1 text-[10px] font-bold text-red-400 hover:bg-red-950/40 transition disabled:opacity-50"
                                                >
                                                    <Trash2 className="h-3 w-3" /> 删除
                                                </button>
                                            </div>
                                        </td>
                                    </tr>
                                );
                            })}
                        </tbody>
                    </table>
                )}
            </div>
        </main>
    );
}
