import { useEffect, useState } from "react";
import { Loader2, Plus, RefreshCw, Trash2 } from "lucide-react";
import {
  createAuthUser,
  deleteAuthUser,
  listAuthRoles,
  listAuthUsers,
  type AuthRole,
  type AuthUser,
} from "../api";
import { useAuth } from "../hooks";
import { useAsync } from "../hooks";

function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("zh-CN", { hour12: false });
}

// 用户管理：查看 / 添加 / 删除管理员（统一认证 apps/auth :8008）。
// 仅 admin 角色可见可用（Sidebar 门控 + 本页双重校验）。
export function Users() {
  const { isConnected, user } = useAuth();
  const [q, setQ] = useState("");
  // 添加表单
  const [showForm, setShowForm] = useState(false);
  const [newWallet, setNewWallet] = useState("");
  const [newRoles, setNewRoles] = useState<string[]>(["admin"]);
  const [submitting, setSubmitting] = useState(false);

  const isAdmin = !!isConnected && user?.roles.includes("admin");

  // 未登录/非 admin 不拉取（页面早退，不渲染列表）
  const { data, loading, error, reload: load, setError } = useAsync<{ users: AuthUser[]; roles: AuthRole[] }>(
    async () => {
      const [u, r] = await Promise.all([listAuthUsers(q), listAuthRoles()]);
      return { users: u.items ?? [], roles: r.roles ?? [] };
    },
    [q],
    { immediate: false },
  );
  const users = data?.users ?? [];
  const roles = data?.roles ?? [];

  useEffect(() => {
    if (isAdmin) void load();
  }, [isAdmin, load]);

  if (!isConnected) {
    return (
      <div className="flex-1 p-4 overflow-y-auto">
        <div className="border border-(--border-subtle) p-8 text-center text-(--text-muted) font-mono text-sm">
          请先登录（需管理员权限）
        </div>
      </div>
    );
  }
  if (!isAdmin) {
    return (
      <div className="flex-1 p-4 overflow-y-auto">
        <div className="border border-(--border-subtle) p-8 text-center text-(--text-muted) font-mono text-sm">
          需要管理员（admin）权限
        </div>
      </div>
    );
  }

  const handleCreate = async () => {
    const wallet = newWallet.trim().toLowerCase();
    if (!/^0x[0-9a-f]{40}$/.test(wallet)) {
      setError("钱包地址格式错误（0x + 40 位 hex）");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await createAuthUser(wallet, newRoles);
      setNewWallet("");
      setNewRoles(["admin"]);
      setShowForm(false);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  };

  const handleDelete = async (u: AuthUser) => {
    const label = u.wallet_addr || u.email || u.id;
    if (!window.confirm(`删除用户 ${label}？将级联删除其角色/会话，不可恢复。`)) return;
    try {
      await deleteAuthUser(u.id);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const toggleRole = (id: string) => {
    setNewRoles((prev) => (prev.includes(id) ? prev.filter((r) => r !== id) : [...prev, id]));
  };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-8 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase">
            用户管理
          </h1>
          <p className="mt-1 text-sm text-(--text-muted)">
            统一认证（auth :8008）用户与管理员管理
          </p>
        </div>
        <button
          onClick={() => void load()}
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

      <div className="border border-(--border-subtle)">
        <div className="p-4 border-b border-(--border-subtle) flex items-center justify-between">
          <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted)">
            用户列表（{users.length}）
          </h2>
          <div className="flex items-center gap-2">
            <input
              value={q}
              onChange={(e) => setQ(e.target.value.trim())}
              placeholder="搜索钱包地址 / 邮箱"
              className="w-44 bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1 text-xs focus:outline-none focus:border-white"
            />
            <button
              onClick={() => setShowForm((v) => !v)}
              className="flex items-center gap-1 border border-(--border-subtle) px-3 py-1 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
            >
              <Plus className="h-3 w-3" />
              添加管理员
            </button>
          </div>
        </div>

        {showForm && (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              void handleCreate();
            }}
            className="p-4 border-b border-(--border-subtle) bg-(--bg-surface) space-y-3"
          >
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1">钱包地址（必填）</label>
              <input
                value={newWallet}
                onChange={(e) => setNewWallet(e.target.value)}
                placeholder="0xabc...def（40 位 hex）"
                className="w-96 bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm font-mono focus:outline-none focus:border-white"
              />
            </div>
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1">角色</label>
              <div className="flex flex-wrap gap-2">
                {roles.map((r) => (
                  <button
                    key={r.id}
                    type="button"
                    onClick={() => toggleRole(r.id)}
                    className={`border px-3 py-1 text-xs font-bold transition ${
                      newRoles.includes(r.id)
                        ? "border-white bg-(--accent) text-(--accent-foreground)"
                        : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"
                    }`}
                  >
                    {r.name}（{r.id}）
                  </button>
                ))}
              </div>
              <p className="mt-2 text-[10px] text-(--text-muted)">
                基础角色 user 自动授予；创建后该钱包首次登录即生效，无需重复建号。
              </p>
            </div>
            <div className="flex items-center gap-2">
              <button
                type="submit"
                disabled={submitting}
                className="flex items-center gap-1 border border-white bg-(--accent) px-4 py-1.5 text-xs font-bold text-(--accent-foreground) hover:opacity-90 transition disabled:opacity-50"
              >
                {submitting ? <Loader2 className="h-3 w-3 animate-spin" /> : <Plus className="h-3 w-3" />}
                确认添加
              </button>
              <button
                type="button"
                onClick={() => setShowForm(false)}
                className="border border-(--border-subtle) px-4 py-1.5 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition"
              >
                取消
              </button>
            </div>
          </form>
        )}

        {users.length === 0 && !loading ? (
          <div className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
            暂无用户
          </div>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-(--border-subtle) text-[10px] uppercase text-(--text-muted)">
                <th className="px-4 py-2 text-left">钱包地址</th>
                <th className="px-4 py-2 text-left">状态</th>
                <th className="px-4 py-2 text-left">角色</th>
                <th className="px-4 py-2 text-left">创建时间</th>
                <th className="px-4 py-2 text-right">操作</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id} className="border-b border-(--border-divider) hover:bg-(--bg-elevated)">
                  <td className="px-4 py-2 font-mono text-xs text-(--text-primary)">{u.wallet_addr || u.email || "-"}</td>
                  <td className="px-4 py-2 text-xs">
                    <span className={u.status === "active" ? "text-green-400" : "text-red-400"}>
                      {u.status === "active" ? "正常" : "禁用"}
                    </span>
                  </td>
                  <td className="px-4 py-2">
                    <div className="flex flex-wrap gap-1">
                      {(u.roles ?? []).map((r) => (
                        <span
                          key={r}
                          className={`px-2 py-0.5 text-[10px] font-bold uppercase border ${
                            r === "admin"
                              ? "border-white bg-(--accent) text-(--accent-foreground)"
                              : "border-(--border-subtle) text-(--text-secondary)"
                          }`}
                        >
                          {r}
                        </span>
                      ))}
                    </div>
                  </td>
                  <td className="px-4 py-2 font-mono text-[10px] text-(--text-muted)">{fmtTime(u.created_at)}</td>
                  <td className="px-4 py-2 text-right">
                    <button
                      onClick={() => void handleDelete(u)}
                      disabled={u.id === user?.id}
                      title={u.id === user?.id ? "不能删除自己" : "删除用户"}
                      className="text-(--text-muted) hover:text-red-400 transition disabled:opacity-30 cursor-pointer disabled:cursor-not-allowed"
                    >
                      <Trash2 className="h-4 w-4" />
                    </button>
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
