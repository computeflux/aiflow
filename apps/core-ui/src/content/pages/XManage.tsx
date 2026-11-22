import { useCallback, useEffect, useState } from "react";
import { Loader2, RefreshCw, Plus, Edit3, Trash2, PowerOff, Power, Heart, Send } from "lucide-react";
import {
  fetchXAccounts,
  createXAccount,
  updateXAccount,
  deleteXAccount,
  fetchFollowTargets,
  createFollowTarget,
  updateFollowTarget,
  deleteFollowTarget,
  dispatchFollowTarget,
  type XAccount,
  type FollowTarget,
} from "../api";

function fmtTime(v?: string) {
  return v ? new Date(v).toLocaleString() : "-";
}

export function XManagePage() {
  const [tab, setTab] = useState<"accounts" | "targets">("accounts");

  const [accounts, setAccounts] = useState<XAccount[]>([]);
  const [targets, setTargets] = useState<FollowTarget[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [machineFilter, setMachineFilter] = useState("");
  const [busyDispatch, setBusyDispatch] = useState<number | null>(null);

  // account form
  const [showAccountForm, setShowAccountForm] = useState(false);
  const [editingAccountId, setEditingAccountId] = useState<number | null>(null);
  const [accMachineId, setAccMachineId] = useState("");
  const [accUsername, setAccUsername] = useState("");
  const [accAuthToken, setAccAuthToken] = useState("");
  const [accCt0, setAccCt0] = useState("");
  const [accEnabled, setAccEnabled] = useState(true);
  const [accDailyLimit, setAccDailyLimit] = useState(50);
  const [accCooldown, setAccCooldown] = useState(10);

  // target form
  const [showTargetForm, setShowTargetForm] = useState(false);
  const [editingTargetId, setEditingTargetId] = useState<number | null>(null);
  const [tgtHandle, setTgtHandle] = useState("");
  const [tgtEnabled, setTgtEnabled] = useState(true);

  const loadAccounts = useCallback(async () => {
    setLoading(true);
    try {
      setAccounts(await fetchXAccounts(machineFilter));
      setError("");
    } catch (err) {
      setError(String(err));
    } finally {
      setLoading(false);
    }
  }, [machineFilter]);

  const loadTargets = useCallback(async () => {
    setLoading(true);
    try {
      // 订阅目标不分机器，分发器为所有机器生成任务
      setTargets(await fetchFollowTargets());
      setError("");
    } catch (err) {
      setError(String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadAccounts();
  }, [loadAccounts]);
  useEffect(() => {
    if (tab === "targets") loadTargets();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab]);

  const resetAccountForm = () => {
    setEditingAccountId(null);
    setAccMachineId(machineFilter);
    setAccUsername("");
    setAccAuthToken("");
    setAccCt0("");
    setAccEnabled(true);
    setAccDailyLimit(50);
    setAccCooldown(10);
  };
  const openAccountCreate = () => {
    resetAccountForm();
    setShowAccountForm(true);
  };
  const openAccountEdit = (a: XAccount) => {
    setEditingAccountId(a.id);
    setAccMachineId(a.machine_id);
    setAccUsername(a.username);
    setAccAuthToken("");
    setAccCt0("");
    setAccEnabled(a.enabled);
    setAccDailyLimit(a.daily_like_limit);
    setAccCooldown(a.cooldown_minutes);
    setShowAccountForm(true);
  };
  const handleAccountSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    try {
      if (editingAccountId !== null) {
        const payload: Partial<{ machine_id: string; username: string; auth_token: string; ct0: string; enabled: boolean; daily_like_limit: number; cooldown_minutes: number }> = {
          machine_id: accMachineId,
          username: accUsername,
          enabled: accEnabled,
          daily_like_limit: accDailyLimit,
          cooldown_minutes: accCooldown,
        };
        if (accAuthToken) payload.auth_token = accAuthToken;
        if (accCt0) payload.ct0 = accCt0;
        await updateXAccount(editingAccountId, payload);
      } else {
        if (!accAuthToken || !accCt0) {
          setError("auth_token 和 ct0 必填");
          return;
        }
        await createXAccount({
          machine_id: accMachineId,
          username: accUsername,
          auth_token: accAuthToken,
          ct0: accCt0,
          enabled: accEnabled,
          daily_like_limit: accDailyLimit,
          cooldown_minutes: accCooldown,
        });
      }
      setShowAccountForm(false);
      await loadAccounts();
    } catch (err) {
      setError(String(err));
    } finally {
      setSubmitting(false);
    }
  };
  const handleAccountToggle = async (a: XAccount) => {
    try {
      await updateXAccount(a.id, { enabled: !a.enabled });
      await loadAccounts();
    } catch (err) {
      setError(String(err));
    }
  };
  const handleAccountDelete = async (a: XAccount) => {
    if (!confirm(`确认删除账号 ${a.username || "#" + a.id}？`)) return;
    try {
      await deleteXAccount(a.id);
      await loadAccounts();
    } catch (err) {
      setError(String(err));
    }
  };

  const resetTargetForm = () => {
    setEditingTargetId(null);
    setTgtHandle("");
    setTgtEnabled(true);
  };
  const openTargetCreate = () => {
    resetTargetForm();
    setShowTargetForm(true);
  };
  const openTargetEdit = (t: FollowTarget) => {
    setEditingTargetId(t.id);
    setTgtHandle(t.handle);
    setTgtEnabled(t.enabled);
    setShowTargetForm(true);
  };
  const handleTargetSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    try {
      if (editingTargetId !== null) {
        await updateFollowTarget(editingTargetId, {
          handle: tgtHandle,
          enabled: tgtEnabled,
        });
      } else {
        if (!tgtHandle.trim()) {
          setError("handle 必填");
          return;
        }
        await createFollowTarget({
          handle: tgtHandle,
          enabled: tgtEnabled,
        });
      }
      setShowTargetForm(false);
      await loadTargets();
    } catch (err) {
      setError(String(err));
    } finally {
      setSubmitting(false);
    }
  };
  const handleTargetToggle = async (t: FollowTarget) => {
    try {
      await updateFollowTarget(t.id, { enabled: !t.enabled });
      await loadTargets();
    } catch (err) {
      setError(String(err));
    }
  };
  const handleTargetDelete = async (t: FollowTarget) => {
    if (!confirm(`确认删除订阅目标 @${t.handle}？`)) return;
    try {
      await deleteFollowTarget(t.id);
      await loadTargets();
    } catch (err) {
      setError(String(err));
    }
  };
  const handleTargetDispatch = async (t: FollowTarget) => {
    if (!confirm(`立即分发 @${t.handle} 的新推文点赞任务到所有机器？`)) return;
    setBusyDispatch(t.id);
    setError("");
    try {
      await dispatchFollowTarget(t.id);
    } catch (err) {
      setError(String(err));
    } finally {
      setBusyDispatch(null);
    }
  };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase">
            X 点赞管理
          </h1>
          <p className="mt-1 text-sm text-(--text-muted)">
            点赞账号池与订阅目标管理（pyreplay worker）
          </p>
        </div>
        <button
          onClick={() => (tab === "accounts" ? loadAccounts() : loadTargets())}
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
      </header>

      {error && (
        <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">
          {error}
        </div>
      )}

      {/* tab switch */}
      <div className="flex gap-1 mb-4">
        <button
          onClick={() => setTab("accounts")}
          className={`border px-4 py-1.5 text-xs font-bold uppercase transition ${tab === "accounts" ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
        >
          X 账号管理
        </button>
        <button
          onClick={() => setTab("targets")}
          className={`border px-4 py-1.5 text-xs font-bold uppercase transition ${tab === "targets" ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
        >
          订阅目标管理
        </button>
        {tab === "accounts" && (
          <div className="ml-auto flex items-center gap-2">
            <label className="text-[10px] uppercase text-(--text-muted)">
              机器筛选
            </label>
            <input
              value={machineFilter}
              onChange={(e) => setMachineFilter(e.target.value.trim())}
              placeholder="留空=全部 (default)"
              className="w-44 bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1 text-xs focus:outline-none focus:border-white"
            />
          </div>
        )}
      </div>

      {/* ============ accounts ============ */}
      {tab === "accounts" && (
        <div className="border border-(--border-subtle)">
          <div className="p-4 border-b border-(--border-subtle) flex items-center justify-between">
            <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted)">
              点赞账号列表（{accounts.length}）
            </h2>
            <button
              onClick={openAccountCreate}
              className="flex items-center gap-1 border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
            >
              <Plus className="h-3 w-3" />
              新增账号
            </button>
          </div>

          {showAccountForm && (
            <form
              onSubmit={handleAccountSubmit}
              className="p-4 border-b border-(--border-subtle) bg-(--bg-surface) space-y-3"
            >
              <div className="flex flex-wrap gap-3 items-end">
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                    机器 ID
                  </label>
                  <input
                    value={accMachineId}
                    onChange={(e) => setAccMachineId(e.target.value)}
                    placeholder="bot-01，留空=default"
                    className="w-32 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                  />
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                    用户名
                  </label>
                  <input
                    value={accUsername}
                    onChange={(e) => setAccUsername(e.target.value)}
                    placeholder="acc1"
                    className="w-40 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                  />
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                    auth_token {editingAccountId !== null && "(留空=不变)"}
                  </label>
                  <input
                    value={accAuthToken}
                    onChange={(e) => setAccAuthToken(e.target.value)}
                    placeholder={editingAccountId !== null ? "留空保持不变" : "必填"}
                    className="w-72 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm font-mono focus:outline-none focus:border-white"
                  />
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                    ct0 {editingAccountId !== null && "(留空=不变)"}
                  </label>
                  <input
                    value={accCt0}
                    onChange={(e) => setAccCt0(e.target.value)}
                    placeholder={editingAccountId !== null ? "留空保持不变" : "必填"}
                    className="w-52 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm font-mono focus:outline-none focus:border-white"
                  />
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                    每日限额
                  </label>
                  <input
                    type="number"
                    min={0}
                    value={accDailyLimit}
                    onChange={(e) => setAccDailyLimit(Number(e.target.value))}
                    className="w-20 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                  />
                </div>
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                    冷却(分)
                  </label>
                  <input
                    type="number"
                    min={0}
                    value={accCooldown}
                    onChange={(e) => setAccCooldown(Number(e.target.value))}
                    className="w-20 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                  />
                </div>
                <label className="flex items-center gap-2 pb-1.5 text-xs text-(--text-secondary)">
                  <input
                    type="checkbox"
                    checked={accEnabled}
                    onChange={(e) => setAccEnabled(e.target.checked)}
                  />
                  启用
                </label>
              </div>
              <div className="flex gap-2">
                <button
                  type="submit"
                  disabled={submitting}
                  className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm hover:bg-(--text-primary) disabled:opacity-50 flex items-center gap-2"
                >
                  {submitting ? <Loader2 className="h-4 w-4 animate-spin" /> : <>{editingAccountId !== null ? "保存修改" : "创建账号"}</>}
                </button>
                <button
                  type="button"
                  onClick={() => setShowAccountForm(false)}
                  className="border border-(--border-subtle) px-4 py-1.5 text-sm text-(--text-secondary) hover:bg-(--bg-elevated)"
                >
                  取消
                </button>
              </div>
            </form>
          )}

          <table className="w-full text-left text-sm">
            <thead className="border-b border-(--border-subtle) text-(--text-muted)">
              <tr>
                <th className="px-4 py-3 font-bold uppercase w-12">ID</th>
                <th className="px-4 py-3 font-bold uppercase">机器</th>
                <th className="px-4 py-3 font-bold uppercase">用户名</th>
                <th className="px-4 py-3 font-bold uppercase">Auth Token</th>
                <th className="px-4 py-3 font-bold uppercase">ct0</th>
                <th className="px-4 py-3 font-bold uppercase">状态</th>
                <th className="px-4 py-3 font-bold uppercase">每日限额</th>
                <th className="px-4 py-3 font-bold uppercase">冷却(分)</th>
                <th className="px-4 py-3 font-bold uppercase">今日已用</th>
                <th className="px-4 py-3 font-bold uppercase">上次使用</th>
                <th className="px-4 py-3 text-right font-bold uppercase">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-(--border-divider)">
              {accounts.length === 0 ? (
                <tr>
                  <td colSpan={11} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
                    NO X ACCOUNTS
                  </td>
                </tr>
              ) : (
                accounts.map((a) => (
                  <tr key={a.id} className="hover:bg-(--bg-elevated) transition-colors">
                    <td className="px-4 py-3 font-mono text-(--text-secondary) text-xs">{a.id}</td>
                    <td className="px-4 py-3">
                      <span className="text-xs font-mono border border-(--border-subtle) px-1.5 py-0.5">{a.machine_id || "default"}</span>
                    </td>
                    <td className="px-4 py-3 text-(--text-primary) font-medium text-sm">{a.username || "-"}</td>
                    <td className="px-4 py-3 font-mono text-xs text-(--text-muted)">{a.auth_token || "-"}</td>
                    <td className="px-4 py-3 font-mono text-xs text-(--text-muted)">{a.ct0 || "-"}</td>
                    <td className="px-4 py-3">
                      <span className={`text-xs font-bold uppercase ${a.enabled ? "text-(--success)" : "text-(--text-muted)"}`}>
                        {a.enabled ? "启用" : "停用"}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-xs">{a.daily_like_limit}</td>
                    <td className="px-4 py-3 text-xs">{a.cooldown_minutes}</td>
                    <td className="px-4 py-3 text-xs">{a.used_today}</td>
                    <td className="px-4 py-3 text-(--text-muted) text-xs">{fmtTime(a.last_used_at)}</td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex items-center justify-end gap-1">
                        <button onClick={() => openAccountEdit(a)} title="编辑"
                          className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition">
                          <Edit3 className="h-3 w-3" />
                        </button>
                        <button onClick={() => handleAccountToggle(a)} title={a.enabled ? "停用" : "启用"}
                          className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition">
                          {a.enabled ? <PowerOff className="h-3 w-3" /> : <Power className="h-3 w-3" />}
                        </button>
                        <button onClick={() => handleAccountDelete(a)} title="删除"
                          className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-red-500 hover:border-red-500 transition">
                          <Trash2 className="h-3 w-3" />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      )}

      {/* ============ targets ============ */}
      {tab === "targets" && (
        <div className="border border-(--border-subtle)">
          <div className="p-4 border-b border-(--border-subtle) flex items-center justify-between">
            <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted)">
              订阅目标列表（{targets.length}）
            </h2>
            <button
              onClick={openTargetCreate}
              className="flex items-center gap-1 border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
            >
              <Plus className="h-3 w-3" />
              新增目标
            </button>
          </div>

          {showTargetForm && (
            <form
              onSubmit={handleTargetSubmit}
              className="p-4 border-b border-(--border-subtle) bg-(--bg-surface) space-y-3"
            >
              <div className="flex flex-wrap gap-3 items-end">
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1">
                    X 用户名（handle）
                  </label>
                  <input
                    value={tgtHandle}
                    onChange={(e) => setTgtHandle(e.target.value)}
                    placeholder="elonmusk"
                    className="w-56 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
                  />
                </div>
                <label className="flex items-center gap-2 pb-1.5 text-xs text-(--text-secondary)">
                  <input
                    type="checkbox"
                    checked={tgtEnabled}
                    onChange={(e) => setTgtEnabled(e.target.checked)}
                  />
                  启用
                </label>
              </div>
              <div className="flex gap-2">
                <button
                  type="submit"
                  disabled={submitting}
                  className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm hover:bg-(--text-primary) disabled:opacity-50 flex items-center gap-2"
                >
                  {submitting ? <Loader2 className="h-4 w-4 animate-spin" /> : <>{editingTargetId !== null ? "保存修改" : "创建目标"}</>}
                </button>
                <button
                  type="button"
                  onClick={() => setShowTargetForm(false)}
                  className="border border-(--border-subtle) px-4 py-1.5 text-sm text-(--text-secondary) hover:bg-(--bg-elevated)"
                >
                  取消
                </button>
              </div>
            </form>
          )}

          <table className="w-full text-left text-sm">
            <thead className="border-b border-(--border-subtle) text-(--text-muted)">
              <tr>
                <th className="px-4 py-3 font-bold uppercase w-12">ID</th>
                <th className="px-4 py-3 font-bold uppercase">Handle</th>
                <th className="px-4 py-3 font-bold uppercase">状态</th>
                <th className="px-4 py-3 font-bold uppercase">上次扫描</th>
                <th className="px-4 py-3 text-right font-bold uppercase">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-(--border-divider)">
              {targets.length === 0 ? (
                <tr>
                  <td colSpan={5} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
                    NO FOLLOW TARGETS
                  </td>
                </tr>
              ) : (
                targets.map((t) => (
                  <tr key={t.id} className="hover:bg-(--bg-elevated) transition-colors">
                    <td className="px-4 py-3 font-mono text-(--text-secondary) text-xs">{t.id}</td>
                    <td className="px-4 py-3">
                      <a
                        href={`https://x.com/${t.handle}`}
                        target="_blank"
                        rel="noreferrer"
                        className="text-(--accent) font-bold hover:underline"
                      >
                        @{t.handle}
                      </a>
                    </td>
                    <td className="px-4 py-3">
                      <span className={`text-xs font-bold uppercase ${t.enabled ? "text-(--success)" : "text-(--text-muted)"}`}>
                        {t.enabled ? "启用" : "停用"}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-(--text-muted) text-xs">{fmtTime(t.last_checked_at)}</td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex items-center justify-end gap-1">
                        <button onClick={() => handleTargetDispatch(t)} disabled={busyDispatch === t.id} title="立即分发：扫描新推文并为所有机器生成随机时间点赞任务"
                          className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-(--accent) hover:bg-(--bg-hover) transition disabled:opacity-40">
                          {busyDispatch === t.id ? <Loader2 className="h-3 w-3 animate-spin" /> : <Send className="h-3 w-3" />}
                        </button>
                        <button onClick={() => openTargetEdit(t)} title="编辑"
                          className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition">
                          <Edit3 className="h-3 w-3" />
                        </button>
                        <button onClick={() => handleTargetToggle(t)} title={t.enabled ? "停用" : "启用"}
                          className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition">
                          {t.enabled ? <PowerOff className="h-3 w-3" /> : <Power className="h-3 w-3" />}
                        </button>
                        <button onClick={() => handleTargetDelete(t)} title="删除"
                          className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-red-500 hover:border-red-500 transition">
                          <Trash2 className="h-3 w-3" />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>

          <div className="p-4 border-t border-(--border-subtle) text-xs text-(--text-muted)">
            <Heart className="h-3 w-3 inline mr-1" />
            订阅目标的推文由分发器（每 15 分钟 cron）扫描后，为每台机器生成随机时间的点赞任务，各机器后台随机账号执行（每台机器每个账号都会点赞，每日不限量）。
          </div>
        </div>
      )}
    </div>
  );
}
